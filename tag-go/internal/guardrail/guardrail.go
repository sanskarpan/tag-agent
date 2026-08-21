// Package guardrail implements the runtime CONTENT guardrail processor and
// tripwire (PRD-123).
//
// It is a different thing from internal/permission, and the difference matters:
//
//	permission answers "is this SUBJECT (a path, a command name) allowed?"
//	guardrail  answers "does this CONTENT violate policy?"
//
// A permission rule cannot see that a `write_file` body contains an AWS key or
// that a tool result leaked a private key; a guardrail cannot express "deny
// ~/.ssh/**" any better than the rule engine already does. So the guardrail runs
// as a content SCREEN attached to the existing consent gate rather than as a
// second, parallel gate: internal/permission.Guard holds an optional Screener,
// and because the gate already lives inside tool.Register / RegisterMCP, no
// caller can register a tool that is screened for permissions but not for
// content.
//
// # Fail-open vs fail-closed
//
// Stated per check, because "a guardrail that fails open silently is the worst
// outcome here":
//
//	load-time bad regex / bad config -> HARD ERROR, refuse to start.
//	  A half-loaded policy is worse than a policy that refuses to load, and it is
//	  the same doctrine permission.ParseLayer already applies.
//	args not serialisable for inspection -> FAIL CLOSED (block).
//	  Content we cannot read is indistinguishable from content that violates.
//	tripwire counter store unreachable -> FAIL CLOSED (block).
//	  An operator who configured a threshold wants it enforced; "we could not
//	  count, so we allowed" is exactly the silent fail-open this bars.
//	guardrail_events write failure -> FAIL OPEN (best effort) + a loud warning.
//	  Telemetry must never make the security decision, matching
//	  permission.SQLRecorder. It is never SILENT: the warning goes to stderr.
//	no rules configured -> inert. Zero queries, zero regex, nil Screener.
//
// # Command surface
//
// PRDs 121/122/124/125 are still Proposed and claim `tag guardrail input|output|
// result` and `tag constitutional`. To avoid squatting on their namespace this
// PRD's surface is `tag tripwire` (check / list / test / history), which also
// names the thing it actually delivers. `tag guardrail runtime …` from PRD-123
// §6 can be added later as an alias over the same Processor.
package guardrail

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Stage is the point in the run at which content is screened.
type Stage string

const (
	// StageToolInput screens tool arguments BEFORE the tool executes.
	StageToolInput Stage = "tool_input"
	// StageToolOutput screens a tool's result BEFORE the model sees it.
	StageToolOutput Stage = "tool_output"
	// StageModelOutput screens model-generated text.
	StageModelOutput Stage = "model_output"
	// StageUserInput screens operator-supplied text.
	StageUserInput Stage = "user_input"
)

// Stages lists every valid stage, for validation and help text.
func Stages() []Stage {
	return []Stage{StageToolInput, StageToolOutput, StageModelOutput, StageUserInput}
}

// ParseStage validates a stage word.
func ParseStage(s string) (Stage, error) {
	v := Stage(strings.ToLower(strings.TrimSpace(s)))
	for _, k := range Stages() {
		if k == v {
			return v, nil
		}
	}
	return "", fmt.Errorf("invalid guardrail stage %q (want one of %v)", s, Stages())
}

// Action is what a fired rule does.
type Action string

const (
	// ActionBlock halts: the tool call is refused / the content is rejected.
	ActionBlock Action = "block"
	// ActionWarn records and reports but does not halt.
	ActionWarn Action = "warn"
	// ActionInterrupt pauses the run for human approval (PRD-123 FR-03/FR-07)
	// instead of halting. The tool is NOT dispatched until an operator approves;
	// on approval it executes normally. block outranks interrupt outranks warn.
	ActionInterrupt Action = "interrupt"
)

// ParseAction validates an action word.
func ParseAction(s string) (Action, error) {
	switch Action(strings.ToLower(strings.TrimSpace(s))) {
	case ActionBlock:
		return ActionBlock, nil
	case ActionWarn:
		return ActionWarn, nil
	case ActionInterrupt:
		return ActionInterrupt, nil
	case "":
		return ActionBlock, nil
	}
	return "", fmt.Errorf("invalid guardrail action %q (want block, warn, or interrupt)", s)
}

// Type selects a rule's evaluation strategy.
type Type string

