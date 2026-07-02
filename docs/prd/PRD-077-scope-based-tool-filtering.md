# PRD-077: Scope-Based Tool Filtering + Schema Transformation (`tag mcp filter`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `internal/tool`, `internal/cli`
**Depends on:** PRD-014 (MCP Server Registry), PRD-026 (Vector-Based Tool Retrieval), PRD-027 (Eval Framework), PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing & Observability), PRD-034 (Security / Secret Scanning), PRD-001 (Structured Memory Configuration)
**Inspired by:** Composio scope-based filtering, Toolhouse schema processors
**GitHub Issue:** #346

---

## 1. Overview

TAG profiles today expose every tool from every enabled MCP server to the LLM at session start. On a well-stocked developer profile — with GitHub, filesystem, databases, and a browser MCP server all enabled — the raw tool inventory can easily exceed 60 discrete tools. That volume pushes two concrete failure modes: (1) it exceeds the Cursor 40-tool hard limit, causing silent tool truncation at the host layer; (2) even when the host accepts all tools, LLM attention dilutes over the large schema surface, which demonstrably increases tool-selection errors in agent benchmarks.

Scope-based tool filtering addresses this by introducing per-profile allowlists and denylists expressed as glob patterns. A profile author declares exactly which tools are in-scope (`--allow "github:*"`) and which are categorically excluded (`--deny "github:delete_*"`), and TAG enforces that contract at the retrieval layer before the tool list ever reaches the LLM. Filters are stored in SQLite, survive across restarts, compose gracefully with the existing vector-retrieval subsystem (PRD-026), and integrate with the eval framework (PRD-027) for regression testing.

Schema transformation is the complementary layer: MCP tool input schemas are authored by server maintainers, not by the teams using those tools in agent workflows. Field names are often generic (`title`, `body`, `name`) and collide badly when the LLM is calling multiple tools in a single session. Descriptions are frequently missing or too terse to guide model behaviour reliably. Transformation rules let profile authors rename fields to more semantically precise names, inject or overwrite field descriptions, set explicit defaults, and prune fields the LLM should never populate. The rewritten schema is what the LLM sees; the original schema is what gets sent to the MCP server after reverse-mapping the field names back.

Together, filtering and transformation implement a principle from the Composio and Toolhouse ecosystems: the shape of a tool as the LLM experiences it should be intentionally designed for that LLM, not inherited verbatim from the server's generic API surface. This PRD formalises that principle as a first-class TAG feature with CLI ergonomics, a SQLite-backed config store, and a runtime interception layer wired into `internal/tool`.

The feature is rated Difficulty 3/5 because the interception point in `internal/tool` is well-understood, SQLite DDL is straightforward, and glob matching via `gobwas/glob` is trivially portable. Impact is 2/5 in the short term — it is a power-user workflow — but grows meaningfully as the MCP ecosystem proliferates and agent quality requirements tighten.

---

## 2. Problem Statement

### 2.1 Tool Count Explosion and Silent Truncation

The MCP ecosystem has matured to the point where a single server can expose dozens of tools. The `mcp-playwright` server exposes 25 tools alone. A `coder` profile with GitHub, filesystem, Playwright, and a database server enabled is already at 50–60 tools. Cursor silently truncates beyond 40. Claude's context window charges for every tool schema in the system prompt regardless of whether the tool is needed for the current task. There is no mechanism in TAG today to trim this explosion per-profile without editing the MCP server configuration itself, which affects all profiles simultaneously.

### 2.2 LLM Confusion From Overly Generic Schemas

MCP tool schemas are designed for programmatic correctness, not LLM comprehension. The `github:create_issue` tool has a field named `title` — fine in isolation, but when the same session includes `github:create_pull_request` (also with `title`) and `github:create_release` (also with `title`), the model sees three identical field names across three semantically different concepts. Benchmark data from Toolhouse's schema processor work shows that field renaming alone reduces tool-call argument errors by 15–30% on tasks involving multiple tools from the same server. There is no TAG mechanism today to apply such renames without forking the MCP server.

### 2.3 No Audit Trail for Tool Access Control Decisions

In enterprise and security-sensitive deployments, operators need to know exactly which tools each profile can reach. Today, tool access is implicitly defined by which MCP servers are enabled, with no fine-grained record. There is no `tag mcp filter list` that shows an auditor what the `coder` profile can and cannot call. The lack of explicit scope declarations also means there is no input for the security scanner (PRD-034) to reason about tool-level access control.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Per-profile allowlist/denylist of tools by glob pattern (`server:tool_name` or `server:*`), stored in SQLite and applied at retrieval time. |
| G2 | Precedence semantics: explicit deny beats allow; the empty allowlist means all tools from enabled servers are permitted (opt-out model). |
| G3 | Schema transformation rules per tool: field rename, description inject/overwrite, field removal, and default value injection, with automatic reverse-mapping before MCP call dispatch. |
| G4 | CLI surface: `tag mcp filter add/remove/list/clear` and `tag mcp transform add/remove/list/clear` with `--profile` scoping on all commands. |
| G5 | `--json` output on all read commands for scripting and CI consumption. |
| G6 | Integration with `internal/tool`: `SearchTools()` and `KeywordSearchTools()` apply active scope filters before returning results to callers. |
| G7 | Integration with the eval framework (PRD-027): `tag eval run` can pass `--filter-profile <name>` to test agent behaviour under a specific filter configuration. |
| G8 | `tag mcp filter audit --profile <name>` emits a machine-readable JSON report listing every tool that would be visible to the LLM, suitable for security review. |
| G9 | Dry-run flag: `tag mcp filter add --dry-run` shows which tools the new rule would admit or remove from the current active set, without persisting. |
| G10 | Import/export: `tag mcp filter export --profile coder > coder-filters.json` and `tag mcp filter import --profile coder < coder-filters.json` for profile portability. |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Runtime enforcement in MCP server processes themselves. TAG enforces filters at the retrieval/presentation layer; it does not intercept or block actual MCP protocol messages after tool dispatch. |
| NG2 | Automatic filter recommendation from usage analytics. This PRD defines the storage and enforcement layer; smart suggestion of filters is a future feature. |
| NG3 | Cross-user or organisation-level filter policies. Filters are per-profile on the local TAG installation. Multi-tenant policy enforcement is out of scope. |
| NG4 | Filter inheritance or profile extends chains. A profile's filter set is defined entirely by its own rules, not inherited from a parent profile. |
| NG5 | Schema transformation of tool *output* (response) schemas. Only input schemas presented to the LLM are transformed; response handling is unchanged. |
| NG6 | Automatic schema transformation generation via LLM. Transforms are declared manually by the profile author; no AI-assisted generation in this PRD. |
| NG7 | Synchronisation of filter state with remote Hermes instances. Filters live in the local `tag.sqlite3`; remote state is out of scope. |

---

## 5. Success Metrics

