package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Linux confinement is layered, and every layer is optional at runtime because
// kernels differ. Whatever is NOT achieved is stated in Result.Isolation rather
// than glossed over:
//
//  1. rlimits (always): RLIMIT_CPU = timeout+5 soft / timeout+10 hard and
//     RLIMIT_AS = 512MB, matching src/tag/sandbox.py's preexec_fn. Go's
//     os/exec has no preexec_fn and syscall.SysProcAttr exposes no Rlimits
//     field, so we set them with a `ulimit` prologue in the same `sh -c` we
//     already spawn -- POSIX sh applies them to itself and they are inherited
//     by everything it runs. Downside vs. Python: a shell that lacks `ulimit`
//     silently skips them, so the prologue also records nothing; see
//     rlimitPrologue.
//  2. Landlock (kernel >= 5.13): allow-list filesystem confinement. Without it
//     there is NO filesystem confinement and we say so.
//  3. Network: a) Landlock ABI >= 4 (kernel >= 6.7) denies TCP connect/bind;
//     b) an unprivileged user+network namespace denies everything including
//     UDP/ICMP/unix-to-network. (b) requires unprivileged user namespaces to be
//     enabled (kernel.unprivileged_userns_clone=1 and no seccomp/AppArmor
//     block); when the kernel refuses, we fall back and downgrade the reported
//     guarantee. We do NOT claim network isolation we did not get.

// rlimitBytes is the RLIMIT_AS cap (512 MB), matching the Python backend.
const rlimitBytes = 512 * 1024 * 1024

// rlimitPrologue emits the `ulimit` calls that stand in for Python's
// preexec_fn setrlimit. Hard limits are lowered before soft ones because a
// lowered hard limit can never be raised again.
func rlimitPrologue(timeout time.Duration) string {
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("ulimit -H -t %d 2>/dev/null; ulimit -S -t %d 2>/dev/null; "+
		"ulimit -v %d 2>/dev/null; ", secs+10, secs+5, rlimitBytes/1024)
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
		// The run directory is the only writable tree.
		{path: runDir, access: llHandledFS(6)},
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
	// /proc is needed by nearly everything (including the Go runtime and libc).
	rules = append(rules, llRule{path: "/proc", access: llFSRead})
	return rules
}

// buildIsolation assembles the strongest plan this kernel supports, with a
// weaker Alt used only if the kernel refuses to start the strong one.
func buildIsolation(runDir string, timeout time.Duration) (*isolationPlan, *Result, error) {
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
	abi := landlockABI()

	var prefix []string
	var extraEnv []string
	fsClaim := "filesystem NOT confined (Landlock unavailable: kernel < 5.13 or LSM disabled)"
	netClaimLandlock := ""

	if abi >= 1 {
		self, err := os.Executable()
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox: cannot locate own executable for the Landlock helper: %w", err)
		}
		policy := llPolicy{abi: abi, rules: landlockAllowList(runDir), blockNet: abi >= 4}
		spec, err := policy.encode()
		if err != nil {
			return nil, nil, err
		}
		prefix = []string{self, helperArg}
		extraEnv = []string{helperEnv + "=" + spec}
		fsClaim = fmt.Sprintf("Landlock ABI v%d: writes confined to the run dir; reads limited to "+
			"system dirs (/etc account databases, $HOME and /root denied)", abi)
		if abi >= 4 {
			netClaimLandlock = "Landlock denies TCP connect/bind (UDP and raw sockets NOT blocked)"
		}
	}

	rl := fmt.Sprintf("rlimits via ulimit (CPU %ds soft / %ds hard, AS %dMB)",
		int(timeout/time.Second)+5, int(timeout/time.Second)+10, rlimitBytes/1024/1024)

	// Weak plan: no network namespace.
	netWeak := "network NOT blocked"
	if netClaimLandlock != "" {
		netWeak = netClaimLandlock
	}
	weak := &isolationPlan{
		Prefix:    prefix,
		Prologue:  prologue,
		ExtraEnv:  extraEnv,
		Isolation: strings.Join([]string{rl, fsClaim, netWeak}, "; "),
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
		Alt: weak,
	}
	return strong, nil, nil
}
