package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are regression tests for the `restricted` backend having been a plain
// host command runner: before the isolation work, `sandbox run "curl ..."`
// reached the internet and `sandbox run "cat /etc/passwd"` printed the file.
// Each test below states its own precondition and SKIPS (loudly) rather than
// passing when the host cannot answer the question.

// hostCanRun reports whether the given shell command succeeds unsandboxed. It
// is used as a precondition so a probe that is broken for unrelated reasons
// (no curl, no network in CI) skips the test instead of faking a pass.
func hostCanRun(t *testing.T, script string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "sh", "-c", script).Run() == nil
}

// netProbe returns a shell command that exits 0 only if outbound network works.
func netProbe(t *testing.T) string {
	t.Helper()
	candidates := []struct{ bin, script string }{
		{"curl", "curl -sS -m 8 -o /dev/null https://example.com"},
		{"wget", "wget -q -T 8 -O /dev/null https://example.com"},
		{"nc", "nc -z -w 8 1.1.1.1 443"},
		{"python3", `python3 -c 'import socket;socket.create_connection(("1.1.1.1",443),8).close()'`},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c.bin); err == nil {
			return c.script
		}
	}
	return ""
}

// fsConfined reports whether the isolation string claims filesystem confinement.
// On Linux without Landlock the backend honestly says it is NOT confined; the
// filesystem tests then skip instead of asserting a guarantee we never claimed.
func fsConfined(iso string) bool {
	return !strings.Contains(iso, "filesystem NOT confined")
}

func netConfined(iso string) bool {
	return !strings.Contains(iso, "network NOT blocked")
}

// execRunnable runs Exec and, when the platform refuses purely because it cannot
// confine the FILESYSTEM (a Linux kernel with no Landlock), retries with the
// explicit AllowUnconfined opt-in.
//
// It exists for the behavioural tests below -- run dir usable, timeouts, process
// groups -- which are about the runner, not about filesystem confinement, and
// which would otherwise all collapse into the same fail-closed 127 on a
// Landlock-less kernel and stop testing anything. The tests that assert
// confinement itself deliberately do NOT use this; they call Exec directly and
// check what the isolation string claims.
func execRunnable(t *testing.T, opts Options) (*Result, error) {
	t.Helper()
	res, err := Exec(context.Background(), opts)
	if err != nil || res.Exit != 127 || res.Isolation != landlockUnavailableIsolation {
		return res, err
	}
	t.Logf("this kernel cannot confine the filesystem (%s); re-running with AllowUnconfined "+
		"to exercise the runner", res.Stderr)
	opts.AllowUnconfined = true
	return Exec(context.Background(), opts)
}

