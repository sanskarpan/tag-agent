package cli_test

// End-to-end coverage for the PRD-021 agent-loop lifecycle. Every case drives
// the REAL binary in an isolated TAG_HOME against the offline echo provider —
// no API keys, no network. This repo has a documented history of a green unit
// suite masking dispatch-layer bugs, so the contract is asserted at the process
// boundary: real argv, real exit codes, real cross-process coordination.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bgProc is a `tag` invocation running in the background.
type bgProc struct {
	cmd     *exec.Cmd
	logPath string
	t       *testing.T
}

// startBG launches the binary detached from this test's stdin (no TTY: the
// permission gate and the approval gate must both cope with that) and captures
// its output to a file.
func startBG(t *testing.T, home string, args ...string) *bgProc {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "bg.log")
	lf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(os.Environ(), "TAG_HOME="+home)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &bgProc{cmd: cmd, logPath: logPath, t: t}
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
			_, _ = p.cmd.Process.Wait()
		}
	})
	return p
}

func (p *bgProc) output() string {
	b, _ := os.ReadFile(p.logPath)
	return string(b)
}

// waitExit waits up to limit for the process to exit and returns its code.
// It never uses `perl -e alarm` style bounding: Go ignores unhandled SIGALRM.
func (p *bgProc) waitExit(limit time.Duration) (int, bool) {
	p.t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), true
		}
		if err != nil {
			return -1, true
		}
		return 0, true
	case <-time.After(limit):
		return -1, false
	}
}

func (p *bgProc) signal(sig os.Signal) {
	p.t.Helper()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}

// loopJSON runs `tag --json loop ...` and decodes the object it printed.
func loopJSON(t *testing.T, home string, args ...string) (map[string]any, int) {
	t.Helper()
	out, code := run(t, home, append([]string{"--json"}, args...)...)
	var m map[string]any
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &m); err != nil {
		t.Fatalf("not JSON: %q (%v)", out, err)
	}
	return m, code
}

// firstJSONLine extracts the JSON document from combined stdout+stderr (the
// error path also prints a plain "error:" line to stderr).
func firstJSONLine(out string) string {
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		// Multi-line indented JSON followed by an optional stderr line.
		if i := strings.LastIndex(trimmed, "\n}"); i >= 0 {
			return trimmed[:i+2]
		}
		if i := strings.LastIndex(trimmed, "\n]"); i >= 0 {
			return trimmed[:i+2]
		}
		return strings.SplitN(trimmed, "\n", 2)[0]
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			return line
		}
	}
	return trimmed
}

// waitUntil polls cond until it holds or the limit expires.
func waitUntil(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}

// newestLoop returns the most recent loop's id and status.
func newestLoop(t *testing.T, home string) (string, string, int) {
	t.Helper()
	out, code := run(t, home, "--json", "loop", "list")
	if code != 0 {
		t.Fatalf("loop list exit=%d: %s", code, out)
	}
	var runs []map[string]any
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &runs); err != nil {
		t.Fatalf("loop list not JSON: %q", out)
	}
	if len(runs) == 0 {
		return "", "", 0
	}
	iter, _ := runs[0]["current_iter"].(float64)
	return runs[0]["id"].(string), runs[0]["status"].(string), int(iter)
}

// ---------------------------------------------------------------------------

