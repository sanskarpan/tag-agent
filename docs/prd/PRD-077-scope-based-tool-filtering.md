# PRD-077: Scope-Based Tool Filtering + Schema Transformation (`tag mcp filter`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `tool_retrieval.py`
**Depends on:** PRD-014 (MCP Server Registry), PRD-026 (Vector-Based Tool Retrieval), PRD-027 (Eval Framework), PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing & Observability), PRD-034 (Security / Secret Scanning), PRD-001 (Structured Memory Configuration)
**Inspired by:** Composio scope-based filtering, Toolhouse schema processors
**GitHub Issue:** #346

---

## 1. Overview

TAG profiles today expose every tool from every enabled MCP server to the LLM at session start. On a well-stocked developer profile — with GitHub, filesystem, databases, and a browser MCP server all enabled — the raw tool inventory can easily exceed 60 discrete tools. That volume pushes two concrete failure modes: (1) it exceeds the Cursor 40-tool hard limit, causing silent tool truncation at the host layer; (2) even when the host accepts all tools, LLM attention dilutes over the large schema surface, which demonstrably increases tool-selection errors in agent benchmarks.

Scope-based tool filtering addresses this by introducing per-profile allowlists and denylists expressed as glob patterns. A profile author declares exactly which tools are in-scope (`--allow "github:*"`) and which are categorically excluded (`--deny "github:delete_*"`), and TAG enforces that contract at the retrieval layer before the tool list ever reaches the LLM. Filters are stored in SQLite, survive across restarts, compose gracefully with the existing vector-retrieval subsystem (PRD-026), and integrate with the eval framework (PRD-027) for regression testing.

Schema transformation is the complementary layer: MCP tool input schemas are authored by server maintainers, not by the teams using those tools in agent workflows. Field names are often generic (`title`, `body`, `name`) and collide badly when the LLM is calling multiple tools in a single session. Descriptions are frequently missing or too terse to guide model behaviour reliably. Transformation rules let profile authors rename fields to more semantically precise names, inject or overwrite field descriptions, set explicit defaults, and prune fields the LLM should never populate. The rewritten schema is what the LLM sees; the original schema is what gets sent to the MCP server after reverse-mapping the field names back.

Together, filtering and transformation implement a principle from the Composio and Toolhouse ecosystems: the shape of a tool as the LLM experiences it should be intentionally designed for that LLM, not inherited verbatim from the server's generic API surface. This PRD formalises that principle as a first-class TAG feature with CLI ergonomics, a SQLite-backed config store, and a runtime interception layer wired into `tool_retrieval.py`.

