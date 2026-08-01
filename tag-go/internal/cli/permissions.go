package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/guardrail"
	"github.com/tag-agent/tag/internal/hitl"
	"github.com/tag-agent/tag/internal/permission"
)

// permFlags is the shared per-invocation permission surface. Every command that
// can build a tool registry binds it, so the gate is configurable everywhere and
// nothing is silently ungated.
type permFlags struct {
	allow       []string
	deny        []string
	ask         []string
	autoApprove bool
	dangerous   bool
	// noPrompt forces the non-interactive path even on a TTY (used by tests and
	// by daemons that inherit a terminal). It ALSO disables the out-of-process
	// approval gate — see guard().
	noPrompt bool
	// approvalGate turns on the PRD-078 pause/resume gate: an `ask` publishes a
	// durable approval request and waits (bounded) for `tag permissions
	// approve|deny`. Off by default on every surface.
	approvalGate bool
	// approvalTimeout bounds that wait. It must be positive; there is no
	// "wait forever" mode.
	approvalTimeout time.Duration
	// noTripwire disables the config-driven content guardrail for this run.
	noTripwire bool
}

// defaultApprovalTimeout is the bounded wait for --approval-gate. It matches
// internal/loop's human-approval default so the two HITL gates time out alike.
const defaultApprovalTimeout = 300 * time.Second

// bind attaches the permission flags to a command.
func (p *permFlags) bind(c *cobra.Command) {
	c.Flags().StringArrayVar(&p.allow, "allow-tool", nil,
		"permission: allow TOOL or TOOL:PATTERN (repeatable; e.g. bash:'git *', write_file:'*.md')")
	c.Flags().StringArrayVar(&p.deny, "deny-tool", nil,
		"permission: deny TOOL or TOOL:PATTERN (repeatable)")
	c.Flags().StringArrayVar(&p.ask, "ask-tool", nil,
		"permission: prompt for TOOL or TOOL:PATTERN (repeatable; denied when there is no TTY)")
	c.Flags().BoolVar(&p.autoApprove, "auto-approve", false,
		"permission: approve every 'ask' without prompting (explicit automation opt-in; does NOT override deny rules)")
	c.Flags().BoolVar(&p.dangerous, "dangerously-allow-all", false,
		"permission: DISABLE the consent gate entirely, including deny rules — unrestricted host access")
	c.Flags().BoolVar(&p.noPrompt, "no-prompt", false,
		"permission: never prompt; treat 'ask' as deny even on a terminal")
	c.Flags().BoolVar(&p.approvalGate, "approval-gate", false,
		"permission: park an 'ask' as a durable approval request answered by `tag permissions approve|deny` "+
			"(bounded by --approval-gate-timeout; no decision = deny)")
	// NOTE the name: `loop start` already binds --approval-timeout for the LOOP's
	// iteration gate, and both flag sets land on the same command. The names must
	// stay distinct — and the longer one reads correctly anyway, because these
	// really are two different gates (see internal/cli/workflow.go).
	c.Flags().DurationVar(&p.approvalTimeout, "approval-gate-timeout", defaultApprovalTimeout,
		"permission: how long --approval-gate waits for a decision before denying (must be > 0)")
	c.Flags().BoolVar(&p.noTripwire, "no-tripwire", false,
		"permission: disable the configured content guardrail (tripwire) for this run")
}

// rules converts the flags into flag-source rules.
func (p *permFlags) rules() ([]permission.Rule, error) {
	var out []permission.Rule
	for _, spec := range p.deny {
		r, err := permission.ParseSpec(spec, permission.Deny)
		if err != nil {
			return nil, usageErr{err}
		}
		out = append(out, r)
	}
	for _, spec := range p.ask {
		r, err := permission.ParseSpec(spec, permission.Ask)
		if err != nil {
			return nil, usageErr{err}
		}
		out = append(out, r)
	}
	for _, spec := range p.allow {
		r, err := permission.ParseSpec(spec, permission.Allow)
		if err != nil {
			return nil, usageErr{err}
		}
		out = append(out, r)
	}
	return out, nil
}

