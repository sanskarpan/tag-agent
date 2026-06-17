# PRD-078: Human-in-the-Loop Tool Approval with Pause/Resume + Audit Trail (`tag mcp approve`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `controller.py + tool_approval SQLite table`
**Depends on:** PRD-013 (agent tracing/observability), PRD-016 (webhook event triggers), PRD-027 (eval framework), PRD-028 (sandbox execution), PRD-034 (secret scanning/security), PRD-040 (notification hooks), PRD-048 (structured tool call spans)
**Inspired by:** Arcade AI human-in-the-loop, OpenAI Agents SDK guardrails, SOC-2 audit

---

## 1. Overview

Autonomous AI agents executing tool calls against production systems — pushing code, running shell commands, sending emails, modifying databases — represent a fundamental enterprise security risk when those calls are entirely unmonitored. Most organizations that want to adopt agentic AI are blocked not by capability gaps but by governance gaps: they cannot tell a CISO what an agent did, prove an action was authorized, or guarantee a human reviewed a destructive call before it executed. This is the problem PRD-078 solves.

Human-in-the-Loop (HITL) Tool Approval adds a first-class pause/resume gate into the TAG agent execution pipeline. Any MCP tool call — whether it targets a local bash executor, a GitHub API, a database ORM, or a cloud provisioning endpoint — can be marked as requiring human review before it is dispatched. When the agent reaches that call, execution suspends entirely. The pending approval is persisted to the `tool_approval` SQLite table, a desktop/webhook notification fires, and the agent waits (blocking or background, configurable) until a reviewer approves or denies the call via `tag mcp approve <approval-id>` or a matching webhook `POST`.

The design is modeled on Arcade AI's permission intersection model — the effective permission is `Agent ∩ User`, not `Agent ∪ User`. A tool can only execute if the agent's profile authorizes it AND the human reviewer approves this specific invocation. Approval policy is orthogonal to tool grant: granting a tool does not bypass the approval gate if the tool is also on the approve-required list. This separation of concerns is deliberate: it allows granting broad tool access to a profile while still requiring human sign-off for any call that targets a specific MCP server or matches a destructive pattern.

The audit trail is the equal partner of the gate mechanism. Every approval decision — approve, deny, or timeout — is appended to the `tool_approval_log` table with: ISO-8601 timestamp, reviewer identity (local username or webhook caller), approval ID, tool name, MCP server, full argument payload SHA-256, a verbatim copy of the argument payload, the decision, and a free-text rationale. This log is append-only, indexed, and can be exported as NDJSON for ingestion into SIEM tools. It satisfies the evidence trail requirements for SOC-2 Type II CC6.1 (logical access controls) and CC7.2 (monitoring of system operations).

The feature is designed to compose cleanly with TAG's existing systems. Approved/denied decisions surface as `tool_approval.*` events in the `events` table (PRD-040 notification hooks). Each approval creates a span child in the active trace (PRD-013 / PRD-048). The approval gate can be driven from CI with `--auto-deny-on-timeout` to prevent runaway agents in automated pipelines. Webhook-driven approval (for Slack bots, PagerDuty integrations, or custom approval portals) is first-class: the gate polls an internal HTTP server or SQLite flag rather than keeping a persistent connection.

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
| U5 | Reviewer | run `tag mcp approve abc-123-def` with an optional `--rationale "Confirmed: safe to delete tmp"` | The call is authorized, the agent resumes, and my rationale is persisted in the audit log |
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
```

Request body (optional for approve/deny):
```json
{"rationale": "Approved via Slack bot", "reviewer": "alice@example.com"}
```

The `X-Approver` header overrides the `reviewer` field in the body. Response is the logged decision JSON. The server binds to localhost only and requires no authentication in this PRD (see Security Considerations).

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
| FR-09 | `tag mcp approve <id>` MUST record the OS username (`os.getlogin()` or `$USER` env var) as the `reviewer` field in `tool_approval_log`. | Must |
| FR-10 | The `args_sha256` field in `tool_approval_log` MUST be the SHA-256 of the canonical JSON serialization of the tool argument payload (keys sorted, no whitespace). | Must |
| FR-11 | The arguments dispatched to the MCP server MUST be byte-for-byte identical to the arguments that were presented for review (captured at gate time). The agent cannot re-generate arguments after approval. | Must |
| FR-12 | `tag mcp approvals list --pending --json` MUST return results within 200 ms for up to 10,000 log rows. | Must |
| FR-13 | Each pending approval creation MUST fire a `tool_approval.pending` event into the `events` table, enabling PRD-040 notification hooks (Slack, desktop, email). | Must |
| FR-14 | Approval gate creates a child span in the active trace (PRD-013) with name `tool_approval.gate`, attributes including `approval_id`, `tool_name`, `decision`, and `duration_ms`. | Should |
| FR-15 | The webhook approval server MUST bind to `127.0.0.1` only, never `0.0.0.0`. | Must |
| FR-16 | `tag mcp approve-required remove` MUST remove only the matching rule; running the command with a non-matching tool+profile MUST exit 1 with a descriptive error. | Must |
| FR-17 | `tag mcp approvals export` MUST produce valid NDJSON with all required fields as specified in §7.7, with one JSON object per line. | Must |
| FR-18 | Adding or removing an approval rule MUST NOT require restarting an in-progress `tag run`; the agent loop re-checks the rules table on every tool call. | Should |
| FR-19 | `tag mcp approvals list --pending` MUST show the first 200 characters of the argument JSON as a preview column. | Should |
| FR-20 | When `--auto-deny-on-timeout` is set, the timeout MUST be enforced by a background polling thread within the approval gate, not by the caller waiting on a subprocess. | Must |
| FR-21 | `tool_approval_rules` MUST enforce a UNIQUE constraint on `(tool, mcp_server, profile)` to prevent duplicate rules. Attempting to add a duplicate MUST fail with a descriptive error and exit 1. | Must |
| FR-22 | `tag mcp approvals show <id>` MUST display the full (untruncated) argument JSON payload. | Must |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Approval gate overhead (time added to a tool call that requires approval AND is immediately approved programmatically) MUST be less than 50 ms in the happy path with the reviewer already polling. | < 50 ms |
| NFR-02 | The `tool_approval_log` table MUST use append-only semantics enforced by a `BEFORE UPDATE` and `BEFORE DELETE` SQLite trigger that raises an error. | Enforced at DB layer |
| NFR-03 | `tag mcp approvals export` MUST stream rows lazily (no full-table load into memory) for exports exceeding 10,000 rows. | Streaming cursor |
| NFR-04 | The webhook approval server MUST handle concurrent approve/deny requests without race conditions; all state transitions MUST use `UPDATE ... WHERE status='pending'` with row-count assertion. | Atomic update |
| NFR-05 | The approval gate MUST work in both blocking mode (agent process waits) and detached mode (agent writes pending record and exits; a separate `tag run --resume` picks it up after approval). | Both modes |
| NFR-06 | The approval gate MUST be completely inert (zero overhead, zero SQLite queries) when no approval rules are configured. Rules presence check is cached per-session. | Zero overhead |
| NFR-07 | All approval-related SQLite writes MUST complete within a single transaction to prevent partial state (e.g., pending record created without matching span). | Atomic |
| NFR-08 | `tool_approval_log` rows MUST be indexed by `(created_at, profile, tool, decision)` to support sub-100 ms export queries with time and filter constraints. | < 100 ms query |
| NFR-09 | The module implementing approval gate logic (`src/tag/tool_approval.py`) MUST have > 90% line coverage in unit tests. | > 90% |
| NFR-10 | Error messages on approval denial MUST include the approval ID, tool name, reviewer, rationale (if set), and a pointer to `tag mcp approvals show <id>` for full detail. | Human-readable |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/tool_approval.py` | Core approval gate logic: rule lookup, pending record creation, blocking poll, timeout enforcement, audit log append |
| `src/tag/approval_server.py` | Lightweight HTTP server (stdlib `http.server`) for webhook-based approvals |
| `tests/test_tool_approval.py` | Unit and integration tests for all gate paths |

