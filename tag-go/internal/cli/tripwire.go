package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/guardrail"
)

// registerTripwire wires PRD-123 — the runtime CONTENT guardrail processor and
// its halting tripwire:
//
//	tag tripwire list                       resolved ruleset
//	tag tripwire check --text/--file/-       screen content at a stage
//	tag tripwire test --tool T --args JSON   dry-run the tool-input hooks (FR-10)
//	tag tripwire history                     recorded guardrail decisions
//
// # Why `tag tripwire` and not `tag guardrail runtime`
//
// PRD-123 §6 asks for `tag guardrail runtime …`, but PRDs 121, 122, 124 and 125
// are all still **Proposed** and collectively claim `tag guardrail input`,
// `tag guardrail output`, `tag guardrail result` and `tag constitutional`.
// Planting a subcommand in a namespace four unbuilt PRDs are going to reshape
// guarantees a rename later. `tag tripwire` is unclaimed, names the thing this
// PRD actually delivers (the halting check), and leaves `tag guardrail` free —
// when 121/122 land they can add `guardrail runtime` as a thin alias over the
// same guardrail.Processor, which is why the processor takes a Stage rather
// than hard-coding the tool path.
//
// # Exit codes
//
//	0  clean (or --exit-zero)
//	1  runtime failure
//	2  usage error
//	3  TRIPWIRE FIRED — ran fine, found problems (agentic-ci's exitFindings)
//
// 3 is separate from 1 on purpose: a CI gate has to distinguish "the guardrail
// caught something" from "the guardrail crashed", and folding them together is
// how a broken scanner starts reading as a clean run.
func registerTripwire(root *cobra.Command, app *App) {
	c := &cobra.Command{
		Use:     "tripwire",
		Short:   "Content guardrails: screen model output and tool I/O, halt on policy violations",
		GroupID: "tools",
	}
	c.AddCommand(tripwireSubcommands(app)...)
	root.AddCommand(c)

	// PRD-123 §6 names this surface `tag guardrail runtime`. It is a thin alias
	// over the SAME guardrail.Processor and the SAME subcommand builders (no
	// behavioural divergence). It can safely coexist now that the still-Proposed
	// PRDs 121/122/124/125 will add sibling `guardrail input|output|result`
	// verbs; `tag tripwire` stays the canonical spelling.
	registerGuardrail(root, app)
}

