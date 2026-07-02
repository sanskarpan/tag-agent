# PRD-088: Distributed Agent Runtime (gRPC Host/Worker for Cross-Machine Agents) (`tag runtime`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** XL (4-8 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `internal/runtime`
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (security hardening), PRD-027 (eval framework), PRD-012 (cost tracking/budget), PRD-008 (background task queue)
**Inspired by:** AutoGen distributed runtime, Ray distributed actors, Celery workers

---

## 1. Overview

TAG agents today are tightly coupled to the machine and process that invokes them. Running `tag run --profile coder --prompt "..."` spawns an agent synchronously in the foreground of the calling shell. The agent's entire lifecycle — model calls, tool execution, context management — happens on the invoking machine. This architecture is simple and debuggable, but it fundamentally limits horizontal scalability: you cannot distribute work across a fleet of specialized machines, you cannot dedicate a GPU-equipped host to embedding-heavy semantic memory lookups while a separate host handles tool execution, and you cannot submit agent tasks from a CI pipeline to a long-lived worker pool without ad-hoc shell hacks.

PRD-088 introduces the **Distributed Agent Runtime**: a gRPC-based host/worker architecture that allows TAG agents to run across multiple machines in a coordinated cluster. A **host** process exposes a well-known gRPC endpoint that accepts task submissions, manages a registry of connected workers, schedules tasks to workers based on profile affinity and current load, and streams status events back to callers. **Workers** connect outbound to the host, declare which profiles they support, and execute tasks using their locally-configured TAG installation — including local MCP servers, sandboxes, semantic memory indexes, and tool retrievers. Workers report incremental status (thinking, tool calls, partial outputs) back to the host via bidirectional gRPC streams, and the host fans these events out to any subscriber watching the task.

