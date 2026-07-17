package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/tool"
)

// registerRun wires `tag run` — the native agent loop (Track B). It drives a
// provider through tool-calling turns and records the run to the runs/steps
// tables. Defaults to the offline `echo` provider so it is safe without keys;
// real provider adapters register into llm.Registry and are selected via --provider.
func registerRun(root *cobra.Command, app *App) {
	var provider, system, profile string
	var maxSteps int
	var withTools bool

	c := &cobra.Command{
		Use:     "run <prompt>",
		Short:   "Run the native agent loop on a prompt",
		GroupID: "orch",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			loop := &agent.Loop{Provider: prov}
			if withTools {
				reg := agent.NewRegistry()
				tool.Register(reg, tool.DefaultOptions())
				loop.Tools = reg
			}
			started := time.Now().UTC()
			res, err := loop.Run(context.Background(), args[0], agent.Options{
				Model:  app.Cfg.String("profiles."+app.profile(profile)+".config.model.default", ""),
				System: system, MaxSteps: maxSteps,
			})
			if err != nil {
				return err
			}
			// record the run with usage (best-effort; runtime tables exist from bootstrap)
			runID := uuid.NewString()[:16]
			modelID := app.Cfg.String("profiles."+app.profile(profile)+".config.model.default", "")
			durMs := time.Since(started).Milliseconds()
			if db, derr := app.OpenDB(); derr == nil {
				if _, ierr := db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status,
					model_id,prompt_tokens,completion_tokens,cache_read_tokens,duration_ms,completed_at)
					VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					runID, started.Format(time.RFC3339), "agent", "chat", "native", app.profile(profile), "default",
					args[0], "{}", "completed", modelID, res.TotalUsage.PromptTokens, res.TotalUsage.CompletionTokens,
					res.TotalUsage.CacheReadTokens, durMs, time.Now().UTC().Format(time.RFC3339)); ierr != nil {
					return fmt.Errorf("recording run: %w", ierr)
				}
			}
			if flagJSON {
				return emitJSON(map[string]any{
					"run_id": runID, "provider": provider, "stopped": res.Stopped,
					"steps": len(res.Steps), "final_text": res.FinalText,
					"usage": map[string]int{"prompt_tokens": res.TotalUsage.PromptTokens, "completion_tokens": res.TotalUsage.CompletionTokens},
				})
			}
			for i, s := range res.Steps {
				for _, tc := range s.ToolCalls {
					status := "ok"
					if tc.Err != "" {
						status = "err:" + tc.Err
					}
					fmt.Printf("  [step %d] tool %s -> %s\n", i+1, tc.Name, status)
				}
			}
			fmt.Println(res.FinalText)
			fmt.Printf("\n(run %s: %s in %d step(s), %d prompt + %d completion tokens)\n",
				runID, res.Stopped, len(res.Steps), res.TotalUsage.PromptTokens, res.TotalUsage.CompletionTokens)
			return nil
		},
	}
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().StringVar(&system, "system", "", "system prompt")
	c.Flags().StringVar(&profile, "profile", "", "profile")
	c.Flags().IntVar(&maxSteps, "max-steps", 8, "max agent-loop steps")
	c.Flags().BoolVar(&withTools, "tools", false, "enable built-in tools (bash/read_file/write_file/list_dir)")
	root.AddCommand(c)
}

func providerNames() []string {
	var names []string
	for n := range llm.Registry {
		names = append(names, n)
	}
	return names
}
