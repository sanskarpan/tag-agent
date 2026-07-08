package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"

	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/paths"
)

// registerMCPRegistry wires the curated MCP server catalog:
// mcp-registry list/install/enable/disable. Port of
// src/tag/cmd/workflow_mgmt.py:cmd_mcp_registry (bundled mcp-registry.yaml).
func registerMCPRegistry(root *cobra.Command, app *App) {
	m := &cobra.Command{Use: "mcp-registry", Short: "Browse and install curated MCP servers", GroupID: "tools"}

	var category string
	list := &cobra.Command{Use: "list", Short: "List available MCP servers", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			servers, err := config.MCPRegistry()
			if err != nil {
				return err
			}
			type row struct {
				Name        string   `json:"name"`
				Description string   `json:"description"`
				Category    string   `json:"category"`
				RequiresEnv []string `json:"requires_env"`
			}
			var rows []row
			for _, name := range sortedKeys(servers) {
				info := asMap(servers[name])
				if category != "" && str(info["category"]) != category {
					continue
				}
				var env []string
				for _, e := range asSlice(info["requires_env"]) {
					env = append(env, str(e))
				}
				rows = append(rows, row{name, str(info["description"]), str(info["category"]), env})
			}
			if flagJSON {
				return emitJSON(rows)
			}
			fmt.Printf("%-30s %-14s %s\n", "Name", "Category", "Description")
			fmt.Println(strings.Repeat("-", 80))
			for _, r := range rows {
				envNote := ""
				if len(r.RequiresEnv) > 0 {
					envNote = fmt.Sprintf(" [needs: %s]", strings.Join(r.RequiresEnv, ", "))
				}
				fmt.Printf("  %-28s %-14s %s%s\n", r.Name, r.Category, r.Description, envNote)
			}
			return nil
		}}
	list.Flags().StringVar(&category, "category", "", "filter by category")

	install := &cobra.Command{Use: "install <name>", Short: "Install an MCP server globally (npm/pip)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			servers, err := config.MCPRegistry()
			if err != nil {
				return err
			}
			info, ok := servers[args[0]]
			if !ok {
				return fmt.Errorf("unknown MCP server: %s", args[0])
			}
			inst := asMap(asMap(info)["install"])
			pkg := strOr(str(inst["package"]), args[0])
			itype := strOr(str(inst["type"]), "npm")
			var c *exec.Cmd
			switch itype {
			case "npm":
				c = exec.Command("npm", "install", "-g", pkg)
			case "pip":
				c = exec.Command("pip", "install", pkg)
			default:
				return fmt.Errorf("unknown install type: %s", itype)
			}
			out, err := c.CombinedOutput()
			if err != nil {
				return fmt.Errorf("install failed: %s", strings.TrimSpace(string(out)))
			}
			fmt.Printf("Installed MCP server '%s' (%s)\n", args[0], pkg)
			return nil
		}}

	var enProfile string
	enable := &cobra.Command{Use: "enable <name>", Short: "Enable an MCP server for a profile", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			servers, err := config.MCPRegistry()
			if err != nil {
				return err
			}
			info, ok := servers[args[0]]
			if !ok {
				return fmt.Errorf("unknown MCP server: %s", args[0])
			}
			profile := strOr(enProfile, app.Cfg.MasterProfile())
			pcfgPath, pcfg, err := loadProfileConfig(app, profile)
			if err != nil {
				return err
			}
			mcpServers, _ := pcfg["mcp_servers"].(map[string]any)
			if mcpServers == nil {
				mcpServers = map[string]any{}
			}
			if _, exists := mcpServers[args[0]]; exists {
				fmt.Printf("MCP server '%s' is already enabled for profile '%s'\n", args[0], profile)
				return nil
			}
			mcpServers[args[0]] = asMap(info)["config"]
			pcfg["mcp_servers"] = mcpServers
			if err := writeProfileConfig(pcfgPath, pcfg); err != nil {
				return err
			}
			fmt.Printf("Enabled MCP server '%s' for profile '%s'\n", args[0], profile)
			return nil
		}}
	enable.Flags().StringVar(&enProfile, "profile", "", "profile (default master)")

	var disProfile string
	disable := &cobra.Command{Use: "disable <name>", Short: "Disable an MCP server for a profile", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := strOr(disProfile, app.Cfg.MasterProfile())
			pcfgPath, pcfg, err := loadProfileConfig(app, profile)
			if err != nil {
				return err
			}
			if mcpServers, ok := pcfg["mcp_servers"].(map[string]any); ok {
				delete(mcpServers, args[0])
				pcfg["mcp_servers"] = mcpServers
			}
			if err := writeProfileConfig(pcfgPath, pcfg); err != nil {
				return err
			}
			fmt.Printf("Disabled MCP server '%s' for profile '%s'\n", args[0], profile)
			return nil
		}}
	disable.Flags().StringVar(&disProfile, "profile", "", "profile (default master)")

	m.AddCommand(list, install, enable, disable)
	root.AddCommand(m)
}

// loadProfileConfig reads (or initializes) the runtime profile config.yaml for a
// profile, returning its path and parsed contents.
func loadProfileConfig(app *App, profile string) (string, map[string]any, error) {
	if err := validProfileName(profile); err != nil {
		return "", nil, err
	}
	homeDir := app.Cfg.String("runtime.home_dir", "")
	dir := paths.ProfileHome(homeDir, profile)
	path := filepath.Join(dir, "config.yaml")
	pcfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &pcfg); err != nil {
			return "", nil, err
		}
		if pcfg == nil {
			pcfg = map[string]any{}
		}
	}
	return path, pcfg, nil
}

// writeProfileConfig writes a profile config.yaml, creating parent dirs.
func writeProfileConfig(path string, pcfg map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(pcfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
