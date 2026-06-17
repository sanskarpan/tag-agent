# PRD-079: Cloud-Hosted Tool Execution with Version Pinning (`tag mcp host`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `sandbox.py + mcp_host.py` (new), `controller.py` (new `cmd_mcp_host_*` handlers), `tag.sqlite3` (new tables)
**Depends on:** PRD-028 (sandbox code execution), PRD-014 (MCP server registry), PRD-013 (agent tracing/observability), PRD-034 (secret scanning), PRD-039 (token budget enforcement)
**Inspired by:** Toolhouse tool hosting, E2B tool execution, Composio hosted tools
**GitHub Issue:** #346

---

## 1. Overview

Every MCP server that TAG uses today must be installed locally. `npx @modelcontextprotocol/server-notion` pulls Node.js packages into the user's environment. `uvx mcp-playwright` drags in Chromium, browser drivers, and system libraries. `mcp-server-postgres` needs a matching libpq version. Users routinely hit installation failures before ever running a single tool call — broken native dependency chains, version conflicts with pre-existing Node or Python environments, platform-specific binary mismatches on Apple Silicon, and incompatible system libraries on Ubuntu vs. Debian. The path from "I want to use MCP server X" to "X is working in my agent session" involves more yak-shaving than actual work.

`tag mcp host` solves this by running MCP servers inside isolated cloud containers instead of on the host machine. When a user runs `tag mcp host add notion --version 1.2.3 --backend docker`, TAG pulls a pre-built container image for the requested server at the exact specified version, starts it in a subprocess-managed Docker container (or a Modal serverless function), and exposes a local stdio bridge that Hermes connects to as though the server were installed natively. The host machine never accumulates npm packages, Python venvs, or system-level Chromium installations. Teardown is instantaneous — `tag mcp host remove notion` stops the container and the filesystem returns to exactly the state it was in before.

Version pinning is the second critical capability. The MCP spec has no built-in versioning mechanism. A tool description can change between server releases — parameter names shift, required fields become optional, entire tools are renamed — and an agent session has no way to detect the drift. TAG addresses this by computing a SHA-256 content hash over the semantic interface of every hosted server (tool names + descriptions + input schemas, normalized and sorted) at first connection and persisting this hash as a contract snapshot in SQLite. On every subsequent session start, the live server interface is hashed again and compared to the snapshot. A mismatch triggers a configurable fail-fast gate before any tool calls are made, preventing silent behavioral regressions that are nearly impossible to diagnose after the fact.

Per-tool execution quotas are the third pillar. Certain MCP servers — Playwright browser automation in particular — are expensive in CPU, memory, and wall time. A single unbounded Playwright run can saturate a 16-core machine and consume thousands of Modal compute seconds. TAG enforces per-server and per-tool invocation quotas stored in SQLite. When a hosted server approaches its quota, TAG emits a warning to the terminal and the agent's tool result. When the quota is exceeded, TAG's hosted server bridge returns a structured error to the agent instead of forwarding the call, which allows the agent to gracefully acknowledge the limit rather than hanging or crashing.

This feature is intentionally positioned as a developer ergonomics improvement, not a security boundary. Container isolation provided by the Docker backend is a useful side effect, but the canonical security sandbox for untrusted code remains PRD-028. The hosted tool execution system focuses on reproducibility, dependency isolation, and the reliable version contract that teams need to build durable agent pipelines.

---

## 2. Problem Statement

### 2.1 Local MCP Server Installation Is Fragile and Polluting

TAG's current MCP workflow requires users to resolve all transitive dependencies before a server is usable. `mcp-playwright` installs ~150 MB of Chromium binaries into a global cache that conflicts with other Playwright versions. `mcp-server-notion` silently uses a different Node.js version than the project's `.nvmrc` specifies. `mcp-postgres` links against the system libpq and breaks when Homebrew updates it. Each MCP server is an island of dependency management friction. Users on macOS ARM64, Ubuntu 22.04, and NixOS each encounter distinct failure modes, and the TAG documentation cannot enumerate all of them. Worse, removing a server does not cleanly uninstall its transitive dependencies, leaving the host environment progressively more polluted over time.

### 2.2 Tool Interface Changes Are Invisible at Runtime

The MCP protocol sends tool definitions — names, descriptions, and input schemas — as part of the server's `initialize` response, but there is no version field, no changelog, no diff. When a team pins their agent prompt to the parameter name `database_url` and a new server version renames it to `connection_string`, the agent silently sends the old parameter name, the tool fails, and the failure is attributed to agent quality rather than interface drift. Reproducing the bug requires manually correlating the server version, the session timestamp, and the tool schema — information that is not currently captured anywhere in TAG's state. The lack of a versioned tool contract is the single largest source of unexplained behavioral regressions in MCP-based agent pipelines.

### 2.3 Unbounded Resource Consumption Degrades Agent Sessions

