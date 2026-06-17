# PRD-074: MCP OAuth 2.1 with PKCE + Device Authorization Flow (`tag mcp auth`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `mcp_auth.py`
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (secret scanning), PRD-014 (MCP server registry)
**Inspired by:** MCP OAuth 2.1 spec (2025), Composio auth, Arcade AI auth
**GitHub Issue:** #346

---

## 1. Overview

MCP servers increasingly require authenticated access to act on behalf of users — GitHub to open pull requests, Notion to read/write databases, Google Workspace to manage calendar events, Slack to post messages, Stripe to list transactions. The MCP OAuth 2.1 specification (ratified 2025) standardises how MCP clients and servers negotiate these credentials using a five-step discovery chain that culminates in either a PKCE-protected authorisation code flow (for interactive browser sessions) or a device authorisation flow (RFC 8628, for headless CLI environments). TAG currently has no first-class support for this authentication layer, which means users must manually paste tokens into environment variables, rotating them by hand and storing them in plaintext config files.

This PRD specifies `tag mcp auth`: a complete OAuth 2.1 credential lifecycle manager for MCP servers. The system auto-detects whether the host environment has a browser available and selects the appropriate flow — PKCE authorisation code when `DISPLAY` or `BROWSER` is set; device flow when running over SSH or in a headless CI container. Tokens are never stored in plaintext configuration files; they live exclusively in the OS keychain via the `keyring` library (Keychain on macOS, Secret Service on Linux, Credential Manager on Windows). Token refresh happens transparently before expiry. Per-server token isolation ensures that a token issued for `api.notion.com` is never forwarded to `api.github.com`, satisfying the audience-binding requirement introduced in RFC 8707.

The implementation lives in a single new module `src/tag/mcp_auth.py`, wired into the `tag mcp` subcommand family that already exists in `controller.py`. It covers the full OAuth lifecycle: discovery, registration (dynamic client registration per RFC 7591), authorisation, token exchange, secure storage, background refresh, status inspection, revocation, and listing of all connected accounts. The design draws heavily on three production patterns: the MCP spec's Protected Resource Metadata (PRM) discovery chain for server-agnostic endpoint discovery; Composio's entity-scoped brokered credential model where the LLM context window never touches tokens; and Arcade AI's Human-in-the-Loop (HITL) consent gate where destructive scopes require explicit user approval before tool execution unblocks.

The feature supports six first-class server integrations out of the box (GitHub, Notion, Google Workspace, Slack, Stripe, and Linear) with pre-configured scope sets and well-known authorisation server URIs, while remaining fully generic for any RFC 8414-compliant authorisation server that a user might configure manually. `tag mcp auth status` provides a live view of token health across all connected accounts so that expired or revoked tokens surface before an agent run fails mid-task with a cryptic 401.

---

## 2. Problem Statement

### 2.1 Plaintext Token Management is a Security Liability

Today, connecting TAG to an authenticated MCP server means setting `NOTION_TOKEN=secret_xyz` in a profile `.env` file or `~/.tag/config.yaml`. These files live on disk in plaintext. When PRD-026 (Profile Marketplace) ships, a single `tag profile push` could silently exfiltrate every token embedded in profile env files — even with PRD-034's secret scanner active, token formats vary widely and not all match named patterns. Users who follow `.env` hygiene correctly still face manual rotation: when a GitHub PAT expires, the agent fails at runtime with an opaque authentication error, not a prompt to re-authorise.

Beyond storage, scope management is entirely manual today. Users typically provision a PAT with far broader permissions than the agent needs because scoping a fine-grained token requires navigating each provider's settings UI. There is no programmatic record of which scopes TAG actually uses, making principle-of-least-privilege impossible to enforce in practice.

### 2.2 Headless and Interactive Environments Need Different Flows

TAG is used in two fundamentally different execution contexts. Interactive development sessions on a developer workstation have a browser available — the standard OAuth redirect flow works correctly here, and PKCE (RFC 7636) makes it safe for a public CLI client. CI pipelines, SSH sessions into remote machines, and containerised environments have no browser — the redirect URI `http://localhost:PORT/callback` never receives a callback because there is no browser to redirect. Today there is no way to authenticate MCP servers in these headless environments except by pre-provisioning static tokens, which reintroduces the plaintext storage problem.

RFC 8628 (Device Authorization Flow) solves this precisely: the device displays a short code and URL, the user completes authorisation on any device with a browser, and the CLI polls until the token arrives. Without first-class support for this flow, TAG is functionally unusable with authenticated MCP servers in CI/CD and remote development environments — an increasingly common deployment pattern as agentic pipelines move to cloud runners.

### 2.3 Token Lifecycle Management is Entirely Manual

OAuth 2.0 access tokens are short-lived by design. GitHub tokens expire after 8 hours; Google tokens expire after 1 hour; Notion tokens after 8 hours. When a token expires mid-agent-run, the MCP server returns a 401, and the agent either silently fails, wastes tokens attempting retries, or surfaces a confusing error that the user must diagnose and resolve by re-running `export NOTION_TOKEN=$(...)`. Refresh tokens exist precisely to automate this rotation, but TAG has no mechanism to perform background refresh, detect expiry proactively, or re-trigger the authorisation flow when a refresh token itself has expired.

