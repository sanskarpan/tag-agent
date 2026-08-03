// Package loop implements the autonomous agent-loop lifecycle (PRD-021), the Go
// port of src/tag/cmd/ci_loop.py:cmd_loop + src/tag/loop_agent.py.
//
// The Python control plane models a loop as: a `loop_runs` row plus a DETACHED
// worker process (`python -m tag.loop_agent`) that shells out to `tag submit`
// once per iteration, journalling each pass into `loop_iterations`. Go owns its
// runtime, so the iteration body is the in-process agent loop (internal/agent)
// against a provider-neutral llm.Provider — no subprocess, no shell-out. The
// *observable* contract (table shapes, statuses, decisions, goal detection,
// approval semantics) is preserved so the two engines are interchangeable.
//
// All lifecycle state lives in SQLite, which is what makes the cross-process
// requirements work: `tag loop abort` / `approve` / `deny` run in a DIFFERENT
// process from the loop and communicate purely through the store.
package loop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Statuses a loop run can hold. running/completed/failed/aborted/max_iters are
// byte-for-byte the Python set (loop_agent._update_loop_status). WaitingApproval
// is additive and deliberate: Python leaves a loop blocked on a human gate in
// 'running', which makes a waiting loop indistinguishable from a working one —
// exactly the silent-hang failure mode this project bars. See runner.go.
const (
	StatusRunning          = "running"
	StatusWaitingApproval  = "waiting_approval"
	StatusCompleted        = "completed"
	StatusFailed           = "failed"
	StatusAborted          = "aborted"
	StatusMaxIters         = "max_iters"
	DecisionContinue       = "continue"
	DecisionGoalAchieved   = "goal_achieved"
	DecisionError          = "error"
	DecisionAborted        = "aborted"
	ApprovalAuto           = "auto"
	ApprovalHuman          = "human"
	approvalDecisionAllow  = "continue"
	approvalDecisionReject = "abort"
)

// activeStatuses are the non-terminal statuses. A loop in any other status must
// stop; that is how an out-of-process `loop abort` reaches a live runner.
func isActive(status string) bool {
	return status == StatusRunning || status == StatusWaitingApproval
}

// IsTerminal reports whether a status means the loop has stopped for good.
func IsTerminal(status string) bool { return !isActive(status) }

// idRe is the slug guard for a loop id (C013 in the Python port). Python needed
// it because approve/deny built a FILESYSTEM path from the id and '../pwned'
// escaped the approvals directory. Go signals approvals through the DB, so the
// traversal is structurally impossible — the check is kept anyway so a bogus id
// is rejected with the same message rather than silently matching nothing.
var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateID rejects ids that are not plain slugs.
func ValidateID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("Invalid loop id: %q", id)
	}
	return nil
}

// ErrNotFound is returned when a loop id does not exist.
var ErrNotFound = errors.New("loop not found")

// schema mirrors src/tag/core/db.py's PRD-021 block field-for-field, plus the
// Go-only loop_approvals table that replaces Python's loop-approvals/<id>.json.
//
// It is applied lazily from this package rather than added to
// internal/store/migrate/schema.sql, following internal/worker's precedent
// (ensureResultColumn): schema.sql is shared with other in-flight ports, and a
// CREATE TABLE IF NOT EXISTS here is idempotent, self-healing on pre-existing
// databases, and conflict-free.
const schema = `
CREATE TABLE IF NOT EXISTS loop_runs (
  id           TEXT PRIMARY KEY,
  profile      TEXT NOT NULL,
  goal         TEXT NOT NULL,
  max_iters    INTEGER NOT NULL DEFAULT 10,
  current_iter INTEGER NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT 'running',
  approval     TEXT NOT NULL DEFAULT 'auto',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  completed_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_lr_status ON loop_runs(status, created_at);

CREATE TABLE IF NOT EXISTS loop_iterations (
  id         TEXT PRIMARY KEY,
  loop_id    TEXT NOT NULL,
  iteration  INTEGER NOT NULL,
  input      TEXT NOT NULL,
  output     TEXT NOT NULL DEFAULT '',
  decision   TEXT NOT NULL DEFAULT 'continue',
  created_at TEXT NOT NULL,
  FOREIGN KEY(loop_id) REFERENCES loop_runs(id)
);
CREATE INDEX IF NOT EXISTS idx_li_loop ON loop_iterations(loop_id, iteration);

CREATE TABLE IF NOT EXISTS loop_approvals (
  loop_id        TEXT PRIMARY KEY,
  iteration      INTEGER NOT NULL,
  output_preview TEXT NOT NULL DEFAULT '',
  decision       TEXT NOT NULL DEFAULT 'pending',
  requested_at   TEXT NOT NULL,
  decided_at     TEXT
);
`

