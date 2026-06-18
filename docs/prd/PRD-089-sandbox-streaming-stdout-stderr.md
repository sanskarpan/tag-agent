# PRD-089: Real-Time Streaming stdout/stderr from Sandbox (`tag sandbox run --stream`)

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

TAG's sandbox subsystem (PRD-028) currently executes code via `subprocess.run(..., capture_output=True)`, which buffers all stdout and stderr internally and delivers the complete output blob only after the process exits. For short scripts this is invisible, but for any workload that runs longer than a few seconds — ML training loops, data-pipeline scripts, generative programs that print progress, or build tools — this buffering creates an opaque, silent black box. The user has no feedback while the process is running, cannot distinguish a hung process from a slow one, and receives a potentially enormous output dump on completion that is difficult to navigate.

This PRD specifies the streaming execution path for `tag sandbox run`. The core change is replacing the blocking `subprocess.run` with `subprocess.Popen` combined with a non-blocking line reader that emits stdout and stderr lines as they arrive from the child process. When `--stream` is passed (or when streaming is enabled globally via config), TAG prints each line to the terminal the instant the child process flushes it, prefixed with a stream label and a relative timestamp. Every chunk is also written to a new `sandbox_run_chunks` table so that `tag sandbox logs <run-id>` can replay the full timestamped output even after the run completes, and `tag sandbox logs <run-id> --follow` can tail a currently-running sandbox in a second terminal.

The design draws directly from how serious sandbox providers handle this problem in production. E2B's Python SDK exposes `on_stdout` / `on_stderr` callbacks on `sandbox.process.start()`. Docker uses `docker attach` and the multiplexed stream protocol (a one-byte stream identifier — 0x01 for stdout, 0x02 for stderr — prepended to each 8-byte frame header). Modal's `Sandbox.stdout` / `Sandbox.stderr` are async iterables. TAG's implementation adapts these patterns to Python's `subprocess.Popen` + `select`-based I/O multiplexing for the local backends, and wraps provider SDKs appropriately for E2B and Modal. The architecture is backend-agnostic: the streaming interface is expressed as a Python generator of `OutputChunk` dataclass instances regardless of which backend produces them.

Timeout enforcement is two-layered in the streaming path. A per-chunk timeout (default: 30 s) fires if no output is received from either stream for that duration — indicating the process is alive but silent, which could be a deadlock or an infinite wait on input. A total-run timeout (passed as `--timeout`, default: 300 s) fires if the process has not exited by the wall-clock deadline. Both timeouts kill the process, update the run record with `status='timeout'` and the correct `timed_out_reason`, and flush any remaining buffered chunks. This two-layer design avoids the single-timeout ambiguity present in the current `subprocess.run` path where a 60-second timeout does not distinguish between "ran for 60 s then exceeded budget" and "hung on first read for 60 s".

The PRD also specifies schema additions to `sandbox_runs` (new columns for streaming metadata) and the new `sandbox_run_chunks` table for chunk persistence. It integrates with `tui_output.py`'s Rich Live panel for coloured inline streaming and with `tracing.py` for span emission on stream open/close events.

---

## 2. Problem Statement

### 2.1 Blocking Execution Creates a Silent Black Box for Long-Running Jobs

The current `_run_restricted` and `_run_docker` functions both call `subprocess.run(..., capture_output=True)`. From the user's perspective, after typing `tag sandbox run --code "..."` the terminal freezes until the process exits. There is no spinner, no partial output, no progress indication. A 90-second data-processing job looks identical to a hung process. Users routinely kill TAG with `Ctrl+C` assuming a hang when the process was actually progressing normally. This results in lost work and erodes trust in the sandbox feature.

### 2.2 Post-Run Output Blobs Are Unusable for Iterative Workflows

When a long-running sandbox job does complete, `output` is returned as a single TEXT blob concatenating stdout and stderr with a `\n---stderr---\n` delimiter. For jobs that print thousands of lines (e.g., `for i in range(10000): print(i)` or a training loop emitting loss values), this blob is difficult to read in the terminal, impossible to search without scrolling, and discards all timing information. There is no way to know whether line 5000 appeared at second 2 or second 88 of the run.

### 2.3 No Mechanism to Observe a Running Sandbox from a Second Terminal

Once `tag sandbox run` is invoked, the run ID is not visible until the process exits. Even if the user captures the run ID (e.g., from a programmatic API call), there is no `tag sandbox logs <run-id> --follow` command to attach to the live output stream. This blocks multi-terminal workflows where an operator launches a long job in one window and monitors its output in another, a workflow that is standard in Docker (`docker logs -f`) and all major CI systems.

---

## 3. Goals and Non-Goals

### Goals

| # | Goal |
|---|------|
| G1 | Replace blocking `subprocess.run` with `subprocess.Popen` + streaming reader on all local backends when `--stream` is passed or streaming is globally enabled. |
| G2 | Emit stdout and stderr lines to the terminal as they arrive, with stream label (`OUT` / `ERR`) and relative timestamp, formatted via `tui_output.py` Rich panel. |
| G3 | Persist every output chunk (text, stream, relative offset ms, sequence number) to a new `sandbox_run_chunks` table so the full ordered output is queryable after the run completes. |
| G4 | Implement `tag sandbox logs <run-id>` for post-run replay and `tag sandbox logs <run-id> --follow` for live tail of an in-progress run. |
| G5 | Enforce a per-chunk silence timeout (default: 30 s) and a total-run timeout (default: 300 s, overridable via `--timeout`), each killing the process and recording `status='timeout'` with the specific reason. |
| G6 | Support `--stream` flag on `tag sandbox run` and a `sandbox.stream_by_default = true` config key. |
| G7 | Support code-string input (`--code`) and file input (`--file`) with automatic language detection from file extension. |
| G8 | Expose the run ID immediately at stream start so the user can reference it in a second terminal before the run completes. |
| G9 | Extend `sandbox_runs` schema with streaming-specific columns (`streamed`, `chunk_count`, `last_chunk_at`, `timed_out_reason`) without breaking existing rows. |
| G10 | Keep the non-streaming path (`subprocess.run`) fully intact as the default when `--stream` is not passed, ensuring zero regression for existing users. |

### Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | WebSocket-based streaming to a browser or remote client. All streaming in this PRD is terminal-local. Remote streaming is deferred (see Open Questions). |
| NG2 | PTY / interactive terminal allocation (`pty.openpty()`). `--stream` targets non-interactive scripts that write to stdout/stderr. Interactive shell sessions are a separate feature. |
| NG3 | Structured JSON line output parsing or schema validation of streamed content. TAG streams raw text lines; parsing is left to the caller. |
| NG4 | Chunk compression or binary chunk support. All chunks are UTF-8 text. |
| NG5 | Modifying the E2B or Modal backend streaming integration in this PRD. Those backends use provider SDK streaming APIs and are noted as integration points but not fully implemented here. |
| NG6 | Changing how `queue_worker.py` dispatches sandbox jobs. Streaming output in queue context is a follow-on PRD. |
| NG7 | Infinite-retention chunk storage. Chunks older than a configurable TTL (default: 7 days) are swept by the existing TTL sweeper in `cron_scheduler.py`. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Time-to-first-chunk (TTFC) | < 200 ms from process spawn to first terminal line for a `print("hello")` Python script | Measured in integration test: `time.perf_counter()` diff between `Popen()` call and first `OutputChunk` yield |
| Throughput | ≥ 10,000 lines/s sustained for a tight `for i in range(100000): print(i)` loop without terminal lock-up | Performance test: count chunks delivered, assert total time < 10 s |
| Per-chunk silence timeout accuracy | Process killed within ±500 ms of the 30-second per-chunk deadline | Integration test with a `time.sleep(35)` script; assert killed_at − last_chunk_at < 30.5 s |
| Total timeout accuracy | Process killed within ±1 s of `--timeout` value | Integration test with `while True: time.sleep(1); print("x")` |
| Chunk persistence completeness | 100% of lines emitted by child process appear in `sandbox_run_chunks` in correct sequence | Compare streamed set vs. source set in integration test |
| `--follow` attach latency | `tag sandbox logs <id> --follow` begins printing within 500 ms of a new chunk appearing in the table | Integration test: two threads — one writing chunks, one polling via `--follow` |
| Non-streaming regression | `tag sandbox run` (no `--stream`) wall time unchanged vs. pre-PRD baseline within 5% | Benchmark test: 20 runs of 1-second script, compare means |
| Backward compatibility | All existing `sandbox_runs` rows queryable without migration error after schema addition | Schema migration test using a pre-seeded SQLite fixture |

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
| FR-01 | When `--stream` is passed (or `sandbox.stream_by_default` is true), `run_in_sandbox()` MUST use `subprocess.Popen` instead of `subprocess.run` and yield `OutputChunk` instances line-by-line as they arrive. | P0 |
| FR-02 | Each `OutputChunk` MUST carry: `run_id`, `seq` (monotonic integer), `stream` (`"stdout"` or `"stderr"`), `text` (the raw line including trailing newline), `offset_ms` (milliseconds since process spawn), `received_at` (ISO-8601 UTC). | P0 |
| FR-03 | Every `OutputChunk` MUST be persisted to `sandbox_run_chunks` within 500 ms of being received, even for runs that ultimately time out or are killed. | P0 |
| FR-04 | The `sandbox_runs` table MUST be updated with `streamed=1`, incrementing `chunk_count`, and updating `last_chunk_at` for every chunk received. These updates MUST be batched (every 10 chunks or every 500 ms) to avoid per-line SQLite writes at high throughput. | P1 |
| FR-05 | The streaming reader MUST multiplex stdout and stderr using `select.select()` on both file descriptors simultaneously; it MUST NOT read from stdout only and block if the process is writing only to stderr. | P0 |
| FR-06 | The per-chunk silence timeout (`--chunk-timeout`, default 30 s) MUST kill the process with `SIGKILL` if `select.select()` returns empty for that duration. Status MUST be set to `'timeout'` and `timed_out_reason` to `'chunk_silence'`. | P0 |
| FR-07 | The total-run timeout (`--timeout`, default 300 s) MUST kill the process via `proc.kill()` if the process has not exited by the deadline. Status MUST be set to `'timeout'` and `timed_out_reason` to `'total_timeout'`. | P0 |
| FR-08 | After killing the process on either timeout, the reader MUST drain any remaining data from both stdout and stderr buffers before closing the file descriptors. | P1 |
| FR-09 | The run ID MUST be printed to stderr before the first chunk is printed to stdout, so the user can record it while the run is still in progress. | P1 |
| FR-10 | `tag sandbox logs <run-id>` MUST replay chunks from `sandbox_run_chunks` ordered by `seq ASC`, formatted identically to the streaming output during the live run. | P0 |
| FR-11 | `tag sandbox logs <run-id> --follow` MUST poll `sandbox_run_chunks` for new rows every 200 ms using `SELECT ... WHERE seq > ?` and terminate when `sandbox_runs.status` is no longer `'running'`. | P0 |
| FR-12 | `--code` input MUST be written to a temporary file in a secure temp directory (`tempfile.mkstemp`) with mode `0o600`, executed via the appropriate interpreter for the specified `--language`, and the temp file MUST be deleted after the process exits (or is killed). | P0 |
| FR-13 | `--file` input MUST have its language auto-detected from the file extension (`.py` → python, `.sh` → bash, `.js` → node, `.rb` → ruby) if `--language` is not specified; an unsupported extension MUST produce a clear error with a list of supported languages. | P1 |
| FR-14 | In `--json` mode, each event (run_start, chunk, run_end, timeout) MUST be emitted as a complete, valid JSON object on its own line (JSONL format) to stdout. | P1 |
| FR-15 | The existing non-streaming `run_in_sandbox()` code path (no `--stream`) MUST remain unchanged in behavior; streaming MUST be opt-in with no performance regression on the default path. | P0 |
| FR-16 | Schema migrations (new columns on `sandbox_runs`, new `sandbox_run_chunks` table) MUST use `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`-equivalent logic and MUST be idempotent (safe to run against a database that already has the columns). | P0 |
| FR-17 | All security checks from PRD-028 (blocked path patterns, command allowlist for `restricted` backend) MUST execute before `Popen` is called; streaming does not bypass pre-execution validation. | P0 |
| FR-18 | If the child process's output line exceeds 65,536 bytes without a newline, the reader MUST force a chunk boundary at that byte limit to avoid unbounded memory accumulation. | P1 |
| FR-19 | In human-readable mode, `OUT` lines MUST be printed in the terminal's default color and `ERR` lines MUST be printed in yellow/amber to visually distinguish stderr, using Rich markup. | P2 |
| FR-20 | `tag sandbox logs` MUST support `--since <seq>` to start replay from a specific sequence number, enabling incremental fetches by external tooling. | P2 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Throughput:** The streaming reader must not be the bottleneck for processes that produce output at ≥ 10,000 lines/s. Internal buffering via `collections.deque` with a batch-flush thread handles SQLite writes asynchronously. | ≥ 10,000 lines/s |
| NFR-02 | **Memory:** Peak memory overhead of the streaming reader (chunk buffer + deque) must not exceed 32 MB for any single run, regardless of total output volume. Chunks are persisted and evicted from the in-process buffer. | ≤ 32 MB |
| NFR-03 | **SQLite write latency:** Chunk batch writes must complete within 50 ms per batch on a standard NVMe drive. WAL mode (already enabled in TAG) provides the necessary read/write concurrency. | ≤ 50 ms/batch |
| NFR-04 | **TTY compatibility:** The `--stream` output must not corrupt terminal state. Rich Live is used only in interactive TTY contexts; when stdout is piped or redirected, plain text lines (no ANSI escape codes) are emitted. Detect via `sys.stdout.isatty()`. | — |
| NFR-05 | **Signal handling:** SIGINT (Ctrl+C) during `--stream` must kill the child process, flush remaining chunks to SQLite, update status to `'interrupted'`, and exit with code 130. | — |
| NFR-06 | **Concurrent runs:** Multiple simultaneous `tag sandbox run --stream` invocations must not interfere with each other's chunk tables. Run IDs (UUID hex) provide full isolation. SQLite WAL mode allows concurrent readers/writers. | — |
| NFR-07 | **Portability:** The `select.select()` based reader must work on Linux and macOS. On Windows (no `select` on file descriptors), a thread-per-stream fallback must be used, implemented via `threading.Thread` with a `queue.Queue`. | Linux, macOS primary; Windows fallback |
| NFR-08 | **Chunk TTL:** `sandbox_run_chunks` rows older than `sandbox.chunk_retention_days` (default: 7) must be eligible for deletion by the TTL sweeper in `cron_scheduler.py`. The sweeper must not delete chunks for runs whose `status = 'running'`. | Default 7-day retention |
| NFR-09 | **Observability:** A tracing span `sandbox.stream_open` is emitted when streaming starts and `sandbox.stream_close` when it ends, carrying `run_id`, `chunk_count`, `duration_ms`, `exit_code`, and `timed_out_reason` attributes. | — |
| NFR-10 | **Zero new mandatory dependencies:** The streaming implementation uses only Python stdlib (`subprocess`, `select`, `threading`, `queue`, `tempfile`, `os`, `signal`). Rich (already a TAG dependency) is used for display. No new packages are introduced. | — |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change |
|------|--------|
| `src/tag/sandbox.py` | Primary implementation: new streaming path in `run_in_sandbox_streaming()`, schema additions, `stream_sandbox_logs()` generator, Windows thread fallback |
| `src/tag/controller.py` | Extend `cmd_sandbox_run` to handle `--stream`, `--code`, `--file`, `--language`, `--chunk-timeout`, `--json` flags; add `cmd_sandbox_logs` subcommand |
| `src/tag/tui_output.py` | New `SandboxStreamPanel` class wrapping Rich `Live` + `Table` for streaming display |

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