Without token lifecycle management, every authenticated MCP server integration has a latent reliability problem: the longer the agent runs or the more time passes between runs, the more likely it is to hit an expired token mid-task. This is particularly damaging in autonomous loop mode (PRD-021) where an agent may run for hours unattended.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Implement the full MCP OAuth 2.1 five-step discovery chain: 401 response → PRM endpoint → AS metadata (RFC 8414) → dynamic client registration (RFC 7591) → PKCE authorisation code flow or device flow. |
| G2 | Auto-detect headless environments via `DISPLAY`, `SSH_CLIENT`, `SSH_TTY`, and `CI` environment variables; automatically select device flow in headless contexts and PKCE flow in interactive contexts, with `--flow` override. |
| G3 | Store all tokens exclusively in the OS keychain via the `keyring` library; never write tokens, client secrets, or refresh tokens to disk in plaintext. |
| G4 | Implement transparent background token refresh: proactively refresh when `expires_in` is within 5 minutes; re-trigger interactive authorisation when refresh token is expired or revoked. |
| G5 | Enforce audience-binding (RFC 8707): every authorisation request and token exchange request MUST include the `resource` parameter set to the MCP server's canonical URI; tokens MUST NOT be reused across servers. |
| G6 | Provide first-class pre-configured profiles for GitHub, Notion, Google Workspace (Gmail, Drive, Calendar), Slack, Stripe, and Linear — covering well-known AS endpoints, scope sets, and redirect URI requirements. |
| G7 | `tag mcp auth list` provides a health dashboard of all connected accounts: server name, scopes, expiry time, and refresh status. |
| G8 | `tag mcp auth revoke <server>` performs server-side token revocation (RFC 7009) and removes keychain entries. |
| G9 | Implement the HITL consent gate for destructive scopes (write, delete, admin): display the requested scopes and require explicit `y/N` confirmation before initiating the authorisation flow. |
| G10 | Emit OAuth lifecycle events as TAG trace spans (PRD-013): discovery, registration, authorisation, token exchange, refresh, revocation — each with server name, duration, and success/failure. |
| G11 | `tag mcp auth status <server>` performs a live token introspection (RFC 7662) or lightweight API probe and reports token validity, remaining TTL, and scopes. |
| G12 | All operations complete within their documented latency budgets: discovery chain ≤ 2 s; PKCE local callback server starts in ≤ 100 ms; device flow polling respects `interval` from AS response with exponential backoff on `slow_down`. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Implementing a full OAuth 2.1 authorisation server. TAG is always the client, never the server. |
| NG2 | Supporting OAuth 1.0/1.0a. All integrations target OAuth 2.1 with PKCE. Legacy providers (Twitter v1.1) are out of scope. |
| NG3 | Multi-user / multi-tenant credential sharing. Tokens are scoped to the local OS user account; team credential vaults (HashiCorp Vault, AWS Secrets Manager) are a future extension. |
| NG4 | Automatic scope escalation. If a tool call requires a scope not in the current token, TAG surfaces an actionable error message; it does not silently re-authorise with broader scopes. |
| NG5 | Composio SDK integration in this PRD. The entity-scoped brokered model from Composio is architecturally noted as a reference but implementing the Composio SDK connector is a separate PRD. |
| NG6 | GUI / browser extension for token management. All operations are CLI-first; a future `tag desktop` integration (PRD-007) may add a UI layer. |
| NG7 | mTLS or private-key JWT client authentication. This PRD covers `client_secret_basic` and `none` (PKCE public client) authentication methods only. |
| NG8 | Modifying existing `tag run` to automatically re-authenticate on 401. Re-authentication interrupts autonomous mode in non-obvious ways; this is deferred to PRD-021's interactive approval gate. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Time to first authenticated Notion call | < 90 seconds from `tag mcp auth notion` to first successful tool call | Timed integration test |
| Token storage security | Zero plaintext tokens on disk after `tag mcp auth` completes | `grep -r` scan of `~/.tag/` for token patterns post-auth |
| Discovery chain reliability | ≥ 99% of discovery attempts succeed when AS is reachable | 100-run integration test against GitHub and Notion AS |
| Headless flow detection accuracy | Device flow selected in 100% of headless contexts; PKCE flow in 100% of interactive contexts | Unit test matrix over env var combinations |
| Token refresh transparency | Zero 401 errors during a 4-hour agent loop when refresh token is valid | Long-running integration test with mocked AS |
| Revocation completeness | `tag mcp auth revoke` removes both keychain entry and server-side token in 100% of test cases | Integration test with token introspection post-revoke |
| `tag mcp auth list` latency | Dashboard renders in < 200 ms for up to 20 connected accounts | Benchmark with 20 mock keychain entries |
| Audit coverage | 100% of OAuth lifecycle events appear as TAG trace spans | Span assertion in integration tests |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer on a local workstation | run `tag mcp auth notion --scopes read,write` and complete the flow in my browser | my TAG agent can read and update Notion databases without me managing tokens manually |
| U2 | Platform engineer in CI | run `tag mcp auth github --org myorg` in a GitHub Actions runner | the headless device flow displays a code I complete on my phone and the pipeline proceeds authenticated without storing PATs in secrets |
| U3 | Developer | run `tag mcp auth list --json` | I can pipe the output to a monitoring script that alerts me when any token is within 1 hour of expiry |
| U4 | Security-conscious user | run `tag mcp auth revoke notion` | I know the token is invalidated server-side and removed from my keychain after a project ends |
| U5 | Developer | run `tag mcp auth status notion` | I can verify token health before starting a long agent run that depends on Notion access |
| U6 | Developer | receive a clear HITL prompt listing destructive scopes before authorisation | I do not inadvertently grant write access when I only needed read access |
| U7 | Developer | have my Google token automatically refreshed mid-run | a 4-hour research agent loop does not fail after 60 minutes when the Google token expires |
| U8 | Developer | run `tag mcp auth github` without specifying scopes | sensible defaults (repo read, issues read/write) are applied without requiring me to know OAuth scope names |
| U9 | Ops engineer | see OAuth lifecycle spans in `tag trace show` | I can diagnose authentication latency issues and discovery chain failures without adding debug logging |
| U10 | Developer | run `tag mcp auth notion --flow device` even on an interactive terminal | I can test the device flow locally or force it when my browser is unavailable |

---

## 7. Proposed CLI Surface

All auth subcommands live under the `tag mcp auth` namespace.

### 7.1 `tag mcp auth <server>`

Initiate the OAuth flow for a named MCP server.

```
tag mcp auth <server>
  [--scopes <scope1,scope2,...>]
  [--flow {pkce|device}]           # default: auto-detect
  [--org <org>]                    # GitHub: restrict to org
  [--port <n>]                     # PKCE: local callback port (default: 9753)
  [--timeout <seconds>]            # device flow: max wait (default: 300)
  [--profile <name>]               # TAG profile to associate credentials with
  [--force]                        # re-authorise even if valid token exists
  [--json]                         # emit result as JSON
```

**Interactive (PKCE) output:**

```
tag mcp auth notion --scopes read,write

Connecting to Notion MCP server...
  Discovering authorisation server...     ✓  (https://api.notion.com/.well-known/oauth-authorization-server)
  Registering dynamic client...           ✓  (client_id: dyn-a3f9c2)
  Requested scopes: read, write

  ⚠  Destructive scope requested: write
     Grants: create pages, update blocks, delete content
     Allow? [y/N]: y

  Opening browser for authorisation...
  Listening on http://localhost:9753/callback

  ✓  Authorised. Token stored in keychain (service: tag-mcp-notion)
     Expires: 2026-06-17T18:45:00Z (8h from now)
     Refresh token: present
```

**Device flow (headless) output:**

```
tag mcp auth github --org myorg

Connecting to GitHub MCP server...
  Discovering authorisation server...     ✓  (https://github.com)
  Registering dynamic client...           ✓
  Device flow selected (headless environment detected)

  ┌─────────────────────────────────────────────┐
  │  Open:  https://github.com/login/device     │
  │  Code:  ABCD-1234                           │
  └─────────────────────────────────────────────┘

  Waiting for authorisation... (expires in 900s)
  Polling every 5s [■■■■□□□□□□□□□□□□□□□□] 20s elapsed

  ✓  Authorised. Token stored in keychain (service: tag-mcp-github)
     Expires: 2026-06-18T10:45:00Z (8h from now)
     Org restriction: myorg
```

### 7.2 `tag mcp auth list`

List all connected MCP server accounts and their token health.

```
tag mcp auth list [--json] [--profile <name>]
```

**Table output:**

```
Server       Scopes               Expires              Refresh   Status
──────────   ──────────────────   ──────────────────   ───────   ──────────
notion       read, write          2026-06-17 18:45     present   ✓ valid
github       repo, issues         2026-06-18 10:45     present   ✓ valid
slack        chat:write           2026-06-17 09:00     absent    ✗ expired
google/      calendar.events      2026-06-17 11:30     present   ⚠ refresh soon
  calendar   drive.readonly
stripe       (no scopes)          —                    absent    ✗ no token
```

**JSON output (`--json`):**

```json
[
  {
    "server": "notion",
    "scopes": ["read", "write"],
    "expires_at": "2026-06-17T18:45:00Z",
    "has_refresh_token": true,
    "status": "valid",
    "ttl_seconds": 28800
  }
]
```

### 7.3 `tag mcp auth status <server>`

Probe a single server's token validity in real time.

```
tag mcp auth status <server> [--json] [--introspect]
```

**Output:**

```
tag mcp auth status notion

notion token status:
  Valid:         yes
  Scopes:        read, write
  Expires at:    2026-06-17T18:45:00Z (7h 58m remaining)
  Token type:    Bearer
  Audience:      https://api.notion.com
  Refresh token: present (not expirable)
  Keychain:      tag-mcp-notion / access_token
```

### 7.4 `tag mcp auth revoke <server>`

Revoke the token server-side and remove keychain entries.

