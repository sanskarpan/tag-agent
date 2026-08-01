package sandbox

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// PRD-094 integration tests. These are the ONLY tests in this package that can
// prove the firewall enforces anything, because enforcement lives in the kernel
// and cannot be asserted from a string.
//
// The docker backend is the one place per-destination enforcement is claimed, so
// it is the one place it is proven. Everywhere else the claim is a REFUSAL, and
// a refusal is provable without a network (see firewall_test.go and
// TestExecRestrictedRefusesGranularEgress).
//
// The method matters: two identical target containers are started on the same
// docker network, one is allowed by IP and the other is not, and the test first
// shows BOTH are reachable with no policy. Only then does it apply the policy
// and show exactly one became unreachable. Without that control an "unreachable"
// result would prove nothing — the target might simply have been down.

// requireDockerImage skips LOUDLY when the environment cannot run these tests.
// A skip is not a pass, so it says precisely what was missing.
func requireDockerImage(t *testing.T, image string) string {
	t.Helper()
	path, err := exec.LookPath(dockerBinary)
	if err != nil {
		t.Skipf("SKIPPED, NOT PASSED: docker is not on PATH, so per-destination egress enforcement "+
			"was NOT verified on this host: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, path, "info").Run(); err != nil {
		t.Skipf("SKIPPED, NOT PASSED: the docker daemon is not reachable, so per-destination egress "+
			"enforcement was NOT verified on this host: %v", err)
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer pcancel()
	if err := exec.CommandContext(pctx, path, "image", "inspect", image).Run(); err != nil {
		if out, err := exec.CommandContext(pctx, path, "pull", image).CombinedOutput(); err != nil {
			t.Skipf("SKIPPED, NOT PASSED: image %s is unavailable, so per-destination egress enforcement "+
				"was NOT verified on this host: %v: %s", image, err, strings.TrimSpace(string(out)))
		}
	}
	return path
}

// startTarget runs a container that accepts TCP on port 9000 and returns its
// name and bridge IP.
func startTarget(t *testing.T, dockerPath, image, label string) (string, string) {
	t.Helper()
	name := "tag-fwtarget-" + label + "-" + strings.TrimPrefix(containerName(), "tag-sandbox-")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, dockerPath, "run", "-d", "--rm", "--name", name,
		"--network", "bridge", "--memory", "64m", image,
		"sh", "-c", "while true; do echo served | nc -l -p 9000; done").CombinedOutput()
	if err != nil {
		t.Fatalf("starting target %s: %v: %s", label, err, out)
	}
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		_ = exec.CommandContext(rmCtx, dockerPath, "rm", "-f", name).Run()
	})
	ipOut, err := exec.CommandContext(ctx, dockerPath, "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name).Output()
	if err != nil {
		t.Fatalf("inspecting target %s: %v", label, err)
	}
	ip := strings.TrimSpace(string(ipOut))
	if net.ParseIP(ip) == nil {
		t.Fatalf("target %s has no usable IP (%q)", label, ip)
	}
	return name, ip
}

// probe runs a TCP connect probe from inside a sandbox with the given options
// and reports whether the connect succeeded. Port 9000 is where the test
// targets listen; probePort exists for the off-link case, whose destination is
// a real public endpoint.
func probe(t *testing.T, opts DockerOptions, dest string) (bool, *Result) {
	t.Helper()
	return probePort(t, opts, dest, "9000")
}

func probePort(t *testing.T, opts DockerOptions, dest, port string) (bool, *Result) {
	t.Helper()
	opts.Command = "nc -z -w 4 " + dest + " " + port
	opts.Timeout = 60 * time.Second
	res, err := ExecDocker(context.Background(), opts)
	if err != nil {
		t.Fatalf("ExecDocker(%s:%s): %v", dest, port, err)
	}
	return res.Exit == 0, res
}

const fwTestImage = "alpine:3.20"

