# PRD-068: Background Sleep-Time Memory Consolidation Agent (`tag memory gc`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Memory & Knowledge
**Affects:** `memory_gc.py + cron`
**Depends on:** PRD-025 (semantic memory / confidence decay), PRD-022 (cron scheduled agents), PRD-013 (agent tracing / observability), PRD-028 (sandbox code execution), PRD-034 (secret scanning / security), PRD-033 (dependency-aware task queue), PRD-027 (eval framework), PRD-039 (token budget enforcement), PRD-012 (cost tracking / budget)
**Inspired by:** Letta sleep-time agents, MemGPT memory management, Zep graph consolidation

---

## 1. Overview

TAG's semantic memory system (PRD-025) accumulates facts, decisions, conventions, and gotchas across agent sessions. Each memory entry is scored with a time-decaying confidence value and stored in SQLite with FTS5 full-text indexing. Over weeks of active use a single profile can accumulate hundreds of entries — many of which are redundant near-duplicates, superseded by newer entries, or decayed below any practical retrieval threshold. The memory store becomes a long-tail distribution: a dense cluster of high-confidence, recently-accessed entries surrounded by a silent majority of stale noise that inflates search result sets without contributing signal.

This PRD introduces `tag memory gc` — a **Background Sleep-Time Memory Consolidation Agent** — that runs as a scheduled cron job during low-activity periods and systematically improves memory quality through four operations: (1) merging near-duplicate memories into a single authoritative entry, (2) promoting high-confidence, high-access-frequency entries to a privileged `core` tier for fast retrieval, (3) expiring and archiving entries whose decayed confidence has dropped below a configurable floor, and (4) rebuilding a lightweight entity-relationship knowledge graph over the surviving memory set to enable multi-hop retrieval.

The design draws from three external reference architectures. Letta's sleep-time agents are background processes that fire when the user is not actively querying — they read the current memory state, run an LLM over it, and write back a revised, more coherent state without any user interaction. MemGPT's three-tier memory model (core/recall/archival) provides the tier taxonomy: core memory is injected verbatim into every system prompt; recall memory is retrieved on demand via semantic search; archival memory is written to cold storage and only surfaced by explicit user query. Zep's graph consolidation approach uses bitemporal edge semantics — every fact has a `valid_time` interval and a `transaction_time` timestamp — so that when a fact is updated or superseded, the old version is expired (its `valid_to` is set) rather than deleted, preserving the full historical record.

The consolidation pipeline is a two-phase LLM process identical in structure to the mem0 v3 algorithm. Phase 1 (fact extraction) groups memories by embedding cluster, forms a textual representation of each cluster, and calls the LLM with `CONSOLIDATION_EXTRACTION_PROMPT` to produce a canonical fact per cluster. Phase 2 (reconciliation) calls the LLM with `RECONCILIATION_PROMPT` to classify each candidate against the existing memory: ADD a new canonical form, UPDATE an existing entry with the canonical form, DELETE a superseded entry, or NOOP if no change is needed. The result is a memory store that is smaller, denser, and higher-signal after every GC run.

The feature is intentionally low-touch: a user installs it once via `tag cron add` and it runs silently at the configured schedule (default: daily at 02:00 local time). All writes are journal-safe (SQLite WAL mode, atomic transactions). `--dry-run` mode is first-class: every planned mutation is printed without executing, enabling safe inspection before the first live run. Cost transparency is mandatory: before any LLM call, the estimated token count and USD cost are printed and confirmed (skipped with `--yes` or when `TAG_CI=true`).

---

## 2. Problem Statement

### 2.1 Memory Store Entropy Degrades Retrieval Quality

Every `tag memory add` call, every auto-extraction pass, and every import from an external source injects new entries into `semantic_memories`. There is no automatic deduplication beyond the exact-MD5 check added in PRD-025. Over time, the same conceptual fact accumulates in multiple phrasings — "always use pathlib not os.path", "prefer pathlib over os.path for all file operations", "use pathlib; do not use os.path" — each stored as a separate row with independent confidence scores. When the agent retrieves the top-K memories for context injection, these near-duplicates crowd out genuinely distinct entries. The user's effective context window is spent on redundant signal rather than diverse knowledge.

This problem compounds for long-lived profiles. A coder profile used daily for six months will have O(hundreds) of entries. Without consolidation, FTS5 search degenerates toward recall-heavy, precision-poor results: many entries match, but only a fraction are non-redundant. Confidence decay alone (PRD-025) reduces the weight of stale entries but does not remove them from the index, so they continue to consume search result slots.

### 2.2 No Tier Differentiation — All Memories Are Treated Equally

PRD-025's current design has one tier: a flat `semantic_memories` table indexed by profile and memory type. High-confidence architectural decisions (e.g., "this service uses PostgreSQL 16 with row-level security on all user tables") are queried via the same FTS5 path as low-confidence, rarely-accessed facts from a one-off session six months ago. There is no way for the system to fast-path the injection of a small set of absolutely-critical memories that must appear in every agent turn, independent of semantic similarity to the current query.

Letta's core-memory block solves exactly this: a small, named block of text that is unconditionally included in the system prompt. TAG needs an analogous `core` tier whose entries are injected before semantic search results, guaranteeing that critical conventions (naming schemes, forbidden packages, deployment targets) are never crowded out by retrieval noise.

### 2.3 No Structured Entity Graph — Multi-Hop Retrieval Is Not Possible

The FTS5 index supports keyword search and the tool retrieval module (PRD-043) adds vector similarity. But neither supports multi-hop queries: "what files did we discuss in the same sessions where we encountered the rate-limit error?" Answering this requires traversing a graph of entities (files, commands, errors, sessions) and their relationships (MODIFIED, CAUSED, TRIGGERED, OCCURRED_IN). Without a graph, retrieving second-order context requires the agent to issue multiple queries and manually correlate results — burning context window and latency.

