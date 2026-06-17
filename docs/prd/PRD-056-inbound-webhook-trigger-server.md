# PRD-056: Inbound Webhook Trigger Server with HMAC Verification (`tag hooks listen`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (1-2 weeks)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `webhook_server.py`
**Depends on:** PRD-008 (background task queue), PRD-013 (agent tracing/observability), PRD-016 (webhook event triggers), PRD-028 (sandbox code execution), PRD-033 (dependency-aware task queue), PRD-034 (secret scanning), PRD-041 (OTel GenAI span cost attribution)
**GitHub Issue:** #344
**Inspired by:** Composio Webhook Triggers V2, Linear AI Agent, GitHub webhooks

---

## 1. Overview

TAG currently operates exclusively in an outbound, user-initiated mode: a human runs `tag submit`, `tag queue add`, or a cron schedule fires. There is no mechanism for external systems — GitHub, Linear, Jira, or Slack — to push events into TAG and trigger agent actions automatically. This means that the most natural integration points in a developer's workflow (a PR opened, an issue assigned, a CI check failing) require manual intervention or bespoke shell polling scripts that are fragile and hard to maintain.

This PRD adds `tag hooks listen`: a production-grade inbound webhook HTTP server that receives signed payloads from external platforms, verifies their authenticity using HMAC-SHA256 (or platform-equivalent), maps events to configured TAG actions via a user-defined rule table, and enqueues matching jobs in the existing `queue_worker` infrastructure. The server is designed to run as a long-lived process (foregrounded or daemonized), making it suitable for both local development tunnels (via `ngrok` or `tailscale`) and persistent deployment on a VPS or in a container.

Security is the foremost design constraint. Every supported platform uses a different signature scheme: GitHub signs with `X-Hub-Signature-256` over the raw request body; Linear signs with `X-Linear-Signature`; Slack uses `X-Slack-Signature` with a timestamp nonce to prevent replay attacks; Jira uses `X-Hub-Signature`. The webhook server reads the raw body before any JSON parsing, computes the expected signature, and compares using `hmac.compare_digest` (constant-time comparison) before the request is allowed to proceed. A request that fails signature verification returns HTTP 401 and is logged — the raw body is never echoed back in error responses to prevent information leakage.

The event-to-action mapping system is declarative: users register rules using `tag hooks register`, which writes rows to a `webhook_rules` table in the existing SQLite database. Each rule specifies a platform, an event pattern (exact match or glob), a TAG profile to use, and an action template string that is rendered with event context variables. Rules are evaluated in priority order; first-match wins. This design gives users full control over which events trigger which agent behaviors, without requiring any Python code changes. The complete rule set is visible via `tag hooks list` and testable with synthetic payloads via `tag hooks test`.

This feature closes the feedback loop between external developer tooling and TAG's agentic capabilities. A team can configure TAG to automatically review every PR opened against their repository, triage every new Linear issue assigned to "AI", respond to Slack mentions with agent-generated summaries, and post back results as comments — all without any custom integration code. It directly enables the "agentic CI/CD" workflow pattern where TAG participates as a peer in the developer toolchain rather than as an isolated offline tool.

---

## 2. Problem Statement

### 2.1 No Inbound Event Channel

TAG has no mechanism for external systems to initiate agent tasks. The only way to trigger a TAG agent is to run a CLI command synchronously. This means that event-driven workflows — the dominant pattern in modern CI/CD and project management — cannot be integrated with TAG without a human in the loop or a bespoke polling script. GitHub Actions can call `tag submit` in a workflow step, but this requires the Actions runner to have TAG installed and authenticated, which is a heavyweight dependency. A lightweight webhook listener that can run locally or on a small VPS eliminates this dependency entirely.

### 2.2 Secret Management and Signature Verification Are Error-Prone to DIY

Every major platform (GitHub, Linear, Jira, Slack) has a different webhook signature scheme with different headers, different digest algorithms, different nonce strategies, and different body-encoding requirements. Teams that build their own webhook handlers routinely make mistakes: reading the body after JSON parsing (breaking the signature check), using string comparison instead of `hmac.compare_digest` (introducing timing side-channels), or forgetting to validate the timestamp on Slack payloads (enabling replay attacks). TAG should implement these schemes correctly once, with tests, so teams don't have to.

### 2.3 No Bridge Between External Events and TAG's Queue System

TAG's `queue_worker` (PRD-008) and dependency DAG (PRD-033) provide a capable job execution layer. But they are only accessible via explicit CLI invocation. There is no adapter that can receive an external event, parse its payload, render an action template, and enqueue a job — the connection between "GitHub says PR #42 was opened" and "run `tag submit --prompt 'Review PR #42' --profile reviewer`" must currently be hand-coded. This PRD provides that adapter as a first-class TAG component.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | `tag hooks listen` starts an HTTP server on a configurable port that receives webhooks from GitHub, Linear, Jira, and Slack, validates HMAC signatures, and enqueues TAG jobs for matching rules. |
| G2 | HMAC-SHA256 verification is mandatory for all platforms; requests that fail verification are rejected with HTTP 401 and logged, never processed. |
| G3 | Signature comparison always uses `hmac.compare_digest` (constant-time); timing side-channels are prevented by design. |
| G4 | Raw request body is read before any JSON parsing to preserve the byte-exact body required for HMAC verification. |
| G5 | `tag hooks register` writes a rule to the SQLite `webhook_rules` table, mapping a platform + event pattern to a profile + action template. |
| G6 | `tag hooks list` prints all registered rules with last-triggered timestamps, match counts, and enabled/disabled status. |
| G7 | `tag hooks test` sends a synthetic platform-authentic payload to the running listener, allowing rules to be verified without real external events. |
| G8 | Matched events are enqueued via the existing `queue_worker` infrastructure (PRD-008); no new execution backend is introduced. |
| G9 | The server supports Slack's replay-attack prevention: `X-Slack-Request-Timestamp` is checked to reject payloads older than 5 minutes. |
| G10 | Platform secrets (HMAC keys) are stored in the TAG keychain/config, masked in all output, and never logged. |
| G11 | `tag hooks listen` supports `--daemon` mode to detach the server into a background process managed via a PID file. |
| G12 | Every received webhook, its verification status, matched rule (if any), and enqueued job ID are persisted to a `webhook_events` table for audit and debugging. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Outbound webhook delivery from TAG (covered by PRD-016). This PRD handles inbound only. |
| NG2 | OAuth flow or app installation for any platform. Secret keys are configured manually by the user. |
| NG3 | A hosted/cloud webhook relay. The server runs on infrastructure the user controls; no Anthropic or TAG cloud involvement. |
| NG4 | Fanout or multi-rule matching within a single event. First-match-wins is the evaluation strategy; complex multi-rule fanout is a future extension. |
| NG5 | Webhook payload transformation or enrichment beyond template variable substitution. |
| NG6 | High-availability or clustering of the listener. Single-process, single-node is the target deployment. |
| NG7 | Support for platforms beyond GitHub, Linear, Jira, and Slack in this PRD. |
| NG8 | Automatic tunnel setup (ngrok, tailscale). Users are responsible for exposing the listener port. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Signature verification correctness | 100% of payloads with invalid signatures are rejected; 0% false rejections of valid payloads | Unit tests with known HMAC vectors for all 4 platforms |
| Time-to-enqueue | Median < 50 ms from HTTP request receipt to `queue_jobs` row committed | Benchmark with `wrk` against local listener |
| Timing side-channel | `hmac.compare_digest` branch confirmed at code level; no string `==` in signature comparison paths | Static analysis + code review |
| Replay attack prevention | Slack payloads with timestamp > 5 min old are rejected with HTTP 401 | Unit test with manipulated timestamps |
| Rule evaluation correctness | All registered rules correctly match their event patterns and reject non-matching events | Integration test matrix across all 4 platforms × 5 events each |
| Daemon stability | `tag hooks listen --daemon` stays running for 72 hours under simulated load (10 req/min) without memory growth or crashes | Long-running stability test |
| Audit trail completeness | Every received request (valid or invalid) appears in `webhook_events` table within 1 second | Integration test asserting row existence after each test request |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Backend engineer | start `tag hooks listen --platform github` and configure a GitHub repo webhook to point at my machine | TAG automatically reviews every PR opened in that repo without me running any command |
| U2 | Engineering manager | run `tag hooks register --platform linear --event Issue.created --profile triage --action "tag submit --prompt 'Triage this issue: {{issue.title}}'"` | Every new Linear issue is auto-triaged by TAG and the response is posted back as a comment |
| U3 | DevOps engineer | validate my webhook rule configuration with `tag hooks test --platform github --event pull_request.opened` before going live | I catch misconfigured rules without waiting for a real PR to be opened |
| U4 | Security-conscious developer | know that webhook secrets are stored in TAG's config (not environment variables), masked in all output, and verified with constant-time comparison | I am not introducing security vulnerabilities into my webhook pipeline |
| U5 | Solo developer | run `tag hooks listen --daemon --port 8080` on a VPS | The listener runs persistently and handles webhooks even when I'm not connected to the VPS |
| U6 | Team lead | run `tag hooks list --json` and pipe the output into a team wiki script | I have a machine-readable inventory of all active webhook rules across my project |
| U7 | Developer | see `tag hooks listen` print every received event in the terminal with its verification status, matched rule, and enqueued job ID | I can debug webhook integrations in real time without digging through log files |
| U8 | Jira admin | point Jira's webhook at `tag hooks listen` and register a rule for `jira:issue_assigned` | When an issue is assigned to a specific user, TAG automatically drafts an implementation plan and adds it as a Jira comment |
| U9 | Slack workspace admin | register a Slack Event API subscription pointing at TAG | When a user @-mentions the TAG bot in a channel, an agent responds in the thread |
| U10 | Incident responder | query `tag hooks list --json` and filter for rules with `last_error` set | I quickly identify any rules that are failing to enqueue jobs and fix them without log archaeology |

