# PRD-093: GPU Sandbox via Modal Backend (Complete the Modal Integration Stub) (`tag sandbox run --backend modal --gpu`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py (modal backend stub)`
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing & Observability), PRD-034 (Security / Secret Scanning), PRD-012 (Cost Tracking & Budget), PRD-005 (Execution Backend Selection), PRD-039 (Token Budget Enforcement)
**Inspired by:** Modal GPU functions, E2B GPU (coming), Vast.ai

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) established three execution backends — restricted subprocess, Docker, and Modal — with the Modal backend landing as a documented stub: the `BACKENDS` constant lists `"modal"`, the schema records it, but `run_in_sandbox()` dispatches any `backend == "modal"` request to the restricted subprocess path without invoking Modal at all. This PRD specifies the complete implementation of the Modal backend, including GPU type selection, environment variable injection, host-directory volume mounts, structured exit-code and output capture, and per-run cost attribution written back to SQLite.

The driving use case is ML practitioners who use TAG as an agent orchestration layer and want to run GPU-dependent code — PyTorch training loops, CUDA kernel benchmarks, HuggingFace inference — inside a cloud sandbox without standing up their own GPU infrastructure. Modal provides on-demand GPU functions billed per second, with H100 capacity available in under 60 seconds in most regions. TAG already depends on Modal for agent execution backend selection (controller.py reads `execution.backend: modal` from profile YAML); the sandbox extension reuses that credential surface and SDK import.

The four GPU tiers targeted — T4, A10G, A100, H100 — cover the full cost/capability spectrum from $0.000059/GPU-second (T4, ~$0.21/hr) to $0.000305/GPU-second (H100, ~$1.10/hr). A cost-estimation call fires before each GPU run and prints a projected cost alongside a confirmation prompt unless `--yes` is passed or `CI=true` is set, matching the pattern established by `tag eval run`. Every run writes a `modal_cost_usd` column to `sandbox_runs` so that `tag budget` can aggregate GPU spend alongside LLM spend.

The implementation touches four files: `src/tag/sandbox.py` (the Modal backend itself), `src/tag/controller.py` (new `--gpu`, `--env`, `--volume`, `--timeout` flags on `tag sandbox run`), and the `sandbox_runs` SQLite schema (three new columns). No new top-level module is introduced; Modal remains an optional import guarded by a `try/except`, consistent with PRD-028's zero-mandatory-dependency goal.

