# PRD-080: Enterprise IdP SSO Across MCP Servers (`tag mcp sso`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** XL (4-8 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `mcp_auth.py` (new), `src/tag/controller.py` (new `cmd_mcp_sso_*` handlers)
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (secret scanning), PRD-014 (MCP server registry), PRD-040 (notification hooks), PRD-041 (OTel span/cost attribution)
**Inspired by:** Okta SSO, Azure AD SAML/OIDC, WorkOS enterprise SSO

---

## 1. Overview

Enterprise teams deploying TAG across multiple engineers face an identity problem that compounds with every MCP server added to the stack. Each MCP server — whether it wraps a GitHub API, an internal data warehouse, a Jira instance, or a Salesforce org — maintains its own credential store. Today a developer must configure each server independently: generate tokens, rotate secrets, manage expiry, and repeat the process for every tool they enable. When an employee is offboarded, each credential must be revoked server-by-server with no central control plane. When a new engineer joins, onboarding requires manual credential provisioning across a dozen systems. This is not merely inconvenient; it is a security posture failure. Credentials rotate at different cadences, some never rotate, and there is no audit trail correlating user identity to MCP tool invocations.

Enterprise Identity Providers — Okta, Azure Active Directory, Google Workspace, and platforms like WorkOS that federate them — already solve this problem for web applications via OIDC (OpenID Connect) and SAML 2.0. The organization's IdP becomes the single authority for user identity, group membership, and access policy. Applications trust the IdP's tokens rather than maintaining their own user stores. Revocation is instantaneous: disabling a user in Okta immediately revokes access to all connected applications. Group-based access policies mean that adding a developer to the "backend-engineers" group automatically grants the right tool scopes on every MCP server that respects that group.

This PRD introduces `tag mcp sso`: a subsystem in `mcp_auth.py` that integrates TAG with enterprise IdPs using OIDC and SAML 2.0 token exchange. When a user runs `tag mcp sso login`, TAG performs a browser-based OIDC authorization code flow (with PKCE) against the configured IdP. The resulting ID token and access token are stored in the OS keychain. When TAG starts an MCP server session, `mcp_auth.py` exchanges the IdP token for a server-specific access token using the OAuth 2.0 Token Exchange flow (RFC 8693), propagating verified user identity to the MCP server without the user ever touching per-server credentials. Per-server scope mapping is derived from IdP group membership: the `sso_scope_maps` SQLite table maps IdP groups to MCP server scope lists, enabling fine-grained, centrally-managed access control.

The feature targets three use cases: (1) enterprise teams wanting centralized identity management and audit trails for MCP tool usage; (2) platform engineers who need to enforce least-privilege scopes on MCP servers based on team/role membership; and (3) individual developers in organizations that mandate SSO and cannot use per-server API keys due to compliance policy. The design follows the MCP OAuth 2.1 discovery chain established in the cluster research context: 401 → Protected Resource Metadata (PRM) → Authorization Server metadata → token exchange. The `resource` parameter (RFC 8707) is mandatory in every token request, and PKCE S256 is mandatory for all flows.

The scope of this PRD is deliberately bounded. It implements the token acquisition, storage, exchange, and scope-mapping layers. It does not implement a full SAML assertion consumer (SAML is supported only via IdP-initiated OIDC bridging, which all three target IdPs support). It does not modify MCP server code. It does not replace TAG's existing per-server OAuth flow (PRD-014 MCP registry OAuth pattern); instead it provides an alternative authentication path that SSO-configured servers can opt into.

---

## 2. Problem Statement

### 2.1 Per-Server Credential Sprawl Creates Unmanageable Security Debt

A TAG user with ten MCP servers enabled has ten separate credential lifecycles to manage. API keys are created once and forgotten; they do not rotate automatically; they do not expire by default; and they carry no identity information beyond an opaque token string. When an employee leaves the organization, the security team must chase down and revoke ten tokens across ten systems — assuming they can even enumerate which systems the employee accessed. The MCP protocol itself does not define a user identity layer; it leaves authentication to the transport and the application. Without an identity-aware authentication layer in TAG, every MCP server is a potential credential leak waiting to happen.

### 2.2 Group-Based Access Control Is Absent From the MCP Tool Layer

Enterprise access control policies live in the IdP: "members of the `data-engineers` group can query production databases; members of `frontend-engineers` cannot." These policies are enforced at the application layer today, but MCP servers connected via TAG receive no group membership information. A developer who should only have read access to a database MCP server might have the same connection string as a data engineer with write access, because TAG has no mechanism to communicate IdP group membership to the MCP server or to map groups to OAuth scopes at connection time. The result is that teams either over-provision (everyone gets maximum scopes) or implement ad hoc per-user configuration that is fragile and hard to audit.

### 2.3 No Audit Trail Correlates User Identity to MCP Tool Invocations

TAG's tracing subsystem (PRD-013) records tool call spans with server name, tool name, arguments, and latency. It does not record *who* made the call with any identity guarantee. In a shared team environment where multiple developers use the same TAG profile, audit logs are attributable only to a machine or profile — not to a verified human identity. Enterprise compliance requirements (SOC 2, ISO 27001, HIPAA) require that audit logs contain authenticated user identifiers. Without IdP-verified identity propagated to MCP servers, TAG cannot satisfy these requirements. The SSO subsystem solves this by binding every MCP tool invocation to a verifiable IdP subject (`sub`) claim that appears in both TAG's local audit log and the MCP server's own access logs.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag mcp sso configure --idp okta --tenant my-org.okta.com` writes a validated SSO configuration to `~/.tag/sso_config.yaml` and tests IdP connectivity. |
| G2 | `tag mcp sso login` performs a browser-based OIDC authorization code flow with PKCE S256, stores tokens in the OS keychain via `keyring`, and prints token expiry. |
| G3 | `tag mcp sso status` displays the current SSO session state: IdP, tenant, authenticated user (`sub`, `email`), token expiry, and per-server exchange status. |
| G4 | `tag mcp sso logout` revokes the IdP refresh token (if the IdP supports RFC 7009 token revocation), removes all SSO tokens from the keychain, and clears per-server exchanged tokens. |
| G5 | At MCP server session start, `mcp_auth.py` detects SSO configuration, silently performs RFC 8693 token exchange to obtain a server-scoped access token, and injects it into the MCP server connection without user intervention. |
| G6 | Per-server scope mapping from IdP groups is stored in the `sso_scope_maps` table and applied at token exchange time, so group membership from the IdP token controls which OAuth scopes are requested per server. |
| G7 | Support Okta (OIDC), Azure AD (OIDC and SAML 2.0 via OIDC bridge), and Google Workspace (OIDC) as first-class IdP targets with provider-specific discovery and metadata parsing. |
| G8 | The `sso_audit_log` SQLite table records every SSO event (login, logout, token exchange, exchange failure, scope escalation attempt) with IdP subject, server name, scopes granted, and timestamp. |
| G9 | `tag mcp sso scope map --server io.github.acme/db-mcp --group data-engineers --scopes read:query,write:query` manages scope mappings without editing YAML by hand. |
| G10 | When the IdP access token expires, `mcp_auth.py` silently refreshes using the stored refresh token; if refresh fails (revoked, expired), it emits a notification (PRD-040) and blocks tool calls on the affected server until re-login. |
| G11 | `tag mcp sso token show --server <name>` displays the decoded claims of the exchanged token for a specific server (redacting the raw token; showing only `sub`, `email`, `groups`, `scope`, `exp`). |
| G12 | Zero SSO-related code executes when SSO is not configured. All imports are deferred; `mcp_auth.py` is not imported at TAG startup unless `sso_config.yaml` exists. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Implementing a SAML assertion consumer service (ACS) within TAG. SAML is supported only via IdP-initiated OIDC bridging (all three target IdPs support OIDC natively). |
| NG2 | Modifying MCP server implementations to accept SSO tokens. This feature operates entirely on the TAG client side; it produces tokens that MCP servers already accept via their existing OAuth 2.1 flows. |
| NG3 | Supporting arbitrary SAML IdPs beyond the three named targets (Okta, Azure AD, Google Workspace). WorkOS is supported only if the organization configures WorkOS to present an OIDC endpoint. |
| NG4 | Replacing TAG's existing per-server OAuth flow (the 5-step MCP OAuth 2.1 discovery chain). SSO is an alternative path; servers that do not support token exchange continue to use the existing flow. |
| NG5 | Multi-tenant SSO (a single TAG installation serving multiple IdP tenants simultaneously). One SSO configuration is active at a time. |
| NG6 | Implementing a TAG authorization server. TAG is always the OAuth client; it never acts as an authorization server or token issuer. |
| NG7 | Automatic provisioning or deprovisioning of user accounts in MCP servers based on IdP events (SCIM). This PRD covers authentication and authorization at connection time only. |
| NG8 | Browser extension or system proxy-level token injection. Token exchange happens in the TAG process; the MCP server connection is a local stdio or HTTP connection managed by TAG. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Login time (Okta OIDC) | `tag mcp sso login` completes browser flow and stores token in < 30 seconds on fast network | Automated browser test with Playwright; 10-run median |
| Token exchange latency | Per-server RFC 8693 exchange adds < 200 ms to MCP server session startup | Benchmark 50 exchanges against a test AS; P95 |
| Scope mapping correctness | Group-to-scope resolution produces exactly the expected scope list for 100% of test cases | Unit test suite; 30+ test vectors |
| Keychain storage | Zero plaintext tokens appear in `~/.tag/`, `~/.config/`, or temp files after `tag mcp sso login` | File-system scan in integration test |
| Audit log completeness | Every SSO event (login, exchange, refresh, logout) appears in `sso_audit_log` within 1 second | Integration test; assert row count after each event |
| Zero overhead when unconfigured | `tag run` wall time with no `sso_config.yaml` is within 5 ms of baseline (no SSO import overhead) | Benchmark 20 runs; Wilcoxon signed-rank test |
| Revocation propagation | `tag mcp sso logout` causes the next `tag run` MCP call to fail with `SSOSessionExpiredError` within 100 ms | Integration test with mock MCP server |
| Okta / AzureAD / GWS compatibility | All three IdP providers pass the full integration test suite (30 test cases per IdP) | CI matrix job against sandbox IdP tenants |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Platform engineer | run `tag mcp sso configure --idp okta --tenant acme.okta.com` once per workstation | All MCP server connections are authenticated via the company Okta tenant without per-server credentials |
| U2 | Developer | run `tag mcp sso login` and complete the browser flow once per day | My TAG session is bound to my verified corporate identity for the rest of the workday |
| U3 | Security engineer | query `sso_audit_log` for a departing employee's `sub` claim | I can enumerate every MCP tool call made under that identity and verify revocation was effective |
| U4 | Platform engineer | run `tag mcp sso scope map --server io.github.acme/db-mcp --group data-engineers --scopes read:query,write:query` | Data engineers get write access to the DB MCP server automatically via their Okta group; frontend engineers get read-only without any per-user configuration |
| U5 | Developer | run `tag mcp sso status` | I can see at a glance whether my SSO session is valid, when it expires, and which MCP servers have active exchanged tokens |
| U6 | Compliance officer | export the `sso_audit_log` table to a SIEM | TAG MCP tool usage appears in the company's centralized audit log with verified user identities |
| U7 | Developer | have my IdP access token silently refreshed when it expires mid-session | My `tag loop` run does not fail with an authentication error after 60 minutes |
| U8 | New team member | run `tag mcp sso login` after cloning the team's TAG profile repo (which includes `sso_config.yaml`) | I am immediately able to use all MCP servers the team has configured, with the right scopes for my IdP groups, without requesting any individual credentials |
| U9 | Developer | run `tag mcp sso token show --server io.github.acme/github-mcp` | I can verify which scopes my exchanged token carries for a specific MCP server without needing to decode a JWT manually |
| U10 | Security engineer | disable a user in Okta | All subsequent TAG MCP calls by that user fail within one token TTL period (≤ 1 hour by default) without any manual action in TAG |

