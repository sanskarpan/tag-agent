# PRD-075: Per-User Entity-Scoped Multi-Tenant Tool Auth (`tag entity`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** L (2-4 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `mcp_auth.py + entity_credentials SQLite table`
**Depends on:** PRD-013 (agent tracing/observability), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-034 (secret scanning), PRD-014 (MCP server registry)
**Inspired by:** Composio entity-scoped auth, Arcade AI user scoping
**GitHub issue:** #346

---

## 1. Overview

TAG is primarily used as a single-user CLI tool: one developer, one set of tool credentials, one GitHub token, one Slack workspace. This model breaks down the moment a development team tries to build a multi-tenant product on top of TAG — for example, a SaaS platform where each end-user can ask an agent to open a GitHub PR in their own repository, post to their own Slack channel, or read their own Notion pages. In the current architecture, every agent run shares the same ambient credential set loaded from environment variables. There is no mechanism to scope tool access to a named end-user (entity), inject per-user credentials into a run without surfacing them to the LLM, or audit which entity triggered which tool call.

Per-User Entity-Scoped Multi-Tenant Tool Auth solves this by introducing the concept of an **entity** — a named end-user context (identified by a user-supplied `user_id` string) that carries its own credential map per provider. When a caller submits a task with `--entity user-42`, TAG's run context loader resolves that entity's credentials from the `entity_credentials` SQLite table, injects them as environment overrides into the MCP server subprocess environment, and scopes all tracing spans with an `entity.id` attribute. The LLM never receives token values directly; credentials are brokered at the session layer before the first tool call is issued, following the Composio brokered-credential model exactly.

This feature makes TAG viable as the agentic backend for multi-tenant B2B products. A SaaS company can call `tag submit --entity <their_user_id> --prompt "..."` for each of their end-users, confident that user-42's GitHub token will never contaminate user-43's tool calls, that every credential is stored encrypted at rest in the local SQLite database, and that the audit log can be filtered by entity. The entity model is intentionally simple: no RBAC, no OAuth server, no web dashboard. It is a thin credential-routing layer that sits below the existing TAG run machinery and above the MCP server process lifecycle.

The design is directly inspired by Composio's `entity.initiate_connection()` pattern and Arcade AI's `user_id`-scoped tool execution. The key difference from those platforms is that TAG's entity system is entirely local — credentials are stored in the user's own SQLite database rather than a third-party vault — and the API surface is a plain CLI rather than a hosted API. Operators who need cloud-hosted credential storage can adapt the `CredentialBackend` abstraction introduced here to delegate to a secrets manager (AWS Secrets Manager, HashiCorp Vault) without changing the CLI surface.

The `tag entity` command cluster covers the full lifecycle: creating an entity record, associating one or more provider credentials per entity, listing entities with their connection status, revoking credentials, and running agent tasks scoped to a specific entity. The `entity_credentials` SQLite table is the single source of truth, encrypted at the column level using a key derived from the user's machine keychain. All reads and writes go through the `mcp_auth.py` module, which also handles automatic refresh for OAuth-style tokens and exposes a `CredentialContext` dataclass that the run machinery consumes.

---

## 2. Problem Statement

### 2.1 No isolation between end-users in multi-tenant agentic products

When a development team builds a SaaS product that calls `tag submit` on behalf of their end-users, every call shares the same set of ambient credentials loaded from the shell environment or the TAG config file. If user-42 grants the product access to their GitHub account and user-43 grants access to theirs, there is currently no way to tell TAG "use user-42's GitHub token for this request and user-43's token for that one." The only workaround today is to maintain separate TAG installations (separate `HERMES_HOME` directories, separate SQLite databases) per end-user, which is operationally infeasible for products with more than a handful of users.

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
| G4 | The `mcp_auth.py` module provides a `CredentialContext` dataclass and `load_entity_credentials(entity_id, provider)` function that the run machinery calls to fetch the correct token before spawning an MCP server subprocess. |
| G5 | All tracing spans produced during an entity-scoped run carry an `entity.id` attribute, enabling per-entity audit queries via `tag trace list --entity <user_id>`. |
| G6 | `tag entity list --json` outputs a machine-readable list of all entities with their provider connection statuses (connected, expired, missing) without revealing token values. |
| G7 | `tag entity revoke --id <user_id> --provider <slug>` deletes the credential for that entity+provider pair from the database, zeroing the encrypted column before deletion. |
| G8 | Credentials are encrypted at rest using AES-256-GCM with a key stored in the OS keychain (via the `keyring` library); they are never stored in plaintext in the SQLite database or any log file. |
| G9 | `tag entity auth` supports both static tokens (GitHub PAT, Slack bot token) and OAuth 2.1 authorization-code flows with PKCE, auto-selecting device-code flow when the terminal is headless. |
| G10 | A `CredentialBackend` abstract base class allows operators to substitute cloud secrets managers (AWS Secrets Manager, HashiCorp Vault) in place of the default SQLite+keychain backend without changing the CLI surface. |
| G11 | The `entity_credentials` table migration runs automatically inside `open_db()` on first use, following the existing migration pattern. |
| G12 | A connected-account cache `(entity_id, provider) → CredentialContext` is maintained in-process for the lifetime of a single `tag submit` call, eliminating redundant decrypt operations for multi-step runs that call the same provider's tools repeatedly. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Hosted credential storage or a cloud vault API. All credentials remain on the user's local machine in the SQLite database and OS keychain unless the operator explicitly swaps in a cloud backend via `CredentialBackend`. |
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
| Credential inject latency | Per-entity credential load adds < 5 ms overhead to `tag submit` startup | Profiled with `cProfile` on the `load_entity_credentials` call path |
| Zero token leakage | No entity credential value appears in any span attribute, log line, or prompt text during an instrumented run | Automated scan of all `spans` rows and Hermes log output after a test run |
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
- `--scopes`: Comma-separated OAuth scopes to request. Ignored for static tokens. Defaults to the provider's recommended scope set defined in `PROVIDER_DEFAULTS`.
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

Performs an OAuth flow identical to `tag entity auth --oauth`, but writes the new credential only after the flow succeeds and deletes the old credential atomically in a single SQLite transaction. Ongoing runs using the old credential are not interrupted (they hold their own in-process copy of the `CredentialContext`).

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

