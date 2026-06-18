# PRD-110: Loop State Serialization (`tag workflow checkpoint`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `workflow_engine.py + controller.py`
**Depends on:** PRD-112 (graph-based workflow engine), PRD-109 (HITL interrupt), PRD-113 (time-travel debugging)
**Inspired by:** LangGraph SqliteSaver checkpoint, LangChain checkpoint serializers, Redis checkpoint for AI agents, Temporal.io workflow state persistence

---

## 1. Overview

Long-running agent workflows are brittle in the face of process crashes, network timeouts, and deliberate pauses. TAG's current execution model is run-to-completion with no intermediate state persistence — if the process dies on step 47 of a 60-step workflow, all 47 steps must be re-executed from scratch. This wastes API credits, time, and produces non-deterministic results when re-execution takes different code paths.

Loop State Serialization (`tag workflow checkpoint`) introduces a `SqliteCheckpointer` modeled after LangGraph's `SqliteSaver` — every workflow step writes a complete, serialized snapshot of the workflow state graph to a SQLite `checkpoints` table. On restart, the workflow engine detects the most recent checkpoint and resumes from that exact step, replaying no prior work. Checkpoints are also the foundation for HITL interrupt/resume (PRD-109) and time-travel debugging (PRD-113).

The checkpointer serializes the full workflow state dict (including agent outputs, tool call results, and intermediate artifacts) using `msgpack` for compact binary representation, falling back to JSON for debugging. Checkpoint writes are transactional and append-only; the most recent checkpoint per session is the canonical resume point.

---

## 2. Problem Statement

### 2.1 No crash recovery for long workflows

A workflow running 60 LLM calls over 45 minutes can fail at step 47 due to a network timeout, OOM error, or deliberate Ctrl-C. Without checkpointing, the engineer must restart from scratch — re-incurring ~$8 of API cost and 40 minutes of wall time.

### 2.2 HITL interrupt requires durable state

PRD-109 HITL interrupts require that workflow state is durably stored while waiting for human approval (which may take hours or days). Without a checkpointer, the TAG process must remain running during the approval window — blocking resources and failing on any server restart.

### 2.3 No time-travel for debugging

PRD-113 time-travel debugging requires a history of all intermediate states to allow rewinding to a previous step. Without per-step checkpoints, there is no history to rewind.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Write a complete serialized snapshot of the workflow state to SQLite after every step. |
| G2 | On workflow startup, detect and offer to resume from the most recent checkpoint for the given session. |
| G3 | Support `tag workflow checkpoint list <session-id>` to show all checkpoints with step number and timestamp. |
| G4 | Support `tag workflow checkpoint restore <session-id> --step N` to restore state to a specific step (time-travel, PRD-113). |
| G5 | Serialize workflow state using msgpack (primary) or JSON (fallback) for compact, fast serialization. |
| G6 | Checkpoint pruning: keep only the last N checkpoints per session to bound disk usage. |
| G7 | Thread-safe checkpoint writes using SQLite WAL mode and `BEGIN IMMEDIATE` transactions. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Remote checkpoint storage (Redis, S3). SQLite local only. |
| NG2 | Incremental/delta checkpoints. Each checkpoint is a full state snapshot. |
| NG3 | Checkpoint encryption. State may contain sensitive agent outputs; encryption is a future enhancement. |
| NG4 | Cross-session state sharing via checkpoints. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Checkpoint write latency | < 50ms per checkpoint for a 100KB state dict | Benchmark test |
| Resume accuracy | 100% state fidelity after checkpoint/restore cycle | Integration test |
| Disk usage | Checkpoint for 1MB state dict stored in < 1.5MB on disk (msgpack compression) | Disk measurement |
| Concurrent write safety | Zero corruption with 2 concurrent checkpoint writers on same session | Concurrency test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Have workflows automatically resume from the last checkpoint after a crash | I don't lose hours of work |
| US2 | Developer | List all checkpoints for a workflow session | I can understand where a workflow was at each step |
| US3 | Developer | Restore a workflow to step 20 to debug what went wrong | I can replay from a known good state |
| US4 | Platform engineer | Set checkpoint retention to 5 to bound disk usage | Long workflows don't fill the disk |

