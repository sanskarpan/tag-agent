# PRD-080: Enterprise IdP SSO Across MCP Servers (`tag mcp sso`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** XL (4-8 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `internal/mcp` (new `sso.go`, `sso_adapters*.go`), `internal/cli` (new `mcp_sso.go` cobra handlers), `internal/store` (new `sso.go`), `internal/config` (new `sso.go`), `internal/credentials` (SSO keychain helpers)
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (secret scanning), PRD-014 (MCP server registry), PRD-040 (notification hooks), PRD-041 (OTel span/cost attribution)
**Inspired by:** Okta SSO, Azure AD SAML/OIDC, WorkOS enterprise SSO

---

## 1. Overview

Enterprise teams deploying TAG across multiple engineers face an identity problem that compounds with every MCP server added to the stack. Each MCP server — whether it wraps a GitHub API, an internal data warehouse, a Jira instance, or a Salesforce org — maintains its own credential store. Today a developer must configure each server independently: generate tokens, rotate secrets, manage expiry, and repeat the process for every tool they enable. When an employee is offboarded, each credential must be revoked server-by-server with no central control plane. When a new engineer joins, onboarding requires manual credential provisioning across a dozen systems. This is not merely inconvenient; it is a security posture failure. Credentials rotate at different cadences, some never rotate, and there is no audit trail correlating user identity to MCP tool invocations.

Enterprise Identity Providers — Okta, Azure Active Directory, Google Workspace, and platforms like WorkOS that federate them — already solve this problem for web applications via OIDC (OpenID Connect) and SAML 2.0. The organization's IdP becomes the single authority for user identity, group membership, and access policy. Applications trust the IdP's tokens rather than maintaining their own user stores. Revocation is instantaneous: disabling a user in Okta immediately revokes access to all connected applications. Group-based access policies mean that adding a developer to the "backend-engineers" group automatically grants the right tool scopes on every MCP server that respects that group.

