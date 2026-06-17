# PRD-085: Formal HandoffMessage Primitive for Decentralized Agent Routing (`tag handoff`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `controller.py + teams.py`
**Depends on:** PRD-004 (kanban swarm topology), PRD-008 (background task queue), PRD-013 (agent tracing/observability), PRD-023 (multi-agent swarm), PRD-028 (sandbox execution), PRD-033 (DAG dependency-aware queue), PRD-034 (security/secret scanning), PRD-037 (agent personas), PRD-044 (AgentOps session observability)
**Inspired by:** OpenAI Agents SDK handoffs, AutoGen Swarm HandoffMessage, A2A task delegation

---

## 1. Overview

Modern multi-agent systems are increasingly distributed: a coder agent, a reviewer agent, a researcher agent, and an orchestrator may run as separate processes — potentially on separate machines — connected through message queues and shared state rather than a single in-process call graph. Today TAG supports multi-agent coordination through `tag swarm` (PRD-004) and the background queue (PRD-008), but both mechanisms force a centralized model: a single orchestrator must know about all downstream agents and submit tasks to them explicitly. There is no first-class concept of an agent "handing off" work to a peer based on that peer's declared capabilities.

This PRD introduces the `HandoffMessage` primitive: a typed, serializable dataclass that any agent can emit to signal that a unit of work should be transferred to a different agent. The `tag handoff` command suite provides the CLI surface for creating, listing, accepting, and inspecting these messages. Agents emit handoffs instead of making ad-hoc queue insertions; a lightweight dispatcher watches for pending handoffs and routes them to the appropriate agent profile without requiring a central orchestrator to coordinate the transfer.

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
| G1 | Introduce a `HandoffMessage` dataclass (Python `@dataclass`) with fields: `id`, `from_agent`, `to_agent`, `task`, `context`, `priority`, `deadline`, `status`, `created_at`, `accepted_at`, `completed_at`, `metadata`. |
| G2 | Persist `HandoffMessage` instances in a new `handoffs` table in `tag.sqlite3` via `open_db()`, using WAL mode, with appropriate indexes on `(status, to_agent, priority, created_at)`. |
| G3 | Implement `tag handoff send` to create and persist a `HandoffMessage` from the CLI, returning the handoff ID. |
| G4 | Implement `tag handoff list` to list handoffs filtered by status, agent, priority, or deadline, with `--json` output. |
| G5 | Implement `tag handoff accept <id>` to claim a pending handoff, transition it to `ACCEPTED`, and inject it into the target agent's queue (queue_jobs table) as a new job, retaining full context. |
| G6 | Implement `tag handoff status <id>` to retrieve the current state of a handoff with full provenance metadata, with `--json` output. |
| G7 | Implement `tag handoff reject <id>` and `tag handoff cancel <id>` for explicit lifecycle management. |
| G8 | Integrate handoff lifecycle events as OpenTelemetry spans (PRD-013) so handoff delegation chains appear in `tag trace`. |
| G9 | All handoff state transitions are atomic SQLite transactions with WAL mode; no handoff can be accepted by two concurrent workers simultaneously (SQLite exclusive row-level lock via `BEGIN IMMEDIATE`). |
| G10 | `tag handoff send` validates that `--to` references an existing profile (or uses `--force` to skip validation), preventing silent misroutes. |
| G11 | Priority supports a five-level enum (`critical`, `high`, `normal`, `low`, `background`) mapping to integer weights 1–5 compatible with the existing `queue_jobs.priority` column. |
| G12 | Deadline is stored as an ISO 8601 UTC timestamp; the dispatcher warns (but does not reject) when a handoff is accepted after its deadline. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | **Automatic agent discovery or capability matching.** `tag handoff send --to coder` requires the caller to know the target profile name. Semantic capability matching ("find an agent that can write Python") is a future PRD. |
| NG2 | **Cross-machine or cross-process network transport.** Handoffs are stored in the local SQLite database. Distributed delivery over A2A, ACP, or gRPC transport is a separate protocol adapter PRD. |
| NG3 | **Replacing `tag swarm` or `tag queue`.** Handoffs are an additional coordination primitive, not a replacement. Swarm creates kanban boards; queues run background jobs; handoffs model explicit inter-agent delegation intent. |
| NG4 | **Full conversation history transfer.** AutoGen's `HandoffMessage.context` is the model's LLM message history; TAG's `HandoffMessage.context` is a free-form JSON metadata blob. Full context window transfer (like OpenAI's `handoff()`) is out of scope for this PRD. |
| NG5 | **Automatic retry with backoff.** If an accepted handoff fails (the queue job it spawns exits non-zero), the handoff transitions to `FAILED`. Retry scheduling is handled by the existing queue worker (PRD-008), not the handoff layer. |
| NG6 | **UI/TUI visualization of handoff graphs.** `tag handoff list --json` provides the data; graph rendering belongs in PRD-054 (local browser DevUI). |
| NG7 | **Security sandboxing of handoff context payloads.** The `context` field is trusted caller input. Secret scanning (PRD-034) applies to task text; full context sandboxing is out of scope. |

---

## 5. Success Metrics

