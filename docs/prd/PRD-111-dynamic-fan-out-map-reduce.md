# PRD-111: Dynamic Fan-Out/Map-Reduce (`tag workflow fan-out`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `workflow_engine.py + controller.py`
**Depends on:** PRD-112 (graph-based workflow), PRD-110 (state serialization), PRD-082 (multi-agent team primitives)
**Inspired by:** LangGraph Send API, AutoGen parallel execution, Dask graph scheduler, Ray tasks, CrewAI parallel process

---

## 1. Overview

Complex agent workflows often require processing a dynamic list of items in parallel — searching N documents simultaneously, running N code review agents in parallel, or spawning N summarization agents for N sections of a large document. TAG's current workflow model executes steps sequentially; there is no mechanism to fan out over a runtime-determined list of items, execute sub-agents in parallel, and reduce the results back into a single state.

Dynamic Fan-Out/Map-Reduce (`tag workflow fan-out`) introduces LangGraph-inspired `Send` API and map-reduce primitives to the TAG workflow engine. A workflow node can emit multiple `Send(node_name, state_update)` objects from a single conditional edge, causing the workflow engine to spawn parallel execution branches — one per Send — that all run concurrently and whose results are merged back into the parent state via a configurable reduce function.

The design follows LangGraph's `Send` API (introduced in 0.2.x) which enables dynamic parallelism within a graph, and the map-reduce pattern from distributed computing (Google MapReduce, Dask, Spark). Unlike those systems, TAG's implementation uses Python's `concurrent.futures.ThreadPoolExecutor` for local concurrency (no external workers), with SQLite checkpointing (PRD-110) for each parallel branch to support partial failure recovery.

---

## 2. Problem Statement

### 2.1 Sequential processing of large item lists is too slow

Summarizing 20 documents sequentially (20 × 30s = 10 minutes) is impractical when parallel execution (30s total) is possible. Without fan-out primitives, engineers must implement parallel execution manually using `threading` or `asyncio`, outside the workflow graph.

### 2.2 Dynamic fan-out not possible at graph design time

LangGraph's traditional edges connect specific named nodes. When the number of parallel branches is only known at runtime (e.g., number of search results), static graph edges cannot express the parallelism.

### 2.3 No structured reduction of parallel results

Even when engineers implement ad-hoc parallelism, there is no standard way to reduce parallel results back into a shared state dict, handle partial failures, or retry individual failed branches.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Provide a `Send(node_name, state_update)` return type that causes the workflow engine to spawn a parallel branch for each Send. |
| G2 | Execute all parallel branches concurrently using `ThreadPoolExecutor` with configurable max workers. |
| G3 | Provide a reduce node pattern: after all branches complete, a designated reduce node receives a list of all branch results and merges them into the shared state. |
| G4 | Support partial failure handling: on branch failure, either fail fast (default) or collect errors and continue with successful branches. |
| G5 | Checkpoint each branch independently (PRD-110) so partial failures can resume individual branches. |
| G6 | `tag workflow fan-out show <session-id>` displays a live view of parallel branch statuses. |
| G7 | Support nested fan-out (a branch can itself emit Sends). |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Distributed worker pool (all branches run in the same process). |
| NG2 | Streaming reduce (reduce node runs only after all branches complete). |
| NG3 | Load balancing across machines. |
| NG4 | GPU-accelerated parallel execution. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Parallel speedup | 10 parallel branches with 1s work each complete in < 3s (vs 10s sequential) | Benchmark test |
| Partial failure recovery | On 1/10 branch failure with `--on-error continue`, 9/10 results available | Integration test |
| State merge correctness | Reduce function receives all branch outputs in deterministic order | Unit test |
| Max fan-out | 100 parallel branches complete without error | Stress test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Fan out N document summarization agents in parallel | I process large document sets efficiently |
| US2 | Developer | Use a reduce node to aggregate all branch outputs into a final summary | I get a unified result from parallel work |
| US3 | Developer | Configure `--on-error continue` so one failed branch doesn't stop others | I get partial results even on failure |
| US4 | Developer | See live progress of parallel branches during execution | I know which branches are running vs. complete |

---

## 6. CLI Surface

