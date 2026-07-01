# TAG — Modernization / Refactor Plan

> The in-depth, phased plan implementing the recommended approach in :
> **modernize Python in place** (strangler-fig, never big-bang). Do NOT rewrite in Go/Rust.

**Total effort:** Core modernization (Phases 0-4, the recommended path, single maintainer): ~6-9 weeks of focused effort spread over roughly a calendar quarter alongside continued weekly feature releases (Phase 0 ~1-1.5wk, Phase 1 ~0.5-1wk, Phase 2 ~2-3wk — the largest, Phase 3 ~1-1.5wk, Phase 4 ~1-2wk). The OPTIONAL native launcher (Phase 5) adds ~2-3 weeks only if pursued. Total with the optional slice: ~8-12 weeks. This is roughly 1-5% of the cost of a Go/Rust control-plane rewrite (which would be multi-quarter with a dual-toolchain parity period — cf. Codex's multi-quarter parallel TS+Rust maintenance even as a funded team), delivers the same dynamic-dispatch crash-class safety, keeps the 200-bug corpus and 103-command surface intact, and fixes the actually-fixable distribution/startup/concurrency pains. Realistic caveat: the two dominant costs (54.7MB Hermes tarball in every artifact, Hermes venv cold-start) are untouched by ANY option including a rewrite — this plan captures the achievable slice of every benefit at minimal risk.

## Guiding principles

- Language was never the failure mode: only ~11 of 200 fixed bugs (B001/B004/B006/B011/B012/B013/B022, wrong-kwarg / dict-vs-object dispatch crashes) were language-attributable. Type the seams to close that class in place; do not re-earn the other ~92% (logic/contract/concurrency-semantics) in a new language.
- Preserve sunk value: the 200-bug hardening corpus and the 678-test + 103-command --help gate are the crown jewels. No change ships unless that gate stays green. Never big-bang.
- One owning language for the single SQLite DB (core/db.py, ~35-40 tables, WAL+busy_timeout). Never introduce a second-language writer — that reintroduces the non-atomic RMW race class (B005/B035/C010/C017/B102) that 200 fixes just closed.
- The Hermes subprocess seam (core/run.py: run_hermes / run_profile_hermes / run_profile_python; per-profile HERMES_HOME env) is irreducibly Python and must be left structurally intact — only annotated and pydantic-validated, never re-homed.
- Every phase is independently shippable and independently revertible. The CLI keeps releasing weekly on the existing pip+npm channels throughout; modernization runs behind CI gates and feature flags, never as a release-blocking cutover.
- Attack the real pains in cost order: (1) uv-based provisioning + CI speed, (2) typing gate on seams, (3) startup latency via lazy imports, (4) SSE/dashboard concurrency correctness, (5) optional native launcher last. Do not confuse the fixable pains with the irreducible ones (54.7MB tarball + Hermes venv + Hermes cold-start stay regardless).
- Bus-factor-of-one reality (601/629 commits, one author): favor one toolchain, small reversible diffs, and automation (uv, ruff, ty in CI) over anything that demands dual-toolchain maintenance.

## First slice to port

The first concrete slice is Phase 0 + the front half of Phase 2, targeting ONE command group end to end: pick cmd/routing.py (profiles/set-model), because it contains a documented, language-attributable, high-value seam — the config read-modify-write in cmd/routing.py (~L164-188: load_config -> mutate -> save_config -> render_profiles(force=True)) that carried the B005 lost-update and C017 stale-render races, plus the dict-vs-object dispatch surface. Concretely: (1) commit uv.lock and wire `uv run pytest`; (2) snapshot golden --help + --json + exit codes for every routing subcommand; (3) fully type cmd/routing.py's handlers and the config dict it threads through core/config.py + core/run.py, introducing a pydantic/TypedDict model for the profile/config shape; (4) turn on ty + mypy --strict BLOCKING for cmd/routing.py, core/run.py, core/paths.py only; (5) add regression tests reproducing a dispatch-crash and the RMW lost-update. This proves the whole modernization loop (uv build -> typed seam -> pydantic validation -> golden parity -> strict gate) on a single real module before fanning out to the other 14 cmd modules. It touches the seam, not the 40 feature-module internals, and is fully revertible.

## Phases

### Phase 0 — Baseline, freeze parity harness, adopt uv for dev/CI
**Goal:** Lock down an executable definition of 'current behavior' before touching anything, and switch the developer/CI inner loop to uv without changing runtime behavior or distribution.
**Effort:** 1-1.5 weeks
**Scope:**
- Generate a uv.lock from the existing exact pins (pyproject already exact-pinned post Shai-Hulud); wire `uv sync`/`uv run` into CI and contributor docs. Keep pip/npm publish paths byte-for-byte unchanged for now.
- Snapshot the parity baseline: run the full 678-test suite (7 parametrized files: test_controller, test_cross_cutting, test_prd_021_032, test_prd_033_044, test_prd_features, test_clusters_abc, hermes_cli fixtures) and capture as the golden gate.
- Build a `tag --help` sweep harness: enumerate all ~103 subcommands via the argparse tree (from tag.cmd.COMMAND_MODULES) and snapshot help text + exit codes + --json envelopes into golden files. This becomes the CLI-contract regression gate.
- Add a sandboxed end-to-end smoke matrix in an isolated TAG_HOME (per MEMORY: unit suite masked ~30 dispatch bugs; verify by RUNNING commands, not just pytest). Cover a representative command per command-group without requiring a full Hermes run where possible; stub the core/run.py seam.
- Stand up CI jobs (non-blocking at first) for ruff check, ruff format --check, ty, and mypy so their baselines are visible.
**Strategy:** Pure scaffolding: build the parity net that lets every later phase be proven safe. Nothing user-visible strangled yet.
**Risks:** uv.lock resolution differing from the hand-pinned set — diff resolved versions against installed set and reconcile before merging.; --help/--json snapshots being noisy (timestamps, paths) — normalize outputs in the harness.
**Exit criteria:** uv.lock committed; CI runs on uv; golden test + --help/--json contract snapshots committed and reproducible; ruff/ty/mypy run in CI (report-only). Zero runtime/distribution behavior change — pip and npm artifacts identical to pre-Phase-0.

### Phase 1 — ruff + formatting as a merge gate (zero behavior change)
**Goal:** Make lint/format enforced and automatic so later typed diffs are reviewable and the recurring lint-adjacent bug bands stop recurring.
**Effort:** 3-5 days
**Scope:**
- Turn ruff check + ruff format into blocking CI gates. Apply the one-time format sweep as a single isolated commit (excluded from git blame via .git-blame-ignore-revs).
- Enable targeted ruff rules that map to observed bug classes: mutable-default-args, unused, shadowing, and flag the `getattr(args,X,default) or default` idiom (B047/B087/B088/C038/C040) where lint-detectable; add a custom check or grep-gate for the `x or default` clobbering-0 pattern.
- No semantic edits in this phase beyond what ruff --fix does safely; run the full golden gate after the format sweep to prove no behavior drift.
**Strategy:** Strangles inconsistent style and a lint-visible sliver of the `x or default` class; no logic touched.
**Risks:** Large format diff obscuring review — land as one mechanical commit, verify via golden gate not eyeball.; ruff autofix changing behavior on edge cases (e.g. import reordering with side effects) — run full smoke matrix, not just pytest.
**Exit criteria:** ruff check + format gate green and blocking on main; format sweep landed with blame-ignore; full test + --help/--json golden gate unchanged.

### Phase 2 — Type the dispatch seams + core/run.py (close the ONLY language-attributable bug class)
**Goal:** Add static typing + pydantic validation exactly where the ~11 dynamic-dispatch crash bugs lived, getting the same crash-class safety a Go/Rust rewrite would buy, at ~1% of the cost.
**Effort:** 2-3 weeks
**Scope:**
- Annotate the ~15 cmd/*.py registrars: give every `cmd_*`/`do_*` handler a typed signature; replace ad-hoc `args.X` access and dict-as-object patterns with typed access. This is where B001/B004/B006/B011/B012/B013/B022 (wrong kwargs, dict-vs-object) lived.
- Introduce a pydantic model (or TypedDict) for the config dict flowing through core/run.py (run_hermes/run_profile_hermes/run_profile_python) and profile_exec_env; validate at the boundary so wrong-shape crashes surface at the seam, not deep in Hermes.
- Run ty AND mypy --strict on cmd/*.py + core/run.py + core/paths.py first (module-by-module allowlist), tightening until both pass strict on the seams. Keep them report-only on the ~40 feature modules for now.
- Add regression tests reproducing 2-3 of the original dispatch crashes to prove the type gate would have caught them.
**Strategy:** Strangles the untyped dispatch edges module-by-module behind a per-file strict allowlist — the classic incremental-typing strangler. Feature-module bodies stay dynamically typed until later.
**Risks:** ty beta instability (0.0.21, 1.0 targeted 2026, not a mypy drop-in) — MITIGATE by pairing with mypy --strict as the authoritative gate; treat ty as fast local feedback only until it stabilizes.; Annotating heavily-dynamic code (getattr on argparse Namespace) forcing awkward casts — accept typed wrapper accessors rather than over-engineering.; Scope creep into feature-module internals — hard-stop the strict allowlist at the seams this phase.
**Exit criteria:** ty + mypy --strict pass and are BLOCKING for cmd/*.py, core/run.py, core/paths.py, core/config.py; the seam config object is pydantic-validated; golden gate green. The dynamic-dispatch crash class is now compile-time-prevented for new code.

### Phase 3 — Startup latency: lazy imports in build_parser (the reachable perf win)
**Goal:** Cut the control-plane interpreter cold-start (the lighter of TAG's two Python interpreters) by deferring heavy imports, without regressing the stabilized command surface.
**Effort:** 1-1.5 weeks
**Scope:**
- Profile with `python -X importtime` to quantify the eager cost of build_parser importing all COMMAND_MODULES plus pydantic/rich/prompt_toolkit/openai on every invocation.
- Make cmd-module registration lazy: register subparsers without importing each module's heavy deps until the selected command runs (defer heavy imports into the handler bodies). Preserve --help completeness (the --help sweep from Phase 0 is the guard).
- Defer rich/prompt_toolkit/openai imports out of the hot path for metadata commands (help/list/status).
- Measure before/after startup for `tag --help`, `tag <group> --help`, and a no-op status command; target the 50-70% control-plane startup reduction cited for comparable CLIs.
**Strategy:** Strangles eager top-level imports one cmd module at a time; each module's laziness is independently revertible and guarded by the --help sweep.
**Risks:** Lazy imports breaking --help output or argument registration (a command missing from the tree) — the Phase-0 --help sweep is the exact regression net; run it after every module.; Hidden import-time side effects (registration, monkeypatching) breaking when deferred — smoke-test each command's actual execution, not just help.
**Exit criteria:** Measured control-plane cold-start reduction on metadata commands; --help/--json golden gate byte-identical; full test suite green. Honest framing documented: this removes only the lighter interpreter — the Hermes subprocess cold-start (dominant wall-clock on real agent runs) is unchanged.

### Phase 4 — Concurrency correctness for long-lived servers (fix B031-class in place)
**Goal:** Remove the single-threaded stdlib http.server foot-gun still present in devui.py and webhook_server.py, and harden the remaining SQLite RMW seams — the language-independent concurrency pains a rewrite would otherwise be credited for.
**Effort:** 1-2 weeks
**Scope:**
- Swap devui.py and webhook_server.py to ThreadingHTTPServer (matching the fix already applied to api.py after B031) or move them onto the already-present uvicorn dependency; ensure SSE clients no longer wedge the whole server.
- Audit and, where missing, apply the atomic-replace / row-level-locking pattern to remaining non-atomic read-modify-write sites (config set-model RMW race lineage B005/C017, queue dequeue B102) using the existing WAL+busy_timeout foundation in core/db.py — same-language, no schema split.
- Add concurrency regression tests: a held SSE client must not block a second request; two racing config writes must not lose an update.
- Explicitly do NOT move any of this to a native language — that would split SQLite ownership and reintroduce the RMW race class.
**Strategy:** Strangles the single-thread server foot-gun server-by-server; each server swap is independently shippable and revertible. SQLite stays Python-owned.
**Risks:** ThreadingHTTPServer exposing latent shared-state races in handlers — add per-request isolation and test under concurrency.; Over-reaching into a native/async rewrite of the queue/DAG workers — out of scope; app-level locking is the fix, not a language change.
**Exit criteria:** devui.py + webhook_server.py survive a held-SSE-client concurrency test; identified RMW races have atomic-replace tests; golden gate green. Single SQLite writer-language invariant preserved.

### Phase 5 (OPTIONAL, isolated, reversible) — Native uv/PyApp launcher replacing bin/tag.js
**Goal:** Collapse the npm bootstrap: replace the 243-line Node launcher + npm-runtime venv provisioning with a single self-contained launcher that bundles a python-build-standalone interpreter (aider/PyApp/uv precedent), removing the system-Python-on-PATH and Node requirements for launching.
**Effort:** 2-3 weeks (only if pursued)
**Scope:**
- Replace bin/tag.js with a uv-based or PyApp (Rust) launcher that ships a relocatable Python interpreter and pip-installs/execs the UNCHANGED Python control plane. This is launcher-only and logic-free.
- Preserve the O_EXCL install-lock + version-stamp cold-start correctness (B063) behavior in the new launcher.
- Keep pip (tag-agent) distribution untouched; the launcher is the npm-channel replacement plus optional brew/curl channels (Codex/crush multi-channel precedent). Ship BOTH old bin/tag.js and new launcher in parallel behind a channel flag until parity is proven.
- HARD BOUNDARY: the launcher never touches feature modules, the SQLite DB, HTTP/SSE, or config logic. If it ever grows past launch/provision, kill the slice.
**Strategy:** The purest strangler slice: run new launcher and bin/tag.js side by side, cut traffic over per-channel, keep the old path one flag away for instant rollback. Never merges into the feature/DB layer.
**Risks:** Scope creep into owning HTTP/SSE/queue/config — reintroduces the single-SQLite split and the RMW race family; enforce the launcher-only boundary or abandon.; Bespoke native launcher becoming a solo-maintainer burden (rye→uv consolidation cautionary tale) — prefer uv/PyApp off-the-shelf over hand-rolled; if maintenance cost exceeds the bin/tag.js pain it replaces, kill it.; python-build-standalone interpreter not covering a target platform/arch — keep bin/tag.js as the documented fallback channel.
**Exit criteria:** New launcher provisions and execs TAG on macOS/Linux/Windows with no system-Python and no Node requirement; removes the npm-runtime venv (one of the two venvs); passes the full --help + smoke matrix identically to bin/tag.js; old launcher still available as fallback. The Hermes venv, 54.7MB tarball, branding git-patch, and ui-tui npm build are unchanged (documented as irreducible).


## Keeping the CLI shipping during migration

Ship weekly on the existing pip (tag-agent) + npm channels throughout — modernization never becomes a release-blocking cutover. Mechanics: (1) All modernization work is additive behind CI gates, not runtime flags for users — a typed seam or lazy import is invisible to users and rides normal releases once the golden gate is green. (2) The 678-test suite + 103-command --help/--json golden sweep runs on every PR as the merge gate; a red gate blocks merge, never a release. (3) ruff/ty/mypy start report-only (Phase 0) and flip to blocking module-by-module via a strict allowlist, so feature work on not-yet-typed modules is never blocked. (4) Distribution is unchanged until Phase 5, which itself ships the new launcher SIDE BY SIDE with bin/tag.js behind a channel flag — so even the one distribution-affecting phase has a zero-downtime dual-run. (5) Keep the format sweep (Phase 1) as a single mechanical commit with .git-blame-ignore-revs so it doesn't collide with in-flight feature branches. Net: feature velocity continues in Python the whole time; each phase lands as normal small PRs gated by the parity net.

## Parity / testing strategy

Parity is defined by three layers, all frozen in Phase 0 and enforced on every PR. (1) BEHAVIORAL: the existing 678-test suite (7 parametrized files incl. test_controller, test_cross_cutting, test_prd_* contract tests) stays green — this is the primary correctness gate and is never discarded (the core argument against a rewrite is that a rewrite throws this away). (2) CLI-CONTRACT: an auto-generated golden snapshot of all ~103 subcommands' --help text, --json envelope shapes, and exit codes (walking tag.cmd.COMMAND_MODULES), normalized for paths/timestamps; any diff is a parity failure. This directly guards the --json/exit-code contract class (B048/B052/B054/B069/B071/C002/C045) and the lazy-import phase. (3) RUNTIME-SMOKE: per MEMORY (v0.8.1 audit — the unit suite masked ~30 dispatch-layer bugs), a sandboxed end-to-end matrix that actually RUNS a representative command per group in an isolated TAG_HOME with the core/run.py Hermes seam stubbed, catching dispatch/integration bugs pytest misses. Additionally: per-fixed-bug regression tests are added when a phase touches that bug's neighborhood (e.g. reproduce B001-class dispatch crash in Phase 2, B031 SSE wedge in Phase 4, B005 RMW in Phase 4). ty+mypy --strict on the seams is a STATIC parity gate for the dynamic-dispatch crash class. No phase merges unless all three behavioral/contract/smoke layers are green.

## Rollback & kill criteria

Per-phase rollback: every phase lands as independently revertible commits/PRs behind the golden gate, so rollback = `git revert` of that phase's PRs with zero coupling to other phases (uv.lock, ruff config, per-module type gates, lazy imports, and the launcher are all orthogonal). Specific triggers: (Phase 1) if the format sweep or ruff autofix causes any golden-gate or smoke-matrix regression that isn't trivially fixable, revert the sweep and re-scope rules. (Phase 2) if ty/mypy strict on a seam forces changes that regress behavior or the annotations can't converge without unsafe casts, drop that module back to report-only rather than blocking. (Phase 3) if lazy imports drop any command from the --help tree or break execution, revert that module's laziness (guarded by the --help sweep). (Phase 4) if ThreadingHTTPServer surfaces a handler race that can't be contained, revert to single-thread for that server and ship the SQLite-locking fix independently. KILL CRITERIA for the optional Phase 5 launcher: kill it and keep bin/tag.js if (a) the native launcher's maintenance burden exceeds the bin/tag.js pain it replaces (solo-maintainer test), (b) python-build-standalone can't cover a required platform/arch, or (c) the slice shows ANY tendency to grow past launch/provision into feature/DB/HTTP logic — that boundary breach is an automatic kill because it reintroduces the single-SQLite cross-language-writer race class. Global stop condition: if at any point the 200-bug corpus regression rate rises, freeze modernization and stabilize before proceeding — the hardening corpus outranks the modernization.

## Explicit non-goals (do NOT do)

- Do NOT rewrite the control plane in Go or Rust — it pays maximum migration cost to fix ~5-8% of bugs a linter already catches, discards the 678-test + 200-bug corpus, re-earns the ~92% logic/contract/concurrency bugs fresh, and still must ship+provision+subprocess Python Hermes.
- Do NOT embed Hermes via PyO3/cgo — it breaks per-profile HERMES_HOME venv isolation, forces CPython ABI matching, collapses crash isolation, and complicates the static-binary story. Keep the coarse subprocess boundary in core/run.py.
- Do NOT give any non-Python component (or a native launcher) write access to the single SQLite DB — one owning language only; a second writer reintroduces the non-atomic RMW race class (B005/B035/C010/C017/B102).
- Do NOT let the optional native launcher (Phase 5) grow beyond launch/provision into HTTP/SSE, queue/DAG, config, or feature commands — that is the broad-hybrid trap; keep it launcher-only, logic-free, reversible, or kill it.
- Do NOT big-bang or feature-freeze for a cutover — every phase must be independently shippable behind the parity gate with weekly releases continuing.
- Do NOT trust ty alone as the type gate while it is beta (0.0.21, pre-1.0) — pair it with mypy --strict as the authoritative CI gate until ty stabilizes.
- Do NOT promise a self-contained zero-Python single binary or a fixed 54.7MB tarball / Hermes cold-start — those costs are irreducible; correct that expectation with stakeholders up front.
- Do NOT drop the requires-python <3.14 ceiling until pydantic-core cp314 wheels exist; pin the bump to upstream wheel availability.