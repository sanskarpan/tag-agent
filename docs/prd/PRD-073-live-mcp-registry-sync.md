# PRD-073: Live MCP Registry Sync from modelcontextprotocol.io (`tag mcp registry update`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (3-5 days)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `controller.py (_cmd_mcp_registry_update)`, `src/tag/config/mcp-registry.yaml`
**Depends on:** PRD-014 (MCP server registry), PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (secret scanning), PRD-043 (vector-based tool retrieval)
**Inspired by:** MCP Registry (modelcontextprotocol.io), Smithery, mcp.so

---

## 1. Overview

TAG currently ships a hand-curated `src/tag/config/mcp-registry.yaml` containing exactly 10 MCP server entries. This static file is the sole source of truth for `tag mcp registry list`, `tag mcp registry search`, and `tag mcp registry install`. As of June 2026, the official MCP registry at `https://registry.modelcontextprotocol.io` lists 9,652+ server entries spanning filesystem tools, database connectors, browser automation, productivity suites, ML pipelines, and specialized domain tools. The gap between 10 curated entries and 9,652+ live entries is the single largest bottleneck preventing TAG users from accessing the full MCP ecosystem through a managed install path.

This PRD specifies `tag mcp registry update`, a command that performs a cursor-paginated scrape of the official MCP registry REST API and persists the result into a local SQLite cache at `~/.tag/runtime/tag.sqlite3` (WAL mode, accessed via `open_db()`). Subsequent commands — `tag mcp registry list`, `tag mcp registry search`, `tag mcp registry install` — query this SQLite cache rather than the bundled YAML, enabling full-text search over the complete 9,652+ entry corpus with sub-100ms latency. The command also accepts `--source` to target subregistries (Smithery, mcp.so) that implement the same OpenAPI spec and inject custom `_meta` fields with ratings and security scan results.

The design follows the established TAG patterns rigorously. The scrape loop uses `httpx` with exponential backoff and graceful 500-handling (the registry is explicitly preview-grade and resets occur). An `updated_since` parameter enables incremental syncs that touch only entries modified since the last successful scrape, reducing typical sync time from minutes to seconds after the initial cold pull. The SQLite schema uses FTS5 virtual tables for full-text search over server name, description, and package identifiers. The tool budget awareness feature embeds `tool_count` from the registry `tools[]` array so that `tag mcp registry install` can warn before crossing the 40-tool Cursor ceiling.

The implementation is designed for a 3-5 day engineering sprint. The core scrape-and-cache loop, SQLite schema migration, search command, and install method abstraction (npx/uvx/docker/remote) constitute the critical path. Secondary features — `--json` structured output, curated add-backs, tool budget preflight — are additive and non-blocking.

---

## 2. Problem Statement

### 2.1 The Static YAML is a Critical Bottleneck

The bundled `mcp-registry.yaml` has 10 entries. The live ecosystem has 9,652+. Every server outside those 10 entries requires the user to manually find the npm/PyPI package name, construct the `npx`/`uvx` invocation, figure out required environment variables, and hand-edit `lab-config.yaml` inside the target profile directory. This process takes 10-30 minutes per server and produces configuration that is not validated, not version-tracked, and not shareable. Users who want to add Notion, Playwright, Context7, or Linear integration currently have zero managed path through TAG. This is a retention problem: users who discover they need to hand-edit YAML tend to look for alternative tools.

### 2.2 Search is Absent from the Discovery Path

`tag mcp registry list` produces a 10-row table. `tag mcp registry search` does substring matching over those same 10 rows. There is no semantic search, no category filter, no popularity sort, and no way to discover servers for a use case ("calendar scheduling", "browser automation", "vector database") without already knowing the server name. The `tool_retrieval.py` semantic search infrastructure (SentenceTransformer, Chroma, FTS5 fallback) exists and is operational, but it cannot index what it does not have. Connecting the live registry to the existing search infrastructure requires only the cache layer specified in this PRD.

### 2.3 Install Method Diversity is Unhandled

The 10 bundled servers are all npm packages launched via `npx`. The live registry includes servers distributed as:
- npm packages (`npx -y <pkg>` / `npm install -g <pkg>`)
- Python packages (`uvx <pkg>` / `pip install <pkg>` then `python -m <module>`)
- Docker images (`docker run -i --rm <image>`)
- Vendor-hosted cloud endpoints (remote streamable-HTTP URLs — Google Workspace, Atlassian Rovo, Linear)
- SSE-only legacy servers (deprecated; must be skipped or proxied)

The current `cmd_mcp_registry` in `controller.py` handles only `npm` and `pip` install types. Docker and remote-URL servers silently fail or are incorrectly omitted. This leaves the fastest-growing segment of the registry (vendor-hosted cloud MCP) completely inaccessible via `tag mcp registry install`.

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
| NG3 | Real-time streaming of registry updates. Syncs are manual (`tag mcp registry update`) or cron-driven via the existing `cron_scheduler.py`; no WebSocket or SSE subscription. |
| NG4 | Automatic nightly sync as a daemon. Cron-based scheduling is supported by the existing `cron_scheduler.py` but is out of scope for this PRD's implementation. Users may configure it separately. |
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
| HTTP resilience | Scrape continues after 3 consecutive HTTP 500s from registry API | Unit test with mocked httpx responses |
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
  Next sync:      tag mcp registry update  (or schedule via cron_scheduler)
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

