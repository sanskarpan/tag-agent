# PRD-073: Live MCP Registry Sync from modelcontextprotocol.io (`tag mcp registry update`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (3-5 days)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `internal/cli/mcp_registry.go (RegistryUpdateCmd)`, `internal/mcp/registry/`, `internal/store/migrations/073_mcp_registry.sql`, `assets/mcp-registry.yaml`
**Depends on:** PRD-014 (MCP server registry), PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (secret scanning), PRD-043 (vector-based tool retrieval)
**Inspired by:** MCP Registry (modelcontextprotocol.io), Smithery, mcp.so

---

## 1. Overview

TAG currently ships a hand-curated `assets/mcp-registry.yaml` containing exactly 10 MCP server entries. This static file is the sole source of truth for `tag mcp registry list`, `tag mcp registry search`, and `tag mcp registry install`. As of June 2026, the official MCP registry at `https://registry.modelcontextprotocol.io` lists 9,652+ server entries spanning filesystem tools, database connectors, browser automation, productivity suites, ML pipelines, and specialized domain tools. The gap between 10 curated entries and 9,652+ live entries is the single largest bottleneck preventing TAG users from accessing the full MCP ecosystem through a managed install path.

This PRD specifies `tag mcp registry update`, a command that performs a cursor-paginated scrape of the official MCP registry REST API and persists the result into a local SQLite cache at `~/.tag/runtime/tag.sqlite3` (WAL mode, accessed via `internal/store`). Subsequent commands — `tag mcp registry list`, `tag mcp registry search`, `tag mcp registry install` — query this SQLite cache rather than the bundled YAML, enabling full-text search over the complete 9,652+ entry corpus with sub-100ms latency. The command also accepts `--source` to target subregistries (Smithery, mcp.so) that implement the same OpenAPI spec and inject custom `_meta` fields with ratings and security scan results.

The design follows the established TAG patterns rigorously. The scrape loop uses `net/http` with `cenkalti/backoff/v4` exponential backoff and graceful 500-handling (the registry is explicitly preview-grade and resets occur). An `updated_since` parameter enables incremental syncs that touch only entries modified since the last successful scrape, reducing typical sync time from minutes to seconds after the initial cold pull. The SQLite schema uses FTS5 virtual tables for full-text search over server name, description, and package identifiers — FTS5 is compiled into `modernc.org/sqlite` by default with `CGO_ENABLED=0`. The tool budget awareness feature embeds `tool_count` from the registry `tools[]` array so that `tag mcp registry install` can warn before crossing the 40-tool Cursor ceiling.

The implementation is designed for a 3-5 day engineering sprint. The core scrape-and-cache loop, SQLite schema migration, search command, and install method abstraction (npx/uvx/docker/remote) constitute the critical path. Secondary features — `--json` structured output, curated add-backs, tool budget preflight — are additive and non-blocking.

---

## 2. Problem Statement

### 2.1 The Static YAML is a Critical Bottleneck

The bundled `mcp-registry.yaml` has 10 entries. The live ecosystem has 9,652+. Every server outside those 10 entries requires the user to manually find the npm/PyPI package name, construct the `npx`/`uvx` invocation, figure out required environment variables, and hand-edit `lab-config.yaml` inside the target profile directory. This process takes 10-30 minutes per server and produces configuration that is not validated, not version-tracked, and not shareable. Users who want to add Notion, Playwright, Context7, or Linear integration currently have zero managed path through TAG. This is a retention problem: users who discover they need to hand-edit YAML tend to look for alternative tools.

### 2.2 Search is Absent from the Discovery Path

`tag mcp registry list` produces a 10-row table. `tag mcp registry search` does substring matching over those same 10 rows. There is no semantic search, no category filter, no popularity sort, and no way to discover servers for a use case ("calendar scheduling", "browser automation", "vector database") without already knowing the server name. The tool retrieval semantic search infrastructure exists and is operational, but it cannot index what it does not have. Connecting the live registry to the existing search infrastructure requires only the cache layer specified in this PRD.

### 2.3 Install Method Diversity is Unhandled

The 10 bundled servers are all npm packages launched via `npx`. The live registry includes servers distributed as:
- npm packages (`npx -y <pkg>` / `npm install -g <pkg>`)
- Python packages (`uvx <pkg>` / `pip install <pkg>` then `python -m <module>`)
- Docker images (`docker run -i --rm <image>`)
- Vendor-hosted cloud endpoints (remote streamable-HTTP URLs — Google Workspace, Atlassian Rovo, Linear)
- SSE-only legacy servers (deprecated; must be skipped or proxied)

The current `cmd_mcp_registry` in `internal/cli/mcp_registry.go` handles only `npm` and `pip` install types. Docker and remote-URL servers silently fail or are incorrectly omitted. This leaves the fastest-growing segment of the registry (vendor-hosted cloud MCP) completely inaccessible via `tag mcp registry install`.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | `tag mcp registry update` performs a full cursor-paginated scrape of `https://registry.modelcontextprotocol.io/v0.1/servers` and persists all entries into the `mcp_registry_servers` and `mcp_registry_packages` SQLite tables. |
| G2 | Incremental syncs using `updated_since` reduce re-sync time after the initial cold pull to under 30 seconds on a typical broadband connection. |
| G3 | `tag mcp registry search "<query>"` performs FTS5 full-text search over the 9,652+ entry corpus and returns results in under 100ms with `--json` output option. |
| G4 | `tag mcp registry install <server> [<server>...]` selects the correct install method (npx/uvx/docker/remote-url) based on the registry `packages[]` array and writes a validated MCP config block to the target profile's `lab-config.yaml`. |
| G5 | `tag mcp registry install` warns when adding a server would push the active profile over the 40-tool Cursor ceiling, and shows the estimated tool count from the registry `tools[]` array. |
| G6 | `tag mcp registry add-curated` re-applies the 10 hand-curated YAML entries on top of the live registry data, ensuring bundled defaults are never lost after an `update`. |
| G7 | `--source <url>` supports subregistries (Smithery, mcp.so) that implement the same OpenAPI spec; `_meta` fields from subregistries are preserved in the `meta_json` column. |
| G8 | The implementation tolerates HTTP 500 responses and empty cursor pages gracefully; partial scrape results are committed incrementally so a timeout does not lose all progress. |
| G9 | `tag mcp registry list --json` emits structured JSON suitable for piping to `jq` and compatible with the `tag tool index` pipeline (PRD-043). |
| G10 | All scrape activity is written to the `mcp_registry_sync_log` table for diagnostics and `tag doctor` integration. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Hosting or operating a TAG-owned registry. TAG is a consumer of existing registries, not a publisher. |
| NG2 | Authenticating to the registry API. The MCP registry v0.1 is publicly readable and unauthenticated; this PRD does not implement OAuth for private subregistries. |
| NG3 | Real-time streaming of registry updates. Syncs are manual (`tag mcp registry update`) or cron-driven via `go-co-op/gocron v2`; no WebSocket or SSE subscription. |
| NG4 | Automatic nightly sync as a daemon. Cron-based scheduling is supported by `gocron` (PRD-022) but is out of scope for this PRD's implementation. Users may configure it separately. |
| NG5 | MCP server development scaffolding or publishing to the registry. |
| NG6 | Evaluating or benchmarking installed MCP servers. Quality assessment of server behavior is out of scope; only metadata from the registry is surfaced. |
| NG7 | Resolving conflicting `_meta` fields when multiple subregistries cover the same `serverName`. The last-write-wins strategy applies. |
| NG8 | Replacing the bundled `mcp-registry.yaml`. The YAML remains the zero-network fallback; the SQLite cache supplements it. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Cold sync completeness | >= 9,000 servers synced from live registry on first `update` run | Row count in `mcp_registry_servers` after initial sync |
| Cold sync speed | < 5 minutes for full 9,652+ entry scrape on 50 Mbps connection | Wall-clock timing in `mcp_registry_sync_log.duration_ms` |
| Incremental sync speed | < 30 seconds when < 200 entries changed since last sync | Wall-clock timing; compare `entries_updated` vs `duration_ms` |
| Search latency | p99 < 100ms for FTS5 query over full corpus | Integration test timing across 20 representative queries |
| Search relevance | Top-3 result for canonical queries ("github", "filesystem", "postgres", "browser automation") matches expected server | Automated test assertions |
| Install correctness | `tag mcp registry install mcp-github` produces a valid `lab-config.yaml` MCP block on a clean profile | Integration test with profile state assertion |
| Tool count warning | Cursor 40-tool limit warning fires correctly when cumulative tools cross 40 | Unit test with mocked registry tool counts |
| HTTP resilience | Scrape continues after 3 consecutive HTTP 500s from registry API | Unit test with mocked `httptest.Server` responses |
| Zero regression | `tag mcp registry list` with no prior `update` still returns the 10 bundled entries | CI unit test on fresh install state |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Developer new to MCP | run `tag mcp registry update` then `tag mcp registry search "calendar scheduling"` | I can find all calendar MCP servers without knowing their package names in advance |
| U2 | Developer | run `tag mcp registry install notion playwright-mcp context7 github` in one command | All four servers are installed and configured in my active profile without hand-editing YAML |
| U3 | DevOps engineer | run `tag mcp registry update --source https://registry.smithery.ai` | I get Smithery's curated catalog with ratings and security scan results alongside the official entries |
| U4 | Developer working in a Cursor project | run `tag mcp registry install playwright-mcp` and see a budget warning | I know before I install that Playwright adds 25 tools and will push my total over the 40-tool Cursor limit |
| U5 | Developer maintaining profiles | run `tag mcp registry list --json \| jq '.[] \| select(.category=="database")'` | I can filter and pipe registry data into my own scripts |
| U6 | Developer on a fresh machine | run `tag mcp registry list` with no prior `update` | I still see the 10 bundled entries as a zero-network fallback |
| U7 | Team lead setting up a shared environment | run `tag mcp registry add-curated` | The 10 hand-tested, production-safe servers are enabled without risking untested community entries |
| U8 | Developer after a sync error | run `tag mcp registry update` again | Incremental sync picks up from the last successful page rather than restarting from page 1 |
| U9 | Operator running `tag doctor` | see sync freshness and error counts in doctor output | I can detect when the registry cache is stale (>7 days) or has repeated 500 errors |
| U10 | Developer needing a Python MCP server | run `tag mcp registry install mcp-server-context7` | TAG detects the `pypi` package type and uses `uvx` (not `npx`) for the install invocation |

