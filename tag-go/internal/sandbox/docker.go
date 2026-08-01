package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// Egress is the PRD-094 per-sandbox egress policy. Nil (or an open policy)
	// keeps the pre-existing behaviour exactly. A policy that denies everything
	// is satisfied by Network=none. Anything in between is ENFORCED with routes
	// in a helper-owned network namespace (firewall_docker.go), and the run fails
	// closed if that policy cannot be installed.
	Egress *Policy
	// EgressHelperImage overrides the image the namespace-owning helper runs.
	// Empty uses DefaultEgressHelperImage.
	EgressHelperImage string
	// NetworkExplicit tells ExecDocker that Network came from the user rather
	// than from the default, so a contradiction between `--network none` and an
	// allow list can be reported instead of silently resolved.
	NetworkExplicit bool
}

// DefaultDockerMemory / DefaultDockerCPUs / DefaultDockerNetwork are the
// hardened defaults applied when the corresponding option is empty.
const (
	DefaultDockerMemory  = "512m"
	DefaultDockerCPUs    = "1"
	DefaultDockerNetwork = "none"
)

// orDefault returns v, or def when v is blank. Shared by dockerArgs and the
// reported isolation string so the two can never disagree.
func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

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
// deterministic so it can be unit-tested without invoking docker. When name is
// non-empty it is passed as --name so the container can be torn down out of
// band on timeout.
//
// Layout: run --rm [--name <name>] --memory <m> --cpus <c> --network <n> [--workdir <d>] <image> sh -c <command>
func dockerArgs(opts DockerOptions, name string) []string {
	return dockerArgsWith(opts, name, nil)
}

// dockerArgsWith is dockerArgs plus flags the egress enforcer needs to inject
// (the namespace to join and the pinned hosts file). It exists as a separate
// entry point so dockerArgs keeps the exact shape its tests pin.
func dockerArgsWith(opts DockerOptions, name string, extra []string) []string {
	mem := orDefault(opts.Memory, DefaultDockerMemory)
	cpus := orDefault(opts.CPUs, DefaultDockerCPUs)
	network := orDefault(opts.Network, DefaultDockerNetwork)

	args := []string{"run", "--rm"}
	if strings.TrimSpace(name) != "" {
		args = append(args, "--name", name)
	}
	args = append(args,
		"--memory", mem,
		"--cpus", cpus,
		"--network", network,
	)
	args = append(args, extra...)
	if strings.TrimSpace(opts.Dir) != "" {
		args = append(args, "--workdir", opts.Dir)
	}
	args = append(args, opts.Image, "sh", "-c", opts.Command)
	return args
}

// containerName generates a unique, docker-safe container name so the container
// started by ExecDocker can be force-removed on timeout.
func containerName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("tag-sandbox-%d", time.Now().UnixNano())
	}
	return "tag-sandbox-" + hex.EncodeToString(b[:])
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

	// PRD-094: install the egress policy BEFORE the workload starts, and fail
	// closed if it cannot be installed. Everything after this point either runs
	// under the policy or does not run at all.
	enf, egressClaim, err := applyDockerEgress(ctx, dockerPath, &opts)
	if err != nil {
		return &Result{
			Exit:      127,
			Stderr:    "sandbox: " + err.Error() + "\n",
			Isolation: egressFailClosedIsolation,
		}, nil
	}
	defer enf.Close()

	cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Give the container a known name and, on timeout/cancel, force-remove it out
	// of band. SIGKILL-ing the attached docker CLI does NOT stop the
	// daemon-managed container (SIGKILL cannot be proxied), so without this the
	// container would be orphaned and --rm would never fire.
	name := containerName()
	var extra []string
	if enf != nil && enf.HostsPath != "" {
		extra = append(extra, "-v", enf.HostsPath+":/etc/hosts:ro")
	}
	cmd := exec.CommandContext(cctx, dockerPath, dockerArgsWith(opts, name, extra)...)
	cmd.Cancel = func() error {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rmCancel()
		_ = exec.CommandContext(rmCtx, dockerPath, "rm", "-f", name).Run()
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 15 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := &Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		// Report the confinement the container actually got, so `isolation` is
		// meaningful for both backends (network=none only when so configured).
		Isolation: fmt.Sprintf("docker container: image=%s network=%s memory=%s cpus=%s",
			opts.Image, orDefault(opts.Network, DefaultDockerNetwork),
			orDefault(opts.Memory, DefaultDockerMemory), orDefault(opts.CPUs, DefaultDockerCPUs)),
	}
	if egressClaim != "" {
		res.Isolation += "; " + egressClaim
	}

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