```
# In workflow definition (Python API):
from tag.workflow_engine import Send, WorkflowGraph, reduce

def split_node(state):
    """Return a list of Send objects to fan out."""
    return [
        Send("process_doc", {"doc_id": doc_id, "content": content})
        for doc_id, content in state["documents"].items()
    ]

@reduce
def merge_node(state, branch_results):
    """Receives list of all branch state dicts."""
    state["summaries"] = [r["summary"] for r in branch_results]
    return state

graph = WorkflowGraph()
graph.add_node("split", split_node)
graph.add_node("process_doc", process_one_doc)
graph.add_node("merge", merge_node)
graph.add_conditional_edges("split", split_node)
graph.set_fan_out("split", reduce_node="merge", max_workers=10, on_error="continue")

# CLI:
tag workflow fan-out show <session-id> [--live]
tag workflow fan-out retry <session-id> --branch BRANCH_ID

Options:
  --max-workers N    Thread pool size (default: min(10, cpu_count))
  --on-error        fail_fast|continue (default: fail_fast)
  --live            Live-refresh branch status display
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | When a node returns `List[Send]`, the engine creates one branch per Send, each with its own `branch_id` and state update applied to a copy of the shared state. |
| FR-02 | All branches are submitted to `ThreadPoolExecutor` simultaneously; the engine blocks until all branches complete (or fail). |
| FR-03 | Each branch writes checkpoints (PRD-110) with key `(session_id, branch_id, step_num)`. |
| FR-04 | After all branches complete: collect all branch final states, invoke the reduce node with the list of states, and merge the result back into the shared state. |
| FR-05 | `--on-error fail_fast`: first branch failure immediately cancels all remaining branches and raises. |
| FR-06 | `--on-error continue`: collect branch errors in `state["branch_errors"]`, include successful branch results in reduce. |
| FR-07 | Reduce node receives `(shared_state, branch_results: List[dict])` as arguments. |
| FR-08 | Branch status tracked in `workflow_branches` SQLite table: `pending`, `running`, `completed`, `failed`. |
| FR-09 | `tag workflow fan-out show --live` polls `workflow_branches` every second and renders a live table. |
| FR-10 | `tag workflow fan-out retry <session-id> --branch BRANCH_ID` re-runs a specific failed branch from its last checkpoint. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | ThreadPoolExecutor size bounded by `min(--max-workers, cpu_count() * 2)`; never unbounded. |
| NFR-02 | Branch state is a copy of the shared state (deep copy) to prevent cross-branch mutation. |
| NFR-03 | Reduce function is called in the main thread after all branches complete; no reduce concurrency. |
| NFR-04 | Memory per branch: peak usage < 50MB for typical agent state; warn on states > 100MB. |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS workflow_branches (
  id              TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL,
  fan_out_step    INTEGER NOT NULL,
  branch_index    INTEGER NOT NULL,
  target_node     TEXT NOT NULL,
  status          TEXT NOT NULL DEFAULT 'pending',
  error           TEXT,
  created_at      TEXT NOT NULL,
  completed_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_branches_session
  ON workflow_branches(session_id, fan_out_step);
```

### 9.2 Python core

```python
from __future__ import annotations
import copy
import dataclasses
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any, Callable, List, Optional

@dataclasses.dataclass
class Send:
    node: str
    state_update: dict

def _execute_branch(node_fn: Callable, state: dict, send: Send) -> dict:
    branch_state = copy.deepcopy(state)
    branch_state.update(send.state_update)
    return node_fn(branch_state)

def fan_out_execute(
    sends: List[Send],
    node_registry: dict,
    shared_state: dict,
    reduce_fn: Callable,
    max_workers: int = 10,
    on_error: str = "fail_fast",
) -> dict:
    results = []
    errors = []
    with ThreadPoolExecutor(max_workers=min(max_workers, len(sends))) as pool:
        futures = {
            pool.submit(_execute_branch, node_registry[s.node], shared_state, s): i
            for i, s in enumerate(sends)
        }
        for future in as_completed(futures):
            idx = futures[future]
            try:
                results.append((idx, future.result()))
            except Exception as e:
                if on_error == "fail_fast":
                    for f in futures:
                        f.cancel()
                    raise
                errors.append((idx, str(e)))
    ordered = [r for _, r in sorted(results, key=lambda x: x[0])]
    merged = reduce_fn(shared_state, ordered)
    if errors:
        merged["branch_errors"] = {str(i): err for i, err in errors}
    return merged
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Thread safety of shared state | Branch receives a deep copy; only reduce node writes back |
| Unbounded branch creation | Hard cap at 1000 branches per fan-out; larger sends raise ValueError |
| Branch timeout | `ThreadPoolExecutor.shutdown(wait=True, cancel_futures_on_timeout=True)` with configurable timeout |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `fan_out_execute` with 5 mock branches; reduce correctness; `--on-error continue` with 1 failure |
| Integration | Full workflow with `Send` fan-out, reduce, and checkpoint resume |
| Stress | 100-branch fan-out completes without error |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | A node returning `[Send("x", {...}), Send("x", {...})]` spawns 2 parallel branches |
| AC-02 | 10 branches with 1s sleep each complete in < 3s wall time |
| AC-03 | Reduce node receives all branch results |
| AC-04 | On branch failure with `--on-error continue`, remaining branches complete and reduce runs |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-112 graph-based workflow | WorkflowGraph integration point |
| PRD-110 state serialization | Per-branch checkpoint |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should reduce functions be allowed to fail and trigger their own replan? |
| OQ-02 | Should branches support their own `--model` override for mixed-model fan-outs? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `Send` dataclass, `fan_out_execute`, unit tests | 2 |
| 2 | SQLite branch tracking, integration with PRD-112 | 2 |
| 3 | CLI (`fan-out show --live`, `retry`), error handling | 2 |
| 4 | Integration tests, stress test, documentation | 1 |