**Behavior:** Before spawning any MCP server subprocesses, `load_entity_credentials(entity_id)` is called. The resulting `CredentialContext` map is merged into the per-subprocess environment overrides dictionary maintained by the run machinery. The entity ID is recorded in the `runs` table's `metadata_json` column and propagated to all `spans` via the `entity.id` attribute.

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
| FR-05 | `tag entity auth --env-var <VAR>` MUST read the token from the named environment variable and immediately clear the variable from `os.environ` after reading, so it is not inherited by any subprocess. |
| FR-06 | `tag entity auth --oauth` MUST implement the full OAuth 2.1 discovery chain: HTTP 401 → Protected Resource Metadata → Authorization Server Metadata → dynamic client registration (if needed) → PKCE authorization-code or device-code flow. |
| FR-07 | `tag entity auth --oauth` MUST include the `resource` parameter (RFC 8707) in both the authorization request and the token exchange request. |
| FR-08 | `tag entity auth --oauth` MUST auto-detect headless environments by checking for the absence of `DISPLAY` environment variable and the presence of `SSH_CLIENT`; in headless mode it MUST automatically use device-code flow without requiring `--device-flow`. |
| FR-09 | Device-code polling MUST implement exponential backoff starting at 5 seconds and doubling on `slow_down` responses, capped at 30 seconds, for up to 15 minutes before timing out with exit code 2. |
| FR-10 | The encryption key used for `entity_credentials` MUST be derived from a key stored in the OS keychain via the `keyring` library under service name `tag.entity.credentials` and account name `aes256gcm.key`. If no key exists, one MUST be generated via `os.urandom(32)` and stored in the keychain on first use. |
| FR-11 | `load_entity_credentials(entity_id, provider=None)` in `mcp_auth.py` MUST return a `CredentialContext` object (or a dict of provider → `CredentialContext` if `provider` is `None`) without blocking the event loop; decryption MUST complete synchronously in < 5 ms per credential. |
| FR-12 | The run machinery (`controller.py::cmd_submit`) MUST call `load_entity_credentials(entity_id)` before constructing any MCP server `subprocess.Popen` call and inject the resolved environment variables into the `env` dict passed to `Popen`. The credential values MUST NOT appear in the `prompt` argument or any Hermes API call. |
| FR-13 | Every `spans` row produced during an entity-scoped run MUST have `entity.id` set in its `attributes` JSON column. |
| FR-14 | Every `runs` row produced during an entity-scoped run MUST have `entity_id` set in its `metadata_json` column. |
| FR-15 | `tag entity revoke` MUST zero the `credential_enc` column (write 32 zero bytes) before deleting the row, to reduce residual data exposure in SQLite's free-list pages. |
| FR-16 | `tag entity list` MUST compute `status` as: `connected` (token present and not expired), `expired` (token present but `expires_at < now()`), `missing` (no credential row). It MUST NOT attempt a live API call to verify token validity unless `--verify` flag is passed. |
| FR-17 | `tag entity auth` MUST store `scopes` as a comma-separated string in the `entity_credentials` table and return them in `tag entity list --json` output. |
| FR-18 | `tag entity rotate` MUST complete the new OAuth flow and write the new credential in the same SQLite transaction that deletes the old credential, ensuring no window where the entity has zero credentials. |
| FR-19 | A `CredentialBackend` abstract base class MUST be defined in `mcp_auth.py` with `get(entity_id, provider)`, `put(entity_id, provider, context)`, `delete(entity_id, provider)`, and `list_providers(entity_id)` methods. The default implementation MUST use the SQLite + keychain backend. |
| FR-20 | `TAG_CREDENTIAL_BACKEND` environment variable MUST be inspected at import time of `mcp_auth.py` to select the active backend implementation. Supported values: `sqlite` (default), `vault` (HashiCorp Vault via `hvac`), `aws-ssm` (AWS SSM Parameter Store via `boto3`). |
| FR-21 | `tag entity auth` MUST validate the stored token by making a provider-specific test API call (e.g., `GET /user` for GitHub) before confirming storage, unless `--skip-verify` is passed. |
| FR-22 | The `entity_credentials` migration MUST run inside `open_db()` using the existing `conn.executescript()` pattern, adding `entities` and `entity_credentials` tables only if they do not exist. |
| FR-23 | Concurrent `tag submit --entity` calls with different entity IDs MUST NOT share in-process `CredentialContext` cache entries; the cache MUST be keyed by `(entity_id, provider)` and scoped to the invocation, not the process. |
| FR-24 | `tag entity show` MUST display `last_used` timestamp (sourced from the `last_used_at` column updated on each `load_entity_credentials` call) without revealing the credential value. |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|------------|--------|
| NFR-01 | Credential encryption/decryption latency | < 5 ms per credential on a commodity laptop (M2 MacBook Pro baseline) |
| NFR-02 | `tag entity list` latency with 10,000 entities | < 500 ms (index scan on `entities.id`) |
| NFR-03 | Memory footprint of `CredentialContext` cache | < 1 MB for 1,000 simultaneously cached entities |
| NFR-04 | Secret-scan compliance | The `mcp_auth.py` module MUST pass PRD-034 secret scanning with zero findings (no high-entropy strings in source code) |
| NFR-05 | Keychain access latency | The AES key retrieval from the OS keychain MUST be cached in-process after the first read to avoid repeated keychain round-trips; the cached key MUST be stored as a `bytes` object in a module-level `_KEY_CACHE` variable with a `threading.Lock` |
| NFR-06 | SQLite WAL compatibility | All `entity_credentials` writes MUST use WAL-mode transactions; no exclusive locks that could block concurrent `tag submit` calls |
| NFR-07 | Python version compatibility | `mcp_auth.py` MUST support Python 3.11+ with no `walrus operator` usage for compatibility with TAG's minimum Python version |
| NFR-08 | Dependency footprint | New hard dependencies are limited to `keyring` (already used in TAG) and `cryptography` (AES-GCM). OAuth MUST use `httpx` (already a TAG dependency). `hvac` and `boto3` are optional extras. |
| NFR-09 | CLI help completeness | Every `tag entity` subcommand MUST have a `--help` string that names the backing SQLite table and the encryption method used |
| NFR-10 | Audit log retention | `entity_credentials` rows MUST include `created_at` and `last_used_at` columns; `tag entity list` MUST expose `last_used` to support SCIM-style dormant-user detection |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/mcp_auth.py` | Core module: `CredentialContext` dataclass, `CredentialBackend` ABC, `SQLiteCredentialBackend`, `load_entity_credentials()`, `store_entity_credential()`, `revoke_entity_credential()`, OAuth flow orchestration (`_run_oauth_flow`, `_run_device_flow`), headless detection, provider defaults registry |
| `src/tag/integrations/vault_backend.py` | Optional `VaultCredentialBackend` using `hvac`; loaded only when `TAG_CREDENTIAL_BACKEND=vault` |
| `src/tag/integrations/aws_ssm_backend.py` | Optional `AwsSsmCredentialBackend` using `boto3`; loaded only when `TAG_CREDENTIAL_BACKEND=aws-ssm` |
| `tests/test_mcp_auth.py` | Unit tests for encryption round-trip, `CredentialContext` serialization, headless detection, status computation |
| `tests/test_entity_commands.py` | Integration tests for all `tag entity` subcommands against a real in-memory SQLite DB |

### 10.2 SQLite DDL

The following DDL is added to the `conn.executescript()` call inside `open_db()` in `controller.py`:

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
CREATE INDEX IF NOT EXISTS idx_ec_entity ON entity_credentials(entity_id);
CREATE INDEX IF NOT EXISTS idx_ec_provider ON entity_credentials(provider);
CREATE INDEX IF NOT EXISTS idx_ec_expires ON entity_credentials(expires_at)
  WHERE expires_at IS NOT NULL;
```