Zep's Graphiti and MemGPT's archival memory both demonstrate that even a lightweight in-process graph (implemented as SQLite edge tables) dramatically improves multi-hop context retrieval for coding-assistant workloads. TAG's memory GC process is the natural place to extract and update this entity graph from the accumulated memory store, because it already reads every memory entry during consolidation.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | A single `tag memory gc` command runs the full consolidation pipeline and exits cleanly, with no server or daemon required beyond the SQLite database. |
| G2 | `--dry-run` mode emits a complete plan of all merges, promotions, expirations, and graph updates without writing any rows to the database. |
| G3 | The pipeline merges near-duplicate memories (cosine similarity > configurable threshold, default 0.85) into a single canonical entry using a two-phase LLM reconciliation process. |
| G4 | Entries with decayed confidence below `gc.expire_threshold` (default 0.05) and `access_count` below `gc.expire_min_access` (default 2) are moved to a new `memory_archive` table rather than hard-deleted, preserving historical record. |
| G5 | Entries with decayed confidence above `gc.core_threshold` (default 0.80) and `access_count` above `gc.core_min_access` (default 10) are promoted to `memory_tier = 'core'` for unconditional system-prompt injection. |
| G6 | The entity-relationship knowledge graph (`kg_entities` + `kg_edges` tables) is rebuilt (or incrementally updated) from surviving memory entries after every GC run. |
| G7 | Every GC run is recorded in a `memory_gc_runs` table with full statistics: entries_before, entries_merged, entries_expired, entries_promoted, graph_nodes_added, graph_edges_added, llm_tokens_used, llm_cost_usd, duration_seconds. |
| G8 | `tag cron add --name memory-consolidation --schedule "@daily" "tag memory gc"` installs the job in the PRD-022 cron scheduler with no additional configuration. |
| G9 | All LLM calls respect the PRD-039 token budget; the pipeline aborts with a clear error if the estimated cost exceeds `gc.max_cost_usd` (default $0.10). |
| G10 | `--profile` scopes the run to a single profile; without it, all profiles are processed sequentially. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Real-time or mid-session consolidation. GC runs offline, between sessions, never while an agent loop is active. |
| NG2 | Cloud or multi-machine memory sync. All data remains in `~/.tag/runtime/tag.sqlite3`. |
| NG3 | Full graph-database semantics (SPARQL, Cypher). The KG is implemented in SQLite with simple edge traversal helpers; it does not require Neo4j, FalkorDB, or any external graph engine. |
| NG4 | Community detection / topic clustering in the initial release. Leiden-based community reports (described in research context) are deferred to a follow-up PRD. |
| NG5 | Automatic profile-wide memory rewrite (changing content of memories that are not near-duplicates). GC only merges, promotes, expires, or archives. It does not rewrite non-duplicate memories. |
| NG6 | Replacing the FTS5 index with a vector-only index. BM25 search via FTS5 remains the primary retrieval mechanism; the KG is an additive layer. |
| NG7 | UI for browsing the knowledge graph. Graph data is queryable via `tag memory graph` subcommands (separate PRD); this PRD only covers building and maintaining it. |
| NG8 | Bitemporal edge storage using PostgreSQL `tstzrange`. TAG targets SQLite; temporal validity is tracked with `valid_from` / `valid_to` ISO-8601 columns. |

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Duplicate reduction | GC reduces the memory count for a synthetic 200-entry profile with 40% duplicates by ≥ 35% in a single run | Automated test: seed profile, run GC, assert row count |
| Core tier promotion accuracy | ≥ 90% of manually-labelled "critical" entries in the test corpus are promoted to `core` tier after a GC run with default thresholds | Labelled fixture corpus + assertion |
| Archive integrity | 100% of expired entries are readable from `memory_archive` after expiration; no hard deletes | Row count assertion before/after |
| GC run duration | Full GC on a 500-entry profile completes in < 30 seconds on a 2020-era MacBook Pro (M1) | Performance test |
| Token cost | Full GC on a 500-entry profile costs < $0.05 using `claude-haiku-4-5` as the consolidation model | Cost assertion via mock LLM + token count |
| Zero data loss | After GC, every original entry is either present in `semantic_memories`, present in `memory_archive`, or transitively referenced via a merge provenance record in `memory_merge_provenance` | Provenance coverage assertion |
| Cron integration | `tag cron add --name memory-consolidation --schedule "@daily" "tag memory gc"` installs without error; the job fires within 60 seconds of its scheduled time in a test environment | Integration test with mock clock |
| Dry-run idempotency | Running `--dry-run` N times in succession produces identical output and leaves the database unchanged | Property-based test |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer with an active `coder` profile | run `tag memory gc --profile coder --dry-run` before the first live run | I can inspect exactly which entries will be merged, promoted, or expired without risking any data loss |
| U2 | Developer | install `tag cron add --name memory-consolidation --schedule "@daily" "tag memory gc"` once | my memory store is automatically cleaned up every night while I sleep, without any manual intervention |
| U3 | Developer | see GC run statistics with `tag memory gc --stats` | I can track how much the memory store shrank, how many entries reached `core` tier, and what the LLM API call cost was |
| U4 | Developer | run `tag memory gc --profile coder --max-cost 0.02` | the GC pipeline aborts before spending more than my budget, even if the profile is unusually large |
| U5 | Platform engineer | query the entity graph after a GC run to find which files co-occur with a specific error entity | I can build multi-hop context retrieval features without issuing multiple unrelated memory searches |
| U6 | Developer | run `tag memory gc --list-runs` | I can see when the last GC ran, how long it took, and whether it succeeded or was aborted due to cost |
| U7 | Developer | run `tag memory gc --rollback <run_id>` | I can undo a GC run that promoted or expired entries I disagree with, restoring the pre-run state from the provenance table |
| U8 | Developer | configure `gc.similarity_threshold: 0.90` in `config.yaml` for a strict deduplication policy | only very nearly identical entries are merged; nuanced phrasings of related but distinct facts are preserved |
| U9 | Security-conscious developer | see that `tag memory gc` never sends raw memory content to the LLM without first stripping secrets detected by PRD-034's scanner | API keys, tokens, and passwords stored in memory entries are redacted before any LLM consolidation call |

---

## 7. Proposed CLI Surface

All GC subcommands live under `tag memory gc`. The `tag cron` integration uses the existing PRD-022 cron namespace.

### 7.1 `tag memory gc` — Run consolidation

```
tag memory gc \
  [--profile PROFILE] \
  [--dry-run] \
  [--yes] \
  [--max-cost FLOAT] \
  [--similarity-threshold FLOAT] \
  [--expire-threshold FLOAT] \
  [--core-threshold FLOAT] \
  [--model MODEL_ID] \
  [--skip-graph] \
  [--skip-merge] \
  [--skip-expire] \
  [--skip-promote] \
  [--json] \
  [--verbose]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--profile PROFILE` | all profiles | Scope GC to a single profile name |
| `--dry-run` | false | Plan and print all mutations; write nothing |
| `--yes` | false | Skip cost confirmation prompt (auto-set when `TAG_CI=true`) |
| `--max-cost FLOAT` | 0.10 | Abort if estimated LLM cost exceeds this value in USD |
| `--similarity-threshold FLOAT` | 0.85 | Cosine similarity above which two entries are near-duplicates |
| `--expire-threshold FLOAT` | 0.05 | Decayed confidence below which an entry is a candidate for archival |
| `--core-threshold FLOAT` | 0.80 | Decayed confidence above which, combined with access count, an entry is a candidate for `core` tier |
| `--model MODEL_ID` | `anthropic/claude-haiku-4-5` | LLM model for consolidation calls |
| `--skip-graph` | false | Skip knowledge graph rebuild step |
| `--skip-merge` | false | Skip near-duplicate merge step |
| `--skip-expire` | false | Skip expiration/archival step |
| `--skip-promote` | false | Skip core-tier promotion step |
| `--json` | false | Output machine-readable JSON report |
| `--verbose` | false | Print per-entry decisions during processing |

**Example output (human-readable, no `--json`):**

```
tag memory gc --profile coder --dry-run

Memory GC — profile: coder                                       [DRY RUN]
Loaded 247 entries  (core: 3, recall: 238, archival: 6)

Phase 1 — Near-Duplicate Detection
  Embedding 247 entries... done (3.2 s)
  Found 18 duplicate clusters (41 entries → 18 canonical)
  Estimated LLM tokens for merge phase: ~12,400  (~$0.004)

Phase 2 — Expiration Candidates
  Entries below confidence floor (0.05): 22
  Of those, access_count < 2: 14  →  14 entries queued for archive

Phase 3 — Core Promotion Candidates
  Entries above confidence 0.80 AND access_count ≥ 10: 7
  Currently in core tier: 3  →  4 new promotions

Phase 4 — Knowledge Graph Rebuild
  Entities to extract: ~247 entries
  Estimated new nodes: ~34, new edges: ~61

──────────────────────────────────────────────────────────────────────────
Total estimated LLM cost:  $0.007  (within --max-cost $0.10 ✓)
Planned mutations:
  MERGE   41 entries → 18 (saves 23 rows)
  ARCHIVE 14 entries → memory_archive
  PROMOTE  4 entries to core tier
  GRAPH   +34 nodes, +61 edges (incremental)

Run without --dry-run to apply.
```

