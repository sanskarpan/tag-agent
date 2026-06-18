# PRD-115: Stateful Process Framework (`tag process`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `process_framework.py + controller.py`
**Depends on:** PRD-112 (graph-based workflow engine), PRD-110 (state serialization), PRD-022 (cron scheduler)
**Inspired by:** Temporal.io durable workflows, AWS Step Functions, Apache Airflow DAGs, Prefect 2.x flows

---

## 1. Overview

Agent workflows in TAG are currently single-shot: start, execute, complete. There is no abstraction for long-running business processes that span days or weeks, require coordinated multi-step state transitions, survive infrastructure restarts, and support human escalation at defined checkpoints. Production AI systems increasingly need these "durable process" semantics — a code review pipeline that waits for CI, a content moderation process that waits for human review, a data ingestion pipeline with retry logic.

Stateful Process Framework (`tag process`) introduces a lightweight process DSL inspired by Temporal.io's workflow-as-code model and AWS Step Functions' state machine. A `@process` decorator marks a Python function as a durable process; within it, `await step("name", fn)` executes a step with automatic retry, `await wait_for("condition")` suspends until a condition is met, and `await escalate("question")` triggers a HITL interrupt (PRD-109). Process state is persisted to SQLite after every step, enabling crash recovery and process introspection.

Unlike full workflow orchestration systems (Temporal, Airflow), TAG's process framework is local-first, embedded, and Python-native — designed for individual developer use, not enterprise deployment.

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
| G1 | `@process` decorator marks a Python coroutine as a durable process; `tag process start <module.function>` launches it. |
| G2 | `step(name, fn, max_retries=3, backoff=2.0)` executes a step with automatic retry on failure. |
| G3 | `wait_for(condition_fn, poll_interval=60, timeout=86400)` suspends the process until the condition returns True, polling SQLite state. |
| G4 | `escalate(question)` triggers PRD-109 HITL interrupt and resumes after human response. |
| G5 | All step results persisted to SQLite; process survives TAG process restart. |
| G6 | `tag process list`, `tag process show`, `tag process stop`, `tag process resume` CLI commands. |
| G7 | Cron-triggered processes: `tag process schedule <module.function> --cron "0 9 * * 1-5"` (integrates with PRD-022). |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Distributed process execution across machines. |
| NG2 | Visual process flow editor. |
| NG3 | Integration with external workflow engines (Temporal, Airflow). |
| NG4 | Real-time event streaming to process coroutines. |

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

```python
# Process definition (Python API):
from tag.process_framework import process, step, wait_for, escalate

@process(name="code-review-pipeline")
async def code_review(pr_id: str):
    test_result = await step("run-tests", lambda: run_tests(pr_id), max_retries=3)
    ci_status = await wait_for(
        lambda: get_ci_status(pr_id) == "passed",
        poll_interval=60, timeout=3600
    )
    approval = await escalate(f"CI passed. Approve deployment of PR {pr_id}?")
    if "yes" in approval.lower():
        await step("deploy", lambda: deploy(pr_id))
```

