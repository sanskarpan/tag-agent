# TAG — Complete Feature List

> Consolidated, status-verified feature inventory for the TAG agent-orchestration platform.
> Cross-checked against the live CLI surface and the PRD catalog (**PRD-001–133**,
> clusters A–K). Sources: `docs/prd/INDEX.md`, `docs/FEATURES_ROADMAP.md`, and `src/tag/`.

**Legend:** ✅ implemented & shipping (working command) · 🔶 partial (engine exists, no dedicated command) · 📋 planned/proposed (PRD written, not yet built)

**Verified column:** ✅ = exercised and working on the stated edition(s) · ⚠️ = edition gap or caveat (see note) · — = not applicable.

---

## ✅ Verification status — audited 2026-08-24

This inventory was re-verified by exercising **every** top-level command on **both**
editions (Python `tagpy` and the Go `tagqa` binary), from a fresh bootstrapped
`TAG_HOME`, plus a full source cross-check of the PRD catalog. Method: 7 parallel
audit agents, one per feature cluster, running `--help` + a read-only/local
invocation for each command and grepping the implementation.

**Result:** every feature marked ✅ below is present and its local/read-only path
works on at least the Python edition; most work on both. **No feature marked ✅ is
missing or broken.** Findings were limited to (a) intentional edition differences
(the Go native harness omits some managed-runtime passthroughs — see
[`tag-go/MIGRATION_STATUS.md`](../tag-go/MIGRATION_STATUS.md)), and (b) a handful of
Python↔Go polish gaps that were **fixed during this audit** (empty-states,
`annotate stats --json`, `alert create` positional form, `webhook --platform
generic`, `serve`/`web` `/health` + JSON 404).

> **Runtime caveat.** Commands that *execute* a prompt (`run`, `submit`, `models`
> live inventory, `benchmark run`, chat/loop execution) drive the managed "hermes"
> runtime, which cannot build its venv in an offline CI sandbox. Those paths are
> marked ✅ (present, parse, reach execution) but their *output* was not exercised
> here; their command surface and validation are verified.

> **Which edition these counts describe.** This page covers *both* the Python edition
> and the native Go harness, and they do **not** have the same command surface. Counts
> re-measured 2026-08-24:
>
> | Edition | Top-level commands | How counted |
> |---|---|---|
> | **Python** (`pip install tag-agent`) | **107** | recursive walk of `tag.controller.build_parser()` |
> | **Go** (`tag-go`, single binary) | **~90** | recursive `--help` sweep of the built binary |
>
> The Python count grew from 103 → 107 since the last revision: **`run`**, **`guardrail`**,
> **`tripwire`** and **`doc`** were added. Go-specific gaps (`submit`, `openrouter-models`,
> `queue-dep`, `kanban`, `dashboard`, `desktop`, `sessions`/`skills`/`chat`/`config`/
> `status`/`update` passthroughs; `agentic-ci` lacks `install-action`; `lsp` hover-only)
> are intentional native-harness differences tracked in
> [`tag-go/MIGRATION_STATUS.md`](../tag-go/MIGRATION_STATUS.md). **Correction:** the Go
> `swarm` surface is now complete (run/list/status/abort) — the previously-noted "no
> `swarm run`/`abort`" gap no longer exists.

**At a glance:** ~77 features implemented across the Python edition's 107 commands
(PRD-001–072 + PRD-121/122/123/124 + PRD-133) · ~50 planned (PRD-073–120, 125–132).

---

## 0. Core platform (foundation)

| Feature | Verified | Note |
|---|---|---|
| ✅ **Control-plane CLI** wrapping the Hermes runtime — the `tag` binary | ✅ | 107 top-level cmds (Python); ~90 (Go) |
| ✅ **Multi-profile orchestration** — 5 built-in profiles | ✅ | orchestrator/researcher/coder/reviewer/codex-runtime-master, created on `bootstrap` |
| ✅ **Task routing engine** — 4 routes; master/worker/verifier; Kanban vs direct | ✅ | `route` resolves roles per task type, both editions |
| ✅ **Managed runtime provisioning** — `setup`, Hermes tarball, branding, TUI build | ✅ | present both; venv build is env-gated offline |
| ✅ **Dual distribution** — pip + npm (auto-venv); Python 3.11–3.13 | ✅ | declared in pyproject.toml / package.json |
| ✅ **Branding layer** — Hermes→TAG dual-surface rewrite | ✅ | "TAG — The Agent Gateway" in both binaries |
| ✅ **SQLite state** — runs/steps/spans/memory/queue (WAL, atomic + locked writes) | ✅ | `tag.sqlite3` + `-wal`/`-shm` present; shared `(key,value)` maintenance schema |

