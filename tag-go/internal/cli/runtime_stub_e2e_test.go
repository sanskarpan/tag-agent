package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runStdin runs the binary with piped STDIN (for `tag shell`).
func runStdin(t *testing.T, home, stdin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(os.Environ(), "TAG_HOME="+home)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// makeSession creates a run (a "session") via `tag run` and returns its run_id.
func makeSession(t *testing.T, home, prompt string) string {
	t.Helper()
	out, code := run(t, home, "run", prompt, "--json")
	if code != 0 {
		t.Fatalf("tag run failed: %q code=%d", out, code)
	}
	var res struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse run json: %v (%q)", err, out)
	}
	if res.RunID == "" {
		t.Fatalf("empty run_id from %q", out)
	}
	return res.RunID
}

// TestE2EContextCompress: `context compress --session-id` assembles a stored
// session and persists a compressed record (no hermes error).
func TestE2EContextCompress(t *testing.T) {
	h := newHome(t)
	sid := makeSession(t, h, "build a login page")

	out, code := run(t, h, "context", "compress", "--session-id", sid)
	if code != 0 {
		t.Fatalf("context compress code=%d out=%q", code, out)
	}
	if strings.Contains(strings.ToLower(out), "hermes") {
		t.Errorf("compress still references hermes: %q", out)
	}
	if !strings.Contains(out, "Compressed session") || !strings.Contains(out, "record:") {
		t.Errorf("compress did not report a persisted record: %q", out)
	}

	// JSON path persists tokens/summary.
	jout, jcode := run(t, h, "context", "compress", "--session-id", sid, "--json")
	if jcode != 0 {
		t.Fatalf("compress --json code=%d out=%q", jcode, jout)
	}
	var rec struct {
		Action    string `json:"action"`
		SessionID string `json:"session_id"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(jout), &rec); err != nil {
		t.Fatalf("parse compress json: %v (%q)", err, jout)
	}
	if rec.Action != "compress" || rec.Summary == "" {
		t.Errorf("compress json missing fields: %+v", rec)
	}
	if !strings.HasPrefix(rec.SessionID, sid[:8]) {
		t.Errorf("compress session_id %q does not match %q", rec.SessionID, sid)
	}

	// Missing session errors (not a hermes stub).
	if eout, ecode := run(t, h, "context", "compress", "--session-id", "does-not-exist"); ecode == 0 {
		t.Errorf("compress on missing session should fail: %q", eout)
	} else if strings.Contains(strings.ToLower(eout), "hermes") {
		t.Errorf("missing-session error mentions hermes: %q", eout)
	}
}

// TestE2EContextTrim: `context trim --keep-last N` keeps the last N items and
// persists a record.
func TestE2EContextTrim(t *testing.T) {
	h := newHome(t)
	sid := makeSession(t, h, "some longer task with multiple turns")

	out, code := run(t, h, "context", "trim", "--session-id", sid, "--keep-last", "1")
	if code != 0 {
		t.Fatalf("context trim code=%d out=%q", code, out)
	}
	if strings.Contains(strings.ToLower(out), "hermes") {
		t.Errorf("trim still references hermes: %q", out)
	}
	if !strings.Contains(out, "Trimmed session") {
		t.Errorf("trim did not report: %q", out)
	}

	// keep-last <= 0 is a validation error.
	if _, code := run(t, h, "context", "trim", "--session-id", sid, "--keep-last", "0"); code == 0 {
		t.Error("trim --keep-last 0 should fail")
	}
	// Missing --session-id is a validation error.
	if _, code := run(t, h, "context", "trim", "--keep-last", "1"); code == 0 {
		t.Error("trim without --session-id should fail")
	}
}

// TestE2ESplitPlan: `split plan` creates a split_runs row that `split show` and
// `split list` render (read paths preserved).
func TestE2ESplitPlan(t *testing.T) {
	h := newHome(t)

	pout, pcode := run(t, h, "split", "plan", "add authentication to the service", "--json")
	if pcode != 0 {
		t.Fatalf("split plan code=%d out=%q", pcode, pout)
	}
	if strings.Contains(strings.ToLower(pout), "requires the architect") {
		t.Errorf("split plan still stubbed: %q", pout)
	}
	var plan struct {
		RunID      string `json:"run_id"`
		Status     string `json:"status"`
		ItemsTotal int    `json:"items_total"`
	}
	if err := json.Unmarshal([]byte(pout), &plan); err != nil {
		t.Fatalf("parse plan json: %v (%q)", err, pout)
	}
	if plan.RunID == "" || plan.Status != "planned" || plan.ItemsTotal < 1 {
		t.Fatalf("plan missing fields: %+v", plan)
	}

	// split show renders the persisted run + items.
	sout, scode := run(t, h, "split", "show", plan.RunID)
	if scode != 0 {
		t.Fatalf("split show code=%d out=%q", scode, sout)
	}
	if !strings.Contains(sout, plan.RunID) || !strings.Contains(sout, "add authentication") {
		t.Errorf("split show did not render the run: %q", sout)
	}
	if !strings.Contains(sout, "Items:") {
		t.Errorf("split show did not render items: %q", sout)
	}

	// split list shows the run.
	lout, _ := run(t, h, "split", "list")
	if !strings.Contains(lout, "add authentication") {
		t.Errorf("split list did not show the run: %q", lout)
	}

	// Supplied --spec-json persists the provided items.
	spec := `{"task":"custom","items":[{"file":"a.go","description":"do a"},{"file":"b.go","description":"do b"}]}`
	jout, jcode := run(t, h, "split", "plan", "custom", "--spec-json", spec, "--json")
	if jcode != 0 {
		t.Fatalf("split plan --spec-json code=%d out=%q", jcode, jout)
	}
	var plan2 struct {
		RunID      string `json:"run_id"`
		ItemsTotal int    `json:"items_total"`
	}
	if err := json.Unmarshal([]byte(jout), &plan2); err != nil {
		t.Fatalf("parse spec-json plan: %v", err)
	}
	if plan2.ItemsTotal != 2 {
		t.Errorf("spec-json items_total = %d, want 2", plan2.ItemsTotal)
	}
	sout2, _ := run(t, h, "split", "show", plan2.RunID)
	if !strings.Contains(sout2, "a.go") || !strings.Contains(sout2, "b.go") {
		t.Errorf("spec-json items not rendered: %q", sout2)
	}

	// Bad --spec-json is a usage error (exit 2).
	if _, code := run(t, h, "split", "plan", "x", "--spec-json", "{not json"); code == 0 {
		t.Error("bad --spec-json should fail")
	}
}

// TestE2EShellREPL: `shell` piped a line returns a real agent-loop response
// (echo), not the "[stub]" placeholder.
func TestE2EShellREPL(t *testing.T) {
	h := newHome(t)

	out, code := runStdin(t, h, "hello from the shell\nexit\n", "shell")
	if code != 0 {
		t.Fatalf("shell code=%d out=%q", code, out)
	}
	if strings.Contains(out, "[stub]") {
		t.Errorf("shell still emits the stub: %q", out)
	}
	if !strings.Contains(out, "hello from the shell") {
		t.Errorf("shell did not echo the agent response: %q", out)
	}
	if !strings.Contains(out, "Goodbye.") {
		t.Errorf("shell did not exit cleanly: %q", out)
	}

	// EOF without an explicit exit line still terminates.
	eofOut, eofCode := runStdin(t, h, "just one line\n", "shell")
	if eofCode != 0 {
		t.Fatalf("shell EOF code=%d out=%q", eofCode, eofOut)
	}
	if !strings.Contains(eofOut, "just one line") {
		t.Errorf("shell EOF did not process the line: %q", eofOut)
	}
}