GPU sandbox runs are attributed to the calling agent run or queue job via the existing `run_id` / `job_id` context already threaded through `controller.py`. A new `sandbox_gpu_runs` view joins `sandbox_runs` with the `runs` table on `invoking_run_id`, allowing `tag trace` to show GPU sandbox invocations as child spans of agent runs.

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
| FR-01 | `_run_modal()` MUST call `modal.Sandbox.create()` (or equivalent current Modal API) and NOT fall back to `_run_restricted()`. | P0 |
| FR-02 | When `gpu` is not `None`, the Modal function/sandbox MUST be created with the corresponding GPU type string from the mapping table (`t4` → `"T4"`, `a10g` → `"A10G"`, `a100` → `"A100"`, `h100` → `"H100"`). | P0 |
| FR-03 | When `gpu` is `None` and `--backend modal` is requested, the sandbox MUST run on CPU only (no `gpu=` argument to Modal). | P0 |
| FR-04 | `--env KEY=VALUE` arguments MUST be passed to the Modal sandbox as the `environment` dict parameter; they MUST NOT be written to any file or logged. | P0 |
| FR-05 | `--volume HOST_PATH:SANDBOX_PATH` arguments MUST be mounted using `modal.CloudBucketMount` or `modal.Mount.from_local_dir()` as appropriate; the mount MUST be read-write unless `--volume` is suffixed with `:ro`. | P1 |
| FR-06 | Volume HOST_PATH values that match any PRD-028 blocked pattern (`*.env`, `*.key`, `*.pem`, `*secret*`, `*credential*`, `~/.ssh/*`, `~/.aws/*`, `~/.config/op/*`) MUST raise `ValueError` before any Modal call is made. | P0 |
| FR-07 | `run_in_sandbox()` MUST return `exit_code`, `stdout` (up to 1 MB, truncated with warning), and `stderr` (up to 256 KB) from the Modal sandbox execution. | P0 |
| FR-08 | The `sandbox_runs` table MUST be extended with columns `gpu_type TEXT`, `modal_function_id TEXT`, `modal_cost_usd REAL`, `duration_s REAL`, `invoking_run_id TEXT` before the first Modal write. | P0 |
| FR-09 | `modal_cost_usd` MUST be computed from `duration_s * GPU_RATE_PER_SECOND[gpu_type]` and written to `sandbox_runs` after each completed or failed run. | P1 |
| FR-10 | Before dispatching to Modal, `tag sandbox run` MUST print an estimated cost (ceiling at `--timeout` seconds) and prompt for `y/N` confirmation unless `--yes` is passed or `CI` environment variable is non-empty. | P1 |
| FR-11 | If Modal is not installed (`ImportError`), `_run_modal()` MUST raise `RuntimeError("modal SDK not installed — pip install modal")` and the CLI MUST print a user-friendly error with install instructions. | P0 |
| FR-12 | If Modal credentials are absent or expired, `_run_modal()` MUST catch the Modal authentication error and re-raise with a message directing the user to run `modal token new`. | P0 |
| FR-13 | `tag sandbox cost` MUST query only `sandbox_runs WHERE backend='modal'` and present aggregated results by the requested `--group-by` dimension. | P1 |
| FR-14 | `tag sandbox cost` MUST complete in under 200ms for a `sandbox_runs` table of up to 100,000 rows (requires index on `(backend, created_at)`). | P1 |
| FR-15 | `tag sandbox run --file <path>` MUST read the file, upload it to the Modal sandbox as `/workspace/<filename>`, and execute it with `python3 /workspace/<filename>` (or the shebang interpreter if present). | P1 |
| FR-16 | The Modal sandbox MUST have `--timeout` enforced as the Modal function/sandbox timeout, not just a local subprocess timeout. | P0 |
| FR-17 | An OpenTelemetry span named `sandbox.modal.run` MUST be emitted for every Modal run with attributes: `sandbox.backend`, `sandbox.gpu_type`, `sandbox.cost_usd`, `sandbox.exit_code`, `sandbox.duration_s`, `sandbox.modal_function_id`. | P2 |
| FR-18 | `tag doctor` MUST include a `modal_gpu` check that verifies: (a) `modal` importable, (b) token file present, (c) optional network ping to `modal.com`. | P2 |
| FR-19 | `--json` flag on `tag sandbox run` MUST emit a single JSON object to stdout (not mixed with progress lines) containing all fields from the `sandbox_runs` row plus `gpu_type`, `modal_function_id`, `modal_cost_usd`, `duration_s`. | P1 |
| FR-20 | Wall-clock `duration_s` MUST be measured from before the `modal.Sandbox.create()` call to after the final `.wait()` / `.poll()` call and written to `sandbox_runs`. | P1 |
| FR-21 | When `--file` and `--code` are both specified, the CLI MUST error with `"--file and --code are mutually exclusive"`. | P0 |
| FR-22 | When neither `--file` nor `--code` is specified and `--backend modal` is passed, the CLI MUST error with `"modal backend requires --code or --file"`. | P0 |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Cold-start latency for a T4 sandbox running a 1-line Python print is ≤90s end-to-end (network + container pull + execution). | p50 ≤90s |
| NFR-02 | Warm-start latency (same image, same region, second run within 5 min) is ≤15s. | p50 ≤15s |
| NFR-03 | The Modal SDK import MUST be lazy (inside `_run_modal()` function body), keeping `sandbox.py` importable in zero-Modal environments without triggering `ImportError`. | Always |
| NFR-04 | `sandbox.py` MUST NOT import `modal` at module level; the import MUST live inside `_run_modal()` guarded by `try/except ImportError`. | Always |
| NFR-05 | Environment variables passed via `--env` MUST NOT appear in the `command` column of `sandbox_runs`, in log output, or in OTel span attributes. | Always |
| NFR-06 | `modal_cost_usd` MUST be accurate to within ±5% of Modal's actual billing for a given run duration using the static rate table. | ±5% |
| NFR-07 | The implementation MUST be compatible with Modal SDK versions ≥0.60.0 (current stable as of 2026-Q2). Deprecation of the `Sandbox` API must be detected at import time and a clear error surfaced. | Always |
| NFR-08 | No network call is made to Modal until after the user confirms the cost prompt (or `--yes`/`CI` bypasses it). | Always |
| NFR-09 | `tag sandbox run` with `--backend modal` and invalid GPU tier MUST fail with exit code 1 and a human-readable message listing valid tiers — no stack trace exposed. | Always |
| NFR-10 | All new code in `sandbox.py` MUST achieve ≥90% line coverage in `tests/test_sandbox_modal.py` using `pytest-mock` to mock the `modal` module. | ≥90% |

---

## 10. Technical Design

### 10.1 New and Modified Files

| File | Change |
|------|--------|
| `src/tag/sandbox.py` | Add `_run_modal()`, extend `ensure_schema()`, extend `run_in_sandbox()` dispatch, add `list_sandbox_cost()` |
| `src/tag/controller.py` | Add `--gpu`, `--env`, `--volume`, `--no-cost-estimate` flags to `cmd_sandbox_run`; add `cmd_sandbox_cost` subcommand; extend `cmd_doctor` with `modal_gpu` check |
| `pyproject.toml` | Add `modal` to `[project.optional-dependencies]` under key `modal` (already listed in `extras` per PRD-028, verify it is present) |
| `tests/test_sandbox_modal.py` | New test file (see §11) |

### 10.2 SQLite DDL — Schema Extension

The existing `ensure_schema()` function is extended to add new columns via `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` idiom (SQLite does not support `IF NOT EXISTS` on `ALTER TABLE`, so the migration uses a `try/except sqlite3.OperationalError` guard):

```sql
-- New columns added to sandbox_runs via migration in ensure_schema()
ALTER TABLE sandbox_runs ADD COLUMN gpu_type          TEXT;
ALTER TABLE sandbox_runs ADD COLUMN modal_function_id TEXT;
ALTER TABLE sandbox_runs ADD COLUMN modal_cost_usd    REAL;
ALTER TABLE sandbox_runs ADD COLUMN duration_s        REAL;
ALTER TABLE sandbox_runs ADD COLUMN invoking_run_id   TEXT;

-- New index for tag sandbox cost queries
CREATE INDEX IF NOT EXISTS idx_sr_modal_cost
    ON sandbox_runs(backend, created_at)
    WHERE backend = 'modal';

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
WHERE sr.backend = 'modal';
```

The `ensure_schema()` migration code:

```python
def _migrate_modal_columns(conn: sqlite3.Connection) -> None:
    """Add Modal-specific columns to sandbox_runs if they do not exist."""
    new_columns = [
        ("gpu_type",          "TEXT"),
        ("modal_function_id", "TEXT"),
        ("modal_cost_usd",    "REAL"),
        ("duration_s",        "REAL"),
        ("invoking_run_id",   "TEXT"),
    ]
    for col_name, col_type in new_columns:
        try:
            conn.execute(f"ALTER TABLE sandbox_runs ADD COLUMN {col_name} {col_type}")
        except sqlite3.OperationalError:
            pass  # column already exists
    conn.executescript("""
        CREATE INDEX IF NOT EXISTS idx_sr_modal_cost
            ON sandbox_runs(backend, created_at);
        CREATE VIEW IF NOT EXISTS sandbox_gpu_runs AS
        SELECT sr.id AS sandbox_id, sr.command, sr.gpu_type,
               sr.modal_cost_usd, sr.duration_s, sr.status, sr.exit_code,
               sr.created_at, sr.invoking_run_id
        FROM sandbox_runs sr WHERE sr.backend = 'modal';
    """)
    conn.commit()
```

### 10.3 Key Dataclasses

```python
from __future__ import annotations
import dataclasses
from typing import Optional


# GPU tier → Modal GPU string, rate per second (USD), vRAM GB
GPU_TIERS: dict[str, tuple[str, float, int]] = {
    "t4":   ("T4",   0.000059, 16),
    "a10g": ("A10G", 0.000150, 24),
    "a100": ("A100", 0.000214, 40),
    "h100": ("H100", 0.000305, 80),
}


@dataclasses.dataclass
class ModalSandboxConfig:
    """Complete configuration for a single Modal GPU sandbox run."""
    # Execution
    command: list[str]                    # final argv to execute inside sandbox
    image: str = "python:3.12-slim"       # Docker image for the sandbox
    gpu_type: Optional[str] = None        # None = CPU; one of GPU_TIERS keys
    timeout: int = 300                    # seconds; enforced by Modal

    # Injection
    env_vars: dict[str, str] = dataclasses.field(default_factory=dict)
    # List of (host_path, sandbox_path, read_only) tuples
    mounts: list[tuple[str, str, bool]] = dataclasses.field(default_factory=list)

    # File upload
    upload_file: Optional[str] = None     # local path → /workspace/<name>

    # Attribution
    invoking_run_id: Optional[str] = None


@dataclasses.dataclass
class ModalSandboxResult:
    """Structured result from a completed Modal sandbox run."""
    exit_code: int
    stdout: str
    stderr: str
    duration_s: float
    modal_function_id: Optional[str]
    cost_usd: float
    truncated: bool = False               # True if output exceeded 1 MB cap
```

### 10.4 Core Algorithm — `_run_modal()`

```python
def _run_modal(cfg: ModalSandboxConfig) -> ModalSandboxResult:
    """Execute cfg inside a Modal Sandbox. Returns structured result."""
    try:
        import modal
    except ImportError as exc:
        raise RuntimeError(
            "modal SDK not installed — run: pip install modal"
        ) from exc

    import time

    # 1. Build Modal Image
    image = modal.Image.from_registry(cfg.image, add_python="3.12")
    if cfg.gpu_type and cfg.gpu_type.lower() == "t4":
        # T4 images may need CUDA base for PyTorch GPU support
        image = modal.Image.from_registry("pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime")

    # 2. Build mounts list
    modal_mounts = []
    for host_path, sandbox_path, read_only in cfg.mounts:
        _validate_mount_path(host_path)  # raises ValueError on blocked pattern
        modal_mounts.append(
            modal.Mount.from_local_dir(host_path, remote_path=sandbox_path)
        )

    # 3. Resolve GPU string
    gpu_arg = None
    if cfg.gpu_type:
        modal_gpu_str, _, _ = GPU_TIERS[cfg.gpu_type.lower()]
        gpu_arg = modal_gpu_str

    # 4. Upload script file if provided
    upload_mount = None
    if cfg.upload_file:
        from pathlib import Path as _Path
        local_path = _Path(cfg.upload_file)
        upload_mount = modal.Mount.from_local_file(
            local_path, remote_path=f"/workspace/{local_path.name}"
        )
        modal_mounts.append(upload_mount)

    # 5. Create app and sandbox
    app = modal.App.lookup("tag-sandbox", create_if_missing=True)

    t_start = time.monotonic()
    try:
        sb = modal.Sandbox.create(
            *cfg.command,
            app=app,
            image=image,
            gpu=gpu_arg,
            timeout=cfg.timeout,
            mounts=modal_mounts,
            environment_variables=cfg.env_vars,
        )
        function_id = getattr(sb, "object_id", None) or getattr(sb, "sandbox_id", None)
        sb.wait()
        exit_code = sb.returncode
        stdout = sb.stdout.read()[:1_000_000]       # 1 MB cap
        stderr = sb.stderr.read()[:262_144]          # 256 KB cap
        truncated = len(sb.stdout.read()) == 1_000_000
    except modal.exception.AuthError as exc:
        raise RuntimeError(
            "Modal authentication failed — run: modal token new"
        ) from exc

    duration_s = time.monotonic() - t_start

    # 6. Cost calculation
    rate = GPU_TIERS[cfg.gpu_type.lower()][1] if cfg.gpu_type else 0.0000020
    cost_usd = round(duration_s * rate, 6)

    return ModalSandboxResult(
        exit_code=exit_code,
        stdout=stdout,
        stderr=stderr,
        duration_s=duration_s,
        modal_function_id=str(function_id) if function_id else None,
        cost_usd=cost_usd,
        truncated=truncated,
    )
```

