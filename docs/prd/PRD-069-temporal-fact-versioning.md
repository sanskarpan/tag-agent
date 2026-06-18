# PRD-069: Temporal Fact Versioning with valid_at/invalid_at (`tag mem fact`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Memory & Knowledge
**Affects:** `semantic_memory.py schema`
**Depends on:** PRD-025 (Semantic Memory with Confidence Decay), PRD-013 (Agent Tracing/Observability), PRD-028 (Sandbox Code Execution), PRD-034 (Secret Scanning), PRD-027 (Eval Framework)
**GitHub issue:** #345
**Inspired by:** Zep temporal knowledge graphs, Allen interval algebra, Bitemporal data models

---

## 1. Overview

Software teams accumulate facts over time that change: the Python version in use shifts from 3.11 to 3.12, the team's preferred formatter changes from Black to Ruff, the database moves from Postgres 14 to Postgres 16. TAG's current `semantic_memory.py` (PRD-025) treats every memory as a snapshot frozen at `created_at`. When a fact becomes stale, the only recourse is `tag mem forget <id>` — a hard delete that destroys the historical record. The agent cannot answer "what did we believe about the Python version in January 2025?" because the old belief is gone.

This PRD introduces **Temporal Fact Versioning**: a bitemporal extension to the `semantic_memories` table that tracks when each fact was believed to be true (its **valid time**) independently from when the fact was recorded in the database (its **transaction time**). Every fact carries two timestamps — `valid_at` (the moment the fact became true in the real world) and `invalid_at` (the moment it ceased to be true, or `NULL` for currently-true facts). When a fact is superseded, the old row is closed by setting `invalid_at` rather than deleted, and a new row is inserted with `valid_at` set to the transition point. This models the Allen interval algebra concept of a fact having a definite lifespan: [valid_at, invalid_at).

The design is deliberately conservative: it reuses the existing `semantic_memories` SQLite table via an `ALTER TABLE` migration, adds two indexed columns, and exposes four new CLI operations (`tag mem add --valid-from`, `tag mem update --invalidate`, `tag mem search --as-of`, `tag mem history`). No new dependencies are required beyond what PRD-025 already ships. All existing code paths that omit temporal arguments continue to behave identically to today: `valid_at` defaults to `NOW()` and `invalid_at` defaults to `NULL`, so queries that do not specify `--as-of` filter on `invalid_at IS NULL` and see only currently-valid facts — exactly the existing behaviour.

The primary beneficiaries are long-running projects where the same conceptual fact (e.g., "the API base URL") changes over time, teams doing post-mortems who need to reconstruct what the agent believed during an incident window, and eval harnesses (PRD-027) that need to replay memory state at the exact moment a historical run executed. The bitemporal model is the minimal-viable version of what Zep calls "temporal knowledge graph edges" — without requiring a graph database. The SQL realisation uses plain SQLite with two ISO-8601 TEXT columns and a partial index, keeping the entire implementation within the existing `open_db()` + WAL-mode SQLite constraint.

---

## 2. Problem Statement

### 2.1 Fact mutation destroys historical context

When a fact changes — e.g., "the team uses Python 3.11" supersedes "the team uses Python 3.10" — the current model requires the user to `tag mem forget` the old memory and `tag mem add` a new one. The old fact is permanently deleted. Post-hoc questions such as "what did we believe about the Python runtime when that test suite was written six months ago?" become unanswerable. Incident retrospectives, eval replays (PRD-027), and compliance audits all benefit from an immutable historical record of what was believed and when.

### 2.2 The agent operates on stale facts with no visibility into their age semantics

`tag mem search` today returns results ranked by `compute_confidence()`, which applies exponential decay based on `created_at`. This correctly penalises old *unreviewed* facts. But it incorrectly penalises a fact that was explicitly re-confirmed as true last week even though it was first recorded two years ago (`confidence_base` stays high; only `created_at` drives the decay clock). Conversely, a fact that was true a year ago but is now known-false continues to appear in search results until manually deleted. There is no way to express "this fact was true until 2025-03-01 and should not appear in searches after that date."

### 2.3 No temporal query API for agent context injection

