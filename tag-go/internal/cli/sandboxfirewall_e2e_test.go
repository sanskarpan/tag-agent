package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// PRD-094 CLI contract tests. The enforcement itself is proven in
// internal/sandbox (against real docker); these pin the surface: exit codes,
// --json shape, and — most importantly — that the restricted backend REFUSES a
// policy it cannot enforce instead of running the command without it.

// TestE2ESandboxEgressRestrictedRefusesGranularPolicy is the honesty contract at
// the CLI level: the command must not run, the exit code must be the documented
// fail-closed 127, and the reported isolation must claim nothing.
func TestE2ESandboxEgressRestrictedRefusesGranularPolicy(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "sandbox", "run", "echo THE-COMMAND-RAN",
		"--dir", h, "--deny-all", "--allow-host", "pypi.org", "--json")
	if code != 127 {
		t.Fatalf("exit = %d, want 127 (fail closed): %s", code, out)
	}
	if strings.Contains(out, "THE-COMMAND-RAN") {
		t.Errorf("the command RAN under a policy the restricted backend cannot enforce: %s", out)
	}
	var res struct {
		Exit      int    `json:"exit"`
		Stderr    string `json:"stderr"`
		Isolation string `json:"isolation"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(out), "\n}", 2)[0]+"\n}"), &res); err != nil {
		// The payload is pretty-printed; fall back to scanning the text.
		if !strings.Contains(out, "failed closed") {
			t.Fatalf("could not parse --json output and it does not report a fail-closed isolation: %s", out)
		}
		return
	}
	if !strings.Contains(res.Isolation, "failed closed") {
		t.Errorf("isolation = %q, want a fail-closed string that claims nothing", res.Isolation)
	}
	if !strings.Contains(res.Stderr, "--backend docker") {
		t.Errorf("the refusal must point at the backend that CAN enforce it: %q", res.Stderr)
	}
}

// TestE2ESandboxEgressRejectsNoOpAndContradictions: a policy the user got wrong
// is a usage error (exit 2), resolved before anything runs.
func TestE2ESandboxEgressUsageErrors(t *testing.T) {
	h := newHome(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"allow-without-deny-all", []string{"--allow-host", "pypi.org"}, "no effect"},
		{"deny-all-and-allow-all", []string{"--deny-all", "--allow-all"}, "contradict"},
		{"unknown-policy", []string{"--egress", "nope"}, "unknown egress policy"},
		{"cidr-in-allow-host", []string{"--deny-all", "--allow-host", "10.0.0.0/8"}, "not a hostname"},
		{"wildcard-rule", []string{"--deny-all", "--allow-host", "*"}, "default, not a rule"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"sandbox", "run", "echo THE-COMMAND-RAN", "--dir", h}, c.args...)
			out, code := run(t, h, args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage): %s", code, out)
			}
			if strings.Contains(out, "THE-COMMAND-RAN") {
				t.Errorf("the command RAN despite an invalid policy: %s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("error = %q, want it to mention %q", strings.TrimSpace(out), c.want)
			}
		})
	}
}

// TestE2ESandboxFirewallListAndShow pins the read-only surface, including the
// --json array shape.
func TestE2ESandboxFirewallListAndShow(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "sandbox", "firewall", "list", "--json")
	if code != 0 {
		t.Fatalf("firewall list --json: exit %d: %s", code, out)
	}
	var rows []struct {
		Name        string   `json:"name"`
		Default     string   `json:"default"`
		Rules       []string `json:"rules"`
		Enforcement string   `json:"enforcement"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("firewall list --json is not a JSON array: %v: %s", err, out)
	}
	byName := map[string]string{}
	for _, r := range rows {
		byName[r.Name] = r.Default
		if r.Rules == nil {
			t.Errorf("policy %q has null rules; --json must emit [] not null", r.Name)
		}
	}
	for name, def := range map[string]string{"open": "allow", "restricted": "deny", "pypi": "deny"} {
		if byName[name] != def {
			t.Errorf("built-in %q default = %q, want %q (have %v)", name, byName[name], def, byName)
		}
	}

	// `show` must state per-backend enforcement next to the rules, so a reader
	// cannot take "deny" for a guarantee the restricted backend will deliver.
	out, code = run(t, h, "sandbox", "firewall", "show", "pypi")
	if code != 0 {
		t.Fatalf("firewall show pypi: exit %d: %s", code, out)
	}
	if !strings.Contains(out, "pypi.org") {
		t.Errorf("show must list the rules: %s", out)
	}
	if !strings.Contains(out, "docker") || !strings.Contains(out, "REFUSES") {
		t.Errorf("show must say which backend enforces this and which refuses it: %s", out)
	}
}

// TestE2ESandboxFirewallTest pins the decision surface and its precedence.
func TestE2ESandboxFirewallTest(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "sandbox", "firewall", "test", "pypi.org", "--egress", "pypi", "--json")
	if code != 0 {
		t.Fatalf("firewall test: exit %d: %s", code, out)
	}
	var d struct {
		Allowed     bool   `json:"allowed"`
		Rule        string `json:"rule"`
		Enforcement string `json:"enforcement"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("firewall test --json is not JSON: %v: %s", err, out)
	}
	if !d.Allowed || d.Rule != "allow:pypi.org" {
		t.Errorf("pypi.org under the pypi policy = %+v, want an explicit allow", d)
	}
	if d.Enforcement == "" {
		t.Error("every verdict must carry the per-backend enforcement note")
	}

	out, code = run(t, h, "sandbox", "firewall", "test", "evil.example", "--egress", "pypi")
	if code != 0 {
		t.Fatalf("firewall test: exit %d: %s", code, out)
	}
	if !strings.Contains(out, "DENY") {
		t.Errorf("an unlisted host under a default-deny policy must be DENY: %s", out)
	}
	// Explicit allow beats explicit deny.
	out, _ = run(t, h, "sandbox", "firewall", "test", "10.1.2.3",
		"--allow-all", "--deny-cidr", "10.0.0.0/8")
	if !strings.Contains(out, "DENY") {
		t.Errorf("a denied CIDR under default-allow must be DENY: %s", out)
	}
	// A bare `test` with no policy is a usage error, not an implicit "open".
	if out, code := run(t, h, "sandbox", "firewall", "test", "pypi.org"); code != 2 {
		t.Errorf("firewall test with no policy should exit 2, got %d: %s", code, out)
	}
}

// TestE2ESandboxNoEgressFlagsIsUnchanged is the non-regression guard: with no
// egress flag at all, `sandbox run` behaves exactly as it did before PRD-094.
func TestE2ESandboxNoEgressFlagsIsUnchanged(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "sandbox", "run", "echo hello-sandbox", "--dir", h, "--timeout", "20")
	if code != 0 {
		t.Fatalf("plain sandbox run: exit %d: %s", code, out)
	}
	if !strings.Contains(out, "hello-sandbox") {
		t.Errorf("plain sandbox run lost its output: %s", out)
	}
	if strings.Contains(out, "egress policy") {
		t.Errorf("a run with no egress flags must not mention an egress policy: %s", out)
	}
}