**Migration guard** (added to `_migrate_prd_021_032_tables` pattern):

```python
def _migrate_prd_075_entity_tables(conn: sqlite3.Connection) -> None:
    """PRD-075: Add entity and entity_credentials tables if absent."""
    existing = {row[0] for row in conn.execute(
        "SELECT name FROM sqlite_master WHERE type='table'"
    )}
    if "entities" not in existing or "entity_credentials" not in existing:
        conn.executescript(_PRD_075_DDL)
```

### 10.3 Core Dataclasses

```python
# src/tag/mcp_auth.py
from __future__ import annotations

import abc
import base64
import os
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Optional

from cryptography.hazmat.primitives.ciphers.aead import AESGCM


@dataclass(frozen=True)
class CredentialContext:
    """Resolved credential for one entity+provider pair.

    The ``env_vars`` dict maps environment variable names to their
    plaintext token values.  These are injected into MCP server
    subprocess environments only — they are never passed to the LLM.

    Example for provider='github':
        env_vars = {"GITHUB_TOKEN": "ghp_xxxxx"}

    Example for provider='google-drive':
        env_vars = {
            "GOOGLE_ACCESS_TOKEN": "ya29.xxxxx",
            "GOOGLE_REFRESH_TOKEN": "1//xxxxx",
        }
    """
    entity_id: str
    provider: str
    token_type: str            # 'bearer' | 'basic' | 'apikey'
    scopes: list[str]
    expires_at: Optional[datetime]
    env_vars: dict[str, str]   # env var name → plaintext value
    provider_meta: dict        # provider-specific extras (never logged)

    @property
    def is_expired(self) -> bool:
        if self.expires_at is None:
            return False
        return datetime.now(tz=timezone.utc) >= self.expires_at

    @property
    def status(self) -> str:  # 'connected' | 'expired'
        return "expired" if self.is_expired else "connected"


@dataclass
class EntityRecord:
    id: str
    description: str
    created_at: datetime
    updated_at: datetime
    providers: list[str] = field(default_factory=list)
```

### 10.4 Encryption Utilities

```python
# src/tag/mcp_auth.py (continued)

import keyring

_KEY_CACHE: Optional[bytes] = None
_KEY_LOCK = threading.Lock()

_KEYCHAIN_SERVICE = "tag.entity.credentials"
_KEYCHAIN_ACCOUNT = "aes256gcm.key"


def _get_or_create_aes_key() -> bytes:
    """Return the 32-byte AES key, generating and storing it if absent."""
    global _KEY_CACHE
    with _KEY_LOCK:
        if _KEY_CACHE is not None:
            return _KEY_CACHE
        raw = keyring.get_password(_KEYCHAIN_SERVICE, _KEYCHAIN_ACCOUNT)
        if raw is None:
            key = os.urandom(32)
            keyring.set_password(
                _KEYCHAIN_SERVICE,
                _KEYCHAIN_ACCOUNT,
                base64.b64encode(key).decode(),
            )
        else:
            key = base64.b64decode(raw)
        _KEY_CACHE = key
        return key


def _encrypt(plaintext: str) -> str:
    """AES-256-GCM encrypt; return base64url(nonce[12] || ct || tag[16])."""
    key = _get_or_create_aes_key()
    nonce = os.urandom(12)
    ct_and_tag = AESGCM(key).encrypt(nonce, plaintext.encode(), None)
    return base64.urlsafe_b64encode(nonce + ct_and_tag).decode()


def _decrypt(encoded: str) -> str:
    """AES-256-GCM decrypt from base64url blob produced by _encrypt."""
    key = _get_or_create_aes_key()
    raw = base64.urlsafe_b64decode(encoded)
    nonce, ct_and_tag = raw[:12], raw[12:]
    return AESGCM(key).decrypt(nonce, ct_and_tag, None).decode()
```

### 10.5 CredentialBackend ABC

```python
# src/tag/mcp_auth.py (continued)

class CredentialBackend(abc.ABC):
    """Abstract credential storage backend.

    Operators can substitute a cloud backend (Vault, AWS SSM) by
    setting TAG_CREDENTIAL_BACKEND and implementing this interface.
    """

    @abc.abstractmethod
    def get(self, entity_id: str, provider: str) -> Optional[CredentialContext]:
        """Return the credential for entity+provider, or None if absent."""

    @abc.abstractmethod
    def put(self, entity_id: str, provider: str,
            context: CredentialContext) -> None:
        """Upsert a credential for entity+provider."""

    @abc.abstractmethod
    def delete(self, entity_id: str, provider: str) -> None:
        """Zero the credential bytes, then delete the record."""

    @abc.abstractmethod
    def list_providers(self, entity_id: str) -> list[CredentialContext]:
        """Return all credential contexts for the given entity."""
```

### 10.6 SQLiteCredentialBackend

