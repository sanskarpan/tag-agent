package eval

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/permission"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func writeSuite(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "suite.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------------------------------------------------------------- suite load

func TestLoadSuiteValidationMirrorsPython(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"not a mapping", "- a\n- b\n", "must be a YAML mapping"},
		{"no cases key", "name: x\n", "must have a 'cases' list"},
		{"empty cases", "name: x\ncases: []\n", "at least one case"},
		{"case not mapping", "cases:\n  - hello\n", "must be a mapping"},
		{"expect_contains scalar", "cases:\n  - id: a\n    input: i\n    expect_contains: nope\n", "must be a list of strings"},
		{"expect_contains non-string", "cases:\n  - id: a\n    input: i\n    expect_contains: [1]\n", "must be a list of strings"},
		{"min_length bool", "cases:\n  - id: a\n    input: i\n    min_length: true\n", "must be an integer"},
		{"min_length string", "cases:\n  - id: a\n    input: i\n    min_length: \"5\"\n", "must be an integer"},
		// Go-only checks (Python accepts all three and misbehaves later).
		{"bad regex", "cases:\n  - id: a\n    input: i\n    expect_regex: ['([']\n", "invalid pattern"},
		{"typo'd key", "cases:\n  - id: a\n    input: i\n    expect_contain: [x]\n", "unrecognized key"},
		{"duplicate id", "cases:\n  - id: a\n    input: i\n  - id: a\n    input: j\n", "duplicate case id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadSuite(writeSuite(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
	if _, err := LoadSuite("/no/such/suite.yaml"); err == nil || !strings.Contains(err.Error(), "suite not found") {
		t.Fatalf("missing file err = %v", err)
	}
}

