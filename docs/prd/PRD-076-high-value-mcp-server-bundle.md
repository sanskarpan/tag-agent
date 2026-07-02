# PRD-076: High-Value MCP Server Bundle (`tag mcp registry add-curated`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** XS (1-2 days)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `mcp-registry.yaml`
**Depends on:** PRD-014 (MCP Server Registry & Discovery), PRD-028 (Sandbox Code Execution), PRD-013 (Tracing), PRD-034 (Secret Scanning), PRD-039 (Token Budget Enforcement)
**Inspired by:** Composio top integrations, MCP community top servers, Smithery rankings
**GitHub Issue:** #346

---

## 1. Overview

MCP (Model Context Protocol) has become the de facto integration standard for agentic AI tooling. As of mid-2026, the official MCP registry at `registry.modelcontextprotocol.io` indexes hundreds of servers spanning productivity suites, developer infrastructure, data stores, communications platforms, and marketing analytics. However, the breadth of the registry is also its weakness: a developer standing up a new TAG agent profile faces a discovery problem. Which 20 servers are actually worth installing? Which have reliable maintainers, stable schemas, and broad coverage of real-world workflows?

This PRD introduces `tag mcp registry add-curated`: a single command that installs a carefully selected bundle of the 20 highest-value MCP servers, sourced by cross-referencing Composio's top-integration metrics, Smithery's community download rankings, and the MCP community's most-starred GitHub repositories. The bundle covers every major workflow category a professional engineering team or knowledge worker is likely to need: productivity (Notion, Google Workspace), developer infrastructure (GitHub, Docker, Vercel, Cloudflare, AWS), databases (PostgreSQL, MongoDB, Redis), CRM and payments (HubSpot, Stripe), communication (Slack, Twilio), project management (Jira, Linear), design (Figma), browser automation (Playwright), observability (Sentry), SEO analytics (Ahrefs), and design collaboration.

The feature extends the existing `mcp-registry.yaml` bundle format defined in PRD-014, adds four category groupings to enable selective installs (`--category productivity | devops | database | comms`), and enriches the registry YAML schema with a `curated_bundle` top-level key that carries ranking metadata, install-method type, tool count estimates, and required environment variables for pre-flight credential checks. No Python source files are involved. The primary deliverables are the enriched `internal/mcp/registry/registry.yaml` (compiled into the binary via `//go:embed`) and the handlers in `internal/mcp/curated/curated.go` and `internal/cli/mcp_registry.go`. A thin SQLite table (`mcp_curated_installs`) records which servers were installed and when, enabling `tag mcp list --json` to surface curated-install provenance.

The feature also addresses a real constraint in the Cursor/Claude agent tool budget: a single context window supports at most 40 simultaneous MCP tools before the model begins to drop or misroute calls. Playwright alone exposes 25 tools. The curated installer therefore enforces a pre-flight tool-budget check, warns when the selection would exceed the 40-tool soft ceiling, and recommends disabling Playwright from the active context by default (it remains installed and available, but not auto-enabled). This tool-budget awareness makes `add-curated` safe to run in a single command without blowing up an existing agent configuration.

---

## 2. Problem Statement

### 2.1 Discovery Friction Prevents MCP Adoption

The MCP ecosystem has reached critical mass: Smithery lists 3,000+ servers, Composio integrates 250+ tools, and the official registry grows weekly. Yet TAG users still configure MCP servers manually by editing `~/.tag/profiles/<profile>/config.yaml`, looking up npm package names, and guessing at required environment variables. A developer who wants Notion + GitHub + Slack for their `researcher` profile has to perform three separate `tag mcp registry install` invocations, each requiring prior knowledge of the correct server identifier and env-var names. The cognitive overhead of this process means most teams install only the servers they already know about, leaving high-value integrations (Stripe, Sentry, Linear, Ahrefs) permanently undiscovered.

### 2.2 No Opinionated Starting Point for New Profiles

When `tag profile create` scaffolds a new agent profile, it adds zero MCP servers by default. The user must decide from scratch which tools the agent needs. This is the wrong default for a platform positioning itself as the easiest way to build production-grade agents. Heroku had a curated set of add-ons; Homebrew has its own "formulae" quality bar; npm has curated "awesome" lists. TAG needs its own opinionated, maintained list of "these are the 20 servers every serious agent deployment should consider," surfaced as a first-class CLI command. Without this, new users churn during onboarding because their agents have no tools, and experienced users waste time re-curating the same set of servers for every new project.

### 2.3 Tool-Budget Blindness Causes Silent Agent Degradation

Installing too many MCP servers silently degrades agent performance. When the active tool count exceeds the model's context budget (40 for Cursor, ~128 for direct API but varies by model), tools are either truncated from the context or cause the model to misroute calls. TAG has no mechanism today to warn users that their combined MCP tool count is approaching this limit. A developer who installs Playwright (25 tools), GitHub (15 tools), and Notion (10 tools) has already hit 50 tools — and their agent will behave erratically without any error message. The curated installer is the right place to enforce this check because it is the moment of intentional mass-installation; enforcing it here prevents the worst configurations before they take hold.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag mcp registry add-curated` installs all 20 curated servers for the specified profile (or `default`) with a single command, respecting existing installs (idempotent). |
| G2 | `--category <name>` filters the bundle to one of four named groups: `productivity`, `devops`, `database`, `comms`, each with 4-6 servers. |
| G3 | Pre-flight env-var check prints which required secrets are missing with a named keychain reference, without blocking the install (installs proceed; missing-credential servers are marked `pending_credentials`). |
| G4 | Pre-flight tool-budget check computes the cumulative tool count of selected servers plus already-enabled servers; warns (does not block) if the total exceeds 40; lists which servers to disable to stay under budget. |
| G5 | `tag mcp list --json` output includes a `curated_bundle` field per server showing ranking source, category, install date, and status (`active`, `pending_credentials`, `disabled`). |
| G6 | All 20 curated servers are represented in `mcp-registry.yaml` with complete metadata: description, category, install method, command/args, requires_env, tool_count_estimate, curated_rank, and ranking_sources. |
| G7 | Install method is selected per server following the pattern hierarchy: remote URL for vendor-hosted cloud servers, npx for npm packages, uvx for Python packages, Docker for system-dependency-heavy servers. SSE transport is never used. |
| G8 | The feature is fully additive: existing servers in `mcp-registry.yaml` are not modified; the new `curated_bundle` key is appended; no existing commands change behavior. |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Building, hosting, or maintaining any of the 20 MCP server implementations. TAG is a consumer of these servers, not a producer. |
| NG2 | OAuth credential acquisition flows. This PRD records which env-vars are missing and points to docs; OAuth 2.1 / PKCE flows are a separate PRD (MCP OAuth Integration, unscheduled). |
| NG3 | Automatic updates of curated servers to new versions. Version pinning, hash checking, and `.mcpc.json` contract snapshots are addressed in the MCP Version Contract PRD (unscheduled). |
| NG4 | A UI or TUI for browsing the curated list. The CLI table output of `tag mcp registry list-curated` is the browsing interface; a TUI is out of scope. |
| NG5 | Validating that the installed MCP servers actually respond (health check). That is handled by `tag mcp check`, which exists in PRD-014. |
| NG6 | Installing servers outside the curated 20 via the `add-curated` path. For non-curated installs, users continue to use `tag mcp registry install <server>`. |
| NG7 | Windows support for stdio transport. TAG targets macOS and Linux; Windows npx/uvx behavior is not tested in this PRD's scope. |

---

## 5. Success Metrics

| Metric | Baseline | Target (90 days post-ship) | Measurement |
|--------|----------|---------------------------|-------------|
| MCP servers per new profile (P50) | 1.2 | 5.0 | `mcp_curated_installs` table row count per profile, sampled at profile creation +7 days |
| `add-curated` command adoption | 0% | 30% of new profiles | Fraction of profiles with `curated_install = true` in `mcp_curated_installs` |
| Tool-budget warning actionability | N/A | 80% of warned users disable at least one server within 1 session | `mcp_curated_installs` status transitions from `active` to `disabled` for warned profiles |
| Env-var completion rate | N/A | 60% of `pending_credentials` servers become `active` within 7 days | Status transitions in `mcp_curated_installs` |
| `tag mcp list --json` machine-readability adoption | 0 external consumers | Used in 2+ CI pipeline templates shipped with TAG | CI template files referencing `tag mcp list --json` |
| Time-to-first-MCP-tool for new users | ~15 min (manual) | < 2 min (`add-curated` + credential set) | Measured via onboarding telemetry span from profile create to first MCP tool call |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | New TAG user | run `tag mcp registry add-curated --profile researcher` immediately after creating a profile | my researcher agent has Notion, Google Drive, Slack, and GitHub without reading any docs |
| U2 | DevOps engineer | run `tag mcp registry add-curated --category devops --profile coder` | my coder profile gets GitHub, Docker, AWS, Vercel, Cloudflare, and Sentry in one command |
| U3 | Full-stack developer | run `tag mcp registry add-curated --category database --profile coder` | my coder profile gets PostgreSQL, MongoDB, Redis, and GitHub database-adjacent tools without guessing package names |
| U4 | Platform engineer | run `tag mcp list --json | jq '.servers[] | select(.curated_bundle != null)'` | I can audit which curated servers are active, pending credentials, or disabled in a CI pre-flight check |
| U5 | Team lead | run `tag mcp registry add-curated --dry-run` | I see exactly which servers would be installed, which env-vars are needed, and what the tool-count impact is before touching a production profile |
| U6 | Developer | see a warning when `add-curated` would bring total active tools to 52/40 | I know to disable Playwright before running an agent session instead of discovering erratic tool routing at runtime |
| U7 | Product manager | run `tag mcp registry list-curated` | I can see the full curated bundle, each server's category, ranking, and which ones are already installed, without running a full install |
| U8 | Developer | re-run `tag mcp registry add-curated` on a profile that already has some curated servers | only missing servers are added; existing ones are left untouched with no error |
| U9 | Security-conscious team | run `tag mcp registry add-curated --profile prod` on a production profile | servers that require missing env-vars are installed in `pending_credentials` state and never activated until secrets are explicitly set |