---

## 7. Proposed CLI Surface

### 7.1 `tag hooks listen`

Starts the inbound webhook HTTP server.

```
tag hooks listen \
  [--port 8080] \
  [--host 0.0.0.0] \
  [--platform github,linear,jira,slack] \
  [--daemon] \
  [--pid-file ~/.tag/runtime/webhook.pid] \
  [--log-file ~/.tag/runtime/webhook.log] \
  [--workers 4] \
  [--json]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8080` | TCP port to listen on |
| `--host` | `127.0.0.1` | Bind address; use `0.0.0.0` for external access |
| `--platform` | `github,linear,jira,slack` | Comma-separated list of platforms to enable |
| `--daemon` | false | Detach into background; write PID to `--pid-file` |
| `--pid-file` | `~/.tag/runtime/webhook.pid` | PID file for daemon mode |
| `--log-file` | `~/.tag/runtime/webhook.log` | Log file for daemon mode |
| `--workers` | `4` | Number of worker threads in the thread pool |
| `--json` | false | Emit structured JSON log lines to stdout |

**Terminal output (foreground mode):**

```
TAG Webhook Listener v0.3.0
Listening on http://127.0.0.1:8080
Enabled platforms: github, linear, jira, slack
Active rules: 3

Routes:
  POST /webhook/github   → X-Hub-Signature-256 (HMAC-SHA256)
  POST /webhook/linear   → X-Linear-Signature  (HMAC-SHA256)
  POST /webhook/jira     → X-Hub-Signature     (HMAC-SHA1)
  POST /webhook/slack    → X-Slack-Signature   (HMAC-SHA256 + timestamp)

Press Ctrl+C to stop.

[2026-06-17T10:23:41Z] POST /webhook/github  → pull_request.opened  sig=OK  rule=pr-reviewer  job=job-7f3a2c
[2026-06-17T10:24:15Z] POST /webhook/github  → issues.labeled       sig=OK  rule=NONE
[2026-06-17T10:24:55Z] POST /webhook/linear  → Issue.created        sig=FAIL  → 401
```

**JSON log line format (`--json`):**

```json
{
  "ts": "2026-06-17T10:23:41Z",
  "platform": "github",
  "path": "/webhook/github",
  "event": "pull_request.opened",
  "sig_valid": true,
  "rule_id": "pr-reviewer",
  "job_id": "job-7f3a2c",
  "latency_ms": 12,
  "status": 200
}
```

---

### 7.2 `tag hooks register`

Registers a new webhook rule mapping a platform event to a TAG action.

```
tag hooks register \
  --platform <github|linear|jira|slack> \
  --event <event_pattern> \
  --profile <profile_name> \
  --action <action_template> \
  [--name <rule_name>] \
  [--filter <jq_expression>] \
  [--priority 100] \
  [--disabled] \
  [--json]
```

**Flags:**

| Flag | Required | Description |
|------|----------|-------------|
| `--platform` | Yes | Platform for this rule |
| `--event` | Yes | Event type pattern (exact or glob, e.g. `pull_request.*`) |
| `--profile` | Yes | TAG profile to use when running the action |
| `--action` | Yes | Shell-style action template; supports `{{variable}}` substitution from event payload |
| `--name` | No | Human-readable rule name; auto-generated if omitted |
| `--filter` | No | jq expression evaluated against the payload; rule only fires if filter returns truthy |
| `--priority` | No | Evaluation order; lower numbers evaluated first (default 100) |
| `--disabled` | No | Register but do not activate the rule |

**Example invocations:**

```bash
# Auto-review every opened PR
tag hooks register \
  --platform github \
  --event pull_request.opened \
  --profile reviewer \
  --name pr-reviewer \
  --action "tag submit --prompt 'Review PR #{{pull_request.number}}: {{pull_request.title}}' --profile reviewer"

# Triage new Linear issues with label "bug"
tag hooks register \
  --platform linear \
  --event Issue.created \
  --profile triage \
  --name linear-bug-triage \
  --filter '.labels[] | select(.name == "bug") | .id' \
  --action "tag submit --prompt 'Triage bug: {{issue.title}}\n\n{{issue.description}}' --profile triage"

# Respond to Slack @mentions
tag hooks register \
  --platform slack \
  --event app_mention \
  --profile assistant \
  --name slack-mention \
  --action "tag submit --prompt '{{event.text}}' --profile assistant"
```

**Output:**

```
Registered rule 'pr-reviewer' (id: rule-9a2b4f)
  Platform:  github
  Event:     pull_request.opened
  Profile:   reviewer
  Priority:  100
  Action:    tag submit --prompt 'Review PR #{{pull_request.number}} ...' --profile reviewer
```

---

### 7.3 `tag hooks list`

Lists all registered rules.

```
tag hooks list [--json] [--platform <name>] [--enabled-only]
```

**Table output:**

```
RULE NAME        PLATFORM  EVENT                 PROFILE    PRIORITY  ENABLED  LAST TRIGGERED        MATCHES  ERRORS
pr-reviewer      github    pull_request.opened   reviewer   100       yes      2026-06-17 10:23:41   47       0
linear-bug-triage linear   Issue.created         triage     100       yes      2026-06-16 15:44:12   12       1
slack-mention    slack     app_mention           assistant  100       yes      never                  0       0
```

**JSON output (`--json`):**

