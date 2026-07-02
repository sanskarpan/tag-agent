# PRD-098: Process stdin Streaming and Signal Delivery (SIGTERM/SIGKILL/SIGINT) (`tag sandbox signal / tag sandbox write`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** S (3-5 days)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox`
**Depends on:** PRD-028 (sandbox code execution — core sandbox runtime and `sandbox_runs` table), PRD-013 (agent tracing/observability — span instrumentation patterns), PRD-034 (secret scanning — signal validation and input sanitisation), PRD-003 (rich streaming TUI — interactive terminal output), PRD-005 (execution backend selection — runtime dispatch logic)
**Inspired by:** E2B process control, Docker exec, pty subprocess

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) executes agent-generated code and shell commands inside isolated runtimes — Docker containers, E2B micro-VMs, Modal functions, or a restricted subprocess layer. The current implementation is entirely batch-oriented: `RunSandbox()` spawns a process, waits for it to terminate, then returns the complete stdout/stderr buffer. There is no way to interact with a running process once it has started, no way to terminate it cleanly before timeout, and no way to drive interactive programs (REPLs, test harnesses, database CLIs) that expect input over their lifetime.

This limitation blocks several important agent workflows. An AI agent working in a stateful Python REPL needs to send multiple expressions to the same interpreter session, receiving intermediate results between sends, so that variable state accumulates and computations build on each other. An agent orchestrating a long-running build or data-processing command needs to send SIGTERM for graceful shutdown — giving the process a chance to flush buffers and clean up — rather than waiting for an arbitrary timeout to trigger a hard kill. Integration tests driven by agent-generated test suites frequently involve programs that read from stdin to advance through setup wizards or interactive prompts.

This PRD specifies two new `tag sandbox` subcommands — `tag sandbox write` and `tag sandbox signal` — plus an `--interactive` / `--pty` mode on `tag sandbox run`. Together they implement the complete POSIX process interaction model inside TAG's sandbox layer: stdin streaming over a named FIFO or PTY master file descriptor, POSIX signal delivery via `syscall.Kill` against the tracked process PID (or its process group), and optional PTY allocation via `github.com/creack/pty` so that programs behave as they would in a real terminal (cursor addressing, raw keystrokes, readline support, `isatty()` returning `True`). Process lifetimes are bounded by a `context.Context` for cancellation and timeout.

