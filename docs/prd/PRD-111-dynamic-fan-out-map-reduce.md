# PRD-111: Dynamic Fan-Out/Map-Reduce (`tag workflow fan-out`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `internal/queue + internal/cli`
**Depends on:** PRD-112 (graph-based workflow), PRD-110 (state serialization), PRD-082 (multi-agent team primitives)
**Inspired by:** LangGraph Send API, AutoGen parallel execution, Dask graph scheduler, Ray tasks, CrewAI parallel process

---

## 1. Overview

Complex agent workflows often require processing a dynamic list of items in parallel — searching N documents simultaneously, running N code review agents in parallel, or spawning N summarization agents for N sections of a large document. TAG's current workflow model executes steps sequentially; there is no mechanism to fan out over a runtime-determined list of items, execute sub-agents in parallel, and reduce the results back into a single state.

Dynamic Fan-Out/Map-Reduce (`tag workflow fan-out`) introduces a LangGraph-inspired `Send` API and map-reduce primitives to the TAG workflow engine. A workflow node can emit multiple `Send{Node, StateUpdate}` values from a single conditional edge, causing the workflow engine to spawn parallel execution branches — one per Send — that all run concurrently and whose results are merged back into the parent state via a configurable reduce function.

The design follows LangGraph's `Send` API (introduced in 0.2.x) which enables dynamic parallelism within a graph, and the map-reduce pattern from distributed computing (Google MapReduce, Dask, Spark). Concretely, fan-out is implemented on the bespoke SQLite-backed goroutine DAG scheduler in `internal/queue` (see GO_MIGRATION_PLAN.md decision (5)): each `Send` becomes a dynamically enqueued child job, the parallel "map" phase runs on a bounded goroutine worker pool over `golang.org/x/sync/errgroup` + channels, and the reduce node is a join/barrier node whose `deps_json` lists every map job so the scheduler promotes it to `ready` only once all branches are `done`. There are **no** external workers — River (Postgres) and asynq (Redis) are explicitly rejected because they break the single-binary mandate, and no drop-in pure-Go embedded durable DAG queue exists, so this scheduler is genuine engineering. Each parallel branch is event-sourced to the single `modernc.org/sqlite` store (pure-Go, WAL, `CGO_ENABLED=0`) via PRD-110 checkpointing to support partial failure recovery and replay.

---

## 2. Problem Statement

### 2.1 Sequential processing of large item lists is too slow

Summarizing 20 documents sequentially (20 × 30s = 10 minutes) is impractical when parallel execution (30s total) is possible. Without fan-out primitives, engineers must implement parallel execution manually using raw goroutines and `sync`/`errgroup`, outside the workflow graph.

### 2.2 Dynamic fan-out not possible at graph design time

LangGraph's traditional edges connect specific named nodes. When the number of parallel branches is only known at runtime (e.g., number of search results), static graph edges cannot express the parallelism.

### 2.3 No structured reduction of parallel results

Even when engineers implement ad-hoc parallelism, there is no standard way to reduce parallel results back into a shared state dict, handle partial failures, or retry individual failed branches.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Provide a `Send{Node, StateUpdate}` return type that causes the workflow engine to spawn a parallel branch for each Send. |
| G2 | Execute all parallel branches concurrently on a bounded goroutine worker pool (`errgroup` + a semaphore channel) with configurable max workers. |
| G3 | Provide a reduce node pattern: after all branches complete, a designated reduce (join/barrier) node receives a slice of all branch results and merges them into the shared state. |
| G4 | Support partial failure handling: on branch failure, either fail fast (default, via `errgroup` context cancellation) or collect errors and continue with successful branches. |
| G5 | Checkpoint each branch independently (PRD-110) to `modernc.org/sqlite` so partial failures can resume individual branches. |
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