Playwright and similar browser-automation servers can consume 2–4 GB of RAM and multiple CPU cores for a single operation. When run inside a long-lived agent loop (PRD-021), a single slow tool call holds the entire session hostage. There is no mechanism to cap the number of tool invocations for a given server within a session, no timeout escalation path shorter than killing the entire agent process, and no way to reserve compute budget for higher-priority tools. Users who enable Playwright alongside filesystem and code tools discover that the browser-automation server monopolizes system resources and degrades the perceived quality of all other tool calls. The Cursor editor warns that Playwright alone consumes 25 of the 40-tool limit — but TAG currently exposes no equivalent quota awareness layer.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | `tag mcp host add <server> --version <ver> --backend <docker|modal>` pulls, starts, and registers a cloud-hosted MCP server in under 60 seconds on a warm image cache. |
| G2 | Version pinning: on first connection, compute a SHA-256 content hash of the server's tool interface (names + descriptions + input schemas) and persist it as a contract snapshot. Fail fast before session start if the hash drifts from the snapshot. |
| G3 | Per-server and per-tool invocation quotas: configurable via `--quota-calls` and `--quota-window`; quota state persisted in SQLite; warnings at 80% consumption, hard stop at 100%. |
| G4 | `tag mcp host list --json` returns all registered hosted servers with status, version, backend, quota utilization, and contract hash. |
| G5 | `tag mcp host logs <server>` streams the last N lines of container stdout/stderr with live tail. |
| G6 | `tag mcp host remove <server>` stops and removes the container/function, deletes the quota record, and optionally purges the contract snapshot. |
| G7 | Docker backend: pull images, run containers, stream logs — all via the `docker` CLI subprocess, requiring no Python Docker SDK. Modal backend: deploy a `modal.Function` wrapping the server's stdio protocol. |
| G8 | The stdio bridge layer is transparent to Hermes: TAG registers the hosted server as a stdio-type MCP server and the bridge handles protocol translation without any Hermes configuration changes. |
| G9 | All hosted server activity (start, stop, tool call proxied, quota hit, hash mismatch) is appended to a structured audit log at `~/.tag/runtime/mcp-host-audit.jsonl`. |
| G10 | Zero new mandatory runtime dependencies for users who do not use `tag mcp host`. All imports are lazy; `import tag.controller` is not affected. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Building or publishing container images for MCP servers. TAG consumes existing images from Docker Hub or ghcr.io; it does not maintain a container registry. |
| NG2 | Full Kubernetes or container orchestration. TAG manages one container per hosted server via CLI subprocess calls; no orchestration layer is introduced. |
| NG3 | Network policy enforcement between containers. Docker's default bridge network is used; fine-grained eBPF-based network policy is out of scope (handled by PRD-028's network isolation). |
| NG4 | Multi-user or tenant isolation. This feature targets single-user TAG installations. Shared-server deployments require a separate PRD. |
| NG5 | Automatic MCP server image publishing. If an MCP server has no official Docker image, the user must provide a Dockerfile. TAG does not build images on demand. |
| NG6 | Streaming Streamable-HTTP transport between the hosted container and Hermes. v1 uses stdio bridging only. HTTP transport is a v2 consideration. |
| NG7 | Image vulnerability scanning. Users are responsible for trusting the images they reference. CVE scanning is out of scope. |
| NG8 | Replacing PRD-028 sandbox as the primary security isolation layer. Container isolation here is a side effect of dependency management, not the primary safety control. |

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Time to first tool call (warm Docker cache) | < 60 s from `tag mcp host add` to first successful tool invocation | Automated integration test with pre-pulled image |
| Time to first tool call (cold Docker pull) | < 3 min for a typical MCP server image (< 500 MB) | Measured on GH Actions runner with cold cache |
| Version hash false-positive rate | 0 false-positive contract failures across 100 sequential restarts of the same pinned version | Regression test suite |
| Quota enforcement accuracy | Zero tool calls proceed after quota is exhausted in a 1000-call simulation | Unit test with mocked bridge |
| Host environment pollution | Zero files written outside `~/.tag/` on a fresh macOS install after `add + remove` cycle | fs snapshot diff before/after |
| Log streaming latency | `tag mcp host logs` first line appears within 500 ms of container start | timed integration test |
| Audit log completeness | Every proxied tool call has a corresponding audit log entry with < 1 ms clock skew | Log entry count == call count assertion |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag mcp host add notion --version 1.2.3 --backend docker` | I get Notion tools in my agent without installing Node.js or dealing with npm dependency conflicts |
| U2 | Team lead | pin `playwright` to `--version 1.0.0` on all team machines via a shared `tag-config.yaml` | every engineer's agent session uses the exact same Playwright tool interface and I can reproduce bugs deterministically |
| U3 | Platform engineer | receive a `ContractHashMismatch` error when notion's tool interface changes between `1.2.3` and `1.2.4` | I catch the breaking change in CI before it reaches production agent sessions |
| U4 | Developer | run `tag mcp host list --json` | I get a machine-readable inventory of all hosted servers, their versions, and quota status for monitoring dashboards |
| U5 | Developer | run `tag mcp host logs notion --tail 100` | I can debug a failing Notion API call by reading the server's stderr without attaching to the container manually |
| U6 | Platform engineer | set `--quota-calls 500 --quota-window 1h` on playwright | the agent loop cannot exhaust Modal compute credits in a single runaway session |
| U7 | Developer | run `tag mcp host remove notion` | the container stops immediately and my Docker system is not left with a dangling process |
| U8 | Developer | run `tag mcp host add notion --version latest` | TAG resolves "latest" to the concrete version tag from the MCP registry and pins that concrete version, not a floating tag |
| U9 | CI engineer | set `MCP_HOST_FAIL_ON_HASH_DRIFT=1` in the environment | the CI pipeline exits non-zero when any hosted server's contract hash changes, blocking merges that would silently break agent behavior |
| U10 | Developer | run `tag mcp host inspect notion` | I see the full contract snapshot (tool names, descriptions, schema hashes) that was captured at pin time, for audit and diff purposes |

---

## 7. Proposed CLI Surface

All hosted-server subcommands live under `tag mcp host`.

### 7.1 `tag mcp host add`

Register and start a cloud-hosted MCP server.

```
tag mcp host add <server-name>
  [--version <semver|latest>]    # default: latest (resolved to concrete tag)
  [--backend <docker|modal>]     # default: docker
  [--image <image:tag>]          # override default image resolution
  [--env KEY=VALUE ...]          # environment variables injected into the container
  [--secret KEY=<keychain-ref>]  # secrets pulled from OS keychain, never stored in config
  [--quota-calls <N>]            # max tool invocations per quota window (default: unlimited)
  [--quota-window <duration>]    # quota window: 1h, 24h, 7d (default: 1h)
  [--no-pin]                     # skip contract hash capture (not recommended)
  [--profile <profile>]          # associate with a specific TAG profile
  [--pull-policy <always|if-absent|never>]  # Docker pull policy (default: if-absent)
  [--dry-run]                    # print resolved config without starting container
```

**Example output:**

```
$ tag mcp host add notion --version 1.2.3 --backend docker --quota-calls 200 --quota-window 1h

Resolving image for notion@1.2.3...
  Found: ghcr.io/modelcontextprotocol/notion-mcp:1.2.3

Pulling image ghcr.io/modelcontextprotocol/notion-mcp:1.2.3 ...
  Layer 1/4  sha256:a1b2...  [==============================] 45 MB/45 MB
  Layer 2/4  sha256:c3d4...  [==============================] 12 MB/12 MB
  Layer 3/4  sha256:e5f6...  [==============================] 8 MB/8 MB
  Layer 4/4  sha256:g7h8...  [==============================] 3 MB/3 MB
  Image ready.

Starting container tag-mcp-notion-a3f9b1...
  Container ID: a3f9b1e72d14
  Bridge PID:   38421

Connecting to MCP server...
  Protocol handshake complete in 1.2 s

Capturing contract snapshot...
  Tools discovered: 8
    create_page, update_page, get_page, delete_page,
    search_pages, list_databases, query_database, append_block
  Contract hash: sha256:4e7a2b9c1d3f...

Registered notion@1.2.3 (docker) with quota 200 calls/1h
  Config written to ~/.tag/mcp-host/notion.yaml
  Audit log: ~/.tag/runtime/mcp-host-audit.jsonl

Ready. Add to profile with:
  tag profile edit <profile> --add-mcp notion
```

### 7.2 `tag mcp host list`

List all registered hosted servers.

```
tag mcp host list
  [--json]           # machine-readable JSON output
  [--profile <name>] # filter by associated profile
```

**Human-readable output:**

```
$ tag mcp host list

SERVER     VERSION   BACKEND   STATUS    QUOTA           CONTRACT
notion     1.2.3     docker    running   42/200 (21%)    4e7a2b9c OK
playwright latest→1.0.0  modal  stopped  0/500 (0%)     9f3c1a7d OK
postgres   2.1.0     docker    running   0/∞             c8e4b2f1 OK
```

**JSON output (`--json`):**

```json
[
  {
    "name": "notion",
    "version": "1.2.3",
    "image": "ghcr.io/modelcontextprotocol/notion-mcp:1.2.3",
    "backend": "docker",
    "container_id": "a3f9b1e72d14",
    "status": "running",
    "quota_calls_used": 42,
    "quota_calls_limit": 200,
    "quota_window": "1h",
    "quota_window_reset_at": "2026-06-17T15:00:00Z",
    "contract_hash": "sha256:4e7a2b9c1d3f...",
    "contract_ok": true,
    "registered_at": "2026-06-17T14:00:00Z",
    "profile": "researcher"
  }
]
```

### 7.3 `tag mcp host logs`

Stream container logs.

```
tag mcp host logs <server-name>
  [--tail <N>]     # last N lines (default: 50)
  [--follow]       # live tail (Ctrl-C to stop)
  [--since <ts>]   # only logs after RFC3339 timestamp
  [--stderr-only]  # filter to stderr
```

**Example:**

```
$ tag mcp host logs notion --tail 20 --follow

