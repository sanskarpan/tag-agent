# PRD-087: ACP (IBM) Lightweight REST Adapter for Intra-Cluster Agent Messaging (`tag acp`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `api.py + acp_adapter.py` (new), `controller.py` (new `cmd_acp`), `tag.sqlite3` (new `acp_agents`, `acp_runs`, `acp_run_events` tables)
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox), PRD-034 (secret scanning/security), PRD-036 (web dashboard / api.py patterns), PRD-039 (token budget enforcement), PRD-041 (OTel span cost attribution)
**Inspired by:** IBM ACP (Agent Communication Protocol), BeeAI Framework
**GitHub Issue:** #347

---

## 1. Overview

The Agent Communication Protocol (ACP) is a lightweight HTTP-based messaging standard developed under the Linux Foundation AGNTCY umbrella. Unlike A2A (which mandates JSON-RPC 2.0 over HTTP with Server-Sent Event streaming for task lifecycle management) or ANP (which grounds agent identity in DID documents and cryptographic Ed25519 HTTP Message Signatures per RFC 9421), ACP favors simplicity: an agent is a resource reachable via `POST /runs`, a cluster is a registry reachable via `GET /agents`, and intra-cluster calls are plain JSON over HTTPS with no mandatory streaming transport. This maps well to TAG's design philosophy of small, composable commands that default to synchronous behavior and opt into complexity only when needed.

TAG already runs a lightweight HTTP API server (`api.py`, PRD-036) that exposes run history, span waterfalls, and queue state to the web dashboard. Extending that server — or running a parallel server on a dedicated port — to speak ACP makes TAG agents first-class participants in any cluster that uses the BeeAI / ACP runtime. This means a TAG agent running `tag acp serve` on port 8081 can receive tasks from orchestrators like BeeAI's `bee-agent-framework`, from other `tag acp send` callers, and from any ACP-compatible client. The `/agents` registry endpoint allows the cluster coordinator to discover what agents are available, what capabilities they advertise, and how to route tasks to them.

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
| Budget enforcement | A run that would exceed the profile budget receives HTTP 402 before any LLM call is made | Unit test mocking the budget module's `check_budget()` call |
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