The feature is rated Difficulty 3/5 because the interception point in `tool_retrieval.py` is well-understood, SQLite DDL is straightforward, and glob matching is already used elsewhere in the codebase. Impact is 2/5 in the short term — it is a power-user workflow — but grows meaningfully as the MCP ecosystem proliferates and agent quality requirements tighten.

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
| G6 | Integration with `tool_retrieval.py`: `search_tools()` and `keyword_search_tools()` apply active scope filters before returning results to callers. |
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
| Time to apply filter + transform pipeline for 100 tools | — | < 5 ms p99 (in-process, no I/O) | pytest benchmark in `tests/test_tool_filter.py` |
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
| FR-02 | Pattern matching MUST use Python `fnmatch.fnmatch` semantics on the `server:tool_name` qualified string. `*` matches any sequence of characters except `:`. Patterns without `:` are rejected with exit code 1 and an error message. |
| FR-03 | Filter precedence MUST be: (1) explicit `deny` with lower priority number; (2) explicit `allow` with lower priority number; (3) at equal priority, `deny` beats `allow`; (4) if no rules match, default is ALLOW (opt-out model). |
| FR-04 | When at least one `--allow` pattern exists for a profile, the default for non-matching tools MUST flip to DENY (opt-in model). The effective model (opt-in vs opt-out) is computed dynamically from the presence of any allow rule and MUST be displayed in `tag mcp filter list` output. |
| FR-05 | `tool_retrieval.py::search_tools()` and `keyword_search_tools()` MUST accept an optional `scope_filter: ScopeFilter | None` parameter. When non-None, the filter is applied to the candidate list before scoring and before returning results. |
| FR-06 | `tool_retrieval.py::apply_scope_filter(tools, scope_filter)` MUST be a pure function (no I/O, no side effects) that takes a list of tool dicts and a `ScopeFilter` and returns the filtered list. It MUST complete in < 1 ms for inputs up to 200 tools. |
| FR-07 | Schema transformation MUST be applied in this order: (1) field removal; (2) field rename (in the JSON Schema `properties` dict and `required` array); (3) description injection; (4) default injection. Applying steps in a different order MUST NOT produce an equivalent result when the same field is both renamed and has a description injected (the description should apply to the new name). |
| FR-08 | The `reverse_transform(tool_name, call_args, profile)` function in `tool_retrieval.py` MUST reconstruct original field names from renamed ones before the call is dispatched to the MCP server. It MUST be idempotent (calling it twice on an already-reversed dict returns the same result). |
| FR-09 | `tag mcp filter audit` MUST enumerate tools by reading the MCP registry YAML at `~/.tag/mcp-registry.yaml` (same source as PRD-014) and applying active filter rules. It MUST NOT require a running MCP server. |
| FR-10 | `tag mcp filter audit --show-denied` output MUST include, for each denied tool, the specific rule pattern that caused the denial (`denied_by` field). |
| FR-11 | `--dry-run` on `tag mcp filter add` MUST NOT write any rows to SQLite. It MUST print the before/after tool count and a list of tools that would be added to or removed from the visible set. |
| FR-12 | `tag mcp filter export` MUST produce a JSON document that, when piped to `tag mcp filter import --replace`, produces an identical `tag mcp filter list --json` output. Round-trip fidelity is required for both filter rules and transform rules. |
| FR-13 | `tag mcp transform add` MUST validate that `--rename` pairs do not create duplicate field names in the output schema. If a collision is detected, exit code 1 with message `"Rename collision: field '<name>' appears more than once after transforms"`. |
| FR-14 | `tag mcp transform test` MUST fetch the tool schema from the configured MCP server using the same transport mechanism used at session start (stdio/streamable-http as configured in the profile). It MUST time out after 10 seconds and exit 1 with a clear error if the server is unreachable. |
| FR-15 | All `--json` outputs MUST be valid JSON (parseable by `json.loads`) and MUST include an `"as_of"` ISO-8601 timestamp field at the top level. |
| FR-16 | `tag mcp filter clear --yes` MUST delete all rows in `tool_scope_rules` for the given profile and all rows in `tool_transforms` for the given profile in a single transaction. |
| FR-17 | `tag doctor` (existing command in `controller.py`) MUST be extended to report the filter rule count and transform count per profile as informational entries. No warning threshold is enforced — these are purely informational. |
| FR-18 | All filter and transform operations MUST be scoped to a profile. Operations without `--profile` MUST exit 1 with the message `"--profile is required for this command"`. |
| FR-19 | `tag mcp transform add --remove <field>` MUST record removed fields in `tool_transforms.removed_fields` (JSON array). During reverse_transform, removed fields MUST NOT be injected back into call arguments — the server receives only what the LLM provides under the original name. |
| FR-20 | The `ScopeFilter` dataclass and all transform functions MUST live in `tool_retrieval.py`, not in `controller.py`. `controller.py` calls functions from `tool_retrieval.py`; no filter logic lives in the command handler. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Performance:** The full filter + transform pipeline for 100 tools MUST complete in < 5 ms p99 measured in an in-process benchmark (no SQLite I/O in the hot path — rules are loaded once at session start and cached in memory). |
| NFR-02 | **SQLite WAL compatibility:** All writes to `tool_scope_rules` and `tool_transforms` MUST use the existing `open_db()` pattern which enables WAL mode. No raw `sqlite3.connect()` calls outside `open_db()`. |
| NFR-03 | **Backward compatibility:** Profiles with no filter rules MUST behave identically to today — all tools from enabled servers are visible. The `ScopeFilter` with empty rule sets MUST be a no-op pass-through. |
| NFR-04 | **No new mandatory dependencies:** The filter and transform subsystem MUST work with zero additional pip packages. `fnmatch` (stdlib), `json` (stdlib), and `sqlite3` (stdlib) are the only runtime dependencies. |
| NFR-05 | **Atomicity of multi-rule adds:** When `tag mcp filter add` specifies multiple `--allow` and `--deny` flags, all rules MUST be inserted in a single transaction. A failure mid-insert MUST roll back all rules from that invocation. |
| NFR-06 | **TTY vs. pipe output:** All list/audit commands MUST detect whether stdout is a TTY. TTY: Rich table rendering with colour. Non-TTY: tab-separated plain text. `--json` overrides both. |
| NFR-07 | **Pattern validation on input:** Patterns containing characters that are illegal in `server:tool_name` qualified names (whitespace, semicolons, quotes) MUST be rejected at parse time with exit code 1 before any SQLite writes. |
| NFR-08 | **Tracing integration (PRD-013):** When tracing is enabled, `apply_scope_filter()` MUST emit an OTel span with attributes: `tag.filter.profile`, `tag.filter.input_count`, `tag.filter.output_count`, `tag.filter.rule_count`. Span duration feeds the NFR-01 benchmark. |
| NFR-09 | **Security:** Transform rules MUST NOT allow injection of arbitrary Python code or executable content. The `--default` value MUST be validated as valid JSON; arbitrary string blobs that parse as JSON but contain `__class__` or similar prototype-pollution keys MUST be rejected. |
| NFR-10 | **Idempotent re-add:** Running `tag mcp filter add` with an identical pattern that already exists for the same profile MUST be a no-op (no duplicate row). Exit 0 with message `"Rule already exists (id=<N>), no change made"`. |

