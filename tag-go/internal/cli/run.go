package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/memory"
	"github.com/tag-agent/tag/internal/tool"
	"github.com/tag-agent/tag/internal/trace"
)

// registerRun wires `tag run` — the native agent loop (Track B). It drives a
// provider through tool-calling turns and records the run to the runs/steps
// tables. Defaults to the offline `echo` provider so it is safe without keys;
// real provider adapters register into llm.Registry and are selected via --provider.
func registerRun(root *cobra.Command, app *App) {
	var provider, system, profile string
	var maxSteps int
	var timeoutSecs int
	var withTools bool
	var useFallback bool
	var enableWeb bool
	var disableTools []string
	var perms permFlags
	// PRD-065 FR-13: --auto-memorize / --no-auto-memorize force post-run
	// extraction on or off for this invocation, overriding the profile/global
	// config either way. Neither flag → follow config (default: off).
	var forceMemorize, forceNoMemorize bool

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
			runID := uuid.NewString()[:16]
			// #590: the loop emitted no spans at all, so `tag trace list`,
			// `trace show`, `otel-export` and per-span cost were all permanently
			// empty in Go. The trace id IS the run id, so `tag trace show <run>`
			// resolves a run's spans directly.
			rec := trace.NewRecorder(runID, app.profile(profile))
			loop.Tracer = rec
			// Persisting the trace is best-effort and must never fail the run, but
			// it must happen on EVERY exit path (including a provider error) —
			// an untraced failure is exactly the blind spot #590 describes.
			defer func() {
				if db, derr := app.OpenDB(); derr == nil {
					if serr := rec.Save(db.DB); serr != nil {
						fmt.Fprintf(os.Stderr, "  warning: recording trace spans: %v\n", serr)
					}
				}
			}()
			if withTools {
				reg := agent.NewRegistry()
				topts := tool.DefaultOptions()
				topts.EnableExa = enableWeb // Exa web_search (needs EXA_API_KEY)
				// Consent gate: resolved from --allow-tool/--deny-tool/--auto-approve
				// and the profile's permissions block. Without it tool.Register would
				// fall back to the secure default policy (bash/write_file denied when
				// headless) — this just makes it configurable.
				g, gerr := perms.guard(app, profile, runID, os.Stderr)
				if gerr != nil {
					return gerr
				}
				topts.Guard = g
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
			// Bound the whole agent loop. This used context.Background(), so a
			// provider that accepted the connection and never answered kept
			// `tag run` silent for the HTTP client's 10-minute timeout — TAG must
			// never hang silently. Deriving from cmd.Context() also lets an
			// interrupt propagate into the in-flight request.
			ctx := cmd.Context()
			if timeoutSecs > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
				defer cancel()
			}
			started := time.Now().UTC()
			// A provider that accepts the connection and then stalls produces NO
			// output until ResponseHeaderTimeout (60s by default) fires. That is
			// bounded and it does fail honestly — but a full minute of silence
			// reads as a hang, which is the one thing this project promises not
			// to look like. Say we are still waiting, on stderr so stdout stays
			// parseable and --json is unaffected.
			stopWait := startWaitNotice(cmd.ErrOrStderr())
			res, err := loop.Run(ctx, args[0], agent.Options{
				Model:  app.Cfg.String("profiles."+app.profile(profile)+".config.model.default", ""),
				System: system, MaxSteps: maxSteps,
			})
			stopWait()
			if err != nil {
				// Report a deadline as a deadline, not as an opaque transport error.
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return fmt.Errorf("run timed out after %ds waiting on provider %q (raise --timeout, or use 0 to disable): %w",
						timeoutSecs, provider, err)
				}
				return err
			}
			// record the run with usage (best-effort; runtime tables exist from bootstrap)
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
			// PRD-065 post-run hook. Deliberately AFTER the run row is recorded and
			// (below) after the run's own output, so nothing it does can change the
			// run's result or exit code. Opt-in only.
			defer maybeAutoExtract(cmd.Context(), app, prov, runID, app.profile(profile), forceMemorize, forceNoMemorize)
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
	c.Flags().IntVar(&timeoutSecs, "timeout", 300, "abort the run after N seconds (0 = no limit)")
	c.Flags().BoolVar(&withTools, "tools", false, "enable built-in tools (bash/read_file/write_file/list_dir)")
	c.Flags().BoolVar(&enableWeb, "web", false, "add the Exa web_search tool (requires --tools and EXA_API_KEY)")
	c.Flags().StringSliceVar(&disableTools, "disable-tools", nil, "tool-budget: comma-list of tool names to omit (e.g. bash,write_file)")
	c.Flags().BoolVar(&useFallback, "fallback", false, "on a retryable provider error, walk the profile's route-fallback chain")
	c.Flags().BoolVar(&forceMemorize, "auto-memorize", false, "extract memories from this run when it finishes (overrides memory.auto_extract)")
	c.Flags().BoolVar(&forceNoMemorize, "no-auto-memorize", false, "skip post-run memory extraction even if memory.auto_extract is on")
	perms.bind(c)
	root.AddCommand(c)
}

// maybeAutoExtract is the PRD-065 post-run hook.
//
// Three properties are load-bearing:
//
//   - OPT-IN. It returns immediately unless the user asked for extraction, via
//     --auto-memorize or memory.auto_extract (profile, then global, then
//     TAG_MEMORY_AUTO_EXTRACT). Extraction calls an LLM, so no `tag run` may
//     acquire cost or latency it was not asked for.
//   - NON-FATAL. Every failure — rate limit, timeout, provider error — becomes a
//     stderr warning. It runs from a deferred call after the run's own output,
//     so it cannot change what `tag run` printed or the exit code it returns.
//   - QUIET ON STDOUT. The summary goes to stderr, so `tag run --json` stdout
//     stays a single parseable object.
//
// It runs synchronously rather than in a detached goroutine on purpose: a CLI
// process exits as soon as RunE returns, so a background goroutine would be
// killed mid-flight and the "extraction" would be a silent no-op — the exact
// class of fake success this project bars.
func maybeAutoExtract(ctx context.Context, app *App, prov llm.Provider, runID, profile string, forceOn, forceOff bool) {
	if forceOff {
		return
	}
	if !forceOn {
		enabled, _ := autoExtractSetting(app, profile)
		if !enabled {
			return
		}
	}
	db, err := app.OpenDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: post-run memory extraction skipped (db: %v)\n", err)
		return
	}
	// The extractor uses the same provider the run used, so `--provider echo`
	// stays entirely offline and honestly extracts nothing.
	if p := memoryCfgString(app, profile, "extractor_provider", ""); p != "" {
		if alt, ok := llm.Registry[p]; ok {
			prov = alt
		} else {
			fmt.Fprintf(os.Stderr, "  warning: memory.extractor_provider %q is not a registered provider — using %q\n", p, prov.Name())
		}
	}
	res, err := memory.Extract(ctx, db.DB, prov, runID, memory.ExtractOptions{
		Profile: profile,
		Model:   app.Cfg.String("profiles."+profile+".config.model.default", ""),
		Timeout: extractTimeout(app),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: post-run memory extraction failed (run is unaffected): %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "  memory: extracted %d, skipped %d, redacted %d from run %s\n",
		res.Added, res.Skipped, res.Redacted, short(res.RunID))
	if res.Note != "" {
		fmt.Fprintf(os.Stderr, "  memory: %s\n", res.Note)
	}
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
