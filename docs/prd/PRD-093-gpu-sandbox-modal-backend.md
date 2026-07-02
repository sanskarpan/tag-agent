# PRD-093: GPU Sandbox via Modal Backend (Complete the Modal Integration Stub) (`tag sandbox run --backend modal --gpu`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox` (Backend interface + firecracker GPU tier + optional Modal HTTP remote backend)
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing & Observability), PRD-034 (Security / Secret Scanning), PRD-012 (Cost Tracking & Budget), PRD-005 (Execution Backend Selection), PRD-039 (Token Budget Enforcement)
**Inspired by:** Modal GPU functions, E2B GPU (coming), Vast.ai

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) establishes an isolation ladder of execution backends. In the Go harness this is expressed as a single `Backend` interface in `internal/sandbox` (`Run(ctx, Spec) (Result, error)`) with concrete implementations registered at startup: the `restricted` tier (landlock + seccomp), the `docker` tier (docker/moby client), and — for GPU workloads, the strongest tier of the ladder — a native **Firecracker microVM** backend built on `firecracker-microvm/firecracker-go-sdk` with GPU passthrough via VFIO. Modal, which has **no official Go SDK**, is demoted to an *optional remote cloud backend* invoked over Modal's HTTP API via `net/http` behind the same `Backend` interface. This PRD specifies the complete GPU-tier implementation: GPU type selection, environment-variable injection, host-directory volume mounts, structured exit-code and output capture, and per-run cost attribution written back to SQLite via `internal/store`.

> **Go re-framing note:** the original "complete the Modal Python-SDK stub" premise does not survive the Go move. There is no Go Modal SDK to complete. The native GPU path becomes `firecracker-go-sdk` (VFIO GPU passthrough), and Modal becomes an optional HTTP remote backend registered behind the `Backend` interface. GPU-tier/cost/env/volume feature scope is unchanged; only the mechanism is Go-native.

The driving use case is ML practitioners who use TAG as an agent orchestration layer and want to run GPU-dependent code — PyTorch training loops, CUDA kernel benchmarks, HuggingFace inference — inside a strong sandbox without standing up their own orchestration. The native Firecracker tier gives a hardware-isolated microVM on Linux hosts with `/dev/kvm` and a VFIO-bound GPU; the Modal remote backend offers on-demand cloud GPU functions billed per second (H100 capacity in under 60 seconds in most regions) for users without local GPU hardware. Backend selection reuses the existing execution-backend surface (`internal/config` reads `execution.backend` from profile YAML via koanf) and, for Modal, the credential surface in `internal/credentials`.

The four GPU tiers targeted — T4, A10G, A100, H100 — cover the full cost/capability spectrum from $0.000059/GPU-second (T4, ~$0.21/hr) to $0.000305/GPU-second (H100, ~$1.10/hr). A cost-estimation step fires before each GPU run and prints a projected cost alongside a confirmation prompt unless `--yes` is passed or `CI=true` is set, matching the pattern established by `tag eval run`. Every run writes a `modal_cost_usd` column to `sandbox_runs` so that `tag budget` can aggregate GPU spend alongside LLM spend. (The column name is retained for schema/data-model continuity; for the native Firecracker tier it holds the metered microVM GPU-second cost.)

The implementation is contained in `internal/sandbox` (the `Backend` interface, the `firecracker` GPU backend, and the optional `modal` HTTP backend), `internal/cli` (new `--gpu`, `--env`, `--volume`, `--timeout` flags on the `sandbox run` cobra command, and a `sandbox cost` subcommand), and the `sandbox_runs` schema owned by `internal/store` (new columns). No CGO is introduced. The Modal backend is compiled unconditionally but registers itself only when Modal credentials/config are present (optional backend registration + a runtime capability check), preserving PRD-028's zero-mandatory-dependency goal without Python's lazy-import trick.

GPU sandbox runs are attributed to the calling agent run or queue job via the existing `run_id` / `job_id` context threaded through `internal/runtime`. A new `sandbox_gpu_runs` view joins `sandbox_runs` with the `runs` table on `invoking_run_id`, allowing `tag trace` to show GPU sandbox invocations as child spans of agent runs.

---

## 2. Problem Statement

### 2.1 The Modal Backend Is a Silent No-Op

`run_in_sandbox()` in `sandbox.py` accepts `backend="modal"` without error but silently falls through to `_run_restricted()`. A user who passes `--backend modal` sees the run recorded with `backend=modal` in SQLite but the command actually runs locally in a restricted subprocess, with none of Modal's isolation, cloud execution, or GPU access. There is no warning, no error, and no indication of the fallback. This is a correctness bug disguised as a missing feature: the function contract says "run on Modal" and the implementation violates it silently.

### 2.2 GPU Workloads Are Blocked from TAG's Sandbox Layer

ML workflows are among the most common use cases for agent-generated code that should run in a sandbox rather than on the host — training scripts consume GPU memory, can run for hours, and may download multi-GB model checkpoints to unpredictable paths. TAG has no mechanism to dispatch these to a GPU-capable environment. Users either run GPU code directly on the host (unsafe, resource-hungry) or manually copy it to Modal/Colab (breaks the agent loop). The missing Modal backend leaves a significant use-case gap for the ML/AI subset of TAG users.

### 2.3 GPU Spend Is Invisible to TAG's Budget Subsystem

`tag budget` (PRD-012) tracks LLM API costs from the `cost_usd` column of the `runs` table. Modal GPU runs, when dispatched manually outside TAG, produce costs that never appear in `tag budget status`. There is no audit trail linking a GPU training run to the agent task that triggered it, making total-cost-of-task analysis impossible. Even after PRD-028 landed, the `sandbox_runs` table has no cost column; this PRD adds `modal_cost_usd`, `modal_function_id`, and `gpu_type` columns.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Implement `_run_modal()` in `sandbox.py` that creates a Modal Sandbox with the specified GPU type, executes the command, captures stdout/stderr, and returns `(exit_code, stdout, stderr)`. |
| G2 | Support four GPU tiers: `t4`, `a10g`, `a100`, `h100` (case-insensitive), mapped to Modal GPU string identifiers with optional count suffix (e.g., `"H100:1"`). |
| G3 | Accept arbitrary environment variable injection via `--env KEY=VALUE` (repeatable), passed into the Modal Sandbox environment. |
| G4 | Accept host-to-sandbox volume mounts via `--volume HOST_PATH:SANDBOX_PATH` (repeatable), with credential-path blocking inherited from PRD-028 security rules. |
| G5 | Return structured output: exit code, stdout, stderr, wall-clock duration, and estimated USD cost from Modal's billing API or a static rate table, written to `sandbox_runs`. |
| G6 | Print a cost estimate (GPU tier, estimated duration, projected USD) before execution; require confirmation or `--yes` / `CI=true` bypass. |
| G7 | Attribute sandbox runs to the calling agent run or queue job via an `invoking_run_id` column, enabling `tag trace` to include GPU sandbox spans. |
| G8 | Expose `tag sandbox run --backend modal --gpu <tier> --code <inline>` and `--file <path>` as CLI entry points. |
| G9 | Add `tag sandbox cost` subcommand that queries `sandbox_runs` and summarises total GPU spend grouped by tier, date, and invoking run. |
| G10 | Emit an OpenTelemetry span for each Modal sandbox run with `sandbox.backend=modal`, `sandbox.gpu_type`, `sandbox.cost_usd`, and `sandbox.exit_code` attributes (PRD-013 integration). |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Multi-GPU distributed training (e.g., `H100:8` multi-node) — v1 supports single-GPU-per-run only; multi-GPU is a follow-on. |
| NG2 | Persistent Modal sandbox sessions with resume/pause semantics — each `tag sandbox run` is a fresh ephemeral Modal Sandbox; stateful warm pools are deferred. |
| NG3 | Custom Docker image builds within TAG — users specify an existing image via `--image`; TAG does not build or push images. |
| NG4 | E2B GPU support — E2B GPU is listed as "coming"; this PRD is Modal-only and does not block on E2B availability. |
| NG5 | Real-time streaming of Modal sandbox stdout to the terminal — Modal Sandbox output is captured after completion; live streaming via `modal.io` SSE is deferred. |
| NG6 | Cost forecasting from actual Modal billing API — v1 uses a static rate table updated per release; live billing API integration is a follow-on. |
| NG7 | Windows support for volume mounts — `pathlib.Path` normalization handles macOS/Linux; Windows path semantics (drive letters) are excluded from v1. |
| NG8 | Automatic Modal credential provisioning — users must have `modal token new` completed; TAG surfaces a clear error message and doctor check, not a credential wizard. |
| NG9 | Daytona or Vast.ai GPU backends — this PRD is scoped to Modal only; provider abstraction is addressed in a separate future PRD. |

