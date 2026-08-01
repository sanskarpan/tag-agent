package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Linux confinement is layered, and every layer is optional at runtime because
// kernels differ. Whatever is NOT achieved is stated in Result.Isolation rather
// than glossed over:
//
//  1. rlimits (always attempted): RLIMIT_CPU = timeout+5 soft / timeout+10 hard
//     and RLIMIT_AS = 512MB, matching src/tag/sandbox.py's preexec_fn. Go's
//     os/exec has no preexec_fn and syscall.SysProcAttr exposes no Rlimits
//     field, so we set them with a `ulimit` prologue in the same `sh -c` we
//     already spawn -- POSIX sh applies them to itself and they are inherited
//     by everything it runs. Downside vs. Python: a shell that lacks `ulimit
//     -t`/`-v` silently skips them, so we PROBE the shell (rlimitSupport) and
//     report only the limits it was observed to honour.
//  2. Landlock (kernel >= 5.13): allow-list filesystem confinement. Without it
//     there is NO filesystem confinement and we say so. /proc stays readable
//     even with it, which is disclosed rather than hidden (procReadDisclosure).
//  3. Network: a) Landlock ABI >= 4 (kernel >= 6.7) denies TCP connect/bind;
//     b) an unprivileged user+network namespace denies everything including
//     UDP/ICMP/unix-to-network. (b) requires unprivileged user namespaces to be
//     enabled (kernel.unprivileged_userns_clone=1 and no seccomp/AppArmor
//     block); when the kernel refuses, we fall back and downgrade the reported
//     guarantee. We do NOT claim network isolation we did not get.
//
// The prologue, the claim strings and the fail-closed wording all live in
// isolation_claims.go so they can be pinned by tests from a non-Linux host.

// rlimitProbeTimeout is the (arbitrary, fixed) timeout the capability probe
// pretends to run with. Only whether the shell honours ulimit is being measured,
// and that does not vary with the value.
const rlimitProbeTimeout = 30 * time.Second

var (
	rlimitProbeOnce sync.Once
	rlimitProbeCPU  bool
	rlimitProbeAS   bool
)

// rlimitSupport reports whether this host's /bin/sh actually applies the CPU and
// address-space limits rlimitPrologue asks for.
//
// It answers by running the real prologue in a throwaway `sh` and reading the
// limits back out of that shell. Doing the read-back in a SEPARATE probe process
// (rather than inside the sandboxed run, via an extra fd or a stderr sentinel)
// is deliberate: the probe cannot corrupt the command's stdout/stderr, cannot
// deadlock the run on a pipe, and cannot change the command's own exit status.
// The shell binary does not change under us, so one probe per process is enough.
//
// Any failure -- no `sh`, a timeout, unparseable output -- yields false, i.e. the
// weaker claim. Guessing "applied" on a failed probe would be exactly the
// overclaim this function exists to remove.
func rlimitSupport() (cpuOK, asOK bool) {
	rlimitProbeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "sh", "-c",
			rlimitPrologue(rlimitProbeTimeout)+rlimitReadback).Output()
		if err != nil {
			return
		}
		rlimitProbeCPU, rlimitProbeAS = parseRlimitReadback(string(out), rlimitProbeTimeout)
	})
	return rlimitProbeCPU, rlimitProbeAS
}

// landlockAllowList is the filesystem allow-list. Anything not listed is denied
// -- notably /etc/passwd, /etc/shadow, /root and the invoking user's $HOME.
//
// The run dir is the ONLY writable tree, which is exactly why validateRunDir
// must have vetted it first: a run dir of `/` would grant write over the whole
// filesystem and `--dir $HOME` over the user's entire home, silently undoing
// every denial this list encodes.
func landlockAllowList(runDir string) []llRule {
	rules := []llRule{
		// The run directory is the only writable tree, and it is REQUIRED: a
		// sandbox that cannot grant its own working directory is broken, not
		// merely tightened, so failing to open it is fatal (see llPolicy.apply).
		{path: runDir, access: llHandledFS(6), required: true},
	}
	// Read+execute for the system so the dynamic loader and the shell work.
	for _, p := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/libx32", "/opt", "/nix/store"} {
		rules = append(rules, llRule{path: p, access: llFSRead | llFSExecute})
	}
	// A hand-picked slice of /etc: enough to link and localise a process, but
	// NOT the account databases.
	for _, p := range []string{
		"/etc/ld.so.cache", "/etc/ld.so.preload", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
		"/etc/localtime", "/etc/alternatives", "/etc/ssl/certs", "/etc/ca-certificates",
	} {
		rules = append(rules, llRule{path: p, access: llFSRead})
	}
	// Character devices the runtime needs.
	for _, p := range []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"} {
		rules = append(rules, llRule{path: p, access: llFSReadWriteFile})
	}
	// /proc is needed by nearly everything (including the Go runtime and libc)
	// and CANNOT be narrowed safely here:
	//
	//   - Landlock rules are resolved to a path at policy-install time. A rule on
	//     "/proc/self" is opened by the helper, so it pins the HELPER's pid dir.
	//     That happens to survive the helper's execve (exec keeps the pid), but
	//     every process the command then spawns has a different pid and would be
	//     denied its own /proc/self -- breaking libc, the Go runtime and most
	//     language runtimes inside the sandbox. Landlock has no pid wildcard, and
	//     hiding other pids would need a PID+mount namespace with a private
	//     hidepid proc, which is a different (and much larger) design.
	//
	// So the read grant stays whole-/proc, and the consequence -- another
	// same-uid process's environ/cmdline is readable from inside the sandbox --
	// is DISCLOSED in Result.Isolation via procReadDisclosure. Any change here
	// must keep that disclosure in sync with what is actually granted.
	rules = append(rules, llRule{path: "/proc", access: llFSRead})
	return rules
}