---

## 7. Proposed CLI Surface

### 7.1 `tag mcp registry update`

```
tag mcp registry update [--source URL] [--limit N] [--dry-run] [--json]
```

Flags:
- `--source URL` — Override the base registry URL (default: `https://registry.modelcontextprotocol.io`). Accepts any URL implementing the `/v0.1/servers` OpenAPI spec.
- `--limit N` — Stop after N servers (useful for testing; default: unlimited).
- `--dry-run` — Fetch and count pages but do not write to SQLite. Prints page-by-page progress.
- `--json` — Emit a JSON sync summary instead of human-readable output.

Example output (human):
```
Syncing MCP registry from https://registry.modelcontextprotocol.io ...
  Page  1 / ~97:  100 servers  [cursor: io.github.anthropics/computer-use:1.0.0]
  Page  2 / ~97:  100 servers  [cursor: io.github.azure/mcp:0.3.1]
  ...
  Page 97 / ~97:   52 servers  [cursor: null — done]

Registry sync complete.
  Servers synced: 9,652
  New:            321
  Updated:        44
  Unchanged:      9,287
  Errors:         0
  Duration:       2m 14s
  Next sync:      tag mcp registry update  (or schedule via gocron)
```

Example output (JSON, `--json`):
```json
{
  "source": "https://registry.modelcontextprotocol.io",
  "synced_at": "2026-06-17T10:23:45Z",
  "servers_total": 9652,
  "servers_new": 321,
  "servers_updated": 44,
  "servers_unchanged": 9287,
  "errors": 0,
  "duration_ms": 134211,
  "cursor_final": null
}
```

### 7.2 `tag mcp registry search`

```
tag mcp registry search "<query>" [--category CAT] [--transport TYPE] [--limit N] [--json]
```

Flags:
- `--category` — Filter by category: `filesystem`, `database`, `web`, `messaging`, `vcs`, `ml`, `productivity`, `other`.
- `--transport` — Filter by transport type: `stdio`, `streamable-http`, `docker`.
- `--limit N` — Maximum results (default: 20).
- `--json` — Structured JSON output.

Example (human):
```
$ tag mcp registry search "calendar scheduling" --limit 5

Searching MCP registry for "calendar scheduling" (9,652 servers indexed)

  #  Name                              Category      Transport  Description
 ──────────────────────────────────────────────────────────────────────────────────────
  1  io.github.nspilman/google-cal    productivity  stdio      Google Calendar read/write via OAuth
  2  io.github.reclaim-ai/mcp         productivity  stdio      Reclaim.ai smart scheduling assistant
  3  io.github.cal.com/mcp-server     productivity  stdio      Cal.com meeting scheduling and booking
  4  io.github.fantastical/mcp        productivity  stdio      Fantastical calendar integration (macOS)
  5  io.github.notion-mcp-server/...  productivity  stdio      Notion databases including calendar views

  Run: tag mcp registry install <name>  to install any server.
```

Example (JSON):
```json
[
  {
    "name": "io.github.nspilman/google-cal",
    "description": "Google Calendar read/write via OAuth",
    "category": "productivity",
    "transport_type": "stdio",
    "package_type": "npm",
    "package_identifier": "@nspilman/google-calendar-mcp",
    "version": "1.2.0",
    "tool_count": 8,
    "requires_env": ["GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"],
    "repository_url": "https://github.com/nspilman/google-calendar-mcp",
    "published_at": "2026-01-14T09:22:00Z",
    "_meta": {}
  }
]
```

### 7.3 `tag mcp registry install`

```
tag mcp registry install <server> [<server>...] [--profile PROFILE] [--force] [--dry-run] [--json]
```

Flags:
- `--profile` — Target profile (default: `master_profile` from config, or `orchestrator`).
- `--force` — Install even if server is already configured in the profile.
- `--dry-run` — Show the config block that would be written without modifying any file.
- `--json` — Emit install result as JSON.

Example (human):
```
$ tag mcp registry install notion playwright-mcp context7 github --profile coder

Installing 4 MCP servers into profile 'coder':

  [1/4] notion
        Package:   @notionhq/notion-mcp-server (npm)
        Transport: stdio
        Tools:     12  (cumulative: 12 / 40)
        Env vars:  NOTION_API_KEY  [not set — add to profile env before use]
        ✓ Written to ~/.tag/profiles/coder/lab-config.yaml

  [2/4] playwright-mcp
        Package:   @playwright/mcp (npm)
        Transport: stdio
        Tools:     25  (cumulative: 37 / 40)
        ⚠ Tool budget: 37/40 tools — 3 remaining before Cursor limit
        ✓ Written to ~/.tag/profiles/coder/lab-config.yaml

  [3/4] context7
        Package:   context7-mcp (pypi → uvx)
        Transport: stdio
        Tools:     4   (cumulative: 41 / 40)
        ✗ Tool budget exceeded: adding context7 would reach 41 tools (Cursor limit: 40)
        Use --force to install anyway.

  [4/4] github
        Package:   @modelcontextprotocol/server-github (npm)
        Transport: stdio
        Tools:     6
        ✗ Skipped (budget exceeded — use --force to override)

Installed 2/4 servers. 1 warning, 1 error.
Re-run with --force to bypass the tool budget check.
```

### 7.4 `tag mcp registry add-curated`

```
tag mcp registry add-curated [--profile PROFILE] [--json]
```

Re-applies the 10 bundled YAML entries from `assets/mcp-registry.yaml` (embedded via `go:embed`) into the SQLite cache and the target profile's `lab-config.yaml`. Does not remove any existing configured servers. Intended for onboarding new profiles with the known-good baseline set.

Example:
```
$ tag mcp registry add-curated --profile researcher

Adding curated MCP servers to profile 'researcher':
  ✓ mcp-filesystem
  ✓ mcp-brave-search
  ✓ mcp-fetch
  ✓ mcp-memory
  ✓ mcp-sequentialthinking
  - mcp-github         (already configured — skipped)
  - mcp-postgres       (already configured — skipped)
  - mcp-sqlite         (already configured — skipped)
  - mcp-slack          (env SLACK_BOT_TOKEN not set — skipped, use --force)
  - mcp-puppeteer      (not in recommended list for 'researcher' — skipped)

5 servers added, 3 skipped (already configured), 1 skipped (missing env), 1 skipped (not recommended).
```

### 7.5 `tag mcp registry list`

```
tag mcp registry list [--category CAT] [--transport TYPE] [--installed] [--json]
```

Flags:
- `--installed` — Show only servers currently configured in the active profile.
- Inherits `--category`, `--transport`, `--json` from `search`.

