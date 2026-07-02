# PRD-115: Stateful Process Framework (`tag process`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `internal/queue + internal/cli + internal/runtime`
**Depends on:** PRD-112 (graph-based workflow engine), PRD-110 (state serialization), PRD-022 (cron scheduler)
**Inspired by:** Temporal.io durable workflows, AWS Step Functions, Apache Airflow DAGs, Prefect 2.x flows

---

## 1. Overview

Agent workflows in TAG are currently single-shot: start, execute, complete. There is no abstraction for long-running business processes that span days or weeks, require coordinated multi-step state transitions, survive infrastructure restarts, and support human escalation at defined checkpoints. Production AI systems increasingly need these "durable process" semantics — a code review pipeline that waits for CI, a content moderation process that waits for human review, a data ingestion pipeline with retry logic.

Stateful Process Framework (`tag process`) introduces a lightweight process API inspired by Temporal.io's workflow-as-code model and AWS Step Functions' state machine. A process is a Go function registered against a `Process` registry; within it, the injected `ProcessContext` exposes `ctx.Step("name", fn)` to execute a step with automatic retry, `ctx.WaitFor(cond)` to suspend until a condition is met, and `ctx.Escalate("question")` to trigger a HITL interrupt (PRD-109). Process state is event-sourced to the single `tag.sqlite3` store after every step, enabling crash recovery and process introspection.

