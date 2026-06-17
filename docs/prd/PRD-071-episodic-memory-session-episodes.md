# PRD-071: Episodic Memory: Structured Session Episode Storage (`tag mem episode`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Memory & Knowledge
**Affects:** `memory_episodes SQLite table`
**Depends on:** PRD-002 (cross-session memory journal), PRD-013 (agent tracing/observability), PRD-025 (semantic memory with confidence decay), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-034 (security)
**Inspired by:** Zep session episodes, Cognitive architectures (SOAR, ACT-R), MemoryOS
**GitHub Issue:** #345

---

## 1. Overview

Cognitive architectures such as SOAR and ACT-R distinguish sharply between declarative/semantic memory (timeless facts) and episodic memory (bounded experiences anchored in time). TAG's existing memory layer, implemented in `semantic_memory.py` and surfaced through `tag mem`, handles the declarative tier: individual facts with confidence decay, FTS search, and per-profile scoping. What is absent is the episodic tier: the structured record of *what happened during a complete agent session* — the sequence of key events, which entities were touched, what the outcome was, and a natural-language summary that lets future sessions ground-truth their decisions against past experience.

Episodic memory is the mechanism by which an agent can answer questions like "what did we decide about the auth module last Thursday?", "what refactoring approaches have I already tried on this codebase?", or "which sessions involving the database schema ended successfully?". Without it, each session starts from a blank context window. The agent may re-derive conclusions that were already reached, repeat mistakes that were already logged in `steps`, or fail to build on prior breakthroughs. The `runs` and `steps` tables in `tag.sqlite3` hold raw execution logs, but they are unstructured, voluminous, and not designed for semantic retrieval.

This PRD introduces `tag mem episode`: a first-class episodic memory subsystem that stores complete agent sessions as structured episodes in a new `memory_episodes` table. Each episode captures a natural-language summary, a JSON array of key events (tool calls, decisions, errors, file edits), the set of entities touched (files, functions, modules, URLs), the outcome (success/failure/partial), and an optional embedding vector for semantic search. Episodes are created either automatically at session end (via `--from-run`) or interactively by the user. They are queryable by profile, time range, entity, outcome, or free-text semantic search. They are distinct from individual `semantic_memories` (which are atomic facts) and from raw `runs`/`steps` rows (which are unstructured logs).

The design is inspired by three bodies of prior art. Zep's session episodes model each conversation as a graph node with temporal edges, extracting facts and summaries per turn and linking them via bitemporal valid-time semantics. Cognitive architectures (SOAR, ACT-R) treat episodic memory as an indexed store of past problem-solving episodes that can be pattern-matched against the current problem state to transfer learned strategies. MemoryOS structures memory into a three-tier OS analogy (working, episodic, semantic) where episodic memory serves as the middle tier — longer-lived than context but shorter-lived than facts — with explicit compression from episode to semantic fact when confidence is established. TAG's implementation adopts a pragmatic subset: episodes stored in SQLite with FTS5 full-text search, optional SentenceTransformer embeddings reusing `tool_retrieval.py`'s existing model, and an LLM-driven extraction pipeline to auto-generate structured episodes from completed runs.

The feature ships as four CLI subcommands under `tag mem episode`: `list`, `show`, `search`, and `create`. All four read from and write to the `memory_episodes` table via `open_db()`. The new `src/tag/episodic_memory.py` module contains all business logic and is independently testable.

---

## 2. Problem Statement

### 2.1 Stateless Sessions Lose Hard-Won Context

Every `tag run` or `tag loop` session starts with only the current context window, the static system prompt, and whatever facts have been explicitly saved to `semantic_memories`. When a developer works on a multi-day refactoring, the agent has no access to what approaches were tried on day one, which files caused problems, or what the team decided at session end. The developer must re-explain context in every new session. This is not merely inconvenient: it means the agent re-explores dead ends, contradicts prior decisions, and cannot accumulate problem-solving skill over time. The raw `runs` table contains all this history but it is unstructured text — injecting a week of `steps` rows into a context window is both token-expensive and cognitively unordered.

### 2.2 No Session-Level Retrieval Primitive

TAG's memory subsystem provides two retrieval granularities: individual atomic facts (via `tag mem search`) and raw execution logs (via `tag runs list`). Neither satisfies the need for session-level retrieval. Atomic facts are too granular — a refactoring session may produce fifty facts but the cohesive narrative of what was attempted and why matters more than any individual fact. Raw logs are too voluminous and unstructured — there is no way to semantically search "sessions where I worked on the auth module" without scanning every step row. A session-level episodic record — with a summary, key events, entities, and outcome — fills this gap and enables efficient retrieval at the right granularity.

### 2.3 No Automatic Knowledge Transfer Across Sessions

When an agent succeeds at a complex task, the knowledge of *how* it succeeded — the strategy, the sequence of tool calls, the entities involved — is ephemeral. It exists in the `steps` table but is not extracted, summarized, or made available to future sessions. Conversely, when a session fails (exit status non-zero, partial completion, or explicit error), the failure mode is not recorded in a form that future sessions can recognize and avoid. Episodic memory directly addresses this by creating a structured artifact from each session that encodes both success patterns and failure modes, making prior experience retrievable and injectable into future context windows.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Provide a `memory_episodes` SQLite table that stores complete session episodes with summary, key events, entities, outcome, and optional embedding vector. |
| G2 | Implement `tag mem episode create --from-run <run-id>` which extracts a structured episode from an existing run by invoking an LLM extraction pipeline over the `steps` rows. |
| G3 | Implement `tag mem episode list` and `tag mem episode show <episode-id>` for browsing and inspecting stored episodes. |
| G4 | Implement `tag mem episode search "<query>"` with FTS5 full-text search over episode summaries and events, plus optional semantic (embedding) search with `--semantic` flag. |
| G5 | Reuse `open_db()` and the WAL-mode SQLite connection pattern from `controller.py` for all database operations. |
| G6 | Implement all episode business logic in a new `src/tag/episodic_memory.py` module, callable from `cmd_memory_semantic` in `controller.py` without creating a new top-level command. |
| G7 | Support `--json` output on all subcommands for machine-readable consumption and scripting. |
| G8 | Auto-compress key facts from high-confidence episodes into `semantic_memories` via `tag mem episode promote <episode-id>`, enabling the episodic-to-semantic knowledge transfer loop. |
| G9 | Ensure all operations complete within acceptable latency bounds: `list`/`show` under 100 ms, `search` (FTS5) under 200 ms, `search --semantic` under 500 ms, `create --from-run` under 30 s (LLM call). |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Real-time episode ingestion during a running session. Episodes are created post-hoc from completed runs. Streaming episode construction is out of scope. |
| NG2 | Graph database backend. Episodes are stored in SQLite, not Neo4j, FalkorDB, or any graph store. Entity relationship graphs are not built in this PRD. |
| NG3 | Bitemporal SQL with `tstzrange` exclusion constraints. SQLite lacks native range types; valid_time semantics are tracked with `started_at`/`ended_at` TEXT columns only. |
| NG4 | Community detection (Leiden/Louvain) over the episode entity graph. Batch topic clustering is a follow-on feature (PRD-072). |
| NG5 | Replacing `semantic_memories`. Episodes and facts are complementary tiers. `tag mem episode promote` moves knowledge from episodic to semantic, but neither tier is deprecated. |
| NG6 | Multi-user or team-shared episode stores. Episodes are per-user local SQLite only; no cloud sync or shared access in this release. |
| NG7 | Automatic episode creation at session end without explicit invocation. The `--from-run` flag requires a deliberate user or CI action. Fully automatic post-run episode creation is a follow-on. |
| NG8 | LanceDB or external vector store integration. Embeddings are stored as BLOB in SQLite using numpy binary serialization, consistent with `tool_retrieval.py`'s existing approach. |

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Episode creation latency (LLM path) | p95 < 30 s | `timer` in `create_episode_from_run()` |
| FTS5 search latency | p99 < 200 ms | `time.perf_counter()` in `search_episodes()` |
| Semantic search latency | p99 < 500 ms | Includes SentenceTransformer encode + cosine sort |
| Extraction fidelity (key events) | >= 5 events for runs with >= 10 steps | Verified in integration test |
| Episode compression ratio | Summary <= 20% of raw steps token count | Measured in `create_episode_from_run()` |
| JSON output correctness | 100% of `--json` outputs parse as valid JSON | CI test against all subcommands |
| `tag mem episode list` cold-start | < 100 ms with 10,000 episodes | SQLite index benchmark |
| FTS5 recall | >= 90% on `tests/test_prd_features.py` search fixtures | Precision/recall test |
| Zero new mandatory dependencies | `tag mem episode` works without extra `pip install` | Import guard test |
| Promote accuracy | >= 80% of promoted facts judged relevant by human eval | Manual spot-check |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag mem episode create --from-run run-abc123` after a complex refactoring session | the key decisions, files touched, and outcome are preserved in a structured form I can retrieve next week |
| U2 | Developer | run `tag mem episode search "auth refactor" --top-k 3` | I can quickly find the three most relevant past sessions and inject their summaries into my next context window |
| U3 | Developer | run `tag mem episode list --profile coder --json` | I can pipe the episode list to a script that auto-injects the last 5 episode summaries into a new session's system prompt |
| U4 | Team lead | run `tag mem episode list --outcome failure --profile coder` | I can see which sessions failed and understand failure patterns before attempting a similar task |
| U5 | Developer | run `tag mem episode show ep-abc12345 --json` | I can inspect the full structured record of a past session including all key events and entities |
| U6 | Developer | run `tag mem episode search "database schema" --semantic --top-k 5` | I can find semantically related past sessions even if they don't share exact keywords with my query |
| U7 | Developer | run `tag mem episode promote ep-abc12345` | the most important facts from a successful session are extracted and stored in `semantic_memories` for permanent retention |
| U8 | CI engineer | run `tag mem episode create --from-run $RUN_ID --json` in a post-run hook | CI can automatically archive session knowledge and report episode IDs in the build log |
| U9 | Developer | run `tag mem episode list --since 2026-06-01 --entity src/tag/auth.py` | I can see all sessions that touched the auth module in the last two weeks |
| U10 | Developer | run `tag mem episode search "postgres connection pool" --top-k 3 --format-for-context` | I get a context-window-ready block I can paste directly into a new session's system prompt |

---

## 7. Proposed CLI Surface

All episodic memory subcommands live under `tag mem episode`. The `tag mem` namespace already exists (see `cmd_memory_semantic` in `controller.py` and the `mem_subcommand` dispatcher at line 9776). `episode` is added as a new subcommand routed through the same dispatcher.

### 7.1 `tag mem episode list`

List stored episodes, optionally filtered by profile, outcome, time range, or entity.

```
tag mem episode list
  [--profile <profile>]
  [--outcome success|failure|partial|unknown]
  [--since <ISO-date>]
  [--until <ISO-date>]
  [--entity <path-or-name>]
  [--limit <N>]
  [--json]
