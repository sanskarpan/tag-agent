package permission

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A guardrail `interrupt` outcome (PRD-123 require-approval) must turn a call the
// rules would ALLOW into one that routes through the approval gate — pause for a
// human, not run silently. RED against pre-#718 code, where the screener had no
// interrupt channel and an interrupt verdict was treated as a pass (allowed).

// TestScreenerInterruptRoutesToApprovalGate: allow rule + interrupt → the Pauser
// is consulted; the reviewer's yes becomes an allow-via-pause.
func TestScreenerInterruptRoutesToApprovalGate(t *testing.T) {
	src := Sources{Flags: []Rule{{Tool: ToolBash, Kind: KindCommand, Action: Allow, Source: "flag"}}}
	g := NewGuard(Policy{Rules: Resolve(src)}, nil, nil)
	g.Screener = &screener{inInterrupt: true, reason: "HTTP POST — approve?"}
	p := &funcPauser{fn: func(context.Context, Request, time.Duration) (Response, error) {
		return ResponseAllowOnce, nil
	}}
	g.Pauser = p
	g.PauseTimeout = time.Second

	d := g.Check(context.Background(), bashReq())
	if !d.Allowed() {
		t.Fatalf("an approved interrupt must allow the call, got %+v", d)
	}
	if d.Via != ViaPause {
		t.Errorf("via = %q, want %q (it must go through the pause gate)", d.Via, ViaPause)
	}
	if p.called != 1 {
		t.Errorf("the Pauser must be consulted exactly once, got %d", p.called)
	}
}

// TestScreenerInterruptDeniesWhenNoReviewer: allow rule + interrupt, but no
// approval mechanism and no TTY → deny (an interrupt is never silently allowed).
func TestScreenerInterruptDeniesWithoutReviewer(t *testing.T) {
	src := Sources{Flags: []Rule{{Tool: ToolBash, Kind: KindCommand, Action: Allow, Source: "flag"}}}
	g := NewGuard(Policy{Rules: Resolve(src)}, nil, nil)
	g.Screener = &screener{inInterrupt: true, reason: "approve?"}
	// No Pauser, no Prompter → non-interactive.

	d := g.Check(context.Background(), bashReq())
	if d.Allowed() {
		t.Fatal("an interrupt with no way to approve must deny, not silently allow")
	}
}

// TestScreenerInterruptDeniedByReviewer: the reviewer's no is a deny.
func TestScreenerInterruptDeniedByReviewer(t *testing.T) {
	src := Sources{Flags: []Rule{{Tool: ToolBash, Kind: KindCommand, Action: Allow, Source: "flag"}}}
	g := NewGuard(Policy{Rules: Resolve(src)}, nil, nil)
	g.Screener = &screener{inInterrupt: true, reason: "approve?"}
	g.Pauser = &funcPauser{fn: func(context.Context, Request, time.Duration) (Response, error) {
		return ResponseDeny, nil
	}}
	g.PauseTimeout = time.Second

	if d := g.Check(context.Background(), bashReq()); d.Allowed() {
		t.Fatal("a reviewer's denial of an interrupt must deny the call")
	}
}

// TestScreenerBlockOutranksInterrupt: when the screener BLOCKS, it is refused
// outright and never offered for approval, even with a Pauser installed.
func TestScreenerBlockOutranksInterrupt(t *testing.T) {
	src := Sources{Flags: []Rule{{Tool: ToolBash, Kind: KindCommand, Action: Allow, Source: "flag"}}}
	g := NewGuard(Policy{Rules: Resolve(src)}, nil, nil)
	sc := &screener{inBlocked: true, reason: "guardrail blocked: rm -rf"}
	g.Screener = sc
	p := &funcPauser{fn: func(context.Context, Request, time.Duration) (Response, error) {
		t.Fatal("a blocked call must never reach the approval gate")
		return ResponseAllowOnce, nil
	}}
	g.Pauser = p
	g.PauseTimeout = time.Second

	d := g.Check(context.Background(), bashReq())
	if d.Allowed() {
		t.Fatal("a block must refuse outright")
	}
	if d.Via != ViaTripwire {
		t.Errorf("via = %q, want %q", d.Via, ViaTripwire)
	}
	if !strings.Contains(d.Reason, "rm -rf") {
		t.Errorf("the block reason must be reported honestly: %q", d.Reason)
	}
}
