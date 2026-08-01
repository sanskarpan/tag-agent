package sandbox

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// PRD-094: per-sandbox egress firewall.
//
// READ THIS FIRST, because the whole design turns on it: an egress rule that is
// not actually enforced must never be reported as enforced. This package's
// contract is that Result.Isolation describes what the run GOT, never what it
// asked for, and a firewall is the place where an aspirational claim does the
// most damage — a user who believes `--deny-all --allow-host pypi.org` is in
// force will run code they would not otherwise run.
//
// What that means per backend, concretely:
//
//   - docker: CIDR and hostname rules ARE enforced, in the kernel, by routes
//     installed in a network namespace the workload cannot modify. See
//     firewall_docker.go. This is the only backend with destination granularity.
//   - restricted, any OS: NO destination granularity is possible. macOS seatbelt
//     offers exactly `(deny network*)`; Linux offers an unprivileged network
//     namespace (all egress) and Landlock ABI>=4 TCP scoping (all TCP). None of
//     them can express "this host but not that one". A policy that denies
//     EVERYTHING therefore maps cleanly onto what the backend already does and
//     is accepted; ANY policy with per-destination granularity is REFUSED with
//     exit 127 and a pointer to --backend docker. It is never silently ignored,
//     and never partially applied while claiming to be whole.
//
// Hostname rules are resolved to IP addresses when the policy is applied, and
// the resolved addresses are what the kernel enforces. That is a real TOCTOU
// window and it is disclosed rather than hidden (see Policy.Disclosures): a name
// that maps to a rotating CDN pool may resolve to addresses the sandbox is then
// denied, and an attacker who controls DNS between resolution and connect cannot
// widen the pinned set but can shrink it. For the docker backend the sandbox is
// additionally given a hosts file containing exactly the pinned mappings and no
// working resolver, so the name it connects to is the address we pinned.

// RuleKind distinguishes the two things a rule can name.
type RuleKind string

const (
	// RuleCIDR is an address range, enforceable directly.
	RuleCIDR RuleKind = "cidr"
	// RuleHost is a hostname, enforceable only after resolution.
	RuleHost RuleKind = "host"
)

// Rule is one allow or deny entry.
type Rule struct {
	Deny bool     `json:"deny"`
	Kind RuleKind `json:"kind"`
	// Raw is the rule exactly as the user wrote it, used in every message so a
	// report always echoes the user's own words back.
	Raw string `json:"rule"`
	// Net is set for RuleCIDR.
	Net *net.IPNet `json:"-"`
	// Host is set for RuleHost, lower-cased. A leading "*." makes it a suffix
	// match over subdomains.
	Host string `json:"-"`
}

// String renders the rule for a report.
func (r Rule) String() string {
	verb := "allow"
	if r.Deny {
		verb = "deny"
	}
	return verb + ":" + r.Raw
}

