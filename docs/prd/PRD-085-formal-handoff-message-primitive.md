# PRD-085: Formal HandoffMessage Primitive for Decentralized Agent Routing (`tag handoff`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `internal/agent (handoff dispatcher) + cmd/tag (handoff command)`
**Depends on:** PRD-004 (kanban swarm topology), PRD-008 (background task queue), PRD-013 (agent tracing/observability), PRD-023 (multi-agent swarm), PRD-028 (sandbox execution), PRD-033 (DAG dependency-aware queue), PRD-034 (security/secret scanning), PRD-037 (agent personas), PRD-044 (AgentOps session observability)
**Inspired by:** OpenAI Agents SDK handoffs, AutoGen Swarm HandoffMessage, A2A task delegation

---

## 1. Overview

Modern multi-agent systems are increasingly distributed: a coder agent, a reviewer agent, a researcher agent, and an orchestrator may run as separate processes — potentially on separate machines — connected through message queues and shared state rather than a single in-process call graph. Today TAG supports multi-agent coordination through `tag swarm` (PRD-004) and the background queue (PRD-008), but both mechanisms force a centralized model: a single orchestrator must know about all downstream agents and submit tasks to them explicitly. There is no first-class concept of an agent "handing off" work to a peer based on that peer's declared capabilities.

This PRD introduces the `HandoffMessage` primitive: a typed, JSON-serializable Go struct that any agent can emit to signal that a unit of work should be transferred to a different agent. The `tag handoff` command suite provides the CLI surface for creating, listing, accepting, and inspecting these messages. Agents emit handoffs instead of making ad-hoc queue insertions; a lightweight dispatcher watches for pending handoffs and routes them to the appropriate agent profile without requiring a central orchestrator to coordinate the transfer.

The design draws directly from two proven prior-art patterns. The OpenAI Agents SDK models handoffs as function-tool invocations: `handoff()` generates a `transfer_to_<agent_name>` tool that, when called, transfers full conversation history to the target agent; `as_tool()` generates a scoped function tool that returns a string result. AutoGen's Swarm pattern uses an explicit `HandoffMessage(source, target, content, context)` — the Swarm runtime scans the most recent messages for a `HandoffMessage` and selects the next speaker accordingly. The A2A protocol (Linux Foundation A2A Project, v1.0 stable, 2026) models this as task delegation: a client agent submits a Task to a server agent's A2A endpoint, with the Task containing the full context payload. TAG's `HandoffMessage` unifies these patterns into a single local primitive that is protocol-agnostic, SQLite-backed, and inspectable via the CLI.

The core guarantee of this feature is **decentralization**: no single process needs to know the complete routing graph at startup. An orchestrator agent emits a `HandoffMessage` targeting `coder`. The `tag handoff` dispatcher picks it up and submits it to the `coder` profile's queue. If `coder` is busy, the dispatcher waits; if `coder` does not exist, the handoff transitions to `REJECTED` with a reason. Agents gain and lose availability dynamically. The routing graph is emergent from the set of live handoff messages, not from a statically configured topology.

The feature is additive: no existing `tag run`, `tag swarm`, or `tag queue` behavior changes. Teams that do not use handoffs are unaffected. Teams that adopt handoffs gain explicit, auditable, retryable delegation semantics that persist across process restarts and are visible in both `tag trace` and the AgentOps integration (PRD-044).

---

## 2. Problem Statement

### 2.1 Task submission is orchestrator-centric, creating a single point of failure

All multi-agent coordination in TAG today flows through a central orchestrator. The `tag swarm` command (PRD-004) creates a kanban board and assigns cards; the queue (PRD-008) accepts explicit job submissions with a hard-coded target profile. If the orchestrator process crashes mid-execution, in-flight delegations are lost. There is no mechanism for a leaf agent to say "I am not the right agent for this subtask; route it elsewhere." Agents cannot initiate routing — they can only receive it.

In production multi-agent deployments, this is a meaningful reliability gap. The A2A protocol explicitly supports agents initiating task delegation to other agents through its Agent-to-Agent RPC layer. AutoGen Swarm allows any agent to emit a `HandoffMessage` that causes the runtime to transfer control. OpenAI Agents SDK's `handoff()` makes every agent a potential router. TAG has none of this.

### 2.2 There is no durable, inspectable record of inter-agent delegation

When the current `cmd_swarm` implementation assigns kanban cards to profiles, the assignment is recorded in a kanban board row, but the delegation intent — why this subtask was given to this agent, with what priority and deadline, and what context was passed — is not captured in a structured way. Auditing multi-agent runs requires reconstructing delegation chains from span data (PRD-013) or log files, which is fragile and incomplete.

Regulated environments (financial services, healthcare AI) increasingly require audit trails that show exactly which agent delegated which task to which other agent, with timestamps and context. A structured `HandoffMessage` table in SQLite, with full provenance metadata, directly addresses this requirement.

### 2.3 Agent-to-agent protocol interoperability requires a native delegation primitive

The broader agent interoperability ecosystem (A2A, ACP, ANP, MCP) is converging on a common pattern: agents publish capability declarations, tasks are submitted via structured messages, and responses flow back through typed reply channels. TAG's current ad-hoc queue insertion is invisible to external A2A clients, ACP runners, and ANP-connected agents. Before TAG can participate in cross-protocol agent networks, it needs an internal primitive that maps cleanly to the external protocol's task-delegation semantics.

A `HandoffMessage` with explicit `from_agent`, `to_agent`, `task`, `context`, `priority`, and `deadline` fields maps directly to:
- A2A `Task.message` (A2A v1.0 spec, `lf.a2a.v1` protobuf package)
- ACP `RunCreateRequest` (`POST /runs` with `agent_name` + `input` + `metadata`)
- AutoGen `HandoffMessage(source, target, content, context)` (context = model message history)
- OpenAI Agents SDK implicit handoff via `transfer_to_<agent_name>` tool invocation

Building the internal primitive first allows future PRDs to add protocol adapters without redesigning the core data model.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Introduce a `HandoffMessage` Go struct (in `internal/agent`) with JSON/schema tags and fields: `id`, `from_agent`, `to_agent`, `task`, `context`, `priority`, `deadline`, `status`, `created_at`, `accepted_at`, `completed_at`, `metadata`. Schema is generated with `invopop/jsonschema`. |
| G2 | Persist `HandoffMessage` instances in a new `handoffs` table in `tag.sqlite3` via the `modernc.org/sqlite` store, using WAL mode, with appropriate indexes on `(status, to_agent, priority, created_at)`. |
| G3 | Implement `tag handoff send` to create and persist a `HandoffMessage` from the CLI, returning the handoff ID. |
| G4 | Implement `tag handoff list` to list handoffs filtered by status, agent, priority, or deadline, with `--json` output. |
| G5 | Implement `tag handoff accept <id>` to claim a pending handoff, transition it to `ACCEPTED`, and inject it into the target agent's queue (queue_jobs table) as a new job, retaining full context. |
| G6 | Implement `tag handoff status <id>` to retrieve the current state of a handoff with full provenance metadata, with `--json` output. |
| G7 | Implement `tag handoff reject <id>` and `tag handoff cancel <id>` for explicit lifecycle management. |
| G8 | Integrate handoff lifecycle events as OpenTelemetry spans (PRD-013, via `go.opentelemetry.io/otel`) so handoff delegation chains appear in `tag trace`. |
| G9 | All handoff state transitions are atomic SQLite transactions with WAL mode; no handoff can be accepted by two concurrent goroutines/processes simultaneously (SQLite exclusive row-level lock via `BEGIN IMMEDIATE`; `modernc.org/sqlite` is single-writer). |
| G10 | `tag handoff send` validates that `--to` references an existing profile (or uses `--force` to skip validation), preventing silent misroutes. |
| G11 | Priority supports a five-level typed string constant set (`critical`, `high`, `normal`, `low`, `background`) mapping to integer weights 1–5 compatible with the existing `queue_jobs.priority` column. |
| G12 | Deadline is stored as an ISO 8601 UTC timestamp (`time.Time` marshalled via RFC 3339); the dispatcher warns (but does not reject) when a handoff is accepted after its deadline. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | **Automatic agent discovery or capability matching.** `tag handoff send --to coder` requires the caller to know the target profile name. Semantic capability matching ("find an agent that can write Python") is a future PRD. |
| NG2 | **Cross-machine or cross-process network transport.** Handoffs are stored in the local SQLite database. Distributed delivery over A2A, ACP, or gRPC transport is a separate protocol adapter PRD. |
| NG3 | **Replacing `tag swarm` or `tag queue`.** Handoffs are an additional coordination primitive, not a replacement. Swarm creates kanban boards; queues run background jobs; handoffs model explicit inter-agent delegation intent. |
| NG4 | **Full conversation history transfer.** AutoGen's `HandoffMessage.context` is the model's LLM message history; TAG's `HandoffMessage.Context` is a free-form `map[string]any` JSON metadata blob. Full context window transfer (like OpenAI's `handoff()`) is out of scope for this PRD. |
| NG5 | **Automatic retry with backoff.** If an accepted handoff fails (the queue job it spawns exits non-zero), the handoff transitions to `FAILED`. Retry scheduling is handled by the existing queue worker (PRD-008), not the handoff layer. |
| NG6 | **UI/TUI visualization of handoff graphs.** `tag handoff list --json` provides the data; graph rendering belongs in PRD-054 (local browser DevUI). |
| NG7 | **Security sandboxing of handoff context payloads.** The `context` field is trusted caller input. Secret scanning (PRD-034) applies to task text; full context sandboxing is out of scope. |

