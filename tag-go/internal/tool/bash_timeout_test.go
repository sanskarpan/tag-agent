package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bashExec returns the bash tool's Exec with the given timeout.
func bashExec(t *testing.T, timeout time.Duration) func(context.Context, map[string]any) (string, error) {
	t.Helper()
	return bashTool(Options{BashTimeout: timeout, MaxReadBytes: 4096}).Exec
}

// TestBashTimeoutKillsBackgroundedChild pins F2: a command that backgrounds a
// long-lived grandchild must still return at the timeout. Killing only the
// direct `sh` leaves the grandchild holding the stdout/stderr pipe, so
// CombinedOutput blocks until the grandchild exits — 40s for a 2s timeout, and
// forever for an unbounded child (`npm run dev &`), which is reachable from
// untrusted model output.
func TestBashTimeoutKillsBackgroundedChild(t *testing.T) {
	run := bashExec(t, 1*time.Second)
	start := time.Now()
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		out, err = run(context.Background(), map[string]any{"command": "sleep 40 & wait"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("bash tool still blocked after 15s on a 1s timeout (F2: backgrounded child hangs the agent)")
	}
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Errorf("returned only after %s; the 1s timeout was not enforced", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected a timeout error, got err=%v out=%q", err, out)
	}
}

// TestBashTimeoutReapsOrphans pins the other half: after the timeout the whole
// process group must be gone, not re-parented to init and left running on the
// host. The child writes a marker file every 200ms; nothing may be written
// after the tool returns.
func TestBashTimeoutReapsOrphans(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "alive")
	run := bashExec(t, 1*time.Second)
	cmd := "(while true; do date +%s%N >> " + marker + "; sleep 0.2; done) & wait"
	done := make(chan struct{})
	go func() { run(context.Background(), map[string]any{"command": cmd}); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("bash tool blocked past 15s")
	}
	before := fileLines(t, marker)
	time.Sleep(1500 * time.Millisecond)
	after := fileLines(t, marker)
	if after != before {
		t.Errorf("background child survived the timeout: marker grew %d -> %d lines", before, after)
	}
}

func fileLines(t *testing.T, p string) int {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
}

// TestBashPlainTimeoutStillWorks guards the case unit tests already covered, so
// the fix does not regress the simple path.
func TestBashPlainTimeoutStillWorks(t *testing.T) {
	run := bashExec(t, 500*time.Millisecond)
	start := time.Now()
	_, err := run(context.Background(), map[string]any{"command": "sleep 20"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("plain sleep timeout took %s", d)
	}
}

// TestBashCapturesOutputAndExit guards the happy paths.
func TestBashCapturesOutputAndExit(t *testing.T) {
	run := bashExec(t, 10*time.Second)
	out, err := run(context.Background(), map[string]any{"command": "echo hi; echo err >&2"})
	if err != nil || !strings.Contains(out, "hi") || !strings.Contains(out, "err") {
		t.Errorf("out=%q err=%v (combined stdout+stderr expected)", out, err)
	}
	if _, err := run(context.Background(), map[string]any{"command": "exit 7"}); err == nil {
		t.Error("non-zero exit must error")
	}
}