---

## 1. Setup, diagnostics & config

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `setup`, `bootstrap`, `render`, `env` | Provision runtime, render per-profile config | — | ✅ | Python; Go has no `render` (uses native config) |
| ✅ | `doctor` | Health check (pass/warn/fail per component) | PRD-009 | ✅ | full report both editions |
| ✅ | `config`, `status`, `update` | Config passthrough, status, self-update | — | ⚠️ | Python only — Go native harness omits these passthroughs |
| ✅ | `runtime`, `tui`, `chat`, `gateway`, `completion`, `prompt-size`, `logs`, `sessions`, `skills`, `plugins`, `tools`, `mcp`, `model`, `dashboard` | Managed-runtime passthrough surface | — | ⚠️ | Python full; Go replaces several with native equivalents (`serve`/`web`/`tui`, `mcp-connect/-serve/-registry`, `tool-index`, `set-model`) |

## 2. Credential import (18 sources)

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `import-codex/claude/gemini/continue/mistral/opencode/zed/copilot/aider/aws/cursor/supermemory/honcho/nous-portal` | Multi-source credential import | PRD-001, PRD-006 | ✅ | all 14 present & parse, both editions |
| ✅ | `import-docker/ssh/modal/daytona` | Execution-backend selection per profile | PRD-005 | ✅ | present both; success path env-gated offline |

## 3. Routing & models

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `route`, `assignments`, `set-model`, `runs` | Task routing, model assignment, run history | — | ✅ | both editions; `route`/`assignments` `--json` identical |
| ✅ | `models`, `openrouter-models` | Model inventory | — | ⚠️ | Python `models` needs live runtime (Go reads config offline); `openrouter-models` Python-only |
| ✅ | `submit` | Route + execute | — | ⚠️ | Python only (Go uses `run`); execution env-gated |
| ✅ | `benchmark`, `compare` | Multi-model benchmarking & comparison | PRD-017 | ✅ | `list`/`show` work both; `benchmark run/list/show` group + flat form; Go `compare` lacks `run` |
| ✅ | `route-fallback` | Model fallback chains (cycle detection); `run --fallback` | PRD-031 | ✅ | add/list/resolve/remove + cycle detection, both editions |

## 4. Memory subsystem

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `memory-journal` | Cross-session memory journal | PRD-002 | ✅ | save/list/forget/clear both |
| ✅ | `mem` | Semantic memory with confidence decay + FTS | PRD-025 | ✅ | add/list/search/stats; Go search = hybrid RRF |
| ✅ | `mem2 gc` | Sleep-time consolidation / GC | PRD-068 | ✅ | `--dry-run` both; Go adds `--daemon` |
| ✅ | `mem2 extract` | Automatic post-run memory extraction | PRD-065 | ✅ | both editions |
| ✅ | `mem2 tier` | Hierarchical memory tiers | PRD-067 | ✅ | core/recall/archival both |
| ✅ | `mem2 fact` | Temporal fact versioning | PRD-069 | ✅ | update/history/list-at both |
| ✅ | `mem2 episode` | Episodic memory | PRD-071 | ✅ | start/end/list/get both |
| ✅ | `mem2 store` | Cross-session vector store / hybrid search | PRD-066, PRD-072 | ✅ | store/search/rebuild both |
| ✅ | `graph` | Entity graph + community detection | PRD-070 | ✅ | show/query/build; community detection surfaces both |
| ✅ | (per-profile config) | Structured memory configuration | PRD-001 | ✅ | config-backed |

