# PRD-104: Node-Level Caching with TTL for Expensive LLM Calls (`tag cache node`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `tracing.py + cache_store.py` (new), `controller.py` (integration hooks), `tag.sqlite3` (new tables)
**Depends on:** PRD-013 (agent tracing/observability), PRD-027 (eval framework), PRD-028 (sandbox), PRD-030 (prompt cache analytics), PRD-034 (secret scanning / security), PRD-041 (OTel span cost attribution), PRD-048 (structured tool-call child spans)
**Inspired by:** LangGraph CachePolicy, GPTCache, semantic cache (Zilliz)
**GitHub issue:** #349

---

## 1. Overview

Every `tag run` that exercises an LLM node — a call to Hermes/Anthropic with a prompt, model, and temperature — carries a latency cost (often 2–15 seconds per call) and a direct dollar cost proportional to tokens consumed. When the same logical task is re-executed with the same inputs — developer retry after a transient error, a nightly cron job re-evaluating an unchanged document, a self-consistency ensemble calling the same prompt multiple times — those costs multiply without providing new information. TAG currently has no mechanism to detect these redundant calls and serve a cached response instead.

This PRD introduces `tag cache node`: a node-level response cache that stores LLM responses keyed on a deterministic hash of `(prompt_text, model_id, temperature, profile)`, with a configurable TTL controlling entry lifetime. The cache is implemented as a new SQLite-backed module `src/tag/cache_store.py`, stored in the existing WAL-mode database at `~/.tag/runtime/tag.sqlite3`, and integrated into the tracing layer (`tracing.py`) so that cache hits and misses are recorded as span attributes visible in `tag trace`. An optional Redis backend enables cross-process and cross-machine cache sharing for teams running distributed TAG agents.

Beyond exact-match caching, the feature offers a semantic cache mode: when enabled, incoming prompts are embedded using the same `SentenceTransformer` pipeline already present in `tool_retrieval.py`, and a cosine-similarity search over recent cache entries identifies semantically equivalent prompts whose responses can be reused. This extends cache utility to slight prompt variations (rephrased queries, minor context differences) without requiring byte-for-byte prompt identity. Semantic cache uses a configurable similarity threshold (default `0.85`) above which a hit is declared, analogous to the `θ=0.7` skill-retrieval threshold in TDAG and the `sentence-transformers` embedding pipeline already shipped in `tool_retrieval.py`.

Per-node cache policies give granular control: a profile can declare that its `summarize` node should cache aggressively (TTL 24 h) while its `execute_code` node should never cache (TTL 0, policy `bypass`). This mirrors LangGraph's `CachePolicy` attachment semantics, where each node in the computation graph independently declares its caching behavior. The integration with TAG's tracing layer means cache hits appear as zero-latency spans with a `cache.hit=true` attribute, giving cost and latency dashboards an accurate picture of effective vs. billed work.

The feature is additive and opt-in: when no cache policy is configured and the `cache.enabled` config key is `false` (the default), `cache_store.py` is never imported and no overhead is introduced. Enabling caching for a profile is a single CLI command. The security design avoids the pickle deserialization RCE vector identified in LangGraph's `SqliteCache` (GHSA-mhr3-j7m5-c7c9) by storing all cache values as JSON-serialized text, never as pickled bytes.

---

## 2. Problem Statement

### 2.1 Redundant LLM Calls Multiply Cost and Latency Linearly

TAG's self-consistency ensemble (PRD-101) samples the same prompt N=10 times at `temperature=0.7` to aggregate a majority-voted answer. Without caching, all 10 calls are billed independently. A single 2 000-token prompt at claude-sonnet-4-6 rates costs roughly $0.006 per call; 10 calls per ensemble iteration add up to $0.06 per query. Nightly CI eval jobs (PRD-027, PRD-047) repeatedly invoke the same profile against the same eval suite; if the underlying documents have not changed, every repeat call is pure waste. Developer retry loops — running `tag submit` twice after a partial failure — re-execute every completed node at full cost even when those nodes already produced correct, cacheable output.

### 2.2 No Mechanism for Intra-Run Deduplication

Within a single `tag swarm` run, multiple profile agents may independently formulate the same sub-query. For example, a `researcher` profile and a `summarizer` profile may both issue a "summarize this document" call against the same 10 000-token document text. TAG has no shared request registry, no deduplication layer, and no way for one agent to discover that another agent already produced an equivalent response 30 seconds earlier in the same run. Each agent pays full cost for its own call. This redundancy grows quadratically with agent fan-out.

### 2.3 Developer Experience Suffers from Long Retry Latency

When a developer iterates on a prompt — changing a few words of a system prompt while keeping the user message constant — `tag run` re-executes all nodes from scratch. Nodes whose inputs did not change (document retrieval, pre-processing summarization, fixed-context reasoning) are re-run at full latency. A 40-second run that could complete in 4 seconds with caching discourages rapid iteration. There is no way to mark individual nodes as safe-to-cache while leaving others (e.g., web search, code execution) as always-fresh.

---

## 3. Goals and Non-Goals

### 3.1 Goals