All ACP subcommands live under the `tag acp` namespace, implemented in `cmd_acp()` in `controller.py` with business logic delegated to `src/tag/acp_adapter.py`.

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
- `--log-level`: Python logging level for HTTP access logs and ACP lifecycle events.
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
| FR-01 | `tag acp serve` MUST start an HTTP/1.1 server using Python's `http.server.HTTPServer` (consistent with `api.py` pattern), binding to the configured host:port within 500 ms of invocation. | Must |
| FR-02 | `GET /agents` MUST return a JSON array of `AgentManifest` objects with fields: `name` (RFC 1123), `description`, `metadata`, `capabilities`. The array MUST contain at least the locally registered agent. | Must |
| FR-03 | `POST /runs` MUST accept a `RunRequest` body with `agent_name` and `input` (array of `Message` objects). MUST return HTTP 200 with a `Run` object in `status=created` within 50 ms of receiving the request (before LLM inference begins). | Must |
| FR-04 | On receiving a valid `POST /runs`, the server MUST immediately transition the run to `in-progress`, begin executing the task against the specified TAG profile, and update `acp_runs.status` in SQLite. | Must |
| FR-05 | `GET /runs/{run_id}` MUST return the current `Run` object with accurate `status`, `output` (if completed), and `await_request` (if awaiting). MUST return HTTP 404 with `{"error": "run not found"}` for unknown run IDs. | Must |
| FR-06 | `POST /runs/{run_id}` with a `RunResumeRequest` body MUST resume a run in `awaiting` status by delivering `await_resume` data to the waiting agent coroutine. MUST return HTTP 409 if the run is not in `awaiting` status. | Must |
| FR-07 | `DELETE /runs/{run_id}` MUST transition a `created` or `in-progress` run to `cancelling` immediately and then to `cancelled` once the running agent coroutine acknowledges cancellation. MUST return HTTP 409 if the run is already in a terminal state. | Must |
| FR-08 | All ACP run state (run ID, agent name, status, input messages, output messages, `await_request`, timestamps) MUST be persisted to `acp_runs` and `acp_run_events` in TAG's SQLite database before any response is returned to the caller. | Must |
| FR-09 | Agent names MUST be validated against `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` (RFC 1123 DNS label). `tag acp serve` MUST reject `--agent-name` values that fail this pattern with a clear error message listing the constraint. | Must |
| FR-10 | TAG profile names with uppercase letters or underscores MUST be auto-coerced to RFC 1123 form (lowercase, underscores → hyphens). If the coerced name would collide with an existing agent name, the server MUST append a 4-character hex suffix. | Must |
| FR-11 | `tag acp send` MUST propagate a `X-TAG-Trace-ID` and `X-TAG-Span-ID` header on every outbound HTTP request for distributed tracing (PRD-013). The receiving server MUST log these values in `acp_run_events` if present. | Must |
| FR-12 | Before executing a run, the server MUST call the budget module (PRD-039) to check if the run is within the profile's token budget. If the check fails, MUST return HTTP 402 with body `{"error": {"type": "budget_exceeded", "detail": "..."}}` without making any LLM call. | Must |
| FR-13 | When a run transitions to `awaiting`, the server MUST surface the blocked run via TAG's HITL approval system (PRD-078) so that `tag status` displays it as pending human input. | Should |
| FR-14 | `tag acp list-agents` MUST display a formatted table including `name`, `description`, and `capabilities` for each manifest. MUST support `--json` to emit the raw array. | Must |
| FR-15 | `GET /health` MUST return HTTP 200 with `{"status": "ok", "version": "<tag.__version__>", "uptime_s": <float>}` without requiring any database access. | Must |
| FR-16 | The ACP server MUST write a PID file to `--pid-file` on startup and remove it on clean shutdown. `tag acp stop` MUST use this file to send SIGTERM. | Should |
| FR-17 | `tag acp serve --json` MUST emit newline-delimited JSON events for every lifecycle transition: `acp_server_started`, `acp_run_created`, `acp_run_status_changed`, `acp_server_stopped`. | Should |
| FR-18 | The server MUST handle concurrent runs up to a configurable `--max-concurrent-runs` limit (default: 4). Requests that would exceed this limit MUST receive HTTP 503 with `{"error": {"type": "capacity_exceeded"}}`. | Should |
| FR-19 | `tag acp send --no-await` MUST return immediately after receiving the `Run` object from `POST /runs`, print the run ID, and exit 0. The run proceeds asynchronously on the server. | Must |
| FR-20 | Input `Message` objects in `RunRequest` MUST support both `TextPart` (`content_type=text/plain`) and `BlobPart` (`content_type`, `content` as base64). The adapter MUST pass blob parts to the TAG agent as temporary files in the sandbox. | Should |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Latency:** `POST /runs` response (before LLM inference) MUST complete in under 50 ms on a modern laptop. `GET /runs/{id}` MUST complete in under 20 ms. | p99 measured in integration tests |
| NFR-02 | **Throughput:** The server MUST handle at least 10 concurrent `GET /runs/{id}` poll requests per second without degradation, using threading (consistent with `api.py`). | Load test with `wrk` or `locust` |
| NFR-03 | **Reliability:** Any exception during run execution MUST result in the run transitioning to `failed` with the exception message in `acp_runs.error_detail`. The server process MUST NOT crash. | 100% coverage of exception paths in unit tests |
| NFR-04 | **SQLite WAL mode:** The `acp_adapter.py` MUST use `open_db()` (existing pattern), which enables WAL mode and `PRAGMA journal_mode=WAL`. All writes MUST use explicit transactions to avoid lock contention with the web dashboard's concurrent reads. | Verified by `PRAGMA journal_mode` assertion in tests |
| NFR-05 | **Dependency footprint:** `acp_adapter.py` MUST use only Python stdlib (`http.server`, `threading`, `json`, `uuid`, `logging`, `re`, `signal`) plus TAG's own modules. No new third-party dependencies for the core path. The optional ACP OpenAPI schema validator (`openapi-core`) is a dev dependency only. | `pip show tag` optional-deps check |
| NFR-06 | **Startup isolation:** Importing `tag.acp_adapter` MUST NOT start any threads, bind any ports, or open any sockets. All network activity begins only when `ACPServer.start()` is called. | Import-time unit test (`import tag.acp_adapter; assert no threads started`) |
| NFR-07 | **Security defaults:** The server MUST bind to `127.0.0.1` by default. Any use of `0.0.0.0` or an external IP MUST print a visible warning and be recorded in the ACP server's startup log event. | CLI integration test |
| NFR-08 | **Observability:** Every ACP run MUST produce at least one OTel span (PRD-041/PRD-013) in TAG's `spans` table with `acp.run_id`, `acp.agent_name`, and `acp.status` as span attributes following OTel GenAI semantic conventions where applicable. | Span attribute assertion in integration tests |
| NFR-09 | **Graceful shutdown:** `SIGTERM` to the server process MUST wait for all in-flight runs to either complete or transition to `failed` before exiting, with a maximum drain timeout of 30 seconds. | Signal handling integration test |
| NFR-10 | **Idempotency of registration:** Re-starting `tag acp serve` after a crash MUST NOT create duplicate entries in the `acp_agents` table; it MUST upsert using `INSERT OR REPLACE`. | Unit test asserting single-row after double registration |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/acp_adapter.py` | ACP HTTP server, request handlers, run lifecycle state machine, client functions (`send_run`, `list_agents`, `resume_run`, `cancel_run`) |
| `src/tag/integrations/acp_client.py` | Thin HTTP client wrapper used by `tag acp send` / `tag acp list-agents` — separated for testability |

**Modified files:**
- `src/tag/controller.py`: Add `cmd_acp()` and register `acp` subcommand with argparse.
- `src/tag/api.py`: Optionally expose ACP status via the existing web dashboard (read-only `GET /api/acp/runs` endpoint).

---

### 10.2 SQLite DDL

These tables are added to `~/.tag/runtime/tag.sqlite3` via `open_db()` on first ACP server start.

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

### 10.3 ACP Data Model (Python Dataclasses)

```python
# src/tag/acp_adapter.py
from __future__ import annotations

import dataclasses
import enum
import json
import re
import time
import uuid
from typing import Any

RFC1123_PATTERN = re.compile(r'^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$')
RFC1123_MAX_LEN = 63


class ACPRunStatus(str, enum.Enum):
    CREATED = "created"
    IN_PROGRESS = "in-progress"
    AWAITING = "awaiting"
    COMPLETED = "completed"
    FAILED = "failed"
    CANCELLED = "cancelled"
    CANCELLING = "cancelling"


@dataclasses.dataclass
class TextPart:
    content: str
    content_type: str = "text/plain"

    def to_dict(self) -> dict:
        return {"type": "text", "content": self.content, "content_type": self.content_type}