// policy resolves flags + config into a Policy.
func (p *permFlags) policy(app *App, profile string) (permission.Policy, error) {
	flagRules, err := p.rules()
	if err != nil {
		return permission.Policy{}, err
	}
	src := permission.Sources{Flags: flagRules}
	autoApprove := p.autoApprove

	if app != nil && app.Cfg != nil {
		rootBlock, _ := app.Cfg.Data["permissions"].(map[string]any)
		rootLayer, rootAuto, err := permission.ParseLayer(rootBlock, "config")
		if err != nil {
			return permission.Policy{}, usageErr{err}
		}
		src.Root = rootLayer
		autoApprove = autoApprove || rootAuto

		prof := app.profile(profile)
		if profCfg, ok := app.Cfg.Data["profiles"].(map[string]any); ok {
			if pm, ok := profCfg[prof].(map[string]any); ok {
				if cfgm, ok := pm["config"].(map[string]any); ok {
					block, _ := cfgm["permissions"].(map[string]any)
					layer, profAuto, err := permission.ParseLayer(block, "config:profile:"+prof)
					if err != nil {
						return permission.Policy{}, usageErr{err}
					}
					src.Profile = layer
					autoApprove = autoApprove || profAuto
				}
			}
		}
	}
	return permission.Policy{
		Rules:               permission.Resolve(src),
		AutoApprove:         autoApprove,
		DangerouslyAllowAll: p.dangerous,
	}, nil
}

// guard builds the consent gate for this invocation: policy from flags+config,
// a TTY prompter only when stdin AND stderr are real terminals, the optional
// PRD-078 out-of-process approval gate, the optional PRD-123 content guardrail,
// and a SQLite audit recorder when the store is reachable.
//
// The interactivity check is the safety hinge: headless callers (queue worker,
// cron daemon, dag run --execute, gateway, CI) get a nil Prompter, so `ask`
// resolves to an immediate deny with a reason instead of blocking on a read.
//
// # Why the pause gate cannot hang a background run
//
// Two independent guarantees, either of which alone would be sufficient:
//
//  1. STRUCTURAL. The Pauser is installed only when the operator passed
//     --approval-gate AND this caller did not force non-interactivity. Every
//     background surface forces it (`pf.noPrompt = true` in buildWorkerOptions,
//     loopRunFlags.runnerOptions, aciRunner, …), and they all funnel through
//     THIS function, so there is no path by which a detached worker acquires a
//     pause gate — even if the operator typed --approval-gate on its command
//     line. When that happens the flag is refused out loud, not ignored.
//  2. TEMPORAL. Even where it IS installed, the wait is bounded by
//     --approval-gate-timeout (default 300s, must be > 0) and expiry is an audited
//     auto-DENY. permission.Guard independently refuses a Pauser that arrives
//     without a positive timeout.
func (p *permFlags) guard(app *App, profile, runID string, warnTo io.Writer) (*permission.Guard, error) {
	pol, err := p.policy(app, profile)
	if err != nil {
		return nil, err
	}
	if p.approvalGate && p.approvalTimeout <= 0 {
		return nil, usageErrorf("--approval-gate-timeout must be greater than 0 " +
			"(an unbounded approval gate would hang the run; there is no wait-forever mode)")
	}

	var prompter permission.Prompter
	var pauser permission.Pauser
	hint := "no interactive terminal is available"

	// The out-of-process gate is considered first: asking for it is an explicit
	// per-invocation decision, unlike "a terminal happens to be attached".
	gateOn := p.approvalGate && !pol.AutoApprove && !pol.DangerouslyAllowAll
	if p.approvalGate && p.noPrompt {
		gateOn = false
		if warnTo != nil {
			fmt.Fprintln(warnTo, "  WARNING: --approval-gate is IGNORED here — this command runs non-interactively "+
				"(background worker or --no-prompt), and parking it on a human approval would strand the run. "+
				"'ask' verdicts resolve to deny; grant with --allow-tool/--auto-approve or a permissions block.")
		}
	}
	switch {
	case gateOn:
		db, derr := app.OpenDB()
		if derr != nil || db == nil {
			// Fail closed: without a store there is nowhere to publish the request,
			// and pretending the gate is on would be a lie.
			return nil, fmt.Errorf("--approval-gate needs the TAG store to publish approval requests: %w", derr)
		}
		var notify io.Writer = warnTo
		if notify == nil {
			notify = os.Stderr
		}
		pauser = &hitl.ToolPauser{DB: db.DB, SessionID: runID, Notify: notify}
		hint = "approval requests are answered with `tag permissions approve|deny`"
	case p.noPrompt:
		hint = "prompting is disabled for this command (--no-prompt, or a headless worker)"
	case pol.AutoApprove || pol.DangerouslyAllowAll:
		// nothing can reach `ask` anyway
	case permission.StdioInteractive():
		prompter = permission.NewTTYPrompter(os.Stdin, os.Stderr)
	}

	var rec permission.Recorder
	if app != nil {
		if db, derr := app.OpenDB(); derr == nil && db != nil {
			if r := permission.NewSQLRecorder(db.DB, runID); r != nil {
				rec = r
			}
		}
	}
	if warnTo != nil {
		if pol.DangerouslyAllowAll {
			fmt.Fprintln(warnTo, "  WARNING: --dangerously-allow-all is set — the tool permission gate is DISABLED for this run (including deny rules, the approval gate and the content guardrail).")
		} else if pol.AutoApprove {
			fmt.Fprintln(warnTo, "  note: --auto-approve is set — every 'ask' is approved without prompting (deny rules still apply).")
		}
	}

	g := permission.NewGuard(pol, prompter, rec)
	g.NonInteractiveHint = hint
	if pauser != nil {
		g.Pauser = pauser
		g.PauseTimeout = p.approvalTimeout
	}

	// Content guardrail. Unlike the approval gate this is safe everywhere — it
	// never waits on a human — so it applies to background surfaces too.
	proc, perr := p.processor(app, profile, runID, warnTo)
	if perr != nil {
		return nil, perr
	}
	if proc.Enabled() {
		g.Screener = proc
	}
	return g, nil
}

