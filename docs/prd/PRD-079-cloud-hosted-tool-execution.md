# PRD-079: Cloud-Hosted Tool Execution with Version Pinning (`tag mcp host`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** MCP Ecosystem & Tool Connectivity
**Affects:** `internal/mcp/host` (new package), `internal/cli/cmd_mcp_host.go` (new CLI handlers), `internal/store` (new migration + tables), `internal/runtime` (session startup hook)
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
| FR-07 | The Docker backend MUST NOT use the `docker/docker` moby client SDK. All Docker operations MUST be performed by invoking the `docker` CLI via `os/exec.CommandContext`, keeping the dependency surface minimal and matching the subprocess-only approach from the original design. | Must |
| FR-08 | `tag mcp host logs <server> --follow` MUST stream container stdout and stderr to the terminal in real time. The implementation MUST use `os/exec.CommandContext` with `cmd.StdoutPipe()` and a goroutine read loop. The first line MUST appear within 500 ms of the container emitting it. | Must |
| FR-09 | `tag mcp host remove` MUST execute `docker stop` followed by `docker rm` (or the Modal equivalent) and MUST verify the container is no longer running before returning success. | Must |
| FR-10 | All hosted server activity (start, stop, tool call proxied, quota hit, hash mismatch, error) MUST be appended to `~/.tag/runtime/mcp-host-audit.jsonl` as newline-delimited JSON with fields: `timestamp`, `event`, `server_name`, `version`, `backend`, `tool_name` (nullable), `container_id`, `quota_used`, `quota_limit`, `details`. | Must |
| FR-11 | `tag mcp host list --json` MUST return valid JSON to stdout with no additional text; human-readable output MUST go to stdout in table form; errors MUST go to stderr. | Must |
| FR-12 | Environment variables passed via `--env KEY=VALUE` MUST be forwarded to the container or Modal function without being stored in the SQLite database. Secrets referenced via `--secret KEY=<keychain-ref>` MUST be fetched from the OS keychain at container start time using the `keyring` library and injected as environment variables; the keychain reference name MUST be stored in SQLite, not the secret value itself. | Must |
| FR-13 | The `--pull-policy if-absent` (default) MUST check for the image locally via `docker image inspect` before pulling. `always` MUST pull unconditionally. `never` MUST fail if the image is not already present with a clear error. | Should |
| FR-14 | The bridge process MUST handle the MCP stdio protocol framing (Content-Length headers + JSON-RPC body) correctly for both reads from the container and writes to Hermes. Malformed frames from the container MUST be logged to the audit file and forwarded as MCP error responses rather than silently dropped. | Must |
| FR-15 | The Modal backend MUST interact with Modal's REST API using `net/http` + `cenkalti/backoff/v4` (no Modal Python SDK). It MUST create a Modal function run carrying the server's container image, open a bidirectional stream for stdin/stdout forwarding, and multiplex output back over a local channel that the bridge consumes. | Should |
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
| NFR-07 | Init isolation | The `internal/mcp/host` package MUST NOT register any `init()` side effects that contact Docker, Modal, or the OS keyring. All I/O MUST be deferred to explicit `cmd_mcp_host_*` handler invocations. Running `tag --help` MUST NOT trigger any network or subprocess calls. |
| NFR-08 | CLI startup time | `tag --help` wall time MUST NOT increase by more than 5 ms due to this feature (enforced by import isolation) |
| NFR-09 | Error message quality | Every error from `tag mcp host` MUST include the server name, the attempted operation, the underlying error message, and an actionable next step. Generic "Operation failed" messages are not acceptable. |
| NFR-10 | Cross-platform | Docker backend MUST work on macOS (ARM64 and x86_64), Ubuntu 22.04+, and Debian 12+. Modal backend is platform-agnostic (cloud-side). |

---

## 10. Technical Design

### 10.1 New Packages / Files

```
internal/mcp/host/            # Package mcphost: lifecycle, bridge, quota, contract, audit
  host.go                     #   Add/Remove/Start/Stop orchestration
  types.go                    #   HostedServerConfig, ContractSnapshot, QuotaState structs
  contract.go                 #   ComputeContractHash, VerifyAllContractHashes
  docker.go                   #   DockerStart, DockerStop, DockerLogs (os/exec only)
  modal.go                    #   Modal REST API client (net/http + backoff)
  bridge.go                   #   McpStdioBridge: Content-Length framer + quota intercept
  quota.go                    #   Rolling-window quota enforcement (modernc.org/sqlite WAL)
  audit.go                    #   WriteAuditEvent (best-effort JSONL append)
  registry.go                 #   ResolveVersion (net/http + cenkalti/backoff/v4)
internal/store/migrations/    #   PRD-079 migration applied via modernc.org/sqlite
  prd079_mcp_host.go          #   migrate079Tables(db *sql.DB) error
internal/cli/cmd_mcp_host.go  #   CLI handlers wired via go-chi/chi v5 + huma v2
docs/prd/PRD-079-cloud-hosted-tool-execution.md
```

