package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Docker-backend egress enforcement (PRD-094).
//
// HOW IT ACTUALLY WORKS, because "we run docker with a flag" would be a lie:
//
// Docker's own network options are all-or-nothing (`--network none` or a
// network), so per-destination policy has to come from the kernel's routing
// table inside the container's network namespace. Programming that table needs
// CAP_NET_ADMIN — and giving the WORKLOAD that capability would let the very
// code we are confining delete the rules. So the namespace is owned by a
// separate, minimal helper container:
//
//  1. the helper starts with --cap-add NET_ADMIN on the requested network and
//     installs the policy as routes: a blackhole route per denied range, a
//     via-gateway route per allowed range, and a blackhole default route when
//     the policy default is deny. Longest-prefix match then does the matching,
//     in the kernel, for every protocol — TCP, UDP, ICMP alike.
//  2. the helper prints a sentinel ONLY after every route command has succeeded
//     (`set -e`), and TAG waits for it. No sentinel, no run: a policy that did
//     not install fails the run closed rather than running unprotected.
//  3. the workload joins the helper's namespace with `--network container:<id>`
//     and gets NO capabilities of its own, so it can read the routing table but
//     cannot change it.
//  4. the helper is torn down unconditionally when the run ends.
//
// Hostname rules are pinned to addresses at step 1 and the same mappings are
// mounted into the workload as /etc/hosts. DNS itself is left unreachable under
// a default-deny policy (the resolver is not in the allow list), which is
// deliberate: it closes DNS-tunnelled exfiltration and it means the name the
// workload connects to is the address TAG pinned.
//
// What this does NOT do is enumerate blocked connections. A blackhole route
// drops the packet without a counter TAG can attribute to a rule, so per-
// connection violation logging is not implemented and is disclosed as such
// rather than approximated.

// DefaultEgressHelperImage is the image the netns-owning helper runs. It needs
// only a shell and `ip` (busybox provides both), and it never executes user
// input. It is overridable because an air-gapped host may have a different
// minimal image already pulled.
const DefaultEgressHelperImage = "alpine:3.20"

// egressReadyMarker is printed by the helper once — and only once — every route
// command has succeeded.
const egressReadyMarker = "TAG_FW_READY"

// egressNoGatewayMarker is printed when the helper's network has no default
// route, so there is nothing to route allowed traffic through.
const egressNoGatewayMarker = "TAG_FW_NO_GATEWAY"

// egressHelperGrace is how much longer than the run timeout the helper lives, so
// it cannot expire out from under a workload that is still running.
const egressHelperGrace = 60 * time.Second

// egressReadyTimeout bounds the wait for the helper to install its policy.
const egressReadyTimeout = 45 * time.Second

// cidrTextRe is a belt-and-braces guard on what reaches the helper's shell. The
// values are already produced by net.IPNet.String(), so they cannot contain
// shell metacharacters; asserting it here means a future refactor that starts
// interpolating something else fails loudly instead of quietly.
var cidrTextRe = regexp.MustCompile(`^[0-9a-fA-F:.]+/[0-9]+$`)

// egressPlan is the concrete, enforceable form of a Policy for one docker run.
type egressPlan struct {
	// AllowNets are the ranges routed to the gateway under a default-deny policy.
	AllowNets []string
	// DenyNets are the ranges blackholed under a default-allow policy.
	DenyNets []string
	// DefaultDeny replaces the default route with a blackhole.
	DefaultDeny bool
	// Hosts maps a pinned hostname to the addresses it resolved to.
	Hosts []ResolvedHost
	// Disclosures are the policy's honest caveats, carried into Isolation.
	Disclosures []string
}

