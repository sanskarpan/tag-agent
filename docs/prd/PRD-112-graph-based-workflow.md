# PRD-112: Graph-Based Workflow Engine (`tag workflow graph`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** L (8-13 days)
**Category:** Workflow State
**Affects:** `internal/queue + internal/cli`
**Depends on:** PRD-110 (state serialization), PRD-111 (dynamic fan-out), PRD-109 (HITL interrupt), PRD-082 (multi-agent team)
**Inspired by:** LangGraph StateGraph, AutoGen 0.4 event-driven agents, Dagger.io pipeline graphs, Prefect task graphs

---

## 1. Overview

TAG's current task execution is a flat sequential loop: one agent, one task, linear steps. This is insufficient for real-world workflows that require conditional branching (retry on failure, choose path based on model output), parallel execution (PRD-111 fan-out), cyclical retries (loop until condition met), and reusable sub-graphs (shared helper workflows).

Graph-Based Workflow Engine (`tag workflow graph`) introduces a `WorkflowGraph` API modeled after LangGraph's `StateGraph`: engineers define nodes (Go functions that receive and return a state map), connect them with edges (static or conditional), and compile the graph to a runnable workflow. The engine executes the graph step by step, checkpointing state (PRD-110) after each node, supporting interrupt/resume (PRD-109), dynamic fan-out (PRD-111), and time-travel debugging (PRD-113).

The design is directly inspired by LangGraph (state-based graph execution), Prefect 2.x (task graphs with automatic retry and state persistence), and Temporal.io (workflow-as-code with durable execution). Concretely, the compiled graph is validated and scheduled by the bespoke SQLite-backed goroutine DAG engine in `internal/queue` (GO_MIGRATION_PLAN.md decision (5)): the declarative nodes+edges are validated acyclic with Kahn's algorithm at compile time, static and conditional edges become scheduler transitions (conditional edges are predicates evaluated by the scheduler goroutine), and dynamic fan-out (PRD-111) is layered on the same engine. TAG's implementation is simpler than the production systems it draws from — it uses the single `modernc.org/sqlite` store (pure-Go, WAL, `CGO_ENABLED=0`) for event-sourced state persistence rather than a workflow database, and in-process Go functions rather than remote task workers, and it reaches for **no** external durable-queue library (River/asynq both break the single-binary mandate; no drop-in pure-Go embedded DAG queue exists) — but provides the same core abstractions: nodes, edges, conditional routing, cyclic graphs, and compiled workflows. For heavier static graph analytics, `gonum.org/v1/gonum` graph packages are available, but the scheduler itself uses a hand-rolled adjacency list + Kahn's algorithm.

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
| G1 | `WorkflowGraph` API: `AddNode(name, fn)`, `AddEdge(from, to)`, `AddConditionalEdges(from, routerFn)`, `Compile()`. |
| G2 | Compiled graph is executable: `graph.Run(ctx, initialState)` executes nodes, following edges, updating state at each step. |
| G3 | Support cyclic graphs (retry loops) with a configurable max-iteration limit per cycle. |
| G4 | Named START and END sentinel nodes; conditional edges route to END to terminate the graph. |
| G5 | Subgraph support: a node can wrap another compiled `WorkflowGraph` instance. |
| G6 | `tag workflow graph describe <file>` loads a workflow definition and prints a text-art graph representation. |
| G7 | Persist execution history to the SQLite `workflow_sessions` table; each step is a `workflow_steps` row. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Visual GUI graph editor. |
| NG2 | Remote task execution or distributed workers. |
| NG3 | YAML/JSON workflow definition format (Go-only API in this PRD). |
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
| US1 | Developer | Define a workflow with conditional branching in Go | I express complex agent logic without ad-hoc code |
| US2 | Developer | Wrap a research workflow as a subgraph in a larger analysis workflow | I reuse workflow components |
| US3 | Developer | Use cyclic edges to retry until a quality threshold is met | I build self-improving agent loops |
| US4 | Developer | Inspect a workflow's graph structure with `tag workflow graph describe` | I understand a workflow without executing it |

---

## 6. CLI Surface

```go
// Go API (internal/queue):
import "github.com/tag-agent/tag/internal/queue"

func researchNode(state queue.State) (queue.State, error) {
    state["research"] = callAgent("research", state["query"])
    return state, nil
}

func qualityCheck(state queue.State) string {
    if score(state, "research") >= 0.8 {
        return "write"
    }
    return "research" // retry
}

g := queue.NewWorkflowGraph()
g.AddNode("research", researchNode)
g.AddNode("write", writeNode)
g.AddEdge(queue.START, "research")
g.AddConditionalEdges("research", qualityCheck, map[string]string{"write": "write", "research": "research"})
g.AddEdge("write", queue.END)

compiled, err := g.Compile(queue.WithCheckpointer(queue.NewSQLiteCheckpointer(db)), queue.WithMaxIterations(10))
result, err := compiled.Run(ctx, queue.State{"query": "summarize AI papers"}, queue.WithSessionID("my-session"))
```

