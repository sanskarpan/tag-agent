# PRD-026: Vector-Based Tool Retrieval (`tag mcp-registry index`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (1 sprint, ~2 weeks)
**Affects:** `controller.py` (new `cmd_mcp_registry_index`, `cmd_mcp_registry_search`; patch `cmd_mcp_registry`, `cmd_shell`), new `src/tag/tool_retrieval.py`
**Dependencies (optional extras):** `chromadb`, `sentence-transformers`
**Related:** PRD-014 (MCP Server Registry), PRD-021/022 (Semantic Memory — shares ChromaDB infra)

---

## 1. Overview

TAG's MCP registry stores tool definitions for every enabled MCP server. Today, when an agent call is made, the full set of tool descriptions for the active profile is injected verbatim into the system prompt. This works well for small registries but becomes untenable at scale: a profile with 50+ tools can consume 8,000–15,000 prompt tokens on tool descriptions alone, crowding out context that matters, and eventually exceeding the model's effective context budget.

This PRD introduces **Vector-Based Tool Retrieval**: a locally-indexed, vector-similarity layer over the MCP tool registry that dynamically selects a relevant subset of tools for each agent call at query time. Before every `hermes` invocation, TAG embeds the user's query using a local `sentence-transformers` model, queries a ChromaDB collection of indexed tool descriptions, and injects only the top-K most relevant tools into the system prompt. The full registry is preserved on disk; only the retrieval window changes per call.

The design is directly analogous to LlamaIndex's `ObjectIndex` with `VectorStoreIndex`, which uses an `ObjectRetriever` to select relevant tool definitions before passing function signatures to an LLM's function-calling API. TAG applies the same pattern locally, without requiring any hosted vector service.

The feature is **transparent by default**: no CLI flags change for end-users. It is opt-in via `tag config set mcp.tool_retrieval true` and degrades gracefully to the existing full-registry injection if the index has not been built or the optional packages are absent.

---

## 2. Goals

1. **Vector-indexed tool descriptions** — Build a persistent ChromaDB collection (`tag_tools`) over all tool `name + description` fields in the MCP registry, enabling sub-50ms cosine-similarity lookup at query time.
2. **Query-time top-K retrieval** — Before each `hermes` call, embed the user's query and retrieve the K most relevant tools, replacing the full registry in the injected system prompt.
3. **Configurable K** — Expose `mcp.retrieval_top_k` (default: 8) via `tag config set`, allowing users to widen or narrow the retrieval window without rebuilding the index.
4. **Transparent integration** — No change to existing CLI invocations. The retrieval layer is invisible to the user when enabled and bypassed silently when disabled, index-missing, or when optional packages are absent.
5. **Local embeddings only** — Use `sentence-transformers/all-MiniLM-L6-v2` for all embedding operations. No API key is required; the model is downloaded once and cached in `~/.cache/tag/embeddings/`.
6. **Automatic index invalidation** — Detect changes to `mcp-registry.yaml` via mtime comparison and prompt the user to rebuild, preventing stale retrieval results after the registry changes.
7. **Inspectable retrieval** — Provide `tag mcp-registry search "<query>"` so users can see exactly which tools would be selected for a given query before running an agent call.
8. **Shared ChromaDB infrastructure with PRD-021/022** — Reuse the same ChromaDB client and persistence path established by the Semantic Memory PRD, avoiding duplicate installations.

---

## 3. Non-Goals

- **Semantic tool composition or chaining** — the retriever selects tools; it does not infer how to chain their outputs. Tool orchestration remains Hermes' responsibility.
- **Automatic tool creation or synthesis** — the system retrieves existing tool definitions; it does not generate new tools from descriptions.
- **Tool deduplication across servers** — if two MCP servers expose tools with identical names and descriptions, both will be indexed and may both appear in top-K results. Deduplication is out of scope.
- **Remote or cloud vector stores** — only local ChromaDB (`PersistentClient`) is supported in this PRD. Pinecone, Weaviate, etc. are explicitly deferred.
- **Re-ranking with a cross-encoder** — first-pass bi-encoder retrieval (sentence-transformers) is the only ranking stage in this PRD. A cross-encoder re-ranking pass is listed as an open question.
- **Automatic tool description quality improvement** — many MCP tools ship with terse or missing descriptions. Enriching those descriptions is a separate concern.