Since SQLite does not support `IF NOT EXISTS` on `ALTER TABLE ADD COLUMN`, the actual migration logic wraps each statement in an existence check:

```python
def _add_column_if_missing(conn: sqlite3.Connection, table: str, column: str, defn: str) -> None:
    existing = {row[1] for row in conn.execute(f"PRAGMA table_info({table})")}
    if column not in existing:
        conn.execute(f"ALTER TABLE {table} ADD COLUMN {column} {defn}")
```

#### 9.2.2 `sandbox_run_chunks` (new table)

```sql
CREATE TABLE IF NOT EXISTS sandbox_run_chunks (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,  -- rowid alias for O(1) appends
  run_id      TEXT NOT NULL,                       -- FK → sandbox_runs.id
  seq         INTEGER NOT NULL,                    -- monotonic per-run, starts at 1
  stream      TEXT NOT NULL CHECK(stream IN ('stdout','stderr')),
  text        TEXT NOT NULL,                       -- raw line text, UTF-8, may include trailing \n
  offset_ms   INTEGER NOT NULL,                    -- milliseconds since process spawn (perf_counter)
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

### 9.3 Core Dataclasses

```python
# src/tag/sandbox.py (additions)
from __future__ import annotations
import dataclasses
import datetime
from typing import Literal

StreamType = Literal["stdout", "stderr"]
TimeoutReason = Literal["chunk_silence", "total_timeout"]


@dataclasses.dataclass(slots=True)
class OutputChunk:
    """A single line of output from a streaming sandbox run."""
    run_id: str
    seq: int
    stream: StreamType
    text: str           # raw line, usually includes trailing \n
    offset_ms: int      # ms since proc spawn (monotonic)
    received_at: str    # ISO-8601 UTC


@dataclasses.dataclass(slots=True)
class StreamRunResult:
    """Final result returned by run_in_sandbox_streaming() after the generator is exhausted."""
    run_id: str
    exit_code: int | None
    status: str                              # 'done' | 'failed' | 'timeout' | 'interrupted'
    timed_out_reason: TimeoutReason | None
    duration_ms: int
    chunk_count: int
    stdout_chunks: int
    stderr_chunks: int


@dataclasses.dataclass(slots=True)
class StreamConfig:
    """Parameters for a streaming sandbox execution."""
    code: str | None = None                  # inline code string
    file: str | None = None                  # path to script file
    language: str = "python"                 # interpreter selection
    command: list[str] | None = None         # raw command (non-code path)
    backend: str = "restricted"
    image: str = "python:3.12-slim"
    timeout: int = 300                       # total run timeout (seconds)
    chunk_timeout: int = 30                  # per-chunk silence timeout (seconds)
    workdir: str | None = None
    env_overrides: dict[str, str] = dataclasses.field(default_factory=dict)
    json_output: bool = False
    no_color: bool = False
```

### 9.4 Core Streaming Algorithm

The streaming reader uses `select.select()` on Unix to multiplex stdout and stderr without blocking. A `threading.Timer` enforces the per-chunk silence timeout, and the main loop tracks total elapsed time against `--timeout`.

```python
# src/tag/sandbox.py — streaming runner (simplified pseudocode showing key logic)

import os
import select
import signal
import subprocess
import sys
import tempfile
import threading
import time
import queue as _queue
from collections import deque
from pathlib import Path


_LANGUAGE_INTERPRETERS = {
    "python": ["python3", "-u"],   # -u: unbuffered
    "bash":   ["bash"],
    "node":   ["node"],
    "ruby":   ["ruby"],
}

_EXT_TO_LANGUAGE = {
    ".py": "python", ".sh": "bash", ".js": "node", ".rb": "ruby",
}