```

**Options:**
- `--profile`: Filter to episodes for this profile. Defaults to the configured `defaults.master_profile`.
- `--outcome`: Filter by outcome field. Choices: `success`, `failure`, `partial`, `unknown`.
- `--since`: ISO 8601 date or datetime. Include only episodes with `started_at >= since`.
- `--until`: ISO 8601 date or datetime. Include only episodes with `started_at <= until`.
- `--entity`: Filter episodes where `entities_json` contains this string (substring match after JSON parse).
- `--limit`: Maximum number of episodes to return. Default: 20.
- `--json`: Output JSON array of episode objects.

**Human-readable output example:**
```
EPISODE LIST  profile=coder  (3 of 47 total)
─────────────────────────────────────────────────────────────────────────────
ID           STARTED              OUTCOME   DURATION  SUMMARY
ep-a1b2c3d4  2026-06-10 14:32:01  success   18m 43s   Refactored auth.py JWT validation to use PyJWT 2.x API. Added refresh token rotation. All 12 tests passing.
ep-e5f6a7b8  2026-06-09 09:11:55  partial   32m 12s   Attempted PostgreSQL connection pool migration. Completed pgbouncer config but left asyncpg client wiring incomplete.
ep-c9d0e1f2  2026-06-07 17:04:33  failure    4m 02s   Tried to upgrade SQLAlchemy 1.4→2.0. Failed: 23 breaking API changes, session rolled back.
─────────────────────────────────────────────────────────────────────────────
```

**JSON output example:**
```json
[
  {
    "id": "ep-a1b2c3d4",
    "profile": "coder",
    "run_id": "run-abc123",
    "started_at": "2026-06-10T14:32:01Z",
    "ended_at": "2026-06-10T14:50:44Z",
    "duration_seconds": 1123,
    "outcome": "success",
    "summary": "Refactored auth.py JWT validation to use PyJWT 2.x API. Added refresh token rotation. All 12 tests passing.",
    "entities": ["src/tag/auth.py", "tests/test_auth.py", "requirements.txt", "PyJWT"],
    "key_events_count": 14,
    "created_at": "2026-06-10T14:51:10Z"
  }
]
```

### 7.2 `tag mem episode show`

Show full detail for a single episode.

```
tag mem episode show <episode-id>
  [--json]
```

**Human-readable output example:**
```
EPISODE  ep-a1b2c3d4
──────────────────────────────────────────────────────────────────────────────
Profile:    coder
Run ID:     run-abc123
Started:    2026-06-10 14:32:01 UTC
Ended:      2026-06-10 14:50:44 UTC
Duration:   18m 43s
Outcome:    success

SUMMARY
  Refactored auth.py JWT validation to use PyJWT 2.x API. Added refresh token
  rotation. All 12 tests passing. Key change: replaced jwt.decode() positional
  algorithm arg with algorithms=["HS256"] keyword.

ENTITIES TOUCHED (6)
  src/tag/auth.py
  tests/test_auth.py
  requirements.txt
  requirements-dev.txt
  PyJWT (library)
  jwt (module)

KEY EVENTS (14)
  1. [tool:read_file]         Read src/tag/auth.py (1,847 tokens)
  2. [tool:bash]              Run: pytest tests/test_auth.py → 3 failures
  3. [decision]               Identified root cause: deprecated jwt.decode() positional algorithm arg
  4. [tool:str_replace_editor] Patched jwt.decode() call in auth.py line 112
  5. [tool:bash]              Run: pip install 'PyJWT>=2.8'
  6. [tool:str_replace_editor] Updated requirements.txt PyJWT pin to >=2.8,<3
  7. [tool:bash]              Run: pytest tests/test_auth.py → all 12 passing
  8. [tool:read_file]         Read tests/test_auth.py (checked refresh token coverage)
  9. [tool:str_replace_editor] Added refresh token rotation test case
  10. [tool:bash]             Run: pytest tests/ -k auth → 12 pass, 0 fail
  11. [tool:bash]             Run: git diff --stat → 3 files changed
  12. [decision]              Decided not to pin jwt to exact version (semver safe)
  13. [tool:bash]             Run: git add -p → staged 3 hunks
  14. [outcome]               Session complete: auth module PyJWT 2.x migration done

NOTES
  (none)
──────────────────────────────────────────────────────────────────────────────
```

### 7.3 `tag mem episode search`

Full-text or semantic search over stored episodes.

```
tag mem episode search "<query>"
  [--profile <profile>]
  [--top-k <N>]
  [--semantic]
  [--outcome success|failure|partial|unknown]
  [--format-for-context]
  [--json]
