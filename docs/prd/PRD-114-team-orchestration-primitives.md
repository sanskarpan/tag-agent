# PRD-114: Five Team Orchestration Primitives (`tag team`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** L (8-13 days)
**Category:** Workflow State
**Affects:** `internal/swarm (process registry + orchestrator) + internal/cli (team command group)`
**Depends on:** PRD-082 (multi-agent team primitives), PRD-108 (MagenticOne orchestrator), PRD-112 (graph-based workflow), PRD-109 (HITL interrupt)
**Inspired by:** CrewAI Process types, AutoGen GroupChat, LangGraph multi-agent, Microsoft Autogen 0.4 team primitives

---

## 1. Overview

TAG's PRD-082 introduced basic multi-agent team coordination (agent registration, task dispatch, result aggregation). However, real-world multi-agent applications require higher-level orchestration patterns: a supervisor who routes tasks, a hierarchical team with sub-team delegation, a sequential pipeline, a round-robin critic debate, and a parallel specialist swarm. Each pattern has different fault tolerance, latency, and communication properties.

Five Team Orchestration Primitives (`tag team`) introduces five first-class orchestration modes, each implemented as a configurable process type in the TAG team system:

1. **Sequential** — agents execute in a defined order, passing state forward (CrewAI `Process.sequential`)
2. **Hierarchical** — a manager agent decomposes tasks and delegates to worker agents (CrewAI `Process.hierarchical`)
3. **Supervisor** — an LLM supervisor decides which agent to call next (LangGraph multi-agent supervisor)
4. **Debate** — agents take turns producing outputs that others critique until consensus (multi-agent debate)
5. **Swarm** — all agents work in parallel on decomposed subtasks, results merged at the end (AutoGen Swarm)

Each mode is selectable via `tag team run --process TYPE` and persists execution state to SQLite, enabling checkpoint/resume (PRD-110) and interrupt/resume (PRD-109).

---

## 2. Problem Statement

### 2.1 One-size-fits-all dispatch is insufficient

PRD-082's flat team dispatch works for simple "assign to best-fit agent" scenarios but provides no support for supervisor-directed routing, sequential pipelines with handoff, or consensus-building through debate.

### 2.2 No hierarchical delegation

Complex tasks require a manager agent that decomposes the goal, delegates subtasks to specialists, and synthesizes results. Without a hierarchical process primitive, engineers must implement this ad-hoc.

### 2.3 Quality improvement through debate is manual