Re-applies the 10 bundled YAML entries from `src/tag/config/mcp-registry.yaml` into the SQLite cache and the target profile's `lab-config.yaml`. Does not remove any existing configured servers. Intended for onboarding new profiles with the known-good baseline set.

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
| FR-01 | `_cmd_mcp_registry_update(args)` MUST perform cursor-paginated GET requests to `{source}/v0.1/servers?limit=100&cursor={cursor}` until `nextCursor` is null or empty. | P0 |
| FR-02 | Each page response MUST be committed to SQLite incrementally (not buffered in memory) so that partial scrapes preserve all fetched data on timeout or error. | P0 |
| FR-03 | When `updated_since` is available from `mcp_registry_sync_log`, the scrape MUST append `&updated_since={iso8601}` to the first request URL; this timestamp MUST be the `started_at` of the last successful sync. | P0 |
| FR-04 | HTTP 5xx responses from the registry MUST trigger exponential backoff (base 1s, multiplier 2, max 30s) up to 5 retries before skipping the page and recording the error in `mcp_registry_sync_log.error_pages_json`. | P0 |
| FR-05 | Each `MCPServerEntry` scraped MUST be upserted into `mcp_registry_servers` (keyed on `name + version`); `is_latest` MUST be set from the `_meta.isLatest` field when present, otherwise from position in versions list. | P0 |
| FR-06 | Each `MCPPackage` in the `packages[]` array MUST be upserted into `mcp_registry_packages` with its `registry_type` (`npm`, `pypi`, `docker`), `identifier`, `version`, and `transport_type`. | P0 |
| FR-07 | `tag mcp registry search "<query>"` MUST execute an FTS5 `MATCH` query over `name`, `description`, and `package_identifiers_fts` columns and return results ordered by FTS5 BM25 rank. | P0 |
| FR-08 | When the SQLite cache contains zero rows (no prior `update`), `search` and `list` MUST fall back to the bundled `mcp-registry.yaml` and display a notice prompting the user to run `update`. | P0 |
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
| FR-21 | The `_cmd_mcp_registry_update` function MUST be invocable with `--limit N` to scrape at most N servers total, enabling fast smoke-tests in CI without a full scrape. | P2 |
| FR-22 | `tag mcp registry search` MUST support `--category` filter translated to `WHERE category = ?` on the SQLite query. | P2 |
| FR-23 | The FTS5 content table MUST be rebuilt automatically after any sync that updates more than 100 rows (`INSERT INTO mcp_registry_fts(mcp_registry_fts) VALUES('rebuild')`). | P2 |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Cold sync of 9,652 servers with 100 entries per page (~97 pages) MUST complete in < 5 minutes on a 50 Mbps connection. | < 5 min |
| NFR-02 | Incremental sync when < 200 entries changed MUST complete in < 30 seconds. | < 30 s |
| NFR-03 | FTS5 search over 9,652 entries MUST return p99 results in < 100 ms. | < 100 ms |
| NFR-04 | The scrape loop MUST NOT load more than 50 MB of JSON into process memory at once; pages are parsed and upserted before the next page is fetched. | < 50 MB |
| NFR-05 | All SQLite writes MUST use WAL mode (inherited from `open_db()`) and MUST batch-upsert within a single transaction per page (100 rows per `BEGIN...COMMIT`). | Per existing pattern |
| NFR-06 | `httpx` is the HTTP client (already used in TAG); no new heavy HTTP dependencies may be added. Connection pool size: 10 connections. Request timeout: 30 seconds. | Existing deps only |
| NFR-07 | `tag mcp registry install` MUST NOT execute any `npm install`, `uvx`, or `docker pull` command unless the user has confirmed (or `--force` is passed). Config-only write is the default; actual package resolution happens at agent startup via `npx -y` (which downloads on demand). | No silent side effects |
| NFR-08 | All network errors (DNS failure, TLS error, timeout) MUST be caught, logged to `mcp_registry_sync_log`, and result in a non-zero exit code with a human-readable message. The existing `mcp-registry.yaml` fallback is NEVER overwritten by a failed sync. | Graceful failure |
| NFR-09 | The `_meta` field from subregistries may contain arbitrary JSON and MUST be stored as-is in `meta_json TEXT`. No schema validation is applied to `_meta` contents. | Open schema |
| NFR-10 | `tag mcp registry update` MUST be idempotent: running it twice in a row with no registry changes results in `servers_new=0`, `servers_updated=0`, and the same final database state. | Idempotent |
| NFR-11 | Secret scanning (PRD-034 patterns) MUST be applied to all string values written from `_meta` into `meta_json`; any value matching a secret pattern MUST be redacted to `"[REDACTED]"` and the redaction counted in `sync_log.redacted_count`. | Security |
| NFR-12 | The implementation MUST pass all existing `cmd_mcp_registry` tests without modification; new behavior is additive behind the `update` subcommand. | Backward compat |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/integrations/mcp_registry_client.py` | HTTP client for `registry.modelcontextprotocol.io`; pagination loop; dataclasses; secret redaction |
| `src/tag/integrations/__init__.py` | Already exists (touch only if needed) |

Controller changes are confined to `controller.py`:
- `_cmd_mcp_registry_update(args)` — new handler
- `_mcp_registry_search_sql(conn, query, category, transport, limit)` — new helper
- `_mcp_install_server(conn, name, profile_path, force, dry_run)` — new helper refactored from existing `cmd_mcp_registry`
- `cmd_mcp_registry(args)` — dispatch updated to route `update` subcommand to `_cmd_mcp_registry_update`

### 10.2 SQLite DDL

The following tables are added to the `executescript` block in `open_db()`:

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

### 10.3 Core Dataclasses

```python
# src/tag/integrations/mcp_registry_client.py
from __future__ import annotations

import dataclasses
import re
from typing import Optional

# Patterns lifted from security.py (PRD-034)
_SECRET_PATTERNS: list[re.Pattern] = [
    re.compile(r"(?i)(api[_-]?key|secret|token|password|passwd|credential)\s*[:=]\s*\S+"),
    re.compile(r"(?i)(sk|pk|ak|rk)-[A-Za-z0-9]{20,}"),
]