**Modifications to existing files:**
- `src/tag/controller.py`: New `cmd_mcp_approve_required`, `cmd_mcp_approvals`, `cmd_mcp_approve` handler functions; new `_migrate_prd_078_tables` migration function; CLI parser additions under `tag mcp` subcommand tree.
- `src/tag/hermes_bridge.py`: Inject `ApprovalGate.check()` call in the tool dispatch path, before the MCP server call is made.

### 10.2 SQLite DDL

All three new tables are created in `_migrate_prd_078_tables(conn)` which is called from `open_db()` following the established pattern.

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
    args_json    TEXT NOT NULL,           -- verbatim argument JSON payload
    args_sha256  TEXT NOT NULL,           -- SHA-256 of canonical args (sorted keys, no whitespace)
    status       TEXT NOT NULL DEFAULT 'pending',  -- 'pending' | 'approved' | 'denied' | 'timeout'
    reviewer     TEXT,                    -- OS username or webhook X-Approver
    reviewer_source TEXT,                 -- 'cli' | 'webhook' | 'auto'
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

### 10.3 Core Dataclasses

```python
# src/tag/tool_approval.py
from __future__ import annotations

import hashlib
import json
import os
import sqlite3
import threading
import time
import uuid
from dataclasses import dataclass, field
from typing import Optional


def _ulid(prefix: str) -> str:
    """Generate a prefixed pseudo-ULID using uuid4 for uniqueness."""
    return f"{prefix}_{uuid.uuid4().hex[:26].upper()}"


def _canonical_args_sha256(args: dict) -> str:
    """SHA-256 of canonical JSON: keys sorted, no whitespace."""
    canonical = json.dumps(args, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode()).hexdigest()


@dataclass
class ApprovalRule:
    id: str
    tool: str
    mcp_server: Optional[str]
    profile: Optional[str]          # None means --always (global)
    timeout_seconds: Optional[int]
    auto_deny_on_timeout: bool
    notify_channels: list[str]
    created_at: str
    created_by: str


@dataclass
class PendingApproval:
    id: str
    rule_id: str
    run_id: str
    trace_span_id: Optional[str]
    tool: str
    mcp_server: Optional[str]
    profile: str
    args_json: str
    args_sha256: str
    status: str                     # 'pending' | 'approved' | 'denied' | 'timeout'
    reviewer: Optional[str]
    reviewer_source: Optional[str]
    rationale: Optional[str]
    created_at: str
    decided_at: Optional[str]
    expires_at: Optional[str]


@dataclass
class ApprovalDecision:
    approval_id: str
    decision: str                   # 'approved' | 'denied' | 'timeout'
    reviewer: str
    reviewer_source: str            # 'cli' | 'webhook' | 'auto'
    rationale: Optional[str]
    decided_at: str
```

### 10.4 ApprovalGate Class

