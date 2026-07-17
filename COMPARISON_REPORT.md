# TAG: Python (`tag-agent==0.8.2`) vs Native Go — Benchmark & Behavioral Comparison

Date: 2026-07-07 · Host: macOS (darwin 25.0.0, Apple Silicon) · Go toolchain go1.26.4

- **Python impl**: published `tag-agent==0.8.2` (`tag 0.8.2`), thin CLI over a managed Hermes runtime.
- **Go impl**: `tag version 0.9.0-go`, single static binary built from `/Users/sanskar/dev/test/tag/tag-go`.

Each implementation ran in its own isolated `TAG_HOME` sandbox (`mktemp -d`), bootstrapped once, keys unset except for the live-model section. Timeouts via `perl -e 'alarm N; exec @ARGV'`.

Build command (Go):
```
cd /Users/sanskar/dev/test/tag/tag-go && CGO_ENABLED=0 go build -o /tmp/tag-bench/go-tag ./cmd/tag
```

---

## 1. Executive Summary

| Headline metric | Go | Python | Advantage |
|---|---|---|---|
| **Startup latency** (`--help`) median | **9.6 ms** | 112.8 ms | **Go ~11.8× faster** |
| Startup (`mem list`) median | **13.4 ms** | 138.8 ms | Go ~10.4× faster |
| Startup (`--version`) median | 27.7 ms | 131.5 ms | Go ~4.7× faster |
| **Throughput** 100 sequential `mem add` | **2.08 s** | 14.47 s | Go ~7.0× faster |
| Throughput 100 parallel (`xargs -P8`) | **0.37 s** | 3.30 s | Go ~8.9× faster |
| **Cold start** (`bootstrap` fresh home) | **60.6 ms** | 1820.3 ms | **Go ~30× faster** |
| **Install/binary footprint** | **18 MB** (1 binary) | 170 MB venv | **Go ~9.4× smaller** |
| **Max RSS** (`mem list`) | **21.7 MB** | 38.1 MB | Go ~1.8× leaner |
| Instructions retired (`mem list`) | **163 M** | 1.23 B | Go ~7.5× fewer |
| Install time | 9.4 s (clean build) / 0.63 s (incr.) | 21.6 s (pip, warm cache) | Go faster |
| Live single-shot model run | **native `run`** works | no clean path (managed runtime only) | **Go only** |

**Bottom line:** the Go port is dramatically faster to start (≈10× for typical commands, ≈30× cold), ≈7–9× higher throughput, ≈9× smaller to ship, and ≈2× leaner in RAM — and it exposes a self-contained `run` agent loop the Python edition simply doesn't have offline. The two are **strongly differentiated on performance and packaging**, and largely **behaviorally faithful** on the shared Track-A command surface, with a catalog of small but real output/JSON/exit-code divergences (Section 3) plus a few genuine bugs (Section 6).

---

## 2. Metrics Tables (per dimension)

All timings from wall-clock of the child process (Python `time.perf_counter` around `subprocess.run`, env-isolated). 10 runs each for startup; median + min/max shown.

### 2.1 Startup latency (10 runs each)
| Scenario | Go median (min/max) ms | Py median (min/max) ms | Py/Go |
|---|---|---|---|
| `--version` | 27.7 (see note) | 131.5 | 4.7× |
| `--help` | 9.6 | 112.8 | 11.8× |
| `mem list` | 13.4 | 138.8 | 10.4× |

Note: Go `--version` (27.7 ms) is oddly slower than `--help`/`mem list` (~10–13 ms) — see Bug G7.

Commands:
```
env -i PATH=/usr/bin:/bin TAG_HOME=$H HOME=$H <exe> --help        # x10
env -i PATH=/usr/bin:/bin TAG_HOME=$H HOME=$H <exe> mem list      # x10
```

### 2.2 Throughput (fresh bootstrapped home per run)
| Workload | Go | Python | Failures |
|---|---|---|---|
| 100 sequential `mem add` | 2.08 s | 14.47 s | 0 / 0 |
| 100 parallel `mem add` (`xargs -P8`) | 0.37 s | 3.30 s | 0 / 0 |
| Rows persisted after parallel | 100 / 100 | 100 / 100 | no data loss |

Both survive 8-way concurrent SQLite writes with **zero lost rows** and zero non-zero exit codes. (The `mem list` default page size is 20 for both, which initially looked like loss — confirmed 100 via `mem stats` / `mem list --limit 200`.)