---

## 5. Success Metrics

| Metric | Baseline | Target | Measurement Method |
|--------|----------|--------|--------------------|
| Modal backend no-op rate | 100% (all Modal runs fall through) | 0% | Integration test asserting `_run_modal()` is called |
| GPU run end-to-end latency (T4, 60s workload) | N/A (not implemented) | <90s p50 cold, <15s p50 warm | Automated integration test with Modal's `pytest-modal` harness |
| Cost attribution coverage | 0% (no cost column) | 100% of Modal runs have `modal_cost_usd != NULL` | SQL query on `sandbox_runs` |
| `tag sandbox cost` command p99 query time | N/A | <50ms | Unit test with synthetic 10,000-row `sandbox_runs` table |
| Credential-path block rate | N/A | 100% (no mount accepted for blocked patterns) | Unit test matrix of 15 blocked path patterns |
| `--gpu` flag acceptance rate for all 4 tiers | 0% | 100% (t4/a10g/a100/h100 all accepted without error) | Unit test parametrised over all 4 tiers |
| `tag doctor` Modal GPU check pass rate | 0% (no check exists) | 100% on machines with `modal` installed and token | `tag doctor --backend modal` integration test |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | ML engineer | run `tag sandbox run --backend modal --gpu a10g --code "import torch; print(torch.cuda.is_available())"` | I can verify CUDA availability in a cloud GPU sandbox without leaving the TAG workflow |
| U2 | ML engineer | run `tag sandbox run --backend modal --gpu h100 --file train.py --volume ./data:/data` | I can dispatch a training script that reads from a local data directory into a cloud H100 sandbox in a single command |
| U3 | Agent author | configure a queue job with `sandbox: true` and `backend: modal` and `gpu: a10g` in the job YAML | My GPU-dependent agent-generated code automatically runs on Modal without any manual invocation |
| U4 | Team lead | run `tag sandbox cost --since 2026-06-01 --group-by gpu_type` | I can see a breakdown of GPU spending by tier for the current month and make informed infrastructure decisions |
| U5 | Developer | run `tag sandbox run --backend modal --gpu t4 --env WANDB_API_KEY=$WANDB_API_KEY --file eval.py` | I can inject secrets as environment variables into the sandbox without hardcoding them in the script |
| U6 | DevOps engineer | see a cost estimate and confirmation prompt before a GPU run executes | I cannot accidentally incur large GPU bills from an accidental command or a mistyped script |
| U7 | Platform engineer | run `tag trace show <run-id>` and see GPU sandbox invocations as child spans | I can reconstruct the full causal chain from agent prompt to GPU execution in a single trace view |
| U8 | Developer | run `tag doctor` and see a `modal_gpu` check that reports green/yellow/red | I can diagnose Modal credential and SDK issues before attempting a GPU sandbox run |

---

## 7. Proposed CLI Surface

### 7.1 `tag sandbox run --backend modal --gpu`

Run arbitrary code on a Modal GPU sandbox.

```
tag sandbox run \
  --backend modal \
  --gpu <t4|a10g|a100|h100> \
  [--code <inline_python_or_shell>] \
  [--file <path/to/script.py>] \
  [--image <docker_image>] \
  [--env KEY=VALUE ...] \
  [--volume HOST_PATH:SANDBOX_PATH ...] \
  [--timeout <seconds>] \
  [--yes] \
  [--json] \
  [--no-cost-estimate]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--backend modal` | str | `restricted` | Select Modal cloud backend |
| `--gpu` | str | None | GPU tier: `t4`, `a10g`, `a100`, `h100` |
| `--code` | str | None | Inline code string to execute (runs as `python3 -c "<code>"`) |
| `--file` | path | None | Path to script file; uploaded to Modal sandbox and executed |
| `--image` | str | `python:3.12-slim` | Base Docker image for the Modal sandbox |
| `--env` | KEY=VAL | [] | Environment variable injection; repeatable |
| `--volume` | HOST:SANDBOX | [] | Directory mount; repeatable; credential paths blocked |
| `--timeout` | int | 300 | Hard timeout in seconds (max 86400) |
| `--yes` | flag | False | Skip cost confirmation prompt |
| `--json` | flag | False | Emit JSON result to stdout instead of formatted output |
| `--no-cost-estimate` | flag | False | Skip cost estimate display (but not safety prompt) |

**Example 1 — inline GPU verification:**

```bash
$ tag sandbox run --backend modal --gpu a10g \
    --code "import torch; print(torch.cuda.is_available(), torch.cuda.get_device_name(0))"

Estimated cost: A10G GPU for ~60s ceiling → $0.0042 USD
Proceed? [y/N]: y

sandbox a1b2c3d4e5f6  backend=modal  gpu=a10g  status=running
  Submitted Modal function  function_id=fn-8x9yz
  Container cold-start      7.3s
  Execution                 2.1s

True NVIDIA A10G

exit_code=0  duration=9.4s  cost=$0.0009 USD
```

**Example 2 — file + volume mount:**

```bash
$ tag sandbox run --backend modal --gpu h100 \
    --file train.py \
    --volume ./data:/data \
    --env WANDB_API_KEY=wand-xyz123 \
    --timeout 3600

Estimated cost: H100 GPU for ~3600s ceiling → $1.10 USD
Proceed? [y/N]: y

sandbox b2c3d4e5f6a1  backend=modal  gpu=h100  status=running
  Uploading  train.py (4.2 KB)
  Mounting   ./data → /data (142 MB)
  Submitted  Modal function  function_id=fn-abc999
  ...
  Epoch 10/10  loss=0.0041

exit_code=0  duration=847s  cost=$0.258 USD
```

**Example 3 — JSON output:**

```bash
$ tag sandbox run --backend modal --gpu t4 \
    --code "print('hello')" --yes --json

{
  "id": "c3d4e5f6a1b2",
  "backend": "modal",
  "gpu_type": "t4",
  "image": "python:3.12-slim",
  "status": "done",
  "exit_code": 0,
  "output": "hello\n",
  "error": null,
  "duration_s": 5.3,
  "modal_function_id": "fn-t4-9988aa",
  "modal_cost_usd": 0.0003,
  "created_at": "2026-06-17T10:00:00Z",
  "completed_at": "2026-06-17T10:00:05Z"
}
```

### 7.2 `tag sandbox cost`

Summarise GPU spend from `sandbox_runs`.

```
tag sandbox cost \
  [--since <YYYY-MM-DD>] \
  [--until <YYYY-MM-DD>] \
  [--group-by <gpu_type|date|run_id>] \
  [--json]
```

**Example output:**

```
GPU Sandbox Cost Report  (2026-06-01 → 2026-06-17)

  GPU     Runs    Total Duration    Total Cost
  ------  ------  ----------------  ----------
  t4         12      1h 04m 22s        $0.46
  a10g        5      3h 12m 07s        $4.12
  a100        1         22m 45s        $1.88
  h100        2      1h 47m 03s        $7.41

  TOTAL      20      6h 26m 17s       $13.87
```

### 7.3 `tag doctor` extension

