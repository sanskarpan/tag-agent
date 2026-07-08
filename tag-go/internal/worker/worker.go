// Package worker adds an execution runtime for the queue/dag/cron subsystem
// (issue #532). The Go CLI historically only *enqueued* jobs into queue_jobs;
// this package drains those jobs and actually runs each one through the native
// agent loop (internal/agent) against a provider-neutral llm.Provider.
//
// It mirrors the Python controller/dag semantics (src/tag/dag.py,
// src/tag/controller.py):
//   - a job runs only when every dependency in deps_json has reached the
//     terminal 'done' status;
//   - a job whose dependency reached a *failed* terminal status ('failed',
//     'cancelled', 'timed_out') is cascade-failed rather than left pending;
//   - claiming is atomic (compare-and-set on status) so concurrent drains never
//     double-execute a job.
//
// The default provider is the offline llm.EchoProvider, so Drain is fully
// exercisable without network access or API keys.
package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/tool"
)

// claimableStatuses are the pre-execution statuses a job can be drained from.
// 'queued' is used by `queue add` (no deps); 'ready'/'pending' are used by the
// DAG engine (dag run). A job in any of these becomes 'running' when claimed.
var claimableStatuses = []string{"queued", "ready", "pending"}

// failedDepStatuses are terminal statuses that mean a dependency can never reach
// 'done', so a dependent must be cascade-failed (mirrors dag.py _FAILED_DEP_STATUSES).
var failedDepStatuses = map[string]bool{"failed": true, "cancelled": true, "timed_out": true}

// staleClaimLease is how long a job may sit in 'running' before it is treated
// as abandoned (worker crash/SIGKILL) and requeued for another drainer.
const staleClaimLease = 30 * time.Minute

// finishTimeout bounds the terminal-status write, which must succeed even when
// the drain context has already been cancelled.
const finishTimeout = 10 * time.Second