### 10.5 `run_in_sandbox()` Dispatch Extension

```python
# Inside run_in_sandbox(), after the existing docker dispatch branch:

elif backend == "modal":
    from dataclasses import asdict  # noqa: PLC0415
    modal_cfg = ModalSandboxConfig(
        command=cmd,
        image=image,
        gpu_type=kwargs.get("gpu_type"),
        timeout=timeout,
        env_vars=kwargs.get("env_vars", {}),
        mounts=kwargs.get("mounts", []),
        upload_file=kwargs.get("upload_file"),
        invoking_run_id=kwargs.get("invoking_run_id"),
    )
    result = _run_modal(modal_cfg)
    exit_code  = result.exit_code
    stdout     = result.stdout
    stderr     = result.stderr
    cost_usd   = result.cost_usd
    fn_id      = result.modal_function_id
    duration_s = result.duration_s
```

### 10.6 Mount Path Validation

```python
import re as _re

_BLOCKED_MOUNT_PATTERNS = [
    _re.compile(r".*\.env$", _re.IGNORECASE),
    _re.compile(r".*\.key$", _re.IGNORECASE),
    _re.compile(r".*\.pem$", _re.IGNORECASE),
    _re.compile(r".*secret.*", _re.IGNORECASE),
    _re.compile(r".*credential.*", _re.IGNORECASE),
    _re.compile(r".*/\.ssh(/.*)?$"),
    _re.compile(r".*/\.aws(/.*)?$"),
    _re.compile(r".*/\.config/op(/.*)?$"),
    _re.compile(r".*\.p12$", _re.IGNORECASE),
    _re.compile(r".*\.pfx$", _re.IGNORECASE),
    _re.compile(r".*id_rsa.*"),
    _re.compile(r".*id_ed25519.*"),
    _re.compile(r".*\.token$", _re.IGNORECASE),
    _re.compile(r".*vault.*", _re.IGNORECASE),
    _re.compile(r".*/\.gnupg(/.*)?$"),
]


def _validate_mount_path(host_path: str) -> None:
    """Raise ValueError if host_path matches any blocked credential pattern."""
    from pathlib import Path as _P
    expanded = str(_P(host_path).expanduser().resolve())
    for pattern in _BLOCKED_MOUNT_PATTERNS:
        if pattern.match(expanded):
            raise ValueError(
                f"Refusing to mount credential path: {host_path!r}\n"
                f"Matched blocked pattern: {pattern.pattern!r}"
            )
```

### 10.7 Cost Estimation — Pre-Run Display

```python
def _estimate_modal_cost(gpu_type: str | None, timeout_s: int) -> float:
    """Return worst-case cost estimate in USD (ceiling at timeout_s)."""
    if gpu_type and gpu_type.lower() in GPU_TIERS:
        rate = GPU_TIERS[gpu_type.lower()][1]
    else:
        rate = 0.0000020  # CPU-only Modal rate
    return round(rate * timeout_s, 4)


def _confirm_gpu_run(gpu_type: str | None, timeout_s: int) -> bool:
    """Print cost estimate and prompt for confirmation. Returns True if confirmed."""
    import os
    cost = _estimate_modal_cost(gpu_type, timeout_s)
    tier_str = gpu_type.upper() if gpu_type else "CPU"
    print(
        f"Estimated cost: {tier_str} GPU for ~{timeout_s}s ceiling "
        f"→ ${cost:.4f} USD"
    )
    if os.environ.get("CI"):
        return True
    answer = input("Proceed? [y/N]: ").strip().lower()
    return answer in ("y", "yes")
```

### 10.8 OTel Integration (PRD-013)

```python
def _emit_sandbox_span(result: ModalSandboxResult, cfg: ModalSandboxConfig) -> None:
    """Emit an OTel span for the Modal sandbox run. No-op if tracing not configured."""
    try:
        from tag.tracing import get_tracer  # type: ignore[import]
        from tag.otel_semconv import SandboxAttributes  # type: ignore[import]
    except ImportError:
        return
    tracer = get_tracer("tag.sandbox")
    with tracer.start_as_current_span("sandbox.modal.run") as span:
        span.set_attribute(SandboxAttributes.BACKEND, "modal")
        span.set_attribute(SandboxAttributes.GPU_TYPE, cfg.gpu_type or "cpu")
        span.set_attribute(SandboxAttributes.COST_USD, result.cost_usd)
        span.set_attribute(SandboxAttributes.EXIT_CODE, result.exit_code)
        span.set_attribute(SandboxAttributes.DURATION_S, result.duration_s)
        if result.modal_function_id:
            span.set_attribute(SandboxAttributes.MODAL_FUNCTION_ID, result.modal_function_id)
        if cfg.invoking_run_id:
            span.set_attribute(SandboxAttributes.INVOKING_RUN_ID, cfg.invoking_run_id)
```

### 10.9 `tag sandbox cost` Implementation