| Metric | Target | How Measured |
|--------|--------|--------------|
| Handoff creation latency | `tag handoff send` completes in < 50ms (p95) | `time tag handoff send ...` over 100 iterations |
| Acceptance atomicity | Zero duplicate acceptances under 20 concurrent `tag handoff accept` calls on the same ID | Concurrent pytest with `ThreadPoolExecutor(20)` |
| Queue injection fidelity | 100% of accepted handoffs appear as `queue_jobs` rows within 100ms | Integration test: accept → query queue_jobs |
| Trace integration | Every handoff lifecycle event (SEND, ACCEPT, COMPLETE, REJECT, CANCEL) appears as a child span under the originating trace | `tag trace show` with handoff-linked run ID |
| Profile validation | `tag handoff send --to nonexistent` exits non-zero with a human-readable error | Unit test |
| Priority ordering | `tag handoff list --pending --to coder` returns messages ordered by (priority ASC, deadline ASC, created_at ASC) | SQL query result ordering assertion |
| Deadline warning | Accepting a past-deadline handoff prints a visible warning but still succeeds | Integration test with `deadline = now() - 1h` |
| JSON output conformance | `--json` output on all subcommands passes `jsonschema` validation against the `HandoffMessage` schema | Unit test |

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
| U10 | CI pipeline | run `tag handoff list --pending --to coder --json \| jq '.[0].id'` and then `tag handoff accept <id>` | a scripted agent worker can implement a polling-and-processing loop without custom Python code |

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
| FR-01 | `HandoffMessage` is a Python `@dataclass(frozen=False)` with fields: `id: str`, `from_agent: str`, `to_agent: str`, `task: str`, `context: dict`, `priority: HandoffPriority`, `deadline: datetime \| None`, `status: HandoffStatus`, `created_at: datetime`, `accepted_at: datetime \| None`, `completed_at: datetime \| None`, `queue_job_id: str \| None`, `trace_id: str \| None`, `metadata: dict`, `reject_reason: str \| None`. | Must |
| FR-02 | `HandoffStatus` is a `str` enum with values: `PENDING`, `ACCEPTED`, `COMPLETED`, `REJECTED`, `CANCELLED`, `EXPIRED`. State machine: `PENDING → {ACCEPTED, REJECTED, CANCELLED, EXPIRED}`; `ACCEPTED → {COMPLETED, FAILED}`; all others are terminal. | Must |
| FR-03 | `HandoffPriority` is a `str` enum with values `critical`, `high`, `normal`, `low`, `background` mapping to integer weights 1, 2, 3, 4, 5 respectively — compatible with the `queue_jobs.priority` INTEGER column. | Must |
| FR-04 | IDs are generated with a `hnd_` prefix followed by a ULID (26-character base32 Crockford encoding, monotonic, sortable by creation time). No external dependency: generate via `uuid.uuid4().hex` encoded to match the existing `run_id` pattern in `controller.py`, or via a ULID library if already present. | Must |
| FR-05 | A new `handoffs` table is created in `open_db()` via `CREATE TABLE IF NOT EXISTS handoffs (...)` within an idempotent `executescript` block. Schema migration via `ALTER TABLE ADD COLUMN` for future additions, following the existing `_migrate_*` pattern. | Must |
| FR-06 | `tag handoff accept` uses `BEGIN IMMEDIATE` transaction to lock the row during the `status = PENDING` check and `status = ACCEPTED` update atomically. Any concurrent acceptor that loses the lock receives the "already ACCEPTED" error message and exits with code 4. | Must |
| FR-07 | When `tag handoff accept` succeeds, it inserts a row into `queue_jobs` with: `profile = handoff.to_agent`, `task = handoff.task`, `task_type = 'handoff'`, `priority = handoff.priority.weight`, `status = 'queued'`, and `deps_json = '[]'`. The inserted `queue_jobs.id` is stored back into `handoffs.queue_job_id`. | Must |
| FR-08 | The `context` field is serialized as JSON text in SQLite (`context_json TEXT`). `tag handoff send --context` validates that the argument is valid JSON before insertion; invalid JSON causes exit code 2 with a descriptive error. | Must |
| FR-09 | `tag handoff list` default ordering is `ORDER BY priority_weight ASC, deadline ASC NULLS LAST, created_at ASC`. This ordering ensures critical tasks surface first, then by deadline urgency, then by age. | Must |
| FR-10 | `tag handoff list --pending` is syntactic sugar for `--status PENDING`. Both forms are supported. | Should |
| FR-11 | Deadline validation: `--deadline` accepts ISO 8601 strings parseable by `datetime.fromisoformat()` (Python 3.11+). If the supplied deadline is already in the past at send-time, the CLI prints a warning but does not reject (agents may pre-stage handoffs for delayed dispatch). | Should |
| FR-12 | `tag handoff expire` queries `SELECT id FROM handoffs WHERE status='PENDING' AND deadline < <now_utc>` and bulk-updates them to `status='EXPIRED'` in a single transaction. With `--dry-run`, only `SELECT` is executed. | Should |
| FR-13 | Every status transition emits an OpenTelemetry span (via the existing `tracing.py` module, PRD-013) with span name `handoff.<transition>` (e.g., `handoff.send`, `handoff.accept`, `handoff.reject`) and attributes `handoff.id`, `handoff.from_agent`, `handoff.to_agent`, `handoff.priority`, `handoff.status`. | Should |
| FR-14 | `--trace-id` in `tag handoff send` stores the supplied trace ID in `handoffs.trace_id`, linking the handoff to an existing `tag run` trace for end-to-end observability. | Should |
| FR-15 | `tag handoff status <id>` for an `ACCEPTED` handoff performs a `LEFT JOIN` with `queue_jobs` on `queue_job_id` and returns the nested `queue_job` object in the JSON response, including current `status` and `exit_code`. | Should |
| FR-16 | The `--from` flag in `tag handoff send` validates against existing profiles with a warning (not an error) — agent names in handoffs can be external/remote agents not registered as TAG profiles. `--to` validation is strict by default (error, not warning) since we must enqueue into a known profile. | Should |
| FR-17 | `tag handoff cancel` only transitions `PENDING → CANCELLED`. Attempting to cancel an `ACCEPTED` handoff exits with code 5 and a message directing the user to `tag handoff reject`. | Must |
| FR-18 | `tag handoff reject` transitions `PENDING → REJECTED` or `ACCEPTED → REJECTED`. When rejecting an `ACCEPTED` handoff with a `queue_job_id`, the corresponding `queue_jobs` row is updated to `status='cancelled'` in the same transaction. | Must |
| FR-19 | The `teams.py` module exposes a `HandoffDispatcher` class with `send()`, `accept()`, `reject()`, `cancel()`, `list_pending()`, and `expire_overdue()` methods. The `controller.py` `cmd_handoff` function delegates to `HandoffDispatcher` — no direct SQL in `cmd_handoff`. | Must |
| FR-20 | `--json` on all subcommands outputs valid JSON to stdout. Human-readable output goes to stdout; error messages go to stderr. Zero human-readable output when `--json` is specified (no "Created:" prefix lines). | Must |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Latency.** `tag handoff send` (including SQLite write + OTel span) completes in < 50ms p95 on a MacBook with the DB on local SSD. | < 50ms p95 |
| NFR-02 | **Concurrency safety.** `tag handoff accept` with 100 concurrent callers on the same handoff ID must result in exactly 1 successful acceptance and 99 rejections. No deadlocks. | 100% correct |
| NFR-03 | **DB size.** Each `handoffs` row consumes < 4KB on average, allowing 250,000 handoffs in a 1GB database. Context payloads > 64KB are rejected with exit code 6. | < 4KB/row |
| NFR-04 | **No new mandatory dependencies.** `src/tag/teams.py` imports only from the Python standard library and modules already in `pyproject.toml` (`sqlite3`, `uuid`, `datetime`, `json`, `dataclasses`, `enum`). No new `pip install` required for the core feature. | Stdlib only |
| NFR-05 | **Backward compatibility.** Adding `handoffs` to `open_db()` is idempotent (`CREATE TABLE IF NOT EXISTS`). Existing databases are migrated non-destructively. `tag` commands other than `handoff` are unaffected. | 100% |
| NFR-06 | **Test coverage.** All 8 public methods on `HandoffDispatcher` have unit tests using an in-memory SQLite database (`":memory:"`). The `accept` concurrency test uses `ThreadPoolExecutor`. | >= 90% branch |
| NFR-07 | **JSON schema stability.** The `HandoffMessage` JSON output schema is versioned (`"schema_version": 1` in the root object) to enable forward-compatible parsing by external tools. | v1 stable |
| NFR-08 | **OTel span overhead.** OTel span creation in `tracing.py` adds < 1ms per operation (consistent with existing span timing in PRD-013 benchmarks). | < 1ms |
| NFR-09 | **Error messages.** All user-facing errors include the handoff ID, the current status, and a suggested next action (e.g., "Use `tag handoff reject` to reject an accepted handoff"). | Human-readable |
| NFR-10 | **Context size validation.** The `--context` JSON blob is validated to be ≤ 65,536 bytes (64KB) before insertion. Larger payloads are rejected with exit code 6 and a clear message. | 64KB limit |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/teams.py` | `HandoffMessage`, `HandoffStatus`, `HandoffPriority` dataclasses and enums; `HandoffDispatcher` class with all persistence and state-machine logic. |
| `tests/test_handoff.py` | Unit and integration tests for `HandoffDispatcher` and `cmd_handoff`. |

### 10.2 Modified Files

| File | Change |
|------|--------|
| `src/tag/controller.py` | Add `handoffs` table DDL to `open_db()`. Add `cmd_handoff()` function. Register `handoff` subparser in `main()`. |

### 10.3 SQLite DDL

The following DDL is added inside the idempotent `conn.executescript(...)` block in `open_db()`:

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

### 10.4 Core Dataclasses and Enums (`src/tag/teams.py`)

```python
from __future__ import annotations

