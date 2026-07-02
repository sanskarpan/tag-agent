# PRD-095: Sandbox Pause/Resume with Billing Pause (`tag sandbox pause / tag sandbox resume`)
> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox`
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing & Observability), PRD-034 (Security), PRD-012 (Cost Tracking & Budget), PRD-008 (Background Task Queue)
**Inspired by:** Daytona pause/resume, E2B pause, Fly.io suspend
**GitHub Issue:** #348

---

## 1. Overview

Developer workflows are punctuated by long idle periods — meetings, code review, lunch breaks, end-of-day — during which a running sandbox consumes compute resources and accrues billing at full rate even though no agent or user is actively using it. TAG sandboxes today have no mechanism to pause execution and freeze billing; the only options are destroying the sandbox (losing all in-memory and filesystem state) or leaving it running (paying for idle time).

This PRD specifies `tag sandbox pause` and `tag sandbox resume`: commands that checkpoint a running sandbox's full execution state (memory, process tree, open file descriptors, filesystem diffs) to durable storage, transition the sandbox into a zero-cost paused state, and later restore the sandbox to exactly the same state so work can continue without any cold-start overhead. While paused, the sandbox occupies no active compute allocation — billing stops. This directly addresses a core pain point for long-running development sessions: the choice between destroying state and paying for idle time is eliminated.

The implementation wraps provider-native checkpoint APIs where available. For E2B-backed sandboxes (the primary production path), the Firecracker microVM REST API provides `PATCH /vm {"state": "Paused"}` followed by `PUT /snapshot/create` — pausing at approximately 4 seconds per GiB of RAM and resuming in approximately 1 second. For Daytona-backed sandboxes, the provider's native pause/resume semantics apply. For Docker-backed local sandboxes, CRIU (Checkpoint/Restore In Userspace) is used when available, with a graceful-degradation path to `docker pause` (which suspends processes via SIGSTOP but does not checkpoint memory to disk, therefore does not stop billing for cloud-hosted Docker environments). For the restricted subprocess backend, pause is implemented via `SIGSTOP` to the process tree.

The billing pause guarantee is explicit and provider-specific: E2B paused sandboxes accrue no credits; Daytona paused workspaces consume no workspace credits; Modal sandboxes that are checkpointed via filesystem snapshot and then destroyed are not billed for idle compute. The TAG CLI reflects provider-reported billing state in `tag sandbox status --json` so users can verify the billing impact of a pause operation before relying on it for cost management.

Pause/resume integrates with the existing SQLite-backed sandbox state machine in `sandbox_runs` (PRD-028) via a new `sandbox_checkpoints` table, and with the budget subsystem (PRD-012) via a new `paused_at` / `resumed_at` lifecycle event pair that is excluded from cost roll-up calculations. The feature is designed such that a sandbox paused in session A can be resumed in session B on the same or a different machine, as long as the checkpoint is stored in a location reachable from both (local filesystem, or cloud provider checkpoint storage).

---

## 2. Problem Statement

### 2.1 Idle Compute Waste in Long Development Sessions

TAG sandboxes are frequently used for multi-step development workflows: install dependencies, run tests, iterate on code, run tests again. These sessions routinely span multiple hours. During code review, lunch, or end-of-day the sandbox sits idle but continues to consume compute and accrue charges. E2B Pro tier charges at a continuous rate regardless of whether any process is executing inside the sandbox. Developers make the economically irrational choice of destroying sandboxes to stop billing, then spending 30–120 seconds rebuilding state (reinstalling packages, re-applying patches, waiting for services to start) when they return. This destroys flow state and wastes developer time at a rate that far exceeds the compute savings.

### 2.2 No Durable Mid-Session Checkpoint

TAG has no mechanism to preserve the exact in-memory execution state of a sandbox across TAG CLI restarts, machine reboots, or session boundaries. If a user is running a multi-hour agent task inside a sandbox and their laptop loses power, the entire sandbox state is lost — including any intermediate results, installed packages, and in-progress computations. There is no equivalent of `hibernate` for TAG sandbox sessions. CRIU and Firecracker snapshot APIs solve this problem at the infrastructure level, but TAG exposes no interface to them.

### 2.3 Queue Jobs Cannot Yield Compute Between Phases

TAG's `queue_worker.py` dispatches multi-phase jobs where phase N must complete before phase N+1 begins. Between phases there is often a human approval gate or a waiting period (waiting for an external API, waiting for CI to complete). During this inter-phase wait, the sandbox allocated to the job sits idle and burns billing. The queue system has no mechanism to checkpoint and release the sandbox between phases, then restore it for the next phase. This forces job authors to either provision overly large timeouts (billing for idle wait) or redesign jobs to destroy and recreate state between phases (increasing latency and complexity).

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | **Pause with billing stop:** `tag sandbox pause <id>` checkpoints a running sandbox and transitions it to a state in which the backing provider charges no active compute fees. |
| G2 | **Resume with state fidelity:** `tag sandbox resume <id>` restores a paused sandbox to exactly the pre-pause execution state; the process tree, memory, open file descriptors, and filesystem all match the paused point. |
| G3 | **Provider abstraction:** E2B (Firecracker snapshot), Daytona (native pause), Docker with CRIU, and SIGSTOP fallback are each implemented behind a common `SandboxProvider.pause()` / `SandboxProvider.resume()` interface. |
| G4 | **SQLite state tracking:** Every pause/resume operation is recorded in `sandbox_checkpoints` with checkpoint path, provider checkpoint ID, and billing state so TAG can reason about cost impact across sessions. |
| G5 | **Cross-session resume:** A sandbox paused in one TAG CLI session can be resumed in a later session (same or different machine for cloud providers; same machine for local Docker/restricted). |
| G6 | **Budget integration:** The cost roll-up in `budget.py` treats `paused_at → resumed_at` intervals as zero-cost, so `tag costs` correctly reflects only active compute time. |
| G7 | **List and status visibility:** `tag sandbox list --paused` and `tag sandbox status <id> --json` surface checkpoint metadata, billing state, and resume ETA in both human-readable and JSON formats. |
| G8 | **Queue integration:** `queue_worker.py` can declare inter-phase pause points via job config; the worker automatically pauses the sandbox, waits for the gate condition, and resumes without user intervention. |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | **Live migration:** Moving a paused sandbox from one host or region to another is not supported in v1. Checkpoint files are tied to the provider's storage layer. |
| NG2 | **Windows CRIU support:** CRIU does not support Windows. Docker `pause` (SIGSTOP) is the Windows fallback; full checkpoint-to-disk is Linux-only. |
| NG3 | **Incremental / differential checkpoints:** All checkpoints in v1 are full snapshots. Incremental dirty-page tracking (as used in live migration) is deferred. |
| NG4 | **Automatic idle-pause:** Automatically pausing sandboxes after N minutes of inactivity is a policy feature deferred to PRD-096 (Sandbox Auto-Suspend Policy). This PRD covers only the explicit pause/resume API. |
| NG5 | **Multi-sandbox coordinated pause:** Pausing a linked sandbox group (parent + children) atomically is deferred. Each sandbox is paused individually. |
| NG6 | **Checkpoint encryption at rest:** Checkpoint files stored on local disk are not encrypted by TAG in v1. Users relying on disk encryption at the OS level (FileVault, LUKS) should ensure it is enabled. |
| NG7 | **Billing guarantee enforcement:** TAG reports provider-stated billing state but cannot enforce or audit provider billing. Billing pause is a provider contract, not a TAG guarantee. |
| NG8 | **Restricted subprocess cross-session resume:** The restricted subprocess backend supports SIGSTOP-based pause within the same TAG process only; it does not checkpoint memory to disk and therefore does not support cross-session resume. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| E2B pause duration | < 6 s for 512 MB sandbox | `tag sandbox pause <id> --json` → `pause_duration_s`; median over 50 ops |
| E2B resume duration | < 2 s | `tag sandbox resume <id> --json` → `resume_duration_s`; median over 50 ops |
| State fidelity after resume | 100% process tree and filesystem match | Integration test: write file, install package, pause, resume, verify file and package present |
| Billing stop on pause | Confirmed by provider API status | `tag sandbox status <id> --json` → `billing_active: false` within 10 s of pause |
| Cross-session resume success rate | > 99% for E2B and Daytona | Integration test suite: pause in session A, exit CLI, new session, resume |
| Cost undercount error | < 0.5% | Synthetic test: known active/paused schedule vs. `tag costs` report |
| `tag sandbox list --paused` latency | < 200 ms | CLI timing; SQLite query only, no provider API call |
| Developer time saved per session | Estimated 5–30 min/day for users with idle periods > 30 min | User survey; A/B comparison of session reconstruction time |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag sandbox pause <id>` before a 2-hour meeting | I stop accruing E2B credits while I am away, without losing my pip installs or in-progress test results |
| U2 | Developer | run `tag sandbox resume <id>` when I return | I am back in exactly the state I left, with no time spent reinstalling dependencies or re-applying patches |
| U3 | Platform engineer | run `tag sandbox list --paused --json` | I can see all paused sandboxes across the team (or my own sessions) with checkpoint age, checkpoint size, and estimated cost savings |
| U4 | Cost-conscious operator | view `tag costs` | Idle paused time is correctly excluded so my cost report reflects only active compute, and I can make informed decisions about sandbox lifecycle |
| U5 | Queue job author | declare `"pause_between_phases": true` in my job config | The queue worker automatically pauses the sandbox during an approval gate and resumes it when the gate clears, without manual intervention |
| U6 | Developer on E2B | run `tag sandbox status <id> --json` immediately after pausing | I can verify `billing_active: false` and `state: paused` before trusting that the pause was effective |
| U7 | Developer on Docker + CRIU | run `tag sandbox pause <id>` on a local container | The container process tree is checkpointed to `~/.tag/checkpoints/<id>/` and I can resume it later with full state, even after a laptop reboot |
| U8 | Developer on Docker without CRIU | run `tag sandbox pause <id>` | The CLI warns me that CRIU is unavailable, falls back to `docker pause` (SIGSTOP), and makes clear that billing is not paused in this mode |
| U9 | Developer | run `tag sandbox resume <id>` from a different machine | For E2B and Daytona (cloud providers), the checkpoint is in provider storage and I can resume on any machine with valid credentials |
| U10 | Developer | run `tag sandbox pause <id> --wait` | The CLI blocks until the checkpoint is fully written to durable storage and the provider confirms billing has stopped |

