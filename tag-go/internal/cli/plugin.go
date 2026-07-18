package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/paths"
)

// pluginEnsureTable self-ensures the plugins_installed table that records which
// registry plugins have been installed (natively) for which profile. Mirrors the
// self-ensuring pattern used by marketplace_profiles (see marketplace.go).
const pluginEnsureTable = `CREATE TABLE IF NOT EXISTS plugins_installed(
  profile TEXT, name TEXT, pypi TEXT, installed_at TEXT,
  PRIMARY KEY(profile, name))`

// registerPlugin wires TAG plugin management: plugin list/install/enable/disable.
// Port of src/tag/cmd/routing.py:cmd_plugin (bundled plugin-registry.yaml).
// enable/disable toggle TAG_PLUGIN_<NAME>_ENABLED in the profile .env.
//
// `install` design decision (Go-native, no pip):
// The Python runtime pip-installs a plugin's PyPI package into a per-profile
// managed venv. TAG-Go has no Python venv — its tools come via MCP servers
// (internal/mcp + the curated mcp-registry). So `plugin install` here does NOT
// shell out to pip. Instead it resolves the plugin from the curated registry and
// RECORDS it as installed (a plugins_installed row) + ENABLED (the existing
// TAG_PLUGIN_<NAME>_ENABLED env mechanism) for the profile. This is the honest
// native equivalent of "make this plugin active for the profile".
//
// Honesty guard: a plugin may declare requires_env secrets (e.g. SERP_API_KEY)
// that genuinely can't be represented natively. If any are missing from the
// environment we do NOT fake success — we return an error naming exactly which
// vars are needed and record nothing.
func registerPlugin(root *cobra.Command, app *App) {
	p := &cobra.Command{Use: "plugin", Aliases: []string{"plugins"}, Short: "Manage TAG plugins", GroupID: "tools"}

	var listProfile string
	list := &cobra.Command{Use: "list", Short: "List available plugins (marks installed)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := config.PluginRegistry()
			if err != nil {
				return err
			}
			if len(reg) == 0 {
				fmt.Println("No plugins in registry.")
				return nil
			}
			profile := strOr(listProfile, app.Cfg.MasterProfile())
			installed, err := installedPlugins(app, profile)
			if err != nil {
				return err
			}
			type row struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				PyPI        string `json:"pypi"`
				Installed   bool   `json:"installed"`
			}
			var rows []row
			for _, name := range sortedKeys(reg) {
				info := asMap(reg[name])
				rows = append(rows, row{name, str(info["description"]), str(info["pypi"]), installed[name]})
			}
			if flagJSON {
				return emitJSON(rows)
			}
			for _, r := range rows {
				mark := " "
				if r.Installed {
					mark = "*"
				}
				fmt.Printf("%s %-35s %s\n", mark, r.Name, r.Description)
			}
			return nil
		}}
	list.Flags().StringVar(&listProfile, "profile", "", "profile to check installed state for (default master)")

	var enProfile string
	enable := &cobra.Command{Use: "enable <name>", Short: "Enable a plugin for a profile", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := config.PluginRegistry()
			if err != nil {
				return err
			}
			if _, ok := reg[args[0]]; !ok {
				return fmt.Errorf("unknown plugin: %s", args[0])
			}
			return setPluginEnabled(app, strOr(enProfile, app.Cfg.MasterProfile()), args[0], true)
		}}
	enable.Flags().StringVar(&enProfile, "profile", "", "profile (default master)")

	var disProfile string
	disable := &cobra.Command{Use: "disable <name>", Short: "Disable a plugin for a profile", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setPluginEnabled(app, strOr(disProfile, app.Cfg.MasterProfile()), args[0], false)
		}}
	disable.Flags().StringVar(&disProfile, "profile", "", "profile (default master)")

	var instProfile string
	install := &cobra.Command{Use: "install <name>", Short: "Install (record + enable) a plugin for a profile", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			reg, err := config.PluginRegistry()
			if err != nil {
				return err
			}
			info, ok := reg[name]
			if !ok {
				return fmt.Errorf("unknown plugin: %s", name)
			}
			im := asMap(info)
			pypi := strOr(str(im["pypi"]), name)
			profile := strOr(instProfile, app.Cfg.MasterProfile())
			if err := validProfileName(profile); err != nil {
				return err
			}

			// Honesty guard: a plugin may declare requires_env secrets that we
			// genuinely can't represent natively. If any are missing, refuse and
			// name exactly what's needed — do NOT record a fake install. A secret
			// counts as present if it's exported in this process OR persisted in
			// the profile .env — the same source the runtime loads secrets from
			// (and where enable/disable write), so the guard matches runtime reality.
			penv := profileEnvKeys(app, profile)
			var missing []string
			for _, e := range asSlice(im["requires_env"]) {
				key := str(e)
				if key == "" {
					continue
				}
				if _, present := os.LookupEnv(key); present {
					continue
				}
				if penv[key] {
					continue
				}
				missing = append(missing, key)
			}
			if len(missing) > 0 {
				return fmt.Errorf("plugin %q requires environment variable(s) not set: %s; set them and re-run (not installed)",
					name, strings.Join(missing, ", "))
			}

			// Native install: record the plugin as installed for the profile.
			// TAG-Go has no pip/venv — tools come via MCP — so "installed" means
			// "recorded + enabled for this profile", not a pip package on disk.
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if _, err := db.Exec(pluginEnsureTable); err != nil {
				return err
			}

			// Enable it via the existing TAG_PLUGIN_<NAME>_ENABLED env mechanism
			// BEFORE recording, so a failure here records nothing (no
			// recorded-but-not-enabled state). setPluginEnabled prints
			// "Enabled plugin ...".
			if err := setPluginEnabled(app, profile, name, true); err != nil {
				return err
			}

			now := time.Now().UTC().Format(time.RFC3339)
			if _, err := db.Exec(`INSERT INTO plugins_installed(profile, name, pypi, installed_at)
				VALUES(?,?,?,?)
				ON CONFLICT(profile, name) DO UPDATE SET
				  pypi=excluded.pypi, installed_at=excluded.installed_at`,
				profile, name, pypi, now); err != nil {
				return err
			}

			// Only claim success once both the enable and the record landed.
			fmt.Printf("Installed plugin '%s' (%s) for profile '%s'\n", name, pypi, profile)
			return nil
		}}
	install.Flags().StringVar(&instProfile, "profile", "", "profile (default master)")

	p.AddCommand(list, enable, disable, install)
	root.AddCommand(p)
}

