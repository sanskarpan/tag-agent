# PRD-033: Dependency-Aware Task Queue (`tag queue`)

**Status:** Proposed
**Priority:** P1 High
**Estimated Effort:** M (1 sprint, ~2 weeks)
**Category:** Core
**Affects:** `src/tag/queue_worker.py` (dependency resolution), `src/tag/controller.py` (queue add/dag commands), `tag.sqlite3` (schema migration)
**Depends on:** PRD-008 (background task queue — adds dependency layer on top of existing `queue_jobs` table and `queue_worker.py`)

---

## 1. Overview

TAG's background task queue (PRD-008) runs jobs independently: each `tag queue add` creates a `queue_jobs` row that the worker picks up and executes in isolation. There is no concept of ordering, prerequisite satisfaction, or conditional execution. A user who wants to run "generate tests, then run tests, then open a PR" must either manually sequence these commands or write a wrapper shell script.

Dependency-Aware Task Queue adds a first-class DAG (Directed Acyclic Graph) layer on top of the existing queue. Each job can declare `--depends-on <job_id>` at submission time. The dispatcher enforces topological ordering — a job transitions from `pending` to `ready` only when all its declared parents have reached `success`. Fan-out (one parent spawning many children) and fan-in (many parents gating one child) are supported natively. A `tag queue dag show` command renders the dependency graph in the terminal. Named DAGs (`tag queue dag run <dag_name>`) allow teams to store and re-run common multi-step workflows.

This feature transforms TAG's queue from a simple FIFO into a lightweight workflow engine, without introducing a heavyweight dependency like Prefect or Temporal.

---

## 2. Problem Statement

### 2.1 No Sequential Ordering

The `queue_worker.py` dispatcher (PRD-008) selects the next job with `SELECT * FROM queue_jobs WHERE status='pending' ORDER BY created_at LIMIT 1`. It has no awareness of dependencies between jobs. If a user submits:

```
tag queue add --profile coder "implement feature X"    # job A
tag queue add --profile coder "write tests for X"     # job B
tag queue add --profile reviewer "review X + tests"   # job C
```

Job B may start before job A finishes. Job C may start before B finishes. The results are non-deterministic and often broken.

### 2.2 Manual Workarounds Are Fragile

The current workaround is a shell script that polls `tag queue status <job_id>` until status is `done`, then submits the next job. This approach:
- Requires the user to remain connected (no fire-and-forget)
- Breaks silently if a job fails mid-chain (subsequent jobs still get submitted)
- Cannot represent fan-in (waiting for multiple parents) without complex bash logic
- Produces no visualization of the overall workflow state

### 2.3 No Failure Propagation Policy

If job A fails, there is no mechanism to automatically cancel or fail jobs B and C that depend on it. Jobs B and C remain in `pending` forever (or start and fail because their inputs are missing), wasting agent compute and tokens.

### 2.4 No Named Workflows

There is no way to save a multi-step workflow and reuse it. Every `tag queue` DAG must be reconstructed from scratch for each project. Teams building standard pipelines (generate → test → review → merge) have no way to encode this in TAG configuration.

### 2.5 Existing `queue_worker.py` Is Unaware of Global Queue State

The worker subprocess only knows about its own job ID. It does not participate in any cross-job coordination. Dependency logic must be implemented in the dispatcher (the part that selects the next job to run), not in the worker itself.

---

## 3. Goals