// tripwireSubcommands builds a fresh set of the four read-only / dry-run
// guardrail subcommands (list, check, test, history). It is called once per
// parent — `tripwire` and `guardrail runtime` — so the two surfaces share
// behaviour without sharing mutable flag state.
func tripwireSubcommands(app *App) []*cobra.Command {
	var profile string
	list := &cobra.Command{
		Use:   "list",
		Short: "Print the resolved guardrail ruleset",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rules, err := tripwireRules(app, profile)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			guardrail.SortRules(rules)
			if flagJSON {
				out := make([]map[string]any, 0, len(rules))
				for _, r := range rules {
					out = append(out, tripwireRuleJSON(r))
				}
				return emitJSON(out)
			}
			if len(rules) == 0 {
				fmt.Println("no guardrail rules configured")
				fmt.Printf("  add a `tripwire:` block to tag.yaml — the quickest start is `tripwire: {preset: standard}`\n")
				fmt.Printf("  presets: %v   builtin detectors: destructive, secrets\n", guardrail.PresetNames())
				return nil
			}
			fmt.Printf("%d guardrail rule(s):\n", len(rules))
			for i, r := range rules {
				det := r.Builtin
				if det == "" {
					det = "/" + r.Pattern + "/"
				}
				fmt.Printf("  %2d. %-28s %-9s stage=%-13s tool=%-10s %s\n", i+1, r.Name,
					string(r.Action), strOr(string(r.Stage), "any"), r.Tool, det)
				if r.Type == guardrail.TypeTripwire {
					fmt.Printf("      tripwire: threshold=%d window=%s\n", r.Threshold, r.Window)
				}
				fmt.Printf("      [%s]\n", r.Source)
			}
			return nil
		},
	}
	list.Flags().StringVar(&profile, "profile", "", "profile whose tripwire block to resolve")

	var stage, text, file, session string
	var stdin, exitZero bool
	check := &cobra.Command{
		Use:   "check",
		Short: "Screen content against the guardrail ruleset (exit 3 if the tripwire fires)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := guardrail.ParseStage(stage)
			if err != nil {
				return jsonErrorMaybe(usageErr{err})
			}
			content, err := readContent(cmd.InOrStdin(), text, file, stdin)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			proc, err := buildProcessor(app, profile, session, cmd.ErrOrStderr())
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if !proc.Enabled() {
				// Honest refusal: reporting "clean" from an empty ruleset is a
				// fabricated pass, and a CI job wiring this up would never notice.
				return jsonErrorMaybe(usageErrorf("no guardrail rules are configured, so nothing was checked " +
					"— add a `tripwire:` block to tag.yaml (see `tag tripwire list`) rather than treating this as a pass"))
			}
			v := proc.Scan(cmd.Context(), st, "", content)
			return emitVerdict(v, exitZero)
		},
	}
	check.Flags().StringVar(&stage, "stage", string(guardrail.StageModelOutput),
		fmt.Sprintf("which screening point to evaluate %v", guardrail.Stages()))
	check.Flags().StringVar(&text, "text", "", "content to screen")
	check.Flags().StringVar(&file, "file", "", "read the content from a file")
	check.Flags().BoolVar(&stdin, "stdin", false, "read the content from stdin")
	check.Flags().StringVar(&session, "session", "", "session id for tripwire counters")
	check.Flags().StringVar(&profile, "profile", "", "profile whose tripwire block to resolve")
	check.Flags().BoolVar(&exitZero, "exit-zero", false, "exit 0 even when the tripwire fires (advisory use)")

	var toolName, argsJSON string
	test := &cobra.Command{
		Use:   "test",
		Short: "Dry-run the tool-input guardrails against a mock tool call (no dispatch, no side effects)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(toolName) == "" {
				return jsonErrorMaybe(usageErrorf("--tool is required"))
			}
			toolArgs := map[string]any{}
			if s := strings.TrimSpace(argsJSON); s != "" {
				if err := json.Unmarshal([]byte(s), &toolArgs); err != nil {
					return jsonErrorMaybe(usageErrorf("--args must be a JSON object: %v", err))
				}
			}
			proc, err := buildProcessor(app, profile, session, cmd.ErrOrStderr())
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if !proc.Enabled() {
				return jsonErrorMaybe(usageErrorf("no guardrail rules are configured, so nothing was checked " +
					"— add a `tripwire:` block to tag.yaml (see `tag tripwire list`)"))
			}
			v := proc.ScanArgs(cmd.Context(), toolName, toolArgs)
			return emitVerdict(v, exitZero)
		},
	}
	test.Flags().StringVar(&toolName, "tool", "", "tool name to simulate (required)")
	test.Flags().StringVar(&argsJSON, "args", "{}", "tool arguments as a JSON object")
	test.Flags().StringVar(&session, "session", "", "session id for tripwire counters")
	test.Flags().StringVar(&profile, "profile", "", "profile whose tripwire block to resolve")
	test.Flags().BoolVar(&exitZero, "exit-zero", false, "exit 0 even when the tripwire fires")

	var histLimit int
	history := &cobra.Command{
		Use:   "history",
		Short: "Show recorded guardrail decisions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			rows, err := guardrail.History(db.DB, histLimit)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(rows)
			}
			if len(rows) == 0 {
				fmt.Println("no guardrail events recorded yet")
				return nil
			}
			for _, e := range rows {
				verdict := e.Action
				if e.Blocked {
					verdict = "BLOCK"
				}
				if e.Undecidable {
					verdict = "UNDECIDABLE"
				}
				fmt.Printf("%s  %-12s %-14s %-24s %s\n", e.CreatedAt, verdict, e.Stage, e.Rule, e.Tool)
				if e.Detail != "" {
					fmt.Printf("    %s\n", e.Detail)
				}
			}
			return nil
		},
	}
	history.Flags().IntVar(&histLimit, "limit", 50, "max rows")

	return []*cobra.Command{list, check, test, history}
}

