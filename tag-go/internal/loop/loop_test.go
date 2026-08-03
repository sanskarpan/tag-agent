package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "tag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureSchema(db.DB); err != nil {
		t.Fatal(err)
	}
	return db
}

// scriptProvider replays a fixed sequence of turns. Turn i emits scripted[i]:
// either plain text, or a tool call when ToolName is set. It performs NO network
// I/O, so the whole suite is offline.
type scriptProvider struct {
	mu    sync.Mutex
	turns []scriptTurn
	// cycle replays the script from the top instead of sticking on the last
	// turn, so a multi-ITERATION run repeats the same behaviour each pass.
	cycle bool
	n     int
	seen  []llm.Request
}

type scriptTurn struct {
	text     string
	toolName string
	toolArgs map[string]any
}

func (p *scriptProvider) Name() string { return "script" }

func (p *scriptProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.mu.Lock()
	i := p.n
	p.n++
	p.seen = append(p.seen, req)
	var turn scriptTurn
	switch {
	case len(p.turns) == 0:
	case p.cycle:
		turn = p.turns[i%len(p.turns)]
	case i < len(p.turns):
		turn = p.turns[i]
	default:
		turn = p.turns[len(p.turns)-1]
	}
	p.mu.Unlock()

	ch := make(chan llm.Event, 4)
	go func() {
		defer close(ch)
		if turn.toolName != "" {
			ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{
				ID: "call-1", Name: turn.toolName, Input: turn.toolArgs}}
		}
		if turn.text != "" {
			ch <- llm.Event{Type: llm.EventTextDelta, Text: turn.text}
		}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

// ---------------------------------------------------------------------------
// Lifecycle persistence: list/status read what start wrote.
// ---------------------------------------------------------------------------

func TestLifecyclePersistsAcrossConnections(t *testing.T) {
	db := testDB(t)
	id, err := Create(db.DB, "orchestrator", "ship it", 3, ApprovalAuto)
	if err != nil {
		t.Fatal(err)
	}
	// Re-open the same file with a SECOND connection, the way a second `tag`
	// process would: list/status must see the row.
	other, err := store.OpenPath(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	runs, err := List(other.DB, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != id || runs[0].Status != StatusRunning {
		t.Fatalf("List from second connection = %+v", runs)
	}
	rec, err := Get(other.DB, id)
	if err != nil || rec.Goal != "ship it" || rec.MaxIters != 3 {
		t.Fatalf("Get = %+v err=%v", rec, err)
	}
	if _, err := Get(other.DB, "nope"); err != ErrNotFound {
		t.Fatalf("Get(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestListIsEmptySliceNotNil(t *testing.T) {
	db := testDB(t)
	runs, err := List(db.DB, 0)
	if err != nil {
		t.Fatal(err)
	}
	if runs == nil {
		t.Fatal("List returned nil; --json would render null instead of []")
	}
	if len(runs) != 0 {
		t.Fatalf("List = %v, want empty", runs)
	}
}

// ---------------------------------------------------------------------------
// Offline echo path.
// ---------------------------------------------------------------------------

func TestRunEchoReachesMaxIters(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 3, ApprovalAuto)
	out, err := Run(context.Background(), db.DB, id, Options{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusMaxIters || out.Iterations != 3 {
		t.Fatalf("outcome = %+v, want max_iters after 3", out)
	}
	if !out.Degraded || !strings.Contains(out.Note, "OFFLINE") {
		t.Errorf("echo run must be reported as degraded, got %+v", out)
	}
	iters, _ := Iterations(db.DB, id)
	if len(iters) != 3 {
		t.Fatalf("journalled %d iterations, want 3", len(iters))
	}
	rec, _ := Get(db.DB, id)
	if rec.CurrentIter != 3 || rec.CompletedAt == "" {
		t.Errorf("run row = %+v", rec)
	}
}

// TestEchoDoesNotFakeGoalAchieved is the no-fake-success guard. The prompt
// literally contains "GOAL_ACHIEVED" ("Output GOAL_ACHIEVED when done"), so
// Python's bare substring check on the echoed output declares victory on
// iteration 1 without any work happening.
func TestEchoDoesNotFakeGoalAchieved(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 2, ApprovalAuto)
	out, err := Run(context.Background(), db.DB, id, Options{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status == StatusCompleted {
		t.Fatal("echo provider reported goal_achieved: an echoed instruction was mistaken for success")
	}
}

func TestGoalAchievedDetection(t *testing.T) {
	prompt := buildPrompt("g", 1, "")
	if goalAchieved(prompt, prompt) {
		t.Error("a verbatim echo of the prompt must not count as success")
	}
	if !goalAchieved(prompt, prompt+"\nGOAL_ACHIEVED") {
		t.Error("a real declaration after an echoed prompt must count")
	}
	if !goalAchieved(prompt, "all done: GOAL_ACHIEVED") {
		t.Error("a bare declaration must count")
	}
}

func TestRunStopsOnGoalAchieved(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 5, ApprovalAuto)
	prov := &scriptProvider{turns: []scriptTurn{{text: "working"}, {text: "GOAL_ACHIEVED"}}}
	out, err := Run(context.Background(), db.DB, id, Options{Provider: prov, WorkRoot: t.TempDir(), MaxSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusCompleted || out.Iterations != 2 {
		t.Fatalf("outcome = %+v, want completed after 2", out)
	}
	iters, _ := Iterations(db.DB, id)
	if iters[1].Decision != DecisionGoalAchieved {
		t.Errorf("iteration 2 decision = %q", iters[1].Decision)
	}
}

// ---------------------------------------------------------------------------
// Cross-process abort.
// ---------------------------------------------------------------------------

func TestAbortFromAnotherConnectionStopsLiveLoop(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 100000, ApprovalAuto)

	other, err := store.OpenPath(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	done := make(chan Outcome, 1)
	go func() {
		out, rerr := Run(context.Background(), db.DB, id, Options{
			WorkRoot: t.TempDir(), AbortPoll: 20 * time.Millisecond})
		if rerr != nil {
			t.Error(rerr)
		}
		done <- out
	}()

	// Wait until the loop is demonstrably running, then abort over the OTHER
	// connection (the stand-in for `tag loop abort` in a second process).
	waitFor(t, 5*time.Second, func() bool {
		r, e := Get(other.DB, id)
		return e == nil && r.CurrentIter > 0
	})
	ok, err := Abort(other.DB, id)
	if err != nil || !ok {
		t.Fatalf("Abort = %v %v", ok, err)
	}

	select {
	case out := <-done:
		if out.Status != StatusAborted {
			t.Fatalf("outcome status = %q, want aborted", out.Status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("loop did not stop after an out-of-process abort")
	}
	rec, _ := Get(other.DB, id)
	if rec.Status != StatusAborted {
		t.Fatalf("stored status = %q, want aborted", rec.Status)
	}
}

func TestAbortOnTerminalLoopReportsNoMatch(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "g", 1, ApprovalAuto)
	if _, err := Run(context.Background(), db.DB, id, Options{WorkRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	ok, err := Abort(db.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("aborting an already-terminal loop must report no running loop")
	}
}

// ---------------------------------------------------------------------------
// SIGTERM / cancellation must never strand a loop in 'running'.
// ---------------------------------------------------------------------------

func TestCancelledContextLeavesNothingRunning(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 100000, ApprovalAuto)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Outcome, 1)
	go func() {
		out, _ := Run(ctx, db.DB, id, Options{WorkRoot: t.TempDir(), AbortPoll: 20 * time.Millisecond})
		done <- out
	}()
	waitFor(t, 5*time.Second, func() bool {
		r, e := Get(db.DB, id)
		return e == nil && r.CurrentIter > 0
	})
	cancel() // stand-in for SIGTERM, which the CLI turns into exactly this

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("loop did not stop on context cancellation")
	}
	rec, _ := Get(db.DB, id)
	if rec.Status == StatusRunning {
		t.Fatal("loop stranded in 'running' after cancellation (#574)")
	}
	if !IsTerminal(rec.Status) {
		t.Fatalf("status = %q, want terminal", rec.Status)
	}
}

func TestCancelDuringApprovalWaitLeavesNothingRunning(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 5, ApprovalHuman)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, db.DB, id, Options{
			WorkRoot: t.TempDir(), ApprovalTimeout: time.Minute,
			ApprovalPoll: 20 * time.Millisecond, AbortPoll: 20 * time.Millisecond})
	}()
	waitFor(t, 5*time.Second, func() bool {
		s, e := Status(db.DB, id)
		return e == nil && s == StatusWaitingApproval
	})
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("loop hung in the approval wait after cancellation")
	}
	rec, _ := Get(db.DB, id)
	if !IsTerminal(rec.Status) {
		t.Fatalf("status = %q, want terminal", rec.Status)
	}
}

// ---------------------------------------------------------------------------
// Human-in-the-loop approval gate.
// ---------------------------------------------------------------------------

func TestApproveFromAnotherConnectionResumesLoop(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 3, ApprovalHuman)
	other, err := store.OpenPath(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	done := make(chan Outcome, 1)
	go func() {
		out, rerr := Run(context.Background(), db.DB, id, Options{
			WorkRoot: t.TempDir(), ApprovalTimeout: 20 * time.Second,
			ApprovalPoll: 20 * time.Millisecond, AbortPoll: 20 * time.Millisecond})
		if rerr != nil {
			t.Error(rerr)
		}
		done <- out
	}()

	// Two checkpoints (iterations 1 and 2); the last iteration is never gated.
	for want := 1; want <= 2; want++ {
		waitFor(t, 10*time.Second, func() bool {
			a, e := PendingApproval(other.DB, id)
			return e == nil && a != nil && a.Decision == "pending" && a.Iteration == want
		})
		s, _ := Status(other.DB, id)
		if s != StatusWaitingApproval {
			t.Fatalf("status while gated = %q, want waiting_approval", s)
		}
		if err := Decide(other.DB, id, true); err != nil {
			t.Fatalf("approve from other connection: %v", err)
		}
	}
	select {
	case out := <-done:
		if out.Status != StatusMaxIters || out.Iterations != 3 {
			t.Fatalf("outcome = %+v, want all 3 iterations to run", out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("approved loop never finished")
	}
}

func TestDenyFromAnotherConnectionStopsLoop(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 5, ApprovalHuman)
	other, err := store.OpenPath(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	done := make(chan Outcome, 1)
	go func() {
		out, _ := Run(context.Background(), db.DB, id, Options{
			WorkRoot: t.TempDir(), ApprovalTimeout: 20 * time.Second,
			ApprovalPoll: 20 * time.Millisecond, AbortPoll: 20 * time.Millisecond})
		done <- out
	}()
	waitFor(t, 10*time.Second, func() bool {
		a, e := PendingApproval(other.DB, id)
		return e == nil && a != nil && a.Decision == "pending"
	})
	if err := Decide(other.DB, id, false); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.Status != StatusAborted {
			t.Fatalf("outcome = %+v, want aborted after deny", out)
		}
		if out.Iterations != 1 {
			t.Fatalf("denied loop ran %d iterations, want 1", out.Iterations)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("denied loop never stopped")
	}
}

// TestApprovalWaitNeverBlocksOnATerminal is the headless bar: the gate is
// resolved through the store on a bounded timeout, never by reading stdin. A
// loop with nobody to answer must DENY and stop, not hang.
func TestApprovalTimesOutRatherThanHanging(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 5, ApprovalHuman)
	start := time.Now()
	out, err := Run(context.Background(), db.DB, id, Options{
		WorkRoot: t.TempDir(), ApprovalTimeout: 300 * time.Millisecond,
		ApprovalPoll: 20 * time.Millisecond, AbortPoll: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("approval wait took %s — that is a hang, not a timeout", elapsed)
	}
	if out.Status != StatusAborted {
		t.Fatalf("status = %q, want aborted on approval timeout", out.Status)
	}
	if !strings.Contains(out.Note, "timed out") {
		t.Errorf("timeout must be reported plainly, got note %q", out.Note)
	}
}

func TestAbortWhileAwaitingApprovalBreaksTheWait(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 5, ApprovalHuman)
	other, err := store.OpenPath(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	done := make(chan Outcome, 1)
	go func() {
		out, _ := Run(context.Background(), db.DB, id, Options{
			WorkRoot: t.TempDir(), ApprovalTimeout: time.Minute,
			ApprovalPoll: 20 * time.Millisecond, AbortPoll: 20 * time.Millisecond})
		done <- out
	}()
	waitFor(t, 10*time.Second, func() bool {
		s, e := Status(other.DB, id)
		return e == nil && s == StatusWaitingApproval
	})
	// Python aborts only status='running', so this used to be a no-op and the
	// loop sat out the full timeout.
	ok, err := Abort(other.DB, id)
	if err != nil || !ok {
		t.Fatalf("Abort while gated = %v %v", ok, err)
	}
	select {
	case out := <-done:
		if out.Status != StatusAborted {
			t.Fatalf("status = %q", out.Status)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("abort did not break the approval wait")
	}
}

func TestDecideRequiresAPendingCheckpoint(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "g", 1, ApprovalHuman)
	err := Decide(db.DB, id, true)
	if err == nil || !strings.Contains(err.Error(), "No pending approval request") {
		t.Fatalf("Decide with no checkpoint err = %v", err)
	}
}

func TestDecideRejectsTraversalID(t *testing.T) {
	db := testDB(t)
	if err := Decide(db.DB, "../pwned", true); err == nil ||
		!strings.Contains(err.Error(), "Invalid loop id") {
		t.Fatalf("Decide('../pwned') err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Permission gate + per-iteration working directories.
// ---------------------------------------------------------------------------

// TestToolCallIsRoutedThroughTheGuard proves loop-invoked tools go through the
// consent gate: a deny rule must turn into a refusal in the iteration output
// rather than a performed write.
func TestToolCallIsRoutedThroughTheGuard(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 1, ApprovalAuto)
	workRoot := t.TempDir()
	prov := &scriptProvider{turns: []scriptTurn{
		{toolName: "write_file", toolArgs: map[string]any{"path": "out.txt", "content": "x"}},
		{text: "stopped"},
	}}
	pol := permission.DefaultPolicy()
	pol.Rules = append([]permission.Rule{{
		Tool: "write_file", Kind: permission.KindPath, Pattern: "*",
		Action: permission.Deny, Source: "test",
	}}, pol.Rules...)
	// No prompter: exactly how the CLI builds a loop guard.
	guard := permission.NewGuard(pol, nil, nil)

	out, err := Run(context.Background(), db.DB, id, Options{
		Provider: prov, WithTools: true, Guard: guard, WorkRoot: workRoot, MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusMaxIters {
		t.Fatalf("status = %q", out.Status)
	}
	if _, statErr := os.Stat(filepath.Join(workRoot, "loop-"+id, "iter-1", "out.txt")); statErr == nil {
		t.Fatal("denied write_file still created the file — the guard was bypassed")
	}
}

// TestNilGuardIsFailClosed: forgetting to wire a Guard must not open the gate.
func TestNilGuardIsFailClosed(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 1, ApprovalAuto)
	workRoot := t.TempDir()
	prov := &scriptProvider{turns: []scriptTurn{
		{toolName: "bash", toolArgs: map[string]any{"command": "echo pwned > /tmp/tag-loop-pwned"}},
		{text: "done"},
	}}
	if _, err := Run(context.Background(), db.DB, id, Options{
		Provider: prov, WithTools: true, Guard: nil, WorkRoot: workRoot, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat("/tmp/tag-loop-pwned"); statErr == nil {
		os.Remove("/tmp/tag-loop-pwned")
		t.Fatal("a nil Guard executed bash: the gate must fail CLOSED")
	}
}

// TestIterationsGetPrivateWorkDirs is the #591 guard for loops: two iterations
// writing the same filename must not clobber each other.
func TestIterationsGetPrivateWorkDirs(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 2, ApprovalAuto)
	workRoot := t.TempDir()
	prov := &scriptProvider{cycle: true, turns: []scriptTurn{
		{toolName: "write_file", toolArgs: map[string]any{
			"path": "shared.txt", "content": "iteration-content"}},
		{text: "ok"},
	}}

	if _, err := Run(context.Background(), db.DB, id, Options{
		Provider: prov, WithTools: true, Guard: permission.UnsafeAllowAllGuard(),
		WorkRoot: workRoot, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		p := filepath.Join(workRoot, "loop-"+id, fmt.Sprintf("iter-%d", i), "shared.txt")
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("iteration %d has no private artifact at %s: %v", i, p, err)
		}
	}
}

func TestIterWorkDirCannotEscapeWorkRoot(t *testing.T) {
	root := t.TempDir()
	dir, cleanup, err := iterWorkDir(root, "../../etc", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(root)) {
		t.Fatalf("work dir %q escaped work root %q", dir, root)
	}
}

// ---------------------------------------------------------------------------
// Schema self-healing.
// ---------------------------------------------------------------------------

func TestEnsureSchemaIsIdempotentOnAPreexistingDB(t *testing.T) {
	db := testDB(t)
	for i := 0; i < 3; i++ {
		if err := EnsureSchema(db.DB); err != nil {
			t.Fatalf("EnsureSchema pass %d: %v", i, err)
		}
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
		 ('loop_runs','loop_iterations','loop_approvals')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("created %d loop tables, want 3", n)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", limit)
}

// TestApprovalCheckpointIsAtomic pins the fix for a CI flake that was reporting
// a real inconsistency.
//
// The pending-approval row and the waiting_approval status used to be two
// statements, so between them another connection saw a gated loop still
// reporting `running`. An observer polling status to answer "is this gated?"
// got the wrong answer, and the test that caught it looked merely flaky:
//
//	loop_test.go:362: status while gated = "running", want waiting_approval
//
// The window is only observable by a CONCURRENT reader on a SECOND connection —
// reading after the write returns sees both halves and proves nothing, which is
// how the first version of this test managed to pass against the broken code.
func TestApprovalCheckpointIsAtomic(t *testing.T) {
	db := testDB(t)
	id, _ := Create(db.DB, "p", "goal", 1, ApprovalHuman)
	other, err := store.OpenPath(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	stop := make(chan struct{})
	bad := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			a, aerr := PendingApproval(other.DB, id)
			s, serr := Status(other.DB, id)
			if aerr == nil && serr == nil && a != nil && a.Decision == "pending" &&
				s != StatusWaitingApproval {
				select {
				case bad <- s:
				default:
				}
				return
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if err := openApprovalCheckpoint(context.Background(), db.DB, id, i+1, "preview"); err != nil {
			close(stop)
			t.Fatal(err)
		}
		if err := Decide(db.DB, id, true); err != nil {
			close(stop)
			t.Fatal(err)
		}
	}
	close(stop)

	select {
	case s := <-bad:
		t.Fatalf("a pending approval was visible while the status still said %q", s)
	default:
	}
}