### 10.2 SQLite DDL

All tables are added via `migrate079Tables(db *sql.DB) error` called from the existing migration chain in `internal/store`.  The DDL is language-agnostic SQL executed through `modernc.org/sqlite` (pure-Go, CGO_ENABLED=0, FTS5 built-in); the single-writer discipline is enforced with `gofrs/flock` + `PRAGMA journal_mode=WAL`.

```sql
-- Registered hosted MCP servers
CREATE TABLE IF NOT EXISTS mcp_hosted_servers (
    id              TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(8)))),
    name            TEXT NOT NULL UNIQUE,          -- e.g. 'notion'
    version         TEXT NOT NULL,                 -- concrete semver, never 'latest'
    image           TEXT NOT NULL,                 -- full image ref with digest
    backend         TEXT NOT NULL CHECK (backend IN ('docker', 'modal')),
    container_id    TEXT,                          -- Docker container ID or Modal run ID
    bridge_goroutine_id TEXT,                      -- opaque ID of the bridge goroutine
    status          TEXT NOT NULL DEFAULT 'stopped'
                        CHECK (status IN ('starting', 'running', 'stopped', 'error')),
    profile         TEXT,                          -- associated TAG profile (nullable)
    quota_calls_limit   INTEGER,                   -- NULL = unlimited
    quota_window_secs   INTEGER,                   -- NULL = no window
    env_vars        TEXT,                          -- JSON object of non-secret env vars
    keychain_refs   TEXT,                          -- JSON object of key -> keychain service name
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

### 10.3 Core Go Structs

Pydantic dataclasses are replaced by plain Go structs tagged for `encoding/json`.  Schema generation for huma v2 uses `invopop/jsonschema`.

```go
// internal/mcp/host/types.go
package mcphost

import "time"

var defaultImageMap = map[string]string{
    "notion":     "ghcr.io/modelcontextprotocol/notion-mcp",
    "playwright": "ghcr.io/modelcontextprotocol/playwright-mcp",
    "postgres":   "ghcr.io/modelcontextprotocol/postgres-mcp",
    "filesystem": "ghcr.io/modelcontextprotocol/filesystem-mcp",
    "github":     "ghcr.io/modelcontextprotocol/github-mcp",
    "slack":      "ghcr.io/modelcontextprotocol/slack-mcp",
}

// HostedServerConfig is the persisted and runtime configuration for one hosted MCP server.
type HostedServerConfig struct {
    Name            string            `json:"name"`
    Version         string            `json:"version"`          // always concrete semver
    Image           string            `json:"image"`            // full image ref with digest
    Backend         string            `json:"backend"`          // "docker" | "modal"
    Profile         string            `json:"profile,omitempty"`
    QuotaCallsLimit *int64            `json:"quota_calls_limit,omitempty"` // nil = unlimited
    QuotaWindowSecs *int64            `json:"quota_window_secs,omitempty"` // nil = no window
    EnvVars         map[string]string `json:"env_vars,omitempty"`
    KeychainRefs    map[string]string `json:"keychain_refs,omitempty"`
    PullPolicy      string            `json:"pull_policy"` // "always" | "if-absent" | "never"
    NoPin           bool              `json:"no_pin,omitempty"`
}

// ContractSnapshot holds the version-pinned tool interface hash for one server version.
type ContractSnapshot struct {
    ServerName   string       `json:"server_name"`
    Version      string       `json:"version"`
    ContractHash string       `json:"contract_hash"` // "sha256:<hex>"
    ToolCount    int          `json:"tool_count"`
    Tools        []ToolRecord `json:"tools"`
    CapturedAt   time.Time    `json:"captured_at"`
}

// ToolRecord is one row of per-tool data within a ContractSnapshot.
type ToolRecord struct {
    Name        string `json:"name"`
    Description string `json:"description"`
    SchemaHash  string `json:"schema_hash"` // "sha256:<hex>"
}