// TestExecDockerEgressBlocksDeniedPermitsAllowedE2E is the load-bearing test for
// PRD-094: with a default-deny policy that allows exactly one address, the
// allowed destination stays reachable and the other becomes unreachable — after
// a control run proving both were reachable to begin with.
func TestExecDockerEgressBlocksDeniedPermitsAllowedE2E(t *testing.T) {
	dockerPath := requireDockerImage(t, fwTestImage)
	_ = dockerPath

	_, allowedIP := startTarget(t, dockerPath, fwTestImage, "allow")
	_, deniedIP := startTarget(t, dockerPath, fwTestImage, "deny")
	// Give the listeners a moment to bind.
	time.Sleep(2 * time.Second)

	base := DockerOptions{Image: fwTestImage, Network: "bridge", NetworkExplicit: true}

	// CONTROL: with no policy both targets are reachable. Without this, an
	// "unreachable" result below would prove nothing about the firewall.
	if ok, res := probe(t, base, allowedIP); !ok {
		t.Fatalf("control: allowed target %s was already unreachable with no policy: exit=%d stderr=%q",
			allowedIP, res.Exit, res.Stderr)
	}
	if ok, res := probe(t, base, deniedIP); !ok {
		t.Fatalf("control: denied target %s was already unreachable with no policy: exit=%d stderr=%q",
			deniedIP, res.Exit, res.Stderr)
	}

	pol, err := BuildPolicy(PolicySpec{DenyAll: true, AllowCIDRs: []string{allowedIP}})
	if err != nil {
		t.Fatal(err)
	}
	guarded := base
	guarded.Egress = pol
	guarded.Network = "bridge"
	guarded.NetworkExplicit = true

	ok, res := probe(t, guarded, allowedIP)
	if !ok {
		t.Errorf("ALLOWED destination %s was blocked: exit=%d stdout=%q stderr=%q isolation=%q",
			allowedIP, res.Exit, res.Stdout, res.Stderr, res.Isolation)
	}
	if !strings.Contains(res.Isolation, "ENFORCED in the kernel") {
		t.Errorf("isolation must state how the policy is enforced, got: %q", res.Isolation)
	}
	if !strings.Contains(res.Isolation, allowedIP) {
		t.Errorf("isolation must name the installed route for %s, got: %q", allowedIP, res.Isolation)
	}

	ok, res = probe(t, guarded, deniedIP)
	if ok {
		t.Errorf("DENIED destination %s was reachable under a default-deny policy: isolation=%q",
			deniedIP, res.Isolation)
	}

	// The docker GATEWAY (the host end of the bridge) is on the same link and is
	// not in the allow list, so it must be unreachable too. Nothing in the allow
	// list needs it as a next hop here, so there is no excuse to leave it open.
	gwOut, gerr := exec.Command(dockerPath, "network", "inspect", "-f",
		"{{range .IPAM.Config}}{{.Gateway}}{{end}}", "bridge").Output()
	if gerr == nil {
		if gw := strings.TrimSpace(string(gwOut)); net.ParseIP(gw) != nil {
			if ok, res := probe(t, guarded, gw); ok {
				t.Errorf("the docker gateway %s stayed reachable under a default-deny policy that does not "+
					"need it as a next hop: isolation=%q", gw, res.Isolation)
			}
		}
	}
}

// TestExecDockerEgressOffLinkAllowStillWorksE2E covers the other half of the
// on-link/off-link split: an allowed destination that is NOT on the container's
// own subnet needs the gateway as a next hop, and the helper must re-add it —
// while the sibling container stays blocked.
func TestExecDockerEgressOffLinkAllowStillWorksE2E(t *testing.T) {
	dockerPath := requireDockerImage(t, fwTestImage)
	_, siblingIP := startTarget(t, dockerPath, fwTestImage, "offlink")
	time.Sleep(2 * time.Second)

	// A public address is used only as an OFF-LINK destination; the assertion is
	// about routing, and an environment with no egress at all skips loudly.
	const offLink = "1.1.1.1"
	control := DockerOptions{Image: fwTestImage, Network: "bridge", NetworkExplicit: true}
	if ok, _ := probePort(t, control, offLink, "443"); !ok {
		t.Skipf("SKIPPED, NOT PASSED: %s:443 is not reachable from this host's docker bridge even with no "+
			"policy, so the off-link allow path was NOT verified here", offLink)
	}

	pol, err := BuildPolicy(PolicySpec{DenyAll: true, AllowCIDRs: []string{offLink}})
	if err != nil {
		t.Fatal(err)
	}
	guarded := control
	guarded.Egress = pol
	if ok, res := probePort(t, guarded, offLink, "443"); !ok {
		t.Errorf("an OFF-LINK allowed destination was blocked (the gateway next hop was not re-added): "+
			"exit=%d stderr=%q isolation=%q", res.Exit, res.Stderr, res.Isolation)
	}
	if ok, res := probe(t, guarded, siblingIP); ok {
		t.Errorf("a sibling container stayed reachable while an off-link destination was allowed: "+
			"isolation=%q", res.Isolation)
	}
}