```
tag mcp auth revoke <server> [--all] [--json] [--yes]
```

**Output:**

```
tag mcp auth revoke notion

  Revoking access token...  ✓  (POST https://api.notion.com/v1/oauth/revoke)
  Revoking refresh token... ✓
  Removing keychain entry... ✓  (tag-mcp-notion)

  notion credentials removed.
```

### 7.5 `tag mcp auth refresh <server>`

Force a token refresh without waiting for expiry.

```
tag mcp auth refresh <server> [--all] [--json]
```

### 7.6 `tag mcp auth export`

Export credential metadata (NOT tokens) for portability.

```
tag mcp auth export [--server <name>] [--output <file>]
# Exports: server name, scopes, expiry, flow type. Never exports token values.
```

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `mcp_auth.py` MUST implement the five-step MCP OAuth 2.1 discovery chain: parse `WWW-Authenticate` header from a 401 response to locate the PRM URL; fetch PRM (`application/json` at `/.well-known/oauth-protected-resource`); follow `authorization_servers[0]` to AS metadata (RFC 8414); perform dynamic client registration (RFC 7591) if `registration_endpoint` is present; initiate PKCE or device flow. | P0 |
| FR-02 | PKCE flow MUST use `code_challenge_method=S256`; `code_verifier` MUST be a cryptographically random 43–128 character string (base64url, no padding); `code_challenge = BASE64URL(SHA256(ASCII(code_verifier)))`. | P0 |
| FR-03 | Every authorisation request and token request MUST include `resource=<mcp_server_uri>` (RFC 8707). Requests without this parameter MUST be rejected by the client before sending. | P0 |
| FR-04 | The `is_headless()` function MUST return `True` when none of `DISPLAY`, `WAYLAND_DISPLAY`, `BROWSER` are set, OR when `SSH_CLIENT` or `SSH_TTY` is set, OR when `CI` is set. | P0 |
| FR-05 | When `is_headless()` is `True` and the AS metadata contains `device_authorization_endpoint`, the system MUST automatically use device flow. When headless but no `device_authorization_endpoint` exists, the system MUST fail with error code `AUTH_NO_DEVICE_FLOW` and a human-readable message. | P0 |
| FR-06 | Token storage MUST use `keyring.set_password(service=f"tag-mcp-{server_name}", username="access_token", password=<token>)` for access tokens and `keyring.set_password(service=f"tag-mcp-{server_name}", username="refresh_token", password=<token>)` for refresh tokens. Token values MUST NOT appear in any log output, trace attribute, or SQLite row. | P0 |
| FR-07 | Token metadata (expiry timestamp, scopes, server URI, client_id, flow_type) MUST be stored in the `mcp_auth_accounts` SQLite table (see Section 9.2). The `access_token` and `refresh_token` columns in this table MUST contain only keychain lookup keys, not token values. | P0 |
| FR-08 | Before initiating any authorisation flow that includes a scope in the `DESTRUCTIVE_SCOPES` set (see Section 9.3), the system MUST display the scope name, a human-readable description of what it permits, and prompt `[y/N]`. The flow MUST NOT proceed if the user responds `N` or does not respond. This gate is skippable only with `--yes` in non-interactive mode. | P1 |
| FR-09 | The PKCE callback server MUST bind to `127.0.0.1:<port>` (default 9753), generate a random `state` parameter, verify `state` on callback receipt, and shut down within 5 seconds of receiving the callback regardless of success or failure. | P0 |
| FR-10 | Device flow polling MUST start at the `interval` returned by the device authorisation endpoint (default 5 s), apply exponential backoff by adding 5 s on each `slow_down` error response, and stop after `expires_in` seconds or `--timeout` seconds, whichever is shorter. | P0 |
| FR-11 | The background refresh task MUST proactively refresh tokens when `expires_at - now() < 5 minutes`. It MUST be invoked as a side-effect of `get_token(server_name)` (lazy refresh), not as a persistent daemon. | P1 |
| FR-12 | When a refresh token is used and returns `invalid_grant`, the system MUST delete the keychain entry, update `mcp_auth_accounts.status = 'expired'`, and raise `TokenExpiredError` with a message directing the user to run `tag mcp auth <server>` again. | P1 |
| FR-13 | `tag mcp auth revoke <server>` MUST POST to the `revocation_endpoint` from AS metadata with `token=<access_token>` and `token_type_hint=access_token`, then repeat with the refresh token. Both requests MUST use `client_id` in the body (not Basic auth for public clients). | P1 |
| FR-14 | `tag mcp auth list` MUST complete in < 200 ms for up to 20 accounts by reading only from the `mcp_auth_accounts` SQLite table without making any network calls. | P1 |
| FR-15 | `tag mcp auth status <server>` with `--introspect` MUST POST to `introspection_endpoint` (RFC 7662) if available; otherwise MUST make a lightweight authenticated GET to a well-known API endpoint (e.g., `https://api.github.com/user` for GitHub) and interpret 200 as valid, 401 as expired. | P2 |
| FR-16 | All six pre-configured server profiles (github, notion, google/calendar, google/drive, google/gmail, slack, stripe) MUST be hard-coded in `KNOWN_SERVERS` dict with: `as_metadata_url`, `default_scopes`, `resource_uri`, and `redirect_uri_required` flag. | P1 |
| FR-17 | `tag mcp auth <unknown_server> --as-url <url> --resource <uri>` MUST work for arbitrary RFC 8414-compliant authorisation servers. | P2 |
| FR-18 | Every OAuth lifecycle event (discovery, registration, authorisation, token exchange, refresh, revocation) MUST be recorded as a child span under the active TAG trace, with attributes: `mcp.server_name`, `mcp.flow_type`, `mcp.scope`, `oauth.step`, `http.status_code`, `duration_ms`. | P2 |
| FR-19 | `tag mcp auth export` MUST write only metadata (server name, scopes, expiry, flow type) to the output file. It MUST raise `ExportSecurityError` if any field value matches the secret scanning patterns from PRD-034. | P2 |
| FR-20 | Dynamic client registrations MUST be stored in `mcp_auth_registrations` table and reused across sessions for the same `(server_name, as_url)` pair. Re-registration MUST only occur when the existing `client_id` is rejected with a `invalid_client` error. | P2 |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Discovery chain latency (401 → token obtained) | ≤ 2 s net I/O; user-perceived time dominated by browser or device flow consent |
| NFR-02 | Local PKCE callback server startup | ≤ 100 ms from `tag mcp auth` invocation to `http://localhost:PORT/callback` accepting connections |
| NFR-03 | `tag mcp auth list` cold render | ≤ 200 ms for 20 accounts (SQLite read only, no network) |
| NFR-04 | Token refresh latency | ≤ 500 ms for a successful refresh token exchange (single HTTPS POST) |
| NFR-05 | Keychain operation latency | ≤ 50 ms per `keyring.get_password` / `keyring.set_password` on macOS Keychain |
| NFR-06 | Memory footprint | `mcp_auth.py` import adds ≤ 5 MB RSS; no background threads unless actively polling in device flow |
| NFR-07 | Dependency footprint | New mandatory dependencies limited to: `keyring>=25.0`, `cryptography>=42.0` (for PKCE SHA-256); `httpx` is already used in TAG |
| NFR-08 | Cross-platform keychain support | macOS (Keychain), Linux (Secret Service via `secretstorage`), Windows (Credential Manager) — all via `keyring` abstraction |
| NFR-09 | Test coverage | `mcp_auth.py` MUST have ≥ 90% line coverage in pytest; all network calls MUST be mockable via `respx` (httpx mock library) |
| NFR-10 | No token values in logs | TAG's log level `DEBUG` MUST NOT emit token values; `mcp_auth.py` MUST mask tokens in all log calls with `mask_secret()` from `security.py` |
| NFR-11 | Graceful keychain absence | If `keyring` raises `NoKeyringError` (e.g., headless Linux without Secret Service), the system MUST fail with a clear error: `"No secure keychain available. Install libsecret-1-dev and python3-secretstorage."` — never fall back to plaintext file storage. | 
| NFR-12 | Concurrent auth safety | Concurrent `tag mcp auth` calls for the same server MUST use SQLite's WAL mode advisory lock (via `open_db()`) to prevent duplicate registrations. |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/mcp_auth.py` | Primary implementation: discovery, flows, token store, refresh, revocation |
| `tests/test_mcp_auth.py` | Unit and integration tests with `respx` mock AS |
| `tests/fixtures/as_metadata_github.json` | Fixture AS metadata for GitHub mock |
| `tests/fixtures/as_metadata_notion.json` | Fixture AS metadata for Notion mock |

