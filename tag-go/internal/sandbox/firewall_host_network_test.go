package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression suite for the egress helper rewriting HOST routes (CWE-250/284).
//
// A granular egress policy is enforced by starting a helper container with
// CAP_NET_ADMIN and having it program the routing table of the network
// namespace it lands in. When the operator also passed `--network host`, that
// namespace IS THE HOST'S: the helper blackholes the host's connected subnets
// and replaces the host's default route. The blast radius is the machine, not
// the sandbox — and the sandbox is not confined either, since the workload joins
// the same namespace.
//
// These tests need no docker daemon: they hand applyDockerEgress a fake `docker`
// that records its argv, so the exact `--network host --cap-add NET_ADMIN`
// invocation is observable.

// fakeDocker writes a shell script that logs every invocation to logPath and
// answers `logs` with the helper's ready sentinel, so applyDockerEgress can run
// to completion without a daemon.
func fakeDocker(t *testing.T, logPath string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"case \"$1\" in\n" +
		"  logs) echo '" + egressReadyMarker + "'; echo 'default via 172.17.0.1 dev eth0' ;;\n" +
		"  inspect) echo true ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	return script
}

func granularPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := BuildPolicy(PolicySpec{DenyAll: true, AllowCIDRs: []string{"198.18.0.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGranularEgressRefusesHostNetwork is the core repro. Before the fix this
// started a CAP_NET_ADMIN helper on the host's own network namespace.
func TestGranularEgressRefusesHostNetwork(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	dockerPath := fakeDocker(t, log)
	opts := DockerOptions{
		Image: "busybox", Command: "echo hi", Timeout: 10 * time.Second,
		Network: "host", NetworkExplicit: true, Egress: granularPolicy(t),
	}
	enf, claim, err := applyDockerEgress(context.Background(), dockerPath, &opts)
	if enf != nil {
		enf.Close()
	}
	argv, _ := os.ReadFile(log)
	if strings.Contains(string(argv), "--network host") {
		t.Errorf("HOST ROUTES AT RISK: the NET_ADMIN helper was started in the host namespace:\n%s", argv)
	}
	if err == nil {
		t.Fatalf("a granular egress policy on --network host must be refused; got claim %q", claim)
	}
	for _, want := range []string{"host", "NOT run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q: %v", want, err)
		}
	}
}

// TestGranularEgressRefusesHostNetworkEvenWhenImplicit: NetworkExplicit only
// records where the value came from. It must not gate the refusal — the risk is
// the value, not its provenance.
func TestGranularEgressRefusesHostNetworkEvenWhenImplicit(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	dockerPath := fakeDocker(t, log)
	opts := DockerOptions{
		Image: "busybox", Command: "echo hi", Timeout: 10 * time.Second,
		Network: "host", NetworkExplicit: false, Egress: granularPolicy(t),
	}
	enf, _, err := applyDockerEgress(context.Background(), dockerPath, &opts)
	if enf != nil {
		enf.Close()
	}
	if err == nil {
		t.Fatal("--network host must be refused for a granular policy regardless of provenance")
	}
	if argv, _ := os.ReadFile(log); strings.Contains(string(argv), "--network host") {
		t.Errorf("the helper was still started on the host namespace:\n%s", argv)
	}
}

// TestGranularEgressRefusesJoiningAnotherNamespace: `--network container:<id>`
// is the same defect one step removed — the helper would reprogram a namespace
// TAG does not own and cannot tear down.
func TestGranularEgressRefusesJoiningAnotherNamespace(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	dockerPath := fakeDocker(t, log)
	opts := DockerOptions{
		Image: "busybox", Command: "echo hi", Timeout: 10 * time.Second,
		Network: "container:someone-elses", NetworkExplicit: true, Egress: granularPolicy(t),
	}
	enf, _, err := applyDockerEgress(context.Background(), dockerPath, &opts)
	if enf != nil {
		enf.Close()
	}
	if err == nil {
		t.Fatal("--network container:<id> must be refused for a granular policy")
	}
	if argv, _ := os.ReadFile(log); strings.Contains(string(argv), "container:someone-elses") {
		t.Errorf("the helper was still started in the foreign namespace:\n%s", argv)
	}
}

// TestDenyAllStillAcceptsHostNetworkRefusalPath: a blanket-deny policy never
// starts a helper (it is enforced by `--network none`), and the pre-existing
// contradiction message is what the operator should see.
func TestDenyAllStillAcceptsHostNetworkRefusalPath(t *testing.T) {
	p, err := BuildPolicy(PolicySpec{DenyAll: true})
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "argv.log")
	opts := DockerOptions{
		Image: "busybox", Command: "echo hi", Timeout: 10 * time.Second,
		Network: "host", NetworkExplicit: true, Egress: p,
	}
	_, _, err = applyDockerEgress(context.Background(), fakeDocker(t, log), &opts)
	if err == nil || !strings.Contains(err.Error(), "contradict") {
		t.Fatalf("deny-all + --network host should keep its contradiction error, got: %v", err)
	}
}

// TestGranularEgressStillWorksOnABridgeNetwork is the anti-overshoot check: the
// supported path must be untouched, including the default (empty network, which
// resolves to `none` and is upgraded to `bridge` for the helper).
func TestGranularEgressStillWorksOnABridgeNetwork(t *testing.T) {
	for _, network := range []string{"", "bridge", "my-custom-net"} {
		log := filepath.Join(t.TempDir(), "argv.log")
		dockerPath := fakeDocker(t, log)
		opts := DockerOptions{
			Image: "busybox", Command: "echo hi", Timeout: 10 * time.Second,
			Network: network, Egress: granularPolicy(t),
		}
		enf, claim, err := applyDockerEgress(context.Background(), dockerPath, &opts)
		if err != nil {
			t.Fatalf("network %q: a granular policy must still install: %v", network, err)
		}
		enf.Close()
		if claim == "" || !strings.HasPrefix(opts.Network, "container:") {
			t.Errorf("network %q: the workload should join the helper namespace, got %q", network, opts.Network)
		}
		argv, _ := os.ReadFile(log)
		if !strings.Contains(string(argv), "--cap-add NET_ADMIN") {
			t.Errorf("network %q: the helper should still be started: %s", network, argv)
		}
	}
}

// TestOpenPolicyIgnoresHostNetwork: no egress policy means no helper and no new
// restriction. `--network host` stays the operator's own call.
func TestOpenPolicyIgnoresHostNetwork(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	opts := DockerOptions{
		Image: "busybox", Command: "echo hi", Timeout: 10 * time.Second,
		Network: "host", NetworkExplicit: true, Egress: nil,
	}
	enf, claim, err := applyDockerEgress(context.Background(), fakeDocker(t, log), &opts)
	if err != nil || enf != nil || claim != "" {
		t.Fatalf("an open policy must be a no-op: enf=%v claim=%q err=%v", enf, claim, err)
	}
	if opts.Network != "host" {
		t.Errorf("an open policy must not rewrite the network, got %q", opts.Network)
	}
}
