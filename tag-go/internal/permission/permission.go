// Package permission implements the consent gate for tool execution.
//
// Every tool the agent loop can execute is routed through a Guard before its
// side effects run. The Guard resolves an ordered rule list (most specific
// first) down to a single verdict: allow, ask, or deny. `ask` is escalated to a
// human via a Prompter when — and only when — one is available; with no
// Prompter (the headless case: `queue worker`, `cron daemon`, `dag run
// --execute`, `gateway`, CI) `ask` resolves to DENY immediately. It never
// blocks on input and never silently auto-approves. Automation opts in
// explicitly and loudly via AutoApprove (--auto-approve) or an allow rule.
//
// The package has no I/O dependencies of its own: prompting and auditing are
// injected interfaces, so the whole gate is testable without a terminal or a
// database.
package permission

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Action is the three-valued policy verdict for a tool call.
type Action string

// Via values that name the mechanism which finalised a verdict. The pre-existing
// ones ("rule", "session", "prompt", "auto-approve", "non-interactive",
// "dangerously-allow-all") are string literals at their sites; the two added by
// PRD-078/PRD-123 are named so the audit log and tests agree on the spelling.
const (
	// ViaPause means an out-of-process reviewer decided it (PRD-078).
	ViaPause = "approval-gate"
	// ViaTripwire means a content guardrail decided it (PRD-123).
	ViaTripwire = "tripwire"
	// SourceTripwire labels the synthetic rule recorded for a guardrail block.
	SourceTripwire = "guardrail"
)

const (
	// Allow executes the tool with no human interaction.
	Allow Action = "allow"
	// Ask escalates to a human when a Prompter is available, otherwise denies.
	Ask Action = "ask"
	// Deny refuses the call and returns an error to the model.
	Deny Action = "deny"
)

// ParseAction validates a policy word.
func ParseAction(s string) (Action, error) {
	switch Action(strings.ToLower(strings.TrimSpace(s))) {
	case Allow:
		return Allow, nil
	case Ask:
		return Ask, nil
	case Deny:
		return Deny, nil
	}
	return "", fmt.Errorf("invalid permission action %q (want allow, ask, or deny)", s)
}

// Kind describes what a tool's permission "subject" is, which selects the
// pattern-matching dialect. A rule declaring a Kind only matches tools of that
// Kind, so a path glob like "*.pem" can never accidentally match a shell
// command and vice versa.
type Kind string

const (
	// KindNone is a tool with no meaningful subject (matched by pattern-less rules only).
	KindNone Kind = ""
	// KindPath is a filesystem path argument; patterns are globs.
	KindPath Kind = "path"
	// KindCommand is a shell command line; patterns are command prefixes.
	KindCommand Kind = "command"
)

// Rule is one entry of the resolved, ordered ruleset. The first rule that
// matches a request decides it.
type Rule struct {
	// Tool is an exact tool name, or "*" for any tool.
	Tool string
	// Kind restricts the rule to tools with this subject kind. Empty = any.
	Kind Kind
	// Pattern matches the request subject. Empty = match any subject.
	Pattern string
	// Action is the verdict when this rule matches.
	Action Action
	// Source records where the rule came from, for audit and `permissions show`
	// (e.g. "flag", "config:profile", "config", "builtin", "session").
	Source string
}

// String renders a rule the way `tag permissions show` prints it and the way it
// is recorded in the audit log.
func (r Rule) String() string {
	subj := r.Pattern
	if subj == "" {
		subj = "*"
	}
	kind := ""
	if r.Kind != KindNone {
		kind = " " + string(r.Kind)
	}
	return fmt.Sprintf("%s%s:%s = %s [%s]", r.Tool, kind, subj, r.Action, r.Source)
}

// matches reports whether the rule applies to a request.
func (r Rule) matches(req Request) bool {
	if r.Tool != "*" && r.Tool != req.Tool {
		return false
	}
	if r.Kind != KindNone && r.Kind != req.Kind {
		return false
	}
	if r.Pattern == "" {
		return true
	}
	switch req.Kind {
	case KindPath:
		return MatchPath(r.Pattern, req.Subject)
	case KindCommand:
		return MatchCommand(r.Pattern, req.Subject)
	default:
		// A pattern-bearing rule cannot match a subject-less tool.
		return false
	}
}