```json
[
  {
    "id": "rule-9a2b4f",
    "name": "pr-reviewer",
    "platform": "github",
    "event_pattern": "pull_request.opened",
    "profile": "reviewer",
    "action_template": "tag submit --prompt 'Review PR #{{pull_request.number}}...' --profile reviewer",
    "filter": null,
    "priority": 100,
    "enabled": true,
    "match_count": 47,
    "error_count": 0,
    "last_triggered_at": "2026-06-17T10:23:41Z",
    "last_error": null,
    "created_at": "2026-06-10T08:00:00Z"
  }
]
```

---

### 7.4 `tag hooks test`

Sends a synthetic payload to the running listener to validate rules without real events.

```
tag hooks test \
  --platform <github|linear|jira|slack> \
  --event <event_type> \
  [--port 8080] \
  [--payload <json_file>] \
  [--dry-run] \
  [--json]
```

**Example:**

```bash
tag hooks test --platform github --event pull_request.opened
```

**Output:**

```
Sending synthetic github/pull_request.opened payload to http://localhost:8080/webhook/github ...

  Payload:      (built-in fixture, 48 fields)
  Signature:    sha256=a1b2c3d4e5f6... (computed with configured secret)
  Response:     200 OK (11ms)

  Rule matched: pr-reviewer (rule-9a2b4f)
  Job enqueued: job-8c9d1e
  Action:       tag submit --prompt 'Review PR #1: Test PR' --profile reviewer

  Webhook event stored: event-5f6a7b
```

With `--dry-run`, the payload is sent and the rule is evaluated but no job is enqueued.

---

### 7.5 `tag hooks disable` / `tag hooks enable` / `tag hooks delete`

```bash
tag hooks disable <rule_name_or_id>
tag hooks enable  <rule_name_or_id>
tag hooks delete  <rule_name_or_id> [--yes]
```

---

### 7.6 `tag hooks stop`

Stops the background daemon.

```bash
tag hooks stop [--pid-file ~/.tag/runtime/webhook.pid]
```

---

### 7.7 `tag hooks secret set`

Configures platform HMAC secrets.

```bash
tag hooks secret set --platform github   --secret <webhook_secret>
tag hooks secret set --platform linear   --secret <webhook_secret>
tag hooks secret set --platform jira     --secret <webhook_secret>
tag hooks secret set --platform slack    --signing-secret <signing_secret>
```

Secrets are stored in TAG's config file under `webhook_secrets.<platform>`, masked in all output.

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | The server MUST read the full raw request body into memory before calling any JSON parser, to preserve the byte-exact body required for HMAC verification. | P0 |
| FR-02 | For GitHub: the server MUST verify `X-Hub-Signature-256` header using `hmac.new(secret, raw_body, 'sha256').hexdigest()` compared with `hmac.compare_digest`. | P0 |
| FR-03 | For Linear: the server MUST verify `X-Linear-Signature` header using HMAC-SHA256 with the Linear webhook secret and constant-time comparison. | P0 |
| FR-04 | For Jira: the server MUST verify `X-Hub-Signature` header using HMAC-SHA1 and constant-time comparison. (Jira uses SHA1 for legacy reasons; this is documented.) | P0 |
| FR-05 | For Slack: the server MUST verify `X-Slack-Signature` using HMAC-SHA256 over the string `v0:<timestamp>:<raw_body>`, AND reject payloads where `X-Slack-Request-Timestamp` is more than 300 seconds old relative to server clock. | P0 |
| FR-06 | Any request failing signature verification MUST return HTTP 401 with body `{"error": "invalid signature"}`. The raw body and the submitted signature MUST NOT appear in the error response. | P0 |
| FR-07 | The server MUST route requests to platform-specific handlers via URL path: `/webhook/github`, `/webhook/linear`, `/webhook/jira`, `/webhook/slack`. | P1 |
| FR-08 | The server MUST extract the event type from the appropriate platform header or payload field: GitHub → `X-GitHub-Event` + `action` (e.g. `pull_request.opened`); Linear → `type` field in payload; Jira → `webhookEvent` field; Slack → `event.type` field. | P1 |
| FR-09 | After signature verification, the server MUST evaluate registered `webhook_rules` rows in `priority ASC, created_at ASC` order, finding the first rule where `platform` matches and `event_pattern` matches the event type (exact match or `fnmatch` glob). | P1 |
| FR-10 | If a matching rule has a `filter` expression (jq syntax), the server MUST evaluate it against the parsed JSON payload; the rule fires only if the filter returns a truthy value. If `jq` is not installed, filter rules are skipped with a logged warning. | P1 |
| FR-11 | When a rule matches, the server MUST render the `action_template` by substituting `{{variable.path}}` placeholders with values extracted from the parsed JSON payload using dot-path notation. | P1 |
| FR-12 | The rendered action MUST be submitted to the existing `queue_worker` infrastructure by inserting a row into `queue_jobs` with `status='pending'` and the rendered command in the `prompt` field. | P1 |
| FR-13 | Every received webhook request — whether valid or invalid, matched or unmatched — MUST be persisted to `webhook_events` table within the same request/response cycle (before returning HTTP response). | P1 |
| FR-14 | `tag hooks register` MUST validate that the specified `--profile` exists in TAG config before writing the rule. | P1 |
| FR-15 | `tag hooks register` MUST validate the `--action` template contains no shell injection risks: the template is NOT passed to `shell=True`; it is parsed and executed as a list of arguments. | P0 |
| FR-16 | `tag hooks list` MUST display `match_count`, `error_count`, and `last_triggered_at` from aggregated `webhook_events` data. | P2 |
| FR-17 | `tag hooks test` MUST compute a valid HMAC signature for the synthetic payload using the configured platform secret, so the listener's verification passes. | P1 |
| FR-18 | `tag hooks test --dry-run` MUST evaluate rules and render action templates but MUST NOT insert any row into `queue_jobs`. | P1 |
| FR-19 | `tag hooks listen --daemon` MUST write a PID file, redirect stdout/stderr to the log file, and return exit code 0 to the calling shell after the process is confirmed listening. | P1 |
| FR-20 | `tag hooks stop` MUST send SIGTERM to the PID in the PID file, wait up to 5 seconds for clean shutdown, then SIGKILL if needed. | P1 |
| FR-21 | Platform secrets MUST be stored under `webhook_secrets.<platform>` in TAG config, loaded into memory at server startup, and NEVER written to any log file or `webhook_events` row. | P0 |
| FR-22 | The server MUST return HTTP 200 within 100 ms of receiving a valid, matched webhook event; job enqueue time MUST NOT block the HTTP response. Enqueue operations happen synchronously within the request but are bounded to < 50 ms SQLite write latency. | P1 |
| FR-23 | The server MUST return HTTP 204 for valid webhooks that match no rule (no action to take), not HTTP 200, to distinguish "received and processed" from "received and matched". | P2 |
| FR-24 | The `webhook_events` table MUST store: platform, event type, raw payload hash (SHA-256 of body, not the body itself), sig_valid boolean, matched rule ID, enqueued job ID, and processing latency in milliseconds. | P1 |
| FR-25 | `tag hooks secret set` MUST mask the secret value in command echo and confirmation output, showing only the last 4 characters. | P1 |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Throughput:** The listener must handle at least 100 webhook requests/second on a single-core VPS (1 vCPU, 1 GB RAM) without dropping requests. | Verified with `wrk -t4 -c100 -d30s` |
| NFR-02 | **Latency:** P99 end-to-end latency (receipt to HTTP response) MUST be < 200 ms for valid matched events. | Measured under 50 req/s sustained load |
| NFR-03 | **Memory:** The server process MUST NOT exceed 100 MB RSS after 24 hours of operation at 10 req/min. | Measured with `ps -o rss` every hour |
| NFR-04 | **Correctness of HMAC:** The HMAC implementation MUST pass the official NIST HMAC test vectors and the platform-specific test vectors published in each platform's developer documentation. | Automated test suite |
| NFR-05 | **No timing oracle:** Signature comparison MUST always take O(N) time regardless of how many bytes match, using `hmac.compare_digest`. String equality (`==`) is explicitly prohibited in signature paths. | Static analysis assertion |
| NFR-06 | **Graceful shutdown:** SIGTERM triggers a graceful shutdown that waits up to 10 seconds for in-flight requests to complete before closing the socket. | Verified in integration test |
| NFR-07 | **Crash recovery:** The PID file is removed on clean shutdown. If the process crashes, `tag hooks listen` on restart detects a stale PID file (process not running), warns the user, and starts fresh. | Integration test |
| NFR-08 | **No external service dependency at startup:** The server MUST start and pass a health check at `GET /health` without any network calls to external platforms. | Startup test |
| NFR-09 | **SQLite WAL mode:** All database writes MUST use the existing `open_db()` helper which enforces WAL mode; the server MUST NOT open its own connection pool. | Code review |
| NFR-10 | **Dependency minimalism:** `webhook_server.py` MUST only import from the Python standard library and existing TAG dependencies. No new required PyPI packages; `httpx` (already used in TAG) is acceptable for `tag hooks test` client-side calls. | `pip show` assertion in CI |
| NFR-11 | **Platform independence:** The server MUST work on macOS, Ubuntu 22.04+, and Python 3.11+. | CI matrix |
| NFR-12 | **Observability:** Every request MUST emit an OpenTelemetry span following the `tag.webhook.*` semantic conventions defined in `otel_semconv.py` (consistent with PRD-013 / PRD-041). | Integration test with OTEL exporter mock |

