package sandbox

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// PRD-094 unit tests for the policy engine. Like the rest of this package's
// claim tests they carry no build tag and make no syscalls, so the exact wording
// of every refusal is pinned from ANY host — including the macOS dev machines
// where the Linux refusal can never be triggered.

func TestParseRule(t *testing.T) {
	cases := []struct {
		in   string
		kind RuleKind
		norm string
		bad  bool
	}{
		{in: "pypi.org", kind: RuleHost, norm: "pypi.org"},
		{in: "*.githubusercontent.com", kind: RuleHost, norm: "*.githubusercontent.com"},
		{in: "10.0.0.0/8", kind: RuleCIDR, norm: "10.0.0.0/8"},
		{in: "169.254.169.254", kind: RuleCIDR, norm: "169.254.169.254/32"},
		{in: "2001:db8::1", kind: RuleCIDR, norm: "2001:db8::1/128"},
		{in: "", bad: true},
		{in: "*", bad: true},         // a default, not a rule
		{in: "0.0.0.0/0", bad: true}, // ditto
		{in: "::/0", bad: true},      // ditto
		{in: "10.0.0.0/99", bad: true},
		{in: "not a host", bad: true},
		{in: "http://pypi.org", bad: true}, // a URL is not a destination rule
		{in: "pypi.org:443", bad: true},    // ports are not part of the model
	}
	for _, c := range cases {
		r, err := ParseRule(c.in, false)
		if c.bad {
			if err == nil {
				t.Errorf("ParseRule(%q) accepted an invalid rule as %+v", c.in, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRule(%q): %v", c.in, err)
			continue
		}
		if r.Kind != c.kind {
			t.Errorf("ParseRule(%q).Kind = %q, want %q", c.in, r.Kind, c.kind)
		}
		got := r.Raw
		if r.Kind == RuleCIDR {
			got = r.Net.String()
		}
		if got != c.norm {
			t.Errorf("ParseRule(%q) normalised to %q, want %q", c.in, got, c.norm)
		}
	}
}

func TestBuildPolicyRejectsNoOpAndContradictions(t *testing.T) {
	if _, err := BuildPolicy(PolicySpec{DenyAll: true, AllowAll: true}); err == nil {
		t.Error("--deny-all with --allow-all must be rejected")
	}
	// An allow rule under a default-allow policy enforces nothing. Installing it
	// silently would let a user believe they had restricted egress.
	if _, err := BuildPolicy(PolicySpec{AllowHosts: []string{"pypi.org"}}); err == nil {
		t.Error("an allow rule with no --deny-all must be rejected as a no-op")
	}
	// The mirror image: a deny rule under a default-deny policy.
	if _, err := BuildPolicy(PolicySpec{DenyAll: true, DenyHosts: []string{"evil.example"}}); err == nil {
		t.Error("a deny rule under --deny-all must be rejected as a no-op")
	}
	if _, err := BuildPolicy(PolicySpec{Named: "nope"}); err == nil {
		t.Error("an unknown named policy must be rejected")
	}
	// A CIDR passed to --allow-host (and vice versa) is a user error, not a
	// thing to guess at.
	if _, err := BuildPolicy(PolicySpec{DenyAll: true, AllowHosts: []string{"10.0.0.0/8"}}); err == nil {
		t.Error("--allow-host with a CIDR must be rejected")
	}
	if _, err := BuildPolicy(PolicySpec{AllowAll: true, DenyCIDRs: []string{"evil.example"}}); err == nil {
		t.Error("--deny-cidr with a hostname must be rejected")
	}
}

func TestPolicyShapes(t *testing.T) {
	open, err := BuildPolicy(PolicySpec{Named: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if !open.IsOpen() || open.NeedsDestinationGranularity() || open.DeniesEverything() {
		t.Errorf("open policy misclassified: %+v", open)
	}
	if !(*Policy)(nil).IsOpen() {
		t.Error("a nil policy must count as open")
	}

	deny, err := BuildPolicy(PolicySpec{DenyAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if deny.IsOpen() || !deny.DeniesEverything() || deny.NeedsDestinationGranularity() {
		t.Errorf("blanket denial misclassified: %+v", deny)
	}

	mixed, err := BuildPolicy(PolicySpec{DenyAll: true, AllowHosts: []string{"pypi.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if mixed.IsOpen() || mixed.DeniesEverything() || !mixed.NeedsDestinationGranularity() {
		t.Errorf("granular policy misclassified: %+v", mixed)
	}

	// `restricted` is a blanket denial; `pypi` is granular.
	r, _ := BuiltinPolicy("restricted")
	if !r.DeniesEverything() {
		t.Error("built-in 'restricted' must be a blanket denial")
	}
	p, _ := BuiltinPolicy("pypi")
	if !p.NeedsDestinationGranularity() {
		t.Error("built-in 'pypi' must be granular")
	}
}

func TestDecidePrecedence(t *testing.T) {
	p, err := BuildPolicy(PolicySpec{
		DenyAll:    true,
		AllowHosts: []string{"pypi.org", "*.githubusercontent.com"},
		AllowCIDRs: []string{"198.18.0.0/15"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host    string
		ip      string
		allowed bool
	}{
		{host: "pypi.org", allowed: true},
		{host: "PyPI.org", allowed: true}, // names are case-insensitive
		{host: "evil.example", allowed: false},
		{host: "raw.githubusercontent.com", allowed: true},
		{host: "githubusercontent.com", allowed: true}, // *.x covers the apex too
		{host: "notgithubusercontent.com", allowed: false},
		{ip: "198.18.0.5", allowed: true},
		{ip: "8.8.8.8", allowed: false},
	}
	for _, c := range cases {
		var ip net.IP
		if c.ip != "" {
			ip = net.ParseIP(c.ip)
		}
		d := p.Decide(c.host, ip)
		if d.Allowed != c.allowed {
			t.Errorf("Decide(%q,%q) = %v (%s), want %v", c.host, c.ip, d.Allowed, d.Rule, c.allowed)
		}
	}

	// Explicit allow beats explicit deny for the same destination, regardless of
	// the order the flags were written in.
	both, err := BuildPolicy(PolicySpec{AllowAll: true, DenyCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := both.Decide("", net.ParseIP("10.1.2.3")); d.Allowed {
		t.Errorf("a denied CIDR must be denied under default-allow, got %+v", d)
	}
	if d := both.Decide("", net.ParseIP("1.2.3.4")); !d.Allowed {
		t.Errorf("an unlisted address must be allowed under default-allow, got %+v", d)
	}
}

func TestResolveHostRulesFailsClosed(t *testing.T) {
	ctx := context.Background()
	// A wildcard has no enumerable address set, so no rule can be built for it.
	wild, err := BuildPolicy(PolicySpec{DenyAll: true, AllowHosts: []string{"*.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveHostRules(ctx, wild, func(context.Context, string) ([]net.IP, error) {
		t.Fatal("a wildcard must be refused before any resolution is attempted")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("wildcard rule error = %v, want a refusal naming the wildcard", err)
	}

	// A name that does not resolve leaves the rule unenforceable either way.
	p, err := BuildPolicy(PolicySpec{DenyAll: true, AllowHosts: []string{"nope.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveHostRules(ctx, p, func(context.Context, string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	})
	if err == nil || !strings.Contains(err.Error(), "NOT started") {
		t.Errorf("unresolvable rule error = %v, want a fail-closed refusal", err)
	}
	// An empty (but non-erroring) answer is the same thing.
	_, err = ResolveHostRules(ctx, p, func(context.Context, string) ([]net.IP, error) { return nil, nil })
	if err == nil {
		t.Error("a name that resolves to zero addresses must fail closed")
	}
}

func TestRestrictedRefusalNamesThePlatformReason(t *testing.T) {
	p, err := BuildPolicy(PolicySpec{DenyAll: true, AllowHosts: []string{"pypi.org"}})
	if err != nil {
		t.Fatal(err)
	}
	for goos, want := range map[string]string{
		"darwin":  "SBPL",
		"linux":   "Landlock",
		"openbsd": "no egress-filtering primitive",
	} {
		msg := restrictedEgressRefusalMsg(p, goos)
		if !strings.Contains(msg, want) {
			t.Errorf("refusal for %s = %q, want it to mention %q", goos, msg, want)
		}
		if !strings.Contains(msg, "NOT run") {
			t.Errorf("refusal for %s must say the command was NOT run: %q", goos, msg)
		}
		if !strings.Contains(msg, "--backend docker") {
			t.Errorf("refusal for %s must name the alternative that works: %q", goos, msg)
		}
	}
	// The isolation string for the refusal must claim nothing at all.
	for _, bad := range []string{"denied", "enforced", "blocked"} {
		if strings.Contains(strings.ToLower(restrictedEgressRefusalIsolation), bad) {
			t.Errorf("the refusal isolation string must not sound like a guarantee: %q",
				restrictedEgressRefusalIsolation)
		}
	}
	if !strings.Contains(restrictedEgressRefusalIsolation, "failed closed") {
		t.Errorf("refusal isolation = %q, want the package's 'failed closed' shape", restrictedEgressRefusalIsolation)
	}
	if !strings.Contains(restrictedDenyAllUnenforceableIsolation, "failed closed") {
		t.Errorf("deny-all refusal isolation = %q, want the 'failed closed' shape",
			restrictedDenyAllUnenforceableIsolation)
	}
}

func TestBuildEgressPlanAllowWinsAndPinsHosts(t *testing.T) {
	p, err := BuildPolicy(PolicySpec{
		DenyAll:    true,
		AllowHosts: []string{"good.example"},
		AllowCIDRs: []string{"198.18.0.0/15"},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildEgressPlan(context.Background(), p, func(_ context.Context, h string) ([]net.IP, error) {
		return parseIPList("203.0.113.7", "203.0.113.8"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DefaultDeny {
		t.Error("plan must carry the default-deny disposition")
	}
	want := map[string]bool{"198.18.0.0/15": true, "203.0.113.7/32": true, "203.0.113.8/32": true}
	if len(plan.AllowNets) != len(want) {
		t.Fatalf("AllowNets = %v, want %v", plan.AllowNets, want)
	}
	for _, n := range plan.AllowNets {
		if !want[n] {
			t.Errorf("unexpected allow route %q (have %v)", n, plan.AllowNets)
		}
	}

	// The hosts file must carry exactly the pinned mappings so the sandbox can
	// resolve the allowed name with no working DNS.
	hosts := hostsFileContents(plan)
	for _, ip := range []string{"203.0.113.7", "203.0.113.8"} {
		if !strings.Contains(hosts, ip+"\tgood.example") {
			t.Errorf("hosts file missing %s -> good.example:\n%s", ip, hosts)
		}
	}

	// Allow beats deny for the SAME prefix; longest-prefix match cannot settle
	// that one, so the plan must.
	both, err := BuildPolicy(PolicySpec{AllowAll: true, DenyCIDRs: []string{"198.18.0.0/15"}})
	if err != nil {
		t.Fatal(err)
	}
	both.Rules = append(both.Rules, Rule{Kind: RuleCIDR, Raw: "198.18.0.0/15",
		Net: mustCIDR(t, "198.18.0.0/15")})
	plan2, err := buildEgressPlan(context.Background(), both, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range plan2.DenyNets {
		if n == "198.18.0.0/15" {
			t.Errorf("an explicitly allowed prefix must not also be blackholed: %v", plan2.DenyNets)
		}
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHelperScriptOnlySignalsReadyAfterEveryRule(t *testing.T) {
	plan := &egressPlan{
		DefaultDeny: true,
		AllowNets:   []string{"203.0.113.7/32"},
		DenyNets:    []string{"169.254.169.254/32"},
	}
	s := helperScript(plan, 90)
	if !strings.HasPrefix(s, "set -e\n") {
		t.Fatalf("the helper script must abort on the first failed rule:\n%s", s)
	}
	ready := strings.Index(s, egressReadyMarker)
	if ready < 0 {
		t.Fatalf("no ready marker in:\n%s", s)
	}
	// Every route command must precede the sentinel: reaching it is the ONLY
	// evidence TAG has that the policy installed.
	for _, frag := range []string{
		"ip route get 203.0.113.7",                      // on-link vs off-link decided by the kernel
		"ip route replace 203.0.113.7/32 via $GW",       // off-link form
		"ip route replace 203.0.113.7/32 dev $IFACE",    // on-link form
		"ip route replace blackhole 169.254.169.254/32", // the deny rule
		// The connected subnet is the hole a blackhole default does NOT cover:
		// without this, every sibling container stays reachable.
		"for N in $LINKNETS; do ip route replace blackhole $N; done",
		"ip -6 route replace blackhole fe80::/64",
		"ip route del default",
		"ip route add blackhole default",
	} {
		at := strings.Index(s, frag)
		if at < 0 {
			t.Errorf("missing %q in:\n%s", frag, s)
			continue
		}
		if at > ready {
			t.Errorf("%q comes AFTER the ready marker; the sentinel would lie", frag)
		}
	}
	// The gateway's own address is re-added ONLY when an allowed destination
	// actually needs it as a next hop, so "deny everything except a sibling
	// container" does not silently hand back the docker host.
	if !strings.Contains(s, `if [ "$NEEDGW" = 1 ]; then ip route replace $GW/32 dev $IFACE; fi`) {
		t.Errorf("the gateway must be re-added conditionally, not unconditionally:\n%s", s)
	}

	// A default-allow policy must NOT touch the default route or the link nets.
	open := helperScript(&egressPlan{DenyNets: []string{"169.254.169.254/32"}}, 90)
	if strings.Contains(open, "ip route del default") {
		t.Errorf("a default-allow policy must keep the default route:\n%s", open)
	}
	if strings.Contains(open, "blackhole $N") {
		t.Errorf("a default-allow policy must not blackhole the connected subnet:\n%s", open)
	}
}

// TestBuildEgressPlanRefusesUnroutableIPv6Allow: docker's default bridge has no
// IPv6 gateway, so a v6 allow route would be installed and still not carry
// traffic. Reporting it as applied would be the exact overclaim this package
// forbids, so the plan refuses instead. A v6 DENY is unaffected — a blackhole
// works with or without a gateway.
func TestBuildEgressPlanRefusesUnroutableIPv6Allow(t *testing.T) {
	p, err := BuildPolicy(PolicySpec{DenyAll: true, AllowCIDRs: []string{"2001:db8::1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildEgressPlan(context.Background(), p, nil); err == nil {
		t.Error("an IPv6 allow rule must be refused rather than reported as applied")
	}
	deny, err := BuildPolicy(PolicySpec{AllowAll: true, DenyCIDRs: []string{"2001:db8::1"}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildEgressPlan(context.Background(), deny, nil)
	if err != nil {
		t.Fatalf("an IPv6 deny rule is enforceable and must be accepted: %v", err)
	}
	if len(plan.DenyNets) != 1 || plan.DenyNets[0] != "2001:db8::1/128" {
		t.Errorf("DenyNets = %v, want [2001:db8::1/128]", plan.DenyNets)
	}
}

func TestEgressFailClosedIsolationClaimsNothing(t *testing.T) {
	if !strings.Contains(egressFailClosedIsolation, "failed closed") ||
		!strings.Contains(egressFailClosedIsolation, "NOT run") {
		t.Errorf("egressFailClosedIsolation = %q, want the package's fail-closed shape", egressFailClosedIsolation)
	}
}

func TestDisclosuresAreExplicitAboutWhatIsNotCovered(t *testing.T) {
	p, err := BuildPolicy(PolicySpec{DenyAll: true, AllowHosts: []string{"good.example"}})
	if err != nil {
		t.Fatal(err)
	}
	resolved := []ResolvedHost{{Rule: p.Rules[0], IPs: parseIPList("203.0.113.7")}}
	joined := strings.Join(p.Disclosures(resolved), " | ")
	for _, want := range []string{
		"203.0.113.7",     // the pinned address is named
		"resolves elsew",  // the TOCTOU window is disclosed
		"any port",        // port granularity is disclosed as absent
		"violation loggi", // per-connection logging is disclosed as absent
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("disclosures must mention %q, got: %s", want, joined)
		}
	}
}
