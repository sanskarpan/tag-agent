# PRD-088: Distributed Agent Runtime (gRPC Host/Worker for Cross-Machine Agents) (`tag runtime`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** XL (4-8 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `runtime.py`
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (security hardening), PRD-027 (eval framework), PRD-012 (cost tracking/budget), PRD-008 (background task queue)
**Inspired by:** AutoGen distributed runtime, Ray distributed actors, Celery workers

---

## 1. Overview

TAG agents today are tightly coupled to the machine and process that invokes them. Running `tag run --profile coder --prompt "..."` spawns an agent synchronously in the foreground of the calling shell. The agent's entire lifecycle — model calls, tool execution, context management — happens on the invoking machine. This architecture is simple and debuggable, but it fundamentally limits horizontal scalability: you cannot distribute work across a fleet of specialized machines, you cannot dedicate a GPU-equipped host to embedding-heavy semantic memory lookups while a separate host handles tool execution, and you cannot submit agent tasks from a CI pipeline to a long-lived worker pool without ad-hoc shell hacks.

PRD-088 introduces the **Distributed Agent Runtime**: a gRPC-based host/worker architecture that allows TAG agents to run across multiple machines in a coordinated cluster. A **host** process exposes a well-known gRPC endpoint that accepts task submissions, manages a registry of connected workers, schedules tasks to workers based on profile affinity and current load, and streams status events back to callers. **Workers** connect outbound to the host, declare which profiles they support, and execute tasks using their locally-configured TAG installation — including local MCP servers, sandboxes, semantic memory indexes, and tool retrievers. Workers report incremental status (thinking, tool calls, partial outputs) back to the host via bidirectional gRPC streams, and the host fans these events out to any subscriber watching the task.

