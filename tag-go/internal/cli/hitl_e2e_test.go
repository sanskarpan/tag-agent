package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive the BUILT BINARY. Green unit tests have masked
// dispatch-layer bugs in this tree before (a flag-name collision that panicked
// the whole CLI was caught this way and not by any package test), so every
// PRD-078/109/123 surface is exercised through `tag …` in an isolated TAG_HOME.
//
// Every one of them is BOUNDED via runBounded: a regression that reintroduces a
// blocking gate must FAIL the test, never wedge the suite.

// appendTripwire writes a `tripwire:` block into the home's tag.yaml.
func appendTripwire(t *testing.T, home, block string) {
	t.Helper()
	// `bootstrap` (newHome) has already materialised the default config.
	path := filepath.Join(home, "config", "tag.yaml")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString("\n" + block + "\n"); err != nil {
		t.Fatal(err)
	}
}

// startReadFileServer is startBashServer's file-tool twin: turn 1 asks for
// read_file on the given (root-relative) path, turn 2 answers in text.
func startReadFileServer(t *testing.T, path string) *httptest.Server {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if n == 1 {
			args := fmt.Sprintf(`{"path":%q}`, path)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":""}}]}}]}`)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":%s}}]}}]}\n\n", jsonString(args))
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
			fmt.Fprintf(w, "data: [DONE]\n\n")
		} else {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"finished"}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			fmt.Fprintf(w, "data: [DONE]\n\n")
		}
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runBoundedIn is runBounded with an explicit working directory, so the file
// tools' root (which defaults to cwd) can be pointed at a fixture dir. Bounded
// the same way: a hang FAILS rather than wedging the suite.
func runBoundedIn(t *testing.T, dir, home string, env []string, d time.Duration, args ...string) (string, int, bool) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "TAG_HOME="+home), env...)
	cmd.Stdin = strings.NewReader("")
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return out.String(), code, false
	case <-time.After(d):
		_ = cmd.Process.Kill()
		<-done
		return out.String(), -1, true
	}
}

// firstJSONID pulls the first "id" out of a JSON document (object or array).
func firstJSONID(t *testing.T, s string) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(s)))
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	var walk func(any) string
	walk = func(n any) string {
		switch x := n.(type) {
		case map[string]any:
			if id, ok := x["id"].(string); ok {
				return id
			}
		case []any:
			for _, e := range x {
				if id := walk(e); id != "" {
					return id
				}
			}
		}
		return ""
	}
	id := walk(v)
	if id == "" {
		t.Fatalf("no id in JSON: %s", s)
	}
	return id
}

// ---------------------------------------------------------------------------
// PRD-078 — tool approval pause/resume
// ---------------------------------------------------------------------------