---

## 4. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer with 50+ MCP tools enabled across multiple servers | run `tag run "refactor the auth module"` without context overflow | the agent receives only the tools relevant to the task (filesystem, git, shell) rather than all 50+ tool descriptions eating my context window |
| U2 | Developer performing a code review task | have TAG automatically select `github`, `git`, and `filesystem` tools for a query like "review PR #42 and check for style violations" | the system prompt contains precisely the tools needed for that operation rather than unrelated database, calendar, or email tools |
| U3 | Developer who knows exactly which tools are needed for a task | pass `--tools github,git,filesystem` to force specific tools regardless of retrieval | I can override the retrieval layer when domain knowledge makes the correct tool set obvious |
| U4 | Developer debugging unexpected agent behavior | run `tag mcp-registry search "review github pull request" --top-k 10` | I can inspect the exact tool subset the agent would receive for a given query and validate that retrieval is working as expected |
| U5 | Developer who just added a new MCP server to their registry | receive a clear warning that the tool index is stale and run `tag mcp-registry index` to rebuild | retrieval results reflect the current registry without manual cache management |

---

## 5. Proposed CLI Surface

### 5.1 New subcommands

```
tag mcp-registry index [--force]
```
Build or rebuild the ChromaDB tool vector index from the current MCP registry. Without `--force`, skips rebuild if the index is already current (mtime of `mcp-registry.yaml` has not changed since last index). With `--force`, drops and recreates the collection unconditionally. Prints a progress summary: `Indexed N tools from M servers in X.Xs`.

```
tag mcp-registry search "<query>" [--top-k 5]
```
Embed `<query>`, query the tool index, and print the top-K retrieved tools in a formatted table showing `rank | server | tool_name | description | score`. Defaults to `--top-k 5`. Does not invoke any agent or execute any tool. Exits with code 1 if the index has not been built.

### 5.2 Config keys

```
tag config set mcp.tool_retrieval true        # enable retrieval globally (default: false)
tag config set mcp.tool_retrieval false       # disable; fall back to full registry injection
tag config set mcp.retrieval_top_k 8         # set K (default: 8)
tag config set mcp.retrieval_top_k 20        # wider window for complex multi-tool tasks
```

Both keys are written to `cli-config.yaml` under the `mcp:` stanza and are respected immediately on the next agent call with no restart required.

### 5.3 Inline override

```
tag run "review PR #42" --tools github,git,filesystem
```
The existing (or newly-introduced) `--tools` flag accepts a comma-separated list of `tool_name` values. When present, retrieval is bypassed entirely and the specified tools are used directly, looked up from the full registry. This flag is additive with per-profile tool restrictions.

---