---

## 6. CLI Surface

```
tag workflow checkpoint <subcommand> [options]

Subcommands:
  list       List all checkpoints for a session
  show       Show the state dict at a specific checkpoint
  restore    Restore workflow state to a specific step
  delete     Delete checkpoints for a session
  prune      Delete all but the last N checkpoints

tag workflow checkpoint list <session-id>
tag workflow checkpoint show <session-id> --step N
tag workflow checkpoint restore <session-id> --step N
tag workflow checkpoint delete <session-id> [--step N | --all]
tag workflow checkpoint prune <session-id> --keep N

# Workflow execution options:
tag workflow run --goal GOAL [--checkpoint-interval 1] [--no-checkpoint] [--resume <session-id>]

Options:
  --checkpoint-interval N   Checkpoint every N steps (default: 1)
  --no-checkpoint           Disable checkpointing
  --resume SESSION_ID       Resume from most recent checkpoint for this session
  --keep N                  Number of checkpoints to retain per session (default: 100)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | After each workflow step, `SqliteCheckpointer.save(session_id, step_num, state)` serializes the state and writes a row to `workflow_checkpoints`. |
| FR-02 | Serialization: try msgpack first (`pip install msgpack`); fall back to JSON with `default=str` for non-serializable objects. |
| FR-03 | On `tag workflow run --resume <session-id>`: query the most recent checkpoint for the session, deserialize state, and start the workflow graph at `step_num + 1`. |
| FR-04 | `tag workflow checkpoint list` renders a table of (step, timestamp, state_size_bytes, serialization_format). |
| FR-05 | `tag workflow checkpoint show --step N` deserializes the checkpoint and renders the state dict as formatted JSON (with truncation for large values). |
| FR-06 | `tag workflow checkpoint restore --step N` returns the deserialized state dict to the caller (used by PRD-113 time-travel debugging). |
| FR-07 | Checkpoint pruning: after writing checkpoint step N, delete all checkpoints where `step_num < N - --keep`. |
| FR-08 | Thread safety: use `BEGIN IMMEDIATE` transaction for all checkpoint writes to serialize concurrent access. |
| FR-09 | On serialization failure (non-serializable state value): log a warning, skip the value with a `<non-serializable>` placeholder, and continue. |
| FR-10 | `--no-checkpoint` disables all checkpoint writes; workflow runs in streaming mode (no crash recovery). |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Checkpoint table indexed on `(session_id, step_num DESC)` for fast latest-checkpoint queries. |
| NFR-02 | Checkpoint blobs stored as SQLite BLOB columns (not external files) for atomicity. |
| NFR-03 | Checkpoint write must be completed before returning from the step function; async buffering not allowed (would lose state on crash). |
| NFR-04 | Maximum checkpoint blob size 10MB; larger states trigger a warning and a size reduction (drop large artifact blobs). |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS workflow_checkpoints (
  id              TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL,
  step_num        INTEGER NOT NULL,
  state_blob      BLOB NOT NULL,
  state_format    TEXT NOT NULL DEFAULT 'msgpack',  -- 'msgpack'|'json'
  state_size      INTEGER NOT NULL,
  created_at      TEXT NOT NULL,
  UNIQUE(session_id, step_num)
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_session_step
  ON workflow_checkpoints(session_id, step_num DESC);
```

### 9.2 Python core