// matchesHost reports whether name is covered by a hostname rule.
func (r Rule) matchesHost(name string) bool {
	if r.Kind != RuleHost {
		return false
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if strings.HasPrefix(r.Host, "*.") {
		return strings.HasSuffix(name, r.Host[1:]) || name == r.Host[2:]
	}
	return name == r.Host
}

// Policy is an egress policy: a default disposition plus explicit rules.
type Policy struct {
	// Name is the built-in policy this started from ("open", "restricted",
	// "pypi") or "custom" when composed from flags.
	Name string `json:"name"`
	// DefaultDeny inverts the disposition for anything no rule names.
	DefaultDeny bool `json:"default_deny"`
	// Rules are the explicit entries, in the order they were given. Evaluation
	// is NOT order-dependent: see Decide.
	Rules []Rule `json:"rules"`
}

// IsOpen reports whether the policy constrains nothing, i.e. whether the run can
// proceed exactly as it did before this feature existed.
func (p *Policy) IsOpen() bool {
	return p == nil || (!p.DefaultDeny && len(p.Rules) == 0)
}

// DeniesEverything reports whether the policy is a blanket denial with no
// exceptions. This is the ONE shape the restricted backend can honour, and the
// shape the docker backend can satisfy with `--network none` alone.
func (p *Policy) DeniesEverything() bool {
	if p == nil || !p.DefaultDeny {
		return false
	}
	for _, r := range p.Rules {
		if !r.Deny {
			return false
		}
	}
	return true
}

// NeedsDestinationGranularity reports whether honouring this policy requires
// telling destinations apart — the capability no restricted-backend primitive
// has on any OS.
func (p *Policy) NeedsDestinationGranularity() bool {
	return !p.IsOpen() && !p.DeniesEverything()
}

// hostRuleRe constrains a hostname rule to a DNS name, optionally with a single
// leading "*." label. It rejects anything that is not a hostname BEFORE the name
// reaches a resolver or a policy script.
var hostRuleRe = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// ParseRule parses one rule entry. A bare IP is normalised to a host-length
// CIDR, so `--deny-cidr 169.254.169.254` and `.../32` mean the same thing.
func ParseRule(raw string, deny bool) (Rule, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Rule{}, fmt.Errorf("empty egress rule")
	}
	if s == "*" || s == "0.0.0.0/0" || s == "::/0" {
		// A wildcard is a default, not a rule: representing it as a rule would
		// make it compete with the explicit entries in Decide.
		return Rule{}, fmt.Errorf("%q is a default, not a rule: use --deny-all or --allow-all", s)
	}
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return Rule{}, fmt.Errorf("invalid CIDR %q: %w", raw, err)
		}
		return Rule{Deny: deny, Kind: RuleCIDR, Raw: s, Net: n}, nil
	}
	if ip := net.ParseIP(s); ip != nil {
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		_, n, err := net.ParseCIDR(fmt.Sprintf("%s/%d", ip.String(), bits))
		if err != nil {
			return Rule{}, fmt.Errorf("invalid address %q: %w", raw, err)
		}
		return Rule{Deny: deny, Kind: RuleCIDR, Raw: s, Net: n}, nil
	}
	if !hostRuleRe.MatchString(s) {
		return Rule{}, fmt.Errorf("invalid egress rule %q: expected a hostname (optionally *.example.com), "+
			"an IP address, or a CIDR range", raw)
	}
	return Rule{Deny: deny, Kind: RuleHost, Raw: s, Host: strings.ToLower(s)}, nil
}

// builtinPolicies are the named policies selectable with --egress.
//
// `pypi` is a language-toolchain allow-list, not a claim about what those hosts
// then do: allowing pypi.org allows everything PyPI serves.
var builtinPolicies = map[string]func() *Policy{
	"open": func() *Policy { return &Policy{Name: "open"} },
	"restricted": func() *Policy {
		return &Policy{Name: "restricted", DefaultDeny: true}
	},
	"pypi": func() *Policy {
		p := &Policy{Name: "pypi", DefaultDeny: true}
		for _, h := range []string{
			"pypi.org", "files.pythonhosted.org",
			"github.com", "api.github.com", "codeload.github.com",
			"objects.githubusercontent.com", "raw.githubusercontent.com",
			"registry.npmjs.org", "proxy.golang.org", "sum.golang.org",
		} {
			r, err := ParseRule(h, false)
			if err != nil {
				panic("built-in pypi policy has an invalid rule: " + h) // unreachable: literals
			}
			p.Rules = append(p.Rules, r)
		}
		return p
	},
}