---

## 10. Technical Design

### 10.1 New Files

- **`src/tag/tool_retrieval.py`** — Extended in-place (no new file). New additions:
  - `ScopeFilter` dataclass
  - `ToolTransform` dataclass
  - `apply_scope_filter(tools, scope_filter) -> list[dict]`
  - `apply_transform(tool_schema, transform) -> dict`
  - `reverse_transform(tool_name, call_args, transforms) -> dict`
  - `load_scope_filter(conn, profile) -> ScopeFilter`
  - `load_transforms(conn, profile, tool_name=None) -> list[ToolTransform]`
  - `ensure_filter_schema(conn)` — DDL migration

No new source files are required; all logic is additive to `tool_retrieval.py` with command handlers in the existing `cmd_mcp_filter` and `cmd_mcp_transform` functions in `controller.py`.

### 10.2 SQLite DDL

```sql
-- Migration: add to the existing ensure_schema() call in tool_retrieval.py
-- or call separately via ensure_filter_schema(conn)

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

### 10.3 Core Dataclasses

```python
from __future__ import annotations
from dataclasses import dataclass, field
from typing import Any
import fnmatch
import json

@dataclass
class ScopeRule:
    """A single allow/deny glob rule for a profile."""
    id: int
    profile: str
    rule_type: str          # 'allow' | 'deny'
    pattern: str            # e.g. "github:*" or "github:delete_*"
    priority: int           # lower = higher precedence
    created_at: str


@dataclass
class ScopeFilter:
    """The complete filter configuration for a profile, loaded from SQLite."""
    profile: str
    rules: list[ScopeRule] = field(default_factory=list)

    def is_passthrough(self) -> bool:
        """True if no rules are defined — all tools pass through."""
        return len(self.rules) == 0

    def has_any_allow(self) -> bool:
        return any(r.rule_type == "allow" for r in self.rules)

    def is_visible(self, qualified_name: str) -> bool:
        """
        Evaluate whether a tool (e.g. "github:create_issue") is visible
        under this filter.

        Precedence (evaluated in priority order, lower number = first):
          1. Deny at lower priority number → DENY wins immediately
          2. Allow at lower priority number → ALLOW wins immediately
          3. At equal priority → deny beats allow
          4. No matching rule:
             - If any allow rule exists → DENY (opt-in model)
             - Otherwise → ALLOW (opt-out model)
        """
        if self.is_passthrough():
            return True

        # Sort rules by (priority ASC, rule_type DESC so 'deny' > 'allow' lexically)
        sorted_rules = sorted(self.rules, key=lambda r: (r.priority, r.rule_type))
        last_priority: int | None = None
        allow_at: int | None = None
        deny_at: int | None = None

        for rule in sorted_rules:
            if not fnmatch.fnmatch(qualified_name, rule.pattern):
                continue
            p = rule.priority
            if rule.rule_type == "deny":
                if deny_at is None or p < deny_at:
                    deny_at = p
            else:
                if allow_at is None or p < allow_at:
                    allow_at = p

        if deny_at is not None and allow_at is not None:
            if deny_at < allow_at:
                return False
            if allow_at < deny_at:
                return True
            # equal priority — deny wins
            return False
        if deny_at is not None:
            return False
        if allow_at is not None:
            return True
        # No match
        return not self.has_any_allow()

    def denying_rule(self, qualified_name: str) -> ScopeRule | None:
        """Return the highest-precedence deny rule that blocks this tool, or None."""
        sorted_rules = sorted(self.rules, key=lambda r: (r.priority, r.rule_type))
        for rule in sorted_rules:
            if rule.rule_type == "deny" and fnmatch.fnmatch(qualified_name, rule.pattern):
                return rule
        return None


@dataclass
class ToolTransform:
    """Schema transformation rules for a single tool in a profile."""
    id: int
    profile: str
    tool_qualified: str            # "server:tool_name"
    renames: dict[str, str]        # {"old": "new", ...}
    descriptions: dict[str, str]   # {"field": "description text", ...}
    removed_fields: list[str]
    defaults: dict[str, Any]
    created_at: str
    updated_at: str