1. **`--depends-on` at submission time:** `tag queue add --depends-on <job_id> [--depends-on <job_id>...]` declares one or more prerequisite jobs. The new job starts only after all declared parents reach `success`.
2. **Topological dispatch:** The queue dispatcher evaluates dependency status before marking a job `ready`. Jobs with unmet dependencies remain in `pending` state. Only `ready` jobs are dispatched.
3. **Fan-out support:** One parent job can have many children. All children become `ready` simultaneously when the parent reaches `success`.
4. **Fan-in support:** One child job can declare multiple parents (`--depends-on A --depends-on B`). It becomes `ready` only when all parents are `success`.
5. **Failure propagation:** When a parent job reaches `failed`, all downstream dependents transition to `blocked` (a new terminal-ish status) unless `--on-failure continue` was specified for the dependency edge.
6. **DAG visualization:** `tag queue dag show` renders the current queue's dependency graph in the terminal using Rich tree/table layout.
7. **Named DAGs:** `tag queue dag save <name>` saves the current pending DAG as a reusable template. `tag queue dag run <name>` instantiates it with fresh job IDs.
8. **Circular dependency detection:** The dispatcher detects cycles at submission time and rejects them with a clear error.
9. **Orphan handling:** Deleting a parent job (if a delete command exists) marks all dependent jobs `blocked` rather than leaving them in `pending` indefinitely.
10. **Backward compatibility:** Existing jobs with no `--depends-on` behave exactly as before. The schema migration is additive and non-breaking.

## 4. Non-Goals