---

## 7. Proposed CLI Surface

All pause/resume subcommands extend the existing `tag sandbox` namespace established in PRD-028. The CLI commands are registered as cobra subcommands under the `sandbox` command group in `internal/cli`, following the same `cobra.Command` pattern (with `RunE` handlers and `Flags()` bindings) as `tag sandbox run` and `tag sandbox kill`.

### 7.1 `tag sandbox pause`

```
tag sandbox pause <sandbox-id> [OPTIONS]

Pause a running sandbox by checkpointing its full execution state.
For E2B-backed sandboxes, uses the Firecracker microVM snapshot API.
For Daytona-backed sandboxes, uses the provider's native pause API.
For Docker + CRIU, checkpoints the container to disk.
For Docker without CRIU, falls back to docker pause (SIGSTOP; billing NOT stopped).
For restricted subprocess, sends SIGSTOP to the process group.

ARGUMENTS
  <sandbox-id>        Sandbox ID as shown in `tag sandbox list`. Required.

OPTIONS
  --wait              Block until checkpoint is fully written and provider
                      confirms billing_active=false. Default: block with
                      progress spinner. Use --no-wait for fire-and-forget.

  --no-wait           Return immediately after issuing the pause request.
                      The sandbox transitions to 'pausing' state asynchronously.
                      Poll with `tag sandbox status <id>` to confirm completion.

  --checkpoint-dir <path>
                      For Docker+CRIU backend: directory to write checkpoint
                      files. Default: ~/.tag/checkpoints/<sandbox-id>/.
                      Must have sufficient free space (estimate: RAM size + 20%).

  --json              Emit a JSON object on completion:
                      {
                        "sandbox_id": "sbx-abc123",
                        "state": "paused",
                        "checkpoint_id": "ckpt-xyz789",
                        "checkpoint_size_bytes": 536870912,
                        "pause_duration_s": 3.7,
                        "billing_active": false,
                        "provider": "e2b",
                        "paused_at": "2026-06-17T14:23:01Z"
                      }

  --quiet             Suppress progress output; only emit errors and final
                      JSON (if --json) or exit code.

EXAMPLES
  # Pause an E2B sandbox and wait for confirmation
  tag sandbox pause sbx-abc123

  # Pause a Docker sandbox with CRIU to a custom checkpoint directory
  tag sandbox pause sbx-local01 --checkpoint-dir /mnt/fast-storage/ckpts/

  # Pause and capture machine-readable output for scripting
  tag sandbox pause sbx-abc123 --json | jq '.billing_active'

  # Fire-and-forget pause (useful in scripts where the pause will complete
  # before the next step that needs it)
  tag sandbox pause sbx-abc123 --no-wait

EXIT CODES
  0   Pause completed successfully; billing stopped (where applicable).
  1   Sandbox not found or already paused or in a non-pausable state.
  2   Pause operation timed out (provider did not confirm within 60 s).
  3   Checkpoint write failed (disk full, permission error, etc.).
  4   Provider does not support pause for this sandbox type.
  5   CRIU not available; fell back to docker pause (SIGSTOP). Billing NOT paused.
      (Non-zero to surface the degraded behavior explicitly.)
```

### 7.2 `tag sandbox resume`

```
tag sandbox resume <sandbox-id> [OPTIONS]

Resume a previously paused sandbox from its checkpoint.
Restores the exact process tree, memory contents, open file descriptors,
and filesystem state at the time of the last `tag sandbox pause` call.
After resume, the sandbox timeout resets to max(5 min, original_timeout).

ARGUMENTS
  <sandbox-id>        Sandbox ID as shown in `tag sandbox list --paused`. Required.

OPTIONS
  --wait              Block until the sandbox is in 'running' state and
                      has passed its health check (process 1 responsive).
                      Default: block with progress spinner.

  --no-wait           Return after issuing the resume request without
                      waiting for 'running' confirmation.

  --timeout <seconds>
                      Override the sandbox timeout after resume.
                      Default: max(300, original_timeout_at_creation).
                      Maximum: 86400 (E2B Pro limit).

  --json              Emit a JSON object on completion:
                      {
                        "sandbox_id": "sbx-abc123",
                        "state": "running",
                        "resume_duration_s": 0.9,
                        "billing_active": true,
                        "provider": "e2b",
                        "resumed_at": "2026-06-17T16:45:12Z",
                        "paused_duration_s": 8531.4
                      }

  --quiet             Suppress progress output.

EXAMPLES
  # Resume an E2B sandbox
  tag sandbox resume sbx-abc123

  # Resume and pipe to status check
  tag sandbox resume sbx-abc123 --json | jq '.resume_duration_s'

  # Resume with extended timeout for a long afternoon session
  tag sandbox resume sbx-abc123 --timeout 14400

EXIT CODES
  0   Resume completed; sandbox is running and billing is active.
  1   Sandbox not found or not in paused state.
  2   Resume timed out (sandbox did not reach 'running' within 30 s).
  3   Checkpoint not found or corrupted.
  4   Provider resume API error (see stderr for details).
```

### 7.3 `tag sandbox list --paused`

```
tag sandbox list [--paused] [--running] [--all] [--json]

OPTIONS (additions to existing list command)
  --paused            Show only sandboxes in state 'paused' or 'pausing'.
  --running           Show only sandboxes in state 'running' (existing behavior).
  --all               Show all states: creating, starting, running, pausing,
                      paused, resuming, killing, killed, error.

OUTPUT (table, --paused)
  SANDBOX_ID    PROVIDER  STATE   PAUSED_AT              CHECKPOINT_SIZE  PAUSED_FOR
  sbx-abc123    e2b       paused  2026-06-17T14:23:01Z   512 MB           2h 22m
  sbx-def456    docker    paused  2026-06-17T09:11:44Z   1.2 GB           7h 33m
  sbx-ghi789    e2b       pausing 2026-06-17T16:44:58Z   —                0m (in progress)

JSON output (--paused --json):
  [
    {
      "sandbox_id": "sbx-abc123",
      "provider": "e2b",
      "state": "paused",
      "paused_at": "2026-06-17T14:23:01Z",
      "checkpoint_id": "ckpt-xyz789",
      "checkpoint_size_bytes": 536870912,
      "billing_active": false,
      "paused_duration_s": 8521,
      "original_image": "e2b-default",
      "created_at": "2026-06-17T08:00:00Z"
    }
  ]
```

### 7.4 `tag sandbox status <id> --json`