| Metric | Baseline | Target | Measurement Method |
|--------|----------|--------|-------------------|
| Active tool count at session start for profiles with filters | 50–70 (unconstrained) | Operator-defined ceiling (e.g. ≤ 40 for Cursor profiles) | `tag mcp filter audit --json | jq '.visible_count'` |
| Tool-call argument error rate (eval suite) | Measured in PRD-027 baseline | ≥ 10% reduction on tasks using renamed fields | `tag eval run` with and without transform rules, same suite |
| Time to apply filter + transform pipeline for 100 tools | — | < 5 ms p99 (in-process, no I/O) | Go benchmark in `internal/tool/filter_test.go` |
| Filter configuration round-trip fidelity | — | 100% — export then import produces identical `tag mcp filter list` output | Integration test |
| Audit report completeness | — | 0 false negatives — every visible tool in audit, every denied tool excluded | Property test over 1,000 random tool/pattern combinations |
| `tag doctor` reports correct filter count per profile | — | Filter counts match `tag mcp filter list --json | jq length` | CI integration test |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Profile author | run `tag mcp filter add --profile coder --allow "github:*" --deny "github:delete_*"` | the coder profile can use all GitHub tools except destructive delete operations |
| U2 | Platform engineer | run `tag mcp filter audit --profile coder --json` | I can include the tool access manifest in a security review without running the agent |
| U3 | Developer | run `tag mcp filter add --profile coder --deny "playwright:*" --dry-run` | I can preview which tools would disappear before committing the change |
| U4 | Profile author | run `tag mcp transform add --tool "github:create_issue" --rename "title:issue_title" --desc "body:Markdown body of the issue (max 65535 chars)"` | the LLM calls this tool with semantically unambiguous field names |
| U5 | Developer | run `tag mcp filter list --profile coder --json` | I can pipe the filter config into a diff tool to track changes across profile versions |
| U6 | Platform engineer | run `tag mcp filter export --profile coder > coder-filters.json && tag mcp filter import --profile staging < coder-filters.json` | I can clone a filter configuration from production to a staging profile |
| U7 | DevOps engineer | run `tag mcp filter add --profile ci --allow "filesystem:read_file" --allow "filesystem:list_directory"` | the CI agent profile has read-only filesystem access, with all other tools implicitly denied |
| U8 | Developer | run `tag mcp transform list --profile coder --json` | I can review all active schema transforms before a major session, ensuring they match the current server schemas |
| U9 | Team lead | run `tag eval run --suite evals/coding.yaml --profile coder` after adding rename transforms | I get objective eval scores showing whether the schema rename improved tool-call accuracy |
| U10 | Developer | run `tag mcp filter clear --profile scratch` | I can reset a profile's filter configuration to the default pass-through state with one command |

---

## 7. Proposed CLI Surface

### 7.1 `tag mcp filter add`

Add one or more allow/deny rules to a profile's filter configuration.

```
tag mcp filter add \
  --profile <name> \
  [--allow <pattern>] \
  [--deny <pattern>] \
  [--priority <int>] \
  [--dry-run] \
  [--json]
```

- `--profile`: Target profile (required). Must exist in `~/.tag/profiles/`.
- `--allow <pattern>`: Glob pattern in `server:tool_name` format. `*` is a wildcard matching any character sequence within a single segment. `server:*` matches all tools from a server. Can be repeated.
- `--deny <pattern>`: Same format as `--allow`. Deny rules take precedence over allow rules at equal priority. Can be repeated.
- `--priority <int>`: Rule evaluation priority (lower = higher precedence). Default: 100. Useful for inserting override rules without reordering existing ones.
- `--dry-run`: Compute the effective visible tool set before and after the rule addition; print a diff. Do not write to SQLite.
- `--json`: Output the newly created rule records as JSON.

**Example:**

```bash
$ tag mcp filter add --profile coder \
    --allow "github:*" \
    --deny "github:delete_*" \
    --deny "github:archive_*"

Profile: coder
Added 3 filter rule(s):
  [allow] github:*           priority=100
  [deny]  github:delete_*    priority=100
  [deny]  github:archive_*   priority=100

Effective tool count: 47 → 31 (removed 16 tools)
```

**Dry-run example:**

```bash
$ tag mcp filter add --profile coder --deny "playwright:*" --dry-run

DRY RUN — no changes written
Would remove 25 tool(s):
  - playwright:browser_close
  - playwright:browser_new_page
  - playwright:page_click
  ... (22 more)
Effective tool count: 31 → 6
```

---

### 7.2 `tag mcp filter remove`

Remove specific filter rules by rule ID.

```
tag mcp filter remove \
  --profile <name> \
  --rule-id <id> [--rule-id <id> ...] \
  [--all] \
  [--json]
```

- `--rule-id`: Integer rule ID from `tag mcp filter list`. Can be repeated.
- `--all`: Remove all filter rules for the profile (equivalent to `tag mcp filter clear`).

---

### 7.3 `tag mcp filter list`

Show all active filter rules for a profile, in evaluation order.

```
tag mcp filter list \
  --profile <name> \
  [--json]
```

**Plain-text output:**

```
Profile: coder   (3 rules)

ID   TYPE   PATTERN              PRIORITY  CREATED
1    allow  github:*             100       2026-06-12T09:00:00Z
2    deny   github:delete_*      100       2026-06-12T09:00:00Z
3    deny   github:archive_*     100       2026-06-12T09:00:00Z
```

**JSON output (`--json`):**

```json
[
  {"id": 1, "profile": "coder", "type": "allow", "pattern": "github:*", "priority": 100, "created_at": "2026-06-12T09:00:00Z"},
  {"id": 2, "profile": "coder", "type": "deny",  "pattern": "github:delete_*", "priority": 100, "created_at": "2026-06-12T09:00:00Z"},
  {"id": 3, "profile": "coder", "type": "deny",  "pattern": "github:archive_*", "priority": 100, "created_at": "2026-06-12T09:00:00Z"}
]
```

---

### 7.4 `tag mcp filter clear`

Remove all filter rules for a profile (returns to unrestricted pass-through).

```
tag mcp filter clear --profile <name> [--yes]
```

---

### 7.5 `tag mcp filter audit`

Compute and display the full effective tool set for a profile after all filters are applied.

```
tag mcp filter audit \
  --profile <name> \
  [--server <server>] \
  [--show-denied] \
  [--json]
```

- `--server`: Restrict audit to tools from a specific server name.
- `--show-denied`: Include a `denied_tools` section in the output listing every tool that would be blocked.

**JSON output:**

```json
{
  "profile": "coder",
  "evaluated_at": "2026-06-12T09:15:00Z",
  "total_available": 47,
  "visible_count": 31,
  "denied_count": 16,
  "visible_tools": [
    {"server": "github", "name": "create_issue", "qualified": "github:create_issue"},
    ...
  ],
  "denied_tools": [
    {"server": "github", "name": "delete_repository", "qualified": "github:delete_repository", "denied_by": "github:delete_*"},
    ...
  ]
}
```

---

### 7.6 `tag mcp filter export` / `tag mcp filter import`

```bash
tag mcp filter export --profile coder > coder-filters.json
tag mcp filter import --profile staging --file coder-filters.json [--merge | --replace]
```

- `--merge`: Append imported rules to existing rules (default).
- `--replace`: Delete all existing rules for the target profile before importing.

---

### 7.7 `tag mcp transform add`

Add a schema transformation rule for a specific tool.

```
tag mcp transform add \
  --profile <name> \
  --tool <server:tool_name> \
  [--rename <old_field:new_field>] \
  [--desc "<field:New description text>"] \
  [--remove <field_name>] \
  [--default "<field:json_value>"] \
  [--json]
```

- `--rename <old:new>`: Rename `old_field` to `new_field` in the schema presented to the LLM. Multiple `--rename` flags allowed.
- `--desc "<field:text>"`: Inject or overwrite the `description` property of `field` in the JSON Schema. Text after the first `:` is the full description.
- `--remove <field>`: Remove a field entirely from the schema presented to the LLM. The field is still sent to the MCP server with its original name if the LLM omits it (server defaults apply).
- `--default "<field:json_value>"`: Inject a JSON Schema `default` annotation for `field`. Value must be valid JSON.

**Example:**

```bash
$ tag mcp transform add \
    --profile coder \
    --tool "github:create_issue" \
    --rename "title:issue_title" \
    --rename "body:issue_body" \
    --desc "issue_title:Short one-line summary of the issue (max 256 chars, no markdown)" \
    --remove "assignees" \
    --default "labels:[\"bug\"]"

Transform added for github:create_issue (profile: coder)
  rename:  title → issue_title
  rename:  body  → issue_body
  desc:    issue_title = "Short one-line summary..."
  remove:  assignees
  default: labels = ["bug"]
```