@dataclasses.dataclass
class MCPPackage:
    registry_type: str          # 'npm' | 'pypi' | 'docker' | 'remote'
    identifier: str             # npm pkg name, pypi name, docker image, or URL
    package_version: str        # semantic version or 'latest'
    transport_type: str         # 'stdio' | 'streamable-http' | 'sse'
    transport_url: Optional[str] = None   # only for remote packages
    runtime_args: list[str] = dataclasses.field(default_factory=list)


@dataclasses.dataclass
class MCPServerEntry:
    name: str                   # 'io.github.username/server-name'
    version: str
    description: str
    category: str
    repository_url: str
    published_at: str
    updated_at: str
    status: str                 # 'active' | 'deprecated' | 'deleted'
    is_latest: bool
    tool_count: int
    transport_type: str         # primary transport
    requires_env: list[str]
    packages: list[MCPPackage]
    meta_json: dict             # raw _meta field


@dataclasses.dataclass
class SyncResult:
    source_url: str
    servers_synced: int = 0
    servers_new: int = 0
    servers_updated: int = 0
    errors: int = 0
    redacted_count: int = 0
    cursor_final: Optional[str] = None
    duration_ms: int = 0
    error_pages: list[dict] = dataclasses.field(default_factory=list)


def redact_secrets(value: str) -> tuple[str, int]:
    """Redact secret-like patterns from a string. Returns (redacted_str, count_redacted)."""
    count = 0
    for pat in _SECRET_PATTERNS:
        if pat.search(value):
            value = pat.sub("[REDACTED]", value)
            count += 1
    return value, count
```

### 10.4 HTTP Pagination Loop

```python
# src/tag/integrations/mcp_registry_client.py  (continued)
import json
import time
import httpx

MCP_REGISTRY_BASE = "https://registry.modelcontextprotocol.io"
PAGE_SIZE = 100
MAX_RETRIES = 5


def scrape_registry(
    source: str = MCP_REGISTRY_BASE,
    updated_since: Optional[str] = None,
    limit: Optional[int] = None,
    on_page: Optional[callable] = None,  # callback(page_num, entries: list[MCPServerEntry])
) -> SyncResult:
    """
    Full cursor-paginated scrape of the MCP registry.

    Args:
        source:        Base URL of registry implementing /v0.1/servers
        updated_since: RFC3339 timestamp; if set, only fetch entries updated after this time
        limit:         Stop after this many total servers (for CI smoke tests)
        on_page:       Optional callback invoked after each page is parsed

    Returns:
        SyncResult with counts and error summary
    """
    result = SyncResult(source_url=source)
    t0 = time.monotonic()
    cursor: Optional[str] = None
    page_num = 0

    with httpx.Client(timeout=30.0, limits=httpx.Limits(max_connections=10)) as client:
        while True:
            params: dict = {"limit": PAGE_SIZE}
            if cursor:
                params["cursor"] = cursor
            if updated_since:
                params["updated_since"] = updated_since

            # Exponential backoff retry loop
            backoff = 1.0
            resp = None
            for attempt in range(MAX_RETRIES):
                try:
                    resp = client.get(f"{source}/v0.1/servers", params=params)
                    if resp.status_code < 500:
                        break
                    # 5xx: record and retry
                    result.errors += 1
                    result.error_pages.append({
                        "page": page_num,
                        "cursor": cursor,
                        "status_code": resp.status_code,
                        "error": resp.text[:200],
                    })
                    if attempt < MAX_RETRIES - 1:
                        time.sleep(min(backoff, 30.0))
                        backoff *= 2
                except httpx.HTTPError as exc:
                    result.errors += 1
                    result.error_pages.append({
                        "page": page_num,
                        "cursor": cursor,
                        "status_code": 0,
                        "error": str(exc)[:200],
                    })
                    if attempt < MAX_RETRIES - 1:
                        time.sleep(min(backoff, 30.0))
                        backoff *= 2
                    resp = None

            if resp is None or resp.status_code >= 500:
                # Exhausted retries — skip page and continue
                break

            if resp.status_code != 200:
                result.errors += 1
                break

            data = resp.json()
            raw_servers = data.get("servers", [])
            next_cursor = data.get("nextCursor") or None

            entries = _parse_page(raw_servers, source, result)
            page_num += 1

            if on_page:
                on_page(page_num, entries)

            result.servers_synced += len(entries)

            if limit and result.servers_synced >= limit:
                result.cursor_final = next_cursor
                break

            if not next_cursor:
                result.cursor_final = None
                break

            cursor = next_cursor

    result.duration_ms = int((time.monotonic() - t0) * 1000)
    return result


def _parse_page(
    raw_servers: list[dict],
    source_url: str,
    result: SyncResult,
) -> list[MCPServerEntry]:
    """Parse a single page of raw server JSON into MCPServerEntry objects."""
    entries = []
    for raw in raw_servers:
        # Redact _meta before storage
        meta = raw.get("_meta", {})
        meta_str = json.dumps(meta)
        meta_str, redact_count = redact_secrets(meta_str)
        result.redacted_count += redact_count
        meta = json.loads(meta_str)

        packages = []
        for pkg in raw.get("packages", []):
            packages.append(MCPPackage(
                registry_type=pkg.get("registryType", "npm"),
                identifier=pkg.get("name", ""),
                package_version=pkg.get("version", ""),
                transport_type=(pkg.get("runtime", {}) or {}).get("transport", "stdio"),
                transport_url=(pkg.get("runtime", {}) or {}).get("url"),
                runtime_args=(pkg.get("runtime", {}) or {}).get("args", []),
            ))

        tool_count = sum(
            len(pkg.get("tools", []))
            for pkg in raw.get("packages", [])
        )

        entry = MCPServerEntry(
            name=raw.get("name", ""),
            version=raw.get("version", ""),
            description=raw.get("description", ""),
            category=_infer_category(raw),
            repository_url=(raw.get("repository") or {}).get("url", ""),
            published_at=raw.get("publishedAt", ""),
            updated_at=raw.get("updatedAt", ""),
            status=meta.get("status", "active"),
            is_latest=bool(meta.get("isLatest", False)),
            tool_count=tool_count,
            transport_type=packages[0].transport_type if packages else "stdio",
            requires_env=_infer_required_env(raw),
            packages=packages,
            meta_json=meta,
        )
        entries.append(entry)
    return entries