| ID | Goal |
|----|------|
| G1 | Provide an exact-match cache keyed on `SHA-256(prompt_text + model_id + str(temperature) + profile)` with configurable TTL per node and per profile. |
| G2 | Provide a semantic cache mode using `SentenceTransformer` embeddings (reusing `tool_retrieval.py`'s existing pipeline) with cosine-similarity threshold (default `0.85`). |
| G3 | Persist cache entries in `tag.sqlite3` (SQLite WAL) via a new `cache_entries` table using `open_db()`, with optional Redis backend for cross-process sharing. |
| G4 | Integrate with `tracing.py` so every cache hit and miss is recorded as a span attribute (`cache.hit`, `cache.key`, `cache.backend`, `cache.similarity_score`). |
| G5 | Expose per-node, per-profile cache policies in CLI config: `tag cache node enable`, `tag cache node disable`, `tag cache node policy set`. |
| G6 | Provide `tag cache node status --json`, `tag cache node stats --json`, and `tag cache node clear --older-than <duration>` management commands. |
| G7 | Zero performance overhead (no imports, no SQLite queries) when caching is disabled for a profile. |
| G8 | Avoid the pickle deserialization RCE vector: all stored values are JSON text, never pickled bytes. |
| G9 | Emit eviction on TTL expiry lazily (at read time) plus a periodic background sweep configurable via `cache.sweep_interval_hours`. |
| G10 | Cache hits reduce billed token counts: `tag cache node stats` reports `tokens_saved` and `usd_saved` based on model pricing tables from `budget.py`. |

### 3.2 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Caching tool call results (web search, code execution): those are non-deterministic by nature and should use purpose-specific caches. This PRD covers LLM inference calls only. |
| NG2 | Distributed cache invalidation protocols: when using Redis, TTL-based expiry is the sole invalidation mechanism. No pub/sub invalidation events. |
| NG3 | Cache warming: pre-populating the cache by issuing synthetic requests before real runs is not part of this PRD. |
| NG4 | Serving cache hits for streaming responses: streaming output is not cached; only complete, non-streaming completions are stored and replayed. |
| NG5 | Cross-user cache sharing: cache entries are scoped per-user (by filesystem path) even in Redis mode. No multi-tenant shared cache. |
| NG6 | Fine-grained cache invalidation triggered by profile edits: when a system prompt changes, cache entries for that profile should be manually cleared with `tag cache node clear --profile <name>`. No automatic invalidation. |
| NG7 | Semantic cache for non-text inputs (images, files): semantic similarity is computed over the text portion of the prompt only. |
| NG8 | Real-time monitoring dashboard for cache hit rates: stats are available via CLI commands; a live TUI panel is out of scope. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Cache hit latency | P50 < 5 ms for SQLite backend, < 2 ms for in-memory L1 | `pytest-benchmark` against `cache_store.get()` with 10 000 warm entries |
| Cache miss overhead | < 2 ms added to any LLM call path (i.e., `cache_store.get()` returns `None` in < 2 ms) | Benchmark against cold cache |
| Exact-match accuracy | 100% — no false cache hits for distinct `(prompt, model, temperature, profile)` tuples | Fuzz test with 10 000 distinct prompts, verify zero cross-contamination |
| Semantic cache precision | Cosine similarity threshold `0.85` produces < 1% false positive rate on a held-out eval suite | Manual spot-check on `evals/coding.yaml` cases with paraphrased prompts |
| Token savings accuracy | `tokens_saved` field matches actual re-run token count within ±5% | Compare cached stats vs. force-fresh re-run counts |
| Zero-overhead guarantee | `tag run` wall time with cache disabled is statistically identical to pre-feature wall time (t-test over 20 runs) | CI benchmark job |
| SQLite WAL safety | No data corruption after 100 concurrent read/write operations across 4 processes | `pytest` with `multiprocessing.Pool` |
| TTL correctness | Entries older than TTL are never returned; entries within TTL are always returned | Parametrized unit test with mocked clock |
| Redis fallback | When Redis is unreachable, falls back to SQLite without error propagation | Unit test with mocked `redis.Redis` raising `ConnectionError` |
| RCE safety | `cache_store.get()` never calls `pickle.loads()` on stored data | `grep -rn 'pickle.loads' src/tag/cache_store.py` returns empty |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer iterating on a prompt | enable node caching for my `coder` profile with `tag cache node enable --profile coder --ttl 3600` | Nodes whose inputs have not changed are served from cache instantly, reducing my iteration loop from 40 s to 4 s |
| U2 | Team platform engineer | run `tag cache node stats --json` | I can see total cache hits, misses, tokens saved, and USD saved across all profiles in a machine-readable format for dashboards |
| U3 | Developer running a nightly eval job | configure TTL 86400 for the `researcher` profile | The CI eval job re-uses LLM responses from previous runs when documents have not changed, cutting eval runtime from 20 min to 2 min |
| U4 | Developer debugging a cache miss | inspect `tag trace show <run_id>` | I see `cache.hit=false, cache.key=sha256:abc123` in the span attributes so I understand exactly why the cache was not used |
| U5 | Security-conscious developer | know that cached response data is stored as JSON, not pickled bytes | I am not exposed to the pickle deserialization RCE vector described in GHSA-mhr3-j7m5-c7c9 |
| U6 | Developer using semantic cache | enable semantic mode with `--semantic --similarity-threshold 0.85` | Rephrased versions of the same logical question reuse the prior response without requiring identical prompt text |
| U7 | Ops engineer with Redis cluster | configure `tag config set cache.redis_url redis://redis.internal:6379` | Cache entries are shared across multiple TAG agent processes on different machines |
| U8 | Developer cleaning up | run `tag cache node clear --older-than 24h` | Stale entries older than one day are purged, freeing disk space without deleting recent useful entries |
| U9 | Self-consistency pipeline author | configure a `bypass` policy on the `vote_aggregator` node | The aggregator node is never cached (it must always see all N sampled responses), while the upstream sampling nodes are cached for token efficiency |
| U10 | Developer running a cost report | run `tag cache node stats --profile coder --since 7d` | I see a breakdown of how many tokens and dollars were saved by the cache over the last week |

---

## 6. Proposed CLI Surface

All node cache commands live under the `tag cache node` namespace, distinct from the existing `tag cache` namespace in PRD-030 (which covers Anthropic prompt cache analytics).

### 6.1 `tag cache node enable`

Enable node-level LLM response caching for a profile.

```
tag cache node enable \
  --profile <name> \
  [--ttl <seconds>] \
  [--semantic] \
  [--similarity-threshold <float>] \
  [--backend sqlite|redis] \
  [--node <node_name>]
```

- `--profile <name>` (required): Profile to enable caching for. Must exist in `~/.tag/profiles/`.
- `--ttl <seconds>` (default: `3600`): Time-to-live in seconds. `0` means no expiry. `--ttl 0` is equivalent to permanent cache; use with caution.
- `--semantic`: Enable semantic similarity cache mode in addition to exact-match. Requires `sentence-transformers` installed.
- `--similarity-threshold <float>` (default: `0.85`): Cosine similarity threshold for semantic cache hits. Only used when `--semantic` is set. Valid range: `0.5`–`1.0`.
- `--backend sqlite|redis` (default: `sqlite`): Storage backend. `redis` requires `cache.redis_url` to be set in config.
- `--node <node_name>`: Apply policy only to a specific node name (e.g., `llm_call`, `summarize`). If omitted, the policy applies to all LLM nodes in the profile.

**Example:**
```
$ tag cache node enable --profile coder --ttl 3600
Cache enabled for profile 'coder' (backend: sqlite, ttl: 3600s, mode: exact-match)
Policy written to ~/.tag/profiles/coder.yaml [cache] section.

$ tag cache node enable --profile researcher --ttl 86400 --semantic --similarity-threshold 0.87
Cache enabled for profile 'researcher' (backend: sqlite, ttl: 86400s, mode: exact+semantic, threshold: 0.87)
```

### 6.2 `tag cache node disable`

Disable caching for a profile or node.

```
tag cache node disable --profile <name> [--node <node_name>]
```

- Sets `cache.policy: bypass` for the profile (or named node), causing all future calls to skip cache entirely.
- Does not delete existing cache entries. Use `tag cache node clear` to remove data.

**Example:**
```
$ tag cache node disable --profile coder
Cache disabled for profile 'coder'. Existing entries retained (use 'tag cache node clear --profile coder' to delete).
```

### 6.3 `tag cache node status`

Show current cache configuration and live statistics for all profiles or a specific one.

```
tag cache node status [--profile <name>] [--json]
```

**Human-readable output:**
```
TAG Node Cache Status
Profile       Backend  TTL    Mode         Entries  Hit Rate  USD Saved
------------------------------------------------------------------------
coder         sqlite   3600s  exact        1 247    73.4%     $2.14
researcher    sqlite   86400s exact+sem    342      81.2%     $8.71
writer        -        -      disabled     0        -         -
```

**JSON output (`--json`):**
```json
{
  "profiles": [
    {
      "profile": "coder",
      "enabled": true,
      "backend": "sqlite",
      "ttl_seconds": 3600,
      "mode": "exact",
      "similarity_threshold": null,
      "entry_count": 1247,
      "hit_rate": 0.734,
      "usd_saved": 2.14,
      "tokens_saved": 142800
    }
  ],
  "global": {
    "total_entries": 1589,
    "total_hit_rate": 0.751,
    "total_usd_saved": 10.85,
    "backend_path": "~/.tag/runtime/tag.sqlite3",
    "redis_url": null
  }
}
```

### 6.4 `tag cache node stats`

Detailed statistics with time-range filtering.

```
tag cache node stats \
  [--profile <name>] \
  [--since <duration>] \
  [--until <duration>] \
  [--model <model_id>] \
  [--json] \
  [--csv]
```

- `--since <duration>`: Filter to cache events after this relative duration (e.g., `7d`, `24h`, `30m`).
- `--until <duration>`: Filter to cache events before this relative duration.
- `--model <model_id>`: Filter by model (e.g., `claude-sonnet-4-6`).
- `--json`: Machine-readable output.
- `--csv`: CSV output for spreadsheet import.

**JSON output:**
```json
{
  "period": {"since": "2026-06-10T00:00:00Z", "until": "2026-06-17T00:00:00Z"},
  "profile": "coder",
  "exact_hits": 892,
  "exact_misses": 324,
  "semantic_hits": 31,
  "semantic_misses": 0,
  "total_requests": 1247,
  "hit_rate": 0.740,
  "tokens_saved": 142800,
  "usd_saved": 2.14,
  "avg_hit_latency_ms": 3.2,
  "avg_miss_latency_ms": 4180,
  "top_nodes": [
    {"node": "llm_call", "hits": 712, "misses": 201},
    {"node": "summarize", "hits": 180, "misses": 123}
  ],
  "daily_breakdown": [
    {"date": "2026-06-10", "hits": 145, "misses": 62, "usd_saved": 0.31},
    {"date": "2026-06-11", "hits": 133, "misses": 41, "usd_saved": 0.28}
  ]
}
```

### 6.5 `tag cache node clear`

Purge cache entries matching filter criteria.

```
tag cache node clear \
  [--profile <name>] \
  [--older-than <duration>] \
  [--node <node_name>] \
  [--model <model_id>] \
  [--all] \
  [--dry-run] \
  [--yes]
```

- `--older-than <duration>`: Delete entries older than this duration (e.g., `24h`, `7d`).
- `--all`: Delete all cache entries. Requires `--yes` confirmation.
- `--dry-run`: Print what would be deleted without deleting.
- `--yes`: Skip confirmation prompt.

**Example:**
```
$ tag cache node clear --older-than 24h
Scanning cache entries older than 24h...
  Would delete 847 entries (across profiles: coder: 612, researcher: 235)
  Estimated disk freed: 18.4 MB

Proceed? [y/N]: y
Deleted 847 entries. Disk freed: 18.4 MB.

$ tag cache node clear --older-than 24h --dry-run
[DRY RUN] Would delete 847 entries (18.4 MB). No changes made.
```

### 6.6 `tag cache node policy`

Fine-grained per-node policy management.

```
tag cache node policy set \
  --profile <name> \
  --node <node_name> \
  --ttl <seconds> \
  [--policy cache|bypass|refresh]

tag cache node policy list --profile <name> [--json]

tag cache node policy reset --profile <name> [--node <node_name>]
```

- `--policy cache`: Normal cache behavior (default).
- `--policy bypass`: Never read from or write to cache for this node.
- `--policy refresh`: Always write to cache (overwriting any existing entry) but never read from it. Useful for warming the cache with fresh responses.

**Example:**
```
$ tag cache node policy set --profile coder --node execute_code --policy bypass
Node 'execute_code' in profile 'coder': policy set to bypass.

$ tag cache node policy list --profile coder
Node             Policy   TTL     Notes
-----------------------------------------------
llm_call         cache    3600s   (profile default)
summarize        cache    7200s
execute_code     bypass   -       (never cached)
vote_aggregator  bypass   -       (never cached)
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `cache_store.py` must expose `get(key: CacheKey) -> CacheEntry | None` and `put(key: CacheKey, entry: CacheEntry) -> None` functions operating on the SQLite `cache_entries` table via `open_db()`. | P0 |
| FR-02 | The cache key must be computed as `SHA-256(prompt_text + "\x00" + model_id + "\x00" + str(temperature) + "\x00" + profile)` using `hashlib.sha256`, producing a 64-character hex digest. | P0 |
| FR-03 | `get()` must check the entry's `expires_at` timestamp against `datetime.now(UTC)` before returning; expired entries must return `None` and be lazily deleted. | P0 |
| FR-04 | `put()` must compute `expires_at = now + timedelta(seconds=ttl)` for `ttl > 0`; for `ttl == 0`, `expires_at` must be `NULL` (permanent). | P0 |
| FR-05 | Cache response values must be stored as JSON text (`response_json TEXT NOT NULL`) using `json.dumps()`. The module must never call `pickle.dumps()` or `pickle.loads()`. | P0 (security) |
| FR-06 | `tracing.py` span close must annotate the span with `cache.hit: bool`, `cache.key: str`, `cache.backend: str`, `cache.similarity_score: float | null` when a cache lookup is performed. | P0 |
| FR-07 | When `cache.policy` for a profile or node is `bypass`, `cache_store.get()` and `cache_store.put()` must be entirely skipped; no SQL queries may be issued. | P0 |
| FR-08 | Semantic cache mode must embed the prompt using the `SentenceTransformer` model configured in `tool_retrieval.py` (`all-MiniLM-L6-v2` by default), compute cosine similarity against all non-expired entries for the same `model_id` and `profile`, and return the entry with the highest similarity if it exceeds `similarity_threshold`. | P1 |
| FR-09 | Semantic cache must store the prompt embedding as a BLOB in `cache_entries.prompt_embedding` (serialized via `numpy.ndarray.tobytes()` + dtype/shape metadata in `embedding_meta_json`). | P1 |
| FR-10 | When Redis backend is configured and reachable, `cache_store.get()` must first query Redis (L1), then fall back to SQLite (L2) on a miss, and promote the SQLite hit to Redis. When Redis is unreachable, must fall back silently to SQLite-only. | P1 |
| FR-11 | `tag cache node clear --older-than <duration>` must parse duration strings in the format `<N>h`, `<N>d`, `<N>m` (minutes), `<N>w` (weeks) and delete matching rows using `DELETE FROM cache_entries WHERE expires_at < ?`. | P1 |
| FR-12 | `tag cache node stats` must compute `usd_saved` using the per-model pricing table from `budget.py` as `tokens_saved_input * input_price_per_token + tokens_saved_output * output_price_per_token`. | P1 |
| FR-13 | `tag cache node enable` must write a `[cache]` section to the target profile's YAML file: `enabled: true`, `ttl: <N>`, `backend: sqlite|redis`, `mode: exact|semantic`, `similarity_threshold: <float>`. | P1 |
| FR-14 | `cache_store.py` must record each cache access event (hit/miss, key, profile, node, latency_ms, tokens_prompt, tokens_completion) into the `cache_events` table for aggregation by `stats`. | P1 |
| FR-15 | A background sweep deletes all expired entries when `cache.sweep_interval_hours` elapses since the last sweep. Sweep timestamp is stored in `cache_meta` key-value table. Sweep runs synchronously before the first cache operation after the interval elapses. | P2 |
| FR-16 | `tag cache node status` must compute per-profile hit rate as `hits / (hits + misses)` from the `cache_events` table, applying a 30-day rolling window by default. | P1 |
| FR-17 | The `key_func` used for cache key computation must exclude fields that should not affect caching semantics: message IDs, timestamps embedded in tool call metadata, and run-specific trace IDs. | P0 |
| FR-18 | When `--node <node_name>` is provided, per-node policy takes precedence over the profile-level policy using a precedence chain: node policy > profile policy > global default (disabled). | P1 |
| FR-19 | `tag cache node stats --csv` must output valid RFC 4180 CSV with a header row. | P2 |
| FR-20 | Cache entries must not exceed `cache.max_entry_size_kb` (default: `512` KB). Entries larger than this limit are silently not cached; a warning is logged to `tracing.py` span attributes as `cache.skip_reason: "entry_too_large"`. | P2 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Cache `get()` P99 latency (SQLite, warm, 10 000 entries) | < 10 ms |
| NFR-02 | Cache `get()` P99 latency (Redis, warm) | < 3 ms |
| NFR-03 | Cache `put()` P99 latency (SQLite, WAL mode) | < 20 ms |
| NFR-04 | Memory overhead of `cache_store.py` module import | < 2 MB RSS additional |
| NFR-05 | Module import time when `cache.enabled = false` | 0 ms (module not imported) |
| NFR-06 | SQLite `cache_entries` table must use WAL journal mode consistent with the rest of `tag.sqlite3` | Enforced via existing `open_db()` PRAGMA sequence |
| NFR-07 | Cache entries must survive process restart (SQLite persistence) | 100% for entries within TTL |
| NFR-08 | No cache data must be written to log files, stdout, or error messages (responses may be confidential) | Code review + `grep` assertion in CI |
| NFR-09 | Semantic cache embedding computation must not block the main thread for > 200 ms; run in `asyncio.run_in_executor()` when called from async context | Enforced in implementation |
| NFR-10 | `tag cache node clear --all` must complete within 5 seconds for a database with 100 000 entries | Verified by performance test |
| NFR-11 | Redis connection pool must be bounded to max 5 connections (configurable via `cache.redis_pool_size`) | Configurable default |
| NFR-12 | All SQL in `cache_store.py` must use parameterized queries; no string interpolation of user-controlled values | Static analysis + code review |
| NFR-13 | The `cache_entries` table must include indexes on `(profile, model_id, expires_at)` and `(cache_key)` to ensure sub-10ms lookups at 100 000 entries | DDL enforced |
| NFR-14 | `cache_store.py` must be fully testable without a running Redis or real LLM by accepting injected connection objects | Dependency injection pattern |

---

## 9. Technical Design

### 9.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/cache_store.py` | Core cache implementation: exact-match lookup, semantic lookup, put, clear, stats, sweep |
| `tests/test_cache_store.py` | Unit tests for cache_store (mocked DB, mocked Redis, mocked embeddings) |
| `tests/test_cache_integration.py` | Integration tests against real SQLite via `open_db()` |

Existing files modified:

| File | Change |
|------|--------|
| `src/tag/tracing.py` | Extend `Span.attributes` population in `close_span()` to include `cache.*` keys when a cache lookup was performed |
| `src/tag/controller.py` | Wire `cache_store.get()` / `cache_store.put()` into the LLM call path in `run_chat_step()`; add `cmd_cache_node_*` command handlers; add `_migrate_prd_104_tables()` migration |
| `src/tag/budget.py` | Export `get_model_pricing(model_id: str) -> ModelPricing` for use by `cache_store.compute_usd_saved()` |

### 9.2 SQLite DDL

```sql
-- Migration: PRD-104
-- Applied in _migrate_prd_104_tables(conn)

CREATE TABLE IF NOT EXISTS cache_entries (
    cache_key           TEXT NOT NULL,          -- SHA-256 hex of (prompt+model+temp+profile)
    profile             TEXT NOT NULL,
    node_name           TEXT NOT NULL DEFAULT 'llm_call',
    model_id            TEXT NOT NULL,
    temperature         REAL NOT NULL,
    prompt_hash         TEXT NOT NULL,          -- SHA-256 of prompt_text alone (for semantic lookup)
    prompt_embedding    BLOB,                   -- numpy ndarray bytes; NULL if semantic mode disabled
    embedding_meta_json TEXT NOT NULL DEFAULT '{}',  -- {"dtype":"float32","shape":[384]}
    response_json       TEXT NOT NULL,          -- JSON-serialised LLM response (never pickle)
    prompt_tokens       INTEGER NOT NULL DEFAULT 0,
    completion_tokens   INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL,          -- ISO-8601 UTC
    expires_at          TEXT,                   -- ISO-8601 UTC; NULL = permanent
    hit_count           INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (cache_key, profile)
);

CREATE INDEX IF NOT EXISTS idx_cache_entries_profile_model_expires
    ON cache_entries (profile, model_id, expires_at);

CREATE INDEX IF NOT EXISTS idx_cache_entries_key
    ON cache_entries (cache_key);

CREATE TABLE IF NOT EXISTS cache_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at     TEXT NOT NULL,              -- ISO-8601 UTC
    profile         TEXT NOT NULL,
    node_name       TEXT NOT NULL,
    model_id        TEXT NOT NULL,
    cache_key       TEXT NOT NULL,
    event_type      TEXT NOT NULL,              -- 'hit_exact' | 'hit_semantic' | 'miss' | 'put' | 'evict'
    similarity      REAL,                       -- NULL for exact hits and misses
    latency_ms      REAL NOT NULL DEFAULT 0,
    tokens_prompt   INTEGER NOT NULL DEFAULT 0,
    tokens_completion INTEGER NOT NULL DEFAULT 0,
    run_id          TEXT,                       -- FK to runs.id (nullable, for correlation)
    span_id         TEXT                        -- FK to spans.id (nullable)
);

CREATE INDEX IF NOT EXISTS idx_cache_events_profile_occurred
    ON cache_events (profile, occurred_at);

CREATE TABLE IF NOT EXISTS cache_policies (
    profile         TEXT NOT NULL,
    node_name       TEXT NOT NULL DEFAULT '__profile__',  -- '__profile__' = profile-level default
    policy          TEXT NOT NULL DEFAULT 'cache',        -- 'cache' | 'bypass' | 'refresh'
    ttl_seconds     INTEGER,                              -- NULL = inherit profile/global default
    backend         TEXT NOT NULL DEFAULT 'sqlite',       -- 'sqlite' | 'redis'
    mode            TEXT NOT NULL DEFAULT 'exact',        -- 'exact' | 'semantic'
    similarity_threshold REAL NOT NULL DEFAULT 0.85,
    enabled         INTEGER NOT NULL DEFAULT 1,           -- 0 = disabled
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (profile, node_name)
);

CREATE TABLE IF NOT EXISTS cache_meta (
    key             TEXT PRIMARY KEY,
    value           TEXT NOT NULL
);
-- e.g., INSERT OR REPLACE INTO cache_meta VALUES ('last_sweep_at', '2026-06-17T00:00:00Z')
```

### 9.3 Core Python Dataclasses

```python
# src/tag/cache_store.py

from __future__ import annotations

import hashlib
import json
import sqlite3
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any

# ──────────────────────────────────────────────
# Domain types
# ──────────────────────────────────────────────

@dataclass(frozen=True)
class CacheKey:
    """Immutable value object representing the lookup key for one LLM call."""
    prompt_text: str
    model_id: str
    temperature: float
    profile: str
    node_name: str = "llm_call"

    def digest(self) -> str:
        """Return a 64-character SHA-256 hex digest.

        Fields are joined with the NULL byte separator to prevent
        length-extension collisions between adjacent fields.
        Components are normalised: temperature is rounded to 4 decimal
        places to absorb float representation noise.
        """
        canonical = "\x00".join([
            self.prompt_text,
            self.model_id,
            f"{self.temperature:.4f}",
            self.profile,
        ])
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()

    def prompt_digest(self) -> str:
        """SHA-256 of the prompt_text alone (for semantic candidate filtering)."""
        return hashlib.sha256(self.prompt_text.encode("utf-8")).hexdigest()


@dataclass
class CacheEntry:
    """One cached LLM response."""
    cache_key: str                        # CacheKey.digest()
    profile: str
    node_name: str
    model_id: str
    temperature: float
    response: dict[str, Any]              # Deserialised JSON of the LLM response
    prompt_tokens: int = 0
    completion_tokens: int = 0
    created_at: str = field(default_factory=lambda: datetime.now(timezone.utc).isoformat())
    expires_at: str | None = None         # None = permanent
    hit_count: int = 0
    similarity: float | None = None       # Set on semantic hits


@dataclass
class CachePolicy:
    """Per-profile or per-node cache policy, read from cache_policies table."""
    profile: str
    node_name: str = "__profile__"
    policy: str = "cache"                 # 'cache' | 'bypass' | 'refresh'
    ttl_seconds: int | None = 3600
    backend: str = "sqlite"              # 'sqlite' | 'redis'
    mode: str = "exact"                  # 'exact' | 'semantic'
    similarity_threshold: float = 0.85
    enabled: bool = True


@dataclass
class CacheStats:
    """Aggregated statistics returned by tag cache node stats."""
    profile: str | None
    since: str | None
    until: str | None
    exact_hits: int = 0
    exact_misses: int = 0
    semantic_hits: int = 0
    total_requests: int = 0
    tokens_saved_prompt: int = 0
    tokens_saved_completion: int = 0
    usd_saved: float = 0.0
    avg_hit_latency_ms: float = 0.0
    avg_miss_latency_ms: float = 0.0
    entry_count: int = 0

    @property
    def hit_rate(self) -> float:
        total = self.exact_hits + self.exact_misses + self.semantic_hits
        return (self.exact_hits + self.semantic_hits) / total if total else 0.0
```

### 9.4 Cache Lookup Algorithm

```python
# src/tag/cache_store.py  (continued)

MAX_ENTRY_SIZE_BYTES = 512 * 1024  # 512 KB default


def get(
    conn: sqlite3.Connection,
    key: CacheKey,
    policy: CachePolicy,
    redis_client=None,        # Optional[redis.Redis]
    embed_fn=None,            # Optional[Callable[[str], np.ndarray]]
) -> CacheEntry | None:
    """
    Two-tier cache lookup:
      1. Redis L1 (if configured and reachable)  — O(1) network round-trip
      2. SQLite L2                                — O(log N) B-tree index scan

    Within SQLite, two modes:
      a. Exact match: WHERE cache_key = ? AND (expires_at IS NULL OR expires_at > ?)
      b. Semantic match (if policy.mode == 'semantic' and embed_fn provided):
           - Embed the query prompt
           - SELECT all non-expired (cache_key, prompt_embedding, ...) WHERE profile = ? AND model_id = ?
           - Cosine similarity against each; return best if >= similarity_threshold

    Returns None on miss; caller proceeds with live LLM call.
    """
    now = datetime.now(timezone.utc).isoformat()
    digest = key.digest()

    # ── L1: Redis ──────────────────────────────────────────────
    if redis_client is not None:
        try:
            raw = redis_client.get(f"tag:cache:{digest}")
            if raw is not None:
                data = json.loads(raw)
                entry = CacheEntry(**data)
                if entry.expires_at is None or entry.expires_at > now:
                    _record_event(conn, key, "hit_exact", 0, entry, similarity=None)
                    return entry
        except Exception:
            pass  # Redis unreachable: fall through to SQLite

    # ── L2a: SQLite exact match ─────────────────────────────────
    row = conn.execute(
        """
        SELECT * FROM cache_entries
        WHERE cache_key = ?
          AND profile    = ?
          AND (expires_at IS NULL OR expires_at > ?)
        LIMIT 1
        """,
        (digest, key.profile, now),
    ).fetchone()

    if row is not None:
        # Lazy eviction: bump hit_count
        conn.execute(
            "UPDATE cache_entries SET hit_count = hit_count + 1 WHERE cache_key = ? AND profile = ?",
            (digest, key.profile),
        )
        conn.commit()
        entry = _row_to_entry(row)
        if redis_client is not None:
            _promote_to_redis(redis_client, entry)
        _record_event(conn, key, "hit_exact", 0, entry, similarity=None)
        return entry

    # ── L2b: SQLite semantic match (optional) ────────────────────
    if policy.mode == "semantic" and embed_fn is not None:
        import numpy as np
        query_vec = embed_fn(key.prompt_text)  # shape: (D,)
        rows = conn.execute(
            """
            SELECT cache_key, profile, prompt_embedding, embedding_meta_json,
                   response_json, prompt_tokens, completion_tokens,
                   created_at, expires_at, hit_count, model_id, temperature, node_name
            FROM cache_entries
            WHERE profile  = ?
              AND model_id = ?
              AND prompt_embedding IS NOT NULL
              AND (expires_at IS NULL OR expires_at > ?)
            """,
            (key.profile, key.model_id, now),
        ).fetchall()

        best_sim = -1.0
        best_row = None
        for r in rows:
            meta = json.loads(r["embedding_meta_json"])
            dtype = np.dtype(meta["dtype"])
            shape = tuple(meta["shape"])
            vec = np.frombuffer(r["prompt_embedding"], dtype=dtype).reshape(shape)
            # Cosine similarity (both vectors are L2-normalised at storage time)
            sim = float(np.dot(query_vec, vec))
            if sim > best_sim:
                best_sim = sim
                best_row = r

        if best_row is not None and best_sim >= policy.similarity_threshold:
            entry = _row_to_entry(best_row)
            entry.similarity = best_sim
            _record_event(conn, key, "hit_semantic", 0, entry, similarity=best_sim)
            return entry

    return None


def put(
    conn: sqlite3.Connection,
    key: CacheKey,
    response: dict[str, Any],
    policy: CachePolicy,
    prompt_tokens: int = 0,
    completion_tokens: int = 0,
    redis_client=None,
    embed_fn=None,
) -> None:
    """
    Store an LLM response in the cache.

    Guards:
    - Skip if policy.policy == 'bypass'.
    - Skip if JSON-serialised response exceeds MAX_ENTRY_SIZE_BYTES.
    - Always use json.dumps(); never pickle.
    """
    if policy.policy == "bypass":
        return

    now = datetime.now(timezone.utc).isoformat()
    expires_at: str | None = None
    if policy.ttl_seconds and policy.ttl_seconds > 0:
        expires_at = (
            datetime.now(timezone.utc) + timedelta(seconds=policy.ttl_seconds)
        ).isoformat()

    response_json = json.dumps(response, ensure_ascii=False)
    if len(response_json.encode("utf-8")) > MAX_ENTRY_SIZE_BYTES:
        # Entry too large; record skip reason in calling span attributes externally
        return

    digest = key.digest()
    prompt_digest = key.prompt_digest()

    embedding_blob: bytes | None = None
    embedding_meta: dict = {}
    if policy.mode == "semantic" and embed_fn is not None:
        import numpy as np
        vec = embed_fn(key.prompt_text)
        # L2-normalise before storage so dot product == cosine similarity
        norm = np.linalg.norm(vec)
        if norm > 0:
            vec = vec / norm
        embedding_blob = vec.astype(np.float32).tobytes()
        embedding_meta = {"dtype": "float32", "shape": list(vec.shape)}

    conn.execute(
        """
        INSERT OR REPLACE INTO cache_entries
          (cache_key, profile, node_name, model_id, temperature,
           prompt_hash, prompt_embedding, embedding_meta_json,
           response_json, prompt_tokens, completion_tokens,
           created_at, expires_at, hit_count)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
        """,
        (
            digest, key.profile, key.node_name, key.model_id,
            round(key.temperature, 4), prompt_digest,
            embedding_blob, json.dumps(embedding_meta),
            response_json, prompt_tokens, completion_tokens,
            now, expires_at,
        ),
    )
    conn.commit()

    if redis_client is not None:
        try:
            entry = CacheEntry(
                cache_key=digest, profile=key.profile, node_name=key.node_name,
                model_id=key.model_id, temperature=key.temperature,
                response=response, prompt_tokens=prompt_tokens,
                completion_tokens=completion_tokens,
                created_at=now, expires_at=expires_at,
            )
            _promote_to_redis(redis_client, entry, policy.ttl_seconds)
        except Exception:
            pass  # Redis write failure is non-fatal
```

### 9.5 Integration with `tracing.py`

The existing `close_span()` function in `tracing.py` accepts an `attributes: dict` parameter. The LLM call wrapper in `controller.py` must populate cache metadata before closing the span:

```python
# In controller.py run_chat_step() — illustrative integration sketch

from tag.cache_store import CacheKey, get as cache_get, put as cache_put
from tag import cache_store

cache_key = CacheKey(
    prompt_text=canonical_prompt,
    model_id=model_id,
    temperature=temperature,
    profile=profile_name,
    node_name=node_name,
)
policy = cache_store.load_policy(db, profile_name, node_name)

t0 = time.monotonic()
cached = cache_get(conn=db, key=cache_key, policy=policy, redis_client=_redis)
cache_latency_ms = (time.monotonic() - t0) * 1000

if cached is not None:
    # Cache hit: construct span as if LLM responded instantly
    span = open_span(trace_id, f"{node_name}:cache_hit", profile=profile_name, model_id=model_id)
    close_span(
        span,
        status="ok",
        prompt_tokens=0,   # not billed
        completion_tokens=0,
    )
    span.attributes.update({
        "cache.hit": True,
        "cache.key": cache_key.digest(),
        "cache.backend": policy.backend,
        "cache.similarity_score": cached.similarity,
        "cache.hit_type": "semantic" if cached.similarity is not None else "exact",
        "cache.lookup_latency_ms": round(cache_latency_ms, 2),
    })
    return cached.response

# Cache miss: call LLM normally
span = open_span(trace_id, node_name, profile=profile_name, model_id=model_id)
response = _call_hermes(canonical_prompt, model_id, temperature)
close_span(span, status="ok", prompt_tokens=pt, completion_tokens=ct)
span.attributes.update({
    "cache.hit": False,
    "cache.key": cache_key.digest(),
    "cache.backend": policy.backend,
    "cache.lookup_latency_ms": round(cache_latency_ms, 2),
})
# Store in cache
cache_put(conn=db, key=cache_key, response=response, policy=policy,
          prompt_tokens=pt, completion_tokens=ct, redis_client=_redis)
return response
```

### 9.6 Duration Parsing Utility

```python
# src/tag/cache_store.py

import re
from datetime import timedelta

_DURATION_RE = re.compile(r"^(\d+)(m|h|d|w)$")

def parse_duration(s: str) -> timedelta:
    """Parse '30m', '24h', '7d', '2w' into a timedelta.

    Raises ValueError on unrecognised format.
    """
    m = _DURATION_RE.match(s.strip().lower())
    if not m:
        raise ValueError(
            f"Unrecognised duration '{s}'. "
            "Expected format: <N>m (minutes), <N>h (hours), <N>d (days), <N>w (weeks)."
        )
    n = int(m.group(1))
    unit = m.group(2)
    multipliers = {"m": 60, "h": 3600, "d": 86400, "w": 604800}
    return timedelta(seconds=n * multipliers[unit])
```

### 9.7 USD Savings Computation

```python
# src/tag/cache_store.py

def compute_usd_saved(
    stats_rows: list[sqlite3.Row],
    get_pricing,   # budget.get_model_pricing
) -> float:
    """Sum USD saved across all cache hit events.

    For each 'hit_exact' or 'hit_semantic' event, the saved cost is:
        tokens_prompt * input_price_per_token
      + tokens_completion * output_price_per_token

    where pricing comes from budget.py's model pricing table.
    Rows with unknown model_ids are skipped (no pricing data).
    """
    total = 0.0
    for row in stats_rows:
        pricing = get_pricing(row["model_id"])
        if pricing is None:
            continue
        total += (
            row["tokens_prompt"] * pricing.input_per_token
            + row["tokens_completion"] * pricing.output_per_token
        )
    return round(total, 6)
```

### 9.8 Redis Backend

Redis is accessed via the `redis` optional dependency (`pip install tag[redis]`). The client is lazily initialised on first use:

```python
# src/tag/cache_store.py

_redis_client = None  # module-level singleton

def _get_redis(cfg: dict) -> "redis.Redis | None":
    global _redis_client
    if _redis_client is not None:
        return _redis_client
    redis_url = cfg.get("cache", {}).get("redis_url")
    if not redis_url:
        return None
    try:
        import redis as redis_lib
        pool = redis_lib.ConnectionPool.from_url(
            redis_url,
            max_connections=cfg.get("cache", {}).get("redis_pool_size", 5),
            socket_connect_timeout=0.5,
            socket_timeout=0.5,
        )
        _redis_client = redis_lib.Redis(connection_pool=pool)
        _redis_client.ping()   # Fail fast on bad URL
        return _redis_client
    except Exception:
        return None  # Redis unavailable; fall back to SQLite silently


def _promote_to_redis(client, entry: CacheEntry, ttl_seconds: int | None = None) -> None:
    redis_key = f"tag:cache:{entry.cache_key}"
    data = json.dumps({
        "cache_key": entry.cache_key,
        "profile": entry.profile,
        "node_name": entry.node_name,
        "model_id": entry.model_id,
        "temperature": entry.temperature,
        "response": entry.response,
        "prompt_tokens": entry.prompt_tokens,
        "completion_tokens": entry.completion_tokens,
        "created_at": entry.created_at,
        "expires_at": entry.expires_at,
        "hit_count": entry.hit_count,
    })
    if ttl_seconds and ttl_seconds > 0:
        client.setex(redis_key, ttl_seconds, data)
    else:
        client.set(redis_key, data)
```

### 9.9 Migration Hook in `controller.py`

```python
def _migrate_prd_104_tables(conn: sqlite3.Connection) -> None:
    """Create cache_entries, cache_events, cache_policies, cache_meta tables (PRD-104)."""
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS cache_entries ( ... );  -- full DDL from 9.2
        CREATE TABLE IF NOT EXISTS cache_events  ( ... );
        CREATE TABLE IF NOT EXISTS cache_policies( ... );
        CREATE TABLE IF NOT EXISTS cache_meta    ( ... );
        -- indexes
        CREATE INDEX IF NOT EXISTS idx_cache_entries_profile_model_expires
            ON cache_entries (profile, model_id, expires_at);
        CREATE INDEX IF NOT EXISTS idx_cache_entries_key
            ON cache_entries (cache_key);
        CREATE INDEX IF NOT EXISTS idx_cache_events_profile_occurred
            ON cache_events (profile, occurred_at);
    """)
    conn.commit()
```

This function is called from the existing migration chain in `open_db()` after `_migrate_prd_033_044_tables()`.

---

## 10. Security Considerations

1. **No pickle serialisation.** All cache values are stored as JSON text using `json.dumps()`. The module must never call `pickle.dumps()` or `pickle.loads()`. This directly mitigates the LangGraph CachePolicy RCE vector documented in GHSA-mhr3-j7m5-c7c9, where deserialising attacker-controlled pickle bytes from a cache backend grants arbitrary code execution. CI must include a `grep -rn 'pickle' src/tag/cache_store.py` assertion that returns empty.

2. **Parameterised SQL only.** Every SQL statement in `cache_store.py` must use `?` placeholders. No f-string or `%`-style interpolation of user-supplied values (profile names, node names, model IDs, cache keys) is permitted. Violation catches attacker-controlled profile names that include SQL fragments.

3. **Cache key collision resistance.** SHA-256 provides 256 bits of preimage resistance. The NULL-byte separator between fields prevents length-extension collisions (e.g., `prompt="ab", model="c"` vs. `prompt="a", model="bc"`). The `profile` field is included in the key to prevent cross-profile cache poisoning: an attacker who can write a cache entry for profile A cannot influence cache reads for profile B.

4. **Redis key namespace isolation.** All Redis keys are prefixed with `tag:cache:` followed by the SHA-256 digest. No user-controlled string is used directly as a Redis key. The Redis URL must come from the TAG config file (owned by the local user), not from environment variables or CLI flags, to prevent injection via compromised environment.

5. **Response confidentiality.** Cache entries may contain sensitive LLM responses (code, private documents, business logic). The SQLite database file is at `~/.tag/runtime/tag.sqlite3` with filesystem permissions `0600` (user-only read/write). `ensure_runtime_dirs()` must verify these permissions and warn if the file is world-readable. No cache data may be written to stdout, log files, or error messages — even in debug mode.

6. **Redis authentication.** When `cache.redis_url` includes a password (`redis://:password@host:port/0`), the URL is stored in the TAG config file (user-owned, `0600`). `tag config get cache.redis_url` must mask the password component, showing `redis://:****@host:port/0`. This follows the existing masking pattern for `agentops.api_key` in PRD-044.

7. **Prompt embedding privacy.** In semantic cache mode, `SentenceTransformer` embeddings are stored as BLOBs in SQLite. Embeddings are not directly reversible to the original prompt text, but they are proximity-preserving and could leak information about prompt similarity patterns. Users operating in high-sensitivity environments should disable semantic mode (`--mode exact`) or use `tag cache node clear` periodically.

8. **Cache poisoning via `--policy refresh`.** The `refresh` policy writes a fresh response to the cache, overwriting any existing entry. This could be abused by a process that injects a known-bad response. The `refresh` policy should only be configurable by the local user (no remote policy injection path). Future multi-tenant extensions must re-evaluate this.

9. **TTL bypass via clock skew.** Expiry comparison uses `datetime.now(timezone.utc)` on the reading machine. In distributed Redis mode, clock skew between writer and reader machines could cause premature or late expiry. Redis TTL (`SETEX`) uses the Redis server clock, which is authoritative; the `expires_at` TEXT field in SQLite is advisory for the SQLite-only path. Document this limitation.

10. **Secret scanning integration.** PRD-034 (secret scanning) should be extended to scan cache entries for accidentally cached API keys or tokens. A future integration can run `security.py`'s pattern scanner against `response_json` at `put()` time and emit a warning span attribute `cache.secret_detected=true` without blocking the cache write. This is a recommendation for a follow-on PRD rather than a blocking requirement here.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_cache_store.py`)

All tests use an in-memory SQLite connection (`sqlite3.connect(":memory:")`). Redis is mocked with `unittest.mock.MagicMock`.

| Test | Description |
|------|-------------|
| `test_exact_hit` | Put an entry; get with identical key; assert entry returned. |
| `test_exact_miss` | Get with a key never put; assert `None` returned. |
| `test_ttl_expired` | Put with TTL=1s; mock clock to T+2s; assert `get()` returns `None`. |
| `test_ttl_permanent` | Put with TTL=0 (permanent); advance clock by 1 year; assert entry still returned. |
| `test_bypass_policy_skips_get` | Set policy to `bypass`; assert `get()` returns `None` without issuing any SQL. |
| `test_bypass_policy_skips_put` | Set policy to `bypass`; call `put()`; assert `cache_entries` table remains empty. |
| `test_key_collision_resistance` | Two keys differing only in field boundary (length extension); assert different digests. |
| `test_entry_too_large_skipped` | Generate a 600 KB response JSON; assert `put()` does not insert a row. |
| `test_hit_count_incremented` | Get the same entry twice; assert `hit_count` becomes 2. |
| `test_redis_l1_hit` | Mock Redis `.get()` to return a valid serialised entry; assert `get()` returns it without querying SQLite. |
| `test_redis_fallback_on_connection_error` | Mock Redis `.get()` to raise `ConnectionError`; assert `get()` falls back to SQLite without raising. |
| `test_no_pickle_usage` | Inspect `cache_store` module source for `pickle` imports; assert not found. |
| `test_parse_duration_valid` | Assert `parse_duration("24h") == timedelta(hours=24)` etc. for all units. |
| `test_parse_duration_invalid` | Assert `parse_duration("5x")` raises `ValueError`. |
| `test_semantic_hit_above_threshold` | Mock `embed_fn` to return a fixed vector; put entry; call `get()` with a cosine-similar prompt; assert hit. |
| `test_semantic_miss_below_threshold` | Mock `embed_fn` to return an orthogonal vector; assert `get()` returns `None`. |
| `test_usd_saved_computation` | Feed known hit events with known token counts and mocked model pricing; assert `compute_usd_saved` returns correct value. |
| `test_sql_parameterisation` | Inject a profile name containing SQL fragment `'; DROP TABLE cache_entries; --`; assert table survives. |

### 11.2 Integration Tests (`tests/test_cache_integration.py`)

Use a real temporary SQLite file via `tmpdir` fixture and the actual `open_db()` function.

| Test | Description |
|------|-------------|
| `test_put_get_roundtrip_sqlite` | Full roundtrip with real DB; verify response JSON deserialises correctly. |
| `test_migration_idempotent` | Call `_migrate_prd_104_tables()` twice on the same connection; assert no error. |
| `test_concurrent_writes` | `multiprocessing.Pool(4)` each calling `put()` 250 times; assert 1 000 rows, no corruption. |
| `test_clear_older_than` | Insert 100 entries with varying `created_at`; call `clear(older_than=timedelta(hours=24))`; assert correct subset deleted. |
| `test_stats_aggregation` | Insert known hit/miss events; call `compute_stats()`; assert hit_rate, tokens_saved, usd_saved match expectations. |
| `test_sweep_deletes_expired` | Insert 50 expired entries; trigger sweep; assert all 50 deleted. |
| `test_cache_event_written_on_hit` | After a cache hit, assert a row exists in `cache_events` with `event_type='hit_exact'`. |
| `test_policy_precedence` | Set profile-level policy to `cache` and node-level policy to `bypass`; assert node-level takes precedence. |

### 11.3 Performance Tests

```python
# tests/test_cache_performance.py
# Run with: pytest tests/test_cache_performance.py --benchmark-only

import pytest

@pytest.mark.benchmark(group="cache_get_exact")
def test_get_warm_10k_entries(benchmark, warm_cache_db):
    """P99 of exact-match get() across 10 000 entries must be < 10 ms."""
    key = make_key(prompt="test prompt", model="claude-sonnet-4-6", temperature=0.7, profile="coder")
    result = benchmark(cache_get, conn=warm_cache_db, key=key, policy=default_policy())
    assert result is not None

@pytest.mark.benchmark(group="cache_put")
def test_put_latency(benchmark, empty_cache_db):
    """P99 of put() must be < 20 ms on WAL-mode SQLite."""
    benchmark(cache_put, conn=empty_cache_db, key=random_key(), response=sample_response(), policy=default_policy())
```

---

## 12. Acceptance Criteria

| ID | Criterion | Test Method |
|----|-----------|-------------|
| AC-01 | `tag cache node enable --profile coder --ttl 3600` writes `cache.enabled=true`, `cache.ttl=3600` to `coder.yaml` and inserts a row in `cache_policies` | Manual + `test_enable_writes_policy` |
| AC-02 | A second `tag run` with identical prompt/model/temperature on a cache-enabled profile returns without issuing an Anthropic API call (verifiable by `--dry-run` or mocked Hermes) | Integration test with mocked Hermes |
| AC-03 | `tag trace show <run_id>` for a cache-hit run displays `cache.hit: true` and `cache.key: sha256:<hex>` in span attributes | Integration test |
| AC-04 | `tag cache node stats --json` output includes `hit_rate`, `tokens_saved`, `usd_saved` fields with correct types | `test_stats_aggregation` |
| AC-05 | `tag cache node clear --older-than 24h` deletes all entries with `expires_at < now - 24h` without deleting newer entries | `test_clear_older_than` |
| AC-06 | `tag cache node clear --older-than 24h --dry-run` prints the count of entries that would be deleted without modifying the database | Unit test with assertion on row count pre/post |
| AC-07 | Entries stored by `put()` contain only JSON text in `response_json`; no pickle bytes are present | `grep` CI assertion + `test_no_pickle_usage` |
| AC-08 | `tag cache node disable --profile coder` sets `cache.enabled=false`; subsequent runs do not issue any SQL against `cache_entries` | `test_bypass_policy_skips_get` |
| AC-09 | When Redis is configured and reachable, a second identical call is served from Redis in < 3 ms (verifiable via `cache.lookup_latency_ms` span attribute) | Integration test with local Redis via `pytest-redis` fixture |
| AC-10 | When Redis is unreachable, `cache_store.get()` falls back to SQLite and does not raise or propagate the connection error | `test_redis_fallback_on_connection_error` |
| AC-11 | Semantic cache returns a hit when a paraphrased prompt achieves cosine similarity >= 0.85 with a stored entry | `test_semantic_hit_above_threshold` |
| AC-12 | Semantic cache does not return a hit when cosine similarity < `similarity_threshold` | `test_semantic_miss_below_threshold` |
| AC-13 | `tag cache node policy set --profile coder --node execute_code --policy bypass` causes zero cache reads or writes for `execute_code` node while other nodes continue caching | `test_policy_precedence` |
| AC-14 | Concurrent writes from 4 processes produce 1 000 rows with no data corruption (WAL-mode consistency) | `test_concurrent_writes` |
| AC-15 | `tag run` with `cache.enabled=false` (default) does not import `cache_store.py` (verified via `sys.modules` assertion) | Unit test asserting `"tag.cache_store" not in sys.modules` |
| AC-16 | `tag cache node stats --csv` produces valid RFC 4180 CSV parseable by Python's `csv.DictReader` | `test_stats_csv_output` |
| AC-17 | `tag cache node clear --all` without `--yes` prompts for confirmation; with `--yes` deletes all rows in `cache_entries` | Manual + unit test with mocked stdin |
| AC-18 | A 600 KB response is silently not cached; the span attribute `cache.skip_reason: "entry_too_large"` is set | `test_entry_too_large_skipped` |
| AC-19 | `compute_usd_saved()` produces a value within 0.01% of the expected value for a known input | `test_usd_saved_computation` |
| AC-20 | `_migrate_prd_104_tables()` is idempotent: calling it twice on the same connection raises no error | `test_migration_idempotent` |

---

## 13. Dependencies

| Dependency | Type | Reason | Optional? |
|------------|------|---------|-----------|
| `hashlib` (stdlib) | Runtime | SHA-256 cache key computation | No |
| `json` (stdlib) | Runtime | Response serialisation (replacing pickle) | No |
| `sqlite3` (stdlib) | Runtime | Primary cache persistence via `open_db()` | No |
| `re` (stdlib) | Runtime | Duration string parsing | No |
| `sentence-transformers` | Runtime | Embedding model for semantic cache mode | Yes (`cache.mode=semantic` only) |
| `numpy` | Runtime | Embedding vector serialisation and cosine similarity | Yes (semantic mode only) |
| `redis` | Runtime | Redis L1 cache backend | Yes (`cache.backend=redis` only) |
| `tracing.py` (PRD-013) | Internal | Span attribute population for cache metadata | No |
| `budget.py` (PRD-012) | Internal | `get_model_pricing()` for USD savings computation | No |
| `tool_retrieval.py` (PRD-043) | Internal | Shared `SentenceTransformer` model instance for embeddings | Yes (semantic mode) |
| `security.py` (PRD-034) | Internal | Future integration for secret scanning of cached responses | No (future) |
| `controller.py` | Internal | Migration hook, LLM call wrapper integration | No |

---

## 14. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should semantic cache be gated behind a separate optional dependency install group (`pip install tag[semantic-cache]`) or bundled with the existing `sentence-transformers` dependency from `tool_retrieval.py`? The latter avoids fragmentation but couples features. | Platform team | Before implementation start |
| OQ-02 | Should `cache_entries.response_json` store the raw Hermes API response object (including usage metadata) or only the text completion? Storing the full object enables accurate token savings replay but increases entry size. | Backend team | Before FR-05 implementation |
| OQ-03 | What is the appropriate default TTL for production use? `3600` (1 hour) is conservative but may result in low hit rates for infrequent users. `86400` (24 hours) risks serving stale LLM responses after model updates. Should the default vary by node type? | Product | Before GA |
| OQ-04 | For semantic cache, should the cosine similarity search be done in pure Python/numpy (linear scan, O(N)) or via a vector index (e.g., `hnswlib`, O(log N))? Linear scan suffices up to ~10 000 entries; a vector index is needed for larger caches but adds a new dependency. | Engineering | After beta, based on observed cache sizes |
| OQ-05 | Should expired entries be deleted eagerly at read time (current design: lazy eviction) or immediately on expiry using SQLite triggers or a background thread? Lazy eviction keeps the read path simple but accumulates dead rows. | Engineering | Before FR-15 implementation |
| OQ-06 | Should `tag cache node stats` include a per-node breakdown in the default (non-JSON) table output, or only in `--json`? The human-readable table may become unwieldy with many nodes. | UX | Before CLI surface is finalised |
| OQ-07 | Can the cache subsystem safely be enabled for self-consistency ensemble (PRD-101) where `temperature > 0` and diverse samples are the desired output? Caching temperature-parameterised calls with TTL=session would collapse diversity. The recommended approach is `bypass` policy for self-consistency nodes, but this needs explicit guidance. | PRD-101 author | Before integration with PRD-101 |
| OQ-08 | Should `tag cache node clear` honour Redis entries in addition to SQLite rows? Currently the design only purges SQLite; Redis entries expire via their own TTL. This means stale Redis entries may serve hits even after `tag cache node clear`. | Engineering | Before Redis backend is merged |
| OQ-09 | Are there legal or compliance requirements for certain user deployments that prohibit storing LLM response content at rest, even in a local SQLite file? If so, a `--no-store-response` mode that only caches tokens/cost metadata (not the response text) may be needed. | Legal / enterprise team | Before GA for enterprise customers |
| OQ-10 | Should the `embedding_meta_json` also store the model name used for embedding (e.g., `all-MiniLM-L6-v2`) so that cache entries are invalidated if the embedding model changes? Without this, changing `tool_retrieval.embed_model` would silently produce incompatible embeddings being compared against stored ones. | Engineering | Before semantic mode is merged |

---

## 15. Complexity and Timeline

**Estimated total effort:** M (8–10 engineering days)

### Phase 1 — Schema and Core Store (Days 1–3)

- Day 1: Write `_migrate_prd_104_tables()` migration, add to `open_db()` chain. Write `CacheKey`, `CacheEntry`, `CachePolicy`, `CacheStats` dataclasses. Write `cache_store.put()` and exact-match `cache_store.get()` (SQLite only, no Redis, no semantic). Write `parse_duration()`. Unit tests for all above.
- Day 2: Write `clear()`, `load_policy()`, `compute_stats()`, `compute_usd_saved()`. Wire `cache_events` recording into `get()` and `put()`. Unit tests for stats, clear, policy loading.
- Day 3: Write `_migrate_prd_104_tables()` idempotency test, concurrent-write integration test, WAL correctness test. Achieve 100% coverage on `cache_store.py` core functions.

### Phase 2 — Controller Integration and Span Attributes (Days 4–5)

- Day 4: Wire `cache_store.get()` / `cache_store.put()` into `run_chat_step()` in `controller.py`. Add `cache.*` span attributes to `tracing.py` close path. Wire policy precedence (node > profile > global).
- Day 5: Write integration tests using mocked Hermes to verify second-call cache hit, span attributes populated correctly, bypass policy respected at node level.

### Phase 3 — CLI Commands (Days 6–7)

- Day 6: Implement `cmd_cache_node_enable`, `cmd_cache_node_disable`, `cmd_cache_node_status`, `cmd_cache_node_clear`. Add command registration to `controller.py` dispatch table.
- Day 7: Implement `cmd_cache_node_stats` (with `--since`, `--until`, `--json`, `--csv`), `cmd_cache_node_policy` (set / list / reset). Write CLI surface tests against mocked DB.

### Phase 4 — Optional Backends (Days 8–9)

- Day 8: Implement Redis L1 backend (`_get_redis()`, `_promote_to_redis()`). Write Redis unit tests with mocked `redis.Redis`. Verify fallback-to-SQLite on `ConnectionError`.
- Day 9: Implement semantic cache mode (`embed_fn` integration, cosine similarity scan, embedding storage as BLOB). Write semantic cache unit and integration tests. Add `embedding_meta_json` model-name field (OQ-10 resolution).

### Phase 5 — Hardening and Documentation (Day 10)

- Day 10: Performance benchmarks (P99 latency assertions). Security CI assertion (`grep pickle`). Profile YAML writer for `cache.enabled` / `cache.ttl` fields. Address open questions OQ-01 through OQ-05 with decisions documented in code comments. Final review and handoff.

---

*This document describes PRD-104 at the Proposed stage. All implementation details are subject to revision during Phase 1 architecture review. Breaking changes to the `cache_store.py` public API require a new minor version annotation in the module docstring.*