// installedPlugins returns the set of plugin names recorded as installed for the
// given profile (empty map on a fresh DB). Reads the self-ensured table.
func installedPlugins(app *App, profile string) (map[string]bool, error) {
	out := map[string]bool{}
	db, err := app.OpenDB()
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(pluginEnsureTable); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT name FROM plugins_installed WHERE profile=?`, profile)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// profileEnvKeys returns the set of keys present in the profile's .env — the
// same file the runtime loads secrets from (and where enable/disable persist
// state). Returns an empty set if the file is absent/unreadable. Used by the
// install honesty guard so it checks the source the runtime actually consumes.
func profileEnvKeys(app *App, profile string) map[string]bool {
	keys := map[string]bool{}
	homeDir := app.Cfg.String("runtime.home_dir", "")
	envFile := filepath.Join(paths.ProfileHome(homeDir, profile), ".env")
	b, err := os.ReadFile(envFile)
	if err != nil {
		return keys
	}
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") || !strings.Contains(ln, "=") {
			continue
		}
		if strings.HasPrefix(ln, "export ") || strings.HasPrefix(ln, "export\t") {
			ln = strings.TrimLeft(ln[len("export"):], " \t")
		}
		k, _, _ := strings.Cut(ln, "=")
		if k = strings.TrimSpace(k); k != "" {
			keys[k] = true
		}
	}
	return keys
}

var pluginEnvSanitize = regexp.MustCompile(`[^A-Z0-9]`)

// setPluginEnabled toggles TAG_PLUGIN_<NAME>_ENABLED in the profile .env,
// replacing any existing line for that key (port of the enable/disable logic).
func setPluginEnabled(app *App, profile, name string, enabled bool) error {
	if err := validProfileName(profile); err != nil {
		return err
	}
	suffix := pluginEnvSanitize.ReplaceAllString(strings.ToUpper(name), "_")
	key := "TAG_PLUGIN_" + suffix + "_ENABLED"
	homeDir := app.Cfg.String("runtime.home_dir", "")
	dir := paths.ProfileHome(homeDir, profile)
	envFile := filepath.Join(dir, ".env")

	var lines []string
	if b, err := os.ReadFile(envFile); err == nil {
		for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if ln == "" || strings.HasPrefix(ln, key+"=") {
				continue // drop the old value for this key (and blanks)
			}
			lines = append(lines, ln)
		}
	}
	if enabled {
		lines = append(lines, key+"=true")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		return err
	}
	state := "Disabled"
	if enabled {
		state = "Enabled"
	}
	fmt.Printf("%s plugin '%s' for profile '%s'\n", state, name, profile)
	return nil
}