// EnsureSchema creates the loop tables if they are absent. Safe to call on every
// command (idempotent), which is what makes `list`/`status` work on a database
// that predates this feature.
func EnsureSchema(db *sql.DB) error {
	// Retried: the DDL takes a short write lock, and a live loop hammering the
	// same database would otherwise make `loop abort` fail during its schema
	// check, before its own retry-protected UPDATE ever ran.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := execRetry(ctx, db, schema)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// isBusy reports a transient SQLITE_BUSY / locked error (same test as
// store.isBusy, which is unexported).
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "database is locked") ||
		strings.Contains(m, "sqlite_busy") ||
		strings.Contains(m, "database table is locked")
}

// execRetry runs a write, retrying while SQLite reports the database busy.
//
// This matters specifically for the CROSS-PROCESS commands. A running loop is a
// steady stream of small writes on its own connection; `tag loop abort` from
// another shell contends with that stream for the single WAL writer, and the
// DSN's 5s busy_timeout can genuinely expire under a fast loop — the abort then
// failed with a raw "database is locked (5)" and the loop kept running. An abort
// that does not abort is worse than a slow one, so the write is retried with
// bounded backoff (it can still fail honestly rather than hang forever).
func execRetry(ctx context.Context, db *sql.DB, query string, args ...any) (sql.Result, error) {
	const attempts = 40
	delay := 25 * time.Millisecond
	var res sql.Result
	var err error
	for i := 0; i < attempts; i++ {
		res, err = db.ExecContext(ctx, query, args...)
		if err == nil || !isBusy(err) || ctx.Err() != nil {
			return res, err
		}
		select {
		case <-ctx.Done():
			return res, err
		case <-time.After(delay):
		}
		if delay < 400*time.Millisecond {
			delay *= 2
		}
	}
	return res, err
}

// queryRowRetry is the read counterpart of execRetry for the abort watcher's hot
// status poll: a spurious busy there must not be mistaken for "loop is gone".
func statusRetry(db *sql.DB, id string) (string, error) {
	const attempts = 10
	delay := 20 * time.Millisecond
	var s string
	var err error
	for i := 0; i < attempts; i++ {
		err = db.QueryRow(`SELECT status FROM loop_runs WHERE id=?`, id).Scan(&s)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if err == nil || !isBusy(err) {
			return s, err
		}
		time.Sleep(delay)
		if delay < 200*time.Millisecond {
			delay *= 2
		}
	}
	return s, err
}

// hexID mirrors Python's uuid.uuid4().hex[:n] (dashes stripped BEFORE truncating).
func hexID(n int) string {
	h := strings.ReplaceAll(uuid.NewString(), "-", "")
	if len(h) > n {
		return h[:n]
	}
	return h
}