---

## 6. Proposed CLI Surface

All SSO subcommands live under `tag mcp sso`. The `mcp` namespace already hosts `tag mcp list`, `tag mcp install`, and `tag mcp auth` (existing per-server OAuth); `sso` is a new subgroup.

### 6.1 `tag mcp sso configure`

```
tag mcp sso configure \
  --idp <okta|azure-ad|google-workspace|workos> \
  --tenant <tenant-domain-or-id> \
  [--client-id <oidc-client-id>] \
  [--redirect-uri <uri>]            # default: http://localhost:9753/callback \
  [--scopes <space-separated>]      # default: openid email profile groups offline_access \
  [--device-flow]                   # use device code flow instead of browser (for headless) \
  [--test]                          # perform OIDC discovery and print metadata; do not save \
  [--output yaml|json]
```

**Example output (success):**

```
SSO Configuration
IdP:           okta
Tenant:        acme.okta.com
Discovery URL: https://acme.okta.com/.well-known/openid-configuration
Issuer:        https://acme.okta.com
JWKS URI:      https://acme.okta.com/oauth2/v1/keys
Device flow:   no
Redirect URI:  http://localhost:9753/callback
Scopes:        openid email profile groups offline_access

Configuration written to ~/.tag/sso_config.yaml
Run `tag mcp sso login` to authenticate.
```

**Example output (--test, connectivity check):**

```
Testing IdP connectivity...
  OIDC Discovery:     OK (234 ms)
  JWKS endpoint:      OK (89 ms)
  Token endpoint:     OK (reachable)
  Revocation endpoint:OK (RFC 7009 supported)

Configuration NOT saved (--test mode).
```

### 6.2 `tag mcp sso login`

```
tag mcp sso login \
  [--force]          # re-authenticate even if a valid session exists \
  [--device-flow]    # override; use device code flow \
  [--json]
```

**Browser flow output:**

```
Opening browser for SSO login...
  IdP: Okta  Tenant: acme.okta.com

If the browser did not open automatically, visit:
  https://acme.okta.com/oauth2/v1/authorize?client_id=...&response_type=code&...

Waiting for authorization... (Ctrl+C to cancel)
  Authorization received.
  Exchanging code for tokens...

SSO Login Successful
  User:    alice@acme.com (sub: 00u1abcdef2GHIJKLMN)
  Groups:  data-engineers, backend-team
  Expires: 2026-06-18T10:23:00Z (access token)
           2026-07-17T10:23:00Z (refresh token)
  Tokens stored in OS keychain (service: tag-sso)

Run `tag mcp sso status` to verify.
```

**Device flow output (headless):**

```
Device Authorization Flow
  Visit: https://acme.okta.com/activate
  Code:  XKQP-MWRZ

Waiting for authorization...
  Polling... (attempt 1/30, interval 5s)
  ...
  Authorization granted.

SSO Login Successful
  User:    alice@acme.com
  Expires: 2026-06-18T10:23:00Z
```

### 6.3 `tag mcp sso status`

```
tag mcp sso status [--json] [--verbose]
```

**Output:**

```
SSO Session Status
  IdP:        Okta
  Tenant:     acme.okta.com
  User:       alice@acme.com
  Subject:    00u1abcdef2GHIJKLMN
  Groups:     data-engineers, backend-team
  Access token expires: 2026-06-18T10:23:00Z (in 47 min)
  Refresh token:        valid (expires 2026-07-17)

Per-Server Token Exchange Status
  SERVER                          SCOPES                        EXCHANGED          EXPIRES
  io.github.acme/db-mcp           read:query write:query        2026-06-18T09:21Z  10:21Z
  io.github.acme/github-mcp       repo:read  pr:write           2026-06-18T09:19Z  10:19Z
  io.github.acme/jira-mcp         issues:read issues:write      not yet exchanged  —

Scope Mappings (2 active)
  data-engineers → io.github.acme/db-mcp: read:query, write:query
  backend-team   → io.github.acme/github-mcp: repo:read, pr:write
```

### 6.4 `tag mcp sso logout`

```
tag mcp sso logout \
  [--revoke]         # attempt RFC 7009 token revocation at IdP (default: true) \
  [--all-servers]    # also clear all per-server exchanged tokens from keychain \
  [--force]          # skip confirmation prompt
```

**Output:**

```
SSO Logout
  Revoking refresh token at acme.okta.com... OK
  Removing access token from keychain... OK
  Removing refresh token from keychain... OK
  Clearing 2 per-server exchanged tokens... OK

Logged out. Run `tag mcp sso login` to re-authenticate.
```

### 6.5 `tag mcp sso scope map` (scope mapping management)

```
# Add or update a mapping
tag mcp sso scope map \
  --server io.github.acme/db-mcp \
  --group data-engineers \
  --scopes "read:query,write:query"

# List all mappings
tag mcp sso scope map list [--server <name>] [--group <group>] [--json]

# Remove a mapping
tag mcp sso scope map remove \
  --server io.github.acme/db-mcp \
  --group data-engineers

# Import mappings from YAML file
tag mcp sso scope map import --file scope_policy.yaml
```

