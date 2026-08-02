package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	looppkg "github.com/tag-agent/tag/internal/loop"
)

// registerLoop wires the autonomous agent-loop LIFECYCLE (PRD-021), the Go port
// of src/tag/cmd/ci_loop.py:cmd_loop:
//
//	loop start --goal ... [--profile] [--max-iters] [--approval auto|human]
//	loop list
//	loop status LOOP_ID
//	loop abort LOOP_ID
//	loop approve LOOP_ID
//	loop deny LOOP_ID
//
// It ATTACHES to the existing `loop <prompt> --iterations N` command registered
// by registerCI rather than replacing it, so the pre-existing single-shot driver
// keeps working (`tag loop --provider echo --iterations 2 "hi"`) while the
// lifecycle verbs are added alongside. Cobra dispatches a first argument that
// matches a subcommand name to that subcommand and otherwise falls through to
// the parent's RunE, so both forms coexist.
//
// Registration order matters: this must run after registerCI (see root.go). If
// `loop` is somehow absent, the command is created here so the tree is still
// well-formed.
func registerLoop(root *cobra.Command, app *App) {
	loopCmd := findCommand(root, "loop")
	if loopCmd == nil {
		loopCmd = &cobra.Command{
			Use:     "loop",
			Short:   "Autonomous agent loop with goal detection and iteration cap",
			GroupID: "orch",
		}
		root.AddCommand(loopCmd)
	}

	var rf loopRunFlags
	var startGoal, startProfile, startApproval string
	var startMaxIters int
	var startDetach bool

	start := &cobra.Command{
		Use:   "start",
		Short: "Start an autonomous loop",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			goal := strings.TrimSpace(startGoal)
			if goal == "" {
				// Python declares --goal as argparse required=True (exit 2) and then
				// ALSO rejects an all-whitespace value with exit 1. Both are usage
				// failures; Go reports 2 for either, matching the repo's #537a
				// convention for a semantically invalid flag value.
				return jsonErrorMaybe(usageErrorf("--goal TEXT is required"))
			}
			if startMaxIters < 1 {
				// #537a: a semantically invalid flag VALUE is exit 2, the same as the
				// --approval check three lines down and as every swarm/eval numeric
				// flag. Python emits 1 here through its own error helper, but the two
				// adjacent validations disagreeing about the exit code for one class of
				// mistake is not a parity property worth keeping.
				return jsonErrorMaybe(usageErrorf("--max-iters must be >= 1"))
			}
			if startApproval != looppkg.ApprovalAuto && startApproval != looppkg.ApprovalHuman {
				// Python parity: argparse choices=[auto,human] -> exit 2.
				return jsonErrorMaybe(usageErrorf("--approval must be 'auto' or 'human'"))
			}
			if rf.approvalTimeout <= 0 {
				// Silently coercing 0/-1s to the 5m default let an operator believe
				// they had disabled (or shortened) the wait while it ran unchanged.
				return jsonErrorMaybe(usageErrorf("--approval-timeout must be > 0 (got %s)", rf.approvalTimeout))
			}
			if _, err := rf.provider(); err != nil {
				return jsonErrorMaybe(err)
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			profile := app.profile(startProfile)
			id, err := looppkg.Create(db.DB, profile, goal, startMaxIters, startApproval)
			if err != nil {
				return jsonErrorMaybe(err)
			}

			if startDetach {
				pid, logPath, derr := spawnLoopWorker(id, &rf)
				if derr != nil {
					// A detach that could not start is a FAILURE, not a "running"
					// loop. Python's _launch_loop_worker reports status "running"
					// with a pid without ever checking the worker came up, and it
					// sends the worker's stdout/stderr to DEVNULL — a loop that dies
					// on startup is indistinguishable from one that works. Roll the
					// row back to 'failed' and report honestly.
					_, _ = looppkg.Abort(db.DB, id)
					_ = derr
					return jsonErrorMaybe(fmt.Errorf("could not start detached loop worker: %w", derr))
				}
				outJSON(
					map[string]any{"loop_id": id, "pid": pid, "status": "running", "log": logPath},
					fmt.Sprintf("loop started: %s  (worker pid %d)\ngoal: %s\nmax iterations: %d  approval: %s\nlog: %s",
						id, pid, truncate(goal, 80), startMaxIters, startApproval, logPath))
				return nil
			}

			// Foreground (default). Divergence from Python, which ALWAYS detaches:
			// running in-process surfaces the loop's real exit code and its output,
			// which is the whole point of Go owning the runtime. `--detach` restores
			// the Python shape for callers that want it.
			opts, err := rf.runnerOptions(app, profile, cmd.ErrOrStderr())
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if !flagJSON {
				fmt.Printf("loop started: %s\ngoal: %s\nmax iterations: %d  approval: %s\n",
					id, truncate(goal, 80), startMaxIters, startApproval)
				opts.Log = cmd.OutOrStdout()
			}
			// SIGINT/SIGTERM must not strand the row in 'running' (#574): the
			// runner writes a terminal status on cancellation. Same construction as
			// `queue worker` / `dag run --execute`.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return finishLoop(looppkg.Run(ctx, db.DB, id, opts))
		},
	}
	start.Flags().StringVar(&startGoal, "goal", "", "goal text the loop works toward (required)")
	start.Flags().StringVar(&startProfile, "profile", "", "profile to run (default: master profile)")
	start.Flags().IntVar(&startMaxIters, "max-iters", 10, "maximum number of iterations (default: 10)")
	start.Flags().StringVar(&startApproval, "approval", looppkg.ApprovalAuto,
		"gate mode: 'auto' continues automatically, 'human' pauses for `tag loop approve|deny`")
	start.Flags().BoolVar(&startDetach, "detach", false,
		"run the loop in a background process and return immediately (Python-compatible behaviour)")
	rf.bind(start)

	// Hidden re-entry point used by --detach. Not part of the public surface.
	var workerID string
	worker := &cobra.Command{
		Use:    "run-worker",
		Short:  "internal: drive an already-created loop (used by `loop start --detach`)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if workerID == "" {
				return usageErrorf("--loop-id is required")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			run, err := looppkg.Get(db.DB, workerID)
			if err != nil {
				return err
			}
			opts, err := rf.runnerOptions(app, run.Profile, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			opts.Log = cmd.OutOrStdout()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return finishLoop(looppkg.Run(ctx, db.DB, workerID, opts))
		},
	}
	worker.Flags().StringVar(&workerID, "loop-id", "", "loop id to drive")
	rf.bind(worker)

	var listLimit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List all loop runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if listLimit < 0 {
				return jsonErrorMaybe(usageErrorf("--limit must be >= 0, got %d", listLimit))
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			runs, err := looppkg.List(db.DB, listLimit)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				// runs is never nil — empty renders as [], not null.
				return emitJSON(runs)
			}
			if len(runs) == 0 {
				fmt.Println("No loop runs.")
				return nil
			}
			fmt.Printf("  %-14s %-16s %-18s %-8s %s\n", "ID", "PROFILE", "STATUS", "ITERS", "GOAL")
			fmt.Println("  " + strings.Repeat("-", 76))
			for _, r := range runs {
				fmt.Printf("  %-14s %-16s %-18s %-8s %s\n", r.ID, r.Profile, r.Status,
					fmt.Sprintf("%d/%d", r.CurrentIter, r.MaxIters), truncate(r.Goal, 40))
			}
			return nil
		},
	}
	list.Flags().IntVar(&listLimit, "limit", 20, "max runs to show (default: 20)")

	status := &cobra.Command{
		Use:   "status LOOP_ID",
		Short: "Show status and iterations of a loop",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			run, err := looppkg.Get(db.DB, args[0])
			if errors.Is(err, looppkg.ErrNotFound) {
				return jsonErrorMaybe(fmt.Errorf("Loop '%s' not found", args[0]))
			}
			if err != nil {
				return jsonErrorMaybe(err)
			}
			iters, err := looppkg.Iterations(db.DB, args[0])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			pending, err := looppkg.PendingApproval(db.DB, args[0])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				payload := map[string]any{
					"id": run.ID, "profile": run.Profile, "goal": run.Goal,
					"max_iters": run.MaxIters, "current_iter": run.CurrentIter,
					"status": run.Status, "approval": run.Approval,
					"created_at": run.CreatedAt, "updated_at": run.UpdatedAt,
					"completed_at": run.CompletedAt, "iterations": iters,
				}
				if pending != nil && pending.Decision == "pending" {
					payload["pending_approval"] = pending
				}
				return emitJSON(payload)
			}
			fmt.Printf("Loop: %s\n", run.ID)
			fmt.Printf("  Profile: %s  Status: %s\n", run.Profile, run.Status)
			fmt.Printf("  Goal: %s\n", truncate(run.Goal, 80))
			fmt.Printf("  Iterations: %d/%d\n", run.CurrentIter, run.MaxIters)
			for _, it := range iters {
				fmt.Printf("  [%d] %s: %s\n", it.Iteration, it.Decision, truncate(oneLineCLI(it.Output), 80))
			}
			if pending != nil && pending.Decision == "pending" {
				fmt.Printf("  awaiting approval at iteration %d — `tag loop approve %s` / `tag loop deny %s`\n",
					pending.Iteration, run.ID, run.ID)
			}
			return nil
		},
	}

	abort := &cobra.Command{
		Use:   "abort LOOP_ID",
		Short: "Abort a running loop",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			ok, err := looppkg.Abort(db.DB, args[0])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if !ok {
				return jsonErrorMaybe(fmt.Errorf("No running loop found: %s", args[0]))
			}
			outJSON(map[string]any{"loop_id": args[0], "status": "aborted"},
				"abort requested: "+args[0])
			return nil
		},
	}

	decision := func(name, short string, approve bool) *cobra.Command {
		return &cobra.Command{
			Use:   name + " LOOP_ID",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				db, err := app.OpenDB()
				if err != nil {
					return err
				}
				if err := looppkg.Decide(db.DB, args[0], approve); err != nil {
					return jsonErrorMaybe(err)
				}
				d := "abort"
				verb := "denied (abort)"
				if approve {
					d, verb = "continue", "approved (continue)"
				}
				outJSON(map[string]any{"loop_id": args[0], "decision": d},
					fmt.Sprintf("loop %s %s", args[0], verb))
				return nil
			},
		}
	}

	loopCmd.AddCommand(start, worker, list, status, abort,
		decision("approve", "Approve a loop waiting for human approval (continue)", true),
		decision("deny", "Deny a loop waiting for human approval (abort)", false))
}

