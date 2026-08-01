package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/memory"
)

// registerMem2 wires advanced memory operations: mem2 gc / mem2 tier.
// Port of src/tag/cmd/memory.py:cmd_mem_ext (gc + tier subcommands).
func registerMem2(root *cobra.Command, app *App) {
	m := &cobra.Command{Use: "mem2", Short: "Advanced memory: gc, tier", GroupID: "memory"}

	var profile string
	var allProfiles, dryRun, gcDaemon bool
	var gcInterval time.Duration
	gc := &cobra.Command{Use: "gc", Short: "Run memory garbage collection (evict/merge/promote); --daemon for sleep-time consolidation", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			cfg := memory.DefaultGCConfig()
			if gcDaemon {
				// PRD-068: the same consolidation pipeline, driven on a schedule.
				// --dry-run is meaningless here (a daemon that never mutates would
				// just burn cycles), so reject the combination as a usage error
				// rather than silently ignoring one of the two flags.
				if dryRun {
					return jsonErrorMaybe(usageErrorf("--daemon and --dry-run are mutually exclusive"))
				}
				if err := memory.ValidateInterval(gcInterval); err != nil {
					return jsonErrorMaybe(usageErr{err})
				}
				// SIGINT/SIGTERM cancels the wait, so shutdown is immediate rather
				// than "at the end of the current interval" (same shape as `cron daemon`).
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				scope := app.profile(profile)
				if allProfiles {
					scope = "all profiles"
				}
				fmt.Printf("TAG memory consolidation daemon starting (every %s, scope: %s) — Ctrl+C to stop\n", gcInterval, scope)
				err := memory.RunConsolidationDaemon(ctx, db.DB, memory.ConsolidationOptions{
					Interval:    gcInterval,
					Profile:     app.profile(profile),
					AllProfiles: allProfiles,
					Cfg:         cfg,
					OnCycle: func(cycle int, results []memory.GCResult, cerr error) {
						if cerr != nil {
							// A transient DB error must not kill a background agent.
							fmt.Fprintf(os.Stderr, "  consolidation cycle %d failed: %v\n", cycle, cerr)
							return
						}
						for _, r := range results {
							fmt.Printf("  cycle %d %s: evicted=%d merged=%d promoted=%d (%.3fs)\n",
								cycle, r.Profile, r.EvictedCount, r.MergedCount, r.PromotedCount, r.DurationSeconds)
						}
					},
				})
				if err != nil {
					return err
				}
				fmt.Println("memory consolidation daemon stopping")
				return nil
			}
			if dryRun {
				// GC has no non-mutating mode, so a dry run reports intent only.
				// Under --json the preview must still be JSON, and must NOT be
				// shaped like a completed GCResult: `dry_run: true` and the absence
				// of evicted/merged/promoted counts are what tell a consumer that
				// nothing happened.
				if flagJSON {
					return emitJSON(map[string]any{
						"profile":                  app.profile(profile),
						"dry_run":                  true,
						"max_memories_per_profile": cfg.MaxMemoriesPerProfile,
						"min_confidence_to_keep":   cfg.MinConfidenceToKeep,
					})
				}
				fmt.Printf("dry-run: GC preview for '%s' — no changes made. Re-run without --dry-run to evict/merge/promote (cap=%d, min_confidence=%g).\n",
					app.profile(profile), cfg.MaxMemoriesPerProfile, cfg.MinConfidenceToKeep)
				return nil
			}
			if allProfiles {
				results, err := memory.RunGCAllProfiles(db.DB, cfg)
				if err != nil {
					return err
				}
				if flagJSON {
					return emitJSON(results)
				}
				for _, r := range results {
					fmt.Printf("%s: evicted=%d merged=%d promoted=%d\n", r.Profile, r.EvictedCount, r.MergedCount, r.PromotedCount)
				}
				return nil
			}
			r, err := memory.RunGC(db.DB, app.profile(profile), cfg)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(r)
			}
			fmt.Printf("GC done: evicted=%d merged=%d promoted=%d\n", r.EvictedCount, r.MergedCount, r.PromotedCount)
			return nil
		}}
	gc.Flags().StringVar(&profile, "profile", "", "profile")
	gc.Flags().BoolVar(&allProfiles, "all-profiles", false, "GC every profile")
	gc.Flags().BoolVar(&dryRun, "dry-run", false, "preview only; make no changes")
	gc.Flags().BoolVar(&gcDaemon, "daemon", false, "run consolidation continuously in the background (blocking; SIGTERM to stop)")
	gc.Flags().DurationVar(&gcInterval, "interval", memory.DefaultConsolidationInterval, "consolidation cadence for --daemon (e.g. 30m, 1h)")

	var tierFilter string
	tier := &cobra.Command{Use: "tier", Short: "List memories grouped by tier (core/recall/archival)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			mems, err := memory.List(db.DB, app.profile(profile), "", 0)
			if err != nil {
				return err
			}
			tiers := memory.MemoryTiers
			if tierFilter != "" {
				valid := false
				for _, t := range tiers {
					if t == tierFilter {
						valid = true
					}
				}
				if !valid {
					return fmt.Errorf("tier must be one of core/recall/archival, got %q", tierFilter)
				}
				tiers = []string{tierFilter}
			}
			// classify each memory by its effective (decayed) confidence
			byTier := map[string][]memory.Mem{}
			for _, mm := range mems {
				byTier[memory.Tier(mm.Confidence, mm.CreatedAt)] = append(byTier[memory.Tier(mm.Confidence, mm.CreatedAt)], mm)
			}
			if flagJSON {
				out := map[string]any{}
				for _, t := range tiers {
					group := byTier[t]
					if group == nil {
						group = []memory.Mem{}
					}
					out[t] = group
				}
				return emitJSON(out)
			}
			for _, t := range tiers {
				group := byTier[t]
				fmt.Printf("\n=== %s (%d) ===\n", upper(t), len(group))
				for _, mm := range group {
					fmt.Printf("  [%.3f] %s\n", mm.Confidence, truncate(mm.Content, 80))
				}
			}
			return nil
		}}
	tier.Flags().StringVar(&profile, "profile", "", "profile")
	tier.Flags().StringVar(&tierFilter, "tier", "", "only show this tier")

	var epID, summary string
	episode := &cobra.Command{Use: "episode <start|end|list|get> [id]", Short: "Episodic memory sessions", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			p := app.profile(profile)
			// allow the episode id as an optional positional arg (falls back to --id)
			if len(args) == 2 && epID == "" {
				epID = args[1]
			}
			switch args[0] {
			case "start":
				id, err := memory.StartEpisode(db.DB, p, strOr(summary, "CLI session"))
				if err != nil {
					return jsonErrorMaybe(err)
				}
				// `episode list`/`get` already key on episode_id (as Python's
				// list_episodes does), so start must hand back the same field name
				// rather than only a prose line a --json caller has to scrape.
				outJSON(map[string]any{"episode_id": id, "profile": p},
					fmt.Sprintf("Episode started: %s", id))
			case "end":
				if epID == "" {
					return jsonErrorMaybe(fmt.Errorf("--id required"))
				}
				ended, err := memory.EndEpisode(db.DB, epID, summary)
				if err != nil {
					return jsonErrorMaybe(err)
				}
				if !ended {
					return jsonErrorMaybe(fmt.Errorf("episode not found: %q", epID))
				}
				outJSON(map[string]any{"episode_id": epID, "status": "ended"}, "Episode ended")
			case "list":
				eps, err := memory.ListEpisodes(db.DB, p, 20)
				if err != nil {
					return err
				}
				if eps == nil {
					eps = []memory.Episode{}
				}
				return emitJSON(eps)
			case "get":
				if epID == "" {
					return fmt.Errorf("--id required")
				}
				eps, err := memory.ListEpisodes(db.DB, p, 1000)
				if err != nil {
					return err
				}
				var found *memory.Episode
				for i := range eps {
					if eps[i].EpisodeID == epID {
						found = &eps[i]
						break
					}
				}
				if found == nil {
					return jsonErrorMaybe(fmt.Errorf("episode not found: %q", epID))
				}
				mems, err := memory.EpisodeMemories(db.DB, epID)
				if err != nil {
					return err
				}
				return emitJSON(map[string]any{"episode": found, "memories": mems})
			default:
				return jsonErrorMaybe(fmt.Errorf("action must be start|end|list|get, got %q", args[0]))
			}
			return nil
		}}
	episode.Flags().StringVar(&profile, "profile", "", "profile")
	episode.Flags().StringVar(&epID, "id", "", "episode id (for end/get)")
	episode.Flags().StringVar(&summary, "summary", "", "episode summary/description")

	var factID, factContent, atTime string
	fact := &cobra.Command{Use: "fact <update|history|list-at>", Short: "Temporal fact versioning", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			p := app.profile(profile)
			switch args[0] {
			case "update":
				if factID == "" {
					return jsonErrorMaybe(fmt.Errorf("--id required for fact update"))
				}
				if !cmd.Flags().Changed("content") {
					return jsonErrorMaybe(fmt.Errorf("--content required for fact update"))
				}
				if strings.TrimSpace(factContent) == "" {
					return jsonErrorMaybe(fmt.Errorf("--content must not be empty"))
				}
				newID, err := memory.UpdateFact(db.DB, factID, factContent, p, "")
				if err != nil {
					return jsonErrorMaybe(err)
				}
				// A fact update supersedes one memory with a new one, so a --json
				// caller needs BOTH ids: the new row to follow, and the superseded
				// one it can no longer resolve.
				outJSON(map[string]any{"id": newID, "previous_id": factID, "profile": p},
					fmt.Sprintf("Updated fact, new id=%s", newID))
			case "history":
				if factID == "" {
					return fmt.Errorf("--id required")
				}
				hist, err := memory.FactHistory(db.DB, factID)
				if err != nil {
					return err
				}
				if hist == nil {
					hist = []memory.FactVersion{}
				}
				return emitJSON(hist)
			case "list-at":
				at := atTime
				if at == "" {
					at = time.Now().UTC().Format(time.RFC3339)
				}
				facts, err := memory.FactAt(db.DB, p, at)
				if err != nil {
					return err
				}
				if facts == nil {
					facts = []memory.Mem{}
				}
				return emitJSON(facts)
			default:
				return fmt.Errorf("action must be update|history|list-at, got %q", args[0])
			}
			return nil
		}}
	fact.Flags().StringVar(&profile, "profile", "", "profile")
	fact.Flags().StringVar(&factID, "id", "", "memory id to update/inspect")
	fact.Flags().StringVar(&factContent, "content", "", "new content (for update)")
	fact.Flags().StringVar(&atTime, "at", "", "ISO timestamp for list-at (default now)")

	var exProvider, exModel string
	var exDryRun bool
	var exMaxTurns int
	extract := &cobra.Command{Use: "extract RUN_ID", Short: "Extract memories from a run's transcript", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			prov, ok := llm.Registry[exProvider]
			if !ok {
				return jsonErrorMaybe(usageErrorf("unknown provider %q (available: %v)", exProvider, providerNames()))
			}
			p := app.profile(profile)
			if exModel == "" {
				exModel = app.Cfg.String("profiles."+p+".config.model.default", "")
			}
			res, err := memory.Extract(cmd.Context(), db.DB, prov, args[0], memory.ExtractOptions{
				Profile:  p,
				Model:    exModel,
				DryRun:   exDryRun,
				MaxTurns: exMaxTurns,
				Timeout:  extractTimeout(app),
			})
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(res)
			}
			printExtractResult(res)
			return nil
		}}
	extract.Flags().StringVar(&profile, "profile", "", "profile")
	extract.Flags().StringVar(&exProvider, "provider", "echo", "llm provider for the extractor (echo = offline; extracts nothing, honestly)")
	extract.Flags().StringVar(&exModel, "model", "", "extractor model (defaults to the profile's model)")
	extract.Flags().BoolVar(&exDryRun, "dry-run", false, "classify candidates but write nothing")
	extract.Flags().IntVar(&exMaxTurns, "max-turns", 0, "only use the last N transcript turns (0 = all)")

	var exLast int
	extractions := &cobra.Command{Use: "extractions", Short: "List post-run memory extraction history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			scope := ""
			if cmd.Flags().Changed("profile") || !allProfiles {
				scope = app.profile(profile)
			}
			rows, err := memory.ListExtractions(db.DB, scope, exLast)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(rows)
			}
			if len(rows) == 0 {
				fmt.Println("No extraction runs recorded.")
				return nil
			}
			for _, r := range rows {
				fmt.Printf("%-18s run=%-16s %-8s found=%-3d added=%-3d skipped=%-3d redacted=%-3d %s\n",
					r.ID, short(r.RunID), r.Status, r.Found, r.Added, r.Skipped, r.Redacted, r.StartedAt)
			}
			return nil
		}}
	extractions.Flags().StringVar(&profile, "profile", "", "profile")
	extractions.Flags().BoolVar(&allProfiles, "all-profiles", false, "include every profile")
	extractions.Flags().IntVar(&exLast, "last", 20, "max rows")

	memConfig := &cobra.Command{Use: "config <show|set> [key] [value]", Short: "Show or set memory config (auto_extract, extractor_provider, extractor_timeout_s)",
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := app.profile(profile)
			switch args[0] {
			case "show":
				enabled, origin := autoExtractSetting(app, p)
				out := map[string]any{
					"profile":             p,
					"auto_extract":        enabled,
					"auto_extract_origin": origin,
					"extractor_provider":  memoryCfgString(app, p, "extractor_provider", "echo"),
					"extractor_timeout_s": int(extractTimeout(app).Seconds()),
				}
				if flagJSON {
					return emitJSON(out)
				}
				fmt.Printf("Memory config for profile: %s\n", p)
				fmt.Printf("  auto_extract:        %v  (%s)\n", enabled, origin)
				fmt.Printf("  extractor_provider:  %s\n", out["extractor_provider"])
				fmt.Printf("  extractor_timeout_s: %v\n", out["extractor_timeout_s"])
				return nil
			case "set":
				if len(args) != 3 {
					return jsonErrorMaybe(usageErrorf("usage: mem2 config set KEY VALUE"))
				}
				key, val := args[1], args[2]
				switch key {
				case "auto_extract", "extractor_provider", "extractor_timeout_s":
				default:
					return jsonErrorMaybe(usageErrorf("unknown memory config key %q (auto_extract|extractor_provider|extractor_timeout_s)", key))
				}
				target := "global"
				if cmd.Flags().Changed("profile") {
					target = p
				}
				if _, err := config.Update(app.ConfigPath, func(data map[string]any) {
					var dst map[string]any
					if target == "global" {
						dst = childMap(data, "memory")
					} else {
						dst = childMap(childMap(childMap(data, "profiles"), p), "memory")
					}
					dst[key] = coerceCfgValue(val)
				}); err != nil {
					return jsonErrorMaybe(err)
				}
				outJSON(map[string]any{"scope": target, "key": key, "value": coerceCfgValue(val)},
					fmt.Sprintf("Updated memory config (%s): %s = %s", target, key, val))
				return nil
			}
			return jsonErrorMaybe(usageErrorf("action must be show|set, got %q", args[0]))
		}}
	memConfig.Flags().StringVar(&profile, "profile", "", "profile (omit to read/write the global default)")

	var storeQuery, storeID string
	var storeForce bool
	var storeLimit int
	store := &cobra.Command{Use: "store <store|search|rebuild>", Short: "Store or search vector embeddings", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			p := app.profile(profile)
			// Resolve the embeddings backend from the environment. When no key /
			// base URL is configured, embedder stays a nil interface and vector
			// paths degrade to FTS (search) or error clearly (store/rebuild).
			// NOTE: keep this a nil *interface* — boxing a nil *OpenAIEmbedder into
			// the interface would defeat the e==nil guards in the memory package.
			var embedder memory.Embedder
			model := memory.DefaultEmbedModel
			if e, ok := memory.EmbedderFromEnv(); ok {
				embedder = e
				model = e.Model()
			}
			limit := storeLimit
			if limit <= 0 {
				limit = 10
			}
			switch args[0] {
			case "store":
				if storeID == "" {
					return jsonErrorMaybe(fmt.Errorf("--id required for store"))
				}
				n, err := memory.StoreEmbedding(context.Background(), db.DB, embedder, p, storeID)
				if err != nil {
					return err
				}
				if flagJSON {
					return emitJSON(map[string]any{"id": storeID, "profile": p, "dims": n, "model": model})
				}
				fmt.Printf("Stored embedding for %s (%d dims, model %s)\n", storeID, n, model)
				return nil
			case "search":
				// Embed the query and cosine-rank stored vectors. Falls back to FTS
				// transparently when no embedding key is configured, the query can't
				// be embedded, or no memories carry vectors yet (mirrors Python's
				// search_by_vector). Always prints the JSON list.
				hits, vectorUsed, err := memory.SearchByVector(context.Background(), db.DB, embedder, p, strings.TrimSpace(storeQuery), limit)
				if err != nil {
					return err
				}
				if hits == nil {
					hits = []memory.VectorHit{}
				}
				if flagJSON {
					return emitJSON(map[string]any{"mode": searchMode(vectorUsed), "results": hits})
				}
				return emitJSON(hits)
			case "rebuild":
				n, err := memory.RebuildEmbeddings(context.Background(), db.DB, embedder, p, storeForce)
				if err != nil {
					return err
				}
				if flagJSON {
					return emitJSON(map[string]any{"profile": p, "embedded": n, "model": model})
				}
				fmt.Printf("Rebuilt embeddings: %d memories embedded (model %s)\n", n, model)
				return nil
			default:
				return jsonErrorMaybe(fmt.Errorf("Unknown store action: %q", args[0]))
			}
		}}
	store.Flags().StringVar(&profile, "profile", "", "profile")
	store.Flags().StringVar(&storeQuery, "query", "", "query text (for search)")
	store.Flags().StringVar(&storeID, "id", "", "memory id (for store)")
	store.Flags().BoolVar(&storeForce, "force", false, "re-embed all memories, not just those missing a vector (rebuild)")
	store.Flags().IntVar(&storeLimit, "limit", 10, "max results (search)")

	m.AddCommand(gc, tier, episode, fact, extract, extractions, memConfig, store)
	root.AddCommand(m)
}

