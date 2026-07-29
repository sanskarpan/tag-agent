package sandbox

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run on EVERY host, macOS included. The Linux confinement layers
// cannot be exercised from a Mac, but the honesty of what they CLAIM is pure
// string logic, and that is exactly where the "fake success" bugs lived: a
// prologue that might be ignored reported as applied, a /proc grant reported as
// nothing, a policy that failed to install reported as installed.

// TestRlimitClaimNeverAssertsAnUnappliedLimit is the core FIX-1 contract: only
// the both-honoured case may state the limits as facts.
func TestRlimitClaimNeverAssertsAnUnappliedLimit(t *testing.T) {
	const timeout = 30 * time.Second
	cases := []struct {
		cpuOK, asOK      bool
		mustSay, mustNot []string
	}{
		{true, true,
			[]string{"rlimits via ulimit (CPU 35s soft / 40s hard, AS 512MB)"},
			[]string{"NOT applied"}},
		{true, false,
			[]string{"CPU 35s soft / 40s hard", "AS 512MB REQUESTED but NOT applied", "NO memory cap"},
			[]string{"AS 512MB)"}},
		{false, true,
			[]string{"AS 512MB", "CPU 35s/40s REQUESTED but NOT applied", "NO CPU cap"},
			[]string{"CPU 35s soft / 40s hard)"}},
		{false, false,
			[]string{"rlimits REQUESTED but NOT applied", "NO CPU or memory cap"},
			[]string{"rlimits via ulimit"}},
	}
	for _, c := range cases {
		got := rlimitClaim(timeout, c.cpuOK, c.asOK)
		for _, want := range c.mustSay {
			if !strings.Contains(got, want) {
				t.Fatalf("rlimitClaim(cpuOK=%v, asOK=%v) = %q; must contain %q", c.cpuOK, c.asOK, got, want)
			}
		}
		for _, bad := range c.mustNot {
			if strings.Contains(got, bad) {
				t.Fatalf("rlimitClaim(cpuOK=%v, asOK=%v) = %q; must NOT contain %q", c.cpuOK, c.asOK, got, bad)
			}
		}
	}
}

// TestRlimitPrologueAgreesWithClaim: the numbers the prologue REQUESTS and the
// numbers the claim REPORTS must come from the same place. They did not before
// (the claim used an unclamped timeout, so a sub-second run asked for a 6s/11s
// CPU limit and reported 5s/10s).
func TestRlimitPrologueAgreesWithClaim(t *testing.T) {
	for _, d := range []time.Duration{0, 500 * time.Millisecond, time.Second, 30 * time.Second, 600 * time.Second} {
		soft, hard := rlimitCPUSecs(d)
		p := rlimitPrologue(d)
		if !strings.Contains(p, fmt.Sprintf("ulimit -H -t %d ", hard)) ||
			!strings.Contains(p, fmt.Sprintf("ulimit -S -t %d ", soft)) {
			t.Fatalf("prologue for %s does not request the claimed limits %d/%d: %q", d, soft, hard, p)
		}
		if !strings.Contains(rlimitClaim(d, true, true), fmt.Sprintf("CPU %ds soft / %ds hard", soft, hard)) {
			t.Fatalf("claim for %s does not report the requested limits %d/%d", d, soft, hard)
		}
		// Hard before soft: a lowered hard limit can never be raised again.
		if strings.Index(p, "-H -t") > strings.Index(p, "-S -t") {
			t.Fatalf("prologue lowers the hard CPU limit after the soft one: %q", p)
		}
		if soft < 6 || hard < 11 {
			t.Fatalf("sub-second timeout %s was not clamped: soft=%d hard=%d", d, soft, hard)
		}
	}
}

// TestParseRlimitReadback pins the probe parser: anything other than exactly the
// value we asked for must read as "not applied".
func TestParseRlimitReadback(t *testing.T) {
	const timeout = 30 * time.Second
	kb := rlimitBytes / 1024
	cases := []struct {
		name        string
		out         string
		cpuOK, asOK bool
	}{
		{"both applied", fmt.Sprintf("t=35\nv=%d\n", kb), true, true},
		{"shell ignored -v", "t=35\nv=unlimited\n", true, false},
		{"shell rejected -v entirely", "t=35\nv=\n", true, false},
		{"shell rejected -t entirely", fmt.Sprintf("t=\nv=%d\n", kb), false, true},
		{"busybox ignores both", "t=unlimited\nv=unlimited\n", false, false},
		{"no output at all", "", false, false},
		{"garbage", "sh: ulimit: not found\n", false, false},
		{"stricter pre-existing limit reads as not applied", "t=10\nv=1024\n", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cpuOK, asOK := parseRlimitReadback(c.out, timeout)
			if cpuOK != c.cpuOK || asOK != c.asOK {
				t.Fatalf("parseRlimitReadback(%q) = (%v, %v), want (%v, %v)", c.out, cpuOK, asOK, c.cpuOK, c.asOK)
			}
		})
	}
}