```go
// In a workflow definition (Go API, internal/queue):
import "github.com/tag-agent/tag/internal/queue"

// SplitNode returns a slice of Send values to fan out.
func SplitNode(state queue.State) ([]queue.Send, error) {
    var sends []queue.Send
    for docID, content := range state.Map("documents") {
        sends = append(sends, queue.Send{
            Node:        "process_doc",
            StateUpdate: queue.State{"doc_id": docID, "content": content},
        })
    }
    return sends, nil
}

// MergeNode is the reduce/barrier node: it receives all branch results.
func MergeNode(state queue.State, branchResults []queue.State) (queue.State, error) {
    summaries := make([]any, 0, len(branchResults))
    for _, r := range branchResults {
        summaries = append(summaries, r["summary"])
    }
    state["summaries"] = summaries
    return state, nil
}

g := queue.NewWorkflowGraph()
g.AddNode("split", SplitNode)
g.AddNode("process_doc", ProcessOneDoc)
g.AddReduceNode("merge", MergeNode) // marks merge as a join/barrier node
g.AddConditionalEdges("split", SplitNode)
g.SetFanOut("split", queue.FanOut{ReduceNode: "merge", MaxWorkers: 10, OnError: "continue"})
```

```
# CLI:
tag workflow fan-out show <session-id> [--live]
tag workflow fan-out retry <session-id> --branch BRANCH_ID

Options:
  --max-workers N    Worker-pool size (default: min(10, runtime.NumCPU()))
  --on-error        fail_fast|continue (default: fail_fast)
  --live            Live-refresh branch status display
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | When a node returns `[]Send`, the engine enqueues one child job per Send, each with its own `branch_id` and state update applied to a deep copy of the shared state. |
| FR-02 | All branches are dispatched to the bounded goroutine worker pool concurrently; the scheduler blocks (on the reduce barrier node) until all branches complete or fail. |
| FR-03 | Each branch writes checkpoints (PRD-110) with key `(session_id, branch_id, step_num)` to `modernc.org/sqlite`. |
| FR-04 | After all branches complete: collect all branch final states, invoke the reduce node with the slice of states, and merge the result back into the shared state. |
| FR-05 | `--on-error fail_fast`: the first branch failure cancels the shared `context.Context` via `errgroup`, stopping all remaining branches, and `RunFanOut` returns the error. |
| FR-06 | `--on-error continue`: collect branch errors in `state["branch_errors"]`, include successful branch results in reduce. |
| FR-07 | Reduce node receives `(sharedState queue.State, branchResults []queue.State)` as arguments. |
| FR-08 | Branch status tracked in the `workflow_branches` SQLite table: `pending`, `running`, `completed`, `failed`. |
| FR-09 | `tag workflow fan-out show --live` polls `workflow_branches` every second and renders a live table (`lipgloss`/`bubbles` table). |
| FR-10 | `tag workflow fan-out retry <session-id> --branch BRANCH_ID` re-runs a specific failed branch from its last checkpoint. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Worker-pool concurrency bounded by `min(--max-workers, runtime.NumCPU() * 2)` via a semaphore channel; never unbounded goroutine spawn. |
| NFR-02 | Branch state is a deep copy of the shared state (no aliased maps/slices) to prevent cross-branch mutation. |
| NFR-03 | The reduce function runs on the scheduler goroutine after all branches complete; no reduce concurrency. |
| NFR-04 | Memory per branch: peak usage < 50MB for typical agent state; warn on states > 100MB. |

---

## 9. Technical Design

### 9.1 SQLite DDL

Created by a `internal/store` migration against the single pure-Go `modernc.org/sqlite` connection (WAL, `CGO_ENABLED=0`); all writes go through the single-writer + `flock` atomic RMW contract.

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

### 9.2 Go core (`internal/queue`)

The map phase runs on `errgroup` with a semaphore channel for bounded concurrency; branch ordering is preserved by index rather than completion order (`errgroup.Wait` replaces `as_completed`). `fail_fast` uses the group's derived `context.Context`; `continue` records per-branch errors.

```go
package queue

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"sync"

	"golang.org/x/sync/errgroup"
)

