package cli

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// registerEval wires eval suite results: eval list/show.
// Port of src/tag/cmd/marketplace.py:cmd_eval (list/show read paths). The
// `run` subcommand executes a suite through the runtime (Track B).
func registerEval(root *cobra.Command, app *App) {
	e := &cobra.Command{Use: "eval", Short: "Eval suite runs (list/show results)", GroupID: "obs"}

	list := &cobra.Command{Use: "list", Short: "List eval runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id, suite_name, profile, status, pass_count, fail_count
				FROM eval_runs ORDER BY created_at DESC LIMIT 20`)
			if err != nil {
				return err
			}
			defer rows.Close()
			type run struct {
				ID        string `json:"id"`
				SuiteName string `json:"suite_name"`
				Profile   string `json:"profile"`
				Status    string `json:"status"`
				PassCount int    `json:"pass_count"`
				FailCount int    `json:"fail_count"`
			}
			var out []run
			for rows.Next() {
				var r run
				if err := rows.Scan(&r.ID, &r.SuiteName, &r.Profile, &r.Status, &r.PassCount, &r.FailCount); err != nil {
					return err
				}
				out = append(out, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				if out == nil {
					out = []run{}
				}
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No eval runs yet.")
				return nil
			}
			fmt.Printf("  %-18s %-24s %-14s %-10s %-6s %-6s\n", "ID", "SUITE", "PROFILE", "STATUS", "PASS", "FAIL")
			fmt.Println("  " + strings.Repeat("-", 80))
			for _, r := range out {
				fmt.Printf("  %-18s %-24s %-14s %-10s %-6d %-6d\n", r.ID, truncate(r.SuiteName, 24), r.Profile, r.Status, r.PassCount, r.FailCount)
			}
			return nil
		}}

	show := &cobra.Command{Use: "show <run-id>", Short: "Show eval run detail", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var id, suitePath, profile, suiteName, status, createdAt string
			var pass, fail, total int
			var completed sql.NullString
			err = db.QueryRow(`SELECT id, suite_path, profile, suite_name, status, pass_count, fail_count, total_count, created_at, completed_at
				FROM eval_runs WHERE id=?`, args[0]).Scan(&id, &suitePath, &profile, &suiteName, &status, &pass, &fail, &total, &createdAt, &completed)
			if err == sql.ErrNoRows {
				return fmt.Errorf("eval run %q not found", args[0])
			}
			if err != nil {
				return err
			}
			crows, err := db.Query(`SELECT case_id, passed, score, COALESCE(failure_reason,''), created_at FROM eval_cases WHERE eval_run_id=?`, id)
			if err != nil {
				return err
			}
			defer crows.Close()
			type ecase struct {
				CaseID        string  `json:"case_id"`
				Passed        bool    `json:"passed"`
				Score         float64 `json:"score"`
				FailureReason string  `json:"failure_reason"`
				CreatedAt     string  `json:"created_at"`
			}
			var cases []ecase
			for crows.Next() {
				var c ecase
				var p int
				if err := crows.Scan(&c.CaseID, &p, &c.Score, &c.FailureReason, &c.CreatedAt); err != nil {
					return err
				}
				c.Passed = p != 0
				cases = append(cases, c)
			}
			if err := crows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{
					"id": id, "suite_path": suitePath, "profile": profile, "suite_name": suiteName,
					"status": status, "pass_count": pass, "fail_count": fail, "total_count": total,
					"created_at": createdAt, "completed_at": completed.String, "cases": cases,
				})
			}
			fmt.Printf("Eval run: %s\n", id)
			fmt.Printf("  Suite: %s  Profile: %s\n", suiteName, profile)
			fmt.Printf("  Status: %s  %d/%d passed\n", status, pass, total)
			for _, c := range cases {
				icon := "✓"
				if !c.Passed {
					icon = "✗"
				}
				reason := ""
				if c.FailureReason != "" {
					reason = "  (" + c.FailureReason + ")"
				}
				fmt.Printf("  [%s] %s  score=%.2f%s\n", icon, c.CaseID, c.Score, reason)
			}
			return nil
		}}

	e.AddCommand(list, show)
	root.AddCommand(e)
}