MAX_LINE_BYTES = 65_536   # force chunk boundary at 64 KiB for lines without newline


def _resolve_command(cfg: StreamConfig) -> list[str]:
    """Build the argv list from StreamConfig code/file/command."""
    if cfg.command:
        return cfg.command
    lang = cfg.language.lower()
    interpreter = _LANGUAGE_INTERPRETERS.get(lang)
    if interpreter is None:
        supported = ", ".join(_LANGUAGE_INTERPRETERS)
        raise ValueError(f"Unsupported language {lang!r}. Supported: {supported}")
    if cfg.code:
        # write to a secure temp file
        fd, tmp_path = tempfile.mkstemp(suffix=f".{lang}", prefix="tag_sandbox_")
        try:
            os.write(fd, cfg.code.encode())
        finally:
            os.close(fd)
        os.chmod(tmp_path, 0o600)
        return interpreter + [tmp_path], tmp_path  # caller must unlink tmp_path
    if cfg.file:
        return interpreter + [cfg.file], None
    raise ValueError("One of --code, --file, or a command must be provided")


def _stream_unix(
    proc: subprocess.Popen,
    run_id: str,
    *,
    chunk_timeout: int,
    total_timeout: int,
    start_mono: float,
) -> "Generator[OutputChunk, None, StreamRunResult]":
    """
    Unix implementation: select()-based multiplexed reader.
    Yields OutputChunk instances; returns StreamRunResult.
    """
    seq = 0
    stdout_buf = b""
    stderr_buf = b""
    fds = {proc.stdout.fileno(): ("stdout", stdout_buf),
           proc.stderr.fileno(): ("stderr", stderr_buf)}
    open_fds = set(fds.keys())
    start_wall = time.time()
    last_chunk_mono = start_mono

    def _make_chunk(stream: str, line: bytes) -> OutputChunk:
        nonlocal seq
        seq += 1
        return OutputChunk(
            run_id=run_id,
            seq=seq,
            stream=stream,
            text=line.decode("utf-8", errors="replace"),
            offset_ms=int((time.monotonic() - start_mono) * 1000),
            received_at=_utc_now(),
        )

    timed_out_reason: TimeoutReason | None = None

    while open_fds:
        elapsed = time.monotonic() - start_mono
        if elapsed >= total_timeout:
            proc.kill()
            timed_out_reason = "total_timeout"
            break

        remaining_total = total_timeout - elapsed
        wait_secs = min(chunk_timeout, remaining_total)

        try:
            readable, _, _ = select.select(list(open_fds), [], [], wait_secs)
        except (ValueError, OSError):
            break

        if not readable:
            # select timed out → chunk_timeout silence exceeded
            proc.kill()
            timed_out_reason = "chunk_silence"
            break

        last_chunk_mono = time.monotonic()

        for fd in readable:
            stream_name = fds[fd][0]
            data = os.read(fd, 4096)
            if not data:
                open_fds.discard(fd)
                continue
            # accumulate in the appropriate buffer
            if stream_name == "stdout":
                stdout_buf += data
                while b"\n" in stdout_buf or len(stdout_buf) >= MAX_LINE_BYTES:
                    idx = stdout_buf.find(b"\n")
                    if idx == -1:
                        idx = MAX_LINE_BYTES - 1
                    line, stdout_buf = stdout_buf[:idx + 1], stdout_buf[idx + 1:]
                    chunk = _make_chunk("stdout", line)
                    yield chunk
            else:
                stderr_buf += data
                while b"\n" in stderr_buf or len(stderr_buf) >= MAX_LINE_BYTES:
                    idx = stderr_buf.find(b"\n")
                    if idx == -1:
                        idx = MAX_LINE_BYTES - 1
                    line, stderr_buf = stderr_buf[:idx + 1], stderr_buf[idx + 1:]
                    chunk = _make_chunk("stderr", line)
                    yield chunk

    # drain remaining buffer contents after loop
    for buf, stream_name in [(stdout_buf, "stdout"), (stderr_buf, "stderr")]:
        if buf:
            yield _make_chunk(stream_name, buf)

    proc.wait(timeout=5)
    exit_code = proc.returncode
    duration_ms = int((time.monotonic() - start_mono) * 1000)

    return StreamRunResult(
        run_id=run_id,
        exit_code=exit_code,
        status=("timeout" if timed_out_reason
                else "done" if exit_code == 0
                else "failed"),
        timed_out_reason=timed_out_reason,
        duration_ms=duration_ms,
        chunk_count=seq,
        stdout_chunks=sum(1 for _ in range(seq)),  # tracked by caller
        stderr_chunks=0,
    )
```

#### 9.4.1 Windows thread-per-stream fallback

On Windows, `select.select()` does not work with subprocess pipes. A `threading.Thread` per stream feeds into a `queue.Queue`, and the main loop drains the queue with a timeout:

```python
def _stream_windows(proc, run_id, *, chunk_timeout, total_timeout, start_mono):
    """Thread-per-stream fallback for Windows (no select on pipes)."""
    chunk_q: _queue.Queue = _queue.Queue()

    def _reader(stream_obj, stream_name: str):
        try:
            for raw_line in iter(stream_obj.readline, b""):
                chunk_q.put((stream_name, raw_line))
        finally:
            chunk_q.put((stream_name, None))  # sentinel

    threads = [
        threading.Thread(target=_reader, args=(proc.stdout, "stdout"), daemon=True),
        threading.Thread(target=_reader, args=(proc.stderr, "stderr"), daemon=True),
    ]
    for t in threads:
        t.start()

    seq = 0
    closed = 0
    timed_out_reason = None
    start = time.monotonic()

    while closed < 2:
        elapsed = time.monotonic() - start
        if elapsed >= total_timeout:
            proc.kill(); timed_out_reason = "total_timeout"; break
        try:
            stream_name, data = chunk_q.get(timeout=min(chunk_timeout, total_timeout - elapsed))
        except _queue.Empty:
            proc.kill(); timed_out_reason = "chunk_silence"; break
        if data is None:
            closed += 1; continue
        seq += 1
        yield OutputChunk(
            run_id=run_id, seq=seq, stream=stream_name,
            text=data.decode("utf-8", errors="replace"),
            offset_ms=int((time.monotonic() - start) * 1000),
            received_at=_utc_now(),
        )

    proc.wait(timeout=5)
    # ... return StreamRunResult