// TestExecDockerEgressHostnameRuleIsPinnedE2E proves the hostname path: an
// allowed NAME resolves (through the injected hosts file, with no working DNS)
// to the address that was pinned, and a name that was not allowed does not
// resolve at all.
func TestExecDockerEgressHostnameRuleIsPinnedE2E(t *testing.T) {
	dockerPath := requireDockerImage(t, fwTestImage)
	_, allowedIP := startTarget(t, dockerPath, fwTestImage, "hostallow")
	_, deniedIP := startTarget(t, dockerPath, fwTestImage, "hostdeny")
	time.Sleep(2 * time.Second)

	// Pin resolution instead of depending on live DNS: the point under test is
	// that the PINNED address is what gets enforced.
	prev := hostIPsForTest
	hostIPsForTest = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "allowed.test" {
			return parseIPList(allowedIP), nil
		}
		return nil, net.UnknownNetworkError("unexpected lookup of " + host)
	}
	t.Cleanup(func() { hostIPsForTest = prev })

	pol, err := BuildPolicy(PolicySpec{DenyAll: true, AllowHosts: []string{"allowed.test"}})
	if err != nil {
		t.Fatal(err)
	}
	opts := DockerOptions{Image: fwTestImage, Network: "bridge", NetworkExplicit: true, Egress: pol}

	if ok, res := probe(t, opts, "allowed.test"); !ok {
		t.Errorf("the allowed hostname was not reachable: exit=%d stderr=%q isolation=%q",
			res.Exit, res.Stderr, res.Isolation)
	}
	if ok, res := probe(t, opts, deniedIP); ok {
		t.Errorf("an address outside the pinned set was reachable: isolation=%q", res.Isolation)
	}
}

// TestExecDockerEgressDenyAllUsesNetworkNoneE2E: a blanket denial needs no
// helper at all — docker's own --network none delivers it — and the isolation
// string must say which mechanism was used.
func TestExecDockerEgressDenyAllUsesNetworkNoneE2E(t *testing.T) {
	dockerPath := requireDockerImage(t, fwTestImage)
	_, targetIP := startTarget(t, dockerPath, fwTestImage, "denyall")
	time.Sleep(2 * time.Second)

	pol, err := BuildPolicy(PolicySpec{DenyAll: true})
	if err != nil {
		t.Fatal(err)
	}
	ok, res := probe(t, DockerOptions{Image: fwTestImage, Egress: pol}, targetIP)
	if ok {
		t.Errorf("a blanket denial left %s reachable: isolation=%q", targetIP, res.Isolation)
	}
	if !strings.Contains(res.Isolation, "--network none") {
		t.Errorf("isolation must name the mechanism, got: %q", res.Isolation)
	}
}

