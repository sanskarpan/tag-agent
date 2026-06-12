# PRD-036: Model Fallback Chains

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** M (1 sprint, ~2 weeks)  
**Affects:** `controller.py` (`cmd_route`, fallback pre-flight in hermes dispatch), `tag.sqlite3` schema migration

---

## 1. Overview

TAG routes each task type to a named set of profiles, each profile backed by a configured model. When that primary model hits a context window overflow, a rate limit, or a provider error, the current behavior is an unhandled failure — the run aborts, the user sees a raw exception, and must intervene manually.

Model Fallback Chains gives operators a first-class way to declare: "if `claude-opus-4` overflows context, retry with `gpt-4o`; if `gpt-4o` hits a 429, retry with `deepseek/deepseek-v4`." Chains are stored per-route in SQLite, evaluated in order, and limited to three hops to prevent cascade failures. Every substitution is logged to a dedicated table at WARNING level — silent downgrade to a cheaper or less capable model is treated as a security-relevant behavior change and is never silent.

The implementation has two distinct check points: a pre-flight context check (before the Hermes call) that compares estimated prompt tokens to the primary model's `context_length` from the cached OpenRouter catalog, and a runtime error check (after a failed Hermes subprocess or HTTP call) that matches error codes to registered triggers.

---

## 2. Problem Statement

- When a model call fails mid-run (context overflow, 429, 5xx), the entire run fails with a raw error. There is no recovery path.
- Users have no way to express "fall back to a cheaper model when over context" as first-class configuration.
- Operators running unattended overnight agents have no audit trail showing which runs used a fallback model vs. the primary.
- TAG already caches `context_length` per OpenRouter model (`load_openrouter_catalog`) but nothing consumes it to prevent overflow before calling the model.
- LiteLLM and OpenRouter both support fallback semantics at the provider layer — but TAG users need TAG-side control, not dependency on provider-side configuration.

---

## 3. Goals

1. **Context-length-aware pre-flight:** Before dispatching to Hermes, estimate prompt token count and compare to the primary model's `context_length` from the OpenRouter catalog. If the prompt would overflow, select the first fallback whose `context_length` is sufficient.
2. **Rate limit handling:** Catch HTTP 429 responses from Hermes and transparently retry with the next fallback model in the chain.
3. **Configurable trigger conditions:** Each fallback hop declares which triggers activate it (`context-overflow`, `rate-limit`, `error-5xx`, `error-timeout`, `any`). A fallback only fires when its declared trigger matches the observed failure.
4. **Substitution logging:** Every fallback activation writes a row to `fallback_substitutions` (route, primary model, fallback model, trigger, run_id, tokens at trigger, timestamp) and emits a WARNING-level log line. Silent substitution is never permitted.
5. **Chain depth limiting:** Maximum three hops per run. Attempting a fourth hop logs an error and fails the run rather than entering unbounded recursion.
6. **Per-route fallback configuration:** Fallback chains are stored per named route (keyed by route name + profile name), allowing different fallback strategies for `coding` vs. `research` task types.

---

## 4. Non-Goals