```

**Options:**
- `<query>`: Required. Free-text search query. Used for FTS5 MATCH by default; if `--semantic` is set, also used as embedding query.
- `--profile`: Scope search to a specific profile.
- `--top-k`: Maximum results to return. Default: 5.
- `--semantic`: Enable embedding-based semantic search using `tool_retrieval.py`'s SentenceTransformer model (`all-MiniLM-L6-v2`). When set, computes cosine similarity between the query embedding and stored episode embeddings. Results are ranked by hybrid score: `0.6 * semantic_similarity + 0.4 * fts_rank_normalized`.
- `--outcome`: Pre-filter by outcome before ranking.
- `--format-for-context`: Output a context-window-ready text block suitable for pasting into a system prompt. Includes episode summaries with ISO timestamps, outcome labels, and entity lists but omits raw key events.
- `--json`: Output JSON array of ranked episode objects with `score` field.

**Human-readable output (default):**
```
EPISODE SEARCH  query="auth refactor"  top-k=3
─────────────────────────────────────────────────────────────────────────────
RANK  SCORE  ID           STARTED              OUTCOME   SUMMARY
1     0.91   ep-a1b2c3d4  2026-06-10 14:32:01  success   Refactored auth.py JWT validation...
2     0.78   ep-b3c4d5e6  2026-05-28 11:15:44  partial   Explored OAuth2 PKCE flow for auth module...
3     0.64   ep-f0a1b2c3  2026-05-15 09:02:11  failure   Attempted to extract auth.py into standalone package...
─────────────────────────────────────────────────────────────────────────────
```

**`--format-for-context` output:**
```
=== PAST RELEVANT EPISODES (3) ===

[ep-a1b2c3d4 | 2026-06-10 | success | coder]
Refactored auth.py JWT validation to use PyJWT 2.x API. Added refresh token
rotation. All 12 tests passing. Entities: src/tag/auth.py, tests/test_auth.py,
requirements.txt, PyJWT.

[ep-b3c4d5e6 | 2026-05-28 | partial | coder]
Explored OAuth2 PKCE flow for auth module. Implemented authorization URL
generator but did not complete token exchange handler. Entities: src/tag/auth.py,
src/tag/oauth.py, httpx.

[ep-f0a1b2c3 | 2026-05-15 | failure | coder]
Attempted to extract auth.py into standalone package. Blocked by 14 circular
imports with controller.py. Decision: defer extraction to post-refactor phase.
Entities: src/tag/auth.py, src/tag/controller.py, pyproject.toml.

=== END PAST EPISODES ===
```

### 7.4 `tag mem episode create`

Create a new episode. The primary path is `--from-run`, which auto-generates the episode from an existing run's steps using an LLM extraction call.

```
tag mem episode create
  --from-run <run-id>
  [--profile <profile>]
  [--model <model-id>]
  [--summary <text>]
  [--outcome success|failure|partial|unknown]
  [--notes <text>]
  [--dry-run]
  [--json]
```

**Options:**
- `--from-run`: Required. The `run_id` from the `runs` table to extract an episode from. The command reads all `steps` rows for this run, constructs the extraction prompt, and calls the configured model.
- `--profile`: Profile to associate with this episode. Defaults to the profile stored in the run record.
- `--model`: LLM model to use for extraction. Defaults to `episode.extraction_model` in `cli-config.yaml`, then to `anthropic/claude-haiku-3-5` (cheap, fast).
- `--summary`: Override the LLM-generated summary with a manually provided one.
- `--outcome`: Override the inferred outcome. When not provided, the command infers outcome from the run's exit `status` field: `completed` → `success`, `failed` → `failure`, `partial` → `partial`, others → `unknown`.
- `--notes`: Free-text notes attached to the episode (not extracted by LLM; user-provided).
- `--dry-run`: Print the extraction prompt and estimated token count without making any API call or writing to SQLite.
- `--json`: Output the created episode as JSON.

**Example output:**
```
Extracting episode from run run-abc123...
  Steps: 14 | Tokens (est.): 3,241
  Model: anthropic/claude-haiku-3-5
  Calling LLM... done (2.4 s)

Episode created: ep-a1b2c3d4
  Profile:  coder
  Outcome:  success
  Events:   14
  Entities: 6
  Summary:  Refactored auth.py JWT validation to use PyJWT 2.x API...

To inject this episode into a future session:
  tag mem episode show ep-a1b2c3d4 --format-for-context
```

### 7.5 `tag mem episode promote`

Promote an episode's key facts into `semantic_memories` for permanent retention.

```
tag mem episode promote <episode-id>
  [--profile <profile>]
  [--model <model-id>]
  [--dry-run]
  [--json]
```

**Options:**
- `--dry-run`: Print the facts that would be promoted without writing to `semantic_memories`.
- `--model`: LLM model for fact extraction. Defaults to `episode.extraction_model`.
- `--json`: Output the list of promoted memory IDs as JSON.

**Example output:**
```
Promoting facts from episode ep-a1b2c3d4...
  Extracted 4 facts:
  1. [convention] PyJWT 2.x requires algorithms=["HS256"] as keyword argument.
  2. [decision]   auth.py refresh token rotation implemented 2026-06-10.
  3. [gotcha]     jwt.decode() positional algorithm arg deprecated in PyJWT 2.0.
  4. [fact]       All auth tests pass as of commit a1b2c3d.

