# PRD-078: Human-in-the-Loop Tool Approval with Pause/Resume + Audit Trail (`tag mcp approve`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `internal/cli/mcp_approve.go + internal/tool/approval.go + tool_approval SQLite table`
**Depends on:** PRD-013 (agent tracing/observability), PRD-016 (webhook event triggers), PRD-027 (eval framework), PRD-028 (sandbox execution), PRD-034 (secret scanning/security), PRD-040 (notification hooks), PRD-048 (structured tool call spans)
**Inspired by:** Arcade AI human-in-the-loop, OpenAI Agents SDK guardrails, SOC-2 audit

---

## 1. Overview

Autonomous AI agents executing tool calls against production systems — pushing code, running shell commands, sending emails, modifying databases — represent a fundamental enterprise security risk when those calls are entirely unmonitored. Most organizations that want to adopt agentic AI are blocked not by capability gaps but by governance gaps: they cannot tell a CISO what an agent did, prove an action was authorized, or guarantee a human reviewed a destructive call before it executed. This is the problem PRD-078 solves.

Human-in-the-Loop (HITL) Tool Approval adds a first-class pause/resume gate into the TAG agent execution pipeline. Any MCP tool call — whether it targets a local bash executor, a GitHub API, a database ORM, or a cloud provisioning endpoint — can be marked as requiring human review before it is dispatched. When the agent reaches that call, execution suspends entirely. The pending approval is persisted to the `tool_approval` SQLite table, a desktop/webhook notification fires, and the agent waits (blocking or background, configurable) until a reviewer approves or denies the call via `tag mcp approve <approval-id>` or a matching webhook `POST`.

The design is modeled on Arcade AI's permission intersection model — the effective permission is `Agent ∩ User`, not `Agent ∪ User`. A tool can only execute if the agent's profile authorizes it AND the human reviewer approves this specific invocation. Approval policy is orthogonal to tool grant: granting a tool does not bypass the approval gate if the tool is also on the approve-required list. This separation of concerns is deliberate: it allows granting broad tool access to a profile while still requiring human sign-off for any call that targets a specific MCP server or matches a destructive pattern.

The audit trail is the equal partner of the gate mechanism. Every approval decision — approve, deny, or timeout — is appended to the `tool_approval_log` table with: ISO-8601 timestamp, reviewer identity (local username or webhook caller), approval ID, tool name, MCP server, full argument payload SHA-256, a verbatim copy of the argument payload, the decision, and a free-text rationale. This log is append-only, indexed, and can be exported as NDJSON for ingestion into SIEM tools. It satisfies the evidence trail requirements for SOC-2 Type II CC6.1 (logical access controls) and CC7.2 (monitoring of system operations).

The feature is designed to compose cleanly with TAG's existing systems. Approved/denied decisions surface as `tool_approval.*` events in the `events` table (PRD-040 notification hooks). Each approval creates a span child in the active trace (PRD-013 / PRD-048). The approval gate can be driven from CI with `--auto-deny-on-timeout` to prevent runaway agents in automated pipelines. Webhook-driven approval (for Slack bots, PagerDuty integrations, or custom approval portals) is first-class: the gate blocks on an in-process channel; the embedded HTTP/SSE server receives the POST and sends on that channel to resume the blocked goroutine.

---

## 2. Problem Statement

### 2.1 Agentic tool calls are ungoverned by default

TAG profiles can grant arbitrary MCP tool access. In practice this means a `coder` profile running `tag run` can execute `bash`, modify files, push to GitHub, and call external APIs without any human visibility into individual tool invocations. For personal developer use this is acceptable. For teams using TAG on shared infrastructure, for regulated industries, or for any agent that touches production systems, this creates an unacceptable blast radius. There is currently no mechanism to say "this tool is allowed in this profile, but any call to it must be reviewed before execution." The only granularity available is binary: grant or revoke. HITL approval adds the necessary third state: grant-with-gate.

### 2.2 Post-hoc logs are insufficient for compliance

TAG already records run history in the `runs` table and spans in `spans`. But these records are descriptive, not prescriptive: they describe what happened, they do not document that a human authorized it. SOC-2 Type II auditors require evidence that access controls on privileged operations include human authorization for actions above a defined risk threshold. A SQLite row saying "bash was called with `rm -rf /tmp/build`" does not satisfy this requirement. What is needed is a separate, write-once, tamper-evident log that records: who approved, when, what exact arguments were authorized, and what the outcome was. PRD-078 creates this log as a distinct table with append semantics and SHA-256 argument hashing.

### 2.3 Agent runaway in automated pipelines is not currently preventable at the tool level

When `tag run` is executed in CI with a broad tool grant, there is no mechanism to interrupt a destructive tool call that is about to be made. The only recourse is killing the process, which loses partial work and produces no audit record. HITL approval solves this at the semantic level: the agent pauses before dispatching the call and waits for a decision. In CI environments, a configurable timeout with auto-deny ensures the pipeline fails loudly rather than silently executing a destructive action.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Any MCP tool (by name) or tool+profile combination can be marked as requiring human approval before execution, stored in the `tool_approval_rules` table. |
| G2 | When an agent reaches a gated tool call, execution pauses and a pending approval record is created in `tool_approval_pending`. |
| G3 | Reviewers can approve or deny via `tag mcp approve <id>` (CLI), a POST to the local approval webhook server, or by directly editing a flag in SQLite (escape hatch for scripted approval). |
| G4 | Every decision (approve, deny, timeout, auto-deny) is appended to `tool_approval_log` with full argument payload, SHA-256 hash, reviewer identity, timestamp, rationale, and trace span ID. |
| G5 | The log is append-only from the application layer: no UPDATE or DELETE is issued against `tool_approval_log`; integrity is enforced by a SQLite trigger. |
| G6 | Desktop and webhook notifications fire on pending approval creation (integrating with PRD-040 hooks), enabling Slack/PagerDuty approval flows. |
| G7 | `tag mcp approvals list --pending --json` gives a machine-readable view of all outstanding approvals across all profiles. |
| G8 | A `--auto-deny-on-timeout <seconds>` mode makes HITL safe in CI: if no approval arrives within N seconds, the call is denied and the run fails with exit code 5. |
| G9 | Approved calls execute with exactly the arguments presented for review — the agent cannot modify arguments after approval is granted. |
| G10 | `tag mcp approve-required list` shows all active approval rules with their scope (profile, always) and creation metadata. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Full MFA or cryptographic reviewer identity verification. Reviewer identity is captured as the local OS username or a webhook `X-Approver` header; it is not cryptographically signed in this PRD. |
| NG2 | A web-based approval UI. The approval surface is the TAG CLI and webhooks only. A browser UI is a future extension (PRD-036 web dashboard). |
| NG3 | Per-argument-value approval rules (e.g., "approve bash only when argument matches `*.py`"). Rules are per-tool-name or per-MCP-server. Argument-level policy is a future feature. |
| NG4 | Automatic argument sanitization or redaction on behalf of the reviewer. The full argument payload is always shown; redaction of secrets visible in arguments is out of scope for this PRD. |
| NG5 | Multi-reviewer quorum (requiring 2-of-N approvals). Single reviewer per approval in this iteration. |
| NG6 | Integration with external identity providers (Okta, Azure AD) for reviewer authentication. |
| NG7 | Retroactive approval of calls that already executed. The gate operates pre-execution only. |
| NG8 | Modifying the MCP protocol itself. The HITL gate operates in the TAG agent loop layer, not at the MCP wire level. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Gate reliability | Zero tool calls execute through a gated tool without a logged approval decision | Integration test: assert `tool_approval_log` has a row before any gated tool call reaches its MCP server |
| Approval latency | `tag mcp approve <id>` completes the gate and resumes agent execution within 500 ms of CLI invocation | Benchmark test: time from approve command to agent resume event |
| Log completeness | 100% of approval decisions (approve, deny, timeout) appear in `tool_approval_log` within 1 second of decision | Integration test with SQLite assertion |
| Audit export | `tag mcp approvals export --format ndjson` produces valid NDJSON with all required fields for 1,000 log rows in < 2 seconds | Performance test |
| CI timeout coverage | Auto-deny fires within ±500 ms of the configured timeout value in 99th percentile | Benchmark test with mocked clock |
| Zero overhead for ungated tools | `tag run` wall time with no approval rules configured is statistically unchanged vs baseline | Benchmark: 20-run t-test, p > 0.05 |
| Webhook round-trip | Approval via webhook POST reaches agent resume in < 1 second on localhost | Integration test |
| Rule persistence | Approval rules survive TAG process restart | Integration test: add rule, kill process, relaunch, assert rule present |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Platform engineer | run `tag mcp approve-required add --tool bash --profile coder` | Any bash call from the coder profile requires my review before execution, giving me a governance gate on the most dangerous tool |
| U2 | Security engineer | run `tag mcp approve-required add --tool github:push --always` | Every GitHub push from any profile or any user requires human approval, regardless of who initiated the run |
| U3 | Reviewer | receive a desktop notification that says "TAG: bash call pending approval — coder profile — `rm -rf /tmp/old_build`" | I can evaluate the call and decide whether to approve or deny without watching the terminal |
| U4 | Reviewer | run `tag mcp approvals list --pending` | I can see all outstanding approvals at a glance, including which profile, which tool, and what arguments are waiting |
| U5 | Reviewer | run `tag mcp approve abc-123-def` with an optional `--rationale "Confirmed: safe to delete"` | The call is authorized, the agent resumes, and my rationale is persisted in the audit log |
| U6 | Reviewer | run `tag mcp approve abc-123-def --deny --rationale "Unsafe path in argument"` | The agent receives a denial, the run fails gracefully with a clear error, and the denial is logged |
| U7 | Compliance officer | run `tag mcp approvals export --format ndjson --since 2026-01-01` | I get a machine-readable audit log of every approval decision for the year, ready for SIEM ingestion |
| U8 | DevOps engineer | configure `auto_deny_timeout_seconds: 120` in the profile's approval config | CI pipelines never block indefinitely waiting for a human; they fail loudly after 2 minutes |
| U9 | Developer | run `tag mcp approve-required remove --tool bash --profile coder` | I can remove a gate rule when it's no longer needed without restarting the agent |
| U10 | Developer | run `tag mcp approve-required list` | I can see all active gate rules with their scope, creation date, and whether they're profile-scoped or global |
| U11 | Platform engineer | configure a webhook approval endpoint so a Slack bot can call `POST /approvals/<id>/approve` | Approvals happen through our existing Slack workflow without requiring the reviewer to open a terminal |
| U12 | Agent developer | write an eval that verifies a gated profile always pauses on `bash` calls | I can regression-test the gate behavior as part of CI (PRD-027 eval integration) |