## 6. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | A ChromaDB collection named `tag_tools` is created in the TAG persistence directory (`~/.local/share/tag/chroma/` or `XDG_DATA_HOME/tag/chroma/`). Each document in the collection represents one tool, with `document = "{tool_name}: {description}"` and metadata fields `tool_id`, `server_id`, `required_env` (comma-separated list of required environment variable names), and `tool_schema_hash` (SHA-256 of the raw tool definition for change detection). |
| FR-02 | Embeddings are computed using `sentence-transformers/all-MiniLM-L6-v2` loaded once per process and cached. The model is downloaded to `~/.cache/tag/embeddings/all-MiniLM-L6-v2/` on first use. No API key or network call is required after the initial download. |
| FR-03 | At query time, the user's prompt (or the concatenation of `system_context + user_prompt` if a system context is configured) is embedded with the same model and passed to `collection.query(n_results=K)`. The returned `tool_id` values are used to filter the full in-memory registry, producing the injected tool subset. |
| FR-04 | The index is considered stale if the mtime of `mcp-registry.yaml` is newer than the mtime of the ChromaDB collection's last-write marker file (`~/.local/share/tag/chroma/tag_tools_mtime`). When stale, TAG emits a warning to stderr: `[warn] Tool index is stale — run 'tag mcp-registry index' to rebuild.` The stale index is still used for retrieval rather than falling back to the full registry, to avoid surprise context expansion. |
| FR-05 | When `tag mcp-registry index [--force]` is run, all existing documents in `tag_tools` are deleted and replaced atomically. If `--force` is absent and the index is current (mtime matches), the command exits early with `Index is up-to-date (N tools). Use --force to rebuild.` |
| FR-06 | Retrieved tool definitions are injected transparently into the system prompt at the same injection point currently used for full-registry injection, replacing (not appending to) the full list. The prompt template and all other system prompt content remain unchanged. |
| FR-07 | K is configurable via `mcp.retrieval_top_k` (default: 8). K must be in `[1, 50]`; values outside this range are clamped with a warning. If fewer than K tools exist in the registry, all tools are returned (no error). |
| FR-08 | If the ChromaDB collection does not exist (index was never built) and `mcp.tool_retrieval = true`, TAG falls back to full-registry injection and emits a one-time warning: `[warn] Tool retrieval enabled but index not built — using full registry. Run 'tag mcp-registry index' to enable retrieval.` |
| FR-09 | If `chromadb` or `sentence-transformers` are not installed and `mcp.tool_retrieval = true`, TAG falls back to full-registry injection silently and emits a single installation hint at startup: `[info] mcp.tool_retrieval requires 'chromadb' and 'sentence-transformers'. Install with: pip install tag-agent[retrieval]`. |
| FR-10 | `tag mcp-registry search "<query>" [--top-k N]` embeds the query and prints a ranked table of matching tools without executing any agent call. The table columns are: rank, server_id, tool_name, description (truncated to 80 chars), cosine similarity score. |
| FR-11 | When `--tools tool1,tool2` is supplied on the CLI, the retrieval layer is bypassed and the named tools are loaded directly from the in-memory registry. If a named tool does not exist in the registry, TAG exits with an error listing the unknown names. |
| FR-12 | The index rebuild (`tag mcp-registry index`) emits a structured summary to stdout on completion: number of tools indexed, number of servers covered, elapsed time in seconds, and any tools skipped (e.g., tools with empty descriptions, which are stored with a placeholder `"{tool_name}: (no description)"` to ensure they are still retrievable). |
| FR-13 | Tools with `required_env` metadata are retrieved normally but, at injection time, TAG checks whether the required environment variables are set. If any required env var is absent, a per-tool warning is appended to the system prompt section for that tool: `(Note: this tool requires {VAR_NAME} which is not set in the current environment.)` |

---

## 7. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | End-to-end retrieval latency (embed query + ChromaDB lookup + filter registry) must be under 50ms on a MacBook M1 or equivalent for a collection of up to 500 tools. The embedding model must already be loaded in memory (warm path). |
| NFR-02 | The `sentence-transformers` model is loaded once per process and held in memory for the lifetime of the process. Subsequent calls to `retrieve_tools()` within the same process must not reload the model. Startup cost (cold load) is acceptable and is documented (`~300ms for all-MiniLM-L6-v2`). |
| NFR-03 | Index rebuild for 500 tools must complete in under 10 seconds on a MacBook M1, including embedding computation (batched), ChromaDB upsert, and mtime marker write. |
| NFR-04 | The tool_retrieval module must not import `chromadb` or `sentence_transformers` at module load time. Imports are deferred to function call time and wrapped in `try/except ImportError`, ensuring that TAG's startup time is unaffected when the optional packages are not installed. |
| NFR-05 | ChromaDB persistence data must not be committed to version control. The `.gitignore` must include `~/.local/share/tag/chroma/` (or the `XDG_DATA_HOME` equivalent). For project-local invocations, if `TAG_DATA_DIR` is set, the chroma path is `$TAG_DATA_DIR/chroma/`. |
| NFR-06 | The feature must not degrade performance for users who have `mcp.tool_retrieval = false` (the default). All retrieval code paths must be unreachable when the feature is disabled. |