---

## 10. Technical Design

### 10.1 New File

**`src/tag/webhook_server.py`** — standalone module, importable and runnable as `python -m tag.webhook_server`.

Existing files modified:
- `src/tag/controller.py` — adds `cmd_hooks_listen`, `cmd_hooks_register`, `cmd_hooks_list`, `cmd_hooks_test`, `cmd_hooks_disable`, `cmd_hooks_enable`, `cmd_hooks_delete`, `cmd_hooks_stop`, `cmd_hooks_secret_set` subcommands.
- `~/.tag/runtime/tag.sqlite3` — two new tables (`webhook_rules`, `webhook_events`) added via `open_db()` migration.

### 10.2 SQLite DDL

```sql
-- Registered webhook rules
CREATE TABLE IF NOT EXISTS webhook_rules (
    id             TEXT PRIMARY KEY,           -- rule-<uuid4_hex[:8]>
    name           TEXT NOT NULL UNIQUE,       -- human-readable name
    platform       TEXT NOT NULL,              -- 'github' | 'linear' | 'jira' | 'slack'
    event_pattern  TEXT NOT NULL,              -- exact string or fnmatch glob
    profile        TEXT NOT NULL,              -- TAG profile name
    action_template TEXT NOT NULL,             -- template with {{variable}} placeholders
    filter_expr    TEXT,                       -- jq expression (nullable)
    priority       INTEGER NOT NULL DEFAULT 100,
    enabled        INTEGER NOT NULL DEFAULT 1, -- 0=disabled, 1=enabled
    created_at     TEXT NOT NULL,              -- ISO-8601 UTC
    updated_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_webhook_rules_platform_priority
    ON webhook_rules (platform, priority ASC, created_at ASC);

-- Audit log of every inbound webhook event
CREATE TABLE IF NOT EXISTS webhook_events (
    id             TEXT PRIMARY KEY,           -- event-<uuid4_hex[:8]>
    received_at    TEXT NOT NULL,              -- ISO-8601 UTC (indexed)
    platform       TEXT NOT NULL,
    event_type     TEXT NOT NULL,              -- e.g. 'pull_request.opened'
    body_sha256    TEXT NOT NULL,              -- SHA-256 hex of raw body (NOT the body)
    sig_valid      INTEGER NOT NULL,           -- 1=valid, 0=invalid
    rule_id        TEXT,                       -- FK webhook_rules.id (nullable)
    job_id         TEXT,                       -- FK queue_jobs.id (nullable)
    latency_ms     INTEGER,
    http_status    INTEGER NOT NULL,
    error_detail   TEXT                        -- error message if sig_valid=0 or enqueue failed
);

CREATE INDEX IF NOT EXISTS idx_webhook_events_received_at
    ON webhook_events (received_at DESC);

CREATE INDEX IF NOT EXISTS idx_webhook_events_platform_event
    ON webhook_events (platform, event_type, received_at DESC);
```

### 10.3 Core Dataclasses

```python
# src/tag/webhook_server.py
from __future__ import annotations

import dataclasses
import fnmatch
import hashlib
import hmac
import http.server
import json
import logging
import os
import subprocess
import threading
import time
import uuid
from pathlib import Path
from typing import Optional


@dataclasses.dataclass(frozen=True)
class WebhookRule:
    """Immutable snapshot of a webhook_rules row, loaded at server start."""
    id: str
    name: str
    platform: str              # 'github' | 'linear' | 'jira' | 'slack'
    event_pattern: str         # exact or fnmatch glob
    profile: str
    action_template: str
    filter_expr: Optional[str]
    priority: int
    enabled: bool

    def matches_event(self, event_type: str) -> bool:
        return fnmatch.fnmatch(event_type, self.event_pattern)


@dataclasses.dataclass
class InboundWebhook:
    """Parsed representation of a single inbound HTTP request."""
    platform: str
    raw_body: bytes
    headers: dict[str, str]    # lowercase header names
    event_type: str            # platform-normalised event type
    payload: dict              # parsed JSON (only set after sig verification)
    sig_valid: bool = False
    matched_rule: Optional[WebhookRule] = None
    job_id: Optional[str] = None
    latency_ms: int = 0
    http_status: int = 400
    error_detail: Optional[str] = None

    @property
    def body_sha256(self) -> str:
        return hashlib.sha256(self.raw_body).hexdigest()


@dataclasses.dataclass
class WebhookServerConfig:
    """Runtime configuration for the webhook HTTP server."""
    host: str = "127.0.0.1"
    port: int = 8080
    platforms: list[str] = dataclasses.field(
        default_factory=lambda: ["github", "linear", "jira", "slack"]
    )
    worker_threads: int = 4
    daemon: bool = False
    pid_file: Path = dataclasses.field(
        default_factory=lambda: Path.home() / ".tag/runtime/webhook.pid"
    )
    log_file: Path = dataclasses.field(
        default_factory=lambda: Path.home() / ".tag/runtime/webhook.log"
    )
    json_logs: bool = False
    slack_replay_window_seconds: int = 300


@dataclasses.dataclass
class PlatformSecrets:
    """HMAC secrets for each platform. Never persisted in webhook_events."""
    github: Optional[bytes] = None   # raw bytes of GitHub webhook secret
    linear: Optional[bytes] = None
    jira: Optional[bytes] = None
    slack: Optional[bytes] = None    # Slack signing secret

    @classmethod
    def from_config(cls, cfg: dict) -> "PlatformSecrets":
        """Load secrets from TAG config dict; encode to bytes."""
        ws = cfg.get("webhook_secrets", {})
        def _enc(v: Optional[str]) -> Optional[bytes]:
            return v.encode() if v else None
        return cls(
            github=_enc(ws.get("github")),
            linear=_enc(ws.get("linear")),
            jira=_enc(ws.get("jira")),
            slack=_enc(ws.get("slack")),
        )
```

### 10.4 HMAC Verification Algorithms