---

## 7. Proposed CLI Surface

### 7.1 `tag mcp registry add-curated`

Install the full curated bundle or a category subset.

```
tag mcp registry add-curated \
  [--profile <profile-name>]        # default: "default"
  [--category productivity|devops|database|comms]
  [--dry-run]                        # print plan, no writes
  [--yes]                            # skip confirmation prompt
  [--json]                           # machine-readable output
  [--no-budget-check]                # skip tool-count pre-flight
  [--disable-playwright]             # mark Playwright as installed-but-disabled (recommended)
```

**Flags:**

- `--profile`: Target profile name. Must exist in `~/.tag/profiles/` or be `"default"`. Error if profile not found.
- `--category`: Install only servers in the named category. Valid values: `productivity`, `devops`, `database`, `comms`. When omitted, all 20 servers are installed.
- `--dry-run`: Validate that all server definitions exist in `mcp-registry.yaml`, print the install plan table, print the tool-budget impact, print missing env-vars. Exit 0. No SQLite writes, no config modifications.
- `--yes`: Skip the "Install N servers? [y/N]" confirmation prompt. Auto-set when `CI=true`.
- `--json`: Output machine-readable JSON to stdout. Human-readable progress is suppressed. Errors go to stderr.
- `--no-budget-check`: Suppress the tool-budget pre-flight. Useful for profiles with custom model configurations that support more than 40 tools.
- `--disable-playwright`: Register Playwright as installed in `mcp_curated_installs` with `status = 'disabled'`. Recommended because Playwright alone uses 25 tool slots. Can be enabled later with `tag mcp enable mcp-playwright --profile <name>`.

**Exit codes:**

- `0` — all selected servers installed (or already installed). Some may be `pending_credentials`.
- `1` — internal error (YAML missing, SQLite error, profile not found).
- `2` — tool-budget check would be violated and `--no-budget-check` was not set. (Non-blocking warning by default; only becomes exit 2 in `--strict` mode.)

**Example output (TTY, no flags):**

```
$ tag mcp registry add-curated --profile researcher --disable-playwright

Curated MCP Bundle — 20 servers (4 categories)
Checking profile: researcher  [OK]
Checking existing MCP servers: 2 already installed (mcp-github, mcp-filesystem)

Pre-flight: Environment Variables
  NOTION_API_KEY          missing  → set with: export NOTION_API_KEY=...
  GOOGLE_CLIENT_ID        missing  → see: https://console.cloud.google.com/
  GOOGLE_CLIENT_SECRET    missing  → see: https://console.cloud.google.com/
  STRIPE_SECRET_KEY       missing  → set with: export STRIPE_SECRET_KEY=sk_...
  GITHUB_TOKEN            present  [OK]
  SLACK_BOT_TOKEN         missing  → set with: export SLACK_BOT_TOKEN=xoxb-...
  JIRA_API_TOKEN          missing
  HUBSPOT_API_KEY         missing
  AHREFS_API_KEY          missing
  LINEAR_API_KEY          missing
  FIGMA_PERSONAL_TOKEN    missing
  AWS_ACCESS_KEY_ID       missing
  VERCEL_TOKEN            missing
  CLOUDFLARE_API_TOKEN    missing
  MONGODB_URI             missing
  REDIS_URL               missing
  TWILIO_ACCOUNT_SID      missing
  TWILIO_AUTH_TOKEN       missing
  SENTRY_AUTH_TOKEN       missing
  DATABASE_URL            present  [OK]

Pre-flight: Tool Budget
  Currently active tools:  12
  New tools (18 servers):  +143
  Playwright (disabled):   0  (would be +25 if enabled)
  Projected total:         155  ← WARNING: exceeds 40-tool soft limit for Cursor
  Recommended: use --category flag to install a focused subset, or use
               tag mcp disable <server> --profile researcher after install.

Install plan (20 servers, Playwright disabled):
  Server                  Category      Method   Env Vars  Tools  Status
  ─────────────────────── ───────────── ──────── ───────── ─────  ──────────────────
  mcp-notion              productivity  npx      1 missing  8     pending_credentials
  mcp-google-drive        productivity  remote   2 missing  6     pending_credentials
  mcp-google-calendar     productivity  remote   2 missing  5     pending_credentials
  mcp-google-gmail        productivity  remote   2 missing  7     pending_credentials
  mcp-stripe              productivity  npx      1 missing  12    pending_credentials
  mcp-playwright          productivity  npx      0          25    DISABLED (--disable-playwright)
  mcp-github              devops        npx      0          15    already installed
  mcp-docker              devops        npx      0          9     will install
  mcp-jira                devops        npx      1 missing  10    pending_credentials
  mcp-aws                 devops        npx      1 missing  18    pending_credentials
  mcp-vercel              devops        npx      1 missing  8     pending_credentials
  mcp-cloudflare          devops        npx      1 missing  11    pending_credentials
  mcp-sentry              devops        npx      1 missing  7     pending_credentials
  mcp-postgresql          database      npx      0          14    already installed
  mcp-mongodb             database      npx      1 missing  9     pending_credentials
  mcp-redis               database      npx      1 missing  6     pending_credentials
  mcp-slack               comms         npx      1 missing  8     pending_credentials
  mcp-hubspot             comms         npx      1 missing  11    pending_credentials
  mcp-linear              comms         npx      1 missing  7     pending_credentials
  mcp-twilio              comms         npx      2 missing  6     pending_credentials
  mcp-figma               comms         npx      1 missing  9     pending_credentials
  mcp-ahrefs              comms         npx      1 missing  5     pending_credentials

Install 22 servers for profile 'researcher'? (2 already installed, 1 disabled) [y/N]: y

Installing...
  [OK] mcp-github          (already installed, skipped)
  [OK] mcp-postgresql      (already installed, skipped)
  [OK] mcp-notion          → registered (pending_credentials: NOTION_API_KEY)
  [OK] mcp-google-drive    → registered (pending_credentials: GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET)
  ...
  [OK] mcp-playwright      → registered (disabled, enable with: tag mcp enable mcp-playwright --profile researcher)

Done. 18 servers registered, 2 skipped (already installed), 1 disabled.
17 servers are pending credentials — run `tag mcp creds --profile researcher` to see setup guide.
Active tool count: 29/40 (with pending-credentials servers excluded from budget).
```

### 7.2 `tag mcp registry list-curated`

Show the full curated bundle without installing anything.

```
tag mcp registry list-curated \
  [--category productivity|devops|database|comms]
  [--profile <profile-name>]         # annotate with install status for this profile
  [--json]
```

**Example output (TTY):**

```
$ tag mcp registry list-curated --category devops

Curated Bundle — devops (7 servers)

  Rank  Server           Description                             Tools  Method  Installed
  ───── ──────────────── ─────────────────────────────────────── ─────  ─────── ─────────
  #2    mcp-github       GitHub repos, PRs, issues, commits       15    npx     YES (researcher)
  #7    mcp-docker       Docker container lifecycle management     9     npx     no
  #9    mcp-jira         Jira issue tracking and sprint mgmt      10    npx     no
  #12   mcp-aws          AWS multi-service operations             18    npx     no
  #14   mcp-vercel       Deploy and manage Vercel projects         8     npx     no
  #15   mcp-cloudflare   Cloudflare DNS, Workers, R2, KV          11    npx     no
  #17   mcp-sentry       Sentry error monitoring and alerts        7     npx     no
```

### 7.3 `tag mcp list --json` (extended output)

Existing command; extended to include `curated_bundle` field per server.

```bash
tag mcp list --json
```

**Example JSON output (partial):**

```json
{
  "profile": "researcher",
  "servers": [
    {
      "name": "mcp-notion",
      "description": "Notion workspace: pages, databases, search",
      "category": "productivity",
      "status": "pending_credentials",
      "enabled": false,
      "install_method": "npx",
      "package": "@notionhq/notion-mcp-server",
      "tool_count_estimate": 8,
      "requires_env": ["NOTION_API_KEY"],
      "missing_env": ["NOTION_API_KEY"],
      "curated_bundle": {
        "rank": 1,
        "category": "productivity",
        "ranking_sources": ["composio_top_integrations", "smithery_downloads"],
        "installed_at": "2026-06-17T10:23:45Z",
        "installed_by": "add-curated"
      }
    }
  ],
  "tool_budget": {
    "active_tool_count": 29,
    "soft_limit": 40,
    "pending_credential_tools": 143,
    "disabled_tools": 25
  }
}
```

### 7.4 `tag mcp creds` (new convenience command)

Show credential setup guide for all `pending_credentials` servers on a profile.

