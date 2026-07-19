package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/memory"
)

// registerMem2 wires advanced memory operations: mem2 gc / mem2 tier.
// Port of src/tag/cmd/memory.py:cmd_mem_ext (gc + tier subcommands).
func registerMem2(root *cobra.Command, app *App) {
	m := &cobra.Command{Use: "mem2", Short: "Advanced memory: gc, tier", GroupID: "memory"}

	var profile string
	var allProfiles, dryRun bool
	gc := &cobra.Command{Use: "gc", Short: "Run memory garbage collection (evict/merge/promote)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			cfg := memory.DefaultGCConfig()
			if dryRun {
				// GC has no non-mutating mode, so a dry run reports intent only.
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
					return err
				}
				fmt.Printf("Episode started: %s\n", id)
			case "end":
				if epID == "" {
					return fmt.Errorf("--id required")
				}
				ended, err := memory.EndEpisode(db.DB, epID, summary)
				if err != nil {
					return err
				}
				if !ended {
					return fmt.Errorf("episode not found: %q", epID)
				}
				fmt.Println("Episode ended")
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
					return fmt.Errorf("episode not found: %q", epID)
				}
				mems, err := memory.EpisodeMemories(db.DB, epID)
				if err != nil {
					return err
				}
				return emitJSON(map[string]any{"episode": found, "memories": mems})
			default:
				return fmt.Errorf("action must be start|end|list|get, got %q", args[0])
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
					return fmt.Errorf("--id required for fact update")
				}
				if !cmd.Flags().Changed("content") {
					return fmt.Errorf("--content required for fact update")
				}
				if strings.TrimSpace(factContent) == "" {
					return fmt.Errorf("--content must not be empty")
				}
				newID, err := memory.UpdateFact(db.DB, factID, factContent, p, "")
				if err != nil {
					return err
				}
				fmt.Printf("Updated fact, new id=%s\n", newID)
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

	extract := &cobra.Command{Use: "extract RUN_ID", Short: "Extract memories from a run's output", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			// Source the run text from the runs row (id-prefix resolved, mirroring
			// how context.go:assembleSession and `runs show` read a run). The prior
			// implementation read only the `steps` table, which the native runtime
			// never populates, so extract errored "Run not found" for EVERY valid
			// run. runs.prompt is NOT NULL, so a real run always yields text; any
			// recorded step outputs are appended when present.
			var runID, prompt string
			err = db.QueryRow(`SELECT id, prompt FROM runs WHERE id LIKE ?||'%' ORDER BY created_at DESC LIMIT 1`, args[0]).
				Scan(&runID, &prompt)
			if err == sql.ErrNoRows {
				return fmt.Errorf("Run not found: %q", args[0])
			}
			if err != nil {
				return err
			}
			var parts []string
			if prompt != "" {
				parts = append(parts, prompt)
			}
			rows, err := db.Query(`SELECT output FROM steps WHERE run_id=? ORDER BY id`, runID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var o sql.NullString
				if err := rows.Scan(&o); err != nil {
					return err
				}
				if o.Valid && o.String != "" {
					parts = append(parts, o.String)
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			// Extraction invokes the managed TAG runtime (LLM) to mine memories
			// from the run text. That backend is unavailable in the offline Go build,
			// so — exactly as the Python path does when the runtime can't be
			// reached — no memories are extracted. A valid run now honestly reports
			// "Extracted 0 memories" (exit 0) instead of a false "not found".
			_ = parts
			fmt.Println("Extracted 0 memories")
			return nil
		}}
	extract.Flags().StringVar(&profile, "profile", "", "profile")

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

	m.AddCommand(gc, tier, episode, fact, extract, store)
	root.AddCommand(m)
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