def _infer_category(raw: dict) -> str:
    """Map registry tags/description to TAG category labels."""
    tags = [t.lower() for t in raw.get("tags", [])]
    desc = raw.get("description", "").lower()
    name = raw.get("name", "").lower()
    for keyword, cat in [
        ("filesystem", "filesystem"), ("file", "filesystem"),
        ("database", "database"), ("postgres", "database"), ("sqlite", "database"),
        ("mysql", "database"), ("mongo", "database"),
        ("github", "vcs"), ("gitlab", "vcs"), ("git", "vcs"),
        ("browser", "web"), ("playwright", "web"), ("puppeteer", "web"),
        ("fetch", "web"), ("search", "web"),
        ("slack", "messaging"), ("email", "messaging"), ("gmail", "messaging"),
        ("calendar", "productivity"), ("notion", "productivity"), ("linear", "productivity"),
        ("memory", "memory"), ("vector", "ml"), ("embedding", "ml"),
        ("reasoning", "reasoning"), ("thinking", "reasoning"),
        ("docker", "devops"), ("kubernetes", "devops"), ("ci", "devops"),
    ]:
        if keyword in tags or keyword in desc or keyword in name:
            return cat
    return "other"


def _infer_required_env(raw: dict) -> list[str]:
    """Extract required environment variable names from registry entry."""
    env_vars = []
    for pkg in raw.get("packages", []):
        for env in (pkg.get("environment", []) or []):
            if env.get("required", False):
                env_vars.append(env.get("name", ""))
    return [v for v in env_vars if v]
```

### 10.5 SQLite Upsert Batch

```python
# In controller.py — _cmd_mcp_registry_update
def _upsert_page_to_db(conn: sqlite3.Connection, entries: list, source_url: str) -> tuple[int, int]:
    """
    Upsert a page of MCPServerEntry objects into mcp_registry_servers and mcp_registry_packages.
    Returns (new_count, updated_count).
    """
    new_count = 0
    updated_count = 0

    with conn:
        for entry in entries:
            existing = conn.execute(
                "SELECT id, updated_at FROM mcp_registry_servers WHERE name=? AND version=?",
                (entry.name, entry.version),
            ).fetchone()

            if existing is None:
                conn.execute(
                    """
                    INSERT INTO mcp_registry_servers
                      (name, version, description, category, repository_url,
                       published_at, updated_at, status, is_latest, tool_count,
                       transport_type, requires_env, meta_json, source_url, synced_at)
                    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,datetime('now'))
                    """,
                    (
                        entry.name, entry.version, entry.description, entry.category,
                        entry.repository_url, entry.published_at, entry.updated_at,
                        entry.status, int(entry.is_latest), entry.tool_count,
                        entry.transport_type, json.dumps(entry.requires_env),
                        json.dumps(entry.meta_json), source_url,
                    ),
                )
                new_count += 1
            elif existing["updated_at"] != entry.updated_at:
                conn.execute(
                    """
                    UPDATE mcp_registry_servers
                    SET description=?, category=?, repository_url=?, published_at=?,
                        updated_at=?, status=?, is_latest=?, tool_count=?,
                        transport_type=?, requires_env=?, meta_json=?, source_url=?,
                        synced_at=datetime('now')
                    WHERE name=? AND version=?
                    """,
                    (
                        entry.description, entry.category, entry.repository_url,
                        entry.published_at, entry.updated_at, entry.status,
                        int(entry.is_latest), entry.tool_count, entry.transport_type,
                        json.dumps(entry.requires_env), json.dumps(entry.meta_json),
                        source_url, entry.name, entry.version,
                    ),
                )
                updated_count += 1

            # Refresh packages for this server
            conn.execute(
                "DELETE FROM mcp_registry_packages WHERE server_name=? AND server_version=?",
                (entry.name, entry.version),
            )
            for pkg in entry.packages:
                conn.execute(
                    """
                    INSERT INTO mcp_registry_packages
                      (server_name, server_version, registry_type, identifier,
                       package_version, transport_type, transport_url, runtime_args)
                    VALUES (?,?,?,?,?,?,?,?)
                    """,
                    (
                        entry.name, entry.version, pkg.registry_type, pkg.identifier,
                        pkg.package_version, pkg.transport_type, pkg.transport_url,
                        json.dumps(pkg.runtime_args),
                    ),
                )

    return new_count, updated_count
```

### 10.6 Install Method Abstraction

```python
# In controller.py — _build_mcp_config_block
INSTALL_PRIORITY = ["npm", "pypi", "docker", "remote"]
SSE_TRANSPORT = "sse"