**`scope map list` output:**

```
Scope Mappings (3 total)

  GROUP              SERVER                        SCOPES
  data-engineers     io.github.acme/db-mcp         read:query, write:query
  backend-team       io.github.acme/github-mcp     repo:read, pr:write
  all-employees      io.github.acme/jira-mcp       issues:read

Use `tag mcp sso scope map --server <name> --group <group> --scopes <...>` to add or update.
```

### 6.6 `tag mcp sso token show`

```
tag mcp sso token show \
  --server io.github.acme/db-mcp \
  [--claims]    # decode and display JWT claims (never prints raw token) \
  [--json]
```

**Output:**

```
Exchanged Token: io.github.acme/db-mcp
  Subject:  00u1abcdef2GHIJKLMN
  Email:    alice@acme.com
  Scope:    read:query write:query
  Issuer:   https://acme.okta.com
  Audience: https://db-mcp.internal.acme.com
  Issued:   2026-06-18T09:21:00Z
  Expires:  2026-06-18T10:21:00Z (in 58 min)
  Groups:   data-engineers, backend-team

Raw token: [REDACTED — use --claims to see decoded claims]
```

### 6.7 `tag mcp sso audit`

```
tag mcp sso audit \
  [--since <RFC3339>] \
  [--until <RFC3339>] \
  [--sub <idp-subject>] \
  [--server <server-name>] \
  [--event <login|logout|exchange|refresh|revocation|scope_escalation>] \
  [--limit N] \
  [--json] \
  [--export csv|jsonl]
```

**Output:**