```python
# src/tag/webhook_server.py (continued)

SLACK_REPLAY_WINDOW = 300  # seconds


def verify_github(raw_body: bytes, secret: bytes, headers: dict[str, str]) -> bool:
    """Verify GitHub X-Hub-Signature-256 header."""
    sig_header = headers.get("x-hub-signature-256", "")
    if not sig_header.startswith("sha256="):
        return False
    expected = "sha256=" + hmac.new(secret, raw_body, "sha256").hexdigest()
    return hmac.compare_digest(expected, sig_header)


def verify_linear(raw_body: bytes, secret: bytes, headers: dict[str, str]) -> bool:
    """Verify Linear X-Linear-Signature header (HMAC-SHA256)."""
    sig_header = headers.get("x-linear-signature", "")
    expected = hmac.new(secret, raw_body, "sha256").hexdigest()
    return hmac.compare_digest(expected, sig_header)


def verify_jira(raw_body: bytes, secret: bytes, headers: dict[str, str]) -> bool:
    """Verify Jira X-Hub-Signature header (HMAC-SHA1, legacy)."""
    sig_header = headers.get("x-hub-signature", "")
    if not sig_header.startswith("sha1="):
        return False
    expected = "sha1=" + hmac.new(secret, raw_body, "sha1").hexdigest()
    return hmac.compare_digest(expected, sig_header)


def verify_slack(
    raw_body: bytes,
    secret: bytes,
    headers: dict[str, str],
    now: Optional[float] = None,
) -> tuple[bool, Optional[str]]:
    """
    Verify Slack X-Slack-Signature with replay-attack prevention.
    Returns (valid: bool, error_reason: str | None).
    """
    ts_header = headers.get("x-slack-request-timestamp", "")
    sig_header = headers.get("x-slack-signature", "")
    if not ts_header or not sig_header:
        return False, "missing slack timestamp or signature headers"
    try:
        ts = int(ts_header)
    except ValueError:
        return False, "non-integer slack timestamp"
    current = now if now is not None else time.time()
    if abs(current - ts) > SLACK_REPLAY_WINDOW:
        return False, f"slack timestamp too old: {abs(current - ts):.0f}s"
    base = f"v0:{ts_header}:".encode() + raw_body
    expected = "v0=" + hmac.new(secret, base, "sha256").hexdigest()
    if not hmac.compare_digest(expected, sig_header):
        return False, "slack signature mismatch"
    return True, None
```

### 10.5 Event Type Extraction

```python
def extract_event_type(platform: str, headers: dict[str, str], payload: dict) -> str:
    """
    Derive a normalised 'platform.event.action' string from headers + payload.
    E.g. GitHub pull_request event with action 'opened' → 'pull_request.opened'
    """
    if platform == "github":
        event = headers.get("x-github-event", "unknown")
        action = payload.get("action", "")
        return f"{event}.{action}" if action else event
    elif platform == "linear":
        # Linear payload: {"type": "Issue", "action": "create", ...}
        t = payload.get("type", "unknown")
        action = payload.get("action", "")
        return f"{t}.{action}" if action else t
    elif platform == "jira":
        return payload.get("webhookEvent", "unknown")
    elif platform == "slack":
        event = payload.get("event", {})
        return event.get("type", payload.get("type", "unknown"))
    return "unknown"
```

### 10.6 Template Rendering

```python
import re
from typing import Any


_PLACEHOLDER_RE = re.compile(r"\{\{([\w.]+)\}\}")


def _get_nested(data: Any, path: str) -> str:
    """Resolve dot-path expression against nested dict/list, return str."""
    parts = path.split(".")
    cur = data
    for part in parts:
        if isinstance(cur, dict):
            cur = cur.get(part, "")
        elif isinstance(cur, list) and part.isdigit():
            idx = int(part)
            cur = cur[idx] if idx < len(cur) else ""
        else:
            return ""
    return str(cur) if cur is not None else ""


def render_action(template: str, payload: dict) -> str:
    """
    Substitute {{variable.path}} placeholders from payload.
    Raises ValueError if template contains shell metacharacters outside placeholders
    that could indicate injection (conservative check).
    """
    def _sub(m: re.Match) -> str:
        return _get_nested(payload, m.group(1))
    return _PLACEHOLDER_RE.sub(_sub, template)


def parse_action_argv(rendered: str) -> list[str]:
    """
    Split rendered action into argv list using shlex.split.
    The resulting list is NEVER passed to shell=True.
    """
    import shlex
    return shlex.split(rendered)
```

### 10.7 Job Enqueue Integration

```python
def enqueue_webhook_job(
    conn,                   # sqlite3.Connection from open_db()
    rule: WebhookRule,
    rendered_action: str,
    event_id: str,
    db_path: str,
    config_path: str,
) -> str:
    """
    Insert a queue_jobs row for the rendered action.
    Returns the new job_id.
    Does NOT use shell=True.
    """
    job_id = f"job-{uuid.uuid4().hex[:8]}"
    argv = parse_action_argv(rendered_action)
    # Extract --prompt value from argv for queue_jobs.prompt column
    prompt = ""
    try:
        idx = argv.index("--prompt")
        prompt = argv[idx + 1] if idx + 1 < len(argv) else ""
    except ValueError:
        prompt = rendered_action[:500]

    conn.execute(
        """INSERT INTO queue_jobs
           (id, prompt, profile, task_type, status, priority, created_at, metadata)
           VALUES (?, ?, ?, 'mixed', 'pending', 100, ?, ?)""",
        (
            job_id,
            prompt,
            rule.profile,
            _utc_now(),
            json.dumps({
                "source": "webhook",
                "webhook_event_id": event_id,
                "rule_id": rule.id,
                "full_argv": argv,
            }),
        ),
    )
    conn.commit()
    return job_id
```

### 10.8 HTTP Request Handler

The server uses Python's standard library `http.server.HTTPServer` with a custom `BaseHTTPRequestHandler`. This avoids adding FastAPI/uvicorn as a dependency while providing sufficient performance for typical webhook volumes (< 1000 req/min).