- **Load balancing:** Fallback chains activate only on failure, not to distribute load across equivalent models.
- **Automatic model selection without configuration:** TAG will not auto-discover or auto-select fallback models. All chains must be explicitly configured by the operator.
- **Provider health monitoring:** TAG does not maintain a background health-check loop for providers. The runtime check fires on observed failure only.
- **Cross-provider token counting accuracy:** Pre-flight token estimation uses a character-based heuristic (4 chars ≈ 1 token) or the `tiktoken` library if available. Exact provider-specific tokenizers are out of scope.
- **Fallback during streaming:** Fallback applies only to complete call failures, not mid-stream truncation.
- **Automatic chain generation:** TAG will not generate fallback chains from benchmarks or cost data automatically (see PRD-017).

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Operator | run `tag route fallback add --route coding --primary anthropic/claude-opus-4 --fallback openai/gpt-4o --on context-overflow,rate-limit` | My coding agent never hard-fails when the opus context window fills up |
| U2 | Developer | run `tag route fallback history --route coding --last 20` | I can audit which runs actually hit a fallback and why, and verify no unexpected capability downgrade occurred |
| U3 | Operator | configure a three-hop chain: `claude-opus-4 → gpt-4o → deepseek/deepseek-v4` with different triggers per hop | Critical overnight runs always have two escape hatches before failing |
| U4 | Developer | run `tag route fallback test --route coding --trigger context-overflow` as a dry-run | I validate my fallback chain is correctly configured before running production tasks |
| U5 | Developer | run `tag route fallback list` | I see the full fallback chain for every route in one view, including which trigger conditions each hop handles |
| U6 | Operator | run `tag route list` and see fallback chains inline next to each route | I do not need to run a separate sub-command to understand the full routing topology |

---

## 6. Proposed CLI Surface

### 6.1 `tag route fallback add`

```
tag route fallback add \
  --route <name> \
  --primary <provider/model> \
  --fallback <provider/model> \
  --on <trigger>[,<trigger>...]
```

Appends a fallback hop to the chain for the given route and primary model. If a chain already exists for this route/primary pair, the new hop is appended in order. Rejects if adding this hop would create a cycle or exceed 3 hops.

Triggers accepted: `context-overflow`, `rate-limit`, `error-5xx`, `error-timeout`, `any`.

Example:

```
tag route fallback add --route coding --primary anthropic/claude-opus-4 \
  --fallback openai/gpt-4o --on context-overflow,rate-limit

tag route fallback add --route coding --primary openai/gpt-4o \
  --fallback openrouter/deepseek/deepseek-v4 --on any
```

### 6.2 `tag route fallback list`

```
tag route fallback list [--route <name>] [--json]
```

Lists all configured fallback chains. Without `--route`, shows all routes. Output format:

```
coding  (master: researcher)
  1. anthropic/claude-opus-4  --[context-overflow,rate-limit]-->  openai/gpt-4o
  2. openai/gpt-4o            --[any]-->                          openrouter/deepseek/deepseek-v4
```

### 6.3 `tag route fallback remove`

```
tag route fallback remove --route <name> [--primary <provider/model>] [--all]
```

Removes fallback configuration. With `--primary`, removes only the chain originating from that primary model. With `--all`, removes all fallback chains for the route. Requires confirmation unless `--yes` is passed.

### 6.4 `tag route fallback test`

```
tag route fallback test --route <name> --trigger <trigger> [--prompt-tokens <n>]
```

Dry-run that simulates trigger firing and prints which fallback model would be selected and why. Does not make any API calls. Useful for CI validation of routing config.

Example output:

```
[DRY RUN] Route: coding
Trigger: context-overflow
Primary: anthropic/claude-opus-4  (context_length: 200000)
Simulated prompt tokens: 210000  (exceeds limit by 10000)
Fallback selected: openai/gpt-4o  (context_length: 128000)
  -> WARNING: simulated prompt also exceeds gpt-4o context (128000 < 210000)
  -> Next hop: openrouter/deepseek/deepseek-v4  (context_length: 65536)
  -> WARNING: simulated prompt also exceeds deepseek context (65536 < 210000)
  -> Chain exhausted after 2 hops. Run would FAIL.
```

### 6.5 `tag route fallback history`

```
tag route fallback history [--route <name>] [--last <n>] [--json]
```

Shows recent fallback activation records from `fallback_substitutions`. Default: last 20 rows ordered by timestamp descending.

Columns: `timestamp`, `route`, `run_id`, `primary_model`, `fallback_model`, `trigger`, `tokens_at_trigger`.

### 6.6 Extended `tag route list`

`tag route list` (not yet implemented; added as part of this PRD) shows all task types with their master/worker/verifier assignments and their fallback chains. This is a read-only summary command.

