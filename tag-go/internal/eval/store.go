package eval

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// StatusRunning etc. are the eval_runs.status values. Python only ever writes
// 'running' then 'completed', so a crashed or SIGTERM'd Python run is stranded
// in 'running' forever. The Go runner always reaches a terminal status.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// EnsureSchema creates the eval_runs / eval_cases tables if missing and adds the
// cost-attribution columns. Both tables also exist in store/migrate/schema.sql;
// this mirrors them idempotently so the package owns its own schema and no
// shared migration file has to change (see internal/evaljudge for the pattern).
func EnsureSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS eval_runs (
		  id TEXT PRIMARY KEY, suite_path TEXT NOT NULL, profile TEXT NOT NULL,
		  suite_name TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'running',
		  pass_count INTEGER NOT NULL DEFAULT 0, fail_count INTEGER NOT NULL DEFAULT 0,
		  total_count INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, completed_at TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_er_status ON eval_runs(status, created_at);
		CREATE TABLE IF NOT EXISTS eval_cases (
		  id TEXT PRIMARY KEY, eval_run_id TEXT NOT NULL, case_id TEXT NOT NULL,
		  input TEXT NOT NULL, output TEXT NOT NULL DEFAULT '',
		  passed INTEGER NOT NULL DEFAULT 0, score REAL NOT NULL DEFAULT 0.0,
		  failure_reason TEXT, created_at TEXT NOT NULL,
		  FOREIGN KEY(eval_run_id) REFERENCES eval_runs(id)
		);
		CREATE INDEX IF NOT EXISTS idx_ec_run ON eval_cases(eval_run_id, passed);`); err != nil {
		return err
	}
	// Additive columns for cost attribution (internal/pricing). cost_usd stays
	// NULL when no rate is known for the model — never a misleading 0.
	for _, c := range []struct{ table, col, decl string }{
		{"eval_cases", "prompt_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"eval_cases", "completion_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"eval_cases", "cost_usd", "REAL"},
		{"eval_cases", "provider", "TEXT NOT NULL DEFAULT ''"},
		{"eval_cases", "model", "TEXT NOT NULL DEFAULT ''"},
		{"eval_runs", "provider", "TEXT NOT NULL DEFAULT ''"},
		{"eval_runs", "total_cost_usd", "REAL"},
	} {
		if err := addColumn(db, c.table, c.col, c.decl); err != nil {
			return err
		}
	}
	return nil
}

func addColumn(db *sql.DB, table, col, decl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		if n == col {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl))
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// CreateRun inserts a 'running' eval_runs row and returns its id (Python's
// create_eval_run: uuid4().hex[:16]).
func CreateRun(db *sql.DB, suitePath, profile, suiteName, provider string) (string, error) {
	if err := EnsureSchema(db); err != nil {
		return "", err
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	_, err := db.Exec(`INSERT INTO eval_runs(id,suite_path,profile,suite_name,status,
		pass_count,fail_count,total_count,created_at,provider)
		VALUES(?,?,?,?,'running',0,0,0,?,?)`, id, suitePath, profile, suiteName, now(), provider)
	if err != nil {
		return "", err
	}
	return id, nil
}

// RecordCase persists one graded case.
func RecordCase(db *sql.DB, r CaseResult) error {
	var reason any
	if r.FailureReason != "" {
		reason = r.FailureReason
	}
	var cost any
	if r.CostUSD != nil {
		cost = *r.CostUSD
	}
	passed := 0
	if r.Passed {
		passed = 1
	}
	_, err := db.Exec(`INSERT INTO eval_cases(id,eval_run_id,case_id,input,output,passed,score,
		failure_reason,created_at,prompt_tokens,completion_tokens,cost_usd,provider,model)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		strings.ReplaceAll(uuid.NewString(), "-", "")[:16], r.RunID, r.CaseID, r.Input, r.Output,
		passed, r.Score, reason, now(), r.PromptTokens, r.CompletionTokens, cost, r.Provider, r.Model)
	return err
}

// FinalizeRun aggregates eval_cases into the parent run and closes it with a
// terminal status. Unlike Python's finalize_eval_run it takes the status, so a
// cancelled or crashed run is recorded as such rather than as 'completed'.
func FinalizeRun(db *sql.DB, runID, status string) (total, passed, failed int, err error) {
	row := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(passed),0), COALESCE(SUM(1-passed),0)
		FROM eval_cases WHERE eval_run_id=?`, runID)
	if err = row.Scan(&total, &passed, &failed); err != nil {
		return
	}
	var cost sql.NullFloat64
	_ = db.QueryRow(`SELECT SUM(cost_usd) FROM eval_cases WHERE eval_run_id=? AND cost_usd IS NOT NULL`, runID).Scan(&cost)
	var costArg any
	if cost.Valid {
		costArg = cost.Float64
	}
	_, err = db.Exec(`UPDATE eval_runs SET status=?, pass_count=?, fail_count=?, total_count=?,
		completed_at=?, total_cost_usd=? WHERE id=?`, status, passed, failed, total, now(), costArg, runID)
	return
}