```

### 10.4 Core Algorithms

#### `apply_scope_filter`

```python
def apply_scope_filter(
    tools: list[dict[str, Any]],
    scope_filter: ScopeFilter | None,
) -> list[dict[str, Any]]:
    """Filter *tools* to only those visible under *scope_filter*.

    Tools must have 'server' and 'name' keys (or 'qualified' = 'server:name').
    Pure function — no I/O, no side effects.
    """
    if scope_filter is None or scope_filter.is_passthrough():
        return list(tools)

    result = []
    for tool in tools:
        qualified = tool.get("qualified") or f"{tool.get('server', '')}:{tool.get('name', '')}"
        if scope_filter.is_visible(qualified):
            result.append(tool)
    return result
```

#### `apply_transform`

```python
def apply_transform(
    tool_schema: dict[str, Any],
    transform: ToolTransform,
) -> dict[str, Any]:
    """Return a new tool schema dict with transform rules applied.

    Applies in order: remove → rename → describe → default.
    Does not mutate *tool_schema*.
    """
    import copy
    schema = copy.deepcopy(tool_schema)
    props: dict = schema.get("inputSchema", {}).get("properties", {})
    required: list = schema.get("inputSchema", {}).get("required", [])

    # Step 1: remove
    for field_name in transform.removed_fields:
        props.pop(field_name, None)
        if field_name in required:
            required.remove(field_name)

    # Step 2: rename (in properties AND required array)
    for old_name, new_name in transform.renames.items():
        if old_name in props:
            props[new_name] = props.pop(old_name)
        if old_name in required:
            idx = required.index(old_name)
            required[idx] = new_name

    # Step 3: inject/overwrite descriptions (use post-rename names)
    for field_name, desc_text in transform.descriptions.items():
        if field_name in props:
            props[field_name]["description"] = desc_text

    # Step 4: inject defaults (use post-rename names)
    for field_name, default_val in transform.defaults.items():
        if field_name in props:
            props[field_name]["default"] = default_val

    if "inputSchema" in schema:
        schema["inputSchema"]["properties"] = props
        schema["inputSchema"]["required"] = required
    return schema
```

#### `reverse_transform`

```python
def reverse_transform(
    call_args: dict[str, Any],
    transform: ToolTransform,
) -> dict[str, Any]:
    """Reverse field renames before dispatching call to the MCP server.

    Converts LLM-visible field names (post-transform) back to original names.
    Idempotent: calling twice on an already-reversed dict is safe.
    """
    reverse_map = {v: k for k, v in transform.renames.items()}
    result = {}
    for k, v in call_args.items():
        result[reverse_map.get(k, k)] = v
    return result
```

#### `load_scope_filter`

```python
def load_scope_filter(conn: sqlite3.Connection, profile: str) -> ScopeFilter:
    """Load all scope rules for *profile* from SQLite into a ScopeFilter."""
    ensure_filter_schema(conn)
    rows = conn.execute(
        "SELECT id, profile, rule_type, pattern, priority, created_at "
        "FROM tool_scope_rules WHERE profile = ? ORDER BY priority ASC, rule_type DESC",
        (profile,),
    ).fetchall()
    rules = [
        ScopeRule(
            id=r["id"], profile=r["profile"], rule_type=r["rule_type"],
            pattern=r["pattern"], priority=r["priority"], created_at=r["created_at"],
        )
        for r in rows
    ]
    return ScopeFilter(profile=profile, rules=rules)
```

#### `load_transforms`

```python
def load_transforms(
    conn: sqlite3.Connection,
    profile: str,
    tool_qualified: str | None = None,
) -> list[ToolTransform]:
    """Load transform rows for a profile, optionally restricted to one tool."""
    ensure_filter_schema(conn)
    if tool_qualified:
        rows = conn.execute(
            "SELECT * FROM tool_transforms WHERE profile=? AND tool_qualified=?",
            (profile, tool_qualified),
        ).fetchall()
    else:
        rows = conn.execute(
            "SELECT * FROM tool_transforms WHERE profile=? ORDER BY tool_qualified",
            (profile,),
        ).fetchall()
    return [
        ToolTransform(
            id=r["id"], profile=r["profile"], tool_qualified=r["tool_qualified"],
            renames=json.loads(r["renames"]),
            descriptions=json.loads(r["descriptions"]),
            removed_fields=json.loads(r["removed_fields"]),
            defaults=json.loads(r["defaults"]),
            created_at=r["created_at"], updated_at=r["updated_at"],
        )
        for r in rows
    ]
