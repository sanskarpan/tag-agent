// Package sandbox provides a restricted command-execution backend (Go port of
// src/tag/sandbox.py's `restricted` backend) plus a docker backend.
//
// The restricted backend applies real OS-level confinement, not just a cwd and
// a trimmed environment. What is enforced depends on the platform, and the
// enforced set is always reported back to the caller in Result.Isolation so a
// user can tell exactly what they got:
//
//   - darwin: the command is wrapped in `sandbox-exec` with an SBPL profile that
//     denies all network access, denies read/write of /etc, /private/etc,
//     /var/db, /private/var/db and master.passwd, denies reads of the invoking
//     user's home tree (with credential dirs such as ~/.ssh and ~/.aws denied for
//     write too), re-allows read/write under the resolved run directory, and
//     makes /usr, /bin, /sbin, /System and /Library read-only. If sandbox-exec is
//     missing the run FAILS CLOSED with exit 127 (matching Python).
//   - linux: RLIMIT_CPU and RLIMIT_AS are applied via a shell `ulimit` prologue,
//     Landlock (ABI v1+, no CGO) confines the filesystem to an allow-list that
//     excludes /etc/passwd and the user's home, and network egress is blocked
//     via Landlock ABI v4 TCP scoping and/or an unprivileged network namespace.
//     Layers that the running kernel cannot provide are DEGRADED, never faked --
//     the reduced guarantee is spelled out in Result.Isolation.
//   - everything else: FAILS CLOSED, pointing at `--backend docker`.
//
// The path-confinement idea (EvalSymlinks guard) is reused from internal/tool so
// the working directory resolves to a real path before it is baked into a
// policy.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Options configures a sandboxed run.
type Options struct {
	// Command is the shell command string (run via `sh -c`).
	Command string
	// Dir is the working directory. Empty = current working directory.
	Dir string
	// Timeout caps runtime. Non-positive is rejected (mirrors Python).
	Timeout time.Duration
}

// Result is the captured outcome of a sandboxed run.
//
// The stdout/stderr/exit/timed_out shape is unchanged and backward compatible;
// Isolation is additive and describes the confinement that was actually applied.
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Exit     int    `json:"exit"`
	TimedOut bool   `json:"timed_out"`
	// Isolation is a human-readable description of the confinement layers that
	// were actually active for this run (never an aspirational claim).
	Isolation string `json:"isolation"`
}

// isolationPlan is how a platform asks Exec to launch the command.
type isolationPlan struct {
	// Prefix is argv prepended before the `sh -c <script>` invocation.
	Prefix []string
	// Prologue is prepended to the shell script (e.g. `ulimit` calls).
	Prologue string
	// ExtraEnv is appended to the child's environment.
	ExtraEnv []string
	// SysProcAttr, when set, is applied to the child process.
	SysProcAttr *syscall.SysProcAttr
	// Isolation describes the guarantees this plan delivers.
	Isolation string
	// Alt is a weaker fallback plan used only when the process cannot be
	// started with this plan (e.g. unprivileged netns denied by the kernel).
	Alt *isolationPlan
}

// confineDir resolves dir to a real absolute path, following symlinks so the
// caller works against the concrete location (macOS /tmp -> /private/tmp). It
// requires the directory to exist and be a directory. Reuses the EvalSymlinks
// guard idea from internal/tool.resolvePath.
func confineDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("sandbox dir is not a directory: " + dir)
	}
	return real, nil
}

// Exec runs opts.Command in an OS-confined subprocess and returns its result. A
// timeout yields Exit=124 with TimedOut=true (matching Python's convention).
//
// If the platform cannot provide isolation, Exec fails closed: it returns a
// Result with a non-zero exit and an explanatory stderr rather than running the
// command unconfined.
func Exec(ctx context.Context, opts Options) (*Result, error) {
	if strings.TrimSpace(opts.Command) == "" {
		return nil, errors.New("empty command")
	}
	if opts.Timeout <= 0 {
		return nil, errors.New("timeout must be > 0")
	}
	runDir, err := confineDir(opts.Dir)
	if err != nil {
		return nil, err
	}

	plan, failClosed, err := buildIsolation(runDir, opts.Timeout)
	if err != nil {
		return nil, err
	}
	if failClosed != nil {
		return failClosed, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	for {
		res, startErr := runPlan(cctx, plan, runDir, opts.Command)
		if startErr != nil && plan.Alt != nil {
			// The kernel refused this confinement (e.g. user namespaces
			// disabled). Degrade to the weaker, honestly-labelled plan.
			plan = plan.Alt
			continue
		}
		if startErr != nil {
			return nil, startErr
		}
		return res, nil
	}
}

// runPlan launches one isolationPlan. A non-nil error means the process could
// not be started at all (the caller may then try plan.Alt).
func runPlan(cctx context.Context, plan *isolationPlan, runDir, command string) (*Result, error) {
	argv := append(append([]string{}, plan.Prefix...), "sh", "-c", plan.Prologue+command)
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = runDir
	// Minimal, confined environment: a fixed PATH and HOME pinned to the run dir
	// (mirrors Python's restricted backend env), so the command cannot lean on
	// the caller's HOME-relative secrets.
	cmd.Env = append([]string{
		"PATH=/usr/bin:/bin:/usr/local/bin:/usr/sbin:/sbin",
		"HOME=" + runDir,
	}, plan.ExtraEnv...)
	cmd.SysProcAttr = plan.SysProcAttr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	runErr := cmd.Wait()
	res := &Result{Stdout: stdout.String(), Stderr: stderr.String(), Isolation: plan.Isolation}

	if cctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.Exit = 124
		return res, nil
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.Exit = ee.ExitCode()
			return res, nil
		}
		return nil, runErr
	}
	res.Exit = 0
	return res, nil
}
