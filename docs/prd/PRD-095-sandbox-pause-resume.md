# PRD-095: Sandbox Pause/Resume with Billing Pause (`tag sandbox pause / tag sandbox resume`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py`
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

All pause/resume subcommands extend the existing `tag sandbox` namespace established in PRD-028. The CLI parser additions live in `controller.py` in the `_build_sandbox_parser()` function, following the same argparse subparser pattern as `tag sandbox run` and `tag sandbox kill`.

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
| FR-02 | For E2B-backed sandboxes, pause must use the Firecracker REST API sequence: `PATCH /vm {"state": "Paused"}` → `PUT /snapshot/create {"snapshot_type": "Full", "mem_file_path": "<path>", "snapshot_path": "<path>"}` → confirm with `GET /vm` that state is `Paused`. |
| FR-03 | For E2B-backed sandboxes, resume must use: `PUT /snapshot/load {"snapshot_path": "<path>", "mem_file_path": "<path>", "enable_diff_snapshots": false, "resume_vm": true}` and confirm with `GET /vm` that state is `Running`. |
| FR-04 | For Daytona-backed sandboxes, pause must call the Daytona workspace pause API and confirm the workspace enters `Stopped` state within 30 seconds. |
| FR-05 | For Docker + CRIU, pause must: (a) check for `criu` binary on PATH; (b) run `docker checkpoint create <container-id> <checkpoint-name>`; (c) write the checkpoint to `~/.tag/checkpoints/<sandbox-id>/`; (d) stop the container after checkpoint creation. |
| FR-06 | For Docker without CRIU, pause must: (a) run `docker pause <container-id>` (SIGSTOP); (b) set `state = 'paused'` and `billing_paused = false` in the SQLite record; (c) exit with code 5 and print `Warning: CRIU not available. Container is suspended (SIGSTOP) but billing is NOT paused. Install CRIU for full checkpoint support.` |
| FR-07 | For the restricted subprocess backend, pause must send `os.killpg(os.getpgid(pid), signal.SIGSTOP)` to the process group of the managed subprocess. Resume sends `SIGCONT`. |
| FR-08 | Every pause operation must write a row to `sandbox_checkpoints` (schema in Section 9.2) within the same SQLite transaction as updating the corresponding `sandbox_runs.state` to `'paused'`. Both updates must be atomic; if either fails, neither is committed. |
| FR-09 | Every resume operation must update `sandbox_runs.state` to `'running'`, set `sandbox_checkpoints.resumed_at` to the current UTC timestamp, and increment `sandbox_runs.resume_count`. |
| FR-10 | `tag sandbox list --paused` must read exclusively from the local `sandbox_runs` + `sandbox_checkpoints` tables; it must not make any provider API calls. Response must complete in < 200 ms on a warm SQLite connection. |
| FR-11 | `tag sandbox status <id> --json` must merge local SQLite state with a live provider API call to fetch `provider_state_raw` and `billing_active`. If the provider API call fails, the command must return local state with `"provider_state_raw": null, "provider_api_error": "<message>"` and exit code 0 (local state is still useful). |
| FR-12 | The budget module (`budget.py`) must be updated to exclude `paused_at → resumed_at` intervals from cost roll-up. The `cost_seconds` field in `sandbox_runs` must reflect only active (non-paused) compute time. |
| FR-13 | `pause_duration_s` (wall-clock time from pause request to provider confirmation) must be measured and stored in `sandbox_checkpoints.pause_duration_s`. Similarly `resume_duration_s`. Both are included in `--json` output. |
| FR-14 | The `SandboxProvider` protocol (Section 9.3) must define `pause(sandbox_id: str) -> CheckpointRef` and `resume(sandbox_id: str, checkpoint: CheckpointRef) -> SandboxHandle` as abstract methods. All provider implementations must implement both. |
| FR-15 | If a sandbox is killed or errors while in `pausing` state, the partial checkpoint must be cleaned up (provider API call to delete snapshot, local files removed) and the sandbox transitioned to `error` state with `error_message` set. |
| FR-16 | `tag sandbox pause` with `--wait` (the default) must poll for provider confirmation of billing stop at 1-second intervals with a 60-second hard timeout. If the 60-second timeout elapses without billing confirmation, the command exits with code 2 and leaves the sandbox in `pausing` state in SQLite so the operator can re-query. |
| FR-17 | The `queue_worker.py` integration must check for a `pause_between_phases` boolean in the job config. When true, after each phase completes, `queue_worker.py` calls `sandbox.pause(sandbox_id)`, enters a polling loop on a gate condition (configurable: time delay, external HTTP poll, or SQLite flag), and calls `sandbox.resume(sandbox_id)` when the gate clears. |
| FR-18 | Pause and resume operations must emit OpenTelemetry spans (via `tracing.py`) with attributes: `sandbox.id`, `sandbox.provider`, `sandbox.operation` (`pause` or `resume`), `sandbox.checkpoint_id`, `sandbox.checkpoint_size_bytes`, `sandbox.pause_duration_s`. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Pause latency (E2B):** End-to-end pause time (CLI invocation to `billing_active=false` confirmed) must be < 6 s for sandboxes with ≤ 512 MB RAM under normal network conditions. Per the E2B research finding of ~4 s/GiB, a 512 MB sandbox should pause in ~2 s with margin. |
| NFR-02 | **Resume latency (E2B):** End-to-end resume time (CLI invocation to `state=running` confirmed) must be < 3 s for sandboxes with ≤ 512 MB RAM. The research baseline is ~1 s for Firecracker snapshot restore. |
| NFR-03 | **Local CRIU checkpoint throughput:** Docker + CRIU checkpoint write speed must not be the bottleneck beyond the kernel's memory dump rate. TAG must not add more than 500 ms of overhead on top of the raw `docker checkpoint create` duration. |
| NFR-04 | **SQLite atomicity:** All pause/resume state transitions in SQLite must use a single `BEGIN IMMEDIATE; ...; COMMIT` block. No intermediate state where `sandbox_runs.state` has been updated but `sandbox_checkpoints` has not yet been written, or vice versa. |
| NFR-05 | **No provider API calls in list:** `tag sandbox list --paused` reads only from SQLite. Provider API calls are gated behind `status` subcommand calls so list remains fast even with hundreds of paused sandboxes. |
| NFR-06 | **Error message clarity:** All error messages from pause/resume must include: (a) the sandbox ID, (b) the current state, (c) the expected state, (d) the provider name, (e) the underlying error from the provider SDK (if any). One-liners in the format `[sbx-abc123] pause failed: sandbox is in state 'killed', expected 'running' (provider: e2b)`. |
| NFR-07 | **Checkpoint storage hygiene:** Checkpoint files written to local disk (`~/.tag/checkpoints/`) must be removed when the corresponding sandbox is killed or when `tag sandbox rm <id>` is called. A `tag sandbox checkpoints prune --older-than 7d` subcommand cleans up orphaned checkpoint directories. |
| NFR-08 | **Billing accuracy:** The cost delta between a session that uses pause/resume and one that does not should be quantifiable via `tag costs --detail`. The `active_seconds` and `paused_seconds` fields in the detailed cost breakdown must be accurate to within 5 seconds. |
| NFR-09 | **Backward compatibility:** The new `pause` and `resume` subcommands must not alter behavior of existing `tag sandbox run`, `tag sandbox kill`, `tag sandbox list`, or `tag sandbox logs` commands. The new `--paused` flag on `list` is additive; existing `list` output is unchanged when `--paused` is not specified. |
| NFR-10 | **Dependency isolation:** CRIU integration must use only the `docker` CLI (`subprocess`) and not require a new Python package. E2B pause uses the existing `e2b` SDK. Daytona pause uses the existing `daytona` SDK. No new mandatory dependencies are introduced by this PRD. |

