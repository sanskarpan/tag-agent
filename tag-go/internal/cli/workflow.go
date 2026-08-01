package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/hitl"
)

// registerWorkflow wires PRD-109 — the human-in-the-loop interrupt/resume
// primitive:
//
//	tag workflow interrupt raise --session S --question Q [--context JSON] [--step ID]
//	tag workflow interrupt show  <interrupt-id>
//	tag workflow interrupt list  [--session S] [--pending]
//	tag workflow interrupt wait  <interrupt-id> [--timeout D]
//	tag workflow resume <interrupt-id> --input TEXT
//	tag workflow list [--filter interrupted|all]
//
// # How this differs from the two neighbouring gates
//
// There are three human gates in this tree and they are NOT interchangeable:
//
//	loop start --approval human  gates the NEXT ITERATION of an autonomous loop.
//	                             Answer: continue / abort. Owner: internal/loop.
//	permissions approve|deny     gates ONE TOOL CALL before it executes.
//	                             Answer: allow / deny. Owner: internal/permission.
//	workflow interrupt|resume    gates an ARBITRARY STEP and injects DATA.
//	                             Answer: free text. Owner: this file.
//
// The last two collapse onto one mechanism — internal/hitl — because both are
// "publish a pending row, wait out-of-process, resume with the answer"; they
// differ only in the payload. `tag permissions approve` is literally
// `workflow resume` specialised to a tool call with a binary answer, and both
// write the same hitl_pauses table and the same append-only hitl_audit trail.
// The loop's gate is deliberately NOT folded in: it shipped with its own
// loop_approvals table and iteration semantics, and rewriting a just-released
// contract to save one table is a bad trade. See the RETURN notes.
func registerWorkflow(root *cobra.Command, app *App) {
	w := &cobra.Command{
		Use:     "workflow",
		Short:   "Workflow sessions with human-in-the-loop interrupt/resume",
		GroupID: "orch",
	}

	interrupt := &cobra.Command{
		Use:   "interrupt",
		Short: "Raise, inspect and wait on human-in-the-loop interrupts",
	}

	var rSession, rQuestion, rContext, rStep string
	var rTimeout time.Duration
	raise := &cobra.Command{
		Use:   "raise",
		Short: "Record a pending interrupt: the session is paused until a human resumes it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(rSession) == "" {
				return jsonErrorMaybe(usageErrorf("--session is required"))
			}
			if strings.TrimSpace(rQuestion) == "" {
				return jsonErrorMaybe(usageErrorf("--question is required (an interrupt with no question cannot be answered)"))
			}
			var ctxMap map[string]any
			if s := strings.TrimSpace(rContext); s != "" {
				if err := json.Unmarshal([]byte(s), &ctxMap); err != nil {
					return jsonErrorMaybe(usageErrorf("--context must be a JSON object: %v", err))
				}
			}
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			gate := &hitl.Gate{DB: db.DB, SessionID: rSession}
			_, ierr := hitl.Interrupt(cmd.Context(), gate, hitl.InterruptRequest{
				StepID: rStep, Question: rQuestion, Context: ctxMap,
				TimeoutS: int(rTimeout / time.Second),
			})
			step := rStep
			if step == "" {
				step = hitl.StepIDFor(rQuestion, 0)
			}
			p, gerr := hitl.FindStep(db.DB, rSession, step)
			if gerr != nil {
				return jsonErrorMaybe(gerr)
			}
			switch {
			case errors.Is(ierr, hitl.ErrInterrupt):
				outJSON(p, fmt.Sprintf("interrupt raised: %s\n  session : %s\n  step    : %s\n  question: %s\n"+
					"  resume with: tag workflow resume %s --input \"...\"",
					p.ID, p.SessionID, p.StepID, p.Question, p.ID))
				return nil
			case ierr != nil:
				return jsonErrorMaybe(ierr)
			default:
				// Already answered: report the standing response rather than
				// pretending a fresh pause was created.
				outJSON(p, fmt.Sprintf("interrupt %s was already answered (%s): %q", p.ID, p.Status, p.Response))
				return nil
			}
		},
	}
	raise.Flags().StringVar(&rSession, "session", "", "session/run id this interrupt belongs to (required)")
	raise.Flags().StringVar(&rQuestion, "question", "", "the question put to the operator (required)")
	raise.Flags().StringVar(&rContext, "context", "", "JSON object of context shown with the question")
	raise.Flags().StringVar(&rStep, "step", "", "deterministic step id (default: derived from the question)")
	raise.Flags().DurationVar(&rTimeout, "timeout", 0, "advisory timeout recorded on the row")

	show := &cobra.Command{
		Use:   "show INTERRUPT_ID",
		Short: "Show one interrupt in full, including its JSON context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			p, err := hitl.Get(db.DB, args[0])
			if errors.Is(err, hitl.ErrNotFound) {
				return jsonErrorMaybe(fmt.Errorf("no interrupt with id %q", args[0]))
			}
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(p)
			}
			fmt.Printf("Interrupt: %s\n  session : %s\n  step    : %s\n  status  : %s\n  question: %s\n",
				p.ID, p.SessionID, p.StepID, p.Status, p.Question)
			fmt.Printf("  context : %s\n", prettyJSON(p.ContextJSON))
			fmt.Printf("  created : %s\n", p.CreatedAt)
			if hitl.Terminal(p.Status) {
				fmt.Printf("  decided : %s by %s\n", p.DecidedAt, strOr(p.Reviewer, "unknown"))
				fmt.Printf("  input   : %q\n", p.Response)
				if p.Rationale != "" {
					fmt.Printf("  reason  : %s\n", p.Rationale)
				}
			}
			return nil
		},
	}

	var lSession string
	var lPending bool
	var lLimit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List interrupts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			rows, err := hitl.List(db.DB, hitl.Filter{
				Kind: hitl.KindWorkflowInterrupt, SessionID: lSession,
				PendingOnly: lPending, Limit: lLimit,
			})
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(rows)
			}
			if len(rows) == 0 {
				fmt.Println("no interrupts")
				return nil
			}
			fmt.Printf("  %-24s %-18s %-12s %s\n", "INTERRUPT ID", "SESSION", "STATUS", "QUESTION")
			fmt.Println("  " + strings.Repeat("-", 88))
			for _, p := range rows {
				fmt.Printf("  %-24s %-18s %-12s %s\n", p.ID, truncate(p.SessionID, 18), p.Status, truncate(p.Question, 40))
			}
			return nil
		},
	}
	list.Flags().StringVar(&lSession, "session", "", "filter by session id")
	list.Flags().BoolVar(&lPending, "pending", false, "only unanswered interrupts")
	list.Flags().IntVar(&lLimit, "limit", 50, "max rows")

	var wTimeout, wPoll time.Duration
	var wExitZero bool
	wait := &cobra.Command{
		Use:   "wait INTERRUPT_ID",
		Short: "Block (bounded) until an interrupt is answered; exit 3 if it is not",
		Long: "Waits for an out-of-process `tag workflow resume`. The wait is ALWAYS bounded:\n" +
			"--timeout must be positive, and on expiry the interrupt is recorded as timed_out\n" +
			"and the command exits 3. There is no wait-forever mode.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if wTimeout <= 0 {
				return jsonErrorMaybe(usageErrorf("--timeout must be greater than 0 " +
					"(an unbounded wait would hang; there is no wait-forever mode)"))
			}
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			// A long wait must die cleanly on SIGINT/SIGTERM and record that it did,
			// the same construction `queue worker` uses.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			res, err := hitl.Wait(ctx, db.DB, args[0], wTimeout, wPoll)
			if errors.Is(err, hitl.ErrNotFound) {
				return jsonErrorMaybe(fmt.Errorf("no interrupt with id %q", args[0]))
			}
			if err != nil {
				return jsonErrorMaybe(err)
			}
			payload := map[string]any{
				"id": args[0], "status": res.Status, "input": res.Pause.Response,
				"reviewer": res.Pause.Reviewer, "elapsed_ms": res.Elapsed.Milliseconds(),
			}
			outJSON(payload, fmt.Sprintf("interrupt %s: %s after %s\n  input: %q",
				args[0], res.Status, res.Elapsed.Round(time.Millisecond), res.Pause.Response))
			if res.Status != hitl.StatusApproved && !wExitZero {
				// "Ran fine, the gate did not pass" is not a crash and not a usage
				// error, so it gets its own code (agentic-ci's exitFindings).
				return exitCodeErr{code: exitFindings}
			}
			return nil
		},
	}
	wait.Flags().DurationVar(&wTimeout, "timeout", 60*time.Second, "bounded wait for a decision (must be > 0)")
	wait.Flags().DurationVar(&wPoll, "poll", hitl.DefaultPoll, "store poll cadence")
	wait.Flags().BoolVar(&wExitZero, "exit-zero", false, "exit 0 even when the interrupt was not answered")

	interrupt.AddCommand(raise, show, list, wait)

	var resumeInput, resumeReason string
	var resumeDeny bool
	resume := &cobra.Command{
		Use:   "resume INTERRUPT_ID",
		Short: "Answer a pending interrupt with operator input so the session can continue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !resumeDeny && !cmd.Flags().Changed("input") {
				return jsonErrorMaybe(usageErrorf("--input TEXT is required (or --deny to refuse)"))
			}
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			var p hitl.Pause
			if resumeDeny {
				p, err = hitl.Resolve(db.DB, args[0], hitl.Decision{
					Status: hitl.StatusDenied, Source: "cli", Rationale: resumeReason,
				})
			} else {
				p, err = hitl.Respond(db.DB, args[0], resumeInput, resumeReason)
			}
			switch {
			case errors.Is(err, hitl.ErrNotFound):
				return jsonErrorMaybe(fmt.Errorf("no interrupt with id %q (see `tag workflow interrupt list --pending`)", args[0]))
			case errors.Is(err, hitl.ErrAlreadyDecided):
				return jsonErrorMaybe(fmt.Errorf("interrupt %s was already %s by %s at %s; "+
					"a recorded decision is never overwritten", p.ID, p.Status,
					strOr(p.Reviewer, "unknown"), p.DecidedAt))
			case err != nil:
				return jsonErrorMaybe(err)
			}
			outJSON(p, fmt.Sprintf("interrupt %s %s\n  input: %q\n  the session may now re-execute the step",
				p.ID, p.Status, p.Response))
			return nil
		},
	}
	resume.Flags().StringVar(&resumeInput, "input", "", "operator response injected at the interrupt point")
	resume.Flags().StringVar(&resumeReason, "rationale", "", "why (stored verbatim in the audit log)")
	resume.Flags().BoolVar(&resumeDeny, "deny", false, "refuse the interrupt instead of answering it")

	var wlFilter string
	var wlLimit int
	wlist := &cobra.Command{
		Use:   "list",
		Short: "List workflow sessions and their interrupt state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pendingOnly := false
			switch strings.ToLower(strings.TrimSpace(wlFilter)) {
			case "", "all":
			case "interrupted":
				pendingOnly = true
			default:
				return jsonErrorMaybe(usageErrorf("--filter must be 'interrupted' or 'all', got %q", wlFilter))
			}
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			rows, err := hitl.Sessions(db.DB, pendingOnly, wlLimit)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(rows)
			}
			if len(rows) == 0 {
				fmt.Println("no workflow sessions with interrupts")
				return nil
			}
			fmt.Printf("  %-20s %-13s %-8s %s\n", "SESSION", "STATUS", "PENDING", "QUESTION")
			fmt.Println("  " + strings.Repeat("-", 84))
			for _, s := range rows {
				fmt.Printf("  %-20s %-13s %-8d %s\n", truncate(s.SessionID, 20), s.Status, s.Pending, truncate(s.Question, 38))
			}
			return nil
		},
	}
	wlist.Flags().StringVar(&wlFilter, "filter", "all", "'interrupted' shows only sessions with a pending interrupt")
	wlist.Flags().IntVar(&wlLimit, "limit", 50, "max rows")

	w.AddCommand(interrupt, resume, wlist)
	root.AddCommand(w)
}

// prettyJSON re-indents a stored JSON blob for display, falling back to the raw
// text when it is not parseable (never swallowing content).
func prettyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}
