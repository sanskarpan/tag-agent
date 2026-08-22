package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// swarm_runs and swarm_tasks already ship in internal/store/migrate/schema.sql,
// so this package adds only the table that was missing: swarm_context, the
// write-once bus. It is created on demand (the same self-healing approach
// internal/worker uses for queue_jobs.result) rather than by editing the shared
// schema file, which several parallel ports also touch.
const swarmContextSchema = `
CREATE TABLE IF NOT EXISTS swarm_context (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  swarm_id    TEXT NOT NULL REFERENCES swarm_runs(swarm_id),
  key         TEXT NOT NULL,
  value       TEXT NOT NULL,
  value_type  TEXT NOT NULL CHECK(value_type IN ('string','number','boolean','json_object','json_array')),
  written_by  TEXT NOT NULL,
  written_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  schema_hint TEXT,
  UNIQUE(swarm_id, key)
);
CREATE INDEX IF NOT EXISTS idx_swarm_tasks_swarm_id ON swarm_tasks(swarm_id);
CREATE INDEX IF NOT EXISTS idx_swarm_ctx_swarm_key ON swarm_context(swarm_id, key);
`

// EnsureSchema creates the swarm_context table if the DB predates it, and adds
// the degraded/degraded_reason columns to swarm_runs on a DB created before they
// existed (so `swarm list/status/results` can report a coordinator fallback).
func EnsureSchema(db *sql.DB) error {
	if _, err := db.Exec(swarmContextSchema); err != nil {
		return err
	}
	return ensureSwarmDegradedColumns(db)
}

// ensureSwarmDegradedColumns adds degraded/degraded_reason to swarm_runs if
// absent. SQLite has no ADD COLUMN IF NOT EXISTS, so check PRAGMA table_info
// first (the same pattern internal/worker.ensureQueueColumn uses).
func ensureSwarmDegradedColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(swarm_runs)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if !have["degraded"] {
		if _, err := db.Exec(`ALTER TABLE swarm_runs ADD COLUMN degraded INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !have["degraded_reason"] {
		if _, err := db.Exec(`ALTER TABLE swarm_runs ADD COLUMN degraded_reason TEXT`); err != nil {
			return err
		}
	}
	return nil
}

// markDegraded records a coordinator fallback on the run row, so machine
// consumers (swarm list/results --json) can see the run did not run as a real
// decomposed swarm even though its status is "completed".
func markDegraded(db *sql.DB, swarmID, reason string) error {
	ctx, cancel := bgCtx()
	defer cancel()
	_, err := db.ExecContext(ctx,
		`UPDATE swarm_runs SET degraded=1, degraded_reason=? WHERE swarm_id=?`, reason, swarmID)
	return err
}

// nowISO matches Python's _now_iso: millisecond-precision UTC with a Z suffix,
// which is what already-persisted swarm rows use.
func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

// finishTimeout bounds terminal-status writes so a cancelled run can still
// record its outcome (same contract as internal/worker.finish).
const finishTimeout = 10 * time.Second

// bgCtx returns a short-lived context for a write that must happen even after
// the run context has been cancelled (SIGTERM).
func bgCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), finishTimeout)
}

// CreateRun inserts the swarm_runs row (Python: create_swarm_run).
func CreateRun(ctx context.Context, db *sql.DB, swarmID, goal, coordinatorProfile, failurePolicy string, maxAgents int) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO swarm_runs(swarm_id, goal, coordinator_profile, failure_policy, max_agents, status, created_at)
		 VALUES(?,?,?,?,?, 'pending', ?)`,
		swarmID, goal, coordinatorProfile, failurePolicy, maxAgents, nowISO())
	return err
}

// InsertTasks inserts the swarm_tasks rows and sets task_count
// (Python: insert_swarm_tasks).
func InsertTasks(ctx context.Context, db *sql.DB, swarmID string, tasks []Task) error {
	for _, t := range tasks {
		cs, _ := json.Marshal(t.ContextSlice)
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO swarm_tasks(swarm_id, task_id, profile, description, context_slice_json, status)
			 VALUES(?,?,?,?,?, 'pending')`,
			swarmID, t.TaskID, t.Profile, t.Description, string(cs)); err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx, `UPDATE swarm_runs SET task_count=? WHERE swarm_id=?`, len(tasks), swarmID)
	return err
}

// SetManifest stores the manifest JSON on the run for later inspection.
func SetManifest(ctx context.Context, db *sql.DB, swarmID string, m *Manifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE swarm_runs SET manifest_json=? WHERE swarm_id=?`, string(b), swarmID)
	return err
}