```
tag mcp creds [--profile <name>] [--server <server-name>] [--json]
```

```
$ tag mcp creds --profile researcher

Pending Credentials — researcher profile (17 servers)

  mcp-notion
    NOTION_API_KEY   https://www.notion.so/profile/integrations → Create integration → copy token
    Set:  export NOTION_API_KEY=secret_...
          tag mcp activate mcp-notion --profile researcher   (activates after setting)

  mcp-google-drive
    GOOGLE_CLIENT_ID      https://console.cloud.google.com/ → APIs & Services → Credentials
    GOOGLE_CLIENT_SECRET  (same OAuth 2.0 client)
    Note: Google Workspace uses OAuth 2.1 PKCE. Run: tag mcp auth mcp-google-drive --profile researcher
    ...
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | **`mcp-registry.yaml` curated_bundle section:** The registry YAML must include a top-level `curated_bundle` key containing a list of 20 server references, each with: `server_key` (matching a key in `servers:`), `rank` (int 1–20), `category` (one of `productivity`, `devops`, `database`, `comms`), `ranking_sources` (list of strings), `tool_count_estimate` (int), and `default_disabled` (bool, true for Playwright). |
| FR-02 | **Idempotent install:** Re-running `tag mcp registry add-curated` on a profile that already has some curated servers must skip those servers silently and install only missing ones. The exit code must be 0; the output must clearly indicate which were skipped. |
| FR-03 | **Category filter:** `--category` must restrict the install to servers in that category group only. Running `add-curated --category devops` must not modify, remove, or re-register servers in other categories already installed on the profile. |
| FR-04 | **Pre-flight env-var check:** Before any writes, the command must iterate over each selected server's `requires_env` list and call `os.Getenv` for each variable. Missing variables are collected and displayed in the pre-flight summary. Missing variables do not block the install. |
| FR-05 | **`pending_credentials` status:** Servers installed with one or more missing env-vars must be written to `mcp_curated_installs` with `status = 'pending_credentials'` and must not be written to the profile's active MCP server config until all required env-vars are present. |
| FR-06 | **`mcp_curated_installs` SQLite table:** The installation record table must be created via `internal/store.Migrate(db)` (WAL mode already set by `internal/store.OpenDB()`). Schema is defined in Section 10.2. Every `add-curated` invocation writes one row per server to this table (`INSERT OR REPLACE`), wrapped in a single transaction. |
| FR-07 | **Tool-budget pre-flight:** Before displaying the confirmation prompt, the command must sum `tool_count_estimate` for all selected servers with `status != 'disabled'` and add the count of already-active tools for the profile. If the sum exceeds 40, a WARNING block is printed listing which servers to disable to reach a safe count. This check is skipped when `--no-budget-check` is set. |
| FR-08 | **`--disable-playwright` flag:** When set, Playwright is registered in `mcp_curated_installs` with `status = 'disabled'` and its tool count is excluded from the budget calculation. The user must explicitly run `tag mcp enable mcp-playwright --profile <name>` to activate it. |
| FR-09 | **Dry-run mode:** `--dry-run` must not write any rows to `mcp_curated_installs`, must not modify any profile config, and must print the full install plan table, tool-budget impact, and missing env-vars. Exit code is 0 if `mcp-registry.yaml` parses cleanly, 1 otherwise. |
| FR-10 | **`tag mcp list --json` extension:** The JSON output of `tag mcp list --json` must include a `curated_bundle` object for every server that has a row in `mcp_curated_installs`, populated from that row. Non-curated servers must omit the `curated_bundle` key. |
| FR-11 | **`tag mcp registry list-curated`:** Must read the `curated_bundle` section of `mcp-registry.yaml` and render a table of all 20 servers. When `--profile` is passed, each row must be annotated with the install status from `mcp_curated_installs` if a row exists. |
| FR-12 | **Install method fidelity:** Each server in the curated bundle must use exactly one of: `npx` (Node.js stdio), `uvx` (Python stdio), `docker` (container stdio), or `remote` (streamable-HTTP or remote URL). SSE transport must never be used. The `config` block in `mcp-registry.yaml` must include a transport-appropriate `command` and `args` for each server. |
| FR-13 | **Google Workspace as five distinct servers:** Gmail, Drive, Calendar, Sheets, and Chat must be registered as five separate server keys in `mcp-registry.yaml` (e.g., `mcp-google-drive`, `mcp-google-calendar`, `mcp-google-gmail`). Each has its own `requires_env` entry for the shared OAuth client credentials. The curated bundle counts them as separate entries toward the 20 total but notes they share one OAuth client. |
| FR-14 | **`tag mcp creds` output:** The `tag mcp creds` command must read `mcp_curated_installs` for the given profile, collect all servers with `status = 'pending_credentials'`, and for each server print the required env-var name, a one-sentence setup URL, and the `export VAR=...` command. When `--server` is provided, output only that server's credential guide. |
| FR-15 | **`tag mcp activate`:** When a user sets a missing env-var and runs `tag mcp activate <server> --profile <name>`, the command must re-check all `requires_env` for that server, confirm all are present via `os.Getenv`, and update the `mcp_curated_installs` row to `status = 'active'` and write the server to the profile's MCP config via `knadh/koanf/v2`. |
| FR-16 | **Confirmation prompt:** Unless `--yes` or `CI=true`, the command must display a summary line ("Install N servers for profile X? [y/N]") and await user input before making any writes. |
| FR-17 | **`--json` output:** In `--json` mode, all human-readable output is suppressed. The final JSON object (schema in Section 9.3) is written to stdout. Progress and errors go to stderr. |
| FR-18 | **Ranking sources:** Each server's `curated_bundle` entry in `mcp-registry.yaml` must list at least one of: `composio_top_integrations`, `smithery_downloads`, `github_stars`, `mcp_community_vote`. This is editorial metadata; it is included in `--json` output for transparency but not validated against live APIs. |
| FR-19 | **No mutation of existing `servers:` entries:** The PR adding `mcp-registry.yaml` changes must only add new server keys and the `curated_bundle` top-level key. Existing server definitions must not be modified. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Command latency:** `tag mcp registry add-curated --dry-run` must complete in under 200 ms on a warm machine (YAML already parsed). No network calls are made during dry-run. Actual install (YAML writes + SQLite) must complete in under 500 ms for all 20 servers. |
| NFR-02 | **Idempotency:** Running `add-curated` N times on the same profile must produce the same final state as running it once. SQLite writes use `INSERT OR REPLACE` keyed on `(profile, server_key)`. Profile config writes check for existing server entry before appending. |
| NFR-03 | **Zero network requirement:** The curated bundle is defined entirely in the bundled `mcp-registry.yaml`. No network calls to `registry.modelcontextprotocol.io` or any other endpoint are made by `add-curated`. Network calls happen only at agent runtime when the MCP servers themselves are invoked. |
| NFR-04 | **YAML parse safety:** `internal/mcp/registry/registry.yaml` is embedded at compile time via `//go:embed` and parsed once at startup with `gopkg.in/yaml.v3` (`yaml.Unmarshal`). The file is read-only at runtime; no round-trip writes occur. The `curated_bundle` section is appended as a top-level key after `servers:` in the source file; YAML comments are preserved in version control but are not required to survive parse/marshal cycles. |
| NFR-05 | **SQLite WAL mode:** All writes to `mcp_curated_installs` go through `internal/store.OpenDB()` (`modernc.org/sqlite`, WAL mode, `journal_mode=WAL`, `synchronous=NORMAL`). Single-writer isolation is enforced by `gofrs/flock`. Concurrent reads from `tag mcp list --json` are never blocked. |
| NFR-06 | **Secret hygiene:** No secret values are ever written to `mcp-registry.yaml`, `mcp_curated_installs`, or any log output. The `requires_env` list contains only variable names (strings). The pre-flight check uses `os.Getenv(name) != ""` — no value logging. |
| NFR-07 | **`--json` machine-readability:** The JSON schema for `tag mcp list --json` output must be stable across minor TAG releases (additive changes only). Breaking schema changes require a new `--json-version` flag. |
| NFR-08 | **Graceful missing YAML keys:** If a server referenced in `curated_bundle` does not have a corresponding entry in `servers:`, the command must emit a WARNING (not an ERROR) for that server, skip it, and continue. This handles the case where a server is removed from the registry. |
| NFR-09 | **TTY vs. pipe output:** Progress output (spinner, per-server status lines) is only emitted when stdout is a TTY. When piped, only the final summary line is written to stdout; `--json` overrides both. |

---

## 10. Technical Design

### 10.1 New and Modified Files

