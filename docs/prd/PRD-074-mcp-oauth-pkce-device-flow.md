# PRD-074: MCP OAuth 2.1 with PKCE + Device Authorization Flow (`tag mcp auth`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `internal/mcp/oauth`
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (secret scanning), PRD-014 (MCP server registry)
**Inspired by:** MCP OAuth 2.1 spec (2025), Composio auth, Arcade AI auth
**GitHub Issue:** #346

---

## 1. Overview

MCP servers increasingly require authenticated access to act on behalf of users — GitHub to open pull requests, Notion to read/write databases, Google Workspace to manage calendar events, Slack to post messages, Stripe to list transactions. The MCP OAuth 2.1 specification (ratified 2025) standardises how MCP clients and servers negotiate these credentials using a five-step discovery chain that culminates in either a PKCE-protected authorisation code flow (for interactive browser sessions) or a device authorisation flow (RFC 8628, for headless CLI environments). TAG currently has no first-class support for this authentication layer, which means users must manually paste tokens into environment variables, rotating them by hand and storing them in plaintext config files.

This PRD specifies `tag mcp auth`: a complete OAuth 2.1 credential lifecycle manager for MCP servers. The system auto-detects whether the host environment has a browser available and selects the appropriate flow — PKCE authorisation code when `DISPLAY` or `BROWSER` is set; device flow when running over SSH or in a headless CI container. Tokens are never stored in plaintext configuration files; they live exclusively in the OS keychain via `github.com/zalando/go-keyring` (Keychain on macOS, Secret Service on Linux, Credential Manager on Windows). Token refresh happens transparently before expiry. Per-server token isolation ensures that a token issued for `api.notion.com` is never forwarded to `api.github.com`, satisfying the audience-binding requirement introduced in RFC 8707.

