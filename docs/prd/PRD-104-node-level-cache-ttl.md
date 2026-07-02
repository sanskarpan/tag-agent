# PRD-104: Node-Level Caching with TTL for Expensive LLM Calls (`tag cache node`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `internal/store` (new cache store + tables), `internal/agent` + `internal/queue` (node-execution integration hooks), `internal/obs` (span attributes), `tag.sqlite3` (new tables)
**Depends on:** PRD-013 (agent tracing/observability), PRD-027 (eval framework), PRD-028 (sandbox), PRD-030 (prompt cache analytics), PRD-034 (secret scanning / security), PRD-041 (OTel span cost attribution), PRD-048 (structured tool-call child spans)
**Inspired by:** LangGraph CachePolicy, GPTCache, semantic cache (Zilliz)
**GitHub issue:** #349

---

## 1. Overview

Every `tag run` that exercises an LLM node — a call to the provider (Anthropic/OpenAI-compatible, via the `internal/llm` provider interface) with a prompt, model, and temperature — carries a latency cost (often 2–15 seconds per call) and a direct dollar cost proportional to tokens consumed. When the same logical task is re-executed with the same inputs — developer retry after a transient error, a nightly cron job re-evaluating an unchanged document, a self-consistency ensemble calling the same prompt multiple times — those costs multiply without providing new information. TAG currently has no mechanism to detect these redundant calls and serve a cached response instead.

This PRD introduces `tag cache node`: a node-level response cache that stores LLM responses keyed on a deterministic hash of `(prompt_text, model_id, temperature, profile)`, with a configurable TTL controlling entry lifetime. The cache is implemented as a new package in `internal/store` (the single `modernc.org/sqlite` state store), persisted in the existing WAL-mode database at `~/.tag/runtime/tag.sqlite3`, and integrated into the observability layer (`internal/obs`) so that cache hits and misses are recorded as span attributes visible in `tag trace`. An optional Redis backend enables cross-process and cross-machine cache sharing for teams running distributed TAG agents.

Beyond exact-match caching, the feature offers a semantic cache mode: when enabled, incoming prompts are embedded using the same `Embedder` interface (`internal/memory/embed`) already used by the in-Go tool-retrieval index (`internal/toolindex`), and a cosine-similarity search over recent cache entries identifies semantically equivalent prompts whose responses can be reused. This extends cache utility to slight prompt variations (rephrased queries, minor context differences) without requiring byte-for-byte prompt identity. Semantic cache uses a configurable similarity threshold (default `0.85`) above which a hit is declared, analogous to the `θ=0.7` skill-retrieval threshold in TDAG and reusing the same provider/offline embedding pipeline that backs `internal/toolindex`.

Per-node cache policies give granular control: a profile can declare that its `summarize` node should cache aggressively (TTL 24 h) while its `execute_code` node should never cache (TTL 0, policy `bypass`). This mirrors LangGraph's `CachePolicy` attachment semantics, where each node in the computation graph independently declares its caching behavior. The integration with TAG's observability layer means cache hits appear as zero-latency spans with a `cache.hit=true` attribute, giving cost and latency dashboards an accurate picture of effective vs. billed work.

The feature is additive and opt-in: when no cache policy is configured and the `cache.enabled` config key is `false` (the default), the cache store is never initialized and no overhead is introduced. Enabling caching for a profile is a single CLI command. The security design avoids the deserialization RCE vector identified in LangGraph's `SqliteCache` (GHSA-mhr3-j7m5-c7c9) by storing all cache values as JSON-serialized text via `encoding/json`, never as `gob`, `pickle`, or any reflection-driven binary format. Go's standard library has no `pickle` equivalent, so the RCE class is structurally absent from this design.

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
| G1 | Provide an exact-match cache keyed on `SHA-256(prompt_text ‖ model_id ‖ temperature ‖ profile)` (via `crypto/sha256`) with configurable TTL per node and per profile. |
| G2 | Provide a semantic cache mode using the shared `Embedder` interface (`internal/memory/embed`, reusing the pipeline behind `internal/toolindex`) with in-Go cosine-similarity threshold (default `0.85`). |
| G3 | Persist cache entries in `tag.sqlite3` (SQLite WAL, `modernc.org/sqlite`) via a new `cache_entries` table using the shared `internal/store` connection, with optional Redis backend for cross-process sharing. |
| G4 | Integrate with `internal/obs` so every cache hit and miss is recorded as a span attribute (`cache.hit`, `cache.key`, `cache.backend`, `cache.similarity_score`). |
| G5 | Expose per-node, per-profile cache policies in CLI config: `tag cache node enable`, `tag cache node disable`, `tag cache node policy set`. |
| G6 | Provide `tag cache node status --json`, `tag cache node stats --json`, and `tag cache node clear --older-than <duration>` management commands. |
| G7 | Zero performance overhead (no cache-store initialization, no SQLite queries) when caching is disabled for a profile. |
| G8 | Avoid the deserialization RCE vector: all stored values are JSON text (`encoding/json`), never `gob`/`pickle` or any reflection-driven binary blob. |
| G9 | Emit eviction on TTL expiry lazily (at read time) plus a periodic background sweep goroutine configurable via `cache.sweep_interval_hours`. |
| G10 | Cache hits reduce billed token counts: `tag cache node stats` reports `tokens_saved` and `usd_saved` based on the embedded model pricing table in `internal/obs`. |