### 10.2 SQLite DDL

```sql
-- Migration: 0020_mcp_auth.sql
-- Applied via open_db() migration runner in controller.py

CREATE TABLE IF NOT EXISTS mcp_auth_accounts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    server_name     TEXT    NOT NULL,           -- e.g. 'notion', 'github', 'google/calendar'
    resource_uri    TEXT    NOT NULL,           -- audience: e.g. 'https://api.notion.com'
    as_url          TEXT    NOT NULL,           -- authorisation server metadata URL
    client_id       TEXT    NOT NULL,           -- from dynamic registration or well-known
    flow_type       TEXT    NOT NULL CHECK(flow_type IN ('pkce', 'device')),
    scopes          TEXT    NOT NULL,           -- space-separated scope string
    keychain_service TEXT   NOT NULL,           -- e.g. 'tag-mcp-notion'
    status          TEXT    NOT NULL DEFAULT 'valid'
                            CHECK(status IN ('valid', 'expired', 'revoked', 'refresh_failed')),
    expires_at      TEXT,                       -- ISO-8601 UTC; NULL = non-expiring
    refresh_expires_at TEXT,                    -- ISO-8601 UTC; NULL = unknown/non-expiring
    profile_name    TEXT,                       -- TAG profile association; NULL = global
    org_hint        TEXT,                       -- GitHub org restriction hint
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(server_name, profile_name)
);

CREATE TABLE IF NOT EXISTS mcp_auth_registrations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    server_name     TEXT    NOT NULL,
    as_url          TEXT    NOT NULL,           -- authorisation server base URL
    client_id       TEXT    NOT NULL,
    -- client_secret stored in keychain under 'tag-mcp-reg-{server_name}' / 'client_secret'
    -- never stored in SQLite
    registration_access_token_key TEXT,        -- keychain lookup key for RAT, if issued
    redirect_uris   TEXT,                       -- JSON array
    grant_types     TEXT,                       -- JSON array
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(server_name, as_url)
);

CREATE TABLE IF NOT EXISTS mcp_auth_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    server_name     TEXT    NOT NULL,
    event_type      TEXT    NOT NULL CHECK(event_type IN (
                        'discovery', 'registration', 'authorise',
                        'token_exchange', 'refresh', 'revoke', 'introspect'
                    )),
    flow_type       TEXT,
    success         INTEGER NOT NULL CHECK(success IN (0, 1)),
    http_status     INTEGER,
    duration_ms     INTEGER,
    error_code      TEXT,
    trace_span_id   TEXT,                       -- PRD-013 span ID
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_mcp_auth_accounts_server
    ON mcp_auth_accounts(server_name);
CREATE INDEX IF NOT EXISTS idx_mcp_auth_events_server_type
    ON mcp_auth_events(server_name, event_type);
```

### 10.3 Core Dataclasses

```python
from __future__ import annotations
from dataclasses import dataclass, field
from typing import Optional, List, Literal
import secrets
import hashlib
import base64

FlowType = Literal["pkce", "device"]
TokenStatus = Literal["valid", "expired", "revoked", "refresh_failed"]


@dataclass
class ASMetadata:
    """RFC 8414 Authorization Server Metadata."""
    issuer: str
    authorization_endpoint: str
    token_endpoint: str
    registration_endpoint: Optional[str] = None
    revocation_endpoint: Optional[str] = None
    introspection_endpoint: Optional[str] = None
    device_authorization_endpoint: Optional[str] = None
    scopes_supported: List[str] = field(default_factory=list)
    code_challenge_methods_supported: List[str] = field(default_factory=list)
    grant_types_supported: List[str] = field(default_factory=list)


@dataclass
class PRMDocument:
    """MCP Protected Resource Metadata document."""
    resource: str                           # canonical MCP server URI (audience)
    authorization_servers: List[str]        # list of AS issuer URLs
    bearer_methods_supported: List[str] = field(default_factory=lambda: ["header"])
    resource_documentation: Optional[str] = None
    scopes_supported: List[str] = field(default_factory=list)


@dataclass
class DynamicClientRegistration:
    """Result of RFC 7591 dynamic client registration."""
    client_id: str
    client_secret: Optional[str]            # None for public clients
    redirect_uris: List[str]
    grant_types: List[str]
    registration_access_token: Optional[str]  # stored in keychain, not here
    registration_client_uri: Optional[str]


@dataclass
class PKCEParams:
    """PKCE S256 challenge parameters."""
    code_verifier: str
    code_challenge: str
    code_challenge_method: str = "S256"
    state: str = field(default_factory=lambda: secrets.token_urlsafe(32))

    @classmethod
    def generate(cls) -> "PKCEParams":
        verifier = secrets.token_urlsafe(64)[:96]  # 96 chars, well within 43-128
        challenge_bytes = hashlib.sha256(verifier.encode("ascii")).digest()
        challenge = base64.urlsafe_b64encode(challenge_bytes).rstrip(b"=").decode()
        return cls(code_verifier=verifier, code_challenge=challenge)


@dataclass
class DeviceAuthResponse:
    """RFC 8628 device authorisation response."""
    device_code: str
    user_code: str
    verification_uri: str
    verification_uri_complete: Optional[str]
    expires_in: int
    interval: int = 5


@dataclass
class TokenResponse:
    """OAuth token exchange response."""
    access_token: str                       # stored in keychain only
    token_type: str
    expires_in: Optional[int]
    refresh_token: Optional[str]            # stored in keychain only
    scope: Optional[str]
    resource: Optional[str]                 # RFC 8707 audience echo


@dataclass
class ConnectedAccount:
    """Hydrated view of an mcp_auth_accounts row (no token values)."""
    server_name: str
    resource_uri: str
    as_url: str
    client_id: str
    flow_type: FlowType
    scopes: List[str]
    keychain_service: str
    status: TokenStatus
    expires_at: Optional[str]
    refresh_expires_at: Optional[str]
    profile_name: Optional[str]
    org_hint: Optional[str]


@dataclass
class KnownServerProfile:
    """Pre-configured profile for a well-known MCP server."""
    name: str                               # canonical slug: 'github', 'notion', etc.
    resource_uri: str                       # RFC 8707 audience
    as_metadata_url: str                    # RFC 8414 discovery URL
    default_scopes: List[str]
    redirect_uri_required: bool = True      # False for device-flow-only servers
    destructive_scopes: List[str] = field(default_factory=list)
    scope_descriptions: dict = field(default_factory=dict)
```

### 10.4 Known Server Registry