```
# CLI:
tag process start <module.process_name> [--arg key=value ...] [--detach]
tag process list [--status running|paused|completed|failed]
tag process show <process-id>
tag process stop <process-id>
tag process resume <process-id>
tag process schedule <module.process_name> --cron "0 9 * * 1-5" [--arg key=value ...]

Options:
  --arg key=value    Initial arguments for the process
  --detach           Run in background (subprocess)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `@process` decorator registers the function as a named process; `tag process start` instantiates it and saves a `process_instances` SQLite row. |
| FR-02 | `step(name, fn, max_retries, backoff)`: executes `fn()`, retries on exception up to `max_retries` times with `backoff^attempt` second delay; persists result to `process_steps` table. |
| FR-03 | On process restart: detect completed steps from `process_steps` table; skip them (return cached result); resume from first incomplete step. |
| FR-04 | `wait_for(condition_fn, poll_interval, timeout)`: poll `condition_fn()` every `poll_interval` seconds; if True, resume; if `timeout` exceeded, raise `WaitForTimeoutError`. |
| FR-05 | `escalate(question)`: calls PRD-109 `interrupt()` with the question; suspends process; resumes after operator input. |
| FR-06 | `tag process list` queries `process_instances` and renders: process_id, name, status, current_step, started_at, last_step_at. |
| FR-07 | `tag process show <id>` renders all completed steps with result summaries. |
| FR-08 | `tag process stop <id>` sets status to `stopped`; running subprocess receives SIGTERM. |
| FR-09 | `tag process schedule` calls PRD-022 `cron_jobs` table to trigger the process on schedule. |
| FR-10 | Process coroutine runs in an `asyncio` event loop; `step()` and `wait_for()` are `async def` functions. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | `wait_for` polling must not busy-wait; use `asyncio.sleep(poll_interval)` between polls. |
| NFR-02 | Step results serialized as JSON (not pickle) for portability and debuggability. |
| NFR-03 | Process subprocess (--detach) writes stdout/stderr to `~/.tag/logs/process_<id>.log`. |
| NFR-04 | Maximum step retry delay: `min(backoff^attempt, 300)` seconds (5-minute cap). |

---

## 9. Technical Design

### 9.1 SQLite DDL

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

### 9.2 Python core

```python
from __future__ import annotations
import asyncio
import json
import sqlite3
import uuid
from functools import wraps
from typing import Any, Callable, Optional

class ProcessContext:
    def __init__(self, process_id: str, db_path: str) -> None:
        self.process_id = process_id
        self.db_path = db_path

    async def step(self, name: str, fn: Callable, max_retries: int = 3, backoff: float = 2.0) -> Any:
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        # Check if already completed
        row = conn.execute(
            "SELECT result FROM process_steps WHERE process_id=? AND step_name=? AND status='completed'",
            (self.process_id, name)
        ).fetchone()
        if row:
            return json.loads(row["result"]) if row["result"] else None
        # Execute with retry
        for attempt in range(1, max_retries + 1):
            try:
                result = fn() if not asyncio.iscoroutinefunction(fn) else await fn()
                conn.execute(
                    "INSERT OR REPLACE INTO process_steps(id,process_id,step_name,attempt,status,result,created_at) "
                    "VALUES(?,?,?,?,?,?,?)",
                    (uuid.uuid4().hex[:8], self.process_id, name, attempt, "completed",
                     json.dumps(result, default=str), _utc_now())
                )
                conn.commit()
                return result
            except Exception as e:
                if attempt >= max_retries:
                    raise
                await asyncio.sleep(min(backoff ** attempt, 300))

    async def wait_for(self, condition: Callable, poll_interval: int = 60, timeout: int = 86400) -> None:
        import time
        start = time.time()
        while True:
            if condition():
                return
            if time.time() - start > timeout:
                raise TimeoutError(f"wait_for timed out after {timeout}s")
            await asyncio.sleep(poll_interval)

    async def escalate(self, question: str) -> str:
        from tag.workflow_engine import interrupt
        return interrupt(question)

def _utc_now() -> str:
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).isoformat()

def process(name: str):
    def decorator(fn):
        @wraps(fn)
        async def wrapper(*args, **kwargs):
            return await fn(*args, **kwargs)
        wrapper._process_name = name
        return wrapper
    return decorator
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
| Unit | `step()` retry logic, backoff delay, cached result return; `wait_for` timeout |
| Integration | Multi-step process crash/resume; escalate interrupt flow |
| CLI | `tag process list/show/stop` render correct output |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `@process` function restarts from last completed step after process kill |
| AC-02 | `step()` retries 3 times with backoff before raising |
| AC-03 | `wait_for()` polls without busy-waiting |
| AC-04 | `escalate()` suspends until operator input |
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
| 1 | `ProcessContext`, `step()`, `wait_for()`, SQLite DDL | 2 |
| 2 | `@process` decorator, `tag process start/list/show/stop` CLI | 2 |
| 3 | Crash recovery, escalate integration, cron scheduling | 2 |
| 4 | Integration tests, documentation | 1 |