---

## 7. Proposed CLI Surface

All subcommands live under the `tag mcp` namespace. Approval-related commands are grouped into two families: `approve-required` (managing rules) and `approvals` (managing pending/historical decisions).

### 7.1 `tag mcp approve-required add`

Mark a tool as requiring human approval before execution.

```
tag mcp approve-required add \
  --tool <tool-name-or-server:tool> \
  [--profile <profile-name>] \
  [--always] \
  [--timeout <seconds>] \
  [--auto-deny-on-timeout] \
  [--notify slack,desktop] \
  [--json]
```

**Flags:**
- `--tool`: Tool name to gate. Accepts bare name (`bash`) or `server:tool` form (`github:push`). Required.
- `--profile`: Scope the rule to a specific profile. Mutually exclusive with `--always`.
- `--always`: Apply this rule for all profiles, globally. Mutually exclusive with `--profile`.
- `--timeout`: Seconds to wait for approval before auto-deny or raising an error (default: none / wait forever).
- `--auto-deny-on-timeout`: If set with `--timeout`, automatically deny the call on timeout instead of raising a blocking error.
- `--notify`: Comma-separated list of notification channels to fire on pending (e.g. `slack,desktop`). Channels must be configured via PRD-040 hooks.
- `--json`: Print the created rule as JSON.

**Output (default):**
```
Approval rule created.
  Rule ID : rule_01JX9K3MBHA2VEQNRZ5T7GVF4M
  Tool    : bash
  Scope   : profile=coder
  Timeout : 120s (auto-deny)
  Notify  : desktop
```

**Output (`--json`):**
```json
{
  "rule_id": "rule_01JX9K3MBHA2VEQNRZ5T7GVF4M",
  "tool": "bash",
  "server": null,
  "profile": "coder",
  "always": false,
  "timeout_seconds": 120,
  "auto_deny_on_timeout": true,
  "notify_channels": ["desktop"],
  "created_at": "2026-06-17T10:00:00Z",
  "created_by": "sanskar"
}
```

### 7.2 `tag mcp approve-required remove`

Remove an existing approval rule.

```
tag mcp approve-required remove \
  --tool <tool-name> \
  [--profile <profile-name>] \
  [--always] \
  [--rule-id <rule-id>] \
  [--json]
```

Matches the rule by (tool, profile/always) combination or directly by `--rule-id`. Prints confirmation. Exits 1 if no matching rule found.

### 7.3 `tag mcp approve-required list`

List all active approval rules.

```
tag mcp approve-required list \
  [--profile <profile-name>] \
  [--json]
```

**Output (default):**
```
RULE ID                           TOOL          SCOPE           TIMEOUT  AUTO-DENY  CREATED
rule_01JX9K3MBHA2VEQNRZ5T7GVF4M  bash          profile=coder   120s     yes        2026-06-17 10:00
rule_01JX9K3MBHB7XMQWRZ5T7GVF9N  github:push   always          --       --         2026-06-17 10:05
```

### 7.4 `tag mcp approvals list`

List pending or historical approval requests.

```
tag mcp approvals list \
  [--pending] \
  [--profile <profile-name>] \
  [--tool <tool-name>] \
  [--since <ISO8601>] \
  [--limit <n>] \
  [--json]
```

**Flags:**
- `--pending`: Filter to only pending (unanswered) approvals. Without this flag, shows all historical approvals.
- `--profile`: Filter by originating profile.
- `--tool`: Filter by tool name.
- `--since`: Show only approvals created after this ISO-8601 datetime.
- `--limit`: Max rows (default 50).
- `--json`: Machine-readable NDJSON (one JSON object per line).

**Output (default, `--pending`):**
```
APPROVAL ID                       TOOL  PROFILE  CREATED              ARGS (PREVIEW)
appr_01JX9K3MBHA2VEQNRZ7GVF4MB   bash  coder    2026-06-17 10:01:23  {"command": "rm -rf /tmp/old_build"}
```

**Output (`--json`, one line per approval):**
```json
{"approval_id":"appr_01JX9K3MBHA2VEQNRZ7GVF4MB","tool":"bash","profile":"coder","run_id":"run-abc123","status":"pending","created_at":"2026-06-17T10:01:23Z","args_json":"{\"command\":\"rm -rf /tmp/old_build\"}","args_sha256":"e3b0c44298fc1c...","trace_span_id":"span-xyz"}
```

### 7.5 `tag mcp approve`

Approve or deny a pending approval request.

```
tag mcp approve <approval-id> \
  [--deny] \
  [--rationale <text>] \
  [--json]
```

**Args:**
- `<approval-id>`: The approval ID from `tag mcp approvals list --pending`. Required.

**Flags:**
- `--deny`: Deny the call instead of approving. Default is approve.
- `--rationale`: Free-text reason for the decision. Stored verbatim in `tool_approval_log`. Optional but strongly recommended for compliance.
- `--json`: Print the logged decision as JSON.

**Output (approve, default):**
```
Approved: appr_01JX9K3MBHA2VEQNRZ7GVF4MB
  Tool     : bash
  Profile  : coder
  Decision : APPROVED
  Reviewer : sanskar
  Rationale: (none)
Agent execution will resume.
```

**Output (deny):**
```
Denied: appr_01JX9K3MBHA2VEQNRZ7GVF4MB
  Tool     : bash
  Profile  : coder
  Decision : DENIED
  Reviewer : sanskar
  Rationale: Unsafe path in argument — escalate to infra team
The agent run will be terminated with exit code 5.
```

**Exit codes:**
- `0` — decision recorded, agent will resume (approve) or be terminated (deny).
- `1` — approval ID not found.
- `2` — approval already decided (cannot re-decide).
- `3` — approval has expired (timeout already triggered auto-deny).

### 7.6 `tag mcp approvals show`

Show full detail for a single approval request or decision.

```
tag mcp approvals show <approval-id> [--json]
```

Shows: approval ID, tool, MCP server, profile, run ID, trace span ID, full argument payload (pretty-printed JSON), args SHA-256, creation time, decision, reviewer, rationale, and decision time.

### 7.7 `tag mcp approvals export`

Export the append-only audit log.

```
tag mcp approvals export \
  [--format ndjson|csv|json] \
  [--since <ISO8601>] \
  [--until <ISO8601>] \
  [--profile <profile>] \
  [--tool <tool>] \
  [--decision approved|denied|timeout] \
  [--output <file>]
```

Streams `tool_approval_log` rows matching filters. Default format is NDJSON (one JSON per line). The `--output` flag writes to a file; default is stdout. Suitable for piping to `jq` or SIEM ingestion pipelines.

**NDJSON record format:**
```json
{
  "log_id": "log_01JX9K4XMBA2VEQNRZ7GVF4MB",
  "approval_id": "appr_01JX9K3MBHA2VEQNRZ7GVF4MB",
  "rule_id": "rule_01JX9K3MBHA2VEQNRZ5T7GVF4M",
  "tool": "bash",
  "mcp_server": null,
  "profile": "coder",
  "run_id": "run-abc123",
  "trace_span_id": "span-xyz",
  "args_json": "{\"command\": \"rm -rf /tmp/old_build\"}",
  "args_sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "decision": "approved",
  "reviewer": "sanskar",
  "reviewer_source": "cli",
  "rationale": "Confirmed safe to delete",
  "decided_at": "2026-06-17T10:01:45Z",
  "created_at": "2026-06-17T10:01:23Z"
}
```