## 5. Queue, DAG & swarm

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `queue` | Background task queue + notifications | PRD-008 | ✅ | add/list/result/cancel/clear/**worker**; enqueue-only + 4-field `list --json`, both |
| ✅ | `dag`, `queue-dep` | Dependency-aware DAG engine (cycle detection) | PRD-033 | ✅ | `dag` both (Go adds `state`); `queue-dep` Python-only (Go folds into `dag`) |
| ✅ | `swarm` | Multi-agent swarm, context routing | PRD-004, PRD-023 | ✅ | run/list/status/abort/results **both** (Go gap closed) |
| ✅ | `kanban` | Kanban topology helpers | PRD-004 | ⚠️ | Python only |

## 6. Observability & cost

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `costs`, `pricing` | Cost tracking / per-span USD attribution | PRD-012, PRD-046 | ✅ | both; Go richer (`--run-id`/`--by`) |
| ✅ | `trace` (list/show/export/**replay**/diff/checkpoint/snapshot) | Tracing + time-travel/replay | PRD-013, PRD-032 | ✅ | all 7 subs both; not-found → `{"error":...}`+exit 1 both |
| ✅ | `cache` | Prompt-cache analytics | PRD-030 | ✅ | stats/trend/tips both |
| ✅ | `otel-export` | OTel GenAI semconv span export | PRD-041, PRD-048 | ✅ | OTLP/JSON both; errors on unknown `--trace-id` |
| ✅ | `agentops` | AgentOps session observability | PRD-044 | ✅ | both editions |

## 7. Eval & quality

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `eval` | Eval framework | PRD-027 | ✅ | list; show bad → `{"error"}`+exit 1 both |
| ✅ | `eval-judge` | LLM-as-judge evaluators | PRD-045 | ✅ | both editions |
| ✅ | `eval-dataset` | Versioned eval dataset management | PRD-049 | ✅ | both editions |
| ✅ | `eval-ci` | Eval CI gate + PR comment + GH Action | PRD-047 | ✅ | run + scaffold both |
| ✅ | `alert` | Alert rules on metric thresholds | PRD-050 | ✅ | create (positional+flags), list/firings empty-states, enumerated metric error — Python fixed 2026-08-24 |
| ✅ | `annotate` | Human annotation / labeling queue | PRD-051 | ✅ | `stats` human default + `--json` — Python fixed 2026-08-24 |
| ✅ | `prompt` | Prompt versioning hub | PRD-052 | ✅ | list empty-state — Python fixed 2026-08-24 |

## 8. Agent tools

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `security` | Secret scanning & security audit | PRD-034 | ✅ | finds secret, exit 1, values not shown |
| ✅ | `persona` | Agent personas | PRD-037 | ✅ | `list --json` has id/inject/tags **both** (Go added 2026-08-24) |
| ✅ | `diff-context` | Diff-aware context injection | PRD-038 | ✅ | both editions |
| ✅ | `budget` | Token budget enforcement | PRD-039 | ✅ | set/check both |
| ✅ | `notify` | Notification hooks (Slack/email/desktop) | PRD-040 | ✅ | add/list/test/remove/enable/disable |
| ✅ | `split` | Architect/editor agent split | PRD-042 | ✅ | list/show/plan both |
| ✅ | `tool-index` | Vector-based tool retrieval | PRD-043 | ✅ | index/search/status both |
| ✅ | `sandbox` | Isolated code execution (restricted / Docker) | PRD-028 | ✅ | `run 'echo hi'` → hi, exit 0 |
| ✅ | `context` | Context-window management | PRD-018 | ✅ | show/compress/trim both |
| ✅ | `tripwire`, `guardrail runtime` | Runtime content guardrails + tripwire | **PRD-123** | ✅ | list/check/test/history/add/remove; fires (exit 3), secrets **fully redacted** — **both editions** |
| ✅ | `guardrail input` | Input guardrails: prompt-injection/pii/secret/length-limit; block/sanitize/warn | **PRD-122** | ✅ | add/list/remove/test/history; PII sanitize; **both editions** |
| ✅ | `guardrail output` | Output guardrails: pii/secret/json-schema/profanity; block/rewrite/warn | **PRD-121** | ✅ | add/list/remove/test/history; **both editions** |
| ✅ | (library) | GuardrailResult shared result type | **PRD-124** | ✅ | Go `internal/guardrail` + Python `content_guardrail` |