// buildEgressPlan turns a policy into the routes that will be installed.
//
// Precedence note: the kernel resolves overlapping prefixes by longest match,
// which already implements "the more specific rule wins". The one case it cannot
// resolve is two rules over the SAME prefix with opposite dispositions, so that
// is settled here, in favour of allow, matching Policy.Decide.
func buildEgressPlan(ctx context.Context, p *Policy, resolve Resolver) (*egressPlan, error) {
	resolved, err := ResolveHostRules(ctx, p, resolve)
	if err != nil {
		return nil, err
	}
	plan := &egressPlan{DefaultDeny: p.DefaultDeny, Hosts: resolved, Disclosures: p.Disclosures(resolved)}

	allowed := map[string]bool{}
	denied := map[string]bool{}
	for _, r := range p.Rules {
		if r.Kind != RuleCIDR {
			continue
		}
		if r.Deny {
			denied[r.Net.String()] = true
		} else {
			allowed[r.Net.String()] = true
		}
	}
	for _, rh := range resolved {
		for _, ip := range rh.IPs {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			key := fmt.Sprintf("%s/%d", ip.String(), bits)
			if rh.Rule.Deny {
				denied[key] = true
			} else {
				allowed[key] = true
			}
		}
	}
	for k := range allowed {
		// Explicit allow wins over an explicit deny of the same prefix.
		delete(denied, k)
	}
	for k := range allowed {
		if !cidrTextRe.MatchString(k) {
			return nil, fmt.Errorf("refusing to build an egress policy from the unexpected range %q", k)
		}
		if strings.Contains(k, ":") {
			// There is no IPv6 gateway on docker's default bridge, so a v6 allow
			// route would not carry traffic. Emitting it anyway would put an
			// "allow" in the report for a destination that stays unreachable.
			// (A v6 DENY is different: a blackhole works regardless.)
			return nil, fmt.Errorf("egress allow rule for %q cannot be enforced: the sandbox's docker network "+
				"has no IPv6 gateway to route it through, so the rule would be reported as applied while the "+
				"destination stayed unreachable. Use an IPv4 destination. The run was NOT started", k)
		}
		plan.AllowNets = append(plan.AllowNets, k)
	}
	for k := range denied {
		if !cidrTextRe.MatchString(k) {
			return nil, fmt.Errorf("refusing to build an egress policy from the unexpected range %q", k)
		}
		plan.DenyNets = append(plan.DenyNets, k)
	}
	sortStrings(plan.AllowNets)
	sortStrings(plan.DenyNets)
	return plan, nil
}

// sortStrings keeps the emitted script deterministic (and its logs diffable).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// helperScript renders the shell the helper runs. Every command runs under
// `set -e`, and the sentinel is the LAST thing before the sleep, so reaching it
// is proof the entire policy installed. Nothing here interpolates user input:
// every value is a net.IPNet.String() already checked against cidrTextRe.
//
// The subtle part is the CONNECTED SUBNET, and getting it wrong is a real hole
// rather than a cosmetic one. A blackhole default route does not cover the
// container's own link: the kernel keeps `172.17.0.0/16 dev eth0 scope link`,
// which is more specific than `default`, so under "deny everything except X"
// every SIBLING CONTAINER on the same docker network stayed reachable. (That is
// not hypothetical — it is what the integration test caught on the first run.)
// A default-deny policy therefore replaces each connected subnet with a
// blackhole as well, and re-adds only what is genuinely needed:
//
//   - an allowed address that is ON-LINK gets a /32 `dev <iface>` route, so it
//     needs no gateway at all;
//   - an allowed address that is OFF-LINK gets a `via <gw>` route, and only then
//     is the gateway's own /32 re-added (it has to be resolvable as the next
//     hop). When every allowed address is on-link the gateway — i.e. the docker
//     host — stays unreachable too.
//
// On-link versus off-link is decided with `ip route get` while the original
// table is still intact, which is the kernel's own answer rather than subnet
// arithmetic reimplemented in shell.
func helperScript(plan *egressPlan, sleepFor time.Duration) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("DEF=$(ip route show default 2>/dev/null | head -1)\n")
	b.WriteString("GW=$(echo \"$DEF\" | awk '{print $3}')\n")
	b.WriteString("IFACE=$(echo \"$DEF\" | awk '{print $5}')\n")
	b.WriteString("if [ -z \"$GW\" ] || [ -z \"$IFACE\" ]; then echo \"" + egressNoGatewayMarker + "\"; exit 1; fi\n")
	// Captured BEFORE anything is added, so our own /32 host routes are not
	// mistaken for connected subnets and blackholed.
	b.WriteString("LINKNETS=$(ip route show scope link | awk '{print $1}')\n")
	b.WriteString("NEEDGW=0\n")

	for _, n := range plan.AllowNets {
		if strings.Contains(n, ":") {
			// An IPv6 allow needs a v6 gateway, which docker's default bridge does
			// not provide. buildEgressPlan refuses such a policy rather than
			// emitting a rule that would not take effect.
			continue
		}
		addr := strings.SplitN(n, "/", 2)[0]
		fmt.Fprintf(&b, "case \"$(ip route get %s 2>/dev/null | head -1)\" in\n", addr)
		fmt.Fprintf(&b, "  *' via '*) ip route replace %s via $GW dev $IFACE; NEEDGW=1 ;;\n", n)
		fmt.Fprintf(&b, "  *) ip route replace %s dev $IFACE ;;\n", n)
		b.WriteString("esac\n")
	}
	for _, n := range plan.DenyNets {
		if strings.Contains(n, ":") {
			fmt.Fprintf(&b, "ip -6 route replace blackhole %s\n", n)
			continue
		}
		fmt.Fprintf(&b, "ip route replace blackhole %s\n", n)
	}
	if plan.DefaultDeny {
		b.WriteString("if [ \"$NEEDGW\" = 1 ]; then ip route replace $GW/32 dev $IFACE; fi\n")
		b.WriteString("for N in $LINKNETS; do ip route replace blackhole $N; done\n")
		b.WriteString("ip route del default\n")
		b.WriteString("ip route add blackhole default\n")
		// IPv6 link-local is the same lateral-movement path over v6, and it is
		// on-link, so the v6 default alone does not cover it.
		b.WriteString("ip -6 route replace blackhole fe80::/64\n")
		// A v6 default may simply not exist on a host without IPv6; its absence
		// is not a policy failure, so only this pair is tolerant.
		b.WriteString("ip -6 route del default 2>/dev/null || true\n")
		b.WriteString("ip -6 route add blackhole default 2>/dev/null || true\n")
	}
	b.WriteString("echo " + egressReadyMarker + "\n")
	b.WriteString("ip route show\n")
	fmt.Fprintf(&b, "sleep %d\n", int(sleepFor.Seconds()))
	return b.String()
}