The implementation lives in `internal/mcp/oauth`, wired into the `tag mcp` subcommand family in `internal/cli`. It covers the full OAuth lifecycle: discovery, registration (dynamic client registration per RFC 7591), authorisation, token exchange, secure storage, background refresh, status inspection, revocation, and listing of all connected accounts. The design draws heavily on three production patterns: the MCP spec's Protected Resource Metadata (PRM) discovery chain for server-agnostic endpoint discovery; Composio's entity-scoped brokered credential model where the LLM context window never touches tokens; and Arcade AI's Human-in-the-Loop (HITL) consent gate where destructive scopes require explicit user approval before tool execution unblocks.

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
| G1 | Implement the full MCP OAuth 2.1 five-step discovery chain: 401 response → PRM endpoint → AS metadata (RFC 8414) → dynamic client registration (RFC 7591) → PKCE authorisation code flow or device flow. Leverage `github.com/modelcontextprotocol/go-sdk` v1.6.1 Auth-Code+PKCE support where available; supplement with `golang.org/x/oauth2` for device flow. |
| G2 | Auto-detect headless environments via `DISPLAY`, `SSH_CLIENT`, `SSH_TTY`, and `CI` environment variables; automatically select device flow in headless contexts and PKCE flow in interactive contexts, with `--flow` override. |
| G3 | Store all tokens exclusively in the OS keychain via `github.com/zalando/go-keyring`; never write tokens, client secrets, or refresh tokens to disk in plaintext. |
| G4 | Implement transparent background token refresh: proactively refresh when the token expiry is within 5 minutes; re-trigger interactive authorisation when refresh token is expired or revoked. |
| G5 | Enforce audience-binding (RFC 8707): every authorisation request and token exchange request MUST include the `resource` parameter set to the MCP server's canonical URI; tokens MUST NOT be reused across servers. |
| G6 | Provide first-class pre-configured profiles for GitHub, Notion, Google Workspace (Gmail, Drive, Calendar), Slack, Stripe, and Linear — covering well-known AS endpoints, scope sets, and redirect URI requirements. |
| G7 | `tag mcp auth list` provides a health dashboard of all connected accounts: server name, scopes, expiry time, and refresh status. |
| G8 | `tag mcp auth revoke <server>` performs server-side token revocation (RFC 7009) and removes keychain entries. |
| G9 | Implement the HITL consent gate for destructive scopes (write, delete, admin): display the requested scopes and require explicit `y/N` confirmation before initiating the authorisation flow. |
| G10 | Emit OAuth lifecycle events as TAG trace spans (PRD-013) via `go.opentelemetry.io/otel`: discovery, registration, authorisation, token exchange, refresh, revocation — each with server name, duration, and success/failure. |
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
| Token refresh transparency | Zero 401 errors during a 4-hour agent loop when refresh token is valid | Long-running integration test with mock AS via `httptest.NewServer` |
| Revocation completeness | `tag mcp auth revoke` removes both keychain entry and server-side token in 100% of test cases | Integration test with token introspection post-revoke |
| `tag mcp auth list` latency | Dashboard renders in < 200 ms for up to 20 connected accounts | Go benchmark with 20 mock `modernc.org/sqlite` rows |
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
| FR-01 | `internal/mcp/oauth` MUST implement the five-step MCP OAuth 2.1 discovery chain: parse `WWW-Authenticate` header from a 401 response to locate the PRM URL; fetch PRM (`application/json` at `/.well-known/oauth-protected-resource`); follow `authorization_servers[0]` to AS metadata (RFC 8414); perform dynamic client registration (RFC 7591) if `registration_endpoint` is present; initiate PKCE or device flow. The Auth-Code+PKCE flow is consumed via the go-sdk's built-in auth support (`github.com/modelcontextprotocol/go-sdk` v1.6.1, stabilised in v1.5.0); device flow supplements with `golang.org/x/oauth2`. | P0 |
| FR-02 | PKCE flow MUST use `code_challenge_method=S256`; `code_verifier` MUST be a cryptographically random 43–128 character string produced by `crypto/rand` and base64url-encoded without padding; `code_challenge = base64url(sha256(code_verifier))` computed with `crypto/sha256`. | P0 |
| FR-03 | Every authorisation request and token request MUST include `resource=<mcp_server_uri>` (RFC 8707). Requests without this parameter MUST be rejected by the client before sending. | P0 |
| FR-04 | The `IsHeadless()` function in `internal/mcp/oauth` MUST return `true` when none of `DISPLAY`, `WAYLAND_DISPLAY`, `BROWSER` are set via `os.Getenv`, OR when `SSH_CLIENT` or `SSH_TTY` is set, OR when `CI` is set. | P0 |
| FR-05 | When `IsHeadless()` returns `true` and the AS metadata contains `device_authorization_endpoint`, the system MUST automatically use device flow. When headless but no `device_authorization_endpoint` exists, the system MUST return an error wrapping `ErrNoDeviceFlow` with a human-readable message. | P0 |
| FR-06 | Token storage MUST use `keyring.Set(service, "access_token", token)` from `github.com/zalando/go-keyring` for access tokens and `keyring.Set(service, "refresh_token", token)` for refresh tokens, where `service = "tag-mcp-{server_name}"`. Token values MUST NOT appear in any log output, trace attribute, or SQLite row. | P0 |
| FR-07 | Token metadata (expiry timestamp, scopes, server URI, client_id, flow_type) MUST be stored in the `mcp_auth_accounts` SQLite table via `internal/store` (see Section 9.2). The table MUST contain only keychain lookup keys, not token values. Migrations are applied via `internal/store`'s migration runner on first use. | P0 |
| FR-08 | Before initiating any authorisation flow that includes a scope in the `DestructiveScopeGate` set (see Section 9.4), the system MUST display the scope name, a human-readable description of what it permits, and prompt `[y/N]`. The flow MUST NOT proceed if the user responds `N` or does not respond. This gate is skippable only with `--yes` in non-interactive mode. | P1 |
| FR-09 | The PKCE callback server MUST bind to `127.0.0.1:<port>` (default 9753) using a `net/http.Server`, generate a random `state` parameter via `crypto/rand`, verify `state` on callback receipt, and call `Server.Shutdown(ctx)` within 5 seconds of receiving the callback regardless of success or failure. | P0 |
| FR-10 | Device flow polling MUST start at the `interval` returned by the device authorisation endpoint (default 5 s), apply exponential backoff by adding 5 s on each `slow_down` error response, and stop after `expires_in` seconds or `--timeout` seconds, whichever is shorter. Implemented via a goroutine with `time.NewTicker` respecting a `context.WithTimeout`. | P0 |
| FR-11 | The background refresh function MUST proactively refresh tokens when `time.Until(expiresAt) < 5*time.Minute`. It MUST be invoked lazily from `GetToken()` (not as a persistent background goroutine), using `golang.org/x/sync/errgroup` if concurrent operations are needed. | P1 |
| FR-12 | When a refresh token exchange returns `invalid_grant`, the system MUST delete the keychain entry via `go-keyring`, update `mcp_auth_accounts.status = 'expired'` in SQLite, and return a wrapped `ErrTokenExpired` with a message directing the user to run `tag mcp auth <server>` again. | P1 |
| FR-13 | `tag mcp auth revoke <server>` MUST POST to the `revocation_endpoint` from AS metadata with `token=<access_token>` and `token_type_hint=access_token` via `net/http`, then repeat with the refresh token. Both requests MUST use `client_id` in the body (not Basic auth for public clients). | P1 |
| FR-14 | `tag mcp auth list` MUST complete in < 200 ms for up to 20 accounts by reading only from the `mcp_auth_accounts` table via `internal/store` without making any network calls. | P1 |
| FR-15 | `tag mcp auth status <server>` with `--introspect` MUST POST to `introspection_endpoint` (RFC 7662) if available; otherwise MUST make a lightweight authenticated GET to a well-known API endpoint (e.g., `https://api.github.com/user` for GitHub) via `net/http` and interpret 200 as valid, 401 as expired. | P2 |
| FR-16 | All six pre-configured server profiles (github, notion, google/calendar, google/drive, google/gmail, slack, stripe) MUST be declared in the `KnownServers map[string]KnownServerProfile` variable in `internal/mcp/oauth` with: `ASMetadataURL`, `DefaultScopes`, `ResourceURI`, and `RedirectURIRequired` fields. | P1 |
| FR-17 | `tag mcp auth <unknown_server> --as-url <url> --resource <uri>` MUST work for arbitrary RFC 8414-compliant authorisation servers. | P2 |
| FR-18 | Every OAuth lifecycle event (discovery, registration, authorisation, token exchange, refresh, revocation) MUST be recorded as a child span under the active TAG trace via `go.opentelemetry.io/otel/trace`, with attributes: `mcp.server_name`, `mcp.flow_type`, `mcp.scope`, `oauth.step`, `http.status_code`, `duration_ms`. | P2 |
| FR-19 | `tag mcp auth export` MUST write only metadata (server name, scopes, expiry, flow type) to the output file. It MUST return `ErrExportSecurity` if any field value matches the secret scanning patterns from PRD-034. | P2 |
| FR-20 | Dynamic client registrations MUST be stored in `mcp_auth_registrations` table via `internal/store` and reused across sessions for the same `(server_name, as_url)` pair. Re-registration MUST only occur when the existing `client_id` is rejected with an `invalid_client` error. | P2 |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Discovery chain latency (401 → token obtained) | ≤ 2 s net I/O; user-perceived time dominated by browser or device flow consent |
| NFR-02 | Local PKCE callback server startup | ≤ 100 ms from `tag mcp auth` invocation to `http://localhost:PORT/callback` accepting connections |
| NFR-03 | `tag mcp auth list` cold render | ≤ 200 ms for 20 accounts (`internal/store` SQLite read only, no network) |
| NFR-04 | Token refresh latency | ≤ 500 ms for a successful refresh token exchange (single HTTPS POST via `net/http`) |
| NFR-05 | Keychain operation latency | ≤ 50 ms per `keyring.Get` / `keyring.Set` on macOS Keychain |
| NFR-06 | Goroutine footprint | `internal/mcp/oauth` creates goroutines only during active device flow polling and PKCE callback listening; no persistent background goroutines at idle |
| NFR-07 | Dependency footprint | New Go module dependencies limited to: `github.com/zalando/go-keyring` (keychain); `golang.org/x/oauth2` (device flow raw client); `crypto/rand`, `crypto/sha256`, `encoding/base64`, `net/http` are Go standard library — no CGO required |
| NFR-08 | Cross-platform keychain support | macOS (Keychain), Linux (Secret Service via D-Bus — `go-keyring` handles transparently), Windows (Credential Manager) — all via `github.com/zalando/go-keyring` |
| NFR-09 | Test coverage | `internal/mcp/oauth` MUST have ≥ 90% line coverage under `go test`; all network calls MUST be mockable via `net/http/httptest.NewServer`; the keychain MUST be injectable via a `Keyring` interface |
| NFR-10 | No token values in logs | TAG's logger at `DEBUG` MUST NOT emit token values; `internal/mcp/oauth` MUST mask tokens via `internal/credentials.MaskSecret()` before any log call involving token strings |
| NFR-11 | Graceful keychain absence | If `go-keyring` returns a no-keychain-available error (headless Linux without D-Bus/Secret Service), the system MUST fail with a clear error — never fall back to plaintext file storage |
| NFR-12 | Concurrent auth safety | Concurrent `tag mcp auth` calls for the same server MUST use `gofrs/flock` advisory lock plus `modernc.org/sqlite` WAL mode (via `internal/store`) to prevent duplicate registrations |

---

## 10. Technical Design

> **Go SDK OAuth gaps are CLOSED.** Older TAG documentation flagged MCP client-side OAuth as unimplemented. As of `github.com/modelcontextprotocol/go-sdk` v1.6.1 (GA), Auth-Code+PKCE is stabilised (v1.5.0), Client-Credentials is available (v1.6.0), Enterprise Managed Auth is supported, and sampling-with-tools has shipped since v1.4.0. `internal/mcp/oauth` consumes the SDK's built-in auth middleware for PKCE Auth-Code flows and supplements with `golang.org/x/oauth2` for the Device Authorization Grant (RFC 8628). The loopback callback listener is a plain `net/http.Server` on `127.0.0.1`. PKCE crypto uses only Go standard library (`crypto/rand`, `crypto/sha256`, `encoding/base64`).