```

### 10.5 Integration Points

#### 10.5.1 `tool_retrieval.py::search_tools` and `keyword_search_tools`

Both functions gain an optional `scope_filter: ScopeFilter | None = None` parameter. Before returning the final result list, they call `apply_scope_filter(results, scope_filter)`. The filter runs after scoring so vector distances are not distorted by the filter; it is a post-retrieval gate.

#### 10.5.2 Session Startup in `controller.py`

At session start (wherever tool lists are assembled before the first LLM call), `controller.py` calls:

```python
scope_filter = load_scope_filter(conn, profile_name)
transforms   = load_transforms(conn, profile_name)
tools        = apply_scope_filter(raw_tools, scope_filter)
tools        = [apply_transform(t, tx) for t in tools
                for tx in transforms if tx.tool_qualified == t.get("qualified")]
```

`transforms` is indexed by `tool_qualified` at load time for O(1) lookup.

#### 10.5.3 Call Dispatch in `controller.py`

Before any MCP tool call is dispatched, `controller.py` looks up the transform for the tool and calls `reverse_transform(call_args, transform)` to restore original field names. If no transform exists for the tool, `call_args` is passed through unchanged.

#### 10.5.4 `tag eval` Integration (PRD-027)

`tag eval run` can be given an explicit profile whose filter configuration is active. Because filter/transform state is stored in SQLite and keyed to the profile name, no additional `eval`-specific integration is needed — eval already runs the agent under the named profile.

#### 10.5.5 Tracing (PRD-013)

```python
# In apply_scope_filter, when a tracer is active:
with tracer.start_as_current_span("tag.tool_filter.apply") as span:
    span.set_attribute("tag.filter.profile", scope_filter.profile)
    span.set_attribute("tag.filter.input_count", len(tools))
    result = _apply_scope_filter_inner(tools, scope_filter)
    span.set_attribute("tag.filter.output_count", len(result))
    span.set_attribute("tag.filter.rule_count", len(scope_filter.rules))
```

### 10.6 `ensure_filter_schema` Migration

```python
def ensure_filter_schema(conn: sqlite3.Connection) -> None:
    """Idempotent DDL migration for filter and transform tables."""
    conn.executescript("""
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
    """)
    conn.commit()
```

### 10.7 Export/Import JSON Schema

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

1. **No executable content in transforms:** The `--default` JSON value is parsed with `json.loads` and then validated to exclude object keys that could facilitate deserialization attacks (`__class__`, `__reduce__`, `__import__`). Any such key causes exit code 1.

2. **Pattern injection:** Patterns are validated against `^[a-zA-Z0-9_\-.*:]+$` before storage. No shell metacharacters (`;`, `|`, `&`, backtick, `$`) are permitted in patterns, preventing injection if patterns are ever interpolated into shell commands.

3. **Audit trail:** Every write to `tool_scope_rules` and `tool_transforms` is logged via the existing tracing subsystem (PRD-013) with the profile name, operation, and pattern. This provides a tamper-evident log of who changed filter configurations and when.

4. **No filter bypass at call time:** The `reverse_transform` function only restores field *names* — it never reconstructs a field that was removed. A field in `removed_fields` that appears in `call_args` (e.g. injected by a jailbreak) is silently dropped during reverse transform, not passed to the server.

5. **Deny-on-error:** If `apply_scope_filter` raises an exception (malformed pattern, unexpected data), it MUST fall back to returning an empty tool list (deny all) rather than returning the unfiltered list. This ensures filter failures are safe-by-default.

6. **SQLite integrity:** The `CHECK (rule_type IN ('allow','deny'))` constraint is enforced at the database layer, not only in application code. Even direct SQLite writes cannot insert an invalid rule type.

7. **No secret exposure in audit output:** `tag mcp filter audit` reads only tool names and descriptions from the MCP registry YAML. It MUST NOT read or display API keys, tokens, or credentials from MCP server configurations.

8. **Profile isolation:** All SQLite queries include a `WHERE profile = ?` clause. There is no cross-profile API; one profile's filter rules cannot be applied to another without an explicit import command.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_tool_filter.py`)

```python
# Core pattern matching
def test_allow_star_pattern():
    sf = ScopeFilter(profile="coder", rules=[
        ScopeRule(id=1, profile="coder", rule_type="allow",
                  pattern="github:*", priority=100, created_at=""),
    ])
    assert sf.is_visible("github:create_issue") is True
    assert sf.is_visible("filesystem:read_file") is False  # no allow match → deny

def test_deny_beats_allow_equal_priority():
    sf = ScopeFilter(profile="coder", rules=[
        ScopeRule(id=1, profile="coder", rule_type="allow", pattern="github:*", priority=100, created_at=""),
        ScopeRule(id=2, profile="coder", rule_type="deny",  pattern="github:delete_*", priority=100, created_at=""),
    ])
    assert sf.is_visible("github:create_issue") is True
    assert sf.is_visible("github:delete_repository") is False

def test_lower_priority_deny_beats_higher_priority_allow():
    sf = ScopeFilter(profile="coder", rules=[
        ScopeRule(id=1, profile="coder", rule_type="allow", pattern="github:*", priority=50, created_at=""),
        ScopeRule(id=2, profile="coder", rule_type="deny",  pattern="github:*", priority=10, created_at=""),
    ])
    assert sf.is_visible("github:create_issue") is False

def test_empty_filter_is_passthrough():
    sf = ScopeFilter(profile="coder", rules=[])
    assert sf.is_visible("any:tool") is True

def test_apply_scope_filter_pure():
    tools = [
        {"server": "github", "name": "create_issue"},
        {"server": "github", "name": "delete_repository"},
    ]
    sf = ScopeFilter(profile="coder", rules=[
        ScopeRule(id=1, profile="coder", rule_type="deny", pattern="github:delete_*", priority=100, created_at=""),
    ])
    result = apply_scope_filter(tools, sf)
    assert len(result) == 1
    assert result[0]["name"] == "create_issue"
```