```
tag sandbox status <sandbox-id> [--json] [--watch]

OPTIONS
  --json    Emit machine-readable JSON including pause/checkpoint metadata.
  --watch   Refresh every 2 seconds until state becomes 'running' or 'killed'.

JSON output:
  {
    "sandbox_id": "sbx-abc123",
    "provider": "e2b",
    "state": "paused",
    "created_at": "2026-06-17T08:00:00Z",
    "paused_at": "2026-06-17T14:23:01Z",
    "resumed_at": null,
    "billing_active": false,
    "checkpoint_id": "ckpt-xyz789",
    "checkpoint_size_bytes": 536870912,
    "pause_count": 2,
    "total_active_seconds": 22981,
    "total_paused_seconds": 8521,
    "provider_state_raw": "Paused",
    "timeout_remaining_s": null,
    "image": "e2b-default",
    "metadata": {}
  }
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag sandbox pause <id>` must confirm that the sandbox is in `running` state before issuing a pause request; if the sandbox is in any other state, the command exits with code 1 and an explicit error message including the current state. |
| FR-02 | For E2B-backed sandboxes, pause must drive the `firecracker-go-sdk` machine API in the sequence `PauseVM(ctx)` (equivalent to `PATCH /vm {"state":"Paused"}`) → `CreateSnapshot(ctx, memPath, snapPath)` (a Full snapshot, equivalent to `PUT /snapshot/create`) → confirm via `DescribeInstance(ctx)` that state is `Paused`. |
| FR-03 | For E2B-backed sandboxes, resume must call `LoadSnapshot(ctx, snapPath)` followed by `ResumeVM(ctx)` (equivalent to `PUT /snapshot/load {..., "resume_vm": true}`) and confirm via `DescribeInstance(ctx)` that state is `Running`. |
| FR-04 | For Daytona-backed sandboxes, pause must call the Daytona workspace pause API (via its Go HTTP client) and confirm the workspace enters `Stopped` state within 30 seconds. |
| FR-05 | For Docker + CRIU, pause must: (a) feature-detect the `criu` binary with `exec.LookPath`; (b) call the moby client `CheckpointCreate(ctx, containerID, checkpoint.CreateOptions{...})`; (c) write the checkpoint under `~/.tag/checkpoints/<sandbox-id>/`; (d) stop the container after checkpoint creation (`Exit: true`). |
| FR-06 | For Docker without CRIU, pause must: (a) call the moby client `ContainerPause(ctx, containerID)` (SIGSTOP); (b) set `state = 'paused'` and `billing_paused = false` in the SQLite record; (c) exit with code 5 and print `Warning: CRIU not available. Container is suspended (SIGSTOP) but billing is NOT paused. Install CRIU for full checkpoint support.` |
| FR-07 | For the restricted subprocess backend, the managed process is started with `SysProcAttr{Setpgid: true}` so it leads its own process group; pause must send `syscall.Kill(-pgid, syscall.SIGSTOP)` to the process group. Resume sends `syscall.SIGCONT`. Both calls honor a cancellable `context.Context`. |
| FR-08 | Every pause operation must write a row to `sandbox_checkpoints` (schema in Section 9.2) within the same SQLite transaction as updating the corresponding `sandbox_runs.state` to `'paused'`. Both updates must be atomic; if either fails, neither is committed. |
| FR-09 | Every resume operation must update `sandbox_runs.state` to `'running'`, set `sandbox_checkpoints.resumed_at` to the current UTC timestamp, and increment `sandbox_runs.resume_count`. |
| FR-10 | `tag sandbox list --paused` must read exclusively from the local `sandbox_runs` + `sandbox_checkpoints` tables; it must not make any provider API calls. Response must complete in < 200 ms on a warm SQLite connection. |
| FR-11 | `tag sandbox status <id> --json` must merge local SQLite state with a live provider API call to fetch `provider_state_raw` and `billing_active`. If the provider API call fails, the command must return local state with `"provider_state_raw": null, "provider_api_error": "<message>"` and exit code 0 (local state is still useful). |
| FR-12 | The budget module (`internal/budget`) must be updated to exclude `paused_at → resumed_at` intervals from cost roll-up. The `cost_seconds` field in `sandbox_runs` must reflect only active (non-paused) compute time. |
| FR-13 | `pause_duration_s` (wall-clock time from pause request to provider confirmation) must be measured and stored in `sandbox_checkpoints.pause_duration_s`. Similarly `resume_duration_s`. Both are included in `--json` output. |
| FR-14 | The `SandboxProvider` interface (Section 10.3) must declare `Pause(ctx context.Context, sandboxID string) (PauseResult, error)` and `Resume(ctx context.Context, sandboxID string, checkpoint CheckpointRef) (ResumeResult, error)` methods. All provider structs must satisfy the interface. |
| FR-15 | If a sandbox is killed or errors while in `pausing` state, the partial checkpoint must be cleaned up (provider API call to delete snapshot, local files removed) and the sandbox transitioned to `error` state with `error_message` set. |
| FR-16 | `tag sandbox pause` with `--wait` (the default) must poll for provider confirmation of billing stop at 1-second intervals with a 60-second hard timeout. If the 60-second timeout elapses without billing confirmation, the command exits with code 2 and leaves the sandbox in `pausing` state in SQLite so the operator can re-query. |
| FR-17 | The `internal/queue` worker integration must check for a `pause_between_phases` boolean in the job config. When true, after each phase completes, the worker calls `provider.Pause(ctx, sandboxID)`, enters a polling loop on a gate condition (configurable: time delay, external HTTP poll, or SQLite flag) driven by a `context.Context` deadline, and calls `provider.Resume(ctx, sandboxID, ckpt)` when the gate clears. |
| FR-18 | Pause and resume operations must emit OpenTelemetry spans (via `internal/obs`, using `go.opentelemetry.io/otel` tracer spans) with attributes: `sandbox.id`, `sandbox.provider`, `sandbox.operation` (`pause` or `resume`), `sandbox.checkpoint_id`, `sandbox.checkpoint_size_bytes`, `sandbox.pause_duration_s`. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Pause latency (E2B):** End-to-end pause time (CLI invocation to `billing_active=false` confirmed) must be < 6 s for sandboxes with ≤ 512 MB RAM under normal network conditions. Per the E2B research finding of ~4 s/GiB, a 512 MB sandbox should pause in ~2 s with margin. |
| NFR-02 | **Resume latency (E2B):** End-to-end resume time (CLI invocation to `state=running` confirmed) must be < 3 s for sandboxes with ≤ 512 MB RAM. The research baseline is ~1 s for Firecracker snapshot restore. |
| NFR-03 | **Local CRIU checkpoint throughput:** Docker + CRIU checkpoint write speed must not be the bottleneck beyond the kernel's memory dump rate. TAG must not add more than 500 ms of overhead on top of the raw `docker checkpoint create` duration. |
| NFR-04 | **SQLite atomicity:** All pause/resume state transitions in SQLite must run inside a single `sql.Tx` opened with `db.BeginTx(ctx, ...)` against `modernc.org/sqlite` (which issues `BEGIN IMMEDIATE`), committed with `tx.Commit()` (or `tx.Rollback()` via a deferred guard). No intermediate state where `sandbox_runs.state` has been updated but `sandbox_checkpoints` has not yet been written, or vice versa. |
| NFR-05 | **No provider API calls in list:** `tag sandbox list --paused` reads only from SQLite. Provider API calls are gated behind `status` subcommand calls so list remains fast even with hundreds of paused sandboxes. |
| NFR-06 | **Error message clarity:** All error messages from pause/resume must include: (a) the sandbox ID, (b) the current state, (c) the expected state, (d) the provider name, (e) the underlying error from the provider SDK (if any). One-liners in the format `[sbx-abc123] pause failed: sandbox is in state 'killed', expected 'running' (provider: e2b)`. |
| NFR-07 | **Checkpoint storage hygiene:** Checkpoint files written to local disk (`~/.tag/checkpoints/`) must be removed when the corresponding sandbox is killed or when `tag sandbox rm <id>` is called. A `tag sandbox checkpoints prune --older-than 7d` subcommand cleans up orphaned checkpoint directories. |
| NFR-08 | **Billing accuracy:** The cost delta between a session that uses pause/resume and one that does not should be quantifiable via `tag costs --detail`. The `active_seconds` and `paused_seconds` fields in the detailed cost breakdown must be accurate to within 5 seconds. |
| NFR-09 | **Backward compatibility:** The new `pause` and `resume` subcommands must not alter behavior of existing `tag sandbox run`, `tag sandbox kill`, `tag sandbox list`, or `tag sandbox logs` commands. The new `--paused` flag on `list` is additive; existing `list` output is unchanged when `--paused` is not specified. |
| NFR-10 | **Dependency isolation:** CRIU integration must go through the moby Docker client (`github.com/docker/docker/client`) checkpoint APIs (with the `docker` host binary / CRIU as a system dependency) and must not add a new mandatory Go module beyond those already vendored by PRD-028. Firecracker pause uses `firecracker-microvm/firecracker-go-sdk`. Daytona pause uses the existing Daytona HTTP client. No new mandatory dependencies are introduced by this PRD. |

---

## 10. Technical Design

### 10.1 New and Modified Files

| File | Change Type | Description |
|------|-------------|-------------|
| `internal/sandbox` | Modified | Add `Pause()`, `Resume()`, `SandboxState` typed-string constants, `CheckpointRef` struct, `SandboxProvider` interface extension, provider-specific pause/resume implementations for Firecracker/E2B, Daytona, Docker+CRIU, restricted subprocess |
| `internal/cli` | Modified | Add `sandbox pause` and `sandbox resume` cobra commands, extend `sandbox list` with `--paused` / `--all` flags, extend `sandbox status` with checkpoint metadata output |
| `internal/budget` | Modified | Update cost roll-up to exclude paused intervals; add `active_seconds` / `paused_seconds` decomposition to cost records |
| `internal/queue` | Modified | Add `pause_between_phases` job config handling; integrate `provider.Pause()` / `provider.Resume()` at phase boundaries |
| `internal/obs` | Modified | Add `sandbox.pause` and `sandbox.resume` OTel span instrumentation following existing sandbox span patterns |

### 10.2 SQLite DDL

The following DDL additions are applied by the schema-migration path in `internal/sandbox` (invoked from `internal/store` when the database is opened at startup), executing statements against `modernc.org/sqlite` (pure-Go, `CGO_ENABLED=0`) via `database/sql`.

