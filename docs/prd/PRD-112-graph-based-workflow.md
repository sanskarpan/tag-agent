# PRD-112: Graph-Based Workflow Engine (`tag workflow graph`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** L (8-13 days)
**Category:** Workflow State
**Affects:** `workflow_engine.py + controller.py`
**Depends on:** PRD-110 (state serialization), PRD-111 (dynamic fan-out), PRD-109 (HITL interrupt), PRD-082 (multi-agent team)
**Inspired by:** LangGraph StateGraph, AutoGen 0.4 event-driven agents, Dagger.io pipeline graphs, Prefect task graphs

---

## 1. Overview

TAG's current task execution is a flat sequential loop: one agent, one task, linear steps. This is insufficient for real-world workflows that require conditional branching (retry on failure, choose path based on model output), parallel execution (PRD-111 fan-out), cyclical retries (loop until condition met), and reusable sub-graphs (shared helper workflows).

Graph-Based Workflow Engine (`tag workflow graph`) introduces a `WorkflowGraph` API modeled after LangGraph's `StateGraph`: engineers define nodes (Python functions that receive and return a state dict), connect them with edges (static or conditional), and compile the graph to a runnable workflow. The engine executes the graph step by step, checkpointing state (PRD-110) after each node, supporting interrupt/resume (PRD-109), dynamic fan-out (PRD-111), and time-travel debugging (PRD-113).

The design is directly inspired by LangGraph (Python-native, state-based graph execution), Prefect 2.x (task graphs with automatic retry and state persistence), and Temporal.io (workflow-as-code with durable execution). TAG's implementation is simpler than these production systems — it uses SQLite for state persistence rather than a workflow database, and Python functions rather than remote task workers — but provides the same core abstractions: nodes, edges, conditional routing, cyclic graphs, and compiled workflows.

---

## 2. Problem Statement

### 2.1 Sequential execution cannot express real workflows

Most real agent workflows require branching: "if the code review found errors, run the fix agent; otherwise, proceed to the deploy agent." Sequential execution cannot express this without ad-hoc if/else logic outside the workflow framework.

### 2.2 No reusable workflow components

Today, each TAG run is a standalone script. There is no mechanism to define a reusable "research and summarize" sub-workflow and call it from multiple parent workflows.

### 2.3 Cyclic retries require manual loop logic

Many agent workflows need a "try → evaluate → retry if failed" loop. This requires manual while-loop code outside any framework, with no built-in stall detection or max-iteration limits.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `WorkflowGraph` API: `add_node(name, fn)`, `add_edge(from, to)`, `add_conditional_edges(from, router_fn)`, `compile()`. |
| G2 | Compiled graph is executable: `graph.run(initial_state)` executes nodes, following edges, updating state at each step. |
| G3 | Support cyclic graphs (retry loops) with a configurable max-iteration limit per cycle. |
| G4 | Named START and END nodes; conditional edges route to END to terminate the graph. |
| G5 | Subgraph support: a node can wrap another compiled `WorkflowGraph` instance. |
| G6 | `tag workflow graph describe <file>` loads a workflow file and prints a text-art graph representation. |
| G7 | Persist execution history to SQLite `workflow_sessions` table; each step is a `workflow_steps` row. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Visual GUI graph editor. |
| NG2 | Remote task execution or distributed workers. |
| NG3 | YAML/JSON workflow definition format (Python-only API in this PRD). |
| NG4 | Event-driven reactive execution (push-based). All execution is pull-based/synchronous. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Graph compile time | < 10ms for a 20-node graph | Benchmark test |
| Step execution overhead | < 5ms overhead per node (excluding node function execution) | Benchmark test |
| Cycle detection | Detect infinite loops within `--max-iterations` (default: 50) | Unit test |
| Subgraph nesting | 3-level deep subgraph executes correctly | Integration test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Define a workflow with conditional branching in Python | I express complex agent logic without ad-hoc code |
| US2 | Developer | Wrap a research workflow as a subgraph in a larger analysis workflow | I reuse workflow components |
| US3 | Developer | Use cyclic edges to retry until a quality threshold is met | I build self-improving agent loops |
| US4 | Developer | Inspect a workflow's graph structure with `tag workflow graph describe` | I understand a workflow without executing it |

---

## 6. CLI Surface