```
# CLI:
tag workflow graph describe <workflow> [--format text|mermaid]
tag workflow graph run <workflow> --initial-state '{"query": "..."}' [--session-id ID]
tag workflow graph list [--status running|completed|failed]
tag workflow graph show <session-id>

Options:
  --format text|mermaid    Output format for graph describe
  --initial-state JSON     Initial state dict as JSON string
  --session-id ID          Use existing session (for resume)
  --max-iterations N       Max cycles per loop (default: 50)
```

`<workflow>` is the name of a graph registered in the binary via an explicit `queue.Register(name, buildFn)` call (workflows are compiled-in Go definitions — per the single-binary/no-dynamic-plugin decision in GO_MIGRATION_PLAN.md (6), there is no runtime code loading of `.py`/`.go` files). `describe`/`run` resolve the registered `WorkflowGraph`, compile it, and operate on the resulting DAG.

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `WorkflowGraph.AddNode(name, fn)` registers a node; `fn` (a `NodeFunc`) receives the current state map and returns an updated state map + error. |
| FR-02 | `AddEdge(from, to)` adds a static edge; `AddConditionalEdges(from, routerFn, mapping)` adds a conditional edge where `routerFn` returns a string key mapped to the next node. |
| FR-03 | `Compile()` validates the graph (no orphan nodes, START/END reachable, no duplicate names, acyclic-with-escape via Kahn) and returns a `*CompiledGraph` (or an error). |
| FR-04 | `CompiledGraph.Run(ctx, initialState)` executes nodes following edges until END is reached; `ctx` carries cancellation/interrupt. |
| FR-05 | At each step: call the current `NodeFunc`, write checkpoint (PRD-110), upsert the `workflow_steps` row, determine next node via edge. |
| FR-06 | Cycle detection: track visit count per node per run; if a node is visited more than `maxIterations` times, return an error wrapping `ErrCycle`. |
| FR-07 | Subgraph node: `AddNode("sub", compiledSubgraph)` — when executed (a `*CompiledGraph` implements `NodeFunc` semantics), runs the subgraph with the current state and merges its final state. |
| FR-08 | `tag workflow graph describe` resolves the registered workflow, calls `graph.Describe()`, and renders a text-art adjacency list or Mermaid diagram. |
| FR-09 | `workflow_steps` table records: session_id, step_num, node_name, input_state_hash, output_state_hash, duration_ms, status. |
| FR-10 | `END` node: when an edge or `routerFn` maps to `END`, execution terminates and `finalState` is returned. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Graph compilation must detect cycles that don't include END (infinite loops with no escape) via Kahn's algorithm and return a warning, but allow intentional retry cycles that have a conditional edge to END. |
| NFR-02 | Node functions are called on the scheduler goroutine; no implicit concurrency. Fan-out is explicit via PRD-111 `Send`. |
| NFR-03 | The state map is passed by shallow copy to each node to prevent accidental cross-node mutation. |
| NFR-04 | The graph topology (nodes + edges + node names) is serializable to JSON (`encoding/json`) for storage and version control; node function pointers are referenced by registered name, not serialized. |

---

## 9. Technical Design

### 9.1 SQLite DDL

Created by an `internal/store` migration against the single pure-Go `modernc.org/sqlite` connection (WAL, `CGO_ENABLED=0`); all writes go through the single-writer + `flock` atomic RMW contract, and step transitions are event-sourced for time-travel replay (PRD-113).

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

### 9.2 Go core (`internal/queue`)

Edges are modeled as a small interface (`staticEdge` | `conditionalEdge`) rather than Python's tagged-tuple union; `NodeFunc` and `*CompiledGraph` both satisfy the node contract, giving subgraph support without an `isinstance` check. Cycle detection combines Kahn validation at compile time (NFR-01) with a per-run visit counter (FR-06). Cancellation flows through `context.Context` (interrupt/timeout, PRD-109).

