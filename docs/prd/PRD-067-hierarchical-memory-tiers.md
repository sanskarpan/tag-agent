# PRD-067: Hierarchical Memory Tiers: Core / Recall / Archival (`tag mem tier`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** L (2-4 weeks)
**Category:** Memory & Knowledge
**Affects:** `semantic_memory.py`
**Depends on:** PRD-025 (semantic memory with confidence decay), PRD-027 (eval framework), PRD-028 (sandbox), PRD-013 (agent tracing/observability), PRD-034 (secret scanning), PRD-039 (token budget enforcement), PRD-043 (vector-based tool retrieval)
**Inspired by:** Letta/MemGPT memory tiers, Zep memory hierarchy, HippoRAG
**GitHub Issue:** #345

---

## 1. Overview

TAG's existing semantic memory (PRD-025) stores all memories in a flat table with confidence-based decay and FTS5 full-text search. Every memory — whether a permanent architectural decision, a transient PR review note, or a years-old fact that has long since been superseded — competes on equal footing for the same retrieval pool. There is no concept of primacy: a core project convention that must always be in context occupies the same tier as a disposable session observation. This flat model forces agents into a dilemma: either retrieve too little (missing critical pinned context) or retrieve too much (flooding the context window with stale, low-relevance memories and burning tokens unnecessarily).

This PRD introduces a three-tier memory hierarchy modeled on the Letta/MemGPT virtual-memory analogy and informed by Zep's temporal edge semantics and HippoRAG's graph-based retrieval pattern. The three tiers are **core** (always injected verbatim into every agent system prompt, like OS RAM), **recall** (retrieved on relevance via hybrid search when context permits, like a page cache), and **archival** (persisted but never automatically surfaced — requires explicit search, like cold disk storage). Tier assignment can be set manually at write time or tuned later via `tag mem tier promote` / `tag mem tier demote`. An auto-paging policy automatically demotes the least-relevant recall memories to archival when the context window budget for memory is exceeded, exactly as an OS page-replaces cold frames.

The feature preserves full backward compatibility with PRD-025. All existing memories without an explicit tier are treated as `recall`. The confidence-decay algorithm (exponential with type-specific half-lives) continues to drive relevance ranking within the recall tier. Core memories are exempt from decay because they are pinned; archival memories still accumulate decay but are never automatically surfaced. The DDL migration is additive — a single `tier` column with a default of `'recall'` is added to the existing `semantic_memories` table.