### 7.8 Webhook Approval Server

When `approval.webhook_port` is configured (default: disabled), TAG starts a lightweight HTTP server on `127.0.0.1:<port>` during `tag run` that accepts approval decisions:

```
POST /approvals/<approval-id>/approve
POST /approvals/<approval-id>/deny
GET  /approvals/pending
GET  /approvals/<approval-id>
GET  /approvals/events          (SSE stream of pending-approval events)
```

Request body (optional for approve/deny):
```json
{"rationale": "Approved via Slack bot", "reviewer": "alice@example.com"}
```

The `X-Approver` header overrides the `reviewer` field in the body. Response is the logged decision JSON. The server binds to localhost only and requires no authentication in this PRD (see Security Considerations). The `/approvals/events` SSE endpoint allows clients to subscribe to pending-approval events with `Last-Event-ID` replay; reconnecting clients do not miss events fired while disconnected.

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag mcp approve-required add --tool <name> --profile <profile>` creates a row in `tool_approval_rules` scoped to the given profile. | Must |
| FR-02 | `tag mcp approve-required add --tool <name> --always` creates a row in `tool_approval_rules` with `profile = NULL`, matching all profiles. | Must |
| FR-03 | If a tool is on the approve-required list for a profile, every call to that tool during `tag run` for that profile MUST create a pending approval record before dispatching. | Must |
| FR-04 | A global (`--always`) rule takes precedence over the absence of a profile-scoped rule. If both a global and a profile-scoped rule exist, the most restrictive timeout applies. | Must |
| FR-05 | The agent execution loop MUST block the tool call dispatch until the `tool_approval_pending.status` column transitions from `pending` to `approved` or `denied`. | Must |
| FR-06 | If `status` transitions to `denied`, the agent run MUST be terminated with exit code 5 and a human-readable denial reason. | Must |
| FR-07 | If `status` transitions to `timeout` (auto-deny path), the behavior MUST match a denial: exit code 5, human-readable message. | Must |
| FR-08 | Every status transition (`pending → approved`, `pending → denied`, `pending → timeout`) MUST append exactly one row to `tool_approval_log`. No UPDATE or DELETE on `tool_approval_log` is ever issued by the application. | Must |
| FR-09 | `tag mcp approve <id>` MUST record the OS username (`os/user.Current().Username`) as the `reviewer` field in `tool_approval_log`. | Must |
| FR-10 | The `args_sha256` field in `tool_approval_log` MUST be the SHA-256 of the canonical JSON serialization of the tool argument payload (keys sorted, no whitespace). | Must |
| FR-11 | The arguments dispatched to the MCP server MUST be byte-for-byte identical to the arguments that were presented for review (captured at gate time). The agent cannot re-generate arguments after approval. | Must |
| FR-12 | `tag mcp approvals list --pending --json` MUST return results within 200 ms for up to 10,000 log rows. | Must |
| FR-13 | Each pending approval creation MUST fire a `tool_approval.pending` event into the `events` table, enabling PRD-040 notification hooks (Slack, desktop, email). | Must |
| FR-14 | Approval gate creates a child span in the active trace (PRD-013) with name `tool_approval.gate`, attributes including `approval_id`, `tool_name`, `decision`, and `duration_ms`. | Should |
| FR-15 | The webhook approval server MUST bind to `127.0.0.1` only, never `0.0.0.0`. | Must |
| FR-16 | `tag mcp approve-required remove` MUST remove only the matching rule; running the command with a non-matching tool+profile MUST exit 1 with a descriptive error. | Must |
| FR-17 | `tag mcp approvals export` MUST produce valid NDJSON with all required fields as specified in §7.7, with one JSON object per line. | Must |
| FR-18 | Adding or removing an approval rule MUST NOT require restarting an in-progress `tag run`; the agent loop re-checks the rules cache (TTL 5 s) on every tool call. | Should |
| FR-19 | `tag mcp approvals list --pending` MUST show the first 200 characters of the argument JSON as a preview column. | Should |
| FR-20 | When `--auto-deny-on-timeout` is set, the timeout MUST be enforced by the `context.WithTimeout` deadline passed to the gate goroutine, not by the caller waiting on a subprocess. | Must |
| FR-21 | `tool_approval_rules` MUST enforce a UNIQUE constraint on `(tool, mcp_server, profile)` to prevent duplicate rules. Attempting to add a duplicate MUST fail with a descriptive error and exit 1. | Must |
| FR-22 | `tag mcp approvals show <id>` MUST display the full (untruncated) argument JSON payload. | Must |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Approval gate overhead (time added to a tool call that requires approval AND is immediately approved programmatically) MUST be less than 50 ms in the happy path with the reviewer already polling. | < 50 ms |
| NFR-02 | The `tool_approval_log` table MUST use append-only semantics enforced by a `BEFORE UPDATE` and `BEFORE DELETE` SQLite trigger that raises an error. | Enforced at DB layer |
| NFR-03 | `tag mcp approvals export` MUST stream rows lazily (no full-table load into memory) for exports exceeding 10,000 rows. | Streaming cursor |
| NFR-04 | The webhook approval server MUST handle concurrent approve/deny requests without race conditions; all state transitions MUST use `UPDATE ... WHERE status='pending'` with row-count assertion, and the in-process `Resolve()` call is protected by a mutex. | Atomic update |
| NFR-05 | The approval gate MUST work in both blocking mode (agent goroutine waits on a channel) and detached mode (agent writes pending record and exits; a separate `tag run --resume` picks it up after approval). | Both modes |
| NFR-06 | The approval gate MUST be completely inert (zero overhead, zero SQLite queries) when no approval rules are configured. Rules presence check is an in-memory cache lookup per tool call. | Zero overhead |
| NFR-07 | All approval-related SQLite writes MUST complete within a single transaction to prevent partial state (e.g., pending record created without matching span). | Atomic |
| NFR-08 | `tool_approval_log` rows MUST be indexed by `(created_at, profile, tool, decision)` to support sub-100 ms export queries with time and filter constraints. | < 100 ms query |
| NFR-09 | The Go package implementing approval gate logic (`internal/tool/approval.go`) MUST have > 90% line coverage in unit tests. | > 90% |
| NFR-10 | Error messages on approval denial MUST include the approval ID, tool name, reviewer, rationale (if set), and a pointer to `tag mcp approvals show <id>` for full detail. | Human-readable |

---

## 10. Technical Design

### 10.1 New Packages

| Package / File | Purpose |
|---|---|
| `internal/tool/approval.go` | `PermissionService`: rule lookup (with 5 s TTL cache), pending record creation, channel-based blocking gate, `context.Context` timeout/cancellation, audit-log append with optional SHA-256 hash-chaining, SSE notification dispatch |
| `internal/tool/approval_test.go` | Unit and integration tests for all gate paths |
| `internal/server/approval.go` | Webhook approval server: `net/http` + `go-chi/chi v5` router; SSE stream of pending-approval events via `tmaxmax/go-sse` with `Last-Event-ID` replay; bound to `127.0.0.1` only |
| `internal/cli/mcp_approve.go` | Cobra command handlers for `tag mcp approve-required {add,remove,list}`, `tag mcp approvals {list,show,export}`, and `tag mcp approve` |

**Modifications to existing packages:**

- `internal/runtime/dispatch.go`: Inject `PermissionService.Check()` call before the MCP server call is made. `PermissionService` is passed as a pointer and is `nil` when no approval rules are configured; the call-site is a single nil-check with zero overhead in the common case (NFR-06).
- `internal/store/migrate.go`: Add `migratePRD078Tables(db *sql.DB) error` for the three new SQLite tables, called from the store's schema-migration chain during `store.Open()`.

### 10.2 SQLite DDL

All three new tables are created in `migratePRD078Tables` inside `internal/store/migrate.go`, called during store initialisation. All DDL executes inside a single transaction (NFR-07). The store uses `modernc.org/sqlite` (pure-Go, `CGO_ENABLED=0`, FTS5 built-in) with WAL mode and a single-writer lock enforced by `gofrs/flock` + `os.Rename` atomic RMW.

The `tool_approval_log` table includes an optional `prev_hash TEXT` column for SHA-256 hash-chaining (using `crypto/sha256`). When `approval.hash_chain: true` is set in profile config, each appended log row stores `SHA-256(canonical-JSON of the previous row)`, creating a tamper-evident chain that strengthens the SOC-2 immutability guarantee beyond what SQLite triggers alone provide. A verifier can walk the chain and detect any modified row by recomputing hashes.

```sql
-- Approval gate rules: which tools require human approval
CREATE TABLE IF NOT EXISTS tool_approval_rules (
    id           TEXT PRIMARY KEY,        -- ULID, e.g. rule_01JX9K3MBHA2VEQNRZ5T7GVF4M
    tool         TEXT NOT NULL,           -- bare tool name, e.g. 'bash', or 'github:push'
    mcp_server   TEXT,                    -- MCP server name if tool is server-scoped, else NULL
    profile      TEXT,                    -- profile name, or NULL for --always (global)
    timeout_seconds INTEGER,              -- NULL = wait forever
    auto_deny_on_timeout INTEGER NOT NULL DEFAULT 0,  -- 1 = auto-deny, 0 = block/error
    notify_channels TEXT NOT NULL DEFAULT '[]',       -- JSON array of channel names
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL,           -- OS username of rule creator
    UNIQUE(tool, mcp_server, profile)
);
CREATE INDEX IF NOT EXISTS idx_tar_profile ON tool_approval_rules(profile, tool);
CREATE INDEX IF NOT EXISTS idx_tar_tool    ON tool_approval_rules(tool, mcp_server);