// markRunning flips the run to 'running' and stamps started_at.
func markRunning(ctx context.Context, db *sql.DB, swarmID string) error {
	_, err := db.ExecContext(ctx, `UPDATE swarm_runs SET status='running', started_at=? WHERE swarm_id=?`,
		nowISO(), swarmID)
	return err
}

// finishRun writes the run's terminal status on a background context so a
// SIGTERM-cancelled swarm is still recorded rather than left 'running'.
func finishRun(db *sql.DB, swarmID, status, finalOutput string, promptTok, completionTok int, cost float64) error {
	ctx, cancel := bgCtx()
	defer cancel()
	_, err := db.ExecContext(ctx,
		`UPDATE swarm_runs SET status=?, completed_at=?, final_output=?,
		   total_tokens_prompt=?, total_tokens_completion=?, total_cost_usd=?
		 WHERE swarm_id=?`,
		status, nowISO(), finalOutput, promptTok, completionTok, cost, swarmID)
	return err
}

// setTaskRunning claims a task row.
func setTaskRunning(ctx context.Context, db *sql.DB, swarmID, taskID, profile, description string, slice ContextSlice) error {
	cs, _ := json.Marshal(slice)
	_, err := db.ExecContext(ctx,
		`UPDATE swarm_tasks SET status='running', started_at=?, profile=?, description=?, context_slice_json=?
		 WHERE swarm_id=? AND task_id=?`,
		nowISO(), profile, description, string(cs), swarmID, taskID)
	return err
}

// setTaskStatus records a terminal task status (Python: _set_task_status). It
// uses a background context for the same reason finishRun does.
func setTaskStatus(db *sql.DB, swarmID, taskID, status, errMsg string) error {
	ctx, cancel := bgCtx()
	defer cancel()
	_, err := db.ExecContext(ctx,
		`UPDATE swarm_tasks SET status=?, completed_at=?, error_message=? WHERE swarm_id=? AND task_id=?`,
		status, nowISO(), truncate(errMsg, 2000), swarmID, taskID)
	return err
}

// completeTask records a task outcome plus its usage, and folds that usage into
// the run totals.
func completeTask(db *sql.DB, swarmID string, r TaskResult) error {
	ctx, cancel := bgCtx()
	defer cancel()
	status := r.Status
	switch status {
	case "done", "failed", "timed_out", "skipped", "memory_limit_exceeded":
	default:
		status = "failed"
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE swarm_tasks SET status=?, completed_at=?, output=?, error_message=?,
		   tokens_prompt=?, tokens_completion=?, cost_usd=?, model=?
		 WHERE swarm_id=? AND task_id=?`,
		status, nowISO(), truncate(r.Output, 10000), truncate(r.ErrorMessage, 2000),
		r.TokensPrompt, r.TokensCompletion, r.CostUSD, r.Model, swarmID, r.TaskID); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		`UPDATE swarm_runs SET total_tokens_prompt = total_tokens_prompt + ?,
		   total_tokens_completion = total_tokens_completion + ?,
		   total_cost_usd = total_cost_usd + ?
		 WHERE swarm_id=?`,
		r.TokensPrompt, r.TokensCompletion, r.CostUSD, swarmID)
	return err
}

// runAborted reports whether another process flipped this run to 'aborted'
// (i.e. `tag swarm abort` was issued). In Python a sub-agent is a subprocess and
// abort reaches it with SIGTERM; Go sub-agents are goroutines, so the DB status
// IS the abort channel.
func runAborted(ctx context.Context, db *sql.DB, swarmID string) bool {
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM swarm_runs WHERE swarm_id=?`, swarmID).Scan(&status); err != nil {
		return false
	}
	return status == "aborted"
}

// truncate clamps a string to n bytes (matching Python's [:n] slicing on the DB
// writes) without splitting a multi-byte rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