---

## 10. Technical Design

### 10.1 New and Modified Files

| File | Change Type | Description |
|------|-------------|-------------|
| `src/tag/sandbox.py` | Modified | Add `pause()`, `resume()`, `SandboxState` enum extension, `CheckpointRef` dataclass, `SandboxProvider` protocol extension, provider-specific pause/resume implementations for E2B, Daytona, Docker+CRIU, restricted subprocess |
| `src/tag/controller.py` | Modified | Add `cmd_sandbox_pause`, `cmd_sandbox_resume`, extend `cmd_sandbox_list` with `--paused` / `--all`, extend `cmd_sandbox_status` with checkpoint metadata output; add argparse subparsers for `pause`, `resume` |
| `src/tag/budget.py` | Modified | Update cost roll-up to exclude paused intervals; add `active_seconds` / `paused_seconds` decomposition to cost records |
| `src/tag/queue_worker.py` | Modified | Add `pause_between_phases` job config handling; integrate `sandbox.pause()` / `sandbox.resume()` at phase boundaries |
| `src/tag/tracing.py` | Modified | Add `sandbox.pause` and `sandbox.resume` span instrumentation following existing sandbox span patterns |

### 10.2 SQLite DDL

The following DDL additions are applied via `ensure_schema()` in `sandbox.py`, called at startup from `controller.py`'s `open_db()` initialization path.

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
-- These are added via ALTER TABLE IF NOT EXISTS column; safe to re-run.
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS state TEXT NOT NULL DEFAULT 'running';
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS pause_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS resume_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS total_paused_seconds REAL NOT NULL DEFAULT 0.0;
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS last_paused_at TEXT;
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS last_resumed_at TEXT;
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS provider TEXT;
ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS provider_sandbox_id TEXT;

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

### 10.3 Core Dataclasses

