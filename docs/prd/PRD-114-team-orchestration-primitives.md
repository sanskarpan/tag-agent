# PRD-114: Five Team Orchestration Primitives (`tag team`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** L (8-13 days)
**Category:** Workflow State
**Affects:** `team_orchestration.py + controller.py`
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
| NFR-04 | Swarm fan-out uses PRD-111 `fan_out_execute` directly; not re-implemented. |

---

## 9. Technical Design

### 9.1 SQLite DDL

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

### 9.2 Process dispatcher

```python
from __future__ import annotations
from typing import List

PROCESS_TYPES = {
    "sequential": "_run_sequential",
    "hierarchical": "_run_hierarchical",
    "supervisor": "_run_supervisor",
    "debate": "_run_debate",
    "swarm": "_run_swarm",
}

class TeamOrchestrator:
    def __init__(self, team_agents: List[dict], model: str) -> None:
        self.agents = team_agents
        self.model = model

    def run(self, process: str, goal: str, **kwargs) -> dict:
        method = getattr(self, PROCESS_TYPES[process])
        return method(goal, **kwargs)

    def _run_sequential(self, goal: str, order: List[str] = None, **kw) -> dict:
        agents = [a for a in self.agents if a["name"] in (order or [a["name"] for a in self.agents])]
        state: dict = {"goal": goal, "outputs": []}
        for agent in agents:
            result = _call_agent(agent, state)
            state["outputs"].append({"agent": agent["name"], "result": result})
            state["last_output"] = result
        return state

    def _run_supervisor(self, goal: str, max_rounds: int = 20, **kw) -> dict:
        import json
        state: dict = {"goal": goal, "history": []}
        for _ in range(max_rounds):
            prompt = _supervisor_prompt(self.agents, state)
            response = _call_llm(self.model, prompt)
            try:
                parsed = json.loads(response)
            except Exception:
                break
            if parsed.get("action") == "FINISH":
                state["final"] = parsed.get("result", "")
                break
            agent_name = parsed.get("agent")
            agent = next((a for a in self.agents if a["name"] == agent_name), None)
            if not agent:
                break
            result = _call_agent(agent, state)
            state["history"].append({"agent": agent_name, "result": result})
        return state
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Supervisor model prompt injection via agent outputs | Sanitize agent outputs in supervisor prompt; truncate to 1000 chars |
| Hierarchical manager infinite delegation | Max task list size = team size; reject larger lists |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Sequential order compliance; supervisor FINISH detection; debate agreement scoring |
| Integration | 3-agent sequential pipeline end-to-end; 2-agent debate convergence |
| Resilience | Agent failure mid-process triggers graceful degradation |

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
| OQ-01 | Should process types be extensible (plugin architecture)? |
| OQ-02 | Should the debate process use an LLM judge to score agreement, or embedding cosine similarity? |

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
