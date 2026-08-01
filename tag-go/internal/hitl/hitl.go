// Package hitl implements ONE human-in-the-loop pause/resume primitive shared by
// two surfaces:
//
//   - PRD-078 tool approval  (`tag permissions pending|approve|deny`): a gated
//     tool CALL parks pre-execution until a human answers approve/deny.
//   - PRD-109 workflow interrupt (`tag workflow interrupt|resume`): an arbitrary
//     workflow STEP parks until a human supplies free-text input.
//
// They are deliberately not two mechanisms. Both are "publish a pending row,
// wait out-of-process for a human, resume with the answer"; they differ only in
// the payload (binary verdict vs. free text) and the subject (a tool call vs. a
// named step). So both live in one table, share one bounded wait, and share one
// append-only audit log. The third HITL surface in the tree — `loop start
// --approval human` (PRD-021) — is NOT folded in here: it shipped with its own
// loop_approvals table and iteration-gate semantics, it gates the next
// ITERATION rather than a step or a call, and rewriting a just-released
// contract to save one table is a bad trade. internal/cli/workflow.go carries
// the three-way comparison.
//
// THE HARD RULE OF THIS PACKAGE: a wait is always BOUNDED. Wait refuses a
// non-positive timeout, and on expiry it resolves the pause to `timed_out` and
// returns. There is no "wait forever" mode, because a background run that blocks
// on a human that will never come is the exact silent hang this project bars.
// PRD-078 §10.2 specifies "timeout_seconds NULL = wait forever"; that is
// deliberately NOT implemented (documented divergence).
package hitl

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Kinds of pause. The kind selects the CLI facade, not the mechanism.
const (
	// KindToolApproval is a PRD-078 tool-call approval (binary answer).
	KindToolApproval = "tool_approval"
	// KindWorkflowInterrupt is a PRD-109 workflow interrupt (free-text answer).
	KindWorkflowInterrupt = "workflow_interrupt"
)

// Statuses a pause can hold. Only `pending` is non-terminal.
const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusDenied    = "denied"
	StatusTimedOut  = "timed_out"
	StatusCancelled = "cancelled"
)

// Terminal reports whether a status means the pause is resolved for good.
func Terminal(status string) bool { return status != StatusPending }

// ErrNotFound is returned when a pause id is unknown.
var ErrNotFound = errors.New("pause not found")

// ErrAlreadyDecided is returned when a decision lands on a resolved pause. A
// second decision must never silently overwrite the first: the audit trail's
// whole value is that each row corresponds to exactly one human act.
var ErrAlreadyDecided = errors.New("pause already decided")

// DefaultPoll is the store poll cadence for Wait.
const DefaultPoll = 200 * time.Millisecond

// DefaultTimeout is the bounded wait used when a caller does not choose one.
const DefaultTimeout = 300 * time.Second