---

## 8. Technical Design

### 8.1 New file: `src/tag/tool_retrieval.py`

```python
# Public interface (abbreviated signatures)

def build_index(registry_path: str, force: bool = False) -> IndexBuildResult:
    """
    Load mcp-registry.yaml, embed all tool name+description pairs,
    upsert into ChromaDB tag_tools collection, write mtime marker.
    Returns IndexBuildResult(n_tools, n_servers, elapsed_s, n_skipped).
    """

def retrieve_tools(query: str, top_k: int = 8) -> list[ToolDefinition]:
    """
    Embed query, query tag_tools collection, return top_k ToolDefinition
    objects filtered from the in-memory registry. Falls back to full
    registry (with warning) if index is missing or packages absent.
    """

def is_index_stale(registry_path: str) -> bool:
    """
    Compare mtime of registry_path against mtime marker file.
    Returns True if index needs rebuild.
    """

def search_tools(query: str, top_k: int = 5) -> list[SearchResult]:
    """
    Like retrieve_tools() but returns SearchResult(rank, server_id,
    tool_name, description, score) for display in 'tag mcp-registry search'.
    """
```

### 8.2 ChromaDB collection schema

**Collection name:** `tag_tools`

| Field | Type | Content |
|-------|------|---------|
| `id` | string | `"{server_id}__{tool_name}"` (unique per tool) |
| `document` | string | `"{tool_name}: {description}"` (what is embedded) |
| `metadata.tool_id` | string | canonical tool identifier |
| `metadata.server_id` | string | originating MCP server name |
| `metadata.required_env` | string | comma-separated required env var names (empty string if none) |
| `metadata.tool_schema_hash` | string | SHA-256 of the raw tool JSON for change detection |
| `metadata.indexed_at` | float | `time.time()` at index build time |

### 8.3 Index rebuild trigger logic

```
on_agent_call():
    if mcp.tool_retrieval == false:
        return full_registry_tools()
    if is_index_stale(registry_path):
        emit_warning("Tool index is stale — run 'tag mcp-registry index'")
        # use stale index anyway; do not silently expand context
    if not collection_exists():
        emit_warning("Index not built — using full registry")
        return full_registry_tools()
    return retrieve_tools(query, top_k=config.retrieval_top_k)
```

### 8.4 mtime marker

After each successful `build_index()` call, the mtime of `mcp-registry.yaml` is written as a float to `$TAG_DATA_DIR/chroma/tag_tools_mtime`. `is_index_stale()` reads this file and compares against `os.path.getmtime(registry_path)`.

### 8.5 Integration points in `controller.py`

Two integration points require patching:

1. **`cmd_mcp_registry` (pre-hermes call path)** — After resolving the effective tool list for the profile, if `mcp.tool_retrieval` is enabled, call `retrieve_tools(user_query, top_k)` and replace the tool list passed to the system prompt builder.
2. **`cmd_shell`** — Same patch point: before constructing the Hermes invocation, intercept the tool injection and replace with retrieved subset if enabled.

Both integration points must check `mcp.tool_retrieval` before any import of `tool_retrieval.py` to preserve NFR-06 (no performance impact when disabled).

### 8.6 Embedding model caching