### 10.1 New Files

| File | Purpose |
|------|---------|
| `internal/mcp/oauth/oauth.go` | Primary implementation: discovery, flows, token store, refresh, revocation |
| `internal/mcp/oauth/types.go` | Go struct definitions: `ASMetadata`, `PRMDocument`, `PKCEParams`, `TokenResponse`, `ConnectedAccount`, `KnownServerProfile` |
| `internal/mcp/oauth/known_servers.go` | `KnownServers` map and `DestructiveScopeGate` set |
| `internal/mcp/oauth/callback.go` | PKCE loopback `net/http` callback server |
| `internal/mcp/oauth/headless.go` | `IsHeadless()` environment detection |
| `internal/mcp/oauth/oauth_test.go` | Unit and integration tests with `httptest.NewServer` mock AS |
| `internal/mcp/oauth/testdata/as_metadata_github.json` | Fixture AS metadata for GitHub mock |
| `internal/mcp/oauth/testdata/as_metadata_notion.json` | Fixture AS metadata for Notion mock |
| `internal/store/migrations/0020_mcp_auth.sql` | SQLite DDL migration for auth tables |
| `internal/cli/mcp_auth.go` | Cobra subcommands: `tag mcp auth`, `list`, `status`, `revoke`, `refresh`, `export` |

### 10.2 SQLite DDL

```sql
-- Migration: 0020_mcp_auth.sql
-- Applied via internal/store migration runner on first use.

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

### 10.3 Core Go Types

```go
// internal/mcp/oauth/types.go
package oauth

import (
    "crypto/rand"
    "crypto/sha256"
    "encoding/base64"
    "fmt"
    "time"
)

// MCPProtocolVersion is the single pinned MCP protocol version for all connections.
// Update only on a scheduled version-bump milestone; never inline elsewhere.
const MCPProtocolVersion = "2025-11-25"

// FlowType identifies which OAuth grant was used.
type FlowType string

const (
    FlowPKCE   FlowType = "pkce"
    FlowDevice FlowType = "device"
)

// TokenStatus represents the health state of a stored token.
type TokenStatus string

const (
    StatusValid         TokenStatus = "valid"
    StatusExpired       TokenStatus = "expired"
    StatusRevoked       TokenStatus = "revoked"
    StatusRefreshFailed TokenStatus = "refresh_failed"
)

// ASMetadata holds RFC 8414 Authorization Server Metadata.
type ASMetadata struct {
    Issuer                        string   `json:"issuer"`
    AuthorizationEndpoint         string   `json:"authorization_endpoint"`
    TokenEndpoint                 string   `json:"token_endpoint"`
    RegistrationEndpoint          string   `json:"registration_endpoint,omitempty"`
    RevocationEndpoint            string   `json:"revocation_endpoint,omitempty"`
    IntrospectionEndpoint         string   `json:"introspection_endpoint,omitempty"`
    DeviceAuthorizationEndpoint   string   `json:"device_authorization_endpoint,omitempty"`
    ScopesSupported               []string `json:"scopes_supported,omitempty"`
    CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported,omitempty"`
    GrantTypesSupported           []string `json:"grant_types_supported,omitempty"`
}

// PRMDocument is the MCP Protected Resource Metadata document.
type PRMDocument struct {
    Resource               string   `json:"resource"`
    AuthorizationServers   []string `json:"authorization_servers"`
    BearerMethodsSupported []string `json:"bearer_methods_supported"`
    ScopesSupported        []string `json:"scopes_supported,omitempty"`
}

// DynamicClientRegistration holds the result of RFC 7591 dynamic client registration.
// ClientSecret is zero-string for public clients; when present it is immediately
// moved to the OS keychain — never retained in this struct beyond the registration call.
type DynamicClientRegistration struct {
    ClientID              string   `json:"client_id"`
    ClientSecret          string   `json:"client_secret,omitempty"` // moved to keychain immediately
    RedirectURIs          []string `json:"redirect_uris"`
    GrantTypes            []string `json:"grant_types"`
    RegistrationClientURI string   `json:"registration_client_uri,omitempty"`
}

// PKCEParams holds S256 PKCE challenge parameters generated for a single auth request.
type PKCEParams struct {
    CodeVerifier        string // 96-char base64url string, crypto/rand
    CodeChallenge       string // base64url(sha256(CodeVerifier))
    CodeChallengeMethod string // always "S256"
    State               string // 256-bit random per request, for CSRF protection
}

// GeneratePKCEParams produces a fresh set of PKCE + state values.
// Uses crypto/rand for the verifier bytes and crypto/sha256 for the challenge.
func GeneratePKCEParams() (PKCEParams, error) {
    vb := make([]byte, 72) // 72 raw bytes → 96 base64url chars (within RFC 43–128 range)
    if _, err := rand.Read(vb); err != nil {
        return PKCEParams{}, fmt.Errorf("pkce verifier: %w", err)
    }
    verifier := base64.RawURLEncoding.EncodeToString(vb)

    sum := sha256.Sum256([]byte(verifier))
    challenge := base64.RawURLEncoding.EncodeToString(sum[:])

    sb := make([]byte, 32)
    if _, err := rand.Read(sb); err != nil {
        return PKCEParams{}, fmt.Errorf("pkce state: %w", err)
    }
    return PKCEParams{
        CodeVerifier:        verifier,
        CodeChallenge:       challenge,
        CodeChallengeMethod: "S256",
        State:               base64.RawURLEncoding.EncodeToString(sb),
    }, nil
}

// DeviceAuthResponse represents an RFC 8628 device authorisation response.
type DeviceAuthResponse struct {
    DeviceCode              string `json:"device_code"`
    UserCode                string `json:"user_code"`
    VerificationURI         string `json:"verification_uri"`
    VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
    ExpiresIn               int    `json:"expires_in"`
    Interval                int    `json:"interval"` // default 5 s
}

// TokenResponse represents an OAuth token exchange response.
// This struct is ephemeral: token values are moved to the keychain immediately after
// exchange and this struct is never persisted to SQLite or logs.
type TokenResponse struct {
    AccessToken  string `json:"access_token"`           // moved to keychain immediately
    TokenType    string `json:"token_type"`
    ExpiresIn    int    `json:"expires_in,omitempty"`
    RefreshToken string `json:"refresh_token,omitempty"` // moved to keychain immediately
    Scope        string `json:"scope,omitempty"`
    Resource     string `json:"resource,omitempty"`     // RFC 8707 audience echo
}

// ConnectedAccount is the hydrated view of an mcp_auth_accounts row (no token values).
type ConnectedAccount struct {
    ServerName      string      `json:"server"`
    ResourceURI     string      `json:"resource_uri"`
    ASURL           string      `json:"as_url"`
    ClientID        string      `json:"client_id"`
    FlowType        FlowType    `json:"flow_type"`
    Scopes          []string    `json:"scopes"`
    KeychainService string      `json:"keychain_service"`
    Status          TokenStatus `json:"status"`
    ExpiresAt       *time.Time  `json:"expires_at,omitempty"`
    RefreshExpiresAt *time.Time `json:"refresh_expires_at,omitempty"`
    ProfileName     string      `json:"profile_name,omitempty"`
    OrgHint         string      `json:"org_hint,omitempty"`
    TTLSeconds      int         `json:"ttl_seconds,omitempty"`
    HasRefreshToken bool        `json:"has_refresh_token"`
}