This is the state-machine/serialization backbone of the durable-workflow subsystem: process state is persisted as an append-only event log in the bespoke SQLite-backed scheduler in `internal/queue` (GO_MIGRATION_PLAN decision #5), so a restarted process replays completed steps and resumes from the first incomplete one. Unlike full workflow orchestration systems (Temporal, Airflow), TAG's process framework is local-first, embedded, and ships inside the single Go binary — designed for individual developer use, not enterprise deployment.

---

## 2. Problem Statement

### 2.1 No abstraction for multi-day workflows

A code review process that: (1) runs tests, (2) waits for CI, (3) requests human approval, (4) deploys if approved — spans minutes to days. TAG's current model requires manual re-invocation at each stage with no state carried between invocations.

### 2.2 No automatic retry with backoff

When a step fails (e.g., API rate limit), the entire workflow fails. There is no built-in retry with exponential backoff at the step level.

### 2.3 No process lifecycle management

TAG has no concept of a "running process" that persists across CLI invocations. Each `tag run` is independent. Engineers cannot list, pause, or resume named long-running processes.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | A registered process function is marked durable via the `Process` registry; `tag process start <name>` launches it. |
| G2 | `ctx.Step(name, fn, WithMaxRetries(3), WithBackoff(2.0))` executes a step with automatic retry on failure. |
| G3 | `ctx.WaitFor(cond, WithPollInterval(60), WithTimeout(86400))` suspends the process until `cond` returns true, polling SQLite state. |
| G4 | `ctx.Escalate(question)` triggers PRD-109 HITL interrupt and resumes after human response. |
| G5 | All step results event-sourced to SQLite; process survives TAG binary restart. |
| G6 | `tag process list`, `tag process show`, `tag process stop`, `tag process resume` CLI commands. |
| G7 | Cron-triggered processes: `tag process schedule <name> --cron "0 9 * * 1-5"` (integrates with PRD-022 / gocron). |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Distributed process execution across machines. |
| NG2 | Visual process flow editor. |
| NG3 | Integration with external workflow engines (Temporal, Airflow). |
| NG4 | Real-time event streaming to process goroutines. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Process crash recovery | Process resumes from last completed step after restart in < 5s | Integration test |
| Step retry | 3 retries with exponential backoff complete within 2× baseline time | Unit test |
| Wait-for polling | `wait_for` polls every `poll_interval` seconds without busy-waiting | CPU usage test |
| Process listing | `tag process list` renders all running/paused/completed processes in < 500ms | Integration test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Define a multi-step process with automatic retry | I build resilient workflows without try/except boilerplate |
| US2 | Developer | Use `wait_for` to pause until a CI build passes | I build event-driven workflows without polling loops |
| US3 | Developer | Have my process resume after a crash without re-running completed steps | I don't lose work on failures |
| US4 | Platform engineer | List all running processes and their current step | I monitor long-running automation |

---

## 6. CLI Surface

```go
// Process definition (Go API): register a durable process against the runtime.
package pipelines

import (
	"context"
	"strings"

	"example.com/tag/internal/queue/process"
)

func init() {
	process.Register("code-review-pipeline", codeReview)
}

func codeReview(ctx *process.Context, pr string) error {
	if _, err := ctx.Step("run-tests", func() (any, error) {
		return runTests(pr)
	}, process.WithMaxRetries(3)); err != nil {
		return err
	}

	if err := ctx.WaitFor(func() (bool, error) {
		return getCIStatus(pr) == "passed", nil
	}, process.WithPollInterval(60), process.WithTimeout(3600)); err != nil {
		return err
	}

	approval, err := ctx.Escalate("CI passed. Approve deployment of PR " + pr + "?")
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(approval), "yes") {
		if _, err := ctx.Step("deploy", func() (any, error) {
			return deploy(pr)
		}); err != nil {
			return err
		}
	}
	return nil
}
```

```
# CLI:
tag process start <name> [--arg key=value ...] [--detach]
tag process list [--status running|paused|completed|failed]
tag process show <process-id>
tag process stop <process-id>
tag process resume <process-id>
tag process schedule <name> --cron "0 9 * * 1-5" [--arg key=value ...]

Options:
  --arg key=value    Initial arguments for the process (registered process name)
  --detach           Run in background (spawned tag daemon goroutine)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `process.Register(name, fn)` registers the function as a named process; `tag process start` instantiates it and saves a `process_instances` SQLite row. |
| FR-02 | `ctx.Step(name, fn, opts...)`: executes `fn`, retries on returned `error` up to `max_retries` times with `backoff^attempt` second delay; persists result event to `process_steps` table. |
| FR-03 | On process restart: detect completed steps from `process_steps` table; skip them (return cached result); resume from first incomplete step (event-log replay). |
| FR-04 | `ctx.WaitFor(cond, opts...)`: poll `cond()` every `poll_interval` seconds; if it returns true, resume; if `timeout` exceeded, return `ErrWaitForTimeout`. |
| FR-05 | `ctx.Escalate(question)`: calls the PRD-109 interrupt API with the question; suspends process; resumes after operator input. |
| FR-06 | `tag process list` queries `process_instances` and renders (bubbles table): process_id, name, status, current_step, started_at, last_step_at. |
| FR-07 | `tag process show <id>` renders all completed steps with result summaries. |
| FR-08 | `tag process stop <id>` sets status to `stopped`; a detached daemon run has its `context.Context` cancelled (and the spawned OS process receives SIGTERM). |
| FR-09 | `tag process schedule` writes a PRD-022 `cron_jobs` row (driven by `gocron`) to trigger the process on schedule. |
| FR-10 | The process function runs as a goroutine driven by a `context.Context`; `ctx.Step()` and `ctx.WaitFor()` block on channels/timers rather than busy-waiting. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | `WaitFor` polling must not busy-wait; block on a `time.Ticker` / `<-ctx.Done()` select between polls. |
| NFR-02 | Step results serialized as JSON via `encoding/json` (never Go gob) for portability and debuggability. |
| NFR-03 | Detached run (`--detach`) writes stdout/stderr to `~/.tag/logs/process_<id>.log`. |
| NFR-04 | Maximum step retry delay: `min(backoff^attempt, 300)` seconds (5-minute cap). |

---

## 9. Technical Design

The framework is the state-machine/serialization backbone for durable workflows. Each `Step`/`WaitFor`/`Escalate` transition is appended as an event to the single `tag.sqlite3` store owned by `internal/queue` (GO_MIGRATION_PLAN decision #5); the current process state is a projection over that append-only log, which is what makes crash recovery and replay deterministic. `Checkpointer` is a Go interface implemented by the `internal/store` layer.

### 9.1 SQLite DDL

SQL DDL below is DB-neutral but targets `modernc.org/sqlite` (pure-Go, CGO_ENABLED=0). All writes route through the single-writer `internal/store` layer (`gofrs/flock` + `os.Rename` atomic RMW).

```sql
CREATE TABLE IF NOT EXISTS process_instances (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  status        TEXT NOT NULL DEFAULT 'running',
  current_step  TEXT,
  args          TEXT,  -- JSON
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS process_steps (
  id            TEXT PRIMARY KEY,
  process_id    TEXT NOT NULL REFERENCES process_instances(id),
  step_name     TEXT NOT NULL,
  attempt       INTEGER NOT NULL DEFAULT 1,
  status        TEXT NOT NULL DEFAULT 'completed',
  result        TEXT,  -- JSON
  error         TEXT,
  created_at    TEXT NOT NULL,
  UNIQUE(process_id, step_name)
);
```

### 9.2 Go core

`ProcessContext` (package `process` under `internal/queue`) carries the process ID and a `Checkpointer`. `Step` is cache-aware (idempotent replay), retries on error with capped exponential backoff, and appends a completed-step event via the store. `WaitFor` polls on a `time.Ticker`, honoring both the timeout and `ctx.Done()`. `Escalate` delegates to the PRD-109 interrupt API. IDs come from `google/uuid`; results serialize with `encoding/json`.

```go
package process

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
)

var ErrWaitForTimeout = errors.New("wait_for timed out")

// Checkpointer is implemented by the internal/store event-sourced layer.
type Checkpointer interface {
	// CompletedStep returns the cached result (decoded JSON) if this step
	// already completed, and ok=false otherwise.
	CompletedStep(ctx context.Context, processID, step string) (result any, ok bool, err error)
	// RecordStep appends a completed-step event.
	RecordStep(ctx context.Context, processID, step string, attempt int, result any) error
}

// Interrupter is the PRD-109 HITL interrupt API.
type Interrupter interface {
	Interrupt(ctx context.Context, question string) (string, error)
}

type Context struct {
	ctx         context.Context
	ProcessID   string
	cp          Checkpointer
	interrupter Interrupter
}

type stepOpts struct {
	maxRetries int
	backoff    float64
}

type StepOption func(*stepOpts)

func WithMaxRetries(n int) StepOption { return func(o *stepOpts) { o.maxRetries = n } }
func WithBackoff(b float64) StepOption { return func(o *stepOpts) { o.backoff = b } }

// Step executes fn with idempotent replay and capped exponential-backoff retry.
func (c *Context) Step(name string, fn func() (any, error), opts ...StepOption) (any, error) {
	o := stepOpts{maxRetries: 3, backoff: 2.0}
	for _, opt := range opts {
		opt(&o)
	}
	// Skip if already completed (crash-recovery replay).
	if result, ok, err := c.cp.CompletedStep(c.ctx, c.ProcessID, name); err != nil {
		return nil, err
	} else if ok {
		return result, nil
	}
	var lastErr error
	for attempt := 1; attempt <= o.maxRetries; attempt++ {
		result, err := fn()
		if err == nil {
			if rerr := c.cp.RecordStep(c.ctx, c.ProcessID, name, attempt, result); rerr != nil {
				return nil, rerr
			}
			return result, nil
		}
		lastErr = err
		if attempt >= o.maxRetries {
			break
		}
		delay := math.Min(math.Pow(o.backoff, float64(attempt)), 300)
		select {
		case <-time.After(time.Duration(delay) * time.Second):
		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		}
	}
	return nil, lastErr
}

type waitOpts struct {
	pollInterval time.Duration
	timeout      time.Duration
}

type WaitOption func(*waitOpts)

func WithPollInterval(sec int) WaitOption {
	return func(o *waitOpts) { o.pollInterval = time.Duration(sec) * time.Second }
}
func WithTimeout(sec int) WaitOption {
	return func(o *waitOpts) { o.timeout = time.Duration(sec) * time.Second }
}

// WaitFor polls cond without busy-waiting until it returns true or timeout.
func (c *Context) WaitFor(cond func() (bool, error), opts ...WaitOption) error {
	o := waitOpts{pollInterval: 60 * time.Second, timeout: 24 * time.Hour}
	for _, opt := range opts {
		opt(&o)
	}
	deadline := time.Now().Add(o.timeout)
	ticker := time.NewTicker(o.pollInterval)
	defer ticker.Stop()
	for {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrWaitForTimeout
		}
		select {
		case <-ticker.C:
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	}
}

// Escalate suspends the process on a PRD-109 HITL interrupt.
func (c *Context) Escalate(question string) (string, error) {
	return c.interrupter.Interrupt(c.ctx, question)
}

// Register marks a function as a named durable process.
func Register(name string, fn func(*Context, string) error) {
	registry[name] = fn
}

var registry = map[string]func(*Context, string) error{}

func newProcessID() string { return uuid.NewString()[:8] }
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Arbitrary code execution via `--arg` injection | Arguments validated as JSON; not eval'd |
| Long-running processes consuming resources | `wait_for` timeout prevents indefinite blocking; process `stop` always available |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Table-driven Go tests for `Step` retry logic, backoff delay, and cached-result replay; `WaitFor` timeout (fake clock / short intervals) |
| Integration | Multi-step process crash/resume against a `modernc.org/sqlite` fixture; `Escalate` interrupt flow |
| CLI | `tag process list/show/stop` render correct output (bubbles table golden snapshots) |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | A registered process restarts from last completed step after process kill |
| AC-02 | `Step()` retries 3 times with backoff before returning the error |
| AC-03 | `WaitFor()` polls without busy-waiting |
| AC-04 | `Escalate()` suspends until operator input |
| AC-05 | `tag process list` shows all process instances |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-110 state serialization | Step result persistence |
| PRD-109 HITL interrupt | `escalate()` implementation |
| PRD-022 cron scheduler | `tag process schedule` |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should processes support sub-processes (hierarchical nesting)? |
| OQ-02 | Should `wait_for` support event-driven wakeup (notify instead of poll)? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `ProcessContext`, `Step()`, `WaitFor()`, event-sourced SQLite DDL | 2 |
| 2 | `process.Register` registry, `tag process start/list/show/stop` CLI | 2 |
| 3 | Crash-recovery replay, `Escalate` integration, gocron scheduling | 2 |
| 4 | Integration tests, documentation | 1 |