```python
KNOWN_SERVERS: dict[str, KnownServerProfile] = {
    "github": KnownServerProfile(
        name="github",
        resource_uri="https://github.com",
        as_metadata_url="https://github.com/.well-known/oauth-authorization-server",
        default_scopes=["repo", "issues:write"],
        redirect_uri_required=True,
        destructive_scopes=["repo:delete", "admin:org", "delete_repo", "write:packages"],
        scope_descriptions={
            "repo": "Full read/write access to repositories",
            "admin:org": "Full admin access to org settings",
            "delete_repo": "Allows deleting repositories",
        },
    ),
    "notion": KnownServerProfile(
        name="notion",
        resource_uri="https://api.notion.com",
        as_metadata_url="https://api.notion.com/.well-known/oauth-authorization-server",
        default_scopes=["read"],
        redirect_uri_required=True,
        destructive_scopes=["write", "delete"],
        scope_descriptions={
            "write": "Create and update pages and blocks",
            "delete": "Delete pages and databases",
        },
    ),
    "google/calendar": KnownServerProfile(
        name="google/calendar",
        resource_uri="https://www.googleapis.com/auth/calendar",
        as_metadata_url="https://accounts.google.com/.well-known/openid-configuration",
        default_scopes=["https://www.googleapis.com/auth/calendar.readonly"],
        redirect_uri_required=True,
        destructive_scopes=["https://www.googleapis.com/auth/calendar"],
        scope_descriptions={
            "https://www.googleapis.com/auth/calendar": "Full read/write access to calendars and events",
        },
    ),
    "google/drive": KnownServerProfile(
        name="google/drive",
        resource_uri="https://www.googleapis.com/auth/drive",
        as_metadata_url="https://accounts.google.com/.well-known/openid-configuration",
        default_scopes=["https://www.googleapis.com/auth/drive.readonly"],
        redirect_uri_required=True,
        destructive_scopes=["https://www.googleapis.com/auth/drive"],
        scope_descriptions={
            "https://www.googleapis.com/auth/drive": "Full read/write/delete access to Drive files",
        },
    ),
    "google/gmail": KnownServerProfile(
        name="google/gmail",
        resource_uri="https://www.googleapis.com/auth/gmail",
        as_metadata_url="https://accounts.google.com/.well-known/openid-configuration",
        default_scopes=["https://www.googleapis.com/auth/gmail.readonly"],
        redirect_uri_required=True,
        destructive_scopes=["https://www.googleapis.com/auth/gmail.modify", "https://www.googleapis.com/auth/gmail.send"],
        scope_descriptions={
            "https://www.googleapis.com/auth/gmail.send": "Send email on your behalf",
            "https://www.googleapis.com/auth/gmail.modify": "Read, compose, send, and delete email",
        },
    ),
    "slack": KnownServerProfile(
        name="slack",
        resource_uri="https://slack.com/api",
        as_metadata_url="https://slack.com/.well-known/openid-configuration",
        default_scopes=["channels:read", "chat:write"],
        redirect_uri_required=True,
        destructive_scopes=["chat:write", "files:write", "admin"],
        scope_descriptions={
            "chat:write": "Post messages in channels",
            "admin": "Administer the workspace",
        },
    ),
    "stripe": KnownServerProfile(
        name="stripe",
        resource_uri="https://api.stripe.com",
        as_metadata_url="https://connect.stripe.com/.well-known/openid-configuration",
        default_scopes=["read_only"],
        redirect_uri_required=True,
        destructive_scopes=["read_write"],
        scope_descriptions={
            "read_write": "Create, update, and delete Stripe objects including charges and refunds",
        },
    ),
    "linear": KnownServerProfile(
        name="linear",
        resource_uri="https://api.linear.app",
        as_metadata_url="https://api.linear.app/.well-known/oauth-authorization-server",
        default_scopes=["read"],
        redirect_uri_required=True,
        destructive_scopes=["write", "issues:archive"],
        scope_descriptions={
            "write": "Create and update issues, cycles, and projects",
            "issues:archive": "Archive and delete issues",
        },
    ),
}

DESTRUCTIVE_SCOPE_GATE: set[str] = {
    s for profile in KNOWN_SERVERS.values()
    for s in profile.destructive_scopes
}
```

### 10.5 Core Algorithm: Discovery Chain

```python
import httpx
import json
from typing import Tuple

async def discover_auth_server(
    mcp_server_uri: str,
    client: httpx.AsyncClient,
) -> Tuple[PRMDocument, ASMetadata]:
    """
    Five-step MCP OAuth 2.1 discovery chain.

    Step 1: Probe the MCP server root and capture 401 WWW-Authenticate header.
    Step 2: Parse the header for resource_metadata parameter → PRM URL.
    Step 3: GET PRM URL → PRMDocument (authorization_servers[0]).
    Step 4: GET {as_issuer}/.well-known/oauth-authorization-server → ASMetadata.
    Step 5: Return (PRMDocument, ASMetadata) for caller to proceed with registration.
    """
    # Step 1: Probe — expect 401 with WWW-Authenticate
    resp = await client.get(mcp_server_uri, follow_redirects=True)
    if resp.status_code != 401:
        raise DiscoveryError(
            f"Expected 401 from MCP server probe, got {resp.status_code}. "
            "Server may not require auth or URL is incorrect."
        )

    www_auth = resp.headers.get("WWW-Authenticate", "")
    if not www_auth:
        raise DiscoveryError("401 response missing WWW-Authenticate header.")

    # Step 2: Parse WWW-Authenticate for resource_metadata
    prm_url = _parse_resource_metadata_url(www_auth)
    if not prm_url:
        raise DiscoveryError(
            f"WWW-Authenticate header present but no resource_metadata parameter: {www_auth!r}"
        )

    # Step 3: Fetch PRM document
    prm_resp = await client.get(prm_url)
    prm_resp.raise_for_status()
    prm_data = prm_resp.json()
    prm = PRMDocument(
        resource=prm_data["resource"],
        authorization_servers=prm_data["authorization_servers"],
        bearer_methods_supported=prm_data.get("bearer_methods_supported", ["header"]),
        scopes_supported=prm_data.get("scopes_supported", []),
    )

    if not prm.authorization_servers:
        raise DiscoveryError("PRM document has empty authorization_servers list.")

    # Step 4: Fetch AS metadata (RFC 8414)
    as_issuer = prm.authorization_servers[0]
    as_meta_url = f"{as_issuer.rstrip('/')}/.well-known/oauth-authorization-server"
    as_resp = await client.get(as_meta_url)
    as_resp.raise_for_status()
    as_data = as_resp.json()
    as_meta = ASMetadata(
        issuer=as_data["issuer"],
        authorization_endpoint=as_data["authorization_endpoint"],
        token_endpoint=as_data["token_endpoint"],
        registration_endpoint=as_data.get("registration_endpoint"),
        revocation_endpoint=as_data.get("revocation_endpoint"),
        introspection_endpoint=as_data.get("introspection_endpoint"),
        device_authorization_endpoint=as_data.get("device_authorization_endpoint"),
        scopes_supported=as_data.get("scopes_supported", []),
        code_challenge_methods_supported=as_data.get(
            "code_challenge_methods_supported", ["S256"]
        ),
        grant_types_supported=as_data.get(
            "grant_types_supported", ["authorization_code"]
        ),
    )

    return prm, as_meta
```

### 10.6 Core Algorithm: Headless Detection