// KnownServerProfile is the pre-configured profile for a well-known MCP server.
type KnownServerProfile struct {
    Name                string
    ResourceURI         string            // RFC 8707 audience
    ASMetadataURL       string            // RFC 8414 discovery URL
    DefaultScopes       []string
    RedirectURIRequired bool              // false for device-flow-only servers
    DestructiveScopes   []string
    ScopeDescriptions   map[string]string
}
```

### 10.4 Known Server Registry

```go
// internal/mcp/oauth/known_servers.go
package oauth

// KnownServers contains pre-configured profiles for well-known MCP servers.
// Keys are canonical slugs ("github", "notion", "google/calendar", etc.).
var KnownServers = map[string]KnownServerProfile{
    "github": {
        Name:                "github",
        ResourceURI:         "https://github.com",
        ASMetadataURL:       "https://github.com/.well-known/oauth-authorization-server",
        DefaultScopes:       []string{"repo", "issues:write"},
        RedirectURIRequired: true,
        DestructiveScopes:   []string{"repo:delete", "admin:org", "delete_repo", "write:packages"},
        ScopeDescriptions: map[string]string{
            "repo":        "Full read/write access to repositories",
            "admin:org":   "Full admin access to org settings",
            "delete_repo": "Allows deleting repositories",
        },
    },
    "notion": {
        Name:                "notion",
        ResourceURI:         "https://api.notion.com",
        ASMetadataURL:       "https://api.notion.com/.well-known/oauth-authorization-server",
        DefaultScopes:       []string{"read"},
        RedirectURIRequired: true,
        DestructiveScopes:   []string{"write", "delete"},
        ScopeDescriptions: map[string]string{
            "write":  "Create and update pages and blocks",
            "delete": "Delete pages and databases",
        },
    },
    "google/calendar": {
        Name:                "google/calendar",
        ResourceURI:         "https://www.googleapis.com/auth/calendar",
        ASMetadataURL:       "https://accounts.google.com/.well-known/openid-configuration",
        DefaultScopes:       []string{"https://www.googleapis.com/auth/calendar.readonly"},
        RedirectURIRequired: true,
        DestructiveScopes:   []string{"https://www.googleapis.com/auth/calendar"},
        ScopeDescriptions: map[string]string{
            "https://www.googleapis.com/auth/calendar": "Full read/write access to calendars and events",
        },
    },
    "google/drive": {
        Name:                "google/drive",
        ResourceURI:         "https://www.googleapis.com/auth/drive",
        ASMetadataURL:       "https://accounts.google.com/.well-known/openid-configuration",
        DefaultScopes:       []string{"https://www.googleapis.com/auth/drive.readonly"},
        RedirectURIRequired: true,
        DestructiveScopes:   []string{"https://www.googleapis.com/auth/drive"},
        ScopeDescriptions: map[string]string{
            "https://www.googleapis.com/auth/drive": "Full read/write/delete access to Drive files",
        },
    },
    "google/gmail": {
        Name:                "google/gmail",
        ResourceURI:         "https://www.googleapis.com/auth/gmail",
        ASMetadataURL:       "https://accounts.google.com/.well-known/openid-configuration",
        DefaultScopes:       []string{"https://www.googleapis.com/auth/gmail.readonly"},
        RedirectURIRequired: true,
        DestructiveScopes: []string{
            "https://www.googleapis.com/auth/gmail.modify",
            "https://www.googleapis.com/auth/gmail.send",
        },
        ScopeDescriptions: map[string]string{
            "https://www.googleapis.com/auth/gmail.send":   "Send email on your behalf",
            "https://www.googleapis.com/auth/gmail.modify": "Read, compose, send, and delete email",
        },
    },
    "slack": {
        Name:                "slack",
        ResourceURI:         "https://slack.com/api",
        ASMetadataURL:       "https://slack.com/.well-known/openid-configuration",
        DefaultScopes:       []string{"channels:read", "chat:write"},
        RedirectURIRequired: true,
        DestructiveScopes:   []string{"chat:write", "files:write", "admin"},
        ScopeDescriptions: map[string]string{
            "chat:write": "Post messages in channels",
            "admin":      "Administer the workspace",
        },
    },
    "stripe": {
        Name:                "stripe",
        ResourceURI:         "https://api.stripe.com",
        ASMetadataURL:       "https://connect.stripe.com/.well-known/openid-configuration",
        DefaultScopes:       []string{"read_only"},
        RedirectURIRequired: true,
        DestructiveScopes:   []string{"read_write"},
        ScopeDescriptions: map[string]string{
            "read_write": "Create, update, and delete Stripe objects including charges and refunds",
        },
    },
    "linear": {
        Name:                "linear",
        ResourceURI:         "https://api.linear.app",
        ASMetadataURL:       "https://api.linear.app/.well-known/oauth-authorization-server",
        DefaultScopes:       []string{"read"},
        RedirectURIRequired: true,
        DestructiveScopes:   []string{"write", "issues:archive"},
        ScopeDescriptions: map[string]string{
            "write":          "Create and update issues, cycles, and projects",
            "issues:archive": "Archive and delete issues",
        },
    },
}

// DestructiveScopeGate is the union of all destructive scopes across all known server profiles.
var DestructiveScopeGate = func() map[string]bool {
    m := make(map[string]bool)
    for _, p := range KnownServers {
        for _, s := range p.DestructiveScopes {
            m[s] = true
        }
    }
    return m
}()
```

### 10.5 Core Algorithm: Discovery Chain

The go-sdk's auth middleware handles the PKCE Auth-Code discovery chain automatically when `internal/mcp` creates a client connection. `DiscoverAuthServer` is exposed separately for the CLI registration path and the device flow, where raw control is needed.

```go
// internal/mcp/oauth/oauth.go (illustrative)
package oauth

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "strings"
)