`loop_agent.py` injects the top-k memories into each agent turn. When replaying a historical run (e.g., for eval regression testing or RCA), the injected memories should reflect what was known *at that moment*, not what is known today. Without `valid_at`/`invalid_at`, there is no way to reconstruct point-in-time memory state. This means eval replays of historical runs may inject different context than was available during the original run, producing non-reproducible scores.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Every row in `semantic_memories` carries a `valid_at` (ISO-8601 UTC) and `invalid_at` (ISO-8601 UTC or NULL) column that defines the real-world validity interval [valid_at, invalid_at). |
| G2 | `tag mem add --valid-from <date>` lets users back-date a fact to the date it actually became true, not just the date it was recorded. |
| G3 | `tag mem update <id> --invalidate [--at <date>]` marks an existing fact as no longer true by setting `invalid_at`, without deleting the row, preserving history. |
| G4 | `tag mem search <query> --as-of <date>` returns only facts whose validity interval contains the specified date, enabling point-in-time memory reconstruction. |
| G5 | `tag mem history <id>` lists all versions of a fact chain (linked via `supersedes_id`), from oldest to newest, showing the full temporal evolution. |
| G6 | The default behaviour of all existing `tag mem` commands is unchanged: queries without `--as-of` operate on currently-valid facts (`invalid_at IS NULL`), and `tag mem add` without `--valid-from` defaults `valid_at` to the current timestamp. |
| G7 | `loop_agent.py` accepts an optional `as_of` parameter in memory-injection calls, enabling eval harnesses to replay context at a historical point in time. |
| G8 | The SQLite migration is additive-only: `ALTER TABLE ... ADD COLUMN` with sensible defaults — no data loss on upgrade, no schema recreation. |
| G9 | All temporal operations are logged as spans in TAG's OTel tracing layer (PRD-013) with `memory.valid_at`, `memory.invalid_at`, and `memory.as_of_query` span attributes. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | **Graph-based temporal knowledge edges (Zep/Graphiti style):** The Allen-algebra interval model is implemented as two columns on the existing flat table, not as a property graph. Entity-relationship extraction and graph traversal are out of scope. |
| NG2 | **Transaction-time bitemporal tracking:** This PRD implements valid-time versioning only. Transaction time (when the row was physically inserted) is captured implicitly by `created_at` (existing column) and is not exposed as a separate query axis in this version. |
| NG3 | **Automatic supersession detection:** The system does not use LLM inference to detect that a new memory contradicts an existing one and automatically invalidate the old row. That is the mem0 UPDATE_MEMORY_PROMPT pattern and is scoped to a future PRD. |
| NG4 | **PostgreSQL tstzrange with exclusion constraints:** The implementation targets TAG's embedded SQLite. The exclusion constraint pattern (which enforces non-overlapping intervals at the DB level) is documented in the Open Questions section as a future migration path if TAG adopts PostgreSQL. |
| NG5 | **Retroactive invalidation of all memories matching a pattern:** `tag mem update --invalidate` targets a single memory ID. Bulk invalidation (e.g., "invalidate all memories about Python version") is out of scope. |
| NG6 | **UI or dashboard for temporal timelines:** Temporal history is exposed via the CLI (`tag mem history`) and `--json` output. No web UI or TUI timeline view is included. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|-------------------|
| Schema migration completes without data loss | All pre-existing rows survive migration with `valid_at = created_at` and `invalid_at = NULL` | Integration test: insert 100 rows, run migration, assert all 100 rows present with correct defaults |
| `tag mem search --as-of` returns correct point-in-time set | Zero false positives (facts outside the validity window) and zero false negatives (facts inside the window) in test suite | Unit tests with synthetic temporal fixture data covering all Allen interval cases |
| `tag mem update --invalidate` latency | < 5 ms for a single invalidation on a 10,000-row table | Benchmark test in `tests/perf/` |
| `tag mem history` completeness | Returns all versions in a chain of ≥ 10 supersessions in correct chronological order | Integration test with 10-deep supersession chain |
| No regression on existing `tag mem` commands | All pre-existing `tag mem` tests pass with zero modification | CI gate: `pytest tests/test_semantic_memory.py` |
| `loop_agent.py` as-of injection | Eval replay with `as_of=<past_date>` injects only facts valid at that date | Integration test comparing injected context for two different `as_of` values |
| Index selectivity | `--as-of` query on 100,000-row table completes in < 50 ms | SQLite EXPLAIN QUERY PLAN shows index usage; benchmark confirms timing |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag mem add "The team uses Python 3.12" --valid-from 2025-06-01` after a runtime upgrade | the fact's validity period accurately reflects when the change happened, not when I remembered to record it |
| U2 | Developer | run `tag mem update <id> --invalidate --at 2025-05-31` on the "uses Python 3.11" memory | the old fact is closed out as of the correct date rather than deleted, and history is preserved |
| U3 | Developer | run `tag mem search "Python version" --as-of 2025-01-15` | I get the Python runtime fact that was true in January, not the one that is true today, enabling accurate retrospectives |
| U4 | SRE doing incident review | run `tag mem search "database host" --as-of 2025-03-15T02:00:00Z` | I can reconstruct exactly what the agent believed about the database host during the incident window at 2 AM |
| U5 | Eval engineer | pass `as_of=run.started_at` to memory injection during eval replay | the eval sees the same memory context as the original run, producing reproducible scores rather than context drift |
| U6 | Developer | run `tag mem history abc123def456 --json` | I get a machine-readable timeline of all versions of that fact for scripting, reporting, or export |
| U7 | Team lead | run `tag mem list --current` | I see only currently-valid facts (the default view) without having to think about temporal flags |
| U8 | Developer | add a fact without any temporal flags | the system records `valid_at = NOW()` and `invalid_at = NULL` automatically, identical to pre-PRD behaviour |
| U9 | Developer | run `tag mem history abc123def456` without `--json` | I get a human-readable table showing the version chain: ID, content snippet, valid_at, invalid_at |
| U10 | CI pipeline | call `memory_at_time(conn, profile, as_of=run_start)` from Python | the eval harness gets a list of dicts that represents point-in-time memory state without any CLI subprocess overhead |

---

## 6. Proposed CLI Surface

All new surface lives under the existing `tag mem` namespace. No new top-level commands are introduced.

### 6.1 `tag mem add` — extended with `--valid-from`

```
tag mem add <content> [--valid-from DATE] [--type TYPE] [--confidence FLOAT] [--source SOURCE] [--profile PROFILE] [--json]
```

**New flag:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--valid-from` | ISO-8601 date or datetime string | current UTC timestamp | The date/time from which this fact is true in the real world. May be in the past (back-dating) or absent (default to now). |

**Example — back-dated fact:**
```
$ tag mem add "The team uses Python 3.12" --valid-from 2025-06-01
Memory added: id=a1b2c3d4e5f6 valid_from=2025-06-01T00:00:00+00:00
```

**Example — standard add (unchanged behaviour):**
```
$ tag mem add "We use pytest for all unit tests"
Memory added: id=9f8e7d6c5b4a valid_from=2026-06-17T09:14:22+00:00
```

---

### 6.2 `tag mem update` — extended with `--invalidate`

```
tag mem update <id> --invalidate [--at DATE] [--profile PROFILE] [--json]
```

**New flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--invalidate` | flag | — | Mark the memory as no longer true. Sets `invalid_at` on the target row. |
| `--at` | ISO-8601 date or datetime string | current UTC timestamp | The point in time at which the fact became false. Defaults to now. May be back-dated. |

**Behaviour:** Sets `invalid_at` on the row identified by `<id>`. Raises an error if `invalid_at` is already set (the fact is already closed). Does not delete the row.

**Example:**
```
$ tag mem update a1b2c3d4e5f6 --invalidate --at 2025-05-31
Memory a1b2c3d4e5f6 invalidated as of 2025-05-31T00:00:00+00:00
  Content: "The team uses Python 3.11"
  Was valid: 2024-11-01T00:00:00+00:00 → 2025-05-31T00:00:00+00:00
```

**Error case — already invalidated:**
```
$ tag mem update a1b2c3d4e5f6 --invalidate
Error: memory a1b2c3d4e5f6 is already invalidated (invalid_at=2025-05-31T00:00:00+00:00)
       Use 'tag mem history a1b2c3d4e5f6' to see the full version chain.
```

---

### 6.3 `tag mem search` — extended with `--as-of`

```
tag mem search <query> [--as-of DATE] [--type TYPE] [--limit N] [--min-confidence FLOAT] [--profile PROFILE] [--json]
```

**New flag:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--as-of` | ISO-8601 date or datetime string | `NULL` (current facts only) | Return only facts whose validity interval [valid_at, invalid_at) contains this timestamp. When omitted, returns only `invalid_at IS NULL` rows (currently-valid facts). |

**Without `--as-of` (existing behaviour, unchanged):**
```
$ tag mem search "Python version"
┌──────────────────┬────────────────────────────────────┬──────────┬──────────┐
│ ID               │ Content                            │ Type     │ Score    │
├──────────────────┼────────────────────────────────────┼──────────┼──────────┤
│ a1b2c3d4e5f6     │ The team uses Python 3.12          │ fact     │ 0.9821   │
└──────────────────┴────────────────────────────────────┴──────────┴──────────┘
```

**With `--as-of` (new behaviour):**
```
$ tag mem search "Python version" --as-of 2025-01-15
┌──────────────────┬────────────────────────────────────┬──────────┬──────────┐
│ ID               │ Content                            │ Type     │ Score    │
├──────────────────┼────────────────────────────────────┼──────────┼──────────┤
│ 8c7d6e5f4a3b     │ The team uses Python 3.11          │ fact     │ 0.9744   │
└──────────────────┴────────────────────────────────────┴──────────┴──────────┘
```