// QuotaState is the live rolling-window counters for one server (or tool).
type QuotaState struct {
    ServerName    string    `json:"server_name"`
    ToolName      *string   `json:"tool_name,omitempty"` // nil = server-level
    WindowStartAt time.Time `json:"window_start_at"`
    WindowEndAt   time.Time `json:"window_end_at"`
    CallsUsed     int64     `json:"calls_used"`
    CallsLimit    *int64    `json:"calls_limit,omitempty"` // nil = unlimited
}

func (q QuotaState) IsExceeded() bool {
    return q.CallsLimit != nil && q.CallsUsed >= *q.CallsLimit
}

func (q QuotaState) UtilizationPct() float64 {
    if q.CallsLimit == nil || *q.CallsLimit == 0 {
        return 0.0
    }
    return float64(q.CallsUsed) / float64(*q.CallsLimit) * 100.0
}
```

### 10.4 Contract Hash Algorithm

The hash must be deterministic across Go versions, OS platforms, and `encoding/json` implementations.  `encoding/json` sorts map keys lexicographically by default, providing the same guarantee as Python's `json.dumps(sort_keys=True)`.

```go
// internal/mcp/host/contract.go
package mcphost

import (
    "crypto/sha256"
    "encoding/json"
    "fmt"
    "sort"
)

// canonicalTool contains only the three fields included in the contract hash.
// Excluding all other MCP server metadata ensures the hash is stable across
// non-semantic server updates (e.g., packaging changes).
type canonicalTool struct {
    Name        string         `json:"name"`
    Description string         `json:"description"`
    InputSchema map[string]any `json:"inputSchema"`
}

// ComputeContractHash derives a SHA-256 content hash of the MCP tool interface.
//
// Input:  raw tools slice from the MCP initialize response ([]map[string]any).
// Output: (contractHash, perToolRecords, error).  contractHash is "sha256:<hex>".
//
// Normalization rules:
//  1. Only include fields: name, description, inputSchema (all others discarded).
//  2. Sort tools by name ascending.
//  3. Marshal with encoding/json — map keys are sorted lexicographically.
//  4. Hash UTF-8 bytes with crypto/sha256.
func ComputeContractHash(tools []map[string]any) (string, []ToolRecord, error) {
    sorted := make([]map[string]any, len(tools))
    copy(sorted, tools)
    sort.Slice(sorted, func(i, j int) bool {
        return fmt.Sprint(sorted[i]["name"]) < fmt.Sprint(sorted[j]["name"])
    })

    canonical := make([]canonicalTool, 0, len(sorted))
    records := make([]ToolRecord, 0, len(sorted))

    for _, t := range sorted {
        name, _ := t["name"].(string)
        desc, _ := t["description"].(string)
        schema, _ := t["inputSchema"].(map[string]any)
        if schema == nil {
            schema = map[string]any{}
        }
        ct := canonicalTool{Name: name, Description: desc, InputSchema: schema}
        canonical = append(canonical, ct)

        toolBytes, err := json.Marshal(ct)
        if err != nil {
            return "", nil, fmt.Errorf("marshal tool %q: %w", name, err)
        }
        h := sha256.Sum256(toolBytes)
        records = append(records, ToolRecord{
            Name:        name,
            Description: desc,
            SchemaHash:  fmt.Sprintf("sha256:%x", h),
        })
    }

    fullBytes, err := json.Marshal(canonical)
    if err != nil {
        return "", nil, fmt.Errorf("marshal canonical tool list: %w", err)
    }
    h := sha256.Sum256(fullBytes)
    return fmt.Sprintf("sha256:%x", h), records, nil
}
```

### 10.5 Docker Backend: Container Lifecycle

All Docker operations use `os/exec.CommandContext` — no `docker/docker` moby client SDK.  Context cancellation propagates through every subprocess call, enabling clean timeout escalation.

```go
// internal/mcp/host/docker.go
package mcphost

import (
    "context"
    "crypto/rand"
    "encoding/hex"
    "fmt"
    "io"
    "os/exec"
    "strings"
)