func TestLoadSuiteGeneratesPositionalIDs(t *testing.T) {
	s, err := LoadSuite(writeSuite(t, "name: S\ncases:\n  - input: one\n  - input: two\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Cases[0].ID != "case_1" || s.Cases[1].ID != "case_2" {
		t.Fatalf("ids = %q,%q", s.Cases[0].ID, s.Cases[1].ID)
	}
}

// ---------------------------------------------------------------- scoring

func TestScoreCaseParityWithPython(t *testing.T) {
	five := 5
	ten := 10
	c := Case{ExpectContains: []string{"print", "Hello"}, ExpectNotContain: []string{"error"}, MinLength: &five}
	sc := ScoreCase(c, "print('Hello world')")
	if !sc.Passed || sc.Score != 1.0 || sc.Reason != "" {
		t.Fatalf("all-pass: %+v", sc)
	}
	// Case-insensitive, like Python's .lower() comparison.
	if sc := ScoreCase(Case{ExpectContains: []string{"HELLO"}}, "hello"); !sc.Passed {
		t.Error("contains must be case-insensitive")
	}
	// Partial: score is the fraction, but passed requires every check.
	sc = ScoreCase(Case{ExpectContains: []string{"a", "zzz"}}, "a")
	if sc.Passed || sc.Score != 0.5 || !strings.Contains(sc.Reason, "missing 'zzz'") {
		t.Fatalf("partial: %+v", sc)
	}
	sc = ScoreCase(Case{MaxLength: &ten}, strings.Repeat("x", 11))
	if sc.Passed || !strings.Contains(sc.Reason, "output too long (11 > 10)") {
		t.Fatalf("maxlen: %+v", sc)
	}
	sc = ScoreCase(Case{MinLength: &ten}, "abc")
	if sc.Passed || !strings.Contains(sc.Reason, "output too short (3 < 10)") {
		t.Fatalf("minlen: %+v", sc)
	}
}

// TestScoreCaseNoChecksIsFlaggedUnchecked: Python returns (True, 1.0) for a case
// with no expectations and tells the caller nothing, so a suite of such cases
// reports "all passed". The pass is preserved for parity, but it must be
// flagged so the runner can report it.
func TestScoreCaseNoChecksIsFlaggedUnchecked(t *testing.T) {
	sc := ScoreCase(Case{ID: "x", Input: "hi"}, "anything")
	if !sc.Passed || sc.Checks != 0 || !sc.Unchecked {
		t.Fatalf("%+v", sc)
	}
}

// TestScoreCaseChecksExpectedOutput: the fake-success bug in Python. Cases
// produced by `eval-dataset export` carry ONLY expected_output, which
// eval_framework.score_case ignores — so they pass no matter what the model
// says. Here expected_output is a real check.
func TestScoreCaseChecksExpectedOutput(t *testing.T) {
	want := "42"
	sc := ScoreCase(Case{ExpectedOutput: &want}, "the answer is 7")
	if sc.Passed {
		t.Fatal("a dataset case whose expected_output is absent from the output must FAIL")
	}
	if sc.Unchecked {
		t.Fatal("expected_output must count as a check")
	}
	if sc := ScoreCase(Case{ExpectedOutput: &want}, "the answer is 42"); !sc.Passed {
		t.Fatal("matching expected_output must pass")
	}
}

// ---------------------------------------------------------------- run

func TestRunPersistsResultsReadableByListAndShow(t *testing.T) {
	db := testDB(t)
	s, err := LoadSuite(writeSuite(t, `
name: Persist Suite
cases:
  - id: ok_case
    input: "hello world"
    expect_contains: ["hello"]
  - id: bad_case
    input: "hello world"
    expect_contains: ["definitely-not-present-xyz"]
`))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Run(context.Background(), db, s, "suite.yaml", Options{Profile: "coder"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 2 || sum.Passed != 1 || sum.Failed != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Status != StatusCompleted {
		t.Fatalf("status = %q", sum.Status)
	}
	// What `eval list` reads.
	var status, suiteName string
	var pass, fail, total int
	if err := db.QueryRow(`SELECT status, suite_name, pass_count, fail_count, total_count
		FROM eval_runs WHERE id=?`, sum.EvalRunID).Scan(&status, &suiteName, &pass, &fail, &total); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || suiteName != "Persist Suite" || pass != 1 || fail != 1 || total != 2 {
		t.Fatalf("eval_runs row: %s %s %d/%d/%d", status, suiteName, pass, fail, total)
	}
	// What `eval show` reads.
	rows, err := db.Query(`SELECT case_id, passed, failure_reason FROM eval_cases WHERE eval_run_id=? ORDER BY case_id`, sum.EvalRunID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var id string
		var p int
		var reason sql.NullString
		if err := rows.Scan(&id, &p, &reason); err != nil {
			t.Fatal(err)
		}
		got[id] = p != 0
		if id == "bad_case" && (!reason.Valid || !strings.Contains(reason.String, "definitely-not-present-xyz")) {
			t.Errorf("failing case must record why: %v", reason)
		}
	}
	if got["ok_case"] != true || got["bad_case"] != false {
		t.Fatalf("persisted verdicts = %v", got)
	}
}

// TestFailingCaseIsNotSilentlyPassed guards the headline regression: a case
// whose expectations are unmet must be reported as failing end-to-end (result,
// DB row, and non-zero Failed count).
func TestFailingCaseIsNotSilentlyPassed(t *testing.T) {
	db := testDB(t)
	s, err := LoadSuite(writeSuite(t, "cases:\n  - id: c1\n    input: \"abc\"\n    expect_contains: [\"nope-not-here\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Run(context.Background(), db, s, "s.yaml", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Passed != 0 || sum.Failed != 1 || sum.Cases[0].Passed {
		t.Fatalf("failing case reported as %+v", sum)
	}
}

// TestEchoRunIsHonest: the offline path must run to completion (no hang) and
// must say plainly that its results mean nothing — including flagging passes
// that only happen because echo replays the prompt.
func TestEchoRunIsHonest(t *testing.T) {
	db := testDB(t)
	s, err := LoadSuite(writeSuite(t, "cases:\n  - id: c1\n    input: \"please print hello\"\n    expect_contains: [\"print\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Run(context.Background(), db, s, "s.yaml", Options{Provider: llm.EchoProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Offline || sum.Meaningful {
		t.Fatalf("echo run must be marked offline and not meaningful: %+v", sum)
	}
	if !sum.Cases[0].Trivial || sum.Trivial != 1 {
		t.Fatal("a pass that also holds against the prompt alone must be flagged trivial")
	}
	joined := strings.Join(sum.Notes, " ")
	if !strings.Contains(joined, "NOT meaningful") || !strings.Contains(joined, "NOT real passes") {
		t.Fatalf("notes must be explicit, got %q", joined)
	}
}

// TestNonTrivialPassIsNotFlagged: the trivial-pass detector must not cry wolf on
// a check the prompt does not already satisfy.
func TestNonTrivialPassIsNotFlagged(t *testing.T) {
	db := testDB(t)
	// echo returns the input, so expect_not_contains passes for a real reason.
	s, err := LoadSuite(writeSuite(t, "cases:\n  - id: c1\n    input: \"clean output\"\n    expect_not_contains: [\"traceback\"]\n    min_length: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Run(context.Background(), db, s, "s.yaml", Options{})
	if err != nil {
		t.Fatal(err)
	}
	// expect_not_contains DOES hold against the input too, so this one is
	// genuinely trivial — assert the detector's shape rather than a false one.
	if !sum.Cases[0].Passed {
		t.Fatal("case should pass")
	}
	// A check the input cannot satisfy must NOT be trivial.
	s2, _ := LoadSuite(writeSuite(t, "cases:\n  - id: c2\n    input: \"short\"\n    min_length: 100\n"))
	sum2, err := Run(context.Background(), db, s2, "s2.yaml", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum2.Cases[0].Passed || sum2.Cases[0].Trivial {
		t.Fatalf("min_length 100 against a 5-char echo must fail and not be trivial: %+v", sum2.Cases[0])
	}
}

// TestUncheckedCasesAreCounted: a suite of assertion-less cases must not be
// presented as a clean green run without comment.
func TestUncheckedCasesAreCounted(t *testing.T) {
	db := testDB(t)
	s, _ := LoadSuite(writeSuite(t, "cases:\n  - id: a\n    input: x\n  - id: b\n    input: y\n"))
	sum, err := Run(context.Background(), db, s, "s.yaml", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unchecked != 2 {
		t.Fatalf("unchecked = %d, want 2", sum.Unchecked)
	}
	if !strings.Contains(strings.Join(sum.Notes, " "), "cannot fail") {
		t.Fatalf("notes = %v", sum.Notes)
	}
}

// TestCancelledRunLeavesNothingRunning: SIGTERM (modelled by cancelling the
// context the CLI derives from signal.NotifyContext) must reach a terminal
// status, never strand the run in 'running'.
func TestCancelledRunLeavesNothingRunning(t *testing.T) {
	db := testDB(t)
	s, _ := LoadSuite(writeSuite(t, "cases:\n  - id: a\n    input: x\n  - id: b\n    input: y\n  - id: c\n    input: z\n"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: nothing should dispatch
	sum, err := Run(ctx, db, s, "s.yaml", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", sum.Status)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM eval_runs WHERE status='running'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d run(s) stranded in 'running'", n)
	}
}

func TestCancelMidRunStillFinalizes(t *testing.T) {
	db := testDB(t)
	s, _ := LoadSuite(writeSuite(t, "cases:\n  - id: a\n    input: x\n  - id: b\n    input: y\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	sum, err := Run(ctx, db, s, "s.yaml", Options{Provider: slowProvider{delay: 200 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Status != StatusCancelled {
		t.Fatalf("status = %q", sum.Status)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM eval_runs WHERE id=?`, sum.EvalRunID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == StatusRunning {
		t.Fatal("run stranded in 'running' after cancellation")
	}
}

// TestConcurrentCasesDoNotShareWorkingDir: #591 for evals. Every case writes the
// same filename through the write_file tool; with a shared cwd they clobber each
// other, with per-case roots each file holds that case's content.
func TestConcurrentCasesDoNotShareWorkingDir(t *testing.T) {
	db := testDB(t)
	s, err := LoadSuite(writeSuite(t, `
cases:
  - id: alpha
    input: "alpha"
    expect_contains: ["wrote"]
  - id: beta
    input: "beta"
    expect_contains: ["wrote"]
  - id: gamma
    input: "gamma"
    expect_contains: ["wrote"]
`))
	if err != nil {
		t.Fatal(err)
	}
	workRoot := t.TempDir()
	pol := permission.DefaultPolicy()
	pol.AutoApprove = true
	sum, err := Run(context.Background(), db, s, "s.yaml", Options{
		Provider:    &writerProvider{},
		WithTools:   true,
		Guard:       permission.NewGuard(pol, nil, nil),
		WorkRoot:    workRoot,
		Concurrency: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 0 {
		t.Fatalf("cases failed: %+v", sum.Cases)
	}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		p := filepath.Join(workRoot, sum.EvalRunID, id, "out.txt")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("case %s has no private artifact at %s: %v", id, p, err)
		}
		if string(b) != id {
			t.Fatalf("case %s artifact = %q — cases clobbered each other", id, b)
		}
	}
}

// TestNilGuardFailsClosed: an eval run with tools but no guard must not get a
// free pass on the dangerous tools.
func TestNilGuardFailsClosed(t *testing.T) {
	db := testDB(t)
	s, _ := LoadSuite(writeSuite(t, "cases:\n  - id: a\n    input: a\n    expect_contains: ['denied']\n"))
	sum, err := Run(context.Background(), db, s, "s.yaml", Options{
		Provider: &writerProvider{}, WithTools: true, Guard: nil, WorkRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// tool.Register substitutes the secure default policy for a nil Guard, which
	// puts write_file behind `ask`; with no prompter that is an immediate deny.
	if strings.Contains(sum.Cases[0].Output, "wrote") {
		t.Fatal("write_file executed with a nil Guard — the gate must fail closed")
	}
	if !strings.Contains(strings.ToLower(sum.Cases[0].Output), "permission denied") {
		t.Fatalf("want an explicit permission denial, got %q", sum.Cases[0].Output)
	}
}

func TestJudgeWithEchoDoesNotFabricateAPass(t *testing.T) {
	db := testDB(t)
	s, _ := LoadSuite(writeSuite(t, "cases:\n  - id: a\n    input: 'say hi'\n"))
	sum, err := Run(context.Background(), db, s, "s.yaml", Options{Judge: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Cases[0].Passed {
		t.Fatal("the echo judge returns the neutral 0.5 fallback, which is below the 0.7 threshold — it must not pass")
	}
	if sum.Cases[0].JudgeScore == nil {
		t.Fatal("judge score must be recorded")
	}
}

func TestCaseWorkDirCannotEscapeWorkRoot(t *testing.T) {
	root := t.TempDir()
	dir, cleanup, err := caseWorkDir(root, "run1", "../../etc")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(dir, filepath.Join(root, "run1")+string(os.PathSeparator)) {
		t.Fatalf("work dir %q escaped %q", dir, root)
	}
}

// ---------------------------------------------------------------- fakes

type slowProvider struct{ delay time.Duration }

func (slowProvider) Name() string { return "slow" }
func (p slowProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 2)
	go func() {
		defer close(ch)
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			ch <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
			return
		}
		ch <- llm.Event{Type: llm.EventTextDelta, Text: "ok"}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

// writerProvider asks write_file to write the user prompt into out.txt on its
// first turn, then reports what happened. Each case therefore races on the same
// filename unless it has its own working directory.
type writerProvider struct {
	mu    sync.Mutex
	turns map[string]int
}

func (*writerProvider) Name() string { return "writer" }

func (p *writerProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	var user string
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			user = m.Content
		}
	}
	p.mu.Lock()
	if p.turns == nil {
		p.turns = map[string]int{}
	}
	p.turns[user]++
	n := p.turns[user]
	p.mu.Unlock()

	ch := make(chan llm.Event, 3)
	go func() {
		defer close(ch)
		if n == 1 {
			ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{
				ID: "w1", Name: "write_file",
				Input: map[string]any{"path": "out.txt", "content": user},
			}}
			ch <- llm.Event{Type: llm.EventFinish}
			return
		}
		text := "wrote " + user
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && strings.Contains(strings.ToLower(m.Content), "denied") {
				text = "denied: " + m.Content
			}
		}
		ch <- llm.Event{Type: llm.EventTextDelta, Text: text}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}