```
$ tag doctor --backend modal

[modal_sdk]       ✓  modal 0.67.43 installed
[modal_token]     ✓  token present at ~/.modal/token_id (expires 2027-06-17)
[modal_gpu]       ✓  GPU access verified (A10G available in us-east-1)
[modal_image]     ✓  default image python:3.12-slim pullable
```

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | The GPU `Backend.Run` implementation MUST dispatch to the selected GPU tier — the native Firecracker microVM backend or the Modal HTTP remote backend — and MUST NOT silently fall back to the `restricted` backend. A backend that cannot satisfy the request MUST return a non-nil `error`. | P0 |
| FR-02 | When `gpu` is non-empty, the microVM (or Modal HTTP request) MUST be provisioned with the corresponding GPU type from the mapping table (`t4` → `"T4"`, `a10g` → `"A10G"`, `a100` → `"A100"`, `h100` → `"H100"`). For Firecracker this selects the VFIO device profile; for Modal it sets the `gpu` field of the JSON request body. | P0 |
| FR-03 | When `gpu` is empty and a GPU backend is requested, the sandbox MUST run on CPU only (no VFIO device attached / no `gpu` field in the Modal request). | P0 |
| FR-04 | `--env KEY=VALUE` arguments MUST be injected into the guest environment (Firecracker guest env / Modal `environment` request field); they MUST NOT be written to any file or logged. | P0 |
| FR-05 | `--volume HOST_PATH:SANDBOX_PATH` arguments MUST be attached as a microVM block/virtio-fs mount (Firecracker) or uploaded as a mount payload over the Modal HTTP API; the mount MUST be read-write unless `--volume` is suffixed with `:ro`. | P1 |
| FR-06 | Volume HOST_PATH values that match any PRD-028 blocked pattern (`*.env`, `*.key`, `*.pem`, `*secret*`, `*credential*`, `~/.ssh/*`, `~/.aws/*`, `~/.config/op/*`) MUST cause `Run` to return an error (`ErrBlockedMountPath`) before any microVM boot or Modal HTTP call is made. | P0 |
| FR-07 | `Backend.Run` MUST return `ExitCode`, `Stdout` (up to 1 MB, truncated with warning), and `Stderr` (up to 256 KB) from the sandbox execution. | P0 |
| FR-08 | The `sandbox_runs` table MUST be extended (via `internal/store` migration) with columns `gpu_type TEXT`, `modal_function_id TEXT`, `modal_cost_usd REAL`, `duration_s REAL`, `invoking_run_id TEXT` before the first GPU write. (`modal_function_id` holds the Modal function id for the remote backend, or the Firecracker VM id for the native backend.) | P0 |
| FR-09 | `modal_cost_usd` MUST be computed from `duration_s * gpuRatePerSecond[gpu_type]` and written to `sandbox_runs` after each completed or failed run. | P1 |
| FR-10 | Before dispatching a GPU run, `tag sandbox run` MUST print an estimated cost (ceiling at `--timeout` seconds) and prompt for `y/N` confirmation unless `--yes` is passed or the `CI` environment variable is non-empty. | P1 |
| FR-11 | If the requested backend is unavailable (e.g. Firecracker requires Linux + `/dev/kvm`; Modal requires configured credentials), `Run` MUST return a descriptive `error` and the CLI MUST print a user-friendly message with remediation steps (no stack trace). Backend availability is a runtime capability check, not an import guard. | P0 |
| FR-12 | If the Modal HTTP API returns an authentication failure (401/403), the Modal backend MUST wrap the error with a message directing the user to configure Modal credentials (`tag credentials add modal` / `modal token new`). | P0 |
| FR-13 | `tag sandbox cost` MUST query only `sandbox_runs WHERE backend IN ('modal','firecracker')` and present aggregated results by the requested `--group-by` dimension. | P1 |
| FR-14 | `tag sandbox cost` MUST complete in under 200ms for a `sandbox_runs` table of up to 100,000 rows (requires index on `(backend, created_at)`). | P1 |
| FR-15 | `tag sandbox run --file <path>` MUST read the file, place it into the guest as `/workspace/<filename>`, and execute it with `python3 /workspace/<filename>` (or the shebang interpreter if present). | P1 |
| FR-16 | The GPU sandbox MUST have `--timeout` enforced at the isolation boundary (the Firecracker VM lifetime / the Modal function timeout) via a `context.WithTimeout` deadline propagated into `Run`, not merely a local wall-clock check. | P0 |
| FR-17 | An OpenTelemetry span named `sandbox.gpu.run` MUST be emitted (via `internal/obs` / `go.opentelemetry.io/otel`) for every GPU run with attributes: `sandbox.backend`, `sandbox.gpu_type`, `sandbox.cost_usd`, `sandbox.exit_code`, `sandbox.duration_s`, `sandbox.modal_function_id`. | P2 |
| FR-18 | `tag doctor` MUST include a `gpu_sandbox` check that verifies backend availability: for Firecracker (Linux + `/dev/kvm` + a VFIO-bound GPU) and, when configured, Modal credentials + optional reachability of the Modal API. | P2 |
| FR-19 | `--json` flag on `tag sandbox run` MUST emit a single JSON object to stdout (not mixed with progress lines) containing all fields from the `sandbox_runs` row plus `gpu_type`, `modal_function_id`, `modal_cost_usd`, `duration_s`. | P1 |
| FR-20 | Wall-clock `duration_s` MUST be measured from before the microVM boot / Modal HTTP submit to after the final wait/poll, using `time.Now()`/`time.Since`, and written to `sandbox_runs`. | P1 |
| FR-21 | When `--file` and `--code` are both specified, the CLI MUST error with `"--file and --code are mutually exclusive"`. | P0 |
| FR-22 | When neither `--file` nor `--code` is specified and a GPU backend is passed, the CLI MUST error with `"gpu sandbox requires --code or --file"`. | P0 |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Cold-start latency for a T4 sandbox running a 1-line Python print is ≤90s end-to-end (microVM boot + rootfs + execution for Firecracker; network + container pull + execution for Modal). | p50 ≤90s |
| NFR-02 | Warm-start latency (same image/rootfs, same region, second run within 5 min) is ≤15s. | p50 ≤15s |
| NFR-03 | The Modal backend MUST be a compiled-in, optionally-registered `Backend` implementation; the binary MUST run with zero GPU/Modal configuration present and expose no GPU backend rather than failing at startup. | Always |
| NFR-04 | `internal/sandbox` MUST NOT require any CGO or host GPU library to build (`CGO_ENABLED=0`); GPU/microVM availability is resolved at runtime via a capability check, never at build/link time. | Always |
| NFR-05 | Environment variables passed via `--env` MUST NOT appear in the `command` column of `sandbox_runs`, in log output, or in OTel span attributes. | Always |
| NFR-06 | `modal_cost_usd` MUST be accurate to within ±5% of the provider's actual billing for a given run duration using the static rate table. | ±5% |
| NFR-07 | The Modal HTTP backend MUST target a pinned Modal REST API version constant and surface a clear error if the API responds with an unsupported/deprecated version. The Firecracker backend MUST target a pinned `firecracker-go-sdk` version. | Always |
| NFR-08 | No network call is made to Modal (and no microVM is booted) until after the user confirms the cost prompt (or `--yes`/`CI` bypasses it). | Always |
| NFR-09 | `tag sandbox run` with a GPU backend and an invalid GPU tier MUST fail with exit code 1 and a human-readable message listing valid tiers — no stack trace exposed. | Always |
| NFR-10 | All new code in `internal/sandbox` MUST achieve ≥90% line coverage via table-driven Go `testing` tests that inject a fake `Backend` (dependency injection), with no live network or KVM required. | ≥90% |

---

## 10. Technical Design

### 10.0 Architecture — the `Backend` interface and the GPU tier

The isolation ladder is expressed as one Go interface in `internal/sandbox`. Every tier (restricted, docker, gVisor, firecracker, modal) implements it, and the CLI/runtime select a tier by name and depend only on the interface — the same dependency-injection seam used by the tests to substitute a fake backend.

```go
package sandbox

import (
    "context"
    "time"
)

// Backend is the isolation-ladder contract. The GPU tier is satisfied by the
// native Firecracker microVM backend or the optional Modal HTTP remote backend.
type Backend interface {
    Name() string                                  // "firecracker", "modal", ...
    Available(ctx context.Context) error           // runtime capability check; nil == usable
    Run(ctx context.Context, spec Spec) (Result, error)
}

// Registry holds the backends registered at startup. Modal registers itself
// only when credentials/config are present (optional backend registration),
// replacing Python's lazy `import modal` guarded by try/except ImportError.
type Registry struct{ backends map[string]Backend }

func (r *Registry) Register(b Backend) { r.backends[b.Name()] = b }
func (r *Registry) Get(name string) (Backend, bool) { b, ok := r.backends[name]; return b, ok }
```

### 10.1 New and Modified Packages