---

### 7.8 `tag mcp transform list`

```
tag mcp transform list \
  [--profile <name>] \
  [--tool <server:tool_name>] \
  [--json]
```

Lists all active transform rules, optionally filtered to a profile and/or specific tool.

---

### 7.9 `tag mcp transform remove`

```
tag mcp transform remove \
  --profile <name> \
  --tool <server:tool_name> \
  [--all] \
  [--rule-id <id>]
```

---

### 7.10 `tag mcp transform test`

Dry-run a transform against a live tool schema fetched from the server.

```
tag mcp transform test \
  --profile <name> \
  --tool <server:tool_name> \
  [--json]
```

Fetches the raw schema from the MCP server, applies all active transform rules for the tool in the given profile, and prints before/after JSON Schema side by side (or as a JSON diff with `--json`).

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag mcp filter add` MUST persist each `--allow` and `--deny` flag as a separate row in the `tool_scope_rules` table with `profile`, `rule_type` (`allow`/`deny`), `pattern`, `priority`, and `created_at` columns. |
| FR-02 | Pattern matching MUST use `gobwas/glob` semantics on the `server:tool_name` qualified string. `*` matches any sequence of characters except `:`. Patterns without `:` are rejected with exit code 1 and an error message. Glob patterns are compiled once at `ScopeFilter` load time and cached for the life of the session. |
| FR-03 | Filter precedence MUST be: (1) explicit `deny` with lower priority number; (2) explicit `allow` with lower priority number; (3) at equal priority, `deny` beats `allow`; (4) if no rules match, default is ALLOW (opt-out model). |
| FR-04 | When at least one `--allow` pattern exists for a profile, the default for non-matching tools MUST flip to DENY (opt-in model). The effective model (opt-in vs opt-out) is computed dynamically from the presence of any allow rule and MUST be displayed in `tag mcp filter list` output. |
| FR-05 | `internal/tool.SearchTools()` and `KeywordSearchTools()` MUST accept an optional `*ScopeFilter` parameter (nil = passthrough). When non-nil, the filter is applied to the candidate list before scoring and before returning results. |
| FR-06 | `internal/tool.ApplyScopeFilter(tools []Tool, f *ScopeFilter) []Tool` MUST be a pure function (no I/O, no side effects) that returns the filtered slice. It MUST complete in < 1 ms for inputs up to 200 tools. |
| FR-07 | Schema transformation MUST be applied in this order: (1) field removal; (2) field rename (in the JSON Schema `properties` map and `required` slice); (3) description injection; (4) default injection. Applying steps in a different order MUST NOT produce an equivalent result when the same field is both renamed and has a description injected (the description should apply to the new name). |
| FR-08 | `internal/tool.ReverseTransform(callArgs map[string]any, tx *ToolTransform) map[string]any` MUST reconstruct original field names from renamed ones before the call is dispatched to the MCP server. It MUST be idempotent (calling it twice on an already-reversed map returns the same result). |
| FR-09 | `tag mcp filter audit` MUST enumerate tools by reading the MCP registry YAML at `~/.tag/mcp-registry.yaml` (same source as PRD-014) and applying active filter rules. It MUST NOT require a running MCP server. |
| FR-10 | `tag mcp filter audit --show-denied` output MUST include, for each denied tool, the specific rule pattern that caused the denial (`denied_by` field). |
| FR-11 | `--dry-run` on `tag mcp filter add` MUST NOT write any rows to SQLite. It MUST print the before/after tool count and a list of tools that would be added to or removed from the visible set. |
| FR-12 | `tag mcp filter export` MUST produce a JSON document that, when piped to `tag mcp filter import --replace`, produces an identical `tag mcp filter list --json` output. Round-trip fidelity is required for both filter rules and transform rules. |
| FR-13 | `tag mcp transform add` MUST validate that `--rename` pairs do not create duplicate field names in the output schema. If a collision is detected, exit code 1 with message `"Rename collision: field '<name>' appears more than once after transforms"`. |
| FR-14 | `tag mcp transform test` MUST fetch the tool schema from the configured MCP server using the same transport mechanism used at session start (stdio/streamable-http as configured in the profile, via `internal/mcp`). It MUST time out after 10 seconds and exit 1 with a clear error if the server is unreachable. |
| FR-15 | All `--json` outputs MUST be valid JSON (parseable by `encoding/json`) and MUST include an `"as_of"` ISO-8601 timestamp field at the top level. |
| FR-16 | `tag mcp filter clear --yes` MUST delete all rows in `tool_scope_rules` for the given profile and all rows in `tool_transforms` for the given profile in a single transaction. |
| FR-17 | `tag doctor` (existing command in `internal/cli`) MUST be extended to report the filter rule count and transform count per profile as informational entries. No warning threshold is enforced — these are purely informational. |
| FR-18 | All filter and transform operations MUST be scoped to a profile. Operations without `--profile` MUST exit 1 with the message `"--profile is required for this command"`. |
| FR-19 | `tag mcp transform add --remove <field>` MUST record removed fields in `tool_transforms.removed_fields` (JSON array). During `ReverseTransform`, removed fields MUST NOT be injected back into call arguments — the server receives only what the LLM provides under the original name. |
| FR-20 | `ScopeFilter`, `ToolTransform`, and all filter/transform functions MUST live in `internal/tool`, not in `internal/cli`. CLI handlers in `internal/cli` call exported functions from `internal/tool`; no filter logic lives in the command handler. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Performance:** The full filter + transform pipeline for 100 tools MUST complete in < 5 ms p99 measured in a Go benchmark (`BenchmarkApplyScopeFilter` in `internal/tool/filter_test.go`). Glob patterns are compiled once at `ScopeFilter` load time; no pattern compilation in the hot path. |
| NFR-02 | **SQLite single-writer:** All writes to `tool_scope_rules` and `tool_transforms` MUST go through the single-writer `*sql.DB` opened via `internal/store.Open()`, which uses `modernc.org/sqlite` (CGO_ENABLED=0), WAL mode, and `gofrs/flock` for cross-process mutual exclusion. No raw `sql.Open("sqlite3", …)` calls outside `internal/store`. |
| NFR-03 | **Backward compatibility:** Profiles with no filter rules MUST behave identically to today — all tools from enabled servers are visible. A `ScopeFilter` with an empty `Rules` slice MUST be a no-op pass-through. |
| NFR-04 | **No new mandatory runtime dependencies beyond go.mod:** The filter and transform subsystem MUST work with `gobwas/glob` (already planned for the Go stack), `modernc.org/sqlite` (already planned), and `encoding/json` (stdlib). No additional modules are required. |
| NFR-05 | **Atomicity of multi-rule adds:** When `tag mcp filter add` specifies multiple `--allow` and `--deny` flags, all rules MUST be inserted in a single database transaction. A failure mid-insert MUST roll back all rules from that invocation. |
| NFR-06 | **TTY vs. pipe output:** All list/audit commands MUST detect whether stdout is a TTY. TTY: Rich table rendering with colour. Non-TTY: tab-separated plain text. `--json` overrides both. |
| NFR-07 | **Pattern validation on input:** Patterns containing characters that are illegal in `server:tool_name` qualified names (whitespace, semicolons, quotes) MUST be rejected at parse time with exit code 1 before any SQLite writes. |
| NFR-08 | **Tracing integration (PRD-013):** When tracing is enabled, `ApplyScopeFilter()` MUST emit an OTel span via `go.opentelemetry.io/otel` with attributes: `tag.filter.profile`, `tag.filter.input_count`, `tag.filter.output_count`, `tag.filter.rule_count`. The `context.Context` carrying the OTel span is threaded through via typed context keys defined in `internal/tool`. |
| NFR-09 | **Security:** Transform rules MUST NOT allow injection of arbitrary executable content. The `--default` value MUST be validated as well-formed JSON via `encoding/json`; any value whose decoded type contains map keys matching `__class__`, `__reduce__`, or `__import__` MUST be rejected. |
| NFR-10 | **Idempotent re-add:** Running `tag mcp filter add` with an identical pattern that already exists for the same profile MUST be a no-op (no duplicate row, enforced by the `UNIQUE (profile, rule_type, pattern)` constraint). Exit 0 with message `"Rule already exists (id=<N>), no change made"`. |

---

## 10. Technical Design

### 10.1 Package Layout

All filter and transform logic lives in `internal/tool`. CLI command handlers live in `internal/cli`. No filter logic is permitted in `internal/cli`.

```
internal/
  tool/
    filter.go        — ScopeRule, ScopeFilter, IsVisible, DenyingRule, compiled glob cache
    transform.go     — ToolTransform, ApplyTransform, ReverseTransform
    store.go         — LoadScopeFilter, LoadTransforms, EnsureFilterSchema
    filter_test.go   — unit + benchmark tests
    transform_test.go
  cli/
    mcp_filter.go    — handlers: add, remove, list, clear, audit, export, import
    mcp_transform.go — handlers: add, remove, list, test
  store/
    db.go            — Open() returning *sql.DB via modernc.org/sqlite + WAL + gofrs/flock
