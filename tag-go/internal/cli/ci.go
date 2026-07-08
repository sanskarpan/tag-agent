package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/ciauto"
)

// registerCI wires `ci` and `loop`, both driving the native agent loop offline
// (default provider "echo"). Port of src/tag/cmd/ci_loop.py:cmd_loop/cmd_ci,
// adapted to the Go agent.Loop (internal/agent) which is fully offline via echo.
func registerCI(root *cobra.Command, app *App) {
	// ---- loop <prompt> ----
	var cixLoopProvider string
	var cixLoopIters int
	loop := &cobra.Command{
		Use:     "loop <prompt>",
		Short:   "Run the agent loop over a prompt for N iterations (offline via echo)",
		GroupID: "orch",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cixRunLoop(cixLoopProvider, args[0], cixLoopIters)
		},
	}
	loop.Flags().StringVar(&cixLoopProvider, "provider", "echo", "LLM provider (default echo, offline)")
	loop.Flags().IntVar(&cixLoopIters, "iterations", 1, "number of loop passes")

	// ---- ci <task> (thin alias: one agent-loop pass) ----
	var cixCIProvider string
	ci := &cobra.Command{
		Use:     "ci <task>",
		Short:   "Run one agent-loop pass over a task (offline via echo)",
		GroupID: "orch",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cixRunLoop(cixCIProvider, args[0], 1)
		},
	}
	ci.Flags().StringVar(&cixCIProvider, "provider", "echo", "LLM provider (default echo, offline)")

	root.AddCommand(loop, ci)
}

// cixRunLoop executes the agent loop and prints each iteration's final text.
func cixRunLoop(provider, prompt string, iterations int) error {
	results, err := ciauto.RunLoop(context.Background(), provider, prompt, iterations)
	if err != nil {
		return err
	}
	if flagJSON {
		return emitJSON(results)
	}
	for _, r := range results {
		fmt.Printf("[iteration %d] (%s, %d step(s)) %s\n", r.Iteration, r.Stopped, r.Steps, r.FinalText)
	}
	return nil
}