This PRD introduces `tag mcp sso`: a subsystem in `internal/mcp` that integrates TAG with enterprise IdPs using OIDC and SAML 2.0 token exchange. When a user runs `tag mcp sso login`, TAG performs a browser-based OIDC authorization code flow (with PKCE) against the configured IdP via `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2`. The resulting ID token and access token are stored in the OS keychain via `github.com/zalando/go-keyring`. When TAG starts an MCP server session, `internal/mcp` exchanges the IdP token for a server-specific access token using the OAuth 2.0 Token Exchange flow (RFC 8693), propagating verified user identity to the MCP server without the user ever touching per-server credentials. The go-sdk's Enterprise Managed Auth support (`github.com/modelcontextprotocol/go-sdk v1.6.1`) injects the exchanged token into per-user MCP client sessions. Per-server scope mapping is derived from IdP group membership: the `sso_scope_maps` SQLite table (via `modernc.org/sqlite`) maps IdP groups to MCP server scope lists, enabling fine-grained, centrally-managed access control.

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
| G2 | `tag mcp sso login` performs a browser-based OIDC authorization code flow with PKCE S256 (via `golang.org/x/oauth2`), stores tokens in the OS keychain via `go-keyring`, and prints token expiry. |
| G3 | `tag mcp sso status` displays the current SSO session state: IdP, tenant, authenticated user (`sub`, `email`), token expiry, and per-server exchange status. |
| G4 | `tag mcp sso logout` revokes the IdP refresh token (if the IdP supports RFC 7009 token revocation), removes all SSO tokens from the keychain, and clears per-server exchanged tokens. |
| G5 | At MCP server session start, `internal/mcp` detects SSO configuration via `config.SSOConfigured()`, silently performs RFC 8693 token exchange to obtain a server-scoped access token, and injects it into the go-sdk MCP client session via an `http.RoundTripper` auth transport — without user intervention. |
| G6 | Per-server scope mapping from IdP groups is stored in the `sso_scope_maps` table and applied at token exchange time, so group membership from the IdP token controls which OAuth scopes are requested per server. |
| G7 | Support Okta (OIDC), Azure AD (OIDC and SAML 2.0 via OIDC bridge), and Google Workspace (OIDC) as first-class IdP targets with provider-specific discovery and metadata parsing. |
| G8 | The `sso_audit_log` SQLite table records every SSO event (login, logout, token exchange, exchange failure, scope escalation attempt) with IdP subject, server name, scopes granted, and timestamp. |
| G9 | `tag mcp sso scope map --server io.github.acme/db-mcp --group data-engineers --scopes read:query,write:query` manages scope mappings without editing YAML by hand. |
| G10 | When the IdP access token expires, `internal/mcp` silently refreshes using the stored refresh token via `golang.org/x/oauth2`'s `TokenSource`; if refresh fails (revoked, expired), it emits a notification (PRD-040) and blocks tool calls on the affected server until re-login. |
| G11 | `tag mcp sso token show --server <name>` displays the decoded claims of the exchanged token for a specific server (redacting the raw token; showing only `sub`, `email`, `groups`, `scope`, `exp`). |
| G12 | Zero SSO-related work executes when SSO is not configured. `config.SSOConfigured()` (a single `os.Stat` call) short-circuits all SSO code paths at the call site; the compiled-in package incurs no measurable startup overhead. |

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
| FR-06 | All tokens (IdP access token, refresh token, per-server exchanged tokens) MUST be stored exclusively in the OS keychain via `github.com/zalando/go-keyring` (service name: `tag-sso`; account names defined as constants in `internal/credentials`). Zero token data MUST be written to disk files, SQLite, or environment variables. | P0 |
| FR-07 | At MCP server session start, `internal/mcp` MUST attempt RFC 8693 token exchange if and only if all three conditions hold: (a) `config.SSOConfigured()` returns true (i.e., `~/.tag/sso_config.yaml` exists), (b) a valid (non-expired) IdP access token is in the keychain, (c) the MCP server's AS metadata exposes a `token_endpoint` that accepts `urn:ietf:params:oauth:grant-type:token-exchange`. If the server's AS does not support token exchange, `internal/mcp` MUST fall back to the existing per-server OAuth flow silently. | P0 |
| FR-08 | Token exchange requests (RFC 8693) MUST include the `resource` parameter (RFC 8707) set to the MCP server's canonical URI, and the `subject_token_type` set to `urn:ietf:params:oauth:token-type:access_token`. | P0 |
| FR-09 | Scope selection at token exchange time MUST be computed as the intersection of: (a) scopes supported by the MCP server (from AS metadata), (b) scopes mapped from the user's IdP groups via `sso_scope_maps`, and (c) scopes present in the IdP access token. The system MUST NOT request scopes the user is not entitled to via their groups. | P0 |
| FR-10 | If the IdP access token has expired and a refresh token is present, `internal/mcp` MUST silently refresh the access token via `golang.org/x/oauth2`'s `TokenSource.Token()` before attempting token exchange. If refresh fails (HTTP 400 with `error=invalid_grant`), `internal/mcp` MUST return `*SSOSessionExpiredError` and emit a notification via PRD-040 hooks. | P1 |
| FR-11 | Every SSO event (login, logout, token exchange, token refresh, exchange failure, revocation attempt, scope escalation attempt) MUST be written to the `sso_audit_log` SQLite table within 500 ms of the event occurring. | P0 |
| FR-12 | `tag mcp sso logout` MUST attempt RFC 7009 token revocation (`POST` to the IdP's `revocation_endpoint`) for both the access token and refresh token. Revocation failure MUST be logged but MUST NOT block local cleanup. Local keychain entries MUST be deleted regardless of revocation outcome. | P1 |
| FR-13 | `tag mcp sso scope map` MUST validate that the specified group name is present in the IdP access token's `groups` claim (or comparable claim per IdP) before writing the mapping, or emit a warning if validation cannot be performed offline. | P1 |
| FR-14 | `tag mcp sso token show` MUST decode and display JWT claims without printing the raw token string. The raw token value MUST be redacted in all CLI output and log output. | P0 |
| FR-15 | The SSO code path in `internal/mcp` MUST NOT execute at TAG startup unless `~/.tag/sso_config.yaml` exists. `config.SSOConfigured()` (an `os.Stat` call) MUST be the sole gate; the package is compiled in but incurs zero work when the file is absent. Verified by the NFR-03 startup benchmark. | P1 |
| FR-16 | For Azure AD, the OIDC discovery URL MUST be constructed as `https://login.microsoftonline.com/<tenant-id>/v2.0/.well-known/openid-configuration` with tenant ID normalization (accept both GUID and `<domain>.onmicrosoft.com`). | P1 |
| FR-17 | For Google Workspace, the groups claim MUST be obtained via the Google Directory API (using the access token) since Google's OIDC ID token does not include group membership. The `GoogleWorkspaceAdapter.ExtractGroups()` method MUST call the Directory API at login time and cache the group list in `sso_session_cache`. | P1 |
| FR-18 | `tag mcp sso configure --test` MUST verify IdP connectivity without writing any configuration or performing authentication. Exit code MUST be 0 on success, 1 on connectivity failure. | P1 |
| FR-19 | JWS signature verification of IdP-issued JWT tokens MUST be performed using `github.com/coreos/go-oidc/v3`'s `IDTokenVerifier`, which fetches and caches the IdP's JWKS automatically. TAG MUST NOT accept tokens whose signature cannot be verified. `alg=none` is rejected by the library unconditionally. JWKS remote-key cache TTL is 5 minutes (library default). | P0 |
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
| NFR-07 | **Concurrency** | Multiple concurrent `tag run` goroutines or processes (e.g., in a `tag swarm` scenario) MUST each independently hold their own SSO token exchange result; they MUST NOT race on shared keychain writes. `go-keyring`'s OS-level atomic set semantics provide this guarantee; no additional mutex is required for the keychain layer. In-process concurrent exchanges for different servers run in separate goroutines with no shared mutable state beyond the keychain. |
| NFR-08 | **IdP Compatibility** | All three IdPs (Okta, Azure AD, Google Workspace) MUST pass the full integration test suite. Provider-specific quirks MUST be handled in the `IdPAdapter` interface implementations (`OktaAdapter`, `AzureADAdapter`, `GoogleWorkspaceAdapter` structs in `internal/mcp`), not with `if idp == "okta"` conditionals in shared code. |
| NFR-09 | **Error Messages** | All authentication failures MUST produce human-readable error messages that include: the failed operation, the HTTP status (if applicable), the IdP endpoint that failed, and the next action the user should take. |
| NFR-10 | **Dependency Footprint** | New Go module dependencies are limited to: `github.com/zalando/go-keyring` (OS keychain; pure-Go + OS API), `github.com/coreos/go-oidc/v3` (OIDC provider + JWKS verifier), `golang.org/x/oauth2` (token exchange, PKCE, device flow — already a transitive dependency via go-sdk). No CGO dependencies introduced; `CGO_ENABLED=0` is preserved. |
| NFR-11 | **Secrets in Logs** | The OTel tracing layer (PRD-013, `go.opentelemetry.io/otel`) MUST redact token values from any span attributes before export. `internal/mcp` MUST never pass raw token strings to `slog` calls or OTel span attributes. |
| NFR-12 | **Graceful Degradation** | If the IdP is unreachable at session start (network partition), `internal/mcp` MUST use the cached (non-expired) exchanged token from the keychain if available, or return `*SSOIdPUnreachableError` with a clear message. MUST NOT silently fall back to unauthenticated access. |

---

## 9. Technical Design

### 9.1 New Go Packages

| Package / File | Purpose |
|----------------|---------|
| `internal/mcp/sso.go` | Core SSO logic: PKCE flow, token exchange, scope resolution, session refresh, error types |
| `internal/mcp/sso_adapters.go` | `IdPAdapter` interface + `AdapterFor()` registry |
| `internal/mcp/sso_adapters_okta.go` | `OktaAdapter` — OIDC discovery URL, `groups` claim extraction |
| `internal/mcp/sso_adapters_azure.go` | `AzureADAdapter` — v2.0 discovery URL, tenant GUID normalization |
| `internal/mcp/sso_adapters_google.go` | `GoogleWorkspaceAdapter` — OIDC + Directory API groups fetch |
| `internal/mcp/sso_adapters_workos.go` | `WorkOSAdapter` — thin wrapper over any OIDC endpoint WorkOS exposes |
| `internal/credentials/sso.go` | Keychain helpers wrapping `github.com/zalando/go-keyring`; canonical key constants |
| `internal/cli/mcp_sso.go` | Cobra subcommands: `configure`, `login`, `logout`, `status`, `scope map`, `token show`, `audit` |
| `internal/store/sso.go` | SQLite queries for `sso_*` tables via `modernc.org/sqlite` (`database/sql` interface) |
| `internal/config/sso.go` | `SSOConfig` load/merge via `koanf/v2` + `~/.tag/sso_config.yaml` write via `yaml.v3` + `gofrs/flock` |
| `internal/mcp/sso_test.go` | Unit + integration tests (`go test` + `testify` + `net/http/httptest`) |
| `testdata/sso/` | Mock OIDC discovery documents, JWKS, token responses for each IdP |

### 9.2 SQLite DDL

All tables are created by `internal/store`'s migration runner and share the WAL-mode `tag.sqlite3` database at `~/.tag/runtime/tag.sqlite3`, accessed via `modernc.org/sqlite` (pure-Go, `CGO_ENABLED=0`). The `STRICT` keyword on `sso_audit_log` requires modernc 1.29+ (SQLite 3.37+).

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

### 9.3 Core Structs

```go
// internal/mcp/sso.go

package mcp

import "time"

// IdPType identifies the enterprise identity provider.
type IdPType string

const (
    IdPTypeOkta            IdPType = "okta"
    IdPTypeAzureAD         IdPType = "azure-ad"
    IdPTypeGoogleWorkspace IdPType = "google-workspace"
    IdPTypeWorkOS          IdPType = "workos"
)

// Keychain constants. Service is always KeychainService; per-server account
// keys are constructed as "server-token:<server_name>".
const (
    KeychainService    = "tag-sso"
    KeyIDPAccessToken  = "idp-access-token"
    KeyIDPRefreshToken = "idp-refresh-token"
)

// SSOConfig is loaded from ~/.tag/sso_config.yaml (0600; no secrets stored here).
// koanf/v2 merges env overrides; yaml.v3 writes it back atomically via gofrs/flock.
type SSOConfig struct {
    IdPType      IdPType  `yaml:"idp_type"                 koanf:"idp_type"`
    Tenant       string   `yaml:"tenant"                   koanf:"tenant"`
    ClientID     string   `yaml:"client_id"                koanf:"client_id"`
    RedirectURI  string   `yaml:"redirect_uri"             koanf:"redirect_uri"`
    Scopes       []string `yaml:"scopes"                   koanf:"scopes"`
    DeviceFlow   bool     `yaml:"device_flow"              koanf:"device_flow"`
    DiscoveryURL string   `yaml:"discovery_url,omitempty"  koanf:"discovery_url"` // auto-derived if empty
}

// OIDCMetadata holds the fields parsed from the IdP's OIDC discovery document.
type OIDCMetadata struct {
    Issuer                            string   `json:"issuer"`
    AuthorizationEndpoint             string   `json:"authorization_endpoint"`
    TokenEndpoint                     string   `json:"token_endpoint"`
    JWKsURI                           string   `json:"jwks_uri"`
    DeviceAuthorizationEndpoint       string   `json:"device_authorization_endpoint,omitempty"`
    RevocationEndpoint                string   `json:"revocation_endpoint,omitempty"`
    TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported,omitempty"`
}

