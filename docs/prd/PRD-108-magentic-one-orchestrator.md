# PRD-108: MagenticOne Dual-Ledger Orchestrator (`tag orchestrate --mode magentic-one`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** L (8-13 days)
**Category:** Reasoning
**Affects:** `internal/swarm (coordinator + wave runner + ContextBus) + internal/agent + internal/runtime + internal/cli`
**Depends on:** PRD-082 (multi-agent team primitives), PRD-105 (TDAG dependency-first task decomposition), PRD-111 (dynamic fan-out/map-reduce), PRD-113 (time-travel debugging)
**Inspired by:** Microsoft MagenticOne, AutoGen 0.4 orchestrator, LangGraph StateGraph, CrewAI hierarchical process

---

## 1. Overview

TAG's current multi-agent orchestration (PRD-082) uses a flat team model where all agents are peer workers and the orchestrator dispatches tasks round-robin or by capability match. This works for simple parallel workflows but lacks the adaptive replanning, stall detection, and progress tracking of production orchestrators like Microsoft's MagenticOne — which was shown to achieve top-1 performance on GAIA, WebArena, and AssistantBench benchmarks.

MagenticOne's key insight is a **dual-ledger architecture**: an **Orchestrator Ledger** tracks the overall plan and progress toward the goal, while a **Task Ledger** tracks per-step context and artifacts. The orchestrator replans when a sub-agent stalls (no progress in N steps), detects loops, and can reassign or retry failed steps with a different agent. Critically, the orchestrator itself uses an LLM to reason about task progress — it is not a static scheduler.

This PRD introduces `tag orchestrate --mode magentic-one`: a MagenticOne-inspired orchestration engine that maintains both ledgers in SQLite, uses an LLM-driven planner to generate and update the task plan, detects agent stalls via configurable progress metrics, and supports replanning with pruned context to avoid context-window overflow. The implementation integrates with the existing PRD-082 team primitives and PRD-105 TDAG task decomposer.

---

## 2. Problem Statement

### 2.1 Flat orchestration fails on complex multi-step tasks

Simple task dispatching (PRD-082) provides no mechanism for replanning when a sub-agent returns an incorrect result or gets stuck. The orchestrator dispatches, waits, and moves on — there is no feedback loop to detect when the plan needs revision.

### 2.2 No stall detection or loop prevention

Without progress tracking, an agent stuck in a retry loop or producing identical outputs on every step continues consuming tokens indefinitely. Production multi-agent systems need stall detection and graceful escalation.

### 2.3 Context window overflow on long tasks

Passing the full task history to the orchestrator LLM on every planning step causes context overflow on tasks with more than 20 steps. MagenticOne's approach of maintaining a structured ledger (rather than full conversation history) is the production solution.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Implement the MagenticOne dual-ledger pattern: `OrchestratorLedger` (plan + progress) and `TaskLedger` (step artifacts) persisted in SQLite. |
| G2 | LLM-driven orchestrator: call the planning model at each step to decide the next action (assign to agent, replan, complete, or abort). |
| G3 | Stall detection: after N consecutive steps with no measurable progress on the current subtask, trigger a replan. |
| G4 | Loop detection: detect when the task ledger contains identical outputs from consecutive steps; inject a "try a different approach" prompt. |
| G5 | Context compression: provide the orchestrator LLM only the current ledger state (not full conversation history). |
| G6 | Integration with PRD-082 team agent registry for agent selection by capability. |
| G7 | `tag orchestrate --mode magentic-one --goal GOAL` launches an orchestration session with the given goal. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Replicating MagenticOne's exact WebArena evaluation harness. |
| NG2 | Multi-machine distributed orchestration. |
| NG3 | Automatic agent spawning (agents must be pre-registered in the team). |
| NG4 | GUI visualization of the dual ledger. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Stall detection accuracy | Detects 95%+ of simulated stall scenarios (no progress in N steps) in unit tests | Unit test |
| Context window efficiency | Orchestrator prompt < 4096 tokens per step regardless of task length | Token count assertion |
| Task completion rate | ≥ 80% task completion rate on 20-task internal benchmark vs 60% without replanning | Eval benchmark |
| SQLite ledger overhead | Ledger writes add < 10ms per step | Benchmark test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Run a complex multi-step goal with automatic replanning | The agent system recovers from failures without manual intervention |
| US2 | Platform engineer | See the orchestrator ledger to understand task progress | I can debug where a long-running orchestration got stuck |
| US3 | ML engineer | Configure stall detection sensitivity | I tune the replanning aggressiveness for my use case |

---