Multi-agent debate (multiple agents critiquing each other's outputs to converge on a better answer) is a proven technique (PRD-102 multi-agent debate) but requires a structured turn-taking orchestration that PRD-082 does not provide.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Sequential process: execute agents in a fixed order; each receives the accumulated state from all prior agents. |
| G2 | Hierarchical process: a designated manager agent receives the goal, produces a task list, and dispatches each task to a worker agent. |
| G3 | Supervisor process: an LLM supervisor decides at each turn which agent to invoke next based on current state; terminates when supervisor returns FINISH. |
| G4 | Debate process: agents take turns; each round, agents receive all prior outputs and produce a critique/update; terminates when all agents agree or max rounds reached. |
| G5 | Swarm process: decompose goal into N subtasks, assign one per agent in parallel (PRD-111 fan-out), reduce results. |
| G6 | All processes persist their state to SQLite and support checkpoint/resume (PRD-110). |
| G7 | `tag team run --process TYPE --team TEAM --goal GOAL` launches the selected process. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Implementing the underlying debate/supervisor LLM logic (that's PRD-108). |
| NG2 | Custom process types via plugins. |
| NG3 | Multi-machine process execution. |
| NG4 | Real-time streaming of intermediate agent outputs to clients. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Sequential 5-agent pipeline completion | Executes in < 2× the sum of individual agent times | Benchmark test |
| Supervisor routing accuracy | Supervisor routes to the correct agent 90%+ of the time on 10-task benchmark | Eval test |
| Debate convergence | Debate converges to consensus in ≤ 5 rounds for 80% of test prompts | Eval test |
| Swarm speedup | 5-agent swarm completes in < 2× single agent time | Benchmark test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Run a sequential pipeline where agent A's output feeds agent B | I express ordered agent pipelines naturally |
| US2 | Developer | Use a supervisor to route tasks to specialists | I build adaptive multi-agent systems |
| US3 | ML engineer | Run a debate between 3 agents to improve answer quality | I use collaborative reasoning without manual orchestration |
| US4 | Developer | Use a swarm to process 5 documents in parallel with 5 agents | I maximize throughput on embarrassingly parallel tasks |
| US5 | Developer | Have a manager agent decompose my goal automatically | I delegate task decomposition to an LLM |

---

## 6. CLI Surface

```
tag team run \
  --process sequential|hierarchical|supervisor|debate|swarm \
  --team TEAM_NAME \
  --goal "Analyze and summarize the following documents..." \
  [--max-rounds N] \
  [--model MODEL] \
  [--verbose]

tag team list [--process TYPE]
tag team show <session-id>
tag team stop <session-id>

Process-specific options:
  sequential:
    --order agent1,agent2,agent3    Explicit agent execution order

  hierarchical:
    --manager agent-name            Designate manager agent (default: first in team)

  supervisor:
    --supervisor-model MODEL        LLM model for supervisor decisions

  debate:
    --rounds N                      Max debate rounds (default: 5)
    --consensus-threshold FLOAT     Agreement score to stop early (default: 0.9)

  swarm:
    --max-workers N                 Parallel workers (default: team size)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag team run --process sequential`: execute agents in `--order` sequence; accumulate state; return final accumulated state. |
| FR-02 | `tag team run --process hierarchical`: call manager agent with goal; parse JSON task list from response; dispatch each task to the designated worker agent; synthesize results. |
| FR-03 | `tag team run --process supervisor`: call supervisor model at each turn with current state + available agents; parse next agent selection; call that agent; repeat until supervisor returns "FINISH". |
| FR-04 | `tag team run --process debate`: each round, call all agents with accumulated debate history; compute pairwise agreement score; if agreement ≥ threshold, stop; else continue to next round. |
| FR-05 | `tag team run --process swarm`: decompose goal into N subtasks (one per agent); fan-out (PRD-111); reduce results to final answer. |
| FR-06 | All processes persist process type, round/step progress, and agent outputs to `team_process_sessions` SQLite table. |
| FR-07 | All processes support `--max-rounds` to bound execution; on limit exceeded, return best result so far. |
| FR-08 | `tag team show <session-id>` renders process type, current step, agent assignment, and last 5 outputs. |
| FR-09 | Supervisor process must support both "next agent" and "FINISH" outputs from the supervisor model; robust JSON parsing required. |
| FR-10 | Hierarchical process manager must produce task list as JSON array; if parsing fails, fall back to single-agent execution. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Supervisor model prompt stays under 4096 tokens via state summarization. |
| NFR-02 | Debate round timeout: if any agent fails to respond within 60s, skip that agent for this round. |
| NFR-03 | Sequential process must detect cycles (agent A → agent B → agent A) via visit tracking. |
| NFR-04 | Swarm fan-out reuses the PRD-111 `errgroup`-bounded worker pool directly (`golang.org/x/sync/errgroup` + semaphore-bounded goroutines); not re-implemented. |

---

## 9. Technical Design

### 9.1 SQLite DDL

DB-neutral DDL; targets `modernc.org/sqlite` (pure-Go, CGO_ENABLED=0) in the single `tag.sqlite3` store owned by `internal/store`. All writes go through the single-writer + `gofrs/flock` atomic read-modify-write path.

```sql
CREATE TABLE IF NOT EXISTS team_process_sessions (
  id              TEXT PRIMARY KEY,
  team_name       TEXT NOT NULL,
  process_type    TEXT NOT NULL,
  goal            TEXT NOT NULL,
  profile         TEXT,
  status          TEXT NOT NULL DEFAULT 'running',
  current_round   INTEGER NOT NULL DEFAULT 0,
  max_rounds      INTEGER NOT NULL DEFAULT 10,
  final_result    TEXT,
  created_at      TEXT NOT NULL,
  completed_at    TEXT
);

CREATE TABLE IF NOT EXISTS team_process_steps (
  id              TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL REFERENCES team_process_sessions(id),
  round_num       INTEGER NOT NULL,
  agent_name      TEXT NOT NULL,
  input_summary   TEXT,
  output_summary  TEXT,
  created_at      TEXT NOT NULL
);
```

### 9.2 Process interface + registry (`internal/swarm`)

The five process types become a `Process` interface with one concrete implementation per mode, wired through a `map[string]Process` registry — no reflection / getattr-style method dispatch. New process types (a non-goal, see §3.1 NG2) would require a new Go type in the registry at compile time.

```go
package swarm

import (
	"context"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

// Agent is a team member resolved from the PRD-082 registry.
type Agent struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Model string `json:"model,omitempty"`
}

// State is the accumulated process state, persisted between rounds.
type State struct {
	Goal    string        `json:"goal"`
	Outputs []AgentOutput `json:"outputs"`
	Final   string        `json:"final,omitempty"`
}

type AgentOutput struct {
	Agent  string `json:"agent"`
	Round  int    `json:"round"`
	Result string `json:"result"`
}

// Options carries process-specific flags (order, manager, max rounds, workers…).
type Options struct {
	Order      []string
	Manager    string
	MaxRounds  int
	MaxWorkers int
	Model      string
}

// Result is returned to the CLI/persistence layer.
type Result struct {
	State  State
	Rounds int
}

// Process is the single orchestration primitive contract. Every mode
// (sequential/hierarchical/supervisor/debate/swarm) implements it.
type Process interface {
	Run(ctx context.Context, goal string, opts Options) (Result, error)
}

// registry maps the --process flag to a constructor; no reflection.
type Factory func(agents []Agent, prov llm.Provider) Process

var registry = map[string]Factory{
	"sequential":   func(a []Agent, p llm.Provider) Process { return &Sequential{agents: a, prov: p} },
	"hierarchical": func(a []Agent, p llm.Provider) Process { return &Hierarchical{agents: a, prov: p} },
	"supervisor":   func(a []Agent, p llm.Provider) Process { return &Supervisor{agents: a, prov: p} },
	"debate":       func(a []Agent, p llm.Provider) Process { return &Debate{agents: a, prov: p} },
	"swarm":        func(a []Agent, p llm.Provider) Process { return &Swarm{agents: a, prov: p} },
}

// New resolves a process type, or an error for an unknown --process value.
func New(process string, agents []Agent, prov llm.Provider) (Process, error) {
	f, ok := registry[process]
	if !ok {
		return nil, fmt.Errorf("unknown process type %q", process)
	}
	return f(agents, prov), nil
}
```

Two representative implementations. Agents run via the shared `internal/agent` loop; the LLM supervisor calls the `internal/llm` provider (`Stream(ctx, Request) -> <-chan Event`, accumulated to a final message). LLM output is parsed with a tolerant `encoding/json` decode.

```go
// Sequential: run agents in a fixed order, threading accumulated state forward.
type Sequential struct {
	agents []Agent
	prov   llm.Provider
}

func (s *Sequential) Run(ctx context.Context, goal string, opts Options) (Result, error) {
	order := opts.Order
	if len(order) == 0 {
		for _, a := range s.agents {
			order = append(order, a.Name)
		}
	}
	byName := indexByName(s.agents)
	seen := map[string]bool{} // NFR-03 cycle detection
	state := State{Goal: goal}
	for i, name := range order {
		if seen[name] {
			return Result{}, fmt.Errorf("sequential cycle detected at %q", name)
		}
		seen[name] = true
		a, ok := byName[name]
		if !ok {
			return Result{}, fmt.Errorf("unknown agent %q", name)
		}
		out, err := agent.Run(ctx, a.Model, a.Role, renderState(state))
		if err != nil {
			return Result{}, err
		}
		state.Outputs = append(state.Outputs, AgentOutput{Agent: name, Round: i, Result: out})
	}
	return Result{State: state, Rounds: len(order)}, nil
}

// Supervisor: an LLM decides the next agent each turn; stops on FINISH.
type Supervisor struct {
	agents []Agent
	prov   llm.Provider
}

func (s *Supervisor) Run(ctx context.Context, goal string, opts Options) (Result, error) {
	maxRounds := opts.MaxRounds
	if maxRounds == 0 {
		maxRounds = 20
	}
	byName := indexByName(s.agents)
	state := State{Goal: goal}
	for round := 0; round < maxRounds; round++ {
		msg, err := s.prov.Complete(ctx, supervisorRequest(opts.Model, s.agents, state))
		if err != nil {
			return Result{}, err
		}
		var decision struct {
			Action string `json:"action"`
			Agent  string `json:"agent"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal([]byte(msg.Text), &decision); err != nil {
			// FR-09: robust parse; treat unparseable supervisor output as a stop.
			break
		}
		if decision.Action == "FINISH" {
			state.Final = decision.Result
			break
		}
		a, ok := byName[decision.Agent]
		if !ok {
			break
		}
		out, err := agent.Run(ctx, a.Model, a.Role, renderState(state))
		if err != nil {
			return Result{}, err
		}
		state.Outputs = append(state.Outputs, AgentOutput{Agent: decision.Agent, Round: round, Result: out})
	}
	return Result{State: state, Rounds: len(state.Outputs)}, nil
}
```

The `swarm` process does not implement its own fan-out: it decomposes the goal into N subtasks and dispatches them through the PRD-111 `errgroup`-bounded worker pool (`golang.org/x/sync/errgroup` + a semaphore channel sized to `opts.MaxWorkers`), then reduces the per-agent `AgentOutput` slice into the final result (NFR-04).

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Supervisor model prompt injection via agent outputs | Sanitize agent outputs in supervisor prompt; truncate to 1000 chars |
| Hierarchical manager infinite delegation | Max task list size = team size; reject larger lists |

---

## 11. Testing Strategy

Go `testing` (table-driven cases per process type; a fake `llm.Provider` returns scripted events for deterministic supervisor/debate runs). Latency/throughput metrics from §4 measured with `testing.B` benchmarks. Integration tests run against an in-memory `modernc.org/sqlite` store.

| Layer | Tests |
|-------|-------|
| Unit | Table-driven: sequential order compliance + cycle detection; supervisor FINISH detection; debate agreement scoring; registry lookup rejects unknown `--process` |
| Integration | 3-agent sequential pipeline end-to-end; 2-agent debate convergence (against in-memory store) |
| Benchmark | `testing.B` for the sequential-pipeline and swarm-speedup targets in §4 |
| Resilience | Agent `error` mid-process triggers graceful degradation (errgroup context cancellation for swarm) |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag team run --process sequential --order a,b,c` executes agents in a→b→c order |
| AC-02 | `--process supervisor` stops on FINISH response from supervisor model |
| AC-03 | `--process debate --rounds 3` stops after 3 rounds or on consensus |
| AC-04 | `--process swarm` runs agents in parallel via PRD-111 fan-out |
| AC-05 | All processes persist steps to SQLite |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-082 multi-agent team primitives | Agent registry |
| PRD-111 dynamic fan-out | Swarm parallel execution |
| PRD-110 state serialization | Process checkpoint |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should process types be extensible (plugin architecture)? **Reconsidered by the Go move:** the static single-binary model has no dynamic plugin loading (GO_MIGRATION_PLAN decision #6 rejects `go-plugin`/gRPC and scripting VMs for v1). A new process type is a new Go type registered in the `map[string]Factory` at compile time. Runtime extensibility, if ever needed, is limited to the shell/HTTP hook surface + MCP servers — not in-process custom `Process` implementations. This reinforces NG2 ("custom process types via plugins" is a non-goal). |
| OQ-02 | Should the debate process use an LLM judge to score agreement, or embedding cosine similarity (in-Go cosine over provider/`internal/memory` embeddings, per decision #2)? |

---

## 15. Complexity & Timeline

**Complexity:** Large (L)
**Estimated effort:** 8–13 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | Sequential + swarm processes, SQLite DDL | 3 |
| 2 | Hierarchical + supervisor processes | 3 |
| 3 | Debate process, agreement scoring | 2 |
| 4 | CLI integration, checkpoint wiring, tests | 3 |