### 12.2 Transform Unit Tests

```python
def test_apply_transform_rename_then_describe():
    schema = {
        "name": "create_issue",
        "inputSchema": {
            "properties": {"title": {"type": "string"}},
            "required": ["title"],
        }
    }
    tx = ToolTransform(
        id=1, profile="coder", tool_qualified="github:create_issue",
        renames={"title": "issue_title"},
        descriptions={"issue_title": "The issue heading"},
        removed_fields=[], defaults={}, created_at="", updated_at="",
    )
    result = apply_transform(schema, tx)
    assert "issue_title" in result["inputSchema"]["properties"]
    assert "title" not in result["inputSchema"]["properties"]
    assert result["inputSchema"]["properties"]["issue_title"]["description"] == "The issue heading"
    assert "issue_title" in result["inputSchema"]["required"]

def test_reverse_transform_idempotent():
    tx = ToolTransform(
        id=1, profile="coder", tool_qualified="github:create_issue",
        renames={"title": "issue_title"}, descriptions={},
        removed_fields=[], defaults={}, created_at="", updated_at="",
    )
    args = {"issue_title": "My bug", "body": "Details"}
    reversed1 = reverse_transform(args, tx)
    reversed2 = reverse_transform(reversed1, tx)  # already original names
    assert reversed1 == {"title": "My bug", "body": "Details"}
    assert reversed2 == reversed1  # idempotent

def test_removed_field_not_passed_through():
    tx = ToolTransform(
        id=1, profile="coder", tool_qualified="github:create_issue",
        renames={}, descriptions={}, removed_fields=["assignees"],
        defaults={}, created_at="", updated_at="",
    )
    args = {"title": "Bug", "assignees": ["attacker"]}
    result = reverse_transform(args, tx)
    assert "assignees" not in result
```

### 12.3 Integration Tests

- **`test_filter_cli_add_list_round_trip`**: Calls `cmd_mcp_filter_add` against a test SQLite DB, then `cmd_mcp_filter_list --json`, asserts JSON contains inserted rules.
- **`test_filter_export_import_replace`**: Export → import with `--replace` → compare list output for equality.
- **`test_dry_run_no_writes`**: Assert SQLite `tool_scope_rules` row count is unchanged after a `--dry-run` invocation.
- **`test_transform_add_collision_rejected`**: Calling `cmd_mcp_transform_add` with two `--rename` flags mapping different source fields to the same target name returns exit code 1.
- **`test_filter_audit_denied_by_field`**: After adding a deny rule, audit with `--show-denied --json` confirms `denied_by` matches the rule pattern.

### 12.4 Performance Tests

```python
import pytest
import time

def test_apply_scope_filter_200_tools_under_5ms():
    tools = [{"server": "github", "name": f"tool_{i}"} for i in range(200)]
    rules = [ScopeRule(id=i, profile="p", rule_type="deny",
                       pattern=f"github:tool_{i}0*", priority=100, created_at="")
             for i in range(10)]
    sf = ScopeFilter(profile="p", rules=rules)
    start = time.perf_counter()
    for _ in range(100):
        apply_scope_filter(tools, sf)
    elapsed_ms = (time.perf_counter() - start) / 100 * 1000
    assert elapsed_ms < 5.0, f"filter took {elapsed_ms:.2f} ms (limit: 5 ms)"
```

### 12.5 Property Tests (hypothesis)

