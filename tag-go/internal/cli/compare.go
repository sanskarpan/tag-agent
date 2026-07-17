package cli

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// registerCompare wires multi-model benchmark comparisons: compare list/show.
// Port of src/tag/cmd/workflow_mgmt.py:cmd_compare (list/show read paths). The
// `run` subcommand executes live model calls and belongs to the Track-B runtime.
func registerCompare(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "compare", Short: "Multi-model benchmark comparisons", GroupID: "obs"}

	var limit int
	list := &cobra.Command{Use: "list", Short: "List saved comparisons", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				return fmt.Errorf("--limit must be positive")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id, suite_path, created_at, status, models FROM benchmark_comparisons ORDER BY created_at DESC LIMIT ?`, limit)
			if err != nil {
				return err
			}
			defer rows.Close()
			type cmp struct {
				ID        string `json:"id"`
				SuitePath string `json:"suite_path"`
				CreatedAt string `json:"created_at"`
				Status    string `json:"status"`
				Models    string `json:"models"`
			}
			var out []cmp
			for rows.Next() {
				var r cmp
				if err := rows.Scan(&r.ID, &r.SuitePath, &r.CreatedAt, &r.Status, &r.Models); err != nil {
					return err
				}
				out = append(out, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				if out == nil {
					out = []cmp{}
				}
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No benchmark comparisons found.")
				return nil
			}
			fmt.Printf("%-14s %-40s %-12s %s\n", "ID", "Suite", "Status", "Created")
			fmt.Println(strings.Repeat("-", 90))
			for _, r := range out {
				fmt.Printf("  %-12s %-40s %-12s %s\n", r.ID, r.SuitePath, r.Status, r.CreatedAt)
			}
			return nil
		}}
	list.Flags().IntVar(&limit, "limit", 20, "max comparisons")

	show := &cobra.Command{Use: "show <id>", Short: "Show comparison results", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var id, suite, created, status, models string
			err = db.QueryRow(`SELECT id, suite_path, created_at, status, models FROM benchmark_comparisons WHERE id=?`, args[0]).
				Scan(&id, &suite, &created, &status, &models)
			if err == sql.ErrNoRows {
				return fmt.Errorf("comparison %q not found", args[0])
			}
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT model_id, case_id, quality_score, latency_ms FROM benchmark_results WHERE comparison_id=? ORDER BY case_id, quality_score DESC`, id)
			if err != nil {
				return err
			}
			defer rows.Close()
			type res struct {
				Model, Case string
				Score       sql.NullFloat64
				Latency     sql.NullInt64
			}
			var results []res
			for rows.Next() {
				var r res
				if err := rows.Scan(&r.Model, &r.Case, &r.Score, &r.Latency); err != nil {
					return err
				}
				results = append(results, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				jr := make([]map[string]any, 0, len(results))
				for _, r := range results {
					jr = append(jr, map[string]any{"model_id": r.Model, "case_id": r.Case,
						"quality_score": nullF(r.Score), "latency_ms": nullI(r.Latency)})
				}
				return emitJSON(map[string]any{"id": id, "suite_path": suite, "created_at": created,
					"status": status, "models": models, "results": jr})
			}
			fmt.Printf("Comparison: %s (id=%s)\n", suite, id)
			fmt.Printf("Status:     %s  |  Created: %s\n", status, created)
			fmt.Printf("Models:     %s\n", models)
			fmt.Printf("\n%-40s %-25s %6s %10s\n", "Model", "Case", "Score", "Latency")
			fmt.Println(strings.Repeat("-", 90))
			for _, r := range results {
				score := "-"
				if r.Score.Valid {
					score = fmt.Sprintf("%.2f", r.Score.Float64)
				}
				lat := "n/a"
				if r.Latency.Valid {
					lat = fmt.Sprintf("%dms", r.Latency.Int64)
				}
				fmt.Printf("  %-38s %-25s %6s %10s\n", r.Model, r.Case, score, lat)
			}
			return nil
		}}

	c.AddCommand(list, show)
	root.AddCommand(c)
}

func nullF(n sql.NullFloat64) any {
	if n.Valid {
		return n.Float64
	}
	return nil
}
func nullI(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}