| File | Change Type | Description |
|------|-------------|-------------|
| `internal/mcp/registry/registry.yaml` | Modified | Add 11 new server entries + `curated_bundle` top-level key (go:embed'd into the binary at compile time) |
| `internal/mcp/registry/embed.go` | New | `//go:embed registry.yaml` directive; exports `LoadRegistry() (*Registry, error)` parsing the embedded YAML via `gopkg.in/yaml.v3` |
| `internal/mcp/curated/curated.go` | New | `ComputeInstallPlan`, `ExecuteInstallPlan`; Go structs `CuratedServerDef`, `MCPServerConfig`, `CuratedInstallPlan`, `CuratedInstallResult` |
| `internal/cli/mcp_registry.go` | Modified | Add `CmdMCPRegistryAddCurated`, `CmdMCPRegistryListCurated`, `CmdMCPCreds`, `CmdMCPActivate`; extend `CmdMCPList` to LEFT JOIN `mcp_curated_installs` when `--json` is requested |
| `internal/store/migrations/0NNN_mcp_curated_installs.sql` | New | DDL migration for the `mcp_curated_installs` table (Section 10.2) |

No Python source files are involved. The registry YAML is read-only at runtime (go:embed'd at compile time). Profile config writes use `knadh/koanf/v2` (existing pattern). SQLite access goes through `internal/store` with `modernc.org/sqlite` (CGO_ENABLED=0, pure-Go).

> **Runtime dependency flag:** All 20 curated servers in this bundle launch via `npx` (Node.js stdio) or connect over `streamable-http`. The Go binary orchestrates these external processes via `CommandTransport`/`StdioTransport` from `github.com/modelcontextprotocol/go-sdk v1.6.1`; it does **not** embed Node.js. `node` (≥18) must be present on the host PATH for npx-launched servers to function. This was also true under the Python implementation; the Go move does not change this runtime constraint. Users must understand the binary orchestrates external MCP server processes — it does not eliminate their runtime dependencies.

### 10.2 SQLite DDL — `mcp_curated_installs` Table

Delivered as a numbered migration in `internal/store/migrations/`. Applied by `internal/store.Migrate(db)` on first use. The underlying driver is `modernc.org/sqlite` (pure-Go, FTS5 built-in, CGO_ENABLED=0). Single-writer access is enforced by `gofrs/flock` on the database file; concurrent reads from `tag mcp list --json` use WAL mode without blocking.

```sql
-- Applied via internal/store.Migrate(db); WAL mode already set by store.OpenDB().

CREATE TABLE IF NOT EXISTS mcp_curated_installs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    profile         TEXT    NOT NULL,            -- profile name, e.g. "researcher"
    server_key      TEXT    NOT NULL,            -- matches registry.yaml servers key, e.g. "mcp-notion"
    status          TEXT    NOT NULL             -- 'active' | 'pending_credentials' | 'disabled'
                    CHECK (status IN ('active', 'pending_credentials', 'disabled')),
    curated_rank    INTEGER,                     -- rank within the curated bundle (1–20)
    category        TEXT,                        -- 'productivity' | 'devops' | 'database' | 'comms'
    ranking_sources TEXT,                        -- JSON array of source strings
    tool_count_est  INTEGER,                     -- estimated tool count at install time
    missing_env     TEXT,                        -- JSON array of missing env-var names at install time
    installed_at    TEXT    NOT NULL             -- ISO-8601 UTC timestamp
                    DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    installed_by    TEXT    NOT NULL             -- 'add-curated' | 'manual' | 'activate'
                    DEFAULT 'add-curated',
    activated_at    TEXT,                        -- ISO-8601 UTC, set when status transitions to 'active'
    notes           TEXT,                        -- free-form notes, e.g. 'disabled via --disable-playwright'
    UNIQUE (profile, server_key)
);

CREATE INDEX IF NOT EXISTS idx_mcp_curated_profile
    ON mcp_curated_installs (profile);

CREATE INDEX IF NOT EXISTS idx_mcp_curated_status
    ON mcp_curated_installs (profile, status);
```

### 10.3 Go Structs (replaces Python dataclasses)

```go
// internal/mcp/curated/curated.go
// Package curated implements the curated-bundle install planner for TAG.

package curated

import "database/sql"

// CuratedServerDef is parsed from the go:embed'd registry.yaml curated_bundle entry.
type CuratedServerDef struct {
    ServerKey         string   `yaml:"server_key"          json:"server_key"`
    Rank              int      `yaml:"rank"                json:"rank"`
    Category          string   `yaml:"category"            json:"category"` // "productivity"|"devops"|"database"|"comms"
    RankingSources    []string `yaml:"ranking_sources"     json:"ranking_sources"`
    ToolCountEstimate int      `yaml:"tool_count_estimate" json:"tool_count_estimate"`
    DefaultDisabled   bool     `yaml:"default_disabled"    json:"default_disabled"`
}

// MCPServerConfig is parsed from the registry.yaml servers entry.
type MCPServerConfig struct {
    Key               string   `yaml:"key"                 json:"key"`
    Description       string   `yaml:"description"         json:"description"`
    Category          string   `yaml:"category"            json:"category"`
    InstallType       string   `yaml:"install_type"        json:"install_type"` // "npx"|"uvx"|"docker"|"remote"
    Package           string   `yaml:"package"             json:"package,omitempty"`
    Command           string   `yaml:"command"             json:"command"`
    Args              []string `yaml:"args"                json:"args"`
    TransportType     string   `yaml:"transport_type"      json:"transport_type"` // "stdio"|"streamable-http"
    TransportURL      string   `yaml:"transport_url"       json:"transport_url,omitempty"`
    RequiresEnv       []string `yaml:"requires_env"        json:"requires_env"`
    ToolCountEstimate int      `yaml:"tool_count_estimate" json:"tool_count_estimate"`
    DockerImage       string   `yaml:"docker_image"        json:"docker_image,omitempty"`
}

// CuratedInstallPlan is computed before writes; used for dry-run and confirmation.
type CuratedInstallPlan struct {
    SelectedServers    []CuratedServerDef
    ServerConfigs      map[string]MCPServerConfig
    AlreadyInstalled   []string
    ToInstall          []string
    ToDisable          []string
    MissingEnv         map[string][]string // server_key -> missing var names
    PresentEnv         map[string][]string // server_key -> present var names
    CurrentActiveTools int
    AddedActiveTools   int
    BudgetExceeded     bool
    BudgetOverage      int // tools over the 40-tool soft limit
}

// CuratedInstallResult is returned after install completes; serialised to stdout in --json mode.
type CuratedInstallResult struct {
    Profile            string   `json:"profile"`
    Installed          []string `json:"installed"`
    SkippedExisting    []string `json:"skipped_existing"`
    Disabled           []string `json:"disabled"`
    PendingCredentials []string `json:"pending_credentials"`
    TotalActiveTools   int      `json:"total_active_tools"`
    Warnings           []string `json:"warnings"`
    Errors             []string `json:"errors"`
}
```

JSON schema annotations are generated via `invopop/jsonschema` for use in `--json` output validation and CI schema-stability tests (NFR-07).

### 10.4 `internal/mcp/registry/registry.yaml` — New Curated Server Entries

This file lives at `internal/mcp/registry/registry.yaml` and is compiled into the binary via `//go:embed registry.yaml` in `internal/mcp/registry/embed.go`:

```go
// internal/mcp/registry/embed.go
package registry

import (
    _ "embed"
    "gopkg.in/yaml.v3"
)

//go:embed registry.yaml
var registryYAML []byte

// LoadRegistry parses the embedded registry.yaml and returns a Registry value.
// Called once at startup; result is safe for concurrent reads (no writes at runtime).
func LoadRegistry() (*Registry, error) {
    var r Registry
    if err := yaml.Unmarshal(registryYAML, &r); err != nil {
        return nil, err
    }
    return &r, nil
}
```

The following 11 server keys are new additions to the `servers:` block. (9 already exist: `mcp-github`, `mcp-postgresql`, `mcp-slack`, `mcp-puppeteer` → replaced by `mcp-playwright`, `mcp-brave-search`, `mcp-filesystem`, `mcp-sqlite`, `mcp-fetch`, `mcp-memory`). The table below shows all 20 curated servers with their YAML keys. The YAML content itself is transport-level data and is language-agnostic:

```yaml
# --- NEW ENTRIES to append to servers: block in registry.yaml ---

  mcp-notion:
    description: "Notion workspace: create/read pages, query databases, search content"
    category: productivity
    install:
      type: npm
      package: "@notionhq/notion-mcp-server"
    config:
      command: "npx"
      args: ["-y", "@notionhq/notion-mcp-server"]
      transport: stdio
    requires_env: ["NOTION_API_KEY"]
    tool_count_estimate: 8
    profiles:
      recommended: [researcher, orchestrator]

  mcp-google-drive:
    description: "Google Drive: list, read, upload, and search files"
    category: productivity
    install:
      type: remote
      url: "https://mcp.googleapis.com/drive"
    config:
      command: null
      args: []
      transport: streamable-http
      transport_url: "https://mcp.googleapis.com/drive"
    requires_env: ["GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"]
    tool_count_estimate: 6
    profiles:
      recommended: [researcher]

  mcp-google-calendar:
    description: "Google Calendar: read/create/update events, manage calendars"
    category: productivity
    install:
      type: remote
      url: "https://mcp.googleapis.com/calendar"
    config:
      command: null
      args: []
      transport: streamable-http
      transport_url: "https://mcp.googleapis.com/calendar"
    requires_env: ["GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"]
    tool_count_estimate: 5
    profiles:
      recommended: [orchestrator]

  mcp-google-gmail:
    description: "Gmail: read threads, send messages, manage labels and drafts"
    category: productivity
    install:
      type: remote
      url: "https://mcp.googleapis.com/gmail"
    config:
      command: null
      args: []
      transport: streamable-http
      transport_url: "https://mcp.googleapis.com/gmail"
    requires_env: ["GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"]
    tool_count_estimate: 7
    profiles:
      recommended: [orchestrator]

  mcp-stripe:
    description: "Stripe payments: customers, charges, subscriptions, invoices, webhooks"
    category: productivity
    install:
      type: npm
      package: "@stripe/agent-toolkit"
    config:
      command: "npx"
      args: ["-y", "@stripe/agent-toolkit", "--transport", "stdio"]
      transport: stdio
    requires_env: ["STRIPE_SECRET_KEY"]
    tool_count_estimate: 12
    profiles:
      recommended: [orchestrator]

  mcp-playwright:
    description: "Browser automation: navigate, click, fill forms, screenshot, extract content"
    category: productivity
    install:
      type: npm
      package: "@executeautomation/playwright-mcp-server"
    config:
      command: "npx"
      args: ["-y", "@executeautomation/playwright-mcp-server"]
      transport: stdio
    requires_env: []
    tool_count_estimate: 25
    default_disabled: true
    profiles:
      recommended: [researcher]

  mcp-docker:
    description: "Docker: list containers/images, run containers, manage volumes and networks"
    category: devops
    install:
      type: npm
      package: "docker-mcp"
    config:
      command: "npx"
      args: ["-y", "docker-mcp"]
      transport: stdio
    requires_env: []
    tool_count_estimate: 9
    profiles:
      recommended: [coder, devops]

  mcp-jira:
    description: "Jira: issues, sprints, projects, comments, transitions"
    category: devops
    install:
      type: npm
      package: "@atlassian/jira-mcp"
    config:
      command: "npx"
      args: ["-y", "@atlassian/jira-mcp"]
      transport: stdio
    requires_env: ["JIRA_API_TOKEN", "JIRA_BASE_URL", "JIRA_USER_EMAIL"]
    tool_count_estimate: 10
    profiles:
      recommended: [orchestrator, devops]

  mcp-aws:
    description: "AWS multi-service: EC2, S3, Lambda, RDS, CloudWatch, IAM"
    category: devops
    install:
      type: npm
      package: "@aws-mcp/aws-mcp-server"
    config:
      command: "npx"
      args: ["-y", "@aws-mcp/aws-mcp-server"]
      transport: stdio
    requires_env: ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION"]
    tool_count_estimate: 18
    profiles:
      recommended: [devops, coder]

  mcp-vercel:
    description: "Vercel: deployments, projects, domains, environment variables, logs"
    category: devops
    install:
      type: npm
      package: "@vercel/mcp-adapter"
    config:
      command: "npx"
      args: ["-y", "@vercel/mcp-adapter"]
      transport: stdio
    requires_env: ["VERCEL_TOKEN"]
    tool_count_estimate: 8
    profiles:
      recommended: [coder, devops]

  mcp-cloudflare:
    description: "Cloudflare: DNS records, Workers, R2 storage, KV, Pages deployments"
    category: devops
    install:
      type: npm
      package: "@cloudflare/mcp-server-cloudflare"
    config:
      command: "npx"
      args: ["-y", "@cloudflare/mcp-server-cloudflare"]
      transport: stdio
    requires_env: ["CLOUDFLARE_API_TOKEN"]
    tool_count_estimate: 11
    profiles:
      recommended: [devops]

  mcp-sentry:
    description: "Sentry: issues, events, performance, releases, alerts"
    category: devops
    install:
      type: npm
      package: "@sentry/mcp-server"
    config:
      command: "npx"
      args: ["-y", "@sentry/mcp-server"]
      transport: stdio
    requires_env: ["SENTRY_AUTH_TOKEN", "SENTRY_ORG"]
    tool_count_estimate: 7
    profiles:
      recommended: [devops, coder]

  mcp-mongodb:
    description: "MongoDB: CRUD operations, aggregation pipelines, index management"
    category: database
    install:
      type: npm
      package: "@mongodb-js/mongodb-mcp-server"
    config:
      command: "npx"
      args: ["-y", "@mongodb-js/mongodb-mcp-server", "${MONGODB_URI}"]
      transport: stdio
    requires_env: ["MONGODB_URI"]
    tool_count_estimate: 9
    profiles:
      recommended: [coder]

  mcp-redis:
    description: "Redis: get/set/del keys, list/set/hash operations, pub/sub, streams"
    category: database
    install:
      type: npm
      package: "redis-mcp-server"
    config:
      command: "npx"
      args: ["-y", "redis-mcp-server", "--url", "${REDIS_URL}"]
      transport: stdio
    requires_env: ["REDIS_URL"]
    tool_count_estimate: 6
    profiles:
      recommended: [coder]

  mcp-hubspot:
    description: "HubSpot CRM: contacts, companies, deals, tickets, workflows"
    category: comms
    install:
      type: npm
      package: "@hubspot/mcp-server"
    config:
      command: "npx"
      args: ["-y", "@hubspot/mcp-server"]
      transport: stdio
    requires_env: ["HUBSPOT_API_KEY"]
    tool_count_estimate: 11
    profiles:
      recommended: [orchestrator]

  mcp-linear:
    description: "Linear: issues, projects, cycles, teams, roadmaps"
    category: comms
    install:
      type: npm
      package: "@linear/mcp-server"
    config:
      command: "npx"
      args: ["-y", "@linear/mcp-server"]
      transport: stdio
    requires_env: ["LINEAR_API_KEY"]
    tool_count_estimate: 7
    profiles:
      recommended: [orchestrator, coder]

  mcp-figma:
    description: "Figma: read files, nodes, comments; export components as code"
    category: comms
    install:
      type: npm
      package: "figma-mcp"
    config:
      command: "npx"
      args: ["-y", "figma-mcp"]
      transport: stdio
    requires_env: ["FIGMA_PERSONAL_TOKEN"]
    tool_count_estimate: 9
    profiles:
      recommended: [coder, researcher]

  mcp-twilio:
    description: "Twilio: send SMS/voice, manage phone numbers, look up numbers"
    category: comms
    install:
      type: npm
      package: "twilio-mcp"
    config:
      command: "npx"
      args: ["-y", "twilio-mcp"]
      transport: stdio
    requires_env: ["TWILIO_ACCOUNT_SID", "TWILIO_AUTH_TOKEN"]
    tool_count_estimate: 6
    profiles:
      recommended: [orchestrator]

  mcp-ahrefs:
    description: "Ahrefs SEO: keyword rankings, backlink analysis, site audit, competitor data"
    category: comms
    install:
      type: npm
      package: "ahrefs-mcp"
    config:
      command: "npx"
      args: ["-y", "ahrefs-mcp"]
      transport: stdio
    requires_env: ["AHREFS_API_KEY"]
    tool_count_estimate: 5
    profiles:
      recommended: [researcher]

# --- NEW TOP-LEVEL KEY: curated_bundle ---
curated_bundle:
  version: "1.0.0"
  last_updated: "2026-06-17"
  description: "Top 20 highest-value MCP servers ranked by Composio integrations, Smithery downloads, and MCP community votes"
  categories:
    productivity:
      description: "SaaS productivity and workspace tools"
      servers: [mcp-notion, mcp-google-drive, mcp-google-calendar, mcp-google-gmail, mcp-stripe, mcp-playwright]
    devops:
      description: "Infrastructure, CI/CD, and developer operations"
      servers: [mcp-github, mcp-docker, mcp-jira, mcp-aws, mcp-vercel, mcp-cloudflare, mcp-sentry]
    database:
      description: "Data stores and query interfaces"
      servers: [mcp-postgresql, mcp-mongodb, mcp-redis]
    comms:
      description: "Communication, CRM, project management, and analytics"
      servers: [mcp-slack, mcp-hubspot, mcp-linear, mcp-figma, mcp-twilio, mcp-ahrefs]
  servers:
    - server_key: mcp-notion
      rank: 1
      category: productivity
      ranking_sources: [composio_top_integrations, smithery_downloads]
      tool_count_estimate: 8
      default_disabled: false
    - server_key: mcp-github
      rank: 2
      category: devops
      ranking_sources: [composio_top_integrations, github_stars, smithery_downloads]
      tool_count_estimate: 15
      default_disabled: false
    - server_key: mcp-google-drive
      rank: 3
      category: productivity
      ranking_sources: [composio_top_integrations]
      tool_count_estimate: 6
      default_disabled: false
    - server_key: mcp-google-gmail
      rank: 4
      category: productivity
      ranking_sources: [composio_top_integrations]
      tool_count_estimate: 7
      default_disabled: false
    - server_key: mcp-slack
      rank: 5
      category: comms
      ranking_sources: [composio_top_integrations, smithery_downloads]
      tool_count_estimate: 8
      default_disabled: false
    - server_key: mcp-stripe
      rank: 6
      category: productivity
      ranking_sources: [composio_top_integrations, smithery_downloads]
      tool_count_estimate: 12
      default_disabled: false
    - server_key: mcp-docker
      rank: 7
      category: devops
      ranking_sources: [smithery_downloads, github_stars]
      tool_count_estimate: 9
      default_disabled: false
    - server_key: mcp-google-calendar
      rank: 8
      category: productivity
      ranking_sources: [composio_top_integrations]
      tool_count_estimate: 5
      default_disabled: false
    - server_key: mcp-jira
      rank: 9
      category: devops
      ranking_sources: [composio_top_integrations, smithery_downloads]
      tool_count_estimate: 10
      default_disabled: false
    - server_key: mcp-postgresql
      rank: 10
      category: database
      ranking_sources: [smithery_downloads, github_stars]
      tool_count_estimate: 14
      default_disabled: false
    - server_key: mcp-hubspot
      rank: 11
      category: comms
      ranking_sources: [composio_top_integrations]
      tool_count_estimate: 11
      default_disabled: false
    - server_key: mcp-aws
      rank: 12
      category: devops
      ranking_sources: [composio_top_integrations, smithery_downloads]
      tool_count_estimate: 18
      default_disabled: false
    - server_key: mcp-linear
      rank: 13
      category: comms
      ranking_sources: [smithery_downloads, mcp_community_vote]
      tool_count_estimate: 7
      default_disabled: false
    - server_key: mcp-vercel
      rank: 14
      category: devops
      ranking_sources: [smithery_downloads, github_stars]
      tool_count_estimate: 8
      default_disabled: false
    - server_key: mcp-cloudflare
      rank: 15
      category: devops
      ranking_sources: [smithery_downloads, github_stars]
      tool_count_estimate: 11
      default_disabled: false
    - server_key: mcp-mongodb
      rank: 16
      category: database
      ranking_sources: [smithery_downloads]
      tool_count_estimate: 9
      default_disabled: false
    - server_key: mcp-sentry
      rank: 17
      category: devops
      ranking_sources: [smithery_downloads, mcp_community_vote]
      tool_count_estimate: 7
      default_disabled: false
    - server_key: mcp-figma
      rank: 18
      category: comms
      ranking_sources: [smithery_downloads, mcp_community_vote]
      tool_count_estimate: 9
      default_disabled: false
    - server_key: mcp-redis
      rank: 19
      category: database
      ranking_sources: [smithery_downloads]
      tool_count_estimate: 6
      default_disabled: false
    - server_key: mcp-playwright
      rank: 20
      category: productivity
      ranking_sources: [github_stars, mcp_community_vote]
      tool_count_estimate: 25
      default_disabled: true
    - server_key: mcp-twilio
      rank: 21
      category: comms
      ranking_sources: [composio_top_integrations]
      tool_count_estimate: 6
      default_disabled: false
    - server_key: mcp-ahrefs
      rank: 22
      category: comms
      ranking_sources: [composio_top_integrations]
      tool_count_estimate: 5
      default_disabled: false
```

### 10.5 Core Algorithm: `ComputeInstallPlan`

```go
// internal/mcp/curated/curated.go

import (
    "database/sql"
    "encoding/json"
    "fmt"
    "os"
    "time"

    "github.com/tag-ai/tag/internal/mcp/registry"
)

// ComputeInstallPlan computes which servers to install, their env-var status,
// and the tool-budget impact. Makes no writes to SQLite or disk.
func ComputeInstallPlan(
    reg *registry.Registry,
    profile string,
    category string,       // "" = all categories
    disablePlaywright bool,
    db *sql.DB,
) (*CuratedInstallPlan, error) {
    // 1. Filter bundle entries by category if specified.
    entries := reg.CuratedBundle.Servers
    if category != "" {
        var filtered []registry.CuratedEntry
        for _, e := range entries {
            if e.Category == category {
                filtered = append(filtered, e)
            }
        }
        entries = filtered
    }

    // 2. Load already-installed server_keys for this profile.
    rows, err := db.Query(
        `SELECT server_key FROM mcp_curated_installs WHERE profile = ?`, profile,
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    alreadyInstalled := map[string]bool{}
    for rows.Next() {
        var key string
        if err := rows.Scan(&key); err != nil {
            return nil, err
        }
        alreadyInstalled[key] = true
    }

    // 3. Build selected defs; apply disable-playwright override.
    // Servers absent from reg.Servers are skipped with a warning (FR-08 — caller logs).
    var selected []CuratedServerDef
    for _, entry := range entries {
        if _, ok := reg.Servers[entry.ServerKey]; !ok {
            continue
        }
        def := CuratedServerDef{
            ServerKey:         entry.ServerKey,
            Rank:              entry.Rank,
            Category:          entry.Category,
            RankingSources:    entry.RankingSources,
            ToolCountEstimate: entry.ToolCountEstimate,
            DefaultDisabled:   entry.DefaultDisabled || (disablePlaywright && entry.ServerKey == "mcp-playwright"),
        }
        selected = append(selected, def)
    }

    // 4. Compute env-var status per server.
    missingEnv := map[string][]string{}
    presentEnv := map[string][]string{}
    for _, s := range selected {
        cfg := reg.Servers[s.ServerKey]
        for _, v := range cfg.RequiresEnv {
            if os.Getenv(v) == "" {
                missingEnv[s.ServerKey] = append(missingEnv[s.ServerKey], v)
            } else {
                presentEnv[s.ServerKey] = append(presentEnv[s.ServerKey], v)
            }
        }
    }

    // 5. Count currently active tools for the profile.
    currentActive, err := countActiveToolsForProfile(db, profile)
    if err != nil {
        return nil, err
    }

    // 6. Compute additions (exclude disabled and pending_credentials from budget).
    var toInstall, toDisable []string
    addedActive := 0
    for _, s := range selected {
        if !alreadyInstalled[s.ServerKey] {
            toInstall = append(toInstall, s.ServerKey)
        }
        if s.DefaultDisabled {
            toDisable = append(toDisable, s.ServerKey)
            continue
        }
        if !alreadyInstalled[s.ServerKey] && len(missingEnv[s.ServerKey]) == 0 {
            addedActive += s.ToolCountEstimate
        }
    }

    projected := currentActive + addedActive
    overage := projected - 40
    if overage < 0 {
        overage = 0
    }
    return &CuratedInstallPlan{
        SelectedServers:    selected,
        ServerConfigs:      reg.Servers,
        AlreadyInstalled:   mapKeys(alreadyInstalled),
        ToInstall:          toInstall,
        ToDisable:          toDisable,
        MissingEnv:         missingEnv,
        PresentEnv:         presentEnv,
        CurrentActiveTools: currentActive,
        AddedActiveTools:   addedActive,
        BudgetExceeded:     projected > 40,
        BudgetOverage:      overage,
    }, nil
}

// ExecuteInstallPlan writes mcp_curated_installs rows inside a single transaction.
// Does NOT modify profile config files — that is deferred to CmdMCPActivate for
// pending_credentials servers, and done immediately for fully-credentialed servers.
func ExecuteInstallPlan(plan *CuratedInstallPlan, profile string, db *sql.DB) (*CuratedInstallResult, error) {
    now := time.Now().UTC().Format(time.RFC3339)
    var installed, skipped, disabled, pending []string

    tx, err := db.Begin()
    if err != nil {
        return nil, err
    }
    defer tx.Rollback() //nolint:errcheck

    alreadySet := map[string]bool{}
    for _, k := range plan.AlreadyInstalled {
        alreadySet[k] = true
    }

    for _, s := range plan.SelectedServers {
        if alreadySet[s.ServerKey] {
            skipped = append(skipped, s.ServerKey)
            continue
        }
        missing := plan.MissingEnv[s.ServerKey]
        var status string
        switch {
        case s.DefaultDisabled:
            status = "disabled"
            disabled = append(disabled, s.ServerKey)
        case len(missing) > 0:
            status = "pending_credentials"
            pending = append(pending, s.ServerKey)
        default:
            status = "active"
            installed = append(installed, s.ServerKey)
        }
        missingJSON, _ := json.Marshal(missing)
        sourcesJSON, _ := json.Marshal(s.RankingSources)
        if _, err := tx.Exec(`
            INSERT OR REPLACE INTO mcp_curated_installs
                (profile, server_key, status, curated_rank, category,
                 ranking_sources, tool_count_est, missing_env, installed_at, installed_by)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'add-curated')`,
            profile, s.ServerKey, status, s.Rank, s.Category,
            string(sourcesJSON), s.ToolCountEstimate, string(missingJSON), now,
        ); err != nil {
            return nil, err
        }
    }
    if err := tx.Commit(); err != nil {
        return nil, err
    }

    var warnings []string
    if plan.BudgetExceeded {
        warnings = append(warnings, fmt.Sprintf(
            "Tool budget exceeded: %d active tools (soft limit: 40). Disable %d tools or use --category.",
            plan.CurrentActiveTools+plan.AddedActiveTools, plan.BudgetOverage,
        ))
    }
    return &CuratedInstallResult{
        Profile:            profile,
        Installed:          installed,
        SkippedExisting:    skipped,
        Disabled:           disabled,
        PendingCredentials: pending,
        TotalActiveTools:   plan.CurrentActiveTools + plan.AddedActiveTools,
        Warnings:           warnings,
        Errors:             nil,
    }, nil
}
```

### 10.6 Integration with `CmdMCPList`

The existing `CmdMCPList` handler in `internal/cli/mcp_registry.go` is extended to LEFT JOIN `mcp_curated_installs` when `--json` is requested:

```go
// internal/cli/mcp_registry.go

// enrichMCPListWithCurated adds curated_bundle metadata to each server that
// has a mcp_curated_installs row. Non-curated servers are left unchanged.
func enrichMCPListWithCurated(servers []MCPServerJSON, db *sql.DB, profile string) ([]MCPServerJSON, error) {
    rows, err := db.Query(`
        SELECT server_key, status, curated_rank, category, ranking_sources,
               installed_at, installed_by, activated_at
        FROM mcp_curated_installs WHERE profile = ?`, profile)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    type curatedRow struct {
        Status      string
        Rank        int
        Category    string
        Sources     string // JSON array
        InstalledAt string
        InstalledBy string
        ActivatedAt sql.NullString
    }
    curatedMap := map[string]curatedRow{}
    for rows.Next() {
        var key string
        var r curatedRow
        if err := rows.Scan(&key, &r.Status, &r.Rank, &r.Category,
            &r.Sources, &r.InstalledAt, &r.InstalledBy, &r.ActivatedAt); err != nil {
            return nil, err
        }
        curatedMap[key] = r
    }

    for i, s := range servers {
        if c, ok := curatedMap[s.Name]; ok {
            var sources []string
            _ = json.Unmarshal([]byte(c.Sources), &sources)
            servers[i].CuratedBundle = &CuratedBundleMeta{
                Rank:           c.Rank,
                Category:       c.Category,
                RankingSources: sources,
                InstalledAt:    c.InstalledAt,
                InstalledBy:    c.InstalledBy,
                ActivatedAt:    c.ActivatedAt.String,
            }
        }
    }
    return servers, nil
}
```

---

## 11. Security Considerations

1. **No secrets in YAML or SQLite:** `requires_env` stores only variable names, never values. `missing_env` column in `mcp_curated_installs` stores only the names of missing variables. Any debug logging must explicitly exclude environment variable values (`os.Environ()` dumps are forbidden in structured logs). The `internal/obs` secret-redaction helper must be applied to any structured log output from `CmdMCPRegistryAddCurated`.

2. **Supply chain risk for curated npm packages:** Each of the 20 curated servers is a third-party npm package. A compromised package could exfiltrate environment variables, filesystem contents, or API keys accessible to the agent. Mitigation: the `mcp-registry.yaml` `curated_bundle` entries should include a `pinned_version` field (future PRD) with a SHA-256 content hash of the npm package `.mcpc.json` contract snapshot. Until version pinning is implemented, users should be aware that `npx -y` always pulls the latest version.

3. **Google Workspace OAuth client secret exposure:** `GOOGLE_CLIENT_SECRET` is a long-lived credential. It must never be written to `mcp_curated_installs`, any log file, or any profile config file. The credential guide in `tag mcp creds` must explicitly warn that `GOOGLE_CLIENT_SECRET` should be stored in the OS keychain (e.g., `security add-generic-password -a google_mcp -s GOOGLE_CLIENT_SECRET -w <value>` on macOS) and referenced via a `keychain://` URI in config, not as a plain env-var export in shell config files.

4. **`pending_credentials` servers never activated without explicit action:** A server in `pending_credentials` state is registered in `mcp_curated_installs` but its `command`/`args` block is never written to the profile's Hermes MCP config until `tag mcp activate` is explicitly invoked. This prevents a server from inadvertently receiving API credentials that were added to the environment after the initial install.

5. **Tool-budget enforcement prevents context-window stuffing attacks:** An adversarial MCP server that advertises an unexpectedly large number of tools (e.g., 200) could fill the model's tool context and cause other tools to be dropped. The pre-flight budget check uses `tool_count_estimate` from the curated registry (not from the server's live tool manifest) as an upper bound. Actual tool count verification at connect time is handled by PRD-039 (Token Budget Enforcement).