// TestRlimitReadbackMatchesPrologue: the probe must read back the same limits
// the prologue sets, or the probe would always report "not applied".
func TestRlimitReadbackMatchesPrologue(t *testing.T) {
	if !strings.Contains(rlimitReadback, "ulimit -t") || !strings.Contains(rlimitReadback, "ulimit -v") {
		t.Fatalf("read-back does not read the limits the prologue sets: %q", rlimitReadback)
	}
	// Simulate a shell that honoured everything: prologue values echoed back.
	soft, _ := rlimitCPUSecs(rlimitProbeTimeoutForTest)
	out := fmt.Sprintf("t=%d\nv=%d\n", soft, rlimitBytes/1024)
	if cpuOK, asOK := parseRlimitReadback(out, rlimitProbeTimeoutForTest); !cpuOK || !asOK {
		t.Fatalf("a fully-honouring shell was read as not applying the limits (%v, %v)", cpuOK, asOK)
	}
}

// rlimitProbeTimeoutForTest mirrors the Linux probe's fixed timeout without
// pulling the Linux-only constant into this platform-neutral file.
const rlimitProbeTimeoutForTest = 30 * time.Second

// TestProcReadDisclosureIsSpecific: FIX 2's disclosure must actually name the
// exposure, not hint at it. A vague string is how this got missed the first time.
func TestProcReadDisclosureIsSpecific(t *testing.T) {
	for _, want := range []string{"/proc IS readable", "environ", "same-uid", "NOT hidden"} {
		if !strings.Contains(procReadDisclosure, want) {
			t.Fatalf("procReadDisclosure = %q; must mention %q", procReadDisclosure, want)
		}
	}
}

// TestLandlockFailClosedIsolationClaimsNothing is FIX 3's wording contract: the
// 126 string must follow the "none (failed closed: ...)" convention and must not
// describe a ruleset that was never installed.
func TestLandlockFailClosedIsolationClaimsNothing(t *testing.T) {
	if !strings.HasPrefix(landlockFailClosedIsolation, "none (failed closed:") {
		t.Fatalf("fail-closed isolation %q does not follow the `none (failed closed: ...)` convention",
			landlockFailClosedIsolation)
	}
	if strings.Contains(landlockFailClosedIsolation, "Landlock ABI") {
		t.Fatalf("fail-closed isolation %q still claims a Landlock ABI level", landlockFailClosedIsolation)
	}
	if !strings.Contains(landlockFailClosedIsolation, "NOT run") {
		t.Fatalf("fail-closed isolation %q should say the command did not run", landlockFailClosedIsolation)
	}
}

// TestResolveIsolationOnlyDowngradesOnAMarkedFailure is the logic half of FIX 3.
// The status alone is not enough: a command may legitimately exit 126 (shell
// "permission denied"), and mislabelling that run as fail-closed would be a
// different lie.
func TestResolveIsolationOnlyDowngradesOnAMarkedFailure(t *testing.T) {
	plan := &isolationPlan{
		Isolation:           "Landlock ABI v5: writes confined to the run dir",
		FailClosedExit:      126,
		FailClosedMarker:    helperFailMarker,
		FailClosedIsolation: landlockFailClosedIsolation,
	}
	cases := []struct {
		name   string
		exit   int
		stderr string
		want   string
	}{
		{"helper failed closed", 126, helperFailMarker + " landlock_restrict_self: EPERM\n", landlockFailClosedIsolation},
		{"marker anywhere in stderr", 126, "warming up\n" + helperFailMarker + " boom\n", landlockFailClosedIsolation},
		{"command's own 126", 126, "sh: ./script: Permission denied\n", plan.Isolation},
		{"marker but a different status", 1, helperFailMarker + " boom\n", plan.Isolation},
		{"ordinary success", 0, "", plan.Isolation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveIsolation(plan, c.exit, c.stderr); got != c.want {
				t.Fatalf("resolveIsolation(exit=%d) = %q, want %q", c.exit, got, c.want)
			}
		})
	}

	// A plan with no fail-closed contract (macOS, and Linux without Landlock)
	// must be completely unaffected, including at status 126.
	plain := &isolationPlan{Isolation: "sandbox-exec (SBPL): network denied"}
	for _, exit := range []int{0, 1, 126, 127} {
		if got := resolveIsolation(plain, exit, helperFailMarker+" boom"); got != plain.Isolation {
			t.Fatalf("a plan without a fail-closed contract was rewritten at exit %d: %q", exit, got)
		}
	}
}