// Request is one tool invocation awaiting a verdict.
type Request struct {
	// Tool is the registered tool name (e.g. "bash", "write_file").
	Tool string
	// Kind is the tool's subject kind.
	Kind Kind
	// Subject is the concrete, already-resolved argument the rule matches
	// against: an ABSOLUTE, lexically cleaned filesystem path for KindPath (so a
	// "../.." traversal cannot dodge a rule) or the full command line for
	// KindCommand.
	Subject string
	// Args is the raw tool input, shown to the human and summarised in the audit log.
	Args map[string]any
}

// Describe renders the request the way it is shown to a human and stored.
func (r Request) Describe() string {
	if r.Subject == "" {
		return r.Tool
	}
	label := "arg"
	switch r.Kind {
	case KindPath:
		label = "path"
	case KindCommand:
		label = "command"
	}
	return fmt.Sprintf("%s (%s: %s)", r.Tool, label, r.Subject)
}

// Decision is a fully resolved verdict. Action is only ever Allow or Deny — an
// `ask` has already been escalated or safely defaulted by the time a Decision
// exists.
type Decision struct {
	Action Action
	// Rule is the ruleset entry that produced the verdict (zero for mode overrides).
	Rule Rule
	// Via names the mechanism that finalised the verdict: "rule", "session",
	// "prompt", "auto-approve", "non-interactive", or "dangerously-allow-all".
	Via string
	// Reason is a human-readable explanation, also returned to the model on deny.
	Reason string
}

// Allowed reports whether the tool may execute.
func (d Decision) Allowed() bool { return d.Action == Allow }

// Response is a human's answer to a prompt.
type Response int

const (
	// ResponseDeny refuses this call.
	ResponseDeny Response = iota
	// ResponseAllowOnce permits this call only.
	ResponseAllowOnce
	// ResponseAllowSession permits this call and every later call of the same tool.
	ResponseAllowSession
)

// Prompter asks a human to adjudicate an `ask`. Implementations MUST return
// promptly; the Guard passes the caller's context so a cancelled run does not
// wait on a human. A nil Prompter means "non-interactive" and `ask` becomes deny.
type Prompter interface {
	Ask(ctx context.Context, req Request) (Response, error)
}

// Pauser adjudicates an `ask` OUT OF PROCESS: it publishes a durable approval
// request that a human answers from another shell (`tag permissions approve
// <id>`), then resumes. This is PRD-078's pause/resume gate.
//
// Contract, and it is not negotiable:
//
//   - Pause MUST return within `timeout`. There is no "wait forever" mode. The
//     Guard refuses to install a Pauser without a positive timeout precisely so
//     a future wiring mistake cannot produce a run that blocks on a human who is
//     never coming.
//   - Pause MUST NOT return an allow response unless a human actually recorded
//     one. Expiry is a DENY, not a default-approve.
//   - Pause MUST honour ctx cancellation.
//
// A nil Pauser means the pause gate is off, which is the default everywhere.
type Pauser interface {
	Pause(ctx context.Context, req Request, timeout time.Duration) (Response, error)
}

// Screener is the CONTENT guardrail seam (PRD-123). The rule engine adjudicates
// a request's SUBJECT (a path, a command name); a Screener adjudicates its
// CONTENT — a credential in a write_file body, `rm -rf /` inside a command, a
// private key coming back in a tool result.
//
// It is expressed with primitive types on purpose: this package deliberately has
// no I/O or policy dependencies of its own, and the guardrail implementation
// (internal/guardrail) must stay swappable and independently testable.
//
// Both methods return (blocked, reason). A Screener that CANNOT evaluate must
// return blocked=true with a reason that says so — never (false, "").
type Screener interface {
	// ScreenToolInput runs before the tool executes. blocked=true refuses the call.
	ScreenToolInput(ctx context.Context, tool string, args map[string]any) (bool, string)
	// ScreenToolResult runs after the tool executes, before the model sees the
	// result. The side effect has already happened, so blocked=true means "withhold
	// this result and tell the model why" — it cannot un-run the tool.
	ScreenToolResult(ctx context.Context, tool string, result string) (bool, string)
}