---

## 5. Success Metrics

| Metric | Target | How Measured |
|--------|--------|--------------|
| Handoff creation latency | `tag handoff send` completes in < 50ms (p95) | `time tag handoff send ...` over 100 iterations |
| Acceptance atomicity | Zero duplicate acceptances under 20 concurrent `tag handoff accept` calls on the same ID | Concurrent `go test` spawning 20 goroutines via `sync.WaitGroup`/`errgroup` |
| Queue injection fidelity | 100% of accepted handoffs appear as `queue_jobs` rows within 100ms | Integration test: accept → query queue_jobs |
| Trace integration | Every handoff lifecycle event (SEND, ACCEPT, COMPLETE, REJECT, CANCEL) appears as a child span under the originating trace | `tag trace show` with handoff-linked run ID |
| Profile validation | `tag handoff send --to nonexistent` exits non-zero with a human-readable error | Unit test |
| Priority ordering | `tag handoff list --pending --to coder` returns messages ordered by (priority ASC, deadline ASC, created_at ASC) | SQL query result ordering assertion |
| Deadline warning | Accepting a past-deadline handoff prints a visible warning but still succeeds | Integration test with `deadline = now() - 1h` |
| JSON output conformance | `--json` output on all subcommands validates against the `invopop/jsonschema`-generated `HandoffMessage` schema | Unit test (`santhosh-tekuri/jsonschema`) |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Orchestrator agent | emit `tag handoff send --from orchestrator --to coder --task "Implement the auth module" --priority high` from a tool call | the coder agent receives a fully-specified work item without me needing to know the coder's internal queue address |
| U2 | Coder agent | run `tag handoff list --pending --to coder --json` | I can poll for work delegated to me and pick up the next highest-priority task |
| U3 | Coder agent | run `tag handoff accept abc123 --profile coder` | the handoff is atomically claimed (no duplicate) and a queue job is created that I can process |
| U4 | Platform engineer | run `tag handoff status abc123 --json` | I can inspect the full provenance of a delegation — who sent it, when it was accepted, what queue job it produced |
| U5 | Reviewer agent | emit `tag handoff send --from reviewer --to coder --task "Fix the 3 lint errors in auth.py" --context '{"pr": 42}' --deadline 2026-06-18T09:00:00Z` | the coder receives a time-bounded, context-enriched fix request without me knowing the coder's schedule |
| U6 | Platform engineer | run `tag handoff list --status EXPIRED --json` | I can detect handoffs that were not accepted before their deadline and decide whether to re-emit them |
| U7 | Orchestrator | run `tag handoff cancel abc123 --reason "task superseded by PR merge"` | pending delegation is cleanly withdrawn without being picked up by a waiting agent |
| U8 | Security auditor | query `SELECT * FROM handoffs WHERE from_agent = 'orchestrator' AND created_at > '2026-06-01'` | I have a complete, tamper-evident record of all inter-agent delegations for compliance reporting |
| U9 | Developer | run `tag handoff list --from orchestrator --to coder --since 2026-06-10 --json` | I can review the delegation history between two specific agents for debugging a coordination bug |
| U10 | CI pipeline | run `tag handoff list --pending --to coder --json \| jq '.[0].id'` and then `tag handoff accept <id>` | a scripted agent worker can implement a polling-and-processing loop without custom Go code |

---

## 7. Proposed CLI Surface

All handoff subcommands live under the `tag handoff` namespace.

### 7.1 `tag handoff send`

Create and persist a new `HandoffMessage`.

```
tag handoff send \
  --from <agent>            # Required. Originating agent profile name.
  --to <agent>              # Required. Target agent profile name.
  --task <text>             # Required. Task description (free-form text).
  [--context <json>]        # Optional. JSON object with arbitrary metadata.
  [--priority critical|high|normal|low|background]  # Default: normal
  [--deadline <iso8601>]    # Optional. UTC ISO 8601 deadline (e.g. 2026-06-18T09:00:00Z).
  [--force]                 # Skip target profile existence validation.
  [--json]                  # Output the created HandoffMessage as JSON.
  [--trace-id <id>]         # Associate with an existing trace (PRD-013).
```

**Example:**
```sh
$ tag handoff send \
    --from orchestrator \
    --to coder \
    --task "Implement the auth module per spec in docs/auth-spec.md" \
    --priority high \
    --context '{"pr": 42, "branch": "feature/auth"}' \
    --deadline 2026-06-18T09:00:00Z

Handoff created: hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7
  from:     orchestrator
  to:       coder
  priority: high
  deadline: 2026-06-18T09:00:00Z
  status:   PENDING
```

**JSON output (`--json`):**
```json
{
  "id": "hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7",
  "from_agent": "orchestrator",
  "to_agent": "coder",
  "task": "Implement the auth module per spec in docs/auth-spec.md",
  "context": {"pr": 42, "branch": "feature/auth"},
  "priority": "high",
  "priority_weight": 2,
  "deadline": "2026-06-18T09:00:00Z",
  "status": "PENDING",
  "created_at": "2026-06-17T14:22:05Z",
  "accepted_at": null,
  "completed_at": null,
  "queue_job_id": null,
  "trace_id": null,
  "metadata": {}
}
```

**Exit codes:**
- `0` — handoff created successfully
- `1` — target profile not found (without `--force`)
- `2` — invalid `--context` (not valid JSON)
- `3` — invalid `--deadline` (not parseable as ISO 8601)

---

### 7.2 `tag handoff list`

List handoffs with filtering and sorting.

```
tag handoff list \
  [--pending]               # Shorthand for --status PENDING
  [--status PENDING|ACCEPTED|COMPLETED|REJECTED|CANCELLED|EXPIRED]
  [--to <agent>]            # Filter by target agent
  [--from <agent>]          # Filter by source agent
  [--priority critical|high|normal|low|background]
  [--since <iso8601>]       # Filter by created_at >= since
  [--until <iso8601>]       # Filter by created_at <= until
  [--limit N]               # Max rows returned (default: 50)
  [--json]                  # Output as JSON array
```

**Example (human-readable):**
```
$ tag handoff list --pending --to coder

ID                           FROM          TO     PRIORITY  DEADLINE              STATUS
hnd_01J4KXPQ3Y8Z2MRVW5T6...  orchestrator  coder  high      2026-06-18T09:00:00Z  PENDING
hnd_01J4KXPR2A7N1LQVB8S3...  reviewer      coder  normal    —                     PENDING
```

**JSON output (`--json`):**
```json
[
  {
    "id": "hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7",
    "from_agent": "orchestrator",
    "to_agent": "coder",
    "task": "Implement the auth module...",
    "priority": "high",
    "priority_weight": 2,
    "deadline": "2026-06-18T09:00:00Z",
    "status": "PENDING",
    "created_at": "2026-06-17T14:22:05Z",
    "accepted_at": null,
    "completed_at": null,
    "queue_job_id": null
  }
]
```

---

### 7.3 `tag handoff accept <id>`

Atomically claim a pending handoff and enqueue it as a `queue_jobs` row.

```
tag handoff accept <id> \
  [--profile <profile>]     # Profile to run as (default: handoff's to_agent value)
  [--no-enqueue]            # Mark ACCEPTED without creating a queue_jobs row
  [--json]                  # Output the updated HandoffMessage + queue job ID as JSON
```

**Example:**
```sh
$ tag handoff accept hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7 --profile coder

Accepted handoff: hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7
  Queue job: qjob_7f3a2e1b4c9d
  Profile:   coder
  Status:    ACCEPTED
```

If the handoff is already `ACCEPTED` or in a terminal state, the command exits with code `4` and prints:
```
Error: handoff hnd_... is already ACCEPTED (accepted_at: 2026-06-17T14:25:10Z)
```

**JSON output (`--json`):**
```json
{
  "id": "hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7",
  "status": "ACCEPTED",
  "accepted_at": "2026-06-17T14:25:10Z",
  "queue_job_id": "qjob_7f3a2e1b4c9d",
  "profile": "coder"
}
```

---

### 7.4 `tag handoff status <id>`

Retrieve the full state of a handoff.

```
tag handoff status <id> \
  [--json]                  # Output full HandoffMessage as JSON
```

**Example:**
```sh
$ tag handoff status hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7 --json
```

Returns the full `HandoffMessage` JSON object (same schema as `send --json`), plus a `queue_job` nested object if a job was created:

```json
{
  "id": "hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7",
  "from_agent": "orchestrator",
  "to_agent": "coder",
  "task": "Implement the auth module per spec in docs/auth-spec.md",
  "context": {"pr": 42, "branch": "feature/auth"},
  "priority": "high",
  "priority_weight": 2,
  "deadline": "2026-06-18T09:00:00Z",
  "status": "COMPLETED",
  "created_at": "2026-06-17T14:22:05Z",
  "accepted_at": "2026-06-17T14:25:10Z",
  "completed_at": "2026-06-17T16:03:41Z",
  "queue_job_id": "qjob_7f3a2e1b4c9d",
  "trace_id": "trace_abc123",
  "metadata": {"version": 1},
  "queue_job": {
    "id": "qjob_7f3a2e1b4c9d",
    "status": "done",
    "exit_code": 0,
    "started_at": "2026-06-17T14:25:11Z",
    "finished_at": "2026-06-17T16:03:41Z"
  }
}
```

