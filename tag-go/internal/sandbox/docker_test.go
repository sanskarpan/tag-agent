package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// contains reports whether want appears as a contiguous subslice of args (used
// to assert flag/value pairs are present and adjacent).
func containsPair(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

// TestDockerArgsDefaults verifies the argv builder applies hardened defaults
// (--rm, --memory 512m, --cpus 1, --network none) and terminates in
// `<image> sh -c <command>` — WITHOUT invoking docker.
func TestDockerArgsDefaults(t *testing.T) {
	args := dockerArgs(DockerOptions{Command: "echo hi", Image: "alpine:3.20"})

	if args[0] != "run" {
		t.Fatalf("expected first arg 'run', got %q (full: %v)", args[0], args)
	}
	if !hasArg(args, "--rm") {
		t.Errorf("missing --rm in %v", args)
	}
	if !containsPair(args, "--memory", DefaultDockerMemory) {
		t.Errorf("missing --memory %s in %v", DefaultDockerMemory, args)
	}
	if !containsPair(args, "--cpus", DefaultDockerCPUs) {
		t.Errorf("missing --cpus %s in %v", DefaultDockerCPUs, args)
	}
	if !containsPair(args, "--network", "none") {
		t.Errorf("missing --network none in %v", args)
	}
	// No --workdir when Dir empty.
	if hasArg(args, "--workdir") {
		t.Errorf("unexpected --workdir when Dir empty: %v", args)
	}
	// Tail must be: <image> sh -c <command>.
	tail := args[len(args)-4:]
	if tail[0] != "alpine:3.20" || tail[1] != "sh" || tail[2] != "-c" || tail[3] != "echo hi" {
		t.Errorf("unexpected tail %v", tail)
	}
}

// TestDockerArgsOverrides verifies explicit memory/cpus/network/workdir flow
// through and override the defaults.
func TestDockerArgsOverrides(t *testing.T) {
	args := dockerArgs(DockerOptions{
		Command: "id",
		Image:   "busybox",
		Dir:     "/work",
		Memory:  "256m",
		CPUs:    "0.5",
		Network: "bridge",
	})
	if !containsPair(args, "--memory", "256m") {
		t.Errorf("missing --memory 256m in %v", args)
	}
	if !containsPair(args, "--cpus", "0.5") {
		t.Errorf("missing --cpus 0.5 in %v", args)
	}
	if !containsPair(args, "--network", "bridge") {
		t.Errorf("missing --network bridge in %v", args)
	}
	if !containsPair(args, "--workdir", "/work") {
		t.Errorf("missing --workdir /work in %v", args)
	}
}

// TestExecDockerRejectsBadInput checks input validation without docker.
func TestExecDockerRejectsBadInput(t *testing.T) {
	if _, err := ExecDocker(context.Background(), DockerOptions{Image: "alpine", Timeout: time.Second}); err == nil {
		t.Error("expected error for empty command")
	}
	if _, err := ExecDocker(context.Background(), DockerOptions{Command: "echo x", Timeout: time.Second}); err == nil {
		t.Error("expected error for missing image")
	}
	if _, err := ExecDocker(context.Background(), DockerOptions{Command: "echo x", Image: "alpine", Timeout: 0}); err == nil {
		t.Error("expected error for non-positive timeout")
	}
}

// dockerAvailable reports whether a usable docker daemon is on PATH and up.
func dockerAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping real docker E2E")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("docker daemon not responding; skipping real docker E2E")
	}
}

const testImage = "alpine:3.20"

// pullTestImage ensures the E2E image is present locally so per-test timeouts
// aren't consumed by a first-run pull.
func pullTestImage(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "pull", testImage).CombinedOutput(); err != nil {
		t.Skipf("could not pull %s (%v): %s", testImage, err, out)
	}
}

// TestExecDockerEchoE2E: real docker run, stdout captured, exit 0.
func TestExecDockerEchoE2E(t *testing.T) {
	dockerAvailable(t)
	pullTestImage(t)
	res, err := ExecDocker(context.Background(), DockerOptions{
		Command: "echo hi from docker",
		Image:   testImage,
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("ExecDocker: %v", err)
	}
	if res.TimedOut || res.Exit != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(res.Stdout, "hi from docker") {
		t.Fatalf("stdout = %q, want it to contain 'hi from docker'", res.Stdout)
	}
}

// TestExecDockerNonzeroExitE2E: real container returns a nonzero code.
func TestExecDockerNonzeroExitE2E(t *testing.T) {
	dockerAvailable(t)
	pullTestImage(t)
	res, err := ExecDocker(context.Background(), DockerOptions{
		Command: "exit 7",
		Image:   testImage,
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("ExecDocker: %v", err)
	}
	if res.TimedOut || res.Exit != 7 {
		t.Fatalf("expected exit 7, got %+v", res)
	}
}

// TestExecDockerNetworkNoneE2E: with the default --network none, a command that
// needs the network must fail (nonzero exit), proving isolation.
func TestExecDockerNetworkNoneE2E(t *testing.T) {
	dockerAvailable(t)
	pullTestImage(t)
	res, err := ExecDocker(context.Background(), DockerOptions{
		// nslookup / wget against a public host must fail with no network.
		Command: "wget -q -T 5 -O - http://example.com",
		Image:   testImage,
		Timeout: 60 * time.Second,
		// Network defaults to none.
	})
	if err != nil {
		t.Fatalf("ExecDocker: %v", err)
	}
	if res.Exit == 0 {
		t.Fatalf("expected network failure with --network none, got exit 0 (stdout=%q)", res.Stdout)
	}
}

// TestExecDockerTimeoutE2E: a sleep longer than the timeout kills the container
// and yields exit 124 / timed_out.
func TestExecDockerTimeoutE2E(t *testing.T) {
	dockerAvailable(t)
	pullTestImage(t)
	res, err := ExecDocker(context.Background(), DockerOptions{
		Command: "sleep 30",
		Image:   testImage,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("ExecDocker: %v", err)
	}
	if !res.TimedOut || res.Exit != 124 {
		t.Fatalf("expected timeout (exit 124), got %+v", res)
	}
}
