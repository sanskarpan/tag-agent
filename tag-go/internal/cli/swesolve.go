package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/solver"
)

// registerSWESolve wires `tag swe-solve <task>` — the SWE agentic solver
// (parity roadmap #527). It gathers repo context (optional --repo), drives the
// native agent loop, records the run, and prints a structured result. With
// --tools (and a real --provider) the agent reads/edits files confined to --repo;
// --run-tests runs a command afterwards and reports pass/fail. Defaults to the
// offline `echo` provider so it is safe without keys.
func registerSWESolve(root *cobra.Command, app *App) {
	var provider, repo, runTests string
	var maxSteps int
	var withTools, allowBash bool
	c := &cobra.Command{
		Use:     "swe-solve <task>",
		Short:   "Solve a software-engineering task with the agent loop",
		GroupID: "orch",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			if withTools && repo == "" {
				return fmt.Errorf("--tools requires --repo (the file tools are confined to the repo root)")
			}
			if allowBash && !withTools {
				return fmt.Errorf("--allow-bash requires --tools")
			}
			db, _ := app.OpenDB() // best-effort; solver records when non-nil
			model := app.Cfg.String("profiles."+app.profile("")+".config.model.default", "")
			res, err := solver.Solve(context.Background(), db, prov, model, solver.Options{
				Kind:        solver.KindSWE,
				Task:        args[0],
				RepoPath:    repo,
				MaxSteps:    maxSteps,
				EnableTools: withTools,
				EnableBash:  allowBash,
				RunTests:    runTests,
			})
			if err != nil {
				return err
			}
			return emitSolveResult(res)
		},
	}
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().StringVar(&repo, "repo", "", "repository working directory for context")
	c.Flags().IntVar(&maxSteps, "max-steps", 8, "max agent-loop steps")
	c.Flags().BoolVar(&withTools, "tools", false, "enable root-confined file tools so the agent reads/edits files under --repo")
	c.Flags().BoolVar(&allowBash, "allow-bash", false, "also enable the bash tool (unrestricted host exec; requires --tools)")
	c.Flags().StringVar(&runTests, "run-tests", "", "shell command to run after the loop (working dir = --repo); reports pass/fail")
	root.AddCommand(c)
}

// emitSolveResult renders a solver.Result as JSON (when --json) or plain text.
// Shared by the four agentic-solver commands.
func emitSolveResult(res *solver.Result) error {
	if flagJSON {
		return emitJSON(res)
	}
	fmt.Printf("[%s] run %s via %s (%d step(s), %s)\n", res.Kind, res.ID, res.Provider, res.Steps, res.Stopped)
	fmt.Println(res.Output)
	for _, it := range res.Iterations {
		status := "FAIL"
		if it.Passed {
			status = "PASS"
		}
		fmt.Printf("  iter %d: check %s\n", it.Iteration, status)
		if it.Fix != "" {
			fmt.Printf("    fix suggestion: %s\n", it.Fix)
		}
	}
	if len(res.Iterations) > 0 {
		if res.Converged {
			fmt.Println("result: converged (check passes)")
		} else {
			fmt.Println("result: NOT converged (check still failing)")
		}
	}
	if tr := res.TestResult; tr != nil {
		status := "FAILED"
		if tr.Passed {
			status = "PASSED"
		}
		fmt.Printf("tests: %s (%s)\n", status, tr.Command)
	}
	for _, n := range res.Notes {
		fmt.Printf("note: %s\n", n)
	}
	return nil
}
