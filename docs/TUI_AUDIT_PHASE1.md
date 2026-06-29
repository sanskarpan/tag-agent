# Phase 1 — TUI Audit & Crash Analysis (TAG CLI)

**Date:** 2026-06-28 · **Runtime:** TAG v0.16.0 (hermes-agent-upstream, prepatched) · **Host:** darwin, node v25.6.1, npm 11.9.0

## Scope note / what was NOT live-tested and why

Under the standing constraint *"don't use your own Anthropic account for testing"* and with **no `OPENROUTER_API_KEY` configured in any profile** (doctor: all 5 profiles `warn`), no prompt was ever submitted to the TUI. Surfaces that require an authenticated model round-trip — live modal submit/SSE (1C), interactive keybind sweep on the Ink runtime (1D), and kill-runtime-mid-stream (1G) — are **not safely runnable** here and are deferred. They need a *throwaway provider key* (OpenRouter/Codex), not the Anthropic account, to exercise. Everything else was run for real.

## 1A — Baseline (PASS)
- `tui_dist_exists: true`, `tui_react_installed: true`, `tui_vitest_installed: true`; `dist/entry.js` = 3.1 MB, non-empty; `node_modules` present.
- `tag_bin_exists: true`, `patch_status: prepatched`, 5 profiles all `home: pass`.
- `tag tui --help` and `tag doctor --json` exit cleanly, no traceback.
- **WARN:** `python_runtime_supported: false` — host Python 3.14.3 vs runtime requirement 3.12 (mitigated by bundled 3.12.12). Cosmetic-only here, but flag for environments without the bundle.

## 1B — Cold-start launch (PASS)
Real PTY launch (stdlib `pty`, 100×30, `TAG_FORCE_TUI=1`, 8 s box) rendered the full shell: "TAG" ASCII banner, `TAG Control` rounded panel, AGENTS/CONTROL diagram, status bar (`ready │ gpt 5.4 │ 1s │ voice off │ 1 session`), `orchestrator ›` prompt. **Box borders align; no raw ANSI; no traceback.** Non-TTY invocation hits a clean guard (`TAG TUI requires an interactive terminal…`, exit 2) — good.

---

## FINDINGS

### LAUNCH BLOCKER #1 — auth-failure remediation points to a non-existent command
```
SEVERITY: HIGH (launch blocker)
COMPONENT: Error surface / rewrite_cli_hints — auth remediation hint
TRIGGER: Launch `tag tui` with no provider credentials (default fresh-install state).
SYMPTOM: TUI error panel shows: "error: agent init failed: Codex auth is missing
         access_token. Run `tag auth` to re-authenticate." — but `tag auth` is NOT a
         valid command (`invalid choice: 'auth'`). Correct command is `tag runtime auth`.
ROOT_CAUSE: src/tag/core/utils.py:143-156. The backtick-inner substitution (lines
         143-154) rewrites `\bhermes\b`->label INSIDE backticks first, turning
         `` `hermes auth` `` into `` `tag auth` ``. The `hermes auth`->`tag runtime auth`
         special-case at line 156 then no longer matches. Verified directly:
         rewrite_cli_hints("Run `hermes auth`...") => "Run `tag auth`...".
         The node-side TUI brand rewrite has the same gap.
FIX_APPROACH: Run the `hermes auth`/`hermes portal` special-cases BEFORE the generic
         backtick/quote inner substitution (reorder lines 156-157 above 143-154), and
         add the same mapping to the node-side branding so both surfaces agree.
```

### MEDIUM #2 — env-var brand leak (`HERMES_*`) survives rewrite
```
SEVERITY: MEDIUM
COMPONENT: rewrite_cli_hints — brand substitution
TRIGGER: `tag runtime chat --help` (and any text containing HERMES_<NAME> env vars).
SYMPTOM: Raw `HERMES_ACCEPT_HOOKS` printed in user-facing help — unrewritten Hermes brand.
ROOT_CAUSE: src/tag/core/utils.py:141 uses `\bhermes\b`. The trailing `_` in `HERMES_` is
         a word character, so `\b` never fires after "HERMES" — the token is skipped.
         Verified: rewrite_cli_hints("Set HERMES_ACCEPT_HOOKS=1") returns it unchanged.
FIX_APPROACH: Decide policy on env-var names (often you WANT the real name preserved so
         it still works). If rebranding: add an explicit `HERMES_`->`TAG_` rule; if not:
         leave intentionally and document it so it isn't re-flagged.
```

### MEDIUM #3 — box title shortened but not re-centered (non-`⚕` titles)
```
SEVERITY: MEDIUM
COMPONENT: _fix_box_title_alignment — box re-centering
TRIGGER: Any boxed title containing "Hermes" WITHOUT the `⚕` icon (e.g. "Hermes Status").
SYMPTOM: Brand substitution shortens the title (Hermes=6 -> tag/TAG=3) but the content
         line is NOT re-padded, so it ends 3 cols short and the box's right border pulls
         in / misaligns. Proven: a 32-wide box's title line becomes len=29.
ROOT_CAUSE: src/tag/core/utils.py:212 — recentre() early-returns unless `"⚕" in content`.
         Only medical-icon boxes are re-centered; all other shortened titles break.
FIX_APPROACH: Re-center any box whose content-line length != inner border width, not just
         `⚕` lines (drop the icon gate; key off width mismatch instead).
```