// schema is applied lazily and idempotently from this package rather than added
// to internal/store/migrate/schema.sql, following internal/loop's precedent:
// schema.sql is shared with other in-flight ports, and CREATE ... IF NOT EXISTS
// here is self-healing on a pre-existing database and conflict-free.
//
// hitl_audit carries BEFORE UPDATE / BEFORE DELETE triggers so the audit trail
// is append-only at the DATABASE layer, not merely by application convention
// (PRD-078 G5 / NFR-02 / AC-08 / AC-09).
const schema = `
CREATE TABLE IF NOT EXISTS hitl_pauses (
  id            TEXT PRIMARY KEY,
  kind          TEXT NOT NULL,
  session_id    TEXT NOT NULL DEFAULT '',
  step_id       TEXT NOT NULL DEFAULT '',
  tool          TEXT NOT NULL DEFAULT '',
  subject       TEXT NOT NULL DEFAULT '',
  question      TEXT NOT NULL DEFAULT '',
  context_json  TEXT NOT NULL DEFAULT '{}',
  args_json     TEXT NOT NULL DEFAULT '{}',
  args_sha256   TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL DEFAULT 'pending',
  response      TEXT NOT NULL DEFAULT '',
  reviewer      TEXT NOT NULL DEFAULT '',
  reviewer_src  TEXT NOT NULL DEFAULT '',
  rationale     TEXT NOT NULL DEFAULT '',
  timeout_s     INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL,
  decided_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_hitl_status  ON hitl_pauses(status, created_at);
CREATE INDEX IF NOT EXISTS idx_hitl_session ON hitl_pauses(session_id, kind, status);
-- A workflow interrupt is keyed by (session, step) so a node re-executed after
-- "workflow resume" finds ITS row and short-circuits (PRD-109 FR-04/FR-05)
-- instead of opening a second pause. Tool approvals are intentionally excluded:
-- the same command may legitimately be requested twice in one run.
CREATE UNIQUE INDEX IF NOT EXISTS idx_hitl_step ON hitl_pauses(session_id, step_id)
  WHERE kind = 'workflow_interrupt' AND step_id <> '';

CREATE TABLE IF NOT EXISTS hitl_audit (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  pause_id        TEXT NOT NULL,
  kind            TEXT NOT NULL,
  session_id      TEXT NOT NULL DEFAULT '',
  tool            TEXT NOT NULL DEFAULT '',
  subject         TEXT NOT NULL DEFAULT '',
  args_sha256     TEXT NOT NULL DEFAULT '',
  decision        TEXT NOT NULL,
  reviewer        TEXT NOT NULL DEFAULT '',
  reviewer_source TEXT NOT NULL DEFAULT 'cli',
  rationale       TEXT NOT NULL DEFAULT '',
  response        TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL,
  decided_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hitl_audit_at ON hitl_audit(decided_at DESC);

CREATE TRIGGER IF NOT EXISTS trg_hitl_audit_no_update
  BEFORE UPDATE ON hitl_audit
BEGIN
  SELECT RAISE(FAIL, 'hitl_audit is append-only: UPDATE is not permitted');
END;
CREATE TRIGGER IF NOT EXISTS trg_hitl_audit_no_delete
  BEFORE DELETE ON hitl_audit
BEGIN
  SELECT RAISE(FAIL, 'hitl_audit is append-only: DELETE is not permitted');
END;
`

// EnsureSchema creates the HITL tables if absent. Idempotent; safe on every
// command so the CLI works on a database that predates this feature.
func EnsureSchema(db *sql.DB) error {
	if db == nil {
		return errors.New("hitl: no database handle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := execRetry(ctx, db, schema)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "database is locked") ||
		strings.Contains(m, "sqlite_busy") ||
		strings.Contains(m, "database table is locked")
}

// execRetry runs a write, retrying while SQLite reports the database busy. The
// cross-process shape here is the same as internal/loop's: the waiting run holds
// a steady poll while `tag permissions approve` writes from another shell, and a
// decision that fails with "database is locked" is a decision that silently did
// not happen.
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

// Pause is one parked decision point.
type Pause struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	SessionID   string `json:"session_id"`
	StepID      string `json:"step_id,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Question    string `json:"question"`
	ContextJSON string `json:"context_json,omitempty"`
	ArgsJSON    string `json:"args_json,omitempty"`
	ArgsSHA256  string `json:"args_sha256,omitempty"`
	Status      string `json:"status"`
	Response    string `json:"response,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	ReviewerSrc string `json:"reviewer_source,omitempty"`
	Rationale   string `json:"rationale,omitempty"`
	TimeoutS    int    `json:"timeout_s"`
	CreatedAt   string `json:"created_at"`
	DecidedAt   string `json:"decided_at,omitempty"`
}

// NewID mints a prefixed, greppable pause id.
func NewID(kind string) string {
	prefix := "pause"
	switch kind {
	case KindToolApproval:
		prefix = "appr"
	case KindWorkflowInterrupt:
		prefix = "intr"
	}
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
}

// CanonicalArgs renders tool arguments as canonical JSON (Go sorts map keys) and
// returns the text plus its SHA-256. The hash is what makes "the arguments the
// reviewer saw are the arguments that ran" checkable after the fact
// (PRD-078 FR-10/FR-11/AC-13).
func CanonicalArgs(args map[string]any) (string, string) {
	if args == nil {
		args = map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		// Unmarshalable args must not silently become "{}" — that would hash a
		// payload nobody reviewed. Record the failure verbatim so the reviewer
		// sees it and the hash covers the real stored text.
		b = []byte(fmt.Sprintf("{\"__unserializable__\":%q}", err.Error()))
	}
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:])
}

