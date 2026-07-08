package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/paths"
)

// registerPlugin wires TAG plugin management: plugin list/enable/disable.
// Port of src/tag/cmd/routing.py:cmd_plugin (bundled plugin-registry.yaml).
// enable/disable toggle TAG_PLUGIN_<NAME>_ENABLED in the profile .env. The
// `install` subcommand pip-installs into the profile venv (Track-B runtime).
func registerPlugin(root *cobra.Command, app *App) {
	p := &cobra.Command{Use: "plugin", Aliases: []string{"plugins"}, Short: "Manage TAG plugins", GroupID: "tools"}

	list := &cobra.Command{Use: "list", Short: "List available plugins", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := config.PluginRegistry()
			if err != nil {
				return err
			}
			if len(reg) == 0 {
				fmt.Println("No plugins in registry.")
				return nil
			}
			type row struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				PyPI        string `json:"pypi"`
			}
			var rows []row
			for _, name := range sortedKeys(reg) {
				info := asMap(reg[name])
				rows = append(rows, row{name, str(info["description"]), str(info["pypi"])})
			}
			if flagJSON {
				return emitJSON(rows)
			}
			for _, r := range rows {
				fmt.Printf("  %-35s %s\n", r.Name, r.Description)
			}
			return nil
		}}

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
	install := &cobra.Command{Use: "install <name>", Short: "Install a plugin into a profile's venv", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := config.PluginRegistry()
			if err != nil {
				return err
			}
			info, ok := reg[args[0]]
			if !ok {
				return fmt.Errorf("Unknown plugin: %s", args[0])
			}
			pypi := strOr(str(asMap(info)["pypi"]), args[0])
			profile := strOr(instProfile, app.Cfg.MasterProfile())
			if err := validProfileName(profile); err != nil {
				return err
			}
			// Installing pip-installs the plugin's PyPI package into the profile's
			// managed (Hermes) venv. That runtime is not part of the offline Go
			// build, so we can't perform the install — report honestly rather than
			// claim success.
			return fmt.Errorf("plugin install requires the managed runtime venv (would pip install %q into profile %q); not available in this Go build", pypi, profile)
		}}
	install.Flags().StringVar(&instProfile, "profile", "", "profile (default master)")

	p.AddCommand(list, enable, disable, install)
	root.AddCommand(p)
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
