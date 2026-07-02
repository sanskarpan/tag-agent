# PRD-087: ACP (IBM) Lightweight REST Adapter for Intra-Cluster Agent Messaging (`tag acp`)
> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `internal/server` (extend the existing chi/huma API), `internal/acp` (new adapter package), `internal/cli` (new `acp` command tree), `tag.sqlite3` (new `acp_agents`, `acp_runs`, `acp_run_events` tables)
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox), PRD-034 (secret scanning/security), PRD-036 (web dashboard / internal/server patterns), PRD-039 (token budget enforcement), PRD-041 (OTel span cost attribution)
**Inspired by:** IBM ACP (Agent Communication Protocol), BeeAI Framework
**GitHub Issue:** #347

---

## 1. Overview

The Agent Communication Protocol (ACP) is a lightweight HTTP-based messaging standard developed under the Linux Foundation AGNTCY umbrella. Unlike A2A (which mandates JSON-RPC 2.0 over HTTP with Server-Sent Event streaming for task lifecycle management) or ANP (which grounds agent identity in DID documents and cryptographic Ed25519 HTTP Message Signatures per RFC 9421), ACP favors simplicity: an agent is a resource reachable via `POST /runs`, a cluster is a registry reachable via `GET /agents`, and intra-cluster calls are plain JSON over HTTPS with no mandatory streaming transport. This maps well to TAG's design philosophy of small, composable commands that default to synchronous behavior and opt into complexity only when needed.

TAG already runs a lightweight HTTP API server (`internal/server`, PRD-036) that exposes run history, span waterfalls, and queue state to the web dashboard. Extending that server — or running a parallel server on a dedicated port — to speak ACP makes TAG agents first-class participants in any cluster that uses the BeeAI / ACP runtime. This means a TAG agent running `tag acp serve` on port 8081 can receive tasks from orchestrators like BeeAI's `bee-agent-framework`, from other `tag acp send` callers, and from any ACP-compatible client. The `/agents` registry endpoint allows the cluster coordinator to discover what agents are available, what capabilities they advertise, and how to route tasks to them.

The primary operational scenario is intra-cluster: multiple TAG instances (or a TAG instance alongside BeeAI agents) running on a shared Kubernetes namespace or Docker Compose network, communicating over internal DNS without traversing the public internet. ACP's flat HTTP semantics are ideal for this: no service mesh, no gRPC reflection, no OAuth2 discovery document — just POST and GET. The secondary scenario is development: a developer running `tag acp serve` locally while iterating on an agent, and using `tag acp send` from a second terminal to invoke it without writing any client code.

ACP defines a 7-state run lifecycle: `created → in-progress → awaiting → [completed | failed | cancelled | cancelling]`. The `awaiting` state is ACP's mechanism for human-in-the-loop: when an agent needs external input before proceeding, it transitions to `awaiting` and sets `await_request` on the Run object describing what it needs. The caller resumes via `POST /runs/{run_id}` with a `RunResumeRequest` containing `await_resume` — the response to the request. TAG maps this to its existing HITL approval flow (PRD-078) so that `awaiting` runs surface as pending approvals in `tag status` and can be resumed via `tag acp resume`. This PRD defines the full ACP adapter, SQLite schema, CLI surface, and integration points needed to realize this vision within 1-2 weeks of focused development.

The ACP AgentManifest `name` field must conform to RFC 1123 DNS label syntax: lowercase alphanumeric and hyphens only, 1-63 characters, matching `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`. This is stricter than A2A agent names (which are free-form strings) and stricter than TAG profile names (which allow underscores). The adapter enforces this constraint at registration time and maps TAG profile names to valid ACP names by substituting underscores with hyphens and lowercasing.

---

## 2. Problem Statement

### 2.1 TAG agents are invisible to ACP-native orchestrators

The BeeAI Framework and other ACP-compatible orchestrators discover agents via `GET /agents` on a known cluster endpoint. TAG agents do not currently expose this endpoint, so they cannot participate in any BeeAI-managed cluster. Platform engineers who want to use TAG as one agent in a heterogeneous fleet — alongside Python agents built with `bee-agent-framework`, JavaScript agents using `@i-am-bee/acp-sdk`, and Go agents — must write custom adapter code outside of TAG, bypassing TAG's config, tracing, budget enforcement, and security controls. This creates a maintenance burden and an observability gap: tasks dispatched to TAG via a custom adapter are not captured in TAG's SQLite `runs` table and do not appear in `tag status` or the web dashboard.

### 2.2 Intra-cluster agent messaging has no lightweight TAG-native path

TAG's existing multi-agent support (PRD-023 swarm context routing) operates at the level of a single TAG process spawning multiple profile instances via `tag swarm`. For scenarios where agents run as independent processes — or on separate hosts within a cluster — there is no TAG-native way to send a task from agent A to agent B and receive a result. Developers fall back to writing ad-hoc HTTP clients, using message queues, or coupling agents through shared SQLite (which breaks the process-isolation model). ACP's `POST /runs` semantics directly address this: it is the simplest possible RPC primitive that preserves agent autonomy and supports async result polling.

### 2.3 The human-in-the-loop gap in distributed runs