```python
# src/tag/tool_approval.py (continued)

import datetime as dt


class ApprovalGate:
    """
    Central controller for the HITL approval gate.

    Call ApprovalGate.check(conn, tool, mcp_server, profile, run_id, args)
    from the hermes_bridge tool dispatch path. The method returns only when
    a decision is made (approved), raises ApprovalDeniedError on denial, or
    raises ApprovalTimeoutError if auto-deny fires.
    """

    POLL_INTERVAL_SECONDS = 0.25

    def __init__(self, conn: sqlite3.Connection) -> None:
        self._conn = conn
        # Cache: None means "not yet fetched this session"
        self._rules_cache: Optional[list[ApprovalRule]] = None
        self._cache_ts: float = 0.0
        self._cache_ttl: float = 5.0  # seconds; re-read rules every 5s

    def _load_rules(self) -> list[ApprovalRule]:
        now = time.monotonic()
        if self._rules_cache is None or (now - self._cache_ts) > self._cache_ttl:
            rows = self._conn.execute(
                "SELECT * FROM tool_approval_rules ORDER BY profile NULLS LAST"
            ).fetchall()
            self._rules_cache = [
                ApprovalRule(
                    id=r["id"],
                    tool=r["tool"],
                    mcp_server=r["mcp_server"],
                    profile=r["profile"],
                    timeout_seconds=r["timeout_seconds"],
                    auto_deny_on_timeout=bool(r["auto_deny_on_timeout"]),
                    notify_channels=json.loads(r["notify_channels"] or "[]"),
                    created_at=r["created_at"],
                    created_by=r["created_by"],
                )
                for r in rows
            ]
            self._cache_ts = now
        return self._rules_cache

    def match_rule(self, tool: str, mcp_server: Optional[str], profile: str) -> Optional[ApprovalRule]:
        """Return the most specific matching rule, or None if tool is ungated."""
        rules = self._load_rules()
        # Profile-scoped rule takes precedence over global; within same scope, first match wins.
        profile_match: Optional[ApprovalRule] = None
        global_match: Optional[ApprovalRule] = None
        for rule in rules:
            tool_matches = rule.tool == tool or (
                mcp_server is not None and rule.tool == f"{mcp_server}:{tool}"
            )
            if not tool_matches:
                continue
            if rule.mcp_server is not None and rule.mcp_server != mcp_server:
                continue
            if rule.profile is None:
                if global_match is None:
                    global_match = rule
            elif rule.profile == profile:
                if profile_match is None:
                    profile_match = rule
        return profile_match or global_match

    def check(
        self,
        tool: str,
        mcp_server: Optional[str],
        profile: str,
        run_id: str,
        args: dict,
        trace_span_id: Optional[str] = None,
    ) -> None:
        """
        Gate the tool call. Returns normally if approved. Raises on denial.
        Must be called from the tool dispatch path in hermes_bridge.py before
        the MCP server call is made.
        """
        rule = self.match_rule(tool, mcp_server, profile)
        if rule is None:
            return  # Fast path: ungated tool, zero overhead beyond dict lookup

        pending = self._create_pending(rule, tool, mcp_server, profile, run_id, args, trace_span_id)
        self._fire_notification(pending, rule)
        self._block_until_decided(pending, rule)

    def _create_pending(
        self,
        rule: ApprovalRule,
        tool: str,
        mcp_server: Optional[str],
        profile: str,
        run_id: str,
        args: dict,
        trace_span_id: Optional[str],
    ) -> PendingApproval:
        approval_id = _ulid("appr")
        now_iso = dt.datetime.utcnow().isoformat() + "Z"
        args_json = json.dumps(args, sort_keys=True, separators=(",", ":"))
        args_sha256 = _canonical_args_sha256(args)
        expires_at: Optional[str] = None
        if rule.timeout_seconds is not None:
            expires_dt = dt.datetime.utcnow() + dt.timedelta(seconds=rule.timeout_seconds)
            expires_at = expires_dt.isoformat() + "Z"

        self._conn.execute(
            """
            INSERT INTO tool_approval_pending
              (id, rule_id, run_id, trace_span_id, tool, mcp_server, profile,
               args_json, args_sha256, status, created_at, expires_at)
            VALUES (?,?,?,?,?,?,?,?,?,'pending',?,?)
            """,
            (approval_id, rule.id, run_id, trace_span_id, tool, mcp_server,
             profile, args_json, args_sha256, now_iso, expires_at),
        )
        # Fire event for PRD-040 notification hooks
        event_id = _ulid("evt")
        self._conn.execute(
            """
            INSERT INTO events (id, event_type, profile, run_id, payload, created_at)
            VALUES (?, 'tool_approval.pending', ?, ?, ?, ?)
            """,
            (event_id, profile, run_id,
             json.dumps({"approval_id": approval_id, "tool": tool, "profile": profile}),
             now_iso),
        )
        self._conn.commit()
        return PendingApproval(
            id=approval_id, rule_id=rule.id, run_id=run_id,
            trace_span_id=trace_span_id, tool=tool, mcp_server=mcp_server,
            profile=profile, args_json=args_json, args_sha256=args_sha256,
            status="pending", reviewer=None, reviewer_source=None, rationale=None,
            created_at=now_iso, decided_at=None, expires_at=expires_at,
        )

    def _block_until_decided(self, pending: PendingApproval, rule: ApprovalRule) -> None:
        """Poll SQLite until status leaves 'pending'. Enforce timeout if configured."""
        deadline: Optional[float] = None
        if rule.timeout_seconds is not None:
            deadline = time.monotonic() + rule.timeout_seconds

        while True:
            row = self._conn.execute(
                "SELECT status, reviewer, reviewer_source, rationale, decided_at "
                "FROM tool_approval_pending WHERE id = ?",
                (pending.id,),
            ).fetchone()
            if row is None:
                raise ApprovalGateError(f"Approval record {pending.id} disappeared from DB.")

            status = row["status"]
            if status == "approved":
                self._append_log(pending, "approved", row["reviewer"], row["reviewer_source"], row["rationale"], row["decided_at"])
                return
            if status in ("denied", "timeout"):
                self._append_log(pending, status, row["reviewer"], row["reviewer_source"], row["rationale"], row["decided_at"])
                raise ApprovalDeniedError(pending.id, pending.tool, status, row["rationale"])

            # Check for timeout
            if deadline is not None and time.monotonic() > deadline:
                if rule.auto_deny_on_timeout:
                    self._record_timeout(pending)
                    raise ApprovalDeniedError(pending.id, pending.tool, "timeout", "Auto-denied: approval timeout expired")
                else:
                    raise ApprovalTimeoutError(pending.id, rule.timeout_seconds)

            time.sleep(self.POLL_INTERVAL_SECONDS)

    def _record_timeout(self, pending: PendingApproval) -> None:
        now_iso = dt.datetime.utcnow().isoformat() + "Z"
        self._conn.execute(
            """UPDATE tool_approval_pending
               SET status='timeout', reviewer='system', reviewer_source='auto',
                   rationale='Auto-denied: timeout expired', decided_at=?
               WHERE id=? AND status='pending'""",
            (now_iso, pending.id),
        )
        self._conn.commit()

    def _append_log(
        self,
        pending: PendingApproval,
        decision: str,
        reviewer: Optional[str],
        reviewer_source: Optional[str],
        rationale: Optional[str],
        decided_at: Optional[str],
    ) -> None:
        log_id = _ulid("log")
        now_iso = dt.datetime.utcnow().isoformat() + "Z"
        self._conn.execute(
            """
            INSERT INTO tool_approval_log
              (id, approval_id, rule_id, tool, mcp_server, profile, run_id,
               trace_span_id, args_json, args_sha256, decision, reviewer,
               reviewer_source, rationale, created_at, decided_at)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
            """,
            (log_id, pending.id, pending.rule_id, pending.tool, pending.mcp_server,
             pending.profile, pending.run_id, pending.trace_span_id,
             pending.args_json, pending.args_sha256, decision,
             reviewer, reviewer_source or "cli", rationale,
             pending.created_at, decided_at or now_iso),
        )
        self._conn.commit()

    def _fire_notification(self, pending: PendingApproval, rule: ApprovalRule) -> None:
        """Fire desktop notification if desktop channel is configured."""
        if "desktop" in rule.notify_channels:
            try:
                send_desktop_notification(
                    title=f"TAG: Approval Required — {pending.tool}",
                    message=f"Profile: {pending.profile}\nID: {pending.id}\nArgs: {pending.args_json[:120]}",
                )
            except Exception:
                pass  # Never block the gate on notification failures


class ApprovalGateError(Exception):
    pass


class ApprovalDeniedError(ApprovalGateError):
    def __init__(self, approval_id: str, tool: str, decision: str, rationale: Optional[str]):
        self.approval_id = approval_id
        self.tool = tool
        self.decision = decision
        self.rationale = rationale
        super().__init__(
            f"Tool call '{tool}' was {decision} (approval: {approval_id}). "
            f"Reason: {rationale or '(none)'}. "
            f"See: tag mcp approvals show {approval_id}"
        )


class ApprovalTimeoutError(ApprovalGateError):
    def __init__(self, approval_id: str, timeout_seconds: int):
        self.approval_id = approval_id
        super().__init__(
            f"Approval {approval_id} timed out after {timeout_seconds}s. "
            f"Configure auto_deny_on_timeout=true to auto-deny instead of blocking."
        )
```