```python
# src/tag/sandbox.py additions

from __future__ import annotations

import enum
from dataclasses import dataclass, field
from typing import Optional, Protocol, runtime_checkable


class SandboxState(str, enum.Enum):
    """Full sandbox state machine including pause lifecycle."""
    CREATING  = "creating"
    STARTING  = "starting"
    RUNNING   = "running"
    PAUSING   = "pausing"
    PAUSED    = "paused"
    RESUMING  = "resuming"
    KILLING   = "killing"
    KILLED    = "killed"
    ERROR     = "error"

    # Terminal states — no further transitions possible.
    TERMINAL_STATES = frozenset({KILLED, ERROR})

    # States from which pause is valid.
    PAUSABLE_STATES = frozenset({RUNNING})

    # States from which resume is valid.
    RESUMABLE_STATES = frozenset({PAUSED})


@dataclass
class CheckpointRef:
    """Opaque reference to a sandbox checkpoint.

    For cloud providers (E2B, Daytona), ``provider_checkpoint_id`` is the
    authoritative reference and ``local_path`` is None.
    For Docker + CRIU, ``local_path`` is the directory containing checkpoint
    files and ``provider_checkpoint_id`` may be None.
    """
    checkpoint_id: str                  # TAG-internal UUID (ckpt-*)
    sandbox_id: str
    provider: str
    provider_checkpoint_id: Optional[str] = None
    local_path: Optional[str] = None
    checkpoint_size_bytes: Optional[int] = None
    billing_paused: bool = False
    paused_at: Optional[str] = None     # ISO-8601 UTC
    checkpoint_ready_at: Optional[str] = None


@dataclass
class PauseResult:
    """Return value from SandboxProvider.pause()."""
    checkpoint: CheckpointRef
    pause_duration_s: float
    billing_active: bool            # True = billing was NOT paused (degraded mode)
    provider_state_raw: str         # Raw state string from provider API
    warnings: list[str] = field(default_factory=list)


@dataclass
class ResumeResult:
    """Return value from SandboxProvider.resume()."""
    sandbox_id: str
    resume_duration_s: float
    billing_active: bool            # True = billing is active (expected after resume)
    provider_state_raw: str
    new_timeout_s: Optional[int] = None


@dataclass
class SandboxCheckpointRecord:
    """SQLite row in sandbox_checkpoints as a Python object."""
    id: str
    sandbox_id: str
    provider: str
    provider_checkpoint_id: Optional[str]
    checkpoint_path: Optional[str]
    checkpoint_size_bytes: Optional[int]
    state: str
    billing_paused: bool
    pause_duration_s: Optional[float]
    resume_duration_s: Optional[float]
    error_message: Optional[str]
    paused_at: str
    checkpoint_ready_at: Optional[str]
    resumed_at: Optional[str]
    created_at: str


@runtime_checkable
class SandboxProvider(Protocol):
    """Protocol that all sandbox provider implementations must satisfy.

    Extended from PRD-028 base protocol to include pause/resume.
    """

    def pause(self, sandbox_id: str) -> PauseResult:
        """Checkpoint the running sandbox and stop billing.

        Precondition: sandbox is in RUNNING state.
        Postcondition: sandbox is in PAUSED state; billing_active=False
                       (except for SIGSTOP-only fallbacks where billing
                       cannot be stopped).
        Raises:
            SandboxNotFoundError: if sandbox_id is unknown.
            SandboxStateError: if sandbox is not in RUNNING state.
            SandboxPauseError: if the provider checkpoint API fails.
        """
        ...

    def resume(
        self,
        sandbox_id: str,
        checkpoint: CheckpointRef,
        *,
        timeout_override: Optional[int] = None,
    ) -> ResumeResult:
        """Restore a paused sandbox from its checkpoint.

        Precondition: sandbox is in PAUSED state and checkpoint.state == 'ready'.
        Postcondition: sandbox is in RUNNING state; billing_active=True.
        Raises:
            SandboxNotFoundError: if sandbox_id is unknown.
            SandboxStateError: if sandbox is not in PAUSED state.
            CheckpointNotFoundError: if the checkpoint is missing or corrupted.
            SandboxResumeError: if the provider restore API fails.
        """
        ...
```

### 10.4 E2B Provider Implementation