```

No new source files outside this layout are required; all additions are purely additive.

### 10.2 SQLite DDL

The migration runs inside `EnsureFilterSchema(db *sql.DB) error` in `internal/tool/store.go`, called idempotently at startup via `internal/store.Open()`.

```sql
CREATE TABLE IF NOT EXISTS tool_scope_rules (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    profile     TEXT    NOT NULL,
    rule_type   TEXT    NOT NULL CHECK (rule_type IN ('allow', 'deny')),
    pattern     TEXT    NOT NULL,
    priority    INTEGER NOT NULL DEFAULT 100,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (profile, rule_type, pattern)
);

CREATE INDEX IF NOT EXISTS idx_tool_scope_rules_profile
    ON tool_scope_rules (profile);

CREATE TABLE IF NOT EXISTS tool_transforms (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    profile         TEXT    NOT NULL,
    tool_qualified  TEXT    NOT NULL,  -- "server:tool_name"
    renames         TEXT    NOT NULL DEFAULT '{}',   -- JSON: {"old_field": "new_field", ...}
    descriptions    TEXT    NOT NULL DEFAULT '{}',   -- JSON: {"field_name": "description text", ...}
    removed_fields  TEXT    NOT NULL DEFAULT '[]',   -- JSON array of field name strings
    defaults        TEXT    NOT NULL DEFAULT '{}',   -- JSON: {"field_name": <json_value>, ...}
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (profile, tool_qualified)
);

CREATE INDEX IF NOT EXISTS idx_tool_transforms_profile_tool
    ON tool_transforms (profile, tool_qualified);
```

### 10.3 Core Types (`internal/tool/filter.go`)

```go
package tool

import (
    "context"
    "sort"
    "time"

    "github.com/gobwas/glob"
)

// contextKey is unexported to avoid collisions with other packages.
type contextKey string

const (
    scopeFilterKey contextKey = "tag.scope_filter"
    transformsKey  contextKey = "tag.transforms"
)

// ScopeRule is a single allow/deny glob rule for a profile.
type ScopeRule struct {
    ID        int64
    Profile   string
    RuleType  string    // "allow" | "deny"
    Pattern   string    // e.g. "github:*" or "github:delete_*"
    Priority  int
    CreatedAt time.Time

    compiled glob.Glob // compiled once at load time; not serialised
}

// ScopeFilter holds the complete filter configuration for a profile,
// loaded from SQLite. Glob patterns are pre-compiled at load time.
type ScopeFilter struct {
    Profile string
    Rules   []ScopeRule
}

// IsPassthrough reports whether no rules are defined (all tools pass through).
func (f *ScopeFilter) IsPassthrough() bool { return len(f.Rules) == 0 }

// HasAnyAllow reports whether the filter contains at least one allow rule,
// which switches the default from opt-out (allow) to opt-in (deny).
func (f *ScopeFilter) HasAnyAllow() bool {
    for _, r := range f.Rules {
        if r.RuleType == "allow" {
            return true
        }
    }
    return false
}

// IsVisible evaluates whether a qualified tool name (e.g. "github:create_issue")
// is visible under this filter.
//
// Precedence (evaluated in priority order, lower number = higher precedence):
//  1. Deny at a lower priority number → DENY wins immediately.
//  2. Allow at a lower priority number → ALLOW wins immediately.
//  3. At equal priority → deny beats allow.
//  4. No matching rule:
//     - If any allow rule exists → DENY (opt-in model).
//     - Otherwise → ALLOW (opt-out model).
func (f *ScopeFilter) IsVisible(qualified string) bool {
    if f.IsPassthrough() {
        return true
    }

    sorted := make([]ScopeRule, len(f.Rules))
    copy(sorted, f.Rules)
    // Sort by priority ASC; at equal priority, "deny" > "allow" lexically.
    sort.Slice(sorted, func(i, j int) bool {
        if sorted[i].Priority != sorted[j].Priority {
            return sorted[i].Priority < sorted[j].Priority
        }
        return sorted[i].RuleType > sorted[j].RuleType // "deny" before "allow"
    })

    var allowAt, denyAt *int
    for _, r := range sorted {
        if r.compiled == nil || !r.compiled.Match(qualified) {
            continue
        }
        p := r.Priority
        switch r.RuleType {
        case "deny":
            if denyAt == nil || p < *denyAt {
                denyAt = &p
            }
        case "allow":
            if allowAt == nil || p < *allowAt {
                allowAt = &p
            }
        }
    }

    if denyAt != nil && allowAt != nil {
        if *denyAt < *allowAt {
            return false
        }
        if *allowAt < *denyAt {
            return true
        }
        return false // equal priority → deny wins
    }
    if denyAt != nil {
        return false
    }
    if allowAt != nil {
        return true
    }
    // No matching rule.
    return !f.HasAnyAllow()
}

// DenyingRule returns the highest-precedence deny rule that blocks the tool, or nil.
func (f *ScopeFilter) DenyingRule(qualified string) *ScopeRule {
    sorted := make([]ScopeRule, len(f.Rules))
    copy(sorted, f.Rules)
    sort.Slice(sorted, func(i, j int) bool {
        return sorted[i].Priority < sorted[j].Priority
    })
    for i, r := range sorted {
        if r.RuleType == "deny" && r.compiled != nil && r.compiled.Match(qualified) {
            return &sorted[i]
        }
    }
    return nil
}

// WithScopeFilter stores f in ctx under the TAG scope-filter key.
func WithScopeFilter(ctx context.Context, f *ScopeFilter) context.Context {
    return context.WithValue(ctx, scopeFilterKey, f)
}

// ScopeFilterFromContext retrieves the ScopeFilter stored by WithScopeFilter.
func ScopeFilterFromContext(ctx context.Context) (*ScopeFilter, bool) {
    f, ok := ctx.Value(scopeFilterKey).(*ScopeFilter)
    return f, ok
}
```

### 10.4 Core Types (`internal/tool/transform.go`)

```go
package tool

import "time"

