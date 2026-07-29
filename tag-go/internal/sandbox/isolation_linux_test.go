//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
	// Both opt-in states are checked: --allow-unconfined waives FILESYSTEM
	// confinement only, and must never be mistakable for a waiver of the run-dir
	// boundary check -- `--dir /` stays refused even with it.
	for _, allowUnconfined := range []bool{false, true} {
		for _, dir := range dirs {
			plan, failClosed, err := buildIsolation(dir, 10*time.Second, allowUnconfined)
			if err != nil {
				t.Fatalf("buildIsolation(%q, allowUnconfined=%v): unexpected error %v", dir, allowUnconfined, err)
			}
			if failClosed == nil || plan != nil {
				t.Fatalf("buildIsolation(%q, allowUnconfined=%v) produced a plan; a boundary run dir must fail closed",
					dir, allowUnconfined)
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
}

// TestBuildIsolationAcceptsScratchDir: a normal scratch dir still produces a
// real plan that reports what it got.
func TestBuildIsolationAcceptsScratchDir(t *testing.T) {
	plan, failClosed, err := buildIsolation(t.TempDir(), 10*time.Second, true)
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

// TestLandlockFileRulesUseFileOnlyRights is the regression test for the bug that
// made the sandbox 100% unusable on every Landlock-capable kernel: the
// allow-list handed directory-only rights (READ_DIR, MAKE_*, REMOVE_*) to
// regular files such as /etc/ld.so.cache, and landlock_add_rule answered EINVAL
// for the whole policy -- so every sandboxed run failed closed with exit 126.
//
// It runs on ANY kernel, Landlock or not: nothing here needs the LSM, only
// stat(2). A future edit that adds a file entry with a directory mask, or that
// removes the narrowing in apply(), fails here instead of only on a real kernel.
func TestLandlockFileRulesUseFileOnlyRights(t *testing.T) {
	for _, abi := range []int{1, 2, 3, 4, 5, 6} {
		fileMask := llFileAccessMask(abi)
		want := uint64(llFSFileV1)
		if abi >= 3 {
			want |= llFSTruncate
		}
		if abi >= 5 {
			want |= llFSIoctlDev
		}
		if fileMask != want {
			t.Fatalf("abi %d: file mask %#x, want %#x", abi, fileMask, want)
		}
		// A directory keeps everything; a non-directory keeps only the subset.
		full := llHandledFS(abi)
		if got := llAccessForType(full, true, abi); got != full {
			t.Fatalf("abi %d: a directory lost rights: %#x -> %#x", abi, full, got)
		}
		if got := llAccessForType(full, false, abi); got != fileMask {
			t.Fatalf("abi %d: a non-directory kept %#x, want the file mask %#x", abi, got, fileMask)
		}
		// The narrowing must actually drop something, or it is not doing its job.
		if full&^fileMask == 0 {
			t.Fatalf("abi %d: no directory-only rights exist in %#x; the mask is vacuous", abi, full)
		}
	}

	// And the real allow-list, against the real filesystem of whatever host this
	// runs on: every entry that exists and is NOT a directory must end up with a
	// mask the kernel will accept, and must not be emptied out by the narrowing.
	abi := 6
	handled := llHandledFS(abi)
	fileMask := llFileAccessMask(abi)
	sawNonDir := false
	for _, r := range landlockAllowList(t.TempDir()) {
		fi, err := os.Stat(r.path) // follows symlinks, like the O_PATH fd apply() uses
		if err != nil {
			continue // not present on this distro; apply() skips it too
		}
		if fi.IsDir() {
			continue
		}
		sawNonDir = true
		// Go through the same open+fstat path apply() uses, so a regression that
		// removes the narrowing there is caught here and not only on a kernel
		// that actually has Landlock.
		pfd, err := unix.Open(r.path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		got, isDir, err := llAccessForFD(pfd, r.access&handled, abi)
		unix.Close(pfd)
		if err != nil {
			t.Fatalf("llAccessForFD(%s): %v", r.path, err)
		}
		if isDir {
			continue // raced with os.Stat; not this test's business
		}
		if got&^fileMask != 0 {
			t.Fatalf("%s is not a directory (mode %v) yet would be sent %#x, which contains the "+
				"directory-only rights %#x -- landlock_add_rule returns EINVAL and the whole policy fails",
				r.path, fi.Mode(), got, got&^fileMask)
		}
		if got == 0 {
			t.Fatalf("%s ends up with no access rights at all; it was granted %#x, none of which is a file right",
				r.path, r.access)
		}
	}
	if !sawNonDir {
		t.Log("no non-directory allow-list entry exists on this host")
	}

	// The other half of the contract: a real directory (the run dir, the one rule
	// that MUST keep its write rights) is not narrowed at all.
	runDir := t.TempDir()
	dfd, err := unix.Open(runDir, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open %s: %v", runDir, err)
	}
	defer unix.Close(dfd)
	got, isDir, err := llAccessForFD(dfd, handled, abi)
	if err != nil {
		t.Fatalf("llAccessForFD(%s): %v", runDir, err)
	}
	if !isDir {
		t.Fatalf("%s is not seen as a directory", runDir)
	}
	if got != handled {
		t.Fatalf("the run dir lost rights: %#x -> %#x (dropped %#x); it is the sandbox's only writable tree",
			handled, got, handled&^got)
	}
	if got&unix.LANDLOCK_ACCESS_FS_MAKE_REG == 0 || got&unix.LANDLOCK_ACCESS_FS_WRITE_FILE == 0 {
		t.Fatalf("the run dir cannot create or write files: %#x", got)
	}
}

// TestLandlockClaimDisclosesProcExposure is FIX 2: the allow-list grants read
// over ALL of /proc, so another same-uid process's /proc/<pid>/environ (API keys
// and all) is readable from inside the sandbox. Landlock cannot scope that
// per-pid, so the claim must at minimum SAY so -- it previously talked only
// about $HOME and /root and was silent on /proc.
func TestLandlockClaimDisclosesProcExposure(t *testing.T) {
	runDir := t.TempDir()
	rules := landlockAllowList(runDir)
	if !grantsProcRead(rules) {
		t.Skip("/proc is no longer granted; the disclosure requirement is moot")
	}
	plan, failClosed, err := buildIsolation(runDir, 10*time.Second, true)
	if err != nil || failClosed != nil {
		t.Fatalf("buildIsolation: err=%v failClosed=%+v", err, failClosed)
	}
	if !strings.Contains(plan.Isolation, "Landlock ABI") {
		t.Skipf("no Landlock on this kernel (%s); nothing claims a filesystem policy", plan.Isolation)
	}
	if !strings.Contains(plan.Isolation, procReadDisclosure) {
		t.Fatalf("the allow-list grants read over all of /proc but the isolation string does not "+
			"disclose it: %q", plan.Isolation)
	}
	// Both plans go out to users; the weak (no-netns) one must disclose too.
	if plan.Alt != nil && !strings.Contains(plan.Alt.Isolation, procReadDisclosure) {
		t.Fatalf("the fallback plan hides the /proc exposure: %q", plan.Alt.Isolation)
	}
}

// TestBuildIsolationCarriesTheFailClosedContract is FIX 3: when a Landlock
// helper is in the argv, the plan must know that exit 126 + the helper marker
// means "the policy was never installed", and the string it reports then must
// claim nothing. Start() has already succeeded at that point, so runPlan never
// falls back to Alt and this is the only thing standing between the user and a
// description of a ruleset that does not exist.
func TestBuildIsolationCarriesTheFailClosedContract(t *testing.T) {
	plan, failClosed, err := buildIsolation(t.TempDir(), 10*time.Second, true)
	if err != nil || failClosed != nil {
		t.Fatalf("buildIsolation: err=%v failClosed=%+v", err, failClosed)
	}
	if len(plan.Prefix) == 0 {
		t.Skip("no Landlock helper on this kernel; there is no post-Start failure mode to report")
	}
	for name, p := range map[string]*isolationPlan{"strong": plan, "weak": plan.Alt} {
		if p == nil {
			continue
		}
		if p.FailClosedExit != helperExitPolicy || p.FailClosedMarker != helperFailMarker {
			t.Fatalf("%s plan does not recognise a helper failure: exit=%d marker=%q",
				name, p.FailClosedExit, p.FailClosedMarker)
		}
		got := resolveIsolation(p, helperExitPolicy, helperFailMarker+" landlock_restrict_self: EPERM")
		if strings.Contains(got, "Landlock ABI") {
			t.Fatalf("%s plan still claims a Landlock ABI after the policy failed to install: %q", name, got)
		}
		if !strings.Contains(got, "none (failed closed") {
			t.Fatalf("%s plan does not fail closed in its isolation string: %q", name, got)
		}
		// A command that exits 126 on its own must keep the real claim.
		if own := resolveIsolation(p, helperExitPolicy, "sh: ./x: Permission denied"); own != p.Isolation {
			t.Fatalf("%s plan mislabels a command's own 126 as a helper failure: %q", name, own)
		}
	}
}

// TestLandlockAllowListMarksRunDirRequired is FIX 4's data half: exactly one
// rule -- the run dir -- may be required, because it is the only one whose
// absence breaks the sandbox instead of tightening it.
func TestLandlockAllowListMarksRunDirRequired(t *testing.T) {
	const runDir = "/tmp/scratch-xyz"
	rules := landlockAllowList(runDir)
	var required []string
	for _, r := range rules {
		if r.required {
			required = append(required, r.path)
		}
	}
	if len(required) != 1 || required[0] != runDir {
		t.Fatalf("required rules = %v, want exactly [%s]", required, runDir)
	}
}

// TestCheckRequiredPathsRejectsAMissingRunDir is FIX 4's behaviour half. A
// missing run dir used to `continue` past the rule, leaving the sandbox with NO
// writable tree while still claiming "writes confined to the run dir"; it must
// now be a hard error the helper turns into a fail-closed 126.
func TestCheckRequiredPathsRejectsAMissingRunDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	p := llPolicy{abi: 1, rules: landlockAllowList(missing)}
	err := p.checkRequiredPaths()
	if err == nil {
		t.Fatal("a run dir that cannot be opened was accepted; the sandbox would have no writable tree")
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "run directory") {
		t.Fatalf("error does not name the run directory: %v", err)
	}
	// apply() must refuse for the same reason, before installing anything.
	if err := p.apply(); err == nil {
		t.Fatal("apply() installed a policy with no writable tree")
	}
	// An OPTIONAL path that is missing stays a silent tightening.
	ok := llPolicy{abi: 1, rules: []llRule{{path: missing, access: llFSRead}}}
	if err := ok.checkRequiredPaths(); err != nil {
		t.Fatalf("a missing optional path must be skipped, not fatal: %v", err)
	}
}

// TestPolicyEncodeRoundTripsRequired: the required flag is what makes the run
// dir fatal, and it crosses a process boundary as text. A path containing ':'
// must survive too, since the encoding now has two leading fields.
func TestPolicyEncodeRoundTripsRequired(t *testing.T) {
	in := llPolicy{abi: 5, blockNet: true, rules: []llRule{
		{path: "/tmp/run:dir/x", access: llHandledFS(5), required: true},
		{path: "/usr", access: llFSRead | llFSExecute},
	}}
	spec, err := in.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodePolicy(spec)
	if err != nil {
		t.Fatalf("decode(%q): %v", spec, err)
	}
	if out.abi != in.abi || out.blockNet != in.blockNet || len(out.rules) != len(in.rules) {
		t.Fatalf("policy head lost in transit: %+v vs %+v", out, in)
	}
	for i, r := range out.rules {
		if r != in.rules[i] {
			t.Fatalf("rule %d round-tripped as %+v, want %+v", i, r, in.rules[i])
		}
	}
}

// TestRlimitClaimMatchesThisShell is FIX 1 end-to-end on the real host: the
// claim in the plan must be the claim for what /bin/sh here was actually
// observed to do, never the unconditional "applied" string.
func TestRlimitClaimMatchesThisShell(t *testing.T) {
	const timeout = 10 * time.Second
	plan, failClosed, err := buildIsolation(t.TempDir(), timeout, true)
	if err != nil || failClosed != nil {
		t.Fatalf("buildIsolation: err=%v failClosed=%+v", err, failClosed)
	}
	cpuOK, asOK := rlimitSupport()
	want := rlimitClaim(timeout, cpuOK, asOK)
	if !strings.Contains(plan.Isolation, want) {
		t.Fatalf("isolation %q does not carry the probed rlimit claim %q", plan.Isolation, want)
	}
	if !cpuOK || !asOK {
		if !strings.Contains(plan.Isolation, "NOT applied") {
			t.Fatalf("this shell did not apply every rlimit (cpu=%v as=%v) but the claim hides it: %q",
				cpuOK, asOK, plan.Isolation)
		}
	}
	// Most Linux hosts run dash/bash, both of which honour these; a host where
	// neither sticks is worth surfacing in the log rather than failing on.
	if !cpuOK || !asOK {
		t.Logf("this host's /bin/sh honoured cpu=%v as=%v", cpuOK, asOK)
	}
}

// TestExecReportsAFailClosedIsolationWhenTheHelperCannotInstall drives the real
// helper: an intact argv marker with a policy that cannot possibly install must
// yield exit 126 AND an isolation string that claims nothing.
func TestExecReportsAFailClosedIsolationWhenTheHelperCannotInstall(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate own executable: %v", err)
	}
	// A required run dir that does not exist: apply() refuses, the helper exits
	// 126 with the marker, and the command never runs.
	missing := filepath.Join(t.TempDir(), "gone")
	policy := llPolicy{abi: 1, rules: landlockAllowList(missing)}
	spec, err := policy.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	plan := &isolationPlan{
		Prefix:              []string{self, helperArg},
		ExtraEnv:            []string{helperEnv + "=" + spec},
		Isolation:           "Landlock ABI v1: writes confined to the run dir",
		FailClosedExit:      helperExitPolicy,
		FailClosedMarker:    helperFailMarker,
		FailClosedIsolation: landlockFailClosedIsolation,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := runPlan(ctx, plan, t.TempDir(), "echo LEAKED")
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if res.Exit != helperExitPolicy {
		t.Fatalf("helper failure exit = %d, want %d (stderr %q)", res.Exit, helperExitPolicy, res.Stderr)
	}
	if strings.Contains(res.Stdout, "LEAKED") {
		t.Fatalf("the command RAN despite the policy failing to install: %q", res.Stdout)
	}
	if strings.Contains(res.Isolation, "Landlock ABI") {
		t.Fatalf("isolation still describes a policy that was never installed: %q", res.Isolation)
	}
	if res.Isolation != landlockFailClosedIsolation {
		t.Fatalf("isolation = %q, want the fail-closed string %q", res.Isolation, landlockFailClosedIsolation)
	}
}

// --- Landlock availability: fail closed, or actually confine -------------------
//
// The two tests below are deliberately written so that EVERY kernel asserts
// something. A Landlock-less kernel (Docker Desktop's linuxkit, most container
// runtimes) must refuse to run; a kernel with Landlock (GitHub's ubuntu-latest
// runners) must genuinely deny /etc/shadow while keeping the run dir writable.
// Skipping either branch would be exactly the vacuous pass that let `sandbox
// run "cat /etc/shadow"` print the file for a whole release.

// TestBuildIsolationFailsClosedWithoutLandlock is DEFECT 1 at the unit level: no
// Landlock and no opt-in means no plan, exit 127, and a message that names both
// the docker alternative and the opt-in flag. With the opt-in, a plan appears
// and its claim must say the filesystem is NOT confined.
func TestBuildIsolationFailsClosedWithoutLandlock(t *testing.T) {
	abi, errno := landlockABI()
	dir := t.TempDir()

	plan, failClosed, err := buildIsolation(dir, 10*time.Second, false)
	if err != nil {
		t.Fatalf("buildIsolation: %v", err)
	}
	if abi >= 1 {
		// Landlock IS available: the default must produce a real, confining plan.
		if failClosed != nil || plan == nil {
			t.Fatalf("Landlock ABI v%d is available but the default run failed closed: %+v", abi, failClosed)
		}
		if !strings.Contains(plan.Isolation, "Landlock ABI") {
			t.Fatalf("plan does not claim the Landlock policy it installs: %q", plan.Isolation)
		}
		if strings.Contains(plan.Isolation, "filesystem NOT confined") {
			t.Fatalf("plan claims no filesystem confinement on a Landlock ABI v%d kernel: %q", abi, plan.Isolation)
		}
		return
	}

	// No Landlock: fail closed by default.
	if failClosed == nil || plan != nil {
		t.Fatalf("no Landlock (errno %v) yet buildIsolation produced a plan: the command would run with "+
			"NO filesystem confinement", errno)
	}
	if failClosed.Exit != 127 {
		t.Fatalf("fail-closed exit = %d, want 127", failClosed.Exit)
	}
	for _, want := range []string{"Landlock", "--backend docker", "--allow-unconfined", "was NOT run"} {
		if !strings.Contains(failClosed.Stderr, want) {
			t.Fatalf("fail-closed message must mention %q, got %q", want, failClosed.Stderr)
		}
	}
	if failClosed.Isolation != landlockUnavailableIsolation {
		t.Fatalf("isolation = %q, want %q", failClosed.Isolation, landlockUnavailableIsolation)
	}

	// The opt-in runs, and says exactly what the user gave up.
	plan, failClosed, err = buildIsolation(dir, 10*time.Second, true)
	if err != nil || failClosed != nil {
		t.Fatalf("--allow-unconfined still refused: err=%v failClosed=%+v", err, failClosed)
	}
	if !strings.Contains(plan.Isolation, "filesystem NOT confined") {
		t.Fatalf("the opt-in plan must state that the filesystem is NOT confined: %q", plan.Isolation)
	}
	if strings.Contains(plan.Isolation, "Landlock ABI") {
		t.Fatalf("the opt-in plan claims a Landlock policy it never installed: %q", plan.Isolation)
	}
	// The network layer genuinely works without Landlock and must survive.
	if !strings.Contains(plan.Isolation, "network namespace") {
		t.Fatalf("the opt-in plan dropped the network namespace layer: %q", plan.Isolation)
	}
	// DEFECT 2: the reason must reflect the real errno, not a canned kernel range.
	if !strings.Contains(plan.Isolation, landlockUnavailableReason(errno)) {
		t.Fatalf("the opt-in plan does not carry the probed reason %q: %q",
			landlockUnavailableReason(errno), plan.Isolation)
	}
	if errno == syscall.ENOSYS && strings.Contains(plan.Isolation, "5.13") {
		t.Fatalf("ENOSYS reported as a kernel-version problem; it is a not-compiled-in problem: %q", plan.Isolation)
	}
}

// TestExecConfinesFilesystemOrFailsClosed is the end-to-end half, and the one
// that would have caught the live bug: `sandbox run "cat /etc/shadow"` in a
// perfectly ordinary scratch dir.
//
//   - Landlock available  -> the run dir is writable AND /etc/shadow is NOT
//     readable (this is the allow-list path, exercised for real).
//   - Landlock absent     -> the command does not run at all (exit 127), and the
//     opt-in run does execute while reporting no filesystem confinement.
func TestExecConfinesFilesystemOrFailsClosed(t *testing.T) {
	abi, _ := landlockABI()
	dir := t.TempDir()

	res, err := Exec(context.Background(), Options{
		Command: "cat /etc/shadow", Dir: dir, Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if abi >= 1 {
		if res.Exit == 0 {
			t.Fatalf("/etc/shadow was READ inside the sandbox on a Landlock ABI v%d kernel: stdout=%q isolation=%q",
				abi, res.Stdout, res.Isolation)
		}
		if strings.Contains(res.Stdout, ":") && strings.Contains(res.Stdout, "root") {
			t.Fatalf("/etc/shadow contents leaked: %q", res.Stdout)
		}
		if !strings.Contains(res.Isolation, "Landlock ABI") {
			t.Fatalf("isolation does not report the Landlock policy: %q", res.Isolation)
		}
		// The confinement must not have cost us our own working directory.
		w, err := Exec(context.Background(), Options{
			Command: "echo confined > out.txt && cat out.txt", Dir: dir, Timeout: 20 * time.Second,
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if w.Exit != 0 || strings.TrimSpace(w.Stdout) != "confined" {
			t.Fatalf("the run dir is not writable under Landlock ABI v%d: exit=%d stdout=%q stderr=%q",
				abi, w.Exit, w.Stdout, w.Stderr)
		}
		return
	}

	// No Landlock: the command must not have run.
	if res.Exit != 127 || res.Isolation != landlockUnavailableIsolation {
		t.Fatalf("no Landlock, yet the run was not refused: exit=%d isolation=%q stdout=%q",
			res.Exit, res.Isolation, res.Stdout)
	}
	if res.Stdout != "" {
		t.Fatalf("a refused run produced stdout: %q", res.Stdout)
	}

	// The opt-in must actually run the command, and say the filesystem is open.
	opt, err := Exec(context.Background(), Options{
		Command: "echo unconfined > out.txt && cat out.txt", Dir: dir,
		Timeout: 20 * time.Second, AllowUnconfined: true,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if opt.Exit != 0 || strings.TrimSpace(opt.Stdout) != "unconfined" {
		t.Fatalf("--allow-unconfined did not run the command: exit=%d stdout=%q stderr=%q",
			opt.Exit, opt.Stdout, opt.Stderr)
	}
	if !strings.Contains(opt.Isolation, "filesystem NOT confined") {
		t.Fatalf("the opt-in run hides that the filesystem was open: %q", opt.Isolation)
	}
}
