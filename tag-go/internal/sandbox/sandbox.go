// Package sandbox provides a restricted command-execution backend (Go port of
// src/tag/sandbox.py's `restricted` backend). It runs a shell command confined
// to a working directory with a timeout and a minimal environment, capturing
// stdout/stderr/exit. It reuses the path-confinement idea from internal/tool
// (EvalSymlinks guard) to resolve the working directory to a real path.
//
// This is a best-effort restriction on the host: it constrains cwd, env and
// runtime, but it is not a full OS-level jail (Python's version wraps macOS in
// sandbox-exec / Linux in rlimits — those platform jails are not reproduced
// here; see the parity note in the CLI wiring).
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Exit     int    `json:"exit"`
	TimedOut bool   `json:"timed_out"`
}

// confineDir resolves dir to a real absolute path, following symlinks so the
// caller works against the concrete location (macOS /tmp -> /private/tmp). It
// requires the directory to exist and be a directory. Reuses the EvalSymlinks
// guard idea from internal/tool.resolvePath.
func confineDir(dir string) (string, error) {
	if dir == "" {
		return os.Getwd()
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

// Exec runs opts.Command in a restricted subprocess and returns its result. A
// timeout yields Exit=124 with TimedOut=true (matching Python's convention).
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

	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", opts.Command)
	cmd.Dir = runDir
	// Minimal, confined environment: a fixed PATH and HOME pinned to the run dir
	// (mirrors Python's restricted backend env), so the command cannot lean on
	// the caller's HOME-relative secrets.
	cmd.Env = []string{
		"PATH=/usr/bin:/bin:/usr/local/bin:/usr/sbin:/sbin",
		"HOME=" + runDir,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := &Result{Stdout: stdout.String(), Stderr: stderr.String()}

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
		// Failed to start (e.g. sh missing): surface as error.
		return nil, runErr
	}
	res.Exit = 0
	return res, nil
}
