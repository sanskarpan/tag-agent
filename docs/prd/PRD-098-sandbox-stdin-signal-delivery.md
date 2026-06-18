# PRD-098: Process stdin Streaming and Signal Delivery (SIGTERM/SIGKILL/SIGINT) (`tag sandbox signal / tag sandbox write`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** S (3-5 days)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py`
**Depends on:** PRD-028 (sandbox code execution — core sandbox runtime and `sandbox_runs` table), PRD-013 (agent tracing/observability — span instrumentation patterns), PRD-034 (secret scanning — signal validation and input sanitisation), PRD-003 (rich streaming TUI — interactive terminal output), PRD-005 (execution backend selection — runtime dispatch logic)
**Inspired by:** E2B process control, Docker exec, pty subprocess

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) executes agent-generated code and shell commands inside isolated runtimes — Docker containers, E2B micro-VMs, Modal functions, or a restricted subprocess layer. The current implementation is entirely batch-oriented: `run_in_sandbox()` spawns a process, waits for it to terminate, then returns the complete stdout/stderr buffer. There is no way to interact with a running process once it has started, no way to terminate it cleanly before timeout, and no way to drive interactive programs (REPLs, test harnesses, database CLIs) that expect input over their lifetime.

This limitation blocks several important agent workflows. An AI agent working in a stateful Python REPL needs to send multiple expressions to the same interpreter session, receiving intermediate results between sends, so that variable state accumulates and computations build on each other. An agent orchestrating a long-running build or data-processing command needs to send SIGTERM for graceful shutdown — giving the process a chance to flush buffers and clean up — rather than waiting for an arbitrary timeout to trigger a hard kill. Integration tests driven by agent-generated test suites frequently involve programs that read from stdin to advance through setup wizards or interactive prompts.

This PRD specifies two new `tag sandbox` subcommands — `tag sandbox write` and `tag sandbox signal` — plus an `--interactive` / `--pty` mode on `tag sandbox run`. Together they implement the complete POSIX process interaction model inside TAG's sandbox layer: stdin streaming over a named FIFO or PTY master file descriptor, POSIX signal delivery via `os.kill()` against the tracked process PID, and optional PTY allocation via Python's `pty.openpty()` so that programs behave as they would in a real terminal (cursor addressing, raw keystrokes, readline support, `isatty()` returning `True`).

The design draws from three proven reference implementations: E2B's process control API (which exposes `process.send_stdin()` and `process.kill()` on its `Process` handle), Docker's `docker exec` protocol (which multiplexes stdin/stdout/stderr over a single connection with a well-defined stream header framing), and the terminado/pyxtermjs PTY-WebSocket bridge pattern (which connects a PTY master fd to an asyncio event loop reader and routes messages via a JSON array protocol). TAG's implementation is simpler than all three because it targets a single-user local CLI rather than a multi-tenant web service, but the process model and signal routing follow the same POSIX primitives.

The feature is intentionally narrow in scope. It extends `sandbox.py` with a long-lived process table, a FIFO-based stdin channel, signal delivery, and PTY support. It does not implement WebSocket-based terminal streaming, multi-user session sharing, or terminal recording (all of which are follow-on features). The surface is small — roughly 350 additional lines in `sandbox.py`, two new CLI subcommands, and three new SQLite columns — but the impact on interactive agent workflows is significant.

---

## 2. Problem Statement

### 2.1 Batch-only execution prevents stateful agent interactions

`run_in_sandbox()` in `sandbox.py` calls `subprocess.run()` (for the restricted backend) or `docker run` (for the Docker backend), both of which wait for process termination before returning. There is no `subprocess.Popen` handle stored anywhere, no PID recorded in SQLite, and no way to send additional input to a process once it is running.

This forces agents that need stateful Python evaluation to use workarounds: write a monolithic script to a tempfile and run it as a single batch invocation, or serialise all state to disk between invocations. Both workarounds are fragile. A monolithic script cannot show intermediate results between statements. Disk-based serialisation fails for objects that cannot be pickled, breaks REPL-style exploration, and doubles the I/O cost of every interaction.

The root cause is architectural: the current sandbox layer was designed as a one-shot code runner, not as a process host. Fixing this requires introducing the concept of a *long-lived sandbox process* — a process that is created once, assigned a stable ID, tracked in SQLite, and then interacted with via subsequent commands.

### 2.2 No graceful shutdown path

When a sandbox run exceeds its timeout, the current code lets `subprocess.TimeoutExpired` bubble and records `status='failed'`. No signal is sent; the subprocess is simply abandoned. Processes that registered signal handlers — web servers, database engines, compilers with incremental cache writes — never get a chance to flush state or release locks.

For Docker-backend runs, `docker run` has `--stop-timeout` set to the timeout value, but the outer `subprocess.run(..., timeout=timeout+30)` call means TAG kills the docker process 30 seconds after the container timeout. This produces orphaned container IDs in `docker ps` output and leaves named volumes in inconsistent states when the container was mounting one.

POSIX defines a two-phase shutdown sequence — SIGTERM (please exit) → wait → SIGKILL (exit now) — which is the correct pattern. TAG needs to be able to send either signal on demand, not only on timeout.

### 2.3 Interactive programs are unusable

Programs that use `isatty(0)` to decide whether to show a prompt — Python's interactive interpreter, bash, psql, sqlite3 CLI, Node.js REPL — all detect that stdin is a pipe and switch to non-interactive mode. In non-interactive mode, Python suppresses the `>>>` prompt and changes its readline behaviour. The sqlite3 CLI omits the `sqlite>` prompt. These programs are technically usable as batch processors, but they are not the interactive REPLs that agents need for exploratory computation.

