package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/store"
)

func registerRouteFallback(root *cobra.Command, app *App) {
	var profile, primary, fallback, condition string
	var priority int
	rf := &cobra.Command{Use: "route-fallback", Short: "Manage model fallback chains", GroupID: "routing"}
	rf.PersistentFlags().StringVar(&profile, "profile", "", "profile")

	add := &cobra.Command{Use: "add", Short: "Add a fallback chain",
		RunE: func(cmd *cobra.Command, args []string) error {
			if primary == "" || fallback == "" {
				return fmt.Errorf("--primary and --fallback required")
			}
			if primary == fallback {
				return fmt.Errorf("primary and fallback must be different models")
			}
			if priority < 0 {
				return fmt.Errorf("priority must be >= 0")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			pr := app.profile(profile)
			// dedupe
			var dup int
			db.QueryRow(`SELECT COUNT(*) FROM route_fallbacks WHERE profile=? AND primary_model=? AND fallback_model=? AND condition=?`, pr, primary, fallback, condition).Scan(&dup)
			if dup > 0 {
				return fmt.Errorf("identical fallback already exists")
			}
			// cycle check: does fallback already reach primary?
			if reaches(db, pr, fallback, primary, condition) {
				return fmt.Errorf("would create a cycle (%s -> ... -> %s)", primary, fallback)
			}
			id := uuid.NewString()[:12]
			_, err = db.Exec(`INSERT INTO route_fallbacks(id,profile,primary_model,fallback_model,condition,priority,enabled,created_at)
				VALUES(?,?,?,?,?,?,1,?)`, id, pr, primary, fallback, condition, priority, time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			if flagJSON {
				// Compact JSON to match Python cmd_route_fallback add (#534).
				b, _ := json.Marshal(map[string]any{"id": id, "profile": pr, "primary": primary, "fallback": fallback})
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("Fallback added: %s -> %s (condition: %s)\n", primary, fallback, condition)
			return nil
		}}
	add.Flags().StringVar(&primary, "primary", "", "primary model")
	add.Flags().StringVar(&fallback, "fallback", "", "fallback model")
	add.Flags().StringVar(&condition, "condition", "context_overflow", "condition")
	add.Flags().IntVar(&priority, "priority", 1, "priority")

	list := &cobra.Command{Use: "list", Short: "List fallback chains",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id,primary_model,fallback_model,condition,priority,enabled FROM route_fallbacks WHERE profile=? ORDER BY primary_model,priority`, app.profile(profile))
			if err != nil {
				return err
			}
			defer rows.Close()
			type fbRow struct {
				ID        string `json:"id"`
				Primary   string `json:"primary"`
				Fallback  string `json:"fallback"`
				Condition string `json:"condition"`
				Priority  int    `json:"priority"`
				Enabled   bool   `json:"enabled"`
			}
			out := []fbRow{}
			for rows.Next() {
				var r fbRow
				var en int
				if err := rows.Scan(&r.ID, &r.Primary, &r.Fallback, &r.Condition, &r.Priority, &en); err != nil {
					return err
				}
				r.Enabled = en != 0
				out = append(out, r)
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No fallback chains configured.")
				return nil
			}
			for _, r := range out {
				fmt.Printf("%s  %s -> %s  [%s p%d]\n", r.ID, r.Primary, r.Fallback, r.Condition, r.Priority)
			}
			return nil
		}}
	remove := &cobra.Command{Use: "remove FALLBACK_ID", Short: "Remove a fallback chain", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			pr := app.profile(profile)
			r, err := db.Exec(`DELETE FROM route_fallbacks WHERE id=? AND profile=?`, args[0], pr)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n == 0 {
				return fmt.Errorf("Fallback '%s' not found for profile '%s'", args[0], pr)
			}
			fmt.Printf("removed: %s\n", args[0])
			return nil
		}}
	resolve := &cobra.Command{Use: "resolve", Short: "Resolve the fallback for a primary",
		RunE: func(cmd *cobra.Command, args []string) error {
			if primary == "" {
				return fmt.Errorf("--primary required")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var fm string
			err = db.QueryRow(`SELECT fallback_model FROM route_fallbacks WHERE profile=? AND primary_model=? AND condition=? AND enabled=1 ORDER BY priority LIMIT 1`,
				app.profile(profile), primary, condition).Scan(&fm)
			if err != nil {
				// A valid query that simply has no fallback configured is not an
				// error (parity with Python cmd_route_fallback resolve, #542) — exit 0.
				outJSON(map[string]any{"primary": primary, "fallback": nil, "condition": condition},
					fmt.Sprintf("No fallback configured for %q on condition=%q", primary, condition))
				return nil
			}
			outJSON(map[string]any{"primary": primary, "fallback": fm, "condition": condition},
				fmt.Sprintf("Fallback: %s -> %s (condition: %s)", primary, fm, condition))
			return nil
		}}
	resolve.Flags().StringVar(&primary, "primary", "", "primary model")
	resolve.Flags().StringVar(&condition, "condition", "context_overflow", "condition")
	rf.AddCommand(add, list, resolve, remove)
	root.AddCommand(rf)
}

// reaches does a BFS over the fallback edges to detect if `from` can already
// reach `target` under the same condition (i.e. adding target->... would cycle).
func reaches(db *store.DB, profile, from, target, cond string) bool {
	seen := map[string]bool{}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == target {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		rows, err := db.Query(`SELECT fallback_model FROM route_fallbacks WHERE profile=? AND primary_model=? AND condition=?`, profile, cur, cond)
		if err != nil {
			return false
		}
		for rows.Next() {
			var fm string
			if err := rows.Scan(&fm); err != nil {
				rows.Close()
				return false
			}
			queue = append(queue, fm)
		}
		rows.Close()
	}
	return false
}