// DiscoverAuthServer implements the five-step MCP OAuth 2.1 discovery chain.
//
// Step 1: Probe the MCP server root; expect 401 with WWW-Authenticate header.
// Step 2: Parse WWW-Authenticate for the resource_metadata URL.
// Step 3: GET PRM URL → PRMDocument (authorization_servers[0]).
// Step 4: GET {as_issuer}/.well-known/oauth-authorization-server → ASMetadata (RFC 8414).
// Step 5: Return (PRMDocument, ASMetadata) for the caller to proceed with registration.
func DiscoverAuthServer(ctx context.Context, mcpServerURI string, hc *http.Client) (*PRMDocument, *ASMetadata, error) {
    // Step 1: Probe — expect 401.
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, mcpServerURI, nil)
    if err != nil {
        return nil, nil, err
    }
    resp, err := hc.Do(req)
    if err != nil {
        return nil, nil, fmt.Errorf("mcp probe: %w", err)
    }
    resp.Body.Close()
    if resp.StatusCode != http.StatusUnauthorized {
        return nil, nil, fmt.Errorf("%w: expected 401, got %d", ErrDiscovery, resp.StatusCode)
    }

    wwwAuth := resp.Header.Get("WWW-Authenticate")
    if wwwAuth == "" {
        return nil, nil, fmt.Errorf("%w: 401 missing WWW-Authenticate header", ErrDiscovery)
    }

    // Step 2: Extract resource_metadata URL.
    prmURL, err := parseResourceMetadataURL(wwwAuth)
    if err != nil {
        return nil, nil, err
    }

    // Step 3: Fetch PRM document.
    prm, err := fetchJSON[PRMDocument](ctx, hc, prmURL)
    if err != nil {
        return nil, nil, fmt.Errorf("prm fetch: %w", err)
    }
    if len(prm.AuthorizationServers) == 0 {
        return nil, nil, fmt.Errorf("%w: PRM has empty authorization_servers", ErrDiscovery)
    }

    // Step 4: Fetch AS metadata (RFC 8414).
    asIssuer := strings.TrimRight(prm.AuthorizationServers[0], "/")
    asMeta, err := fetchJSON[ASMetadata](ctx, hc, asIssuer+"/.well-known/oauth-authorization-server")
    if err != nil {
        return nil, nil, fmt.Errorf("as metadata fetch: %w", err)
    }

    return prm, asMeta, nil
}
```

### 10.6 Core Algorithm: Headless Detection

```go
// internal/mcp/oauth/headless.go
package oauth

import (
    "os"
    "runtime"
)

// IsHeadless returns true when no browser-capable display is available,
// meaning the device flow should be selected over the PKCE redirect flow.
func IsHeadless() bool {
    // Explicit environment overrides.
    if os.Getenv("TAG_MCP_FORCE_DEVICE_FLOW") == "1" {
        return true
    }
    if os.Getenv("TAG_MCP_FORCE_PKCE_FLOW") == "1" {
        return false
    }

    // SSH session indicators always mean headless.
    if os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" {
        return true
    }

    // CI environment indicators.
    if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
        return true
    }

    // Linux/BSD: check for X11 or Wayland display.
    if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
        return false
    }

    // macOS Aqua sessions always have a display unless we are in an SSH session
    // (already handled above).
    if runtime.GOOS == "darwin" {
        return false
    }

    // Windows / WSL with a browser configured.
    if os.Getenv("BROWSER") != "" {
        return false
    }

    // Default: headless if no display evidence found.
    return true
}
```

### 10.7 Core Algorithm: PKCE Local Callback Server

```go
// internal/mcp/oauth/callback.go
package oauth

import (
    "context"
    "errors"
    "fmt"
    "net/http"
    "net/url"
    "time"
)

// RunPKCECallbackServer starts a net/http.Server bound to 127.0.0.1:<port>,
// waits for the OAuth redirect callback, validates the state parameter,
// and returns the callback query values. The server shuts down via
// Server.Shutdown within 5 seconds of receiving the callback (success or failure).
func RunPKCECallbackServer(ctx context.Context, port int, expectedState string) (url.Values, error) {
    resultCh := make(chan url.Values, 1)
    errCh    := make(chan error, 1)

    mux := http.NewServeMux()
    mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
        q := r.URL.Query()
        if q.Get("state") != expectedState {
            http.Error(w, "State mismatch. Possible CSRF. Close this window.", http.StatusBadRequest)
            errCh <- ErrStateMismatch
            return
        }
        if errCode := q.Get("error"); errCode != "" {
            http.Error(w, "Authorization failed: "+errCode+". Close this window.", http.StatusBadRequest)
            errCh <- fmt.Errorf("oauth error: %s %s", errCode, q.Get("error_description"))
            return
        }
        fmt.Fprint(w, "Authorized! You may close this window and return to TAG.")
        resultCh <- q
    })

    srv := &http.Server{
        Addr:    fmt.Sprintf("127.0.0.1:%d", port),
        Handler: mux,
    }

    go func() {
        if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            errCh <- err
        }
    }()

    shutdown := func() {
        shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        srv.Shutdown(shutCtx) //nolint:errcheck
    }

    select {
    case vals := <-resultCh:
        shutdown()
        return vals, nil
    case err := <-errCh:
        shutdown()
        return nil, err
    case <-ctx.Done():
        shutdown()
        return nil, fmt.Errorf("PKCE callback timed out: %w", ctx.Err())
    }
}
```

### 10.8 Integration Points

| Component | Integration |
|-----------|-------------|
| `internal/cli/mcp_auth.go` | Registers `tag mcp auth` cobra subcommand family; calls `oauth.CmdAuth()`, `CmdList()`, `CmdStatus()`, `CmdRevoke()`, `CmdRefresh()` |
| `internal/store` | DB handle and `0020_mcp_auth.sql` migration runner; all SQLite reads/writes go through typed query helpers here |
| `internal/credentials` | `MaskSecret(token)` used before any log call involving token strings; `ScanForSecrets()` called in `CmdExport()` |
| `internal/obs` | `obs.StartSpan(ctx, "mcp.auth.<step>")` wraps each OAuth lifecycle step; span attributes follow PRD-013 conventions via `go.opentelemetry.io/otel` |
| `internal/tui` | Notification path for "Token for {server} expiring in 10 minutes" when proactive refresh is triggered |
| `internal/mcp` (client layer) | `oauth.GetToken(ctx, db, serverName, profileName)` called by the MCP transport before each tool call; injects the returned Bearer token as an HTTP header — never as a tool argument or system prompt |

### 10.9 `GetToken()` — Primary Consumer API

```go
// internal/mcp/oauth/oauth.go