// Open records a new pending pause and returns it. The caller supplies the id
// via NewID or leaves it empty for one to be minted.
func Open(db *sql.DB, p Pause) (Pause, error) {
	if err := EnsureSchema(db); err != nil {
		return Pause{}, err
	}
	if p.ID == "" {
		p.ID = NewID(p.Kind)
	}
	if p.Kind == "" {
		return Pause{}, errors.New("hitl: pause kind is required")
	}
	if p.ContextJSON == "" {
		p.ContextJSON = "{}"
	}
	if p.ArgsJSON == "" {
		p.ArgsJSON = "{}"
	}
	p.Status = StatusPending
	p.CreatedAt = now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := execRetry(ctx, db,
		`INSERT INTO hitl_pauses(id, kind, session_id, step_id, tool, subject, question,
		   context_json, args_json, args_sha256, status, timeout_s, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Kind, p.SessionID, p.StepID, p.Tool, p.Subject, p.Question,
		p.ContextJSON, p.ArgsJSON, p.ArgsSHA256, StatusPending, p.TimeoutS, p.CreatedAt)
	if err != nil {
		return Pause{}, err
	}
	return p, nil
}

const selectCols = `id, kind, session_id, step_id, tool, subject, question, context_json,
	args_json, args_sha256, status, response, reviewer, reviewer_src, rationale,
	timeout_s, created_at, COALESCE(decided_at,'')`

func scanPause(sc interface{ Scan(...any) error }) (Pause, error) {
	var p Pause
	err := sc.Scan(&p.ID, &p.Kind, &p.SessionID, &p.StepID, &p.Tool, &p.Subject, &p.Question,
		&p.ContextJSON, &p.ArgsJSON, &p.ArgsSHA256, &p.Status, &p.Response, &p.Reviewer,
		&p.ReviewerSrc, &p.Rationale, &p.TimeoutS, &p.CreatedAt, &p.DecidedAt)
	return p, err
}

// Get reads one pause by id.
func Get(db *sql.DB, id string) (Pause, error) {
	if err := EnsureSchema(db); err != nil {
		return Pause{}, err
	}
	p, err := scanPause(db.QueryRow(`SELECT `+selectCols+` FROM hitl_pauses WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Pause{}, ErrNotFound
	}
	return p, err
}

// Filter narrows a List query. A zero Filter lists everything.
type Filter struct {
	Kind      string
	SessionID string
	Tool      string
	// PendingOnly restricts to unresolved pauses.
	PendingOnly bool
	// Limit caps rows; <= 0 uses 50.
	Limit int
}