The design is explicitly inspired by three mature distributed task systems. **AutoGen's distributed runtime** (v0.4+) separates agent logic from the communication substrate, using gRPC message passing so agents on different hosts can send and receive messages without knowing each other's location. **Ray distributed actors** treat each long-running stateful object (here: a worker's loaded profile context and tool singletons) as a remote actor that can be addressed by stable identity. **Celery workers** pioneered the broker-mediated task queue pattern where workers pull tasks matching their declared capabilities — here translated into gRPC streaming subscriptions rather than AMQP queues, eliminating an external broker dependency entirely.

TAG's SQLite state store (`~/.tag/runtime/tag.sqlite3`) gains two new tables: `runtime_tasks` tracking submitted tasks with their lifecycle state, and `runtime_workers` caching the worker registry seen by each host. The host's gRPC server is implemented in `src/tag/runtime.py` using `grpcio` and a hand-rolled proto that avoids heavy proto compilation toolchains by using `grpcio-reflection` and runtime service descriptors. The CLI surface is `tag runtime host start`, `tag runtime worker start`, `tag runtime status`, and `tag submit --runtime grpc://...`.

This feature is categorized P3 (nice-to-have) because TAG's primary deployment model — single-user, single-machine — does not require distributed infrastructure. The feature targets platform engineers building internal AI automation pipelines who need to burst agent workloads across a small cluster (2–10 machines) without adopting a full-weight orchestration platform like Ray or Kubernetes. The implementation must not increase startup latency or import overhead for the common single-machine case: all gRPC imports are deferred behind the `tag runtime` subcommand.

---

## 2. Problem Statement

### 2.1 Agents Are Vertically Constrained to a Single Machine

Every TAG task today competes for CPU, RAM, and API-rate-limit headroom on one machine. A busy developer running `tag kanban process` while also iterating on a profile via `tag eval run` saturates their local anthropic API concurrency. Organizations that want to run multiple concurrent agent pipelines — nightly eval regressions, PR review automation, background documentation generation — either queue them serially on one machine (slow) or independently manage multiple TAG installations with no coordination (operationally fragile). There is no mechanism to say "send this task to the machine with the GPU for embedding, and that task to the cloud VM with the GitHub MCP server."

### 2.2 No Task Submission API for Headless / CI Environments

CI pipelines and webhooks need to submit agent tasks without an interactive TTY and without blocking the CI runner waiting for the task to complete. PRD-008's background queue runs tasks on the same machine where they are submitted. There is no way to submit a task to a remote machine from a GitHub Actions runner and poll for results. This forces teams to build ad-hoc SSH wrappers or HTTP polling loops around `tag run`, which have no structured status reporting, no retry semantics, and no streaming output.

### 2.3 Profile Specialization Demands Hardware Heterogeneity

The `coder` profile benefits from fast disk I/O and high-memory local MCP filesystem tools. The `researcher` profile benefits from a large SentenceTransformer model loaded into RAM (tool_retrieval.py). The `analyst` profile may need a GPU for local embedding inference. No single developer workstation optimally serves all three profiles simultaneously. A distributed runtime allows each worker to be started on hardware matched to the profiles it declares — a Mac Studio for the coder worker, a GPU server for the researcher worker — while the host dispatches tasks based on declared profile affinity.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Implement a gRPC host server (`tag runtime host start`) that accepts task submissions, maintains a worker registry, schedules tasks to affinity-matched workers, and streams task events to subscribers. |
| G2 | Implement a gRPC worker process (`tag runtime worker start`) that connects outbound to a host, declares supported profiles, pulls assigned tasks, executes them via the existing TAG agent infrastructure, and streams results back. |
| G3 | Enable headless task submission (`tag submit --runtime grpc://host:port --profile <p> --prompt "..."`) that returns a task ID immediately and optionally streams or polls status. |
| G4 | Implement `tag runtime status` to show connected workers, queued/running/completed tasks, and per-worker load metrics, with `--json` for machine consumption. |
| G5 | Persist task state and worker registry snapshots in `runtime_tasks` and `runtime_workers` SQLite tables on the host, enabling status queries even after a host restart. |
| G6 | All gRPC imports are lazy: `import grpc` and proto-generated stubs are imported only when `tag runtime` or `tag submit --runtime grpc://` is invoked. The common single-machine path has zero gRPC overhead. |
| G7 | Workers authenticate to the host via a shared bearer token (configured in cli-config.yaml or `--token` flag) to prevent unauthorized task submission or worker registration. |
| G8 | Task results (final output, cost, token usage) are stored in `runtime_tasks` and are retrievable via `tag runtime task get <task-id>` after the task completes. |
| G9 | The host exposes a Prometheus `/metrics` HTTP endpoint (on a separate port, default 9090) with counters for tasks submitted, tasks completed, tasks failed, and worker count. |
| G10 | Workers inherit their local TAG config (profiles, MCP servers, budget limits) and do not require separate configuration beyond the host address and token. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | **Automatic worker auto-scaling.** Spinning up new cloud VMs or containers is out of scope. Workers must be started manually or by external infra tooling. |
| NG2 | **Message broker replacement for Celery.** This is a direct gRPC streaming implementation, not a general-purpose task queue. Applications requiring AMQP, Redis, or SQS semantics should use those systems. |
| NG3 | **Multi-tenant isolation.** All workers on a host share the same auth token. Per-user or per-org isolation within a single cluster is out of scope. |
| NG4 | **Automatic failover and leader election.** There is exactly one host; host high-availability (e.g., hot standby) is not covered. |
| NG5 | **Distributed tracing correlation across host and worker.** While each task gets a trace ID, stitching host-side and worker-side OTel spans into a single distributed trace (W3C TraceContext propagation) is a future enhancement. |
| NG6 | **gRPC as a replacement for A2A/ACP agent-to-agent protocols.** This runtime is for TAG-internal task distribution. Protocol bridging to A2A or ACP endpoints is handled by separate PRDs in Cluster E. |
| NG7 | **Windows support for the host process.** The host uses `SO_REUSEPORT` and `signal.SIGTERM` handling that are POSIX-specific. Worker support on Windows may work but is untested. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Host startup latency | `tag runtime host start` serving within 2 seconds | `time tag runtime host start &; sleep 2; grpc_health_probe` |
| Worker connect time | Worker registers with host within 3 seconds of starting | Integration test with subprocess timing |
| Task dispatch latency | Time from `tag submit` return to worker receiving task | <500 ms at p99 on local LAN | Prometheus histogram |
| Streaming lag | First streaming event from worker reaches `tag submit` caller | <200 ms after worker emits it | Integration test |
| gRPC import overhead | `import tag.controller` does not import `grpc` | `sys.modules` assertion in unit test |
| Task persistence | Tasks in `runtime_tasks` survive host restart and remain queryable | Integration test: kill host, restart, `tag runtime task get` |
| Auth rejection | Requests with wrong token rejected with gRPC `UNAUTHENTICATED` status code | Unit test |
| Prometheus metrics | `curl :9090/metrics` returns counters with correct values after tasks | Integration test |
| Concurrent task throughput | 10 concurrent tasks across 3 workers complete without deadlock | Load test |
| Worker profile affinity | Task submitted with `--profile coder` dispatched only to workers that declared `coder` | Integration test |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Platform engineer | run `tag runtime host start --port 50051` on a central server | I have a stable task-submission endpoint that CI pipelines and developers can target without managing individual machine addresses |
| U2 | Developer | run `tag runtime worker start --host grpc://myserver:50051 --profile coder` on my Mac Studio | My fast local machine contributes to the team's coding task pool without me needing to expose any inbound ports |
| U3 | CI engineer | run `tag submit --runtime grpc://host:50051 --profile reviewer --prompt "review PR #42"` from a GitHub Actions runner | The review task runs on a dedicated worker with the right GitHub MCP server, not on the ephemeral CI runner, and I get the result back as a JSON artifact |
| U4 | Team lead | run `tag runtime status --host grpc://host:50051 --json` from any machine | I can see worker health, queue depth, and running tasks in a dashboard without SSH-ing into the host server |
| U5 | Developer | run `tag submit --runtime grpc://host:50051 --stream --profile coder --prompt "..."` | I see agent thinking steps and tool call outputs in real time as they stream from the remote worker |
| U6 | Platform engineer | have tasks survive host restarts | A host restart for patching does not lose in-flight task state; workers reconnect and resume |
| U7 | Security engineer | set `runtime.auth_token` in cli-config.yaml | Unauthorized machines cannot register as workers or submit tasks to the cluster |
| U8 | Developer | run `tag runtime task get <task-id>` after a long-running task completes | I can retrieve the full output and cost attribution of a task that finished hours ago, even if I was not streaming at the time |
| U9 | Platform engineer | see a Prometheus metrics endpoint at `:9090/metrics` | I can wire the host into my existing Grafana dashboard alongside other infrastructure metrics |
| U10 | Developer | have worker processes reconnect automatically after a transient host restart | A brief host restart does not require manual worker restarts |

---

## 6. Proposed CLI Surface

### 6.1 `tag runtime host start`

Start the gRPC host server on the current machine.

```
tag runtime host start \
  [--port 50051] \
  [--metrics-port 9090] \
  [--token <bearer-token>] \
  [--db-path ~/.tag/runtime/tag.sqlite3] \
  [--max-workers-per-task 1] \
  [--dispatch-strategy affinity|round-robin|least-loaded] \
  [--tls-cert <path>] \
  [--tls-key <path>] \
  [--log-level info|debug|warning] \
  [--json]
```

**Options:**
- `--port`: gRPC listen port (default: 50051). Reads `runtime.host_port` from cli-config.yaml.
- `--metrics-port`: Prometheus HTTP metrics port (default: 9090). Set to 0 to disable.
- `--token`: Shared bearer token for worker/client auth. If not provided, reads `runtime.auth_token` from cli-config.yaml. If neither is set, a random UUID token is generated and printed on startup (one-time).
- `--db-path`: Override the SQLite path for task persistence.
- `--dispatch-strategy`: Task scheduling algorithm. `affinity` (default) assigns tasks to workers that declared the exact profile. `round-robin` cycles workers ignoring profile. `least-loaded` picks the worker with fewest in-progress tasks.
- `--tls-cert` / `--tls-key`: Enable TLS. Both must be provided together. Without TLS, the host uses insecure gRPC (appropriate for localhost or VPN-protected LANs).
- `--log-level`: Logging verbosity (default: info).

**Startup output:**
```
TAG Runtime Host starting...
  gRPC endpoint : 0.0.0.0:50051 (insecure)
  Metrics       : http://0.0.0.0:9090/metrics
  Auth token    : ••••••••••••••••••••••6f2a  (set RUNTIME_AUTH_TOKEN or --token to configure)
  DB path       : /Users/alice/.tag/runtime/tag.sqlite3
  Dispatch      : affinity
Ready. Waiting for workers and tasks.
```

Runs in the foreground. `Ctrl-C` / `SIGTERM` initiates graceful shutdown: stops accepting new tasks, waits up to 30 seconds for in-progress tasks to complete, then exits.

### 6.2 `tag runtime worker start`

Connect to a host and begin accepting tasks.

```
tag runtime worker start \
  --host grpc://host:50051 \
  [--profile coder] \
  [--profile researcher] \
  [--token <bearer-token>] \
  [--worker-id <id>] \
  [--concurrency 1] \
  [--tls-ca-cert <path>] \
  [--heartbeat-interval 10] \
  [--reconnect-backoff 5] \
  [--log-level info|debug] \
  [--json]
```

**Options:**
- `--host`: Host gRPC address in `grpc://host:port` format (required).
- `--profile`: Profiles this worker supports. Repeatable. If omitted, the worker declares support for all locally-configured profiles.
- `--token`: Auth token matching the host's token.
- `--worker-id`: Human-readable worker name (default: `<hostname>-<pid>`). Used in `tag runtime status` output.
- `--concurrency`: Maximum simultaneous tasks this worker will accept (default: 1).
- `--tls-ca-cert`: CA certificate for verifying the host's TLS cert.
- `--heartbeat-interval`: Seconds between heartbeat RPCs sent to the host (default: 10).
- `--reconnect-backoff`: Seconds to wait before reconnecting after host disconnect (default: 5, capped at 60 with exponential backoff).

**Startup output:**
```
TAG Runtime Worker starting...
  Host          : grpc://myserver:50051
  Worker ID     : mac-studio-3421
  Profiles      : coder, researcher
  Concurrency   : 1
  Heartbeat     : 10s
Connected to host. Waiting for tasks.
```

### 6.3 `tag runtime status`

Show cluster status from the host's perspective.

```
tag runtime status \
  [--host grpc://host:50051] \
  [--token <bearer-token>] \
  [--json] \
  [--watch]
```

**Options:**
- `--host`: Host address. Reads `runtime.default_host` from cli-config.yaml if omitted.
- `--watch`: Refresh the status table every 2 seconds (like `watch`).
- `--json`: Machine-readable output.

**Human-readable output:**
```
TAG Runtime Status — grpc://myserver:50051
Updated: 2026-06-17 14:23:01 UTC

Workers (2 connected):
  ID                  PROFILES              CONCURRENCY  IN-PROGRESS  STATUS
  mac-studio-3421     coder, researcher     1            1            healthy
  cloud-vm-7890       coder                 2            0            healthy

Tasks (last 10):
  TASK ID          PROFILE     STATUS      WORKER              STARTED              ELAPSED
  task-abc123      coder       running     mac-studio-3421     2026-06-17 14:22:51  0:00:10
  task-def456      researcher  queued      —                   2026-06-17 14:22:58  —
  task-ghi789      coder       completed   cloud-vm-7890       2026-06-17 14:20:01  0:01:43

Queue depth: 1  |  Running: 1  |  Completed today: 47  |  Failed today: 2
```

**JSON output (with `--json`):**
```json
{
  "host": "grpc://myserver:50051",
  "timestamp": "2026-06-17T14:23:01Z",
  "workers": [
    {
      "worker_id": "mac-studio-3421",
      "profiles": ["coder", "researcher"],
      "concurrency": 1,
      "in_progress": 1,
      "status": "healthy",
      "last_heartbeat": "2026-06-17T14:23:00Z"
    }
  ],
  "queue_depth": 1,
  "running": 1,
  "completed_today": 47,
  "failed_today": 2,
  "tasks": [...]
}
```

### 6.4 `tag submit --runtime`

Submit a task to a remote runtime host.

```
tag submit \
  --runtime grpc://host:50051 \
  --profile coder \
  --prompt "Refactor auth.py to use OAuth2 PKCE" \
  [--token <bearer-token>] \
  [--stream] \
  [--wait] \
  [--timeout 300] \
  [--files path/to/file1,path/to/file2] \
  [--env KEY=VALUE] \
  [--priority 0-9] \
  [--json] \
  [--output-file results.json]
```

**Options:**
- `--runtime`: Target host. When not prefixed with `grpc://`, falls back to local execution (backward-compatible).
- `--stream`: Stream task events (thinking steps, tool calls, partial outputs) to stdout in real time as they arrive from the worker.
- `--wait`: Block until the task reaches a terminal state. Implied by `--stream`.
- `--timeout`: Seconds before the submit command gives up waiting (default: never). Does not cancel the remote task; use `tag runtime task cancel <id>` for that.
- `--files`: Comma-separated local file paths to include as task context (uploaded as bytes in the task payload).
- `--env`: Additional environment variables to pass to the worker for this task.
- `--priority`: Integer 0–9 (default 5). Higher priority tasks are dispatched first within a worker's queue.

**Immediate output (without `--wait`):**
```
Task submitted.
  Task ID  : task-abc123
  Profile  : coder
  Host     : grpc://myserver:50051
  Status   : queued

Track: tag runtime task get task-abc123
Stream:  tag runtime task stream task-abc123
```

**Streaming output (with `--stream`):**
```
[task-abc123] [14:22:51] status: queued → running (worker: mac-studio-3421)
[task-abc123] [14:22:52] thinking: Analyzing auth.py for OAuth2 refactoring opportunities...
[task-abc123] [14:22:54] tool_call: read_file(path="auth.py")
[task-abc123] [14:22:55] tool_result: 247 lines read
[task-abc123] [14:23:10] thinking: Identified 3 places to replace password flow with PKCE...
[task-abc123] [14:23:44] status: running → completed
[task-abc123] [14:23:44] output:
  Refactored auth.py: replaced client_secret_basic with PKCE in 3 locations.
  Changes written to auth.py (diff: +42/-18 lines).
[task-abc123] [14:23:44] cost: $0.0032 | tokens: 2847 in / 612 out
```

### 6.5 `tag runtime task get`

Retrieve completed task details.

```
tag runtime task get <task-id> \
  [--host grpc://host:50051] \
  [--token <bearer-token>] \
  [--json]
```

### 6.6 `tag runtime task cancel`

Cancel a queued or running task.

```
tag runtime task cancel <task-id> \
  [--host grpc://host:50051] \
  [--token <bearer-token>]
```

### 6.7 `tag runtime task stream`

Stream events from an already-submitted task (attach to a running task).

```
tag runtime task stream <task-id> \
  [--host grpc://host:50051] \
  [--token <bearer-token>] \
  [--from-beginning]
```

`--from-beginning` replays all stored events from SQLite before following live events.

### 6.8 `tag runtime worker list`

List workers registered with the host (alias for the workers section of `tag runtime status`).

```
tag runtime worker list \
  [--host grpc://host:50051] \
  [--token <bearer-token>] \
  [--profile coder] \
  [--json]
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | **gRPC service definition:** `runtime.py` must define a `TagRuntimeService` with RPCs: `RegisterWorker(WorkerInfo) → stream WorkerCommand`, `SubmitTask(TaskRequest) → TaskAck`, `StreamTaskEvents(TaskRef) → stream TaskEvent`, `GetTaskResult(TaskRef) → TaskResult`, `CancelTask(TaskRef) → CancelAck`, `GetStatus(StatusRequest) → StatusResponse`, `Heartbeat(HeartbeatRequest) → HeartbeatAck`. All message types are defined as Python dataclasses and serialized to JSON bytes (no `.proto` compilation required). |
| FR-02 | **Worker registration:** Workers must call `RegisterWorker` with their ID, declared profiles, concurrency, hostname, and auth token on connect. The host must verify the token, register the worker in `runtime_workers`, and open a server-side streaming channel through which the host pushes `WorkerCommand` messages (task assignments, cancellations, drain instructions). |
| FR-03 | **Task submission:** `SubmitTask` accepts a `TaskRequest` (profile, prompt, files, env, priority, timeout_seconds) and synchronously writes a row to `runtime_tasks` with status `QUEUED`, returning a `TaskAck` with the assigned `task_id`. Dispatch to a worker is asynchronous and must not block the submit RPC response. |
| FR-04 | **Affinity dispatch:** The default scheduler selects the worker with the lowest current load ratio (`in_progress / concurrency`) among workers that declared the requested profile. Ties broken by worker registration order (FIFO). If no affinity-matching worker is available, the task stays `QUEUED` until one connects or becomes free. |
| FR-05 | **Round-robin and least-loaded strategies:** When `--dispatch-strategy round-robin` is set, workers cycle in registration order ignoring profile. When `least-loaded`, the worker with the lowest absolute `in_progress` count is selected regardless of profile. Both strategies still require at least one worker to be connected. |
| FR-06 | **Task event streaming:** Workers send `TaskEvent` messages back to the host via the bidirectional `RegisterWorker` stream. Each event has: `task_id`, `event_type` (status_change | thinking | tool_call | tool_result | partial_output | final_output | cost), `payload` (JSON string), and `timestamp`. The host stores events in `runtime_task_events` and fans them out to any active `StreamTaskEvents` subscribers. |
| FR-07 | **Replay stored events:** `StreamTaskEvents` must first replay all events stored in `runtime_task_events` for the requested `task_id` before forwarding live events, allowing late subscribers to reconstruct the full task history. |
| FR-08 | **Task state machine:** Tasks transition through: `QUEUED → DISPATCHED → RUNNING → [COMPLETED | FAILED | CANCELED]`. Illegal transitions (e.g., `COMPLETED → RUNNING`) must be rejected with a logged warning; the state in SQLite is not updated. |
| FR-09 | **Worker heartbeat:** Workers send `Heartbeat` RPCs at `--heartbeat-interval` seconds. The host marks a worker `UNHEALTHY` after 3 missed heartbeats (3× interval). Tasks assigned to an `UNHEALTHY` worker are re-queued with `QUEUED` status and re-dispatched to another worker. |
| FR-10 | **Auth token validation:** Every inbound RPC (from both workers and submit clients) must include a `Authorization: Bearer <token>` gRPC metadata header. The host must reject missing or mismatched tokens with gRPC status `UNAUTHENTICATED` (code 16). |
| FR-11 | **Task persistence on host:** `runtime_tasks` rows survive host restart. On startup, the host reads all non-terminal tasks and re-queues them with status `QUEUED`, setting `requeued_at` to the restart timestamp. Terminal tasks (`COMPLETED`, `FAILED`, `CANCELED`) are not re-queued. |
| FR-12 | **Worker reconnection:** Workers detect gRPC stream errors and reconnect using exponential backoff (base 5s, max 60s, jitter ±10%). On reconnect, the worker re-sends `RegisterWorker` and resumes from where it left off. If a task was `RUNNING` on the worker at reconnect time, the worker reports its current state in the `RegisterWorker` payload and the host reconciles. |
| FR-13 | **Graceful host shutdown:** On `SIGTERM`, the host stops accepting `SubmitTask` RPCs, sends `DrainCommand` to all workers, waits up to 30 seconds for in-progress tasks to complete, then sends `ShutdownCommand` and exits. In-progress tasks not completed within the 30s window are marked `FAILED` with reason `host_shutdown`. |
| FR-14 | **Prometheus metrics:** The host exposes the following metrics on the metrics port: `tag_runtime_tasks_submitted_total` (counter, labels: profile), `tag_runtime_tasks_completed_total` (counter, labels: profile, status), `tag_runtime_workers_connected` (gauge), `tag_runtime_task_dispatch_latency_seconds` (histogram), `tag_runtime_task_duration_seconds` (histogram, labels: profile). |
| FR-15 | **Cost attribution:** When a worker emits a `cost` event, the host updates `runtime_tasks.cost_usd` and `runtime_tasks.tokens_in` / `tokens_out`. `tag runtime task get` surfaces these fields. Cost data is also forwarded to the local budget module (PRD-012) on the host via `budget.record_spend`. |
| FR-16 | **`--files` payload:** Files specified in `tag submit --files` are read locally, base64-encoded, and included in the `TaskRequest.files` field (list of `{name: str, content_b64: str}`). The worker decodes them into a temporary directory and sets the working directory for the agent run. Maximum total file size: 10 MB. |
| FR-17 | **`--priority` scheduling:** Within a worker's incoming task queue, tasks with higher `priority` values (0–9, default 5) are dispatched before lower-priority ones. The host maintains per-worker priority queues using Python's `heapq`. |
| FR-18 | **`tag runtime task cancel` propagation:** If the task is `QUEUED`, it is immediately marked `CANCELED` in SQLite. If `DISPATCHED` or `RUNNING`, the host sends a `CancelCommand` to the holding worker; the worker emits a `status_change: CANCELED` event and terminates the agent subprocess. |
| FR-19 | **No proto compilation requirement:** The gRPC service uses `grpcio` with `grpc.experimental.aio` and `betterproto` (or raw channel descriptors) so that no `protoc` or `grpc_tools` invocation is required at install time. All message types are Python dataclasses. |
| FR-20 | **Worker-local TAG execution:** Workers execute tasks by calling the existing `controller.py` agent runner functions (same code path as `tag run`). No separate agent binary or container is required. The worker passes the `profile`, `prompt`, `files` working directory, and `env` overrides to the runner and captures streaming output via the existing Hermes callback mechanism. |
| FR-21 | **`tag runtime status --watch`:** When `--watch` is specified, the CLI re-polls the host via `GetStatus` every 2 seconds and re-renders the Rich table in-place (using Rich Live). |
| FR-22 | **Worker drain on SIGTERM:** When a worker receives `SIGTERM` or a `DrainCommand` from the host, it finishes any in-progress task, reports the result, and exits cleanly without accepting new assignments. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Zero import overhead on cold path.** `import tag.controller` must not import `grpc`, `betterproto`, or any runtime module. All runtime imports are inside functions decorated with `@_require_grpc` or inside `cmd_runtime_*` and `cmd_submit` handlers. Verified by unit test asserting `'grpc' not in sys.modules`. |
| NFR-02 | **Startup latency.** `tag runtime host start` must be ready to accept connections within 2 seconds on any machine with Python 3.10+ and `grpcio` installed. Measured from CLI invocation to first successful `grpc_health_probe` response. |
| NFR-03 | **Throughput.** A single host with three workers (each concurrency=2) must sustain 6 simultaneous tasks and successfully complete a batch of 100 sequential tasks without memory growth exceeding 50 MB on the host process. |
| NFR-04 | **Streaming latency.** Individual `TaskEvent` messages must propagate from worker to a `tag runtime task stream` subscriber in under 200 ms at p99 on a LAN (≤1 ms RTT). |
| NFR-05 | **SQLite WAL durability.** All task state transitions and event writes use WAL mode (consistent with existing TAG SQLite usage). A host crash mid-transition must not leave the database in an inconsistent state; the next startup reads committed state only. |
| NFR-06 | **Security.** The auth token must be at least 32 characters. Tokens shorter than 32 characters are rejected at startup with an actionable error. The host must log each auth failure with the source IP but must not log the token itself. |
| NFR-07 | **Graceful degradation.** If `grpcio` is not installed, `tag submit --runtime grpc://...` must print a clear install instruction (`pip install tag[runtime]`) and exit 1 without a Python traceback. All other `tag` commands are unaffected. |
| NFR-08 | **Python version compatibility.** `runtime.py` requires Python 3.10+ (for `match` statement on task state transitions and `asyncio.TaskGroup` for concurrent event dispatch). The existing package already requires Python 3.10+. |
| NFR-09 | **Log structured output.** All host and worker log lines are JSON-structured when `--log-level debug` is set: `{"ts": "<iso>", "level": "...", "event": "...", "task_id": "...", "worker_id": "..."}`. Human-readable by default. |
| NFR-10 | **TLS recommendation.** When TLS is not configured, the host prints a startup warning: `WARNING: gRPC endpoint is not TLS-protected. Use --tls-cert and --tls-key for production deployments.` |
| NFR-11 | **Idempotent worker registration.** If a worker reconnects with the same `worker_id`, the host updates the existing registry row rather than creating a duplicate. In-progress task assignments are reconciled based on the worker's reported state. |
| NFR-12 | **Maximum task payload.** Total `TaskRequest` payload (prompt + files) must not exceed 100 MB. The host rejects larger payloads with gRPC status `RESOURCE_EXHAUSTED` (code 8). |

---

## 9. Technical Design

### 9.1 New Files

- **`src/tag/runtime.py`** — All host and worker logic: gRPC server, client stubs, dispatch scheduler, task state machine, SQLite integration, Prometheus exporter, and CLI handler functions (`cmd_runtime_host_start`, `cmd_runtime_worker_start`, `cmd_runtime_status`, `cmd_runtime_task_get`, `cmd_runtime_task_cancel`, `cmd_runtime_task_stream`, `cmd_submit_remote`).
- **`src/tag/runtime_proto.py`** — Message dataclass definitions (no `.proto` file). Handles serialization/deserialization to JSON bytes for gRPC transport.

No new directories required. The `runtime.py` follows the same module pattern as `queue_worker.py` and `kanban.py`.

### 9.2 SQLite DDL

All tables live in the existing `~/.tag/runtime/tag.sqlite3` database, opened via `open_db()`.

```sql
-- Task registry maintained by the host.
CREATE TABLE IF NOT EXISTS runtime_tasks (
    task_id          TEXT PRIMARY KEY,           -- UUID v4 assigned at submit time
    profile          TEXT NOT NULL,
    prompt           TEXT NOT NULL,
    files_json       TEXT,                       -- JSON array of {name, content_b64}
    env_json         TEXT,                       -- JSON object of KEY=VALUE pairs
    priority         INTEGER NOT NULL DEFAULT 5, -- 0-9
    timeout_seconds  INTEGER,                    -- NULL = no timeout
    status           TEXT NOT NULL DEFAULT 'QUEUED',
    worker_id        TEXT,                       -- NULL until dispatched
    submitted_at     TEXT NOT NULL,              -- ISO 8601 UTC
    dispatched_at    TEXT,
    started_at       TEXT,
    completed_at     TEXT,
    requeued_at      TEXT,                       -- set on host restart re-queue
    output           TEXT,                       -- final agent output
    error            TEXT,                       -- error message if FAILED
    cost_usd         REAL,
    tokens_in        INTEGER,
    tokens_out       INTEGER,
    submitter_host   TEXT,                       -- IP or hostname of submit client
    FOREIGN KEY (worker_id) REFERENCES runtime_workers(worker_id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_tasks_status
    ON runtime_tasks(status, priority DESC, submitted_at ASC);

CREATE INDEX IF NOT EXISTS idx_runtime_tasks_profile
    ON runtime_tasks(profile, status);

-- Task event log. Append-only.
CREATE TABLE IF NOT EXISTS runtime_task_events (
    event_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id      TEXT NOT NULL,
    event_type   TEXT NOT NULL,  -- status_change|thinking|tool_call|tool_result|partial_output|final_output|cost
    payload      TEXT NOT NULL,  -- JSON
    emitted_at   TEXT NOT NULL,  -- ISO 8601 UTC (from worker clock)
    received_at  TEXT NOT NULL,  -- ISO 8601 UTC (host receipt time)
    FOREIGN KEY (task_id) REFERENCES runtime_tasks(task_id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_task_events_task
    ON runtime_task_events(task_id, event_id ASC);

-- Worker registry maintained by the host.
CREATE TABLE IF NOT EXISTS runtime_workers (
    worker_id          TEXT PRIMARY KEY,
    hostname           TEXT NOT NULL,
    profiles_json      TEXT NOT NULL,   -- JSON array of profile names
    concurrency        INTEGER NOT NULL DEFAULT 1,
    in_progress        INTEGER NOT NULL DEFAULT 0,
    status             TEXT NOT NULL DEFAULT 'HEALTHY', -- HEALTHY|UNHEALTHY|DRAINING|DISCONNECTED
    connected_at       TEXT NOT NULL,
    last_heartbeat_at  TEXT,
    disconnected_at    TEXT,
    tag_version        TEXT,            -- worker's TAG version string
    python_version     TEXT
);
```

### 9.3 Core Dataclasses (`runtime_proto.py`)

```python
from dataclasses import dataclass, field
from typing import Any
import json
import uuid
from datetime import datetime, timezone


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


@dataclass
class WorkerInfo:
    worker_id: str
    hostname: str
    profiles: list[str]
    concurrency: int = 1
    tag_version: str = ""
    python_version: str = ""
    auth_token: str = ""          # stripped before storage

    def to_bytes(self) -> bytes:
        d = {k: v for k, v in self.__dict__.items() if k != "auth_token"}
        return json.dumps(d).encode()

    @classmethod
    def from_bytes(cls, b: bytes) -> "WorkerInfo":
        return cls(**json.loads(b))


@dataclass
class TaskRequest:
    profile: str
    prompt: str
    files: list[dict] = field(default_factory=list)   # [{name, content_b64}]
    env: dict[str, str] = field(default_factory=dict)
    priority: int = 5
    timeout_seconds: int | None = None
    task_id: str = field(default_factory=lambda: f"task-{uuid.uuid4().hex[:8]}")
    submitted_at: str = field(default_factory=_now_iso)
    submitter_host: str = ""

    def to_bytes(self) -> bytes:
        return json.dumps(self.__dict__).encode()

    @classmethod
    def from_bytes(cls, b: bytes) -> "TaskRequest":
        return cls(**json.loads(b))


@dataclass
class TaskAck:
    task_id: str
    status: str = "QUEUED"
    message: str = ""


@dataclass
class TaskEvent:
    task_id: str
    event_type: str         # status_change|thinking|tool_call|tool_result|partial_output|final_output|cost
    payload: dict[str, Any]
    emitted_at: str = field(default_factory=_now_iso)

    def to_bytes(self) -> bytes:
        return json.dumps(self.__dict__).encode()

    @classmethod
    def from_bytes(cls, b: bytes) -> "TaskEvent":
        d = json.loads(b)
        d["payload"] = d.get("payload", {})
        return cls(**d)


@dataclass
class WorkerCommand:
    command: str                    # assign_task|cancel_task|drain|shutdown
    task_request: TaskRequest | None = None
    task_id: str | None = None
    message: str = ""

    def to_bytes(self) -> bytes:
        d = {
            "command": self.command,
            "task_request": self.task_request.__dict__ if self.task_request else None,
            "task_id": self.task_id,
            "message": self.message,
        }
        return json.dumps(d).encode()

    @classmethod
    def from_bytes(cls, b: bytes) -> "WorkerCommand":
        d = json.loads(b)
        if d.get("task_request"):
            d["task_request"] = TaskRequest(**d["task_request"])
        return cls(**d)


@dataclass
class HeartbeatRequest:
    worker_id: str
    in_progress: int
    task_ids_in_progress: list[str] = field(default_factory=list)
    timestamp: str = field(default_factory=_now_iso)


@dataclass
class StatusResponse:
    host_address: str
    timestamp: str
    workers: list[dict]
    tasks: list[dict]
    queue_depth: int
    running: int
    completed_today: int
    failed_today: int


# Task state machine — allowed transitions
TASK_TRANSITIONS: dict[str, set[str]] = {
    "QUEUED":      {"DISPATCHED", "CANCELED"},
    "DISPATCHED":  {"RUNNING",    "QUEUED",    "CANCELED"},
    "RUNNING":     {"COMPLETED",  "FAILED",    "CANCELED"},
    "COMPLETED":   set(),
    "FAILED":      set(),
    "CANCELED":    set(),
}
```

### 9.4 Host Architecture

The host is an `asyncio`-based gRPC server using `grpc.aio`. The key design decision is using **raw gRPC byte streams** (method type `BIDI_STREAMING` with request serializer `bytes`) rather than compiled proto stubs. This avoids `protoc` and makes the package installable without build tools.

```python
# src/tag/runtime.py (host core, simplified)
import asyncio
import grpc
import grpc.aio
from collections import defaultdict
import heapq

class TagRuntimeHost:
    """
    gRPC server. Uses asyncio.Queue per worker for command dispatch,
    and asyncio.Queue per task for event fan-out to subscribers.
    """

    def __init__(self, port: int, auth_token: str, db_path: str,
                 dispatch_strategy: str = "affinity"):
        self.port = port
        self.auth_token = auth_token
        self.db_path = db_path
        self.dispatch_strategy = dispatch_strategy

        # worker_id → asyncio.Queue[WorkerCommand]
        self._worker_queues: dict[str, asyncio.Queue] = {}
        # worker_id → WorkerInfo
        self._worker_info: dict[str, WorkerInfo] = {}
        # task_id → asyncio.Queue[TaskEvent | None]  (None = stream closed)
        self._task_event_queues: dict[str, list[asyncio.Queue]] = defaultdict(list)
        # per-profile priority queue: list of (-priority, submitted_at, task_id)
        self._dispatch_queue: list[tuple] = []
        self._queue_lock = asyncio.Lock()

    async def _validate_token(self, context: grpc.aio.ServicerContext) -> bool:
        meta = dict(context.invocation_metadata())
        auth = meta.get("authorization", "")
        if not auth.startswith("Bearer ") or auth[7:] != self.auth_token:
            await context.abort(grpc.StatusCode.UNAUTHENTICATED, "Invalid auth token")
            return False
        return True

    async def register_worker(self, request_iterator, context):
        """BIDI_STREAMING: worker sends WorkerInfo once, then HeartbeatRequests.
        Host sends WorkerCommand messages."""
        if not await self._validate_token(context):
            return

        info_bytes = await request_iterator.__anext__()
        info = WorkerInfo.from_bytes(info_bytes)
        await self._on_worker_connect(info)

        cmd_queue = self._worker_queues[info.worker_id]
        try:
            # Fan out: simultaneously send commands and receive heartbeats/events
            async with asyncio.TaskGroup() as tg:
                tg.create_task(self._send_commands(cmd_queue, context))
                tg.create_task(self._recv_worker_messages(
                    info.worker_id, request_iterator))
        except* Exception:
            pass
        finally:
            await self._on_worker_disconnect(info.worker_id)

    async def _dispatch_loop(self):
        """Continuously tries to match queued tasks to available workers."""
        while True:
            await asyncio.sleep(0.1)
            async with self._queue_lock:
                if not self._dispatch_queue:
                    continue
                task_id = await self._try_dispatch_next()
                if task_id:
                    pass  # dispatched; loop again immediately

    async def _try_dispatch_next(self) -> str | None:
        """Pop highest-priority task and send to best-matching worker."""
        from tag.runtime_proto import TASK_TRANSITIONS
        # Find best worker for the top task
        if not self._dispatch_queue:
            return None
        _, _, task_id, profile = self._dispatch_queue[0]
        worker_id = self._select_worker(profile)
        if worker_id is None:
            return None
        heapq.heappop(self._dispatch_queue)
        cmd = WorkerCommand(
            command="assign_task",
            task_request=await self._load_task_request(task_id),
        )
        self._worker_queues[worker_id].put_nowait(cmd)
        await self._transition_task(task_id, "DISPATCHED", worker_id=worker_id)
        return task_id

    def _select_worker(self, profile: str) -> str | None:
        """Affinity: pick lowest-loaded worker that supports the profile."""
        candidates = [
            (info.in_progress / info.concurrency, wid)
            for wid, info in self._worker_info.items()
            if profile in info.profiles
            and info.status == "HEALTHY"
            and self._worker_info[wid].in_progress < info.concurrency
        ]
        if not candidates:
            return None
        return min(candidates)[1]
```

### 9.5 Worker Architecture

The worker runs a single `asyncio` coroutine that maintains a bidirectional stream to the host, receives `WorkerCommand` messages, and executes tasks by invoking the existing TAG agent controller.

```python
# src/tag/runtime.py (worker core, simplified)
import asyncio
import tempfile
import base64
from pathlib import Path

class TagRuntimeWorker:
    def __init__(self, host: str, worker_id: str, profiles: list[str],
                 concurrency: int, auth_token: str, heartbeat_interval: int = 10):
        self.host = host          # "host:50051"
        self.worker_id = worker_id
        self.profiles = profiles
        self.concurrency = concurrency
        self.auth_token = auth_token
        self.heartbeat_interval = heartbeat_interval
        self._semaphore: asyncio.Semaphore | None = None
        self._in_progress: set[str] = set()

    async def run(self):
        """Main loop with exponential-backoff reconnection."""
        self._semaphore = asyncio.Semaphore(self.concurrency)
        backoff = 5
        while True:
            try:
                await self._connect_and_serve()
                backoff = 5  # reset on clean disconnect
            except Exception as exc:
                import logging
                logging.warning(f"Worker disconnected ({exc}), retrying in {backoff}s")
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 60)

    async def _connect_and_serve(self):
        import grpc.aio
        creds = grpc.local_channel_credentials()  # insecure for now
        async with grpc.aio.insecure_channel(self.host) as channel:
            stub = channel  # raw byte stub
            metadata = [("authorization", f"Bearer {self.auth_token}")]

            # Send initial WorkerInfo, then heartbeats
            async def request_gen():
                yield WorkerInfo(
                    worker_id=self.worker_id,
                    hostname=_hostname(),
                    profiles=self.profiles,
                    concurrency=self.concurrency,
                ).to_bytes()
                while True:
                    await asyncio.sleep(self.heartbeat_interval)
                    yield HeartbeatRequest(
                        worker_id=self.worker_id,
                        in_progress=len(self._in_progress),
                        task_ids_in_progress=list(self._in_progress),
                    ).to_bytes()

            async for cmd_bytes in stub.RegisterWorker(
                request_gen(), metadata=metadata
            ):
                cmd = WorkerCommand.from_bytes(cmd_bytes)
                if cmd.command == "assign_task" and cmd.task_request:
                    asyncio.create_task(
                        self._execute_task(stub, cmd.task_request, metadata)
                    )
                elif cmd.command == "cancel_task" and cmd.task_id:
                    await self._cancel_task(cmd.task_id)
                elif cmd.command in ("drain", "shutdown"):
                    break

    async def _execute_task(self, channel, req: TaskRequest, metadata):
        """Run the TAG agent for the task and stream events back."""
        async with self._semaphore:
            self._in_progress.add(req.task_id)
            workdir = None
            try:
                # Decode uploaded files into a temp directory
                workdir = tempfile.mkdtemp(prefix=f"tag-task-{req.task_id}-")
                for f in req.files:
                    fpath = Path(workdir) / f["name"]
                    fpath.write_bytes(base64.b64decode(f["content_b64"]))

                await self._emit_event(channel, metadata, TaskEvent(
                    task_id=req.task_id,
                    event_type="status_change",
                    payload={"from": "DISPATCHED", "to": "RUNNING"},
                ))

                # Bridge into existing TAG agent runner
                from tag.controller import run_agent_for_runtime
                async for event in run_agent_for_runtime(
                    profile=req.profile,
                    prompt=req.prompt,
                    workdir=workdir,
                    env=req.env,
                    timeout=req.timeout_seconds,
                ):
                    await self._emit_event(channel, metadata, TaskEvent(
                        task_id=req.task_id,
                        event_type=event["type"],
                        payload=event["data"],
                    ))

            except Exception as exc:
                await self._emit_event(channel, metadata, TaskEvent(
                    task_id=req.task_id,
                    event_type="status_change",
                    payload={"from": "RUNNING", "to": "FAILED", "error": str(exc)},
                ))
            finally:
                self._in_progress.discard(req.task_id)
                if workdir:
                    import shutil
                    shutil.rmtree(workdir, ignore_errors=True)
```

### 9.6 Integration with `controller.py`

`controller.py` gains a new async generator function `run_agent_for_runtime` that wraps the existing agent execution path and yields structured event dicts:

```python
# In controller.py
async def run_agent_for_runtime(
    profile: str,
    prompt: str,
    workdir: str,
    env: dict[str, str],
    timeout: int | None,
) -> AsyncGenerator[dict, None]:
    """
    Async generator that runs a TAG agent and yields TaskEvent-compatible dicts.
    Used by runtime workers. Bridges the existing Hermes callback system
    into an async event stream.
    """
    cfg = load_config()
    profile_cfg = load_profile(cfg, profile)

    event_queue: asyncio.Queue = asyncio.Queue()

    def on_thinking(text: str):
        event_queue.put_nowait({"type": "thinking", "data": {"text": text}})

    def on_tool_call(name: str, args: dict):
        event_queue.put_nowait({"type": "tool_call", "data": {"name": name, "args": args}})

    def on_tool_result(name: str, result: str):
        event_queue.put_nowait({"type": "tool_result", "data": {"name": name, "result": result}})

    def on_output(text: str, cost: dict | None = None):
        event_queue.put_nowait({"type": "final_output", "data": {"output": text}})
        if cost:
            event_queue.put_nowait({"type": "cost", "data": cost})
        event_queue.put_nowait(None)  # sentinel

    runner_task = asyncio.create_task(
        _run_hermes_agent(
            profile_cfg, prompt, workdir, env,
            on_thinking=on_thinking,
            on_tool_call=on_tool_call,
            on_tool_result=on_tool_result,
            on_output=on_output,
        )
    )

    async with asyncio.timeout(timeout):
        while True:
            event = await event_queue.get()
            if event is None:
                break
            yield event

    await runner_task
```

### 9.7 gRPC Service Registration (No `.proto` File)

Rather than generating stubs from a `.proto` file, `runtime.py` uses `grpc`'s generic service API with a `GenericMethodHandler` for each RPC. This is less ergonomic but avoids build-time dependencies:

```python
def _build_server_handlers() -> list[grpc.ServiceRpcHandlers]:
    """Register RPCs as raw byte streams using grpc.GenericMethodHandler."""
    from grpc import unary_unary, unary_stream, stream_unary, stream_stream

    return [
        grpc.method_service_handler(
            "tag.runtime.TagRuntimeService",
            {
                "RegisterWorker": grpc.stream_stream_rpc_method_handler(
                    host.register_worker,
                    request_deserializer=lambda b: b,
                    response_serializer=lambda b: b,
                ),
                "SubmitTask": grpc.unary_unary_rpc_method_handler(
                    host.submit_task,
                    request_deserializer=lambda b: b,
                    response_serializer=lambda b: b,
                ),
                "StreamTaskEvents": grpc.unary_stream_rpc_method_handler(
                    host.stream_task_events,
                    request_deserializer=lambda b: b,
                    response_serializer=lambda b: b,
                ),
                "GetTaskResult": grpc.unary_unary_rpc_method_handler(
                    host.get_task_result,
                    request_deserializer=lambda b: b,
                    response_serializer=lambda b: b,
                ),
                "CancelTask": grpc.unary_unary_rpc_method_handler(
                    host.cancel_task,
                    request_deserializer=lambda b: b,
                    response_serializer=lambda b: b,
                ),
                "GetStatus": grpc.unary_unary_rpc_method_handler(
                    host.get_status,
                    request_deserializer=lambda b: b,
                    response_serializer=lambda b: b,
                ),
                "Heartbeat": grpc.unary_unary_rpc_method_handler(
                    host.heartbeat,
                    request_deserializer=lambda b: b,
                    response_serializer=lambda b: b,
                ),
            }
        )
    ]
```

### 9.8 Prometheus Metrics Exporter

The metrics HTTP server runs on a separate port using `prometheus_client` (optional dependency):

```python
async def _start_metrics_server(port: int):
    """Start a simple Prometheus HTTP server. Lazy import."""
    try:
        from prometheus_client import start_http_server, Counter, Gauge, Histogram
    except ImportError:
        print(f"WARNING: prometheus_client not installed; metrics endpoint disabled.")
        return

    TASKS_SUBMITTED = Counter(
        "tag_runtime_tasks_submitted_total",
        "Tasks submitted to the runtime host",
        ["profile"],
    )
    TASKS_COMPLETED = Counter(
        "tag_runtime_tasks_completed_total",
        "Tasks completed by runtime workers",
        ["profile", "status"],
    )
    WORKERS_CONNECTED = Gauge(
        "tag_runtime_workers_connected",
        "Number of workers currently connected",
    )
    DISPATCH_LATENCY = Histogram(
        "tag_runtime_task_dispatch_latency_seconds",
        "Seconds between task submission and worker dispatch",
        buckets=[0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0],
    )
    TASK_DURATION = Histogram(
        "tag_runtime_task_duration_seconds",
        "Total task execution duration in seconds",
        ["profile"],
        buckets=[1, 5, 15, 30, 60, 120, 300, 600],
    )

    start_http_server(port)
    # Expose registry on module for use in host methods
    return TASKS_SUBMITTED, TASKS_COMPLETED, WORKERS_CONNECTED, DISPATCH_LATENCY, TASK_DURATION
```

### 9.9 `pyproject.toml` Optional Dependency

```toml
[project.optional-dependencies]
runtime = [
    "grpcio>=1.62.0",
    "grpcio-status>=1.62.0",
    "prometheus-client>=0.20.0",
]
```

Install with: `pip install tag[runtime]`

---

## 10. Security Considerations

1. **Bearer token minimum entropy.** The auth token must be at least 32 characters. On startup, if no token is configured, the host generates a cryptographically random token using `secrets.token_hex(32)` (256 bits of entropy) and prints it once. The token is never written to SQLite or log files; it is only stored in the process's in-memory config and in `cli-config.yaml` (mode 0600).

2. **Token comparison timing safety.** The host validates tokens using `hmac.compare_digest` (constant-time comparison) to prevent timing oracle attacks.

3. **TLS for production deployments.** Without TLS, auth tokens and task payloads (including file contents and prompts) are transmitted in plaintext. The host emits a startup warning and documentation must state that TLS is required for any non-localhost deployment. Provide a quickstart using `mkcert` for LAN deployments.

4. **File payload sanitization.** Uploaded files from `tag submit --files` are written to an isolated temporary directory on the worker, not to the worker's working directory or home. The worker must validate that decoded file paths do not contain `..` path traversal sequences before writing.

5. **Task prompt injection.** Worker operators must be aware that the prompt field of `TaskRequest` is executed verbatim by the TAG agent. A malicious host can inject arbitrary prompts. Workers should only connect to hosts they trust (controlled by the same organization). Future enhancement: sign `TaskRequest` payloads with the host's private key.

6. **gRPC reflection disabled by default.** The host must not enable `grpc.reflection.v1alpha.ServerReflection` in production mode, as it exposes the full service schema to unauthenticated clients. Reflection can be enabled with `--debug-reflection` for development tooling.

7. **Worker isolation.** Workers run with the same OS user as the `tag runtime worker start` invocation. If the TAG sandbox (PRD-028) is configured for a profile, the worker's agent execution uses the sandbox. Operators should run workers under dedicated low-privilege OS users.

8. **IP logging for auth failures.** Each failed auth attempt is logged with the client's IP address (from gRPC peer metadata) at WARN level, without logging the submitted token. Rate-limiting auth failures (max 10/minute per IP) is recommended but not implemented in v1.

9. **Prompt/output data residency.** Task prompts, file contents, and outputs are stored in the host's SQLite database. Operators must be aware of data residency implications when running the host in a different jurisdiction from where the task data originates.

10. **Timeout enforcement.** The `timeout_seconds` field in `TaskRequest` is enforced by the worker using `asyncio.timeout`. When the timeout fires, the agent subprocess is `SIGKILL`ed and a `FAILED` event with `reason: timeout` is emitted. Callers must not rely on graceful cleanup within the agent on timeout.

---

## 11. Testing Strategy

### 11.1 Unit Tests

- **`tests/test_runtime_proto.py`**: Roundtrip serialization tests for all dataclasses (`WorkerInfo`, `TaskRequest`, `TaskEvent`, `WorkerCommand`). Edge cases: empty profiles list, null optional fields, UTF-8 prompt with emoji, priority 0 and 9.
- **`tests/test_runtime_state_machine.py`**: Test all allowed and disallowed transitions in `TASK_TRANSITIONS`. Verify that illegal transitions raise `ValueError`. Test the dispatch priority queue invariant (higher priority tasks dispatch first).
- **`tests/test_runtime_auth.py`**: Verify `hmac.compare_digest` comparison, too-short token rejection at startup, `UNAUTHENTICATED` gRPC status on wrong token.
- **`tests/test_runtime_import_isolation.py`**: Assert that `import tag.controller` does not import `grpc`, `grpcio`, or `prometheus_client`. Assert that calling `cmd_runtime_host_start` without `grpcio` installed raises a friendly `SystemExit(1)` with install instructions.
- **`tests/test_runtime_dispatch.py`**: Test affinity selection with various worker+profile combinations. Test least-loaded selection. Test round-robin ordering. Test that tasks remain QUEUED when no matching worker is connected.

### 11.2 Integration Tests

- **`tests/integration/test_runtime_end_to_end.py`**: Starts a host subprocess and a worker subprocess (both using `subprocess.Popen` with `--port 50099` to avoid conflicts). Submits a task via `tag submit --runtime grpc://localhost:50099`, waits for completion, asserts the task appears as COMPLETED in `tag runtime status --json`. Cleans up subprocesses on teardown.
- **`tests/integration/test_runtime_worker_reconnect.py`**: Starts host, connects worker, kills host process, restarts it, verifies worker reconnects within 30 seconds.
- **`tests/integration/test_runtime_task_cancel.py`**: Submits a long-running task (prompt designed to run >10 seconds), cancels it via `tag runtime task cancel`, verifies the task reaches CANCELED state and the worker process terminates.
- **`tests/integration/test_runtime_event_replay.py`**: Submits a task, lets it complete, then calls `tag runtime task stream <id> --from-beginning` and verifies all stored events are replayed in order.
- **`tests/integration/test_runtime_file_upload.py`**: Submits a task with `--files` pointing to a small Python file, verifies the worker can read the file and the agent output references it.

### 11.3 Performance Tests

- **`tests/perf/test_runtime_throughput.py`**: Starts 1 host and 3 workers (concurrency=2 each). Submits 100 no-op tasks (profile `echo` that returns immediately). Measures total wall time and asserts completion within 30 seconds. Checks for memory leaks in the host process (RSS growth < 50 MB).
- **`tests/perf/test_runtime_streaming_latency.py`**: Measures the p99 latency between a worker emitting a `TaskEvent` and the event arriving at a `StreamTaskEvents` subscriber. Uses monotonic timestamps in the payload. Asserts p99 < 200 ms on localhost.
- **`tests/perf/test_runtime_concurrent_dispatch.py`**: Verifies that 10 tasks submitted simultaneously are all dispatched within 1 second with no dropped events.

---

## 12. Acceptance Criteria

| ID | Criterion | Pass Condition |
|----|-----------|----------------|
| AC-01 | Host starts and accepts gRPC connections | `tag runtime host start --port 50051` is serving within 2 seconds; `grpc_health_probe` returns OK |
| AC-02 | Worker connects and appears in status | After `tag runtime worker start --host grpc://localhost:50051 --profile coder`, `tag runtime status --json` shows the worker as HEALTHY |
| AC-03 | Task submission returns task ID immediately | `tag submit --runtime grpc://localhost:50051 --profile coder --prompt "hi"` exits with a task ID within 500 ms (before agent completes) |
| AC-04 | Task dispatched to affinity-matching worker | Task submitted with `--profile coder` is assigned only to a worker that declared `coder` in its profiles list |
| AC-05 | Task streaming works end-to-end | `tag submit --runtime grpc://localhost:50051 --stream --profile coder --prompt "..."` prints at least one `thinking` event before the final output |
| AC-06 | Task result persists and is retrievable | After a task completes, `tag runtime task get <task-id>` returns the full output and cost |
| AC-07 | Auth rejection works | A `tag submit` with a wrong `--token` is rejected with a clear `UNAUTHENTICATED` error |
| AC-08 | Worker reconnects after host restart | Host is killed and restarted; worker reconnects within 60 seconds without manual intervention |
| AC-09 | Task state survives host restart | Tasks in RUNNING state at host restart are re-queued to QUEUED on next host startup |
| AC-10 | Task cancellation works | `tag runtime task cancel <id>` on a RUNNING task causes the task to reach CANCELED state within 5 seconds |
| AC-11 | Priority ordering is respected | Three tasks submitted with priorities 1, 5, 9 to a single worker; the priority-9 task is dispatched first |
| AC-12 | File upload reaches worker | `tag submit --files auth.py` results in `auth.py` being present in the agent's working directory on the worker |
| AC-13 | Prometheus metrics are exported | After submitting 3 tasks, `curl http://localhost:9090/metrics` shows `tag_runtime_tasks_submitted_total{profile="coder"} 3` |
| AC-14 | Zero gRPC import on cold path | `python -c "import tag.controller; import sys; assert 'grpc' not in sys.modules"` exits 0 |
| AC-15 | Missing grpcio handled gracefully | `tag submit --runtime grpc://...` without grpcio installed prints a `pip install tag[runtime]` hint and exits 1, not a Python traceback |
| AC-16 | Worker drain on SIGTERM | `kill -TERM <worker-pid>` causes the worker to finish its current task and exit cleanly |
| AC-17 | Event replay for late subscribers | `tag runtime task stream <completed-id> --from-beginning` replays all stored events in chronological order |
| AC-18 | Unhealthy worker task re-queue | Kill worker process mid-task; after 3 missed heartbeats, host re-queues the task to another worker |
| AC-19 | Concurrent tasks with semaphore | Worker with `--concurrency 2` accepts exactly 2 simultaneous tasks and queues the third until one finishes |
| AC-20 | Token minimum entropy enforcement | `tag runtime host start --token "short"` exits 1 with "Auth token must be at least 32 characters" |

---

## 13. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `grpcio` | Optional Python package | `>=1.62.0` | Core gRPC transport. Install via `pip install tag[runtime]`. |
| `grpcio-status` | Optional Python package | `>=1.62.0` | Rich status codes for gRPC errors. Companion to `grpcio`. |
| `prometheus-client` | Optional Python package | `>=0.20.0` | Prometheus metrics HTTP exporter. Disabled gracefully if absent. |
| `asyncio` | Python stdlib | 3.10+ | `asyncio.TaskGroup` (3.11+) used; fallback to `asyncio.gather` for 3.10. |
| `hmac` | Python stdlib | any | Constant-time token comparison. |
| `secrets` | Python stdlib | any | Cryptographic token generation. |
| `tempfile` | Python stdlib | any | Isolated working directories for file uploads. |
| PRD-013 (tracing) | Internal | — | Trace IDs assigned to runtime tasks for cross-referencing. |
| PRD-028 (sandbox) | Internal | — | Workers honor per-profile sandbox config for agent execution. |
| PRD-034 (security) | Internal | — | Token validation follows the credential hygiene patterns from PRD-034. |
| PRD-012 (budget) | Internal | — | `budget.record_spend` called on host with task cost data. |
| PRD-008 (queue) | Internal | — | Conceptual predecessor; runtime supersedes queue for multi-machine scenarios. |
| `controller.py:run_agent_for_runtime` | Internal | — | New async generator function added to controller.py to bridge agent execution into the worker's event stream. |

---

## 14. Open Questions

| # | Question | Owner | Target Date |
|---|----------|-------|-------------|
| OQ-1 | **`grpc.aio` vs thread-based gRPC.** `grpc.aio` (asyncio native) is the modern path but has known issues with some `grpcio` versions on macOS. Should we implement a synchronous thread-based fallback? | Runtime lead | Before Phase 2 start |
| OQ-2 | **Compiled proto vs. raw byte handlers.** Raw byte handlers (current plan) avoid build tooling but lose type safety and IDE autocompletion for gRPC message types. Is a Makefile-based `protoc` step acceptable for contributors? | Tech lead | Architecture review |
| OQ-3 | **Worker task resumption after crash.** If the worker OS process crashes mid-task (not a graceful disconnect), the task is re-queued by the host after 3 missed heartbeats. But the agent may have partially modified files. Should we snapshot agent state (e.g., diff context) before execution? | Runtime lead | Phase 3 |
| OQ-4 | **mTLS vs. bearer token auth.** mTLS would eliminate the need for a shared secret (each worker gets a certificate signed by the host's CA). More operationally complex but more secure. Treat as a v2 enhancement or required for v1? | Security | Before Phase 1 complete |
| OQ-5 | **Event fanout scalability.** The current design uses one `asyncio.Queue` per `StreamTaskEvents` subscriber. With many concurrent subscribers watching the same task, this could become a bottleneck. A broadcast mechanism (e.g., `asyncio.Event` + shared list) may be needed at scale. | Runtime lead | Phase 2 |
| OQ-6 | **`run_agent_for_runtime` controller bridge.** The current proposal requires adding a new async generator to `controller.py`. Given controller.py is already ~10,000 lines, should this live in a dedicated `runtime_agent_bridge.py` to keep concerns separated? | Lead engineer | Design review |
| OQ-7 | **SQLite concurrency on the host.** The host's asyncio event loop writes task events at high frequency (many events per second from multiple workers). WAL mode helps but sequential writes in asyncio may become a bottleneck. Should events be batched (e.g., every 100 ms)? | Runtime lead | Phase 2 performance testing |
| OQ-8 | **GitHub issue #347 A2A interoperability.** Should the host also expose an A2A-compatible endpoint (JSON-RPC 2.0 over HTTP+SSE at `/.well-known/agent-card.json`) so that A2A clients can submit tasks without the TAG runtime client? This would make the host interoperable with the broader A2A ecosystem. | Product | Before v1.0 release |
| OQ-9 | **Worker identity for audit.** Should the `runtime_tasks` table record the submitter's identity (not just IP)? This requires extending `tag submit` to include a user identity claim in the task request. | Security | Before enterprise pilot |

---

## 15. Complexity and Timeline

### Phase 1 — Core Infrastructure (Weeks 1–2, ~10 days)

**Goal:** A working host and worker on localhost. Task submission, dispatch, execution, and result retrieval. No TLS, no Prometheus, no file uploads.

- Day 1–2: Define all dataclasses in `runtime_proto.py`. Implement serialization roundtrips. Write unit tests for all dataclasses and the state machine. CI green.
- Day 3–4: Implement `TagRuntimeHost` gRPC server using `grpc.aio` with raw byte handlers. `RegisterWorker` BIDI stream, `SubmitTask` unary, `GetStatus` unary. Write minimal in-memory worker registry (no SQLite yet).
- Day 5–6: Implement `TagRuntimeWorker` client. Connect to host, register, receive `assign_task` command, execute task via a stub `run_agent_for_runtime` (returns "hello" immediately), emit `final_output` event.
- Day 7: Wire `cmd_runtime_host_start` and `cmd_runtime_worker_start` into `controller.py` CLI dispatch. Lazy import guard. `tag runtime host start` and `tag runtime worker start` work end-to-end on localhost.
- Day 8: Implement `runtime_tasks` and `runtime_workers` SQLite tables. Persist task state transitions. `cmd_runtime_task_get` works.
- Day 9: Implement affinity dispatch scheduler with `heapq` priority queue. Test with 2 workers of different profiles.
- Day 10: Auth token validation with `hmac.compare_digest`. 32-character minimum. End-to-end integration test (host subprocess + worker subprocess + submit). Phase 1 review.

### Phase 2 — Feature Completeness (Weeks 3–4, ~10 days)

**Goal:** All CLI surface implemented. Streaming, cancellation, reconnection, file uploads, Prometheus.

- Day 11–12: `StreamTaskEvents` RPC on host. Event fan-out to subscribers. `runtime_task_events` table. `tag runtime task stream` CLI. Event replay from SQLite.
- Day 13: `CancelTask` RPC. `cancel_task` command sent to worker. Worker SIGKILL of agent subprocess on cancel. CANCELED state in SQLite.
- Day 14: Worker reconnection with exponential backoff. Host detects missed heartbeats and marks workers UNHEALTHY. Task re-queue on worker UNHEALTHY.
- Day 15: `--files` upload in `TaskRequest`. File decoding in worker temp directory. Path traversal validation. 10 MB payload limit check.
- Day 16: Real `run_agent_for_runtime` bridge in `controller.py`. Wire Hermes callbacks into async event queue. Test with `coder` profile on actual prompt.
- Day 17: Prometheus metrics (`prometheus_client` lazy import). Counters and histograms. Metrics HTTP server on separate port. Test with `curl`.
- Day 18: `tag runtime status --watch` Rich Live rendering. `tag runtime worker list` command. `--json` output for all commands.
- Day 19–20: `tag submit --stream`, `--wait`, `--priority`, `--env`, `--timeout` flags. TLS support (`--tls-cert`, `--tls-key` on host; `--tls-ca-cert` on worker). Phase 2 review.

### Phase 3 — Hardening and Documentation (Weeks 5–6, ~8 days)

**Goal:** Production-ready reliability. Performance tests pass. Documentation complete.

- Day 21–22: Performance tests (100-task batch, streaming latency p99). Identify and fix bottlenecks (event queue batching if needed).
- Day 23: Graceful host shutdown (SIGTERM, 30s drain window, `DrainCommand` to workers).
- Day 24: Worker drain on SIGTERM. Worker-side task reconciliation on reconnect.
- Day 25: Security hardening: IP logging for auth failures, gRPC reflection disabled by default, file path sanitization tests.
- Day 26: Full acceptance criteria verification. Run all AC-01 through AC-20.
- Day 27–28: Documentation in `docs/`, CLI `--help` text, `pyproject.toml` optional dependency entry, GitHub issue #347 close checklist. Final review and merge.

**Total: 28 engineering days (5–6 weeks with review overhead)**

---

*GitHub Issue: [#347](https://github.com/tag-agent/tag/issues/347)*
*Cluster: E — Multi-Agent Interoperability Protocols*
*PRD Author: Generated 2026-06-17*