```

### 9.5 SQLite Chunk Persistence (Batched Writer)

To avoid per-chunk SQLite writes at high throughput, chunks are collected in a `collections.deque` and flushed in batches:

```python
def _flush_chunks(conn: sqlite3.Connection, batch: list[OutputChunk]) -> None:
    """Batch-insert OutputChunk rows into sandbox_run_chunks."""
    conn.executemany(
        """INSERT OR IGNORE INTO sandbox_run_chunks
             (run_id, seq, stream, text, offset_ms, received_at)
           VALUES (?, ?, ?, ?, ?, ?)""",
        [(c.run_id, c.seq, c.stream, c.text, c.offset_ms, c.received_at)
         for c in batch],
    )
    if batch:
        conn.execute(
            """UPDATE sandbox_runs
               SET chunk_count = chunk_count + ?,
                   last_chunk_at = ?
               WHERE id = ?""",
            (len(batch), batch[-1].received_at, batch[0].run_id),
        )
    conn.commit()


BATCH_SIZE = 10
BATCH_INTERVAL_S = 0.5


def run_in_sandbox_streaming(
    conn: sqlite3.Connection,
    cfg: StreamConfig,
) -> "Generator[OutputChunk, None, StreamRunResult]":
    """
    Public streaming entry point. Yields OutputChunk; caller iterates.
    Persists to SQLite in batches. Returns StreamRunResult via StopIteration.value.
    """
    ensure_schema(conn)
    run_id = uuid.uuid4().hex[:12]
    now = _utc_now()

    # Resolve command
    tmp_path = None
    if cfg.code or cfg.file:
        resolved = _resolve_command(cfg)
        if isinstance(resolved[0], list):
            cmd, tmp_path = resolved
        else:
            cmd = resolved
    else:
        cmd = cfg.command

    conn.execute(
        """INSERT INTO sandbox_runs
             (id, command, backend, image, status, created_at, streamed, language, source_type)
           VALUES (?, ?, ?, ?, 'running', ?, 1, ?, ?)""",
        (run_id,
         " ".join(cmd),
         cfg.backend,
         cfg.image if cfg.backend == "docker" else None,
         now,
         cfg.language,
         "code_string" if cfg.code else "file" if cfg.file else "command"),
    )
    conn.commit()

    # Print run ID immediately (to stderr so it doesn't pollute --json stdout)
    print(f"Run ID: {run_id}", file=sys.stderr)

    env = {"PATH": "/usr/bin:/bin:/usr/local/bin"}
    env.update(cfg.env_overrides)

    proc = subprocess.Popen(
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        cwd=cfg.workdir,
    )

    start_mono = time.monotonic()
    batch: list[OutputChunk] = []
    last_flush = time.monotonic()
    result: StreamRunResult | None = None

    reader_fn = _stream_unix if sys.platform != "win32" else _stream_windows
    gen = reader_fn(
        proc, run_id,
        chunk_timeout=cfg.chunk_timeout,
        total_timeout=cfg.timeout,
        start_mono=start_mono,
    )

    try:
        for chunk in gen:
            batch.append(chunk)
            yield chunk
            now_mono = time.monotonic()
            if len(batch) >= BATCH_SIZE or (now_mono - last_flush) >= BATCH_INTERVAL_S:
                _flush_chunks(conn, batch)
                batch.clear()
                last_flush = now_mono
    except GeneratorExit:
        proc.kill()
        conn.execute(
            "UPDATE sandbox_runs SET status='interrupted', completed_at=? WHERE id=?",
            (_utc_now(), run_id),
        )
        conn.commit()
        return
    finally:
        if tmp_path:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass

    # flush remaining batch
    if batch:
        _flush_chunks(conn, batch)
        batch.clear()

    result = gen.value if hasattr(gen, "value") else StreamRunResult(
        run_id=run_id, exit_code=proc.returncode,
        status="done" if proc.returncode == 0 else "failed",
        timed_out_reason=None, duration_ms=0,
        chunk_count=0, stdout_chunks=0, stderr_chunks=0,
    )

    conn.execute(
        """UPDATE sandbox_runs
           SET status=?, exit_code=?, completed_at=?, timed_out_reason=?
           WHERE id=?""",
        (result.status, result.exit_code, _utc_now(), result.timed_out_reason, run_id),
    )
    conn.commit()
    return result
```

### 9.6 `tag sandbox logs` — Replay and Follow

```python
def stream_sandbox_logs(
    conn: sqlite3.Connection,
    run_id: str,
    *,
    follow: bool = False,
    since_seq: int = 0,
    stream_filter: str = "both",   # 'stdout' | 'stderr' | 'both'
    poll_interval_s: float = 0.2,
) -> "Generator[OutputChunk, None, None]":
    """
    Replay or follow chunks for a given run_id.
    In follow mode, polls for new rows until the run is no longer 'running'.
    """
    last_seq = since_seq
    stream_clause = "" if stream_filter == "both" else f" AND stream = '{stream_filter}'"

    while True:
        rows = conn.execute(
            f"""SELECT run_id, seq, stream, text, offset_ms, received_at
                  FROM sandbox_run_chunks
                 WHERE run_id = ? AND seq > ?{stream_clause}
              ORDER BY seq ASC""",
            (run_id, last_seq),
        ).fetchall()

        for row in rows:
            last_seq = row[1]
            yield OutputChunk(
                run_id=row[0], seq=row[1], stream=row[2],
                text=row[3], offset_ms=row[4], received_at=row[5],
            )

        if not follow:
            break

        # check if run is still active
        run_row = conn.execute(
            "SELECT status FROM sandbox_runs WHERE id = ?", (run_id,)
        ).fetchone()
        if not run_row or run_row[0] != "running":
            # final drain: one more query to catch any chunks written in the last poll window
            rows = conn.execute(
                f"""SELECT run_id, seq, stream, text, offset_ms, received_at
                      FROM sandbox_run_chunks
                     WHERE run_id = ? AND seq > ?{stream_clause}
                  ORDER BY seq ASC""",
                (run_id, last_seq),
            ).fetchall()
            for row in rows:
                yield OutputChunk(
                    run_id=row[0], seq=row[1], stream=row[2],
                    text=row[3], offset_ms=row[4], received_at=row[5],
                )
            break

        time.sleep(poll_interval_s)
```

### 9.7 TUI Display (`tui_output.py` — `SandboxStreamPanel`)

```python
# src/tag/tui_output.py (additions)
from rich.console import Console
from rich.live import Live
from rich.table import Table
from rich.text import Text

_STREAM_COLORS = {"stdout": "default", "stderr": "yellow"}