// ---- PRD-065 helpers ---------------------------------------------------------

// coerceCfgValue turns a CLI string into the natural YAML scalar so
// `mem2 config set auto_extract true` stores a bool, not the string "true".
func coerceCfgValue(v string) any {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return n
	}
	return v
}

// cfgTruthy interprets a YAML scalar as a boolean; the second result reports
// whether the key was present at all (so an explicit `false` beats a default).
func cfgTruthy(v any) (bool, bool) {
	switch t := v.(type) {
	case nil:
		return false, false
	case bool:
		return t, true
	case int:
		return t != 0, true
	case float64:
		return t != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "yes", "on", "1":
			return true, true
		case "false", "no", "off", "0":
			return false, true
		}
	}
	return false, false
}

// memoryCfg returns the memory stanza for a profile (or the global one).
func memoryCfg(app *App, profile string) (perProfile, global map[string]any) {
	global = asMap(app.Cfg.Section("memory"))
	perProfile = asMap(asMap(asMap(app.Cfg.Profiles())[profile])["memory"])
	return perProfile, global
}

func memoryCfgString(app *App, profile, key, def string) string {
	per, glob := memoryCfg(app, profile)
	if s := str(per[key]); s != "" {
		return s
	}
	if s := str(glob[key]); s != "" {
		return s
	}
	return def
}