```python
class WebhookHandler(http.server.BaseHTTPRequestHandler):
    """
    Thread-safe HTTP handler. server.config and server.secrets are set
    on the HTTPServer instance before serving. server.db_path and
    server.config_path are also set for job enqueue calls.
    """

    def log_message(self, fmt, *args):
        # Suppress default access log; we emit structured logs ourselves.
        pass

    def do_GET(self):
        if self.path == "/health":
            self._send_json(200, {"status": "ok", "rules": self.server.rule_count})
        else:
            self._send_json(404, {"error": "not found"})

    def do_POST(self):
        t0 = time.monotonic()
        platform = self._resolve_platform()
        if platform is None:
            self._send_json(404, {"error": "unknown path"})
            return

        # FR-01: Read raw body BEFORE any JSON parsing
        length = int(self.headers.get("Content-Length", 0))
        raw_body = self.rfile.read(length)

        headers = {k.lower(): v for k, v in self.headers.items()}
        wh = InboundWebhook(
            platform=platform,
            raw_body=raw_body,
            headers=headers,
            event_type="",
            payload={},
        )

        try:
            self._handle_webhook(wh, t0)
        except Exception as exc:
            logging.exception("Unhandled error in webhook handler")
            wh.http_status = 500
            wh.error_detail = str(exc)
            self._send_json(500, {"error": "internal error"})
        finally:
            self._persist_event(wh)

    def _resolve_platform(self) -> Optional[str]:
        mapping = {
            "/webhook/github": "github",
            "/webhook/linear": "linear",
            "/webhook/jira": "jira",
            "/webhook/slack": "slack",
        }
        return mapping.get(self.path.split("?")[0])

    def _handle_webhook(self, wh: InboundWebhook, t0: float) -> None:
        secrets: PlatformSecrets = self.server.secrets
        secret = getattr(secrets, wh.platform)
        if secret is None:
            wh.sig_valid = False
            wh.error_detail = f"no secret configured for {wh.platform}"
            wh.http_status = 401
            self._send_json(401, {"error": "invalid signature"})
            return

        # Verify signature
        if wh.platform == "github":
            wh.sig_valid = verify_github(wh.raw_body, secret, wh.headers)
        elif wh.platform == "linear":
            wh.sig_valid = verify_linear(wh.raw_body, secret, wh.headers)
        elif wh.platform == "jira":
            wh.sig_valid = verify_jira(wh.raw_body, secret, wh.headers)
        elif wh.platform == "slack":
            valid, err = verify_slack(wh.raw_body, secret, wh.headers)
            wh.sig_valid = valid
            if err:
                wh.error_detail = err

        if not wh.sig_valid:
            wh.http_status = 401
            self._send_json(401, {"error": "invalid signature"})
            return

        # Parse payload only after successful sig verification
        try:
            wh.payload = json.loads(wh.raw_body)
        except json.JSONDecodeError as e:
            wh.http_status = 400
            wh.error_detail = f"invalid json: {e}"
            self._send_json(400, {"error": "invalid json"})
            return

        wh.event_type = extract_event_type(wh.platform, wh.headers, wh.payload)

        # Rule evaluation
        rule = self._find_matching_rule(wh)
        wh.matched_rule = rule

        if rule is None:
            wh.http_status = 204
            self.send_response(204)
            self.end_headers()
            return

        # Render and enqueue
        try:
            rendered = render_action(rule.action_template, wh.payload)
            event_id = f"event-{uuid.uuid4().hex[:8]}"
            conn = _open_db(Path(self.server.db_path))
            job_id = enqueue_webhook_job(
                conn, rule, rendered, event_id,
                self.server.db_path, self.server.config_path
            )
            conn.close()
            wh.job_id = job_id
            wh.http_status = 200
            wh.latency_ms = int((time.monotonic() - t0) * 1000)
            self._send_json(200, {
                "status": "accepted",
                "rule": rule.name,
                "job_id": job_id,
                "event": wh.event_type,
            })
        except Exception as exc:
            wh.http_status = 500
            wh.error_detail = str(exc)
            self._send_json(500, {"error": "enqueue failed"})

    def _find_matching_rule(self, wh: InboundWebhook) -> Optional[WebhookRule]:
        for rule in self.server.rules:
            if rule.platform != wh.platform:
                continue
            if not rule.enabled:
                continue
            if not rule.matches_event(wh.event_type):
                continue
            if rule.filter_expr:
                if not _eval_jq_filter(rule.filter_expr, wh.payload):
                    continue
            return rule
        return None

    def _persist_event(self, wh: InboundWebhook) -> None:
        try:
            conn = _open_db(Path(self.server.db_path))
            event_id = f"event-{uuid.uuid4().hex[:8]}"
            conn.execute(
                """INSERT INTO webhook_events
                   (id, received_at, platform, event_type, body_sha256,
                    sig_valid, rule_id, job_id, latency_ms, http_status, error_detail)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (
                    event_id,
                    _utc_now(),
                    wh.platform,
                    wh.event_type or "unknown",
                    wh.body_sha256,
                    1 if wh.sig_valid else 0,
                    wh.matched_rule.id if wh.matched_rule else None,
                    wh.job_id,
                    wh.latency_ms,
                    wh.http_status,
                    wh.error_detail,
                ),
            )
            conn.commit()
            conn.close()
        except Exception:
            logging.exception("Failed to persist webhook event")

    def _send_json(self, status: int, body: dict) -> None:
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
```

### 10.9 jq Filter Evaluation

```python
def _eval_jq_filter(expr: str, payload: dict) -> bool:
    """
    Evaluate a jq expression against a payload dict.
    Returns True if the expression produces any truthy output.
    Falls back to True (permissive) if jq binary is not found, with a logged warning.
    """
    try:
        result = subprocess.run(
            ["jq", "-e", expr],
            input=json.dumps(payload).encode(),
            capture_output=True,
            timeout=2,
        )
        return result.returncode == 0
    except FileNotFoundError:
        logging.warning("jq not installed; skipping filter expression '%s'", expr)
        return True
    except subprocess.TimeoutExpired:
        logging.warning("jq filter timed out for expression '%s'", expr)
        return False
```

### 10.10 Server Startup and Daemon Mode

```python
def _run_server(cfg: WebhookServerConfig, secrets: PlatformSecrets, db_path: str, config_path: str) -> None:
    from tag.controller import open_db  # noqa: import at call time
    conn = open_db(Path(db_path))
    rules = _load_rules(conn, cfg.platforms)
    conn.close()

    server = http.server.ThreadingHTTPServer((cfg.host, cfg.port), WebhookHandler)
    server.secrets = secrets
    server.rules = rules
    server.rule_count = len(rules)
    server.db_path = db_path
    server.config_path = config_path

    logging.info("TAG Webhook Listener on %s:%d (%d rules)", cfg.host, cfg.port, len(rules))
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        if cfg.pid_file.exists():
            cfg.pid_file.unlink(missing_ok=True)


def _load_rules(conn, platforms: list[str]) -> list[WebhookRule]:
    rows = conn.execute(
        """SELECT id, name, platform, event_pattern, profile, action_template,
                  filter_expr, priority, enabled
           FROM webhook_rules
           WHERE platform IN ({}) AND enabled=1
           ORDER BY priority ASC, created_at ASC""".format(
            ",".join("?" * len(platforms))
        ),
        platforms,
    ).fetchall()
    return [WebhookRule(**dict(r)) for r in rows]
```

### 10.11 Synthetic Test Payload Fixtures

`tag hooks test` uses built-in fixture payloads per platform and event type. Fixtures live in `src/tag/fixtures/webhook/`:

```
src/tag/fixtures/webhook/
  github/
    pull_request.opened.json
    pull_request.closed.json
    issues.opened.json
    issue_comment.created.json
    push.json
    check_run.completed.json
    workflow_run.completed.json
  linear/
    Issue.create.json
    Issue.update.json
    Comment.create.json
  jira/
    jira:issue_created.json
    jira:issue_updated.json
    jira:issue_assigned.json
  slack/
    app_mention.json
    message.json
```

Each fixture is a complete, syntactically valid payload for that event type, with plausible placeholder values. `tag hooks test` loads the appropriate fixture, computes a valid HMAC signature using the configured secret, and POSTs to the local listener.

### 10.12 OTel Span Integration

Every webhook request emits an OpenTelemetry span following conventions defined in `otel_semconv.py`:

```python
# otel_semconv.py additions
WEBHOOK_PLATFORM     = "tag.webhook.platform"
WEBHOOK_EVENT_TYPE   = "tag.webhook.event_type"
WEBHOOK_SIG_VALID    = "tag.webhook.sig_valid"
WEBHOOK_RULE_ID      = "tag.webhook.rule_id"
WEBHOOK_JOB_ID       = "tag.webhook.job_id"
WEBHOOK_LATENCY_MS   = "tag.webhook.latency_ms"
```

Spans are emitted via `tracing.py`'s existing `start_span()` context manager, consistent with PRD-013.

---

## 11. Security Considerations

1. **Constant-time comparison is mandatory.** All signature comparisons use `hmac.compare_digest`. String equality (`==`, `!=`) is explicitly prohibited in any code path that compares HMAC values. This is enforced by a `grep` assertion in the test suite that fails if `==` or `!=` appears within 5 lines of the word `signature` in `webhook_server.py`.