```python
import os

def is_headless() -> bool:
    """
    Return True when no browser-capable display is available.
    Priority order: explicit env overrides > SSH indicator > CI indicator > display check.
    """
    # Explicit override
    if os.environ.get("TAG_MCP_FORCE_DEVICE_FLOW") == "1":
        return True
    if os.environ.get("TAG_MCP_FORCE_PKCE_FLOW") == "1":
        return False

    # SSH session indicators
    if os.environ.get("SSH_CLIENT") or os.environ.get("SSH_TTY"):
        return True

    # CI environment indicators
    if os.environ.get("CI") or os.environ.get("GITHUB_ACTIONS"):
        return True

    # Display availability (Linux/macOS)
    if os.environ.get("DISPLAY") or os.environ.get("WAYLAND_DISPLAY"):
        return False

    # macOS: Aqua sessions always have a display
    if os.uname().sysname == "Darwin" and not os.environ.get("SSH_CLIENT"):
        return False

    # WSL / Windows with browser configured
    if os.environ.get("BROWSER"):
        return False

    # Default headless if no display evidence found
    return True
```

### 10.7 Core Algorithm: PKCE Local Callback Server

```python
import asyncio
import urllib.parse
from http.server import HTTPServer, BaseHTTPRequestHandler
from typing import Optional

class _CallbackHandler(BaseHTTPRequestHandler):
    """Minimal HTTP handler to capture OAuth redirect callback."""
    result: Optional[dict] = None
    expected_state: str = ""

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        params = dict(urllib.parse.parse_qsl(parsed.query))

        if parsed.path != "/callback":
            self.send_response(404)
            self.end_headers()
            return

        if params.get("state") != self.expected_state:
            self.send_response(400)
            self.end_headers()
            self.wfile.write(b"State mismatch. Possible CSRF. Close this window.")
            _CallbackHandler.result = {"error": "state_mismatch"}
            return

        if "error" in params:
            self.send_response(400)
            self.end_headers()
            self.wfile.write(
                f"Authorization failed: {params['error']}. Close this window.".encode()
            )
            _CallbackHandler.result = params
            return

        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"Authorized! You may close this window and return to TAG.")
        _CallbackHandler.result = params

    def log_message(self, format, *args):
        pass  # suppress default HTTP server logging


async def run_pkce_callback_server(
    port: int,
    state: str,
    timeout: int = 300,
) -> dict:
    """
    Start a local HTTP server on 127.0.0.1:<port>, wait for the OAuth callback,
    validate state, return query params dict. Raises TimeoutError after `timeout` seconds.
    """
    _CallbackHandler.expected_state = state
    _CallbackHandler.result = None

    server = HTTPServer(("127.0.0.1", port), _CallbackHandler)
    server.timeout = 1.0  # poll interval

    deadline = asyncio.get_event_loop().time() + timeout
    loop = asyncio.get_event_loop()

    while _CallbackHandler.result is None:
        if loop.time() > deadline:
            server.server_close()
            raise TimeoutError(
                f"No OAuth callback received within {timeout}s. "
                "Did you complete the browser flow?"
            )
        await loop.run_in_executor(None, server.handle_request)

    server.server_close()
    return _CallbackHandler.result
```

### 10.8 Integration Points

| Component | Integration |
|-----------|-------------|
| `controller.py` | Registers `tag mcp auth` subcommand family; calls `mcp_auth.cmd_auth()`, `cmd_list()`, `cmd_status()`, `cmd_revoke()`, `cmd_refresh()` |
| `open_db()` | Used in `mcp_auth.py` for all SQLite reads/writes; migration `0020_mcp_auth.sql` applied on first import |
| `security.py` | `mask_secret(token)` used before any log call involving token strings; `scan_for_secrets()` called in `cmd_export()` |
| `tracing.py` | `create_span("mcp.auth.<step>")` wraps each OAuth lifecycle step; span attributes follow PRD-013 conventions |
| `notifications.py` | `notify_user("Token for {server} expiring in 10 minutes")` sent when proactive refresh is triggered |
| `hermes_bridge.py` | `get_token(server_name)` called by the MCP client connection layer before each tool call; returns bearer token string from keychain |

### 10.9 `get_token()` — Primary Consumer API

```python
import keyring
from datetime import datetime, timezone

def get_token(server_name: str, profile_name: Optional[str] = None) -> str:
    """
    Primary API consumed by the MCP client layer (hermes_bridge.py).
    Returns a valid Bearer token string for the named server.
    Raises TokenExpiredError if token cannot be refreshed.
    Raises NoTokenError if no connected account exists for this server.
    """
    db = open_db()
    row = db.execute(
        "SELECT * FROM mcp_auth_accounts WHERE server_name = ? AND profile_name IS ?",
        (server_name, profile_name),
    ).fetchone()

    if not row:
        raise NoTokenError(
            f"No connected account for '{server_name}'. "
            f"Run: tag mcp auth {server_name}"
        )

    if row["status"] in ("revoked",):
        raise TokenRevokedError(
            f"Token for '{server_name}' has been revoked. "
            f"Run: tag mcp auth {server_name}"
        )

    # Proactive refresh if within 5 minutes of expiry
    if row["expires_at"]:
        expires_at = datetime.fromisoformat(row["expires_at"].replace("Z", "+00:00"))
        now = datetime.now(tz=timezone.utc)
        if (expires_at - now).total_seconds() < 300:
            _refresh_token_sync(server_name, row, db)
            # Re-read after refresh
            row = db.execute(
                "SELECT * FROM mcp_auth_accounts WHERE server_name = ? AND profile_name IS ?",
                (server_name, profile_name),
            ).fetchone()

    if row["status"] == "expired":
        raise TokenExpiredError(
            f"Token for '{server_name}' has expired and could not be refreshed. "
            f"Run: tag mcp auth {server_name}"
        )

    # Retrieve from keychain
    token = keyring.get_password(row["keychain_service"], "access_token")
    if not token:
        raise NoTokenError(
            f"Token for '{server_name}' not found in keychain. "
            f"Run: tag mcp auth {server_name}"
        )

    return token
```

---

## 11. Security Considerations

1. **Token isolation per server:** Every token is audience-bound to a specific `resource` URI (RFC 8707). The `get_token()` function returns only the token for the requested server; there is no API to enumerate all tokens in a single call. Cross-server token reuse is architecturally impossible because each token lives under a distinct keychain service key.

2. **No plaintext token storage:** `keyring` is the only write path for token values. `mcp_auth.py` MUST be audited to ensure that no code path writes a token string to: SQLite, any file under `~/.tag/`, any log sink, or any environment variable. The `TokenResponse` dataclass is ephemeral (not persisted); only the keychain call persists token values.

3. **PKCE state parameter CSRF protection:** The `state` parameter is a 256-bit random value generated per authorisation request. The callback handler verifies state before accepting any `code` parameter. A mismatch immediately closes the callback server and raises an error without exchanging the code.

4. **Local callback server binding:** The PKCE callback server MUST bind only to `127.0.0.1`, never `0.0.0.0`. This prevents remote machines on the same network from intercepting the OAuth callback, which is a known attack vector for loopback redirect URIs.

5. **Dynamic client registration security:** `client_secret` values from dynamic registration are stored in the keychain under `tag-mcp-reg-{server_name} / client_secret`. They are never stored in SQLite, never logged, and never included in `tag mcp auth export` output.

6. **Destructive scope HITL gate:** Any scope in `DESTRUCTIVE_SCOPE_GATE` triggers an explicit `[y/N]` prompt before the authorisation URL is opened. This prevents accidental grant of write/delete/admin access when the user intended read-only. The gate cannot be bypassed programmatically except via `--yes` (intended for automated tests with mocked AS endpoints).

7. **Device code expiry enforcement:** The device flow polling loop MUST check `expires_in` from the device authorisation response and abort polling when the device code expires. Expired device codes MUST NOT be reused; the user must restart the auth flow.

