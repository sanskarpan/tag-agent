# PRD-089: Real-Time Streaming stdout/stderr from Sandbox (`tag sandbox run --stream`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (3-5 days)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py`
**Depends on:** PRD-028 (sandbox code execution — base sandbox schema and `sandbox_runs` table), PRD-013 (agent tracing/observability — span model and `open_db()` pattern), PRD-034 (secret scanning — blocked path patterns referenced in security checks), PRD-003 (rich streaming TUI — `tui_output.py` Live/Panel patterns used for inline streaming display)
**Inspired by:** E2B streaming, Docker attach, Modal streaming output
**GitHub Issue:** #348

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) currently executes code via `exec.Command(...).CombinedOutput()`, which buffers all stdout and stderr internally and delivers the complete output blob only after the process exits. For short scripts this is invisible, but for any workload that runs longer than a few seconds — ML training loops, data-pipeline scripts, generative programs that print progress, or build tools — this buffering creates an opaque, silent black box. The user has no feedback while the process is running, cannot distinguish a hung process from a slow one, and receives a potentially enormous output dump on completion that is difficult to navigate.

This PRD specifies the streaming execution path for `tag sandbox run`. The core change is replacing the blocking `CombinedOutput()` call with `exec.CommandContext` wired to `cmd.StdoutPipe()` / `cmd.StderrPipe()`, each drained by a dedicated goroutine that scans lines with `bufio.Scanner` and emits them over a bounded channel as they arrive from the child process. When `--stream` is passed (or when streaming is enabled globally via config), TAG prints each line to the terminal the instant the child process flushes it, prefixed with a stream label and a relative timestamp. Every chunk is also written to a new `sandbox_run_chunks` table so that `tag sandbox logs <run-id>` can replay the full timestamped output even after the run completes, and `tag sandbox logs <run-id> --follow` can tail a currently-running sandbox in a second terminal.