Stored: mem-0001, mem-0002, mem-0003, mem-0004
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | **Schema creation:** `ensure_episode_schema(conn)` must create the `memory_episodes` and `memory_episodes_fts` tables if they do not exist, using `CREATE TABLE IF NOT EXISTS` and `CREATE VIRTUAL TABLE IF NOT EXISTS` idioms consistent with `open_db()`. Schema creation must be idempotent. |
| FR-02 | **Episode fields:** Each `memory_episodes` row must store: `id` (TEXT, `ep-` prefix + 8 hex chars), `profile` (TEXT), `run_id` (TEXT nullable, FK to `runs.id`), `started_at` (TEXT ISO 8601), `ended_at` (TEXT ISO 8601 nullable), `duration_seconds` (INTEGER nullable), `outcome` (TEXT, one of `success`/`failure`/`partial`/`unknown`), `summary` (TEXT), `key_events_json` (TEXT, JSON array), `entities_json` (TEXT, JSON array of strings), `embedding_blob` (BLOB nullable), `notes` (TEXT nullable), `created_at` (TEXT ISO 8601), `source` (TEXT, `manual` or `auto`). |
| FR-03 | **FTS5 virtual table:** A `memory_episodes_fts` virtual table must index `id`, `profile`, `summary`, and the JSON-serialized content of `key_events_json` and `entities_json` (as concatenated text) using `tokenize='porter unicode61'`. The FTS table must be kept in sync with inserts and deletes via explicit `INSERT`/`DELETE` calls in `add_episode()` and `delete_episode()`. |
| FR-04 | **`tag mem episode list` filtering:** The `list` subcommand must support filtering by `--profile`, `--outcome`, `--since` (ISO date), `--until` (ISO date), and `--entity` (substring match in `entities_json`). All filters are ANDed. The default limit is 20 rows sorted by `started_at DESC`. |
| FR-05 | **`tag mem episode show` completeness:** The `show` subcommand must display all episode fields: summary, entities (parsed from JSON), key events (parsed from JSON array, numbered), outcome, duration, profile, run_id, notes, and timestamps. `--json` must output the raw database row with all fields. |
| FR-06 | **FTS5 search:** `tag mem episode search` without `--semantic` must use FTS5 MATCH over the `memory_episodes_fts` table. The query string is passed directly to `MATCH`. Results are ranked by FTS5 `rank` column (BM25 built-in). Up to `--top-k` results are returned. |
| FR-07 | **Semantic search:** When `--semantic` is provided, `search_episodes()` must: (1) compute the query embedding using `SentenceTransformer('all-MiniLM-L6-v2')` via the same model loading pattern as `tool_retrieval.py`; (2) fetch all episodes for the profile that have non-NULL `embedding_blob`; (3) deserialize each blob with `numpy.frombuffer`; (4) compute cosine similarities; (5) if FTS5 results are also available, compute a hybrid score: `0.6 * semantic_score + 0.4 * normalized_fts_rank`; (6) sort by hybrid score descending and return top-k. |
| FR-08 | **Embedding generation on create:** When `create_episode_from_run()` or `add_episode()` is called, `episodic_memory.py` must optionally compute and store an embedding of the episode summary. Embedding is enabled by default but skipped if SentenceTransformer is not installed (graceful degradation). The embedding is stored as `numpy.float32` array serialized with `array.tobytes()`. |
| FR-09 | **LLM extraction pipeline:** `create_episode_from_run(conn, run_id, cfg, ...)` must: (1) query `steps` table for all rows with matching `run_id`; (2) build `EPISODE_EXTRACTION_PROMPT` (defined in `episodic_memory.py`) with steps content; (3) call the configured model via the same subprocess/API pattern used in `eval.py`; (4) parse the structured JSON response containing `summary`, `key_events` (list of objects with `type`, `description`, `tool` optional), `entities` (list of strings), and `outcome`; (5) call `add_episode()` to persist the result. |
| FR-10 | **Outcome inference:** When `--outcome` is not provided, infer from the run's `status` field: `"completed"` maps to `"success"`, `"failed"` maps to `"failure"`, `"partial"` maps to `"partial"`, all others map to `"unknown"`. |
| FR-11 | **`--dry-run` for create:** When `--dry-run` is set, `create_episode_from_run()` must print the full extraction prompt text, the estimated input token count (character count / 4, rounded), and the selected model. No API call is made and no rows are written to SQLite. Exit code 0. |
| FR-12 | **`--format-for-context` output:** The `--format-for-context` flag in `search` produces a plain-text block bounded by `=== PAST RELEVANT EPISODES (N) ===` / `=== END PAST EPISODES ===` markers. Each episode entry includes: `[id | date | outcome | profile]` header, summary text, and entity list. No key events are included (brevity). |
| FR-13 | **Promote to semantic memory:** `promote_episode(conn, episode_id, cfg, ...)` must: (1) load the episode; (2) construct `EPISODE_PROMOTE_PROMPT` with the episode's summary and key events; (3) call the LLM to extract a JSON array of facts, each with `content` (str), `memory_type` (one of the `VALID_TYPES` from `semantic_memory.py`), and `confidence` (float); (4) call `add_memory()` from `semantic_memory.py` for each extracted fact; (5) return the list of inserted memory IDs. |
| FR-14 | **Profile scoping:** All operations default to `cfg["defaults"]["master_profile"]` when `--profile` is not specified. The `--profile` flag overrides this for the duration of the command. Episodes for profile `"*"` (global) are included in all profile-scoped queries. |
| FR-15 | **JSON output compliance:** `--json` on `list` outputs a JSON array; on `show` outputs a JSON object; on `search` outputs a JSON array with a `score` field per element; on `create` outputs the newly created episode object; on `promote` outputs `{"episode_id": "...", "promoted_memory_ids": [...]}`. All JSON outputs must be valid (parseable by `json.loads`). |
| FR-16 | **Error handling:** If `--from-run <run-id>` references a run that does not exist in the `runs` table, exit with code 1 and message: `"Run '<run-id>' not found in database"`. If the LLM extraction call fails or returns malformed JSON, exit with code 1 and message: `"Episode extraction failed: <reason>"`. Never write a partial episode. |
| FR-17 | **Idempotent schema migration:** `ensure_episode_schema()` must be callable multiple times without error. Adding the `embedding_blob` column via `ALTER TABLE` (for databases that already have the base schema) must use a try/except `OperationalError` guard, consistent with `_migrate_runs_cost_columns()` in `controller.py`. |
| FR-18 | **`--entity` filter correctness:** The `--entity` filter in `list` must match against the parsed `entities_json` array (not raw substring match on JSON text). An entity value matches if any element in the parsed list contains the filter string (case-insensitive). |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Zero new mandatory dependencies:** `tag mem episode list`, `show`, and FTS5 `search` must work with no packages beyond what TAG already requires. SentenceTransformer (for `--semantic`) and an LLM API key (for `create`) are optional; their absence produces a clear error message, not an import crash. |
| NFR-02 | **WAL-mode SQLite safety:** All writes to `memory_episodes` and `memory_episodes_fts` must use the existing `open_db()` connection (WAL mode, `busy_timeout=5000`). Long-running LLM extraction calls must not hold the database connection open during the network round-trip. The pattern is: open DB → read steps → close or release → call LLM → reopen DB → write episode. |
| NFR-03 | **TTY vs pipe rendering:** When stdout is a TTY and `rich` is available via `tui_output.py`, `list` and `search` output is rendered as a Rich table. When piped or `--json` is set, plain JSON is emitted. This matches the convention in `cmd_memory_semantic`. |
| NFR-04 | **Embedding lazy loading:** The SentenceTransformer model (`all-MiniLM-L6-v2`, ~23 MB) is only loaded when `--semantic` is requested or when `create` is invoked with embedding enabled. It must not be imported at module load time. Use the same `_get_model()` lazy-singleton pattern from `tool_retrieval.py`. |
| NFR-05 | **Token budget for extraction:** The `EPISODE_EXTRACTION_PROMPT` must include a guard: if the concatenated steps text exceeds 80,000 characters (~20,000 tokens), the steps are truncated to the first 40,000 characters plus the last 10,000 characters (preserving beginning context and final outcome) with an ellipsis marker inserted. This prevents exceeding model context limits. |
| NFR-06 | **Atomic episode writes:** `add_episode()` must wrap all `INSERT` statements (main table + FTS table) in a single `conn.execute` block and call `conn.commit()` once. If the FTS insert fails, the main table insert must be rolled back (use a `try/except` with explicit `conn.rollback()`). |
| NFR-07 | **Graceful FTS5 degradation:** If FTS5 is not available in the SQLite build (rare but possible), `search_episodes()` must fall back to `LIKE '%query%'` on the `summary` and `key_events_json` columns. The fallback is logged with a `print_warning()` call. |
| NFR-08 | **LLM extraction cost transparency:** Before making the extraction API call, `create_episode_from_run()` must print: `"Estimated tokens: ~N | Model: <model-id> | Press Ctrl+C to abort"` to stderr. This line is suppressed when `--json` is set. |
| NFR-09 | **Reproducible episode IDs:** Episode IDs use the format `ep-` + `uuid.uuid4().hex[:8]`. The prefix ensures IDs are lexically sortable by type and do not collide with `mem-` prefixed semantic memory IDs or raw UUIDs used in `runs`. |
| NFR-10 | **Security — no shell injection:** The `--from-run` value is used only as a SQL parameter placeholder (`?`), never interpolated into SQL strings. The `--model` value is validated against an allowlist from `cli-config.yaml` or a regex `^[a-z0-9_/.-]+$` before being passed to any subprocess. |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/episodic_memory.py` | All episode business logic: schema, CRUD, extraction, search, promote |
| `tests/test_episodic_memory.py` | Unit tests for `episodic_memory.py` using `sqlite3.connect(":memory:")` |

No other new files. `controller.py` gains `cmd_episode` (routed through `cmd_memory_semantic`) and new argparse entries under the existing `mem` subparser.

### 10.2 SQLite DDL

```sql
-- Primary episode store
CREATE TABLE IF NOT EXISTS memory_episodes (
  id               TEXT PRIMARY KEY,          -- "ep-" + 8 hex chars
  profile          TEXT NOT NULL,             -- profile name or '*' for global
  run_id           TEXT,                      -- FK to runs.id (nullable: manual episodes)
  started_at       TEXT NOT NULL,             -- ISO 8601 UTC session start
  ended_at         TEXT,                      -- ISO 8601 UTC session end (nullable)
  duration_seconds INTEGER,                   -- computed: ended_at - started_at
  outcome          TEXT NOT NULL DEFAULT 'unknown',  -- success|failure|partial|unknown
  summary          TEXT NOT NULL,             -- LLM-generated or user-provided summary
  key_events_json  TEXT NOT NULL DEFAULT '[]',  -- JSON array of KeyEvent objects
  entities_json    TEXT NOT NULL DEFAULT '[]',  -- JSON array of strings
  embedding_blob   BLOB,                      -- numpy float32 array .tobytes() or NULL
  notes            TEXT,                      -- free-text user notes
  source           TEXT NOT NULL DEFAULT 'auto',  -- 'auto' | 'manual'
  created_at       TEXT NOT NULL,             -- ISO 8601 UTC write time
  FOREIGN KEY(run_id) REFERENCES runs(id)
);