### 3.2 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Caching tool call results (web search, code execution): those are non-deterministic by nature and should use purpose-specific caches. This PRD covers LLM inference calls only. |
| NG2 | Distributed cache invalidation protocols: when using Redis, TTL-based expiry is the sole invalidation mechanism. No pub/sub invalidation events. |
| NG3 | Cache warming: pre-populating the cache by issuing synthetic requests before real runs is not part of this PRD. |
| NG4 | Serving cache hits for streaming responses: streaming output is not cached; only complete, non-streaming completions (the accumulated final `Finish` event) are stored and replayed. |
| NG5 | Cross-user cache sharing: cache entries are scoped per-user (by filesystem path) even in Redis mode. No multi-tenant shared cache. |
| NG6 | Fine-grained cache invalidation triggered by profile edits: when a system prompt changes, cache entries for that profile should be manually cleared with `tag cache node clear --profile <name>`. No automatic invalidation. |
| NG7 | Semantic cache for non-text inputs (images, files): semantic similarity is computed over the text portion of the prompt only. |
| NG8 | Real-time monitoring dashboard for cache hit rates: stats are available via CLI commands; a live TUI panel is out of scope. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Cache hit latency | P50 < 5 ms for SQLite backend, < 2 ms for in-memory L1 | `testing.B` benchmark against `Cache.Get()` with 10 000 warm entries |
| Cache miss overhead | < 2 ms added to any LLM call path (i.e., `Cache.Get()` returns a miss in < 2 ms) | Benchmark against cold cache |
| Exact-match accuracy | 100% — no false cache hits for distinct `(prompt, model, temperature, profile)` tuples | Fuzz test (`testing/quick` / `go test -fuzz`) with 10 000 distinct prompts, verify zero cross-contamination |
| Semantic cache precision | Cosine similarity threshold `0.85` produces < 1% false positive rate on a held-out eval suite | Manual spot-check on `evals/coding.yaml` cases with paraphrased prompts |
| Token savings accuracy | `tokens_saved` field matches actual re-run token count within ±5% | Compare cached stats vs. force-fresh re-run counts |
| Zero-overhead guarantee | `tag run` wall time with cache disabled is statistically identical to pre-feature wall time (t-test over 20 runs) | CI benchmark job |
| SQLite WAL safety | No data corruption after 100 concurrent read/write operations across 4 goroutines/processes | Go test with `errgroup` + `t.Parallel()` |
| TTL correctness | Entries older than TTL are never returned; entries within TTL are always returned | Table-driven unit test with an injected `Clock` (fake time source) |
| Redis fallback | When Redis is unreachable, falls back to SQLite without error propagation | Unit test with a fake Redis client returning a dial error |
| RCE safety | `Cache.Get()` never decodes stored data via `gob`/`pickle`/reflection blobs | `grep -rn 'encoding/gob' internal/store/cache*.go` returns empty; code review |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer iterating on a prompt | enable node caching for my `coder` profile with `tag cache node enable --profile coder --ttl 3600` | Nodes whose inputs have not changed are served from cache instantly, reducing my iteration loop from 40 s to 4 s |
| U2 | Team platform engineer | run `tag cache node stats --json` | I can see total cache hits, misses, tokens saved, and USD saved across all profiles in a machine-readable format for dashboards |
| U3 | Developer running a nightly eval job | configure TTL 86400 for the `researcher` profile | The CI eval job re-uses LLM responses from previous runs when documents have not changed, cutting eval runtime from 20 min to 2 min |
| U4 | Developer debugging a cache miss | inspect `tag trace show <run_id>` | I see `cache.hit=false, cache.key=sha256:abc123` in the span attributes so I understand exactly why the cache was not used |
| U5 | Security-conscious developer | know that cached response data is stored as JSON, not a reflection-driven binary blob | I am not exposed to the deserialization RCE vector described in GHSA-mhr3-j7m5-c7c9 |
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
- `--semantic`: Enable semantic similarity cache mode in addition to exact-match. Requires an embeddings provider configured (or the build-tagged offline MiniLM model) for the `Embedder` interface.
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
| FR-01 | The cache package must expose `Get(ctx, key Key, policy Policy) (*Entry, bool, error)` and `Put(ctx, key Key, entry Entry, policy Policy) error` methods on a `Cache` struct operating on the SQLite `cache_entries` table via the shared `internal/store` connection. | P0 |
| FR-02 | The cache key must be computed as `SHA-256(prompt_text + "\x00" + model_id + "\x00" + fmt.Sprintf("%.4f", temperature) + "\x00" + profile)` using `crypto/sha256`, hex-encoded via `encoding/hex` to a 64-character digest. | P0 |
| FR-03 | `Get()` must compare the entry's `expires_at` timestamp against `time.Now().UTC()` before returning; expired entries must return a miss and be lazily deleted. | P0 |
| FR-04 | `Put()` must compute `expires_at = time.Now().UTC().Add(time.Duration(ttl) * time.Second)` for `ttl > 0`; for `ttl == 0`, `expires_at` must be `NULL` (permanent), represented as `sql.NullString{Valid: false}`. | P0 |
| FR-05 | Cache response values must be stored as JSON text (`response_json TEXT NOT NULL`) using `json.Marshal`. The package must never use `encoding/gob`, `pickle`, or any reflection-driven binary serialization. | P0 (security) |
| FR-06 | The `internal/obs` span-close path must annotate the span with `cache.hit: bool`, `cache.key: string`, `cache.backend: string`, `cache.similarity_score: float64/null` (via `attribute.KeyValue` on the OTel span) when a cache lookup is performed. | P0 |
| FR-07 | When `cache.policy` for a profile or node is `bypass`, `Cache.Get()` and `Cache.Put()` must be entirely skipped (early return); no SQL queries may be issued. | P0 |
| FR-08 | Semantic cache mode must embed the prompt using the `Embedder` interface (`internal/memory/embed`, default provider embedding, offline MiniLM behind a build tag), compute in-Go cosine similarity against all non-expired entries for the same `model_id` and `profile`, and return the entry with the highest similarity if it exceeds `similarity_threshold`. | P1 |
| FR-09 | Semantic cache must store the prompt embedding as a `BLOB` in `cache_entries.prompt_embedding` (a `[]float32` serialized to little-endian bytes via `encoding/binary`, with dtype/shape metadata in `embedding_meta_json`). | P1 |
| FR-10 | When Redis backend is configured and reachable (`github.com/redis/go-redis/v9`), `Cache.Get()` must first query Redis (L1), then fall back to SQLite (L2) on a miss, and promote the SQLite hit to Redis. When Redis is unreachable, must fall back silently to SQLite-only. | P1 |
| FR-11 | `tag cache node clear --older-than <duration>` must parse duration strings in the format `<N>h`, `<N>d`, `<N>m` (minutes), `<N>w` (weeks) and delete matching rows using `DELETE FROM cache_entries WHERE expires_at < ?`. | P1 |
| FR-12 | `tag cache node stats` must compute `usd_saved` using the embedded per-model pricing table in `internal/obs` (`GetModelPricing(modelID) ModelPricing`) as `tokens_saved_input * input_price_per_token + tokens_saved_output * output_price_per_token`. | P1 |
| FR-13 | `tag cache node enable` must write a `[cache]` section to the target profile's YAML file (`gopkg.in/yaml.v3` marshal): `enabled: true`, `ttl: <N>`, `backend: sqlite|redis`, `mode: exact|semantic`, `similarity_threshold: <float>`, via a `gofrs/flock` + `os.Rename` atomic locked write. | P1 |
| FR-14 | The cache package must record each cache access event (hit/miss, key, profile, node, latency_ms, tokens_prompt, tokens_completion) into the `cache_events` table for aggregation by `stats`. | P1 |
| FR-15 | A background sweep goroutine (started via `context.Context`, driven by a `time.Ticker`) deletes all expired entries when `cache.sweep_interval_hours` elapses. The last-sweep timestamp is stored in the `cache_meta` key-value table so the interval survives restarts; a lazy sweep also runs before the first cache operation after the interval elapses. | P2 |
| FR-16 | `tag cache node status` must compute per-profile hit rate as `hits / (hits + misses)` from the `cache_events` table, applying a 30-day rolling window by default. | P1 |
| FR-17 | The key-derivation function must exclude fields that should not affect caching semantics: message IDs, timestamps embedded in tool call metadata, and run-specific trace IDs. | P0 |
| FR-18 | When `--node <node_name>` is provided, per-node policy takes precedence over the profile-level policy using a precedence chain: node policy > profile policy > global default (disabled). | P1 |
| FR-19 | `tag cache node stats --csv` must output valid RFC 4180 CSV (via `encoding/csv`) with a header row. | P2 |
| FR-20 | Cache entries must not exceed `cache.max_entry_size_kb` (default: `512` KB). Entries larger than this limit are silently not cached; a warning is recorded on the calling span as `cache.skip_reason: "entry_too_large"`. | P2 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Cache `Get()` P99 latency (SQLite, warm, 10 000 entries) | < 10 ms |
| NFR-02 | Cache `Get()` P99 latency (Redis, warm) | < 3 ms |
| NFR-03 | Cache `Put()` P99 latency (SQLite, WAL mode) | < 20 ms |
| NFR-04 | Heap overhead of the cache package when active | < 2 MB additional (measured via `runtime.ReadMemStats`) |
| NFR-05 | Initialization cost when `cache.enabled = false` | 0 ms (cache store not constructed; no goroutine started) |
| NFR-06 | SQLite `cache_entries` table must use WAL journal mode consistent with the rest of `tag.sqlite3` | Enforced via the shared `internal/store` PRAGMA sequence |
| NFR-07 | Cache entries must survive process restart (SQLite persistence) | 100% for entries within TTL |
| NFR-08 | No cache data must be written to log files, stdout, or error messages (responses may be confidential) | Code review + `grep` assertion in CI |
| NFR-09 | Semantic cache embedding computation must not block the request goroutine for > 200 ms; it runs on a worker goroutine and is bounded by `context.WithTimeout` | Enforced in implementation |
| NFR-10 | `tag cache node clear --all` must complete within 5 seconds for a database with 100 000 entries | Verified by performance test |
| NFR-11 | Redis connection pool must be bounded to max 5 connections (configurable via `cache.redis_pool_size`, mapped to `redis.Options.PoolSize`) | Configurable default |
| NFR-12 | All SQL in the cache package must use parameterized queries (`?` placeholders); no string concatenation of user-controlled values | Static analysis (`go vet`) + code review |
| NFR-13 | The `cache_entries` table must include indexes on `(profile, model_id, expires_at)` and `(cache_key)` to ensure sub-10ms lookups at 100 000 entries | DDL enforced |
| NFR-14 | The cache package must be fully testable without a running Redis or real LLM by accepting an injected `*sql.DB`, a `RedisClient` interface, and an `Embedder` interface | Interface-based dependency injection |