### 7.2 `tag memory gc --stats` — Show run history

```
tag memory gc --stats [--last N] [--profile PROFILE] [--json]
```

**Example output:**

```
GC Run History — all profiles

Run ID      Profile   Timestamp            Duration   Merged  Expired  Promoted  Cost
──────────────────────────────────────────────────────────────────────────────────────
gc-a3f2b1  coder     2026-06-16 02:01:14  18.4 s     23      14       4         $0.007
gc-c9d41e  coder     2026-06-15 02:00:58  21.1 s     2       1        0         $0.003
gc-88fa02  writer    2026-06-16 02:02:31  9.3 s      5       3        1         $0.002
```

### 7.3 `tag memory gc --rollback <run_id>` — Undo a GC run

```
tag memory gc --rollback gc-a3f2b1 [--yes]
```

Restores all entries modified by run `gc-a3f2b1` to their pre-run state using the `memory_merge_provenance` and `memory_gc_snapshots` tables. Prints a diff of what will be restored and prompts for confirmation unless `--yes` is set.

### 7.4 `tag cron add` integration

```bash
# Install daily GC at 02:00
tag cron add \
  --name memory-consolidation \
  --schedule "0 2 * * *" \
  "tag memory gc --yes"

# Use the @daily shorthand (PRD-022 expands to "0 0 * * *")
tag cron add \
  --name memory-consolidation \
  --schedule "@daily" \
  "tag memory gc --yes"

# List scheduled jobs to verify
tag cron list
# NAME                    SCHEDULE       LAST RUN             STATUS
# memory-consolidation    0 2 * * *      2026-06-16 02:00:58  ok
```

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag memory gc` MUST run the full four-phase pipeline (merge, expire, promote, graph) in sequence and emit a summary report to stdout. | Must |
| FR-02 | `--dry-run` MUST NOT write any rows to `semantic_memories`, `memory_archive`, `kg_entities`, `kg_edges`, or any GC audit table. It MUST print every planned mutation. | Must |
| FR-03 | Before any LLM API call, the pipeline MUST estimate the total token count and USD cost, display it to the user, and abort if it exceeds `--max-cost`. | Must |
| FR-04 | The merge phase MUST use cosine similarity over sentence-transformer embeddings (same model as PRD-025 / PRD-043: `all-MiniLM-L6-v2`) to detect clusters of near-duplicate entries with similarity ≥ `--similarity-threshold`. | Must |
| FR-05 | For each duplicate cluster, the pipeline MUST call the LLM with `CONSOLIDATION_EXTRACTION_PROMPT` to produce a single canonical fact string, then call the LLM with `RECONCILIATION_PROMPT` to determine the ADD/UPDATE/DELETE/NOOP disposition for each cluster member. | Must |
| FR-06 | Every memory entry that is merged-away (classified DELETE in the reconciliation phase) MUST have a corresponding row written to `memory_merge_provenance` before deletion from `semantic_memories`. | Must |
| FR-07 | The expiration phase MUST compute `effective_confidence = confidence_base * 2^(-age_days / half_life)` (identical to PRD-025 `compute_confidence`) and move entries below `--expire-threshold` with `access_count < gc.expire_min_access` to `memory_archive`. | Must |
| FR-08 | The promotion phase MUST set `memory_tier = 'core'` on entries satisfying `effective_confidence >= --core-threshold AND access_count >= gc.core_min_access`. Core entries MUST be injected unconditionally into agent system prompts, before semantic-search recall results. | Must |
| FR-09 | The graph rebuild phase MUST extract entity mentions and relationship triplets from surviving memory entries using `ENTITY_EXTRACTION_PROMPT` and write them to `kg_entities` and `kg_edges` as incremental upserts. | Must |
| FR-10 | Every GC run MUST write a row to `memory_gc_runs` with: run_id, profile, started_at, finished_at, phase outcomes, llm_tokens_used, llm_cost_usd, status (ok/aborted/failed), and error_message if applicable. | Must |
| FR-11 | `--rollback <run_id>` MUST restore all rows modified by the identified run to their pre-mutation values, using data stored in `memory_gc_snapshots`. It MUST also delete KG entities/edges added in that run. | Must |
| FR-12 | All database mutations during a GC run MUST be wrapped in a single SQLite transaction per phase. If any phase fails, its transaction is rolled back without affecting completed phases. | Must |
| FR-13 | Secret detection (PRD-034) MUST be applied to each memory entry's content before it is sent to the LLM. Secrets MUST be replaced with `[REDACTED:<type>]` placeholders. A warning MUST be emitted for each redaction. | Must |
| FR-14 | `--profile` MUST scope the GC run to only entries where `semantic_memories.profile = PROFILE`. Without the flag, all distinct profiles are processed in lexicographic order. | Must |
| FR-15 | GC MUST abort cleanly if another GC run is already in progress for the same profile, using a SQLite advisory lock (insert into a `memory_gc_locks` table, cleared on completion or crash recovery). | Must |
| FR-16 | `--skip-merge`, `--skip-expire`, `--skip-promote`, `--skip-graph` MUST independently disable the corresponding phase without affecting others. | Should |
| FR-17 | `tag memory gc --stats` MUST read from `memory_gc_runs` and display a formatted table of the last N runs (default 10). | Should |
| FR-18 | LLM calls MUST be traced via PRD-013's tracing infrastructure (a `gc_consolidation` span wrapping each LLM call cluster). | Should |
| FR-19 | The pipeline MUST respect the PRD-039 token budget. If the remaining token budget for the current budget period is below the estimated GC cost, the pipeline MUST warn and require `--yes` to proceed. | Should |
| FR-20 | `--json` MUST emit a single JSON object after completion with all statistics matching the `GCRunReport` dataclass schema. | Should |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | GC runtime on a 500-entry single-profile corpus MUST complete in < 30 seconds on Apple M1 hardware. | Performance |
| NFR-02 | GC MUST NOT hold any SQLite write lock for more than 500 ms at a time; reads from `semantic_memories` by the agent loop MUST never be blocked. | Concurrency |
| NFR-03 | Memory usage of the `memory_gc.py` process MUST NOT exceed 512 MB RSS during graph rebuild on a 1,000-entry corpus (embedding matrix fits in RAM). | Resource |
| NFR-04 | GC MUST produce identical results (same merge decisions, same archive set, same promotions) given the same database state and LLM responses; the pipeline MUST be deterministic when the LLM is mocked. | Correctness |
| NFR-05 | No memory entry is permanently destroyed without a provenance record. Hard-delete is never performed on `semantic_memories`; only soft-move to `memory_archive`. | Safety |
| NFR-06 | `memory_gc.py` MUST have zero mandatory external dependencies beyond the packages already required by PRD-025 (`sentence-transformers`) and PRD-043 (`numpy`). The `anthropic` SDK is already a core TAG dependency. | Dependency |
| NFR-07 | All new SQLite tables introduced by this PRD MUST be created by `ensure_gc_schema(conn)` called at GC startup; no migration scripts or external schema managers are required. | Operability |
| NFR-08 | `--dry-run` execution time MUST be < 10 seconds for a 500-entry corpus (embedding phase runs, LLM phase is estimated but not called). | Performance |
| NFR-09 | GC log output written to `~/.tag/logs/memory_gc.log` MUST be append-only, include ISO-8601 timestamps, and MUST NOT contain raw memory content (only entry IDs and anonymized statistics). | Security |
| NFR-10 | When run as a cron job (`TAG_CI=true` or `--yes`), all prompts are suppressed and exit code 0 indicates success, exit code 1 indicates partial failure, exit code 2 indicates hard abort (cost exceeded or lock contention). | Operability |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/memory_gc.py` | Main GC pipeline: phases, LLM prompts, dataclasses, `cmd_memory_gc` controller entry point |
| `src/tag/memory_graph.py` | KG entity/edge extraction helpers, `kg_search()`, graph traversal utilities |
| `tests/test_memory_gc.py` | Unit and integration tests for the GC pipeline |
| `tests/test_memory_graph.py` | Unit tests for KG extraction and traversal |

