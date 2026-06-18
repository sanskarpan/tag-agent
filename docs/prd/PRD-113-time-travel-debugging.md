# PRD-113: Time-Travel Debugging (`tag workflow rewind`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `workflow_engine.py + controller.py`
**Depends on:** PRD-110 (state serialization/checkpointing), PRD-112 (graph-based workflow engine)
**Inspired by:** LangGraph time-travel debugging, Redux DevTools state inspector, Temporal.io workflow replay, rr (Mozilla record-replay)

---

## 1. Overview

Debugging complex agent workflows is notoriously difficult: when a 50-step workflow produces a wrong answer at step 47, the engineer must re-run the entire workflow (incurring time and cost) to reproduce the bug. LangGraph's Studio offers "time-travel" debugging — the ability to rewind a workflow to a previous step, modify the state, and re-execute from that point. TAG has no equivalent.

Time-Travel Debugging (`tag workflow rewind`) builds on the checkpoint infrastructure (PRD-110) to add full workflow replay and fork capabilities. Engineers can inspect any historical state snapshot, modify the state dict, and fork a new execution branch from any checkpoint. The fork creates a new session that diverges from the original at the chosen step, allowing "what if" experiments without re-running prior steps.

The design is inspired by LangGraph's `update_state()` + `graph.stream(None, config, stream_mode="values")` time-travel pattern, Redux DevTools' step inspector, and rr's record-replay debugging. TAG's implementation is terminal-first: a TUI inspector (`tag workflow rewind <session-id>`) walks through checkpoints interactively.

---

## 2. Problem Statement

### 2.1 No way to inspect intermediate workflow state

When a workflow fails at step 47, the engineer can only see the final error. Checkpoints (PRD-110) store intermediate state, but there is no user-facing tool to inspect them.

### 2.2 Re-running from scratch is expensive

A 60-step workflow costs $15 and 40 minutes. Re-running from scratch to reproduce a bug at step 47 wastes $12 and 35 minutes of prior steps that ran correctly.

### 2.3 No "what if" experimentation

Engineers often want to test "what if I had given the agent different context at step 30?" This requires forking the execution from step 30 — impossible without time-travel.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag workflow rewind <session-id>` opens an interactive TUI showing all checkpoint steps with timestamps and brief state summaries. |
| G2 | From the TUI, select any step to inspect the full state dict at that checkpoint. |
| G3 | `tag workflow rewind <session-id> --step N --fork` creates a new session that begins executing from step N with the original state at that checkpoint. |
| G4 | `tag workflow rewind <session-id> --step N --fork --patch '{"key": "value"}'` forks with a modified state dict (patches applied before re-execution). |
| G5 | Fork sessions maintain a `forked_from` reference to the original session for lineage tracking. |
| G6 | `tag workflow rewind show <session-id> --step N` non-interactively prints the state at step N as JSON. |
| G7 | Support diff view: `--diff N1 N2` shows the state changes between step N1 and N2. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Visual GUI time-travel debugger. Terminal TUI only. |
| NG2 | Replaying individual tool calls (only full node replay). |
| NG3 | Modifying the graph structure on fork (only state modification). |
| NG4 | Distributed session forking across machines. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Fork launch time | Fork session starts in < 500ms (checkpoint load + new session init) | Benchmark test |
| State inspection latency | TUI renders any checkpoint state in < 100ms | Benchmark test |
| Fork lineage tracking | `forked_from` session reference visible in `tag workflow graph list` | Integration test |
| Diff accuracy | `--diff N1 N2` shows exactly the keys that changed between steps | Unit test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Inspect intermediate workflow state at any step | I diagnose what went wrong without re-running |
| US2 | Developer | Fork from step 30 to test a different agent prompt | I experiment without re-executing 30 steps |
| US3 | Developer | Apply a state patch before forking | I correct a bad intermediate state and continue |
| US4 | Developer | See the diff between two steps | I understand what changed between checkpoints |

---

## 6. CLI Surface

```
tag workflow rewind <session-id> [options]

Options:
  --step N              Step number to rewind to (omit for interactive TUI)
  --fork                Create a new forked session from step N
  --patch JSON          JSON patch to apply to state before forking
  --diff N1 N2          Show state diff between steps N1 and N2
  --format json|table   Output format for state inspection

Examples:
  tag workflow rewind abc123                          # Interactive TUI
  tag workflow rewind abc123 --step 30               # Show state at step 30
  tag workflow rewind abc123 --step 30 --fork         # Fork new session from step 30
  tag workflow rewind abc123 --step 30 --fork --patch '{"query": "different query"}'
  tag workflow rewind abc123 --diff 29 31            # Show changes from step 29 to 31