const (
	// TypePattern matches a regex (or a built-in detector) against the content.
	TypePattern Type = "pattern"
	// TypeTripwire counts matching events per session and fires at a threshold.
	TypeTripwire Type = "tripwire"
	// TypeRequireApproval fires on EVERY invocation of a matched tool, regardless
	// of content — the "this tool always needs a human" rule (PRD-123 US2). It
	// carries no pattern/builtin; its default action is interrupt.
	TypeRequireApproval Type = "require-approval"
)

// Rule is one configured guardrail.
type Rule struct {
	Name string `json:"name"`
	Type Type   `json:"type"`
	// Stage restricts the rule to one screening point. Empty = every stage.
	Stage Stage `json:"stage,omitempty"`
	// Tool is a glob over tool names ("*", "http_*"). Empty = "*".
	Tool string `json:"tool"`
	// Builtin selects a shipped detector ("secrets", "destructive"). Mutually
	// exclusive with Pattern.
	Builtin string `json:"builtin,omitempty"`
	// Pattern is a Go regexp, matched case-insensitively unless it sets its own flags.
	Pattern string `json:"pattern,omitempty"`
	// Threshold / Window configure a tripwire.
	Threshold int           `json:"threshold,omitempty"`
	Window    time.Duration `json:"window,omitempty"`
	Action    Action        `json:"action"`
	Message   string        `json:"message,omitempty"`
	Source    string        `json:"source"`

	re      *regexp.Regexp
	toolPat string
}

// Finding is one guardrail hit.
type Finding struct {
	Rule      string `json:"rule"`
	Type      string `json:"type"`
	Stage     string `json:"stage"`
	Tool      string `json:"tool,omitempty"`
	Detector  string `json:"detector"`
	Action    string `json:"action"`
	Excerpt   string `json:"excerpt"`
	Offset    int    `json:"offset"`
	Count     int    `json:"count,omitempty"`
	Threshold int    `json:"threshold,omitempty"`
	Message   string `json:"message"`
}

// Verdict is the result of screening one piece of content.
type Verdict struct {
	Stage    string    `json:"stage"`
	Tool     string    `json:"tool,omitempty"`
	Findings []Finding `json:"findings"`
	// Blocked means a block-action rule fired (the tripwire "halt").
	Blocked bool `json:"blocked"`
	// Interrupted means an interrupt-action rule fired: the run should pause for
	// human approval rather than halt. It is subordinate to Blocked — if a block
	// rule ALSO fired for the same content, Blocked wins and the call is refused
	// outright, never paused (PRD-123 Security §10 row 3). Consumers must check
	// Blocked before Interrupted; Outcome() encodes that precedence.
	Interrupted bool `json:"interrupted,omitempty"`
	// Warned means at least one warn-action rule fired.
	Warned bool `json:"warned"`
	// Reason is the human-readable summary; on Blocked it is what the model is
	// told, so it must be honest about WHY.
	Reason string `json:"reason,omitempty"`
	// Undecidable is set when the processor could not evaluate. It is always
	// accompanied by Blocked (fail closed) and a Reason saying so — a guardrail
	// that cannot evaluate must say so rather than wave content through.
	Undecidable bool `json:"undecidable,omitempty"`
}

// Fired reports whether anything at all triggered.
func (v Verdict) Fired() bool { return v.Blocked || v.Interrupted || v.Warned }

// Outcome collapses the verdict to its single effective action, applying the
// precedence block > interrupt > warn > pass. It is the one place consumers
// should ask "what do I do with this call?" so the precedence is never
// re-implemented (and mis-ordered) at a call site.
func (v Verdict) Outcome() Action {
	switch {
	case v.Blocked:
		return ActionBlock
	case v.Interrupted:
		return ActionInterrupt
	case v.Warned:
		return ActionWarn
	default:
		return ""
	}
}

// Processor evaluates rules against content. A nil *Processor is inert (that is
// the "no rules configured" case, NFR-06), which is why every method is
// nil-receiver-safe.
type Processor struct {
	// Rules is the compiled, ordered ruleset.
	Rules []Rule
	// DB backs tripwire counters and the guardrail_events log. Nil disables
	// persistence: pattern rules still work, tripwire rules fail closed.
	DB *sql.DB
	// SessionID scopes tripwire counters (the run id).
	SessionID string
	// Log receives warn-action notices and audit-write failures. Nil discards
	// them — but the block path never depends on it.
	Log io.Writer
	// QuietWarn suppresses the warn banner on Log. Set by callers that render
	// the findings themselves (`tag tripwire check`), so a warning is not
	// printed twice; audit-write failures are still reported.
	QuietWarn bool
}

// Enabled reports whether the processor has anything to do.
func (p *Processor) Enabled() bool { return p != nil && len(p.Rules) > 0 }