// TestHelperFailMarkerIsUnambiguous: the marker must be specific enough that a
// command echoing something ordinary cannot forge it.
func TestHelperFailMarkerIsUnambiguous(t *testing.T) {
	if !strings.HasPrefix(helperFailMarker, "sandbox: ") || len(helperFailMarker) < 20 {
		t.Fatalf("helperFailMarker %q is too generic to identify our own helper", helperFailMarker)
	}
}

// TestLandlockUnavailableReasonMatchesTheErrno is DEFECT 2: the reason a user
// reads has to match the errno the kernel returned, because the two errnos have
// completely different remedies. The old single string claimed "kernel < 5.13 or
// LSM disabled" on a 6.10 kernel that simply has no Landlock compiled in, which
// sends the reader after the wrong fix.
func TestLandlockUnavailableReasonMatchesTheErrno(t *testing.T) {
	enosys := landlockUnavailableReason(syscall.ENOSYS)
	for _, want := range []string{"ENOSYS", "not compiled in", "ANY kernel version"} {
		if !strings.Contains(enosys, want) {
			t.Fatalf("ENOSYS reason must contain %q: %q", want, enosys)
		}
	}
	// The specific inaccuracy that shipped: a version bound implying an upgrade.
	for _, forbidden := range []string{"5.13", "LSM disabled"} {
		if strings.Contains(enosys, forbidden) {
			t.Fatalf("ENOSYS reason still blames %q, but the syscall is simply absent: %q", forbidden, enosys)
		}
	}

	notsup := landlockUnavailableReason(syscall.EOPNOTSUPP)
	for _, want := range []string{"EOPNOTSUPP", "disabled", "lsm="} {
		if !strings.Contains(notsup, want) {
			t.Fatalf("EOPNOTSUPP reason must contain %q: %q", want, notsup)
		}
	}
	if strings.Contains(notsup, "not compiled in") {
		t.Fatalf("EOPNOTSUPP means compiled-but-off; the reason says the opposite: %q", notsup)
	}
	if enosys == notsup {
		t.Fatal("the two errnos have different remedies but share one message")
	}

	// Anything else is reported verbatim rather than guessed at.
	other := landlockUnavailableReason(syscall.EPERM)
	if !strings.Contains(other, "EPERM") && !strings.Contains(other, other) {
		t.Fatalf("an unexpected errno must be reported verbatim: %q", other)
	}
	if !strings.Contains(other, fmt.Sprintf("%d", int(syscall.EPERM))) {
		t.Fatalf("an unexpected errno must carry its numeric value: %q", other)
	}
}

// TestLandlockUnavailableMsgMirrorsDarwin: the Linux fail-closed message must
// read like the macOS one -- what is missing, that the command did NOT run, and
// `--backend docker` -- plus the opt-in flag by name. A user who hits this on
// one platform should recognise it on the other.
func TestLandlockUnavailableMsgMirrorsDarwin(t *testing.T) {
	msg := landlockUnavailableMsg(landlockUnavailableReason(syscall.ENOSYS))
	for _, want := range []string{
		"cannot confine the filesystem", "was NOT run", "--backend docker", "--allow-unconfined",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("fail-closed message must contain %q: %q", want, msg)
		}
	}
	if !strings.Contains(landlockUnavailableIsolation, "none (failed closed") {
		t.Fatalf("the isolation string must claim nothing: %q", landlockUnavailableIsolation)
	}
	if strings.Contains(landlockUnavailableIsolation, "Landlock ABI") {
		t.Fatalf("the isolation string names a policy that was never installed: %q", landlockUnavailableIsolation)
	}
}

// TestLandlockUnconfinedClaimLeadsWithTheBadNews: the opt-in claim must say the
// filesystem is not confined BEFORE it explains why, so a reader skimming the
// isolation line cannot miss it.
func TestLandlockUnconfinedClaimLeadsWithTheBadNews(t *testing.T) {
	claim := landlockUnconfinedClaim(landlockUnavailableReason(syscall.ENOSYS))
	if !strings.HasPrefix(claim, "filesystem NOT confined") {
		t.Fatalf("the opt-in claim must lead with the loss of confinement: %q", claim)
	}
	if !strings.Contains(claim, "--allow-unconfined") {
		t.Fatalf("the opt-in claim must name the flag that caused it: %q", claim)
	}
	// isolation_test.go's fsConfined() keys off this exact substring; if the
	// wording drifts, every filesystem assertion silently starts asserting.
	if fsConfined(claim) {
		t.Fatalf("fsConfined() no longer recognises the unconfined claim: %q", claim)
	}
}