---

## 9. Technical Design

### 9.1 New Files

| File | Purpose |
|------|---------|
| `internal/store/cache.go` | Core cache implementation: exact-match lookup, semantic lookup, put, clear, stats, sweep |
| `internal/store/cache_key.go` | `Key` value type + `Digest()` / `PromptDigest()` (`crypto/sha256`) |
| `internal/store/cache_test.go` | Table-driven unit tests (in-memory SQLite, fake Redis, fake Embedder) |
| `internal/store/cache_integration_test.go` | Integration tests against a real temp SQLite via `internal/store` |
| `internal/cli/cache_node.go` | `tag cache node ...` cobra command tree |

Existing files modified:

| File | Change |
|------|--------|
| `internal/obs/tracing.go` | Extend span attribute population on span close to include `cache.*` keys when a cache lookup was performed |
| `internal/agent/loop.go` | Wire `Cache.Get()` / `Cache.Put()` into the LLM call path of the agent inner loop; resolve per-node policy |
| `internal/queue/scheduler.go` | Apply node cache policy at DAG-node execution boundaries (nodes that dispatch LLM calls) |
| `internal/store/migrate` | Add `migratePRD104(ctx, db)` to the migration chain |
| `internal/obs/pricing.go` | Export `GetModelPricing(modelID string) (ModelPricing, bool)` for `Cache.computeUSDSaved()` |

### 9.2 SQLite DDL

```sql
-- Migration: PRD-104
-- Applied in migratePRD104(ctx, db)

CREATE TABLE IF NOT EXISTS cache_entries (
    cache_key           TEXT NOT NULL,          -- SHA-256 hex of (prompt+model+temp+profile)
    profile             TEXT NOT NULL,
    node_name           TEXT NOT NULL DEFAULT 'llm_call',
    model_id            TEXT NOT NULL,
    temperature         REAL NOT NULL,
    prompt_hash         TEXT NOT NULL,          -- SHA-256 of prompt_text alone (for semantic lookup)
    prompt_embedding    BLOB,                   -- []float32 little-endian bytes; NULL if semantic disabled
    embedding_meta_json TEXT NOT NULL DEFAULT '{}',  -- {"dtype":"float32","dim":384,"model":"MiniLM-L6-v2"}
    response_json       TEXT NOT NULL,          -- JSON-serialised LLM response (never gob/pickle)
    prompt_tokens       INTEGER NOT NULL DEFAULT 0,
    completion_tokens   INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL,          -- RFC 3339 UTC
    expires_at          TEXT,                   -- RFC 3339 UTC; NULL = permanent
    hit_count           INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (cache_key, profile)
);

CREATE INDEX IF NOT EXISTS idx_cache_entries_profile_model_expires
    ON cache_entries (profile, model_id, expires_at);

CREATE INDEX IF NOT EXISTS idx_cache_entries_key
    ON cache_entries (cache_key);

CREATE TABLE IF NOT EXISTS cache_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at     TEXT NOT NULL,              -- RFC 3339 UTC
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

### 9.3 Core Go Types

```go
// internal/store/cache_key.go
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Key is an immutable value object representing the lookup key for one LLM call.
type Key struct {
	PromptText  string
	ModelID     string
	Temperature float64
	Profile     string
	NodeName    string // default "llm_call"
}

