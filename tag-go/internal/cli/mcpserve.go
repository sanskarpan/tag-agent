package cli

// registerMCPServe wires `tag mcp-serve`, which turns TAG into an MCP server:
// it exposes a handful of built-in tools over a stdio JSON-RPC transport so
// external MCP clients (editors, agents) can call them. Handlers are
// implemented inline here to avoid an import cycle with internal/tool and
// internal/agent; all package-level identifiers are mcps-prefixed.

import (
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/mcp"
)

func registerMCPServe(root *cobra.Command, app *App) {
	cmd := &cobra.Command{
		Use:     "mcp-serve",
		Short:   "Expose TAG's built-in tools as an MCP server over stdio",
		GroupID: "tools",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			srv := mcps_buildServer(app)
			return srv.Serve(os.Stdin, os.Stdout)
		},
	}
	root.AddCommand(cmd)
}

// mcps_buildServer constructs the server and registers the built-in tools.
func mcps_buildServer(app *App) *mcp.Server {
	srv := mcp.NewServer("tag")

	srv.Register("echo", "Echo back the provided text",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "text to echo"},
			},
			"required": []any{"text"},
		},
		func(args map[string]any) (string, error) {
			return mcps_argStr(args, "text"), nil
		})

	srv.Register("now", "Return the current UTC time (RFC3339)",
		map[string]any{"type": "object"},
		func(args map[string]any) (string, error) {
			return time.Now().UTC().Format(time.RFC3339), nil
		})

	srv.Register("tag_profiles", "List configured TAG profile names",
		map[string]any{"type": "object"},
		func(args map[string]any) (string, error) {
			return mcps_profileNames(app), nil
		})

	return srv
}

// mcps_argStr pulls a string argument, tolerating a missing/typed value.
func mcps_argStr(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}

// mcps_profileNames returns the comma-joined, sorted profile names from config.
func mcps_profileNames(app *App) string {
	if app == nil || app.Cfg == nil {
		return ""
	}
	profiles := app.Cfg.Profiles()
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
