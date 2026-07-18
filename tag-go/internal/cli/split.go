package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
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
// The write/execute path (`split plan`) drives the native architect agent loop
// (Track B) to decompose a task into a change spec, or persists a supplied
// --spec-json. It defaults to the offline `echo` provider (no keys, no network);
// `--provider openai|anthropic` selects a real adapter.
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

	// plan: native architect agent loop (Track B) that decomposes a task into a
	// change spec and persists it to split_runs (+ split_items).
	var planArchitect, planEditor, planProfile, planSpecJSON, planProvider string
	plan := &cobra.Command{Use: "plan <task>", Short: "Decompose a task into a split run plan (echo default; --provider for real)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return splitPlan(app, args[0], planProvider, planArchitect, planEditor, planProfile, planSpecJSON)
		}}
	plan.Flags().StringVar(&planArchitect, "architect", "claude-opus-4", "architect model")
	plan.Flags().StringVar(&planEditor, "editor", "claude-haiku-4-5", "editor model")
	plan.Flags().StringVar(&planProfile, "profile", "", "profile (default: master profile)")
	plan.Flags().StringVar(&planSpecJSON, "spec-json", "", "optional pre-built spec JSON")
	plan.Flags().StringVar(&planProvider, "provider", "echo", "llm provider for the architect loop (echo = offline)")

	c.AddCommand(list, show, plan)
	root.AddCommand(c)
}

// splitItem is one change in a split spec (parity with split_agent.ChangeItem).
type splitItem struct {
	ID          string `json:"id"`
	File        string `json:"file"`
	Description string `json:"description"`
	Action      string `json:"action"`
}

// splitSpec is the architect's change specification (parity with
// split_agent.ChangeSpec: task + rationale + items).
type splitSpec struct {
	Task      string      `json:"task"`
	Rationale string      `json:"rationale"`
	Items     []splitItem `json:"items"`
}

// extractJSONObject returns the first brace-balanced {...} block in s, or "".
// Unlike a greedy regex, it stops at the matching close brace (not the last one
// in the string), so trailing braces a model may emit after the spec don't
// corrupt the captured span. Braces inside JSON string literals are ignored.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// architectPrompt instructs the architect model to emit a JSON change spec. The
// offline echo provider replays the last user message verbatim, so this prompt
// itself embeds a valid example spec — parseSpec extracts it and the deterministic
// fallback guarantees a usable plan even when a model returns prose.
func architectPrompt(task string) string {
	return "Decompose the following software task into a JSON change specification.\n" +
		"Respond with ONLY a JSON object of the form " +
		`{"task": "...", "rationale": "...", "items": [{"id": "item-1", "file": "path", "description": "...", "action": "modify"}]}.` +
		"\n\nTask: " + task
}

// parseSpec extracts a splitSpec from model output. It tolerates surrounding
// prose by grabbing the first {...} block. On any failure it returns a
// single-item deterministic fallback so `split plan` always produces a usable,
// persisted plan (offline-safe).
func parseSpec(task, output string) splitSpec {
	if m := extractJSONObject(output); m != "" {
		var s splitSpec
		if err := json.Unmarshal([]byte(m), &s); err == nil && len(s.Items) > 0 {
			return normalizeSpec(task, s)
		}
	}
	// Fallback: one item describing the whole task.
	return normalizeSpec(task, splitSpec{
		Task:      task,
		Rationale: "single-step plan (architect returned no structured spec)",
		Items: []splitItem{{
			ID: "item-1", File: "TBD", Description: task, Action: "modify",
		}},
	})
}

// normalizeSpec fills defaults (task, item ids, actions) so persistence and
// rendering are consistent regardless of what the architect emitted.
func normalizeSpec(task string, s splitSpec) splitSpec {
	if strings.TrimSpace(s.Task) == "" {
		s.Task = task
	}
	for i := range s.Items {
		if strings.TrimSpace(s.Items[i].ID) == "" {
			s.Items[i].ID = fmt.Sprintf("item-%d", i+1)
		}
		if strings.TrimSpace(s.Items[i].Action) == "" {
			s.Items[i].Action = "modify"
		}
		if strings.TrimSpace(s.Items[i].File) == "" {
			s.Items[i].File = "TBD"
		}
	}
	return s
}

// splitPlan drives the architect agent loop to build a change spec (or uses a
// supplied --spec-json), then persists it to split_runs + split_items.
func splitPlan(app *App, task, provider, architect, editor, profileFlag, specJSON string) error {
	profile := app.profile(profileFlag)
	db, err := app.OpenDB()
	if err != nil {
		return err
	}
	if err := splitEnsureSchema(db); err != nil {
		return err
	}

	var spec splitSpec
	if strings.TrimSpace(specJSON) != "" {
		// Supplied spec: parse strictly (a bad --spec-json is a usage error).
		if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
			return usageErrorf("invalid --spec-json: %v", err)
		}
		spec = normalizeSpec(task, spec)
		if len(spec.Items) == 0 {
			return usageErrorf("--spec-json has no items")
		}
	} else {
		prov, ok := llm.Registry[provider]
		if !ok {
			return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
		}
		loop := &agent.Loop{Provider: prov}
		res, err := loop.Run(context.Background(), architectPrompt(task), agent.Options{
			Model:  architect,
			System: "You are a software architect. Decompose tasks into a JSON change specification.",
		})
		if err != nil {
			return err
		}
		spec = parseSpec(task, res.FinalText)
	}

	runID := uuid.NewString()[:16]
	now := time.Now().UTC().Format(time.RFC3339)
	specBytes, _ := json.Marshal(spec)
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("recording split run: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO split_runs
		(id,task,architect_model,editor_model,profile,spec_json,status,items_total,items_done,items_rejected,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, task, architect, editor, profile, string(specBytes), "planned", len(spec.Items), 0, 0, now, now); err != nil {
		return fmt.Errorf("recording split run: %w", err)
	}
	for _, it := range spec.Items {
		itemID := uuid.NewString()[:16]
		if _, err := tx.Exec(`INSERT INTO split_items
			(id,run_id,item_id,file,description,action,status,created_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			itemID, runID, it.ID, it.File, it.Description, it.Action, "pending", now); err != nil {
			return fmt.Errorf("recording split item: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording split run: %w", err)
	}

	if flagJSON {
		return emitJSON(map[string]any{
			"run_id": runID, "task": task, "provider": provider,
			"architect_model": architect, "editor_model": editor,
			"status": "planned", "items_total": len(spec.Items), "spec": spec,
		})
	}
	fmt.Printf("Planned split run %s (%s)\n", runID, provider)
	fmt.Printf("Task:      %s\n", task)
	fmt.Printf("Architect: %s\n", architect)
	fmt.Printf("Editor:    %s\n", editor)
	fmt.Printf("Items:     %d\n", len(spec.Items))
	for _, it := range spec.Items {
		fmt.Printf("  - [%-6s] %-40s  %s\n", it.Action, it.File, truncate(it.Description, 50))
	}
	fmt.Printf("\nRun `tag split show %s` for details.\n", runID)
	return nil
}