The design is explicitly inspired by three mature distributed task systems. **AutoGen's distributed runtime** (v0.4+) separates agent logic from the communication substrate, using gRPC message passing so agents on different hosts can send and receive messages without knowing each other's location. **Ray distributed actors** treat each long-running stateful object (here: a worker's loaded profile context and tool singletons) as a remote actor that can be addressed by stable identity. **Celery workers** pioneered the broker-mediated task queue pattern where workers pull tasks matching their declared capabilities — here translated into gRPC streaming subscriptions rather than AMQP queues, eliminating an external broker dependency entirely.

TAG's SQLite state store (`~/.tag/runtime/tag.sqlite3`, via `modernc.org/sqlite`) gains two new tables: `runtime_tasks` tracking submitted tasks with their lifecycle state, and `runtime_workers` caching the worker registry seen by each host. The host's gRPC server lives in package `internal/runtime` (module `github.com/tag-agent/tag`) and is built on `google.golang.org/grpc` with generated stubs from a language-neutral `.proto` service definition (`protoc-gen-go` + `protoc-gen-go-grpc`). The CLI surface is `tag runtime host start`, `tag runtime worker start`, `tag runtime status`, and `tag submit --runtime grpc://...`.

This feature is categorized P3 (nice-to-have) because TAG's primary deployment model — single-user, single-machine — does not require distributed infrastructure. The feature targets platform engineers building internal AI automation pipelines who need to burst agent workloads across a small cluster (2–10 machines) without adopting a full-weight orchestration platform like Ray or Kubernetes. Because TAG ships as a single static Go binary (`CGO_ENABLED=0`), the runtime code is compiled into the same binary and gated behind the `tag runtime` subcommand; there is no separate install extra and no per-command import cost — the gRPC server and client are simply not initialized on the common single-machine path.

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
| gRPC init overhead | single-machine `tag run` does not construct a gRPC server/client | benchmark + no-op assertion in `runtime_init_test.go` |
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
| FR-01 | **gRPC service definition:** `internal/runtime/proto/runtime.proto` must define a `TagRuntimeService` with RPCs: `RegisterWorker(stream WorkerMessage) → stream WorkerCommand` (bidi), `SubmitTask(TaskRequest) → TaskAck`, `StreamTaskEvents(TaskRef) → stream TaskEvent`, `GetTaskResult(TaskRef) → TaskResult`, `CancelTask(TaskRef) → CancelAck`, `GetStatus(StatusRequest) → StatusResponse`, `Heartbeat(HeartbeatRequest) → HeartbeatAck`. Message types are declared as protobuf messages; Go stubs are generated with `protoc-gen-go` + `protoc-gen-go-grpc` and checked in. DTOs that are not on the wire (e.g. internal registry snapshots) are plain Go structs. |
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
| FR-17 | **`--priority` scheduling:** Within a worker's incoming task queue, tasks with higher `priority` values (0–9, default 5) are dispatched before lower-priority ones. The host maintains per-worker priority queues using `container/heap` from the Go standard library, guarded by a `sync.Mutex`. |
| FR-18 | **`tag runtime task cancel` propagation:** If the task is `QUEUED`, it is immediately marked `CANCELED` in SQLite. If `DISPATCHED` or `RUNNING`, the host sends a `CancelCommand` to the holding worker; the worker emits a `status_change: CANCELED` event and terminates the agent subprocess. |
| FR-19 | **Generated stubs, no install-time codegen:** Go stubs are generated from `runtime.proto` at development time (`go generate ./internal/runtime/...`) and committed to the repo, so end users building or downloading the single static binary never run `protoc`. `CGO_ENABLED=0` keeps the binary fully static; the `.proto` file remains the single language-neutral source of truth for the wire contract. |
| FR-20 | **Worker-local TAG execution:** Workers execute tasks by calling the existing in-process agent runner in `internal/agent` (same code path as `tag run`). No separate agent binary or container is required. The worker passes the `profile`, `prompt`, `files` working directory, and `env` overrides to the runner and captures streaming output via the existing event-callback mechanism (a Go channel of events). |
| FR-21 | **`tag runtime status --watch`:** When `--watch` is specified, the CLI re-polls the host via `GetStatus` every 2 seconds (a `time.Ticker` loop with `context.Context` cancellation) and re-renders the status table in-place using the existing terminal table renderer. |
| FR-22 | **Worker drain on SIGTERM:** When a worker receives `SIGTERM` or a `DrainCommand` from the host, it finishes any in-progress task, reports the result, and exits cleanly without accepting new assignments. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Zero init overhead on cold path.** The single-machine `tag run` path must never construct a `grpc.Server`, dial a channel, or open a runtime SQLite connection. Runtime wiring is confined to the `internal/runtime` package and only invoked from the `runtime` and `submit --runtime grpc://` cobra subcommands. Verified by a benchmark plus a test that runs a `tag run` and asserts no gRPC listener is opened. |
| NFR-02 | **Startup latency.** `tag runtime host start` must be ready to accept connections within 2 seconds on any supported platform (the single static Go binary has no interpreter warm-up). Measured from process start to first successful `grpc_health_probe` response against the registered `grpc_health_v1` service. |
| NFR-03 | **Throughput.** A single host with three workers (each concurrency=2) must sustain 6 simultaneous tasks and successfully complete a batch of 100 sequential tasks without memory growth exceeding 50 MB on the host process. |
| NFR-04 | **Streaming latency.** Individual `TaskEvent` messages must propagate from worker to a `tag runtime task stream` subscriber in under 200 ms at p99 on a LAN (≤1 ms RTT). |
| NFR-05 | **SQLite WAL durability.** All task state transitions and event writes use WAL mode (consistent with existing TAG SQLite usage). A host crash mid-transition must not leave the database in an inconsistent state; the next startup reads committed state only. |
| NFR-06 | **Security.** The auth token must be at least 32 characters. Tokens shorter than 32 characters are rejected at startup with an actionable error. The host must log each auth failure with the source IP but must not log the token itself. |
| NFR-07 | **Graceful degradation.** gRPC support is compiled into the single binary, so it is always available — there is no missing-dependency case. If the target host is unreachable, `tag submit --runtime grpc://...` must print a clear connection error (host, port, and the underlying gRPC status) and exit 1 without a Go panic/stack trace. All other `tag` commands are unaffected. |
| NFR-08 | **Toolchain compatibility.** `internal/runtime` targets Go 1.24+ and builds with `CGO_ENABLED=0` for a fully static binary. Concurrent event dispatch uses goroutines coordinated with `golang.org/x/sync/errgroup`; task state transitions use a typed state enum with a `switch` over allowed transitions. |
| NFR-09 | **Log structured output.** All host and worker log lines are JSON-structured when `--log-level debug` is set: `{"ts": "<iso>", "level": "...", "event": "...", "task_id": "...", "worker_id": "..."}`. Human-readable by default. |
| NFR-10 | **TLS recommendation.** When TLS is not configured, the host prints a startup warning: `WARNING: gRPC endpoint is not TLS-protected. Use --tls-cert and --tls-key for production deployments.` |
| NFR-11 | **Idempotent worker registration.** If a worker reconnects with the same `worker_id`, the host updates the existing registry row rather than creating a duplicate. In-progress task assignments are reconciled based on the worker's reported state. |
| NFR-12 | **Maximum task payload.** Total `TaskRequest` payload (prompt + files) must not exceed 100 MB. The host rejects larger payloads with gRPC status `RESOURCE_EXHAUSTED` (code 8). |

---

## 9. Technical Design

### 9.1 New Files

- **`internal/runtime/proto/runtime.proto`** — Language-neutral protobuf service + message definitions (the source of truth for the wire contract).
- **`internal/runtime/proto/runtime.pb.go`, `runtime_grpc.pb.go`** — Generated Go stubs (`protoc-gen-go` + `protoc-gen-go-grpc`), committed to the repo. Regenerated via `go generate`.
- **`internal/runtime/host.go`** — `TagRuntimeHost`: the gRPC server, dispatch scheduler, task state machine, SQLite integration, metrics registration, and graceful-shutdown handling.
- **`internal/runtime/worker.go`** — `TagRuntimeWorker`: outbound client, registration/heartbeat loop, task execution bridge, reconnection with backoff.
- **`internal/runtime/store.go`** — `modernc.org/sqlite`-backed persistence for `runtime_tasks`, `runtime_task_events`, and `runtime_workers`.
- **`internal/server/runtime_cmd.go`** — Cobra command handlers: `runtimeHostStart`, `runtimeWorkerStart`, `runtimeStatus`, `runtimeTaskGet`, `runtimeTaskCancel`, `runtimeTaskStream`, `submitRemote`.

No new top-level directories beyond the `internal/runtime` package. It follows the same package pattern as `internal/queue` and `internal/kanban`.

### 9.2 SQLite DDL

All tables live in the existing `~/.tag/runtime/tag.sqlite3` database, opened via the shared `store.Open()` helper backed by `modernc.org/sqlite` (pure-Go, CGO-free driver registered as `sqlite`). DDL is language-neutral and unchanged by the Go migration.

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
    go_version         TEXT             -- Go toolchain version the worker was built with
);
```

### 9.3 Wire Contract (`runtime.proto`) and Go Types

Message types are defined once in protobuf; `protoc-gen-go` emits the Go structs and `google.golang.org/protobuf` handles serialization (no hand-rolled JSON codecs). Timestamps use `google.protobuf.Timestamp`; the auth token travels as gRPC metadata, not as a message field.

```proto
// internal/runtime/proto/runtime.proto
syntax = "proto3";
package tag.runtime.v1;
option go_package = "github.com/tag-agent/tag/internal/runtime/proto;runtimepb";

import "google/protobuf/timestamp.proto";

service TagRuntimeService {
  rpc RegisterWorker(stream WorkerMessage) returns (stream WorkerCommand);
  rpc SubmitTask(TaskRequest) returns (TaskAck);
  rpc StreamTaskEvents(TaskRef) returns (stream TaskEvent);
  rpc GetTaskResult(TaskRef) returns (TaskResult);
  rpc CancelTask(TaskRef) returns (CancelAck);
  rpc GetStatus(StatusRequest) returns (StatusResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatAck);
}

message WorkerInfo {
  string worker_id = 1;
  string hostname = 2;
  repeated string profiles = 3;
  int32 concurrency = 4;
  string tag_version = 5;
  string go_version = 6;
}

// Bidi client->host frame: first message is registration, then heartbeats/events.
message WorkerMessage {
  oneof kind {
    WorkerInfo register = 1;
    HeartbeatRequest heartbeat = 2;
    TaskEvent event = 3;
  }
}

message FileBlob { string name = 1; bytes content = 2; }

message TaskRequest {
  string profile = 1;
  string prompt = 2;
  repeated FileBlob files = 3;      // raw bytes; proto handles framing (no base64)
  map<string, string> env = 4;
  int32 priority = 5;               // 0-9, default 5
  int32 timeout_seconds = 6;        // 0 = no timeout
  string task_id = 7;               // "task-" + first 8 hex of a UUID, assigned at submit
  google.protobuf.Timestamp submitted_at = 8;
  string submitter_host = 9;
}