```python
# src/tag/sandbox.py — E2BProvider.pause() and E2BProvider.resume()

import time
import uuid


class E2BProvider:
    """E2B Firecracker microVM provider with full checkpoint support."""

    def pause(self, sandbox_id: str) -> PauseResult:
        import e2b  # lazy import; e2b is an optional extra

        t0 = time.monotonic()
        checkpoint_id = f"ckpt-{uuid.uuid4().hex[:8]}"

        # Step 1: Retrieve the live sandbox handle.
        sbx = e2b.Sandbox.connect(sandbox_id)

        # Step 2: Issue pause via Firecracker REST API.
        # E2B SDK exposes this as sbx.pause() in v0.4+.
        # Falls back to direct HTTP if SDK version is older.
        try:
            provider_ckpt_id = sbx.pause()  # Returns provider checkpoint ID
        except AttributeError:
            # Older SDK: use HTTP directly against the E2B management API.
            provider_ckpt_id = self._pause_via_http(sandbox_id)

        # Step 3: Poll for billing confirmation.
        deadline = time.monotonic() + 60.0
        billing_active = True
        while time.monotonic() < deadline:
            info = sbx.get_info()  # {"state": "Paused", "billing_active": false}
            if info.get("state") == "Paused":
                billing_active = info.get("billing_active", False)
                break
            time.sleep(1.0)

        pause_duration_s = time.monotonic() - t0

        ckpt = CheckpointRef(
            checkpoint_id=checkpoint_id,
            sandbox_id=sandbox_id,
            provider="e2b",
            provider_checkpoint_id=provider_ckpt_id,
            local_path=None,
            billing_paused=not billing_active,
            paused_at=_utc_now(),
            checkpoint_ready_at=_utc_now(),
        )
        return PauseResult(
            checkpoint=ckpt,
            pause_duration_s=pause_duration_s,
            billing_active=billing_active,
            provider_state_raw="Paused",
        )

    def resume(
        self,
        sandbox_id: str,
        checkpoint: CheckpointRef,
        *,
        timeout_override: Optional[int] = None,
    ) -> ResumeResult:
        import e2b

        t0 = time.monotonic()

        # Resume from provider checkpoint ID.
        sbx = e2b.Sandbox.resume(
            checkpoint.provider_checkpoint_id,
            timeout=timeout_override or 300,
        )

        # Wait for running state.
        deadline = time.monotonic() + 30.0
        while time.monotonic() < deadline:
            info = sbx.get_info()
            if info.get("state") == "Running":
                break
            time.sleep(0.5)

        resume_duration_s = time.monotonic() - t0

        return ResumeResult(
            sandbox_id=sbx.id,
            resume_duration_s=resume_duration_s,
            billing_active=True,
            provider_state_raw="Running",
            new_timeout_s=timeout_override,
        )
```

### 10.5 Docker + CRIU Provider Implementation

```python
# src/tag/sandbox.py — DockerProvider.pause() with CRIU fallback

import shutil
import subprocess
from pathlib import Path


class DockerProvider:
    """Docker container provider with CRIU checkpoint or SIGSTOP fallback."""

    CRIU_AVAILABLE: Optional[bool] = None  # Cached after first check.

    @classmethod
    def _check_criu(cls) -> bool:
        if cls.CRIU_AVAILABLE is None:
            cls.CRIU_AVAILABLE = shutil.which("criu") is not None
        return cls.CRIU_AVAILABLE

    def pause(self, sandbox_id: str) -> PauseResult:
        t0 = time.monotonic()
        checkpoint_id = f"ckpt-{uuid.uuid4().hex[:8]}"
        container_id = self._get_container_id(sandbox_id)
        warnings: list[str] = []

        if self._check_criu():
            # Full CRIU checkpoint: captures memory + process tree to disk.
            ckpt_dir = Path.home() / ".tag" / "checkpoints" / sandbox_id
            ckpt_dir.mkdir(parents=True, exist_ok=True)

            result = subprocess.run(
                ["docker", "checkpoint", "create",
                 "--checkpoint-dir", str(ckpt_dir),
                 container_id, checkpoint_id],
                capture_output=True, text=True, timeout=120,
            )
            if result.returncode != 0:
                raise SandboxPauseError(
                    f"docker checkpoint create failed: {result.stderr}"
                )
            # docker checkpoint create also stops the container.
            # Measure checkpoint size.
            ckpt_size = sum(
                f.stat().st_size
                for f in ckpt_dir.rglob("*")
                if f.is_file()
            )
            billing_paused = False  # Local Docker billing is always user-controlled.
            local_path = str(ckpt_dir / checkpoint_id)
        else:
            # SIGSTOP fallback — no disk checkpoint; billing NOT paused for cloud Docker.
            result = subprocess.run(
                ["docker", "pause", container_id],
                capture_output=True, text=True, timeout=10,
            )
            if result.returncode != 0:
                raise SandboxPauseError(
                    f"docker pause failed: {result.stderr}"
                )
            ckpt_size = None
            billing_paused = False
            local_path = None
            warnings.append(
                "CRIU not available. Container is suspended (SIGSTOP) "
                "but checkpoint is in-memory only. "
                "Cross-session resume and billing pause are not supported. "
                "Install CRIU for full checkpoint support."
            )

        pause_duration_s = time.monotonic() - t0
        ckpt = CheckpointRef(
            checkpoint_id=checkpoint_id,
            sandbox_id=sandbox_id,
            provider="docker",
            local_path=local_path,
            checkpoint_size_bytes=ckpt_size,
            billing_paused=billing_paused,
            paused_at=_utc_now(),
        )
        return PauseResult(
            checkpoint=ckpt,
            pause_duration_s=pause_duration_s,
            billing_active=True,  # Local Docker is always user's machine
            provider_state_raw="exited" if self._check_criu() else "paused",
            warnings=warnings,
        )

    def resume(
        self,
        sandbox_id: str,
        checkpoint: CheckpointRef,
        *,
        timeout_override: Optional[int] = None,
    ) -> ResumeResult:
        t0 = time.monotonic()
        container_id = self._get_container_id(sandbox_id)

        if checkpoint.local_path is not None:
            # CRIU restore.
            ckpt_dir = Path(checkpoint.local_path).parent
            result = subprocess.run(
                ["docker", "start",
                 f"--checkpoint={checkpoint.checkpoint_id}",
                 f"--checkpoint-dir={ckpt_dir}",
                 container_id],
                capture_output=True, text=True, timeout=60,
            )
            if result.returncode != 0:
                raise SandboxResumeError(
                    f"docker start --checkpoint failed: {result.stderr}"
                )
        else:
            # SIGSTOP resume via SIGCONT (docker unpause).
            result = subprocess.run(
                ["docker", "unpause", container_id],
                capture_output=True, text=True, timeout=10,
            )
            if result.returncode != 0:
                raise SandboxResumeError(
                    f"docker unpause failed: {result.stderr}"
                )

        resume_duration_s = time.monotonic() - t0
        return ResumeResult(
            sandbox_id=sandbox_id,
            resume_duration_s=resume_duration_s,
            billing_active=True,
            provider_state_raw="running",
        )

    def _get_container_id(self, sandbox_id: str) -> str:
        """Look up the Docker container ID for a TAG sandbox ID via SQLite."""
        # Implemented by querying sandbox_runs.provider_sandbox_id
        raise NotImplementedError
```

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