// HasStage reports whether any rule applies at a stage (lets hot paths skip work).
func (p *Processor) HasStage(s Stage) bool {
	if !p.Enabled() {
		return false
	}
	for _, r := range p.Rules {
		if r.Stage == "" || r.Stage == s {
			return true
		}
	}
	return false
}

// ScanArgs screens tool arguments. Serialisation failure is FAIL CLOSED: the
// call is blocked with an honest "could not inspect" reason rather than allowed.
func (p *Processor) ScanArgs(ctx context.Context, tool string, args map[string]any) Verdict {
	if !p.HasStage(StageToolInput) {
		return Verdict{Stage: string(StageToolInput), Tool: tool, Findings: []Finding{}}
	}
	b, err := json.Marshal(args)
	if err != nil {
		v := Verdict{
			Stage: string(StageToolInput), Tool: tool, Findings: []Finding{},
			Blocked: true, Undecidable: true,
			Reason: fmt.Sprintf("guardrail could not serialise the arguments of tool %q for inspection (%v); "+
				"blocking rather than allowing uninspected content", tool, err),
		}
		p.record(v)
		return v
	}
	return p.Scan(ctx, StageToolInput, tool, string(b))
}

// Scan screens a string of content at a stage.
func (p *Processor) Scan(ctx context.Context, stage Stage, tool string, content string) Verdict {
	v := Verdict{Stage: string(stage), Tool: tool, Findings: []Finding{}}
	if !p.Enabled() {
		return v
	}
	if tool == "" {
		tool = "-"
	}
	for _, r := range p.Rules {
		if r.Stage != "" && r.Stage != stage {
			continue
		}
		if !matchTool(r.toolPat, tool) {
			continue
		}
		if r.Type == TypeRequireApproval {
			// Invocation-based, not content-based: the matched tool always needs a
			// human. No pattern is evaluated.
			v.Findings = append(v.Findings, Finding{
				Rule: r.Name, Type: string(r.Type), Stage: string(stage), Tool: tool,
				Detector: "require-approval", Action: string(r.Action),
				Message: r.describe(fmt.Sprintf("tool %q requires approval before it runs", r.Tool)),
			})
			p.apply(&v, r)
			continue
		}
		hits := r.evaluate(content)
		if r.Type == TypeTripwire {
			// A tripwire with a pattern counts matches; a tripwire without one
			// counts invocations. Either way the counter, not the content, decides.
			inc := 1
			if r.Pattern != "" || r.Builtin != "" {
				inc = len(hits)
				if inc == 0 {
					continue
				}
			}
			count, cerr := p.bump(r, inc)
			if cerr != nil {
				v.Blocked = true
				v.Undecidable = true
				v.Reason = fmt.Sprintf("tripwire %q could not read its counter (%v); "+
					"blocking rather than silently not enforcing the threshold", r.Name, cerr)
				p.record(v)
				return v
			}
			if count < r.Threshold {
				continue
			}
			v.Findings = append(v.Findings, Finding{
				Rule: r.Name, Type: string(r.Type), Stage: string(stage), Tool: tool,
				Detector: "tripwire", Action: string(r.Action),
				Count: count, Threshold: r.Threshold,
				Message: r.describe(fmt.Sprintf("tripwire threshold reached: %d/%d matching events for %q within %s",
					count, r.Threshold, r.Tool, r.Window)),
			})
			p.apply(&v, r)
			continue
		}
		for _, h := range hits {
			v.Findings = append(v.Findings, Finding{
				Rule: r.Name, Type: string(r.Type), Stage: string(stage), Tool: tool,
				Detector: h.detector, Action: string(r.Action),
				Excerpt: h.excerpt, Offset: h.offset,
				Message: r.describe(h.detector + " matched"),
			})
			p.apply(&v, r)
		}
	}
	if v.Blocked && v.Reason == "" {
		v.Reason = summarise(v.Findings, ActionBlock)
	}
	if v.Interrupted && v.Reason == "" {
		v.Reason = summarise(v.Findings, ActionInterrupt)
	}
	if v.Warned && v.Reason == "" {
		v.Reason = summarise(v.Findings, ActionWarn)
	}
	if v.Fired() {
		p.record(v)
	}
	if v.Warned && !v.Blocked && p.Log != nil && !p.QuietWarn {
		fmt.Fprintf(p.Log, "  guardrail WARNING (%s): %s\n", stage, v.Reason)
	}
	return v
}