PTY allocation solves this. When a process runs under a PTY, `isatty(0)` returns `True`, the slave side of the PTY carries the standard terminal semantics (ECHO, ICANON, SIGINT on Ctrl-C, SIGTSTP on Ctrl-Z), and programs behave exactly as they do in a real terminal. Without PTY support, agent-driven interactive workflows that depend on REPL prompts, readline completion, or raw-mode terminal input are impossible in TAG's sandbox.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Introduce long-lived sandbox processes: `tag sandbox run --interactive` spawns a process, records its PID in SQLite, and returns a `<process-id>` the user can reference in subsequent commands. |
| G2 | `tag sandbox write <id> <text>` sends `text` to the running process's stdin. The process must be in `running` state. Multiple `write` calls to the same process accumulate. |
| G3 | `tag sandbox signal <id> SIGTERM` and `tag sandbox signal <id> SIGKILL` deliver the named signal to the process via `os.kill(pid, signal_number)`. SIGINT is also supported. |
| G4 | `--pty` flag on `tag sandbox run --interactive` allocates a PTY pair via `pty.openpty()` so that `isatty(stdin_fd)` returns `True` inside the process and terminal-aware programs behave interactively. |
| G5 | All process state transitions (created → running → stopped/killed) are recorded in the existing `sandbox_runs` table with two new columns: `pid` and `stdin_fifo`. |
| G6 | Signal validation rejects unknown signal names with a clear error before any `os.kill()` call is attempted. Only SIGTERM, SIGKILL, SIGINT, SIGHUP, SIGUSR1, SIGUSR2 are allowed. |
| G7 | `tag sandbox run --interactive` streams stdout in real time to the terminal for the duration of the process, using an asyncio reader loop on the process stdout fd. |
| G8 | The restricted backend and Docker backend both support interactive mode. For Docker, `docker exec -i` is used to attach stdin; PTY support uses `docker exec -it`. |
| G9 | `tag sandbox ps` lists all currently-running interactive processes with PID, start time, and command. |
| G10 | Every signal delivery and stdin write is appended to `~/.tag/runtime/sandbox-audit.jsonl` following the existing PRD-028 audit log format. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | WebSocket-based terminal streaming. The PTY master fd is connected to the local terminal, not to a WebSocket. Remote browser-based terminal access is a separate feature. |
| NG2 | Terminal recording (asciinema / ttyrec format). Capturing a full terminal session replay is deferred. |
| NG3 | Multi-user session sharing. One interactive process is owned by one TAG invocation. No session handoff or attach-from-another-terminal. |
| NG4 | E2B and Modal backends for interactive mode. PTY support is implemented for the `restricted` and `docker` backends only. E2B's `process.send_stdin()` API could be wired in a follow-on PRD. |
| NG5 | Window resize (SIGWINCH) propagation. PTY window size is set once at spawn time via `fcntl.ioctl(master_fd, termios.TIOCSWINSZ, ...)` and not tracked thereafter. |
| NG6 | stdin streaming over the network. `tag sandbox write` only works on the local machine where the process is running. |
| NG7 | Signal delivery to Docker containers via the Docker API. Signal delivery is via `os.kill(pid)` to the host-side process (the docker process wrapper). Killing the Docker-internal PID 1 requires `docker kill --signal`, which is left for a follow-on. |
| NG8 | Process groups / job control (SIGSTOP, SIGCONT, SIGTSTP). Only termination-class signals are in scope for this PRD. |

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Interactive REPL round-trip latency | Median < 100 ms from `tag sandbox write` call to stdout line appearing on terminal | Timing test against `python3 -c "import sys; [print(eval(line)) for line in sys.stdin]"` |
| Signal delivery time | SIGTERM delivered and process exits within 500 ms of `tag sandbox signal <id> SIGTERM` | Integration test: `sleep 3600` process; measure time from signal command to `status='stopped'` in SQLite |
| PTY isatty detection | `python3 -c "import sys; print(sys.stdin.isatty())"` prints `True` when run with `--pty` | Unit test / integration test |
| Audit log completeness | 100% of signal deliveries and stdin writes appear in `sandbox-audit.jsonl` | Checked in integration test by counting JSONL lines |
| Backend coverage | Both `restricted` and `docker` backends pass the interactive mode integration test suite | CI matrix job |
| Signal rejection rate | All signal names not in the allowlist rejected with exit code 1 before `os.kill()` | Unit test parameterised over invalid names |
| Orphan prevention | No orphaned processes after `tag sandbox signal <id> SIGKILL` | `ps` check in integration test teardown |
| ps listing freshness | `tag sandbox ps` shows only processes where `status='running'` in SQLite | Unit test with synthetic rows |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Agent developer | run `tag sandbox run --interactive --code "python3" --pty` | I get a live Python REPL inside the sandbox where my agent can evaluate expressions one at a time and observe intermediate output |
| U2 | Agent developer | run `tag sandbox write <id> "x = 42\n"` followed by `tag sandbox write <id> "print(x)\n"` | My agent can carry state across multiple turns of a REPL without re-running a full script each time |
| U3 | Agent developer | run `tag sandbox signal <id> SIGTERM` when an agent-generated long-running process needs to be stopped cleanly | The process gets a chance to flush its write buffer and remove temporary files before exiting, rather than being hard-killed |
| U4 | Platform engineer | run `tag sandbox signal <id> SIGKILL` if SIGTERM does not stop the process within 10 seconds | I can guarantee the process terminates even if it ignores SIGTERM or is blocked in an uninterruptible syscall |
| U5 | Agent developer | run `tag sandbox run --interactive --code "sqlite3 mydb.db" --pty` | The sqlite3 CLI shows its `sqlite>` prompt and I can send SQL statements interactively |
| U6 | Developer | run `tag sandbox ps` | I can see all currently running interactive sandbox processes with their IDs and start times |
| U7 | Developer | have `tag sandbox signal <id> BADNAME` rejected with a clear error | I do not accidentally send an unexpected signal to the process due to a typo |
| U8 | Operator | see every `tag sandbox write` and `tag sandbox signal` call in `sandbox-audit.jsonl` | I have a complete audit trail of all interactions with sandbox processes, for security review and post-incident forensics |
| U9 | Agent developer | use `--backend docker --pty` | My agent can interact with programs inside a Docker container that require a terminal, such as apt-get or a build system's interactive configuration step |
| U10 | Developer | run `tag sandbox run --interactive --code "bash" --backend docker` | I can get an interactive bash shell inside a Docker sandbox container and send shell commands from my agent |

---

## 7. Proposed CLI Surface

### 7.1 `tag sandbox run` (extended)

Extended with `--interactive` and `--pty` flags. When `--interactive` is absent, behaviour is identical to the current batch mode (PRD-028).

```
tag sandbox run \
  [--backend restricted|docker|modal|e2b]  \
  [--image <docker-image>]                 \
  [--timeout <seconds>]                    \
  [--interactive]                          \
  [--pty]                                  \
  [--pty-rows <N>]                         \
  [--pty-cols <N>]                         \
  [--code <command-string>]                \
  [--workdir <path>]                       \
  [--json]                                 \
  [-- <command> [<args>...]]
```

**New flags:**

- `--interactive` — spawn the process as a long-lived interactive process. Returns `<process-id>` instead of waiting for termination. Implies stdout is streamed live.
- `--pty` — allocate a PTY pair (`pty.openpty()`). Requires `--interactive`. The slave fd is passed as stdin/stdout/stderr of the child process; the master fd is read by TAG and forwarded to the terminal. Only valid for `restricted` and `docker` backends.
- `--pty-rows` — PTY rows (default: current terminal height or 24).
- `--pty-cols` — PTY columns (default: current terminal width or 80).
- `--code` — shorthand for `-- python3 -c <code>` when used without positional args; when combined with `--interactive`, passes the command string as the shell command to run (e.g., `--code "python3"` runs a Python REPL).

**Interactive mode output:**

```
$ tag sandbox run --interactive --code "python3" --pty
[sandbox] Started interactive process
  Process ID : sp_a3f8c2b1
  PID        : 48291
  Backend    : restricted
  PTY        : yes (80x24)
  Started    : 2026-06-17T10:42:00Z

Python 3.12.3 (main, Apr  9 2024, 08:09:14) [GCC 13.2.0] on linux
Type "help", "copyright", "credits" or "license" for more information.
>>>
```

The process continues running. The user returns to the shell prompt. The process's stdout continues streaming to a background thread that forwards output to the terminal.

**JSON output (`--json`):**

```json
{
  "process_id": "sp_a3f8c2b1",
  "pid": 48291,
  "backend": "restricted",
  "pty": true,
  "pty_rows": 24,
  "pty_cols": 80,
  "started_at": "2026-06-17T10:42:00.123456Z",
  "status": "running"
}
```

---

### 7.2 `tag sandbox write`

Send text to the stdin of a running interactive sandbox process.

```
tag sandbox write <process-id> <text>
tag sandbox write <process-id> --file <path>
tag sandbox write <process-id> --hex <hex-bytes>
```

**Arguments:**