@dataclasses.dataclass
class BlobPart:
    content: bytes          # raw bytes; serialized as base64 in JSON
    content_type: str
    name: str | None = None

    def to_dict(self) -> dict:
        import base64
        return {
            "type": "blob",
            "content": base64.b64encode(self.content).decode(),
            "content_type": self.content_type,
            "name": self.name,
        }


@dataclasses.dataclass
class ACPMessage:
    role: str                   # "user" | "assistant"
    parts: list[TextPart | BlobPart]

    def to_dict(self) -> dict:
        return {"role": self.role, "parts": [p.to_dict() for p in self.parts]}

    @classmethod
    def from_dict(cls, d: dict) -> "ACPMessage":
        parts: list[TextPart | BlobPart] = []
        for p in d.get("parts", []):
            if p.get("type") == "text":
                parts.append(TextPart(
                    content=p["content"],
                    content_type=p.get("content_type", "text/plain"),
                ))
            elif p.get("type") == "blob":
                import base64
                parts.append(BlobPart(
                    content=base64.b64decode(p["content"]),
                    content_type=p["content_type"],
                    name=p.get("name"),
                ))
        return cls(role=d.get("role", "user"), parts=parts)


@dataclasses.dataclass
class ACPAwaitRequest:
    """Describes what an awaiting agent needs before it can resume."""
    type: str                   # e.g. "human_input", "external_event"
    description: str
    schema_json: dict | None = None   # optional JSON Schema for the expected resume data

    def to_dict(self) -> dict:
        d: dict = {"type": self.type, "description": self.description}
        if self.schema_json:
            d["schema"] = self.schema_json
        return d


@dataclasses.dataclass
class ACPRun:
    run_id: str
    agent_name: str
    status: ACPRunStatus
    input: list[ACPMessage]
    output: list[ACPMessage] | None = None
    await_request: ACPAwaitRequest | None = None
    error: str | None = None
    created_at: str = dataclasses.field(default_factory=lambda: _utcnow())
    started_at: str | None = None
    completed_at: str | None = None

    def to_dict(self) -> dict:
        d: dict = {
            "run_id": self.run_id,
            "agent_name": self.agent_name,
            "status": self.status.value,
            "input": [m.to_dict() for m in self.input],
            "created_at": self.created_at,
        }
        if self.output is not None:
            d["output"] = [m.to_dict() for m in self.output]
        if self.await_request is not None:
            d["await_request"] = self.await_request.to_dict()
        if self.error is not None:
            d["error"] = {"detail": self.error}
        if self.started_at:
            d["started_at"] = self.started_at
        if self.completed_at:
            d["completed_at"] = self.completed_at
        return d


@dataclasses.dataclass
class ACPAgentManifest:
    name: str                   # RFC 1123 DNS label
    description: str
    capabilities: list[str]     # e.g. ["run", "await"]
    metadata: dict[str, Any] = dataclasses.field(default_factory=dict)

    def to_dict(self) -> dict:
        return {
            "name": self.name,
            "description": self.description,
            "capabilities": self.capabilities,
            "metadata": self.metadata,
        }

    @classmethod
    def validate_name(cls, name: str) -> None:
        if not (1 <= len(name) <= RFC1123_MAX_LEN):
            raise ValueError(
                f"Agent name {name!r} must be 1-63 characters (RFC 1123). Got {len(name)}."
            )
        if not RFC1123_PATTERN.match(name):
            raise ValueError(
                f"Agent name {name!r} must match ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ (RFC 1123). "
                "Use only lowercase alphanumeric characters and hyphens."
            )

    @classmethod
    def coerce_profile_name(cls, profile_name: str) -> str:
        """Coerce a TAG profile name to a valid RFC 1123 DNS label."""
        coerced = profile_name.lower().replace("_", "-").replace(" ", "-")
        # Strip leading/trailing hyphens
        coerced = coerced.strip("-")
        # Remove any characters not in [a-z0-9-]
        coerced = re.sub(r'[^a-z0-9\-]', '', coerced)
        # Truncate to 63 chars
        coerced = coerced[:RFC1123_MAX_LEN]
        return coerced or "tag-agent"


def _utcnow() -> str:
    import datetime
    return datetime.datetime.utcnow().strftime('%Y-%m-%dT%H:%M:%S.%f')[:-3] + 'Z'
```

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

```python
VALID_TRANSITIONS: dict[ACPRunStatus, set[ACPRunStatus]] = {
    ACPRunStatus.CREATED:     {ACPRunStatus.IN_PROGRESS, ACPRunStatus.FAILED, ACPRunStatus.CANCELLING},
    ACPRunStatus.IN_PROGRESS: {ACPRunStatus.AWAITING, ACPRunStatus.COMPLETED, ACPRunStatus.FAILED, ACPRunStatus.CANCELLING},
    ACPRunStatus.AWAITING:    {ACPRunStatus.IN_PROGRESS, ACPRunStatus.FAILED, ACPRunStatus.CANCELLING},
    ACPRunStatus.CANCELLING:  {ACPRunStatus.CANCELLED},
    ACPRunStatus.COMPLETED:   set(),
    ACPRunStatus.FAILED:      set(),
    ACPRunStatus.CANCELLED:   set(),
}
```

---

### 10.5 HTTP Server Architecture

`ACPServer` extends Python's `HTTPServer` with a thread-pool worker for run execution. The request handler (`ACPRequestHandler`) derives from `BaseHTTPRequestHandler`. This follows the pattern established in `api.py` (PRD-036) for consistency and zero new dependencies.

```python
import http.server
import threading
import queue as stdlib_queue