6. **Playwright sandbox:** Playwright controls a real browser with network access. It must never be installed as `status = 'active'` by default (FR-08 enforces `default_disabled: true`). When a user explicitly enables Playwright, `tag mcp activate mcp-playwright` must print a one-time warning: "Playwright gives agents full browser control including access to authenticated sessions. Enable only for trusted agent profiles."

7. **Docker daemon access:** `mcp-docker` communicates with the host Docker daemon via the Docker socket. An agent with Docker MCP access can create privileged containers. Users should be warned at activate time to use Docker MCP only with trusted agent profiles and to configure Docker's `userns-remap` or socket proxy if running in multi-user environments.

---

## 12. Testing Strategy

### 12.1 Unit Tests

All unit tests live in `internal/mcp/curated/curated_test.go`, using `go test` with `github.com/stretchr/testify/assert`. SQLite is `modernc.org/sqlite` opened as `sql.Open("sqlite", ":memory:")`.

| Test | What it validates |
|------|------------------|
| `TestComputeInstallPlanFull` | All 20 servers in plan when no `--category` flag |
| `TestComputeInstallPlanCategoryDevops` | Only 7 devops servers in plan with `--category devops` |
| `TestComputeInstallPlanIdempotent` | Already-installed servers appear in `AlreadyInstalled`, not `ToInstall` |
| `TestEnvVarCheckMissing` | `MissingEnv` populated when env-vars absent (via `t.Setenv` to isolate) |
| `TestEnvVarCheckPresent` | `PresentEnv` populated when env-vars are set |
| `TestBudgetCheckExceeds` | `BudgetExceeded=true` when projected total > 40 |
| `TestBudgetCheckWithin` | `BudgetExceeded=false` for category subset under 40 tools |
| `TestDisablePlaywright` | Playwright has `status="disabled"` in result when `disablePlaywright=true` |
| `TestExecuteInstallPlanWritesSQLite` | `mcp_curated_installs` has correct rows after `ExecuteInstallPlan` |
| `TestExecuteInstallPlanIdempotent` | Running `ExecuteInstallPlan` twice produces same SQLite state (INSERT OR REPLACE) |
| `TestMissingServerKeyWarns` | Server key in `curated_bundle` but absent from `servers:` does not panic; plan excludes it |
| `TestMCPListJSONCuratedField` | `CmdMCPList --json` output includes `curated_bundle` for installed servers |
| `TestMCPListJSONNoCuratedField` | Non-curated servers omit `curated_bundle` key from JSON |
| `TestPendingCredentialsStatus` | Server with missing env-var gets `status="pending_credentials"` |
| `TestActivateTransitionsStatus` | `CmdMCPActivate` updates row to `status="active"` when all env-vars present |