// List returns pauses newest-first. The slice is NEVER nil, so a --json consumer
// sees [] rather than null.
func List(db *sql.DB, f Filter) ([]Pause, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	q := `SELECT ` + selectCols + ` FROM hitl_pauses WHERE 1=1`
	var args []any
	if f.Kind != "" {
		q += ` AND kind=?`
		args = append(args, f.Kind)
	}
	if f.SessionID != "" {
		q += ` AND session_id=?`
		args = append(args, f.SessionID)
	}
	if f.Tool != "" {
		q += ` AND tool=?`
		args = append(args, f.Tool)
	}
	if f.PendingOnly {
		q += ` AND status='pending'`
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pause{}
	for rows.Next() {
		p, err := scanPause(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FindStep returns the pause for a (session, step) workflow interrupt, if any.
// This is the resume short-circuit: a re-executed node looks itself up here.
func FindStep(db *sql.DB, sessionID, stepID string) (Pause, error) {
	if err := EnsureSchema(db); err != nil {
		return Pause{}, err
	}
	p, err := scanPause(db.QueryRow(`SELECT `+selectCols+` FROM hitl_pauses
		WHERE kind=? AND session_id=? AND step_id=?`,
		KindWorkflowInterrupt, sessionID, stepID))
	if errors.Is(err, sql.ErrNoRows) {
		return Pause{}, ErrNotFound
	}
	return p, err
}

// Decision is the human's answer.
type Decision struct {
	// Status is one of StatusApproved / StatusDenied / StatusTimedOut / StatusCancelled.
	Status string
	// Response is the operator's free text (PRD-109 --input). Empty for a plain
	// approve/deny.
	Response string
	// Reviewer identifies the human; defaults to the OS username.
	Reviewer string
	// Source is 'cli', 'auto', or 'system'.
	Source string
	// Rationale is a free-text justification, persisted verbatim.
	Rationale string
}

// CurrentUser returns the OS username used as the default reviewer identity
// (PRD-078 FR-09). It never fails the caller: an unresolvable user is recorded
// as "unknown" rather than blocking a decision.
func CurrentUser() string {
	u, err := user.Current()
	if err != nil || u == nil || strings.TrimSpace(u.Username) == "" {
		return "unknown"
	}
	return u.Username
}

// Resolve records a decision on a PENDING pause and appends exactly one audit
// row. It is designed to run in a DIFFERENT process from the waiter; the store
// is the whole channel.
//
// The UPDATE is guarded by `status='pending'` with a row-count assertion, so two
// concurrent approvals cannot both "win" and cannot produce two audit rows
// (PRD-078 FR-08/AC-07, NFR-04).
func Resolve(db *sql.DB, id string, d Decision) (Pause, error) {
	if err := EnsureSchema(db); err != nil {
		return Pause{}, err
	}
	if !Terminal(d.Status) {
		return Pause{}, fmt.Errorf("hitl: %q is not a terminal decision status", d.Status)
	}
	if d.Reviewer == "" {
		d.Reviewer = CurrentUser()
	}
	if d.Source == "" {
		d.Source = "cli"
	}
	// Existence is checked first so "unknown id" and "already decided" are
	// distinguishable to the caller (they map to different messages, and a
	// caller that cannot tell them apart cannot report honestly).
	cur, err := Get(db, id)
	if err != nil {
		return Pause{}, err
	}

	ts := now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := execRetry(ctx, db,
		`UPDATE hitl_pauses SET status=?, response=?, reviewer=?, reviewer_src=?,
		   rationale=?, decided_at=? WHERE id=? AND status='pending'`,
		d.Status, d.Response, d.Reviewer, d.Source, d.Rationale, ts, id)
	if err != nil {
		return Pause{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return cur, ErrAlreadyDecided
	}

	// Audit append. Unlike permission.SQLRecorder (best-effort, because losing a
	// telemetry row must never fail a security decision), THIS write is the
	// decision's only durable evidence, so a failure is reported. The pause is
	// already resolved either way, which is why the error is returned alongside
	// the updated row rather than instead of it.
	_, aerr := execRetry(ctx, db,
		`INSERT INTO hitl_audit(pause_id, kind, session_id, tool, subject, args_sha256,
		   decision, reviewer, reviewer_source, rationale, response, created_at, decided_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		cur.ID, cur.Kind, cur.SessionID, cur.Tool, cur.Subject, cur.ArgsSHA256,
		d.Status, d.Reviewer, d.Source, d.Rationale, d.Response, cur.CreatedAt, ts)

	out, gerr := Get(db, id)
	if gerr != nil {
		return Pause{}, gerr
	}
	if aerr != nil {
		return out, fmt.Errorf("decision recorded but the audit row could not be written: %w", aerr)
	}
	return out, nil
}

// Audit is one append-only decision record.
type Audit struct {
	ID         int64  `json:"id"`
	PauseID    string `json:"pause_id"`
	Kind       string `json:"kind"`
	SessionID  string `json:"session_id,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Subject    string `json:"subject,omitempty"`
	ArgsSHA256 string `json:"args_sha256,omitempty"`
	Decision   string `json:"decision"`
	Reviewer   string `json:"reviewer"`
	Source     string `json:"reviewer_source"`
	Rationale  string `json:"rationale,omitempty"`
	Response   string `json:"response,omitempty"`
	CreatedAt  string `json:"created_at"`
	DecidedAt  string `json:"decided_at"`
}

// AuditLog returns the append-only decision log, newest first. Never nil.
func AuditLog(db *sql.DB, limit int) ([]Audit, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`SELECT id, pause_id, kind, session_id, tool, subject, args_sha256,
		decision, reviewer, reviewer_source, rationale, response, created_at, decided_at
		FROM hitl_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Audit{}
	for rows.Next() {
		var a Audit
		if err := rows.Scan(&a.ID, &a.PauseID, &a.Kind, &a.SessionID, &a.Tool, &a.Subject,
			&a.ArgsSHA256, &a.Decision, &a.Reviewer, &a.Source, &a.Rationale, &a.Response,
			&a.CreatedAt, &a.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// WaitResult is the outcome of a bounded wait.
type WaitResult struct {
	Pause   Pause
	Status  string
	Elapsed time.Duration
}

// Approved reports whether the human let the work proceed.
func (w WaitResult) Approved() bool { return w.Status == StatusApproved }

// ErrUnboundedWait is returned when a caller asks for a wait with no deadline.
// It is a PROGRAMMING error, surfaced loudly rather than defaulted, because
// defaulting it is how an unbounded human gate sneaks into a background run.
var ErrUnboundedWait = errors.New("hitl: a pause wait requires a positive timeout " +
	"(an unbounded human gate is forbidden — it would hang a background run forever)")

// Wait blocks until the pause is resolved out-of-process, the timeout expires,
// or ctx is cancelled. It NEVER reads stdin, and it is ALWAYS bounded.
//
// Outcomes:
//   - decided elsewhere -> that status
//   - timeout           -> the pause is atomically flipped to `timed_out`, audited,
//     and StatusTimedOut is returned (auto-deny: PRD-078 G8/FR-07)
//   - ctx cancelled     -> the pause is flipped to `cancelled` and audited, so a
//     SIGTERMed run does not strand a row in `pending` forever
func Wait(ctx context.Context, db *sql.DB, id string, timeout, poll time.Duration) (WaitResult, error) {
	if timeout <= 0 {
		return WaitResult{}, ErrUnboundedWait
	}
	if poll <= 0 {
		poll = DefaultPoll
	}
	if err := EnsureSchema(db); err != nil {
		return WaitResult{}, err
	}
	start := time.Now()
	deadline := start.Add(timeout)
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		p, err := Get(db, id)
		if err != nil && !isBusy(err) {
			return WaitResult{}, err
		}
		if err == nil && Terminal(p.Status) {
			return WaitResult{Pause: p, Status: p.Status, Elapsed: time.Since(start)}, nil
		}
		if !time.Now().Before(deadline) {
			p, rerr := systemResolve(db, id, StatusTimedOut,
				fmt.Sprintf("no decision within %s", timeout))
			return WaitResult{Pause: p, Status: p.Status, Elapsed: time.Since(start)}, rerr
		}
		select {
		case <-ctx.Done():
			p, rerr := systemResolve(db, id, StatusCancelled, "run interrupted before a decision arrived")
			return WaitResult{Pause: p, Status: p.Status, Elapsed: time.Since(start)}, rerr
		case <-t.C:
		}
	}
}

// systemResolve records a non-human terminal outcome (timeout / cancellation).
// If the pause was decided in the same instant the deadline fired, the human's
// decision wins and is returned unchanged.
func systemResolve(db *sql.DB, id, status, rationale string) (Pause, error) {
	p, err := Resolve(db, id, Decision{
		Status: status, Reviewer: "system", Source: "auto", Rationale: rationale,
	})
	if errors.Is(err, ErrAlreadyDecided) {
		return p, nil
	}
	return p, err
}