The design draws directly from how serious sandbox providers handle this problem in production. E2B's SDK exposes `on_stdout` / `on_stderr` callbacks on process start. Docker uses `docker attach` and the multiplexed stream protocol (a one-byte stream identifier — 0x01 for stdout, 0x02 for stderr — prepended to each 8-byte frame header); the Go `docker/docker` moby client demultiplexes this via `stdcopy.StdCopy`. Modal's `Sandbox.stdout` / `Sandbox.stderr` are streaming readers. TAG's implementation adapts these patterns to Go's `os/exec` pipes + goroutine/channel fan-in for the local backends, and wraps provider clients appropriately for E2B and Modal. The architecture is backend-agnostic: the streaming interface is expressed as a Go channel of `OutputChunk` structs (`<-chan OutputChunk`) regardless of which backend produces them. The same channel is the natural seam for a future SSE bridge (`tmaxmax/go-sse` over `go-chi/chi`, spec'd with `huma` v2) should remote streaming land (deferred, see NG1 / Open Questions).

Timeout enforcement is two-layered in the streaming path. A per-chunk timeout (default: 30 s) fires if no output is received from either stream for that duration — indicating the process is alive but silent, which could be a deadlock or an infinite wait on input. A total-run timeout (passed as `--timeout`, default: 300 s) is bound to the `context.Context` deadline that backs `exec.CommandContext` and fires if the process has not exited by the wall-clock deadline. Both timeouts kill the process group, update the run record with `status='timeout'` and the correct `timed_out_reason`, and flush any remaining buffered chunks. This two-layer design avoids the single-timeout ambiguity present in the current blocking path where a 60-second timeout does not distinguish between "ran for 60 s then exceeded budget" and "hung on first read for 60 s".

The PRD also specifies schema additions to `sandbox_runs` (new columns for streaming metadata) and the new `sandbox_run_chunks` table for chunk persistence, backed by the pure-Go `modernc.org/sqlite` driver. It integrates with the `internal/tui` streaming printer for coloured inline streaming and with `go.opentelemetry.io/otel` for span emission on stream open/close events.

The sandboxed process itself is launched through TAG's isolation ladder: the in-process `restricted` tier composes `landlock-lsm/go-landlock` (filesystem), `elastic/go-seccomp-bpf` (CGO-free syscall filter), and `google/nftables` (egress) around the `os/exec` invocation; heavier tiers escalate to the `docker/docker` moby client, gVisor (`runsc`) as a Docker runtime, and `firecracker-microvm/firecracker-go-sdk` for the strongest/GPU isolation. These primitives are Linux-only; off-Linux TAG feature-detects and degrades to a plain subprocess or Docker Desktop. Streaming is orthogonal to the isolation tier — it reads whatever stdout/stderr the launched process exposes.

---

## 2. Problem Statement

### 2.1 Blocking Execution Creates a Silent Black Box for Long-Running Jobs

The current `runRestricted` and `runDocker` functions both call `exec.Command(...).CombinedOutput()`. From the user's perspective, after typing `tag sandbox run --code "..."` the terminal freezes until the process exits. There is no spinner, no partial output, no progress indication. A 90-second data-processing job looks identical to a hung process. Users routinely kill TAG with `Ctrl+C` assuming a hang when the process was actually progressing normally. This results in lost work and erodes trust in the sandbox feature.

### 2.2 Post-Run Output Blobs Are Unusable for Iterative Workflows

When a long-running sandbox job does complete, `output` is returned as a single TEXT blob concatenating stdout and stderr with a `\n---stderr---\n` delimiter. For jobs that print thousands of lines (e.g., `for i in range(10000): print(i)` or a training loop emitting loss values), this blob is difficult to read in the terminal, impossible to search without scrolling, and discards all timing information. There is no way to know whether line 5000 appeared at second 2 or second 88 of the run.

### 2.3 No Mechanism to Observe a Running Sandbox from a Second Terminal

Once `tag sandbox run` is invoked, the run ID is not visible until the process exits. Even if the user captures the run ID (e.g., from a programmatic API call), there is no `tag sandbox logs <run-id> --follow` command to attach to the live output stream. This blocks multi-terminal workflows where an operator launches a long job in one window and monitors its output in another, a workflow that is standard in Docker (`docker logs -f`) and all major CI systems.

---

## 3. Goals and Non-Goals

### Goals

| # | Goal |
|---|------|
| G1 | Replace blocking `CombinedOutput()` with `os/exec` pipes (`StdoutPipe`/`StderrPipe`) + per-stream reader goroutines feeding a channel, on all local backends when `--stream` is passed or streaming is globally enabled. |
| G2 | Emit stdout and stderr lines to the terminal as they arrive, with stream label (`OUT` / `ERR`) and relative timestamp, formatted via the `internal/tui` streaming printer. |
| G3 | Persist every output chunk (text, stream, relative offset ms, sequence number) to a new `sandbox_run_chunks` table so the full ordered output is queryable after the run completes. |
| G4 | Implement `tag sandbox logs <run-id>` for post-run replay and `tag sandbox logs <run-id> --follow` for live tail of an in-progress run. |
| G5 | Enforce a per-chunk silence timeout (default: 30 s) and a total-run timeout (default: 300 s, overridable via `--timeout`), each killing the process and recording `status='timeout'` with the specific reason. |
| G6 | Support `--stream` flag on `tag sandbox run` and a `sandbox.stream_by_default = true` config key. |
| G7 | Support code-string input (`--code`) and file input (`--file`) with automatic language detection from file extension. |
| G8 | Expose the run ID immediately at stream start so the user can reference it in a second terminal before the run completes. |
| G9 | Extend `sandbox_runs` schema with streaming-specific columns (`streamed`, `chunk_count`, `last_chunk_at`, `timed_out_reason`) without breaking existing rows. |
| G10 | Keep the non-streaming path (`CombinedOutput()`) fully intact as the default when `--stream` is not passed, ensuring zero regression for existing users. |

### Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | WebSocket-based streaming to a browser or remote client. All streaming in this PRD is terminal-local. Remote streaming is deferred (see Open Questions). |
| NG2 | PTY / interactive terminal allocation (`github.com/creack/pty`). `--stream` targets non-interactive scripts that write to stdout/stderr. Interactive shell sessions are a separate feature. |
| NG3 | Structured JSON line output parsing or schema validation of streamed content. TAG streams raw text lines; parsing is left to the caller. |
| NG4 | Chunk compression or binary chunk support. All chunks are UTF-8 text. |
| NG5 | Modifying the E2B or Modal backend streaming integration in this PRD. Those backends use provider SDK streaming APIs and are noted as integration points but not fully implemented here. |
| NG6 | Changing how `internal/queue` dispatches sandbox jobs. Streaming output in queue context is a follow-on PRD. |
| NG7 | Infinite-retention chunk storage. Chunks older than a configurable TTL (default: 7 days) are swept by the existing TTL sweeper in `internal/cron`. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Time-to-first-chunk (TTFC) | < 200 ms from process spawn to first terminal line for a `print("hello")` script | Measured in Go test: `time.Since(start)` diff between `cmd.Start()` and first `OutputChunk` received on the channel |
| Throughput | ≥ 10,000 lines/s sustained for a tight `for i in range(100000): print(i)` loop without terminal lock-up | Benchmark (`testing.B`): count chunks delivered, assert total time < 10 s |
| Per-chunk silence timeout accuracy | Process killed within ±500 ms of the 30-second per-chunk deadline | Go test with a `sleep 35` script; assert killedAt − lastChunkAt < 30.5 s |
| Total timeout accuracy | Process killed within ±1 s of `--timeout` value | Go test with `while true; do sleep 1; echo x; done` bound to a `context.WithTimeout` |
| Chunk persistence completeness | 100% of lines emitted by child process appear in `sandbox_run_chunks` in correct sequence | Compare streamed set vs. source set in Go integration test |
| `--follow` attach latency | `tag sandbox logs <id> --follow` begins printing within 500 ms of a new chunk appearing in the table | Go test: two goroutines — one writing chunks, one polling via `--follow` |
| Non-streaming regression | `tag sandbox run` (no `--stream`) wall time unchanged vs. pre-PRD baseline within 5% | Benchmark (`testing.B`): 20 runs of 1-second script, compare means |
| Backward compatibility | All existing `sandbox_runs` rows queryable without migration error after schema addition | Schema migration test using a pre-seeded `modernc.org/sqlite` fixture |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag sandbox run --code "for i in range(100): print(i)" --language python --stream` | I see each number printed as it is produced, not all 100 lines at once after the loop finishes |
| U2 | Data engineer | run `tag sandbox run --file pipeline.py --stream --timeout 300` | I can watch a 5-minute ETL script's progress in real time and kill it early if I see an error on line 3 |
| U3 | DevOps engineer | run `tag sandbox run --file build.sh --stream 2>&1` | I see interleaved stdout and stderr with stream labels so I can tell which errors come from which tool |
| U4 | Platform operator | run `tag sandbox logs abc123def456 --follow` in a monitoring terminal | I can watch a long-running job without blocking the terminal where I launched it |
| U5 | Developer | run `tag sandbox logs abc123def456` after a completed run | I see the full timestamped output replay with relative timing information preserved from when the run executed |
| U6 | CI engineer | run `tag sandbox run --file test_suite.py --stream --timeout 120 --json` | I get machine-readable JSON lines on stdout for each chunk so my CI log aggregator can ingest structured output |
| U7 | Security-conscious user | observe that `tag sandbox run --stream` still respects all `--backend restricted` blocked-path rules | Streaming does not bypass the existing security allowlist/blocklist |
| U8 | Developer | see the run ID printed immediately at stream start before any output lines | I can capture the run ID for `tag sandbox logs --follow` in a second terminal before the script finishes |
| U9 | Developer | run `tag sandbox run --code "import time; time.sleep(60)" --stream --chunk-timeout 10` | The process is killed after 10 seconds of silence and I receive a clear `[TIMEOUT: no output for 10s]` message |
| U10 | Developer | have streaming enabled by default via `tag config set sandbox.stream_by_default true` | I never have to remember to pass `--stream` for interactive sandbox sessions |

---

## 6. Proposed CLI Surface

### 6.1 `tag sandbox run` (extended)

```
tag sandbox run \
  --code "for i in range(100): print(i)" \
  --language python \
  --stream \
  [--timeout 300] \
  [--chunk-timeout 30] \
  [--backend restricted|docker|e2b|modal] \
  [--image python:3.12-slim] \
  [--workdir /tmp/sandbox] \
  [--json] \
  [--no-color]
```

```
tag sandbox run \
  --file script.py \
  --stream \
  --timeout 60 \
  [--env KEY=VALUE ...] \
  [--backend docker] \
  [--image python:3.12-slim]
```

**New flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--stream` | bool | `false` (or `sandbox.stream_by_default` config) | Enable real-time streaming of stdout/stderr |
| `--code TEXT` | str | — | Inline code string to execute |
| `--language TEXT` | str | `python` | Language for `--code` input; determines interpreter. Valid: `python`, `bash`, `node`, `ruby` |
| `--file PATH` | path | — | Script file to execute; language inferred from extension |
| `--timeout INT` | int | 300 | Total run timeout in seconds |
| `--chunk-timeout INT` | int | 30 | Per-chunk silence timeout in seconds (kills process if no output for this duration) |
| `--env KEY=VALUE` | list | — | Environment variable overrides (repeatable) |
| `--json` | bool | false | Emit each chunk as a JSON line to stdout instead of human-readable format |
| `--no-color` | bool | false | Disable Rich colour formatting |

**Example terminal output (human mode):**

```
$ tag sandbox run --code "import time
for i in range(5):
    print(f'step {i}')
    time.sleep(0.5)" --language python --stream

Run ID: 4a7f2c91e830
Backend: restricted | Language: python | Timeout: 300s | Chunk-timeout: 30s
─────────────────────────────────────────────────────────────────────────────
  0.012s  OUT  step 0
  0.513s  OUT  step 1
  1.014s  OUT  step 2
  1.516s  OUT  step 3
  2.017s  OUT  step 4
─────────────────────────────────────────────────────────────────────────────
Completed in 2.019s | exit 0 | 5 lines (5 OUT, 0 ERR) | run 4a7f2c91e830
```

**Example terminal output (JSON mode, `--json`):**

```json
{"event":"run_start","run_id":"4a7f2c91e830","backend":"restricted","ts_iso":"2026-06-17T10:00:00.000Z"}
{"event":"chunk","run_id":"4a7f2c91e830","seq":1,"stream":"stdout","text":"step 0\n","offset_ms":12}
{"event":"chunk","run_id":"4a7f2c91e830","seq":2,"stream":"stdout","text":"step 1\n","offset_ms":513}
{"event":"run_end","run_id":"4a7f2c91e830","exit_code":0,"duration_ms":2019,"chunk_count":5}
```

**Silence timeout output:**

```
  0.012s  OUT  step 0
 30.013s  ERR  [TIMEOUT] No output for 30s — killing process (chunk-timeout exceeded)
─────────────────────────────────────────────────────────────────────────────
Timed out (chunk-timeout) after 30.01s | exit -9 | 1 lines | run 4a7f2c91e830
```

### 6.2 `tag sandbox logs`

```
tag sandbox logs <run-id> [--follow] [--since N] [--stream stdout|stderr|both] [--json]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `<run-id>` | str | required | Run ID from `sandbox_runs.id` |
| `--follow` | bool | false | Tail live output; polls `sandbox_run_chunks` every 200 ms until `sandbox_runs.status != 'running'` |
| `--since INT` | int | 0 | Start replay from chunk sequence number N |
| `--stream TEXT` | enum | `both` | Filter to `stdout`, `stderr`, or `both` |
| `--json` | bool | false | Emit chunks as JSON lines |

**Example — replay completed run:**

```
$ tag sandbox logs 4a7f2c91e830

Run 4a7f2c91e830 | backend: restricted | exit 0 | completed 2026-06-17T10:00:02Z
─────────────────────────────────────────────────────────────────────────────
  0.012s  OUT  step 0
  0.513s  OUT  step 1
  1.014s  OUT  step 2
  1.516s  OUT  step 3
  2.017s  OUT  step 4
─────────────────────────────────────────────────────────────────────────────
5 chunks | 5 OUT, 0 ERR
```

**Example — follow live run:**

```
$ tag sandbox logs 4a7f2c91e830 --follow

[following run 4a7f2c91e830 — Ctrl+C to stop]
  0.012s  OUT  step 0
  0.513s  OUT  step 1
^C
[detached — run still in progress]
```

### 6.3 Config integration

```bash
# Enable streaming by default
tag config set sandbox.stream_by_default true

# Set default chunk timeout
tag config set sandbox.chunk_timeout_seconds 30

# Set default total timeout
tag config set sandbox.default_timeout_seconds 300
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | When `--stream` is passed (or `sandbox.stream_by_default` is true), `RunInSandbox()` MUST use `exec.CommandContext` with `StdoutPipe`/`StderrPipe` instead of `CombinedOutput()` and deliver `OutputChunk` values line-by-line over a channel as they arrive. | P0 |
| FR-02 | Each `OutputChunk` MUST carry: `RunID`, `Seq` (monotonic integer), `Stream` (`"stdout"` or `"stderr"`), `Text` (the raw line including trailing newline), `OffsetMs` (milliseconds since process spawn), `ReceivedAt` (RFC 3339 / ISO-8601 UTC). | P0 |
| FR-03 | Every `OutputChunk` MUST be persisted to `sandbox_run_chunks` within 500 ms of being received, even for runs that ultimately time out or are killed. | P0 |
| FR-04 | The `sandbox_runs` table MUST be updated with `streamed=1`, incrementing `chunk_count`, and updating `last_chunk_at` for every chunk received. These updates MUST be batched (every 10 chunks or every 500 ms) to avoid per-line SQLite writes at high throughput. | P1 |
| FR-05 | The streaming reader MUST drain stdout and stderr concurrently via two reader goroutines fanning into a single channel; it MUST NOT read from stdout only and block if the process is writing only to stderr. | P0 |
| FR-06 | The per-chunk silence timeout (`--chunk-timeout`, default 30 s) MUST kill the process group if no chunk is received on the fan-in channel within that duration (enforced by a `time.Timer`/`select` on the channel). Status MUST be set to `'timeout'` and `timed_out_reason` to `'chunk_silence'`. | P0 |
| FR-07 | The total-run timeout (`--timeout`, default 300 s) MUST be bound to the `context.WithTimeout` deadline backing `exec.CommandContext`; on expiry the process group MUST be killed. Status MUST be set to `'timeout'` and `timed_out_reason` to `'total_timeout'`. | P0 |
| FR-08 | After killing the process on either timeout, the reader goroutines MUST drain any remaining data buffered in both pipes before the channel is closed. | P1 |
| FR-09 | The run ID MUST be printed to stderr before the first chunk is printed to stdout, so the user can record it while the run is still in progress. | P1 |
| FR-10 | `tag sandbox logs <run-id>` MUST replay chunks from `sandbox_run_chunks` ordered by `seq ASC`, formatted identically to the streaming output during the live run. | P0 |
| FR-11 | `tag sandbox logs <run-id> --follow` MUST poll `sandbox_run_chunks` for new rows every 200 ms using `SELECT ... WHERE seq > ?` and terminate when `sandbox_runs.status` is no longer `'running'`. | P0 |
| FR-12 | `--code` input MUST be written to a temporary file via `os.CreateTemp` chmod'd to `0o600`, executed via the appropriate interpreter for the specified `--language`, and the temp file MUST be removed (`os.Remove` in a `defer`) after the process exits (or is killed). | P0 |
| FR-13 | `--file` input MUST have its language auto-detected from the file extension (`.py` → python, `.sh` → bash, `.js` → node, `.rb` → ruby) if `--language` is not specified; an unsupported extension MUST produce a clear error with a list of supported languages. | P1 |
| FR-14 | In `--json` mode, each event (run_start, chunk, run_end, timeout) MUST be emitted as a complete, valid JSON object on its own line (JSONL format) to stdout via `encoding/json`. | P1 |
| FR-15 | The existing non-streaming `RunInSandbox()` code path (no `--stream`) MUST remain unchanged in behavior; streaming MUST be opt-in with no performance regression on the default path. | P0 |
| FR-16 | Schema migrations (new columns on `sandbox_runs`, new `sandbox_run_chunks` table) MUST use `PRAGMA table_info`-guarded `ALTER TABLE ADD COLUMN` logic and MUST be idempotent (safe to run against a database that already has the columns). | P0 |
| FR-17 | All security checks from PRD-028 (blocked path patterns, command allowlist for `restricted` backend) plus the isolation-ladder setup (landlock/seccomp/nftables for `restricted`) MUST execute before `cmd.Start()` is called; streaming does not bypass pre-execution validation. | P0 |
| FR-18 | If the child process's output line exceeds 65,536 bytes without a newline, the reader MUST force a chunk boundary at that byte limit to avoid unbounded memory accumulation (`bufio.Scanner` buffer cap with a custom `SplitFunc`). | P1 |
| FR-19 | In human-readable mode, `OUT` lines MUST be printed in the terminal's default color and `ERR` lines MUST be printed in yellow/amber to visually distinguish stderr, using ANSI styling (`lipgloss`/`fatih/color`). | P2 |
| FR-20 | `tag sandbox logs` MUST support `--since <seq>` to start replay from a specific sequence number, enabling incremental fetches by external tooling. | P2 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Throughput:** The streaming reader must not be the bottleneck for processes that produce output at ≥ 10,000 lines/s. A buffered channel (bounded) feeds a batch-flush goroutine that handles SQLite writes asynchronously. | ≥ 10,000 lines/s |
| NFR-02 | **Memory:** Peak memory overhead of the streaming reader (chunk batch slice + channel buffer) must not exceed 32 MB for any single run, regardless of total output volume. Chunks are persisted and released from the in-process batch. | ≤ 32 MB |
| NFR-03 | **SQLite write latency:** Chunk batch writes must complete within 50 ms per batch on a standard NVMe drive. WAL mode (`PRAGMA journal_mode=WAL`, already enabled in TAG via `modernc.org/sqlite`) provides the necessary read/write concurrency. | ≤ 50 ms/batch |
| NFR-04 | **TTY compatibility:** The `--stream` output must not corrupt terminal state. ANSI styling is used only in interactive TTY contexts; when stdout is piped or redirected, plain text lines (no ANSI escape codes) are emitted. Detect via `golang.org/x/term.IsTerminal(int(os.Stdout.Fd()))`. | — |
| NFR-05 | **Signal handling:** SIGINT (Ctrl+C) during `--stream` must cancel the run `context`, kill the child process group, flush remaining chunks to SQLite, update status to `'interrupted'`, and exit with code 130. Wired via `signal.NotifyContext`. | — |
| NFR-06 | **Concurrent runs:** Multiple simultaneous `tag sandbox run --stream` invocations must not interfere with each other's chunk tables. Run IDs (UUID hex) provide full isolation. SQLite WAL mode allows concurrent readers/writers. | — |
| NFR-07 | **Portability:** Because Go's `os/exec` pipes are drained by goroutines rather than `select(2)` on raw fds, a single reader implementation works uniformly across Linux, macOS, and Windows — no platform-specific fallback is required. Process-group kill semantics differ per OS (see FR-06/FR-07) and are handled behind a small build-tagged helper. | Linux, macOS, Windows (single code path) |
| NFR-08 | **Chunk TTL:** `sandbox_run_chunks` rows older than `sandbox.chunk_retention_days` (default: 7) must be eligible for deletion by the TTL sweeper in `internal/cron`. The sweeper must not delete chunks for runs whose `status = 'running'`. | Default 7-day retention |
| NFR-09 | **Observability:** An OTel span `sandbox.stream_open` is emitted (`go.opentelemetry.io/otel`) when streaming starts and `sandbox.stream_close` when it ends, carrying `run_id`, `chunk_count`, `duration_ms`, `exit_code`, and `timed_out_reason` attributes. | — |
| NFR-10 | **Minimal new dependencies:** The streaming core uses only the Go stdlib (`os/exec`, `bufio`, `context`, `os/signal`, `os`, `syscall`) plus `golang.org/x/sync/errgroup` for goroutine lifecycle. Terminal styling reuses TAG's existing `lipgloss` dependency. No new heavyweight packages are introduced. | — |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change |
|------|--------|
| `internal/sandbox/stream.go` | Primary implementation: new streaming path in `RunInSandboxStreaming()`, `OutputChunk` fan-in, `_streamReaders` goroutines |
| `internal/sandbox/schema.go` | Schema additions (`sandbox_runs` columns, `sandbox_run_chunks` table) via `modernc.org/sqlite` |
| `internal/sandbox/logs.go` | `StreamSandboxLogs()` — replay/follow iterator over `sandbox_run_chunks` |
| `internal/sandbox/persist.go` | Batched chunk writer (`flushChunks`) |
| `cmd/tag/sandbox.go` (chi/cobra command layer) | Extend `sandbox run` to handle `--stream`, `--code`, `--file`, `--language`, `--chunk-timeout`, `--json` flags; add `sandbox logs` subcommand |
| `internal/tui/sandbox_stream.go` | New `SandboxStreamPrinter` writing coloured, TTY-aware line output (`lipgloss`) |

### 9.2 SQLite Schema

#### 9.2.1 `sandbox_runs` extensions (additive, backward-compatible)

```sql
-- Add columns to existing sandbox_runs table
-- Use separate ALTER TABLE statements for SQLite compatibility
-- (SQLite does not support multiple ADD COLUMN in one ALTER TABLE)

ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS streamed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS chunk_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS last_chunk_at TEXT;
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS timed_out_reason TEXT;
-- language used when --code or --file is provided
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS language TEXT;
-- original source: 'code_string' | 'file' | 'command'
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS source_type TEXT NOT NULL DEFAULT 'command';
```

Since SQLite does not support `IF NOT EXISTS` on `ALTER TABLE ADD COLUMN`, the actual migration logic wraps each statement in a `PRAGMA table_info` existence check:

```go
// internal/sandbox/schema.go
func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, defn string) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := map[string]struct{}{}
	for rows.Next() {
		var (
			cid                        int
			name, ctype                string
			notnull, pk                int
			dflt                       sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		existing[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, ok := existing[column]; ok {
		return nil
	}
	_, err = db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, defn))
	return err
}
```

#### 9.2.2 `sandbox_run_chunks` (new table)

```sql
CREATE TABLE IF NOT EXISTS sandbox_run_chunks (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,  -- rowid alias for O(1) appends
  run_id      TEXT NOT NULL,                       -- FK → sandbox_runs.id
  seq         INTEGER NOT NULL,                    -- monotonic per-run, starts at 1
  stream      TEXT NOT NULL CHECK(stream IN ('stdout','stderr')),
  text        TEXT NOT NULL,                       -- raw line text, UTF-8, may include trailing \n
  offset_ms   INTEGER NOT NULL,                    -- milliseconds since process spawn (monotonic clock)
  received_at TEXT NOT NULL,                       -- ISO-8601 UTC timestamp when chunk was received
  UNIQUE(run_id, seq)
);

-- Primary access pattern: replay ordered by seq for a given run
CREATE INDEX IF NOT EXISTS idx_src_run_seq
  ON sandbox_run_chunks(run_id, seq);

-- TTL sweep: find old completed runs
CREATE INDEX IF NOT EXISTS idx_src_received
  ON sandbox_run_chunks(received_at);
```

### 9.3 Core Types

Go structs replace the Python dataclasses. JSON tags drive `--json` (JSONL) output; `invopop/jsonschema` can derive the wire schema for the API surface if the SSE bridge (NG1, deferred) is built.

```go
// internal/sandbox/stream.go
package sandbox

import "time"

type StreamType string

const (
	StreamStdout StreamType = "stdout"
	StreamStderr StreamType = "stderr"
)

type TimeoutReason string

const (
	TimeoutChunkSilence TimeoutReason = "chunk_silence"
	TimeoutTotal        TimeoutReason = "total_timeout"
)

// OutputChunk is a single line of output from a streaming sandbox run.
type OutputChunk struct {
	RunID      string     `json:"run_id"`
	Seq        int64      `json:"seq"`
	Stream     StreamType `json:"stream"`
	Text       string     `json:"text"`       // raw line, usually includes trailing \n
	OffsetMs   int64      `json:"offset_ms"`  // ms since proc spawn (monotonic)
	ReceivedAt string     `json:"received_at"` // RFC 3339 UTC
}

// StreamRunResult is the final result returned once the chunk channel is closed.
type StreamRunResult struct {
	RunID          string        `json:"run_id"`
	ExitCode       int           `json:"exit_code"` // -1 if killed before exit
	Status         string        `json:"status"`    // done | failed | timeout | interrupted
	TimedOutReason TimeoutReason `json:"timed_out_reason,omitempty"`
	DurationMs     int64         `json:"duration_ms"`
	ChunkCount     int64         `json:"chunk_count"`
	StdoutChunks   int64         `json:"stdout_chunks"`
	StderrChunks   int64         `json:"stderr_chunks"`
}

// StreamConfig holds parameters for a streaming sandbox execution.
// Populated by the command layer; validated before launch.
type StreamConfig struct {
	Code         string            // inline code string
	File         string            // path to script file
	Language     string            // interpreter selection (default "python")
	Command      []string          // raw command (non-code path)
	Backend      string            // default "restricted"
	Image        string            // default "python:3.12-slim"
	Timeout      time.Duration     // total run timeout (default 300s)
	ChunkTimeout time.Duration     // per-chunk silence timeout (default 30s)
	Workdir      string
	EnvOverrides map[string]string
	JSONOutput   bool
	NoColor      bool
}
```

### 9.4 Core Streaming Algorithm

The streaming reader spawns one goroutine per pipe. Each goroutine scans lines with a `bufio.Scanner` (buffer capped at `MaxLineBytes` to force a chunk boundary on newline-less lines) and sends `OutputChunk` values on a shared bounded channel. An `errgroup` tracks the two readers; the channel is closed once both return. The consumer loop `select`s on the chunk channel against a per-chunk silence `time.Timer` (reset on every chunk) and the run `context` (which carries the total-run deadline). This replaces the Unix-only `select(2)` fd multiplexing with a portable goroutine fan-in.

```go
// internal/sandbox/stream.go

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sync/errgroup"
)

var languageInterpreters = map[string][]string{
	"python": {"python3", "-u"}, // -u: unbuffered
	"bash":   {"bash"},
	"node":   {"node"},
	"ruby":   {"ruby"},
}

var extToLanguage = map[string]string{
	".py": "python", ".sh": "bash", ".js": "node", ".rb": "ruby",
}

const MaxLineBytes = 64 * 1024 // force chunk boundary at 64 KiB for lines without newline

// resolveCommand builds the argv slice from StreamConfig code/file/command.
// If a temp file is created for --code, its path is returned for cleanup.
func resolveCommand(cfg StreamConfig) (argv []string, tmpPath string, err error) {
	if len(cfg.Command) > 0 {
		return cfg.Command, "", nil
	}
	lang := strings.ToLower(cfg.Language)
	interp, ok := languageInterpreters[lang]
	if !ok {
		return nil, "", fmt.Errorf("unsupported language %q; supported: %s",
			lang, strings.Join(sortedKeys(languageInterpreters), ", "))
	}
	switch {
	case cfg.Code != "":
		f, err := os.CreateTemp("", "tag_sandbox_*."+lang)
		if err != nil {
			return nil, "", err
		}
		tmpPath = f.Name()
		if _, err := f.WriteString(cfg.Code); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return nil, "", err
		}
		f.Close()
		if err := os.Chmod(tmpPath, 0o600); err != nil {
			os.Remove(tmpPath)
			return nil, "", err
		}
		return append(append([]string{}, interp...), tmpPath), tmpPath, nil
	case cfg.File != "":
		return append(append([]string{}, interp...), cfg.File), "", nil
	default:
		return nil, "", fmt.Errorf("one of --code, --file, or a command must be provided")
	}
}

// scanStream drains one pipe, emitting an OutputChunk per line onto out.
// seq is atomically incremented so stdout/stderr chunks share a monotonic order.
func scanStream(
	pipe io.Reader,
	stream StreamType,
	runID string,
	startMono time.Time,
	seq *int64,
	out chan<- OutputChunk,
) error {
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 4096), MaxLineBytes)
	sc.Split(scanLinesCapped) // ScanLines variant that yields at MaxLineBytes without a newline
	for sc.Scan() {
		n := atomic.AddInt64(seq, 1)
		out <- OutputChunk{
			RunID:      runID,
			Seq:        n,
			Stream:     stream,
			Text:       sc.Text() + "\n",
			OffsetMs:   time.Since(startMono).Milliseconds(),
			ReceivedAt: utcNow(),
		}
	}
	return sc.Err()
}

// streamProcess launches the reader goroutines and returns the fan-in channel.
// The channel is closed when both pipes are fully drained.
func streamProcess(
	ctx context.Context, // carries the total-run deadline
	cmd *exec.Cmd,
	runID string,
	startMono time.Time,
) (<-chan OutputChunk, func() error, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	out := make(chan OutputChunk, 256) // bounded: backpressure when the DB writer lags
	var seq int64

	g := new(errgroup.Group)
	g.Go(func() error { return scanStream(stdout, StreamStdout, runID, startMono, &seq, out) })
	g.Go(func() error { return scanStream(stderr, StreamStderr, runID, startMono, &seq, out) })

	go func() {
		_ = g.Wait() // scanner errors surface via cmd.Wait / the result
		close(out)
	}()

	// wait returns the process exit after both readers finish.
	wait := func() error { return cmd.Wait() }
	return out, wait, nil
}
```

The consumer that enforces the per-chunk silence timeout and drives delivery lives in `RunInSandboxStreaming` (§9.5). It kills the process group by cancelling `ctx` (total timeout) or firing on the silence timer:

```go
silence := time.NewTimer(cfg.ChunkTimeout)
defer silence.Stop()

for {
	select {
	case chunk, ok := <-chunkCh:
		if !ok {
			return finalize(nil) // channel closed → clean EOF
		}
		if !silence.Stop() {
			<-silence.C
		}
		silence.Reset(cfg.ChunkTimeout)
		deliver(chunk) // print + batch for persistence
	case <-silence.C:
		killGroup(cmd) // SIGKILL to the process group
		return finalize(TimeoutChunkSilence)
	case <-ctx.Done(): // total-run deadline (context.WithTimeout)
		killGroup(cmd)
		return finalize(TimeoutTotal)
	}
}
```

#### 9.4.1 Cross-platform note (no Windows fallback needed)

The Python design required a separate `select(2)` path (Unix) and a thread-per-stream path (Windows). In Go this bifurcation disappears: `os/exec` pipes are drained by goroutines on every platform, so the single `streamProcess` implementation above works on Linux, macOS, and Windows. Only the process-group kill differs by OS and is isolated in a build-tagged helper:

```go
// kill_unix.go  (//go:build !windows)
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		// cmd.SysProcAttr.Setpgid = true was set at construction;
		// negative pid signals the whole process group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// kill_windows.go  (//go:build windows)
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill() // no process-group semantics; kills the direct child only
	}
}
```

### 9.5 SQLite Chunk Persistence (Batched Writer)

To avoid per-chunk SQLite writes at high throughput, chunks are collected in a slice and flushed in batches inside a transaction against the `modernc.org/sqlite` (`database/sql`) connection:

```go
// internal/sandbox/persist.go

const (
	batchSize     = 10
	batchInterval = 500 * time.Millisecond
)

func flushChunks(ctx context.Context, db *sql.DB, batch []OutputChunk) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO sandbox_run_chunks
		   (run_id, seq, stream, text, offset_ms, received_at)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, c := range batch {
		if _, err := stmt.ExecContext(ctx,
			c.RunID, c.Seq, string(c.Stream), c.Text, c.OffsetMs, c.ReceivedAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sandbox_runs
		    SET chunk_count = chunk_count + ?, last_chunk_at = ?
		  WHERE id = ?`,
		len(batch), batch[len(batch)-1].ReceivedAt, batch[0].RunID); err != nil {
		return err
	}
	return tx.Commit()
}
```

`RunInSandboxStreaming` is the public entry point. It returns a receive-only chunk channel and a func the caller blocks on for the `StreamRunResult` — the idiomatic Go replacement for the Python generator whose `StopIteration.value` carried the result. Persistence runs in a dedicated goroutine so terminal printing and DB writes proceed concurrently.

```go
// internal/sandbox/stream.go