### 10.5 Integration Point: hermes_bridge.py

The gate is inserted in the tool dispatch path. The exact hook site is the pre-execution callback before any MCP server transport call:

```python
# src/tag/hermes_bridge.py — in the tool call dispatch function
# (pseudocode showing where ApprovalGate.check is inserted)

from tag.tool_approval import ApprovalGate, ApprovalDeniedError

_approval_gate: Optional[ApprovalGate] = None

def _get_gate(conn: sqlite3.Connection) -> ApprovalGate:
    global _approval_gate
    if _approval_gate is None:
        _approval_gate = ApprovalGate(conn)
    return _approval_gate

def dispatch_tool_call(
    tool_name: str,
    mcp_server: Optional[str],
    profile: str,
    run_id: str,
    args: dict,
    conn: sqlite3.Connection,
    trace_span_id: Optional[str] = None,
) -> dict:
    gate = _get_gate(conn)
    try:
        gate.check(tool_name, mcp_server, profile, run_id, args, trace_span_id)
    except ApprovalDeniedError as exc:
        # Surface as a tool error response so the agent loop handles it gracefully
        return {"error": str(exc), "approval_denied": True, "exit_code": 5}
    # ... proceed with actual MCP server call
```

### 10.6 Webhook Approval Server

```python
# src/tag/approval_server.py

import json
import os
import sqlite3
import threading
import datetime as dt
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Optional

from tag.tool_approval import _ulid


class ApprovalHandler(BaseHTTPRequestHandler):
    """Handles POST /approvals/<id>/approve|deny and GET /approvals[/<id>]."""

    db_path: str = ""  # Set before starting server

    def _conn(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path, timeout=5)
        conn.row_factory = sqlite3.Row
        return conn

    def _respond(self, status: int, body: dict) -> None:
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self) -> None:
        parts = self.path.strip("/").split("/")
        if parts == ["approvals", "pending"]:
            conn = self._conn()
            rows = conn.execute(
                "SELECT * FROM tool_approval_pending WHERE status='pending' ORDER BY created_at DESC LIMIT 50"
            ).fetchall()
            conn.close()
            self._respond(200, {"approvals": [dict(r) for r in rows]})
        elif len(parts) == 2 and parts[0] == "approvals":
            approval_id = parts[1]
            conn = self._conn()
            row = conn.execute(
                "SELECT * FROM tool_approval_pending WHERE id=?", (approval_id,)
            ).fetchone()
            conn.close()
            if row is None:
                self._respond(404, {"error": "Not found"})
            else:
                self._respond(200, dict(row))
        else:
            self._respond(404, {"error": "Not found"})

    def do_POST(self) -> None:
        parts = self.path.strip("/").split("/")
        # POST /approvals/<id>/approve or /deny
        if len(parts) != 3 or parts[0] != "approvals" or parts[2] not in ("approve", "deny"):
            self._respond(404, {"error": "Not found"})
            return
        approval_id = parts[1]
        action = parts[2]
        decision = "approved" if action == "approve" else "denied"

        length = int(self.headers.get("Content-Length", "0"))
        body: dict = {}
        if length > 0:
            body = json.loads(self.rfile.read(length))

        reviewer = (
            self.headers.get("X-Approver")
            or body.get("reviewer")
            or os.environ.get("USER", "webhook")
        )
        rationale = body.get("rationale")
        now_iso = dt.datetime.utcnow().isoformat() + "Z"

        conn = self._conn()
        rowcount = conn.execute(
            """UPDATE tool_approval_pending
               SET status=?, reviewer=?, reviewer_source='webhook', rationale=?, decided_at=?
               WHERE id=? AND status='pending'""",
            (decision, reviewer, rationale, now_iso, approval_id),
        ).rowcount
        conn.commit()
        conn.close()

        if rowcount == 0:
            self._respond(409, {"error": "Approval not found or already decided"})
        else:
            self._respond(200, {"approval_id": approval_id, "decision": decision, "reviewer": reviewer})

    def log_message(self, format: str, *args) -> None:  # noqa: A002
        pass  # Suppress default HTTP server logging


def start_approval_server(db_path: str, port: int) -> threading.Thread:
    """Start the approval webhook server in a daemon thread. Returns the thread."""
    ApprovalHandler.db_path = db_path

    server = HTTPServer(("127.0.0.1", port), ApprovalHandler)

    def _serve():
        server.serve_forever()

    thread = threading.Thread(target=_serve, daemon=True, name="approval-server")
    thread.start()
    return thread
```