-- Pending and resolved approval requests (one per gated tool call invocation)
CREATE TABLE IF NOT EXISTS tool_approval_pending (
    id           TEXT PRIMARY KEY,        -- ULID, e.g. appr_01JX9K3MBHA2VEQNRZ7GVF4MB
    rule_id      TEXT NOT NULL,           -- FK to tool_approval_rules.id
    run_id       TEXT NOT NULL,           -- FK to runs.id
    trace_span_id TEXT,                   -- span ID from PRD-013 spans table
    tool         TEXT NOT NULL,           -- tool name at call time
    mcp_server   TEXT,                    -- MCP server name at call time
    profile      TEXT NOT NULL,           -- profile that triggered the call
    args_json    TEXT NOT NULL,           -- verbatim argument JSON payload (frozen at gate entry)
    args_sha256  TEXT NOT NULL,           -- SHA-256 of canonical args (sorted keys, no whitespace)
    status       TEXT NOT NULL DEFAULT 'pending',  -- 'pending' | 'approved' | 'denied' | 'timeout'
    reviewer     TEXT,                    -- OS username or webhook X-Approver
    reviewer_source TEXT,                 -- 'cli' | 'webhook' | 'tui' | 'auto'
    rationale    TEXT,                    -- free-text decision reason
    created_at   TEXT NOT NULL,
    decided_at   TEXT,                    -- NULL until decision made
    expires_at   TEXT,                    -- NULL if no timeout configured
    FOREIGN KEY(rule_id) REFERENCES tool_approval_rules(id),
    FOREIGN KEY(run_id)  REFERENCES runs(id)
);
CREATE INDEX IF NOT EXISTS idx_tap_status  ON tool_approval_pending(status, created_at);
CREATE INDEX IF NOT EXISTS idx_tap_run     ON tool_approval_pending(run_id, status);
CREATE INDEX IF NOT EXISTS idx_tap_profile ON tool_approval_pending(profile, tool, status);

-- Append-only audit log: one row per decision event
-- prev_hash is populated when approval.hash_chain=true in profile config (crypto/sha256).
CREATE TABLE IF NOT EXISTS tool_approval_log (
    id              TEXT PRIMARY KEY,     -- ULID, e.g. log_01JX9K4XMBA2VEQNRZ7GVF4MB
    approval_id     TEXT NOT NULL,        -- FK to tool_approval_pending.id
    rule_id         TEXT NOT NULL,
    tool            TEXT NOT NULL,
    mcp_server      TEXT,
    profile         TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    trace_span_id   TEXT,
    args_json       TEXT NOT NULL,
    args_sha256     TEXT NOT NULL,
    decision        TEXT NOT NULL,        -- 'approved' | 'denied' | 'timeout'
    reviewer        TEXT,
    reviewer_source TEXT NOT NULL DEFAULT 'cli',
    rationale       TEXT,
    prev_hash       TEXT,                 -- SHA-256 of previous log row; NULL if hash-chain disabled
    created_at      TEXT NOT NULL,        -- approval request creation time
    decided_at      TEXT NOT NULL         -- decision recording time
);
CREATE INDEX IF NOT EXISTS idx_tal_decided  ON tool_approval_log(decided_at DESC);
CREATE INDEX IF NOT EXISTS idx_tal_profile  ON tool_approval_log(profile, tool, decided_at);
CREATE INDEX IF NOT EXISTS idx_tal_decision ON tool_approval_log(decision, decided_at);

-- Append-only enforcement trigger: no UPDATE or DELETE on tool_approval_log
CREATE TRIGGER IF NOT EXISTS trg_tal_no_update
    BEFORE UPDATE ON tool_approval_log
BEGIN
    SELECT RAISE(FAIL, 'tool_approval_log is append-only: UPDATE is not permitted');
END;

CREATE TRIGGER IF NOT EXISTS trg_tal_no_delete
    BEFORE DELETE ON tool_approval_log
BEGIN
    SELECT RAISE(FAIL, 'tool_approval_log is append-only: DELETE is not permitted');
END;
```

### 10.3 Core Go Structs (`internal/tool/approval.go`)

Go structs replace Python `@dataclass` definitions. `encoding/json` is used for serialisation; `invopop/jsonschema` for schema generation; `crypto/sha256` replaces `hashlib`. `encoding/json.Marshal` on a `map[string]any` sorts keys alphabetically by default, producing canonical JSON without a custom serialiser — identical to Python's `json.dumps(sort_keys=True, separators=(",",":"))`.

```go
package tool

import (
    "context"
    "crypto/sha256"
    "database/sql"
    "encoding/json"
    "fmt"
    "sync"
    "time"
)

// canonicalArgsSHA256 returns the SHA-256 hex digest of canonical JSON.
// json.Marshal on map[string]any sorts keys alphabetically (Go spec), matching
// Python json.dumps(sort_keys=True, separators=(",",":")):
func canonicalArgsSHA256(args map[string]any) (string, error) {
    b, err := json.Marshal(args) // compact, map keys sorted
    if err != nil {
        return "", err
    }
    sum := sha256.Sum256(b)
    return fmt.Sprintf("%x", sum), nil
}

// ApprovalRule is a gate rule loaded from tool_approval_rules and held in the rules cache.
type ApprovalRule struct {
    ID                string
    Tool              string
    MCPServer         string    // empty = bare tool name, not server-scoped
    Profile           string    // empty = global (--always)
    TimeoutSeconds    int       // 0 = wait forever
    AutoDenyOnTimeout bool
    NotifyChannels    []string
    CreatedAt         time.Time
    CreatedBy         string
}

// Decision is the resolution of a pending approval, sent on the in-process channel.
type Decision struct {
    Outcome        string    // "approved" | "denied" | "timeout"
    Reviewer       string
    ReviewerSource string    // "cli" | "webhook" | "tui" | "auto"
    Rationale      string
    DecidedAt      time.Time
}

// pendingApproval is an in-flight gate record, held in PermissionService.pending.
type pendingApproval struct {
    ID          string
    RuleID      string
    RunID       string
    TraceSpanID string
    Tool        string
    MCPServer   string
    Profile     string
    ArgsJSON    string    // canonical JSON frozen at gate entry — dispatched verbatim (FR-11)
    ArgsSHA256  string
    CreatedAt   time.Time
    ExpiresAt   time.Time // zero value if no timeout configured
    ch          chan Decision // buffered(1); resolved by Resolve() or auto-timeout path
}

// ApprovalDeniedError is returned by Check() on denial or auto-deny timeout.
type ApprovalDeniedError struct {
    ApprovalID string
    Tool       string
    Outcome    string // "denied" | "timeout"
    Rationale  string
}

func (e *ApprovalDeniedError) Error() string {
    return fmt.Sprintf(
        "tool call %q was %s (approval: %s). Reason: %s. See: tag mcp approvals show %s",
        e.Tool, e.Outcome, e.ApprovalID, e.Rationale, e.ApprovalID,
    )
}

// ApprovalTimeoutError is returned when the deadline fires and auto-deny is not configured.
type ApprovalTimeoutError struct {
    ApprovalID     string
    TimeoutSeconds int
}