## 9. CI/CD & agentic dev workflows

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `review-pr`, `ci` | CI/CD integration + PR-review signal classes | PRD-020, PRD-061 | ✅ | both; exec env-gated |
| ✅ | `loop` | Autonomous agent loop (goal detection, iteration cap, approve/deny) | PRD-021 | ✅ | subcommands **and** bare `loop "<prompt>" --iterations N` both |
| ✅ | `cron` | Cron-style scheduled agent runs | PRD-022 | ✅ | add/list/enable/disable/run both |
| ✅ | `workspace` | Repo-map / workspace context indexing | PRD-024 | ✅ | index + map both (~500 files) |
| ✅ | `issue-solve` | Issue-to-PR autonomous loop | PRD-055 | ✅ | both editions |
| ✅ | `webhook` | Inbound webhook trigger server (HMAC) | PRD-056 | ✅ | `rule-add --platform generic` accepted **both** (Python fixed 2026-08-24) |
| ✅ | `agentic-ci` (test-gen/gen-pipeline/fix-vuln/ci-diagnose/flaky-fix + `install-action`) | Automated dev workflows | PRD-057–063 | ⚠️ | Python 7 subs; Go 6 (lacks `install-action`) — gap nearly closed |
| ✅ | `swe-solve` | SWE-agent bash/editor harness | PRD-064 | ✅ | both editions |

## 10. Marketplace, plugins, templates, MCP & documents

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `marketplace` | Profile marketplace (pull/push) | PRD-026 | ✅ | list/pull/push both |
| ✅ | `template` | Profile templates & sharing | PRD-015 | ✅ | export/import/fetch both |
| ✅ | `hooks` | Webhook / lifecycle event hooks | PRD-016 | ✅ | list/log/test both |
| ✅ | `mcp-registry` | Curated MCP server registry | PRD-014 | ✅ | both; Go adds add-curated/list-curated |
| ✅ | `plugin` | Plugin management | PRD-011 | ✅ | list/install/enable/disable both |
| ✅ | `shell` | Natural-language TAG shell | PRD-019 | ✅ | both editions |
| ✅ | `doc` (check/read) | Document ingestion — PDF | **PRD-133** | ✅ | engine detection, nonexistent→not found, `--json` engine `""`; **both editions** |

## 11. Dashboards, UI & IDE

| Status | Command(s) | Feature | PRD | Verified | Note |
|---|---|---|---|---|---|
| ✅ | `serve`, `web`, `dashboard` | HTTP dashboard + admin panel (SSE) | PRD-010, PRD-036, PRD-029 | ✅ | `serve`/`web` now expose `/health` + JSON 404 (Python fixed 2026-08-24); `dashboard` Python-only |
| ✅ | `devui` | Local browser DevUI | PRD-054 | ✅ | `/health` 200 + JSON 404 both |
| ✅ | `lsp` | IDE bridge / LSP server | PRD-035 | ⚠️ | both; Go hover-only (depth gap) |
| ✅ | `desktop` | Electron desktop app launcher | PRD-007 | ⚠️ | Python only |
| ✅ | (runtime) | Rich streaming TUI (spinners/progress/status bar) | PRD-003 | ✅ | `tui` both |

Library-level features backing the above (no dedicated command): TraceProcessor lifecycle hooks (PRD-053),
structured tool-call child spans (PRD-048).

---

## 12. ✅ Guardrail siblings (cluster J) — now implemented

The guardrail cluster is now built out on **both distributions**:

| Status | PRD | Feature | Evidence |
|---|---|---|---|
| ✅ | PRD-121 | Output guardrail processor | `tag guardrail output add/list/remove/test/history` — pii/secret/json-schema/topic-filter/profanity/toxicity |
| ✅ | PRD-122 | Input guardrail validator | `tag guardrail input …` — prompt-injection/pii/secret/topic-filter/length-limit; block/sanitize/warn |
| ✅ | PRD-123 | Runtime guardrail hooks | `tripwire` + `guardrail runtime` (see §8) |
| ✅ | PRD-124 | GuardrailResult type | shared value type in Go `internal/guardrail` + Python `content_guardrail` |

`PRD-125` (constitutional-AI policy) remains 📋 planned (deliberately unbuilt).

> **Runtime enforcement (live).** The input/output chains gate an actual `tag run`: input guardrails
> screen the prompt **before** the model (a block short-circuits the run; a sanitize threads the
> redacted prompt onward), output guardrails screen the response **after** (a block replaces it), and a
> fired guardrail exits 3. Go wires this into the native agent loop (`internal/agent`); Python screens
> around `run_chat_step` (the input path works fully offline). PRD-121 FR-08 / PRD-122 FR-09 satisfied.