### 10.2 SQLite Schema

All new tables are created by `ensure_gc_schema(conn: sqlite3.Connection)` called at GC startup. This function is idempotent (uses `CREATE TABLE IF NOT EXISTS`).

```sql
-- ─────────────────────────────────────────────────────────────────────────────
-- memory_archive: soft-deleted / expired entries, fully queryable
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS memory_archive (
    id              TEXT PRIMARY KEY,          -- original semantic_memories.id
    profile         TEXT NOT NULL,
    content         TEXT NOT NULL,
    memory_type     TEXT NOT NULL,
    confidence      REAL NOT NULL,             -- confidence_base at time of archival
    confidence_eff  REAL NOT NULL,             -- effective decayed confidence at archival
    created_at      TEXT NOT NULL,
    accessed_at     TEXT NOT NULL,
    access_count    INTEGER NOT NULL,
    source          TEXT NOT NULL DEFAULT 'manual',
    archived_at     TEXT NOT NULL,             -- ISO-8601 UTC
    archive_reason  TEXT NOT NULL              -- 'expired' | 'merged' | 'manual'
);
CREATE INDEX IF NOT EXISTS idx_ma_profile ON memory_archive(profile, archived_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- memory_merge_provenance: audit trail for every merge operation
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS memory_merge_provenance (
    id              TEXT PRIMARY KEY,          -- uuid
    gc_run_id       TEXT NOT NULL,
    canonical_id    TEXT NOT NULL,             -- resulting entry in semantic_memories
    source_id       TEXT NOT NULL,             -- entry that was merged-away
    source_content  TEXT NOT NULL,             -- content at time of merge
    created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mmp_run  ON memory_merge_provenance(gc_run_id);
CREATE INDEX IF NOT EXISTS idx_mmp_src  ON memory_merge_provenance(source_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- memory_gc_runs: one row per GC execution
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS memory_gc_runs (
    run_id          TEXT PRIMARY KEY,          -- 'gc-' + 6 hex chars
    profile         TEXT,                      -- NULL means all profiles
    started_at      TEXT NOT NULL,
    finished_at     TEXT,
    entries_before  INTEGER NOT NULL DEFAULT 0,
    entries_merged  INTEGER NOT NULL DEFAULT 0,
    entries_expired INTEGER NOT NULL DEFAULT 0,
    entries_promoted INTEGER NOT NULL DEFAULT 0,
    graph_nodes_added INTEGER NOT NULL DEFAULT 0,
    graph_edges_added INTEGER NOT NULL DEFAULT 0,
    llm_tokens_used INTEGER NOT NULL DEFAULT 0,
    llm_cost_usd    REAL NOT NULL DEFAULT 0.0,
    duration_seconds REAL,
    status          TEXT NOT NULL DEFAULT 'running', -- 'running'|'ok'|'aborted'|'failed'
    error_message   TEXT,
    dry_run         INTEGER NOT NULL DEFAULT 0  -- 1 = dry run
);
CREATE INDEX IF NOT EXISTS idx_mgr_profile ON memory_gc_runs(profile, started_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- memory_gc_snapshots: per-entry before-image for rollback
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS memory_gc_snapshots (
    id              TEXT PRIMARY KEY,          -- uuid
    gc_run_id       TEXT NOT NULL,
    entry_id        TEXT NOT NULL,             -- semantic_memories.id
    operation       TEXT NOT NULL,             -- 'merge'|'expire'|'promote'
    before_state    TEXT NOT NULL              -- JSON blob of the row before mutation
);
CREATE INDEX IF NOT EXISTS idx_mgs_run ON memory_gc_snapshots(gc_run_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- memory_gc_locks: advisory lock table (one row per active GC run per profile)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS memory_gc_locks (
    profile         TEXT PRIMARY KEY,          -- 'ALL' for cross-profile runs
    run_id          TEXT NOT NULL,
    locked_at       TEXT NOT NULL,
    pid             INTEGER NOT NULL
);

-- ─────────────────────────────────────────────────────────────────────────────
-- kg_entities: knowledge graph nodes extracted from memory entries
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS kg_entities (
    id              TEXT PRIMARY KEY,          -- uuid
    profile         TEXT NOT NULL,
    name            TEXT NOT NULL,             -- canonical entity name
    entity_type     TEXT NOT NULL,             -- 'file'|'command'|'error'|'concept'|'person'|'tool'
    first_seen      TEXT NOT NULL,
    last_seen       TEXT NOT NULL,
    mention_count   INTEGER NOT NULL DEFAULT 1,
    embedding       BLOB                       -- serialised numpy float32 array, nullable
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_kge_name ON kg_entities(profile, name, entity_type);
CREATE INDEX IF NOT EXISTS idx_kge_type ON kg_entities(profile, entity_type);

-- ─────────────────────────────────────────────────────────────────────────────
-- kg_edges: knowledge graph edges (relationship triplets)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS kg_edges (
    id              TEXT PRIMARY KEY,          -- uuid
    profile         TEXT NOT NULL,
    source_id       TEXT NOT NULL REFERENCES kg_entities(id),
    target_id       TEXT NOT NULL REFERENCES kg_entities(id),
    relation        TEXT NOT NULL,             -- 'MODIFIED'|'CAUSED'|'TRIGGERED'|'USED_IN'|'CO_OCCURS_WITH'|etc.
    weight          REAL NOT NULL DEFAULT 1.0,
    valid_from      TEXT NOT NULL,             -- ISO-8601: when this fact became valid
    valid_to        TEXT,                      -- NULL = currently valid
    memory_id       TEXT,                      -- originating semantic_memories.id (nullable after merge)
    created_at      TEXT NOT NULL,
    gc_run_id       TEXT                       -- GC run that created this edge
);
CREATE INDEX IF NOT EXISTS idx_kge_src ON kg_edges(profile, source_id);
CREATE INDEX IF NOT EXISTS idx_kge_tgt ON kg_edges(profile, target_id);
CREATE INDEX IF NOT EXISTS idx_kge_rel ON kg_edges(profile, relation);

-- ─────────────────────────────────────────────────────────────────────────────
-- Extend semantic_memories with tier column (ALTER TABLE, idempotent via try/except)
-- ─────────────────────────────────────────────────────────────────────────────
-- Applied in ensure_gc_schema() as:
--   conn.execute("ALTER TABLE semantic_memories ADD COLUMN memory_tier TEXT NOT NULL DEFAULT 'recall'")
-- (wrapped in try/except sqlite3.OperationalError to be idempotent)
```