func (e *ApprovalTimeoutError) Error() string {
    return fmt.Sprintf(
        "approval %s timed out after %ds. Set auto_deny_on_timeout: true to auto-deny instead of blocking.",
        e.ApprovalID, e.TimeoutSeconds,
    )
}
```

### 10.4 PermissionService (`internal/tool/approval.go`)

`PermissionService` is the central HITL gate, held as a singleton for the lifetime of `tag run` and injected into the dispatch path. It replaces the Python `ApprovalGate` class.

The key architectural change from Python: **polling is eliminated**. Python's `ApprovalGate._block_until_decided()` called `time.sleep(0.25)` in a loop, checking SQLite every 250 ms. In Go, `Check()` blocks on `<-pend.ch` (a buffered channel of capacity 1). `Resolve()` sends the decision on that channel, unblocking `Check()` in microseconds. The `context.WithTimeout` deadline (when configured) races the channel receive — no goroutine is needed to enforce it. This achieves sub-millisecond resume latency from the moment a decision is delivered.

The rules cache is re-read from SQLite every 5 s (configurable via `ruleTTL`), supporting live rule add/remove without restarting the agent (FR-18).

```go
// PermissionService is the singleton HITL gate for the lifetime of tag run.
type PermissionService struct {
    db            *sql.DB
    mu            sync.RWMutex
    rulesCache    []ApprovalRule
    rulesCachedAt time.Time
    ruleTTL       time.Duration // default 5s; supports hot-reload (FR-18)
    pending       map[string]*pendingApproval // keyed by approval ID; protected by mu
    hashChain     bool // when true, appendLog writes prev_hash (crypto/sha256)
}

// Check gates a tool call. Returns nil if approved.
// Returns *ApprovalDeniedError on denial or auto-deny timeout.
// Returns *ApprovalTimeoutError when timeout fires without auto-deny.
// Fast path when no rule matches: single in-memory cache lookup, zero DB I/O (NFR-06).
func (ps *PermissionService) Check(
    ctx context.Context,
    tool, mcpServer, profile, runID, spanID string,
    args map[string]any,
) error {
    rule := ps.matchRule(tool, mcpServer, profile) // in-memory cache lookup
    if rule == nil {
        return nil // ungated: zero overhead
    }

    pend, err := ps.createPending(ctx, rule, tool, mcpServer, profile, runID, spanID, args)
    if err != nil {
        return fmt.Errorf("approval gate: %w", err)
    }
    ps.fireNotification(pend, rule) // desktop notification + SSE event publish

    // Build a deadline context if the rule specifies a timeout.
    gateCtx := ctx
    if rule.TimeoutSeconds > 0 {
        var cancel context.CancelFunc
        gateCtx, cancel = context.WithTimeout(ctx, time.Duration(rule.TimeoutSeconds)*time.Second)
        defer cancel()
    }

    // Block until Resolve() delivers a decision or the deadline fires.
    // No polling loop; channel send from Resolve() costs ~100 ns.
    select {
    case dec := <-pend.ch:
        ps.appendLog(ctx, pend, dec) // single transaction; optional hash-chain (NFR-07)
        if dec.Outcome == "approved" {
            return nil
        }
        return &ApprovalDeniedError{
            ApprovalID: pend.ID, Tool: pend.Tool,
            Outcome: dec.Outcome, Rationale: dec.Rationale,
        }

    case <-gateCtx.Done():
        if rule.AutoDenyOnTimeout {
            dec := Decision{
                Outcome: "timeout", Reviewer: "system", ReviewerSource: "auto",
                Rationale: "Auto-denied: approval timeout expired",
                DecidedAt: time.Now().UTC(),
            }
            ps.recordTimeout(ctx, pend, dec) // UPDATE tool_approval_pending
            ps.appendLog(ctx, pend, dec)
            return &ApprovalDeniedError{
                ApprovalID: pend.ID, Tool: pend.Tool,
                Outcome: "timeout", Rationale: dec.Rationale,
            }
        }
        return &ApprovalTimeoutError{ApprovalID: pend.ID, TimeoutSeconds: rule.TimeoutSeconds}
    }
}

// Resolve delivers a decision to the goroutine blocked in Check().
// Called by: the approval HTTP server handler (webhook POST), the TUI key-handler,
// or cmdMCPApprove when running in the same process as the agent.
// For the out-of-process case (tag mcp approve in a separate terminal), the approval
// server runs as a goroutine within the agent process and calls Resolve on its behalf.
func (ps *PermissionService) Resolve(approvalID string, dec Decision) error {
    ps.mu.Lock()
    pend, ok := ps.pending[approvalID]
    ps.mu.Unlock()
    if !ok {
        return fmt.Errorf("approval %q not found or already resolved", approvalID)
    }
    pend.ch <- dec // unblocks Check(); channel is buffered(1) so this never blocks
    return nil
}
```

`createPending` writes the `tool_approval_pending` row and the `tool_approval.pending` event row inside a single transaction, then adds the `*pendingApproval` (with its channel) to `ps.pending`. `appendLog` writes one row to `tool_approval_log`; when `hashChain=true`, it first queries the latest log row, computes `SHA-256(canonical-JSON of that row)` via `crypto/sha256`, and stores the result in `prev_hash`. `matchRule` implements profile-scoped-over-global precedence, returning the most specific matching rule or `nil` for ungated tools.

### 10.5 Integration Point: `internal/runtime/dispatch.go`

The gate is inserted into the unified tool dispatch path. Both built-in tools and MCP tools use the same `Run()` interface, gated through `PermissionService.Check()`. The MCP call itself uses `github.com/modelcontextprotocol/go-sdk v1.6.1`, protocol version pinned to `2025-11-25`.

```go
// internal/runtime/dispatch.go

func DispatchToolCall(
    ctx    context.Context,
    ps     *tool.PermissionService, // nil when no approval rules are configured
    name, mcpServer, profile, runID, spanID string,
    args   map[string]any,
) (json.RawMessage, error) {
    if ps != nil {
        if err := ps.Check(ctx, name, mcpServer, profile, runID, spanID, args); err != nil {
            // ApprovalDeniedError and ApprovalTimeoutError both map to exit code 5
            // in the agent loop caller; the error message is human-readable (NFR-10).
            return nil, err
        }
    }
    // args are the values frozen at gate entry and stored in tool_approval_pending.args_json.
    // The dispatch layer re-serialises from these same values — the agent cannot substitute
    // different arguments after approval is granted (FR-11).
    //
    // Proceed with MCP server call via go-sdk v1.6.1, protocol 2025-11-25:
    // client.CallTool(ctx, &mcp.CallToolRequest{Name: name, Arguments: args})
    // ...
    return result, nil
}
```

### 10.6 Webhook Approval Server (`internal/server/approval.go`)

Replaces Python's `http.server.BaseHTTPRequestHandler` / `threading.Thread` pattern. The server starts as a goroutine within the `tag run` process and shuts down via `context.Context` cancellation when the run completes. It uses `net/http` + `go-chi/chi v5` for routing and `tmaxmax/go-sse` for the SSE event stream, which supports `Last-Event-ID` replay so reconnecting clients do not miss pending-approval events.

```go
// internal/server/approval.go
package server

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "os"
    "time"

    "github.com/go-chi/chi/v5"
    sse "github.com/tmaxmax/go-sse"
    "github.com/tag-project/tag/internal/tool"
)

// ApprovalServer exposes approval endpoints and streams events to SSE subscribers.
type ApprovalServer struct {
    ps        *tool.PermissionService
    sseServer *sse.Server // Last-Event-ID replay for reconnecting clients
}

// Mount returns the chi router. The caller MUST bind only to 127.0.0.1:<port> (FR-15).
func (s *ApprovalServer) Mount() http.Handler {
    r := chi.NewRouter()
    r.Get("/approvals/events", s.sseServer.ServeHTTP) // SSE: subscribe to pending events
    r.Get("/approvals/pending", s.listPending)
    r.Get("/approvals/{id}", s.show)
    r.Post("/approvals/{id}/approve", s.decide("approved"))
    r.Post("/approvals/{id}/deny", s.decide("denied"))
    return r
}

func (s *ApprovalServer) decide(outcome string) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        id := chi.URLParam(r, "id")
        var body struct {
            Rationale string `json:"rationale"`
            Reviewer  string `json:"reviewer"`
        }
        _ = json.NewDecoder(r.Body).Decode(&body)

        reviewer := r.Header.Get("X-Approver")
        if reviewer == "" { reviewer = body.Reviewer }
        if reviewer == "" { reviewer = os.Getenv("USER") }

        dec := tool.Decision{
            Outcome:        outcome,
            Reviewer:       reviewer,
            ReviewerSource: "webhook",
            Rationale:      body.Rationale,
            DecidedAt:      time.Now().UTC(),
        }
        // Resolve() sends on pend.ch, unblocking the Check() goroutine in the agent.
        // Concurrent approve+deny requests: the first Resolve() drains the channel;
        // subsequent calls find no entry in ps.pending and return an error (NFR-04).
        if err := s.ps.Resolve(id, dec); err != nil {
            http.Error(w, err.Error(), http.StatusConflict) // 409: already decided
            return
        }
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]any{
            "approval_id": id, "decision": outcome, "reviewer": reviewer,
        })
    }
}