In `budget.py`, the existing `_sum_sandbox_cost(conn, start, end)` function is modified to join `sandbox_billing_intervals` and sum only rows where `interval_type = 'active'`:

```python
def _active_sandbox_seconds(
    conn: sqlite3.Connection,
    sandbox_id: str,
    start: str,
    end: str,
) -> float:
    """Return total active (non-paused) seconds for a sandbox in a time range."""
    rows = conn.execute("""
        SELECT
            MAX(started_at, :start) AS effective_start,
            MIN(COALESCE(ended_at, :end), :end) AS effective_end
        FROM sandbox_billing_intervals
        WHERE sandbox_id = :sandbox_id
          AND interval_type = 'active'
          AND started_at < :end
          AND (ended_at IS NULL OR ended_at > :start)
    """, {"sandbox_id": sandbox_id, "start": start, "end": end}).fetchall()

    total = 0.0
    for effective_start, effective_end in rows:
        if effective_end and effective_end > effective_start:
            from datetime import datetime, timezone
            fmt = "%Y-%m-%dT%H:%M:%S%z"
            t_start = datetime.fromisoformat(effective_start)
            t_end = datetime.fromisoformat(effective_end)
            total += (t_end - t_start).total_seconds()
    return total
```

### 10.8 Queue Worker Integration

```python
# src/tag/queue_worker.py — inter-phase pause logic

async def _run_job_with_phases(job: dict, sandbox_id: str) -> None:
    pause_between = job.get("pause_between_phases", False)
    phases = job.get("phases", [job])  # single-phase jobs wrapped in list

    for i, phase in enumerate(phases):
        await _run_phase(phase, sandbox_id)

        if pause_between and i < len(phases) - 1:
            gate = phase.get("gate", {"type": "immediate"})

            # Pause the sandbox between phases.
            from tag.sandbox import pause_sandbox
            pause_result = pause_sandbox(sandbox_id)
            _log(f"[job:{job['id']}] sandbox paused after phase {i}; "
                 f"billing_active={pause_result.billing_active}; "
                 f"checkpoint={pause_result.checkpoint.checkpoint_id}")

            # Wait for gate condition.
            await _wait_for_gate(gate, job)

            # Resume before next phase.
            from tag.sandbox import resume_sandbox, get_latest_checkpoint
            ckpt = get_latest_checkpoint(sandbox_id)
            resume_sandbox(sandbox_id, ckpt)
            _log(f"[job:{job['id']}] sandbox resumed for phase {i+1}")
```

---

## 11. Security Considerations

1. **Checkpoint file permissions:** Checkpoint directories created under `~/.tag/checkpoints/<sandbox-id>/` must be created with mode `0700` (owner read/write/execute only). CRIU checkpoint files contain full process memory dumps including any secrets that were in the process's address space at pause time. Mode `0700` ensures other local users cannot read them.

2. **Cloud checkpoint access control:** For E2B and Daytona, checkpoint data resides in provider-managed storage. The TAG CLI must not log or persist provider checkpoint IDs in plaintext files accessible to other users. `sandbox_checkpoints.provider_checkpoint_id` in SQLite is only accessible to the DB owner since `~/.tag/runtime/tag.sqlite3` is created with `0600` permissions (enforced by `open_db()`).

3. **API key non-exposure in pause/resume:** The `E2B_API_KEY`, `DAYTONA_API_KEY`, and Docker credentials are loaded from the environment or keychain at runtime. They must not appear in `sandbox_checkpoints` rows, `--json` output, or OTel span attributes. The tracing integration must explicitly exclude credential-bearing attributes from spans.

4. **CRIU privilege requirement:** `docker checkpoint create` requires either the Docker daemon to run as root (standard Linux Docker installation) or the user to be in the `docker` group. TAG must not require `sudo` or elevated privileges beyond what Docker already requires. If the user lacks Docker group membership, the checkpoint command will fail with a clear permission error; TAG surfaces the message `"Run: sudo usermod -aG docker $USER and restart your session"`.

