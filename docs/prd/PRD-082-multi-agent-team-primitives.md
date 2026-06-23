# PRD-082: Multi-Agent Team Primitives: RoundRobin, Selector, Swarm Handoff (`tag team`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `teams.py`
**Depends on:** PRD-004 (kanban swarm helpers), PRD-008 (background task queue), PRD-013 (agent tracing/observability), PRD-021 (agent loop/autonomous mode), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-033 (dependency-aware task queue), PRD-034 (secret scanning), PRD-041 (OTel GenAI span cost attribution)
**Inspired by:** AutoGen AgentChat team types, CrewAI crews, MAF agent groups

---

## 1. Overview

TAG today runs one agent at a time: a single profile receives a goal, iterates through its loop, and terminates. Even the existing swarm helpers in `kanban.py` (PRD-004) are fundamentally a fan-out pattern — tasks are distributed to agents independently, not orchestrated through a shared conversation or a structured handoff protocol. There is no mechanism to compose named, reusable groups of agents with a declared coordination strategy, run a shared goal through the group, and observe the multi-agent interaction as a first-class entity.

Multi-agent team frameworks have converged on three foundational orchestration primitives: **RoundRobin** (agents take turns speaking in fixed rotation, excellent for iterative refinement and deliberation), **Selector** (an LLM or rule-based router chooses which agent speaks next, optimal for dynamic dispatch based on expertise), and **Swarm** (agents autonomously emit handoff signals that determine the next speaker, modeled after AutoGen's `HandoffMessage` pattern). These three primitives cover the overwhelming majority of real agentic collaboration patterns — feature teams, review cycles, research pipelines, and full-stack engineering crews.

This PRD introduces `tag team`: a first-class CLI namespace that lets users define named teams of TAG profiles with an explicit orchestration strategy, run any goal through a team, observe per-turn transcripts and per-agent cost/token attribution, and share a kanban board across team members for work-item tracking. The team definition is stored in SQLite (persistent, inspectable, portable) and the runtime runs as a coordinated set of `loop_agent.py`-style iterations stitched together by the orchestration strategy layer.

The design is directly inspired by AutoGen AgentChat's team types (`RoundRobinGroupChat`, `SelectorGroupChat`, `Swarm`), CrewAI's `Crew` primitive with `Process.sequential` / `Process.hierarchical` modes, and the Multi-Agent Framework (MAF) `AgentGroup` abstraction. All three frameworks share a common insight: the team definition (membership, strategy, termination condition) should be declarative and reusable, while the execution runtime should be observable and interruptible.

A shared kanban board, implemented on top of the existing `kanban.py` layer, provides a shared work-item ledger accessible by all agents in the team. Any agent can create, update, or close kanban tasks during its turn; the next agent in the rotation can read those tasks to understand what work has been done and what remains. This kanban integration bridges the gap between conversational coordination (messages) and work-item coordination (tasks), mirroring how real engineering teams work.

---

## 2. Problem Statement

### 2.1 No Reusable Multi-Agent Composition Primitive

TAG users who need more than one agent to collaborate on a goal today must either: (a) manually chain `tag run` commands in shell scripts, passing output files between invocations; or (b) use `tag queue` with dependency edges (PRD-033), which serializes work but provides no shared conversational context and no handoff protocol. Neither approach is reusable — the orchestration logic lives in the user's shell script, not in TAG configuration. Every new project requires rebuilding the same wiring from scratch.

CrewAI solves this by making the crew definition (`Crew(agents=[...], tasks=[...], process=Process.sequential)`) a first-class, saveable unit. AutoGen solves it by making the team definition (`RoundRobinGroupChat(participants=[...])`) the unit of reuse. TAG has no equivalent. A platform engineer who wants to run the same "research + writer + editor" team across dozens of projects must copy-paste shell scripts or build their own orchestration layer on top of TAG.

### 2.2 No Structured Agent-to-Agent Handoff

When agents need to hand work from one to another, the current TAG patterns offer two unsatisfying options: (a) write output to a file and have the next agent read it — opaque, brittle, no conversation threading; or (b) stuff the full prior output into the next agent's prompt — context-polluting, token-expensive, and lossy. Neither approach supports the richer handoff semantics used by AutoGen (where `HandoffMessage(target="reviewer")` is a structured signal emitted by the agent runtime) or the OpenAI Agents SDK (where `handoff(agent)` transfers the conversation object with full history intact).

Without structured handoff, downstream agents do not know why they are receiving work, what context was established by the upstream agent, or what specifically needs their attention. The result is lower-quality outputs and higher token costs as each agent re-derives context that the prior agent had already established.

### 2.3 No Shared State Across Concurrent Agents

TAG's existing kanban layer (`kanban.py`, PRD-004) tracks tasks per-board, but there is no protocol for multiple agents to share a board in a coordinated way during a single team run. Agents that run concurrently may create duplicate tasks, overwrite each other's updates, or fail to notice completed work items when composing their next turn. A shared, transactionally safe kanban board that all team members can read and write is the missing coordination primitive.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Users can define named teams via `tag team create` with a list of `role:profile` agent members and an orchestration strategy (`roundrobin`, `selector`, `swarm`). |
| G2 | `tag team run <name> --goal "<text>"` executes the team goal through the chosen orchestration strategy and streams per-turn output to the terminal. |
| G3 | The RoundRobin strategy rotates through agents in declaration order, repeating until a termination condition is met (max-turns or a termination phrase detected in any agent output). |
| G4 | The Selector strategy uses a designated LLM (defaulting to the orchestrator profile's model) to choose the next speaker at each turn, with a structured JSON `{"next_speaker": "<role>", "reason": "..."}` response. |
| G5 | The Swarm strategy enables agents to emit a `HANDOFF_TO:<role>` signal in their output; the runtime scans for this signal and routes the next turn to the named role. |
| G6 | All team turns are persisted to a `team_turns` SQLite table with full input/output, agent role, token counts, and cost attribution per turn. |
| G7 | Each team run provisions a dedicated kanban board (named `team-<team_name>-<run_id>`) that all agents in the team can read and update during their turns via injected tool context. |
| G8 | `tag team list --json` and `tag team show <name> --json` surface team definitions and recent run summaries in machine-readable form. |
| G9 | Team runs emit OTel spans (one root span per team run, one child span per turn) compatible with PRD-013 and PRD-041 cost attribution. |
| G10 | `tag team delete <name>` removes the team definition (but not historical run data). |

## 3.1 Non-Goals

| # | Non-Goal |
|----|----------|
| NG1 | Real-time parallel agent execution within a single team run. All strategies are sequential at the turn level (one agent speaks, then the next). True parallelism is addressed by PRD-008 queue workers. |
| NG2 | Cross-host or network-distributed team execution. All agents run on the local machine using local profiles. Network-distributed multi-agent protocols (A2A, ACP, ANP) are separate PRDs. |
| NG3 | A visual team design UI. Team definitions are created and edited via CLI only in this PRD. A web dashboard view (PRD-036) may extend this. |
| NG4 | Dynamic agent addition/removal during a running team. Team membership is fixed at team creation time. |
| NG5 | Integration with AutoGen, CrewAI, or other frameworks at the library level. This PRD implements equivalent semantics natively using TAG primitives — it does not wrap or embed external frameworks. |
| NG6 | Team-level budget enforcement beyond what individual agent runs already enforce via PRD-039 (token budget). |
| NG7 | Automatic agent capability negotiation or role assignment. Roles and profiles are explicitly declared by the user. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Team creation latency | `tag team create` completes in < 200 ms | Timed in CI integration test |
| RoundRobin correctness | For a 3-agent team, each agent is called exactly `ceil(max_turns/3)` times | Assertion in unit test against mock agent calls |
| Selector routing accuracy | LLM selector chooses the agent matching the expected expertise in ≥ 85% of test cases | Eval suite with known-answer routing scenarios |
| Swarm handoff detection | `HANDOFF_TO:` signals are parsed and routed correctly in 100% of unit test cases | Unit test with synthetic agent outputs |
| Kanban board provisioning | A new board is created and accessible in < 100 ms at team run start | Timed integration test |
| Turn persistence | All turns for a 10-agent, 20-turn run are stored in SQLite with no missing rows | Integration test with database assertion |
| OTel span emission | Each team run root span contains child spans for all turns, with `team.name`, `team.strategy`, and `agent.role` attributes | Span capture test |
| `--json` output validity | `tag team list --json` and `tag team show --json` produce valid JSON parseable by `json.loads()` | CI unit test |
| P99 team run overhead | Orchestration overhead (excluding agent LLM time) < 50 ms per turn for 10-member teams | Benchmark with mocked agent calls |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Platform engineer | define a `fullstack-crew` team once with orchestrator, coder, and reviewer roles | I can re-run the same team composition on any new feature goal without rebuilding the wiring each time |
| U2 | Developer | run `tag team run fullstack-crew --goal "Build a REST API for user authentication"` | I get a complete multi-agent implementation + review without manually chaining `tag run` commands |
| U3 | Team lead | use `selector` strategy with an orchestrator profile as router | The most relevant expert agent is called for each subtask rather than wasting turns on agents without the right expertise |
| U4 | Developer | use `swarm` strategy and have the coder agent emit `HANDOFF_TO:reviewer` when done | The review starts immediately after coding completes, with full context transferred automatically |
| U5 | Developer | inspect `tag team show fullstack-crew --json` | I can see exactly which profiles are bound to which roles and what strategy is configured before running |
| U6 | Platform engineer | see per-turn token counts and cost in `tag team run` output | I can identify which agent is consuming the most tokens and optimize the team composition |
| U7 | Developer | have all agents in the team share a kanban board | Agents can coordinate work items without passing large context blobs — the coder creates a task "API scaffolding done", the reviewer reads it and knows what to check |
| U8 | DevOps engineer | run `tag team run --max-turns 20 --termination-phrase "DONE"` | The team run terminates automatically when any agent declares completion rather than consuming all max-turns |
| U9 | Developer | see OTel traces for a team run in my OTLP backend | I can identify bottlenecks, observe per-agent latency, and attribute costs to specific agent roles in Grafana |
| U10 | Platform engineer | delete a team definition without losing historical run data | I can clean up obsolete team configurations while preserving audit history |
| U11 | Developer | `tag team list --json` to enumerate all defined teams in a script | I can programmatically iterate team definitions in CI pipelines or tooling |
| U12 | Developer | run a team with `--dry-run` | I can validate team composition and strategy config is correct before spending LLM API budget |

---

## 6. Proposed CLI Surface

### 6.1 `tag team create`

Define a new named team with agent members and orchestration strategy.

```
tag team create <name>
  --agent <role>:<profile>          # repeatable; at least 1 required
  --strategy <roundrobin|selector|swarm>   # default: roundrobin
  --selector-model <model>          # only for selector strategy; default: orchestrator profile model
  --max-turns <N>                   # default: 10
  --termination-phrase <text>       # phrase that halts the run if found in any agent output
  --description <text>              # optional human description
  [--json]                          # output created team as JSON
```

Example:

```bash
tag team create "fullstack-crew" \
  --agent orchestrator:orchestrator \
  --agent coder:coder \
  --agent reviewer:reviewer \
  --strategy roundrobin \
  --max-turns 15 \
  --termination-phrase "IMPLEMENTATION COMPLETE"

# Output:
Team 'fullstack-crew' created.
Strategy : roundrobin
Agents   : orchestrator (profile: orchestrator)
           coder (profile: coder)
           reviewer (profile: reviewer)
Max turns: 15
Board    : (provisioned at run time)
```

With `--json`:

```json
{
  "id": "team-9a3f1c2d",
  "name": "fullstack-crew",
  "strategy": "roundrobin",
  "agents": [
    {"role": "orchestrator", "profile": "orchestrator"},
    {"role": "coder",        "profile": "coder"},
    {"role": "reviewer",     "profile": "reviewer"}
  ],
  "max_turns": 15,
  "termination_phrase": "IMPLEMENTATION COMPLETE",
  "selector_model": null,
  "description": null,
  "created_at": "2026-06-17T10:00:00Z"
}
```

### 6.2 `tag team run`

Execute a goal through a named team.

```
tag team run <name>
  --goal <text>                     # goal / task description (required)
  [--max-turns <N>]                 # override team default
  [--termination-phrase <text>]     # override team default
  [--no-kanban]                     # skip shared kanban board provisioning
  [--output <file.json>]            # write full run transcript to file
  [--dry-run]                       # validate config, print plan, exit 0
  [--json]                          # stream turn events as NDJSON
  [--budget-usd <float>]            # abort if cumulative cost exceeds limit
```

Example output (non-JSON, streaming):

```
[team:fullstack-crew] run-7b9e2a1f started  strategy=roundrobin  board=team-fullstack-crew-7b9e2a1f
[team:fullstack-crew] goal: "Build a REST API for user authentication"

── Turn 1 / orchestrator ───────────────────────────────────────────────────
I will break this into three subtasks: (1) schema design, (2) endpoint
implementation, (3) review and hardening. Creating kanban tasks now.
[kanban] Created task kb-001: "Design user schema"
[kanban] Created task kb-002: "Implement /auth/register and /auth/login"
[kanban] Created task kb-003: "Security review"
tokens=312  cost=$0.0009

── Turn 2 / coder ──────────────────────────────────────────────────────────
Reading kanban board…  kb-001 pending, kb-002 pending, kb-003 pending
Implementing schema and endpoints…
[kanban] Closed task kb-001
[kanban] Closed task kb-002
tokens=1847  cost=$0.0055

── Turn 3 / reviewer ───────────────────────────────────────────────────────
Reviewing implementation…
Found: no rate limiting on /auth/login. Recommended: add slowapi middleware.
Found: password hash uses MD5 — must use bcrypt or argon2.
[kanban] Created task kb-004: "Fix: rate limiting"
[kanban] Created task kb-005: "Fix: use bcrypt for password hashing"
tokens=923  cost=$0.0028

[team:fullstack-crew] Turn 3 complete. Rotating to orchestrator.

… (continues until max-turns or termination phrase) …

[team:fullstack-crew] run-7b9e2a1f completed  turns=9  total_tokens=8241  total_cost=$0.0247
```

With `--json` (NDJSON, one object per line):

```json
{"event":"run_start","run_id":"run-7b9e2a1f","team":"fullstack-crew","strategy":"roundrobin","goal":"Build a REST API for user authentication","timestamp":"2026-06-17T10:01:00Z"}
{"event":"turn_start","turn":1,"role":"orchestrator","profile":"orchestrator","timestamp":"2026-06-17T10:01:01Z"}
{"event":"turn_end","turn":1,"role":"orchestrator","output":"I will break this into...","tokens":312,"cost_usd":0.0009,"timestamp":"2026-06-17T10:01:04Z"}
{"event":"handoff","from_role":"orchestrator","to_role":"coder","reason":"roundrobin","timestamp":"2026-06-17T10:01:04Z"}
{"event":"run_end","run_id":"run-7b9e2a1f","turns":9,"total_tokens":8241,"total_cost_usd":0.0247,"status":"completed","timestamp":"2026-06-17T10:05:22Z"}
```

### 6.3 `tag team list`

```
tag team list [--json]
```

Example output:

```
NAME               STRATEGY     AGENTS  RUNS  LAST RUN
fullstack-crew     roundrobin   3       12    2026-06-17 09:45
research-team      selector     4       3     2026-06-16 14:22
review-swarm       swarm        2       7     2026-06-17 08:10
```

With `--json`:

```json
[
  {
    "id": "team-9a3f1c2d",
    "name": "fullstack-crew",
    "strategy": "roundrobin",
    "agent_count": 3,
    "run_count": 12,
    "last_run_at": "2026-06-17T09:45:00Z"
  }
]
```

### 6.4 `tag team show`

```
tag team show <name> [--json] [--runs <N>]    # default --runs 5
```

Non-JSON output shows team definition + recent run summary table. JSON output includes full agent list and recent run objects.

### 6.5 `tag team delete`

```
tag team delete <name> [--yes]
```

Prompts for confirmation unless `--yes`. Removes the team definition row; `team_turns` rows for historical runs are preserved (orphaned by design for audit).

### 6.6 `tag team run show`

```
tag team run show <run_id> [--json] [--turn <N>]
```

Show full transcript for a specific team run, optionally filtered to a single turn.

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag team create` validates that every `--agent role:profile` pair references a profile that exists in the TAG config; exits non-zero with actionable error if any profile is missing. | P0 |
| FR-02 | Team names must match `^[a-z0-9][a-z0-9\-_]{0,63}$`; the create command rejects names that do not match and prints the pattern. | P0 |
| FR-03 | At least one `--agent` is required; create fails with a clear error if none are specified. | P0 |
| FR-04 | `tag team create` is idempotent when called with `--upsert`; without `--upsert`, a duplicate name is an error. | P1 |
| FR-05 | RoundRobin strategy rotates agents in declaration order, wrapping around after the last agent. Turn N goes to agent at index `(N-1) % len(agents)`. | P0 |
| FR-06 | Selector strategy calls the selector model at the start of each turn with a structured prompt containing the goal, turn history summary, and agent roster; the response must parse as `{"next_speaker": "<role>", "reason": "<text>"}`. Retries up to 3 times on parse failure before falling back to roundrobin rotation. | P0 |
| FR-07 | Swarm strategy scans each agent output for the pattern `HANDOFF_TO:<role>` (case-insensitive, optional whitespace around colon). If the named role does not exist in the team, the run logs a warning and falls back to roundrobin for that turn. | P0 |
| FR-08 | All strategies respect `--max-turns`; the run terminates after that many total turns across all agents, even if no termination phrase is detected. | P0 |
| FR-09 | When `--termination-phrase` is set, the runtime checks each agent output (case-insensitive substring match) after the turn completes; if found, the run status is set to `terminated` and no further turns are executed. | P0 |
| FR-10 | Every turn is written to the `team_turns` table before the next turn begins, ensuring crash-safe persistence. Each row includes: team name, run ID, turn number, agent role, agent profile, input context (truncated to 8 KB), output, token counts (prompt + completion), cost USD, and UTC timestamp. | P0 |
| FR-11 | Each team run provisions a kanban board named `team-<team_name>-<run_id>` at run start and injects the board ID into each agent's system prompt as `TEAM_KANBAN_BOARD=<board_id>`. The agent's context includes instructions to use kanban task creation/update tools to track work items. | P1 |
| FR-12 | `--no-kanban` disables board provisioning and kanban context injection entirely. | P1 |
| FR-13 | `tag team run --dry-run` prints the full execution plan (agent roster, strategy, turn order for roundrobin, selector prompt template for selector, handoff scan pattern for swarm) and exits 0 without making any LLM calls. | P1 |
| FR-14 | `tag team list` returns all defined teams sorted by `created_at DESC`. `--json` output must be valid JSON (`json.loads()`-parseable). | P1 |
| FR-15 | `tag team show <name>` returns team definition plus a summary of the last N runs (default 5). `--runs 0` returns definition only. | P1 |
| FR-16 | `tag team delete` requires explicit confirmation (y/N prompt) unless `--yes` is passed. The `teams` table row is deleted; `team_runs` and `team_turns` rows are retained. | P1 |
| FR-17 | Each team run emits one root OTel span (`tag.team.run`) and one child span per turn (`tag.team.turn`) with attributes: `team.name`, `team.strategy`, `team.run_id`, `agent.role`, `agent.profile`, `turn.number`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.cost_usd`. | P1 |
| FR-18 | When `--budget-usd` is set, the runtime accumulates per-turn cost and aborts the run (status=`budget_exceeded`) if the cumulative cost exceeds the limit before the next turn starts. | P2 |
| FR-19 | The Selector strategy prompt template is stored in the `teams` table as `selector_prompt_template` (nullable TEXT); if null, a built-in default template is used. | P2 |
| FR-20 | `tag team run show <run_id>` streams the turn-by-turn transcript from `team_turns` in order; `--turn <N>` filters to a single turn. `--json` outputs each turn as a JSON object. | P2 |
| FR-21 | The `agent:role` input/output context passed to each agent in a team includes a `CONVERSATION_HISTORY` section containing the last K turns (default K=5, configurable via `--context-turns`), formatted as a conversation thread. | P1 |
| FR-22 | Each agent in a team run uses its bound profile's configured model, tools, system prompt, and budget settings. Team orchestration does not override individual profile configurations. | P0 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Orchestration overhead per turn (excluding LLM call time) must be < 50 ms P99 for teams of up to 20 members | Benchmark with mock agents |
| NFR-02 | SQLite writes use WAL mode with `PRAGMA busy_timeout = 5000`; concurrent reads from `tag team list` while a run is in progress must not block or corrupt turn writes | Integration test with concurrent reader |
| NFR-03 | Team definitions and run data survive process crashes; the last completed turn is always persisted before the next turn begins | Chaos test: kill process mid-turn, verify DB state |
| NFR-04 | `teams.py` must not import any framework-level dependencies (AutoGen, CrewAI, LangChain) at module level; all orchestration logic is native TAG Python | `import tag.teams` assertion in CI |
| NFR-05 | Selector strategy LLM call uses a lightweight, cost-efficient model by default (configurable via `--selector-model`); the default must not use the most expensive model tier | Documented default; unit test verifies default selection |
| NFR-06 | `--json` output to stdout must not interleave with log output; logs go to stderr | Output capture assertion in integration test |
| NFR-07 | Team names and role names are sanitized before use in SQL queries and kanban board names; no SQL injection vectors | Security unit test with adversarial name inputs |
| NFR-08 | The `team_turns` table `input_context` column stores at most 8 KB per row (truncated with a `[TRUNCATED]` marker); full context is never stored to prevent unbounded DB growth | Unit test: input > 8 KB is truncated |
| NFR-09 | `tag team delete` requires user confirmation to prevent accidental team loss; the `--yes` flag bypasses this for scripted use only | Integration test: delete without --yes prompts |
| NFR-10 | All new code in `teams.py` must have ≥ 85% line coverage in the test suite | Coverage report in CI |

---

## 9. Technical Design

### 9.1 New File: `src/tag/teams.py`

The entire team primitive implementation lives in a single new module, consistent with the existing `kanban.py`, `dag.py`, `loop_agent.py` pattern.

### 9.2 SQLite DDL

```sql
-- Team definitions
CREATE TABLE IF NOT EXISTS teams (
    id                      TEXT PRIMARY KEY,          -- "team-<8hex>"
    name                    TEXT NOT NULL UNIQUE,      -- user-facing slug
    strategy                TEXT NOT NULL              -- 'roundrobin' | 'selector' | 'swarm'
                             CHECK (strategy IN ('roundrobin', 'selector', 'swarm')),
    agents_json             TEXT NOT NULL,             -- JSON array of {role, profile}
    max_turns               INTEGER NOT NULL DEFAULT 10,
    termination_phrase      TEXT,                      -- nullable
    selector_model          TEXT,                      -- nullable; only for selector strategy
    selector_prompt_template TEXT,                     -- nullable; custom selector prompt
    context_turns           INTEGER NOT NULL DEFAULT 5, -- conversation history window
    description             TEXT,                      -- optional human label
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_teams_name ON teams(name);

-- Team run instances
CREATE TABLE IF NOT EXISTS team_runs (
    id                TEXT PRIMARY KEY,               -- "run-<12hex>"
    team_id           TEXT NOT NULL REFERENCES teams(id),
    team_name         TEXT NOT NULL,                  -- denormalized for fast display
    strategy          TEXT NOT NULL,
    goal              TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'running'
                       CHECK (status IN ('running', 'completed', 'failed',
                                         'terminated', 'budget_exceeded')),
    board_id          TEXT,                            -- kanban board slug; null if --no-kanban
    total_turns       INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    total_cost_usd    REAL    NOT NULL DEFAULT 0.0,
    otel_trace_id     TEXT,
    started_at        TEXT NOT NULL,
    completed_at      TEXT,
    error             TEXT                             -- nullable; populated on failure
);

CREATE INDEX IF NOT EXISTS idx_team_runs_team ON team_runs(team_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_team_runs_status ON team_runs(status, started_at DESC);

-- Per-turn transcript
CREATE TABLE IF NOT EXISTS team_turns (
    id                TEXT PRIMARY KEY,               -- "turn-<12hex>"
    run_id            TEXT NOT NULL REFERENCES team_runs(id),
    team_name         TEXT NOT NULL,                  -- denormalized
    turn_number       INTEGER NOT NULL,               -- 1-indexed
    agent_role        TEXT NOT NULL,
    agent_profile     TEXT NOT NULL,
    input_context     TEXT NOT NULL,                  -- truncated to 8192 bytes
    output            TEXT NOT NULL DEFAULT '',
    handoff_target    TEXT,                           -- nullable; for swarm strategy
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd          REAL    NOT NULL DEFAULT 0.0,
    otel_span_id      TEXT,
    started_at        TEXT NOT NULL,
    completed_at      TEXT,
    error             TEXT
);

CREATE INDEX IF NOT EXISTS idx_team_turns_run ON team_turns(run_id, turn_number);
```

### 9.3 Core Dataclasses

```python
from __future__ import annotations

import json
import secrets
from dataclasses import dataclass, field, asdict
from enum import Enum
from typing import Any


class Strategy(str, Enum):
    ROUNDROBIN = "roundrobin"
    SELECTOR   = "selector"
    SWARM      = "swarm"


class RunStatus(str, Enum):
    RUNNING         = "running"
    COMPLETED       = "completed"
    FAILED          = "failed"
    TERMINATED      = "terminated"
    BUDGET_EXCEEDED = "budget_exceeded"


@dataclass
class TeamAgent:
    """A single member of a team: a named role bound to a TAG profile."""
    role: str     # e.g. "coder"
    profile: str  # e.g. "coder"  (TAG profile name)

    def to_dict(self) -> dict[str, str]:
        return {"role": self.role, "profile": self.profile}


@dataclass
class TeamDefinition:
    """Persistent team configuration stored in the `teams` table."""
    id: str
    name: str
    strategy: Strategy
    agents: list[TeamAgent]
    max_turns: int = 10
    termination_phrase: str | None = None
    selector_model: str | None = None
    selector_prompt_template: str | None = None
    context_turns: int = 5
    description: str | None = None
    created_at: str = ""
    updated_at: str = ""

    def agents_json(self) -> str:
        return json.dumps([a.to_dict() for a in self.agents])

    @classmethod
    def from_row(cls, row: dict[str, Any]) -> "TeamDefinition":
        agents = [TeamAgent(**a) for a in json.loads(row["agents_json"])]
        return cls(
            id=row["id"],
            name=row["name"],
            strategy=Strategy(row["strategy"]),
            agents=agents,
            max_turns=row["max_turns"],
            termination_phrase=row["termination_phrase"],
            selector_model=row["selector_model"],
            selector_prompt_template=row["selector_prompt_template"],
            context_turns=row["context_turns"],
            description=row["description"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
        )


@dataclass
class TurnResult:
    """Result of a single agent turn."""
    role: str
    profile: str
    output: str
    prompt_tokens: int
    completion_tokens: int
    cost_usd: float
    handoff_target: str | None = None   # populated by swarm strategy
    error: str | None = None


@dataclass
class TeamRunContext:
    """Runtime state for an in-progress team run."""
    run_id: str
    team: TeamDefinition
    goal: str
    board_id: str | None
    budget_usd: float | None
    turn_history: list[TurnResult] = field(default_factory=list)
    total_cost_usd: float = 0.0
    current_turn: int = 0
    status: RunStatus = RunStatus.RUNNING
```

### 9.4 Orchestration Strategy Implementations

#### 9.4.1 RoundRobin

```python
import re

_HANDOFF_PATTERN = re.compile(
    r'HANDOFF_TO\s*:\s*(\w[\w\-]*)', re.IGNORECASE
)
_MAX_CONTEXT_BYTES = 8192

def _truncate(text: str, max_bytes: int = _MAX_CONTEXT_BYTES) -> str:
    encoded = text.encode("utf-8")
    if len(encoded) <= max_bytes:
        return text
    return encoded[:max_bytes].decode("utf-8", errors="ignore") + "\n[TRUNCATED]"


class RoundRobinOrchestrator:
    """Rotate through agents in declaration order."""

    def next_agent(self, ctx: TeamRunContext) -> TeamAgent:
        idx = ctx.current_turn % len(ctx.team.agents)
        return ctx.team.agents[idx]


class SelectorOrchestrator:
    """Use an LLM to choose the next agent based on conversation context."""

    DEFAULT_PROMPT = """You are a team orchestrator. Given the goal and conversation
history below, select the most appropriate next speaker from the available agents.

Goal: {goal}

Available agents:
{agent_roster}

Recent history (last {k} turns):
{history}

Respond with ONLY valid JSON in this exact format:
{{"next_speaker": "<role>", "reason": "<one sentence explanation>"}}"""

    def __init__(self, model: str, prompt_template: str | None = None):
        self.model = model
        self.template = prompt_template or self.DEFAULT_PROMPT

    def next_agent(self, ctx: TeamRunContext, max_retries: int = 3) -> TeamAgent:
        """Call selector LLM; fall back to roundrobin on repeated parse failure."""
        roster = "\n".join(
            f"  - {a.role} (profile: {a.profile})" for a in ctx.team.agents
        )
        k = ctx.team.context_turns
        history_lines = [
            f"[{t.role}]: {t.output[:500]}" for t in ctx.turn_history[-k:]
        ]
        prompt = self.template.format(
            goal=ctx.goal,
            agent_roster=roster,
            k=k,
            history="\n".join(history_lines) or "(none yet)",
        )
        role_map = {a.role: a for a in ctx.team.agents}

        for attempt in range(max_retries):
            raw = _call_selector_llm(self.model, prompt)  # implementation detail
            try:
                parsed = json.loads(raw)
                chosen_role = parsed["next_speaker"]
                if chosen_role in role_map:
                    return role_map[chosen_role]
            except (json.JSONDecodeError, KeyError):
                pass  # retry

        # Fallback: roundrobin
        idx = ctx.current_turn % len(ctx.team.agents)
        return ctx.team.agents[idx]


class SwarmOrchestrator:
    """Route based on HANDOFF_TO:<role> signals in agent output."""

    def next_agent(self, ctx: TeamRunContext) -> TeamAgent:
        """Scan the most recent turn for a handoff signal."""
        if ctx.turn_history:
            last = ctx.turn_history[-1]
            if last.handoff_target:
                role_map = {a.role: a for a in ctx.team.agents}
                if last.handoff_target in role_map:
                    return role_map[last.handoff_target]
                # Named role not in team — warn and fall back
                import sys
                print(
                    f"[warn] HANDOFF_TO:{last.handoff_target} — role not found "
                    f"in team '{ctx.team.name}'; falling back to roundrobin",
                    file=sys.stderr,
                )
        # No handoff signal — use roundrobin
        idx = ctx.current_turn % len(ctx.team.agents)
        return ctx.team.agents[idx]
```

#### 9.4.2 Handoff Signal Extraction

```python
def extract_handoff_target(output: str) -> str | None:
    """Extract HANDOFF_TO:<role> from agent output text (Swarm strategy)."""
    match = _HANDOFF_PATTERN.search(output)
    return match.group(1).strip() if match else None
```

### 9.5 Core Run Loop

```python
def run_team(
    ctx: TeamRunContext,
    conn: sqlite3.Connection,
    *,
    json_output: bool = False,
    max_turns: int | None = None,
    termination_phrase: str | None = None,
) -> TeamRunContext:
    """
    Main team execution loop. Persists every turn before advancing.
    Emits NDJSON events to stdout when json_output=True.
    """
    strategy = ctx.team.strategy
    max_t = max_turns or ctx.team.max_turns
    term_phrase = (termination_phrase or ctx.team.termination_phrase or "").lower()

    orchestrator: RoundRobinOrchestrator | SelectorOrchestrator | SwarmOrchestrator
    if strategy == Strategy.ROUNDROBIN:
        orchestrator = RoundRobinOrchestrator()
    elif strategy == Strategy.SELECTOR:
        model = ctx.team.selector_model or _default_selector_model(ctx.team)
        orchestrator = SelectorOrchestrator(model, ctx.team.selector_prompt_template)
    else:
        orchestrator = SwarmOrchestrator()

    _emit_event(json_output, "run_start", run_id=ctx.run_id,
                team=ctx.team.name, strategy=strategy.value, goal=ctx.goal)

    while ctx.current_turn < max_t and ctx.status == RunStatus.RUNNING:
        # Budget check before each turn
        if ctx.budget_usd and ctx.total_cost_usd >= ctx.budget_usd:
            ctx.status = RunStatus.BUDGET_EXCEEDED
            break

        agent = orchestrator.next_agent(ctx)
        ctx.current_turn += 1

        input_ctx = _build_input_context(ctx, agent)

        _emit_event(json_output, "turn_start",
                    turn=ctx.current_turn, role=agent.role, profile=agent.profile)

        turn_result = _execute_agent_turn(agent, input_ctx, ctx)

        # Extract swarm handoff target
        if strategy == Strategy.SWARM:
            turn_result.handoff_target = extract_handoff_target(turn_result.output)

        ctx.turn_history.append(turn_result)
        ctx.total_cost_usd += turn_result.cost_usd

        _persist_turn(conn, ctx, turn_result, input_ctx)

        _emit_event(json_output, "turn_end",
                    turn=ctx.current_turn, role=agent.role,
                    output=turn_result.output,
                    tokens=turn_result.prompt_tokens + turn_result.completion_tokens,
                    cost_usd=turn_result.cost_usd)

        # Termination phrase check
        if term_phrase and term_phrase in turn_result.output.lower():
            ctx.status = RunStatus.TERMINATED
            break

    if ctx.status == RunStatus.RUNNING:
        ctx.status = RunStatus.COMPLETED

    _update_run_status(conn, ctx)
    _emit_event(json_output, "run_end",
                run_id=ctx.run_id,
                turns=ctx.current_turn,
                total_tokens=sum(
                    t.prompt_tokens + t.completion_tokens for t in ctx.turn_history
                ),
                total_cost_usd=ctx.total_cost_usd,
                status=ctx.status.value)
    return ctx
```

### 9.6 Input Context Construction

```python
def _build_input_context(ctx: TeamRunContext, agent: TeamAgent) -> str:
    """
    Construct the prompt context passed to the agent for this turn.
    Includes: goal, team role, kanban board reference, conversation history.
    """
    k = ctx.team.context_turns
    history_section = ""
    if ctx.turn_history:
        lines = [
            f"[{t.role}]: {t.output}"
            for t in ctx.turn_history[-k:]
        ]
        history_section = "CONVERSATION_HISTORY:\n" + "\n---\n".join(lines)

    kanban_section = ""
    if ctx.board_id:
        kanban_section = f"TEAM_KANBAN_BOARD={ctx.board_id}\n"
        kanban_section += (
            "You can create, update, and close tasks on this shared board. "
            "Check the board for tasks created by your teammates before starting work.\n"
        )

    return _truncate(
        f"TEAM_GOAL: {ctx.goal}\n\n"
        f"YOUR_ROLE: {agent.role}\n\n"
        f"{kanban_section}"
        f"{history_section}\n\n"
        f"It is now your turn as '{agent.role}'. "
        f"Continue working toward the team goal."
    )
```

### 9.7 Database Access Pattern

All database access goes through `open_db()` from `controller.py`, consistent with all existing modules:

```python
def open_db(db_path: Path | None = None) -> sqlite3.Connection:
    """Open WAL-mode SQLite connection; create team tables if absent."""
    from tag.controller import open_db as _open_db  # re-export
    conn = _open_db(db_path)
    _ensure_schema(conn)
    return conn
```

`_ensure_schema(conn)` executes the DDL from section 9.2 using the standard `CREATE TABLE IF NOT EXISTS` + `PRAGMA table_info` pattern (as in `dag.py` and `loop_agent.py`).

### 9.8 Kanban Board Integration

At team run start, when `--no-kanban` is not set:

```python
from tag.kanban import Board, ensure_board_schema

def _provision_team_board(conn: sqlite3.Connection, team_name: str, run_id: str) -> str:
    """Create a dedicated kanban board for this team run. Returns board slug."""
    board_slug = f"team-{team_name}-{run_id[-8:]}"
    # Validate slug matches ^[a-z0-9][a-z0-9\-_]{0,63}$
    ensure_board_schema(conn)
    conn.execute(
        "INSERT OR IGNORE INTO kanban_boards(id, name, created_at) VALUES (?,?,?)",
        (board_slug, f"Team run: {team_name} / {run_id}", _utc_now())
    )
    conn.commit()
    return board_slug
```

### 9.9 OTel Span Integration

```python
from tag.tracing import get_tracer

def _with_spans(ctx: TeamRunContext) -> None:
    tracer = get_tracer("tag.teams")

    with tracer.start_as_current_span("tag.team.run") as root_span:
        root_span.set_attribute("team.name",     ctx.team.name)
        root_span.set_attribute("team.strategy", ctx.team.strategy.value)
        root_span.set_attribute("team.run_id",   ctx.run_id)
        root_span.set_attribute("team.goal",     ctx.goal[:256])
        # ... run loop here; each turn gets a child span:

        with tracer.start_as_current_span("tag.team.turn") as turn_span:
            turn_span.set_attribute("turn.number",  ctx.current_turn)
            turn_span.set_attribute("agent.role",   agent.role)
            turn_span.set_attribute("agent.profile",agent.profile)
            # populated after turn completes:
            turn_span.set_attribute("gen_ai.usage.input_tokens",  result.prompt_tokens)
            turn_span.set_attribute("gen_ai.usage.output_tokens", result.completion_tokens)
            turn_span.set_attribute("gen_ai.usage.cost_usd",      result.cost_usd)
```

### 9.10 Controller Integration

New command handlers in `controller.py` following existing patterns:

```python
# In the argparse subparser block:
team_parser = subparsers.add_parser("team", help="Multi-agent team primitives")
team_sub = team_parser.add_subparsers(dest="team_cmd")

# Subcommands: create, run, list, show, delete, run-show
# Each dispatches to a function in teams.py via:
#   from tag.teams import cmd_team_create, cmd_team_run, ...
```

The `cmd_team_run` handler is the heaviest; it:
1. Validates the team exists
2. Creates the `team_runs` row
3. Provisions the kanban board (unless `--no-kanban`)
4. Constructs `TeamRunContext`
5. Calls `run_team(ctx, conn, json_output=args.json, ...)`
6. Prints final summary

### 9.11 Agent Turn Execution

`_execute_agent_turn` reuses the existing `loop_agent.py`-style single-iteration loop:

```python
def _execute_agent_turn(
    agent: TeamAgent,
    input_context: str,
    ctx: TeamRunContext,
) -> TurnResult:
    """
    Execute one turn for the given agent using its bound profile.
    Delegates to the same Hermes/API bridge used by loop_agent.py.
    Token and cost tracking mirrors loop_iterations handling.
    """
    from tag.hermes_bridge import run_single_turn

    result = run_single_turn(
        profile=agent.profile,
        user_message=input_context,
    )
    return TurnResult(
        role=agent.role,
        profile=agent.profile,
        output=result.content,
        prompt_tokens=result.usage.prompt_tokens,
        completion_tokens=result.usage.completion_tokens,
        cost_usd=result.usage.cost_usd,
    )
```

---

## 10. Security Considerations

1. **Name injection prevention.** Team names and role names are validated against `^[a-z0-9][a-z0-9\-_]{0,63}$` before any use in SQL or kanban board slug construction. All SQL uses parameterized queries (never f-string interpolation). A unit test with adversarial names (SQL injection payloads, path traversal) must pass.

2. **Kanban board isolation.** Each team run gets a unique board slug incorporating the run ID. Cross-team board access is not possible without knowing the exact slug. No board contents are exposed in `tag team list` output.

3. **Selector LLM prompt injection.** The goal text and conversation history injected into the selector prompt are treated as untrusted data. The selector prompt template uses format placeholders (not f-strings with arbitrary substitution); goal and history values are truncated to 4 KB before inclusion to limit prompt injection attack surface.

4. **Input context truncation.** `_truncate()` enforces an 8 KB hard ceiling on the `input_context` column and on the kanban section included in agent prompts. This prevents a malicious or runaway agent from producing unbounded output that inflates the next agent's context to millions of tokens.

5. **Secret scanning at turn boundaries.** If PRD-034 secret scanning is enabled (`security.SCAN_OUTPUTS=True`), each turn output is passed through the secret scanner before persistence. A detected secret blocks the output from being stored and sets the turn `error` field to `SECRET_DETECTED`.

6. **API key exposure.** `selector_model` is stored in the `teams` table as a model identifier (e.g., `anthropic/claude-haiku-3`), never as an API key. Actual API keys remain in the profile configuration layer (outside `teams.py`).

7. **Budget enforcement as a security boundary.** `--budget-usd` is enforced before each turn, not after. A runaway agent that produces tokens rapidly cannot exceed the budget by more than one turn's cost. This is a defense-in-depth measure against runaway loops (complements PRD-039).

8. **Sandboxed code execution.** When any agent in the team has code execution tools enabled (PRD-028 sandbox), those tools run inside the existing sandbox layer. `teams.py` does not weaken or bypass sandbox constraints.

9. **Swarm handoff target validation.** `HANDOFF_TO:<role>` targets are validated against the team's known role set before routing. An agent cannot route to a role outside its team. Unknown targets fall back to roundrobin with a stderr warning, not a crash.

10. **OTel data minimization.** Span attributes for `team.goal` are truncated to 256 characters before export. Full goal text stays in the local SQLite database only and is not exported via OTLP unless explicitly configured.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_teams.py`)

- `test_roundrobin_rotation`: Assert agent at turn N is `agents[N % len(agents)]` for N in range(30) across team sizes 1, 2, 3, 5, 10.
- `test_roundrobin_termination_phrase`: Assert run halts immediately when termination phrase is found in turn output.
- `test_roundrobin_max_turns`: Assert run halts at exactly `max_turns` turns with status=`completed`.
- `test_selector_fallback_on_parse_error`: Mock LLM returning malformed JSON 3 times; assert fallback to roundrobin on turn 4.
- `test_selector_valid_response`: Mock LLM returning `{"next_speaker": "coder", "reason": "..."}` ; assert `coder` agent is selected.
- `test_swarm_handoff_detected`: Synthetic turn output containing `HANDOFF_TO: reviewer`; assert `extract_handoff_target` returns `"reviewer"`.
- `test_swarm_handoff_case_insensitive`: `handoff_to:REVIEWER` and `HANDOFF_TO:Reviewer` both detected.
- `test_swarm_unknown_role_fallback`: `HANDOFF_TO:nonexistent` triggers roundrobin fallback and stderr warning.
- `test_truncate_input_context`: Input > 8 KB is truncated to ≤ 8 KB with `[TRUNCATED]` suffix.
- `test_team_name_validation`: Names with spaces, dots, path separators, SQL keywords rejected; valid slugs accepted.
- `test_budget_exceeded`: Team run with `budget_usd=0.001` and mocked turns costing $0.0006 each; run terminates after 2nd turn.
- `test_no_kanban_flag`: Assert no kanban board is provisioned and no `TEAM_KANBAN_BOARD=` appears in input context.
- `test_dry_run_exits_zero`: `cmd_team_run` with `dry_run=True` returns without executing any agent calls.
- `test_agents_json_roundtrip`: `TeamDefinition.agents_json()` → `json.loads()` → `TeamAgent` reconstruction is lossless.

### 11.2 Integration Tests (`tests/test_teams_integration.py`)

- `test_create_and_show`: `cmd_team_create` followed by `cmd_team_show`; assert returned definition matches inputs.
- `test_list_returns_created_team`: After create, `cmd_team_list --json` includes the new team.
- `test_delete_removes_definition_keeps_runs`: Create team, run it once (mocked agents), delete team; assert `team_runs` row still present.
- `test_run_persists_all_turns`: 3-agent roundrobin run with mocked agents, 6 turns; assert 6 rows in `team_turns`, correct agent order.
- `test_kanban_board_provisioned`: Run with kanban enabled; assert board slug row exists in `kanban_boards`.
- `test_otel_spans_emitted`: Capture OTel spans from a mocked team run; assert one root span and N child spans with correct attributes.
- `test_json_output_valid_ndjson`: Capture stdout from `run_team` with `json_output=True`; assert every line is parseable JSON with expected event types.
- `test_selector_model_override`: Create team with `--selector-model anthropic/claude-haiku-3`; assert `selector_model` stored correctly.
- `test_duplicate_name_rejected`: Second `cmd_team_create` with same name (no `--upsert`) returns non-zero exit code.
- `test_concurrent_reads_during_run`: Start team run in thread, concurrently call `cmd_team_list` 50 times; assert no SQLite errors.

### 11.3 Performance Benchmark (`tests/bench_teams.py`)

```python
import time, statistics

def bench_orchestration_overhead(n_agents=20, n_turns=100):
    """Measure per-turn overhead excluding LLM time."""
    # Mock _execute_agent_turn to return instantly
    ...
    times = []
    for _ in range(n_turns):
        t0 = time.perf_counter()
        # One turn cycle: next_agent() + _build_input_context() + _persist_turn()
        ...
        times.append(time.perf_counter() - t0)
    p99 = statistics.quantiles(times, n=100)[98]
    assert p99 < 0.05, f"P99 turn overhead {p99*1000:.1f}ms exceeds 50ms target"
```

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag team create "crew" --agent orchestrator:orchestrator --agent coder:coder --strategy roundrobin` exits 0 and creates a row in `teams` with correct `agents_json`. | Integration test + SQL assertion |
| AC-02 | `tag team create "crew"` without `--agent` exits non-zero and prints an error containing "at least one agent". | Unit test |
| AC-03 | `tag team create "invalid name"` (contains space) exits non-zero and prints the valid name pattern. | Unit test |
| AC-04 | `tag team run "crew" --goal "Build X"` with a 3-member roundrobin team calls agents in order orchestrator→coder→reviewer→orchestrator… and persists all turns to `team_turns`. | Integration test |
| AC-05 | `tag team run "crew" --max-turns 6 --goal "Build X"` stops at exactly 6 turns (status=`completed`). | Integration test |
| AC-06 | `tag team run "crew" --goal "Build X" --termination-phrase "DONE"` stops immediately when an agent outputs text containing "DONE" (case-insensitive). | Unit test |
| AC-07 | Selector strategy selects `coder` when the mocked LLM returns `{"next_speaker": "coder", "reason": "needs coding"}`. | Unit test |
| AC-08 | Selector strategy falls back to roundrobin after 3 consecutive LLM parse failures; no exception is raised. | Unit test |
| AC-09 | Swarm strategy routes to `reviewer` when coder output contains `HANDOFF_TO: reviewer`. | Unit test |
| AC-10 | Swarm strategy logs a warning (stderr) and falls back to roundrobin when `HANDOFF_TO:unknown` is detected. | Unit test capturing stderr |
| AC-11 | A kanban board named `team-<team_name>-<run_id_suffix>` exists in `kanban_boards` after a team run without `--no-kanban`. | Integration test |
| AC-12 | `--no-kanban` flag results in no board row created and no `TEAM_KANBAN_BOARD=` in any `input_context` column. | Integration test |
| AC-13 | `tag team list --json` output is valid JSON (parseable by `json.loads()`); contains the team created in AC-01. | Integration test |
| AC-14 | `tag team show "crew" --json` output contains `agents` array with correct `role` and `profile` entries. | Integration test |
| AC-15 | `tag team delete "crew"` without `--yes` prompts for confirmation and does not delete on 'N' input. | Integration test with mocked stdin |
| AC-16 | `tag team delete "crew" --yes` removes the `teams` row; previously created `team_runs` rows remain. | Integration test |
| AC-17 | Each turn in `team_turns` has `prompt_tokens > 0`, `completion_tokens > 0`, and `cost_usd > 0.0` (using mocked LLM returning realistic usage). | Integration test |
| AC-18 | `--budget-usd 0.001` with mocked turns costing $0.0006 each aborts after turn 2 with status=`budget_exceeded`. | Unit test |
| AC-19 | OTel root span `tag.team.run` contains attributes `team.name`, `team.strategy`, `team.run_id`, `team.goal` (truncated to 256 chars). | Span capture test |
| AC-20 | Each OTel child span `tag.team.turn` contains `turn.number`, `agent.role`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.cost_usd`. | Span capture test |
| AC-21 | `--dry-run` prints the execution plan and exits 0 without inserting any rows into `team_runs` or `team_turns`. | Integration test + SQL row count assertion |
| AC-22 | Input context exceeding 8 KB is stored as truncated text ending in `[TRUNCATED]`. | Unit test |
| AC-23 | `tag team run show <run_id>` returns all turns for that run in turn_number order. | Integration test |
| AC-24 | Second `tag team create "crew"` (same name, no `--upsert`) exits non-zero with error containing "already exists". | Integration test |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| `src/tag/kanban.py` (PRD-004) | Internal | Board provisioning, `ensure_board_schema`, `kanban_boards` table |
| `src/tag/loop_agent.py` (PRD-021) | Internal | Single-turn agent execution pattern reused in `_execute_agent_turn` |
| `src/tag/tracing.py` (PRD-013) | Internal | OTel tracer acquisition via `get_tracer()` |
| `src/tag/hermes_bridge.py` | Internal | `run_single_turn()` — actual LLM API call dispatch |
| `src/tag/security.py` (PRD-034) | Internal | Optional secret scanning of turn outputs |
| `src/tag/budget.py` (PRD-039) | Internal | Per-turn cost tracking; budget enforcement |
| `src/tag/otel_semconv.py` (PRD-041) | Internal | `gen_ai.usage.*` span attribute names |
| `src/tag/controller.py` | Internal | `open_db()`, argparse subparser registration |
| `sqlite3` (stdlib) | stdlib | WAL-mode database; no additional install required |
| `dataclasses` (stdlib) | stdlib | `TeamDefinition`, `TurnResult`, `TeamRunContext` |
| `re` (stdlib) | stdlib | Handoff signal pattern matching |
| `json` (stdlib) | stdlib | Agent JSON serialization, selector response parsing |
| `secrets` (stdlib) | stdlib | Run ID and team ID generation |

No new third-party dependencies are introduced. The optional OTel exporter dependency is already present from PRD-013.

---

## 14. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|-------------------|
| OQ-1 | Should the shared kanban board be auto-deleted after team run completion, or retained indefinitely? Retention is useful for post-run analysis but accumulates boards over time. Proposal: retain by default, add `--delete-board` flag. | Team | Phase 2 design |
| OQ-2 | The Selector strategy makes an extra LLM call per turn, adding cost and latency. Should there be a cost-cap per selector call (e.g., max 500 tokens)? | Team | Phase 1 implementation |
| OQ-3 | For Swarm strategy, should `HANDOFF_TO:<role>` be stripped from the output before it is passed to the next agent's history? Including it could confuse downstream agents; removing it loses traceability. Proposal: strip from `input_context` history but retain in `team_turns.output`. | Team | Phase 1 implementation |
| OQ-4 | Should teams support a `--max-turns-per-agent <N>` guard to prevent any single agent from dominating a Swarm run (e.g., cascading self-handoffs)? | Team | Phase 2 |
| OQ-5 | The context window for the conversation history (`context_turns`, default 5) may be insufficient for complex goals requiring deep context. Should this be per-agent (different roles need different amounts of history) or global? | Team | Phase 2 |
| OQ-6 | How should team runs interact with `tag eval` (PRD-027)? A team run could be an eval target (evaluate the team's collective output), but the eval framework today targets single profiles. Should `tag eval run --team <name>` be added? | Team | Future PRD |
| OQ-7 | Should team definitions be exportable/importable as YAML (analogous to profile YAML export)? This would enable sharing team configurations across TAG installations and in source control. | Team | Phase 2 |
| OQ-8 | The `_call_selector_llm` function needs an implementation. Should it go through `hermes_bridge.run_single_turn` with a synthetic profile, or directly through the model API? The former is cleaner but creates a circular dependency on profile existence. | Team | Phase 1 implementation |

---

## 15. Complexity and Timeline

### Phase 1 — Core (Days 1–5)

**Day 1–2: Schema, dataclasses, and basic CRUD**
- Write `teams.py` skeleton with `TeamDefinition`, `TurnResult`, `TeamRunContext` dataclasses
- Implement `_ensure_schema()` with full DDL from section 9.2
- Implement `cmd_team_create`, `cmd_team_list`, `cmd_team_show`, `cmd_team_delete`
- Register subparsers in `controller.py`
- Unit tests: name validation, CRUD round-trips, `--json` output format

**Day 3–4: RoundRobin strategy + run loop**
- Implement `RoundRobinOrchestrator`
- Implement `run_team()` core loop: turn execution, persistence, termination conditions
- Implement `_build_input_context()` with truncation
- Implement `_persist_turn()` and `_update_run_status()`
- Integration test: 3-agent, 6-turn run; assert all turns persisted in correct order

**Day 5: Kanban integration + OTel spans**
- Implement `_provision_team_board()` using `kanban.py`
- Inject board context into `_build_input_context()`
- Wire OTel spans via `tracing.get_tracer()`
- Unit test: kanban board provisioned; OTel spans captured with correct attributes

### Phase 2 — Selector + Swarm (Days 6–8)

**Day 6: Selector strategy**
- Implement `SelectorOrchestrator` with default prompt template
- Implement `_call_selector_llm()` using `hermes_bridge`
- Implement parse-failure fallback to roundrobin (3 retries)
- Unit tests: valid response routing, parse failure fallback, custom prompt template

**Day 7: Swarm strategy**
- Implement `SwarmOrchestrator`
- Implement `extract_handoff_target()` with regex
- Handle unknown role fallback, output stripping (OQ-3 resolution)
- Unit tests: all handoff signal variants, unknown role warning, case insensitivity

**Day 8: Budget enforcement + dry-run + run-show**
- Implement `--budget-usd` check in run loop
- Implement `--dry-run` plan output
- Implement `cmd_team_run_show` with `--turn` filter
- Integration tests: budget exceeded, dry-run no DB writes

### Phase 3 — Hardening + Performance (Days 9–10)

**Day 9: Security hardening + edge cases**
- Add adversarial name validation tests (SQL injection, path traversal)
- Add secret scanning integration at turn boundary (PRD-034)
- Add concurrent read test (50 concurrent `tag team list` during a run)
- Add input > 8 KB truncation test

**Day 10: Performance benchmark + documentation**
- Run `bench_teams.py`; assert P99 < 50 ms with 20-agent team
- Add `tag team` to CLI help and `--help` output
- Update `INDEX.md` in `docs/prd/`
- Final CI pass: coverage ≥ 85%, all acceptance criteria green

### Total: 10 business days (~2 weeks)

Consistent with **M (1-2 weeks)** effort estimate. The complexity is rated 3/5: the orchestration logic itself is straightforward Python, but the integration surface (SQLite WAL, kanban, OTel, hermes_bridge, controller argparse) is broad and requires careful testing of each boundary.

---

---

## Enhancement: Trinity-Style Dynamic Role Assignment

**Added:** v0.7.2 planning cycle — inspired by Sakana AI Trinity (ICLR 2026, arXiv:2604.xxxxx) and Conductor (ICLR 2026).

### Background

The initial PRD defines team orchestration with static roles: each profile is assigned one role (researcher, coder, reviewer) and keeps it for the entire run. Sakana AI's **Trinity** paper demonstrates that a compact evolved coordinator — under 20K parameters — dynamically assigns **Thinker**, **Worker**, and **Verifier** roles to a pool of LLMs *turn-by-turn*, not run-by-run. Their companion **Conductor** paper trains a 7B RL model to write *different specialist instructions* for each worker model per turn. Together they achieve 83.9% on LiveCodeBench and 87.5% on GPQA-Diamond by continuously re-assigning roles based on the current conversational state.

TAG cannot train a Conductor — that requires RL fine-tuning on millions of tokens. But the *role rotation* pattern is implementable at the software layer using TAG's existing profile system and the `teams.py` orchestration runtime.

### New Team Strategy: `TrinityRotation`

```bash
# Create a Trinity-style rotating-role team
tag team create trinity-dev \
    --members researcher,coder,reviewer \
    --strategy trinity \
    --coordinator orchestrator \
    --max-turns 12

# Run a goal through the team
tag team run trinity-dev \
    --goal "Implement a Redis-backed rate limiter with tests and a benchmark"

# Trinity strategy emits per-turn role assignments in trace
tag team trace <run-id>
```

**How it works:**

1. Before each turn, the `orchestrator` coordinator profile (acting as Trinity's evolved coordinator) is called with the current conversation history and emits a JSON role assignment:

```json
{
  "turn": 3,
  "assignments": {
    "coder": "worker",
    "reviewer": "verifier",
    "researcher": "thinker"
  },
  "rationale": "coder has produced a draft; reviewer should now verify correctness; researcher should think about edge cases"
}
```

2. **Thinker** speaks first: generates a chain-of-thought about the current state and what's needed next (not yet acting).
3. **Worker** acts on the Thinker's reasoning: writes code, makes edits, calls tools.
4. **Verifier** critiques the Worker's output, requests corrections, or approves it for the next turn.
5. Roles rotate on each turn based on the coordinator's judgment — the coder can become the verifier if the verifier proved to be the best critic in prior turns.

### Role Assignment Schema

```json
{
  "turn": 3,
  "assignments": {
    "<profile_name>": "thinker | worker | verifier | idle"
  },
  "rationale": "<one-sentence explanation>",
  "next_speaker": "<profile_name>"
}
```

`idle` agents skip the turn and receive only a summary, preserving their context window. This is Trinity's key efficiency: agents not assigned an active role are not called, avoiding redundant API calls.

### Conductor-Inspired Specialist Instructions

When the coordinator assigns roles, it also emits *per-agent turn instructions* — a brief, targeted directive for each active agent in this turn:

```json
{
  "turn_instructions": {
    "coder": "Focus on the Redis connection pool initialization in the rate_limiter.py module. Use hiredis for performance.",
    "reviewer": "Verify that the atomic INCR + EXPIRE Redis sequence is race-condition-free under concurrent requests.",
    "researcher": "Think about whether sliding window or token bucket semantics are more appropriate for this use case."
  }
}
```

These instructions are prepended to each agent's context for this turn. They replicate Conductor's key capability — writing "specialized natural-language instructions for each worker LLM" — without training a 7B model.

### Implementation Details

**New strategy class in `teams.py`:**

```python
class TrinityRotationStrategy:
    def __init__(self, coordinator_profile: str, max_turns: int):
        self.coordinator = coordinator_profile
        self.max_turns = max_turns

    def assign_roles(self, history: list[dict], members: list[str]) -> dict:
        """Call coordinator to assign Thinker/Worker/Verifier roles for this turn."""
        prompt = TRINITY_ROLE_ASSIGNMENT_PROMPT.format(
            history=_format_history(history),
            members=", ".join(members),
        )
        raw = _invoke_profile(self.coordinator, prompt)
        return _parse_json_safe(raw) or _fallback_assignment(members)

    def run(self, goal: str, team: Team) -> TeamResult:
        """Execute the Trinity rotation loop for up to max_turns."""
        history = [{"role": "system", "content": goal}]
        for turn in range(self.max_turns):
            assignment = self.assign_roles(history, [m.name for m in team.members])
            if assignment.get("status") == "done":
                break
            thinker, worker, verifier = _extract_roles(assignment, team)
            if thinker:
                thought = _run_turn(thinker, history, role="thinker",
                                    instruction=assignment["turn_instructions"].get(thinker.name, ""))
                history.append({"role": thinker.name, "kind": "thought", "content": thought})
            if worker:
                action = _run_turn(worker, history, role="worker",
                                   instruction=assignment["turn_instructions"].get(worker.name, ""))
                history.append({"role": worker.name, "kind": "action", "content": action})
            if verifier:
                verdict = _run_turn(verifier, history, role="verifier",
                                    instruction=assignment["turn_instructions"].get(verifier.name, ""))
                history.append({"role": verifier.name, "kind": "verdict", "content": verdict})
                if "APPROVED" in verdict.upper():
                    break
        return _synthesize_history(history)
```

### New DB Column

```sql
ALTER TABLE team_runs ADD COLUMN strategy_metadata_json TEXT;
-- Stores per-turn role assignments for trace replay and debugging
```

### New CLI Flags for Trinity

```bash
# Existing: tag team create <name> --members A,B,C --strategy roundrobin
# New:
tag team create <name> --members A,B,C --strategy trinity \
    --coordinator orchestrator \
    --max-turns 15 \
    --require-verifier-approval  # don't proceed to next turn until verifier approves

# Inspect role assignments per turn
tag team trace <run-id> --show-roles
# Output shows:
# Turn 1: researcher=thinker, coder=worker, reviewer=verifier
# Turn 2: coder=thinker, reviewer=worker, researcher=verifier
# Turn 3: researcher=idle, coder=worker, reviewer=verifier → APPROVED

# Export turn-by-turn transcript
tag team trace <run-id> --format json > transcript.json
```

### Performance Characteristics

- **Coordinator overhead:** 1 extra LLM call per turn (coordinator role assignment). For 12 turns, 12 coordinator calls + up to 36 member calls = 48 total calls max.
- **Efficiency gain vs RoundRobin:** Idle agents (role=`idle`) skip the turn, saving 1/3 of calls on average when one agent is idle per turn.
- **Quality vs RoundRobin:** Trinity paper shows 15–25% quality improvement over static round-robin on complex reasoning tasks by concentrating the right expertise at the right moment.

### Testing Requirements (Trinity extension)

| Test | Assertion |
|---|---|
| `test_trinity_role_assignment_parses` | Coordinator JSON parsed correctly; thinker/worker/verifier extracted |
| `test_trinity_role_rotation` | Role assignments differ between turns (coordinator actually rotates) |
| `test_trinity_idle_skips` | Member with role=idle not called for that turn |
| `test_trinity_verifier_stops` | APPROVED in verifier output terminates the turn loop |
| `test_trinity_fallback_assignment` | Malformed coordinator JSON → fallback round-robin assignment |
| `test_trinity_turn_instructions` | Each active member's prompt contains its turn instruction |
| `test_trinity_max_turns_terminates` | Loop stops at max_turns even without APPROVED |

*GitHub issue: #347*