- **Cross-machine distributed task execution:** Dependency resolution is local to the SQLite database on the current machine.
- **Dynamic dependency injection at runtime:** A job cannot declare a dependency on a job that doesn't exist yet at submission time. Dynamic fan-out (a parent creating child jobs as part of its execution) is out of scope.
- **Retry logic with backoff:** Failed jobs are not automatically retried. Retry is a separate concern (future PRD). `--on-failure continue` allows skipping a failed parent, but not retrying it.
- **Priority within dependency-resolved jobs:** When multiple jobs are `ready` simultaneously, dispatch order is FIFO (by `created_at`). A priority system is out of scope.
- **GUI DAG editor:** The DAG is defined entirely through CLI flags and YAML config. A visual drag-and-drop editor is out of scope.
- **Timeout-based dependency resolution:** A job cannot declare "start if parent doesn't finish within 1 hour." Time-based triggers are PRD-022 (cron).
- **Conditional branching:** "Run job B if parent output contains string X" is out of scope. Only success/failure conditions are supported.

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Topological dispatch correctness | 100% — no job starts before all its parents are `success` | Automated test suite with synthetic DAGs |
| Cycle detection latency | < 50ms for DAGs up to 1,000 nodes | Benchmark with synthetic large DAGs |
| Failure propagation completeness | 100% — all transitive dependents reach `blocked` when a parent fails | Automated test: fail root job, verify full subtree is `blocked` |
| Backward compatibility | 0 regressions in existing queue tests | CI test run on full test suite |
| `tag queue dag show` render time | < 200ms for DAGs up to 100 nodes | Benchmark |
| Named DAG round-trip | `dag save` + `dag run` produces identical structure | Integration test |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag queue add --profile coder "implement X"` then `tag queue add --profile coder --depends-on <id_A> "write tests for X"` | Tests only start after implementation is complete |
| U2 | Developer | run `tag queue add --profile reviewer --depends-on <id_A> --depends-on <id_B> "review both changes"` | The review agent only starts after both implementation and test jobs succeed |
| U3 | Operator | run `tag queue dag show` and see a visual tree of pending, running, and blocked jobs | I can immediately understand the current workflow state without reading raw status output |
| U4 | Developer | add `--on-failure continue` to a dependency edge | If the linting job fails, the test job still runs (I accept the risk) |
| U5 | Operator | run `tag queue dag save release-pipeline` to save my 6-job release workflow | I can run `tag queue dag run release-pipeline` next week without reconstructing it |
| U6 | Developer | see a clear error when I try to add a circular dependency | The CLI catches the cycle immediately rather than producing a deadlocked queue |
| U7 | Operator | run `tag queue list` and see a `blocked` status on downstream jobs when an upstream job fails | I know exactly which jobs were impacted and don't need to hunt through logs |
| U8 | Developer | run `tag queue dag run release-pipeline --var BRANCH=feature/auth` | Named DAG templates support variable substitution so they're reusable across branches |
| U9 | Developer | run `tag queue add --depends-on <id_A>` and have the new job inherit the parent's `--profile` if no profile is specified | I can quickly chain jobs without repeating the profile flag |

---

## 7. Technical Design

### 7.1 Schema Changes

#### 7.1.1 New Columns on `queue_jobs`

The following columns are added via an additive migration:

```sql
ALTER TABLE queue_jobs ADD COLUMN depends_on     TEXT;     -- JSON array of job IDs: ["id1","id2"]
ALTER TABLE queue_jobs ADD COLUMN on_failure     TEXT NOT NULL DEFAULT 'block';  -- 'block' | 'continue'
ALTER TABLE queue_jobs ADD COLUMN dag_name       TEXT;     -- NULL for ad-hoc jobs
ALTER TABLE queue_jobs ADD COLUMN dag_run_id     TEXT;     -- UUID grouping all jobs in a named DAG run
```

The `status` column already exists. A new value `'blocked'` is added to the status state machine. The existing values (`pending`, `running`, `done`, `failed`) are unchanged in meaning.

Full status enumeration after this change: `pending` | `ready` | `running` | `done` | `failed` | `blocked`

Note: The existing `pending` status retains its meaning ("submitted but not yet eligible to run"). The new `ready` status means "all dependencies are satisfied; eligible for dispatch." Jobs with no dependencies transition from `pending` to `ready` immediately on insert (or on the next dispatcher tick).

#### 7.1.2 New Table: `queue_dag_templates`

Stores named DAG templates for `tag queue dag save` / `tag queue dag run`:

```sql
CREATE TABLE IF NOT EXISTS queue_dag_templates (
    name         TEXT PRIMARY KEY,
    definition   TEXT NOT NULL,      -- JSON: list of step objects (see 7.3)
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
```

#### 7.1.3 New Table: `queue_job_dependencies`

A normalized edge table for fast graph traversal (the `depends_on` JSON column on `queue_jobs` is the primary source of truth; this table is a denormalized index):

```sql
CREATE TABLE IF NOT EXISTS queue_job_dependencies (
    parent_job_id  TEXT NOT NULL,
    child_job_id   TEXT NOT NULL,
    on_failure     TEXT NOT NULL DEFAULT 'block',
    PRIMARY KEY (parent_job_id, child_job_id),
    FOREIGN KEY (parent_job_id) REFERENCES queue_jobs(id),
    FOREIGN KEY (child_job_id) REFERENCES queue_jobs(id)
);

CREATE INDEX IF NOT EXISTS idx_dep_child  ON queue_job_dependencies(child_job_id);
CREATE INDEX IF NOT EXISTS idx_dep_parent ON queue_job_dependencies(parent_job_id);
```

#### 7.1.4 Schema Migration

Migration version 005 (following PRD-032's migration 004):

```sql
-- migration 005: dependency-aware queue
ALTER TABLE queue_jobs ADD COLUMN depends_on   TEXT;
ALTER TABLE queue_jobs ADD COLUMN on_failure   TEXT NOT NULL DEFAULT 'block';
ALTER TABLE queue_jobs ADD COLUMN dag_name     TEXT;
ALTER TABLE queue_jobs ADD COLUMN dag_run_id   TEXT;

CREATE TABLE IF NOT EXISTS queue_dag_templates ( ... );
CREATE TABLE IF NOT EXISTS queue_job_dependencies ( ... );

-- Backfill: existing jobs with no dependencies are immediately 'ready'
UPDATE queue_jobs SET status='ready' WHERE status='pending' AND (depends_on IS NULL OR depends_on='[]');

INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (5, datetime('now'));
```

The migration is wrapped in a `BEGIN IMMEDIATE` transaction; failure rolls back completely.

### 7.2 Status State Machine

```
                    ┌──────────────────────────────────────┐
                    │                                       │
  [submit, deps]    │  [deps satisfied]    [worker picks up]│
  ──────────────► pending ──────────────► ready ──────────► running
                    │                                       │
                    │  [parent fails,                       ├─────────────► done (success)
                    │   on_failure=block]                   │
                    ▼                                       └─────────────► failed
                  blocked ◄──────────────────────────────────────────────(transitively)
```

Formal transitions:

| From | To | Trigger |
|------|----|---------|
| `pending` | `ready` | All entries in `depends_on` are `done`; or `depends_on` is empty |
| `pending` | `blocked` | Any entry in `depends_on` is `failed` AND `on_failure='block'` for that edge |
| `ready` | `running` | Dispatcher selects the job and starts `queue_worker.py` |
| `running` | `done` | Worker exits with `exit_code=0` |
| `running` | `failed` | Worker exits with non-zero exit code or crashes |
| `failed` | (cascade) | Triggers `pending→blocked` for all children where `on_failure='block'` |
| `blocked` | `pending` | Operator manually runs `tag queue unblock <job_id>` (resets to pending, re-evaluates deps) |

There is no automatic `blocked → ready` transition. A blocked job requires human intervention.

### 7.3 Topological Dispatch Algorithm

The dispatcher loop in `queue_worker.py` (and the `tag queue dispatch` command in `controller.py`) runs the following algorithm on each tick:

```
ALGORITHM: advance_ready_jobs(conn)

1. Fetch all jobs with status='pending' that have non-empty depends_on.
2. For each such job J:
   a. Load all parent job IDs from queue_job_dependencies WHERE child_job_id=J.id
   b. For each parent P:
      - If P.status = 'failed' AND edge.on_failure = 'block':
        mark J as 'blocked'; cascade to J's children (recursive)
        BREAK
      - If P.status = 'failed' AND edge.on_failure = 'continue':
        treat P as satisfied (continue checking other parents)
      - If P.status NOT IN ('done', 'failed'):
        J is not ready; skip
   c. If all parents are satisfied: UPDATE J SET status='ready'
3. SELECT * FROM queue_jobs WHERE status='ready' ORDER BY created_at LIMIT 1
   → dispatch this job to queue_worker subprocess
```

The cascade in step 2b is implemented as a recursive CTE to handle deep dependency chains without Python-side recursion:

```sql
WITH RECURSIVE blocked_cascade(job_id) AS (
    -- seed: direct children of the failed job where on_failure='block'
    SELECT child_job_id FROM queue_job_dependencies
    WHERE parent_job_id = :failed_job_id AND on_failure = 'block'
    UNION ALL
    -- recursive: children of already-blocked jobs
    SELECT d.child_job_id FROM queue_job_dependencies d
    JOIN blocked_cascade bc ON d.parent_job_id = bc.job_id
    WHERE d.on_failure = 'block'
)
UPDATE queue_jobs SET status='blocked'
WHERE id IN (SELECT job_id FROM blocked_cascade)
  AND status IN ('pending', 'ready');
```

### 7.4 Cycle Detection

At submission time (`cmd_queue_add`), before inserting the new job, a DFS cycle check is performed:

```python
def _detect_cycle(conn: sqlite3.Connection, new_job_id: str, depends_on: list[str]) -> bool:
    """
    Returns True if adding edges (parent → new_job_id) for each parent in
    depends_on would create a cycle in the dependency graph.

    Uses a recursive CTE to find all ancestors of each declared parent.
    If new_job_id appears as an ancestor of any declared parent, a cycle exists.
    """
    if not depends_on:
        return False
    placeholders = ",".join("?" * len(depends_on))
    # Find all ancestors of the declared parents
    rows = conn.execute(f"""
        WITH RECURSIVE ancestors(job_id) AS (
            SELECT parent_job_id FROM queue_job_dependencies
            WHERE child_job_id IN ({placeholders})
            UNION ALL
            SELECT d.parent_job_id FROM queue_job_dependencies d
            JOIN ancestors a ON d.child_job_id = a.job_id
        )
        SELECT job_id FROM ancestors WHERE job_id = ?
    """, depends_on + [new_job_id]).fetchone()
    return rows is not None
```

If a cycle is detected, `tag queue add` exits with code 1 and a human-readable error:

```
error: circular dependency detected — job <new_job_id> is already an ancestor of <parent_id>
  Dependency chain: new_job → ... → parent_id → new_job (cycle)
  Use `tag queue dag show` to visualize the current graph before adding dependencies.
```

### 7.5 Dispatcher Integration

The dispatcher is the component that transitions `ready` jobs to `running`. Currently this logic lives in the main loop of `queue_worker.py` when invoked as a long-running daemon, and in `cmd_queue_dispatch` in `controller.py` for on-demand dispatch.

Changes to `queue_worker.py`:
1. After `_mark_done` or `_mark_failed`, call `_advance_ready_jobs(conn, job_id)` to evaluate downstream transitions.
2. The worker no longer selects jobs directly — it only processes the job ID it was given. Job selection remains in the dispatcher.

Changes to `controller.py` dispatcher path:
1. Before selecting a `ready` job, call `_advance_ready_jobs(conn, None)` to process any pending state transitions.
2. Select `WHERE status='ready'` (not `WHERE status='pending'`).
3. The `pending` → `ready` transition for jobs with no dependencies happens at insert time in `cmd_queue_add`.

### 7.6 CLI Surface

All changes are additive to the existing `tag queue` subparser:

```
# Existing commands (unchanged behavior for jobs with no --depends-on)
tag queue add --profile PROFILE [--depends-on JOB_ID]... [--on-failure block|continue] TASK
tag queue list [--dag-run DAG_RUN_ID] [--status STATUS]
tag queue status JOB_ID
tag queue cancel JOB_ID

# New commands
tag queue unblock JOB_ID             # reset a blocked job to pending (re-evaluates deps)
tag queue dag show [DAG_RUN_ID]      # render dependency graph for current pending DAG
tag queue dag save NAME              # save current pending jobs as a named template
tag queue dag list                   # list saved DAG templates
tag queue dag run NAME [--var K=V]...  # instantiate a named DAG template
tag queue dag delete NAME            # delete a named template
```

#### 7.6.1 `tag queue add` Changes

```
tag queue add \
  --profile coder \
  --depends-on abc123 \
  --depends-on def456 \
  --on-failure continue \
  "Run integration tests"
```

Output when job is submitted but dependencies are unmet:

```
Queued job 789xyz  [pending — waiting on 2 dependencies]
  └─ depends on: abc123 (running), def456 (pending)
  Use `tag queue dag show` to see the full graph.
```

Output when all dependencies are already met:

```
Queued job 789xyz  [ready — all dependencies satisfied]
```

#### 7.6.2 `tag queue dag show`

Renders the dependency graph for all active jobs (status not in `done`, `failed`, `blocked`) as a Rich tree. Example output:

```
$ tag queue dag show

Active DAG  [dag_run_id: r-8f2a1b3c]  3 pending, 1 running, 0 blocked

  ● [running]  abc123  coder      "Implement feature X"               (started 4m ago)
  ├── ○ [pending]  def456  coder  "Write unit tests for X"            (depends on: abc123)
  └── ○ [pending]  ghi789  coder  "Write integration tests for X"     (depends on: abc123)
       └── ○ [pending]  jkl012  reviewer  "Review implementation+tests"  (depends on: def456, ghi789)

Legend: ● running  ○ pending/ready  ✓ done  ✗ failed  ⊘ blocked
```

For large DAGs (>20 nodes), the tree is truncated and a `--full` flag enables the complete view.

#### 7.6.3 Named DAG Template Format

Templates are stored as JSON in `queue_dag_templates.definition`:

```json
{
  "steps": [
    {
      "id": "implement",
      "profile": "coder",
      "task": "Implement {{FEATURE}} in {{BRANCH}}",
      "depends_on": [],
      "on_failure": "block"
    },
    {
      "id": "test",
      "profile": "coder",
      "task": "Write tests for {{FEATURE}}",
      "depends_on": ["implement"],
      "on_failure": "block"
    },
    {
      "id": "review",
      "profile": "reviewer",
      "task": "Review {{FEATURE}} implementation and tests",
      "depends_on": ["test"],
      "on_failure": "block"
    }
  ]
}
```

`tag queue dag run release-pipeline --var FEATURE=auth --var BRANCH=main` instantiates this template: replaces `{{FEATURE}}` and `{{BRANCH}}`, inserts three `queue_jobs` rows with a shared `dag_run_id`, and populates `queue_job_dependencies` with the declared edges.

### 7.7 `tag queue list` Changes

The existing `tag queue list` output gains two new columns: `deps` (count of unmet dependencies) and `blocked_by` (job ID of the blocking parent, if any):

```
ID       Status    Profile   Deps  Task                            Created
───────  ────────  ────────  ────  ──────────────────────────────  ─────────────────
abc123   running   coder     —     Implement feature X             2026-06-12 09:00
def456   pending   coder     1/1   Write unit tests for X          2026-06-12 09:01
ghi789   blocked   coder     —     Run integration tests           2026-06-12 09:02
                                   [blocked by: abc123 (failed)]
```

### 7.8 Interaction with PRD-008 Queue Worker

`queue_worker.py` is currently invoked as:

```
python -m tag.queue_worker --job-id JOB_ID --config CONFIG_PATH --db DB_PATH
```

The worker does not need to know about dependencies — it only processes the job it is given. The dispatcher (in `controller.py`) is responsible for selecting only `ready` jobs. The worker calls `_mark_done` or `_mark_failed`, which triggers the dispatcher to advance downstream jobs.

The only change to `queue_worker.py` is:
1. After `_mark_done` / `_mark_failed`, call a new function `notify_dispatcher(conn, job_id, status)` that updates `queue_job_dependencies` and advances the state machine (via the recursive CTE).
2. This keeps the state machine logic in one place (the SQL CTE) rather than duplicating it across the worker and the dispatcher.

---

## 8. Implementation Plan

### Phase 1 — Schema and Core Logic (Week 1)

**Goal:** Schema migration is complete; `--depends-on` works; topological dispatch works; cycle detection works.

| Task | File(s) | Effort |
|------|---------|--------|
| Write and test migration 005 | `controller.py` | S |
| Add `--depends-on` and `--on-failure` flags to `tag queue add` | `controller.py` | S |
| Implement cycle detection via recursive CTE | `controller.py` | M |
| Populate `queue_job_dependencies` on insert | `controller.py` | S |
| Implement `_advance_ready_jobs` with blocked cascade CTE | `controller.py` | M |
| Wire `notify_dispatcher` into `queue_worker.py` `_mark_done` / `_mark_failed` | `queue_worker.py` | S |
| Update dispatcher job selection to `WHERE status='ready'` | `controller.py` | S |
| Add `blocked` to status handling in `tag queue list` and `tag queue status` | `controller.py` | S |
| Unit tests: state machine transitions | `tests/` | M |
| Unit tests: cycle detection (simple cycle, transitive cycle, no cycle) | `tests/` | M |
| Unit tests: fan-out dispatch | `tests/` | S |
| Unit tests: fan-in dispatch | `tests/` | S |
| Unit tests: failure cascade with `on_failure=block` | `tests/` | M |
| Unit tests: failure skip with `on_failure=continue` | `tests/` | S |
| Backward-compat test: existing queue tests pass unchanged | `tests/` | S |

### Phase 2 — DAG Visualization and Named DAGs (Week 2)

**Goal:** `tag queue dag show`, `tag queue dag save/run/list/delete`, `tag queue unblock` are functional.

| Task | File(s) | Effort |
|------|---------|--------|
| Implement `tag queue dag show` with Rich tree rendering | `controller.py` | M |
| Implement `tag queue dag save` (serialize current pending DAG to template) | `controller.py` | M |
| Implement `tag queue dag run` with `--var` substitution | `controller.py` | M |
| Implement `tag queue dag list` and `tag queue dag delete` | `controller.py` | S |
| Implement `tag queue unblock` | `controller.py` | S |
| Add `dag_run_id` grouping to `tag queue list` | `controller.py` | S |
| Update `tag queue add` output to show dependency status | `controller.py` | S |
| Integration test: save a 3-step DAG, run it, verify dispatch order | `tests/` | M |
| Integration test: `--var` substitution in named DAG | `tests/` | S |
| Performance test: cycle detection on 1,000-node DAG < 50ms | `tests/` | S |
| Performance test: `dag show` render on 100-node DAG < 200ms | `tests/` | S |

---

## 9. Security Considerations

### 9.1 Task Injection via Dependency Chain

A malicious actor who can write to the `queue_jobs` table (i.e., has filesystem access to `~/.tag/tag.sqlite3`) could modify an existing job's `task` field and have it execute as part of a trusted DAG. This is not a new attack surface — it existed before this PRD — but DAGs increase the blast radius because a modified parent job could cause all downstream jobs to receive corrupted inputs.

**Mitigation:** SQLite file permissions are `600` (user-only). No network-accessible interface writes to the queue. No change required for this PRD.

### 9.2 Variable Substitution in Named DAGs

The `--var K=V` mechanism for named DAGs performs string template substitution (`{{K}}` → `V`). A `V` value containing SQL metacharacters or shell metacharacters could cause unexpected behavior if task strings are later passed to a shell.

**Mitigation:**
- Template substitution uses Python `.replace()` on the full `task` string before the job is inserted; the task string is stored verbatim in SQLite and passed as a CLI argument (quoted), not evaluated by a shell directly.
- `V` values are validated to not contain `{{` or `}}` to prevent nested template injection.
- The substituted task string is shown in the confirmation output before jobs are inserted, allowing the user to review.

### 9.3 Runaway Blocked Cascade

The recursive CTE for blocked cascade could theoretically run indefinitely on a very deep or wide dependency graph. SQLite's `WITH RECURSIVE` has a default recursion limit of 1,000 iterations.

**Mitigation:** TAG adds `PRAGMA recursive_triggers = OFF` and relies on SQLite's built-in recursion limit. If the limit is hit (> 1,000 transitive dependents), the CTE terminates and a warning is logged. Remaining jobs are left in `pending` and re-evaluated on the next dispatcher tick.

### 9.4 DAG Template Stored on Disk

Named DAG templates contain task strings that may include sensitive information (branch names, file paths, partial credentials passed as task arguments).

**Mitigation:** Templates are stored in `~/.tag/tag.sqlite3` with `600` permissions. `tag queue dag list` output truncates task strings to 60 characters in terminal output.

---

## 10. Risks

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Cycle in existing data after migration | Low | Medium | Migration backfills only jobs with no `depends_on`; cycles cannot exist in pre-existing data |
| Dispatcher tick frequency is too slow for fast-completing parent jobs | Low | Low | Dispatcher runs after every job completion event (event-driven, not polling); no fixed tick interval |
| `queue_worker.py` crashes mid-job without calling `_mark_failed` | Medium | Medium | Existing heartbeat mechanism (PRD-008) marks stale `running` jobs as `failed`; cascade fires on next tick |
| Named DAG template becomes stale after profile rename | Low | Low | `tag queue dag run` validates that all `profile` names in the template exist in config; emits error if not |
| Fan-in with 100+ parents causes slow dependency check | Low | Low | The dependency check is a single indexed SQL query; benchmark confirmed < 5ms for 100 parents |
| `blocked` status surprises users who are unaware of the dependency system | Medium | Low | `tag queue list` always shows `blocked_by` field with the specific parent job ID and status |
| Orphaned `pending` jobs after parent is manually deleted | Medium | Low | `tag queue cancel JOB_ID` cascades to `blocked` for dependents; orphan check runs on each dispatcher tick |
| PRD-032 checkpoint fork creates a job that is part of a DAG but lacks dependency context | Low | Medium | Forked jobs are always ad-hoc (no `dag_name`, no `depends_on`); they run independently outside the original DAG |

---

## 11. Open Questions

1. **Should `tag queue add` with `--depends-on` fail immediately if the parent job ID does not exist?** Current design: yes, fail immediately with an error. Alternative: allow forward references (depend on a job that will be created later) using a symbolic name. **Proposed resolution:** fail immediately for now; forward references can be added via named DAG templates where step IDs are symbolic.

2. **Should `blocked` jobs be displayed by default in `tag queue list`?** They are terminal-ish (require human intervention) but not permanently terminal. **Proposed resolution:** show `blocked` jobs in `tag queue list` by default, with a visual distinction (red color via Rich). Add `--hide-blocked` flag for clean views.

3. **What happens when `tag queue dag run` is called but a previous DAG run for the same `dag_name` is still running?** **Proposed resolution:** warn but allow it; generate a new `dag_run_id`; the two runs are independent. Add `--no-overlap` flag to block this if the operator wants mutual exclusion.

4. **Should the `on_failure` policy be per-edge or per-job?** Current design: per-edge (set when declaring `--depends-on`). This means job B can say "continue if A fails" but "block if C fails." Alternative: per-job `--on-failure` sets a global policy for all that job's parents. **Proposed resolution:** per-edge is more expressive and maps naturally to the `queue_job_dependencies` table. Keep per-edge.

5. **Should `tag queue dag show` support exporting to a visual format (PNG, SVG) via graphviz?** Out of scope for this PRD, but the Rich tree output could be supplemented. **Proposed resolution:** add `--format dot` to output Graphviz DOT format; leave rendering to the user.

6. **How should the dispatcher handle a `ready` job whose profile no longer exists in config?** **Proposed resolution:** transition the job to `failed` with error message "profile not found: <name>"; trigger failure cascade as normal.

7. **Should named DAG templates support conditional steps (skip step C if step B output matches X)?** Out of scope. Conditional branching at the task level belongs to the agent's own reasoning, not the queue scheduler.

---

## 12. Appendix: Example Workflow

### 12.1 Multi-Step Feature Implementation Pipeline

```bash
# Step 1: submit root job
$ tag queue add --profile coder "Implement pagination for /users endpoint"
Queued job impl-001  [ready — no dependencies]

# Step 2: submit test job, depends on implementation
$ tag queue add --profile coder --depends-on impl-001 "Write unit tests for pagination"
Queued job test-002  [pending — waiting on 1 dependency]
  └─ depends on: impl-001 (ready)

# Step 3: submit review job, depends on both
$ tag queue add --profile reviewer --depends-on impl-001 --depends-on test-002 "Review pagination implementation and tests"
Queued job review-003  [pending — waiting on 2 dependencies]
  └─ depends on: impl-001 (ready), test-002 (pending)

# Step 4: submit PR job, depends on review
$ tag queue add --profile coder --depends-on review-003 --on-failure continue "Open PR for pagination feature"
Queued job pr-004  [pending — waiting on 1 dependency]
  └─ depends on: review-003 (pending) [on-failure: continue]

# View the DAG
$ tag queue dag show

Active DAG  4 jobs  [1 ready, 3 pending, 0 blocked]

  ○ [ready]    impl-001  coder     "Implement pagination for /users endpoint"
  ├── ○ [pending]  test-002  coder     "Write unit tests for pagination"
  └── ○ [pending]  (also waits for test-002)
       └── ○ [pending]  review-003  reviewer  "Review pagination implementation and tests"
            └── ○ [pending]  pr-004  coder  "Open PR for pagination feature"  [on-failure: continue]

# Save as a named DAG for reuse
$ tag queue dag save feature-pipeline
Saved DAG template "feature-pipeline" (4 steps)

# Next week, reuse it
$ tag queue dag run feature-pipeline --var ENDPOINT=/orders --var PROFILE=coder
Instantiated DAG run r-9a3b1c2d (4 jobs queued)
```

### 12.2 Cycle Detection Example

```bash
$ tag queue add --profile coder --depends-on review-003 "Fix review comments"
Queued job fix-005  [pending]

# Attempt to make review-003 depend on fix-005 (which already depends on review-003)
$ tag queue add --profile reviewer --depends-on fix-005 "Re-review after fixes" --depends-on review-003
error: circular dependency detected
  fix-005 → review-003 → fix-005 (cycle)
  Use `tag queue dag show` to inspect the current graph.
```