type State map[string]any

type Send struct {
	Node        string
	StateUpdate State
}

type NodeFunc func(State) (State, error)
type ReduceFunc func(State, []State) (State, error)

// deepCopy returns a value-independent clone so branches cannot alias the parent.
func deepCopy(s State) State { /* recursive clone of maps/slices/scalars */ return s }

func executeBranch(fn NodeFunc, shared State, send Send) (State, error) {
	bs := deepCopy(shared)
	for k, v := range send.StateUpdate {
		bs[k] = v
	}
	return fn(bs)
}

func RunFanOut(
	ctx context.Context,
	sends []Send,
	registry map[string]NodeFunc,
	shared State,
	reduce ReduceFunc,
	maxWorkers int,
	onError string, // "fail_fast" | "continue"
) (State, error) {
	if maxWorkers <= 0 || maxWorkers > runtime.NumCPU()*2 {
		maxWorkers = min(maxWorkers, runtime.NumCPU()*2)
	}
	results := make([]State, len(sends)) // indexed => deterministic order
	branchErrs := map[string]string{}
	var mu sync.Mutex

	g, ctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, min(maxWorkers, len(sends)))
	for i, s := range sends {
		i, s := i, s
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			defer func() { <-sem }()

			out, err := executeBranch(registry[s.Node], shared, s)
			if err != nil {
				if onError == "fail_fast" {
					return fmt.Errorf("branch %d: %w", i, err) // cancels ctx -> stops peers
				}
				mu.Lock()
				branchErrs[strconv.Itoa(i)] = err.Error()
				mu.Unlock()
				return nil
			}
			results[i] = out
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Drop nil slots (failed branches under "continue") before reduce.
	ordered := make([]State, 0, len(results))
	for _, r := range results {
		if r != nil {
			ordered = append(ordered, r)
		}
	}
	merged, err := reduce(shared, ordered)
	if err != nil {
		return nil, err
	}
	if len(branchErrs) > 0 {
		merged["branch_errors"] = branchErrs
	}
	return merged, nil
}
```

In the persisted scheduler path, `RunFanOut` is expressed declaratively as DAG jobs: each `Send` is an enqueued child job row, and the reduce node is a barrier whose `deps_json` names every child. The in-memory `RunFanOut` above is the equivalent single-shot form used for small, non-durable fan-outs and for unit tests.

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Data race on shared state | Each branch receives a deep copy; only the reduce node (on the scheduler goroutine) writes back. Verified under `go test -race`. |
| Unbounded branch creation | Hard cap at 1000 branches per fan-out; larger `[]Send` returns an error before any goroutine is spawned. |
| Branch timeout | A per-fan-out `context.WithTimeout` cancels all in-flight branches; workers observe `ctx.Done()` and child processes are killed via `Setpgid` process-group signal. |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Table-driven `RunFanOut` with 5 fake branches; reduce correctness; `--on-error continue` with 1 failure; all run under `go test -race` to prove no shared-state data race |
| Integration | Full workflow with `Send` fan-out, reduce, and checkpoint resume against a temp `modernc.org/sqlite` DB |
| Stress | 100-branch fan-out completes without error; assert bounded goroutine count |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | A node returning `[]Send{{Node: "x", ...}, {Node: "x", ...}}` spawns 2 parallel branches |
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
| 1 | `Send` struct, `RunFanOut` (errgroup + semaphore + deep copy), table-driven unit tests | 2 |
| 2 | SQLite branch tracking (`workflow_branches`), integration with PRD-112 declarative DAG jobs | 2 |
| 3 | CLI (`fan-out show --live`, `retry`) cobra handlers, error handling | 2 |
| 4 | Integration tests, `-race` + stress test, documentation | 1 |