// TestE2EApprovalGateApprovedOutOfProcess is the whole point of PRD-078: a run
// parks on a tool call, a human in ANOTHER process approves it, the run resumes
// and the tool actually executes.
func TestE2EApprovalGateApprovedOutOfProcess(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "approved.txt")
	srv := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	type res struct {
		out  string
		code int
		hung bool
	}
	done := make(chan res, 1)
	go func() {
		out, code, hung := runBounded(t, h, env, 60*time.Second,
			"run", "do it", "--provider", "local", "--tools",
			"--approval-gate", "--approval-gate-timeout", "40s")
		done <- res{out, code, hung}
	}()

	// Poll for the parked request rather than sleeping a fixed amount.
	var id string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := run(t, h, "--json", "permissions", "pending")
		if strings.Contains(out, `"id"`) {
			id = firstJSONID(t, out)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if id == "" {
		r := <-done
		t.Fatalf("no approval request ever appeared; run output: %s", r.out)
	}

	if out, code := run(t, h, "permissions", "approve", id, "--rationale", "safe"); code != 0 {
		t.Fatalf("approve exit %d: %s", code, out)
	}

	r := <-done
	if r.hung {
		t.Fatalf("HANG: the run did not resume after approval. output: %s", r.out)
	}
	if r.code != 0 {
		t.Fatalf("run exit %d: %s", r.code, r.out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the approved tool never executed: %v\n%s", err, r.out)
	}
	if !strings.Contains(r.out, "APPROVAL REQUIRED") {
		t.Errorf("the parked run must announce itself, not wait silently: %s", r.out)
	}

	// The decision is in the append-only audit trail with its reviewer identity.
	audit, _ := run(t, h, "--json", "permissions", "audit")
	if !strings.Contains(audit, `"decision": "approved"`) || !strings.Contains(audit, `"rationale": "safe"`) {
		t.Errorf("audit trail missing the decision: %s", audit)
	}
}

// TestE2EApprovalGateDeniedOutOfProcess: a denial blocks the side effect and the
// model is told, honestly, that it was refused.
func TestE2EApprovalGateDeniedOutOfProcess(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "denied.txt")
	srv := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	done := make(chan string, 1)
	go func() {
		out, _, hung := runBounded(t, h, env, 60*time.Second,
			"run", "do it", "--provider", "local", "--tools",
			"--approval-gate", "--approval-gate-timeout", "40s")
		if hung {
			out = "HUNG: " + out
		}
		done <- out
	}()

	var id string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && id == "" {
		out, _ := run(t, h, "--json", "permissions", "pending")
		if strings.Contains(out, `"id"`) {
			id = firstJSONID(t, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if id == "" {
		t.Fatalf("no approval request appeared: %s", <-done)
	}
	if out, code := run(t, h, "permissions", "deny", id, "--rationale", "not authorised"); code != 0 {
		t.Fatalf("deny exit %d: %s", code, out)
	}

	out := <-done
	if strings.HasPrefix(out, "HUNG") {
		t.Fatalf("HANG after denial: %s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("SIDE EFFECT RAN despite a denial: %s", out)
	}
	if !strings.Contains(out, "permission denied") {
		t.Errorf("the model must be told it was refused: %s", out)
	}
}

// TestE2EApprovalGateAutoDeniesOnTimeout: nobody answers, and the run finishes
// on its own inside the bound. This is what makes the gate CI-safe.
func TestE2EApprovalGateAutoDeniesOnTimeout(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "timeout.txt")
	srv := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	start := time.Now()
	out, code, hung := runBounded(t, h, env, 45*time.Second,
		"run", "do it", "--provider", "local", "--tools",
		"--approval-gate", "--approval-gate-timeout", "2s")
	if hung {
		t.Fatalf("HANG: a 2s-bounded approval gate never returned. output: %s", out)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Second {
		t.Fatalf("a 2s gate took %s", elapsed)
	}
	if code != 0 {
		t.Fatalf("exit %d (an auto-denied tool must not crash the run): %s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("SIDE EFFECT RAN after an approval timeout: %s", out)
	}
	if !strings.Contains(out, "no approval within") {
		t.Errorf("the timeout must be reported explicitly: %s", out)
	}
	audit, _ := run(t, h, "--json", "permissions", "audit")
	if !strings.Contains(audit, `"decision": "timed_out"`) {
		t.Errorf("the timeout must be audited: %s", audit)
	}
}

// TestE2EBackgroundWorkerNeverParksOnApprovalGate is THE regression test for the
// biggest risk in PRD-078.
//
// `queue worker` forces non-interactivity in code. Even when the operator
// explicitly passes --approval-gate with a 10-MINUTE timeout, the worker must
// refuse the gate out loud and finish immediately. The generous timeout is
// deliberate: if the structural guarantee ever breaks, this test blows its
// 60-second bound and FAILS rather than the suite hanging for ten minutes.
func TestE2EBackgroundWorkerNeverParksOnApprovalGate(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "worker.txt")
	srv := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	if out, code := run(t, h, "queue", "add", "do it"); code != 0 {
		t.Fatalf("queue add: %d %s", code, out)
	}

	start := time.Now()
	out, code, hung := runBounded(t, h, env, 60*time.Second,
		"queue", "worker", "--max", "1", "--tools", "--provider", "local",
		"--approval-gate", "--approval-gate-timeout", "10m")
	if hung {
		t.Fatalf("HANG: a background worker parked on a human approval gate. output: %s", out)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Second {
		t.Fatalf("the worker took %s — it parked on the gate", elapsed)
	}
	if code != 0 {
		t.Fatalf("worker exit %d: %s", code, out)
	}
	if !strings.Contains(out, "--approval-gate is IGNORED") {
		t.Errorf("the refusal must be LOUD, not silent: %s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the worker executed bash without any approval: %s", out)
	}
	// Nothing may have been published: a pending row nobody will ever answer is
	// its own kind of lie.
	pending, _ := run(t, h, "--json", "permissions", "pending")
	if strings.Contains(pending, `"id"`) {
		t.Errorf("the worker published an approval request it could never wait for: %s", pending)
	}
	if strings.TrimSpace(pending) != "[]" {
		t.Errorf("empty --json must be [] not %q", strings.TrimSpace(pending))
	}
}

// TestE2EApprovalGateRejectsUnboundedTimeout: exit 2, and no run starts.
func TestE2EApprovalGateRejectsUnboundedTimeout(t *testing.T) {
	h := newHome(t)
	out, code, hung := runBounded(t, h, nil, 30*time.Second,
		"run", "hi", "--provider", "echo", "--tools",
		"--approval-gate", "--approval-gate-timeout", "0")
	if hung {
		t.Fatalf("HANG: %s", out)
	}
	if code != 2 {
		t.Fatalf("exit %d, want 2 (usage): %s", code, out)
	}
	if !strings.Contains(out, "greater than 0") {
		t.Errorf("the error must explain the refusal: %s", out)
	}
}

// TestE2EApprovalDecisionsAreOnceOnly: unknown id and re-decide are distinct,
// honest failures.
func TestE2EApprovalDecisionsAreOnceOnly(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "permissions", "approve", "appr_missing"); code != 1 ||
		!strings.Contains(out, "no approval request") {
		t.Errorf("unknown id: exit %d %s", code, out)
	}
	if out, code := run(t, h, "--json", "permissions", "approve", "appr_missing"); code != 1 ||
		!strings.Contains(out, `"error"`) {
		t.Errorf("--json error path must emit a JSON error object: exit %d %s", code, out)
	}
	if out, _ := run(t, h, "--json", "permissions", "pending"); strings.TrimSpace(out) != "[]" {
		t.Errorf("empty pending --json = %q, want []", strings.TrimSpace(out))
	}
	if out, _ := run(t, h, "--json", "permissions", "audit"); strings.TrimSpace(out) != "[]" {
		t.Errorf("empty audit --json = %q, want []", strings.TrimSpace(out))
	}
}

// TestE2ECredentialPathsStillDeniedUnderApprovalGate: the load-bearing property
// of the shipped model survives. A blanket --allow-tool must not unprotect .env,
// and the gate must not turn that structural deny into something approvable.
func TestE2ECredentialPathsStillDeniedUnderApprovalGate(t *testing.T) {
	h := newHome(t)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := startReadFileServer(t, ".env")
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	out, code, hung := runBoundedIn(t, work, h, env, 45*time.Second,
		"run", "read env", "--provider", "local", "--tools",
		"--allow-tool", "read_file", "--approval-gate", "--approval-gate-timeout", "10m")
	if hung {
		t.Fatalf("HANG: a credential-path deny reached the approval gate. output: %s", out)
	}
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "builtin:credentials") {
		t.Errorf("the built-in credential deny must still fire: %s", out)
	}
	if strings.Contains(out, "APPROVAL REQUIRED") {
		t.Errorf("a credential deny must never be offered to a reviewer: %s", out)
	}
	if pending, _ := run(t, h, "--json", "permissions", "pending"); strings.Contains(pending, `"id"`) {
		t.Errorf("a credential deny published an approval request: %s", pending)
	}
}

// ---------------------------------------------------------------------------
// PRD-109 — workflow interrupt / resume
// ---------------------------------------------------------------------------

// TestE2EWorkflowInterruptResume: raise, list, wait (in another process),
// resume, and confirm the operator input round-trips.
func TestE2EWorkflowInterruptResume(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "--json", "workflow", "interrupt", "raise",
		"--session", "sess-1", "--question", "Delete 47 files?", "--context", `{"count":47}`)
	if code != 0 {
		t.Fatalf("raise exit %d: %s", code, out)
	}
	id := firstJSONID(t, out)

	if lst, _ := run(t, h, "workflow", "list", "--filter", "interrupted"); !strings.Contains(lst, "sess-1") ||
		!strings.Contains(lst, "Delete 47 files?") {
		t.Errorf("`workflow list --filter interrupted` must show the open question: %s", lst)
	}

	type res struct {
		out  string
		code int
		hung bool
	}
	done := make(chan res, 1)
	go func() {
		o, c, hung := runBounded(t, h, nil, 45*time.Second,
			"workflow", "interrupt", "wait", id, "--timeout", "30s")
		done <- res{o, c, hung}
	}()
	time.Sleep(500 * time.Millisecond)
	if o, c := run(t, h, "workflow", "resume", id, "--input", "yes, proceed"); c != 0 {
		t.Fatalf("resume exit %d: %s", c, o)
	}
	r := <-done
	if r.hung {
		t.Fatalf("HANG: `workflow interrupt wait` did not release after resume: %s", r.out)
	}
	if r.code != 0 {
		t.Fatalf("wait exit %d, want 0 after a resume: %s", r.code, r.out)
	}
	if !strings.Contains(r.out, "yes, proceed") {
		t.Errorf("the operator input must round-trip: %s", r.out)
	}

	// Re-deciding is refused, so an audit row always maps to one human act.
	if o, c := run(t, h, "workflow", "resume", id, "--input", "changed my mind"); c == 0 ||
		!strings.Contains(o, "already approved") {
		t.Errorf("re-resume: exit %d %s", c, o)
	}
}

// TestE2EWorkflowInterruptWaitIsBounded: the wait times out on its own, records
// timed_out, and exits 3 — distinguishable from a crash (1) and a bad flag (2).
func TestE2EWorkflowInterruptWaitIsBounded(t *testing.T) {
	h := newHome(t)
	out, _ := run(t, h, "--json", "workflow", "interrupt", "raise",
		"--session", "sess-2", "--question", "Deploy?")
	id := firstJSONID(t, out)

	start := time.Now()
	wout, code, hung := runBounded(t, h, nil, 30*time.Second,
		"workflow", "interrupt", "wait", id, "--timeout", "2s")
	if hung {
		t.Fatalf("HANG: a 2s-bounded wait never returned: %s", wout)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("a 2s wait took %s", elapsed)
	}
	if code != 3 {
		t.Fatalf("exit %d, want 3 (ran fine, gate did not pass): %s", code, wout)
	}
	if !strings.Contains(wout, "timed_out") {
		t.Errorf("the outcome must be named: %s", wout)
	}
	// --exit-zero downgrades it for advisory use, without changing the report.
	out2, _ := run(t, h, "--json", "workflow", "interrupt", "raise",
		"--session", "sess-2b", "--question", "Deploy?")
	id2 := firstJSONID(t, out2)
	if o, c, _ := runBounded(t, h, nil, 30*time.Second,
		"workflow", "interrupt", "wait", id2, "--timeout", "1s", "--exit-zero"); c != 0 {
		t.Errorf("--exit-zero: exit %d, want 0: %s", c, o)
	}

	// Unbounded is a usage error, not a default.
	if o, c := run(t, h, "workflow", "interrupt", "wait", id, "--timeout", "0"); c != 2 ||
		!strings.Contains(o, "greater than 0") {
		t.Errorf("--timeout 0: exit %d %s", c, o)
	}
}

// TestE2EWorkflowInterruptJSONShapes pins the --json contract.
func TestE2EWorkflowInterruptJSONShapes(t *testing.T) {
	h := newHome(t)
	for _, args := range [][]string{
		{"--json", "workflow", "interrupt", "list"},
		{"--json", "workflow", "list"},
	} {
		out, code := run(t, h, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d %s", args, code, out)
		}
		if strings.TrimSpace(out) != "[]" {
			t.Errorf("%v: empty --json = %q, want []", args, strings.TrimSpace(out))
		}
	}
	if out, code := run(t, h, "--json", "workflow", "interrupt", "show", "intr_missing"); code != 1 ||
		!strings.Contains(out, `"error"`) {
		t.Errorf("error path must emit a JSON error object: exit %d %s", code, out)
	}
	if out, code := run(t, h, "workflow", "interrupt", "raise", "--session", "s"); code != 2 ||
		!strings.Contains(out, "--question is required") {
		t.Errorf("missing --question: exit %d %s", code, out)
	}
	if out, code := run(t, h, "workflow", "list", "--filter", "sideways"); code != 2 {
		t.Errorf("bad --filter: exit %d %s", code, out)
	}
}

// ---------------------------------------------------------------------------
// PRD-123 — content guardrail / tripwire
// ---------------------------------------------------------------------------

const tripwireBlock = `tripwire:
  preset: standard
  rules:
    - name: no-prod-endpoint
      stage: model_output
      pattern: "https://api\\.prod\\.internal"
      action: block
      message: "reference to the production endpoint"
    - name: pii-warn
      stage: model_output
      pattern: "[0-9]{3}-[0-9]{2}-[0-9]{4}"
      action: warn
      message: "possible US SSN"`

// TestE2ETripwireExitCodes: a fired tripwire is exit 3 — a RESULT, not a crash.
func TestE2ETripwireExitCodes(t *testing.T) {
	h := newHome(t)
	appendTripwire(t, h, tripwireBlock)

	if out, code := run(t, h, "tripwire", "check", "--stage", "model_output",
		"--text", "the build passed"); code != 0 || !strings.Contains(out, "clean") {
		t.Errorf("clean content: exit %d %s", code, out)
	}
	out, code := run(t, h, "tripwire", "check", "--stage", "model_output",
		"--text", "deploy to https://api.prod.internal")
	if code != 3 {
		t.Fatalf("fired tripwire: exit %d, want 3: %s", code, out)
	}
	if !strings.Contains(out, "TRIPWIRE FIRED") || !strings.Contains(out, "no-prod-endpoint") {
		t.Errorf("the finding must be reported: %s", out)
	}
	if o, c := run(t, h, "tripwire", "check", "--stage", "model_output",
		"--text", "deploy to https://api.prod.internal", "--exit-zero"); c != 0 ||
		!strings.Contains(o, "TRIPWIRE FIRED") {
		t.Errorf("--exit-zero must keep the report and drop the code: exit %d %s", c, o)
	}
	// Usage errors stay 2 and never masquerade as a finding.
	if o, c := run(t, h, "tripwire", "check", "--stage", "sideways", "--text", "x"); c != 2 {
		t.Errorf("bad --stage: exit %d %s", c, o)
	}
	if o, c := run(t, h, "tripwire", "check", "--stage", "model_output"); c != 2 {
		t.Errorf("no content source: exit %d %s", c, o)
	}
}

// TestE2ETripwireNoRulesIsHonest: an empty ruleset must NOT report "clean".
// Silently passing an unconfigured guardrail is the fabricated-success failure
// mode this whole PRD exists to prevent.
func TestE2ETripwireNoRulesIsHonest(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "tripwire", "check", "--stage", "model_output", "--text", "anything")
	if code == 0 {
		t.Fatalf("an unconfigured guardrail reported success: %s", out)
	}
	if !strings.Contains(out, "nothing was checked") {
		t.Errorf("the refusal must say nothing was checked: %s", out)
	}
	if o, c := run(t, h, "--json", "tripwire", "list"); c != 0 || strings.TrimSpace(o) != "[]" {
		t.Errorf("empty list --json = %q (exit %d), want []", strings.TrimSpace(o), c)
	}
}

// TestE2ETripwireJSONDistinguishesBlockFromCrash: under --json the verdict
// carries explicit `blocked`/`undecidable` fields, so a consumer never has to
// infer "did it fire or did it die?" from the exit code alone.
func TestE2ETripwireJSONDistinguishesBlockFromCrash(t *testing.T) {
	h := newHome(t)
	appendTripwire(t, h, tripwireBlock)

	out, code := run(t, h, "--json", "tripwire", "check", "--stage", "model_output",
		"--text", "https://api.prod.internal")
	if code != 3 {
		t.Fatalf("exit %d, want 3: %s", code, out)
	}
	var v struct {
		Blocked     bool `json:"blocked"`
		Warned      bool `json:"warned"`
		Undecidable bool `json:"undecidable"`
		Findings    []struct {
			Rule   string `json:"rule"`
			Action string `json:"action"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("verdict is not JSON: %v\n%s", err, out)
	}
	if !v.Blocked || v.Undecidable || len(v.Findings) != 1 {
		t.Fatalf("verdict = %+v, want a clean single-finding block", v)
	}

	// A crash path is a JSON ERROR OBJECT and exit 1/2 — structurally different.
	eout, ecode := run(t, h, "--json", "tripwire", "check", "--stage", "bogus", "--text", "x")
	if ecode == 3 {
		t.Fatalf("a usage error was reported as a finding: %s", eout)
	}
	if !strings.Contains(eout, `"error"`) {
		t.Errorf("error path must emit {\"error\": ...}: %s", eout)
	}
}

// TestE2ETripwireBadConfigIsAStartupError: a guardrail that cannot be loaded
// fails loudly rather than resolving to an empty (and silently permissive) set.
func TestE2ETripwireBadConfigIsAStartupError(t *testing.T) {
	h := newHome(t)
	appendTripwire(t, h, "tripwire:\n  rules:\n    - name: broken\n      pattern: \"([unclosed\"\n      action: block")

	for _, args := range [][]string{
		{"tripwire", "list"},
		{"tripwire", "check", "--stage", "model_output", "--text", "x"},
		{"permissions", "show"},
	} {
		out, code := run(t, h, args...)
		if code == 0 {
			t.Errorf("%v: a malformed guardrail must not load silently: %s", args, out)
		}
		if !strings.Contains(out, "invalid pattern") {
			t.Errorf("%v: the error must name the problem: %s", args, out)
		}
	}
}

// TestE2ETripwireBlocksRealToolCall: the runtime pre-hook. `--allow-tool bash`
// grants the SUBJECT; it must not let banned CONTENT through.
func TestE2ETripwireBlocksRealToolCall(t *testing.T) {
	h := newHome(t)
	appendTripwire(t, h, "tripwire:\n  preset: standard")
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := startBashServer(t, "rm -rf "+victim+"/")
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	out, code, hung := runBounded(t, h, env, 45*time.Second,
		"run", "clean up", "--provider", "local", "--tools", "--allow-tool", "bash")
	if hung {
		t.Fatalf("HANG: %s", out)
	}
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("SIDE EFFECT RAN: the guardrail did not stop `rm -rf`: %v\n%s", err, out)
	}
	if !strings.Contains(out, "guardrail blocked") {
		t.Errorf("the model must be told a guardrail refused it: %s", out)
	}

	hist, _ := run(t, h, "--json", "tripwire", "history")
	if !strings.Contains(hist, "builtin-destructive-command") {
		t.Errorf("the decision must be recorded: %s", hist)
	}

	// The escape hatch works and is explicit.
	out2, _, _ := runBounded(t, h, env, 45*time.Second,
		"run", "clean up", "--provider", "local", "--tools", "--allow-tool", "bash", "--no-tripwire")
	if strings.Contains(out2, "guardrail blocked") {
		t.Errorf("--no-tripwire did not disable the guardrail: %s", out2)
	}
}

// TestE2ETripwireWithholdsSecretInToolResult: the runtime POST hook. The tool
// already ran, so the result is withheld with an honest explanation rather than
// handed to the model or silently emptied.
func TestE2ETripwireWithholdsSecretInToolResult(t *testing.T) {
	h := newHome(t)
	appendTripwire(t, h, "tripwire:\n  preset: standard")
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "creds.txt"),
		[]byte("AKIAIOSFODNN7EXAMPLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := startReadFileServer(t, "creds.txt")
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	out, code, hung := runBoundedIn(t, work, h, env, 45*time.Second,
		"run", "read it", "--provider", "local", "--tools")
	if hung {
		t.Fatalf("HANG: %s", out)
	}
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("the secret reached the transcript: %s", out)
	}
	if !strings.Contains(out, "guardrail blocked") || !strings.Contains(out, "already ran") {
		t.Errorf("the post-hook must admit the tool ran and the output was withheld: %s", out)
	}
}

// TestE2ETripwireDryRun covers FR-10: simulate a call, dispatch nothing.
func TestE2ETripwireDryRun(t *testing.T) {
	h := newHome(t)
	appendTripwire(t, h, "tripwire:\n  preset: standard")
	out, code := run(t, h, "tripwire", "test", "--tool", "bash", "--args", `{"command":"rm -rf /"}`)
	if code != 3 {
		t.Fatalf("exit %d, want 3: %s", code, out)
	}
	if !strings.Contains(out, "builtin-destructive-command") {
		t.Errorf("the dry run must name the rule: %s", out)
	}
	if o, c := run(t, h, "tripwire", "test", "--tool", "bash", "--args", "not json"); c != 2 {
		t.Errorf("bad --args: exit %d %s", c, o)
	}
	if o, c := run(t, h, "tripwire", "test", "--args", "{}"); c != 2 {
		t.Errorf("missing --tool: exit %d %s", c, o)
	}
}

// TestE2ETripwireCounterPersistsAcrossProcesses pins NFR-02: the counter lives
// in SQLite, so a restart cannot silently reset a threshold.
func TestE2ETripwireCounterPersistsAcrossProcesses(t *testing.T) {
	h := newHome(t)
	appendTripwire(t, h, `tripwire:
  rules:
    - name: bash-flood
      type: tripwire
      stage: tool_input
      tool: bash
      threshold: 3
      window: 1h
      action: block
      message: "too many shell calls"`)

	for i := 1; i <= 3; i++ {
		out, code := run(t, h, "tripwire", "test", "--tool", "bash",
			"--args", `{"command":"ls"}`, "--session", "S1")
		switch {
		case i < 3 && code != 0:
			t.Fatalf("call %d fired early (exit %d): %s", i, code, out)
		case i == 3 && code != 3:
			t.Fatalf("call %d did not fire at the threshold (exit %d): %s", i, code, out)
		}
	}
	// A different session keeps its own count.
	if out, code := run(t, h, "tripwire", "test", "--tool", "bash",
		"--args", `{"command":"ls"}`, "--session", "S2"); code != 0 {
		t.Errorf("the counter leaked across sessions (exit %d): %s", code, out)
	}
}
