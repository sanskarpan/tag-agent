package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/solver"
)

// registerAgenticCI wires `tag agentic-ci <task>` — the CI-automation agentic
// solver (parity roadmap #527). It drives the native agent loop over a CI task
// for up to --max-iters passes, records the run, and prints a structured
// result. Defaults to the offline `echo` provider.
func registerAgenticCI(root *cobra.Command, app *App) {
	var provider string
	var maxIters int
	c := &cobra.Command{
		Use:     "agentic-ci <task>",
		Short:   "Run the CI-automation agent loop over a task",
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
			})
			if err != nil {
				return err
			}
			return emitSolveResult(res)
		},
	}
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().IntVar(&maxIters, "max-iters", 1, "number of agent-loop passes")
	root.AddCommand(c)
}