**With `--json` flag:**
```json
[
  {
    "id": "8c7d6e5f4a3b",
    "content": "The team uses Python 3.11",
    "memory_type": "fact",
    "confidence": 0.9744,
    "valid_at": "2024-11-01T00:00:00+00:00",
    "invalid_at": "2025-05-31T00:00:00+00:00",
    "as_of_query": "2025-01-15T00:00:00+00:00"
  }
]
```

---

### 6.4 `tag mem history` — new subcommand

```
tag mem history <id> [--profile PROFILE] [--json] [--all-profiles]
```

**Description:** Displays all versions of a fact chain anchored at `<id>`. Traverses `supersedes_id` links both forward (from `<id>` to its descendants) and backward (from `<id>` to its ancestors) to reconstruct the complete version chain in chronological order.

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | flag | — | Output the chain as a JSON array. |
| `--all-profiles` | flag | — | Include versions across all profiles (for memories that were migrated or re-tagged). |

**Example — human output:**
```
$ tag mem history 8c7d6e5f4a3b
Fact history for 8c7d6e5f4a3b (root)
Chain length: 2 versions

  Version 1  [CLOSED]
  ID:         8c7d6e5f4a3b
  Content:    The team uses Python 3.11
  Valid:      2024-11-01T00:00:00+00:00 → 2025-05-31T00:00:00+00:00
  Duration:   211 days
  Superseded: a1b2c3d4e5f6

  Version 2  [CURRENT]
  ID:         a1b2c3d4e5f6
  Content:    The team uses Python 3.12
  Valid:      2025-06-01T00:00:00+00:00 → (open)
  Duration:   381 days (ongoing)
  Supersedes: 8c7d6e5f4a3b
```

**Example — JSON output:**
```json
{
  "root_id": "8c7d6e5f4a3b",
  "chain_length": 2,
  "versions": [
    {
      "id": "8c7d6e5f4a3b",
      "content": "The team uses Python 3.11",
      "valid_at": "2024-11-01T00:00:00+00:00",
      "invalid_at": "2025-05-31T00:00:00+00:00",
      "supersedes_id": null,
      "superseded_by": "a1b2c3d4e5f6",
      "status": "closed"
    },
    {
      "id": "a1b2c3d4e5f6",
      "content": "The team uses Python 3.12",
      "valid_at": "2025-06-01T00:00:00+00:00",
      "invalid_at": null,
      "supersedes_id": "8c7d6e5f4a3b",
      "superseded_by": null,
      "status": "current"
    }
  ]
}
```

---

### 6.5 `tag mem list` — extended with `--current` / `--all` / `--as-of`

```
tag mem list [--current] [--all] [--as-of DATE] [--type TYPE] [--limit N] [--profile PROFILE] [--json]
```

| Flag | Description |
|------|-------------|
| `--current` | Show only `invalid_at IS NULL` rows (default when no temporal flag given — no change). |
| `--all` | Show all rows including invalidated ones. Adds `Status` column to output. |
| `--as-of DATE` | Show rows valid at the specified point in time. |

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | The `semantic_memories` table must gain two new columns: `valid_at TEXT NOT NULL` and `invalid_at TEXT`. Migration runs via `ALTER TABLE … ADD COLUMN` with defaults, preserving all existing rows. | Must |
| FR-02 | The `semantic_memories` table must gain a `supersedes_id TEXT REFERENCES semantic_memories(id)` column to form explicit version chains between related facts. | Must |
| FR-03 | On `tag mem add`, `valid_at` defaults to the current UTC timestamp when `--valid-from` is not provided; `invalid_at` defaults to `NULL`. | Must |
| FR-04 | `--valid-from` must accept ISO-8601 date (`YYYY-MM-DD`), datetime (`YYYY-MM-DDTHH:MM:SS`), and datetime-with-timezone (`YYYY-MM-DDTHH:MM:SS+HH:MM`) strings. All are normalised to UTC ISO-8601 before storage. | Must |
| FR-05 | `tag mem update <id> --invalidate [--at DATE]` must set `invalid_at` on the target row. It must raise `ValueError` (surfaced as CLI error with exit code 1) if `invalid_at` is already set. | Must |
| FR-06 | `tag mem search <query>` without `--as-of` must return only rows where `invalid_at IS NULL`, identical to current behaviour. | Must |
| FR-07 | `tag mem search <query> --as-of DATE` must return only rows where `valid_at <= DATE AND (invalid_at IS NULL OR invalid_at > DATE)`. | Must |
| FR-08 | `tag mem history <id>` must traverse both the `supersedes_id` backward chain and the forward chain (rows whose `supersedes_id = <id>`) and return all versions in ascending `valid_at` order. | Must |
| FR-09 | `tag mem list` without temporal flags must return only `invalid_at IS NULL` rows, identical to current behaviour. | Must |
| FR-10 | `tag mem list --all` must return all rows, adding a `status` field: `"current"` if `invalid_at IS NULL`, `"closed"` otherwise. | Must |
| FR-11 | The FTS5 virtual table `semantic_memories_fts` must remain synchronised: invalidated rows must remain indexed (they are still findable via `--all`). The `--as-of` filter is applied as a post-FTS WHERE clause on the main table. | Must |
| FR-12 | All Python API functions (`add_memory`, `search_memories`, `list_memories`) must accept new keyword arguments (`valid_at`, `as_of`, `include_invalid`) and maintain backward compatibility when those arguments are absent. | Must |
| FR-13 | A new Python function `invalidate_memory(conn, mem_id, profile, *, at=None)` must be added to `semantic_memory.py`. | Must |
| FR-14 | A new Python function `memory_history(conn, mem_id, profile)` must be added to `semantic_memory.py`, returning a list of dicts in chronological order. | Must |
| FR-15 | A new Python function `memory_at_time(conn, profile, query, as_of, *, limit=10)` must be added, callable from `loop_agent.py` eval replay paths. | Must |
| FR-16 | `loop_agent.py`'s memory injection path must accept an optional `as_of: str | None` parameter; when `None`, it uses current-only semantics; when set, it calls `memory_at_time`. | Should |
| FR-17 | All temporal operations must emit OTel spans with attributes `memory.valid_at`, `memory.invalid_at`, and `memory.as_of_query` consistent with PRD-013 `otel_semconv.py` conventions. | Should |
| FR-18 | `tag mem add --valid-from` must reject dates more than 50 years in the past or any date in the future beyond 1 year, with a descriptive error and `--force` override flag. | Should |
| FR-19 | The migration must be idempotent: running it on a database where the columns already exist must be a no-op (use `ALTER TABLE … ADD COLUMN IF NOT EXISTS` or a schema version check). | Must |
| FR-20 | `tag mem history` output with `--json` must be machine-parseable with a stable schema documented in the PRD (see §6.4). | Should |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Migration latency:** `ensure_schema()` with the migration must complete in < 100 ms even on a table with 100,000 rows (ADD COLUMN is O(1) in SQLite 3.37+ via page-level extension). | < 100 ms |
| NFR-02 | **Query latency (`--as-of`):** `tag mem search --as-of` on a 100,000-row table must complete in < 50 ms wall time, verified by `tests/perf/test_temporal_perf.py`. | < 50 ms |
| NFR-03 | **Query latency (default, no temporal flag):** The `invalid_at IS NULL` partial index must ensure that the default search path is no slower than pre-PRD (measured by CI benchmark). | ≤ 5% overhead |
| NFR-04 | **Backward compatibility:** All unit tests in `tests/test_semantic_memory.py` that existed before this PRD must pass without modification to the test files. | 100% pass rate |
| NFR-05 | **Storage overhead:** Adding three columns (`valid_at`, `invalid_at`, `supersedes_id`) to 100,000 rows must increase database size by ≤ 15 MB (approx. 50 bytes per row overhead). | ≤ 15 MB |
| NFR-06 | **Thread safety:** All new functions use the same `conn: sqlite3.Connection` pattern as existing code. No global state or module-level connection is introduced. WAL mode (already enabled) handles concurrent reads. | Existing pattern |
| NFR-07 | **No new runtime dependencies:** The temporal versioning feature must work with zero additional `pip install` requirements beyond what PRD-025 already mandates. | 0 new deps |
| NFR-08 | **Date parsing robustness:** `_parse_temporal_arg()` must handle at minimum: `YYYY-MM-DD`, `YYYY-MM-DDTHH:MM:SS`, `YYYY-MM-DDTHH:MM:SSZ`, `YYYY-MM-DDTHH:MM:SS+HH:MM`. Invalid strings must raise `ValueError` with the offending string quoted in the message. | 4 formats + error |
| NFR-09 | **FTS consistency:** The FTS5 sync trigger for `invalid_at` changes must not degrade FTS insert throughput by more than 10% vs. the current baseline (measured by `tests/perf/test_fts_throughput.py`). | ≤ 10% overhead |
| NFR-10 | **Auditability:** Every call to `invalidate_memory()` must record the caller's identity via the `source` column update (set `source = 'invalidated:' + original_source`). | Logged |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change Type | Description |
|------|-------------|-------------|
| `src/tag/semantic_memory.py` | Modify | Add schema migration, new columns, new API functions. Central change. |
| `src/tag/controller.py` | Modify | Wire new `--valid-from`, `--invalidate`, `--at`, `--as-of` flags to `cmd_mem_*` handlers. |
| `tests/test_semantic_memory.py` | Modify | Add temporal unit tests. Existing tests must pass unchanged. |
| `tests/test_prd_features.py` | Modify | Add PRD-069 acceptance test block. |
| `tests/perf/test_temporal_perf.py` | New | Benchmark `--as-of` query on 100k-row table. |