---

### 7.5 `tag handoff reject <id>`

Reject a pending handoff (agent refuses the task).

```
tag handoff reject <id> \
  [--reason <text>]         # Human-readable rejection reason
  [--json]
```

**Example:**
```sh
$ tag handoff reject hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7 \
    --reason "Auth implementation requires credentials I don't have"

Rejected handoff: hnd_01J4KXPQ3Y8Z2MRVW5T6NGDE7
  Reason: Auth implementation requires credentials I don't have
  Status: REJECTED
```

---

### 7.6 `tag handoff cancel <id>`

Cancel a handoff (sender withdraws it before acceptance).

```
tag handoff cancel <id> \
  [--reason <text>]
  [--json]
```

Only handoffs in `PENDING` status can be cancelled. `ACCEPTED` handoffs must be rejected by the acceptor.

---

### 7.7 `tag handoff expire`

Mark all past-deadline `PENDING` handoffs as `EXPIRED`. Intended for cron/CI use.

```
tag handoff expire \
  [--dry-run]               # Print which handoffs would be expired without mutating
  [--json]                  # Output list of expired handoff IDs
```

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `HandoffMessage` is a Go struct with `json`/`jsonschema` tags and fields: `ID string`, `FromAgent string`, `ToAgent string`, `Task string`, `Context map[string]any`, `Priority HandoffPriority`, `Deadline *time.Time`, `Status HandoffStatus`, `CreatedAt time.Time`, `AcceptedAt *time.Time`, `CompletedAt *time.Time`, `QueueJobID *string`, `TraceID *string`, `Metadata map[string]any`, `RejectReason *string`. | Must |
| FR-02 | `HandoffStatus` is a `string`-typed named type with constants: `PENDING`, `ACCEPTED`, `COMPLETED`, `REJECTED`, `CANCELLED`, `EXPIRED`. State machine: `PENDING → {ACCEPTED, REJECTED, CANCELLED, EXPIRED}`; `ACCEPTED → {COMPLETED, FAILED}`; all others are terminal. | Must |
| FR-03 | `HandoffPriority` is a `string`-typed named type with constants `critical`, `high`, `normal`, `low`, `background` mapping (via a `Weight()` method) to integer weights 1, 2, 3, 4, 5 respectively — compatible with the `queue_jobs.priority` INTEGER column. | Must |
| FR-04 | IDs are generated with a `hnd_` prefix followed by a ULID (26-character base32 Crockford encoding, monotonic, sortable by creation time) via `github.com/oklog/ulid/v2`, matching the existing `run_id` pattern in `internal/runtime`. | Must |
| FR-05 | A new `handoffs` table is created by the store's `Migrate(ctx)` step via `CREATE TABLE IF NOT EXISTS handoffs (...)` executed as an idempotent DDL batch. Schema migration via `ALTER TABLE ADD COLUMN` for future additions, following the existing migration pattern in `internal/runtime`. | Must |
| FR-06 | `tag handoff accept` uses a `BEGIN IMMEDIATE` transaction (`sql.Tx` opened with the modernc `_txlock=immediate` pragma) to lock the row during the `status = PENDING` check and `status = ACCEPTED` update atomically. Any concurrent acceptor that loses the lock receives the "already ACCEPTED" error and exits with code 4. | Must |
| FR-07 | When `tag handoff accept` succeeds, it inserts a row into `queue_jobs` with: `profile = handoff.ToAgent`, `task = handoff.Task`, `task_type = 'handoff'`, `priority = handoff.Priority.Weight()`, `status = 'queued'`, and `deps_json = '[]'`. The inserted `queue_jobs.id` is stored back into `handoffs.queue_job_id`. | Must |
| FR-08 | The `Context` field is serialized as JSON text in SQLite (`context_json TEXT`) via `encoding/json`. `tag handoff send --context` validates that the argument is valid JSON (via `json.Valid`/`json.Unmarshal`) before insertion; invalid JSON causes exit code 2 with a descriptive error. | Must |
| FR-09 | `tag handoff list` default ordering is `ORDER BY priority_weight ASC, deadline ASC NULLS LAST, created_at ASC`. This ordering ensures critical tasks surface first, then by deadline urgency, then by age. | Must |
| FR-10 | `tag handoff list --pending` is syntactic sugar for `--status PENDING`. Both forms are supported. | Should |
| FR-11 | Deadline validation: `--deadline` accepts ISO 8601 strings parseable by `time.Parse(time.RFC3339, ...)`. If the supplied deadline is already in the past at send-time, the CLI prints a warning but does not reject (agents may pre-stage handoffs for delayed dispatch). | Should |
| FR-12 | `tag handoff expire` queries `SELECT id FROM handoffs WHERE status='PENDING' AND deadline < <now_utc>` and bulk-updates them to `status='EXPIRED'` in a single transaction. With `--dry-run`, only `SELECT` is executed. | Should |
| FR-13 | Every status transition emits an OpenTelemetry span (via `go.opentelemetry.io/otel` wired through `internal/runtime` tracing, PRD-013) with span name `handoff.<transition>` (e.g., `handoff.send`, `handoff.accept`, `handoff.reject`) and attributes `handoff.id`, `handoff.from_agent`, `handoff.to_agent`, `handoff.priority`, `handoff.status`. | Should |
| FR-14 | `--trace-id` in `tag handoff send` stores the supplied trace ID in `handoffs.trace_id`, linking the handoff to an existing `tag run` trace for end-to-end observability. | Should |
| FR-15 | `tag handoff status <id>` for an `ACCEPTED` handoff performs a `LEFT JOIN` with `queue_jobs` on `queue_job_id` and returns the nested `queue_job` object in the JSON response, including current `status` and `exit_code`. | Should |
| FR-16 | The `--from` flag in `tag handoff send` validates against existing profiles with a warning (not an error) — agent names in handoffs can be external/remote agents not registered as TAG profiles. `--to` validation is strict by default (error, not warning) since we must enqueue into a known profile. | Should |
| FR-17 | `tag handoff cancel` only transitions `PENDING → CANCELLED`. Attempting to cancel an `ACCEPTED` handoff exits with code 5 and a message directing the user to `tag handoff reject`. | Must |
| FR-18 | `tag handoff reject` transitions `PENDING → REJECTED` or `ACCEPTED → REJECTED`. When rejecting an `ACCEPTED` handoff with a `queue_job_id`, the corresponding `queue_jobs` row is updated to `status='cancelled'` in the same transaction. | Must |
| FR-19 | The `internal/agent` package exposes a `HandoffDispatcher` type with `Send()`, `Accept()`, `Reject()`, `Cancel()`, `ListPending()`, and `ExpireOverdue()` methods (all taking `context.Context`). The `cmd/tag` `handoff` command delegates to `HandoffDispatcher` — no direct SQL in the command layer. | Must |
| FR-20 | `--json` on all subcommands outputs valid JSON to stdout. Human-readable output goes to stdout; error messages go to stderr. Zero human-readable output when `--json` is specified (no "Created:" prefix lines). | Must |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Latency.** `tag handoff send` (including SQLite write + OTel span) completes in < 50ms p95 on a MacBook with the DB on local SSD. | < 50ms p95 |
| NFR-02 | **Concurrency safety.** `tag handoff accept` with 100 concurrent callers on the same handoff ID must result in exactly 1 successful acceptance and 99 rejections. No deadlocks. | 100% correct |
| NFR-03 | **DB size.** Each `handoffs` row consumes < 4KB on average, allowing 250,000 handoffs in a 1GB database. Context payloads > 64KB are rejected with exit code 6. | < 4KB/row |
| NFR-04 | **No new heavyweight dependencies.** `internal/agent` imports only the Go standard library (`database/sql`, `encoding/json`, `time`, `context`, `errors`), the already-vendored `modernc.org/sqlite` driver, `github.com/oklog/ulid/v2` for IDs, and `invopop/jsonschema` for schema. `CGO_ENABLED=0`; the feature ships in the single static binary with no new system libraries. | Pure-Go only |
| NFR-05 | **Backward compatibility.** Adding `handoffs` to the store's `Migrate()` step is idempotent (`CREATE TABLE IF NOT EXISTS`). Existing databases are migrated non-destructively. `tag` commands other than `handoff` are unaffected. | 100% |
| NFR-06 | **Test coverage.** All 6 exported methods on `HandoffDispatcher` have unit tests using a temp-file SQLite database (modernc `sqlite` does not support a shared `:memory:` across connections, so tests use `t.TempDir()`). The `Accept` concurrency test spawns goroutines via `errgroup`. | >= 90% (branch, `go test -cover`) |
| NFR-07 | **JSON schema stability.** The `HandoffMessage` JSON output schema is versioned (`"schema_version": 1` in the root object) to enable forward-compatible parsing by external tools. | v1 stable |
| NFR-08 | **OTel span overhead.** OTel span creation via `go.opentelemetry.io/otel` adds < 1ms per operation (consistent with existing span timing in PRD-013 benchmarks). | < 1ms |
| NFR-09 | **Error messages.** All user-facing errors include the handoff ID, the current status, and a suggested next action (e.g., "Use `tag handoff reject` to reject an accepted handoff"). | Human-readable |
| NFR-10 | **Context size validation.** The `--context` JSON blob is validated to be ≤ 65,536 bytes (64KB) before insertion. Larger payloads are rejected with exit code 6 and a clear message. | 64KB limit |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `internal/agent/handoff.go` | `HandoffMessage` struct, `HandoffStatus`, `HandoffPriority` named types, and the `HandoffStateError` error type. |
| `internal/agent/handoff_dispatcher.go` | `HandoffDispatcher` type with all persistence and state-machine logic. |
| `internal/agent/handoff_test.go` | Unit and concurrency tests for `HandoffDispatcher`. |
| `cmd/tag/handoff.go` | `tag handoff` cobra command tree (`send`, `list`, `accept`, `status`, `reject`, `cancel`, `expire`) delegating to `HandoffDispatcher`. |
| `cmd/tag/handoff_test.go` | Command-level integration tests exercising the CLI against a temp DB. |

