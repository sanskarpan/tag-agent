package guardrail

import (
	"context"
	"testing"
)

// TestParseActionInterrupt: the interrupt action word is accepted (PRD-123
// FR-03). RED against pre-fix code, where ParseAction rejects anything but
// block/warn.
func TestParseActionInterrupt(t *testing.T) {
	a, err := ParseAction("interrupt")
	if err != nil {
		t.Fatalf("ParseAction(interrupt) errored: %v", err)
	}
	if a != ActionInterrupt {
		t.Fatalf("ParseAction(interrupt) = %q, want %q", a, ActionInterrupt)
	}
}

// TestRequireApprovalRuleCompiles: a require-approval rule loads from config and
// defaults its action to interrupt. RED pre-fix: the type is rejected.
func TestRequireApprovalRuleCompiles(t *testing.T) {
	rules, err := ParseLayer(map[string]any{
		"rules": []any{
			map[string]any{
				"name": "approve-http",
				"type": "require-approval",
				"tool": "http_*",
				// action deliberately omitted -> must default to interrupt.
			},
		},
	}, "config")
	if err != nil {
		t.Fatalf("require-approval rule failed to compile: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	if rules[0].Type != TypeRequireApproval {
		t.Fatalf("type = %q, want %q", rules[0].Type, TypeRequireApproval)
	}
	if rules[0].Action != ActionInterrupt {
		t.Fatalf("action = %q, want default %q", rules[0].Action, ActionInterrupt)
	}
}

// TestRequireApprovalInterruptsNotBlocks: firing a require-approval rule yields
// an interrupt verdict — pause, not halt — carrying the operator message.
func TestRequireApprovalInterruptsNotBlocks(t *testing.T) {
	rules := mustCompile(t, []Rule{{
		Name: "approve-http", Type: TypeRequireApproval, Stage: StageToolInput,
		Tool: "http_post", Action: ActionInterrupt, Message: "HTTP POST — approve?",
	}})
	p := &Processor{Rules: rules}
	v := p.ScanArgs(context.Background(), "http_post", map[string]any{"url": "https://x"})

	if !v.Interrupted {
		t.Fatal("a fired require-approval rule must set Interrupted")
	}
	if v.Blocked {
		t.Fatal("require-approval must not Block — it pauses, it does not halt")
	}
	if v.Outcome() != ActionInterrupt {
		t.Fatalf("Outcome = %q, want %q", v.Outcome(), ActionInterrupt)
	}
	if v.Reason == "" || !contains(v.Reason, "approve") {
		t.Fatalf("interrupt reason must carry the operator message, got %q", v.Reason)
	}
}

// TestBlockOutranksInterrupt: when a block rule and an interrupt rule both fire
// on the same call, block wins and the call is refused, never merely paused
// (Security §10 row 3).
func TestBlockOutranksInterrupt(t *testing.T) {
	rules := mustCompile(t, []Rule{
		{Name: "approve-all", Type: TypeRequireApproval, Stage: StageToolInput, Tool: "bash", Action: ActionInterrupt},
		{Name: "no-rmrf", Type: TypePattern, Stage: StageToolInput, Tool: "bash", Pattern: "rm -rf", Action: ActionBlock},
	})
	p := &Processor{Rules: rules}
	v := p.ScanArgs(context.Background(), "bash", map[string]any{"command": "rm -rf /"})

	if !v.Blocked {
		t.Fatal("a block rule firing alongside an interrupt rule must Block")
	}
	if v.Outcome() != ActionBlock {
		t.Fatalf("Outcome = %q, want %q (block outranks interrupt)", v.Outcome(), ActionBlock)
	}
}

// TestTripwireCanInterrupt: a tripwire carrying action=interrupt raises an
// interrupt (not a block) when it crosses its threshold (AC-03).
func TestTripwireCanInterrupt(t *testing.T) {
	rules := mustCompile(t, []Rule{{
		Name: "http-flood", Type: TypeTripwire, Stage: StageToolInput,
		Tool: "http_*", Threshold: 2, Action: ActionInterrupt,
	}})
	p := &Processor{Rules: rules, DB: testDB(t), SessionID: "s1"}

	if v := p.ScanArgs(context.Background(), "http_post", map[string]any{}); v.Fired() {
		t.Fatal("first call is below threshold; must not fire")
	}
	v := p.ScanArgs(context.Background(), "http_post", map[string]any{})
	if !v.Interrupted || v.Blocked {
		t.Fatalf("threshold crossing on an interrupt tripwire must Interrupt, not Block (blocked=%v interrupted=%v)", v.Blocked, v.Interrupted)
	}
}

// TestInterruptRuleStillFailsClosed: fail-closed is never downgraded to a pause.
// Uninspectable args must Block even when the only rule present is an interrupt
// rule.
func TestInterruptRuleStillFailsClosed(t *testing.T) {
	rules := mustCompile(t, []Rule{{
		Name: "approve-all", Type: TypeRequireApproval, Stage: StageToolInput, Tool: "*", Action: ActionInterrupt,
	}})
	p := &Processor{Rules: rules}
	v := p.ScanArgs(context.Background(), "bash", map[string]any{"ch": make(chan int)})
	if !v.Blocked || !v.Undecidable {
		t.Fatalf("uninspectable args must Block+Undecidable, got blocked=%v undecidable=%v", v.Blocked, v.Undecidable)
	}
	if v.Interrupted {
		t.Fatal("fail-closed must never present as an interrupt (a pause the operator could approve past)")
	}
}

// TestRequireApprovalRejectsContentMatcher: a require-approval rule matches every
// invocation, so a pattern/builtin would be silently ignored — Compile must
// refuse it rather than mislead.
func TestRequireApprovalRejectsContentMatcher(t *testing.T) {
	_, err := Compile([]Rule{{
		Name: "bad", Type: TypeRequireApproval, Tool: "bash", Pattern: "rm -rf", Action: ActionInterrupt,
	}})
	if err == nil {
		t.Fatal("a require-approval rule carrying a pattern must be rejected at load")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