```python
class SQLiteCredentialBackend(CredentialBackend):
    """Default backend: AES-256-GCM encrypted values in tag.sqlite3."""

    def __init__(self, cfg: dict) -> None:
        self._cfg = cfg

    def get(self, entity_id: str, provider: str) -> Optional[CredentialContext]:
        import json
        from tag.controller import open_db
        conn = open_db(self._cfg)
        row = conn.execute(
            """
            SELECT credential_enc, refresh_enc, token_type, scopes,
                   expires_at, provider_meta
            FROM entity_credentials
            WHERE entity_id = ? AND provider = ?
            """,
            (entity_id, provider),
        ).fetchone()
        if row is None:
            return None
        # Update last_used_at
        conn.execute(
            "UPDATE entity_credentials SET last_used_at = ? "
            "WHERE entity_id = ? AND provider = ?",
            (datetime.now(tz=timezone.utc).isoformat(), entity_id, provider),
        )
        conn.commit()
        token = _decrypt(row["credential_enc"])
        refresh = _decrypt(row["refresh_enc"]) if row["refresh_enc"] else None
        meta = json.loads(row["provider_meta"] or "{}")
        expires = (
            datetime.fromisoformat(row["expires_at"])
            if row["expires_at"] else None
        )
        env_vars = _build_env_vars(provider, token, refresh, meta)
        return CredentialContext(
            entity_id=entity_id,
            provider=provider,
            token_type=row["token_type"],
            scopes=[s for s in (row["scopes"] or "").split(",") if s],
            expires_at=expires,
            env_vars=env_vars,
            provider_meta=meta,
        )

    def put(self, entity_id: str, provider: str,
            context: CredentialContext) -> None:
        import json
        from tag.controller import open_db
        conn = open_db(self._cfg)
        token_plain = context.env_vars.get(
            _PROVIDER_PRIMARY_ENV_VAR.get(provider, "TOKEN"), ""
        )
        refresh_plain = context.env_vars.get(
            _PROVIDER_REFRESH_ENV_VAR.get(provider, ""), ""
        )
        now = datetime.now(tz=timezone.utc).isoformat()
        conn.execute(
            """
            INSERT INTO entity_credentials
              (entity_id, provider, credential_enc, refresh_enc,
               token_type, scopes, expires_at, provider_meta,
               created_at, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(entity_id, provider) DO UPDATE SET
              credential_enc = excluded.credential_enc,
              refresh_enc    = excluded.refresh_enc,
              token_type     = excluded.token_type,
              scopes         = excluded.scopes,
              expires_at     = excluded.expires_at,
              provider_meta  = excluded.provider_meta,
              updated_at     = excluded.updated_at
            """,
            (
                entity_id,
                provider,
                _encrypt(token_plain),
                _encrypt(refresh_plain) if refresh_plain else None,
                context.token_type,
                ",".join(context.scopes),
                context.expires_at.isoformat() if context.expires_at else None,
                json.dumps(context.provider_meta),
                now,
                now,
            ),
        )
        conn.commit()

    def delete(self, entity_id: str, provider: str) -> None:
        from tag.controller import open_db
        conn = open_db(self._cfg)
        # Zero the encrypted column before deletion
        zeroed = base64.urlsafe_b64encode(bytes(44)).decode()
        conn.execute(
            "UPDATE entity_credentials SET credential_enc = ?, refresh_enc = NULL "
            "WHERE entity_id = ? AND provider = ?",
            (zeroed, entity_id, provider),
        )
        conn.execute(
            "DELETE FROM entity_credentials WHERE entity_id = ? AND provider = ?",
            (entity_id, provider),
        )
        conn.commit()

    def list_providers(self, entity_id: str) -> list[CredentialContext]:
        from tag.controller import open_db
        conn = open_db(self._cfg)
        rows = conn.execute(
            "SELECT provider FROM entity_credentials WHERE entity_id = ?",
            (entity_id,),
        ).fetchall()
        return [
            self.get(entity_id, row["provider"])
            for row in rows
            if self.get(entity_id, row["provider"]) is not None
        ]
```

### 10.7 Provider Defaults Registry

```python
# src/tag/mcp_auth.py (continued)

# Maps provider slug → primary env var name injected into MCP subprocess
_PROVIDER_PRIMARY_ENV_VAR: dict[str, str] = {
    "github":           "GITHUB_TOKEN",
    "gitlab":           "GITLAB_TOKEN",
    "bitbucket":        "BITBUCKET_TOKEN",
    "slack":            "SLACK_BOT_TOKEN",
    "notion":           "NOTION_TOKEN",
    "linear":           "LINEAR_API_KEY",
    "jira":             "JIRA_API_TOKEN",
    "google-drive":     "GOOGLE_ACCESS_TOKEN",
    "google-calendar":  "GOOGLE_ACCESS_TOKEN",
    "gmail":            "GOOGLE_ACCESS_TOKEN",
}

# Maps provider slug → refresh token env var (None if not applicable)
_PROVIDER_REFRESH_ENV_VAR: dict[str, str] = {
    "google-drive":     "GOOGLE_REFRESH_TOKEN",
    "google-calendar":  "GOOGLE_REFRESH_TOKEN",
    "gmail":            "GOOGLE_REFRESH_TOKEN",
}

# Maps provider slug → default OAuth scopes to request
PROVIDER_DEFAULT_SCOPES: dict[str, list[str]] = {
    "github":           ["repo", "read:user", "read:org"],
    "gitlab":           ["api", "read_user"],
    "slack":            ["channels:read", "chat:write", "users:read"],
    "notion":           ["read_content", "update_content"],
    "linear":           ["read", "write"],
    "google-drive":     ["https://www.googleapis.com/auth/drive"],
    "google-calendar":  ["https://www.googleapis.com/auth/calendar"],
    "gmail":            ["https://mail.google.com/"],
}

# Maps provider slug → OAuth authorization server metadata URL
# Used as the starting point for RFC 8414 AS metadata discovery
PROVIDER_AS_METADATA_URL: dict[str, str] = {
    "github":           "https://github.com/.well-known/oauth-authorization-server",
    "gitlab":           "https://gitlab.com/.well-known/oauth-authorization-server",
    "google-drive":     "https://accounts.google.com/.well-known/openid-configuration",
    "google-calendar":  "https://accounts.google.com/.well-known/openid-configuration",
    "gmail":            "https://accounts.google.com/.well-known/openid-configuration",
}


def _build_env_vars(
    provider: str,
    token: str,
    refresh: Optional[str],
    meta: dict,
) -> dict[str, str]:
    """Build the subprocess env var dict for a given provider's credential."""
    result: dict[str, str] = {}
    primary_var = _PROVIDER_PRIMARY_ENV_VAR.get(provider, f"{provider.upper()}_TOKEN")
    result[primary_var] = token
    refresh_var = _PROVIDER_REFRESH_ENV_VAR.get(provider)
    if refresh_var and refresh:
        result[refresh_var] = refresh
    # Provider-specific extras from meta
    if provider == "github" and "installation_id" in meta:
        result["GITHUB_INSTALLATION_ID"] = str(meta["installation_id"])
    return result
```