message TaskAck   { string task_id = 1; TaskStatus status = 2; string message = 3; }
message TaskRef   { string task_id = 1; bool from_beginning = 2; }
message CancelAck { string task_id = 1; TaskStatus status = 2; }

enum EventType {
  STATUS_CHANGE = 0;
  THINKING = 1;
  TOOL_CALL = 2;
  TOOL_RESULT = 3;
  PARTIAL_OUTPUT = 4;
  FINAL_OUTPUT = 5;
  COST = 6;
}

message TaskEvent {
  string task_id = 1;
  EventType event_type = 2;
  string payload = 3;               // JSON-encoded, event-type-specific fields
  google.protobuf.Timestamp emitted_at = 4;
}

message WorkerCommand {
  enum Command { ASSIGN_TASK = 0; CANCEL_TASK = 1; DRAIN = 2; SHUTDOWN = 3; }
  Command command = 1;
  TaskRequest task_request = 2;     // set for ASSIGN_TASK
  string task_id = 3;               // set for CANCEL_TASK
  string message = 4;
}

message HeartbeatRequest {
  string worker_id = 1;
  int32 in_progress = 2;
  repeated string task_ids_in_progress = 3;
  google.protobuf.Timestamp timestamp = 4;
}
message HeartbeatAck { bool ok = 1; }

enum TaskStatus { QUEUED = 0; DISPATCHED = 1; RUNNING = 2; COMPLETED = 3; FAILED = 4; CANCELED = 5; }

message StatusRequest  { string profile_filter = 1; }
message StatusResponse {
  string host_address = 1;
  google.protobuf.Timestamp timestamp = 2;
  repeated WorkerStatus workers = 3;
  repeated TaskSummary tasks = 4;
  int32 queue_depth = 5;
  int32 running = 6;
  int32 completed_today = 7;
  int32 failed_today = 8;
}
message WorkerStatus { string worker_id = 1; repeated string profiles = 2; int32 concurrency = 3; int32 in_progress = 4; string status = 5; google.protobuf.Timestamp last_heartbeat = 6; }
message TaskSummary  { string task_id = 1; string profile = 2; TaskStatus status = 3; string worker_id = 4; }
message TaskResult   { string task_id = 1; TaskStatus status = 2; string output = 3; string error = 4; double cost_usd = 5; int64 tokens_in = 6; int64 tokens_out = 7; }
```

The task state machine lives in Go as a typed enum plus an allowed-transition table, validated with a `switch`:

```go
// internal/runtime/state.go
package runtime

import runtimepb "github.com/tag-agent/tag/internal/runtime/proto"

// allowedTransitions maps a current status to the set of legal next statuses.
var allowedTransitions = map[runtimepb.TaskStatus][]runtimepb.TaskStatus{
    runtimepb.TaskStatus_QUEUED:     {runtimepb.TaskStatus_DISPATCHED, runtimepb.TaskStatus_CANCELED},
    runtimepb.TaskStatus_DISPATCHED: {runtimepb.TaskStatus_RUNNING, runtimepb.TaskStatus_QUEUED, runtimepb.TaskStatus_CANCELED},
    runtimepb.TaskStatus_RUNNING:    {runtimepb.TaskStatus_COMPLETED, runtimepb.TaskStatus_FAILED, runtimepb.TaskStatus_CANCELED},
    // COMPLETED, FAILED, CANCELED are terminal (no entries => no legal transitions).
}

func canTransition(from, to runtimepb.TaskStatus) bool {
    for _, next := range allowedTransitions[from] {
        if next == to {
            return true
        }
    }
    return false
}

func taskID() string { // "task-" + first 8 hex chars of a UUIDv4
    return "task-" + uuid.NewString()[:8]
}
```

### 9.4 Host Architecture

The host is a standard `google.golang.org/grpc` server implementing the generated `TagRuntimeServiceServer` interface. There is no event loop: each RPC runs in its own goroutine, per-worker command delivery uses a buffered Go channel, and event fan-out uses a slice of subscriber channels guarded by a mutex. Auth is enforced by a `grpc.UnaryServerInterceptor` / `grpc.StreamServerInterceptor` pair (chained with the `otelgrpc` interceptor), so individual handlers do not repeat token checks.

```go
// internal/runtime/host.go (host core, simplified)
package runtime

import (
    "context"
    "crypto/subtle"
    "sync"

    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/metadata"
    "google.golang.org/grpc/status"

    runtimepb "github.com/tag-agent/tag/internal/runtime/proto"
)

type workerState struct {
    info       *runtimepb.WorkerInfo
    cmds       chan *runtimepb.WorkerCommand // buffered per-worker command queue
    inProgress int
    healthy    bool
}

type TagRuntimeHost struct {
    runtimepb.UnimplementedTagRuntimeServiceServer

    port      int
    authToken string
    store     *Store // modernc.org/sqlite-backed persistence
    strategy  string // "affinity" | "round-robin" | "least-loaded"

    mu       sync.Mutex
    workers  map[string]*workerState                // worker_id -> state
    subs     map[string][]chan *runtimepb.TaskEvent // task_id -> event subscribers
    dispatch *taskHeap                              // container/heap, priority-ordered
}

// authInterceptor validates the bearer token once for every RPC.
func (h *TagRuntimeHost) authInterceptor(ctx context.Context) error {
    md, _ := metadata.FromIncomingContext(ctx)
    var got string
    if v := md.Get("authorization"); len(v) == 1 {
        got, _ = strings.CutPrefix(v[0], "Bearer ")
    }
    // constant-time comparison (equivalent of Python hmac.compare_digest)
    if subtle.ConstantTimeCompare([]byte(got), []byte(h.authToken)) != 1 {
        return status.Error(codes.Unauthenticated, "invalid auth token")
    }
    return nil
}