### 10.3 Core Python Dataclasses

```python
# src/tag/memory_gc.py
from __future__ import annotations

import json
import uuid
from dataclasses import dataclass, field, asdict
from datetime import datetime, timezone
from typing import Literal


OperationT = Literal["ADD", "UPDATE", "DELETE", "NOOP"]
PhaseStatusT = Literal["ok", "skipped", "failed", "dry_run"]
GCStatusT = Literal["running", "ok", "aborted", "failed"]
MemoryTierT = Literal["core", "recall", "archival"]
EntityTypeT = Literal["file", "command", "error", "concept", "person", "tool"]


@dataclass
class MemoryEntry:
    """In-memory representation of a semantic_memories row during GC."""
    id: str
    profile: str
    content: str
    memory_type: str
    confidence_base: float
    confidence_eff: float       # computed: confidence_base * 2^(-age/half_life)
    memory_tier: str = "recall"
    created_at: str = ""
    accessed_at: str = ""
    access_count: int = 0
    source: str = "manual"
    embedding: "np.ndarray | None" = field(default=None, repr=False)


@dataclass
class MergeCluster:
    """A group of near-duplicate MemoryEntry objects that should be consolidated."""
    cluster_id: str
    members: list[MemoryEntry]
    canonical_content: str = ""          # set after LLM extraction
    canonical_confidence: float = 0.0
    llm_disposition: list[dict] = field(default_factory=list)  # UPDATE_MEMORY_PROMPT output


@dataclass
class GCPhaseResult:
    phase: str
    status: PhaseStatusT
    entries_affected: int = 0
    llm_tokens: int = 0
    llm_cost_usd: float = 0.0
    mutations: list[str] = field(default_factory=list)  # human-readable descriptions
    error: str | None = None


@dataclass
class GCRunReport:
    run_id: str
    profile: str | None
    started_at: str
    finished_at: str
    dry_run: bool
    entries_before: int
    entries_merged: int
    entries_expired: int
    entries_promoted: int
    graph_nodes_added: int
    graph_edges_added: int
    llm_tokens_used: int
    llm_cost_usd: float
    duration_seconds: float
    status: GCStatusT
    error_message: str | None
    phases: list[GCPhaseResult] = field(default_factory=list)

    def to_json(self) -> str:
        return json.dumps(asdict(self), indent=2)


@dataclass
class KGEntity:
    id: str
    profile: str
    name: str
    entity_type: EntityTypeT
    first_seen: str
    last_seen: str
    mention_count: int = 1


@dataclass
class KGEdge:
    id: str
    profile: str
    source_id: str
    target_id: str
    relation: str
    weight: float = 1.0
    valid_from: str = ""
    valid_to: str | None = None
    memory_id: str | None = None
    gc_run_id: str | None = None
```

### 10.4 LLM Prompts

```python
# src/tag/memory_gc.py  (prompt constants)

CONSOLIDATION_EXTRACTION_PROMPT = """\
You are a precise memory consolidation assistant for a software developer's AI CLI tool.
You are given a cluster of near-duplicate memory entries that represent the same underlying fact.
Your job is to produce ONE canonical, concise, accurate statement that captures the full meaning
of all entries in the cluster, eliminating redundancy.

Rules:
- Prefer the most specific, actionable phrasing.
- If entries conflict (e.g., different filenames or different conclusions), note the conflict
  with "CONFLICT:" prefix and keep both alternatives separated by " | ".
- Output ONLY the canonical fact string — no explanation, no prefix, no JSON.

Cluster entries:
{cluster_text}

Canonical fact:"""


RECONCILIATION_PROMPT = """\
You are a memory reconciliation agent. You are given:
1. A CANONICAL fact that should be the new authoritative form of a cluster.
2. The EXISTING memory entries from the cluster.

For each existing entry, output a JSON array where each element has:
  - "id": the entry ID
  - "event": one of "UPDATE" (this entry becomes the canonical), "DELETE" (remove this entry),
             or "NOOP" (keep unchanged — should be rare in a duplicate cluster)
  - "old_content": the original content (only for UPDATE)

Exactly ONE entry should be "UPDATE" (the one closest to the canonical fact);
all others should be "DELETE".

Canonical fact: {canonical_fact}

Existing entries:
{entries_json}

JSON output:"""


ENTITY_EXTRACTION_PROMPT = """\
You are an entity and relationship extractor for a software developer's memory graph.
Given a memory entry, extract:
1. Named entities with their types (file, command, error, concept, person, tool).
2. Relationships between entity pairs as (subject, relation, object) triplets.
   Valid relations: MODIFIED, CAUSED, TRIGGERED, USED_IN, CO_OCCURS_WITH, PRODUCES, FIXED_BY,
   DEPENDS_ON, CONFIGURED_BY, BELONGS_TO.

Output ONLY a JSON object with two keys:
  "entities": [{"name": str, "type": str}]
  "edges":    [{"subject": str, "relation": str, "object": str, "weight": float}]

If no entities or relationships are found, output {"entities": [], "edges": []}.

Memory entry (ID: {memory_id}):
{content}

JSON:"""
```

### 10.5 Core Algorithm: Four-Phase Pipeline

