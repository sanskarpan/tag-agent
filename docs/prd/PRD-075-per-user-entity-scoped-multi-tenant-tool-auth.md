# PRD-075: Per-User Entity-Scoped Multi-Tenant Tool Auth (`tag entity`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** L (2-4 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `internal/credentials + entity_credentials SQLite table`
**Depends on:** PRD-013 (agent tracing/observability), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-034 (secret scanning), PRD-014 (MCP server registry)
**Inspired by:** Composio entity-scoped auth, Arcade AI user scoping
**GitHub issue:** #346

---

## 1. Overview

TAG is primarily used as a single-user CLI tool: one developer, one set of tool credentials, one GitHub token, one Slack workspace. This model breaks down the moment a development team tries to build a multi-tenant product on top of TAG — for example, a SaaS platform where each end-user can ask an agent to open a GitHub PR in their own repository, post to their own Slack channel, or read their own Notion pages. In the current architecture, every agent run shares the same ambient credential set loaded from environment variables. There is no mechanism to scope tool access to a named end-user (entity), inject per-user credentials into a run without surfacing them to the LLM, or audit which entity triggered which tool call.

Per-User Entity-Scoped Multi-Tenant Tool Auth solves this by introducing the concept of an **entity** — a named end-user context (identified by a user-supplied `user_id` string) that carries its own credential map per provider. When a caller submits a task with `--entity user-42`, TAG's run context loader resolves that entity's credentials from the `entity_credentials` SQLite table, injects them as environment overrides into the MCP server subprocess environment, and scopes all tracing spans with an `entity.id` attribute. The LLM never receives token values directly; credentials are brokered at the session layer before the first tool call is issued, following the Composio brokered-credential model exactly.

This feature makes TAG viable as the agentic backend for multi-tenant B2B products. A SaaS company can call `tag submit --entity <their_user_id> --prompt "..."` for each of their end-users, confident that user-42's GitHub token will never contaminate user-43's tool calls, that every credential is stored encrypted at rest in the local SQLite database, and that the audit log can be filtered by entity. The entity model is intentionally simple: no RBAC, no OAuth server, no web dashboard. It is a thin credential-routing layer that sits below the existing TAG run machinery and above the MCP server process lifecycle.

The design is directly inspired by Composio's `entity.initiate_connection()` pattern and Arcade AI's `user_id`-scoped tool execution. The key difference from those platforms is that TAG's entity system is entirely local — credentials are stored in the user's own SQLite database rather than a third-party vault — and the API surface is a plain CLI rather than a hosted API. Operators who need cloud-hosted credential storage can adapt the `CredentialBackend` interface introduced here to delegate to a secrets manager (AWS Secrets Manager, HashiCorp Vault) without changing the CLI surface.

The `tag entity` command cluster covers the full lifecycle: creating an entity record, associating one or more provider credentials per entity, listing entities with their connection status, revoking credentials, and running agent tasks scoped to a specific entity. The `entity_credentials` SQLite table is the single source of truth, encrypted at the column level using a key derived from the user's machine keychain. All reads and writes go through the `internal/credentials` package, which also handles automatic refresh for OAuth-style tokens and exposes a `CredentialContext` struct that the run machinery consumes.

---

## 2. Problem Statement

### 2.1 No isolation between end-users in multi-tenant agentic products

When a development team builds a SaaS product that calls `tag submit` on behalf of their end-users, every call shares the same set of ambient credentials loaded from the shell environment or the TAG config file. If user-42 grants the product access to their GitHub account and user-43 grants access to theirs, there is currently no way to tell TAG "use user-42's GitHub token for this request and user-43's token for that one." The only workaround today is to maintain separate TAG installations (separate `TAG_HOME` directories, separate SQLite databases) per end-user, which is operationally infeasible for products with more than a handful of users.

### 2.2 Credentials surface in LLM context or subprocess environment globally

Even if a developer manually injects a per-user token into the environment before calling `tag submit`, that token is set as a global environment variable for the entire TAG process. This means other concurrent runs (via `tag submit --background` or the queue worker) inherit that credential unintentionally. More critically, if the LLM is prompted with instructions that include environment variable names, a sufficiently capable model can discover and relay credential values. The brokered-credential model — where the credential never appears in any prompt, tool description, or LLM-visible context — requires first-class support in the run machinery, which does not currently exist.

### 2.3 No per-entity audit trail