Example (no prior update — YAML fallback):
```
$ tag mcp registry list

MCP Registry  [source: bundled YAML — run 'tag mcp registry update' to sync 9,652+ servers]

  Name                    Category    Transport  Description
  ────────────────────────────────────────────────────────────────────────────
  mcp-filesystem          filesystem  stdio      Read, write, and search files on the local filesystem
  mcp-brave-search        web         stdio      Web search via Brave Search API
  mcp-github              vcs         stdio      GitHub repository operations
  mcp-postgres            database    stdio      PostgreSQL read/write access via MCP
  mcp-sqlite              database    stdio      SQLite database access via MCP
  mcp-fetch               web         stdio      HTTP fetch and web scraping tool
  mcp-memory              memory      stdio      In-memory key-value store for agent state
  mcp-sequentialthinking  reasoning   stdio      Sequential reasoning and chain-of-thought tool
  mcp-slack               messaging   stdio      Send and read Slack messages via MCP
  mcp-puppeteer           web         stdio      Browser automation and web scraping via Puppeteer

10 servers (bundled). Last sync: never.
```

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `RegistryUpdateCmd` MUST perform cursor-paginated GET requests to `{source}/v0.1/servers?limit=100&cursor={cursor}` until `nextCursor` is null or empty. | P0 |
| FR-02 | Each page response MUST be committed to SQLite incrementally (not buffered in memory) so that partial scrapes preserve all fetched data on timeout or error. | P0 |
| FR-03 | When `updated_since` is available from `mcp_registry_sync_log`, the scrape MUST append `&updated_since={iso8601}` to the first request URL; this timestamp MUST be the `started_at` of the last successful sync. | P0 |
| FR-04 | HTTP 5xx responses from the registry MUST trigger exponential backoff (base 1s, multiplier 2, max 30s) up to 5 retries before skipping the page and recording the error in `mcp_registry_sync_log.error_pages_json`. | P0 |
| FR-05 | Each `MCPServerEntry` scraped MUST be upserted into `mcp_registry_servers` (keyed on `name + version`); `is_latest` MUST be set from the `_meta.isLatest` field when present, otherwise from position in versions list. | P0 |
| FR-06 | Each `MCPPackage` in the `packages[]` array MUST be upserted into `mcp_registry_packages` with its `registry_type` (`npm`, `pypi`, `docker`), `identifier`, `version`, and `transport_type`. | P0 |
| FR-07 | `tag mcp registry search "<query>"` MUST execute an FTS5 `MATCH` query over `name`, `description`, and `package_identifiers_fts` columns and return results ordered by FTS5 BM25 rank. | P0 |
| FR-08 | When the SQLite cache contains zero rows (no prior `update`), `search` and `list` MUST fall back to the bundled `mcp-registry.yaml` (embedded via `go:embed`) and display a notice prompting the user to run `update`. | P0 |
| FR-09 | `tag mcp registry install <name>` MUST resolve `<name>` against both the SQLite cache (exact and prefix match on `name` column) and the bundled YAML, with cache taking precedence. | P0 |
| FR-10 | `install` MUST select the install method from the `packages[]` array in this priority order: `npm` stdio, `pypi` stdio, `docker`, remote streamable-HTTP URL. SSE-only packages MUST be skipped with a deprecation warning. | P0 |
| FR-11 | For `npm` packages, `install` MUST write an MCP config block with `command: npx` and `args: ["-y", "<identifier>"]`. For `pypi` packages, `command: uvx` and `args: ["<identifier>"]`. For `docker`, `command: docker` with appropriate `run -i --rm <image>` args. For remote URL, `url: <transport_url>` with no `command`. | P0 |
| FR-12 | Before writing the config block, `install` MUST read the active profile's current `mcp_servers` list, sum `tool_count` for all configured servers (from `mcp_registry_servers.tool_count`), and emit a warning if the sum after install would exceed 40. With `--force`, the warning is demoted to informational and the install proceeds. | P1 |
| FR-13 | `install` MUST detect already-configured servers (by `name` in `mcp_servers` list of the profile YAML) and skip them unless `--force` is passed. | P1 |
| FR-14 | `install --dry-run` MUST print the exact YAML config block that would be appended to `lab-config.yaml` but MUST NOT modify any file. | P1 |
| FR-15 | `add-curated` MUST upsert the 10 bundled YAML entries into `mcp_registry_servers` (using `curated: true` flag in `meta_json`) and write their config blocks to the target profile. | P1 |
| FR-16 | `tag mcp registry update --source <url>` MUST accept any URL whose `/v0.1/servers` endpoint returns the same schema. Subregistry entries MUST preserve the raw `_meta` JSON in the `meta_json` column. | P1 |
| FR-17 | `--json` on every subcommand MUST emit valid JSON to stdout and emit all human-readable messages (progress, warnings) to stderr only. This ensures `--json` output is always pipe-safe. | P1 |
| FR-18 | `mcp_registry_sync_log` MUST record: `started_at`, `finished_at`, `source_url`, `servers_synced`, `servers_new`, `servers_updated`, `errors`, `cursor_final`, `duration_ms` for every sync invocation. | P1 |
| FR-19 | `tag doctor` MUST read `mcp_registry_sync_log` and surface a warning if: (a) no sync has ever completed, or (b) the last sync was more than 7 days ago, or (c) the last sync had `errors > 0`. | P2 |
| FR-20 | `tag mcp registry list --installed` MUST join `mcp_registry_servers` with the profile's `mcp_servers` list and show only servers present in both. | P2 |
| FR-21 | `RegistryUpdateCmd` MUST be invocable with `--limit N` to scrape at most N servers total, enabling fast smoke-tests in CI without a full scrape. | P2 |
| FR-22 | `tag mcp registry search` MUST support `--category` filter translated to `WHERE category = ?` on the SQLite query. | P2 |
| FR-23 | The FTS5 content table MUST be rebuilt automatically after any sync that updates more than 100 rows (`INSERT INTO mcp_registry_fts(mcp_registry_fts) VALUES('rebuild')`). | P2 |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Cold sync of 9,652 servers with 100 entries per page (~97 pages) MUST complete in < 5 minutes on a 50 Mbps connection. | < 5 min |
| NFR-02 | Incremental sync when < 200 entries changed MUST complete in < 30 seconds. | < 30 s |
| NFR-03 | FTS5 search over 9,652 entries MUST return p99 results in < 100 ms. | < 100 ms |
| NFR-04 | The scrape loop MUST NOT load more than 50 MB of JSON into process memory at once; pages are parsed and upserted before the next page is fetched. `io.LimitReader` caps each response body at 50 MB. | < 50 MB |
| NFR-05 | All SQLite writes MUST use WAL mode (inherited from `store.Open()`) and MUST batch-upsert within a single transaction per page (100 rows per `BEGIN…COMMIT`). | Per existing pattern |
| NFR-06 | `net/http` (stdlib) is the HTTP client; `cenkalti/backoff/v4` handles retry/backoff at the orchestration level. No new heavy HTTP dependencies may be added. `http.Transport.MaxIdleConnsPerHost`: 10. Request timeout: 30 seconds. | Existing deps only |
| NFR-07 | `tag mcp registry install` MUST NOT execute any `npm install`, `uvx`, or `docker pull` command unless the user has confirmed (or `--force` is passed). Config-only write is the default; actual package resolution happens at agent startup via `npx -y` (which downloads on demand). | No silent side effects |
| NFR-08 | All network errors (DNS failure, TLS error, timeout) MUST be caught, logged to `mcp_registry_sync_log`, and result in a non-zero exit code with a human-readable message. The existing `mcp-registry.yaml` fallback is NEVER overwritten by a failed sync. | Graceful failure |
| NFR-09 | The `_meta` field from subregistries may contain arbitrary JSON and MUST be stored as-is in `meta_json TEXT`. No schema validation is applied to `_meta` contents. | Open schema |
| NFR-10 | `tag mcp registry update` MUST be idempotent: running it twice in a row with no registry changes results in `servers_new=0`, `servers_updated=0`, and the same final database state. | Idempotent |
| NFR-11 | Secret scanning (PRD-034 patterns) MUST be applied to all string values written from `_meta` into `meta_json`; any value matching a secret pattern MUST be redacted to `"[REDACTED]"` and the redaction counted in `sync_log.redacted_count`. | Security |
| NFR-12 | The implementation MUST pass all existing `mcp_registry` Go tests without modification; new behavior is additive behind the `update` subcommand. | Backward compat |

---

## 10. Technical Design

### 10.1 New Packages and Files

| File | Purpose |
|------|---------|
| `internal/mcp/registry/client.go` | `ScrapeRegistry()` pagination loop; `MCPServerEntry`, `MCPPackage`, `SyncResult` structs; `RedactSecrets()`; `inferCategory()`; `inferRequiredEnv()`; `parsePage()` |
| `internal/mcp/registry/db.go` | `UpsertPage()` transactional batch upsert; `RebuildFTS()` trigger; sync log helpers |
| `internal/mcp/registry/search.go` | `SearchServers()` with FTS5 MATCH and LIKE fallback |
| `internal/mcp/registry/install.go` | `BuildConfigBlock()` with npm/pypi/docker/remote priority; SSE rejection |
| `internal/cli/mcp_registry.go` | Cobra subcommand handlers: `runRegistryUpdate`, `runRegistrySearch`, `runRegistryInstall`, `runRegistryList`, `runRegistryAddCurated` |
| `internal/store/migrations/073_mcp_registry.sql` | DDL for `mcp_registry_servers`, `mcp_registry_packages`, `mcp_registry_fts`, `mcp_registry_sync_log` (loaded via `go:embed`) |
| `assets/mcp-registry.yaml` | Bundled 10-entry curated list; embedded via `go:embed` as zero-network fallback |

CLI changes are confined to `internal/cli/mcp_registry.go`:
- `runRegistryUpdate` — new handler wired to `tag mcp registry update`
- `runRegistrySearch` — updated to use SQLite cache when populated
- `runRegistryInstall` — refactored to call `BuildConfigBlock()` and resolve from cache
- `runRegistryList` — updated with YAML fallback when cache empty
- `runRegistryAddCurated` — new handler

### 10.2 SQLite DDL

The following DDL lives in `internal/store/migrations/073_mcp_registry.sql` and is applied by the `internal/store` migration runner on `store.Open()`:

```sql
-- Core server metadata (one row per name+version combination)
CREATE TABLE IF NOT EXISTS mcp_registry_servers (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT NOT NULL,        -- 'io.github.username/server-name'
    version          TEXT NOT NULL,
    description      TEXT NOT NULL DEFAULT '',
    category         TEXT NOT NULL DEFAULT 'other',
    repository_url   TEXT NOT NULL DEFAULT '',
    published_at     TEXT NOT NULL DEFAULT '',
    updated_at       TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'active',  -- 'active' | 'deprecated' | 'deleted'
    is_latest        INTEGER NOT NULL DEFAULT 0,      -- 1 if this is the latest version
    tool_count       INTEGER NOT NULL DEFAULT 0,      -- from packages[].tools[] array length
    transport_type   TEXT NOT NULL DEFAULT 'stdio',   -- 'stdio' | 'streamable-http' | 'sse'
    requires_env     TEXT NOT NULL DEFAULT '[]',      -- JSON array of env var names
    meta_json        TEXT NOT NULL DEFAULT '{}',      -- raw _meta field from registry/subregistry
    source_url       TEXT NOT NULL DEFAULT 'https://registry.modelcontextprotocol.io',
    synced_at        TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(name, version)
);
CREATE INDEX IF NOT EXISTS idx_mrs_name       ON mcp_registry_servers(name);
CREATE INDEX IF NOT EXISTS idx_mrs_category   ON mcp_registry_servers(category);
CREATE INDEX IF NOT EXISTS idx_mrs_is_latest  ON mcp_registry_servers(is_latest);
CREATE INDEX IF NOT EXISTS idx_mrs_updated_at ON mcp_registry_servers(updated_at);

-- Package distribution info (one row per package per server version)
CREATE TABLE IF NOT EXISTS mcp_registry_packages (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    server_name         TEXT NOT NULL,
    server_version      TEXT NOT NULL,
    registry_type       TEXT NOT NULL,   -- 'npm' | 'pypi' | 'docker' | 'remote'
    identifier          TEXT NOT NULL,   -- npm pkg name, pypi name, docker image, or URL
    package_version     TEXT NOT NULL DEFAULT '',
    transport_type      TEXT NOT NULL DEFAULT 'stdio',
    transport_url       TEXT,            -- only for remote/streamable-http packages
    runtime_args        TEXT NOT NULL DEFAULT '[]',  -- JSON array of extra args
    FOREIGN KEY(server_name, server_version)
        REFERENCES mcp_registry_servers(name, version)
        ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_mrp_server ON mcp_registry_packages(server_name, server_version);

-- FTS5 virtual table for search
-- modernc.org/sqlite compiles FTS5 in by default (CGO_ENABLED=0 safe)
CREATE VIRTUAL TABLE IF NOT EXISTS mcp_registry_fts USING fts5(
    name,
    description,
    category,
    package_identifiers,  -- space-separated list of all package identifiers for this server
    content='mcp_registry_servers',
    content_rowid='id',
    tokenize='porter ascii'
);

-- Triggers to keep FTS5 in sync with content table
CREATE TRIGGER IF NOT EXISTS mcp_registry_fts_insert AFTER INSERT ON mcp_registry_servers BEGIN
    INSERT INTO mcp_registry_fts(rowid, name, description, category, package_identifiers)
    VALUES (new.id, new.name, new.description, new.category, '');
END;

CREATE TRIGGER IF NOT EXISTS mcp_registry_fts_update AFTER UPDATE ON mcp_registry_servers BEGIN
    INSERT INTO mcp_registry_fts(mcp_registry_fts, rowid, name, description, category, package_identifiers)
    VALUES ('delete', old.id, old.name, old.description, old.category, '');
    INSERT INTO mcp_registry_fts(rowid, name, description, category, package_identifiers)
    VALUES (new.id, new.name, new.description, new.category, '');
END;

CREATE TRIGGER IF NOT EXISTS mcp_registry_fts_delete AFTER DELETE ON mcp_registry_servers BEGIN
    INSERT INTO mcp_registry_fts(mcp_registry_fts, rowid, name, description, category, package_identifiers)
    VALUES ('delete', old.id, old.name, old.description, old.category, '');
END;

-- Sync audit log
CREATE TABLE IF NOT EXISTS mcp_registry_sync_log (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at       TEXT NOT NULL,
    finished_at      TEXT,
    source_url       TEXT NOT NULL,
    servers_synced   INTEGER NOT NULL DEFAULT 0,
    servers_new      INTEGER NOT NULL DEFAULT 0,
    servers_updated  INTEGER NOT NULL DEFAULT 0,
    errors           INTEGER NOT NULL DEFAULT 0,
    redacted_count   INTEGER NOT NULL DEFAULT 0,
    cursor_final     TEXT,
    duration_ms      INTEGER,
    error_pages_json TEXT NOT NULL DEFAULT '[]',  -- JSON array of {page, cursor, status_code, error}
    status           TEXT NOT NULL DEFAULT 'running'  -- 'running' | 'complete' | 'failed' | 'partial'
);
```

### 10.3 Core Go Structs

```go
// internal/mcp/registry/client.go
package registry

import "regexp"

// MCP protocol version pinned globally.
const MCPProtocolVersion = "2025-11-25"

// DefaultRegistryBase is the canonical public registry URL.
const DefaultRegistryBase = "https://registry.modelcontextprotocol.io"

// secretPatterns are borrowed from internal/obs (PRD-034 secret scanning).
var secretPatterns = []*regexp.Regexp{
    regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|passwd|credential)\s*[:=]\s*\S+`),
    regexp.MustCompile(`(?i)(sk|pk|ak|rk)-[A-Za-z0-9]{20,}`),
}

// MCPPackage describes a single distribution artifact for an MCP server.
type MCPPackage struct {
    RegistryType   string   // "npm" | "pypi" | "docker" | "remote"
    Identifier     string   // npm pkg name, PyPI name, docker image, or remote URL
    PackageVersion string
    TransportType  string   // "stdio" | "streamable-http" | "sse"
    TransportURL   string   // non-empty only for remote packages
    RuntimeArgs    []string
}

// MCPServerEntry represents one server version scraped from the registry.
type MCPServerEntry struct {
    Name          string
    Version       string
    Description   string
    Category      string
    RepositoryURL string
    PublishedAt   string
    UpdatedAt     string
    Status        string // "active" | "deprecated" | "deleted"
    IsLatest      bool
    ToolCount     int
    TransportType string   // primary transport of first non-SSE package
    RequiresEnv   []string // required environment variable names
    Packages      []MCPPackage
    MetaJSON      map[string]any // raw _meta from registry / subregistry (redacted)
}

// SyncResult accumulates progress counters for a single sync run.
type SyncResult struct {
    SourceURL      string
    ServersSynced  int
    ServersNew     int
    ServersUpdated int
    Errors         int
    RedactedCount  int
    CursorFinal    string
    DurationMS     int64
    ErrorPages     []ErrorPage
}

// ErrorPage records a single failed page fetch for the sync audit log.
type ErrorPage struct {
    Page       int    `json:"page"`
    Cursor     string `json:"cursor"`
    StatusCode int    `json:"status_code"`
    ErrText    string `json:"error"`
}

// RedactSecrets scans s for secret-like patterns and replaces matches with
// "[REDACTED]". Returns the sanitised string and the number of redactions made.
func RedactSecrets(s string) (string, int) {
    count := 0
    for _, pat := range secretPatterns {
        if pat.MatchString(s) {
            s = pat.ReplaceAllString(s, "[REDACTED]")
            count++
        }
    }
    return s, count
}
```

### 10.4 HTTP Pagination Loop

```go
// internal/mcp/registry/client.go (continued)

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "strings"
    "time"

    "github.com/cenkalti/backoff/v4"
)

const (
    pageSize    = 100
    maxRetries  = 5
    httpTimeout = 30 * time.Second
    maxBodySize = 50 << 20 // 50 MB per NFR-04
)

var httpClient = &http.Client{
    Timeout: httpTimeout,
    Transport: &http.Transport{MaxIdleConnsPerHost: 10},
}

// PageCallback is invoked after each page is parsed; used for incremental DB writes.
type PageCallback func(pageNum int, entries []MCPServerEntry)

// ScrapeRegistry performs a cursor-paginated scrape of an MCP registry endpoint.
// source must be https://. updatedSince is RFC3339 or empty for a full scrape.
// limit=0 means unlimited. cb is called per-page (before the next fetch begins).
func ScrapeRegistry(ctx context.Context, source, updatedSince string, limit int, cb PageCallback) (*SyncResult, error) {
    if !strings.HasPrefix(source, "https://") {
        return nil, fmt.Errorf("registry source must use https, got: %s", source)
    }

    result := &SyncResult{SourceURL: source}
    start := time.Now()
    cursor := ""
    pageNum := 0

    for {
        params := url.Values{"limit": {fmt.Sprintf("%d", pageSize)}}
        if cursor != "" {
            params.Set("cursor", cursor)
        }
        if updatedSince != "" {
            params.Set("updated_since", updatedSince)
        }
        endpoint := source + "/v0.1/servers?" + params.Encode()

        var rawBody []byte
        op := func() error {
            req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
            if err != nil {
                return backoff.Permanent(err)
            }
            resp, err := httpClient.Do(req)
            if err != nil {
                return err // transient — backoff will retry
            }
            defer resp.Body.Close()

            // Respect Retry-After on 429
            if resp.StatusCode == http.StatusTooManyRequests {
                if ra := resp.Header.Get("Retry-After"); ra != "" {
                    if d, err := time.ParseDuration(ra + "s"); err == nil {
                        time.Sleep(d)
                    }
                }
                return fmt.Errorf("HTTP 429 — rate limited")
            }
            if resp.StatusCode >= 500 {
                result.ErrorPages = append(result.ErrorPages, ErrorPage{
                    Page: pageNum, Cursor: cursor, StatusCode: resp.StatusCode,
                })
                return fmt.Errorf("HTTP %d", resp.StatusCode) // transient — retry
            }
            if resp.StatusCode != http.StatusOK {
                return backoff.Permanent(fmt.Errorf("unexpected HTTP %d", resp.StatusCode))
            }
            rawBody, err = io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
            return err
        }

        bo := backoff.WithContext(
            backoff.WithMaxRetries(
                backoff.NewExponentialBackOff(
                    backoff.WithInitialInterval(time.Second),
                    backoff.WithMultiplier(2),
                    backoff.WithMaxInterval(30*time.Second),
                ),
                maxRetries,
            ),
            ctx,
        )

        if err := backoff.Retry(op, bo); err != nil {
            result.Errors++
            break // exhausted retries — stop scrape gracefully
        }

        var page struct {
            Servers    []json.RawMessage `json:"servers"`
            NextCursor string            `json:"nextCursor"`
        }
        if err := json.Unmarshal(rawBody, &page); err != nil {
            result.Errors++
            break
        }

        entries := parsePage(page.Servers, source, result)
        pageNum++
        if cb != nil {
            cb(pageNum, entries)
        }
        result.ServersSynced += len(entries)

        if limit > 0 && result.ServersSynced >= limit {
            result.CursorFinal = page.NextCursor
            break
        }
        if page.NextCursor == "" {
            break
        }
        cursor = page.NextCursor
    }

    result.DurationMS = time.Since(start).Milliseconds()
    return result, nil
}