5. **Checkpoint injection prevention:** `tag sandbox resume` must validate that the `sandbox_id` in the `CheckpointRef` matches the `sandbox_id` argument. An attacker with write access to `~/.tag/checkpoints/` could not redirect a resume to a different sandbox by swapping checkpoint directories, because the sandbox ID is embedded in the SQLite record and cross-checked before the `docker start --checkpoint` call.

6. **Path traversal in `--checkpoint-dir`:** The `--checkpoint-dir` argument is resolved to an absolute path and checked to ensure it is not a parent of `~/.ssh`, `~/.aws`, `~/.config`, `/etc`, `/proc`, `/sys`, or `/dev`. This prevents a malicious job from checkpointing into a sensitive directory via path components like `../../.ssh/`.

7. **Credential scrubbing from checkpoint output:** The `--json` output of `tag sandbox pause` must not include environment variable contents, even if the sandbox's environment contained credentials. The JSON schema is fixed to the fields defined in Section 7.1; no environment dump or process memory excerpt is ever included.

8. **OTel span data minimization:** The `sandbox.pause` and `sandbox.resume` OTel spans must set only the attributes listed in FR-18. The `sandbox.id` attribute must use the TAG-internal opaque ID, not any provider URL or credential-bearing endpoint. The `sandbox.checkpoint_id` attribute must use the TAG-internal `ckpt-*` ID, not the provider checkpoint ID.

---

## 12. Testing Strategy

### 12.1 Unit Tests

Located in `tests/test_sandbox_pause_resume.py`.

| Test | Description |
|------|-------------|
| `test_pause_state_validation` | Verify that `sandbox.pause()` raises `SandboxStateError` when sandbox is in `killed`, `paused`, `pausing`, or `error` state. |
| `test_resume_state_validation` | Verify that `sandbox.resume()` raises `SandboxStateError` when sandbox is not in `paused` state. |
| `test_checkpoint_ref_serialization` | Round-trip `CheckpointRef` to and from the `sandbox_checkpoints` SQLite row via `SandboxCheckpointRecord`. |
| `test_billing_interval_active_seconds` | Given a synthetic `sandbox_billing_intervals` table with known active/paused intervals, verify `_active_sandbox_seconds()` returns the correct sum. |
| `test_sqlite_atomicity_pause` | Simulate a failure mid-transaction (after `sandbox_runs` update, before `sandbox_checkpoints` insert) and verify that a rollback leaves both tables in pre-pause state. |
| `test_criu_unavailable_fallback` | Mock `shutil.which("criu")` returning None; verify `DockerProvider.pause()` calls `docker pause`, returns a `PauseResult` with `billing_active=True`, and includes the SIGSTOP warning. |
| `test_criu_checkpoint_dir_permissions` | Verify that the checkpoint directory is created with mode `0700`. |
| `test_pause_result_json_no_credentials` | Verify that the `--json` output dict for a pause result contains none of the keys `env`, `environ`, `secrets`, `api_key`, or any `*TOKEN*` / `*SECRET*` keys. |
| `test_queue_worker_pause_between_phases` | Unit test the `_run_job_with_phases()` function with a two-phase job and `pause_between_phases=True`; verify `pause_sandbox` and `resume_sandbox` are called exactly once each, in the correct order. |
| `test_budget_excludes_paused_intervals` | Verify that `tag costs` (mocked SQLite) reports only active-interval seconds and excludes paused intervals from cost calculations. |

### 12.2 Integration Tests

Located in `tests/integration/test_sandbox_pause_resume_integration.py`. These tests require a running Docker daemon (CI matrix: ubuntu-latest with Docker pre-installed).

| Test | Description |
|------|-------------|
| `test_docker_sigstop_pause_resume` | Start a Docker sandbox running `sleep 3600`; pause it; verify `docker inspect` shows `Paused: true`; resume; verify `docker inspect` shows `Paused: false`; verify process still alive. |
| `test_docker_criu_checkpoint_restore` | (Skipped if CRIU not installed.) Start a Docker sandbox; write a file to `/tmp/testfile`; pause with CRIU; verify checkpoint directory exists and has files; resume; verify `/tmp/testfile` still exists. |
| `test_sqlite_state_after_pause_resume` | After a full pause/resume cycle on a Docker sandbox, query `sandbox_runs` and `sandbox_checkpoints` and verify: `sandbox_runs.state='running'`, `sandbox_runs.pause_count=1`, `sandbox_runs.resume_count=1`, one `sandbox_checkpoints` row with `state='restored'`. |
| `test_list_paused_no_provider_calls` | Pause a Docker sandbox; mock the Docker API to raise an exception; verify `tag sandbox list --paused` still returns the paused sandbox from SQLite without error. |
| `test_billing_interval_rows` | After pause then resume, verify two `sandbox_billing_intervals` rows exist: one `active` interval (started_at = sandbox creation, ended_at = pause time) and one `paused` interval (started_at = pause time, ended_at = resume time). |

### 12.3 E2B Integration Tests (Optional, Requires E2B_API_KEY)

