package cli

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// registerSwarm wires swarm run inspection: swarm list/status/results.
// Port of src/tag/cmd/swarm.py read paths (swarm_runs / swarm_tasks). The `run`
// (agent fan-out) and `abort` (signal PIDs) paths belong to the Track-B runtime.
func registerSwarm(root *cobra.Command, app *App) {
	s := &cobra.Command{Use: "swarm", Short: "Inspect multi-agent swarm runs", GroupID: "orch"}

	list := &cobra.Command{Use: "list", Short: "List swarm runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT swarm_id, goal, status, task_count, total_cost_usd FROM swarm_runs ORDER BY created_at DESC LIMIT 20`)
			if err != nil {
				return err
			}
			defer rows.Close()
			type run struct {
				SwarmID string  `json:"swarm_id"`
				Goal    string  `json:"goal"`
				Status  string  `json:"status"`
				Tasks   int     `json:"task_count"`
				Cost    float64 `json:"total_cost_usd"`
			}
			out := []run{}
			for rows.Next() {
				var r run
				var cost sql.NullFloat64
				if err := rows.Scan(&r.SwarmID, &r.Goal, &r.Status, &r.Tasks, &cost); err != nil {
					return err
				}
				r.Cost = cost.Float64
				out = append(out, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No swarm runs found.")
				return nil
			}
			fmt.Printf("%-14s %-12s %5s %8s  Goal\n", "Swarm ID", "Status", "Tasks", "Cost")
			fmt.Println(strings.Repeat("-", 80))
			for _, r := range out {
				fmt.Printf("%-14s %-12s %5d $%7.4f  %s\n", r.SwarmID, r.Status, r.Tasks, r.Cost, truncate(r.Goal, 45))
			}
			return nil
		}}

	status := &cobra.Command{Use: "status <swarm-id>", Short: "Show per-task status for a swarm", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var sid, goal, st string
			var tasks int
			var cost sql.NullFloat64
			err = db.QueryRow(`SELECT swarm_id, goal, status, task_count, total_cost_usd FROM swarm_runs WHERE swarm_id=?`, args[0]).
				Scan(&sid, &goal, &st, &tasks, &cost)
			if err == sql.ErrNoRows {
				return fmt.Errorf("swarm %q not found", args[0])
			}
			if err != nil {
				return err
			}
			trows, err := db.Query(`SELECT task_id, COALESCE(profile,''), COALESCE(status,''), cost_usd, COALESCE(error_message,'') FROM swarm_tasks WHERE swarm_id=? ORDER BY id`, sid)
			if err != nil {
				return err
			}
			defer trows.Close()
			type task struct {
				TaskID  string  `json:"task_id"`
				Profile string  `json:"profile"`
				Status  string  `json:"status"`
				Cost    float64 `json:"cost_usd"`
				Error   string  `json:"error_message"`
			}
			var trs []task
			for trows.Next() {
				var t task
				var tcost sql.NullFloat64
				if err := trows.Scan(&t.TaskID, &t.Profile, &t.Status, &tcost, &t.Error); err != nil {
					return err
				}
				t.Cost = tcost.Float64
				trs = append(trs, t)
			}
			if err := trows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{"run": map[string]any{"swarm_id": sid, "goal": goal, "status": st, "task_count": tasks, "total_cost_usd": cost.Float64}, "tasks": trs})
			}
			fmt.Printf("Swarm:  %s  (%s)  tasks=%d  cost=$%.4f\n", sid, st, tasks, cost.Float64)
			fmt.Printf("Goal:   %s\n\n", goal)
			fmt.Printf("%-22s %-18s %-14s Cost\n", "Task ID", "Profile", "Status")
			fmt.Println(strings.Repeat("-", 70))
			for _, t := range trs {
				errNote := ""
				if t.Error != "" {
					errNote = "  " + t.Error
				}
				fmt.Printf("%-22s %-18s %-14s $%.4f%s\n", t.TaskID, t.Profile, t.Status, t.Cost, errNote)
			}
			return nil
		}}

	results := &cobra.Command{Use: "results <swarm-id>", Short: "Show results and final output for a swarm", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var sid, goal, st string
			var finalOut sql.NullString
			var cost sql.NullFloat64
			err = db.QueryRow(`SELECT swarm_id, goal, status, final_output, total_cost_usd FROM swarm_runs WHERE swarm_id=?`, args[0]).
				Scan(&sid, &goal, &st, &finalOut, &cost)
			if err == sql.ErrNoRows {
				return fmt.Errorf("swarm %q not found", args[0])
			}
			if err != nil {
				return err
			}
			trows, err := db.Query(`SELECT task_id, COALESCE(status,''), cost_usd, tokens_prompt, tokens_completion FROM swarm_tasks WHERE swarm_id=? ORDER BY id`, sid)
			if err != nil {
				return err
			}
			defer trows.Close()
			type task struct {
				TaskID string  `json:"task_id"`
				Status string  `json:"status"`
				Cost   float64 `json:"cost_usd"`
				Tokens int64   `json:"tokens"`
			}
			var trs []task
			for trows.Next() {
				var t task
				var tcost sql.NullFloat64
				var tp, tc sql.NullInt64
				if err := trows.Scan(&t.TaskID, &t.Status, &tcost, &tp, &tc); err != nil {
					return err
				}
				t.Cost = tcost.Float64
				t.Tokens = tp.Int64 + tc.Int64
				trs = append(trs, t)
			}
			if err := trows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{"swarm_id": sid, "goal": goal, "status": st, "final_output": finalOut.String, "total_cost_usd": cost.Float64, "tasks": trs})
			}
			fmt.Printf("Swarm:  %s  (%s)  total_cost=$%.4f\n", sid, st, cost.Float64)
			fmt.Printf("Goal:   %s\n\n", goal)
			fmt.Printf("%-22s %-14s %8s %8s\n", "Task ID", "Status", "Tokens", "Cost")
			fmt.Println(strings.Repeat("-", 60))
			for _, t := range trs {
				fmt.Printf("%-22s %-14s %8d $%7.4f\n", t.TaskID, t.Status, t.Tokens, t.Cost)
			}
			if finalOut.String != "" {
				fmt.Printf("\n-- Final Output --\n%s\n", finalOut.String)
			}
			return nil
		}}

	s.AddCommand(list, status, results)
	root.AddCommand(s)
}
