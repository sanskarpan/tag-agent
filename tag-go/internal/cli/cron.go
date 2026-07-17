package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/cron"
	"github.com/tag-agent/tag/internal/worker"
)

func registerCron(root *cobra.Command, app *App) {
	var name, schedule, profile string
	c := &cobra.Command{Use: "cron", Short: "Cron-style scheduled agent runs", GroupID: "orch"}

	add := &cobra.Command{Use: "add TASK", Short: "Add a scheduled job", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || schedule == "" {
				return fmt.Errorf("--name and --schedule required")
			}
			if err := cron.Validate(schedule); err != nil {
				return err
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			id := uuid.NewString()[:8]
			_, err = db.Exec(`INSERT INTO cron_jobs(id,name,schedule,task,profile,enabled,created_at,run_count) VALUES(?,?,?,?,?,1,?,0)`,
				id, name, schedule, args[0], app.profile(profile), time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			outJSON(map[string]any{"id": id, "name": name}, fmt.Sprintf("cron job added: %s  %q  [%s]", id, name, schedule))
			return nil
		}}
	add.Flags().StringVar(&name, "name", "", "job name")
	add.Flags().StringVar(&schedule, "schedule", "", "5-field cron expr")
	add.Flags().StringVar(&profile, "profile", "", "profile")

	list := &cobra.Command{Use: "list", Short: "List cron jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id,name,schedule,enabled,run_count FROM cron_jobs ORDER BY created_at`)
			if err != nil {
				return err
			}
			defer rows.Close()
			items := []map[string]any{}
			for rows.Next() {
				var id, nm, sc string
				var en, rc int
				if err := rows.Scan(&id, &nm, &sc, &en, &rc); err != nil {
					return err
				}
				items = append(items, map[string]any{"id": id, "name": nm, "schedule": sc,
					"enabled": en != 0, "run_count": rc})
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
				fmt.Println("No cron jobs.")
				return nil
			}
			for _, it := range items {
				st := "✓"
				if it["enabled"] == false {
					st = "✗"
				}
				fmt.Printf("%s %s  %-24s [%s]  runs=%d\n", st, it["id"], it["name"], it["schedule"], it["run_count"])
			}
			return nil
		}}
	remove := &cobra.Command{Use: "remove ID", Short: "Remove a cron job", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			r, err := db.Exec(`DELETE FROM cron_jobs WHERE id=?`, args[0])
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n == 0 {
				return fmt.Errorf("cron job not found: %s", args[0])
			}
			fmt.Println("removed")
			return nil
		}}
	next := &cobra.Command{Use: "next EXPR", Short: "Validate an expr and show next 3 fire times", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cron.Validate(args[0]); err != nil {
				return err
			}
			t := time.Now().Truncate(time.Minute)
			found := 0
			for i := 0; i < 366*24*60 && found < 3; i++ {
				t = t.Add(time.Minute)
				if cron.Matches(args[0], t) {
					fmt.Println(t.Format("2006-01-02 15:04"))
					found++
				}
			}
			return nil
		}}
	setEnabled := func(id string, enabled int, verb string) error {
		db, err := app.OpenDB()
		if err != nil {
			return err
		}
		r, err := db.Exec(`UPDATE cron_jobs SET enabled=? WHERE id=?`, enabled, id)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			return fmt.Errorf("Job '%s' not found", id)
		}
		fmt.Printf("%s: %s\n", verb, id)
		return nil
	}
	enable := &cobra.Command{Use: "enable ID", Short: "Enable a cron job", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return setEnabled(args[0], 1, "enabled") }}
	disable := &cobra.Command{Use: "disable ID", Short: "Disable a cron job", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return setEnabled(args[0], 0, "disabled") }}

	var cronExecute, cronTools bool
	var cronProvider string
	run := &cobra.Command{Use: "run ID", Short: "Trigger a cron job immediately (ignore schedule)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var profileVal, task string
			if err := db.QueryRow(`SELECT profile, task FROM cron_jobs WHERE id=?`, args[0]).Scan(&profileVal, &task); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("Job '%s' not found", args[0])
				}
				return err
			}
			now := time.Now().UTC().Format(time.RFC3339)
			qID := uuid.NewString()[:8]
			if _, err := db.Exec(`INSERT INTO queue_jobs(id,profile,task,task_type,status,priority,created_at,notify,deps_json)
				VALUES(?,?,?,?,'queued',5,?,1,'[]')`, qID, profileVal, task, "mixed", now); err != nil {
				return err
			}
			if _, err := db.Exec(`UPDATE cron_jobs SET last_run=?, run_count=run_count+1 WHERE id=?`, now, args[0]); err != nil {
				return err
			}
			if cronExecute {
				opts, err := buildWorkerOptions(app, cronProvider, 0, false, cronTools)
				if err != nil {
					return err
				}
				opts.OnlyJobs = []string{qID}
				sum, err := worker.Drain(context.Background(), db.DB, opts)
				if err != nil {
					return err
				}
				fmt.Printf("triggered: cron job %s → queue job %s (executed: %d done, %d failed)\n",
					args[0], qID, sum.Done, sum.Failed)
				return nil
			}
			// Default: enqueue-only (no worker launched).
			fmt.Printf("triggered: cron job %s → queue job %s (queued)\n", args[0], qID)
			return nil
		}}
	run.Flags().BoolVar(&cronExecute, "execute", false, "run the enqueued job through the agent loop after triggering")
	run.Flags().StringVar(&cronProvider, "provider", "echo", "llm provider for --execute (echo = offline)")
	run.Flags().BoolVar(&cronTools, "tools", false, "enable built-in tools for --execute")

	daemon := &cobra.Command{Use: "daemon", Short: "Run the cron daemon in-process (blocking)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			// Cancellable poll loop: derive a context from the command's that is
			// cancelled on SIGINT/SIGTERM so the 30s wait is interruptible rather
			// than an un-cancellable time.Sleep.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			fmt.Println("TAG cron daemon starting (polling every 30s) — Ctrl+C to stop")
			// Offline poller: on each tick, enqueue any enabled job whose schedule
			// matches the current minute. No worker is launched (Go build has no
			// managed runtime), so due jobs land in queue_jobs as 'queued'.
			for {
				t := time.Now().Truncate(time.Minute)
				rows, err := db.Query(`SELECT id, schedule, profile, task, COALESCE(last_run,'') FROM cron_jobs WHERE enabled=1`)
				if err != nil {
					return err
				}
				type due struct{ id, profile, task string }
				var dueJobs []due
				var scanErr error
				for rows.Next() {
					var id, sched, profileVal, task, lastRun string
					if err := rows.Scan(&id, &sched, &profileVal, &task, &lastRun); err != nil {
						scanErr = err
						break
					}
					if !cron.Matches(sched, t) {
						continue
					}
					// Guard against firing twice in the same calendar minute: the poll
					// cadence (30s) is finer than the schedule resolution (1 minute),
					// so skip a job whose last_run already falls in this minute
					// (mirrors cron_scheduler.py fix C031).
					if lastRun != "" {
						if lr, perr := time.Parse(time.RFC3339, lastRun); perr == nil && lr.Truncate(time.Minute).Equal(t) {
							continue
						}
					}
					dueJobs = append(dueJobs, due{id, profileVal, task})
				}
				rows.Close()
				if scanErr != nil {
					return scanErr
				}
				if err := rows.Err(); err != nil {
					return err
				}
				now := time.Now().UTC().Format(time.RFC3339)
				for _, d := range dueJobs {
					qID := uuid.NewString()[:8]
					// Only report the enqueue and bump run_count when the INSERT
					// actually succeeds; a failed insert must not be counted as a run.
					if _, err := db.Exec(`INSERT INTO queue_jobs(id,profile,task,task_type,status,priority,created_at,notify,deps_json)
						VALUES(?,?,?,?,'queued',5,?,1,'[]')`, qID, d.profile, d.task, "mixed", now); err != nil {
						fmt.Fprintf(os.Stderr, "  cron %s: enqueue failed: %v\n", d.id, err)
						continue
					}
					if _, err := db.Exec(`UPDATE cron_jobs SET last_run=?, run_count=run_count+1 WHERE id=?`, now, d.id); err != nil {
						fmt.Fprintf(os.Stderr, "  cron %s: run_count update failed: %v\n", d.id, err)
					}
					fmt.Printf("  enqueued %s → queue job %s\n", d.id, qID)
				}
				select {
				case <-ctx.Done():
					fmt.Println("cron daemon stopping")
					return nil
				case <-time.After(30 * time.Second):
				}
			}
		}}

	c.AddCommand(add, list, remove, next, enable, disable, run, daemon)
	root.AddCommand(c)
}