---

## 7. Functional Requirements

### FR-01 Fallback chain storage

Fallback chains are stored as a JSON array in a new `fallback_chain` column on the `routes` table (see schema migration in section 8). Each element is an object: `{"model": "provider/model-id", "triggers": ["context-overflow", "rate-limit"]}`. The array is ordered — index 0 is the first fallback, index 1 is the second, etc.

### FR-02 Trigger condition vocabulary

The supported trigger values are:

| Trigger | Condition |
|---------|-----------|
| `context-overflow` | Estimated prompt tokens exceed the primary model's `context_length` from OpenRouter catalog |
| `rate-limit` | HTTP 429 response or Hermes subprocess exit with a rate-limit-specific error message |
| `error-5xx` | HTTP 5xx response from the model provider (500, 502, 503, 504) |
| `error-timeout` | Hermes subprocess times out (configurable, default 300s) or HTTP timeout from provider |
| `any` | Matches any of the above; used as a catch-all hop |

A fallback hop is activated only when at least one of its declared triggers matches the observed failure.

### FR-03 Context-length pre-flight check

Before dispatching a prompt to Hermes, TAG:

1. Looks up the primary model's `context_length` in the OpenRouter catalog cache (stored locally after `tag openrouter-models`). If no cached data is available, the pre-flight check is skipped and a debug log line is emitted.
2. Estimates prompt token count. Method priority: (a) `tiktoken` if importable, (b) `len(prompt) / 4` character heuristic with a 10% safety margin.
3. If `estimated_tokens >= context_length`, selects the first fallback hop whose `triggers` includes `context-overflow` and whose own `context_length` is sufficient for the prompt. If no hop has sufficient context, the chain is exhausted and the run fails with a clear error message listing each model's limit.
4. The selected fallback model is injected into the route before Hermes is invoked. The substitution is logged to `fallback_substitutions` with `trigger = "context-overflow"` and `tokens_at_trigger = estimated_tokens`.

### FR-04 Runtime error matching

After a Hermes subprocess exits non-zero or an HTTP call returns an error:

1. TAG inspects the exit code, stderr output, and any embedded HTTP status code.
2. The error is classified into one of the trigger categories (`rate-limit`, `error-5xx`, `error-timeout`).
3. TAG walks the fallback chain in order, selecting the first hop whose triggers include the classified error type or `any`.
4. If a matching hop is found, the Hermes call is retried with the fallback model substituted into the route. The substitution is logged.
5. If no matching hop is found, or the chain is exhausted, the run fails with the original error plus a message listing the fallback chain that was attempted.

### FR-05 Maximum chain depth

A single run may activate at most 3 fallback hops. Hop count is tracked per-run in the execution context. On the 4th hop attempt, TAG logs an error (`FALLBACK_CHAIN_EXHAUSTED`) and raises a fatal error rather than attempting further substitution.

### FR-06 Mandatory substitution logging

Every fallback activation — whether triggered by pre-flight or runtime error — writes a row to `fallback_substitutions` and emits a WARNING log line in the format:

```
WARNING [fallback] route=coding primary=anthropic/claude-opus-4 fallback=openai/gpt-4o trigger=context-overflow tokens=85234 run_id=abc123
```

This is enforced at the call site; there is no code path that substitutes a model without logging.

### FR-07 Clear user-facing warning

When running interactively (TUI or terminal output), `print_warning()` is called with a human-readable message:

```
Fallback activated: switched from claude-opus-4 to gpt-4o (trigger: context-overflow, 85,234 tokens estimated)
```

This message appears before the retry begins, not after.

### FR-08 Cycle detection