// autoExtractSetting resolves whether post-run extraction is enabled for a
// profile, and says where the answer came from. Precedence: per-profile config,
// then global config, then TAG_MEMORY_AUTO_EXTRACT, then off.
//
// Off is the default on purpose: extraction calls an LLM, and no `tag run`
// should ever acquire a cost it was not asked for.
func autoExtractSetting(app *App, profile string) (bool, string) {
	per, glob := memoryCfg(app, profile)
	if v, ok := cfgTruthy(per["auto_extract"]); ok {
		return v, "profile config"
	}
	if v, ok := cfgTruthy(glob["auto_extract"]); ok {
		return v, "global config"
	}
	if v, ok := cfgTruthy(os.Getenv("TAG_MEMORY_AUTO_EXTRACT")); ok {
		return v, "TAG_MEMORY_AUTO_EXTRACT"
	}
	return false, "default (off)"
}

// extractTimeout bounds the extractor LLM call.
func extractTimeout(app *App) time.Duration {
	_, glob := memoryCfg(app, app.Cfg.MasterProfile())
	if n, ok := glob["extractor_timeout_s"].(int); ok && n > 0 {
		return time.Duration(n) * time.Second
	}
	if f, ok := glob["extractor_timeout_s"].(float64); ok && f > 0 {
		return time.Duration(f * float64(time.Second))
	}
	return memory.DefaultExtractTimeout
}