class ACPServer:
    def __init__(
        self,
        host: str,
        port: int,
        manifest: ACPAgentManifest,
        tag_profile: str,
        db_path: str,
        max_concurrent_runs: int = 4,
    ):
        self._manifest = manifest
        self._tag_profile = tag_profile
        self._db_path = db_path
        self._runs: dict[str, ACPRun] = {}       # in-memory run cache
        self._run_lock = threading.Lock()
        self._resume_events: dict[str, threading.Event] = {}
        self._resume_data: dict[str, dict] = {}
        self._cancel_flags: dict[str, threading.Event] = {}
        self._semaphore = threading.Semaphore(max_concurrent_runs)
        self._start_time = time.monotonic()
        self._server = http.server.HTTPServer(
            (host, port),
            lambda *a, **kw: ACPRequestHandler(*a, acp=self, **kw),
        )

    def start(self) -> None:
        """Start serving in the current thread (blocking)."""
        self._server.serve_forever()

    def stop(self, drain_timeout: float = 30.0) -> None:
        self._server.shutdown()

    def uptime_seconds(self) -> float:
        return time.monotonic() - self._start_time

    def submit_run(self, run: ACPRun) -> None:
        """Persist run to SQLite, cache in memory, dispatch worker thread."""
        with self._run_lock:
            self._runs[run.run_id] = run
            self._cancel_flags[run.run_id] = threading.Event()
        self._persist_run(run)
        t = threading.Thread(
            target=self._execute_run,
            args=(run,),
            daemon=True,
            name=f"acp-run-{run.run_id[:8]}",
        )
        t.start()

    def get_run(self, run_id: str) -> ACPRun | None:
        with self._run_lock:
            return self._runs.get(run_id)

    def resume_run(self, run_id: str, resume_data: dict) -> bool:
        """Signal a waiting run to resume. Returns False if run is not awaiting."""
        with self._run_lock:
            run = self._runs.get(run_id)
            if run is None or run.status != ACPRunStatus.AWAITING:
                return False
            self._resume_data[run_id] = resume_data
        evt = self._resume_events.get(run_id)
        if evt:
            evt.set()
        return True

    def cancel_run(self, run_id: str) -> bool:
        """Request cancellation of a run. Returns False if already terminal."""
        with self._run_lock:
            run = self._runs.get(run_id)
            if run is None or run.status in (
                ACPRunStatus.COMPLETED, ACPRunStatus.FAILED,
                ACPRunStatus.CANCELLED, ACPRunStatus.CANCELLING,
            ):
                return False
        flag = self._cancel_flags.get(run_id)
        if flag:
            flag.set()
        self._transition(run_id, ACPRunStatus.CANCELLING)
        return True
```

---

### 10.6 Run Execution and TAG Integration

The `_execute_run` method bridges ACP runs into TAG's existing execution infrastructure. It extracts the text message from ACP `Message` objects, invokes the TAG agent via the `hermes_bridge` / `loop_agent` machinery, and translates the result back into ACP `Message` output.

```python
    def _execute_run(self, run: ACPRun) -> None:
        """Worker thread: execute an ACP run against the TAG agent."""
        from tag import budget as _budget
        from tag import tracing as _tracing

        if not self._semaphore.acquire(blocking=False):
            # Capacity exceeded — this shouldn't happen if HTTP layer checks,
            # but guard here for thread-safety.
            self._fail_run(run.run_id, "Server at capacity; try again later.")
            return

        try:
            self._transition(run.run_id, ACPRunStatus.IN_PROGRESS)

            # Extract text content from input messages
            text_parts = []
            for msg in run.input:
                for part in msg.parts:
                    if isinstance(part, TextPart):
                        text_parts.append(part.content)
            task_text = "\n\n".join(text_parts)

            # OTel span for this ACP run
            with _tracing.start_span(
                "acp.run",
                attributes={
                    "acp.run_id": run.run_id,
                    "acp.agent_name": run.agent_name,
                    "acp.status": "in-progress",
                    "tag.profile": self._tag_profile,
                },
                trace_id=run.trace_id,
                parent_span_id=run.span_id,
            ) as span:
                # Budget check
                cfg = _load_cfg(self._db_path)
                budget_ok, budget_detail = _budget.check_budget(
                    cfg, self._tag_profile, estimated_tokens=None
                )
                if not budget_ok:
                    span.set_attribute("acp.budget_exceeded", True)
                    self._fail_run(run.run_id, f"Budget exceeded: {budget_detail}")
                    return

                # Cancel check before invoking agent
                cancel_flag = self._cancel_flags.get(run.run_id, threading.Event())
                if cancel_flag.is_set():
                    self._transition(run.run_id, ACPRunStatus.CANCELLED)
                    return

                # Invoke TAG agent (blocking)
                try:
                    output_text = self._invoke_tag_agent(
                        task=task_text,
                        profile=self._tag_profile,
                        run_id=run.run_id,
                        cancel_flag=cancel_flag,
                    )
                except ACPAwaitRequiredException as e:
                    # Agent signalled it needs external input
                    self._transition(run.run_id, ACPRunStatus.AWAITING, await_request=e.request)
                    resume_evt = threading.Event()
                    self._resume_events[run.run_id] = resume_evt
                    resume_evt.wait()  # block until resume_run() is called
                    resume_data = self._resume_data.pop(run.run_id, {})
                    # Continue execution with resume data (simplified; real impl recurses)
                    output_text = self._invoke_tag_agent(
                        task=task_text,
                        profile=self._tag_profile,
                        run_id=run.run_id,
                        cancel_flag=cancel_flag,
                        resume_data=resume_data,
                    )

                if cancel_flag.is_set():
                    self._transition(run.run_id, ACPRunStatus.CANCELLED)
                    return

                output_msg = ACPMessage(
                    role="assistant",
                    parts=[TextPart(content=output_text)],
                )
                self._complete_run(run.run_id, output=[output_msg])
                span.set_attribute("acp.status", "completed")

        except Exception as exc:
            self._fail_run(run.run_id, str(exc))
        finally:
            self._semaphore.release()