// DockerStart pulls (per PullPolicy), starts, and returns the container ID.
func DockerStart(ctx context.Context, cfg HostedServerConfig, envSecrets map[string]string) (string, error) {
    switch cfg.PullPolicy {
    case "if-absent":
        if err := exec.CommandContext(ctx, "docker", "image", "inspect", cfg.Image).Run(); err != nil {
            if err := dockerPull(ctx, cfg.Image); err != nil {
                return "", err
            }
        }
    case "always":
        if err := dockerPull(ctx, cfg.Image); err != nil {
            return "", err
        }
    case "never":
        if err := exec.CommandContext(ctx, "docker", "image", "inspect", cfg.Image).Run(); err != nil {
            return "", fmt.Errorf("image %q not found locally and pull-policy=never; run: docker pull %s",
                cfg.Image, cfg.Image)
        }
    }

    b := make([]byte, 4)
    _, _ = rand.Read(b)
    containerName := fmt.Sprintf("tag-mcp-%s-%s", cfg.Name, hex.EncodeToString(b))

    args := []string{
        "run", "--detach",
        "--name", containerName,
        "--label", fmt.Sprintf("tag.mcp.server=%s", cfg.Name),
        "--label", fmt.Sprintf("tag.mcp.version=%s", cfg.Version),
        "--rm",
        "--network", "host",
        "--memory", "512m",
        "--cpus", "2.0",
        "--pids-limit", "256",
    }
    for k, v := range cfg.EnvVars {
        args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
    }
    for k, v := range envSecrets {
        args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
    }
    args = append(args, cfg.Image)

    out, err := exec.CommandContext(ctx, "docker", args...).Output()
    if err != nil {
        return "", fmt.Errorf("docker run for %s: %w", cfg.Name, err)
    }
    return strings.TrimSpace(string(out)), nil
}

// DockerStop stops and removes the container.
func DockerStop(ctx context.Context, containerID string) error {
    exec.CommandContext(ctx, "docker", "stop", "--time", "10", containerID).Run() //nolint:errcheck
    return exec.CommandContext(ctx, "docker", "rm", "--force", containerID).Run()
}

// DockerLogs streams container stdout+stderr lines on the returned channel.
// The channel is closed when the log stream ends or ctx is cancelled.
func DockerLogs(ctx context.Context, containerID string, tail int, follow bool) (<-chan string, error) {
    args := []string{"logs", "--timestamps", "--tail", fmt.Sprintf("%d", tail)}
    if follow {
        args = append(args, "--follow")
    }
    args = append(args, containerID)

    cmd := exec.CommandContext(ctx, "docker", args...)
    pr, pw := io.Pipe()
    cmd.Stdout = pw
    cmd.Stderr = pw
    if err := cmd.Start(); err != nil {
        return nil, err
    }

    ch := make(chan string, 64)
    go func() {
        defer close(ch)
        defer pw.Close()
        buf := make([]byte, 4096)
        var line strings.Builder
        for {
            n, err := pr.Read(buf)
            for _, b := range buf[:n] {
                if b == '\n' {
                    ch <- line.String()
                    line.Reset()
                } else {
                    line.WriteByte(b)
                }
            }
            if err != nil {
                if line.Len() > 0 {
                    ch <- line.String()
                }
                return
            }
        }
    }()
    return ch, nil
}
```

### 10.6 Stdio Bridge Protocol

The bridge sits between the TAG process (Hermes) and the container (via `docker exec`).  Concurrency is handled by goroutines and channels instead of threads.

1. Launches `docker exec -i <containerID> <mcp-entrypoint>` via `os/exec.CommandContext` with `cmd.SysProcAttr{Setpgid: true}` so SIGKILL reaches the whole process group.
2. Two goroutines (`upstream` → container, `downstream` → Hermes) run concurrently via `golang.org/x/sync/errgroup`.
3. The downstream goroutine intercepts each complete `tools/call` request, checks/increments the quota, and either forwards the frame or returns a synthetic MCP error without touching the container.
4. `Content-Length: <N>\r\n\r\n<N bytes of JSON>` framing is handled by `readFrame`/`writeFrame`; malformed frames are logged to the audit file and converted to MCP error responses.

```go
// internal/mcp/host/bridge.go
package mcphost

import (
    "bufio"
    "context"
    "database/sql"
    "encoding/json"
    "fmt"
    "io"
    "strconv"
    "strings"
    "sync"
)

// McpStdioBridge is a transparent proxy between Hermes and a containerized MCP server.
// Quota enforcement runs in the bridge goroutine; no external locking is needed beyond
// the per-bridge quotaMu which guards the SQLite BEGIN IMMEDIATE transaction.
type McpStdioBridge struct {
    cfg         HostedServerConfig
    containerID string
    db          *sql.DB
    quotaMu     sync.Mutex
}