### 10.2 Modified Files

| File | Change |
|------|--------|
| `internal/runtime/store.go` | Add `handoffs` table DDL + indexes to the idempotent `Migrate(ctx)` migration batch. |
| `cmd/tag/root.go` | Register the `handoff` command on the root `cobra.Command` in `newRootCmd()`. |

### 10.3 SQLite DDL

The following DDL is added to the idempotent migration batch executed by the store's `Migrate(ctx)` method (a single `db.ExecContext` per statement, or one multi-statement string on the modernc driver). The `modernc.org/sqlite` connection is opened with `PRAGMA journal_mode=WAL` and `PRAGMA busy_timeout=5000`:

```sql
CREATE TABLE IF NOT EXISTS handoffs (
  id              TEXT PRIMARY KEY,
  from_agent      TEXT NOT NULL,
  to_agent        TEXT NOT NULL,
  task            TEXT NOT NULL,
  context_json    TEXT NOT NULL DEFAULT '{}',
  priority        TEXT NOT NULL DEFAULT 'normal',
  priority_weight INTEGER NOT NULL DEFAULT 3,
  deadline        TEXT,           -- ISO 8601 UTC, nullable
  status          TEXT NOT NULL DEFAULT 'PENDING',
  created_at      TEXT NOT NULL,
  accepted_at     TEXT,
  completed_at    TEXT,
  queue_job_id    TEXT,           -- FK to queue_jobs.id, nullable
  trace_id        TEXT,           -- FK to spans.trace_id, nullable
  reject_reason   TEXT,
  metadata_json   TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_handoffs_status_to_priority
  ON handoffs(status, to_agent, priority_weight, created_at);

CREATE INDEX IF NOT EXISTS idx_handoffs_from
  ON handoffs(from_agent, created_at);

CREATE INDEX IF NOT EXISTS idx_handoffs_deadline
  ON handoffs(deadline)
  WHERE deadline IS NOT NULL AND status = 'PENDING';

CREATE INDEX IF NOT EXISTS idx_handoffs_trace
  ON handoffs(trace_id)
  WHERE trace_id IS NOT NULL;
```

The `priority_weight` column is denormalized from the `priority` enum to allow efficient `ORDER BY priority_weight ASC` without a CASE expression. It is always set atomically with `priority` in the application layer.

### 10.4 Core Types (`internal/agent/handoff.go`)

```go
package agent

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// HandoffStatus is the lifecycle state of a HandoffMessage.
type HandoffStatus string

const (
	StatusPending   HandoffStatus = "PENDING"
	StatusAccepted  HandoffStatus = "ACCEPTED"
	StatusCompleted HandoffStatus = "COMPLETED"
	StatusRejected  HandoffStatus = "REJECTED"
	StatusCancelled HandoffStatus = "CANCELLED"
	StatusExpired   HandoffStatus = "EXPIRED"
	StatusFailed    HandoffStatus = "FAILED"
)

// IsTerminal reports whether no further transitions are allowed.
func (s HandoffStatus) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusRejected, StatusCancelled, StatusExpired, StatusFailed:
		return true
	default:
		return false
	}
}

// HandoffPriority is a five-level priority mapped to a queue weight.
type HandoffPriority string

const (
	PriorityCritical   HandoffPriority = "critical"
	PriorityHigh       HandoffPriority = "high"
	PriorityNormal     HandoffPriority = "normal"
	PriorityLow        HandoffPriority = "low"
	PriorityBackground HandoffPriority = "background"
)

// Weight returns the integer weight compatible with queue_jobs.priority.
func (p HandoffPriority) Weight() int {
	switch p {
	case PriorityCritical:
		return 1
	case PriorityHigh:
		return 2
	case PriorityLow:
		return 4
	case PriorityBackground:
		return 5
	default: // normal
		return 3
	}
}

// HandoffMessage is the typed, serializable handoff primitive. JSON and
// jsonschema tags drive both CLI --json output and invopop/jsonschema generation.
type HandoffMessage struct {
	ID           string          `json:"id" jsonschema:"required"`
	FromAgent    string          `json:"from_agent" jsonschema:"required"`
	ToAgent      string          `json:"to_agent" jsonschema:"required"`
	Task         string          `json:"task" jsonschema:"required"`
	Context      map[string]any  `json:"context"`
	Priority     HandoffPriority `json:"priority"`
	PriorityWeight int           `json:"priority_weight"`
	Deadline     *time.Time      `json:"deadline"`
	Status       HandoffStatus   `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	AcceptedAt   *time.Time      `json:"accepted_at"`
	CompletedAt  *time.Time      `json:"completed_at"`
	QueueJobID   *string         `json:"queue_job_id"`
	TraceID      *string         `json:"trace_id"`
	RejectReason *string         `json:"reject_reason"`
	Metadata     map[string]any  `json:"metadata"`
	SchemaVersion int            `json:"schema_version"`
}

// MarshalJSON stamps the derived priority_weight and schema_version so external
// consumers always see them without callers having to set them by hand.
func (h HandoffMessage) MarshalJSON() ([]byte, error) {
	type alias HandoffMessage
	a := alias(h)
	a.PriorityWeight = h.Priority.Weight()
	a.SchemaVersion = 1
	return json.Marshal(a)
}

// NewHandoffID generates a hnd_-prefixed ULID matching TAG's ID conventions.
func NewHandoffID() string {
	return "hnd_" + ulid.Make().String()
}

// scanHandoff deserializes one SQL row (SELECT * FROM handoffs ...) into a struct.
func scanHandoff(rows interface{ Scan(...any) error }) (*HandoffMessage, error) {
	var (
		h                                  HandoffMessage
		ctxJSON, metaJSON                  string
		deadline, acceptedAt, completedAt  sql.NullString
		queueJobID, traceID, rejectReason  sql.NullString
		priority, status                   string
	)
	if err := rows.Scan(
		&h.ID, &h.FromAgent, &h.ToAgent, &h.Task, &ctxJSON,
		&priority, &h.PriorityWeight, &deadline, &status,
		new(string) /*created_at bound below*/, &acceptedAt, &completedAt,
		&queueJobID, &traceID, &rejectReason, &metaJSON,
	); err != nil {
		return nil, err
	}
	h.Priority = HandoffPriority(priority)
	h.Status = HandoffStatus(status)
	_ = json.Unmarshal([]byte(orDefault(ctxJSON, "{}")), &h.Context)
	_ = json.Unmarshal([]byte(orDefault(metaJSON, "{}")), &h.Metadata)
	h.Deadline = parseNullTime(deadline)
	h.AcceptedAt = parseNullTime(acceptedAt)
	h.CompletedAt = parseNullTime(completedAt)
	h.QueueJobID = nullStr(queueJobID)
	h.TraceID = nullStr(traceID)
	h.RejectReason = nullStr(rejectReason)
	return &h, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nullStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

func parseNullTime(n sql.NullString) *time.Time {
	if !n.Valid || n.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, n.String)
	if err != nil {
		return nil
	}
	return &t
}
```

> The real `scanHandoff` binds `created_at` into `h.CreatedAt` (elided above for brevity); production code uses `sqlx`-style column scanning or an explicit ordered `Scan` matching the DDL column order.

### 10.5 `HandoffDispatcher` Type (`internal/agent/handoff_dispatcher.go`)

```go
package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const maxContextBytes = 65_536 // 64KB

var tracer = otel.Tracer("github.com/tag-agent/tag/internal/agent")

// ErrNotFound is returned when a handoff ID does not exist.
var ErrNotFound = errors.New("handoff not found")

// HandoffStateError signals an illegal state transition.
type HandoffStateError struct {
	HandoffID     string
	CurrentStatus HandoffStatus
	Message       string
}

func (e *HandoffStateError) Error() string { return e.Message }

// HandoffDispatcher holds persistence and state-machine logic for handoffs.
// It wraps *sql.DB (modernc.org/sqlite, WAL). No SQL appears in cmd/tag.
type HandoffDispatcher struct {
	db *sql.DB
}

// NewHandoffDispatcher constructs a dispatcher over an open store DB handle.
func NewHandoffDispatcher(db *sql.DB) *HandoffDispatcher {
	return &HandoffDispatcher{db: db}
}

// SendParams are the inputs to Send.
type SendParams struct {
	FromAgent string
	ToAgent   string
	Task      string
	Context   map[string]any
	Priority  HandoffPriority
	Deadline  *time.Time
	TraceID   *string
	Metadata  map[string]any
}