// hostsFileContents renders the /etc/hosts the workload is given: loopback plus
// exactly the ALLOWED pinned names. A denied name is deliberately absent — the
// workload should fail to resolve it, not resolve it and then be blackholed.
func hostsFileContents(plan *egressPlan) string {
	var b strings.Builder
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	for _, rh := range plan.Hosts {
		if rh.Rule.Deny {
			continue
		}
		for _, ip := range rh.IPs {
			fmt.Fprintf(&b, "%s\t%s\n", ip.String(), rh.Rule.Host)
		}
	}
	return b.String()
}

// egressEnforcement is a started helper plus everything ExecDocker needs to
// attach a workload to it.
type egressEnforcement struct {
	// NetworkMode is the value to pass as the workload's --network.
	NetworkMode string
	// HostsPath is a host file to bind-mount at /etc/hosts, or "" for none.
	HostsPath string
	// Isolation is the honest description of what was installed.
	Isolation string
	// Routes is the helper's routing table after installation, as evidence.
	Routes string

	cleanup func()
}

// Close tears the helper (and the temp hosts file) down.
func (e *egressEnforcement) Close() {
	if e != nil && e.cleanup != nil {
		e.cleanup()
	}
}

// startEgressHelper installs the policy in a fresh network namespace and returns
// how to join it. Every failure path tears down whatever it created and returns
// an error: there is no partial-enforcement success.
func startEgressHelper(ctx context.Context, dockerPath, image, network string,
	plan *egressPlan, runTimeout time.Duration) (*egressEnforcement, error) {

	name := "tag-egress-" + strings.TrimPrefix(containerName(), "tag-sandbox-")
	script := helperScript(plan, runTimeout+egressHelperGrace)

	args := []string{"run", "-d", "--rm", "--name", name,
		"--cap-add", "NET_ADMIN", "--network", network,
		"--memory", "64m", "--cpus", "0.5",
		image, "sh", "-c", script}
	startCtx, cancelStart := context.WithTimeout(ctx, egressReadyTimeout)
	defer cancelStart()
	var startErr bytes.Buffer
	start := exec.CommandContext(startCtx, dockerPath, args...)
	start.Stderr = &startErr
	if err := start.Run(); err != nil {
		return nil, fmt.Errorf("egress policy NOT applied and the command was NOT run: the firewall helper "+
			"container (%s) could not be started: %v: %s", image, err, strings.TrimSpace(startErr.String()))
	}
	remove := func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rmCancel()
		_ = exec.CommandContext(rmCtx, dockerPath, "rm", "-f", name).Run()
	}

	routes, err := waitForEgressReady(ctx, dockerPath, name)
	if err != nil {
		remove()
		return nil, err
	}

	hostsDir, err := os.MkdirTemp("", "tag-egress-hosts")
	if err != nil {
		remove()
		return nil, fmt.Errorf("egress policy NOT applied and the command was NOT run: %w", err)
	}
	hostsPath := filepath.Join(hostsDir, "hosts")
	if err := os.WriteFile(hostsPath, []byte(hostsFileContents(plan)), 0o644); err != nil { //nolint:gosec // must be world-readable inside the container
		remove()
		os.RemoveAll(hostsDir)
		return nil, fmt.Errorf("egress policy NOT applied and the command was NOT run: %w", err)
	}

	return &egressEnforcement{
		NetworkMode: "container:" + name,
		HostsPath:   hostsPath,
		Routes:      routes,
		cleanup: func() {
			remove()
			os.RemoveAll(hostsDir)
		},
	}, nil
}