// readFrame reads one Content-Length-framed JSON-RPC message from r.
func readFrame(r *bufio.Reader) ([]byte, error) {
    var contentLength int
    for {
        line, err := r.ReadString('\n')
        if err != nil {
            return nil, fmt.Errorf("read MCP header: %w", err)
        }
        line = strings.TrimRight(line, "\r\n")
        if line == "" {
            break // blank line terminates headers
        }
        if strings.HasPrefix(strings.ToLower(line), "content-length:") {
            val := strings.TrimSpace(line[len("content-length:"):])
            var err error
            contentLength, err = strconv.Atoi(val)
            if err != nil {
                return nil, fmt.Errorf("invalid Content-Length %q: %w", val, err)
            }
        }
    }
    if contentLength == 0 {
        return nil, fmt.Errorf("missing or zero Content-Length header")
    }
    body := make([]byte, contentLength)
    if _, err := io.ReadFull(r, body); err != nil {
        return nil, fmt.Errorf("read MCP body: %w", err)
    }
    return body, nil
}

// writeFrame writes one Content-Length-framed JSON-RPC message to w.
func writeFrame(w io.Writer, payload []byte) error {
    if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
        return err
    }
    _, err := w.Write(payload)
    return err
}

// checkAndIncrementQuota is concurrency-safe via quotaMu + SQLite WAL BEGIN IMMEDIATE.
// Returns (allowed, callsUsedAfter, callsLimit).
func (b *McpStdioBridge) checkAndIncrementQuota(ctx context.Context, toolName string) (bool, int64, *int64, error) {
    b.quotaMu.Lock()
    defer b.quotaMu.Unlock()

    tx, err := b.db.BeginTx(ctx, nil) // store layer sets PRAGMA journal_mode=WAL + BEGIN IMMEDIATE
    if err != nil {
        return false, 0, nil, err
    }
    defer tx.Rollback() //nolint:errcheck

    // Rolling-window query + upsert — identical SQL logic to the original design.
    // Full implementation lives in internal/mcp/host/quota.go; placeholder here.
    _ = tx.Commit()
    return true, 0, nil, nil
}
```

### 10.7 Version Resolution from MCP Registry

`httpx` is replaced by the standard `net/http` client.  Retries use `cenkalti/backoff/v4`.  The HTTP client is dialed through `internal/netguard` (connect-time IP-pin + redirect-revalidation) to prevent SSRF.

```go
// internal/mcp/host/registry.go
package mcphost

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "net/url"

    "github.com/cenkalti/backoff/v4"
)

const registryBase = "https://registry.modelcontextprotocol.io"

// ResolveVersion queries the MCP registry for a concrete version tag.
// If version == "latest", returns the entry marked isLatest.
// All HTTP calls are dialed through the internal/netguard SSRF-safe dialer.
// 5xx responses are retried with exponential backoff; 4xx errors are permanent.
func ResolveVersion(ctx context.Context, client *http.Client, serverName, version string) (string, error) {
    registryName := serverNameToRegistryName(serverName)
    encoded := url.PathEscape(registryName)

    var data registryVersionsResponse
    op := func() error {
        req, err := http.NewRequestWithContext(ctx, http.MethodGet,
            fmt.Sprintf("%s/v0.1/servers/%s/versions", registryBase, encoded), nil)
        if err != nil {
            return backoff.Permanent(err)
        }
        if version != "latest" {
            q := req.URL.Query()
            q.Set("version", version)
            req.URL.RawQuery = q.Encode()
        }
        resp, err := client.Do(req)
        if err != nil {
            return err // network error — retryable
        }
        defer resp.Body.Close()
        if resp.StatusCode >= 500 {
            return fmt.Errorf("registry returned %d", resp.StatusCode) // retryable
        }
        if resp.StatusCode != http.StatusOK {
            return backoff.Permanent(fmt.Errorf("registry %d for %s", resp.StatusCode, serverName))
        }
        return json.NewDecoder(resp.Body).Decode(&data)
    }

    bo := backoff.WithContext(backoff.NewExponentialBackOff(), ctx)
    if err := backoff.Retry(op, bo); err != nil {
        return "", fmt.Errorf("MCP registry lookup for %q: %w\nSpecify --image to bypass registry.", serverName, err)
    }

    if len(data.Servers) == 0 {
        return "", fmt.Errorf("no versions found for %q; use --image to specify a container image directly", serverName)
    }
    if version == "latest" {
        for _, e := range data.Servers {
            if e.Meta.IsLatest {
                return e.Version, nil
            }
        }
        return data.Servers[0].Version, nil
    }
    for _, e := range data.Servers {
        if e.Version == version {
            return version, nil
        }
    }
    avail := make([]string, 0, min(5, len(data.Servers)))
    for _, e := range data.Servers[:min(5, len(data.Servers))] {
        avail = append(avail, e.Version)
    }
    return "", fmt.Errorf("version %q not found for %q; available: %v", version, serverName, avail)
}
```

### 10.8 Integration Points

**`internal/cli/cmd_mcp_host.go` — CLI handlers** (registered under `tag mcp host` via go-chi/chi v5 + huma v2):

```go
// All handler functions call into internal/mcp/host with zero init() side effects.
// The binary's startup cost is unaffected until a `tag mcp host` subcommand is invoked.

