package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/mcp"
)

// registerMCPConnect wires `tag mcp-connect <command> [args...]` — spawn an
// external MCP server subprocess, initialize, and list (or call) its tools.
// Completes the MCP story: TAG both serves (mcp-serve) and consumes external
// servers over stdio.
func registerMCPConnect(root *cobra.Command, app *App) {
	var call string
	// mcp.Client defaults to 120s, which suits a long-lived agent session but
	// not an interactive command: an unresponsive server blocked the CLI for two
	// minutes with no progress output and no way to shorten the wait.
	timeout := 30 * time.Second
	c := &cobra.Command{
		Use:     "mcp-connect <command> [args...]",
		Short:   "Connect to an external MCP server subprocess and list its tools",
		GroupID: "tools",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			pc, err := mcp.NewProcessClient(ctx, args[0], args[1:]...)
			if err != nil {
				return err
			}
			defer pc.Close()
			if timeout > 0 {
				pc.Timeout = timeout
			}
			if err := pc.Initialize("tag"); err != nil {
				return fmt.Errorf("MCP initialize failed: %w", err)
			}
			tools, err := pc.ListTools()
			if err != nil {
				return err
			}
			if call != "" {
				res, err := pc.CallTool(call, nil)
				if err != nil {
					return err
				}
				fmt.Println(res.Text())
				return nil
			}
			if flagJSON {
				return emitJSON(tools)
			}
			fmt.Printf("Connected — %d tool(s):\n", len(tools))
			for _, t := range tools {
				fmt.Printf("  %-24s %s\n", t.Name, t.Description)
			}
			return nil
		},
	}
	c.Flags().StringVar(&call, "call", "", "call this tool (no args) instead of listing")
	c.Flags().DurationVar(&timeout, "timeout", timeout, "per-request timeout waiting for the server (0 = wait forever)")
	root.AddCommand(c)
}