class SandboxStreamPanel:
    """Rich Live panel for real-time sandbox output display."""

    def __init__(self, run_id: str, *, no_color: bool = False, max_lines: int = 500):
        self.run_id = run_id
        self.no_color = no_color
        self.max_lines = max_lines
        self._console = Console(no_color=no_color)
        self._lines: list[tuple[str, str, int]] = []  # (stream, text, offset_ms)

    def __enter__(self):
        self._console.print(f"Run ID: [bold cyan]{self.run_id}[/]", highlight=False)
        self._console.rule()
        return self

    def add_chunk(self, chunk: "OutputChunk") -> None:
        label = "OUT" if chunk.stream == "stdout" else "ERR"
        color = _STREAM_COLORS[chunk.stream]
        ts = f"{chunk.offset_ms / 1000:>7.3f}s"
        line = chunk.text.rstrip("\n")
        if self.no_color or not sys.stdout.isatty():
            self._console.print(f"  {ts}  {label}  {line}")
        else:
            self._console.print(
                Text.assemble(
                    (f"  {ts}  ", "dim"),
                    (f"{label}  ", f"bold {color}"),
                    (line, color),
                )
            )

    def __exit__(self, *args):
        self._console.rule()
```

### 9.8 Integration Points

| Integration | Mechanism |
|-------------|-----------|
| `controller.py: cmd_sandbox_run` | Parses `--stream`, `--code`, `--file`, `--language`, `--chunk-timeout`, `--json` flags; calls `run_in_sandbox_streaming()` if streaming; delegates to existing `run_in_sandbox()` otherwise |
| `controller.py: cmd_sandbox_logs` | New subcommand; calls `stream_sandbox_logs()` with `--follow`, `--since`, `--stream` args |
| `tracing.py` | Emits `sandbox.stream_open` span at stream start and `sandbox.stream_close` span at end; attributes: `run_id`, `backend`, `chunk_count`, `exit_code`, `duration_ms` |
| `cron_scheduler.py` | Existing TTL sweeper extended to delete `sandbox_run_chunks` rows where `received_at < NOW() - interval(chunk_retention_days)` AND `run_id NOT IN (SELECT id FROM sandbox_runs WHERE status = 'running')` |
| `tui_output.py` | `SandboxStreamPanel` provides Rich Live output; falls back to plain print when stdout is not a TTY |
| `security.py` | Blocked-path pattern checks (PRD-034) called before `Popen`; streaming does not modify the pre-execution security gate |

---

## 10. Security Considerations

1. **Temp file permissions:** Code strings passed via `--code` are written to `tempfile.mkstemp()` with mode `0o600`. The temp file is deleted in a `finally` block even if the process is killed. On systems with a shared `/tmp`, this prevents other users from reading the code before execution.

2. **Pre-execution security gate:** All existing PRD-028 blocked-path and command-allowlist checks execute synchronously before `Popen` is called. Streaming does not short-circuit or bypass these checks. The security gate is a hard pre-condition.

3. **Environment variable isolation:** The `env` dict passed to `Popen` contains only `PATH` plus explicit `--env` overrides. Host secrets (`AWS_SECRET_ACCESS_KEY`, `OPENAI_API_KEY`, etc.) present in the parent process environment are not inherited, matching the behavior of the existing restricted backend.

4. **Process group kill on timeout:** `proc.kill()` sends `SIGKILL` to the process. On Unix, if the sandboxed process has spawned child processes (e.g., via `subprocess` inside a Python script), those children may escape the kill. For the `docker` backend this is a non-issue (container cleanup kills all descendants). For `restricted` backend, the process should be launched via `os.setsid()` and killed with `os.killpg(os.getpgid(proc.pid), signal.SIGKILL)` to kill the entire process group.

5. **Chunk text sanitization for TUI:** Raw output from the child process may contain ANSI escape sequences, including cursor-movement and terminal-reset codes that could corrupt the Rich Live display. When outputting to a TTY, strip ANSI control sequences from chunk text before passing to Rich markup rendering. This prevents a malicious script from, e.g., overwriting terminal history.

6. **JSONL injection:** In `--json` mode, chunk `text` is embedded as a JSON string value. Python's `json.dumps()` is used unconditionally to serialize the text field — never f-string interpolation — preventing control-character injection in the JSON stream.

7. **Run ID entropy:** Run IDs are `uuid.uuid4().hex[:12]` (48 bits of entropy). This is sufficient for local isolation but not suitable as a secret token. `tag sandbox logs` requires the exact run ID; there is no wildcard query path that would allow enumeration.

8. **Chunk retention:** Chunks may contain sensitive output (API responses, partial secrets printed by scripts). The 7-day default TTL limits exposure. Users can set `sandbox.chunk_retention_days = 1` or `0` (delete on run completion) in config.

9. **Signal forwarding:** When the user sends `SIGINT` (Ctrl+C) to TAG, the `SIGINT` handler must kill the child process before exiting. Without this, the child process may continue running as an orphan. Implement via `signal.signal(signal.SIGINT, lambda s, f: (proc.kill(), sys.exit(130)))` registered before `Popen`.

10. **Docker backend streaming:** For the `docker` backend, streaming is achieved by running without `-d` (detached) and reading from the `docker run` subprocess's own stdout/stderr pipes, which Docker multiplexes from the container's streams. This is identical to the current approach and does not require the Docker socket API. The security properties of the existing Docker backend (network isolation, memory cap, CPU cap) are unchanged.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_sandbox_streaming.py`)

| Test | Description |
|------|-------------|
| `test_stream_basic_output` | Script prints 5 lines; assert 5 `OutputChunk` objects yielded with correct `text`, `seq` monotonic, `stream='stdout'` |
| `test_stream_stderr_separation` | Script writes to both `sys.stdout` and `sys.stderr`; assert chunks with `stream='stderr'` for stderr lines and `stream='stdout'` for stdout lines |
| `test_chunk_timeout_kills_process` | Script sleeps 60 s; set `chunk_timeout=2`; assert `StreamRunResult.timed_out_reason == 'chunk_silence'` within 3 s |
| `test_total_timeout_kills_process` | Script loops forever printing; set `total_timeout=2`; assert `StreamRunResult.timed_out_reason == 'total_timeout'` |
| `test_chunks_persisted_to_db` | After streaming, query `sandbox_run_chunks`; assert count == number of chunks yielded |
| `test_chunks_ordered_by_seq` | Assert `SELECT seq FROM sandbox_run_chunks WHERE run_id=? ORDER BY seq` has no gaps and starts at 1 |
| `test_nonstreaming_path_unchanged` | Call `run_in_sandbox()` (no stream); assert `subprocess.run` is used, not `Popen`; assert output in `sandbox_runs.output` as before |
| `test_code_tempfile_deleted` | After `--code` run (success and failure), assert temp file does not exist |
| `test_language_inference_from_extension` | `.py` → `python`, `.sh` → `bash`, `.js` → `node`, `.rb` → `ruby`; `.cpp` → `ValueError` with supported-list message |
| `test_max_line_bytes_boundary` | Script writes a 70,000-byte line without newline; assert two chunks are produced (first at 65536 bytes) |
| `test_sigint_sets_interrupted_status` | Simulate `GeneratorExit` mid-stream; assert `sandbox_runs.status == 'interrupted'` |
| `test_schema_migration_idempotent` | Call `ensure_schema()` twice on a fresh DB; assert no error and all columns present |
| `test_schema_migration_existing_db` | Populate a DB with pre-PRD schema (no streaming columns); call `ensure_schema()`; assert new columns added without data loss |
| `test_batch_flush_at_10_chunks` | Intercept `_flush_chunks`; emit 25 chunks; assert `_flush_chunks` called 3 times (at 10, 20, and final drain) |
| `test_process_group_kill` | Script spawns a child subprocess that sleeps; apply chunk_timeout; assert child subprocess also killed (via `ps` check on pid) |