// TestE2ELoopJSONContracts pins the --json surface and the exit-code ladder.
func TestE2ELoopJSONContracts(t *testing.T) {
	h := newHome(t)

	// Empty list must be [] and not null (a null breaks `jq length`).
	out, code := run(t, h, "--json", "loop", "list")
	if code != 0 {
		t.Fatalf("empty list exit=%d: %s", code, out)
	}
	if got := strings.TrimSpace(firstJSONLine(out)); got != "[]" {
		t.Fatalf("empty `loop list --json` = %q, want []", got)
	}

	// Usage failures are exit 2 and still emit a JSON error object.
	m, code := loopJSON(t, h, "loop", "start")
	if code != 2 {
		t.Errorf("`loop start` with no --goal exit=%d, want 2", code)
	}
	if _, ok := m["error"]; !ok {
		t.Errorf("missing error object: %v", m)
	}
	if _, code := run(t, h, "loop", "start", "--goal", "x", "--approval", "bogus"); code != 2 {
		t.Errorf("bad --approval exit=%d, want 2", code)
	}

	// Runtime failures are exit 1 with a JSON error object.
	m, code = loopJSON(t, h, "loop", "start", "--goal", "x", "--max-iters", "0")
	if code != 1 {
		t.Errorf("--max-iters 0 exit=%d, want 1", code)
	}
	if s, _ := m["error"].(string); !strings.Contains(s, "max-iters") {
		t.Errorf("error = %v", m)
	}
	m, code = loopJSON(t, h, "loop", "status", "does-not-exist")
	if code != 1 {
		t.Errorf("status of unknown loop exit=%d, want 1", code)
	}
	if _, ok := m["error"]; !ok {
		t.Errorf("status error object missing: %v", m)
	}
	m, code = loopJSON(t, h, "loop", "abort", "does-not-exist")
	if code != 1 || m["error"] == nil {
		t.Errorf("abort unknown: exit=%d %v", code, m)
	}
	m, code = loopJSON(t, h, "loop", "approve", "does-not-exist")
	if code != 1 || m["error"] == nil {
		t.Errorf("approve unknown: exit=%d %v", code, m)
	}

	// Missing positional arg is a usage error.
	if _, code := run(t, h, "loop", "status"); code != 2 {
		t.Errorf("`loop status` with no id exit=%d, want 2", code)
	}
}

// TestE2ELoopOfflineEchoLifecycle: start/list/status against `--provider echo`.
func TestE2ELoopOfflineEchoLifecycle(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "loop", "start", "--goal", "tidy the repo",
		"--max-iters", "3", "--provider", "echo")
	if code != 0 {
		t.Fatalf("loop start exit=%d: %s", code, out)
	}
	// Offline honesty: the echo run must say plainly that it is degraded.
	if !strings.Contains(out, "OFFLINE") {
		t.Errorf("echo run did not disclose it was degraded:\n%s", out)
	}
	// ...and must NOT fabricate success from the echoed "Output GOAL_ACHIEVED".
	if strings.Contains(out, "finished: completed") {
		t.Errorf("echo run reported a completed goal — fake success:\n%s", out)
	}

	id, status, iters := newestLoop(t, h)
	if id == "" {
		t.Fatal("loop list did not persist the run")
	}
	if status != "max_iters" || iters != 3 {
		t.Fatalf("list shows status=%q iters=%d, want max_iters 3", status, iters)
	}

	// status --json exposes the run plus its journal with stable field names.
	m, code := loopJSON(t, h, "loop", "status", id)
	if code != 0 {
		t.Fatalf("status exit=%d", code)
	}
	for _, k := range []string{"id", "profile", "goal", "max_iters", "current_iter",
		"status", "approval", "created_at", "updated_at", "iterations"} {
		if _, ok := m[k]; !ok {
			t.Errorf("status --json missing field %q", k)
		}
	}
	its, _ := m["iterations"].([]any)
	if len(its) != 3 {
		t.Fatalf("journal has %d iterations, want 3", len(its))
	}

	// Plain-text status renders the journal too.
	out, code = run(t, h, "loop", "status", id)
	if code != 0 || !strings.Contains(out, "Iterations: 3/3") {
		t.Errorf("text status: exit=%d\n%s", code, out)
	}
}

// TestE2ELoopLegacySingleShotStillWorks: adding lifecycle subcommands must not
// break the pre-existing `loop <prompt> --iterations N` driver.
func TestE2ELoopLegacySingleShotStillWorks(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "loop", "--provider", "echo", "--iterations", "2", "hello loop")
	if code != 0 {
		t.Fatalf("legacy loop exit=%d: %s", code, out)
	}
	if strings.Count(out, "hello loop") != 2 {
		t.Errorf("legacy loop output:\n%s", out)
	}
}