// waitForEgressReady blocks until the helper prints its sentinel, and returns
// the routing table it dumped. A helper that dies, or that has not finished
// within egressReadyTimeout, is an error — never a run that proceeds anyway.
func waitForEgressReady(ctx context.Context, dockerPath, name string) (string, error) {
	deadline := time.Now().Add(egressReadyTimeout)
	for {
		logCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, _ := exec.CommandContext(logCtx, dockerPath, "logs", name).CombinedOutput()
		cancel()
		text := string(out)
		if strings.Contains(text, egressReadyMarker) {
			_, routes, _ := strings.Cut(text, egressReadyMarker)
			return strings.TrimSpace(routes), nil
		}
		if strings.Contains(text, egressNoGatewayMarker) {
			return "", fmt.Errorf("egress policy NOT applied and the command was NOT run: the firewall helper "+
				"found no default route on network %q, so there is no gateway to route allowed traffic through",
				name)
		}
		// A helper that exited without the sentinel failed to install the policy.
		inspectCtx, icancel := context.WithTimeout(ctx, 10*time.Second)
		st, _ := exec.CommandContext(inspectCtx, dockerPath, "inspect", "-f", "{{.State.Running}}", name).Output()
		icancel()
		if strings.TrimSpace(string(st)) == "false" {
			return "", fmt.Errorf("egress policy NOT applied and the command was NOT run: the firewall helper "+
				"exited before installing the policy: %s", strings.TrimSpace(text))
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("egress policy NOT applied and the command was NOT run: the firewall helper "+
				"did not confirm the policy within %s: %s", egressReadyTimeout, strings.TrimSpace(text))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// egressIsolationClaim renders what the installed policy actually gives you.
func egressIsolationClaim(p *Policy, plan *egressPlan, routes string) string {
	parts := []string{
		p.Summary(),
		"ENFORCED in the kernel by routes in a network namespace the sandbox cannot modify " +
			"(the workload holds no NET_ADMIN; the namespace is owned by a separate helper container)",
	}
	if plan.DefaultDeny {
		parts = append(parts, "the default route AND the container's own connected subnet(s) are blackholed "+
			"(IPv4, plus the IPv6 default and fe80::/64), so every destination outside the allow list is "+
			"unreachable for TCP, UDP and ICMP alike — including sibling containers on the same docker "+
			"network and, unless an allowed destination needs it as a next hop, the docker gateway itself; "+
			"DNS is therefore also unreachable and the allowed names are supplied through /etc/hosts")
	} else {
		parts = append(parts, "denied ranges are blackholed; everything else is reachable")
	}
	if routes != "" {
		parts = append(parts, "installed routes: "+strings.Join(strings.Fields(strings.ReplaceAll(routes, "\n", " | ")), " "))
	}
	parts = append(parts, plan.Disclosures...)
	return strings.Join(parts, "; ")
}

// egressFailClosedIsolation is Result.Isolation when a policy could not be
// installed. It follows the package's "none (failed closed: ...)" convention and
// claims nothing, because nothing was applied and nothing was run.
const egressFailClosedIsolation = "none (failed closed: the egress policy could not be enforced, " +
	"so the command was NOT run)"

// applyDockerEgress installs opts.Egress for one docker run.
//
// It returns the started enforcement (possibly nil when none was needed), the
// isolation claim to append, and an error that ExecDocker turns into a
// fail-closed exit 127. Three shapes, three answers:
//
//   - open policy: nothing to do, behaviour is byte-identical to before.
//   - blanket deny: `--network none` IS the enforcement — docker gives the
//     container no interface but loopback — so no helper is started. If the
//     caller explicitly asked for a network, that contradiction is reported
//     rather than silently overridden.
//   - anything with destination granularity: the helper path.
func applyDockerEgress(ctx context.Context, dockerPath string, opts *DockerOptions) (*egressEnforcement, string, error) {
	p := opts.Egress
	if p.IsOpen() {
		return nil, "", nil
	}
	if p.DeniesEverything() {
		requested := orDefault(opts.Network, DefaultDockerNetwork)
		if opts.NetworkExplicit && requested != "none" {
			return nil, "", fmt.Errorf("egress policy %q denies all egress but --network %s was requested; "+
				"these contradict, so nothing was run. Drop --network, or drop the deny-all policy",
				p.Name, requested)
		}
		opts.Network = "none"
		return nil, p.Summary() + "; ENFORCED by docker --network none: the container has no interface " +
			"but loopback, so no destination is reachable by any protocol", nil
	}
	// Destination granularity: the helper owns the namespace.
	if opts.NetworkExplicit && orDefault(opts.Network, DefaultDockerNetwork) == "none" {
		return nil, "", fmt.Errorf("egress policy %q permits specific destinations but --network none was "+
			"requested, which permits none; these contradict, so nothing was run", p.Name)
	}
	// The helper OWNS whatever namespace it lands in: it holds CAP_NET_ADMIN and
	// it blackholes the connected subnets and replaces the default route. That is
	// the enforcement mechanism when the namespace is one TAG created for this
	// run — and a machine-wide outage (or worse) when it is not.
	//
	//   - `--network host` puts the helper in the HOST's namespace, so the routes
	//     it installs are the host's routes. It would also not confine anything:
	//     the workload joins the same namespace, and "the sandbox cannot modify
	//     these routes" stops being true.
	//   - `--network container:<id>` reprograms a namespace belonging to something
	//     else, which TAG neither created nor tears down.
	//
	// Both are refused BEFORE the helper is started, consistent with how the rest
	// of this package handles a policy it cannot honestly enforce: fail closed,
	// exit 127, run nothing.
	if reason := unownableNamespace(orDefault(opts.Network, DefaultDockerNetwork)); reason != "" {
		return nil, "", fmt.Errorf("egress policy %q permits specific destinations, which TAG enforces with "+
			"routes installed by a helper container holding CAP_NET_ADMIN in the namespace it joins. %s "+
			"The egress policy was NOT applied and the command was NOT run. Use the default network (or a named "+
			"docker network) for a granular policy, or drop the policy if you meant to keep --network %s",
			p.Name, reason, orDefault(opts.Network, DefaultDockerNetwork))
	}
	plan, err := buildEgressPlan(ctx, p, egressResolver())
	if err != nil {
		return nil, "", err
	}
	helperNet := orDefault(opts.Network, DefaultDockerNetwork)
	if helperNet == "none" {
		// `none` is only the DEFAULT here (NetworkExplicit is false), and a
		// namespace with no interface cannot reach the destinations the policy
		// permits. Bridge is the network the policy is then applied on top of.
		helperNet = "bridge"
	}
	enf, err := startEgressHelper(ctx, dockerPath, orDefault(opts.EgressHelperImage, DefaultEgressHelperImage),
		helperNet, plan, opts.Timeout)
	if err != nil {
		return nil, "", err
	}
	opts.Network = enf.NetworkMode
	enf.Isolation = egressIsolationClaim(p, plan, enf.Routes)
	return enf, enf.Isolation, nil
}

// unownableNamespace reports why a docker network mode cannot be handed to the
// egress helper, or "" when it can. It names namespaces TAG does not create and
// therefore must not reprogram.
func unownableNamespace(network string) string {
	switch n := strings.ToLower(strings.TrimSpace(network)); {
	case n == "host":
		return "--network host would make that namespace the HOST's own, so the helper would rewrite the " +
			"machine's routing table (and the workload, sharing the namespace, would not be confined at all)."
	case strings.HasPrefix(n, "container:"):
		return "--network " + network + " would make that namespace another container's, which TAG did not " +
			"create and does not tear down, so the helper would rewrite that container's routing table."
	default:
		return ""
	}
}

// hostIPsForTest is the resolver seam used by the integration tests, which need
// pinned addresses rather than whatever live DNS returns today.
var hostIPsForTest Resolver

// egressResolver picks the resolver in force.
func egressResolver() Resolver {
	if hostIPsForTest != nil {
		return hostIPsForTest
	}
	return defaultResolver
}

// parseIPList is a small helper for tests and for callers holding literals.
func parseIPList(ss ...string) []net.IP {
	out := make([]net.IP, 0, len(ss))
	for _, s := range ss {
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}