CREATE INDEX IF NOT EXISTS idx_ep_profile_started
  ON memory_episodes(profile, started_at DESC);

CREATE INDEX IF NOT EXISTS idx_ep_outcome
  ON memory_episodes(outcome, profile);

CREATE INDEX IF NOT EXISTS idx_ep_run_id
  ON memory_episodes(run_id);

-- FTS5 full-text search index
-- Indexes summary + flattened key_events_json + flattened entities_json
CREATE VIRTUAL TABLE IF NOT EXISTS memory_episodes_fts
  USING fts5(
    id,
    profile,
    summary,
    events_text,    -- flattened key_events descriptions
    entities_text,  -- space-joined entities list
    tokenize='porter unicode61'
  );
```

The FTS table's `events_text` and `entities_text` columns are populated at insert time by `add_episode()` which extracts the description strings from `key_events_json` and joins `entities_json` into a space-separated string before inserting.

### 10.3 Python Dataclasses

```python
# src/tag/episodic_memory.py

from __future__ import annotations
import json
import sqlite3
import time
import uuid
from dataclasses import dataclass, field, asdict
from datetime import datetime, timezone
from typing import Any


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _ep_id() -> str:
    return "ep-" + uuid.uuid4().hex[:8]


VALID_OUTCOMES = {"success", "failure", "partial", "unknown"}


@dataclass
class KeyEvent:
    """A single notable event within an agent session."""
    type: str          # "tool", "decision", "error", "outcome", "observation"
    description: str   # Human-readable event description
    tool: str | None = None   # Tool name if type == "tool"
    timestamp: str | None = None  # ISO 8601 UTC (optional, from span data)

    def to_dict(self) -> dict[str, Any]:
        d = asdict(self)
        return {k: v for k, v in d.items() if v is not None}


@dataclass
class Episode:
    """A complete structured record of one agent session."""
    id: str = field(default_factory=_ep_id)
    profile: str = ""
    run_id: str | None = None
    started_at: str = field(default_factory=_utc_now)
    ended_at: str | None = None
    duration_seconds: int | None = None
    outcome: str = "unknown"
    summary: str = ""
    key_events: list[KeyEvent] = field(default_factory=list)
    entities: list[str] = field(default_factory=list)
    embedding: list[float] | None = None   # in-memory only; stored as BLOB
    notes: str | None = None
    source: str = "auto"
    created_at: str = field(default_factory=_utc_now)

    def __post_init__(self) -> None:
        if self.outcome not in VALID_OUTCOMES:
            raise ValueError(
                f"outcome must be one of {sorted(VALID_OUTCOMES)}, got {self.outcome!r}"
            )

    def to_row(self) -> dict[str, Any]:
        """Serialize to a dict suitable for SQLite insertion."""
        import numpy as np
        blob = None
        if self.embedding is not None:
            arr = np.array(self.embedding, dtype=np.float32)
            blob = arr.tobytes()
        return {
            "id": self.id,
            "profile": self.profile,
            "run_id": self.run_id,
            "started_at": self.started_at,
            "ended_at": self.ended_at,
            "duration_seconds": self.duration_seconds,
            "outcome": self.outcome,
            "summary": self.summary,
            "key_events_json": json.dumps(
                [e.to_dict() for e in self.key_events], ensure_ascii=False
            ),
            "entities_json": json.dumps(self.entities, ensure_ascii=False),
            "embedding_blob": blob,
            "notes": self.notes,
            "source": self.source,
            "created_at": self.created_at,
        }

    @classmethod
    def from_row(cls, row: sqlite3.Row | dict) -> "Episode":
        """Deserialize from a SQLite row."""
        import numpy as np
        d = dict(row)
        key_events_raw = json.loads(d.pop("key_events_json", "[]") or "[]")
        entities = json.loads(d.pop("entities_json", "[]") or "[]")
        blob = d.pop("embedding_blob", None)
        embedding = None
        if blob:
            arr = np.frombuffer(blob, dtype=np.float32)
            embedding = arr.tolist()
        key_events = [KeyEvent(**e) for e in key_events_raw]
        return cls(
            key_events=key_events,
            entities=entities,
            embedding=embedding,
            **{k: v for k, v in d.items() if k in cls.__dataclass_fields__},
        )


@dataclass
class EpisodeSearchResult:
    """An episode augmented with a retrieval score."""
    episode: Episode
    score: float        # 0.0–1.0 hybrid score
    fts_rank: float | None = None
    semantic_similarity: float | None = None
```

### 10.4 Core Algorithm: LLM Extraction Pipeline

```python
EPISODE_EXTRACTION_PROMPT = """\
You are an expert software engineering assistant analyzing a completed agent session.
Below are the steps from a TAG agent session (role, tool calls, outputs, decisions).
Extract a structured episode summary.

STEPS:
{steps_text}

Respond with ONLY valid JSON matching this schema:
{{
  "summary": "<1-3 sentence summary of what was accomplished or attempted>",
  "outcome": "success" | "failure" | "partial" | "unknown",
  "key_events": [
    {{
      "type": "tool" | "decision" | "error" | "outcome" | "observation",
      "description": "<concise description>",
      "tool": "<tool_name or null>"
    }}
  ],
  "entities": ["<file paths, library names, function names, URLs touched>"]
}}

Rules:
- summary: focus on what was attempted and what the result was, not on individual steps
- key_events: include 5-20 events; omit trivial read-only steps; prioritize decisions, errors, and edits
- entities: list only concretely referenced items; deduplicate; use shortest unambiguous names
- outcome: "success" = task fully completed; "partial" = some progress but incomplete; "failure" = no net progress or reverted
"""

EPISODE_PROMOTE_PROMPT = """\
You are extracting durable facts from an episodic memory for long-term storage.

EPISODE SUMMARY:
{summary}

KEY EVENTS:
{events_text}

Extract 2-6 durable facts worth storing in long-term memory.
Respond with ONLY valid JSON array:
[
  {{
    "content": "<specific, timeless fact>",
    "memory_type": "fact" | "convention" | "decision" | "gotcha",
    "confidence": 0.5-1.0
  }}
]

Rules:
- Prefer conventions, gotchas, and decisions over raw facts
- Do not extract ephemeral facts (specific error messages, transient state)
- confidence >= 0.9 for well-established conventions; 0.7-0.89 for decisions; 0.5-0.69 for uncertain facts
"""
```

### 10.5 Core Functions in `episodic_memory.py`

```python
def ensure_episode_schema(conn: sqlite3.Connection) -> None:
    """Create memory_episodes and memory_episodes_fts tables if not present."""
    ...  # DDL from Section 10.2

def add_episode(conn: sqlite3.Connection, episode: Episode) -> str:
    """Persist an episode. Returns episode ID. Atomic (main + FTS in one commit)."""
    row = episode.to_row()
    events_text = " ".join(
        e.get("description", "") for e in json.loads(row["key_events_json"])
    )
    entities_text = " ".join(json.loads(row["entities_json"]))
    try:
        conn.execute(
            """INSERT INTO memory_episodes(id, profile, run_id, started_at, ended_at,
               duration_seconds, outcome, summary, key_events_json, entities_json,
               embedding_blob, notes, source, created_at)
               VALUES(:id,:profile,:run_id,:started_at,:ended_at,:duration_seconds,
               :outcome,:summary,:key_events_json,:entities_json,:embedding_blob,
               :notes,:source,:created_at)""",
            row,
        )
        conn.execute(
            """INSERT INTO memory_episodes_fts(id, profile, summary, events_text, entities_text)
               VALUES(?,?,?,?,?)""",
            (episode.id, episode.profile, episode.summary, events_text, entities_text),
        )
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    return episode.id


def get_episode(conn: sqlite3.Connection, episode_id: str) -> Episode | None:
    """Fetch a single episode by ID. Returns None if not found."""
    row = conn.execute(
        "SELECT * FROM memory_episodes WHERE id=?", (episode_id,)
    ).fetchone()
    return Episode.from_row(row) if row else None