// GetToken is the primary API consumed by the internal/mcp client transport layer.
// It returns a valid Bearer token string for the named server, performing a lazy
// proactive refresh when within 5 minutes of expiry.
// Token values are fetched exclusively from the OS keychain via go-keyring; never from SQLite.
func GetToken(ctx context.Context, db *store.DB, serverName, profileName string) (string, error) {
    row, err := db.GetMCPAuthAccount(ctx, serverName, profileName)
    if errors.Is(err, store.ErrNotFound) {
        return "", fmt.Errorf("%w: run: tag mcp auth %s", ErrNoToken, serverName)
    }
    if err != nil {
        return "", err
    }

    if row.Status == StatusRevoked {
        return "", fmt.Errorf("%w: run: tag mcp auth %s", ErrTokenRevoked, serverName)
    }

    // Proactive refresh if within 5 minutes of expiry.
    if row.ExpiresAt != nil && time.Until(*row.ExpiresAt) < 5*time.Minute {
        if err := refreshTokenSync(ctx, db, serverName, row); err != nil {
            return "", err
        }
        row, err = db.GetMCPAuthAccount(ctx, serverName, profileName)
        if err != nil {
            return "", err
        }
    }

    if row.Status == StatusExpired {
        return "", fmt.Errorf("%w: run: tag mcp auth %s", ErrTokenExpired, serverName)
    }

    // Retrieve from OS keychain only.
    token, err := keyring.Get(row.KeychainService, "access_token")
    if err != nil || token == "" {
        return "", fmt.Errorf("%w: keychain entry missing for %s: run: tag mcp auth %s",
            ErrNoToken, serverName, serverName)
    }
    return token, nil
}
```

---

## 11. Security Considerations

1. **Token isolation per server:** Every token is audience-bound to a specific `resource` URI (RFC 8707). `GetToken()` returns only the token for the requested server; there is no API to enumerate all tokens in a single call. Cross-server token reuse is architecturally impossible because each token lives under a distinct keychain service key (`tag-mcp-{server}`).

2. **No plaintext token storage:** `github.com/zalando/go-keyring` is the only write path for token values. `internal/mcp/oauth` MUST be audited to ensure that no code path writes a token string to: SQLite, any file under `~/.tag/`, any log sink, or any environment variable. `TokenResponse` is ephemeral and never persisted; only the `keyring.Set` call persists token values.

3. **PKCE state parameter CSRF protection:** The `state` parameter is a 256-bit random value produced by `crypto/rand` for each authorisation request. `RunPKCECallbackServer` verifies state equality before accepting any `code` parameter. A mismatch immediately closes the callback server and returns `ErrStateMismatch` without exchanging the code.

4. **Local callback server binding:** The PKCE callback server MUST bind only to `127.0.0.1`, never `0.0.0.0`. The `net/http.Server.Addr` field is set to `fmt.Sprintf("127.0.0.1:%d", port)`. This prevents remote machines on the same network from intercepting the OAuth callback.

5. **Dynamic client registration security:** `client_secret` values from dynamic registration are stored in the keychain under `tag-mcp-reg-{server_name} / client_secret` via `go-keyring`. They are never stored in SQLite, never logged, and never included in `tag mcp auth export` output.

6. **Destructive scope HITL gate:** Any scope in `DestructiveScopeGate` triggers an explicit `[y/N]` prompt before the authorisation URL is opened. This gate cannot be bypassed programmatically except via `--yes` (intended for automated tests with `httptest.NewServer` mock AS endpoints).

7. **Device code expiry enforcement:** The device flow goroutine MUST check `ExpiresIn` from the device authorisation response and abort when the deadline passes via `context.WithTimeout`. Expired device codes MUST NOT be reused.

8. **Refresh token rotation:** When an AS issues a new refresh token on each refresh (RFC 6749 §10.4), the old refresh token MUST be immediately deleted from the keychain via `keyring.Delete` and replaced with the new one.

9. **Secret scanning on export:** `CmdExport()` calls `internal/credentials.ScanForSecrets()` (PRD-034) on the output buffer before writing to disk.

10. **Revocation on `tag profile delete`:** `internal/mcp/oauth` MUST expose `RevokeAll(ctx, profileName string) error` for profile cleanup hooks, preventing orphaned keychain entries.

11. **Audit trail in `mcp_auth_events`:** Every token exchange, refresh, and revocation is recorded with a timestamp and success/failure status — enabling after-the-fact auditing without exposing token values.

12. **No token in LLM context:** The MCP transport in `internal/mcp` MUST inject the Bearer token as an HTTP header at the transport level, never as a tool argument, system prompt field, or message content. The LLM context window MUST never contain a token string.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/mcp/oauth/oauth_test.go`)

Each exported function is tested in isolation. All HTTP calls are mocked via `net/http/httptest.NewServer`; keychain calls are mocked via a `Keyring` interface injected at construction time (the real `go-keyring` is never called in unit tests).

| Test | Verifies |
|------|----------|
| `TestGeneratePKCEParams` | `CodeVerifier` length 43–128; `CodeChallenge == base64url(sha256(verifier))`; unique per call |
| `TestIsHeadlessSSH` | Returns `true` when `SSH_CLIENT` is set via `t.Setenv` |
| `TestIsHeadlessDisplay` | Returns `false` when `DISPLAY` is set and `SSH_CLIENT` is not |
| `TestIsHeadlessCI` | Returns `true` when `CI=true` |
| `TestDiscoveryChainHappyPath` | 401 → PRM → AS metadata parsed correctly with `httptest.NewServer` mocks |
| `TestDiscoveryMissingWWWAuth` | `ErrDiscovery` returned when 401 has no `WWW-Authenticate` |
| `TestDiscoveryNoDeviceEndpointHeadless` | `ErrNoDeviceFlow` returned when headless but AS has no `device_authorization_endpoint` |
| `TestPKCEStateMismatch` | Callback server returns `ErrStateMismatch` on state mismatch |
| `TestPKCECallbackTimeout` | Context cancellation produces a timeout error from `RunPKCECallbackServer` |
| `TestDeviceFlowSlowDownBackoff` | Polling interval increases by 5 s on each `slow_down` response |
| `TestDeviceFlowExpire` | Polling goroutine stops when `context.WithTimeout` deadline passes |
| `TestTokenStoredInKeychainNotSQLite` | After auth, `access_token` absent from all SQLite rows; present in mock keyring |
| `TestGetTokenProactiveRefresh` | `refreshTokenSync` called when `ExpiresAt - now < 5m` |
| `TestGetTokenRefreshFailure` | `ErrTokenExpired` returned when refresh endpoint returns `invalid_grant` |
| `TestRevokeCallsRevocationEndpoint` | `POST /revoke` called with both `access_token` and `refresh_token` via `httptest` |
| `TestDestructiveScopeGateBlocksFlow` | Auth flow not started when user responds `N` to HITL prompt |
| `TestResourceParamInAuthRequest` | `resource=` present in authorisation URL query string |
| `TestResourceParamInTokenRequest` | `resource=` present in token exchange POST body |
| `TestNoTokenInLogs` | Log buffer captured via `slog` handler contains no string matching `[A-Za-z0-9_\-]{20,}` after auth |

### 12.2 Integration Tests

Integration tests run against a mock OAuth 2.1 authorisation server implemented with `httptest.NewServer`. These tests exercise the full end-to-end flow including SQLite writes via `internal/store` with a `t.TempDir()` database.

```go
// internal/mcp/oauth/oauth_test.go (illustrative integration test)
package oauth_test

import (
    "net/http"
    "net/http/httptest"
    "sync"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// inMemKeyring satisfies the Keyring interface using a plain in-memory map.
// Injected into the oauth package via functional options in tests.
type inMemKeyring struct {
    mu    sync.Mutex
    store map[string]string
}

func (k *inMemKeyring) Set(service, user, pass string) error {
    k.mu.Lock(); defer k.mu.Unlock()
    k.store[service+"/"+user] = pass
    return nil
}
func (k *inMemKeyring) Get(service, user string) (string, error) {
    k.mu.Lock(); defer k.mu.Unlock()
    return k.store[service+"/"+user], nil
}
func (k *inMemKeyring) Delete(service, user string) error {
    k.mu.Lock(); defer k.mu.Unlock()
    delete(k.store, service+"/"+user)
    return nil
}

func TestFullPKCEFlowNotion(t *testing.T) {
    kr := &inMemKeyring{store: make(map[string]string)}

    // Stand up mock PRM + AS metadata + token endpoint servers.
    asSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // respond with AS metadata JSON including authorization_endpoint, token_endpoint, etc.
    }))
    defer asSrv.Close()

    // ... additional mock servers for PRM and token exchange ...

    // Run the full PKCE flow with injected mock keyring and httptest servers.
    // Assert: mock keyring has access_token for "tag-mcp-notion";
    //         SQLite row has status='valid';
    //         no token value appears in any SQLite column.
    _ = kr
    _ = asSrv
}
```

