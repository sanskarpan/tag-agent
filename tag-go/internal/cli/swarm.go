package cli

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/swarm"
	"github.com/tag-agent/tag/internal/trace"
)

// registerSwarm wires the full swarm command set: run/abort (PRD-004/PRD-023
// execution, ported from src/tag/swarm.py + src/tag/cmd/swarm.py) plus the
// pre-existing list/status/results inspection paths — which until now read
// swarm_runs/swarm_tasks that nothing in Go ever wrote.
func registerSwarm(root *cobra.Command, app *App) {
	s := &cobra.Command{Use: "swarm", Short: "Multi-agent swarm orchestration", GroupID: "orch"}

	s.AddCommand(swarmRunCmd(app), swarmAbortCmd(app), swarmListCmd(app), swarmStatusCmd(app), swarmResultsCmd(app))
	root.AddCommand(s)
}

// swarmJSONError prints Python's {"error":..., "swarm_id":...} shape and returns
// a runtime error so the exit code stays 1.
func swarmJSONError(swarmID, msg string) error {
	if flagJSON {
		b, _ := json.Marshal(map[string]any{"error": msg, "swarm_id": swarmID})
		fmt.Println(string(b))
		return exitCodeErr{1}
	}
	return errors.New(msg)
}

// swarmHexID mirrors Python's uuid.uuid4().hex[:12].
func swarmHexID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

// ---------------------------------------------------------------------------
// swarm run
// ---------------------------------------------------------------------------