// TestExecReportsIsolation pins the contract that every run says what it got.
func TestExecReportsIsolation(t *testing.T) {
	res, err := execRunnable(t, Options{Command: "echo hi", Dir: t.TempDir(), Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Isolation) == "" {
		t.Fatal("Result.Isolation is empty: a run must always report what confinement it received")
	}
	if res.Exit != 0 {
		t.Fatalf("echo failed under isolation: %+v", res)
	}
}

// TestExecDeniesNetworkEgress is the headline regression: the pre-fix backend
// let `curl https://example.com` succeed with exit 0.
func TestExecDeniesNetworkEgress(t *testing.T) {
	probe := netProbe(t)
	if probe == "" {
		t.Skip("no network probe tool (curl/wget/nc/python3) available on this host")
	}
	if !hostCanRun(t, probe) {
		t.Skipf("host itself has no outbound network (probe %q failed unsandboxed); cannot prove denial", probe)
	}
	res, err := execRunnable(t, Options{Command: probe, Dir: t.TempDir(), Timeout: 25 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !netConfined(res.Isolation) {
		t.Skipf("this platform/kernel reports no network confinement (%s); nothing to assert", res.Isolation)
	}
	if res.Exit == 0 {
		t.Fatalf("network egress SUCCEEDED inside the sandbox (probe %q, isolation %q): the sandbox does not isolate the network",
			probe, res.Isolation)
	}
}

// TestExecDeniesEtcPasswd is the second headline regression: the pre-fix
// backend happily printed /etc/passwd.
func TestExecDeniesEtcPasswd(t *testing.T) {
	target := "/etc/passwd"
	if _, err := os.ReadFile(target); err != nil {
		t.Skipf("host cannot read %s anyway (%v); cannot prove denial", target, err)
	}
	res, err := Exec(context.Background(), Options{Command: "cat " + target, Dir: t.TempDir(), Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !fsConfined(res.Isolation) {
		t.Skipf("this platform/kernel reports no filesystem confinement (%s); nothing to assert", res.Isolation)
	}
	if res.Exit == 0 {
		t.Fatalf("reading %s SUCCEEDED inside the sandbox (isolation %q, stdout %q)", target, res.Isolation, res.Stdout)
	}
	if strings.Contains(res.Stdout, "root:") {
		t.Fatalf("%s contents leaked into sandbox stdout: %q", target, res.Stdout)
	}
}

// assertRunDirUsable checks the re-allow: the sandbox must still be able to
// read and write files in its own run directory.
func assertRunDirUsable(t *testing.T, dir string) {
	t.Helper()
	res, err := execRunnable(t, Options{
		Command: `echo sentinel-value > out.txt && cat out.txt`,
		Dir:     dir,
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec in %s: %v", dir, err)
	}
	if res.Exit != 0 {
		t.Fatalf("run dir %s is not read/writable inside the sandbox: exit=%d stderr=%q isolation=%q",
			dir, res.Exit, res.Stderr, res.Isolation)
	}
	if strings.TrimSpace(res.Stdout) != "sentinel-value" {
		t.Fatalf("run dir read-back mismatch in %s: stdout=%q", dir, res.Stdout)
	}
	real, err := confineDir(dir)
	if err != nil {
		t.Fatalf("confineDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "out.txt")); err != nil {
		t.Fatalf("file written inside the sandbox is missing on the host: %v", err)
	}
}

func TestExecRunDirIsReadWritable(t *testing.T) {
	assertRunDirUsable(t, t.TempDir())
}

// TestExecRunDirUnderHomeIsReadWritable covers the ordering trap: the profile
// denies reads of the whole $HOME subtree, so a scratch dir under $HOME only
// works if the run-dir re-allow comes AFTER that denial.
func TestExecRunDirUnderHomeIsReadWritable(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available")
	}
	dir, err := os.MkdirTemp(home, "tag-sbx-home-")
	if err != nil {
		t.Skipf("cannot create a scratch dir under %s: %v", home, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	assertRunDirUsable(t, dir)
}

// TestExecRunDirWithHostilePunctuation is the policy-injection regression: the
// run dir is interpolated into an SBPL profile on macOS (and an env-encoded
// policy on Linux). A directory whose name contains a quote or a backslash must
// either work correctly or be rejected -- it must never silently weaken the
// policy. We assert BOTH that the run dir still works and that network denial
// survives, since a successful injection would re-allow the network.
func TestExecRunDirWithHostilePunctuation(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{`quote"dir`, `back\slash`, `both"and\slash`} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(base, name)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Skipf("filesystem rejects %q: %v", name, err)
			}
			res, err := execRunnable(t, Options{
				Command: `echo ok > f.txt && cat f.txt`,
				Dir:     dir,
				Timeout: 20 * time.Second,
			})
			if err != nil {
				// Rejecting the path outright is an acceptable, fail-closed answer.
				t.Logf("run dir %q rejected (fail-closed, acceptable): %v", name, err)
				return
			}
			if res.Exit != 0 || strings.TrimSpace(res.Stdout) != "ok" {
				t.Fatalf("run dir %q unusable: exit=%d stdout=%q stderr=%q", name, res.Exit, res.Stdout, res.Stderr)
			}
			// The policy must still hold for a hostile path name.
			probe, err2 := execRunnable(t, Options{Command: "cat /etc/passwd", Dir: dir, Timeout: 20 * time.Second})
			if err2 != nil {
				t.Fatalf("Exec: %v", err2)
			}
			if fsConfined(probe.Isolation) && probe.Exit == 0 {
				t.Fatalf("policy injection: /etc/passwd readable from run dir %q (isolation %q)", name, probe.Isolation)
			}
		})
	}
}

// pgrepCount returns how many processes match the given full-command pattern.
func pgrepCount(t *testing.T, pattern string) int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", pattern).Output()
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// TestExecTimeoutKillsProcessGroup is the regression for a --timeout that did
// not actually terminate the run: CommandContext only kills the direct child
// (`sandbox-exec`), so a backgrounded grandchild kept the stdout pipe open,
// cmd.Wait blocked FOREVER and `tag` never returned -- while the grandchild was
// re-parented to init and kept running on the host.
//
// The command backgrounds a long sleep with a marker only this test uses, so
// the orphan check cannot collide with unrelated processes.
func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available; cannot check for orphans")
	}
	dir := t.TempDir()
	// The backgrounded sleep deliberately INHERITS stdout/stderr: that is what
	// used to wedge cmd.Wait forever. The unusual duration doubles as the
	// orphan-search key.
	script := "sleep 97 & echo started"

	done := make(chan *Result, 1)
	go func() {
		res, err := execRunnable(t, Options{Command: script, Dir: dir, Timeout: 1 * time.Second})
		if err != nil {
			done <- nil
			return
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res == nil {
			t.Fatal("Exec returned an error for a timing-out command")
		}
		if !res.TimedOut || res.Exit != 124 {
			t.Fatalf("expected a 124/timed-out result, got %+v", res)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Exec did not return within 20s for a --timeout 1 run: the timeout does not terminate the run")
	}
	// The whole process group must be gone: no orphaned `sleep 97`.
	time.Sleep(500 * time.Millisecond)
	if n := pgrepCount(t, "sleep 97"); n > 0 {
		_ = exec.Command("pkill", "-f", "sleep 97").Run()
		t.Fatalf("%d orphaned `sleep 97` process(es) survived the timeout: the process group was not reaped", n)
	}
}

// TestExecReapsBackgroundedChildOnCleanExit: even without a timeout, a command
// that backgrounds something and exits must not leak that grandchild onto the
// host (it used to be re-parented to init and keep running).
func TestExecReapsBackgroundedChildOnCleanExit(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available; cannot check for orphans")
	}
	res, err := execRunnable(t, Options{
		Command: "sleep 93 >/dev/null 2>&1 </dev/null & echo started",
		Dir:     t.TempDir(),
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "started" {
		t.Fatalf("unexpected stdout %q", res.Stdout)
	}
	time.Sleep(500 * time.Millisecond)
	if n := pgrepCount(t, "sleep 93"); n > 0 {
		_ = exec.Command("pkill", "-f", "sleep 93").Run()
		t.Fatalf("%d orphaned `sleep 93` process(es) leaked from a completed sandbox run", n)
	}
}

// TestExecTimeoutSimpleCommand pins the plain case that already worked, so the
// process-group change cannot regress it.
func TestExecTimeoutSimpleCommand(t *testing.T) {
	start := time.Now()
	res, err := execRunnable(t, Options{Command: "sleep 5", Dir: t.TempDir(), Timeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut || res.Exit != 124 {
		t.Fatalf("expected timed_out with exit 124, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("a 1s timeout took %s to return", elapsed)
	}
}

// TestExecRejectsControlCharsInRunDir: a path we cannot safely encode into a
// policy must be refused, never run unconfined.
func TestExecRejectsControlCharsInRunDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "new\nline")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Skipf("filesystem rejects newline in a directory name: %v", err)
	}
	res, err := Exec(context.Background(), Options{Command: "echo hi", Dir: dir, Timeout: 20 * time.Second})
	if err != nil {
		return // rejected outright: correct
	}
	if res.Exit == 0 {
		t.Fatalf("a run dir containing a newline was accepted and run (isolation %q); it must be rejected or fail closed", res.Isolation)
	}
}