// RegisterWorker is a bidi stream: the worker sends a WorkerInfo frame, then
// heartbeats/events; the host pushes WorkerCommand messages. Two goroutines are
// coordinated with errgroup — one draining the command channel to the stream,
// one receiving worker frames — both cancelled when the stream context ends.
func (h *TagRuntimeHost) RegisterWorker(stream runtimepb.TagRuntimeService_RegisterWorkerServer) error {
    ctx := stream.Context()
    first, err := stream.Recv()
    if err != nil {
        return err
    }
    info := first.GetRegister()
    if info == nil {
        return status.Error(codes.InvalidArgument, "first frame must be WorkerInfo")
    }
    ws := h.onWorkerConnect(info)
    defer h.onWorkerDisconnect(info.WorkerId)

    g, gctx := errgroup.WithContext(ctx)
    g.Go(func() error { // send commands
        for {
            select {
            case <-gctx.Done():
                return gctx.Err()
            case cmd := <-ws.cmds:
                if err := stream.Send(cmd); err != nil {
                    return err
                }
            }
        }
    })
    g.Go(func() error { return h.recvWorkerMessages(gctx, info.WorkerId, stream) })
    return g.Wait()
}

// dispatchLoop continuously matches queued tasks to available workers.
// Runs as a goroutine started at host boot; exits on ctx cancellation.
func (h *TagRuntimeHost) dispatchLoop(ctx context.Context) {
    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            for h.tryDispatchNext(ctx) { // keep dispatching while progress is made
            }
        }
    }
}

func (h *TagRuntimeHost) tryDispatchNext(ctx context.Context) bool {
    h.mu.Lock()
    defer h.mu.Unlock()
    if h.dispatch.Len() == 0 {
        return false
    }
    top := h.dispatch.Peek() // highest priority, FIFO on ties
    wid := h.selectWorker(top.profile)
    if wid == "" {
        return false
    }
    heap.Pop(h.dispatch)
    req := h.store.LoadTaskRequest(ctx, top.taskID)
    h.workers[wid].cmds <- &runtimepb.WorkerCommand{
        Command:     runtimepb.WorkerCommand_ASSIGN_TASK,
        TaskRequest: req,
    }
    h.transitionTask(ctx, top.taskID, runtimepb.TaskStatus_DISPATCHED, wid)
    return true
}

// selectWorker (affinity): lowest load ratio among healthy workers with the profile.
func (h *TagRuntimeHost) selectWorker(profile string) string {
    best, bestRatio := "", 2.0
    for wid, ws := range h.workers {
        if !ws.healthy || ws.inProgress >= int(ws.info.Concurrency) {
            continue
        }
        if !slices.Contains(ws.info.Profiles, profile) {
            continue
        }
        if r := float64(ws.inProgress) / float64(ws.info.Concurrency); r < bestRatio {
            best, bestRatio = wid, r
        }
    }
    return best // "" if no candidate
}
```

### 9.5 Worker Architecture

The worker maintains a bidirectional stream to the host, receives `WorkerCommand` messages, and executes tasks by invoking the existing in-process TAG agent runner. Concurrency is bounded by a buffered channel used as a semaphore; the reconnection loop uses `cenkalti/backoff/v4`. The token is attached once to the outgoing context with `metadata.AppendToOutgoingContext`.

```go
// internal/runtime/worker.go (worker core, simplified)
package runtime

import (
    "context"
    "os"
    "path/filepath"
    "sync"
    "time"

    "github.com/cenkalti/backoff/v4"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    "google.golang.org/grpc/metadata"

    runtimepb "github.com/tag-agent/tag/internal/runtime/proto"
    "github.com/tag-agent/tag/internal/agent"
)

type TagRuntimeWorker struct {
    host      string // "host:50051"
    workerID  string
    profiles  []string
    conc      int
    authToken string
    hbEvery   time.Duration

    sem        chan struct{}       // buffered to `conc`; acts as a semaphore
    mu         sync.Mutex
    inProgress map[string]context.CancelFunc // task_id -> cancel (for CancelTask)
}

// Run reconnects with exponential backoff (base 5s, max 60s, jitter).
func (w *TagRuntimeWorker) Run(ctx context.Context) error {
    w.sem = make(chan struct{}, w.conc)
    bo := backoff.NewExponentialBackOff()
    bo.InitialInterval, bo.MaxInterval = 5*time.Second, 60*time.Second
    return backoff.RetryNotify(
        func() error { return w.connectAndServe(ctx) },
        backoff.WithContext(bo, ctx),
        func(err error, d time.Duration) {
            slog.Warn("worker disconnected, retrying", "err", err, "in", d)
        },
    )
}

func (w *TagRuntimeWorker) connectAndServe(ctx context.Context) error {
    conn, err := grpc.NewClient(w.host,
        grpc.WithTransportCredentials(insecure.NewCredentials()), // TLS wired via creds when configured
        grpc.WithChainStreamInterceptor(otelgrpc.StreamClientInterceptor()),
    )
    if err != nil {
        return err
    }
    defer conn.Close()

    client := runtimepb.NewTagRuntimeServiceClient(conn)
    ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+w.authToken)
    stream, err := client.RegisterWorker(ctx)
    if err != nil {
        return err
    }

    // Send registration frame, then heartbeats on a ticker in a goroutine.
    if err := stream.Send(&runtimepb.WorkerMessage{Kind: &runtimepb.WorkerMessage_Register{
        Register: &runtimepb.WorkerInfo{
            WorkerId: w.workerID, Hostname: hostname(),
            Profiles: w.profiles, Concurrency: int32(w.conc),
        }}}); err != nil {
        return err
    }
    go w.heartbeatLoop(ctx, stream)

    for {
        cmd, err := stream.Recv()
        if err != nil {
            return err // triggers backoff reconnect
        }
        switch cmd.Command {
        case runtimepb.WorkerCommand_ASSIGN_TASK:
            go w.executeTask(ctx, stream, cmd.TaskRequest)
        case runtimepb.WorkerCommand_CANCEL_TASK:
            w.cancelTask(cmd.TaskId)
        case runtimepb.WorkerCommand_DRAIN, runtimepb.WorkerCommand_SHUTDOWN:
            return nil // clean exit; caller drains in-progress work
        }
    }
}