### 12.3 Security Tests

| Test | Verifies |
|------|----------|
| `TestNoPlaintextTokensInTagDir` | After full auth flow with `t.TempDir()` as `TAG_HOME`, `filepath.WalkDir` finds no file containing the mock access token string |
| `TestCrossServerTokenIsolation` | `GetToken(ctx, db, "github", "")` returns GitHub token; `GetToken(ctx, db, "notion", "")` returns Notion token; values are distinct |
| `TestCallbackBindsOnlyLoopback` | `net.Listen` in `RunPKCECallbackServer` uses `127.0.0.1:<port>`; verified by inspecting `srv.Addr` |
| `TestExportBlockedOnTokenInMetadata` | `CmdExport()` returns `ErrExportSecurity` when a metadata field contains a token-shaped string |

### 12.4 Performance Tests

| Test | Target | Method |
|------|--------|--------|
| `BenchmarkList20Accounts` | < 200 ms | `testing.B` over 20 pre-seeded `modernc.org/sqlite` rows |
| `BenchmarkGetTokenCacheHit` | < 60 ms | `testing.B` over 100 mock keyring reads |
| `BenchmarkDiscoveryChain` | < 2 s net | Integration test with `httptest.NewServer` as local mock AS |

---

## 13. Acceptance Criteria

| ID | Criterion | How Verified |
|----|-----------|-------------|
| AC-01 | `tag mcp auth notion --scopes read,write` completes the PKCE flow and stores a valid token for Notion in the OS keychain | Integration test + `keyring.Get("tag-mcp-notion", "access_token")` asserts non-empty |
| AC-02 | `tag mcp auth github` with `SSH_CLIENT` set in environment selects device flow automatically without requiring `--flow device` | Unit test with `httptest` mock AS and `t.Setenv("SSH_CLIENT", "user@host")` |
| AC-03 | The authorisation URL for any server always contains `resource=<server_uri>` as a query parameter | URL assertion in unit test for all six known servers |
| AC-04 | The token exchange POST body always contains `resource=<server_uri>` | Request body assertion via `httptest` mock handler |
| AC-05 | No file under `~/.tag/` contains a string matching the access token after `tag mcp auth notion` completes | `filepath.WalkDir` scan in integration test |
| AC-06 | `tag mcp auth list` renders within 200 ms for 20 mock accounts | `testing.B` benchmark |
| AC-07 | `tag mcp auth revoke notion` calls the Notion revocation endpoint for both access token and refresh token, then removes the keychain entry | `httptest` handler assertion + `keyring.Get` returns `""` post-revoke |
| AC-08 | When `expires_at` is 4 minutes in the future, `GetToken("notion", ...)` triggers a refresh token exchange before returning the (new) access token | Unit test with mock token endpoint returning new token |
| AC-09 | When `invalid_grant` is returned during refresh, `GetToken()` returns a wrapped `ErrTokenExpired` with a message containing `tag mcp auth notion` | Unit test |
| AC-10 | `tag mcp auth notion --scopes write` prompts `[y/N]` before opening the browser, because `write` is in `DestructiveScopeGate` | Unit test with mocked stdin returning `N`; asserts no browser open call made |
| AC-11 | `tag mcp auth list --json` returns valid JSON with schema matching `ConnectedAccount` (server, scopes, expires_at, status, has_refresh_token, ttl_seconds) | JSON schema assertion in unit test |
| AC-12 | `tag mcp auth <unknown_server> --as-url https://example.com --resource https://example.com/mcp` completes the full discovery chain against the provided AS URL | Integration test with `httptest.NewServer` at that URL |
| AC-13 | Device flow polling backs off by 5 s on each `slow_down` response | Unit test asserting polling intervals: [5, 10, 15] s after two `slow_down` responses |
| AC-14 | All six known server profiles produce syntactically valid authorisation URLs when PKCE params are applied | Table-driven unit test across all six profiles |
| AC-15 | `tag mcp auth status notion --introspect` makes a POST to `introspection_endpoint` and correctly parses `active: true` | `httptest` handler assertion |
| AC-16 | OAuth lifecycle events appear as child spans in the active TAG trace (PRD-013) | OTel span collector assertion in integration test |
| AC-17 | PKCE callback server shuts down within 5 seconds of receiving the callback, regardless of whether exchange succeeds | Timing assertion in integration test |
| AC-18 | `tag mcp auth list` shows `status: expired` for a token whose `expires_at` is in the past and for which no refresh token is present | Unit test with mocked SQLite row via `internal/store` |

---

## 14. Dependencies

| Dependency | Type | Version | Reason |
|-----------|------|---------|--------|
| `github.com/zalando/go-keyring` | Go module | latest stable | Cross-platform OS keychain access (macOS Keychain, Linux Secret Service via D-Bus, Windows Credential Manager); replaces Python `keyring` + `secretstorage` |
| `github.com/modelcontextprotocol/go-sdk` | Go module | v1.6.1 (GA) | MCP client; ships Auth-Code+PKCE natively (v1.5.0) and Client-Credentials (v1.6.0); pin `MCPProtocolVersion = "2025-11-25"` |
| `golang.org/x/oauth2` | Go module | latest | Device Authorization Grant (RFC 8628) raw client; supplements go-sdk for device flow |
| `crypto/rand` | Go stdlib | — | PKCE `code_verifier` generation; replaces Python `secrets.token_urlsafe` |
| `crypto/sha256` | Go stdlib | — | PKCE S256 `code_challenge`; replaces Python `hashlib.sha256` |
| `encoding/base64` | Go stdlib | — | base64url encoding for PKCE; replaces Python `base64.urlsafe_b64encode` |
| `net/http` | Go stdlib | — | PKCE loopback callback server + all OAuth HTTP calls; replaces Python `httpx` |
| `modernc.org/sqlite` | Go module (via `internal/store`) | latest | Pure-Go SQLite (CGO_ENABLED=0); `mcp_auth_accounts`, `mcp_auth_registrations`, `mcp_auth_events` tables; FTS5 built in; replaces Python `aiosqlite` |
| `github.com/gofrs/flock` | Go module (via `internal/store`) | latest | Cross-platform file lock for concurrent auth safety; replaces Python `fcntl` (which no-ops on Windows) |
| `go.opentelemetry.io/otel` | Go module (via `internal/obs`) | v1.44.x | OAuth lifecycle trace spans; replaces hand-rolled `tracing.py` |
| `github.com/stretchr/testify` | Go module (test only) | latest | Assertion helpers in unit and integration tests; replaces Python `pytest` assert style |
| `net/http/httptest` | Go stdlib (test only) | — | Mock OAuth AS server (`httptest.NewServer`); replaces Python `respx` (httpx mock) |
| PRD-013 (tracing) | Internal PRD | Shipped | Span creation for OAuth lifecycle events |
| PRD-034 (secret scanning) | Internal PRD | Shipped | `internal/credentials.MaskSecret()` and `ScanForSecrets()` used in export |
| PRD-028 (sandbox) | Internal PRD | Shipped | PKCE callback server runs inside sandbox boundary |
| MCP OAuth 2.1 spec | External spec | 2025 | Protocol definition |
| RFC 7636 (PKCE) | External spec | — | Code challenge method S256 |
| RFC 8628 (Device Flow) | External spec | — | Headless authorisation |
| RFC 8414 (AS Metadata) | External spec | — | Discovery endpoint convention |
| RFC 7591 (Dynamic Registration) | External spec | — | Per-server client registration |
| RFC 8707 (Resource Indicators) | External spec | — | Audience binding for tokens |
| RFC 7009 (Token Revocation) | External spec | — | `revocation_endpoint` POST |
| RFC 7662 (Token Introspection) | External spec | — | `--introspect` status check |