// ToolTransform holds schema transformation rules for a single tool in a profile.
type ToolTransform struct {
    ID            int64
    Profile       string
    ToolQualified string            // "server:tool_name"
    Renames       map[string]string // {"old_field": "new_field"}
    Descriptions  map[string]string // {"field_name": "description text"}
    RemovedFields []string
    Defaults      map[string]any    // {"field_name": json_value}
    CreatedAt     time.Time
    UpdatedAt     time.Time
}
```

### 10.5 Core Algorithms

#### `ApplyScopeFilter` (`internal/tool/filter.go`)

```go
import (
    "context"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
)

// ApplyScopeFilter returns the subset of tools visible under f.
// Pure function — no I/O, no side effects.
// When f is nil or has no rules, it returns a copy of tools unchanged.
func ApplyScopeFilter(ctx context.Context, tools []Tool, f *ScopeFilter) []Tool {
    ctx, span := otel.Tracer("tag").Start(ctx, "tag.tool_filter.apply")
    defer span.End()

    if f != nil {
        span.SetAttributes(
            attribute.String("tag.filter.profile", f.Profile),
            attribute.Int("tag.filter.input_count", len(tools)),
            attribute.Int("tag.filter.rule_count", len(f.Rules)),
        )
    }

    if f == nil || f.IsPassthrough() {
        result := make([]Tool, len(tools))
        copy(result, tools)
        span.SetAttributes(attribute.Int("tag.filter.output_count", len(result)))
        return result
    }

    result := make([]Tool, 0, len(tools))
    for _, t := range tools {
        if f.IsVisible(t.Qualified()) {
            result = append(result, t)
        }
    }
    span.SetAttributes(attribute.Int("tag.filter.output_count", len(result)))
    return result
}
```

#### `ApplyTransform` (`internal/tool/transform.go`)

```go
import "encoding/json"

// ApplyTransform returns a new Tool with transform rules applied.
// Steps execute in order: remove → rename → describe → default.
// Does not mutate src.
func ApplyTransform(src Tool, tx *ToolTransform) (Tool, error) {
    if tx == nil {
        return src, nil
    }
    // Deep-copy the input schema via JSON round-trip.
    raw, err := json.Marshal(src.InputSchema)
    if err != nil {
        return src, err
    }
    var props map[string]any
    if err := json.Unmarshal(raw, &props); err != nil {
        return src, err
    }

    properties, _ := props["properties"].(map[string]any)
    required, _   := props["required"].([]any)

    // Step 1: remove
    for _, field := range tx.RemovedFields {
        delete(properties, field)
        required = removeString(required, field)
    }

    // Step 2: rename (properties map + required slice)
    for old, new := range tx.Renames {
        if v, ok := properties[old]; ok {
            properties[new] = v
            delete(properties, old)
        }
        required = replaceString(required, old, new)
    }

    // Step 3: inject/overwrite descriptions (post-rename names)
    for field, desc := range tx.Descriptions {
        if entry, ok := properties[field].(map[string]any); ok {
            entry["description"] = desc
        }
    }

    // Step 4: inject defaults (post-rename names)
    for field, def := range tx.Defaults {
        if entry, ok := properties[field].(map[string]any); ok {
            entry["default"] = def
        }
    }

    props["properties"] = properties
    props["required"]   = required
    dst := src // shallow copy; replace InputSchema
    dst.InputSchema = props
    return dst, nil
}
```

#### `ReverseTransform` (`internal/tool/transform.go`)

```go
// ReverseTransform converts LLM-visible field names (post-transform) back to
// original names before dispatching to the MCP server.
// Idempotent: calling twice on an already-reversed map is safe.
// Fields in RemovedFields that appear in callArgs are silently dropped.
func ReverseTransform(callArgs map[string]any, tx *ToolTransform) map[string]any {
    if tx == nil {
        return callArgs
    }
    // Build reverse rename map: new_name → old_name.
    rev := make(map[string]string, len(tx.Renames))
    for old, new := range tx.Renames {
        rev[new] = old
    }
    // Build removed-field set for O(1) lookup.
    removed := make(map[string]struct{}, len(tx.RemovedFields))
    for _, f := range tx.RemovedFields {
        removed[f] = struct{}{}
    }

    result := make(map[string]any, len(callArgs))
    for k, v := range callArgs {
        original := k
        if mapped, ok := rev[k]; ok {
            original = mapped
        }
        if _, isRemoved := removed[original]; isRemoved {
            continue // silently drop fields the profile author blocked
        }
        result[original] = v
    }
    return result
}
```

#### `LoadScopeFilter` and `LoadTransforms` (`internal/tool/store.go`)

```go
import (
    "database/sql"
    "encoding/json"
    "time"

    "github.com/gobwas/glob"
)