### 12.2 Integration Tests

Located in `internal/mcp/curated/integration_test.go`. These tests use a real SQLite in-memory DB via `sql.Open("sqlite", ":memory:")` with `modernc.org/sqlite` and a minimal `registry.yaml` fixture embedded via `//go:embed testdata/minimal_registry.yaml`.

| Test | What it validates |
|------|------------------|
| `TestAddCuratedE2EDryRun` | `CmdMCPRegistryAddCurated(dryRun=true)` returns nil error and makes no SQLite writes |
| `TestAddCuratedE2EFullInstall` | All 20 servers written to `mcp_curated_installs` after full install |
| `TestAddCuratedE2ECategoryOnly` | Only category-filtered servers written to `mcp_curated_installs` |
| `TestAddCuratedYAMLRoundTrip` | `registry.yaml` round-trips cleanly through `gopkg.in/yaml.v3` `Unmarshal`/`Marshal` without data loss |
| `TestMCPCredsOutputCompleteness` | `CmdMCPCreds` lists all servers with `status="pending_credentials"` |
| `TestMCPActivateLive` | After `t.Setenv` for a required var, `CmdMCPActivate` flips status and writes profile config |

### 12.3 Fixture: Minimal `registry.yaml` for Tests

```go
// internal/mcp/curated/testdata/minimal_registry.yaml
// Loaded in tests via //go:embed testdata/minimal_registry.yaml in a test helper.
```