### MEDIUM #4 — inconsistent brand casing (`tag Status` vs `TAG Configuration`)
```
SEVERITY: MEDIUM
COMPONENT: rewrite_cli_hints — rule ordering
TRIGGER: Text "Hermes Status".
SYMPTOM: Renders lowercase "tag Status", while "Hermes Configuration" correctly renders
         "TAG Configuration" — inconsistent capitalization of the product name.
ROOT_CAUSE: src/tag/core/utils.py:158-163 — the generic `hermes <subcommand>`->`tag `
         rule (label is lowercase "tag") lists `status`, firing before the specific
         "Hermes Status"->"TAG Status" rule at line 196, which then can't match. "config"
         is matched by the generic rule too, but the title word is "Configuration" (not a
         listed subcommand), so the specific rule survives there — hence the inconsistency.
FIX_APPROACH: Run the specific title-case rules (lines 195-197) BEFORE the generic
         subcommand rule, or exclude capitalized title words from the generic rule.
```

### LOW #5 — leaked upstream tagline in banner
```
SEVERITY: LOW
COMPONENT: TUI banner (node-side, entry.js)
TRIGGER: Launch `tag tui`.
SYMPTOM: Banner ASCII is rebranded to "TAG" but the subtitle still reads
         "Nous Research · Messenger of the Digital Gods" (Hermes/Nous upstream tagline).
ROOT_CAUSE: Subtitle string baked into the node TUI bundle; not reachable by the Python
         rewrite_cli_hints pipeline.
FIX_APPROACH: Decide whether attribution is intended; if rebranding, patch the subtitle in
         the node TUI source/patch set (it won't be fixed by the Python rewriter).
```

---

## Coverage matrix
| Phase | Status | Notes |
|---|---|---|
| 1A Setup/baseline | ✅ Done | doctor JSON, dist verified |
| 1B Launch/cold-start | ✅ Done | real PTY render, clean |
| 1C Modals | ⚠️ Partial | error modal observed; submit/SSE needs a provider key |
| 1D Keybinds | ⛔ Deferred | needs live Ink session (upstream); throwaway key |
| 1E Profile switch | ⚠️ Partial | orchestrator launched, model indicator `gpt-5.4` shown |
| 1F Passthrough rendering | ✅ Done | core findings #1–#4 (TAG-specific) |
| 1G Error/crash recovery | ⚠️ Partial | missing-key path graceful but wrong remediation (#1); kill-mid-stream deferred |
| 1H CSS/layout | ⚠️ Partial | clean at 100 cols; box-align bug proven (#3); width sweep deferred |

## LAUNCH_BLOCKERS (must fix before public launch)
1. **#1 — `tag auth` dead remediation command** (HIGH). First thing a credential-less new user sees. Fix = reorder rewrite rules + node-side mapping. Owner: CLI/rewrite.

---

## RESOLUTION (fixed 2026-06-28)

| # | Status | Fix |
|---|--------|-----|
| #1 | ✅ Fixed (both surfaces) | **Python** `src/tag/core/utils.py` — `hermes auth`/`hermes portal` special-cases now run *before* the code-span substitution, so `hermes auth` → `tag runtime auth`. **Node** `ui-tui/src/lib/externalCli.ts` (via `src/tag/patches/hermes-ui.patch`) — same reorder; `auth`/`portal` removed from the generic subcommand list. Bundle `entry.js` rebuilt. |
| #2 | ✅ Resolved (by design) | `HERMES_*` env-var names are read by the runtime (`agent/shell_hooks.py`) — rewriting them would break functionality. Left intact deliberately; locked with `test_rewrite_cli_hints_preserves_functional_env_vars`. |
| #3 | ✅ Fixed | `_fix_box_title_alignment` now re-centres any title whose width ≠ border width (not only `⚕` panels). |
| #4 | ✅ Fixed | Title-case product strings (`TAG Status`/`TAG Configuration`/`TAG Runtime`) rewritten before the generic lowercase subcommand rule. |
| #5 | ✅ Fixed | `ui-tui/src/components/branding.tsx` (via patch) — banner tagline is now conditional: `TAG · Terminal Agent Gateway` when running as TAG, original Hermes/Nous tagline otherwise. |

### Verification evidence
- **Python:** 5 new/updated regression tests in `tests/test_controller.py`; full suite green — `test_controller`+`test_cross_cutting` 154 passed, `test_prd_features` 239 passed.
- **Node:** `createGatewayEventHandler.test.ts` updated to expect `tag runtime auth add openrouter`; 71 runnable TUI tests pass. (`brandingBanner.test.tsx` and ~30 other files cannot run in this env — pre-existing missing `packages/hermes-ink/dist/entry-exports.js` build, unrelated to these changes.)
- **Patch integrity:** `git apply --reverse --check` passes and a full reverse→forward round-trip applies cleanly to pristine upstream — fixes persist across `tag setup`.
- **Live TUI (PTY):** auth-failure panel now reads ``Run `tag runtime auth` to re-authenticate.`` (no bare `tag auth`); banner shows `Terminal Agent Gateway`.
- **Live end-to-end (real `openai-api/gpt-5-mini` call, key provided by user):** session reached `ready`, returned `OK` inside the `◈ TAG` panel with a correct `tag --resume` hint, exit 0 — credential path, provider resolution, and live-output branding all healthy. Orchestrator model config was restored to its original `openai-codex/gpt-5.4 [auto]`; the API key was env-sourced and never written to disk.

### Closed deferred coverage
1G (missing/invalid key path) and the happy-path session are now verified. Still deferred (need throwaway key + interactive driving of the upstream Ink runtime): full keybind sweep (1D), live modal submit/SSE (1C), kill-runtime-mid-stream, concurrent-session SQLite locking.

## Recommended Phase 1.5 (to close deferred coverage safely)
Provision a **throwaway OpenRouter/Codex key** in a scratch profile (NOT the Anthropic account) and re-run 1C/1D/1G-kill against it: live modal submit, full keybind sweep, `kill -9` the runtime mid-stream, `OPENROUTER_API_KEY=invalid`, corrupt-YAML config, and concurrent-session SQLite locking.