```

---

### 10.7 Client Functions (`acp_client.py`)

```python
# src/tag/integrations/acp_client.py
from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from typing import Any


def submit_run(
    server_url: str,
    agent_name: str,
    messages: list[dict],
    trace_id: str | None = None,
    span_id: str | None = None,
) -> dict[str, Any]:
    """POST /runs to server_url. Returns the Run dict."""
    payload = json.dumps({
        "agent_name": agent_name,
        "input": messages,
    }).encode()
    headers = {"Content-Type": "application/json"}
    if trace_id:
        headers["X-TAG-Trace-ID"] = trace_id
    if span_id:
        headers["X-TAG-Span-ID"] = span_id
    req = urllib.request.Request(
        f"{server_url.rstrip('/')}/runs",
        data=payload,
        headers=headers,
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read())


def poll_run(server_url: str, run_id: str, timeout: float = 300.0, interval: float = 2.0) -> dict[str, Any]:
    """Poll GET /runs/{run_id} until terminal status or timeout."""
    deadline = time.monotonic() + timeout
    terminal = {"completed", "failed", "cancelled"}
    while time.monotonic() < deadline:
        req = urllib.request.Request(
            f"{server_url.rstrip('/')}/runs/{run_id}",
            method="GET",
        )
        with urllib.request.urlopen(req, timeout=10) as resp:
            run = json.loads(resp.read())
        if run.get("status") in terminal or run.get("status") == "awaiting":
            return run
        time.sleep(interval)
    raise TimeoutError(f"Run {run_id} did not complete within {timeout}s")


def list_agents(server_url: str) -> list[dict[str, Any]]:
    """GET /agents. Returns list of AgentManifest dicts."""
    req = urllib.request.Request(
        f"{server_url.rstrip('/')}/agents",
        method="GET",
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read())


def resume_run(server_url: str, run_id: str, resume_data: dict) -> dict[str, Any]:
    """POST /runs/{run_id} with RunResumeRequest."""
    payload = json.dumps({"await_resume": resume_data}).encode()
    req = urllib.request.Request(
        f"{server_url.rstrip('/')}/runs/{run_id}",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read())


def cancel_run(server_url: str, run_id: str) -> dict[str, Any]:
    """DELETE /runs/{run_id}."""
    req = urllib.request.Request(
        f"{server_url.rstrip('/')}/runs/{run_id}",
        method="DELETE",
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read())
```

---

### 10.8 Controller Integration (`cmd_acp` in `controller.py`)

```python
def cmd_acp(args: argparse.Namespace) -> int:
    """Dispatch ACP subcommands."""
    sub = getattr(args, "acp_subcommand", None)
    if sub == "serve":
        return _acp_serve(args)
    elif sub == "send":
        return _acp_send(args)
    elif sub == "list-agents":
        return _acp_list_agents(args)
    elif sub == "resume":
        return _acp_resume(args)
    elif sub == "cancel":
        return _acp_cancel(args)
    elif sub == "status":
        return _acp_status(args)
    elif sub == "stop":
        return _acp_stop(args)
    else:
        print("Usage: tag acp <serve|send|list-agents|resume|cancel|status|stop>")
        return 1
```

Argparse registration (added to the existing subcommand registration block in `controller.py`):

```python
# In build_parser() or equivalent argparse setup:
acp_parser = subparsers.add_parser("acp", help="ACP intra-cluster agent messaging")
acp_sub = acp_parser.add_subparsers(dest="acp_subcommand")

# serve
p_serve = acp_sub.add_parser("serve", help="Start ACP server")
p_serve.add_argument("--port", type=int, default=8081)
p_serve.add_argument("--host", default="127.0.0.1")
p_serve.add_argument("--profile", default=None)
p_serve.add_argument("--agent-name", default=None)
p_serve.add_argument("--agent-description", default=None)
p_serve.add_argument("--capabilities", default="run,await")
p_serve.add_argument("--pid-file", default=str(Path.home() / ".tag/runtime/acp.pid"))
p_serve.add_argument("--max-concurrent-runs", type=int, default=4)
p_serve.add_argument("--log-level", default="info")
p_serve.add_argument("--json", action="store_true")

# send
p_send = acp_sub.add_parser("send", help="Send a run to a remote ACP server")
p_send.add_argument("--to", required=True)
p_send.add_argument("--message", default=None)
p_send.add_argument("--message-file", default=None)
p_send.add_argument("--await", dest="do_await", action="store_true", default=True)
p_send.add_argument("--no-await", dest="do_await", action="store_false")
p_send.add_argument("--timeout", type=float, default=300.0)
p_send.add_argument("--poll-interval", type=float, default=2.0)
p_send.add_argument("--output-file", default=None)
p_send.add_argument("--trace-id", default=None)
p_send.add_argument("--json", action="store_true")