```sql
-- sandbox_checkpoints: one row per pause event per sandbox.
-- A sandbox may have multiple checkpoint rows if it has been paused and
-- resumed multiple times.
CREATE TABLE IF NOT EXISTS sandbox_checkpoints (
    id                   TEXT PRIMARY KEY,           -- UUID4, e.g. "ckpt-abc123"
    sandbox_id           TEXT NOT NULL,              -- FK → sandbox_runs.id
    provider             TEXT NOT NULL,              -- 'e2b' | 'daytona' | 'docker' | 'restricted'
    provider_checkpoint_id TEXT,                     -- Provider-native checkpoint ID (E2B snapshot ID, etc.)
    checkpoint_path      TEXT,                       -- Local filesystem path (Docker+CRIU only); NULL for cloud
    checkpoint_size_bytes INTEGER,                   -- Size of checkpoint data in bytes; NULL if not available
    state                TEXT NOT NULL DEFAULT 'creating',
                         -- 'creating' | 'ready' | 'restoring' | 'restored' | 'failed' | 'pruned'
    billing_paused       INTEGER NOT NULL DEFAULT 0, -- 1 if provider confirmed billing stop; 0 otherwise
    pause_duration_s     REAL,                       -- Wall-clock seconds from pause request to provider confirm
    resume_duration_s    REAL,                       -- Wall-clock seconds from resume request to running confirm
    error_message        TEXT,                       -- Set on state='failed'
    paused_at            TEXT NOT NULL,              -- ISO-8601 UTC timestamp when pause was initiated
    checkpoint_ready_at  TEXT,                       -- ISO-8601 UTC when checkpoint was fully written
    resumed_at           TEXT,                       -- ISO-8601 UTC when resume completed; NULL if not yet resumed
    created_at           TEXT NOT NULL               -- ISO-8601 UTC insert time
);

CREATE INDEX IF NOT EXISTS idx_sc_sandbox_id
    ON sandbox_checkpoints(sandbox_id, paused_at DESC);

CREATE INDEX IF NOT EXISTS idx_sc_state
    ON sandbox_checkpoints(state, paused_at DESC);

-- Extend sandbox_runs with pause lifecycle columns.
-- NOTE: SQLite (modernc.org/sqlite) does NOT support
-- "ALTER TABLE ... ADD COLUMN IF NOT EXISTS". Each ALTER below is run
-- unconditionally by the Go migration code, which wraps every statement and
-- ignores the "duplicate column name" error returned via database/sql so the
-- migration remains idempotent and safe to re-run.
ALTER TABLE sandbox_runs ADD COLUMN state TEXT NOT NULL DEFAULT 'running';
ALTER TABLE sandbox_runs ADD COLUMN pause_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandbox_runs ADD COLUMN resume_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandbox_runs ADD COLUMN total_paused_seconds REAL NOT NULL DEFAULT 0.0;
ALTER TABLE sandbox_runs ADD COLUMN last_paused_at TEXT;
ALTER TABLE sandbox_runs ADD COLUMN last_resumed_at TEXT;
ALTER TABLE sandbox_runs ADD COLUMN provider TEXT;
ALTER TABLE sandbox_runs ADD COLUMN provider_sandbox_id TEXT;

-- Index for the list --paused query path
CREATE INDEX IF NOT EXISTS idx_sr_state_paused
    ON sandbox_runs(state, last_paused_at DESC)
    WHERE state IN ('paused', 'pausing');

-- sandbox_billing_intervals: fine-grained active/paused timeline for cost roll-up.
-- One row per contiguous active or paused interval.
CREATE TABLE IF NOT EXISTS sandbox_billing_intervals (
    id           TEXT PRIMARY KEY,
    sandbox_id   TEXT NOT NULL,
    interval_type TEXT NOT NULL CHECK(interval_type IN ('active', 'paused')),
    started_at   TEXT NOT NULL,
    ended_at     TEXT,           -- NULL if interval is still open
    duration_s   REAL            -- Populated when ended_at is set
);

CREATE INDEX IF NOT EXISTS idx_sbi_sandbox
    ON sandbox_billing_intervals(sandbox_id, started_at);
```

### 10.3 Core Types and Interface

```go
// internal/sandbox — pause/resume type additions

package sandbox

import (
	"context"
	"errors"
)

// SandboxState is the full sandbox state machine including pause lifecycle.
type SandboxState string

const (
	StateCreating SandboxState = "creating"
	StateStarting SandboxState = "starting"
	StateRunning  SandboxState = "running"
	StatePausing  SandboxState = "pausing"
	StatePaused   SandboxState = "paused"
	StateResuming SandboxState = "resuming"
	StateKilling  SandboxState = "killing"
	StateKilled   SandboxState = "killed"
	StateError    SandboxState = "error"
)

// Membership sets, evaluated by helper functions on SandboxState.
var (
	terminalStates  = map[SandboxState]bool{StateKilled: true, StateError: true}
	pausableStates  = map[SandboxState]bool{StateRunning: true}
	resumableStates = map[SandboxState]bool{StatePaused: true}
)

func (s SandboxState) IsTerminal() bool  { return terminalStates[s] }
func (s SandboxState) IsPausable() bool  { return pausableStates[s] }
func (s SandboxState) IsResumable() bool { return resumableStates[s] }

// Sentinel errors returned by pause/resume. Callers use errors.Is to branch;
// these replace the Python SandboxStateError / SandboxPauseError raises.
var (
	ErrSandboxNotFound  = errors.New("sandbox not found")
	ErrSandboxState     = errors.New("sandbox not in expected state")
	ErrSandboxPause     = errors.New("sandbox pause failed")
	ErrSandboxResume    = errors.New("sandbox resume failed")
	ErrCheckpointNotFound = errors.New("checkpoint not found or corrupted")
)

// CheckpointRef is an opaque reference to a sandbox checkpoint.
//
// For cloud providers (Firecracker/E2B, Daytona), ProviderCheckpointID is the
// authoritative reference and LocalPath is empty.
// For Docker + CRIU, LocalPath is the directory containing checkpoint files
// and ProviderCheckpointID may be empty.
type CheckpointRef struct {
	CheckpointID         string `json:"checkpoint_id"`   // TAG-internal UUID (ckpt-*)
	SandboxID            string `json:"sandbox_id"`
	Provider             string `json:"provider"`
	ProviderCheckpointID string `json:"provider_checkpoint_id,omitempty"`
	LocalPath            string `json:"local_path,omitempty"`
	CheckpointSizeBytes  int64  `json:"checkpoint_size_bytes,omitempty"`
	BillingPaused        bool   `json:"billing_paused"`
	PausedAt             string `json:"paused_at,omitempty"`          // RFC3339 UTC
	CheckpointReadyAt    string `json:"checkpoint_ready_at,omitempty"`
}

// PauseResult is the return value from SandboxProvider.Pause.
type PauseResult struct {
	Checkpoint       CheckpointRef `json:"checkpoint"`
	PauseDurationS   float64       `json:"pause_duration_s"`
	BillingActive    bool          `json:"billing_active"`     // true = billing was NOT paused (degraded mode)
	ProviderStateRaw string        `json:"provider_state_raw"` // raw state string from provider API
	Warnings         []string      `json:"warnings,omitempty"`
}

// ResumeResult is the return value from SandboxProvider.Resume.
type ResumeResult struct {
	SandboxID        string  `json:"sandbox_id"`
	ResumeDurationS  float64 `json:"resume_duration_s"`
	BillingActive    bool    `json:"billing_active"` // true = billing is active (expected after resume)
	ProviderStateRaw string  `json:"provider_state_raw"`
	NewTimeoutS      *int    `json:"new_timeout_s,omitempty"`
}

// SandboxCheckpointRecord mirrors a row in sandbox_checkpoints. Nullable SQL
// columns use pointer fields so a missing value stays distinct from a zero.
type SandboxCheckpointRecord struct {
	ID                   string
	SandboxID            string
	Provider             string
	ProviderCheckpointID *string
	CheckpointPath       *string
	CheckpointSizeBytes  *int64
	State                string
	BillingPaused        bool
	PauseDurationS       *float64
	ResumeDurationS      *float64
	ErrorMessage         *string
	PausedAt             string
	CheckpointReadyAt    *string
	ResumedAt            *string
	CreatedAt            string
}

// ResumeOptions carries optional resume parameters (replaces the Python
// keyword-only timeout_override argument).
type ResumeOptions struct {
	TimeoutOverride *int
}

// SandboxProvider is the interface every provider implementation must satisfy.
// Extended from the PRD-028 base interface to include pause/resume.
type SandboxProvider interface {
	// Pause checkpoints the running sandbox and stops billing.
	//
	// Precondition:  sandbox is in StateRunning.
	// Postcondition: sandbox is in StatePaused; PauseResult.BillingActive == false
	//                (except SIGSTOP-only fallbacks where billing cannot be stopped).
	// Returns ErrSandboxNotFound, ErrSandboxState, or ErrSandboxPause (wrapped
	// with the provider error) on failure.
	Pause(ctx context.Context, sandboxID string) (PauseResult, error)

	// Resume restores a paused sandbox from its checkpoint.
	//
	// Precondition:  sandbox is in StatePaused and checkpoint.State == "ready".
	// Postcondition: sandbox is in StateRunning; ResumeResult.BillingActive == true.
	// Returns ErrSandboxNotFound, ErrSandboxState, ErrCheckpointNotFound, or
	// ErrSandboxResume (wrapped with the provider error) on failure.
	Resume(ctx context.Context, sandboxID string, checkpoint CheckpointRef, opts ResumeOptions) (ResumeResult, error)
}
```