### 10.7 Migration Function (controller.py)

```python
def _migrate_prd_078_tables(conn: sqlite3.Connection) -> None:
    """Create tool_approval tables for PRD-078 (idempotent)."""
    try:
        conn.executescript("""
            -- PRD-078: HITL Tool Approval
            CREATE TABLE IF NOT EXISTS tool_approval_rules (
                id                   TEXT PRIMARY KEY,
                tool                 TEXT NOT NULL,
                mcp_server           TEXT,
                profile              TEXT,
                timeout_seconds      INTEGER,
                auto_deny_on_timeout INTEGER NOT NULL DEFAULT 0,
                notify_channels      TEXT NOT NULL DEFAULT '[]',
                created_at           TEXT NOT NULL,
                created_by           TEXT NOT NULL,
                UNIQUE(tool, mcp_server, profile)
            );
            CREATE INDEX IF NOT EXISTS idx_tar_profile ON tool_approval_rules(profile, tool);
            CREATE INDEX IF NOT EXISTS idx_tar_tool    ON tool_approval_rules(tool, mcp_server);

            CREATE TABLE IF NOT EXISTS tool_approval_pending (
                id              TEXT PRIMARY KEY,
                rule_id         TEXT NOT NULL,
                run_id          TEXT NOT NULL,
                trace_span_id   TEXT,
                tool            TEXT NOT NULL,
                mcp_server      TEXT,
                profile         TEXT NOT NULL,
                args_json       TEXT NOT NULL,
                args_sha256     TEXT NOT NULL,
                status          TEXT NOT NULL DEFAULT 'pending',
                reviewer        TEXT,
                reviewer_source TEXT,
                rationale       TEXT,
                created_at      TEXT NOT NULL,
                decided_at      TEXT,
                expires_at      TEXT,
                FOREIGN KEY(rule_id) REFERENCES tool_approval_rules(id),
                FOREIGN KEY(run_id)  REFERENCES runs(id)
            );
            CREATE INDEX IF NOT EXISTS idx_tap_status  ON tool_approval_pending(status, created_at);
            CREATE INDEX IF NOT EXISTS idx_tap_run     ON tool_approval_pending(run_id, status);
            CREATE INDEX IF NOT EXISTS idx_tap_profile ON tool_approval_pending(profile, tool, status);

            CREATE TABLE IF NOT EXISTS tool_approval_log (
                id              TEXT PRIMARY KEY,
                approval_id     TEXT NOT NULL,
                rule_id         TEXT NOT NULL,
                tool            TEXT NOT NULL,
                mcp_server      TEXT,
                profile         TEXT NOT NULL,
                run_id          TEXT NOT NULL,
                trace_span_id   TEXT,
                args_json       TEXT NOT NULL,
                args_sha256     TEXT NOT NULL,
                decision        TEXT NOT NULL,
                reviewer        TEXT,
                reviewer_source TEXT NOT NULL DEFAULT 'cli',
                rationale       TEXT,
                created_at      TEXT NOT NULL,
                decided_at      TEXT NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_tal_decided  ON tool_approval_log(decided_at DESC);
            CREATE INDEX IF NOT EXISTS idx_tal_profile  ON tool_approval_log(profile, tool, decided_at);
            CREATE INDEX IF NOT EXISTS idx_tal_decision ON tool_approval_log(decision, decided_at);

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
        """)
        conn.commit()
    except sqlite3.OperationalError:
        pass
```

### 10.8 CLI Handler Sketch (controller.py)