# list-agents
p_list = acp_sub.add_parser("list-agents", help="List agents on a remote ACP server")
p_list.add_argument("--server", required=True)
p_list.add_argument("--filter", default=None)
p_list.add_argument("--json", action="store_true")

# resume / cancel / status / stop (similar patterns omitted for brevity)
```

---

### 10.9 Integration with Existing TAG Modules

| Existing Module | Integration Point |
|-----------------|-------------------|
| `open_db()` (`controller.py:355`) | Used by `ACPServer._persist_run()`, `_transition()`, `_complete_run()`, `_fail_run()` for all SQLite writes. |
| `tracing.py` (PRD-013) | `_execute_run()` wraps each run in an `acp.run` span; `X-TAG-Trace-ID` / `X-TAG-Span-ID` headers are parsed from inbound requests and used as parent context. |
| `budget.py` (PRD-039) | `_execute_run()` calls `budget.check_budget(cfg, profile)` before any LLM call; HTTP 402 returned if budget exceeded. |
| `security.py` (PRD-034) | Input messages are scanned for secrets/PII before being stored in SQLite or forwarded to the agent. |
| `sandbox.py` (PRD-028) | BlobPart inputs are written to a sandboxed temp directory before being passed to the agent. |
| `api.py` (PRD-036) | `GET /api/acp/runs` added to the web dashboard API server for read-only ACP run visibility. |
| `notifications.py` (PRD-040) | When a run transitions to `awaiting`, a notification is emitted (if configured) so operators are alerted. |

---

## 11. Security Considerations

1. **Bind address default is loopback.** The server binds to `127.0.0.1` by default. Any deviation (`--host 0.0.0.0` or a non-loopback IP) prints a bold warning and records the deviation in the startup log event. This is a defense-in-depth measure; actual network security must be handled at the network/infra layer.

2. **No authentication on the ACP server in v1.** This is intentional for intra-cluster use where Kubernetes NetworkPolicy or Docker Compose network isolation handles access control. A future hardening PRD will add optional Bearer token validation via `Authorization: Bearer <token>` header, with the expected token stored in TAG's secret store.

3. **Input validation before SQLite writes.** All `POST /runs` request bodies are parsed and validated before any database write. Malformed JSON returns HTTP 400. Fields are parameterized in all SQLite queries (no string interpolation) to prevent SQL injection.

4. **Secret scanning on message content.** Before storing input messages in `acp_runs.input_json` or forwarding to the agent, the content is passed through TAG's secret scanner (`security.py`, PRD-034) to detect and reject requests containing API keys, passwords, or credential patterns. The scanner runs in-process; no network call is made.

5. **Run ID is a UUIDv4.** Run IDs are generated server-side using `uuid.uuid4()`. They are not predictable or sequential, so enumerating run IDs via `GET /runs/{id}` is not feasible without prior knowledge of the ID.

6. **BlobPart sandbox isolation.** Binary content from ACP `BlobPart` inputs is written to a sandboxed temporary directory (PRD-028) and the agent receives only the file path. The temporary directory is removed after the run completes or fails.

7. **Budget enforcement is pre-flight.** The budget check (FR-12) occurs before any LLM call is made. This prevents budget exhaustion through concurrent run submissions. The check uses the budget module's `check_budget()` which reads from SQLite in a read transaction.

8. **Error responses do not leak internals.** `status=failed` error messages are truncated to 500 characters and Python tracebacks are never included in the HTTP response body. Full stack traces are written only to the TAG log file and the `acp_run_events` table.

9. **PID file permissions.** The PID file at `~/.tag/runtime/acp.pid` is written with permissions `0o600` (owner read/write only). `tag acp stop` verifies that the process in the PID file is owned by the current user before sending SIGTERM.

10. **Header injection prevention.** `X-TAG-Trace-ID` and `X-TAG-Span-ID` values from inbound requests are validated as hex strings (UUID format) before being stored in SQLite or logged. Non-conforming values are silently ignored with a debug log entry.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_acp_adapter.py`)

| Test | Description |
|------|-------------|
| `test_manifest_name_validation` | Assert `ACPAgentManifest.validate_name()` rejects uppercase, underscores, leading/trailing hyphens, length > 63, empty string. Assert valid names pass. |
| `test_profile_name_coercion` | Table-driven: `"My_Coder"` → `"my-coder"`, `"UPPER__CASE"` → `"upper--case"` (then strip double hyphens), `"123"` → `"123"`, long name → truncated to 63 chars. |
| `test_run_status_transitions_valid` | For each (from, to) in `VALID_TRANSITIONS`, assert `_validate_transition()` does not raise. |
| `test_run_status_transitions_invalid` | For each (from, to) NOT in `VALID_TRANSITIONS`, assert `_validate_transition()` raises `InvalidTransitionError`. |
| `test_text_part_serialization` | Round-trip `TextPart` → dict → `ACPMessage.from_dict()`. |
| `test_blob_part_serialization` | Round-trip `BlobPart` with binary content through base64 encode/decode. |
| `test_budget_check_http_402` | Mock `budget.check_budget()` to return `(False, "over limit")`. Assert `_execute_run()` sets `status=failed` and that the HTTP response for the run shows the budget error detail. |
| `test_run_id_is_uuid4` | Assert run IDs match `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`. |
| `test_import_does_not_start_threads` | Import `tag.acp_adapter`; assert `threading.active_count()` is unchanged. |
| `test_sql_ddl_creates_tables` | Call `_ensure_schema(conn)` on an in-memory SQLite connection; assert all three tables exist with the expected columns. |
| `test_secret_scanner_rejects_api_key` | Submit a `POST /runs` with a message containing `sk-...` API key pattern. Assert HTTP 400 is returned and no row is written to `acp_runs`. |