### 10.4 Firecracker / E2B Provider Implementation

The Firecracker-backed provider drives pause/resume through the
`firecracker-microvm/firecracker-go-sdk` machine API. `PauseVM` +
`CreateSnapshot` implement the same semantics the Python design expressed as
`PATCH /vm {"state":"Paused"}` followed by `PUT /snapshot/create`;
`LoadSnapshot` + `ResumeVM` implement `PUT /snapshot/load ... {"resume_vm":true}`.
This replaces the Python E2B SDK `sbx.pause()` / HTTP-to-Firecracker-REST code.

```go
// internal/sandbox — firecrackerProvider.Pause and firecrackerProvider.Resume

package sandbox

import (
	"context"
	"fmt"
	"time"

	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/google/uuid"
)

// firecrackerProvider is the E2B/Firecracker microVM provider with full
// snapshot-based checkpoint support.
type firecrackerProvider struct {
	machine *fcsdk.Machine // resolved per sandbox via connect(sandboxID)
	snapDir string         // provider-managed snapshot storage
}

func (p *firecrackerProvider) Pause(ctx context.Context, sandboxID string) (PauseResult, error) {
	t0 := time.Now()
	checkpointID := "ckpt-" + uuid.NewString()[:8]

	m, err := p.connect(ctx, sandboxID)
	if err != nil {
		return PauseResult{}, fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
	}

	// Step 1: PATCH /vm {"state":"Paused"} — pause the microVM.
	if err := m.PauseVM(ctx); err != nil {
		return PauseResult{}, fmt.Errorf("%w: pause vm: %v", ErrSandboxPause, err)
	}

	// Step 2: PUT /snapshot/create {"snapshot_type":"Full", ...} — full snapshot.
	memPath := fmt.Sprintf("%s/%s.mem", p.snapDir, checkpointID)
	snapPath := fmt.Sprintf("%s/%s.snap", p.snapDir, checkpointID)
	if err := m.CreateSnapshot(ctx, memPath, snapPath); err != nil {
		return PauseResult{}, fmt.Errorf("%w: create snapshot: %v", ErrSandboxPause, err)
	}

	// Step 3: GET /vm — poll for Paused / billing confirmation with a deadline.
	pollCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	billingActive := true
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		info, err := m.DescribeInstance(pollCtx)
		if err == nil && info.State == "Paused" {
			billingActive = info.BillingActive
			break
		}
		select {
		case <-pollCtx.Done():
			return PauseResult{}, fmt.Errorf("%w: billing confirmation timed out", ErrSandboxPause)
		case <-ticker.C:
		}
	}

	now := utcNow()
	ckpt := CheckpointRef{
		CheckpointID:         checkpointID,
		SandboxID:            sandboxID,
		Provider:             "e2b",
		ProviderCheckpointID: snapPath,
		BillingPaused:        !billingActive,
		PausedAt:             now,
		CheckpointReadyAt:    now,
	}
	return PauseResult{
		Checkpoint:       ckpt,
		PauseDurationS:   time.Since(t0).Seconds(),
		BillingActive:    billingActive,
		ProviderStateRaw: "Paused",
	}, nil
}

func (p *firecrackerProvider) Resume(ctx context.Context, sandboxID string, checkpoint CheckpointRef, opts ResumeOptions) (ResumeResult, error) {
	t0 := time.Now()

	m, err := p.connect(ctx, sandboxID)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
	}

	// PUT /snapshot/load {"snapshot_path":..., "resume_vm":true} then confirm Running.
	if err := m.LoadSnapshot(ctx, checkpoint.ProviderCheckpointID); err != nil {
		return ResumeResult{}, fmt.Errorf("%w: load snapshot: %v", ErrCheckpointNotFound, err)
	}
	if err := m.ResumeVM(ctx); err != nil {
		return ResumeResult{}, fmt.Errorf("%w: resume vm: %v", ErrSandboxResume, err)
	}

	pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := m.DescribeInstance(pollCtx)
		if err == nil && info.State == "Running" {
			break
		}
		select {
		case <-pollCtx.Done():
			return ResumeResult{}, fmt.Errorf("%w: did not reach Running", ErrSandboxResume)
		case <-ticker.C:
		}
	}

	return ResumeResult{
		SandboxID:        sandboxID,
		ResumeDurationS:  time.Since(t0).Seconds(),
		BillingActive:    true,
		ProviderStateRaw: "Running",
		NewTimeoutS:      opts.TimeoutOverride,
	}, nil
}
```

### 10.5 Docker + CRIU Provider Implementation

The Docker-backed provider talks to the daemon through the moby Go client
(`github.com/docker/docker/client`). `CheckpointCreate` drives CRIU
checkpoint-to-disk; `ContainerPause` / `ContainerUnpause` provide the
SIGSTOP-equivalent fallback. Calls to the `docker` CLI via `os/exec` are a last
resort only (e.g. daemon flags the client does not expose). CRIU availability
is feature-detected and the provider degrades off-Linux (CRIU is Linux-only).

```go
// internal/sandbox — dockerProvider.Pause with CRIU / SIGSTOP fallback

package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/docker/docker/api/types/checkpoint"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
)

// dockerProvider is the Docker container provider with CRIU checkpoint or
// SIGSTOP fallback.
type dockerProvider struct {
	cli  client.APIClient
	db   Store // resolves sandboxID -> provider container ID

	criuOnce sync.Once
	criuOK   bool
}

// checkCRIU feature-detects the criu binary once and caches the result.
func (p *dockerProvider) checkCRIU() bool {
	p.criuOnce.Do(func() {
		_, err := exec.LookPath("criu")
		p.criuOK = err == nil
	})
	return p.criuOK
}

func (p *dockerProvider) Pause(ctx context.Context, sandboxID string) (PauseResult, error) {
	t0 := time.Now()
	checkpointID := "ckpt-" + uuid.NewString()[:8]

	containerID, err := p.db.ContainerID(ctx, sandboxID)
	if err != nil {
		return PauseResult{}, fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
	}

	var (
		warnings      []string
		ckptSize      int64
		localPath     string
		providerState string
	)

	if p.checkCRIU() {
		// Full CRIU checkpoint: captures memory + process tree to disk.
		home, _ := os.UserHomeDir()
		ckptDir := filepath.Join(home, ".tag", "checkpoints", sandboxID)
		if err := os.MkdirAll(ckptDir, 0o700); err != nil {
			return PauseResult{}, fmt.Errorf("%w: mkdir checkpoint dir: %v", ErrSandboxPause, err)
		}

		opts := checkpoint.CreateOptions{
			CheckpointID:  checkpointID,
			CheckpointDir: ckptDir,
			Exit:          true, // stop the container after checkpoint creation
		}
		if err := p.cli.CheckpointCreate(ctx, containerID, opts); err != nil {
			return PauseResult{}, fmt.Errorf("%w: checkpoint create: %v", ErrSandboxPause, err)
		}

		// Measure checkpoint size by walking the checkpoint directory.
		_ = filepath.WalkDir(ckptDir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if fi, ferr := d.Info(); ferr == nil {
					ckptSize += fi.Size()
				}
			}
			return nil
		})
		localPath = filepath.Join(ckptDir, checkpointID)
		providerState = "exited"
	} else {
		// SIGSTOP fallback — no disk checkpoint; billing NOT paused for cloud Docker.
		if err := p.cli.ContainerPause(ctx, containerID); err != nil {
			return PauseResult{}, fmt.Errorf("%w: container pause: %v", ErrSandboxPause, err)
		}
		providerState = "paused"
		warnings = append(warnings,
			"CRIU not available. Container is suspended (SIGSTOP) but checkpoint "+
				"is in-memory only. Cross-session resume and billing pause are not "+
				"supported. Install CRIU for full checkpoint support.")
	}

	ckpt := CheckpointRef{
		CheckpointID:        checkpointID,
		SandboxID:           sandboxID,
		Provider:            "docker",
		LocalPath:           localPath,
		CheckpointSizeBytes: ckptSize,
		BillingPaused:       false, // local Docker billing is always user-controlled
		PausedAt:            utcNow(),
	}
	return PauseResult{
		Checkpoint:       ckpt,
		PauseDurationS:   time.Since(t0).Seconds(),
		BillingActive:    true, // local Docker runs on the user's machine
		ProviderStateRaw: providerState,
		Warnings:         warnings,
	}, nil
}

func (p *dockerProvider) Resume(ctx context.Context, sandboxID string, checkpoint CheckpointRef, opts ResumeOptions) (ResumeResult, error) {
	t0 := time.Now()

	containerID, err := p.db.ContainerID(ctx, sandboxID)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
	}

	if checkpoint.LocalPath != "" {
		// CRIU restore: start the container from its checkpoint directory.
		ckptDir := filepath.Dir(checkpoint.LocalPath)
		startOpts := container.StartOptions{
			CheckpointID:  checkpoint.CheckpointID,
			CheckpointDir: ckptDir,
		}
		if err := p.cli.ContainerStart(ctx, containerID, startOpts); err != nil {
			return ResumeResult{}, fmt.Errorf("%w: start from checkpoint: %v", ErrSandboxResume, err)
		}
	} else {
		// SIGSTOP resume via SIGCONT (container unpause).
		if err := p.cli.ContainerUnpause(ctx, containerID); err != nil {
			return ResumeResult{}, fmt.Errorf("%w: container unpause: %v", ErrSandboxResume, err)
		}
	}

	return ResumeResult{
		SandboxID:        sandboxID,
		ResumeDurationS:  time.Since(t0).Seconds(),
		BillingActive:    true,
		ProviderStateRaw: "running",
	}, nil
}
```