// Recorder persists permission decisions for after-the-fact review. Errors are
// ignored by the Guard: auditing must never block or fail a security decision.
type Recorder interface {
	Record(req Request, d Decision)
}

// Policy is the resolved permission configuration for one process.
type Policy struct {
	// Rules is the ordered ruleset; the first match wins.
	Rules []Rule
	// AutoApprove upgrades an `ask` verdict to allow WITHOUT prompting. It is the
	// explicit, greppable automation opt-in (--auto-approve). It never overrides
	// an explicit `deny`.
	AutoApprove bool
	// DangerouslyAllowAll bypasses the gate entirely, including `deny` rules.
	DangerouslyAllowAll bool
}

// Guard applies a Policy to tool calls.
type Guard struct {
	Policy Policy
	// Prompter is consulted for `ask`. Nil = non-interactive (ask -> deny).
	Prompter Prompter
	// Pauser is the out-of-process approval gate for `ask` (PRD-078). Nil = off,
	// which is the default on every surface: it is installed ONLY when the
	// operator passes --approval-gate on that invocation. Background surfaces
	// (queue worker, dag run --execute, cron run --execute, loop) never pass it,
	// so they can never park on it. See PauseTimeout.
	Pauser Pauser
	// PauseTimeout bounds a Pauser wait. A Pauser with a non-positive timeout is
	// a wiring bug and is treated as a DENY rather than an unbounded wait.
	PauseTimeout time.Duration
	// Screener is the optional content guardrail (PRD-123). Nil = inert.
	Screener Screener
	// Recorder receives every decision (nil = no audit).
	Recorder Recorder
	// NonInteractiveHint explains, in the deny reason, WHY no human could be
	// asked (defaults to "no interactive terminal is available"). Callers that
	// suppressed prompting on purpose — `--no-prompt`, or the always-headless
	// queue/dag/cron worker — set their own so the message is not misleading.
	NonInteractiveHint string

	mu      sync.Mutex
	session map[string]bool // tool name -> allowed for the rest of this process
}

// NewGuard builds a Guard. A nil Prompter is the safe, headless configuration.
func NewGuard(p Policy, prompter Prompter, rec Recorder) *Guard {
	return &Guard{Policy: p, Prompter: prompter, Recorder: rec}
}

// Interactive reports whether an `ask` can reach a human, by either route: a
// terminal prompt or a durable out-of-process approval request.
func (g *Guard) Interactive() bool { return g != nil && (g.Prompter != nil || g.Pauser != nil) }

// Prompting reports whether an `ask` reaches a human on THIS terminal.
func (g *Guard) Prompting() bool { return g != nil && g.Prompter != nil }

// PauseGated reports whether the out-of-process approval gate is installed.
func (g *Guard) PauseGated() bool { return g != nil && g.Pauser != nil }

// resolve walks the ruleset and returns the first matching rule. The ruleset
// always ends in a catch-all, so a match is guaranteed for a well-formed Policy;
// if one is somehow absent the fail-closed default is Ask (which is deny when
// headless).
//
// One deliberate exception to "first match wins": a BLANKET allow (an allow rule
// with no pattern, e.g. `--allow-tool read_file`, `permissions.tools.read_file:
// allow`, or a `default: allow` catch-all) is skipped for a credential-shaped
// path. Typing `--allow-tool read_file` must not silently hand the model your
// .env or ~/.ssh/id_rsa. To cover such a path you have to name it — an allow
// rule WITH a pattern applies normally — or use --dangerously-allow-all.
// Deny and ask rules are never skipped, so an explicit refusal always wins.
func (g *Guard) resolve(req Request) (Rule, bool) {
	sensitive := IsCredentialPath(req)
	for _, r := range g.Policy.Rules {
		if sensitive && r.Action == Allow && r.Pattern == "" {
			continue
		}
		if r.matches(req) {
			return r, true
		}
	}
	return Rule{Tool: "*", Action: Ask, Source: "fallback"}, false
}