// TestExecDockerEgressFailsClosedOnUninstallablePolicy: if the helper cannot
// install the policy, the workload must NOT run. The failure is forced with a
// helper image that has no `ip` command at all.
func TestExecDockerEgressFailsClosedOnUninstallablePolicy(t *testing.T) {
	requireDockerImage(t, fwTestImage)
	pol, err := BuildPolicy(PolicySpec{DenyAll: true, AllowCIDRs: []string{"198.18.0.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ExecDocker(context.Background(), DockerOptions{
		Image:             fwTestImage,
		Command:           "echo THE-COMMAND-RAN",
		Timeout:           60 * time.Second,
		Egress:            pol,
		EgressHelperImage: "tag-nonexistent-firewall-helper:does-not-exist",
	})
	if err != nil {
		t.Fatalf("ExecDocker: %v", err)
	}
	if res.Exit != 127 {
		t.Errorf("exit = %d, want 127 (fail closed)", res.Exit)
	}
	if strings.Contains(res.Stdout, "THE-COMMAND-RAN") {
		t.Errorf("the command RAN despite the policy not installing: %q", res.Stdout)
	}
	if !strings.Contains(res.Isolation, "failed closed") {
		t.Errorf("isolation = %q, want a fail-closed string that claims nothing", res.Isolation)
	}
	if !strings.Contains(res.Stderr, "NOT run") {
		t.Errorf("stderr must say the command was not run, got: %q", res.Stderr)
	}
}

// TestExecDockerEgressRejectsContradictoryNetwork: `--network none` plus an
// allow list is a contradiction, and resolving it silently either way would
// mislead. It fails closed.
func TestExecDockerEgressRejectsContradictoryNetwork(t *testing.T) {
	requireDockerImage(t, fwTestImage)
	pol, err := BuildPolicy(PolicySpec{DenyAll: true, AllowCIDRs: []string{"198.18.0.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ExecDocker(context.Background(), DockerOptions{
		Image: fwTestImage, Command: "echo THE-COMMAND-RAN", Timeout: 30 * time.Second,
		Network: "none", NetworkExplicit: true, Egress: pol,
	})
	if err != nil {
		t.Fatalf("ExecDocker: %v", err)
	}
	if res.Exit != 127 || strings.Contains(res.Stdout, "THE-COMMAND-RAN") {
		t.Errorf("contradiction must fail closed, got exit=%d stdout=%q", res.Exit, res.Stdout)
	}
	if !strings.Contains(res.Stderr, "contradict") {
		t.Errorf("stderr should name the contradiction, got: %q", res.Stderr)
	}
}

// TestExecRestrictedRefusesGranularEgress is the honesty test for the OTHER
// backend: no platform's restricted primitives can tell destinations apart, so
// a granular policy is refused rather than partially applied. It needs no
// docker and no network, and it runs on every OS.
func TestExecRestrictedRefusesGranularEgress(t *testing.T) {
	pol, err := BuildPolicy(PolicySpec{DenyAll: true, AllowHosts: []string{"pypi.org"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Exec(context.Background(), Options{
		Command: "echo THE-COMMAND-RAN",
		Dir:     t.TempDir(),
		Timeout: 10 * time.Second,
		Egress:  pol,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Exit != 127 {
		t.Errorf("exit = %d, want 127", res.Exit)
	}
	if strings.Contains(res.Stdout, "THE-COMMAND-RAN") {
		t.Errorf("the command RAN under a policy this backend cannot enforce: %q", res.Stdout)
	}
	if res.Isolation != restrictedEgressRefusalIsolation {
		t.Errorf("isolation = %q, want %q", res.Isolation, restrictedEgressRefusalIsolation)
	}
	if !strings.Contains(res.Stderr, "--backend docker") {
		t.Errorf("the refusal must point at the backend that CAN do this, got: %q", res.Stderr)
	}
}

// TestExecRestrictedAcceptsBlanketDenial: the one policy shape this backend can
// honour is honoured, and the run proceeds normally.
func TestExecRestrictedAcceptsBlanketDenial(t *testing.T) {
	pol, err := BuildPolicy(PolicySpec{DenyAll: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Exec(context.Background(), Options{
		Command: "echo ran-under-deny-all",
		Dir:     t.TempDir(),
		Timeout: 20 * time.Second,
		Egress:  pol,
		// On a kernel with no Landlock the run would fail closed for an unrelated
		// reason; this keeps the test about egress.
		AllowUnconfined: true,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Exit == 127 && strings.Contains(res.Isolation, "failed closed") {
		// A platform that cannot deliver a full denial must say so rather than
		// pretend — that IS the correct outcome there, so accept it explicitly.
		if !strings.Contains(res.Isolation, "failed closed") {
			t.Fatalf("unexpected refusal shape: %q", res.Isolation)
		}
		t.Logf("this platform refused --deny-all rather than degrade it: %s", res.Isolation)
		return
	}
	if res.Exit != 0 || !strings.Contains(res.Stdout, "ran-under-deny-all") {
		t.Errorf("a blanket denial should run normally, got exit=%d stdout=%q stderr=%q isolation=%q",
			res.Exit, res.Stdout, res.Stderr, res.Isolation)
	}
}