// finishLoop renders a terminal Outcome and maps it to an exit code:
// 'failed' -> 1 (parity with loop_agent.main), everything else -> 0.
func finishLoop(out looppkg.Outcome, err error) error {
	if err != nil {
		return jsonErrorMaybe(err)
	}
	if flagJSON {
		if jerr := emitJSON(out); jerr != nil {
			return jerr
		}
	} else {
		fmt.Printf("loop %s finished: %s (%d/%d iterations)\n",
			out.LoopID, out.Status, out.Iterations, out.MaxIters)
		if out.Note != "" {
			fmt.Println("note: " + out.Note)
		}
	}
	// Already reported above; carry only the exit status. `aborted` (SIGTERM, or
	// a cross-process `loop deny`) is NOT success: a CI wrapper could not tell a
	// killed loop from a finished one. 4 is the code swarm already uses for the
	// identical situation.
	switch out.Status {
	case looppkg.StatusFailed:
		return exitCodeErr{1}
	case looppkg.StatusAborted:
		return exitCodeErr{4}
	}
	return nil
}

// loopRunFlags is the execution surface shared by `loop start` and the hidden
// detached worker.
type loopRunFlags struct {
	provName        string
	model           string
	maxSteps        int
	tools           bool
	approvalTimeout time.Duration
	perms           permFlags
}

