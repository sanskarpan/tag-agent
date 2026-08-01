package permission

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

// funcPauser adapts a function to Pauser.
type funcPauser struct {
	fn     func(ctx context.Context, req Request, timeout time.Duration) (Response, error)
	called int
	sawTO  time.Duration
}

func (f *funcPauser) Pause(ctx context.Context, req Request, timeout time.Duration) (Response, error) {
	f.called++
	f.sawTO = timeout
	return f.fn(ctx, req, timeout)
}

// screener is a test Screener.
type screener struct {
	inBlocked, outBlocked bool
	reason                string
	inCalls, outCalls     int
}

func (s *screener) ScreenToolInput(context.Context, string, map[string]any) (bool, string) {
	s.inCalls++
	return s.inBlocked, s.reason
}
func (s *screener) ScreenToolResult(context.Context, string, string) (bool, string) {
	s.outCalls++
	return s.outBlocked, s.reason
}

func askGuard(t *testing.T) *Guard {
	t.Helper()
	return NewGuard(DefaultPolicy(), nil, nil)
}

func bashReq() Request {
	return Request{Tool: ToolBash, Kind: KindCommand, Subject: "echo hi",
		Args: map[string]any{"command": "echo hi"}}
}

// TestPauseGateApproves: an installed Pauser turns `ask` into an allow when the
// out-of-process reviewer says yes.
func TestPauseGateApproves(t *testing.T) {
	g := askGuard(t)
	p := &funcPauser{fn: func(context.Context, Request, time.Duration) (Response, error) {
		return ResponseAllowOnce, nil
	}}
	g.Pauser = p
	g.PauseTimeout = time.Second

	d := g.Check(context.Background(), bashReq())
	if !d.Allowed() {
		t.Fatalf("expected allow, got %+v", d)
	}
	if d.Via != ViaPause {
		t.Errorf("via = %q, want %q", d.Via, ViaPause)
	}
	if p.called != 1 {
		t.Errorf("pauser called %d times, want 1", p.called)
	}
	if p.sawTO != time.Second {
		t.Errorf("pauser got timeout %s, want 1s", p.sawTO)
	}
}

// TestPauseGateDenies: a reviewer's "no" is a deny, and the reason names the gate.
func TestPauseGateDenies(t *testing.T) {
	g := askGuard(t)
	g.Pauser = &funcPauser{fn: func(context.Context, Request, time.Duration) (Response, error) {
		return ResponseDeny, nil
	}}
	g.PauseTimeout = time.Second

	d := g.Check(context.Background(), bashReq())
	if d.Allowed() {
		t.Fatal("a denied approval must not allow the call")
	}
	if !strings.Contains(d.Reason, "approval gate") {
		t.Errorf("reason should name the gate: %q", d.Reason)
	}
}

// TestPauseGateWithoutTimeoutFailsClosed is the anti-hang invariant at the type
// level: a Pauser that arrives with no bounded deadline is a wiring bug, and the
// Guard must refuse it rather than call into an unbounded wait.
func TestPauseGateWithoutTimeoutFailsClosed(t *testing.T) {
	g := askGuard(t)
	p := &funcPauser{fn: func(context.Context, Request, time.Duration) (Response, error) {
		t.Fatal("the Pauser must NOT be called without a bounded timeout")
		return ResponseAllowOnce, nil
	}}
	g.Pauser = p
	g.PauseTimeout = 0

	d := g.Check(context.Background(), bashReq())
	if d.Allowed() {
		t.Fatal("an unbounded approval gate must fail CLOSED")
	}
	if !strings.Contains(d.Reason, "bounded timeout") {
		t.Errorf("reason must explain the refusal: %q", d.Reason)
	}
	if p.called != 0 {
		t.Errorf("pauser was invoked %d times despite no deadline", p.called)
	}
}