func parsePage(rawServers []json.RawMessage, sourceURL string, result *SyncResult) []MCPServerEntry {
    entries := make([]MCPServerEntry, 0, len(rawServers))
    for _, raw := range rawServers {
        var m map[string]any
        if err := json.Unmarshal(raw, &m); err != nil {
            result.Errors++
            continue
        }

        // Redact _meta before storage
        metaStr, _ := json.Marshal(m["_meta"])
        redacted, n := RedactSecrets(string(metaStr))
        result.RedactedCount += n
        var meta map[string]any
        _ = json.Unmarshal([]byte(redacted), &meta)
        if meta == nil {
            meta = map[string]any{}
        }

        var pkgs []MCPPackage
        if rawPkgs, ok := m["packages"].([]any); ok {
            for _, rp := range rawPkgs {
                if p, ok := rp.(map[string]any); ok {
                    runtime, _ := p["runtime"].(map[string]any)
                    if runtime == nil {
                        runtime = map[string]any{}
                    }
                    var args []string
                    if a, ok := runtime["args"].([]any); ok {
                        for _, v := range a {
                            if s, ok := v.(string); ok {
                                args = append(args, s)
                            }
                        }
                    }
                    pkgs = append(pkgs, MCPPackage{
                        RegistryType:   strVal(p, "registryType", "npm"),
                        Identifier:     strVal(p, "name", ""),
                        PackageVersion: strVal(p, "version", ""),
                        TransportType:  strVal(runtime, "transport", "stdio"),
                        TransportURL:   strVal(runtime, "url", ""),
                        RuntimeArgs:    args,
                    })
                }
            }
        }

        toolCount := 0
        if rawPkgs, ok := m["packages"].([]any); ok {
            for _, rp := range rawPkgs {
                if p, ok := rp.(map[string]any); ok {
                    if tools, ok := p["tools"].([]any); ok {
                        toolCount += len(tools)
                    }
                }
            }
        }

        primaryTransport := "stdio"
        if len(pkgs) > 0 {
            primaryTransport = pkgs[0].TransportType
        }

        e := MCPServerEntry{
            Name:          strVal(m, "name", ""),
            Version:       strVal(m, "version", ""),
            Description:   strVal(m, "description", ""),
            Category:      inferCategory(m),
            RepositoryURL: repoURL(m),
            PublishedAt:   strVal(m, "publishedAt", ""),
            UpdatedAt:     strVal(m, "updatedAt", ""),
            Status:        strVal(meta, "status", "active"),
            IsLatest:      boolVal(meta, "isLatest"),
            ToolCount:     toolCount,
            TransportType: primaryTransport,
            RequiresEnv:   inferRequiredEnv(m),
            Packages:      pkgs,
            MetaJSON:      meta,
        }
        entries = append(entries, e)
    }
    return entries
}

func inferCategory(m map[string]any) string {
    tags := lowerSlice(m, "tags")
    desc := strings.ToLower(strVal(m, "description", ""))
    name := strings.ToLower(strVal(m, "name", ""))
    kv := [][2]string{
        {"filesystem", "filesystem"}, {"file", "filesystem"},
        {"database", "database"}, {"postgres", "database"}, {"sqlite", "database"},
        {"github", "vcs"}, {"gitlab", "vcs"}, {"git", "vcs"},
        {"browser", "web"}, {"playwright", "web"}, {"puppeteer", "web"},
        {"fetch", "web"}, {"search", "web"},
        {"slack", "messaging"}, {"email", "messaging"}, {"gmail", "messaging"},
        {"calendar", "productivity"}, {"notion", "productivity"}, {"linear", "productivity"},
        {"memory", "memory"}, {"vector", "ml"}, {"embedding", "ml"},
        {"reasoning", "reasoning"}, {"thinking", "reasoning"},
        {"docker", "devops"}, {"kubernetes", "devops"},
    }
    for _, pair := range kv {
        kw, cat := pair[0], pair[1]
        for _, t := range tags {
            if t == kw {
                return cat
            }
        }
        if strings.Contains(desc, kw) || strings.Contains(name, kw) {
            return cat
        }
    }
    return "other"
}

func inferRequiredEnv(m map[string]any) []string {
    var envVars []string
    if pkgs, ok := m["packages"].([]any); ok {
        for _, rp := range pkgs {
            if p, ok := rp.(map[string]any); ok {
                if envs, ok := p["environment"].([]any); ok {
                    for _, re := range envs {
                        if e, ok := re.(map[string]any); ok {
                            if req, _ := e["required"].(bool); req {
                                if n := strVal(e, "name", ""); n != "" {
                                    envVars = append(envVars, n)
                                }
                            }
                        }
                    }
                }
            }
        }
    }
    return envVars
}
```

### 10.5 SQLite Upsert Batch

```go
// internal/mcp/registry/db.go
package registry

import (
    "context"
    "database/sql"
    "encoding/json"
    "time"
)