When adding a fallback hop via `tag route fallback add`, TAG validates that the new hop does not create a cycle in the chain (i.e., the fallback model does not appear as any earlier hop's primary, and the primary of this chain does not appear as a fallback target anywhere in its own chain). Cycles are rejected at configuration time with a clear error.

### FR-09 Per-route fallback scope

Fallback chains are namespaced by route name (task type key from `routing.task_types`). The same primary model can have different fallback chains in different routes. A `--route` flag is required for all `tag route fallback` sub-commands.

### FR-10 OpenRouter catalog cache lookup

The pre-flight check uses the last-fetched OpenRouter catalog data. Cache is stored at `~/.tag/openrouter_models_cache.json` (or equivalent profile-scoped path). If cache age exceeds 24 hours, a debug warning is emitted but the stale data is still used (stale-but-functional). If no cache exists at all, pre-flight is skipped.

### FR-11 Fallback chain validation on add

When `tag route fallback add` is called:

- Both `--primary` and `--fallback` must be in `provider/model-id` format (validated by existing `parse_model_ref`).
- At least one `--on` trigger must be specified.
- `--fallback` must not equal `--primary`.
- Adding a hop that would bring the chain to > 3 hops for a single primary model is rejected.

### FR-12 Dry-run test mode

`tag route fallback test` resolves the chain using in-memory simulation only. No subprocess is spawned, no API key is used, no HTTP calls are made. If `--prompt-tokens` is provided, it uses that value for context-overflow simulation. If not provided, defaults to a token count that overflows the primary model (for testing purposes).

### FR-13 History pagination

`tag route fallback history --last N` returns at most N rows. Default N is 20. Rows are ordered by `timestamp DESC`. The `--json` flag returns a JSON array for machine consumption.

### FR-14 Integration with `tag submit`

When `cmd_submit` resolves a route and dispatches to Hermes, the fallback pre-flight check runs before the Hermes subprocess is spawned. The retry-on-runtime-error logic wraps the `run_profile_hermes` call within `cmd_submit` (and anywhere else `run_profile_hermes` / `run_hermes` is called with a model-backed route).

### FR-15 `tag route list` command

A new `tag route list` sub-command prints all task types from `routing.task_types`, each showing: task type name, execution mode, master profile and model, workers, verifier, and fallback chain (if any). This command is read-only and requires no additional arguments.

---

## 8. Non-Functional Requirements

### NFR-01 Fallback decision latency

The pre-flight check (token estimation + catalog lookup + chain selection) must complete in under 100ms for prompts up to 500k characters. The catalog lookup reads from a local JSON file — no network call is made during pre-flight.

### NFR-02 No silent substitution

Every fallback activation must produce a WARNING log entry and a user-facing `print_warning()` call. This is a hard requirement, not best-effort. Code review checkers (future) should flag any `run_hermes` or `run_profile_hermes` call that is not wrapped by the fallback logging layer.

### NFR-03 Chain cycle detection at configuration time

Cycles are detected and rejected when adding a hop, not at runtime. Runtime cycle detection is a backstop only (enforced via the 3-hop limit in FR-05).

### NFR-04 SQLite migration safety

The schema migration (ALTER TABLE + CREATE TABLE) is wrapped in a `try/except` for `OperationalError` on "duplicate column" to be idempotent with existing databases. The migration runs in `init_db()` which is called on every `tag` invocation.

### NFR-05 No breakage of existing `tag route` behavior

The existing `cmd_route` behavior (resolve and print route for a task type) is unchanged. Fallback configuration is additive. Routes without fallback chains behave exactly as before.

---

## 9. Technical Design

### 9.1 Changed files

| File | Change |
|------|--------|
| `src/tag/controller.py` | Extend `cmd_route` to dispatch `fallback` sub-command; add `cmd_route_fallback_add`, `cmd_route_fallback_list`, `cmd_route_fallback_remove`, `cmd_route_fallback_test`, `cmd_route_fallback_history`; add `_pre_flight_context_check(route, prompt)` helper; wrap `run_profile_hermes` calls in `cmd_submit` with `_execute_with_fallback(...)` |
| `src/tag/controller.py` (`init_db`) | Add `ALTER TABLE routes ADD COLUMN fallback_chain TEXT DEFAULT '[]'` migration; add `CREATE TABLE IF NOT EXISTS fallback_substitutions` |
| `src/tag/controller.py` (`resolve_route`) | Augment returned snapshot with `fallback_chain` loaded from the DB row for the matching route |

### 9.2 Schema migration

```sql
-- Migration: add fallback_chain column to routes table (if routes table exists)
ALTER TABLE routes ADD COLUMN fallback_chain TEXT NOT NULL DEFAULT '[]';

-- New table for substitution audit log
CREATE TABLE IF NOT EXISTS fallback_substitutions (
    id              TEXT PRIMARY KEY,
    route           TEXT NOT NULL,
    primary_model   TEXT NOT NULL,
    fallback_model  TEXT NOT NULL,
    trigger         TEXT NOT NULL,
    run_id          TEXT,
    tokens_at_trigger INTEGER,
    hop_number      INTEGER NOT NULL DEFAULT 1,
    timestamp       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_fs_route     ON fallback_substitutions(route, timestamp);
CREATE INDEX IF NOT EXISTS idx_fs_run_id    ON fallback_substitutions(run_id);
```

The `routes` table does not currently exist in the schema shown in `init_db`. This PRD also introduces the `routes` table:

```sql
CREATE TABLE IF NOT EXISTS routes (
    name            TEXT PRIMARY KEY,
    task_type       TEXT NOT NULL,
    config_json     TEXT NOT NULL DEFAULT '{}',
    fallback_chain  TEXT NOT NULL DEFAULT '[]',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
```

Until `cmd_route_fallback_add` is called, the fallback chain for a route is `[]` (empty) and behaves identically to the current state.

### 9.3 Fallback chain data model

The `fallback_chain` column stores a JSON array. Each element:

```json
{
  "model": "openai/gpt-4o",
  "triggers": ["context-overflow", "rate-limit"]
}
```

A complete three-hop chain:

```json
[
  {"model": "openai/gpt-4o",               "triggers": ["context-overflow", "rate-limit"]},
  {"model": "openrouter/deepseek/deepseek-v4", "triggers": ["any"]}
]
```

### 9.4 Pre-flight context check

```python
def _pre_flight_context_check(
    route: dict[str, Any],
    prompt: str,
    openrouter_cache: list[dict[str, Any]],
) -> dict[str, Any]:
    """
    Returns the (possibly-substituted) route dict and a substitution record
    if a fallback was selected, or None if no substitution occurred.
    """
    primary_model = format_model_ref(route["master"]["model"])
    # Look up context_length from cached catalog
    ctx_limit = _get_context_length(primary_model, openrouter_cache)
    if ctx_limit is None:
        return route, None  # no data, skip pre-flight

    estimated_tokens = _estimate_tokens(prompt)
    if estimated_tokens < ctx_limit:
        return route, None  # fits, no action needed

    # Overflow: walk fallback chain
    fallback_chain = route.get("fallback_chain", [])
    for hop_idx, hop in enumerate(fallback_chain):
        if "context-overflow" not in hop["triggers"] and "any" not in hop["triggers"]:
            continue
        hop_ctx = _get_context_length(hop["model"], openrouter_cache)
        if hop_ctx is not None and estimated_tokens >= hop_ctx:
            continue  # this hop also overflows, keep walking
        # Select this hop
        substituted_route = _substitute_master_model(route, hop["model"])
        sub_record = {
            "primary_model": primary_model,
            "fallback_model": hop["model"],
            "trigger": "context-overflow",
            "tokens_at_trigger": estimated_tokens,
            "hop_number": hop_idx + 1,
        }
        return substituted_route, sub_record

    return route, None  # chain exhausted, caller raises error
```

### 9.5 Runtime error wrapping

```python
def _execute_with_fallback(
    cfg: dict,
    route: dict,
    run_id: str,
    *hermes_args: str,
    hop_count: int = 0,
) -> subprocess.CompletedProcess:
    if hop_count >= 3:
        raise SystemExit("FALLBACK_CHAIN_EXHAUSTED: max 3 hops reached")

    try:
        result = run_profile_hermes(cfg, route["master"]["name"], *hermes_args)
        if result.returncode == 0:
            return result
        trigger = _classify_hermes_error(result)
    except subprocess.TimeoutExpired:
        trigger = "error-timeout"

    fallback_chain = route.get("fallback_chain", [])
    for hop in fallback_chain:
        if trigger not in hop["triggers"] and "any" not in hop["triggers"]:
            continue
        primary_model = format_model_ref(route["master"]["model"])
        _log_substitution(cfg, route, primary_model, hop["model"], trigger, run_id, hop_count + 1)
        substituted_route = _substitute_master_model(route, hop["model"])
        return _execute_with_fallback(
            cfg, substituted_route, run_id, *hermes_args, hop_count=hop_count + 1
        )

    raise SystemExit(f"Run failed (trigger={trigger}) and no matching fallback found")
```

### 9.6 Token estimation

```python
def _estimate_tokens(text: str) -> int:
    try:
        import tiktoken
        enc = tiktoken.get_encoding("cl100k_base")
        return len(enc.encode(text))
    except ImportError:
        # Heuristic: 4 chars per token with 10% safety margin
        return int(len(text) / 4 * 1.1)
```

### 9.7 OpenRouter catalog cache

`_get_context_length(model_id, catalog)` looks up a model in the OpenRouter catalog list by matching against the `id` field. The catalog is loaded from disk at the start of each `cmd_submit` call if a fallback chain is configured (lazy, only when needed).

---

## 10. Security Considerations

1. **Silent downgrade is a security-relevant behavior change.** A fallback from `claude-opus-4` (safety-trained, capable) to a less-capable or differently-aligned model changes the trust level of the agent's output. This must always produce a WARNING-level log entry and a user-facing message. There is no configuration option to suppress this warning.

2. **Fallback chain cycles could cause runaway retries.** Cycle detection at configuration time (FR-08) is the primary guard. The 3-hop runtime limit (FR-05) is the backstop. Both must be present.

3. **Fallback chain stored in SQLite is user-controlled configuration.** The `fallback_chain` JSON is validated on write (via `cmd_route_fallback_add`) but also validated on read before use, rejecting any chain that contains invalid model references or unsupported trigger names. Malformed JSON in the column is treated as an empty chain.

4. **Pre-flight token estimation must not include secrets from the prompt in log output.** The `tokens_at_trigger` field stores only the integer token count, never the prompt text. The WARNING log line includes only the count, not the prompt content.

---

## 11. Testing Strategy

### 11.1 Pre-flight context overflow detection

```
test_preflight_overflow_selects_first_matching_hop:
  - Build route with primary model context_length=10000
  - Provide prompt estimated at 12000 tokens
  - Assert fallback hop with context_length=50000 is selected
  - Assert substitution record is returned with trigger="context-overflow"

test_preflight_no_overflow_returns_unchanged_route:
  - Prompt estimated at 5000 tokens, primary limit=10000
  - Assert route returned unchanged, substitution record is None

test_preflight_skips_hop_that_also_overflows:
  - Chain: hop1 context_length=8000 (< 12000), hop2 context_length=50000
  - Assert hop1 is skipped, hop2 is selected

test_preflight_chain_exhausted_returns_no_substitution:
  - All hops overflow
  - Assert (original route, None) returned — caller must handle failure
```

### 11.2 HTTP error matching

```
test_classify_rate_limit_from_returncode:
  - Mock hermes subprocess returning exit 1 with stderr "429 Too Many Requests"
  - Assert trigger="rate-limit"

test_classify_5xx_from_stderr:
  - stderr contains "503 Service Unavailable"
  - Assert trigger="error-5xx"

test_classify_timeout:
  - subprocess.TimeoutExpired raised
  - Assert trigger="error-timeout"

test_runtime_fallback_retries_with_fallback_model:
  - First call returns 429, chain has rate-limit hop
  - Assert second call uses fallback model
  - Assert substitution logged
```

### 11.3 Chain depth limit

```
test_three_hop_limit_enforced:
  - Configure chain with 3 hops all triggering on "any"
  - All hermes calls fail with 503
  - Assert SystemExit("FALLBACK_CHAIN_EXHAUSTED") on 4th attempt
  - Assert 3 substitution rows written (one per successful hop transition)
```

### 11.4 Substitution logging

```
test_substitution_always_logged:
  - Trigger fallback via pre-flight
  - Query fallback_substitutions table
  - Assert 1 row with correct route, primary_model, fallback_model, trigger

test_no_log_when_no_fallback:
  - Successful primary call
  - Assert 0 rows in fallback_substitutions

test_warning_printed_to_stderr_on_substitution:
  - Capture stderr
  - Assert "Fallback activated" message present
```

### 11.5 Cycle detection

```
test_add_self_referential_hop_rejected:
  - Add fallback where primary == fallback
  - Assert SystemExit with cycle error

test_add_hop_creating_cycle_rejected:
  - Chain: A -> B
  - Add hop B -> A
  - Assert SystemExit with cycle error
```

### 11.6 CLI surface

```
test_route_fallback_list_empty:
  - No chains configured
  - Assert "No fallback chains configured" output

test_route_fallback_test_dry_run_no_api_call:
  - Mock subprocess to assert it is never called
  - Run cmd_route_fallback_test
  - Assert subprocess not called

test_route_fallback_history_respects_last_param:
  - Insert 30 substitution rows
  - Run history --last 5
  - Assert exactly 5 rows returned
```

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag route fallback add --route coding --primary anthropic/claude-opus-4 --fallback openai/gpt-4o --on context-overflow` writes a row to the `routes` table with `fallback_chain` containing one hop | `sqlite3 tag.sqlite3 "SELECT fallback_chain FROM routes WHERE name='coding'"` |
| AC-02 | Pre-flight check with a 90k-token prompt against a 80k-context model selects the first fallback with sufficient context before any Hermes subprocess is spawned | Unit test: subprocess call count == 0 when overflow detected pre-flight |
| AC-03 | A run that triggers a fallback always writes exactly one row to `fallback_substitutions` per hop | `tag route fallback history --last 5 --json` shows the row |
| AC-04 | `tag route fallback test --route coding --trigger context-overflow` exits 0, prints a dry-run summary, and makes zero HTTP calls | Mock `urllib.request.urlopen` and assert not called |
| AC-05 | A chain configured with 3 hops that all fail results in `FALLBACK_CHAIN_EXHAUSTED` error, not infinite recursion | `tag submit` exits non-zero with that error string |
| AC-06 | A self-referential fallback (`--primary A --fallback A`) is rejected at add-time with a non-zero exit code | `tag route fallback add` exits 1 |
| AC-07 | A fallback cycle (`A -> B`, then add `B -> A`) is rejected at add-time | `tag route fallback add` exits 1 |
| AC-08 | `print_warning()` is called on every fallback activation when running interactively | Integration test: capture stdout/stderr, assert "Fallback activated" present |
| AC-09 | A route with no configured fallback chain behaves identically to current behavior | Existing tests for `cmd_submit` and `cmd_route` pass without modification |
| AC-10 | `tag route fallback history --route coding --last 10` returns at most 10 rows ordered newest-first | Test with 25 inserted rows |
| AC-11 | `tag route list` includes fallback chain summary for routes that have chains configured | Output contains "--> openai/gpt-4o (context-overflow)" for configured chains |
| AC-12 | The `fallback_substitutions` table is created on first `tag` invocation via `init_db` migration, without errors on fresh databases | `init_db()` called on empty DB returns without exception |

---

## 13. Dependencies

| Dependency | Nature | Notes |
|------------|--------|-------|
| `load_openrouter_catalog` (existing) | Hard — pre-flight reads `context_length` from this data | Cache must be populated before pre-flight can run; if cache is absent, pre-flight is skipped gracefully |
| `cmd_route` / `resolve_route` (existing) | Hard — fallback sub-commands extend this infrastructure | No breaking changes to existing `resolve_route` signature |
| `run_profile_hermes` / `run_hermes` (existing) | Hard — runtime error wrapping wraps these call sites | Only `cmd_submit` and `cmd_benchmark` dispatch paths need wrapping initially |
| `parse_model_ref` (existing) | Hard — used to validate `--primary` and `--fallback` arguments | No changes needed |
| `print_warning` / `print_error` (existing, via `tui_output`) | Hard — used for mandatory substitution warnings | Fallback path in controller.py already handles `_TUI_OUTPUT_AVAILABLE = False` |
| `tiktoken` (optional) | Soft — used for accurate token counting in pre-flight | Falls back to character heuristic if not installed; no hard dependency added to `pyproject.toml` |
| `init_db` (existing) | Hard — schema migration added here | Migration is idempotent |

---

## 14. Open Questions

| ID | Question | Impact | Owner |
|----|----------|--------|-------|
| OQ-01 | Should TAG-side fallback and OpenRouter-side `fallbacks` (in the request body) be mutually exclusive, or can they coexist? If both are configured, which takes precedence? | Medium — double-fallback could cause confusing behavior | Architecture review |
| OQ-02 | Which token counting library should be the preferred dependency? `tiktoken` (OpenAI, accurate for GPT models), `anthropic`'s token counter (accurate for Claude), or a provider-agnostic heuristic? | Low for pre-flight accuracy; high for correctness on context-overflow decisions | Developer |
| OQ-03 | How should quota exhaustion (different from rate limit — a hard monthly cap) be handled? It produces a 402 or a 429 with a different error body. Should it be a separate trigger (`quota-exhausted`)? | Low in current scope, but could become P0 for teams with tight budgets | Product |
| OQ-04 | Should `fallback_chain` configuration live in the `routes` SQLite table or in the YAML config file? SQLite is runtime-mutable; YAML is version-controllable. A hybrid (YAML-defined chains, DB overrides) is possible but complex. | High — affects team workflows and GitOps practices | Architecture review |
| OQ-05 | When the pre-flight detects overflow and falls back, should the fallback model's context length be checked recursively (walk the entire chain in one pass) or lazily (check current hop, retry if that also fails)? The current design uses one-pass selection during pre-flight and lazy retry during runtime. Is this the right split? | Low | Developer |

---

## 15. Complexity and Timeline

**Complexity:** M

**Estimated timeline:** 1 sprint (2 weeks)

| Phase | Tasks | Days |
|-------|-------|------|
| Phase 1: Schema and storage | `init_db` migration; `cmd_route_fallback_add/list/remove`; `parse_model_ref` validation | 3 |
| Phase 2: Pre-flight check | `_estimate_tokens`; `_get_context_length`; `_pre_flight_context_check`; integrate into `cmd_submit` | 3 |
| Phase 3: Runtime error wrapping | `_classify_hermes_error`; `_execute_with_fallback`; 3-hop limit; substitution logging | 3 |
| Phase 4: CLI surface | `cmd_route_fallback_test` dry-run; `cmd_route_fallback_history`; extend `tag route list` | 2 |
| Phase 5: Tests and docs | Unit tests for all FR; integration test for AC-01 through AC-12 | 3 |

**Total: ~14 working days.** Phases 1 and 2 can begin in parallel with separate developers.