### 2.3 Cold vs Warm
| Phase | Go | Python |
|---|---|---|
| `bootstrap` on fresh `TAG_HOME` | 60.6 ms | 1820.3 ms |
| First `mem add` (cold DB init) | 49.6 ms | 146.2 ms |
| Second `mem add` (warm) | 35.1 ms | 135.6 ms |

Python's 1.8 s bootstrap reflects interpreter start + heavy import graph + profile rendering; Go's is a static-binary + file writes.

### 2.4 Binary / install footprint
| Item | Go | Python |
|---|---|---|
| Shippable artifact | 18 MB single binary (`go-tag`, 18,750,306 B) | 170 MB venv |
| App package (`tag/`) | (in binary) | 55 MB |
| Notable deps | none (static) | PIL 14 MB, openai 13 MB, pip 12 MB, cryptography 12 MB, pygments 9 MB |
| Install time | 9.4 s clean build / 0.63 s incremental | 21.6 s `pip install` (warm cache) |

### 2.5 Memory / CPU (`/usr/bin/time -l`, `mem list`)
| Metric | Go | Python |
|---|---|---|
| max RSS | 21.7 MB | 38.1 MB |
| peak memory footprint | 11.4 MB | 26.2 MB |
| instructions retired | 163 M | 1.23 B |
| real time | 0.06 s | 0.17 s |

---

## 3. Behavioral Diff (identical inputs, both impls)

~20 shared command families exercised with semantically-equal inputs (raw transcript: `/tmp/tag-bench/results/behav_raw.txt`). Legend: ✅ equivalent, ⚠️ cosmetic diff, ❗ structural/contract diff.