2026-06-17T14:02:11.340Z [notion-mcp] Server initialized
2026-06-17T14:02:11.341Z [notion-mcp] Registered 8 tools
2026-06-17T14:03:45.112Z [notion-mcp] Tool call: search_pages {"query": "Q3 OKRs"}
2026-06-17T14:03:45.893Z [notion-mcp] search_pages returned 5 results (781 ms)
```

### 7.4 `tag mcp host remove`

Stop and deregister a hosted server.

```
tag mcp host remove <server-name>
  [--keep-snapshot]  # retain contract snapshot for future comparison
  [--force]          # remove even if server is active in a running session
```

**Example:**

```
$ tag mcp host remove notion

Stopping container tag-mcp-notion-a3f9b1 ...
  Container stopped.
  Container removed.
Deregistered notion from mcp_hosted_servers.
Contract snapshot retained (use --keep-snapshot=false to purge).
```

### 7.5 `tag mcp host inspect`

Show the full contract snapshot for a server.

```
tag mcp host inspect <server-name>
  [--json]
  [--diff <other-version>]  # compare contract hashes across two registered versions
```

**Example output:**

```
$ tag mcp host inspect notion

notion@1.2.3 — Contract Snapshot
  Captured:    2026-06-17T14:00:22Z
  Hash:        sha256:4e7a2b9c1d3f...
  Tool count:  8

  TOOL              PARAM COUNT   SCHEMA HASH
  create_page       5             sha256:1a2b3c...
  update_page       4             sha256:4d5e6f...
  get_page          1             sha256:7g8h9i...
  delete_page       1             sha256:0j1k2l...
  search_pages      3             sha256:3m4n5o...
  list_databases    0             sha256:6p7q8r...
  query_database    4             sha256:9s0t1u...
  append_block      3             sha256:2v3w4x...
```

### 7.6 `tag mcp host start` / `tag mcp host stop`

Lifecycle management without re-pulling.

```
tag mcp host start <server-name>   # restart a stopped server using existing config
tag mcp host stop <server-name>    # stop without removing registration
```

### 7.7 `tag mcp host quota reset`

Reset quota counters manually (for testing or one-time overrides).

```
tag mcp host quota reset <server-name>
  [--tool <tool-name>]   # reset only a specific tool's counter
  [--confirm]            # required flag to prevent accidental resets
```

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag mcp host add` MUST accept `--backend docker` and `--backend modal` as mutually exclusive values; any other value produces a clear error with the list of valid options. | Must |
| FR-02 | When `--version latest` is specified, TAG MUST resolve the concrete version from the MCP registry (GET `/v0.1/servers/{name}/versions?version=latest`) and record the resolved concrete tag in the SQLite row, never storing "latest" as the pinned version. | Must |
| FR-03 | On first connection to a hosted server, TAG MUST compute `sha256(json.dumps(sorted_tool_interface))` and persist the result to `mcp_contract_snapshots`. The sort order is: outer list sorted by tool name ascending; per-tool inner dict has keys `name`, `description`, `inputSchema` only (excluding server timestamps). | Must |
| FR-04 | On each subsequent session start that includes a hosted server, TAG MUST re-derive the contract hash from the live server and compare it to the snapshot. If they differ, TAG MUST emit a `ContractHashMismatch` error to stderr and exit non-zero unless `--allow-drift` is passed or `MCP_HOST_ALLOW_DRIFT=1` is set. | Must |
| FR-05 | Per-server quota enforcement: TAG's bridge MUST track invocation counts per server per quota window in the `mcp_host_quota_usage` table; when `quota_calls_used >= quota_calls_limit`, the bridge MUST return a structured MCP error `{"error": {"code": -32099, "message": "Quota exceeded: notion reached 200/200 calls in 1h window."}}` without forwarding the call to the container. | Must |
| FR-06 | Per-tool quota enforcement: if `--quota-calls` is applied at the tool level (via `tag mcp host quota set <server> <tool> --limit N`), the per-tool counter MUST be tracked independently and MUST take precedence over the per-server counter when both are configured. | Should |
| FR-07 | The Docker backend MUST NOT use the Python `docker` SDK. All Docker operations MUST be performed via `subprocess.run(["docker", ...])` to avoid the Docker SDK's transitive dependency chain. | Must |
| FR-08 | `tag mcp host logs <server> --follow` MUST stream container stdout and stderr to the terminal in real time using `subprocess.Popen` with `stdout=PIPE, stderr=STDOUT` and a read loop. The first line MUST appear within 500 ms of the container emitting it. | Must |
| FR-09 | `tag mcp host remove` MUST execute `docker stop` followed by `docker rm` (or the Modal equivalent) and MUST verify the container is no longer running before returning success. | Must |
| FR-10 | All hosted server activity (start, stop, tool call proxied, quota hit, hash mismatch, error) MUST be appended to `~/.tag/runtime/mcp-host-audit.jsonl` as newline-delimited JSON with fields: `timestamp`, `event`, `server_name`, `version`, `backend`, `tool_name` (nullable), `container_id`, `quota_used`, `quota_limit`, `details`. | Must |
| FR-11 | `tag mcp host list --json` MUST return valid JSON to stdout with no additional text; human-readable output MUST go to stdout in table form; errors MUST go to stderr. | Must |
| FR-12 | Environment variables passed via `--env KEY=VALUE` MUST be forwarded to the container or Modal function without being stored in the SQLite database. Secrets referenced via `--secret KEY=<keychain-ref>` MUST be fetched from the OS keychain at container start time using the `keyring` library and injected as environment variables; the keychain reference name MUST be stored in SQLite, not the secret value itself. | Must |
| FR-13 | The `--pull-policy if-absent` (default) MUST check for the image locally via `docker image inspect` before pulling. `always` MUST pull unconditionally. `never` MUST fail if the image is not already present with a clear error. | Should |
| FR-14 | The bridge process MUST handle the MCP stdio protocol framing (Content-Length headers + JSON-RPC body) correctly for both reads from the container and writes to Hermes. Malformed frames from the container MUST be logged to the audit file and forwarded as MCP error responses rather than silently dropped. | Must |
| FR-15 | The Modal backend MUST deploy the server as a `modal.Function` with the server's container image, forward stdin via `modal.Function.spawn()`, and multiplex stdout back over a local socket that the bridge reads. | Should |
| FR-16 | Quota windows MUST be rolling (not fixed calendar windows). A `1h` window starting at 14:23 expires at 15:23, not at 15:00. The `quota_window_reset_at` field MUST reflect the rolling expiry. | Must |
| FR-17 | `tag mcp host inspect --diff <other-version>` MUST produce a human-readable diff of tool names and schema hashes between two contract snapshots stored in SQLite for the same server name at two different versions. | Should |
| FR-18 | When the Docker daemon is not running, `tag mcp host add --backend docker` MUST detect this via `docker info` exit code and emit an actionable error: "Docker daemon is not running. Start Docker Desktop or run `sudo systemctl start docker`." | Must |
| FR-19 | When Modal credentials are not configured, `tag mcp host add --backend modal` MUST detect this and emit: "Modal credentials not found. Run `modal setup` to authenticate." | Must |
| FR-20 | `tag mcp host start` on an already-running server MUST be idempotent — return success with a message indicating the server is already running rather than starting a duplicate container. | Must |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Bridge process memory overhead | < 20 MB RSS per hosted server bridge (measured on macOS ARM64) |
| NFR-02 | Protocol round-trip latency overhead | The bridge MUST add < 5 ms median latency to each tool call (measured: client → bridge → container → bridge → client, excluding container execution time) |
| NFR-03 | Concurrent hosted servers | TAG MUST support at least 10 simultaneously running hosted servers without performance degradation in `tag mcp host list` |
| NFR-04 | Audit log write performance | Each audit log write MUST complete in < 1 ms (non-blocking append to JSONL file) |
| NFR-05 | SQLite write contention | All quota updates MUST use SQLite WAL mode with `BEGIN IMMEDIATE` to prevent write conflicts from concurrent agent sessions |
| NFR-06 | Container startup reproducibility | Given the same image digest, `tag mcp host start` MUST produce the same container environment on 10 consecutive runs (no random ports, no time-seeded state) |
| NFR-07 | Import isolation | `import tag.controller` MUST NOT import `modal`, `docker`, or `keyring` when `tag mcp host` has never been invoked. All imports MUST be deferred to the `cmd_mcp_host_*` handlers. |
| NFR-08 | CLI startup time | `tag --help` wall time MUST NOT increase by more than 5 ms due to this feature (enforced by import isolation) |
| NFR-09 | Error message quality | Every error from `tag mcp host` MUST include the server name, the attempted operation, the underlying error message, and an actionable next step. Generic "Operation failed" messages are not acceptable. |
| NFR-10 | Cross-platform | Docker backend MUST work on macOS (ARM64 and x86_64), Ubuntu 22.04+, and Debian 12+. Modal backend is platform-agnostic (cloud-side). |

