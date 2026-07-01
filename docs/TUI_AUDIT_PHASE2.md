# TAG CLI — Phase 2 In-Depth Runtime Audit

Method: real command execution in isolated sandboxes (`TAG_HOME` per tester), ~380 invocations
across the full command surface. No live model calls; no Anthropic account used. Focus: features
that *actually run*, edge cases, `--json` contracts, data integrity, security. This complements the
Phase 1 branding/TUI audit — Phase 2 found that a green unit suite masked many broken dispatch paths.

Legend: ☐ open · ☑ fixed

## Resolution summary (v0.8.1)

**Fixed & verified in sandbox (all with regression suite green — 678 passed):**
- **A1, A2** — systemic `main()` SystemExit/exception handling (converts ~59 traceback paths to clean
  errors + a top-level safety net, `TAG_DEBUG=1` to re-raise); version single-sourced to 0.8.1.
- **B1–B15** — every broken feature: mem2 gc/extract/fact-list-at/episode/store, graph query,
  dag run (name deps + non-list + cycle detection), prompt save, eval-ci run (rewired to the real
  eval API), eval-judge run (+existence check, +criteria default, +valid JSON), budget check/overflow.
- **C1** — template-import path-traversal (security) sanitized; C2–C11 tracebacks now clean via A1's net.
- **D1** — config writes are now lock-serialized + atomic (`os.replace`); concurrent `set-model` no longer
  bricks the CLI. **D3** confidence `0` no longer silently becomes `1.0`. **D4** secret scanner cap 1 MB→10 MB.
- **E1** — `--json` now valid on: costs, trace list, trace show/extended, budget get, tool-index search,
  alert firings, route-fallback resolve (empty/error paths).
- **F1** false-success fixed (swarm abort, loop abort, notify enable/disable, security scan missing path).
  **F5** workspace index (validate max-files, dir check, per-run token count). **F6** cron validation
  (negatives, `*/0`, reversed ranges). **F7** marketplace push finds bootstrapped profiles.
  **F8** eval-ci scaffold emits real commands.

**Deferred (low severity / higher-risk-than-reward — tracked, not release-blocking):**
D2 (render `--force` semantics), F2 (otel `--semconv` override), F3 (cache trend `--json`),
F4 (set-model provider allow-list — risks rejecting valid future models), F9 (template duplicate now
errors; no-name still defaults to `imported` for back-compat), F10–F16, and a few remaining `--json`
empty/error paths (cache trend, agentops show, queue cancel, hooks list/log, import-* errors —
the import errors already print a clean message via A1). These are cosmetic/consistency items.

---


## A. Systemic

- ☐ **A1 [high, systemic]** `main()` (`controller.py`) does `int(exc.code)` on caught `SystemExit`.
  The Python convention `raise SystemExit("msg")` sets `exc.code` to a **string**, so `int("msg")`
  raises an uncaught `ValueError` — every one of ~59 string-message exits crashes with a traceback
  instead of printing the message and exiting 1. Also no top-level guard for other unexpected
  exceptions (yaml/FileNotFound/Type/Overflow errors all reach the user as raw tracebacks).
- ☐ **A2 [high]** `tag --version` reports **0.7.2** — `src/tag/__init__.py.__version__` was never
  bumped with pyproject/npm (now 0.8.0). Published 0.8.0 misreports its own version.

## B. Completely broken features (wrong kwargs/arity/schema — never actually ran)

- ☐ **B1 [crash]** `mem2 gc [--dry-run]` — `GCConfig.__init__() got unexpected kwarg 'dry_run'` (memory.py:254)
- ☐ **B2 [crash]** `mem2 fact list-at --at ...` — `list_facts_at() got unexpected kwarg 'at'` (memory.py:332)
- ☐ **B3 [crash]** `mem2 extract <run>` — `sqlite3.OperationalError: no such column: output` (memory.py:274)
- ☐ **B4 [high]** `mem2 episode get --id <id>` — always returns `[]` though the episode exists
- ☐ **B5 [med]** `mem2 episode start --summary X` — flag silently discarded (hardcoded "CLI session")
- ☐ **B6 [med]** `mem2 store store` — advertised choice `store` rejected as "Unknown store action"
- ☐ **B7 [crash]** `graph query <e>` — `query_graph() got unexpected kwarg 'max_depth'` (prd_clusters.py:702)
- ☐ **B8 [crash]** `dag run` with name-based `depends_on` — `TypeError: '<' not supported str/int` (dag.py:274)
- ☐ **B9 [crash]** `dag run` with non-list `steps` — `AttributeError: 'str' has no attribute 'get'` (dag.py:273)
- ☐ **B10 [crash]** `prompt save NAME FILE --notes` — `save_prompt() got unexpected kwarg 'notes'` (prd_clusters.py:313); entire prompt feature unusable
- ☐ **B11 [crash]** `eval-ci run SUITE` — `ValueError: too many values to unpack (expected 2)` (eval_ci.py:47)
- ☐ **B12 [crash]** `eval-judge run <run>` (no --criteria) — `TypeError: 'NoneType' not iterable` (eval_judge.py:440)
- ☐ **B13 [high]** `eval-judge run` — `--json` prints dataclass repr not JSON; and silently "succeeds"
  (exit 0, fabricated empty result) on a nonexistent eval run
- ☐ **B14 [high]** `budget check` — never sees a configured budget: `check_budget()` returns a dict with
  no `"budget"` key while `cmd_budget` tests `result.get("budget") is None`. Budget enforcement dead.