## 6. CLI Surface

```
tag orchestrate --mode magentic-one \
  --goal "Research and summarize the latest papers on LLM reasoning" \
  --profile default \
  --team research-team \
  [--max-steps 50] \
  [--stall-after 3] \
  [--model claude-sonnet-4-6] \
  [--verbose]

tag orchestrate ledger show <session-id>
tag orchestrate ledger history <session-id>

Options:
  --mode magentic-one|flat|hierarchical  Orchestration mode
  --goal TEXT                            Natural language goal
  --team TEAM_NAME                       Pre-registered team to use
  --max-steps N                          Max orchestration steps (default: 50)
  --stall-after N                        Steps without progress before replan (default: 3)
  --model MODEL                          Orchestrator LLM (default: claude-sonnet-4-6)
  --verbose                              Print ledger state after each step
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | On `tag orchestrate --mode magentic-one`, create an `OrchestratorLedger` row and an initial `TaskLedger` row in SQLite. |
| FR-02 | Each orchestration step: call planning model with current ledger summary; parse response to extract next action (assign/replan/complete/abort). |
| FR-03 | Dispatch assigned action to the designated team agent (PRD-082); wait for result; update TaskLedger with step output. |
| FR-04 | Progress tracking: compare current step output hash against previous N step output hashes; if all identical, increment stall counter. |
| FR-05 | On stall counter ≥ `--stall-after`: call replanning model with explicit "the previous N steps made no progress" context; reset stall counter. |
| FR-06 | Loop detection: if the same subtask has been assigned 3+ times with identical inputs, inject diversity prompt. |
| FR-07 | Context compression: only pass the last 3 step summaries + current ledger state to the orchestrator model; not the full step history. |
| FR-08 | On `complete` action: write final answer to OrchestratorLedger, set status to `completed`, and return to CLI. |
| FR-09 | On `abort` action or `--max-steps` exceeded: set status to `aborted`, write reason to ledger. |
| FR-10 | `tag orchestrate ledger show` renders the current orchestrator ledger state and last 10 task ledger entries. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Orchestrator LLM call must include a structured output format (JSON) to ensure reliable action parsing. |
| NFR-02 | All ledger state persisted after each step so progress survives TAG process crash. |
| NFR-03 | Maximum orchestrator prompt tokens per step: 4096 (enforced by ledger summarization). |
| NFR-04 | Support `--dry-run` mode that prints what each step would do without calling agents or LLMs. |

---

## 9. Technical Design

### 9.1 Orchestration model — goroutines over `internal/runtime`, not a multi-agent framework

There is no AutoGen/MagenticOne/LangGraph framework in the Go build; TAG owns the orchestration outright (per `GO_MIGRATION_RESEARCH.md`: "prefer hand-rolled … no eino/langchaingo"). The MagenticOne "lead orchestrator + specialist workers" pattern maps onto the existing `internal/swarm` design:

- **Coordinator goroutine** (`internal/swarm`): plays the MagenticOne orchestrator. It owns both ledgers, runs the LLM-driven planning step each iteration, dispatches the chosen action, and applies stall/loop/completion logic. It is the single writer of ledger state (all persistence flows through `internal/store`).
- **Worker agents = goroutines** running the hand-rolled bounded agent loop in `internal/agent` (`continue|compact|stop` + doom-loop(3) + `IterationBudget` + grace call + cooperative interrupt), each turn driven by the `internal/llm` `Stream(ctx, Request) -> <-chan Event` interface. A worker is selected from the PRD-082 team registry by capability.
- **Channels + write-once `ContextBus`**: the coordinator hands a subtask to a worker over a request channel and receives the result over a reply channel; shared artifacts are published to the swarm's write-once `ContextBus` (a keyed, immutable-once-written store) so a step's output is visible to later waves without mutation races. The wave runner bounds concurrency (`errgroup`) when a plan fans out multiple independent subtasks.
- **Cancellation**: the whole session runs under a `context.Context`; `--max-steps`, abort, and Ctrl-C all resolve to `ctx` cancellation that unwinds every worker goroutine cleanly.

```
                       ┌───────────────────────────┐
   goal ──▶ tag orchestrate (internal/cli)          │
                       │  coordinator goroutine      │  ← plans via internal/llm
                       │  (internal/swarm)           │     structured-output turn
                       └──────┬─────────────┬────────┘
                     subtask  │             │ result
                       chan   ▼             ▲ chan
                   ┌──────────────┐   ┌──────────────┐
                   │ worker agent │   │ worker agent │  ← goroutines running the
                   │ internal/    │   │ internal/    │    internal/agent bounded
                   │ agent loop   │   │ agent loop   │    loop over internal/llm
                   └──────┬───────┘   └──────┬───────┘
                          ▼ publish          ▼ publish
                   ┌───────────────────────────────────┐
                   │ write-once ContextBus (internal/   │
                   │ swarm) — artifacts by stable key   │
                   └───────────────────────────────────┘
        both ledgers persisted after every step ▶ internal/store (modernc.org/sqlite)
