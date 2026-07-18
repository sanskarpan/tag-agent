package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/solver"
)

// registerAgenticCI wires `tag agentic-ci <task>` — the CI-automation agentic
// solver (parity roadmap #527). With --check it runs a real check→fix→re-check
// loop: run the check command; on failure feed the output to the agent loop for
// a fix, then re-check, up to --max-iters, reporting converged/failed. Without
// --check it drives the loop over the task text for --max-iters passes. Defaults
// to the offline `echo` provider.
func registerAgenticCI(root *cobra.Command, app *App) {
	var provider, check, repo string
	var maxIters int
	c := &cobra.Command{
		Use:     "agentic-ci <task>",
		Short:   "Run a CI check→fix loop (or the agent loop over a task)",
		GroupID: "orch",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			db, _ := app.OpenDB()
			model := app.Cfg.String("profiles."+app.profile("")+".config.model.default", "")
			res, err := solver.Solve(context.Background(), db, prov, model, solver.Options{
				Kind:     solver.KindCI,
				Task:     args[0],
				MaxIters: maxIters,
				CheckCmd: check,
				RepoPath: repo,
			})
			if err != nil {
				return err
			}
			return emitSolveResult(res)
		},
	}
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().IntVar(&maxIters, "max-iters", 1, "max check→fix iterations")
	c.Flags().StringVar(&check, "check", "", "check command (build/test); enables the real check→fix loop")
	c.Flags().StringVar(&repo, "repo", "", "working directory for the check command")
	root.AddCommand(c)
}
