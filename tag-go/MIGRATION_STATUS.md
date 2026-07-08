# TAG → Go Migration Status

Native Go port of the Python TAG control plane, per `../docs/GO_MIGRATION_PLAN.md`.
Single static binary (`CGO_ENABLED=0`, ~18 MB), owns its own SQLite runtime
(`modernc.org/sqlite`, FTS5, WAL, single-writer). Module: `github.com/tag-agent/tag`.

**Status: feature-complete + adversarially audited.** 67 top-level commands · 25 packages ·
182 test funcs · `gofmt`/`go vet` clean · `go test ./...` green · 159-command `--help` sweep passes.
A 5-agent adversarial audit (read + RUN) found ~30 real bugs behind the green suite; all
critical/high are fixed and regression-tested — see the "Audit fixes" section below.
Both tracks are done: the full control plane (Track A) and the native runtime
(Track B — multi-provider LLM, agent loop, tools, MCP client+server+subprocess,
HTTP servers, LSP, TUI). The only intentional non-ports are live-model execution
paths, exercised via the offline `echo` provider per the no-model-calls constraint.

Faithful port discipline: behavior is verified by **running the binary** in isolated
`TAG_HOME` sandboxes (not just unit tests) — the Python audit lesson that the unit
suite masks dispatch-layer bugs applies equally here. Known Python quirks are
preserved intentionally (e.g. substring keyword matching in the entity graph;
`entities-processed` vs `distinct-entities` counts in `graph build`).

## Ported & tested (Track A — control plane)

| Group | Commands | Backing |
|---|---|---|
| system | bootstrap, doctor, env, setup, version | config + paths |
| mem / memory-journal | add, search, list, forget, stats; save/list/forget | `internal/memory` (BM25 decay) |
| budget | set, get, list, remove | token_budgets |
| persona | list, apply, stack, remove | personas / active_personas |
| route-fallback | add, list, resolve (BFS cycle detection) | route_fallbacks |
| **routing** | **route, assignments, set-model, models** | config profiles |
| cron | add, list, remove, next | `internal/cron` (hardened matcher) |
| queue / dag | add, list, cancel; save, list | queue_jobs / queue_dags |
| security | scan, list | `internal/security` (entropy + patterns) |
| workspace | index, status | workspace_files (SHA256) |
| observability | costs, pricing, trace | embedded pricing table |
| **notify** | **add, list, test, remove, enable, disable** | notification_hooks |
| **graph** | **show, query, build** | `internal/graph` (union-find communities) |
| **prompt** | **save, get, list, versions, diff** | prompt_versions (LCS diff) |
| **alert** | **create, list, check, firings, delete** | alert_rules / alert_firings (cooldown-suppressed) |
| **annotate** | **add, next, label, skip, stats, export** | annotation_tasks (atomic priority claim, jsonl/csv) |
| **eval-dataset** | **create, add-case, list, export, delete** | eval_datasets / eval_dataset_cases (YAML export, C022) |
| **mem2** | **gc, tier, episode, fact** | `internal/memory/{gc,episode,fact}.go` (evict/merge/promote; episodes; temporal fact versioning) |
| **diff-context** | (single cmd) | `internal/diffcontext` (git-exec, secret/binary filter, token estimate) |
| **hooks** | **list, log, test** | config `hooks` section + hook_log; shell-safe {{var}} interpolation |
| **mcp-registry** | **list, install, enable, disable** | embedded 10-server catalog; profile config.yaml read/write |
| **template** | **export, import** | profile-home .env/config.yaml; secret redaction, 0600, traversal guard |
| **compare** | **list, show** | benchmark_comparisons / benchmark_results (run path is Track-B) |
| **plugin** | **list, enable, disable** | embedded plugin catalog; TAG_PLUGIN_*_ENABLED in profile .env |
| **eval** | **list, show** | eval_runs / eval_cases (run path is Track-B) |
| **swarm** | **list, status, results** | swarm_runs / swarm_tasks (run/abort are Track-B) |

| **run** | (native agent loop) | `internal/agent` + `internal/tool`; drives a provider through tool turns, records usage to `runs` |
| **serve** | (HTTP dashboard) | `internal/server`; loopback dashboard + `/api/snapshot` + `/events` SSE |
| **tool-index** | **index, search, status** | keyword retrieval over the embedded MCP registry |
| **cache** | **stats** | prompt-cache hit rate + token totals per profile/model (from `runs`) |
| **otel-export** | (single cmd) | spans → OTLP/JSON with OTel GenAI semconv attributes |
| **webhook** | **listen, rule-add, rule-list, events** | `internal/webhook`; HMAC verify (GitHub/Slack/Linear), rule match, enqueue |
| **import-*** | codex/claude/gemini/continue/mistral/opencode/zed/copilot/aider | `internal/importer`; read source-tool creds → profile .env (0600) |
| **mcp-serve** | (MCP server) | `internal/mcp` server side; exposes echo/now/tag_profiles over JSON-RPC stdio |
| **eval-ci** | **scaffold, run** | `internal/ciauto`; GitHub Actions YAML scaffold (byte-identical to Python); run is dry-run offline |
| **ci / loop** | (agent-loop drivers) | drive `internal/agent` loop via a provider (echo default, offline) |
| **marketplace** | **list, pull, push** | `internal/marketplace`; SSRF-guarded fetch, cache table |
| **agentops** | (session observability) | rollup over `runs` (per-profile runs/tokens/cost/status) |
| **shell** | (stub REPL) | reads stdin line-by-line; pipe-friendly stub |