- `<process-id>` — the process ID returned by `tag sandbox run --interactive` (prefix `sp_`).
- `<text>` — literal text to write. C-style escape sequences are interpreted: `\n` → newline, `\t` → tab, `\r` → carriage return, `\x1b` → ESC. Use `--raw` to disable escape interpretation.
- `--file <path>` — read text from file and write it to stdin. Mutually exclusive with positional `<text>`.
- `--hex <hex-bytes>` — write raw bytes specified as hex string (e.g., `03` for ETX/Ctrl-C). Mutually exclusive with positional `<text>`.
- `--raw` — do not interpret C-style escape sequences in `<text>`.
- `--no-newline` — do not append a trailing `\n` (default: newline is appended).

**Example usage:**

```bash
# Send a Python expression to a running REPL
tag sandbox write sp_a3f8c2b1 "x = [i**2 for i in range(10)]\n"
tag sandbox write sp_a3f8c2b1 "print(sum(x))\n"

# Send EOF (Ctrl-D) to gracefully close a REPL
tag sandbox write sp_a3f8c2b1 --hex 04

# Send a multi-line block from a file
tag sandbox write sp_a3f8c2b1 --file /tmp/setup.py

# Send a raw string without escape interpretation
tag sandbox write sp_a3f8c2b1 --raw "print('hello\\nworld')\n"
```

**Normal output:**

```
[sandbox:write] sp_a3f8c2b1 ← 32 bytes written
```

**Error output (process not running):**

```
error: process sp_a3f8c2b1 is not in running state (status: stopped)
```

**JSON output (`--json`):**

```json
{
  "process_id": "sp_a3f8c2b1",
  "bytes_written": 32,
  "timestamp": "2026-06-17T10:42:05.001Z"
}
```

---

### 7.3 `tag sandbox signal`

Deliver a POSIX signal to a running interactive sandbox process.

```
tag sandbox signal <process-id> <signal-name>
tag sandbox signal <process-id> --signum <N>
```

**Arguments:**

- `<process-id>` — the process ID (prefix `sp_`) of a running interactive process.
- `<signal-name>` — one of: `SIGTERM`, `SIGKILL`, `SIGINT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2`. Case-insensitive.
- `--signum <N>` — alternative to named signal; raw signal number (integer). Validated against allowed set before use.
- `--wait` — after delivering SIGTERM, wait up to `--wait-timeout` seconds for the process to exit, then deliver SIGKILL automatically (implements the standard two-phase shutdown).
- `--wait-timeout <seconds>` — seconds to wait between SIGTERM and SIGKILL (default: 5). Only valid with `--wait`.

**Example usage:**

```bash
# Graceful shutdown
tag sandbox signal sp_a3f8c2b1 SIGTERM

# Hard kill
tag sandbox signal sp_a3f8c2b1 SIGKILL

# Two-phase shutdown: SIGTERM, wait 10s, then SIGKILL
tag sandbox signal sp_a3f8c2b1 SIGTERM --wait --wait-timeout 10

# Send SIGINT (equivalent to Ctrl-C in terminal)
tag sandbox signal sp_a3f8c2b1 SIGINT

# Send SIGHUP to reload config
tag sandbox signal sp_a3f8c2b1 SIGHUP
```

**Normal output:**

```
[sandbox:signal] SIGTERM → sp_a3f8c2b1 (pid 48291) delivered
```

With `--wait`:

```
[sandbox:signal] SIGTERM → sp_a3f8c2b1 (pid 48291) delivered
[sandbox:signal] Waiting up to 10s for process to exit...
[sandbox:signal] Process exited with code 0 after 0.3s
```

**Error output (process not found):**

```
error: process sp_a3f8c2b1 not found
```

**Error output (invalid signal):**

```
error: signal 'SIGPIPE' is not in the allowed set: SIGTERM SIGKILL SIGINT SIGHUP SIGUSR1 SIGUSR2
hint: use --signum <N> for raw signal numbers if you are certain
```

**JSON output (`--json`):**

```json
{
  "process_id": "sp_a3f8c2b1",
  "pid": 48291,
  "signal": "SIGTERM",
  "delivered_at": "2026-06-17T10:42:10.000Z",
  "wait": true,
  "exited": true,
  "exit_code": 0,
  "elapsed_seconds": 0.3
}
```

---

### 7.4 `tag sandbox ps`

List all currently-running interactive sandbox processes.

```
tag sandbox ps [--all] [--json]
```

**Flags:**

- `--all` — include stopped, killed, and failed processes (default: only `running`).
- `--json` — machine-readable JSON array.

**Normal output:**

```
PROCESS ID      PID    BACKEND     PTY    COMMAND           STARTED
sp_a3f8c2b1     48291  restricted  yes    python3           2026-06-17 10:42:00
sp_d9e1a4c7     50012  docker      no     bash              2026-06-17 10:55:30
```

**JSON output:**