// Record is one row of loop_runs.
type Record struct {
	ID          string `json:"id"`
	Profile     string `json:"profile"`
	Goal        string `json:"goal"`
	MaxIters    int    `json:"max_iters"`
	CurrentIter int    `json:"current_iter"`
	Status      string `json:"status"`
	Approval    string `json:"approval"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	CompletedAt string `json:"completed_at"`
}

// Iteration is one row of loop_iterations.
type Iteration struct {
	Iteration int    `json:"iteration"`
	Decision  string `json:"decision"`
	Output    string `json:"output"`
	Input     string `json:"input,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// Create inserts a new loop run in 'running' and returns its id. Mirrors
// cmd_loop's `start` INSERT (12-hex id, current_iter 0, status running).
func Create(db *sql.DB, profile, goal string, maxIters int, approval string) (string, error) {
	if err := EnsureSchema(db); err != nil {
		return "", err
	}
	id := hexID(12)
	ts := now()
	_, err := db.Exec(
		`INSERT INTO loop_runs(id, profile, goal, max_iters, current_iter, status, approval, created_at, updated_at)
		 VALUES(?,?,?,?,0,'running',?,?,?)`,
		id, profile, goal, maxIters, approval, ts, ts)
	if err != nil {
		return "", err
	}
	return id, nil
}

// Get reads one run. Returns ErrNotFound when the id is unknown.
func Get(db *sql.DB, id string) (*Record, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	var r Record
	var completed sql.NullString
	err := db.QueryRow(
		`SELECT id, profile, goal, max_iters, current_iter, status, approval, created_at, updated_at, completed_at
		 FROM loop_runs WHERE id=?`, id).
		Scan(&r.ID, &r.Profile, &r.Goal, &r.MaxIters, &r.CurrentIter, &r.Status, &r.Approval,
			&r.CreatedAt, &r.UpdatedAt, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CompletedAt = completed.String
	return &r, nil
}

// List returns the most recent runs, newest first (Python: ORDER BY created_at
// DESC LIMIT 20). A limit <= 0 keeps the Python default of 20.
func List(db *sql.DB, limit int) ([]Record, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(
		`SELECT id, profile, goal, max_iters, current_iter, status, approval, created_at, updated_at, completed_at
		 FROM loop_runs ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Never nil: a --json consumer must see [] rather than null.
	out := []Record{}
	for rows.Next() {
		var r Record
		var completed sql.NullString
		if err := rows.Scan(&r.ID, &r.Profile, &r.Goal, &r.MaxIters, &r.CurrentIter, &r.Status,
			&r.Approval, &r.CreatedAt, &r.UpdatedAt, &completed); err != nil {
			return nil, err
		}
		r.CompletedAt = completed.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// Iterations returns a run's journal in iteration order.
func Iterations(db *sql.DB, id string) ([]Iteration, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT iteration, decision, output, input, created_at FROM loop_iterations
		 WHERE loop_id=? ORDER BY iteration`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Iteration{}
	for rows.Next() {
		var it Iteration
		if err := rows.Scan(&it.Iteration, &it.Decision, &it.Output, &it.Input, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Status reads just the status column (hot path for the abort watcher).
func Status(db *sql.DB, id string) (string, error) { return statusRetry(db, id) }

// Abort flips an active loop to 'aborted'. It returns false when no ACTIVE loop
// matched, which is how `tag loop abort` reports "no running loop found".
//
// Divergence from Python (intentional): Python matches only status='running', so
// aborting a loop that is parked on a human-approval gate silently did nothing.
// Here both active statuses are abortable.
// abortWriteBudget bounds how long an abort waits for the write lock.
const abortWriteBudget = 45 * time.Second

func Abort(db *sql.DB, id string) (bool, error) {
	if err := EnsureSchema(db); err != nil {
		return false, err
	}
	ts := now()
	// A loop iterating with no model latency writes almost continuously (journal
	// row, status, spans), and SQLite serialises writers -- so an abort issued
	// from another process can starve behind it. 15s was not enough on a loaded
	// machine and the operator got a bare "context deadline exceeded", which
	// says nothing about what to do.
	//
	// The budget is raised, and a timeout now explains itself. Both matter: an
	// abort that gives up is bad, and an abort that gives up illegibly is worse,
	// because the operator cannot tell it from a loop that ignored them.
	ctx, cancel := context.WithTimeout(context.Background(), abortWriteBudget)
	defer cancel()
	r, err := execRetry(ctx, db,
		`UPDATE loop_runs SET status='aborted', updated_at=?, completed_at=?
		 WHERE id=? AND status IN ('running','waiting_approval')`, ts, ts, id)
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf(
				"could not write the abort for loop %s within %s: the loop is writing to the "+
					"store continuously and the write never got through. It is still running; "+
					"retry the abort", id, abortWriteBudget)
		}
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// setStatus writes a terminal (or intermediate) status. Terminal statuses also
// stamp completed_at, mirroring loop_agent._update_loop_status.
func setStatus(ctx context.Context, db *sql.DB, id, status string) error {
	ts := now()
	var completed any
	if IsTerminal(status) {
		completed = ts
	}
	_, err := execRetry(ctx, db,
		`UPDATE loop_runs SET status=?, updated_at=?, completed_at=? WHERE id=?`,
		status, ts, completed, id)
	return err
}

// finishStatus records a terminal status on its own bounded context so a
// cancelled/SIGTERMed run can still persist its outcome. Without this a SIGTERM
// strands the loop in 'running' forever (#574 pattern).
func finishStatus(db *sql.DB, id, status string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return setStatus(ctx, db, id, status)
}

// markIteration journals one pass and advances current_iter (loop_agent._mark_iteration).
func markIteration(db *sql.DB, id string, iteration int, input, output, decision string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ts := now()
	if _, err := execRetry(ctx, db,
		`INSERT INTO loop_iterations(id, loop_id, iteration, input, output, decision, created_at)
		 VALUES(?,?,?,?,?,?,?)`, hexID(12), id, iteration, input, output, decision, ts); err != nil {
		return err
	}
	_, err := execRetry(ctx, db,
		`UPDATE loop_runs SET current_iter=?, updated_at=? WHERE id=?`, iteration, ts, id)
	return err
}

// ApprovalRequest is a pending human-approval checkpoint.
type ApprovalRequest struct {
	LoopID        string `json:"loop_id"`
	Iteration     int    `json:"iteration"`
	OutputPreview string `json:"output_preview"`
	Decision      string `json:"decision"`
	RequestedAt   string `json:"requested_at"`
}

// openApprovalCheckpoint records the pending approval AND flips the run to
// waiting_approval in one transaction.
//
// These used to be two statements. Between them any other process — the CLI,
// another connection — saw a pending approval on a loop whose status still said
// `running`, so an observer polling status to answer "is this gated?" got the
// wrong answer. The window was small, which is why it surfaced as a flaky test
// rather than a bug report:
//
//	loop_test.go:362: status while gated = "running", want waiting_approval
//
// A checkpoint is one fact about the run. Writing it as one fact removes both
// the flake and the inconsistency it was reporting.
func openApprovalCheckpoint(ctx context.Context, db *sql.DB, id string, iteration int, preview string) error {
	const attempts = 40
	delay := 25 * time.Millisecond
	var err error
	for i := 0; i < attempts; i++ {
		if err = func() error {
			tx, terr := db.BeginTx(ctx, nil)
			if terr != nil {
				return terr
			}
			defer tx.Rollback() //nolint:errcheck // no-op after a successful commit
			if _, e := tx.ExecContext(ctx,
				`INSERT INTO loop_approvals(loop_id, iteration, output_preview, decision, requested_at, decided_at)
				 VALUES(?,?,?,'pending',?,NULL)
				 ON CONFLICT(loop_id) DO UPDATE SET iteration=excluded.iteration,
				   output_preview=excluded.output_preview, decision='pending',
				   requested_at=excluded.requested_at, decided_at=NULL`,
				id, iteration, preview, now()); e != nil {
				return e
			}
			ts := now()
			if _, e := tx.ExecContext(ctx,
				`UPDATE loop_runs SET status=?, updated_at=?, completed_at=NULL WHERE id=?`,
				StatusWaitingApproval, ts, id); e != nil {
				return e
			}
			return tx.Commit()
		}(); err == nil {
			return nil
		}
		if !isBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 400*time.Millisecond {
			delay *= 2
		}
	}
	return err
}

// readApproval returns the current decision for a loop's checkpoint.
func readApproval(db *sql.DB, id string) (string, bool, error) {
	var d string
	err := db.QueryRow(`SELECT decision FROM loop_approvals WHERE loop_id=?`, id).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if isBusy(err) {
		// Transient contention is "no decision yet", never a reason to fail the
		// loop out of its approval wait.
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return d, true, nil
}

// PendingApproval returns the open checkpoint for a loop, if any.
func PendingApproval(db *sql.DB, id string) (*ApprovalRequest, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	var a ApprovalRequest
	err := db.QueryRow(
		`SELECT loop_id, iteration, output_preview, decision, requested_at
		 FROM loop_approvals WHERE loop_id=?`, id).
		Scan(&a.LoopID, &a.Iteration, &a.OutputPreview, &a.Decision, &a.RequestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Decide records an approve ("continue") or deny ("abort") decision for a loop
// parked at a checkpoint. It is the producer half of the gate that Runner polls,
// and is designed to be called from a DIFFERENT process than the loop.
//
// The decision only lands on a checkpoint that is still 'pending': approving a
// checkpoint that already resolved would otherwise silently leak into whatever
// the loop does next.
func Decide(db *sql.DB, id string, approve bool) error {
	if err := EnsureSchema(db); err != nil {
		return err
	}
	if err := ValidateID(id); err != nil {
		return err
	}
	decision := approvalDecisionReject
	if approve {
		decision = approvalDecisionAllow
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, err := execRetry(ctx, db,
		`UPDATE loop_approvals SET decision=?, decided_at=? WHERE loop_id=? AND decision='pending'`,
		decision, now(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("No pending approval request for loop '%s'. The loop must be "+
			"running with --approval human and waiting at a checkpoint.", id)
	}
	return nil
}