---

## 10. Technical Design

### 10.1 New Files

```
src/tag/mcp_host.py          # Core hosted server lifecycle and bridge logic
src/tag/sandbox.py           # Extended with ContainerBackend.MCP_HOST (see PRD-028)
tests/test_mcp_host.py       # Unit and integration tests
docs/prd/PRD-079-cloud-hosted-tool-execution.md
```

### 10.2 SQLite DDL

All tables are added via a migration function `_migrate_prd_079_tables(conn)` called from the existing migration chain in `controller.py`.

```sql
-- Registered hosted MCP servers
CREATE TABLE IF NOT EXISTS mcp_hosted_servers (
    id              TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(8)))),
    name            TEXT NOT NULL UNIQUE,          -- e.g. 'notion'
    version         TEXT NOT NULL,                 -- concrete semver, never 'latest'
    image           TEXT NOT NULL,                 -- full image ref with digest
    backend         TEXT NOT NULL CHECK (backend IN ('docker', 'modal')),
    container_id    TEXT,                          -- Docker container ID or Modal function ID
    bridge_pid      INTEGER,                       -- PID of the local bridge process
    status          TEXT NOT NULL DEFAULT 'stopped'
                        CHECK (status IN ('starting', 'running', 'stopped', 'error')),
    profile         TEXT,                          -- associated TAG profile (nullable)
    quota_calls_limit   INTEGER,                   -- NULL = unlimited
    quota_window_secs   INTEGER,                   -- NULL = no window
    env_vars        TEXT,                          -- JSON dict of non-secret env vars
    keychain_refs   TEXT,                          -- JSON dict of key -> keychain service name
    registered_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_started_at TEXT,
    last_stopped_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_mhs_status ON mcp_hosted_servers(status);
CREATE INDEX IF NOT EXISTS idx_mhs_profile ON mcp_hosted_servers(profile);

-- Contract snapshots for version pinning
CREATE TABLE IF NOT EXISTS mcp_contract_snapshots (
    id              TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(8)))),
    server_name     TEXT NOT NULL,
    version         TEXT NOT NULL,
    contract_hash   TEXT NOT NULL,  -- sha256:<hex>
    tool_count      INTEGER NOT NULL,
    tools_json      TEXT NOT NULL,  -- JSON array of {name, description, schema_hash}
    captured_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (server_name, version)
);

CREATE INDEX IF NOT EXISTS idx_mcs_server ON mcp_contract_snapshots(server_name);

-- Quota usage (rolling window counters)
CREATE TABLE IF NOT EXISTS mcp_host_quota_usage (
    id              TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(8)))),
    server_name     TEXT NOT NULL,
    tool_name       TEXT,           -- NULL = server-level counter
    window_start_at TEXT NOT NULL,  -- ISO8601, start of current rolling window
    window_end_at   TEXT NOT NULL,  -- ISO8601, expiry of current rolling window
    calls_used      INTEGER NOT NULL DEFAULT 0,
    calls_limit     INTEGER,        -- NULL = unlimited
    UNIQUE (server_name, tool_name, window_start_at)
);

CREATE INDEX IF NOT EXISTS idx_mhqu_server ON mcp_host_quota_usage(server_name, tool_name, window_end_at);

-- Per-call audit trail (lightweight, append-only)
CREATE TABLE IF NOT EXISTS mcp_host_call_log (
    id              TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(8)))),
    server_name     TEXT NOT NULL,
    tool_name       TEXT NOT NULL,
    request_id      TEXT,           -- JSON-RPC request id
    quota_used_after INTEGER,
    quota_limit     INTEGER,
    quota_exceeded  INTEGER NOT NULL DEFAULT 0,  -- boolean
    latency_ms      REAL,
    error           TEXT,
    called_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_mhcl_server ON mcp_host_call_log(server_name, called_at);
```

### 10.3 Core Dataclasses

```python
# src/tag/mcp_host.py
from __future__ import annotations

import hashlib
import json
import os
import subprocess
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterator, Optional

BACKENDS = {"docker", "modal"}
QUOTA_WINDOWS = {"1h": 3600, "24h": 86400, "7d": 604800}
AUDIT_LOG = Path.home() / ".tag" / "runtime" / "mcp-host-audit.jsonl"

# Default image resolver: maps server name + version to a container image.
# Users can override with --image.
DEFAULT_IMAGE_MAP: dict[str, str] = {
    "notion":     "ghcr.io/modelcontextprotocol/notion-mcp",
    "playwright": "ghcr.io/modelcontextprotocol/playwright-mcp",
    "postgres":   "ghcr.io/modelcontextprotocol/postgres-mcp",
    "filesystem": "ghcr.io/modelcontextprotocol/filesystem-mcp",
    "github":     "ghcr.io/modelcontextprotocol/github-mcp",
    "slack":      "ghcr.io/modelcontextprotocol/slack-mcp",
}


@dataclass
class HostedServerConfig:
    name: str
    version: str                            # always concrete semver
    image: str                              # full image ref: registry/name:tag@sha256:...
    backend: str                            # 'docker' | 'modal'
    profile: Optional[str] = None
    quota_calls_limit: Optional[int] = None
    quota_window_secs: Optional[int] = None # None = no rolling window
    env_vars: dict[str, str] = field(default_factory=dict)
    keychain_refs: dict[str, str] = field(default_factory=dict)
    pull_policy: str = "if-absent"          # 'always' | 'if-absent' | 'never'
    no_pin: bool = False


@dataclass
class ContractSnapshot:
    server_name: str
    version: str
    contract_hash: str                      # 'sha256:<hex>'
    tool_count: int
    tools: list[dict[str, str]]             # [{name, description, schema_hash}]
    captured_at: str


@dataclass
class QuotaState:
    server_name: str
    tool_name: Optional[str]               # None = server-level
    window_start_at: str
    window_end_at: str
    calls_used: int
    calls_limit: Optional[int]

    @property
    def is_exceeded(self) -> bool:
        return self.calls_limit is not None and self.calls_used >= self.calls_limit

    @property
    def utilization_pct(self) -> float:
        if self.calls_limit is None or self.calls_limit == 0:
            return 0.0
        return (self.calls_used / self.calls_limit) * 100.0


@dataclass
class BridgeProcess:
    """Manages the stdio proxy between Hermes and a hosted MCP container."""
    server_name: str
    container_id: str
    backend: str
    _proc: Optional[subprocess.Popen] = field(default=None, repr=False)
    _lock: threading.Lock = field(default_factory=threading.Lock, repr=False)
```