### 12.2 Integration Tests (`tests/test_acp_integration.py`)

| Test | Description |
|------|-------------|
| `test_serve_and_list_agents` | Start `ACPServer` on a random port in a background thread. `GET /agents`; assert the test agent manifest is in the response. |
| `test_submit_run_and_poll_completion` | Submit a run with a mock TAG agent that returns immediately. Poll until `status=completed`; assert output messages are present and `acp_runs.status='completed'` in SQLite. |
| `test_full_lifecycle_awaiting_resume` | Mock TAG agent that raises `ACPAwaitRequiredException`. Verify run transitions to `awaiting`; call `resume_run()`; verify run completes. Assert `acp_run_events` contains `awaiting` and `resumed` events. |
| `test_cancel_in_progress_run` | Submit a slow-running mock agent. Call `cancel_run()`; assert run transitions through `cancelling` to `cancelled`. |
| `test_http_404_unknown_run` | `GET /runs/nonexistent-id`; assert HTTP 404. |
| `test_http_409_resume_non_awaiting` | Submit a completed run; attempt to `POST /runs/{id}` (resume); assert HTTP 409. |
| `test_http_503_over_capacity` | Set `max_concurrent_runs=1`; submit 2 simultaneous runs; assert the second receives HTTP 503. |
| `test_trace_header_propagation` | Submit run with `X-TAG-Trace-ID: test-trace-123`. Assert `acp_runs.trace_id='test-trace-123'` in SQLite. |
| `test_health_endpoint` | `GET /health`; assert HTTP 200 and body contains `{"status": "ok"}`. |
| `test_concurrent_poll_performance` | Submit 1 run; fire 10 concurrent `GET /runs/{id}` requests from threads; assert all respond in under 50 ms each. |

### 12.3 CLI Integration Tests (`tests/test_acp_cli.py`)

| Test | Description |
|------|-------------|
| `test_tag_acp_serve_starts` | Invoke `tag acp serve --port <random> --profile test` as a subprocess. Assert it starts, `GET /health` succeeds, and process terminates cleanly on SIGTERM. |
| `test_tag_acp_send_local` | Start server; run `tag acp send --to http://localhost:<port> --message "hello"` subprocess. Assert exit code 0 and output contains agent response. |
| `test_tag_acp_list_agents_json` | Start server; run `tag acp list-agents --server http://localhost:<port> --json`. Assert output is valid JSON array with `name` field. |
| `test_tag_acp_send_no_await` | Run with `--no-await`; assert exit code 0 and output contains run ID. |
| `test_tag_acp_serve_network_warning` | Run `tag acp serve --host 0.0.0.0 --port <random>`. Assert stderr contains `WARNING: ACP server is network-accessible`. |

### 12.4 Performance Tests

| Test | Target |
|------|--------|
| `POST /runs` response time (before LLM) | p99 < 50 ms (100 iterations, sequential) |
| `GET /runs/{id}` response time | p99 < 20 ms (1000 iterations, sequential against SQLite WAL) |
| Server startup to first healthy response | < 500 ms |
| Concurrent poll throughput | 10 req/s without degradation (10 threads × 100 requests each) |

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
| AC-18 | `tag acp stop` terminates the server process within 5 seconds and removes the PID file. | CLI integration test with `subprocess.Popen` |
| AC-19 | The server handles 10 concurrent `GET /runs/{id}` requests simultaneously without deadlock or error. | Concurrent integration test using `concurrent.futures.ThreadPoolExecutor` |
| AC-20 | `GET /health` returns HTTP 200 with `{"status": "ok"}` at any point after server startup, including during active run execution. | Integration test |

---

## 14. Dependencies

| Dependency | Type | Purpose | Notes |
|------------|------|---------|-------|
| `http.server` (Python stdlib) | Runtime | HTTP server for ACP endpoints | Consistent with `api.py` pattern |
| `threading` (Python stdlib) | Runtime | Worker threads for run execution, run lock, semaphore | |
| `uuid` (Python stdlib) | Runtime | UUIDv4 run ID generation | |
| `re` (Python stdlib) | Runtime | RFC 1123 name validation | |
| `signal` (Python stdlib) | Runtime | SIGTERM handler for graceful shutdown | |
| `tag.tracing` (PRD-013) | Runtime | OTel span creation for `acp.run` spans | Must be merged before ACP |
| `tag.budget` (PRD-039) | Runtime | Pre-flight budget check | Must be merged before ACP |
| `tag.security` (PRD-034) | Runtime | Input message secret scanning | Must be merged before ACP |
| `tag.sandbox` (PRD-028) | Runtime (optional) | BlobPart temp file isolation | Required only if BlobPart support is enabled |
| `tag.notifications` (PRD-040) | Runtime (optional) | `awaiting` run notifications | Gracefully skipped if not configured |
| `openapi-core` | Dev/test only | ACP OpenAPI schema response validation in tests | Not imported in production code |
| ACP OpenAPI spec (`openapi.yaml`) | Spec reference | Normative ACP request/response schema | From `github.com/i-am-bee/acp/blob/main/docs/spec/openapi.yaml` |