func CmdMcpHostAdd(cfg *config.Config, args AddArgs) error {
    return mcphost.Add(context.Background(), cfg, args)
}
func CmdMcpHostList(cfg *config.Config, args ListArgs) error   { ... }
func CmdMcpHostLogs(cfg *config.Config, args LogsArgs) error   { ... }
func CmdMcpHostRemove(cfg *config.Config, args RemoveArgs) error { ... }
func CmdMcpHostInspect(cfg *config.Config, args InspectArgs) error { ... }
func CmdMcpHostStart(cfg *config.Config, args StartArgs) error { ... }
func CmdMcpHostStop(cfg *config.Config, args StopArgs) error   { ... }
```

**Hermes session startup hook** (`internal/runtime/session.go`), before spawning the Hermes process:

```go
// PRD-079: verify contract hashes for all hosted MCP servers in this profile.
if len(profileCfg.MCP.HostedServers) > 0 {
    allowDrift := os.Getenv("MCP_HOST_ALLOW_DRIFT") == "1"
    violations, err := mcphost.VerifyAllContractHashes(ctx, db, profileCfg.MCP.HostedServers, allowDrift)
    if err != nil {
        return fmt.Errorf("contract hash verification: %w", err)
    }
    for _, v := range violations {
        fmt.Fprintf(os.Stderr, "ERROR: Contract hash mismatch for %s@%s\n  Expected: %s\n  Got:      %s\n",
            v.ServerName, v.Version, v.ExpectedHash, v.ActualHash)
    }
    if len(violations) > 0 {
        return fmt.Errorf("contract hash mismatch — set MCP_HOST_ALLOW_DRIFT=1 or re-pin with: tag mcp host add")
    }
}
```

**Audit log writer** (`internal/mcp/host/audit.go`):

```go
var auditLog = filepath.Join(os.Getenv("HOME"), ".tag", "runtime", "mcp-host-audit.jsonl")