```go
package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const (
	START = "__start__"
	END   = "__end__"
)

var ErrCycle = errors.New("cycle")

type State map[string]any

// NodeFunc is a graph node. A *CompiledGraph also implements this via RunNode,
// which is how subgraphs are embedded.
type NodeFunc func(State) (State, error)

// edge is either a static target or a conditional router+mapping.
type edge struct {
	static      string              // "" if conditional
	router      func(State) string  // nil if static
	mapping     map[string]string
}

type WorkflowGraph struct {
	nodes map[string]NodeFunc
	subs  map[string]*CompiledGraph
	edges map[string]edge
	order []string // insertion order for stable Describe output
}

func NewWorkflowGraph() *WorkflowGraph {
	return &WorkflowGraph{
		nodes: map[string]NodeFunc{},
		subs:  map[string]*CompiledGraph{},
		edges: map[string]edge{},
	}
}

func (g *WorkflowGraph) AddNode(name string, fn NodeFunc) { g.nodes[name] = fn; g.order = append(g.order, name) }
func (g *WorkflowGraph) AddSubgraph(name string, sub *CompiledGraph) { g.subs[name] = sub; g.order = append(g.order, name) }
func (g *WorkflowGraph) AddEdge(from, to string)          { g.edges[from] = edge{static: to} }
func (g *WorkflowGraph) AddConditionalEdges(from string, router func(State) string, mapping map[string]string) {
	g.edges[from] = edge{router: router, mapping: mapping}
}

type CompileOption func(*CompiledGraph)

func WithCheckpointer(c Checkpointer) CompileOption { return func(cg *CompiledGraph) { cg.checkpointer = c } }
func WithMaxIterations(n int) CompileOption         { return func(cg *CompiledGraph) { cg.maxIterations = n } }

// Compile validates the graph (Kahn acyclic-with-escape, START/END reachable,
// no orphans/dupes) and returns a runnable *CompiledGraph.
func (g *WorkflowGraph) Compile(opts ...CompileOption) (*CompiledGraph, error) {
	cg := &CompiledGraph{nodes: g.nodes, subs: g.subs, edges: g.edges, order: g.order, maxIterations: 50}
	for _, o := range opts {
		o(cg)
	}
	if err := cg.validate(); err != nil { // Kahn cycle-detection + reachability
		return nil, err
	}
	return cg, nil
}

func (g *WorkflowGraph) Describe() string {
	var b strings.Builder
	b.WriteString("WorkflowGraph:\n")
	for _, node := range g.order {
		e, ok := g.edges[node]
		switch {
		case !ok:
			fmt.Fprintf(&b, "  %s → (no edge)\n", node)
		case e.router == nil:
			fmt.Fprintf(&b, "  %s → %s\n", node, e.static)
		default:
			targets := make([]string, 0, len(e.mapping))
			for _, t := range e.mapping {
				targets = append(targets, t)
			}
			fmt.Fprintf(&b, "  %s → conditional(%v)\n", node, targets)
		}
	}
	return b.String()
}

type RunOption func(*runCfg)
type runCfg struct{ sessionID string }

func WithSessionID(id string) RunOption { return func(c *runCfg) { c.sessionID = id } }

type CompiledGraph struct {
	nodes         map[string]NodeFunc
	subs          map[string]*CompiledGraph
	edges         map[string]edge
	order         []string
	checkpointer  Checkpointer
	maxIterations int
}

func (cg *CompiledGraph) Run(ctx context.Context, initial State, opts ...RunOption) (State, error) {
	cfg := runCfg{}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.sessionID == "" {
		cfg.sessionID = uuid.NewString()[:8]
	}
	state := shallowCopy(initial)
	current := START
	visits := map[string]int{}
	step := 0

	if cg.checkpointer != nil { // resume from latest checkpoint
		if s, n, ok := cg.checkpointer.LoadLatest(ctx, cfg.sessionID); ok {
			state, step = s, n+1
		}
	}

	for current != END {
		if err := ctx.Err(); err != nil { // interrupt / timeout (PRD-109)
			return state, err
		}
		if current != START && current != END {
			visits[current]++
			if visits[current] > cg.maxIterations {
				return state, fmt.Errorf("%w: node %q visited %d times", ErrCycle, current, visits[current])
			}
			var err error
			if sub, ok := cg.subs[current]; ok { // subgraph node
				state, err = sub.Run(ctx, state, WithSessionID(cfg.sessionID+"_"+current))
			} else if fn, ok := cg.nodes[current]; ok {
				var out State
				out, err = fn(shallowCopy(state))
				if out != nil {
					state = out
				}
			}
			if err != nil {
				return state, err
			}
			if cg.checkpointer != nil {
				if err := cg.checkpointer.Save(ctx, cfg.sessionID, step, state); err != nil {
					return state, err
				}
			}
			step++
		}
		e, ok := cg.edges[current]
		if !ok {
			break
		}
		if e.router == nil {
			current = e.static
			continue
		}
		key := e.router(state)
		if next, ok := e.mapping[key]; ok {
			current = next
		} else {
			current = key // allow router to return a node name directly (incl. END)
		}
	}
	return state, nil
}
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Arbitrary code execution via workflow definitions | Workflows are compiled-in Go code registered by name (no runtime `.py`/`.go` loading); `tag workflow graph run` executes trusted, in-binary node functions. Untrusted node work is delegated to `internal/sandbox` where configured. |
| Infinite loop consuming resources | `maxIterations` hard cap per node per run; a wrapped `ErrCycle` is returned; `context` deadline bounds total wall-clock. |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Table-driven compile/run of a 5-node linear graph; conditional edge routing; cycle detection at `maxIterations` (assert `errors.Is(err, ErrCycle)`) |
| Integration | Workflow with interrupt (`context` cancel), fan-out (PRD-111), and checkpoint resume against a temp `modernc.org/sqlite` DB |
| Correctness | State map passed by copy — `go test -race` proves node mutations don't bleed across steps |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `WorkflowGraph` with START → A → B → END compiles and runs correctly |
| AC-02 | Conditional edge routes to correct next node based on router function |
| AC-03 | Cyclic graph with `maxIterations=3` returns an error wrapping `ErrCycle` after 3 visits |
| AC-04 | `tag workflow graph describe <workflow>` prints a text representation |
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
| OQ-02 | Should there be a YAML/JSON workflow format for non-Go users (avoiding a recompile to define a workflow)? |

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