---

## 15. Open Questions

| # | Question | Owner | Due | Notes |
|---|----------|-------|-----|-------|
| OQ-1 | Should `tag acp serve` support running the server as a daemon (background process) with `--daemon` flag, like `tag queue start --daemon`? Or is foreground-only acceptable for v1? | @team | Pre-implementation | Foreground preferred for v1 to keep signal handling simple. |
| OQ-2 | ACP's `await/resume` is designed for the BeeAI server-side `AsyncGenerator[RunYield, RunYieldResume]` pattern. How does TAG signal an `awaiting` state to the calling client given the synchronous HTTP polling model in v1? Current plan: worker thread blocks on `threading.Event`; client polls `GET /runs/{id}` and sees `status=awaiting`. Is this sufficient, or do we need a webhook/SSE notification? | @team | Pre-implementation | Polling is sufficient for v1 per PRD-scope constraints. |
| OQ-3 | The ACP spec says `await_request.type` is an open string. What specific `await_request.type` values should TAG use for its HITL cases? Proposal: `"human_approval"` for PRD-078 approval gates. | @team | Pre-implementation | Needs alignment with PRD-078 owner. |
| OQ-4 | Should `acp_runs` have a foreign key to `runs.id`? This requires that each ACP run creates a corresponding row in the existing `runs` table for dashboard visibility. Is that the right approach, or should ACP runs be a standalone table visible only via `/api/acp/runs`? | @team | Pre-implementation | Creating a `runs` row per ACP run gives maximum dashboard integration at the cost of schema coupling. |
| OQ-5 | The ACP `POST /runs` endpoint in the OpenAPI spec uses `agent_name` to target a specific agent in the registry. If a cluster runs multiple TAG agents (each as a separate `tag acp serve` process), should TAG support a multi-agent server that proxies to the correct backend based on `agent_name`? | @team | Post-v1 | Out of scope for this PRD. Each `tag acp serve` instance hosts exactly one agent. |
| OQ-6 | Should the `capabilities` field in `AgentManifest` be a free-form list or restricted to the ACP-defined set (`run`, `await`, `stream`)? The BeeAI OpenAPI schema is evolving; should TAG follow the upstream schema strictly or allow extension? | @team | Pre-implementation | Restrict to `["run", "await"]` in v1; `stream` reserved for SSE follow-up. |
| OQ-7 | `tag acp send --message` currently interprets the argument as `text/plain`. Should there be support for `--message-type application/json` to send structured JSON as a single `TextPart` with `content_type=application/json`? | @team | Pre-implementation | Add `--message-content-type` flag in v1 with default `text/plain`. |

---

## 16. Complexity and Timeline

### Phase 1 — Foundation (Days 1-3)

- `acp_adapter.py`: `ACPAgentManifest`, `ACPRun`, `ACPMessage`, `TextPart`, `BlobPart`, `ACPRunStatus` dataclasses and enums.
- `ACPAgentManifest.validate_name()` and `coerce_profile_name()` with full unit tests.
- State machine: `VALID_TRANSITIONS`, `_validate_transition()`, unit tests for all valid/invalid pairs.
- SQLite DDL: `acp_agents`, `acp_runs`, `acp_run_events` tables; `_ensure_schema()` function.
- `acp_client.py`: `submit_run`, `poll_run`, `list_agents`, `resume_run`, `cancel_run` using `urllib.request`.

**Deliverable:** All dataclasses and state machine logic with 100% unit test coverage. No HTTP server yet.

### Phase 2 — Server (Days 4-6)

- `ACPServer` class: `__init__`, `start`, `stop`, `submit_run`, `get_run`, `resume_run`, `cancel_run`.
- `ACPRequestHandler`: route parsing, `do_GET` for `/agents`, `/runs/{id}`, `/health`; `do_POST` for `/runs`, `/runs/{id}`; `do_DELETE` for `/runs/{id}`.
- Worker thread pool with `threading.Semaphore` for `max_concurrent_runs`.
- Graceful shutdown with drain timeout.
- Integration tests: full server lifecycle in background thread.

**Deliverable:** Functional ACP server that passes all integration tests with mock agents.

### Phase 3 — TAG Agent Integration (Days 7-9)

- `_execute_run()`: bridge ACP run to TAG's hermes_bridge / loop_agent.
- Budget check integration (`budget.check_budget()`).
- OTel span creation (`tracing.start_span("acp.run", ...)`).
- Secret scanning on input messages.
- `ACPAwaitRequiredException` + `threading.Event` pause/resume mechanism.
- HITL notification via `notifications.py` on `awaiting` transition.
- Full integration tests including budget rejection and await/resume lifecycle.

**Deliverable:** ACP runs actually execute TAG agents with full observability, budget enforcement, and HITL support.

### Phase 4 — CLI Surface (Days 10-11)

- `cmd_acp()` in `controller.py` with all subcommands.
- Argparse registration for `serve`, `send`, `list-agents`, `resume`, `cancel`, `status`, `stop`.
- Human-readable output formatters for each subcommand.
- `--json` mode for all subcommands.
- PID file write/remove.
- `tag acp stop` with SIGTERM/SIGKILL.
- CLI integration tests.

**Deliverable:** Full `tag acp` CLI surface working end-to-end.

### Phase 5 — Web Dashboard + Polish (Days 12-14)

- `GET /api/acp/runs` added to `api.py` for web dashboard read-only visibility.
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