| Package / file | Change |
|------|--------|
| `internal/sandbox/backend.go` | `Backend` interface, `Registry`, `Spec`/`Result` structs, `GPUTier` constants + rate table |
| `internal/sandbox/firecracker.go` | Native GPU tier over `firecracker-microvm/firecracker-go-sdk` (VFIO GPU passthrough); build target: Linux only |
| `internal/sandbox/modal.go` | Optional Modal remote backend over `net/http` (Modal REST API); self-registers when credentials present |
| `internal/sandbox/mount.go` | `validateMountPath` credential-path blocker (uses `path/filepath` + `os`) |
| `internal/sandbox/cost.go` | `EstimateCost`, `ListSandboxCost` (aggregation query via `internal/store`) |
| `internal/store/migrate/` | Add `sandbox_runs` columns + index + `sandbox_gpu_runs` view migration (`database/sql` + modernc driver) |
| `internal/cli/sandbox.go` | `--gpu`, `--env`, `--volume`, `--timeout`, `--no-cost-estimate`, `--yes` on `sandbox run`; `sandbox cost` subcommand; `doctor` gpu check (cobra) |
| `internal/obs/` | `sandbox.gpu.run` span emission (`go.opentelemetry.io/otel`) |
| `internal/credentials/` | Modal token source (already part of the 18-source credential import) |
| `go.mod` | Add `github.com/firecracker-microvm/firecracker-go-sdk`; no Modal module exists (HTTP client only) |

### 10.2 SQLite DDL — Schema Extension