### 10.4 Contract Hash Algorithm

The hash must be deterministic across Python versions, OS platforms, and JSON library implementations:

```python
def compute_contract_hash(tools: list[dict[str, Any]]) -> tuple[str, list[dict[str, str]]]:
    """
    Compute a SHA-256 content hash of the tool interface.

    Input: raw MCP tools array from initialize response.
    Output: (contract_hash, per_tool_records) where contract_hash is 'sha256:<hex>'
    and per_tool_records is the list stored in mcp_contract_snapshots.tools_json.

    Normalization rules:
    1. Only include keys: name, description, inputSchema (ignore all others).
    2. Sort tools list by name ascending.
    3. Serialize with json.dumps(sort_keys=True, separators=(',', ':')) — no whitespace.
    4. Encode as UTF-8 before hashing.
    """
    normalized = []
    per_tool = []
    for tool in sorted(tools, key=lambda t: t["name"]):
        canonical = {
            "name": tool["name"],
            "description": tool.get("description", ""),
            "inputSchema": tool.get("inputSchema", {}),
        }
        canonical_bytes = json.dumps(canonical, sort_keys=True, separators=(",", ":")).encode("utf-8")
        tool_hash = "sha256:" + hashlib.sha256(canonical_bytes).hexdigest()
        normalized.append(canonical)
        per_tool.append({"name": tool["name"], "description": tool.get("description", ""), "schema_hash": tool_hash})

    full_payload = json.dumps(normalized, sort_keys=True, separators=(",", ":")).encode("utf-8")
    contract_hash = "sha256:" + hashlib.sha256(full_payload).hexdigest()
    return contract_hash, per_tool
```

### 10.5 Docker Backend: Container Lifecycle

```python
def docker_start(config: HostedServerConfig, env_secrets: dict[str, str]) -> str:
    """
    Pull (per pull_policy), start, and return container ID.
    All operations use subprocess.run(["docker", ...]) — no Python Docker SDK.
    """
    # 1. Resolve or verify image
    if config.pull_policy == "if-absent":
        result = subprocess.run(
            ["docker", "image", "inspect", config.image],
            capture_output=True, text=True
        )
        if result.returncode != 0:
            _docker_pull(config.image)
    elif config.pull_policy == "always":
        _docker_pull(config.image)
    elif config.pull_policy == "never":
        result = subprocess.run(
            ["docker", "image", "inspect", config.image],
            capture_output=True, text=True
        )
        if result.returncode != 0:
            raise RuntimeError(
                f"Image {config.image!r} not found locally and --pull-policy=never was set. "
                f"Run: docker pull {config.image}"
            )

    # 2. Build docker run command
    container_name = f"tag-mcp-{config.name}-{os.urandom(4).hex()}"
    cmd = [
        "docker", "run",
        "--detach",
        "--name", container_name,
        "--label", f"tag.mcp.server={config.name}",
        "--label", f"tag.mcp.version={config.version}",
        "--rm",                             # auto-remove on stop
        "--network", "host",               # needed for Hermes on loopback
        "--memory", "512m",
        "--cpus", "2.0",
        "--pids-limit", "256",
    ]
    # Inject non-secret env vars
    for k, v in config.env_vars.items():
        cmd += ["-e", f"{k}={v}"]
    # Inject secrets (resolved at call time, not stored)
    for k, v in env_secrets.items():
        cmd += ["-e", f"{k}={v}"]
    cmd.append(config.image)

    result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    if result.returncode != 0:
        raise RuntimeError(
            f"docker run failed for {config.name}:\n{result.stderr.strip()}"
        )
    return result.stdout.strip()  # container ID


def docker_stop(container_id: str, server_name: str) -> None:
    subprocess.run(["docker", "stop", "--time", "10", container_id],
                   capture_output=True, timeout=20)
    subprocess.run(["docker", "rm", "--force", container_id],
                   capture_output=True, timeout=10)


def docker_logs(container_id: str, *, tail: int = 50, follow: bool = False) -> Iterator[str]:
    cmd = ["docker", "logs", "--timestamps"]
    if follow:
        cmd.append("--follow")
    cmd += ["--tail", str(tail), container_id]
    with subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True) as proc:
        assert proc.stdout is not None
        for line in proc.stdout:
            yield line.rstrip("\n")
```

### 10.6 Stdio Bridge Protocol

The bridge sits between Hermes (which speaks MCP over stdio) and the container (which also speaks MCP over stdio via `docker exec`). The bridge:

1. Opens a `docker exec -i <container_id> <mcp-entrypoint>` subprocess.
2. Forwards raw bytes from Hermes's stdin to the exec process's stdin.
3. Intercepts each complete JSON-RPC message from the exec process's stdout.
4. Before forwarding a `tools/call` response back to Hermes, increments the quota counter and checks the limit.
5. Forwards the (possibly replaced) message to Hermes's stdout.

The MCP stdio framing is: `Content-Length: <N>\r\n\r\n<N bytes of JSON>`. The bridge MUST buffer input correctly and MUST NOT split or merge frames.

```python
class McpStdioBridge:
    """
    Transparent proxy between Hermes and a containerized MCP server.
    Intercepts tools/call requests to enforce quota before forwarding.
    """

    def __init__(self, config: HostedServerConfig, container_id: str, db_path: Path):
        self.config = config
        self.container_id = container_id
        self.db_path = db_path
        self._quota_lock = threading.Lock()

    def _read_frame(self, stream) -> Optional[bytes]:
        """Read one Content-Length-framed JSON-RPC message."""
        header = b""
        while not header.endswith(b"\r\n\r\n"):
            byte = stream.read(1)
            if not byte:
                return None
            header += byte
        for line in header.split(b"\r\n"):
            if line.lower().startswith(b"content-length:"):
                length = int(line.split(b":", 1)[1].strip())
                return stream.read(length)
        return None

    def _write_frame(self, stream, payload: bytes) -> None:
        frame = f"Content-Length: {len(payload)}\r\n\r\n".encode() + payload
        stream.write(frame)
        stream.flush()

    def _check_and_increment_quota(self, tool_name: str) -> tuple[bool, int, Optional[int]]:
        """
        Returns (allowed, calls_used_after, calls_limit).
        Thread-safe via _quota_lock.
        """
        import sqlite3
        with self._quota_lock:
            conn = sqlite3.connect(str(self.db_path), timeout=5)
            conn.execute("PRAGMA journal_mode=WAL")
            try:
                conn.execute("BEGIN IMMEDIATE")
                now_iso = _utc_now()
                # Find or create rolling window row
                row = conn.execute(
                    """
                    SELECT id, calls_used, calls_limit, window_end_at
                    FROM mcp_host_quota_usage
                    WHERE server_name = ? AND tool_name IS NULL
                      AND window_end_at > ?
                    ORDER BY window_start_at DESC LIMIT 1
                    """,
                    (self.config.name, now_iso)
                ).fetchone()

                if row is None:
                    # Start new rolling window
                    window_secs = self.config.quota_window_secs or 3600
                    window_end = _add_seconds(now_iso, window_secs)
                    conn.execute(
                        """
                        INSERT INTO mcp_host_quota_usage
                          (server_name, tool_name, window_start_at, window_end_at,
                           calls_used, calls_limit)
                        VALUES (?, NULL, ?, ?, 1, ?)
                        """,
                        (self.config.name, now_iso, window_end, self.config.quota_calls_limit)
                    )
                    conn.commit()
                    return True, 1, self.config.quota_calls_limit

                row_id, calls_used, calls_limit, _ = row
                if calls_limit is not None and calls_used >= calls_limit:
                    conn.commit()
                    return False, calls_used, calls_limit

                new_count = calls_used + 1
                conn.execute(
                    "UPDATE mcp_host_quota_usage SET calls_used = ? WHERE id = ?",
                    (new_count, row_id)
                )
                conn.commit()
                return True, new_count, calls_limit
            finally:
                conn.close()
```