// StartApprovalServer starts the server as a goroutine, bound to 127.0.0.1 only (FR-15).
// Shuts down cleanly when ctx is cancelled (tag run completes or is interrupted).
func StartApprovalServer(ctx context.Context, ps *tool.PermissionService, port int) {
    as := &ApprovalServer{ps: ps, sseServer: sse.NewServer()}
    srv := &http.Server{
        Addr:    fmt.Sprintf("127.0.0.1:%d", port), // never 0.0.0.0
        Handler: as.Mount(),
    }
    go func() { _ = srv.ListenAndServe() }()
    go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
}
```

### 10.7 Migration Function (`internal/store/migrate.go`)

Replaces the Python `_migrate_prd_078_tables(conn)` pattern. Added to the existing migration chain in `internal/store/migrate.go`, called during `store.Open()`. Uses `modernc.org/sqlite` (pure-Go, `CGO_ENABLED=0`). The single-writer contract is enforced by `gofrs/flock` at the store level; no Python process may concurrently open the DB (Phase-3 DB-ownership handoff).

```go
// internal/store/migrate.go (addition to existing migration chain)

const prd078DDL = `
    CREATE TABLE IF NOT EXISTS tool_approval_rules ( ... );
    CREATE INDEX IF NOT EXISTS idx_tar_profile ON tool_approval_rules(profile, tool);
    CREATE INDEX IF NOT EXISTS idx_tar_tool    ON tool_approval_rules(tool, mcp_server);
    CREATE TABLE IF NOT EXISTS tool_approval_pending ( ... );
    -- ... indexes ...
    CREATE TABLE IF NOT EXISTS tool_approval_log ( ... );
    -- ... indexes ...
    CREATE TRIGGER IF NOT EXISTS trg_tal_no_update BEFORE UPDATE ON tool_approval_log
    BEGIN SELECT RAISE(FAIL, 'tool_approval_log is append-only: UPDATE is not permitted'); END;
    CREATE TRIGGER IF NOT EXISTS trg_tal_no_delete BEFORE DELETE ON tool_approval_log
    BEGIN SELECT RAISE(FAIL, 'tool_approval_log is append-only: DELETE is not permitted'); END;
`

func migratePRD078Tables(db *sql.DB) error {
    _, err := db.ExecContext(context.Background(), prd078DDL)
    return err
}
```

### 10.8 CLI Handler Sketch (`internal/cli/mcp_approve.go`)

Replaces the Python `argparse`-based `cmd_mcp_approve_required` and `cmd_mcp_approve` functions in `controller.py`. Registered as `cobra` sub-commands under `tag mcp`. Reviewer identity is obtained from `os/user.Current().Username` (replaces Python `os.getlogin()` / `os.environ.get("USER")`). Config is loaded via `knadh/koanf/v2`.

```go
// internal/cli/mcp_approve.go
package cli

import (
    "encoding/json"
    "fmt"
    "os/user"
    "strings"
    "time"

    "github.com/spf13/cobra"
    "github.com/tag-project/tag/internal/store"
    "github.com/tag-project/tag/internal/tool"
)

func cmdMCPApproveRequiredAdd(cmd *cobra.Command, _ []string) error {
    toolFlag, _    := cmd.Flags().GetString("tool")
    profileFlag, _ := cmd.Flags().GetString("profile")
    alwaysFlag, _  := cmd.Flags().GetBool("always")
    timeoutSec, _  := cmd.Flags().GetInt("timeout")
    autoDeny, _    := cmd.Flags().GetBool("auto-deny-on-timeout")
    notifyStr, _   := cmd.Flags().GetString("notify")
    asJSON, _      := cmd.Flags().GetBool("json")

    if profileFlag != "" && alwaysFlag {
        return fmt.Errorf("--profile and --always are mutually exclusive")
    }
    profile := profileFlag
    if alwaysFlag { profile = "" } // NULL in DB

    // Parse "server:tool" form
    mcpServer, toolName := "", toolFlag
    if idx := strings.IndexByte(toolFlag, ':'); idx >= 0 {
        mcpServer, toolName = toolFlag[:idx], toolFlag[idx+1:]
    }

    var notifyChannels []string
    if notifyStr != "" {
        notifyChannels = strings.Split(notifyStr, ",")
    }
    notifyJSON, _ := json.Marshal(notifyChannels)

    u, _ := user.Current()
    ruleID := newULID("rule") // internal/util ULID generator

    db := store.FromContext(cmd.Context())
    _, err := db.ExecContext(cmd.Context(),
        `INSERT INTO tool_approval_rules
           (id, tool, mcp_server, profile, timeout_seconds, auto_deny_on_timeout,
            notify_channels, created_at, created_by)
         VALUES (?,?,?,?,?,?,?,?,?)`,
        ruleID, toolName, mcpServer, sqlNull(profile),
        sqlNullInt(timeoutSec), boolToInt(autoDeny),
        string(notifyJSON), time.Now().UTC().Format(time.RFC3339), u.Username,
    )
    if isSQLiteUniqueViolation(err) {
        return fmt.Errorf("approval rule already exists for tool=%q profile=%q (exit 1)", toolName, profile)
    }
    if err != nil { return err }

    if asJSON {
        return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
            "rule_id": ruleID, "tool": toolName, "mcp_server": mcpServer, "profile": profile,
        })
    }
    scope := fmt.Sprintf("profile=%s", profile)
    if alwaysFlag { scope = "always (global)" }
    fmt.Fprintf(cmd.OutOrStdout(), "Approval rule created: %s — tool=%s scope=%s\n", ruleID, toolName, scope)
    return nil
}

func cmdMCPApprove(cmd *cobra.Command, args []string) error {
    approvalID := args[0]
    denyFlag, _ := cmd.Flags().GetBool("deny")
    rationale, _ := cmd.Flags().GetString("rationale")

    outcome := "approved"
    if denyFlag { outcome = "denied" }

    u, _ := user.Current()
    dec := tool.Decision{
        Outcome:        outcome,
        Reviewer:       u.Username,
        ReviewerSource: "cli",
        Rationale:      rationale,
        DecidedAt:      time.Now().UTC(),
    }

    // Resolve via in-process PermissionService if available (interactive mode).
    // Otherwise POST to the approval server at approval.webhook_port.
    // Fallback: UPDATE tool_approval_pending directly and let the agent's
    // next Resolve() call pick up the decision via SSE notification.
    ps := tool.PermissionServiceFromContext(cmd.Context())
    if ps != nil {
        return ps.Resolve(approvalID, dec)
    }
    return resolveViaWebhook(cmd.Context(), approvalID, dec)
}
```

### 10.9 Config Schema Additions

New keys under the profile config YAML, loaded and merged via `knadh/koanf/v2` (replaces direct YAML dict access). Written back atomically via `gopkg.in/yaml.v3` marshal + `gofrs/flock` + `os.Rename`. Defaults shown:

```yaml
approval:
  webhook_port: null              # null = disabled; integer = bind approval server on that port
  auto_deny_timeout_seconds: null # null = wait forever (overridable per rule)
  notify_channels: []             # default notification channels for all approval rules
  hash_chain: false               # true = SHA-256 hash-chain tool_approval_log rows (crypto/sha256)
```

---

## 11. Security Considerations

1. **Localhost-only webhook server.** The approval webhook server MUST bind to `127.0.0.1` only (enforced in `StartApprovalServer` via the `Addr` field of `http.Server`). It must never bind to `0.0.0.0` or a public interface, preventing remote attackers from approving tool calls. Verified in integration tests by asserting `srv.Addr` begins with `127.0.0.1:`.

2. **No authentication on the webhook in this PRD.** Since the server is localhost-only, authentication relies on OS-level process isolation. This is documented as a known limitation. Future work: add a shared secret token verified in a `chi` middleware on the `Authorization` header.

3. **Append-only audit log integrity.** The `trg_tal_no_update` and `trg_tal_no_delete` SQLite triggers prevent application-layer modification of `tool_approval_log`. An attacker who can issue arbitrary SQL bypasses these triggers; the triggers are not a cryptographic guarantee. When `approval.hash_chain: true` is configured, `appendLog` chains each row's `prev_hash` field via `crypto/sha256` over the canonical JSON of the previous row, making tampering detectable by chain verification without an external store.

4. **Argument payload at gate time is the canonical payload.** The `args_json` and `args_sha256` stored in `tool_approval_pending` at gate entry time are the values passed to the MCP server. The dispatch code in `internal/runtime/dispatch.go` re-serialises from the same frozen `args map[string]any` that was captured before `Check()` was called — the agent cannot substitute different arguments after approval is granted, preventing TOCTOU substitution.

5. **Reviewer identity is OS-level only.** `reviewer` is set from `os/user.Current().Username`. This is adequate for single-user workstations and CI environments but is not a strong identity claim in shared-user systems. Teams requiring stronger identity guarantees should gate the `tag` binary itself (e.g., via sudo or a signed CLI wrapper).

6. **Secrets in argument payloads.** Tool arguments may contain API keys or sensitive values. The `args_json` column stores these verbatim. The `tool_approval_log` table should be included in the same access control perimeter as the rest of the TAG SQLite database (`~/.tag/runtime/tag.sqlite3`). Do not export audit logs to untrusted destinations without redacting sensitive argument fields.

7. **Denial-of-service via approval flood.** A misconfigured or malicious agent could generate thousands of pending approvals per second, filling the `tool_approval_pending` table. A rate limit of at most 10 pending approvals per profile per minute is enforced at the gate layer; calls exceeding this limit are auto-denied with a rate-limit error logged.

8. **Race condition on concurrent approve+deny.** The `UPDATE ... WHERE status='pending'` pattern with row-count assertion prevents two concurrent `tag mcp approve` invocations from both succeeding on the same approval. The in-process `Resolve()` path is additionally protected by a mutex and a buffered channel of capacity 1; only the first send succeeds.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/tool/approval_test.go`)