// executeTask runs the TAG agent for the task and streams events back over the bidi stream.
func (w *TagRuntimeWorker) executeTask(ctx context.Context, stream runtimepb.TagRuntimeService_RegisterWorkerClient, req *runtimepb.TaskRequest) {
    w.sem <- struct{}{}          // acquire
    defer func() { <-w.sem }()   // release

    taskCtx, cancel := context.WithCancel(ctx)
    if req.TimeoutSeconds > 0 {
        taskCtx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
    }
    w.track(req.TaskId, cancel)
    defer w.untrack(req.TaskId)

    // Decode uploaded files into a temp dir (bytes come straight off the wire).
    workdir, _ := os.MkdirTemp("", "tag-task-"+req.TaskId+"-")
    defer os.RemoveAll(workdir)
    for _, f := range req.Files {
        _ = os.WriteFile(filepath.Join(workdir, filepath.Base(f.Name)), f.Content, 0o600)
    }

    w.emit(stream, statusEvent(req.TaskId, "DISPATCHED", "RUNNING"))

    // Bridge into the existing in-process agent runner: it returns a channel of events.
    events, errc := agent.RunForRuntime(taskCtx, agent.RuntimeJob{
        Profile: req.Profile, Prompt: req.Prompt, Workdir: workdir, Env: req.Env,
    })
    for ev := range events {
        w.emit(stream, &runtimepb.TaskEvent{
            TaskId: req.TaskId, EventType: ev.Type, Payload: ev.PayloadJSON,
        })
    }
    if err := <-errc; err != nil {
        w.emit(stream, failEvent(req.TaskId, err))
    }
}
```

### 9.6 Integration with `internal/agent`

`internal/agent` gains a `RunForRuntime` function that wraps the existing agent execution path and streams structured events over a Go channel. Instead of Python's async generator + sentinel, it returns a receive-only events channel plus a one-shot error channel; the caller ranges over the events channel until it closes, then reads the terminal error. Cancellation and timeout are carried by `context.Context` (set up by the worker), so no separate timeout primitive is needed.

```go
// internal/agent/runtime_bridge.go
package agent

import "context"

// RuntimeEvent mirrors the runtime TaskEvent shape; PayloadJSON is the
// event-type-specific body already marshaled to JSON.
type RuntimeEvent struct {
    Type        runtimepb.EventType
    PayloadJSON string
}

type RuntimeJob struct {
    Profile string
    Prompt  string
    Workdir string
    Env     map[string]string
}

// RunForRuntime runs a TAG agent and emits RuntimeEvents on the returned channel,
// bridging the existing agent callback hooks into a channel-based event stream.
// The events channel is closed when the run finishes; the final error (or nil)
// is delivered exactly once on errc.
func RunForRuntime(ctx context.Context, job RuntimeJob) (<-chan RuntimeEvent, <-chan error) {
    events := make(chan RuntimeEvent, 64)
    errc := make(chan error, 1)

    go func() {
        defer close(events)
        cfg, err := config.Load()
        if err != nil {
            errc <- err
            return
        }
        profileCfg, err := config.LoadProfile(cfg, job.Profile)
        if err != nil {
            errc <- err
            return
        }

        emit := func(t runtimepb.EventType, payload any) {
            b, _ := json.Marshal(payload)
            select {
            case events <- RuntimeEvent{Type: t, PayloadJSON: string(b)}:
            case <-ctx.Done():
            }
        }

        hooks := AgentHooks{
            OnThinking:   func(text string) { emit(runtimepb.EventType_THINKING, map[string]string{"text": text}) },
            OnToolCall:   func(name string, args map[string]any) { emit(runtimepb.EventType_TOOL_CALL, map[string]any{"name": name, "args": args}) },
            OnToolResult: func(name, result string) { emit(runtimepb.EventType_TOOL_RESULT, map[string]string{"name": name, "result": result}) },
            OnOutput: func(text string, cost *CostInfo) {
                emit(runtimepb.EventType_FINAL_OUTPUT, map[string]string{"output": text})
                if cost != nil {
                    emit(runtimepb.EventType_COST, cost)
                }
            },
        }

        // ctx carries the deadline/cancellation set up by the worker.
        errc <- runAgent(ctx, profileCfg, job.Prompt, job.Workdir, job.Env, hooks)
    }()

    return events, errc
}
```

### 9.7 gRPC Service Registration (Generated Stubs)

> **Note (Go reframing):** the original Python design used raw byte-stream handlers specifically to avoid a `protoc` step at install time. That constraint disappears in Go: stubs are generated once at development time and committed, and they compile into the single static binary. The host therefore registers the strongly-typed generated server rather than hand-rolled generic handlers — a net simplification, not a workaround.

Stubs are generated via a `//go:generate` directive and the standard plugins:

```go
// internal/runtime/proto/gen.go
//go:generate protoc --go_out=. --go_opt=paths=source_relative \
//   --go-grpc_out=. --go-grpc_opt=paths=source_relative runtime.proto
package runtimepb
```

Server wiring registers the typed service, the health service, and the OTel interceptors:

```go
// internal/runtime/serve.go
func (h *TagRuntimeHost) Serve(ctx context.Context, lis net.Listener) error {
    srv := grpc.NewServer(
        grpc.ChainUnaryInterceptor(
            otelgrpc.UnaryServerInterceptor(),
            h.unaryAuth, // returns codes.Unauthenticated on bad token
        ),
        grpc.ChainStreamInterceptor(
            otelgrpc.StreamServerInterceptor(),
            h.streamAuth,
        ),
        grpc.MaxRecvMsgSize(100<<20), // 100 MB payload cap (NFR-12)
    )

    runtimepb.RegisterTagRuntimeServiceServer(srv, h)

    // Service discovery / liveness via the standard gRPC health protocol.
    hsrv := health.NewServer()
    grpc_health_v1.RegisterHealthServer(srv, hsrv)
    hsrv.SetServingStatus("tag.runtime.v1.TagRuntimeService", grpc_health_v1.HealthCheckResponse_SERVING)

    go h.dispatchLoop(ctx)
    go func() { <-ctx.Done(); srv.GracefulStop() }() // SIGTERM -> graceful drain
    return srv.Serve(lis)
}
```

### 9.8 Prometheus Metrics Exporter

RPC-level metrics (dispatch latency, request counts) are produced automatically by the `otelgrpc` interceptor. The runtime-specific counters/gauges/histograms are registered with `prometheus/client_golang` and exposed on a separate port via `promhttp` — compiled into the binary, so there is no "metrics disabled if not installed" branch. Metric names are unchanged from the original contract (FR-14).

