package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"

	"github.com/tag-agent/tag/internal/marketplace"
	"github.com/tag-agent/tag/internal/paths"
)

// registerTemplate wires profile templates: template export/import.
// Port of src/tag/cmd/workflow_mgmt.py:cmd_template. Secrets are redacted on
// export and written 0600 on import; profile names are validated against path
// traversal. The network `fetch` subcommand reuses the marketplace SSRF guard.
var (
	redactRe      = regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|credential|auth|url)`)
	profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func registerTemplate(root *cobra.Command, app *App) {
	t := &cobra.Command{Use: "template", Short: "Export/import profile templates", GroupID: "tools"}

	var exProfile, output string
	export := &cobra.Command{Use: "export", Short: "Export a profile as a YAML template (secrets redacted)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := strOr(exProfile, app.Cfg.MasterProfile())
			if err := validProfileName(profile); err != nil {
				return err
			}
			homeDir := app.Cfg.String("runtime.home_dir", "")
			dir := paths.ProfileHome(homeDir, profile)

			tmpl := map[string]any{
				"name":        profile,
				"version":     "1",
				"description": fmt.Sprintf("TAG profile template for '%s'", profile),
				"env":         map[string]any{},
				"config":      map[string]any{},
			}
			// read .env (redacting secret-like keys)
			env := map[string]any{}
			if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
				for _, line := range strings.Split(string(b), "\n") {
					line = strings.TrimSpace(line)
					if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
						continue
					}
					k, v, _ := strings.Cut(line, "=")
					k, v = strings.TrimSpace(k), strings.TrimSpace(v)
					env[k] = redactEnv(k, v)
				}
			}
			tmpl["env"] = env
			// read config.yaml
			if b, err := os.ReadFile(filepath.Join(dir, "config.yaml")); err == nil {
				var pcfg map[string]any
				if yaml.Unmarshal(b, &pcfg) == nil && pcfg != nil {
					tmpl["config"] = pcfg
				}
			}
			out, err := yaml.Marshal(tmpl)
			if err != nil {
				return err
			}
			if output != "" {
				if err := os.WriteFile(output, out, 0o644); err != nil {
					return err
				}
				fmt.Printf("Template exported to %s\n", output)
				return nil
			}
			fmt.Print(string(out))
			return nil
		}}
	export.Flags().StringVar(&exProfile, "profile", "", "profile to export (default master)")
	export.Flags().StringVar(&output, "output", "", "write template to file")

	var imProfile string
	imp := &cobra.Command{Use: "import <template-file>", Short: "Create a profile from a YAML template", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var tmpl map[string]any
			if err := yaml.Unmarshal(b, &tmpl); err != nil || tmpl == nil {
				return fmt.Errorf("template file %q does not contain a valid YAML mapping", args[0])
			}
			profile := strings.TrimSpace(strOr(imProfile, strOr(str(tmpl["name"]), "imported")))
			if !profileNameRe.MatchString(profile) {
				return fmt.Errorf("invalid profile name: %q (use letters, digits, dot, dash, underscore; no path separators)", profile)
			}
			homeDir := app.Cfg.String("runtime.home_dir", "")
			dir := paths.ProfileHome(homeDir, profile)
			if _, err := os.Stat(dir); err == nil {
				return fmt.Errorf("profile '%s' already exists; choose a different --profile name", profile)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			// .env — placeholder values (<...>) become commented; write 0600.
			if envData := asMap(tmpl["env"]); len(envData) > 0 {
				var lines []string
				for _, k := range sortedKeys(envData) {
					v := fmt.Sprint(envData[k])
					if strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">") {
						lines = append(lines, fmt.Sprintf("# %s=<fill in>", k))
					} else {
						lines = append(lines, fmt.Sprintf("%s=%s", k, v))
					}
				}
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
					return err
				}
			}
			// config.yaml
			if cfgData := asMap(tmpl["config"]); len(cfgData) > 0 {
				out, _ := yaml.Marshal(cfgData)
				if err := os.WriteFile(filepath.Join(dir, "config.yaml"), out, 0o644); err != nil {
					return err
				}
			}
			fmt.Printf("Template imported as profile '%s'\n", profile)
			return nil
		}}
	imp.Flags().StringVar(&imProfile, "profile", "", "target profile name (default template name)")

	fetch := &cobra.Command{Use: "fetch URL", Short: "Fetch a template from a URL", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Reuse the marketplace SSRF guard: reject non-public hosts, file://,
			// and re-validate every redirect hop / dialed IP.
			if err := marketplace.ValidateFetchURL(args[0]); err != nil {
				return fmt.Errorf("Refused to fetch template: %v", err)
			}
			body, err := marketplace.Fetch(args[0], 15*time.Second)
			if err != nil {
				return fmt.Errorf("Failed to fetch template: %v", err)
			}
			fmt.Print(string(body))
			return nil
		}}

	t.AddCommand(export, imp, fetch)
	root.AddCommand(t)
}

// redactEnv masks secret-like env values (port of _redact_env).
func redactEnv(key, val string) string {
	if redactRe.MatchString(key) {
		return "<" + strings.ToUpper(key) + ">"
	}
	return val
}