Tests use the standard `testing` package + `testify/assert` + `testify/require`. SQLite is opened in-memory (`modernc.org/sqlite`, `file::memory:?cache=shared`) and the migration DDL is applied before each test. Run with `go test ./internal/tool/...`.

| Test | Description |
|------|-------------|
| `TestMatchRuleProfileScoped` | Gate matches profile-scoped rule and ignores global rule when profile matches |
| `TestMatchRuleGlobalFallback` | Gate falls back to global rule when no profile-scoped rule matches |
| `TestMatchRuleNoMatch` | Gate returns `nil` for ungated tool; `Check()` returns without creating any DB row |
| `TestCreatePendingRow` | `Check()` creates a `tool_approval_pending` row with correct `args_sha256` |
| `TestArgsSHA256Canonical` | `SHA-256({"b":1,"a":2})` equals `SHA-256({"a":2,"b":1})` (both marshal to same sorted JSON) |
| `TestApprovedResumes` | `Check()` returns nil after `Resolve()` sends an `approved` Decision on the channel |
| `TestDeniedReturnsError` | `Check()` returns `*ApprovalDeniedError` after `Resolve()` sends a `denied` Decision |
| `TestTimeoutAutoDeny` | Auto-deny fires within 250 ms of `context.WithTimeout` deadline when `AutoDenyOnTimeout=true` |
| `TestTimeoutBlocks` | Without auto-deny, `*ApprovalTimeoutError` is returned at deadline |
| `TestAppendLogOnApprove` | Approving a pending record appends one row to `tool_approval_log` |
| `TestAppendLogOnDeny` | Denying appends one row with `decision='denied'` |
| `TestLogNoUpdateTrigger` | `db.Exec("UPDATE tool_approval_log SET decision='approved' WHERE id='x'")` returns an error containing `"append-only"` |
| `TestLogNoDeleteTrigger` | `db.Exec("DELETE FROM tool_approval_log WHERE id='x'")` returns an error containing `"append-only"` |
| `TestDuplicateRuleRejected` | Adding a duplicate `(tool, profile)` rule returns a unique-constraint error; no second DB row created |
| `TestZeroOverheadUngated` | `Check()` on ungated tool completes in < 1 ms (no DB writes); verified with `testing.B` |
| `TestEventFiredOnPending` | Creating a pending approval inserts a `tool_approval.pending` event row in the `events` table |
| `TestHashChain` | When `hashChain=true`, successive log rows have `prev_hash` equal to `SHA-256(canonical-JSON of previous row)` |

### 12.2 Integration Tests

Integration tests start the full approval server via `net/http/httptest.NewServer`, exercise the `PermissionService` against a real on-disk SQLite file, and invoke Cobra commands directly.

| Test | Description |
|------|-------------|
| `TestEndToEndCLIApprove` | Full flow: add rule, start gated run goroutine, call `cmdMCPApprove`, verify run completes and log row exists |
| `TestEndToEndCLIDeny` | Full flow: add rule, start gated run goroutine, call `cmdMCPApprove --deny`, verify goroutine returns `*ApprovalDeniedError` and denial logged |
| `TestWebhookApprove` | Start `ApprovalServer` via `httptest.NewServer`; POST to `/approvals/<id>/approve`; verify agent goroutine resumes |
| `TestWebhookDeny` | POST to `/approvals/<id>/deny`; verify agent goroutine returns error with exit code 5 |
| `TestListPendingJSON` | `cmdMCPApprovalsList --pending --json` returns valid JSON within 200 ms for 1,000 pending rows |
| `TestExportNDJSON` | `cmdMCPApprovalsExport --format ndjson` produces valid NDJSON with all required fields |
| `TestExportFilteredByProfile` | `--profile coder` filter returns only rows for coder profile |
| `TestRuleSurvivesRestart` | Add rule, close and reopen store, assert rule present via `cmdMCPApproveRequiredList` |
| `TestArgsDispatchedUnchanged` | Verify args JSON dispatched to the MCP mock equals `tool_approval_pending.args_json` byte-for-byte |
| `TestConcurrentResolve` | Two concurrent `Resolve()` calls on the same approval ID: first succeeds, second returns conflict error (NFR-04) |
| `TestSSEPendingEvent` | SSE client connected to `/approvals/events` receives a `pending-approval` event within 500 ms of `Check()` creating a pending record |

### 12.3 Performance Tests (`internal/tool/approval_bench_test.go`)

