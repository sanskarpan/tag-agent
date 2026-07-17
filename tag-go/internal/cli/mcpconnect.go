package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/mcp"
)

// registerMCPConnect wires `tag mcp-connect <command> [args...]` — spawn an
// external MCP server subprocess, initialize, and list (or call) its tools.
// Completes the MCP story: TAG both serves (mcp-serve) and consumes external
// servers over stdio.
func registerMCPConnect(root *cobra.Command, app *App) {
	var call string
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
	root.AddCommand(c)
}