```json
[
  {
    "process_id": "sp_a3f8c2b1",
    "pid": 48291,
    "backend": "restricted",
    "pty": true,
    "command": "python3",
    "status": "running",
    "started_at": "2026-06-17T10:42:00Z"
  }
]
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag sandbox run --interactive` MUST spawn the process using `subprocess.Popen` (not `subprocess.run`), record the PID in `sandbox_runs.pid`, record the FIFO path or master fd path in `sandbox_runs.stdin_fifo`, set `status='running'`, and return before the process exits. |
| FR-02 | `tag sandbox run --interactive` MUST generate a process ID with format `sp_<12 hex chars>` and return it to stdout as the first line of output (or as the `process_id` key in JSON mode). |
| FR-03 | `tag sandbox run --interactive` MUST stream the process's stdout (and stderr for non-PTY mode) to the terminal in real time using a background thread or asyncio task. Buffering MUST NOT suppress output that the process has already written. |
| FR-04 | When `--pty` is specified, `sandbox.py` MUST call `pty.openpty()` to obtain `(master_fd, slave_fd)`, pass `slave_fd` as `stdin`, `stdout`, and `stderr` in `subprocess.Popen`, set the PTY window size via `fcntl.ioctl(master_fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))` before spawning the process, and close `slave_fd` in the parent process after spawn. |
| FR-05 | When `--pty` is NOT specified and `--interactive` is specified, `sandbox.py` MUST create a named FIFO via `os.mkfifo()` at a path under `~/.tag/runtime/fifo/<process-id>`, open it for writing (non-blocking), and pass it as the subprocess's stdin. The FIFO path MUST be stored in `sandbox_runs.stdin_fifo`. |
| FR-06 | `tag sandbox write <id> <text>` MUST look up the process in `sandbox_runs`, verify `status='running'`, open the FIFO at `stdin_fifo` (or write to `master_fd` for PTY mode), write the interpreted text bytes, and close the file descriptor. Writes MUST be atomic from the POSIX perspective (i.e., `os.write()` for sizes <= PIPE_BUF=4096 bytes). |
| FR-07 | `tag sandbox write` MUST interpret C-style escape sequences (`\n`, `\t`, `\r`, `\x??`, `\\`) in the `<text>` argument unless `--raw` is passed. The `--hex` flag MUST accept a hex string (even number of characters, 0-9a-fA-F) and write the decoded bytes verbatim. |
| FR-08 | `tag sandbox signal <id> <name>` MUST validate the signal name against the allowed set {SIGTERM, SIGKILL, SIGINT, SIGHUP, SIGUSR1, SIGUSR2} (case-insensitive) and exit with code 1 and a descriptive error if the name is not in the set. |
| FR-09 | `tag sandbox signal <id> <name>` MUST look up the PID from `sandbox_runs.pid`, call `os.kill(pid, signal.SIG<name>)`, and catch `ProcessLookupError` (errno ESRCH) if the process has already exited. On `ProcessLookupError`, update `sandbox_runs.status` to `'stopped'` and exit with code 0 (the desired state — process not running — is achieved). |
| FR-10 | `tag sandbox signal <id> SIGTERM --wait --wait-timeout <N>` MUST poll `os.kill(pid, 0)` (null signal, probe only) every 100 ms for up to `N` seconds after delivering SIGTERM. If the process is still alive after `N` seconds, automatically deliver SIGKILL and log the escalation. |
| FR-11 | `tag sandbox ps` MUST query `sandbox_runs` for rows where `status='running'` and `pid IS NOT NULL`. For each row, verify liveness via `os.kill(pid, 0)` and update `status` to `'stopped'` for any process that has exited without TAG being notified (zombie cleanup). |
| FR-12 | Every `tag sandbox write` call MUST append a JSONL record to `~/.tag/runtime/sandbox-audit.jsonl` with fields: `event='stdin_write'`, `process_id`, `bytes_written`, `timestamp`. |
| FR-13 | Every `tag sandbox signal` call MUST append a JSONL record to `~/.tag/runtime/sandbox-audit.jsonl` with fields: `event='signal_deliver'`, `process_id`, `pid`, `signal_name`, `signal_num`, `timestamp`, `delivered` (bool). |
| FR-14 | When a process exits (detected by background stdout reader thread getting EOF), `sandbox_runs.status` MUST be updated to `'stopped'` if exit code == 0, or `'failed'` if exit code != 0, and `sandbox_runs.exit_code` and `sandbox_runs.completed_at` MUST be set. |
| FR-15 | The Docker backend in interactive mode MUST use `docker run -i` (or `-it` with `--pty`) instead of the current `docker run`. The container MUST be left running after `sandbox run --interactive` returns; subsequent `sandbox write` calls MUST use `docker exec -i <container_id>` to attach stdin. |
| FR-16 | FIFO files under `~/.tag/runtime/fifo/` MUST be deleted when the process exits or when `sandbox signal <id> SIGKILL` is delivered. Cleanup MUST occur in a `finally` block in the background thread. |
| FR-17 | The `--pty-rows` and `--pty-cols` flags MUST default to the current terminal dimensions obtained via `shutil.get_terminal_size(fallback=(80, 24))` when not explicitly provided. |
| FR-18 | `tag sandbox run --pty` without `--interactive` MUST be a validation error: PTY mode requires `--interactive` because PTY allocation is only meaningful for long-lived processes that receive subsequent stdin writes. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | The stdout-streaming background thread MUST use a read buffer of 4096 bytes and MUST forward output to the terminal with latency <= 50 ms under normal system load. |
| NFR-02 | The PTY master fd reader MUST use `os.read()` in a loop (not `subprocess.communicate()`) to avoid buffering. Under a PTY, data is line-buffered by default; the reader MUST not introduce additional buffering. |
| NFR-03 | `tag sandbox write` MUST complete (return to the caller) within 200 ms for inputs up to 64 KB under normal system load. The FIFO write MUST NOT block indefinitely; a 5-second write timeout MUST be implemented via `select.select()`. |
| NFR-04 | Signal delivery MUST complete (return to the caller) within 100 ms. `os.kill()` is a syscall and is expected to complete in microseconds; the 100 ms budget covers SQLite write and audit log append. |
| NFR-05 | New SQLite columns (`pid`, `stdin_fifo`) MUST be added via `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` inside `ensure_schema()` so that existing databases without these columns are upgraded non-destructively on first use. |
| NFR-06 | The feature MUST work on macOS (Darwin) and Linux. `pty.openpty()` is available on both. `os.mkfifo()` is available on both. Windows is explicitly out of scope; attempting interactive mode on Windows MUST print a clear error and exit 1. |
| NFR-07 | The new code path MUST add zero imports to the module-level import block. `import pty`, `import termios`, `import fcntl`, `import signal`, and `import struct` MUST all be inside function bodies (`_spawn_pty_process`, `_spawn_fifo_process`), imported only when actually needed. |
| NFR-08 | No new top-level package dependencies are introduced. All required modules (`pty`, `termios`, `fcntl`, `signal`, `select`, `struct`, `threading`) are part of the Python standard library. |
| NFR-09 | The background stdout reader thread MUST be a daemon thread (`thread.daemon = True`) so that it does not prevent the TAG process from exiting if the user presses Ctrl-C at the TAG CLI level. |
| NFR-10 | `tag sandbox ps` MUST complete in under 500 ms for up to 100 rows, including the liveness check via `os.kill(pid, 0)` for each row. |

---

## 10. Technical Design

### 10.1 Schema Changes

The existing `sandbox_runs` table in `~/.tag/runtime/tag.sqlite3` receives two new nullable columns. The `ensure_schema()` function in `sandbox.py` is extended to apply these additions idempotently using `ALTER TABLE ... ADD COLUMN` guarded by a `PRAGMA table_info` check:

```sql
-- Added to ensure_schema() — applied once, idempotent:
ALTER TABLE sandbox_runs ADD COLUMN pid INTEGER;
ALTER TABLE sandbox_runs ADD COLUMN stdin_fifo TEXT;
ALTER TABLE sandbox_runs ADD COLUMN pty_master_fd INTEGER;
ALTER TABLE sandbox_runs ADD COLUMN pty_rows INTEGER;
ALTER TABLE sandbox_runs ADD COLUMN pty_cols INTEGER;

-- New index for ps command performance:
CREATE INDEX IF NOT EXISTS idx_sr_running_pid
  ON sandbox_runs(status, pid)
  WHERE status = 'running';
```