```python
# Python API (workflow_engine.py):
from tag.workflow_engine import WorkflowGraph, START, END

def research_node(state: dict) -> dict:
    state["research"] = call_agent("research", state["query"])
    return state

def quality_check(state: dict) -> str:
    if state["research"]["score"] >= 0.8:
        return "write"
    return "research"  # retry

graph = WorkflowGraph()
graph.add_node("research", research_node)
graph.add_node("write", write_node)
graph.add_edge(START, "research")
graph.add_conditional_edges("research", quality_check, {"write": "write", "research": "research"})
graph.add_edge("write", END)

compiled = graph.compile(checkpointer=SqliteCheckpointer(db_path), max_iterations=10)
result = compiled.run({"query": "summarize AI papers"}, session_id="my-session")
```

```
# CLI:
tag workflow graph describe workflow.py [--format text|mermaid]
tag workflow graph run workflow.py --initial-state '{"query": "..."}' [--session-id ID]
tag workflow graph list [--status running|completed|failed]
tag workflow graph show <session-id>

Options:
  --format text|mermaid    Output format for graph describe
  --initial-state JSON     Initial state dict as JSON string
  --session-id ID          Use existing session (for resume)
  --max-iterations N       Max cycles per loop (default: 50)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `WorkflowGraph.add_node(name, fn)` registers a node; `fn` receives the current state dict and returns an updated state dict. |
| FR-02 | `add_edge(from, to)` adds a static edge; `add_conditional_edges(from, router_fn, mapping)` adds a conditional edge where `router_fn` returns a string key mapped to the next node. |
| FR-03 | `compile()` validates the graph (no orphan nodes, START/END reachable, no duplicate names) and returns a `CompiledGraph`. |
| FR-04 | `CompiledGraph.run(initial_state)` executes nodes in topological order, following edges, until END is reached. |
| FR-05 | At each step: call the current node function, write checkpoint (PRD-110), update `workflow_steps` row, determine next node via edge. |
| FR-06 | Cycle detection: track visit count per node per run; if a node is visited more than `max_iterations` times, raise `CycleError`. |
| FR-07 | Subgraph node: `add_node("sub", compiled_subgraph)` — when executed, runs the subgraph with the current state and merges its final state. |
| FR-08 | `tag workflow graph describe` imports the workflow file, calls `graph.describe()`, and renders a text-art adjacency list or Mermaid diagram. |
| FR-09 | `workflow_steps` table records: session_id, step_num, node_name, input_state_hash, output_state_hash, duration_ms, status. |
| FR-10 | `END` node: when a node returns or `router_fn` maps to `END`, execution terminates and `final_state` is returned. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Graph compilation must detect cycles that don't include END (infinite loops with no escape) and raise a warning, but allow intentional retry cycles that have a conditional edge to END. |
| NFR-02 | Node functions are called in the main thread; no implicit threading. Fan-out is explicit via PRD-111 Send. |
| NFR-03 | State dict is passed by value (shallow copy) to each node to prevent accidental cross-node mutation. |
| NFR-04 | Graph definition is serializable to JSON for storage and version control. |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS workflow_sessions (
  id              TEXT PRIMARY KEY,
  workflow_name   TEXT,
  profile         TEXT,
  status          TEXT NOT NULL DEFAULT 'running',
  initial_state   TEXT,  -- JSON
  final_state     TEXT,  -- JSON
  step_count      INTEGER NOT NULL DEFAULT 0,
  created_at      TEXT NOT NULL,
  completed_at    TEXT
);

CREATE TABLE IF NOT EXISTS workflow_steps (
  id              TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL REFERENCES workflow_sessions(id),
  step_num        INTEGER NOT NULL,
  node_name       TEXT NOT NULL,
  duration_ms     REAL,
  status          TEXT NOT NULL DEFAULT 'completed',
  error           TEXT,
  created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_steps_session
  ON workflow_steps(session_id, step_num);
```

### 9.2 Python core