func swarmRunCmd(app *App) *cobra.Command {
	var (
		goal            string
		coordProfile    string
		maxAgents       int
		failurePolicy   string
		timeoutPerAgent int
		approve         bool
		sequential      bool
		dryRun          bool
		provider        string
		withTools       bool
		maxSteps        int
		workRoot        string
		perms           permFlags
	)

	c := &cobra.Command{
		Use:   "run",
		Short: "Launch a context-centric multi-agent swarm",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// ---- validation (usage errors => exit 2) -------------------------
			//
			// Python returns 1 for these. That is inconsistent with its own comment
			// ("Exit 2 is reserved for usage/argument errors") and with the Go tree's
			// #537a convention that a bad flag VALUE is exit 2. Deliberate divergence.
			if strings.TrimSpace(goal) == "" {
				return jsonErrorMaybe(usageErrorf("--goal is required"))
			}
			if maxAgents < 1 {
				return jsonErrorMaybe(usageErrorf("--max-agents must be >= 1"))
			}
			if maxAgents > swarm.MaxAgents {
				return jsonErrorMaybe(usageErrorf("--max-agents must be <= %d", swarm.MaxAgents))
			}
			if !swarm.ValidFailurePolicy(failurePolicy) {
				return jsonErrorMaybe(usageErrorf("invalid --failure-policy %q (want one of %s)",
					failurePolicy, strings.Join(swarm.ValidFailurePolicies, ", ")))
			}
			if timeoutPerAgent < 1 {
				return jsonErrorMaybe(usageErrorf("--timeout-per-agent must be >= 1"))
			}
			if maxSteps < 0 {
				return jsonErrorMaybe(usageErrorf("--max-steps must be >= 0"))
			}
			prov, ok := llm.Registry[provider]
			if !ok {
				return jsonErrorMaybe(usageErrorf("unknown provider %q (available: %v)", provider, providerNames()))
			}
			if coordProfile == "" {
				coordProfile = app.Cfg.String("defaults.master_profile", "orchestrator")
			}

			swarmID := swarmHexID()
			parallel := !sequential

			// ---- --dry-run: describe, touch nothing --------------------------
			//
			// Offline by construction: no coordinator call, no DB row, no work dir.
			if dryRun {
				return swarmDryRun(swarmID, goal, coordProfile, failurePolicy, provider,
					maxAgents, timeoutPerAgent, parallel, withTools)
			}

			if approve && !permission.StdioInteractive() {
				// Never block a headless run on a read that can never be answered.
				return jsonErrorMaybe(fmt.Errorf("--approve needs an interactive terminal; re-run without it (or use --dry-run to preview the plan)"))
			}

			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if err := swarm.EnsureSchema(db.DB); err != nil {
				return err
			}

			// SIGINT/SIGTERM cancels the run so no task is stranded 'running'
			// (#574 pattern, same as `queue worker` / `dag run --execute`).
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			rec := trace.NewRecorder(swarmID, coordProfile)
			defer func() { _ = rec.Save(db.DB) }()

			modelFor := func(p string) string {
				return app.Cfg.String("profiles."+p+".config.model.default", "")
			}

			// ---- step 1: coordinator produces the manifest -------------------
			coord := &swarm.Coordinator{
				Provider: prov,
				Model:    modelFor(coordProfile),
				Profile:  coordProfile,
				Profiles: configuredProfiles(app),
				Tracer:   rec,
			}
			degraded, degradedReason := false, ""
			manifest, cerr := coord.ProduceManifest(ctx, goal, swarmID, maxAgents)
			if cerr != nil {
				if ctx.Err() != nil {
					return jsonErrorMaybe(fmt.Errorf("cancelled before the swarm started"))
				}
				// Offline honesty: `--provider echo` (and any model that will not
				// emit a manifest) degrades to a single real sub-agent on the whole
				// goal, loudly labelled. Python exits 1 here, which makes swarm run
				// unusable without live credentials.
				degraded, degradedReason = true, cerr.Error()
				manifest = swarm.FallbackManifest(goal, swarmID, coordProfile, degradedReason)
				fmt.Fprintf(os.Stderr,
					"warning: DEGRADED — the coordinator (provider=%s) produced no task manifest: %s\n"+
						"warning: falling back to a single sub-agent on the whole goal; this is NOT a decomposed swarm.\n",
					provider, degradedReason)
			}
			manifest.CoordinatorProfile = coordProfile
			if manifest.FailurePolicy == "" {
				manifest.FailurePolicy = failurePolicy
			}

			// ---- step 2: persist run + tasks ---------------------------------
			if err := swarm.CreateRun(ctx, db.DB, swarmID, goal, coordProfile, failurePolicy, maxAgents); err != nil {
				return err
			}
			if err := swarm.InsertTasks(ctx, db.DB, swarmID, manifest.Tasks); err != nil {
				return err
			}
			if err := swarm.SetManifest(ctx, db.DB, swarmID, manifest); err != nil {
				return err
			}

			if !flagJSON {
				fmt.Printf("Swarm %s — %d tasks — policy: %s\n", swarmID, len(manifest.Tasks), failurePolicy)
			}

			// ---- step 3: execute ---------------------------------------------
			var guard *permission.Guard
			if withTools {
				pf := perms
				// Sub-agents run concurrently on goroutines with no controlling
				// terminal of their own; an `ask` must resolve to a recorded deny
				// rather than N agents racing for stdin.
				pf.noPrompt = true
				g, gerr := pf.guard(app, coordProfile, swarmID, os.Stderr)
				if gerr != nil {
					return gerr
				}
				guard = g
			}

			opts := swarm.Options{
				Provider:        prov,
				Model:           modelFor(coordProfile),
				ModelForProfile: modelFor,
				MaxSteps:        maxSteps,
				WithTools:       withTools,
				Guard:           guard,
				WorkRoot:        workRoot,
				MaxAgents:       maxAgents,
				FailurePolicy:   failurePolicy,
				Sequential:      sequential,
				TimeoutPerAgent: time.Duration(timeoutPerAgent) * time.Second,
				Tracer:          rec,
				Degraded:        degraded,
				DegradedReason:  degradedReason,
			}
			if !flagJSON {
				opts.Out = os.Stdout
			}
			if approve {
				opts.Approve = swarmTTYApprover()
			}

			bus := swarm.NewContextBus(db.DB, swarmID)
			res, rerr := swarm.NewRunner(db.DB, manifest, bus, opts).Run(ctx, swarmID)
			if rerr != nil {
				return jsonErrorMaybe(rerr)
			}

			if flagJSON {
				if err := emitJSON(res); err != nil {
					return err
				}
			} else {
				fmt.Printf("\nStatus: %s", res.Status)
				if res.Degraded {
					fmt.Printf("  (degraded: %s)", res.DegradedReason)
				}
				fmt.Println()
				if res.FinalOutput != "" {
					fmt.Printf("\n%s\n", res.FinalOutput)
				}
			}
			// Python's exit-code map. Note Python returns 0 for ANY status when
			// --json is set, which hides a failed swarm from any script that asked
			// for machine-readable output; that bug is not ported.
			if code := swarmExitCode(res.Status); code != 0 {
				return exitCodeErr{code}
			}
			return nil
		},
	}

	c.Flags().StringVar(&goal, "goal", "", "natural-language goal for the swarm (required)")
	c.Flags().StringVar(&coordProfile, "coordinator-profile", "", "profile to use as coordinator")
	c.Flags().IntVar(&maxAgents, "max-agents", swarm.DefaultMaxAgents,
		fmt.Sprintf("max concurrent sub-agents (cap: %d)", swarm.MaxAgents))
	c.Flags().StringVar(&failurePolicy, "failure-policy", swarm.PolicyBestEffort,
		"one of: "+strings.Join(swarm.ValidFailurePolicies, ", "))
	c.Flags().IntVar(&timeoutPerAgent, "timeout-per-agent", 300, "per-sub-agent timeout in seconds")
	c.Flags().BoolVar(&approve, "approve", false, "pause for approval before each subtask (needs a TTY)")
	c.Flags().BoolVar(&sequential, "sequential", false, "dispatch agents sequentially (default: parallel)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without running anything")
	// Go-native additions: the Python swarm has no provider/tool flags because it
	// delegates execution to a subprocess that re-reads config.
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().BoolVar(&withTools, "tools", false, "enable built-in tools for sub-agents (bash/read_file/write_file/list_dir)")
	c.Flags().IntVar(&maxSteps, "max-steps", 0, "cap agent-loop turns per sub-agent (0 = loop default)")
	c.Flags().StringVar(&workRoot, "work-root", "", "parent dir for per-agent working directories (default: <TAG_HOME>/work)")
	perms.bind(c)
	return c
}