No new module files are created; all logic lives in the existing `semantic_memory.py`, consistent with how PRD-025 was implemented.

---

### 9.2 SQLite DDL — Schema Migration

The migration is applied inside `ensure_schema()` using a version-gated pattern already used elsewhere in the codebase.

```sql
-- Applied via ensure_schema() with IF NOT EXISTS guards
-- Compatible with SQLite 3.35+ (ALTER TABLE ADD COLUMN IF NOT EXISTS)

ALTER TABLE semantic_memories ADD COLUMN IF NOT EXISTS
  valid_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));

ALTER TABLE semantic_memories ADD COLUMN IF NOT EXISTS
  invalid_at  TEXT;

ALTER TABLE semantic_memories ADD COLUMN IF NOT EXISTS
  supersedes_id TEXT REFERENCES semantic_memories(id);

-- Partial index for the hot path: queries over currently-valid facts.
-- Covers the WHERE invalid_at IS NULL filter used by all default queries.
CREATE INDEX IF NOT EXISTS idx_sm_current
  ON semantic_memories(profile, memory_type)
  WHERE invalid_at IS NULL;

-- Index for as-of queries: scans [valid_at, invalid_at) intervals.
-- Used by: SELECT ... WHERE valid_at <= ? AND (invalid_at IS NULL OR invalid_at > ?)
CREATE INDEX IF NOT EXISTS idx_sm_temporal
  ON semantic_memories(profile, valid_at, invalid_at);

-- Index for history traversal: walk supersedes_id chain forward.
CREATE INDEX IF NOT EXISTS idx_sm_supersedes
  ON semantic_memories(supersedes_id)
  WHERE supersedes_id IS NOT NULL;
```