### 10.7 Version Resolution from MCP Registry

```python
def resolve_version_from_registry(server_name: str, version: str) -> str:
    """
    If version == 'latest', query the MCP registry and return the concrete tag.
    If version is already a semver string, validate it is published and return as-is.
    Raises ValueError with an actionable message on failure.
    """
    import httpx  # lazy import — only when mcp host add is invoked

    REGISTRY_BASE = "https://registry.modelcontextprotocol.io"
    # Map short names to registry reverse-domain names
    name_map = {
        "notion": "io.modelcontextprotocol/notion",
        "playwright": "io.modelcontextprotocol/playwright",
        "postgres": "io.modelcontextprotocol/postgres",
    }
    registry_name = name_map.get(server_name, server_name)
    encoded = registry_name.replace("/", "%2F")

    try:
        with httpx.Client(timeout=10) as client:
            resp = client.get(
                f"{REGISTRY_BASE}/v0.1/servers/{encoded}/versions",
                params={"version": version} if version != "latest" else {}
            )
            resp.raise_for_status()
            data = resp.json()
    except httpx.HTTPError as exc:
        raise ValueError(
            f"MCP registry lookup failed for {server_name!r}: {exc}\n"
            f"Check your network connection or specify --image explicitly."
        ) from exc

    servers = data.get("servers", [])
    if not servers:
        raise ValueError(
            f"No versions found for {server_name!r} in MCP registry.\n"
            f"Use --image to specify a container image directly."
        )

    if version == "latest":
        # Find the entry with isLatest=True in _meta
        for entry in servers:
            if entry.get("_meta", {}).get("isLatest", False):
                return entry["version"]
        # Fallback: return the first entry (registry sorts newest-first)
        return servers[0]["version"]
    else:
        for entry in servers:
            if entry["version"] == version:
                return version
        raise ValueError(
            f"Version {version!r} not found for {server_name!r} in MCP registry.\n"
            f"Available: {[e['version'] for e in servers[:5]]}"
        )
```

### 10.8 Integration Points

**controller.py additions:**

```python
def cmd_mcp_host_add(cfg, args):
    """tag mcp host add <name> [flags]"""
    from tag.mcp_host import (
        HostedServerConfig, resolve_version_from_registry,
        resolve_image, docker_start, capture_contract_snapshot,
        register_hosted_server, write_audit_event,
    )
    # ... implementation delegates to mcp_host.py functions

def cmd_mcp_host_list(cfg, args):
    """tag mcp host list [--json]"""
    ...

def cmd_mcp_host_logs(cfg, args):
    """tag mcp host logs <name> [--tail N] [--follow]"""
    ...

def cmd_mcp_host_remove(cfg, args):
    """tag mcp host remove <name>"""
    ...

def cmd_mcp_host_inspect(cfg, args):
    """tag mcp host inspect <name> [--json] [--diff <other-version>]"""
    ...

def cmd_mcp_host_start(cfg, args):
    """tag mcp host start <name>"""
    ...

def cmd_mcp_host_stop(cfg, args):
    """tag mcp host stop <name>"""
    ...
```

**Hermes session startup hook:**

In the existing `run_hermes()` function in `controller.py`, after loading profile config and before spawning the Hermes process, add:

```python
# PRD-079: verify contract hashes for all hosted MCP servers in this profile
if profile_config.get("mcp", {}).get("hosted_servers"):
    from tag.mcp_host import verify_all_contract_hashes
    violations = verify_all_contract_hashes(
        profile_config["mcp"]["hosted_servers"],
        conn,
        allow_drift=os.environ.get("MCP_HOST_ALLOW_DRIFT") == "1",
    )
    if violations:
        for v in violations:
            print(f"ERROR: Contract hash mismatch for {v.server_name}@{v.version}", file=sys.stderr)
            print(f"  Expected: {v.expected_hash}", file=sys.stderr)
            print(f"  Got:      {v.actual_hash}", file=sys.stderr)
        sys.exit(1)
```

**Audit log writer:**

```python
def write_audit_event(event: str, **kwargs) -> None:
    """Append a structured event to the audit JSONL log. Non-blocking."""
    AUDIT_LOG.parent.mkdir(parents=True, exist_ok=True)
    record = {"timestamp": _utc_now(), "event": event, **kwargs}
    try:
        with open(AUDIT_LOG, "a", encoding="utf-8") as f:
            f.write(json.dumps(record, separators=(",", ":")) + "\n")
    except OSError:
        pass  # Never crash the agent session due to audit log failures
```

---

## 11. Security Considerations

1. **Secret handling**: Secrets passed via `--secret KEY=<keychain-ref>` are fetched from the OS keychain using `keyring.get_password(service, account)` at container start time. The resolved secret value exists only in the process environment of the container start subprocess and is never written to disk, SQLite, or the audit log. The keychain reference name (not the value) is stored in `mcp_hosted_servers.keychain_refs`.

2. **Image provenance**: TAG does not verify image signatures or digests by default. Users who require image provenance SHOULD specify images with `@sha256:<digest>` notation (e.g., `--image ghcr.io/mcp/notion@sha256:abc123`). A future feature may add `cosign verify` as an optional gate.

3. **Container isolation scope**: The Docker backend provides filesystem and process isolation via Linux namespaces, but shares the host network stack when `--network host` is used (required for Hermes on loopback). Users who want full network isolation SHOULD use `--network none` combined with a volume-mounted config; this is a per-server opt-in, not the default.

4. **Audit log tampering**: The audit log at `~/.tag/runtime/mcp-host-audit.jsonl` is a plain append-only file with no cryptographic integrity protection. Privileged local processes can modify it. For regulatory audit trails, the JSONL file SHOULD be shipped to an immutable log store (e.g., CloudWatch Logs, Datadog) using the existing OTLP export path from PRD-013.

5. **Quota bypass via container restart**: A caller that has direct Docker access can restart the hosted container to reset the quota state. This is acceptable because quota enforcement targets accidental resource exhaustion in agent loops, not adversarial abuse. The audit log records all start/stop events.

6. **Bridge process privilege**: The bridge process runs as the same Unix user as TAG. It does not acquire elevated privileges. The `docker` CLI invocations require the user to be in the `docker` group (or equivalent), which is the standard Docker setup requirement. TAG does not use `sudo docker`.

