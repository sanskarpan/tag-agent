package loop

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/paths"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/tool"
	"github.com/tag-agent/tag/internal/trace"
)

// goalMarker is the sentinel the model emits to declare success
// (loop_agent._is_goal_achieved).
const goalMarker = "GOAL_ACHIEVED"

// Defaults for the approval gate. Python polls every 2s for 300s; the Go poll is
// tighter because the checkpoint lives in the local SQLite store, not a file the
// worker re-reads over a filesystem.
const (
	DefaultApprovalTimeout = 300 * time.Second
	DefaultApprovalPoll    = 250 * time.Millisecond
	DefaultAbortPoll       = 250 * time.Millisecond
)

// Options configures Runner.Run.
type Options struct {
	// Provider is the LLM provider each iteration runs against. Nil defaults to
	// the offline echo provider, so a loop is always exercisable without keys.
	Provider llm.Provider
	// Model is the model id passed to the agent loop.
	Model string
	// System is an optional system prompt.
	System string
	// MaxSteps caps agent-loop turns per iteration (0 = loop default).
	MaxSteps int
	// WithTools enables the built-in tools (bash/read_file/write_file/list_dir).
	WithTools bool
	// Guard is the TOOL consent gate. It is a different mechanism from the loop's
	// own --approval human gate: the guard decides whether a tool CALL may run,
	// the approval gate decides whether the next ITERATION may run.
	//
	// A loop is a long-running autonomous driver, so its guard must never carry a
	// TTY prompter (see cli.buildLoopGuard, which forces --no-prompt exactly as
	// buildWorkerOptions does for background drains). Nil is fail-closed:
	// tool.Register substitutes permission's secure default policy.
	Guard *permission.Guard
	// WorkRoot is the parent for per-iteration working directories. Empty
	// defaults to <TAG_HOME>/work.
	WorkRoot string
	// ApprovalTimeout bounds a human-approval wait (0 = DefaultApprovalTimeout).
	// A timeout is treated as a DENY, matching loop_agent._request_approval.
	ApprovalTimeout time.Duration
	// ApprovalPoll / AbortPoll are the store poll cadences.
	ApprovalPoll time.Duration
	AbortPoll    time.Duration
	// Log receives human-readable progress. Nil discards it.
	Log io.Writer
}

// Outcome is what a completed Run reports back to the CLI.
type Outcome struct {
	LoopID     string `json:"loop_id"`
	Status     string `json:"status"`
	Iterations int    `json:"iterations"`
	MaxIters   int    `json:"max_iters"`
	FinalText  string `json:"final_text"`
	Provider   string `json:"provider"`
	Degraded   bool   `json:"degraded"`
	Note       string `json:"note,omitempty"`
}

// echoNote is the offline-honesty disclaimer. A run against the echo provider
// produces no real reasoning, so it must never be presented as a real result.
const echoNote = "provider 'echo' is an OFFLINE stub: iterations echo the prompt back and no real reasoning happened. " +
	"The lifecycle (journal, abort, approval gates) is real; the agent output is not."

func (o Options) logf(format string, a ...any) {
	if o.Log == nil {
		return
	}
	fmt.Fprintf(o.Log, format+"\n", a...)
}