// registerGuardrail mounts the PRD-123 §6 `tag guardrail runtime` surface as an
// alias over the tripwire engine, and adds the config-editing `add`/`remove`
// verbs that only make sense under this namespace.
func registerGuardrail(root *cobra.Command, app *App) {
	g := &cobra.Command{
		Use:     "guardrail",
		Short:   "Runtime guardrails (PRD-123): the same engine as `tag tripwire`, under the guardrail namespace",
		GroupID: "tools",
	}
	runtime := &cobra.Command{
		Use:   "runtime",
		Short: "Runtime content guardrails: list, check, test, inspect, and edit the ruleset",
	}
	runtime.AddCommand(tripwireSubcommands(app)...)
	runtime.AddCommand(guardrailRuntimeEditCommands(app)...)
	g.AddCommand(runtime)
	// PRD-122 input + PRD-121 output content guardrails.
	g.AddCommand(guardrailContentCommand(app, "input"))
	g.AddCommand(guardrailContentCommand(app, "output"))
	root.AddCommand(g)
}

// buildProcessor resolves the guardrail ruleset for a CLI invocation.
func buildProcessor(app *App, profile, session string, logTo io.Writer) (*guardrail.Processor, error) {
	rules, err := tripwireRules(app, profile)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, nil
	}
	// QuietWarn: these commands print every finding themselves, so the
	// processor's own warn banner would duplicate it. Audit-write failures still
	// reach Log.
	p := &guardrail.Processor{Rules: rules, SessionID: session, Log: logTo, QuietWarn: true}
	if db, derr := app.OpenDB(); derr == nil && db != nil {
		p.DB = db.DB
	}
	return p, nil
}

// readContent resolves exactly one content source.
func readContent(in io.Reader, text, file string, useStdin bool) (string, error) {
	n := 0
	if text != "" {
		n++
	}
	if file != "" {
		n++
	}
	if useStdin {
		n++
	}
	switch {
	case n == 0:
		return "", usageErrorf("supply the content with --text, --file or --stdin")
	case n > 1:
		return "", usageErrorf("--text, --file and --stdin are mutually exclusive")
	}
	if text != "" {
		return text, nil
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", usageErrorf("cannot read --file %s: %v", file, err)
		}
		return string(b), nil
	}
	b, err := io.ReadAll(in)
	if err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	return string(b), nil
}

// emitVerdict renders a guardrail verdict and maps it to an exit code. A fired
// tripwire is reported as a RESULT (exit 3), never as an error, so it stays
// distinguishable from a crash — including under --json, where `blocked` and
// `undecidable` are explicit fields rather than an inference from the exit code.
func emitVerdict(v guardrail.Verdict, exitZero bool) error {
	if flagJSON {
		if err := emitJSON(v); err != nil {
			return err
		}
	} else {
		switch {
		case v.Undecidable:
			fmt.Printf("UNDECIDABLE (%s): %s\n", v.Stage, v.Reason)
			fmt.Println("  the guardrail could not evaluate this content and therefore BLOCKED it (fail-closed)")
		case v.Blocked:
			fmt.Printf("TRIPWIRE FIRED (%s): %s\n", v.Stage, v.Reason)
		case v.Interrupted:
			// An interrupt rule fired: the run would pause for human approval.
			// This must NOT read as "clean" — the exit code is 3.
			fmt.Printf("APPROVAL REQUIRED (%s): %s\n", v.Stage, v.Reason)
		case v.Warned:
			fmt.Printf("WARN (%s): %s\n", v.Stage, v.Reason)
		default:
			fmt.Printf("clean (%s): no guardrail rule matched\n", v.Stage)
		}
		for _, f := range v.Findings {
			fmt.Printf("  - %-24s %-8s %s\n", f.Rule, f.Action, f.Message)
			if f.Excerpt != "" {
				fmt.Printf("      at offset %d: %s\n", f.Offset, f.Excerpt)
			}
			if f.Threshold > 0 {
				fmt.Printf("      count %d of %d\n", f.Count, f.Threshold)
			}
		}
	}
	if v.Fired() && !exitZero {
		return exitCodeErr{code: exitFindings}
	}
	return nil
}
