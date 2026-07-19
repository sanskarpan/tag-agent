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
				return err
			}
			outJSON(map[string]any{"id": id}, "Memory saved: "+id)
			return nil
		}}
	memAdd.Flags().StringVar(&memType, "type", "fact", "memory type")
	memAdd.Flags().Float64Var(&confidence, "confidence", 1.0, "confidence (0,1]")

	memSearch := &cobra.Command{Use: "search QUERY", Short: "Search memories (FTS/BM25)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			res, err := memory.Search(db.DB, app.profile(profile), args[0], limit, "")
			if err != nil {
				return err
			}
			printMems(res, args[0])
			return nil
		}}
	memSearch.Flags().IntVar(&limit, "limit", 10, "max results")

	memList := &cobra.Command{Use: "list", Short: "List memories",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			res, err := memory.List(db.DB, app.profile(profile), memType, limit)
			if err != nil {
				return err
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
				return err
			}
			outJSON(map[string]any{"deleted": ok}, ternary(ok, "forgotten", "not found"))
			if !ok {
				return fmt.Errorf("memory not found: %s", args[0])
			}
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