```python
from __future__ import annotations
import json
import sqlite3
import uuid
from datetime import datetime, timezone
from typing import Any, Optional

try:
    import msgpack
    HAS_MSGPACK = True
except ImportError:
    HAS_MSGPACK = False

class SqliteCheckpointer:
    def __init__(self, db_path: str, keep: int = 100) -> None:
        self.db_path = db_path
        self.keep = keep

    def _conn(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path, timeout=10)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA busy_timeout=5000")
        return conn

    def save(self, session_id: str, step_num: int, state: dict) -> None:
        if HAS_MSGPACK:
            try:
                blob = msgpack.packb(state, use_bin_type=True)
                fmt = "msgpack"
            except Exception:
                blob = json.dumps(state, default=str).encode()
                fmt = "json"
        else:
            blob = json.dumps(state, default=str).encode()
            fmt = "json"
        conn = self._conn()
        with conn:
            conn.execute(
                "INSERT OR REPLACE INTO workflow_checkpoints"
                "(id,session_id,step_num,state_blob,state_format,state_size,created_at) VALUES(?,?,?,?,?,?,?)",
                (uuid.uuid4().hex[:8], session_id, step_num, blob, fmt, len(blob),
                 datetime.now(timezone.utc).isoformat())
            )
            # Prune old checkpoints
            conn.execute(
                "DELETE FROM workflow_checkpoints WHERE session_id=? AND step_num <= ?",
                (session_id, step_num - self.keep)
            )

    def load_latest(self, session_id: str) -> Optional[tuple[int, dict]]:
        conn = self._conn()
        row = conn.execute(
            "SELECT step_num, state_blob, state_format FROM workflow_checkpoints "
            "WHERE session_id=? ORDER BY step_num DESC LIMIT 1",
            (session_id,)
        ).fetchone()
        if not row:
            return None
        return row["step_num"], self._deserialize(row["state_blob"], row["state_format"])

    def load_step(self, session_id: str, step_num: int) -> Optional[dict]:
        conn = self._conn()
        row = conn.execute(
            "SELECT state_blob, state_format FROM workflow_checkpoints WHERE session_id=? AND step_num=?",
            (session_id, step_num)
        ).fetchone()
        if not row:
            return None
        return self._deserialize(row["state_blob"], row["state_format"])

    def list_checkpoints(self, session_id: str) -> list:
        conn = self._conn()
        return conn.execute(
            "SELECT step_num, state_size, state_format, created_at FROM workflow_checkpoints "
            "WHERE session_id=? ORDER BY step_num",
            (session_id,)
        ).fetchall()

    def _deserialize(self, blob: bytes, fmt: str) -> dict:
        if fmt == "msgpack" and HAS_MSGPACK:
            return msgpack.unpackb(blob, raw=False)
        return json.loads(blob.decode())
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Sensitive agent outputs in checkpoints | Checkpoints stored in `~/.tag/runtime/tag.sqlite3` (mode 0600); BLOB format not human-readable |
| Checkpoint blob injection | Checkpoint deserialization uses msgpack or JSON (not pickle); no arbitrary code execution |
| Disk exhaustion | `--keep N` pruning + 10MB blob size limit + warning on oversized states |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `save` + `load_latest` round-trip; msgpack vs JSON fallback; pruning logic |
| Integration | Simulate crash mid-workflow; `--resume` from checkpoint; verify step outputs match |
| Concurrency | Two threads writing checkpoints simultaneously; verify no corruption |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | After each workflow step, a checkpoint row exists in `workflow_checkpoints` |
| AC-02 | Killing the process at step 5 and running `tag workflow run --resume <id>` continues from step 5 |
| AC-03 | `tag workflow checkpoint list <id>` shows all checkpoint steps with timestamps |
| AC-04 | Checkpoint writes complete in < 50ms for a 100KB state dict |
| AC-05 | Checkpoints beyond `--keep N` are pruned automatically |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-109 HITL interrupt | Interrupt/resume requires checkpoint |
| PRD-112 graph-based workflow | Step execution framework |
| PRD-113 time-travel debugging | Restore-to-step functionality |
| msgpack (optional) | Compact binary serialization |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should checkpoint blobs be compressed (zlib/lz4) before storage? |
| OQ-02 | Should there be a checkpoint size budget per session to avoid unbounded growth? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | SQLite DDL, `SqliteCheckpointer` save/load, unit tests | 2 |
| 2 | Pruning, list/show/restore CLI commands | 1 |
| 3 | Integration with workflow engine (PRD-112), resume logic | 2 |
| 4 | Integration tests, concurrency tests, documentation | 1 |