// BuiltinPolicyNames lists the named policies, sorted.
func BuiltinPolicyNames() []string {
	out := make([]string, 0, len(builtinPolicies))
	for k := range builtinPolicies {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuiltinPolicy returns a fresh copy of a named policy.
func BuiltinPolicy(name string) (*Policy, error) {
	fn, ok := builtinPolicies[name]
	if !ok {
		return nil, fmt.Errorf("unknown egress policy %q (built-ins: %s)", name, strings.Join(BuiltinPolicyNames(), ", "))
	}
	return fn(), nil
}

// PolicySpec is the flag-level description of a policy, before parsing.
type PolicySpec struct {
	// Named is the --egress built-in to start from ("" = open).
	Named string
	// AllowHosts / DenyHosts / AllowCIDRs / DenyCIDRs are the per-invocation
	// rules, already split on commas by the caller.
	AllowHosts []string
	DenyHosts  []string
	AllowCIDRs []string
	DenyCIDRs  []string
	// DenyAll / AllowAll set the default disposition explicitly.
	DenyAll  bool
	AllowAll bool
}

// Empty reports whether the spec asks for nothing at all.
func (s PolicySpec) Empty() bool {
	return s.Named == "" && !s.DenyAll && !s.AllowAll &&
		len(s.AllowHosts) == 0 && len(s.DenyHosts) == 0 &&
		len(s.AllowCIDRs) == 0 && len(s.DenyCIDRs) == 0
}

// BuildPolicy composes a spec into a Policy, applying the documented precedence:
// per-invocation rules override the named policy's default, and --allow-all /
// --deny-all set the default disposition. Contradictions are rejected rather
// than resolved silently.
func BuildPolicy(s PolicySpec) (*Policy, error) {
	if s.DenyAll && s.AllowAll {
		return nil, fmt.Errorf("--deny-all and --allow-all contradict each other; pass at most one")
	}
	p := &Policy{Name: "open"}
	if s.Named != "" {
		b, err := BuiltinPolicy(s.Named)
		if err != nil {
			return nil, err
		}
		p = b
	}
	switch {
	case s.DenyAll:
		p.DefaultDeny = true
	case s.AllowAll:
		p.DefaultDeny = false
	}
	// added counts only the PER-INVOCATION rules, so a built-in used verbatim
	// keeps its own name instead of being reported as "pypi+custom".
	added := 0
	add := func(entries []string, kind RuleKind, deny bool) error {
		for _, e := range entries {
			r, err := ParseRule(e, deny)
			if err != nil {
				return err
			}
			if r.Kind != kind {
				want := "a hostname"
				if kind == RuleCIDR {
					want = "an IP address or CIDR range"
				}
				return fmt.Errorf("%q is not %s", e, want)
			}
			p.Rules = append(p.Rules, r)
			added++
		}
		return nil
	}
	if err := add(s.AllowHosts, RuleHost, false); err != nil {
		return nil, err
	}
	if err := add(s.DenyHosts, RuleHost, true); err != nil {
		return nil, err
	}
	if err := add(s.AllowCIDRs, RuleCIDR, false); err != nil {
		return nil, err
	}
	if err := add(s.DenyCIDRs, RuleCIDR, true); err != nil {
		return nil, err
	}
	switch {
	case added > 0 && s.Named == "":
		p.Name = "custom"
	case added > 0:
		p.Name = s.Named + "+custom"
	}
	// An allow rule under a default-allow policy, or a deny rule under a
	// default-deny policy, changes nothing. Saying so is better than installing
	// a no-op and letting the user believe it did something.
	for _, r := range p.Rules {
		if !r.Deny && !p.DefaultDeny {
			return nil, fmt.Errorf("allow rule %q has no effect: the default is already allow — "+
				"pass --deny-all (or --egress restricted) to make the allow list meaningful", r.Raw)
		}
		if r.Deny && p.DefaultDeny {
			return nil, fmt.Errorf("deny rule %q has no effect: the default is already deny", r.Raw)
		}
	}
	return p, nil
}

// Decision is the outcome of evaluating a destination against a policy.
type Decision struct {
	Allowed bool   `json:"allowed"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason"`
}

// Decide evaluates a destination against the policy.
//
// Precedence is explicit-allow > explicit-deny > default, and it is evaluated on
// the RULE SET, not in list order, so re-ordering flags can never change the
// answer. `ip` may be nil when only a name is known, and `host` may be empty
// when only an address is known.
//
// This is the function `sandbox firewall test` reports, and it is deliberately
// separate from enforcement: it tells you what the policy MEANS. Whether that
// meaning is enforced for a given backend is a different question, answered by
// Policy.EnforcementFor.
func (p *Policy) Decide(host string, ip net.IP) Decision {
	if p == nil {
		return Decision{Allowed: true, Rule: "default:allow", Reason: "no egress policy"}
	}
	var denied Rule
	var haveDeny bool
	for _, r := range p.Rules {
		var hit bool
		switch r.Kind {
		case RuleHost:
			hit = host != "" && r.matchesHost(host)
		case RuleCIDR:
			hit = ip != nil && r.Net.Contains(ip)
		}
		if !hit {
			continue
		}
		if !r.Deny {
			// Explicit allow wins outright; no need to look further.
			return Decision{Allowed: true, Rule: r.String(), Reason: "matched an explicit allow rule"}
		}
		if !haveDeny {
			denied, haveDeny = r, true
		}
	}
	if haveDeny {
		return Decision{Allowed: false, Rule: denied.String(), Reason: "matched an explicit deny rule"}
	}
	if p.DefaultDeny {
		return Decision{Allowed: false, Rule: "default:deny", Reason: "no rule matched and the policy default is deny"}
	}
	return Decision{Allowed: true, Rule: "default:allow", Reason: "no rule matched and the policy default is allow"}
}

// Summary renders the policy in one line, for Result.Isolation.
func (p *Policy) Summary() string {
	if p.IsOpen() {
		return "egress policy open (no restrictions)"
	}
	def := "allow"
	if p.DefaultDeny {
		def = "deny"
	}
	if len(p.Rules) == 0 {
		return fmt.Sprintf("egress policy %q: default %s, no exceptions", p.Name, def)
	}
	parts := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		parts = append(parts, r.String())
	}
	return fmt.Sprintf("egress policy %q: default %s; %s", p.Name, def, strings.Join(parts, ", "))
}

// HostRules returns the policy's hostname rules, which are the ones that need
// resolving before anything can be enforced.
func (p *Policy) HostRules() []Rule {
	var out []Rule
	for _, r := range p.Rules {
		if r.Kind == RuleHost {
			out = append(out, r)
		}
	}
	return out
}

// ResolvedHost is one hostname rule after resolution.
type ResolvedHost struct {
	Rule Rule
	IPs  []net.IP
}

// Resolver resolves a hostname to addresses. It is an indirection purely so the
// tests can pin resolution instead of depending on live DNS.
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// defaultResolver uses the host's own resolver.
func defaultResolver(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// ResolveHostRules pins every hostname rule to addresses.
//
// A wildcard rule (`*.example.com`) CANNOT be resolved: there is no way to
// enumerate the subdomains of a name, so there is no address set to program. It
// is refused rather than silently dropped — the whole point of this package is
// that an unenforceable rule stops the run.
//
// An unresolvable name is likewise refused: an allow rule for a name we cannot
// pin would leave the destination unreachable while the user believes it is
// permitted, and a deny rule for a name we cannot pin would leave it reachable
// while the user believes it is blocked. Both are the failure this package
// exists to prevent, so both fail closed.
func ResolveHostRules(ctx context.Context, p *Policy, resolve Resolver) ([]ResolvedHost, error) {
	if resolve == nil {
		resolve = defaultResolver
	}
	var out []ResolvedHost
	for _, r := range p.HostRules() {
		if strings.HasPrefix(r.Host, "*.") {
			return nil, fmt.Errorf("egress rule %q cannot be enforced: a wildcard hostname has no enumerable "+
				"address set, so no kernel rule can be built for it. Use explicit hostnames or a CIDR range "+
				"(the run was NOT started)", r.Raw)
		}
		ips, err := resolve(ctx, r.Host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("egress rule %q cannot be enforced: %q did not resolve to any address "+
				"(%v), so the rule would be neither applied nor honoured. The run was NOT started", r.Raw, r.Host, err)
		}
		out = append(out, ResolvedHost{Rule: r, IPs: ips})
	}
	return out, nil
}

// restrictedEgressReason explains, per OS, WHY the restricted backend cannot
// tell destinations apart. It lives here (no build tag, no syscalls) so the
// exact wording can be pinned by tests from any host, exactly like the rest of
// this package's claims.
func restrictedEgressReason(goos string) string {
	switch goos {
	case "darwin":
		return "macOS seatbelt (SBPL) can express only `(deny network*)` — an all-or-nothing switch with no " +
			"per-host or per-CIDR granularity, and no way to allow one destination while denying another"
	case "linux":
		return "the Linux restricted backend blocks egress with an unprivileged network namespace (which " +
			"removes every interface but loopback, all-or-nothing) and Landlock ABI>=4 TCP scoping (which " +
			"denies ALL TCP connect/bind); neither primitive can name a destination"
	default:
		return "this platform has no egress-filtering primitive available to the restricted backend at all"
	}
}

// restrictedEgressRefusalMsg is the fail-closed message for an egress policy the
// restricted backend cannot honour. It follows the shape of the other refusals
// in this package: what is missing, that the command was NOT run, and the
// alternative that does work.
func restrictedEgressRefusalMsg(p *Policy, goos string) string {
	return fmt.Sprintf("sandbox: the restricted backend cannot enforce the %s. %s. "+
		"Applying part of it and reporting the whole would be a false guarantee, so the command was NOT run. "+
		"Use --backend docker, where CIDR and hostname rules are enforced by kernel routes, or reduce the "+
		"policy to a blanket denial (--deny-all with no allow rules), which this backend CAN enforce.",
		p.Summary(), restrictedEgressReason(goos))
}

// restrictedEgressRefusalIsolation is Result.Isolation for that refusal.
const restrictedEgressRefusalIsolation = "none (failed closed: the restricted backend cannot enforce a " +
	"per-destination egress policy, so the command was NOT run)"

// restrictedDenyAllUnenforceableMsg is the message for the one remaining honest
// gap on Linux: the policy IS a blanket denial, which the backend can express,
// but the kernel refused the network namespace that would deliver it. Degrading
// to "TCP only" or "not blocked" would contradict what the user asked for, so
// the run is refused instead.
const restrictedDenyAllUnenforceableMsg = "sandbox: --deny-all was requested but this kernel would not " +
	"create the unprivileged user+network namespace that delivers it, and the remaining layers block at " +
	"most TCP. A partial denial reported as a denial is exactly the false guarantee this backend refuses " +
	"to make, so the command was NOT run. Use --backend docker for enforced egress control."

// restrictedDenyAllUnenforceableIsolation is Result.Isolation for that refusal.
const restrictedDenyAllUnenforceableIsolation = "none (failed closed: --deny-all could not be delivered on " +
	"this kernel, so the command was NOT run)"

// Disclosures lists the things this policy does NOT guarantee, in the caller's
// own terms. They are appended to Result.Isolation, because a guarantee a reader
// has to infer is a guarantee they will over-read.
func (p *Policy) Disclosures(resolved []ResolvedHost) []string {
	var out []string
	if len(resolved) > 0 {
		names := make([]string, 0, len(resolved))
		for _, rh := range resolved {
			ips := make([]string, 0, len(rh.IPs))
			for _, ip := range rh.IPs {
				ips = append(ips, ip.String())
			}
			names = append(names, fmt.Sprintf("%s=[%s]", rh.Rule.Raw, strings.Join(ips, " ")))
		}
		out = append(out, "hostname rules are enforced as the ADDRESSES they resolved to when the policy was "+
			"applied ("+strings.Join(names, ", ")+"); a name that later resolves elsewhere (a rotating CDN pool, "+
			"a DNS change mid-run) is NOT covered, and the sandbox is given these mappings in /etc/hosts with no "+
			"working resolver so it cannot reach a different address for the same name")
	}
	out = append(out, "enforcement is per-DESTINATION only: an allowed host is allowed for any port and any "+
		"payload, so this is not a data-exfiltration control for destinations you allow")
	out = append(out, "blocked connections are counted by neither this run nor the kernel in a form TAG can "+
		"read back: per-connection violation logging is NOT implemented, so a blocked attempt shows up only as "+
		"the sandboxed command's own connection error")
	return out
}