// processor builds the content guardrail from config (root `tripwire:` block,
// overridden by the profile's). A malformed block is a HARD error: an operator
// who wrote a guardrail and got a silently-empty one is worse off than one who
// got a startup failure.
func (p *permFlags) processor(app *App, profile, runID string, logTo io.Writer) (*guardrail.Processor, error) {
	if p == nil || p.noTripwire || app == nil || app.Cfg == nil {
		return nil, nil
	}
	rules, err := tripwireRules(app, profile)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, nil
	}
	proc := &guardrail.Processor{Rules: rules, SessionID: runID, Log: logTo}
	if db, derr := app.OpenDB(); derr == nil && db != nil {
		proc.DB = db.DB
	}
	return proc, nil
}

// tripwireRules resolves the guardrail ruleset: the profile's `tripwire:` block
// REPLACES the root one when present (a profile that declares a policy owns it),
// otherwise the root block applies.
func tripwireRules(app *App, profile string) ([]guardrail.Rule, error) {
	if app == nil || app.Cfg == nil {
		return nil, nil
	}
	prof := app.profile(profile)
	if profCfg, ok := app.Cfg.Data["profiles"].(map[string]any); ok {
		if pm, ok := profCfg[prof].(map[string]any); ok {
			if cfgm, ok := pm["config"].(map[string]any); ok {
				if block, ok := cfgm["tripwire"].(map[string]any); ok {
					rules, err := guardrail.ParseLayer(block, "config:profile:"+prof)
					if err != nil {
						return nil, usageErr{err}
					}
					return rules, nil
				}
			}
		}
	}
	block, _ := app.Cfg.Data["tripwire"].(map[string]any)
	rules, err := guardrail.ParseLayer(block, "config")
	if err != nil {
		return nil, usageErr{err}
	}
	return rules, nil
}