```go
// internal/runtime/metrics.go
package runtime

import (
    "net/http"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

type metrics struct {
    submitted       *prometheus.CounterVec   // labels: profile
    completed       *prometheus.CounterVec   // labels: profile, status
    workersConn     prometheus.Gauge
    dispatchLatency prometheus.Histogram
    taskDuration    *prometheus.HistogramVec // labels: profile
}

func newMetrics(reg prometheus.Registerer) *metrics {
    f := promauto.With(reg)
    return &metrics{
        submitted: f.NewCounterVec(prometheus.CounterOpts{
            Name: "tag_runtime_tasks_submitted_total",
            Help: "Tasks submitted to the runtime host",
        }, []string{"profile"}),
        completed: f.NewCounterVec(prometheus.CounterOpts{
            Name: "tag_runtime_tasks_completed_total",
            Help: "Tasks completed by runtime workers",
        }, []string{"profile", "status"}),
        workersConn: f.NewGauge(prometheus.GaugeOpts{
            Name: "tag_runtime_workers_connected",
            Help: "Number of workers currently connected",
        }),
        dispatchLatency: f.NewHistogram(prometheus.HistogramOpts{
            Name:    "tag_runtime_task_dispatch_latency_seconds",
            Help:    "Seconds between task submission and worker dispatch",
            Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
        }),
        taskDuration: f.NewHistogramVec(prometheus.HistogramOpts{
            Name:    "tag_runtime_task_duration_seconds",
            Help:    "Total task execution duration in seconds",
            Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600},
        }, []string{"profile"}),
    }
}

// startMetricsServer serves /metrics on its own port; a metricsPort of 0 disables it.
func startMetricsServer(ctx context.Context, port int, reg *prometheus.Registry) *http.Server {
    mux := http.NewServeMux()
    mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
    srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
    go func() { _ = srv.ListenAndServe() }()
    go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
    return srv
}
```

### 9.9 `go.mod` Dependencies

Runtime support is not an optional install extra — it compiles into the single static binary. The relevant module requirements:

```
// go.mod (module github.com/tag-agent/tag; go 1.24)
require (
    google.golang.org/grpc v1.68.0
    google.golang.org/protobuf v1.36.0
    github.com/prometheus/client_golang v1.20.0
    go.opentelemetry.io/otel v1.32.0
    go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.57.0
    github.com/cenkalti/backoff/v4 v4.3.0
    golang.org/x/sync v0.10.0            // errgroup
    modernc.org/sqlite v1.34.1           // pure-Go, CGO_ENABLED=0
)
```

Built and released as a single static binary via GoReleaser (with cosign signing and SLSA provenance); `grpc_health_probe` is used only in tests/ops, not shipped as a dependency.

---

## 10. Security Considerations

1. **Bearer token minimum entropy.** The auth token must be at least 32 characters. On startup, if no token is configured, the host generates a cryptographically random token by reading 32 bytes from `crypto/rand` and hex-encoding them (256 bits of entropy) and prints it once. The token is never written to SQLite or log files; it is only stored in the process's in-memory config and in `cli-config.yaml` (mode 0600), loaded via `koanf/v2` + `yaml.v3`.

2. **Token comparison timing safety.** The host validates tokens using `crypto/subtle.ConstantTimeCompare` (constant-time comparison) to prevent timing oracle attacks.

3. **TLS for production deployments.** Without TLS, auth tokens and task payloads (including file contents and prompts) are transmitted in plaintext. The host emits a startup warning and documentation must state that TLS is required for any non-localhost deployment. Provide a quickstart using `mkcert` for LAN deployments.

4. **File payload sanitization.** Uploaded files from `tag submit --files` are written to an isolated temporary directory on the worker, not to the worker's working directory or home. The worker must validate that decoded file paths do not contain `..` path traversal sequences before writing.

5. **Task prompt injection.** Worker operators must be aware that the prompt field of `TaskRequest` is executed verbatim by the TAG agent. A malicious host can inject arbitrary prompts. Workers should only connect to hosts they trust (controlled by the same organization). Future enhancement: sign `TaskRequest` payloads with the host's private key.

6. **gRPC reflection disabled by default.** The host must not register `google.golang.org/grpc/reflection` in production mode, as it exposes the full service schema to unauthenticated clients. Reflection can be enabled with `--debug-reflection` for development tooling (e.g. `grpcurl`).

7. **Worker isolation.** Workers run with the same OS user as the `tag runtime worker start` invocation. If the TAG sandbox (PRD-028) is configured for a profile, the worker's agent execution uses the sandbox. Operators should run workers under dedicated low-privilege OS users.

8. **IP logging for auth failures.** Each failed auth attempt is logged with the client's IP address (from gRPC peer metadata) at WARN level, without logging the submitted token. Rate-limiting auth failures (max 10/minute per IP) is recommended but not implemented in v1.

9. **Prompt/output data residency.** Task prompts, file contents, and outputs are stored in the host's SQLite database. Operators must be aware of data residency implications when running the host in a different jurisdiction from where the task data originates.

10. **Timeout enforcement.** The `timeout_seconds` field in `TaskRequest` is enforced by the worker via a `context.WithTimeout` deadline on the task context. When the deadline fires, the context is canceled (propagating to any agent subprocess via `exec.CommandContext`, which sends the configured kill signal) and a `FAILED` event with `reason: timeout` is emitted. Callers must not rely on graceful cleanup within the agent on timeout.

---

## 11. Testing Strategy

### 11.1 Unit Tests

Standard `go test` with table-driven cases. In-process gRPC uses `google.golang.org/grpc/test/bufconn` (an in-memory listener) so no real ports are bound.

- **`internal/runtime/proto_test.go`**: Protobuf marshal/unmarshal roundtrip for all messages (`WorkerInfo`, `TaskRequest`, `TaskEvent`, `WorkerCommand`). Edge cases: empty profiles slice, zero-valued optional fields, UTF-8 prompt with emoji, priority 0 and 9, binary file blobs.
- **`internal/runtime/state_test.go`**: Table-driven test of `canTransition` over all allowed and disallowed pairs. Verify illegal transitions are rejected (returns `false`, state unchanged). Test the `container/heap` dispatch queue invariant (higher priority dequeues first, FIFO on ties).
- **`internal/runtime/auth_test.go`**: Verify `subtle.ConstantTimeCompare` acceptance/rejection, too-short token rejection at startup, and that the auth interceptor returns `codes.Unauthenticated` on a wrong token (asserted via `status.Code(err)`).
- **`internal/runtime/init_test.go`**: Assert the single-machine `tag run` path constructs no `grpc.Server` and dials no channel (via an injected listener/dialer counter). A `go test -bench` guards against init-cost regressions.
- **`internal/runtime/dispatch_test.go`**: Test affinity selection over various worker+profile combinations. Test least-loaded selection. Test round-robin ordering. Test that tasks remain QUEUED when no matching worker is connected.