// LoadScopeFilter reads all scope rules for profile from db into a ScopeFilter.
// Glob patterns are compiled at load time; compilation errors skip the rule and log a warning.
func LoadScopeFilter(db *sql.DB, profile string) (*ScopeFilter, error) {
    rows, err := db.Query(
        `SELECT id, profile, rule_type, pattern, priority, created_at
           FROM tool_scope_rules
          WHERE profile = ?
          ORDER BY priority ASC, rule_type DESC`,
        profile,
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var rules []ScopeRule
    for rows.Next() {
        var r ScopeRule
        var createdAt string
        if err := rows.Scan(&r.ID, &r.Profile, &r.RuleType, &r.Pattern, &r.Priority, &createdAt); err != nil {
            return nil, err
        }
        r.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
        r.compiled, _ = glob.Compile(r.Pattern) // nil on error; IsVisible skips nil
        rules = append(rules, r)
    }
    return &ScopeFilter{Profile: profile, Rules: rules}, rows.Err()
}

// LoadTransforms reads transform rows for a profile, optionally restricted to one tool.
// Pass toolQualified="" to load all transforms for the profile.
func LoadTransforms(db *sql.DB, profile, toolQualified string) ([]*ToolTransform, error) {
    var (
        rows *sql.Rows
        err  error
    )
    if toolQualified != "" {
        rows, err = db.Query(
            `SELECT id, profile, tool_qualified, renames, descriptions, removed_fields, defaults, created_at, updated_at
               FROM tool_transforms WHERE profile=? AND tool_qualified=?`,
            profile, toolQualified,
        )
    } else {
        rows, err = db.Query(
            `SELECT id, profile, tool_qualified, renames, descriptions, removed_fields, defaults, created_at, updated_at
               FROM tool_transforms WHERE profile=? ORDER BY tool_qualified`,
            profile,
        )
    }
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var txs []*ToolTransform
    for rows.Next() {
        var t ToolTransform
        var renames, descriptions, removedFields, defaults string
        var createdAt, updatedAt string
        if err := rows.Scan(
            &t.ID, &t.Profile, &t.ToolQualified,
            &renames, &descriptions, &removedFields, &defaults,
            &createdAt, &updatedAt,
        ); err != nil {
            return nil, err
        }
        _ = json.Unmarshal([]byte(renames), &t.Renames)
        _ = json.Unmarshal([]byte(descriptions), &t.Descriptions)
        _ = json.Unmarshal([]byte(removedFields), &t.RemovedFields)
        _ = json.Unmarshal([]byte(defaults), &t.Defaults)
        t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
        t.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
        txs = append(txs, &t)
    }
    return txs, rows.Err()
}
```

### 10.6 Integration Points

#### 10.6.1 `internal/tool.SearchTools` and `KeywordSearchTools`

Both functions accept an optional `*ScopeFilter` (nil = passthrough). Before returning the final result slice, they call `ApplyScopeFilter(ctx, results, f)`. The filter runs after scoring so vector distances are not distorted by the filter; it is a post-retrieval gate.

#### 10.6.2 Session Startup in `internal/cli`

At session start (wherever tool lists are assembled before the first LLM call), the session handler in `internal/cli` calls:

```go
f, err := tool.LoadScopeFilter(db, profileName)
if err != nil {
    return err
}
txs, err := tool.LoadTransforms(db, profileName, "")
if err != nil {
    return err
}
// Index transforms by qualified name for O(1) lookup.
txByTool := make(map[string]*tool.ToolTransform, len(txs))
for _, tx := range txs {
    txByTool[tx.ToolQualified] = tx
}

visible := tool.ApplyScopeFilter(ctx, rawTools, f)
for i, t := range visible {
    if tx, ok := txByTool[t.Qualified()]; ok {
        visible[i], err = tool.ApplyTransform(t, tx)
        if err != nil {
            return err
        }
    }
}
// visible is now the tool list presented to the LLM.
```

The `*ScopeFilter` is stored in `ctx` via `tool.WithScopeFilter` so downstream subsystems (tracing, audit) can access it without additional parameter threading.

#### 10.6.3 Call Dispatch in `internal/cli`

Before any MCP tool call is dispatched, the handler looks up the transform and calls `tool.ReverseTransform(callArgs, tx)` to restore original field names. If no transform exists for the tool, `callArgs` is passed through unchanged.

```go
tx := txByTool[qualifiedName] // may be nil
restored := tool.ReverseTransform(callArgs, tx)
// dispatch restored to the MCP server via internal/mcp
```

#### 10.6.4 `tag eval` Integration (PRD-027)

`tag eval run` accepts an explicit profile whose filter configuration is active. Because filter/transform state is stored in SQLite and keyed to the profile name, no additional eval-specific integration is needed — eval already runs the agent under the named profile.

#### 10.6.5 MCP Transport (`internal/mcp`)

`tag mcp transform test` fetches the live tool schema via the TAG-owned `internal/mcp` facade (wrapping `github.com/modelcontextprotocol/go-sdk v1.6.1`, MCP protocol version `2025-11-25`). A `context.WithTimeout` of 10 seconds wraps the call; on timeout the command exits 1 with a clear error message.

### 10.7 `EnsureFilterSchema` Migration (`internal/tool/store.go`)

```go
// EnsureFilterSchema runs the idempotent DDL migration for filter and transform tables.
// Called once during internal/store.Open().
func EnsureFilterSchema(db *sql.DB) error {
    _, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS tool_scope_rules (
            id          INTEGER PRIMARY KEY AUTOINCREMENT,
            profile     TEXT    NOT NULL,
            rule_type   TEXT    NOT NULL CHECK (rule_type IN ('allow','deny')),
            pattern     TEXT    NOT NULL,
            priority    INTEGER NOT NULL DEFAULT 100,
            created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
            UNIQUE (profile, rule_type, pattern)
        );
        CREATE INDEX IF NOT EXISTS idx_tool_scope_rules_profile
            ON tool_scope_rules (profile);

        CREATE TABLE IF NOT EXISTS tool_transforms (
            id              INTEGER PRIMARY KEY AUTOINCREMENT,
            profile         TEXT    NOT NULL,
            tool_qualified  TEXT    NOT NULL,
            renames         TEXT    NOT NULL DEFAULT '{}',
            descriptions    TEXT    NOT NULL DEFAULT '{}',
            removed_fields  TEXT    NOT NULL DEFAULT '[]',
            defaults        TEXT    NOT NULL DEFAULT '{}',
            created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
            updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
            UNIQUE (profile, tool_qualified)
        );
        CREATE INDEX IF NOT EXISTS idx_tool_transforms_profile_tool
            ON tool_transforms (profile, tool_qualified);
    `)
    return err
}
```

### 10.8 Export/Import JSON Schema

```json
{
  "version": "1",
  "as_of": "2026-06-12T09:00:00Z",
  "profile": "coder",
  "filter_rules": [
    {"rule_type": "allow", "pattern": "github:*", "priority": 100},
    {"rule_type": "deny",  "pattern": "github:delete_*", "priority": 100}
  ],
  "transforms": [
    {
      "tool_qualified": "github:create_issue",
      "renames": {"title": "issue_title", "body": "issue_body"},
      "descriptions": {"issue_title": "Short one-line summary of the issue"},
      "removed_fields": ["assignees"],
      "defaults": {"labels": ["bug"]}
    }
  ]
}
```

---

## 11. Security Considerations

1. **No executable content in transforms:** The `--default` JSON value is decoded with `encoding/json` and then validated to exclude map keys that could facilitate deserialization attacks (`__class__`, `__reduce__`, `__import__`). Any such key causes exit code 1.

2. **Pattern injection:** Patterns are validated against `^[a-zA-Z0-9_\-.*:]+$` before storage. No shell metacharacters (`;`, `|`, `&`, backtick, `$`) are permitted in patterns, preventing injection if patterns are ever interpolated into shell commands.

3. **Audit trail:** Every write to `tool_scope_rules` and `tool_transforms` is logged via the existing tracing subsystem (PRD-013) with the profile name, operation, and pattern. This provides a tamper-evident log of who changed filter configurations and when.

4. **No filter bypass at call time:** `ReverseTransform` only restores field *names* — it never reconstructs a field that was removed. A field in `RemovedFields` that appears in `callArgs` (e.g. injected by a jailbreak) is silently dropped during reverse transform, not passed to the server.

5. **Deny-on-error:** If `ApplyScopeFilter` encounters a panic or unexpected error, it MUST recover and return an empty tool slice (deny all) rather than returning the unfiltered list. This ensures filter failures are safe-by-default.

6. **SQLite integrity:** The `CHECK (rule_type IN ('allow','deny'))` constraint is enforced at the database layer, not only in application code. Even direct SQLite writes cannot insert an invalid rule type.

7. **No secret exposure in audit output:** `tag mcp filter audit` reads only tool names and descriptions from the MCP registry YAML. It MUST NOT read or display API keys, tokens, or credentials from MCP server configurations.

8. **Profile isolation:** All SQLite queries include a `WHERE profile = ?` clause. There is no cross-profile API; one profile's filter rules cannot be applied to another without an explicit import command.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/tool/filter_test.go`)

```go
package tool_test

import (
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/tag-project/tag/internal/tool"
)

func newRule(id int64, ruleType, pattern string, priority int) tool.ScopeRule {
    r := tool.ScopeRule{ID: id, Profile: "coder", RuleType: ruleType,
        Pattern: pattern, Priority: priority}
    // In production LoadScopeFilter compiles the glob; here we compile inline.
    r = tool.MustCompileRule(r)
    return r
}

func TestAllowStarPattern(t *testing.T) {
    sf := &tool.ScopeFilter{
        Profile: "coder",
        Rules:   []tool.ScopeRule{newRule(1, "allow", "github:*", 100)},
    }
    assert.True(t, sf.IsVisible("github:create_issue"))
    assert.False(t, sf.IsVisible("filesystem:read_file")) // no allow match → deny (opt-in)
}

func TestDenyBeatsAllowEqualPriority(t *testing.T) {
    sf := &tool.ScopeFilter{
        Profile: "coder",
        Rules: []tool.ScopeRule{
            newRule(1, "allow", "github:*", 100),
            newRule(2, "deny", "github:delete_*", 100),
        },
    }
    assert.True(t, sf.IsVisible("github:create_issue"))
    assert.False(t, sf.IsVisible("github:delete_repository"))
}

func TestLowerPriorityDenyBeatsHigherPriorityAllow(t *testing.T) {
    sf := &tool.ScopeFilter{
        Profile: "coder",
        Rules: []tool.ScopeRule{
            newRule(1, "allow", "github:*", 50),
            newRule(2, "deny", "github:*", 10),
        },
    }
    assert.False(t, sf.IsVisible("github:create_issue"))
}

func TestEmptyFilterIsPassthrough(t *testing.T) {
    sf := &tool.ScopeFilter{Profile: "coder"}
    assert.True(t, sf.IsVisible("any:tool"))
}

func TestApplyScopeFilterPure(t *testing.T) {
    tools := []tool.Tool{
        tool.NewTool("github", "create_issue", nil),
        tool.NewTool("github", "delete_repository", nil),
    }
    sf := &tool.ScopeFilter{
        Profile: "coder",
        Rules:   []tool.ScopeRule{newRule(1, "deny", "github:delete_*", 100)},
    }
    result := tool.ApplyScopeFilter(t.Context(), tools, sf)
    require.Len(t, result, 1)
    assert.Equal(t, "create_issue", result[0].Name)
}
```

### 12.2 Transform Unit Tests (`internal/tool/transform_test.go`)

```go
func TestApplyTransformRenameThenDescribe(t *testing.T) {
    src := tool.NewTool("github", "create_issue", map[string]any{
        "properties": map[string]any{"title": map[string]any{"type": "string"}},
        "required":   []any{"title"},
    })
    tx := &tool.ToolTransform{
        ToolQualified: "github:create_issue",
        Renames:       map[string]string{"title": "issue_title"},
        Descriptions:  map[string]string{"issue_title": "The issue heading"},
    }
    result, err := tool.ApplyTransform(src, tx)
    require.NoError(t, err)
    props := result.InputSchema["properties"].(map[string]any)
    assert.Contains(t, props, "issue_title")
    assert.NotContains(t, props, "title")
    assert.Equal(t, "The issue heading", props["issue_title"].(map[string]any)["description"])
    req := result.InputSchema["required"].([]any)
    assert.Contains(t, req, "issue_title")
}

func TestReverseTransformIdempotent(t *testing.T) {
    tx := &tool.ToolTransform{
        ToolQualified: "github:create_issue",
        Renames:       map[string]string{"title": "issue_title"},
    }
    args := map[string]any{"issue_title": "My bug", "body": "Details"}
    reversed1 := tool.ReverseTransform(args, tx)
    reversed2 := tool.ReverseTransform(reversed1, tx)
    assert.Equal(t, map[string]any{"title": "My bug", "body": "Details"}, reversed1)
    assert.Equal(t, reversed1, reversed2) // idempotent
}

func TestRemovedFieldNotPassedThrough(t *testing.T) {
    tx := &tool.ToolTransform{
        ToolQualified: "github:create_issue",
        RemovedFields: []string{"assignees"},
    }
    args := map[string]any{"title": "Bug", "assignees": []any{"attacker"}}
    result := tool.ReverseTransform(args, tx)
    assert.NotContains(t, result, "assignees")
}
```

### 12.3 Integration Tests

- **`TestFilterCLIAddListRoundTrip`**: Calls the filter-add handler against a temporary `modernc.org/sqlite` DB, then filter-list with `--json`, asserts JSON contains inserted rules.
- **`TestFilterExportImportReplace`**: Export → import with `--replace` → compare list output for equality.
- **`TestDryRunNoWrites`**: Assert `tool_scope_rules` row count is unchanged after a `--dry-run` invocation.
- **`TestTransformAddCollisionRejected`**: Calling the transform-add handler with two `--rename` flags mapping different source fields to the same target name returns exit code 1.
- **`TestFilterAuditDeniedByField`**: After adding a deny rule, audit with `--show-denied --json` confirms `denied_by` matches the rule pattern.

### 12.4 Performance Benchmarks (`internal/tool/filter_test.go`)

```go
func BenchmarkApplyScopeFilter200Tools(b *testing.B) {
    tools := make([]tool.Tool, 200)
    for i := range tools {
        tools[i] = tool.NewTool("github", fmt.Sprintf("tool_%d", i), nil)
    }
    rules := make([]tool.ScopeRule, 10)
    for i := range rules {
        rules[i] = newRule(int64(i), "deny", fmt.Sprintf("github:tool_%d0*", i), 100)
    }
    sf := &tool.ScopeFilter{Profile: "bench", Rules: rules}
    ctx := context.Background()
    b.ResetTimer()
    for b.N > 0 {
        b.N--
        _ = tool.ApplyScopeFilter(ctx, tools, sf)
    }
}
```

The benchmark target is < 5 ms p99 for a single call (NFR-01). Glob patterns are compiled in `newRule` outside the timed loop.

### 12.5 Property Tests (`internal/tool/filter_test.go`)

Use `pgregory.net/rapid` (the idiomatic Go property-test library) to generate random tool slices and allow-pattern sets, asserting that no tool present in the output of `ApplyScopeFilter` fails `sf.IsVisible`:

```go
func TestApplyFilterNoFalseNegatives(t *testing.T) {
    rapid.Check(t, func(rt *rapid.T) {
        names := rapid.SliceOf(rapid.StringMatching(`[a-z]{3,8}`)).Draw(rt, "names")
        tools := make([]tool.Tool, len(names))
        for i, n := range names {
            tools[i] = tool.NewTool("svc", n, nil)
        }
        patterns := rapid.SliceOfN(rapid.Just("svc:*"), 0, 5).Draw(rt, "patterns")
        rules := make([]tool.ScopeRule, len(patterns))
        for i, p := range patterns {
            rules[i] = newRule(int64(i), "allow", p, 100)
        }
        sf := &tool.ScopeFilter{Profile: "p", Rules: rules}
        result := tool.ApplyScopeFilter(context.Background(), tools, sf)
        for _, res := range result {
            assert.True(rt, sf.IsVisible(res.Qualified()),
                "false negative: %s removed but should be visible", res.Qualified())
        }
    })
}
```

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag mcp filter add --profile coder --allow "github:*" --deny "github:delete_*"` exits 0 and inserts 2 rows in `tool_scope_rules` | `SELECT COUNT(*) FROM tool_scope_rules WHERE profile='coder'` = 2 |
| AC-02 | After AC-01, `tag mcp filter list --profile coder --json` returns a JSON array of length 2 with correct `rule_type` and `pattern` fields | `jq length` = 2; `jq '.[].rule_type'` = `["allow","deny"]` |
| AC-03 | `ApplyScopeFilter` with the above rules: `github:create_issue` is visible, `github:delete_repository` is not visible | Unit test `TestAllowStarDenyDelete` passes |
| AC-04 | `tag mcp filter add --profile coder --deny "playwright:*" --dry-run` prints "DRY RUN" and exits 0 with no new rows in `tool_scope_rules` | Row count unchanged; stdout contains "DRY RUN" |
| AC-05 | `tag mcp transform add --profile coder --tool "github:create_issue" --rename "title:issue_title"` exits 0; `tag mcp transform list --profile coder --json` contains a transform for `github:create_issue` with `renames.title = "issue_title"` | Integration test passes |
| AC-06 | `ApplyTransform` renames `title` to `issue_title` in `InputSchema["properties"]` and `required` slice | Unit test passes |
| AC-07 | `ReverseTransform(map[string]any{"issue_title": "Bug"}, tx)` returns `map[string]any{"title": "Bug"}` | Unit test passes |
| AC-08 | `ReverseTransform` called twice on the same already-reversed map returns the same result | Idempotency unit test passes |
| AC-09 | `tag mcp filter export --profile coder` produces valid JSON parseable by `encoding/json` | CI integration test |
| AC-10 | `tag mcp filter export --profile coder | tag mcp filter import --profile staging --replace` followed by `tag mcp filter list --profile staging --json` produces identical output to `tag mcp filter list --profile coder --json` | Round-trip integration test |
| AC-11 | `tag mcp filter audit --profile coder --show-denied --json` JSON output has `visible_count + denied_count == total_available` | Arithmetic assertion in integration test |
| AC-12 | Every denied tool in the audit output has a non-empty `denied_by` field matching the pattern that blocked it | Integration test iterates `denied_tools` array |
| AC-13 | `tag mcp filter clear --yes --profile coder` deletes all rows in `tool_scope_rules` and `tool_transforms` for `coder` in a single transaction | `SELECT COUNT(*)` = 0 for both tables after clear |
| AC-14 | Adding a duplicate rule (same profile, rule_type, pattern) exits 0 with "Rule already exists" message and does not insert a duplicate row | Row count unchanged; stdout contains "already exists" |
| AC-15 | `ApplyScopeFilter` completes in < 5 ms p99 for 200 tools and 10 rules | Go benchmark `BenchmarkApplyScopeFilter200Tools` passes in CI |
| AC-16 | `tag mcp transform add --rename "x:y" --rename "z:y"` (collision) exits 1 with "Rename collision" message | Exit code check in integration test |
| AC-17 | A field in `RemovedFields` that appears in `callArgs` is dropped by `ReverseTransform` and NOT passed to the MCP server | Security unit test `TestRemovedFieldNotPassedThrough` passes |
| AC-18 | `tag doctor` output includes filter rule count for each configured profile | End-to-end CLI test |
| AC-19 | `tag mcp filter add` without `--profile` exits 1 with "--profile is required" | CLI unit test |
| AC-20 | `EnsureFilterSchema` is idempotent — calling it 3 times on the same `*sql.DB` raises no error and does not create duplicate tables | Unit test with 3 sequential calls |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-014 MCP Server Registry | Feature (existing PRD) | Provides `~/.tag/mcp-registry.yaml` which `tag mcp filter audit` reads to enumerate available tools. Must be implemented first for audit to work without a live server. |
| PRD-026 Vector-Based Tool Retrieval | Feature (existing PRD) | `internal/tool` already exists; this PRD extends it. `SearchTools()` and `KeywordSearchTools()` are the integration call-sites. |
| PRD-013 Agent Tracing | Feature (existing PRD) | OTel span emission in `ApplyScopeFilter` (NFR-08) via `go.opentelemetry.io/otel`. If tracing is not active the span is a no-op. |
| PRD-027 Eval Framework | Feature (existing PRD) | `tag eval run` tests agent behaviour under filter configurations. No code changes to the eval package required — filter state is keyed to profile name automatically. |
| PRD-034 Secret Scanning | Feature (existing PRD) | `tag mcp filter audit` must not expose credentials. The audit function must never read the MCP server `command`/`env` sections, only `tools[]` metadata. |
| `github.com/gobwas/glob` | Go module | Wildcard glob matching (replaces Python `fnmatch`). Patterns compiled once at load time. |
| `modernc.org/sqlite` | Go module | Pure-Go SQLite driver, CGO_ENABLED=0, FTS5 built-in, WAL mode. Already the canonical store for the Go harness. |
| `gofrs/flock` | Go module | Cross-process file locking for single-writer SQLite access. |
| `encoding/json` | Go stdlib | Transform serialisation/deserialisation. No additional module. |
| `go.opentelemetry.io/otel` | Go module | OTel tracing for NFR-08. Already planned for the Go harness. |
| `pgregory.net/rapid` | Go module (test only) | Property-based testing (replaces Python `hypothesis`). |
| `github.com/modelcontextprotocol/go-sdk v1.6.1` | Go module | MCP client transport for `tag mcp transform test` (via `internal/mcp` facade). Protocol version pinned as `const MCPProtocolVersion = "2025-11-25"`. |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|------------------|
| OQ-01 | Should the opt-in model (deny-by-default when any allow rule exists) be configurable per-profile, or always automatic based on rule presence? A profile author might want to combine an allow list with an explicit deny list while still allowing unmatched tools through. | Profile team | Before implementation |
| OQ-02 | Should `tag mcp transform test` require a running MCP server, or should it work against the registry YAML schema? Running servers is the most accurate source but adds a hard dependency on server availability. | Engineering | Before implementing FR-14 |
| OQ-03 | Glob patterns today use `gobwas/glob` semantics where `*` does not match `:`. Should we support `**` for cross-segment matching (e.g. `*:delete_*` to deny all delete tools across all servers)? `gobwas/glob` supports this natively via `glob.Compile(p, glob.WithSeparator(':'))`. | Engineering | Sprint planning |
| OQ-04 | Should transform rules be versioned (e.g. keyed to a tool schema hash) so that a server schema change that breaks a rename can be detected? This ties into PRD version-pinning (cluster research context item 6). | Security | Post-MVP |
| OQ-05 | When `ReverseTransform` encounters a field in `callArgs` that was removed (i.e. the LLM populated a field the profile author intended to block), should it drop silently, log a warning, or halt the call? Current spec says drop silently. | Security, UX | Before AC-17 is finalised |
| OQ-06 | `tag mcp filter import --merge` behaviour when an identical rule already exists: skip silently, or error? Current spec says skip (idempotent import). | Engineering | Sprint planning |
| OQ-07 | Should `tag mcp filter audit` work without the MCP registry YAML by falling back to querying live MCP server tool lists? This would make audit more accurate but add latency and a server dependency. | Engineering | Post-MVP |

---

## 16. Complexity and Timeline

### Phase 1 — SQLite Schema + Core Types (Days 1–2)

- Write `EnsureFilterSchema(db *sql.DB) error` DDL migration in `internal/tool/store.go`.
- Define `ScopeRule`, `ScopeFilter`, `ToolTransform` structs in `internal/tool/filter.go` and `internal/tool/transform.go`.
- Implement `LoadScopeFilter` and `LoadTransforms` with glob pre-compilation.
- Unit tests for structs and loaders; verify `EnsureFilterSchema` idempotency.

### Phase 2 — Filter Engine (Days 3–4)

- Implement `ScopeFilter.IsVisible()` with priority-based precedence semantics.
- Implement `ApplyScopeFilter(ctx, tools, f)` with OTel span emission.
- Wire `*ScopeFilter` parameter into `SearchTools()` and `KeywordSearchTools()`.
- Comprehensive unit tests: empty filter, allow-only, deny-only, allow+deny, equal-priority tie-breaking, opt-in vs opt-out model.
- Property tests with `pgregory.net/rapid`.
- Go benchmark (AC-15).

### Phase 3 — Transform Engine (Days 5–6)

- Implement `ApplyTransform(src Tool, tx *ToolTransform)` with the four-step pipeline.
- Implement `ReverseTransform(callArgs map[string]any, tx *ToolTransform)` with idempotency guarantee.
- Session startup wiring in `internal/cli` (tool list assembly + call dispatch).
- Unit tests for all four transform steps, collision detection, removed-field security.

### Phase 4 — CLI Commands (Days 7–9)

- Implement `internal/cli/mcp_filter.go` with subcommands: `add`, `remove`, `list`, `clear`, `audit`, `export`, `import`.
- Implement `internal/cli/mcp_transform.go` with subcommands: `add`, `remove`, `list`, `test`.
- Flag registration for all flags documented in Section 7.
- Integration tests: round-trip export/import, dry-run no-write, audit arithmetic, `tag doctor` output.

### Phase 5 — Integration and Hardening (Days 10–14)

- `tag doctor` extension (FR-17) in `internal/cli`.
- Security validation: pattern injection guard, `--default` JSON key denylist.
- End-to-end test running a real `coder` profile with GitHub MCP server enabled and filters active; verify tool count in audit output matches visible tool count at session start.
- Documentation update: `tag mcp filter --help` long-form help text.
- Address open questions OQ-01 and OQ-03 based on engineering consensus.

**Total: 10–14 working days (fits M estimate of 1–2 weeks).**