// WriteAuditEvent appends a structured event to the audit JSONL log.
// Best-effort: errors are silently discarded so an audit failure never crashes
// the agent session. The write is a single os.File.Write call and is atomic
// on POSIX for payloads under the filesystem's atomic-write limit (~4 KB).
func WriteAuditEvent(event string, fields map[string]any) {
    record := map[string]any{
        "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
        "event":     event,
    }
    for k, v := range fields {
        record[k] = v
    }
    b, err := json.Marshal(record)
    if err != nil {
        return
    }
    b = append(b, '\n')
    _ = os.MkdirAll(filepath.Dir(auditLog), 0o700)
    f, err := os.OpenFile(auditLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
    if err != nil {
        return
    }
    defer f.Close()
    _, _ = f.Write(b)
}
```

---

## 11. Security Considerations

1. **Secret handling**: Secrets passed via `--secret KEY=<keychain-ref>` are fetched from the OS keychain using `zalando/go-keyring` (`keyring.Get(service, account)`) at container start time. The resolved secret value exists only in the environment map passed to `DockerStart` or the Modal request and is never written to disk, SQLite, or the audit log. The keychain reference name (not the value) is stored in `mcp_hosted_servers.keychain_refs`.

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

All tests use the Go standard `testing` package plus `github.com/stretchr/testify/assert` and `github.com/stretchr/testify/require`.  HTTP interactions are mocked with `net/http/httptest`.  There are no pytest markers; integration tests are gated by the `//go:build integration` build tag and skipped when `DOCKER_AVAILABLE != "1"`.

### 12.1 Unit Tests

```
internal/mcp/host/contract_test.go
    TestComputeContractHash_Deterministic
        - Same tool list in different order → same hash
        - Extra fields in raw map → ignored (only name/description/inputSchema hashed)
        - Empty description → treated as empty string, not omitted

    TestComputeContractHash_Sensitivity
        - Change one tool name → different hash
        - Change one description → different hash
        - Add one tool → different hash
        - Change inputSchema field type → different hash

internal/mcp/host/quota_test.go
    TestQuotaEnforcement_UnderLimit
        - 99 calls with limit=100 → all allowed, calls_used=99

    TestQuotaEnforcement_AtLimit
        - 100th call with limit=100 → allowed (boundary), calls_used=100

    TestQuotaEnforcement_OverLimit
        - 101st call with limit=100 → blocked, returns MCP error JSON with code -32099

    TestQuotaRollingWindowReset
        - Advance mock clock past window_end_at → new window starts with calls_used=0

internal/mcp/host/registry_test.go
    TestResolveVersion_LatestSelectsIsLatest
        - httptest.NewServer returns JSON with isLatest=true on one entry → that version returned

    TestResolveVersion_NotFound
        - Version absent in registry response → error message includes available list

internal/mcp/host/secret_test.go
    TestSecret_NotStoredInSQLite
        - After DockerStart with keychain ref, SQLite row stores ref name, not resolved value

internal/mcp/host/audit_test.go
    TestWriteAuditEvent_OSError
        - auditLog path unwriteable → WriteAuditEvent does not panic or return error

internal/mcp/host/bridge_test.go
    TestMcpStdioFrame_ReadWriteRoundtrip
        - Content-Length framing: encode and decode roundtrip is lossless via bytes.Buffer

    TestContractHashMismatch_ReturnsViolation
        - VerifyAllContractHashes with mismatched stored hash → returns ContractViolation slice

internal/cli/cmd_mcp_host_test.go
    TestInitIsolation
        - Import the mcphost package in a subprocess test; assert no Docker/Modal/keyring
          calls occur before a cmd_mcp_host_* handler is explicitly invoked.
          Verified by intercepting os/exec calls with a test hook.
```

### 12.2 Integration Tests

Integration tests require a running Docker daemon.  They carry `//go:build integration` and are excluded from `go test ./...`; CI runs them via `go test -tags=integration ./internal/mcp/host/...` when `DOCKER_AVAILABLE=1`.

```
internal/mcp/host/docker_integration_test.go  // go:build integration

    TestDockerStartStopCycle
        - Start a lightweight MCP container (mcp-server-echo image)
        - Verify container is listed by `docker ps`
        - Stop and verify container is removed

    TestContractSnapshotCapturedOnAdd
        - tag mcp host add <test-server> → mcp_contract_snapshots row exists in SQLite

    TestContractMismatchBlocksSession
        - Manually corrupt contract_hash in SQLite
        - Run VerifyAllContractHashes → returns ContractViolation; caller exits non-zero

    TestLogsStreaming
        - Start container, call DockerLogs with tail=5, follow=true
        - Pump a tool call through the bridge; assert log line arrives within 1 s

    TestQuotaBlocksCallViaBridge
        - Configure QuotaCallsLimit=3
        - Send 4 tool calls through the bridge
        - 4th call receives MCP error response; container never receives the call
```

### 12.3 Performance Tests (Go Benchmarks)

```
internal/mcp/host/bench_test.go

    BenchmarkBridgeLatencyOverhead
        - 100 tool-call roundtrips through McpStdioBridge vs. direct container exec
        - Assert: median overhead < 5 ms, p99 overhead < 20 ms

    BenchmarkQuotaCheckWriteLatency
        - 1000 sequential checkAndIncrementQuota calls against an in-memory SQLite DB
        - Assert: median < 1 ms per call (WAL, single goroutine writer)

    BenchmarkAuditLogWriteThroughput
        - 10 000 sequential WriteAuditEvent calls to a temp file
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
| AC-10 | Importing the `internal/mcp/host` package in a subprocess test does not trigger any Docker, Modal, or keyring I/O before a `CmdMcpHost*` handler is explicitly called. | `TestInitIsolation` subprocess test with `os/exec` hook interceptor |
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
| `docker` CLI | External tool | >= 24.0 | Must be in `$PATH`. The `docker/docker` moby client SDK is NOT used; all operations go through `os/exec`. |
| `github.com/modelcontextprotocol/go-sdk` | Go module | v1.6.1 | MCP protocol 2025-11-25 framing for the stdio bridge. Pin this version. |
| `modernc.org/sqlite` | Go module | current project pin | Pure-Go SQLite (CGO_ENABLED=0), FTS5 built-in. Already used project-wide. Single-writer discipline via `gofrs/flock` + WAL. |
| `cenkalti/backoff/v4` | Go module | v4 | Exponential retry for MCP registry HTTP calls and Modal REST API. Already in project. |
| `go.opentelemetry.io/otel` | Go module | current project pin | Hosted server events emitted as OTel spans when `OTEL_EXPORTER_OTLP_ENDPOINT` is set (PRD-013). Already in project. |
| `go-chi/chi` v5 + `danielgtaylor/huma` v2 | Go modules | current project pins | CLI handler routing and request/response schema. Already in project. |
| `invopop/jsonschema` | Go module | current project pin | JSON schema generation for huma v2 handler structs. Already in project. |
| `zalando/go-keyring` | Go module | latest | OS keychain access. Compiled in but called only when `--secret` is used; zero init() side effects. Added to `go.mod`. |
| `net/http` (stdlib) | Go stdlib | — | MCP registry version resolution and Modal REST API client. Replaces `httpx`. No new dependency. |
| `internal/netguard` | Internal package | current | SSRF-safe dialer (connect-time IP-pin + redirect-revalidation) wrapping `net/http` clients for registry and Modal calls. |
| PRD-028 (sandbox) | Internal | current | `internal/mcp/host` reuses UTC-time helpers and audit log path conventions from `internal/sandbox`. |
| PRD-014 (MCP registry) | Internal | current | Version resolution queries the same MCP registry endpoints defined in PRD-014. |
| PRD-013 (tracing) | Internal | current | OTel spans for hosted server events via `internal/tracing`. |
| PRD-034 (secret scanning) | Internal | current | Pattern detection for `--env` secret warnings reuses patterns in `internal/store` (secret scanner). |
| MCP Registry API | External service | v0.1 | `https://registry.modelcontextprotocol.io` — used for version resolution. 5xx responses are retried; fall back to `--image` on permanent failure. |

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
| 1-2 | SQLite DDL migration (`migrate079Tables` in `internal/store/migrations/prd079_mcp_host.go`); `HostedServerConfig`, `ContractSnapshot`, `QuotaState` Go structs in `internal/mcp/host/types.go`; `ComputeContractHash` in `contract.go` with full unit test coverage (`contract_test.go`) |
| 3-4 | Docker backend: `DockerStart`, `DockerStop`, `DockerLogs`, `dockerPull` — all via `os/exec.CommandContext` in `docker.go`; `CmdMcpHostAdd` and `CmdMcpHostRemove` wired in `internal/cli/cmd_mcp_host.go`; `WriteAuditEvent` in `audit.go` |
| 5-6 | `McpStdioBridge` in `bridge.go` — `readFrame`/`writeFrame` Content-Length parser, goroutine-based forwarder, quota intercept logic in `quota.go`; `CmdMcpHostList --json`; `CmdMcpHostLogs --follow` streaming via channel |
| 7 | Contract snapshot capture and `VerifyAllContractHashes`; integration into `internal/runtime/session.go` session startup hook before Hermes spawn |
| 8 | `CmdMcpHostInspect --diff`; `--secret` flag with `zalando/go-keyring` integration; `--env` secret pattern warning from `internal/store` scanner |

### Phase 2 — Quota System and Polish (Days 9–13)

| Day | Deliverable |
|-----|-------------|
| 9-10 | Rolling-window quota implementation with `BEGIN IMMEDIATE` in `quota.go`; quota warning at 80%; `tag mcp host quota reset`; per-tool quota (`FR-06`) |
| 11 | `CmdMcpHostStart` / `CmdMcpHostStop` idempotent lifecycle; orphaned container cleanup at startup (scan `tag.mcp.server` labels via `docker ps`); `--pull-policy` all three modes |
| 12 | `ResolveVersion` in `registry.go` with `cenkalti/backoff/v4` retry and graceful 500 handling; `--version latest` concrete resolution; `--dry-run` mode |
| 13 | Integration test suite (`docker_integration_test.go` with `//go:build integration`); Go benchmark suite (`bench_test.go`); bridge latency benchmarks |

### Phase 3 — Modal Backend and Hardening (Days 14–18)

| Day | Deliverable |
|-----|-------------|
| 14-15 | Modal backend in `modal.go`: Modal REST API client (`net/http` + `cenkalti/backoff/v4`), bidirectional stdin/stdout stream via local goroutine channel; `CmdMcpHostAdd --backend modal` |
| 16 | OTel span emission for hosted server events via `go.opentelemetry.io/otel` (PRD-013 integration); `MCP_HOST_FAIL_ON_HASH_DRIFT` CI environment variable |
| 17 | End-to-end test: `tag mcp host add playwright --backend docker → tag run --profile browser → verify tool call reaches container and returns result` |
| 18 | Documentation update in `docs/prd/INDEX.md`; `tag doctor` check for Docker daemon and Modal credentials when hosted servers are registered |

---

*PRD-079 authored for TAG CLI — GitHub issue #346*