// registerPermissions wires `tag permissions` — inspect the resolved ruleset and
// the audit trail. Read-only; it makes the gate auditable rather than magic.
func registerPermissions(root *cobra.Command, app *App) {
	c := &cobra.Command{
		Use:     "permissions",
		Short:   "Inspect the tool-permission policy and decision log",
		GroupID: "tools",
	}

	var pf permFlags
	var profile string
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the resolved permission ruleset (most specific first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pol, err := pf.policy(app, profile)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			interactive := !pf.noPrompt && permission.StdioInteractive()
			gateOn := pf.approvalGate && !pf.noPrompt && !pol.AutoApprove && !pol.DangerouslyAllowAll
			tw, terr := tripwireRules(app, profile)
			if terr != nil {
				return jsonErrorMaybe(terr)
			}
			if flagJSON {
				rules := make([]map[string]any, 0, len(pol.Rules))
				for _, r := range pol.Rules {
					rules = append(rules, map[string]any{
						"tool": r.Tool, "kind": string(r.Kind), "pattern": r.Pattern,
						"action": string(r.Action), "source": r.Source,
					})
				}
				grules := make([]map[string]any, 0, len(tw))
				for _, r := range tw {
					grules = append(grules, tripwireRuleJSON(r))
				}
				return emitJSON(map[string]any{
					"interactive": interactive, "auto_approve": pol.AutoApprove,
					"dangerously_allow_all": pol.DangerouslyAllowAll,
					"approval_gate":         gateOn,
					"approval_timeout":      pf.approvalTimeout.String(),
					"rules":                 rules, "tripwire_rules": grules,
				})
			}
			fmt.Printf("interactive: %v   auto_approve: %v   dangerously_allow_all: %v   approval_gate: %v\n",
				interactive, pol.AutoApprove, pol.DangerouslyAllowAll, gateOn)
			switch {
			case gateOn:
				fmt.Printf("approval gate: an 'ask' parks as a durable request answered by `tag permissions approve|deny`; "+
					"no decision within %s = DENY\n", pf.approvalTimeout)
			case !interactive && !pol.AutoApprove && !pol.DangerouslyAllowAll:
				fmt.Println("non-interactive: any 'ask' verdict resolves to DENY (no hang, no auto-approval)")
			}
			if len(tw) > 0 {
				fmt.Printf("content guardrail (tripwire): %d rule(s) — see `tag tripwire list`\n", len(tw))
			}
			fmt.Printf("resolved ruleset (%d rules, first match wins):\n", len(pol.Rules))
			for i, r := range pol.Rules {
				fmt.Printf("  %2d. %s\n", i+1, r.String())
			}
			return nil
		},
	}
	show.Flags().StringVar(&profile, "profile", "", "profile whose permissions block to resolve")
	pf.bind(show)

	var limit int
	logCmd := &cobra.Command{
		Use:   "log",
		Short: "Show recent permission decisions (what was approved or blocked)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if limit <= 0 {
				limit = 20
			}
			rows, err := db.Query(`SELECT created_at, tool, subject, verdict, via, rule, reason
				FROM permission_decisions ORDER BY id DESC LIMIT ?`, limit)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var at, tool, subject, verdict, via, rule, reason string
				if err := rows.Scan(&at, &tool, &subject, &verdict, &via, &rule, &reason); err != nil {
					return jsonErrorMaybe(err)
				}
				out = append(out, map[string]any{"created_at": at, "tool": tool, "subject": subject,
					"verdict": verdict, "via": via, "rule": rule, "reason": reason})
			}
			if err := rows.Err(); err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("no permission decisions recorded yet")
				return nil
			}
			for _, e := range out {
				fmt.Printf("%s  %-8s %-10s %s  [%s]\n", e["created_at"], e["verdict"], e["via"], e["tool"], e["rule"])
				if s, _ := e["subject"].(string); s != "" {
					fmt.Printf("    %s\n", s)
				}
			}
			return nil
		},
	}
	logCmd.Flags().IntVar(&limit, "limit", 20, "how many decisions to show")

	c.AddCommand(show, logCmd)
	c.AddCommand(approvalCommands(app)...)
	root.AddCommand(c)
}