// TestE2ELoopAbortFromAnotherProcess is the headline cross-process requirement:
// a `tag loop abort` issued from a SECOND process stops a live loop.
func TestE2ELoopAbortFromAnotherProcess(t *testing.T) {
	h := newHome(t)
	bg := startBG(t, h, "loop", "start", "--goal", "long running",
		"--max-iters", "100000", "--provider", "echo")

	var id string
	waitUntil(t, 30*time.Second, "the loop to start iterating", func() bool {
		lid, st, iters := newestLoop(t, h)
		id = lid
		return lid != "" && st == "running" && iters > 0
	})

	m, code := loopJSON(t, h, "loop", "abort", id)
	if code != 0 {
		t.Fatalf("abort exit=%d: %v", code, m)
	}
	if m["status"] != "aborted" || m["loop_id"] != id {
		t.Errorf("abort --json = %v", m)
	}

	exit, ok := bg.waitExit(30 * time.Second)
	if !ok {
		t.Fatalf("loop process did not exit after an out-of-process abort\n%s", bg.output())
	}
	if exit != 0 {
		t.Errorf("aborted loop exit=%d, want 0\n%s", exit, bg.output())
	}
	_, st, _ := newestLoop(t, h)
	if st != "aborted" {
		t.Fatalf("persisted status = %q, want aborted", st)
	}
}

// TestE2ELoopApproveDenyCrossProcess drives the --approval human gate entirely
// from other processes: approve once, then deny.
func TestE2ELoopApproveDenyCrossProcess(t *testing.T) {
	h := newHome(t)
	bg := startBG(t, h, "loop", "start", "--goal", "gated work", "--max-iters", "4",
		"--approval", "human", "--approval-timeout", "60s", "--provider", "echo")

	var id string
	waitUntil(t, 30*time.Second, "the loop to park on its first checkpoint", func() bool {
		lid, st, _ := newestLoop(t, h)
		id = lid
		return lid != "" && st == "waiting_approval"
	})

	// The gate is visible to an operator, not a silent block.
	m, code := loopJSON(t, h, "loop", "status", id)
	if code != 0 {
		t.Fatalf("status exit=%d", code)
	}
	pending, _ := m["pending_approval"].(map[string]any)
	if pending == nil || pending["decision"] != "pending" {
		t.Fatalf("status --json does not surface the pending checkpoint: %v", m)
	}

	m, code = loopJSON(t, h, "loop", "approve", id)
	if code != 0 || m["decision"] != "continue" {
		t.Fatalf("approve exit=%d %v", code, m)
	}

	waitUntil(t, 30*time.Second, "the loop to reach its second checkpoint", func() bool {
		_, st, iters := newestLoop(t, h)
		return st == "waiting_approval" && iters >= 2
	})

	m, code = loopJSON(t, h, "loop", "deny", id)
	if code != 0 || m["decision"] != "abort" {
		t.Fatalf("deny exit=%d %v", code, m)
	}

	if _, ok := bg.waitExit(30 * time.Second); !ok {
		t.Fatalf("denied loop did not stop\n%s", bg.output())
	}
	_, st, iters := newestLoop(t, h)
	if st != "aborted" {
		t.Fatalf("status after deny = %q, want aborted", st)
	}
	if iters != 2 {
		t.Fatalf("denied loop ran %d iterations, want 2 (the deny must gate iteration 3)", iters)
	}
}

// TestE2ELoopBackgroundApprovalDoesNotHang: a loop with no TTY and nobody to
// answer must time out and stop, not block forever.
func TestE2ELoopBackgroundApprovalDoesNotHang(t *testing.T) {
	h := newHome(t)
	bg := startBG(t, h, "loop", "start", "--goal", "unattended", "--max-iters", "5",
		"--approval", "human", "--approval-timeout", "2s", "--provider", "echo")

	exit, ok := bg.waitExit(60 * time.Second)
	if !ok {
		t.Fatalf("unattended --approval human loop HUNG with no TTY\n%s", bg.output())
	}
	if exit != 0 {
		t.Errorf("exit=%d, want 0\n%s", exit, bg.output())
	}
	if !strings.Contains(bg.output(), "timed out") {
		t.Errorf("the timeout was not reported plainly:\n%s", bg.output())
	}
	_, st, _ := newestLoop(t, h)
	if st != "aborted" {
		t.Fatalf("status = %q, want aborted after an unanswered gate", st)
	}
}