### 10.8 Public API Functions

```python
# src/tag/mcp_auth.py (continued)

# Module-level backend singleton (selected by TAG_CREDENTIAL_BACKEND env var)
_BACKEND: Optional[CredentialBackend] = None
_BACKEND_LOCK = threading.Lock()


def _get_backend(cfg: dict) -> CredentialBackend:
    global _BACKEND
    with _BACKEND_LOCK:
        if _BACKEND is not None:
            return _BACKEND
        backend_name = os.environ.get("TAG_CREDENTIAL_BACKEND", "sqlite")
        if backend_name == "vault":
            from tag.integrations.vault_backend import VaultCredentialBackend
            _BACKEND = VaultCredentialBackend(cfg)
        elif backend_name == "aws-ssm":
            from tag.integrations.aws_ssm_backend import AwsSsmCredentialBackend
            _BACKEND = AwsSsmCredentialBackend(cfg)
        else:
            _BACKEND = SQLiteCredentialBackend(cfg)
        return _BACKEND


def load_entity_credentials(
    cfg: dict,
    entity_id: str,
    provider: Optional[str] = None,
) -> dict[str, CredentialContext]:
    """Load credentials for entity_id, optionally filtered by provider.

    Returns a dict mapping provider slug → CredentialContext.
    Raises EntityNotFoundError if entity_id does not exist in the DB.
    Raises CredentialExpiredError if a credential is expired and no
    refresh token is available.
    """
    backend = _get_backend(cfg)
    if provider is not None:
        ctx = backend.get(entity_id, provider)
        if ctx is None:
            return {}
        if ctx.is_expired:
            refreshed = _attempt_refresh(cfg, entity_id, provider, ctx)
            if refreshed is not None:
                return {provider: refreshed}
        return {provider: ctx}
    # Load all providers for this entity
    contexts = backend.list_providers(entity_id)
    result: dict[str, CredentialContext] = {}
    for ctx in contexts:
        if ctx.is_expired:
            refreshed = _attempt_refresh(cfg, entity_id, ctx.provider, ctx)
            result[ctx.provider] = refreshed if refreshed is not None else ctx
        else:
            result[ctx.provider] = ctx
    return result


class EntityNotFoundError(Exception):
    pass


class CredentialExpiredError(Exception):
    pass
```

### 10.9 OAuth Device-Code Flow

```python
# src/tag/mcp_auth.py (continued)

import httpx


def _is_headless() -> bool:
    """True when running without a graphical display (CI, SSH, Docker)."""
    if os.environ.get("SSH_CLIENT") or os.environ.get("SSH_TTY"):
        return True
    if not os.environ.get("DISPLAY") and not os.environ.get("WAYLAND_DISPLAY"):
        import sys
        return not sys.stdout.isatty()
    return False


def _run_device_flow(
    provider: str,
    scopes: list[str],
    as_metadata: dict,
) -> tuple[str, Optional[str], Optional[datetime]]:
    """Execute RFC 8628 device authorization flow.

    Returns (access_token, refresh_token, expires_at).
    Raises OAuthTimeoutError after 15 minutes.
    """
    device_endpoint = as_metadata.get("device_authorization_endpoint")
    if not device_endpoint:
        raise OAuthFlowError(
            f"Provider {provider!r} does not support device-code flow."
        )
    client_id = os.environ.get(
        f"TAG_{provider.upper().replace('-', '_')}_CLIENT_ID",
        _PROVIDER_DEFAULT_CLIENT_IDS.get(provider, ""),
    )
    resp = httpx.post(device_endpoint, data={
        "client_id": client_id,
        "scope": " ".join(scopes),
    }, timeout=10)
    resp.raise_for_status()
    data = resp.json()

    device_code = data["device_code"]
    user_code = data["user_code"]
    verification_uri = data["verification_uri"]
    interval = int(data.get("interval", 5))
    expires_in = int(data.get("expires_in", 900))

    print(f"\n  Authorization URL: {verification_uri}")
    print(f"  User code:        {user_code}")
    print(f"  Expires in:       {expires_in} seconds\n")
    print("Waiting for authorization...", flush=True)

    token_endpoint = as_metadata["token_endpoint"]
    deadline = time.monotonic() + min(expires_in, 900)
    while time.monotonic() < deadline:
        time.sleep(interval)
        token_resp = httpx.post(token_endpoint, data={
            "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
            "device_code": device_code,
            "client_id": client_id,
        }, timeout=10)
        token_data = token_resp.json()
        error = token_data.get("error")
        if error == "authorization_pending":
            continue
        elif error == "slow_down":
            interval = min(interval * 2, 30)
            continue
        elif error == "expired_token":
            raise OAuthTimeoutError("Device code expired before authorization.")
        elif error == "access_denied":
            raise OAuthFlowError("User denied authorization.")
        elif error:
            raise OAuthFlowError(f"OAuth error: {error}")
        access_token = token_data["access_token"]
        refresh_token = token_data.get("refresh_token")
        expires_in_tok = token_data.get("expires_in")
        expires_at = (
            datetime.now(tz=timezone.utc).replace(microsecond=0)
            + __import__("datetime").timedelta(seconds=expires_in_tok)
            if expires_in_tok else None
        )
        return access_token, refresh_token, expires_at
    raise OAuthTimeoutError("Device flow timed out after 15 minutes.")


class OAuthFlowError(Exception):
    pass


class OAuthTimeoutError(Exception):
    pass
```

### 10.10 Run Machinery Integration

The integration point in `controller.py` is `cmd_submit`. The following pseudocode shows the injection site:

```python
# controller.py — cmd_submit (addition for PRD-075)

def cmd_submit(args, cfg):
    entity_id: Optional[str] = getattr(args, "entity", None)
    entity_creds: dict[str, CredentialContext] = {}

    if entity_id:
        from tag.mcp_auth import load_entity_credentials, EntityNotFoundError
        try:
            entity_creds = load_entity_credentials(cfg, entity_id)
        except EntityNotFoundError:
            print_error(f"Entity {entity_id!r} not found. "
                        f"Run: tag entity create --id {entity_id}")
            raise SystemExit(1)

    # Build per-provider env var overrides for MCP subprocesses
    mcp_env_overrides: dict[str, str] = {}
    for ctx in entity_creds.values():
        mcp_env_overrides.update(ctx.env_vars)

    # Inject into the run context object passed to hermes_bridge
    run_meta = {
        "entity_id": entity_id,
        **existing_metadata,
    }

    # Span attribute injection (called in the span factory)
    span_attrs_extra = {"entity.id": entity_id} if entity_id else {}

    # ... existing submit logic ...
    # When spawning MCP server subprocesses:
    #   env = {**os.environ, **mcp_env_overrides}
    # mcp_env_overrides is NEVER passed to the LLM prompt builder
```

### 10.11 Tracing Integration

The `entity.id` span attribute follows the OTel semantic convention namespace used in `otel_semconv.py`:

```python
# src/tag/otel_semconv.py — additions for PRD-075
ENTITY_ID = "entity.id"           # string: the entity user_id
ENTITY_PROVIDER = "entity.provider"  # string: provider slug used in this span
```

---

## 11. Security Considerations

1. **Token never in LLM context.** The `env_vars` dict from `CredentialContext` is injected exclusively into the `env` argument of `subprocess.Popen` when spawning an MCP server subprocess. It is never concatenated into a prompt string, stored in the `prompt` column of the `runs` table, or passed to any Hermes API call body. This is enforced by code review gate: any PR touching `cmd_submit` that adds `entity_creds` to a string-format expression must be rejected.

2. **AES-256-GCM encryption at rest.** Each credential value is encrypted with a unique 12-byte random nonce using AES-256-GCM before being stored in the `credential_enc` column. The 32-byte AES key is stored in the OS keychain (macOS Keychain, Linux Secret Service via D-Bus, Windows Credential Manager) and never written to disk in plaintext. The nonce is stored prepended to the ciphertext blob, making each encryption operation non-deterministic.

3. **Keychain key compromise scope.** If the OS keychain is compromised, all entity credentials stored in the SQLite database are exposed. This is an accepted residual risk for a local-first tool. Operators deploying TAG in production multi-tenant environments MUST use the `vault` or `aws-ssm` backend where the AES key is never stored on the host machine.

4. **Shell history exposure.** `tag entity auth --token <tok>` accepts a token on the command line, which appears in shell history. The `--env-var <VAR>` flag is the recommended alternative for production use. The CLI MUST print a warning when `--token` is used directly: `Warning: token passed via CLI argument will appear in shell history. Prefer --env-var.`

5. **SQLite free-list residual data.** SQLite's WAL mode does not guarantee that deleted row data is immediately overwritten on disk. `tag entity revoke` MUST first update the `credential_enc` column to 44 zero bytes (the length of a base64-encoded 32-byte value) before issuing the `DELETE`, minimizing the window during which a forensic read of the SQLite file could recover the ciphertext. `PRAGMA secure_delete = ON` is set on connections that issue `revoke` operations.

6. **Concurrent process credential isolation.** The in-process `CredentialContext` cache is scoped to the invocation, not the module. This is enforced by passing `cfg` (which includes the run ID) as a cache key rather than using a global dict. Each `tag submit` call loads its own copy of the credentials and does not share the cache with concurrent calls in a queue-worker scenario.

7. **OAuth state parameter CSRF protection.** The authorization-code flow (non-device-code path) MUST generate a cryptographically random `state` parameter via `secrets.token_urlsafe(32)` and verify it on the redirect callback. The PKCE `code_verifier` MUST be generated via `secrets.token_urlsafe(64)` and the `code_challenge` MUST use S256 (SHA-256 + base64url, no padding).

8. **Token validation before storage.** `tag entity auth` MUST make a provider-specific test API call after receiving the token and before writing it to the database. For GitHub: `GET https://api.github.com/user` with `Authorization: token <tok>`. If the call returns a non-2xx response, the token is discarded without being written to the database, and the CLI exits with code 3.

9. **Secret scanner integration (PRD-034).** `mcp_auth.py` exports a `_decrypt` function that returns plaintext token values. The PRD-034 secret scanner MUST be extended to skip `entity_credentials` rows (since the values are intentionally high-entropy) but MUST scan all other columns and all log files for accidental credential leakage.

