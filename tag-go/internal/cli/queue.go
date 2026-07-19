package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/worker"
)

// queueHexID returns a dash-free hex job id of the given length (mirrors
// Python dag.add_job's uuid.uuid4().hex[:n]).
func queueHexID(n int) string {
	h := strings.ReplaceAll(uuid.NewString(), "-", "")
	if len(h) > n {
		return h[:n]
	}
	return h
}

func registerQueue(root *cobra.Command, app *App) {
	var profile, taskType string
	var priority int
	q := &cobra.Command{Use: "queue", Short: "Background task queue", GroupID: "orch"}

	var deps []string
	add := &cobra.Command{Use: "add TASK", Short: "Enqueue a job", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if priority < 0 {
				return fmt.Errorf("priority must be >= 0")
			}
			task := strings.ReplaceAll(args[0], "\x00", "")
			if strings.TrimSpace(task) == "" {
				return fmt.Errorf("task text must not be empty")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			// Dependency-aware path (mirrors Python `queue-dep add` / dag.add_job):
			// validate each dependency exists, then queue as 'pending' until
			// promoted, or 'ready' when it has deps but... — parity: no deps keeps
			// the legacy 'queued' status; deps => validate + 'pending'.
			if len(deps) > 0 {
				for _, dep := range deps {
					var got string
					if err := db.QueryRow(`SELECT id FROM queue_jobs WHERE id=?`, dep).Scan(&got); err != nil {
						if errors.Is(err, sql.ErrNoRows) {
							return fmt.Errorf("Dependency job not found: %q", dep)
						}
						return err
					}
				}
				id := queueHexID(16)
				depsJSON, _ := json.Marshal(deps)
				_, err = db.Exec(`INSERT INTO queue_jobs(id,profile,task,task_type,status,priority,created_at,notify,deps_json)
					VALUES(?,?,?,?,'pending',?,?,1,?)`, id, app.profile(profile), task, taskType, priority, time.Now().UTC().Format(time.RFC3339), string(depsJSON))
				if err != nil {
					return err
				}
				outJSON(map[string]any{"job_id": id, "status": "pending", "depends_on": deps},
					fmt.Sprintf("Queue job added: %s  (pending on dependencies — run `tag dag show` to inspect)", id))
				return nil
			}
			// Match Python cmd_queue add: uuid.uuid4().hex[:8], JSON key "job_id".
			id := queueHexID(8)
			_, err = db.Exec(`INSERT INTO queue_jobs(id,profile,task,task_type,status,priority,created_at,notify,deps_json)
				VALUES(?,?,?,?,'queued',?,?,1,'[]')`, id, app.profile(profile), task, taskType, priority, time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			outJSON(map[string]any{"job_id": id, "status": "queued"}, "queued: "+id)
			return nil
		}}
	add.Flags().StringVar(&profile, "profile", "", "profile")
	add.Flags().StringVar(&taskType, "task-type", "mixed", "task type")
	add.Flags().IntVar(&priority, "priority", 5, "priority")
	add.Flags().StringArrayVar(&deps, "dep", nil, "prerequisite job ID (repeatable)")

	var listStatus string
	var listLimit int
	list := &cobra.Command{Use: "list", Short: "List jobs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Honor an explicit 0 (show none) and reject negatives, mirroring
			// Python cmd_queue list (B047/B087).
			if listLimit < 0 {
				msg := fmt.Sprintf("--limit must be >= 0, got %d.", listLimit)
				if flagJSON {
					b, _ := json.Marshal(map[string]any{"error": msg})
					fmt.Println(string(b))
				} else {
					fmt.Fprintf(os.Stderr, "error: %s\n", msg)
				}
				os.Exit(1)
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			// Match Python queue_list_jobs: optional status filter, ORDER BY
			// created_at DESC, parametrized LIMIT.
			query := `SELECT id,status,priority,task FROM queue_jobs WHERE 1=1`
			var qargs []any
			if listStatus != "" {
				query += ` AND status=?`
				qargs = append(qargs, listStatus)
			}
			query += ` ORDER BY created_at DESC LIMIT ?`
			qargs = append(qargs, listLimit)
			rows, err := db.Query(query, qargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			items := []map[string]any{}
			for rows.Next() {
				var id, st, task string
				var pr int
				if err := rows.Scan(&id, &st, &pr, &task); err != nil {
					return err
				}
				items = append(items, map[string]any{"id": id, "status": st, "priority": pr, "task": task})
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				b, _ := json.Marshal(items)
				fmt.Println(string(b))
			} else if len(items) == 0 {
				fmt.Println("Queue is empty.")
			} else {
				for _, it := range items {
					fmt.Printf("%s  [%s]  p%v  %s\n", it["id"], it["status"], it["priority"], truncate(it["task"].(string), 50))
				}
			}
			return nil
		}}
	list.Flags().StringVar(&listStatus, "status", "", "filter by status")
	list.Flags().IntVar(&listLimit, "limit", 50, "max jobs to show (default: 50)")
	cancel := &cobra.Command{Use: "cancel ID", Short: "Cancel a queued or running job", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			// Cancel any non-terminal job (queued OR running), mirroring Python
			// cmd_queue's cancel which rejects only already-terminal jobs. Go runs
			// jobs in-process via internal/worker (no separate PID to SIGTERM), so
			// flipping status to 'cancelled' is the Go-model equivalent of the
			// Python os.kill(pid, SIGTERM) + status flip.
			r, err := db.Exec(`UPDATE queue_jobs SET status='cancelled' WHERE id=? AND status NOT IN ('done','failed','cancelled','timed_out')`, args[0])
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n == 0 {
				return fmt.Errorf("job not found or not cancellable: %s", args[0])
			}
			outJSON(map[string]any{"job_id": args[0], "status": "cancelled"}, "cancelled: "+args[0])
			return nil
		}}
	result := &cobra.Command{Use: "result ID", Short: "Show output of a completed job", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var status, resultPath string
			err = db.QueryRow(`SELECT status, COALESCE(result_path,'') FROM queue_jobs WHERE id=?`, args[0]).Scan(&status, &resultPath)
			if err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if flagJSON {
					b, _ := json.Marshal(map[string]any{"error": fmt.Sprintf("job %s not found", args[0]), "job_id": args[0]})
					fmt.Println(string(b))
				} else {
					fmt.Fprintf(os.Stderr, "Job '%s' not found.\n", args[0])
				}
				os.Exit(1)
			}
			var content any
			if resultPath != "" {
				if data, rerr := os.ReadFile(resultPath); rerr == nil {
					content = string(data)
				}
			}
			// Fall back to the inline `result` column the native worker populates
			// (internal/worker). The column is added on first worker run, so guard
			// against it being absent on DBs the worker never touched.
			if content == nil {
				var inline string
				if rerr := db.QueryRow(`SELECT COALESCE(result,'') FROM queue_jobs WHERE id=?`, args[0]).Scan(&inline); rerr == nil && inline != "" {
					content = inline
				}
			}
			if flagJSON {
				var rp any
				if resultPath != "" {
					rp = resultPath
				}
				b, _ := json.Marshal(map[string]any{"job_id": args[0], "status": status,
					"result_path": rp, "result": content})
				fmt.Println(string(b))
				return nil
			}
			if content != nil {
				fmt.Println(content)
			} else {
				fmt.Printf("No result yet (status: %s)\n", status)
			}
			return nil
		}}
	clear := &cobra.Command{Use: "clear", Short: "Remove completed/failed jobs from list", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			// Keep terminal jobs that are still referenced by a non-terminal
			// dependent: the dependency check treats a missing dep as forever
			// unsatisfied, so deleting a completed parent would strand its
			// pending children.
			rows, err := db.Query(`SELECT COALESCE(deps_json,'[]') FROM queue_jobs
				WHERE status NOT IN ('done','failed','cancelled','timed_out')`)
			if err != nil {
				return err
			}
			referenced := map[string]bool{}
			for rows.Next() {
				var depsJSON string
				if err := rows.Scan(&depsJSON); err != nil {
					rows.Close()
					return err
				}
				var ds []string
				json.Unmarshal([]byte(depsJSON), &ds)
				for _, d := range ds {
					referenced[d] = true
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			del := `DELETE FROM queue_jobs WHERE status IN ('done','failed','cancelled')`
			var qargs []any
			if len(referenced) > 0 {
				ph := strings.TrimSuffix(strings.Repeat("?,", len(referenced)), ",")
				del += ` AND id NOT IN (` + ph + `)`
				for id := range referenced {
					qargs = append(qargs, id)
				}
			}
			var kept int
			if len(referenced) > 0 {
				ph := strings.TrimSuffix(strings.Repeat("?,", len(referenced)), ",")
				if err := db.QueryRow(`SELECT COUNT(*) FROM queue_jobs WHERE status IN ('done','failed','cancelled') AND id IN (`+ph+`)`, qargs...).Scan(&kept); err != nil {
					return err
				}
			}
			r, err := db.Exec(del, qargs...)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			msg := fmt.Sprintf("cleared %d completed/failed jobs", n)
			if kept > 0 {
				msg += fmt.Sprintf(" (kept %d still referenced by active jobs)", kept)
			}
			outJSON(map[string]any{"cleared": n, "kept": kept}, msg)
			return nil
		}}
	// worker: drain ready jobs and run each through the native agent loop.
	var wProvider string
	var wMax int
	var wWatch, wTools bool
	workerCmd := &cobra.Command{Use: "worker", Short: "Execute queued/ready jobs through the agent loop", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			opts, err := buildWorkerOptions(app, wProvider, wMax, wWatch, wTools)
			if err != nil {
				return err
			}
			// Cancel on SIGINT/SIGTERM (mirroring the cron daemon) so an
			// interrupted in-flight job is recorded as failed instead of being
			// left in 'running' forever.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			sum, err := worker.Drain(ctx, db.DB, opts)
			if err != nil {
				return err
			}
			outJSON(map[string]any{"claimed": sum.Claimed, "done": sum.Done, "failed": sum.Failed, "skipped": sum.Skipped},
				fmt.Sprintf("worker: %d claimed, %d done, %d failed, %d skipped", sum.Claimed, sum.Done, sum.Failed, sum.Skipped))
			return nil
		}}
	workerCmd.Flags().StringVar(&wProvider, "provider", "echo", "llm provider (echo = offline)")
	workerCmd.Flags().IntVar(&wMax, "max", 0, "max jobs to run (0 = unlimited)")
	workerCmd.Flags().BoolVar(&wWatch, "watch", false, "keep polling for new jobs")
	workerCmd.Flags().BoolVar(&wTools, "tools", false, "enable built-in tools (bash/read_file/write_file/list_dir)")

	q.AddCommand(add, list, cancel, result, clear, workerCmd)

	// ---- dag ----
	var specJSON string
	dag := &cobra.Command{Use: "dag", Short: "DAG workflow engine", GroupID: "orch"}
	save := &cobra.Command{Use: "save NAME", Short: "Save a DAG spec", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return jsonErrorMaybe(fmt.Errorf("DAG name must not be empty"))
			}
			var steps []map[string]any
			if err := json.Unmarshal([]byte(specJSON), &steps); err != nil {
				return jsonErrorMaybe(fmt.Errorf("invalid --steps JSON: %w", err))
			}
			// Validate each step (mirrors dag.py:validate_dag_spec, incl. C032:
			// reject unknown dependency-alias keys that would silently drop edges).
			// The canonical dependency key is `depends_on` (matching the Python
			// engine, which is the only key run_dag reads); the alias set below is
			// rejected with a "use 'depends_on' instead" hint so a spec written for
			// either implementation is validated identically.
			recognized := map[string]bool{"name": true, "task": true, "depends_on": true, "profile": true, "task_type": true}
			depAliases := map[string]bool{"deps": true, "depends": true, "needs": true, "dependencies": true, "requires": true, "after": true}
			for i, s := range steps {
				taskVal, ok := s["task"]
				if !ok {
					return jsonErrorMaybe(fmt.Errorf("step %d is missing required non-empty 'task'", i))
				}
				taskStr, isStr := taskVal.(string)
				if !isStr || strings.TrimSpace(taskStr) == "" {
					return jsonErrorMaybe(fmt.Errorf("step %d 'task' must be a non-empty string", i))
				}
				// Reject any unrecognized key; give the dependency-alias hint when the
				// unknown key is a known alias (it would silently drop the edge).
				for k := range s {
					if recognized[k] {
						continue
					}
					if depAliases[k] {
						return jsonErrorMaybe(fmt.Errorf("step %d uses unrecognized dependency key %q; use 'depends_on' instead", i, k))
					}
					return jsonErrorMaybe(fmt.Errorf("step %d has unrecognized key %q; allowed keys are [depends_on name profile task task_type]", i, k))
				}
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			spec, _ := json.Marshal(map[string]any{"name": args[0], "steps": steps})
			_, err = db.Exec(`INSERT INTO queue_dags(id,name,spec_json,created_at) VALUES(?,?,?,?)
				ON CONFLICT(name) DO UPDATE SET spec_json=excluded.spec_json`, uuid.NewString()[:12], args[0], string(spec), time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			fmt.Printf("DAG '%s' saved (%d steps)\n", args[0], len(steps))
			return nil
		}}
	save.Flags().StringVar(&specJSON, "steps", "[]", "JSON step array")
	dagList := &cobra.Command{Use: "list", Short: "List DAGs",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT name,spec_json FROM queue_dags ORDER BY name`)
			if err != nil {
				return err
			}
			defer rows.Close()
			items := []map[string]any{}
			for rows.Next() {
				var nm, spec string
				if err := rows.Scan(&nm, &spec); err != nil {
					return err
				}
				var sp map[string]any
				json.Unmarshal([]byte(spec), &sp)
				steps, _ := sp["steps"].([]any)
				items = append(items, map[string]any{"name": nm, "steps": len(steps)})
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				b, _ := json.Marshal(items)
				fmt.Println(string(b))
				return nil
			}
			if len(items) == 0 {
				fmt.Println("No DAGs.")
				return nil
			}
			for _, it := range items {
				fmt.Printf("%-30s %d steps\n", it["name"], it["steps"])
			}
			return nil
		}}
	dagShow := &cobra.Command{Use: "show [JOB_ID...]", Short: "Show job dependency graph",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			query := `SELECT id,task,task_type,profile,status,COALESCE(deps_json,'[]'),created_at FROM queue_jobs`
			var qargs []any
			if len(args) > 0 {
				ph := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
				query += ` WHERE id IN (` + ph + `)`
				for _, a := range args {
					qargs = append(qargs, a)
				}
			} else {
				query += ` ORDER BY created_at LIMIT 50`
			}
			rows, err := db.Query(query, qargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			type jrec struct {
				id, task, ttype, profile, status, deps, created string
			}
			var recs []jrec
			for rows.Next() {
				var r jrec
				if err := rows.Scan(&r.id, &r.task, &r.ttype, &r.profile, &r.status, &r.deps, &r.created); err != nil {
					return err
				}
				recs = append(recs, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				items := []map[string]any{}
				for _, r := range recs {
					var deps []string
					json.Unmarshal([]byte(r.deps), &deps)
					if deps == nil {
						deps = []string{}
					}
					items = append(items, map[string]any{"id": r.id, "task": r.task,
						"task_type": r.ttype, "profile": r.profile, "status": r.status,
						"deps": deps, "created_at": r.created})
				}
				b, _ := json.MarshalIndent(items, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(recs) == 0 {
				fmt.Println("No jobs found.")
				return nil
			}
			icons := map[string]string{"ready": "⏳", "running": "▶", "done": "✓", "failed": "✗",
				"pending": "○", "cancelled": "⊘", "queued": "•", "timed_out": "⌛", "skipped": "–"}
			fmt.Println("Job Dependency Graph")
			fmt.Println(strings.Repeat("=", 40))
			for _, r := range recs {
				icon := icons[r.status]
				if icon == "" {
					icon = "?"
				}
				var deps []string
				json.Unmarshal([]byte(r.deps), &deps)
				depStr := ""
				if len(deps) > 0 {
					short := make([]string, len(deps))
					for i, d := range deps {
						short[i] = truncate(d, 8)
					}
					depStr = " ← [" + strings.Join(short, ", ") + "]"
				}
				fmt.Printf("%s %-12s [%-8s] %s%s\n", icon, truncate(r.id, 12), r.status, truncate(r.task, 50), depStr)
			}
			return nil
		}}

	var runBoard, dagProvider string
	var dagExecute, dagTools bool
	dagRun := &cobra.Command{Use: "run NAME", Short: "Submit a named DAG", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var specJSON string
			if err := db.QueryRow(`SELECT spec_json FROM queue_dags WHERE name=?`, args[0]).Scan(&specJSON); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("DAG not found: %q", args[0])
				}
				return err
			}
			var spec struct {
				Steps []map[string]any `json:"steps"`
			}
			if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
				return fmt.Errorf("DAG %q has malformed spec: %w", args[0], err)
			}
			// Pre-index step names for name-based dependency references.
			nameToIdx := map[string]int{}
			for i, s := range spec.Steps {
				if _, ok := s["task"]; !ok {
					return fmt.Errorf("DAG %q step %d is missing required 'task'", args[0], i)
				}
				if nm, ok := s["name"].(string); ok && nm != "" {
					nameToIdx[nm] = i
				}
			}
			now := time.Now().UTC().Format(time.RFC3339)
			var submitted []string
			var ready, pending []string
			for i, s := range spec.Steps {
				var depIdx []int
				if raw, ok := s["depends_on"]; ok && raw != nil {
					list, isList := raw.([]any)
					if !isList {
						return fmt.Errorf("DAG %q step %d 'depends_on' must be a list", args[0], i)
					}
					for _, ref := range list {
						var idx int
						switch v := ref.(type) {
						case float64:
							idx = int(v)
						case string:
							j, ok := nameToIdx[v]
							if !ok {
								return fmt.Errorf("DAG %q step %d depends on unknown step %q", args[0], i, v)
							}
							idx = j
						default:
							return fmt.Errorf("DAG %q step %d has an invalid dependency %v", args[0], i, ref)
						}
						if idx == i {
							return fmt.Errorf("DAG %q step %d cannot depend on itself", args[0], i)
						}
						if idx < 0 || idx >= i {
							return fmt.Errorf("DAG %q step %d depends on step %v, which is not an earlier step", args[0], i, ref)
						}
						depIdx = append(depIdx, idx)
					}
				}
				depIDs := make([]string, 0, len(depIdx))
				for _, di := range depIdx {
					depIDs = append(depIDs, submitted[di])
				}
				status := "ready"
				if len(depIDs) > 0 {
					status = "pending"
				}
				id := queueHexID(16)
				profileVal, _ := s["profile"].(string)
				if profileVal == "" {
					profileVal = "default"
				}
				taskType, _ := s["task_type"].(string)
				if taskType == "" {
					taskType = "mixed"
				}
				depsJSON, _ := json.Marshal(depIDs)
				_, err := db.Exec(`INSERT INTO queue_jobs(id,profile,task,task_type,status,priority,created_at,notify,deps_json)
					VALUES(?,?,?,?,?,5,?,1,?)`, id, profileVal, strings.ReplaceAll(str(s["task"]), "\x00", ""), taskType, status, now, string(depsJSON))
				if err != nil {
					return err
				}
				submitted = append(submitted, id)
				if status == "ready" {
					ready = append(ready, id)
				} else {
					pending = append(pending, id)
				}
			}
			if submitted == nil {
				submitted = []string{}
			}
			if ready == nil {
				ready = []string{}
			}
			if pending == nil {
				pending = []string{}
			}
			// Optional execution: after enqueuing, drain the jobs through the agent
			// loop so `dag run --execute` actually runs work. Default stays
			// enqueue-only (offline parity) unless --execute is given.
			var execSummary *worker.Summary
			if dagExecute {
				opts, err := buildWorkerOptions(app, dagProvider, 0, false, dagTools)
				if err != nil {
					return err
				}
				opts.OnlyJobs = submitted
				s, err := worker.Drain(context.Background(), db.DB, opts)
				if err != nil {
					return err
				}
				execSummary = &s
			}
			if flagJSON {
				payload := map[string]any{"dag": args[0], "submitted": submitted,
					"dispatched": ready, "pending": pending}
				if execSummary != nil {
					payload["executed"] = map[string]any{"claimed": execSummary.Claimed, "done": execSummary.Done,
						"failed": execSummary.Failed, "skipped": execSummary.Skipped}
				}
				b, _ := json.Marshal(payload)
				fmt.Println(string(b))
				return nil
			}
			// Offline (no managed runtime): jobs with no unmet deps are marked
			// 'ready'; dependents stay 'pending' until their parents reach 'done'.
			fmt.Printf("DAG '%s' submitted: %d jobs (%d ready, %d pending on dependencies)\n",
				args[0], len(submitted), len(ready), len(pending))
			for _, id := range ready {
				fmt.Printf("  %s  (ready)\n", id)
			}
			for _, id := range pending {
				fmt.Printf("  %s  (pending on dependencies)\n", id)
			}
			if execSummary != nil {
				fmt.Printf("executed: %d claimed, %d done, %d failed, %d skipped\n",
					execSummary.Claimed, execSummary.Done, execSummary.Failed, execSummary.Skipped)
			}
			return nil
		}}
	dagRun.Flags().StringVar(&runBoard, "board", "default", "board")
	dagRun.Flags().BoolVar(&dagExecute, "execute", false, "run enqueued jobs through the agent loop after submitting")
	dagRun.Flags().StringVar(&dagProvider, "provider", "echo", "llm provider for --execute (echo = offline)")
	dagRun.Flags().BoolVar(&dagTools, "tools", false, "enable built-in tools for --execute")

	dag.AddCommand(save, dagList, dagShow, dagRun)
	root.AddCommand(q, dag)
}

// buildWorkerOptions resolves a worker.Options from CLI flags. The provider is
// looked up in the llm registry (echo = offline default); the per-job model is
// resolved from profiles.<profile>.config.model.default.
func buildWorkerOptions(app *App, provider string, max int, watch, tools bool) (worker.Options, error) {
	prov, ok := llm.Registry[provider]
	if !ok {
		return worker.Options{}, fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
	}
	return worker.Options{
		Provider: prov,
		ModelForProfile: func(p string) string {
			return app.Cfg.String("profiles."+p+".config.model.default", "")
		},
		WithTools: tools,
		MaxJobs:   max,
		Watch:     watch,
	}, nil
}
