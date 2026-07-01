# TAG — Stack Decision (Go / Rust / Python / Hybrid)

> Evidence-based evaluation of the best implementation stack for TAG, produced by a multi-agent
> research workflow (14 agents, web-grounded + codebase-grounded, adversarial verification).
> **This is a recommendation, not a mandate** — see "What would change the answer".

## TL;DR

**Recommendation:** Stay Python and modernize in place (Astral toolchain: uv + ruff + ty/mypy), hardening typing at the dispatch seams and fixing distribution/startup pains directly — do NOT rewrite the control plane in Go or Rust. Optionally adopt a single narrow, reversible native/PyApp launcher later to replace bin/tag.js, but only as an isolated distribution slice that never touches the feature modules or the SQLite writer.

**Runner-up:** HYBRID (narrow slice only): a PyApp/uv-style native launcher binary that replaces bin/tag.js and the npm-runtime venv provisioning, bundles a relocatable Python interpreter, and hands off to the unchanged Python control plane. This is the only native move with a defensible cost/benefit, and it converges with the modern-Python plan rather than competing with it. The BROAD hybrid (native also owning HTTP/SSE, queue/DAG, config) is explicitly rejected — it splits the single SQLite DB across two languages and reintroduces the non-atomic-RMW race class that 200 fixes just closed.

**Verdict:** TAG's pains and bugs are overwhelmingly language-independent and its deepest coupling is irreducibly Python, so modernize Python in place; a Go/Rust rewrite pays maximum migration cost to fix ~5-8% of bugs a linter already catches while leaving the dominant costs untouched.

**Confidence:** High

## Fit scores (per option, for THIS project)

| Option | Fit /10 |
|---|---|
| Modernized Python (uv + ruff + ty/mypy, harden in place) | **8** |
| Hybrid — narrow native launcher only | 6 |
| Go (control-plane rewrite) | 6 |
| Rust (control-plane rewrite) | 4 |

## Adversarial verdicts (3 independent lenses — all converged)

| Lens | modern-python | hybrid | go | rust | pick |
|---|---|---|---|---|---|
| Distribution & operability | 8 | 7 | 6 | 5 | modern-python |
| Velocity & risk | 9 | 6 | 4 | 3 | modern-python |
| Architecture fit | 8 | 6 | 6 | 4 | modern-python |

## Weighted decision criteria

| Criterion | Weight | Winner |
|---|---|---|
| Migration cost & preservation of the 200-bug / 678-test hardening corpus | 25% | modern-python |
| Solo-maintainer velocity & single-toolchain leverage (bus-factor-of-one) | 20% | modern-python |
| Fit to the irreducible Python-Hermes seam (inline-Python execs, branding patch, subprocess boundary) | 15% | modern-python |
| Single-owner integrity of the one SQLite state store (no cross-language writers) | 15% | modern-python |
| Distribution & cold-start UX (venv provisioning, system-Python dependency, artifact size) | 10% | hybrid (narrow launcher) / modern-python via uv — tie, both in-ecosystem |
| Correctness gains actually reachable (dispatch-crash class) | 8% | modern-python (ty+pydantic gets same class as Go/Rust) |
| Long-lived-server concurrency (SSE/dashboard/queue workers) | 5% | go (goroutines) — but shallow: SQLite contention is language-independent and api.py already fixed via ThreadingHTTPServer |
| Agent/LLM/MCP ecosystem access | 2% | modern-python (Python-first; TAG does no agent work itself) |

## Rationale