// approvalCommands builds the PRD-078 pause/resume surface.
//
// PRD-078 §7 specifies `tag mcp approve …`. There is no `tag mcp` namespace in
// the Go tree (only mcp-connect / mcp-registry / mcp-serve), and the gate covers
// the BUILT-IN tools as well as MCP-bridged ones, so the verbs live under
// `tag permissions` next to the ruleset and the decision log they belong to.
// That is a documented divergence, not an omission.
func approvalCommands(app *App) []*cobra.Command {
	var sessionID, toolName string
	var limit int
	pending := &cobra.Command{
		Use:   "pending",
		Short: "List tool calls parked at the approval gate, awaiting a human decision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			list, err := hitl.List(db.DB, hitl.Filter{
				Kind: hitl.KindToolApproval, SessionID: sessionID, Tool: toolName,
				PendingOnly: true, Limit: limit,
			})
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				// Never nil: an empty result renders as [], not null.
				return emitJSON(list)
			}
			if len(list) == 0 {
				fmt.Println("no tool calls are awaiting approval")
				return nil
			}
			fmt.Printf("  %-24s %-12s %-20s %s\n", "APPROVAL ID", "TOOL", "CREATED", "SUBJECT")
			fmt.Println("  " + strings.Repeat("-", 88))
			for _, p := range list {
				fmt.Printf("  %-24s %-12s %-20s %s\n", p.ID, p.Tool, p.CreatedAt, truncate(p.Subject, 34))
				fmt.Printf("      args: %s\n", truncate(p.ArgsJSON, 200))
				fmt.Printf("      sha256: %s\n", p.ArgsSHA256)
			}
			return nil
		},
	}
	pending.Flags().StringVar(&sessionID, "session", "", "filter by run/session id")
	pending.Flags().StringVar(&toolName, "tool", "", "filter by tool name")
	pending.Flags().IntVar(&limit, "limit", 50, "max rows")

	decide := func(name, short string, approve bool) *cobra.Command {
		var rationale string
		c := &cobra.Command{
			Use:   name + " APPROVAL_ID",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				db, err := app.OpenDB()
				if err != nil {
					return jsonErrorMaybe(err)
				}
				status := hitl.StatusDenied
				if approve {
					status = hitl.StatusApproved
				}
				p, err := hitl.Resolve(db.DB, args[0], hitl.Decision{
					Status: status, Source: "cli", Rationale: rationale,
				})
				switch {
				case errors.Is(err, hitl.ErrNotFound):
					return jsonErrorMaybe(fmt.Errorf("no approval request with id %q "+
						"(see `tag permissions pending`)", args[0]))
				case errors.Is(err, hitl.ErrAlreadyDecided):
					// Honest and specific: re-deciding is refused, and the caller is
					// told what the standing decision actually is.
					return jsonErrorMaybe(fmt.Errorf("approval %s was already %s by %s at %s; "+
						"a recorded decision is never overwritten", p.ID, p.Status,
						strOr(p.Reviewer, "unknown"), p.DecidedAt))
				case err != nil:
					return jsonErrorMaybe(err)
				}
				outJSON(p, fmt.Sprintf("%s: %s\n  tool     : %s\n  subject  : %s\n  reviewer : %s\n  rationale: %s",
					strings.ToUpper(p.Status), p.ID, p.Tool, p.Subject, p.Reviewer,
					strOr(p.Rationale, "(none)")))
				return nil
			},
		}
		c.Flags().StringVar(&rationale, "rationale", "", "why this decision was made (stored verbatim in the audit log)")
		return c
	}

	var auditLimit int
	audit := &cobra.Command{
		Use:   "audit",
		Short: "Show the append-only approval audit trail (approve/deny/timeout/cancel)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			rows, err := hitl.AuditLog(db.DB, auditLimit)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(rows)
			}
			if len(rows) == 0 {
				fmt.Println("no approval decisions recorded yet")
				return nil
			}
			for _, a := range rows {
				fmt.Printf("%s  %-10s %-14s %-12s %s\n", a.DecidedAt, a.Decision, a.Reviewer, a.Tool, a.PauseID)
				if a.Subject != "" {
					fmt.Printf("    %s\n", a.Subject)
				}
				if a.Rationale != "" {
					fmt.Printf("    rationale: %s\n", a.Rationale)
				}
			}
			return nil
		},
	}
	audit.Flags().IntVar(&auditLimit, "limit", 50, "how many decisions to show")

	return []*cobra.Command{
		pending,
		decide("approve", "Approve a parked tool call so the waiting run proceeds", true),
		decide("deny", "Deny a parked tool call so the waiting run is refused", false),
		audit,
	}
}

// tripwireRuleJSON renders a guardrail rule for --json output.
func tripwireRuleJSON(r guardrail.Rule) map[string]any {
	m := map[string]any{
		"name": r.Name, "type": string(r.Type), "tool": r.Tool,
		"action": string(r.Action), "source": r.Source,
	}
	if r.Stage != "" {
		m["stage"] = string(r.Stage)
	}
	if r.Builtin != "" {
		m["builtin"] = r.Builtin
	}
	if r.Pattern != "" {
		m["pattern"] = r.Pattern
	}
	if r.Threshold > 0 {
		m["threshold"] = r.Threshold
	}
	if r.Window > 0 {
		m["window"] = r.Window.String()
	}
	if r.Message != "" {
		m["message"] = r.Message
	}
	return m
}