### 11.2 Integration Tests (`tests/test_sandbox_streaming_integration.py`)

| Test | Description |
|------|-------------|
| `test_cli_stream_flag` | `subprocess.run(["tag", "sandbox", "run", "--code", "print(1)", "--language", "python", "--stream"])` exits 0, stdout contains "1" |
| `test_cli_file_flag` | Write a temp `.py` file; run with `--file`; assert output matches |
| `test_cli_json_flag` | With `--json`, assert stdout is valid JSONL with `event` field on each line |
| `test_sandbox_logs_replay` | Run a script, capture run_id, then `tag sandbox logs <run-id>`; assert identical output |
| `test_sandbox_logs_follow` | Start a long-running script in a thread, attach `--follow` in main thread, assert lines arrive in order before run completes |
| `test_sandbox_logs_since` | Run 10-line script, then `tag sandbox logs <id> --since 5`; assert only seqs 6-10 returned |
| `test_config_stream_by_default` | Set `sandbox.stream_by_default=true` in config; run without `--stream`; assert streaming behavior |

### 11.3 Performance Tests (`tests/test_sandbox_streaming_perf.py`)

| Test | Target |
|------|--------|
| `test_throughput_10k_lines` | Script with `for i in range(10000): print(i)` completes with all 10,000 chunks in < 5 s |
| `test_ttfc_under_200ms` | Measure `perf_counter()` from `Popen()` to first chunk yield; assert < 200 ms |
| `test_memory_overhead_high_volume` | Run 100,000-line script; measure `resource.getrusage().ru_maxrss` delta; assert < 32 MB increase |
| `test_db_write_latency` | Time `_flush_chunks()` with 10-chunk batch on a WAL-mode DB; assert < 50 ms |
| `test_concurrent_runs` | Launch 5 simultaneous streaming runs; assert no chunk cross-contamination (each run's chunks reference only its run_id) |

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag sandbox run --code "for i in range(10): print(i)" --language python --stream` prints each number on a separate line as it is produced, not all at once after the loop. | Manual test: observe incremental output with `time.sleep(0.1)` between prints |
| AC-02 | Each printed line is prefixed with a relative timestamp (format: `N.NNNs`) and a stream label (`OUT` or `ERR`). | Unit test: assert prefix format via regex `r"^\s+\d+\.\d{3}s\s+(OUT|ERR)\s+"` |
| AC-03 | The run ID is printed to stderr before the first chunk line appears on stdout. | Integration test: capture stderr and stdout separately; assert run_id line in stderr precedes first stdout line |
| AC-04 | After a streaming run completes, `SELECT COUNT(*) FROM sandbox_run_chunks WHERE run_id=?` returns the exact number of lines the script printed. | Integration test with a script that prints exactly 47 lines |
| AC-05 | `tag sandbox logs <run-id>` replays all chunks in sequence order with identical relative timestamps. | Integration test: compare streamed output vs. `logs` output |
| AC-06 | `tag sandbox logs <run-id> --follow` begins printing new lines within 500 ms of them being written to `sandbox_run_chunks`. | Integration test: two-thread test measuring latency |
| AC-07 | A script that produces no output for 35 seconds is killed within 500 ms of the 30-second chunk-timeout and the run record shows `status='timeout'` and `timed_out_reason='chunk_silence'`. | Integration test with `time.sleep(35)` script and `--chunk-timeout 30` |
| AC-08 | A script running for more than `--timeout 10` seconds is killed and the run record shows `status='timeout'` and `timed_out_reason='total_timeout'`. | Integration test with `while True: time.sleep(1); print("x")` and `--timeout 10` |
| AC-09 | `tag sandbox run --code "..." --stream` without `--language` defaults to Python. | Unit test: assert interpreter list starts with `python3` |
| AC-10 | `tag sandbox run --file script.sh --stream` infers `bash` from the `.sh` extension. | Unit test: assert interpreter `bash` selected |
| AC-11 | `tag sandbox run --file script.cpp --stream` exits with a non-zero code and an error message listing supported languages. | Integration test: assert exit code ≠ 0 and message contains "Supported:" |
| AC-12 | In `--json` mode, every event is valid JSON parseable by `json.loads()`. | Integration test: parse all stdout lines |
| AC-13 | The temp file created for `--code` input is deleted after the run, even when the process is killed by timeout. | Integration test: check `os.path.exists(tmp_path)` after run; assert False |
| AC-14 | Running `tag sandbox run` (no `--stream`) produces identical output format and exit code as before this PRD was implemented. | Regression test comparing pre/post behavior using subprocess output capture |
| AC-15 | `sandbox_runs` rows created before this PRD (without the new columns) can be read by `get_sandbox_run()` without error after schema migration. | Schema migration test with pre-seeded fixture |
| AC-16 | Pressing Ctrl+C during `--stream` kills the child process and sets `sandbox_runs.status='interrupted'`. | Manual test confirmed by checking DB after interrupt |
| AC-17 | Lines exceeding 65,536 bytes without a newline are split into multiple chunks of ≤ 65,536 bytes each. | Unit test with a 70,000-byte line |
| AC-18 | `tag config set sandbox.stream_by_default true` causes subsequent `tag sandbox run` invocations to stream without `--stream` flag. | Integration test: set config, run without flag, assert streaming behavior |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-028 (Sandbox Code Execution) | Hard prerequisite | `sandbox_runs` table, `ensure_schema()`, `run_in_sandbox()`, backend dispatch logic; must be merged and deployed first |
| PRD-013 (Agent Tracing) | Soft prerequisite | `tracing.py` span emission for `sandbox.stream_open/close`; gracefully no-ops if tracing is disabled |
| PRD-003 (Rich Streaming TUI) | Soft prerequisite | `tui_output.py` Rich patterns; feature degrades to plain `print()` if Rich is unavailable |
| PRD-034 (Secret Scanning) | Soft prerequisite | Blocked-path patterns from `security.py` must run before `Popen`; feature degrades gracefully if `security.py` is not yet merged |
| `subprocess` (stdlib) | None | Python stdlib; no install required |
| `select` (stdlib) | None | Unix only; Windows fallback uses `threading` + `queue` (both stdlib) |
| `tempfile` (stdlib) | None | For `--code` temp file creation |
| `rich` (existing TAG dep) | None | Already in `pyproject.toml` |
| `uuid` (stdlib) | None | Already used in sandbox.py |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should `--stream` be the default for all new `tag sandbox run` invocations eventually, with `--no-stream` as the opt-out? Or should non-streaming remain the permanent default? The current design makes `--stream` opt-in. | Product | Before v1.0 GA of streaming feature |
| OQ-2 | Should `tag sandbox logs --follow` use SQLite polling (current design, 200 ms interval) or a SQLite `NOTIFY`-style mechanism? SQLite has no native pub/sub; alternatives include a Unix socket, a shared memory flag, or a file-watch on the WAL file. | Engineering | Can be deferred to a follow-on PRD if polling latency is acceptable |
| OQ-3 | Should chunk text be stored compressed (e.g., zstd) to reduce storage for high-volume runs? A 100,000-line run at average 50 bytes/line is 5 MB raw. zstd at 5:1 would be 1 MB. The tradeoff is read complexity and CPU overhead during writes. | Engineering | Evaluate at scale; defer to follow-on if storage is not a user complaint |
| OQ-4 | Should E2B and Modal backends be integrated with the streaming interface in this PRD or in a follow-on? E2B's `on_stdout`/`on_stderr` callbacks and Modal's async iterables can both be adapted to yield `OutputChunk` instances. The implementation is straightforward but requires E2B/Modal SDK dependencies and credentials in CI. | Engineering | Follow-on PRD targeting E2B and Modal streaming integration specifically |
| OQ-5 | Should `tag sandbox run --stream --json` be the recommended interface for programmatic consumers (e.g., `queue_worker.py`, `loop_agent.py`)? If so, should `queue_worker.py` parse the JSONL stream in a follow-on PRD to provide structured job progress? | Product | Follow-on PRD for queue integration |
| OQ-6 | Should there be a `tag sandbox run --stream --output-file path.log` flag to write the full stream to a file simultaneously with terminal display (tee behavior)? | Product | Low priority; can be achieved with shell tee in the interim |
| OQ-7 | What is the correct behavior for `tag sandbox logs --follow` if the run_id does not exist? Currently returns immediately with no output. Should it poll for the run to appear (useful for race conditions in programmatic use) or error immediately? | Engineering | Error immediately with clear message; polling on non-existent run_id is a footgun |

---

## 15. Complexity and Timeline

**Total estimated effort: 3–5 days (S)**

### Phase 1 — Schema and Core Streaming Engine (Day 1–2)

- Add `sandbox_run_chunks` table DDL to `ensure_schema()` in `sandbox.py`
- Add new columns to `sandbox_runs` using the `_add_column_if_missing()` migration helper
- Implement `OutputChunk`, `StreamRunResult`, `StreamConfig` dataclasses
- Implement `_stream_unix()` generator with `select.select()` multiplexing
- Implement `_stream_windows()` thread-per-stream fallback
- Implement `_flush_chunks()` batched writer with 10-chunk / 500 ms dual threshold
- Implement `run_in_sandbox_streaming()` public entry point
- Unit tests: `test_stream_basic_output`, `test_stream_stderr_separation`, `test_chunk_timeout_kills_process`, `test_total_timeout_kills_process`, `test_chunks_persisted_to_db`, `test_chunks_ordered_by_seq`, `test_nonstreaming_path_unchanged`

### Phase 2 — Code/File Input, Logs Command, TUI (Day 2–3)

- Implement `_resolve_command()` with `--code` temp file and `--file` language inference
- Implement `stream_sandbox_logs()` replay and follow generator
- Add `SandboxStreamPanel` to `tui_output.py`
- Implement `--json` JSONL output mode
- Add `cmd_sandbox_logs` to `controller.py`
- Extend `cmd_sandbox_run` in `controller.py` with all new flags
- Unit tests: `test_language_inference_from_extension`, `test_code_tempfile_deleted`, `test_max_line_bytes_boundary`, `test_sigint_sets_interrupted_status`, `test_batch_flush_at_10_chunks`

### Phase 3 — Integration Tests, Performance, Security Hardening (Day 3–4)

- Integration tests: full CLI invocation tests for all acceptance criteria
- Performance tests: throughput, TTFC, memory overhead, DB write latency, concurrent runs
- Process group kill implementation (`os.setsid()` + `os.killpg()`)
- ANSI strip for TUI output from untrusted child processes
- `signal.SIGINT` handler for clean interrupt
- TTL sweeper extension in `cron_scheduler.py` for chunk retention
- Tracing span emission in `tracing.py` for `sandbox.stream_open/close`

### Phase 4 — Documentation, Config, Final QA (Day 4–5)

- `tag config set sandbox.stream_by_default`, `sandbox.chunk_timeout_seconds`, `sandbox.default_timeout_seconds`, `sandbox.chunk_retention_days`
- Schema migration test with pre-seeded fixture
- Update `docs/prd/INDEX.md` to reference PRD-089
- Final acceptance criteria verification pass
- Merge and tag release

### Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| `select.select()` edge cases on macOS (large fd numbers, closed pipe timing) | Medium | Medium | Test against macOS in CI; use `selectors.DefaultSelector` as a more robust alternative if select() shows issues |
| High-throughput SQLite writes causing WAL file growth | Low | Low | WAL mode + batch writes minimize contention; add checkpoint hint after run completion |
| Orphaned child processes on Windows (process group kill not available) | Medium | Low | Windows fallback uses `proc.kill()` which kills the process but not descendants; document limitation |
| Breaking change if existing code calls `run_in_sandbox()` and expects the old return dict format | Low | High | Streaming path is a new function `run_in_sandbox_streaming()`; existing `run_in_sandbox()` is untouched |