// Send creates and persists a new PENDING HandoffMessage.
func (d *HandoffDispatcher) Send(ctx context.Context, p SendParams) (*HandoffMessage, error) {
	ctxSpan, span := tracer.Start(ctx, "handoff.send")
	defer span.End()

	if p.Context == nil {
		p.Context = map[string]any{}
	}
	ctxJSON, err := json.Marshal(p.Context)
	if err != nil {
		return nil, fmt.Errorf("marshal context: %w", err)
	}
	if len(ctxJSON) > maxContextBytes {
		return nil, fmt.Errorf("context payload exceeds %d bytes (%d bytes); reduce context size",
			maxContextBytes, len(ctxJSON))
	}
	if p.Priority == "" {
		p.Priority = PriorityNormal
	}
	metaJSON, _ := json.Marshal(orDefaultMap(p.Metadata))

	h := &HandoffMessage{
		ID:        NewHandoffID(),
		FromAgent: p.FromAgent,
		ToAgent:   p.ToAgent,
		Task:      p.Task,
		Context:   p.Context,
		Priority:  p.Priority,
		Deadline:  p.Deadline,
		Status:    StatusPending,
		CreatedAt: time.Now().UTC(),
		TraceID:   p.TraceID,
		Metadata:  orDefaultMap(p.Metadata),
	}

	_, err = d.db.ExecContext(ctxSpan, `
		INSERT INTO handoffs (
		  id, from_agent, to_agent, task, context_json,
		  priority, priority_weight, deadline, status,
		  created_at, trace_id, metadata_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.FromAgent, h.ToAgent, h.Task, string(ctxJSON),
		string(h.Priority), h.Priority.Weight(), rfc3339Ptr(h.Deadline),
		string(h.Status), h.CreatedAt.Format(time.RFC3339), h.TraceID, string(metaJSON),
	)
	if err != nil {
		return nil, err
	}
	setHandoffSpanAttrs(span, h)
	return h, nil
}

// Accept atomically transitions PENDING → ACCEPTED and inserts a queue_jobs row.
// It uses a BEGIN IMMEDIATE transaction to prevent concurrent double-acceptance.
func (d *HandoffDispatcher) Accept(ctx context.Context, id, profile string) (*HandoffMessage, string, error) {
	ctxSpan, span := tracer.Start(ctx, "handoff.accept")
	defer span.End()

	// _txlock=immediate on the DSN makes BeginTx acquire a write lock up front.
	tx, err := d.db.BeginTx(ctxSpan, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	h, err := scanHandoff(tx.QueryRowContext(ctxSpan, `SELECT * FROM handoffs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("%w: %q", ErrNotFound, id)
	} else if err != nil {
		return nil, "", err
	}
	if h.Status != StatusPending {
		return nil, "", &HandoffStateError{
			HandoffID: id, CurrentStatus: h.Status,
			Message: fmt.Sprintf("Cannot accept: handoff is already %s", h.Status),
		}
	}

	effProfile := profile
	if effProfile == "" {
		effProfile = h.ToAgent
	}
	jobID := "qjob_" + strings.ToLower(ulid.Make().String()[:12])
	now := time.Now().UTC().Format(time.RFC3339)

	if _, err = tx.ExecContext(ctxSpan, `
		INSERT INTO queue_jobs
		  (id, profile, task, task_type, status, priority, created_at, notify, deps_json)
		VALUES (?, ?, ?, 'handoff', 'queued', ?, ?, 1, '[]')`,
		jobID, effProfile, h.Task, h.Priority.Weight(), now); err != nil {
		return nil, "", err
	}
	if _, err = tx.ExecContext(ctxSpan, `
		UPDATE handoffs SET status='ACCEPTED', accepted_at=?, queue_job_id=? WHERE id=?`,
		now, jobID, id); err != nil {
		return nil, "", err
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}

	updated, err := d.Get(ctxSpan, id)
	if err != nil {
		return nil, "", err
	}
	setHandoffSpanAttrs(span, updated)
	return updated, jobID, nil
}

// Reject transitions PENDING or ACCEPTED → REJECTED, cancelling any linked job.
func (d *HandoffDispatcher) Reject(ctx context.Context, id string, reason *string) (*HandoffMessage, error) {
	_, span := tracer.Start(ctx, "handoff.reject")
	defer span.End()

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	h, err := scanHandoff(tx.QueryRowContext(ctx, `SELECT * FROM handoffs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	} else if err != nil {
		return nil, err
	}
	if h.Status != StatusPending && h.Status != StatusAccepted {
		return nil, &HandoffStateError{
			HandoffID: id, CurrentStatus: h.Status,
			Message: fmt.Sprintf("Cannot reject: handoff is in terminal state %s", h.Status),
		}
	}
	if h.QueueJobID != nil {
		if _, err = tx.ExecContext(ctx,
			`UPDATE queue_jobs SET status='cancelled' WHERE id=? AND status='queued'`,
			*h.QueueJobID); err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE handoffs SET status='REJECTED', reject_reason=? WHERE id=?`, reason, id); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return d.Get(ctx, id)
}

// Cancel transitions PENDING → CANCELLED. Sender-only, before acceptance.
func (d *HandoffDispatcher) Cancel(ctx context.Context, id string, reason *string) (*HandoffMessage, error) {
	_, span := tracer.Start(ctx, "handoff.cancel")
	defer span.End()

	h, err := d.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if h.Status != StatusPending {
		return nil, &HandoffStateError{
			HandoffID: id, CurrentStatus: h.Status,
			Message: fmt.Sprintf(
				"Cannot cancel: handoff is %s. Use `tag handoff reject` to reject an already-accepted handoff.",
				h.Status),
		}
	}
	if _, err = d.db.ExecContext(ctx,
		`UPDATE handoffs SET status='CANCELLED', reject_reason=? WHERE id=?`, reason, id); err != nil {
		return nil, err
	}
	return d.Get(ctx, id)
}

// ListFilter carries the optional filters for ListPending.
type ListFilter struct {
	ToAgent   string
	FromAgent string
	Status    HandoffStatus // "" means no status filter
	Priority  HandoffPriority
	Since     *time.Time
	Until     *time.Time
	Limit     int
}

// ListPending returns handoffs matching the filter, ordered by priority then deadline then age.
func (d *HandoffDispatcher) ListPending(ctx context.Context, f ListFilter) ([]*HandoffMessage, error) {
	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString("SELECT * FROM handoffs WHERE 1=1")
	if f.Status != "" {
		sb.WriteString(" AND status = ?")
		args = append(args, string(f.Status))
	}
	if f.ToAgent != "" {
		sb.WriteString(" AND to_agent = ?")
		args = append(args, f.ToAgent)
	}
	if f.FromAgent != "" {
		sb.WriteString(" AND from_agent = ?")
		args = append(args, f.FromAgent)
	}
	if f.Priority != "" {
		sb.WriteString(" AND priority = ?")
		args = append(args, string(f.Priority))
	}
	if f.Since != nil {
		sb.WriteString(" AND created_at >= ?")
		args = append(args, f.Since.Format(time.RFC3339))
	}
	if f.Until != nil {
		sb.WriteString(" AND created_at <= ?")
		args = append(args, f.Until.Format(time.RFC3339))
	}
	sb.WriteString(" ORDER BY priority_weight ASC, deadline ASC NULLS LAST, created_at ASC")
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	sb.WriteString(" LIMIT ?")
	args = append(args, limit)

	rows, err := d.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*HandoffMessage
	for rows.Next() {
		h, err := scanHandoff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Get returns a single handoff by ID.
func (d *HandoffDispatcher) Get(ctx context.Context, id string) (*HandoffMessage, error) {
	h, err := scanHandoff(d.db.QueryRowContext(ctx, `SELECT * FROM handoffs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return h, err
}

// ExpireOverdue marks past-deadline PENDING handoffs as EXPIRED, returning their IDs.
func (d *HandoffDispatcher) ExpireOverdue(ctx context.Context, dryRun bool) ([]string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := d.db.QueryContext(ctx,
		`SELECT id FROM handoffs WHERE status='PENDING' AND deadline IS NOT NULL AND deadline < ?`, now)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if dryRun || len(ids) == 0 {
		return ids, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE handoffs SET status='EXPIRED' WHERE id IN (%s)`, placeholders),
		args...); err != nil {
		return nil, err
	}
	return ids, nil
}

func setHandoffSpanAttrs(span interface{ SetAttributes(...attribute.KeyValue) }, h *HandoffMessage) {
	span.SetAttributes(
		attribute.String("handoff.id", h.ID),
		attribute.String("handoff.from_agent", h.FromAgent),
		attribute.String("handoff.to_agent", h.ToAgent),
		attribute.String("handoff.priority", string(h.Priority)),
		attribute.String("handoff.status", string(h.Status)),
	)
}

func rfc3339Ptr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339)
}

func orDefaultMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
```

### 10.6 `handoff` command (`cmd/tag/handoff.go`)

The command layer is built with `spf13/cobra`. Each subcommand builds a
`HandoffDispatcher` over the shared store handle and never issues SQL directly.
Exit codes are returned by mapping errors in the root `Execute()` wrapper.

```go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/runtime"
)

// exitErr carries a process exit code alongside an error message.
type exitErr struct {
	code int
	err  error
}

func (e exitErr) Error() string { return e.err.Error() }

func newHandoffCmd(app *runtime.App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Formal inter-agent HandoffMessage routing (PRD-085)",
	}
	cmd.AddCommand(
		newHandoffSendCmd(app),
		newHandoffListCmd(app),
		newHandoffAcceptCmd(app),
		newHandoffStatusCmd(app),
		newHandoffRejectCmd(app),
		newHandoffCancelCmd(app),
		newHandoffExpireCmd(app),
	)
	return cmd
}