func RunInSandboxStreaming(
	ctx context.Context,
	db *sql.DB,
	cfg StreamConfig,
) (<-chan OutputChunk, func() (StreamRunResult, error), error) {
	if err := EnsureSchema(ctx, db); err != nil {
		return nil, nil, err
	}
	runID := newRunID() // uuid.NewString()[:12] via google/uuid

	argv, tmpPath, err := resolveCommand(cfg)
	if err != nil {
		return nil, nil, err
	}

	sourceType := "command"
	switch {
	case cfg.Code != "":
		sourceType = "code_string"
	case cfg.File != "":
		sourceType = "file"
	}
	image := sql.NullString{}
	if cfg.Backend == "docker" {
		image = sql.NullString{String: cfg.Image, Valid: true}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sandbox_runs
		   (id, command, backend, image, status, created_at, streamed, language, source_type)
		 VALUES (?, ?, ?, ?, 'running', ?, 1, ?, ?)`,
		runID, strings.Join(argv, " "), cfg.Backend, image, utcNow(),
		cfg.Language, sourceType); err != nil {
		return nil, nil, err
	}

	// Print run ID immediately to stderr so it never pollutes --json stdout.
	fmt.Fprintf(os.Stderr, "Run ID: %s\n", runID)

	// Total-run deadline drives exec.CommandContext.
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = cfg.Workdir
	cmd.Env = buildEnv(cfg.EnvOverrides)  // PATH + explicit overrides only
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // enable process-group kill

	// The isolation ladder is applied here for the "restricted" tier:
	// landlock-lsm/go-landlock (fs), elastic/go-seccomp-bpf (syscalls),
	// google/nftables (egress) are installed against the child before exec;
	// "docker" delegates to the moby client; see internal/sandbox/isolate_linux.go.
	if err := applyIsolation(cmd, cfg); err != nil {
		cancel()
		return nil, nil, err
	}

	startMono := time.Now()
	chunkCh, wait, err := streamProcess(runCtx, cmd, runID, startMono)
	if err != nil {
		cancel()
		return nil, nil, err
	}

	// Fan the chunks to (a) the caller and (b) the batched DB writer.
	out := make(chan OutputChunk, 256)
	done := make(chan struct{})
	result := StreamRunResult{RunID: runID}

	go func() {
		defer close(out)
		defer close(done)
		defer cancel()
		if tmpPath != "" {
			defer os.Remove(tmpPath)
		}

		batch := make([]OutputChunk, 0, batchSize)
		lastFlush := time.Now()
		silence := time.NewTimer(cfg.ChunkTimeout)
		defer silence.Stop()

		flush := func() {
			if err := flushChunks(ctx, db, batch); err == nil {
				batch = batch[:0]
				lastFlush = time.Now()
			}
		}

	loop:
		for {
			select {
			case c, ok := <-chunkCh:
				if !ok {
					break loop
				}
				if !silence.Stop() {
					<-silence.C
				}
				silence.Reset(cfg.ChunkTimeout)
				result.ChunkCount++
				if c.Stream == StreamStdout {
					result.StdoutChunks++
				} else {
					result.StderrChunks++
				}
				batch = append(batch, c)
				out <- c
				if len(batch) >= batchSize || time.Since(lastFlush) >= batchInterval {
					flush()
				}
			case <-silence.C:
				killGroup(cmd)
				result.TimedOutReason = TimeoutChunkSilence
				break loop
			case <-runCtx.Done():
				if runCtx.Err() == context.DeadlineExceeded {
					result.TimedOutReason = TimeoutTotal
				}
				killGroup(cmd)
				break loop
			}
		}

		flush() // final drain
		waitErr := wait()
		result.DurationMs = time.Since(startMono).Milliseconds()
		result.ExitCode = exitCodeOf(waitErr, cmd)
		result.Status = classifyStatus(result, ctx.Err())

		_, _ = db.ExecContext(context.Background(),
			`UPDATE sandbox_runs
			    SET status=?, exit_code=?, completed_at=?, timed_out_reason=?
			  WHERE id=?`,
			result.Status, result.ExitCode, utcNow(),
			nullReason(result.TimedOutReason), runID)
	}()

	waitResult := func() (StreamRunResult, error) {
		<-done
		return result, nil
	}
	return out, waitResult, nil
}
```

If the caller abandons the run (e.g. Ctrl+C), it cancels the parent `ctx`; `runCtx.Done()` fires, the process group is killed, and `classifyStatus` records `'interrupted'` — the Go equivalent of the Python `GeneratorExit` handler.

### 9.6 `tag sandbox logs` — Replay and Follow

Replay/follow is exposed as a channel producer. `streamFilter` is validated against an allowlist and mapped to a parameterized `stream = ?` predicate (never interpolated) to avoid SQL injection. In follow mode it polls for new rows until the run is no longer `'running'`, then performs one final drain.

```go
// internal/sandbox/logs.go

func StreamSandboxLogs(
	ctx context.Context,
	db *sql.DB,
	runID string,
	follow bool,
	sinceSeq int64,
	streamFilter string, // "stdout" | "stderr" | "both"
	pollInterval time.Duration, // default 200ms
) (<-chan OutputChunk, error) {
	pred := ""
	var extra []any
	switch streamFilter {
	case "stdout", "stderr":
		pred = " AND stream = ?"
		extra = []any{streamFilter}
	case "", "both":
	default:
		return nil, fmt.Errorf("invalid --stream %q (want stdout|stderr|both)", streamFilter)
	}

	out := make(chan OutputChunk, 64)
	go func() {
		defer close(out)
		lastSeq := sinceSeq
		query := `SELECT run_id, seq, stream, text, offset_ms, received_at
		            FROM sandbox_run_chunks
		           WHERE run_id = ? AND seq > ?` + pred + ` ORDER BY seq ASC`

		drain := func() error {
			args := append([]any{runID, lastSeq}, extra...)
			rows, err := db.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var c OutputChunk
				if err := rows.Scan(&c.RunID, &c.Seq, &c.Stream,
					&c.Text, &c.OffsetMs, &c.ReceivedAt); err != nil {
					return err
				}
				lastSeq = c.Seq
				select {
				case out <- c:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return rows.Err()
		}

		for {
			if err := drain(); err != nil {
				return
			}
			if !follow {
				return
			}
			var status string
			err := db.QueryRowContext(ctx,
				`SELECT status FROM sandbox_runs WHERE id = ?`, runID).Scan(&status)
			if err != nil || status != "running" {
				_ = drain() // final drain for chunks written in the last poll window
				return
			}
			select {
			case <-time.After(pollInterval):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
```

### 9.7 TUI Display (`internal/tui` — `SandboxStreamPrinter`)

A lightweight printer replaces the Rich `Live` panel. It writes each chunk as it arrives, styling with `lipgloss` only when stdout is a TTY (`golang.org/x/term.IsTerminal`); otherwise it emits plain, escape-free lines. Untrusted chunk text is passed through an ANSI-strip before rendering (see §10.5).

```go
// internal/tui/sandbox_stream.go
package tui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

var (
	dimStyle = lipgloss.NewStyle().Faint(true)
	errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // amber
)

type SandboxStreamPrinter struct {
	w       io.Writer
	runID   string
	noColor bool
	tty     bool
}

func NewSandboxStreamPrinter(w io.Writer, runID string, noColor bool) *SandboxStreamPrinter {
	tty := false
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		tty = term.IsTerminal(int(f.Fd()))
	}
	return &SandboxStreamPrinter{w: w, runID: runID, noColor: noColor, tty: tty}
}

func (p *SandboxStreamPrinter) Start() {
	fmt.Fprintf(p.w, "Run ID: %s\n", p.runID)
	fmt.Fprintln(p.w, strings.Repeat("─", 79))
}

func (p *SandboxStreamPrinter) AddChunk(c sandbox.OutputChunk) {
	label := "OUT"
	if c.Stream == sandbox.StreamStderr {
		label = "ERR"
	}
	ts := fmt.Sprintf("%7.3fs", float64(c.OffsetMs)/1000)
	line := stripANSI(strings.TrimRight(c.Text, "\n"))
	if p.noColor || !p.tty {
		fmt.Fprintf(p.w, "  %s  %s  %s\n", ts, label, line)
		return
	}
	prefix := dimStyle.Render("  " + ts + "  ")
	if c.Stream == sandbox.StreamStderr {
		fmt.Fprintf(p.w, "%s%s\n", prefix, errStyle.Render(label+"  "+line))
	} else {
		fmt.Fprintf(p.w, "%s%s\n", prefix, label+"  "+line)
	}
}

func (p *SandboxStreamPrinter) End() {
	fmt.Fprintln(p.w, strings.Repeat("─", 79))
}
```

### 9.8 Integration Points

| Integration | Mechanism |
|-------------|-----------|
| `cmd/tag/sandbox.go` (`sandbox run`) | Parses `--stream`, `--code`, `--file`, `--language`, `--chunk-timeout`, `--json` flags (koanf/v2 binds config defaults); calls `RunInSandboxStreaming()` if streaming; delegates to existing `RunInSandbox()` otherwise |
| `cmd/tag/sandbox.go` (`sandbox logs`) | New subcommand; calls `StreamSandboxLogs()` with `--follow`, `--since`, `--stream` args |
| `go.opentelemetry.io/otel` | Emits `sandbox.stream_open` span at stream start and `sandbox.stream_close` span at end; attributes: `run_id`, `backend`, `chunk_count`, `exit_code`, `duration_ms` |
| `internal/cron` (TTL sweeper) | Existing sweeper extended to delete `sandbox_run_chunks` rows where `received_at < now - chunk_retention_days` AND `run_id NOT IN (SELECT id FROM sandbox_runs WHERE status = 'running')` |
| `internal/tui` | `SandboxStreamPrinter` provides coloured line output; falls back to plain print when stdout is not a TTY |
| `internal/security` | Blocked-path pattern checks (PRD-034) plus isolation-ladder setup called before `cmd.Start()`; streaming does not modify the pre-execution security gate |
| `internal/server` (deferred, NG1) | Same `<-chan OutputChunk` seam can back an SSE endpoint (`tmaxmax/go-sse` over `go-chi/chi` v5, spec'd with `huma` v2) for remote follow |

---

## 10. Security Considerations

1. **Temp file permissions:** Code strings passed via `--code` are written via `os.CreateTemp` and chmod'd to `0o600`. The temp file is removed in a `defer os.Remove(...)` even if the process is killed. On systems with a shared `/tmp`, this prevents other users from reading the code before execution.

2. **Pre-execution security gate:** All existing PRD-028 blocked-path and command-allowlist checks execute synchronously before `cmd.Start()` is called, together with the isolation-ladder setup (landlock/seccomp/nftables for the `restricted` tier). Streaming does not short-circuit or bypass these checks. The security gate is a hard pre-condition.

3. **Environment variable isolation:** `cmd.Env` is set explicitly (never left nil, which would inherit `os.Environ()`) and contains only `PATH` plus explicit `--env` overrides. Host secrets (`AWS_SECRET_ACCESS_KEY`, `OPENAI_API_KEY`, etc.) present in the parent process environment are not inherited, matching the behavior of the existing restricted backend.

4. **Process group kill on timeout:** The child is launched with `SysProcAttr{Setpgid: true}`, and `killGroup` sends `SIGKILL` to the whole group via `syscall.Kill(-pgid, syscall.SIGKILL)` so that grandchildren spawned by the script cannot escape the kill. For the `docker` backend this is a non-issue (container cleanup via the moby client kills all descendants). On Windows there are no process-group semantics, so only the direct child is killed (documented limitation).

5. **Chunk text sanitization for TUI:** Raw output from the child process may contain ANSI escape sequences, including cursor-movement and terminal-reset codes that could corrupt the terminal display. When outputting to a TTY, `stripANSI` removes ANSI control sequences from chunk text before styling with `lipgloss`. This prevents a malicious script from, e.g., overwriting terminal history.

6. **JSONL injection:** In `--json` mode, chunk `Text` is embedded as a JSON string value. `encoding/json` (`json.Marshal` / `json.Encoder`) serializes the text field unconditionally — never `fmt.Sprintf` interpolation — preventing control-character injection in the JSON stream.

7. **Run ID entropy:** Run IDs are the first 12 hex chars of a `google/uuid` v4 (48 bits of entropy). This is sufficient for local isolation but not suitable as a secret token. `tag sandbox logs` requires the exact run ID; there is no wildcard query path that would allow enumeration.

8. **Chunk retention:** Chunks may contain sensitive output (API responses, partial secrets printed by scripts). The 7-day default TTL limits exposure. Users can set `sandbox.chunk_retention_days = 1` or `0` (delete on run completion) in config.

9. **Signal forwarding:** When the user sends `SIGINT` (Ctrl+C) to TAG, the run `context` (obtained via `signal.NotifyContext(ctx, os.Interrupt)`) is cancelled; `runCtx.Done()` fires, `killGroup(cmd)` kills the child process group, and the process exits 130. Without this, the child may continue running as an orphan. The context is established before `cmd.Start()`.

10. **Docker backend streaming:** For the `docker` backend, streaming reads the container's multiplexed output via the `docker/docker` moby client (`ContainerAttach` / `ContainerLogs` with `stdcopy.StdCopy` demultiplexing the 0x01/0x02 stream frames). The security properties of the existing Docker backend (network isolation, memory cap, CPU cap, and optionally gVisor/`runsc` as the runtime) are unchanged.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/sandbox/stream_test.go`)

Standard `testing` package with table-driven cases; `testing/synctest` or channel draining for timing assertions.

| Test | Description |
|------|-------------|
| `TestStreamBasicOutput` | Script prints 5 lines; assert 5 `OutputChunk` values received with correct `Text`, `Seq` monotonic, `Stream=stdout` |
| `TestStreamStderrSeparation` | Script writes to both stdout and stderr; assert chunks with `Stream=stderr` for stderr lines and `Stream=stdout` for stdout lines |
| `TestChunkTimeoutKillsProcess` | Script sleeps 60 s; set `ChunkTimeout=2s`; assert `StreamRunResult.TimedOutReason == TimeoutChunkSilence` within 3 s |
| `TestTotalTimeoutKillsProcess` | Script loops forever printing; set `Timeout=2s`; assert `StreamRunResult.TimedOutReason == TimeoutTotal` |
| `TestChunksPersistedToDB` | After streaming, query `sandbox_run_chunks`; assert count == number of chunks received |
| `TestChunksOrderedBySeq` | Assert `SELECT seq FROM sandbox_run_chunks WHERE run_id=? ORDER BY seq` has no gaps and starts at 1 |
| `TestNonStreamingPathUnchanged` | Call `RunInSandbox()` (no stream); assert `CombinedOutput()` path used, not pipes; assert output in `sandbox_runs.output` as before |
| `TestCodeTempFileDeleted` | After `--code` run (success and failure), assert temp file does not exist |
| `TestLanguageInferenceFromExtension` | `.py` → `python`, `.sh` → `bash`, `.js` → `node`, `.rb` → `ruby`; `.cpp` → error with supported-list message |
| `TestMaxLineBytesBoundary` | Script writes a 70,000-byte line without newline; assert two chunks are produced (first at 65536 bytes) |
| `TestSIGINTSetsInterruptedStatus` | Cancel the run `context` mid-stream; assert `sandbox_runs.status == 'interrupted'` |
| `TestSchemaMigrationIdempotent` | Call `EnsureSchema()` twice on a fresh DB; assert no error and all columns present |
| `TestSchemaMigrationExistingDB` | Populate a DB with pre-PRD schema (no streaming columns); call `EnsureSchema()`; assert new columns added without data loss |
| `TestBatchFlushAt10Chunks` | Inject a spy DB; emit 25 chunks; assert `flushChunks` called 3 times (at 10, 20, and final drain) |
| `TestProcessGroupKill` | Script spawns a child subprocess that sleeps; apply chunk timeout; assert child subprocess also killed (poll `/proc` or `os.FindProcess`+`Signal(0)`) |

### 11.2 Integration Tests (`internal/sandbox/stream_integration_test.go`, build tag `integration`)

| Test | Description |
|------|-------------|
| `TestCLIStreamFlag` | `exec.Command("tag", "sandbox", "run", "--code", "print(1)", "--language", "python", "--stream")` exits 0, stdout contains "1" |
| `TestCLIFileFlag` | Write a temp `.py` file; run with `--file`; assert output matches |
| `TestCLIJSONFlag` | With `--json`, assert stdout is valid JSONL: `json.Unmarshal` each line, assert `event` field present |
| `TestSandboxLogsReplay` | Run a script, capture run_id, then `tag sandbox logs <run-id>`; assert identical output |
| `TestSandboxLogsFollow` | Start a long-running script in a goroutine, attach `--follow` in main goroutine, assert lines arrive in order before run completes |
| `TestSandboxLogsSince` | Run 10-line script, then `tag sandbox logs <id> --since 5`; assert only seqs 6-10 returned |
| `TestConfigStreamByDefault` | Set `sandbox.stream_by_default=true` in config (koanf); run without `--stream`; assert streaming behavior |

### 11.3 Performance Tests / Benchmarks (`internal/sandbox/stream_bench_test.go`)

| Benchmark / Test | Target |
|------|--------|
| `BenchmarkThroughput10kLines` | Script with `for i in range(10000): print(i)` completes with all 10,000 chunks in < 5 s |
| `TestTTFCUnder200ms` | Measure `time.Since(start)` from `cmd.Start()` to first chunk received; assert < 200 ms |
| `TestMemoryOverheadHighVolume` | Run 100,000-line script; sample `runtime.ReadMemStats` HeapInuse delta; assert < 32 MB increase |
| `BenchmarkDBWriteLatency` | Time `flushChunks()` with 10-chunk batch on a WAL-mode DB; assert < 50 ms |
| `TestConcurrentRuns` | Launch 5 simultaneous streaming runs; assert no chunk cross-contamination (each run's chunks reference only its run_id) |

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag sandbox run --code "for i in range(10): print(i)" --language python --stream` prints each number on a separate line as it is produced, not all at once after the loop. | Manual test: observe incremental output with `time.sleep(0.1)` between prints |
| AC-02 | Each printed line is prefixed with a relative timestamp (format: `N.NNNs`) and a stream label (`OUT` or `ERR`). | Go test: assert prefix format via `regexp.MustCompile(`^\s+\d+\.\d{3}s\s+(OUT|ERR)\s+`)` |
| AC-03 | The run ID is printed to stderr before the first chunk line appears on stdout. | Integration test: capture stderr and stdout separately; assert run_id line in stderr precedes first stdout line |
| AC-04 | After a streaming run completes, `SELECT COUNT(*) FROM sandbox_run_chunks WHERE run_id=?` returns the exact number of lines the script printed. | Integration test with a script that prints exactly 47 lines |
| AC-05 | `tag sandbox logs <run-id>` replays all chunks in sequence order with identical relative timestamps. | Integration test: compare streamed output vs. `logs` output |
| AC-06 | `tag sandbox logs <run-id> --follow` begins printing new lines within 500 ms of them being written to `sandbox_run_chunks`. | Integration test: two-thread test measuring latency |
| AC-07 | A script that produces no output for 35 seconds is killed within 500 ms of the 30-second chunk-timeout and the run record shows `status='timeout'` and `timed_out_reason='chunk_silence'`. | Integration test with `time.sleep(35)` script and `--chunk-timeout 30` |
| AC-08 | A script running for more than `--timeout 10` seconds is killed and the run record shows `status='timeout'` and `timed_out_reason='total_timeout'`. | Integration test with `while True: time.sleep(1); print("x")` and `--timeout 10` |
| AC-09 | `tag sandbox run --code "..." --stream` without `--language` defaults to Python. | Go test: assert interpreter slice starts with `python3` |
| AC-10 | `tag sandbox run --file script.sh --stream` infers `bash` from the `.sh` extension. | Go test: assert interpreter `bash` selected |
| AC-11 | `tag sandbox run --file script.cpp --stream` exits with a non-zero code and an error message listing supported languages. | Integration test: assert exit code ≠ 0 and message contains "supported:" |
| AC-12 | In `--json` mode, every event is valid JSON parseable by `json.Unmarshal`. | Integration test: unmarshal all stdout lines |
| AC-13 | The temp file created for `--code` input is deleted after the run, even when the process is killed by timeout. | Integration test: `os.Stat(tmpPath)` after run; assert `os.IsNotExist(err)` |
| AC-14 | Running `tag sandbox run` (no `--stream`) produces identical output format and exit code as before this PRD was implemented. | Regression test comparing pre/post behavior using captured command output |
| AC-15 | `sandbox_runs` rows created before this PRD (without the new columns) can be read by `get_sandbox_run()` without error after schema migration. | Schema migration test with pre-seeded fixture |
| AC-16 | Pressing Ctrl+C during `--stream` kills the child process and sets `sandbox_runs.status='interrupted'`. | Manual test confirmed by checking DB after interrupt |
| AC-17 | Lines exceeding 65,536 bytes without a newline are split into multiple chunks of ≤ 65,536 bytes each. | Unit test with a 70,000-byte line |
| AC-18 | `tag config set sandbox.stream_by_default true` causes subsequent `tag sandbox run` invocations to stream without `--stream` flag. | Integration test: set config, run without flag, assert streaming behavior |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-028 (Sandbox Code Execution) | Hard prerequisite | `sandbox_runs` table, `EnsureSchema()`, `RunInSandbox()`, backend dispatch + isolation ladder; must be merged and deployed first |
| PRD-013 (Agent Tracing) | Soft prerequisite | `go.opentelemetry.io/otel` span emission for `sandbox.stream_open/close`; gracefully no-ops if tracing is disabled |
| PRD-003 (Streaming TUI) | Soft prerequisite | `internal/tui` printer patterns; feature degrades to plain `fmt.Fprintln` when not a TTY |
| PRD-034 (Secret Scanning) | Soft prerequisite | Blocked-path patterns from `internal/security` must run before `cmd.Start()`; feature degrades gracefully if not yet merged |
| `os/exec`, `bufio`, `context`, `os/signal`, `syscall` (stdlib) | None | Go stdlib; no module add required |
| `golang.org/x/sync/errgroup` | go.mod | Reader-goroutine lifecycle / fan-in |
| `golang.org/x/term` | go.mod | TTY detection for coloured vs. plain output |
| `modernc.org/sqlite` | go.mod | Pure-Go (CGO_ENABLED=0) SQLite driver for `sandbox_run_chunks` persistence |
| `github.com/google/uuid` | go.mod | Run ID generation (already used in sandbox pkg) |
| `github.com/charmbracelet/lipgloss` | go.mod | Terminal styling (existing TAG dep) |
| `github.com/docker/docker` (moby client) | go.mod | `docker` backend stream demux (`stdcopy.StdCopy`); only when Docker backend selected |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should `--stream` be the default for all new `tag sandbox run` invocations eventually, with `--no-stream` as the opt-out? Or should non-streaming remain the permanent default? The current design makes `--stream` opt-in. | Product | Before v1.0 GA of streaming feature |
| OQ-2 | Should `tag sandbox logs --follow` use SQLite polling (current design, 200 ms interval) or a SQLite `NOTIFY`-style mechanism? SQLite has no native pub/sub; alternatives include a Unix socket, a shared memory flag, or a file-watch on the WAL file. | Engineering | Can be deferred to a follow-on PRD if polling latency is acceptable |
| OQ-3 | Should chunk text be stored compressed (e.g., zstd) to reduce storage for high-volume runs? A 100,000-line run at average 50 bytes/line is 5 MB raw. zstd at 5:1 would be 1 MB. The tradeoff is read complexity and CPU overhead during writes. | Engineering | Evaluate at scale; defer to follow-on if storage is not a user complaint |
| OQ-4 | Should E2B and Modal backends be integrated with the streaming interface in this PRD or in a follow-on? E2B's `on_stdout`/`on_stderr` callbacks and Modal's async iterables can both be adapted to yield `OutputChunk` instances. The implementation is straightforward but requires E2B/Modal SDK dependencies and credentials in CI. | Engineering | Follow-on PRD targeting E2B and Modal streaming integration specifically |
| OQ-5 | Should `tag sandbox run --stream --json` be the recommended interface for programmatic consumers (e.g., `internal/queue`, `internal/loop`)? If so, should `internal/queue` parse the JSONL stream in a follow-on PRD to provide structured job progress? | Product | Follow-on PRD for queue integration |
| OQ-6 | Should there be a `tag sandbox run --stream --output-file path.log` flag to write the full stream to a file simultaneously with terminal display (tee behavior)? | Product | Low priority; can be achieved with shell tee in the interim |
| OQ-7 | What is the correct behavior for `tag sandbox logs --follow` if the run_id does not exist? Currently returns immediately with no output. Should it poll for the run to appear (useful for race conditions in programmatic use) or error immediately? | Engineering | Error immediately with clear message; polling on non-existent run_id is a footgun |

---

## 15. Complexity and Timeline

**Total estimated effort: 3–5 days (S)**

### Phase 1 — Schema and Core Streaming Engine (Day 1–2)

- Add `sandbox_run_chunks` table DDL to `EnsureSchema()` in `internal/sandbox/schema.go`
- Add new columns to `sandbox_runs` using the `addColumnIfMissing()` migration helper
- Implement `OutputChunk`, `StreamRunResult`, `StreamConfig` structs
- Implement `scanStream()` + `streamProcess()` goroutine fan-in over `StdoutPipe`/`StderrPipe`
- Implement the build-tagged `killGroup()` helper (Setpgid group kill on Unix; direct kill on Windows)
- Implement `flushChunks()` batched writer with 10-chunk / 500 ms dual threshold
- Implement `RunInSandboxStreaming()` public entry point (channel + wait func)
- Unit tests: `TestStreamBasicOutput`, `TestStreamStderrSeparation`, `TestChunkTimeoutKillsProcess`, `TestTotalTimeoutKillsProcess`, `TestChunksPersistedToDB`, `TestChunksOrderedBySeq`, `TestNonStreamingPathUnchanged`

### Phase 2 — Code/File Input, Logs Command, TUI (Day 2–3)

- Implement `resolveCommand()` with `--code` temp file and `--file` language inference
- Implement `StreamSandboxLogs()` replay and follow producer
- Add `SandboxStreamPrinter` to `internal/tui`
- Implement `--json` JSONL output mode (`encoding/json`)
- Add `sandbox logs` subcommand to `cmd/tag/sandbox.go`
- Extend `sandbox run` command with all new flags
- Unit tests: `TestLanguageInferenceFromExtension`, `TestCodeTempFileDeleted`, `TestMaxLineBytesBoundary`, `TestSIGINTSetsInterruptedStatus`, `TestBatchFlushAt10Chunks`

### Phase 3 — Integration Tests, Performance, Security Hardening (Day 3–4)

- Integration tests: full CLI invocation tests for all acceptance criteria
- Benchmarks: throughput, TTFC, memory overhead, DB write latency, concurrent runs
- Process group kill implementation (`SysProcAttr{Setpgid:true}` + `syscall.Kill(-pgid, SIGKILL)`)
- ANSI strip (`stripANSI`) for TUI output from untrusted child processes
- `signal.NotifyContext` wiring for clean SIGINT interrupt
- TTL sweeper extension in `internal/cron` for chunk retention
- OTel span emission (`go.opentelemetry.io/otel`) for `sandbox.stream_open/close`

### Phase 4 — Documentation, Config, Final QA (Day 4–5)

- `tag config set sandbox.stream_by_default`, `sandbox.chunk_timeout_seconds`, `sandbox.default_timeout_seconds`, `sandbox.chunk_retention_days`
- Schema migration test with pre-seeded fixture
- Update `docs/prd/INDEX.md` to reference PRD-089
- Final acceptance criteria verification pass
- Merge and tag release

### Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Goroutine/channel deadlock if a reader blocks on a full channel while the consumer is stuck on a kill path | Medium | Medium | Bounded channel + `select` with `ctx.Done()` on every send; `errgroup` ensures both readers terminate before the channel closes; race-detector (`go test -race`) in CI |
| High-throughput SQLite writes causing WAL file growth | Low | Low | WAL mode + batch writes minimize contention; add `PRAGMA wal_checkpoint(TRUNCATE)` hint after run completion |
| Orphaned child processes on Windows (process-group kill not available) | Medium | Low | Windows `killGroup` uses `Process.Kill()` which kills the direct child but not descendants; document limitation |
| Isolation ladder is Linux-only (landlock/seccomp/nftables); off-Linux the sandbox degrades to a plain subprocess or Docker Desktop | Medium | Medium | Feature-detect at startup; streaming itself is OS-agnostic and unaffected, but the security posture of `--backend restricted` off-Linux must be clearly documented |
| Breaking change if existing callers of `RunInSandbox()` expect the old return shape | Low | High | Streaming path is a new function `RunInSandboxStreaming()`; existing `RunInSandbox()` is untouched |

