package cli

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// registerAnnotate wires the human annotation queue: annotate add/next/label/skip/stats/export.
// Port of src/tag/cmd/prd_clusters.py:cmd_annotate + annotation_queue.py.
// `add` is a usability extension — the Python CLI only enqueues via eval import.
func registerAnnotate(root *cobra.Command, app *App) {
	a := &cobra.Command{Use: "annotate", Short: "Human annotation queue", GroupID: "obs"}

	var question, sourceType, sourceID string
	var priority int
	add := &cobra.Command{Use: "add <content>", Short: "Enqueue an annotation task", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if question == "" {
				return fmt.Errorf("--question is required")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			id := uuid.NewString()
			_, err = db.Exec(`INSERT INTO annotation_tasks(id,source_type,source_id,content,question,label_schema,status,created_at,priority,tags)
				VALUES(?,?,?,?,?,'{}','pending',?,?,'[]')`,
				id, strOr(sourceType, "manual"), strOr(sourceID, id), args[0], question, time.Now().UTC().Format(time.RFC3339), priority)
			if err != nil {
				return err
			}
			fmt.Printf("Enqueued annotation task %s\n", id)
			return nil
		}}
	add.Flags().StringVar(&question, "question", "", "what the annotator should answer")
	add.Flags().StringVar(&sourceType, "source-type", "manual", "eval_case|span|run_output|manual")
	add.Flags().StringVar(&sourceID, "source-id", "", "source identifier")
	add.Flags().IntVar(&priority, "priority", 0, "higher = more urgent")

	var assignee string
	var batch int
	next := &cobra.Command{Use: "next", Short: "Claim the next pending task(s)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			// Atomic claim: single UPDATE...RETURNING under the write lock avoids a
			// SELECT-then-UPDATE lost-update window between concurrent workers.
			var asg any
			if assignee != "" {
				asg = assignee
			}
			rows, err := db.Query(`UPDATE annotation_tasks SET status='in_progress', assigned_to=?
				WHERE id IN (SELECT id FROM annotation_tasks WHERE status='pending' ORDER BY priority DESC, created_at ASC LIMIT ?)
				RETURNING id, source_type, source_id, question, content`, asg, batch)
			if err != nil {
				return err
			}
			defer rows.Close()
			type claimed struct {
				ID         string `json:"id"`
				SourceType string `json:"source_type"`
				SourceID   string `json:"source_id"`
				Question   string `json:"question"`
				Content    string `json:"content"`
			}
			var tasks []claimed
			for rows.Next() {
				var t claimed
				if err := rows.Scan(&t.ID, &t.SourceType, &t.SourceID, &t.Question, &t.Content); err != nil {
					return err
				}
				tasks = append(tasks, t)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				if tasks == nil {
					tasks = []claimed{}
				}
				return emitJSON(tasks)
			}
			if len(tasks) == 0 {
				fmt.Println("Queue is empty")
				return nil
			}
			for _, t := range tasks {
				content := t.Content
				if len(content) > 200 {
					content = content[:200]
				}
				fmt.Printf("[%s] %s:%s\n  %s\n  Content: %s\n", t.ID, t.SourceType, t.SourceID, t.Question, content)
			}
			return nil
		}}
	next.Flags().StringVar(&assignee, "assignee", "", "assign claimed tasks to")
	next.Flags().IntVar(&batch, "batch", 1, "how many to claim")

	var notes string
	label := &cobra.Command{Use: "label <task-id> <label>", Short: "Submit a label, marking the task completed", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var nt any
			if notes != "" {
				nt = notes
			}
			res, err := db.Exec(`UPDATE annotation_tasks SET status='completed', label=?, notes=?, completed_at=? WHERE id=?`,
				args[1], nt, time.Now().UTC().Format(time.RFC3339), args[0])
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				fmt.Println("Task not found")
				return fmt.Errorf("task not found: %q", args[0])
			}
			fmt.Println("Labeled")
			return nil
		}}
	label.Flags().StringVar(&notes, "notes", "", "optional annotator notes")

	skip := &cobra.Command{Use: "skip <task-id>", Short: "Mark a task skipped", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			res, err := db.Exec(`UPDATE annotation_tasks SET status='skipped' WHERE id=?`, args[0])
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("task not found: %q", args[0])
			}
			fmt.Println("Skipped")
			return nil
		}}

	stats := &cobra.Command{Use: "stats", Short: "Show queue statistics", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			counts := map[string]int{"pending": 0, "in_progress": 0, "completed": 0, "skipped": 0}
			rows, err := db.Query(`SELECT status, COUNT(*) FROM annotation_tasks GROUP BY status`)
			if err != nil {
				return err
			}
			for rows.Next() {
				var s string
				var c int
				if err := rows.Scan(&s, &c); err != nil {
					rows.Close()
					return err
				}
				if _, ok := counts[s]; ok {
					counts[s] = c
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			var avg *float64
			var v float64
			if err := db.QueryRow(`SELECT AVG((julianday(completed_at)-julianday(created_at))*24.0)
				FROM annotation_tasks WHERE status='completed' AND completed_at IS NOT NULL AND created_at IS NOT NULL`).Scan(&v); err == nil {
				avg = &v
			}
			total := counts["pending"] + counts["in_progress"] + counts["completed"] + counts["skipped"]
			return emitJSON(map[string]any{
				"pending": counts["pending"], "in_progress": counts["in_progress"],
				"completed": counts["completed"], "skipped": counts["skipped"],
				"total": total, "avg_latency_hours": avg,
			})
		}}

	var format, out string
	export := &cobra.Command{Use: "export", Short: "Export completed tasks (jsonl|csv)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "jsonl" && format != "csv" {
				return fmt.Errorf("unsupported export format: %q (supported: jsonl, csv)", format)
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id,source_type,source_id,content,question,label_schema,label,notes,assigned_to,created_at,completed_at,priority,tags
				FROM annotation_tasks WHERE status='completed' ORDER BY completed_at ASC`)
			if err != nil {
				return err
			}
			defer rows.Close()
			type rec struct {
				ID, SourceType, SourceID, Content, Question, LabelSchema, Label, Notes, AssignedTo, CreatedAt, CompletedAt string
				Priority                                                                                                   int
				Tags                                                                                                       string
			}
			var recs []rec
			for rows.Next() {
				var r rec
				var label, notes, assigned, completed *string
				if err := rows.Scan(&r.ID, &r.SourceType, &r.SourceID, &r.Content, &r.Question, &r.LabelSchema, &label, &notes, &assigned, &r.CreatedAt, &completed, &r.Priority, &r.Tags); err != nil {
					return err
				}
				r.Label, r.Notes, r.AssignedTo, r.CompletedAt = deref(label), deref(notes), deref(assigned), deref(completed)
				recs = append(recs, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			var data string
			if format == "csv" {
				var sb strings.Builder
				w := csv.NewWriter(&sb)
				w.Write([]string{"id", "source_type", "source_id", "content", "question", "label_schema", "label", "notes", "assigned_to", "created_at", "completed_at", "priority", "tags"})
				for _, r := range recs {
					w.Write([]string{r.ID, r.SourceType, r.SourceID, r.Content, r.Question, r.LabelSchema, r.Label, r.Notes, r.AssignedTo, r.CreatedAt, r.CompletedAt, fmt.Sprint(r.Priority), r.Tags})
				}
				w.Flush()
				data = sb.String()
			} else {
				var lines []string
				for _, r := range recs {
					var schema any
					if json.Unmarshal([]byte(strOr(r.LabelSchema, "{}")), &schema) != nil {
						schema = r.LabelSchema
					}
					b, _ := json.Marshal(map[string]any{
						"id": r.ID, "source_type": r.SourceType, "source_id": r.SourceID,
						"content": r.Content, "question": r.Question, "label_schema": schema,
						"label": r.Label, "notes": r.Notes, "assigned_to": r.AssignedTo, "created_at": r.CreatedAt,
						"completed_at": r.CompletedAt, "priority": r.Priority, "tags": r.Tags,
					})
					lines = append(lines, string(b))
				}
				data = strings.Join(lines, "\n")
			}
			if out != "" {
				if err := os.WriteFile(out, []byte(data), 0o644); err != nil {
					return err
				}
				fmt.Printf("Exported to %s\n", out)
				return nil
			}
			fmt.Println(data)
			return nil
		}}
	export.Flags().StringVar(&format, "format", "jsonl", "jsonl|csv")
	export.Flags().StringVar(&out, "out", "", "write to file instead of stdout")

	a.AddCommand(add, next, label, skip, stats, export)
	root.AddCommand(a)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