```python
_MODEL_CACHE: dict[str, Any] = {}

def _get_model(model_name: str = "all-MiniLM-L6-v2"):
    if model_name not in _MODEL_CACHE:
        from sentence_transformers import SentenceTransformer
        _MODEL_CACHE[model_name] = SentenceTransformer(model_name)
    return _MODEL_CACHE[model_name]
```

The module-level `_MODEL_CACHE` dict ensures the model is loaded at most once per process. This is safe because `tool_retrieval.py` is imported lazily.

### 8.7 Batch embedding for index rebuild

During `build_index()`, all tool descriptions are collected into a list and passed to `model.encode(descriptions, batch_size=64, show_progress_bar=True)`. ChromaDB upsert is performed in one call per 100 documents to stay within default SQLite transaction limits.

---

## 9. Security Considerations

1. **Local index only** — The ChromaDB collection is stored in the user's local data directory (`$XDG_DATA_HOME/tag/chroma/`). No tool descriptions, embeddings, or query vectors are sent to any remote service. The feature has the same privacy posture as the rest of TAG's local-only configuration.
2. **Sanitized tool description injection** — Tool descriptions retrieved from the collection are taken from the `document` field written at index-build time, not re-read from `mcp-registry.yaml` at query time. The index-build path HTML-escapes and strips control characters from descriptions before upsert, ensuring that a malicious tool description in the registry cannot inject prompt control sequences at retrieval time.
3. **No tool execution at retrieval time** — `retrieve_tools()` and `search_tools()` are pure read operations. They embed a query and return tool definitions. No subprocess is spawned, no MCP server is contacted, and no tool handler is invoked during retrieval.
4. **Required-env metadata does not expose secret values** — The `required_env` metadata field stores only the names of required environment variables (e.g., `"GITHUB_TOKEN,GH_TOKEN"`), never their values. The warning emitted to the system prompt when a required var is absent contains only the variable name, not the value.

---

## 10. Testing Strategy

### 10.1 Embedding reproducibility

Verify that `_get_model().encode("filesystem: Read and write local files")` produces the same vector across two calls within the same process and across two separate process invocations (determinism of `all-MiniLM-L6-v2` on fixed input). Test: assert `cosine_similarity(v1, v2) > 0.9999`.

### 10.2 Top-K accuracy tests (fixture-based)

Provide a synthetic registry of 30 tools covering domains: `git`, `filesystem`, `github`, `database`, `email`, `calendar`, `web_search`, `shell`. For each of the following queries, assert the expected tools appear in the top-3 results:

| Query | Expected top-3 tools |
|-------|---------------------|
| `"review pull request and check CI status"` | `github_get_pr`, `github_list_checks`, `git_log` |
| `"read the contents of config.yaml"` | `filesystem_read_file`, `filesystem_list_dir`, `shell_exec` |
| `"send a meeting invite for tomorrow"` | `calendar_create_event`, `email_send`, `calendar_list_events` |
| `"search the web for Python packaging best practices"` | `web_search`, `web_fetch`, `filesystem_write_file` |

Tests use a `tmp_chroma_dir` pytest fixture that creates an isolated ChromaDB client per test to avoid cross-test pollution.

### 10.3 Fallback tests

- **Index missing**: assert `retrieve_tools("any query")` returns the full registry when `tag_tools` collection does not exist, and emits the expected warning to stderr.
- **Packages absent**: mock `chromadb` and `sentence_transformers` as unimportable; assert `retrieve_tools()` returns full registry with install hint.
- **K > registry size**: create a registry with 3 tools, call `retrieve_tools(query, top_k=10)`, assert exactly 3 tools are returned with no error.

### 10.4 Index rebuild tests

- **No-force, current**: build index, assert second `build_index(force=False)` returns early without re-embedding.
- **Force rebuild**: build index, modify `mcp-registry.yaml` mtime, call `build_index(force=True)`, assert all documents are re-upserted.
- **Stale detection**: build index, advance the mock mtime of `mcp-registry.yaml` by 1 second, assert `is_index_stale()` returns `True`.
- **Empty description**: include a tool with no description in the fixture registry, assert it is indexed with placeholder text and is still retrievable.