```python
def cmd_mcp_approve_required(args: argparse.Namespace) -> int:
    """Handles: tag mcp approve-required {add,remove,list}"""
    cfg = load_config()
    conn = open_db(cfg)
    sub = args.approve_required_subcommand

    if sub == "add":
        if not args.tool:
            print_error("--tool is required")
            return 1
        if args.profile and args.always:
            print_error("--profile and --always are mutually exclusive")
            return 1
        profile = None if args.always else args.profile
        rule_id = _ulid("rule")
        now_iso = dt.datetime.utcnow().isoformat() + "Z"
        reviewer = os.environ.get("USER", "unknown")
        tool_raw = args.tool
        mcp_server = None
        if ":" in tool_raw:
            mcp_server, tool_name = tool_raw.split(":", 1)
        else:
            tool_name = tool_raw
        notify_channels = json.dumps(args.notify.split(",") if args.notify else [])
        try:
            conn.execute(
                """INSERT INTO tool_approval_rules
                   (id, tool, mcp_server, profile, timeout_seconds,
                    auto_deny_on_timeout, notify_channels, created_at, created_by)
                   VALUES (?,?,?,?,?,?,?,?,?)""",
                (rule_id, tool_name, mcp_server, profile,
                 args.timeout, int(args.auto_deny_on_timeout),
                 notify_channels, now_iso, reviewer),
            )
            conn.commit()
        except sqlite3.IntegrityError:
            print_error(f"Approval rule already exists for tool='{tool_name}' profile='{profile}'")
            return 1
        if args.json:
            print(json.dumps({"rule_id": rule_id, "tool": tool_name, "profile": profile}))
        else:
            scope = f"profile={profile}" if profile else "always (global)"
            print_success(f"Approval rule created: {rule_id} — tool={tool_name} scope={scope}")
        return 0

    elif sub == "remove":
        # Match by (tool, profile) or --rule-id
        # ... (similar pattern)
        pass

    elif sub == "list":
        rows = conn.execute("SELECT * FROM tool_approval_rules ORDER BY created_at").fetchall()
        if args.json:
            print(json.dumps([dict(r) for r in rows], indent=2))
        else:
            # Tabular output
            pass
        return 0

    return 1


def cmd_mcp_approve(args: argparse.Namespace) -> int:
    """Handles: tag mcp approve <approval-id> [--deny] [--rationale TEXT]"""
    cfg = load_config()
    conn = open_db(cfg)
    approval_id = args.approval_id
    decision = "denied" if args.deny else "approved"
    reviewer = os.environ.get("USER", "unknown")
    now_iso = dt.datetime.utcnow().isoformat() + "Z"

    row = conn.execute(
        "SELECT * FROM tool_approval_pending WHERE id=?", (approval_id,)
    ).fetchone()
    if row is None:
        print_error(f"Approval '{approval_id}' not found.")
        return 1
    if row["status"] != "pending":
        print_error(f"Approval '{approval_id}' is already {row['status']}.")
        return 2
    if row["expires_at"] and row["expires_at"] < now_iso:
        print_error(f"Approval '{approval_id}' has expired.")
        return 3

    conn.execute(
        """UPDATE tool_approval_pending
           SET status=?, reviewer=?, reviewer_source='cli', rationale=?, decided_at=?
           WHERE id=? AND status='pending'""",
        (decision, reviewer, args.rationale, now_iso, approval_id),
    )
    conn.commit()
    # The ApprovalGate polling loop in the agent process will detect the status change
    # and append the log row on its next poll cycle.
    print_success(f"{decision.upper()}: {approval_id} — tool={row['tool']} profile={row['profile']}")
    return 0
```

### 10.9 Config Schema Additions

New keys under the profile config YAML (optional, defaults shown):

```yaml
approval:
  webhook_port: null          # null = disabled; set to integer to enable webhook server
  auto_deny_timeout_seconds: null  # null = wait forever (overridable per rule)
  notify_channels: []         # default notification channels for all approval rules
```

---

## 11. Security Considerations

1. **Localhost-only webhook server.** The approval webhook server MUST bind to `127.0.0.1` only. It must never bind to `0.0.0.0` or a public interface, preventing remote attackers from approving tool calls. This is enforced in `approval_server.py` and verified in integration tests.

2. **No authentication on the webhook in this PRD.** Since the server is localhost-only, authentication relies on OS-level process isolation. This is documented as a known limitation. Future work: add a shared secret token in the request `Authorization` header.

3. **Append-only audit log integrity.** The `trg_tal_no_update` and `trg_tal_no_delete` SQLite triggers prevent application-layer modification of `tool_approval_log`. An attacker who can write arbitrary SQL to the database can still bypass these triggers; the triggers are not a cryptographic guarantee. For higher assurance environments, consider periodically hashing the log table content and storing the hash externally.