Located in `tests/integration/test_e2b_pause_resume.py`. Gated behind `pytest.mark.e2b` marker; skipped in CI unless `E2B_API_KEY` is set.

| Test | Description |
|------|-------------|
| `test_e2b_pause_billing_stop` | Create an E2B sandbox; pause; verify `billing_active=False` in pause result; resume; verify `billing_active=True` in resume result. |
| `test_e2b_filesystem_state_after_resume` | Create sandbox; write `/home/user/test.txt` via `sandbox.filesystem.write()`; pause; resume; verify `/home/user/test.txt` exists. |
| `test_e2b_process_state_after_resume` | Create sandbox; start a background process; pause; resume; verify process is still listed in `ps aux`. |
| `test_e2b_pause_duration_within_sla` | Pause a fresh E2B sandbox (default 512 MB); verify `pause_duration_s < 6.0`. |
| `test_e2b_resume_duration_within_sla` | Resume an E2B sandbox from checkpoint; verify `resume_duration_s < 3.0`. |

### 12.4 Performance Tests

Located in `tests/perf/test_sandbox_pause_perf.py`.

```python
@pytest.mark.perf
def test_pause_resume_throughput():
    """Verify pause + resume cycle completes within SLA for 10 sequential cycles."""
    import statistics
    pause_times, resume_times = [], []

    for _ in range(10):
        sbx_id = create_test_sandbox()
        t0 = time.monotonic()
        result = pause_sandbox(sbx_id)
        pause_times.append(result.pause_duration_s)

        t1 = time.monotonic()
        ckpt = get_latest_checkpoint(sbx_id)
        resume_sandbox(sbx_id, ckpt)
        resume_times.append(time.monotonic() - t1)
        kill_sandbox(sbx_id)

    assert statistics.median(pause_times) < 6.0, (
        f"Median pause time {statistics.median(pause_times):.2f}s exceeds 6s SLA"
    )
    assert statistics.median(resume_times) < 3.0, (
        f"Median resume time {statistics.median(resume_times):.2f}s exceeds 3s SLA"
    )
```

---

## 13. Acceptance Criteria

| ID | Criterion | Verified by |
|----|-----------|-------------|
| AC-01 | `tag sandbox pause <id>` on a running E2B sandbox completes with exit code 0, sets `state=paused` in SQLite, and has `billing_active: false` in `--json` output within 60 s. | E2B integration test |
| AC-02 | `tag sandbox resume <id>` on a paused E2B sandbox restores the sandbox to `state=running` with full filesystem and process state preserved. | E2B filesystem + process integration tests |
| AC-03 | `tag sandbox pause <id>` on a Docker sandbox with CRIU creates a checkpoint directory at `~/.tag/checkpoints/<id>/` with mode `0700` and at least one checkpoint file. | Docker CRIU integration test |
| AC-04 | `tag sandbox pause <id>` on a Docker sandbox without CRIU exits with code 5, prints a warning containing `SIGSTOP`, and sets `billing_paused=0` in SQLite. | Unit test `test_criu_unavailable_fallback` |
| AC-05 | `tag sandbox list --paused --json` returns the correct list of paused sandboxes from SQLite and completes in < 200 ms without making any provider API calls. | Integration test `test_list_paused_no_provider_calls` |
| AC-06 | `tag sandbox status <id> --json` returns `billing_active: false` for a paused E2B sandbox and `billing_active: true` after resume. | E2B integration test |
| AC-07 | `tag costs` correctly excludes paused intervals: a sandbox that was active for 1 hour, paused for 1 hour, then active for 1 hour is billed for exactly 2 hours ± 5 s. | Unit test `test_billing_excludes_paused_intervals` |
| AC-08 | A `queue_worker.py` job with `pause_between_phases: true` and two phases: (a) pauses the sandbox after phase 1, (b) waits for the gate, (c) resumes before phase 2, without any manual intervention. | Queue worker unit test |
| AC-09 | `tag sandbox pause <id>` on a sandbox in `killed` or `error` state exits with code 1 and prints an error message containing the current state. | Unit test `test_pause_state_validation` |
| AC-10 | `tag sandbox resume <id>` on a sandbox not in `paused` state exits with code 1 and prints an error message containing the current state. | Unit test `test_resume_state_validation` |
| AC-11 | OTel spans for `sandbox.pause` and `sandbox.resume` are emitted with attributes `sandbox.id`, `sandbox.provider`, `sandbox.operation`, `sandbox.checkpoint_id`, and `sandbox.pause_duration_s` or `sandbox.resume_duration_s`. | Unit test mocking `tracing.start_span()` |
| AC-12 | The `--json` output of `tag sandbox pause` contains none of the fields `env`, `environ`, `api_key`, or any key matching `*TOKEN*` or `*SECRET*`. | Unit test `test_pause_result_json_no_credentials` |
| AC-13 | A sandbox paused in session A can be resumed in a new TAG CLI session B (same machine, Docker + CRIU) by running `tag sandbox resume <id>`. | Docker CRIU integration test |
| AC-14 | For E2B, the median `pause_duration_s` across 10 iterations for a 512 MB sandbox is < 6.0 s. | Performance test |
| AC-15 | For E2B, the median `resume_duration_s` across 10 iterations is < 3.0 s. | Performance test |