```
SSO Audit Log (last 10 events)

  TIMESTAMP              EVENT       USER                  SERVER                   SCOPES
  2026-06-18T09:19:00Z   login       alice@acme.com        —                        openid email groups
  2026-06-18T09:21:00Z   exchange    alice@acme.com        io.github.acme/db-mcp    read:query write:query
  2026-06-18T09:21:01Z   exchange    alice@acme.com        io.github.acme/github-mcp repo:read pr:write
  2026-06-18T10:19:00Z   refresh     alice@acme.com        —                        —
  ...
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag mcp sso configure` MUST perform OIDC discovery (fetch `/.well-known/openid-configuration`) and validate that the discovery document contains `authorization_endpoint`, `token_endpoint`, `jwks_uri`, and `issuer` before writing config. | P0 |
| FR-02 | `tag mcp sso configure` MUST write the SSO configuration to `~/.tag/sso_config.yaml` with `0600` file permissions. The file MUST NOT contain client secrets; client secrets are stored in keychain only. | P0 |
| FR-03 | `tag mcp sso login` MUST perform PKCE S256 code challenge/verifier generation per RFC 7636, section 4.2. The `code_challenge_method=S256` parameter MUST be present in the authorization request. | P0 |
| FR-04 | `tag mcp sso login` MUST use a loopback redirect URI (`http://localhost:<port>/callback`) and bind a short-lived HTTP server to capture the authorization code. The port MUST be selected randomly from the ephemeral range (49152-65535) to avoid conflicts. | P0 |
| FR-05 | `tag mcp sso login` MUST detect headless environments (absence of `DISPLAY` and `SSH_TTY` env vars, or presence of `TAG_HEADLESS=1`) and automatically switch to device code flow (RFC 8628) when headless is detected. | P1 |
| FR-06 | All tokens (IdP access token, refresh token, per-server exchanged tokens) MUST be stored exclusively in the OS keychain via the `keyring` library (service name: `tag-sso`, account names defined in `KeychainKey` enum). Zero token data MUST be written to disk files, SQLite, or environment variables. | P0 |
| FR-07 | At MCP server session start, `mcp_auth.py` MUST attempt RFC 8693 token exchange if and only if all three conditions hold: (a) `~/.tag/sso_config.yaml` exists, (b) a valid (non-expired) IdP access token is in the keychain, (c) the MCP server's AS metadata exposes a `token_endpoint` that accepts `urn:ietf:params:oauth:grant-type:token-exchange`. If the server's AS does not support token exchange, `mcp_auth.py` MUST fall back to the existing per-server OAuth flow silently. | P0 |
| FR-08 | Token exchange requests (RFC 8693) MUST include the `resource` parameter (RFC 8707) set to the MCP server's canonical URI, and the `subject_token_type` set to `urn:ietf:params:oauth:token-type:access_token`. | P0 |
| FR-09 | Scope selection at token exchange time MUST be computed as the intersection of: (a) scopes supported by the MCP server (from AS metadata), (b) scopes mapped from the user's IdP groups via `sso_scope_maps`, and (c) scopes present in the IdP access token. The system MUST NOT request scopes the user is not entitled to via their groups. | P0 |
| FR-10 | If the IdP access token has expired and a refresh token is present, `mcp_auth.py` MUST silently refresh the access token before attempting token exchange. If refresh fails (HTTP 400 with `error=invalid_grant`), `mcp_auth.py` MUST raise `SSOSessionExpiredError` and emit a notification via PRD-040 hooks. | P1 |
| FR-11 | Every SSO event (login, logout, token exchange, token refresh, exchange failure, revocation attempt, scope escalation attempt) MUST be written to the `sso_audit_log` SQLite table within 500 ms of the event occurring. | P0 |
| FR-12 | `tag mcp sso logout` MUST attempt RFC 7009 token revocation (`POST` to the IdP's `revocation_endpoint`) for both the access token and refresh token. Revocation failure MUST be logged but MUST NOT block local cleanup. Local keychain entries MUST be deleted regardless of revocation outcome. | P1 |
| FR-13 | `tag mcp sso scope map` MUST validate that the specified group name is present in the IdP access token's `groups` claim (or comparable claim per IdP) before writing the mapping, or emit a warning if validation cannot be performed offline. | P1 |
| FR-14 | `tag mcp sso token show` MUST decode and display JWT claims without printing the raw token string. The raw token value MUST be redacted in all CLI output and log output. | P0 |
| FR-15 | The `mcp_auth.py` module MUST NOT be imported at TAG startup unless `~/.tag/sso_config.yaml` exists. The import MUST be deferred and lazy. | P1 |
| FR-16 | For Azure AD, the OIDC discovery URL MUST be constructed as `https://login.microsoftonline.com/<tenant-id>/v2.0/.well-known/openid-configuration` with tenant ID normalization (accept both GUID and `<domain>.onmicrosoft.com`). | P1 |
| FR-17 | For Google Workspace, the groups claim MUST be obtained via the Google Directory API (using the access token) since Google's OIDC ID token does not include group membership. `mcp_auth.py` MUST call the Directory API at login time and cache the group list in `sso_session_cache`. | P1 |
| FR-18 | `tag mcp sso configure --test` MUST verify IdP connectivity without writing any configuration or performing authentication. Exit code MUST be 0 on success, 1 on connectivity failure. | P1 |
| FR-19 | JWS signature verification of IdP-issued JWT tokens MUST be performed using the IdP's JWKS endpoint. TAG MUST NOT accept tokens whose signature cannot be verified against the current JWKS. JWKS caching TTL is 5 minutes. | P0 |
| FR-20 | Scope escalation attempts (a tool requesting scopes beyond what the user's group mappings allow) MUST be blocked, logged to `sso_audit_log` with `event_type=scope_escalation`, and reported via notification (PRD-040). | P0 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Latency — Token Exchange** | Per-server RFC 8693 exchange MUST complete in < 500 ms at P95 over a 50 ms RTT network. |
| NFR-02 | **Latency — Login** | Browser OIDC flow (excluding user interaction time) MUST complete code exchange and keychain write in < 3 seconds after redirect. |
| NFR-03 | **Startup Overhead** | `tag run` start time with no `sso_config.yaml` MUST be within 5 ms of baseline. Verified by benchmark harness. |
| NFR-04 | **Keychain Security** | Zero plaintext token bytes MUST appear in any file in `~/.tag/`, `/tmp/`, or process environment variables at any point during or after the login flow. |
| NFR-05 | **Token Verification** | JWS signature verification MUST reject tokens with algorithm `none`, `RS256` with mismatched kid, or expired `exp` claims. |
| NFR-06 | **Audit Log Durability** | `sso_audit_log` writes use WAL mode (shared with `tag.sqlite3`); each write is followed by an explicit `COMMIT`. No audit event MAY be lost on process crash after the event occurs. |
| NFR-07 | **Concurrency** | Multiple concurrent `tag run` processes (e.g., in a `tag swarm` scenario) MUST each independently hold their own SSO token exchange result; they MUST NOT race on shared keychain writes. Lock via `keyring`'s atomic set semantics. |
| NFR-08 | **IdP Compatibility** | All three IdPs (Okta, Azure AD, Google Workspace) MUST pass the full integration test suite. Provider-specific quirks MUST be handled in the `IdPAdapter` class hierarchy, not with `if idp == "okta":` conditionals scattered across `mcp_auth.py`. |
| NFR-09 | **Error Messages** | All authentication failures MUST produce human-readable error messages that include: the failed operation, the HTTP status (if applicable), the IdP endpoint that failed, and the next action the user should take. |
| NFR-10 | **Dependency Footprint** | New runtime dependencies are limited to: `keyring` (already a TAG dependency or well-established), `cryptography` (for JWS verification, already common in Python envs), `httpx` (already used in TAG). No new heavyweight dependencies. |
| NFR-11 | **Secrets in Logs** | The OTel tracing layer (PRD-013) MUST redact token values from any span attributes before export. `mcp_auth.py` MUST never pass raw token strings to `logger.debug/info/warning/error`. |
| NFR-12 | **Graceful Degradation** | If the IdP is unreachable at session start (network partition), `mcp_auth.py` MUST use the cached (non-expired) exchanged token if available, or raise `SSOIdPUnreachableError` with a clear message. MUST NOT silently fall back to unauthenticated access. |

---

## 9. Technical Design

### 9.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/mcp_auth.py` | Core SSO logic: IdP adapters, PKCE flow, token exchange, scope resolution, keychain I/O |
| `src/tag/integrations/sso_adapters/okta.py` | Okta-specific OIDC discovery and groups claim parsing |
| `src/tag/integrations/sso_adapters/azure_ad.py` | Azure AD v2.0 OIDC + tenant normalization |
| `src/tag/integrations/sso_adapters/google_workspace.py` | Google Workspace OIDC + Directory API groups fetch |
| `src/tag/integrations/sso_adapters/__init__.py` | `IdPAdapterRegistry`; adapter lookup by `IdPType` enum |
| `tests/test_mcp_sso.py` | Unit + integration tests |
| `tests/fixtures/sso/` | Mock OIDC discovery documents, JWKS, token responses for each IdP |

### 9.2 SQLite DDL

All tables use the existing `open_db()` context manager and share the WAL-mode `tag.sqlite3` database at `~/.tag/runtime/tag.sqlite3`.

```sql
-- SSO session cache: tracks current IdP session metadata (NOT token values)
CREATE TABLE IF NOT EXISTS sso_session_cache (
    id               INTEGER PRIMARY KEY,
    idp_type         TEXT NOT NULL,          -- 'okta' | 'azure-ad' | 'google-workspace'
    tenant           TEXT NOT NULL,
    subject          TEXT NOT NULL,          -- IdP 'sub' claim
    email            TEXT,
    groups_json      TEXT NOT NULL DEFAULT '[]', -- JSON array of group names
    access_token_exp INTEGER NOT NULL,       -- Unix epoch seconds
    refresh_token_exp INTEGER,               -- NULL if IdP does not issue refresh tokens
    created_at       INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at       INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_sso_session_subject ON sso_session_cache(subject);

-- Per-server exchanged token metadata (NOT the token itself — stored in keychain)
CREATE TABLE IF NOT EXISTS sso_server_tokens (
    id               INTEGER PRIMARY KEY,
    server_name      TEXT NOT NULL,          -- MCP server reverse-domain name
    server_uri       TEXT NOT NULL,          -- RFC 8707 resource URI
    subject          TEXT NOT NULL,          -- IdP sub claim; FK to sso_session_cache
    scopes_granted   TEXT NOT NULL,          -- space-separated OAuth scopes
    token_exp        INTEGER NOT NULL,       -- Unix epoch seconds
    keychain_account TEXT NOT NULL,          -- keyring account name for lookup
    exchanged_at     INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (server_name, subject)
);
CREATE INDEX IF NOT EXISTS idx_sso_server_tokens_server ON sso_server_tokens(server_name);
CREATE INDEX IF NOT EXISTS idx_sso_server_tokens_subject ON sso_server_tokens(subject);

-- Scope mapping policy: IdP group -> MCP server -> allowed OAuth scopes
CREATE TABLE IF NOT EXISTS sso_scope_maps (
    id               INTEGER PRIMARY KEY,
    idp_group        TEXT NOT NULL,
    server_name      TEXT NOT NULL,          -- MCP server reverse-domain name
    scopes           TEXT NOT NULL,          -- comma-separated OAuth scope list
    created_at       INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at       INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (idp_group, server_name)
);
CREATE INDEX IF NOT EXISTS idx_sso_scope_maps_server ON sso_scope_maps(server_name);
CREATE INDEX IF NOT EXISTS idx_sso_scope_maps_group ON sso_scope_maps(idp_group);

-- Immutable audit log: every SSO event
CREATE TABLE IF NOT EXISTS sso_audit_log (
    id               INTEGER PRIMARY KEY,
    event_type       TEXT NOT NULL,          -- 'login'|'logout'|'exchange'|'refresh'
                                             -- |'revocation'|'scope_escalation'|'exchange_failure'
    subject          TEXT,                   -- IdP sub claim (NULL before login completes)
    email            TEXT,
    server_name      TEXT,                   -- NULL for login/logout/refresh events
    scopes_requested TEXT,                   -- space-separated; for exchange events
    scopes_granted   TEXT,                   -- may differ from requested after intersection
    outcome          TEXT NOT NULL,          -- 'success' | 'failure' | 'blocked'
    error_code       TEXT,                   -- OAuth error code if outcome=failure
    error_detail     TEXT,                   -- human-readable error detail
    idp_type         TEXT,
    tenant           TEXT,
    created_at       INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;
CREATE INDEX IF NOT EXISTS idx_sso_audit_event ON sso_audit_log(event_type, created_at);
CREATE INDEX IF NOT EXISTS idx_sso_audit_subject ON sso_audit_log(subject, created_at);
CREATE INDEX IF NOT EXISTS idx_sso_audit_server ON sso_audit_log(server_name, created_at);
```

### 9.3 Core Dataclasses

```python
# src/tag/mcp_auth.py

from __future__ import annotations
import enum
import time
from dataclasses import dataclass, field
from typing import Optional

class IdPType(str, enum.Enum):
    OKTA             = "okta"
    AZURE_AD         = "azure-ad"
    GOOGLE_WORKSPACE = "google-workspace"
    WORKOS           = "workos"

class KeychainKey(str, enum.Enum):
    """Canonical keyring account names. Service is always 'tag-sso'."""
    IDP_ACCESS_TOKEN  = "idp-access-token"
    IDP_REFRESH_TOKEN = "idp-refresh-token"
    # Per-server keys are constructed as f"server-token:{server_name}"

@dataclass
class SSOConfig:
    idp_type:       IdPType
    tenant:         str                        # e.g. "acme.okta.com" or Azure tenant GUID
    client_id:      str
    redirect_uri:   str = "http://localhost:9753/callback"
    scopes:         list[str] = field(default_factory=lambda: [
        "openid", "email", "profile", "groups", "offline_access"
    ])
    device_flow:    bool = False
    discovery_url:  Optional[str] = None       # auto-derived if None

@dataclass
class OIDCMetadata:
    issuer:                  str
    authorization_endpoint:  str
    token_endpoint:          str
    jwks_uri:                str
    device_authorization_endpoint: Optional[str] = None
    revocation_endpoint:     Optional[str] = None
    token_endpoint_auth_methods_supported: list[str] = field(default_factory=list)

@dataclass
class SSOSession:
    subject:           str
    email:             str
    groups:            list[str]
    access_token_exp:  int          # Unix epoch
    refresh_token_exp: Optional[int]
    idp_type:          IdPType
    tenant:            str

    def is_access_token_valid(self, clock_skew_s: int = 30) -> bool:
        return int(time.time()) < (self.access_token_exp - clock_skew_s)

    def is_refresh_token_valid(self) -> bool:
        if self.refresh_token_exp is None:
            return False
        return int(time.time()) < self.refresh_token_exp

@dataclass
class TokenExchangeResult:
    server_name:     str
    server_uri:      str
    scopes_granted:  list[str]
    token_exp:       int          # Unix epoch
    keychain_account: str         # keyring account key for retrieval

@dataclass
class ScopeMappingPolicy:
    """Resolved scope policy for a user+server combination."""
    server_name:      str
    user_groups:      list[str]
    mapped_scopes:    list[str]   # union of all group mappings for this server
    server_scopes:    list[str]   # scopes advertised by the MCP server AS
    idp_token_scopes: list[str]   # scopes present in the IdP access token
    effective_scopes: list[str]   # intersection of all three above
```

### 9.4 IdP Adapter Interface

```python
# src/tag/mcp_auth.py (continued)

import abc
import httpx

class IdPAdapter(abc.ABC):
    """Abstract base for IdP-specific OIDC behavior."""

    def __init__(self, config: SSOConfig):
        self.config = config

    @abc.abstractmethod
    def discovery_url(self) -> str:
        """Return the OIDC discovery document URL for this IdP/tenant."""
        ...

    @abc.abstractmethod
    def extract_groups(
        self,
        id_token_claims: dict,
        access_token: str,
        http: httpx.Client,
    ) -> list[str]:
        """Return list of group names for the authenticated user.

        Some IdPs (Okta) include groups in the token claim directly.
        Others (Google Workspace) require a secondary API call.
        """
        ...

    def normalize_tenant(self, tenant: str) -> str:
        return tenant  # default: no transformation

    async def fetch_metadata(self, http: httpx.AsyncClient) -> OIDCMetadata:
        resp = await http.get(self.discovery_url(), timeout=10.0)
        resp.raise_for_status()
        doc = resp.json()
        return OIDCMetadata(
            issuer=doc["issuer"],
            authorization_endpoint=doc["authorization_endpoint"],
            token_endpoint=doc["token_endpoint"],
            jwks_uri=doc["jwks_uri"],
            device_authorization_endpoint=doc.get("device_authorization_endpoint"),
            revocation_endpoint=doc.get("revocation_endpoint"),
            token_endpoint_auth_methods_supported=doc.get(
                "token_endpoint_auth_methods_supported", []
            ),
        )
```

### 9.5 PKCE and Authorization Code Flow

```python
# src/tag/mcp_auth.py (continued)

import base64
import hashlib
import os
import secrets
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

def _pkce_pair() -> tuple[str, str]:
    """Return (code_verifier, code_challenge) for PKCE S256."""
    verifier = base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()
    digest = hashlib.sha256(verifier.encode()).digest()
    challenge = base64.urlsafe_b64encode(digest).rstrip(b"=").decode()
    return verifier, challenge

def _build_auth_url(
    metadata: OIDCMetadata,
    config: SSOConfig,
    state: str,
    code_challenge: str,
    port: int,
) -> str:
    params = {
        "response_type":         "code",
        "client_id":             config.client_id,
        "redirect_uri":          f"http://localhost:{port}/callback",
        "scope":                 " ".join(config.scopes),
        "state":                 state,
        "code_challenge":        code_challenge,
        "code_challenge_method": "S256",
    }
    return metadata.authorization_endpoint + "?" + urllib.parse.urlencode(params)
```

### 9.6 RFC 8693 Token Exchange Algorithm

```python
# src/tag/mcp_auth.py (continued)

import keyring

async def exchange_token_for_server(
    server_name: str,
    server_uri: str,
    token_endpoint: str,
    client_id: str,
    policy: ScopeMappingPolicy,
    http: httpx.AsyncClient,
) -> TokenExchangeResult:
    """
    Perform RFC 8693 token exchange: trade IdP access token for
    a server-scoped access token.

    The `resource` parameter (RFC 8707) is MANDATORY and set to `server_uri`.
    """
    if not policy.effective_scopes:
        raise ScopeEscalationError(
            f"No scopes authorized for {server_name} given current group membership"
        )

    idp_access_token = keyring.get_password("tag-sso", KeychainKey.IDP_ACCESS_TOKEN)
    if not idp_access_token:
        raise SSOSessionExpiredError("No IdP access token in keychain. Run `tag mcp sso login`.")

    payload = {
        "grant_type":          "urn:ietf:params:oauth:grant-type:token-exchange",
        "client_id":           client_id,
        "subject_token":       idp_access_token,
        "subject_token_type":  "urn:ietf:params:oauth:token-type:access_token",
        "requested_token_type":"urn:ietf:params:oauth:token-type:access_token",
        "resource":            server_uri,   # RFC 8707 — MANDATORY
        "scope":               " ".join(policy.effective_scopes),
    }

    resp = await http.post(token_endpoint, data=payload, timeout=15.0)

    if resp.status_code == 400:
        err = resp.json()
        if err.get("error") == "unsupported_grant_type":
            raise TokenExchangeNotSupportedError(server_name)
        raise TokenExchangeError(server_name, err.get("error"), err.get("error_description"))

    resp.raise_for_status()
    token_resp = resp.json()

    issued_token = token_resp["access_token"]
    exp = int(time.time()) + int(token_resp.get("expires_in", 3600))
    keychain_account = f"server-token:{server_name}"
    keyring.set_password("tag-sso", keychain_account, issued_token)

    return TokenExchangeResult(
        server_name=server_name,
        server_uri=server_uri,
        scopes_granted=token_resp.get("scope", "").split(),
        token_exp=exp,
        keychain_account=keychain_account,
    )
```

### 9.7 Scope Resolution Algorithm

```python
# src/tag/mcp_auth.py (continued)

from tag.controller import open_db

def resolve_scope_policy(
    server_name: str,
    user_groups: list[str],
    server_supported_scopes: list[str],
    idp_token_scopes: list[str],
) -> ScopeMappingPolicy:
    """
    Compute effective scopes for a token exchange as the three-way intersection:
      effective = mapped_scopes ∩ server_supported_scopes ∩ idp_token_scopes

    mapped_scopes = UNION of all scope entries in sso_scope_maps
                    where idp_group IN user_groups AND server_name = server_name
    """
    with open_db() as db:
        placeholders = ",".join("?" * len(user_groups))
        rows = db.execute(
            f"""
            SELECT scopes FROM sso_scope_maps
            WHERE server_name = ? AND idp_group IN ({placeholders})
            """,
            [server_name, *user_groups],
        ).fetchall()

    mapped: set[str] = set()
    for row in rows:
        for s in row["scopes"].split(","):
            mapped.add(s.strip())

    server_set = set(server_supported_scopes)
    idp_set    = set(idp_token_scopes)
    effective  = sorted(mapped & server_set & idp_set)

    return ScopeMappingPolicy(
        server_name=server_name,
        user_groups=user_groups,
        mapped_scopes=sorted(mapped),
        server_scopes=server_supported_scopes,
        idp_token_scopes=idp_token_scopes,
        effective_scopes=effective,
    )
```

### 9.8 JWS Signature Verification

```python
# src/tag/mcp_auth.py (continued)

import functools
import time as _time
from cryptography.hazmat.primitives.asymmetric.rsa import RSAPublicKey
from cryptography.hazmat.backends import default_backend

@functools.lru_cache(maxsize=8)
def _fetch_jwks_cached(jwks_uri: str, cache_bust: int) -> dict:
    """cache_bust = int(time.time()) // 300  (5-minute TTL)"""
    resp = httpx.get(jwks_uri, timeout=10.0)
    resp.raise_for_status()
    return resp.json()

def fetch_jwks(jwks_uri: str) -> dict:
    return _fetch_jwks_cached(jwks_uri, int(_time.time()) // 300)

def verify_jwt(token: str, jwks: dict, expected_issuer: str, expected_audience: str | None = None) -> dict:
    """
    Verify JWS signature and return decoded claims.
    Rejects: alg=none, expired tokens, issuer mismatch, audience mismatch.
    Uses `cryptography` library for RSA public key reconstruction from JWKS.
    """
    import json as _json
    header_b64, payload_b64, sig_b64 = token.split(".")
    header = _json.loads(_b64_decode(header_b64))

    if header.get("alg") == "none":
        raise JWSVerificationError("Algorithm 'none' is not permitted")

    kid = header.get("kid")
    alg = header.get("alg", "RS256")

    # Find matching key in JWKS
    key_data = next(
        (k for k in jwks.get("keys", []) if k.get("kid") == kid),
        None,
    )
    if key_data is None:
        raise JWSVerificationError(f"No JWKS key found for kid={kid!r}")

    # Reconstruct RSA public key and verify signature
    # (full implementation uses cryptography.hazmat.primitives.serialization)
    _verify_rsa_signature(header_b64 + "." + payload_b64, sig_b64, key_data, alg)

    claims = _json.loads(_b64_decode(payload_b64))

    if claims.get("iss") != expected_issuer:
        raise JWSVerificationError(
            f"Issuer mismatch: expected {expected_issuer!r}, got {claims.get('iss')!r}"
        )
    if int(_time.time()) > claims.get("exp", 0):
        raise JWSVerificationError("Token has expired")
    if expected_audience and claims.get("aud") != expected_audience:
        raise JWSVerificationError(
            f"Audience mismatch: expected {expected_audience!r}"
        )
    return claims
```

### 9.9 Audit Log Writer

```python
# src/tag/mcp_auth.py (continued)

from tag.controller import open_db

def audit(
    event_type: str,
    outcome: str,
    *,
    subject: str | None = None,
    email: str | None = None,
    server_name: str | None = None,
    scopes_requested: str | None = None,
    scopes_granted: str | None = None,
    error_code: str | None = None,
    error_detail: str | None = None,
    idp_type: str | None = None,
    tenant: str | None = None,
) -> None:
    """Write a single row to sso_audit_log. Called on every SSO event."""
    with open_db() as db:
        db.execute(
            """
            INSERT INTO sso_audit_log
              (event_type, subject, email, server_name, scopes_requested,
               scopes_granted, outcome, error_code, error_detail, idp_type, tenant)
            VALUES (?,?,?,?,?,?,?,?,?,?,?)
            """,
            (event_type, subject, email, server_name, scopes_requested,
             scopes_granted, outcome, error_code, error_detail, idp_type, tenant),
        )
        db.commit()
```

### 9.10 Integration Into MCP Server Session Startup

The existing MCP server connection code in `controller.py` calls a hook point at session initialization. `mcp_auth.py` is invoked here:

```python
# src/tag/controller.py  (sketch of modified section)

async def _get_mcp_auth_headers(server_name: str, server_uri: str) -> dict[str, str]:
    """
    Return Authorization header for MCP server connection.
    Prefers SSO token exchange if configured; falls back to per-server OAuth token;
    falls back to empty dict if neither is configured.
    """
    sso_config_path = pathlib.Path.home() / ".tag" / "sso_config.yaml"
    if not sso_config_path.exists():
        return await _get_per_server_oauth_token(server_name)  # existing flow

    # Lazy import — only when SSO is configured
    from tag.mcp_auth import get_or_exchange_server_token, SSOSessionExpiredError, \
        TokenExchangeNotSupportedError

    try:
        token = await get_or_exchange_server_token(server_name, server_uri)
        return {"Authorization": f"Bearer {token}"}
    except TokenExchangeNotSupportedError:
        return await _get_per_server_oauth_token(server_name)
    except SSOSessionExpiredError as exc:
        from tag.notifications import notify
        notify(f"SSO session expired for {server_name}: {exc}. Run `tag mcp sso login`.")
        raise
```

### 9.11 SSO Config File Format (`~/.tag/sso_config.yaml`)

```yaml
# ~/.tag/sso_config.yaml
# Permissions: 0600
# Do NOT store secrets here — they are in OS keychain only.

idp_type: okta
tenant: acme.okta.com
client_id: "0oa1abcdef2GHIJKLMN"
redirect_uri: "http://localhost:9753/callback"
scopes:
  - openid
  - email
  - profile
  - groups
  - offline_access
device_flow: false

# Optional: per-IdP overrides
okta:
  groups_claim: "groups"         # claim name in Okta ID token for group list
  audience: "api://default"      # Okta Authorization Server audience

azure_ad:
  tenant_id: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
  groups_claim: "groups"
  use_v2_endpoint: true

google_workspace:
  directory_api_enabled: true    # fetch groups via Google Directory API
  customer_id: "C01abc123"       # Google Workspace customer ID
```

### 9.12 Scope Policy YAML Import Format

```yaml
# scope_policy.yaml — importable via `tag mcp sso scope map import --file`
version: "1"
mappings:
  - group: data-engineers
    server: io.github.acme/db-mcp
    scopes:
      - read:query
      - write:query
  - group: backend-team
    server: io.github.acme/github-mcp
    scopes:
      - repo:read
      - pr:write
  - group: all-employees
    server: io.github.acme/jira-mcp
    scopes:
      - issues:read
```

---

## 10. Security Considerations

1. **Zero plaintext token storage.** All token values (IdP access tokens, refresh tokens, exchanged server tokens) are stored exclusively in the OS keychain via `keyring`. The `keyring` library uses macOS Keychain, Windows Credential Manager, or libsecret on Linux. No token byte ever touches a file, the SQLite database, an environment variable, or a log line. The `audit` function explicitly accepts only non-sensitive metadata, not token values.

2. **PKCE S256 mandatory for all flows.** The authorization code flow always uses PKCE S256 (RFC 7636 section 4.2). The `code_challenge_method=plain` variant is explicitly rejected. This prevents authorization code interception attacks even if the loopback redirect is race-conditioned by another local process.

3. **State parameter CSRF protection.** A cryptographically random 32-byte state parameter is generated per login attempt. The callback handler verifies state before accepting any authorization code. Mismatched state causes the login to abort with an error, not a silent failure.

4. **Audience-bound tokens prevent cross-server token reuse.** Each token exchange result includes `resource=<server_uri>` (RFC 8707), which causes the MCP server's authorization server to issue a token with `aud` set to that specific server URI. A token for `io.github.acme/db-mcp` cannot be used against `io.github.acme/github-mcp` even if intercepted.

5. **Scope intersection enforces least privilege.** The `resolve_scope_policy` function computes a three-way intersection. No code path allows scopes to be granted that are not simultaneously present in the user's IdP group mappings, the MCP server's supported scope list, and the IdP access token's own scope claim. A scope escalation attempt is blocked, audited, and reported.

6. **JWS verification with JWKS caching.** Tokens are verified against the IdP's JWKS endpoint before any claims are trusted. The `alg=none` algorithm is explicitly rejected. JWKS are cached for 5 minutes to limit network calls, but each login and exchange operation fetches fresh JWKS by busting the cache key. `kid` mismatch causes rejection.

7. **Audit log integrity.** The `sso_audit_log` table uses `STRICT` mode (SQLite 3.37+), enforcing declared column types. Each row is committed immediately. The table has no `DELETE` or `UPDATE` permission granted in application code — `audit()` only calls `INSERT`. Deletion requires direct SQLite access.

8. **Secret scanning integration (PRD-034).** The secret scanner's regex patterns are extended to detect Okta API tokens (`SSWS [a-zA-Z0-9_-]{42}`), Azure AD bearer tokens (`Bearer eyJ...`), and Google OAuth tokens (`ya29.[a-zA-Z0-9_-]{...}`) in any file or clipboard content that passes through TAG.

9. **Loopback redirect only.** The redirect URI is restricted to `http://localhost:<port>/callback`. Non-loopback redirect URIs are rejected at `configure` time. This prevents open-redirect phishing attacks where a malicious application captures the authorization code.

10. **Token revocation on logout.** `tag mcp sso logout` sends RFC 7009 revocation requests to the IdP for both access and refresh tokens. Revocation failure is logged but does not block local cleanup. This ensures that even if keychain entries are copied, the tokens are invalid at the IdP level as soon as logout is called.

11. **Headless environment token isolation.** In device code flow (headless), the `user_code` and verification URI are printed to stdout only. They are never logged to the audit log or written to any file. The `device_code` value (which carries higher trust than `user_code`) is stored only in process memory until the exchange completes.

12. **Tracing redaction.** The OTel span emitted by `exchange_token_for_server` sets attributes `sso.server_name`, `sso.scopes_granted`, `sso.subject`, and `sso.outcome`. It explicitly does NOT set `sso.token` or any raw token attribute. The existing `security.py` redaction layer (PRD-034) covers these spans.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_mcp_sso.py`)

| Test | Description |
|------|-------------|
| `test_pkce_pair_s256` | Verify that `_pkce_pair()` returns a valid S256 code_challenge derived from the verifier: `base64url(sha256(verifier)) == challenge`. |
| `test_pkce_pair_uniqueness` | Call `_pkce_pair()` 100 times; assert all verifiers are distinct (birthday-paradox collision probability < 1e-38). |
| `test_scope_intersection_basic` | User in `data-engineers`; mapped scopes `[read:query, write:query]`; server supports `[read:query, admin:query]`; IdP token has `[openid, read:query]`; effective MUST be `[read:query]`. |
| `test_scope_intersection_empty` | User has no groups; effective scopes MUST be `[]`; `resolve_scope_policy` MUST NOT raise. |
| `test_scope_escalation_blocked` | `exchange_token_for_server` with empty `effective_scopes` MUST raise `ScopeEscalationError`. |
| `test_jwt_verify_none_alg_rejected` | Construct a JWT with `alg=none`; `verify_jwt` MUST raise `JWSVerificationError`. |
| `test_jwt_verify_expired` | Construct a JWT with `exp` in the past; MUST raise `JWSVerificationError`. |
| `test_jwt_verify_issuer_mismatch` | Construct a JWT with wrong `iss`; MUST raise `JWSVerificationError`. |
| `test_audit_log_write` | Call `audit(...)` with fixture DB; assert exactly one row inserted with correct fields. |
| `test_sso_config_file_permissions` | After `cmd_mcp_sso_configure`, assert `sso_config.yaml` has mode `0600`. |
| `test_no_import_without_config` | Assert `tag.mcp_auth` not in `sys.modules` after importing `tag.controller` when no `sso_config.yaml` exists. |
| `test_okta_discovery_url` | `OktaAdapter(config).discovery_url()` returns `https://<tenant>/.well-known/openid-configuration`. |
| `test_azure_ad_tenant_normalization` | Azure AD adapter normalizes `acme.onmicrosoft.com` → GUID lookup; raw GUID passes through unchanged. |
| `test_google_workspace_groups_api_call` | Mock `httpx.AsyncClient`; assert Directory API call is made with access token in `Authorization` header when `directory_api_enabled=true`. |
| `test_token_exchange_resource_param` | Mock `httpx.AsyncClient`; call `exchange_token_for_server`; assert `resource=<server_uri>` in POST body. |
| `test_token_exchange_unsupported_falls_back` | HTTP 400 `unsupported_grant_type` MUST raise `TokenExchangeNotSupportedError`, not `TokenExchangeError`. |
| `test_keychain_no_disk_writes` | After `exchange_token_for_server`, assert no new files in `~/.tag/` and no token value in SQLite. |
| `test_scope_map_import_yaml` | Parse a scope policy YAML; assert all rows written to `sso_scope_maps` with correct column values. |
| `test_jwks_cache_ttl` | Call `fetch_jwks` twice within 5 minutes; assert only one HTTP request made (LRU cache hit). |
| `test_state_param_csrf` | Simulate callback with wrong `state`; assert login aborts with `SSOStateError`. |

### 11.2 Integration Tests

Each test requires a sandbox IdP tenant (Okta dev tenant, Azure AD app registration, Google Cloud OAuth client). CI uses environment variables to inject tenant IDs and test credentials.

| Test | Description |
|------|-------------|
| `test_okta_full_flow` | `configure → login (device flow) → status → exchange → token show → logout` against Okta dev tenant. |
| `test_azure_ad_full_flow` | Same flow against Azure AD v2.0 sandbox tenant. |
| `test_google_workspace_full_flow` | Same flow against Google Workspace dev account with Directory API. |
| `test_token_refresh_on_expiry` | Inject a short-lived access token (1 minute TTL); sleep until expiry; trigger an MCP server session start; assert refresh happened and exchange succeeded. |
| `test_revocation_on_logout` | After logout, attempt to use the old refresh token at the IdP's token endpoint; assert HTTP 400 `invalid_grant`. |
| `test_scope_escalation_in_exchange` | Configure scope map with `read:query` only; attempt to construct a request for `write:query`; assert blocked and audited. |
| `test_audit_log_completeness` | Run full login→exchange→logout cycle; assert `sso_audit_log` contains exactly: 1 `login`, N `exchange` (one per server), 1 `logout`. |
| `test_headless_detection` | Set `DISPLAY=""`, `SSH_TTY=""`, `TAG_HEADLESS=1`; assert device flow is automatically selected. |

### 11.3 Performance Tests

| Test | Target | Method |
|------|--------|--------|
| Token exchange latency | P95 < 500 ms | Run 50 exchanges against a mock AS with 50 ms artificial latency; assert P95 threshold. |
| Startup overhead (no SSO) | < 5 ms delta | `time tag --version` vs. `time tag --version` with SSO configured but no active session; Wilcoxon signed-rank test. |
| Scope resolution throughput | > 10,000 lookups/sec | Benchmark `resolve_scope_policy` with in-memory SQLite; 100 groups, 50 servers. |
| JWKS cache hit rate | > 95% on repeated exchanges | Run 100 exchanges in 4 minutes; assert `httpx.get` called ≤ 2 times. |

---

## 12. Acceptance Criteria

| ID | Criterion | Testable |
|----|-----------|---------|
| AC-01 | `tag mcp sso configure --idp okta --tenant acme.okta.com --client-id <id>` writes `~/.tag/sso_config.yaml` with mode `0600`, containing no token values. | Yes — file permissions + grep for token patterns |
| AC-02 | `tag mcp sso configure --test` exits 0 when IdP is reachable, exits 1 when not, and writes no config in either case. | Yes — integration test with mock IdP |
| AC-03 | `tag mcp sso login` completes PKCE S256 authorization code flow and stores IdP access and refresh tokens in the OS keychain. Zero token bytes appear in any file under `~/.tag/`. | Yes — file scan post-login |
| AC-04 | `tag mcp sso login` automatically uses device code flow when `TAG_HEADLESS=1` is set. | Yes — env var injection in test |
| AC-05 | `tag mcp sso status` displays subject (`sub`), email, groups, and token expiry from the active session. | Yes — parse stdout |
| AC-06 | `tag mcp sso logout --revoke` sends RFC 7009 revocation POST to the IdP; subsequent use of the refresh token returns HTTP 400 `invalid_grant`. | Yes — integration test |
| AC-07 | `tag mcp sso logout` removes all keychain entries (access, refresh, all server tokens). `keyring.get_password("tag-sso", ...)` returns `None` for all known keys after logout. | Yes — keyring assertion |
| AC-08 | When a user in group `data-engineers` connects to `io.github.acme/db-mcp` with scope map `read:query,write:query`, the exchanged token's scope claim contains exactly `read:query write:query` (no more, no less, per intersection). | Yes — token claims assertion |
| AC-09 | When a user is NOT in any mapped group for a server, token exchange for that server raises `ScopeEscalationError` and writes a `scope_escalation/blocked` row to `sso_audit_log`. | Yes — exception type + DB assertion |
| AC-10 | Token exchange request includes `resource=<server_uri>` (RFC 8707). Verified by intercepting the HTTP POST body in integration test. | Yes — HTTP request capture |
| AC-11 | JWTs with `alg=none`, expired `exp`, or mismatched `iss` are rejected by `verify_jwt` with `JWSVerificationError`. | Yes — unit tests |
| AC-12 | `tag mcp sso token show --server <name>` prints decoded claims including `sub`, `email`, `scope`, `exp`, and does not print the raw token string. | Yes — stdout parse; assert raw token not present |
| AC-13 | `tag mcp sso scope map --server X --group G --scopes S` writes a row to `sso_scope_maps` and `tag mcp sso scope map list` displays it. | Yes — DB + stdout |
| AC-14 | `tag mcp sso scope map import --file policy.yaml` correctly inserts all rows from the YAML file and reports the count of mappings added/updated. | Yes — DB row count assertion |
| AC-15 | `tag run` with no `sso_config.yaml` does not import `tag.mcp_auth` (assert `"tag.mcp_auth" not in sys.modules`). | Yes — `sys.modules` inspection in test |
| AC-16 | When the IdP access token expires, `mcp_auth.py` silently refreshes and retries token exchange without user intervention. The `sso_audit_log` records a `refresh/success` event. | Yes — integration test with short-lived token |
| AC-17 | When the refresh token is also expired/revoked, `mcp_auth.py` raises `SSOSessionExpiredError`, emits a notification via PRD-040, and does NOT fall back to unauthenticated access. | Yes — mock IdP returning `invalid_grant` |
| AC-18 | The `sso_audit_log` table contains exactly one row per SSO event, with correct `event_type`, `subject`, `outcome`, and `server_name` for login, exchange, and logout in the full-cycle integration test. | Yes — DB row assertions |
| AC-19 | All three IdP providers (Okta, Azure AD, Google Workspace) pass the full 30-case integration test suite. | Yes — CI matrix |
| AC-20 | `tag mcp sso audit --sub <sub> --export jsonl` produces a valid JSONL file with one JSON object per row, all having the queried subject. | Yes — parse output |

---

## 13. Dependencies

| Dependency | Type | Notes |
|-----------|------|-------|
| `keyring >= 24.0` | Runtime | OS keychain abstraction; already used or easily added. `keyrings.alt` as fallback on headless Linux. |
| `cryptography >= 42.0` | Runtime | JWS RSA signature verification; widely used in Python ecosystem. |
| `httpx >= 0.27` | Runtime | Already used in TAG for MCP registry calls (PRD-014). |
| `PyYAML >= 6.0` | Runtime | Already used in TAG for profile/config YAML. |
| PRD-013 | Internal | OTel tracing for SSO span emission and redaction. |
| PRD-034 | Internal | Secret scanner patterns extended with IdP token patterns. |
| PRD-040 | Internal | Notification hooks for `SSOSessionExpiredError` and scope escalation events. |
| PRD-014 | Internal | MCP server registry provides `server_uri` values for RFC 8707 `resource` parameter. |
| PRD-041 | Internal | Per-span cost attribution; SSO exchange spans need redaction before OTLP export. |
| Okta dev tenant | External | Sandbox Okta org for integration tests (free developer account). |
| Azure AD app registration | External | Azure AD v2.0 app with `oidc` and `groups` permissions for integration tests. |
| Google Cloud OAuth client | External | OAuth 2.0 client with `openid`, `email`, `profile`, Directory API read scope. |
| `sqlite3 >= 3.37` | System | Required for `STRICT` table mode in `sso_audit_log`. |

---

## 14. Open Questions

| ID | Question | Owner | Resolution Target |
|----|----------|-------|-------------------|
| OQ-01 | Does the target MCP server's Authorization Server need to explicitly support RFC 8693 `token-exchange` grant type, or can we negotiate this via PRM metadata? Some self-hosted MCP servers may not expose this grant. Should we maintain a compatibility list? | Platform team | Phase 1 design review |
| OQ-02 | Google Workspace does not include group membership in the OIDC ID token. The current design calls the Directory API at login time. Should groups be re-fetched at each token exchange (fresher but slower) or only at login and refresh (stale for long sessions)? | Security team | Phase 1 |
| OQ-03 | Azure AD Conditional Access Policies can require MFA for specific resources. If the MCP server's AS requires a higher authentication context than the IdP token carries, the exchange fails. Should TAG prompt for step-up authentication, or fail with a clear error? | UX team | Phase 2 |
| OQ-04 | WorkOS is listed in the inspiration but the IdP list is Okta/Azure AD/Google. WorkOS acts as a broker that supports SSO via those three IdPs plus many others. Should WorkOS be implemented as a fourth first-class `IdPAdapter` (accepting any OIDC endpoint it exposes), or deferred post-v1? | Product | Phase 1 scoping |
| OQ-05 | The `sso_scope_maps` table stores policies locally per developer workstation. Enterprise teams need centralized policy management. Should scope policy be shareable via a file in the team's profile repo (checked in), loaded at startup? What is the conflict resolution if local and repo policies disagree? | Platform team | Phase 2 |
| OQ-06 | `keyring` on headless Linux CI environments (GitHub Actions) requires `keyrings.alt` or environment variable fallback. Should TAG auto-detect CI (`CI=true`) and use a memory-only keyring backed by an encrypted file for the test run duration? | Engineering | Before Phase 1 tests |
| OQ-07 | The loopback redirect server opens a random port. Firewalls or security software (e.g., CrowdStrike) may block loopback TCP connections on non-80 ports. Should the redirect URI be configurable to port 80 (requires sudo on some systems) or should we add a `/etc/hosts` workaround? | UX team | Phase 1 |
| OQ-08 | SAML 2.0 is listed as an inspiration but is explicitly excluded from scope (NG1). Several enterprise customers use SAML-only IdPs that do not expose OIDC. Should SAML assertion parsing be added as a future PRD, or can it be addressed via a WorkOS bridge in all cases? | Product | Post-launch |
| OQ-09 | The `sso_audit_log` table has no built-in retention policy. For long-running installations it will grow unboundedly. Should there be an automatic compaction (e.g., keep 90 days) or should this be left to a separate `tag db vacuum` command? | Platform team | Phase 2 |

---

## 15. Complexity and Timeline

### Phase 1 — Core IdP Integration and Token Storage (2 weeks)

**Days 1–3:** Scaffold `mcp_auth.py` with dataclasses, `IdPAdapter` ABC, `KeychainKey` enum, `SSOConfig`, `SSOSession`, `TokenExchangeResult`, and `ScopeMappingPolicy`. Write `_pkce_pair()`, `_build_auth_url()`, and the loopback redirect server handler. Write all unit tests for these components (FR-01 through FR-06).

**Days 4–6:** Implement `OktaAdapter`: discovery URL derivation, `extract_groups` from `groups` claim, JWKS fetch and caching, `verify_jwt`. Write integration test against Okta dev tenant. Implement `cmd_mcp_sso_configure` in `controller.py` with OIDC discovery validation and `0600` file write. Write `cmd_mcp_sso_login` browser flow with state + PKCE.

**Days 7–8:** Implement `cmd_mcp_sso_status` with keychain reads and per-server exchange display. Implement `cmd_mcp_sso_logout` with RFC 7009 revocation. Write `audit()` function and all four SQLite tables (DDL migration in `open_db()`). Verify AC-01 through AC-07.

**Days 9–10:** Implement device code flow (RFC 8628) with polling and exponential backoff. Implement `is_headless()` detection. Write headless integration test. Implement `cmd_mcp_sso_token_show` with claim decoding and redaction. Verify AC-03, AC-04, AC-12.

### Phase 2 — Azure AD and Google Workspace Adapters (1.5 weeks)

**Days 11–12:** Implement `AzureADAdapter`: tenant GUID normalization, v2.0 discovery URL, groups claim parsing (note: Azure AD requires app manifest change to include `groups` claim; document this prerequisite). Write integration test against Azure AD sandbox.

**Days 13–14:** Implement `GoogleWorkspaceAdapter`: OIDC flow + Directory API call for group fetch. Handle `directory_api_enabled=false` gracefully (warn that groups cannot be resolved). Write integration test against Google Cloud OAuth sandbox.

**Days 15:** Implement `WorkOSAdapter` as a thin wrapper (any OIDC endpoint WorkOS exposes). Resolve OQ-04. Write smoke test. Run full three-IdP CI matrix.

### Phase 3 — Scope Mapping and Token Exchange (1 week)

**Days 16–17:** Implement `resolve_scope_policy()` with three-way intersection and `sso_scope_maps` DB lookups. Write 30+ unit test vectors. Implement `exchange_token_for_server()` with RFC 8693 POST construction and `resource` parameter. Write HTTP request capture test (AC-10).

**Days 18–19:** Implement `cmd_mcp_sso_scope_map` (add/remove/list/import). Write scope policy YAML parser with validation. Implement `_get_mcp_auth_headers()` hook in `controller.py` with lazy `mcp_auth` import and `TokenExchangeNotSupportedError` fallback. Verify AC-08, AC-09, AC-10, AC-13, AC-14, AC-15.

**Day 20:** Implement automatic token refresh (`check_and_refresh_idp_token()`), `SSOSessionExpiredError` → PRD-040 notification, and `SSOIdPUnreachableError` for network partition. Write token expiry integration test. Verify AC-16, AC-17.

### Phase 4 — Audit, Observability, Security Hardening (0.5 weeks)

**Days 21–22:** Integrate audit log writes into all event paths. Implement `cmd_mcp_sso_audit` with filtering and CSV/JSONL export. Add OTel span emission with PRD-034 redaction for token attributes. Extend PRD-034 secret scanner with IdP token regex patterns. Run full acceptance criteria verification (AC-01 through AC-20). Write performance benchmarks (token exchange P95, startup overhead). Write final integration test matrix.

**Total: ~4.5 weeks core implementation + 0.5 weeks buffer = 5 weeks**

The XL estimate (4-8 weeks) accounts for: IdP sandbox provisioning delays (typically 1-2 days per IdP for approval), potential firewall/keychain issues in CI environments (OQ-06, OQ-07), and the possibility that Azure AD Conditional Access (OQ-03) requires a Phase 2 step-up authentication implementation.