---

## 13. 📋 Planned / proposed — clusters D–K (PRD-073–132)

> PRDs written during the v0.6.x–v0.7.x planning cycles; not yet implemented as commands.
> Cross-checked 2026-08-24 against the live surface + `src/tag/` and `tag-go/` — **no false
> "planned" flags found**: none of these ship a command today. See `docs/prd/` for each spec.

### D · MCP ecosystem & tool connectivity (PRD-073–080)
📋 Live MCP registry sync · MCP OAuth PKCE/device flow · per-user entity-scoped multi-tenant tool auth ·
high-value MCP server bundle · scope-based tool filtering · HITL tool approval + audit trail ·
cloud-hosted tool execution · enterprise IdP/SSO for MCP servers.

### E · Multi-agent interoperability (PRD-081–088)
📋 A2A agent-card publication · multi-agent team primitives · agent-as-tool pattern · A2A signed agent
cards · formal handoff message primitive · ANP identity layer (W3C DID) · ACP lightweight REST adapter ·
distributed agent runtime (gRPC).

### F · Sandbox & execution environment (PRD-089–100)
📋 Sandbox streaming stdout/stderr · template/snapshot system · configurable TTL + session refresh ·
desktop-GUI sandbox (VNC) · GPU sandbox (Modal) · per-sandbox egress firewall · pause/resume ·
persistent volume mounts · sandbox secrets vault · stdin/signal delivery · per-second cost attribution ·
sandbox lifecycle policies. *(Current `sandbox` = run/list/result only.)*

### G · Advanced reasoning & planning (PRD-101–108)
📋 Self-consistency ensemble · multi-agent debate · dynamic task-type classifier (embeddings) ·
node-level cache TTL · TDAG dependency-first decomposition · speculative action execution ·
confidence-aware model routing · Magentic-One orchestrator.

### H · Agentic workflow state & graph (PRD-109–116)
📋 Human-in-the-loop interrupt · loop-state serialization · dynamic fan-out/map-reduce ·
graph-based workflow · time-travel debugging · team-orchestration primitives · stateful process
framework · memex persistent scratchpad. *(Note: a Go `hitl` package backs loop approvals, but the
generic `workflow interrupt()`/resume of PRD-109 is not built.)*

### I · Computer use & browser automation (PRD-117–120)
📋 Playwright MCP integration · computer-use CLI · Claude computer-use screenshot loop ·
desktop-GUI sandbox (VNC).

### J · Security & guardrails (PRD-121–125)
✅ **PRD-121 (`guardrail output`), PRD-122 (`guardrail input`), PRD-123 (`tripwire`/`guardrail runtime`), PRD-124 (GuardrailResult) — all IMPLEMENTED** on both editions (see §8, §12). 📋 PRD-125 constitutional policy remains planned.

### K · Sakana-gap features (PRD-126–127) + newer PRDs (128–132)
📋 `tag solve` — inference-time multi-model tree search (AB-MCTS) · `tag evolve` — evolutionary
profile-config optimization.
📋 **PRD-128** agent-skills (SKILL.md packages) — *note:* the existing plural `skills` command is a
managed-runtime passthrough, **not** TAG's own SKILL.md package system · **PRD-129** plan/act execution
mode · **PRD-130** git-worktree isolation · **PRD-131** Zed ACP editor bridge · **PRD-132** provider-adapter
breadth + model catalog.
✅ **PRD-133** document ingestion (PDF) — **IMPLEMENTED** as `tag doc` (see §10).

---

## Summary

| | Count |
|---|---|
| ✅ Implemented features | ~77 (PRD-001–072 + PRD-121/122/123/124 + PRD-133) |
| 🔶 Partially delivered | 0 (PRD-121/122/124 now fully implemented) |
| ✅ Live CLI commands — **Python** edition | **107** top-level |
| ✅ Live CLI commands — **Go** harness | ~90 top-level |
| 📋 Planned features (PRD-073–120, 125–132) | ~50 |
| **Total PRDs cataloged** | **133** |

*Verified 2026-08-24 by exercising every command on both editions (7 parallel audit agents) and
cross-checking `docs/prd/` against `src/tag/` + `tag-go/`. Implemented = a working command exists and
its local path was verified; partial = engine present, no command; planned = PRD spec only.*
