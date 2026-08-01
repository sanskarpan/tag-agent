package cli

import (
	"fmt"
	"net"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/sandbox"
)

// PRD-094 CLI surface: the egress flags on `sandbox run`, plus
// `sandbox firewall list/show/test` for inspecting a policy without running
// anything.
//
// `firewall test` deliberately answers a different question from `run`: it says
// what a policy MEANS for a destination. Whether that meaning is enforced
// depends on the backend, so the answer always carries the per-backend
// enforcement note alongside the verdict — a user must never be able to read
// "DENY" here and conclude the restricted backend will deliver it.

// egressFlags collects the per-invocation egress flags.
type egressFlags struct {
	named      string
	allowHosts []string
	denyHosts  []string
	allowCIDRs []string
	denyCIDRs  []string
	denyAll    bool
	allowAll   bool
}

// splitList flattens repeatable flags that also accept comma-separated values,
// so `--allow-host a,b --allow-host c` and three separate flags agree.
func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func (e *egressFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&e.named, "egress", "",
		"named egress policy: "+strings.Join(sandbox.BuiltinPolicyNames(), "|")+
			" (enforced only by --backend docker; the restricted backend can enforce a blanket denial and "+
			"refuses anything finer)")
	c.Flags().StringArrayVar(&e.allowHosts, "allow-host", nil, "hostname the sandbox may reach (repeatable, comma-separated)")
	c.Flags().StringArrayVar(&e.denyHosts, "deny-host", nil, "hostname the sandbox may not reach (repeatable, comma-separated)")
	c.Flags().StringArrayVar(&e.allowCIDRs, "allow-cidr", nil, "IP/CIDR the sandbox may reach (repeatable, comma-separated)")
	c.Flags().StringArrayVar(&e.denyCIDRs, "deny-cidr", nil, "IP/CIDR the sandbox may not reach (repeatable, comma-separated)")
	c.Flags().BoolVar(&e.denyAll, "deny-all", false, "default-deny egress; only --allow-host/--allow-cidr destinations are reachable")
	c.Flags().BoolVar(&e.allowAll, "allow-all", false, "default-allow egress (explicit opt-out of a named policy's default)")
}

// spec renders the flags as a sandbox.PolicySpec.
func (e *egressFlags) spec() sandbox.PolicySpec {
	return sandbox.PolicySpec{
		Named:      e.named,
		AllowHosts: splitList(e.allowHosts),
		DenyHosts:  splitList(e.denyHosts),
		AllowCIDRs: splitList(e.allowCIDRs),
		DenyCIDRs:  splitList(e.denyCIDRs),
		DenyAll:    e.denyAll,
		AllowAll:   e.allowAll,
	}
}

// policy builds the effective policy, or nil when no egress flag was given at
// all (which keeps `sandbox run` byte-identical to its pre-PRD-094 behaviour).
func (e *egressFlags) policy() (*sandbox.Policy, error) {
	s := e.spec()
	if s.Empty() {
		return nil, nil
	}
	return sandbox.BuildPolicy(s)
}

// enforcementNote is the honest per-backend answer to "will this actually be
// applied?", printed next to every verdict so the two can never be read apart.
func enforcementNote(p *sandbox.Policy) string {
	switch {
	case p.IsOpen():
		return "nothing to enforce (open policy)"
	case p.DeniesEverything():
		return "ENFORCEABLE by both backends: docker via --network none, restricted via its existing " +
			"all-egress denial (which --deny-all then requires rather than degrades to)"
	default:
		return "ENFORCEABLE by --backend docker ONLY (kernel routes in a helper-owned network namespace). " +
			"The restricted backend has no per-destination primitive on any OS and REFUSES this policy " +
			"with exit 127 rather than running unprotected"
	}
}