A two-phase LLM pipeline (borrowed from mem0's FACT_RETRIEVAL_PROMPT / UPDATE_MEMORY_PROMPT architecture) is added as an optional agent callback: the agent's conversation turns are processed to extract facts, those facts are reconciled against existing memory via ADD/UPDATE/DELETE/NOOP classification, and the resulting operations are applied at the correct tier. The auto-paging daemon runs as a lightweight background check at agent session start, ensuring the core tier never exceeds its configured token budget without operator intervention.

The net result is a memory subsystem that correctly mirrors how human working memory operates: a small set of always-present facts (core), a larger pool of relevant-when-needed memories (recall), and a deep archive of historical context that is findable but never intrusive (archival). Agents that use this system spend fewer tokens on irrelevant context, maintain stronger awareness of invariant project facts, and produce more consistent outputs across long-running sessions.

---

## 2. Problem Statement

### 2.1 Flat memory pools degrade agent context quality as memory grows

PRD-025's `semantic_memories` table today stores all entries in a single flat namespace sorted by decayed confidence. In practice, this means that a memory like `"The project uses PostgreSQL as its primary database"` — a fact that should be present in every agent context — competes with `"Reviewed PR #123 on 2026-01-15"` — a disposable session note — for the same retrieval slot. As the memory store grows beyond a few hundred entries, the top-K retrieval window becomes increasingly noisy: high-confidence but contextually irrelevant entries crowd out memories that are semantically relevant to the current query. Agents begin generating responses that contradict known conventions because those conventions simply did not make it into the retrieved context. There is currently no mechanism to guarantee that certain memories are always present regardless of retrieval ranking.

### 2.2 Context window budgets are exhausted by untriaged memory injection

PRD-039 (token budget enforcement) caps the total tokens available to an agent invocation. Semantic memory retrieval currently operates without awareness of this budget: `search_memories()` returns up to `limit` entries and all of them are injected into the system prompt unconditionally. On profiles with many stored memories and a model with a small context window (e.g., a local Llama-3 model via Ollama with an 8k context), the memory injection alone can exhaust the available budget before the task description is even written. There is no prioritization signal that tells the agent "this memory is mandatory; that one can be dropped if space is tight." A tiered model with explicit promotion/demotion and an auto-paging policy solves this by ensuring core memories are always injected first, recall memories fill remaining budget in relevance order, and archival memories are never injected automatically.

### 2.3 There is no lifecycle management for memories as projects evolve

Memories added during early exploration of a codebase (e.g., `"The auth module uses JWT tokens"`) may become stale or wrong as the project evolves. Without tier semantics, stale memories linger in the active retrieval pool indefinitely, degrading agent accuracy. Operators have no way to say "this memory is archival — keep it for historical reference but stop surfacing it in active context." The `forget_memory()` function in PRD-025 offers only a binary choice: keep the memory in the active pool or delete it permanently. A three-tier model introduces a third option — demote to archival — that preserves historical accuracy without polluting active context.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Introduce three named memory tiers — `core`, `recall`, `archival` — with distinct retrieval semantics enforced at the database and application layers. |
| G2 | Core memories are always injected into the agent system prompt, exempt from eviction, and immune to confidence decay. Their combined token footprint must be tracked and bounded by a configurable `mem.core_token_limit` (default: 2000 tokens). |
| G3 | Recall memories are retrieved via hybrid search (FTS5 + semantic similarity scoring) ranked by decayed confidence, filling the remaining memory budget after core injection. |
| G4 | Archival memories are persisted and searchable via explicit `tag mem search --tier archival` but are never injected automatically into agent context. |
| G5 | Auto-paging: when the recall tier exceeds `mem.recall_soft_limit` entries (default: 200), the lowest-scoring recall memories are automatically demoted to archival in a background sweep at session start. |
| G6 | The CLI surface `tag mem add --tier`, `tag mem tier list`, `tag mem tier promote`, `tag mem tier demote` provides full operator control over tier placement. |
| G7 | The optional two-phase LLM extraction pipeline (FACT_RETRIEVAL + RECONCILE) adds memories at the correct tier based on a configurable heuristic (fact type → tier mapping). |
| G8 | Full backward compatibility: all memories created before this feature is deployed are treated as `recall`. No existing data is lost or modified by the migration. |
| G9 | `tag mem tier stats` reports per-tier entry counts, token footprints, and auto-paging event history. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Replacing PRD-025's confidence decay algorithm. Decay continues to operate within the recall tier. Core tier is exempt from decay. Archival tier accumulates decay but this does not trigger automatic deletion. |
| NG2 | Graph-based memory (entity-relationship triples, community detection, Personalized PageRank). This is a separate future PRD. The tier system is a prerequisite for, not a replacement of, graph memory. |
| NG3 | Multi-user or team-shared memory. All memory remains profile-scoped, as in PRD-025. |
| NG4 | Automatic LLM-driven tier assignment at write time without explicit operator intent. The LLM extraction pipeline assigns tiers based on a configurable fact-type-to-tier mapping, not autonomous inference. |
| NG5 | Replacing LanceDB or ChromaDB as the vector backend. This PRD adds tier logic on top of the existing SQLite FTS5 + optional sentence-transformer embedding path from PRD-025 and PRD-043. |
| NG6 | Real-time streaming injection of memory updates during a live agent conversation turn. Tier assignment and auto-paging run at session start and at `mem add` call time, not mid-turn. |
| NG7 | Web UI for memory management. The CLI surface defined in Section 6 is the only operator interface in scope. PRD-054 (browser dev UI) may surface tiers in a future extension. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|-------------------|
| Core memory guaranteed injection rate | 100% of agent runs inject all core memories when `core_token_limit` is not exceeded | Unit test asserting core memories appear in every built context |
| Auto-paging trigger rate | Auto-paging fires and demotes ≥ 1 memory when recall count exceeds `recall_soft_limit` | Integration test seeding 201 recall memories and asserting demotion occurs at next session start |
| p95 session-start latency overhead (auto-paging) | ≤ 50ms on a memory store with 10,000 entries | Benchmark on macOS with SQLite WAL mode |
| Context token reduction | Average tokens injected from memory reduced by ≥ 20% after tiering, relative to flat-pool baseline | Compare token counts before/after tiering on a reference profile with 500 memories |
| Backward compatibility | Zero existing memories deleted or tier-changed by migration | Migration test asserting all pre-migration rows have `tier = 'recall'` |
| CLI round-trip correctness | `tag mem add --tier core` followed by `tag mem tier list --tier core` shows the added memory | Integration test |
| Promote/demote audit trail | Every promote/demote event is recorded in `mem_tier_events` with actor, timestamp, and old/new tier | DB assertion after promote call |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer working on a long-running project | add `"The project uses PostgreSQL 16 with pgvector"` as a `core` memory | Every agent I run always knows this fact without me having to repeat it in every prompt |
| U2 | Developer doing code review | add `"Reviewed PR #123 on 2026-01-15; merged, no issues"` as a `recall` memory | The review note is searchable when I need it but doesn't pollute core context |
| U3 | Project lead | promote an existing memory from `recall` to `core` after it proves consistently useful | High-value facts get elevated to guaranteed context without needing to recreate them |
| U4 | Developer | demote a memory to `archival` when a project convention has changed | The old convention is preserved for historical reference but no longer misleads the agent |
| U5 | Platform engineer | run `tag mem tier list --json` to see all tier assignments | I can audit the memory configuration programmatically and pipe it to other tools |
| U6 | Developer using a model with a small context window | configure `mem.core_token_limit = 800` | Core memories fit within the available budget for local models |
| U7 | Developer | run `tag mem tier stats` and see per-tier counts and token footprints | I know when my core tier is approaching its budget and needs pruning |
| U8 | Developer | search archival memories explicitly with `tag mem search --tier archival "old convention"` | I can recover historical context without it contaminating active runs |
| U9 | Agent (automated) | trigger auto-paging to demote lowest-confidence recall memories when limit is exceeded | The recall pool stays lean and high-quality without manual intervention |
| U10 | Developer | see a compact tier badge (`[C]`, `[R]`, `[A]`) next to each memory in `tag mem list` | I can quickly scan tier assignments in the standard memory listing without a separate command |

---

## 6. Proposed CLI Surface

All memory subcommands live under the `tag mem` namespace. The `tag mem tier` subgroup is new; existing `tag mem add`, `tag mem list`, `tag mem forget`, and `tag mem search` are extended with `--tier` flags.

### 6.1 `tag mem add` (extended)

```
tag mem add "<content>" [--tier core|recall|archival] [--type fact|decision|convention|gotcha|other]
            [--confidence 0.0-1.0] [--source manual|agent|import] [--profile <name>] [--json]
```

**Examples:**

```bash
# Add a pinned core memory
tag mem add "The project uses PostgreSQL 16 with pgvector extension" --tier core

# Add a transient recall memory (default tier)
tag mem add "Reviewed PR #123 on 2026-01-15; merged cleanly" --tier recall

# Add a historical archival memory
tag mem add "We used MySQL before 2025-03 migration" --tier archival --type decision

# Add with explicit confidence
tag mem add "Build system is Bazel (experimental)" --tier recall --confidence 0.7 --type convention
```

**Output (plain):**
```
Added memory abc1234def5678 [core] "The project uses PostgreSQL 16 with pgvector extension"
```

**Output (`--json`):**
```json
{
  "id": "abc1234def5678",
  "tier": "core",
  "memory_type": "fact",
  "confidence": 1.0,
  "created_at": "2026-06-12T10:30:00Z"
}
```

### 6.2 `tag mem tier list`

List memories grouped by tier with tier-level statistics.

```
tag mem tier list [--tier core|recall|archival] [--profile <name>] [--limit N] [--json]
```

**Output (plain TTY):**
```
Tier: CORE  (3 entries, ~420 tokens)
────────────────────────────────────────────────────────────
  abc1234 [fact]      conf:1.000  "The project uses PostgreSQL 16..."
  bcd2345 [convention] conf:1.000  "All Python files use ruff for linting..."
  cde3456 [decision]  conf:1.000  "Auth module uses JWT with 24h expiry..."

Tier: RECALL  (147 entries, ~8,200 tokens)
────────────────────────────────────────────────────────────
  def4567 [fact]      conf:0.923  "Reviewed PR #123 on 2026-01-15..."
  efg5678 [gotcha]    conf:0.871  "SQLite WAL mode must be enabled..."
  ...

Tier: ARCHIVAL  (52 entries)
────────────────────────────────────────────────────────────
  fgh6789 [decision]  conf:0.201  "We used MySQL before 2025-03..."
  ...
```

**Output (`--json`):**
```json
{
  "profile": "default",
  "tiers": {
    "core": {
      "count": 3,
      "estimated_tokens": 420,
      "token_limit": 2000,
      "memories": [
        {
          "id": "abc1234def5678",
          "content": "The project uses PostgreSQL 16 with pgvector extension",
          "memory_type": "fact",
          "confidence": 1.0,
          "tier": "core",
          "created_at": "2026-06-12T10:30:00Z",
          "accessed_at": "2026-06-12T14:22:00Z",
          "access_count": 42
        }
      ]
    },
    "recall": { "count": 147, "estimated_tokens": 8200, "memories": ["..."] },
    "archival": { "count": 52, "memories": ["..."] }
  }
}
```

### 6.3 `tag mem tier promote`

Move a memory to a higher tier.

```
tag mem tier promote <id> --to core|recall [--profile <name>] [--json]
```

**Examples:**
```bash
# Promote from recall to core
tag mem tier promote def4567 --to core

# Promote from archival to recall
tag mem tier promote fgh6789 --to recall
```

**Output:**
```
Promoted memory def4567: recall → core
Core tier: 4 entries, ~580 tokens (limit: 2000 tokens)
```

**Error cases:**
- `--to archival` is rejected: use `tag mem tier demote` instead.
- Promoting to `core` when `core_token_limit` would be exceeded:
  ```
  Error: promoting memory def4567 to core would exceed core_token_limit (2000 tokens).
  Core tier currently: 1,950 tokens. Memory adds ~85 tokens.
  Use --force to promote anyway, or demote another core memory first.
  ```

### 6.4 `tag mem tier demote`

Move a memory to a lower tier.

```
tag mem tier demote <id> --to recall|archival [--profile <name>] [--reason <text>] [--json]
```

**Examples:**
```bash
# Demote from core to recall
tag mem tier demote abc1234 --to recall

# Demote from recall to archival with audit reason
tag mem tier demote efg5678 --to archival --reason "Convention changed in v2.0 refactor"
```

**Output:**
```
Demoted memory abc1234: core → recall
Recorded reason: "Convention changed in v2.0 refactor"
```

### 6.5 `tag mem tier stats`

Show per-tier statistics and auto-paging history.

```
tag mem tier stats [--profile <name>] [--history N] [--json]
```

**Output:**
```
Memory Tier Statistics — profile: default
─────────────────────────────────────────────
Tier       Entries   Est. Tokens   Limit
core             3         420    2,000  [21% used]
recall         147       8,200      200* [74% of soft limit]
archival        52           —      —

* recall soft limit: auto-page fires at >200 entries

Auto-Paging History (last 5 events):
  2026-06-11T22:00:00Z  demoted 8 memories (recall→archival), freed 3,200 tokens
  2026-06-10T22:00:00Z  demoted 5 memories (recall→archival), freed 2,100 tokens
```

### 6.6 `tag mem list` (extended with tier badge)

The existing `tag mem list` command gains a compact tier badge:

```
tag mem list [--tier core|recall|archival] [--type <type>] [--limit N] [--json]
```

**Output:**
```
[C] abc1234  fact     conf:1.000  "The project uses PostgreSQL 16..."
[C] bcd2345  conv     conf:1.000  "All Python files use ruff..."
[R] def4567  fact     conf:0.923  "Reviewed PR #123 on 2026-01-15..."
[R] efg5678  gotcha   conf:0.871  "SQLite WAL mode must be enabled..."
[A] fgh6789  decision conf:0.201  "We used MySQL before 2025-03..."
```

Badge legend: `[C]` = core, `[R]` = recall, `[A]` = archival.

### 6.7 `tag mem search` (extended)

```
tag mem search "<query>" [--tier core|recall|archival|all] [--type <type>]
               [--limit N] [--min-confidence F] [--json]
```

Default behavior (no `--tier`): searches core + recall only (backward compatible).
`--tier archival`: explicitly searches the archival tier.
`--tier all`: searches all three tiers.

---

## 7. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | **Tier column:** The `semantic_memories` table gains a `tier` column of type `TEXT NOT NULL DEFAULT 'recall'` constrained to `CHECK(tier IN ('core','recall','archival'))`. The migration runs via `ensure_schema()` using `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` for idempotency. |
| FR-02 | **Tier-aware add:** `add_memory()` accepts an optional `tier: str = 'recall'` parameter. If `tier='core'` and the new memory would cause the core tier's estimated token count to exceed `mem.core_token_limit`, the call raises `CoreTierBudgetExceededError` with details unless `force=True` is passed. |
| FR-03 | **Core injection:** A new function `build_memory_context(conn, profile, budget_tokens)` returns two lists: `core_memories` (all tier='core' entries, always included) and `recall_memories` (top-K tier='recall' entries by decayed confidence, filling the remaining token budget after core injection). Archival memories are never returned by this function. |
| FR-04 | **Core decay exemption:** `compute_confidence()` returns the stored `confidence_base` unchanged when `tier='core'`, bypassing the age-decay formula. This is implemented by checking the `tier` parameter in the function signature. |
| FR-05 | **Recall search:** `search_memories()` defaults to searching tier='recall' only. A `tiers` parameter (list, default `['recall']`) controls which tiers are included. Existing callers that do not pass `tiers` continue to receive recall-only results, preserving backward compatibility. |
| FR-06 | **Archival exclusion from auto-injection:** `build_memory_context()` must never include tier='archival' memories in its output regardless of their confidence score. |
| FR-07 | **Auto-paging policy:** A function `autopage_recall(conn, profile, soft_limit, dry_run=False)` queries the count of tier='recall' memories. If the count exceeds `soft_limit`, it computes decayed confidence for all recall memories, sorts ascending, and demotes the bottom `(count - soft_limit)` entries to tier='archival', recording each demotion in `mem_tier_events` with `actor='autopager'`. |
| FR-08 | **Auto-paging trigger point:** `autopage_recall()` is called automatically at the start of each agent session (in `controller.py`'s session setup path) and is also triggerable manually via `tag mem tier autopage [--dry-run]`. |
| FR-09 | **Tier event audit log:** Every tier transition (manual promote/demote, auto-page demotion) is recorded in the `mem_tier_events` table with `memory_id`, `old_tier`, `new_tier`, `actor` (`'manual'`, `'autopager'`, or `'llm_pipeline'`), `reason` (optional free text), and `event_at` timestamp. |
| FR-10 | **Promote/demote CLI:** `tag mem tier promote <id> --to <tier>` and `tag mem tier demote <id> --to <tier>` validate that the direction is valid (promote = toward core, demote = toward archival), update the `tier` column, and write to `mem_tier_events`. Invalid direction transitions exit 1 with a clear error. |
| FR-11 | **Token estimation:** A helper `estimate_tokens(text: str) -> int` uses the heuristic `max(1, len(text) // 4)` (1 token per 4 characters, a conservative estimate). This is used for `CoreTierBudgetExceededError` and `tag mem tier stats` output. A configurable `mem.token_estimation_model` may substitute a tiktoken-based estimate when the `tiktoken` package is available. |
| FR-12 | **`tag mem tier list` grouping:** The command fetches all memories for the profile grouped by tier, applies `compute_confidence()` per entry, and returns them sorted by confidence descending within each tier. The core tier summary includes the estimated total token count versus `core_token_limit`. |
| FR-13 | **`tag mem tier stats` history:** The `--history N` flag returns the last N rows from `mem_tier_events` where `actor='autopager'`, formatted as a table of event timestamp, count demoted, and estimated tokens freed. |
| FR-14 | **Backward compatibility:** All existing `add_memory()` callers that do not pass `tier` receive `tier='recall'`. All existing `search_memories()` callers that do not pass `tiers` search recall only. `list_memories()` without `tier` filter returns all three tiers sorted by confidence. |
| FR-15 | **Optional LLM extraction pipeline:** `extract_and_store_memories(conn, profile, conversation_turn, llm_callable, fact_type_tier_map)` runs the two-phase FACT_RETRIEVAL → RECONCILE pipeline. The `fact_type_tier_map` dict (e.g., `{'convention': 'core', 'decision': 'recall', 'fact': 'recall', 'gotcha': 'recall', 'other': 'archival'}`) determines the tier for each extracted fact. This function is optional and is never called in the default code path without explicit operator configuration. |
| FR-16 | **`tag mem tier autopage --dry-run`:** Reports which memories would be demoted and how many tokens would be freed without writing any changes to the database. Exits 0. |
| FR-17 | **Core tier hard cap:** If the core tier's total estimated token count exceeds `mem.core_token_limit` at session start (e.g., due to `--force` additions), `build_memory_context()` injects core memories in descending confidence order and truncates at the budget, emitting a warning to stderr: `"Warning: core tier exceeds core_token_limit; N memories omitted."` |
| FR-18 | **`tag mem search` tier filtering:** With `--tier archival`, `search_memories()` searches only tier='archival'. With `--tier all`, it searches all three tiers. The default (no flag) searches core + recall. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|-----|-------------|
| NFR-01 | **Migration safety:** The `ALTER TABLE ... ADD COLUMN` migration must be idempotent and run within an existing SQLite transaction via `open_db()`. Databases with thousands of existing memories must migrate in under 500ms on commodity hardware. |
| NFR-02 | **Autopager latency:** `autopage_recall()` on a store with 10,000 recall memories must complete in ≤ 50ms as measured by the benchmarks in `tests/test_mem_tier_perf.py`. This requires a single sorted SQL query rather than fetching all rows into Python. |
| NFR-03 | **Context build latency:** `build_memory_context()` on a store with 500 core memories and 5,000 recall memories must return within 100ms. Confidence decay is computed in-process; no external API calls. |
| NFR-04 | **Thread safety:** All `semantic_memory.py` functions use the WAL-mode SQLite connection from `open_db()`. Concurrent calls from multiple threads (e.g., parallel agent sessions on the same profile) must not corrupt tier assignments. Write operations use explicit `BEGIN IMMEDIATE` transactions for tier mutations. |
| NFR-05 | **JSON output correctness:** `--json` output from all `tag mem tier` subcommands must be valid JSON parseable by `json.loads()` with no trailing text. Snake_case keys only. |
| NFR-06 | **No external network calls:** The core tiering logic (add, promote, demote, autopager, context build) must never make outbound network calls. The optional LLM extraction pipeline is opt-in and clearly documented as requiring network access. |
| NFR-07 | **Audit log retention:** `mem_tier_events` rows are never automatically deleted. The table is expected to accumulate O(thousands) of rows over a project lifetime and must not impact `tag mem tier list` performance (separate indexed table). |
| NFR-08 | **Token estimation fallback:** When `tiktoken` is not installed, the `len(text) // 4` heuristic is used silently. No error is raised. When `tiktoken` is installed and `mem.token_estimation_model` is set, it is used for both per-memory estimates and the core tier budget check. |
| NFR-09 | **Graceful degradation of LLM pipeline:** If the LLM callable in `extract_and_store_memories()` fails (network error, rate limit, bad response), the function logs a warning via Python's `logging` module and returns without writing any memories. It does not raise. The session continues without memory extraction. |
| NFR-10 | **No deepeval dependency:** The tiering system has zero dependency on `deepeval` or any eval-related package. It depends only on stdlib, the existing `sqlite3` module, and optionally `tiktoken`. |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Status | Description |
|------|--------|-------------|
| `src/tag/semantic_memory.py` | Modified | All tier logic: new `tier` param on `add_memory()`, `build_memory_context()`, `autopage_recall()`, `extract_and_store_memories()`, `promote_memory()`, `demote_memory()`, `tier_stats()`, updated `ensure_schema()` |
| `src/tag/controller.py` | Modified | Wire `autopage_recall()` into session start, add `cmd_mem_tier_list`, `cmd_mem_tier_promote`, `cmd_mem_tier_demote`, `cmd_mem_tier_stats`, `cmd_mem_tier_autopage` handlers; extend `cmd_mem_add` with `--tier`; extend `cmd_mem_list` with tier badge; extend `cmd_mem_search` with `--tier` |
| `tests/test_mem_tier.py` | New | Unit and integration tests for all tier functions |
| `tests/test_mem_tier_perf.py` | New | Performance benchmarks for autopager and context build |

### 9.2 SQLite DDL

#### 9.2.1 Migration: `semantic_memories` table extension

```sql
-- Migration: add tier column to existing table (idempotent)
-- Executed inside ensure_schema() via executescript

ALTER TABLE semantic_memories ADD COLUMN tier TEXT NOT NULL DEFAULT 'recall'
  CHECK(tier IN ('core','recall','archival'));

-- Index for fast tier-scoped queries
CREATE INDEX IF NOT EXISTS idx_sm_tier ON semantic_memories(profile, tier, confidence DESC);

-- Index for autopager: recall entries ordered by confidence ascending
CREATE INDEX IF NOT EXISTS idx_sm_recall_conf_asc ON semantic_memories(profile, tier, confidence ASC)
  WHERE tier = 'recall';
```

Because SQLite does not support `ADD COLUMN IF NOT EXISTS` syntax prior to 3.43.0, the migration wraps the `ALTER TABLE` in a try/except that catches `OperationalError` with message `"duplicate column name: tier"` and silently continues.

#### 9.2.2 `mem_tier_events` table (new)

```sql
CREATE TABLE IF NOT EXISTS mem_tier_events (
  id          TEXT    PRIMARY KEY,           -- uuid4 hex, 16 chars
  memory_id   TEXT    NOT NULL,              -- FK → semantic_memories.id (not enforced by SQLite)
  profile     TEXT    NOT NULL,              -- profile name, for fast per-profile queries
  old_tier    TEXT    NOT NULL               CHECK(old_tier IN ('core','recall','archival')),
  new_tier    TEXT    NOT NULL               CHECK(new_tier IN ('core','recall','archival')),
  actor       TEXT    NOT NULL               CHECK(actor IN ('manual','autopager','llm_pipeline')),
  reason      TEXT,                          -- operator-supplied free text for audits
  event_at    TEXT    NOT NULL               -- ISO-8601 UTC timestamp
);

CREATE INDEX IF NOT EXISTS idx_mte_memory ON mem_tier_events(memory_id);
CREATE INDEX IF NOT EXISTS idx_mte_profile_time ON mem_tier_events(profile, event_at DESC);
CREATE INDEX IF NOT EXISTS idx_mte_actor ON mem_tier_events(profile, actor, event_at DESC);
```

#### 9.2.3 Full `ensure_schema()` additions

```python
def ensure_schema(conn: sqlite3.Connection) -> None:
    # Existing schema (unchanged from PRD-025) ...
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS semantic_memories (
          id           TEXT PRIMARY KEY,
          profile      TEXT NOT NULL,
          content      TEXT NOT NULL,
          memory_type  TEXT NOT NULL DEFAULT 'fact',
          confidence   REAL NOT NULL DEFAULT 1.0,
          created_at   TEXT NOT NULL,
          accessed_at  TEXT NOT NULL,
          access_count INTEGER NOT NULL DEFAULT 0,
          source       TEXT NOT NULL DEFAULT 'manual'
        );
        CREATE INDEX IF NOT EXISTS idx_sm_profile ON semantic_memories(profile, memory_type);
        CREATE INDEX IF NOT EXISTS idx_sm_conf ON semantic_memories(confidence DESC);

        CREATE VIRTUAL TABLE IF NOT EXISTS semantic_memories_fts
          USING fts5(id, profile, content, memory_type, tokenize='porter unicode61');

        CREATE TABLE IF NOT EXISTS mem_tier_events (
          id        TEXT PRIMARY KEY,
          memory_id TEXT NOT NULL,
          profile   TEXT NOT NULL,
          old_tier  TEXT NOT NULL,
          new_tier  TEXT NOT NULL,
          actor     TEXT NOT NULL,
          reason    TEXT,
          event_at  TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_mte_memory ON mem_tier_events(memory_id);
        CREATE INDEX IF NOT EXISTS idx_mte_profile_time ON mem_tier_events(profile, event_at DESC);
    """)
    conn.commit()

    # Idempotent migration: add tier column if not present
    try:
        conn.execute(
            "ALTER TABLE semantic_memories ADD COLUMN tier TEXT NOT NULL DEFAULT 'recall'"
        )
        conn.commit()
    except sqlite3.OperationalError as e:
        if "duplicate column name" not in str(e).lower():
            raise

    # Tier-scoped indexes (safe to re-run: IF NOT EXISTS)
    conn.executescript("""
        CREATE INDEX IF NOT EXISTS idx_sm_tier
          ON semantic_memories(profile, tier, confidence DESC);
        CREATE INDEX IF NOT EXISTS idx_sm_recall_asc
          ON semantic_memories(profile, confidence ASC)
          WHERE tier = 'recall';
    """)
    conn.commit()
```

### 9.3 Core Dataclasses

```python
from __future__ import annotations
from dataclasses import dataclass, field
from typing import Literal

TierName = Literal["core", "recall", "archival"]

VALID_TIERS: set[str] = {"core", "recall", "archival"}

# Tier ordering for validation (lower index = higher in hierarchy)
TIER_ORDER: dict[str, int] = {"core": 0, "recall": 1, "archival": 2}


@dataclass
class MemoryRecord:
    """Full representation of a single memory row including tier."""
    id: str
    profile: str
    content: str
    memory_type: str
    confidence_base: float
    confidence: float          # effective (decayed) confidence
    tier: TierName
    created_at: str
    accessed_at: str
    access_count: int
    source: str
    estimated_tokens: int = field(init=False)

    def __post_init__(self) -> None:
        self.estimated_tokens = estimate_tokens(self.content)


@dataclass
class TierStats:
    """Per-tier aggregate statistics."""
    tier: TierName
    count: int
    estimated_tokens: int
    token_limit: int | None     # None for recall and archival
    avg_confidence: float


@dataclass
class MemoryContext:
    """
    Output of build_memory_context(): what gets injected into an agent system prompt.
    core_memories: always injected; recall_memories: filled up to remaining token budget.
    archival_memories: never populated by this function.
    """
    core_memories: list[MemoryRecord]
    recall_memories: list[MemoryRecord]
    core_tokens_used: int
    recall_tokens_used: int
    core_budget: int
    recall_budget: int
    core_truncated: bool = False    # True if core exceeded budget and was truncated


@dataclass
class AutopageResult:
    """Result of an autopager run."""
    demoted_count: int
    demoted_ids: list[str]
    tokens_freed: int
    dry_run: bool


@dataclass
class TierTransitionEvent:
    """Single event recorded in mem_tier_events."""
    id: str
    memory_id: str
    profile: str
    old_tier: TierName
    new_tier: TierName
    actor: Literal["manual", "autopager", "llm_pipeline"]
    reason: str | None
    event_at: str


class CoreTierBudgetExceededError(ValueError):
    """Raised when adding a memory to core would exceed core_token_limit."""
    def __init__(self, current_tokens: int, new_tokens: int, limit: int) -> None:
        self.current_tokens = current_tokens
        self.new_tokens = new_tokens
        self.limit = limit
        super().__init__(
            f"Core tier budget exceeded: {current_tokens} + {new_tokens} "
            f"> limit {limit}. Use --force or demote a core memory first."
        )
```

### 9.4 Core Algorithms

#### 9.4.1 `estimate_tokens(text: str) -> int`

```python
_TIKTOKEN_ENCODER = None

def _get_tiktoken_encoder(model: str | None):
    global _TIKTOKEN_ENCODER
    if _TIKTOKEN_ENCODER is None:
        try:
            import tiktoken
            _TIKTOKEN_ENCODER = tiktoken.encoding_for_model(model or "gpt-4o")
        except (ImportError, KeyError):
            _TIKTOKEN_ENCODER = False   # sentinel: unavailable
    return _TIKTOKEN_ENCODER if _TIKTOKEN_ENCODER is not False else None


def estimate_tokens(text: str, model: str | None = None) -> int:
    """
    Estimate token count for text. Uses tiktoken when available (accurate),
    falls back to len(text) // 4 heuristic (conservative over-estimate).
    """
    enc = _get_tiktoken_encoder(model)
    if enc is not None:
        return len(enc.encode(text))
    return max(1, len(text) // 4)
```

#### 9.4.2 `build_memory_context()`

```python
def build_memory_context(
    conn: sqlite3.Connection,
    profile: str,
    *,
    core_budget: int = 2000,
    recall_budget: int = 4000,
    recall_limit: int = 20,
    token_estimation_model: str | None = None,
) -> MemoryContext:
    """
    Build the memory payload to inject into an agent system prompt.

    Core memories: fetched all, injected in confidence-desc order until core_budget is exhausted.
    Recall memories: fetched top recall_limit by decayed confidence, injected until recall_budget exhausted.
    Archival memories: never included.
    """
    ensure_schema(conn)

    # --- Core tier ---
    core_rows = conn.execute(
        """SELECT id, profile, content, memory_type, confidence, created_at,
                  accessed_at, access_count, source
           FROM semantic_memories
           WHERE profile = ? AND tier = 'core'
           ORDER BY confidence DESC""",
        (profile,),
    ).fetchall()

    core_memories: list[MemoryRecord] = []
    core_tokens_used = 0
    core_truncated = False

    for row in core_rows:
        mem_id, prof, content, mtype, conf_base, created, accessed, count, src = row
        # Core memories are exempt from decay
        rec = MemoryRecord(
            id=mem_id, profile=prof, content=content, memory_type=mtype,
            confidence_base=conf_base, confidence=conf_base, tier="core",
            created_at=created, accessed_at=accessed, access_count=count, source=src,
        )
        if core_tokens_used + rec.estimated_tokens > core_budget:
            core_truncated = True
            import warnings
            warnings.warn(
                f"Core tier exceeds core_budget ({core_budget} tokens); "
                f"memory {mem_id} omitted.",
                stacklevel=2,
            )
            continue
        core_memories.append(rec)
        core_tokens_used += rec.estimated_tokens

    # --- Recall tier ---
    recall_rows = conn.execute(
        """SELECT id, profile, content, memory_type, confidence, created_at,
                  accessed_at, access_count, source
           FROM semantic_memories
           WHERE profile = ? AND tier = 'recall'
           ORDER BY confidence DESC
           LIMIT ?""",
        (profile, recall_limit * 3),   # over-fetch for re-sort after decay
    ).fetchall()

    recall_candidates: list[MemoryRecord] = []
    for row in recall_rows:
        mem_id, prof, content, mtype, conf_base, created, accessed, count, src = row
        effective = compute_confidence(conf_base, mtype, created)
        recall_candidates.append(MemoryRecord(
            id=mem_id, profile=prof, content=content, memory_type=mtype,
            confidence_base=conf_base, confidence=round(effective, 4), tier="recall",
            created_at=created, accessed_at=accessed, access_count=count, source=src,
        ))

    recall_candidates.sort(key=lambda r: -r.confidence)

    recall_memories: list[MemoryRecord] = []
    recall_tokens_used = 0
    for rec in recall_candidates:
        if len(recall_memories) >= recall_limit:
            break
        if recall_tokens_used + rec.estimated_tokens > recall_budget:
            break
        recall_memories.append(rec)
        recall_tokens_used += rec.estimated_tokens

    return MemoryContext(
        core_memories=core_memories,
        recall_memories=recall_memories,
        core_tokens_used=core_tokens_used,
        recall_tokens_used=recall_tokens_used,
        core_budget=core_budget,
        recall_budget=recall_budget,
        core_truncated=core_truncated,
    )
```

#### 9.4.3 `autopage_recall()`

```python
def autopage_recall(
    conn: sqlite3.Connection,
    profile: str,
    *,
    soft_limit: int = 200,
    dry_run: bool = False,
) -> AutopageResult:
    """
    If recall tier count > soft_limit, demote the lowest-confidence recall memories
    to archival until count == soft_limit. Returns summary of what was (or would be) demoted.
    """
    ensure_schema(conn)

    count_row = conn.execute(
        "SELECT COUNT(*) FROM semantic_memories WHERE profile=? AND tier='recall'",
        (profile,),
    ).fetchone()
    total_recall = count_row[0]

    if total_recall <= soft_limit:
        return AutopageResult(demoted_count=0, demoted_ids=[], tokens_freed=0, dry_run=dry_run)

    # How many to demote
    to_demote = total_recall - soft_limit

    # Fetch the lowest-confidence recall memories (using decayed confidence proxy: confidence ASC)
    # We use stored confidence ASC as a fast proxy; exact decayed confidence is computed in-process
    candidates = conn.execute(
        """SELECT id, content, memory_type, confidence, created_at
           FROM semantic_memories
           WHERE profile=? AND tier='recall'
           ORDER BY confidence ASC
           LIMIT ?""",
        (profile, to_demote * 2),   # over-fetch for re-sort
    ).fetchall()

    # Re-sort by actual decayed confidence ascending
    scored: list[tuple[float, str, str]] = []
    for mem_id, content, mtype, conf_base, created in candidates:
        effective = compute_confidence(conf_base, mtype, created)
        scored.append((effective, mem_id, content))
    scored.sort(key=lambda x: x[0])  # ascending = least relevant first

    to_demote_rows = scored[:to_demote]
    demoted_ids = [row[1] for row in to_demote_rows]
    tokens_freed = sum(estimate_tokens(row[2]) for row in to_demote_rows)

    if dry_run:
        return AutopageResult(
            demoted_count=len(demoted_ids),
            demoted_ids=demoted_ids,
            tokens_freed=tokens_freed,
            dry_run=True,
        )

    now = _utc_now()
    event_id_base = uuid.uuid4().hex[:12]

    for i, (_, mem_id, _) in enumerate(to_demote_rows):
        conn.execute(
            "UPDATE semantic_memories SET tier='archival' WHERE id=? AND profile=?",
            (mem_id, profile),
        )
        conn.execute(
            """INSERT INTO mem_tier_events(id, memory_id, profile, old_tier, new_tier,
               actor, reason, event_at) VALUES(?,?,?,?,?,?,?,?)""",
            (
                f"{event_id_base}{i:04d}",
                mem_id, profile, "recall", "archival",
                "autopager", f"auto-demoted: count={total_recall} exceeded soft_limit={soft_limit}",
                now,
            ),
        )

    conn.commit()

    return AutopageResult(
        demoted_count=len(demoted_ids),
        demoted_ids=demoted_ids,
        tokens_freed=tokens_freed,
        dry_run=False,
    )
```

#### 9.4.4 `promote_memory()` and `demote_memory()`

```python
def promote_memory(
    conn: sqlite3.Connection,
    mem_id: str,
    profile: str,
    *,
    to_tier: TierName,
    actor: str = "manual",
    reason: str | None = None,
    force: bool = False,
    core_token_limit: int = 2000,
) -> TierTransitionEvent:
    """
    Promote a memory to a higher tier. Valid transitions: archival→recall, recall→core, archival→core.
    Raises ValueError for invalid transitions (e.g., recall→archival — use demote_memory instead).
    """
    ensure_schema(conn)

    row = conn.execute(
        "SELECT tier, content FROM semantic_memories WHERE id=? AND profile=?",
        (mem_id, profile),
    ).fetchone()
    if not row:
        raise KeyError(f"Memory {mem_id!r} not found in profile {profile!r}")

    current_tier, content = row

    if TIER_ORDER[to_tier] >= TIER_ORDER[current_tier]:
        raise ValueError(
            f"Cannot promote memory {mem_id!r} from {current_tier!r} to {to_tier!r}: "
            f"destination must be higher in hierarchy (core < recall < archival). "
            f"Use demote_memory() for downward transitions."
        )

    if to_tier == "core" and not force:
        # Check budget
        current_core_tokens = conn.execute(
            "SELECT COALESCE(SUM(LENGTH(content) / 4), 0) "
            "FROM semantic_memories WHERE profile=? AND tier='core'",
            (profile,),
        ).fetchone()[0]
        new_tokens = estimate_tokens(content)
        if current_core_tokens + new_tokens > core_token_limit:
            raise CoreTierBudgetExceededError(current_core_tokens, new_tokens, core_token_limit)

    conn.execute(
        "UPDATE semantic_memories SET tier=? WHERE id=? AND profile=?",
        (to_tier, mem_id, profile),
    )

    event_id = uuid.uuid4().hex[:16]
    now = _utc_now()
    conn.execute(
        """INSERT INTO mem_tier_events(id, memory_id, profile, old_tier, new_tier,
           actor, reason, event_at) VALUES(?,?,?,?,?,?,?,?)""",
        (event_id, mem_id, profile, current_tier, to_tier, actor, reason, now),
    )
    conn.commit()

    return TierTransitionEvent(
        id=event_id, memory_id=mem_id, profile=profile,
        old_tier=current_tier, new_tier=to_tier,  # type: ignore[arg-type]
        actor=actor, reason=reason, event_at=now,  # type: ignore[arg-type]
    )


def demote_memory(
    conn: sqlite3.Connection,
    mem_id: str,
    profile: str,
    *,
    to_tier: TierName,
    actor: str = "manual",
    reason: str | None = None,
) -> TierTransitionEvent:
    """
    Demote a memory to a lower tier. Valid transitions: core→recall, recall→archival, core→archival.
    """
    ensure_schema(conn)

    row = conn.execute(
        "SELECT tier FROM semantic_memories WHERE id=? AND profile=?",
        (mem_id, profile),
    ).fetchone()
    if not row:
        raise KeyError(f"Memory {mem_id!r} not found in profile {profile!r}")

    current_tier = row[0]

    if TIER_ORDER[to_tier] <= TIER_ORDER[current_tier]:
        raise ValueError(
            f"Cannot demote memory {mem_id!r} from {current_tier!r} to {to_tier!r}: "
            f"destination must be lower in hierarchy. Use promote_memory() for upward transitions."
        )

    conn.execute(
        "UPDATE semantic_memories SET tier=? WHERE id=? AND profile=?",
        (to_tier, mem_id, profile),
    )

    event_id = uuid.uuid4().hex[:16]
    now = _utc_now()
    conn.execute(
        """INSERT INTO mem_tier_events(id, memory_id, profile, old_tier, new_tier,
           actor, reason, event_at) VALUES(?,?,?,?,?,?,?,?)""",
        (event_id, mem_id, profile, current_tier, to_tier, actor, reason, now),
    )
    conn.commit()

    return TierTransitionEvent(
        id=event_id, memory_id=mem_id, profile=profile,
        old_tier=current_tier, new_tier=to_tier,  # type: ignore[arg-type]
        actor=actor, reason=reason, event_at=now,  # type: ignore[arg-type]
    )
```

### 9.5 Optional LLM Extraction Pipeline

The two-phase pipeline is inspired by mem0's FACT_RETRIEVAL_PROMPT / UPDATE_MEMORY_PROMPT architecture. It is entirely optional and is never called in the default code path.

```python
FACT_RETRIEVAL_PROMPT = """\
You are a Personal Information Organizer for an AI coding assistant.
Extract discrete, self-contained facts from the following conversation turn.
Focus on: technical decisions, coding conventions, project constraints, bugs/gotchas found, and plans.
Ignore: generic conversation, greetings, affirmations without content.
Return a JSON array of objects with fields:
  - "text": the fact as a short, complete sentence
  - "type": one of "fact", "decision", "convention", "gotcha", "other"
Return [] if no extractable facts are present.

Conversation turn:
{turn}
"""

RECONCILE_PROMPT = """\
You are a memory reconciliation engine for an AI coding assistant.
Given a list of newly extracted facts and the top existing memories most similar to them,
classify each new fact with one of: ADD, UPDATE, DELETE, NOOP.
- ADD: fact is genuinely new, not present in existing memories
- UPDATE: fact supersedes or refines an existing memory (provide "existing_id")
- DELETE: fact contradicts an existing memory, which should be removed (provide "existing_id")
- NOOP: fact is already captured accurately in existing memories
Return a JSON array: [{{"text": str, "type": str, "event": "ADD"|"UPDATE"|"DELETE"|"NOOP",
  "existing_id": str|null}}]

Existing memories:
{existing}

New facts:
{facts}
"""

FACT_TYPE_TIER_MAP: dict[str, str] = {
    "convention": "core",
    "decision": "recall",
    "fact": "recall",
    "gotcha": "recall",
    "other": "archival",
}


def extract_and_store_memories(
    conn: sqlite3.Connection,
    profile: str,
    conversation_turn: str,
    llm_callable,   # Callable[[str], str]: accepts prompt, returns completion text
    *,
    fact_type_tier_map: dict[str, str] | None = None,
    core_token_limit: int = 2000,
) -> list[MemoryRecord]:
    """
    Two-phase LLM memory extraction pipeline.
    Phase 1: Extract facts from conversation_turn.
    Phase 2: Reconcile against existing recall memories; apply ADD/UPDATE/DELETE/NOOP.
    Returns list of newly added/updated MemoryRecord objects.
    """
    import json
    import logging

    logger = logging.getLogger(__name__)
    tier_map = fact_type_tier_map or FACT_TYPE_TIER_MAP

    # Phase 1: fact extraction
    try:
        extraction_prompt = FACT_RETRIEVAL_PROMPT.format(turn=conversation_turn)
        raw = llm_callable(extraction_prompt)
        facts: list[dict] = json.loads(raw)
        if not isinstance(facts, list):
            raise ValueError("Extraction result is not a list")
    except Exception as exc:
        logger.warning("Memory extraction failed (phase 1): %s", exc)
        return []

    if not facts:
        return []

    # Phase 2: reconcile against existing memories
    # Retrieve top-10 recall+core memories via FTS for each extracted fact
    existing_memories: list[dict] = []
    for fact in facts:
        candidates = search_memories(conn, profile, fact["text"], limit=5, tiers=["core", "recall"])
        for m in candidates:
            if not any(e["id"] == m["id"] for e in existing_memories):
                existing_memories.append(m)

    try:
        existing_text = json.dumps(
            [{"id": m["id"], "text": m["content"], "type": m["memory_type"]} for m in existing_memories],
            indent=2,
        )
        reconcile_prompt = RECONCILE_PROMPT.format(
            existing=existing_text,
            facts=json.dumps(facts, indent=2),
        )
        raw2 = llm_callable(reconcile_prompt)
        operations: list[dict] = json.loads(raw2)
        if not isinstance(operations, list):
            raise ValueError("Reconcile result is not a list")
    except Exception as exc:
        logger.warning("Memory extraction failed (phase 2): %s", exc)
        return []

    # Apply operations
    added: list[MemoryRecord] = []
    for op in operations:
        event = op.get("event", "NOOP")
        fact_type = op.get("type", "fact")
        target_tier: str = tier_map.get(fact_type, "recall")

        if event == "ADD":
            try:
                mem_id = add_memory(
                    conn, profile, op["text"],
                    memory_type=fact_type,
                    tier=target_tier,
                    source="llm_pipeline",
                    core_token_limit=core_token_limit,
                )
                row = conn.execute(
                    "SELECT id, profile, content, memory_type, confidence, tier, "
                    "created_at, accessed_at, access_count, source "
                    "FROM semantic_memories WHERE id=?", (mem_id,)
                ).fetchone()
                if row:
                    mem_id2, prof, content, mtype, conf, tier, created, accessed, count, src = row
                    added.append(MemoryRecord(
                        id=mem_id2, profile=prof, content=content, memory_type=mtype,
                        confidence_base=conf, confidence=conf, tier=tier,
                        created_at=created, accessed_at=accessed, access_count=count, source=src,
                    ))
            except CoreTierBudgetExceededError:
                logger.warning("LLM pipeline: core budget exceeded for fact %r; adding as recall", op["text"])
                add_memory(conn, profile, op["text"], memory_type=fact_type,
                           tier="recall", source="llm_pipeline")

        elif event == "UPDATE" and op.get("existing_id"):
            conn.execute(
                "UPDATE semantic_memories SET content=?, memory_type=?, confidence=1.0, "
                "accessed_at=? WHERE id=? AND profile=?",
                (op["text"], fact_type, _utc_now(), op["existing_id"], profile),
            )
            conn.execute(
                "UPDATE semantic_memories_fts SET content=?, memory_type=? WHERE id=?",
                (op["text"], fact_type, op["existing_id"]),
            )
            conn.commit()

        elif event == "DELETE" and op.get("existing_id"):
            forget_memory(conn, op["existing_id"], profile)

        # NOOP: do nothing

    return added
```

### 9.6 `add_memory()` signature extension

```python
def add_memory(
    conn: sqlite3.Connection,
    profile: str,
    content: str,
    *,
    memory_type: str = "fact",
    confidence: float = 1.0,
    source: str = "manual",
    tier: str = "recall",              # NEW
    force: bool = False,               # NEW: bypass core budget check
    core_token_limit: int = 2000,      # NEW: for budget check when tier='core'
) -> str:
    """Insert a new memory. Returns the new memory id."""
    ensure_schema(conn)
    content = content.strip()
    if not content:
        raise ValueError("Memory content must not be empty")
    if memory_type not in VALID_TYPES:
        raise ValueError(f"memory_type must be one of {sorted(VALID_TYPES)}, got {memory_type!r}")
    if tier not in VALID_TIERS:
        raise ValueError(f"tier must be one of {sorted(VALID_TIERS)}, got {tier!r}")
    if not (0.0 < confidence <= 1.0):
        raise ValueError(f"confidence must be in (0, 1], got {confidence}")

    if tier == "core" and not force:
        current_tokens = conn.execute(
            "SELECT COALESCE(SUM(LENGTH(content) / 4), 0) "
            "FROM semantic_memories WHERE profile=? AND tier='core'",
            (profile,),
        ).fetchone()[0]
        new_tokens = estimate_tokens(content)
        if current_tokens + new_tokens > core_token_limit:
            raise CoreTierBudgetExceededError(current_tokens, new_tokens, core_token_limit)

    mem_id = uuid.uuid4().hex[:16]
    now = _utc_now()
    conn.execute(
        """INSERT INTO semantic_memories(id, profile, content, memory_type, confidence,
           created_at, accessed_at, access_count, source, tier)
           VALUES(?,?,?,?,?,?,?,0,?,?)""",
        (mem_id, profile, content, memory_type, confidence, now, now, source, tier),
    )
    conn.execute(
        "INSERT INTO semantic_memories_fts(id, profile, content, memory_type) VALUES(?,?,?,?)",
        (mem_id, profile, content, memory_type),
    )
    conn.commit()
    return mem_id
```

### 9.7 `controller.py` integration points

The following connection points in `controller.py` require modification:

1. **Session start**: After `open_db(cfg)`, call `autopage_recall(conn, profile, soft_limit=cfg.get('mem', {}).get('recall_soft_limit', 200))`. Errors are caught and logged; they do not abort the session.

2. **`cmd_mem_add`**: Parse `--tier` flag (default `'recall'`). Pass to `add_memory()`. On `CoreTierBudgetExceededError`, print the error message and exit 1 unless `--force` was passed.

3. **`cmd_mem_list`**: Fetch `tier` column and prepend `[C]`/`[R]`/`[A]` badge. Accept `--tier` filter.

4. **`cmd_mem_search`**: Accept `--tier` flag; map to `tiers` list for `search_memories()`.

5. **Context injection** (`cmd_chat`, `cmd_run`, `loop_agent.py`): Replace the existing `search_memories()` call with `build_memory_context()`. The `core_memories` and `recall_memories` lists are serialized into the system prompt in separate labeled sections:
   ```
   ## Always-Available Context (Core Memory)
   - <content of core memory 1>
   - <content of core memory 2>

   ## Relevant Context (Recall Memory)
   - <content of recall memory 1>
   - <content of recall memory 2>
   ```

---

## 10. Security Considerations

1. **Tier escalation via LLM pipeline:** The optional two-phase extraction pipeline accepts LLM-generated JSON that specifies tier assignments indirectly (via `fact_type_tier_map`). A compromised or adversarial LLM response could attempt to classify a spurious fact as `"convention"` type to force it into `core` tier (permanent context injection). Mitigation: the `fact_type_tier_map` is operator-controlled, not LLM-controlled. The LLM specifies only `fact_type`; the tier derivation is a local lookup. Additionally, content added by the LLM pipeline is marked `source='llm_pipeline'` and can be audited or bulk-removed: `DELETE FROM semantic_memories WHERE source='llm_pipeline' AND tier='core'`.

2. **Core tier as persistent injection surface:** A memory in `core` tier is injected verbatim into every agent system prompt for the life of the profile. An attacker who can write a memory (e.g., via a tool that calls `add_memory()` with `tier='core'`) achieves persistent system-prompt injection. Mitigation: (a) `add_memory()` with `tier='core'` must never be callable from within an agent tool unless the tool has explicit operator authorization; (b) PRD-034 (secret scanning) should extend its patterns to scan new core memory content for prompt-injection markers (e.g., `"Ignore previous instructions"`); (c) `mem_tier_events` provides an audit trail of all tier changes.

3. **Token budget exhaustion via core tier:** An operator who adds many large core memories could exhaust the context window entirely, preventing normal agent operation. The `core_token_limit` check (FR-02) enforces a hard cap. The `--force` flag bypasses this and is documented as dangerous. When `core_truncated=True` is set in `MemoryContext`, the warning is emitted to stderr on every session start until the issue is resolved.

4. **SQLite transaction integrity for tier mutations:** Promote and demote operations write both the `semantic_memories` row update and the `mem_tier_events` row in a single commit. If the process is killed between these writes, the event log may be incomplete but the memory tier will be correct (the memory update is written first). A future migration could use a SQLite trigger to enforce atomicity at the DB level.

5. **Auto-pager permanent data movement:** The autopager demotes memories to archival irreversibly within a single call (no dry-run by default in the session-start path). A bug in `autopage_recall()` could accidentally demote all recall memories. Mitigation: (a) `dry_run=True` is always used when `mem.autopager_dry_run = true` is configured; (b) demoted memories remain in the database with `tier='archival'` — they are never deleted; (c) the `mem_tier_events` log allows identifying and bulk-reversing an erroneous autopager run via `UPDATE semantic_memories SET tier='recall' WHERE id IN (SELECT memory_id FROM mem_tier_events WHERE actor='autopager' AND event_at > '<timestamp>')`.

6. **Audit log tampering:** `mem_tier_events` uses a separate table that operators can query but that is not exposed via `tag mem` commands for modification. Direct SQLite access could alter the audit log. For environments requiring tamper-evident logs, the audit events should additionally be written to TAG's OpenTelemetry span log (PRD-013) or syslog.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_mem_tier.py`)

- **`ensure_schema()` migration idempotency:** Call `ensure_schema()` twice on the same connection; assert no error and `tier` column exists with default `'recall'`.
- **`add_memory()` tier parameter:** Add memories with each of the three tiers; assert `SELECT tier FROM semantic_memories WHERE id=?` returns the correct value.
- **Core budget enforcement:** Seed core tier with `core_token_limit - 10` tokens worth of content. Attempt to add a memory whose content would exceed the limit; assert `CoreTierBudgetExceededError` is raised. Repeat with `force=True`; assert success.
- **Decay exemption for core:** Add a `convention` memory with `tier='core'` and manually set `created_at` to 365 days ago. Call `compute_confidence()` and assert it returns `confidence_base` unchanged (= 1.0). Compare with an identical memory with `tier='recall'` which should decay.
- **`promote_memory()` valid transitions:** archival→recall, recall→core, archival→core all succeed. Assert `mem_tier_events` row is written.
- **`promote_memory()` invalid transitions:** recall→archival raises `ValueError`. core→recall raises `ValueError`. Assert `mem_tier_events` has zero rows after failed calls.
- **`demote_memory()` valid transitions:** core→recall, recall→archival, core→archival all succeed. Assert audit row written.
- **`autopage_recall()` below threshold:** Seed 150 recall memories with `soft_limit=200`. Assert `AutopageResult.demoted_count == 0`.
- **`autopage_recall()` above threshold:** Seed 210 recall memories of varying confidence. Assert exactly 10 are demoted (210 - 200), and the 10 with lowest decayed confidence are chosen.
- **`autopage_recall()` dry_run:** Assert zero DB writes; assert `AutopageResult.dry_run == True` and `demoted_count == 10`.
- **`build_memory_context()` core always injected:** Seed 3 core + 20 recall memories. Assert `MemoryContext.core_memories` length == 3 regardless of query.
- **`build_memory_context()` recall budget respected:** Set `recall_budget=100` (very small). Assert `recall_tokens_used <= 100`.
- **`build_memory_context()` archival excluded:** Add archival memories with confidence 1.0. Assert they never appear in `core_memories` or `recall_memories`.
- **`search_memories()` tier filtering:** Add one memory per tier. Call with `tiers=['core']`; assert only core memory returned. Call with `tiers=['recall', 'archival']`; assert recall and archival returned but not core.
- **`tier_stats()`:** Seed 3 core, 10 recall, 5 archival. Assert returned `TierStats` objects have correct counts.

### 11.2 Integration Tests (`tests/test_mem_tier_integration.py`)

- **Full session workflow:** Simulate a session: add 3 core + 250 recall memories → assert autopager fires at session start and demotes 50 → assert `build_memory_context()` returns exactly 3 core + ≤ 20 recall.
- **Promote/demote round-trip:** Add a recall memory → promote to core → demote back to recall → assert final tier = `'recall'` and `mem_tier_events` has 2 rows for this memory ID.
- **`--json` CLI output:** Run `tag mem tier list --json` via subprocess; assert output is valid JSON with `tiers.core`, `tiers.recall`, `tiers.archival` keys.
- **Backward compatibility:** Create a `semantic_memories` table without the `tier` column (simulate pre-migration state). Call `ensure_schema()`. Assert all existing rows now have `tier = 'recall'`.

### 11.3 Performance Benchmarks (`tests/test_mem_tier_perf.py`)

```python
import time
import pytest

def test_autopager_10k_memories(tmp_db):
    """autopage_recall() on 10,000 recall entries must complete in <= 50ms."""
    conn = tmp_db
    for i in range(10_000):
        add_memory(conn, "perf_profile", f"Memory content number {i}", tier="recall",
                   confidence=0.5 + (i % 100) / 200)
    start = time.perf_counter()
    result = autopage_recall(conn, "perf_profile", soft_limit=200)
    elapsed_ms = (time.perf_counter() - start) * 1000
    assert elapsed_ms <= 50, f"Autopager took {elapsed_ms:.1f}ms, expected <= 50ms"
    assert result.demoted_count == 9800

def test_build_memory_context_500_core_5000_recall(tmp_db):
    """build_memory_context() on 500 core + 5000 recall must complete in <= 100ms."""
    conn = tmp_db
    for i in range(500):
        add_memory(conn, "perf_profile", f"Core fact {i}", tier="core", force=True)
    for i in range(5000):
        add_memory(conn, "perf_profile", f"Recall fact {i}", tier="recall")
    start = time.perf_counter()
    ctx = build_memory_context(conn, "perf_profile", core_budget=999999, recall_budget=999999)
    elapsed_ms = (time.perf_counter() - start) * 1000
    assert elapsed_ms <= 100, f"Context build took {elapsed_ms:.1f}ms, expected <= 100ms"
    assert len(ctx.core_memories) == 500
```

### 11.4 LLM Pipeline Tests (`tests/test_mem_tier_llm_pipeline.py`)

These tests use a deterministic mock `llm_callable` that returns pre-canned JSON:

- **FACT_RETRIEVAL phase:** Assert the prompt contains the conversation turn verbatim. Assert empty array return leads to zero stored memories.
- **RECONCILE ADD path:** Mock returns `[{"text": "X", "type": "convention", "event": "ADD", "existing_id": null}]`. Assert memory is added with `tier='core'` (per `fact_type_tier_map`).
- **RECONCILE UPDATE path:** Mock returns UPDATE with an existing ID. Assert the row is updated in-place.
- **RECONCILE DELETE path:** Assert `forget_memory()` is called for the existing ID.
- **RECONCILE NOOP path:** Assert no writes to the database.
- **LLM failure graceful degradation:** Mock raises `RuntimeError`. Assert function returns `[]` without raising and no DB writes occur.

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|-----------|
| AC-01 | `tag mem add "The project uses PostgreSQL 16" --tier core` succeeds and `tag mem tier list --json` shows the memory in `tiers.core.memories` with `tier="core"`. |
| AC-02 | `tag mem add "long content..."` with content that would push core token count over `core_token_limit` exits 1 with an error message containing `"core_token_limit"`. Passing `--force` succeeds. |
| AC-03 | `tag mem tier promote <recall-id> --to core` updates the memory's tier to `core` and writes one row to `mem_tier_events` with `old_tier='recall'`, `new_tier='core'`, `actor='manual'`. |
| AC-04 | `tag mem tier demote <core-id> --to archival --reason "Deprecated"` updates the memory's tier to `archival`, writes one row to `mem_tier_events` with `reason='Deprecated'`. |
| AC-05 | `tag mem tier promote <recall-id> --to archival` exits 1 with an error message indicating that downward transitions require `demote`. |
| AC-06 | After seeding 210 recall memories, running `tag mem tier autopage` (or triggering session start) demotes exactly 10 memories to `archival`. `tag mem tier list --tier recall | wc -l` outputs 200. |
| AC-07 | `tag mem tier autopage --dry-run` prints the count and IDs of memories that would be demoted without writing any changes to the database. `SELECT COUNT(*) FROM mem_tier_events` returns 0 after the dry run. |
| AC-08 | A memory added with `tier='core'` has exactly `confidence_base` as its effective confidence regardless of how old `created_at` is (no decay). A memory with the same content at `tier='recall'` decays normally. |
| AC-09 | `build_memory_context()` never includes any `tier='archival'` memory in its output, even when archival memories have higher stored `confidence` than recall memories. |
| AC-10 | `tag mem list` displays `[C]`, `[R]`, or `[A]` badges before each memory ID. `tag mem list --tier core` returns only core memories. |
| AC-11 | `tag mem search "PostgreSQL" --tier archival` returns archival memories matching the query. `tag mem search "PostgreSQL"` (no flag) does not return archival memories. |
| AC-12 | All existing tests in `tests/test_semantic_memory.py` pass without modification after the tier migration is applied. |
| AC-13 | `tag mem tier stats` shows per-tier entry counts, estimated token footprints, the core tier's `core_token_limit`, and the last 5 auto-paging events from `mem_tier_events`. |
| AC-14 | A `semantic_memories` table created by PRD-025 (without `tier` column) is successfully migrated by `ensure_schema()`. All pre-migration rows have `tier='recall'` in the result. |
| AC-15 | `--json` output from `tag mem tier list`, `tag mem tier promote`, `tag mem tier demote`, and `tag mem tier stats` is valid JSON parseable by `json.loads()`. |

---

## 13. Dependencies

| Dependency | Type | Blocking? | Notes |
|------------|------|-----------|-------|
| PRD-025: Semantic Memory with Confidence Decay | Internal prerequisite | Yes — this PRD extends PRD-025's schema and functions | `semantic_memory.py` must be at the version implementing PRD-025 before this work starts. The `tier` column is added via migration. |
| PRD-039: Token Budget Enforcement | Internal — complementary | No | `build_memory_context()` is designed to integrate with PRD-039's token budget via the `core_budget` and `recall_budget` parameters. PRD-039 provides the per-session budget figure that feeds these parameters. |
| PRD-013: Agent Tracing / Observability | Internal — complementary | No | Autopager events and tier transitions can optionally be emitted as OTEL spans for observability. Not blocking for Phase 1. |
| PRD-034: Secret Scanning | Internal — complementary | No | Should be extended to scan core memory content for prompt-injection markers. Not blocking for Phase 1. |
| PRD-043: Vector-Based Tool Retrieval | Internal — complementary | No | The `sentence-transformers` embedding path from PRD-043 can enhance recall tier retrieval quality (semantic similarity scoring beyond FTS5). Not required for Phase 1; pluggable via `search_memories()` extension. |
| `tiktoken` | Optional Python package | No | Used for accurate token estimation when `mem.token_estimation_model` is configured. Graceful fallback to `len(text) // 4` when absent. Install: `pip install tiktoken`. |
| SQLite ≥ 3.35.0 | Runtime | Yes | Required for `ALTER TABLE ... ADD COLUMN` with `CHECK` constraint. Bundled with Python ≥ 3.12 and macOS 12+. |

---

## 14. Open Questions

| ID | Question | Impact | Owner | Status |
|----|----------|--------|-------|--------|
| OQ-01 | **Default `recall_soft_limit` value:** Should the default be 200 entries or a token-based limit (e.g., 50,000 tokens)? A token-based limit is more directly tied to model context budgets but requires token estimation on every autopager run, increasing latency. A count-based limit is O(1) to check. | Autopager correctness for diverse content sizes | Product | Open — recommend count-based (200) for Phase 1, add token-based option in Phase 2 |
| OQ-02 | **Core tier and multi-profile context:** When an agent runs under profile A but inherits from profile B, should core memories from both profiles be injected? Current design is profile-scoped only. Cross-profile core injection requires a profile hierarchy query. | Multi-profile setups | Engineering | Open — out of scope for this PRD; defer to a future profile-inheritance PRD |
| OQ-03 | **Auto-promotion heuristic:** Should the autopager optionally auto-promote high-access-count recall memories to core (e.g., if `access_count > 50` and `confidence > 0.9`)? This inverts the paging direction and provides automatic escalation. Risk: pollutes core tier without operator intent. | UX convenience vs. predictability | Product | Open — recommend against in Phase 1; revisit after user feedback |
| OQ-04 | **Archival search by default in `tag mem search`:** Current proposal excludes archival from default search. Some users may expect a global search to include archival. Should `--tier` default to `all` instead of `core+recall`? | UX discoverability | Product | Open — recommend `core+recall` default to match retrieval semantics; users who want archival search know to pass `--tier archival` |
| OQ-05 | **LLM pipeline invocation trigger:** How should `extract_and_store_memories()` be triggered in production? Options: (a) after every agent turn; (b) at session end as a batch; (c) on `tag mem extract <session-id>` explicitly. Option (a) adds per-turn LLM cost; option (b) risks data loss if session is interrupted; option (c) requires explicit operator action. | LLM cost, completeness | Product | Open — recommend option (c) for Phase 1 as an explicit command; auto-trigger in Phase 2 as a configurable hook |
| OQ-06 | **Semantic similarity for recall retrieval:** The current `search_memories()` uses FTS5 (keyword) search only. PRD-043's `SentenceTransformer` embedding path could replace or augment this with ANN vector search (RRF fusion). Should PRD-067 require PRD-043 for recall retrieval, or remain FTS5-only? | Retrieval quality | Engineering | Open — recommend FTS5-only for Phase 1 (no new dependencies); add optional ANN path in Phase 2 via PRD-043 integration |
| OQ-07 | **`mem_tier_events` retention policy:** As the event log grows indefinitely, should there be an automatic pruning policy (e.g., keep only the last 10,000 events per profile)? Or leave pruning to the operator? | Storage growth | Engineering | Open — recommend no auto-pruning in Phase 1; add `tag mem tier events prune --older-than 90d` in a follow-up |

---

## 15. Complexity and Timeline

**Estimated Effort:** L (2-4 weeks, approximately 14-18 engineering days)

| Phase | Tasks | Days |
|-------|-------|------|
| **Phase 1: Schema and core library** (Days 1–4) | `ensure_schema()` migration with `tier` column and indexes; `mem_tier_events` DDL; dataclasses (`MemoryRecord`, `MemoryContext`, `AutopageResult`, `TierTransitionEvent`, `CoreTierBudgetExceededError`); `estimate_tokens()` with tiktoken fallback; extend `add_memory()` with `tier` + budget check; extend `compute_confidence()` with core-decay exemption; extend `search_memories()` with `tiers` param; unit tests for all of the above | 4 |
| **Phase 2: Context builder and autopager** (Days 5–8) | `build_memory_context()` with core-first injection and recall budget fill; `autopage_recall()` with dry-run support; `promote_memory()` and `demote_memory()` with audit log; `tier_stats()` aggregate query; integration with `controller.py` session start (autopager hook) and context injection path | 4 |
| **Phase 3: CLI surface** (Days 9–12) | `tag mem add --tier` extension; `tag mem tier list` (grouped display + `--json`); `tag mem tier promote` and `tag mem tier demote` (with `--reason`); `tag mem tier stats` (with `--history`); `tag mem tier autopage` (with `--dry-run`); extend `tag mem list` with tier badges and `--tier` filter; extend `tag mem search` with `--tier` filter; plain + Rich TTY formatting | 4 |
| **Phase 4: Optional LLM pipeline and polish** (Days 13–16) | `extract_and_store_memories()` two-phase pipeline; `FACT_RETRIEVAL_PROMPT` and `RECONCILE_PROMPT` prompts; `FACT_TYPE_TIER_MAP` default config; `tag mem extract <session-id>` CLI command; LLM pipeline unit tests with mock callable; performance benchmarks (`test_mem_tier_perf.py`); backward compatibility regression tests against PRD-025 suite | 4 |
| **Phase 5: Hardening and documentation** (Days 17–18) | Security: PRD-034 core content scanning extension; audit log integration with OTel (PRD-013); edge-case handling (empty profile, zero-budget core, concurrent writes); end-to-end acceptance criterion verification; update `docs/prd/INDEX.md` | 2 |

**Risks:**

- **SQLite `ALTER TABLE` on large existing databases:** A production database with 100k+ memories may take several seconds for the column addition. Mitigation: run the migration with a progress spinner in `open_db()` if the row count exceeds a threshold (e.g., `PRAGMA page_count > 10000`).
- **tiktoken version compatibility:** `tiktoken` encoding names change between model generations. The `estimate_tokens()` fallback to `len(text) // 4` ensures the feature works without tiktoken; the heuristic over-estimates by approximately 15-25% for typical English prose.
- **Autopager competing with concurrent session starts:** If two `tag` sessions for the same profile start simultaneously, both may trigger `autopage_recall()` and attempt to demote the same set of memories. The `UPDATE ... WHERE tier='recall'` is idempotent (demoting an already-archival memory to archival is a no-op at the DB level), and `mem_tier_events` may record duplicate events. Mitigation: the autopager uses `BEGIN IMMEDIATE` to serialize concurrent access via SQLite WAL mode; duplicate events are benign.
- **LLM pipeline accuracy:** The quality of the two-phase extraction depends heavily on the LLM used. Poor extraction (false facts, missed facts) can pollute core tier permanently. Mitigation: the `source='llm_pipeline'` marker on all LLM-extracted memories enables bulk audit and rollback: `SELECT * FROM semantic_memories WHERE source='llm_pipeline' AND tier='core'`.