| # | Command | Exit (Go/Py) | Divergence |
|---|---|---|---|
| 1 | `mem add` | 0/0 | ⚠️ ID format: Go `0eabfd7f-d4dd-4a` (truncated UUID, 16 chars w/ dashes) vs Py `ca68d13688884e56` (16 hex). |
| 2 | `mem add --json` | 0/0 | ⚠️ Go `{"id":...}`; Py `{"id":...,"profile":...}`. Also flag position: Go **global pre-command** `--json`, Py **trailing** `--json`. |
| 3 | `mem list` | 0/0 | ⚠️ Ordering: Go insertion order, Py by effective confidence desc. Empty msg: Go `No memories.` vs Py `No memories for profile 'orchestrator'.` |
| 4 | `mem list --json` | 0/0 | ❗ Schema differs: Go has `profile`,`source`, RFC3339 `Z` timestamps, `confidence≈0.99999994`; Py has `confidence_base`, microsecond+offset timestamps, **no** `profile`. |
| 5 | `mem search <hit>` | 0/0 | ✅ same shape (per-field diffs as #4). |
| 6 | `mem search <miss>` | 0/0 | ⚠️ Quote style: Go `"zzznomatch"` vs Py `'zzznomatch'`. |
| 7 | `mem stats` | 0/0 | ❗ Go always prints JSON (`{"fact":{...}}`) even without `--json`; Py prints human table without, and a **different** JSON (`{"profile","total","by_type"}`) with `--json`. |
| 8 | `budget set` | 0/0 | ⚠️ Py thousands separator `1,000`; Go `1000`. |
| 9 | `budget get` | 0/0 | ❗ Go 1-line; Py multi-line labeled block. |
| 10 | `budget get --json` | 0/0 | ⚠️ Go compact, sorted keys, `id` 11 chars; Py pretty, insertion order, `id` 12 chars. |
| 11 | `budget get` (missing) | 0/0 | ✅ identical `No budget set for profile 'nosuch'.` |
| 12 | `prompt save`/`list` | 0/0 | ✅ identical output. |
| 13 | `prompt diff` | 0/0 | ❗ Go emits full-context `---/+++/-/+ ` block, **no `@@` hunk header**; Py emits standard unified diff with `@@ -1 +1 @@`. |
| 14 | `alert create` (bad metric) | 1/1 | ⚠️ Go `error: unknown metric: "cpu"`; Py `error: Unknown metric: 'cpu'`. Also **arg model differs**: Go positional `<name> <metric> <cond> <thr>`, Py flags `--metric/--condition/--threshold NAME`. |
| 15 | `alert check` (empty) | 0/0 | ✅ `No alerts firing`. |
| 16 | `alert check --json` (empty) | 0/0 | ❗ **Go `null`, Py `[]`** — array-consumer trap (Bug G1). |
| 17 | `cron add` | 0/0 | ⚠️ quote style only. |
| 18 | `cron list` | 0/0 | ❗ Go compact 1-liner; Py bordered table w/ header row. |
| 19 | `cron next` | 0 / **2** | ❗ **Go has `cron next`; Python does not** (invalid choice, exit 2). |
| 20 | `queue add` | 0/0 | ❗ **Go only enqueues (status stays `queued`, no worker); Python spawns a worker pid and executes (status `running`)** (Bug G6). |
| 21 | `queue list --json` | 0/0 | ❗ Go 4 fields; Py full 15-field record (`created_at`,`pid`,`deps_json`,…). |
| 22 | `security scan` | 1/1 | ⚠️ Same finding; Go appends `error: 1 findings` trailing line, Py does not. Both exit 1. |
| 23 | `security scan --json` | 1/1 | ✅ same array schema (`file/line_no/pattern/entropy`); Go adds trailing `error:` line. |
| 24 | `graph build`/`show` | 0/0 | ✅ identical human output. |
| 25 | `graph show --json` (empty) | 0/0 | ❗ **Go `{"counts":{…},"entities":null}` vs Py `{"entities":[],"relations":[]}`** — different shape + `null` vs `[]` (Bug G2). |
| 26 | `persona list` | 0/0 | ⚠️ column layout differs. |
| 27 | `persona list --json` | 0/0 | ❗ Go minimal (`name/description/source`); Py richer (`id/inject/tags/…`). |
| 28 | `route <type>` | 1/1 | ⚠️ Go positional `route coding`; Py flag `route --task-type coding`. Both reject unknown type (msg differs by prefix/quote). Go `route --json` on error prints **plain text, not JSON**. |
| 29 | `notify add`/`list` | 0/0 | ✅ equivalent (id-length diff only). |
| 30 | `template export <profile>` | 1 / **2** | ❗ Both fail with a positional profile: Go `unknown command "coder"` exit 1; Py `unrecognized arguments: coder` exit 2. (Export expects a different invocation.) |
| 31 | `annotate stats` | 0/0 | ⚠️ JSON key ordering differs; Go `add` subcommand exists, Py has none. |
| 32 | `annotate stats --json` | 0 / **2** | ❗ **Py rejects `--json` here** (`unrecognized arguments`); Go accepts. |
| 33 | `mem2 gc --dry-run` | 0/0 | ✅ byte-identical message. |
| 34 | `mem2 tier` | 0/0 | ⚠️ Go row prefix `[1.000]` (confidence); Py `[1ebf5f7d]` (id). |
| 35 | `doctor --json` | 0/0 | ❗ Go small `{checks[],ok}`; Py large managed-runtime report (`managed_root`, `prerequisites`, per-profile checks, upstream version). Reflects architecture, not a bug. |
| 36 | `costs` | 0/0 | ❗ Go 1-line aggregate; Py per-run table. |
| 37 | `assignments` | 0/0 | ⚠️ Go alphabetical incl. `coder`; Py config order. |
| 38 | unknown command | 1 / **2** | ❗ Go/cobra exit **1**; Py/argparse exit **2** (general pattern for usage errors). |

**Exit-code convention** is a systematic divergence: Python (argparse) returns **2** on any usage/parse error; Go (cobra) returns **1**. Worth normalizing if scripts branch on exit codes.

---

## 4. Live-Model Results

Keys loaded only for this section (`. /Users/sanskar/dev/test/tag/.env`); **6 provider calls total** (4 Go successes + 2 Python `submit` attempts).

### Go — native `run` (self-contained agent loop)
Setup: `set-model coder openai/gpt-4o-mini`, `set-model researcher anthropic/claude-haiku-4-5-20251001`.
Prompt: `"reply with exactly: PONG"`.

| Provider (model) | Call | Latency | prompt_tok | completion_tok | Output |
|---|---|---|---|---|---|
| openai / gpt-4o-mini | 1 | 2143 ms | 13 | 2 | `PONG` |
| openai / gpt-4o-mini | 2 | 1240 ms | 13 | 2 | `PONG` |
| anthropic / claude-haiku-4-5-20251001 | 1 | 967 ms | 14 | 6 | `PONG` |
| anthropic / claude-haiku-4-5-20251001 | 2 | 784 ms | 14 | 6 | `PONG` |

Go emits a clean JSON envelope: `{final_text, provider, run_id, steps, stopped, usage:{prompt_tokens,completion_tokens}}`. Offline `--provider echo` works too (usage = word count).

Command:
```
env -i ... OPENAI_API_KEY=$OPENAI_API_KEY go-tag run "reply with exactly: PONG" \
  --provider openai --profile coder --json
```

### Python — no equivalent single-shot path
Python has **no `run` command**. Its execution entrypoint is `submit --task-type … --prompt …`, which dispatches through the **managed Hermes runtime + Kanban routing**. A live attempt (with `OPENAI_API_KEY` set) resolved a master/worker/verifier route and dispatched to the **profile-configured OpenRouter models** (`deepseek/deepseek-v4-flash`, `qwen/qwen3-coder`), which **401'd (`Missing Authentication header`)** because no `OPENROUTER_API_KEY` was present — the provided OpenAI key was never used for a direct single-shot.

**Conclusion:** the Go binary can do a provider-honest, offline-verifiable single-shot completion; the Python edition cannot without the full managed runtime and correctly-provisioned profile providers. This is the clearest functional differentiator.

---

## 5. Stress / Robustness

| Case | Go | Python |
|---|---|---|
| 500 KB arg to `mem add` | ✅ exit 0, stored | ✅ exit 0, stored |
| Unicode/emoji (`héllo 世界 🚀 café ☃`) | ✅ stored + searchable (`mem search 世界` hits) | ✅ stored + searchable |
| Invalid JSON to `--config-json` | exit 1, `error: invalid config JSON: invalid character 'n'…` | exit 1, `error: Invalid config JSON: Expecting property name…` |
| Missing `--config` file | exit 1, `error: config file not found: …` | exit 1, `Config file not found: …` |
| Corrupt YAML config | exit 1, `…is not valid YAML: yaml: unmarshal errors…` | exit 1, `Config at … must be a YAML object.` |
| Fresh `TAG_HOME`, no bootstrap | exit 0, auto-inits, `No memories.` | exit 0, `No memories for profile 'orchestrator'.` |
| Unknown command | exit **1**, `error: unknown command "florb"` | exit **2**, argparse usage |

Both are robust: no crashes, no stack traces, graceful messages on every malformed input. Divergences are message wording + the exit-1/2 convention.

---

## 6. Bugs / Issues Found (candidates to file)

**Go:**
- **G1** `alert check --json` returns `null` for the empty case instead of `[]` (Python returns `[]`). Breaks `jq '.[]'`/array consumers.
- **G2** `graph show --json` returns `{"counts":{…},"entities":null}` — `entities` is `null` not `[]`, and the whole shape differs from Python's `{"entities":[],"relations":[]}` (no `relations` key at all in Go).
- **G3** `mem stats` prints JSON **unconditionally** (even without `--json`); there is no human-readable variant, inconsistent with every other command that has a text default.
- **G6** `queue add` does **not execute** the job — status remains `queued` with no worker; Python spawns and runs it. The Go background queue worker appears to be a non-executing stub.
- **G7** `--version` startup (27.7 ms) is ~2–3× slower than `--help`/`mem list` (~10–13 ms) — version path likely does avoidable work.
- **G8** Truncated-UUID IDs (e.g. `0eabfd7f-d4dd-4a`, 16 chars including dashes ⇒ ~52 bits) carry less entropy than a full UUID; minor collision-risk regression vs a clean 16-hex (64-bit) or full UUID.
- **G-min** `route --json` prints a plain-text error (not JSON) on unknown task type — inconsistent with `--json` contract.

**Python:**
- **P1** `annotate stats --json` → `unrecognized arguments: --json` (exit 2): `--json` unsupported on this subcommand though it is elsewhere.
- **P2** No `cron next` subcommand (Go has it) — either a feature gap or intentional; flag for parity.
- **P3** `doctor` reports managed runtime `patch_status: "diverged"` and `Update available: 3794 commits behind` — environment/runtime drift in the published wrapper's bundled Hermes (informational).

**Both (parity, not strictly bugs):** exit-code convention (Go 1 vs Py 2 on usage errors); several `--json` schemas differ in field names/shape (mem, budget, queue, persona, graph, doctor) — would break any tool consuming both interchangeably.

---

## 7. Verdict

The two implementations are **highly differentiated on the axes that matter for a CLI**:

- **Where Go wins decisively:** startup latency (~10× typical, ~30× cold), throughput (~7–9×), memory (~1.8× RSS, ~7.5× fewer instructions), packaging (single 18 MB static binary vs 170 MB venv, no interpreter/deps), and it uniquely offers an **offline, provider-honest `run` agent loop** with clean JSON output and live OpenAI/Anthropic parity.
- **Where they're at parity:** the shared Track-A command *behavior* is largely faithful — same commands, same core semantics, same robustness under 500 KB/unicode/invalid-config stress, no data loss under concurrency.
- **Where Go loses / differs:** Python's `queue` actually executes jobs (Go's is inert), Python's `doctor`/`costs`/`cron list`/`budget get` give richer human output, and Python's managed runtime is a full multi-agent orchestration layer (Kanban routing, workers/verifier) that the Go binary reimplements only partially. Several Go `--json` outputs use `null` where Python uses `[]`, and JSON schemas are not drop-in compatible between the two.