The restricted subprocess backend (see FR-07) is a third `SandboxProvider`
implementation: it starts the managed process with
`&syscall.SysProcAttr{Setpgid: true}`, pauses via
`syscall.Kill(-pgid, syscall.SIGSTOP)`, and resumes via `syscall.SIGCONT`,
all under a cancellable `context.Context`. It keeps no on-disk checkpoint and so
does not support cross-session resume (NG8).

### 10.6 Sandbox State Machine

```
         ┌──────────┐
         │ creating │
         └────┬─────┘
              │
         ┌────▼─────┐
         │ starting │
         └────┬─────┘
              │
         ┌────▼─────┐   pause()   ┌─────────┐
    ────► │ running │ ──────────► │ pausing │
         └────┬─────┘             └────┬────┘
              │                        │ checkpoint written
              │ kill()           ┌─────▼────┐
         ┌────▼─────┐           │  paused  │ ◄── list --paused shows this state
         │ killing  │           └─────┬────┘
         └────┬─────┘                 │ resume()
              │              ┌────────▼───────┐
         ┌────▼─────┐        │   resuming     │
         │  killed  │        └────────┬───────┘
         └──────────┘                 │
                              ┌───────▼──────┐
                              │   running    │ (same node, timeout reset)
                              └──────────────┘

         ┌───────┐
         │ error │ ◄── any state may transition to error on provider API failure
         └───────┘
```

### 10.7 Budget Integration

In `internal/budget`, the existing sandbox cost summation is modified to join
`sandbox_billing_intervals` and sum only rows where `interval_type = 'active'`.
The Go implementation uses `database/sql` with `QueryContext` and parses the
RFC3339 timestamps with `time.Parse`:

```go
// internal/budget — active (non-paused) seconds for a sandbox in a time range.

func activeSandboxSeconds(ctx context.Context, db *sql.DB, sandboxID, start, end string) (float64, error) {
	const q = `
		SELECT
			MAX(started_at, :start) AS effective_start,
			MIN(COALESCE(ended_at, :end), :end) AS effective_end
		FROM sandbox_billing_intervals
		WHERE sandbox_id = :sandbox_id
		  AND interval_type = 'active'
		  AND started_at < :end
		  AND (ended_at IS NULL OR ended_at > :start)`

	rows, err := db.QueryContext(ctx, q,
		sql.Named("sandbox_id", sandboxID),
		sql.Named("start", start),
		sql.Named("end", end),
	)
	if err != nil {
		return 0, fmt.Errorf("query active intervals: %w", err)
	}
	defer rows.Close()

	var total float64
	for rows.Next() {
		var effStart, effEnd string
		if err := rows.Scan(&effStart, &effEnd); err != nil {
			return 0, err
		}
		if effEnd == "" || effEnd <= effStart {
			continue
		}
		tStart, err := time.Parse(time.RFC3339, effStart)
		if err != nil {
			return 0, err
		}
		tEnd, err := time.Parse(time.RFC3339, effEnd)
		if err != nil {
			return 0, err
		}
		total += tEnd.Sub(tStart).Seconds()
	}
	return total, rows.Err()
}
```

### 10.8 Queue Worker Integration

```go
// internal/queue — inter-phase pause logic

func (w *Worker) runJobWithPhases(ctx context.Context, job Job, sandboxID string) error {
	pauseBetween := job.PauseBetweenPhases
	phases := job.Phases
	if len(phases) == 0 {
		phases = []Phase{job.AsSinglePhase()} // single-phase jobs wrapped in slice
	}

	for i, phase := range phases {
		if err := w.runPhase(ctx, phase, sandboxID); err != nil {
			return err
		}

		if pauseBetween && i < len(phases)-1 {
			gate := phase.Gate
			if gate.Type == "" {
				gate = Gate{Type: "immediate"}
			}

			// Pause the sandbox between phases.
			pauseResult, err := w.sandbox.Pause(ctx, sandboxID)
			if err != nil {
				return fmt.Errorf("pause after phase %d: %w", i, err)
			}
			w.logf("[job:%s] sandbox paused after phase %d; billing_active=%t; checkpoint=%s",
				job.ID, i, pauseResult.BillingActive, pauseResult.Checkpoint.CheckpointID)

			// Wait for gate condition (context deadline / cancellation aware).
			if err := w.waitForGate(ctx, gate, job); err != nil {
				return fmt.Errorf("gate wait after phase %d: %w", i, err)
			}

			// Resume before next phase.
			ckpt, err := w.store.LatestCheckpoint(ctx, sandboxID)
			if err != nil {
				return fmt.Errorf("load checkpoint: %w", err)
			}
			if _, err := w.sandbox.Resume(ctx, sandboxID, ckpt, ResumeOptions{}); err != nil {
				return fmt.Errorf("resume for phase %d: %w", i+1, err)
			}
			w.logf("[job:%s] sandbox resumed for phase %d", job.ID, i+1)
		}
	}
	return nil
}
```

The gate-wait loop and any concurrent phase work use goroutines coordinated by
`golang.org/x/sync/errgroup` and channels; the polling deadline is enforced with
`context.WithTimeout` rather than an async sleep loop.

---

## 11. Security Considerations

1. **Checkpoint file permissions:** Checkpoint directories created under `~/.tag/checkpoints/<sandbox-id>/` must be created with mode `0700` (owner read/write/execute only). CRIU checkpoint files contain full process memory dumps including any secrets that were in the process's address space at pause time. Mode `0700` ensures other local users cannot read them.

2. **Cloud checkpoint access control:** For E2B and Daytona, checkpoint data resides in provider-managed storage. The TAG CLI must not log or persist provider checkpoint IDs in plaintext files accessible to other users. `sandbox_checkpoints.provider_checkpoint_id` in SQLite is only accessible to the DB owner since `~/.tag/runtime/tag.sqlite3` is created with `0600` permissions (enforced by the database open path in `internal/store`).

3. **API key non-exposure in pause/resume:** The `E2B_API_KEY`, `DAYTONA_API_KEY`, and Docker credentials are loaded from the environment or keychain at runtime. They must not appear in `sandbox_checkpoints` rows, `--json` output, or OTel span attributes. The tracing integration must explicitly exclude credential-bearing attributes from spans.

4. **CRIU privilege requirement:** `docker checkpoint create` requires either the Docker daemon to run as root (standard Linux Docker installation) or the user to be in the `docker` group. TAG must not require `sudo` or elevated privileges beyond what Docker already requires. If the user lacks Docker group membership, the checkpoint command will fail with a clear permission error; TAG surfaces the message `"Run: sudo usermod -aG docker $USER and restart your session"`.

5. **Checkpoint injection prevention:** `tag sandbox resume` must validate that the `sandbox_id` in the `CheckpointRef` matches the `sandbox_id` argument. An attacker with write access to `~/.tag/checkpoints/` could not redirect a resume to a different sandbox by swapping checkpoint directories, because the sandbox ID is embedded in the SQLite record and cross-checked before the `docker start --checkpoint` call.

6. **Path traversal in `--checkpoint-dir`:** The `--checkpoint-dir` argument is normalized with `filepath.Clean`, resolved to an absolute path, and dereferenced through `filepath.EvalSymlinks`; the result is then prefix/pattern-matched (reusing the validation in `internal/credentials` / `internal/sandbox`) to ensure it is not a parent of `~/.ssh`, `~/.aws`, `~/.config`, `/etc`, `/proc`, `/sys`, or `/dev`. This prevents a malicious job from checkpointing into a sensitive directory via path components like `../../.ssh/` or a symlink.

7. **Credential scrubbing from checkpoint output:** The `--json` output of `tag sandbox pause` must not include environment variable contents, even if the sandbox's environment contained credentials. The JSON schema is fixed to the fields defined in Section 7.1; no environment dump or process memory excerpt is ever included.