```python
from __future__ import annotations
import copy
import uuid
from typing import Any, Callable, Dict, Optional

START = "__start__"
END = "__end__"

class WorkflowGraph:
    def __init__(self) -> None:
        self._nodes: Dict[str, Callable] = {}
        self._edges: Dict[str, Any] = {}

    def add_node(self, name: str, fn: Callable) -> None:
        self._nodes[name] = fn

    def add_edge(self, from_node: str, to_node: str) -> None:
        self._edges[from_node] = to_node

    def add_conditional_edges(self, from_node: str, router_fn: Callable,
                              mapping: Optional[Dict[str, str]] = None) -> None:
        self._edges[from_node] = (router_fn, mapping or {})

    def compile(self, checkpointer=None, max_iterations: int = 50) -> "CompiledGraph":
        return CompiledGraph(
            nodes=dict(self._nodes),
            edges=dict(self._edges),
            checkpointer=checkpointer,
            max_iterations=max_iterations,
        )

    def describe(self) -> str:
        lines = ["WorkflowGraph:"]
        for node in self._nodes:
            edge = self._edges.get(node)
            if edge is None:
                lines.append(f"  {node} → (no edge)")
            elif isinstance(edge, str):
                lines.append(f"  {node} → {edge}")
            else:
                router_fn, mapping = edge
                lines.append(f"  {node} → conditional({list(mapping.values())})")
        return "\n".join(lines)

class CompiledGraph:
    def __init__(self, nodes: dict, edges: dict, checkpointer=None,
                 max_iterations: int = 50) -> None:
        self.nodes = nodes
        self.edges = edges
        self.checkpointer = checkpointer
        self.max_iterations = max_iterations

    def run(self, initial_state: dict, session_id: Optional[str] = None) -> dict:
        session_id = session_id or uuid.uuid4().hex[:8]
        state = copy.copy(initial_state)
        current = START
        visit_count: dict = {}
        step_num = 0
        # Resume from checkpoint
        if self.checkpointer:
            resume = self.checkpointer.load_latest(session_id)
            if resume:
                step_num_restored, state = resume
                step_num = step_num_restored + 1
        while current != END:
            if current not in (START, END) and current in self.nodes:
                count = visit_count.get(current, 0) + 1
                if count > self.max_iterations:
                    raise RuntimeError(f"CycleError: node '{current}' visited {count} times")
                visit_count[current] = count
                fn = self.nodes[current]
                # Handle subgraph
                if isinstance(fn, CompiledGraph):
                    state = fn.run(state, session_id=f"{session_id}_{current}")
                else:
                    state = fn(copy.copy(state)) or state
                if self.checkpointer:
                    self.checkpointer.save(session_id, step_num, state)
                step_num += 1
            # Determine next node
            edge = self.edges.get(current)
            if edge is None:
                break
            elif isinstance(edge, str):
                current = edge
            else:
                router_fn, mapping = edge
                key = router_fn(state)
                current = mapping.get(key, key)
        return state
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Arbitrary code execution via workflow file | `tag workflow graph run` warns that workflow files are trusted code; no sandboxing |
| Infinite loop consuming resources | `max_iterations` hard cap; `CycleError` raised |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Compile/run 5-node linear graph; conditional edge routing; cycle detection at max_iterations |
| Integration | Workflow with interrupt, fan-out, and checkpoint resume |
| Correctness | State dict passed by copy; node mutations don't bleed across |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `WorkflowGraph` with START → A → B → END compiles and runs correctly |
| AC-02 | Conditional edge routes to correct next node based on router function |
| AC-03 | Cyclic graph with `max_iterations=3` raises `CycleError` after 3 visits |
| AC-04 | `tag workflow graph describe workflow.py` prints a text representation |
| AC-05 | Subgraph node executes and merges state correctly |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-110 state serialization | Step checkpointing |
| PRD-109 HITL interrupt | Interrupt in nodes |
| PRD-111 dynamic fan-out | Send API integration |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should workflow definitions be storable in the DB for discovery and sharing? |
| OQ-02 | Should there be a YAML/JSON workflow format for non-Python users? |

---

## 15. Complexity & Timeline

**Complexity:** Large (L)
**Estimated effort:** 8–13 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `WorkflowGraph`, `CompiledGraph`, basic execution, unit tests | 3 |
| 2 | Checkpoint integration, cycle detection, subgraph support | 2 |
| 3 | SQLite session/step tracking, `describe` command | 2 |
| 4 | CLI (`graph run`, `graph list`, `graph show`) | 2 |
| 5 | Integration tests, documentation | 2 |