| Benchmark | Description | Target |
|-----------|-------------|--------|
| `BenchmarkGateOverheadUngated` | 1,000 ungated tool calls through `Check()` | < 1 ms per call |
| `BenchmarkListPending1k` | `cmdMCPApprovalsList --pending` with 1,000 pending rows | < 200 ms |
| `BenchmarkExport10k` | `cmdMCPApprovalsExport` streaming 10,000 log rows via cursor | < 2 s |
| `BenchmarkApproveLatency` | Time from `Resolve()` send to `Check()` return | < 1 ms (channel-based; no poll) |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification Method |
|----|-----------|---------------------|
| AC-01 | `tag mcp approve-required add --tool bash --profile coder` creates a row in `tool_approval_rules` with `profile='coder'` and `tool='bash'`. | SQLite assertion in integration test |
| AC-02 | `tag mcp approve-required add --tool github:push --always` creates a row with `mcp_server='github'`, `tool='push'`, `profile=NULL`. | SQLite assertion |
| AC-03 | A `tag run` on the `coder` profile that calls `bash` with a gated rule does NOT dispatch the bash call until `status='approved'` in `tool_approval_pending`. | Integration test: assert no MCP call before approval |
| AC-04 | `tag mcp approve <id>` transitions `tool_approval_pending.status` to `'approved'` and the agent resumes within 500 ms. | Timing assertion in integration test |
| AC-05 | `tag mcp approve <id> --deny` transitions status to `'denied'`, the run exits with code 5, and `tool_approval_log` has a row with `decision='denied'`. | Integration test |
| AC-06 | With `--timeout 5 --auto-deny-on-timeout`, a run with no reviewer fires auto-deny within 5.5 seconds and exits code 5. | Integration test with `context.WithTimeout` |
| AC-07 | `tool_approval_log` contains exactly one row per approval decision (not zero, not two). | Assert `COUNT(*) = 1` after each decision in integration tests |
| AC-08 | `db.Exec("UPDATE tool_approval_log SET decision='approved' WHERE id='x'")` returns an error whose message contains `"append-only"`. | Unit test asserting `err.Error()` contains `"append-only"` |
| AC-09 | `db.Exec("DELETE FROM tool_approval_log WHERE id='x'")` returns an error whose message contains `"append-only"`. | Unit test asserting `err.Error()` contains `"append-only"` |
| AC-10 | `tag mcp approvals list --pending --json` returns valid JSON array within 200 ms for 1,000 pending rows. | Performance test |
| AC-11 | `tag mcp approvals export --format ndjson` produces one valid JSON object per line with all fields: `log_id`, `approval_id`, `rule_id`, `tool`, `mcp_server`, `profile`, `run_id`, `args_json`, `args_sha256`, `decision`, `reviewer`, `reviewer_source`, `rationale`, `created_at`, `decided_at`. | Schema validation in integration test |
| AC-12 | A `POST /approvals/<id>/approve` to the webhook server with a `X-Approver: alice` header records `reviewer='alice'` and `reviewer_source='webhook'` in the log. | Integration test |
| AC-13 | The `args_sha256` in `tool_approval_log` matches `fmt.Sprintf("%x", sha256.Sum256(jsonBytes))` where `jsonBytes = json.Marshal(args)` (Go's map marshalling sorts keys). | Unit test with known fixture |
| AC-14 | The arguments received by the MCP server are byte-for-byte identical to `tool_approval_pending.args_json` for the corresponding approval. | Integration test with MCP mock capturing request |
| AC-15 | An ungated tool call (no matching rule) adds zero rows to `tool_approval_pending` and zero rows to `tool_approval_log`. | Unit test: assert both tables empty after ungated `Check()` |
| AC-16 | `tag mcp approve-required remove --tool bash --profile coder` removes the rule; subsequent gated runs for `coder/bash` proceed without pausing. | Integration test |
| AC-17 | Webhook server `http.Server.Addr` begins with `127.0.0.1:`, never `0.0.0.0`. | Unit test asserting `srv.Addr` prefix |
| AC-18 | Adding a duplicate rule (same tool+profile) returns a descriptive error and exits 1; no second row is created. | Integration test |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-013: Agent Tracing / Observability | Upstream | Provides `spans` table and span IDs recorded in `tool_approval_log.trace_span_id`. Gate creates a child OTel span (`go.opentelemetry.io/otel`) named `tool_approval.gate` with attributes `approval_id`, `tool_name`, `decision`, `duration_ms`. |
| PRD-016: Webhook Event Triggers | Upstream | The `events` table (populated by this PRD on `tool_approval.pending`) is consumed by PRD-016's webhook dispatcher to fire Slack/HTTP callbacks. |
| PRD-027: Eval Framework | Upstream | Eval suites can include cases that verify gated profiles pause on target tools; the eval runner must handle `*ApprovalDeniedError` gracefully in test runs. |
| PRD-028: Sandbox Execution | Related | `internal/runtime/sandbox.go` and the approval gate may both intercept `bash` calls; the approval gate fires first (pre-dispatch in `DispatchToolCall`), sandbox restrictions fire at the OS level. Both are orthogonal layers. |
| PRD-034: Secret Scanning / Security | Related | `internal/security` may scan argument payloads for secrets. The approval gate surfaces the full payload to the reviewer before any secret-containing call executes. |
| PRD-040: Notification Hooks | Upstream | `tool_approval.pending` events flow through the PRD-040 notification hook dispatch chain; Slack/email/desktop channels must be configured there. |
| PRD-048: Structured Tool Call Spans | Related | Approval gate creates a `tool_approval.gate` OTel span as a child of the existing tool call span created by PRD-048. |
| `modernc.org/sqlite` | Runtime | Pure-Go SQLite driver (`CGO_ENABLED=0`); FTS5 built-in; single-writer via `gofrs/flock`. Replaces Python `sqlite3`/`aiosqlite`. |
| `go-chi/chi v5` | Runtime | HTTP router for the approval webhook server. |
| `tmaxmax/go-sse` | Runtime | Spec-compliant SSE with `Last-Event-ID` replay for streaming pending-approval events to clients. |
| `crypto/sha256` | stdlib | SHA-256 of canonical argument JSON and optional log row hash-chaining. Replaces Python `hashlib`. |
| `encoding/json` | stdlib | Canonical JSON serialisation (map keys sorted by default). Replaces Python `json`. |
| `sync`, `context` | stdlib | Mutex for `pending` map; `context.WithTimeout` for gate deadline. Replaces Python `threading` / `asyncio`. |
| `os/user` | stdlib | `user.Current().Username` for reviewer identity. Replaces Python `os.getlogin()` / `os.environ.get("USER")`. |
| `go.opentelemetry.io/otel` | Runtime | OTel spans for approval gate duration (PRD-013/048). |
| `knadh/koanf/v2` | Runtime | Config loading and profile YAML merge for `approval.*` keys. |
| `github.com/spf13/cobra` | Runtime | CLI command registration for all `tag mcp approve*` sub-commands. |

---

## 15. Open Questions

| # | Question | Impact | Owner | Target Resolution |
|---|----------|--------|-------|-------------------|
| OQ-1 | Should the approval gate support a **detached/async mode** where the agent serializes its entire state to disk, releases the process, and a `tag run --resume <run-id>` picks up execution after approval? This would eliminate the need for a long-running agent process during review. | High — fundamental to CI/CD and long-running agent use cases | Arch | Phase 2 decision |
| OQ-2 | Should `--rationale` be **required** (not optional) for compliance profiles? A `require_rationale: true` config key could enforce this. | Medium — affects compliance story | Security | Before Phase 1 implementation |
| OQ-3 | What is the **argument size limit** for the `args_json` column? If a bash call includes a file diff as an argument, the stored payload could be megabytes. Should a truncation policy exist with a hash still covering the full payload? | Medium — storage and UX | Eng | Phase 1 spec review |
| OQ-4 | Should the **webhook server use a shared secret token** in this PRD, or is localhost-only isolation sufficient for the initial release? | High — security posture | Security | Phase 1 go/no-go |
| OQ-5 | Should `tag mcp approvals export` support a **streaming cursor mode** for very large log tables (> 1M rows) via pagination? | Low for initial deployment; high for enterprise | Eng | Phase 2 |
| OQ-6 | Can the approval gate be **bypassed per-session** with an explicit `--no-approval-gate` flag for power users running trusted local profiles? If yes, does bypassing it require a log entry? | Medium — developer ergonomics vs. security | Product | Phase 1 |
| OQ-7 | Should the `tool_approval_log` support **external export to SIEM** (Splunk, Datadog, CloudWatch) via an OTel exporter (PRD-041), or is NDJSON file export sufficient for the first iteration? | Medium — enterprise adoption | Product | Phase 2 |
| OQ-8 | How should the gate behave when `tag run` is invoked with `--non-interactive` (no TTY) and no timeout is configured? Currently it would block forever on the channel. Should non-interactive mode force `--auto-deny-on-timeout` with a default of 300 seconds? | High — CI safety | Eng | Phase 1 |

---

## 16. Complexity and Timeline

### Phase 1 — Core Gate + SQLite (Days 1–6)

| Day | Work |
|-----|------|
| 1–2 | `migratePRD078Tables` DDL in `internal/store/migrate.go`; `internal/tool/approval.go` Go structs (`ApprovalRule`, `Decision`, `pendingApproval`, error types) and `PermissionService` skeleton; unit tests for rule matching and `canonicalArgsSHA256` |
| 3–4 | `PermissionService.Check()` full implementation: `createPending` (single-transaction DB write + event row), channel-based blocking `select`, `context.WithTimeout` deadline, auto-deny path, `appendLog` with optional hash-chain (`crypto/sha256`) |
| 5   | `migratePRD078Tables` integrated into `store.Open()` migration chain; append-only triggers; trigger enforcement unit tests asserting `err.Error()` contains `"append-only"` |
| 6   | `internal/runtime/dispatch.go` integration: inject `ps.Check()` call; integration test verifying MCP mock receives no call before `Resolve()` is called |

### Phase 2 — CLI Commands (Days 7–11)

| Day | Work |
|-----|------|
| 7–8 | `cmdMCPApproveRequiredAdd`, `cmdMCPApproveRequiredRemove`, `cmdMCPApproveRequiredList` in `internal/cli/mcp_approve.go`; cobra wiring under `tag mcp approve-required`; UNIQUE constraint error handling; unit + integration tests |
| 9   | `cmdMCPApprovalsList`, `cmdMCPApprovalsShow`, `cmdMCPApprovalsExport`; `--pending` filter; lazy NDJSON streaming via `*sql.Rows` cursor (NFR-03) |
| 10  | `cmdMCPApprove` (approve/deny with rationale); `os/user.Current().Username` reviewer capture; exit code mapping; in-process vs. webhook-POST resolution path |
| 11  | Manual CLI smoke tests; edge-case fixes |

### Phase 3 — Webhook Server + Notifications (Days 12–16)

| Day | Work |
|-----|------|
| 12–13 | `internal/server/approval.go`: `ApprovalServer` with `go-chi/chi v5` router + `tmaxmax/go-sse` SSE endpoint; `StartApprovalServer` goroutine + context-cancel shutdown; bind assertion `127.0.0.1` |
| 14  | `StartApprovalServer` wired into `tag run` startup when `approval.webhook_port` is configured (loaded via `koanf/v2`); PRD-040 notification hook integration for `tool_approval.pending` events; desktop notification in `PermissionService.fireNotification()` |
| 15  | Webhook integration tests via `httptest.NewServer`: approve/deny via HTTP, `X-Approver` header, concurrent request race condition, SSE event delivery |
| 16  | Performance benchmarks; zero-overhead ungated tool call benchmark (NFR-06) |

### Phase 4 — Hardening + Docs (Days 17–20)

| Day | Work |
|-----|------|
| 17  | Edge case coverage: duplicate rule rejection, expired approval CLI error, non-interactive mode auto-deny behavior (OQ-8), `--no-approval-gate` bypass flag (OQ-6) |
| 18  | `tag doctor` check for `tool_approval_rules` table existence and pending approvals count |
| 19  | Security review: webhook `127.0.0.1` binding assertion test, rate-limit implementation (SC-7), argument size handling (OQ-3), hash-chain verification utility |
| 20  | Final integration test pass; `go test -coverprofile` assertion (> 90% for `internal/tool/approval.go`); PR review |

**Total estimate:** 20 engineering days (4 weeks for a single developer, ~2 weeks with two developers working Phase 1/2 in parallel with Phase 3/4).
