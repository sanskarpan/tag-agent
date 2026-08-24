package cli_test

import "testing"

// TestDagShowUnmatchedArgErrors: `dag show <name-or-bad-id>` that matches no job
// must exit non-zero (it takes job ids), not print "No jobs found." with exit 0
// (#762). `dag show` with no args still lists jobs and exits 0.
func TestDagShowUnmatchedArgErrors(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "dag", "save", "pipe", "--steps", `[{"name":"build","task":"compile"}]`); code != 0 {
		t.Fatalf("dag save failed: %d", code)
	}
	if _, code := run(t, h, "dag", "show", "no-such-job"); code == 0 {
		t.Error("dag show with an unmatched arg must exit non-zero, got 0")
	}
	if _, code := run(t, h, "dag", "show"); code != 0 {
		t.Error("dag show with no args must exit 0")
	}
}