Three independent adversarial lenses — Distribution/Operability, Velocity/Risk, and Architecture Fit — all ranked modern-python first (8, 9, 8) and rust last (5, 3, 4), which is a strong convergence signal. The decision rests on four verified facts about THIS codebase, not generic language preference. (1) Bug forensics: only ~11 of the 200 just-fixed bugs (~5-8%) were language-attributable dynamic-dispatch crashes (wrong-kwarg / dict-vs-object, B001/B004/B006/B011/B012/B013/B022); the other ~92% were logic/validation/contract/concurrency-SEMANTICS bugs (cron off-by-one, POSIX dom/dow OR-semantics, `x or default` clobbering 0, missing HMAC/SSRF guards, non-atomic SQLite RMW races) that recur identically in Go or Rust. A rewrite discards the 678-test gate and the 200-bug hardening corpus, then re-earns the 92% fresh. The 5-8% it would prevent is caught in-place by ty/mypy --strict + pydantic at the ~14 cmd/*.py seams and core/run.py — and ty==0.0.21 + ruff==0.15.10 are ALREADY pinned in the dev extra (pyproject.toml:121), so the toolchain is half-adopted. (2) Irreducible Python coupling: run_profile_python (run.py:41-50) execs inline Python STRINGS into the Hermes venv, credential import execs inline Python against hermes_cli.auth, and hermes-ui.patch edits Hermes Python source — none of this ports. Any non-Python control plane still ships, provisions, and subprocesses Python. (3) Distribution ceiling: the win is hard-capped by a verified 54.7MB Hermes tarball (src/tag/vendor/hermes-agent-upstream.tar.gz) that ships in every artifact and needs its OWN pip venv via tag setup. A native binary removes only the lighter of two interpreters and one of two venvs; the tarball, the Hermes cold-start (dominant wall-clock), the branding git-patch, and the ui-tui npm build all remain. uv + python-build-standalone attacks the same #1 pain (system-Python-on-PATH, slow venv provisioning) in-ecosystem — exactly what aider, TAG's only true language-peer, actually shipped instead of rewriting. (4) Maintainer reality: bus-factor-of-one (601/629 commits) on a just-stabilized codebase cannot safely absorb a dual-toolchain, dual-CI, feature-frozen rewrite; Codex needed multi-quarter parallel dual-maintenance to reach parity even as a well-funded team. Net: modern-Python delivers the achievable slice of every benefit (faster provisioning via uv, one-fetched-binary UX via optional PyApp, the same crash-class safety via ty+pydantic, cleaner SSE via ThreadingHTTPServer/uvicorn already-a-dep) at ~1% of the cost and risk, while keeping TAG in the Python-first agent/MCP ecosystem where its features live.

## Key risks / caveats

- ty is still beta (1.0 targeted 2026, not a mypy drop-in) — mitigate by pairing ty with mypy --strict in CI until ty stabilizes, so the type-gate on the dispatch seams is trustworthy.
- Modernizing does NOT remove the two irreducible costs: the 54.7MB tarball ships in every artifact and the Hermes venv cold-start (the dominant wall-clock) stays. If leadership's actual goal is a truly self-contained zero-Python binary, no option delivers it — that expectation must be corrected up front.
- The long-lived single-thread stdlib http.server foot-gun (devui.py, webhook_server.py; B031-class) remains until explicitly fixed — but this is a one-line ThreadingHTTPServer/uvicorn swap, not a language problem; schedule it as a discrete fix.
- The <3.14 Python ceiling (pydantic-core cp314 wheels) persists regardless of choice and blocks nothing today, but pin the upgrade to upstream wheel availability so it does not silently rot.
- Scope-creep on the optional narrow launcher: if a native launcher ever grows to own HTTP/SSE, queue/DAG, or config, it splits the single SQLite DB across two languages and reintroduces the most-repeated bug family (non-atomic RMW races). Keep the native slice strictly launcher-only, logic-free, and reversible.
- Modernization is real work, not a no-op: lazy-importing the 13 eager cmd modules in build_parser and annotating the seams must be done carefully to avoid regressing the just-stabilized surface — gate every change behind the existing 678-test suite.

## What would change this answer

The recommendation would flip toward a native (Go, not Rust) rewrite of the control plane if the hard constraint dissolved — i.e., if Hermes were reimplemented or replaced by a non-Python runtime, or exposed as a stable remote/wire service so TAG no longer had to ship and provision a bundled Python venv. It would also shift if the project's center of gravity moved from the ~103 thin Hermes-wrapping commands to a fleet of long-lived, high-concurrency, high-throughput servers where goroutine-class concurrency and instant startup dominated real user value (and SQLite were replaced by a server DB removing the single-writer constraint), OR if the team grew past bus-factor-of-one with committed Go maintainers making dual-toolchain maintenance sustainable. Even then Go would be preferred over Rust: Rust's one differentiator here (PyO3 in-process embedding) is unusable because it breaks the per-profile HERMES_HOME venv isolation and crash-isolation the profile-routing model depends on, and its velocity tax on a small team is the worst of any option. If none of these change, stay Python.

## How comparable AI-agent CLIs are actually built (2026 precedent survey)

- aider (Aider-AI): PYTHON. Distributed on PyPI as `aider-chat` (pip, Python 3.9-3.12). Notably ships a SEPARATE bootstrap package `aider-install` that installs only `uv`, then runs `uv tool install --python python3.12 aider-chat` into an ISOLATED tool env so 'only 2 packages (uv + aider-install) touch the base Python env' and uv auto-installs Python 3.12 if absent. Directly relevant to TAG: the one precedent whose language situation matches TAG (Python) solved Python-provisioning/dependency-pollution pain with uv, NOT a rewrite. Sources: pypi.org/project/aider-install, aider.chat/docs/install.html, github.com/aider-ai/aider.
- Claude Code (Anthropic): TypeScript on Node.js. Distributed via npm `@anthropic-ai/claude-code` (Node 18+), plus a native installer in 2026. Chose TS/Node for ecosystem reach and standalone CLI ergonomics; ships MCP + hooks extensibility. Source: platform.claude.com/docs, nxcode.io install guide.
- OpenAI Codex CLI: REWROTE TypeScript/React/Node -> RUST (codex-rs, ~95% Rust by late 2025). Stated reasons: (1) 'zero-dependency install' - Node v22+ requirement was 'frustrating or a blocker'; (2) lower memory, no GC; (3) native OS sandboxing (macOS sandbox-exec, Linux Landlock/seccomp) via existing Rust bindings; (4) a 'wire protocol' so TypeScript/Python/other langs can extend the agent + native MCP. Distribution: npm wrapper `@openai/codex` that ships/downloads the native binary (native now default), standalone GitHub-release binaries, and Homebrew. Kept TS version in parallel for bug fixes until Rust parity. Sources: github.com/openai/codex/discussions/1174, infoq.com/news/2025/06/codex-cli-rust-native-rewrite.
- block/goose (now Agentic AI Foundation / Linux Foundation): RUST. Ships as a single native binary — full CLI plus an Electron desktop app, macOS/Linux/Windows. Chose Rust for speed, portability, single-binary distribution, no central control plane/telemetry. 15+ providers, 70+ MCP extensions. ~49.8k stars, Apache-2.0. Sources: github.com/block/goose, goose-docs.ai, terminaltrove.com/ai-coding-agents/goose-cli.
- sst/opencode (Anomaly Innovations, ex-SST): TypeScript on the BUN runtime + Hono framework, client-server architecture (core agent runs as local server; TUI/desktop/IDE/CI are clients). Built in-house OpenTUI (TypeScript TUI framework with Zig bindings) replacing Go Bubble Tea in v1.0. History: original 'opencode' was GO; a 2025 split archived the Go codebase (which became Charm's Crush) while SST kept the name and rebuilt in TypeScript. Distribution: npm / curl installer. Source: opencode.ai/docs, aiwiki.ai/wiki/opencode, openaitoolshub.org review.
- cursor-cli / cursor-agent (Cursor/Anysphere): closed-source NATIVE binary. Install via `curl https://cursor.com/install -fsSL | bash`, Homebrew cask `cursor-cli`, or platform archives (agent-cli-package.tar.gz) from downloads.cursor.com. No Node/Python runtime requirement — ships a self-contained native agent binary. Sources: cursor.com/cli, cursor.com/docs/cli/installation, formulae.brew.sh/cask/cursor-cli.
- Continue (continuedev) `cn` CLI: TypeScript. Distributed via npm `@continuedev/cli` (Node 20+), plus Homebrew and direct download. Same agent that powers the Continue IDE extensions, run headless in terminal (Alpha in 2026). Chose TS to share the existing IDE-extension codebase. Sources: docs.continue.dev/cli, npmjs.com/package/@continuedev/cli.
- charmbracelet/crush: GO. Single cross-platform binary <10MB (macOS/Linux/Windows PowerShell+WSL/FreeBSD/OpenBSD/NetBSD). Widest install matrix of any peer: GitHub binaries, `go install`, Homebrew, npm (`@charmland/crush`), Arch (yay), Nix, Winget, Scoop. LSP for code intel; MCP over http/stdio/sse. Inherits the archived Go opencode lineage + Charm TUI ecosystem (Bubble Tea). Sources: github.com/charmbracelet/crush, toolhunter.cc/tools/crush.

*Takeaway: the only true language-peer (aider — a Python CLI wrapping Python) stayed Python and
shipped uv-based provisioning; the Rust/Go single-binary peers (Codex, goose, crush) do NOT wrap a
bundled Python runtime, so their distribution win doesn't transfer to TAG's Hermes-Python seam.*