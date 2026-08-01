package cli

import (
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
	"github.com/tag-agent/tag/internal/permission"
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
	var wPerms permFlags
	workerCmd := &cobra.Command{Use: "worker", Short: "Execute queued/ready jobs through the agent loop", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			opts, err := buildWorkerOptions(app, wProvider, wMax, wWatch, wTools, &wPerms)
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
	wPerms.bind(workerCmd)

	q.AddCommand(add, list, cancel, result, clear, workerCmd)

	root.AddCommand(q)
	// PRD-112: the `dag` group (save/run/show/list/state, conditional edges and
	// state reducers) lives in dag.go — it outgrew this file. It is registered
	// from here so the orchestration commands still come up together.
	registerDAG(root, app)
}

// buildWorkerOptions resolves a worker.Options from CLI flags. The provider is
// looked up in the llm registry (echo = offline default); the per-job model is
// resolved from profiles.<profile>.config.model.default.
//
// The permission guard built here is ALWAYS non-interactive (noPrompt), even if
// the operator happens to have a terminal: `queue worker`, `dag run --execute`
// and `cron run --execute` drain jobs in the background, and stopping a drain to
// wait on a human is exactly the silent hang this project forbids. An `ask`
// therefore resolves to a recorded deny; grant with --allow-tool/--auto-approve
// or a permissions block in config.yaml.
func buildWorkerOptions(app *App, provider string, max int, watch, tools bool, perms *permFlags) (worker.Options, error) {
	prov, ok := llm.Registry[provider]
	if !ok {
		return worker.Options{}, fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
	}
	var guard *permission.Guard
	if tools {
		pf := permFlags{}
		if perms != nil {
			pf = *perms
		}
		pf.noPrompt = true
		g, err := pf.guard(app, "", "", os.Stderr)
		if err != nil {
			return worker.Options{}, err
		}
		guard = g
	}
	return worker.Options{
		Guard:    guard,
		Provider: prov,
		ModelForProfile: func(p string) string {
			return app.Cfg.String("profiles."+p+".config.model.default", "")
		},
		WithTools: tools,
		MaxJobs:   max,
		Watch:     watch,
	}, nil
}