```python
def list_sandbox_cost(
    conn: sqlite3.Connection,
    *,
    since: str | None = None,
    until: str | None = None,
    group_by: str = "gpu_type",
) -> list[dict]:
    """Query sandbox_runs for Modal GPU cost aggregation."""
    ensure_schema(conn)
    valid_groups = {"gpu_type", "date", "invoking_run_id"}
    if group_by not in valid_groups:
        raise ValueError(f"group_by must be one of {valid_groups}")

    group_expr = {
        "gpu_type":        "COALESCE(gpu_type, 'cpu')",
        "date":            "date(created_at)",
        "invoking_run_id": "COALESCE(invoking_run_id, 'direct')",
    }[group_by]

    params: list = []
    where_clauses = ["backend = 'modal'"]
    if since:
        where_clauses.append("created_at >= ?")
        params.append(since)
    if until:
        where_clauses.append("created_at <= ?")
        params.append(until + "T23:59:59Z")

    where = " AND ".join(where_clauses)
    sql = f"""
        SELECT
            {group_expr}                   AS group_key,
            COUNT(*)                        AS run_count,
            SUM(COALESCE(duration_s, 0))   AS total_duration_s,
            SUM(COALESCE(modal_cost_usd,0)) AS total_cost_usd
        FROM sandbox_runs
        WHERE {where}
        GROUP BY {group_expr}
        ORDER BY total_cost_usd DESC
    """
    rows = conn.execute(sql, params).fetchall()
    return [
        {
            "group_key":       r[0],
            "run_count":       r[1],
            "total_duration_s": r[2],
            "total_cost_usd":  r[3],
        }
        for r in rows
    ]
```

### 10.10 `controller.py` Integration Points

The following changes are required in `controller.py`:

1. **`cmd_sandbox_run` argument parser** — add `--gpu`, `--env` (action=`append`), `--volume` (action=`append`), `--no-cost-estimate`, `--yes` to the `sandbox run` subparser.

2. **Pre-dispatch cost prompt** — before calling `run_in_sandbox()`, when `backend == "modal"`:

```python
if backend == "modal" and not args.no_cost_estimate:
    if not args.yes and not _confirm_gpu_run(args.gpu, args.timeout):
        print("Aborted.")
        return
```

3. **`cmd_sandbox_cost` subcommand** — new function wired to `tag sandbox cost`:

```python
def cmd_sandbox_cost(args: argparse.Namespace) -> None:
    from tag.sandbox import list_sandbox_cost, open_db_sandbox
    conn = open_db_sandbox()
    rows = list_sandbox_cost(
        conn,
        since=args.since,
        until=args.until,
        group_by=args.group_by,
    )
    if args.json:
        print(json.dumps(rows, indent=2))
        return
    # Formatted table output (reuse existing table-printing pattern from controller.py)
    _print_cost_table(rows)
```

4. **`cmd_doctor` extension** — add `modal_gpu` check:

```python
def _check_modal_gpu() -> tuple[str, str, str]:
    """Returns (name, status, detail) for modal GPU doctor check."""
    try:
        import modal
        version = getattr(modal, "__version__", "unknown")
        token_path = Path.home() / ".modal" / "token_id"
        if not token_path.exists():
            return "modal_token", "warn", "no token at ~/.modal/token_id — run: modal token new"
        return "modal_gpu", "ok", f"modal {version} ready"
    except ImportError:
        return "modal_sdk", "error", "modal not installed — pip install modal"
```

---

## 11. Security Considerations

1. **Credential path blocking** — all 15 blocked patterns from `_BLOCKED_MOUNT_PATTERNS` are enforced before any Modal API call. The check uses `Path.expanduser().resolve()` to canonicalize symlinks and `~` expansion before pattern matching, preventing bypass via relative paths or symlink chains.

2. **Environment variable secrecy** — `--env` values are stored in `ModalSandboxConfig.env_vars` dict and passed directly to `modal.Sandbox.create(environment_variables=...)`. They are never written to the `command` column of `sandbox_runs`, never logged via Python `logging`, and never set as OTel span attributes. The `sandbox_runs.command` column stores only the user-supplied `--code` string or `--file` path, not the env vars.

3. **Modal credential isolation** — TAG uses the user's existing `~/.modal/token_id` credential established by `modal token new`. TAG never reads, copies, stores, or transmits the Modal token. If the token is absent, TAG raises a clear error and directs the user to re-authenticate directly with Modal CLI.

4. **Output truncation** — stdout is capped at 1 MB and stderr at 256 KB before being written to SQLite. This prevents runaway sandbox output from filling the local database. A `truncated: true` field is set in the result and surfaced in `--json` output.

5. **Timeout enforcement at Modal** — the `timeout` parameter is passed to `modal.Sandbox.create(timeout=...)`, not just enforced locally via `subprocess.run(timeout=...)`. This ensures the GPU is released even if the TAG client process is killed or the network connection drops.