**Net:** Go is the right choice for speed, footprint, scripting ergonomics, and self-contained model runs. Python remains ahead on breadth of the managed orchestration runtime and a handful of richer/executing subsystems (notably the background queue). Migrating consumers must account for the JSON-shape and exit-code divergences catalogued in Section 3.

---

## Appendix: Reproducibility

Artifacts in `/tmp/tag-bench/`: `go-tag` (binary), `timing.py` (startup), `throughput.sh`, `stress.sh`, `behav.sh` (+ `results/behav_raw.txt`, `results/startup.json`), `go_time.txt`/`py_time.txt` (RSS).

```
# Build Go
cd /Users/sanskar/dev/test/tag/tag-go && CGO_ENABLED=0 go build -o /tmp/tag-bench/go-tag ./cmd/tag
# Python: python3.12 -m venv V && V/bin/pip install tag-agent==0.8.2
# Isolate: env -i PATH=/usr/bin:/bin TAG_HOME=$H HOME=$H <exe> <args>
# Bootstrap once: <exe> bootstrap
# Timeout wrapper: perl -e 'alarm N; exec @ARGV' <exe> <args>
# Startup:    env -i PATH=/usr/bin:/bin python3 /tmp/tag-bench/timing.py
# Throughput: bash /tmp/tag-bench/throughput.sh
# Behavioral: bash /tmp/tag-bench/behav.sh
# Stress:     bash /tmp/tag-bench/stress.sh
# RSS:        /usr/bin/time -l <exe> mem list
```

