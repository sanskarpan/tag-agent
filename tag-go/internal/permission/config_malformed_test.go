package permission

import (
	"strings"
	"testing"
)

// Regression suite for the silently-widening malformed permission rule
// (CWE-20/693).
//
// ParseLayer's own doctrine is that a malformed entry is a HARD ERROR: "a
// permission block that silently half-loads is worse than one that refuses to
// start". Three fields did not follow it. `tool`, `pattern` and `kind` were read
// with a comma-ok type assertion whose failure was discarded, so a value of the
// wrong YAML type did not fail the load — it fell through to the permissive
// default. An empty Pattern means "match ANY subject" (Rule.matches) and an
// empty Tool becomes "*", so a typo'd ALLOW rule silently became a broader allow
// than the operator wrote.

// TestMalformedPatternDoesNotWidenAnAllow is the core repro: `pattern: 42` (an
// unquoted YAML scalar, the easiest typo to make) must not turn
// `write_file: *.md = allow` into `write_file: * = allow`.
func TestMalformedPatternDoesNotWidenAnAllow(t *testing.T) {
	block := map[string]any{
		"rules": []any{
			// The operator meant pattern: "*.md". YAML hands us an int.
			map[string]any{"tool": "write_file", "pattern": 42, "action": "allow"},
		},
	}
	l, _, err := ParseLayer(block, "config")
	if err == nil {
		t.Fatalf("a malformed pattern must fail the config load; instead it parsed to %+v", l.Rules)
	}
	if !strings.Contains(err.Error(), "pattern") {
		t.Errorf("the error must name the offending field: %v", err)
	}
	if !strings.Contains(err.Error(), "rules[0]") {
		t.Errorf("the error must name the offending rule: %v", err)
	}
}

// TestMalformedPatternWideningIsObservable proves the consequence, not just the
// parse: with the old behaviour the rule matched a path the operator never
// granted. Guarded so it only runs if the load somehow succeeds.
func TestMalformedPatternWideningIsObservable(t *testing.T) {
	block := map[string]any{
		"rules": []any{
			map[string]any{"tool": "write_file", "pattern": 42, "action": "allow"},
		},
	}
	l, _, err := ParseLayer(block, "config")
	if err != nil {
		return // correctly rejected; nothing to widen
	}
	g := NewGuard(Policy{Rules: Resolve(Sources{Root: l})}, nil, nil)
	d := g.Check(t.Context(), Request{
		Tool: "write_file", Kind: KindPath, Subject: "/work/production/deploy.sh"})
	if d.Allowed() {
		t.Fatalf("WIDENED: a rule written for *.md allowed %q via %s",
			"/work/production/deploy.sh", d.Reason)
	}
}

// TestMalformedToolDoesNotWidenAcrossTools: `tool: [bash]` (a list, e.g. an
// operator reaching for a multi-tool rule that the schema does not support) fell
// through to "*", applying a bash-scoped allow to every tool.
func TestMalformedToolDoesNotWidenAcrossTools(t *testing.T) {
	block := map[string]any{
		"rules": []any{
			map[string]any{"tool": []any{"bash"}, "pattern": "git *", "action": "allow"},
		},
	}
	l, _, err := ParseLayer(block, "config")
	if err == nil {
		t.Fatalf("a malformed tool must fail the config load; instead it parsed to %+v", l.Rules)
	}
	if !strings.Contains(err.Error(), "tool") || !strings.Contains(err.Error(), "rules[0]") {
		t.Errorf("the error must name the offending field and rule: %v", err)
	}
}

// TestMalformedKindDoesNotFallThrough: `kind: 1` skipped the switch entirely and
// silently kept the tool-derived kind, so the operator's narrowing was dropped.
func TestMalformedKindDoesNotFallThrough(t *testing.T) {
	block := map[string]any{
		"rules": []any{
			map[string]any{"tool": "*", "pattern": "*.key", "action": "deny", "kind": 1},
		},
	}
	if _, _, err := ParseLayer(block, "config"); err == nil {
		t.Fatal("a malformed kind must fail the config load")
	}
}

// TestMalformedActionIsNamedClearly: a non-string action already failed (via
// ParseAction("")), but with a message that blamed the value rather than the
// type. It must still fail, and say why.
func TestMalformedActionIsNamedClearly(t *testing.T) {
	block := map[string]any{
		"rules": []any{map[string]any{"tool": "bash", "action": true}},
	}
	_, _, err := ParseLayer(block, "config")
	if err == nil {
		t.Fatal("a malformed action must fail the config load")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("the error must name the offending field: %v", err)
	}
}

// TestWellFormedRulesStillLoad is the anti-overshoot check: everything the
// documented schema allows must keep loading, including an omitted `tool`
// (which legitimately defaults to "*") and an omitted `pattern`.
func TestWellFormedRulesStillLoad(t *testing.T) {
	block := map[string]any{
		"default":      "ask",
		"auto_approve": false,
		"tools":        map[string]any{"read_file": "allow"},
		"rules": []any{
			map[string]any{"tool": "write_file", "pattern": "*.md", "action": "allow"},
			map[string]any{"pattern": "*.tf", "action": "deny", "kind": "path"},
			map[string]any{"tool": "bash", "action": "ask"},              // no pattern
			map[string]any{"action": "deny", "pattern": "*.pem"},         // no tool
			map[string]any{"tool": nil, "pattern": nil, "action": "ask"}, // explicit YAML nulls
		},
	}
	l, _, err := ParseLayer(block, "config")
	if err != nil {
		t.Fatalf("a well-formed block must still load: %v", err)
	}
	if len(l.Rules) != 5 {
		t.Fatalf("expected 5 rules, got %d: %+v", len(l.Rules), l.Rules)
	}
	if l.Rules[1].Tool != "*" || l.Rules[1].Kind != KindPath {
		t.Errorf("a rule without a tool should still default to '*': %+v", l.Rules[1])
	}
	if l.Rules[2].Pattern != "" {
		t.Errorf("a rule may legitimately omit its pattern: %+v", l.Rules[2])
	}
	if l.Rules[3].Tool != "*" || l.Rules[3].Pattern != "*.pem" {
		t.Errorf("a tool-less deny should keep its pattern: %+v", l.Rules[3])
	}
}