7. **Environment variable leakage**: Non-secret env vars passed via `--env` are stored in `mcp_hosted_servers.env_vars` as plaintext JSON. Users MUST NOT pass secrets via `--env`. TAG SHOULD detect common secret patterns (names containing `TOKEN`, `SECRET`, `KEY`, `PASSWORD`) in `--env` values and emit a warning recommending `--secret` instead, leveraging the pattern detection already in `security.py` (PRD-034).

8. **Contract hash collision resistance**: SHA-256 provides 256-bit collision resistance. An adversary who can modify a container image to produce an identical contract hash while changing tool behavior would need a SHA-256 preimage attack. This is currently considered computationally infeasible.

9. **Modal backend trust**: The Modal backend executes containers on Modal's cloud infrastructure. The security boundary is Modal's sandbox, not a local Docker container. Users running sensitive workloads SHOULD evaluate Modal's security posture independently. Secrets injected via `--secret` are transmitted to Modal as encrypted environment variables using Modal's secure parameter store.

10. **Orphaned containers on crash**: If the TAG process is killed with SIGKILL while a container is running, the container continues running until the `--rm` Docker flag causes it to be removed when the exec process exits. TAG SHOULD run a cleanup check at startup (`tag mcp host list` equivalent) to detect containers with `tag.mcp.server` labels that are no longer referenced in SQLite and stop them.

---

## 12. Testing Strategy

### 12.1 Unit Tests

```
tests/test_mcp_host.py::test_compute_contract_hash_deterministic
    - Same tool list in different order → same hash
    - Extra fields in tool dict → ignored (only name/description/inputSchema hashed)
    - Empty description → treated as empty string not omitted

tests/test_mcp_host.py::test_compute_contract_hash_sensitivity
    - Change one tool name → different hash
    - Change one description → different hash
    - Add one tool → different hash
    - Change inputSchema field type → different hash

tests/test_mcp_host.py::test_quota_enforcement_under_limit
    - 99 calls with limit=100 → all allowed, calls_used=99

tests/test_mcp_host.py::test_quota_enforcement_at_limit
    - 100th call with limit=100 → allowed (boundary), calls_used=100

tests/test_mcp_host.py::test_quota_enforcement_over_limit
    - 101st call with limit=100 → blocked, returns MCP error JSON

tests/test_mcp_host.py::test_quota_rolling_window_reset
    - Advance mock clock past window_end_at → new window starts with calls_used=0

tests/test_mcp_host.py::test_resolve_version_latest_selects_is_latest
    - Mock registry response with isLatest=True on entry → returns that version

tests/test_mcp_host.py::test_resolve_version_not_found
    - Version not in registry response → raises ValueError with available list

tests/test_mcp_host.py::test_secret_not_stored_in_sqlite
    - After docker_start with --secret, sqlite row has keychain_ref not secret value

tests/test_mcp_host.py::test_audit_log_write_on_os_error
    - AUDIT_LOG path unwriteable → write_audit_event does not raise

tests/test_mcp_host.py::test_import_isolation
    - `import tag.controller` does not import modal, docker, or keyring
    - Verified via sys.modules assertion

tests/test_mcp_host.py::test_mcp_stdio_frame_read_write_roundtrip
    - Content-Length framing: encode and decode roundtrip is lossless

tests/test_mcp_host.py::test_contract_hash_mismatch_raises
    - verify_all_contract_hashes with mismatched hash → returns ContractViolation
```

### 12.2 Integration Tests

Integration tests require Docker daemon. Marked with `@pytest.mark.integration` and skipped when `DOCKER_AVAILABLE != "1"`.

```
tests/test_mcp_host_integration.py::test_docker_start_stop_cycle
    - Start a lightweight MCP container (e.g., mcp-server-echo image)
    - Verify container is listed by docker ps
    - Stop and verify container is removed

tests/test_mcp_host_integration.py::test_contract_snapshot_captured_on_add
    - tag mcp host add <test-server> → contract snapshot row exists in SQLite

tests/test_mcp_host_integration.py::test_contract_mismatch_blocks_session
    - Manually corrupt snapshot hash in SQLite
    - Run verify_all_contract_hashes → returns violation
    - Exit code check confirms non-zero

tests/test_mcp_host_integration.py::test_logs_streaming
    - Start container, request --tail 5 --follow
    - Send a tool call through the bridge
    - Assert log line appears within 1 s

tests/test_mcp_host_integration.py::test_quota_blocks_call_via_bridge
    - Configure quota_calls_limit=3
    - Make 4 tool calls through bridge
    - 4th call receives MCP error without hitting container
```

### 12.3 Performance Tests

```
tests/test_mcp_host_perf.py::test_bridge_latency_overhead
    - 100 tool call roundtrips through bridge vs. direct container exec
    - Assert: median overhead < 5 ms, p99 overhead < 20 ms

tests/test_mcp_host_perf.py::test_quota_check_write_latency
    - 1000 sequential _check_and_increment_quota calls
    - Assert: median < 1 ms per call (SQLite WAL, single writer)

tests/test_mcp_host_perf.py::test_audit_log_write_throughput
    - 10000 sequential write_audit_event calls
    - Assert: total wall time < 5 s (< 0.5 ms/write)
```

---

## 13. Acceptance Criteria

| ID | Criterion | Test Method |
|----|-----------|-------------|
| AC-01 | `tag mcp host add notion --version 1.2.3 --backend docker` exits 0 and writes a row to `mcp_hosted_servers` with `version='1.2.3'` (never 'latest'). | Integration test + SQL assertion |
| AC-02 | `tag mcp host add notion --version latest --backend docker` resolves to a concrete semver and writes that concrete tag to SQLite. | Integration test + SQL assertion |
| AC-03 | After `add`, `mcp_contract_snapshots` contains one row for `(server_name='notion', version='1.2.3')` with a non-null `contract_hash`. | SQL assertion |
| AC-04 | After manually updating `contract_hash` in SQLite to a wrong value, running `verify_all_contract_hashes` returns a `ContractViolation` and the session startup exits 1. | Integration test |
| AC-05 | `tag mcp host add notion --version 1.2.3 --backend docker --quota-calls 200 --quota-window 1h` stores `quota_calls_limit=200` and `quota_window_secs=3600` in SQLite. | SQL assertion |
| AC-06 | After 200 tool calls through the bridge, the 201st returns a JSON-RPC error with code `-32099` and `calls_used >= calls_limit` in the audit log. | Integration test |
| AC-07 | `tag mcp host list --json` emits valid JSON array to stdout; stderr is empty. | `json.loads(stdout)` assertion |
| AC-08 | `tag mcp host logs notion --tail 20` emits 20 or fewer lines of container log to stdout within 2 s. | Timing assertion |
| AC-09 | `tag mcp host remove notion` exits 0 and the container is no longer present in `docker ps -a` output. | Integration test + `docker ps` check |
| AC-10 | `import tag.controller` does not import `modal`, `docker`, or `keyring` in a fresh Python process. | `sys.modules` unit test |
| AC-11 | `--secret NOTION_TOKEN=my-keychain-ref` stores `{"NOTION_TOKEN": "my-keychain-ref"}` in `keychain_refs` column and the actual token is never written to disk or SQLite. | SQL assertion + filesystem scan |
| AC-12 | Every `tools/call` proxied by the bridge generates an `mcp_host_call_log` row with `latency_ms` populated. | SQL count assertion |
| AC-13 | The audit JSONL file gains one entry per bridge event (start, stop, call, quota_hit). | File line count assertion |
| AC-14 | `tag mcp host start notion` on an already-running server exits 0 and prints "notion is already running" rather than starting a second container. | Integration test + `docker ps` count |
| AC-15 | When `DOCKER_AVAILABLE` is false (no docker daemon), `tag mcp host add --backend docker` exits non-zero with an actionable error message referencing `docker info`. | Unit test with mocked subprocess |
| AC-16 | `tag mcp host inspect notion --diff 1.2.4` shows a diff of tool name / schema hash changes between the two stored snapshots. | Integration test with two pre-seeded snapshot rows |
| AC-17 | `--env KEY=VALUE` pairs appear as environment variables inside the running container (verified via `docker exec <id> env`). | Integration test |
| AC-18 | `--env SECRET_TOKEN=abc` (name matching secret pattern) emits a warning to stderr recommending `--secret` instead. | Unit test checking stderr output |