// Digest returns a 64-character SHA-256 hex digest.
//
// Fields are joined with the NUL byte separator to prevent length-extension
// collisions between adjacent fields. Temperature is formatted to 4 decimal
// places to absorb float representation noise.
func (k Key) Digest() string {
	canonical := strings.Join([]string{
		k.PromptText,
		k.ModelID,
		fmt.Sprintf("%.4f", k.Temperature),
		k.Profile,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// PromptDigest is the SHA-256 of the prompt text alone (semantic candidate filtering).
func (k Key) PromptDigest() string {
	sum := sha256.Sum256([]byte(k.PromptText))
	return hex.EncodeToString(sum[:])
}
```

```go
// internal/store/cache.go
package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// Entry is one cached LLM response. Serialized to/from cache_entries.
type Entry struct {
	CacheKey         string          `json:"cache_key"`
	Profile          string          `json:"profile"`
	NodeName         string          `json:"node_name"`
	ModelID          string          `json:"model_id"`
	Temperature      float64         `json:"temperature"`
	Response         json.RawMessage `json:"response"` // provider response as JSON (never gob)
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	CreatedAt        time.Time       `json:"created_at"`
	ExpiresAt        *time.Time      `json:"expires_at,omitempty"` // nil = permanent
	HitCount         int             `json:"hit_count"`
	Similarity       *float64        `json:"similarity,omitempty"` // set on semantic hits
}

// Policy is a per-profile or per-node cache policy, read from cache_policies.
type Policy struct {
	Profile             string  `json:"profile"`
	NodeName            string  `json:"node_name"`             // default "__profile__"
	Policy              string  `json:"policy"`                // "cache" | "bypass" | "refresh"
	TTLSeconds          *int    `json:"ttl_seconds,omitempty"` // nil = inherit default
	Backend             string  `json:"backend"`               // "sqlite" | "redis"
	Mode                string  `json:"mode"`                  // "exact" | "semantic"
	SimilarityThreshold float64 `json:"similarity_threshold"`
	Enabled             bool    `json:"enabled"`
}

// Stats is aggregated data returned by `tag cache node stats`.
type Stats struct {
	Profile             *string `json:"profile"`
	Since               *string `json:"since"`
	Until               *string `json:"until"`
	ExactHits           int     `json:"exact_hits"`
	ExactMisses         int     `json:"exact_misses"`
	SemanticHits        int     `json:"semantic_hits"`
	TotalRequests       int     `json:"total_requests"`
	TokensSavedPrompt   int     `json:"tokens_saved_prompt"`
	TokensSavedComplete int     `json:"tokens_saved_completion"`
	USDSaved            float64 `json:"usd_saved"`
	AvgHitLatencyMS     float64 `json:"avg_hit_latency_ms"`
	AvgMissLatencyMS    float64 `json:"avg_miss_latency_ms"`
	EntryCount          int     `json:"entry_count"`
}

func (s Stats) HitRate() float64 {
	total := s.ExactHits + s.ExactMisses + s.SemanticHits
	if total == 0 {
		return 0
	}
	return float64(s.ExactHits+s.SemanticHits) / float64(total)
}
```

`invopop/jsonschema` is used to generate the JSON schema for the `--json` output payloads, keeping the CLI contract in sync with the structs above.

### 9.4 Cache Lookup Algorithm

```go
// internal/store/cache.go (continued)

const MaxEntrySizeBytes = 512 * 1024 // 512 KB default

// RedisClient is the minimal Redis surface the cache uses (satisfied by *redis.Client);
// an interface so tests can inject a fake.
type RedisClient interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, val string, ttl time.Duration) error
}

// Embedder produces an L2-normalised embedding for a prompt (internal/memory/embed).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Cache holds injected dependencies (DB, optional Redis, optional Embedder) and a Clock.
type Cache struct {
	db     *sql.DB
	redis  RedisClient // may be nil
	embed  Embedder    // may be nil (exact-only)
	now    func() time.Time
}

// Get performs a two-tier lookup:
//   1. Redis L1 (if configured and reachable) — O(1) network round-trip
//   2. SQLite L2                              — O(log N) B-tree index scan
//
// Within SQLite, two modes:
//   a. Exact match: WHERE cache_key = ? AND (expires_at IS NULL OR expires_at > ?)
//   b. Semantic match (policy.Mode == "semantic" && c.embed != nil):
//        - embed the query prompt
//        - SELECT non-expired rows WHERE profile = ? AND model_id = ? AND prompt_embedding IS NOT NULL
//        - in-Go cosine similarity against each; return best if >= SimilarityThreshold
//
// Returns (nil, false, nil) on miss; caller proceeds with a live LLM call.
func (c *Cache) Get(ctx context.Context, key Key, policy Policy) (*Entry, bool, error) {
	if policy.Policy == "bypass" || policy.Policy == "refresh" {
		return nil, false, nil // never read
	}
	now := c.now().UTC()
	digest := key.Digest()

	// ── L1: Redis ─────────────────────────────────────────────
	if c.redis != nil {
		if raw, err := c.redis.Get(ctx, "tag:cache:"+digest); err == nil && raw != "" {
			var e Entry
			if json.Unmarshal([]byte(raw), &e) == nil {
				if e.ExpiresAt == nil || e.ExpiresAt.After(now) {
					c.recordEvent(ctx, key, "hit_exact", 0, &e, nil)
					return &e, true, nil
				}
			}
		}
		// Redis unreachable / miss: fall through to SQLite.
	}

	// ── L2a: SQLite exact match ────────────────────────────────
	row := c.db.QueryRowContext(ctx, `
		SELECT cache_key, profile, node_name, model_id, temperature,
		       response_json, prompt_tokens, completion_tokens,
		       created_at, expires_at, hit_count
		  FROM cache_entries
		 WHERE cache_key = ? AND profile = ?
		   AND (expires_at IS NULL OR expires_at > ?)
		 LIMIT 1`, digest, key.Profile, now.Format(time.RFC3339))
	if e, ok, err := scanEntry(row); err != nil {
		return nil, false, err
	} else if ok {
		_, _ = c.db.ExecContext(ctx,
			`UPDATE cache_entries SET hit_count = hit_count + 1 WHERE cache_key = ? AND profile = ?`,
			digest, key.Profile)
		if c.redis != nil {
			c.promoteToRedis(ctx, e, policy)
		}
		c.recordEvent(ctx, key, "hit_exact", 0, e, nil)
		return e, true, nil
	}

	// ── L2b: SQLite semantic match (optional) ──────────────────
	if policy.Mode == "semantic" && c.embed != nil {
		queryVec, err := c.embed.Embed(ctx, key.PromptText)
		if err == nil {
			best, bestSim := c.bestSemantic(ctx, key, queryVec, now)
			if best != nil && bestSim >= policy.SimilarityThreshold {
				best.Similarity = &bestSim
				c.recordEvent(ctx, key, "hit_semantic", 0, best, &bestSim)
				return best, true, nil
			}
		}
	}

	return nil, false, nil
}

// Put stores an LLM response in the cache.
//
// Guards:
//   - Skip if policy is "bypass".
//   - Skip if JSON-serialised response exceeds MaxEntrySizeBytes.
//   - Always use json.Marshal; never gob/pickle.
func (c *Cache) Put(ctx context.Context, key Key, response json.RawMessage, policy Policy,
	promptTokens, completionTokens int) error {
	if policy.Policy == "bypass" {
		return nil
	}
	if len(response) > MaxEntrySizeBytes {
		return nil // caller records cache.skip_reason="entry_too_large" on its span
	}

	now := c.now().UTC()
	var expiresAt sql.NullString
	if policy.TTLSeconds != nil && *policy.TTLSeconds > 0 {
		exp := now.Add(time.Duration(*policy.TTLSeconds) * time.Second)
		expiresAt = sql.NullString{String: exp.Format(time.RFC3339), Valid: true}
	}

	var embBlob []byte
	embMeta := "{}"
	if policy.Mode == "semantic" && c.embed != nil {
		if vec, err := c.embed.Embed(ctx, key.PromptText); err == nil {
			embBlob = float32sToLEBytes(l2Normalise(vec)) // dot product == cosine at read time
			embMeta = string(mustJSON(map[string]any{"dtype": "float32", "dim": len(vec)}))
		}
	}

	_, err := c.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO cache_entries
		  (cache_key, profile, node_name, model_id, temperature,
		   prompt_hash, prompt_embedding, embedding_meta_json,
		   response_json, prompt_tokens, completion_tokens,
		   created_at, expires_at, hit_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		key.Digest(), key.Profile, key.NodeName, key.ModelID,
		roundTo4(key.Temperature), key.PromptDigest(),
		embBlob, embMeta,
		string(response), promptTokens, completionTokens,
		now.Format(time.RFC3339), expiresAt)
	if err != nil {
		return err
	}
	if c.redis != nil {
		c.promoteToRedis(ctx, &Entry{ /* ... */ }, policy) // non-fatal on failure
	}
	return nil
}
```

`bestSemantic` reads embedding BLOBs (`[]float32` via `encoding/binary`), computes cosine similarity in Go (both vectors L2-normalised at storage time, so the similarity is a dot product), and returns the best candidate. This reuses the `internal/toolindex` / `internal/memory` in-Go cosine path — no `numpy` and no separate vector engine, consistent with the migration decision to keep brute-force cosine over BLOB columns until scale (~100k vectors) triggers `sqlite-vec`.

### 9.5 Integration with `internal/obs`

The agent inner loop (`internal/agent/loop.go`) wraps the LLM call. On a hit, it emits a zero-latency span; on a miss it emits the normal call span and then writes to the cache. Span attributes are set through the OTel API in `internal/obs`.

```go
// internal/agent/loop.go — illustrative integration sketch

key := store.Key{
	PromptText:  canonicalPrompt,
	ModelID:     modelID,
	Temperature: temperature,
	Profile:     profileName,
	NodeName:    nodeName,
}
policy := cache.LoadPolicy(ctx, db, profileName, nodeName) // node > profile > global

t0 := time.Now()
cached, hit, err := cache.Get(ctx, key, policy)
lookupMS := float64(time.Since(t0).Microseconds()) / 1000.0

if err == nil && hit {
	ctx, span := tracer.Start(ctx, nodeName+":cache_hit")
	span.SetAttributes(
		attribute.Bool("cache.hit", true),
		attribute.String("cache.key", key.Digest()),
		attribute.String("cache.backend", policy.Backend),
		attribute.String("cache.hit_type", hitType(cached.Similarity)),
		attribute.Float64("cache.lookup_latency_ms", round2(lookupMS)),
	)
	if cached.Similarity != nil {
		span.SetAttributes(attribute.Float64("cache.similarity_score", *cached.Similarity))
	}
	span.End() // prompt/completion tokens = 0 (not billed)
	return cached.Response, nil
}

// Cache miss: call the provider normally.
ctx, span := tracer.Start(ctx, nodeName)
resp, usage, err := provider.Complete(ctx, req) // internal/llm
span.SetAttributes(
	attribute.Bool("cache.hit", false),
	attribute.String("cache.key", key.Digest()),
	attribute.String("cache.backend", policy.Backend),
	attribute.Float64("cache.lookup_latency_ms", round2(lookupMS)),
)
span.End()

_ = cache.Put(ctx, key, resp, policy, usage.PromptTokens, usage.CompletionTokens)
return resp, nil
```

### 9.6 Duration Parsing Utility

Go's `time.ParseDuration` does not accept day/week units, so the cache defines a small parser that maps the CLI's `<N>{m,h,d,w}` grammar onto `time.Duration`.

```go
// internal/store/cache.go
package store

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var durationRE = regexp.MustCompile(`^(\d+)(m|h|d|w)$`)

// ParseDuration parses "30m", "24h", "7d", "2w" into a time.Duration.
func ParseDuration(s string) (time.Duration, error) {
	m := durationRE.FindStringSubmatch(strings.TrimSpace(strings.ToLower(s)))
	if m == nil {
		return 0, fmt.Errorf(
			"unrecognised duration %q: expected <N>m (minutes), <N>h (hours), <N>d (days), <N>w (weeks)", s)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{
		"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
	}[m[2]]
	return time.Duration(n) * unit, nil
}
```

### 9.7 USD Savings Computation

```go
// internal/store/cache.go

// computeUSDSaved sums USD saved across all cache hit events.
//
// For each 'hit_exact' or 'hit_semantic' event, the saved cost is:
//     tokens_prompt     * pricing.InputPerToken
//   + tokens_completion * pricing.OutputPerToken
// using the embedded model pricing table in internal/obs.
// Rows with unknown model IDs are skipped (no pricing data).
func computeUSDSaved(rows []eventRow, getPricing func(string) (obs.ModelPricing, bool)) float64 {
	var total float64
	for _, r := range rows {
		p, ok := getPricing(r.ModelID)
		if !ok {
			continue
		}
		total += float64(r.TokensPrompt)*p.InputPerToken +
			float64(r.TokensCompletion)*p.OutputPerToken
	}
	return math.Round(total*1e6) / 1e6
}
```

### 9.8 Redis Backend

Redis is an optional, opt-in cross-process backend (it does not affect the single-binary distribution story — it is an external service the operator already runs). It is accessed via `github.com/redis/go-redis/v9` and lazily initialised on first use behind the `RedisClient` interface, so unit tests inject a fake.

```go
// internal/store/cache_redis.go
package store

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// newRedis builds a bounded pool from config; returns nil (and no error) when
// no URL is set or the server is unreachable, so callers fall back to SQLite silently.
func newRedis(ctx context.Context, url string, poolSize int) RedisClient {
	if url == "" {
		return nil
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil
	}
	if poolSize <= 0 {
		poolSize = 5
	}
	opt.PoolSize = poolSize
	opt.DialTimeout = 500 * time.Millisecond
	opt.ReadTimeout = 500 * time.Millisecond
	client := redis.NewClient(opt)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil // Redis unavailable; SQLite-only
	}
	return &goRedis{client}
}

func (c *Cache) promoteToRedis(ctx context.Context, e *Entry, policy Policy) {
	blob, err := json.Marshal(e)
	if err != nil {
		return // non-fatal
	}
	var ttl time.Duration
	if policy.TTLSeconds != nil && *policy.TTLSeconds > 0 {
		ttl = time.Duration(*policy.TTLSeconds) * time.Second
	}
	_ = c.redis.Set(ctx, "tag:cache:"+e.CacheKey, string(blob), ttl) // SETEX when ttl>0
}
```

### 9.9 Migration Hook

```go
// internal/store/migrate/prd104.go
package migrate

import (
	"context"
	"database/sql"
)

// migratePRD104 creates cache_entries, cache_events, cache_policies, cache_meta
// tables (PRD-104). Idempotent (CREATE TABLE IF NOT EXISTS). Registered in the
// migration chain after migratePRD033_044.
func migratePRD104(ctx context.Context, db *sql.DB) error {
	const ddl = `
        CREATE TABLE IF NOT EXISTS cache_entries ( ... );  -- full DDL from 9.2
        CREATE TABLE IF NOT EXISTS cache_events  ( ... );
        CREATE TABLE IF NOT EXISTS cache_policies( ... );
        CREATE TABLE IF NOT EXISTS cache_meta    ( ... );
        CREATE INDEX IF NOT EXISTS idx_cache_entries_profile_model_expires
            ON cache_entries (profile, model_id, expires_at);
        CREATE INDEX IF NOT EXISTS idx_cache_entries_key
            ON cache_entries (cache_key);
        CREATE INDEX IF NOT EXISTS idx_cache_events_profile_occurred
            ON cache_events (profile, occurred_at);`
	_, err := db.ExecContext(ctx, ddl)
	return err
}
```

The migration is invoked from the `internal/store` migration chain (single-writer, `gofrs/flock` + WAL discipline) after `migratePRD033_044`.

---

## 10. Security Considerations

1. **No unsafe deserialization.** All cache values are stored as JSON text via `encoding/json`. The package must never use `encoding/gob`, a `pickle` port, or any reflection-driven binary decoder on stored bytes. This structurally eliminates the LangGraph CachePolicy RCE vector documented in GHSA-mhr3-j7m5-c7c9 (deserialising attacker-controlled pickle bytes from a cache backend grants arbitrary code execution). Go's standard library has no `pickle` equivalent, and `gob` is explicitly banned for cross-version state. CI must include a `grep -rn 'encoding/gob' internal/store/cache*.go` assertion that returns empty.

2. **Parameterised SQL only.** Every SQL statement in the cache package must use `?` placeholders passed as `ExecContext`/`QueryContext` args. No `fmt.Sprintf` or string concatenation of user-supplied values (profile names, node names, model IDs, cache keys) is permitted. This defeats attacker-controlled profile names that include SQL fragments.

3. **Cache key collision resistance.** SHA-256 (`crypto/sha256`) provides 256 bits of preimage resistance. The NUL-byte separator between fields prevents length-extension collisions (e.g., `prompt="ab", model="c"` vs. `prompt="a", model="bc"`). The `profile` field is included in the key to prevent cross-profile cache poisoning: an attacker who can write a cache entry for profile A cannot influence cache reads for profile B.

4. **Redis key namespace isolation.** All Redis keys are prefixed with `tag:cache:` followed by the SHA-256 digest. No user-controlled string is used directly as a Redis key. The Redis URL must come from the TAG config file (owned by the local user, read via `koanf`), not from environment variables or CLI flags, to prevent injection via a compromised environment.

5. **Response confidentiality.** Cache entries may contain sensitive LLM responses (code, private documents, business logic). The SQLite database file at `~/.tag/runtime/tag.sqlite3` must have filesystem permissions `0600` (user-only). The runtime-dir bootstrap must verify these permissions and warn if the file is world-readable. No cache data may be written to stdout, log files, or error messages — even in debug mode.

6. **Redis authentication.** When `cache.redis_url` includes a password (`redis://:password@host:port/0`), the URL is stored in the TAG config file (user-owned, `0600`). `tag config get cache.redis_url` must mask the password component, showing `redis://:****@host:port/0`. This follows the existing masking pattern for `agentops.api_key` in PRD-044.

7. **Prompt embedding privacy.** In semantic cache mode, `Embedder` embeddings are stored as BLOBs (`[]float32`) in SQLite. Embeddings are not directly reversible to the original prompt text, but they are proximity-preserving and could leak information about prompt similarity patterns. Users operating in high-sensitivity environments should disable semantic mode (`--mode exact`) or run `tag cache node clear` periodically.

8. **Cache poisoning via `--policy refresh`.** The `refresh` policy writes a fresh response to the cache, overwriting any existing entry. This could be abused by a process that injects a known-bad response. The `refresh` policy should only be configurable by the local user (no remote policy injection path). Future multi-tenant extensions must re-evaluate this.

9. **TTL bypass via clock skew.** Expiry comparison uses `time.Now().UTC()` on the reading machine. In distributed Redis mode, clock skew between writer and reader machines could cause premature or late expiry. Redis TTL (`SET ... EX`) uses the Redis server clock, which is authoritative; the `expires_at` TEXT field in SQLite is advisory for the SQLite-only path. Document this limitation.

10. **Secret scanning integration.** PRD-034 (secret scanning) should be extended to scan cache entries for accidentally cached API keys or tokens. A future integration can run the `internal/*` security pattern scanner against `response_json` at `Put()` time and emit a warning span attribute `cache.secret_detected=true` without blocking the cache write. This is a recommendation for a follow-on PRD rather than a blocking requirement here.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/store/cache_test.go`)

All tests use an in-memory SQLite connection (`sql.Open("sqlite", ":memory:")` via `modernc.org/sqlite`). Redis is a fake `RedisClient`; the `Embedder` is a stub returning fixed vectors. Tests are table-driven, and time-sensitive cases inject a fake `Clock` (`c.now`).

| Test | Description |
|------|-------------|
| `TestExactHit` | Put an entry; get with identical key; assert entry returned. |
| `TestExactMiss` | Get with a key never put; assert miss returned. |
| `TestTTLExpired` | Put with TTL=1s; advance fake clock to T+2s; assert `Get()` returns a miss. |
| `TestTTLPermanent` | Put with TTL=0 (permanent); advance clock by 1 year; assert entry still returned. |
| `TestBypassPolicySkipsGet` | Set policy to `bypass`; assert `Get()` returns a miss without issuing any SQL (verify via a query-counting `*sql.DB` wrapper). |
| `TestBypassPolicySkipsPut` | Set policy to `bypass`; call `Put()`; assert `cache_entries` remains empty. |
| `TestKeyCollisionResistance` | Two keys differing only at a field boundary (length extension); assert different digests. |
| `TestEntryTooLargeSkipped` | 600 KB response JSON; assert `Put()` does not insert a row. |
| `TestHitCountIncremented` | Get the same entry twice; assert `hit_count` becomes 2. |
| `TestRedisL1Hit` | Fake Redis returns a valid serialised entry; assert `Get()` returns it without querying SQLite. |
| `TestRedisFallbackOnDialError` | Fake Redis returns a dial error; assert `Get()` falls back to SQLite without returning an error. |
| `TestNoGobUsage` | Static assertion: `internal/store/cache*.go` does not import `encoding/gob`. |
| `TestParseDurationValid` | Table of `{"24h": 24h, "7d": 168h, ...}`; assert equality. |
| `TestParseDurationInvalid` | Assert `ParseDuration("5x")` returns an error. |
| `TestSemanticHitAboveThreshold` | Stub `Embedder` returns a fixed vector; put entry; `Get()` with a cosine-similar prompt; assert hit. |
| `TestSemanticMissBelowThreshold` | Stub returns an orthogonal vector; assert `Get()` returns a miss. |
| `TestUSDSavedComputation` | Feed known hit events + stub pricing; assert `computeUSDSaved` result. |
| `TestSQLParameterisation` | Profile name `'; DROP TABLE cache_entries; --`; assert table survives. |

### 11.2 Integration Tests (`internal/store/cache_integration_test.go`)

Use a real temp SQLite file (`t.TempDir()`) opened through the actual `internal/store` constructor.

| Test | Description |
|------|-------------|
| `TestPutGetRoundtripSQLite` | Full roundtrip with real DB; verify response JSON deserialises correctly. |
| `TestMigrationIdempotent` | Call `migratePRD104()` twice on the same DB; assert no error. |
| `TestConcurrentWrites` | `errgroup` with 4 goroutines each calling `Put()` 250 times; assert 1 000 rows, no corruption (WAL). |
| `TestClearOlderThan` | Insert 100 entries with varying `created_at`; `Clear(olderThan: 24h)`; assert correct subset deleted. |
| `TestStatsAggregation` | Insert known hit/miss events; `ComputeStats()`; assert hit_rate, tokens_saved, usd_saved. |
| `TestSweepDeletesExpired` | Insert 50 expired entries; trigger sweep; assert all 50 deleted. |
| `TestCacheEventWrittenOnHit` | After a hit, assert a row in `cache_events` with `event_type='hit_exact'`. |
| `TestPolicyPrecedence` | Profile-level `cache`, node-level `bypass`; assert node-level wins. |

### 11.3 Benchmarks

```go
// internal/store/cache_bench_test.go
// Run with: go test -bench=. -benchmem ./internal/store/

func BenchmarkGetWarm10kEntries(b *testing.B) {
	db := warmCacheDB(b, 10_000) // fixture: 10k entries
	c := NewCache(db, nil, nil)
	key := Key{PromptText: "test prompt", ModelID: "claude-sonnet-4-6", Temperature: 0.7, Profile: "coder"}
	pol := defaultPolicy()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, _ := c.Get(context.Background(), key, pol); !ok {
			b.Fatal("expected hit")
		}
	}
	// P99 of exact-match Get() across 10 000 entries must be < 10 ms.
}

func BenchmarkPutLatency(b *testing.B) {
	c := NewCache(emptyCacheDB(b), nil, nil)
	pol := defaultPolicy()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Put(context.Background(), randomKey(), sampleResponse(), pol, 100, 200)
	}
	// P99 of Put() must be < 20 ms on WAL-mode SQLite.
}
```

---

## 12. Acceptance Criteria

| ID | Criterion | Test Method |
|----|-----------|-------------|
| AC-01 | `tag cache node enable --profile coder --ttl 3600` writes `cache.enabled=true`, `cache.ttl=3600` to `coder.yaml` and inserts a row in `cache_policies` | Manual + `TestEnableWritesPolicy` |
| AC-02 | A second `tag run` with identical prompt/model/temperature on a cache-enabled profile returns without issuing a provider API call (verifiable by a fake `internal/llm` provider) | Integration test with stub provider |
| AC-03 | `tag trace show <run_id>` for a cache-hit run displays `cache.hit: true` and `cache.key: sha256:<hex>` in span attributes | Integration test |
| AC-04 | `tag cache node stats --json` output includes `hit_rate`, `tokens_saved`, `usd_saved` fields with correct types | `TestStatsAggregation` |
| AC-05 | `tag cache node clear --older-than 24h` deletes all entries with `expires_at < now - 24h` without deleting newer entries | `TestClearOlderThan` |
| AC-06 | `tag cache node clear --older-than 24h --dry-run` prints the count of entries that would be deleted without modifying the database | Unit test with row-count pre/post assertion |
| AC-07 | Entries stored by `Put()` contain only JSON text in `response_json`; no `gob`/binary blobs | `grep` CI assertion + `TestNoGobUsage` |
| AC-08 | `tag cache node disable --profile coder` sets `cache.enabled=false`; subsequent runs issue no SQL against `cache_entries` | `TestBypassPolicySkipsGet` |
| AC-09 | When Redis is configured and reachable, a second identical call is served from Redis in < 3 ms (verifiable via `cache.lookup_latency_ms` span attribute) | Integration test against a local Redis (`testcontainers-go` or a real instance) |
| AC-10 | When Redis is unreachable, `Cache.Get()` falls back to SQLite and does not return or propagate the connection error | `TestRedisFallbackOnDialError` |
| AC-11 | Semantic cache returns a hit when a paraphrased prompt achieves cosine similarity >= 0.85 with a stored entry | `TestSemanticHitAboveThreshold` |
| AC-12 | Semantic cache does not return a hit when cosine similarity < `similarity_threshold` | `TestSemanticMissBelowThreshold` |
| AC-13 | `tag cache node policy set --profile coder --node execute_code --policy bypass` causes zero cache reads or writes for `execute_code` while other nodes continue caching | `TestPolicyPrecedence` |
| AC-14 | Concurrent writes from 4 goroutines produce 1 000 rows with no data corruption (WAL consistency) | `TestConcurrentWrites` |
| AC-15 | `tag run` with `cache.enabled=false` (default) does not construct the cache store or start the sweep goroutine | Unit test asserting no `Cache` instance is created |
| AC-16 | `tag cache node stats --csv` produces valid RFC 4180 CSV parseable by `encoding/csv` | `TestStatsCSVOutput` |
| AC-17 | `tag cache node clear --all` without `--yes` prompts for confirmation; with `--yes` deletes all rows in `cache_entries` | Manual + unit test with a fake stdin reader |
| AC-18 | A 600 KB response is silently not cached; the span attribute `cache.skip_reason: "entry_too_large"` is set | `TestEntryTooLargeSkipped` |
| AC-19 | `computeUSDSaved()` produces a value within 0.01% of the expected value for a known input | `TestUSDSavedComputation` |
| AC-20 | `migratePRD104()` is idempotent: calling it twice on the same DB returns no error | `TestMigrationIdempotent` |

---

## 13. Dependencies

| Dependency | Type | Reason | Optional? |
|------------|------|---------|-----------|
| `crypto/sha256`, `encoding/hex` (stdlib) | Runtime | SHA-256 cache key computation | No |
| `encoding/json` (stdlib) | Runtime | Response serialisation (replacing pickle/gob) | No |
| `modernc.org/sqlite` + `database/sql` | Runtime | Primary cache persistence via `internal/store` | No |
| `regexp`, `time` (stdlib) | Runtime | Duration string parsing | No |
| `encoding/binary` (stdlib) | Runtime | `[]float32` embedding BLOB (de)serialisation | Yes (semantic mode only) |
| `internal/memory/embed` (`Embedder`) | Internal | Embedding model for semantic cache mode (provider or build-tagged offline MiniLM) | Yes (`cache.mode=semantic` only) |
| `github.com/redis/go-redis/v9` | Runtime | Redis L1 cache backend | Yes (`cache.backend=redis` only) |
| `internal/obs` (PRD-013) | Internal | OTel span attribute population + `GetModelPricing()` for USD savings | No |
| `internal/toolindex` (PRD-043) | Internal | Shared in-Go cosine + embedding pipeline reused for semantic lookup | Yes (semantic mode) |
| `internal/config` (koanf v2 + yaml.v3 + gofrs/flock) | Internal | Read config; atomic profile-YAML write-back for `[cache]` section | No |
| `github.com/invopop/jsonschema` | Build/Runtime | JSON schema for `--json` output contract | No |
| `internal/agent` + `internal/queue` | Internal | Node-execution wrapper integration (agent loop + DAG nodes) | No |
| security scanner (PRD-034) | Internal | Future integration for secret scanning of cached responses | No (future) |

---

## 14. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should semantic cache be gated behind a build tag (`-tags offline_embed`) for the offline MiniLM path, or always available via the provider `Embedder`? The latter avoids a second binary variant but adds a network dependency and per-embed cost. | Platform team | Before implementation start |
| OQ-02 | Should `cache_entries.response_json` store the full provider response object (including usage metadata) or only the text completion? Storing the full object enables accurate token savings replay but increases entry size. | Backend team | Before FR-05 implementation |
| OQ-03 | What is the appropriate default TTL for production use? `3600` (1 hour) is conservative but may result in low hit rates for infrequent users. `86400` (24 hours) risks serving stale LLM responses after model updates. Should the default vary by node type? | Product | Before GA |
| OQ-04 | For semantic cache, should the cosine similarity search be a brute-force in-Go linear scan (O(N)) or via a vector index (`sqlite-vec`, O(log N))? Linear scan suffices up to ~10 000 entries; a vector index is the documented scale trigger past ~100k, per the migration decision. | Engineering | After beta, based on observed cache sizes |
| OQ-05 | Should expired entries be deleted eagerly at read time (current design: lazy eviction) or immediately on expiry via a dedicated sweep goroutine / SQLite triggers? Lazy eviction keeps the read path simple but accumulates dead rows. | Engineering | Before FR-15 implementation |
| OQ-06 | Should `tag cache node stats` include a per-node breakdown in the default (non-JSON) table output, or only in `--json`? The human-readable table may become unwieldy with many nodes. | UX | Before CLI surface is finalised |
| OQ-07 | Can the cache subsystem safely be enabled for self-consistency ensemble (PRD-101) where `temperature > 0` and diverse samples are the desired output? Caching temperature-parameterised calls with TTL=session would collapse diversity. The recommended approach is `bypass` policy for self-consistency nodes, but this needs explicit guidance. | PRD-101 author | Before integration with PRD-101 |
| OQ-08 | Should `tag cache node clear` honour Redis entries in addition to SQLite rows? Currently the design only purges SQLite; Redis entries expire via their own TTL. This means stale Redis entries may serve hits even after `tag cache node clear`. | Engineering | Before Redis backend is merged |
| OQ-09 | Are there legal or compliance requirements for certain user deployments that prohibit storing LLM response content at rest, even in a local SQLite file? If so, a `--no-store-response` mode that only caches tokens/cost metadata (not the response text) may be needed. | Legal / enterprise team | Before GA for enterprise customers |
| OQ-10 | Should `embedding_meta_json` store the embedding model name + dim so cache entries are invalidated if the embedding model changes? Without this, changing the `Embedder` model would silently compare incompatible vectors (this ties into the migration's dimension-guard: 384-dim MiniLM vs 1536/3072-dim provider). | Engineering | Before semantic mode is merged |

---

## 15. Complexity and Timeline

**Estimated total effort:** M (8–10 engineering days)

### Phase 1 — Schema and Core Store (Days 1–3)

- Day 1: Write `migratePRD104()` and add to the `internal/store` migration chain. Write `Key`, `Entry`, `Policy`, `Stats` structs. Write `Cache.Put()` and exact-match `Cache.Get()` (SQLite only, no Redis, no semantic). Write `ParseDuration()`. Table-driven unit tests for all above.
- Day 2: Write `Clear()`, `LoadPolicy()`, `ComputeStats()`, `computeUSDSaved()`. Wire `cache_events` recording into `Get()` and `Put()`. Unit tests for stats, clear, policy loading.
- Day 3: Write the migration-idempotency test, concurrent-write (`errgroup`) integration test, WAL correctness test. Achieve high coverage on the cache package core.

### Phase 2 — Agent/Queue Integration and Span Attributes (Days 4–5)

- Day 4: Wire `Cache.Get()` / `Cache.Put()` into the agent inner loop (`internal/agent/loop.go`) and DAG-node execution (`internal/queue`). Add `cache.*` OTel span attributes in `internal/obs`. Wire policy precedence (node > profile > global).
- Day 5: Write integration tests using a stub `internal/llm` provider to verify second-call cache hit, span attributes populated correctly, bypass policy respected at node level.

### Phase 3 — CLI Commands (Days 6–7)

- Day 6: Implement `enable`, `disable`, `status`, `clear` cobra subcommands under `internal/cli/cache_node.go`. Register in the `tag cache node` command tree.
- Day 7: Implement `stats` (with `--since`, `--until`, `--json`, `--csv` via `encoding/csv`), `policy` (set / list / reset). Write CLI surface tests against an in-memory DB.

### Phase 4 — Optional Backends (Days 8–9)

- Day 8: Implement the Redis L1 backend (`newRedis()`, `promoteToRedis()`, `RedisClient` interface). Write Redis unit tests with a fake client. Verify fallback-to-SQLite on dial error.
- Day 9: Implement semantic cache mode (`Embedder` integration, in-Go cosine scan, `[]float32` BLOB storage via `encoding/binary`). Write semantic cache unit and integration tests. Add the embedding model name + dim to `embedding_meta_json` (OQ-10 resolution).

### Phase 5 — Hardening and Documentation (Day 10)

- Day 10: `testing.B` benchmarks (P99 latency assertions). Security CI assertion (`grep encoding/gob`). Profile-YAML writer for `cache.enabled` / `cache.ttl` (koanf read + yaml.v3 + flock atomic write). Address open questions OQ-01 through OQ-05 with decisions documented in package doc comments. Final review and handoff.

---

*This document describes PRD-104 at the Proposed stage. All implementation details are subject to revision during Phase 1 architecture review. Breaking changes to the cache package's exported API require a new minor version annotation in the package doc comment.*