// grantsProcRead reports whether an allow-list still contains the whole-/proc
// read grant, i.e. whether procReadDisclosure must appear in the claim. It keeps
// the disclosure tied to the rules instead of to a comment someone can forget.
func grantsProcRead(rules []llRule) bool {
	for _, r := range rules {
		if r.path == "/proc" && r.access&llFSRead != 0 {
			return true
		}
	}
	return false
}

// buildIsolation assembles the strongest plan this kernel supports, with a
// weaker Alt used only if the kernel refuses to start the strong one.
//
// allowUnconfined is the caller's explicit acceptance of a run with NO
// filesystem confinement. Without it, a kernel that cannot give us Landlock
// FAILS CLOSED (exit 127, command never runs), exactly as macOS does when
// sandbox-exec is missing. This is a deliberate behaviour change: the previous
// version degraded instead, so `sandbox run --dir /tmp/w "cat /etc/shadow"`
// printed the shadow file with exit 0 on any Landlock-less kernel -- honestly
// labelled, but a command named `sandbox` must not run with zero filesystem
// isolation unless the user asked for that in so many words.
func buildIsolation(runDir string, timeout time.Duration, allowUnconfined bool) (*isolationPlan, *Result, error) {
	// A run dir at or above a sensitive boundary is refused before any layer is
	// assembled, exactly as on macOS: the Landlock allow-list would otherwise
	// make that entire tree the sandbox's writable area. The check is
	// unconditional (not gated on Landlock being available) so the same command
	// behaves the same way on every kernel.
	home, _ := os.UserHomeDir()
	if home != "" {
		// Match confineDir's resolution so a symlinked home compares equal.
		if real, err := filepath.EvalSymlinks(home); err == nil {
			home = real
		}
	}
	if err := validateRunDir(runDir, home); err != nil {
		return nil, &Result{
			Exit:      127,
			Stderr:    err.Error(),
			Isolation: runDirTooBroadIsolation,
		}, nil
	}

	prologue := rlimitPrologue(timeout)
	abi, abiErrno := landlockABI()

	var prefix []string
	var extraEnv []string
	var fsClaim string
	netClaimLandlock := ""
	// Set only when a helper is in the argv: only then can the run fail closed
	// AFTER Start() succeeded, and only then is the fail-closed string right.
	failExit := 0
	failMarker := ""
	failIsolation := ""

	if abi < 1 {
		// No Landlock: there is no filesystem boundary to be had on this kernel.
		// The rlimit and network-namespace layers still work, but they protect
		// nothing about the filesystem, so this is a fail-closed refusal unless
		// the user opted in.
		reason := landlockUnavailableReason(abiErrno)
		if !allowUnconfined {
			return nil, &Result{
				Exit:      127,
				Stderr:    landlockUnavailableMsg(reason),
				Isolation: landlockUnavailableIsolation,
			}, nil
		}
		fsClaim = landlockUnconfinedClaim(reason)
	}

	if abi >= 1 {
		self, err := os.Executable()
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox: cannot locate own executable for the Landlock helper: %w", err)
		}
		rules := landlockAllowList(runDir)
		policy := llPolicy{abi: abi, rules: rules, blockNet: abi >= 4}
		spec, err := policy.encode()
		if err != nil {
			return nil, nil, err
		}
		prefix = []string{self, helperArg}
		extraEnv = []string{helperEnv + "=" + spec}
		fsClaim = fmt.Sprintf("Landlock ABI v%d: writes confined to the run dir; reads limited to "+
			"system dirs (/etc account databases, $HOME and /root denied)", abi)
		if grantsProcRead(rules) {
			fsClaim += "; " + procReadDisclosure
		}
		if abi >= 4 {
			netClaimLandlock = "Landlock denies TCP connect/bind (UDP and raw sockets NOT blocked)"
		}
		failExit, failMarker, failIsolation = helperExitPolicy, helperFailMarker, landlockFailClosedIsolation
	}

	// The rlimit claim is whatever this shell was OBSERVED to honour, not what
	// the prologue asked for.
	cpuOK, asOK := rlimitSupport()
	rl := rlimitClaim(timeout, cpuOK, asOK)

	// Weak plan: no network namespace.
	netWeak := "network NOT blocked"
	if netClaimLandlock != "" {
		netWeak = netClaimLandlock
	}
	weak := &isolationPlan{
		Prefix:              prefix,
		Prologue:            prologue,
		ExtraEnv:            extraEnv,
		Isolation:           strings.Join([]string{rl, fsClaim, netWeak}, "; "),
		FailClosedExit:      failExit,
		FailClosedMarker:    failMarker,
		FailClosedIsolation: failIsolation,
	}

	// Strong plan: unprivileged user+network namespace => no network at all.
	strong := &isolationPlan{
		Prefix:   prefix,
		Prologue: prologue,
		ExtraEnv: extraEnv,
		SysProcAttr: &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		},
		Isolation: strings.Join([]string{rl, fsClaim,
			"network namespace: all egress denied (loopback only)"}, "; "),
		// Only THIS plan denies everything. `weak` deliberately leaves
		// DeniesAllEgress false: with Landlock ABI>=4 it blocks TCP and nothing
		// else, and without it nothing at all. An egress policy of --deny-all
		// therefore refuses to fall back to it (see Exec).
		DeniesAllEgress:     true,
		Alt:                 weak,
		FailClosedExit:      failExit,
		FailClosedMarker:    failMarker,
		FailClosedIsolation: failIsolation,
	}
	return strong, nil, nil
}