// TestNoPauserIsImmediateDeny: the pre-existing non-interactive contract is
// untouched — with neither Prompter nor Pauser, `ask` denies at once.
func TestNoPauserIsImmediateDeny(t *testing.T) {
	g := askGuard(t)
	done := make(chan Decision, 1)
	go func() { done <- g.Check(context.Background(), bashReq()) }()
	select {
	case d := <-done:
		if d.Allowed() {
			t.Fatal("headless ask must deny")
		}
		if d.Via != "non-interactive" {
			t.Errorf("via = %q, want non-interactive", d.Via)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HANG: a headless `ask` blocked instead of denying immediately")
	}
}

// TestPauseGateNeverConsultedForDenyRule: an explicit deny short-circuits before
// any human is asked. A gate that parks on a call the policy already refused
// would burn a reviewer's attention and, worse, offer them a way to approve it.
func TestPauseGateNeverConsultedForDenyRule(t *testing.T) {
	pol := DefaultPolicy()
	pol.Rules = append([]Rule{{Tool: ToolBash, Kind: KindCommand, Action: Deny, Source: "test"}}, pol.Rules...)
	g := NewGuard(pol, nil, nil)
	p := &funcPauser{fn: func(context.Context, Request, time.Duration) (Response, error) {
		t.Fatal("a deny rule must never reach the approval gate")
		return ResponseAllowOnce, nil
	}}
	g.Pauser = p
	g.PauseTimeout = time.Second

	if d := g.Check(context.Background(), bashReq()); d.Allowed() {
		t.Fatal("deny rule must win")
	}
	if p.called != 0 {
		t.Errorf("pauser called %d times for a deny rule", p.called)
	}
}

// TestCredentialPathStillProtectedWithPauseGate pins the load-bearing property
// from the shipped model: a BLANKET allow is skipped for a credential path, and
// adding the approval gate must not turn that structural deny into something a
// reviewer can wave through.
func TestCredentialPathStillProtectedWithPauseGate(t *testing.T) {
	src := Sources{Flags: []Rule{{Tool: ToolReadFile, Kind: KindPath, Action: Allow, Source: "flag"}}}
	g := NewGuard(Policy{Rules: Resolve(src)}, nil, nil)
	p := &funcPauser{fn: func(context.Context, Request, time.Duration) (Response, error) {
		t.Fatal("a credential-path deny must never be offered to a reviewer")
		return ResponseAllowOnce, nil
	}}
	g.Pauser = p
	g.PauseTimeout = time.Second

	for _, path := range []string{"/repo/.env", "/home/u/.ssh/id_rsa", "/repo/server.pem"} {
		d := g.Check(context.Background(), Request{Tool: ToolReadFile, Kind: KindPath, Subject: path})
		if d.Allowed() {
			t.Errorf("%s was allowed despite the built-in credential deny: %+v", path, d)
		}
		if d.Via != "rule" {
			t.Errorf("%s: via = %q, want a rule deny", path, d.Via)
		}
	}
	if p.called != 0 {
		t.Errorf("pauser called %d times on credential paths", p.called)
	}
}

// TestScreenerBlocksBeforeRules: a content guardrail outranks an explicit allow
// rule. `--allow-tool bash` grants the SUBJECT; it must not smuggle banned
// CONTENT past the guardrail.
func TestScreenerBlocksBeforeRules(t *testing.T) {
	src := Sources{Flags: []Rule{{Tool: ToolBash, Kind: KindCommand, Action: Allow, Source: "flag"}}}
	g := NewGuard(Policy{Rules: Resolve(src)}, nil, nil)
	g.Screener = &screener{inBlocked: true, reason: "guardrail blocked: destructive shell command"}

	d := g.Check(context.Background(), bashReq())
	if d.Allowed() {
		t.Fatal("an allow rule must not defeat the content guardrail")
	}
	if d.Via != ViaTripwire {
		t.Errorf("via = %q, want %q", d.Via, ViaTripwire)
	}
	if !strings.Contains(d.Reason, "destructive shell command") {
		t.Errorf("the block reason must be reported honestly: %q", d.Reason)
	}
}

// TestDangerouslyAllowAllBypassesScreener keeps the documented meaning of the
// escape hatch: it disables the gate ENTIRELY, guardrail included. Anything else
// would make the flag's own warning a lie.
func TestDangerouslyAllowAllBypassesScreener(t *testing.T) {
	pol := DefaultPolicy()
	pol.DangerouslyAllowAll = true
	g := NewGuard(pol, nil, nil)
	sc := &screener{inBlocked: true, reason: "blocked"}
	g.Screener = sc

	if d := g.Check(context.Background(), bashReq()); !d.Allowed() {
		t.Fatalf("--dangerously-allow-all must bypass everything: %+v", d)
	}
	if sc.inCalls != 0 {
		t.Errorf("screener consulted %d times under --dangerously-allow-all", sc.inCalls)
	}
}

// TestNilGuardStillFailsClosedWithScreener re-pins the nil-Guard contract now
// that Wrap does more work: a wiring mistake must still produce a refusal, not
// an ungated tool.
func TestNilGuardStillFailsClosedWithScreener(t *testing.T) {
	ran := false
	tl := agent.Tool{
		Def:  llm.ToolDef{Name: "danger"},
		Exec: func(context.Context, map[string]any) (string, error) { ran = true; return "did it", nil },
	}
	wrapped := Wrap(nil, tl, NoSubject)

	out, err := wrapped.Exec(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("a nil Guard must fail CLOSED, not execute the tool")
	}
	if ran {
		t.Fatal("SIDE EFFECT RAN through a nil Guard")
	}
	if out != "" {
		t.Errorf("a denied tool must return no output, got %q", out)
	}
	var de *DeniedError
	if !asDenied(err, &de) {
		t.Fatalf("want *DeniedError, got %T: %v", err, err)
	}
}

// TestPostHookWithholdsResult: the tool ran, but a guardrail-blocked RESULT is
// withheld from the model with an honest explanation — never returned as a
// success and never silently emptied.
func TestPostHookWithholdsResult(t *testing.T) {
	pol := DefaultPolicy()
	pol.Rules = append([]Rule{{Tool: "leaky", Action: Allow, Source: "test"}}, pol.Rules...)
	g := NewGuard(pol, nil, nil)
	sc := &screener{outBlocked: true, reason: "guardrail blocked: credential-shaped value in tool result"}
	g.Screener = sc

	ran := false
	tl := agent.Tool{
		Def: llm.ToolDef{Name: "leaky"},
		Exec: func(context.Context, map[string]any) (string, error) {
			ran = true
			return "AKIAIOSFODNN7EXAMPLE", nil
		},
	}
	out, err := Wrap(g, tl, NoSubject).Exec(context.Background(), map[string]any{})
	if !ran {
		t.Fatal("the post hook must run AFTER the tool, not instead of it")
	}
	if err == nil {
		t.Fatal("a blocked result must surface as an error, not as a quiet empty string")
	}
	if strings.Contains(out, "AKIA") || strings.Contains(err.Error(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("the withheld secret leaked into the response: out=%q err=%v", out, err)
	}
	if !strings.Contains(err.Error(), "already ran") {
		t.Errorf("the message must admit the side effect happened: %v", err)
	}
	if sc.outCalls != 1 {
		t.Errorf("result screener called %d times, want 1", sc.outCalls)
	}
}

// TestScreenerNotConsultedWhenDenied: no point screening content for a call that
// is already refused, and doing so would double-count a tripwire.
func TestScreenerNotConsultedWhenDenied(t *testing.T) {
	pol := DefaultPolicy()
	pol.Rules = append([]Rule{{Tool: "blocked", Action: Deny, Source: "test"}}, pol.Rules...)
	g := NewGuard(pol, nil, nil)
	sc := &screener{}
	g.Screener = sc

	tl := agent.Tool{Def: llm.ToolDef{Name: "blocked"},
		Exec: func(context.Context, map[string]any) (string, error) { return "x", nil }}
	if _, err := Wrap(g, tl, NoSubject).Exec(context.Background(), map[string]any{}); err == nil {
		t.Fatal("expected a deny")
	}
	if sc.outCalls != 0 {
		t.Errorf("result screener ran %d times on a denied call", sc.outCalls)
	}
}

// TestInteractiveReportsBothRoutes: `permissions show` and the loop rely on this
// predicate to decide whether an `ask` can reach anyone at all.
func TestInteractiveReportsBothRoutes(t *testing.T) {
	g := askGuard(t)
	if g.Interactive() || g.PauseGated() || g.Prompting() {
		t.Fatal("a bare headless guard reaches nobody")
	}
	g.Pauser = &funcPauser{fn: func(context.Context, Request, time.Duration) (Response, error) {
		return ResponseDeny, nil
	}}
	if !g.Interactive() || !g.PauseGated() || g.Prompting() {
		t.Errorf("with a Pauser: interactive=%v pauseGated=%v prompting=%v",
			g.Interactive(), g.PauseGated(), g.Prompting())
	}
}

func asDenied(err error, target **DeniedError) bool {
	de, ok := err.(*DeniedError)
	if ok {
		*target = de
	}
	return ok
}