### 11.2 Integration Tests

- **`internal/runtime/e2e_test.go`**: Spins up a host and worker in-process against a `bufconn` listener (and a separate variant launching the compiled binary with `os/exec` on `--port 50099`). Submits a task, waits for completion, asserts the task appears as COMPLETED in `GetStatus`. Uses `t.Cleanup` to tear down goroutines/subprocesses.
- **`internal/runtime/reconnect_test.go`**: Starts host, connects worker, stops the host, restarts it, verifies the worker reconnects (backoff loop) within 30 seconds.
- **`internal/runtime/cancel_test.go`**: Submits a long-running task (>10s), cancels it via `CancelTask`, verifies the task reaches CANCELED and the task context is canceled on the worker.
- **`internal/runtime/replay_test.go`**: Submits a task, lets it complete, then calls `StreamTaskEvents` with `from_beginning=true` and verifies all stored events replay in order.
- **`internal/runtime/fileupload_test.go`**: Submits a task with a small file blob, verifies the worker writes it into the temp workdir and the agent output references it.

### 11.3 Performance / Benchmark Tests

- **`internal/runtime/bench_throughput_test.go`**: 1 host + 3 workers (concurrency=2 each) over `bufconn`. Submits 100 no-op tasks (profile `echo`). Asserts completion within 30 seconds and, using `runtime.ReadMemStats` / `testing.B` allocation reporting, that host heap growth stays under 50 MB.
- **`internal/runtime/bench_streaming_test.go`**: Measures p99 latency between a worker emitting a `TaskEvent` and its arrival at a `StreamTaskEvents` subscriber, using monotonic `time.Now()` timestamps. Asserts p99 < 200 ms in-process.
- **`internal/runtime/bench_dispatch_test.go`**: Verifies 10 concurrently-submitted tasks are all dispatched within 1 second with no dropped events (goroutine-driven concurrency).

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
| AC-14 | Zero gRPC init on cold path | `runtime.InitTest` (a `go test` that runs `tag run` and inspects the injected listener/dialer counters) confirms no gRPC server or channel is created; `init` benchmark shows no regression |
| AC-15 | Unreachable host handled gracefully | `tag submit --runtime grpc://badhost:50051` prints a clear connection error (host, port, gRPC status) and exits 1, not a Go panic/stack trace |
| AC-16 | Worker drain on SIGTERM | `kill -TERM <worker-pid>` causes the worker to finish its current task and exit cleanly |
| AC-17 | Event replay for late subscribers | `tag runtime task stream <completed-id> --from-beginning` replays all stored events in chronological order |
| AC-18 | Unhealthy worker task re-queue | Kill worker process mid-task; after 3 missed heartbeats, host re-queues the task to another worker |
| AC-19 | Concurrent tasks with semaphore | Worker with `--concurrency 2` accepts exactly 2 simultaneous tasks and queues the third until one finishes |
| AC-20 | Token minimum entropy enforcement | `tag runtime host start --token "short"` exits 1 with "Auth token must be at least 32 characters" |

---

## 13. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `google.golang.org/grpc` | Go module | `>=1.68.0` | Core gRPC transport (server + client). Compiled into the single static binary. |
| `google.golang.org/protobuf` | Go module | `>=1.36.0` | Wire serialization; stubs via `protoc-gen-go` + `protoc-gen-go-grpc`. |
| `google.golang.org/grpc/health` + `grpc_health_v1` | Go module (in grpc) | — | Service discovery / liveness health checks. |
| `github.com/prometheus/client_golang` | Go module | `>=1.20.0` | Prometheus metrics registry + `promhttp` `/metrics` handler. |
| `go.opentelemetry.io/otel` + `.../otelgrpc` | Go module | `otel >=1.32.0` | Tracing/metrics interceptors on the gRPC server and client. |
| `github.com/cenkalti/backoff/v4` | Go module | `>=4.3.0` | Exponential-backoff reconnection for workers. |
| `golang.org/x/sync/errgroup` | Go module | `>=0.10.0` | Coordinate the send/receive goroutines of the bidi stream. |
| `modernc.org/sqlite` | Go module | `>=1.34.0` | Pure-Go, CGO-free SQLite driver for host task/worker persistence. |
| `crypto/subtle`, `crypto/rand` | Go stdlib | — | Constant-time token comparison and cryptographic token generation. |
| `context`, `os`, `container/heap` | Go stdlib | — | Cancellation/deadlines, temp workdirs for file uploads, priority queue. |
| PRD-013 (tracing) | Internal | — | Trace IDs assigned to runtime tasks; carried via `otelgrpc` context propagation. |
| PRD-028 (sandbox) | Internal | — | Workers honor per-profile sandbox config for agent execution. |
| PRD-034 (security) | Internal | — | Token validation follows the credential hygiene patterns from PRD-034. |
| PRD-012 (budget) | Internal | — | `budget.RecordSpend` (`internal/budget`) called on host with task cost data. |
| PRD-008 (queue) | Internal | — | Conceptual predecessor; runtime supersedes queue for multi-machine scenarios. |
| `internal/agent.RunForRuntime` | Internal | — | New function in `internal/agent` returning an events channel to bridge agent execution into the worker's event stream. |

---

## 14. Open Questions

