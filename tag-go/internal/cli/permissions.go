package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

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
	// by daemons that inherit a terminal).
	noPrompt bool
}

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
// a TTY prompter only when stdin AND stderr are real terminals, and a SQLite
// audit recorder when the store is reachable.
//
// The interactivity check is the safety hinge: headless callers (queue worker,
// cron daemon, dag run --execute, gateway, CI) get a nil Prompter, so `ask`
// resolves to an immediate deny with a reason instead of blocking on a read.
func (p *permFlags) guard(app *App, profile, runID string, warnTo io.Writer) (*permission.Guard, error) {
	pol, err := p.policy(app, profile)
	if err != nil {
		return nil, err
	}
	var prompter permission.Prompter
	hint := "no interactive terminal is available"
	switch {
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
			fmt.Fprintln(warnTo, "  WARNING: --dangerously-allow-all is set — the tool permission gate is DISABLED for this run (including deny rules).")
		} else if pol.AutoApprove {
			fmt.Fprintln(warnTo, "  note: --auto-approve is set — every 'ask' is approved without prompting (deny rules still apply).")
		}
	}
	g := permission.NewGuard(pol, prompter, rec)
	g.NonInteractiveHint = hint
	return g, nil
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
			if flagJSON {
				rules := make([]map[string]any, 0, len(pol.Rules))
				for _, r := range pol.Rules {
					rules = append(rules, map[string]any{
						"tool": r.Tool, "kind": string(r.Kind), "pattern": r.Pattern,
						"action": string(r.Action), "source": r.Source,
					})
				}
				return emitJSON(map[string]any{
					"interactive": interactive, "auto_approve": pol.AutoApprove,
					"dangerously_allow_all": pol.DangerouslyAllowAll, "rules": rules,
				})
			}
			fmt.Printf("interactive: %v   auto_approve: %v   dangerously_allow_all: %v\n",
				interactive, pol.AutoApprove, pol.DangerouslyAllowAll)
			if !interactive && !pol.AutoApprove && !pol.DangerouslyAllowAll {
				fmt.Println("non-interactive: any 'ask' verdict resolves to DENY (no hang, no auto-approval)")
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
	root.AddCommand(c)
}