The design draws from three proven reference implementations: E2B's process control API (which exposes `process.send_stdin()` and `process.kill()` on its `Process` handle), Docker's `docker exec` protocol (which multiplexes stdin/stdout/stderr over a single hijacked connection with a well-defined stream header framing — reached in Go via the `docker/moby` client's `ContainerAttach`/`ContainerExecAttach`), and the terminado/pyxtermjs PTY-WebSocket bridge pattern (which connects a PTY master fd to an event-loop reader and routes messages via a JSON array protocol; in Go this becomes a goroutine copying the master fd to the terminal). TAG's implementation is simpler than all three because it targets a single-user local CLI rather than a multi-tenant web service, but the process model and signal routing follow the same POSIX primitives.

The feature is intentionally narrow in scope. It extends `internal/sandbox` with a long-lived process table, a FIFO-based stdin channel, signal delivery, and PTY support. It does not implement WebSocket-based terminal streaming, multi-user session sharing, or terminal recording (all of which are follow-on features). The surface is small — roughly 350 additional lines in `internal/sandbox`, two new CLI subcommands, and three new SQLite columns — but the impact on interactive agent workflows is significant.

---

## 2. Problem Statement

### 2.1 Batch-only execution prevents stateful agent interactions

`RunSandbox()` in `internal/sandbox` runs `exec.CommandContext(...).Run()` (for the restricted backend) or a blocking `docker/moby` container run (for the Docker backend), both of which wait for process termination before returning. There is no `*exec.Cmd` handle retained anywhere, no PID recorded in SQLite, and no way to send additional input to a process once it is running.

This forces agents that need stateful evaluation (e.g. a Python REPL) to use workarounds: write a monolithic script to a tempfile and run it as a single batch invocation, or serialise all state to disk between invocations. Both workarounds are fragile. A monolithic script cannot show intermediate results between statements. Disk-based serialisation fails for values that cannot be serialised, breaks REPL-style exploration, and doubles the I/O cost of every interaction.

The root cause is architectural: the current sandbox layer was designed as a one-shot code runner, not as a process host. Fixing this requires introducing the concept of a *long-lived sandbox process* — a process that is created once, assigned a stable ID, tracked in SQLite, and then interacted with via subsequent commands.

### 2.2 No graceful shutdown path

When a sandbox run exceeds its timeout, the current code lets the `context.Context` deadline fire (`context.DeadlineExceeded`) and records `status='failed'`. No signal is sent; `CommandContext` sends a bare kill and the child is otherwise abandoned. Processes that registered signal handlers — web servers, database engines, compilers with incremental cache writes — never get a chance to flush state or release locks.

For Docker-backend runs, the moby client's `ContainerStop` timeout is set to the run timeout, but the surrounding context deadline (timeout+30s) means TAG force-removes the container 30 seconds after the container timeout. This produces orphaned container IDs in `docker ps` output and leaves named volumes in inconsistent states when the container was mounting one.

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
| G3 | `tag sandbox signal <id> SIGTERM` and `tag sandbox signal <id> SIGKILL` deliver the named signal to the process via `syscall.Kill(pid, sig)` (or `syscall.Kill(-pgid, sig)` for process-group fan-out when the child was started with `Setpgid`). SIGINT is also supported. |
| G4 | `--pty` flag on `tag sandbox run --interactive` allocates a PTY pair via `github.com/creack/pty` (`pty.Start`) so that `isatty(stdin_fd)` returns `True` inside the process and terminal-aware programs behave interactively. |
| G5 | All process state transitions (created → running → stopped/killed) are recorded in the existing `sandbox_runs` table with two new columns: `pid` and `stdin_fifo`. |
| G6 | Signal validation rejects unknown signal names with a clear error before any `syscall.Kill()` call is attempted. Only SIGTERM, SIGKILL, SIGINT, SIGHUP, SIGUSR1, SIGUSR2 are allowed. |
| G7 | `tag sandbox run --interactive` streams stdout in real time to the terminal for the duration of the process, using a background goroutine that copies the process stdout/PTY-master fd to the terminal (`io.Copy`). |
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
| NG5 | Window resize (SIGWINCH) propagation. PTY window size is set once at spawn time via `pty.Setsize(master, &pty.Winsize{...})` and not tracked thereafter. |
| NG6 | stdin streaming over the network. `tag sandbox write` only works on the local machine where the process is running. |
| NG7 | Signal delivery to Docker containers via the Docker API. Signal delivery is via `syscall.Kill(pid, sig)` to the host-side process (the `docker exec` client process). Killing the Docker-internal PID 1 requires the moby client's `ContainerKill(ctx, id, signal)`, which is left for a follow-on. |
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
| Signal rejection rate | All signal names not in the allowlist rejected with exit code 1 before `syscall.Kill()` | Table-driven unit test over invalid names |
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
- `--pty` — allocate a PTY pair (`github.com/creack/pty`). Requires `--interactive`. The child is started attached to the PTY slave; the master fd is read by TAG (a goroutine) and forwarded to the terminal. Only valid for `restricted` and `docker` backends.
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

The process continues running. The user returns to the shell prompt. The process's stdout continues streaming via a background goroutine that forwards output to the terminal.

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
| FR-01 | `tag sandbox run --interactive` MUST spawn the process with `exec.CommandContext` and `cmd.Start()` (not a blocking `Run`), record the PID in `sandbox_runs.pid`, record the FIFO path or master-fd marker in `sandbox_runs.stdin_fifo`, set `status='running'`, and return before the process exits. |
| FR-02 | `tag sandbox run --interactive` MUST generate a process ID with format `sp_<12 hex chars>` (`crypto/rand` + `hex.EncodeToString`) and return it to stdout as the first line of output (or as the `process_id` key in JSON mode). |
| FR-03 | `tag sandbox run --interactive` MUST stream the process's stdout (and stderr for non-PTY mode) to the terminal in real time using a background goroutine (`io.Copy(os.Stdout, r)`). Buffering MUST NOT suppress output that the process has already written. |
| FR-04 | When `--pty` is specified, `internal/sandbox` MUST start the child under a PTY via `creack/pty` (`pty.Start(cmd)` returns the master `*os.File`), set the PTY window size via `pty.Setsize(master, &pty.Winsize{Rows: rows, Cols: cols})` immediately after start, and read the master fd in a goroutine. `creack/pty` handles slave-fd wiring and closing internally. |
| FR-05 | When `--pty` is NOT specified and `--interactive` is specified, `internal/sandbox` MUST create a named FIFO via `unix.Mkfifo()` (`golang.org/x/sys/unix`) at a path under `~/.tag/runtime/fifo/<process-id>`, open the read end non-blocking and pass it as `cmd.Stdin`. The FIFO path MUST be stored in `sandbox_runs.stdin_fifo`. A named FIFO (not an `os.Pipe`) is required so that a separate `tag sandbox write` process can open and write to it. |
| FR-06 | `tag sandbox write <id> <text>` MUST look up the process in `sandbox_runs`, verify `status='running'`, open the FIFO at `stdin_fifo` (or write to the master fd via the in-process registry for PTY mode), write the interpreted bytes with `unix.Write`/`os.File.Write`, and close the descriptor. Writes MUST be atomic from the POSIX perspective (single `Write` for sizes <= PIPE_BUF=4096 bytes). |
| FR-07 | `tag sandbox write` MUST interpret C-style escape sequences (`\n`, `\t`, `\r`, `\x??`, `\\`) in the `<text>` argument unless `--raw` is passed (via `strconv.Unquote` on a wrapped literal, or an equivalent hand-rolled decoder). The `--hex` flag MUST accept a hex string (even length, `0-9a-fA-F`) decoded with `encoding/hex` and write the bytes verbatim. |
| FR-08 | `tag sandbox signal <id> <name>` MUST validate the signal name against the allowed set {SIGTERM, SIGKILL, SIGINT, SIGHUP, SIGUSR1, SIGUSR2} (case-insensitive) and exit with code 1 and a descriptive error if the name is not in the set. |
| FR-09 | `tag sandbox signal <id> <name>` MUST look up the PID from `sandbox_runs.pid`, call `syscall.Kill(pid, sig)`, and handle `syscall.ESRCH` (no such process) if the process has already exited. On `ESRCH`, update `sandbox_runs.status` to `'stopped'` and exit with code 0 (the desired state — process not running — is achieved). |
| FR-10 | `tag sandbox signal <id> SIGTERM --wait --wait-timeout <N>` MUST poll `syscall.Kill(pid, 0)` (null signal, probe only) every 100 ms for up to `N` seconds (a `time.Ticker` bounded by a `context.WithTimeout`). If the process is still alive after `N` seconds, automatically deliver SIGKILL and log the escalation. |
| FR-11 | `tag sandbox ps` MUST query `sandbox_runs` for rows where `status='running'` and `pid IS NOT NULL`. For each row, verify liveness via `syscall.Kill(pid, 0)` and update `status` to `'stopped'` for any process that has exited without TAG being notified (zombie cleanup). |
| FR-12 | Every `tag sandbox write` call MUST append a JSONL record to `~/.tag/runtime/sandbox-audit.jsonl` with fields: `event="stdin_write"`, `process_id`, `bytes_written`, `timestamp` (`json.Marshal` of a struct + newline). |
| FR-13 | Every `tag sandbox signal` call MUST append a JSONL record to `~/.tag/runtime/sandbox-audit.jsonl` with fields: `event="signal_deliver"`, `process_id`, `pid`, `signal_name`, `signal_num`, `timestamp`, `delivered` (bool). |
| FR-14 | When a process exits (detected by the background reader goroutine getting EOF and calling `cmd.Wait()`), `sandbox_runs.status` MUST be updated to `'stopped'` if exit code == 0, or `'failed'` if exit code != 0 (from `exec.ExitError.ExitCode()`), and `sandbox_runs.exit_code` and `sandbox_runs.completed_at` MUST be set. |
| FR-15 | The Docker backend in interactive mode MUST start a detached container via the moby client `ContainerCreate`+`ContainerStart` (keepalive), then attach the command's stdin via `ContainerExecCreate`+`ContainerExecAttach` (a hijacked stream; `Tty: true` when `--pty`). The container MUST be left running after `sandbox run --interactive` returns; subsequent `sandbox write` calls MUST use a fresh exec-attach against `<container_id>`. |
| FR-16 | FIFO files under `~/.tag/runtime/fifo/` MUST be deleted when the process exits or when `sandbox signal <id> SIGKILL` is delivered. Cleanup MUST occur in a `defer` inside the background reader goroutine. |
| FR-17 | The `--pty-rows` and `--pty-cols` flags MUST default to the current terminal dimensions obtained via `pty.GetsizeFull(os.Stdin)` (falling back to 80x24) when not explicitly provided. |
| FR-18 | `tag sandbox run --pty` without `--interactive` MUST be a validation error: PTY mode requires `--interactive` because PTY allocation is only meaningful for long-lived processes that receive subsequent stdin writes. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | The stdout-streaming goroutine MUST use a read buffer of 4096 bytes and MUST forward output to the terminal with latency <= 50 ms under normal system load. |
| NFR-02 | The PTY master fd reader MUST use `os.File.Read`/`io.Copy` in a loop (not a buffered `bufio.Scanner` that waits for full lines) to avoid buffering. Under a PTY, data is line-buffered by default; the reader MUST not introduce additional buffering. |
| NFR-03 | `tag sandbox write` MUST complete (return to the caller) within 200 ms for inputs up to 64 KB under normal system load. The FIFO write MUST NOT block indefinitely; a 5-second write timeout MUST be implemented by opening the FIFO with `O_NONBLOCK` and using `unix.Select` (or a `SetWriteDeadline` on the file). |
| NFR-04 | Signal delivery MUST complete (return to the caller) within 100 ms. `syscall.Kill` is a syscall and is expected to complete in microseconds; the 100 ms budget covers the SQLite write and audit log append. |
| NFR-05 | New SQLite columns (`pid`, `stdin_fifo`, ...) MUST be added via `ALTER TABLE ... ADD COLUMN` inside `EnsureSchema()`, with the `duplicate column name` error from the driver swallowed, so that existing databases without these columns are upgraded non-destructively and idempotently on first use. |
| NFR-06 | The feature MUST work on macOS (Darwin) and Linux. `creack/pty` and `unix.Mkfifo` are available on both. Windows is explicitly out of scope; the POSIX-only code lives in `_unix.go` files (build tag `//go:build unix`) and a `_windows.go` stub prints a clear error and exits 1. |
| NFR-07 | POSIX-only functionality (`creack/pty`, `golang.org/x/sys/unix`, `syscall` signals) MUST be isolated in build-tagged files (`interactive_unix.go` / `interactive_windows.go`) so the package still compiles on Windows and cross-compiles cleanly (GoReleaser matrix). |
| NFR-08 | No new heavyweight dependencies are introduced beyond `github.com/creack/pty` and `golang.org/x/sys/unix`; everything else (`os/exec`, `syscall`, `io`, `encoding/hex`, `encoding/json`) is Go standard library. |
| NFR-09 | The background reader goroutine MUST NOT block TAG process exit; it observes a cancellable `context.Context` and the process closes the master/stdout fd on shutdown, so a Ctrl-C at the TAG CLI level unwinds cleanly. |
| NFR-10 | `tag sandbox ps` MUST complete in under 500 ms for up to 100 rows, including the liveness check via `syscall.Kill(pid, 0)` for each row. |

---

## 10. Technical Design

### 10.1 Schema Changes

The existing `sandbox_runs` table in `~/.tag/runtime/tag.sqlite3` receives new nullable columns. The `EnsureSchema()` function in `internal/sandbox` (over the `modernc.org/sqlite` driver) applies these additions idempotently: each `ALTER TABLE ... ADD COLUMN` is executed and any `duplicate column name` error returned by the driver is treated as success (the standard guarded-migration idiom in `internal/store`), rather than pre-checking via `PRAGMA table_info`.

```go
// addColumn is idempotent: it swallows the "duplicate column name" error.
func addColumn(db *sql.DB, ddl string) error {
	_, err := db.Exec(ddl)
	if err != nil && strings.Contains(err.Error(), "duplicate column name") {
		return nil
	}
	return err
}
```

```sql
-- Applied by EnsureSchema() via addColumn(...) — idempotent:
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

The `pty_master_fd` column stores the integer file descriptor of the PTY master side. This descriptor is only meaningful within the process that created it (it is a file descriptor number in the current process's fd table), so it is stored as a convenience for the in-process reader goroutine but MUST NOT be used across process boundaries.

### 10.2 Core Types

```go
// internal/sandbox/interactive_unix.go
//go:build unix

package sandbox

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// AllowedSignals is validated before any syscall.Kill() call.
var AllowedSignals = map[string]syscall.Signal{
	"SIGTERM": syscall.SIGTERM,
	"SIGKILL": syscall.SIGKILL,
	"SIGINT":  syscall.SIGINT,
	"SIGHUP":  syscall.SIGHUP,
	"SIGUSR1": syscall.SIGUSR1,
	"SIGUSR2": syscall.SIGUSR2,
}

// InteractiveProcess represents a long-lived sandbox process spawned with --interactive.
type InteractiveProcess struct {
	ProcessID   string    // Unique ID (format: sp_<12hex>). Stored in sandbox_runs.id.
	PID         int       // OS process ID. Stored in sandbox_runs.pid.
	Backend     string    // Runtime backend ("restricted" or "docker").
	PTY         bool      // True if a PTY pair was allocated.
	Master      *os.File  // PTY master (nil if not PTY mode); creack/pty owns the slave.
	StdinFIFO   string    // Path to named FIFO ("" if PTY mode).
	ContainerID string    // Docker container ID ("" if restricted backend).
	Cmd         *exec.Cmd // process handle (nil if docker backend manages it).
	PTYRows     uint16    // Terminal rows at spawn time (default 24).
	PTYCols     uint16    // Terminal cols at spawn time (default 80).
}

// WriteResult is the result of a sandbox write operation.
type WriteResult struct {
	ProcessID    string    `json:"process_id"`
	BytesWritten int       `json:"bytes_written"`
	Timestamp    time.Time `json:"timestamp"`
}

// SignalResult is the result of a sandbox signal delivery.
type SignalResult struct {
	ProcessID   string        `json:"process_id"`
	PID         int           `json:"pid"`
	SignalName  string        `json:"signal"`
	SignalNum   int           `json:"signal_num"`
	Delivered   bool          `json:"delivered"`
	DeliveredAt time.Time     `json:"delivered_at"`
	Wait        bool          `json:"wait"`
	Exited      bool          `json:"exited"`
	ExitCode    *int          `json:"exit_code,omitempty"`
	Elapsed     time.Duration `json:"elapsed,omitempty"`
}
```

### 10.3 PTY Spawn Algorithm

`github.com/creack/pty` handles openpty, slave wiring, and slave-fd closing internally, so the Go version is markedly simpler than the Python `pty.openpty()` + `fcntl.ioctl` dance:

```go
// spawnPTYProcess starts command under a PTY. Returns (cmd, master, error).
// The caller reads from and writes to master; creack/pty owns the slave lifecycle.
func spawnPTYProcess(
	ctx context.Context,
	command []string,
	rows, cols uint16,
	env []string,
	workdir string,
) (*exec.Cmd, *os.File, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = env
	cmd.Dir = workdir
	// New session so the PTY becomes the controlling terminal; Setpgid enables
	// process-group signal fan-out (syscall.Kill(-pgid, sig)).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setpgid: true}

	master, err := pty.Start(cmd) // allocates PTY, wires child to slave, returns master
	if err != nil {
		return nil, nil, err
	}
	// Set the window size on the master immediately after start.
	if err := pty.Setsize(master, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return cmd, master, nil
}
```

### 10.4 FIFO Spawn Algorithm

```go
// spawnFIFOProcess starts command with a named FIFO for stdin.
// Returns (cmd, fifoPath, stdout, error). The FIFO persists for the process
// lifetime; a separate `tag sandbox write` invocation opens fifoPath to send input.
func spawnFIFOProcess(
	ctx context.Context,
	command []string,
	processID string,
	env []string,
	workdir string,
) (*exec.Cmd, string, io.ReadCloser, error) {
	fifoDir := filepath.Join(home(), ".tag", "runtime", "fifo")
	if err := os.MkdirAll(fifoDir, 0o700); err != nil {
		return nil, "", nil, err
	}
	fifoPath := filepath.Join(fifoDir, processID)
	_ = os.Remove(fifoPath) // clear any stale FIFO
	if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
		return nil, "", nil, err
	}

	// Open the read end non-blocking so the parent does not block before a
	// writer appears; hand it to the child as stdin.
	readFD, err := unix.Open(fifoPath, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", nil, err
	}
	readFile := os.NewFile(uintptr(readFD), fifoPath)

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = env
	cmd.Dir = workdir
	cmd.Stdin = readFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe() // stderr merged via cmd.Stderr = cmd.Stdout equivalent
	if err != nil {
		return nil, "", nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, "", nil, err
	}
	_ = readFile.Close() // child inherited the fd; close the parent copy

	return cmd, fifoPath, stdout, nil
}
```

### 10.5 Background stdout Reader Goroutine

```go
// startStdoutReader launches a goroutine that forwards process output to the
// terminal. For PTY mode, src is the master *os.File; for FIFO mode it is the
// cmd.StdoutPipe reader. onExit runs when EOF is reached (FIFO cleanup, status update).
func startStdoutReader(src io.Reader, processID string, onExit func(processID string)) {
	go func() {
		defer onExit(processID)
		buf := make([]byte, 4096)
		// io.CopyBuffer streams without waiting for full lines (NFR-02);
		// a read error / EOF (master closed => child exited) ends the loop.
		_, _ = io.CopyBuffer(os.Stdout, src, buf)
	}()
}
```

The goroutine holds no reference that would block `os.Exit`; on TAG-level Ctrl-C the fd is closed and `io.CopyBuffer` returns, running `onExit` (NFR-09).

### 10.6 Signal Delivery Implementation

```go
// DeliverSignal delivers a POSIX signal to a running interactive sandbox process.
func DeliverSignal(
	ctx context.Context,
	db *sql.DB,
	processID, signalName string,
	wait bool,
	waitTimeout time.Duration,
) (SignalResult, error) {
	nameUpper := strings.ToUpper(signalName)
	sig, ok := AllowedSignals[nameUpper]
	if !ok {
		return SignalResult{}, fmt.Errorf(
			"signal %q is not in the allowed set: %s",
			signalName, strings.Join(allowedNames(), " "))
	}

	var pid int
	var status string
	err := db.QueryRowContext(ctx,
		"SELECT pid, status FROM sandbox_runs WHERE id = ?", processID).
		Scan(&pid, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return SignalResult{}, fmt.Errorf("process %q not found", processID)
	} else if err != nil {
		return SignalResult{}, err
	}

	delivered := false
	if err := syscall.Kill(pid, sig); err == nil {
		delivered = true
	} else if errors.Is(err, syscall.ESRCH) {
		// Already exited: record stopped and treat as success.
		_, _ = db.ExecContext(ctx,
			"UPDATE sandbox_runs SET status='stopped', completed_at=? WHERE id=?",
			time.Now().UTC(), processID)
	} else {
		return SignalResult{}, err
	}

	now := time.Now().UTC()
	appendAudit(auditRecord{
		Event: "signal_deliver", ProcessID: processID, PID: pid,
		SignalName: nameUpper, SignalNum: int(sig), Timestamp: now, Delivered: delivered,
	})

	res := SignalResult{
		ProcessID: processID, PID: pid, SignalName: nameUpper,
		SignalNum: int(sig), Delivered: delivered, DeliveredAt: now, Wait: wait,
	}

	if wait && delivered {
		wctx, cancel := context.WithTimeout(ctx, waitTimeout)
		defer cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		start := time.Now()
	poll:
		for {
			select {
			case <-wctx.Done():
				break poll
			case <-ticker.C:
				if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
					res.Exited = true
					if code, ok := reapExitCode(processID); ok {
						res.ExitCode = &code
					}
					res.Elapsed = time.Since(start)
					break poll
				}
			}
		}
		if !res.Exited {
			// Escalate to SIGKILL.
			_ = syscall.Kill(pid, syscall.SIGKILL)
			appendAudit(auditRecord{
				Event: "signal_deliver", ProcessID: processID, PID: pid,
				SignalName: "SIGKILL", SignalNum: int(syscall.SIGKILL),
				Timestamp: time.Now().UTC(), Delivered: true, Escalated: true,
			})
		}
	}

	return res, nil
}
```

For a child started with `Setpgid`, `syscall.Kill(-pid, sig)` fans the signal out to the whole process group (the child plus anything it spawned), which is the correct default for shutting down a REPL that forked helpers.

### 10.7 stdin Write Implementation

```go
// WriteStdin writes text (or raw bytes) to a running interactive process's stdin.
func WriteStdin(
	ctx context.Context,
	db *sql.DB,
	processID string,
	data []byte, // already escape-interpreted / hex-decoded by the caller
) (WriteResult, error) {
	var (
		pid       int
		fifoPath  sql.NullString
		masterFD  sql.NullInt64
		status    string
	)
	err := db.QueryRowContext(ctx,
		"SELECT pid, stdin_fifo, pty_master_fd, status FROM sandbox_runs WHERE id=?", processID).
		Scan(&pid, &fifoPath, &masterFD, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return WriteResult{}, fmt.Errorf("process %q not found", processID)
	} else if err != nil {
		return WriteResult{}, err
	}
	if status != "running" {
		return WriteResult{}, fmt.Errorf("process %q is not running (status: %q)", processID, status)
	}

	var written int
	switch {
	case fifoPath.Valid && fifoPath.String != "":
		// FIFO-based stdin: open the write end non-blocking with a 5s deadline.
		fd, err := unix.Open(fifoPath.String, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return WriteResult{}, err
		}
		f := os.NewFile(uintptr(fd), fifoPath.String)
		defer f.Close()
		_ = f.SetWriteDeadline(time.Now().Add(5 * time.Second))
		written, err = f.Write(data)
		if os.IsTimeout(err) {
			return WriteResult{}, fmt.Errorf("FIFO write timed out after 5s for %q", processID)
		} else if err != nil {
			return WriteResult{}, err
		}
	case masterFD.Valid:
		// PTY master fd is only valid in the spawning process — look it up
		// from the in-process registry.
		proc, ok := registryGet(processID)
		if !ok || proc.Master == nil {
			return WriteResult{}, fmt.Errorf(
				"PTY master fd for %q is not available in this process; "+
					"interactive PTY processes must be written from the same TAG "+
					"process that spawned them", processID)
		}
		written, err = proc.Master.Write(data)
		if err != nil {
			return WriteResult{}, err
		}
	}

	now := time.Now().UTC()
	appendAudit(auditRecord{
		Event: "stdin_write", ProcessID: processID, BytesWritten: written, Timestamp: now,
	})
	return WriteResult{ProcessID: processID, BytesWritten: written, Timestamp: now}, nil
}