| # | Question | Owner | Target Date |
|---|----------|-------|-------------|
| OQ-1 | **Concurrency model.** *(Largely resolved by the Go move.)* `google.golang.org/grpc` uses a goroutine-per-RPC model with no async/sync split, eliminating the original `grpc.aio`-vs-threads question and its macOS caveats. Remaining sub-question: should the per-worker command channel be bounded (backpressure) or unbounded? | Runtime lead | Before Phase 2 start |
| OQ-2 | **Generated-stub codegen workflow.** *(Reframed for Go.)* Go uses committed `protoc-gen-go`/`protoc-gen-go-grpc` stubs, so there is no runtime-codegen tradeoff. Open sub-question: pin `protoc` + plugin versions via a tool dependency (`tools.go` / `go run`) so contributor codegen is reproducible, or require a documented local `protoc`? | Tech lead | Architecture review |
| OQ-3 | **Worker task resumption after crash.** If the worker OS process crashes mid-task (not a graceful disconnect), the task is re-queued by the host after 3 missed heartbeats. But the agent may have partially modified files. Should we snapshot agent state (e.g., diff context) before execution? | Runtime lead | Phase 3 |
| OQ-4 | **mTLS vs. bearer token auth.** mTLS would eliminate the need for a shared secret (each worker gets a certificate signed by the host's CA). More operationally complex but more secure. Treat as a v2 enhancement or required for v1? | Security | Before Phase 1 complete |
| OQ-5 | **Event fanout scalability.** The current design uses one buffered Go channel per `StreamTaskEvents` subscriber, with the host fanning out under a mutex. With many concurrent subscribers watching the same task, this could become a bottleneck; a `sync.Cond`- or broadcast-based mechanism may be needed at scale. | Runtime lead | Phase 2 |
| OQ-6 | **`RunForRuntime` agent bridge placement.** The proposal adds `RunForRuntime` to `internal/agent`. Should it instead live in a dedicated `internal/agent/runtime_bridge.go` (or a separate `internal/runtimebridge` package) to keep the runtime event-channel concerns separated from the core agent loop? | Lead engineer | Design review |
| OQ-7 | **SQLite write concurrency on the host.** With `modernc.org/sqlite`, event writes from many workers arrive on separate goroutines; even with WAL mode, a single writer serializes them. Should events be batched (e.g., flushed every 100 ms) or funneled through a dedicated writer goroutine + channel? | Runtime lead | Phase 2 performance testing |
| OQ-8 | **GitHub issue #347 A2A interoperability.** Should the host also expose an A2A-compatible endpoint (JSON-RPC 2.0 over HTTP+SSE at `/.well-known/agent-card.json`) so that A2A clients can submit tasks without the TAG runtime client? This would make the host interoperable with the broader A2A ecosystem. | Product | Before v1.0 release |
| OQ-9 | **Worker identity for audit.** Should the `runtime_tasks` table record the submitter's identity (not just IP)? This requires extending `tag submit` to include a user identity claim in the task request. | Security | Before enterprise pilot |

---

## 15. Complexity and Timeline

### Phase 1 — Core Infrastructure (Weeks 1–2, ~10 days)

**Goal:** A working host and worker on localhost. Task submission, dispatch, execution, and result retrieval. No TLS, no Prometheus, no file uploads.

- Day 1–2: Author `runtime.proto`; generate and commit Go stubs (`protoc-gen-go` + `protoc-gen-go-grpc`). Implement the state enum + `canTransition`. Write `go test` unit tests for proto roundtrips and the state machine. CI green.
- Day 3–4: Implement `TagRuntimeHost` on `google.golang.org/grpc` with the generated typed server. `RegisterWorker` bidi stream, `SubmitTask` unary, `GetStatus` unary. In-memory worker registry (no SQLite yet); register `grpc_health_v1`.
- Day 5–6: Implement `TagRuntimeWorker` client. Connect, register, receive `ASSIGN_TASK` command, execute via a stub `agent.RunForRuntime` (returns "hello" immediately), emit `FINAL_OUTPUT` event.
- Day 7: Wire `runtimeHostStart` and `runtimeWorkerStart` cobra commands in `internal/server`. `tag runtime host start` and `tag runtime worker start` work end-to-end on localhost.
- Day 8: Implement `runtime_tasks` and `runtime_workers` tables via `modernc.org/sqlite` (`internal/runtime/store.go`). Persist task state transitions. `runtimeTaskGet` works.
- Day 9: Implement affinity dispatch scheduler with a `container/heap` priority queue. Test with 2 workers of different profiles.
- Day 10: Auth interceptor with `subtle.ConstantTimeCompare`. 32-character minimum. End-to-end test over `bufconn` (host + worker + submit). Phase 1 review.

### Phase 2 — Feature Completeness (Weeks 3–4, ~10 days)

**Goal:** All CLI surface implemented. Streaming, cancellation, reconnection, file uploads, Prometheus.

- Day 11–12: `StreamTaskEvents` RPC on host. Event fan-out to subscribers. `runtime_task_events` table. `tag runtime task stream` CLI. Event replay from SQLite.
- Day 13: `CancelTask` RPC. `cancel_task` command sent to worker. Worker SIGKILL of agent subprocess on cancel. CANCELED state in SQLite.
- Day 14: Worker reconnection with exponential backoff. Host detects missed heartbeats and marks workers UNHEALTHY. Task re-queue on worker UNHEALTHY.
- Day 15: `--files` upload in `TaskRequest`. File decoding in worker temp directory. Path traversal validation. 10 MB payload limit check.
- Day 16: Real `agent.RunForRuntime` bridge in `internal/agent`. Wire agent callback hooks into the events channel. Test with `coder` profile on an actual prompt.
- Day 17: Prometheus metrics via `prometheus/client_golang` + `promhttp` on a separate port; `otelgrpc` interceptors for RPC-level metrics. Counters and histograms. Test with `curl`.
- Day 18: `tag runtime status --watch` in-place table rendering (`time.Ticker` + context). `tag runtime worker list` command. `--json` output for all commands.
- Day 19–20: `tag submit --stream`, `--wait`, `--priority`, `--env`, `--timeout` flags. TLS support (`--tls-cert`, `--tls-key` on host; `--tls-ca-cert` on worker). Phase 2 review.

### Phase 3 — Hardening and Documentation (Weeks 5–6, ~8 days)

**Goal:** Production-ready reliability. Performance tests pass. Documentation complete.

- Day 21–22: Performance tests (100-task batch, streaming latency p99). Identify and fix bottlenecks (event queue batching if needed).
- Day 23: Graceful host shutdown (SIGTERM, 30s drain window, `DrainCommand` to workers).
- Day 24: Worker drain on SIGTERM. Worker-side task reconciliation on reconnect.
- Day 25: Security hardening: IP logging for auth failures, gRPC reflection disabled by default, file path sanitization tests.
- Day 26: Full acceptance criteria verification. Run all AC-01 through AC-20.
- Day 27–28: Documentation in `docs/`, CLI `--help` text, `go.mod` dependency entries, GoReleaser config for the static binary (cosign + SLSA), GitHub issue #347 close checklist. Final review and merge.

**Total: 28 engineering days (5–6 weeks with review overhead)**

---

*GitHub Issue: [#347](https://github.com/tag-agent/tag/issues/347)*
*Cluster: E — Multi-Agent Interoperability Protocols*
*PRD Author: Generated 2026-06-17*

