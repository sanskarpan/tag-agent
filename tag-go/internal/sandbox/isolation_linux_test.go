//go:build linux

package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Linux counterparts of the macOS run-dir tests. They only build and run on
// Linux; on a macOS development host they are compile-checked with
// `GOOS=linux go test -c ./internal/sandbox/`, and CI must actually RUN them.

// TestBuildIsolationRejectsBroadRunDirs is the Linux wiring test: buildIsolation
// must fail closed (exit 127, no plan) for a run dir at or above a boundary,
// because landlockAllowList would otherwise make that whole tree writable.
func TestBuildIsolationRejectsBroadRunDirs(t *testing.T) {
	home, _ := os.UserHomeDir()
	dirs := []string{"/", "/home", "/etc", "/usr", "/var", "/proc", "/root", "/opt"}
	if home != "" {
		dirs = append(dirs, home)
	}
	for _, dir := range dirs {
		plan, failClosed, err := buildIsolation(dir, 10*time.Second)
		if err != nil {
			t.Fatalf("buildIsolation(%q): unexpected error %v", dir, err)
		}
		if failClosed == nil || plan != nil {
			t.Fatalf("buildIsolation(%q) produced a plan; a boundary run dir must fail closed", dir)
		}
		if failClosed.Exit != 127 {
			t.Fatalf("buildIsolation(%q) exit = %d, want 127", dir, failClosed.Exit)
		}
		if !strings.Contains(failClosed.Stderr, "--backend docker") {
			t.Fatalf("buildIsolation(%q) stderr should name the escape hatch, got %q", dir, failClosed.Stderr)
		}
		if !strings.Contains(failClosed.Isolation, "none (failed closed") ||
			!strings.Contains(failClosed.Isolation, "run directory too broad") {
			t.Fatalf("buildIsolation(%q) isolation = %q; it must admit that no layer was applied",
				dir, failClosed.Isolation)
		}
	}
}

// TestBuildIsolationAcceptsScratchDir: a normal scratch dir still produces a
// real plan that reports what it got.
func TestBuildIsolationAcceptsScratchDir(t *testing.T) {
	plan, failClosed, err := buildIsolation(t.TempDir(), 10*time.Second)
	if err != nil {
		t.Fatalf("buildIsolation: %v", err)
	}
	if failClosed != nil {
		t.Fatalf("an ordinary scratch dir was refused: %+v", failClosed)
	}
	if plan == nil || strings.TrimSpace(plan.Isolation) == "" {
		t.Fatal("a plan must always describe the confinement it delivers")
	}
	if !strings.Contains(plan.Isolation, "rlimits") {
		t.Fatalf("plan does not report its rlimit layer: %q", plan.Isolation)
	}
}

// TestExecRejectsBroadRunDirsLinux is the end-to-end half: `--dir /` and
// `--dir $HOME` must fail closed rather than run with the whole tree writable.
func TestExecRejectsBroadRunDirsLinux(t *testing.T) {
	home, _ := os.UserHomeDir()
	dirs := []string{"/", "/home", "/etc"}
	if home != "" {
		dirs = append(dirs, home)
	}
	for _, dir := range dirs {
		res, err := Exec(context.Background(), Options{
			Command: "cat /etc/passwd", Dir: dir, Timeout: 20 * time.Second,
		})
		if err != nil {
			continue // rejected outright: also fail-closed, acceptable
		}
		if res.Exit == 127 && strings.Contains(res.Stderr, "--backend docker") {
			continue // the expected fail-closed answer
		}
		t.Fatalf("run dir %q was accepted: exit=%d stdout=%q isolation=%q",
			dir, res.Exit, res.Stdout, res.Isolation)
	}
}

// TestRlimitPrologueIsOrdered pins the ulimit prologue: hard limits must be
// lowered before soft ones (a lowered hard limit can never be raised again).
func TestRlimitPrologueIsOrdered(t *testing.T) {
	p := rlimitPrologue(30 * time.Second)
	hard := strings.Index(p, "ulimit -H -t 40")
	soft := strings.Index(p, "ulimit -S -t 35")
	if hard < 0 || soft < 0 {
		t.Fatalf("prologue does not set the expected CPU limits: %q", p)
	}
	if hard > soft {
		t.Fatalf("hard CPU limit is lowered after the soft one: %q", p)
	}
	// A sub-second timeout must still produce a valid (>=1s) limit.
	if !strings.Contains(rlimitPrologue(0), "ulimit -H -t 11") {
		t.Fatalf("a zero timeout produced a bogus prologue: %q", rlimitPrologue(0))
	}
}

// TestLandlockAllowListWritesOnlyRunDir: exactly one rule may carry write
// rights over a directory tree, and it must be the run dir.
func TestLandlockAllowListWritesOnlyRunDir(t *testing.T) {
	const runDir = "/tmp/scratch-xyz"
	rules := landlockAllowList(runDir)
	if len(rules) == 0 {
		t.Fatal("empty allow-list")
	}
	if rules[0].path != runDir {
		t.Fatalf("first rule is %q, want the run dir %q", rules[0].path, runDir)
	}
	for _, r := range rules[1:] {
		if r.path == runDir {
			t.Fatalf("run dir appears twice in the allow-list")
		}
		// No other rule may grant directory-creation/removal rights.
		if r.access&llHandledFS(1) & ^uint64(llFSRead|llFSExecute|llFSReadWriteFile) != 0 {
			t.Fatalf("rule for %q grants tree-write rights (%#x); only the run dir may", r.path, r.access)
		}
	}
	for _, denied := range []string{"/etc/passwd", "/etc/shadow", "/root", "/home"} {
		for _, r := range rules {
			if r.path == denied {
				t.Fatalf("allow-list contains %q; it must stay denied", denied)
			}
		}
	}
}