---

## 14. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `docker` CLI | External tool | >= 24.0 | Must be in `$PATH`. Python `docker` SDK is NOT used. |
| `modal` Python SDK | Optional Python dep | >= 0.64 | Lazy-imported only when `--backend modal` is used. Added to `pyproject.toml` as optional extra `[modal]`. |
| `keyring` | Optional Python dep | >= 24.0 | Lazy-imported only when `--secret` flag is used. Added to `pyproject.toml` as optional extra `[secrets]`. |
| `httpx` | Python dep | already in project | Used for MCP registry version resolution. Already present. |
| PRD-028 (sandbox) | Internal | current | `mcp_host.py` reuses `sandbox.py`'s `_utc_now()`, `AUDIT_LOG` path conventions, and the `open_db()` migration pattern from `controller.py`. |
| PRD-014 (MCP registry) | Internal | current | Version resolution queries the same MCP registry endpoints defined in PRD-014. |
| PRD-013 (tracing) | Internal | current | Hosted server events are emitted as OTel spans using `tracing.py` if `OTEL_EXPORTER_OTLP_ENDPOINT` is set. |
| PRD-034 (secret scanning) | Internal | current | Pattern detection for `--env` secret warnings reuses `security.py` patterns. |
| MCP Registry API | External service | v0.1 | `https://registry.modelcontextprotocol.io` — used for version resolution. Handle 500s gracefully; fall back to user-specified `--image`. |

---

## 15. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|-------------------|
| OQ-01 | Should `tag mcp host add` auto-add the server to the active profile's MCP config, or require an explicit `tag profile edit --add-mcp <server>` step? Auto-add is more ergonomic but could surprise users running multiple profiles. | Feature owner | Before implementation kickoff |
| OQ-02 | The `--network host` Docker flag (required for Hermes on loopback) defeats container network isolation. Is there a better networking model — e.g., a named Docker bridge with a local port mapping — that preserves isolation without breaking Hermes connectivity? | Infrastructure | Phase 1 design review |
| OQ-03 | Should the contract snapshot be captured from the container's image metadata (at pull time, before start) or from a live `initialize` handshake (at runtime)? Image metadata is faster but may not reflect runtime-injected tool registrations. | Protocol design | Phase 1 |
| OQ-04 | The MCP registry API is marked preview-grade and may reset or return 500s. What is the fallback UX when version resolution fails — error out, or prompt the user to specify `--image` directly? | UX | Phase 1 |
| OQ-05 | Modal functions have a cold-start latency of 5–15 seconds for large images. Should TAG pre-warm Modal functions (keep-alive pings) or accept cold starts with a progress indicator? | Infrastructure | Phase 2 |
| OQ-06 | Should per-tool quotas (`tag mcp host quota set <server> <tool> --limit N`) be included in v1 or deferred to v2? Per-server quotas cover the primary use case; per-tool quotas add SQL complexity. | Feature owner | Phase 1 kickoff |
| OQ-07 | The existing `sandbox.py` (PRD-028) also manages Docker containers. Should `mcp_host.py` reuse `sandbox.py`'s container lifecycle functions, or keep them separate to avoid coupling? The use cases are different enough (long-lived MCP servers vs. ephemeral command runners) that coupling may be premature. | Architecture | Phase 1 design review |
| OQ-08 | When a hosted server's quota window expires mid-session, should TAG automatically start a new window (allowing more calls) or require `tag mcp host quota reset` to unblock? Auto-reset is more user-friendly but makes quota a soft limit. | Product | Before implementation |
| OQ-09 | Should `tag mcp host` support Streamable-HTTP transport (not just stdio bridging) so hosted servers can be shared across multiple concurrent Hermes instances without requiring per-instance container starts? | Architecture | Phase 3 planning |
| OQ-10 | Is `ghcr.io/modelcontextprotocol/<name>-mcp` a stable image naming convention, or should TAG maintain its own image resolution registry? The MCP registry's `packages` field includes Docker image refs — should resolution always go through the registry API? | Infrastructure | Phase 1 |

---

## 16. Complexity and Timeline

**Total estimate:** L (2-4 weeks, 1 engineer)

### Phase 1 — Core Docker Backend (Days 1–8)

| Day | Deliverable |
|-----|-------------|
| 1-2 | SQLite DDL migration (`_migrate_prd_079_tables`), `HostedServerConfig` / `ContractSnapshot` / `QuotaState` dataclasses, `compute_contract_hash()` with full unit test coverage |
| 3-4 | Docker backend: `docker_start`, `docker_stop`, `docker_logs`, `_docker_pull` — all via subprocess; `cmd_mcp_host_add` and `cmd_mcp_host_remove` in controller.py; audit log writer |
| 5-6 | `McpStdioBridge` — Content-Length frame parser, request forwarder, quota intercept logic; `cmd_mcp_host_list --json`; `cmd_mcp_host_logs --follow` |
| 7 | Contract snapshot capture and `verify_all_contract_hashes`; integration into `run_hermes()` session startup hook |
| 8 | `cmd_mcp_host_inspect --diff`; `--secret` flag with keyring integration; `--env` secret pattern warning from `security.py` |

### Phase 2 — Quota System and Polish (Days 9–13)

| Day | Deliverable |
|-----|-------------|
| 9-10 | Rolling window quota implementation with `BEGIN IMMEDIATE` SQLite writes; quota warning at 80%; `tag mcp host quota reset`; per-tool quota (`FR-06`) |
| 11 | `cmd_mcp_host_start` / `cmd_mcp_host_stop` idempotent lifecycle; orphaned container cleanup at startup; `--pull-policy` all three modes |
| 12 | Version resolution from MCP registry with graceful 500 handling; `--version latest` concrete resolution; `--dry-run` mode |
| 13 | Integration test suite (`test_mcp_host_integration.py`); performance test suite (`test_mcp_host_perf.py`); bridge latency benchmarks |

### Phase 3 — Modal Backend and Hardening (Days 14–18)

| Day | Deliverable |
|-----|-------------|
| 14-15 | Modal backend: `modal.Function` deployment, stdin/stdout multiplexing over local socket; `cmd_mcp_host_add --backend modal` |
| 16 | OTel span emission for hosted server events (PRD-013 integration); `MCP_HOST_FAIL_ON_HASH_DRIFT` CI environment variable |
| 17 | End-to-end test: `tag mcp host add playwright --backend docker → tag run --profile browser → verify tool call reaches container and returns result` |
| 18 | Documentation update in `docs/prd/INDEX.md`; `tag doctor` check for Docker daemon and Modal credentials when hosted servers are registered |

---

*PRD-079 authored for TAG CLI — GitHub issue #346*