func newHandoffSendCmd(app *runtime.App) *cobra.Command {
	var (
		from, to, task, ctxStr, priority, deadline, traceID string
		force, asJSON                                       bool
	)
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Create and persist a HandoffMessage",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Validate --context JSON (must be a JSON object).
			ctxMap := map[string]any{}
			if ctxStr != "" {
				if err := json.Unmarshal([]byte(ctxStr), &ctxMap); err != nil {
					return exitErr{2, fmt.Errorf("invalid --context JSON: %w", err)}
				}
			}

			// Validate --deadline (RFC 3339 / ISO 8601).
			var deadlinePtr *time.Time
			if deadline != "" {
				t, err := time.Parse(time.RFC3339, deadline)
				if err != nil {
					return exitErr{3, fmt.Errorf(
						"invalid --deadline: %v. Use ISO 8601, e.g. 2026-06-18T09:00:00Z", err)}
				}
				if t.Before(time.Now().UTC()) {
					fmt.Fprintln(os.Stderr, "Warning: --deadline is already in the past")
				}
				deadlinePtr = &t
			}

			// Validate target profile unless --force.
			if !force && !app.Profiles.Exists(to) {
				return exitErr{1, fmt.Errorf(
					"profile %q not found. Use --force to send to an unregistered agent", to)}
			}

			d := agent.NewHandoffDispatcher(app.DB)
			h, err := d.Send(cmd.Context(), agent.SendParams{
				FromAgent: from, ToAgent: to, Task: task, Context: ctxMap,
				Priority: agent.HandoffPriority(priority), Deadline: deadlinePtr,
				TraceID: nilIfEmpty(traceID),
			})
			if err != nil {
				return exitErr{6, err} // context-too-large and other Send errors
			}

			if asJSON {
				return printJSON(h)
			}
			fmt.Printf("Handoff created: %s\n", h.ID)
			fmt.Printf("  from:     %s\n", h.FromAgent)
			fmt.Printf("  to:       %s\n", h.ToAgent)
			fmt.Printf("  priority: %s\n", h.Priority)
			fmt.Printf("  deadline: %s\n", deadlineStr(h.Deadline))
			fmt.Printf("  status:   %s\n", h.Status)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "Originating agent profile name (required)")
	f.StringVar(&to, "to", "", "Target agent profile name (required)")
	f.StringVar(&task, "task", "", "Task description (required)")
	f.StringVar(&ctxStr, "context", "", "JSON object with arbitrary metadata")
	f.StringVar(&priority, "priority", "normal", "critical|high|normal|low|background")
	f.StringVar(&deadline, "deadline", "", "ISO 8601 UTC deadline")
	f.StringVar(&traceID, "trace-id", "", "Associate with an existing trace")
	f.BoolVar(&force, "force", false, "Skip target profile validation")
	f.BoolVar(&asJSON, "json", false, "Output the created HandoffMessage as JSON")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	_ = cmd.MarkFlagRequired("task")
	return cmd
}