```yaml
servers:
  mcp-notion:
    description: "Notion"
    category: productivity
    install:
      type: npm
      package: "@notionhq/notion-mcp-server"
    config:
      command: "npx"
      args: ["-y", "@notionhq/notion-mcp-server"]
      transport: stdio
    requires_env: ["NOTION_API_KEY"]
    tool_count_estimate: 8

curated_bundle:
  version: "1.0.0"
  categories:
    productivity:
      servers: [mcp-notion]
  servers:
    - server_key: mcp-notion
      rank: 1
      category: productivity
      ranking_sources: [composio_top_integrations]
      tool_count_estimate: 8
      default_disabled: false
```

### 12.4 Performance Tests

- `BenchmarkComputeInstallPlanDryRun`: Assert `ComputeInstallPlan` (registry pre-loaded, in-memory SQLite) completes in < 200 ms using `testing.B`.
- `BenchmarkExecuteInstallPlanFull`: Assert 20-server SQLite write via `ExecuteInstallPlan` completes in < 500 ms.
- `BenchmarkMCPListJSONLatency`: Assert `CmdMCPList --json` with 20 curated rows completes in < 100 ms (SQLite read only).

---

## 13. Acceptance Criteria

| ID | Criterion | How to verify |
|----|-----------|---------------|
| AC-01 | `tag mcp registry add-curated --dry-run` exits 0 and prints a plan table with exactly 20 servers | Run command; inspect exit code and stdout line count |
| AC-02 | `tag mcp registry add-curated --profile test` writes exactly 20 rows to `mcp_curated_installs` | Query `SELECT COUNT(*) FROM mcp_curated_installs WHERE profile='test'` = 20 |
| AC-03 | Re-running `add-curated` on a profile that already has all 20 servers exits 0 and reports "20 skipped (already installed)" | Run twice; second run exit code and stdout |
| AC-04 | `add-curated --category devops` writes exactly 7 rows (GitHub, Docker, Jira, AWS, Vercel, Cloudflare, Sentry) | Query with `WHERE category='devops'` |
| AC-05 | A server with a missing required env-var is written with `status='pending_credentials'` | Unset `NOTION_API_KEY`; run `add-curated`; query row |
| AC-06 | A server with all required env-vars present is written with `status='active'` | Set all required vars for one server; verify status |
| AC-07 | Playwright is written with `status='disabled'` when `--disable-playwright` is passed | Pass flag; verify row status |
| AC-08 | `tag mcp list --json` output includes `curated_bundle` object for each curated-installed server | Parse JSON; check key presence for known curated server |
| AC-09 | `tag mcp list --json` output does NOT include `curated_bundle` for non-curated servers | Parse JSON; verify key absent for `mcp-filesystem` |
| AC-10 | `tag mcp registry list-curated` prints all 20 servers in a table with rank, category, description, and tool count | Inspect stdout; verify 20 rows |
| AC-11 | `tag mcp registry list-curated --profile <name>` annotates each row with install status from `mcp_curated_installs` | Install some; run command; verify YES/no annotations |
| AC-12 | `tag mcp creds --profile <name>` lists all `pending_credentials` servers with their required env-var names and setup URLs | Run after install with missing env-vars; inspect output |
| AC-13 | `tag mcp activate mcp-notion --profile <name>` with `NOTION_API_KEY` set transitions the row to `status='active'` | Set env-var; run activate; query row |
| AC-14 | `add-curated` prints a tool-budget WARNING when projected total exceeds 40 | Install full bundle; verify WARNING in stdout |
| AC-15 | `add-curated --dry-run` makes zero writes to `mcp_curated_installs` | Run dry-run; query row count = 0 |
| AC-16 | No secret values appear in any log output or SQLite column | Run with real env-var; grep logs and SQLite for value |
| AC-17 | `add-curated --json` outputs valid JSON to stdout; no human-readable text on stdout | Parse stdout as JSON; verify stderr has progress |
| AC-18 | `internal/mcp/registry/registry.yaml` parses cleanly via `gopkg.in/yaml.v3` and all 20+ server keys are present | `go test ./internal/mcp/registry/...`; assert key set |
| AC-19 | All 20 curated servers use transport type `stdio` (npx/uvx/docker) or `streamable-http` (remote); none use `sse` | Assert `transport != 'sse'` for all entries |
| AC-20 | `add-curated --category database` does not modify or re-register any `productivity`, `devops`, or `comms` servers already installed | Install `devops`; then install `database`; verify `devops` rows unchanged |