// UpsertPage upserts one page of entries inside a single BEGIN…COMMIT transaction.
// modernc.org/sqlite WAL mode; single-writer via store.Open()'s gofrs/flock.
// Returns (newCount, updatedCount, error).
func UpsertPage(ctx context.Context, db *sql.DB, entries []MCPServerEntry, sourceURL string) (int, int, error) {
    tx, err := db.BeginTx(ctx, nil)
    if err != nil {
        return 0, 0, err
    }
    defer tx.Rollback() //nolint:errcheck

    newCount, updatedCount := 0, 0
    now := time.Now().UTC().Format(time.RFC3339)

    for _, e := range entries {
        // Validate server name (FR security guard)
        if len(e.Name) > 256 {
            continue
        }

        var existingUpdatedAt string
        err := tx.QueryRowContext(ctx,
            `SELECT updated_at FROM mcp_registry_servers WHERE name=? AND version=?`,
            e.Name, e.Version,
        ).Scan(&existingUpdatedAt)

        reqEnvJSON, _ := json.Marshal(e.RequiresEnv)
        metaJSON, _ := json.Marshal(e.MetaJSON)
        isLatestInt := 0
        if e.IsLatest {
            isLatestInt = 1
        }

        switch {
        case err == sql.ErrNoRows:
            _, err = tx.ExecContext(ctx, `
                INSERT INTO mcp_registry_servers
                  (name, version, description, category, repository_url,
                   published_at, updated_at, status, is_latest, tool_count,
                   transport_type, requires_env, meta_json, source_url, synced_at)
                VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
                e.Name, e.Version, e.Description, e.Category, e.RepositoryURL,
                e.PublishedAt, e.UpdatedAt, e.Status, isLatestInt, e.ToolCount,
                e.TransportType, string(reqEnvJSON), string(metaJSON), sourceURL, now,
            )
            if err == nil {
                newCount++
            }
        case err == nil && existingUpdatedAt != e.UpdatedAt:
            _, err = tx.ExecContext(ctx, `
                UPDATE mcp_registry_servers
                SET description=?, category=?, repository_url=?, published_at=?,
                    updated_at=?, status=?, is_latest=?, tool_count=?,
                    transport_type=?, requires_env=?, meta_json=?, source_url=?, synced_at=?
                WHERE name=? AND version=?`,
                e.Description, e.Category, e.RepositoryURL, e.PublishedAt,
                e.UpdatedAt, e.Status, isLatestInt, e.ToolCount,
                e.TransportType, string(reqEnvJSON), string(metaJSON), sourceURL, now,
                e.Name, e.Version,
            )
            if err == nil {
                updatedCount++
            }
        }
        if err != nil {
            return newCount, updatedCount, err
        }

        // Refresh packages: delete-and-reinsert within the same transaction
        if _, err = tx.ExecContext(ctx,
            `DELETE FROM mcp_registry_packages WHERE server_name=? AND server_version=?`,
            e.Name, e.Version,
        ); err != nil {
            return newCount, updatedCount, err
        }
        for _, pkg := range e.Packages {
            argsJSON, _ := json.Marshal(pkg.RuntimeArgs)
            if _, err = tx.ExecContext(ctx, `
                INSERT INTO mcp_registry_packages
                  (server_name, server_version, registry_type, identifier,
                   package_version, transport_type, transport_url, runtime_args)
                VALUES (?,?,?,?,?,?,?,?)`,
                e.Name, e.Version, pkg.RegistryType, pkg.Identifier,
                pkg.PackageVersion, pkg.TransportType, pkg.TransportURL, string(argsJSON),
            ); err != nil {
                return newCount, updatedCount, err
            }
        }
    }

    return newCount, updatedCount, tx.Commit()
}

// RebuildFTS triggers a full FTS5 content-table rebuild.
// Called when a sync updates more than 100 rows (FR-23).
func RebuildFTS(ctx context.Context, db *sql.DB) error {
    _, err := db.ExecContext(ctx, `INSERT INTO mcp_registry_fts(mcp_registry_fts) VALUES('rebuild')`)
    return err
}
```

### 10.6 Install Method Abstraction

```go
// internal/mcp/registry/install.go
package registry

import "sort"

var installPriority = map[string]int{
    "npm":    0,
    "pypi":   1,
    "docker": 2,
    "remote": 3,
}

// ConfigBlock is the MCP server stanza written to lab-config.yaml via gopkg.in/yaml.v3.
type ConfigBlock struct {
    Name      string   `yaml:"name"`
    Command   string   `yaml:"command,omitempty"`
    Args      []string `yaml:"args,omitempty"`
    URL       string   `yaml:"url,omitempty"`
    Transport string   `yaml:"transport"`
}

// BuildConfigBlock selects the best install method from registry packages.
// Priority: npm > pypi > docker > remote (streamable-http). SSE is rejected.
// Returns nil if no supported package is found.
func BuildConfigBlock(serverName string, pkgs []MCPPackage) *ConfigBlock {
    sorted := make([]MCPPackage, len(pkgs))
    copy(sorted, pkgs)
    sort.Slice(sorted, func(i, j int) bool {
        pi, pj := 99, 99
        if v, ok := installPriority[sorted[i].RegistryType]; ok {
            pi = v
        }
        if v, ok := installPriority[sorted[j].RegistryType]; ok {
            pj = v
        }
        return pi < pj
    })

    for _, pkg := range sorted {
        if pkg.TransportType == "sse" {
            continue // SSE transport is deprecated; skip with warning at call site
        }
        switch pkg.RegistryType {
        case "npm":
            return &ConfigBlock{
                Name:      serverName,
                Command:   "npx",
                Args:      []string{"-y", pkg.Identifier},
                Transport: "stdio",
            }
        case "pypi":
            return &ConfigBlock{
                Name:      serverName,
                Command:   "uvx",
                Args:      []string{pkg.Identifier},
                Transport: "stdio",
            }
        case "docker":
            return &ConfigBlock{
                Name:      serverName,
                Command:   "docker",
                Args:      []string{"run", "-i", "--rm", pkg.Identifier},
                Transport: "stdio",
            }
        case "remote":
            if pkg.TransportURL != "" {
                return &ConfigBlock{
                    Name:      serverName,
                    URL:       pkg.TransportURL,
                    Transport: "streamable-http",
                }
            }
        }
    }
    return nil
}
```

### 10.7 FTS5 Search Query

```go
// internal/mcp/registry/search.go
package registry

import (
    "context"
    "database/sql"
    "fmt"
    "strings"
)

// SearchResult is one row returned from a registry search.
type SearchResult struct {
    Name          string `json:"name"`
    Version       string `json:"version"`
    Description   string `json:"description"`
    Category      string `json:"category"`
    TransportType string `json:"transport_type"`
    ToolCount     int    `json:"tool_count"`
    RequiresEnv   string `json:"requires_env"`   // raw JSON array string
    RepositoryURL string `json:"repository_url"`
    MetaJSON      string `json:"_meta"`
}

// SearchServers executes an FTS5 MATCH search with optional category/transport filters.
// Falls back to a LIKE query if the FTS5 expression is malformed.
func SearchServers(ctx context.Context, db *sql.DB, query, category, transport string, limit int) ([]SearchResult, error) {
    filters := "WHERE s.is_latest = 1"
    baseArgs := []any{}
    if category != "" {
        filters += " AND s.category = ?"
        baseArgs = append(baseArgs, category)
    }
    if transport != "" {
        filters += " AND s.transport_type = ?"
        baseArgs = append(baseArgs, transport)
    }

    ftsQ := strings.ReplaceAll(query, `"`, `""`)
    sqlFTS := fmt.Sprintf(`
        SELECT s.name, s.version, s.description, s.category, s.transport_type,
               s.tool_count, s.requires_env, s.repository_url, s.meta_json
        FROM mcp_registry_fts AS f
        JOIN mcp_registry_servers AS s ON s.id = f.rowid
        %s AND f.mcp_registry_fts MATCH ?
        ORDER BY bm25(mcp_registry_fts) ASC
        LIMIT ?`, filters)

    rows, err := db.QueryContext(ctx, sqlFTS, append(baseArgs, ftsQ, limit)...)
    if err != nil {
        // FTS5 parse error — fall back to LIKE
        like := "%" + query + "%"
        sqlLIKE := fmt.Sprintf(`
            SELECT s.name, s.version, s.description, s.category, s.transport_type,
                   s.tool_count, s.requires_env, s.repository_url, s.meta_json
            FROM mcp_registry_servers AS s
            %s AND (s.name LIKE ? OR s.description LIKE ?)
            ORDER BY s.updated_at DESC
            LIMIT ?`, filters)
        rows, err = db.QueryContext(ctx, sqlLIKE, append(baseArgs, like, like, limit)...)
        if err != nil {
            return nil, err
        }
    }
    defer rows.Close()

    var results []SearchResult
    for rows.Next() {
        var r SearchResult
        if err := rows.Scan(&r.Name, &r.Version, &r.Description, &r.Category,
            &r.TransportType, &r.ToolCount, &r.RequiresEnv, &r.RepositoryURL, &r.MetaJSON); err != nil {
            return nil, err
        }
        results = append(results, r)
    }
    return results, rows.Err()
}
```

### 10.8 `updated_since` Incremental Sync and `RegistryUpdateCmd`

```go
// internal/cli/mcp_registry.go (excerpt — update subcommand handler)

func runRegistryUpdate(cmd *cobra.Command, _ []string) error {
    source, _   := cmd.Flags().GetString("source")
    limit, _    := cmd.Flags().GetInt("limit")
    dryRun, _   := cmd.Flags().GetBool("dry-run")
    emitJSON, _ := cmd.Flags().GetBool("json")

    db, err := store.Open(cfg.DBPath) // internal/store; modernc.org/sqlite, WAL, gofrs/flock
    if err != nil {
        return err
    }
    defer db.Close()

    // Determine updated_since from the last successful sync for this source URL.
    var updatedSince string
    _ = db.QueryRowContext(cmd.Context(), `
        SELECT started_at FROM mcp_registry_sync_log
        WHERE status='complete' AND source_url=?
        ORDER BY finished_at DESC LIMIT 1`, source,
    ).Scan(&updatedSince)

    if !emitJSON {
        mode := "full"
        if updatedSince != "" {
            mode = fmt.Sprintf("incremental (since %s)", updatedSince)
        }
        fmt.Fprintf(os.Stderr, "Syncing MCP registry [%s] from %s ...\n", mode, source)
    }

    // Insert sync log row with status=running; update at completion.
    var syncLogID int64
    _ = db.QueryRowContext(cmd.Context(), `
        INSERT INTO mcp_registry_sync_log (started_at, source_url, status)
        VALUES (?,?,'running') RETURNING id`,
        time.Now().UTC().Format(time.RFC3339), source,
    ).Scan(&syncLogID)

    totalNew, totalUpdated := 0, 0
    cb := func(pageNum int, entries []registry.MCPServerEntry) {
        if !dryRun {
            n, u, _ := registry.UpsertPage(cmd.Context(), db, entries, source)
            totalNew += n
            totalUpdated += u
        }
        if !emitJSON {
            fmt.Fprintf(os.Stderr, "  Page %3d: %3d servers\r", pageNum, len(entries))
        }
    }

    result, err := registry.ScrapeRegistry(cmd.Context(), source, updatedSince, limit, cb)
    if err != nil {
        return err
    }

    // Rebuild FTS5 index after significant changes (FR-23).
    if !dryRun && (totalNew+totalUpdated) > 100 {
        _ = registry.RebuildFTS(cmd.Context(), db)
    }

    // Finalise sync log.
    status := "complete"
    if result.Errors > 0 {
        if result.ServersSynced > 0 {
            status = "partial"
        } else {
            status = "failed"
        }
    }
    errPagesJSON, _ := json.Marshal(result.ErrorPages)
    _, _ = db.ExecContext(cmd.Context(), `
        UPDATE mcp_registry_sync_log
        SET finished_at=?, servers_synced=?, servers_new=?, servers_updated=?,
            errors=?, redacted_count=?, cursor_final=?, duration_ms=?,
            error_pages_json=?, status=?
        WHERE id=?`,
        time.Now().UTC().Format(time.RFC3339),
        result.ServersSynced, totalNew, totalUpdated,
        result.Errors, result.RedactedCount, result.CursorFinal, result.DurationMS,
        string(errPagesJSON), status,
        syncLogID,
    )

    if emitJSON {
        enc := json.NewEncoder(os.Stdout)
        enc.SetIndent("", "  ")
        _ = enc.Encode(map[string]any{
            "source":             source,
            "synced_at":         time.Now().UTC().Format(time.RFC3339),
            "servers_total":     result.ServersSynced,
            "servers_new":       totalNew,
            "servers_updated":   totalUpdated,
            "servers_unchanged": result.ServersSynced - totalNew - totalUpdated,
            "errors":            result.Errors,
            "duration_ms":       result.DurationMS,
            "cursor_final":      result.CursorFinal,
        })
    } else {
        fmt.Fprintf(os.Stderr, "\n") // clear \r line
        dryTag := ""
        if dryRun {
            dryTag = "(dry-run) "
        }
        fmt.Printf("\nRegistry sync %scomplete.\n", dryTag)
        fmt.Printf("  Servers synced: %d\n", result.ServersSynced)
        fmt.Printf("  New:            %d\n", totalNew)
        fmt.Printf("  Updated:        %d\n", totalUpdated)
        fmt.Printf("  Unchanged:      %d\n", result.ServersSynced-totalNew-totalUpdated)
        fmt.Printf("  Errors:         %d\n", result.Errors)
        fmt.Printf("  Duration:       %.1fs\n", float64(result.DurationMS)/1000)
        if result.Errors > 0 {
            fmt.Fprintf(os.Stderr, "\n  Warning: %d page(s) failed. See mcp_registry_sync_log for details.\n", result.Errors)
        }
    }

    if result.Errors > 0 {
        return fmt.Errorf("%d sync error(s) — see mcp_registry_sync_log", result.Errors)
    }
    return nil
}
```

### 10.9 Integration with `tag doctor`

The existing `runDoctor` function in `internal/cli/doctor.go` receives a new check block:

```go
// internal/cli/doctor.go (excerpt — mcp_registry check)

var lastSync struct {
    FinishedAt string
    Status     string
    Errors     int
}
err := db.QueryRowContext(ctx,
    `SELECT finished_at, status, errors FROM mcp_registry_sync_log ORDER BY finished_at DESC LIMIT 1`,
).Scan(&lastSync.FinishedAt, &lastSync.Status, &lastSync.Errors)

if errors.Is(err, sql.ErrNoRows) {
    doctorWarn("mcp_registry", "No MCP registry sync has been performed. Run: tag mcp registry update")
} else if err == nil {
    t, _ := time.Parse(time.RFC3339, lastSync.FinishedAt)
    ageDays := int(time.Since(t).Hours() / 24)
    switch {
    case ageDays > 7:
        doctorWarn("mcp_registry", fmt.Sprintf("MCP registry cache is %d days old. Run: tag mcp registry update", ageDays))
    case lastSync.Errors > 0:
        doctorWarn("mcp_registry", fmt.Sprintf("Last sync had %d error(s). Run: tag mcp registry update", lastSync.Errors))
    case lastSync.Status == "complete":
        doctorOK("mcp_registry", fmt.Sprintf("Registry cache fresh (%dd old)", ageDays))
    }
}
```

### 10.10 Cobra Flag Registration

```go
// internal/cli/mcp_registry.go — init() Cobra registration

func init() {
    updateCmd := &cobra.Command{
        Use:   "update",
        Short: "Sync MCP registry from modelcontextprotocol.io",
        RunE:  runRegistryUpdate,
    }
    updateCmd.Flags().String("source", registry.DefaultRegistryBase,
        "Registry base URL (must be https://)")
    updateCmd.Flags().Int("limit", 0,
        "Stop after N servers (0 = unlimited; smoke-test mode for CI)")
    updateCmd.Flags().Bool("dry-run", false,
        "Fetch pages but do not write to SQLite")
    updateCmd.Flags().Bool("json", false,
        "Emit sync summary as JSON to stdout; all progress to stderr")

    mcpRegistryCmd.AddCommand(updateCmd)
}
```

---

## 11. Security Considerations

1. **Secret redaction in `_meta`**: The `_meta` field from subregistries is arbitrary third-party JSON. Before storing, `RedactSecrets()` in `internal/mcp/registry/client.go` scans all string values for patterns matching API keys, tokens, and passwords. Any match is replaced with `"[REDACTED]"` and counted in `mcp_registry_sync_log.redacted_count`. This prevents a malicious subregistry from embedding credentials that would then be read by agents.

2. **No credential capture in install config**: `BuildConfigBlock()` never writes API key values into `lab-config.yaml`. Required environment variable *names* (e.g., `NOTION_API_KEY`) are recorded in `requires_env`, but values come exclusively from the user's shell environment at agent startup. This matches the existing pattern in the bundled YAML.

3. **Source URL validation**: `--source` is validated to begin with `https://` inside `ScrapeRegistry()`. HTTP (non-TLS) registry sources are rejected with an error before any network call. This prevents man-in-the-middle injection of malicious server entries over plaintext connections.

4. **No automatic execution of registry-specified commands**: `tag mcp registry install` writes a config block but does NOT execute `npx`, `uvx`, or `docker` commands. The agent runtime is responsible for spawning MCP servers at session start. This means a malicious registry entry cannot trigger arbitrary command execution via `tag mcp registry install`.

5. **Path traversal in profile directory**: The target profile path is validated through the existing `SafeProfilePath()` helper in `internal/config` before any file write, preventing a crafted server `name` field containing `../../etc/passwd` patterns from escaping the profile directory.

6. **Server name allowlist check**: `server_name` values longer than 256 characters or containing characters outside `[a-zA-Z0-9._/:-]` are rejected with a warning and not upserted into the database. This protects against SQLite injection via crafted server names in FTS5 queries.

7. **Rate limiting respect**: The scrape loop checks for `Retry-After` headers on 429 responses and sleeps for the indicated duration before retrying. This prevents TAG from being blocked by the registry's rate limiter.

8. **WAL isolation**: All registry reads during `tag mcp registry search` use a `BEGIN DEFERRED` transaction to ensure a consistent snapshot even if a concurrent `update` is running. This is automatically handled by `modernc.org/sqlite`'s WAL mode and the `store.Open()` configuration.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/mcp/registry/registry_test.go`)

Uses `github.com/stretchr/testify/assert` + `github.com/stretchr/testify/require` + `net/http/httptest` for server mocking. All DB tests use an in-memory `modernc.org/sqlite` instance (`file::memory:?cache=shared`).

| Test | Description |
|------|-------------|
| `TestScrapeEmptyRegistry` | `httptest.NewServer` returns `{"servers":[],"nextCursor":null}`; assert `SyncResult.ServersSynced == 0` and no DB rows inserted |
| `TestScrapeSinglePage` | Mock one page of 5 entries; assert all 5 upserted to `mcp_registry_servers` |
| `TestScrapePagination` | Mock 3 pages with cursors; assert all 300 entries upserted; `CursorFinal` is empty |
| `TestScrape500Retry` | Mock 3 consecutive 500s then a 200; assert `ErrorPages` len; entries from 200 page upserted |
| `TestScrapeExhaustedRetries` | Mock 5 consecutive 500s; assert `SyncResult.Errors == 1`; graceful return (no panic) |
| `TestIncrementalSync` | Pre-insert a sync log row with `started_at`; run scrape; assert `updated_since` query param present in captured request URL |
| `TestSecretRedaction` | Entry with `_meta: {"apiKey": "sk-secret123"}`; assert stored `meta_json` contains `[REDACTED]`; `RedactedCount == 1` |
| `TestFTS5Search` | Insert 50 mock entries via `UpsertPage`; call `SearchServers("calendar scheduling",...)`; assert top result is a calendar server |
| `TestFTS5Fallback` | Drop FTS5 table; `SearchServers` should fall back to LIKE without returning an error |
| `TestBuildNpmConfigBlock` | npm package entry → `ConfigBlock{Command:"npx", Args:["-y","<id>"]}` |
| `TestBuildPypiConfigBlock` | pypi package entry → `ConfigBlock{Command:"uvx", Args:["<id>"]}` |
| `TestBuildDockerConfigBlock` | docker package entry → `ConfigBlock{Command:"docker", Args:["run","-i","--rm","<image>"]}` |
| `TestBuildRemoteConfigBlock` | remote package → `ConfigBlock{URL:"<url>", Transport:"streamable-http"}` |
| `TestSSEPackageSkipped` | SSE-only package → `BuildConfigBlock` returns `nil` |
| `TestToolBudgetWarning` | Mock profile with 35 tools configured; install a 10-tool server; assert warning written to stderr |
| `TestToolBudgetForce` | Same as above with `--force`; assert install proceeds without non-zero exit |
| `TestInstallAlreadyConfigured` | Install a server already in profile YAML; assert "skipped" in output; `lab-config.yaml` mtime unchanged |
| `TestDryRunNoFileWrite` | `--dry-run` flag; assert no `mcp_registry_servers` rows inserted and no `lab-config.yaml` modification |
| `TestSourceURLHTTPRejected` | `--source http://evil.com`; assert `ScrapeRegistry` returns error immediately; no HTTP calls made |
| `TestServerNameLengthLimit` | Entry with 300-char name; assert not upserted; warning recorded in result |
| `TestIdempotentUpsert` | Run `UpsertPage` twice with same entries; assert row count unchanged; `updatedCount == 0` |
| `TestYAMLFallbackWhenEmpty` | Call `SearchServers` with empty SQLite table; caller detects zero rows and loads bundled YAML via `go:embed` |
| `TestAddCuratedIdempotent` | Run `runRegistryAddCurated` twice; assert second run shows all 10 as "already configured" |

### 12.2 Integration Tests (`internal/mcp/registry/integration_test.go`)

Guard with `//go:build integration` and skip when `SKIP_NETWORK_TESTS=1`.

| Test | Description |
|------|-------------|
| `TestLiveRegistryFirstPage` | Fetch one page (`--limit 10`) from live `registry.modelcontextprotocol.io`; assert `>= 10` entries |
| `TestFullSyncAndSearch` | Full sync (`--limit 50`); then `SearchServers("github",...)`; assert top result contains "github" in name |
| `TestInstallIntoTempProfile` | Create `t.TempDir()` profile dir; `runRegistryInstall(["mcp-filesystem"],...)`; assert `lab-config.yaml` has correct `npx` block |
| `TestDoctorStaleRegistryWarning` | Insert a sync log row 8 days old; run doctor check; assert warning text contains "days old" |

### 12.3 Performance Tests (`internal/mcp/registry/bench_test.go`)

| Benchmark | Target |
|-----------|--------|
| `BenchmarkFTS5Search` | Search over 10,000 inserted mock entries; p99 < 100 ms across 20 representative queries |
| `BenchmarkUpsertThroughput` | `UpsertPage` with 100-entry batches × 97 iterations (9,700 entries) in < 60 s on in-memory SQLite WAL |
| `BenchmarkMemoryCeiling` | Scrape loop with 100-entry mock pages; assert no single `cb` invocation holds > 50 MB of parsed JSON (checked via `runtime.ReadMemStats`) |

Golden-transcript fixtures for CLI output are stored under `testdata/golden/mcp_registry_*.txt` and compared via `testify` `assert.Equal` after `go test -update` flag regeneration.

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag mcp registry update` exits 0 and prints sync summary after fetching >= 100 servers from the live registry. | Manual run + `echo $?` |
| AC-02 | After `update`, `tag mcp registry list` shows >= 100 entries (not just the 10 bundled). | `tag mcp registry list --json \| jq length` >= 100 |
| AC-03 | `tag mcp registry search "github"` returns a result with "github" in the `name` field as the first result. | Automated assertion |
| AC-04 | `tag mcp registry search "nonexistent query xyz123"` returns 0 results without crashing. | Automated assertion |
| AC-05 | `tag mcp registry install mcp-github --profile test_profile --dry-run` prints a YAML block with `command: npx` and does not modify any file. | File mtime assertion |
| AC-06 | `tag mcp registry install mcp-github --profile test_profile` writes a valid YAML block to the profile's `lab-config.yaml` with `command: npx` and `-y @modelcontextprotocol/server-github` in `args`. | File content assertion |
| AC-07 | Running `update` twice produces identical DB row counts on the second run (`servers_new=0`, `servers_updated=0` for unchanged data). | Automated SQL assertion |
| AC-08 | `tag mcp registry update --source http://evil.com` exits 1 with "https required" message and writes no rows to `mcp_registry_servers`. | Automated assertion |
| AC-09 | `tag mcp registry update --limit 5` completes after fetching exactly 5 servers and writes 5 rows to `mcp_registry_servers`. | `SELECT COUNT(*) FROM mcp_registry_servers` == 5 |
| AC-10 | `tag mcp registry list` with no prior `update` (empty `mcp_registry_servers`) returns the 10 bundled YAML entries and prints the "run update" notice. | Automated assertion on fresh DB |
| AC-11 | `tag mcp registry install playwright-mcp --profile test_profile` where profile already has 36 tools configured produces a tool budget warning. | Stderr content assertion |
| AC-12 | `tag doctor` includes an "mcp_registry" check and shows a warning when `mcp_registry_sync_log` is empty. | `tag doctor --json` contains `mcp_registry` key |
| AC-13 | `tag mcp registry search "calendar" --json` produces valid JSON parseable by `jq` with all required keys present. | `jq -e .[0].name` succeeds |
| AC-14 | An entry with `_meta: {"token": "sk-abc123"}` in the registry response is stored with `[REDACTED]` in `meta_json` and does not expose the token in any CLI output. | DB inspection + output grep |
| AC-15 | `tag mcp registry install <pypi_server>` produces a config block with `command: uvx` (not `npx`). | Config file content assertion |
| AC-16 | `tag mcp registry add-curated` writes all 10 bundled servers to the target profile and is idempotent on second run. | Config file assertion + second-run "skipped" count |
| AC-17 | `tag mcp registry update --json` emits valid JSON to stdout and all progress messages to stderr. | `stdout \| jq -e .servers_total` succeeds; stderr has progress text |
| AC-18 | HTTP 500 from registry triggers retry with exponential backoff; after 5 failures the scrape moves to the next page rather than crashing. | `httptest.Server` all-500 page + assertion on subsequent page processing |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| `net/http` | Go stdlib | HTTP client for registry scraping. `http.Transport{MaxIdleConnsPerHost:10}`, 30 s timeout per NFR-06. |
| `cenkalti/backoff/v4` | Go module | Exponential backoff for HTTP 5xx retry (`WithMaxRetries(5)`, base 1 s, max 30 s). Orchestration-level only — not embedded in tight loops. |
| `modernc.org/sqlite` | Go module | Pure-Go SQLite driver (`CGO_ENABLED=0`). FTS5, RTree, JSON1 compiled in by default. WAL mode via `store.Open()`. No native library required. |
| `gopkg.in/yaml.v3` | Go module | Already in TAG; reads bundled `mcp-registry.yaml` fallback and writes `lab-config.yaml` profile blocks. |
| `encoding/json` | Go stdlib | `_meta` storage and `--json` output. |
| `github.com/gofrs/flock` | Go module | Already in TAG via `internal/store`; provides cross-platform file locking for single-writer SQLite guarantee. |
| `go:embed` | Go stdlib directive | Embeds `assets/mcp-registry.yaml` (bundled curated list) and `internal/store/migrations/*.sql` into the binary at compile time. Zero-network fallback requires no runtime file access. |
| `PRD-014` (MCP Server Registry) | Internal PRD | This PRD extends the `mcpRegistryCmd` Cobra dispatcher and `loadMCPRegistry()` helpers defined in PRD-014. |
| `PRD-034` (Secret Scanning) | Internal PRD | `RedactSecrets()` borrows compiled `*regexp.Regexp` patterns from `internal/obs` secret scanner. Direct import — no pattern duplication. |
| `PRD-043` (Tool Retrieval) | Internal PRD | After `update`, `tag tool index` will pick up 9,652+ servers if the FTS5 cache is wired to `mcp_registry_servers`. Expose `mcp_registry_servers` as an additional source in `internal/mcp`'s tool index builder. |
| `PRD-013` (Agent Tracing) | Internal PRD | Sync log entries in `mcp_registry_sync_log` appear in `tag doctor` alongside other health checks. |
| `PRD-009` (Enhanced Doctor Diagnostics) | Internal PRD | `tag doctor` receives a new `mcp_registry` check block in `internal/cli/doctor.go`. |
| `modelcontextprotocol.io` | External service | Registry v0.1 REST API; preview-grade; may reset or 500 without notice. Must be handled gracefully per FR-04 and NFR-08. |

---

## 15. Open Questions

| # | Question | Owner | Status |
|---|----------|-------|--------|
| OQ-1 | The registry returns `serverName` in reverse-domain format (`io.github.user/name`). Should `tag mcp registry install` accept both the full reverse-domain name and a short alias (e.g., `github` → `io.modelcontextprotocol/github`)? Short aliases require a deterministic mapping strategy. | eng | Open |
| OQ-2 | Should `tool_count` in `mcp_registry_servers` be estimated from the `tools[]` array in the registry response, or fetched live by connecting to the server? Live fetching is accurate but slow (requires spawning MCP servers). Registry metadata is stale but instant. | eng | Open; recommend registry estimate for now |
| OQ-3 | The registry `/v0.1/servers` endpoint does not support full-text search server-side. FTS5 is client-side. For Smithery subregistry (`--source`), does Smithery support server-side search? Should `tag mcp registry search` pass `?search=` to the source API when available? | eng | Open |
| OQ-4 | What is the correct behavior when the same `serverName` appears in both the official registry and a subregistry (e.g., Smithery)? Last-write-wins by `source_url` currently. Should `meta_json` merge fields from multiple sources? | product | Open |
| OQ-5 | Should `tag mcp registry update` be auto-triggered on first `tag mcp registry search` or `list` when the cache is empty, with a user prompt? This reduces friction but adds a network call to a command expected to be fast. | product | Open |
| OQ-6 | The FTS5 `porter` tokenizer does not handle camelCase identifiers well (`playwright-mcp` vs `playwrightMcp`). Should the tokenize config include a custom tokenizer or should package identifiers be pre-processed (e.g., split on `-`, `_`, `/`)? | eng | Open; pre-process during `UpsertPage` as interim solution |
| OQ-7 | `mcp_registry_packages.runtime_args` stores extra args from the registry. For security, should args containing shell metacharacters (`$`, `;`, `&`, `|`) be rejected or escaped before writing to `lab-config.yaml`? | security | Open; recommend rejection + warning |
| OQ-8 | The registry `_meta.status` field uses values like `active`, `deprecated`, `archived`. Should `tag mcp registry install` refuse to install `deprecated` or `archived` servers without `--force`? | product | Open |

---

## 16. Complexity and Timeline

**Total estimate: 3-5 days (S)**

### Phase 1 — SQLite Schema + HTTP Client (Day 1)

- Add `internal/store/migrations/073_mcp_registry.sql` with full DDL; wire into `store.Open()` migration runner via `go:embed`.
- Write `internal/mcp/registry/client.go`: `MCPServerEntry`, `MCPPackage`, `SyncResult`, `ErrorPage` structs; `ScrapeRegistry()`; `parsePage()`; `inferCategory()`; `inferRequiredEnv()`; `RedactSecrets()`.
- Implement `cenkalti/backoff/v4` retry loop with `io.LimitReader` body cap.
- Unit tests: `TestScrapeSinglePage`, `TestScrapePagination`, `TestScrape500Retry`, `TestSecretRedaction`.

**Exit criteria:** `ScrapeRegistry()` fetches and parses the first 50 entries from the live registry without error; redaction unit tests pass; `go test ./internal/mcp/registry/...` green.

### Phase 2 — `registry update` Command (Day 2)

- Implement `UpsertPage()` and `RebuildFTS()` in `internal/mcp/registry/db.go`.
- Implement `runRegistryUpdate` in `internal/cli/mcp_registry.go`: `updated_since` detection, `PageCallback`, FTS5 rebuild, sync log update.
- Register Cobra flags: `--source`, `--limit`, `--dry-run`, `--json`.
- Unit tests: `TestIncrementalSync`, `TestIdempotentUpsert`, `TestDryRunNoFileWrite`, `TestSourceURLHTTPRejected`.

**Exit criteria:** `tag mcp registry update --limit 50` syncs 50 entries to SQLite; `SELECT COUNT(*) FROM mcp_registry_servers` returns 50; second run returns `servers_new=0`, `servers_updated=0`.

### Phase 3 — Search + List (Day 3)

- Implement `SearchServers()` with FTS5 MATCH and LIKE fallback in `internal/mcp/registry/search.go`.
- Update `runRegistrySearch` and `runRegistryList` in `internal/cli/mcp_registry.go` to use SQLite cache when rows > 0, `go:embed` YAML fallback when rows == 0.
- Add `--category`, `--transport`, `--limit` flags to search.
- Unit tests: `TestFTS5Search`, `TestFTS5Fallback`, `TestYAMLFallbackWhenEmpty`.
- Benchmark: `BenchmarkFTS5Search` (10,000 mock rows, p99 < 100 ms).

**Exit criteria:** `tag mcp registry search "calendar scheduling"` after a 50-entry limit sync returns relevant results; LIKE fallback with empty FTS5 table passes CI; benchmark within target.

### Phase 4 — Install Abstraction + add-curated (Day 4)

- Implement `BuildConfigBlock()` in `internal/mcp/registry/install.go` with npm/pypi/docker/remote priority and SSE rejection.
- Refactor `runRegistryInstall` to use `BuildConfigBlock()` and resolve server name from SQLite (falling back to bundled YAML).
- Implement tool budget check: sum `tool_count` for profile's current MCP servers + incoming server.
- Implement `runRegistryAddCurated` subcommand reading `go:embed`-bundled YAML.
- Unit tests: all `TestBuild*ConfigBlock` tests, `TestToolBudgetWarning`, `TestToolBudgetForce`, `TestSSEPackageSkipped`, `TestAddCuratedIdempotent`.

**Exit criteria:** `tag mcp registry install mcp-filesystem --dry-run` produces correct `npx` block with no file write; `install playwright-mcp` on a near-40-tool profile fires warning to stderr.

### Phase 5 — `tag doctor` Integration + Polish (Day 5)

- Add `mcp_registry` check block to `runDoctor` in `internal/cli/doctor.go`.
- Wire `mcp_registry_sync_log` freshness and error checks (stale > 7 days, errors > 0, never synced).
- Integration tests: `TestLiveRegistryFirstPage` (network, guarded by build tag), `TestDoctorStaleRegistryWarning`.
- Full CLI smoke test across all 18 acceptance criteria.
- Golden-transcript fixtures under `testdata/golden/mcp_registry_*.txt` regenerated with `-update` flag.

**Exit criteria:** All 18 acceptance criteria pass; `tag doctor` shows green `mcp_registry` check after a successful sync; `go test ./... -tags integration` green in CI with `SKIP_NETWORK_TESTS` unset.