// isTerminalSwarmStatus reports whether a run has already finished, in which
// case its status is a record and not something to overwrite.
func isTerminalSwarmStatus(status string) bool {
	switch status {
	case "completed", "failed", "partial", "aborted":
		return true
	}
	return false
}

// swarmExitCode maps a run status to Python's exit-code table.
func swarmExitCode(status string) int {
	switch status {
	case "completed":
		return 0
	case "partial":
		return 5
	case "failed":
		return 3
	case "aborted":
		return 4
	default:
		return 1
	}
}

// swarmDryRun prints exactly what a run would do, and does none of it.
func swarmDryRun(swarmID, goal, coordProfile, failurePolicy, provider string, maxAgents, timeout int, parallel, withTools bool) error {
	dispatch := "parallel"
	if !parallel {
		dispatch = "sequential"
	}
	steps := []string{
		fmt.Sprintf("ask coordinator profile %q (provider=%s) to decompose the goal into at most %d tasks", coordProfile, provider, maxAgents),
		fmt.Sprintf("insert 1 row into swarm_runs (swarm_id=%s) and one row per task into swarm_tasks", swarmID),
		fmt.Sprintf("dispatch tasks %s, at most %d at a time, %ds timeout each", dispatch, maxAgents, timeout),
		fmt.Sprintf("apply failure policy %q, then synthesize the successful results", failurePolicy),
	}
	if withTools {
		steps = append(steps, "give each sub-agent its own working directory under the work root, with tools gated by the permission guard")
	} else {
		steps = append(steps, "run sub-agents WITHOUT tools (pass --tools to enable them)")
	}
	if flagJSON {
		return emitJSON(map[string]any{
			"swarm_id": swarmID, "goal": goal, "coordinator_profile": coordProfile,
			"max_agents": maxAgents, "failure_policy": failurePolicy,
			"timeout_per_agent": timeout, "parallel": parallel, "provider": provider,
			"tools": withTools, "dry_run": true, "would": steps,
		})
	}
	fmt.Printf("Swarm ID:     %s\n", swarmID)
	fmt.Printf("Goal:         %s\n", goal)
	fmt.Printf("Coordinator:  %s\n", coordProfile)
	fmt.Printf("Provider:     %s\n", provider)
	fmt.Printf("Max agents:   %d\n", maxAgents)
	fmt.Printf("Policy:       %s\n", failurePolicy)
	fmt.Printf("Dispatch:     %s\n", dispatch)
	fmt.Println("\nWould:")
	for _, s := range steps {
		fmt.Printf("  - %s\n", s)
	}
	fmt.Println("\n(dry run — nothing was written; re-run without --dry-run to execute.)")
	return nil
}