func (f *loopRunFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&f.provName, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().StringVar(&f.model, "model", "", "model id override")
	c.Flags().IntVar(&f.maxSteps, "max-steps", 0, "cap agent-loop turns per iteration (0 = default)")
	c.Flags().BoolVar(&f.tools, "tools", false, "enable built-in tools (bash/read_file/write_file/list_dir)")
	c.Flags().DurationVar(&f.approvalTimeout, "approval-timeout", looppkg.DefaultApprovalTimeout,
		"how long `--approval human` waits for a decision before denying (must be > 0)")
	f.perms.bind(c)
}

func (f *loopRunFlags) provider() (llm.Provider, error) {
	name := f.provName
	if name == "" {
		name = "echo"
	}
	prov, ok := llm.Registry[name]
	if !ok {
		return nil, usageErrorf("unknown provider %q (available: %v)", name, providerNames())
	}
	return prov, nil
}

// runnerOptions resolves the loop's execution options.
//
// The permission guard is ALWAYS non-interactive (noPrompt), the same rule
// buildWorkerOptions applies to `queue worker` / `dag run --execute`: a loop is a
// long-running autonomous driver that may be detached, so stopping mid-iteration
// to read a consent answer off a terminal is exactly the silent hang this project
// forbids. An `ask` verdict therefore resolves to a recorded deny; grant with
// --allow-tool/--auto-approve or a permissions block in config.
//
// This is separate from `--approval human`, which gates the LOOP's next
// iteration and is answered out-of-band by `tag loop approve|deny`.
func (f *loopRunFlags) runnerOptions(app *App, profile string, warnTo io.Writer) (looppkg.Options, error) {
	prov, err := f.provider()
	if err != nil {
		return looppkg.Options{}, err
	}
	opts := looppkg.Options{
		Provider:        prov,
		Model:           f.model,
		MaxSteps:        f.maxSteps,
		WithTools:       f.tools,
		ApprovalTimeout: f.approvalTimeout,
	}
	if opts.Model == "" && app != nil && app.Cfg != nil {
		opts.Model = app.Cfg.String("profiles."+profile+".config.model.default", "")
	}
	if f.tools {
		pf := f.perms
		pf.noPrompt = true
		g, gerr := pf.guard(app, profile, "", warnTo)
		if gerr != nil {
			return looppkg.Options{}, gerr
		}
		opts.Guard = g
	}
	return opts, nil
}