8. **Refresh token rotation:** When an AS issues a new refresh token on each refresh (RFC 6749 §10.4), the old refresh token MUST be immediately deleted from the keychain and replaced with the new one. Failure to do this can lead to token accumulation and potential replay if the keychain is compromised.

9. **Secret scanning on export:** `cmd_export()` calls `scan_for_secrets()` (PRD-034) on the output buffer before writing to disk. This prevents accidental export of tokens that somehow ended up in metadata fields.

10. **Revocation on `tag profile delete`:** When a TAG profile is deleted (future hook), `mcp_auth.py` MUST expose `revoke_all(profile_name=...)` to clean up associated keychain entries. This prevents orphaned tokens from accumulating indefinitely.

11. **Audit trail in `mcp_auth_events`:** Every token exchange, refresh, and revocation is recorded with a timestamp and success/failure status. This table enables after-the-fact auditing of which systems were accessed and when, without exposing token values.

12. **No token in LLM context:** `hermes_bridge.py` MUST inject the Bearer token as an HTTP header in the MCP transport layer, never as a tool argument or system prompt field. The LLM context window MUST never contain a token string.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_mcp_auth.py`)

Each function in `mcp_auth.py` is tested in isolation with `respx` (httpx async mock) for all HTTP calls and `unittest.mock.patch("keyring.set_password")` / `keyring.get_password` for keychain calls.

| Test | Verifies |
|------|----------|
| `test_pkce_params_generation` | `code_verifier` length 43–128; `code_challenge = b64url(sha256(verifier))`; unique per call |
| `test_is_headless_ssh` | Returns `True` when `SSH_CLIENT` is set |
| `test_is_headless_display` | Returns `False` when `DISPLAY` is set and `SSH_CLIENT` is not |
| `test_is_headless_ci` | Returns `True` when `CI=true` |
| `test_discovery_chain_happy_path` | 401 → PRM → AS metadata parsed correctly with `respx` mocks |
| `test_discovery_missing_www_auth` | `DiscoveryError` raised when 401 has no `WWW-Authenticate` |
| `test_discovery_no_device_endpoint_headless` | `AUTH_NO_DEVICE_FLOW` error when headless but AS has no `device_authorization_endpoint` |
| `test_pkce_state_mismatch` | Callback server rejects mismatched state |
| `test_pkce_callback_timeout` | `TimeoutError` raised after timeout seconds |
| `test_device_flow_slow_down_backoff` | Polling interval increases by 5 s on each `slow_down` |
| `test_device_flow_expire` | Polling stops when device code expires |
| `test_token_stored_in_keychain_not_sqlite` | After auth, `access_token` absent from all SQLite rows |
| `test_get_token_proactive_refresh` | Refresh called when `expires_at - now < 5m` |
| `test_get_token_refresh_failure` | `TokenExpiredError` raised when `invalid_grant` returned |
| `test_revoke_calls_revocation_endpoint` | `POST /revoke` called with both `access_token` and `refresh_token` |
| `test_destructive_scope_gate_blocks_flow` | Auth flow not started when user responds `N` to HITL prompt |
| `test_resource_param_in_auth_request` | `resource=` present in authorisation URL query string |
| `test_resource_param_in_token_request` | `resource=` present in token exchange POST body |
| `test_no_token_in_logs` | `caplog` contains no string matching `r"[A-Za-z0-9_\-]{20,}"` after auth |

### 12.2 Integration Tests

Integration tests run against a mock OAuth 2.1 authorisation server implemented with `respx`. These tests exercise the full end-to-end flow including SQLite writes.

```python
# tests/test_mcp_auth_integration.py
import pytest
import respx
import keyring.backend
from tag.mcp_auth import cmd_auth, cmd_list, cmd_revoke, get_token

class MemoryKeyring(keyring.backend.KeyringBackend):
    """In-memory keyring for testing."""
    _store: dict[tuple, str] = {}
    priority = 100

    def set_password(self, service, username, password):
        self._store[(service, username)] = password

    def get_password(self, service, username):
        return self._store.get((service, username))

    def delete_password(self, service, username):
        self._store.pop((service, username), None)


@pytest.fixture(autouse=True)
def use_memory_keyring(monkeypatch):
    kr = MemoryKeyring()
    monkeypatch.setattr("keyring.get_keyring", lambda: kr)
    return kr


@respx.mock
async def test_full_pkce_flow_notion(tmp_path, monkeypatch):
    """Full PKCE flow for Notion with mock AS and callback server."""
    # ... respx route setup for PRM, AS metadata, registration, token exchange
    # ... monkeypatch callback server to inject code immediately
    # Assert: keychain has access_token; SQLite row has status='valid'
    pass
```

### 12.3 Security Tests

| Test | Verifies |
|------|----------|
| `test_no_plaintext_tokens_in_tag_dir` | After full auth flow, `grep -r 'ghp_\|notion_secret' ~/.tag/` returns empty |
| `test_cross_server_token_isolation` | `get_token("github")` returns GitHub token; `get_token("notion")` returns Notion token; values are distinct |
| `test_callback_binds_only_loopback` | Callback server socket is bound to `127.0.0.1`, not `0.0.0.0` |
| `test_export_blocked_on_token_in_metadata` | `cmd_export()` raises `ExportSecurityError` when metadata field contains token-shaped string |

### 12.4 Performance Tests

| Test | Target | Method |
|------|--------|--------|
| `bench_list_20_accounts` | < 200 ms | `timeit` over 20 mock SQLite rows |
| `bench_get_token_cache_hit` | < 60 ms | `timeit` over 100 keychain reads |
| `bench_discovery_chain` | < 2 s net | Integration test with local mock AS server on localhost |

---

## 13. Acceptance Criteria

| ID | Criterion | How Verified |
|----|-----------|-------------|
| AC-01 | `tag mcp auth notion --scopes read,write` completes the PKCE flow and stores a valid token for Notion in the OS keychain | Integration test + `keyring.get_password("tag-mcp-notion", "access_token")` asserts non-null |
| AC-02 | `tag mcp auth github` with `SSH_CLIENT` set in environment selects device flow automatically without requiring `--flow device` | Unit test with mocked AS and `SSH_CLIENT=user@host` in env |
| AC-03 | The authorisation URL for any server always contains `resource=<server_uri>` as a query parameter | URL assertion in unit test for all six known servers |
| AC-04 | The token exchange POST body always contains `resource=<server_uri>` | Request body assertion in `respx` mock |
| AC-05 | No file under `~/.tag/` contains a string matching the access token after `tag mcp auth notion` completes | `grep -r` scan in integration test |
| AC-06 | `tag mcp auth list` renders within 200 ms for 20 mock accounts | `timeit` benchmark |
| AC-07 | `tag mcp auth revoke notion` calls the Notion revocation endpoint for both access token and refresh token, then removes the keychain entry | `respx` mock assertion + `keyring.get_password` returns `None` post-revoke |
| AC-08 | When `expires_at` is 4 minutes in the future, `get_token("notion")` triggers a refresh token exchange before returning the (new) access token | Unit test with mocked token endpoint returning new token |
| AC-09 | When `invalid_grant` is returned during refresh, `get_token()` raises `TokenExpiredError` with a message containing `tag mcp auth notion` | Unit test |
| AC-10 | `tag mcp auth notion --scopes write` prompts `[y/N]` before opening the browser, because `write` is in `DESTRUCTIVE_SCOPE_GATE` | Unit test with mocked stdin returning `N`; asserts no browser open call made |
| AC-11 | `tag mcp auth list --json` returns valid JSON with schema matching the `ConnectedAccount` dataclass (server, scopes, expires_at, status, has_refresh_token, ttl_seconds) | JSON schema assertion in unit test |
| AC-12 | `tag mcp auth github` for an unknown server with `--as-url https://example.com --resource https://example.com/mcp` completes the full discovery chain against the provided AS URL | Integration test with mock AS at `example.com` URL |
| AC-13 | Device flow polling backs off by 5 s on each `slow_down` response | Unit test asserting polling intervals: [5, 10, 15] s after two `slow_down` responses |
| AC-14 | All six known server profiles produce syntactically valid authorisation URLs when PKCE params are applied | Parametrised unit test across all six profiles |
| AC-15 | `tag mcp auth status notion --introspect` makes a POST to `introspection_endpoint` and correctly parses `active: true` | `respx` mock assertion |
| AC-16 | OAuth lifecycle events appear as child spans in the active TAG trace (PRD-013) | Span collection assertion in integration test |
| AC-17 | PKCE callback server shuts down within 5 seconds of receiving the callback, regardless of whether exchange succeeds | Timing assertion in integration test |
| AC-18 | `tag mcp auth list` shows `status: expired` for a token whose `expires_at` is in the past and for which no refresh token is present | Unit test with mocked SQLite row |

