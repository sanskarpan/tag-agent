package cli

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// registerRuns wires `tag runs list` and `tag runs show <id>`.
// Port of src/tag/cmd/routing.py:cmd_runs (read-only view of the `runs`
// table). The Go version surfaces token/duration columns in addition to the
// Python listing.
func registerRuns(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "runs", Short: "Inspect recorded runs", GroupID: "obs"}

	var limit int
	var profile string
	list := &cobra.Command{Use: "list", Short: "List recent runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			q := `SELECT id, prompt, status, COALESCE(model_id,''), prompt_tokens, completion_tokens, duration_ms, created_at FROM runs`
			var qargs []any
			if profile != "" {
				q += ` WHERE master_profile=?`
				qargs = append(qargs, profile)
			}
			q += ` ORDER BY created_at DESC LIMIT ?`
			qargs = append(qargs, limit)
			rows, err := db.Query(q, qargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			type row struct {
				ID         string `json:"id"`
				Prompt     string `json:"prompt"`
				Status     string `json:"status"`
				ModelID    string `json:"model_id"`
				PromptTok  int    `json:"prompt_tokens"`
				CompTok    int    `json:"completion_tokens"`
				DurationMs *int64 `json:"duration_ms"`
				CreatedAt  string `json:"created_at"`
			}
			// Non-nil so an empty result marshals to [] not null (Python parity).
			out := []row{}
			for rows.Next() {
				var r row
				var dur sql.NullInt64
				if err := rows.Scan(&r.ID, &r.Prompt, &r.Status, &r.ModelID, &r.PromptTok, &r.CompTok, &dur, &r.CreatedAt); err != nil {
					return err
				}
				if dur.Valid {
					v := dur.Int64
					r.DurationMs = &v
				}
				out = append(out, r)
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No runs found.")
				return nil
			}
			fmt.Printf("%-14s %-40s %-10s %-24s %8s %8s %10s %s\n",
				"ID", "Prompt", "Status", "Model", "Prompt", "Compl", "Duration", "Created")
			fmt.Println(strings.Repeat("-", 130))
			for _, r := range out {
				dur := "-"
				if r.DurationMs != nil {
					dur = fmt.Sprintf("%dms", *r.DurationMs)
				}
				fmt.Printf("%-14s %-40s %-10s %-24s %8d %8d %10s %s\n",
					truncate(r.ID, 14), truncate(oneLine(r.Prompt), 40), truncate(r.Status, 10),
					truncate(strOr(r.ModelID, "-"), 24), r.PromptTok, r.CompTok, dur, r.CreatedAt)
			}
			return nil
		}}
	list.Flags().IntVar(&limit, "limit", 20, "max rows to return")
	list.Flags().StringVar(&profile, "profile", "", "filter by master_profile")

	show := &cobra.Command{Use: "show <id>", Short: "Show full detail for a run (accepts an id prefix)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			prefix := args[0]
			var (
				id, createdAt, kind, taskType, execution, masterProfile, board, prompt, status string
				modelID                                                                        sql.NullString
				promptTok, compTok, cacheRead, cacheCreate                                     int
				cost                                                                           float64
				dur                                                                            sql.NullInt64
				completedAt                                                                    sql.NullString
				metadata                                                                       string
			)
			err = db.QueryRow(`SELECT id, created_at, kind, task_type, execution, master_profile, board, prompt, status,
				model_id, prompt_tokens, completion_tokens, cache_read_tokens, cache_creation_tokens,
				estimated_cost_usd, duration_ms, completed_at, metadata_json
				FROM runs WHERE id LIKE ?||'%' ORDER BY created_at DESC LIMIT 1`, prefix).Scan(
				&id, &createdAt, &kind, &taskType, &execution, &masterProfile, &board, &prompt, &status,
				&modelID, &promptTok, &compTok, &cacheRead, &cacheCreate, &cost, &dur, &completedAt, &metadata)
			if err == sql.ErrNoRows {
				if flagJSON {
					// JSON error path stays JSON (clean exit, mirrors cache cmds).
					return emitJSON(map[string]any{"error": fmt.Sprintf("no run matching id prefix %q", prefix)})
				}
				// Non-JSON: return an error so Execute() prints it and exits 1.
				return fmt.Errorf("no run matching id prefix %q", prefix)
			}
			if err != nil {
				return err
			}
			// Pull the most recent step output as the run "result", if any.
			var result string
			db.QueryRow(`SELECT output FROM steps WHERE run_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&result)

			rec := map[string]any{
				"id":                    id,
				"created_at":            createdAt,
				"kind":                  kind,
				"task_type":             taskType,
				"execution":             execution,
				"master_profile":        masterProfile,
				"board":                 board,
				"prompt":                prompt,
				"status":                status,
				"model_id":              nullStrScan(modelID),
				"prompt_tokens":         promptTok,
				"completion_tokens":     compTok,
				"cache_read_tokens":     cacheRead,
				"cache_creation_tokens": cacheCreate,
				"estimated_cost_usd":    cost,
				"duration_ms":           nullIntScan(dur),
				"completed_at":          nullStrScan(completedAt),
				"metadata_json":         metadata,
				"result":                result,
			}
			if flagJSON {
				return emitJSON(rec)
			}
			fmt.Printf("Run %s\n", id)
			fmt.Println(strings.Repeat("-", 60))
			fmt.Printf("Created:     %s\n", createdAt)
			fmt.Printf("Status:      %s\n", status)
			fmt.Printf("Kind:        %s\n", kind)
			fmt.Printf("Task type:   %s\n", taskType)
			fmt.Printf("Execution:   %s\n", execution)
			fmt.Printf("Profile:     %s\n", masterProfile)
			fmt.Printf("Board:       %s\n", board)
			fmt.Printf("Model:       %s\n", strOr(modelID.String, "-"))
			fmt.Printf("Prompt tok:  %d\n", promptTok)
			fmt.Printf("Compl tok:   %d\n", compTok)
			fmt.Printf("Cache read:  %d\n", cacheRead)
			fmt.Printf("Cache creat: %d\n", cacheCreate)
			fmt.Printf("Cost (usd):  %g\n", cost)
			if dur.Valid {
				fmt.Printf("Duration:    %dms\n", dur.Int64)
			}
			if completedAt.Valid {
				fmt.Printf("Completed:   %s\n", completedAt.String)
			}
			fmt.Printf("\nPrompt:\n%s\n", prompt)
			if result != "" {
				fmt.Printf("\nResult:\n%s\n", result)
			}
			return nil
		}}

	c.AddCommand(list, show)
	root.AddCommand(c)
}

// oneLine collapses whitespace/newlines to single spaces for table cells.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// nullStrScan returns nil for a NULL string column (JSON null), else the value.
func nullStrScan(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

// nullIntScan returns nil for a NULL integer column (JSON null), else the value.
func nullIntScan(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}