// Summary reports the outcome of a Drain.
type Summary struct {
	Claimed int `json:"claimed"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// Options configures Drain.
type Options struct {
	// Provider is the LLM provider the agent loop runs against. Defaults to the
	// offline echo provider when nil (safe for tests / no keys).
	Provider llm.Provider
	// Model is the fallback model id passed to the agent loop.
	Model string
	// ModelForProfile, when set, resolves a per-job model from its profile
	// (e.g. profiles.<p>.config.model.default). A non-empty result overrides Model.
	ModelForProfile func(profile string) string
	// System is an optional system prompt for every job.
	System string
	// MaxSteps caps agent-loop turns per job (0 = loop default).
	MaxSteps int
	// WithTools enables the built-in tools (bash/read_file/write_file/list_dir).
	WithTools bool
	// MaxJobs caps how many jobs are claimed in this Drain (0 = unlimited).
	MaxJobs int
	// OnlyJobs, when non-empty, restricts this Drain to the given job ids;
	// other claimable jobs in the queue are left untouched.
	OnlyJobs []string
	// Watch keeps polling instead of returning after the queue is drained.
	Watch bool
	// PollInterval is the Watch poll cadence (default 2s).
	PollInterval time.Duration
}

// Drain executes ready jobs. In RunOnce mode (Watch=false) it repeatedly drains
// passes until a pass claims nothing (so DAG dependency chains resolve fully),
// then returns. In Watch mode it loops on PollInterval until ctx is cancelled or
// MaxJobs is reached.
func Drain(ctx context.Context, db *sql.DB, opts Options) (Summary, error) {
	if opts.Provider == nil {
		opts.Provider = llm.EchoProvider{}
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if err := ensureResultColumn(db); err != nil {
		return Summary{}, err
	}

	var sum Summary
	for {
		if err := ctx.Err(); err != nil {
			return sum, nil
		}
		claimed, err := drainPass(ctx, db, opts, &sum)
		if err != nil {
			return sum, err
		}
		if opts.MaxJobs > 0 && sum.Claimed >= opts.MaxJobs {
			return sum, nil
		}
		if !opts.Watch {
			if claimed == 0 {
				return sum, nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			return sum, nil
		case <-time.After(opts.PollInterval):
		}
	}
}

// jobRow is a claimable job read from queue_jobs.
type jobRow struct {
	id      string
	profile string
	task    string
	deps    []string
}

// drainPass performs one drain pass and returns how many jobs it claimed.
// Cascade-fail and skip counts are folded into sum; Skipped reflects the jobs
// still blocked at the end of this pass (so the terminal pass reports the true
// blocked set rather than accumulating across passes).
func drainPass(ctx context.Context, db *sql.DB, opts Options, sum *Summary) (int, error) {
	if err := reclaimStale(ctx, db); err != nil {
		return 0, err
	}
	// Read all claimable jobs first, then close the cursor before issuing any
	// writes: the store uses a single writer connection, so an open SELECT
	// cursor would deadlock a concurrent UPDATE on the same conn.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(claimableStatuses)), ",")
	q := `SELECT id, profile, task, COALESCE(deps_json,'[]') FROM queue_jobs
	      WHERE status IN (` + placeholders + `) ORDER BY priority DESC, created_at ASC, id ASC`
	args := make([]any, len(claimableStatuses))
	for i, s := range claimableStatuses {
		args[i] = s
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	var jobs []jobRow
	for rows.Next() {
		var j jobRow
		var depsJSON string
		if err := rows.Scan(&j.id, &j.profile, &j.task, &depsJSON); err != nil {
			rows.Close()
			return 0, err
		}
		_ = json.Unmarshal([]byte(depsJSON), &j.deps)
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	var only map[string]bool
	if len(opts.OnlyJobs) > 0 {
		only = make(map[string]bool, len(opts.OnlyJobs))
		for _, id := range opts.OnlyJobs {
			only[id] = true
		}
	}

	claimedThisPass := 0
	skippedThisPass := 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		if opts.MaxJobs > 0 && sum.Claimed >= opts.MaxJobs {
			break
		}
		if only != nil && !only[j.id] {
			continue
		}
		if len(j.deps) > 0 {
			satisfied, failedDep, failedStatus, err := depState(ctx, db, j.deps)
			if err != nil {
				return claimedThisPass, err
			}
			if failedDep != "" {
				// Cascade-fail: a dependency reached a non-recoverable terminal state.
				if ok, err := cascadeFail(ctx, db, j.id, fmt.Sprintf("dependency %s %s", failedDep, failedStatus)); err != nil {
					return claimedThisPass, err
				} else if ok {
					sum.Failed++
				}
				continue
			}
			if !satisfied {
				skippedThisPass++
				continue
			}
		}
		// Atomic claim: compare-and-set status -> 'running'. RowsAffected!=1 means
		// another drainer already claimed it, so we move on without executing.
		claimed, err := claim(ctx, db, j.id)
		if err != nil {
			return claimedThisPass, err
		}
		if !claimed {
			continue
		}
		sum.Claimed++
		claimedThisPass++

		text, runErr := runJob(ctx, opts, j)
		if runErr != nil {
			applied, err := finish(db, j.id, "failed", "", runErr.Error())
			if err != nil {
				return claimedThisPass, err
			}
			if applied {
				sum.Failed++
			}
		} else {
			applied, err := finish(db, j.id, "done", text, "")
			if err != nil {
				return claimedThisPass, err
			}
			if applied {
				sum.Done++
			}
		}
	}
	sum.Skipped = skippedThisPass
	return claimedThisPass, nil
}

// depState reports whether every dep has reached 'done'. If any dep is in a
// terminal failed status it returns that dep id + status so the caller can
// cascade-fail. A missing dep counts as unsatisfied (never cascade-failed),
// matching dag.py which leaves such jobs pending.
func depState(ctx context.Context, db *sql.DB, deps []string) (satisfied bool, failedDep, failedStatus string, err error) {
	allDone := true
	for _, dep := range deps {
		var status string
		e := db.QueryRowContext(ctx, `SELECT status FROM queue_jobs WHERE id=?`, dep).Scan(&status)
		if e == sql.ErrNoRows {
			allDone = false
			continue
		}
		if e != nil {
			return false, "", "", e
		}
		if failedDepStatuses[status] {
			return false, dep, status, nil
		}
		if status != "done" {
			allDone = false
		}
	}
	return allDone, "", "", nil
}

// claim atomically transitions a job from a claimable status to 'running'.
func claim(ctx context.Context, db *sql.DB, id string) (bool, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(claimableStatuses)), ",")
	args := make([]any, 0, len(claimableStatuses)+2)
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	args = append(args, id)
	for _, s := range claimableStatuses {
		args = append(args, s)
	}
	r, err := db.ExecContext(ctx, `UPDATE queue_jobs SET status='running', started_at=?
		WHERE id=? AND status IN (`+placeholders+`)`, args...)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// cascadeFail marks a still-claimable job failed because a dependency failed.
func cascadeFail(ctx context.Context, db *sql.DB, id, reason string) (bool, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(claimableStatuses)), ",")
	args := make([]any, 0, len(claimableStatuses)+3)
	args = append(args, reason, time.Now().UTC().Format(time.RFC3339), id)
	for _, s := range claimableStatuses {
		args = append(args, s)
	}
	r, err := db.ExecContext(ctx, `UPDATE queue_jobs SET status='failed', error=?, finished_at=?
		WHERE id=? AND status IN (`+placeholders+`)`, args...)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// finish records the terminal state of a job that this drainer executed. It
// runs on its own bounded context so a cancelled drain can still persist the
// outcome, and only applies while the job is still 'running' (a concurrent
// `queue cancel` wins). Returns whether the update was applied.
func finish(db *sql.DB, id, status, result, errText string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	r, err := db.ExecContext(ctx, `UPDATE queue_jobs SET status=?, result=?, error=?, finished_at=?
		WHERE id=? AND status='running'`,
		status, result, errText, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// reclaimStale requeues jobs abandoned in 'running' (their claimer crashed or
// was killed before writing a terminal status) once their claim is older than
// staleClaimLease, so dependents are not blocked forever.
func reclaimStale(ctx context.Context, db *sql.DB) error {
	cutoff := time.Now().UTC().Add(-staleClaimLease).Format(time.RFC3339)
	_, err := db.ExecContext(ctx, `UPDATE queue_jobs SET status='queued', started_at=NULL
		WHERE status='running' AND started_at IS NOT NULL AND started_at < ?`, cutoff)
	return err
}

// runJob executes a job's task through the native agent loop.
func runJob(ctx context.Context, opts Options, j jobRow) (string, error) {
	model := opts.Model
	if opts.ModelForProfile != nil {
		if m := opts.ModelForProfile(j.profile); m != "" {
			model = m
		}
	}
	loop := &agent.Loop{Provider: opts.Provider}
	if opts.WithTools {
		reg := agent.NewRegistry()
		tool.Register(reg, tool.DefaultOptions())
		loop.Tools = reg
	}
	res, err := loop.Run(ctx, j.task, agent.Options{
		Model:    model,
		System:   opts.System,
		MaxSteps: opts.MaxSteps,
	})
	if err != nil {
		return "", err
	}
	return res.FinalText, nil
}

// ensureResultColumn self-heals the schema: it adds a `result TEXT` column to
// queue_jobs if the running DB predates it. schema.sql is intentionally left
// untouched (it is owned elsewhere); this keeps the worker package independent.
func ensureResultColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(queue_jobs)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "result" {
			found = true
		}
	}
	rows.Close()
	if found {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE queue_jobs ADD COLUMN result TEXT`); err != nil {
		// A concurrent drainer may have added the column between our check and
		// the ALTER; SQLite reports that as a duplicate-column error we can ignore.
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return nil
		}
		return err
	}
	return nil
}
