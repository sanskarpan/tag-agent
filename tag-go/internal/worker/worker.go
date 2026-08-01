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
	"github.com/tag-agent/tag/internal/paths"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/tool"
	"github.com/tag-agent/tag/internal/trace"
)

// claimableStatuses are the pre-execution statuses a job can be drained from.
// 'queued' is used by `queue add` (no deps); 'ready'/'pending' are used by the
// DAG engine (dag run). A job in any of these becomes 'running' when claimed.
var claimableStatuses = []string{"queued", "ready", "pending"}

// failedDepStatuses are terminal statuses that mean a dependency can never reach
// 'done', so a dependent must be cascade-failed (mirrors dag.py _FAILED_DEP_STATUSES).
var failedDepStatuses = map[string]bool{"failed": true, "cancelled": true, "timed_out": true}

// skippedDepStatus is the terminal status of a node whose conditional edge was
// false (PRD-112). It is NOT a failure — the branch was simply not taken — so a
// dependent is cascade-SKIPPED rather than cascade-failed. Without this a node
// behind an untaken branch would sit 'pending' forever, which is the silent hang
// this project forbids.
const skippedDepStatus = "skipped"

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
	// Skipped is how many jobs were still BLOCKED on unmet dependencies at the
	// end of the terminal pass. It is not a terminal outcome.
	Skipped int `json:"skipped"`
	// Pruned is how many jobs were terminally 'skipped' because a conditional
	// edge was false, or because they sat behind such a node (PRD-112). It is
	// reported separately from Skipped precisely because it IS terminal.
	Pruned int `json:"pruned"`
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
	// Guard is the tool consent gate. A worker is ALWAYS headless, so its guard
	// must never carry a prompter: an `ask` here resolves to an immediate deny
	// with a reason rather than blocking a queue drain forever. Nil falls back to
	// the secure default policy (also headless).
	Guard *permission.Guard
	// WorkRoot is the parent directory under which each job gets its OWN working
	// directory (<WorkRoot>/<job-id>), which becomes the tool root for that job.
	// Empty defaults to <TAG_HOME>/work. Jobs never share a working directory —
	// see jobWorkDir and #591.
	WorkRoot string
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
// passes until a pass makes NO PROGRESS (so DAG dependency chains resolve
// fully), then returns. In Watch mode it loops on PollInterval until ctx is
// cancelled or MaxJobs is reached.
//
// "Progress" is deliberately wider than "claimed a job": a pass that only
// cascade-failed or cascade-skipped dependents still changed the graph, and the
// nodes behind those can only be resolved by another pass. Keying the loop on
// claims alone left the tail of a cascade chain stranded in 'pending' — with
// PRD-112's conditional edges that is the common case (a fork's untaken branch
// is pruned, never claimed), and a job stuck 'pending' forever is exactly the
// silent hang this project forbids.
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
	if err := ensureFlowColumn(db); err != nil {
		return Summary{}, err
	}

	var sum Summary
	for {
		if err := ctx.Err(); err != nil {
			return sum, nil
		}
		progressed, err := drainPass(ctx, db, opts, &sum)
		if err != nil {
			return sum, err
		}
		if opts.MaxJobs > 0 && sum.Claimed >= opts.MaxJobs {
			return sum, nil
		}
		if !opts.Watch {
			if progressed == 0 {
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
	// flow is the PRD-112 workflow metadata (conditional edge, output key,
	// reducer). Nil for every job that predates the feature or was enqueued by
	// plain `queue add`, which is why the whole feature is opt-in per job.
	flow *Flow
	// flowErr is set when flow_json is present but unparseable. Such a job is
	// failed rather than run: its guard is unknown, and running it would be the
	// one outcome the guard might have forbidden.
	flowErr error
}

// drainPass performs one drain pass and returns how many jobs it MOVED to a
// terminal state, whether by executing them or by cascading a dependency's
// outcome onto them (see Drain on why claims alone are not enough).
// Cascade-fail and prune counts are folded into sum; Skipped reflects the jobs
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
	q := `SELECT id, profile, task, COALESCE(deps_json,'[]'), COALESCE(flow_json,'') FROM queue_jobs
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
		var depsJSON, flowJSON string
		if err := rows.Scan(&j.id, &j.profile, &j.task, &depsJSON, &flowJSON); err != nil {
			rows.Close()
			return 0, err
		}
		_ = json.Unmarshal([]byte(depsJSON), &j.deps)
		if flowJSON != "" {
			var f Flow
			// A flow we cannot parse must NOT be treated as "no flow": that would
			// silently run a node whose guard was meant to hold it back. It is
			// recorded and turned into a job failure below, once the cursor is
			// closed (the store pins a single writer connection).
			if err := json.Unmarshal([]byte(flowJSON), &f); err != nil {
				j.flowErr = fmt.Errorf("job %s has a malformed flow_json and cannot be dispatched safely: %w", j.id, err)
			} else {
				j.flow = &f
			}
		}
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

	progressThisPass := 0
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
		if j.flowErr != nil {
			if ok, err := cascadeFail(ctx, db, j.id, j.flowErr.Error()); err != nil {
				return progressThisPass, err
			} else if ok {
				sum.Failed++
				progressThisPass++
			}
			continue
		}
		if len(j.deps) > 0 {
			satisfied, failedDep, failedStatus, err := depState(ctx, db, j.deps)
			if err != nil {
				return progressThisPass, err
			}
			if failedStatus == skippedDepStatus {
				// Cascade-SKIP (PRD-112): this node sits behind a conditional edge
				// that was not taken. That is not a failure, and it must not be
				// reported as one — but it is terminal, so the node cannot be left
				// 'pending' either.
				if ok, err := cascadeSkip(ctx, db, j.id,
					fmt.Sprintf("branch not taken: dependency %s was skipped", failedDep)); err != nil {
					return progressThisPass, err
				} else if ok {
					sum.Pruned++
					progressThisPass++
				}
				continue
			}
			if failedDep != "" {
				// Cascade-fail: a dependency reached a non-recoverable terminal state.
				if ok, err := cascadeFail(ctx, db, j.id, fmt.Sprintf("dependency %s %s", failedDep, failedStatus)); err != nil {
					return progressThisPass, err
				} else if ok {
					sum.Failed++
					progressThisPass++
				}
				continue
			}
			if !satisfied {
				skippedThisPass++
				continue
			}
		}
		// PRD-112: resolve this node's conditional edge and its {{state.<key>}}
		// references against the run's reduced state. Both happen BEFORE the
		// claim, so a node whose guard is false is never marked 'running'.
		if j.flow != nil {
			state, err := RunState(ctx, db, j.flow.RunID)
			if err != nil {
				if ok, err := cascadeFail(ctx, db, j.id, err.Error()); err != nil {
					return progressThisPass, err
				} else if ok {
					sum.Failed++
					progressThisPass++
				}
				continue
			}
			if j.flow.When != nil {
				take, err := j.flow.When.Eval(state)
				if err != nil {
					if ok, err := cascadeFail(ctx, db, j.id, err.Error()); err != nil {
						return progressThisPass, err
					} else if ok {
						sum.Failed++
						progressThisPass++
					}
					continue
				}
				if !take {
					if ok, err := cascadeSkip(ctx, db, j.id, fmt.Sprintf(
						"conditional edge not taken: state[%q] %s %q is false",
						j.flow.When.Source, j.flow.When.Op, j.flow.When.Value)); err != nil {
						return progressThisPass, err
					} else if ok {
						sum.Pruned++
						progressThisPass++
					}
					continue
				}
			}
			if len(StateRefs(j.task)) > 0 {
				resolved, err := Interpolate(j.task, state)
				if err != nil {
					if ok, err := cascadeFail(ctx, db, j.id, err.Error()); err != nil {
						return progressThisPass, err
					} else if ok {
						sum.Failed++
						progressThisPass++
					}
					continue
				}
				j.task = resolved
			}
		}
		// Atomic claim: compare-and-set status -> 'running'. RowsAffected!=1 means
		// another drainer already claimed it, so we move on without executing.
		claimed, err := claim(ctx, db, j.id)
		if err != nil {
			return progressThisPass, err
		}
		if !claimed {
			continue
		}
		sum.Claimed++
		progressThisPass++

		text, runErr := runJob(ctx, db, opts, j)
		if runErr != nil {
			applied, err := finish(db, j.id, "failed", "", runErr.Error())
			if err != nil {
				return progressThisPass, err
			}
			if applied {
				sum.Failed++
			}
		} else {
			applied, err := finish(db, j.id, "done", text, "")
			if err != nil {
				return progressThisPass, err
			}
			if applied {
				sum.Done++
			}
		}
	}
	sum.Skipped = skippedThisPass
	return progressThisPass, nil
}

// depState reports whether every dep has reached 'done'. If any dep is in a
// terminal failed status it returns that dep id + status so the caller can
// cascade-fail. A missing dep counts as unsatisfied (never cascade-failed),
// matching dag.py which leaves such jobs pending.
//
// A dep in the terminal 'skipped' status (PRD-112: its conditional edge was not
// taken) is returned the same way, with failedStatus == skippedDepStatus, so the
// caller can cascade-SKIP instead of cascade-fail. It is checked first: a
// not-taken branch is not an error and must never be reported as one.
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
		if status == skippedDepStatus {
			return false, dep, skippedDepStatus, nil
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

// cascadeSkip marks a still-claimable job terminally 'skipped' because its
// conditional edge was false, or because it sits behind a node that was skipped
// (PRD-112). The reason is written to the `error` column — the column is the
// only free-text field on the row, and `dag state` surfaces it as `reason`, so
// an operator can always tell WHY a node did not run. The status guard means a
// concurrent `queue cancel` still wins.
func cascadeSkip(ctx context.Context, db *sql.DB, id, reason string) (bool, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(claimableStatuses)), ",")
	args := make([]any, 0, len(claimableStatuses)+3)
	args = append(args, reason, time.Now().UTC().Format(time.RFC3339), id)
	for _, s := range claimableStatuses {
		args = append(args, s)
	}
	r, err := db.ExecContext(ctx, `UPDATE queue_jobs SET status='`+skippedDepStatus+`', error=?, finished_at=?
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
func runJob(ctx context.Context, db *sql.DB, opts Options, j jobRow) (string, error) {
	model := opts.Model
	if opts.ModelForProfile != nil {
		if m := opts.ModelForProfile(j.profile); m != "" {
			model = m
		}
	}
	// #590: worker-executed runs (queue worker / dag run --execute / cron run
	// --execute) emitted no spans at all. The job id is the trace id, so
	// `tag trace show <job-id>` resolves that job's spans.
	rec := trace.NewRecorder(j.id, j.profile)
	loop := &agent.Loop{Provider: opts.Provider, Tracer: rec}
	// Telemetry is best-effort: a job's outcome must not depend on it. It runs on
	// every exit path so a failed job is still traced.
	defer func() { _ = rec.Save(db) }()
	if opts.WithTools {
		reg := agent.NewRegistry()
		topts := tool.DefaultOptions()
		topts.Guard = opts.Guard
		// #591: this used to leave Root at "" — tool.Register then falls back to
		// os.Getwd(), so EVERY job in a worker shared the worker process's cwd.
		// The DAG dispatches independent nodes precisely because they are
		// independent, and independent nodes are the ones most likely to write
		// the same filename, so a shared root made them clobber each other.
		// Each job now gets its own root; the traversal/symlink guards in
		// tool.resolvePath are unchanged and simply apply relative to it.
		dir, cleanup, derr := jobWorkDir(opts.WorkRoot, j.id)
		if derr != nil {
			return "", fmt.Errorf("preparing work dir for job %s: %w", j.id, derr)
		}
		defer cleanup()
		topts.Root = dir
		tool.Register(reg, topts)
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

// DefaultWorkRootName is the directory under TAG_HOME that holds per-job working
// directories when Options.WorkRoot is unset.
const DefaultWorkRootName = paths.DefaultWorkRootName

// jobWorkDir creates the private working directory for one job at
// <WorkRoot>/<job-id> and returns it with a cleanup func.
//
// The #591 contract itself — an EMPTY scratch dir (not a copy or checkout), a
// stable derivable path, a confined path segment, and a cleanup that removes the
// dir only if the job left it empty — lives in paths.WorkDir, which is shared
// with swarm, eval, ciauto and loop. The job id comes from the DB and is
// normally a uuid; a degenerate one falls back to "job".
func jobWorkDir(workRoot, jobID string) (string, func(), error) {
	return paths.WorkDir(workRoot, paths.SafeSegment(jobID, "job"))
}

// ensureResultColumn self-heals the schema: it adds a `result TEXT` column to
// queue_jobs if the running DB predates it. schema.sql is intentionally left
// untouched (it is owned elsewhere); this keeps the worker package independent.
func ensureResultColumn(db *sql.DB) error { return ensureQueueColumn(db, "result") }

// EnsureFlowColumn is ensureFlowColumn for callers outside this package: the
// DAG submitter has to INSERT flow_json before any drainer has ever run, so it
// cannot rely on Drain having self-healed the schema first.
func EnsureFlowColumn(db *sql.DB) error { return ensureFlowColumn(db) }

// ensureFlowColumn self-heals the PRD-112 `flow_json TEXT` column the same way.
// It is called from every entry point that reads flow_json (Drain and the
// RunState/RunNodes readers) so a DB that predates the feature never surfaces a
// raw "no such column" error to a user who simply ran `dag state`.
func ensureFlowColumn(db *sql.DB) error { return ensureQueueColumn(db, "flow_json") }

// ensureQueueColumn adds a TEXT column to queue_jobs if it is absent.
func ensureQueueColumn(db *sql.DB, column string) error {
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
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if found {
		return nil
	}
	// The column name is a package-internal constant, never user input, so the
	// interpolation below cannot be steered from outside.
	if _, err := db.Exec(`ALTER TABLE queue_jobs ADD COLUMN ` + column + ` TEXT`); err != nil {
		// A concurrent drainer may have added the column between our check and
		// the ALTER; SQLite reports that as a duplicate-column error we can ignore.
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return nil
		}
		return err
	}
	return nil
}
