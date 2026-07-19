package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
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
	var useFallback bool
	var enableWeb bool
	var disableTools []string

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
			primaryModel := app.Cfg.String("profiles."+app.profile(profile)+".config.model.default", "")
			// When --fallback is set and the profile has a route_fallbacks chain for
			// the primary model, wrap the provider so 429/401/timeout/overload during
			// inference walks the declared chain (gap #2) instead of failing hard.
			if useFallback {
				fp, err := buildFallbackProvider(app, prov, provider, primaryModel, profile)
				if err != nil {
					return err
				}
				if fp != nil {
					prov = fp
				}
			}
			loop := &agent.Loop{Provider: prov}
			if enableWeb {
				switch {
				case !withTools:
					fmt.Fprintln(os.Stderr, "  warning: --web has no effect without --tools; web_search not registered")
				case os.Getenv("EXA_API_KEY") == "":
					fmt.Fprintln(os.Stderr, "  warning: --web set but EXA_API_KEY is empty; web_search not registered")
				}
			}
			if withTools {
				reg := agent.NewRegistry()
				topts := tool.DefaultOptions()
				topts.EnableExa = enableWeb // Exa web_search (needs EXA_API_KEY)
				if len(disableTools) > 0 {
					topts.Disabled = map[string]bool{}
					for _, name := range disableTools {
						if n := strings.TrimSpace(name); n != "" {
							topts.Disabled[n] = true
						}
					}
				}
				tool.Register(reg, topts)
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
	c.Flags().BoolVar(&enableWeb, "web", false, "add the Exa web_search tool (requires --tools and EXA_API_KEY)")
	c.Flags().StringSliceVar(&disableTools, "disable-tools", nil, "tool-budget: comma-list of tool names to omit (e.g. bash,write_file)")
	c.Flags().BoolVar(&useFallback, "fallback", false, "on a retryable provider error, walk the profile's route-fallback chain")
	root.AddCommand(c)
}

// buildFallbackProvider constructs an llm.FallbackProvider from a profile's
// route_fallbacks chain for the given primary model. It returns nil (no error)
// when no chain is configured, so the caller keeps its single provider. Each
// step's provider is resolved from the "provider/model" prefix (falling back to
// the primary provider when a model has no prefix); a step whose provider slug
// isn't registered is skipped so a partially-registered chain still runs.
func buildFallbackProvider(app *App, primaryProv llm.Provider, primaryProvSlug, primaryModel, profile string) (*llm.FallbackProvider, error) {
	if primaryModel == "" {
		return nil, nil
	}
	db, err := app.OpenDB()
	if err != nil {
		return nil, err
	}
	// The primary model can be stored either bare ("gpt-4o-mini", as set-model
	// splits it into model.default + model.provider) or prefixed
	// ("openai/gpt-4o-mini", as a route-fallback --primary is typically typed).
	// Match both forms so the chain resolves regardless of which the user used.
	prof := app.profile(profile)
	cfgProv := app.Cfg.String("profiles."+prof+".config.model.provider", "")
	// modelCandidates expands a model ref into every stored form it could match:
	// the ref as given, plus — for a bare ref — its provider-prefixed forms (from
	// the profile's configured provider and the primary provider slug), and — for
	// a prefixed ref — its bare form. A route_fallbacks graph may store the same
	// logical model under either form at different depths, so matching only the
	// exact edge string (as walk() did before) dead-links a depth-2 chain whose
	// edges use mixed prefix forms (openai/gpt-x vs gpt-x). See #564.
	modelCandidates := func(model string) []string {
		out := []string{model}
		seen := map[string]bool{model: true}
		add := func(m string) {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
		if strings.Contains(model, "/") {
			add(stripProviderPrefix(model))
		} else {
			if cfgProv != "" {
				add(cfgProv + "/" + model)
			}
			if primaryProvSlug != "" {
				add(primaryProvSlug + "/" + model)
			}
		}
		return out
	}
	candidates := modelCandidates(primaryModel)
	steps := []llm.FallbackStep{{Provider: primaryProv, Model: primaryModel}}
	// The stored route_fallbacks form a graph: a fallback can itself declare
	// fallbacks, so primary->A->B is a valid depth-2 chain. Walk it transitively
	// (DFS, priority order) rather than only reading the primary's direct edges,
	// so every declared step is reachable at runtime. `visited` guards against
	// re-adding a model (and against cycles, though `add` already rejects those).
	visited := map[string]bool{}
	for _, c := range candidates {
		visited[c] = true
	}
	var walk func(models []string) error
	walk = func(models []string) error {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(models)), ",")
		qargs := []any{prof}
		for _, m := range models {
			qargs = append(qargs, m)
		}
		rows, err := db.Query(`SELECT fallback_model, condition FROM route_fallbacks
			WHERE profile=? AND primary_model IN (`+placeholders+`) AND enabled=1 ORDER BY priority`, qargs...)
		if err != nil {
			return err
		}
		type edge struct{ model, cond string }
		var edges []edge
		for rows.Next() {
			var fm, cond string
			if err := rows.Scan(&fm, &cond); err != nil {
				rows.Close()
				return err
			}
			edges = append(edges, edge{fm, cond})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, e := range edges {
			if visited[e.model] {
				continue
			}
			// Mark every equivalent prefix form visited so the same logical model
			// reached via a different form (bare vs prefixed) isn't re-added.
			childForms := modelCandidates(e.model)
			for _, cf := range childForms {
				visited[cf] = true
			}
			p := providerForModel(e.model, primaryProvSlug)
			// Pass the bare model id to the adapter (the provider is resolved from
			// the "provider/" prefix separately; adapters expect an unprefixed model).
			steps = append(steps, llm.FallbackStep{Provider: p, Model: stripProviderPrefix(e.model), Condition: e.cond})
			// Recurse across all prefix forms of this edge, so a child edge stored
			// under a different form than e.model is still discovered (#564).
			if err := walk(childForms); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(candidates); err != nil {
		return nil, err
	}
	if len(steps) < 2 {
		return nil, nil // no fallbacks configured for this primary
	}
	return &llm.FallbackProvider{
		Steps: steps,
		OnFallback: func(i int, model string, err error) {
			fmt.Fprintf(os.Stderr, "  fallback: step %d (%s) failed (%v) — trying next\n", i, model, err)
		},
	}, nil
}

// providerForModel resolves the llm.Provider for a "provider/model" ref, falling
// back to the default provider slug when the ref has no prefix.
func providerForModel(modelRef, defaultSlug string) llm.Provider {
	slug := defaultSlug
	if i := strings.IndexByte(modelRef, '/'); i > 0 {
		slug = modelRef[:i]
	}
	return llm.Registry[slug] // may be nil (unregistered) — FallbackProvider skips it
}

// stripProviderPrefix returns the model id without its "provider/" prefix, since
// provider adapters expect a bare model (e.g. "claude-haiku-4-5", not
// "anthropic/claude-haiku-4-5").
func stripProviderPrefix(modelRef string) string {
	if i := strings.IndexByte(modelRef, '/'); i > 0 {
		return modelRef[i+1:]
	}
	return modelRef
}

func providerNames() []string {
	var names []string
	for n := range llm.Registry {
		names = append(names, n)
	}
	return names
}