6. **PRD-034 secret scanning** — the `--code` inline string and uploaded `--file` content are NOT scanned for secrets (that would require executing PRD-034's scanner, which is out of scope here), but the volume-path blocker ensures credential files cannot be read from disk. Users are warned in the CLI help text that `--code` content is sent to Modal's cloud and should not contain plaintext secrets.

7. **Modal app isolation** — all TAG sandbox runs use a dedicated Modal app named `tag-sandbox` (created if missing). This isolates TAG's sandbox traffic from any other Modal apps the user may have, simplifying billing attribution and access control in the Modal dashboard.

8. **No persistent Modal sandbox sessions** — each `tag sandbox run` creates a fresh ephemeral Modal Sandbox that is terminated on completion. No sandbox is reused across runs, eliminating any risk of cross-run data leakage.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_sandbox_modal.py`)

All Modal SDK calls are mocked via `pytest-mock` to avoid live network calls in CI.

```python
import pytest
from unittest.mock import MagicMock, patch

@pytest.fixture
def mock_modal(monkeypatch):
    """Inject a mock modal module into sandbox._run_modal's import."""
    mock_mod = MagicMock()
    # Simulate Sandbox.create() returning a mock sandbox
    mock_sb = MagicMock()
    mock_sb.returncode = 0
    mock_sb.stdout.read.return_value = "True NVIDIA A10G\n"
    mock_sb.stderr.read.return_value = ""
    mock_mod.Sandbox.create.return_value = mock_sb
    mock_mod.Image.from_registry.return_value = MagicMock()
    mock_mod.Mount.from_local_dir.return_value = MagicMock()
    mock_mod.App.lookup.return_value = MagicMock()
    monkeypatch.setitem(sys.modules, "modal", mock_mod)
    return mock_mod


def test_run_modal_dispatches_not_restricted(mock_modal, tmp_path):
    """_run_modal() must call modal.Sandbox.create, never subprocess.run."""
    from tag.sandbox import ModalSandboxConfig, _run_modal
    cfg = ModalSandboxConfig(command=["python3", "-c", "print('hi')"], gpu_type="a10g")
    with patch("subprocess.run") as mock_sub:
        result = _run_modal(cfg)
    mock_sub.assert_not_called()
    mock_modal.Sandbox.create.assert_called_once()
    assert result.exit_code == 0


@pytest.mark.parametrize("tier,expected_modal_str", [
    ("t4",   "T4"),
    ("a10g", "A10G"),
    ("a100", "A100"),
    ("h100", "H100"),
])
def test_gpu_tier_mapping(mock_modal, tier, expected_modal_str):
    from tag.sandbox import ModalSandboxConfig, _run_modal
    cfg = ModalSandboxConfig(command=["python3", "-c", "pass"], gpu_type=tier)
    _run_modal(cfg)
    call_kwargs = mock_modal.Sandbox.create.call_args.kwargs
    assert call_kwargs["gpu"] == expected_modal_str


def test_blocked_mount_path_raises():
    from tag.sandbox import _validate_mount_path
    with pytest.raises(ValueError, match="credential path"):
        _validate_mount_path("~/.ssh/id_rsa")


@pytest.mark.parametrize("blocked", [
    "~/.aws/credentials", "~/.env", "/secrets/api.key",
    "/home/user/.config/op/config", "/data/vault.json",
    "/app/secret_token", "./credentials.pem",
])
def test_all_blocked_patterns_rejected(blocked):
    from tag.sandbox import _validate_mount_path
    with pytest.raises(ValueError):
        _validate_mount_path(blocked)


def test_env_vars_not_in_command_column(mock_modal, tmp_db):
    import sqlite3
    from tag.sandbox import run_in_sandbox, ensure_schema
    conn = sqlite3.connect(tmp_db)
    ensure_schema(conn)
    run_in_sandbox(
        conn, "python3 -c 'pass'",
        backend="modal", gpu_type="t4",
        env_vars={"SECRET_KEY": "super-secret"},
    )
    row = conn.execute("SELECT command FROM sandbox_runs LIMIT 1").fetchone()
    assert "super-secret" not in (row[0] or "")


def test_cost_computed_from_duration(mock_modal, tmp_db):
    import sqlite3
    from tag.sandbox import run_in_sandbox, ensure_schema
    conn = sqlite3.connect(tmp_db)
    ensure_schema(conn)
    run_in_sandbox(conn, "python3 -c 'pass'", backend="modal", gpu_type="a100")
    row = conn.execute(
        "SELECT modal_cost_usd, duration_s FROM sandbox_runs LIMIT 1"
    ).fetchone()
    assert row[0] is not None and row[0] >= 0
    assert row[1] is not None and row[1] >= 0


def test_modal_not_installed_raises_runtime_error(monkeypatch):
    monkeypatch.setitem(sys.modules, "modal", None)  # simulate ImportError
    from tag.sandbox import ModalSandboxConfig, _run_modal
    cfg = ModalSandboxConfig(command=["python3", "-c", "pass"])
    with pytest.raises(RuntimeError, match="pip install modal"):
        _run_modal(cfg)


def test_file_and_code_mutually_exclusive(cli_runner):
    result = cli_runner.invoke(["sandbox", "run", "--backend", "modal",
                                "--code", "pass", "--file", "train.py"])
    assert result.exit_code == 1
    assert "mutually exclusive" in result.output


def test_no_code_no_file_errors(cli_runner):
    result = cli_runner.invoke(["sandbox", "run", "--backend", "modal"])
    assert result.exit_code == 1
    assert "requires --code or --file" in result.output
```

### 12.2 Integration Tests

Integration tests run only when `MODAL_TOKEN_ID` and `MODAL_TOKEN_SECRET` are set in the environment (skipped in default CI):

```python
@pytest.mark.integration
@pytest.mark.skipif(not os.environ.get("MODAL_TOKEN_ID"), reason="Modal creds not set")
def test_modal_t4_cuda_availability():
    """Live T4 test: verify CUDA is available in the sandbox."""
    from tag.sandbox import ModalSandboxConfig, _run_modal
    cfg = ModalSandboxConfig(
        command=["python3", "-c",
                 "import torch; print(torch.cuda.is_available())"],
        gpu_type="t4",
        image="pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime",
        timeout=120,
    )
    result = _run_modal(cfg)
    assert result.exit_code == 0
    assert "True" in result.stdout


@pytest.mark.integration
@pytest.mark.skipif(not os.environ.get("MODAL_TOKEN_ID"), reason="Modal creds not set")
@pytest.mark.parametrize("tier", ["t4", "a10g"])
def test_modal_gpu_tiers_execute(tier):
    from tag.sandbox import ModalSandboxConfig, _run_modal
    cfg = ModalSandboxConfig(
        command=["python3", "-c", "print('ok')"],
        gpu_type=tier,
        timeout=90,
    )
    result = _run_modal(cfg)
    assert result.exit_code == 0
    assert "ok" in result.stdout
    assert result.cost_usd > 0
```

### 12.3 Performance Tests

```python
def test_sandbox_cost_query_large_table(tmp_db):
    """list_sandbox_cost must complete in <200ms for 100k rows."""
    import sqlite3, time, random
    from tag.sandbox import ensure_schema, list_sandbox_cost
    conn = sqlite3.connect(tmp_db)
    ensure_schema(conn)
    rows = [
        (f"id{i}", "python3 -c 'pass'", "modal", None, "done", 0, "", None,
         f"2026-06-{(i%28)+1:02d}T10:00:00Z", f"2026-06-{(i%28)+1:02d}T10:01:00Z",
         random.choice(["t4", "a10g", "a100", "h100"]),
         f"fn-{i}", round(random.uniform(0.001, 1.0), 6),
         random.uniform(10, 3600), None)
        for i in range(100_000)
    ]
    conn.executemany(
        """INSERT INTO sandbox_runs VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
        rows
    )
    conn.commit()
    t0 = time.monotonic()
    result = list_sandbox_cost(conn, group_by="gpu_type")
    elapsed = time.monotonic() - t0
    assert elapsed < 0.200
    assert len(result) == 4  # one row per GPU tier
```

---

## 13. Acceptance Criteria

| ID | Criterion | Test Reference |
|----|-----------|----------------|
| AC-01 | `tag sandbox run --backend modal --gpu a10g --code "print('hi')" --yes` exits 0 and prints `hi` in the output section. | `test_modal_a10g_inline` (integration) |
| AC-02 | `tag sandbox run --backend modal --gpu h100 --file train.py --yes` creates a `sandbox_runs` row with `backend='modal'`, `gpu_type='h100'`, non-NULL `modal_cost_usd`. | `test_modal_h100_file` (integration) |
| AC-03 | When Modal SDK is not installed, `tag sandbox run --backend modal --gpu t4 --code "x"` prints `modal SDK not installed — run: pip install modal` and exits 1. | `test_modal_not_installed_raises_runtime_error` |
| AC-04 | When Modal token is absent, `tag sandbox run --backend modal --gpu t4 --code "x"` prints a message directing user to `modal token new` and exits 1. | `test_modal_auth_error` |
| AC-05 | `--volume ~/.ssh:/host-ssh` is rejected before any Modal call with exit code 1 and message `Refusing to mount credential path`. | `test_blocked_mount_path_raises` |
| AC-06 | `--env SECRET=abc` passed values do NOT appear in the `command` column of `sandbox_runs`. | `test_env_vars_not_in_command_column` |
| AC-07 | `tag sandbox cost --group-by gpu_type` returns a table with columns `GPU`, `Runs`, `Total Duration`, `Total Cost`. | `test_sandbox_cost_output_format` |
| AC-08 | `tag sandbox cost` completes in <200ms on a 100,000-row `sandbox_runs` table. | `test_sandbox_cost_query_large_table` |
| AC-09 | `modal_cost_usd` in `sandbox_runs` equals `duration_s * GPU_TIERS[gpu_type][1]` to within ±1% for all four GPU tiers. | `test_cost_accuracy_per_tier` |
| AC-10 | `tag sandbox run --backend modal --code "x" --file y.py` exits 1 with `--file and --code are mutually exclusive`. | `test_file_and_code_mutually_exclusive` |
| AC-11 | `tag sandbox run --backend modal` (no `--code` or `--file`) exits 1 with `modal backend requires --code or --file`. | `test_no_code_no_file_errors` |
| AC-12 | `tag doctor` includes a `modal_gpu` check that exits green when Modal is installed and token is present. | `test_doctor_modal_gpu_check` |
| AC-13 | `tag sandbox run --backend modal --gpu t4 --code "print('x')" --yes --json` emits valid JSON with fields `id`, `exit_code`, `output`, `gpu_type`, `modal_cost_usd`. | `test_json_output_fields` |
| AC-14 | Without `--yes` and with `CI` unset, a confirmation prompt is printed before any Modal call; answering `N` aborts without making any Modal API call. | `test_cost_prompt_abort_no_modal_call` |
| AC-15 | OTel span `sandbox.modal.run` is emitted with attributes `sandbox.gpu_type` and `sandbox.cost_usd` when tracing is configured. | `test_otel_span_emitted` |
| AC-16 | All four GPU tiers (`t4`, `a10g`, `a100`, `h100`) are accepted without error; an invalid tier (e.g., `a9000`) exits 1 with a message listing valid tiers. | `test_gpu_tier_mapping`, `test_invalid_gpu_tier` |
| AC-17 | `sandbox_runs` table after a Modal run has non-NULL values for `gpu_type`, `modal_function_id`, `duration_s`, and `modal_cost_usd`. | `test_cost_computed_from_duration` |

---

## 14. Dependencies

| Dependency | Version | Type | Notes |
|------------|---------|------|-------|
| `modal` | ≥0.60.0 | Optional Python package | `pip install modal`; lazy-imported inside `_run_modal()` |
| PRD-028 | Implemented | Internal | `sandbox_runs` table schema and `run_in_sandbox()` function signature |
| PRD-013 | Optional | Internal | OTel tracing; graceful no-op if `tag.tracing` not importable |
| PRD-012 | Optional | Internal | `tag budget` aggregation; no code dependency, only data convention |
| PRD-034 | None (awareness) | Internal | Blocked mount patterns inherited from PRD-034 security patterns |
| PRD-005 | Awareness | Internal | Execution backend selection context; no code dependency |
| `pytest-mock` | ≥3.11 | Test | Mock `modal` module in unit tests |
| `sqlite3` | stdlib | Runtime | Already used throughout TAG; no new dependency |
| `pathlib` | stdlib | Runtime | `Path.expanduser().resolve()` for mount validation |

---

## 15. Open Questions

| ID | Question | Owner | Resolution Target |
|----|----------|-------|-------------------|
| OQ-01 | Does `modal.Sandbox.create()` support passing GPU type as a string in Modal SDK ≥0.67? The Sandbox API differs from the `@app.function(gpu=...)` decorator API. Needs verification against Modal changelog. | Engineering | Sprint 1, Day 1 |
| OQ-02 | Should `modal_cost_usd` use the static `GPU_TIERS` rate table or call Modal's billing API (`modal.client.get_usage()`) for post-run actuals? Billing API may introduce latency. | Product | Sprint 1, Day 3 |
| OQ-03 | What is the correct Modal image for T4 GPU runs requiring CUDA? `pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime` is the current heuristic but may lag new PyTorch releases. Should TAG hard-code or expose `--image`? | Engineering | Sprint 1, Day 2 |
| OQ-04 | Should `--volume` use `modal.Mount.from_local_dir()` (copies files at launch time) or `modal.CloudBucketMount` (S3-backed)? The former is simpler but limited to ~500 MB; the latter requires Modal storage setup. | Engineering | Sprint 1, Day 3 |
| OQ-05 | Should `tag sandbox run --backend modal` (CPU, no GPU) be a supported use case? PRD-028 envisioned Modal for GPU but CPU Modal sandboxes are valid. The `--gpu` flag would be optional. | Product | Sprint 1, Day 1 |
| OQ-06 | Should `invoking_run_id` be auto-populated from the current TAG session context (e.g., from an env var `TAG_RUN_ID` set by `cmd_submit`)? This would enable zero-friction attribution. | Engineering | Sprint 2, Day 1 |
| OQ-07 | How should `tag sandbox run --backend modal --gpu a100 --file train.py` handle large uploads (e.g., a 200 MB checkpoint file passed as `--file`)? Modal file upload has practical limits; should there be a size guard? | Engineering | Sprint 2, Day 2 |
| OQ-08 | Should `tag sandbox cost` roll up into `tag budget status`? This would require `budget.py` to query `sandbox_runs` in addition to `runs`. Scope of budget integration is TBD. | Product | Sprint 2, Day 3 |
| OQ-09 | Live stdout streaming from Modal Sandbox (via `sb.stdout` as an async iterator) — is this achievable in v1 given the complexity of async/sync bridge in `sandbox.py`? Or should streaming be a hard non-goal until a follow-on PRD? | Engineering | Sprint 1, Day 4 |

---

## 16. Complexity and Timeline

**Estimated Effort:** M (1-2 weeks, 1 engineer)

### Phase 1 — Core Modal Backend (Days 1-4)

| Day | Task |
|-----|------|
| 1 | Audit Modal SDK `Sandbox` API (OQ-01, OQ-03, OQ-04). Confirm `modal.Sandbox.create(gpu=...)` signature. Write `GPU_TIERS` rate table and `ModalSandboxConfig`/`ModalSandboxResult` dataclasses. |
| 2 | Implement `_run_modal()` with GPU string mapping, environment variable injection, timeout, output capture. Write `_validate_mount_path()` with all 15 blocked patterns. |
| 3 | Extend `ensure_schema()` with `_migrate_modal_columns()`. Extend `run_in_sandbox()` dispatch to call `_run_modal()`. Implement `_estimate_modal_cost()` and `_confirm_gpu_run()`. |
| 4 | Add `list_sandbox_cost()` query function. Write all unit tests in `tests/test_sandbox_modal.py`. Achieve ≥90% coverage with mocked `modal`. |

### Phase 2 — CLI Surface and Integration (Days 5-8)

| Day | Task |
|-----|------|
| 5 | Add `--gpu`, `--env`, `--volume`, `--no-cost-estimate`, `--yes` flags to `cmd_sandbox_run` in `controller.py`. Add `cmd_sandbox_cost` subcommand. Wire `--json` output path. |
| 6 | Add `modal_gpu` check to `cmd_doctor`. Add `--file` upload path in `_run_modal()`. Handle `--file`/`--code` mutual exclusion. |
| 7 | Add OTel span emission via `_emit_sandbox_span()`. Add `sandbox_gpu_runs` view to `ensure_schema()`. Add `invoking_run_id` threading from `cmd_submit` context. |
| 8 | Write integration test scaffold (`@pytest.mark.integration`) with `MODAL_TOKEN_ID` skip guard. Write performance test `test_sandbox_cost_query_large_table`. Manual smoke test on T4 and A10G. |

### Phase 3 — Polish and Review (Days 9-10)

| Day | Task |
|-----|------|
| 9 | Address open questions OQ-02 (billing API vs static table), OQ-08 (budget rollup). Update `pyproject.toml` optional deps. Write changelog entry. |
| 10 | Code review, fix review feedback, run full test suite. Update `docs/prd/INDEX.md` with PRD-093. Close GitHub issue #348. |

---

*End of PRD-093*