- ☐ **B15 [crash]** `budget set --max-tokens <huge>` — `OverflowError: int too large for SQLite` (budget.py:72)
- ☐ **B16 [high]** `context show/trim/compress` — all broken: `show` passes `--json` unconditionally to the
  runtime; `trim`/`compress` map to nonexistent runtime subcommands; every error leaks the `hermes` name.

## C. Uncaught exceptions on bad input (should be clean errors)

- ☐ **C1 [high, security]** `template import <file>` — no name sanitization: a template `name:` of
  `../../x` or `/abs/x` creates directories **outside** `TAG_HOME`. Path-traversal write primitive.
- ☐ **C2 [crash]** `--config <malformed.yaml>` — raw `yaml.ScannerError` traceback
- ☐ **C3 [crash]** `template import <missing file>` — `FileNotFoundError`
- ☐ **C4 [crash]** `template fetch <scheme-less url>` — `ValueError: unknown url type`
- ☐ **C5 [crash]** `trace export <bad-url>` — `ValueError: unknown url type` (observability.py:250); also blindly appends `/v1/traces`
- ☐ **C6 [crash]** `persona install <malformed.yaml>` — `yaml.ScannerError` (persona.py:142)
- ☐ **C7 [crash]** `alert create --metric <bad>` — `ValueError: Unknown metric` (alerts.py:193); metric not validated by argparse
- ☐ **C8 [crash]** `eval-dataset create <dup>` — `sqlite3.IntegrityError: UNIQUE` (eval_datasets.py:103)
- ☐ **C9 [crash]** `annotate export --format csv` — advertised choice raises `ValueError: Unsupported` (annotation_queue.py:373)
- ☐ **C10 [crash]** `prompt save NAME <missing file>` — `FileNotFoundError` (prd_clusters.py:312)
- ☐ **C11 [crash]** `agentic-ci fix-vuln <missing .sarif>` — `FileNotFoundError` (ci.py:842)

## D. Data integrity

- ☐ **D1 [crash]** `set-model` concurrent writes corrupt `tag.yaml` (non-atomic write) → torn YAML →
  every subsequent config-reading command crashes; `bootstrap` does not recover. CLI bricked.
- ☐ **D2 [med]** `render` clobbers manual edits to rendered `config.yaml`; `--force` is a no-op.
- ☐ **D3 [med]** `mem add --confidence 0` stores **1.0** (falsy `x or default` override), bypassing range check.
- ☐ **D4 [med]** `security scan` silently skips files >~1MB → planted key reported "No secrets found" (false negative, no warning).

## E. `--json` contract violations (plain text / repr on empty or error paths)

- ☐ **E1 [med]** Broad: `costs`, `trace list`, `trace show`, `agentops show`, `cache trend`, `route-fallback resolve`(not-found),
  `queue cancel`, `alert firings`, `budget get`, `tool-index search`, `hooks list/log`, `cron`(errors), `import-*`(errors)
  emit plain text or nothing instead of valid JSON when `--json` is set. (Success paths are valid.)

## F. False-success / validation gaps (medium/low)

- ☐ **F1 [med]** False success (exit 0) on nonexistent target: `swarm abort`, `loop abort`, `notify enable/disable`,
  `trace snapshot`, `security scan <missing path>`.
- ☐ **F2 [med]** `otel-export --semconv <v>` ignored (hardcoded 1.28.0).
- ☐ **F3 [med]** `cache trend --json` ignores `--json` (prints ASCII chart).
- ☐ **F4 [med]** `set-model` accepts unknown provider slug (no provider validation).
- ☐ **F5 [med]** `workspace index` token count ignores `--max-files`; negative `--max-files` walks whole tree; nonexistent/file path → silent success.
- ☐ **F6 [med]** `cron add` accepts out-of-range/degenerate schedule fields (negatives, `*/0`, reversed ranges).
- ☐ **F7 [med]** `marketplace push` cannot export bootstrapped profiles (looks in wrong dir).
- ☐ **F8 [med]** `eval-ci scaffold` emits workflows invoking nonexistent commands (`tag eval ci`, `tag ci review`, ...).
- ☐ **F9 [med]** `template`: no-name→`imported`, duplicate silently overwrites, `export <ghost>` fabricates, writes to a store doctor ignores.
- ☐ **F10 [low]** `alert check` fires CRITICAL on zero data (undefined pass-rate treated as 0).
- ☐ **F11 [low]** `queue result` shows `status: queued` for a `done` job.
- ☐ **F12 [low]** `persona stack` index always `[0]`.
- ☐ **F13 [low]** `pricing get` accepts negative token counts.
- ☐ **F14 [low]** `eval-dataset create ""` / blank name accepted.
- ☐ **F15 [low]** `route-fallback` allows duplicate + 2-node cycle chains (self-ref correctly blocked).
- ☐ **F16 [low]** `budget set --warn-pct` range `(0,1)` undocumented in help.

## Not bugs (verified intentional)
- `.hermes/` runtime directory paths appearing in output are real dirs, not text-substitution leaks.
- `HERMES_*` env var names are functional (runtime reads them) — intentionally not rebranded.
- Confirmed WORKING well: `memory-journal`, `mem` (add/list/search/forget/stats, concurrency, unicode/SQLi safety),
  `diff-context`, most `--json` success paths, argparse numeric validation, no SQLite corruption under concurrent `mem`/`queue`/`alert`/`eval-dataset` writes.
