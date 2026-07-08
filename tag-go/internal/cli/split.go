package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/store"
)

// splitEnsureSchema self-ensures the split_runs / split_items tables. These are
// NOT in internal/store/migrate/schema.sql, so — exactly like the Python
// split_agent.ensure_schema (src/tag/split_agent.py) — every split command
// creates them on first use. The DDL below is a faithful copy of that Python
// schema.
func splitEnsureSchema(db *store.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS split_runs (
		  id               TEXT PRIMARY KEY,
		  task             TEXT NOT NULL,
		  architect_model  TEXT NOT NULL,
		  editor_model     TEXT NOT NULL,
		  profile          TEXT NOT NULL,
		  spec_json        TEXT,
		  status           TEXT NOT NULL DEFAULT 'pending',
		  items_total      INTEGER NOT NULL DEFAULT 0,
		  items_done       INTEGER NOT NULL DEFAULT 0,
		  items_rejected   INTEGER NOT NULL DEFAULT 0,
		  created_at       TEXT NOT NULL,
		  updated_at       TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS split_items (
		  id          TEXT PRIMARY KEY,
		  run_id      TEXT NOT NULL,
		  item_id     TEXT NOT NULL,
		  file        TEXT NOT NULL,
		  description TEXT NOT NULL,
		  action      TEXT NOT NULL DEFAULT 'modify',
		  status      TEXT NOT NULL DEFAULT 'pending',
		  diff        TEXT,
		  verdict     TEXT,
		  retry_count INTEGER NOT NULL DEFAULT 0,
		  created_at  TEXT NOT NULL,
		  FOREIGN KEY(run_id) REFERENCES split_runs(id)
		);
		CREATE INDEX IF NOT EXISTS idx_si_run ON split_items(run_id, status);
	`)
	return err
}

// registerSplit wires `tag split` — Architect/Editor split execution (PRD-042,
// src/tag/cmd/agent_tools.py:cmd_split).
//
// Read paths are ported faithfully:
//
//	split list          -> list_split_runs  (read-only)
//	split show <run_id> -> get_split_run     (read-only, id-prefix resolution)
//
// The write/execute path (`split plan`, which drives the architect agent loop
// / LLM to decompose a task, or persists a supplied spec) is an honest stub:
// it needs the agent loop that Track-B stubs out and would make live API calls,
// which is out of scope here.
func registerSplit(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "split", Short: "Architect/Editor agent split execution", GroupID: "tools"}

	list := &cobra.Command{Use: "list", Short: "List split runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if err := splitEnsureSchema(db); err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id, task, architect_model, editor_model, profile, status,
				items_total, items_done, created_at
				FROM split_runs ORDER BY created_at DESC LIMIT 20`)
			if err != nil {
				return err
			}
			defer rows.Close()
			type row struct {
				ID             string `json:"id"`
				Task           string `json:"task"`
				ArchitectModel string `json:"architect_model"`
				EditorModel    string `json:"editor_model"`
				Profile        string `json:"profile"`
				Status         string `json:"status"`
				ItemsTotal     int    `json:"items_total"`
				ItemsDone      int    `json:"items_done"`
				CreatedAt      string `json:"created_at"`
			}
			// Non-nil so an empty result marshals to [] not null (Python parity).
			out := []row{}
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.ID, &r.Task, &r.ArchitectModel, &r.EditorModel,
					&r.Profile, &r.Status, &r.ItemsTotal, &r.ItemsDone, &r.CreatedAt); err != nil {
					return err
				}
				r.Task = truncate(r.Task, 60) // mirrors list_split_runs' task[:60]
				out = append(out, r)
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No architect/editor split runs.")
				return nil
			}
			for _, r := range out {
				fmt.Printf("%-12s  %-12s  %-20s -> %-20s  %s\n",
					truncate(r.ID, 12), truncate(r.Status, 12),
					truncate(r.ArchitectModel, 20), truncate(r.EditorModel, 20),
					truncate(r.Task, 50))
			}
			return nil
		}}

	show := &cobra.Command{Use: "show <run_id>", Short: "Show split run details (accepts an id prefix)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if err := splitEnsureSchema(db); err != nil {
				return err
			}
			prefix := args[0]
			var (
				id, task, architect, editor, profile, status, createdAt, updatedAt string
				specJSON                                                           sql.NullString
				itemsTotal, itemsDone, itemsRejected                               int
			)
			// id-prefix resolution (an enhancement over Python's exact match),
			// consistent with `runs show`.
			err = db.QueryRow(`SELECT id, task, architect_model, editor_model, profile, spec_json,
				status, items_total, items_done, items_rejected, created_at, updated_at
				FROM split_runs WHERE id LIKE ?||'%' ORDER BY created_at DESC LIMIT 1`, prefix).Scan(
				&id, &task, &architect, &editor, &profile, &specJSON,
				&status, &itemsTotal, &itemsDone, &itemsRejected, &createdAt, &updatedAt)
			if err == sql.ErrNoRows {
				if flagJSON {
					_ = emitJSON(map[string]any{"error": fmt.Sprintf("split run not found: %q", prefix)})
					return fmt.Errorf("split run not found: %q", prefix)
				}
				return fmt.Errorf("split run not found: %q", prefix)
			}
			if err != nil {
				return err
			}

			// Load items (mirrors get_split_run's split_items query).
			itemRows, err := db.Query(`SELECT item_id, file, description, action, status, verdict
				FROM split_items WHERE run_id=? ORDER BY rowid`, id)
			if err != nil {
				return err
			}
			defer itemRows.Close()
			type item struct {
				ItemID      string `json:"item_id"`
				File        string `json:"file"`
				Description string `json:"description"`
				Action      string `json:"action"`
				Status      string `json:"status"`
				Verdict     any    `json:"verdict"`
			}
			items := []item{}
			for itemRows.Next() {
				var it item
				var verdict sql.NullString
				if err := itemRows.Scan(&it.ItemID, &it.File, &it.Description, &it.Action, &it.Status, &verdict); err != nil {
					return err
				}
				it.Verdict = nullStrScan(verdict)
				items = append(items, it)
			}

			// spec = parsed spec_json, or null (mirrors get_split_run["spec"]).
			var spec any
			if specJSON.Valid && specJSON.String != "" {
				if e := json.Unmarshal([]byte(specJSON.String), &spec); e != nil {
					spec = nil
				}
			}

			rec := map[string]any{
				"id":              id,
				"task":            task,
				"architect_model": architect,
				"editor_model":    editor,
				"profile":         profile,
				"spec_json":       nullStrScan(specJSON),
				"status":          status,
				"items_total":     itemsTotal,
				"items_done":      itemsDone,
				"items_rejected":  itemsRejected,
				"created_at":      createdAt,
				"updated_at":      updatedAt,
				"spec":            spec,
				"items":           items,
			}
			if flagJSON {
				return emitJSON(rec)
			}
			fmt.Printf("Run:         %s\n", id)
			fmt.Printf("Task:        %s\n", task)
			fmt.Printf("Architect:   %s\n", architect)
			fmt.Printf("Editor:      %s\n", editor)
			fmt.Printf("Status:      %s\n", status)
			fmt.Printf("Items:       %d/%d done, %d rejected\n", itemsDone, itemsTotal, itemsRejected)
			if len(items) > 0 {
				fmt.Println("\nItems:")
				for _, it := range items {
					icon := "?"
					switch it.Status {
					case "accepted":
						icon = "+"
					case "rejected":
						icon = "x"
					case "pending":
						icon = "o"
					}
					fmt.Printf("  %s [%-8s] %-40s  %s\n", icon, it.Action, it.File, truncate(it.Description, 50))
				}
			}
			return nil
		}}

	// plan: honest stub for the write/execute path (agent loop / LLM).
	var planArchitect, planEditor, planProfile, planSpecJSON string
	plan := &cobra.Command{Use: "plan <task>", Short: "Create a split run plan (requires the agent loop; not available offline)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			msg := "split plan requires the architect agent loop (LLM) and is not available in the offline Go port"
			if flagJSON {
				_ = emitJSON(map[string]any{"error": msg})
				return fmt.Errorf("split plan unavailable offline")
			}
			return fmt.Errorf("%s", msg)
		}}
	plan.Flags().StringVar(&planArchitect, "architect", "claude-opus-4", "architect model")
	plan.Flags().StringVar(&planEditor, "editor", "claude-haiku-4-5", "editor model")
	plan.Flags().StringVar(&planProfile, "profile", "", "profile (default: master profile)")
	plan.Flags().StringVar(&planSpecJSON, "spec-json", "", "optional pre-built spec JSON")

	c.AddCommand(list, show, plan)
	root.AddCommand(c)
}