10. **Refresh token rotation.** When an OAuth refresh token is used to obtain a new access token, the new access token and (if provided) new refresh token MUST be written to the database atomically before the old values are discarded. This prevents a state where the old refresh token has been consumed and the new tokens are lost due to a crash.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_mcp_auth.py`)

- **Encryption round-trip:** Assert that `_decrypt(_encrypt(plaintext)) == plaintext` for 100 random inputs including Unicode strings and high-entropy byte sequences.
- **Nonce uniqueness:** Assert that 1,000 calls to `_encrypt("x")` produce 1,000 distinct ciphertext blobs.
- **Keychain mock:** Patch `keyring.get_password` and `keyring.set_password` to avoid touching the actual OS keychain in CI; assert that the first call to `_get_or_create_aes_key()` generates and stores a 32-byte key.
- **`CredentialContext.is_expired`:** Assert that a context with `expires_at = now() - 1s` returns `True` and one with `expires_at = now() + 1h` returns `False`.
- **`_is_headless()`:** Assert `True` when `DISPLAY` is unset and `SSH_CLIENT` is set; assert `False` when `DISPLAY=/tmp/.X11-unix/X0`.
- **`_build_env_vars` correctness:** Assert correct env var names for all built-in provider slugs.
- **Status computation:** Assert `status == "connected"` for non-expired, `"expired"` for expired, `"missing"` for absent.
- **`CredentialBackend` ABC:** Assert that `SQLiteCredentialBackend` cannot be instantiated without implementing all abstract methods (it can, since it does implement them).

### 12.2 Integration Tests (`tests/test_entity_commands.py`)

All integration tests use an in-memory SQLite database via a `tmp_path`-based `cfg` fixture that sets `TAG_DB_PATH` to a temporary file.

- **Full lifecycle:** `create` → `auth` → `list` → `show` → `revoke` → assert entity absent.
- **Duplicate ID rejection:** Assert exit code 1 on second `create` with same ID.
- **Invalid ID format:** Assert exit code 1 for IDs containing spaces, slashes, or exceeding 128 characters.
- **`--env-var` isolation:** Set `MY_TOKEN=abc123` in `os.environ`, call `tag entity auth --env-var MY_TOKEN`, assert `MY_TOKEN` is absent from `os.environ` after the call.
- **Credential injection into subprocess env:** Mock `subprocess.Popen` in `cmd_submit`, call `tag submit --entity user-42 --prompt "test"`, assert `GITHUB_TOKEN` is present in the `env` dict passed to `Popen` and absent from the prompt string.
- **Concurrent entity isolation:** Spawn two threads each calling `load_entity_credentials` with different entity IDs simultaneously; assert each thread receives its own entity's token value.
- **`tag trace list --entity`:** After running a submit with `--entity user-42`, query the spans table and assert `json_extract(attributes, '$.entity.id') = 'user-42'` for all spans.
- **Revoke zeroing:** After `tag entity revoke`, read the raw SQLite file with `sqlite3` module; assert the deleted row's content is not recoverable via `.fetchall()` on the freed pages (approximated by checking WAL checkpoint).
- **Token expiry refresh:** Insert a credential with `expires_at = 1 minute ago` and a `refresh_enc` set; mock the OAuth token endpoint; assert `load_entity_credentials` calls the refresh endpoint and updates the stored token.

### 12.3 OAuth Flow Tests

- **Device-code happy path:** Mock an HTTP server responding to `device_authorization_endpoint` and `token_endpoint` calls; assert the flow returns a valid `CredentialContext` after two polling cycles.
- **`slow_down` response:** Assert the polling interval doubles (capped at 30s) on a `slow_down` error response.
- **Timeout:** Assert `OAuthTimeoutError` is raised when the mock server returns `authorization_pending` for longer than the test's mocked `expires_in`.
- **Headless auto-selection:** Assert that `_run_oauth_flow` selects device-code path when `_is_headless()` returns `True` without requiring `--device-flow` flag.

### 12.4 Performance Tests

- **1,000 entities load time:** Insert 1,000 entities with 3 providers each; assert `tag entity list` completes in < 500 ms using `pytest-benchmark`.
- **Parallel credential loads:** Spawn 50 threads each calling `load_entity_credentials`; assert total wall time < 200 ms (exploiting the in-process key cache).

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag entity create --id user-42` inserts a row in the `entities` table and prints the entity ID without error. | `pytest tests/test_entity_commands.py::test_create_entity` |
| AC-02 | `tag entity create --id user-42` called a second time exits with code 1 and the message "Entity user-42 already exists." | `pytest tests/test_entity_commands.py::test_duplicate_entity` |
| AC-03 | `tag entity auth --id user-42 --provider github --token ghp_test123` stores an encrypted value in `entity_credentials`; `SELECT credential_enc FROM entity_credentials WHERE entity_id='user-42'` returns a non-plaintext blob. | `pytest tests/test_entity_commands.py::test_encrypt_at_rest` |
| AC-04 | `tag submit --entity user-42 --prompt "list my repos"` with a mocked GitHub MCP server asserts `GITHUB_TOKEN=ghp_test123` in the server's subprocess env. | `pytest tests/test_entity_commands.py::test_credential_injection` |
| AC-05 | After `tag submit --entity user-42`, `SELECT json_extract(attributes, '$.entity.id') FROM spans` returns `user-42` for all spans produced in that run. | `pytest tests/test_entity_commands.py::test_span_entity_attribute` |
| AC-06 | `tag entity list --json` returns a JSON array where each object has `id`, `created_at`, and `providers` array with `status` field; no object contains a `token` or `credential` field. | `pytest tests/test_entity_commands.py::test_list_no_token_leak` |
| AC-07 | `tag entity revoke --id user-42 --provider github` followed by `SELECT COUNT(*) FROM entity_credentials WHERE entity_id='user-42' AND provider='github'` returns 0. | `pytest tests/test_entity_commands.py::test_revoke` |
| AC-08 | In a headless environment (no `DISPLAY`, `SSH_CLIENT` set), `tag entity auth --id u1 --provider github --oauth` prints a device code URL and polls the token endpoint. | `pytest tests/test_mcp_auth.py::test_device_flow_headless_autoselect` |
| AC-09 | Two concurrent `tag submit --entity` calls with different entity IDs produce spans with distinct `entity.id` attributes and no cross-entity credential leakage. | `pytest tests/test_entity_commands.py::test_concurrent_isolation` |
| AC-10 | `tag entity auth --token ghp_...` prints a shell history warning to stderr. | `pytest tests/test_entity_commands.py::test_cli_token_history_warning` |
| AC-11 | `tag entity auth --env-var GITHUB_TOKEN` with `GITHUB_TOKEN=ghp_test` in env results in `GITHUB_TOKEN` being absent from `os.environ` after the call returns. | `pytest tests/test_mcp_auth.py::test_env_var_cleared_after_read` |
| AC-12 | Setting `TAG_CREDENTIAL_BACKEND=vault` and providing `VAULT_ADDR`/`VAULT_TOKEN` causes `load_entity_credentials` to call the Vault KV API instead of the SQLite backend. | `pytest tests/test_entity_commands.py::test_vault_backend` (requires mock Vault server) |
| AC-13 | `tag entity rotate --id user-42 --provider github` completes a new device-code flow and atomically replaces the existing credential; no window exists where the entity has zero credentials (verified by polling the DB in a background thread during the rotation). | `pytest tests/test_entity_commands.py::test_rotate_atomicity` |
| AC-14 | `tag trace list --entity user-42` returns only spans where `entity.id = 'user-42'` and correctly excludes spans from runs without an entity. | `pytest tests/test_entity_commands.py::test_trace_filter_by_entity` |
| AC-15 | `mcp_auth.py` passes `tag security scan` (PRD-034) with zero findings. | `pytest tests/test_security.py::test_mcp_auth_no_secrets` |

---

## 14. Dependencies