TAG's HITL approval flow (PRD-078) works well within a single TAG process but has no external protocol binding. When a remote orchestrator dispatches a task to a TAG agent and that task reaches a decision point requiring human input, there is no standard way for the orchestrator to learn that the run is blocked, to surface the blocking reason to an operator, or to resume the run with the operator's decision. ACP's `awaiting` state with `await_request` / `RunResumeRequest` provides exactly the right protocol hook: the orchestrator polls `GET /runs/{run_id}`, sees `status=awaiting`, reads `await_request.description` to understand what is needed, collects the human decision, and sends `POST /runs/{run_id}` with `await_resume`. TAG's adapter must translate this ACP pause/resume protocol into calls to the existing HITL approval system.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Implement a spec-compliant ACP REST server exposing `POST /runs`, `GET /runs/{run_id}`, `POST /runs/{run_id}` (resume), `DELETE /runs/{run_id}` (cancel), and `GET /agents` endpoints, launchable via `tag acp serve --port 8081`. |
| G2 | Implement `tag acp send` as an ACP client that submits a run to a remote ACP server, polls for completion, and prints the result. |
| G3 | Implement `tag acp list-agents` to query `GET /agents` on a remote ACP server and display the manifest list in a human-readable table and `--json` mode. |
| G4 | Persist all ACP run state (runs, lifecycle events, input/output messages) to the TAG SQLite database so runs are visible in `tag status`, the web dashboard, and `tag trace`. |
| G5 | Map the full ACP 7-state lifecycle (`created`, `in-progress`, `awaiting`, `completed`, `failed`, `cancelled`, `cancelling`) to entries in `acp_runs`, with lifecycle events in `acp_run_events`. |
| G6 | Translate ACP `awaiting` state into TAG's HITL approval system (PRD-078) and implement `tag acp resume --run-id <id> --data <json>` to send a `RunResumeRequest`. |
| G7 | Enforce ACP AgentManifest name constraints (RFC 1123 DNS label: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`) at registration time, with automatic coercion from TAG profile names. |
| G8 | Propagate TAG's tracing context (PRD-013) into each ACP-dispatched run via `X-TAG-Trace-ID` and `X-TAG-Span-ID` headers, and emit OTel spans for the ACP call. |
| G9 | Enforce token budget limits (PRD-039) per ACP run and return HTTP 402 with an `error.type=budget_exceeded` body when the run would exceed the profile's budget. |
| G10 | The ACP server binds to `127.0.0.1` by default; `--host 0.0.0.0` enables network-accessible mode with an explicit warning about exposure. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Full A2A protocol support (JSON-RPC 2.0, SSE streaming, Agent Card at `/.well-known/agent-card.json`). A2A is a separate adapter (Cluster E, different PRD). |
| NG2 | ANP (Agent Network Protocol) DID-based identity or RFC 9421 HTTP Message Signatures. ANP is a separate adapter. |
| NG3 | gRPC transport. ACP uses HTTP/REST; gRPC is an A2A v1.0 optional binding and is explicitly not part of ACP's design. |
| NG4 | ACP streaming via `AsyncGenerator[RunYield, RunYieldResume]` in this first release. The initial implementation uses synchronous polling. A follow-up PRD may add SSE-based streaming for intermediate `thought` yields. |
| NG5 | OAuth2 or mTLS authentication on the ACP server in this PRD. The server is designed for intra-cluster use where network-level security (VPC, mTLS service mesh, Kubernetes NetworkPolicy) handles authentication. A future security hardening PRD may add Bearer token validation. |
| NG6 | Web dashboard UI changes for ACP runs. The existing dashboard at PRD-036 will display ACP runs via the existing `runs` table; no new dashboard UI is in scope. |
| NG7 | Multi-tenant agent isolation within a single `tag acp serve` process. One server instance hosts one registered agent (the TAG instance's active profile). |
| NG8 | JCS (RFC 8785) signed agent manifests. Manifest signing is an ANP / A2A concern and is not required by the ACP spec. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| ACP compliance | All mandatory endpoints (`POST /runs`, `GET /runs/{id}`, `GET /agents`) return responses that validate against the ACP OpenAPI schema | Automated schema validation test using `openapi-core` or `jsonschema` against the canonical ACP `openapi.yaml` |
| Serve startup time | `tag acp serve` is ready to accept connections within 500 ms of invocation | `time` the first successful `GET /agents` response in the integration test suite |
| Round-trip latency (local) | `tag acp send` to a local `tag acp serve` with a trivial no-op agent completes in under 200 ms total (excluding LLM inference) | Integration test with a mock agent that returns immediately |
| State persistence | 100% of ACP runs are written to `acp_runs` with correct final status | Integration tests asserting SQLite state after each lifecycle path |
| HITL resume | An `awaiting` run resumes correctly after `tag acp resume` delivers `await_resume` data | Integration test covering full `created→awaiting→completed` lifecycle |
| Budget enforcement | A run that would exceed the profile budget receives HTTP 402 before any LLM call is made | Unit test faking the budget package's `budget.Check()` call |
| Name coercion | TAG profile names with underscores and uppercase are coerced to valid RFC 1123 names without collision | Unit test over a table of 20 pathological profile names |
| Zero overhead when idle | `tag run` (non-ACP) wall time is statistically unchanged after this feature lands | Benchmark 20 runs; t-test |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Platform engineer running a BeeAI cluster | start `tag acp serve --port 8081` on a TAG node | BeeAI's orchestrator can discover it via `GET /agents`, dispatch tasks via `POST /runs`, and poll results — without any custom adapter code |
| U2 | Developer building a multi-agent pipeline | run `tag acp send --to http://summarizer-agent:8081 --message "Summarize this report: ..."` | I can call a remote ACP-compatible agent from the command line and see the result, without writing Python client code |
| U3 | Developer iterating on an agent | run `tag acp list-agents --server http://localhost:8081` | I can quickly verify what agents are registered, their manifests, and their capabilities before wiring up an orchestrator |
| U4 | Platform engineer managing an awaiting run | observe in `tag status` that a run is blocked on human input, collect the decision, and run `tag acp resume --run-id <id> --data '{"answer": "yes"}'` | The blocked agent receives the human decision and continues to completion without requiring direct access to the server process |
| U5 | Security engineer | see that `tag acp serve` binds to `127.0.0.1:8081` by default with a warning when `--host 0.0.0.0` is used | Network exposure is opt-in and visible, reducing the risk of accidentally exposing an ACP endpoint on a public interface |
| U6 | DevOps engineer tracing a distributed pipeline | see ACP run spans in `tag trace show` with the same trace ID as the originating orchestrator's span | I can reconstruct the full distributed trace across agent boundaries using the `X-TAG-Trace-ID` propagation headers |
| U7 | Developer running over budget | receive HTTP 402 with a descriptive error body when a submitted run would exceed the profile's token budget | I am not surprised by unexpected LLM spend; the budget contract from PRD-039 is honored even for externally dispatched runs |
| U8 | Cluster operator | cancel a running ACP task via `DELETE /runs/{run_id}` | I can stop a runaway agent without killing the server process or accessing the TAG node directly |

---

## 7. Proposed CLI Surface

All ACP subcommands live under the `tag acp` namespace, wired as a cobra command tree in `internal/cli/acp.go` with business logic delegated to the `internal/acp` package.

### 7.1 `tag acp serve`

Start an ACP-compatible HTTP server on the given port, registering the active TAG profile as an agent.

```
tag acp serve \
  [--port 8081] \
  [--host 127.0.0.1] \
  [--profile <name>] \
  [--agent-name <rfc1123-name>] \
  [--agent-description "..."] \
  [--capabilities run,await] \
  [--pid-file ~/.tag/runtime/acp.pid] \
  [--log-level info] \
  [--json]
```

**Flags:**
- `--port` (default: `8081`): TCP port to bind. Fails with a clear error if the port is already in use.
- `--host` (default: `127.0.0.1`): Bind address. Using `0.0.0.0` prints a bold warning: `WARNING: ACP server is network-accessible. Ensure your network policy restricts access.`
- `--profile` (default: active profile from `~/.tag/config.yaml`): TAG profile to use when executing incoming runs.
- `--agent-name` (default: RFC 1123-coerced active profile name): Override the ACP `AgentManifest.name`. Must match `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`.
- `--agent-description`: Override the manifest description. Defaults to `"TAG agent running profile '<profile>'"`.
- `--capabilities`: Comma-separated list of declared ACP capabilities. Default: `run,await`. Valid values: `run`, `await`, `stream` (reserved for future SSE support).
- `--pid-file`: Path to write the server PID. Enables `tag acp stop` to locate and terminate the server.
- `--log-level`: `log/slog` level (`debug`, `info`, `warn`, `error`) for HTTP access logs and ACP lifecycle events.
- `--json`: Emit startup/shutdown events as JSON instead of human-readable lines.

**Startup output (human-readable):**
```
ACP server started
  Agent name : tag-coder
  Profile    : coder
  Endpoint   : http://127.0.0.1:8081
  Agents URL : http://127.0.0.1:8081/agents
  PID        : 91234
  PID file   : /Users/alice/.tag/runtime/acp.pid

Listening for ACP runs. Press Ctrl-C to stop.
```

**Startup output (`--json`):**
```json
{
  "event": "acp_server_started",
  "agent_name": "tag-coder",
  "profile": "coder",
  "endpoint": "http://127.0.0.1:8081",
  "pid": 91234,
  "pid_file": "/Users/alice/.tag/runtime/acp.pid",
  "timestamp": "2026-06-17T10:00:00Z"
}
```

**Exposed ACP endpoints (all on `--port`):**

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/agents` | Return array of `AgentManifest` objects |
| `POST` | `/runs` | Submit a new run; body is `RunRequest`; returns `Run` with `status=created` |
| `GET` | `/runs/{run_id}` | Poll run status; returns current `Run` object |
| `POST` | `/runs/{run_id}` | Resume an awaiting run; body is `RunResumeRequest` |
| `DELETE` | `/runs/{run_id}` | Cancel a run; returns `Run` with `status=cancelling` |
| `GET` | `/health` | Health check; returns `{"status": "ok", "version": "<tag-version>"}` |

---

### 7.2 `tag acp send`

Submit a run to a remote ACP server and poll for completion.

```
tag acp send \
  --to http://other-agent:8081 \
  --message "Process this task: ..." \
  [--await] \
  [--timeout 300] \
  [--poll-interval 2] \
  [--output-file path/to/output.json] \
  [--trace-id <id>] \
  [--json]
```

**Flags:**
- `--to` (required): Base URL of the remote ACP server (e.g., `http://summarizer:8081`). The adapter appends `/runs`.
- `--message` (required unless `--message-file` is used): Text content of the ACP `Message` to send. Interpreted as a single `TextPart` with `content_type=text/plain`.
- `--message-file`: Read message content from a file. Mutually exclusive with `--message`.
- `--await` (default: true): Block until the run reaches a terminal state (`completed`, `failed`, `cancelled`). Pass `--no-await` for fire-and-forget (prints run ID and exits 0).
- `--timeout` (default: `300`): Maximum seconds to wait when `--await` is set. Exits with code 3 on timeout.
- `--poll-interval` (default: `2`): Seconds between `GET /runs/{id}` polls.
- `--output-file`: Write the full `Run` JSON to this file on completion.
- `--trace-id`: Inject this trace ID as `X-TAG-Trace-ID` header for distributed tracing. Defaults to a new UUID4.
- `--json`: Print the final `Run` JSON object to stdout instead of formatted output.

**Output on success (human-readable):**
```
Submitted run to http://other-agent:8081
  Run ID  : run_7f3a9c12-...
  Status  : created → in-progress → completed

Output:
  The summary of the report is: ...

Duration : 4.2s
```

**Output on `awaiting` (when `--await` is set):**
```
Run is awaiting human input:
  Run ID      : run_7f3a9c12-...
  Await type  : human_input
  Description : "Please confirm whether to delete the production database."

Use the following command to resume:
  tag acp resume --server http://other-agent:8081 --run-id run_7f3a9c12-... --data '{"answer": "..."}'
```

**Exit codes:**
- `0` — run completed successfully
- `1` — internal error (bad URL, network failure, invalid response)
- `2` — run failed (agent returned `status=failed`)
- `3` — timeout waiting for completion
- `4` — run cancelled

---

### 7.3 `tag acp list-agents`

Query a remote ACP server's agent registry.

```
tag acp list-agents \
  --server http://cluster:8081 \
  [--json] \
  [--filter <substring>]
```

**Flags:**
- `--server` (required): Base URL of the ACP cluster/server to query.
- `--json`: Print raw JSON array of `AgentManifest` objects.
- `--filter`: Only show agents whose `name` or `description` contains this substring (case-insensitive).

**Human-readable output:**
```
ACP Agents at http://cluster:8081  (3 agents)

Name              Description                            Capabilities
----------------- -------------------------------------- ------------
tag-coder         TAG agent running profile 'coder'      run, await
summarizer-bee    BeeAI summarization agent              run
data-analyst      Python pandas agent (BeeAI)            run, await
```

---

### 7.4 `tag acp resume`

Resume an `awaiting` run on a remote ACP server.

```
tag acp resume \
  --server http://agent:8081 \
  --run-id <run-id> \
  --data '{"answer": "yes"}' \
  [--json]
```

---

### 7.5 `tag acp cancel`

Cancel a running or awaiting run.

```
tag acp cancel \
  --server http://agent:8081 \
  --run-id <run-id> \
  [--json]
```

---

### 7.6 `tag acp status`

Show current status of a specific run (local or remote).

```
tag acp status \
  [--server http://agent:8081] \
  --run-id <run-id> \
  [--json]
```

If `--server` is omitted, queries the local SQLite `acp_runs` table.

---

### 7.7 `tag acp stop`

Stop the locally running ACP server.

```
tag acp stop \
  [--pid-file ~/.tag/runtime/acp.pid]
```

Sends SIGTERM to the process in the PID file. Waits up to 5 seconds for clean shutdown, then SIGKILL.

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag acp serve` MUST start an HTTP/1.1 server using `net/http` with a `go-chi/chi` router and `huma` v2 operations (consistent with the `internal/server` dashboard API), binding to the configured host:port within 500 ms of invocation. | Must |
| FR-02 | `GET /agents` MUST return a JSON array of `AgentManifest` objects with fields: `name` (RFC 1123), `description`, `metadata`, `capabilities`. The array MUST contain at least the locally registered agent. | Must |
| FR-03 | `POST /runs` MUST accept a `RunRequest` body with `agent_name` and `input` (array of `Message` objects). MUST return HTTP 200 with a `Run` object in `status=created` within 50 ms of receiving the request (before LLM inference begins). | Must |
| FR-04 | On receiving a valid `POST /runs`, the server MUST immediately transition the run to `in-progress`, begin executing the task against the specified TAG profile, and update `acp_runs.status` in SQLite. | Must |
| FR-05 | `GET /runs/{run_id}` MUST return the current `Run` object with accurate `status`, `output` (if completed), and `await_request` (if awaiting). MUST return HTTP 404 with `{"error": "run not found"}` for unknown run IDs. | Must |
| FR-06 | `POST /runs/{run_id}` with a `RunResumeRequest` body MUST resume a run in `awaiting` status by delivering `await_resume` data to the waiting run goroutine over its resume channel. MUST return HTTP 409 if the run is not in `awaiting` status. | Must |
| FR-07 | `DELETE /runs/{run_id}` MUST transition a `created` or `in-progress` run to `cancelling` immediately (by cancelling the run's `context.Context`) and then to `cancelled` once the running goroutine acknowledges cancellation. MUST return HTTP 409 if the run is already in a terminal state. | Must |
| FR-08 | All ACP run state (run ID, agent name, status, input messages, output messages, `await_request`, timestamps) MUST be persisted to `acp_runs` and `acp_run_events` in TAG's SQLite database before any response is returned to the caller. | Must |
| FR-09 | Agent names MUST be validated against `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` (RFC 1123 DNS label). `tag acp serve` MUST reject `--agent-name` values that fail this pattern with a clear error message listing the constraint. | Must |
| FR-10 | TAG profile names with uppercase letters or underscores MUST be auto-coerced to RFC 1123 form (lowercase, underscores → hyphens). If the coerced name would collide with an existing agent name, the server MUST append a 4-character hex suffix. | Must |
| FR-11 | `tag acp send` MUST propagate a `X-TAG-Trace-ID` and `X-TAG-Span-ID` header on every outbound HTTP request for distributed tracing (PRD-013). The receiving server MUST log these values in `acp_run_events` if present. | Must |
| FR-12 | Before executing a run, the server MUST call the budget module (PRD-039) to check if the run is within the profile's token budget. If the check fails, MUST return HTTP 402 with body `{"error": {"type": "budget_exceeded", "detail": "..."}}` without making any LLM call. | Must |
| FR-13 | When a run transitions to `awaiting`, the server MUST surface the blocked run via TAG's HITL approval system (PRD-078) so that `tag status` displays it as pending human input. | Should |
| FR-14 | `tag acp list-agents` MUST display a formatted table including `name`, `description`, and `capabilities` for each manifest. MUST support `--json` to emit the raw array. | Must |
| FR-15 | `GET /health` MUST return HTTP 200 with `{"status": "ok", "version": "<build.Version>", "uptime_s": <float64>}` (version injected via `-ldflags -X`) without requiring any database access. | Must |
| FR-16 | The ACP server MUST write a PID file to `--pid-file` on startup and remove it on clean shutdown. `tag acp stop` MUST use this file to send `SIGTERM` (via `syscall.Kill` / `os.Process.Signal`). | Should |
| FR-17 | `tag acp serve --json` MUST emit newline-delimited JSON events for every lifecycle transition: `acp_server_started`, `acp_run_created`, `acp_run_status_changed`, `acp_server_stopped`. | Should |
| FR-18 | The server MUST handle concurrent runs up to a configurable `--max-concurrent-runs` limit (default: 4). Requests that would exceed this limit MUST receive HTTP 503 with `{"error": {"type": "capacity_exceeded"}}`. | Should |
| FR-19 | `tag acp send --no-await` MUST return immediately after receiving the `Run` object from `POST /runs`, print the run ID, and exit 0. The run proceeds asynchronously on the server. | Must |
| FR-20 | Input `Message` objects in `RunRequest` MUST support both `TextPart` (`content_type=text/plain`) and `BlobPart` (`content_type`, `content` as base64, decoded via `encoding/base64`). The adapter MUST pass blob parts to the TAG agent as temporary files in the sandbox. | Should |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Latency:** `POST /runs` response (before LLM inference) MUST complete in under 50 ms on a modern laptop. `GET /runs/{id}` MUST complete in under 20 ms. | p99 measured in integration tests |
| NFR-02 | **Throughput:** The server MUST handle at least 10 concurrent `GET /runs/{id}` poll requests per second without degradation. `net/http` serves each request on its own goroutine, so this is handled by the runtime scheduler. | Load test with `wrk` or `vegeta` |
| NFR-03 | **Reliability:** Any error (including a recovered `panic`) during run execution MUST result in the run transitioning to `failed` with the error message in `acp_runs.error_detail`. The server process MUST NOT crash; each run goroutine has a `recover()` guard. | 100% coverage of error paths in unit tests |
| NFR-04 | **SQLite WAL mode:** The `internal/acp` package MUST use the shared `internal/store` handle (existing pattern) backed by `modernc.org/sqlite`, which enables WAL mode via `PRAGMA journal_mode=WAL`. All writes MUST use explicit `*sql.Tx` transactions to avoid lock contention with the dashboard's concurrent reads. | Verified by `PRAGMA journal_mode` assertion in tests |
| NFR-05 | **Dependency footprint:** `internal/acp` MUST rely only on the Go stdlib (`net/http`, `sync`, `context`, `encoding/json`, `regexp`, `log/slog`, `os/signal`), the already-vendored `chi`/`huma` router, and TAG's own packages. No new third-party module is introduced for the core path; `go-sse` is pulled in only when the reserved `stream` capability is implemented. The compiled adapter adds no runtime install footprint (single static binary, `CGO_ENABLED=0`). | `go mod graph` / `go build` size check |
| NFR-06 | **Startup isolation:** Importing (`init`-time) the `internal/acp` package MUST NOT start any goroutines, bind any ports, or open any sockets. All network activity begins only when `Server.Start(ctx)` is called. | Package-level test asserting `runtime.NumGoroutine()` is unchanged after import |
| NFR-07 | **Security defaults:** The server MUST bind to `127.0.0.1` by default. Any use of `0.0.0.0` or an external IP MUST print a visible warning and be recorded in the ACP server's startup log event. | CLI integration test |
| NFR-08 | **Observability:** Every ACP run MUST produce at least one OTel span (PRD-041/PRD-013) in TAG's `spans` table with `acp.run_id`, `acp.agent_name`, and `acp.status` as span attributes following OTel GenAI semantic conventions where applicable. | Span attribute assertion in integration tests |
| NFR-09 | **Graceful shutdown:** `SIGTERM` to the server process MUST call `http.Server.Shutdown(ctx)` and wait (via a `sync.WaitGroup` over run goroutines) for all in-flight runs to either complete or transition to `failed` before exiting, with a maximum drain timeout of 30 seconds enforced by a `context.WithTimeout`. | Signal handling integration test |
| NFR-10 | **Idempotency of registration:** Re-starting `tag acp serve` after a crash MUST NOT create duplicate entries in the `acp_agents` table; it MUST upsert using `INSERT ... ON CONFLICT(name) DO UPDATE`. | Unit test asserting single-row after double registration |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `internal/acp/server.go` | ACP HTTP server: chi router + huma operations, run lifecycle state machine, goroutine dispatch |
| `internal/acp/model.go` | ACP request/response Go structs (`AgentManifest`, `Run`, `Message`, `TextPart`, `BlobPart`, `AwaitRequest`) with `jsonschema`/huma tags |
| `internal/acp/store.go` | SQLite persistence helpers (`ensureSchema`, `persistRun`, `transition`, `completeRun`, `failRun`) over `modernc.org/sqlite` |
| `internal/acp/client.go` | Thin `net/http` client wrapper used by `tag acp send` / `tag acp list-agents` — separated for testability |
| `internal/cli/acp.go` | Cobra command tree for `tag acp <serve|send|list-agents|resume|cancel|status|stop>` |

**Modified files:**
- `internal/cli/root.go`: Register the `acp` command tree on the root cobra command.
- `internal/server`: Optionally expose ACP status via the existing dashboard API (read-only `GET /api/acp/runs` huma operation).

---

### 10.2 SQLite DDL

These tables are added to `~/.tag/runtime/tag.sqlite3` via `ensureSchema(db)` (run against the shared `*sql.DB` from `internal/store`) on first ACP server start. The DDL below is embedded in `internal/acp/store.go` and executed within a single migration transaction.

```sql
-- Registered ACP agents on this TAG node
CREATE TABLE IF NOT EXISTS acp_agents (
    name            TEXT PRIMARY KEY,           -- RFC 1123 agent name
    tag_profile     TEXT NOT NULL,              -- TAG profile this agent represents
    description     TEXT NOT NULL DEFAULT '',
    capabilities    TEXT NOT NULL DEFAULT 'run', -- comma-separated: run,await,stream
    metadata_json   TEXT NOT NULL DEFAULT '{}',  -- arbitrary JSONB metadata
    endpoint_url    TEXT,                        -- external URL if known (for multi-node setups)
    registered_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- ACP runs (one row per POST /runs)
CREATE TABLE IF NOT EXISTS acp_runs (
    run_id          TEXT PRIMARY KEY,           -- UUID4 assigned by TAG on POST /runs
    agent_name      TEXT NOT NULL REFERENCES acp_agents(name),
    tag_run_id      TEXT,                       -- FK to runs.id once the TAG run is created
    status          TEXT NOT NULL DEFAULT 'created',
    -- status IN ('created','in-progress','awaiting','completed','failed','cancelled','cancelling')
    input_json      TEXT NOT NULL DEFAULT '[]', -- serialized array of ACP Message objects
    output_json     TEXT,                       -- serialized array of ACP Message objects (on completion)
    await_request_json TEXT,                    -- ACP AwaitRequest object (when status=awaiting)
    error_detail    TEXT,                       -- error message (when status=failed)
    trace_id        TEXT,                       -- X-TAG-Trace-ID from caller
    span_id         TEXT,                       -- X-TAG-Span-ID from caller
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    started_at      TEXT,
    awaiting_at     TEXT,
    completed_at    TEXT,
    cancelled_at    TEXT,
    failed_at       TEXT
);

CREATE INDEX IF NOT EXISTS idx_acp_runs_agent_status
    ON acp_runs (agent_name, status);

CREATE INDEX IF NOT EXISTS idx_acp_runs_created_at
    ON acp_runs (created_at DESC);

-- Lifecycle events for each ACP run (audit log)
CREATE TABLE IF NOT EXISTS acp_run_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL REFERENCES acp_runs(run_id),
    event_type      TEXT NOT NULL,
    -- event_type IN ('submitted','started','thought','awaiting','resumed','completed','failed','cancelled','error')
    payload_json    TEXT NOT NULL DEFAULT '{}',
    ts              TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_acp_run_events_run_id
    ON acp_run_events (run_id, ts);
```

---

### 10.3 ACP Data Model (Go Structs)

The wire types are plain Go structs with `json` and huma/`jsonschema` tags. huma validates inbound `RunRequest` bodies against the schema derived from these structs, so hand-rolled validation is limited to the RFC 1123 name rule (which huma expresses via a `pattern` tag but we also enforce explicitly for a friendlier error message).

```go
// internal/acp/model.go
package acp

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var rfc1123Pattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`)

const rfc1123MaxLen = 63

// RunStatus is the ACP 7-state lifecycle.
type RunStatus string

const (
	StatusCreated    RunStatus = "created"
	StatusInProgress RunStatus = "in-progress"
	StatusAwaiting   RunStatus = "awaiting"
	StatusCompleted  RunStatus = "completed"
	StatusFailed     RunStatus = "failed"
	StatusCancelled  RunStatus = "cancelled"
	StatusCancelling RunStatus = "cancelling"
)

// Part is a single message part. Exactly one of Text/Blob semantics applies,
// discriminated by Type ("text" | "blob"). Blob content is base64 on the wire.
type Part struct {
	Type        string `json:"type" enum:"text,blob"`
	Content     string `json:"content"`                // text, or base64-encoded bytes for blobs
	ContentType string `json:"content_type,omitempty"` // default "text/plain"
	Name        string `json:"name,omitempty"`         // blob filename hint
}

// DecodeBlob returns the raw bytes of a blob part.
func (p Part) DecodeBlob() ([]byte, error) { return base64.StdEncoding.DecodeString(p.Content) }

// TextPart is a convenience constructor.
func TextPart(content string) Part {
	return Part{Type: "text", Content: content, ContentType: "text/plain"}
}

// Message is an ACP message with role and ordered parts.
type Message struct {
	Role  string `json:"role" enum:"user,assistant"`
	Parts []Part `json:"parts"`
}

// AwaitRequest describes what an awaiting agent needs before it can resume.
type AwaitRequest struct {
	Type        string         `json:"type"` // e.g. "human_input", "human_approval"
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema,omitempty"` // optional JSON Schema for resume data
}

// Run is the ACP Run resource returned by POST/GET /runs.
type Run struct {
	RunID        string        `json:"run_id"`
	AgentName    string        `json:"agent_name"`
	Status       RunStatus     `json:"status"`
	Input        []Message     `json:"input"`
	Output       []Message     `json:"output,omitempty"`
	AwaitRequest *AwaitRequest `json:"await_request,omitempty"`
	Error        *RunError     `json:"error,omitempty"`
	TraceID      string        `json:"-"` // X-TAG-Trace-ID from caller
	SpanID       string        `json:"-"` // X-TAG-Span-ID from caller
	CreatedAt    time.Time     `json:"created_at"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	CompletedAt  *time.Time    `json:"completed_at,omitempty"`
}

// RunError is the ACP error envelope.
type RunError struct {
	Type   string `json:"type,omitempty"`
	Detail string `json:"detail"`
}

// AgentManifest is a single entry in the GET /agents registry.
type AgentManifest struct {
	Name         string            `json:"name" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" maxLength:"63"`
	Description  string            `json:"description"`
	Capabilities []string          `json:"capabilities"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// ValidateName enforces the RFC 1123 DNS label rule with a friendly message.
func ValidateName(name string) error {
	if l := len(name); l < 1 || l > rfc1123MaxLen {
		return fmt.Errorf("agent name %q must be 1-63 characters (RFC 1123), got %d", name, l)
	}
	if !rfc1123Pattern.MatchString(name) {
		return fmt.Errorf(
			"agent name %q must match ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ (RFC 1123); "+
				"use only lowercase alphanumeric characters and hyphens", name)
	}
	return nil
}

var nonLabelChars = regexp.MustCompile(`[^a-z0-9\-]`)

// CoerceProfileName coerces a TAG profile name to a valid RFC 1123 DNS label.
func CoerceProfileName(profile string) string {
	c := strings.ToLower(profile)
	c = strings.NewReplacer("_", "-", " ", "-").Replace(c)
	c = nonLabelChars.ReplaceAllString(c, "")
	c = strings.Trim(c, "-")
	if len(c) > rfc1123MaxLen {
		c = c[:rfc1123MaxLen]
	}
	if c == "" {
		return "tag-agent"
	}
	return c
}
```

Timestamps use `time.Time` (RFC 3339 via the default JSON marshaler); run IDs use `github.com/google/uuid`'s `uuid.NewString()`. The persisted columns store the RFC 3339 string form so the schema in 10.2 is unchanged.

---

### 10.4 ACP Run State Machine

The state machine enforces valid transitions and rejects illegal ones with HTTP 409.

```
                    ┌─────────────────────────────────────────────┐
                    │              POST /runs received             │
                    └─────────────────────┬───────────────────────┘
                                          │
                                          ▼
                                    [ created ]
                                          │  (worker thread picks up run)
                                          ▼
                                   [ in-progress ]
                              ┌───────────┴──────────────┐
                              │                          │
                    (agent needs input)          (agent finishes)
                              │                          │
                              ▼                          ▼
                         [ awaiting ]              [ completed ]
                              │
                   (RunResumeRequest received)
                              │
                              ▼
                         [ in-progress ]  ──── (loop continues)
                              │
                       (DELETE /runs/{id})
                              │
                              ▼
                        [ cancelling ]
                              │
                   (agent acknowledges cancel)
                              │
                              ▼
                         [ cancelled ]

         (exception in agent)          (budget exceeded)
                    │                          │
                    ▼                          ▼
              [ failed ]                 [ failed ]
```

Valid transitions matrix:

| From \ To | created | in-progress | awaiting | completed | failed | cancelling | cancelled |
|-----------|---------|-------------|----------|-----------|--------|------------|-----------|
| created | — | YES | — | — | YES | YES | — |
| in-progress | — | — | YES | YES | YES | YES | — |
| awaiting | — | YES | — | — | YES | YES | — |
| cancelling | — | — | — | — | — | — | YES |
| completed | — | — | — | — | — | — | — |
| failed | — | — | — | — | — | — | — |
| cancelled | — | — | — | — | — | — | — |

```go
// validTransitions maps each status to the set of statuses it may move to.
var validTransitions = map[RunStatus]map[RunStatus]bool{
	StatusCreated:    {StatusInProgress: true, StatusFailed: true, StatusCancelling: true},
	StatusInProgress: {StatusAwaiting: true, StatusCompleted: true, StatusFailed: true, StatusCancelling: true},
	StatusAwaiting:   {StatusInProgress: true, StatusFailed: true, StatusCancelling: true},
	StatusCancelling: {StatusCancelled: true},
	StatusCompleted:  {},
	StatusFailed:     {},
	StatusCancelled:  {},
}

// ErrInvalidTransition is returned by validateTransition; the HTTP layer maps it to 409.
var ErrInvalidTransition = errors.New("invalid run status transition")

func validateTransition(from, to RunStatus) error {
	if !validTransitions[from][to] {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}
```

---

### 10.5 HTTP Server Architecture

`Server` wraps a standard `*http.Server` whose handler is a `chi.Mux` with the ACP routes registered as huma operations. Each accepted run executes on its own goroutine; concurrency is bounded by a buffered `chan struct{}` semaphore. Cancellation flows through a per-run `context.CancelFunc`, and `awaiting`/resume is signalled over a per-run resume channel. This follows the pattern established by `internal/server` (PRD-036) — one router, one `http.Server`, structured `slog` logging — and adds no new runtime dependency beyond the already-vendored chi/huma.

```go
// internal/acp/server.go
package acp

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
)

// runState is the in-memory bookkeeping for a live run.
type runState struct {
	run    *Run
	cancel context.CancelFunc     // cancels the run's context (DELETE /runs/{id})
	resume chan map[string]any    // delivers await_resume payload to the goroutine
}

type Server struct {
	manifest    AgentManifest
	tagProfile  string
	db          *sql.DB
	httpSrv     *http.Server
	api         huma.API

	mu     sync.Mutex          // guards runs
	runs   map[string]*runState

	sem    chan struct{}       // bounded concurrency (max-concurrent-runs)
	wg     sync.WaitGroup      // in-flight run goroutines (drain on shutdown)
	start  time.Time
}

func NewServer(host string, port int, m AgentManifest, profile string, db *sql.DB, maxConcurrent int) *Server {
	r := chi.NewRouter()
	s := &Server{
		manifest:   m,
		tagProfile: profile,
		db:         db,
		runs:       make(map[string]*runState),
		sem:        make(chan struct{}, maxConcurrent),
		start:      time.Now(),
	}
	s.api = humachi.New(r, huma.DefaultConfig("TAG ACP", build.Version))
	s.registerOperations() // GET /agents, POST/GET /runs, POST/DELETE /runs/{id}, GET /health
	s.httpSrv = &http.Server{Addr: net.JoinHostPort(host, strconv.Itoa(port)), Handler: r}
	return s
}

// Start serves until the context is cancelled (blocking).
func (s *Server) Start(ctx context.Context) error {
	go func() { <-ctx.Done(); s.Stop(30 * time.Second) }()
	if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop drains in-flight runs (up to timeout) then shuts down the HTTP server.
func (s *Server) Stop(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	_ = s.httpSrv.Shutdown(ctx)
}

func (s *Server) UptimeSeconds() float64 { return time.Since(s.start).Seconds() }

// submitRun persists the run, caches it, and dispatches a worker goroutine.
func (s *Server) submitRun(run *Run) {
	runCtx, cancel := context.WithCancel(context.Background())
	st := &runState{run: run, cancel: cancel, resume: make(chan map[string]any, 1)}
	s.mu.Lock()
	s.runs[run.RunID] = st
	s.mu.Unlock()
	s.persistRun(run)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.executeRun(runCtx, st)
	}()
}

func (s *Server) getRun(runID string) (*Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runs[runID]
	if !ok {
		return nil, false
	}
	return st.run, true
}

// resumeRun signals a waiting run to resume. Returns false if not awaiting.
func (s *Server) resumeRun(runID string, data map[string]any) bool {
	s.mu.Lock()
	st, ok := s.runs[runID]
	if !ok || st.run.Status != StatusAwaiting {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	st.resume <- data // buffered: non-blocking
	return true
}

// cancelRun requests cancellation. Returns false if already terminal.
func (s *Server) cancelRun(runID string) bool {
	s.mu.Lock()
	st, ok := s.runs[runID]
	if !ok || isTerminal(st.run.Status) || st.run.Status == StatusCancelling {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	st.cancel() // cancels runCtx; the goroutine observes ctx.Err()
	s.transition(runID, StatusCancelling)
	return true
}

func isTerminal(s RunStatus) bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}
```

Where a client requested the reserved `stream` capability, intermediate `thought` yields are relayed over `tmaxmax/go-sse` with `Last-Event-ID` replay; the synchronous polling path above remains the default per NG4.

---

### 10.6 Run Execution and TAG Integration

The `executeRun` goroutine bridges ACP runs into TAG's existing execution infrastructure. It concatenates the text parts from the ACP `Message` slice, invokes the TAG agent via the `internal/agent` loop, and translates the result back into ACP `Message` output. `ErrAwaitRequired` (a sentinel wrapping an `*AwaitRequest`) signals the HITL pause; the goroutine then blocks on the run's resume channel until `resumeRun` delivers the `await_resume` payload.

```go
// executeRun runs on its own goroutine; ctx is cancelled by cancelRun / shutdown.
func (s *Server) executeRun(ctx context.Context, st *runState) {
	run := st.run

	// Panic guard: a crashing agent must fail the run, not the process (NFR-03).
	defer func() {
		if r := recover(); r != nil {
			s.failRun(run.RunID, fmt.Sprintf("panic: %v", r))
		}
	}()

	// Bounded concurrency; the HTTP layer also rejects with 503 (FR-18).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.failRun(run.RunID, "server at capacity; try again later")
		return
	}

	s.transition(run.RunID, StatusInProgress)

	// Concatenate text parts from the input messages.
	var sb strings.Builder
	for _, msg := range run.Input {
		for _, p := range msg.Parts {
			if p.Type == "text" {
				if sb.Len() > 0 {
					sb.WriteString("\n\n")
				}
				sb.WriteString(p.Content)
			}
		}
	}
	task := sb.String()

	// OTel span for this ACP run (parented on the inbound trace context).
	ctx, span := tracing.StartSpan(ctx, "acp.run",
		trace.WithAttributes(
			attribute.String("acp.run_id", run.RunID),
			attribute.String("acp.agent_name", run.AgentName),
			attribute.String("tag.profile", s.tagProfile),
		),
	)
	defer span.End()

	// Pre-flight budget check (FR-12); no LLM call is made if it fails.
	if ok, detail := budget.Check(ctx, s.db, s.tagProfile); !ok {
		span.SetAttributes(attribute.Bool("acp.budget_exceeded", true))
		s.failRunTyped(run.RunID, "budget_exceeded", "budget exceeded: "+detail)
		return
	}

	if ctx.Err() != nil { // cancelled before invocation
		s.transition(run.RunID, StatusCancelled)
		return
	}

	// Invoke the TAG agent; ErrAwaitRequired pauses for HITL input.
	out, err := s.invokeAgent(ctx, task, run.RunID, nil)
	var awaitErr *ErrAwaitRequired
	if errors.As(err, &awaitErr) {
		s.transition(run.RunID, StatusAwaiting, withAwait(awaitErr.Request))
		select {
		case resumeData := <-st.resume: // blocks until resumeRun delivers payload
			s.transition(run.RunID, StatusInProgress)
			out, err = s.invokeAgent(ctx, task, run.RunID, resumeData)
		case <-ctx.Done():
			s.transition(run.RunID, StatusCancelled)
			return
		}
	}
	if err != nil {
		s.failRun(run.RunID, err.Error())
		return
	}
	if ctx.Err() != nil {
		s.transition(run.RunID, StatusCancelled)
		return
	}

	s.completeRun(run.RunID, []Message{{Role: "assistant", Parts: []Part{TextPart(out)}}})
	span.SetAttributes(attribute.String("acp.status", "completed"))
}
```

`invokeAgent` is a thin adapter over the `internal/agent` package's run loop; passing a non-nil `resumeData` map continues an awaiting run. Cancellation is cooperative: the agent loop selects on `ctx.Done()`, so `DELETE /runs/{id}` (which calls `st.cancel()`) unwinds the goroutine cleanly.

---

### 10.7 Client Functions (`internal/acp/client.go`)

```go
// internal/acp/client.go
package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client is a thin net/http wrapper around a remote ACP server.
type Client struct {
	BaseURL string
	HTTP    *http.Client // defaults to a client with a 30s timeout
}

func (c *Client) do(ctx context.Context, method, path string, body any, hdr map[string]string, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("acp: %s %s -> %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// SubmitRun POSTs /runs and returns the created Run.
func (c *Client) SubmitRun(ctx context.Context, agentName string, input []Message, traceID, spanID string) (*Run, error) {
	hdr := map[string]string{}
	if traceID != "" {
		hdr["X-TAG-Trace-ID"] = traceID
	}
	if spanID != "" {
		hdr["X-TAG-Span-ID"] = spanID
	}
	var run Run
	err := c.do(ctx, http.MethodPost, "/runs",
		map[string]any{"agent_name": agentName, "input": input}, hdr, &run)
	return &run, err
}

// PollRun polls GET /runs/{id} until a terminal (or awaiting) status, or timeout.
func (c *Client) PollRun(ctx context.Context, runID string, timeout, interval time.Duration) (*Run, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		var run Run
		if err := c.do(ctx, http.MethodGet, "/runs/"+runID, nil, nil, &run); err != nil {
			return nil, err
		}
		switch run.Status {
		case StatusCompleted, StatusFailed, StatusCancelled, StatusAwaiting:
			return &run, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("run %s did not complete within %s", runID, timeout)
		case <-ticker.C:
		}
	}
}

// ListAgents GETs /agents.
func (c *Client) ListAgents(ctx context.Context) ([]AgentManifest, error) {
	var out []AgentManifest
	err := c.do(ctx, http.MethodGet, "/agents", nil, nil, &out)
	return out, err
}

// ResumeRun POSTs /runs/{id} with a RunResumeRequest.
func (c *Client) ResumeRun(ctx context.Context, runID string, data map[string]any) (*Run, error) {
	var run Run
	err := c.do(ctx, http.MethodPost, "/runs/"+runID,
		map[string]any{"await_resume": data}, nil, &run)
	return &run, err
}

// CancelRun DELETEs /runs/{id}.
func (c *Client) CancelRun(ctx context.Context, runID string) (*Run, error) {
	var run Run
	err := c.do(ctx, http.MethodDelete, "/runs/"+runID, nil, nil, &run)
	return &run, err
}
```

---

### 10.8 CLI Integration (`internal/cli/acp.go`) and huma Operations

The CLI surface is a cobra command tree; flags bind to typed fields via `cobra`/`pflag`, replacing argparse. `newACPCmd()` is registered on the root command in `internal/cli/root.go`.

```go
// internal/cli/acp.go
package cli

import (
	"github.com/spf13/cobra"
)

func newACPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "acp",
		Short: "ACP intra-cluster agent messaging",
	}
	cmd.AddCommand(newACPServeCmd(), newACPSendCmd(), newACPListAgentsCmd(),
		newACPResumeCmd(), newACPCancelCmd(), newACPStatusCmd(), newACPStopCmd())
	return cmd
}

func newACPServeCmd() *cobra.Command {
	var o struct {
		Port              int
		Host              string
		Profile           string
		AgentName         string
		AgentDescription  string
		Capabilities      []string
		PIDFile           string
		MaxConcurrentRuns int
		LogLevel          string
		JSON              bool
	}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start ACP server",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runACPServe(cmd.Context(), &o) },
	}
	f := cmd.Flags()
	f.IntVar(&o.Port, "port", 8081, "TCP port to bind")
	f.StringVar(&o.Host, "host", "127.0.0.1", "bind address")
	f.StringVar(&o.Profile, "profile", "", "TAG profile (default: active)")
	f.StringVar(&o.AgentName, "agent-name", "", "override ACP agent name (RFC 1123)")
	f.StringVar(&o.AgentDescription, "agent-description", "", "override manifest description")
	f.StringSliceVar(&o.Capabilities, "capabilities", []string{"run", "await"}, "declared capabilities")
	f.StringVar(&o.PIDFile, "pid-file", defaultPIDFile(), "path to write server PID")
	f.IntVar(&o.MaxConcurrentRuns, "max-concurrent-runs", 4, "max concurrent runs")
	f.StringVar(&o.LogLevel, "log-level", "info", "slog level")
	f.BoolVar(&o.JSON, "json", false, "emit JSON events")
	return cmd
}

// newACPSendCmd, newACPListAgentsCmd, newACPResumeCmd, newACPCancelCmd,
// newACPStatusCmd, newACPStopCmd follow the same pattern (flags omitted for brevity).
```

The server routes are declared as huma operations (spec-first OpenAPI 3.1), so the ACP `openapi.yaml` is generated from the Go structs rather than hand-maintained. `huma.Register` binds each operation to a typed input/output struct and validates the body automatically:

```go
func (s *Server) registerOperations() {
	huma.Register(s.api, huma.Operation{
		OperationID: "acp-submit-run", Method: http.MethodPost, Path: "/runs",
		DefaultStatus: http.StatusOK,
	}, func(ctx context.Context, in *struct {
		TraceID string `header:"X-TAG-Trace-ID"`
		SpanID  string `header:"X-TAG-Span-ID"`
		Body    RunRequest
	}) (*RunEnvelope, error) {
		return s.handleSubmitRun(ctx, in.TraceID, in.SpanID, in.Body)
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "acp-get-run", Method: http.MethodGet, Path: "/runs/{run_id}",
	}, s.handleGetRun) // returns huma.Error404NotFound for unknown IDs

	huma.Register(s.api, huma.Operation{
		OperationID: "acp-resume-run", Method: http.MethodPost, Path: "/runs/{run_id}",
	}, s.handleResumeRun) // huma.Error409Conflict if not awaiting

	huma.Register(s.api, huma.Operation{
		OperationID: "acp-cancel-run", Method: http.MethodDelete, Path: "/runs/{run_id}",
	}, s.handleCancelRun)

	huma.Register(s.api, huma.Operation{
		OperationID: "acp-list-agents", Method: http.MethodGet, Path: "/agents",
	}, s.handleListAgents)

	huma.Register(s.api, huma.Operation{
		OperationID: "acp-health", Method: http.MethodGet, Path: "/health",
	}, s.handleHealth)
}
```

Where huma returns typed errors (`huma.Error404NotFound`, `huma.Error409Conflict`, `huma.Error402PaymentRequired` for `budget_exceeded`, `huma.Error503ServiceUnavailable` for `capacity_exceeded`), the JSON body follows the ACP `{"error": {"type": ..., "detail": ...}}` shape via a custom `huma.NewError` hook.

---

### 10.9 Integration with Existing TAG Modules

| Existing Package | Integration Point |
|------------------|-------------------|
| `internal/store` (shared `*sql.DB`) | Used by `persistRun`, `transition`, `completeRun`, `failRun` for all SQLite writes (`modernc.org/sqlite`, WAL, explicit `*sql.Tx`). |
| `internal/tracing` (PRD-013) | `executeRun` wraps each run in an `acp.run` OTel span; `X-TAG-Trace-ID` / `X-TAG-Span-ID` headers are parsed by huma into the operation input and used as parent context. |
| `internal/budget` (PRD-039) | `executeRun` calls `budget.Check(ctx, db, profile)` before any LLM call; `huma.Error402PaymentRequired` returned if budget exceeded. |
| `internal/security` (PRD-034) | Input messages are scanned for secrets/PII before being stored in SQLite or forwarded to the agent. |
| `internal/sandbox` (PRD-028) | BlobPart inputs are written to a sandboxed temp dir (`os.MkdirTemp`) before being passed to the agent. |
| `internal/server` (PRD-036) | `GET /api/acp/runs` huma operation added to the dashboard API for read-only ACP run visibility. |
| `internal/notify` (PRD-040) | When a run transitions to `awaiting`, a notification is emitted (if configured) so operators are alerted. |

---

## 11. Security Considerations

1. **Bind address default is loopback.** The server binds to `127.0.0.1` by default. Any deviation (`--host 0.0.0.0` or a non-loopback IP) prints a bold warning and records the deviation in the startup log event. This is a defense-in-depth measure; actual network security must be handled at the network/infra layer.

2. **No authentication on the ACP server in v1.** This is intentional for intra-cluster use where Kubernetes NetworkPolicy or Docker Compose network isolation handles access control. A future hardening PRD will add optional Bearer token validation via `Authorization: Bearer <token>` header, with the expected token stored in TAG's secret store.

3. **Input validation before SQLite writes.** All `POST /runs` request bodies are parsed and validated before any database write. Malformed JSON returns HTTP 400. Fields are parameterized in all SQLite queries (no string interpolation) to prevent SQL injection.

4. **Secret scanning on message content.** Before storing input messages in `acp_runs.input_json` or forwarding to the agent, the content is passed through TAG's secret scanner (`internal/security`, PRD-034) to detect and reject requests containing API keys, passwords, or credential patterns. The scanner runs in-process; no network call is made.

5. **Run ID is a UUIDv4.** Run IDs are generated server-side using `github.com/google/uuid` (`uuid.NewString()`). They are not predictable or sequential, so enumerating run IDs via `GET /runs/{id}` is not feasible without prior knowledge of the ID.

6. **BlobPart sandbox isolation.** Binary content from ACP `BlobPart` inputs is written to a sandboxed temporary directory (PRD-028) and the agent receives only the file path. The temporary directory is removed after the run completes or fails.

7. **Budget enforcement is pre-flight.** The budget check (FR-12) occurs before any LLM call is made. This prevents budget exhaustion through concurrent run submissions. The check uses the budget package's `budget.Check()` which reads from SQLite in a read transaction.

8. **Error responses do not leak internals.** `status=failed` error messages are truncated to 500 characters and Go stack traces (including any `recover()`-captured panic detail) are never included in the HTTP response body. Full traces are written only to the TAG `slog` log and the `acp_run_events` table.

9. **PID file permissions.** The PID file at `~/.tag/runtime/acp.pid` is written with permissions `0o600` (owner read/write only). `tag acp stop` verifies that the process in the PID file is owned by the current user before sending SIGTERM.

10. **Header injection prevention.** `X-TAG-Trace-ID` and `X-TAG-Span-ID` values from inbound requests are validated as hex strings (UUID format) before being stored in SQLite or logged. Non-conforming values are silently ignored with a debug log entry.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/acp/*_test.go`, `go test`)

Table-driven tests using the stdlib `testing` package; assertions via `testify/require` (already vendored).

| Test | Description |
|------|-------------|
| `TestValidateName` | Assert `ValidateName()` rejects uppercase, underscores, leading/trailing hyphens, length > 63, empty string. Assert valid names return `nil`. |
| `TestCoerceProfileName` | Table-driven: `"My_Coder"` → `"my-coder"`, `"UPPER__CASE"` → `"upper--case"` (then strip double hyphens), `"123"` → `"123"`, long name → truncated to 63 chars. |
| `TestValidTransitions` | For each (from, to) in `validTransitions`, assert `validateTransition()` returns `nil`. |
| `TestInvalidTransitions` | For each (from, to) NOT in `validTransitions`, assert `validateTransition()` returns an error wrapping `ErrInvalidTransition`. |
| `TestPartJSONRoundTrip` | Round-trip a text `Part` through `json.Marshal`/`Unmarshal`; assert field fidelity. |
| `TestBlobPartRoundTrip` | Round-trip a blob `Part` with binary content through base64 (`DecodeBlob`). |
| `TestBudgetCheckError402` | Fake `budget.Check` returning `(false, "over limit")`. Assert `executeRun` sets `status=failed` (typed `budget_exceeded`) and the run's error detail is surfaced. |
| `TestRunIDIsUUIDv4` | Assert run IDs match `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`. |
| `TestImportStartsNoGoroutines` | Record `runtime.NumGoroutine()` before/after package use without `Start`; assert unchanged. |
| `TestEnsureSchemaCreatesTables` | Call `ensureSchema(db)` on an in-memory `modernc.org/sqlite` DB; assert all three tables exist with the expected columns. |
| `TestSecretScannerRejectsAPIKey` | Submit a `POST /runs` (via `httptest`) with a message containing an `sk-...` key. Assert HTTP 400 and no row written to `acp_runs`. |

### 12.2 Integration Tests (`internal/acp/integration_test.go`, `net/http/httptest`)

Each test spins up the router with `httptest.NewServer` (or drives the huma API in-process) and exercises the client from 10.7.

| Test | Description |
|------|-------------|
| `TestServeAndListAgents` | Start the ACP handler under `httptest.NewServer`. `GET /agents`; assert the test agent manifest is in the response. |
| `TestSubmitRunAndPollCompletion` | Submit a run with a fake TAG agent that returns immediately. Poll until `status=completed`; assert output messages present and `acp_runs.status='completed'` in SQLite. |
| `TestFullLifecycleAwaitingResume` | Fake agent returning `*ErrAwaitRequired`. Verify run transitions to `awaiting`; call `ResumeRun`; verify completion. Assert `acp_run_events` contains `awaiting` and `resumed` rows. |
| `TestCancelInProgressRun` | Submit a slow fake agent that selects on `ctx.Done()`. Call `CancelRun`; assert transitions through `cancelling` to `cancelled` and the goroutine exits. |
| `TestHTTP404UnknownRun` | `GET /runs/nonexistent-id`; assert HTTP 404. |
| `TestHTTP409ResumeNonAwaiting` | Submit a completed run; attempt `POST /runs/{id}` (resume); assert HTTP 409. |
| `TestHTTP503OverCapacity` | Set `max-concurrent-runs=1`; submit 2 simultaneous runs; assert the second receives HTTP 503. |
| `TestTraceHeaderPropagation` | Submit run with `X-TAG-Trace-ID: test-trace-123`. Assert `acp_runs.trace_id='test-trace-123'` in SQLite. |
| `TestHealthEndpoint` | `GET /health`; assert HTTP 200 and body contains `{"status": "ok"}`. |
| `TestConcurrentPollPerformance` | Submit 1 run; fire 10 concurrent `GET /runs/{id}` requests via goroutines + `errgroup`; assert each responds in under 50 ms. |

### 12.3 CLI Integration Tests (`internal/cli/acp_test.go`)

Build the binary once with `go test` and drive it via `os/exec`, or invoke the cobra command in-process with a captured buffer.

| Test | Description |
|------|-------------|
| `TestACPServeStarts` | Launch `tag acp serve --port <random> --profile test` via `exec.Cmd`. Assert it starts, `GET /health` succeeds, and it terminates cleanly on `SIGTERM`. |
| `TestACPSendLocal` | Start server; run `tag acp send --to http://localhost:<port> --message "hello"`. Assert exit code 0 and output contains agent response. |
| `TestACPListAgentsJSON` | Start server; run `tag acp list-agents --server http://localhost:<port> --json`. Assert output unmarshals into `[]AgentManifest` with a `name`. |
| `TestACPSendNoAwait` | Run with `--no-await`; assert exit code 0 and output contains run ID. |
| `TestACPServeNetworkWarning` | Run `tag acp serve --host 0.0.0.0 --port <random>`. Assert stderr contains `WARNING: ACP server is network-accessible`. |

### 12.4 Performance Tests (`go test -bench`)

Benchmarks live in `internal/acp/bench_test.go` (`func BenchmarkX(b *testing.B)`); latency percentiles measured with `-benchtime` iterations against the in-process huma API.

| Test | Target |
|------|--------|
| `POST /runs` response time (before LLM) | p99 < 50 ms (100 iterations, sequential) |
| `GET /runs/{id}` response time | p99 < 20 ms (1000 iterations, sequential against SQLite WAL) |
| Server startup to first healthy response | < 500 ms |
| Concurrent poll throughput | 10 req/s without degradation (10 goroutines × 100 requests each) |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag acp serve --port 8081` starts successfully and prints the startup message including agent name, profile, endpoint, and PID within 500 ms. | CLI integration test + manual smoke test |
| AC-02 | `GET /agents` on a running server returns a JSON array containing at least one `AgentManifest` with a valid RFC 1123 name. | Automated JSON schema validation test |
| AC-03 | `POST /runs` with a valid `RunRequest` returns HTTP 200 and a `Run` object with `status=created` within 50 ms (mock agent). | Integration test with timing assertion |
| AC-04 | `GET /runs/{run_id}` for a completed run returns `status=completed` and a non-empty `output` array. | Integration test |
| AC-05 | `GET /runs/{run_id}` for an unknown ID returns HTTP 404. | Unit test |
| AC-06 | `POST /runs/{run_id}` (resume) on an `awaiting` run causes the run to transition back to `in-progress` and eventually to `completed`. | Integration test covering full `awaiting→resumed→completed` lifecycle |
| AC-07 | `POST /runs/{run_id}` (resume) on a `completed` run returns HTTP 409. | Unit test |
| AC-08 | `DELETE /runs/{run_id}` on an `in-progress` run causes it to transition to `cancelling` then `cancelled`, and the worker thread exits cleanly. | Integration test |
| AC-09 | All ACP run state (run ID, status, input, output, timestamps) is present in `acp_runs` after run completion. | SQLite assertion in integration tests |
| AC-10 | `tag acp send --to http://localhost:8081 --message "test"` returns exit code 0 and prints the agent output to stdout. | CLI integration test |
| AC-11 | `tag acp list-agents --server http://localhost:8081` prints a formatted table with `name`, `description`, and `capabilities` columns. | CLI integration test |
| AC-12 | Specifying `--agent-name "My_Coder"` (invalid RFC 1123) with `tag acp serve` results in a clear error message and exit code 1 without starting the server. | CLI unit test |
| AC-13 | A TAG profile name `"My_Coder"` is auto-coerced to `"my-coder"` when `--agent-name` is omitted. | Unit test for `coerce_profile_name()` |
| AC-14 | Submitting a run when the profile's token budget is exhausted results in HTTP 402 with `error.type=budget_exceeded` and no LLM call is made. | Integration test with mocked budget module |
| AC-15 | `tag acp serve` with `--host 0.0.0.0` prints a `WARNING: ACP server is network-accessible` message to stderr. | CLI integration test |
| AC-16 | Each ACP run produces at least one row in `acp_run_events` for each lifecycle event (`submitted`, `started`, `completed` or `failed`). | SQLite assertion in integration tests |
| AC-17 | `X-TAG-Trace-ID` from an inbound `POST /runs` request is stored in `acp_runs.trace_id` and appears as the `acp.run_id` attribute in the OTel span for that run. | Integration test checking both SQLite and span table |
| AC-18 | `tag acp stop` terminates the server process within 5 seconds and removes the PID file. | CLI integration test using `os/exec` |
| AC-19 | The server handles 10 concurrent `GET /runs/{id}` requests simultaneously without deadlock or error. | Concurrent integration test using goroutines + `golang.org/x/sync/errgroup` |
| AC-20 | `GET /health` returns HTTP 200 with `{"status": "ok"}` at any point after server startup, including during active run execution. | Integration test |

---

## 14. Dependencies

Module `github.com/tag-agent/tag`, Go 1.24+, `CGO_ENABLED=0`, distributed via GoReleaser + cosign + SLSA.

| Dependency | Type | Purpose | Notes |
|------------|------|---------|-------|
| `net/http` (Go stdlib) | Runtime | HTTP server + client for ACP endpoints | Consistent with `internal/server` |
| `github.com/go-chi/chi/v5` | Runtime | Router / middleware for the ACP mux | Already vendored |
| `github.com/danielgtaylor/huma/v2` | Runtime | Spec-first OpenAPI 3.1 operations + request validation | Generates ACP `openapi.yaml` from Go structs |
| `github.com/invopop/jsonschema` | Runtime | JSON Schema derivation for wire structs | Used transitively by huma |
| `sync` / `context` (Go stdlib) | Runtime | Concurrency: run goroutines, mutex, semaphore chan, cancellation | |
| `github.com/google/uuid` | Runtime | UUIDv4 run ID generation | |
| `regexp` (Go stdlib) | Runtime | RFC 1123 name validation | |
| `os/signal` (Go stdlib) | Runtime | `SIGTERM` handler for graceful shutdown | |
| `modernc.org/sqlite` | Runtime | Pure-Go SQLite driver (WAL) for `acp_*` tables | No CGO; replaces `aiosqlite` |
| `internal/tracing` (PRD-013) | Runtime | OTel span creation for `acp.run` spans | Must be merged before ACP |
| `internal/budget` (PRD-039) | Runtime | Pre-flight budget check | Must be merged before ACP |
| `internal/security` (PRD-034) | Runtime | Input message secret scanning | Must be merged before ACP |
| `internal/sandbox` (PRD-028) | Runtime (optional) | BlobPart temp file isolation | Required only if BlobPart support is enabled |
| `internal/notify` (PRD-040) | Runtime (optional) | `awaiting` run notifications | Gracefully skipped if not configured |
| `github.com/tmaxmax/go-sse` | Runtime (deferred) | SSE for the reserved `stream` capability (Last-Event-ID replay) | Only when NG4 streaming lands |
| `github.com/stretchr/testify` | Dev/test | Assertions for `go test` | |
| ACP OpenAPI spec (`openapi.yaml`) | Spec reference | Normative ACP request/response schema; cross-checked against huma-generated spec in tests | From `github.com/i-am-bee/acp/blob/main/docs/spec/openapi.yaml` |

---

## 15. Open Questions

| # | Question | Owner | Due | Notes |
|---|----------|-------|-----|-------|
| OQ-1 | Should `tag acp serve` support running the server as a daemon (background process) with `--daemon` flag, like `tag queue start --daemon`? Or is foreground-only acceptable for v1? | @team | Pre-implementation | Foreground preferred for v1 to keep signal handling simple. |
| OQ-2 | ACP's `await/resume` is designed for the BeeAI server-side `AsyncGenerator[RunYield, RunYieldResume]` pattern. How does TAG signal an `awaiting` state to the calling client given the synchronous HTTP polling model in v1? Current plan: the run goroutine blocks on its resume channel (`<-st.resume`); client polls `GET /runs/{id}` and sees `status=awaiting`. Is this sufficient, or do we need a webhook/SSE notification? | @team | Pre-implementation | Polling is sufficient for v1 per PRD-scope constraints. |
| OQ-3 | The ACP spec says `await_request.type` is an open string. What specific `await_request.type` values should TAG use for its HITL cases? Proposal: `"human_approval"` for PRD-078 approval gates. | @team | Pre-implementation | Needs alignment with PRD-078 owner. |
| OQ-4 | Should `acp_runs` have a foreign key to `runs.id`? This requires that each ACP run creates a corresponding row in the existing `runs` table for dashboard visibility. Is that the right approach, or should ACP runs be a standalone table visible only via `/api/acp/runs`? | @team | Pre-implementation | Creating a `runs` row per ACP run gives maximum dashboard integration at the cost of schema coupling. |
| OQ-5 | The ACP `POST /runs` endpoint in the OpenAPI spec uses `agent_name` to target a specific agent in the registry. If a cluster runs multiple TAG agents (each as a separate `tag acp serve` process), should TAG support a multi-agent server that proxies to the correct backend based on `agent_name`? | @team | Post-v1 | Out of scope for this PRD. Each `tag acp serve` instance hosts exactly one agent. |
| OQ-6 | Should the `capabilities` field in `AgentManifest` be a free-form list or restricted to the ACP-defined set (`run`, `await`, `stream`)? The BeeAI OpenAPI schema is evolving; should TAG follow the upstream schema strictly or allow extension? | @team | Pre-implementation | Restrict to `["run", "await"]` in v1; `stream` reserved for SSE follow-up. |
| OQ-7 | `tag acp send --message` currently interprets the argument as `text/plain`. Should there be support for `--message-type application/json` to send structured JSON as a single `TextPart` with `content_type=application/json`? | @team | Pre-implementation | Add `--message-content-type` flag in v1 with default `text/plain`. |

---

## 16. Complexity and Timeline

### Phase 1 — Foundation (Days 1-3)

- `internal/acp/model.go`: `AgentManifest`, `Run`, `Message`, `Part`, `AwaitRequest`, `RunStatus` structs and constants.
- `ValidateName()` and `CoerceProfileName()` with full table-driven unit tests.
- State machine: `validTransitions`, `validateTransition()`, unit tests for all valid/invalid pairs.
- SQLite DDL: `acp_agents`, `acp_runs`, `acp_run_events` tables; `ensureSchema()` in `internal/acp/store.go`.
- `internal/acp/client.go`: `SubmitRun`, `PollRun`, `ListAgents`, `ResumeRun`, `CancelRun` using `net/http`.

**Deliverable:** All wire structs and state machine logic with 100% unit test coverage. No HTTP server yet.

### Phase 2 — Server (Days 4-6)

- `Server` type: `NewServer`, `Start(ctx)`, `Stop`, `submitRun`, `getRun`, `resumeRun`, `cancelRun`.
- huma operations: `GET /agents`, `POST /runs`, `GET /runs/{id}`, `POST /runs/{id}` (resume), `DELETE /runs/{id}`, `GET /health`, on a chi mux.
- Bounded concurrency via a buffered `chan struct{}` semaphore for `max-concurrent-runs`.
- Graceful shutdown with `http.Server.Shutdown` + `sync.WaitGroup` drain timeout.
- Integration tests: full server lifecycle via `httptest`.

**Deliverable:** Functional ACP server that passes all integration tests with fake agents.

### Phase 3 — TAG Agent Integration (Days 7-9)

- `executeRun()`: bridge ACP run to the `internal/agent` run loop.
- Budget check integration (`budget.Check(ctx, db, profile)`).
- OTel span creation (`tracing.StartSpan(ctx, "acp.run", ...)`).
- Secret scanning on input messages.
- `ErrAwaitRequired` sentinel + resume-channel pause/resume mechanism.
- HITL notification via `internal/notify` on `awaiting` transition.
- Full integration tests including budget rejection and await/resume lifecycle.

**Deliverable:** ACP runs actually execute TAG agents with full observability, budget enforcement, and HITL support.

### Phase 4 — CLI Surface (Days 10-11)

- `newACPCmd()` cobra tree in `internal/cli/acp.go` with all subcommands.
- Cobra/pflag registration for `serve`, `send`, `list-agents`, `resume`, `cancel`, `status`, `stop`.
- Human-readable output formatters for each subcommand.
- `--json` mode for all subcommands.
- PID file write/remove.
- `tag acp stop` with `SIGTERM`/`SIGKILL`.
- CLI integration tests.

**Deliverable:** Full `tag acp` CLI surface working end-to-end.

### Phase 5 — Dashboard API + Polish (Days 12-14)

- `GET /api/acp/runs` huma operation added to `internal/server` for dashboard read-only visibility.
- `tag acp serve --json` newline-delimited JSON event stream.
- Network binding warning for `--host 0.0.0.0`.
- Error response hardening (truncate, no tracebacks in HTTP response).
- PID file permission enforcement (`0o600`).
- Performance benchmark assertions.
- Full acceptance criteria verification.
- Documentation: update `docs/prd/INDEX.md`.

**Deliverable:** Production-ready ACP adapter ready for code review.

---

## References

- ACP OpenAPI specification: https://github.com/i-am-bee/acp/blob/main/docs/spec/openapi.yaml
- ACP GitHub (i-am-bee): https://github.com/i-am-bee/acp
- Linux Foundation AGNTCY (ACP governance): https://agntcy.org
- Protocol comparison paper: https://arxiv.org/html/2505.02279v1
- ACP vs A2A convergence analysis: https://zylos.ai/research/2026-03-26-agent-interoperability-protocols-mcp-a2a-acp-convergence/
- RFC 1123 (DNS label syntax): https://datatracker.ietf.org/doc/html/rfc1123
- RFC 8615 (Well-Known URIs): https://datatracker.ietf.org/doc/html/rfc8615
- TAG PRD-013 (Agent Tracing): `docs/prd/PRD-013-agent-tracing-observability.md`
- TAG PRD-028 (Sandbox): `docs/prd/PRD-028-sandbox-code-execution.md`
- TAG PRD-034 (Security / Secret Scanning): `docs/prd/PRD-034-secret-scanning.md`
- TAG PRD-036 (Web Dashboard / api.py): `docs/prd/PRD-036-web-dashboard.md`
- TAG PRD-039 (Token Budget Enforcement): `docs/prd/PRD-039-token-budget-enforcement.md`
- TAG PRD-040 (Notification Hooks): `docs/prd/PRD-040-notification-hooks.md`
- TAG PRD-041 (OTel GenAI Cost Attribution): `docs/prd/PRD-041-otel-genai-span-cost-attribution.md`
- TAG PRD-078 (HITL Tool Approval Audit Trail): `docs/prd/PRD-078-hitl-tool-approval-audit-trail.md`