// swarmTTYApprover reproduces Python's `Dispatch? [y/N/skip]` prompt.
func swarmTTYApprover() func(swarm.Task) (swarm.ApproveDecision, error) {
	reader := bufio.NewReader(os.Stdin)
	return func(t swarm.Task) (swarm.ApproveDecision, error) {
		fmt.Fprintf(os.Stderr, "\n[swarm] Task: %s\n", t.TaskID)
		fmt.Fprintf(os.Stderr, "  Description: %s\n", truncate(t.Description, 120))
		fmt.Fprintf(os.Stderr, "  Profile:     %s\n", t.Profile)
		fmt.Fprintf(os.Stderr, "  Context:     %s → %v\n", t.ContextSlice.Type, t.ContextSlice.Selector)
		fmt.Fprint(os.Stderr, "Dispatch? [y/N/skip] ")
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			// EOF on stdin is "no answer", which must abort rather than hang or
			// silently dispatch.
			return swarm.ApproveAbort, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y":
			return swarm.ApproveDispatch, nil
		case "skip":
			return swarm.ApproveSkip, nil
		default:
			return swarm.ApproveAbort, nil
		}
	}
}

// configuredProfiles lists the profile names offered to the coordinator.
func configuredProfiles(app *App) []string {
	var out []string
	if app != nil && app.Cfg != nil {
		if m, ok := app.Cfg.Data["profiles"].(map[string]any); ok {
			for k := range m {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// swarm abort
// ---------------------------------------------------------------------------

func swarmAbortCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "abort <swarm-id>",
		Short: "Abort a running swarm",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			swarmID := args[0]
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var cur string
			if err := db.QueryRow(`SELECT status FROM swarm_runs WHERE swarm_id=? LIMIT 1`, swarmID).Scan(&cur); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return swarmJSONError(swarmID, fmt.Sprintf("swarm %s not found", swarmID))
				}
				return err
			}
			// Aborting a run that already reached a terminal state would rewrite
			// history: a completed run would be relabelled `aborted` and its real
			// outcome lost, with exit 0 reporting that as a successful abort. Python
			// has the same unguarded UPDATE; this is a deliberate divergence.
			if isTerminalSwarmStatus(cur) {
				return swarmJSONError(swarmID, fmt.Sprintf(
					"swarm %s is not running (status: %s); nothing to abort", swarmID, cur))
			}
			// Python SIGTERMs each running task's process group. Go sub-agents are
			// goroutines inside `swarm run`, so there is no PID to signal: the run
			// row IS the abort channel. Flipping the run to 'aborted' is polled by
			// any live Runner (swarm.runAborted), which cancels its context and
			// finalises every in-flight task — so this stays a real cross-process
			// abort, not just a status rewrite.
			var running int
			if err := db.QueryRow(`SELECT COUNT(*) FROM swarm_tasks WHERE swarm_id=? AND status='running'`, swarmID).Scan(&running); err != nil {
				return err
			}
			if _, err := db.Exec(`UPDATE swarm_runs SET status='aborted', completed_at=? WHERE swarm_id=?`,
				time.Now().UTC().Format("2006-01-02T15:04:05.000")+"Z", swarmID); err != nil {
				return err
			}
			if _, err := db.Exec(`UPDATE swarm_tasks SET status='failed', error_message='aborted by user',
				completed_at=? WHERE swarm_id=? AND status='running'`,
				time.Now().UTC().Format("2006-01-02T15:04:05.000")+"Z", swarmID); err != nil {
				return err
			}
			// Leave nothing claimable behind either.
			if _, err := db.Exec(`UPDATE swarm_tasks SET status='skipped', error_message='aborted by user'
				WHERE swarm_id=? AND status='pending'`, swarmID); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{"swarm_id": swarmID, "status": "aborted",
					"signalled": running, "tasks_aborted": running})
			}
			fmt.Printf("Swarm %s aborted — %d agent(s) signalled.\n", swarmID, running)
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// swarm list / status / results
// ---------------------------------------------------------------------------