8. **OTel span data minimization:** The `sandbox.pause` and `sandbox.resume` OTel spans must set only the attributes listed in FR-18. The `sandbox.id` attribute must use the TAG-internal opaque ID, not any provider URL or credential-bearing endpoint. The `sandbox.checkpoint_id` attribute must use the TAG-internal `ckpt-*` ID, not the provider checkpoint ID.

---

## 12. Testing Strategy

### 12.1 Unit Tests

Located in `internal/sandbox/pause_resume_test.go` (plus `internal/budget` and
`internal/queue` test files). Tests use the standard `testing` package with
table-driven cases; providers are injected as the `SandboxProvider` interface so
fakes replace real backends (dependency injection, not monkeypatching).

| Test | Description |
|------|-------------|
| `TestPauseStateValidation` | Table-driven: verify `Pause()` returns an error satisfying `errors.Is(err, ErrSandboxState)` when the sandbox is in `killed`, `paused`, `pausing`, or `error` state. |
| `TestResumeStateValidation` | Verify `Resume()` returns `ErrSandboxState` when the sandbox is not in `paused` state. |
| `TestCheckpointRefSerialization` | Round-trip `CheckpointRef` to and from the `sandbox_checkpoints` SQLite row via `SandboxCheckpointRecord`. |
| `TestBillingIntervalActiveSeconds` | Given a synthetic `sandbox_billing_intervals` table with known active/paused intervals, verify `activeSandboxSeconds()` returns the correct sum. |
| `TestSQLiteAtomicityPause` | Simulate a failure mid-transaction (after the `sandbox_runs` update, before the `sandbox_checkpoints` insert) and verify that `tx.Rollback()` leaves both tables in the pre-pause state. |
| `TestCRIUUnavailableFallback` | Inject a fake CRIU-detector returning false; verify `dockerProvider.Pause()` calls `ContainerPause`, returns a `PauseResult` with `BillingActive=true`, and includes the SIGSTOP warning. |
| `TestCRIUCheckpointDirPermissions` | Verify that the checkpoint directory is created with mode `0700` (check via `os.Stat` and `FileInfo.Mode().Perm()`). |
| `TestPauseResultJSONNoCredentials` | Marshal the pause result to JSON and verify it contains none of the keys `env`, `environ`, `secrets`, `api_key`, or any `*TOKEN*` / `*SECRET*` keys. |
| `TestQueueWorkerPauseBetweenPhases` | Table-driven test of `runJobWithPhases()` with a two-phase job and `PauseBetweenPhases=true`, using a mock `SandboxProvider`; verify `Pause` and `Resume` are each called exactly once, in the correct order. |
| `TestBudgetExcludesPausedIntervals` | Verify that the cost roll-up (in-memory SQLite) reports only active-interval seconds and excludes paused intervals. |

### 12.2 Integration Tests

Located in `internal/sandbox/integration_test.go`, guarded by a `//go:build integration` build tag (and `testing.Short()` skips). These tests require a running Docker daemon (CI matrix: ubuntu-latest with Docker pre-installed).

| Test | Description |
|------|-------------|
| `TestDockerSIGSTOPPauseResume` | Start a Docker sandbox running `sleep 3600`; pause it; verify `ContainerInspect` shows `Paused: true`; resume; verify `Paused: false`; verify the process is still alive. |
| `TestDockerCRIUCheckpointRestore` | (Skipped via build tag / detector if CRIU not installed.) Start a Docker sandbox; write a file to `/tmp/testfile`; pause with CRIU; verify the checkpoint directory exists and has files; resume; verify `/tmp/testfile` still exists. |
| `TestSQLiteStateAfterPauseResume` | After a full pause/resume cycle on a Docker sandbox, query `sandbox_runs` and `sandbox_checkpoints` and verify: `sandbox_runs.state='running'`, `pause_count=1`, `resume_count=1`, and one `sandbox_checkpoints` row with `state='restored'`. |
| `TestListPausedNoProviderCalls` | Pause a Docker sandbox; inject a Docker client stub whose calls error; verify `tag sandbox list --paused` still returns the paused sandbox from SQLite without error. |
| `TestBillingIntervalRows` | After pause then resume, verify two `sandbox_billing_intervals` rows exist: one `active` interval (started_at = sandbox creation, ended_at = pause time) and one `paused` interval (started_at = pause time, ended_at = resume time). |

### 12.3 E2B Integration Tests (Optional, Requires E2B_API_KEY)

Located in `internal/sandbox/e2b_integration_test.go`, guarded by a `//go:build e2b` build tag; skipped in CI unless `E2B_API_KEY` is set (checked at test start with an env-var skip).

| Test | Description |
|------|-------------|
| `TestE2BPauseBillingStop` | Create an E2B sandbox; pause; verify `BillingActive=false` in the pause result; resume; verify `BillingActive=true` in the resume result. |
| `TestE2BFilesystemStateAfterResume` | Create sandbox; write `/home/user/test.txt`; pause; resume; verify `/home/user/test.txt` exists. |
| `TestE2BProcessStateAfterResume` | Create sandbox; start a background process; pause; resume; verify the process is still listed in `ps aux`. |
| `TestE2BPauseDurationWithinSLA` | Pause a fresh E2B sandbox (default 512 MB); verify `PauseDurationS < 6.0`. |
| `TestE2BResumeDurationWithinSLA` | Resume an E2B sandbox from checkpoint; verify `ResumeDurationS < 3.0`. |

### 12.4 Performance Tests

Located in `internal/sandbox/pause_perf_test.go`, guarded by the `//go:build perf` build tag. Implemented as a table-driven timing test (a `go test -bench` benchmark form is equivalent) that measures the median over 10 sequential cycles.

```go
//go:build perf

package sandbox

import (
	"context"
	"sort"
	"testing"
	"time"
)

// TestPauseResumeThroughput verifies the pause + resume cycle stays within SLA
// across 10 sequential cycles.
func TestPauseResumeThroughput(t *testing.T) {
	ctx := context.Background()
	var pauseTimes, resumeTimes []float64

	for i := 0; i < 10; i++ {
		sbxID := createTestSandbox(t)

		res, err := pauseSandbox(ctx, sbxID)
		if err != nil {
			t.Fatalf("pause: %v", err)
		}
		pauseTimes = append(pauseTimes, res.PauseDurationS)

		t1 := time.Now()
		ckpt := latestCheckpoint(t, sbxID)
		if _, err := resumeSandbox(ctx, sbxID, ckpt); err != nil {
			t.Fatalf("resume: %v", err)
		}
		resumeTimes = append(resumeTimes, time.Since(t1).Seconds())

		killSandbox(t, sbxID)
	}

	if m := median(pauseTimes); m >= 6.0 {
		t.Errorf("median pause time %.2fs exceeds 6s SLA", m)
	}
	if m := median(resumeTimes); m >= 3.0 {
		t.Errorf("median resume time %.2fs exceeds 3s SLA", m)
	}
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
```

---

## 13. Acceptance Criteria

| ID | Criterion | Verified by |
|----|-----------|-------------|
| AC-01 | `tag sandbox pause <id>` on a running E2B sandbox completes with exit code 0, sets `state=paused` in SQLite, and has `billing_active: false` in `--json` output within 60 s. | E2B integration test |
| AC-02 | `tag sandbox resume <id>` on a paused E2B sandbox restores the sandbox to `state=running` with full filesystem and process state preserved. | E2B filesystem + process integration tests |
| AC-03 | `tag sandbox pause <id>` on a Docker sandbox with CRIU creates a checkpoint directory at `~/.tag/checkpoints/<id>/` with mode `0700` and at least one checkpoint file. | Docker CRIU integration test |
| AC-04 | `tag sandbox pause <id>` on a Docker sandbox without CRIU exits with code 5, prints a warning containing `SIGSTOP`, and sets `billing_paused=0` in SQLite. | Unit test `TestCRIUUnavailableFallback` |
| AC-05 | `tag sandbox list --paused --json` returns the correct list of paused sandboxes from SQLite and completes in < 200 ms without making any provider API calls. | Integration test `TestListPausedNoProviderCalls` |
| AC-06 | `tag sandbox status <id> --json` returns `billing_active: false` for a paused E2B sandbox and `billing_active: true` after resume. | E2B integration test |
| AC-07 | `tag costs` correctly excludes paused intervals: a sandbox that was active for 1 hour, paused for 1 hour, then active for 1 hour is billed for exactly 2 hours ± 5 s. | Unit test `TestBudgetExcludesPausedIntervals` |
| AC-08 | An `internal/queue` job with `pause_between_phases: true` and two phases: (a) pauses the sandbox after phase 1, (b) waits for the gate, (c) resumes before phase 2, without any manual intervention. | Queue worker unit test |
| AC-09 | `tag sandbox pause <id>` on a sandbox in `killed` or `error` state exits with code 1 and prints an error message containing the current state. | Unit test `TestPauseStateValidation` |
| AC-10 | `tag sandbox resume <id>` on a sandbox not in `paused` state exits with code 1 and prints an error message containing the current state. | Unit test `TestResumeStateValidation` |
| AC-11 | OTel spans for `sandbox.pause` and `sandbox.resume` are emitted with attributes `sandbox.id`, `sandbox.provider`, `sandbox.operation`, `sandbox.checkpoint_id`, and `sandbox.pause_duration_s` or `sandbox.resume_duration_s`. | Unit test with an in-memory OTel span recorder (tracetest) from `internal/obs` |
| AC-12 | The `--json` output of `tag sandbox pause` contains none of the fields `env`, `environ`, `api_key`, or any key matching `*TOKEN*` or `*SECRET*`. | Unit test `TestPauseResultJSONNoCredentials` |
| AC-13 | A sandbox paused in session A can be resumed in a new TAG CLI session B (same machine, Docker + CRIU) by running `tag sandbox resume <id>`. | Docker CRIU integration test |
| AC-14 | For E2B, the median `pause_duration_s` across 10 iterations for a 512 MB sandbox is < 6.0 s. | Performance test |
| AC-15 | For E2B, the median `resume_duration_s` across 10 iterations is < 3.0 s. | Performance test |