### 10.5 CLI integration tests

- `tag mcp-registry index` exits 0 and prints summary line matching `r"Indexed \d+ tools from \d+ servers"`.
- `tag mcp-registry search "git log"` exits 0 and stdout contains a table row with `git` in the server column.
- `tag mcp-registry index --force` exits 0 even when index is current.
- `tag mcp-registry search "x"` exits 1 when no index exists, with error message containing `'tag mcp-registry index'`.

---

## 11. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | With 60 tools in the registry and `mcp.retrieval_top_k = 8`, the injected system prompt contains exactly 8 tool descriptions (not 60). | Integration test: count tool stanza occurrences in captured system prompt. |
| AC-02 | `tag mcp-registry search "review pull request" --top-k 5` returns a table in which at least one of the top-3 rows has `github` in the server column. | Manual + automated test against synthetic registry. |
| AC-03 | `tag mcp-registry index` completes in under 10 seconds for a synthetic registry of 500 tools on the CI runner. | Timed pytest test with `@pytest.mark.timeout(10)`. |
| AC-04 | End-to-end retrieval latency (warm model) is under 50ms as measured by `time.perf_counter()` around the `retrieve_tools()` call for a 500-tool index. | Performance test in `tests/test_tool_retrieval.py`. |
| AC-05 | With `mcp.tool_retrieval = false` (default), the `tool_retrieval` module is never imported and `retrieve_tools()` is never called. | Assert using `sys.modules` inspection in a subprocess test. |
| AC-06 | When `chromadb` is not installed and `mcp.tool_retrieval = true`, TAG falls back to full registry injection and prints an install hint containing `pip install tag-agent[retrieval]`. | Mock `importlib` to raise `ImportError` for `chromadb`; assert fallback and message. |
| AC-07 | After modifying `mcp-registry.yaml`, running `tag run "any task"` with retrieval enabled emits a stale-index warning to stderr. | Test via mtime manipulation + subprocess capture. |
| AC-08 | `tag mcp-registry index --force` drops and recreates the collection even when the index is current, and the resulting document count equals the tool count in the registry. | Assert via `collection.count()` before and after. |
| AC-09 | `--tools github,git` bypasses retrieval entirely and injects exactly those two tools, regardless of whether the index exists. | Unit test: assert `retrieve_tools()` is not called when `--tools` is set. |
| AC-10 | Tool descriptions injected into the system prompt via the retrieval path are identical to the descriptions in `mcp-registry.yaml` (no truncation, no mutation except control-character stripping). | Diff test: compare injected description to raw registry value for a known tool. |

---

## 12. Dependencies

### 12.1 New Python packages

| Package | Version constraint | Purpose | Install target |
|---------|-------------------|---------|----------------|
| `chromadb` | `>=0.4.0` | Persistent vector store for tool descriptions | `tag-agent[retrieval]` extra |
| `sentence-transformers` | `>=2.2.0` | Local embedding model (`all-MiniLM-L6-v2`) | `tag-agent[retrieval]` extra |

Both packages are optional extras, not hard dependencies. The `[retrieval]` extra is added to `pyproject.toml` alongside the existing `[memory]` extra from PRD-021/022.

### 12.2 Shared infrastructure

**PRD-021/022 (Semantic Memory with Confidence Decay)** establishes the ChromaDB `PersistentClient` and the `sentence-transformers` embedding model as optional extras. PRD-026 shares the same client initialization path and the same model (`all-MiniLM-L6-v2`) to avoid loading two separate model instances when both features are enabled.

Concretely, `tool_retrieval.py` will call `memory_store.get_embedding_model()` if `memory_store` is available, falling back to its own local initialization otherwise. The ChromaDB client is shared via a module-level singleton in a new `src/tag/vector_store.py` helper (or extended from `memory_store.py` if PRD-022 ships first).