def list_episodes(
    conn: sqlite3.Connection,
    profile: str,
    *,
    outcome: str | None = None,
    since: str | None = None,
    until: str | None = None,
    entity_filter: str | None = None,
    limit: int = 20,
) -> list[Episode]:
    """List episodes with optional filtering. Sorted by started_at DESC."""
    ...

def search_episodes(
    conn: sqlite3.Connection,
    query: str,
    profile: str,
    *,
    top_k: int = 5,
    semantic: bool = False,
    outcome_filter: str | None = None,
) -> list[EpisodeSearchResult]:
    """FTS5 + optional semantic search. Returns ranked results."""
    ...

def create_episode_from_run(
    conn: sqlite3.Connection,
    run_id: str,
    cfg: dict,
    *,
    model: str | None = None,
    override_summary: str | None = None,
    override_outcome: str | None = None,
    notes: str | None = None,
    dry_run: bool = False,
    compute_embedding: bool = True,
) -> Episode:
    """Extract and persist a structured episode from a completed run."""
    ...

def promote_episode(
    conn: sqlite3.Connection,
    episode_id: str,
    cfg: dict,
    *,
    model: str | None = None,
    dry_run: bool = False,
) -> list[str]:
    """Extract durable facts from an episode into semantic_memories. Returns memory IDs."""
    ...

def delete_episode(conn: sqlite3.Connection, episode_id: str, profile: str) -> bool:
    """Delete an episode and its FTS entry. Returns True if deleted."""
    ...

def episode_stats(conn: sqlite3.Connection, profile: str) -> dict:
    """Return aggregate statistics for a profile's episode store."""
    ...
```

### 10.6 Hybrid Search Implementation Detail

The semantic search path in `search_episodes()`:

```python
def _hybrid_search(
    conn: sqlite3.Connection,
    query: str,
    profile: str,
    top_k: int,
    outcome_filter: str | None,
) -> list[EpisodeSearchResult]:
    import numpy as np
    from tag.tool_retrieval import _get_model  # lazy singleton

    # 1. FTS5 pass (BM25)
    fts_results: dict[str, float] = {}
    try:
        rows = conn.execute(
            """SELECT id, rank FROM memory_episodes_fts
               WHERE summary MATCH ? AND profile=?
               ORDER BY rank LIMIT 50""",
            (query, profile),
        ).fetchall()
        if rows:
            min_rank = min(r[1] for r in rows)
            max_rank = max(r[1] for r in rows)
            span = max_rank - min_rank or 1.0
            for r in rows:
                fts_results[r[0]] = 1.0 - (r[1] - min_rank) / span  # normalize 0-1
    except Exception:
        pass  # FTS5 not available; fall through to semantic only

    # 2. Semantic pass (cosine similarity)
    model = _get_model()
    query_vec = model.encode([query])[0]
    query_norm = query_vec / (np.linalg.norm(query_vec) + 1e-9)

    candidate_rows = conn.execute(
        "SELECT id, embedding_blob FROM memory_episodes WHERE profile=? AND embedding_blob IS NOT NULL",
        (profile,),
    ).fetchall()

    sem_results: dict[str, float] = {}
    for r in candidate_rows:
        ep_vec = np.frombuffer(r[1], dtype=np.float32)
        ep_norm = ep_vec / (np.linalg.norm(ep_vec) + 1e-9)
        sem_results[r[0]] = float(np.dot(query_norm, ep_norm))

    # 3. Hybrid fusion: 0.6 * semantic + 0.4 * fts (if both available)
    all_ids = set(fts_results) | set(sem_results)
    scores: dict[str, float] = {}
    for ep_id in all_ids:
        f = fts_results.get(ep_id, 0.0)
        s = sem_results.get(ep_id, 0.0)
        if fts_results and sem_results:
            scores[ep_id] = 0.6 * s + 0.4 * f
        elif fts_results:
            scores[ep_id] = f
        else:
            scores[ep_id] = s

    ranked_ids = sorted(scores, key=lambda x: scores[x], reverse=True)[:top_k]
    # Fetch full episodes, apply outcome filter, build results
    results = []
    for ep_id in ranked_ids:
        ep = get_episode(conn, ep_id)
        if ep is None:
            continue
        if outcome_filter and ep.outcome != outcome_filter:
            continue
        results.append(EpisodeSearchResult(
            episode=ep,
            score=scores[ep_id],
            fts_rank=fts_results.get(ep_id),
            semantic_similarity=sem_results.get(ep_id),
        ))
    return results
```

### 10.7 Controller Integration

In `controller.py`, the `mem` subparser gains an `episode` subparser. The routing pattern follows the existing `mem_subcommand` dispatch used in `cmd_memory_semantic`:

```python
# In cmd_memory_semantic(), add at the top of the dispatch block:

if sub == "episode":
    ep_sub = getattr(args, "episode_subcommand", "list")
    try:
        from tag.episodic_memory import (
            ensure_episode_schema, list_episodes, get_episode,
            search_episodes, create_episode_from_run, promote_episode,
        )
    except ImportError as exc:
        db.close()
        print_error(f"tag.episodic_memory not available: {exc}")
        return 1
    ensure_episode_schema(db)
    return _dispatch_episode_subcommand(ep_sub, args, db, cfg, profile)
```

The argparse additions under the `mem` subparser:

```python
ep_cmd = mem_sub.add_parser("episode", help="Episodic memory: structured session episodes")
ep_sub_p = ep_cmd.add_subparsers(dest="episode_subcommand")

ep_list = ep_sub_p.add_parser("list", help="List stored episodes")
ep_list.add_argument("--profile")
ep_list.add_argument("--outcome", choices=["success", "failure", "partial", "unknown"])
ep_list.add_argument("--since", metavar="ISO_DATE")
ep_list.add_argument("--until", metavar="ISO_DATE")
ep_list.add_argument("--entity", metavar="ENTITY")
ep_list.add_argument("--limit", type=int, default=20)
ep_list.add_argument("--json", action="store_true")

ep_show = ep_sub_p.add_parser("show", help="Show full episode detail")
ep_show.add_argument("episode_id", metavar="EPISODE_ID")
ep_show.add_argument("--json", action="store_true")

ep_search = ep_sub_p.add_parser("search", help="Search episodes by text or semantic query")
ep_search.add_argument("query", metavar="QUERY")
ep_search.add_argument("--profile")
ep_search.add_argument("--top-k", type=int, default=5, dest="top_k")
ep_search.add_argument("--semantic", action="store_true")
ep_search.add_argument("--outcome", choices=["success", "failure", "partial", "unknown"])
ep_search.add_argument("--format-for-context", action="store_true", dest="format_for_context")
ep_search.add_argument("--json", action="store_true")

ep_create = ep_sub_p.add_parser("create", help="Create an episode from a run")
ep_create.add_argument("--from-run", required=True, metavar="RUN_ID", dest="from_run")
ep_create.add_argument("--profile")
ep_create.add_argument("--model", metavar="MODEL_ID")
ep_create.add_argument("--summary", metavar="TEXT")
ep_create.add_argument("--outcome", choices=["success", "failure", "partial", "unknown"])
ep_create.add_argument("--notes", metavar="TEXT")
ep_create.add_argument("--dry-run", action="store_true", dest="dry_run")
ep_create.add_argument("--json", action="store_true")

ep_promote = ep_sub_p.add_parser("promote", help="Promote episode facts to semantic memory")
ep_promote.add_argument("episode_id", metavar="EPISODE_ID")
ep_promote.add_argument("--profile")
ep_promote.add_argument("--model", metavar="MODEL_ID")
ep_promote.add_argument("--dry-run", action="store_true", dest="dry_run")
ep_promote.add_argument("--json", action="store_true")

for ep in [ep_cmd, ep_list, ep_show, ep_search, ep_create, ep_promote]:
    ep.set_defaults(func=cmd_memory_semantic)