// spawnLoopWorker re-execs this binary as a background loop driver and returns
// its pid plus the log file that captures its output.
//
// Unlike Python's _launch_loop_worker (stdout/stderr -> DEVNULL) the child's
// output is captured to <TAG_HOME>/logs/loop-<id>.log, so a worker that dies on
// startup leaves evidence instead of a permanently 'running' row.
func spawnLoopWorker(id string, rf *loopRunFlags) (int, string, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, "", err
	}
	logDir := filepath.Join(tagHomeDir(), "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return 0, "", err
	}
	logPath := filepath.Join(logDir, "loop-"+id+".log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer lf.Close()

	args := []string{"loop", "run-worker", "--loop-id", id,
		"--provider", rf.provName,
		"--approval-timeout", rf.approvalTimeout.String()}
	if flagConfig != "" {
		args = append(args, "--config", flagConfig)
	}
	if rf.model != "" {
		args = append(args, "--model", rf.model)
	}
	if rf.maxSteps > 0 {
		args = append(args, "--max-steps", fmt.Sprint(rf.maxSteps))
	}
	if rf.tools {
		args = append(args, "--tools")
	}
	for _, a := range rf.perms.allow {
		args = append(args, "--allow-tool", a)
	}
	for _, a := range rf.perms.deny {
		args = append(args, "--deny-tool", a)
	}
	for _, a := range rf.perms.ask {
		args = append(args, "--ask-tool", a)
	}
	if rf.perms.autoApprove {
		args = append(args, "--auto-approve")
	}
	if rf.perms.dangerous {
		args = append(args, "--dangerously-allow-all")
	}

	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return 0, "", err
	}
	pid := cmd.Process.Pid
	// Detach: never Wait, so the child is reparented when this process exits.
	_ = cmd.Process.Release()
	return pid, logPath, nil
}

// tagHomeDir resolves TAG_HOME for the detached worker's log file.
func tagHomeDir() string {
	if h := os.Getenv("TAG_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tag"
	}
	return filepath.Join(home, ".tag")
}

// findCommand returns the direct child command with the given name, or nil.
func findCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func oneLineCLI(s string) string { return strings.Join(strings.Fields(s), " ") }