```

### 9.2 SQLite DDL (`internal/store`, modernc.org/sqlite)

State persists to the single pure-Go `modernc.org/sqlite` store (CGO_ENABLED=0, WAL). All ledger writes go through the single-writer contract in `internal/store` (flock + atomic RMW); the coordinator never opens the DB file from a worker goroutine. Migrations live in `internal/store/migrate/`.

```sql
CREATE TABLE IF NOT EXISTS orchestrator_ledgers (
  id            TEXT PRIMARY KEY,
  goal          TEXT NOT NULL,
  team          TEXT,
  profile       TEXT,
  mode          TEXT NOT NULL DEFAULT 'magentic-one',
  status        TEXT NOT NULL DEFAULT 'running',
  step_count    INTEGER NOT NULL DEFAULT 0,
  stall_count   INTEGER NOT NULL DEFAULT 0,
  final_answer  TEXT,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_ledger_entries (
  id              TEXT PRIMARY KEY,
  orchestrator_id TEXT NOT NULL REFERENCES orchestrator_ledgers(id),
  step_num        INTEGER NOT NULL,
  action          TEXT NOT NULL,
  agent           TEXT,
  input_summary   TEXT,
  output_summary  TEXT,
  output_hash     TEXT,
  status          TEXT NOT NULL DEFAULT 'pending',
  created_at      TEXT NOT NULL
);
```

### 9.3 Core Go types (`internal/swarm`)

Ledgers are plain Go structs (no dataclasses/pydantic); the orchestrator's decision schema is generated with `invopop/jsonschema` and enforced as structured output on the planning turn.

```go
type OrchestratorLedger struct {
    ID          string
    Goal        string
    Team        string
    Profile     string
    Mode        string // "magentic-one"
    Status      string // "running" | "completed" | "aborted"
    StepCount   int
    StallCount  int
    FinalAnswer string
    CreatedAt   time.Time
    UpdatedAt   time.Time
}

type TaskLedgerEntry struct {
    ID             string
    OrchestratorID string
    StepNum        int
    Action         string // "assign" | "replan" | "complete" | "abort"
    Agent          string
    InputSummary   string
    OutputSummary  string
    OutputHash     string // sha-256 of normalized output, for stall/loop detection
    Status         string
    CreatedAt      time.Time
}

// Decision is the structured output the planner must return each step.
// A JSON Schema derived from this struct (invopop/jsonschema) is attached to
// the internal/llm Request so the model emits a parseable action.
type Decision struct {
    Action      string `json:"action" jsonschema:"enum=assign,enum=replan,enum=complete,enum=abort"`
    Agent       string `json:"agent,omitempty"`
    Task        string `json:"task,omitempty"`
    Reason      string `json:"reason,omitempty"`
    FinalAnswer string `json:"final_answer,omitempty"`
}
```

### 9.4 Coordinator loop (sketch)

```go
func (c *Coordinator) Run(ctx context.Context, goal string) (string, error) {
    led := c.store.NewLedger(ctx, goal, c.team, c.profile)
    for led.StepCount < c.maxSteps {
        select {
        case <-ctx.Done():
            return "", c.store.Abort(ctx, led, "cancelled")
        default:
        }
        dec, err := c.plan(ctx, led)              // internal/llm structured-output turn
        if err != nil { return "", err }
        switch dec.Action {
        case "complete":
            return dec.FinalAnswer, c.store.Complete(ctx, led, dec.FinalAnswer)
        case "abort":
            return "", c.store.Abort(ctx, led, dec.Reason)
        case "replan":
            c.store.ResetStall(ctx, led)
            continue
        case "assign":
            out := c.dispatch(ctx, dec.Agent, dec.Task)   // worker goroutine
            c.trackProgress(ctx, led, dec, out)           // hash compare -> StallCount
        }
    }
    return "", c.store.Abort(ctx, led, "max-steps exceeded")
}
```

- `plan` builds the prompt (§9.5), calls `internal/llm` with the `Decision` JSON Schema, and drains the event stream to a single accumulated `Decision`.
- `dispatch` selects the worker from the PRD-082 registry, sends the subtask over its channel, and reads the reply; the worker runs the `internal/agent` loop and publishes artifacts to the `ContextBus`.
- `trackProgress` computes a SHA-256 over the normalized output, compares it against the previous `--stall-after` hashes, and increments `StallCount`; identical repeated subtask inputs trigger the diversity prompt (FR-06).

### 9.5 Orchestrator prompt template

A Go text template (`text/template`), not a Python f-string. Agent outputs are truncated/sanitized before inclusion (see §10).

```go
const orchestratorSystem = `You are an orchestrator managing a multi-agent team.
Current goal: {{.Goal}}
Team agents: {{.Agents}}
Progress so far: {{.LedgerSummary}}
Last 3 steps: {{.RecentSteps}}

Respond with JSON matching the provided schema:
{"action": "assign"|"replan"|"complete"|"abort",
 "agent": "<name>", "task": "<description>", "reason": "<why>",
 "final_answer": "<answer if complete>"}`
```

Only the current ledger summary + last 3 step summaries are rendered (FR-07), keeping the prompt under the NFR-03 budget regardless of task length.

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Prompt injection via agent output into orchestrator context | Truncate and sanitize agent outputs before including in ledger summaries |
| Runaway orchestration consuming unlimited tokens | `--max-steps` hard limit; cost tracking per session |

---

## 11. Testing Strategy

Go `testing` (table-driven), `testing.B` for benchmarks. Workers are faked by injecting a stub `internal/llm` provider (`Stream` returning canned events) and a fake team registry, so no network calls in unit/integration tests. Token budgets estimated with `tiktoken-go` (OpenAI-family) / `len/4` heuristic (Anthropic).

| Layer | Tests |
|-------|-------|
| Unit | Table-driven stall detection; loop-detection SHA-256 hash comparison; context-compression token count; `Decision` JSON-Schema round-trip parse (invopop/jsonschema) |
| Integration | 5-step orchestration with a stub provider + fake workers over channels; assert ledger rows in an in-memory/tmp `modernc.org/sqlite` DB after each step; assert `ContextBus` write-once semantics |
| Concurrency | `go test -race` over a fan-out wave (multiple worker goroutines) to prove no data race on the ledger or ContextBus; verify `ctx` cancellation unwinds all workers |
| Resilience | Kill the coordinator mid-step; reopen the store and verify ledger recovery (state persisted after every step) |
| Benchmark | `testing.B` on ledger write latency (NFR: < 10ms/step) and per-step prompt-token count |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag orchestrate --mode magentic-one --goal "test" --team test-team` creates a ledger and completes within `--max-steps` |
| AC-02 | Stall detection fires and replanning occurs when 3 consecutive steps return identical output |
| AC-03 | `tag orchestrate ledger show <id>` renders step history with action, agent, and output summary |
| AC-04 | Context compression: orchestrator prompt never exceeds 4096 tokens regardless of step count |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-082 multi-agent team primitives (`internal/swarm` team registry) | Worker-agent selection and goroutine dispatch |
| PRD-105 TDAG decomposer | Initial task decomposition |
| `internal/agent` bounded loop | Worker turn execution (continue\|compact\|stop + doom-loop + IterationBudget + interrupt) |
| `internal/llm` provider interface (`anthropics/anthropic-sdk-go`, `openai/openai-go/v3`) | Orchestrator planning turn + worker turns via `Stream(ctx,Request)->events` |
| `internal/store` (`modernc.org/sqlite`) | Dual-ledger persistence, single-writer contract, WAL |
| `invopop/jsonschema` | Structured-output schema for the `Decision` action |
| Claude `claude-sonnet-4-6` | Default orchestrator planning model |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should the orchestrator model be configurable per step (e.g., Haiku for simple dispatch, Sonnet for replan)? |
| OQ-02 | Should ledger entries be exportable to LangSmith or W&B for evaluation? |

---

## 15. Complexity & Timeline

**Complexity:** Large (L)
**Estimated effort:** 8–13 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `internal/store` DDL + migration, `OrchestratorLedger`/`TaskLedgerEntry` structs, ledger CRUD (single-writer) | 2 |
| 2 | Coordinator goroutine: planner turn over `internal/llm` with `Decision` JSON Schema, worker dispatch over channels + `ContextBus` | 3 |
| 3 | Stall detection (SHA-256 hash compare), loop detection, context compression, `ctx` cancellation | 2 |
| 4 | `internal/cli` integration, `ledger show/history` commands | 2 |
| 5 | Table-driven + `-race` + integration tests, `testing.B` benchmarks, documentation | 2 |