import json
import sqlite3
import uuid
from dataclasses import dataclass, field, asdict
from datetime import datetime, timezone
from enum import Enum
from typing import Any


class HandoffStatus(str, Enum):
    PENDING   = "PENDING"
    ACCEPTED  = "ACCEPTED"
    COMPLETED = "COMPLETED"
    REJECTED  = "REJECTED"
    CANCELLED = "CANCELLED"
    EXPIRED   = "EXPIRED"
    FAILED    = "FAILED"

    # Terminal states — no further transitions allowed
    TERMINAL = frozenset({COMPLETED, REJECTED, CANCELLED, EXPIRED, FAILED})


class HandoffPriority(str, Enum):
    CRITICAL   = "critical"
    HIGH       = "high"
    NORMAL     = "normal"
    LOW        = "low"
    BACKGROUND = "background"

    @property
    def weight(self) -> int:
        return {
            "critical":   1,
            "high":       2,
            "normal":     3,
            "low":        4,
            "background": 5,
        }[self.value]


@dataclass
class HandoffMessage:
    id:           str
    from_agent:   str
    to_agent:     str
    task:         str
    context:      dict[str, Any]      = field(default_factory=dict)
    priority:     HandoffPriority     = HandoffPriority.NORMAL
    deadline:     datetime | None     = None
    status:       HandoffStatus       = HandoffStatus.PENDING
    created_at:   datetime            = field(default_factory=lambda: datetime.now(timezone.utc))
    accepted_at:  datetime | None     = None
    completed_at: datetime | None     = None
    queue_job_id: str | None          = None
    trace_id:     str | None          = None
    reject_reason: str | None         = None
    metadata:     dict[str, Any]      = field(default_factory=dict)

    def to_json_dict(self) -> dict[str, Any]:
        """Serialize to a JSON-safe dict for CLI output."""
        return {
            "id":             self.id,
            "from_agent":     self.from_agent,
            "to_agent":       self.to_agent,
            "task":           self.task,
            "context":        self.context,
            "priority":       self.priority.value,
            "priority_weight": self.priority.weight,
            "deadline":       self.deadline.isoformat() if self.deadline else None,
            "status":         self.status.value,
            "created_at":     self.created_at.isoformat(),
            "accepted_at":    self.accepted_at.isoformat() if self.accepted_at else None,
            "completed_at":   self.completed_at.isoformat() if self.completed_at else None,
            "queue_job_id":   self.queue_job_id,
            "trace_id":       self.trace_id,
            "reject_reason":  self.reject_reason,
            "metadata":       self.metadata,
            "schema_version": 1,
        }

    @staticmethod
    def generate_id() -> str:
        """Generate a hnd_-prefixed unique ID compatible with TAG's ID conventions."""
        return f"hnd_{uuid.uuid4().hex[:26]}"

    @staticmethod
    def from_row(row: sqlite3.Row) -> "HandoffMessage":
        """Deserialize from a SQLite Row object."""
        def _dt(s: str | None) -> datetime | None:
            return datetime.fromisoformat(s) if s else None

        return HandoffMessage(
            id=row["id"],
            from_agent=row["from_agent"],
            to_agent=row["to_agent"],
            task=row["task"],
            context=json.loads(row["context_json"] or "{}"),
            priority=HandoffPriority(row["priority"]),
            deadline=_dt(row["deadline"]),
            status=HandoffStatus(row["status"]),
            created_at=datetime.fromisoformat(row["created_at"]),
            accepted_at=_dt(row["accepted_at"]),
            completed_at=_dt(row["completed_at"]),
            queue_job_id=row["queue_job_id"],
            trace_id=row["trace_id"],
            reject_reason=row["reject_reason"],
            metadata=json.loads(row["metadata_json"] or "{}"),
        )
```

### 10.5 `HandoffDispatcher` Class (`src/tag/teams.py`)

```python
_MAX_CONTEXT_BYTES = 65_536  # 64KB