| Dependency | Type | Version Constraint | Reason |
|------------|------|--------------------|--------|
| `cryptography` | Python package (hard) | `>=42.0.0` | AES-256-GCM via `cryptography.hazmat.primitives.ciphers.aead.AESGCM` |
| `keyring` | Python package (hard) | `>=25.0.0` | OS keychain access for AES key storage |
| `httpx` | Python package (hard) | `>=0.27.0` | OAuth HTTP calls; already a TAG dependency |
| `hvac` | Python package (optional extra `[vault]`) | `>=2.0.0` | HashiCorp Vault backend |
| `boto3` | Python package (optional extra `[aws]`) | `>=1.34.0` | AWS SSM Parameter Store backend |
| PRD-013 | Internal PRD | Implemented | `spans` table with `attributes` JSON column for `entity.id` injection |
| PRD-014 | Internal PRD | Implemented | MCP server registry for provider-to-server mapping |
| PRD-034 | Internal PRD | Implemented | Secret scanner; `mcp_auth.py` must pass scanning |
| PRD-028 | Internal PRD | Proposed | Sandbox subprocess isolation; entity credential env vars must not leak across sandbox boundaries |
| GitHub OAuth App | External service | n/a | Client ID/secret for GitHub OAuth flow (operator must register) |
| SQLite WAL mode | Runtime | SQLite >= 3.37 | Already required by TAG; WAL mode must be active for concurrent credential writes |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should `tag entity auth --oauth` for GitHub use GitHub OAuth App (requires client ID/secret) or GitHub's device flow via the GitHub CLI OAuth app (no registration needed)? The GitHub CLI app is usable for personal use but its terms of service prohibit use in third-party tools. | @sanskarpan | Before implementation kick-off |
| OQ-02 | Should the AES key in the keychain be per-machine (one key, all entities) or per-entity (separate key per `user_id`)? Per-machine is simpler and currently proposed; per-entity allows revoking a single entity's decryption capability without re-encrypting all others. | Security review | Before implementation kick-off |
| OQ-03 | When `TAG_CREDENTIAL_BACKEND=vault`, should the Vault path be configurable per-entity (`secret/tag/entities/<entity_id>/<provider>`) or global (`secret/tag/credentials`)? Per-entity paths enable Vault policy scoping but require more Vault ACL configuration by operators. | @sanskarpan | Before Vault backend implementation |
| OQ-04 | Should `tag entity auth --oauth` support the full authorization-code flow (requires a local redirect server on an ephemeral port) in addition to device-code? Authorization-code flow works in browsers without a separate device, but requires `localhost` redirect URI registration with each provider. | Product | Sprint 1 planning |
| OQ-05 | Should expired OAuth tokens be auto-refreshed transparently on `tag submit --entity` (current proposal), or should the CLI fail with an actionable error prompting the user to run `tag entity rotate`? Transparent refresh is more ergonomic but risks using an outdated scope set after the provider's policies change. | Product | Sprint 1 planning |
| OQ-06 | The `entity_credentials` table uses a `UNIQUE(entity_id, provider)` constraint, meaning only one connected account per provider per entity. Composio supports multiple `connected_account_id`s per toolkit per user. Should TAG support multiple accounts per provider (e.g., two GitHub orgs)? | Product | Sprint 2 planning |
| OQ-07 | Should `tag entity list` support pagination for deployments with >10,000 entities? The current design returns all entities in one query. A `--cursor` / `--limit` flag following the registry pagination pattern should be added if the target user count exceeds 10,000. | @sanskarpan | Before GA |
| OQ-08 | Should the `entities` table be exportable via `tag export` (if that command exists) and importable on another machine? Cross-machine entity migration would be useful for disaster recovery but requires re-encrypting with the destination machine's AES key. | @sanskarpan | Post-GA |

---

## 16. Complexity and Timeline

**Total estimated effort: L (2-4 weeks)**

### Phase 1 — Core credential storage (Days 1-5)

- Add `entities` and `entity_credentials` DDL to `open_db()` migration.
- Implement `_encrypt` / `_decrypt` with keychain key management.
- Implement `SQLiteCredentialBackend` (get, put, delete, list_providers).
- Implement `CredentialContext` and `EntityRecord` dataclasses.
- Implement `_build_env_vars` and provider defaults registry.
- Implement `tag entity create`, `tag entity auth --token`, `tag entity list`, `tag entity show`, `tag entity revoke` CLI commands in `controller.py`.
- Unit tests for encryption, dataclasses, and backend CRUD operations.
- **Deliverable:** Static token storage and retrieval working end-to-end.

### Phase 2 — Run machinery integration (Days 6-9)

- Add `--entity` flag to `tag submit` argument parser.
- Implement `load_entity_credentials()` public function.
- Integrate credential injection into `cmd_submit` subprocess env construction.
- Inject `entity.id` into span attributes via `otel_semconv.py` additions.
- Add `entity_id` to `metadata_json` in `runs` table.
- Add `--entity` filter to `tag trace list`.
- Integration tests for credential injection and span attribute presence.
- **Deliverable:** `tag submit --entity` works with static tokens; spans carry `entity.id`.

### Phase 3 — OAuth flows (Days 10-16)

- Implement `_is_headless()` detection.
- Implement `_run_device_flow()` with exponential backoff and 15-minute timeout.
- Implement OAuth AS metadata discovery chain (RFC 8414 + RFC 9470 PRM).
- Implement authorization-code + PKCE flow with local redirect server on ephemeral port (non-headless path).
- Implement token validation test calls per provider.
- Implement `tag entity rotate` with atomic transaction.
- Implement `_attempt_refresh()` for expired OAuth tokens.
- OAuth flow unit tests with mock HTTP server.
- **Deliverable:** Full OAuth flow for GitHub and Google providers working in both headed and headless modes.

### Phase 4 — Backend abstraction and hardening (Days 17-21)

- Implement `CredentialBackend` ABC.
- Implement `VaultCredentialBackend` (optional extra).
- Implement `AwsSsmCredentialBackend` (optional extra).
- Add `TAG_CREDENTIAL_BACKEND` env var selection logic.
- Implement `PRAGMA secure_delete` on revoke connections.
- Add shell history warning for `--token` CLI usage.
- Security review of `mcp_auth.py` against PRD-034 scanner.
- Performance tests (1,000 entities, parallel loads).
- Update `pyproject.toml` with optional extras `[vault]` and `[aws]`.
- **Deliverable:** Full feature complete, all acceptance criteria passing, backend abstraction documented.