TAG's tracing (PRD-013) records every tool call in the `spans` table with a `profile` attribute. In a multi-tenant scenario, the `profile` attribute identifies the agent configuration but not the end-user on whose behalf the call was made. Post-incident forensics (e.g., "which end-user caused this GitHub rate limit hit?") require joining against application-level logs that exist outside TAG's observability layer. Adding a first-class `entity_id` attribute to spans closes this gap and makes TAG's built-in audit log usable for multi-tenant compliance.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | `tag entity create --id <user_id>` creates a named entity record in the `entity_credentials` SQLite table with zero configuration beyond the ID. |
| G2 | `tag entity auth --id <user_id> --provider <slug> --token <tok>` stores a credential (token, refresh token, expiry) for the given entity+provider pair, encrypted at rest using a keychain-derived key. |
| G3 | `tag submit --entity <user_id> --prompt "..."` scopes the run to that entity's credentials, injecting them as per-provider environment overrides into MCP server subprocesses only, never into the LLM prompt. |
| G4 | The `internal/credentials` package exposes a `CredentialContext` struct and `LoadEntityCredentials(ctx, backend, entityID, provider)` function that the run machinery calls to fetch the correct token before spawning an MCP server subprocess. |
| G5 | All tracing spans produced during an entity-scoped run carry an `entity.id` attribute, enabling per-entity audit queries via `tag trace list --entity <user_id>`. |
| G6 | `tag entity list --json` outputs a machine-readable list of all entities with their provider connection statuses (connected, expired, missing) without revealing token values. |
| G7 | `tag entity revoke --id <user_id> --provider <slug>` deletes the credential for that entity+provider pair from the database, zeroing the encrypted column before deletion. |
| G8 | Credentials are encrypted at rest using AES-256-GCM with a key stored in the OS keychain (via `github.com/zalando/go-keyring`); they are never stored in plaintext in the SQLite database or any log file. |
| G9 | `tag entity auth` supports both static tokens (GitHub PAT, Slack bot token) and OAuth 2.1 authorization-code flows with PKCE, auto-selecting device-code flow when the terminal is headless. |
| G10 | A `CredentialBackend` Go interface allows operators to substitute cloud secrets managers (AWS Secrets Manager, HashiCorp Vault) in place of the default SQLite+keychain backend without changing the CLI surface. |
| G11 | The `entity_credentials` table migration runs automatically inside `store.OpenDB()` on first use, following the existing migration pattern in `internal/store`. |
| G12 | A connected-account cache `(entity_id, provider) → *CredentialContext` is maintained in-process for the lifetime of a single `tag submit` call, eliminating redundant decrypt operations for multi-step runs that call the same provider's tools repeatedly. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Hosted credential storage or a cloud vault API. All credentials remain on the user's local machine in the SQLite database and OS keychain unless the operator explicitly swaps in a cloud backend via the `CredentialBackend` interface. |
| NG2 | Role-based access control (RBAC) between entities. This PRD does not introduce the concept of entity permissions, entity groups, or resource-level authorization. All entities are equal; access control is enforced by the external provider (GitHub, Slack, Notion) via the scopes of the stored token. |
| NG3 | A web UI or REST API for entity management. Entity CRUD is exclusively a CLI operation in this PRD. PRD-054 (local browser dev UI) may expose a future web view. |
| NG4 | OAuth server functionality. TAG is an OAuth client only. It does not act as an authorization server or issue tokens to downstream callers. |
| NG5 | Syncing entities or credentials across multiple machines. The `entity_credentials` table lives in `~/.tag/runtime/tag.sqlite3` on a single machine. Cross-machine sync is out of scope. |
| NG6 | Supporting more than one active token per entity+provider pair simultaneously. If `tag entity auth` is called twice for the same entity+provider, the second call overwrites the first. Multiple-connection management (Composio's `connected_account_id` model) is out of scope for this PRD. |
| NG7 | Automatic token rotation for static tokens (GitHub PAT, Slack bot token). Rotation is a manual operation via `tag entity auth --id ... --provider ... --token <new_tok>`. Only OAuth refresh tokens are automatically refreshed. |
| NG8 | Provider-specific MCP server installation. Credential storage is independent of which MCP servers are installed for a profile. `tag mcp registry install` (PRD-014) handles installation. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Entity creation latency | `tag entity create` completes in < 100 ms including DB write | `time tag entity create --id bench-user` averaged over 20 runs |
| Credential inject latency | Per-entity credential load adds < 5 ms overhead to `tag submit` startup | Profiled with `go test -bench` + `pprof` on the `LoadEntityCredentials` call path |
| Zero token leakage | No entity credential value appears in any span attribute, log line, or prompt text during an instrumented run | Automated scan of all `spans` rows and TAG log output after a test run |
| Encryption at rest | `sqlite3 ~/.tag/runtime/tag.sqlite3 "SELECT credential_enc FROM entity_credentials LIMIT 1"` returns an opaque base64 blob, not a plaintext token | CI integration test |
| Audit completeness | Every tool call span in an entity-scoped run carries `entity.id` in its `attributes` JSON | Integration test asserting span attribute presence |
| Entity list correctness | `tag entity list --json` returns accurate `status` (connected/expired/missing) for all providers without any credential value fields | Unit test with mock credentials table |
| OAuth device flow | `tag entity auth --provider github` completes OAuth authorization via device code on a headless terminal (no `DISPLAY` env var) | End-to-end test in CI with a mock OAuth server |
| Multi-entity isolation | Two concurrent `tag submit` calls with different `--entity` values use independent credential sets with no cross-contamination | Concurrency integration test asserting per-subprocess env vars |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | SaaS platform engineer | call `tag entity create --id user-42` and `tag entity auth --id user-42 --provider github --token ghp_...` once per onboarding user | I can route that user's subsequent agent tasks through their own GitHub credentials without managing separate TAG installations |
| U2 | Platform engineer | call `tag submit --entity user-42 --prompt "Open a PR on their repo fixing the bug in issue #17"` | The agent operates with user-42's GitHub token and cannot accidentally use user-43's credentials |
| U3 | Platform engineer | run `tag entity list --json` | I can verify which of my tenant entities have active credentials and which need re-authorization, without a web dashboard |
| U4 | Security engineer | run `tag entity revoke --id user-42 --provider github` when a user offboards | The credential is permanently deleted from the database with no residual plaintext |
| U5 | Developer | run `tag entity auth --id dev-test --provider slack` in a headless CI environment | The CLI detects the headless context, triggers OAuth device-code flow, and prints a URL+code I can complete in a browser |
| U6 | Platform engineer | query `tag trace list --entity user-42` | I can audit exactly which tool calls were made on behalf of user-42 for compliance or debugging purposes |
| U7 | Developer | call `tag entity auth --id user-42 --provider github --scopes repo,read:user` | I can control which OAuth scopes are requested during the authorization flow rather than always requesting maximum scopes |
| U8 | Operator | set `TAG_CREDENTIAL_BACKEND=vault` and configure a HashiCorp Vault URL | All entity credentials are stored in and retrieved from Vault instead of the local SQLite database, for production multi-tenant deployments |
| U9 | Developer | run `tag entity show --id user-42` | I can see a single entity's full provider list with connection status, last-used timestamp, and token expiry (if applicable) without seeing the raw token value |
| U10 | Developer | run `tag entity rotate --id user-42 --provider github` | The CLI triggers a new OAuth flow to replace the existing token, atomically swapping the old credential for the new one with zero downtime for in-flight runs |

---

## 7. Proposed CLI Surface

All entity subcommands live under the `tag entity` namespace. The `--entity` flag is added to `tag submit` and `tag run`.

### 7.1 `tag entity create`

Create a new entity record.

```
tag entity create \
  --id <user_id> \
  [--description "optional human label"] \
  [--json]
```

**Options:**
- `--id` (required): Unique identifier for the entity. Must match `[a-zA-Z0-9_\-]{1,128}`. Duplicate IDs return exit code 1 with a clear error.
- `--description`: Free-text label stored alongside the entity record (e.g. "Acme Corp user #42").
- `--json`: Emit a JSON object instead of the human-readable confirmation.

**Output (human):**
```
Entity created: user-42
  Description: Acme Corp user #42
  Created at:  2026-06-17T10:23:41Z
  Credentials: none
```

**Output (--json):**
```json
{
  "id": "user-42",
  "description": "Acme Corp user #42",
  "created_at": "2026-06-17T10:23:41Z",
  "providers": []
}
```

**Exit codes:** 0 success, 1 duplicate ID or validation error.

---

### 7.2 `tag entity auth`

Store or refresh credentials for an entity+provider pair.

```
tag entity auth \
  --id <user_id> \
  --provider <slug> \
  [--token <static_token>] \
  [--scopes <comma_separated_scopes>] \
  [--oauth] \
  [--device-flow] \
  [--refresh-token <tok>] \
  [--expires-at <iso8601>] \
  [--env-var <VAR_NAME>]
```

**Options:**
- `--id` (required): Entity ID (must already exist via `tag entity create`).
- `--provider` (required): Provider slug. Built-in slugs: `github`, `slack`, `notion`, `linear`, `jira`, `google-drive`, `google-calendar`, `gmail`, `gitlab`, `bitbucket`. Custom slugs are accepted and stored as-is.
- `--token`: Static credential value (PAT, bot token, API key). Mutually exclusive with `--oauth`.
- `--scopes`: Comma-separated OAuth scopes to request. Ignored for static tokens. Defaults to the provider's recommended scope set defined in `ProviderDefaultScopes`.
- `--oauth`: Trigger an OAuth 2.1 authorization-code + PKCE flow. Auto-selects device-code if `DISPLAY` is absent or `SSH_CLIENT` is set.
- `--device-flow`: Force device-code flow regardless of headless detection.
- `--refresh-token`: Store alongside `--token` for providers that issue refresh tokens out-of-band.
- `--expires-at`: ISO 8601 expiry timestamp for the token. Used to compute `status=expired` in `tag entity list`.
- `--env-var`: Instead of reading the token from the CLI argument (which would appear in shell history), read it from the named environment variable. Example: `--env-var GITHUB_TOKEN`.

**Interactive OAuth flow output:**
```
Starting OAuth flow for github (entity: user-42)
  Authorization URL: https://github.com/login/device
  User code:        ABCD-1234
  Expires in:       900 seconds

Waiting for authorization... (polling every 5s)
Authorization complete.
  Scopes granted: repo, read:user, read:org
  Token stored for entity user-42, provider github.
  Expires: never (PAT-style token)
```

**Static token output:**
```
Credential stored for entity user-42, provider github.
  Token type: static
  Stored at:  2026-06-17T10:25:00Z
```

**Exit codes:** 0 success, 1 entity not found, 2 OAuth flow cancelled or timed out, 3 token validation failed (test call rejected by provider).

---

### 7.3 `tag entity list`

List all entities with their provider connection summary.

```
tag entity list \
  [--json] \
  [--provider <slug>] \
  [--status connected|expired|missing]
```

**Options:**
- `--json`: Machine-readable output.
- `--provider`: Filter to only entities that have a credential for this provider.
- `--status`: Filter by connection status.

**Human output:**
```
ENTITY ID         PROVIDERS                         CREATED
user-42           github(connected), slack(expired) 2026-06-17
user-43           github(connected)                 2026-06-16
user-99           (none)                            2026-06-15

3 entities total.
```

**JSON output (--json):**
```json
[
  {
    "id": "user-42",
    "description": "Acme Corp user #42",
    "created_at": "2026-06-17T10:23:41Z",
    "providers": [
      {"slug": "github", "status": "connected", "scopes": ["repo", "read:user"], "expires_at": null, "last_used": "2026-06-17T11:00:00Z"},
      {"slug": "slack",  "status": "expired",   "scopes": ["channels:read"],     "expires_at": "2026-06-10T00:00:00Z", "last_used": "2026-06-09T22:00:00Z"}
    ]
  }
]
```

**Exit codes:** 0 success (even if list is empty), 1 internal DB error.

---

### 7.4 `tag entity show`

Show full detail for a single entity.

```
tag entity show \
  --id <user_id> \
  [--json]
```

**Human output:**
```
Entity: user-42
  Description: Acme Corp user #42
  Created at:  2026-06-17T10:23:41Z

  Provider      Status      Scopes                  Last Used            Expires
  github        connected   repo, read:user         2026-06-17 11:00     never
  slack         expired     channels:read,chat:write 2026-06-09 22:00    2026-06-10 00:00
```

**Exit codes:** 0 success, 1 entity not found.

---

### 7.5 `tag entity revoke`

Delete credentials for an entity+provider pair (or all providers).

```
tag entity revoke \
  --id <user_id> \
  [--provider <slug>] \
  [--all] \
  [--yes]
```

**Options:**
- `--provider`: Revoke only this provider's credential. Mutually exclusive with `--all`.
- `--all`: Revoke all provider credentials for this entity (and delete the entity record).
- `--yes`: Skip confirmation prompt.

**Output:**
```
Revoked credential for entity user-42, provider github.
  Encrypted column zeroed before deletion.
```

**Exit codes:** 0 success, 1 entity or provider not found.

---

### 7.6 `tag entity rotate`

Trigger a fresh OAuth flow to replace an existing token atomically.

```
tag entity rotate \
  --id <user_id> \
  --provider <slug> \
  [--scopes <comma_separated>] \
  [--device-flow]
```

Performs an OAuth flow identical to `tag entity auth --oauth`, but writes the new credential only after the flow succeeds and deletes the old credential atomically in a single SQLite transaction. Ongoing runs using the old credential are not interrupted (they hold their own in-process copy of the `*CredentialContext`).

---

### 7.7 `tag entity delete`

Delete an entity record and all its credentials.

```
tag entity delete \
  --id <user_id> \
  [--yes]
```

Equivalent to `tag entity revoke --all` followed by deleting the entity row from `entities`. Requires confirmation unless `--yes` is passed.

---

### 7.8 `tag submit --entity`

Run an agent task scoped to a named entity.

```
tag submit \
  --entity <user_id> \
  --prompt "Open a PR on their repo fixing issue #17" \
  [--profile <profile_name>] \
  [--json] \
  [--background]
```

**Behavior:** Before spawning any MCP server subprocesses, `LoadEntityCredentials(ctx, backend, entityID, "")` is called. The resulting `map[string]*CredentialContext` is merged into the per-subprocess `exec.Cmd.Env` slice maintained by the run machinery. The entity ID is recorded in the `runs` table's `metadata_json` column and propagated to all `spans` via the `entity.id` attribute. Tenant identity flows through `context.Context` from the submit handler through every layer — no goroutine-local state.

**Output addition (--json):**
```json
{
  "run_id": "run-abc123",
  "entity_id": "user-42",
  "entity_providers": ["github", "slack"],
  ...
}
```

---

### 7.9 `tag trace list --entity`

Filter trace output by entity.

```
tag trace list \
  --entity <user_id> \
  [--since <iso8601>] \
  [--limit N] \
  [--json]
```

Queries spans with `json_extract(attributes, '$.entity.id') = ?`. Returns the same schema as the existing `tag trace list` command with an additional `entity_id` column.

---

## 8. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag entity create --id <id>` MUST insert a row into the `entities` table within a single WAL-mode SQLite transaction and return within 100 ms under nominal conditions. |
| FR-02 | `tag entity create` MUST reject entity IDs that do not match the pattern `[a-zA-Z0-9_\-]{1,128}` with a human-readable error message and exit code 1. |
| FR-03 | `tag entity create` MUST return exit code 1 and a clear error if the entity ID already exists; it MUST NOT silently overwrite an existing entity. |
| FR-04 | `tag entity auth --token <tok>` MUST encrypt the token value using AES-256-GCM before writing it to the `entity_credentials` table. The plaintext token MUST NOT appear in any SQLite column, log line, span attribute, or debug output. |
| FR-05 | `tag entity auth --env-var <VAR>` MUST read the token from the named environment variable and immediately call `os.Unsetenv` on that variable after reading, so it is not inherited by any subprocess. |
| FR-06 | `tag entity auth --oauth` MUST implement the full OAuth 2.1 discovery chain: HTTP 401 → Protected Resource Metadata → Authorization Server Metadata → dynamic client registration (if needed) → PKCE authorization-code or device-code flow. |
| FR-07 | `tag entity auth --oauth` MUST include the `resource` parameter (RFC 8707) in both the authorization request and the token exchange request. |
| FR-08 | `tag entity auth --oauth` MUST auto-detect headless environments by checking for the absence of `DISPLAY` environment variable and the presence of `SSH_CLIENT`; in headless mode it MUST automatically use device-code flow without requiring `--device-flow`. |
| FR-09 | Device-code polling MUST implement exponential backoff starting at 5 seconds and doubling on `slow_down` responses, capped at 30 seconds, for up to 15 minutes before timing out with exit code 2. |
| FR-10 | The encryption key used for `entity_credentials` MUST be derived from a key stored in the OS keychain via `github.com/zalando/go-keyring` under service name `tag.entity.credentials` and account name `aes256gcm.key`. If no key exists, one MUST be generated via `crypto/rand` and stored in the keychain on first use. The key MUST be cached in-process via `sync.Once` to avoid repeated keychain round-trips. |
| FR-11 | `LoadEntityCredentials(ctx, backend, entityID, provider)` in `internal/credentials` MUST return a `map[string]*CredentialContext` (all providers when `provider` is empty) synchronously in the calling goroutine; decryption MUST complete in < 5 ms per credential. |
| FR-12 | The run machinery (`internal/cli` submit handler) MUST call `LoadEntityCredentials` before constructing any MCP server `exec.Cmd` and inject the resolved environment variables into `cmd.Env`. The credential values MUST NOT appear in the `prompt` argument or any LLM API call payload. |
| FR-13 | Every `spans` row produced during an entity-scoped run MUST have `entity.id` set in its `attributes` JSON column. |
| FR-14 | Every `runs` row produced during an entity-scoped run MUST have `entity_id` set in its `metadata_json` column. |
| FR-15 | `tag entity revoke` MUST zero the `credential_enc` column (write 44 zero bytes) before deleting the row, to reduce residual data exposure in SQLite's free-list pages. |
| FR-16 | `tag entity list` MUST compute `status` as: `connected` (token present and not expired), `expired` (token present but `expires_at < now()`), `missing` (no credential row). It MUST NOT attempt a live API call to verify token validity unless `--verify` flag is passed. |
| FR-17 | `tag entity auth` MUST store `scopes` as a comma-separated string in the `entity_credentials` table and return them in `tag entity list --json` output. |
| FR-18 | `tag entity rotate` MUST complete the new OAuth flow and write the new credential in the same SQLite transaction that deletes the old credential, ensuring no window where the entity has zero credentials. |
| FR-19 | A `CredentialBackend` Go interface MUST be defined in `internal/credentials` with `Get`, `Put`, `Delete`, and `ListProviders` methods. The default implementation MUST use the `modernc.org/sqlite` + `go-keyring` backend. |
| FR-20 | `TAG_CREDENTIAL_BACKEND` environment variable MUST be inspected by `GetBackend()` in `internal/credentials` to select the active backend. Supported values: `sqlite` (default), `vault` (HashiCorp Vault via `github.com/hashicorp/vault/api`), `aws-ssm` (AWS SSM Parameter Store via `aws-sdk-go-v2/service/ssm`). |
| FR-21 | `tag entity auth` MUST validate the stored token by making a provider-specific test API call (e.g., `GET /user` for GitHub) before confirming storage, unless `--skip-verify` is passed. |
| FR-22 | The `entity_credentials` migration MUST run inside `store.OpenDB()` using `db.ExecContext` with the DDL guarded by `CREATE TABLE IF NOT EXISTS`, following the existing `internal/store` migration pattern. |
| FR-23 | Concurrent `tag submit --entity` calls with different entity IDs MUST NOT share in-process `*CredentialContext` cache entries; the cache MUST be function-local (not package-global), keyed by `(entity_id, provider)` and scoped to the invocation. |
| FR-24 | `tag entity show` MUST display `last_used` timestamp (sourced from the `last_used_at` column updated on each `LoadEntityCredentials` call) without revealing the credential value. |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|------------|--------|
| NFR-01 | Credential encryption/decryption latency | < 5 ms per credential on a commodity laptop (M2 MacBook Pro baseline) |
| NFR-02 | `tag entity list` latency with 10,000 entities | < 500 ms (index scan on `entities.id`) |
| NFR-03 | Memory footprint of `*CredentialContext` cache | < 1 MB for 1,000 simultaneously cached entities |
| NFR-04 | Secret-scan compliance | The `internal/credentials` package MUST pass PRD-034 secret scanning with zero findings (no high-entropy strings in source code) |
| NFR-05 | Keychain access latency | The AES key retrieval from the OS keychain MUST be cached in-process after the first read via `sync.Once` to avoid repeated keychain round-trips; the cached key MUST be stored as a `[]byte` in a package-level variable guarded by `sync.Once` |
| NFR-06 | SQLite WAL compatibility | All `entity_credentials` writes MUST use WAL-mode transactions via `modernc.org/sqlite`; no exclusive locks that could block concurrent `tag submit` calls |
| NFR-07 | Go version compatibility | `internal/credentials` MUST target Go 1.22+ with `CGO_ENABLED=0` using `modernc.org/sqlite` (pure-Go, no cgo dependency) |
| NFR-08 | Dependency footprint | New hard dependencies are limited to `github.com/zalando/go-keyring` (already targeted by TAG) and stdlib `crypto/aes`+`crypto/cipher` (no new dep). OAuth MUST use stdlib `net/http`. `github.com/hashicorp/vault/api` and `aws-sdk-go-v2` are optional build-tag extras. |
| NFR-09 | CLI help completeness | Every `tag entity` subcommand MUST have a `--help` string that names the backing SQLite table and the encryption method used |
| NFR-10 | Audit log retention | `entity_credentials` rows MUST include `created_at` and `last_used_at` columns; `tag entity list` MUST expose `last_used` to support SCIM-style dormant-user detection |

---

## 10. Technical Design

### 10.1 New Go Files / Packages

| File | Purpose |
|------|---------|
| `internal/credentials/credentials.go` | `CredentialContext` struct, `EntityRecord` struct, `CredentialBackend` interface, `LoadEntityCredentials`, `StoreEntityCredential`, `RevokeEntityCredential`, `GetBackend` |
| `internal/credentials/crypto.go` | AES-256-GCM `encrypt`/`decrypt`, `getOrCreateAESKey` via `sync.Once` + `go-keyring` |
| `internal/credentials/sqlite_backend.go` | `SQLiteBackend` implementing `CredentialBackend` via `modernc.org/sqlite` |
| `internal/credentials/providers.go` | `providerPrimaryEnvVar`, `ProviderDefaultScopes`, `ProviderASMetadataURL`, `buildEnvVars` |
| `internal/credentials/oauth.go` | `IsHeadless`, `RunDeviceFlow`, `RunAuthCodeFlow`, `attemptRefresh`, `OAuthFlowError`, `OAuthTimeoutError` |
| `internal/credentials/context.go` | `WithEntityID(ctx, id)`, `EntityIDFromContext(ctx)` — tenant identity via `context.Context`, not goroutine-locals |
| `internal/credentials/vault.go` | Optional `VaultBackend` via `github.com/hashicorp/vault/api`; selected when `TAG_CREDENTIAL_BACKEND=vault` |
| `internal/credentials/awsssm.go` | Optional `AWSSsmBackend` via `aws-sdk-go-v2/service/ssm`; selected when `TAG_CREDENTIAL_BACKEND=aws-ssm` |
| `internal/store/migrate_prd075.go` | DDL constants + `MigratePRD075(db)` called from `store.OpenDB()` |
| `internal/cli/entity.go` | `tag entity` cobra/huma subcommand handlers |
| `internal/obs/semconv.go` (addition) | `AttrEntityID`, `AttrEntityProvider` OTel constants |

### 10.2 SQLite DDL

The following DDL is executed inside `store.OpenDB()` via `MigratePRD075(db *sql.DB)`:

```sql
-- Entity registry: one row per named end-user entity
CREATE TABLE IF NOT EXISTS entities (
  id           TEXT PRIMARY KEY,
  description  TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_entities_created ON entities(created_at);

-- Per-entity, per-provider credential storage
-- credential_enc: AES-256-GCM ciphertext, base64url-encoded
--   format: base64url(nonce[12] || ciphertext || tag[16])
-- refresh_enc:    AES-256-GCM encrypted refresh token (nullable)
-- scopes:         comma-separated granted scope list
-- provider_meta:  JSON blob for provider-specific fields
--   (e.g. {"installation_id": 123} for GitHub App auth)
CREATE TABLE IF NOT EXISTS entity_credentials (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id       TEXT    NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
  provider        TEXT    NOT NULL,
  credential_enc  TEXT    NOT NULL,
  refresh_enc     TEXT,
  token_type      TEXT    NOT NULL DEFAULT 'bearer',
  scopes          TEXT    NOT NULL DEFAULT '',
  expires_at      TEXT,
  provider_meta   TEXT    NOT NULL DEFAULT '{}',
  created_at      TEXT    NOT NULL,
  updated_at      TEXT    NOT NULL,
  last_used_at    TEXT,
  UNIQUE(entity_id, provider)
);
CREATE INDEX IF NOT EXISTS idx_ec_entity   ON entity_credentials(entity_id);
CREATE INDEX IF NOT EXISTS idx_ec_provider ON entity_credentials(provider);
CREATE INDEX IF NOT EXISTS idx_ec_expires  ON entity_credentials(expires_at)
  WHERE expires_at IS NOT NULL;
```

**Migration guard** in `internal/store/migrate_prd075.go`:

```go
// MigratePRD075 adds entities and entity_credentials tables if absent.
// Called unconditionally from store.OpenDB(); CREATE TABLE IF NOT EXISTS
// makes it idempotent.
func MigratePRD075(db *sql.DB) error {
    _, err := db.ExecContext(context.Background(), prd075DDL)
    return err
}
```

### 10.3 Core Structs

```go
// internal/credentials/credentials.go

package credentials

import "time"

// CredentialContext holds the resolved credential for one entity+provider pair.
//
// EnvVars maps environment variable names to their plaintext token values.
// These are injected into exec.Cmd.Env for MCP server subprocesses only —
// they are never concatenated into a prompt or passed to the LLM API.
//
// Example for provider "github":
//
//	EnvVars = map[string]string{"GITHUB_TOKEN": "ghp_xxxxx"}
type CredentialContext struct {
    EntityID     string
    Provider     string
    TokenType    string            // "bearer" | "basic" | "apikey"
    Scopes       []string
    ExpiresAt    *time.Time        // nil → never expires
    EnvVars      map[string]string // env var name → plaintext value (never logged)
    ProviderMeta map[string]any    // provider-specific extras (never logged)
}

func (c *CredentialContext) IsExpired() bool {
    if c.ExpiresAt == nil {
        return false
    }
    return time.Now().UTC().After(*c.ExpiresAt)
}

func (c *CredentialContext) Status() string {
    if c.IsExpired() {
        return "expired"
    }
    return "connected"
}

// EntityRecord is a named end-user context stored in the entities table.
type EntityRecord struct {
    ID          string
    Description string
    CreatedAt   time.Time
    UpdatedAt   time.Time
    Providers   []string
}
```

### 10.4 Encryption Utilities

```go
// internal/credentials/crypto.go

package credentials

import (
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "encoding/base64"
    "fmt"
    "io"
    "sync"

    "github.com/zalando/go-keyring"
)

const (
    keychainService = "tag.entity.credentials"
    keychainAccount = "aes256gcm.key"
)

// aesKey is loaded exactly once per process via sync.Once (NFR-05).
var (
    aesKeyOnce sync.Once
    aesKeyVal  []byte
    aesKeyErr  error
)

func getOrCreateAESKey() ([]byte, error) {
    aesKeyOnce.Do(func() {
        raw, err := keyring.Get(keychainService, keychainAccount)
        if err != nil {
            // Key absent — generate and store.
            key := make([]byte, 32)
            if _, err2 := io.ReadFull(rand.Reader, key); err2 != nil {
                aesKeyErr = fmt.Errorf("generate AES key: %w", err2)
                return
            }
            encoded := base64.StdEncoding.EncodeToString(key)
            if err2 := keyring.Set(keychainService, keychainAccount, encoded); err2 != nil {
                aesKeyErr = fmt.Errorf("store AES key in keychain: %w", err2)
                return
            }
            aesKeyVal = key
            return
        }
        key, err2 := base64.StdEncoding.DecodeString(raw)
        if err2 != nil {
            aesKeyErr = fmt.Errorf("decode AES key from keychain: %w", err2)
            return
        }
        aesKeyVal = key
    })
    return aesKeyVal, aesKeyErr
}

// encrypt returns base64url(nonce[12] || ciphertext || GCM-tag[16]).
func encrypt(plaintext string) (string, error) {
    key, err := getOrCreateAESKey()
    if err != nil {
        return "", err
    }
    block, err := aes.NewCipher(key)
    if err != nil {
        return "", err
    }
    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return "", err
    }
    nonce := make([]byte, gcm.NonceSize())
    if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
        return "", err
    }
    // Seal appends ciphertext+tag to nonce in one allocation.
    blob := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
    return base64.URLEncoding.EncodeToString(blob), nil
}

// decrypt reverses encrypt.
func decrypt(encoded string) (string, error) {
    key, err := getOrCreateAESKey()
    if err != nil {
        return "", err
    }
    raw, err := base64.URLEncoding.DecodeString(encoded)
    if err != nil {
        return "", fmt.Errorf("base64 decode: %w", err)
    }
    block, err := aes.NewCipher(key)
    if err != nil {
        return "", err
    }
    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return "", err
    }
    if len(raw) < gcm.NonceSize() {
        return "", fmt.Errorf("ciphertext too short")
    }
    nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
    pt, err := gcm.Open(nil, nonce, ct, nil)
    if err != nil {
        return "", fmt.Errorf("AES-GCM decrypt: %w", err)
    }
    return string(pt), nil
}
```

### 10.5 CredentialBackend Interface

```go
// internal/credentials/credentials.go (continued)

import "context"

// Backend is the storage abstraction for entity credentials.
// The default implementation uses modernc.org/sqlite + go-keyring.
// Operators may substitute a cloud backend by setting TAG_CREDENTIAL_BACKEND
// and wiring an alternative implementation (see vault.go, awsssm.go).
type Backend interface {
    // Get returns the credential for entity+provider, or nil if absent.
    Get(ctx context.Context, entityID, provider string) (*CredentialContext, error)
    // Put upserts a credential for entity+provider.
    Put(ctx context.Context, entityID, provider string, cred *CredentialContext) error
    // Delete zeros the encrypted column, then deletes the row.
    Delete(ctx context.Context, entityID, provider string) error
    // ListProviders returns all credential contexts for the given entity.
    ListProviders(ctx context.Context, entityID string) ([]*CredentialContext, error)
}

// EntityNotFoundError is returned when the requested entity_id does not exist.
type EntityNotFoundError struct{ ID string }

func (e *EntityNotFoundError) Error() string {
    return fmt.Sprintf("entity %q not found; run: tag entity create --id %s", e.ID, e.ID)
}

// CredentialExpiredError is returned when a credential is expired with no refresh path.
type CredentialExpiredError struct {
    EntityID string
    Provider string
}

func (e *CredentialExpiredError) Error() string {
    return fmt.Sprintf(
        "credential for entity %q provider %q is expired; run: tag entity rotate --id %s --provider %s",
        e.EntityID, e.Provider, e.EntityID, e.Provider,
    )
}
```

### 10.6 SQLiteBackend

```go
// internal/credentials/sqlite_backend.go

package credentials

import (
    "context"
    "database/sql"
    "encoding/json"
    "fmt"
    "strings"
    "time"

    _ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0
)

// SQLiteBackend is the default Backend.
// It uses a single-writer WAL-mode database opened by internal/store.
// gofrs/flock + WAL snapshot isolation serve concurrent readers without
// blocking writers.
type SQLiteBackend struct {
    db *sql.DB
}

func NewSQLiteBackend(db *sql.DB) *SQLiteBackend { return &SQLiteBackend{db: db} }

func (b *SQLiteBackend) Get(ctx context.Context, entityID, provider string) (*CredentialContext, error) {
    var credEnc, tokenType, scopes, metaJSON string
    var refreshEnc, expiresAt sql.NullString

    err := b.db.QueryRowContext(ctx, `
        SELECT credential_enc, refresh_enc, token_type, scopes, expires_at, provider_meta
        FROM entity_credentials
        WHERE entity_id = ? AND provider = ?`, entityID, provider).
        Scan(&credEnc, &refreshEnc, &tokenType, &scopes, &expiresAt, &metaJSON)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }

    // Update last_used_at; ignore error (best-effort).
    _, _ = b.db.ExecContext(ctx,
        `UPDATE entity_credentials SET last_used_at = ? WHERE entity_id = ? AND provider = ?`,
        time.Now().UTC().Format(time.RFC3339), entityID, provider)

    token, err := decrypt(credEnc)
    if err != nil {
        return nil, fmt.Errorf("decrypt token: %w", err)
    }
    var refresh string
    if refreshEnc.Valid {
        if refresh, err = decrypt(refreshEnc.String); err != nil {
            return nil, fmt.Errorf("decrypt refresh token: %w", err)
        }
    }

    var meta map[string]any
    _ = json.Unmarshal([]byte(metaJSON), &meta)

    var exp *time.Time
    if expiresAt.Valid {
        t, _ := time.Parse(time.RFC3339, expiresAt.String)
        exp = &t
    }

    var scopeList []string
    for _, s := range strings.Split(scopes, ",") {
        if s = strings.TrimSpace(s); s != "" {
            scopeList = append(scopeList, s)
        }
    }

    return &CredentialContext{
        EntityID:     entityID,
        Provider:     provider,
        TokenType:    tokenType,
        Scopes:       scopeList,
        ExpiresAt:    exp,
        EnvVars:      buildEnvVars(provider, token, refresh, meta),
        ProviderMeta: meta,
    }, nil
}

func (b *SQLiteBackend) Put(ctx context.Context, entityID, provider string, cred *CredentialContext) error {
    token := cred.EnvVars[providerPrimaryEnvVar(provider)]
    refresh := cred.EnvVars[providerRefreshEnvVar(provider)]

    encToken, err := encrypt(token)
    if err != nil {
        return err
    }
    var encRefresh sql.NullString
    if refresh != "" {
        v, err2 := encrypt(refresh)
        if err2 != nil {
            return err2
        }
        encRefresh = sql.NullString{String: v, Valid: true}
    }
    meta, _ := json.Marshal(cred.ProviderMeta)
    now := time.Now().UTC().Format(time.RFC3339)
    var exp sql.NullString
    if cred.ExpiresAt != nil {
        exp = sql.NullString{String: cred.ExpiresAt.Format(time.RFC3339), Valid: true}
    }

    _, err = b.db.ExecContext(ctx, `
        INSERT INTO entity_credentials
          (entity_id, provider, credential_enc, refresh_enc,
           token_type, scopes, expires_at, provider_meta, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(entity_id, provider) DO UPDATE SET
          credential_enc = excluded.credential_enc,
          refresh_enc    = excluded.refresh_enc,
          token_type     = excluded.token_type,
          scopes         = excluded.scopes,
          expires_at     = excluded.expires_at,
          provider_meta  = excluded.provider_meta,
          updated_at     = excluded.updated_at`,
        entityID, provider, encToken, encRefresh,
        cred.TokenType, strings.Join(cred.Scopes, ","),
        exp, string(meta), now, now)
    return err
}

func (b *SQLiteBackend) Delete(ctx context.Context, entityID, provider string) error {
    // Zero the encrypted column before deletion (FR-15, security point 5).
    zeroed := strings.Repeat("0", 44)
    if _, err := b.db.ExecContext(ctx,
        `UPDATE entity_credentials SET credential_enc = ?, refresh_enc = NULL
         WHERE entity_id = ? AND provider = ?`,
        zeroed, entityID, provider); err != nil {
        return err
    }
    _, err := b.db.ExecContext(ctx,
        `DELETE FROM entity_credentials WHERE entity_id = ? AND provider = ?`,
        entityID, provider)
    return err
}

func (b *SQLiteBackend) ListProviders(ctx context.Context, entityID string) ([]*CredentialContext, error) {
    rows, err := b.db.QueryContext(ctx,
        `SELECT provider FROM entity_credentials WHERE entity_id = ?`, entityID)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var out []*CredentialContext
    for rows.Next() {
        var p string
        if err := rows.Scan(&p); err != nil {
            return nil, err
        }
        cred, err := b.Get(ctx, entityID, p)
        if err != nil || cred == nil {
            continue
        }
        out = append(out, cred)
    }
    return out, rows.Err()
}
```

### 10.7 Provider Defaults Registry

```go
// internal/credentials/providers.go

package credentials

import (
    "fmt"
    "strings"
)

var primaryEnvVarByProvider = map[string]string{
    "github":          "GITHUB_TOKEN",
    "gitlab":          "GITLAB_TOKEN",
    "bitbucket":       "BITBUCKET_TOKEN",
    "slack":           "SLACK_BOT_TOKEN",
    "notion":          "NOTION_TOKEN",
    "linear":          "LINEAR_API_KEY",
    "jira":            "JIRA_API_TOKEN",
    "google-drive":    "GOOGLE_ACCESS_TOKEN",
    "google-calendar": "GOOGLE_ACCESS_TOKEN",
    "gmail":           "GOOGLE_ACCESS_TOKEN",
}

var refreshEnvVarByProvider = map[string]string{
    "google-drive":    "GOOGLE_REFRESH_TOKEN",
    "google-calendar": "GOOGLE_REFRESH_TOKEN",
    "gmail":           "GOOGLE_REFRESH_TOKEN",
}

// ProviderDefaultScopes maps provider slug → recommended OAuth scopes.
var ProviderDefaultScopes = map[string][]string{
    "github":          {"repo", "read:user", "read:org"},
    "gitlab":          {"api", "read_user"},
    "slack":           {"channels:read", "chat:write", "users:read"},
    "notion":          {"read_content", "update_content"},
    "linear":          {"read", "write"},
    "google-drive":    {"https://www.googleapis.com/auth/drive"},
    "google-calendar": {"https://www.googleapis.com/auth/calendar"},
    "gmail":           {"https://mail.google.com/"},
}

// ProviderASMetadataURL maps provider slug → RFC 8414 AS metadata URL.
var ProviderASMetadataURL = map[string]string{
    "github":          "https://github.com/.well-known/oauth-authorization-server",
    "gitlab":          "https://gitlab.com/.well-known/oauth-authorization-server",
    "google-drive":    "https://accounts.google.com/.well-known/openid-configuration",
    "google-calendar": "https://accounts.google.com/.well-known/openid-configuration",
    "gmail":           "https://accounts.google.com/.well-known/openid-configuration",
}

func providerPrimaryEnvVar(provider string) string {
    if v, ok := primaryEnvVarByProvider[provider]; ok {
        return v
    }
    return strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_TOKEN"
}

func providerRefreshEnvVar(provider string) string {
    return refreshEnvVarByProvider[provider] // empty string if absent
}

func buildEnvVars(provider, token, refresh string, meta map[string]any) map[string]string {
    vars := map[string]string{providerPrimaryEnvVar(provider): token}
    if rv := providerRefreshEnvVar(provider); rv != "" && refresh != "" {
        vars[rv] = refresh
    }
    if provider == "github" {
        if id, ok := meta["installation_id"]; ok {
            vars["GITHUB_INSTALLATION_ID"] = fmt.Sprintf("%v", id)
        }
    }
    return vars
}
```

### 10.8 Public Loader API

```go
// internal/credentials/credentials.go (continued)

import (
    "database/sql"
    "sync"
)

var (
    globalBackend     Backend
    globalBackendOnce sync.Once
    globalBackendErr  error
)

// GetBackend returns the process-wide credential backend.
// Selection is driven by TAG_CREDENTIAL_BACKEND (default: "sqlite").
// The backend is constructed once and cached for the process lifetime.
func GetBackend(db *sql.DB) (Backend, error) {
    globalBackendOnce.Do(func() {
        switch os.Getenv("TAG_CREDENTIAL_BACKEND") {
        case "vault":
            globalBackend, globalBackendErr = NewVaultBackend()
        case "aws-ssm":
            globalBackend, globalBackendErr = NewAWSSsmBackend()
        default:
            globalBackend = NewSQLiteBackend(db)
        }
    })
    return globalBackend, globalBackendErr
}

// LoadEntityCredentials loads all provider credentials for entityID,
// optionally filtered to a single provider (empty string = all).
//
// Tenant identity is carried by ctx (set via WithEntityID); callers
// MUST NOT use goroutine-local state to pass entity identity.
// Expired tokens are silently refreshed when a refresh token is present.
// The returned map is function-local; callers must not share it across
// goroutines without copying (FR-23).
func LoadEntityCredentials(
    ctx context.Context,
    backend Backend,
    entityID string,
    provider string,
) (map[string]*CredentialContext, error) {
    result := make(map[string]*CredentialContext)

    if provider != "" {
        cred, err := backend.Get(ctx, entityID, provider)
        if err != nil {
            return nil, err
        }
        if cred == nil {
            return result, nil
        }
        if cred.IsExpired() {
            if refreshed, err2 := attemptRefresh(ctx, backend, entityID, provider, cred); err2 == nil && refreshed != nil {
                cred = refreshed
            }
        }
        result[provider] = cred
        return result, nil
    }

    creds, err := backend.ListProviders(ctx, entityID)
    if err != nil {
        return nil, err
    }
    for _, cred := range creds {
        if cred.IsExpired() {
            if refreshed, err2 := attemptRefresh(ctx, backend, entityID, cred.Provider, cred); err2 == nil && refreshed != nil {
                cred = refreshed
            }
        }
        result[cred.Provider] = cred
    }
    return result, nil
}
```

### 10.9 Context-Key Tenant Propagation

```go
// internal/credentials/context.go

package credentials

import "context"

type entityContextKey struct{}

// WithEntityID attaches an entity ID to ctx for propagation through
// the full request path (submit handler → run machinery → MCP session →
// span factory). Replaces Python thread-locals with Go context values.
func WithEntityID(ctx context.Context, entityID string) context.Context {
    return context.WithValue(ctx, entityContextKey{}, entityID)
}

// EntityIDFromContext retrieves the entity ID attached by WithEntityID.
func EntityIDFromContext(ctx context.Context) (string, bool) {
    id, ok := ctx.Value(entityContextKey{}).(string)
    return id, ok
}
```

### 10.10 OAuth Device-Code Flow

```go
// internal/credentials/oauth.go

package credentials

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "os"
    "strings"
    "time"
)

// IsHeadless reports whether the process is running without a graphical display.
func IsHeadless() bool {
    if os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" {
        return true
    }
    if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
        fi, err := os.Stdin.Stat()
        if err != nil {
            return true
        }
        return (fi.Mode() & os.ModeCharDevice) == 0
    }
    return false
}

// RunDeviceFlow executes RFC 8628 device authorization flow.
// Returns (accessToken, refreshToken, expiresAt) or an error.
// Polling uses exponential backoff on slow_down, capped at 30 s (FR-09).
func RunDeviceFlow(
    ctx context.Context,
    provider string,
    scopes []string,
    asMetadata map[string]string,
) (string, string, *time.Time, error) {
    deviceEndpoint, ok := asMetadata["device_authorization_endpoint"]
    if !ok || deviceEndpoint == "" {
        return "", "", nil, &OAuthFlowError{fmt.Sprintf("provider %q does not support device-code flow", provider)}
    }

    envKey := fmt.Sprintf("TAG_%s_CLIENT_ID", strings.ToUpper(strings.ReplaceAll(provider, "-", "_")))
    clientID := os.Getenv(envKey)

    resp, err := http.PostForm(deviceEndpoint, url.Values{
        "client_id": {clientID},
        "scope":     {strings.Join(scopes, " ")},
    })
    if err != nil {
        return "", "", nil, err
    }
    defer resp.Body.Close()
    body, _ := io.ReadAll(resp.Body)

    var data struct {
        DeviceCode      string `json:"device_code"`
        UserCode        string `json:"user_code"`
        VerificationURI string `json:"verification_uri"`
        Interval        int    `json:"interval"`
        ExpiresIn       int    `json:"expires_in"`
    }
    if err := json.Unmarshal(body, &data); err != nil {
        return "", "", nil, err
    }
    if data.Interval == 0 {
        data.Interval = 5
    }
    if data.ExpiresIn == 0 {
        data.ExpiresIn = 900
    }

    fmt.Printf("\n  Authorization URL: %s\n", data.VerificationURI)
    fmt.Printf("  User code:        %s\n", data.UserCode)
    fmt.Printf("  Expires in:       %d seconds\n\n", data.ExpiresIn)
    fmt.Println("Waiting for authorization...")

    deadline := time.Now().Add(time.Duration(min(data.ExpiresIn, 900)) * time.Second)
    interval := time.Duration(data.Interval) * time.Second
    tokenEndpoint := asMetadata["token_endpoint"]

    for time.Now().Before(deadline) {
        select {
        case <-ctx.Done():
            return "", "", nil, ctx.Err()
        case <-time.After(interval):
        }

        tresp, err := http.PostForm(tokenEndpoint, url.Values{
            "grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
            "device_code": {data.DeviceCode},
            "client_id":   {clientID},
        })
        if err != nil {
            continue
        }
        tbody, _ := io.ReadAll(tresp.Body)
        tresp.Body.Close()

        var td struct {
            AccessToken  string `json:"access_token"`
            RefreshToken string `json:"refresh_token"`
            ExpiresIn    int    `json:"expires_in"`
            Error        string `json:"error"`
        }
        _ = json.Unmarshal(tbody, &td)

        switch td.Error {
        case "authorization_pending":
            // no-op; keep polling
        case "slow_down":
            interval *= 2
            if interval > 30*time.Second {
                interval = 30 * time.Second
            }
        case "expired_token":
            return "", "", nil, &OAuthTimeoutError{"device code expired before authorization"}
        case "access_denied":
            return "", "", nil, &OAuthFlowError{"user denied authorization"}
        case "":
            var exp *time.Time
            if td.ExpiresIn > 0 {
                t := time.Now().UTC().Add(time.Duration(td.ExpiresIn) * time.Second)
                exp = &t
            }
            return td.AccessToken, td.RefreshToken, exp, nil
        default:
            return "", "", nil, &OAuthFlowError{fmt.Sprintf("OAuth error: %s", td.Error)}
        }
    }
    return "", "", nil, &OAuthTimeoutError{"device flow timed out after 15 minutes"}
}

type OAuthFlowError struct{ Msg string }
func (e *OAuthFlowError) Error() string { return e.Msg }

type OAuthTimeoutError struct{ Msg string }
func (e *OAuthTimeoutError) Error() string { return e.Msg }
```

### 10.11 Run Machinery Integration

The integration point is the `cmdSubmit` handler in `internal/cli/submit.go`. Illustrative excerpt:

```go
// internal/cli/submit.go — entity credential injection (PRD-075 addition)

func cmdSubmit(ctx context.Context, cfg *config.Config, args SubmitArgs) error {
    // Attach entity ID to context so it propagates through all layers
    // without goroutine-local state.
    if args.EntityID != "" {
        ctx = credentials.WithEntityID(ctx, args.EntityID)
    }

    backend, err := credentials.GetBackend(cfg.DB)
    if err != nil {
        return err
    }

    var entityCreds map[string]*credentials.CredentialContext
    if args.EntityID != "" {
        entityCreds, err = credentials.LoadEntityCredentials(ctx, backend, args.EntityID, "")
        var notFound *credentials.EntityNotFoundError
        if errors.As(err, &notFound) {
            return fmt.Errorf("entity %q not found; run: tag entity create --id %s",
                args.EntityID, args.EntityID)
        }
        if err != nil {
            return err
        }
    }

    // Merge per-entity env vars for MCP subprocess injection.
    // Added to exec.Cmd.Env — NEVER passed to the LLM prompt builder.
    mcpEnvOverrides := make(map[string]string)
    for _, cred := range entityCreds {
        for k, v := range cred.EnvVars {
            mcpEnvOverrides[k] = v
        }
    }

    // entity_id written to runs.metadata_json via internal/store.
    // entity.id OTel span attribute injected in the span factory (internal/obs).
    // ... existing submit logic continues ...
}
```

### 10.12 Tracing Integration

```go
// internal/obs/semconv.go — additions for PRD-075

package obs

const (
    AttrEntityID       = "entity.id"       // string: the entity user_id
    AttrEntityProvider = "entity.provider"  // string: provider slug used in this span
)
```

The span factory reads `credentials.EntityIDFromContext(ctx)` and sets `AttrEntityID` on every span produced during an entity-scoped run (FR-13).

---

## 11. Security Considerations

1. **Token never in LLM context.** The `EnvVars` map from `*CredentialContext` is injected exclusively into `exec.Cmd.Env` when spawning an MCP server subprocess. It is never concatenated into a prompt string, stored in the `prompt` column of the `runs` table, or passed to any LLM API call body. This is enforced by code review gate: any PR touching `internal/cli/submit.go` that adds `entityCreds` to a string-format expression must be rejected.

2. **AES-256-GCM encryption at rest.** Each credential value is encrypted with a unique 12-byte random nonce using AES-256-GCM before being stored in the `credential_enc` column. The 32-byte AES key is stored in the OS keychain (macOS Keychain, Linux Secret Service via D-Bus, Windows Credential Manager) via `go-keyring` and never written to disk in plaintext. The nonce is stored prepended to the ciphertext blob, making each encryption operation non-deterministic.

3. **Keychain key compromise scope.** If the OS keychain is compromised, all entity credentials stored in the SQLite database are exposed. This is an accepted residual risk for a local-first tool. Operators deploying TAG in production multi-tenant environments MUST use the `vault` or `aws-ssm` backend where the AES key is never stored on the host machine.

4. **Shell history exposure.** `tag entity auth --token <tok>` accepts a token on the command line, which appears in shell history. The `--env-var <VAR>` flag is the recommended alternative for production use. The CLI MUST print a warning when `--token` is used directly: `Warning: token passed via CLI argument will appear in shell history. Prefer --env-var.`

5. **SQLite free-list residual data.** SQLite's WAL mode does not guarantee that deleted row data is immediately overwritten on disk. `tag entity revoke` MUST first update the `credential_enc` column to 44 zero bytes before issuing the `DELETE`, minimizing the window during which a forensic read of the SQLite file could recover the ciphertext. `PRAGMA secure_delete = ON` is set on connections that issue `revoke` operations.

6. **Concurrent goroutine credential isolation.** The in-process `*CredentialContext` map returned by `LoadEntityCredentials` is function-local and not shared across goroutines. Each `tag submit` call loads its own copy of the credentials. Context-based tenant identity (`WithEntityID`) ensures no cross-entity contamination even when the queue worker processes multiple entity-scoped runs concurrently via goroutines.

7. **OAuth state parameter CSRF protection.** The authorization-code flow (non-device-code path) MUST generate a cryptographically random `state` parameter via `crypto/rand` and verify it on the redirect callback. The PKCE `code_verifier` MUST be generated via `crypto/rand` (64 bytes, base64url-encoded) and the `code_challenge` MUST use S256 (SHA-256 + base64url, no padding).

8. **Token validation before storage.** `tag entity auth` MUST make a provider-specific test API call after receiving the token and before writing it to the database. For GitHub: `GET https://api.github.com/user` with `Authorization: token <tok>` via `net/http`. If the call returns a non-2xx response, the token is discarded without being written to the database, and the CLI exits with code 3.

9. **Secret scanner integration (PRD-034).** `internal/credentials` exports a `decrypt` function that returns plaintext token values. The PRD-034 secret scanner MUST be extended to skip `entity_credentials` rows (since the values are intentionally high-entropy) but MUST scan all other columns and all log files for accidental credential leakage.

10. **Refresh token rotation.** When an OAuth refresh token is used to obtain a new access token, the new access token and (if provided) new refresh token MUST be written to the database atomically before the old values are discarded. This prevents a state where the old refresh token has been consumed and the new tokens are lost due to a crash.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/credentials/credentials_test.go`)

Uses `testing` + `github.com/stretchr/testify/assert`. Keychain calls are intercepted via a test-local `Backend` mock implementing the `CredentialBackend` interface.

- **Encryption round-trip:** Assert `decrypt(encrypt(plain)) == plain` for 100 random inputs including Unicode strings and high-entropy byte slices.
- **Nonce uniqueness:** Assert 1,000 calls to `encrypt("x")` produce 1,000 distinct ciphertext strings.
- **Keychain mock:** Stub `go-keyring` via a test-injected key (set `aesKeyVal` directly in test setup) to avoid touching the OS keychain in CI.
- **`IsExpired`:** Assert a `*CredentialContext` with `ExpiresAt = time.Now().Add(-time.Second)` returns `true`; one with `ExpiresAt = time.Now().Add(time.Hour)` returns `false`.
- **`IsHeadless`:** Use `t.Setenv`/`os.Unsetenv` to simulate headless conditions; assert `true` when `DISPLAY` unset and `SSH_CLIENT` set, `false` when `DISPLAY` is set.
- **`buildEnvVars` correctness:** Assert correct env var names for all built-in provider slugs.
- **Status computation:** Assert `Status() == "connected"` for non-expired, `"expired"` for expired.
- **Interface compliance:** Compile-time assertion `var _ Backend = (*SQLiteBackend)(nil)` ensures `SQLiteBackend` satisfies the interface.

### 12.2 Integration Tests (`internal/credentials/integration_test.go`)

All integration tests use an in-memory `modernc.org/sqlite` database (`file::memory:?cache=shared`) opened in test setup via `store.OpenDB`.

- **Full lifecycle:** `create` → `auth` → `list` → `show` → `revoke` → assert entity absent from DB.
- **Duplicate ID rejection:** Assert exit code 1 on second `create` with same ID.
- **Invalid ID format:** Assert error for IDs containing spaces, slashes, or exceeding 128 characters.
- **`--env-var` isolation:** Set env var via `t.Setenv`, call entity auth with `--env-var`, assert the variable is absent from the process environment after the call returns (`os.Getenv` returns `""`).
- **Credential injection into subprocess env:** Intercept `exec.Cmd` construction in the submit handler; assert `GITHUB_TOKEN` is present in `cmd.Env` and absent from the prompt string passed to the LLM client.
- **Concurrent entity isolation:** Launch 10 goroutines each calling `LoadEntityCredentials` with distinct entity IDs via `golang.org/x/sync/errgroup`; assert each goroutine receives its own entity's token value.
- **`tag trace list --entity`:** After a submit run with `--entity user-42`, query spans and assert `json_extract(attributes, '$.entity.id') = 'user-42'` for all produced spans.
- **Revoke zeroing:** After `Delete`, query the raw SQLite table via the test DB; assert no row exists for the entity+provider pair.
- **Token expiry refresh:** Insert a credential with `expires_at` in the past and a `refresh_enc` set; mock the OAuth token endpoint with `net/http/httptest`; assert `LoadEntityCredentials` calls the refresh endpoint and updates the stored token.

### 12.3 OAuth Flow Tests (`internal/credentials/oauth_test.go`)

Uses `net/http/httptest` for all mock servers; no real OAuth provider calls in CI.

- **Device-code happy path:** Mock `device_authorization_endpoint` and `token_endpoint` with `httptest.NewServer`; assert the flow returns a valid `*CredentialContext` after two polling cycles.
- **`slow_down` response:** Assert the polling interval doubles (capped at 30 s) when the mock server returns `{"error":"slow_down"}`.
- **Timeout:** Assert `*OAuthTimeoutError` is returned when the mock server returns `authorization_pending` past the mocked `expires_in`.
- **Headless auto-selection:** Assert that `RunOAuthFlow` selects the device-code path when `IsHeadless()` returns `true` without requiring an explicit `--device-flow` flag.

### 12.4 Performance Tests (benchmarks in `internal/credentials/bench_test.go`)

- **1,000 entities load time:** Insert 1,000 entities with 3 providers each; use `testing.B` to assert `tag entity list` completes within budget; verify < 500 ms with `go test -bench=BenchmarkEntityList -benchtime=5s`.
- **Parallel credential loads:** Use `b.RunParallel` to call `LoadEntityCredentials` across goroutines; assert total wall time well under 200 ms, demonstrating the `sync.Once` AES key cache eliminates repeated keychain round-trips.

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag entity create --id user-42` inserts a row in the `entities` table and prints the entity ID without error. | `go test ./internal/credentials/... -run TestCreateEntity` |
| AC-02 | `tag entity create --id user-42` called a second time exits with code 1 and the message "Entity user-42 already exists." | `go test ./internal/credentials/... -run TestDuplicateEntity` |
| AC-03 | `tag entity auth --id user-42 --provider github --token ghp_test123` stores an encrypted value in `entity_credentials`; `SELECT credential_enc FROM entity_credentials WHERE entity_id='user-42'` returns a non-plaintext blob. | `go test ./internal/credentials/... -run TestEncryptAtRest` |
| AC-04 | `tag submit --entity user-42 --prompt "list my repos"` with a mocked GitHub MCP server asserts `GITHUB_TOKEN=ghp_test123` in the server's `exec.Cmd.Env`. | `go test ./internal/cli/... -run TestCredentialInjection` |
| AC-05 | After `tag submit --entity user-42`, `SELECT json_extract(attributes, '$.entity.id') FROM spans` returns `user-42` for all spans produced in that run. | `go test ./internal/cli/... -run TestSpanEntityAttribute` |
| AC-06 | `tag entity list --json` returns a JSON array where each object has `id`, `created_at`, and `providers` array with `status` field; no object contains a `token` or `credential` field. | `go test ./internal/cli/... -run TestListNoTokenLeak` |
| AC-07 | `tag entity revoke --id user-42 --provider github` followed by `SELECT COUNT(*) FROM entity_credentials WHERE entity_id='user-42' AND provider='github'` returns 0. | `go test ./internal/credentials/... -run TestRevoke` |
| AC-08 | In a headless environment (no `DISPLAY`, `SSH_CLIENT` set), `tag entity auth --id u1 --provider github --oauth` prints a device code URL and polls the token endpoint. | `go test ./internal/credentials/... -run TestDeviceFlowHeadlessAutoselect` |
| AC-09 | Two concurrent `tag submit --entity` calls with different entity IDs produce spans with distinct `entity.id` attributes and no cross-entity credential leakage. | `go test ./internal/cli/... -run TestConcurrentEntityIsolation -count=20` |
| AC-10 | `tag entity auth --token ghp_...` prints a shell history warning to stderr. | `go test ./internal/cli/... -run TestCLITokenHistoryWarning` |
| AC-11 | `tag entity auth --env-var GITHUB_TOKEN` with `GITHUB_TOKEN=ghp_test` in env results in `GITHUB_TOKEN` being absent from the environment after the call returns. | `go test ./internal/credentials/... -run TestEnvVarClearedAfterRead` |
| AC-12 | Setting `TAG_CREDENTIAL_BACKEND=vault` and providing `VAULT_ADDR`/`VAULT_TOKEN` causes `LoadEntityCredentials` to call the Vault KV API instead of the SQLite backend. | `go test ./internal/credentials/... -run TestVaultBackend -tags vault` (requires `httptest` mock Vault server) |
| AC-13 | `tag entity rotate --id user-42 --provider github` completes a new device-code flow and atomically replaces the existing credential; no window exists where the entity has zero credentials (verified by polling the DB in a background goroutine during rotation). | `go test ./internal/credentials/... -run TestRotateAtomicity` |
| AC-14 | `tag trace list --entity user-42` returns only spans where `entity.id = 'user-42'` and correctly excludes spans from runs without an entity. | `go test ./internal/cli/... -run TestTraceFilterByEntity` |
| AC-15 | `internal/credentials` passes `tag security scan` (PRD-034) with zero findings. | `go test ./internal/security/... -run TestCredentialsNoSecrets` |

---

## 14. Dependencies

| Dependency | Type | Version Constraint | Reason |
|------------|------|--------------------|--------|
| `github.com/zalando/go-keyring` | Go module (hard) | latest stable | OS keychain access for AES key storage (macOS Keychain, Linux Secret Service, Windows Credential Manager) |
| stdlib `crypto/aes` + `crypto/cipher` | Go stdlib | go1.22+ | AES-256-GCM encryption/decryption — no new external dependency |
| `crypto/rand` | Go stdlib | go1.22+ | Nonce generation and PKCE `code_verifier` generation |
| `modernc.org/sqlite` | Go module (hard) | v1.34+ | Pure-Go SQLite, `CGO_ENABLED=0`, FTS5 built-in; single-writer + WAL |
| `net/http` | Go stdlib | go1.22+ | OAuth HTTP calls (replaces `httpx`) |
| `github.com/hashicorp/vault/api` | Go module (optional build tag `vault`) | v1.15+ | HashiCorp Vault backend; pulled in only when `TAG_CREDENTIAL_BACKEND=vault` |
| `github.com/aws/aws-sdk-go-v2/service/ssm` | Go module (optional build tag `aws`) | v1.56+ | AWS SSM Parameter Store backend |
| `go.opentelemetry.io/otel` | Go module (hard) | v1.34+ | OTel span attributes for `entity.id` and `entity.provider` |
| `github.com/stretchr/testify` | Go module (test) | v1.9+ | `assert`/`require` in unit and integration tests |
| PRD-013 | Internal PRD | Implemented | `spans` table with `attributes` JSON column for `entity.id` injection |
| PRD-014 | Internal PRD | Implemented | MCP server registry for provider-to-server mapping |
| PRD-034 | Internal PRD | Implemented | Secret scanner; `internal/credentials` must pass scanning |
| PRD-028 | Internal PRD | Proposed | Sandbox subprocess isolation; entity credential `exec.Cmd.Env` entries must not leak across sandbox boundaries |
| GitHub OAuth App | External service | n/a | Client ID for GitHub OAuth device flow (operator must register) |
| SQLite WAL mode | Runtime | SQLite ≥ 3.37 (bundled in `modernc.org/sqlite`) | WAL mode required for concurrent credential writes |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should `tag entity auth --oauth` for GitHub use GitHub OAuth App (requires client ID/secret) or GitHub's device flow via the GitHub CLI OAuth app (no registration needed)? The GitHub CLI app is usable for personal use but its terms of service prohibit use in third-party tools. | @sanskarpan | Before implementation kick-off |
| OQ-02 | Should the AES key in the keychain be per-machine (one key, all entities) or per-entity (separate key per `user_id`)? Per-machine is simpler and currently proposed; per-entity allows revoking a single entity's decryption capability without re-encrypting all others. | Security review | Before implementation kick-off |
| OQ-03 | When `TAG_CREDENTIAL_BACKEND=vault`, should the Vault path be configurable per-entity (`secret/tag/entities/<entity_id>/<provider>`) or global (`secret/tag/credentials`)? Per-entity paths enable Vault policy scoping but require more Vault ACL configuration by operators. | @sanskarpan | Before Vault backend implementation |
| OQ-04 | Should `tag entity auth --oauth` support the full authorization-code flow (requires a local redirect server on an ephemeral port via `net/http`) in addition to device-code? Authorization-code flow works in browsers without a separate device, but requires `localhost` redirect URI registration with each provider. | Product | Sprint 1 planning |
| OQ-05 | Should expired OAuth tokens be auto-refreshed transparently on `tag submit --entity` (current proposal), or should the CLI fail with an actionable error prompting the user to run `tag entity rotate`? Transparent refresh is more ergonomic but risks using an outdated scope set after the provider's policies change. | Product | Sprint 1 planning |
| OQ-06 | The `entity_credentials` table uses a `UNIQUE(entity_id, provider)` constraint, meaning only one connected account per provider per entity. Composio supports multiple `connected_account_id`s per toolkit per user. Should TAG support multiple accounts per provider (e.g., two GitHub orgs)? | Product | Sprint 2 planning |
| OQ-07 | Should `tag entity list` support pagination for deployments with >10,000 entities? The current design returns all entities in one query. A `--cursor` / `--limit` flag following the registry pagination pattern should be added if the target user count exceeds 10,000. | @sanskarpan | Before GA |
| OQ-08 | Should the `entities` table be exportable via `tag export` (if that command exists) and importable on another machine? Cross-machine entity migration would be useful for disaster recovery but requires re-encrypting with the destination machine's AES key. | @sanskarpan | Post-GA |

---

## 16. Complexity and Timeline

**Total estimated effort: L (2-4 weeks)**

### Phase 1 — Core credential storage (Days 1-5)

- Add `entities` and `entity_credentials` DDL to `internal/store/migrate_prd075.go`; call from `store.OpenDB()`.
- Implement `encrypt` / `decrypt` with `sync.Once` AES key management in `internal/credentials/crypto.go`.
- Implement `SQLiteBackend` (Get, Put, Delete, ListProviders) in `internal/credentials/sqlite_backend.go`.
- Implement `CredentialContext`, `EntityRecord`, and `Backend` interface in `internal/credentials/credentials.go`.
- Implement `buildEnvVars` and provider defaults registry in `internal/credentials/providers.go`.
- Implement `tag entity create`, `tag entity auth --token`, `tag entity list`, `tag entity show`, `tag entity revoke` subcommands in `internal/cli/entity.go`.
- Unit tests for encryption, structs, and backend CRUD.
- **Deliverable:** Static token storage and retrieval working end-to-end.

### Phase 2 — Run machinery integration (Days 6-9)

- Add `--entity` flag to the submit cobra command in `internal/cli/submit.go`.
- Implement `LoadEntityCredentials` and `GetBackend` in `internal/credentials/credentials.go`.
- Integrate credential injection into `cmdSubmit` via `exec.Cmd.Env` merge; never via prompt builder.
- Implement `WithEntityID` / `EntityIDFromContext` in `internal/credentials/context.go`.
- Add `AttrEntityID` / `AttrEntityProvider` constants to `internal/obs/semconv.go`; wire into span factory.
- Write `entity_id` to `runs.metadata_json` in the `internal/store` layer.
- Add `--entity` filter to `tag trace list` using `json_extract`.
- Integration tests for credential injection and span attribute presence.
- **Deliverable:** `tag submit --entity` works with static tokens; spans carry `entity.id`.

### Phase 3 — OAuth flows (Days 10-16)

- Implement `IsHeadless()` in `internal/credentials/oauth.go`.
- Implement `RunDeviceFlow()` with exponential backoff and 15-minute timeout.
- Implement OAuth AS metadata discovery chain (RFC 8414 + RFC 9470 PRM) via `net/http`.
- Implement authorization-code + PKCE flow with `net/http` local redirect server on ephemeral port (non-headless path).
- Implement token validation test calls per provider via `net/http`.
- Implement `tag entity rotate` with atomic SQLite transaction.
- Implement `attemptRefresh()` for expired OAuth tokens.
- OAuth flow tests with `net/http/httptest` mock servers.
- **Deliverable:** Full OAuth flow for GitHub and Google providers in both headed and headless modes.

### Phase 4 — Backend abstraction and hardening (Days 17-21)

- Implement `VaultBackend` in `internal/credentials/vault.go` (build tag `vault`).
- Implement `AWSSsmBackend` in `internal/credentials/awsssm.go` (build tag `aws`).
- Wire `TAG_CREDENTIAL_BACKEND` env var selection in `GetBackend`.
- Add `PRAGMA secure_delete = ON` on revoke DB connections.
- Add shell history warning for `--token` CLI usage.
- Security review of `internal/credentials` against PRD-034 scanner.
- Benchmark suite in `internal/credentials/bench_test.go` (1,000 entities, parallel loads).
- Update `go.mod` with optional build-tag extras for `vault` and `aws` backends.
- **Deliverable:** Full feature complete, all acceptance criteria passing, backend abstraction documented.
