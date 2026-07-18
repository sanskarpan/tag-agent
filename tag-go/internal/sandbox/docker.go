package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DockerOptions configures a containerized run via `docker run --rm`.
type DockerOptions struct {
	// Command is the shell command string, run inside the container via `sh -c`.
	Command string
	// Image is the container image to run (required).
	Image string
	// Dir is the container working directory (--workdir). Empty = image default.
	Dir string
	// Timeout caps runtime. The container is killed on timeout. Must be > 0.
	Timeout time.Duration
	// Memory is the container memory limit (docker --memory syntax, e.g. "512m").
	// Empty defaults to "512m".
	Memory string
	// CPUs is the CPU limit (docker --cpus, e.g. "1", "0.5"). Empty defaults to "1".
	CPUs string
	// Network is the docker network mode (--network). Empty defaults to "none",
	// isolating the container from the network.
	Network string
}

// DefaultDockerMemory / DefaultDockerCPUs / DefaultDockerNetwork are the
// hardened defaults applied when the corresponding option is empty.
const (
	DefaultDockerMemory  = "512m"
	DefaultDockerCPUs    = "1"
	DefaultDockerNetwork = "none"
)

// dockerBinary is the docker executable name (overridable in tests).
var dockerBinary = "docker"

// lookDockerPath resolves the docker binary on PATH, returning a clear error
// when docker is not installed/available.
func lookDockerPath() (string, error) {
	path, err := exec.LookPath(dockerBinary)
	if err != nil {
		return "", fmt.Errorf("docker not found on PATH: install Docker or use --backend restricted: %w", err)
	}
	return path, nil
}

// dockerArgs builds the `docker run` argument vector (excluding the leading
// "docker" binary name) for opts, applying hardened defaults. It is pure and
// deterministic so it can be unit-tested without invoking docker.
//
// Layout: run --rm --memory <m> --cpus <c> --network <n> [--workdir <d>] <image> sh -c <command>
func dockerArgs(opts DockerOptions) []string {
	mem := opts.Memory
	if strings.TrimSpace(mem) == "" {
		mem = DefaultDockerMemory
	}
	cpus := opts.CPUs
	if strings.TrimSpace(cpus) == "" {
		cpus = DefaultDockerCPUs
	}
	network := opts.Network
	if strings.TrimSpace(network) == "" {
		network = DefaultDockerNetwork
	}

	args := []string{
		"run", "--rm",
		"--memory", mem,
		"--cpus", cpus,
		"--network", network,
	}
	if strings.TrimSpace(opts.Dir) != "" {
		args = append(args, "--workdir", opts.Dir)
	}
	args = append(args, opts.Image, "sh", "-c", opts.Command)
	return args
}

// ExecDocker runs opts.Command inside a container via `docker run --rm` with
// resource and network hardening, capturing stdout/stderr/exit. A timeout kills
// the container and yields Exit=124 with TimedOut=true (matching the restricted
// backend's convention). It reuses the {stdout,stderr,exit,timed_out} Result
// shape.
func ExecDocker(ctx context.Context, opts DockerOptions) (*Result, error) {
	if strings.TrimSpace(opts.Command) == "" {
		return nil, errors.New("empty command")
	}
	if strings.TrimSpace(opts.Image) == "" {
		return nil, errors.New("docker backend requires --image")
	}
	if opts.Timeout <= 0 {
		return nil, errors.New("timeout must be > 0")
	}
	dockerPath, err := lookDockerPath()
	if err != nil {
		return nil, err
	}

	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// exec.CommandContext kills the docker CLI process on timeout; docker run
	// (without -d) proxies the container lifecycle, so killing the client with
	// SIGKILL stops the attached container as well. --rm cleans it up.
	cmd := exec.CommandContext(cctx, dockerPath, dockerArgs(opts)...)
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
		// Failed to start the docker client itself: surface as error.
		return nil, runErr
	}
	res.Exit = 0
	return res, nil
}