def _build_mcp_config_block(
    server_name: str,
    packages: list[dict],  # rows from mcp_registry_packages
) -> dict | None:
    """
    Build the MCP server config block for lab-config.yaml from registry package info.
    Returns None if no supported package type is found.
    Priority: npm > pypi > docker > remote (streamable-http).
    SSE-only packages are explicitly rejected.
    """
    # Sort by priority
    def priority(pkg: dict) -> int:
        return INSTALL_PRIORITY.index(pkg["registry_type"]) if pkg["registry_type"] in INSTALL_PRIORITY else 99

    for pkg in sorted(packages, key=priority):
        rtype = pkg["registry_type"]
        transport = pkg.get("transport_type", "stdio")
        identifier = pkg["identifier"]

        if transport == SSE_TRANSPORT:
            continue  # SSE is deprecated; skip

        if rtype == "npm":
            return {
                "name": server_name,
                "command": "npx",
                "args": ["-y", identifier],
                "transport": "stdio",
            }
        elif rtype == "pypi":
            return {
                "name": server_name,
                "command": "uvx",
                "args": [identifier],
                "transport": "stdio",
            }
        elif rtype == "docker":
            return {
                "name": server_name,
                "command": "docker",
                "args": ["run", "-i", "--rm", identifier],
                "transport": "stdio",
            }
        elif rtype == "remote" and pkg.get("transport_url"):
            return {
                "name": server_name,
                "url": pkg["transport_url"],
                "transport": "streamable-http",
            }

    return None  # No supported package found
```

### 10.7 FTS5 Search Query

```python
# In controller.py — _mcp_registry_search_sql
def _mcp_registry_search_sql(
    conn: sqlite3.Connection,
    query: str,
    category: str | None = None,
    transport: str | None = None,
    limit: int = 20,
) -> list[dict]:
    """
    Full-text search over mcp_registry_fts with optional category/transport filters.
    Falls back to LIKE-based search if FTS5 query parse fails.
    Returns list of dicts with keys: name, version, description, category, transport_type,
    tool_count, requires_env, repository_url, meta_json.
    """
    params: list = []
    filters = "WHERE s.is_latest = 1"

    if category:
        filters += " AND s.category = ?"
        params.append(category)
    if transport:
        filters += " AND s.transport_type = ?"
        params.append(transport)

    fts_query = query.replace('"', '""')  # escape FTS5 query syntax

    try:
        sql = f"""
            SELECT s.name, s.version, s.description, s.category, s.transport_type,
                   s.tool_count, s.requires_env, s.repository_url, s.meta_json
            FROM mcp_registry_fts AS f
            JOIN mcp_registry_servers AS s ON s.id = f.rowid
            {filters}
              AND f.mcp_registry_fts MATCH ?
            ORDER BY bm25(mcp_registry_fts) ASC
            LIMIT ?
        """
        rows = conn.execute(sql, params + [fts_query, limit]).fetchall()
    except sqlite3.OperationalError:
        # FTS5 query parse error — fall back to LIKE
        like_query = f"%{query}%"
        sql = f"""
            SELECT s.name, s.version, s.description, s.category, s.transport_type,
                   s.tool_count, s.requires_env, s.repository_url, s.meta_json
            FROM mcp_registry_servers AS s
            {filters}
              AND (s.name LIKE ? OR s.description LIKE ?)
            ORDER BY s.updated_at DESC
            LIMIT ?
        """
        rows = conn.execute(sql, params + [like_query, like_query, limit]).fetchall()

    return [dict(r) for r in rows]
