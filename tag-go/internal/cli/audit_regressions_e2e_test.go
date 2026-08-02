package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regressions for the 2026-08 E2E port audit. Each test names the behaviour that
// was wrong and asserts the corrected contract.

// F8: two adjacent validations of the same class disagreed about the exit code.
func TestLoopStartUsageErrorsAreExit2(t *testing.T) {
	h := newHome(t)
	for _, args := range [][]string{
		{"loop", "start", "--goal", "g", "--max-iters", "0"},
		{"loop", "start", "--goal", "g", "--max-iters", "-3"},
		{"loop", "start", "--goal", "g", "--approval", "bogus"},
		{"loop", "start", "--goal", "g", "--approval-timeout", "0"},
		{"loop", "start", "--goal", "g", "--approval-timeout", "-1s"},
	} {
		out, code := run(t, h, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2 (usage): %q", args[2:], code, out)
		}
	}
}

// F10: the legacy driver silently ran one pass for --iterations 0 or -2.
func TestLegacyLoopRejectsNonPositiveIterations(t *testing.T) {
	h := newHome(t)
	for _, n := range []string{"0", "-2"} {
		out, code := run(t, h, "loop", "--provider", "echo", "--iterations", n, "hi")
		if code != 2 {
			t.Errorf("--iterations %s: exit %d, want 2: %q", n, code, out)
		}
	}
	if out, code := run(t, h, "loop", "--provider", "echo", "--iterations", "1", "hi"); code != 0 {
		t.Errorf("a valid run must still work: exit %d %q", code, out)
	}
}

// F10: eval's unvalidated numeric flags. --case-timeout 0 meant "no timeout at
// all", so a stalled provider ran unbounded and exited 0.
func TestEvalRunRejectsNonsenseNumericFlags(t *testing.T) {
	h := newHome(t)
	suite := filepath.Join(t.TempDir(), "s.yaml")
	os.WriteFile(suite, []byte("cases:\n  - id: a\n    input: hi\n    expect_contains: [hi]\n"), 0o644)
	for _, args := range [][]string{
		{"eval", "run", "--suite", suite, "--case-timeout", "0"},
		{"eval", "run", "--suite", suite, "--case-timeout", "-5s"},
		{"eval", "run", "--suite", suite, "--judge-threshold", "5"},
		{"eval", "run", "--suite", suite, "--judge-threshold", "-1"},
		{"eval", "run", "--suite", suite, "--max-steps", "-1"},
	} {
		out, code := run(t, h, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2 (usage): %q", args[3:], code, out)
		}
	}
	// The defaults, and an explicit in-range value, must still run.
	if out, code := run(t, h, "eval", "run", "--suite", suite, "--judge-threshold", "0.7"); code != 0 {
		t.Errorf("a valid run must still work: exit %d %q", code, out)
	}
}

// F9: abort rewrote a COMPLETED run to `aborted` and exited 0, destroying the
// record of what actually happened.
func TestSwarmAbortRefusesTerminalRuns(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "--json", "swarm", "run", "--goal", "build a thing", "--provider", "echo")
	if code != 0 {
		t.Fatalf("swarm run exit %d: %q", code, out)
	}
	var res struct {
		SwarmID string `json:"swarm_id"`
		Status  string `json:"status"`
	}
	// The echo coordinator emits degradation warnings on stderr, which run()
	// folds in; the JSON document starts at the first '{'.
	body := out
	if i := strings.Index(out, "{"); i >= 0 {
		body = out[i:]
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("swarm run --json: %v\n%s", err, out)
	}
	if res.Status != "completed" || res.SwarmID == "" {
		t.Fatalf("expected a completed run, got %+v", res)
	}

	aout, acode := run(t, h, "swarm", "abort", res.SwarmID)
	if acode == 0 {
		t.Errorf("aborting a finished run must not report success: %q", aout)
	}
	if !strings.Contains(aout, "not running") {
		t.Errorf("expected a 'not running' explanation: %q", aout)
	}

	lout, _ := run(t, h, "--json", "swarm", "list")
	if strings.Contains(lout, `"aborted"`) {
		t.Errorf("the completed run's status must survive the abort attempt: %q", lout)
	}
}

// F11: a loop stopped by `loop deny` reported exit 0, so a CI wrapper could not
// tell a finished loop from a killed one. swarm already used 4 for this.
func TestLoopDeniedExitsNonZero(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "loop", "start", "--goal", "g", "--provider", "echo",
		"--approval", "human", "--approval-timeout", "1s", "--max-iters", "2")
	if code != 4 {
		t.Errorf("an aborted loop must not exit 0 (want 4, got %d): %q", code, out)
	}
	if !strings.Contains(out, "aborted") {
		t.Errorf("expected the aborted status in the report: %q", out)
	}
}

// F6 (CLI half): "0 flaky" on a log whose format was not recognised must not
// read as a clean bill of health.
func TestFlakyFixSaysWhenNothingWasParsed(t *testing.T) {
	h := newHome(t)
	dir := t.TempDir()
	unparsed := filepath.Join(dir, "build.log")
	os.WriteFile(unparsed, []byte("Compiling...\nLinking...\nDone.\n"), 0o644)
	out, code := run(t, h, "agentic-ci", "flaky-fix", unparsed, "--repo", dir)
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out)
	}
	if !strings.Contains(out, "NOT a clean bill of health") {
		t.Errorf("an unparsed log must be called out as unparsed: %q", out)
	}

	// A recognised-but-stable log keeps the original explanation.
	stable := filepath.Join(dir, "stable.log")
	os.WriteFile(stable, []byte("PASSED tests/a.py::b\nPASSED tests/a.py::b\n"), 0o644)
	out2, code2 := run(t, h, "agentic-ci", "flaky-fix", stable, "--repo", dir)
	if code2 != 0 {
		t.Fatalf("exit %d: %q", code2, out2)
	}
	if !strings.Contains(out2, "no test both passed AND failed") {
		t.Errorf("a stable suite keeps its own explanation: %q", out2)
	}
	if strings.Contains(out2, "NOT a clean bill of health") {
		t.Errorf("a stable suite must not be reported as unparsed: %q", out2)
	}
}

// F5: --stages that omits a stage the generated jobs use rendered a
// .gitlab-ci.yml GitLab rejects, and reported success.
func TestGenPipelineRejectsIncompleteStages(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x"}`), 0o644)

	out, code := run(t, h, "agentic-ci", "gen-pipeline", "--repo", repo, "--stages", "a,b", "--dry-run")
	if code != 2 {
		t.Errorf("an incomplete --stages list is a usage error (want 2, got %d): %q", code, out)
	}
	if !strings.Contains(out, "build") || !strings.Contains(out, "test") {
		t.Errorf("the message must name the stages the jobs need: %q", out)
	}

	// A complete list, and the default, still work.
	if out, code := run(t, h, "agentic-ci", "gen-pipeline", "--repo", repo,
		"--stages", "build,test", "--dry-run"); code != 0 {
		t.Errorf("a complete --stages list must be accepted: exit %d %q", code, out)
	}
	if out, code := run(t, h, "agentic-ci", "gen-pipeline", "--repo", repo, "--dry-run"); code != 0 {
		t.Errorf("the default stage list must be accepted: exit %d %q", code, out)
	}
}