The schema stays SQL; the migration is Go over the `database/sql` API with the pure-Go `modernc.org/sqlite` driver (`CGO_ENABLED=0`, FTS5 built in). SQLite has no `ADD COLUMN IF NOT EXISTS`, so each `ALTER TABLE` is guarded by inspecting the returned error for the `duplicate column name` substring (the idiomatic Go replacement for Python's `try/except OperationalError`).

```sql
-- New columns added to sandbox_runs via the internal/store migration
ALTER TABLE sandbox_runs ADD COLUMN gpu_type          TEXT;
ALTER TABLE sandbox_runs ADD COLUMN modal_function_id TEXT;   -- Modal fn id OR Firecracker VM id
ALTER TABLE sandbox_runs ADD COLUMN modal_cost_usd    REAL;   -- metered GPU-second cost
ALTER TABLE sandbox_runs ADD COLUMN duration_s        REAL;
ALTER TABLE sandbox_runs ADD COLUMN invoking_run_id   TEXT;

-- New index for tag sandbox cost queries
CREATE INDEX IF NOT EXISTS idx_sr_gpu_cost
    ON sandbox_runs(backend, created_at);

-- Convenience view joining sandbox GPU runs to agent runs
CREATE VIEW IF NOT EXISTS sandbox_gpu_runs AS
SELECT
    sr.id              AS sandbox_id,
    sr.command,
    sr.gpu_type,
    sr.modal_cost_usd,
    sr.duration_s,
    sr.status,
    sr.exit_code,
    sr.created_at,
    sr.invoking_run_id,
    r.prompt           AS invoking_prompt
FROM sandbox_runs sr
LEFT JOIN runs r ON r.id = sr.invoking_run_id
WHERE sr.backend IN ('modal', 'firecracker');
```

The Go migration (run inside `internal/store` under the single-writer + WAL contract):

```go
package migrate

import (
    "database/sql"
    "strings"
)

var gpuColumns = []struct{ name, typ string }{
    {"gpu_type", "TEXT"},
    {"modal_function_id", "TEXT"},
    {"modal_cost_usd", "REAL"},
    {"duration_s", "REAL"},
    {"invoking_run_id", "TEXT"},
}

// migrateGPUColumns adds Modal/Firecracker columns to sandbox_runs if absent.
// error-check on "duplicate column name" replaces try/except OperationalError.
func migrateGPUColumns(db *sql.DB) error {
    for _, c := range gpuColumns {
        _, err := db.Exec("ALTER TABLE sandbox_runs ADD COLUMN " + c.name + " " + c.typ)
        if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
            return err
        }
    }
    const ddl = `
        CREATE INDEX IF NOT EXISTS idx_sr_gpu_cost ON sandbox_runs(backend, created_at);
        CREATE VIEW IF NOT EXISTS sandbox_gpu_runs AS
        SELECT sr.id AS sandbox_id, sr.command, sr.gpu_type,
               sr.modal_cost_usd, sr.duration_s, sr.status, sr.exit_code,
               sr.created_at, sr.invoking_run_id
        FROM sandbox_runs sr WHERE sr.backend IN ('modal','firecracker');`
    _, err := db.Exec(ddl)
    return err
}
```

### 10.3 Key Types

Python dataclasses become Go structs; the GPU-tier map becomes typed string constants plus a rate table; the Python tuple `(str, float, int)` becomes a named struct.

```go
package sandbox

// GPUTier is a typed enum of supported GPU tiers (case-insensitive on parse).
type GPUTier string

const (
    GPUNone GPUTier = ""
    GPUT4   GPUTier = "t4"
    GPUA10G GPUTier = "a10g"
    GPUA100 GPUTier = "a100"
    GPUH100 GPUTier = "h100"
)

// gpuSpec maps a tier to its provider GPU string, USD rate/sec, and vRAM (GB).
type gpuSpec struct {
    ProviderName  string
    RatePerSecond float64
    VRAMGB        int
}

var gpuTiers = map[GPUTier]gpuSpec{
    GPUT4:   {"T4", 0.000059, 16},
    GPUA10G: {"A10G", 0.000150, 24},
    GPUA100: {"A100", 0.000214, 40},
    GPUH100: {"H100", 0.000305, 80},
}

const cpuRatePerSecond = 0.0000020

// Mount is a resolved host→guest bind (Python's (host, sandbox, read_only) tuple).
type Mount struct {
    HostPath    string
    SandboxPath string
    ReadOnly    bool
}

// Spec is the complete configuration for a single GPU sandbox run.
type Spec struct {
    Command       []string          // argv to execute inside the guest
    Image         string            // default "python:3.12-slim"
    GPU           GPUTier           // GPUNone == CPU
    Timeout       time.Duration     // enforced at the isolation boundary
    EnvVars       map[string]string // injected; never persisted/logged
    Mounts        []Mount
    UploadFile    string // local path → /workspace/<name>
    InvokingRunID string
}

// Result is the structured outcome of a completed run.
type Result struct {
    ExitCode        int
    Stdout          string
    Stderr          string
    Duration        time.Duration
    ModalFunctionID string  // Modal fn id or Firecracker VM id
    CostUSD         float64
    Truncated       bool // output exceeded the 1 MB cap
}
```

### 10.4 Native GPU tier — Firecracker backend

The strongest tier of the isolation ladder. A microVM is booted with a VFIO-bound GPU passed through to the guest; the command runs inside, output is captured over the vsock/console, and the VM is torn down on `Run` return. `Available` performs the runtime capability check (Linux, `/dev/kvm`, a VFIO device) that replaces any import guard.

```go
//go:build linux

package sandbox

import (
    "context"
    "errors"
    "time"

    fc "github.com/firecracker-microvm/firecracker-go-sdk"
)

type FirecrackerBackend struct {
    kernelImage string
    vfioDevice  string // host GPU bound to vfio-pci
}

func (b *FirecrackerBackend) Name() string { return "firecracker" }

func (b *FirecrackerBackend) Available(ctx context.Context) error {
    // Linux + /dev/kvm + a VFIO-bound GPU. Returns a descriptive error otherwise
    // so the CLI can print remediation (no panic, no stack trace).
    return checkKVMAndVFIO(b.vfioDevice)
}

func (b *FirecrackerBackend) Run(ctx context.Context, spec Spec) (Result, error) {
    for _, m := range spec.Mounts {
        if err := validateMountPath(m.HostPath); err != nil {
            return Result{}, err // ErrBlockedMountPath before any VM boot
        }
    }
    ctx, cancel := context.WithTimeout(ctx, spec.Timeout) // boundary-enforced timeout
    defer cancel()

    cfg := fc.Config{ /* kernel, rootfs from spec.Image, vsock, drives from spec.Mounts */ }
    if spec.GPU != GPUNone {
        // Attach the VFIO GPU device profile for the requested tier.
        attachVFIOGPU(&cfg, b.vfioDevice, gpuTiers[spec.GPU].ProviderName)
    }
    start := time.Now()
    machine, err := fc.NewMachine(ctx, cfg)
    if err != nil {
        return Result{}, err
    }
    if err := machine.Start(ctx); err != nil {
        return Result{}, err
    }
    exit, stdout, stderr, trunc := runGuestCommand(ctx, machine, spec) // 1MB/256KB caps
    _ = machine.StopVMM()
    dur := time.Since(start)

    return Result{
        ExitCode:        exit,
        Stdout:          stdout,
        Stderr:          stderr,
        Duration:        dur,
        ModalFunctionID: machine.Cfg.VMID,
        CostUSD:         EstimateActualCost(spec.GPU, dur),
        Truncated:       trunc,
    }, nil
}

var ErrBlockedMountPath = errors.New("refusing to mount credential path")
```

### 10.5 Optional remote tier — Modal HTTP backend

Modal ships **no Go SDK**, so the remote tier is a thin `net/http` client against the Modal REST API. It self-registers only when a Modal token is resolvable via `internal/credentials`; otherwise the GPU tier is simply unavailable (no import to fail on).

```go
package sandbox

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "time"
)

const modalAPIVersion = "2026-05" // pinned; see NFR-07

type ModalBackend struct {
    http  *http.Client
    base  string // https://api.modal.com
    token string // from internal/credentials; empty => not registered
}

func (b *ModalBackend) Name() string { return "modal" }

func (b *ModalBackend) Available(ctx context.Context) error {
    if b.token == "" {
        return fmt.Errorf("modal backend: no credentials — run: tag credentials add modal")
    }
    return nil
}

func (b *ModalBackend) Run(ctx context.Context, spec Spec) (Result, error) {
    for _, m := range spec.Mounts {
        if err := validateMountPath(m.HostPath); err != nil {
            return Result{}, err
        }
    }
    ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
    defer cancel()

    body, _ := json.Marshal(modalRunRequest{
        Image:       spec.Image,
        GPU:         gpuTiers[spec.GPU].ProviderName, // "" for CPU
        Command:     spec.Command,
        Environment: spec.EnvVars,                    // request field only; never logged
        TimeoutSec:  int(spec.Timeout.Seconds()),
    })
    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/v1/sandboxes", bytes.NewReader(body))
    req.Header.Set("Authorization", "Bearer "+b.token)
    req.Header.Set("Modal-Version", modalAPIVersion)

    start := time.Now()
    resp, err := b.http.Do(req)
    if err != nil {
        return Result{}, err
    }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
        return Result{}, fmt.Errorf("modal authentication failed — configure credentials: tag credentials add modal")
    }
    var out modalRunResponse
    if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
        return Result{}, err
    }
    dur := time.Since(start)
    return Result{
        ExitCode:        out.ExitCode,
        Stdout:          cap1MB(out.Stdout),
        Stderr:          cap256KB(out.Stderr),
        Duration:        dur,
        ModalFunctionID: out.FunctionID,
        CostUSD:         EstimateActualCost(spec.GPU, dur),
        Truncated:       len(out.Stdout) >= 1_000_000,
    }, nil
}
```

### 10.6 Dispatch and registration

Backends are registered at startup; the CLI resolves one by name and depends only on `Backend`. There is no silent fallback — an unavailable backend surfaces an error (FR-01/FR-11).

```go
func BuildRegistry(ctx context.Context, creds *credentials.Store) *Registry {
    r := &Registry{backends: map[string]Backend{}}
    r.Register(&RestrictedBackend{})
    r.Register(&DockerBackend{})
    r.Register(&FirecrackerBackend{ /* kernel, vfioDevice */ })
    if tok := creds.Modal(); tok != "" { // optional backend registration
        r.Register(&ModalBackend{http: &http.Client{}, base: "https://api.modal.com", token: tok})
    }
    return r
}

func Dispatch(ctx context.Context, r *Registry, name string, spec Spec) (Result, error) {
    b, ok := r.Get(name)
    if !ok {
        return Result{}, fmt.Errorf("unknown sandbox backend %q", name)
    }
    if err := b.Available(ctx); err != nil {
        return Result{}, err // never falls back to restricted
    }
    return b.Run(ctx, spec)
}
```

### 10.7 Mount Path Validation

The Python regex list ports to a slice of compiled `*regexp.Regexp`; `Path.expanduser().resolve()` becomes `expandUser` + `filepath.Abs`/`filepath.EvalSymlinks`. On a match, `Run` returns `ErrBlockedMountPath` instead of raising `ValueError`.

```go
package sandbox

import (
    "fmt"
    "path/filepath"
    "regexp"
)

var blockedMountPatterns = []*regexp.Regexp{
    regexp.MustCompile(`(?i).*\.env$`),
    regexp.MustCompile(`(?i).*\.key$`),
    regexp.MustCompile(`(?i).*\.pem$`),
    regexp.MustCompile(`(?i).*secret.*`),
    regexp.MustCompile(`(?i).*credential.*`),
    regexp.MustCompile(`.*/\.ssh(/.*)?$`),
    regexp.MustCompile(`.*/\.aws(/.*)?$`),
    regexp.MustCompile(`.*/\.config/op(/.*)?$`),
    regexp.MustCompile(`(?i).*\.p12$`),
    regexp.MustCompile(`(?i).*\.pfx$`),
    regexp.MustCompile(`.*id_rsa.*`),
    regexp.MustCompile(`.*id_ed25519.*`),
    regexp.MustCompile(`(?i).*\.token$`),
    regexp.MustCompile(`(?i).*vault.*`),
    regexp.MustCompile(`.*/\.gnupg(/.*)?$`),
}

func validateMountPath(hostPath string) error {
    expanded, err := filepath.EvalSymlinks(expandUser(hostPath))
    if err != nil {
        expanded, _ = filepath.Abs(expandUser(hostPath)) // best-effort canonicalization
    }
    for _, p := range blockedMountPatterns {
        if p.MatchString(expanded) {
            return fmt.Errorf("%w: %q (matched %q)", ErrBlockedMountPath, hostPath, p.String())
        }
    }
    return nil
}
```

### 10.8 Cost Estimation — Pre-Run Display

```go
// EstimateCost returns the worst-case USD cost (ceiling at timeout).
func EstimateCost(gpu GPUTier, timeout time.Duration) float64 {
    rate := cpuRatePerSecond
    if s, ok := gpuTiers[gpu]; ok {
        rate = s.RatePerSecond
    }
    return round4(rate * timeout.Seconds())
}

// EstimateActualCost meters the realized cost from a run's duration.
func EstimateActualCost(gpu GPUTier, dur time.Duration) float64 {
    rate := cpuRatePerSecond
    if s, ok := gpuTiers[gpu]; ok {
        rate = s.RatePerSecond
    }
    return round6(rate * dur.Seconds())
}

// ConfirmGPURun prints the estimate and prompts unless CI is set. The CLI layer
// (internal/cli) reads from cmd.InOrStdin() for testability.
func ConfirmGPURun(w io.Writer, in io.Reader, gpu GPUTier, timeout time.Duration) (bool, error) {
    tier := "CPU"
    if gpu != GPUNone {
        tier = strings.ToUpper(string(gpu))
    }
    fmt.Fprintf(w, "Estimated cost: %s GPU for ~%.0fs ceiling → $%.4f USD\n",
        tier, timeout.Seconds(), EstimateCost(gpu, timeout))
    if os.Getenv("CI") != "" {
        return true, nil
    }
    fmt.Fprint(w, "Proceed? [y/N]: ")
    var ans string
    if _, err := fmt.Fscanln(in, &ans); err != nil {
        return false, nil
    }
    ans = strings.ToLower(strings.TrimSpace(ans))
    return ans == "y" || ans == "yes", nil
}
```

### 10.9 OTel Integration (PRD-013)

Hand-rolled OTLP is replaced by `go.opentelemetry.io/otel` spans emitted through `internal/obs`. Env-var values are never set as attributes (NFR-05).

```go
func emitGPUSpan(ctx context.Context, r Result, spec Spec) {
    tr := otel.Tracer("tag.sandbox")
    _, span := tr.Start(ctx, "sandbox.gpu.run")
    defer span.End()

    backend := "firecracker"
    if spec.GPU != GPUNone && r.ModalFunctionID != "" && strings.HasPrefix(r.ModalFunctionID, "fn-") {
        backend = "modal"
    }
    gpu := "cpu"
    if spec.GPU != GPUNone {
        gpu = string(spec.GPU)
    }
    span.SetAttributes(
        attribute.String("sandbox.backend", backend),
        attribute.String("sandbox.gpu_type", gpu),
        attribute.Float64("sandbox.cost_usd", r.CostUSD),
        attribute.Int("sandbox.exit_code", r.ExitCode),
        attribute.Float64("sandbox.duration_s", r.Duration.Seconds()),
    )
    if r.ModalFunctionID != "" {
        span.SetAttributes(attribute.String("sandbox.modal_function_id", r.ModalFunctionID))
    }
    if spec.InvokingRunID != "" {
        span.SetAttributes(attribute.String("sandbox.invoking_run_id", spec.InvokingRunID))
    }
}
```

### 10.10 `tag sandbox cost` Implementation

```go
type CostRow struct {
    GroupKey       string  `json:"group_key"`
    RunCount       int     `json:"run_count"`
    TotalDurationS float64 `json:"total_duration_s"`
    TotalCostUSD   float64 `json:"total_cost_usd"`
}

func ListSandboxCost(ctx context.Context, db *sql.DB, since, until, groupBy string) ([]CostRow, error) {
    groupExpr, ok := map[string]string{
        "gpu_type":        "COALESCE(gpu_type, 'cpu')",
        "date":            "date(created_at)",
        "invoking_run_id": "COALESCE(invoking_run_id, 'direct')",
    }[groupBy]
    if !ok {
        return nil, fmt.Errorf("group_by must be one of gpu_type|date|invoking_run_id")
    }

    where := []string{"backend IN ('modal','firecracker')"}
    var args []any
    if since != "" {
        where = append(where, "created_at >= ?")
        args = append(args, since)
    }
    if until != "" {
        where = append(where, "created_at <= ?")
        args = append(args, until+"T23:59:59Z")
    }
    q := fmt.Sprintf(`
        SELECT %s AS group_key, COUNT(*) AS run_count,
               SUM(COALESCE(duration_s,0)) AS total_duration_s,
               SUM(COALESCE(modal_cost_usd,0)) AS total_cost_usd
        FROM sandbox_runs WHERE %s
        GROUP BY %s ORDER BY total_cost_usd DESC`,
        groupExpr, strings.Join(where, " AND "), groupExpr)

    rows, err := db.QueryContext(ctx, q, args...)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var out []CostRow
    for rows.Next() {
        var r CostRow
        if err := rows.Scan(&r.GroupKey, &r.RunCount, &r.TotalDurationS, &r.TotalCostUSD); err != nil {
            return nil, err
        }
        out = append(out, r)
    }
    return out, rows.Err()
}
```

### 10.11 `internal/cli` Integration Points (cobra)

Python's argparse subparsers become cobra commands under `internal/cli`; `action="append"` flags become `StringArrayVar`.

1. **`sandbox run` flags** — register `--gpu`, `--env` (`StringArrayVar`), `--volume` (`StringArrayVar`), `--timeout`, `--no-cost-estimate`, `--yes` on the `sandbox run` cobra command; parse `--gpu` into a `GPUTier`, rejecting unknown tiers with a message listing valid values (FR/NFR-09).

2. **Pre-dispatch cost prompt** — before `Dispatch`, for a GPU backend:

```go
if isGPUBackend(backend) && !noCostEstimate {
    ok, _ := sandbox.ConfirmGPURun(cmd.OutOrStdout(), cmd.InOrStdin(), gpu, timeout)
    if !yes && !ok {
        fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
        return nil
    }
}
```

3. **`sandbox cost` subcommand** — a cobra command wired to `ListSandboxCost`:

```go
func newSandboxCostCmd(store *store.DB) *cobra.Command {
    var since, until, groupBy string
    var asJSON bool
    cmd := &cobra.Command{Use: "cost", Short: "Summarise GPU sandbox spend",
        RunE: func(cmd *cobra.Command, _ []string) error {
            rows, err := sandbox.ListSandboxCost(cmd.Context(), store.DB(), since, until, groupBy)
            if err != nil {
                return err
            }
            if asJSON {
                return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
            }
            return printCostTable(cmd.OutOrStdout(), rows)
        }}
    cmd.Flags().StringVar(&since, "since", "", "start date YYYY-MM-DD")
    cmd.Flags().StringVar(&until, "until", "", "end date YYYY-MM-DD")
    cmd.Flags().StringVar(&groupBy, "group-by", "gpu_type", "gpu_type|date|run_id")
    cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
    return cmd
}
```

4. **`doctor` extension** — a `gpu_sandbox` check that iterates the registry and reports each GPU backend's `Available(ctx)`:

```go
func checkGPUSandbox(ctx context.Context, r *sandbox.Registry) []DoctorResult {
    var out []DoctorResult
    for _, name := range []string{"firecracker", "modal"} {
        b, ok := r.Get(name)
        if !ok {
            out = append(out, DoctorResult{name, "warn", "backend not registered"})
            continue
        }
        if err := b.Available(ctx); err != nil {
            out = append(out, DoctorResult{name, "warn", err.Error()})
        } else {
            out = append(out, DoctorResult{name, "ok", "ready"})
        }
    }
    return out
}
```

---

## 11. Security Considerations

1. **Credential path blocking** — all 15 patterns in `blockedMountPatterns` are enforced (via `validateMountPath`) before any microVM boot or Modal HTTP call. The check canonicalizes with `filepath.EvalSymlinks` plus `~` expansion before matching, preventing bypass via relative paths or symlink chains.

2. **Environment variable secrecy** — `--env` values live only in `Spec.EnvVars` and are injected into the guest environment (Firecracker) or the Modal HTTP request `environment` field. They are never written to the `command` column of `sandbox_runs`, never logged, and never set as OTel span attributes. The `sandbox_runs.command` column stores only the user-supplied `--code` string or `--file` path, not the env vars.

3. **Provider credential isolation** — for the Modal remote backend, TAG resolves the token through `internal/credentials` (the user's existing Modal token); TAG never copies or persists it. If absent, `Available` returns a clear error directing the user to configure credentials. The native Firecracker tier needs no cloud credential at all — a further isolation win of the Go move.

4. **Output truncation** — stdout is capped at 1 MB and stderr at 256 KB before being written to SQLite. This prevents runaway sandbox output from filling the local database. A `Truncated` field is set in the `Result` and surfaced in `--json` output.

5. **Timeout enforcement at the isolation boundary** — the timeout is a `context.WithTimeout` deadline propagated into `Run`, enforced as the Firecracker VM lifetime or the Modal function timeout — not merely a local wall-clock check. This ensures the GPU is released even if the TAG process is killed or the network connection drops.

6. **PRD-034 secret scanning** — the `--code` inline string and uploaded `--file` content are NOT scanned for secrets (that would require PRD-034's scanner, out of scope here), but the volume-path blocker ensures credential files cannot be read from disk. Users are warned in the CLI help text that `--code` content sent to the Modal remote backend leaves the host and should not contain plaintext secrets. (The native Firecracker tier keeps code on-host.)

7. **Provider workload isolation** — Modal remote runs are submitted under a dedicated Modal app/namespace (`tag-sandbox`), isolating TAG's traffic from other user apps and simplifying billing attribution. Native Firecracker runs are isolated per-microVM by hardware virtualization.

8. **No persistent sandbox sessions** — each `tag sandbox run` provisions a fresh ephemeral microVM (or Modal sandbox) that is torn down on completion. No sandbox is reused across runs, eliminating any risk of cross-run data leakage.

---

## 12. Testing Strategy

Go standard `testing` with table-driven cases. The sandbox backend is mocked via a Go `fakeBackend` that satisfies the `Backend` interface (dependency injection) — no monkeypatching, no live KVM, no live network. Integration tests are gated behind a build tag (`//go:build integration`) and an env guard.

### 12.1 Unit Tests (`internal/sandbox/*_test.go`)

```go
package sandbox

import (
    "context"
    "errors"
    "testing"
    "time"
)

// fakeBackend is injected in place of firecracker/modal to test dispatch,
// cost, env-secrecy, and persistence without KVM or the network.
type fakeBackend struct {
    name   string
    avail  error
    result Result
    ran    bool
}

func (f *fakeBackend) Name() string                        { return f.name }
func (f *fakeBackend) Available(context.Context) error     { return f.avail }
func (f *fakeBackend) Run(context.Context, Spec) (Result, error) {
    f.ran = true
    return f.result, nil
}

func TestDispatchNeverFallsBackToRestricted(t *testing.T) {
    gpu := &fakeBackend{name: "firecracker", result: Result{ExitCode: 0}}
    r := &Registry{backends: map[string]Backend{"firecracker": gpu}}
    res, err := Dispatch(context.Background(), r, "firecracker", Spec{GPU: GPUA10G})
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if !gpu.ran || res.ExitCode != 0 {
        t.Fatal("GPU backend must run; no restricted fallback")
    }
}

func TestUnavailableBackendReturnsError(t *testing.T) {
    gpu := &fakeBackend{name: "firecracker", avail: errors.New("no /dev/kvm")}
    r := &Registry{backends: map[string]Backend{"firecracker": gpu}}
    if _, err := Dispatch(context.Background(), r, "firecracker", Spec{}); err == nil {
        t.Fatal("expected error when backend unavailable")
    }
}

func TestGPUTierMapping(t *testing.T) {
    for _, tc := range []struct {
        tier GPUTier
        want string
    }{{GPUT4, "T4"}, {GPUA10G, "A10G"}, {GPUA100, "A100"}, {GPUH100, "H100"}} {
        if got := gpuTiers[tc.tier].ProviderName; got != tc.want {
            t.Errorf("%s: got %q want %q", tc.tier, got, tc.want)
        }
    }
}

func TestBlockedMountPaths(t *testing.T) {
    for _, p := range []string{
        "~/.ssh/id_rsa", "~/.aws/credentials", "~/.env", "/secrets/api.key",
        "/home/user/.config/op/config", "/data/vault.json",
        "/app/secret_token", "./credentials.pem",
    } {
        if err := validateMountPath(p); !errors.Is(err, ErrBlockedMountPath) {
            t.Errorf("%q: expected ErrBlockedMountPath, got %v", p, err)
        }
    }
}

func TestEnvVarsNeverPersisted(t *testing.T) {
    // run_in_sandbox equivalent persists Spec.Command, never Spec.EnvVars.
    db := newTestDB(t) // modernc.org/sqlite in-memory/tmp with WAL
    spec := Spec{Command: []string{"python3", "-c", "pass"}, GPU: GPUT4,
        EnvVars: map[string]string{"SECRET_KEY": "super-secret"}}
    persistRun(db, spec, Result{})
    var command string
    _ = db.QueryRow("SELECT command FROM sandbox_runs LIMIT 1").Scan(&command)
    if strings.Contains(command, "super-secret") {
        t.Fatal("env var leaked into command column")
    }
}

func TestCostComputedFromDuration(t *testing.T) {
    got := EstimateActualCost(GPUA100, 10*time.Second)
    if got <= 0 {
        t.Fatalf("cost must be > 0, got %v", got)
    }
}
```

CLI-level table tests (in `internal/cli`) exercise mutual-exclusion and missing-input errors by invoking the cobra command with a captured buffer and asserting the returned error/exit code (`--file`+`--code` → "mutually exclusive"; neither → "gpu sandbox requires --code or --file"; unknown backend/credentials → descriptive error, no stack trace).

### 12.2 Integration Tests

Gated behind `//go:build integration` and skipped unless the backend is actually available. Firecracker tests require Linux + `/dev/kvm` + a VFIO GPU; Modal tests require a configured token.

```go
//go:build integration

package sandbox

import (
    "context"
    "os"
    "strings"
    "testing"
    "time"
)

func TestFirecrackerT4CUDA(t *testing.T) {
    b := &FirecrackerBackend{ /* kernel, vfioDevice from env */ }
    if err := b.Available(context.Background()); err != nil {
        t.Skipf("firecracker unavailable: %v", err)
    }
    res, err := b.Run(context.Background(), Spec{
        Command: []string{"python3", "-c", "import torch; print(torch.cuda.is_available())"},
        GPU:     GPUT4, Image: "pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime",
        Timeout: 120 * time.Second,
    })
    if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "True") {
        t.Fatalf("CUDA not available: err=%v code=%d out=%q", err, res.ExitCode, res.Stdout)
    }
}

func TestModalGPUTiers(t *testing.T) {
    if os.Getenv("MODAL_TOKEN") == "" {
        t.Skip("Modal creds not set")
    }
    b := &ModalBackend{ /* http, base, token from env */ }
    for _, tier := range []GPUTier{GPUT4, GPUA10G} {
        res, err := b.Run(context.Background(), Spec{
            Command: []string{"python3", "-c", "print('ok')"}, GPU: tier, Timeout: 90 * time.Second})
        if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "ok") || res.CostUSD <= 0 {
            t.Fatalf("tier %s failed: err=%v res=%+v", tier, err, res)
        }
    }
}
```

### 12.3 Performance Tests

```go
func TestSandboxCostQueryLargeTable(t *testing.T) {
    db := newTestDB(t) // modernc.org/sqlite with idx_sr_gpu_cost applied
    tiers := []string{"t4", "a10g", "a100", "h100"}
    tx, _ := db.Begin()
    stmt, _ := tx.Prepare(`INSERT INTO sandbox_runs
        (id,command,backend,status,exit_code,created_at,completed_at,gpu_type,
         modal_function_id,modal_cost_usd,duration_s) VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
    for i := 0; i < 100_000; i++ {
        day := (i % 28) + 1
        ts := fmt.Sprintf("2026-06-%02dT10:00:00Z", day)
        _, _ = stmt.Exec(fmt.Sprintf("id%d", i), "python3 -c 'pass'", "modal", "done", 0,
            ts, ts, tiers[i%4], fmt.Sprintf("fn-%d", i),
            rand.Float64(), 10+rand.Float64()*3590)
    }
    _ = tx.Commit()

    start := time.Now()
    rows, err := ListSandboxCost(context.Background(), db, "", "", "gpu_type")
    if err != nil {
        t.Fatal(err)
    }
    if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
        t.Fatalf("query took %v, want <200ms", elapsed)
    }
    if len(rows) != 4 {
        t.Fatalf("want 4 tier rows, got %d", len(rows))
    }
}
```

---

## 13. Acceptance Criteria

| ID | Criterion | Test Reference |
|----|-----------|----------------|
| AC-01 | `tag sandbox run --backend firecracker --gpu a10g --code "print('hi')" --yes` exits 0 and prints `hi` in the output section. | `TestFirecrackerA10GInline` (integration) |
| AC-02 | `tag sandbox run --backend firecracker --gpu h100 --file train.py --yes` creates a `sandbox_runs` row with `backend='firecracker'`, `gpu_type='h100'`, non-NULL `modal_cost_usd`. | `TestFirecrackerH100File` (integration) |
| AC-03 | When no GPU backend is available (no `/dev/kvm`, no Modal credentials), `tag sandbox run --gpu t4 --code "x"` prints a descriptive capability error with remediation and exits 1 (no stack trace). | `TestUnavailableBackendReturnsError` |
| AC-04 | When the Modal HTTP API returns 401/403, `tag sandbox run --backend modal --gpu t4 --code "x"` prints a message directing the user to configure credentials and exits 1. | `TestModalAuthError` |
| AC-05 | `--volume ~/.ssh:/host-ssh` is rejected before any microVM boot / Modal call with exit code 1 and message `refusing to mount credential path`. | `TestBlockedMountPaths` |
| AC-06 | `--env SECRET=abc` values do NOT appear in the `command` column of `sandbox_runs`. | `TestEnvVarsNeverPersisted` |
| AC-07 | `tag sandbox cost --group-by gpu_type` returns a table with columns `GPU`, `Runs`, `Total Duration`, `Total Cost`. | `TestSandboxCostOutputFormat` |
| AC-08 | `tag sandbox cost` completes in <200ms on a 100,000-row `sandbox_runs` table. | `TestSandboxCostQueryLargeTable` |
| AC-09 | `modal_cost_usd` in `sandbox_runs` equals `duration_s * gpuTiers[gpu].RatePerSecond` to within ±1% for all four GPU tiers. | `TestCostAccuracyPerTier` |
| AC-10 | `tag sandbox run --gpu t4 --code "x" --file y.py` exits 1 with `--file and --code are mutually exclusive`. | `TestFileAndCodeMutuallyExclusive` |
| AC-11 | `tag sandbox run --backend firecracker --gpu t4` (no `--code` or `--file`) exits 1 with `gpu sandbox requires --code or --file`. | `TestNoCodeNoFileErrors` |
| AC-12 | `tag doctor` includes a `gpu_sandbox` check that reports each GPU backend's `Available` status (green when Firecracker or Modal is usable). | `TestDoctorGPUSandboxCheck` |
| AC-13 | `tag sandbox run --gpu t4 --code "print('x')" --yes --json` emits valid JSON with fields `id`, `exit_code`, `output`, `gpu_type`, `modal_cost_usd`. | `TestJSONOutputFields` |
| AC-14 | Without `--yes` and with `CI` unset, a confirmation prompt is printed before any microVM boot / Modal call; answering `N` aborts without provisioning anything. | `TestCostPromptAbortNoRun` |
| AC-15 | OTel span `sandbox.gpu.run` is emitted with attributes `sandbox.gpu_type` and `sandbox.cost_usd` when tracing is configured. | `TestOTelSpanEmitted` |
| AC-16 | All four GPU tiers (`t4`, `a10g`, `a100`, `h100`) are accepted; an invalid tier (e.g., `a9000`) exits 1 with a message listing valid tiers. | `TestGPUTierMapping`, `TestInvalidGPUTier` |
| AC-17 | `sandbox_runs` after a GPU run has non-NULL `gpu_type`, `modal_function_id`, `duration_s`, and `modal_cost_usd`. | `TestCostComputedFromDuration` |

---

## 14. Dependencies

| Dependency | Version | Type | Notes |
|------------|---------|------|-------|
| `github.com/firecracker-microvm/firecracker-go-sdk` | latest GA | Go module (Apache-2.0) | Native GPU tier — microVMs with VFIO GPU passthrough; Linux + `/dev/kvm` at runtime |
| Modal REST API | pinned `Modal-Version` const | HTTP (no Go SDK) | Optional remote backend via `net/http`; no dependency added, credentials via `internal/credentials` |
| `modernc.org/sqlite` | GA | Go module (BSD-3, pure-Go) | Project-wide SQLite driver (`CGO_ENABLED=0`, FTS5 built in) via `internal/store` |
| `go.opentelemetry.io/otel` + `/sdk` | GA | Go module (Apache-2.0) | `sandbox.gpu.run` span emission via `internal/obs` |
| `github.com/spf13/cobra` | GA | Go module (Apache-2.0) | `sandbox run`/`sandbox cost` command tree in `internal/cli` |
| PRD-028 | Implemented | Internal | `sandbox_runs` schema, `Backend` interface, isolation ladder |
| PRD-013 | Optional | Internal | OTel tracing; graceful no-op if tracing not configured |
| PRD-012 | Optional | Internal | `tag budget` aggregation; no code dependency, only data convention |
| PRD-034 | None (awareness) | Internal | Blocked mount patterns inherited from PRD-034 security patterns |
| PRD-005 | Awareness | Internal | Execution backend selection context; no code dependency |
| Go stdlib (`net/http`, `path/filepath`, `regexp`, `database/sql`, `context`, `testing`) | 1.24+ | Runtime/Test | Modal HTTP client, mount validation, migrations, table-driven tests — no third-party test/mocking lib needed (fake `Backend` via interface) |

---

## 15. Open Questions

| ID | Question | Owner | Resolution Target |
|----|----------|-------|-------------------|
| OQ-01 | What is the exact VFIO device-profile mapping per GPU tier for `firecracker-go-sdk` (kernel args, PCI passthrough config), and the corresponding JSON request shape for the Modal REST `/v1/sandboxes` endpoint (GPU string field)? Both need verification (Firecracker GPU-passthrough docs; Modal API reference). | Engineering | Sprint 1, Day 1 |
| OQ-02 | Should `modal_cost_usd` use the static `gpuTiers` rate table or, for the Modal remote backend, read post-run actuals from the Modal usage endpoint? A billing call may introduce latency. | Product | Sprint 1, Day 3 |
| OQ-03 | What is the correct guest rootfs/image for T4 GPU runs requiring CUDA? `pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime` is the current heuristic but may lag new PyTorch releases. Should TAG hard-code or expose `--image`? | Engineering | Sprint 1, Day 2 |
| OQ-04 | For `--volume`, should the Firecracker tier use a virtio-fs share or a block device, and should the Modal remote backend upload as a multipart HTTP payload or a pre-staged bucket? Trade simplicity vs. large-file limits (~500 MB). | Engineering | Sprint 1, Day 3 |
| OQ-05 | Should a GPU backend (`firecracker`/`modal`) with no `--gpu` (CPU-only) be a supported use case? PRD-028 envisioned these for GPU, but CPU microVMs/sandboxes are valid. The `--gpu` flag would be optional. | Product | Sprint 1, Day 1 |
| OQ-06 | Should `invoking_run_id` be auto-populated from the current run context threaded through `internal/runtime` (via `context.Context` or a `TAG_RUN_ID` env var)? This would enable zero-friction attribution. | Engineering | Sprint 2, Day 1 |
| OQ-07 | How should a GPU run with `--file train.py` handle large uploads (e.g., a 200 MB checkpoint)? The Modal HTTP upload and the Firecracker mount both have practical limits; should there be a size guard? | Engineering | Sprint 2, Day 2 |
| OQ-08 | Should `tag sandbox cost` roll up into `tag budget status`? This would require the budget command (`internal/cli` + `internal/obs`) to query `sandbox_runs` in addition to `runs`. Scope of budget integration is TBD. | Product | Sprint 2, Day 3 |
| OQ-09 | Live stdout streaming from the guest — for Firecracker over vsock/console, and for Modal over a streaming HTTP response — is this achievable in v1 by piping output through a Go channel/goroutine, or should streaming be a hard non-goal until a follow-on PRD? | Engineering | Sprint 1, Day 4 |

---

## 16. Complexity and Timeline

**Estimated Effort:** M (1-2 weeks, 1 engineer)

### Phase 1 — Backend interface + GPU tiers (Days 1-4)

| Day | Task |
|-----|------|
| 1 | Confirm Firecracker VFIO GPU-passthrough config and the Modal REST request shape (OQ-01, OQ-03, OQ-04). Define the `Backend` interface, `Registry`, `Spec`/`Result` structs, `GPUTier` constants, and the `gpuTiers` rate table in `internal/sandbox/backend.go`. |
| 2 | Implement the Firecracker GPU backend (`internal/sandbox/firecracker.go`, `//go:build linux`) with tier→VFIO mapping, env injection, context-deadline timeout, output capture. Implement `validateMountPath` with all 15 blocked patterns (`mount.go`). |
| 3 | Implement the optional Modal HTTP backend (`modal.go`) + registration/capability checks. Add the `internal/store` migration (`migrateGPUColumns`, guarded by `duplicate column name`). Implement `Dispatch`, `EstimateCost`, `ConfirmGPURun`. |
| 4 | Add `ListSandboxCost`. Write table-driven unit tests with the injected `fakeBackend`. Achieve ≥90% coverage with no KVM/network. |

### Phase 2 — CLI Surface and Integration (Days 5-8)

| Day | Task |
|-----|------|
| 5 | Add `--gpu`, `--env`, `--volume`, `--no-cost-estimate`, `--yes` to the `sandbox run` cobra command in `internal/cli`. Add the `sandbox cost` subcommand. Wire the `--json` output path. |
| 6 | Add the `gpu_sandbox` check to `tag doctor`. Add the `--file` guest-upload path. Handle `--file`/`--code` mutual exclusion. |
| 7 | Add OTel span emission via `internal/obs` (`sandbox.gpu.run`). Add the `sandbox_gpu_runs` view to the migration. Thread `invoking_run_id` from `internal/runtime` via `context.Context`. |
| 8 | Write `//go:build integration` tests with backend-availability skip guards. Write `TestSandboxCostQueryLargeTable`. Manual smoke test on T4 and A10G (Firecracker where KVM+GPU present; Modal where credentialed). |

### Phase 3 — Polish and Review (Days 9-10)

| Day | Task |
|-----|------|
| 9 | Address OQ-02 (billing endpoint vs static table), OQ-08 (budget rollup). Add `firecracker-go-sdk` to `go.mod` and tidy. Write changelog entry. |
| 10 | Code review, fix review feedback, run `go test ./...`. Update `docs/prd/INDEX.md` with PRD-093. Close GitHub issue #348. |

---

*End of PRD-093*