// TestE2ELoopSIGTERMLeavesNothingRunning is the #574 guard.
func TestE2ELoopSIGTERMLeavesNothingRunning(t *testing.T) {
	h := newHome(t)
	bg := startBG(t, h, "loop", "start", "--goal", "interrupt me",
		"--max-iters", "100000", "--provider", "echo")
	waitUntil(t, 30*time.Second, "the loop to start iterating", func() bool {
		_, st, iters := newestLoop(t, h)
		return st == "running" && iters > 0
	})
	bg.signal(syscall.SIGTERM)
	if _, ok := bg.waitExit(30 * time.Second); !ok {
		t.Fatalf("loop survived SIGTERM\n%s", bg.output())
	}
	_, st, _ := newestLoop(t, h)
	if st == "running" {
		t.Fatal("SIGTERM stranded the loop in 'running'")
	}
	if st != "aborted" {
		t.Fatalf("status after SIGTERM = %q, want aborted", st)
	}
}

// TestE2ELoopSIGTERMDuringApprovalWait: the same guarantee while parked.
func TestE2ELoopSIGTERMDuringApprovalWait(t *testing.T) {
	h := newHome(t)
	bg := startBG(t, h, "loop", "start", "--goal", "gated", "--max-iters", "5",
		"--approval", "human", "--approval-timeout", "300s", "--provider", "echo")
	waitUntil(t, 30*time.Second, "the approval checkpoint", func() bool {
		_, st, _ := newestLoop(t, h)
		return st == "waiting_approval"
	})
	bg.signal(syscall.SIGTERM)
	if _, ok := bg.waitExit(30 * time.Second); !ok {
		t.Fatalf("loop survived SIGTERM while gated\n%s", bg.output())
	}
	_, st, _ := newestLoop(t, h)
	if st == "running" || st == "waiting_approval" {
		t.Fatalf("SIGTERM left the loop in a non-terminal status %q", st)
	}
}

// TestE2ELoopDetach exercises the Python-shaped background start, including its
// cross-process abort.
func TestE2ELoopDetach(t *testing.T) {
	h := newHome(t)
	m, code := loopJSON(t, h, "loop", "start", "--detach", "--goal", "background work",
		"--max-iters", "100000", "--provider", "echo")
	if code != 0 {
		t.Fatalf("detached start exit=%d: %v", code, m)
	}
	for _, k := range []string{"loop_id", "pid", "status"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("detached start --json missing %q: %v", k, m)
		}
	}
	if m["status"] != "running" {
		t.Errorf("status = %v", m["status"])
	}
	id := m["loop_id"].(string)
	pid := int(m["pid"].(float64))

	waitUntil(t, 30*time.Second, "the detached worker to iterate", func() bool {
		_, _, iters := newestLoop(t, h)
		return iters > 0
	})
	if _, code := run(t, h, "--json", "loop", "abort", id); code != 0 {
		t.Fatalf("abort of detached loop exit=%d", code)
	}
	waitUntil(t, 30*time.Second, "the detached worker to exit", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
	_, st, _ := newestLoop(t, h)
	if st != "aborted" {
		t.Fatalf("detached loop status = %q, want aborted", st)
	}
	// Unlike Python's DEVNULL worker, the detached run leaves a readable log.
	if _, err := os.Stat(filepath.Join(h, "logs", "loop-"+id+".log")); err != nil {
		t.Errorf("no worker log: %v", err)
	}
}

// TestE2ELoopConcurrentLoopsDoNotShareState runs two loops at once and checks
// their journals stay separate.
func TestE2ELoopConcurrentLoopsDoNotShareState(t *testing.T) {
	h := newHome(t)
	a := startBG(t, h, "loop", "start", "--goal", "alpha", "--max-iters", "5", "--provider", "echo")
	b := startBG(t, h, "loop", "start", "--goal", "beta", "--max-iters", "5", "--provider", "echo")
	if _, ok := a.waitExit(60 * time.Second); !ok {
		t.Fatalf("loop A hung\n%s", a.output())
	}
	if _, ok := b.waitExit(60 * time.Second); !ok {
		t.Fatalf("loop B hung\n%s", b.output())
	}
	out, code := run(t, h, "--json", "loop", "list")
	if code != 0 {
		t.Fatal(out)
	}
	var runs []map[string]any
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(runs))
	}
	goals := map[string]bool{}
	for _, r := range runs {
		goals[r["goal"].(string)] = true
		if r["status"] != "max_iters" || r["current_iter"].(float64) != 5 {
			t.Errorf("run %v did not complete its own 5 iterations", r)
		}
	}
	if !goals["alpha"] || !goals["beta"] {
		t.Fatalf("goals = %v", goals)
	}
}