**Bold** = ported this pass. **~45 command groups / 62 top-level commands**, 132 Go test funcs,
16 tested packages, full `go test ./...`
+ `go vet` + `gofmt -l` clean; `--help` sweep passes.

**Track-B runtime core is now working and tested offline:** `tag run <prompt>`
drives the native agent loop (`internal/agent`) through tool-calling turns using
the built-in tools (`internal/tool`: bash/read_file/write_file/list_dir, sandboxed),
defaulting to the offline `echo` provider and recording each run to the `runs`
table. The only piece left for live operation is a real provider adapter
(anthropic/openai) that registers into `llm.Registry` — a drop-in behind the
existing interface, deliberately not live-tested per the no-model-calls constraint. The mcp-registry
enable/disable added reusable `loadProfileConfig`/`writeProfileConfig` helpers
(runtime profile-home YAML), which `template` import/export builds on.

**Correctness fix to the base memory subsystem (found while porting mem2):** decay
now uses Python's type-specific half-lives (`convention` never decays, `decision`
180d, `gotcha`/`fact` 90d, `other`/default 60d) instead of a flat 30d; and `mem
stats` no longer double-applies decay. This makes `mem`, `mem2 gc`, and `mem2 tier`
all agree with the Python semantics.

Cross-cutting fix applied to notify + alert: id-prefix resolution for
delete/enable/disable, so the truncated 8-char id shown by `list` is directly
usable (Python required copying the full id from `--json`). FK-enforced cascade
on `alert delete` (Go enforces FKs; Python's sqlite3 defaults them off).

## Track B — runtime ownership (the genuinely new build)

| Package | State |
|---|---|
| `internal/llm` | **interface + 3 providers done.** Provider-neutral `Provider`/`Event`/`Request`; self-registering `EchoProvider` (offline) + **real `AnthropicProvider` and `OpenAIProvider`** (raw net/http SSE streaming, no SDK dep — keeps the binary lean). Both map the neutral Request onto their API shape (Anthropic hoists system + tool_result blocks; OpenAI keeps system + tool-role messages) and decode streamed text **and tool calls** (assembling streamed JSON args). SSE parsers + body builders are unit-tested offline against canned streams; `Stream` refuses without an API key so **no network call is ever made in tests** (protects the no-model-calls constraint). Selected via `tag run --provider anthropic|openai|echo`. |
| `internal/agent` | **agent loop done.** `Loop.Run` drives a `Provider` through tool-calling turns (execute → feed results → repeat) with a tool `Registry`, usage accumulation, unknown-tool handling, and a step cap. Fully tested offline via scripted/echo providers (4 tests). Real provider adapters are the only thing between this and live runs. |
| `internal/mcp` | **client + server + subprocess done.** JSON-RPC 2.0 over stdio: client (`Initialize`/`ListTools`/`CallTool`), server (`Register`/`Serve`), and `NewProcessClient` which spawns an external MCP server as a child process and speaks to it over its stdio. `tag mcp-serve` exposes TAG tools; `tag mcp-connect <cmd…>` consumes a third-party server; `tool.RegisterMCP` bridges external tools into `agent.Registry` (`mcp__<server>__<tool>`). Interop tested offline (in-process pipes + a real subprocess round-trip against our own `mcp-serve`) + agent-loop-over-MCP. |
| `internal/tool` | **built-in tools done.** `bash` (timeout), `read_file`, `write_file`, `list_dir` — all confined to a tool root with a path-traversal guard. Plug into `agent.Registry`; tested end-to-end *through* the agent loop via a one-shot provider (5 tests). |
| `internal/server` | **HTTP `serve` + `devui` + `web` done.** Loopback dashboards + `/api/snapshot`, `/api/spans`, `/api/runs`, `/api/queue`, `/api/costs`, `/health` + SSE streams; no wildcard CORS. Pure `*store.DB` handlers, `httptest`-tested + smoke-tested live. `tag serve/devui/web`. |
| `internal/lsp` | **LSP server done.** JSON-RPC 2.0 with `Content-Length` header framing (correct split-read + back-to-back handling); initialize/initialized/shutdown/exit/textDocument-hover; `-32601` on unknown method. Wired as `tag lsp`; framing tested offline (9 tests) + smoke-tested live. |
| `internal/tui` | **Charm TUI done.** bubbletea + lipgloss dashboard over the same snapshot (runs/queue/journal), refresh/quit keys, live ticker. `Model.Update`/`View` are pure and unit-tested offline (3 tests); `Run()` needs a TTY. Wired as `tag tui`. |
| `internal/swarm` | placeholder — kanban/swarm board |
| `internal/obs` | placeholder — otel export |
| `internal/credentials` | placeholder — provider auth |
| `internal/queue` | placeholder — background worker (CLI CRUD lives in cli/) |

## Remaining

The feature surface is ported. What is *deliberately* left as offline-only:
- **Live-model execution paths** (swarm run, eval run/judge, split plan, issue-solve/
  swe-solve/review-pr, ci/loop against a real provider): the orchestration is built
  and runs against the `echo` provider; wiring `--provider anthropic|openai` makes them
  live with an API key. Per the standing constraint they are never exercised live in tests.
- **`desktop`** (native desktop shell) — an OS packaging concern, not a control-plane feature.
- A handful of thin split/`import-*` variants beyond the 9 importers ported.

**Constraint (still in force):** no live model calls in testing (never use the Anthropic
account); all runtime work is built against the interface + echo/mock, verified offline.

## Audit fixes (2026-07-03)

A 5-agent adversarial audit read each subsystem AND ran the binary in sandboxes.
It found ~30 real bugs the green unit suite masked. All CRITICAL/HIGH fixed + regression-tested:

**Runtime (llm/agent/mcp/tool/lsp)**
- CRITICAL: agent loop dropped the assistant's tool_use/tool_calls, so multi-step tool calling
  was rejected by real Anthropic/OpenAI. Added `Message.ToolCalls`; loop replays it; both
  body-builders emit tool_use / tool_calls blocks before tool results.
- Usage overwrote instead of accumulating (Anthropic sends prompt+completion in two events).
- Mid-stream provider `error` frames were swallowed → now surfaced as `EventError`.
- MCP server responded to notifications and dropped string-id requests (int-typed id) →
  client deadlock. Now raw-JSON id (echo verbatim, detect notifications); client has a 120s
  per-call timeout; subprocess round-trip still works.
- LSP unbounded `Content-Length` → panic/OOM. Now capped at 64 MiB.
- Tool sandbox symlink escape → `EvalSymlinks` guard; `read_file` short-read → bounded ReadAll.

**Security (marketplace/scanner/profile names)**
- SSRF: `marketplace.Fetch` followed redirects to loopback and didn't pin DNS. Added a
  socket-level `Dialer.Control` IP check (defeats redirect + rebinding), redirect re-validation,
  and reserved-range blocking.
- Path traversal via `--profile` (plugin, mcp-registry) and `--name` (marketplace) → arbitrary
  file write. Added a shared `validProfileName` guard.
- Secret scanner: added ~13 missing patterns (stripe/slack/jwt/google/twilio/…), a symlink-escape
  guard, and a slide-by-1 entropy window.

**Memory / observability fidelity**
- `FactAt` (point-in-time) now queries `memory_fact_history` and accepts date-only timestamps.
- `mem search` is AND (was OR) and bumps `access_count` (re-enables GC promotion); `mem add`
  validates `memory_type`; `mem stats` reports `avg_confidence_base`.
- `alert check` computes real eval/span/cache metrics from live tables (was hardcoded 0 → inverted
  alerting); `alert firings`/`check --json` emit full keys; `cache --since` rejects negatives;
  `annotate export` includes `label_schema`.

### Audit round 2 (deeper — foundational + CLI + incomplete-fix verification)

A second 3-agent wave audited the foundational layer, the remaining CLI groups, and
adversarially re-verified round-1 fixes. Confirmed round-1 fixes are complete; found + fixed:
- **HIGH: `persona` was a dead feature** — builtins were never seeded and there was no install,
  so `persona list`/`apply` always failed. Now seeds 5 builtins (INSERT OR IGNORE) on list/apply;
  `active_personas` got its `PRIMARY KEY(profile,persona_name)` + `created_at`; apply upserts.
- **HIGH: `budget set` accepted any `--period`** → now validated daily/weekly/monthly; `budget get
  --json` now includes `id`+`enabled`.
- **HIGH: `doctor --json` emitted empty objects** (unexported struct fields) → exported with tags.
- **MED: `dag save` skipped validation** → rejects empty name, empty/non-string task, and unknown
  dependency-alias keys (C032).
- **MED: `--config ~/x` and bare `~` TAG_HOME weren't tilde-expanded** → added `paths.Expand`.
- alert `cost_usd_per_run` excluded all-null traces (2× error); `template export` got the
  traversal guard; notify remove clears `notification_log` children first (FK-safe).

Result: **19 tested packages, 187 test funcs**, gofmt+vet clean, `go test ./...` green. `internal/
store`/`internal/paths` (previously untested) now have coverage. Confirmed solid by both waves:
SQLite single-writer discipline, atomic config writes, concurrency (20 parallel writers, no lost
updates), no schema drift, no SQL injection, loopback-only servers.