// interpretEscapes decodes C-style escape sequences (\n \t \r \x?? \\).
// strconv.Unquote reuses Go's own escape grammar, which is a superset of the C set.
func interpretEscapes(s string) (string, error) {
	return strconv.Unquote(`"` + strings.ReplaceAll(s, `"`, `\"`) + `"`)
}
```

### 10.8 In-Process Registry

Because PTY master file descriptors are only valid within the process that created them, a package-level map guarded by a mutex maps `processID → *InteractiveProcess`. This is not persisted to SQLite (except for the integer fd value, stored for reference) and is re-populated on `tag sandbox run --interactive`.

```go
// Package-level registry — populated by RunInteractive().
var (
	registryMu sync.Mutex
	registry   = map[string]*InteractiveProcess{}
)

func registrySet(p *InteractiveProcess) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[p.ProcessID] = p
}

func registryGet(id string) (*InteractiveProcess, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	p, ok := registry[id]
	return p, ok
}
```

### 10.9 Audit Log Appender

```go
type auditRecord struct {
	Event        string    `json:"event"`
	ProcessID    string    `json:"process_id"`
	PID          int       `json:"pid,omitempty"`
	SignalName   string    `json:"signal_name,omitempty"`
	SignalNum    int       `json:"signal_num,omitempty"`
	BytesWritten int       `json:"bytes_written,omitempty"`
	Delivered    bool      `json:"delivered,omitempty"`
	Escalated    bool      `json:"escalated,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
}

// appendAudit appends a JSONL record to the sandbox audit log. Cross-process
// safe via gofrs/flock (the project-standard file lock; also fixes the Windows
// no-op that fcntl.flock had).
func appendAudit(rec auditRecord) {
	auditPath := filepath.Join(home(), ".tag", "runtime", "sandbox-audit.jsonl")
	_ = os.MkdirAll(filepath.Dir(auditPath), 0o700)
	line, _ := json.Marshal(rec) // no HTML escaping needed; newlines in strings are JSON-escaped

	lock := flock.New(auditPath + ".lock")
	_ = lock.Lock()
	defer lock.Unlock()

	f, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
```

### 10.10 Docker Backend Integration

For Docker interactive mode, `runDockerInteractive()` replaces the batch docker path, using the `docker/moby` API client (no shelling out): a detached keepalive container plus a hijacked exec stream for stdin/stdout.

```go
import (
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// runDockerInteractive starts an interactive Docker container via the moby client.
// Returns (containerID, hijacked stream, error). The stream's Conn is the stdin
// writer; its Reader carries multiplexed stdout/stderr.
func runDockerInteractive(
	ctx context.Context,
	cli *client.Client,
	command []string,
	image string,
	usePTY bool,
) (string, types.HijackedResponse, error) {
	// Create + start a detached container with a keepalive command.
	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image: image,
		Cmd:   []string{"tail", "-f", "/dev/null"}, // keepalive
	}, &container.HostConfig{
		NetworkMode: "none",
		Resources:   container.Resources{Memory: 512 << 20, NanoCPUs: 1_000_000_000},
	}, nil, nil, "tag-sandbox-"+randHex(8))
	if err != nil {
		return "", types.HijackedResponse{}, err
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", types.HijackedResponse{}, err
	}

	// Exec the real command with an attached, hijacked stdin/stdout stream.
	execResp, err := cli.ContainerExecCreate(ctx, created.ID, types.ExecConfig{
		Cmd:          command,
		Tty:          usePTY, // -t equivalent
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", types.HijackedResponse{}, err
	}
	hj, err := cli.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{Tty: usePTY})
	if err != nil {
		return "", types.HijackedResponse{}, err
	}
	// hj.Conn is the stdin writer; hj.Reader is stdout/stderr (stdcopy-demultiplexed
	// when Tty is false). Signals to the container use cli.ContainerKill (see NG7).
	return created.ID, hj, nil
}
```

### 10.11 New Files

The interactive functionality is added under the existing `internal/sandbox` package, split by platform for cross-compilation (NFR-07):

- `internal/sandbox/interactive_unix.go` (`//go:build unix`) — PTY/FIFO spawn, reader goroutine, signal delivery, write, registry.
- `internal/sandbox/interactive_windows.go` (`//go:build windows`) — stub that returns "not supported on Windows".
- `internal/sandbox/interactive_docker.go` — moby-client interactive path.
- `internal/sandbox/interactive_test.go` — unit + integration tests.

The FIFO directory `~/.tag/runtime/fifo/` is created at runtime.

### 10.12 CLI Integration Points (cobra)

Three new leaf commands are registered under the `tag sandbox` cobra group in `internal/cli`:

- `newSandboxWriteCmd()` — parses `tag sandbox write <id> <text>` (`--file`, `--hex`, `--raw`, `--no-newline`) and calls `sandbox.WriteStdin()`.
- `newSandboxSignalCmd()` — parses `tag sandbox signal <id> <name>` (`--signum`, `--wait`, `--wait-timeout`) and calls `sandbox.DeliverSignal()`.
- `newSandboxPsCmd()` — parses `tag sandbox ps` (`--all`, `--json`) and calls `sandbox.ListInteractive()`.

The existing `tag sandbox run` command gains `--interactive`, `--pty`, `--pty-rows`, `--pty-cols` flags; its `RunE` branches to `sandbox.RunInteractive()` when `--interactive` is set, otherwise `sandbox.RunSandbox()`.

---

## 11. Security Considerations

1. **Signal allowlist enforcement.** `syscall.Kill(pid, sig)` with an unrestricted signal name would allow callers to send arbitrary signals to arbitrary PIDs if the PID lookup were wrong. The allowlist (SIGTERM, SIGKILL, SIGINT, SIGHUP, SIGUSR1, SIGUSR2) is validated before any `syscall.Kill()` call. The PID is never supplied by the user; it is looked up from SQLite using the opaque process ID.

2. **PID ownership verification.** Before calling `syscall.Kill(pid, sig)`, verify that the `pid` in `sandbox_runs` belongs to the current user via `/proc/<pid>/status` (Linux) or `os/user` + `ps -o uid= -p <pid>` (macOS). This prevents a race condition where a sandbox process exits and its PID is reused by an unrelated system process before the signal call.

3. **FIFO permissions.** Named FIFOs are created with `unix.Mkfifo(path, 0o600)`. Only the owning user can read or write the FIFO. The FIFO directory `~/.tag/runtime/fifo/` is created with `os.MkdirAll(..., 0o700)`.

4. **Input size cap on `sandbox write`.** Writes larger than 1 MB are rejected before the FIFO open, with a clear error. This prevents a caller from buffering arbitrarily large data in a kernel FIFO buffer (Linux FIFO capacity is typically 65536 bytes; writes beyond that block).

5. **No shell interpretation of `--code`.** The `--code` argument to `tag sandbox run --interactive` is tokenized (e.g. `github.com/google/shlex.Split`) and passed as an `[]string` argv to `exec.CommandContext`. It is never run through a shell (`sh -c`). This prevents shell injection even if the `--code` string contains shell metacharacters.

6. **PTY slave fd closure.** `creack/pty` (`pty.Start`) closes the slave fd in the parent once the child inherits it, so EOF propagates to the master when the child exits and the reader goroutine unwinds. Manual `os.close` of the slave is not required (a correctness improvement over the hand-managed Python fds).

7. **Audit log integrity.** The audit log uses `gofrs/flock` (advisory file lock) to prevent torn writes under concurrent signal or write commands — also closing the Windows no-op that `fcntl.flock` had. Entries are single-line `json.Marshal` output; newlines within field values are JSON-escaped.

8. **Docker container naming.** Container names are generated with `randHex(8)` (`crypto/rand`) to prevent collisions. Containers are labelled `tag.process_id=<id>` (via `container.Config.Labels`) for identification with the moby client's `ContainerList` label filter. Containers are stopped (`ContainerStop`/`ContainerRemove`), not just exec-killed, when `SIGKILL` is delivered to the Docker backend.

9. **Zombie prevention.** The reader goroutine calls `cmd.Wait()` after the read loop exits to reap the child and prevent a zombie. For the moby-managed container path, the container is removed on exit; there is no host-side zombie because the exec client process is `Wait`ed.

10. **Cross-process PTY fd safety.** The `pty_master_fd` integer is stored in SQLite for reference, but `WriteStdin()` only uses the fd if the calling process is the one that spawned the interactive process (checked via the in-process `registry`). Cross-process PTY writes return a clear error rather than attempting to use an invalid fd number.

---

## 12. Testing Strategy

Tests use Go's `testing` package with table-driven cases and inject dependencies (the store `*sql.DB`, a fake docker client) rather than monkeypatching. POSIX-only tests carry `//go:build unix`.

### 12.1 Unit Tests

Location: `internal/sandbox/interactive_test.go`

| Test | Description |
|------|-------------|
| `TestSignalAllowlistAcceptsValid` | Table over all 6 allowed signal names; verifies no error. |
| `TestSignalAllowlistRejectsInvalid` | Table over `["SIGPIPE", "SIGCHLD", "SIGABRT", "", "bad", "9"]`; verifies an error is returned before any `syscall.Kill`. |
| `TestInterpretEscapesNewline` | `interpretEscapes("hello\\nworld")` == `"hello\nworld"`. |
| `TestInterpretEscapesHex` | `interpretEscapes("\\x41")` == `"A"`. |
| `TestInterpretEscapesRawPassthrough` | `--raw` path leaves `"\\n"` literal (no decode). |
| `TestEnsureSchemaAddsColumns` | Opens an in-memory `modernc.org/sqlite` db with the original schema, calls `EnsureSchema()`, verifies `pid`/`stdin_fifo` columns exist and a second call is a no-op (duplicate-column error swallowed). |
| `TestSpawnPTYSetsWinsize` | Starts a trivial child with rows=40,cols=120; asserts `pty.GetsizeFull(master)` reflects it. |
| `TestWriteStdinRejectsStoppedProcess` | Inserts a row with `status='stopped'`; calls `WriteStdin()`; expects an error. |
| `TestWriteStdinRejectsOversizedInput` | Passes a 1.1 MB payload; expects a size-cap error. |
| `TestAuditLogAppendedOnSignal` | Calls `DeliverSignal()` against a self-owned test process; reads `sandbox-audit.jsonl`; verifies the record is present and valid JSON. |

### 12.2 Integration Tests

Location: `internal/sandbox/interactive_integration_test.go`

| Test | Description |
|------|-------------|
| `TestInteractivePythonREPLEcho` | Spawns `python3 -c "import sys; [print(l.strip()) for l in sys.stdin]"` in interactive mode; writes `"hello\n"`; reads stdout; asserts `"hello"` appears. |
| `TestPTYIsattyTrue` | Spawns `python3 -c "import sys; print(sys.stdin.isatty())"` with `--pty`; captures output; asserts `"True"`. |
| `TestSIGTERMStopsSleep` | Spawns `sleep 3600` in interactive mode; delivers SIGTERM; waits 1s; asserts `status='stopped'` in SQLite. |
| `TestSIGTERMWaitEscalatesToSIGKILL` | Spawns a process that ignores SIGTERM (`trap '' TERM; sleep 3600`); delivers SIGTERM with `--wait --wait-timeout 2`; asserts SIGKILL is delivered and the process exits. |
| `TestFIFOCleanupOnProcessExit` | Spawns a `cat` process; sends EOF (`\x04`); waits for exit; asserts the FIFO file no longer exists. |
| `TestDockerInteractiveBash` | `t.Skip` unless Docker is reachable; spawns `bash` via the moby client; writes `echo hello`; reads output; asserts `"hello"`. |
| `TestPsListsRunningOnly` | Spawns two processes; kills one; calls `ListInteractive()`; asserts only the live one appears. |
| `TestPsCleansZombies` | Inserts a fake `running` row with a non-existent PID; calls `ListInteractive()`; asserts that row's status is updated to `stopped`. |

### 12.3 Performance / Benchmarks

Location: `internal/sandbox/interactive_bench_test.go`

| Benchmark | Target |
|------|--------|
| `BenchmarkWriteLatencyP50` | Median round-trip for `WriteStdin()` + stdout echo < 100 ms over 50 iterations |
| `BenchmarkSignalDeliveryTime` | `DeliverSignal(SIGTERM)` returns in < 100 ms (excluding `--wait` polling) |
| `BenchmarkPsUnderLoad` | `ListInteractive()` with 100 synthetic rows completes in < 500 ms |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag sandbox run --interactive --code "python3" --pty` prints a `Process ID: sp_...` line and returns to the shell prompt within 2 seconds. | Integration test `TestInteractivePythonREPLEcho` |
| AC-02 | `tag sandbox write <id> "print(42)\n"` causes `42` to appear on stdout within 200 ms. | Integration test with timing assertion |
| AC-03 | `tag sandbox signal <id> SIGTERM` on a `sleep 3600` process causes the process to exit and `sandbox_runs.status` to become `stopped` within 1 second. | Integration test `TestSIGTERMStopsSleep` |
| AC-04 | `tag sandbox signal <id> SIGKILL` on a process that ignores SIGTERM causes unconditional exit. | Integration test `TestSIGTERMWaitEscalatesToSIGKILL` |
| AC-05 | `tag sandbox run --pty` without `--interactive` exits with code 1 and prints `"error: --pty requires --interactive"`. | Unit test |
| AC-06 | `tag sandbox signal <id> SIGPIPE` exits with code 1 and prints the allowlist error message without calling `syscall.Kill()`. | Unit test `TestSignalAllowlistRejectsInvalid` |
| AC-07 | `python3 -c "import sys; print(sys.stdin.isatty())"` prints `True` when launched with `--pty`, `False` when launched without. | Integration test `TestPTYIsattyTrue` |
| AC-08 | Every `tag sandbox write` call appends exactly one JSONL record with `event="stdin_write"` to `sandbox-audit.jsonl`. | Integration test audit log assertion |
| AC-09 | Every `tag sandbox signal` call appends exactly one JSONL record with `event="signal_deliver"` to `sandbox-audit.jsonl`. | Integration test audit log assertion |
| AC-10 | FIFO file under `~/.tag/runtime/fifo/<id>` is deleted after the process exits. | Integration test `TestFIFOCleanupOnProcessExit` |
| AC-11 | `tag sandbox ps` shows only `status='running'` rows by default; `--all` shows all rows. | Unit test |
| AC-12 | `tag sandbox ps` updates stale `running` rows to `stopped` for PIDs that no longer exist. | Integration test `TestPsCleansZombies` |
| AC-13 | `EnsureSchema()` adds `pid` and `stdin_fifo` columns to an existing `sandbox_runs` table without data loss, and is idempotent on re-run (duplicate-column error swallowed). | Unit test `TestEnsureSchemaAddsColumns` |
| AC-14 | `tag sandbox write <id>` returns an error if the process is in `stopped` or `failed` state. | Unit test `TestWriteStdinRejectsStoppedProcess` |
| AC-15 | `tag sandbox signal <id> SIGTERM --wait --wait-timeout 10` delivers SIGKILL automatically if the process does not exit within 10 seconds, and records the escalation in the audit log. | Integration test `TestSIGTERMWaitEscalatesToSIGKILL` |
| AC-16 | Docker backend interactive mode (`--backend docker`) passes integration test `TestDockerInteractiveBash` when Docker is available. | CI matrix conditional on Docker |
| AC-17 | Attempting interactive mode on Windows exits with code 1 and prints `"error: interactive sandbox mode is not supported on Windows"`. | The `interactive_windows.go` stub is the compile-time guarantee; test asserts the error on `runtime.GOOS == "windows"` |

---

## 14. Dependencies

| Dependency | Version / Notes | Required? | Install |
|------------|----------------|-----------|---------|
| `github.com/creack/pty` | latest (POSIX only) | Required for `--pty` (openpty, `pty.Start`, `pty.Setsize`) | `go get` |
| `golang.org/x/sys/unix` | latest (POSIX only) | Required for `Mkfifo`, FIFO open flags, syscall constants | `go get` |
| `syscall` (stdlib) | Go stdlib | `Kill`, `SysProcAttr{Setpgid,Setsid}`, signal constants | Built in |
| `os/exec` (stdlib) | Go stdlib | `CommandContext`, `Start`, `Wait`, `StdoutPipe` | Built in |
| `io` / `encoding/hex` / `encoding/json` / `strconv` (stdlib) | Go stdlib | stream copy, `--hex` decode, audit JSONL, escape decode | Built in |
| `github.com/gofrs/flock` | latest | Cross-platform advisory lock for the audit log | `go get` (already project dep) |
| `github.com/docker/docker` (moby client) | latest | Docker backend: `ContainerCreate`/`ExecAttach`/`ContainerKill` | `go get` (already project dep) |
| `modernc.org/sqlite` | project-wide | `sandbox_runs` store (CGO_ENABLED=0) | Already project driver |
| Docker daemon | >= 20.10 | Optional; Docker backend only | System package |
| PRD-028 `sandbox_runs` table | Existing schema | Required | Already in `internal/sandbox` |
| PRD-013 tracing patterns | OTel span conventions | Informational only | Already in `internal/obs` |
| PRD-034 security patterns | Blocked patterns | Informational only | Already in `internal/obs` |

---

## 15. Open Questions

| ID | Question | Owner | Status |
|----|----------|-------|--------|
| OQ-01 | Should `tag sandbox write` block waiting for the process to consume the input, or should it be fire-and-forget? The current design is fire-and-forget (write to FIFO and return). Blocking would give better backpressure semantics but complicates the CLI UX. | Architecture | Open |
| OQ-02 | Should SIGWINCH be propagated when the host terminal is resized? This would require an `os/signal.Notify(ch, syscall.SIGWINCH)` handler in TAG's main process, a `pty.Setsize(master, ...)` update, and `syscall.Kill(pid, syscall.SIGWINCH)`. Low priority but useful for full terminal emulation. | Engineering | Deferred |
| OQ-03 | For E2B backend, `process.send_stdin()` is available in the E2B SDK. Should we wire E2B interactive mode to use E2B's native API instead of local PTY? This would require a separate code path but would enable interactive sessions inside E2B cloud micro-VMs. | Product | Open |
| OQ-04 | Should `tag sandbox signal <id> SIGTERM --wait` be the default instead of requiring an explicit flag? Most users who send SIGTERM intend the two-phase shutdown. Making `--wait` the default (with `--no-wait` to opt out) might be more ergonomic. | UX | Open |
| OQ-05 | The `pty_master_fd` stored in SQLite is only valid within the spawning process. If a user restarts TAG and tries to `tag sandbox write` to a PTY process started in a previous TAG invocation, the write fails with a clear error. Is this acceptable, or should we implement a re-attach mechanism (e.g., re-opening the PTY master via `os.OpenFile("/proc/<pid>/fd/0", ...)` on Linux)? A cleaner long-term option is to make `tag serve` the single owner of interactive processes so `write`/`signal` are RPCs to it. | Architecture | Open |
| OQ-06 | For very long-running interactive sessions (hours), the background reader thread accumulates output in `sandbox_runs.output`. Should there be a rolling window (keep only last N bytes) to prevent the SQLite row from growing unboundedly? | Engineering | Open |
| OQ-07 | Should `tag sandbox write --hex 03` (Ctrl-C) be documented as the preferred way to send keyboard interrupt to a PTY process, rather than `tag sandbox signal <id> SIGINT`? Both work; `--hex 03` is more authentic to what a real terminal does (the line discipline converts it to SIGINT), while `syscall.Kill(pid, SIGINT)` bypasses the PTY line discipline. | UX / Documentation | Open |

---

## 16. Complexity and Timeline

**Total estimated effort: 3–5 days (S)**

### Phase 1 — Schema and Data Model (Day 1)

- Add `pid`, `stdin_fifo`, `pty_master_fd`, `pty_rows`, `pty_cols` columns in `EnsureSchema()` via idempotent `ALTER TABLE` + `duplicate column name` guard.
- Define `InteractiveProcess`, `WriteResult`, `SignalResult` structs.
- Define the `AllowedSignals` map.
- Define the mutex-guarded `registry` (package-level map).
- Write unit test for schema migration (`TestEnsureSchemaAddsColumns`).

### Phase 2 — Spawn and PTY Implementation (Day 2)

- Implement `spawnPTYProcess()` with `creack/pty` (`pty.Start`, `pty.Setsize`).
- Implement `spawnFIFOProcess()` with `unix.Mkfifo()` and `exec.CommandContext`.
- Implement `startStdoutReader()` goroutine (`io.CopyBuffer`).
- Implement `RunInteractive()` selecting PTY vs FIFO branch, recording in SQLite, starting the reader goroutine, populating the registry.
- Write integration tests: `TestPTYIsattyTrue`, `TestInteractivePythonREPLEcho`.

### Phase 3 — Write and Signal Commands (Day 3)

- Implement `WriteStdin()` with FIFO write path and PTY master-fd path.
- Implement `DeliverSignal()` with allowlist validation, `syscall.Kill`, and `--wait` polling (`time.Ticker` + `context.WithTimeout`).
- Implement `appendAudit()` with `gofrs/flock`.
- Implement `interpretEscapes()` and the `--hex` (`encoding/hex`) path.
- Write unit tests for signal allowlist, write rejection, audit log.
- Write integration tests: `TestSIGTERMStopsSleep`, `TestSIGTERMWaitEscalatesToSIGKILL`.

### Phase 4 — Docker Backend and `sandbox ps` (Day 4)

- Implement `runDockerInteractive()` with the moby client (`ContainerCreate`+`ContainerStart` / `ContainerExecAttach`).
- Implement `ListInteractive()` with zombie cleanup via `syscall.Kill(pid, 0)`.
- Register `newSandboxWriteCmd`, `newSandboxSignalCmd`, `newSandboxPsCmd` in the `internal/cli` sandbox cobra group.
- Extend the `tag sandbox run` `RunE` to branch on `--interactive`.
- Write integration test `TestDockerInteractiveBash` (Docker-conditional `t.Skip`).
- Write unit tests `TestPsListsRunningOnly`, `TestPsCleansZombies`.

### Phase 5 — Polish, FIFO Cleanup, and Performance (Day 5)

- Implement FIFO cleanup via `defer` in the reader goroutine and process-exit callback.
- Implement the Windows build-tagged stub (`interactive_windows.go`).
- Write benchmarks (`go test -bench`).
- Update `docs/prd/INDEX.md` with the PRD-098 entry.
- Manual end-to-end walkthrough: Python REPL, sqlite3 CLI, bash in Docker.
- Address any CI failures from the Docker matrix job.