func (p *Processor) apply(v *Verdict, r Rule) {
	// Additive: several rules may fire on one piece of content. The flags are all
	// truthful; precedence (block > interrupt > warn) is resolved at the point of
	// consumption via Verdict.Outcome(), never by clearing a flag here.
	switch r.Action {
	case ActionBlock:
		v.Blocked = true
	case ActionInterrupt:
		v.Interrupted = true
	default:
		v.Warned = true
	}
}

func summarise(fs []Finding, want Action) string {
	var names []string
	for _, f := range fs {
		if f.Action == string(want) {
			names = append(names, fmt.Sprintf("%s (%s)", f.Rule, f.Message))
		}
	}
	if len(names) == 0 {
		return ""
	}
	verb := "blocked"
	switch want {
	case ActionWarn:
		verb = "warned"
	case ActionInterrupt:
		verb = "requires approval"
	}
	return fmt.Sprintf("guardrail %s: %s", verb, strings.Join(names, "; "))
}

type hit struct {
	detector string
	excerpt  string
	offset   int
}

// evaluate runs a rule's detector over content and returns redacted hits.
func (r Rule) evaluate(content string) []hit {
	if r.Builtin != "" {
		return builtinScan(r.Builtin, content)
	}
	if r.re == nil {
		return nil
	}
	locs := r.re.FindAllStringIndex(content, 8)
	out := make([]hit, 0, len(locs))
	for _, l := range locs {
		out = append(out, hit{
			detector: "pattern:" + r.Name,
			excerpt:  Redact(content[l[0]:l[1]]),
			offset:   l[0],
		})
	}
	return out
}

func (r Rule) describe(def string) string {
	if strings.TrimSpace(r.Message) != "" {
		return r.Message
	}
	return def
}

// matchTool applies a tool-name glob. Tool names contain no "/", so stdlib
// path.Match is sufficient and avoids taking a new dependency (PRD-123 NFR-04
// specifies gobwas/glob; the tree has no such dependency and does not need one).
func matchTool(pattern, tool string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	ok, err := path.Match(pattern, tool)
	// A malformed glob is rejected at load time; if one somehow reaches here it
	// matches nothing rather than everything.
	return err == nil && ok
}

// Redact renders a match safely for logs and terminals. A guardrail that prints
// the secret it just caught into an audit table has not helped anyone, so only a
// short prefix and the length survive.
func Redact(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	const keep = 6
	if len(s) <= keep {
		return strings.Repeat("*", len(s))
	}
	return s[:keep] + fmt.Sprintf("…[redacted, %d chars]", len(s))
}

// ---- persistence -----------------------------------------------------------

const schema = `
CREATE TABLE IF NOT EXISTS guardrail_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at  TEXT NOT NULL,
  session_id  TEXT NOT NULL DEFAULT '',
  direction   TEXT NOT NULL DEFAULT 'runtime',
  stage       TEXT NOT NULL,
  tool        TEXT NOT NULL DEFAULT '',
  rule        TEXT NOT NULL DEFAULT '',
  action      TEXT NOT NULL DEFAULT '',
  blocked     INTEGER NOT NULL DEFAULT 0,
  undecidable INTEGER NOT NULL DEFAULT 0,
  detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_guardrail_events_at ON guardrail_events(created_at DESC);

CREATE TABLE IF NOT EXISTS tripwire_counters (
  session_id   TEXT NOT NULL,
  rule         TEXT NOT NULL,
  count        INTEGER NOT NULL DEFAULT 0,
  window_start TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  PRIMARY KEY(session_id, rule)
);
`