// IsCredentialPath reports whether a request targets a path the built-in
// credential rules protect (the `.env.example`-style carve-outs are NOT
// credential paths).
func IsCredentialPath(req Request) bool {
	if req.Kind != KindPath || req.Subject == "" {
		return false
	}
	for _, r := range CredentialRules() {
		if r.matches(req) {
			return r.Action == Deny
		}
	}
	return false
}

// Check adjudicates a tool call. It NEVER blocks without a Prompter and never
// returns Allow for an `ask` it could not put in front of a human unless
// AutoApprove was explicitly set.
func (g *Guard) Check(ctx context.Context, req Request) Decision {
	d := g.decide(ctx, req)
	if g != nil && g.Recorder != nil {
		g.Recorder.Record(req, d)
	}
	return d
}

func (g *Guard) decide(ctx context.Context, req Request) Decision {
	if g == nil {
		// A nil Guard is a programming error at a wiring site. Fail CLOSED rather
		// than silently ungating a tool.
		return Decision{Action: Deny, Via: "non-interactive",
			Reason: "no permission guard is configured for this tool registry (refusing to execute ungated)"}
	}
	if g.Policy.DangerouslyAllowAll {
		return Decision{Action: Allow, Via: "dangerously-allow-all",
			Reason: "--dangerously-allow-all bypassed the permission gate"}
	}

	// Content guardrail (PRD-123) runs BEFORE rule resolution, so a fired
	// tripwire outranks an `allow` rule. That ordering is deliberate: the rule
	// engine grants on the SUBJECT ("bash is allowed"), the guardrail refuses on
	// the CONTENT ("...but this particular command is `rm -rf /`"). A subject
	// grant must not be able to smuggle content the operator banned. Only
	// --dangerously-allow-all, which announces itself, is above it.
	if g.Screener != nil {
		if blocked, reason := g.Screener.ScreenToolInput(ctx, req.Tool, req.Args); blocked {
			if reason == "" {
				reason = "blocked by a content guardrail"
			}
			return Decision{Action: Deny, Via: ViaTripwire,
				Rule:   Rule{Tool: req.Tool, Action: Deny, Source: SourceTripwire},
				Reason: reason}
		}
	}

	rule, _ := g.resolve(req)
	switch rule.Action {
	case Allow:
		return Decision{Action: Allow, Rule: rule, Via: "rule",
			Reason: "allowed by rule " + rule.String()}
	case Deny:
		return Decision{Action: Deny, Rule: rule, Via: "rule",
			Reason: fmt.Sprintf("denied by rule %s", rule.String())}
	}

	// rule.Action == Ask from here.
	if g.sessionAllowed(req.Tool) {
		return Decision{Action: Allow, Rule: rule, Via: "session",
			Reason: fmt.Sprintf("approved for this session (tool %q)", req.Tool)}
	}
	if g.Policy.AutoApprove {
		return Decision{Action: Allow, Rule: rule, Via: "auto-approve",
			Reason: fmt.Sprintf("auto-approved by --auto-approve (rule %s)", rule.String())}
	}

	// Out-of-process approval (PRD-078). Checked before the TTY prompter because
	// installing it is an explicit per-invocation opt-in (--approval-gate) that
	// says "adjudicate this durably and audibly", which is a stronger statement
	// than "there happens to be a terminal attached".
	if g.Pauser != nil {
		if g.PauseTimeout <= 0 {
			// Fail CLOSED. An unbounded human gate is the silent-hang failure mode
			// this whole design exists to prevent, so a Pauser without a deadline is
			// refused rather than honoured.
			return Decision{Action: Deny, Rule: rule, Via: ViaPause,
				Reason: fmt.Sprintf("%s requires approval and an approval gate is installed, but it has no "+
					"bounded timeout; refusing to wait indefinitely (this is a wiring bug)", req.Describe())}
		}
		resp, err := g.Pauser.Pause(ctx, req, g.PauseTimeout)
		if err != nil {
			return Decision{Action: Deny, Rule: rule, Via: ViaPause,
				Reason: fmt.Sprintf("%s requires approval but the approval gate failed (%v); denying", req.Describe(), err)}
		}
		switch resp {
		case ResponseAllowOnce:
			return Decision{Action: Allow, Rule: rule, Via: ViaPause,
				Reason: fmt.Sprintf("approved out-of-band by a reviewer (%s)", req.Describe())}
		case ResponseAllowSession:
			g.grantSession(req.Tool)
			return Decision{Action: Allow, Rule: rule, Via: ViaPause,
				Reason: fmt.Sprintf("approved out-of-band for this session (tool %q)", req.Tool)}
		default:
			return Decision{Action: Deny, Rule: rule, Via: ViaPause,
				Reason: fmt.Sprintf("denied at the approval gate (%s)", req.Describe())}
		}
	}

	if g.Prompter == nil {
		// THE non-interactive contract: return immediately, deny, and say why.
		hint := g.NonInteractiveHint
		if hint == "" {
			hint = "no interactive terminal is available"
		}
		return Decision{Action: Deny, Rule: rule, Via: "non-interactive",
			Reason: fmt.Sprintf("%s requires approval (rule %s) but %s; "+
				"re-run with --auto-approve to allow automatically, --allow-tool %s to allow this tool, "+
				"or set permissions in config.yaml", req.Describe(), rule.String(), hint, req.Tool)}
	}
	resp, err := g.Prompter.Ask(ctx, req)
	if err != nil {
		return Decision{Action: Deny, Rule: rule, Via: "prompt",
			Reason: fmt.Sprintf("%s requires approval but the prompt failed (%v); denying", req.Describe(), err)}
	}
	switch resp {
	case ResponseAllowOnce:
		return Decision{Action: Allow, Rule: rule, Via: "prompt",
			Reason: fmt.Sprintf("approved once by the user (%s)", req.Describe())}
	case ResponseAllowSession:
		g.grantSession(req.Tool)
		return Decision{Action: Allow, Rule: rule, Via: "prompt",
			Reason: fmt.Sprintf("approved for this session by the user (tool %q)", req.Tool)}
	default:
		return Decision{Action: Deny, Rule: rule, Via: "prompt",
			Reason: fmt.Sprintf("denied by the user (%s)", req.Describe())}
	}
}

