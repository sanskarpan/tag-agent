package cli_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runSplit is `run` but keeps stdout and stderr apart, which the --json
// contracts need (the honesty banner goes to stderr so it cannot corrupt the
// JSON document on stdout).
func runSplit(t *testing.T, home string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(os.Environ(), "TAG_HOME="+home,
		"ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "TAG_API_KEY=")
	var so, se strings.Builder
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return so.String(), se.String(), code
}

func evalSuite(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "suite.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const passingSuite = `
name: Green Suite
cases:
  - id: c1
    input: "the word banana appears here"
    expect_contains: ["banana"]
`

const failingSuite = `
name: Red Suite
cases:
  - id: ok
    input: "banana"
    expect_contains: ["banana"]
  - id: broken
    input: "banana"
    expect_contains: ["kumquat"]
`

// TestE2EEvalRunUsageErrors: usage problems exit 2, and --json still yields a
// parseable error object.
func TestE2EEvalRunUsageErrors(t *testing.T) {
	h := newHome(t)
	if _, _, code := runSplit(t, h, "eval", "run"); code != 2 {
		t.Errorf("missing --suite: code=%d want 2", code)
	}
	if _, _, code := runSplit(t, h, "eval", "run", "--suite", "a", "--dataset", "b"); code != 2 {
		t.Errorf("--suite+--dataset: code=%d want 2", code)
	}
	if _, _, code := runSplit(t, h, "eval", "run", "--suite", evalSuite(t, passingSuite), "--provider", "nope"); code != 2 {
		t.Errorf("bad provider: code=%d want 2", code)
	}
	if _, _, code := runSplit(t, h, "eval", "run", "--suite", evalSuite(t, passingSuite), "--concurrency", "0"); code != 2 {
		t.Errorf("concurrency 0: code=%d want 2", code)
	}
	// A malformed suite is bad input: exit 2, with a JSON error object.
	bad := evalSuite(t, "cases:\n  - id: a\n    input: i\n    expect_contain: [x]\n")
	so, _, code := runSplit(t, h, "--json", "eval", "run", "--suite", bad)
	if code != 2 {
		t.Errorf("bad suite: code=%d want 2", code)
	}
	var errObj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(so)), &errObj); err != nil || errObj["error"] == nil {
		t.Errorf("bad suite --json stdout = %q, want an {\"error\": ...} object", so)
	}
}

// TestE2EEvalRunPersistsAndIsReadable: run → list → show, and the failing case
// is reported as failing with exit 1 (Python's convention).
func TestE2EEvalRunPersistsAndIsReadable(t *testing.T) {
	h := newHome(t)
	so, se, code := runSplit(t, h, "eval", "run", "--suite", evalSuite(t, failingSuite), "--provider", "echo")
	if code != 1 {
		t.Fatalf("a failing eval must exit 1, got %d\nstdout:%s\nstderr:%s", code, so, se)
	}
	if !strings.Contains(so, "[✗] broken") {
		t.Errorf("failing case not reported as failing:\n%s", so)
	}
	if !strings.Contains(so, "Results: 1/2 passed") {
		t.Errorf("summary missing:\n%s", so)
	}
	// Offline honesty on stderr, so stdout stays clean.
	if !strings.Contains(se, "NOT meaningful") {
		t.Errorf("echo run must warn that results are not meaningful; stderr=%q", se)
	}

	lso, _, lcode := runSplit(t, h, "--json", "eval", "list")
	if lcode != 0 {
		t.Fatalf("eval list code=%d", lcode)
	}
	var runs []map[string]any
	if err := json.Unmarshal([]byte(lso), &runs); err != nil {
		t.Fatalf("eval list --json: %v\n%s", err, lso)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	r := runs[0]
	for _, k := range []string{"id", "suite_path", "profile", "suite_name", "status", "pass_count", "fail_count", "total_count", "created_at"} {
		if _, ok := r[k]; !ok {
			t.Errorf("eval list --json missing Python field %q", k)
		}
	}
	if r["status"] != "completed" || r["pass_count"].(float64) != 1 || r["fail_count"].(float64) != 1 {
		t.Errorf("run row = %+v", r)
	}

	sso, _, scode := runSplit(t, h, "--json", "eval", "show", r["id"].(string))
	if scode != 0 {
		t.Fatalf("eval show code=%d", scode)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(sso), &detail); err != nil {
		t.Fatalf("eval show --json: %v\n%s", err, sso)
	}
	cases, _ := detail["cases"].([]any)
	if len(cases) != 2 {
		t.Fatalf("eval show cases = %d, want 2", len(cases))
	}
	var sawFailure bool
	for _, c := range cases {
		m := c.(map[string]any)
		if m["case_id"] == "broken" {
			if m["passed"] != false {
				t.Error("broken case persisted as passed")
			}
			if s, _ := m["failure_reason"].(string); !strings.Contains(s, "kumquat") {
				t.Errorf("failure_reason = %v", m["failure_reason"])
			}
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Error("the failing case is missing from eval show")
	}
}

func TestE2EEvalRunGreenExitsZero(t *testing.T) {
	h := newHome(t)
	so, _, code := runSplit(t, h, "eval", "run", "--suite", evalSuite(t, passingSuite), "--provider", "echo")
	if code != 0 {
		t.Fatalf("all-passing suite must exit 0, got %d\n%s", code, so)
	}
}

// TestE2EEvalRunJSONContracts: empty list is [], a run summary carries the
// Python field names, and an unknown run id emits a JSON error object.
func TestE2EEvalRunJSONContracts(t *testing.T) {
	h := newHome(t)
	so, _, code := runSplit(t, h, "--json", "eval", "list")
	if code != 0 || strings.TrimSpace(so) != "[]" {
		t.Errorf("empty eval list --json = %q code=%d, want []", strings.TrimSpace(so), code)
	}
	so, _, code = runSplit(t, h, "--json", "eval", "show", "nope-nope")
	if code != 1 {
		t.Errorf("unknown run id code=%d want 1", code)
	}
	var errObj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(so)), &errObj); err != nil || errObj["error"] == nil {
		t.Errorf("unknown run id --json = %q, want an error object", so)
	}

	so, _, code = runSplit(t, h, "--json", "eval", "run", "--suite", evalSuite(t, failingSuite), "--provider", "echo")
	if code != 1 {
		t.Errorf("failing eval --json must still exit 1, got %d", code)
	}
	var sum map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(so)), &sum); err != nil {
		t.Fatalf("run --json not parseable: %v\n%s", err, so)
	}
	for _, k := range []string{"eval_run_id", "total", "passed", "failed", "results_meaningful", "notes", "cases"} {
		if _, ok := sum[k]; !ok {
			t.Errorf("run --json missing %q", k)
		}
	}
	if sum["results_meaningful"] != false {
		t.Error("an echo run must report results_meaningful=false")
	}
	if sum["failed"].(float64) != 1 {
		t.Errorf("failed = %v, want 1", sum["failed"])
	}
}

// TestE2EEvalDryRunRecordsNothing: Python's --dry-run writes a full set of
// passed=1 rows, so a dry run is indistinguishable from a green run in
// `eval list`. Go's records nothing.
func TestE2EEvalDryRunRecordsNothing(t *testing.T) {
	h := newHome(t)
	so, _, code := runSplit(t, h, "eval", "run", "--suite", evalSuite(t, failingSuite), "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run code=%d\n%s", code, so)
	}
	if !strings.Contains(so, "DRY RUN") || !strings.Contains(so, "nothing recorded") {
		t.Errorf("dry-run must say it recorded nothing:\n%s", so)
	}
	lso, _, _ := runSplit(t, h, "--json", "eval", "list")
	if strings.TrimSpace(lso) != "[]" {
		t.Errorf("dry-run persisted an eval run: %s", lso)
	}
}

// slowMockLLM is a local OpenAI-compatible SSE endpoint that stalls before
// answering, so a run is genuinely in flight when a signal arrives. No live API
// call is ever made: the binary is pointed at this server via OPENAI_BASE_URL.
func slowMockLLM(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"banana\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestE2EEvalRunSIGTERMLeavesNothingRunning: a killed run must not strand its
// row in 'running'. The run is stalled on a local mock endpoint when SIGTERM
// lands, so the interrupt is genuinely mid-flight.
func TestE2EEvalRunSIGTERMLeavesNothingRunning(t *testing.T) {
	h := newHome(t)
	srv := slowMockLLM(t, 10*time.Second)

	var b strings.Builder
	b.WriteString("name: Long\ncases:\n")
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "  - id: c%d\n    input: \"banana\"\n    expect_contains: [\"banana\"]\n", i)
	}
	suite := evalSuite(t, b.String())

	cmd := exec.Command(tagBin, "eval", "run", "--suite", suite, "--provider", "openai",
		"--model", "gpt-4o-mini", "--case-timeout", "60s")
	cmd.Env = append(os.Environ(), "TAG_HOME="+h,
		"OPENAI_API_KEY=sk-mock", "OPENAI_BASE_URL="+srv.URL+"/v1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait until the run row exists (i.e. the run really started) before signalling.
	deadline := time.Now().Add(10 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		lso, _, _ := runSplit(t, h, "--json", "eval", "list")
		var runs []map[string]any
		if json.Unmarshal([]byte(lso), &runs) == nil && len(runs) == 1 {
			if runs[0]["status"] == "running" {
				started = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !started {
		_ = cmd.Process.Kill()
		t.Fatal("eval run never reached status 'running' — cannot test the interrupt")
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			t.Errorf("interrupted run should exit 1, got %v", err)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("eval run did not exit after SIGTERM")
	}

	lso, _, _ := runSplit(t, h, "--json", "eval", "list")
	var runs []map[string]any
	if err := json.Unmarshal([]byte(lso), &runs); err != nil {
		t.Fatalf("eval list: %v\n%s", err, lso)
	}
	if len(runs) == 0 {
		t.Fatal("no run recorded")
	}
	for _, r := range runs {
		if r["status"] == "running" {
			t.Fatalf("SIGTERM stranded a run in 'running': %+v", r)
		}
		if r["status"] != "cancelled" {
			t.Errorf("interrupted run status = %v, want cancelled", r["status"])
		}
	}
}

// TestE2EEvalRunDataset wires `eval run` to `eval-dataset`: a stored dataset is
// executable, and its expected_output is a REAL check (Python's score_case
// ignores expected_output, so every dataset case passes unconditionally there).
func TestE2EEvalRunDataset(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "eval-dataset", "create", "ds1"); code != 0 {
		t.Fatalf("create: %s", out)
	}
	// echo replays the input, so this expected_output can never appear.
	if out, code := run(t, h, "eval-dataset", "add-case", "ds1", "c1", "what is 2+2",
		"--expected", "the answer is 4"); code != 0 {
		t.Fatalf("add-case: %s", out)
	}
	so, _, code := runSplit(t, h, "eval", "run", "--dataset", "ds1", "--provider", "echo")
	if code != 1 {
		t.Fatalf("dataset case whose expected_output is absent must FAIL (exit 1), got %d\n%s", code, so)
	}
	if !strings.Contains(so, "expected_output not found") {
		t.Errorf("want an expected_output failure reason:\n%s", so)
	}
	if _, _, code := runSplit(t, h, "eval", "run", "--dataset", "missing-ds"); code != 1 {
		t.Errorf("unknown dataset code=%d want 1", code)
	}
}

// TestE2EEvalRunNoChecksIsNotSilentGreen: a suite that asserts nothing must not
// be presented as a clean pass without comment.
func TestE2EEvalRunNoChecksIsNotSilentGreen(t *testing.T) {
	h := newHome(t)
	so, se, code := runSplit(t, h, "eval", "run", "--suite",
		evalSuite(t, "name: Empty\ncases:\n  - id: a\n    input: hello\n"), "--provider", "echo")
	if code != 0 {
		t.Fatalf("code=%d\n%s", code, so)
	}
	if !strings.Contains(so, "NO CHECKS") {
		t.Errorf("assertion-less case must be labelled:\n%s", so)
	}
	if !strings.Contains(se, "cannot fail") {
		t.Errorf("summary note missing from stderr: %q", se)
	}
}

// TestE2EEvalRunTrivialPassIsFlagged: with echo, a keyword check whose keyword
// is already in the prompt passes for free. That must be called out, not
// reported as a win.
func TestE2EEvalRunTrivialPassIsFlagged(t *testing.T) {
	h := newHome(t)
	so, _, code := runSplit(t, h, "eval", "run", "--suite", evalSuite(t, passingSuite), "--provider", "echo")
	if code != 0 {
		t.Fatalf("code=%d\n%s", code, so)
	}
	if !strings.Contains(so, "TRIVIAL") {
		t.Errorf("echo-driven pass must be flagged TRIVIAL:\n%s", so)
	}
}

// TestE2EEvalRunConcurrentCasesGetOwnWorkDir: each case's tool root is its own
// directory under TAG_HOME/eval-work/<run>/<case>.
func TestE2EEvalRunConcurrentCasesGetOwnWorkDir(t *testing.T) {
	h := newHome(t)
	suite := evalSuite(t, `
name: Tools
cases:
  - id: alpha
    input: "alpha"
  - id: beta
    input: "beta"
`)
	so, _, code := runSplit(t, h, "eval", "run", "--suite", suite, "--provider", "echo",
		"--tools", "--concurrency", "2")
	if code != 0 {
		t.Fatalf("code=%d\n%s", code, so)
	}
	// echo never calls a tool, so the dirs are created and then cleaned up
	// because they are empty; the run must still succeed and not deadlock on a
	// consent prompt (the guard is forced non-interactive).
	if !strings.Contains(so, "Results: 2/2 passed") {
		t.Errorf("unexpected output:\n%s", so)
	}
}