// Run drives loop `id` to a terminal status. It is the Go equivalent of
// src/tag/loop_agent.py:main, with the subprocess-per-iteration replaced by an
// in-process agent.Loop run.
//
// It returns the terminal Outcome. An error is returned only for infrastructure
// failures (unreadable store, unknown loop); a loop that FAILS reports
// Status=="failed" and a nil error, and the CLI maps that to exit 1.
func Run(ctx context.Context, db *sql.DB, id string, opts Options) (Outcome, error) {
	if err := EnsureSchema(db); err != nil {
		return Outcome{}, err
	}
	if opts.Provider == nil {
		opts.Provider = llm.EchoProvider{}
	}
	if opts.ApprovalTimeout <= 0 {
		opts.ApprovalTimeout = DefaultApprovalTimeout
	}
	if opts.ApprovalPoll <= 0 {
		opts.ApprovalPoll = DefaultApprovalPoll
	}
	if opts.AbortPoll <= 0 {
		opts.AbortPoll = DefaultAbortPoll
	}

	run, err := Get(db, id)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{
		LoopID:   id,
		Status:   run.Status,
		MaxIters: run.MaxIters,
		Provider: opts.Provider.Name(),
	}
	if out.Provider == "echo" {
		out.Degraded = true
		out.Note = echoNote
	}
	if IsTerminal(run.Status) {
		// Nothing to do; do NOT clobber a terminal status back to 'running'
		// (loop_agent B020).
		return out, nil
	}

	// One trace per loop; the loop id IS the trace id, so `tag trace show <loop-id>`
	// resolves the whole run. Wiring mirrors internal/worker.runJob and
	// internal/solver.Solve. Persisting spans is best-effort: telemetry must never
	// decide a loop's outcome.
	rec := trace.NewRecorder(id, run.Profile)
	defer func() { _ = rec.Save(db) }()

	// Watch the store for an out-of-process abort and cancel the in-flight
	// iteration when one lands. Python could only notice an abort BETWEEN
	// iterations (its subprocess ran up to 600s uninterrupted); cancelling the
	// context makes `tag loop abort` stop a LIVE iteration.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		t := time.NewTicker(opts.AbortPoll)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				st, err := Status(db, id)
				if err == nil && IsTerminal(st) {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { <-watchDone }()

	previous := ""
	completed := 0

	for iter := 1; iter <= run.MaxIters; iter++ {
		// Honor an abort that landed before this pass started.
		if st, serr := Status(db, id); serr == nil && IsTerminal(st) {
			out.Status = st
			out.Iterations = completed
			return out, nil
		}
		if ctx.Err() != nil {
			return signalStop(db, id, out, completed), nil
		}

		input := run.Goal
		if iter > 1 {
			input = truncate(previous, 500)
		}
		prompt := buildPrompt(run.Goal, iter, previous)

		iterSpan := rec.Start("loop.iteration", trace.KindAgent, "", opts.Model)
		rec.Attr(iterSpan, "tag.loop_id", id)
		rec.Attr(iterSpan, "tag.iteration", iter)
		rec.Attr(iterSpan, "tag.max_iters", run.MaxIters)

		text, runErr := runIteration(runCtx, rec, iterSpan, id, iter, prompt, run.Profile, opts)

		if runErr != nil {
			rec.End(iterSpan, trace.StatusError, runErr.Error(), 0, 0)
		} else {
			rec.End(iterSpan, trace.StatusOK, "", 0, 0)
		}
		// Persist spans incrementally so an aborted/killed loop still leaves a
		// truthful trace of the iterations it did complete.
		_ = rec.Save(db)

		output := text
		if runErr != nil {
			// Mirror _run_iteration, which folds the failure into the output text
			// rather than losing it.
			output = strings.TrimSpace(output + "\nIteration error: " + runErr.Error())
		}

		// An abort (or SIGTERM) may have landed while the iteration ran. Journal
		// the pass, but do not overwrite the terminal status (B020).
		if st, serr := Status(db, id); serr == nil && IsTerminal(st) {
			_ = markIteration(db, id, iter, input, output, DecisionAborted)
			out.Status = st
			out.Iterations = iter
			out.FinalText = text
			return out, nil
		}
		if ctx.Err() != nil {
			_ = markIteration(db, id, iter, input, output, DecisionAborted)
			return signalStop(db, id, out, iter), nil
		}

		decision := DecisionContinue
		if goalAchieved(prompt, output) {
			decision = DecisionGoalAchieved
		}
		if runErr != nil && strings.TrimSpace(text) == "" {
			decision = DecisionError
		}
		if err := markIteration(db, id, iter, input, output, decision); err != nil {
			return out, err
		}
		completed = iter
		out.Iterations = iter
		out.FinalText = text
		opts.logf("[iteration %d/%d] %s: %s", iter, run.MaxIters, decision, truncate(oneLine(output), 200))

		switch decision {
		case DecisionGoalAchieved:
			if err := finishStatus(db, id, StatusCompleted); err != nil {
				return out, err
			}
			out.Status = StatusCompleted
			return out, nil
		case DecisionError:
			if err := finishStatus(db, id, StatusFailed); err != nil {
				return out, err
			}
			out.Status = StatusFailed
			return out, nil
		}

		// Human-in-the-loop gate. Python skips the gate on the LAST iteration
		// (there is nothing left to approve); keep that.
		if run.Approval == ApprovalHuman && iter < run.MaxIters {
			opts.logf("awaiting approval for iteration %d — run `tag loop approve %s` or `tag loop deny %s` (timeout %s)",
				iter, id, id, opts.ApprovalTimeout)
			approved, reason, aerr := waitForApproval(runCtx, db, id, iter, output, opts)
			if aerr != nil {
				return out, aerr
			}
			if !approved {
				if ctx.Err() != nil {
					return signalStop(db, id, out, iter), nil
				}
				st, serr := Status(db, id)
				if serr != nil || !IsTerminal(st) {
					if err := finishStatus(db, id, StatusAborted); err != nil {
						return out, err
					}
					st = StatusAborted
				}
				out.Status = st
				out.Note = strings.TrimSpace(out.Note + " " + reason)
				opts.logf("loop stopped at iteration %d: %s", iter, reason)
				return out, nil
			}
			// Back to running for the next pass.
			if err := setStatus(context.Background(), db, id, StatusRunning); err != nil {
				return out, err
			}
		}

		previous = output
	}

	if err := finishStatus(db, id, StatusMaxIters); err != nil {
		return out, err
	}
	out.Status = StatusMaxIters
	return out, nil
}

// signalStop records the terminal status for a loop whose CONTEXT was cancelled
// (SIGINT/SIGTERM). This is the #574 guard: without it the process dies and the
// row is stranded in 'running' forever, so `loop list` lies indefinitely.
func signalStop(db *sql.DB, id string, out Outcome, iterations int) Outcome {
	st, err := Status(db, id)
	if err != nil || !IsTerminal(st) {
		_ = finishStatus(db, id, StatusAborted)
		st = StatusAborted
	}
	out.Status = st
	out.Iterations = iterations
	out.Note = strings.TrimSpace(out.Note + " loop interrupted (signal); status recorded as " + st + ".")
	return out
}

// goalAchieved reports whether the model actually DECLARED success, as opposed
// to merely reflecting the instruction back.
//
// This is a deliberate fix, not a port. Python's _is_goal_achieved is a bare
// `"GOAL_ACHIEVED" in output` — but the sentinel is also present in the PROMPT
// ("...Output GOAL_ACHIEVED when done"), so any provider that echoes its input
// (the offline echo provider does exactly that, and quoting models do it partly)
// completes the loop on iteration 1 with a fabricated success. That is the
// no-fake-success bar. Removing the prompt text before scanning makes an echoed
// instruction inert while a genuine "GOAL_ACHIEVED" from the model still counts,
// including when the model quotes the instruction and then declares success.
func goalAchieved(prompt, output string) bool {
	residue := output
	if prompt != "" {
		residue = strings.ReplaceAll(residue, prompt, "")
	}
	return strings.Contains(residue, goalMarker)
}

// buildPrompt reproduces loop_agent._run_iteration's two prompt shapes verbatim.
func buildPrompt(goal string, iteration int, previous string) string {
	if previous != "" {
		return fmt.Sprintf("Goal: %s\n\nPrevious iteration output:\n%s\n\n"+
			"This is iteration %d. Continue working toward the goal, "+
			"or output %s if the goal has been met.",
			goal, truncate(previous, 2000), iteration, goalMarker)
	}
	return fmt.Sprintf("Goal: %s\n\nThis is iteration %d of an autonomous agent loop. "+
		"Work toward achieving the goal. Output %s when done.", goal, iteration, goalMarker)
}

// runIteration executes one agent-loop pass in its OWN working directory.
func runIteration(ctx context.Context, rec *trace.Recorder, parent *trace.Span,
	loopID string, iter int, prompt, profile string, opts Options) (string, error) {

	l := &agent.Loop{Provider: opts.Provider, Tracer: rec}
	if parent != nil {
		// Nest the pass's agent.run under this iteration's span, the way
		// internal/solver nests solver iterations.
		l.ParentSpanID = parent.ID
	}
	if opts.WithTools {
		reg := agent.NewRegistry()
		topts := tool.DefaultOptions()
		topts.Guard = opts.Guard
		// #591: iterations (and concurrent loops) must not share one cwd. Each
		// pass gets a private scratch dir that also becomes its tool root, so a
		// write_file in iteration 2 cannot clobber iteration 1's artifact and two
		// loops running at once cannot collide.
		dir, cleanup, derr := iterWorkDir(opts.WorkRoot, loopID, iter)
		if derr != nil {
			return "", fmt.Errorf("preparing work dir for loop %s iteration %d: %w", loopID, iter, derr)
		}
		defer cleanup()
		topts.Root = dir
		tool.Register(reg, topts)
		l.Tools = reg
	}
	res, err := l.Run(ctx, prompt, agent.Options{
		Model:    opts.Model,
		System:   opts.System,
		MaxSteps: opts.MaxSteps,
	})
	if err != nil {
		return "", err
	}
	return res.FinalText, nil
}

// waitForApproval publishes a checkpoint and blocks until another process
// decides it, the loop is aborted, the context is cancelled, or the timeout
// expires. It NEVER reads stdin: the decision channel is the store, so a loop
// running headless in the background can still be gated by a human without any
// possibility of blocking on a terminal that is not there.
//
// Returns (approved, reason, err).
func waitForApproval(ctx context.Context, db *sql.DB, id string, iter int, output string, opts Options) (bool, string, error) {
	if err := requestApprovalRow(db, id, iter, truncate(output, 500)); err != nil {
		return false, "", err
	}
	if err := setStatus(context.Background(), db, id, StatusWaitingApproval); err != nil {
		return false, "", err
	}
	deadline := time.Now().Add(opts.ApprovalTimeout)
	t := time.NewTicker(opts.ApprovalPoll)
	defer t.Stop()
	for {
		decision, ok, err := readApproval(db, id)
		if err != nil {
			return false, "", err
		}
		if ok {
			switch decision {
			case "continue", "approve", "approved":
				return true, "approved", nil
			case "abort", "deny", "denied", "reject":
				return false, "denied by operator", nil
			}
		}
		// An external abort must break the wait immediately rather than hanging
		// out the full timeout (B020/B021).
		if st, serr := Status(db, id); serr == nil && IsTerminal(st) {
			return false, "aborted while awaiting approval", nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Sprintf("approval timed out after %s (no `tag loop approve %s` was received)",
				opts.ApprovalTimeout, id), nil
		}
		select {
		case <-ctx.Done():
			return false, "interrupted while awaiting approval", nil
		case <-t.C:
		}
	}
}

// DefaultWorkRootName is the directory under TAG_HOME holding per-iteration
// working directories when Options.WorkRoot is unset. Matches
// worker.DefaultWorkRootName so a deployment has one artifacts root.
const DefaultWorkRootName = "work"

// iterWorkDir creates the private working directory for one iteration.
//
// Deliberately the same shape as worker.jobWorkDir (#591): an EMPTY scratch dir
// (not a repo copy or checkout), a stable derivable path so an operator can find
// artifacts afterwards, and a cleanup that removes the dir only if the iteration
// left it empty. It is reimplemented rather than imported because
// worker.jobWorkDir is unexported and internal/worker is owned elsewhere.
func iterWorkDir(workRoot, loopID string, iter int) (string, func(), error) {
	if workRoot == "" {
		workRoot = filepath.Join(paths.Home(), DefaultWorkRootName)
	}
	dir := filepath.Join(workRoot, "loop-"+safeSegment(loopID), fmt.Sprintf("iter-%d", iter))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, err
	}
	return dir, func() {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
			_ = os.Remove(dir)
		}
	}, nil
}

// safeSegment reduces an id to a single path segment so it can never steer the
// path outside WorkRoot.
func safeSegment(s string) string {
	out := filepath.Base(filepath.Clean("/" + strings.ReplaceAll(s, string(os.PathSeparator), "_")))
	if out == "." || out == string(os.PathSeparator) || out == "" {
		return "loop"
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