func (g *Guard) sessionAllowed(tool string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.session[tool]
}

func (g *Guard) grantSession(tool string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.session == nil {
		g.session = map[string]bool{}
	}
	g.session[tool] = true
}

// DeniedError is the error a gated tool returns when a call is refused. It is
// surfaced verbatim to the model as the tool result, so the model learns the
// call was blocked (rather than seeing a fake success) and the loop continues.
type DeniedError struct {
	Request  Request
	Decision Decision
}

func (e *DeniedError) Error() string { return "permission denied: " + e.Decision.Reason }

// SpecificityLess orders flag-supplied rules most-specific-first: a rule naming
// a concrete tool outranks a "*" rule, and a rule with a pattern outranks a
// bare tool rule. Ties break deny > ask > allow (safe-first), then by pattern
// length so a longer, more literal glob wins. This is what makes
// `--deny-tool bash --allow-tool 'bash:git *'` behave the way it reads: the
// patterned allow carves an exception out of the blanket deny.
func SpecificityLess(a, b Rule) bool {
	sa, sb := specScore(a), specScore(b)
	if sa != sb {
		return sa > sb
	}
	ra, rb := actionRank(a.Action), actionRank(b.Action)
	if ra != rb {
		return ra < rb
	}
	if len(a.Pattern) != len(b.Pattern) {
		return len(a.Pattern) > len(b.Pattern)
	}
	if a.Tool != b.Tool {
		return a.Tool < b.Tool
	}
	return a.Pattern < b.Pattern
}

func specScore(r Rule) int {
	n := 0
	if r.Tool != "*" && r.Tool != "" {
		n += 2
	}
	if r.Pattern != "" {
		n++
	}
	return n
}

func actionRank(a Action) int {
	switch a {
	case Deny:
		return 0
	case Ask:
		return 1
	default:
		return 2
	}
}

// SortBySpecificity stably sorts rules most-specific-first.
func SortBySpecificity(rules []Rule) {
	sort.SliceStable(rules, func(i, j int) bool { return SpecificityLess(rules[i], rules[j]) })
}
