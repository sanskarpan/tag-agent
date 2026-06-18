# PRD-109: HITL interrupt()+Command(resume=) (`tag workflow interrupt`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `workflow_engine.py + controller.py`
**Depends on:** PRD-110 (state serialization/checkpointing), PRD-112 (graph-based workflow), PRD-082 (multi-agent team primitives)
**Inspired by:** LangGraph interrupt()+Command(resume=), CrewAI human input, AutoGen human proxy agent, OpenAI Swarm handoff

---

## 1. Overview

Fully autonomous agents are unsuitable for high-stakes decisions: deleting files, making purchases, sending emails, deploying to production. TAG's current execution model runs agents to completion without any mechanism to pause mid-execution and request human approval or input. When an agent reaches a decision point requiring human judgment, the entire run must be aborted and restarted manually.

Human-in-the-Loop interrupt (`tag workflow interrupt`) introduces a first-class pause mechanism modeled after LangGraph's `interrupt()` / `Command(resume=)` pattern. When an agent workflow reaches an interrupt point, execution is suspended, state is checkpointed (PRD-110), and the operator is prompted for input. On resume (`tag workflow resume <session-id> --input "approved"`), the workflow continues exactly from the interrupt point with the human's input injected into the agent's context — no replay, no restart.

The design is directly inspired by LangGraph's 2024 `interrupt()` primitive (which pauses graph execution and stores the interrupt value in the checkpoint), LangGraph's `Command(resume=value)` pattern (which passes the human response back into the graph), and CrewAI's `human_input=True` task parameter. Unlike those frameworks, TAG's implementation works with any workflow graph and persists interrupt state to SQLite for durability across process restarts.

---

## 2. Problem Statement

### 2.1 No pause mechanism for approval gates

Production agent workflows need approval gates: "before calling this tool, confirm with a human." Currently, TAG has no mechanism to pause mid-execution at a defined point and wait for human input before continuing.

### 2.2 Interrupted runs lose all progress

If an engineer manually kills a TAG process mid-run (e.g., to review intermediate output), all state is lost and the run must restart from the beginning — re-executing all prior steps and incurring their cost.

### 2.3 No structured human input injection

Even when engineers manually inspect runs, there is no mechanism to inject a correction or approval decision back into the running agent. Human feedback must be re-fed as a new prompt in a new run.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Provide an `interrupt(question, context)` function that workflow nodes can call to pause execution and prompt the operator. |
| G2 | On interrupt: checkpoint all current state to SQLite (PRD-110), print the interrupt question and context to the terminal, and exit the workflow process. |
| G3 | `tag workflow resume <session-id>` resumes execution from the checkpoint with the operator's input available via `get_interrupt_response()`. |
| G4 | Support `--auto-approve` mode for CI/CD pipelines that should not block on human input. |
| G5 | Support timeout-based auto-escalation: if no response within N seconds, trigger the escalation handler (e.g., abort or skip). |
| G6 | Multiple consecutive interrupts in a single workflow session (approval gates at multiple decision points). |
| G7 | Interrupt state visible in `tag workflow list` as status `interrupted` with the pending question shown. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Web-based approval interface. Terminal only. |
| NG2 | Multi-approver consensus (only single operator input). |
| NG3 | Email/Slack-based interrupt delivery. |
| NG4 | Interrupt points in non-workflow runs (only graph-based workflows). |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Resume latency | `tag workflow resume` continues execution in < 500ms after operator input | Benchmark test |
| State fidelity | All prior step outputs and tool call results available after resume | Integration test |
| Interrupt overhead | Adding interrupt() to a node adds < 5ms per step | Benchmark test |
| Auto-approve CI compatibility | `--auto-approve` skips all interrupts and completes workflow without blocking | Integration test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Define an interrupt point before a destructive tool call | A human must approve before files are deleted |
| US2 | Operator | Resume a paused workflow after reviewing the interrupt context | I can approve or reject with my input |
| US3 | CI engineer | Run workflows with `--auto-approve` in CI | Pipelines don't block on human input |
| US4 | Developer | See all interrupted workflows in `tag workflow list` | I know which workflows are waiting for my input |
| US5 | Developer | Set a timeout so interrupted workflows auto-abort after 1 hour | I prevent workflows from waiting indefinitely |

---

## 6. CLI Surface