// SSOSession holds non-sensitive session metadata; no token values stored here.
type SSOSession struct {
    Subject         string
    Email           string
    Groups          []string
    AccessTokenExp  time.Time
    RefreshTokenExp time.Time // zero value if IdP does not issue refresh tokens
    IdPType         IdPType
    Tenant          string
}

func (s *SSOSession) IsAccessTokenValid(clockSkew time.Duration) bool {
    return time.Now().Add(clockSkew).Before(s.AccessTokenExp)
}

func (s *SSOSession) IsRefreshTokenValid() bool {
    return !s.RefreshTokenExp.IsZero() && time.Now().Before(s.RefreshTokenExp)
}

// TokenExchangeResult holds metadata about a completed RFC 8693 exchange; no raw token.
type TokenExchangeResult struct {
    ServerName      string
    ServerURI       string
    ScopesGranted   []string
    TokenExp        time.Time
    KeychainAccount string // go-keyring account key; raw token stored there
}

// ScopeMappingPolicy is the resolved scope policy for one user+server pair.
type ScopeMappingPolicy struct {
    ServerName      string
    UserGroups      []string
    MappedScopes    []string // union of all sso_scope_maps rows for this server+groups
    ServerScopes    []string // scopes advertised by the MCP server AS
    IdPTokenScopes  []string // scopes present in the IdP access token
    EffectiveScopes []string // three-way intersection — what gets requested
}
```

### 9.4 IdP Adapter Interface

```go
// internal/mcp/sso_adapters.go

package mcp

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
)

// IdPAdapter is the interface each enterprise IdP adapter must satisfy.
// Provider-specific quirks are confined here; shared SSO logic never
// switches on IdPType directly.
type IdPAdapter interface {
    // DiscoveryURL returns the OIDC /.well-known/openid-configuration URL.
    DiscoveryURL() string
    // ExtractGroups returns group names for the authenticated user.
    // Some IdPs (Okta) embed groups in the ID token claim directly.
    // Others (Google Workspace) require a secondary Directory API call.
    ExtractGroups(ctx context.Context, idTokenClaims map[string]any, accessToken string, hc *http.Client) ([]string, error)
    // NormalizeTenant converts any accepted tenant representation to canonical form.
    NormalizeTenant(tenant string) string
}

// FetchOIDCMetadata fetches and validates the IdP discovery document.
// Called by all adapters via a shared helper — no per-IdP duplication.
func FetchOIDCMetadata(ctx context.Context, discoveryURL string, hc *http.Client) (*OIDCMetadata, error) {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
    if err != nil {
        return nil, fmt.Errorf("build discovery request: %w", err)
    }
    resp, err := hc.Do(req)
    if err != nil {
        return nil, fmt.Errorf("fetch OIDC metadata from %s: %w", discoveryURL, err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("OIDC discovery returned HTTP %d from %s", resp.StatusCode, discoveryURL)
    }
    var meta OIDCMetadata
    if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
        return nil, fmt.Errorf("decode OIDC metadata: %w", err)
    }
    if meta.Issuer == "" || meta.AuthorizationEndpoint == "" ||
        meta.TokenEndpoint == "" || meta.JWKsURI == "" {
        return nil, fmt.Errorf("OIDC metadata missing required fields (issuer/authorization_endpoint/token_endpoint/jwks_uri)")
    }
    return &meta, nil
}

// adapterRegistry maps IdPType to its constructor.
var adapterRegistry = map[IdPType]func(*SSOConfig) IdPAdapter{
    IdPTypeOkta:            func(c *SSOConfig) IdPAdapter { return &OktaAdapter{cfg: c} },
    IdPTypeAzureAD:         func(c *SSOConfig) IdPAdapter { return &AzureADAdapter{cfg: c} },
    IdPTypeGoogleWorkspace: func(c *SSOConfig) IdPAdapter { return &GoogleWorkspaceAdapter{cfg: c} },
    IdPTypeWorkOS:          func(c *SSOConfig) IdPAdapter { return &WorkOSAdapter{cfg: c} },
}

// AdapterFor returns the IdPAdapter for the given SSO config.
func AdapterFor(cfg *SSOConfig) (IdPAdapter, error) {
    ctor, ok := adapterRegistry[cfg.IdPType]
    if !ok {
        return nil, fmt.Errorf("unsupported IdP type: %q", cfg.IdPType)
    }
    return ctor(cfg), nil
}
```

### 9.5 PKCE and Authorization Code Flow

```go
// internal/mcp/sso.go (continued)

import (
    "crypto/rand"
    "crypto/sha256"
    "encoding/base64"
    "fmt"
    "net"
    "net/url"
    "strings"
)

// pkceS256Pair generates a PKCE (code_verifier, code_challenge) pair using the
// S256 method (RFC 7636 §4.2). Uses crypto/rand for verifier entropy.
func pkceS256Pair() (verifier, challenge string, err error) {
    raw := make([]byte, 32)
    if _, err = rand.Read(raw); err != nil {
        return "", "", fmt.Errorf("generate PKCE verifier entropy: %w", err)
    }
    verifier = base64.RawURLEncoding.EncodeToString(raw)
    sum := sha256.Sum256([]byte(verifier))
    challenge = base64.RawURLEncoding.EncodeToString(sum[:])
    return verifier, challenge, nil
}

// buildAuthURL constructs the OIDC authorization request URL.
// code_challenge_method is always "S256"; "plain" is never used.
func buildAuthURL(meta *OIDCMetadata, cfg *SSOConfig, state, codeChallenge string, port int) string {
    q := url.Values{
        "response_type":         {"code"},
        "client_id":             {cfg.ClientID},
        "redirect_uri":          {fmt.Sprintf("http://localhost:%d/callback", port)},
        "scope":                 {strings.Join(cfg.Scopes, " ")},
        "state":                 {state},
        "code_challenge":        {codeChallenge},
        "code_challenge_method": {"S256"},
    }
    return meta.AuthorizationEndpoint + "?" + q.Encode()
}

