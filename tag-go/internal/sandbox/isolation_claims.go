package sandbox

import (
	"fmt"
	"strings"
	"time"
)

// The strings in Result.Isolation are a promise, not marketing: a layer that did
// not take effect must be reported as not taken effect. The builders live here,
// with no build tag and no syscalls, so the exact wording of every claim can be
// pinned by tests on ANY host -- including the macOS dev machines where the
// Linux layers themselves cannot run. Only the *inputs* (did this shell honour
// ulimit? did Landlock install?) are platform-specific.

// rlimitBytes is the RLIMIT_AS cap (512 MB), matching the Python backend.
const rlimitBytes = 512 * 1024 * 1024

// rlimitCPUSecs converts a run timeout into the (soft, hard) RLIMIT_CPU pair, so
// the prologue that REQUESTS the limits and the claim that REPORTS them can
// never disagree about the numbers. A sub-second timeout is clamped to 1s.
func rlimitCPUSecs(timeout time.Duration) (soft, hard int) {
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs + 5, secs + 10
}

// rlimitPrologue emits the `ulimit` calls that stand in for Python's
// preexec_fn setrlimit. Hard limits are lowered before soft ones because a
// lowered hard limit can never be raised again.
//
// Every call is `2>/dev/null`, so a shell without `ulimit -t`/`-v` (some busybox
// and dash builds) skips it in silence. That silence is why rlimitClaim takes
// the result of an actual read-back probe instead of assuming success.
func rlimitPrologue(timeout time.Duration) string {
	soft, hard := rlimitCPUSecs(timeout)
	return fmt.Sprintf("ulimit -H -t %d 2>/dev/null; ulimit -S -t %d 2>/dev/null; "+
		"ulimit -v %d 2>/dev/null; ", hard, soft, rlimitBytes/1024)
}

// rlimitReadback is the suffix appended to rlimitPrologue by the capability
// probe: it prints back what the shell actually ended up with. The `t=`/`v=`
// prefixes mean a value that is missing entirely (the shell rejected the option)
// is still a parseable line, so one unsupported limit cannot be mistaken for the
// other also being unsupported.
const rlimitReadback = "echo t=$(ulimit -t 2>/dev/null); echo v=$(ulimit -v 2>/dev/null)"

// parseRlimitReadback interprets the probe output against the values that
// rlimitPrologue(timeout) asked for. A limit counts as applied ONLY when the
// shell reports back exactly what we asked for: "unlimited", an empty value (the
// option was rejected) and a different number all count as not applied. A
// different number is usually a pre-existing STRICTER limit, i.e. reporting it
// as "not applied" understates the confinement -- which is the safe direction to
// be wrong in, and never the direction that fakes a guarantee.
func parseRlimitReadback(out string, timeout time.Duration) (cpuOK, asOK bool) {
	soft, _ := rlimitCPUSecs(timeout)
	wantCPU := fmt.Sprintf("t=%d", soft)
	wantAS := fmt.Sprintf("v=%d", rlimitBytes/1024)
	for _, line := range strings.Split(out, "\n") {
		switch strings.TrimSpace(line) {
		case wantCPU:
			cpuOK = true
		case wantAS:
			asOK = true
		}
	}
	return cpuOK, asOK
}

// rlimitClaim renders the rlimit half of Result.Isolation from what the shell
// was actually observed to honour. Only the cpuOK && asOK case may state the
// limits as facts; every other case says REQUESTED but NOT applied, because a
// user reading "AS 512MB" must be able to rely on there being a memory cap.
func rlimitClaim(timeout time.Duration, cpuOK, asOK bool) string {
	soft, hard := rlimitCPUSecs(timeout)
	mb := rlimitBytes / 1024 / 1024
	switch {
	case cpuOK && asOK:
		return fmt.Sprintf("rlimits via ulimit (CPU %ds soft / %ds hard, AS %dMB)", soft, hard, mb)
	case cpuOK:
		return fmt.Sprintf("rlimits via ulimit (CPU %ds soft / %ds hard); AS %dMB REQUESTED but NOT applied "+
			"(this shell's `ulimit -v` had no effect, so there is NO memory cap)", soft, hard, mb)
	case asOK:
		return fmt.Sprintf("rlimits via ulimit (AS %dMB); CPU %ds/%ds REQUESTED but NOT applied "+
			"(this shell's `ulimit -t` had no effect, so there is NO CPU cap)", mb, soft, hard)
	default:
		return fmt.Sprintf("rlimits REQUESTED but NOT applied (CPU %ds/%ds, AS %dMB): this shell ignores "+
			"`ulimit -t`/`-v`, so there is NO CPU or memory cap", soft, hard, mb)
	}
}

// procReadDisclosure is the mandatory disclosure for the /proc read grant in
// landlockAllowList.
//
// Landlock rules are path-based and cannot be scoped per-pid, and /proc cannot
// be narrowed away without breaking the runtimes that need it (see
// landlockAllowList). The exposure is therefore real and permanent for this
// design, so it is stated in Result.Isolation rather than left for a user to
// discover: anything readable in /proc for the same uid -- most importantly
// another process's environment -- is readable from inside the sandbox.
const procReadDisclosure = "/proc IS readable: the environment (/proc/<pid>/environ) and command line of " +
	"your other same-uid processes are NOT hidden, so secrets held by another process (e.g. API keys in " +
	"another `tag` run) can be read from inside the sandbox"

// landlockFailClosedIsolation is Result.Isolation for a run whose Landlock
// helper could not install the policy it was given (helperExitPolicy).
//
// The process started fine, so runPlan never falls back to the weaker plan and
// the plan's own Isolation string still describes a policy that does not exist.
// The command did NOT run, so nothing leaked -- but the report must say that,
// not describe an imaginary ruleset. It deliberately contains no "Landlock ABI
// vN" claim, matching the "none (failed closed: ...)" convention already used
// for a missing sandbox-exec and a too-broad run dir.
const landlockFailClosedIsolation = "none (failed closed: the Landlock policy could not be installed, " +
	"so the command was NOT run)"

// helperFailMarker prefixes every message the Landlock helper prints when it
// refuses to run because the policy is not in place. It is the signal runPlan
// uses to tell "our helper failed closed" apart from "the command itself exited
// 126", which is an ordinary shell status (permission denied) that must keep
// reporting the real isolation.
const helperFailMarker = "sandbox: landlock policy not installed:"

// resolveIsolation picks the honest Isolation string for a finished run.
//
// A plan may nominate one exit status + stderr marker pair that means "a layer
// this plan promised was never installed"; when both match, the fail-closed
// string is reported instead of the plan's guarantee. Requiring the marker as
// well as the status is what keeps a command that merely exits 126 on its own
// from being mislabelled.
func resolveIsolation(plan *isolationPlan, exit int, stderr string) string {
	if plan.FailClosedMarker != "" && plan.FailClosedIsolation != "" &&
		exit == plan.FailClosedExit && strings.Contains(stderr, plan.FailClosedMarker) {
		return plan.FailClosedIsolation
	}
	return plan.Isolation
}
