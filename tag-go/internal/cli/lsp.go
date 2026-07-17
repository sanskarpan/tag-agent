package cli

// registerLSP wires `tag lsp`, a minimal Language Server Protocol server that
// TAG-aware editors can drive over stdio. It handles the LSP lifecycle plus
// textDocument/hover, surfacing configured profile names. All package-level
// identifiers here are lsp-prefixed to avoid collisions with other cli files.
//
// Mirrors the Python `lsp start` / `lsp status` subcommands: `start` runs the
// server, `status` lists active sessions. The offline Go build does not persist
// LSP session records, so `status` reports none.

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/lsp"
)

func registerLSP(root *cobra.Command, app *App) {
	cmd := &cobra.Command{
		Use:     "lsp",
		Short:   "TAG IDE Bridge / LSP server",
		GroupID: "tools",
		Args:    cobra.NoArgs,
		// Bare `tag lsp` shows session status (matching Python's default).
		RunE: func(cmd *cobra.Command, args []string) error {
			return lspStatus()
		},
	}

	start := &cobra.Command{
		Use:   "start",
		Short: "Start the LSP server (stdio)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "TAG LSP server starting on stdio ...")
			srv := lsp.NewServer(lspProfileNames(app))
			return srv.Serve(os.Stdin, os.Stdout)
		},
	}
	// Accepted for CLI parity with Python; this build serves stdio only.
	start.Flags().Bool("stdio", false, "serve over stdio (default)")
	start.Flags().Int("port", 7878, "TCP port (stdio-only in this build)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show running LSP sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return lspStatus()
		},
	}

	cmd.AddCommand(start, status)
	root.AddCommand(cmd)
}

// lspStatus reports active LSP sessions. The Go build does not persist session
// records, so there are never any active sessions to show.
func lspStatus() error {
	if flagJSON {
		fmt.Println("[]")
		return nil
	}
	fmt.Println("No active LSP sessions.")
	return nil
}

// lspProfileNames returns the sorted TAG profile names from config, or nil.
func lspProfileNames(app *App) []string {
	if app == nil || app.Cfg == nil {
		return nil
	}
	profiles := app.Cfg.Profiles()
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