The `pty_master_fd` column stores the integer file descriptor of the PTY master side. This descriptor is only meaningful within the process that created it (it is a file descriptor number in the current process's fd table), so it is stored as a convenience for the in-process background thread but MUST NOT be used across process boundaries.

### 10.2 Core Dataclasses

```python
from __future__ import annotations
import dataclasses
import signal as _signal
from pathlib import Path
from typing import Optional


# Allowed signals — validated before any os.kill() call.
ALLOWED_SIGNALS: dict[str, int] = {
    "SIGTERM": _signal.SIGTERM,
    "SIGKILL": _signal.SIGKILL,
    "SIGINT":  _signal.SIGINT,
    "SIGHUP":  _signal.SIGHUP,
    "SIGUSR1": _signal.SIGUSR1,
    "SIGUSR2": _signal.SIGUSR2,
}


@dataclasses.dataclass
class InteractiveProcess:
    """Represents a long-lived sandbox process spawned with --interactive.

    Attributes:
        process_id: Unique ID (format: sp_<12hex>). Stored in sandbox_runs.id.
        pid:        OS process ID. Stored in sandbox_runs.pid.
        backend:    Runtime backend ('restricted' or 'docker').
        pty:        True if a PTY pair was allocated.
        master_fd:  PTY master file descriptor (None if not PTY mode).
        slave_fd:   PTY slave file descriptor — closed after spawn.
        stdin_fifo: Path to named FIFO (None if PTY mode).
        container_id: Docker container ID (None if restricted backend).
        popen:      subprocess.Popen handle (None if docker backend manages it).
        pty_rows:   Terminal rows at spawn time.
        pty_cols:   Terminal columns at spawn time.
    """
    process_id:   str
    pid:          int
    backend:      str
    pty:          bool
    master_fd:    Optional[int]       = None
    slave_fd:     Optional[int]       = None
    stdin_fifo:   Optional[Path]      = None
    container_id: Optional[str]       = None
    popen:        Optional[object]    = None   # subprocess.Popen[bytes]
    pty_rows:     int                 = 24
    pty_cols:     int                 = 80


@dataclasses.dataclass
class WriteResult:
    """Result of a sandbox write operation."""
    process_id:    str
    bytes_written: int
    timestamp:     str


@dataclasses.dataclass
class SignalResult:
    """Result of a sandbox signal delivery."""
    process_id:     str
    pid:            int
    signal_name:    str
    signal_num:     int
    delivered:      bool
    delivered_at:   str
    wait:           bool          = False
    exited:         bool          = False
    exit_code:      Optional[int] = None
    elapsed_seconds: Optional[float] = None
```

### 10.3 PTY Spawn Algorithm

```python
def _spawn_pty_process(
    command: list[str],
    *,
    rows: int = 24,
    cols: int = 80,
    env: dict[str, str] | None = None,
    workdir: Path | None = None,
) -> tuple[subprocess.Popen, int, int]:
    """Spawn command under a PTY. Returns (popen, master_fd, slave_fd).

    The caller is responsible for:
      1. Closing slave_fd in the parent process after Popen returns.
      2. Reading from master_fd in a background thread.
      3. Writing to master_fd to send stdin data.
    """
    import fcntl
    import os
    import struct
    import termios
    import pty

    master_fd, slave_fd = pty.openpty()

    # Set PTY window size on the master before spawning.
    # struct layout: rows, cols, xpixels, ypixels (all unsigned short)
    winsize = struct.pack("HHHH", rows, cols, 0, 0)
    fcntl.ioctl(master_fd, termios.TIOCSWINSZ, winsize)

    proc = subprocess.Popen(
        command,
        stdin=slave_fd,
        stdout=slave_fd,
        stderr=slave_fd,
        close_fds=True,
        env=env,
        cwd=str(workdir) if workdir else None,
    )

    # slave_fd is now inherited by the child; close it in the parent
    # so that EOF propagates correctly when the child exits.
    os.close(slave_fd)

    return proc, master_fd, slave_fd
```

### 10.4 FIFO Spawn Algorithm

```python
def _spawn_fifo_process(
    command: list[str],
    process_id: str,
    *,
    env: dict[str, str] | None = None,
    workdir: Path | None = None,
) -> tuple[subprocess.Popen, Path]:
    """Spawn command with a named FIFO for stdin.

    Returns (popen, fifo_path).
    The FIFO remains open for the lifetime of the process.
    The caller writes to fifo_path to send stdin data.
    """
    import os

    fifo_dir = Path.home() / ".tag" / "runtime" / "fifo"
    fifo_dir.mkdir(parents=True, exist_ok=True)
    fifo_path = fifo_dir / process_id

    if fifo_path.exists():
        fifo_path.unlink()
    os.mkfifo(str(fifo_path), mode=0o600)

    # Open write end with O_NONBLOCK to avoid blocking in the parent
    # before the child has opened the read end.
    write_fd = os.open(str(fifo_path), os.O_WRONLY | os.O_NONBLOCK)
    # Open read end so the FIFO has at least one reader.
    read_fd = os.open(str(fifo_path), os.O_RDONLY | os.O_NONBLOCK)

    proc = subprocess.Popen(
        command,
        stdin=read_fd,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        close_fds=True,
        env=env,
        cwd=str(workdir) if workdir else None,
    )
    os.close(read_fd)  # Child inherited it; close parent's copy.

    return proc, fifo_path, write_fd
```

### 10.5 Background stdout Reader Thread

```python
import threading
import os
import sys


def _start_stdout_reader(
    master_fd_or_stdout,
    process_id: str,
    is_pty: bool,
    on_exit_callback,
) -> threading.Thread:
    """Start a daemon thread that forwards process stdout to the terminal.

    For PTY mode, reads from master_fd (an integer fd).
    For FIFO mode, reads from proc.stdout (a file object).
    """

    def _reader():
        try:
            if is_pty:
                fd = master_fd_or_stdout
                while True:
                    try:
                        data = os.read(fd, 4096)
                        if not data:
                            break
                        sys.stdout.buffer.write(data)
                        sys.stdout.buffer.flush()
                    except OSError:
                        # master_fd closed — child has exited.
                        break
            else:
                fobj = master_fd_or_stdout
                for chunk in iter(lambda: fobj.read(4096), b""):
                    sys.stdout.buffer.write(chunk)
                    sys.stdout.buffer.flush()
        finally:
            on_exit_callback(process_id)

    t = threading.Thread(target=_reader, daemon=True, name=f"stdout-{process_id}")
    t.start()
    return t
```

### 10.6 Signal Delivery Implementation

```python
def deliver_signal(
    conn: sqlite3.Connection,
    process_id: str,
    signal_name: str,
    *,
    wait: bool = False,
    wait_timeout: int = 5,
) -> SignalResult:
    """Deliver a POSIX signal to a running interactive sandbox process."""
    import os
    import time

    name_upper = signal_name.upper()
    if name_upper not in ALLOWED_SIGNALS:
        raise ValueError(
            f"Signal '{signal_name}' is not in the allowed set: "
            + " ".join(ALLOWED_SIGNALS)
        )
    sig_num = ALLOWED_SIGNALS[name_upper]

    row = conn.execute(
        "SELECT pid, status FROM sandbox_runs WHERE id = ?",
        (process_id,),
    ).fetchone()
    if row is None:
        raise KeyError(f"Process {process_id!r} not found")
    pid, status = row

    delivered = False
    try:
        os.kill(pid, sig_num)
        delivered = True
    except ProcessLookupError:
        # Process already exited; update status and treat as success.
        conn.execute(
            "UPDATE sandbox_runs SET status='stopped', completed_at=? WHERE id=?",
            (_utc_now(), process_id),
        )
        conn.commit()

    now = _utc_now()
    _append_audit(
        event="signal_deliver",
        process_id=process_id,
        pid=pid,
        signal_name=name_upper,
        signal_num=sig_num,
        timestamp=now,
        delivered=delivered,
    )

    result = SignalResult(
        process_id=process_id,
        pid=pid,
        signal_name=name_upper,
        signal_num=sig_num,
        delivered=delivered,
        delivered_at=now,
        wait=wait,
    )

    if wait and delivered:
        deadline = time.monotonic() + wait_timeout
        while time.monotonic() < deadline:
            try:
                os.kill(pid, 0)  # null signal: probe only
            except ProcessLookupError:
                result.exited = True
                result.exit_code = _reap_exit_code(process_id)
                result.elapsed_seconds = round(
                    wait_timeout - (deadline - time.monotonic()), 3
                )
                break
            time.sleep(0.1)
        if not result.exited:
            # Escalate to SIGKILL
            try:
                os.kill(pid, _signal.SIGKILL)
            except ProcessLookupError:
                pass
            _append_audit(
                event="signal_deliver",
                process_id=process_id,
                pid=pid,
                signal_name="SIGKILL",
                signal_num=_signal.SIGKILL,
                timestamp=_utc_now(),
                delivered=True,
                escalated=True,
            )

    return result
```

### 10.7 stdin Write Implementation

```python
def write_stdin(
    conn: sqlite3.Connection,
    process_id: str,
    text: str | bytes,
    *,
    raw: bool = False,
    no_newline: bool = False,
) -> WriteResult:
    """Write text (or bytes) to a running interactive process's stdin."""
    import os
    import select

    row = conn.execute(
        "SELECT pid, stdin_fifo, pty_master_fd, status FROM sandbox_runs WHERE id=?",
        (process_id,),
    ).fetchone()
    if row is None:
        raise KeyError(f"Process {process_id!r} not found")
    pid, fifo_path, master_fd_stored, status = row

    if status != "running":
        raise RuntimeError(
            f"Process {process_id!r} is not running (status: {status!r})"
        )

    # Interpret escape sequences unless --raw.
    if isinstance(text, str):
        if not raw:
            text = _interpret_escapes(text)
        if not no_newline and not text.endswith("\n"):
            text += "\n"
        data = text.encode("utf-8")
    else:
        data = text

    # For PTY mode: write to master_fd (in-process fd, not persisted across calls)
    # We look it up from the in-process _INTERACTIVE_PROCS registry first,
    # falling back to re-opening the FIFO if it's a FIFO-based process.
    written = 0
    if fifo_path:
        # FIFO-based stdin: open write end with timeout guard.
        fd = os.open(fifo_path, os.O_WRONLY | os.O_NONBLOCK)
        rlist, wlist, _ = select.select([], [fd], [], 5.0)
        if not wlist:
            os.close(fd)
            raise TimeoutError(f"FIFO write timed out after 5s for {process_id!r}")
        written = os.write(fd, data)
        os.close(fd)
    elif master_fd_stored is not None:
        # PTY master fd: look up from in-process registry.
        proc_handle = _INTERACTIVE_PROCS.get(process_id)
        if proc_handle and proc_handle.master_fd is not None:
            written = os.write(proc_handle.master_fd, data)
        else:
            raise RuntimeError(
                f"PTY master fd for {process_id!r} is not available in this process. "
                "Interactive PTY processes must be written from the same TAG process "
                "that spawned them."
            )

    now = _utc_now()
    _append_audit(
        event="stdin_write",
        process_id=process_id,
        bytes_written=written,
        timestamp=now,
    )

    return WriteResult(
        process_id=process_id,
        bytes_written=written,
        timestamp=now,
    )


def _interpret_escapes(s: str) -> str:
    """Interpret C-style escape sequences in s."""
    return (
        s.replace("\\n", "\n")
         .replace("\\t", "\t")
         .replace("\\r", "\r")
         .replace("\\\\", "\\")
         .encode("raw_unicode_escape")
         .decode("unicode_escape")
    )
```

### 10.8 In-Process Registry

Because PTY master file descriptors are only valid within the process that created them, an in-process dictionary maps `process_id → InteractiveProcess`. This is not persisted to SQLite (except for the integer fd value, which is stored for reference) and is re-populated on `tag sandbox run --interactive`.

```python
# Module-level registry — populated by run_interactive_in_sandbox().
# Keys: process_id strings. Values: InteractiveProcess instances.
_INTERACTIVE_PROCS: dict[str, InteractiveProcess] = {}
```

### 10.9 Audit Log Appender

```python
import json
import fcntl


def _append_audit(*, **fields) -> None:
    """Append a JSONL record to the sandbox audit log. Thread-safe via flock."""
    audit_path = Path.home() / ".tag" / "runtime" / "sandbox-audit.jsonl"
    audit_path.parent.mkdir(parents=True, exist_ok=True)
    line = json.dumps(fields, ensure_ascii=False) + "\n"
    with audit_path.open("a", encoding="utf-8") as f:
        fcntl.flock(f, fcntl.LOCK_EX)
        try:
            f.write(line)
        finally:
            fcntl.flock(f, fcntl.LOCK_UN)
```

### 10.10 Docker Backend Integration

For Docker interactive mode, `_run_docker_interactive()` replaces the batch `_run_docker()`:

```python
def _run_docker_interactive(
    command: list[str],
    image: str,
    *,
    pty: bool = False,
    timeout: int = 3600,
) -> tuple[subprocess.Popen, str]:
    """Start an interactive Docker container. Returns (popen, container_id).

    Uses 'docker run -d' to start the container detached, then
    'docker exec -i[-t]' for subsequent stdin writes.
    """
    import subprocess
    import uuid

    container_name = f"tag-sandbox-{uuid.uuid4().hex[:8]}"
    tty_flag = ["-t"] if pty else []

    # Start container in detached mode with a keepalive tail.
    run_cmd = [
        "docker", "run", "-d",
        "--name", container_name,
        "--network=none",
        "--memory=512m",
        "--cpus=1",
        image,
        "tail", "-f", "/dev/null",   # keepalive
    ]
    result = subprocess.run(run_cmd, capture_output=True, text=True)
    if result.returncode != 0:
        raise RuntimeError(f"docker run failed: {result.stderr.strip()}")
    container_id = result.stdout.strip()

    # Exec the actual command inside the running container.
    exec_cmd = [
        "docker", "exec", "-i",
    ] + tty_flag + [container_id] + command

    proc = subprocess.Popen(
        exec_cmd,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    return proc, container_id
```

### 10.11 New Files

No new files are added. All changes are contained in `/Users/sanskar/dev/test/tag/src/tag/sandbox.py`. The FIFO directory `~/.tag/runtime/fifo/` is created at runtime.

### 10.12 controller.py Integration Points

Three new command handlers are registered in `controller.py`:

- `cmd_sandbox_write(args)` — parses `tag sandbox write <id> <text>` and calls `sandbox.write_stdin()`.
- `cmd_sandbox_signal(args)` — parses `tag sandbox signal <id> <name>` and calls `sandbox.deliver_signal()`.
- `cmd_sandbox_ps(args)` — parses `tag sandbox ps` and calls `sandbox.list_interactive_processes()`.

The existing `cmd_sandbox_run(args)` handler is extended to check `args.interactive` and branch to `sandbox.run_interactive_in_sandbox()` instead of `sandbox.run_in_sandbox()`.

---

## 11. Security Considerations

1. **Signal allowlist enforcement.** `os.kill(pid, sig)` with an unrestricted signal name would allow callers to send arbitrary signals to arbitrary PIDs if the PID lookup were wrong. The allowlist (SIGTERM, SIGKILL, SIGINT, SIGHUP, SIGUSR1, SIGUSR2) is validated before any `os.kill()` call. The PID is never supplied by the user; it is looked up from SQLite using the opaque process ID.

2. **PID ownership verification.** Before calling `os.kill(pid, sig)`, verify that the `pid` in `sandbox_runs` matches the current user via `/proc/<pid>/status` (Linux) or `ps -o uid= -p <pid>` (macOS). This prevents a race condition where a sandbox process exits and its PID is reused by an unrelated system process before the signal call.

3. **FIFO permissions.** Named FIFOs are created with `mode=0o600` (`os.mkfifo(path, 0o600)`). Only the owning user can read or write the FIFO. The FIFO directory `~/.tag/runtime/fifo/` is created with `mode=0o700`.

4. **Input size cap on `sandbox write`.** Writes larger than 1 MB are rejected before the FIFO open, with a clear error. This prevents a caller from buffering arbitrarily large data in a kernel FIFO buffer (Linux FIFO capacity is typically 65536 bytes; writes beyond that block).

5. **No shell interpretation of `--code`.** The `--code` argument to `tag sandbox run --interactive` is split via `shlex.split()` and passed as a list to `subprocess.Popen()`. It is never passed through `shell=True`. This prevents shell injection even if the `--code` string contains shell metacharacters.

6. **PTY slave fd closure.** The PTY slave fd is closed in the parent process immediately after `subprocess.Popen()` returns. Leaving it open in the parent would mean EOF is never delivered to the master when the child exits, causing the background reader thread to block forever.

7. **Audit log integrity.** The audit log uses `fcntl.LOCK_EX` to prevent torn writes under concurrent signal or write commands. Log entries are written as single-line JSON with `ensure_ascii=False`; newlines within field values are JSON-escaped.

8. **Docker container naming.** Container names are generated with `uuid4().hex[:8]` to prevent name collisions. Containers are labelled with `--label tag.process_id=<id>` for identification via `docker ps --filter label=...`. Containers are stopped (not just `docker exec` killed) when `SIGKILL` is delivered to the Docker backend.

9. **Zombie prevention.** The background stdout reader thread calls `proc.wait()` after the read loop exits to reap the child process and prevent it from becoming a zombie. For PTY mode, `os.waitpid(pid, 0)` is called in the reader thread's `finally` block.

10. **Cross-process PTY fd safety.** The `pty_master_fd` integer is stored in SQLite for reference, but `write_stdin()` only uses the fd if the calling process is the same process that spawned the interactive process (checked via `_INTERACTIVE_PROCS` registry). Cross-process PTY writes raise a clear error rather than attempting to use an invalid fd number.

---

## 12. Testing Strategy

### 12.1 Unit Tests

Location: `tests/test_sandbox_interactive.py`

| Test | Description |
|------|-------------|
| `test_signal_allowlist_accepts_valid` | Parameterised over all 6 allowed signal names; verifies no `ValueError`. |
| `test_signal_allowlist_rejects_invalid` | Parameterised over `['SIGPIPE', 'SIGCHLD', 'SIGABRT', '', 'bad', '9']`; verifies `ValueError`. |
| `test_interpret_escapes_newline` | `_interpret_escapes("hello\\nworld")` == `"hello\nworld"`. |
| `test_interpret_escapes_hex` | `_interpret_escapes("\\x41")` == `"A"`. |
| `test_interpret_escapes_raw_passthrough` | `_interpret_escapes("\\\\n")` == `"\\n"` (double-escaped). |
| `test_ensure_schema_adds_columns` | Creates an in-memory SQLite db with the original schema, calls `ensure_schema()`, verifies `pid` and `stdin_fifo` columns exist. |
| `test_spawn_pty_sets_winsize` | Mocks `pty.openpty()` and `fcntl.ioctl()`; verifies `ioctl` called with correct `TIOCSWINSZ` struct for (40, 120). |
| `test_write_stdin_rejects_stopped_process` | Inserts a row with `status='stopped'`; calls `write_stdin()`; expects `RuntimeError`. |
| `test_write_stdin_rejects_oversized_input` | Passes 1.1 MB string; expects `ValueError`. |
| `test_audit_log_appended_on_signal` | Calls `deliver_signal()` against a mocked `os.kill()`; reads `sandbox-audit.jsonl`; verifies record present. |

### 12.2 Integration Tests

Location: `tests/test_sandbox_integration.py` (existing file extended)

| Test | Description |
|------|-------------|
| `test_interactive_python_repl_echo` | Spawns `python3 -c "import sys; [print(l.strip()) for l in sys.stdin]"` in interactive mode; writes `"hello\n"`; reads stdout; asserts `"hello"` appears. |
| `test_pty_isatty_true` | Spawns `python3 -c "import sys; print(sys.stdin.isatty())"` with `--pty`; captures output; asserts `"True"`. |
| `test_sigterm_stops_sleep` | Spawns `sleep 3600` in interactive mode; delivers SIGTERM; waits 1s; asserts `status='stopped'` in SQLite. |
| `test_sigterm_wait_escalates_to_sigkill` | Spawns a process that ignores SIGTERM (`trap '' TERM; sleep 3600`); delivers SIGTERM with `--wait --wait-timeout 2`; asserts SIGKILL is delivered and process exits. |
| `test_fifo_cleanup_on_process_exit` | Spawns a `cat` process; sends EOF (`\x04`); waits for exit; asserts FIFO file no longer exists. |
| `test_docker_interactive_bash` | Requires `docker`; spawns `bash` in Docker interactive mode; writes `echo hello`; reads output; asserts `"hello"`. Skipped if Docker not available. |
| `test_ps_lists_running_only` | Spawns two processes; kills one; calls `list_interactive_processes()`; asserts only the live one appears. |
| `test_ps_cleans_zombies` | Inserts a fake `running` row with a non-existent PID; calls `list_interactive_processes()`; asserts that row's status is updated to `stopped`. |

### 12.3 Performance Tests

Location: `tests/test_sandbox_perf.py`

| Test | Target |
|------|--------|
| `test_write_latency_p50` | Median round-trip for `write_stdin()` + stdout echo < 100 ms over 50 iterations |
| `test_signal_delivery_time` | `deliver_signal(SIGTERM)` returns in < 100 ms (excluding `--wait` polling) |
| `test_ps_under_load` | `list_interactive_processes()` with 100 synthetic rows completes in < 500 ms |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag sandbox run --interactive --code "python3" --pty` prints a `Process ID: sp_...` line and returns to the shell prompt within 2 seconds. | Integration test `test_interactive_python_repl_echo` |
| AC-02 | `tag sandbox write <id> "print(42)\n"` causes `42` to appear on stdout within 200 ms. | Integration test with timing assertion |
| AC-03 | `tag sandbox signal <id> SIGTERM` on a `sleep 3600` process causes the process to exit and `sandbox_runs.status` to become `stopped` within 1 second. | Integration test `test_sigterm_stops_sleep` |
| AC-04 | `tag sandbox signal <id> SIGKILL` on a process that ignores SIGTERM causes unconditional exit. | Integration test `test_sigterm_wait_escalates_to_sigkill` |
| AC-05 | `tag sandbox run --pty` without `--interactive` exits with code 1 and prints `"error: --pty requires --interactive"`. | Unit test |
| AC-06 | `tag sandbox signal <id> SIGPIPE` exits with code 1 and prints the allowlist error message without calling `os.kill()`. | Unit test `test_signal_allowlist_rejects_invalid` |
| AC-07 | `python3 -c "import sys; print(sys.stdin.isatty())"` prints `True` when launched with `--pty`, `False` when launched without. | Integration test `test_pty_isatty_true` |
| AC-08 | Every `tag sandbox write` call appends exactly one JSONL record with `event="stdin_write"` to `sandbox-audit.jsonl`. | Integration test audit log assertion |
| AC-09 | Every `tag sandbox signal` call appends exactly one JSONL record with `event="signal_deliver"` to `sandbox-audit.jsonl`. | Integration test audit log assertion |
| AC-10 | FIFO file under `~/.tag/runtime/fifo/<id>` is deleted after the process exits. | Integration test `test_fifo_cleanup_on_process_exit` |
| AC-11 | `tag sandbox ps` shows only `status='running'` rows by default; `--all` shows all rows. | Unit test |
| AC-12 | `tag sandbox ps` updates stale `running` rows to `stopped` for PIDs that no longer exist. | Integration test `test_ps_cleans_zombies` |
| AC-13 | `ensure_schema()` adds `pid` and `stdin_fifo` columns to an existing `sandbox_runs` table without data loss. | Unit test `test_ensure_schema_adds_columns` |
| AC-14 | `tag sandbox write <id>` fails with `RuntimeError` if the process is in `stopped` or `failed` state. | Unit test `test_write_stdin_rejects_stopped_process` |
| AC-15 | `tag sandbox signal <id> SIGTERM --wait --wait-timeout 10` delivers SIGKILL automatically if the process does not exit within 10 seconds, and records the escalation in the audit log. | Integration test `test_sigterm_wait_escalates_to_sigkill` |
| AC-16 | Docker backend interactive mode (`--backend docker`) passes integration test `test_docker_interactive_bash` when Docker is available. | CI matrix conditional on Docker |
| AC-17 | Attempting interactive mode on Windows exits with code 1 and prints `"error: interactive sandbox mode is not supported on Windows"`. | Unit test with `sys.platform='win32'` mock |

---

## 14. Dependencies

| Dependency | Version / Notes | Required? | Install |
|------------|----------------|-----------|---------|
| Python `pty` module | stdlib (Python 3.x, POSIX only) | Required for `--pty` | No install needed |
| Python `termios` module | stdlib (POSIX only) | Required for TIOCSWINSZ | No install needed |
| Python `fcntl` module | stdlib (POSIX only) | Required for FIFO locking and ioctl | No install needed |
| Python `signal` module | stdlib | Required for signal number lookup | No install needed |
| Python `select` module | stdlib | Required for FIFO write timeout | No install needed |
| Python `struct` module | stdlib | Required for TIOCSWINSZ packing | No install needed |
| Python `threading` module | stdlib | Required for background reader | No install needed |
| Docker CLI | >= 20.10 | Optional; Docker backend only | System package |
| PRD-028 sandbox_runs table | Existing schema | Required | Already in `sandbox.py` |
| PRD-013 tracing patterns | Span conventions | Informational only | Already in codebase |
| PRD-034 security patterns | Blocked patterns | Informational only | Already in `security.py` |

---

## 15. Open Questions

| ID | Question | Owner | Status |
|----|----------|-------|--------|
| OQ-01 | Should `tag sandbox write` block waiting for the process to consume the input, or should it be fire-and-forget? The current design is fire-and-forget (write to FIFO and return). Blocking would give better backpressure semantics but complicates the CLI UX. | Architecture | Open |
| OQ-02 | Should SIGWINCH be propagated when the host terminal is resized? This would require a signal handler in TAG's main process, an `ioctl(master_fd, TIOCSWINSZ, ...)` update, and `os.kill(pid, signal.SIGWINCH)`. Low priority but useful for full terminal emulation. | Engineering | Deferred |
| OQ-03 | For E2B backend, `process.send_stdin()` is available in the E2B SDK. Should we wire E2B interactive mode to use E2B's native API instead of local PTY? This would require a separate code path but would enable interactive sessions inside E2B cloud micro-VMs. | Product | Open |
| OQ-04 | Should `tag sandbox signal <id> SIGTERM --wait` be the default instead of requiring an explicit flag? Most users who send SIGTERM intend the two-phase shutdown. Making `--wait` the default (with `--no-wait` to opt out) might be more ergonomic. | UX | Open |
| OQ-05 | The `pty_master_fd` stored in SQLite is only valid within the spawning process. If a user restarts TAG and tries to `tag sandbox write` to a PTY process that was started in a previous TAG invocation, the write will fail with a clear error. Is this acceptable, or should we implement a re-attach mechanism (e.g., re-opening the PTY master via `/proc/<pid>/fd/0` on Linux)? | Architecture | Open |
| OQ-06 | For very long-running interactive sessions (hours), the background reader thread accumulates output in `sandbox_runs.output`. Should there be a rolling window (keep only last N bytes) to prevent the SQLite row from growing unboundedly? | Engineering | Open |
| OQ-07 | Should `tag sandbox write --hex 03` (Ctrl-C) be documented as the preferred way to send keyboard interrupt to a PTY process, rather than `tag sandbox signal <id> SIGINT`? Both work; `--hex 03` is more authentic to what a real terminal does (the line discipline converts it to SIGINT), while `os.kill(SIGINT)` bypasses the PTY line discipline. | UX / Documentation | Open |

---

## 16. Complexity and Timeline

**Total estimated effort: 3–5 days (S)**

### Phase 1 — Schema and Data Model (Day 1)

- Add `pid`, `stdin_fifo`, `pty_master_fd`, `pty_rows`, `pty_cols` columns to `ensure_schema()` via idempotent `ALTER TABLE`.
- Define `InteractiveProcess`, `WriteResult`, `SignalResult` dataclasses.
- Define `ALLOWED_SIGNALS` dict.
- Define `_INTERACTIVE_PROCS` module-level registry.
- Write unit tests for schema migration (`test_ensure_schema_adds_columns`).

### Phase 2 — Spawn and PTY Implementation (Day 2)

- Implement `_spawn_pty_process()` with `pty.openpty()`, `TIOCSWINSZ`, and `subprocess.Popen`.
- Implement `_spawn_fifo_process()` with `os.mkfifo()` and `subprocess.Popen`.
- Implement `_start_stdout_reader()` background thread.
- Implement `run_interactive_in_sandbox()` top-level function that selects PTY vs FIFO branch, records in SQLite, starts reader thread.
- Write integration tests: `test_pty_isatty_true`, `test_interactive_python_repl_echo`.

### Phase 3 — Write and Signal Commands (Day 3)

- Implement `write_stdin()` with FIFO write path and PTY master fd path.
- Implement `deliver_signal()` with allowlist validation, `os.kill()`, and `--wait` polling.
- Implement `_append_audit()` with `fcntl.LOCK_EX`.
- Implement `_interpret_escapes()` and `--hex` path.
- Write unit tests for signal allowlist, write rejection, audit log.
- Write integration tests: `test_sigterm_stops_sleep`, `test_sigterm_wait_escalates_to_sigkill`.

### Phase 4 — Docker Backend and `sandbox ps` (Day 4)

- Implement `_run_docker_interactive()` with `docker run -d` / `docker exec -i`.
- Implement `list_interactive_processes()` with zombie cleanup via `os.kill(pid, 0)`.
- Register `cmd_sandbox_write`, `cmd_sandbox_signal`, `cmd_sandbox_ps` in `controller.py`.
- Extend `cmd_sandbox_run` to branch on `args.interactive`.
- Write integration test `test_docker_interactive_bash` (Docker-conditional).
- Write unit tests `test_ps_lists_running_only`, `test_ps_cleans_zombies`.

### Phase 5 — Polish, FIFO Cleanup, and Performance (Day 5)

- Implement FIFO cleanup in `finally` blocks and process-exit callback.
- Implement Windows platform check.
- Write performance tests.
- Update `docs/prd/INDEX.md` with PRD-098 entry.
- Manual end-to-end walkthrough: Python REPL, sqlite3 CLI, bash in Docker.
- Address any CI failures from the Docker matrix job.