func newHandoffAcceptCmd(app *runtime.App) *cobra.Command {
	var profile string
	var noEnqueue, asJSON bool
	cmd := &cobra.Command{
		Use:   "accept <id>",
		Short: "Accept a pending HandoffMessage and enqueue it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d := agent.NewHandoffDispatcher(app.DB)
			h, jobID, err := d.Accept(cmd.Context(), args[0], profile)
			switch {
			case errors.Is(err, agent.ErrNotFound):
				return exitErr{1, err}
			case err != nil:
				var se *agent.HandoffStateError
				if errors.As(err, &se) {
					return exitErr{4, err}
				}
				return err
			}
			if asJSON {
				return printJSON(map[string]any{
					"id": h.ID, "status": h.Status,
					"accepted_at": h.AcceptedAt, "queue_job_id": jobID, "profile": h.ToAgent,
				})
			}
			fmt.Printf("Accepted handoff: %s\n  Queue job: %s\n  Profile:   %s\n  Status:    %s\n",
				h.ID, jobID, h.ToAgent, h.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "Profile to run as (default: handoff to_agent)")
	cmd.Flags().BoolVar(&noEnqueue, "no-enqueue", false, "Mark ACCEPTED without a queue_jobs row")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	return cmd
}

// newHandoffListCmd, newHandoffStatusCmd, newHandoffRejectCmd, newHandoffCancelCmd,
// and newHandoffExpireCmd follow the same shape: parse flags, call the matching
// HandoffDispatcher method, map agent.ErrNotFound → exit 1, *HandoffStateError →
// exit 4 (reject) / 5 (cancel), and honour --json. `status` LEFT JOINs queue_jobs
// and nests the job object (FR-15). Elided here for brevity.

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
```

### 10.7 Command Registration in `cmd/tag/root.go`

Cobra replaces argparse. The `handoff` command tree is attached to the root
command; `cobra`'s built-in `dest`-style binding is handled by the typed flag
vars above, so there is no separate `set_defaults` step.

```go
func newRootCmd(app *runtime.App) *cobra.Command {
	root := &cobra.Command{
		Use:           "tag",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// ... other commands (run, swarm, queue, trace) ...
	root.AddCommand(newHandoffCmd(app))
	return root
}

// Execute maps exitErr → os.Exit code; plain errors → exit 1.
func Execute(app *runtime.App) {
	if err := newRootCmd(app).Execute(); err != nil {
		var ee exitErr
		if errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, "Error:", ee.Error())
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
```


### 10.8 State Machine

```
                ┌──────────────────────────────────────────────┐
                │                   PENDING                    │
                │  (initial state after tag handoff send)      │
                └───────────┬──────────────┬───────────────────┘
                            │              │               │
                   accept   │     reject   │    cancel     │  deadline passed
                            │              │               │  (tag handoff expire)
                            ▼              ▼               ▼
                       ACCEPTED       REJECTED        CANCELLED
                            │
                    job     │   reject
                   done     │   (tag handoff reject)
                            │
              ┌─────────────┴──────────────┐
              ▼                            ▼
         COMPLETED                      FAILED
        (queue_jobs                  (queue_jobs
         exit_code=0)                 exit_code≠0)
```

All states in `{COMPLETED, REJECTED, CANCELLED, EXPIRED, FAILED}` are terminal. No transitions out of terminal states.

### 10.9 OTel Span Integration

Each `HandoffDispatcher` method starts an OTel span via `go.opentelemetry.io/otel`, using the tracer provider wired up in `internal/runtime` (PRD-013):

```go
// In HandoffDispatcher.Send():
ctx, span := tracer.Start(ctx, "handoff.send")
defer span.End()
span.SetAttributes(
	attribute.String("handoff.id", h.ID),
	attribute.String("handoff.from_agent", h.FromAgent),
	attribute.String("handoff.to_agent", h.ToAgent),
	attribute.String("handoff.priority", string(h.Priority)),
	attribute.String("handoff.status", string(h.Status)),
)
// ... insert into DB via db.ExecContext(ctx, ...) ...
```

The `trace_id` stored in `handoffs.trace_id` is the W3C TraceContext `trace-id` of the originating run, enabling `tag trace show <trace_id>` to display the full delegation chain including handoff spans.

### 10.10 Integration with Queue Worker (PRD-008)

When `HandoffDispatcher.Accept()` inserts a `queue_jobs` row with `task_type='handoff'`, the existing queue worker (the `internal/runtime` queue goroutine, PRD-008) picks it up on its next polling cycle with no modifications. The `task` column contains the handoff task description; the `profile` column contains the accepting agent's profile. The queue worker is unaware of the handoff layer — it sees a normal queued job.

When the queue job completes (`exit_code=0`), a future enhancement (see Open Questions) can hook the queue worker's completion path (a channel/`errgroup` callback) to update `handoffs.status = 'COMPLETED'` and set `completed_at`. For this PRD, `COMPLETED` state is set by the caller via `tag handoff status` polling or a direct `UPDATE`.

Because handoffs are persisted in the shared `modernc.org/sqlite` store rather than sent over a wire, no network transport is required for the in-process case (NG2). If a future PRD makes handoffs cross a process/network boundary, the same `HandoffDispatcher` can be exposed behind the `RuntimeService` wire seam — a `net/http` + `go-chi/chi` + `danielgtaylor/huma` JSON API (with `tmaxmax/go-sse` for `tag handoff watch`-style streaming) — without changing the core struct or state machine.

### 10.11 Comparison to Prior Art

| Concept | OpenAI Agents SDK | AutoGen Swarm | A2A v1.0 | TAG HandoffMessage |
|---------|------------------|---------------|----------|--------------------|
| Transfer mechanism | `handoff()` → `transfer_to_<name>` tool | `HandoffMessage(source, target, content, context)` | Task.send() JSON-RPC | `tag handoff send --from ... --to ...` |
| Context semantics | Full conversation history | LLM message history (model context) | Task payload (arbitrary parts) | Free-form `map[string]any` JSON (metadata only) |
| Persistence | In-memory / LLM context | AutoGen runtime message list | A2A server DB | SQLite `handoffs` table |
| Routing | Agent name → agent lookup | Swarm scans last message for `.target` | Agent Card `/.well-known/agent-card.json` | Profile name → `queue_jobs` row |
| Atomicity | N/A (single process) | N/A (single process) | HTTP POST (idempotent via task ID) | SQLite `BEGIN IMMEDIATE` |
| CLI surface | None (API only) | None (API only) | None (protocol only) | Full `tag handoff` CLI |

---

## 11. Security Considerations

1. **Context payload trust.** The `context_json` field is persisted as caller-supplied JSON. It is not executed, sandboxed, or sanitized beyond the size limit (64KB, NFR-10). Secrets (API keys, tokens) in context are stored in plaintext in SQLite. Users should not put sensitive credentials in handoff context. The secret scanning integration (PRD-034) does not currently scan `context_json`; a future enhancement should add a `security.scan_handoff_context` config flag to opt-in to secret scanning of context payloads before insertion.

2. **Local-only SQLite access.** Handoffs are stored in `~/.tag/runtime/tag.sqlite3` (or the path resolved from the koanf config key `runtime_dir`). Access control is filesystem-level: only the OS user who owns the file can read or write handoffs. There is no authentication layer on the local database. In multi-user deployments, each user has their own `~/.tag/` directory and isolated SQLite file.

3. **No network exposure.** `HandoffDispatcher` and `cmd_handoff` make no network calls. The `--to` profile lookup is entirely local. There is no risk of a handoff triggering an outbound connection to an attacker-controlled endpoint.

4. **Profile name injection.** The `--to` and `--from` values are stored verbatim as TEXT in SQLite using parameterized queries (no string interpolation). SQL injection is not possible. However, a malicious `--to` value like `'; DROP TABLE handoffs; --` is safely stored as a string and fails profile validation (FR-16) unless `--force` is passed.

5. **Race condition in concurrent acceptance.** The `BEGIN IMMEDIATE` transaction in `HandoffDispatcher.Accept()` prevents two concurrent goroutines (or OS processes) from both seeing `status=PENDING` and both succeeding. SQLite's WAL mode with `PRAGMA busy_timeout = 5000` (set on the `modernc.org/sqlite` DSN) means contending callers wait up to 5 seconds before returning a lock timeout error rather than returning incorrect data.

6. **Deadline bypass.** Deadlines are advisory: `tag handoff accept` after a deadline warns but still succeeds. This is intentional — the agent may have valid reasons to accept an overdue handoff (e.g., the deadline was pessimistic). The `EXPIRED` transition via `tag handoff expire` is explicit, not automatic.

7. **Audit trail immutability.** Rows in the `handoffs` table are never `DELETE`d by any `tag handoff` command. Cancel and reject only update the `status` column. This preserves a complete audit trail. A future `tag handoff purge` command for GDPR compliance is out of scope for this PRD.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/agent/handoff_test.go`)

Unit tests use the standard `testing` package with table-driven cases. Each test opens a fresh store DB in `t.TempDir()` (the modernc driver does not share a `:memory:` DB across pooled connections) and runs the store's `Migrate(ctx)` step. Assertions use `testify/require`.

| Test | Description |
|------|-------------|
| `TestSend_CreatesRow` | `d.Send(ctx, ...)` inserts exactly one row in `handoffs` with `status='PENDING'`. |
| `TestSend_InvalidContextJSON` | `--context 'not json'` at the command layer returns exit code 2. |
| `TestSend_ContextTooLarge` | Context map that marshals to > 65536 bytes returns a non-nil error from `d.Send()`. |
| `TestSend_InvalidDeadline` | `--deadline "not-a-date"` returns exit code 3. |
| `TestSend_PastDeadlineWarns` | `--deadline <yesterday>` succeeds (exit 0) but writes a warning to stderr. |
| `TestSend_UnknownProfile` | `--to nonexistent` without `--force` returns exit code 1. |
| `TestSend_UnknownProfileWithForce` | `--to nonexistent --force` returns exit code 0. |
| `TestAccept_TransitionsStatus` | `d.Accept(ctx, id, "")` updates `status` to `ACCEPTED` and sets `accepted_at`. |
| `TestAccept_CreatesQueueJob` | After `d.Accept(ctx, id, "")`, a `queue_jobs` row exists with `profile=to_agent` and `task_type='handoff'`. |
| `TestAccept_SecondCallFails` | Calling `d.Accept(ctx, id, "")` twice returns a `*HandoffStateError` on the second call. |
| `TestAccept_ConcurrentRace` | 20 goroutines (`errgroup.Group`) all calling `d.Accept(ctx, sameID, "")` yield exactly 1 success and 19 `*HandoffStateError` results. |
| `TestReject_FromPending` | `d.Reject(ctx, id, nil)` on a PENDING handoff transitions to REJECTED. |
| `TestReject_FromAcceptedCancelsJob` | `d.Reject(ctx, id, nil)` on an ACCEPTED handoff also sets `queue_jobs.status='cancelled'`. |
| `TestReject_FromTerminalErrors` | `d.Reject(ctx, id, nil)` on COMPLETED returns a `*HandoffStateError`. |
| `TestCancel_FromPending` | `d.Cancel(ctx, id, nil)` on PENDING transitions to CANCELLED. |
| `TestCancel_FromAcceptedErrors` | `d.Cancel(ctx, id, nil)` on ACCEPTED returns a `*HandoffStateError` whose message mentions `tag handoff reject`. |
| `TestList_FiltersByStatus` | `d.ListPending(ctx, ListFilter{Status: StatusPending})` returns only PENDING rows. |
| `TestList_FiltersByToAgent` | `d.ListPending(ctx, ListFilter{ToAgent: "coder"})` returns only rows where `to_agent='coder'`. |
| `TestList_Ordering` | Rows are returned in `(priority_weight ASC, deadline ASC NULLS LAST, created_at ASC)` order. |
| `TestExpireOverdue` | Handoffs with `deadline < now()` are transitioned to EXPIRED. Non-expired handoffs are untouched. |
| `TestExpire_DryRun` | `d.ExpireOverdue(ctx, true)` returns IDs but does not modify `status`. |
| `TestScanHandoff_Roundtrip` | A `HandoffMessage` marshalled to JSON and re-scanned from the row is stable through a send→fetch cycle. |
| `TestJSON_SchemaVersion` | `json.Marshal(h)` emits `"schema_version": 1`. |

### 12.2 Integration Tests (`cmd/tag/handoff_test.go`)

Integration tests build the root cobra command with a real store in `t.TempDir()` and drive it via `cmd.SetArgs(...)` / `cmd.Execute()`, capturing stdout/stderr with `cmd.SetOut`/`cmd.SetErr` (no subprocess needed for the single binary).

| Test | Description |
|------|-------------|
| `TestFlow_SendAcceptStatus` | Full `send → list --pending → accept → status` CLI flow against a real temp DB. |
| `TestAccept_QueueJobVisible` | After `tag handoff accept`, `tag queue list` shows the new job with `task_type=handoff`. |
| `TestReject_WithReason` | `tag handoff reject <id> --reason "..."` stores reason; `tag handoff status <id> --json` returns it. |
| `TestExpire_ViaCLI` | Create handoff with past deadline → `tag handoff expire` → `tag handoff status --json` shows EXPIRED. |
| `TestList_JSONSchemaValid` | `tag handoff list --json` output validates against the `invopop/jsonschema`-generated schema using `santhosh-tekuri/jsonschema`. |

### 12.3 Performance Tests (Go benchmarks)

| Test | Description | Target |
|------|-------------|--------|
| `BenchmarkSend` | `go test -bench=BenchmarkSend`; report ns/op and derive p95 over 100 iterations. | p95 < 50ms |
| `BenchmarkList1000` | Insert 1000 handoffs then `d.ListPending(ctx, ListFilter{Limit: 1000})`; measure query time. | < 100ms |
| `TestAccept_ConcurrentThroughput` | 100 goroutines race to accept the same handoff; measure total wall time and verify exactly 1 success. | < 2s total |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag handoff send --from orchestrator --to coder --task "..." --priority high` exits 0 and prints a `hnd_`-prefixed ID. | `go test -run TestSend_CreatesRow` |
| AC-02 | The created handoff is visible in `tag handoff list --pending --json` as a JSON array element with `status="PENDING"`. | `go test -run TestList_FiltersByStatus` |
| AC-03 | `tag handoff accept <id>` on a PENDING handoff exits 0, transitions to ACCEPTED, and creates a `queue_jobs` row visible in `tag queue list`. | `go test -run TestAccept_CreatesQueueJob`, integration test |
| AC-04 | A second concurrent `tag handoff accept <id>` on the same ID exits with code 4 and message "already ACCEPTED". | `go test -run TestAccept_ConcurrentRace` |
| AC-05 | `tag handoff status <id> --json` returns valid JSON with all `HandoffMessage` fields, `schema_version: 1`, and `queue_job` nested object if accepted. | `go test -run TestJSON_SchemaVersion` |
| AC-06 | `tag handoff reject <id> --reason "..."` transitions a PENDING or ACCEPTED handoff to REJECTED and stores the reason. | `go test -run TestReject_FromPending` |
| AC-07 | Rejecting an ACCEPTED handoff also sets the linked `queue_jobs` row to `status='cancelled'`. | `go test -run TestReject_FromAcceptedCancelsJob` |
| AC-08 | `tag handoff cancel <id>` on a PENDING handoff transitions to CANCELLED; on an ACCEPTED handoff exits code 5 with a message directing the user to `tag handoff reject`. | `go test -run TestCancel_FromAcceptedErrors` |
| AC-09 | `tag handoff send --to nonexistent` (without `--force`) exits code 1 with a message naming the missing profile. | `go test -run TestSend_UnknownProfile` |
| AC-10 | `tag handoff send --context 'not-json'` exits code 2 with a descriptive JSON parse error. | `go test -run TestSend_InvalidContextJSON` |
| AC-11 | `tag handoff send --deadline "not-a-date"` exits code 3. | `go test -run TestSend_InvalidDeadline` |
| AC-12 | `tag handoff expire` marks all PENDING handoffs with `deadline < now()` as EXPIRED. Non-past-deadline handoffs are untouched. | `go test -run TestExpireOverdue` |
| AC-13 | `tag handoff expire --dry-run` prints IDs that would be expired without modifying `status`. | `go test -run TestExpire_DryRun` |
| AC-14 | Every `HandoffDispatcher` method emits an OTel span visible in `tag trace show` output. | Integration test with an OTel in-memory `tracetest.SpanRecorder` |
| AC-15 | The store's `Migrate(ctx)` creates the `handoffs` table idempotently — calling it twice on the same DB returns no error. | `go test -run TestMigrate_Idempotent` |
| AC-16 | `tag handoff send --context <payload exceeding 64KB>` exits code 6 with a clear message about the size limit. | `go test -run TestSend_ContextTooLarge` |
| AC-17 | `tag handoff list --json` on an empty DB returns `[]`. | Unit test |
| AC-18 | `tag handoff list` (human-readable) renders a table with columns `ID`, `FROM`, `TO`, `PRIORITY`, `DEADLINE`, `STATUS`. | Integration test driving the cobra command in-process |

---

## 14. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `database/sql`, `encoding/json`, `time`, `context`, `errors`, `strings` | Stdlib | Go 1.24+ | Core persistence, serialization, timestamps, cancellation. |
| `modernc.org/sqlite` | Module | latest | Pure-Go SQLite driver (`CGO_ENABLED=0`). WAL, `BEGIN IMMEDIATE` via `_txlock=immediate`, single-writer. Already vendored by the store. |
| `github.com/oklog/ulid/v2` | Module | v2.x | `hnd_`-prefixed ULID generation (sortable, monotonic). |
| `github.com/invopop/jsonschema` | Module | v0.x | Generates the `HandoffMessage` JSON schema from struct tags for `--json` validation. |
| `github.com/spf13/cobra` | Module | v1.x | `tag handoff` command tree (replaces argparse). |
| `go.opentelemetry.io/otel` | Module | v1.x | OTel span emission for lifecycle events. |
| `github.com/stretchr/testify` + `github.com/santhosh-tekuri/jsonschema` | Module (test) | latest | Assertions and JSON-schema validation in tests. |
| PRD-008 (`internal/runtime` queue) | Internal | Current | `queue_jobs` table. `HandoffDispatcher.Accept()` inserts into it. |
| PRD-013 (`internal/runtime` tracing) | Internal | Current | OTel span emission for lifecycle events. |
| PRD-028 (`internal/tool` sandbox) | Internal | Current | No direct dependency; sandboxed runs can emit handoffs if the sandbox allows CLI access. |
| PRD-034 (`internal/tool` security) | Internal | Current | Future: opt-in context payload secret scanning. Not in scope for this PRD. |
| PRD-037 (personas) | Internal | Current | `--from` / `--to` can reference persona names as agent identifiers in future. |
| PRD-044 (`internal/runtime` agentops bridge) | Internal | Current | AgentOps session can be associated with handoff accept events for cross-agent session stitching in future. |

---

## 15. Open Questions

| ID | Question | Impact | Owner | Target |
|----|----------|--------|-------|--------|
| OQ-1 | Should the `internal/runtime` queue worker automatically update `handoffs.status = 'COMPLETED'` when a handoff-typed queue job finishes? This would close the feedback loop without requiring polling. Requires hooking the worker's job completion channel/callback. | Medium — determines whether COMPLETED state is useful or just decorative. | PRD author | Phase 2 |
| OQ-2 | Should `tag handoff send` support `--reply-to <handoff_id>` to chain handoffs (orchestrator → coder → reviewer reply chain)? This would enable request/reply patterns between agents. | Low for MVP, high for protocol convergence with A2A's task reply semantics. | PRD author | Future PRD |
| OQ-3 | The Go design standardizes on ULIDs (sortable, time-embedded, Crockford base32) via `github.com/oklog/ulid/v2`, which allows `ORDER BY id` to give chronological order without an extra `created_at` index. Open question: should we drop the redundant `created_at` index once all ID consumers rely on ULID ordering? | Low — affects index footprint only. | Engineering | Before implementation |
| OQ-4 | Should `HandoffMessage.context` support nested agent-card-like capability declarations (inspired by A2A `AgentCard.capabilities`) so that the receiver can verify it has the required capabilities before accepting? | Medium — enables semantic acceptance decisions; out of scope for this PRD. | Future research | Future PRD |
| OQ-5 | Should `tag handoff list --pending` be the recommended polling mechanism for agent workers, or should we add a `tag handoff watch` long-polling/SSE command? Polling has the advantage of composability (pipe to `jq`); SSE would reduce latency for high-frequency handoff systems. | Low for current scale; high if TAG is used in high-throughput pipelines. | Engineering | Phase 2 |
| OQ-6 | Should `tag handoff expire` be registered as a cron job automatically (e.g., every 5 minutes) via PRD-040 notification hooks, or remain purely manual? | Medium — overdue handoffs accumulate silently if no one runs `expire`. | Engineering | Phase 1 or 2 |
| OQ-7 | What is the correct mapping from `HandoffMessage` to an A2A v1.0 `Task` object (`lf.a2a.v1` protobuf)? Specifically: `from_agent` → `Task.metadata.sender_id`? `context` → `Task.message.parts`? This mapping is needed for the A2A adapter PRD (future). | High for protocol interoperability; deferred. | Engineering | A2A adapter PRD |

---

## 16. Complexity and Timeline

### Phase 1 — Core Handoff Primitive (Days 1–4)

| Day | Deliverable |
|-----|-------------|
| 1 | `internal/agent/handoff.go` with `HandoffStatus`, `HandoffPriority`, `HandoffMessage` struct, `HandoffStateError`, plus `HandoffDispatcher.Send()` and `HandoffDispatcher.Get()`. Full unit tests for these. |
| 2 | `HandoffDispatcher.Accept()` with `BEGIN IMMEDIATE` atomicity, queue_jobs injection, concurrent race test (`errgroup`). `HandoffDispatcher.Reject()` and `HandoffDispatcher.Cancel()`. |
| 3 | Add `handoffs` DDL + indexes to the store's `Migrate(ctx)`. Implement the `cmd/tag/handoff.go` cobra tree with `send`, `accept`, `status`, `reject`, `cancel` subcommands and root registration. |
| 4 | `tag handoff list` with all filter flags and `--json` output. Human-readable table output. All exit code paths. |

### Phase 2 — Integration and Polish (Days 5–7)

| Day | Deliverable |
|-----|-------------|
| 5 | OTel span emission in `HandoffDispatcher` (PRD-013 integration). `tag handoff expire` subcommand. `HandoffDispatcher.ExpireOverdue()`. |
| 6 | Integration tests: `send → list → accept → status` full CLI flow. `tag queue list` visibility after accept. `--json` schema validation tests. Performance benchmarks. |
| 7 | Documentation: update `README.md` CLI reference section for `tag handoff`. Update `docs/prd/INDEX.md`. Final review and cleanup. |

### Total Estimate: 7 working days (within the M = 1–2 week envelope)

### Risk Factors

- **SQLite concurrency edge cases:** The `BEGIN IMMEDIATE` approach (via `_txlock=immediate` on the DSN) is correct for WAL mode, but subtle interactions with `PRAGMA busy_timeout` and the `modernc.org/sqlite` single-writer connection pool can cause unexpected lock timeouts under heavy concurrent load. Set `db.SetMaxOpenConns(1)` for the writer path and mitigate with the concurrency test in Phase 1 (Day 2).
- **Timestamp parsing consistency:** All timestamps are stored as RFC 3339 strings and parsed with `time.Parse(time.RFC3339, ...)`; ensure the `Z`/`+00:00` offset forms round-trip identically and that `NULL` deadlines scan into `*time.Time(nil)`. Covered by `TestScanHandoff_Roundtrip`.
- **Queue worker coupling:** If the `internal/runtime` queue worker is modified in a concurrent PRD during Phase 2, the `queue_jobs` insertion in `Accept()` may need to be updated to match new required columns. Keep `Accept()` as minimal as possible (required columns only).

---

## References

- OpenAI Agents SDK handoffs: https://openai.github.io/openai-agents-python/handoffs/
- AutoGen Swarm HandoffMessage: https://microsoft.github.io/autogen/stable//user-guide/agentchat-user-guide/swarm.html
- A2A Protocol Specification v1.0: https://a2a-protocol.org/latest/specification/
- A2A Protobuf schema (`lf.a2a.v1`): https://github.com/a2aproject/A2A/blob/main/specification/a2a.proto
- ACP OpenAPI spec: https://github.com/i-am-bee/acp/blob/main/docs/spec/openapi.yaml
- Agent interoperability protocol comparison: https://arxiv.org/html/2505.02279v1
- RFC 8615 (Well-Known URIs): https://datatracker.ietf.org/doc/html/rfc8615
- TAG GitHub Issue #347: https://github.com/tag-project/tag/issues/347