4. **Argument payload at gate time is the canonical payload.** The `args_json` and `args_sha256` stored in `tool_approval_pending` at gate entry time are the values passed to the MCP server. The dispatch code must re-read `args_json` from the `tool_approval_pending` row (not from the agent's in-memory state) when arguments must be verified post-approval, preventing TOCTOU substitution.

5. **Reviewer identity is OS-level only.** `reviewer` is set from `os.environ.get("USER")` or `os.getlogin()`. This is adequate for single-user workstations and CI environments but is not a strong identity claim in shared-user systems. Teams requiring stronger identity guarantees should gate the `tag` binary itself (e.g., via sudo or a signed CLI wrapper).

6. **Secrets in argument payloads.** Tool arguments may contain API keys or sensitive values. The `args_json` column stores these verbatim. The `tool_approval_log` table should be included in the same access control perimeter as the rest of the TAG SQLite database (`~/.tag/runtime/tag.sqlite3`). Do not export audit logs to untrusted destinations without redacting sensitive argument fields.

7. **Denial-of-service via approval flood.** A misconfigured or malicious agent could generate thousands of pending approvals per second, filling the `tool_approval_pending` table. A rate limit of at most 10 pending approvals per profile per minute is enforced at the gate layer; calls exceeding this limit are auto-denied with a rate-limit error logged.

8. **Race condition on approve+run.** The `UPDATE ... WHERE status='pending'` pattern with rowcount assertion prevents two concurrent `tag mcp approve` invocations from both succeeding on the same approval. If `rowcount == 0`, the second caller receives exit code 2 ("already decided").

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_tool_approval.py`)

| Test | Description |
|------|-------------|
| `test_match_rule_profile_scoped` | Gate matches profile-scoped rule and ignores global rule when profile matches |
| `test_match_rule_global_fallback` | Gate falls back to global rule when no profile-scoped rule matches |
| `test_match_rule_no_match` | Gate returns `None` for ungated tool; `check()` returns without creating any DB row |
| `test_create_pending_row` | `check()` creates a `tool_approval_pending` row with correct `args_sha256` |
| `test_args_sha256_canonical` | SHA-256 of `{"b":1,"a":2}` equals SHA-256 of `{"a":2,"b":1}` |
| `test_approved_resumes` | `check()` returns normally after DB row status updated to `approved` |
| `test_denied_raises` | `check()` raises `ApprovalDeniedError` after DB row status set to `denied` |
| `test_timeout_auto_deny` | Auto-deny fires within 250 ms of deadline when `auto_deny_on_timeout=True` |
| `test_timeout_blocks` | Without auto-deny, `ApprovalTimeoutError` is raised at deadline |
| `test_append_log_on_approve` | Approving a pending record appends one row to `tool_approval_log` |
| `test_append_log_on_deny` | Denying appends one row with `decision='denied'` |
| `test_log_no_update_trigger` | Attempting `UPDATE tool_approval_log` raises `sqlite3.OperationalError` |
| `test_log_no_delete_trigger` | Attempting `DELETE FROM tool_approval_log` raises `sqlite3.OperationalError` |
| `test_duplicate_rule_rejected` | Adding a duplicate (tool, profile) rule exits with code 1, no second DB row created |
| `test_zero_overhead_ungated` | `check()` on ungated tool executes in < 1 ms (no DB writes) |
| `test_event_fired_on_pending` | Creating a pending approval fires a `tool_approval.pending` event in the `events` table |

### 12.2 Integration Tests

| Test | Description |
|------|-------------|
| `test_end_to_end_cli_approve` | Full flow: add rule, start gated run, `tag mcp approve <id>`, verify run completes and log row exists |
| `test_end_to_end_cli_deny` | Full flow: add rule, start gated run, `tag mcp approve <id> --deny`, verify run exits code 5 and denial logged |
| `test_webhook_approve` | Start approval server on random port, POST to `/approvals/<id>/approve`, verify agent resumes |
| `test_webhook_deny` | POST to `/approvals/<id>/deny`, verify agent exit 5 |
| `test_list_pending_json` | `tag mcp approvals list --pending --json` returns valid JSON within 200 ms for 1,000 pending rows |
| `test_export_ndjson` | `tag mcp approvals export --format ndjson` produces valid NDJSON with all required fields |
| `test_export_filtered_by_profile` | `--profile coder` filter returns only rows for coder profile |
| `test_rule_survives_restart` | Add rule, kill process, relaunch, assert rule present via `tag mcp approve-required list` |
| `test_args_dispatched_unchanged` | Verify the args JSON dispatched to MCP server equals the `args_json` in the log row, byte-for-byte |

### 12.3 Performance Tests

| Test | Description | Target |
|------|-------------|--------|
| `bench_gate_overhead_ungated` | 1,000 ungated tool calls through `check()` | < 1 ms per call |
| `bench_list_pending_1k` | `tag mcp approvals list --pending` with 1,000 pending rows | < 200 ms |
| `bench_export_10k` | `tag mcp approvals export` streaming 10,000 log rows | < 2 s |
| `bench_approve_latency` | Time from `tag mcp approve <id>` to agent resume | < 500 ms |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification Method |
|----|-----------|---------------------|
| AC-01 | `tag mcp approve-required add --tool bash --profile coder` creates a row in `tool_approval_rules` with `profile='coder'` and `tool='bash'`. | SQLite assertion in integration test |
| AC-02 | `tag mcp approve-required add --tool github:push --always` creates a row with `mcp_server='github'`, `tool='push'`, `profile=NULL`. | SQLite assertion |
| AC-03 | A `tag run` on the `coder` profile that calls `bash` with a gated rule does NOT dispatch the bash call until `status='approved'` in `tool_approval_pending`. | Integration test: assert no MCP call before approval |
| AC-04 | `tag mcp approve <id>` transitions `tool_approval_pending.status` to `'approved'` and the agent resumes within 500 ms. | Timing assertion in integration test |
| AC-05 | `tag mcp approve <id> --deny` transitions status to `'denied'`, the run exits with code 5, and `tool_approval_log` has a row with `decision='denied'`. | Integration test |
| AC-06 | With `--timeout 5 --auto-deny-on-timeout`, a run with no reviewer fires auto-deny within 5.5 seconds and exits code 5. | Integration test with mocked time |
| AC-07 | `tool_approval_log` contains exactly one row per approval decision (not zero, not two). | Assert `COUNT(*) = 1` after each decision in integration tests |
| AC-08 | `UPDATE tool_approval_log SET decision='approved' WHERE id='x'` raises `sqlite3.OperationalError` containing "append-only". | Unit test |
| AC-09 | `DELETE FROM tool_approval_log WHERE id='x'` raises `sqlite3.OperationalError` containing "append-only". | Unit test |
| AC-10 | `tag mcp approvals list --pending --json` returns valid JSON array within 200 ms for 1,000 pending rows. | Performance test |
| AC-11 | `tag mcp approvals export --format ndjson` produces one valid JSON object per line with all fields: `log_id`, `approval_id`, `rule_id`, `tool`, `mcp_server`, `profile`, `run_id`, `args_json`, `args_sha256`, `decision`, `reviewer`, `reviewer_source`, `rationale`, `created_at`, `decided_at`. | Schema validation in integration test |
| AC-12 | A `POST /approvals/<id>/approve` to the webhook server with a `X-Approver: alice` header records `reviewer='alice'` and `reviewer_source='webhook'` in the log. | Integration test |
| AC-13 | The `args_sha256` in `tool_approval_log` matches `hashlib.sha256(json.dumps(args, sort_keys=True, separators=(",",":")).encode()).hexdigest()`. | Unit test with known fixture |
| AC-14 | The arguments received by the MCP server are byte-for-byte identical to `tool_approval_pending.args_json` for the corresponding approval. | Integration test with request capture |
| AC-15 | An ungated tool call (no matching rule) adds zero rows to `tool_approval_pending` and zero rows to `tool_approval_log`. | Unit test: assert both tables empty after ungated tool call |
| AC-16 | `tag mcp approve-required remove --tool bash --profile coder` removes the rule; subsequent gated runs for `coder/bash` proceed without pausing. | Integration test |
| AC-17 | Webhook server binds only to `127.0.0.1`, not `0.0.0.0`. | Assert `server.server_address[0] == '127.0.0.1'` in unit test |
| AC-18 | Adding a duplicate rule (same tool+profile) exits with code 1 and a descriptive error; no second row is created. | Integration test |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-013: Agent Tracing / Observability | Upstream | Provides `spans` table and span IDs that the approval gate records in `tool_approval_log.trace_span_id` and `tool_approval_pending.trace_span_id`. Gate creates a child span for the approval wait duration. |
| PRD-016: Webhook Event Triggers | Upstream | The `events` table (populated by this PRD on `tool_approval.pending`) is consumed by PRD-016's webhook dispatcher to fire Slack/HTTP callbacks. |
| PRD-027: Eval Framework | Upstream | Eval suites can include cases that verify gated profiles pause on target tools; `eval.py` needs to handle `ApprovalDeniedError` gracefully in test runs. |
| PRD-028: Sandbox Execution | Related | `sandbox.py` and the approval gate may both intercept `bash` calls; the approval gate fires first (pre-dispatch), sandbox restrictions fire at the OS level. Both are orthogonal layers. |
| PRD-034: Secret Scanning / Security | Related | `security.py` may scan argument payloads for secrets. The approval gate surfaces the full payload to the reviewer, making the argument visible before any secret-containing call executes. |
| PRD-040: Notification Hooks | Upstream | `tool_approval.pending` events flow through the PRD-040 notification hook dispatch chain; Slack/email/desktop channels must be configured there. |
| PRD-048: Structured Tool Call Spans | Related | Approval gate creates a `tool_approval.gate` span as a child of the existing tool call span created by PRD-048. |
| `stdlib: http.server` | Runtime | Used for the webhook approval server. No new third-party dependencies required. |
| `stdlib: hashlib, json, threading` | Runtime | Used in `tool_approval.py`. No new third-party dependencies. |

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
| OQ-8 | How should the gate behave when `tag run` is invoked with `--non-interactive` (no TTY) and no timeout is configured? Currently it would block forever. Should non-interactive mode force `--auto-deny-on-timeout` with a default of 300 seconds? | High — CI safety | Eng | Phase 1 |

---

## 16. Complexity and Timeline

### Phase 1 — Core Gate + SQLite (Days 1–6)

| Day | Work |
|-----|------|
| 1–2 | `_migrate_prd_078_tables()` DDL; `tool_approval.py` dataclasses and `ApprovalGate` skeleton; unit tests for rule matching and SHA-256 canonicalization |
| 3–4 | `ApprovalGate.check()` full implementation: pending record creation, `events` table integration, blocking poll with timeout, auto-deny path, `_append_log()` audit trail |
| 5   | `_migrate_prd_078_tables()` integrated into `open_db()`; append-only triggers; trigger enforcement unit tests |
| 6   | `hermes_bridge.py` integration: inject `gate.check()` into tool dispatch path; integration test verifying MCP call does not fire before approval |

### Phase 2 — CLI Commands (Days 7–11)

| Day | Work |
|-----|------|
| 7–8 | `cmd_mcp_approve_required` (add/remove/list); argparse wiring under `tag mcp approve-required`; unit + integration tests |
| 9   | `cmd_mcp_approvals` (list, show, export); `--pending` filter; NDJSON streaming export |
| 10  | `cmd_mcp_approve` (approve/deny with rationale); exit code handling; reviewer identity capture |
| 11  | Manual CLI smoke tests; fix edge cases found during testing |

### Phase 3 — Webhook Server + Notifications (Days 12–16)

| Day | Work |
|-----|------|
| 12–13 | `approval_server.py` implementation; `start_approval_server()` integration into `tag run` startup when `approval.webhook_port` is configured |
| 14  | PRD-040 notification hook integration for `tool_approval.pending` events; desktop notification in `ApprovalGate._fire_notification()` |
| 15  | Webhook integration tests (approve/deny via HTTP, X-Approver header, concurrent request race condition) |
| 16  | Performance benchmarks; ensure zero overhead for ungated tools (NFR-06) |

### Phase 4 — Hardening + Docs (Days 17–20)

| Day | Work |
|-----|------|
| 17  | Edge case coverage: duplicate rule rejection, expired approval CLI error, non-interactive mode behavior (OQ-8) |
| 18  | `tag doctor` check for `tool_approval_rules` table existence and pending approvals count |
| 19  | Security review: webhook binding assertion, rate-limit implementation (SC-7), argument size handling (OQ-3) |
| 20  | Final integration test pass; coverage assertion (> 90%); PR review |

**Total estimate:** 20 engineering days (4 weeks for a single developer, ~2 weeks with two developers working Phase 1/2 in parallel with Phase 3/4).