---

## 14. Dependencies

| Dependency | Type | Purpose | Notes |
|------------|------|---------|-------|
| PRD-028 Sandbox Code Execution | Required predecessor | Provides `sandbox_runs` table, `SandboxProvider` interface base, schema-migration hook, Docker/E2B/Modal provider implementations, and existing `tag sandbox` CLI namespace | Must be implemented first; this PRD extends PRD-028 |
| PRD-013 Agent Tracing & Observability | Required | OTel span emission for `sandbox.pause` / `sandbox.resume` operations | Span attributes defined in Section FR-18 |
| PRD-034 Security | Required | Credential pattern blocklist, mount path validation, DB file permission enforcement in `internal/store` | The mount/path validation from PRD-034 is called in `Pause()` to block checkpoint-dir path traversal |
| PRD-012 Cost Tracking & Budget | Required | Budget roll-up must exclude paused intervals; `sandbox_billing_intervals` table joins into cost calculation | `internal/budget` modification in Section 10.7 |
| PRD-008 Background Task Queue | Soft dependency | Queue worker integration for `pause_between_phases`; not required for core pause/resume commands | Queue integration in Section 10.8 |
| `github.com/firecracker-microvm/firecracker-go-sdk` | Required Go module | Firecracker microVM machine API: `PauseVM` / `ResumeVM` + `CreateSnapshot` / `LoadSnapshot` for E2B snapshot pause/resume | Replaces the Python `e2b` SDK; needs `/dev/kvm` at runtime |
| `github.com/docker/docker/client` (moby) | Required Go module | Docker daemon client: `ContainerPause` / `ContainerUnpause` and `CheckpointCreate` (CRIU) | Replaces `docker` subprocess calls from the Python design |
| `modernc.org/sqlite` | Required Go module | Pure-Go SQLite driver (`CGO_ENABLED=0`, FTS5 built-in) backing `sandbox_checkpoints` / `sandbox_billing_intervals` | Shared with the rest of the Go build |
| `go.opentelemetry.io/otel` | Required Go module | Tracer/span API for `sandbox.pause` / `sandbox.resume` instrumentation in `internal/obs` | — |
| `github.com/spf13/cobra` | Required Go module | Command framework for the `sandbox pause` / `sandbox resume` subcommands in `internal/cli` | — |
| `github.com/google/uuid` | Required Go module | Generates the TAG-internal `ckpt-*` / `sbx-*` identifiers | — |
| CRIU (Checkpoint/Restore In Userspace) | Optional system/host binary | Required for full Docker checkpoint-to-disk. Install: `sudo apt install criu` on Ubuntu; not available on macOS — Docker+CRIU is Linux-only | TAG gracefully degrades to SIGSTOP when absent |
| Docker Engine ≥ 23.0 | Optional runtime/host dependency | Checkpoint create requires experimental features enabled in `daemon.json` on Docker CE: `{"experimental": true}` | Already required by PRD-028 Docker backend |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should `tag sandbox pause --all` be a supported shorthand to pause every running sandbox at once (e.g., at end of day)? Or does the risk of mass-pausing production sandboxes outweigh the convenience? | Product | Pre-beta |
| OQ-02 | E2B's `Sandbox.resume()` returns a new sandbox object with a potentially different sandbox ID. How should TAG handle the sandbox ID change — update the row in `sandbox_runs` in place, or create a new row and mark the old one as `superseded`? | Engineering | Before E2B integration test |
| OQ-03 | Should `sandbox_checkpoints` rows with `state='failed'` be automatically retried? If so, what is the retry policy (max attempts, backoff) and does the user get notified on each retry attempt? | Engineering | Before queue integration |
| OQ-04 | CRIU on Linux requires the kernel to be compiled with `CONFIG_CHECKPOINT_RESTORE=y`. Docker Desktop on macOS does not support CRIU at all (no Linux kernel on Mac). Should the macOS Docker path always warn that pause will use SIGSTOP and never attempt CRIU, regardless of whether `criu` binary is on PATH? | Engineering | Before macOS integration test |
| OQ-05 | Daytona's pause API may require the workspace to have no active PTY sessions. Should TAG automatically close any open `tag sandbox exec` PTY sessions before issuing a pause, or fail with an actionable error? | Engineering | Before Daytona integration |
| OQ-06 | What is the maximum checkpoint size TAG should allow before warning the user? A 16 GiB RAM sandbox would produce a ~16 GiB checkpoint file on a Docker+CRIU system. Should there be a `--max-checkpoint-size` guard? | Product | Pre-alpha |
| OQ-07 | The E2B research finding states "After resume, timeout resets to max(5 min, original creation value)." Should TAG always override this with `--timeout` on resume to prevent unexpected session expiry for users who resume after a long pause? | Engineering | Before E2B integration test |
| OQ-08 | Should `tag sandbox checkpoints prune --older-than 7d` be a manual command only, or should a cron job (via `cron_scheduler.py`) run it automatically? If automatic, what is the default retention policy? | Product | Post-launch |

---

## 16. Complexity and Timeline

**Total estimated effort:** M — 1.5 to 2 weeks for a single engineer with Docker and Go experience. The E2B/Firecracker and Daytona integrations add risk due to provider API surface; allow buffer if the SDK/machine APIs do not yet expose pause natively.

### Phase 1 — Schema and Protocol Foundation (Days 1–2)

- Add `SandboxState` constants, `CheckpointRef`, `PauseResult`, `ResumeResult`, `SandboxCheckpointRecord` structs to `internal/sandbox`.
- Write and apply SQL DDL for `sandbox_checkpoints`, `sandbox_billing_intervals`, and the guarded `ALTER TABLE sandbox_runs` column migrations (each ignoring the `duplicate column name` error).
- Extend the `SandboxProvider` interface with `Pause()` and `Resume()` methods.
- Write unit tests for struct/JSON round-tripping and SQLite atomicity (AC-09, AC-10, subset of AC-07).

### Phase 2 — Docker Provider Implementation (Days 3–5)

- Implement `dockerProvider.Pause()` with CRIU path and SIGSTOP fallback (via the moby `CheckpointCreate` / `ContainerPause` client calls).
- Implement `dockerProvider.Resume()` with CRIU restore (`ContainerStart` from checkpoint) and `ContainerUnpause` fallback.
- Implement CRIU availability check (`checkCRIU()` via `exec.LookPath`), checkpoint directory creation with `0700` permissions, size computation.
- Add the `sandbox pause` and `sandbox resume` cobra commands to `internal/cli` with full flag binding.
- Write Docker integration tests (AC-03, AC-04, AC-13).

### Phase 3 — E2B Provider Implementation (Days 6–8)

- Implement `firecrackerProvider.Pause()` using `PauseVM` + `CreateSnapshot` from `firecracker-go-sdk`.
- Implement `firecrackerProvider.Resume()` using `LoadSnapshot` + `ResumeVM`.
- Handle sandbox ID change on resume (OQ-02 resolution required before this phase completes).
- Write E2B integration tests (AC-01, AC-02, AC-06, AC-14, AC-15) — gated behind the `e2b` build tag and `E2B_API_KEY`.

### Phase 4 — CLI, Budget, and Queue Integration (Days 9–11)

- Extend `tag sandbox list` with `--paused` and `--all` flags; update output table columns.
- Extend `tag sandbox status` with checkpoint metadata in `--json` output.
- Update `internal/budget` with `activeSandboxSeconds()` and the `sandbox_billing_intervals` join (AC-07).
- Add `pause_between_phases` support to `internal/queue` (AC-08).
- Implement OTel span emission in `internal/obs` for pause/resume (AC-11).

### Phase 5 — Security Hardening and Checkpoint Pruning (Days 12–14)

- Enforce `0700` checkpoint directory permissions and verify path traversal guard in `--checkpoint-dir`.
- Verify `--json` output exclusion of credential fields (AC-12).
- Implement `tag sandbox checkpoints prune` subcommand.
- Run full security checklist from Section 11.
- Complete performance tests and verify E2B SLA targets are met (AC-14, AC-15).
- Update `docs/prd/INDEX.md` to add PRD-095 to the priority matrix.

---

*End of PRD-095*