TUI controls:
  j/k     Navigate steps
  Enter   Inspect state at current step
  f       Fork from current step
  p       Fork with patch (opens editor)
  d       Diff with previous step
  q       Quit
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag workflow rewind <session-id>` (no step) opens TUI showing all checkpoint steps with step number, timestamp, and top-3 changed keys. |
| FR-02 | TUI step selection loads the state dict from PRD-110 `SqliteCheckpointer.load_step()` and renders it as paginated JSON. |
| FR-03 | `--fork` creates a new `workflow_sessions` row with `forked_from = original_session_id, forked_at_step = N`. |
| FR-04 | Fork execution: load state at step N, apply optional `--patch`, resume `CompiledGraph.run()` from step N+1 using the forked state. |
| FR-05 | `--patch JSON` is applied as a shallow merge over the checkpoint state dict; conflicting keys overwritten. |
| FR-06 | `--diff N1 N2` loads both checkpoint states, computes the symmetric difference of keys and changed values, and renders as a colored diff table. |
| FR-07 | `tag workflow graph list` shows `forked_from` column for forked sessions. |
| FR-08 | Fork maintains all prior checkpoints from the original session for further time-travel debugging. |
| FR-09 | TUI renders key-value state summary (truncated to 80 chars per value) for navigation; full state shown on Enter. |
| FR-10 | `tag workflow rewind show <session-id> --step N` is a non-interactive version of TUI step inspection. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | TUI must work in any 80×24 terminal; use `rich` for rendering. |
| NFR-02 | State diffs computed in-process without external diff tools. |
| NFR-03 | Fork creates a new SQLite session row; does not copy checkpoint blobs (they remain referenced by original session ID + step). |
| NFR-04 | `--patch` validated as valid JSON before fork; malformed JSON raises a clear error. |

---

## 9. Technical Design

### 9.1 SQLite changes

```sql
-- Add forked_from column to workflow_sessions:
ALTER TABLE workflow_sessions ADD COLUMN forked_from TEXT;
ALTER TABLE workflow_sessions ADD COLUMN forked_at_step INTEGER;
```

### 9.2 Python core

```python
from __future__ import annotations
import copy
import json
from typing import Optional

class TimeTravel:
    def __init__(self, checkpointer, graph: "CompiledGraph") -> None:
        self.cp = checkpointer
        self.graph = graph

    def fork(self, session_id: str, step: int,
             patch: Optional[dict] = None) -> str:
        import uuid
        # Load state at step
        state = self.cp.load_step(session_id, step)
        if state is None:
            raise ValueError(f"No checkpoint at step {step} for session {session_id}")
        if patch:
            state = {**state, **patch}
        # Create new forked session
        fork_id = uuid.uuid4().hex[:8]
        # Copy checkpoints from original up to step so the new session has history
        from datetime import datetime, timezone
        now = datetime.now(timezone.utc).isoformat()
        # Run from fork point
        final = self.graph.run(state, session_id=fork_id)
        return fork_id

    def diff(self, session_id: str, step1: int, step2: int) -> dict:
        s1 = self.cp.load_step(session_id, step1) or {}
        s2 = self.cp.load_step(session_id, step2) or {}
        all_keys = set(s1.keys()) | set(s2.keys())
        changes = {}
        for k in all_keys:
            v1, v2 = s1.get(k), s2.get(k)
            if v1 != v2:
                changes[k] = {"before": v1, "after": v2}
        return changes
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Patch injection enabling malicious state | `--patch` only merges top-level keys; deeply nested injection requires access to the fork command |
| Forking re-executing expensive operations | Fork continues from step N, not from START; no re-execution of prior steps |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `diff()` correctness on known state pair; `fork()` state patch application |
| Integration | Fork from step 5; verify new session runs from step 6 with patched state |
| TUI | Keyboard simulation: navigate steps, press `f`, verify fork created |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag workflow rewind <id>` shows all checkpoint steps in TUI |
| AC-02 | `--step N --fork` creates a new session that runs from step N+1 |
| AC-03 | `--patch '{"x": 1}'` applies x=1 to state before fork |
| AC-04 | `--diff 5 10` shows keys changed between steps 5 and 10 |
| AC-05 | Forked session shows `forked_from` in `tag workflow graph list` |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-110 state serialization | Checkpoint load/restore infrastructure |
| PRD-112 graph-based workflow | `CompiledGraph.run()` for fork re-execution |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should forks be limited in depth (fork-of-fork-of-fork)? |
| OQ-02 | Should there be a cost estimate before forking a long workflow? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `TimeTravel.fork()`, `diff()`, SQLite session updates | 2 |
| 2 | TUI inspector (navigation, state render, keyboard shortcuts) | 2 |
| 3 | CLI commands, `--patch` validation, `--diff` rendering | 2 |
| 4 | Integration tests, documentation | 1 |