```python
# src/tag/memory_gc.py  (pipeline sketch)

import sqlite3
import numpy as np
from tag.db import open_db
from tag.semantic_memory import compute_confidence, HALF_LIVES
from tag.security import scan_for_secrets   # PRD-034
from tag.tracing import span                 # PRD-013
from tag.budget import check_budget          # PRD-039


def run_gc_pipeline(
    conn: sqlite3.Connection,
    *,
    profile: str | None,
    dry_run: bool,
    similarity_threshold: float = 0.85,
    expire_threshold: float = 0.05,
    expire_min_access: int = 2,
    core_threshold: float = 0.80,
    core_min_access: int = 10,
    max_cost_usd: float = 0.10,
    model: str = "anthropic/claude-haiku-4-5",
    skip_merge: bool = False,
    skip_expire: bool = False,
    skip_promote: bool = False,
    skip_graph: bool = False,
    yes: bool = False,
) -> GCRunReport:
    run_id = "gc-" + uuid.uuid4().hex[:6]
    started_at = datetime.now(timezone.utc).isoformat()
    _acquire_lock(conn, profile, run_id)

    try:
        entries = _load_entries(conn, profile)
        report = GCRunReport(
            run_id=run_id, profile=profile, started_at=started_at,
            finished_at="", dry_run=dry_run, entries_before=len(entries),
            entries_merged=0, entries_expired=0, entries_promoted=0,
            graph_nodes_added=0, graph_edges_added=0,
            llm_tokens_used=0, llm_cost_usd=0.0,
            duration_seconds=0.0, status="running", error_message=None,
        )

        # Phase 1: Near-duplicate merge
        if not skip_merge:
            with span("gc.phase.merge"):
                phase = _phase_merge(
                    conn, entries, run_id,
                    threshold=similarity_threshold,
                    model=model, dry_run=dry_run,
                    max_cost_usd=max_cost_usd, yes=yes,
                )
            report.phases.append(phase)
            report.entries_merged = phase.entries_affected
            report.llm_tokens_used += phase.llm_tokens
            report.llm_cost_usd += phase.llm_cost_usd
            # Reload entries after merge (some were deleted)
            entries = _load_entries(conn, profile)

        # Phase 2: Expiration / archival
        if not skip_expire:
            with span("gc.phase.expire"):
                phase = _phase_expire(
                    conn, entries, run_id,
                    threshold=expire_threshold,
                    min_access=expire_min_access,
                    dry_run=dry_run,
                )
            report.phases.append(phase)
            report.entries_expired = phase.entries_affected
            entries = [e for e in entries if e.id not in {
                m.split()[0] for m in phase.mutations if m.startswith("ARCHIVE")
            }]

        # Phase 3: Core-tier promotion
        if not skip_promote:
            with span("gc.phase.promote"):
                phase = _phase_promote(
                    conn, entries, run_id,
                    threshold=core_threshold,
                    min_access=core_min_access,
                    dry_run=dry_run,
                )
            report.phases.append(phase)
            report.entries_promoted = phase.entries_affected

        # Phase 4: Knowledge graph rebuild
        if not skip_graph:
            with span("gc.phase.graph"):
                phase = _phase_graph(
                    conn, entries, run_id,
                    model=model, dry_run=dry_run,
                    max_cost_usd=max_cost_usd - report.llm_cost_usd,
                    yes=yes,
                )
            report.phases.append(phase)
            report.graph_nodes_added = phase.entries_affected  # repurposed field
            report.llm_tokens_used += phase.llm_tokens
            report.llm_cost_usd += phase.llm_cost_usd

        report.status = "ok"
        return report

    except Exception as exc:
        report.status = "failed"
        report.error_message = str(exc)
        raise
    finally:
        report.finished_at = datetime.now(timezone.utc).isoformat()
        _release_lock(conn, profile)
        if not dry_run:
            _write_run_record(conn, report)


def _embed_entries(entries: list[MemoryEntry]) -> np.ndarray:
    """Return (N, D) float32 embedding matrix using all-MiniLM-L6-v2."""
    from sentence_transformers import SentenceTransformer
    model = SentenceTransformer("all-MiniLM-L6-v2")
    texts = [e.content for e in entries]
    return model.encode(texts, normalize_embeddings=True, show_progress_bar=False)


def _cluster_by_similarity(
    embeddings: np.ndarray,
    threshold: float,
) -> list[list[int]]:
    """
    Greedy single-linkage clustering.
    Returns list of index clusters where any pair has cosine_sim >= threshold.
    Complexity: O(N^2) — acceptable for N < 2000.
    """
    sim = embeddings @ embeddings.T  # (N, N) cosine similarity (embeddings are L2-normalised)
    N = len(embeddings)
    assigned = [-1] * N
    clusters: list[list[int]] = []
    for i in range(N):
        if assigned[i] != -1:
            continue
        cluster = [i]
        assigned[i] = len(clusters)
        for j in range(i + 1, N):
            if assigned[j] == -1 and sim[i, j] >= threshold:
                cluster.append(j)
                assigned[j] = len(clusters)
        clusters.append(cluster)
    return [c for c in clusters if len(c) > 1]  # only return multi-member clusters


def _redact_secrets(content: str) -> str:
    """Apply PRD-034 secret scanning; replace hits with [REDACTED:<type>]."""
    hits = scan_for_secrets(content)
    for hit in hits:
        content = content.replace(hit.value, f"[REDACTED:{hit.secret_type}]")
    return content
```

### 10.6 Integration Points

| Integration | How |
|-------------|-----|
| `open_db()` | Called at GC startup to obtain the WAL-mode SQLite connection; all schema creation runs through this connection. |
| `semantic_memory.compute_confidence()` | Reused verbatim for phase 2 (expiration) and phase 3 (promotion) to compute effective confidence; no duplication. |
| `semantic_memory.ensure_schema()` | Called before `ensure_gc_schema()` to guarantee `semantic_memories` exists. |
| `tool_retrieval.py` / `SentenceTransformer` | The same `all-MiniLM-L6-v2` model instance is reused for embedding; `_embed_entries()` calls `SentenceTransformer` directly. |
| `tracing.span()` (PRD-013) | Each pipeline phase is wrapped in a named span; LLM calls emit child spans. |
| `budget.check_budget()` (PRD-012/039) | Called before LLM phases with the estimated token count; raises `BudgetExceededError` if over limit. |
| `security.scan_for_secrets()` (PRD-034) | Applied to every memory entry's content string before it is included in an LLM prompt. |
| `cron_scheduler.py` (PRD-022) | `tag cron add "tag memory gc --yes"` registers via the existing cron table; no changes to cron_scheduler.py are required. |
| `hermes_bridge.py` | After GC updates `memory_tier = 'core'`, the next call to `hermes_env()` must inject core-tier entries unconditionally. A small patch to `hermes_env()` reads `SELECT content FROM semantic_memories WHERE profile=? AND memory_tier='core'` and prepends them before semantic-search results. |

### 10.7 Controller Entry Point

```python
# In src/tag/controller.py — new subcommand handler (abbreviated)

def cmd_memory_gc(args: argparse.Namespace) -> int:
    """Entry point for `tag memory gc` and `tag memory gc --stats`."""
    from tag.memory_gc import (
        run_gc_pipeline, ensure_gc_schema, list_gc_runs, rollback_gc_run
    )
    conn = open_db()
    ensure_gc_schema(conn)

    if args.stats:
        rows = list_gc_runs(conn, profile=args.profile, limit=args.last)
        _print_gc_stats_table(rows)
        return 0

    if args.rollback:
        return rollback_gc_run(conn, args.rollback, yes=args.yes)

    report = run_gc_pipeline(
        conn,
        profile=args.profile,
        dry_run=args.dry_run,
        similarity_threshold=args.similarity_threshold,
        expire_threshold=args.expire_threshold,
        core_threshold=args.core_threshold,
        max_cost_usd=args.max_cost,
        model=args.model,
        skip_merge=args.skip_merge,
        skip_expire=args.skip_expire,
        skip_promote=args.skip_promote,
        skip_graph=args.skip_graph,
        yes=args.yes,
    )

    if args.json:
        print(report.to_json())
    else:
        _print_gc_summary(report)

    return 0 if report.status == "ok" else (1 if report.status == "failed" else 2)
```

---

## 11. Security Considerations

1. **Secret redaction before LLM calls.** Every memory entry's content is passed through PRD-034's `scan_for_secrets()` before inclusion in any LLM prompt. Detected secrets (API keys, tokens, passwords, connection strings) are replaced with `[REDACTED:<type>]` placeholders. A warning line is emitted per redaction. The LLM never receives raw secret values.

2. **No network calls without explicit user opt-in.** The embedding phase (sentence-transformers) runs fully locally. LLM calls are only initiated after the user confirms the cost estimate (or `--yes` / `TAG_CI=true` is set). The GC process makes no other network calls.

3. **SQLite WAL mode and atomic transactions.** All mutations within a phase are wrapped in a single transaction. A crash or SIGTERM between phases leaves only completed phases' data committed; no partial-phase data is persisted. The `memory_gc_locks` table prevents concurrent GC runs on the same profile from creating race conditions.