```

### 10.8 `cli-config.yaml` Configuration Keys

```yaml
# Optional section in cli-config.yaml
episode:
  extraction_model: anthropic/claude-haiku-3-5   # model for create --from-run
  promote_model: anthropic/claude-haiku-3-5       # model for promote
  auto_embed: true                                # compute embeddings on create (requires sentence-transformers)
  max_steps_chars: 80000                          # token budget guard (chars)
  truncation_head_chars: 40000                    # chars kept from start of steps
  truncation_tail_chars: 10000                    # chars kept from end of steps
```

---

## 11. Security Considerations

1. **SQL injection prevention.** All user-supplied values (`--from-run`, `--profile`, `--query`, `--entity`, `--since`, `--until`, `--outcome`) are passed exclusively via SQLite parameterized queries (`?` placeholders). No string interpolation into SQL is permitted in `episodic_memory.py`.

2. **Model ID validation.** The `--model` flag value is validated against the regex `^[a-z0-9_/.-]{1,100}$` before use. If it does not match, the command exits 1 with a descriptive error. This prevents shell injection if the model ID is ever passed to a subprocess.

3. **Episode content sanitization.** LLM-extracted `summary`, `key_events`, and `entities` are treated as untrusted strings: they are stored as-is in SQLite (safe) but are HTML-escaped when rendered in any web UI context (future). They are never executed, eval'd, or passed to a shell.

4. **Embedding blob integrity.** The `embedding_blob` is deserialized with `numpy.frombuffer(..., dtype=np.float32)`. The dtype is hardcoded; no user-controlled dtype is accepted. An unexpected blob length (not a multiple of 4 bytes) is caught with a `ValueError` and the embedding is treated as `None`.

5. **LLM response validation.** The JSON response from the extraction LLM is parsed with `json.loads()` wrapped in `try/except`. The response must match the expected schema (presence of `summary`, `outcome`, `key_events` list, `entities` list). Any schema violation causes extraction to fail with a clear error message — no partial data is written to the database.

6. **File path exposure in entities.** Episode entities may contain file paths from the user's filesystem. These are stored in the local SQLite database and are not transmitted outside the local machine. If episodes are ever exported (future feature), entities should be filtered through the same path-sanitization logic as `diff_context.py`.

7. **Prompt injection in steps text.** Steps text from the `steps` table is inserted into the LLM extraction prompt. This text may contain adversarially crafted content from prior agent runs or tool outputs. The extraction prompt is designed to be purely extractive (no code execution, no shell access). The extraction model is instructed to return only structured JSON; non-JSON output is rejected. This does not fully eliminate prompt injection risk but limits the blast radius to malformed extraction results (which are validated before storage).

8. **WAL-mode concurrent access.** SQLite WAL mode allows concurrent readers but only one writer. All write paths (`add_episode`, `delete_episode`) use `conn.execute` with `conn.commit()` and do not hold write locks during the LLM API call. This prevents timeout-induced data loss in concurrent `tag` invocations.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_episodic_memory.py`)

All unit tests use `sqlite3.connect(":memory:")` and call `ensure_episode_schema()` before each test. No LLM API calls; extraction is mocked.

| Test | Description |
|------|-------------|
| `test_schema_idempotent` | Call `ensure_episode_schema()` twice; assert no error. |
| `test_add_and_get_episode` | Add an episode; retrieve by ID; assert all fields match. |
| `test_add_episode_invalid_outcome` | Assert `ValueError` for outcome not in VALID_OUTCOMES. |
| `test_list_by_profile` | Add episodes for two profiles; assert `list_episodes(profile="coder")` returns only coder episodes. |
| `test_list_by_outcome` | Add success and failure episodes; assert `--outcome success` filters correctly. |
| `test_list_by_since_until` | Add episodes spanning 10 days; assert `--since`/`--until` date bounds. |
| `test_list_entity_filter` | Add episodes with different entity sets; assert `--entity src/tag/auth.py` returns only matching episodes. |
| `test_fts5_search` | Add 5 episodes with distinct summaries; assert `search_episodes("jwt token")` returns the correct episode first. |
| `test_fts5_search_no_results` | Assert `search_episodes("xyzzy_never_mentioned")` returns empty list. |
| `test_fts5_degradation` | Mock `CREATE VIRTUAL TABLE` to fail; assert `search_episodes()` falls back to LIKE and returns results. |
| `test_episode_from_row_roundtrip` | Serialize an Episode via `to_row()`; deserialize via `from_row()`; assert equality. |
| `test_embedding_blob_roundtrip` | Add episode with embedding; retrieve; assert numpy array matches within 1e-6. |
| `test_embedding_blob_bad_length` | Store a blob of 7 bytes (not multiple of 4); assert `from_row()` returns `embedding=None` without crash. |
| `test_delete_episode` | Add then delete an episode; assert `get_episode()` returns None and FTS no longer returns it. |
| `test_atomic_write_failure` | Mock FTS insert to raise; assert main table row is also absent after failure. |
| `test_episode_stats` | Add 3 success, 2 failure episodes; assert `episode_stats()` returns correct counts and outcome distribution. |
| `test_promote_dry_run` | Call `promote_episode(dry_run=True)`; assert no rows in `semantic_memories`. |

### 12.2 Integration Tests (`tests/test_prd_features.py`)

These tests run against a real SQLite file in a temp directory. LLM calls are mocked via `unittest.mock.patch`.

| Test | Description |
|------|-------------|
| `test_create_episode_from_run` | Insert a run + 10 steps into temp DB; mock LLM response; call `create_episode_from_run()`; assert episode written with correct profile and outcome. |
| `test_create_episode_dry_run` | Assert no episode written when `dry_run=True`; assert extraction prompt printed to stderr. |
| `test_search_returns_json` | Create 3 episodes; call `tag mem episode search "query" --json`; assert valid JSON array with `score` fields. |
| `test_list_json_all_fields` | Create episode; run `tag mem episode list --json`; parse JSON; assert all required fields present. |
| `test_promote_writes_semantic_memories` | Create episode; mock LLM promote response; run `promote_episode()`; assert `semantic_memories` rows created. |
| `test_format_for_context_output` | Run `tag mem episode search "query" --format-for-context`; assert output contains `=== PAST RELEVANT EPISODES` marker and episode summaries. |
| `test_missing_run_id_error` | Run `tag mem episode create --from-run nonexistent`; assert exit code 1 and error message. |
| `test_schema_migration_existing_db` | Create DB without `embedding_blob`; call `ensure_episode_schema()`; assert column exists after migration. |

### 12.3 Performance Tests

These run only in CI with `pytest -m perf` marker. They require a populated database fixture.