```python
from hypothesis import given, strategies as st

@given(
    tools=st.lists(
        st.fixed_dictionaries({"server": st.text(min_size=1, max_size=20,
                                                  alphabet=st.characters(whitelist_categories=('Lu','Ll','Nd'))),
                               "name": st.text(min_size=1, max_size=30,
                                               alphabet=st.characters(whitelist_categories=('Lu','Ll','Nd')))}),
        max_size=200,
    ),
    patterns=st.lists(st.from_regex(r'[a-z]{3,8}:\*'), max_size=10),
)
def test_apply_filter_no_false_negatives(tools, patterns):
    """No tool that should be visible is ever removed."""
    rules = [ScopeRule(id=i, profile="p", rule_type="allow",
                       pattern=p, priority=100, created_at="")
             for i, p in enumerate(patterns)]
    sf = ScopeFilter(profile="p", rules=rules)
    result = apply_scope_filter(tools, sf)
    for t in result:
        q = f"{t['server']}:{t['name']}"
        assert sf.is_visible(q), f"False negative: {q} removed but should be visible"
```

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag mcp filter add --profile coder --allow "github:*" --deny "github:delete_*"` exits 0 and inserts 2 rows in `tool_scope_rules` | `SELECT COUNT(*) FROM tool_scope_rules WHERE profile='coder'` = 2 |
| AC-02 | After AC-01, `tag mcp filter list --profile coder --json` returns a JSON array of length 2 with correct `rule_type` and `pattern` fields | `jq length` = 2; `jq '.[].rule_type'` = `["allow","deny"]` |
| AC-03 | `apply_scope_filter` with the above rules: `github:create_issue` is visible, `github:delete_repository` is not visible | Unit test `test_allow_star_deny_delete` passes |
| AC-04 | `tag mcp filter add --profile coder --deny "playwright:*" --dry-run` prints "DRY RUN" and exits 0 with no new rows in `tool_scope_rules` | Row count unchanged; stdout contains "DRY RUN" |
| AC-05 | `tag mcp transform add --profile coder --tool "github:create_issue" --rename "title:issue_title"` exits 0; `tag mcp transform list --profile coder --json` contains a transform for `github:create_issue` with `renames.title = "issue_title"` | Integration test passes |
| AC-06 | `apply_transform` renames `title` to `issue_title` in `inputSchema.properties` and `required` array | Unit test passes |
| AC-07 | `reverse_transform({"issue_title": "Bug"}, tx)` returns `{"title": "Bug"}` | Unit test passes |
| AC-08 | `reverse_transform` called twice on the same already-reversed dict returns the same result | Idempotency unit test passes |
| AC-09 | `tag mcp filter export --profile coder` produces valid JSON parseable by `json.loads` | CI integration test |
| AC-10 | `tag mcp filter export --profile coder | tag mcp filter import --profile staging --replace` followed by `tag mcp filter list --profile staging --json` produces identical output to `tag mcp filter list --profile coder --json` | Round-trip integration test |
| AC-11 | `tag mcp filter audit --profile coder --show-denied --json` JSON output has `visible_count + denied_count == total_available` | Arithmetic assertion in integration test |
| AC-12 | Every denied tool in the audit output has a non-empty `denied_by` field matching the pattern that blocked it | Integration test iterates `denied_tools` array |
| AC-13 | `tag mcp filter clear --yes --profile coder` deletes all rows in `tool_scope_rules` and `tool_transforms` for `coder` in a single transaction | `SELECT COUNT(*)` = 0 for both tables after clear |
| AC-14 | Adding a duplicate rule (same profile, rule_type, pattern) exits 0 with "Rule already exists" message and does not insert a duplicate row | Row count unchanged; stdout contains "already exists" |
| AC-15 | `apply_scope_filter` completes in < 5 ms p99 for 200 tools and 10 rules | Performance test passes in CI |
| AC-16 | `tag mcp transform add --rename "x:y" --rename "z:y"` (collision) exits 1 with "Rename collision" message | Exit code check in integration test |
| AC-17 | A field in `removed_fields` that appears in `call_args` is dropped by `reverse_transform` and NOT passed to the MCP server | Security unit test `test_removed_field_not_passed_through` passes |
| AC-18 | `tag doctor` output includes filter rule count for each configured profile | End-to-end CLI test |
| AC-19 | `tag mcp filter add` without `--profile` exits 1 with "--profile is required" | CLI unit test |
| AC-20 | `ensure_filter_schema` is idempotent — calling it 3 times on the same connection raises no error and does not create duplicate tables | Unit test with 3 sequential calls |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-014 MCP Server Registry | Feature (existing PRD) | Provides `~/.tag/mcp-registry.yaml` which `tag mcp filter audit` reads to enumerate available tools. Must be implemented first for audit to work without a live server. |
| PRD-026 Vector-Based Tool Retrieval | Feature (existing PRD, implemented) | `tool_retrieval.py` already exists; this PRD extends it. `search_tools()` and `keyword_search_tools()` are the integration callsites. |
| PRD-013 Agent Tracing | Feature (existing PRD) | OTel span emission in `apply_scope_filter` (NFR-08). If tracing is not active, the span call is a no-op. |
| PRD-027 Eval Framework | Feature (existing PRD) | `tag eval run` tests agent behavior under filter configurations. No code changes to `eval.py` required — filter state is keyed to profile name automatically. |
| PRD-034 Secret Scanning | Feature (existing PRD) | `tag mcp filter audit` must not expose credentials. The audit function must never read the MCP server `command`/`env` sections, only `tools[]` metadata. |
| `fnmatch` (stdlib) | Python stdlib | Pattern matching. No additional install. |
| `json` (stdlib) | Python stdlib | Transform serialisation/deserialisation. No additional install. |
| `sqlite3` (stdlib) | Python stdlib, WAL mode via `open_db()` | Persistence layer. Already in use throughout TAG. |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|------------------|
| OQ-01 | Should the opt-in model (deny-by-default when any allow rule exists) be configurable per-profile, or always automatic based on rule presence? A profile author might want to combine an allow list with an explicit deny list while still allowing unmatched tools through. | Profile team | Before implementation |
| OQ-02 | Should `tag mcp transform test` require a running MCP server, or should it work against the registry YAML schema? Running servers is the most accurate source but adds a hard dependency on server availability. | Engineering | Before implementing FR-14 |
| OQ-03 | Glob patterns today use `fnmatch` semantics where `*` does not match `:`. Should we support `**` for cross-segment matching (e.g. `*:delete_*` to deny all delete tools across all servers)? This would require a small custom matcher. | Engineering | Sprint planning |
| OQ-04 | Should transform rules be versioned (e.g. keyed to a tool schema hash) so that a server schema change that breaks a rename can be detected? This ties into PRD version-pinning (cluster research context item 6). | Security | Post-MVP |
| OQ-05 | When `reverse_transform` encounters a field in `call_args` that was removed (i.e. the LLM populated a field the profile author intended to block), should it drop silently, log a warning, or halt the call? Current spec says drop silently. | Security, UX | Before AC-17 is finalised |
| OQ-06 | `tag mcp filter import --merge` behaviour when an identical rule already exists: skip silently, or error? Current spec says skip (idempotent import). | Engineering | Sprint planning |
| OQ-07 | Should `tag mcp filter audit` work without the MCP registry YAML by falling back to querying live MCP server tool lists? This would make audit more accurate but add latency and a server dependency. | Engineering | Post-MVP |

---

## 16. Complexity and Timeline

### Phase 1 — SQLite Schema + Core Dataclasses (Days 1–2)

- Write `ensure_filter_schema(conn)` DDL migration.
- Define `ScopeRule`, `ScopeFilter`, `ToolTransform` dataclasses in `tool_retrieval.py`.
- Implement `load_scope_filter(conn, profile)` and `load_transforms(conn, profile)`.
- Unit tests for dataclasses and loaders.

### Phase 2 — Filter Engine (Days 3–4)

- Implement `ScopeFilter.is_visible()` with priority-based precedence semantics.
- Implement `apply_scope_filter(tools, scope_filter)` pure function.
- Wire `scope_filter` parameter into `search_tools()` and `keyword_search_tools()`.
- Comprehensive unit tests: empty filter, allow-only, deny-only, allow+deny, equal-priority tie-breaking, opt-in vs opt-out model.
- Property tests with `hypothesis`.
- Performance benchmark (AC-15).

### Phase 3 — Transform Engine (Days 5–6)

- Implement `apply_transform(tool_schema, transform)` with the four-step pipeline.
- Implement `reverse_transform(call_args, transform)` with idempotency guarantee.
- Session startup wiring in `controller.py` (tool list assembly + call dispatch).
- Unit tests for all four transform steps, collision detection, removed-field security.

### Phase 4 — CLI Commands (Days 7–9)

- Implement `cmd_mcp_filter` handler in `controller.py` with subcommands: `add`, `remove`, `list`, `clear`, `audit`, `export`, `import`.
- Implement `cmd_mcp_transform` handler with subcommands: `add`, `remove`, `list`, `test`.
- Argparse registration for all flags documented in Section 7.
- Integration tests: round-trip export/import, dry-run no-write, audit arithmetic, `tag doctor` output.

### Phase 5 — Integration and Hardening (Days 10–14)

- OTel span emission in `apply_scope_filter` (NFR-08).
- `tag doctor` extension (FR-17).
- Security validation: pattern injection guard, `--default` JSON key denylist.
- End-to-end test running a real `coder` profile with GitHub MCP server enabled and filters active; verify tool count in audit output matches visible tool count at session start.
- Documentation update: `tag mcp filter --help` long-form help text.
- Address open questions OQ-01 and OQ-03 based on engineering consensus.

**Total: 10–14 working days (fits M estimate of 1–2 weeks).**