// pickEphemeralPort binds a random OS-assigned port in the ephemeral range and
// immediately releases the listener, returning the port number for the redirect server.
func pickEphemeralPort() (int, error) {
    l, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        return 0, fmt.Errorf("pick loopback port: %w", err)
    }
    port := l.Addr().(*net.TCPAddr).Port
    _ = l.Close()
    return port, nil
}

// The loopback redirect handler is a minimal net/http server spun up for the
// duration of the login flow. It captures the authorization code and state
// parameters from the redirect URI, verifies state, then shuts itself down.
```

### 9.6 RFC 8693 Token Exchange Algorithm

```go
// internal/mcp/sso.go (continued)

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "strings"
    "time"

    keyring "github.com/zalando/go-keyring"
)

// exchangeTokenForServer performs RFC 8693 token exchange: trades the IdP access
// token for a server-scoped access token. The `resource` parameter (RFC 8707)
// is mandatory and set to serverURI. Identity is propagated via context.Context.
func exchangeTokenForServer(
    ctx context.Context,
    serverName, serverURI, tokenEndpoint, clientID string,
    policy *ScopeMappingPolicy,
    hc *http.Client,
) (*TokenExchangeResult, error) {
    if len(policy.EffectiveScopes) == 0 {
        return nil, &ScopeEscalationError{ServerName: serverName,
            Detail: "no scopes authorized for current group membership"}
    }

    idpAccessToken, err := keyring.Get(KeychainService, KeyIDPAccessToken)
    if err != nil || idpAccessToken == "" {
        return nil, &SSOSessionExpiredError{Detail: "no IdP access token in keychain — run `tag mcp sso login`"}
    }

    form := url.Values{
        "grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
        "client_id":            {clientID},
        "subject_token":        {idpAccessToken},
        "subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
        "requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
        "resource":             {serverURI}, // RFC 8707 — MANDATORY
        "scope":                {strings.Join(policy.EffectiveScopes, " ")},
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint,
        strings.NewReader(form.Encode()))
    if err != nil {
        return nil, fmt.Errorf("build token-exchange request: %w", err)
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

    resp, err := hc.Do(req)
    if err != nil {
        return nil, fmt.Errorf("token exchange POST to %s: %w", tokenEndpoint, err)
    }
    defer resp.Body.Close()
    body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

    if resp.StatusCode == http.StatusBadRequest {
        var oauthErr struct {
            Error       string `json:"error"`
            Description string `json:"error_description"`
        }
        _ = json.Unmarshal(body, &oauthErr)
        if oauthErr.Error == "unsupported_grant_type" {
            return nil, &TokenExchangeNotSupportedError{ServerName: serverName}
        }
        return nil, &TokenExchangeError{ServerName: serverName,
            Code: oauthErr.Error, Detail: oauthErr.Description}
    }
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("token exchange HTTP %d for %s", resp.StatusCode, serverName)
    }

    var tokenResp struct {
        AccessToken string `json:"access_token"`
        ExpiresIn   int    `json:"expires_in"`
        Scope       string `json:"scope"`
    }
    if err := json.Unmarshal(body, &tokenResp); err != nil {
        return nil, fmt.Errorf("decode token response: %w", err)
    }

    ttl := time.Duration(tokenResp.ExpiresIn) * time.Second
    if tokenResp.ExpiresIn == 0 {
        ttl = time.Hour
    }
    keychainAccount := "server-token:" + serverName
    if err := keyring.Set(KeychainService, keychainAccount, tokenResp.AccessToken); err != nil {
        return nil, fmt.Errorf("store exchanged token in keychain: %w", err)
    }

    return &TokenExchangeResult{
        ServerName:      serverName,
        ServerURI:       serverURI,
        ScopesGranted:   strings.Fields(tokenResp.Scope),
        TokenExp:        time.Now().Add(ttl),
        KeychainAccount: keychainAccount,
    }, nil
}
```

### 9.7 Scope Resolution Algorithm

```go
// internal/store/sso.go

package store

import (
    "context"
    "database/sql"
    "fmt"
    "sort"
    "strings"

    "tag/internal/mcp"
)

// ResolveScopePolicy computes the effective scope set for a token exchange as the
// three-way intersection:
//
//   effective = mappedScopes ∩ serverScopes ∩ idpTokenScopes
//
// mappedScopes = UNION of sso_scope_maps rows where
//                 server_name = serverName AND idp_group IN userGroups.
//
// Called from internal/mcp before every RFC 8693 exchange. DB access is via
// modernc.org/sqlite's database/sql driver; the context carries a deadline so
// a slow DB does not stall MCP session startup.
func ResolveScopePolicy(
    ctx context.Context,
    db *sql.DB,
    serverName string,
    userGroups, serverScopes, idpScopes []string,
) (*mcp.ScopeMappingPolicy, error) {
    if len(userGroups) == 0 {
        return &mcp.ScopeMappingPolicy{ServerName: serverName, UserGroups: userGroups}, nil
    }

    ph := strings.Repeat("?,", len(userGroups))
    ph = ph[:len(ph)-1]
    query := `SELECT scopes FROM sso_scope_maps WHERE server_name = ? AND idp_group IN (` + ph + `)`

    args := make([]any, 0, 1+len(userGroups))
    args = append(args, serverName)
    for _, g := range userGroups {
        args = append(args, g)
    }

    rows, err := db.QueryContext(ctx, query, args...)
    if err != nil {
        return nil, fmt.Errorf("query sso_scope_maps: %w", err)
    }
    defer rows.Close()

    mapped := make(map[string]struct{})
    for rows.Next() {
        var scopes string
        if err := rows.Scan(&scopes); err != nil {
            return nil, err
        }
        for _, s := range strings.Split(scopes, ",") {
            mapped[strings.TrimSpace(s)] = struct{}{}
        }
    }
    if err := rows.Err(); err != nil {
        return nil, err
    }

    serverSet := setOf(serverScopes)
    idpSet    := setOf(idpScopes)
    var effective []string
    for s := range mapped {
        if _, inServer := serverSet[s]; inServer {
            if _, inIdP := idpSet[s]; inIdP {
                effective = append(effective, s)
            }
        }
    }
    sort.Strings(effective)

    return &mcp.ScopeMappingPolicy{
        ServerName:      serverName,
        UserGroups:      userGroups,
        MappedScopes:    sortedKeys(mapped),
        ServerScopes:    serverScopes,
        IdPTokenScopes:  idpScopes,
        EffectiveScopes: effective,
    }, nil
}