class HandoffDispatcher:
    """Persistence and state-machine logic for HandoffMessage objects.

    All methods accept an open sqlite3.Connection (WAL mode, from open_db()).
    No SQL appears in controller.py cmd_handoff.
    """

    def __init__(self, conn: sqlite3.Connection) -> None:
        self.conn = conn

    def send(
        self,
        *,
        from_agent: str,
        to_agent: str,
        task: str,
        context: dict[str, Any] | None = None,
        priority: HandoffPriority = HandoffPriority.NORMAL,
        deadline: datetime | None = None,
        trace_id: str | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> HandoffMessage:
        ctx = context or {}
        ctx_json = json.dumps(ctx)
        if len(ctx_json.encode()) > _MAX_CONTEXT_BYTES:
            raise ValueError(
                f"context payload exceeds {_MAX_CONTEXT_BYTES} bytes "
                f"({len(ctx_json.encode())} bytes). Reduce context size."
            )

        hnd = HandoffMessage(
            id=HandoffMessage.generate_id(),
            from_agent=from_agent,
            to_agent=to_agent,
            task=task,
            context=ctx,
            priority=priority,
            deadline=deadline,
            trace_id=trace_id,
            metadata=metadata or {},
        )

        self.conn.execute(
            """
            INSERT INTO handoffs (
              id, from_agent, to_agent, task, context_json,
              priority, priority_weight, deadline, status,
              created_at, trace_id, metadata_json
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                hnd.id, hnd.from_agent, hnd.to_agent, hnd.task,
                ctx_json, hnd.priority.value, hnd.priority.weight,
                hnd.deadline.isoformat() if hnd.deadline else None,
                hnd.status.value, hnd.created_at.isoformat(),
                hnd.trace_id, json.dumps(hnd.metadata),
            ),
        )
        self.conn.commit()
        return hnd

    def accept(self, handoff_id: str, *, profile: str | None = None) -> tuple[HandoffMessage, str]:
        """
        Atomically transition PENDING → ACCEPTED and insert a queue_jobs row.

        Returns (updated_handoff, queue_job_id).
        Raises HandoffStateError if handoff is not PENDING.
        Uses BEGIN IMMEDIATE to prevent concurrent double-acceptance.
        """
        now = datetime.now(timezone.utc).isoformat()

        with self.conn:  # BEGIN / COMMIT
            self.conn.execute("BEGIN IMMEDIATE")

            row = self.conn.execute(
                "SELECT * FROM handoffs WHERE id = ?", (handoff_id,)
            ).fetchone()
            if row is None:
                raise KeyError(f"Handoff {handoff_id!r} not found")
            hnd = HandoffMessage.from_row(row)

            if hnd.status is not HandoffStatus.PENDING:
                raise HandoffStateError(
                    handoff_id=handoff_id,
                    current_status=hnd.status,
                    message=f"Cannot accept: handoff is already {hnd.status.value}",
                )

            effective_profile = profile or hnd.to_agent
            job_id = f"qjob_{uuid.uuid4().hex[:12]}"

            self.conn.execute(
                """
                INSERT INTO queue_jobs
                  (id, profile, task, task_type, status, priority, created_at, notify, deps_json)
                VALUES (?, ?, ?, 'handoff', 'queued', ?, ?, 1, '[]')
                """,
                (job_id, effective_profile, hnd.task, hnd.priority.weight, now),
            )

            self.conn.execute(
                """
                UPDATE handoffs
                   SET status = 'ACCEPTED',
                       accepted_at = ?,
                       queue_job_id = ?
                 WHERE id = ?
                """,
                (now, job_id, handoff_id),
            )

        # Re-fetch after commit
        row = self.conn.execute(
            "SELECT * FROM handoffs WHERE id = ?", (handoff_id,)
        ).fetchone()
        return HandoffMessage.from_row(row), job_id

    def reject(self, handoff_id: str, *, reason: str | None = None) -> HandoffMessage:
        """Transition PENDING or ACCEPTED → REJECTED. Cancels linked queue job if present."""
        now = datetime.now(timezone.utc).isoformat()
        with self.conn:
            row = self.conn.execute(
                "SELECT * FROM handoffs WHERE id = ?", (handoff_id,)
            ).fetchone()
            if row is None:
                raise KeyError(f"Handoff {handoff_id!r} not found")
            hnd = HandoffMessage.from_row(row)

            if hnd.status not in (HandoffStatus.PENDING, HandoffStatus.ACCEPTED):
                raise HandoffStateError(
                    handoff_id=handoff_id,
                    current_status=hnd.status,
                    message=f"Cannot reject: handoff is in terminal state {hnd.status.value}",
                )

            if hnd.queue_job_id:
                self.conn.execute(
                    "UPDATE queue_jobs SET status='cancelled' WHERE id=? AND status='queued'",
                    (hnd.queue_job_id,),
                )

            self.conn.execute(
                "UPDATE handoffs SET status='REJECTED', reject_reason=? WHERE id=?",
                (reason, handoff_id),
            )

        row = self.conn.execute(
            "SELECT * FROM handoffs WHERE id = ?", (handoff_id,)
        ).fetchone()
        return HandoffMessage.from_row(row)

    def cancel(self, handoff_id: str, *, reason: str | None = None) -> HandoffMessage:
        """Transition PENDING → CANCELLED. Only the sender can cancel before acceptance."""
        row = self.conn.execute(
            "SELECT * FROM handoffs WHERE id = ?", (handoff_id,)
        ).fetchone()
        if row is None:
            raise KeyError(f"Handoff {handoff_id!r} not found")
        hnd = HandoffMessage.from_row(row)
        if hnd.status is not HandoffStatus.PENDING:
            raise HandoffStateError(
                handoff_id=handoff_id,
                current_status=hnd.status,
                message=(
                    f"Cannot cancel: handoff is {hnd.status.value}. "
                    "Use `tag handoff reject` to reject an already-accepted handoff."
                ),
            )
        self.conn.execute(
            "UPDATE handoffs SET status='CANCELLED', reject_reason=? WHERE id=?",
            (reason, handoff_id),
        )
        self.conn.commit()
        row = self.conn.execute(
            "SELECT * FROM handoffs WHERE id = ?", (handoff_id,)
        ).fetchone()
        return HandoffMessage.from_row(row)

    def list_pending(
        self,
        *,
        to_agent: str | None = None,
        from_agent: str | None = None,
        status: HandoffStatus | None = HandoffStatus.PENDING,
        priority: HandoffPriority | None = None,
        since: datetime | None = None,
        until: datetime | None = None,
        limit: int = 50,
    ) -> list[HandoffMessage]:
        query = "SELECT * FROM handoffs WHERE 1=1"
        params: list[Any] = []
        if status:
            query += " AND status = ?"
            params.append(status.value)
        if to_agent:
            query += " AND to_agent = ?"
            params.append(to_agent)
        if from_agent:
            query += " AND from_agent = ?"
            params.append(from_agent)
        if priority:
            query += " AND priority = ?"
            params.append(priority.value)
        if since:
            query += " AND created_at >= ?"
            params.append(since.isoformat())
        if until:
            query += " AND created_at <= ?"
            params.append(until.isoformat())
        query += " ORDER BY priority_weight ASC, deadline ASC NULLS LAST, created_at ASC"
        query += f" LIMIT {int(limit)}"
        rows = self.conn.execute(query, params).fetchall()
        return [HandoffMessage.from_row(r) for r in rows]

    def get(self, handoff_id: str) -> HandoffMessage:
        row = self.conn.execute(
            "SELECT * FROM handoffs WHERE id = ?", (handoff_id,)
        ).fetchone()
        if row is None:
            raise KeyError(f"Handoff {handoff_id!r} not found")
        return HandoffMessage.from_row(row)

    def expire_overdue(self, *, dry_run: bool = False) -> list[str]:
        """Mark past-deadline PENDING handoffs as EXPIRED. Returns list of expired IDs."""
        now = datetime.now(timezone.utc).isoformat()
        rows = self.conn.execute(
            "SELECT id FROM handoffs WHERE status='PENDING' AND deadline IS NOT NULL AND deadline < ?",
            (now,),
        ).fetchall()
        ids = [r["id"] for r in rows]
        if not dry_run and ids:
            placeholders = ",".join("?" * len(ids))
            self.conn.execute(
                f"UPDATE handoffs SET status='EXPIRED' WHERE id IN ({placeholders})",
                ids,
            )
            self.conn.commit()
        return ids


class HandoffStateError(Exception):
    def __init__(self, *, handoff_id: str, current_status: HandoffStatus, message: str) -> None:
        super().__init__(message)
        self.handoff_id = handoff_id
        self.current_status = current_status
```

### 10.6 `cmd_handoff` in `controller.py`

```python
def cmd_handoff(args: argparse.Namespace) -> int:
    """PRD-085: Formal HandoffMessage Primitive for decentralized agent routing."""
    from tag.teams import HandoffDispatcher, HandoffPriority, HandoffStatus, HandoffStateError
    import json as _json

    cfg = load_config()
    db = open_db(cfg)
    dispatcher = HandoffDispatcher(db)
    sub = args.handoff_sub  # set by subparser set_defaults

    if sub == "send":
        # Validate context JSON
        ctx: dict = {}
        if args.context:
            try:
                ctx = _json.loads(args.context)
                if not isinstance(ctx, dict):
                    raise ValueError("context must be a JSON object")
            except (ValueError, _json.JSONDecodeError) as exc:
                print_error(f"Invalid --context JSON: {exc}")
                return 2

        # Validate deadline
        deadline = None
        if args.deadline:
            try:
                deadline = datetime.fromisoformat(args.deadline.rstrip("Z")).replace(
                    tzinfo=timezone.utc
                )
                if deadline < datetime.now(timezone.utc):
                    print_warning("Warning: --deadline is already in the past")
            except ValueError as exc:
                print_error(f"Invalid --deadline: {exc}. Use ISO 8601 format, e.g. 2026-06-18T09:00:00Z")
                return 3

        # Validate target profile (unless --force)
        if not args.force:
            profiles = load_profiles(cfg)
            if args.to not in profiles:
                print_error(
                    f"Profile {args.to!r} not found. "
                    "Use --force to send to an unregistered agent."
                )
                return 1

        try:
            hnd = dispatcher.send(
                from_agent=args.from_agent,
                to_agent=args.to,
                task=args.task,
                context=ctx,
                priority=HandoffPriority(args.priority),
                deadline=deadline,
                trace_id=getattr(args, "trace_id", None),
            )
        except ValueError as exc:
            print_error(str(exc))
            return 6

        if args.json:
            print(_json.dumps(hnd.to_json_dict(), indent=2))
        else:
            print(f"Handoff created: {hnd.id}")
            print(f"  from:     {hnd.from_agent}")
            print(f"  to:       {hnd.to_agent}")
            print(f"  priority: {hnd.priority.value}")
            print(f"  deadline: {hnd.deadline.isoformat() if hnd.deadline else '—'}")
            print(f"  status:   {hnd.status.value}")
        return 0

    elif sub == "list":
        status_filter = None
        if args.pending:
            status_filter = HandoffStatus.PENDING
        elif args.status:
            status_filter = HandoffStatus(args.status)

        hnds = dispatcher.list_pending(
            to_agent=args.to,
            from_agent=args.from_agent,
            status=status_filter,
            priority=HandoffPriority(args.priority) if args.priority else None,
            limit=args.limit,
        )

        if args.json:
            print(_json.dumps([h.to_json_dict() for h in hnds], indent=2))
        else:
            if not hnds:
                print("No handoffs found.")
                return 0
            header = f"{'ID':<32}  {'FROM':<14}  {'TO':<14}  {'PRIORITY':<10}  {'DEADLINE':<22}  STATUS"
            print(header)
            print("-" * len(header))
            for h in hnds:
                dl = h.deadline.isoformat() if h.deadline else "—"
                short_id = h.id[:30] + ".."
                print(f"{short_id:<32}  {h.from_agent:<14}  {h.to_agent:<14}  {h.priority.value:<10}  {dl:<22}  {h.status.value}")
        return 0

    elif sub == "accept":
        try:
            hnd, job_id = dispatcher.accept(
                args.handoff_id,
                profile=getattr(args, "profile", None),
            )
        except KeyError as exc:
            print_error(str(exc))
            return 1
        except HandoffStateError as exc:
            print_error(str(exc))
            return 4

        if args.json:
            print(_json.dumps({
                "id": hnd.id,
                "status": hnd.status.value,
                "accepted_at": hnd.accepted_at.isoformat() if hnd.accepted_at else None,
                "queue_job_id": job_id,
                "profile": hnd.to_agent,
            }, indent=2))
        else:
            print(f"Accepted handoff: {hnd.id}")
            print(f"  Queue job: {job_id}")
            print(f"  Profile:   {hnd.to_agent}")
            print(f"  Status:    {hnd.status.value}")
        return 0

    elif sub == "status":
        try:
            hnd = dispatcher.get(args.handoff_id)
        except KeyError as exc:
            print_error(str(exc))
            return 1

        result = hnd.to_json_dict()
        if hnd.queue_job_id:
            job_row = db.execute(
                "SELECT id, status, exit_code, started_at, finished_at FROM queue_jobs WHERE id=?",
                (hnd.queue_job_id,),
            ).fetchone()
            if job_row:
                result["queue_job"] = dict(job_row)

        if args.json:
            print(_json.dumps(result, indent=2))
        else:
            print(f"Handoff: {hnd.id}")
            print(f"  from:        {hnd.from_agent}")
            print(f"  to:          {hnd.to_agent}")
            print(f"  status:      {hnd.status.value}")
            print(f"  priority:    {hnd.priority.value}")
            if hnd.queue_job_id:
                print(f"  queue_job:   {hnd.queue_job_id}")
        return 0

    elif sub == "reject":
        try:
            hnd = dispatcher.reject(args.handoff_id, reason=getattr(args, "reason", None))
        except KeyError as exc:
            print_error(str(exc))
            return 1
        except HandoffStateError as exc:
            print_error(str(exc))
            return 4

        if args.json:
            print(_json.dumps(hnd.to_json_dict(), indent=2))
        else:
            print(f"Rejected handoff: {hnd.id}")
            if hnd.reject_reason:
                print(f"  Reason: {hnd.reject_reason}")
        return 0

    elif sub == "cancel":
        try:
            hnd = dispatcher.cancel(args.handoff_id, reason=getattr(args, "reason", None))
        except KeyError as exc:
            print_error(str(exc))
            return 1
        except HandoffStateError as exc:
            print_error(str(exc))
            return 5

        if args.json:
            print(_json.dumps(hnd.to_json_dict(), indent=2))
        else:
            print(f"Cancelled handoff: {hnd.id}")
        return 0

    elif sub == "expire":
        ids = dispatcher.expire_overdue(dry_run=args.dry_run)
        if args.json:
            print(_json.dumps({"expired": ids, "count": len(ids), "dry_run": args.dry_run}))
        else:
            if args.dry_run:
                print(f"Would expire {len(ids)} handoff(s):")
            else:
                print(f"Expired {len(ids)} handoff(s):")
            for hid in ids:
                print(f"  {hid}")
        return 0

    else:
        print_error(f"Unknown handoff subcommand: {sub!r}")
        return 1
```

### 10.7 Argparse Registration in `main()`

```python
# ---- PRD-085: handoff ----
handoff_p = sub.add_parser("handoff", help="Formal inter-agent HandoffMessage routing (PRD-085)")
handoff_sub = handoff_p.add_subparsers(dest="handoff_sub")

# send
ho_send = handoff_sub.add_parser("send", help="Create and persist a HandoffMessage")
ho_send.add_argument("--from", dest="from_agent", required=True, help="Originating agent profile name")
ho_send.add_argument("--to", required=True, help="Target agent profile name")
ho_send.add_argument("--task", required=True, help="Task description")
ho_send.add_argument("--context", default=None, help="JSON object with arbitrary metadata")
ho_send.add_argument("--priority", default="normal",
                     choices=["critical", "high", "normal", "low", "background"])
ho_send.add_argument("--deadline", default=None, help="ISO 8601 UTC deadline (e.g. 2026-06-18T09:00:00Z)")
ho_send.add_argument("--force", action="store_true", help="Skip target profile validation")
ho_send.add_argument("--trace-id", dest="trace_id", default=None)
ho_send.add_argument("--json", action="store_true")
ho_send.set_defaults(func=cmd_handoff, handoff_sub="send")

# list
ho_list = handoff_sub.add_parser("list", help="List HandoffMessages with filtering")
ho_list.add_argument("--pending", action="store_true", help="Shorthand for --status PENDING")
ho_list.add_argument("--status", choices=["PENDING","ACCEPTED","COMPLETED","REJECTED","CANCELLED","EXPIRED","FAILED"])
ho_list.add_argument("--to", default=None, help="Filter by target agent")
ho_list.add_argument("--from", dest="from_agent", default=None, help="Filter by source agent")
ho_list.add_argument("--priority", default=None,
                     choices=["critical", "high", "normal", "low", "background"])
ho_list.add_argument("--since", default=None)
ho_list.add_argument("--until", default=None)
ho_list.add_argument("--limit", type=int, default=50)
ho_list.add_argument("--json", action="store_true")
ho_list.set_defaults(func=cmd_handoff, handoff_sub="list")

# accept
ho_accept = handoff_sub.add_parser("accept", help="Accept a pending HandoffMessage and enqueue it")
ho_accept.add_argument("handoff_id", help="Handoff ID (hnd_...)")
ho_accept.add_argument("--profile", default=None)
ho_accept.add_argument("--no-enqueue", action="store_true", dest="no_enqueue")
ho_accept.add_argument("--json", action="store_true")
ho_accept.set_defaults(func=cmd_handoff, handoff_sub="accept")

# status
ho_status = handoff_sub.add_parser("status", help="Get full status of a HandoffMessage")
ho_status.add_argument("handoff_id", help="Handoff ID (hnd_...)")
ho_status.add_argument("--json", action="store_true")
ho_status.set_defaults(func=cmd_handoff, handoff_sub="status")

# reject
ho_reject = handoff_sub.add_parser("reject", help="Reject a pending or accepted HandoffMessage")
ho_reject.add_argument("handoff_id")
ho_reject.add_argument("--reason", default=None)
ho_reject.add_argument("--json", action="store_true")
ho_reject.set_defaults(func=cmd_handoff, handoff_sub="reject")

# cancel
ho_cancel = handoff_sub.add_parser("cancel", help="Cancel a pending HandoffMessage (sender only)")
ho_cancel.add_argument("handoff_id")
ho_cancel.add_argument("--reason", default=None)
ho_cancel.add_argument("--json", action="store_true")
ho_cancel.set_defaults(func=cmd_handoff, handoff_sub="cancel")

# expire
ho_expire = handoff_sub.add_parser("expire", help="Mark past-deadline PENDING handoffs as EXPIRED")
ho_expire.add_argument("--dry-run", action="store_true")
ho_expire.add_argument("--json", action="store_true")
ho_expire.set_defaults(func=cmd_handoff, handoff_sub="expire")

handoff_p.set_defaults(func=cmd_handoff)
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

Each `HandoffDispatcher` method emits an OTel span via the existing `tracing.py` helpers (PRD-013):

```python
# In HandoffDispatcher.send():
with _tracer.start_as_current_span("handoff.send") as span:
    span.set_attribute("handoff.id", hnd.id)
    span.set_attribute("handoff.from_agent", hnd.from_agent)
    span.set_attribute("handoff.to_agent", hnd.to_agent)
    span.set_attribute("handoff.priority", hnd.priority.value)
    span.set_attribute("handoff.status", hnd.status.value)
    # ... insert into DB ...
```

The `trace_id` stored in `handoffs.trace_id` is the W3C TraceContext `trace-id` of the originating run, enabling `tag trace show <trace_id>` to display the full delegation chain including handoff spans.

### 10.10 Integration with Queue Worker (PRD-008)

When `HandoffDispatcher.accept()` inserts a `queue_jobs` row with `task_type='handoff'`, the existing queue worker (`queue_worker.py`, PRD-008) picks it up on its next polling cycle with no modifications. The `task` column contains the handoff task description; the `profile` column contains the accepting agent's profile. The queue worker is unaware of the handoff layer — it sees a normal queued job.

When the queue job completes (`exit_code=0`), a future enhancement (see Open Questions) can hook the queue worker to update `handoffs.status = 'COMPLETED'` and set `completed_at`. For this PRD, `COMPLETED` state is set by the caller via `tag handoff status` polling or a direct `UPDATE`.

### 10.11 Comparison to Prior Art

| Concept | OpenAI Agents SDK | AutoGen Swarm | A2A v1.0 | TAG HandoffMessage |
|---------|------------------|---------------|----------|--------------------|
| Transfer mechanism | `handoff()` → `transfer_to_<name>` tool | `HandoffMessage(source, target, content, context)` | Task.send() JSON-RPC | `tag handoff send --from ... --to ...` |
| Context semantics | Full conversation history | LLM message history (model context) | Task payload (arbitrary parts) | Free-form JSON dict (metadata only) |
| Persistence | In-memory / LLM context | AutoGen runtime message list | A2A server DB | SQLite `handoffs` table |
| Routing | Agent name → agent lookup | Swarm scans last message for `.target` | Agent Card `/.well-known/agent-card.json` | Profile name → `queue_jobs` row |
| Atomicity | N/A (single process) | N/A (single process) | HTTP POST (idempotent via task ID) | SQLite `BEGIN IMMEDIATE` |
| CLI surface | None (API only) | None (API only) | None (protocol only) | Full `tag handoff` CLI |

---

## 11. Security Considerations

1. **Context payload trust.** The `context_json` field is persisted as caller-supplied JSON. It is not executed, sandboxed, or sanitized beyond the size limit (64KB, NFR-10). Secrets (API keys, tokens) in context are stored in plaintext in SQLite. Users should not put sensitive credentials in handoff context. The secret scanning integration (PRD-034) does not currently scan `context_json`; a future enhancement should add a `security.scan_handoff_context` config flag to opt-in to secret scanning of context payloads before insertion.

2. **Local-only SQLite access.** Handoffs are stored in `~/.tag/runtime/tag.sqlite3` (or the path from `cfg["runtime_dir"]`). Access control is filesystem-level: only the OS user who owns the file can read or write handoffs. There is no authentication layer on the local database. In multi-user deployments, each user has their own `~/.tag/` directory and isolated SQLite file.

3. **No network exposure.** `HandoffDispatcher` and `cmd_handoff` make no network calls. The `--to` profile lookup is entirely local. There is no risk of a handoff triggering an outbound connection to an attacker-controlled endpoint.

4. **Profile name injection.** The `--to` and `--from` values are stored verbatim as TEXT in SQLite using parameterized queries (no string interpolation). SQL injection is not possible. However, a malicious `--to` value like `'; DROP TABLE handoffs; --` is safely stored as a string and fails profile validation (FR-16) unless `--force` is passed.

5. **Race condition in concurrent acceptance.** The `BEGIN IMMEDIATE` transaction in `HandoffDispatcher.accept()` prevents two concurrent workers from both seeing `status=PENDING` and both succeeding. SQLite's WAL mode with `PRAGMA busy_timeout = 5000` (already set in `open_db()`) means contending callers wait up to 5 seconds before returning a lock timeout error rather than returning incorrect data.

6. **Deadline bypass.** Deadlines are advisory: `tag handoff accept` after a deadline warns but still succeeds. This is intentional — the agent may have valid reasons to accept an overdue handoff (e.g., the deadline was pessimistic). The `EXPIRED` transition via `tag handoff expire` is explicit, not automatic.

7. **Audit trail immutability.** Rows in the `handoffs` table are never `DELETE`d by any `tag handoff` command. Cancel and reject only update the `status` column. This preserves a complete audit trail. A future `tag handoff purge` command for GDPR compliance is out of scope for this PRD.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_handoff.py`)

All unit tests use an in-memory SQLite database created by calling `open_db()` with a temporary config pointing to `":memory:"`.

| Test | Description |
|------|-------------|
| `test_send_creates_row` | `dispatcher.send(...)` inserts exactly one row in `handoffs` with `status='PENDING'`. |
| `test_send_invalid_context_json` | `--context 'not json'` in `cmd_handoff` returns exit code 2. |
| `test_send_context_too_large` | Context dict that serializes to > 65536 bytes raises `ValueError` in `dispatcher.send()`. |
| `test_send_invalid_deadline` | `--deadline "not-a-date"` returns exit code 3. |
| `test_send_past_deadline_warns` | `--deadline <yesterday>` succeeds with exit code 0 but prints a warning to stderr. |
| `test_send_unknown_profile` | `--to nonexistent` without `--force` returns exit code 1. |
| `test_send_unknown_profile_with_force` | `--to nonexistent --force` returns exit code 0. |
| `test_accept_transitions_status` | `dispatcher.accept(id)` updates `status` to `ACCEPTED` and sets `accepted_at`. |
| `test_accept_creates_queue_job` | After `dispatcher.accept(id)`, a `queue_jobs` row exists with `profile=to_agent` and `task_type='handoff'`. |
| `test_accept_idempotent_fails_on_second_call` | Calling `dispatcher.accept(id)` twice raises `HandoffStateError` on second call. |
| `test_accept_concurrent_race` | `ThreadPoolExecutor(20)` with 20 threads all calling `dispatcher.accept(same_id)` results in exactly 1 success and 19 `HandoffStateError` exceptions. |
| `test_reject_from_pending` | `dispatcher.reject(id)` on a PENDING handoff transitions to REJECTED. |
| `test_reject_from_accepted_cancels_job` | `dispatcher.reject(id)` on an ACCEPTED handoff also sets `queue_jobs.status='cancelled'`. |
| `test_reject_from_terminal_raises` | `dispatcher.reject(id)` on COMPLETED raises `HandoffStateError`. |
| `test_cancel_from_pending` | `dispatcher.cancel(id)` on PENDING transitions to CANCELLED. |
| `test_cancel_from_accepted_raises` | `dispatcher.cancel(id)` on ACCEPTED raises `HandoffStateError` with message mentioning `tag handoff reject`. |
| `test_list_filters_by_status` | `dispatcher.list_pending(status=HandoffStatus.PENDING)` returns only PENDING rows. |
| `test_list_filters_by_to_agent` | `dispatcher.list_pending(to_agent="coder")` returns only rows where `to_agent='coder'`. |
| `test_list_ordering` | Rows are returned in `(priority_weight ASC, deadline ASC NULLS LAST, created_at ASC)` order. |
| `test_expire_overdue` | Handoffs with `deadline < now()` are transitioned to EXPIRED. Non-expired handoffs are untouched. |
| `test_expire_dry_run` | `expire_overdue(dry_run=True)` returns IDs but does not modify `status`. |
| `test_from_row_roundtrip` | `HandoffMessage.from_row(row).to_json_dict()` is stable through send→fetch cycle. |
| `test_json_output_schema_version` | `HandoffMessage.to_json_dict()["schema_version"]` is `1`. |

### 12.2 Integration Tests

| Test | Description |
|------|-------------|
| `test_send_accept_status_flow` | Full `send → list --pending → accept → status` CLI flow using `subprocess.run(["tag", "handoff", ...])` against a real temp DB. |
| `test_accept_queue_job_visible` | After `tag handoff accept`, `tag queue list` shows the new job with `task_type=handoff`. |
| `test_reject_with_reason` | `tag handoff reject <id> --reason "..."` stores reason; `tag handoff status <id> --json` returns it. |
| `test_expire_via_cli` | Create handoff with past deadline → `tag handoff expire` → `tag handoff status --json` shows EXPIRED. |
| `test_list_json_schema_valid` | `tag handoff list --json` output passes `jsonschema` validation against the `HandoffMessage` array schema. |

### 12.3 Performance Tests

| Test | Description | Target |
|------|-------------|--------|
| `bench_send_latency` | Time 100 sequential `tag handoff send` calls; report p50, p95, p99. | p95 < 50ms |
| `bench_list_1000` | Insert 1000 handoffs then `tag handoff list --limit 1000`; measure query time. | < 100ms |
| `bench_concurrent_accept` | 100 threads race to accept the same handoff; measure total wall time and verify exactly 1 success. | < 2s total |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag handoff send --from orchestrator --to coder --task "..." --priority high` exits 0 and prints a `hnd_`-prefixed ID. | `pytest test_send_creates_row` |
| AC-02 | The created handoff is visible in `tag handoff list --pending --json` as a JSON array element with `status="PENDING"`. | `pytest test_list_filters_by_status` |
| AC-03 | `tag handoff accept <id>` on a PENDING handoff exits 0, transitions to ACCEPTED, and creates a `queue_jobs` row visible in `tag queue list`. | `pytest test_accept_creates_queue_job`, integration test |
| AC-04 | A second concurrent `tag handoff accept <id>` on the same ID exits with code 4 and message "already ACCEPTED". | `pytest test_accept_concurrent_race` |
| AC-05 | `tag handoff status <id> --json` returns valid JSON with all `HandoffMessage` fields, `schema_version: 1`, and `queue_job` nested object if accepted. | `pytest test_json_output_schema_version` |
| AC-06 | `tag handoff reject <id> --reason "..."` transitions a PENDING or ACCEPTED handoff to REJECTED and stores the reason. | `pytest test_reject_from_pending` |
| AC-07 | Rejecting an ACCEPTED handoff also sets the linked `queue_jobs` row to `status='cancelled'`. | `pytest test_reject_from_accepted_cancels_job` |
| AC-08 | `tag handoff cancel <id>` on a PENDING handoff transitions to CANCELLED; on an ACCEPTED handoff exits code 5 with a message directing the user to `tag handoff reject`. | `pytest test_cancel_from_accepted_raises` |
| AC-09 | `tag handoff send --to nonexistent` (without `--force`) exits code 1 with a message naming the missing profile. | `pytest test_send_unknown_profile` |
| AC-10 | `tag handoff send --context 'not-json'` exits code 2 with a descriptive JSON parse error. | `pytest test_send_invalid_context_json` |
| AC-11 | `tag handoff send --deadline "not-a-date"` exits code 3. | `pytest test_send_invalid_deadline` |
| AC-12 | `tag handoff expire` marks all PENDING handoffs with `deadline < now()` as EXPIRED. Non-past-deadline handoffs are untouched. | `pytest test_expire_overdue` |
| AC-13 | `tag handoff expire --dry-run` prints IDs that would be expired without modifying `status`. | `pytest test_expire_dry_run` |
| AC-14 | Every `HandoffDispatcher` method emits an OTel span visible in `tag trace show` output. | Integration test with `tracing.py` mock |
| AC-15 | `open_db()` creates the `handoffs` table idempotently — calling it twice on the same DB raises no error. | `pytest test_open_db_idempotent` |
| AC-16 | `tag handoff send --context <payload exceeding 64KB>` exits code 6 with a clear message about the size limit. | `pytest test_send_context_too_large` |
| AC-17 | `tag handoff list --json` on an empty DB returns `[]`. | Unit test |
| AC-18 | `tag handoff list` (human-readable) renders a table with columns `ID`, `FROM`, `TO`, `PRIORITY`, `DEADLINE`, `STATUS`. | Integration test with `subprocess.run` |

---

## 14. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `sqlite3` | Stdlib | Python 3.10+ | Already used via `open_db()`. WAL mode, `BEGIN IMMEDIATE` semantics. |
| `uuid` | Stdlib | Python 3.10+ | Used for `hnd_` ID generation. |
| `datetime` | Stdlib | Python 3.10+ | `datetime.fromisoformat()` requires Python 3.11+ for full ISO 8601 (including `Z` suffix). Add `.replace("Z","")` shim for Python 3.10 compatibility. |
| `dataclasses` | Stdlib | Python 3.10+ | `@dataclass` for `HandoffMessage`. |
| `enum` | Stdlib | Python 3.10+ | `HandoffStatus`, `HandoffPriority` enums. |
| `json` | Stdlib | Python 3.10+ | Context serialization. |
| PRD-008 (queue_worker.py) | Internal | Current | `queue_jobs` table. `HandoffDispatcher.accept()` inserts into it. |
| PRD-013 (tracing.py) | Internal | Current | OTel span emission for lifecycle events. |
| PRD-028 (sandbox.py) | Internal | Current | No direct dependency; sandboxed runs can emit handoffs if the sandbox allows CLI access. |
| PRD-034 (security.py) | Internal | Current | Future: opt-in context payload secret scanning. Not in scope for this PRD. |
| PRD-037 (personas) | Internal | Current | `--from` / `--to` can reference persona names as agent identifiers in future. |
| PRD-044 (agentops_bridge.py) | Internal | Current | AgentOps session can be associated with handoff accept events for cross-agent session stitching in future. |

---

## 15. Open Questions

| ID | Question | Impact | Owner | Target |
|----|----------|--------|-------|--------|
| OQ-1 | Should `queue_worker.py` automatically update `handoffs.status = 'COMPLETED'` when a handoff-typed queue job finishes? This would close the feedback loop without requiring polling. Requires hooking the queue worker's job completion path. | Medium — determines whether COMPLETED state is useful or just decorative. | PRD author | Phase 2 |
| OQ-2 | Should `tag handoff send` support `--reply-to <handoff_id>` to chain handoffs (orchestrator → coder → reviewer reply chain)? This would enable request/reply patterns between agents. | Low for MVP, high for protocol convergence with A2A's task reply semantics. | PRD author | Future PRD |
| OQ-3 | Should handoff IDs use ULIDs (sortable, time-embedded, Crockford base32) or the current `uuid4().hex` pattern? ULID would allow `ORDER BY id` to give chronological order without an extra `created_at` index. The `python-ulid` package is not currently in `pyproject.toml`. | Low — affects ID format only. ULID is strictly better but requires a new dependency. | Engineering | Before implementation |
| OQ-4 | Should `HandoffMessage.context` support nested agent-card-like capability declarations (inspired by A2A `AgentCard.capabilities`) so that the receiver can verify it has the required capabilities before accepting? | Medium — enables semantic acceptance decisions; out of scope for this PRD. | Future research | Future PRD |
| OQ-5 | Should `tag handoff list --pending` be the recommended polling mechanism for agent workers, or should we add a `tag handoff watch` long-polling/SSE command? Polling has the advantage of composability (pipe to `jq`); SSE would reduce latency for high-frequency handoff systems. | Low for current scale; high if TAG is used in high-throughput pipelines. | Engineering | Phase 2 |
| OQ-6 | Should `tag handoff expire` be registered as a cron job automatically (e.g., every 5 minutes) via PRD-040 notification hooks, or remain purely manual? | Medium — overdue handoffs accumulate silently if no one runs `expire`. | Engineering | Phase 1 or 2 |
| OQ-7 | What is the correct mapping from `HandoffMessage` to an A2A v1.0 `Task` object (`lf.a2a.v1` protobuf)? Specifically: `from_agent` → `Task.metadata.sender_id`? `context` → `Task.message.parts`? This mapping is needed for the A2A adapter PRD (future). | High for protocol interoperability; deferred. | Engineering | A2A adapter PRD |

---

## 16. Complexity and Timeline

### Phase 1 — Core Handoff Primitive (Days 1–4)

| Day | Deliverable |
|-----|-------------|
| 1 | `src/tag/teams.py` with `HandoffStatus`, `HandoffPriority`, `HandoffMessage` dataclass, `HandoffStateError`, `HandoffDispatcher.send()` and `HandoffDispatcher.get()`. Full unit tests for these. |
| 2 | `HandoffDispatcher.accept()` with `BEGIN IMMEDIATE` atomicity, queue_jobs injection, concurrent race test. `HandoffDispatcher.reject()` and `HandoffDispatcher.cancel()`. |
| 3 | Add `handoffs` DDL to `open_db()` in `controller.py`. Implement `cmd_handoff` with `send`, `accept`, `status`, `reject`, `cancel` subcommands and argparse registration. |
| 4 | `tag handoff list` with all filter flags and `--json` output. Human-readable table output. All exit code paths. |

### Phase 2 — Integration and Polish (Days 5–7)

| Day | Deliverable |
|-----|-------------|
| 5 | OTel span emission in `HandoffDispatcher` (PRD-013 integration). `tag handoff expire` subcommand. `HandoffDispatcher.expire_overdue()`. |
| 6 | Integration tests: `send → list → accept → status` full CLI flow. `tag queue list` visibility after accept. `--json` schema validation tests. Performance benchmarks. |
| 7 | Documentation: update `README.md` CLI reference section for `tag handoff`. Update `docs/prd/INDEX.md`. Final review and cleanup. |

### Total Estimate: 7 working days (within the M = 1–2 week envelope)

### Risk Factors

- **SQLite concurrency edge cases:** The `BEGIN IMMEDIATE` approach is correct for WAL mode, but subtle interactions with `PRAGMA busy_timeout` can cause unexpected lock timeouts under heavy concurrent load. Mitigate with the concurrency integration test in Phase 1 (Day 2).
- **`datetime.fromisoformat()` Python version differences:** Python 3.10 does not support `Z` suffix in `fromisoformat()`. The shim (`.rstrip("Z")` + `.replace(tzinfo=timezone.utc)`) is straightforward but must be tested on both 3.10 and 3.11+.
- **Queue worker coupling:** If the queue worker is modified in a concurrent PRD during Phase 2, the `queue_jobs` insertion in `accept()` may need to be updated to match new required columns. Keep `accept()` as minimal as possible (required columns only).

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