---

## 14. Dependencies

| Dependency | Type | Version | Reason |
|-----------|------|---------|--------|
| `keyring` | New runtime | `>=25.0` | Cross-platform OS keychain access |
| `cryptography` | New runtime | `>=42.0` | PKCE S256: `hazmat.primitives.hashes.SHA256` |
| `httpx` | Existing | `>=0.27` | All OAuth HTTP calls (already used in TAG) |
| `respx` | New dev/test | `>=0.21` | `httpx` mock for unit and integration tests |
| `secretstorage` | Optional runtime | `>=3.3` | Linux Secret Service backend for `keyring` |
| PRD-013 (tracing) | Internal PRD | Shipped | Span creation for OAuth lifecycle events |
| PRD-034 (secret scanning) | Internal PRD | Shipped | `mask_secret()` and `scan_for_secrets()` used in export |
| PRD-028 (sandbox) | Internal PRD | Shipped | PKCE callback server runs inside sandbox boundary |
| MCP OAuth 2.1 spec | External spec | 2025 | Protocol definition |
| RFC 7636 (PKCE) | External spec | — | Code challenge method S256 |
| RFC 8628 (Device Flow) | External spec | — | Headless authorisation |
| RFC 8414 (AS Metadata) | External spec | — | Discovery endpoint convention |
| RFC 7591 (Dynamic Registration) | External spec | — | Per-server client registration |
| RFC 8707 (Resource Indicators) | External spec | — | Audience binding for tokens |
| RFC 7009 (Token Revocation) | External spec | — | `revocation_endpoint` POST |
| RFC 7662 (Token Introspection) | External spec | — | `--introspect` status check |

---

## 15. Open Questions

| ID | Question | Owner | Resolution Target |
|----|----------|-------|-------------------|
| OQ-01 | Should dynamic client registrations be shared across TAG profiles, or be per-profile? Current design: shared by `(server_name, as_url)`, profile only affects token storage. Implication: a revoked registration affects all profiles. | Platform team | Week 1 |
| OQ-02 | Google Workspace requires creating an OAuth client in Google Cloud Console — dynamic client registration (RFC 7591) is not supported by Google. How should the user be guided through manual client setup for Google servers? Pre-populated instructions in `tag mcp auth google/calendar`? | UX | Week 1 |
| OQ-03 | Should `tag mcp auth refresh --all` be a safe operation callable from cron (PRD-022) to proactively refresh all tokens nightly? Risk: triggers device flow re-auth if a refresh token has expired, blocking the cron job. Proposed: skip re-auth in cron mode; emit warning instead. | Platform team | Week 2 |
| OQ-04 | The `keyring` library uses OS keychain backends which may prompt the user for their macOS login password on first access. Is this acceptable UX, or should we document `security unlock-keychain` as a pre-step in CI pipelines? | DevEx | Week 1 |
| OQ-05 | Should `tag mcp auth` support multiple connected accounts for the same server (e.g., two GitHub accounts)? Current design: `UNIQUE(server_name, profile_name)` allows one account per profile. Multi-account would require a named-account slug. Defer? | Product | Week 1 |
| OQ-06 | What is the right behaviour when `tag mcp auth notion` is run with an existing valid token? Current spec: prompt "Token valid until X. Re-authorise? [y/N]" unless `--force` is passed. Confirm this is correct. | UX | Week 1 |
| OQ-07 | Stripe's OAuth 2.0 implementation (Stripe Connect) uses a non-standard token format and does not support RFC 8414 AS metadata discovery. Does Stripe offer a compliant MCP server, or must we hardcode Stripe's endpoints entirely? | Research | Week 1 |
| OQ-08 | Linear's MCP server is in early access. Should it be listed as `experimental` in `KNOWN_SERVERS` with a warning on first use? | Product | Week 2 |

---

## 16. Complexity and Timeline

**Total estimated effort:** 9–11 engineering days (1–2 weeks)

### Phase 1: Core Infrastructure (Days 1–3)

- SQLite migration (`0020_mcp_auth.sql`): create `mcp_auth_accounts`, `mcp_auth_registrations`, `mcp_auth_events` tables
- Dataclasses: `PKCEParams`, `ASMetadata`, `PRMDocument`, `TokenResponse`, `ConnectedAccount`, `KnownServerProfile`
- `KNOWN_SERVERS` registry with all six pre-configured profiles
- `is_headless()` detection logic with full env-var matrix
- Unit tests for all of the above (target: 100% line coverage on pure-logic code)

### Phase 2: OAuth Flows (Days 4–6)

- Discovery chain: `discover_auth_server()` with PRM → AS metadata steps
- Dynamic client registration: `register_client()` with keychain storage of `client_secret`
- PKCE flow: `run_pkce_flow()` including local callback server, browser open, code exchange
- Device flow: `run_device_flow()` including polling loop with exponential backoff on `slow_down`
- Token storage: `store_tokens()` via `keyring` + SQLite metadata update
- Destructive scope HITL gate
- Unit + integration tests with `respx` mock AS

### Phase 3: Token Lifecycle (Days 7–8)

- `get_token()` with proactive refresh logic
- `_refresh_token_sync()` background refresh
- `TokenExpiredError` / `NoTokenError` error hierarchy
- `cmd_revoke()` with RFC 7009 revocation calls
- `cmd_status()` with live introspection and API probe fallback
- `cmd_refresh()` force-refresh command
- Unit tests for all refresh and revocation edge cases

### Phase 4: CLI Surface + Tracing (Days 9–10)

- `cmd_auth()`, `cmd_list()`, `cmd_revoke()`, `cmd_status()`, `cmd_export()` wired into `controller.py`
- `tag mcp auth list` table + JSON rendering
- PRD-013 span instrumentation for all OAuth lifecycle steps
- `notifications.py` integration for proactive refresh warnings
- `cmd_export()` with secret scanning gate (PRD-034)
- End-to-end integration tests for all six known server profiles

### Phase 5: Review + Hardening (Days 11)

- Security audit: grep `mcp_auth.py` for any token value exposure paths
- Performance benchmarks: `bench_list_20_accounts`, `bench_get_token_cache_hit`
- Cross-platform keychain testing: macOS Keychain, Linux Secret Service (Docker), Windows Credential Manager (CI runner)
- `pytest --cov=tag.mcp_auth --cov-fail-under=90`
- Address open questions OQ-01 through OQ-04; update docstrings
- Merge to `main`

---

*End of PRD-074*