func setOf(ss []string) map[string]struct{} {
    m := make(map[string]struct{}, len(ss))
    for _, s := range ss {
        m[s] = struct{}{}
    }
    return m
}
```

### 9.8 JWS Signature Verification

Rather than hand-rolling RSA key reconstruction and signature verification, the Go stack delegates entirely to `github.com/coreos/go-oidc/v3`. The library fetches and caches JWKS automatically from the IdP's discovery document, enforces algorithm restrictions (rejects `alg=none` unconditionally), validates `exp`/`iss`/`aud`, and performs `kid`-matched signature verification. Access token claims (for group extraction on IdPs that embed them) are parsed with `github.com/golang-jwt/jwt/v5` using the same go-oidc-managed key set.

```go
// internal/mcp/sso.go (continued)

import (
    "context"
    "fmt"
    "time"

    "github.com/coreos/go-oidc/v3/oidc"
)

// ssoVerifier wraps a go-oidc Provider + IDTokenVerifier per IdP issuer.
// One instance is created per SSOConfig and reused across all login/exchange
// operations — the library manages JWKS cache refresh internally (5-minute TTL).
type ssoVerifier struct {
    provider *oidc.Provider
    verifier *oidc.IDTokenVerifier
}

// newSSOVerifier initialises an oidc.Provider by fetching the discovery document
// from issuerURL. go-oidc then owns JWKS fetching, caching, and kid-matching.
func newSSOVerifier(ctx context.Context, issuerURL, clientID string) (*ssoVerifier, error) {
    provider, err := oidc.NewProvider(ctx, issuerURL)
    if err != nil {
        return nil, fmt.Errorf("init OIDC provider for %s: %w", issuerURL, err)
    }
    v := provider.Verifier(&oidc.Config{
        ClientID:                   clientID,
        Now:                        time.Now,
        InsecureSkipSignatureCheck: false, // always enforce
    })
    return &ssoVerifier{provider: provider, verifier: v}, nil
}

// VerifyIDToken verifies a raw ID token string and extracts its claims.
// Rejects: alg=none (go-oidc enforces this), expired exp, issuer mismatch,
// audience mismatch, kid not found in JWKS.
// Returns the decoded claims map for group extraction by the IdP adapter.
func (sv *ssoVerifier) VerifyIDToken(ctx context.Context, rawIDToken string) (map[string]any, error) {
    token, err := sv.verifier.Verify(ctx, rawIDToken)
    if err != nil {
        return nil, fmt.Errorf("ID token verification: %w", err)
    }
    var claims map[string]any
    if err := token.Claims(&claims); err != nil {
        return nil, fmt.Errorf("extract ID token claims: %w", err)
    }
    return claims, nil
}
```

### 9.9 Audit Log Writer

```go
// internal/store/sso.go (continued)

import (
    "context"
    "database/sql"
    "fmt"
)

// AuditSSOEvent holds the safe metadata for one SSO audit row.
// Token values are never included — callers must not pass them.
type AuditSSOEvent struct {
    EventType       string // login|logout|exchange|refresh|revocation|scope_escalation|exchange_failure
    Subject         string // IdP sub claim (empty before login completes)
    Email           string
    ServerName      string // empty for login/logout/refresh events
    ScopesRequested string // space-separated; populated for exchange events
    ScopesGranted   string // may differ from requested after intersection
    Outcome         string // success|failure|blocked
    ErrorCode       string // OAuth error code if Outcome=failure
    ErrorDetail     string // human-readable error detail
    IdPType         string
    Tenant          string
}

