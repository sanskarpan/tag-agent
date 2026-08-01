package hitl

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func openToolPause(t *testing.T, db *sql.DB) Pause {
	t.Helper()
	args, sum := CanonicalArgs(map[string]any{"command": "echo hi"})
	p, err := Open(db, Pause{
		Kind: KindToolApproval, SessionID: "run-1", Tool: "bash", Subject: "echo hi",
		Question: "Approve?", ArgsJSON: args, ArgsSHA256: sum,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEnsureSchemaIdempotent: every command calls it, so a second call must be a
// no-op rather than an error on a database that already has the tables.
func TestEnsureSchemaIdempotent(t *testing.T) {
	db := testDB(t)
	for i := 0; i < 3; i++ {
		if err := EnsureSchema(db); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

// TestWaitRefusesUnboundedTimeout is THE anti-hang invariant of this package.
// It must fail loudly, not default to something.
func TestWaitRefusesUnboundedTimeout(t *testing.T) {
	db := testDB(t)
	p := openToolPause(t, db)
	for _, d := range []time.Duration{0, -time.Second} {
		done := make(chan error, 1)
		go func() {
			_, err := Wait(context.Background(), db, p.ID, d, 0)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrUnboundedWait) {
				t.Fatalf("timeout %s: err = %v, want ErrUnboundedWait", d, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("HANG: Wait with timeout %s blocked instead of refusing", d)
		}
	}
}

// TestWaitTimesOutAndAutoDenies: an unanswered pause resolves to timed_out
// within its bound and is audited. Bounded so a regression fails the test rather
// than wedging the suite.
func TestWaitTimesOutAndAutoDenies(t *testing.T) {
	db := testDB(t)
	p := openToolPause(t, db)

	type result struct {
		res WaitResult
		err error
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		r, err := Wait(context.Background(), db, p.ID, 300*time.Millisecond, 20*time.Millisecond)
		ch <- result{r, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.res.Status != StatusTimedOut {
			t.Fatalf("status = %q, want %q", got.res.Status, StatusTimedOut)
		}
		if got.res.Approved() {
			t.Fatal("a timeout must never read as approved")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("timeout took %s, far beyond its 300ms bound", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HANG: a 300ms-bounded wait did not return")
	}

	after, err := Get(db, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusTimedOut || after.Reviewer != "system" {
		t.Errorf("stored pause = %+v, want timed_out by system", after)
	}
	rows, err := AuditLog(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Decision != StatusTimedOut {
		t.Fatalf("audit = %+v, want exactly one timed_out row", rows)
	}
}

// TestWaitResolvedByAnotherProcess: the store IS the channel. Simulate the
// out-of-process approver with a concurrent Resolve.
func TestWaitResolvedByAnotherProcess(t *testing.T) {
	db := testDB(t)
	p := openToolPause(t, db)

	go func() {
		time.Sleep(80 * time.Millisecond)
		_, _ = Resolve(db, p.ID, Decision{Status: StatusApproved, Reviewer: "alice",
			Rationale: "reviewed"})
	}()

	ch := make(chan WaitResult, 1)
	go func() {
		r, err := Wait(context.Background(), db, p.ID, 10*time.Second, 20*time.Millisecond)
		if err != nil {
			t.Error(err)
		}
		ch <- r
	}()
	select {
	case r := <-ch:
		if !r.Approved() {
			t.Fatalf("status = %q, want approved", r.Status)
		}
		if r.Pause.Reviewer != "alice" {
			t.Errorf("reviewer = %q, want alice", r.Pause.Reviewer)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("HANG: an approved pause did not release the waiter")
	}
}

// TestWaitHonoursContextCancellation: SIGTERM during a wait must not strand the
// row in `pending` forever.
func TestWaitHonoursContextCancellation(t *testing.T) {
	db := testDB(t)
	p := openToolPause(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(60 * time.Millisecond); cancel() }()

	ch := make(chan WaitResult, 1)
	go func() {
		r, err := Wait(ctx, db, p.ID, 30*time.Second, 20*time.Millisecond)
		if err != nil {
			t.Error(err)
		}
		ch <- r
	}()
	select {
	case r := <-ch:
		if r.Status != StatusCancelled {
			t.Fatalf("status = %q, want cancelled", r.Status)
		}
		if r.Approved() {
			t.Fatal("a cancellation must never read as approved")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HANG: a cancelled context did not release the waiter")
	}
	after, _ := Get(db, p.ID)
	if after.Status == StatusPending {
		t.Error("the pause was left pending after cancellation")
	}
}

// TestResolveIsOnceOnly: a recorded decision is never overwritten, and exactly
// one audit row exists per decision.
func TestResolveIsOnceOnly(t *testing.T) {
	db := testDB(t)
	p := openToolPause(t, db)

	if _, err := Resolve(db, p.ID, Decision{Status: StatusApproved, Reviewer: "alice"}); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(db, p.ID, Decision{Status: StatusDenied, Reviewer: "mallory"})
	if !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second decision: err = %v, want ErrAlreadyDecided", err)
	}
	if got.Reviewer != "alice" || got.Status != StatusApproved {
		t.Errorf("the standing decision changed: %+v", got)
	}
	rows, _ := AuditLog(db, 10)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(rows))
	}
}

// TestResolveUnknownID distinguishes "no such id" from "already decided"; a
// caller that cannot tell them apart cannot report honestly.
func TestResolveUnknownID(t *testing.T) {
	db := testDB(t)
	if _, err := Resolve(db, "appr_nope", Decision{Status: StatusApproved}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestAuditIsAppendOnlyAtTheDBLayer: the guarantee is enforced by SQLite
// triggers, not by application discipline (PRD-078 G5/NFR-02/AC-08/AC-09).
func TestAuditIsAppendOnlyAtTheDBLayer(t *testing.T) {
	db := testDB(t)
	p := openToolPause(t, db)
	if _, err := Resolve(db, p.ID, Decision{Status: StatusApproved, Reviewer: "alice"}); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`UPDATE hitl_audit SET decision='denied'`)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE err = %v, want an append-only rejection", err)
	}
	_, err = db.Exec(`DELETE FROM hitl_audit`)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE err = %v, want an append-only rejection", err)
	}
}

// TestListNeverNil: --json must render [] and not null.
func TestListNeverNil(t *testing.T) {
	db := testDB(t)
	l, err := List(db, Filter{PendingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if l == nil {
		t.Fatal("List returned nil; --json would emit null instead of []")
	}
	a, err := AuditLog(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatal("AuditLog returned nil")
	}
	s, err := Sessions(db, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("Sessions returned nil")
	}
}

// TestCanonicalArgsHash pins FR-10/AC-13: the hash is over canonical JSON with
// sorted keys, so the reviewer's view and the executed args are checkably equal.
func TestCanonicalArgsHash(t *testing.T) {
	a, sumA := CanonicalArgs(map[string]any{"b": 2, "a": 1})
	b, sumB := CanonicalArgs(map[string]any{"a": 1, "b": 2})
	if a != b || sumA != sumB {
		t.Fatalf("canonicalisation is not order-independent: %q/%s vs %q/%s", a, sumA, b, sumB)
	}
	if a != `{"a":1,"b":2}` {
		t.Errorf("canonical form = %q", a)
	}
	if _, sumEmpty := CanonicalArgs(nil); sumEmpty == "" {
		t.Error("nil args must still hash")
	}
}

// ---- PRD-109 interrupt semantics -------------------------------------------

// TestInterruptNewThenResume walks the whole PRD-109 loop: a new interrupt
// returns ErrInterrupt, and after `resume` the SAME step re-executed returns the
// stored input with no second pause (FR-01/FR-04/FR-05).
func TestInterruptNewThenResume(t *testing.T) {
	db := testDB(t)
	g := &Gate{DB: db, SessionID: "sess-1"}
	req := InterruptRequest{StepID: "step-a", Question: "Delete 47 files?",
		Context: map[string]any{"count": 47}}

	if _, err := Interrupt(context.Background(), g, req); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("first call: err = %v, want ErrInterrupt", err)
	}
	// Re-execution while still pending must NOT open a second row.
	if _, err := Interrupt(context.Background(), g, req); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("re-run while pending: err = %v, want ErrInterrupt", err)
	}
	rows, _ := List(db, Filter{Kind: KindWorkflowInterrupt})
	if len(rows) != 1 {
		t.Fatalf("interrupt rows = %d, want 1 (a re-run must reuse its step row)", len(rows))
	}

	if _, err := Respond(db, rows[0].ID, "yes, proceed", "checked the list"); err != nil {
		t.Fatal(err)
	}
	got, err := Interrupt(context.Background(), g, req)
	if err != nil {
		t.Fatalf("resumed call: %v", err)
	}
	if got != "yes, proceed" {
		t.Errorf("resumed input = %q, want %q", got, "yes, proceed")
	}
}

// TestInterruptDeniedDoesNotSilentlyPass: a refused interrupt must produce an
// error, never an empty-string "answer" that a node would treat as input.
func TestInterruptDeniedDoesNotSilentlyPass(t *testing.T) {
	db := testDB(t)
	g := &Gate{DB: db, SessionID: "sess-2"}
	req := InterruptRequest{StepID: "step-b", Question: "Deploy?"}
	_, _ = Interrupt(context.Background(), g, req)
	rows, _ := List(db, Filter{Kind: KindWorkflowInterrupt, SessionID: "sess-2"})
	if _, err := Resolve(db, rows[0].ID, Decision{Status: StatusDenied, Rationale: "not now"}); err != nil {
		t.Fatal(err)
	}
	got, err := Interrupt(context.Background(), g, req)
	if err == nil {
		t.Fatal("a denied interrupt must not resume cleanly")
	}
	if got != "" {
		t.Errorf("a denied interrupt returned input %q", got)
	}
	if !strings.Contains(err.Error(), "denied") || !strings.Contains(err.Error(), "not now") {
		t.Errorf("the error must carry the decision and rationale: %v", err)
	}
}

// TestInterruptAutoApproveWritesNothing pins FR-06: the CI escape hatch skips
// the checkpoint entirely, so it cannot leave phantom pending rows behind.
func TestInterruptAutoApproveWritesNothing(t *testing.T) {
	db := testDB(t)
	g := &Gate{DB: db, SessionID: "sess-3", AutoApprove: true}
	got, err := Interrupt(context.Background(), g, InterruptRequest{Question: "Proceed?"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "approved" {
		t.Errorf("auto input = %q, want the default %q", got, "approved")
	}
	rows, _ := List(db, Filter{Kind: KindWorkflowInterrupt})
	if len(rows) != 0 {
		t.Errorf("auto-approve wrote %d rows, want 0", len(rows))
	}

	g.AutoInput = "LGTM"
	if got, _ := Interrupt(context.Background(), g, InterruptRequest{Question: "Again?"}); got != "LGTM" {
		t.Errorf("auto input = %q, want LGTM", got)
	}
}

// TestInterruptWithoutStoreFailsClosed: no store means no durable record, and
// returning a fabricated "approved" would be the worst possible default.
func TestInterruptWithoutStoreFailsClosed(t *testing.T) {
	got, err := Interrupt(context.Background(), &Gate{SessionID: "s"}, InterruptRequest{Question: "?"})
	if err == nil {
		t.Fatal("a gate with no store must fail closed")
	}
	if got != "" {
		t.Errorf("returned input %q with no store", got)
	}
	if _, err := Interrupt(context.Background(), nil, InterruptRequest{Question: "?"}); err == nil {
		t.Fatal("a nil gate must fail closed")
	}
}

// TestStepIDDeterministic: the resume short-circuit depends on it.
func TestStepIDDeterministic(t *testing.T) {
	if a, b := StepIDFor("review", 0), StepIDFor("review", 0); a != b {
		t.Fatalf("unstable step id: %s vs %s", a, b)
	}
	if a, b := StepIDFor("review", 0), StepIDFor("review", 1); a == b {
		t.Fatal("different ordinals must give different step ids")
	}
}

// TestSessionsRollup backs `tag workflow list --filter interrupted`.
func TestSessionsRollup(t *testing.T) {
	db := testDB(t)
	g1 := &Gate{DB: db, SessionID: "open"}
	_, _ = Interrupt(context.Background(), g1, InterruptRequest{StepID: "s1", Question: "open question"})
	g2 := &Gate{DB: db, SessionID: "closed"}
	_, _ = Interrupt(context.Background(), g2, InterruptRequest{StepID: "s1", Question: "answered"})
	rows, _ := List(db, Filter{Kind: KindWorkflowInterrupt, SessionID: "closed"})
	_, _ = Respond(db, rows[0].ID, "ok", "")

	all, err := Sessions(db, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("sessions = %d, want 2", len(all))
	}
	only, _ := Sessions(db, true, 10)
	if len(only) != 1 || only[0].SessionID != "open" {
		t.Fatalf("interrupted-only = %+v, want just 'open'", only)
	}
	if only[0].Question != "open question" {
		t.Errorf("rollup must surface the open question, got %q", only[0].Question)
	}
}