| Test | Description |
|------|-------------|
| `test_list_10k_episodes_latency` | Populate 10,000 episodes; time `list_episodes(limit=20)`; assert < 100 ms. |
| `test_fts5_search_latency` | 10,000 episodes; time `search_episodes("auth refactor")`; assert < 200 ms. |
| `test_semantic_search_latency` | 1,000 episodes with embeddings; time `search_episodes(..., semantic=True)`; assert < 500 ms. |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag mem episode list --profile coder --json` outputs a valid JSON array (parseable by `json.loads`) with the correct profile field. | `pytest test_list_json_all_fields` |
| AC-02 | `tag mem episode show <id> --json` outputs a JSON object with all 14 schema fields present and non-null where required. | `pytest test_episode_from_row_roundtrip` |
| AC-03 | `tag mem episode search "auth refactor" --top-k 3` returns at most 3 results and the most relevant episode appears first based on FTS5 BM25 rank. | `pytest test_fts5_search` |
| AC-04 | `tag mem episode search "auth refactor" --top-k 3 --semantic` returns results using cosine similarity scoring when embeddings are present. | `pytest test_semantic_search_latency` |
| AC-05 | `tag mem episode create --from-run <run-id>` creates one row in `memory_episodes` and the FTS table is updated such that the episode is findable via `search`. | `pytest test_create_episode_from_run` |
| AC-06 | `tag mem episode create --from-run <nonexistent-id>` exits with code 1 and prints `Run '<id>' not found in database` to stderr. | `pytest test_missing_run_id_error` |
| AC-07 | `tag mem episode create --from-run <id> --dry-run` prints the extraction prompt and estimated token count to stderr; writes zero rows to `memory_episodes`. | `pytest test_create_episode_dry_run` |
| AC-08 | `tag mem episode promote <id>` creates one or more rows in `semantic_memories` with valid `memory_type` values from `VALID_TYPES`. | `pytest test_promote_writes_semantic_memories` |
| AC-09 | `tag mem episode search "query" --format-for-context` outputs a text block starting with `=== PAST RELEVANT EPISODES` and ending with `=== END PAST EPISODES ===`. | `pytest test_format_for_context_output` |
| AC-10 | `tag mem episode list --outcome failure --profile coder` returns only episodes with `outcome == "failure"`. | `pytest test_list_by_outcome` |
| AC-11 | `tag mem episode list --since 2026-06-01 --until 2026-06-30` returns only episodes with `started_at` in June 2026. | `pytest test_list_by_since_until` |
| AC-12 | `tag mem episode list --entity src/tag/auth.py` returns only episodes whose `entities_json` contains `"src/tag/auth.py"`. | `pytest test_list_entity_filter` |
| AC-13 | Calling `ensure_episode_schema()` twice on the same connection raises no error. | `pytest test_schema_idempotent` |
| AC-14 | All four `tag mem episode` subcommands complete within their NFR latency bounds under the defined load scenarios. | `pytest -m perf` |
| AC-15 | Running `tag mem episode create` with a malformed LLM JSON response (mocked) exits with code 1 and does not write any row to `memory_episodes`. | `pytest test_create_episode_from_run` with bad-response mock |
| AC-16 | `tag mem episode` is listed in `tag mem --help` output. | Manual / `--help` test |

---

## 14. Dependencies

| Dependency | Type | Version | Reason | Optional? |
|------------|------|---------|--------|-----------|
| `sqlite3` (stdlib) | Runtime | Python 3.10+ built-in | Primary storage, FTS5 | No |
| `json` (stdlib) | Runtime | Python 3.10+ built-in | Serialization of `key_events_json`, `entities_json` | No |
| `uuid` (stdlib) | Runtime | Python 3.10+ built-in | Episode ID generation | No |
| `dataclasses` (stdlib) | Runtime | Python 3.10+ built-in | `Episode`, `KeyEvent`, `EpisodeSearchResult` | No |
| `numpy` | Runtime | >=1.24 (already in TAG) | Embedding blob serialization/deserialization | Yes (skipped if absent) |
| `sentence-transformers` | Runtime | >=2.7 (already pulled by `tool_retrieval.py`) | Semantic search embeddings | Yes (only for `--semantic` and auto-embed) |
| LLM API key | Runtime | — | `create --from-run` and `promote` extraction calls | Yes (only for create/promote) |
| PRD-002 memory journal | Design | — | `open_db()` pattern; `memory_journal` table design reference | No |
| PRD-013 tracing | Design | — | `runs` / `steps` table schema (source data for `--from-run`) | No |
| PRD-025 semantic memory | Code | — | `semantic_memory.py` (imported by `promote_episode()`) | No |
| `rich` / `tui_output.py` | Runtime | PRD-003 | TTY table rendering | Yes (degrades to plain text) |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should `tag run` automatically trigger episode creation after each run completes? This would require a post-run hook (see PRD-016 webhook triggers). The risk is unexpected LLM API calls; the benefit is zero-friction episodic capture. | Product | Before Phase 2 |
| OQ-02 | What is the right truncation strategy for very long runs (e.g., 200-step autonomous loops)? The current design takes head+tail. An alternative is to sample representative steps by type (prefer decisions, errors, outcomes over tool reads). | Engineering | Phase 1 implementation |
| OQ-03 | Should `entities` be normalized (e.g., `src/tag/auth.py` vs `./auth.py` vs `auth.py`)? Normalization improves `--entity` filter precision but requires knowing the repo root. | Engineering | Before Phase 2 |
| OQ-04 | Should episode embeddings use the same `all-MiniLM-L6-v2` model as `tool_retrieval.py` or a task-specific model fine-tuned on software engineering summaries? The former requires no new download; the latter may improve recall. | ML | Follow-on PRD |
| OQ-05 | Is `sqlite3` FTS5 available in all target environments (particularly the embedded Python in some Hermes distributions)? Fallback to LIKE is planned but degrades search quality significantly. | Platform | Phase 1 validation |
| OQ-06 | Should `promote_episode` deduplicate against existing `semantic_memories` (to avoid adding a fact already stored)? This would require embedding comparison or FTS5 similarity — adding latency. The current design allows duplicates and relies on `UPDATE_MEMORY_PROMPT`-style reconciliation in a future PRD. | Product | Follow-on PRD |
| OQ-07 | What retention policy should apply to `memory_episodes`? Raw logs in `steps` are already unbounded. Episodes will be smaller but could accumulate over months. A `tag mem episode gc --older-than 90d` command may be needed. | Product | Follow-on PRD |
| OQ-08 | Should `tag mem episode list` include episodes from `runs` not yet episodized, showing them as stubs with a `[no episode]` marker? This would help users discover which sessions lack episodic records. | Product | Phase 2 |

---

## 16. Complexity and Timeline

### Phase 1 — Core Storage and CLI (Days 1–5)

| Task | Day(s) | Notes |
|------|--------|-------|
| Write `src/tag/episodic_memory.py` with `ensure_episode_schema`, `add_episode`, `get_episode`, `list_episodes`, `delete_episode`, `episode_stats` | 1–2 | No LLM, no embeddings; pure SQLite operations |
| Add FTS5 support: `memory_episodes_fts` table, FTS-based `search_episodes` (without semantic) | 2 | Include graceful LIKE fallback |
| Integrate `episode` subparser into `controller.py` under `tag mem` | 3 | Argparse additions, `_dispatch_episode_subcommand` function |
| Implement `list`, `show` CLI commands end-to-end with `--json` and TTY rendering | 3–4 | Use `tui_output.py` Rich table; follow `cmd_memory_semantic` pattern |
| Implement `search` CLI (FTS5 path only) with `--format-for-context` output | 4 | No semantic yet |
| Unit tests for schema, CRUD, FTS5, filtering | 4–5 | `tests/test_episodic_memory.py` |

### Phase 2 — LLM Extraction and Semantic Search (Days 6–9)

| Task | Day(s) | Notes |
|------|--------|-------|
| Implement `create_episode_from_run()`: steps fetch, `EPISODE_EXTRACTION_PROMPT`, LLM call, JSON parse, `add_episode` | 6–7 | Mock LLM in tests; use subprocess pattern from `eval.py` |
| Implement `--dry-run` for `create`: print prompt + token estimate, no write | 7 | |
| Implement embedding generation in `add_episode`: lazy `_get_model()`, `numpy.tobytes()` | 8 | Gated by `auto_embed` config flag |
| Implement semantic search path in `search_episodes()`: cosine similarity + hybrid fusion | 8 | |
| Integration tests: `test_create_episode_from_run`, `test_semantic_search_latency` | 9 | |

### Phase 3 — Promote and Polish (Days 10–12)

| Task | Day(s) | Notes |
|------|--------|-------|
| Implement `promote_episode()`: `EPISODE_PROMOTE_PROMPT`, LLM call, `add_memory()` calls | 10 | |
| CLI `promote` subcommand with `--dry-run` and `--json` | 10 | |
| Schema migration guard: `ALTER TABLE ADD COLUMN embedding_blob` with try/except | 11 | |
| Performance tests: 10,000-episode fixture, latency assertions | 11 | |
| `cli-config.yaml` documentation of `episode.*` keys | 12 | |
| End-to-end acceptance criteria verification | 12 | Run all AC tests; fix gaps |

**Total: 12 working days (approximately 2 calendar weeks for one engineer)**

---

*This document covers PRD-071. For the community detection and topic clustering layer over the episode entity graph, see the forthcoming PRD-072 (Episode Community Detection). For automatic post-run episode creation via hooks, see PRD-016 (Webhook Event Triggers).*