// AuditSSO writes one row to sso_audit_log. Uses the existing *sql.DB connection
// (modernc.org/sqlite WAL mode); each call issues a single INSERT and relies on
// SQLite's implicit per-statement commit in autocommit mode.
// This is called on every SSO event and must complete within 500 ms (FR-11).
func AuditSSO(ctx context.Context, db *sql.DB, e AuditSSOEvent) error {
    _, err := db.ExecContext(ctx, `
        INSERT INTO sso_audit_log
          (event_type, subject, email, server_name, scopes_requested,
           scopes_granted, outcome, error_code, error_detail, idp_type, tenant)
        VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
        e.EventType, e.Subject, e.Email, e.ServerName, e.ScopesRequested,
        e.ScopesGranted, e.Outcome, e.ErrorCode, e.ErrorDetail, e.IdPType, e.Tenant,
    )
    if err != nil {
        return fmt.Errorf("write sso_audit_log: %w", err)
    }
    return nil
}
```

### 9.10 Integration Into MCP Server Session Startup

The go-sdk client (`github.com/modelcontextprotocol/go-sdk v1.6.1`) accepts a custom `http.RoundTripper` that injects the `Authorization` header into every MCP request. The SSO layer provides this transport. `internal/mcp` exposes `GetMCPAuthToken` which is called once per session; the resulting token is wrapped in an `authRoundTripper` and passed to the go-sdk client constructor.

```go
// internal/mcp/client.go (modified section)

package mcp

import (
    "context"
    "errors"
    "fmt"
    "net/http"

    "tag/internal/config"
    "tag/internal/store"
)

// authRoundTripper injects a Bearer token into every outbound MCP HTTP request.
type authRoundTripper struct {
    token string
    base  http.RoundTripper
}

func (a *authRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
    r2 := r.Clone(r.Context())
    r2.Header.Set("Authorization", "Bearer "+a.token)
    return a.base.RoundTrip(r2)
}

// GetMCPAuthToken returns the Bearer token to use for an MCP server session.
// Prefers RFC 8693 SSO exchange when configured; falls back to per-server
// OAuth; returns empty string if neither is active.
// Zero SSO work runs when config.SSOConfigured() is false (FR-15, NFR-03).
func GetMCPAuthToken(ctx context.Context, serverName, serverURI string, db *store.DB) (string, error) {
    if !config.SSOConfigured() {
        return store.GetPerServerOAuthToken(ctx, db, serverName)
    }

    token, err := GetOrExchangeServerToken(ctx, serverName, serverURI, db)
    if err == nil {
        return token, nil
    }

    var notSupported *TokenExchangeNotSupportedError
    if errors.As(err, &notSupported) {
        // MCP server's AS does not advertise token-exchange; use per-server OAuth.
        return store.GetPerServerOAuthToken(ctx, db, serverName)
    }

    var expired *SSOSessionExpiredError
    if errors.As(err, &expired) {
        notifySSOExpired(ctx, serverName) // PRD-040 notification hook
        return "", fmt.Errorf("SSO session expired for %s — run `tag mcp sso login`: %w", serverName, err)
    }

    return "", err
}
```

The go-sdk's Enterprise Managed Auth path (`mcp.WithHTTPClient`) is the injection point: `mcp.NewClient(..., mcp.WithHTTPClient(&http.Client{Transport: &authRoundTripper{token: tok, base: http.DefaultTransport}}))`. For stdio-transport MCP servers the token is passed as an environment variable per the MCP auth spec.

### 9.11 SSO Config File Format (`~/.tag/sso_config.yaml`)

Loaded by `internal/config/sso.go` via `koanf/v2` + `gopkg.in/yaml.v3`; written atomically with `gofrs/flock` + `os.Rename`. Permissions enforced to `0600` on write. No secrets stored here — they are keychain-only.

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

1. **Zero plaintext token storage.** All token values (IdP access tokens, refresh tokens, exchanged server tokens) are stored exclusively in the OS keychain via `github.com/zalando/go-keyring`, which uses macOS Keychain Services, Windows Credential Manager, or libsecret on Linux. No token byte ever touches a file, the SQLite database, an environment variable, or a log line. `AuditSSO` in `internal/store` explicitly accepts only non-sensitive metadata, not token values.

2. **PKCE S256 mandatory for all flows.** The authorization code flow always uses PKCE S256 (RFC 7636 section 4.2). The `code_challenge_method=plain` variant is explicitly rejected. This prevents authorization code interception attacks even if the loopback redirect is race-conditioned by another local process.

3. **State parameter CSRF protection.** A cryptographically random 32-byte state parameter is generated per login attempt. The callback handler verifies state before accepting any authorization code. Mismatched state causes the login to abort with an error, not a silent failure.

4. **Audience-bound tokens prevent cross-server token reuse.** Each token exchange result includes `resource=<server_uri>` (RFC 8707), which causes the MCP server's authorization server to issue a token with `aud` set to that specific server URI. A token for `io.github.acme/db-mcp` cannot be used against `io.github.acme/github-mcp` even if intercepted.

5. **Scope intersection enforces least privilege.** The `resolve_scope_policy` function computes a three-way intersection. No code path allows scopes to be granted that are not simultaneously present in the user's IdP group mappings, the MCP server's supported scope list, and the IdP access token's own scope claim. A scope escalation attempt is blocked, audited, and reported.

6. **JWS verification with JWKS caching.** Tokens are verified via `github.com/coreos/go-oidc/v3`'s `IDTokenVerifier` against the IdP's JWKS endpoint before any claims are trusted. The library unconditionally rejects `alg=none`. JWKS are cached internally for ~5 minutes to limit network calls; `kid` mismatch triggers an immediate re-fetch before rejection.

7. **Audit log integrity.** The `sso_audit_log` table uses `STRICT` mode (SQLite 3.37+), enforcing declared column types. Each row is committed immediately. The table has no `DELETE` or `UPDATE` permission granted in application code — `audit()` only calls `INSERT`. Deletion requires direct SQLite access.

8. **Secret scanning integration (PRD-034).** The secret scanner's regex patterns are extended to detect Okta API tokens (`SSWS [a-zA-Z0-9_-]{42}`), Azure AD bearer tokens (`Bearer eyJ...`), and Google OAuth tokens (`ya29.[a-zA-Z0-9_-]{...}`) in any file or clipboard content that passes through TAG.

9. **Loopback redirect only.** The redirect URI is restricted to `http://localhost:<port>/callback`. Non-loopback redirect URIs are rejected at `configure` time. This prevents open-redirect phishing attacks where a malicious application captures the authorization code.

10. **Token revocation on logout.** `tag mcp sso logout` sends RFC 7009 revocation requests to the IdP for both access and refresh tokens. Revocation failure is logged but does not block local cleanup. This ensures that even if keychain entries are copied, the tokens are invalid at the IdP level as soon as logout is called.

11. **Headless environment token isolation.** In device code flow (headless), the `user_code` and verification URI are printed to stdout only. They are never logged to the audit log or written to any file. The `device_code` value (which carries higher trust than `user_code`) is stored only in process memory until the exchange completes.

12. **Tracing redaction.** The OTel span emitted by `exchangeTokenForServer` (`go.opentelemetry.io/otel`) sets attributes `sso.server_name`, `sso.scopes_granted`, `sso.subject`, and `sso.outcome`. It explicitly does NOT set `sso.token` or any raw token attribute. The OTel span processor in `internal/netguard` (PRD-034) redacts these spans before OTLP export.

---

## 11. Testing Strategy

All tests live in `internal/mcp/sso_test.go` and `internal/store/sso_test.go`. The test runner is `go test ./...` with `github.com/stretchr/testify/assert` and `github.com/stretchr/testify/require`. HTTP-level tests use `net/http/httptest` servers to mock the IdP token endpoint, JWKS endpoint, and MCP AS. SQLite tests use `modernc.org/sqlite` with an in-memory DSN (`file::memory:?cache=shared`).

### 11.1 Unit Tests (`internal/mcp/sso_test.go`)

| Test | Description |
|------|-------------|
| `TestPKCES256Pair` | Assert `pkceS256Pair()` produces `base64url(sha256(verifier)) == challenge` for a computed pair. |
| `TestPKCES256PairUniqueness` | Call `pkceS256Pair()` 100 times; assert all verifiers are distinct. |
| `TestScopeIntersectionBasic` | User in `data-engineers`; mapped scopes `[read:query, write:query]`; server supports `[read:query, admin:query]`; IdP token has `[openid, read:query]`; `EffectiveScopes` MUST be `[read:query]`. |
| `TestScopeIntersectionEmpty` | User has no groups; `EffectiveScopes` MUST be `[]`; `ResolveScopePolicy` MUST return nil error. |
| `TestScopeEscalationBlocked` | `exchangeTokenForServer` with empty `EffectiveScopes` MUST return `*ScopeEscalationError`. |
| `TestVerifyIDTokenNoneAlgRejected` | Serve a malformed token with `alg=none` from an `httptest.Server` JWKS stub; `VerifyIDToken` MUST return an error wrapping go-oidc's rejection. |
| `TestVerifyIDTokenExpired` | Serve a well-signed JWT with `exp` in the past; MUST return error. |
| `TestVerifyIDTokenIssuerMismatch` | Serve a well-signed JWT with wrong `iss`; MUST return error. |
| `TestAuditLogWrite` | Call `AuditSSO` against in-memory SQLite; assert exactly one row inserted with correct fields via `SELECT`. |
| `TestSSOConfigFilePermissions` | After `configureCmd` runs, assert `sso_config.yaml` has mode `0600` via `os.Stat`. |
| `TestSSOConfiguredGate` | Assert `config.SSOConfigured()` returns false when no `sso_config.yaml` exists; assert `GetMCPAuthToken` never calls SSO code (verify via `httptest` request count). |
| `TestOktaDiscoveryURL` | `OktaAdapter{cfg}.DiscoveryURL()` returns `https://<tenant>/.well-known/openid-configuration`. |
| `TestAzureADTenantNormalization` | `AzureADAdapter` normalizes `acme.onmicrosoft.com` → expected v2.0 discovery URL; raw GUID passes through unchanged. |
| `TestGoogleWorkspaceGroupsAPICall` | `httptest.Server` stubs the Directory API; assert call is made with `Authorization: Bearer <token>` when `directory_api_enabled=true`. |
| `TestTokenExchangeResourceParam` | `httptest.Server` captures the POST body; assert `resource=<server_uri>` is present (RFC 8707). |
| `TestTokenExchangeUnsupportedGrantType` | `httptest.Server` returns HTTP 400 `unsupported_grant_type`; assert error is `*TokenExchangeNotSupportedError`, not `*TokenExchangeError`. |
| `TestKeychainNoDiskWrites` | After `exchangeTokenForServer`, assert `~/.tag/` contains no new files and SQLite `sso_server_tokens` has no `token` column (metadata only). |
| `TestScopePolicyYAMLImport` | Parse `testdata/sso/scope_policy.yaml`; assert all rows written to `sso_scope_maps` in in-memory SQLite. |
| `TestJWKSCacheTTL` | Intercept `http.Client` transport; call `newSSOVerifier` + `VerifyIDToken` twice within 5 minutes; assert JWKS endpoint hit ≤ 2 times (go-oidc internal caching). |
| `TestStateParamCSRF` | Simulate loopback callback with mismatched `state`; assert `loginCmd` returns `*SSOStateError`. |

### 11.2 Integration Tests

Each integration test runs against a real sandbox IdP tenant. CI injects `TAG_TEST_OKTA_TENANT`, `TAG_TEST_AZURE_TENANT`, `TAG_TEST_GOOGLE_CLIENT_ID` etc. via environment variables. These tests are gated behind `//go:build integration` and run in a dedicated CI job.

| Test | Description |
|------|-------------|
| `TestOktaFullFlow` | `configure → login (device flow) → status → exchange → token show → logout` against Okta dev tenant. |
| `TestAzureADFullFlow` | Same flow against Azure AD v2.0 sandbox tenant. |
| `TestGoogleWorkspaceFullFlow` | Same flow against Google Workspace dev account with Directory API. |
| `TestTokenRefreshOnExpiry` | Inject a keychain access token with 1-minute TTL; wait for expiry; trigger `GetMCPAuthToken`; assert `sso_audit_log` has a `refresh/success` row. |
| `TestRevocationOnLogout` | After `logoutCmd`, POST the old refresh token to the IdP's `revocation_endpoint`; assert HTTP 400 `invalid_grant`. |
| `TestScopeEscalationInExchange` | Scope map contains `read:query` only; inject a request for `write:query`; assert blocked and `sso_audit_log` has `scope_escalation/blocked`. |
| `TestAuditLogCompleteness` | Full login→exchange→logout cycle; assert `sso_audit_log` contains exactly 1 `login`, N `exchange`, 1 `logout`. |
| `TestHeadlessDetection` | Set `DISPLAY=""`, `SSH_TTY=""`, `TAG_HEADLESS=1`; assert device code flow is selected automatically. |

### 11.3 Performance Tests

Use `go test -bench` + `testing.B` for all performance assertions. The P95 latency benchmark uses a histogram over `b.N` iterations.

| Benchmark | Target | Method |
|-----------|--------|--------|
| `BenchmarkTokenExchangeP95` | P95 < 500 ms | 50 exchanges against `httptest.Server` with 50 ms artificial `time.Sleep`; collect durations; assert P95. |
| `BenchmarkStartupOverheadNoSSO` | < 5 ms delta | Measure `GetMCPAuthToken` round-trip when `SSOConfigured()` returns false; assert < 5 ms. |
| `BenchmarkScopeResolution` | > 10,000 ops/sec | `b.RunParallel` calling `ResolveScopePolicy` on in-memory SQLite with 100 groups, 50 servers. |
| `BenchmarkJWKSCacheHitRate` | ≥ 95% cache hits | 100 `VerifyIDToken` calls in 4 minutes via `httptest.Server`; assert transport hit count ≤ ceiling. |

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
| AC-07 | `tag mcp sso logout` removes all keychain entries (access, refresh, all server tokens). `keyring.Get("tag-sso", ...)` returns `keyring.ErrNotFound` for all known keys after logout. | Yes — go-keyring assertion in test |
| AC-08 | When a user in group `data-engineers` connects to `io.github.acme/db-mcp` with scope map `read:query,write:query`, the exchanged token's scope claim contains exactly `read:query write:query` (no more, no less, per intersection). | Yes — token claims assertion |
| AC-09 | When a user is NOT in any mapped group for a server, token exchange for that server raises `ScopeEscalationError` and writes a `scope_escalation/blocked` row to `sso_audit_log`. | Yes — exception type + DB assertion |
| AC-10 | Token exchange request includes `resource=<server_uri>` (RFC 8707). Verified by intercepting the HTTP POST body in integration test. | Yes — HTTP request capture |
| AC-11 | JWTs with `alg=none`, expired `exp`, or mismatched `iss` are rejected by `ssoVerifier.VerifyIDToken` (go-oidc/v3) with a non-nil error. | Yes — unit tests with `httptest` JWKS server |
| AC-12 | `tag mcp sso token show --server <name>` prints decoded claims including `sub`, `email`, `scope`, `exp`, and does not print the raw token string. | Yes — stdout parse; assert raw token not present |
| AC-13 | `tag mcp sso scope map --server X --group G --scopes S` writes a row to `sso_scope_maps` and `tag mcp sso scope map list` displays it. | Yes — DB + stdout |
| AC-14 | `tag mcp sso scope map import --file policy.yaml` correctly inserts all rows from the YAML file and reports the count of mappings added/updated. | Yes — DB row count assertion |
| AC-15 | `tag run` with no `sso_config.yaml` executes zero SSO logic (`config.SSOConfigured()` short-circuits). Verified by `BenchmarkStartupOverheadNoSSO`: `GetMCPAuthToken` with no config completes in < 5 ms, with no outbound HTTP calls (assert via `httptest` transport counter). | Yes — benchmark + HTTP request count assertion |
| AC-16 | When the IdP access token expires, `internal/mcp` silently refreshes via `golang.org/x/oauth2.TokenSource` and retries token exchange without user intervention. The `sso_audit_log` records a `refresh/success` event. | Yes — `TestTokenRefreshOnExpiry` integration test |
| AC-17 | When the refresh token is also expired/revoked, `internal/mcp` returns `*SSOSessionExpiredError`, emits a notification via PRD-040, and does NOT fall back to unauthenticated access. | Yes — `httptest.Server` returning `invalid_grant` |
| AC-18 | The `sso_audit_log` table contains exactly one row per SSO event, with correct `event_type`, `subject`, `outcome`, and `server_name` for login, exchange, and logout in the full-cycle integration test. | Yes — DB row assertions |
| AC-19 | All three IdP providers (Okta, Azure AD, Google Workspace) pass the full 30-case integration test suite. | Yes — CI matrix |
| AC-20 | `tag mcp sso audit --sub <sub> --export jsonl` produces a valid JSONL file with one JSON object per row, all having the queried subject. | Yes — parse output |

---

## 13. Dependencies

| Dependency | Type | Notes |
|-----------|------|-------|
| `github.com/zalando/go-keyring` | Go module | OS keychain abstraction (macOS Keychain Services, Windows Credential Manager, libsecret on Linux). Pure-Go + OS API; no CGO. On headless CI, inject a mock `keyring.Keyring` via the library's test helper or use `keyrings.File` equivalent. |
| `github.com/coreos/go-oidc/v3` | Go module | OIDC provider, `IDTokenVerifier`, JWKS remote key set with automatic caching. Handles `alg=none` rejection, `exp`/`iss`/`aud` checks. Transitively pulls `golang.org/x/oauth2`. |
| `golang.org/x/oauth2` | Go module | Authorization code + PKCE flow, device code flow (RFC 8628), token refresh `TokenSource`, RFC 7009 revocation. Already a transitive dependency via go-sdk. |
| `github.com/modelcontextprotocol/go-sdk v1.6.1` | Go module | MCP client+server; Enterprise Managed Auth and client-credentials support (v1.6). The SSO token is injected via a custom `http.RoundTripper` passed to the SDK client. |
| `modernc.org/sqlite` | Go module | Pure-Go SQLite driver (`CGO_ENABLED=0`); FTS5 + STRICT table mode built in. Already the TAG state store (shared `tag.sqlite3`). |
| `github.com/knadh/koanf/v2` | Go module | Config load/merge for `sso_config.yaml`; already a TAG dependency. |
| `gopkg.in/yaml.v3` | Go module | YAML marshal for atomic config write-back; already a TAG dependency. |
| `github.com/gofrs/flock` | Go module | File locking for atomic config RMW (shared with other TAG config writers). |
| `go.opentelemetry.io/otel` | Go module | OTel tracing for SSO exchange spans; already a TAG dependency (PRD-013). |
| `github.com/stretchr/testify` | Test | `assert` + `require` for all unit and integration tests. |
| PRD-013 | Internal | OTel tracing; SSO spans emitted and redacted here. |
| PRD-034 | Internal | Secret scanner extended with Okta (`SSWS ...`), Azure AD (`Bearer eyJ...`), Google OAuth (`ya29....`) token regex patterns. |
| PRD-040 | Internal | Notification hooks for `*SSOSessionExpiredError` and scope escalation events. |
| PRD-014 | Internal | MCP server registry provides `server_uri` values for RFC 8707 `resource` parameter. |
| PRD-041 | Internal | Per-span cost attribution; SSO exchange spans need redaction before OTLP export. |
| Okta dev tenant | External | Sandbox Okta org for integration tests (free developer account). |
| Azure AD app registration | External | Azure AD v2.0 app with `oidc` and `groups` permissions for integration tests. |
| Google Cloud OAuth client | External | OAuth 2.0 client with `openid`, `email`, `profile`, Directory API read scope. |

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

**Days 1–3:** Scaffold `internal/mcp/sso.go` with Go structs (`SSOConfig`, `SSOSession`, `TokenExchangeResult`, `ScopeMappingPolicy`), the `IdPAdapter` interface, keychain key constants, `pkceS256Pair()`, `buildAuthURL()`, and the loopback redirect `net/http` handler. Wire `internal/config/sso.go` (`koanf/v2` load + `yaml.v3` + `gofrs/flock` atomic write). Write `TestPKCES256Pair`, `TestSSOConfiguredGate`, and `TestSSOConfigFilePermissions` (FR-01 through FR-06).

**Days 4–6:** Implement `OktaAdapter` in `internal/mcp/sso_adapters_okta.go`: `DiscoveryURL()`, `ExtractGroups()` from `groups` claim, `newSSOVerifier` wrapping `go-oidc/v3`. Write `TestOktaDiscoveryURL` and `TestVerifyIDTokenNoneAlgRejected`. Implement `configureCmd` in `internal/cli/mcp_sso.go` (OIDC discovery validation + `0600` file write). Implement `loginCmd` browser flow with state + PKCE. Verify AC-01, AC-02, AC-03.

**Days 7–8:** Implement `statusCmd` (keychain reads + per-server exchange display) and `logoutCmd` (RFC 7009 revocation via `golang.org/x/oauth2`). Add all four SQLite DDL migrations to `internal/store`'s migration runner. Implement `AuditSSO` in `internal/store/sso.go`. Verify AC-01 through AC-07.

**Days 9–10:** Implement device code flow (RFC 8628) via `golang.org/x/oauth2/deviceauth` with poll loop + `cenkalti/backoff/v4` exponential backoff. Implement `isHeadless()` env-var detection. Write `TestHeadlessDetection`. Implement `tokenShowCmd` with claim decoding via `jwt/v5` and raw-token redaction. Verify AC-03, AC-04, AC-12.

### Phase 2 — Azure AD and Google Workspace Adapters (1.5 weeks)

**Days 11–12:** Implement `AzureADAdapter` in `internal/mcp/sso_adapters_azure.go`: tenant GUID normalization (accept `<domain>.onmicrosoft.com` or raw GUID), v2.0 discovery URL construction. Document Azure AD `groups` claim app manifest prerequisite. Write `TestAzureADTenantNormalization` and `TestAzureADFullFlow` integration test.

**Days 13–14:** Implement `GoogleWorkspaceAdapter` in `internal/mcp/sso_adapters_google.go`: OIDC flow + `ExtractGroups()` via Google Directory API HTTP call. Handle `directory_api_enabled=false` gracefully (log warning; treat groups as empty). Write `TestGoogleWorkspaceGroupsAPICall` with `httptest.Server` stub.

**Day 15:** Implement `WorkOSAdapter` as a thin `IdPAdapter` wrapper (any OIDC endpoint WorkOS exposes). Resolve OQ-04. Write smoke test. Run full three-IdP CI matrix job.

### Phase 3 — Scope Mapping and Token Exchange (1 week)

**Days 16–17:** Implement `ResolveScopePolicy` in `internal/store/sso.go` with three-way intersection and `sso_scope_maps` DB queries. Write 30+ test vectors for `TestScopeIntersectionBasic` variants. Implement `exchangeTokenForServer` in `internal/mcp/sso.go` with RFC 8693 POST + `resource` parameter. Write `TestTokenExchangeResourceParam` via `httptest.Server` body capture (AC-10).

**Days 18–19:** Implement `scopeMapCmd` (add/remove/list/import) in `internal/cli/mcp_sso.go`. Write scope policy YAML import via `yaml.v3` + `internal/store`. Implement `GetMCPAuthToken` in `internal/mcp/client.go` with `config.SSOConfigured()` gate and `*TokenExchangeNotSupportedError` fallback. Verify AC-08, AC-09, AC-10, AC-13, AC-14, AC-15.

**Day 20:** Implement automatic token refresh via `golang.org/x/oauth2.TokenSource`, `*SSOSessionExpiredError` → PRD-040 notification, and `*SSOIdPUnreachableError` for network partition. Write `TestTokenRefreshOnExpiry` integration test. Verify AC-16, AC-17.

### Phase 4 — Audit, Observability, Security Hardening (0.5 weeks)

**Days 21–22:** Wire `AuditSSO` calls into all event paths. Implement `auditCmd` (`internal/cli/mcp_sso.go`) with `--since`/`--until`/`--sub`/`--server`/`--event` filters and CSV/JSONL export. Add OTel spans via `go.opentelemetry.io/otel` with PRD-034 redaction in the span processor. Extend PRD-034 secret scanner with Okta/Azure AD/Google token regex patterns. Run `go test -bench ./...` for all performance benchmarks. Run full AC-01 through AC-20 acceptance matrix.

**Total: ~4.5 weeks core implementation + 0.5 weeks buffer = 5 weeks**

The XL estimate (4-8 weeks) accounts for: IdP sandbox provisioning delays (typically 1-2 days per IdP for approval), potential firewall/keychain issues in CI environments (OQ-06, OQ-07), and the possibility that Azure AD Conditional Access (OQ-03) requires a Phase 2 step-up authentication implementation.