func swarmListCmd(app *App) *cobra.Command {
	var statusFilter string
	c := &cobra.Command{Use: "list", Short: "List swarm runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			q := `SELECT swarm_id, goal, status, task_count, COALESCE(started_at,''),
				COALESCE(completed_at,''), total_cost_usd FROM swarm_runs`
			var qargs []any
			if statusFilter != "" {
				q += ` WHERE status=?`
				qargs = append(qargs, statusFilter)
			}
			q += ` ORDER BY created_at DESC LIMIT 50`
			rows, err := db.Query(q, qargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			type run struct {
				SwarmID     string  `json:"swarm_id"`
				Goal        string  `json:"goal"`
				Status      string  `json:"status"`
				Tasks       int     `json:"task_count"`
				StartedAt   string  `json:"started_at"`
				CompletedAt string  `json:"completed_at"`
				Cost        float64 `json:"total_cost_usd"`
			}
			out := []run{}
			for rows.Next() {
				var r run
				var cost sql.NullFloat64
				if err := rows.Scan(&r.SwarmID, &r.Goal, &r.Status, &r.Tasks, &r.StartedAt, &r.CompletedAt, &cost); err != nil {
					return err
				}
				r.Cost = cost.Float64
				out = append(out, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No swarm runs found.")
				return nil
			}
			fmt.Printf("%-14s %-12s %5s %8s  Goal\n", "Swarm ID", "Status", "Tasks", "Cost")
			fmt.Println(strings.Repeat("-", 80))
			for _, r := range out {
				fmt.Printf("%-14s %-12s %5d $%7.4f  %s\n", r.SwarmID, r.Status, r.Tasks, r.Cost, truncate(r.Goal, 45))
			}
			return nil
		}}
	c.Flags().StringVar(&statusFilter, "status", "", "filter by status (running/completed/aborted/failed/partial)")
	return c
}

func swarmStatusCmd(app *App) *cobra.Command {
	return &cobra.Command{Use: "status <swarm-id>", Short: "Show per-task status for a swarm", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var sid, goal, st string
			var tasks int
			var startedAt string
			var cost sql.NullFloat64
			err = db.QueryRow(`SELECT swarm_id, goal, status, task_count, COALESCE(started_at,''), total_cost_usd
				FROM swarm_runs WHERE swarm_id=?`, args[0]).Scan(&sid, &goal, &st, &tasks, &startedAt, &cost)
			if errors.Is(err, sql.ErrNoRows) {
				return swarmJSONError(args[0], fmt.Sprintf("swarm %s not found", args[0]))
			}
			if err != nil {
				return err
			}
			trows, err := db.Query(`SELECT task_id, COALESCE(profile,''), COALESCE(status,''),
				COALESCE(started_at,''), COALESCE(completed_at,''), cost_usd, COALESCE(error_message,'')
				FROM swarm_tasks WHERE swarm_id=? ORDER BY id`, sid)
			if err != nil {
				return err
			}
			defer trows.Close()
			type task struct {
				TaskID      string  `json:"task_id"`
				Profile     string  `json:"profile"`
				Status      string  `json:"status"`
				StartedAt   string  `json:"started_at"`
				CompletedAt string  `json:"completed_at"`
				Cost        float64 `json:"cost_usd"`
				Error       string  `json:"error_message"`
			}
			trs := []task{}
			for trows.Next() {
				var t task
				var tcost sql.NullFloat64
				if err := trows.Scan(&t.TaskID, &t.Profile, &t.Status, &t.StartedAt, &t.CompletedAt, &tcost, &t.Error); err != nil {
					return err
				}
				t.Cost = tcost.Float64
				trs = append(trs, t)
			}
			if err := trows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{
					"run": map[string]any{"swarm_id": sid, "goal": goal, "status": st,
						"task_count": tasks, "started_at": startedAt, "total_cost_usd": cost.Float64},
					"tasks": trs})
			}
			fmt.Printf("Swarm:  %s  (%s)  tasks=%d  cost=$%.4f\n", sid, st, tasks, cost.Float64)
			fmt.Printf("Goal:   %s\n\n", goal)
			fmt.Printf("%-22s %-18s %-14s Cost\n", "Task ID", "Profile", "Status")
			fmt.Println(strings.Repeat("-", 70))
			for _, t := range trs {
				errNote := ""
				if t.Error != "" {
					errNote = "  [" + truncate(t.Error, 40) + "]"
				}
				fmt.Printf("%-22s %-18s %-14s $%.4f%s\n", t.TaskID, t.Profile, t.Status, t.Cost, errNote)
			}
			return nil
		}}
}

func swarmResultsCmd(app *App) *cobra.Command {
	var format string
	var includeContext bool
	c := &cobra.Command{Use: "results <swarm-id>", Short: "Show results and final output for a swarm", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "table" && format != "json" {
				return jsonErrorMaybe(usageErrorf("--format must be 'table' or 'json'"))
			}
			asJSON := flagJSON || format == "json"
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var sid, goal, st string
			var finalOut sql.NullString
			var cost sql.NullFloat64
			err = db.QueryRow(`SELECT swarm_id, goal, status, final_output, total_cost_usd FROM swarm_runs WHERE swarm_id=?`, args[0]).
				Scan(&sid, &goal, &st, &finalOut, &cost)
			if errors.Is(err, sql.ErrNoRows) {
				if asJSON {
					b, _ := json.Marshal(map[string]any{"error": fmt.Sprintf("swarm %s not found", args[0]), "swarm_id": args[0]})
					fmt.Println(string(b))
					return exitCodeErr{1}
				}
				return fmt.Errorf("swarm %s not found", args[0])
			}
			if err != nil {
				return err
			}
			trows, err := db.Query(`SELECT task_id, COALESCE(profile,''), COALESCE(status,''), cost_usd,
				tokens_prompt, tokens_completion, COALESCE(output,''), COALESCE(error_message,'')
				FROM swarm_tasks WHERE swarm_id=? ORDER BY id`, sid)
			if err != nil {
				return err
			}
			defer trows.Close()
			type task struct {
				TaskID           string  `json:"task_id"`
				Profile          string  `json:"profile"`
				Status           string  `json:"status"`
				Cost             float64 `json:"cost_usd"`
				TokensPrompt     int64   `json:"tokens_prompt"`
				TokensCompletion int64   `json:"tokens_completion"`
				Output           string  `json:"output"`
				Error            string  `json:"error_message"`
			}
			trs := []task{}
			for trows.Next() {
				var t task
				var tcost sql.NullFloat64
				var tp, tc sql.NullInt64
				if err := trows.Scan(&t.TaskID, &t.Profile, &t.Status, &tcost, &tp, &tc, &t.Output, &t.Error); err != nil {
					return err
				}
				t.Cost, t.TokensPrompt, t.TokensCompletion = tcost.Float64, tp.Int64, tc.Int64
				trs = append(trs, t)
			}
			if err := trows.Err(); err != nil {
				return err
			}
			var ctxRows []swarm.BusEntry
			if includeContext {
				if err := swarm.EnsureSchema(db.DB); err != nil {
					return err
				}
				ctxRows = swarm.NewContextBus(db.DB, sid).FullAudit(cmd.Context())
			}
			if asJSON {
				out := map[string]any{"swarm_id": sid, "goal": goal, "status": st,
					"final_output": finalOut.String, "total_cost_usd": cost.Float64, "tasks": trs}
				if includeContext {
					out["context_bus"] = ctxRows
				}
				return emitJSON(out)
			}
			fmt.Printf("Swarm:  %s  (%s)  total_cost=$%.4f\n", sid, st, cost.Float64)
			fmt.Printf("Goal:   %s\n\n", goal)
			fmt.Printf("%-22s %-14s %8s %8s\n", "Task ID", "Status", "Tokens", "Cost")
			fmt.Println(strings.Repeat("-", 60))
			for _, t := range trs {
				fmt.Printf("%-22s %-14s %8d $%7.4f\n", t.TaskID, t.Status, t.TokensPrompt+t.TokensCompletion, t.Cost)
			}
			if finalOut.String != "" {
				fmt.Printf("\n-- Final Output --\n%s\n", finalOut.String)
			}
			if includeContext && len(ctxRows) > 0 {
				fmt.Printf("\n-- Context Bus (%d entries) --\n", len(ctxRows))
				for _, e := range ctxRows {
					v, _ := json.Marshal(e.Value)
					fmt.Printf("  [%s] %s = %s\n", e.WrittenBy, e.Key, truncate(string(v), 80))
				}
			}
			return nil
		}}
	c.Flags().StringVar(&format, "format", "table", "output format: table|json")
	c.Flags().BoolVar(&includeContext, "include-context", false, "include the context-bus audit trail")
	return c
}