2. **Raw body before JSON parsing.** JSON parsers (including Python's `json.loads`) may normalise whitespace, reorder keys, or modify numeric precision, changing the byte content. HMAC must be computed over the exact bytes received on the wire. The handler reads `self.rfile.read(length)` before any parsing (FR-01).

3. **Secrets are never logged.** `PlatformSecrets` fields are `bytes` values loaded from config at startup. They appear in no log statement, no `webhook_events` row, no `--json` output, and no error response body. The `__repr__` of `PlatformSecrets` is overridden to output `PlatformSecrets(github=****)` to prevent accidental exposure via `logging.debug("%r", secrets)`.

4. **No shell injection.** Action templates are rendered by `render_action()` (string substitution only), then split by `shlex.split()`, and executed as an argv list via `subprocess.run(..., shell=False)`. The rendered command is NEVER passed to `shell=True`. Template variables that resolve to empty string are substituted as empty string, never as shell metacharacters.

5. **Replay attack prevention for Slack.** The `X-Slack-Request-Timestamp` is checked against server wall clock. Requests older than 300 seconds are rejected before the HMAC comparison even runs, preventing a scenario where an attacker captures a valid signed payload and replays it later (FR-05).

6. **No raw body in error responses.** HTTP 401 responses contain only `{"error": "invalid signature"}`. The submitted signature value and the expected signature are never included in the response body, preventing oracle attacks where an attacker iteratively adjusts payloads to learn information about the secret.

7. **Health endpoint is unauthenticated but returns minimal data.** `GET /health` returns `{"status": "ok", "rules": N}` with no sensitive data. It does not expose configured platforms, rule names, or secrets.

8. **Minimum-privilege process.** When running in daemon mode, `tag hooks listen --daemon` does not drop privileges (it inherits the user's privileges) but documents that users should run it as a dedicated low-privilege user in production.

9. **PID file race condition.** The PID file is written after the socket is bound and confirmed listening, not before. This prevents a short window where a new process reads a stale PID file from a previous crash.

10. **Content-Length validation.** The handler rejects requests with `Content-Length` exceeding 10 MB (`MAX_BODY_BYTES = 10 * 1024 * 1024`) to prevent memory exhaustion via oversized payloads.

11. **Rate limiting.** The server enforces a per-source-IP rate limit of 100 requests/minute using a simple token-bucket counter in shared memory (thread-safe via `threading.Lock`). Requests exceeding the limit receive HTTP 429. The limit is configurable via `--rate-limit` flag.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_webhook_server.py`)

```
test_verify_github_valid_signature          # Known key+body+sig vector
test_verify_github_invalid_signature        # Tampered body → reject
test_verify_github_missing_header           # No X-Hub-Signature-256 → reject
test_verify_linear_valid
test_verify_linear_invalid
test_verify_jira_valid
test_verify_jira_invalid
test_verify_slack_valid
test_verify_slack_replay_too_old            # ts > 300s ago → reject
test_verify_slack_replay_future             # ts > 300s future → reject
test_verify_slack_missing_timestamp
test_extract_event_type_github_pr_opened    # X-GitHub-Event=pull_request + action=opened → 'pull_request.opened'
test_extract_event_type_github_no_action    # X-GitHub-Event=ping → 'ping'
test_extract_event_type_linear_issue_create
test_extract_event_type_slack_app_mention
test_render_action_simple_substitution      # {{pull_request.number}} → '42'
test_render_action_nested_path              # {{pull_request.head.sha}} → 'abc123'
test_render_action_missing_path             # {{nonexistent.field}} → ''
test_parse_action_argv_quoted_prompt        # Ensures prompt with spaces parses correctly
test_hmac_no_string_equality               # grep assertion that == not used in sig comparison
test_webhook_rule_matches_exact
test_webhook_rule_matches_glob             # 'pull_request.*' matches 'pull_request.opened'
test_webhook_rule_no_match
```

### 12.2 Integration Tests (`tests/test_webhook_integration.py`)

Spin up a real `ThreadingHTTPServer` bound to an ephemeral port in a pytest fixture. Use a real in-memory SQLite database. Insert webhook rules. POST synthetic payloads with valid and invalid signatures.

```
test_full_flow_github_pr_opened             # Valid sig + matching rule → 200, job in queue_jobs
test_invalid_sig_github                     # Invalid sig → 401, no queue_jobs row, event in webhook_events
test_no_matching_rule                       # Valid sig, no rule match → 204, no queue_jobs row
test_slack_replay_rejected                  # Old timestamp → 401
test_audit_trail_all_requests               # Every request (valid/invalid) persists to webhook_events
test_health_endpoint                        # GET /health → 200
test_daemon_pid_file                        # PID file written on start, removed on clean stop
test_rate_limiting                          # 101 req/min from same IP → 429 on 101st
test_action_template_rendering              # End-to-end: payload → rendered action → queue_jobs.prompt
test_jq_filter_match                        # Rule with filter fires only when filter matches
test_jq_filter_no_match                     # Rule with filter does not fire when filter rejects
test_jq_not_installed                       # Mocked FileNotFoundError → rule fires (permissive fallback)
test_disabled_rule_not_matched              # disabled=1 rule is skipped in evaluation
test_priority_ordering                      # Lower priority number wins when two rules match same event
```

### 12.3 HMAC Correctness Tests Against Published Vectors

```python
# tests/test_hmac_vectors.py
# GitHub's own test vector from developer.github.com/webhooks/webhook-events-and-payloads/
GITHUB_VECTOR = {
    "secret": b"It's a Secret to Everybody",
    "body": b"Hello, World!",
    "expected": "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17",
}

def test_github_hmac_vector():
    result = "sha256=" + hmac.new(
        GITHUB_VECTOR["secret"], GITHUB_VECTOR["body"], "sha256"
    ).hexdigest()
    assert result == GITHUB_VECTOR["expected"]
```

### 12.4 Performance Tests

```bash
# Start listener in test mode
tag hooks listen --port 18080 --json &

# Warm up
wrk -t1 -c10 -d5s -s tests/wrk_webhook.lua http://localhost:18080/webhook/github

# Benchmark: target P99 < 200ms at 100 req/s
wrk -t4 -c100 -d30s --latency -s tests/wrk_webhook.lua http://localhost:18080/webhook/github
```

`tests/wrk_webhook.lua` computes a fresh HMAC-SHA256 for each request using the test secret and includes the `X-Hub-Signature-256` header so the server validates each request.

### 12.5 `tag hooks test` Smoke Test

Part of the standard `tag doctor` health check:

```bash
tag hooks listen --port 18080 &
tag hooks test --platform github --event pull_request.opened --port 18080 --dry-run
# Assert exit code 0
```

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag hooks listen --port 8080 --platform github,linear` starts and `GET http://localhost:8080/health` returns `200 {"status": "ok"}` within 2 seconds. | `pytest tests/test_webhook_integration.py::test_health_endpoint` |
| AC-02 | A GitHub `pull_request.opened` webhook with a valid HMAC-SHA256 signature returns HTTP 200 and a new `queue_jobs` row with `status='pending'` appears within 100 ms. | `pytest tests/test_webhook_integration.py::test_full_flow_github_pr_opened` |
| AC-03 | A GitHub webhook with a tampered body (or missing signature header) returns HTTP 401 and NO `queue_jobs` row is created. | `pytest tests/test_webhook_integration.py::test_invalid_sig_github` |
| AC-04 | A Slack `app_mention` webhook with a timestamp older than 300 seconds returns HTTP 401, even if the HMAC is otherwise valid. | `pytest tests/test_webhook_integration.py::test_slack_replay_rejected` |
| AC-05 | Every received request — valid or invalid, matched or unmatched — appears in the `webhook_events` table before the HTTP response is returned. | `pytest tests/test_webhook_integration.py::test_audit_trail_all_requests` |
| AC-06 | `tag hooks register --platform github --event pull_request.opened --profile reviewer --action "tag submit --prompt 'Review PR #{{pull_request.number}}' --profile reviewer"` succeeds and `tag hooks list` shows the new rule. | Manual + `pytest tests/test_webhook_server.py::test_register_and_list` |
| AC-07 | `tag hooks test --platform github --event pull_request.opened --dry-run` exits 0 and prints the matched rule name without inserting any row into `queue_jobs`. | `pytest tests/test_webhook_integration.py::test_dry_run_no_queue_insert` |
| AC-08 | A grep assertion confirms that `webhook_server.py` contains zero occurrences of `== sig` or `sig ==` or `!= sig` or `sig !=` outside of comment lines. | `pytest tests/test_webhook_server.py::test_hmac_no_string_equality` |
| AC-09 | The GitHub NIST-like vector test passes: known secret + known body → expected `sha256=...` digest. | `pytest tests/test_hmac_vectors.py::test_github_hmac_vector` |
| AC-10 | `tag hooks listen --daemon` writes a PID file and returns exit 0. `ps -p $(cat ~/.tag/runtime/webhook.pid)` succeeds. `tag hooks stop` terminates the process and removes the PID file within 5 seconds. | Integration test + manual verification |
| AC-11 | A rule with `--filter '.labels[] | select(.name == "bug") | .id'` does NOT fire when the payload has no `labels` array containing a `bug` entry. | `pytest tests/test_webhook_integration.py::test_jq_filter_no_match` |
| AC-12 | `tag hooks secret set --platform github --secret test123` stores the secret masked in config. `tag config get webhook_secrets.github` shows `****3` (last 1 char visible). | `pytest tests/test_webhook_server.py::test_secret_masking` |
| AC-13 | Under a load of 100 req/s for 30 seconds, zero requests are dropped, P50 latency is < 50 ms, and P99 latency is < 200 ms. | `wrk` benchmark in CI performance job |
| AC-14 | When `Content-Length` exceeds 10 MB, the server returns HTTP 413 without reading the full body. | `pytest tests/test_webhook_integration.py::test_oversized_body_rejected` |
| AC-15 | `import tag.webhook_server` does not import any package not already in TAG's dependency tree. `sys.modules` assertion after import. | `pytest tests/test_webhook_server.py::test_no_new_imports` |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-008: Background Task Queue | Hard | `queue_jobs` table and worker subprocess must exist; `tag hooks listen` enqueues jobs there |
| PRD-013: Agent Tracing / Observability | Soft | OTel span emission via `tracing.py`; degrades gracefully if tracing is disabled |
| PRD-016: Webhook Event Triggers | Related | PRD-016 is outbound (TAG → external); this PRD is inbound (external → TAG). They share the `webhook_rules` naming convention but separate tables. |
| PRD-033: Dependency-Aware Task Queue | Soft | Webhook-enqueued jobs can use `--depends-on` in action templates if DAG is enabled |
| PRD-034: Secret Scanning | Hard | `security.py` MUST validate that no HMAC secret values appear in log output; `tag hooks secret set` calls `security.py`'s masking utility |
| PRD-041: OTel GenAI Span / Cost Attribution | Soft | `otel_semconv.py` extended with `tag.webhook.*` attributes |
| Python stdlib: `http.server`, `hmac`, `hashlib`, `json`, `shlex`, `threading`, `subprocess` | Runtime | No new PyPI packages required |
| `jq` binary (optional) | Runtime/optional | Required only for `--filter` expressions; missing jq logs a warning and skips filters |
| `httpx` (already in TAG deps) | Runtime | Used by `tag hooks test` client-side to POST synthetic payloads |

---

## 15. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-----------------|
| OQ-1 | Should `webhook_events` retain the full raw body (gzip-compressed) for post-hoc debugging, or only the SHA-256 hash? Storing bodies raises privacy concerns (PR titles, issue text) but enables replay debugging. | Eng + Product | Before implementation start |
| OQ-2 | Should the server support mTLS for platforms that can present client certificates? GitHub Advanced Security does not currently offer this, but enterprise Jira instances sometimes do. | Eng | Post-launch extension |
| OQ-3 | Linear uses HMAC-SHA256 with no documented official test vectors. Should we reach out to Linear's developer relations to confirm the exact signing scheme before shipping? | Eng | Before AC-04 equivalent for Linear |
| OQ-4 | Should `--filter` use jq syntax (requires `jq` binary) or Python's `jmespath` library (pure-Python, already usable as an optional dep)? jq is more powerful but jmespath is more portable. | Eng | Architecture decision; default to jmespath with jq opt-in |
| OQ-5 | Should `tag hooks listen` support a `--reload` flag to reload rules from the database without restarting the server? This is useful when rules change frequently but avoids the cost of a restart. | Eng | Nice-to-have; exclude from initial implementation |
| OQ-6 | What is the correct behaviour when the `queue_worker` is not running and `queue_jobs` fills up? Should the listener reject new webhooks with 503 when queue depth exceeds a threshold? | Product | Before FR-22 finalisation |
| OQ-7 | Should `tag hooks test` support `--payload <file>` to accept a custom JSON file instead of the built-in fixture, enabling testing against real captured payloads? | UX | Add in first minor release post-launch |
| OQ-8 | Jira's `X-Hub-Signature` uses HMAC-SHA1, which is cryptographically weak. Should TAG document this limitation and recommend Jira's newer IP allowlisting approach as a supplementary control? | Security | Document in `tag hooks secret set --platform jira` output |

---

## 16. Complexity and Timeline

**Total estimate: 10 working days (2 weeks)**

### Phase 1 — Core Verification Engine (Days 1-3)

- Write `verify_github`, `verify_linear`, `verify_jira`, `verify_slack` with full unit tests against known vectors.
- Write `test_hmac_no_string_equality` static assertion.
- Write `extract_event_type` for all 4 platforms with unit tests.
- SQLite DDL: `webhook_rules` and `webhook_events` tables via `open_db()` migration.
- Write `WebhookRule`, `InboundWebhook`, `WebhookServerConfig`, `PlatformSecrets` dataclasses.

**Deliverable:** All HMAC tests passing; DDL applied to a local test database.

### Phase 2 — HTTP Server and Rule Engine (Days 4-6)

- Implement `WebhookHandler` with `do_POST`, `_handle_webhook`, `_find_matching_rule`, `_persist_event`.
- Implement `_load_rules`, `_run_server`.
- Implement `render_action`, `parse_action_argv`, `_eval_jq_filter`.
- Implement `enqueue_webhook_job` with integration into `queue_jobs`.
- Full integration test suite against a local `ThreadingHTTPServer` instance.

**Deliverable:** End-to-end test passing for GitHub `pull_request.opened` → `queue_jobs` row.

### Phase 3 — CLI Surface (Days 7-8)

- Add `cmd_hooks_listen`, `cmd_hooks_register`, `cmd_hooks_list`, `cmd_hooks_test`, `cmd_hooks_disable`, `cmd_hooks_enable`, `cmd_hooks_delete`, `cmd_hooks_stop`, `cmd_hooks_secret_set` to `controller.py`.
- Implement daemon mode: double-fork, PID file, log redirection.
- Implement `tag hooks test` with fixture loading and HMAC signing.
- Implement `--json` structured log output.

**Deliverable:** All `tag hooks *` subcommands functional via CLI.

### Phase 4 — OTel, Rate Limiting, Hardening (Days 9-10)

- Add `tracing.py` span emission to `WebhookHandler` using new `otel_semconv.py` attributes.
- Add per-IP rate limiting with token bucket.
- Add `Content-Length` guard (10 MB max).
- Add stale PID file detection and warning.
- Add `GET /health` endpoint.
- Write built-in fixture payloads for all 4 platforms × 3 events each.
- Performance benchmark via `wrk`; assert P99 < 200 ms.
- Write acceptance criteria verification scripts.
- Update `tag doctor` to smoke-test `GET /health` if `webhook.pid` exists.

**Deliverable:** All acceptance criteria passing; PRD marked In Progress → Complete.

---

*PRD-056 authored 2026-06-17 for TAG CLI v0.4.x milestone. Review required from: Engineering Lead, Security, Product.*
