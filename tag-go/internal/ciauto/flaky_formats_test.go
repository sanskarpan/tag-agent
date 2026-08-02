package ciauto

import "testing"

// TestDetectFlakyPytestVerboseFormat is the F6 regression: pytest's -v runner
// prints the test name BEFORE the outcome, and only the short-summary order was
// matched. A verbose log of a flaky suite parsed to zero outcomes, so flaky-fix
// reported "0 flaky" and exited 0 — a clean bill of health on a failing suite.
func TestDetectFlakyPytestVerboseFormat(t *testing.T) {
	log := `=== RUN 1 ===
tests/test_a.py::test_stable PASSED                                      [ 50%]
tests/test_a.py::test_wobble PASSED                                      [100%]
=== RUN 2 ===
tests/test_a.py::test_stable PASSED                                      [ 50%]
tests/test_a.py::test_wobble FAILED                                      [100%]
=== RUN 3 ===
tests/test_a.py::test_wobble FAILED                                      [100%]
`
	got := DetectFlaky(log, 0)
	if len(got) != 1 {
		t.Fatalf("expected exactly test_wobble to be flaky, got %+v", got)
	}
	f := got[0]
	if f.TestName != "tests/test_a.py::test_wobble" {
		t.Errorf("name = %q", f.TestName)
	}
	if f.PassCount != 1 || f.FailCount != 2 {
		t.Errorf("counts = %d pass / %d fail, want 1/2", f.PassCount, f.FailCount)
	}
	if !f.Quarantine {
		t.Error("a 2/3 fail rate is over the default threshold and should quarantine")
	}
}

// The short-summary order must keep working, and must not be double-counted by
// the newly added trailing-outcome pattern.
func TestDetectFlakyPytestShortSummaryUnchanged(t *testing.T) {
	log := `FAILED tests/test_a.py::test_wobble
PASSED tests/test_a.py::test_wobble
FAILED tests/test_a.py::test_wobble
`
	got := DetectFlaky(log, 0)
	if len(got) != 1 || got[0].PassCount != 1 || got[0].FailCount != 2 {
		t.Fatalf("got %+v", got)
	}
}

// A trailing outcome must not be paired with the NEXT line's test name — the
// hazard created by matching both orders with a newline-crossing \s+.
func TestDetectFlakyDoesNotPairAcrossLines(t *testing.T) {
	log := `tests/test_a.py::test_one PASSED
tests/test_a.py::test_two FAILED
`
	got := DetectFlaky(log, 0)
	if len(got) != 0 {
		t.Fatalf("neither test both passed and failed; got %+v", got)
	}
	if n := OutcomeCount(log); n != 2 {
		t.Errorf("expected exactly 2 outcomes, got %d (a cross-line pairing inflates this)", n)
	}
}

// TestDetectFlakyUnicodeNames is the F7 regression: Go's \w is ASCII-only where
// Python's re.\w is Unicode-aware, so non-ASCII test names silently vanished
// from the Go port while Python found them.
func TestDetectFlakyUnicodeNames(t *testing.T) {
	log := "FAILED tests/測試.py::test_中文\nPASSED tests/測試.py::test_中文\n"
	got := DetectFlaky(log, 0)
	if len(got) != 1 {
		t.Fatalf("a non-ASCII test name must be detected, got %+v", got)
	}
	if got[0].TestName != "tests/測試.py::test_中文" {
		t.Errorf("name = %q", got[0].TestName)
	}
}

// OutcomeCount separates "the suite is stable" from "the log was not understood".
func TestOutcomeCountDistinguishesUnparsedLogs(t *testing.T) {
	if n := OutcomeCount("some build output with no test results at all\n"); n != 0 {
		t.Errorf("an unrecognised log has no outcomes, got %d", n)
	}
	if n := OutcomeCount("PASSED tests/a.py::b\nPASSED tests/a.py::b\n"); n != 2 {
		t.Errorf("a stable suite still has outcomes, got %d", n)
	}
	if n := OutcomeCount("--- PASS: TestFoo\n--- FAIL: TestFoo\n"); n != 2 {
		t.Errorf("go test -v outcomes, got %d", n)
	}
}

// TestUndeclaredStages is the F5 regression: the job templates hard-code
// build/test/deploy, so a custom --stages list rendered a pipeline GitLab
// rejects — emitted with exit 0 and no warning.
func TestUndeclaredStages(t *testing.T) {
	yaml := GenerateGitLabPipeline([]string{"node"}, PipelineOptions{Stages: []string{"a", "b"}})
	missing := UndeclaredStages(yaml, []string{"a", "b"})
	if len(missing) == 0 {
		t.Fatal("node jobs use build/test, neither declared — expected them reported")
	}
	seen := map[string]bool{}
	for _, s := range missing {
		seen[s] = true
	}
	if !seen["build"] || !seen["test"] {
		t.Errorf("missing = %v, want build and test", missing)
	}

	// The default stage list declares everything the jobs use.
	def := GenerateGitLabPipeline([]string{"node", "python", "go"}, PipelineOptions{IncludeDeploy: true})
	if m := UndeclaredStages(def, []string{"build", "test", "deploy"}); len(m) != 0 {
		t.Errorf("the default pipeline must be self-consistent, got %v", m)
	}
}