```
tag workflow interrupt show <session-id>
tag workflow resume <session-id> --input "approved" [--timeout-action abort|skip]
tag workflow list --filter interrupted

# In workflow definition (Python API):
from tag.workflow_engine import interrupt, get_interrupt_response

def review_node(state):
    human_input = interrupt(
        question="About to delete 47 files. Proceed?",
        context={"files": state["files_to_delete"], "count": 47}
    )
    if human_input.lower() in ("yes", "y", "approved"):
        return {"approved": True}
    return {"approved": False, "reason": human_input}

Options:
  --input TEXT          Human response to inject at the interrupt point
  --timeout N           Auto-escalate after N seconds (default: 3600)
  --timeout-action      What to do on timeout: abort|skip (default: abort)
  --auto-approve        Automatically approve all interrupts (CI mode)
  --auto-input TEXT     Input to inject in --auto-approve mode (default: "approved")
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `interrupt(question, context)` function: checkpoints current state, writes `workflow_interrupts` row with status `pending`, raises `InterruptException` to unwind the call stack. |
| FR-02 | The workflow engine catches `InterruptException`, serializes state to SQLite (PRD-110), and prints the interrupt question + context to terminal before exiting. |
| FR-03 | `tag workflow resume` fetches the checkpoint (PRD-110), stores the operator's `--input` in the `workflow_interrupts` row, and re-executes the graph from the checkpoint. |
| FR-04 | `get_interrupt_response()`: called within the same node after `interrupt()` returns on resume; returns the stored operator input string. |
| FR-05 | On resume, the node that called `interrupt()` is re-executed from its beginning (not from mid-node); `interrupt()` returns immediately on re-execution with the stored response. |
| FR-06 | `--auto-approve` flag sets a process-level flag that causes `interrupt()` to return the `--auto-input` value immediately without checkpointing or terminal prompt. |
| FR-07 | Timeout: if the `workflow_interrupts` row remains `pending` beyond `--timeout` seconds, a daemon marks it `timed_out` and triggers the `--timeout-action`. |
| FR-08 | `tag workflow list --filter interrupted` queries `workflow_sessions` WHERE status='interrupted' and shows session ID, goal, interrupt question, pending duration. |
| FR-09 | Multiple interrupt points: after resume and re-execution past the first interrupt, subsequent `interrupt()` calls follow the same checkpoint/resume flow. |
| FR-10 | Interrupt context is serialized as JSON and stored in `workflow_interrupts.context`; `tag workflow interrupt show` renders it formatted. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Checkpoint/resume must preserve exact Python object state (via pickle or JSON-serializable state dict). |
| NFR-02 | No background daemon required for interrupt detection; polling from CLI on `resume` is sufficient. |
| NFR-03 | Interrupt state must survive TAG process crash; state checkpointed before process exits. |
| NFR-04 | `--auto-approve` must be opt-in via explicit flag; never the default. |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS workflow_interrupts (
  id              TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL,
  step_id         TEXT NOT NULL,
  question        TEXT NOT NULL,
  context         TEXT,          -- JSON
  status          TEXT NOT NULL DEFAULT 'pending',  -- 'pending'|'responded'|'timed_out'
  operator_input  TEXT,
  timeout_s       INTEGER NOT NULL DEFAULT 3600,
  created_at      TEXT NOT NULL,
  responded_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_workflow_interrupts_session
  ON workflow_interrupts(session_id, status);
```

### 9.2 Python core

```python
from __future__ import annotations
import json
import os
import uuid
from typing import Any, Optional

_CURRENT_SESSION_ID: Optional[str] = None
_AUTO_APPROVE: bool = False
_AUTO_INPUT: str = "approved"

class InterruptException(Exception):
    def __init__(self, interrupt_id: str) -> None:
        self.interrupt_id = interrupt_id
        super().__init__(f"Workflow interrupted: {interrupt_id}")

def interrupt(question: str, context: Any = None) -> str:
    global _CURRENT_SESSION_ID, _AUTO_APPROVE, _AUTO_INPUT
    if _AUTO_APPROVE:
        return _AUTO_INPUT
    if not _CURRENT_SESSION_ID:
        raise RuntimeError("interrupt() called outside a workflow session")
    interrupt_id = uuid.uuid4().hex[:8]
    # Check if we're in a resume and this interrupt has already been responded to
    from tag.workflow_engine import _get_db
    conn = _get_db()
    existing = conn.execute(
        "SELECT operator_input, status FROM workflow_interrupts WHERE session_id=? AND step_id=? AND status='responded'",
        (_CURRENT_SESSION_ID, interrupt_id)
    ).fetchone()
    if existing:
        return existing["operator_input"]
    # New interrupt — checkpoint and raise
    conn.execute(
        "INSERT INTO workflow_interrupts(id,session_id,step_id,question,context,status,created_at) VALUES(?,?,?,?,?,?,?)",
        (interrupt_id, _CURRENT_SESSION_ID, interrupt_id, question, json.dumps(context) if context else None,
         "pending", _utc_now())
    )
    conn.commit()
    raise InterruptException(interrupt_id)

def _utc_now() -> str:
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).isoformat()
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Auto-approve bypassing safety gates | `--auto-approve` requires explicit flag; warn prominently in docs |
| Operator input injection (prompt injection via terminal) | Operator input is passed as raw string, not interpolated into system prompt |
| Long-lived interrupted sessions accumulating sensitive state | Implement TTL on interrupt records (default 7 days) |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `interrupt()` raises `InterruptException`; re-execution returns stored response; `--auto-approve` returns immediately |
| Integration | Full workflow: interrupt → checkpoint → resume → continuation; verify prior step state preserved |
| Security | Ensure `--auto-approve` is not default; test timeout escalation |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | Workflow with `interrupt()` pauses, prints question, and exits cleanly |
| AC-02 | `tag workflow resume <id> --input "yes"` continues from interrupt point with "yes" as the response |
| AC-03 | All prior step state is available after resume |
| AC-04 | `tag workflow list --filter interrupted` shows the paused workflow with the pending question |
| AC-05 | `--auto-approve` completes the workflow without pausing |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-110 state serialization | Checkpoint/restore for suspend/resume |
| PRD-112 graph-based workflow | Workflow node execution framework |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should `interrupt()` support structured approval forms (multiple fields) or only free-text input? |
| OQ-02 | Should interrupted workflows appear in a TUI dashboard for easier review? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `interrupt()` / `InterruptException` / SQLite DDL | 1 |
| 2 | Checkpoint integration (PRD-110), resume logic | 2 |
| 3 | CLI commands, `--auto-approve`, timeout handling | 2 |
| 4 | Integration tests, documentation | 1 |

