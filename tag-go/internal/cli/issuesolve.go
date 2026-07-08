package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/solver"
)

// registerIssueSolve wires `tag issue-solve <issue>` — the issue-solving agentic
// solver (parity roadmap #527). The issue body is supplied inline or, with
// --file, read from a path. Fetching a live issue from GitHub/Linear needs
// network + a token and is reported honestly rather than faked. Defaults to the
// offline `echo` provider.
func registerIssueSolve(root *cobra.Command, app *App) {
	var provider, file string
	c := &cobra.Command{
		Use:     "issue-solve <issue>",
		Short:   "Solve an issue (inline body, --file, or a bare reference) with the agent loop",
		GroupID: "orch",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			var task string
			switch {
			case file != "":
				b, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("reading issue file: %w", err)
				}
				task = string(b)
			case len(args) == 1:
				task = args[0]
			default:
				return fmt.Errorf("provide an issue body/reference as an argument or via --file")
			}
			if strings.TrimSpace(task) == "" {
				return fmt.Errorf("issue text is empty")
			}
			db, _ := app.OpenDB()
			model := app.Cfg.String("profiles."+app.profile("")+".config.model.default", "")
			res, err := solver.Solve(context.Background(), db, prov, model, solver.Options{
				Kind: solver.KindIssue,
				Task: task,
			})
			if err != nil {
				return err
			}
			return emitSolveResult(res)
		},
	}
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().StringVar(&file, "file", "", "read the issue body from a file")
	root.AddCommand(c)
}