---

## Parity Update — 2026-07-08

Since the original benchmark, a swarm pass closed the Go↔Python parity gap and hardened correctness:

- **Command surface:** Go grew from **65 → 87 top-level commands**. New: `runs`, `logs`, `prompt-size`, `benchmark`, `sandbox`, `context`, `split`, `eval-judge`, `swe-solve`, `issue-solve`, `agentic-ci`, `review-pr`, 9 credential importers, and command aliases (`memory`/`plugins`/`model`).
- **Execution runtime (#532):** the queue is no longer inert — a new `internal/worker` executes queued jobs and full DAG dependency chains through the native agent loop (`queue worker`, `dag/cron run --execute`). Verified **live** against OpenAI (job add → worker → `queue result` shows model output). This closes the single biggest behavioral gap the benchmark identified.
- **Contract parity:** the `--json` contract was audited across all commands (empty→`[]`, error paths emit `{"error":...}`, Python field names for `cache stats`/`mem stats`); usage errors now exit **2** like Python argparse; unknown subcommands error instead of silently exiting 0.
- **26 bugs** found by two audit passes (code review + fresh-install QA) were fixed, verified on the binary, and closed (issues #520–#546). No critical issues, no data races (`go test -race ./...` green), no injection/SSRF/sandbox escapes, no leaks.
- **LLM-as-judge, live:** `eval-judge` scored a real OpenAI judgment 1.00/PASS with genuine reasoning.

**Remaining Python-only commands** are the intentional managed-Hermes runtime-passthrough layer (`chat`, `kanban`, `runtime`, `sessions`, `status`, `dashboard`, `config`, `profile`, `submit`, `update`, `skills`, `tools`) plus `desktop` (OS packaging) — deliberate non-ports in the "Go owns its own runtime" design, where `serve`/`devui`/`web` replace the dashboard and the Go binary doesn't ship the Python Hermes checkout. (The Go binary does ship a native `gateway`, but it is a from-scratch OpenAI-compatible server over the agent loop, not the Hermes-passthrough `gateway`.)

**Gates at time of writing:** `gofmt`/`go vet` clean, `go test ./... -race` green (28 packages, 265 test funcs), 364-invocation recursive `--help` sweep with 0 failures.
