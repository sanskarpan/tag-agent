package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/memory"
)

// checkMemLimit rejects a negative --limit with the SAME message and JSON shape
// `queue list` uses. `queue list --limit -1 --json` already emitted
// {"error": ...} while the mem commands printed bare prose under --json, so a
// consumer had to special-case each command to learn it had passed bad input.
func checkMemLimit(limit int) error {
	if limit >= 0 {
		return nil
	}
	return jsonErrorMaybe(fmt.Errorf("--limit must be >= 0, got %d.", limit))
}

func (a *App) profile(flag string) string {
	if flag != "" {
		return flag
	}
	return a.Cfg.MasterProfile()
}

func registerMemory(root *cobra.Command, app *App) {
	var profile string
	// ---- memory-journal ----
	mj := &cobra.Command{Use: "memory-journal", Short: "Cross-session memory journal", GroupID: "memory"}
	mj.PersistentFlags().StringVar(&profile, "profile", "", "profile")

	mjSave := &cobra.Command{Use: "save KEY VALUE", Short: "Save a journal entry", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			id := uuid.NewString()[:12]
			now := time.Now().UTC().Format(time.RFC3339)
			_, err = db.Exec(`INSERT INTO memory_journal(id,profile,key,value,scope,created_at) VALUES(?,?,?,?,'profile',?)
				ON CONFLICT(profile,key) DO UPDATE SET value=excluded.value`, id, app.profile(profile), args[0], args[1], now)
			if err != nil {
				return err
			}
			outJSON(map[string]any{"saved": args[0]}, fmt.Sprintf("Saved '%s'", args[0]))
			return nil
		}}
	mjList := &cobra.Command{Use: "list", Short: "List journal entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT key,value FROM memory_journal WHERE profile=? ORDER BY created_at DESC`, app.profile(profile))
			if err != nil {
				return err
			}
			defer rows.Close()
			items := []map[string]string{}
			for rows.Next() {
				var k, v string
				if err := rows.Scan(&k, &v); err != nil {
					return err
				}
				items = append(items, map[string]string{"key": k, "value": v})
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				b, _ := json.Marshal(items)
				fmt.Println(string(b))
			} else if len(items) == 0 {
				fmt.Printf("No entries for profile '%s'.\n", app.profile(profile))
			} else {
				for _, it := range items {
					fmt.Printf("%-24s %s\n", it["key"], it["value"])
				}
			}
			return nil
		}}
	mjForget := &cobra.Command{Use: "forget KEY", Short: "Delete a journal entry", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			r, err := db.Exec(`DELETE FROM memory_journal WHERE profile=? AND key=?`, app.profile(profile), args[0])
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n == 0 {
				return fmt.Errorf("key not found: %s", args[0])
			}
			outJSON(map[string]any{"deleted": true}, "deleted")
			return nil
		}}
	var mjConfirm bool
	mjClear := &cobra.Command{Use: "clear", Short: "Clear all journal entries for a profile", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !mjConfirm {
				fmt.Println("Pass --confirm to clear all journal entries for this profile.")
				os.Exit(1)
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			r, err := db.Exec(`DELETE FROM memory_journal WHERE profile=?`, app.profile(profile))
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			outJSON(map[string]any{"cleared": n}, fmt.Sprintf("cleared %d entries", n))
			return nil
		}}
	mjClear.Flags().BoolVar(&mjConfirm, "confirm", false, "confirm clearing all entries")
	mj.AddCommand(mjSave, mjList, mjForget, mjClear)

	// ---- mem (semantic) ----
	var memType string
	var confidence float64
	var limit int
	mem := &cobra.Command{Use: "mem", Aliases: []string{"memory"}, Short: "Semantic memory with confidence decay", GroupID: "memory"}
	mem.PersistentFlags().StringVar(&profile, "profile", "", "profile")

	memAdd := &cobra.Command{Use: "add CONTENT", Short: "Add a semantic memory", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			id, err := memory.Add(db.DB, app.profile(profile), args[0], memType, confidence)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			// Python: json.dumps({"id": mem_id, "profile": profile}) — the profile
			// disambiguates the id, which is only unique within one.
			outJSON(map[string]any{"id": id, "profile": app.profile(profile)}, "Memory saved: "+id)
			return nil
		}}
	memAdd.Flags().StringVar(&memType, "type", "fact", "memory type")
	memAdd.Flags().Float64Var(&confidence, "confidence", 1.0, "confidence (0,1]")

	var searchType, searchMode string
	var searchAlpha float64
	var searchVerbose bool
	memSearch := &cobra.Command{Use: "search QUERY", Short: "Search memories (hybrid RRF over FTS/BM25 + vector)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkMemLimit(limit); err != nil {
				return err
			}
			// Reject a bad --mode/--alpha as a USAGE error (exit 2), not a runtime
			// failure, since they are bad flag values.
			mode, err := memory.ValidateHybridMode(searchMode)
			if err != nil {
				return jsonErrorMaybe(usageErr{err})
			}
			if searchAlpha < 0 || searchAlpha > 1 {
				return jsonErrorMaybe(usageErrorf("--alpha must be in [0,1], got %g", searchAlpha))
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			prof := app.profile(profile)
			// memory.Search has always accepted a memory_type filter (as Python's
			// search_memories does); the CLI just never exposed it, so
			// `mem search q --type fact` died with "unknown flag: --type", exit 2.
			if mode == memory.ModeFTS {
				// Pure-sparse keeps the historical output shape verbatim.
				res, serr := memory.Search(db.DB, prof, args[0], limit, searchType)
				if serr != nil {
					return jsonErrorMaybe(serr)
				}
				if searchVerbose && !flagJSON {
					fmt.Printf("mode: %s\n", memory.ModeFTS)
				}
				if searchVerbose && flagJSON {
					if res == nil {
						res = []memory.Mem{}
					}
					return emitJSON(map[string]any{"mode": memory.ModeFTS, "results": res})
				}
				printMems(res, args[0])
				return nil
			}
			// Embeddings cost money: the backend is only contacted when the user
			// configured one. Unconfigured → nil interface → honest fts degradation.
			hits, used, err := memory.SearchHybrid(cmd.Context(), db.DB, memory.EmbedderFromEnvIface(), prof, args[0],
				memory.HybridOptions{Mode: mode, Alpha: searchAlpha, Limit: limit, MemType: searchType})
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if hits == nil {
				hits = []memory.HybridHit{}
			}
			// Only nag when the user explicitly asked for a mode we could not give
			// them; the default must not print a warning on every keyless search.
			if used != mode && cmd.Flags().Changed("mode") {
				fmt.Fprintf(os.Stderr,
					"  note: %s retrieval unavailable (no embedding backend configured, or no memory carries a vector for the active model) — answered with %s\n",
					mode, used)
			}
			if flagJSON {
				if searchVerbose {
					return emitJSON(map[string]any{"mode": used, "results": hits})
				}
				return emitJSON(hits)
			}
			if len(hits) == 0 {
				fmt.Printf("No memories found for: %q (mode: %s)\n", args[0], used)
				return nil
			}
			if searchVerbose {
				fmt.Printf("mode: %s  (alpha=%g, rrf k=60)\n", used, searchAlpha)
			}
			for _, m := range hits {
				if searchVerbose {
					fmt.Printf("[%s] (%s conf=%.2f hybrid=%.6f dense=%.4f sparse=%.6f) %s\n",
						short(m.ID), m.MemoryType, m.Confidence, m.HybridScore, m.DenseScore, m.SparseScore, truncate(m.Content, 80))
					continue
				}
				fmt.Printf("[%s] (%s conf=%.2f) %s\n", short(m.ID), m.MemoryType, m.Confidence, truncate(m.Content, 80))
			}
			return nil
		}}
	memSearch.Flags().IntVar(&limit, "limit", 10, "max results")
	memSearch.Flags().StringVar(&searchType, "type", "", "filter type")
	memSearch.Flags().StringVar(&searchMode, "mode", memory.ModeHybrid, "retrieval mode: hybrid|fts|dense (bm25=fts, vector=dense)")
	memSearch.Flags().Float64Var(&searchAlpha, "alpha", 0.5, "hybrid fusion weight: 0=pure BM25, 1=pure vector")
	memSearch.Flags().BoolVar(&searchVerbose, "verbose", false, "show the retrieval mode and per-path scores")

	memList := &cobra.Command{Use: "list", Short: "List memories",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkMemLimit(limit); err != nil {
				return err
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			res, err := memory.List(db.DB, app.profile(profile), memType, limit)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			printMems(res, "")
			return nil
		}}
	memList.Flags().IntVar(&limit, "limit", 20, "max results")
	memList.Flags().StringVar(&memType, "type", "", "filter type")

	memForget := &cobra.Command{Use: "forget ID", Short: "Forget a memory", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			ok, err := memory.Forget(db.DB, app.profile(profile), args[0])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if !ok {
				// A miss is a failure, so it must NOT print the success-shaped
				// {"deleted": false} on stdout: a --json consumer reading stdout and
				// ignoring the exit code would treat it as a completed delete.
				// Message matches Python cmd_memory_semantic's forget branch.
				return jsonErrorMaybe(fmt.Errorf("Memory '%s' not found for profile '%s'", args[0], app.profile(profile)))
			}
			outJSON(map[string]any{"deleted": true, "id": args[0], "profile": app.profile(profile)},
				"forgotten: "+args[0])
			return nil
		}}
	memStats := &cobra.Command{Use: "stats", Short: "Memory stats",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			s, err := memory.Stats(db.DB, app.profile(profile))
			if err != nil {
				return err
			}
			if flagJSON {
				// Match Python semantic_memory.memory_stats shape (#540):
				// {"profile":..., "total":..., "by_type": {type: {count, avg_confidence_base}}}
				byType := map[string]any{}
				total := 0
				for t, v := range s {
					n := 0
					if c, ok := v["count"].(int); ok {
						n = c
					} else if c, ok := v["count"].(int64); ok {
						n = int(c)
					}
					total += n
					base := 0.0
					if b, ok := v["avg_confidence_base"].(float64); ok {
						base = math.Round(b*10000) / 10000
					}
					byType[t] = map[string]any{"count": n, "avg_confidence_base": base}
				}
				return emitJSON(map[string]any{"profile": app.profile(profile), "total": total, "by_type": byType})
			}
			// Human default (parity, issue #529): print a per-type summary instead
			// of raw JSON. Sort types for stable output.
			if len(s) == 0 {
				fmt.Println("No memories stored.")
				return nil
			}
			types := make([]string, 0, len(s))
			for t := range s {
				types = append(types, t)
			}
			sort.Strings(types)
			total := 0
			for _, t := range types {
				n := 0
				if v, ok := s[t]["count"].(int); ok {
					n = v
				} else if v, ok := s[t]["count"].(int64); ok {
					n = int(v)
				}
				total += n
				fmt.Printf("  %-12s %d\n", t, n)
			}
			fmt.Printf("Total: %d memories across %d type(s)\n", total, len(types))
			return nil
		}}
	mem.AddCommand(memAdd, memSearch, memList, memForget, memStats)

	root.AddCommand(mj, mem)
}

func printMems(res []memory.Mem, q string) {
	if flagJSON {
		if res == nil {
			res = []memory.Mem{}
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return
	}
	if len(res) == 0 {
		if q != "" {
			fmt.Printf("No memories found for: %q\n", q)
		} else {
			fmt.Println("No memories.")
		}
		return
	}
	for _, m := range res {
		fmt.Printf("[%s] (%s conf=%.2f) %s\n", short(m.ID), m.MemoryType, m.Confidence, truncate(m.Content, 80))
	}
}

func outJSON(obj any, text string) {
	if flagJSON {
		b, _ := json.Marshal(obj)
		fmt.Println(string(b))
	} else {
		fmt.Println(text)
	}
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