// printExtractResult renders one extraction in human form. It always states the
// honest outcome — including the "the model returned no fact array" note that
// an offline provider produces — so a zero is never mistaken for a clean run.
func printExtractResult(res *memory.ExtractResult) {
	if res.DryRun {
		fmt.Println("DRY RUN — no changes will be written")
	}
	for _, f := range res.Facts {
		label := f.Operation
		if res.DryRun && f.Operation == memory.OpAdd {
			label = "would ADD"
		}
		line := fmt.Sprintf("  %-10s (%.2f) %s", label, f.Confidence, f.Text)
		if f.Reason != "" {
			line += "  — " + f.Reason
		}
		fmt.Println(line)
	}
	if res.Note != "" {
		fmt.Printf("note: %s\n", res.Note)
	}
	verb := "Extracted"
	if res.DryRun {
		verb = "Would extract"
	}
	fmt.Printf("%s %d memories (%d skipped, %d redacted) from run %s in %dms via provider %q\n",
		verb, res.Added, res.Skipped, res.Redacted, short(res.RunID), res.DurationMS, res.Provider)
}

// searchMode labels how mem2 store search produced its results, so callers can
// tell semantic ranking from the FTS fallback.
func searchMode(vectorUsed bool) string {
	if vectorUsed {
		return "vector"
	}
	return "fts"
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}