// EnsureSchema creates the guardrail tables if absent (idempotent, applied
// lazily from this package for the same reason internal/loop does it).
func EnsureSchema(db *sql.DB) error {
	if db == nil {
		return errors.New("guardrail: no database handle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, schema)
	return err
}

func nowTS() string { return time.Now().UTC().Format(time.RFC3339) }

// bump increments a tripwire counter within its window and returns the new
// count. An error here is the caller's cue to FAIL CLOSED.
func (p *Processor) bump(r Rule, by int) (int, error) {
	if p.DB == nil {
		return 0, errors.New("no store is configured for tripwire counters")
	}
	if err := EnsureSchema(p.DB); err != nil {
		return 0, err
	}
	session := p.SessionID
	if session == "" {
		session = "-"
	}
	tx, err := p.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	var windowStart string
	err = tx.QueryRow(`SELECT count, window_start FROM tripwire_counters
		WHERE session_id=? AND rule=?`, session, r.Name).Scan(&count, &windowStart)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		count, windowStart = 0, nowTS()
	case err != nil:
		return 0, err
	default:
		if r.Window > 0 {
			if t, perr := time.Parse(time.RFC3339, windowStart); perr == nil {
				if time.Since(t) > r.Window {
					count, windowStart = 0, nowTS()
				}
			} else {
				// An unparseable window start means we cannot prove the counter is
				// current. Restart the window rather than trusting a bad value.
				count, windowStart = 0, nowTS()
			}
		}
	}
	count += by
	if _, err := tx.Exec(`INSERT INTO tripwire_counters(session_id, rule, count, window_start, updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(session_id, rule) DO UPDATE SET count=excluded.count,
		  window_start=excluded.window_start, updated_at=excluded.updated_at`,
		session, r.Name, count, windowStart, nowTS()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// record appends guardrail_events rows. Best-effort by design (telemetry must
// not decide), but NEVER silent: a failure is reported on Log.
func (p *Processor) record(v Verdict) {
	if p == nil || p.DB == nil {
		return
	}
	if err := EnsureSchema(p.DB); err != nil {
		p.warnf("guardrail: could not prepare the event log (%v); the decision above still stands", err)
		return
	}
	rows := v.Findings
	if len(rows) == 0 {
		rows = []Finding{{Rule: "-", Stage: v.Stage, Tool: v.Tool, Action: "block", Message: v.Reason}}
	}
	for _, f := range rows {
		blocked := 0
		if v.Blocked {
			blocked = 1
		}
		und := 0
		if v.Undecidable {
			und = 1
		}
		if _, err := p.DB.Exec(`INSERT INTO guardrail_events
			(created_at, session_id, direction, stage, tool, rule, action, blocked, undecidable, detail)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			nowTS(), p.SessionID, "runtime", v.Stage, v.Tool, f.Rule, f.Action,
			blocked, und, strings.TrimSpace(f.Message+" "+f.Excerpt)); err != nil {
			p.warnf("guardrail: could not append to guardrail_events (%v); the decision above still stands", err)
			return
		}
	}
}

func (p *Processor) warnf(format string, a ...any) {
	if p == nil || p.Log == nil {
		return
	}
	fmt.Fprintf(p.Log, "  "+format+"\n", a...)
}

// Event is one recorded guardrail decision.
type Event struct {
	ID          int64  `json:"id"`
	CreatedAt   string `json:"created_at"`
	SessionID   string `json:"session_id,omitempty"`
	Direction   string `json:"direction"`
	Stage       string `json:"stage"`
	Tool        string `json:"tool,omitempty"`
	Rule        string `json:"rule"`
	Action      string `json:"action"`
	Blocked     bool   `json:"blocked"`
	Undecidable bool   `json:"undecidable"`
	Detail      string `json:"detail,omitempty"`
}

// History returns recent guardrail events, newest first. Never nil.
func History(db *sql.DB, limit int) ([]Event, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`SELECT id, created_at, session_id, direction, stage, tool, rule,
		action, blocked, undecidable, detail FROM guardrail_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var blocked, und int
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.SessionID, &e.Direction, &e.Stage, &e.Tool,
			&e.Rule, &e.Action, &blocked, &und, &e.Detail); err != nil {
			return nil, err
		}
		e.Blocked, e.Undecidable = blocked == 1, und == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- permission.Screener adapter -------------------------------------------
//
// These two methods make *Processor satisfy permission.Screener without either
// package importing the other's types (the interface is expressed in
// primitives). That keeps internal/permission dependency-free, as its package
// doc promises, and keeps the guardrail implementation swappable.

// ScreenToolInput implements the pre-hook. A nil/inert Processor never blocks.
func (p *Processor) ScreenToolInput(ctx context.Context, tool string, args map[string]any) (bool, string) {
	if !p.HasStage(StageToolInput) {
		return false, ""
	}
	v := p.ScanArgs(ctx, tool, args)
	return v.Blocked, v.Reason
}

// ScreenToolResult implements the post-hook.
func (p *Processor) ScreenToolResult(ctx context.Context, tool string, result string) (bool, string) {
	if !p.HasStage(StageToolOutput) {
		return false, ""
	}
	v := p.Scan(ctx, StageToolOutput, tool, result)
	return v.Blocked, v.Reason
}

// SortRules orders rules deterministically for display (block before warn, then
// by name), so `tag tripwire list` output is stable.
func SortRules(rs []Rule) {
	sort.SliceStable(rs, func(i, j int) bool {
		if (rs[i].Action == ActionBlock) != (rs[j].Action == ActionBlock) {
			return rs[i].Action == ActionBlock
		}
		return rs[i].Name < rs[j].Name
	})
}