```

### 10.8 `updated_since` Incremental Sync Logic

```python
# In controller.py — _cmd_mcp_registry_update (excerpt)
def _cmd_mcp_registry_update(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    conn = open_db(cfg)
    source = getattr(args, "source", None) or MCP_REGISTRY_BASE
    limit = getattr(args, "limit", None)
    dry_run = getattr(args, "dry_run", False)
    emit_json = getattr(args, "json", False)

    # Determine updated_since from last successful sync
    last_sync = conn.execute(
        """
        SELECT started_at FROM mcp_registry_sync_log
        WHERE status = 'complete' AND source_url = ?
        ORDER BY finished_at DESC LIMIT 1
        """,
        (source,),
    ).fetchone()
    updated_since = last_sync["started_at"] if last_sync else None

    if not emit_json:
        mode = f"incremental (since {updated_since})" if updated_since else "full"
        print(f"Syncing MCP registry [{mode}] from {source} ...")

    # Insert sync log row (running)
    sync_log_id = conn.execute(
        """
        INSERT INTO mcp_registry_sync_log (started_at, source_url, status)
        VALUES (datetime('now'), ?, 'running')
        """,
        (source,),
    ).lastrowid
    conn.commit()

    total_new = 0
    total_updated = 0
    page_num = 0

    def on_page(pnum: int, entries):
        nonlocal total_new, total_updated, page_num
        page_num = pnum
        if not dry_run:
            n, u = _upsert_page_to_db(conn, entries, source)
            total_new += n
            total_updated += u
        if not emit_json:
            print(f"  Page {pnum:3}: {len(entries):3} servers", end="\r", flush=True)

    from tag.integrations.mcp_registry_client import scrape_registry
    result = scrape_registry(
        source=source,
        updated_since=updated_since,
        limit=limit,
        on_page=on_page,
    )

    # Rebuild FTS5 index if significant changes
    if not dry_run and (total_new + total_updated) > 100:
        conn.execute("INSERT INTO mcp_registry_fts(mcp_registry_fts) VALUES('rebuild')")
        conn.commit()

    # Update sync log
    status = "complete" if result.errors == 0 else ("partial" if result.servers_synced > 0 else "failed")
    conn.execute(
        """
        UPDATE mcp_registry_sync_log
        SET finished_at=datetime('now'), servers_synced=?, servers_new=?, servers_updated=?,
            errors=?, redacted_count=?, cursor_final=?, duration_ms=?,
            error_pages_json=?, status=?
        WHERE id=?
        """,
        (
            result.servers_synced, total_new, total_updated,
            result.errors, result.redacted_count, result.cursor_final,
            result.duration_ms, json.dumps(result.error_pages), status,
            sync_log_id,
        ),
    )
    conn.commit()
    conn.close()

    # Output
    if emit_json:
        print(json.dumps({
            "source": source,
            "synced_at": dt.datetime.utcnow().isoformat() + "Z",
            "servers_total": result.servers_synced,
            "servers_new": total_new,
            "servers_updated": total_updated,
            "servers_unchanged": result.servers_synced - total_new - total_updated,
            "errors": result.errors,
            "duration_ms": result.duration_ms,
            "cursor_final": result.cursor_final,
        }, indent=2))
    else:
        print()  # clear \r line
        print(f"\nRegistry sync {'(dry-run) ' if dry_run else ''}complete.")
        print(f"  Servers synced: {result.servers_synced:,}")
        print(f"  New:            {total_new:,}")
        print(f"  Updated:        {total_updated:,}")
        print(f"  Unchanged:      {result.servers_synced - total_new - total_updated:,}")
        print(f"  Errors:         {result.errors}")
        print(f"  Duration:       {result.duration_ms / 1000:.1f}s")
        if result.errors > 0:
            print(f"\n  Warning: {result.errors} page(s) failed. See mcp_registry_sync_log for details.")

    return 0 if result.errors == 0 else 1
```

### 10.9 Integration with `tag doctor`

The existing `_cmd_doctor` function in `controller.py` is extended with a new check block:

```python
# In controller.py — inside _cmd_doctor's check loop
last_mcp_sync = conn.execute(
    "SELECT finished_at, status, errors FROM mcp_registry_sync_log ORDER BY finished_at DESC LIMIT 1"
).fetchone()

if last_mcp_sync is None:
    doctor_warn("mcp_registry", "No MCP registry sync has been performed. Run: tag mcp registry update")
else:
    age_days = (dt.datetime.utcnow() - dt.datetime.fromisoformat(last_mcp_sync["finished_at"])).days
    if age_days > 7:
        doctor_warn("mcp_registry", f"MCP registry cache is {age_days} days old. Run: tag mcp registry update")
    if last_mcp_sync["errors"] > 0:
        doctor_warn("mcp_registry", f"Last sync had {last_mcp_sync['errors']} error(s). Run: tag mcp registry update")
    if last_mcp_sync["status"] == "complete" and age_days <= 7:
        doctor_ok("mcp_registry", f"Registry cache fresh ({age_days}d old)")
```

### 10.10 Argument Parser Extensions

```python
# In controller.py — inside _build_mcp_registry_parser() or equivalent
# Existing subparsers: list, install, enable, disable, search, update (stub)
# Add to update subparser:
p_update = mcp_reg_subs.add_parser("update", help="Sync registry from modelcontextprotocol.io")
p_update.add_argument("--source", default=MCP_REGISTRY_BASE,
                      help="Registry base URL (default: https://registry.modelcontextprotocol.io)")
p_update.add_argument("--limit", type=int, default=None,
                      help="Stop after N servers (smoke test mode)")
p_update.add_argument("--dry-run", action="store_true",
                      help="Fetch pages but do not write to SQLite")
p_update.add_argument("--json", action="store_true",
                      help="Emit sync summary as JSON to stdout")
```

---

## 11. Security Considerations

1. **Secret redaction in `_meta`**: The `_meta` field from subregistries is arbitrary third-party JSON. Before storing, `redact_secrets()` scans all string values for patterns matching API keys, tokens, and passwords. Any match is replaced with `"[REDACTED]"` and counted in `mcp_registry_sync_log.redacted_count`. This prevents a malicious subregistry from embedding credentials that would then be read by agents.

2. **No credential capture in install config**: `_build_mcp_config_block()` never writes API key values into `lab-config.yaml`. Required environment variable *names* (e.g., `NOTION_API_KEY`) are recorded in `requires_env`, but values come exclusively from the user's shell environment at agent startup. This matches the existing pattern in the bundled YAML.

3. **Source URL validation**: `--source` is validated to begin with `https://`. HTTP (non-TLS) registry sources are rejected with an error. This prevents man-in-the-middle injection of malicious server entries over plaintext connections.

4. **No automatic execution of registry-specified commands**: `tag mcp registry install` writes a config block but does NOT execute `npx`, `uvx`, or `docker` commands. The agent runtime is responsible for spawning MCP servers at session start. This means a malicious registry entry cannot trigger arbitrary command execution via `tag mcp registry install`.

5. **Path traversal in profile directory**: The target profile path is validated through the existing `_safe_profile_path()` helper before any file write, preventing a crafted server `name` field containing `../../etc/passwd` patterns from escaping the profile directory.

6. **Server name allowlist check**: `server_name` values longer than 256 characters or containing characters outside `[a-zA-Z0-9._/:-]` are rejected with a warning and not upserted into the database. This protects against SQLite injection via crafted server names in FTS5 queries.

7. **Rate limiting respect**: The scrape loop checks for `Retry-After` headers on 429 responses and sleeps for the indicated duration before retrying. This prevents TAG from being blocked by the registry's rate limiter.

8. **WAL isolation**: All registry reads during `tag mcp registry search` use a `BEGIN DEFERRED` transaction to ensure a consistent snapshot even if a concurrent `update` is running. This is automatically handled by SQLite's WAL mode and the existing `open_db()` configuration.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_mcp_registry_sync.py`)

| Test | Description |
|------|-------------|
| `test_scrape_empty_registry` | Mock `httpx` to return `{"servers": [], "nextCursor": null}`; assert `SyncResult.servers_synced == 0` and no DB writes |
| `test_scrape_single_page` | Mock one page of 5 entries; assert all 5 upserted to `mcp_registry_servers` |
| `test_scrape_pagination` | Mock 3 pages with cursors; assert all 300 entries upserted; `cursor_final` is null |
| `test_scrape_500_retry` | Mock 3 consecutive 500s then a 200; assert retry count in error_pages; entries from 200 upserted |
| `test_scrape_exhausted_retries` | Mock 5 consecutive 500s; assert `SyncResult.errors == 1`; graceful exit |
| `test_incremental_sync` | Insert a sync log row with `started_at`; mock registry call; assert `updated_since` param present in request URL |
| `test_secret_redaction` | Mock entry with `_meta: {"apiKey": "sk-secret123"}`; assert stored `meta_json` contains `[REDACTED]`; `redacted_count == 1` |
| `test_fts5_search` | Insert 50 mock entries; query `"calendar scheduling"`; assert top result is a calendar server |
| `test_fts5_fallback` | Drop FTS5 table; search should fall back to LIKE without raising |
| `test_build_npm_config_block` | npm package entry → `command: npx` block |
| `test_build_pypi_config_block` | pypi package entry → `command: uvx` block |
| `test_build_docker_config_block` | docker package entry → `command: docker` block with `run -i --rm` args |
| `test_build_remote_config_block` | remote package → `url:` block with `transport: streamable-http` |
| `test_sse_package_skipped` | SSE-only package → `_build_mcp_config_block` returns None |
| `test_tool_budget_warning` | Mock profile with 35 tools configured; install a 10-tool server; assert warning emitted |
| `test_tool_budget_force` | Same as above with `--force`; assert install proceeds without error exit |
| `test_install_already_configured` | Install a server already in profile YAML; assert "skipped" in output; no file change |
| `test_dry_run_no_file_write` | `--dry-run` mode; assert no `mcp_registry_servers` rows inserted and no `lab-config.yaml` modification |
| `test_source_url_http_rejected` | `--source http://evil.com`; assert exit code 1 and security error message |
| `test_server_name_length_limit` | Entry with 300-char name; assert not upserted; warning logged |
| `test_idempotent_upsert` | Run `_upsert_page_to_db` twice with same entries; assert row count unchanged; `servers_updated == 0` |
| `test_yaml_fallback_when_empty` | Call `search` with empty SQLite table; assert output contains bundled YAML entries |
| `test_add_curated_idempotent` | Run `add-curated` twice; assert second run shows all 10 as "already configured" |

### 12.2 Integration Tests (`tests/test_mcp_registry_integration.py`)

| Test | Description |
|------|-------------|
| `test_live_registry_first_page` | Fetch one page (limit=10) from live `registry.modelcontextprotocol.io`; assert >= 10 entries; skip in CI if `SKIP_NETWORK_TESTS=1` |
| `test_full_sync_and_search` | Full sync (limit=50); then search "github"; assert top result contains "github" in name |
| `test_install_into_temp_profile` | Create temp profile dir; `install mcp-filesystem`; assert `lab-config.yaml` has correct `npx` block |
| `test_doctor_stale_registry_warning` | Insert a sync log row 8 days old; run doctor check; assert warning text contains "days old" |

### 12.3 Performance Tests

| Test | Target |
|------|--------|
| `test_fts5_search_latency` | Search over 10,000 inserted mock entries; p99 < 100 ms across 20 queries |
| `test_upsert_throughput` | `_upsert_page_to_db` with 100-entry batches; 97 batches (9,700 entries) in < 60 seconds on SQLite WAL |
| `test_memory_ceiling` | Scrape loop with 100-entry pages; assert no single `on_page` callback holds > 50 MB of parsed JSON |

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
| AC-17 | `tag mcp registry update --json` emits valid JSON to stdout and all progress messages to stderr. | `stdout | jq -e .servers_total` succeeds; stderr has progress text |
| AC-18 | HTTP 500 from registry triggers retry with exponential backoff; after 5 failures the scrape moves to the next page rather than crashing. | Mock test with all-500 page + assertion on subsequent page processing |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| `httpx` | Python package | Already in TAG; use for registry HTTP client. Pin >= 0.27 for `Limits` constructor. |
| `sqlite3` | stdlib | FTS5 must be compiled in (verify with `SELECT fts5_version()` in `tag doctor`). |
| `yaml` (PyYAML) | Python package | Already in TAG; used for reading bundled YAML fallback. |
| `json` | stdlib | Used for `_meta` storage and `--json` output. |
| `PRD-014` (MCP Server Registry) | Internal PRD | This PRD extends the `cmd_mcp_registry` dispatcher and `_load_mcp_registry()` functions defined in PRD-014. |
| `PRD-034` (Secret Scanning) | Internal PRD | `redact_secrets()` borrows pattern list from `security.py`. Direct import or pattern duplication in `mcp_registry_client.py`. |
| `PRD-043` (Tool Retrieval) | Internal PRD | After `update`, running `tag tool index` will pick up 9,652+ servers if the FTS5 cache is wired to `mcp_registry_servers`. Consider exposing `mcp_registry_servers` as an additional source for `tool_retrieval.py`. |
| `PRD-013` (Agent Tracing) | Internal PRD | Sync log entries in `mcp_registry_sync_log` should appear in `tag doctor` alongside other health checks. |
| `PRD-009` (Enhanced Doctor Diagnostics) | Internal PRD | `tag doctor` receives a new `mcp_registry` check block. |
| `modelcontextprotocol.io` | External service | Registry v0.1 API; preview-grade; may reset or 500 without notice. Must be handled gracefully. |

---

## 15. Open Questions

| # | Question | Owner | Status |
|---|----------|-------|--------|
| OQ-1 | The registry returns `serverName` in reverse-domain format (`io.github.user/name`). Should `tag mcp registry install` accept both the full reverse-domain name and a short alias (e.g., `github` → `io.modelcontextprotocol/github`)? Short aliases require a deterministic mapping strategy. | eng | Open |
| OQ-2 | Should `tool_count` in `mcp_registry_servers` be estimated from the `tools[]` array in the registry response, or fetched live by connecting to the server? Live fetching is accurate but slow (requires spawning MCP servers). Registry metadata is stale but instant. | eng | Open; recommend registry estimate for now |
| OQ-3 | The registry `/v0.1/servers` endpoint does not support full-text search server-side. FTS5 is client-side. For Smithery subregistry (`--source`), does Smithery support server-side search? Should `tag mcp registry search` pass `?search=` to the source API when available? | eng | Open |
| OQ-4 | What is the correct behavior when the same `serverName` appears in both the official registry and a subregistry (e.g., Smithery)? Last-write-wins by `source_url` currently. Should `meta_json` merge fields from multiple sources? | product | Open |
| OQ-5 | Should `tag mcp registry update` be auto-triggered on first `tag mcp registry search` or `list` when the cache is empty, with a user prompt? This reduces friction but adds a network call to a command expected to be fast. | product | Open |
| OQ-6 | The FTS5 `porter` tokenizer does not handle camelCase identifiers well (`playwright-mcp` vs `playwrightMcp`). Should the tokenize config include a custom tokenizer or should package identifiers be pre-processed (e.g., split on `-`, `_`, `/`)? | eng | Open; pre-process during upsert as interim solution |
| OQ-7 | `mcp_registry_packages.runtime_args` stores extra args from the registry. For security, should args containing shell metacharacters (`$`, `;`, `&`, `|`) be rejected or escaped before writing to `lab-config.yaml`? | security | Open; recommend rejection + warning |
| OQ-8 | The registry `_meta.status` field uses values like `active`, `deprecated`, `archived`. Should `tag mcp registry install` refuse to install `deprecated` or `archived` servers without `--force`? | product | Open |

---

## 16. Complexity and Timeline

**Total estimate: 3-5 days (S)**

### Phase 1 — SQLite Schema + HTTP Client (Day 1)

- Add `mcp_registry_servers`, `mcp_registry_packages`, `mcp_registry_fts`, and `mcp_registry_sync_log` DDL to `open_db()`'s `executescript` block.
- Write `src/tag/integrations/mcp_registry_client.py` with `MCPServerEntry`, `MCPPackage`, `SyncResult` dataclasses and `scrape_registry()` pagination loop.
- Implement exponential backoff retry, `_parse_page()`, `_infer_category()`, `_infer_required_env()`, `redact_secrets()`.
- Unit tests: `test_scrape_single_page`, `test_scrape_pagination`, `test_scrape_500_retry`, `test_secret_redaction`.

**Exit criteria:** `scrape_registry()` fetches and parses the first 50 entries from the live registry without error; redaction unit tests pass.

### Phase 2 — `registry update` Command (Day 2)

- Implement `_cmd_mcp_registry_update()` in `controller.py` with `updated_since` detection, `on_page` callback, `_upsert_page_to_db()`, FTS5 rebuild, sync log update.
- Wire into `cmd_mcp_registry()` dispatcher (`sub == "update"`).
- Add `--source`, `--limit`, `--dry-run`, `--json` flags to argparse.
- Unit tests: `test_incremental_sync`, `test_idempotent_upsert`, `test_dry_run_no_file_write`, `test_source_url_http_rejected`.

**Exit criteria:** `tag mcp registry update --limit 50` syncs 50 entries to SQLite; `SELECT COUNT(*) FROM mcp_registry_servers` returns 50; second run returns `servers_new=0`.

### Phase 3 — Search + List (Day 3)

- Implement `_mcp_registry_search_sql()` with FTS5 MATCH and LIKE fallback.
- Update `cmd_mcp_registry sub == "search"` to use SQLite cache when rows > 0, YAML fallback when rows == 0.
- Update `cmd_mcp_registry sub == "list"` similarly.
- Add `--category`, `--transport`, `--limit` flags to search.
- Unit tests: `test_fts5_search`, `test_fts5_fallback`, `test_yaml_fallback_when_empty`.
- Performance test: `test_fts5_search_latency` (10,000 mock rows, p99 < 100 ms).

**Exit criteria:** `tag mcp registry search "calendar scheduling"` after a 50-entry limit sync returns relevant results; LIKE fallback triggered when FTS5 table empty passes CI.

### Phase 4 — Install Abstraction + add-curated (Day 4)

- Implement `_build_mcp_config_block()` with npm/pypi/docker/remote priority logic and SSE rejection.
- Refactor existing `cmd_mcp_registry sub == "install"` to use `_build_mcp_config_block()` and resolve server name from SQLite cache (falling back to YAML).
- Implement tool budget check: sum `tool_count` for profile's current MCP servers + incoming server.
- Implement `add-curated` subcommand.
- Unit tests: all install config block tests, tool budget warning/force tests, SSE skip test, `test_add_curated_idempotent`.

**Exit criteria:** `tag mcp registry install mcp-filesystem --dry-run` produces correct `npx` block; `install playwright-mcp` on a profile near the 40-tool limit fires warning.

### Phase 5 — `tag doctor` Integration + Polish (Day 5)

- Add `mcp_registry` check to `_cmd_doctor`.
- Wire `mcp_registry_sync_log` freshness and error checks.
- Integration tests: `test_live_registry_first_page` (network), `test_doctor_stale_registry_warning`.
- Full CLI surface smoke test (all acceptance criteria).
- Documentation update in `README` (one paragraph on registry sync).

**Exit criteria:** All 18 acceptance criteria pass; `tag doctor` shows green `mcp_registry` check after a successful sync.