---

## 14. Dependencies

| Dependency | Type | Purpose | Notes |
|------------|------|---------|-------|
| PRD-028 Sandbox Code Execution | Required predecessor | Provides `sandbox_runs` table, `SandboxProvider` protocol base, `ensure_schema()` hook, Docker/E2B/Modal provider classes, and existing `tag sandbox` CLI namespace | Must be implemented first; this PRD extends PRD-028 |
| PRD-013 Agent Tracing & Observability | Required | OTel span emission for `sandbox.pause` / `sandbox.resume` operations | Span attributes defined in Section FR-18 |
| PRD-034 Security | Required | Credential pattern blocklist, mount path validation, `open_db()` file permission enforcement | `validate_mount()` from PRD-034 is called in `pause()` to block checkpoint-dir path traversal |
| PRD-012 Cost Tracking & Budget | Required | Budget roll-up must exclude paused intervals; `sandbox_billing_intervals` table joins into cost calculation | `budget.py` modification in Section 10.7 |
| PRD-008 Background Task Queue | Soft dependency | Queue worker integration for `pause_between_phases`; not required for core pause/resume commands | Queue integration in Section 10.8 |
| `e2b` Python SDK ≥ 0.4.0 | Optional runtime | `Sandbox.pause()` and `Sandbox.resume()` methods added in 0.4.0; earlier versions use HTTP fallback | Already in `pyproject.toml` optional extras from PRD-028 |
| CRIU (Checkpoint/Restore In Userspace) | Optional system binary | Required for full Docker checkpoint-to-disk. Install: `sudo apt install criu` on Ubuntu; `brew install --cask criu` does not exist on macOS — Docker+CRIU is Linux-only | TAG gracefully degrades to SIGSTOP when absent |
| Docker Engine ≥ 23.0 | Optional runtime | `docker checkpoint create` requires experimental features enabled in `daemon.json` on Docker CE: `{"experimental": true}` | Already required by PRD-028 Docker backend |

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

**Total estimated effort:** M — 1.5 to 2 weeks for a single engineer with Docker and Python experience. The E2B and Daytona integrations add risk due to provider API surface; allow buffer if SDK versions do not yet expose pause natively.

### Phase 1 — Schema and Protocol Foundation (Days 1–2)

- Add `SandboxState`, `CheckpointRef`, `PauseResult`, `ResumeResult`, `SandboxCheckpointRecord` dataclasses to `sandbox.py`.
- Write and apply SQL DDL for `sandbox_checkpoints`, `sandbox_billing_intervals`, and `ALTER TABLE sandbox_runs` columns.
- Extend `SandboxProvider` protocol with `pause()` and `resume()` abstract methods.
- Write unit tests for dataclass serialization and SQLite atomicity (AC-09, AC-10, subset of AC-07).

### Phase 2 — Docker Provider Implementation (Days 3–5)

- Implement `DockerProvider.pause()` with CRIU path and SIGSTOP fallback.
- Implement `DockerProvider.resume()` with CRIU restore and `docker unpause` fallback.
- Implement CRIU availability check (`_check_criu()`), checkpoint directory creation with `0700` permissions, size computation.
- Add `cmd_sandbox_pause` and `cmd_sandbox_resume` to `controller.py` with full argparse integration.
- Write Docker integration tests (AC-03, AC-04, AC-13).

### Phase 3 — E2B Provider Implementation (Days 6–8)

- Implement `E2BProvider.pause()` using E2B SDK `sbx.pause()` with HTTP fallback for older SDK.
- Implement `E2BProvider.resume()` using `e2b.Sandbox.resume()`.
- Handle sandbox ID change on resume (OQ-02 resolution required before this phase completes).
- Write E2B integration tests (AC-01, AC-02, AC-06, AC-14, AC-15) — gated behind `E2B_API_KEY`.

### Phase 4 — CLI, Budget, and Queue Integration (Days 9–11)

- Extend `tag sandbox list` with `--paused` and `--all` flags; update output table columns.
- Extend `tag sandbox status` with checkpoint metadata in `--json` output.
- Update `budget.py` with `_active_sandbox_seconds()` and `sandbox_billing_intervals` join (AC-07).
- Add `pause_between_phases` support to `queue_worker.py` (AC-08).
- Implement OTel span emission in `tracing.py` for pause/resume (AC-11).

### Phase 5 — Security Hardening and Checkpoint Pruning (Days 12–14)

- Enforce `0700` checkpoint directory permissions and verify path traversal guard in `--checkpoint-dir`.
- Verify `--json` output exclusion of credential fields (AC-12).
- Implement `tag sandbox checkpoints prune` subcommand.
- Run full security checklist from Section 11.
- Complete performance tests and verify E2B SLA targets are met (AC-14, AC-15).
- Update `docs/prd/INDEX.md` to add PRD-095 to the priority matrix.

---

*End of PRD-095*