---

## 14. Dependencies

| ID | Dependency | Type | Notes |
|----|-----------|------|-------|
| D1 | PRD-014 — MCP Server Registry & Discovery | Hard | `mcp-registry.yaml` schema, `cmd_mcp_list`, `cmd_mcp_registry_install` patterns must exist. |
| D2 | PRD-039 — Token Budget Enforcement | Soft | Tool-budget pre-flight in Section 10.5 uses `tool_count_estimate` from YAML; runtime enforcement of budget is PRD-039's responsibility. |
| D3 | PRD-034 — Secret Scanning | Soft | `security.py` `redact_secrets()` must be applied to all structured log output. |
| D4 | PRD-013 — Tracing | Soft | `add-curated` emits an OTel span (`mcp.curated.install`) with attributes: `profile`, `category`, `servers_installed_count`, `servers_pending_count`. |
| D5 | PRD-028 — Sandbox Code Execution | Informational | Playwright and Docker MCP servers should be enabled only in sandbox-aware profiles. No hard code dependency. |
| D6 | `gopkg.in/yaml.v3` (Go module) | External | Required for `registry.yaml` parsing in `internal/mcp/registry`. Already a transitive dependency of `knadh/koanf/v2`. Replaces `ruamel.yaml`. Note: `gopkg.in/yaml.v3` does not preserve YAML comments on round-trip; `registry.yaml` is treated as read-only at runtime so this is not a concern. |
| D7 | `@notionhq/notion-mcp-server` npm package | External runtime | Not a Go dependency; launched at agent runtime via `npx` over stdio (`CommandTransport` from go-sdk). `node` ≥18 must be on the host PATH. |
| D8 | `@atlassian/jira-mcp` npm package | External runtime | Same pattern — runtime only, node required on host. |
| D11 | `github.com/modelcontextprotocol/go-sdk v1.6.1` | External | MCP protocol 2025-11-25. Used in `internal/mcp` to connect to `npx`-launched servers (CommandTransport/Stdio) and to remote HTTP endpoints (StreamableHTTP). Pin `MCPVersion = "2025-11-25"` as a single const in `internal/mcp`. |
| D12 | `modernc.org/sqlite` | External | Pure-Go SQLite driver (CGO_ENABLED=0, FTS5 built-in) used by `internal/store` for `mcp_curated_installs`. Replaces `aiosqlite`/`sqlite3`. Single-writer isolation via `gofrs/flock`. |
| D9 | Google Cloud OAuth 2.0 Client | External service | Required by `mcp-google-*` servers. Users must create credentials at console.cloud.google.com. |
| D10 | MCP OAuth Integration PRD (unscheduled) | Future | Full OAuth 2.1 PKCE flow for Google Workspace and other OAuth-gated servers. This PRD records `pending_credentials` state and defers OAuth to that future PRD. |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|------------------|
| OQ1 | Should `add-curated` write the MCP server `command`/`args` block to the Hermes profile config immediately for `active` servers, or should a separate `tag mcp activate` step always be required? Immediate write is more ergonomic but may surprise users who didn't expect their profile config to be modified. | @sanskarpan | Before implementation begins |
| OQ2 | The Google Workspace servers use `remote` / `streamable-http` transport pointing to `mcp.googleapis.com`. Are these endpoints generally available and stable, or do they require a beta allowlist? If unavailable, should we fall back to the community `@google/mcp-server-*` npm packages which use stdio? | @sanskarpan | Verify with Google docs before shipping |
| OQ3 | Tool count estimates in `mcp-registry.yaml` are editorial (hand-counted from server docs). Should we add a CI step that connects to each server, calls `tools/list`, and fails if the actual count deviates from `tool_count_estimate` by more than 20%? This would catch tool additions in new server versions that break budget assumptions. | @sanskarpan | Resolve in version-pinning PRD |
| OQ4 | Should `--category` be mutually exclusive with a future `--server mcp-notion,mcp-github` flag that allows picking individual servers from the bundle? Or should the interface be `add-curated --server mcp-notion mcp-github` (variadic)? | @sanskarpan | Defer to post-ship issue if demand arises |
| OQ5 | Ahrefs has a public API but the MCP server (`ahrefs-mcp`) is a community package, not an official Ahrefs product. Should we gate the curated bundle on "officially maintained by the vendor" or accept well-maintained community packages? | @sanskarpan | Editorial decision before ship |
| OQ6 | Should `tag mcp creds` emit its output as a shell-sourceable script (i.e., `#!/usr/bin/env sh` with `export VAR=...` stubs) when `--script` is passed, so teams can add credential scaffolding to their onboarding runbooks? | @sanskarpan | Low-priority follow-up; not blocking ship |
| OQ7 | `mcp-redis` and `mcp-mongodb` both accept a connection URI containing credentials. The URI may appear in `args` as `${REDIS_URL}`. Should TAG validate that these args contain only `${}` placeholders (never raw URIs) before writing to the profile config, to prevent credential logging? | @sanskarpan | Security review before ship |

---

## 16. Complexity and Timeline

**Estimated Total Effort:** XS — 1.5 days

| Phase | Tasks | Estimate |
|-------|-------|----------|
| Phase 1 — YAML authoring (0.5 day) | Write 11 new server entries in `internal/mcp/registry/registry.yaml`; write the `curated_bundle` top-level key with all 20 ranked entries; verify the file parses cleanly via `gopkg.in/yaml.v3` (`go test ./internal/mcp/registry/...`) | 0.5 day |
| Phase 2 — Go handlers (0.5 day) | Implement `CuratedServerDef`, `ComputeInstallPlan`, `ExecuteInstallPlan` in `internal/mcp/curated/curated.go`; add `internal/mcp/registry/embed.go`; add CLI handlers `CmdMCPRegistryAddCurated`, `CmdMCPRegistryListCurated`, `CmdMCPCreds`, `CmdMCPActivate` in `internal/cli/mcp_registry.go`; add `internal/store` migration for `mcp_curated_installs`; extend `CmdMCPList` JSON output | 0.5 day |
| Phase 3 — Tests (0.5 day) | Write `internal/mcp/curated/curated_test.go` and `internal/mcp/curated/integration_test.go` with all 15 unit tests and 6 integration tests using `go test` + `testify`; add benchmark functions for latency assertions; add CI step | 0.5 day |

**Parallel work:** YAML authoring (Phase 1) and DDL design can be done concurrently with no dependency.

**Risk:** Low. The primary risk is that one or more npm package names differ from what is documented here (e.g., if `@atlassian/jira-mcp` is the wrong package name). This is resolved by spot-checking each package on npmjs.com during Phase 1 YAML authoring — a 30-minute task. The Go refactor itself is low risk because the registry YAML is language-agnostic data and the Go packages (`internal/mcp`, `internal/store`, `internal/cli`) already provide the patterns needed. No architectural risk.

**Definition of Done:**
- `internal/mcp/registry/registry.yaml` passes `gopkg.in/yaml.v3` unmarshal + schema assertion in CI (`go test ./internal/mcp/registry/...`).
- All 20 AC items pass in CI on macOS and Linux runners.
- `tag mcp registry add-curated --dry-run` output reviewed and approved by a second team member.
- Security review of Section 11 items OQ7 and the Playwright/Docker warnings completed.
- Issue #346 closed with a reference to the merged PR.

