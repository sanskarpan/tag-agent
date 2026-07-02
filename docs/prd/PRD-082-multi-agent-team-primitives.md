# PRD-082: Multi-Agent Team Primitives: RoundRobin, Selector, Swarm Handoff (`tag team`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `internal/swarm`
**Depends on:** PRD-004 (kanban swarm helpers), PRD-008 (background task queue), PRD-013 (agent tracing/observability), PRD-021 (agent loop/autonomous mode), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-033 (dependency-aware task queue), PRD-034 (secret scanning), PRD-041 (OTel GenAI span cost attribution)
**Inspired by:** AutoGen AgentChat team types, CrewAI crews, MAF agent groups

---

## 1. Overview

TAG today runs one agent at a time: a single profile receives a goal, iterates through its loop, and terminates. Even the existing swarm helpers in `internal/swarm` (PRD-004) are fundamentally a fan-out pattern — tasks are distributed to agents independently, not orchestrated through a shared conversation or a structured handoff protocol. There is no mechanism to compose named, reusable groups of agents with a declared coordination strategy, run a shared goal through the group, and observe the multi-agent interaction as a first-class entity.

Multi-agent team frameworks have converged on three foundational orchestration primitives: **RoundRobin** (agents take turns speaking in fixed rotation, excellent for iterative refinement and deliberation), **Selector** (an LLM or rule-based router chooses which agent speaks next, optimal for dynamic dispatch based on expertise), and **Swarm** (agents autonomously emit handoff signals that determine the next speaker, modeled after AutoGen's `HandoffMessage` pattern). These three primitives cover the overwhelming majority of real agentic collaboration patterns — feature teams, review cycles, research pipelines, and full-stack engineering crews.

This PRD introduces `tag team`: a first-class CLI namespace that lets users define named teams of TAG profiles with an explicit orchestration strategy, run any goal through a team, observe per-turn transcripts and per-agent cost/token attribution, and share a kanban board across team members for work-item tracking. The team definition is stored in the shared SQLite store (persistent, inspectable, portable) and the runtime runs as a coordinated sequence of agent-loop iterations — each agent executing as a goroutine driven by the hand-rolled ~200-LOC continue|compact|stop state machine in `internal/agent` — stitched together by the orchestration strategy layer in `internal/swarm`.

The design is directly inspired by AutoGen AgentChat's team types (`RoundRobinGroupChat`, `SelectorGroupChat`, `Swarm`), CrewAI's `Crew` primitive with `Process.sequential` / `Process.hierarchical` modes, and the Multi-Agent Framework (MAF) `AgentGroup` abstraction. All three frameworks share a common insight: the team definition (membership, strategy, termination condition) should be declarative and reusable, while the execution runtime should be observable and interruptible.

A shared kanban board, implemented on top of the existing `internal/store` kanban layer, provides a shared work-item ledger accessible by all agents in the team. Any agent can create, update, or close kanban tasks during its turn; the next agent in the rotation can read those tasks to understand what work has been done and what remains. This kanban integration bridges the gap between conversational coordination (messages) and work-item coordination (tasks), mirroring how real engineering teams work.

---

## 2. Problem Statement

### 2.1 No Reusable Multi-Agent Composition Primitive

TAG users who need more than one agent to collaborate on a goal today must either: (a) manually chain `tag run` commands in shell scripts, passing output files between invocations; or (b) use `tag queue` with dependency edges (PRD-033), which serializes work but provides no shared conversational context and no handoff protocol. Neither approach is reusable — the orchestration logic lives in the user's shell script, not in TAG configuration. Every new project requires rebuilding the same wiring from scratch.

CrewAI solves this by making the crew definition (`Crew(agents=[...], tasks=[...], process=Process.sequential)`) a first-class, saveable unit. AutoGen solves it by making the team definition (`RoundRobinGroupChat(participants=[...])`) the unit of reuse. TAG has no equivalent. A platform engineer who wants to run the same "research + writer + editor" team across dozens of projects must copy-paste shell scripts or build their own orchestration layer on top of TAG.

### 2.2 No Structured Agent-to-Agent Handoff

When agents need to hand work from one to another, the current TAG patterns offer two unsatisfying options: (a) write output to a file and have the next agent read it — opaque, brittle, no conversation threading; or (b) stuff the full prior output into the next agent's prompt — context-polluting, token-expensive, and lossy. Neither approach supports the richer handoff semantics used by AutoGen (where `HandoffMessage(target="reviewer")` is a structured signal emitted by the agent runtime) or the OpenAI Agents SDK (where `handoff(agent)` transfers the conversation object with full history intact).

Without structured handoff, downstream agents do not know why they are receiving work, what context was established by the upstream agent, or what specifically needs their attention. The result is lower-quality outputs and higher token costs as each agent re-derives context that the prior agent had already established.

### 2.3 No Shared State Across Concurrent Agents

TAG's existing kanban layer (`internal/store`, PRD-004) tracks tasks per-board, but there is no protocol for multiple agents to share a board in a coordinated way during a single team run. Agents that run concurrently may create duplicate tasks, overwrite each other's updates, or fail to notice completed work items when composing their next turn. A shared, transactionally safe kanban board that all team members can read and write is the missing coordination primitive.

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
| `--json` output validity | `tag team list --json` and `tag team show --json` produce valid JSON parseable by `encoding/json` | CI unit test |
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
| FR-14 | `tag team list` returns all defined teams sorted by `created_at DESC`. `--json` output must be valid JSON (parseable by `encoding/json`). | P1 |
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
| NFR-04 | `internal/swarm` must not import any external multi-agent framework (AutoGen, CrewAI, LangChain, eino, langchaingo) at compile time; all orchestration logic is native TAG Go | `go build ./internal/swarm/...` + `go mod graph` assertion in CI |
| NFR-05 | Selector strategy LLM call uses a lightweight, cost-efficient model by default (configurable via `--selector-model`); the default must not use the most expensive model tier | Documented default; unit test verifies default selection |
| NFR-06 | `--json` output to stdout must not interleave with log output; logs go to stderr | Output capture assertion in integration test |
| NFR-07 | Team names and role names are validated against `^[a-z0-9][a-z0-9\-_]{0,63}$` and used only in parameterized SQLite queries and `internal/store` kanban board slug construction; no SQL injection vectors | Security unit test with adversarial name inputs |
| NFR-08 | The `team_turns` table `input_context` column stores at most 8 KB per row (truncated with a `[TRUNCATED]` marker); full context is never stored to prevent unbounded DB growth | Unit test: input > 8 KB is truncated |
| NFR-09 | `tag team delete` requires user confirmation to prevent accidental team loss; the `--yes` flag bypasses this for scripted use only | Integration test: delete without --yes prompts |
| NFR-10 | All new code in `internal/swarm` must have ≥ 85% line coverage in the test suite | Coverage report via `go test -cover` in CI |

---

## 9. Technical Design

### 9.1 New Package: `internal/swarm/team.go`

The entire team primitive implementation lives in the `internal/swarm` package, alongside the existing wave runner, ContextBus, and manifest DFS acyclic coordinator. The package exposes the three strategy implementations, the run loop, and the SQLite persistence layer — consistent with the single-package-per-subsystem convention used in `internal/queue`, `internal/store`, and `internal/agent`.

Primary files:

```
internal/swarm/
    team.go          # TeamDefinition, TeamAgent, TurnResult, RunContext structs + SQLite CRUD
    strategy.go      # Orchestrator interface + RoundRobin, Selector, Swarm implementations
    run.go           # RunTeam() main loop, input context construction, turn persistence
    kanban.go        # provisionTeamBoard() helper delegating to internal/store
    otel.go          # OTel span helpers (root + per-turn child spans)
```

CLI commands live in `internal/cli/team.go`, registered as a cobra sub-command tree (`tag team create|run|list|show|delete|run-show`).

### 9.2 SQLite DDL

The DDL is applied via `internal/store`'s migration runner (single-writer, `modernc.org/sqlite`, WAL mode, `gofrs/flock` atomic RMW). Tables are created with `CREATE TABLE IF NOT EXISTS` at startup.

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

### 9.3 Core Go Structs

Types use plain Go structs; JSON serialization via `encoding/json` with struct tags; schema generation for tool definitions via `invopop/jsonschema`. Run IDs and team IDs are generated with `crypto/rand`.

```go
package swarm

import (
    "crypto/rand"
    "encoding/hex"
    "encoding/json"
    "time"
)

// Strategy identifies the orchestration algorithm for a team run.
type Strategy string

const (
    StrategyRoundRobin Strategy = "roundrobin"
    StrategySelector   Strategy = "selector"
    StrategySwarm      Strategy = "swarm"
)

// RunStatus is the terminal or in-progress state of a team run.
type RunStatus string

const (
    RunStatusRunning        RunStatus = "running"
    RunStatusCompleted      RunStatus = "completed"
    RunStatusFailed         RunStatus = "failed"
    RunStatusTerminated     RunStatus = "terminated"
    RunStatusBudgetExceeded RunStatus = "budget_exceeded"
)

// TeamAgent is a single member of a team: a named role bound to a TAG profile.
type TeamAgent struct {
    Role    string `json:"role"`    // e.g. "coder"
    Profile string `json:"profile"` // TAG profile name, e.g. "coder"
}

// TeamDefinition is the persistent team configuration stored in the `teams` table.
type TeamDefinition struct {
    ID                     string      `json:"id"`
    Name                   string      `json:"name"`
    Strategy               Strategy    `json:"strategy"`
    Agents                 []TeamAgent `json:"agents"`
    MaxTurns               int         `json:"max_turns"`
    TerminationPhrase      *string     `json:"termination_phrase"`
    SelectorModel          *string     `json:"selector_model"`
    SelectorPromptTemplate *string     `json:"selector_prompt_template"`
    ContextTurns           int         `json:"context_turns"`
    Description            *string     `json:"description"`
    CreatedAt              time.Time   `json:"created_at"`
    UpdatedAt              time.Time   `json:"updated_at"`
}

// AgentsJSON serializes the agent list for storage.
func (td *TeamDefinition) AgentsJSON() (string, error) {
    b, err := json.Marshal(td.Agents)
    return string(b), err
}

// TurnResult holds the output of a single agent turn.
type TurnResult struct {
    Role             string
    Profile          string
    Output           string
    PromptTokens     int
    CompletionTokens int
    CostUSD          float64
    HandoffTarget    *string // populated by swarm strategy
    Err              error
}

// RunContext carries all mutable state for an in-progress team run.
type RunContext struct {
    RunID        string
    Team         *TeamDefinition
    Goal         string
    BoardID      *string
    BudgetUSD    *float64
    TurnHistory  []TurnResult
    TotalCostUSD float64
    CurrentTurn  int
    Status       RunStatus
}

func newID(prefix string, n int) string {
    b := make([]byte, n)
    _, _ = rand.Read(b)
    return prefix + hex.EncodeToString(b)
}
```

### 9.4 Orchestration Strategy Implementations

All three strategies implement the `Orchestrator` interface. Each `NextAgent` call is synchronous; turns are sequential per NG1. The selector strategy dispatches an LLM call via `internal/llm`'s provider interface (`Stream(ctx, req) <-chan Event`) and falls back to round-robin on parse failure.

```go
package swarm

import (
    "context"
    "encoding/json"
    "fmt"
    "log/slog"
    "regexp"
    "strings"

    "github.com/tag/internal/llm"
)

const maxContextBytes = 8192

var handoffPattern = regexp.MustCompile(`(?i)HANDOFF_TO\s*:\s*(\w[\w\-]*)`)

// Orchestrator selects the next agent to speak.
type Orchestrator interface {
    NextAgent(ctx context.Context, rc *RunContext) (*TeamAgent, error)
}

// truncate enforces the 8 KB ceiling on stored context blobs.
func truncate(s string) string {
    b := []byte(s)
    if len(b) <= maxContextBytes {
        return s
    }
    return string(b[:maxContextBytes]) + "\n[TRUNCATED]"
}

// --- RoundRobin ---

type roundRobinOrchestrator struct{}

func (r *roundRobinOrchestrator) NextAgent(_ context.Context, rc *RunContext) (*TeamAgent, error) {
    idx := rc.CurrentTurn % len(rc.Team.Agents)
    return &rc.Team.Agents[idx], nil
}

// --- Selector ---

type selectorOrchestrator struct {
    model    string
    template string
    provider llm.Provider
}

const defaultSelectorPrompt = `You are a team orchestrator. Given the goal and conversation
history below, select the most appropriate next speaker from the available agents.

Goal: {{.Goal}}

Available agents:
{{.Roster}}

Recent history (last {{.K}} turns):
{{.History}}

Respond with ONLY valid JSON in this exact format:
{"next_speaker": "<role>", "reason": "<one sentence explanation>"}`

func (s *selectorOrchestrator) NextAgent(ctx context.Context, rc *RunContext) (*TeamAgent, error) {
    roleMap := make(map[string]*TeamAgent, len(rc.Team.Agents))
    rosterLines := make([]string, 0, len(rc.Team.Agents))
    for i := range rc.Team.Agents {
        a := &rc.Team.Agents[i]
        roleMap[a.Role] = a
        rosterLines = append(rosterLines, fmt.Sprintf("  - %s (profile: %s)", a.Role, a.Profile))
    }
    k := rc.Team.ContextTurns
    historyLines := make([]string, 0, k)
    start := len(rc.TurnHistory) - k
    if start < 0 {
        start = 0
    }
    for _, t := range rc.TurnHistory[start:] {
        snippet := t.Output
        if len(snippet) > 500 {
            snippet = snippet[:500]
        }
        historyLines = append(historyLines, fmt.Sprintf("[%s]: %s", t.Role, snippet))
    }
    historyStr := strings.Join(historyLines, "\n")
    if historyStr == "" {
        historyStr = "(none yet)"
    }

    prompt := strings.NewReplacer(
        "{{.Goal}}", rc.Goal,
        "{{.Roster}}", strings.Join(rosterLines, "\n"),
        "{{.K}}", fmt.Sprint(k),
        "{{.History}}", historyStr,
    ).Replace(s.template)

    const maxRetries = 3
    for attempt := range maxRetries {
        raw, err := s.provider.Complete(ctx, llm.Request{
            Model:  s.model,
            Prompt: prompt,
        })
        if err != nil {
            slog.WarnContext(ctx, "selector LLM call failed", "attempt", attempt, "err", err)
            continue
        }
        var resp struct {
            NextSpeaker string `json:"next_speaker"`
        }
        if err := json.Unmarshal([]byte(raw), &resp); err == nil {
            if a, ok := roleMap[resp.NextSpeaker]; ok {
                return a, nil
            }
        }
    }

    // Fallback: roundrobin
    idx := rc.CurrentTurn % len(rc.Team.Agents)
    return &rc.Team.Agents[idx], nil
}

// --- Swarm ---

type swarmOrchestrator struct{}

func (sw *swarmOrchestrator) NextAgent(ctx context.Context, rc *RunContext) (*TeamAgent, error) {
    roleMap := make(map[string]*TeamAgent, len(rc.Team.Agents))
    for i := range rc.Team.Agents {
        roleMap[rc.Team.Agents[i].Role] = &rc.Team.Agents[i]
    }
    if len(rc.TurnHistory) > 0 {
        last := rc.TurnHistory[len(rc.TurnHistory)-1]
        if last.HandoffTarget != nil {
            if a, ok := roleMap[*last.HandoffTarget]; ok {
                return a, nil
            }
            slog.WarnContext(ctx, "HANDOFF_TO target not in team; falling back to roundrobin",
                "target", *last.HandoffTarget, "team", rc.Team.Name)
        }
    }
    idx := rc.CurrentTurn % len(rc.Team.Agents)
    return &rc.Team.Agents[idx], nil
}

// ExtractHandoffTarget parses a HANDOFF_TO:<role> signal from agent output.
func ExtractHandoffTarget(output string) *string {
    m := handoffPattern.FindStringSubmatch(output)
    if m == nil {
        return nil
    }
    role := strings.TrimSpace(m[1])
    return &role
}
```

### 9.5 Core Run Loop

`RunTeam` is a straight sequential loop. Because turns are sequential (NG1), no goroutine fan-out is needed inside the loop itself. `context.Context` threads cancellation so that `tag team run` can be interrupted cleanly via `os.Signal`. Each turn result is persisted to SQLite via the single-writer connection before the next turn begins (crash-safe per NFR-03).

```go
package swarm

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "strings"
    "time"

    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
)

// RunTeam executes the team goal sequentially, persisting every turn.
// Events are written to w as NDJSON when jsonOutput is true; otherwise
// human-readable lines are written. db is an *internal/store.DB (single-writer).
func RunTeam(ctx context.Context, rc *RunContext, db DB, w io.Writer, jsonOutput bool) error {
    strategy := rc.Team.Strategy
    maxT := rc.Team.MaxTurns
    termPhrase := ""
    if rc.Team.TerminationPhrase != nil {
        termPhrase = strings.ToLower(*rc.Team.TerminationPhrase)
    }

    var orch Orchestrator
    switch strategy {
    case StrategyRoundRobin:
        orch = &roundRobinOrchestrator{}
    case StrategySelector:
        model := defaultSelectorModel(rc.Team)
        tmpl := defaultSelectorPrompt
        if rc.Team.SelectorPromptTemplate != nil {
            tmpl = *rc.Team.SelectorPromptTemplate
        }
        orch = &selectorOrchestrator{model: model, template: tmpl, provider: db.LLMProvider()}
    default:
        orch = &swarmOrchestrator{}
    }

    tracer := otel.Tracer("tag.teams")
    ctx, rootSpan := tracer.Start(ctx, "tag.team.run")
    rootSpan.SetAttributes(
        attribute.String("team.name", rc.Team.Name),
        attribute.String("team.strategy", string(strategy)),
        attribute.String("team.run_id", rc.RunID),
        attribute.String("team.goal", truncateAttr(rc.Goal, 256)),
    )
    defer rootSpan.End()

    emitEvent(w, jsonOutput, "run_start", map[string]any{
        "run_id": rc.RunID, "team": rc.Team.Name,
        "strategy": string(strategy), "goal": rc.Goal,
    })

    for rc.CurrentTurn < maxT && rc.Status == RunStatusRunning {
        if err := ctx.Err(); err != nil {
            rc.Status = RunStatusFailed
            break
        }
        if rc.BudgetUSD != nil && rc.TotalCostUSD >= *rc.BudgetUSD {
            rc.Status = RunStatusBudgetExceeded
            break
        }

        agent, err := orch.NextAgent(ctx, rc)
        if err != nil {
            rc.Status = RunStatusFailed
            return fmt.Errorf("next agent selection: %w", err)
        }
        rc.CurrentTurn++

        inputCtx := buildInputContext(rc, agent)

        emitEvent(w, jsonOutput, "turn_start", map[string]any{
            "turn": rc.CurrentTurn, "role": agent.Role, "profile": agent.Profile,
        })

        _, turnSpan := tracer.Start(ctx, "tag.team.turn")
        turnSpan.SetAttributes(
            attribute.Int("turn.number", rc.CurrentTurn),
            attribute.String("agent.role", agent.Role),
            attribute.String("agent.profile", agent.Profile),
        )

        result := executeAgentTurn(ctx, agent, inputCtx, rc)

        if strategy == StrategySwarm {
            result.HandoffTarget = ExtractHandoffTarget(result.Output)
        }

        turnSpan.SetAttributes(
            attribute.Int("gen_ai.usage.input_tokens", result.PromptTokens),
            attribute.Int("gen_ai.usage.output_tokens", result.CompletionTokens),
            attribute.Float64("gen_ai.usage.cost_usd", result.CostUSD),
        )
        turnSpan.End()

        rc.TurnHistory = append(rc.TurnHistory, result)
        rc.TotalCostUSD += result.CostUSD

        if err := db.PersistTurn(rc, result, inputCtx); err != nil {
            return fmt.Errorf("persist turn %d: %w", rc.CurrentTurn, err)
        }

        emitEvent(w, jsonOutput, "turn_end", map[string]any{
            "turn": rc.CurrentTurn, "role": agent.Role,
            "output":   result.Output,
            "tokens":   result.PromptTokens + result.CompletionTokens,
            "cost_usd": result.CostUSD,
        })

        if termPhrase != "" && strings.Contains(strings.ToLower(result.Output), termPhrase) {
            rc.Status = RunStatusTerminated
            break
        }
    }

    if rc.Status == RunStatusRunning {
        rc.Status = RunStatusCompleted
    }

    totalTokens := 0
    for _, t := range rc.TurnHistory {
        totalTokens += t.PromptTokens + t.CompletionTokens
    }
    _ = db.UpdateRunStatus(rc)
    emitEvent(w, jsonOutput, "run_end", map[string]any{
        "run_id": rc.RunID, "turns": rc.CurrentTurn,
        "total_tokens": totalTokens, "total_cost_usd": rc.TotalCostUSD,
        "status": string(rc.Status),
    })
    return nil
}

func emitEvent(w io.Writer, asJSON bool, event string, fields map[string]any) {
    fields["event"] = event
    fields["timestamp"] = time.Now().UTC().Format(time.RFC3339)
    if asJSON {
        b, _ := json.Marshal(fields)
        fmt.Fprintln(w, string(b))
    } else {
        fmt.Fprintf(w, "[team] %s %v\n", event, fields)
    }
}
```

### 9.6 Input Context Construction

```go
func buildInputContext(rc *RunContext, agent *TeamAgent) string {
    k := rc.Team.ContextTurns
    var historySection string
    if len(rc.TurnHistory) > 0 {
        start := len(rc.TurnHistory) - k
        if start < 0 {
            start = 0
        }
        var lines []string
        for _, t := range rc.TurnHistory[start:] {
            lines = append(lines, fmt.Sprintf("[%s]: %s", t.Role, t.Output))
        }
        historySection = "CONVERSATION_HISTORY:\n" + strings.Join(lines, "\n---\n")
    }

    var kanbanSection string
    if rc.BoardID != nil {
        kanbanSection = fmt.Sprintf(
            "TEAM_KANBAN_BOARD=%s\n"+
                "You can create, update, and close tasks on this shared board. "+
                "Check the board for tasks created by your teammates before starting work.\n",
            *rc.BoardID,
        )
    }

    raw := fmt.Sprintf(
        "TEAM_GOAL: %s\n\nYOUR_ROLE: %s\n\n%s%s\n\n"+
            "It is now your turn as '%s'. Continue working toward the team goal.",
        rc.Goal, agent.Role, kanbanSection, historySection, agent.Role,
    )
    return truncate(raw)
}
```

### 9.7 Database Access Pattern

All database access goes through `internal/store`'s single-writer `DB` type, which owns the `modernc.org/sqlite` connection, WAL journal mode, `PRAGMA busy_timeout = 5000`, and `gofrs/flock`-guarded atomic read-modify-write. The `swarm` package receives a `DB` interface:

```go
// DB is the subset of internal/store.DB used by internal/swarm.
type DB interface {
    PersistTurn(rc *RunContext, result TurnResult, inputCtx string) error
    UpdateRunStatus(rc *RunContext) error
    InsertRun(rc *RunContext) error
    LLMProvider() llm.Provider
}
```

The `internal/store` migration runner applies the DDL from section 9.2 at startup via its standard versioned migration table — no manual `CREATE TABLE` calls inside `internal/swarm`.

### 9.8 Kanban Board Integration

At team run start, when `--no-kanban` is not set, `internal/swarm/kanban.go` delegates to `internal/store`'s kanban board subsystem:

```go
package swarm

import (
    "fmt"
    "time"

    "github.com/tag/internal/store"
)

// provisionTeamBoard creates a dedicated kanban board for this team run.
// The board slug embeds the last 8 hex chars of the run ID for uniqueness.
func provisionTeamBoard(db store.KanbanStore, teamName, runID string) (string, error) {
    slug := fmt.Sprintf("team-%s-%s", teamName, runID[len(runID)-8:])
    return slug, db.EnsureBoard(store.Board{
        ID:        slug,
        Name:      fmt.Sprintf("Team run: %s / %s", teamName, runID),
        CreatedAt: time.Now().UTC(),
    })
}
```

### 9.9 OTel Span Integration

OTel instrumentation in `internal/swarm/otel.go` uses `go.opentelemetry.io/otel` directly. The root span and per-turn child spans are started and ended within `RunTeam` (see section 9.5). Attribute names follow the pinned `gen_ai.*` semconv table from `internal/otel/semconv.go` (per PRD-041). The `team.goal` attribute is truncated to 256 characters before export (per security consideration 10.10 below).

```go
// truncateAttr caps attribute string values before export.
func truncateAttr(s string, max int) string {
    r := []rune(s)
    if len(r) <= max {
        return s
    }
    return string(r[:max])
}
```

### 9.10 CLI Integration

New cobra commands are registered in `internal/cli/team.go`, following the existing `internal/cli` subcommand pattern:

```go
// In internal/cli/root.go — add once:
rootCmd.AddCommand(newTeamCmd())

// internal/cli/team.go sketch:
func newTeamCmd() *cobra.Command {
    cmd := &cobra.Command{Use: "team", Short: "Multi-agent team primitives"}
    cmd.AddCommand(
        newTeamCreateCmd(),
        newTeamRunCmd(),
        newTeamListCmd(),
        newTeamShowCmd(),
        newTeamDeleteCmd(),
        newTeamRunShowCmd(),
    )
    return cmd
}
```

`newTeamRunCmd` is the heaviest handler. It:
1. Validates the team exists (query `teams` table)
2. Inserts the `team_runs` row via `db.InsertRun`
3. Provisions the kanban board (unless `--no-kanban`)
4. Constructs `RunContext`
5. Calls `swarm.RunTeam(ctx, rc, db, os.Stdout, args.JSON)`
6. Prints the final summary line

### 9.11 Agent Turn Execution

`executeAgentTurn` reuses the `internal/agent` package's single-iteration agent loop — the hand-rolled ~200-LOC `continue|compact|stop` state machine with `IterationBudget`, a grace call, and a cooperative interrupt flag (identical structure to the agent loop described in `GO_MIGRATION_RESEARCH.md`):

```go
func executeAgentTurn(ctx context.Context, agent *TeamAgent, inputCtx string, rc *RunContext) TurnResult {
    result, err := agentpkg.RunSingleTurn(ctx, agentpkg.TurnRequest{
        Profile:     agent.Profile,
        UserMessage: inputCtx,
    })
    if err != nil {
        return TurnResult{Role: agent.Role, Profile: agent.Profile, Err: err}
    }
    return TurnResult{
        Role:             agent.Role,
        Profile:          agent.Profile,
        Output:           result.Content,
        PromptTokens:     result.Usage.PromptTokens,
        CompletionTokens: result.Usage.CompletionTokens,
        CostUSD:          result.Usage.CostUSD,
    }
}
```

`agentpkg.RunSingleTurn` dispatches through `internal/llm`'s provider interface, which wraps `anthropics/anthropic-sdk-go` and `openai/openai-go/v3` behind the unified `Stream(ctx, Request) <-chan Event` interface.

---

## 10. Security Considerations

1. **Name injection prevention.** Team names and role names are validated against `^[a-z0-9][a-z0-9\-_]{0,63}$` before any use in SQLite queries or kanban board slug construction. All SQLite access uses parameterized queries via `modernc.org/sqlite`'s `database/sql` driver (never string formatting into SQL). A unit test with adversarial names (SQL injection payloads, path traversal) must pass.

2. **Kanban board isolation.** Each team run gets a unique board slug incorporating the run ID. Cross-team board access is not possible without knowing the exact slug. No board contents are exposed in `tag team list` output.

3. **Selector LLM prompt injection.** The goal text and conversation history injected into the selector prompt are treated as untrusted data. The selector prompt template uses `strings.NewReplacer` placeholders (not `fmt.Sprintf` with arbitrary substitution); goal and history values are truncated to 4 KB before inclusion to limit prompt injection attack surface.

4. **Input context truncation.** `truncate()` enforces an 8 KB hard ceiling on the `input_context` column and on the kanban section included in agent prompts. This prevents a malicious or runaway agent from producing unbounded output that inflates the next agent's context to millions of tokens.

5. **Secret scanning at turn boundaries.** If PRD-034 secret scanning is enabled (`security.SCAN_OUTPUTS=true` in `internal/config`), each turn output is passed through `internal/security`'s scanner before persistence. A detected secret blocks the output from being stored and sets the turn `error` field to `SECRET_DETECTED`.

6. **API key exposure.** `selector_model` is stored in the `teams` table as a model identifier (e.g., `anthropic/claude-haiku-3`), never as an API key. Actual API keys remain in the profile configuration layer in `internal/config` (outside `internal/swarm`).

7. **Budget enforcement as a security boundary.** `--budget-usd` is enforced before each turn, not after. A runaway agent that produces tokens rapidly cannot exceed the budget by more than one turn's cost. This is a defense-in-depth measure against runaway loops (complements PRD-039).

8. **Sandboxed code execution.** When any agent in the team has code execution tools enabled (PRD-028 sandbox), those tools run inside the existing `internal/sandbox` isolation ladder. `internal/swarm` does not weaken or bypass sandbox constraints.

9. **Swarm handoff target validation.** `HANDOFF_TO:<role>` targets are validated against the team's known role set before routing. An agent cannot route to a role outside its team. Unknown targets fall back to roundrobin with a `slog.Warn` to stderr, not a crash.

10. **OTel data minimization.** Span attributes for `team.goal` are truncated to 256 characters before export via `truncateAttr`. Full goal text stays in the local SQLite database only and is not exported via OTLP unless explicitly configured.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/swarm/team_test.go`)

All tests use the standard `testing` package + `github.com/stretchr/testify/assert` + `github.com/stretchr/testify/require`. Mocks for `DB` and `llm.Provider` are defined as simple local interface implementations.

- `TestRoundRobinRotation`: Assert agent at turn N is `agents[N % len(agents)]` for N in range 0..29, across team sizes 1, 2, 3, 5, 10.
- `TestRoundRobinTerminationPhrase`: Assert run halts immediately when termination phrase is found in turn output.
- `TestRoundRobinMaxTurns`: Assert run halts at exactly `max_turns` turns with status=`RunStatusCompleted`.
- `TestSelectorFallbackOnParseError`: Mock `llm.Provider.Complete` returning malformed JSON 3 times; assert fallback to roundrobin for turn 4.
- `TestSelectorValidResponse`: Mock returning `{"next_speaker": "coder", "reason": "..."}` ; assert `coder` agent is selected.
- `TestSwarmHandoffDetected`: Synthetic turn output containing `HANDOFF_TO: reviewer`; assert `ExtractHandoffTarget` returns `"reviewer"`.
- `TestSwarmHandoffCaseInsensitive`: `handoff_to:REVIEWER` and `HANDOFF_TO:Reviewer` both detected.
- `TestSwarmUnknownRoleFallback`: `HANDOFF_TO:nonexistent` triggers roundrobin fallback and emits a `slog.Warn`.
- `TestTruncateInputContext`: Input > 8 KB is truncated to ≤ 8 KB with `[TRUNCATED]` suffix.
- `TestTeamNameValidation`: Names with spaces, dots, path separators, SQL keywords rejected; valid slugs accepted via `regexp.MustCompile`.
- `TestBudgetExceeded`: Team run with `BudgetUSD=0.001` and mocked turns costing $0.0006 each; run terminates after 2nd turn with `RunStatusBudgetExceeded`.
- `TestNoKanbanFlag`: Assert `BoardID` is nil and no `TEAM_KANBAN_BOARD=` appears in `buildInputContext` output.
- `TestDryRunExitsZero`: `newTeamRunCmd` with `--dry-run` returns without invoking `swarm.RunTeam`.
- `TestAgentsJSONRoundtrip`: `AgentsJSON()` → `json.Unmarshal` → `[]TeamAgent` reconstruction is lossless.

### 11.2 Integration Tests (`internal/swarm/team_integration_test.go`)

Integration tests use an in-process `modernc.org/sqlite` database opened at a `t.TempDir()` path. Tests call the real `internal/swarm` and `internal/store` packages; LLM calls are mocked via a `llm.Provider` stub.

- `TestCreateAndShow`: `CreateTeam` followed by `GetTeam`; assert returned definition matches inputs.
- `TestListReturnsCreatedTeam`: After create, `ListTeams` JSON includes the new team.
- `TestDeleteRemovesDefinitionKeepsRuns`: Create team, run it once (mocked agents), delete team; assert `team_runs` row still present.
- `TestRunPersistsAllTurns`: 3-agent roundrobin run with mocked agents, 6 turns; assert 6 rows in `team_turns`, correct agent order.
- `TestKanbanBoardProvisioned`: Run with kanban enabled; assert board slug row exists in `kanban_boards`.
- `TestOtelSpansEmitted`: Capture OTel spans from a mocked team run via `go.opentelemetry.io/otel/sdk/trace`'s `tracetest.NewInMemoryExporter`; assert one root span and N child spans with correct attributes.
- `TestJSONOutputValidNDJSON`: Capture stdout from `RunTeam` with `jsonOutput=true`; assert every line is parseable via `json.Unmarshal` with expected `event` fields.
- `TestSelectorModelOverride`: Create team with `--selector-model anthropic/claude-haiku-3`; assert `SelectorModel` stored correctly.
- `TestDuplicateNameRejected`: Second `CreateTeam` with same name (no upsert) returns a non-nil error containing "already exists".
- `TestConcurrentReadsDuringRun`: Start team run in a goroutine, concurrently call `ListTeams` 50 times via `errgroup`; assert no SQLite errors and all reads return valid results.

### 11.3 Performance Benchmark (`internal/swarm/bench_test.go`)

```go
func BenchmarkOrchestrationOverhead(b *testing.B) {
    // Mock executeAgentTurn to return instantly; measure
    // NextAgent() + buildInputContext() + db.PersistTurn() cycle.
    const nAgents = 20
    b.ResetTimer()
    for range b.N {
        // one full turn cycle
    }
    // Separately verify P99 < 50ms via benchstat across runs.
}
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
| AC-07 | Selector strategy selects `coder` when the mocked `llm.Provider` returns `{"next_speaker": "coder", "reason": "needs coding"}`. | Unit test |
| AC-08 | Selector strategy falls back to roundrobin after 3 consecutive LLM parse failures; no panic or error is returned. | Unit test |
| AC-09 | Swarm strategy routes to `reviewer` when coder output contains `HANDOFF_TO: reviewer`. | Unit test |
| AC-10 | Swarm strategy logs a `slog.Warn` and falls back to roundrobin when `HANDOFF_TO:unknown` is detected. | Unit test capturing `slog` output |
| AC-11 | A kanban board named `team-<team_name>-<run_id_suffix>` exists in `kanban_boards` after a team run without `--no-kanban`. | Integration test |
| AC-12 | `--no-kanban` flag results in no board row created and no `TEAM_KANBAN_BOARD=` in any `input_context` column. | Integration test |
| AC-13 | `tag team list --json` output is valid JSON (parseable by `encoding/json`); contains the team created in AC-01. | Integration test |
| AC-14 | `tag team show "crew" --json` output contains `agents` array with correct `role` and `profile` entries. | Integration test |
| AC-15 | `tag team delete "crew"` without `--yes` prompts for confirmation and does not delete on 'N' input. | Integration test with mocked `os.Stdin` |
| AC-16 | `tag team delete "crew" --yes` removes the `teams` row; previously created `team_runs` rows remain. | Integration test |
| AC-17 | Each turn in `team_turns` has `prompt_tokens > 0`, `completion_tokens > 0`, and `cost_usd > 0.0` (using mocked LLM returning realistic usage). | Integration test |
| AC-18 | `--budget-usd 0.001` with mocked turns costing $0.0006 each aborts after turn 2 with status=`budget_exceeded`. | Unit test |
| AC-19 | OTel root span `tag.team.run` contains attributes `team.name`, `team.strategy`, `team.run_id`, `team.goal` (truncated to 256 chars). | Span capture test via `tracetest.NewInMemoryExporter` |
| AC-20 | Each OTel child span `tag.team.turn` contains `turn.number`, `agent.role`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.cost_usd`. | Span capture test |
| AC-21 | `--dry-run` prints the execution plan and exits 0 without inserting any rows into `team_runs` or `team_turns`. | Integration test + SQL row count assertion |
| AC-22 | Input context exceeding 8 KB is stored as truncated text ending in `[TRUNCATED]`. | Unit test |
| AC-23 | `tag team run show <run_id>` returns all turns for that run in turn_number order. | Integration test |
| AC-24 | Second `tag team create "crew"` (same name, no `--upsert`) exits non-zero with error containing "already exists". | Integration test |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| `internal/store` (PRD-004 kanban) | Internal | Board provisioning via `KanbanStore.EnsureBoard`; `kanban_boards` table owned by `internal/store` |
| `internal/agent` (PRD-021) | Internal | `RunSingleTurn()` — single-iteration agent loop (hand-rolled ~200-LOC continue\|compact\|stop state machine with IterationBudget, grace call, interrupt flag) |
| `internal/llm` | Internal | `Provider` interface wrapping `anthropics/anthropic-sdk-go` + `openai/openai-go/v3`; `Stream(ctx, Request) <-chan Event` |
| `internal/config` (PRD-013 tracing) | Internal | OTel tracer acquisition; profile config lookup |
| `internal/security` (PRD-034) | Internal | Optional secret scanning of turn outputs |
| `internal/otel/semconv.go` (PRD-041) | Internal | Pinned `gen_ai.*` span attribute name constants |
| `internal/cli` | Internal | cobra command registration for `tag team` subcommand tree |
| `modernc.org/sqlite` | Third-party | Pure-Go SQLite, `CGO_ENABLED=0`, WAL mode, FTS5 built-in; single-writer via `internal/store` |
| `github.com/gofrs/flock` | Third-party | Cross-platform file locking for atomic config/DB RMW (via `internal/store`) |
| `go.opentelemetry.io/otel` | Third-party | OTel tracing; already present from PRD-013 |
| `github.com/stretchr/testify` | Third-party (test) | `assert` / `require` for unit and integration tests |
| `golang.org/x/sync/errgroup` | stdlib-ext | Used in concurrent-reads integration test and any future parallel sub-tasks |
| `encoding/json` | stdlib | Agent JSON serialization, selector response parsing |
| `crypto/rand` | stdlib | Run ID and team ID generation |
| `regexp` | stdlib | Handoff signal pattern matching |
| `context` | stdlib | Cancellation propagation across all turn calls |
| `log/slog` | stdlib | Structured warning/error logging to stderr |

No external multi-agent framework dependencies are introduced. The OTel exporter dependency is already present from PRD-013.

---

## 14. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|-------------------|
| OQ-1 | Should the shared kanban board be auto-deleted after team run completion, or retained indefinitely? Retention is useful for post-run analysis but accumulates boards over time. Proposal: retain by default, add `--delete-board` flag. | Team | Phase 2 design |
| OQ-2 | The Selector strategy makes an extra LLM call per turn, adding cost and latency. Should there be a token cap per selector call (e.g., max 500 completion tokens via `internal/llm` `Request.MaxTokens`)? | Team | Phase 1 implementation |
| OQ-3 | For Swarm strategy, should `HANDOFF_TO:<role>` be stripped from the output before it is passed to the next agent's history? Including it could confuse downstream agents; removing it loses traceability. Proposal: strip from `buildInputContext` history but retain in `team_turns.output`. | Team | Phase 1 implementation |
| OQ-4 | Should teams support a `--max-turns-per-agent <N>` guard to prevent any single agent from dominating a Swarm run (e.g., cascading self-handoffs)? | Team | Phase 2 |
| OQ-5 | The context window for the conversation history (`ContextTurns`, default 5) may be insufficient for complex goals requiring deep context. Should this be per-agent (different roles need different amounts of history) or global? | Team | Phase 2 |
| OQ-6 | How should team runs interact with `tag eval` (PRD-027)? A team run could be an eval target (evaluate the team's collective output), but the eval framework today targets single profiles. Should `tag eval run --team <name>` be added? | Team | Future PRD |
| OQ-7 | Should team definitions be exportable/importable as YAML (analogous to profile YAML export via `gopkg.in/yaml.v3`)? This would enable sharing team configurations across TAG installations and in source control. | Team | Phase 2 |
| OQ-8 | The selector LLM call goes through `internal/agent.RunSingleTurn` with a synthetic profile, or directly through `internal/llm.Provider.Complete`? The former is cleaner but creates a dependency on a profile existing in config; the latter is lighter but bypasses profile-level budget guards. | Team | Phase 1 implementation |

---

## 15. Complexity and Timeline

### Phase 1 — Core (Days 1–5)

**Day 1–2: Schema, structs, and basic CRUD**
- Write `internal/swarm/team.go` with `TeamDefinition`, `TurnResult`, `RunContext` structs
- Register DDL migration in `internal/store` migration runner
- Implement `CreateTeam`, `GetTeam`, `ListTeams`, `DeleteTeam` in `internal/store`
- Wire cobra subcommands `team create|list|show|delete` in `internal/cli/team.go`
- Unit tests: name validation regex, CRUD round-trips, `--json` output marshaling

**Day 3–4: RoundRobin strategy + run loop**
- Implement `roundRobinOrchestrator` and `RunTeam` in `internal/swarm/run.go`
- Implement `buildInputContext` with `truncate`
- Implement `db.PersistTurn` and `db.UpdateRunStatus` in `internal/store`
- Integration test: 3-agent, 6-turn run; assert 6 rows in `team_turns` with correct agent order

**Day 5: Kanban integration + OTel spans**
- Implement `provisionTeamBoard` in `internal/swarm/kanban.go`
- Inject board context into `buildInputContext`
- Wire OTel root and child spans in `internal/swarm/otel.go`
- Unit test: kanban board provisioned; spans captured via `tracetest.NewInMemoryExporter`

### Phase 2 — Selector + Swarm (Days 6–8)

**Day 6: Selector strategy**
- Implement `selectorOrchestrator` with default prompt template and `strings.NewReplacer`
- Wire selector call through `internal/llm.Provider.Complete`
- Implement parse-failure fallback to roundrobin (3 retries)
- Unit tests: valid response routing, parse failure fallback, custom prompt template

**Day 7: Swarm strategy**
- Implement `swarmOrchestrator` and `ExtractHandoffTarget` with `regexp`
- Handle unknown role fallback with `slog.Warn`; resolve OQ-3 (strip from history, keep in DB)
- Unit tests: all handoff signal variants, unknown role warning, case insensitivity

**Day 8: Budget enforcement + dry-run + run-show**
- Implement `--budget-usd` pre-turn check in `RunTeam`
- Implement `--dry-run` plan output in `newTeamRunCmd`
- Implement `newTeamRunShowCmd` with `--turn` filter
- Integration tests: budget exceeded status, dry-run produces zero DB writes

### Phase 3 — Hardening + Performance (Days 9–10)

**Day 9: Security hardening + edge cases**
- Add adversarial name validation tests (SQL injection, path traversal)
- Add `internal/security` integration at turn boundary (PRD-034)
- Add concurrent-reads test (50 goroutines via `errgroup` calling `ListTeams` during a run)
- Add input > 8 KB truncation test

**Day 10: Performance benchmark + documentation**
- Run `BenchmarkOrchestrationOverhead`; assert P99 < 50 ms with 20-agent team via `benchstat`
- Add `tag team` to cobra `--help` output
- Update `docs/prd/INDEX.md`
- Final CI pass: `go test -cover ./internal/swarm/... ≥ 85%`, all acceptance criteria green

### Total: 10 business days (~2 weeks)

Consistent with **M (1-2 weeks)** effort estimate. The complexity is rated 3/5: the orchestration logic itself is straightforward Go, but the integration surface (`modernc.org/sqlite` WAL, `internal/store` kanban, OTel, `internal/agent` agent loop, cobra CLI) is broad and requires careful testing of each boundary. Go's sequential-turn model maps naturally and without loss onto this: asyncio/multiprocessing fan-out patterns from the Python design become goroutine + `context.Context` cancellation — simpler and safer.

---

---

## Enhancement: Trinity-Style Dynamic Role Assignment

**Added:** v0.7.2 planning cycle — inspired by Sakana AI Trinity (ICLR 2026, arXiv:2604.xxxxx) and Conductor (ICLR 2026).

### Background

The initial PRD defines team orchestration with static roles: each profile is assigned one role (researcher, coder, reviewer) and keeps it for the entire run. Sakana AI's **Trinity** paper demonstrates that a compact evolved coordinator — under 20K parameters — dynamically assigns **Thinker**, **Worker**, and **Verifier** roles to a pool of LLMs *turn-by-turn*, not run-by-run. Their companion **Conductor** paper trains a 7B RL model to write *different specialist instructions* for each worker model per turn. Together they achieve 83.9% on LiveCodeBench and 87.5% on GPQA-Diamond by continuously re-assigning roles based on the current conversational state.

TAG cannot train a Conductor — that requires RL fine-tuning on millions of tokens. But the *role rotation* pattern is implementable at the software layer using TAG's existing profile system and the `internal/swarm` orchestration runtime.

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

**New strategy in `internal/swarm/strategy_trinity.go`:**

```go
package swarm

import (
    "context"
    "encoding/json"
    "fmt"
    "log/slog"

    "github.com/tag/internal/llm"
)

// TrinityRole is the per-turn role assigned to a team member.
type TrinityRole string

const (
    TrinityRoleThinker  TrinityRole = "thinker"
    TrinityRoleWorker   TrinityRole = "worker"
    TrinityRoleVerifier TrinityRole = "verifier"
    TrinityRoleIdle     TrinityRole = "idle"
)

// TrinityAssignment is the coordinator's per-turn role allocation.
type TrinityAssignment struct {
    Turn             int                    `json:"turn"`
    Assignments      map[string]TrinityRole `json:"assignments"`
    Rationale        string                 `json:"rationale"`
    NextSpeaker      string                 `json:"next_speaker"`
    TurnInstructions map[string]string      `json:"turn_instructions"`
}

type trinityOrchestrator struct {
    coordinatorProfile string
    provider           llm.Provider
}

// assignRoles calls the coordinator profile to get per-turn role assignments.
func (t *trinityOrchestrator) assignRoles(ctx context.Context, rc *RunContext) (*TrinityAssignment, error) {
    prompt := buildTrinityPrompt(rc)
    raw, err := t.provider.Complete(ctx, llm.Request{
        Profile: t.coordinatorProfile,
        Prompt:  prompt,
    })
    if err != nil {
        slog.WarnContext(ctx, "trinity coordinator call failed; using fallback", "err", err)
        return fallbackTrinityAssignment(rc), nil
    }
    var a TrinityAssignment
    if err := json.Unmarshal([]byte(raw), &a); err != nil {
        slog.WarnContext(ctx, "trinity coordinator JSON malformed; using fallback", "err", err)
        return fallbackTrinityAssignment(rc), nil
    }
    return &a, nil
}

// NextAgent implements Orchestrator; for Trinity the caller uses RunTrinityTurn instead.
func (t *trinityOrchestrator) NextAgent(ctx context.Context, rc *RunContext) (*TeamAgent, error) {
    a, err := t.assignRoles(ctx, rc)
    if err != nil {
        return nil, err
    }
    roleMap := make(map[string]*TeamAgent, len(rc.Team.Agents))
    for i := range rc.Team.Agents {
        roleMap[rc.Team.Agents[i].Role] = &rc.Team.Agents[i]
    }
    if a.NextSpeaker != "" {
        if ag, ok := roleMap[a.NextSpeaker]; ok {
            return ag, nil
        }
    }
    // fallback
    idx := rc.CurrentTurn % len(rc.Team.Agents)
    return &rc.Team.Agents[idx], nil
}

// RunTrinityTurn executes a full Thinker→Worker→Verifier sub-turn sequence.
// Returns true if the verifier approved (APPROVED in output).
func RunTrinityTurn(ctx context.Context, t *trinityOrchestrator, rc *RunContext, db DB) (bool, error) {
    assignment, err := t.assignRoles(ctx, rc)
    if err != nil {
        return false, err
    }

    roleMap := make(map[string]*TeamAgent, len(rc.Team.Agents))
    for i := range rc.Team.Agents {
        roleMap[rc.Team.Agents[i].Role] = &rc.Team.Agents[i]
    }

    runSubTurn := func(profile, trinityRole, instruction string) (TurnResult, error) {
        ag, ok := roleMap[profile]
        if !ok {
            return TurnResult{}, fmt.Errorf("profile %q not in team", profile)
        }
        ctx := fmt.Sprintf("TRINITY_ROLE: %s\nINSTRUCTION: %s\n\n%s",
            trinityRole, instruction, buildInputContext(rc, ag))
        result := executeAgentTurn(ctx, ag, ctx, rc)  // illustrative; real sig passes context.Context
        rc.TurnHistory = append(rc.TurnHistory, result)
        rc.TotalCostUSD += result.CostUSD
        _ = db.PersistTurn(rc, result, ctx)
        return result, nil
    }

    approved := false
    for profile, role := range assignment.Assignments {
        if role == TrinityRoleIdle {
            continue
        }
        instr := assignment.TurnInstructions[profile]
        result, err := runSubTurn(profile, string(role), instr)
        if err != nil {
            return false, err
        }
        if role == TrinityRoleVerifier && containsFold(result.Output, "APPROVED") {
            approved = true
        }
    }
    return approved, nil
}

func fallbackTrinityAssignment(rc *RunContext) *TrinityAssignment {
    a := &TrinityAssignment{
        Turn:             rc.CurrentTurn,
        Assignments:      make(map[string]TrinityRole, len(rc.Team.Agents)),
        TurnInstructions: make(map[string]string),
    }
    roles := []TrinityRole{TrinityRoleThinker, TrinityRoleWorker, TrinityRoleVerifier}
    for i, ag := range rc.Team.Agents {
        a.Assignments[ag.Role] = roles[i%len(roles)]
    }
    return a
}
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
| `TestTrinityRoleAssignmentParses` | Coordinator JSON parsed correctly; thinker/worker/verifier extracted into `TrinityAssignment` |
| `TestTrinityRoleRotation` | Role assignments differ between turns (coordinator actually rotates) |
| `TestTrinityIdleSkips` | Member with role=`idle` not called for that turn |
| `TestTrinityVerifierStops` | "APPROVED" in verifier output sets `approved=true` from `RunTrinityTurn` |
| `TestTrinityFallbackAssignment` | Malformed coordinator JSON → `fallbackTrinityAssignment` round-robin roles |
| `TestTrinityTurnInstructions` | Each active member's prompt contains its `TurnInstructions` entry |
| `TestTrinityMaxTurnsTerminates` | Loop stops at `MaxTurns` even without APPROVED |

*GitHub issue: #347*