4. **Provenance table never truncated.** `memory_merge_provenance` and `memory_gc_snapshots` are append-only. No GC operation deletes rows from these audit tables. Rollback relies on this guarantee.

5. **Log files contain no memory content.** `~/.tag/logs/memory_gc.log` records only entry IDs, counts, and anonymized statistics. Raw memory content is never written to log files to prevent inadvertent exposure in log-aggregation systems.

6. **`memory_archive` is not hard-deleted.** Expired entries are moved to `memory_archive`, not dropped. This prevents accidental data loss and gives users a window to recover entries via `--rollback` or manual SQL query.

7. **Cron command injection prevention.** The `tag cron add` command that schedules GC passes the command string through PRD-034's existing cron-command sanitisation. Shell metacharacters in the `--name` or `--schedule` arguments are rejected at parse time.

8. **LLM output validation.** The JSON output from `RECONCILIATION_PROMPT` and `ENTITY_EXTRACTION_PROMPT` is parsed with strict schema validation (Pydantic or manual field checks). Malformed LLM output causes the containing cluster to be skipped with a warning; it never corrupts the database.

9. **Profile isolation.** GC operations are strictly scoped to the `profile` column in all tables. A GC run for profile `coder` cannot read, modify, or expire entries belonging to profile `writer`. The `WHERE profile = ?` clause is enforced at every query site; there is no `SELECT *` without a profile filter.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_memory_gc.py`)

| Test | Description |
|------|-------------|
| `test_cluster_by_similarity_returns_only_multi_member` | Mock 6 embeddings; 3 pairs above threshold, 1 singleton — assert clusters have length ≥ 2. |
| `test_cluster_by_similarity_threshold_respected` | Vary threshold from 0.5 to 0.99; assert cluster count changes monotonically. |
| `test_consolidation_extraction_prompt_format` | Assert `CONSOLIDATION_EXTRACTION_PROMPT.format(cluster_text="x")` produces a well-formed string with no missing interpolation tokens. |
| `test_reconciliation_prompt_parse` | Feed a realistic LLM JSON response; assert `_parse_reconciliation_output()` returns correct `OperationT` values for each entry. |
| `test_expiration_phase_moves_to_archive` | Seed 5 entries, 2 below expire_threshold; run `_phase_expire()` with `dry_run=False`; assert 2 rows in `memory_archive` and 3 in `semantic_memories`. |
| `test_expiration_dry_run_no_writes` | Same setup; run with `dry_run=True`; assert both tables unchanged. |
| `test_promotion_phase_sets_core_tier` | Seed 10 entries, 3 with high confidence + high access_count; run `_phase_promote()`; assert 3 rows have `memory_tier='core'`. |
| `test_secret_redaction_before_llm` | Inject an entry with content containing an AWS key pattern; assert `_redact_secrets()` returns content with `[REDACTED:aws_key]`. |
| `test_advisory_lock_prevents_concurrent_gc` | Insert a row into `memory_gc_locks` for a profile; assert `_acquire_lock()` raises `GCLockError`. |
| `test_rollback_restores_snapshots` | Run a full pipeline on a seeded database; call `rollback_gc_run()`; assert all `semantic_memories` rows match pre-GC state exactly. |
| `test_cost_abort_below_max` | Mock LLM token estimator to return cost above `max_cost_usd`; assert pipeline raises `GCCostAbortError` before any writes. |
| `test_ensure_gc_schema_idempotent` | Call `ensure_gc_schema()` twice on a fresh in-memory SQLite; assert no error on second call. |

### 12.2 Integration Tests

| Test | Description |
|------|-------------|
| `test_full_pipeline_on_seeded_corpus` | Create 200 entries in a temp SQLite (40 near-duplicate pairs); run full pipeline with a mocked LLM; assert merged count ≥ 35 and archive count matches entries below threshold. |
| `test_cron_fires_gc` | Register `tag memory gc --yes` via `tag cron add`; advance mock clock to fire time; assert `memory_gc_runs` has a new `ok` row. |
| `test_json_output_schema` | Run GC with `--json`; parse stdout; assert all fields in `GCRunReport.to_json()` are present and correctly typed. |
| `test_hermes_env_injects_core_memories` | Promote 2 entries to `core`; call `hermes_env()`; assert both entries appear in the system prompt prefix before any FTS5-retrieved memories. |
| `test_gc_with_real_embeddings` | End-to-end test with real sentence-transformers (mark `@pytest.mark.slow`); seed 50 entries with 10 known duplicate pairs; assert pipeline finds all 10 clusters. |

### 12.3 Performance Tests

| Test | Target | Method |
|------|--------|--------|
| `test_gc_500_entries_under_30s` | < 30 s on M1 | Seed 500 entries; time full pipeline with mock LLM; assert wall time. |
| `test_embedding_memory_under_512mb` | < 512 MB RSS | `resource.getrusage()` before/after embedding 1,000 entries; assert delta < 512 MB. |
| `test_dry_run_under_10s` | < 10 s on M1 | Same 500-entry seed; time `--dry-run` run; assert wall time. |

---

## 13. Acceptance Criteria

| ID | Criterion | How Verified |
|----|-----------|-------------|
| AC-01 | `tag memory gc --dry-run --profile coder` exits 0 and prints a plan without writing any rows to any GC table. | Integration test: assert row counts unchanged across all tables after dry run. |
| AC-02 | A seeded corpus of 200 entries with 40 known near-duplicate pairs (cosine sim > 0.90) is reduced by ≥ 35 unique rows after a live GC run with default thresholds. | Automated test with seeded SQLite + mocked LLM returning correct merge decisions. |
| AC-03 | Every merged-away entry has a corresponding row in `memory_merge_provenance` before it is removed from `semantic_memories`. | SQL assertion: `SELECT COUNT(*) FROM semantic_memories WHERE id IN (SELECT source_id FROM memory_merge_provenance WHERE gc_run_id=?)` = 0. |
| AC-04 | Every expired entry is present in `memory_archive` with `archive_reason = 'expired'` and all original columns preserved. | Row-by-row comparison between pre-GC snapshot and `memory_archive` content. |
| AC-05 | After GC, `SELECT COUNT(*) FROM semantic_memories WHERE memory_tier='core' AND profile=?` is greater than the pre-GC count, and all promoted entries satisfy `confidence_eff >= 0.80 AND access_count >= 10`. | Integration test with seeded entries designed to cross thresholds. |
| AC-06 | `tag memory gc --rollback <run_id>` restores the full pre-GC state of `semantic_memories` for all entries touched in that run, and removes all KG nodes/edges created in that run. | Rollback integration test: run GC, record state, rollback, assert state matches pre-GC. |
| AC-07 | Running `tag memory gc` when `max_cost_usd` would be exceeded causes a clean abort with exit code 2 and no database writes. | Unit test with token-count mock exceeding the configured limit. |
| AC-08 | Memory entries containing secret patterns (AWS keys, GitHub tokens, SSH private key headers) have secrets replaced with `[REDACTED:<type>]` in all LLM prompt strings; originals in SQLite are unmodified. | Unit test: inject entry with known secret; assert LLM prompt contains only `[REDACTED:*]`; assert `semantic_memories.content` unchanged after GC. |
| AC-09 | `tag cron add --name memory-consolidation --schedule "0 2 * * *" "tag memory gc --yes"` writes a row to the cron table and `tag cron list` shows it with the correct schedule. | Integration test using existing cron scaffold. |
| AC-10 | After a GC run, `tag memory gc --stats` shows the run with correct `entries_before`, `entries_merged`, `entries_expired`, `entries_promoted`, and `llm_cost_usd` values matching the actual run. | Assert `memory_gc_runs` row matches values reported on stdout. |
| AC-11 | Concurrent execution of two `tag memory gc` processes targeting the same profile causes the second to exit with a clear error message ("GC already running for profile X") and exit code 2 without corrupting the database. | Integration test: insert a lock row manually; run GC; assert early exit. |
| AC-12 | The `--json` flag produces valid JSON parseable to a `GCRunReport` with all fields present and matching the correct types. | Schema validation in integration test. |
| AC-13 | `hermes_env()` system prompt for a profile with `core`-tier entries contains those entries unconditionally, regardless of the semantic similarity of the current query to those entries. | Integration test: set one core entry and one recall entry; call `hermes_env()` with a query unrelated to the core entry; assert core entry appears in system prompt. |
| AC-14 | GC run duration for a 500-entry profile is < 30 seconds when the LLM is mocked (network latency excluded). | Performance test. |

---

## 14. Dependencies

| Dependency | Type | Version / Notes |
|------------|------|-----------------|
| `sentence-transformers` | Python package (already required by PRD-025/043) | `>=2.2.0`; `all-MiniLM-L6-v2` model |
| `numpy` | Python package (already required by PRD-043) | `>=1.24` |
| `anthropic` | Python package (core TAG dependency) | `>=0.20.0`; used for LLM consolidation calls |
| PRD-025 `semantic_memory.py` | Internal module | Must be merged before memory_gc.py is usable |
| PRD-022 `cron_scheduler.py` | Internal module | Required for `tag cron add` integration |
| PRD-013 `tracing.py` | Internal module | `span()` context manager used for GC phases |
| PRD-034 `security.py` | Internal module | `scan_for_secrets()` required for FR-13 |
| PRD-039 `budget.py` | Internal module | `check_budget()` required for FR-19 |
| PRD-012 cost tracking | Internal module | Token cost attribution written to `memory_gc_runs.llm_cost_usd` |
| SQLite FTS5 | SQLite extension | Must be present (standard on macOS; verify in `tag doctor`) |

---

## 15. Open Questions

| # | Question | Owner | Target |
|---|----------|-------|--------|
| OQ-1 | Should the merge phase use a fixed greedy single-linkage clustering algorithm, or switch to HDBSCAN for profiles with > 1,000 entries where O(N^2) cosine comparison becomes slow? HDBSCAN requires `scikit-learn` or `hdbscan` as a new dependency. | Maintainer | Before implementation |
| OQ-2 | Should `--rollback` restore KG edges to their pre-GC state, or is the KG always considered reconstructible from current memories (i.e., just re-run GC)? Full rollback requires storing pre-GC KG state snapshots which may be large. | Maintainer | Before implementation |
| OQ-3 | Should `core`-tier entries be user-editable (via `tag memory promote <id>` / `tag memory demote <id>`) independent of the GC thresholds? This would be a separate CLI surface PRD but the schema must support it from day one. | Maintainer | Before schema freeze |
| OQ-4 | What is the correct half-life for a `core`-tier entry? The current design never decays core entries once promoted. Should there be an automatic demotion back to `recall` if `access_count` drops significantly after promotion? | Maintainer | Before implementation |
| OQ-5 | Should the entity extraction phase (Phase 4 graph rebuild) re-process all surviving memories on every GC run, or maintain a `last_kg_sync_at` timestamp per entry and only process entries updated since the last GC? Incremental approach is faster but requires additional tracking. | Maintainer | Before implementation |
| OQ-6 | The `ENTITY_EXTRACTION_PROMPT` calls the LLM once per memory entry in the worst case. For a 500-entry profile, this is 500 LLM calls. Should entity extraction be batched (multiple entries per call) to reduce cost, at the expense of per-entry precision? | Maintainer | Before implementation |
| OQ-7 | Should `memory_archive` have a TTL (e.g., purge archived entries older than 365 days) to prevent indefinite growth? This would require a separate `tag memory gc --purge-archive` subcommand and explicit user acknowledgement. | Maintainer | Follow-up PRD |
| OQ-8 | The Leiden community detection algorithm (mentioned in research context) produces topic-cluster summaries that could be stored in a `kg_community_reports` table and surfaced via `tag memory topics`. Should this be included in this PRD's scope or deferred? Current recommendation: defer. | Maintainer | Before scope freeze |

---

## 16. Complexity and Timeline

### Phase 0 — Schema and Scaffolding (Day 1–2)

- Write `ensure_gc_schema()` with all new tables; test idempotency.
- Add `memory_tier TEXT DEFAULT 'recall'` column to `semantic_memories` via ALTER TABLE in `ensure_gc_schema()`.
- Add `cmd_memory_gc` stub to `controller.py` with argument parser.
- Write `test_ensure_gc_schema_idempotent`.

### Phase 1 — Expiration and Promotion (Day 3–4)

- Implement `_phase_expire()`: load entries, compute `effective_confidence`, move qualifying rows to `memory_archive`, write provenance snapshots.
- Implement `_phase_promote()`: update `memory_tier` for qualifying entries.
- Implement advisory lock acquire/release.
- Write unit tests for both phases; verify `--dry-run` produces no writes.

### Phase 2 — Merge Pipeline (Day 5–7)

- Implement `_embed_entries()` using `sentence-transformers`.
- Implement `_cluster_by_similarity()` (greedy O(N^2)).
- Implement `CONSOLIDATION_EXTRACTION_PROMPT` call and response parsing.
- Implement `RECONCILIATION_PROMPT` call, response validation, and database mutations with provenance records.
- Add cost estimation and `--max-cost` abort logic.
- Add secret redaction via PRD-034.
- Write unit tests for clustering, prompt parsing, and cost abort.

### Phase 3 — Knowledge Graph (Day 8–9)

- Write `memory_graph.py` with `kg_entities` + `kg_edges` schema helpers.
- Implement `_phase_graph()`: call `ENTITY_EXTRACTION_PROMPT` per memory entry (or batched), upsert entities and edges.
- Implement `kg_search()` for simple one-hop and two-hop graph traversal.
- Write unit tests for entity extraction parsing and graph upsert idempotency.

### Phase 4 — Rollback, Stats, and Cron Integration (Day 10–11)

- Implement `rollback_gc_run()`: restore `memory_gc_snapshots` to `semantic_memories`, delete GC-created KG rows.
- Implement `list_gc_runs()` and `_print_gc_stats_table()`.
- Implement `hermes_env()` patch to inject core-tier entries unconditionally.
- Write integration test for `tag cron add` → GC fire.
- Write rollback integration test.

### Phase 5 — Performance, Documentation, and Final Review (Day 12–14)

- Run performance tests; tune batch sizes and embedding chunk sizes if needed.
- Write `tag doctor` check: `SELECT 1 FROM pragma_compile_options WHERE compile_options='ENABLE_FTS5'` to verify FTS5 availability.
- Add `tag memory gc` to the INDEX.md entry for Cluster C.
- Final security review of LLM prompt injection surface (ensure memory content cannot break out of prompt delimiters).
- Fix any acceptance criterion gaps found during review.

**Total estimated effort: 12–14 days (M — fits within 2-week sprint)**