**PRD-014 (MCP Server Registry)** defines the `mcp-registry.yaml` format and the `tag mcp-registry` subcommand namespace. PRD-026 adds `index` and `search` subcommands to that namespace and reads tool definitions from the registry path PRD-014 establishes.

---

## 13. Open Questions

| ID | Question | Impact | Owner |
|----|----------|--------|-------|
| OQ-01 | **Embedding model sharing with PRD-022** — if both Semantic Memory and Tool Retrieval are enabled, should they share a single `SentenceTransformer` process-global instance, or should each module manage its own? Sharing reduces RAM by ~90MB but creates an ordering dependency between modules. | Architecture of `vector_store.py` shared singleton vs. independent instances. | PRD-022 author + PRD-026 author |
| OQ-02 | **Tool description quality** — many MCP servers ship tools with one-line or empty descriptions (e.g., `"Execute a shell command."`). Poor descriptions degrade retrieval precision. Should TAG provide a `tag mcp-registry enrich` command that uses the model to auto-generate richer descriptions from the tool's JSON schema? Or should this PRD accept low-quality descriptions as a given and document the limitation? | If unenriched descriptions are accepted, users with poorly-documented MCP servers may see low retrieval accuracy and abandon the feature. | Product |
| OQ-03 | **Re-ranking after initial retrieval** — bi-encoder retrieval (MiniLM) is fast but imprecise for short tool descriptions. A cross-encoder re-ranking pass over the top-20 candidates could improve precision before selecting K. Would the latency cost (~30–80ms additional) be acceptable given NFR-01 (50ms total budget)? | Retrieval accuracy vs. latency trade-off. May require a tighter NFR. | Engineering |
| OQ-04 | **Per-profile retrieval configuration** — should `mcp.tool_retrieval` and `mcp.retrieval_top_k` be configurable per profile (overridable in `profiles/researcher.yaml`) or only globally? Per-profile K would allow the `coder` profile to use a wider window (K=15) while `reviewer` uses a narrow one (K=5). | Complexity of config resolution in `controller.py`. | Product |
| OQ-05 | **Retrieval for structured multi-step tasks** — for a task like "analyze, fix, and push to GitHub", the optimal tool set changes across the three steps. Should TAG retrieve tools once (from the top-level prompt) or retrieve per-step? Per-step retrieval requires knowing step boundaries before they occur. | Requires integration with PRD-021 (multi-agent swarm) if per-step retrieval is desired. | Future PRD |

---

## 14. Complexity and Timeline

**Complexity:** M (Medium)

**Estimated timeline:** 1 sprint (~2 weeks)

| Phase | Duration | Tasks |
|-------|----------|-------|
| Setup | Day 1 | Add `chromadb`, `sentence-transformers` as optional extras in `pyproject.toml`; create `src/tag/tool_retrieval.py` skeleton; add `vector_store.py` shared client helper |
| Core index builder | Days 2–3 | Implement `build_index()`, mtime marker, batch embedding, ChromaDB upsert; write unit tests for rebuild logic |
| Retrieval path | Days 4–5 | Implement `retrieve_tools()`, fallback logic, `is_index_stale()`; write top-K accuracy tests against synthetic registry |
| CLI integration | Days 6–7 | Add `cmd_mcp_registry_index` and `cmd_mcp_registry_search` to `controller.py`; patch `cmd_mcp_registry` and `cmd_shell` integration points; add `--tools` override flag |
| Config keys | Day 8 | Wire `mcp.tool_retrieval` and `mcp.retrieval_top_k` into config reader; validate range clamping; add config tests |
| Performance validation | Day 9 | Benchmark with 500-tool synthetic registry; assert NFR-01 (50ms warm retrieval) and NFR-03 (10s rebuild); tune batch size if needed |
| QA and acceptance tests | Day 10 | Run all AC criteria; fix regressions; update `docs/prd/INDEX.md` |