**Migration backfill:** For existing rows where `valid_at` is `NULL` after migration (SQLite's `ADD COLUMN` with a non-constant default can behave differently across versions), the `ensure_schema()` function runs a one-time backfill:

```sql
UPDATE semantic_memories
SET valid_at = created_at
WHERE valid_at IS NULL OR valid_at = '';
```

---

### 9.3 Python Dataclass

```python
from __future__ import annotations
from dataclasses import dataclass, field
from datetime import datetime, timezone


@dataclass(frozen=True)
class TemporalMemory:
    """Immutable value object representing one version of a temporal fact."""

    id: str
    profile: str
    content: str
    memory_type: str
    confidence_base: float
    confidence: float          # effective (decay-adjusted)
    created_at: str            # transaction time: when the row was written
    valid_at: str              # valid time start: when the fact became true
    invalid_at: str | None     # valid time end: when the fact ceased to be true (None = open)
    accessed_at: str
    access_count: int
    source: str
    supersedes_id: str | None  # FK to previous version of this fact

    @property
    def is_current(self) -> bool:
        return self.invalid_at is None

    @property
    def valid_duration_days(self) -> float | None:
        """Days the fact was/has been valid. None if invalid_at is not set."""
        if self.invalid_at is None:
            end = datetime.now(timezone.utc)
        else:
            end = datetime.fromisoformat(self.invalid_at)
        start = datetime.fromisoformat(self.valid_at)
        if start.tzinfo is None:
            start = start.replace(tzinfo=timezone.utc)
        if end.tzinfo is None:
            end = end.replace(tzinfo=timezone.utc)
        return (end - start).total_seconds() / 86400

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "profile": self.profile,
            "content": self.content,
            "memory_type": self.memory_type,
            "confidence_base": self.confidence_base,
            "confidence": self.confidence,
            "created_at": self.created_at,
            "valid_at": self.valid_at,
            "invalid_at": self.invalid_at,
            "accessed_at": self.accessed_at,
            "access_count": self.access_count,
            "source": self.source,
            "supersedes_id": self.supersedes_id,
            "status": "current" if self.is_current else "closed",
        }
```

---

### 9.4 Core Algorithm: `_parse_temporal_arg()`

```python
import re
from datetime import datetime, timezone, timedelta

_DATE_ONLY_RE = re.compile(r'^\d{4}-\d{2}-\d{2}$')


def _parse_temporal_arg(value: str) -> str:
    """
    Parse a user-supplied date/datetime string and return a normalised
    ISO-8601 UTC string suitable for SQLite TEXT column storage.

    Accepted formats:
      YYYY-MM-DD                        → midnight UTC on that day
      YYYY-MM-DDTHH:MM:SS               → assumed UTC
      YYYY-MM-DDTHH:MM:SSZ              → UTC
      YYYY-MM-DDTHH:MM:SS+HH:MM         → converted to UTC

    Raises ValueError on parse failure, with the offending string quoted.
    """
    original = value
    if _DATE_ONLY_RE.match(value):
        value = value + "T00:00:00+00:00"
    elif value.endswith('Z'):
        value = value[:-1] + '+00:00'
    elif 'T' in value and '+' not in value and len(value) == 19:
        value = value + '+00:00'

    try:
        dt = datetime.fromisoformat(value)
    except ValueError:
        raise ValueError(
            f"Cannot parse temporal argument {original!r}. "
            "Expected ISO-8601 date (YYYY-MM-DD) or datetime "
            "(YYYY-MM-DDTHH:MM:SS[Z|+HH:MM])."
        )

    # Normalise to UTC
    dt_utc = dt.astimezone(timezone.utc)
    return dt_utc.isoformat()
```

---

### 9.5 Core Algorithm: Point-in-Time Query (`memory_at_time`)

```python
def memory_at_time(
    conn: sqlite3.Connection,
    profile: str,
    query: str,
    as_of: str,
    *,
    limit: int = 10,
    min_confidence: float = 0.0,
    memory_type: str | None = None,
) -> list[dict]:
    """
    Return memories valid at the given as_of timestamp.

    SQL interval predicate implements Allen's 'contains' relation:
      valid_at <= as_of AND (invalid_at IS NULL OR invalid_at > as_of)

    This covers:
      - Facts that started before as_of and are still open (invalid_at IS NULL)
      - Facts that started before as_of and ended after as_of

    The as_of timestamp is normalised via _parse_temporal_arg() before
    being used in the query.
    """
    ensure_schema(conn)
    as_of_norm = _parse_temporal_arg(as_of)

    # FTS for candidate IDs (same as search_memories)
    try:
        fts_rows = conn.execute(
            "SELECT id FROM semantic_memories_fts WHERE content MATCH ? AND profile=? LIMIT 50",
            (query, profile),
        ).fetchall()
        candidate_ids = {r[0] for r in fts_rows}
    except Exception:
        candidate_ids = None

    if candidate_ids is not None:
        if not candidate_ids:
            return []
        placeholders = ",".join("?" * len(candidate_ids))
        id_clause = f"AND id IN ({placeholders})"
        id_params: list = list(candidate_ids)
    else:
        id_clause = "AND content LIKE ?"
        id_params = [f"%{query}%"]

    type_clause = "AND memory_type=?" if memory_type else ""
    type_params: list = [memory_type] if memory_type else []

    rows = conn.execute(
        f"""
        SELECT id, profile, content, memory_type, confidence, created_at,
               valid_at, invalid_at, accessed_at, access_count, source,
               supersedes_id
        FROM semantic_memories
        WHERE profile=?
          AND valid_at <= ?
          AND (invalid_at IS NULL OR invalid_at > ?)
          {id_clause}
          {type_clause}
        """,
        [profile, as_of_norm, as_of_norm] + id_params + type_params,
    ).fetchall()

    results = []
    for r in rows:
        (mem_id, prof, content, mtype, conf_base, created,
         valid_at, invalid_at, accessed, count, src, sup_id) = r
        effective = compute_confidence(conf_base, mtype, created)
        if effective < min_confidence:
            continue
        results.append({
            "id": mem_id,
            "profile": prof,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "valid_at": valid_at,
            "invalid_at": invalid_at,
            "accessed_at": accessed,
            "access_count": count,
            "source": src,
            "supersedes_id": sup_id,
            "as_of_query": as_of_norm,
        })

    results.sort(key=lambda x: -x["confidence"])
    return results[:limit]
```

---

### 9.6 Core Algorithm: `invalidate_memory()`

```python
def invalidate_memory(
    conn: sqlite3.Connection,
    mem_id: str,
    profile: str,
    *,
    at: str | None = None,
) -> dict:
    """
    Mark a memory as no longer valid by setting invalid_at.

    Parameters
    ----------
    conn     : open SQLite connection (WAL mode)
    mem_id   : the memory ID to invalidate
    profile  : profile scope (prevents cross-profile invalidation)
    at       : ISO-8601 string for when the fact became false; defaults to NOW

    Returns the updated memory dict.

    Raises
    ------
    ValueError  if the memory does not exist, belongs to a different profile,
                or is already invalidated.
    """
    ensure_schema(conn)
    invalid_ts = _parse_temporal_arg(at) if at else _utc_now()

    row = conn.execute(
        """SELECT id, content, memory_type, confidence, created_at, valid_at,
                  invalid_at, source, supersedes_id
           FROM semantic_memories WHERE id=? AND profile=?""",
        (mem_id, profile),
    ).fetchone()

    if row is None:
        raise ValueError(f"Memory {mem_id!r} not found in profile {profile!r}")

    (_, content, mtype, conf, created, valid_at, existing_invalid,
     source, sup_id) = row

    if existing_invalid is not None:
        raise ValueError(
            f"Memory {mem_id!r} is already invalidated (invalid_at={existing_invalid}). "
            "Use 'tag mem history' to inspect the version chain."
        )

    # Ensure invalid_at >= valid_at
    if invalid_ts < valid_at:
        raise ValueError(
            f"invalid_at={invalid_ts!r} must not be before valid_at={valid_at!r}"
        )

    new_source = f"invalidated:{source}"
    conn.execute(
        "UPDATE semantic_memories SET invalid_at=?, source=? WHERE id=? AND profile=?",
        (invalid_ts, new_source, mem_id, profile),
    )
    conn.commit()

    return {
        "id": mem_id,
        "content": content,
        "memory_type": mtype,
        "valid_at": valid_at,
        "invalid_at": invalid_ts,
        "source": new_source,
        "supersedes_id": sup_id,
    }
```

---

### 9.7 Core Algorithm: `memory_history()`

```python
def memory_history(
    conn: sqlite3.Connection,
    mem_id: str,
    profile: str,
) -> list[dict]:
    """
    Return the full version chain for a fact, in ascending valid_at order.

    Traversal strategy:
    1. Walk backward via supersedes_id until root (no supersedes_id).
    2. Walk forward from root via 'SELECT WHERE supersedes_id = parent_id'.
    3. Collect all unique IDs in the chain; fetch them in one query.
    """
    ensure_schema(conn)

    # Step 1: walk to root
    chain_ids: list[str] = []
    current_id: str | None = mem_id
    visited: set[str] = set()

    while current_id and current_id not in visited:
        visited.add(current_id)
        row = conn.execute(
            "SELECT id, supersedes_id FROM semantic_memories WHERE id=? AND profile=?",
            (current_id, profile),
        ).fetchone()
        if row is None:
            break
        chain_ids.append(row[0])
        current_id = row[1]

    root_id = chain_ids[-1] if chain_ids else mem_id

    # Step 2: walk forward from root using a recursive CTE (SQLite 3.8.3+)
    forward_rows = conn.execute(
        """
        WITH RECURSIVE chain(id) AS (
            SELECT id FROM semantic_memories WHERE id=? AND profile=?
            UNION ALL
            SELECT sm.id
            FROM semantic_memories sm
            JOIN chain c ON sm.supersedes_id = c.id
            WHERE sm.profile=?
        )
        SELECT sm.id, sm.content, sm.memory_type, sm.confidence,
               sm.created_at, sm.valid_at, sm.invalid_at,
               sm.accessed_at, sm.access_count, sm.source, sm.supersedes_id
        FROM semantic_memories sm
        JOIN chain c ON sm.id = c.id
        ORDER BY sm.valid_at ASC
        """,
        (root_id, profile, profile),
    ).fetchall()

    results = []
    for r in forward_rows:
        (fid, content, mtype, conf_base, created, valid_at, invalid_at,
         accessed, count, source, sup_id) = r
        effective = compute_confidence(conf_base, mtype, created)
        results.append({
            "id": fid,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "valid_at": valid_at,
            "invalid_at": invalid_at,
            "accessed_at": accessed,
            "access_count": count,
            "source": source,
            "supersedes_id": sup_id,
            "status": "current" if invalid_at is None else "closed",
        })

    return results
```

---

### 9.8 Updated `add_memory()` Signature

```python
def add_memory(
    conn: sqlite3.Connection,
    profile: str,
    content: str,
    *,
    memory_type: str = "fact",
    confidence: float = 1.0,
    source: str = "manual",
    valid_at: str | None = None,        # NEW: ISO-8601 string; defaults to NOW
    supersedes_id: str | None = None,   # NEW: FK to a fact this supersedes
) -> str:
    """
    Insert a new memory. Returns the new memory id.

    When valid_at is None, defaults to the current UTC timestamp (existing behaviour).
    When supersedes_id is provided, the referenced row is automatically invalidated
    at (valid_at - 1 second) if it is not already invalidated, preventing gaps
    or overlaps in the version chain.
    """
    ...
```

The auto-invalidation logic within `add_memory` when `supersedes_id` is provided:

```python
    if supersedes_id is not None:
        # Validate referenced row exists and is open
        ref_row = conn.execute(
            "SELECT invalid_at, valid_at FROM semantic_memories WHERE id=?",
            (supersedes_id,),
        ).fetchone()
        if ref_row is None:
            raise ValueError(f"supersedes_id {supersedes_id!r} does not exist")
        ref_invalid, ref_valid = ref_row
        if ref_invalid is not None:
            raise ValueError(
                f"supersedes_id {supersedes_id!r} is already invalidated. "
                "Cannot create a supersession of a closed fact."
            )
        # Auto-close the predecessor at new fact's valid_at
        effective_valid_at = valid_at_norm  # already parsed
        conn.execute(
            "UPDATE semantic_memories SET invalid_at=? WHERE id=?",
            (effective_valid_at, supersedes_id),
        )
```

---

### 9.9 Integration with `loop_agent.py`

The memory-injection call site in `loop_agent.py` is updated to accept an optional `as_of` parameter:

```python
# In loop_agent.py — existing call (unchanged signature when as_of=None):
memories = search_memories(conn, profile, goal_summary, limit=3)

# New eval-replay call path:
if replay_as_of:
    memories = memory_at_time(conn, profile, goal_summary, as_of=replay_as_of, limit=3)
else:
    memories = search_memories(conn, profile, goal_summary, limit=3)
```

The `replay_as_of` value is sourced from `run.started_at` in the `runs` table when the eval harness (PRD-027) triggers a replay run.

---

### 9.10 OTel Span Attributes

Following `otel_semconv.py` conventions (PRD-013, PRD-041):

```python
# In semantic_memory.py, wrapped around key functions:
with tracer.start_as_current_span("memory.search") as span:
    span.set_attribute("memory.profile", profile)
    span.set_attribute("memory.query", query)
    span.set_attribute("memory.as_of_query", as_of or "current")
    span.set_attribute("memory.result_count", len(results))

with tracer.start_as_current_span("memory.invalidate") as span:
    span.set_attribute("memory.id", mem_id)
    span.set_attribute("memory.valid_at", valid_at)
    span.set_attribute("memory.invalid_at", invalid_ts)
```

New semconv constants added to `otel_semconv.py`:

```python
MEM_VALID_AT       = "memory.valid_at"
MEM_INVALID_AT     = "memory.invalid_at"
MEM_AS_OF_QUERY    = "memory.as_of_query"
MEM_SUPERSEDES_ID  = "memory.supersedes_id"
MEM_HISTORY_DEPTH  = "memory.history_depth"
```

---

## 10. Security Considerations

1. **No new SQL injection vectors:** All user-supplied values (dates, fact content, IDs) are passed as SQLite positional parameters (`?`) in every query. No string interpolation is used in query construction. The `_parse_temporal_arg()` function validates and normalises before the value ever reaches a SQL statement.

2. **Profile isolation enforcement:** `invalidate_memory()` and `memory_history()` always include `AND profile=?` in their WHERE clauses. A caller cannot invalidate or inspect a fact belonging to a different profile by guessing an ID, consistent with the existing access-control pattern in PRD-034.

3. **Back-dating bounds check:** FR-18 requires that `--valid-from` dates more than 50 years in the past be rejected unless `--force` is passed. This prevents accidental epoch-zero or overflowed-timestamp entries from corrupting interval queries. The 1-year future bound prevents forward-dated facts that would silently disappear from `--as-of NOW` queries.

4. **Recursive CTE depth limit:** The recursive CTE in `memory_history()` has no explicit depth limit in SQLite, which defaults to 1000. A pathological `supersedes_id` cycle (if somehow introduced by a DB corruption or direct SQL manipulation) would be caught at depth 1000 rather than looping forever. A visited-set guard in the backward-walk phase (Step 1 in §9.7) provides a second layer of protection against cycles.

5. **No new network surface:** This feature is entirely local to the SQLite database at `~/.tag/runtime/tag.sqlite3`. No network calls, no external APIs, no RPC. The temporal metadata is never transmitted outside the local machine by this feature.

6. **Invalidation is not deletion:** The `invalid_at` pattern preserves full history. From a compliance and audit perspective, this is strictly stronger than the previous hard-delete approach: invalidated facts cannot be silently removed. Users who need full deletion can still call `forget_memory()`, which remains available and performs a hard DELETE consistent with prior behaviour.

7. **WAL mode concurrency:** SQLite in WAL mode allows concurrent reads alongside a write. `invalidate_memory()` is a single-row UPDATE that holds a write lock for microseconds. No new concurrency hazards are introduced beyond those already present in the `add_memory()` + `forget_memory()` patterns.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_semantic_memory.py`)

All new tests are appended to the existing test file. Existing tests are not modified.

| Test | What it verifies |
|------|-----------------|
| `test_add_with_valid_from_backdated` | `add_memory(..., valid_at="2024-01-01")` stores correct `valid_at`; default search returns the row (it is open). |
| `test_add_default_valid_at` | `add_memory()` without `valid_at` sets `valid_at` to approximately NOW (within 2 seconds). |
| `test_invalidate_sets_invalid_at` | `invalidate_memory()` sets `invalid_at`; subsequent default `search_memories()` does not return the row. |
| `test_invalidate_already_closed_raises` | `invalidate_memory()` on an already-invalidated row raises `ValueError`. |
| `test_invalidate_at_past_date` | `invalidate_memory(..., at="2025-01-01")` sets `invalid_at="2025-01-01T00:00:00+00:00"`. |
| `test_invalidate_before_valid_at_raises` | `invalidate_memory` with `at` before `valid_at` raises `ValueError`. |
| `test_search_as_of_returns_correct_version` | Insert two versions of a fact (v1 valid 2024-01, closed 2025-01; v2 valid 2025-01). `search --as-of 2024-06` returns v1; `search --as-of 2025-06` returns v2. |
| `test_search_as_of_excludes_future_facts` | A fact with `valid_at = 2030-01-01` is not returned by `search --as-of 2025-01-01`. |
| `test_search_default_excludes_invalidated` | Invalidated rows are not returned by `search_memories()` without `as_of`. |
| `test_memory_history_two_versions` | `memory_history()` for a two-version chain returns both in correct order. |
| `test_memory_history_single_version` | `memory_history()` for a root-only fact returns a single-element list. |
| `test_memory_history_wrong_profile_raises` | `memory_history(mem_id, wrong_profile)` returns empty list (not a cross-profile leak). |
| `test_add_with_supersedes_id_auto_closes_predecessor` | `add_memory(..., supersedes_id=old_id)` automatically sets `old_id.invalid_at`. |
| `test_add_supersedes_already_closed_raises` | `add_memory(..., supersedes_id=closed_id)` raises `ValueError`. |
| `test_parse_temporal_arg_formats` | All four accepted formats parse to the expected UTC ISO-8601 string. |
| `test_parse_temporal_arg_invalid_raises` | Nonsense string raises `ValueError` quoting the offending value. |
| `test_schema_migration_idempotent` | Calling `ensure_schema()` twice on the same DB does not raise. |
| `test_migration_preserves_existing_rows` | Insert row without temporal columns; run migration; row exists with `valid_at = created_at` and `invalid_at = NULL`. |
| `test_list_all_includes_invalidated` | `list_memories(..., include_invalid=True)` returns both open and closed rows. |
| `test_list_default_excludes_invalidated` | `list_memories()` without flags returns only open rows. |

### 11.2 Integration Tests (`tests/test_prd_features.py`)

```python
# Sketch of PRD-069 acceptance block
def test_prd069_temporal_roundtrip(tmp_db):
    """Full workflow: add → search → invalidate → as-of search → history."""
    conn = open_db(tmp_db)
    # Add v1
    id_v1 = add_memory(conn, "default", "Team uses Python 3.11",
                       valid_at="2024-11-01")
    # Confirm current search finds v1
    results = search_memories(conn, "default", "Python")
    assert any(r["id"] == id_v1 for r in results)
    # Invalidate v1, add v2
    invalidate_memory(conn, id_v1, "default", at="2025-06-01")
    id_v2 = add_memory(conn, "default", "Team uses Python 3.12",
                       valid_at="2025-06-01", supersedes_id=id_v1)
    # Current search finds only v2
    results = search_memories(conn, "default", "Python")
    ids = [r["id"] for r in results]
    assert id_v2 in ids
    assert id_v1 not in ids
    # as-of query in v1's window returns v1
    past = memory_at_time(conn, "default", "Python", as_of="2025-01-15")
    assert any(r["id"] == id_v1 for r in past)
    # as-of query in v2's window returns v2
    present = memory_at_time(conn, "default", "Python", as_of="2025-07-01")
    assert any(r["id"] == id_v2 for r in present)
    # History shows both versions
    history = memory_history(conn, id_v1, "default")
    assert len(history) == 2
    assert history[0]["id"] == id_v1
    assert history[1]["id"] == id_v2
```

### 11.3 Performance Tests (`tests/perf/test_temporal_perf.py`)

```python
import time
import sqlite3

def test_as_of_query_100k_rows(tmp_path):
    """as-of query on 100k rows completes in < 50 ms."""
    conn = sqlite3.connect(str(tmp_path / "bench.db"))
    ensure_schema(conn)
    # Bulk insert 100k synthetic rows with varied valid_at/invalid_at
    ...  # use executemany with random date ranges
    # Warm up
    memory_at_time(conn, "bench", "Python", as_of="2025-01-01")
    # Timed run
    t0 = time.perf_counter()
    for _ in range(10):
        memory_at_time(conn, "bench", "Python", as_of="2025-01-01", limit=10)
    elapsed_ms = (time.perf_counter() - t0) / 10 * 1000
    assert elapsed_ms < 50, f"as-of query took {elapsed_ms:.1f} ms (> 50 ms)"

def test_default_search_overhead(tmp_path):
    """Default search on 100k rows is within 5% of pre-temporal baseline."""
    ...
```

---

## 12. Acceptance Criteria

| ID | Criterion | How to Verify |
|----|-----------|--------------|
| AC-01 | `tag mem add "X" --valid-from 2024-01-01` stores `valid_at = "2024-01-01T00:00:00+00:00"` in the DB | `sqlite3 tag.sqlite3 "SELECT valid_at FROM semantic_memories WHERE content='X'"` |
| AC-02 | `tag mem add "X"` (no flag) stores `valid_at` within 2 seconds of the current UTC time | Integration test asserting `abs((NOW - valid_at).total_seconds()) < 2` |
| AC-03 | `tag mem update <id> --invalidate` sets `invalid_at` to approximately NOW | Check DB row directly after command |
| AC-04 | `tag mem update <id> --invalidate --at 2025-03-01` sets `invalid_at = "2025-03-01T00:00:00+00:00"` | Check DB row |
| AC-05 | After `--invalidate`, `tag mem search "X"` does not return the invalidated row | Run search, assert ID absent from results |
| AC-06 | `tag mem search "X" --as-of 2025-01-15` returns the row that was valid on that date and not the superseding row | Run search with both `--as-of` values; assert correct IDs |
| AC-07 | `tag mem update <id> --invalidate` on an already-invalidated row exits with code 1 and prints an error mentioning `invalid_at` | Run command; check exit code and stderr |
| AC-08 | `tag mem history <id>` output lists all versions in ascending `valid_at` order | Run command; parse output; assert ordering |
| AC-09 | `tag mem history <id> --json` output is valid JSON matching the schema in §6.4 | `tag mem history <id> --json | python -m json.tool` exits 0 |
| AC-10 | All existing `tests/test_semantic_memory.py` tests pass without modification | `pytest tests/test_semantic_memory.py` exits 0 |
| AC-11 | `ensure_schema()` called twice on the same DB does not raise an exception | `test_schema_migration_idempotent` unit test |
| AC-12 | `tag mem add "X" --valid-from 1900-01-01` without `--force` exits 1 with a message about the 50-year bound | Run command; check exit code and message |
| AC-13 | `memory_at_time()` on a 100,000-row table completes in < 50 ms | `test_as_of_query_100k_rows` passes |
| AC-14 | Adding a memory with `--valid-from` does not affect the `compute_confidence()` decay clock (decay is still driven by `created_at`, not `valid_at`) | Unit test: back-dated fact with `valid_at = 5 years ago` has same confidence as a fact created today and tested at day 0 |
| AC-15 | `tag mem list --all` includes invalidated rows with `status: "closed"` and open rows with `status: "current"` | Run command on a DB with both; assert both statuses present |
| AC-16 | `loop_agent.py` eval replay path with `as_of = past_date` injects only facts valid at `past_date` | Integration test asserting injected context differs from current-facts context |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-025 Semantic Memory with Confidence Decay | Predecessor (required) | This PRD extends `semantic_memory.py` schema and functions established by PRD-025. PRD-025 must be merged before this PRD. |
| PRD-013 Agent Tracing/Observability | Soft dependency | OTel span attributes (§9.10) follow conventions established in PRD-013. Feature works without tracing enabled; spans are no-ops when `OTEL_EXPORTER_OTLP_ENDPOINT` is not set. |
| PRD-027 Eval Framework | Consumer | PRD-027's eval replay path is the primary consumer of `memory_at_time()`. This PRD must be available for PRD-027 to support reproducible historical eval scores. |
| PRD-034 Secret Scanning | Informational | Profile-isolation enforcement in this PRD is consistent with PRD-034's access control model. No direct code dependency. |
| SQLite 3.35+ | Runtime | `ALTER TABLE … ADD COLUMN IF NOT EXISTS` requires SQLite 3.35.0 (released 2021-03-12). Python 3.10+ ships with SQLite ≥ 3.37 on all platforms TAG supports. The `ensure_schema()` fallback path handles older SQLite via a version check and `try/except` on the `IF NOT EXISTS` clause. |
| SQLite 3.8.3+ | Runtime | Recursive CTEs (used in `memory_history()`) require SQLite 3.8.3. This is satisfied by Python 3.8+, which ships SQLite 3.31+ on all platforms. |
| Python 3.10+ | Runtime | `str | None` union syntax in type hints. Already required by the project. |

---

## 14. Open Questions

| # | Question | Owner | Status |
|---|----------|-------|--------|
| OQ-1 | Should `tag mem add --valid-from` with an explicit `--supersedes <id>` flag auto-close the predecessor at `valid_from - 1 second`, or require a separate `--invalidate` call first? The current design auto-closes (§9.8). Is that too magical? | @product | Open |
| OQ-2 | Should `confidence` decay be driven by `valid_at` rather than `created_at` for temporal memories? The argument: a fact that was valid until last week is more recent than its `created_at` suggests. Counter-argument: `created_at` measures epistemic recency (when we learned it), which is what decay should model. | @ml | Open |
| OQ-3 | Should there be a `tag mem add ... --supersedes <id>` shorthand that combines `--invalidate` of the old fact and creation of the new one in a single atomic transaction? This would be the most ergonomic API for the common "update a fact" workflow. | @eng | Open |
| OQ-4 | If TAG ever migrates from SQLite to PostgreSQL, should the schema use `tstzrange` with a GiST index and exclusion constraint (`EXCLUDE USING gist (profile WITH =, memory_type WITH =, valid_period WITH &&)`) to enforce non-overlapping intervals at the DB level? This is the Zep/bitemporal SQL best practice. Document as a future migration path. | @infra | Future |
| OQ-5 | Should `tag mem history` also display the `confidence` effective value at the time of query, or the effective value at each version's `valid_at` midpoint? Currently it shows effective confidence as of today (consistent with `list_memories`). | @product | Open |
| OQ-6 | Is there a use case for **transaction-time** queries (i.e., "what did the database say about this fact as of last Tuesday, regardless of what it claims the real-world validity was")? This would be a full bitemporal model. Current PRD implements valid-time only. | @product | Future |
| OQ-7 | Should invalidated memories continue to be included in the FTS5 index? Currently yes (they are findable via `--all`). If the FTS index grows large, a periodic vacuum of the FTS table for memories invalidated more than N days ago could be useful. | @eng | Open |
| OQ-8 | Does the 50-year bound (FR-18) cause issues for customers recording historical decisions about legacy systems? Should the bound be configurable via `tag config set memory.max_backdate_years <N>`? | @product | Open |

---

## 15. Complexity and Timeline

**Overall estimate: M (7–10 working days)**

### Phase 1 — Schema and Core API (Days 1–3)

- Day 1: Write and land the SQLite DDL migration inside `ensure_schema()`. Add `_parse_temporal_arg()` with full test coverage. Add `valid_at`, `invalid_at`, `supersedes_id` columns to `add_memory()`. Update FTS sync logic.
- Day 2: Implement `invalidate_memory()` with profile isolation and error handling. Implement `memory_at_time()` with the Allen-interval WHERE clause and FTS pre-filter.
- Day 3: Implement `memory_history()` with recursive CTE traversal. Write `TemporalMemory` dataclass. Run full existing test suite to confirm zero regressions.

### Phase 2 — CLI Surface (Days 4–5)

- Day 4: Wire `--valid-from` into `cmd_mem_add` in `controller.py`. Wire `--invalidate` / `--at` into `cmd_mem_update`. Update `tag mem list` to support `--all` and `--as-of`.
- Day 5: Implement `tag mem history` subcommand (human table output + `--json`). Wire `--as-of` into `cmd_mem_search`. Add bounds-check (FR-18) with `--force` override.

### Phase 3 — Integration and Observability (Days 6–7)

- Day 6: Update `loop_agent.py` to accept and thread `as_of` parameter. Add OTel span attributes to all new functions following `otel_semconv.py` conventions. Add new semconv constants.
- Day 7: Write integration tests in `tests/test_prd_features.py`. Write performance benchmarks in `tests/perf/test_temporal_perf.py`. Validate all AC items manually.

### Phase 4 — Polish and Documentation (Days 8–10)

- Day 8: Edge case hardening: cycle detection in history traversal, SQLite version compatibility fallback for `IF NOT EXISTS`, FTS consistency validation.
- Day 9: Update `docs/prd/INDEX.md`. Review all error messages for clarity and actionability. Run `tag doctor` to verify schema migration path end-to-end on a clean install.
- Day 10: Final review, address open questions with team, merge.

### Risk Factors

| Risk | Likelihood | Mitigation |
|------|-----------|------------|
| SQLite `ALTER TABLE` behaviour differs between SQLite versions shipped with Python 3.10 vs 3.12 | Low | Wrap in `try/except` with version check; integration-tested on both. |
| Recursive CTE for history traversal exceeds SQLite depth limit on very long chains | Low | Visited-set guard in backward walk; depth limit documented in security section. |
| FTS5 sync overhead exceeds NFR-09 threshold (10%) | Low | FTS rows for invalidated memories are not updated (only the main table is); FTS remains in sync for searchability. |
| `compute_confidence` decay interacting unexpectedly with back-dated `valid_at` vs `created_at` | Medium | OQ-2 is flagged; unit test AC-14 specifically verifies decay is driven by `created_at`, not `valid_at`. |