// registerSandboxFirewall wires `sandbox firewall list/show/test`.
func registerSandboxFirewall(parent *cobra.Command) {
	fw := &cobra.Command{Use: "firewall", Short: "Inspect and test per-sandbox egress policies"}

	list := &cobra.Command{Use: "list", Short: "List the built-in egress policies", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			type row struct {
				Name        string   `json:"name"`
				Default     string   `json:"default"`
				Rules       []string `json:"rules"`
				Enforcement string   `json:"enforcement"`
			}
			rows := []row{} // [] not null
			for _, name := range sandbox.BuiltinPolicyNames() {
				p, err := sandbox.BuiltinPolicy(name)
				if err != nil {
					return err
				}
				def := "allow"
				if p.DefaultDeny {
					def = "deny"
				}
				rules := []string{}
				for _, r := range p.Rules {
					rules = append(rules, r.String())
				}
				rows = append(rows, row{name, def, rules, enforcementNote(p)})
			}
			if flagJSON {
				return emitJSON(rows)
			}
			fmt.Printf("%-12s %-8s %s\n", "Policy", "Default", "Rules")
			fmt.Println(strings.Repeat("-", 78))
			for _, r := range rows {
				rules := "(none)"
				if len(r.Rules) > 0 {
					rules = fmt.Sprintf("%d", len(r.Rules))
				}
				fmt.Printf("%-12s %-8s %s\n", r.Name, r.Default, rules)
			}
			return nil
		}}

	var showEgress egressFlags
	show := &cobra.Command{Use: "show [POLICY]", Short: "Show a policy's rules and what will actually enforce them",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := showEgress.spec()
			if len(args) == 1 {
				s.Named = args[0]
			}
			if s.Empty() {
				return usageErrorf("name a built-in policy (%s) or compose one with --allow-host/--deny-cidr/--deny-all",
					strings.Join(sandbox.BuiltinPolicyNames(), ", "))
			}
			p, err := sandbox.BuildPolicy(s)
			if err != nil {
				return usageErrorf("%v", err)
			}
			rules := []string{}
			for _, r := range p.Rules {
				rules = append(rules, r.String())
			}
			if flagJSON {
				return emitJSON(map[string]any{
					"name": p.Name, "default_deny": p.DefaultDeny, "rules": rules,
					"summary": p.Summary(), "enforcement": enforcementNote(p),
				})
			}
			fmt.Printf("Policy:      %s\n", p.Name)
			def := "allow"
			if p.DefaultDeny {
				def = "deny"
			}
			fmt.Printf("Default:     %s\n", def)
			if len(rules) == 0 {
				fmt.Println("Rules:       (none)")
			} else {
				fmt.Println("Rules:")
				for _, r := range rules {
					fmt.Printf("  %s\n", r)
				}
			}
			fmt.Printf("Enforcement: %s\n", enforcementNote(p))
			return nil
		}}
	showEgress.bind(show)

	var testEgress egressFlags
	test := &cobra.Command{Use: "test DESTINATION", Short: "Report what a policy decides for one destination",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := testEgress.spec()
			if s.Empty() {
				return usageErrorf("name a policy with --egress (%s) or compose one with "+
					"--allow-host/--deny-cidr/--deny-all", strings.Join(sandbox.BuiltinPolicyNames(), ", "))
			}
			p, err := sandbox.BuildPolicy(s)
			if err != nil {
				return usageErrorf("%v", err)
			}
			dest := strings.TrimSpace(args[0])
			host := ""
			var ip net.IP
			if ip = net.ParseIP(dest); ip == nil {
				host = dest
			}
			d := p.Decide(host, ip)
			// `test` is a pure policy question: it deliberately does NOT resolve
			// a hostname, because resolving here and enforcing later would give
			// two different answers for the same name, and the one printed at a
			// terminal is the one a user would trust.
			if flagJSON {
				return emitJSON(map[string]any{
					"policy": p.Name, "destination": dest, "allowed": d.Allowed,
					"rule": d.Rule, "reason": d.Reason, "enforcement": enforcementNote(p),
				})
			}
			verdict := "ALLOW"
			if !d.Allowed {
				verdict = "DENY"
			}
			fmt.Printf("%s  %s\n", verdict, dest)
			fmt.Printf("  policy:      %s\n", p.Name)
			fmt.Printf("  matched:     %s (%s)\n", d.Rule, d.Reason)
			fmt.Printf("  enforcement: %s\n", enforcementNote(p))
			if host != "" {
				fmt.Printf("  note:        this verdict is the POLICY's answer for the name %q. At run time the "+
					"name is resolved and the addresses it resolved to are what the kernel enforces, so a name "+
					"that resolves elsewhere later is not covered.\n", host)
			}
			return nil
		}}
	testEgress.bind(test)

	fw.AddCommand(list, show, test)
	parent.AddCommand(fw)
}