**Removed Python dependencies (no longer needed):**

| Removed | Replaced by |
|---------|-------------|
| `keyring>=25.0` | `github.com/zalando/go-keyring` |
| `cryptography>=42.0` | Go stdlib `crypto/rand` + `crypto/sha256` |
| `httpx>=0.27` | Go stdlib `net/http` |
| `respx>=0.21` | Go stdlib `net/http/httptest` |
| `secretstorage>=3.3` | Handled transparently by `go-keyring` via D-Bus |

---

## 15. Open Questions

| ID | Question | Owner | Resolution Target |
|----|----------|-------|-------------------|
| OQ-01 | Should dynamic client registrations be shared across TAG profiles, or be per-profile? Current design: shared by `(server_name, as_url)`, profile only affects token storage. Implication: a revoked registration affects all profiles. | Platform team | Week 1 |
| OQ-02 | Google Workspace requires creating an OAuth client in Google Cloud Console — dynamic client registration (RFC 7591) is not supported by Google. How should the user be guided through manual client setup for Google servers? Pre-populated instructions in `tag mcp auth google/calendar`? | UX | Week 1 |
| OQ-03 | Should `tag mcp auth refresh --all` be a safe operation callable from cron (PRD-022) to proactively refresh all tokens nightly? Risk: triggers device flow re-auth if a refresh token has expired, blocking the cron job. Proposed: skip re-auth in cron mode; emit warning instead. | Platform team | Week 2 |
| OQ-04 | The `go-keyring` library uses OS keychain backends which may prompt the user for their macOS login password on first access. Is this acceptable UX, or should we document `security unlock-keychain` as a pre-step in CI pipelines? | DevEx | Week 1 |
| OQ-05 | Should `tag mcp auth` support multiple connected accounts for the same server (e.g., two GitHub accounts)? Current design: `UNIQUE(server_name, profile_name)` allows one account per profile. Multi-account would require a named-account slug. Defer? | Product | Week 1 |
| OQ-06 | What is the right behaviour when `tag mcp auth notion` is run with an existing valid token? Current spec: prompt "Token valid until X. Re-authorise? [y/N]" unless `--force` is passed. Confirm this is correct. | UX | Week 1 |
| OQ-07 | Stripe's OAuth 2.0 implementation (Stripe Connect) uses a non-standard token format and does not support RFC 8414 AS metadata discovery. Does Stripe offer a compliant MCP server, or must we hardcode Stripe's endpoints entirely? | Research | Week 1 |
| OQ-08 | Linear's MCP server is in early access. Should it be listed as `experimental` in `KnownServers` with a warning on first use? | Product | Week 2 |

---

## 16. Complexity and Timeline

**Total estimated effort:** 9–11 engineering days (1–2 weeks)

### Phase 1: Core Infrastructure (Days 1–3)

- SQLite migration (`internal/store/migrations/0020_mcp_auth.sql`): create `mcp_auth_accounts`, `mcp_auth_registrations`, `mcp_auth_events` tables; wire into `internal/store` migration runner
- Go types (`internal/mcp/oauth/types.go`): `PKCEParams` with `GeneratePKCEParams()`, `ASMetadata`, `PRMDocument`, `TokenResponse`, `ConnectedAccount`, `KnownServerProfile`
- `KnownServers` map and `DestructiveScopeGate` in `internal/mcp/oauth/known_servers.go`
- `IsHeadless()` in `internal/mcp/oauth/headless.go` with full env-var matrix
- Unit tests for all of the above (target: 100% line coverage on pure-logic code)

### Phase 2: OAuth Flows (Days 4–6)

- Discovery chain: `DiscoverAuthServer()` with PRM → AS metadata steps; integrate with go-sdk auth middleware for PKCE Auth-Code path
- Dynamic client registration: `RegisterClient()` with `go-keyring` storage of `client_secret`
- PKCE flow: `RunPKCEFlow()` including `RunPKCECallbackServer()`, browser open via `os/exec`, code exchange via `net/http`
- Device flow: `RunDeviceFlow()` including goroutine polling loop with exponential backoff on `slow_down`, using `golang.org/x/oauth2` device auth
- Token storage: `StoreTokens()` via `go-keyring` + `internal/store` metadata update
- Destructive scope HITL gate
- Unit + integration tests with `httptest.NewServer` mock AS

### Phase 3: Token Lifecycle (Days 7–8)

- `GetToken()` with proactive refresh logic in `internal/mcp/oauth/oauth.go`
- `refreshTokenSync()` internal refresh function
- `ErrTokenExpired` / `ErrNoToken` / `ErrTokenRevoked` sentinel errors
- `CmdRevoke()` with RFC 7009 revocation calls via `net/http`
- `CmdStatus()` with live introspection and API probe fallback
- `CmdRefresh()` force-refresh command
- Unit tests for all refresh and revocation edge cases

### Phase 4: CLI Surface + Tracing (Days 9–10)

- `CmdAuth()`, `CmdList()`, `CmdRevoke()`, `CmdStatus()`, `CmdExport()` cobra commands wired into `internal/cli/mcp_auth.go`
- `tag mcp auth list` table + JSON rendering via `internal/tui`
- PRD-013 span instrumentation via `go.opentelemetry.io/otel` for all OAuth lifecycle steps in `internal/obs`
- Notification integration for proactive refresh warnings
- `CmdExport()` with `internal/credentials.ScanForSecrets()` gate (PRD-034)
- End-to-end integration tests for all six known server profiles

### Phase 5: Review + Hardening (Day 11)

- Security audit: scan `internal/mcp/oauth` for any token value exposure paths
- Performance benchmarks: `BenchmarkList20Accounts`, `BenchmarkGetTokenCacheHit`
- Cross-platform keychain testing: macOS Keychain, Linux Secret Service (Docker), Windows Credential Manager (CI runner)
- `go test -coverprofile=cover.out ./internal/mcp/oauth/... && go tool cover -func=cover.out` — fail if < 90%
- Address open questions OQ-01 through OQ-04; update GoDoc comments
- Merge to `main`

---

*End of PRD-074*
