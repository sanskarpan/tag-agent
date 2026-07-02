# PRD-090: Sandbox Template/Snapshot System for <200ms Cold Start (`tag sandbox template`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py + sandbox_templates SQLite`
**Depends on:** PRD-028 (Sandbox Code Execution — base sandbox runtime), PRD-013 (Agent Tracing — span instrumentation for template ops), PRD-034 (Secret Scanning — credential pattern matching for mounts), PRD-005 (Execution Backend Selection — provider abstraction layer), PRD-012 (Cost Tracking — template build cost attribution)
**GitHub Issue:** #348
**Inspired by:** E2B templates, Daytona prebuild, Fly.io machines

---

## 1. Overview

TAG's sandbox system (PRD-028) isolates agent-generated code in Docker containers or E2B microVMs, providing strong security boundaries. However, every `tag sandbox run` invocation currently starts from a base image and installs dependencies at runtime. For a Python data science workload that requires `numpy`, `pandas`, `scikit-learn`, `matplotlib`, and `torch`, this means 60–120 seconds of `pip install` time before the first line of user code executes. For agent-driven iterative workflows — where the same environment is recreated dozens of times per session — this cold-start penalty is catastrophic for developer experience and unacceptable for real-time interactive use cases.

The Sandbox Template/Snapshot System introduces the concept of a **named, pre-baked sandbox environment** — a template — that captures a fully-provisioned sandbox state (filesystem, installed packages, environment variables, working directory) and makes it available for instantaneous cloning. Creating a template is a one-time `tag sandbox template create` operation that runs a base image, installs the specified packages, and persists the resulting state to a SQLite-backed catalog. Subsequent `tag sandbox run --template <name>` invocations restore from that snapshot, bypassing the install phase entirely. The target is a **<200ms wall-clock allocation time** from `run` invocation to code execution start for Docker-backend templates, matching E2B's Firecracker snapshot restore performance on the cloud backend.

The design is directly informed by three production systems: **E2B's Firecracker snapshot API** (PATCH /vm {state:Paused} → PUT /snapshot/create → PATCH /vm {state:Resumed}) achieves ~150ms resume latency; **Daytona's prebuild system** builds workspace images from repository configuration and makes them available as instant-clone sources; and **Fly.io Machines** maintains a warm pool of pre-initialized VMs that can be assigned to a request in under 200ms. For TAG's Docker backend, the analogous technique is `docker commit` to persist a container's filesystem state as a local image, then `docker run` from that committed image — skipping all install steps. For the E2B backend, TAG wraps E2B's native template build API (`e2b template build`) and `Sandbox.create(template=template_id)` to achieve microVM-level isolation with snapshot-speed startup.

TAG templates also solve a correctness problem beyond performance. When an agent iterates on a data analysis script across multiple sandbox runs, each run reinstalling the same packages introduces non-determinism: a `pip install numpy` today may resolve to a different patch version than yesterday, silently changing numeric results. Templates pin the exact environment as a snapshot artifact, ensuring bit-for-bit reproducibility across all runs derived from a given template. This property is essential for eval suites (PRD-027) where sandbox reproducibility directly affects score stability.

The feature is additive: all existing `tag sandbox run` invocations without `--template` continue to work identically. Templates are managed through a dedicated `tag sandbox template` subcommand group. The SQLite catalog stores template metadata including the base image, install manifest, backend-specific artifact reference, creation timestamp, and per-template usage statistics. A `tag sandbox template list` command surfaces this catalog for human and machine consumers. Templates may be deleted, but deletion is guarded by a confirmation prompt when the template has been used in the last 7 days.

---

## 2. Problem Statement

### 2.1 Cold-Start Latency Makes Iterative Agent Workflows Unusable

When a TAG eval suite runs 50 test cases, each requiring a Python data science environment, the total package-install overhead is 50 × 90 seconds = 75 minutes of pure setup time — most of which is repeated redundant work. The same `pip install numpy pandas scikit-learn` runs 50 times across 50 containers. A developer running `tag eval run --suite evals/datascience.yaml` waits over an hour before seeing a single result. This is not a theoretical edge case: any evaluation suite, kanban swarm dispatch, or queue batch that reuses the same environment across tasks exhibits this pathology.

Interactive use is equally impacted. A developer iterating on a data analysis script — fixing a bug, adjusting a plot, re-running with new parameters — faces a 90-second sandbox cold start between each attempt. At this latency, the tight feedback loop that makes interactive development productive is completely broken. The developer abandons the sandbox and runs code directly on the host, defeating the security isolation that PRD-028 was built to provide.

### 2.2 Non-Deterministic Environments Break Eval Reproducibility

TAG's eval framework (PRD-027) scores agent outputs against expected behaviors. When the sandbox environment varies between eval runs — because `pip install` resolves different package versions across days or weeks — the same agent code may produce numerically different outputs, causing false-positive regressions or masking real ones. There is currently no mechanism to pin a sandbox to the exact package set used in a reference run. The result is that eval scores carry unexplained variance that cannot be attributed to agent behavior changes.

### 2.3 Resource Waste from Redundant Package Installation

Each `pip install numpy` in a fresh container downloads ~16 MB from PyPI, extracts, compiles, and links the package. Across a team of 5 developers each running 20 sandbox-backed tasks per day, this represents 100 redundant installs per day of the same packages — approximately 1.6 GB of redundant downloads, 100 × 30-second compile windows, and 5 GB of ephemeral layer storage that is created and immediately discarded. Disk I/O and network bandwidth are finite resources; this waste competes with productive work on developer machines. Templates amortize the install cost to a single one-time build, after which all subsequent runs are pure snapshot restores.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | `tag sandbox template create` builds a named template from a base image and a package install manifest, persisting the result as a tagged Docker image (Docker backend) or E2B template (E2B backend) with full metadata in SQLite. |
| G2 | `tag sandbox run --template <name>` starts a sandbox from the template snapshot with **<200ms wall-clock latency** from CLI invocation to code execution on the Docker backend (local warm image), and <500ms on the E2B backend (network round-trip). |
| G3 | Templates are reproducible: every run from the same template ID executes against the same exact filesystem state, including package versions pinned at template build time. |
| G4 | `tag sandbox template list` displays all templates with their ID, name, base image, install manifest, backend, artifact reference, size on disk, creation date, and run count. |
| G5 | `tag sandbox template delete <id>` removes the template metadata from SQLite and the backing artifact (Docker image or E2B template), with a confirmation guard when the template has recent use. |
| G6 | Templates are tagged with arbitrary key=value metadata (`--tag`) and filterable by tag in `list` and `run` commands. |
| G7 | Template build progress streams to the terminal with elapsed time per step (base pull, dependency install, commit, index). |
| G8 | `tag sandbox template inspect <id>` shows the full install manifest, environment variables, working directory, and all runs that used this template (cross-referenced from `sandbox_runs`). |
| G9 | The Docker backend uses `docker commit` to snapshot, and `docker run --rm <committed_image>` to restore — no external snapshot daemon or Firecracker dependency. |
| G10 | The E2B backend delegates to `e2b template build` and `Sandbox.create(template=template_id)`, inheriting E2B's Firecracker-backed ~150ms resume. |
| G11 | Template creation cost is attributed to TAG's budget tracking (PRD-012) using the build's token consumption and compute time. |
| G12 | All template lifecycle events (create, delete, run-from-template) emit OTEL spans (PRD-013) with `sandbox.template_id`, `sandbox.template_name`, and `sandbox.cold_start_ms` as span attributes. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | **Full VM memory snapshots (Firecracker PATCH /vm)**: TAG does not manage a Firecracker VMM directly. Memory snapshot/restore is delegated entirely to the E2B backend. The Docker backend uses filesystem-only `docker commit` snapshots. |
| NG2 | **Template distribution / registry push**: Templates are local to the machine or the E2B account. Pushing Docker images to a remote registry (ECR, Docker Hub) is not in scope; users can do this manually with standard Docker tooling. |
| NG3 | **Incremental/layered templates**: Templates do not compose (e.g., no `--from-template base-python --install extra-packages`). Each template is a standalone flat snapshot. Layering is a future extension. |
| NG4 | **GPU templates**: Templates backed by GPU-enabled images require NVIDIA runtime configuration outside TAG's scope. GPU sandbox runs are deferred to the Modal backend (PRD-028). |
| NG5 | **Warm pool management**: TAG does not pre-warm a pool of running containers derived from a template. Cold start is from a stopped snapshot, not from a running pre-warmed container. |
| NG6 | **Template versioning / history**: A template name maps to exactly one artifact at any given time. There is no version history or rollback. Deleting and recreating is the upgrade path. |
| NG7 | **Multi-backend template portability**: A template created on the Docker backend cannot be used on the E2B backend and vice versa. Backend affinity is set at creation time and stored in the catalog. |
| NG8 | **Windows container support**: Docker backend templates are Linux containers only, consistent with PRD-028's existing scope. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Cold start latency (container tier) | p50 <200ms, p95 <400ms from `sandbox run --template` to first byte of user code output | Integration test: 100 `sandbox run --template` invocations timed with `time.Now()`/`time.Since` around `exec.CommandContext` spawn to first stdout byte |
| Cold start latency (E2B backend) | p50 <500ms end-to-end (network included) | Same instrumentation against E2B API with `sandbox_cold_start_ms` span attribute |
| Template build reliability | 99% of `template create` invocations complete without error given a valid base image and install command | CI test matrix: 5 base images × 3 install manifests |
| Reproducibility | Given the same template, 100 consecutive `sandbox run --template` invocations produce identical `pip freeze` output | Automated test: compare sorted `pip freeze` output across 100 runs |
| Latency vs. baseline | Template-backed runs are ≥30× faster than cold-install runs for environments with ≥3 packages | Benchmark: `docker run python:3.11 pip install numpy pandas scikit-learn` vs `docker run <template_image> true` |
| SQLite catalog integrity | `sandbox_templates` table correctly reflects creation, update, and delete operations with no orphaned records | Unit tests with in-memory SQLite |
| OTEL instrumentation | Every template lifecycle event (create, run, delete) produces a span with correct `sandbox.template_id` attribute | Test via OTEL `tracetest.SpanRecorder` in-memory exporter |
| Zero regression in base sandbox | `tag sandbox run` without `--template` exhibits no latency or behavior change | Run PRD-028 integration tests before and after; assert no diff |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Data scientist using TAG | create a `python-datascience` template once with numpy, pandas, sklearn pre-installed | every subsequent analysis run starts in <200ms instead of waiting 90 seconds for pip installs |
| U2 | Platform engineer running eval suites | use `--template` in my eval sandbox config | 50 eval cases share the same pre-baked environment without redundant installs, cutting eval time from 75 min to under 5 min |
| U3 | Developer iterating on a script | run `tag sandbox run --template python-datascience --code "..."` and get output in <1 second | I can iterate tightly on code without the sandbox becoming the bottleneck |
| U4 | DevOps engineer | run `tag sandbox template list --json` | I can audit all templates in CI, check their sizes and last-used dates, and enforce a retention policy via script |
| U5 | Security-conscious operator | inspect a template with `tag sandbox template inspect <id>` | I can verify exactly which packages and versions are installed before allowing production use of that template |
| U6 | Developer with an E2B account | run `tag sandbox template create --backend e2b python-ml --base e2b/python:3.11` | I get Firecracker-backed microVM isolation with ~150ms startup instead of ~90-second Docker cold starts |
| U7 | Developer | delete stale templates with `tag sandbox template delete <id>` | I free disk space consumed by Docker images I no longer need, with a confirmation guard if the template was recently used |
| U8 | Team lead | tag templates with `--tag project=analysis --tag owner=team-a` | I can list only my team's templates with `tag sandbox template list --tag project=analysis` |
| U9 | Agent running in autonomous loop | use `--template` flag in programmatically constructed `tag sandbox run` invocations | each loop iteration reuses the pre-baked environment without incurring install overhead, enabling fast iterative agent workflows |
| U10 | Developer debugging a failure | run `tag sandbox template inspect <name>` to see which runs used this template | I can correlate a behavioral change in sandbox outputs with a template rebuild that changed package versions |

---

## 7. Proposed CLI Surface

All template subcommands live under the `tag sandbox template` namespace. The `--template` flag is added to the existing `tag sandbox run` command.

### 7.1 `tag sandbox template create`

Build and register a new sandbox template.

```
tag sandbox template create <name> \
  [--base <image>] \
  [--install <"pip install cmd" | path/to/requirements.txt>] \
  [--setup <"shell command run after install">] \
  [--workdir <path>] \
  [--env KEY=VALUE ...] \
  [--backend docker|e2b] \
  [--tag KEY=VALUE ...] \
  [--timeout <seconds>] \
  [--force] \
  [--json]
```

**Arguments:**
- `<name>`: Human-readable template name (slug: `[a-z0-9_-]{1,64}`). Must be unique; use `--force` to overwrite an existing template with the same name.
- `--base`: Base Docker image or E2B template ID. Defaults to `python:3.11-slim` for Docker backend, `e2b/python:3.11` for E2B backend.
- `--install`: Package install specification. Either a quoted pip install string (`"numpy pandas scikit-learn"`) or a path to a `requirements.txt` file. For non-pip install steps, use `--setup` instead.
- `--setup`: Arbitrary shell command executed after `--install` to further configure the environment (e.g., `"apt-get install -y git && git config --global user.email ci@example.com"`). Runs as root inside the build container.
- `--workdir`: Set the working directory inside the template (default: `/workspace`). Subsequent `sandbox run --template` invocations start in this directory.
- `--env KEY=VALUE`: Environment variables to bake into the template. Multiple allowed. These are set at container start time via `docker run --env`, not baked into the image layer.
- `--backend docker|e2b`: Sandbox backend to use. Default: `docker` if Docker daemon is detected, otherwise `e2b` if `E2B_API_KEY` is set.
- `--tag KEY=VALUE`: Arbitrary metadata tags attached to the template record. Multiple allowed. Used for filtering in `list`.
- `--timeout <seconds>`: Maximum time for the build step (default: 600). Build process is killed and cleaned up on timeout.
- `--force`: Overwrite an existing template with the same name. Deletes the old artifact before creating the new one.
- `--json`: Emit result as JSON instead of human-readable output.

**Example — create a Python data science template:**
```
$ tag sandbox template create python-datascience \
    --base python:3.11-slim \
    --install "numpy pandas scikit-learn matplotlib seaborn" \
    --tag project=analysis \
    --tag owner=alice

Building template 'python-datascience'...
  [1/4] Pulling base image python:3.11-slim          3.2s
  [2/4] Installing packages (numpy pandas ...)       47.3s
  [3/4] Committing snapshot                          0.8s
  [4/4] Registering in catalog                       0.1s

Template created:
  ID:        tmpl_4f8a2b1c
  Name:      python-datascience
  Backend:   docker
  Image:     tag-template:python-datascience-4f8a2b1c
  Base:      python:3.11-slim
  Packages:  numpy==1.26.4 pandas==2.2.1 scikit-learn==1.4.1 ...
  Size:      412 MB
  Tags:      project=analysis, owner=alice
  Created:   2026-06-17T09:14:22Z

Cold start from this template: ~180ms
```

**Example — create a Node.js template from requirements file:**
```
$ tag sandbox template create node-tools \
    --base node:20-slim \
    --install requirements.txt \
    --setup "npm install -g typescript ts-node prettier" \
    --workdir /app

Building template 'node-tools'...
  [1/4] Pulling base image node:20-slim              2.1s
  [2/4] Running setup: npm install -g ...            28.6s
  [3/4] Committing snapshot                          0.5s
  [4/4] Registering in catalog                       0.1s

Template created: tmpl_9c3d7e2f  (node-tools, docker, 287 MB)
```

---

### 7.2 `tag sandbox template list`

List all registered templates.

```
tag sandbox template list \
  [--backend docker|e2b] \
  [--tag KEY=VALUE ...] \
  [--sort name|created|size|runs] \
  [--json]
```

**Example — human-readable output:**
```
$ tag sandbox template list

ID              NAME                  BACKEND  BASE                 SIZE    RUNS  CREATED
tmpl_4f8a2b1c  python-datascience    docker   python:3.11-slim     412 MB  47    2026-06-10
tmpl_9c3d7e2f  node-tools            docker   node:20-slim         287 MB  12    2026-06-12
tmpl_a1b2c3d4  python-ml-gpu         e2b      e2b/python:3.11      1.2 GB  3     2026-06-15
```

**Example — JSON output:**
```
$ tag sandbox template list --json
[
  {
    "id": "tmpl_4f8a2b1c",
    "name": "python-datascience",
    "backend": "docker",
    "base_image": "python:3.11-slim",
    "artifact_ref": "tag-template:python-datascience-4f8a2b1c",
    "install_manifest": "numpy==1.26.4 pandas==2.2.1 scikit-learn==1.4.1",
    "workdir": "/workspace",
    "env_vars": {},
    "tags": {"project": "analysis", "owner": "alice"},
    "size_bytes": 431906816,
    "run_count": 47,
    "last_used_at": "2026-06-16T22:41:05Z",
    "created_at": "2026-06-10T09:14:22Z"
  }
]
```

---

### 7.3 `tag sandbox template inspect`

Show full details for one template, including package list and recent runs.

```
tag sandbox template inspect <id-or-name> [--json]
```

**Example:**
```
$ tag sandbox template inspect python-datascience

Template: python-datascience (tmpl_4f8a2b1c)
Backend:  docker
Artifact: tag-template:python-datascience-4f8a2b1c
Base:     python:3.11-slim
Workdir:  /workspace
Tags:     project=analysis, owner=alice
Created:  2026-06-10T09:14:22Z
Used:     47 times (last: 2026-06-16T22:41:05Z)

Installed packages (pip freeze output at build time):
  matplotlib==3.8.3
  numpy==1.26.4
  pandas==2.2.1
  scikit-learn==1.4.1
  seaborn==0.13.2
  ... (12 total including transitive deps)

Recent runs:
  RUN ID          STARTED               DURATION  EXIT
  run_abc123      2026-06-16T22:41:05Z  0.8s      0
  run_def456      2026-06-16T21:30:12Z  1.2s      0
  run_ghi789      2026-06-16T20:15:44Z  0.9s      1
  (showing 3 of 47)
```

---

### 7.4 `tag sandbox template delete`

Remove a template from the catalog and delete its artifact.

```
tag sandbox template delete <id-or-name> [--force] [--json]
```

- `--force`: Skip the confirmation prompt even if the template was used in the last 7 days.

**Example:**
```
$ tag sandbox template delete python-datascience

Template 'python-datascience' was last used 6 hours ago.
This will delete Docker image tag-template:python-datascience-4f8a2b1c (412 MB).

Confirm deletion? [y/N] y

Deleted template tmpl_4f8a2b1c and image tag-template:python-datascience-4f8a2b1c.
```

---

### 7.5 `tag sandbox run --template` (extension to existing command)

The `--template` flag is added to the existing `tag sandbox run` command.

```
tag sandbox run \
  --template <id-or-name> \
  [--code <python-code-string>] \
  [--file <path-to-script>] \
  [--timeout <seconds>] \
  [--env KEY=VALUE ...] \
  [--json]
```

- `--template`: Template name or ID to use. The sandbox starts from the template snapshot, not a cold base image. Incompatible with `--image` (which specifies a cold-start base image). Raises an error if both are provided.
- `--env KEY=VALUE`: Runtime environment variables layered on top of template-baked env vars. These are NOT persisted to the template.

**Example — fast iterative run:**
```
$ tag sandbox run \
    --template python-datascience \
    --code "import numpy as np; print(np.__version__)"

[sandbox: tmpl_4f8a2b1c | cold_start: 174ms]
1.26.4
[exit 0 | total: 0.31s]
```

**Example — run with a script file:**
```
$ tag sandbox run --template python-datascience --file analysis.py

[sandbox: tmpl_4f8a2b1c | cold_start: 182ms]
... analysis output ...
[exit 0 | total: 4.2s]
```

---

## 8. Functional Requirements

| ID | Requirement | Testable Condition |
|----|-------------|-------------------|
| FR-01 | `tag sandbox template create <name>` pulls the base image, runs the install command inside a container, commits the resulting container state as a new Docker image, and inserts a row into `sandbox_templates`. | `docker image inspect tag-template:<name>-<id>` succeeds after `create`; row exists in SQLite. |
| FR-02 | The `--install` flag accepts either a quoted package string or a path to a requirements.txt file. Both are normalized to a `pip install -r <tempfile>` invocation inside the build container. | Test with `--install "numpy"` and `--install requirements.txt`; both produce equivalent images. |
| FR-03 | After template creation, the `install_manifest` field in `sandbox_templates` stores the output of `pip freeze` run inside the committed image, capturing exact pinned versions of all installed packages including transitive dependencies. | Assert `install_manifest` contains `numpy==` with a version string after creating a template with `--install numpy`. |
| FR-04 | `tag sandbox run --template <name>` resolves the template by name or ID, retrieves the `artifact_ref`, and runs `docker run --rm <artifact_ref> <cmd>` (Docker backend) or `Sandbox.create(template=e2b_template_id)` (E2B backend). | Sandbox run succeeds and produces correct output without any `pip install` step visible in logs. |
| FR-05 | The wall-clock time from `tag sandbox run --template` CLI invocation to first stdout byte of user code is measured and stored as `cold_start_ms` in the `sandbox_runs` table and emitted as a span attribute `sandbox.cold_start_ms`. | `sandbox_runs` row contains `cold_start_ms` < 400 in integration test on local Docker daemon. |
| FR-06 | `--template` and `--image` are mutually exclusive flags. Providing both returns a usage error (`ErrConflictingFlags`) with message "Cannot specify both --template and --image; --template uses a pre-baked image, use one or the other." | CLI test: assert non-zero exit code and message when both flags provided. |
| FR-07 | `tag sandbox template list` returns all rows from `sandbox_templates` ordered by `created_at DESC` by default, filterable by `--backend` and `--tag`. | Insert 3 templates with different backends; assert `--backend e2b` returns only the E2B template. |
| FR-08 | `tag sandbox template delete <id>` removes the `sandbox_templates` row AND calls `docker image rm <artifact_ref>` (Docker backend) or the E2B template delete API (E2B backend). If the Docker image does not exist (already removed externally), the delete proceeds and logs a warning instead of failing. | After `delete`, `docker image ls` does not show the template image; SQLite row is gone. |
| FR-09 | `--force` on `template create` deletes the existing artifact and SQLite row before rebuilding. Without `--force`, creating with a name that already exists returns a usage error (`ErrTemplateExists`). | Assert error message when creating duplicate without `--force`; assert success with `--force`. |
| FR-10 | `tag sandbox template inspect <name>` emits the `install_manifest` (full pip freeze output), `env_vars`, `workdir`, all `tags`, and the last 10 `sandbox_runs` that referenced this template's ID. | Verify all fields present in JSON output; verify run cross-reference after running with `--template`. |
| FR-11 | Template names must match `[a-z0-9][a-z0-9_-]{0,63}` (lowercase slug, 1-64 chars). Invalid names return a usage error (`ErrInvalidName`) with a clear message describing the allowed format. | Assert `ErrInvalidName` for names with uppercase, spaces, and leading hyphens. |
| FR-12 | All template lifecycle operations (create, delete, run-from-template) emit OTEL spans with attributes `sandbox.template_id`, `sandbox.template_name`, `sandbox.backend`, and (for runs) `sandbox.cold_start_ms`. | Mock OTEL exporter in test; assert span attributes after each operation. |
| FR-13 | `tag sandbox template create` with `--timeout <N>` kills the build container and cleans up the partially committed image if the build exceeds N seconds. | Test with a `--setup "sleep 999"` command and a 2-second timeout; assert cleanup and non-zero exit. |
| FR-14 | `tag sandbox template list --json` outputs a valid JSON array parseable by `json.Unmarshal`. Each element contains all fields defined in the `SandboxTemplate` struct (JSON schema generated via `invopop/jsonschema`). | Assert `json.Unmarshal(output, &[]SandboxTemplate{})` succeeds and each item has the required keys. |
| FR-15 | `sandbox_runs` rows created from a template run include `template_id` foreign key referencing the used template. | Assert `template_id` column is set in `sandbox_runs` after `sandbox run --template`. |
| FR-16 | Template build logs are streamed to stderr in real time during `template create`. Each phase (pull, install, commit, index) is prefixed with a step indicator and elapsed time. | Test with `--base python:3.11-slim --install numpy`; assert stderr output contains "[1/4]", "[2/4]", "[3/4]", "[4/4]". |
| FR-17 | `tag sandbox template delete` prints a warning and requires `--force` or interactive confirmation when `last_used_at` is within the past 7 days. | Insert a template with `last_used_at = now() - 1 day`; assert confirmation prompt (or error without `--force`). |
| FR-18 | The `--env KEY=VALUE` flag on `sandbox run --template` injects additional environment variables at runtime without modifying the template artifact. Running the same template twice with different `--env` values produces different environment inside the container but does not alter `sandbox_templates`. | Assert environment variable visible inside sandbox; assert `sandbox_templates` row unchanged after run. |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Cold start latency (Docker backend, warm local image) | p50 <200ms, p95 <400ms from CLI invoke to first user code stdout byte |
| NFR-02 | Cold start latency (E2B backend) | p50 <500ms including E2B API round-trip; dependent on E2B SLA |
| NFR-03 | Template create build time | Does not add overhead beyond the base `docker pull + pip install + docker commit` sequence; `docker commit` step <2s for images under 2 GB |
| NFR-04 | SQLite operation latency | All reads/writes to `sandbox_templates` complete in <5ms; table is indexed on `name` and `backend` |
| NFR-05 | Memory footprint | `tag sandbox template list` with 100 templates consumes <50 MB RSS; template metadata is lazy-loaded |
| NFR-06 | Backward compatibility | All existing `tag sandbox run` invocations without `--template` are functionally identical before and after this feature; no performance regression |
| NFR-07 | Disk space reporting | `size_bytes` in `sandbox_templates` reflects the actual Docker image size as reported by `docker image inspect`; refreshed on each `list` invocation |
| NFR-08 | Error messages | All errors include the template name/ID, the backend, and a suggested remediation action |
| NFR-09 | Concurrent runs | Multiple concurrent `sandbox run --template` invocations from the same template do not interfere; Docker's image layer cache makes this naturally safe |
| NFR-10 | OTEL span overhead | Template-related OTEL span creation adds <1ms to run latency; spans are created asynchronously where possible |
| NFR-11 | Test coverage | All FR-* requirements have corresponding unit or integration tests; overall coverage for new code ≥80% |
| NFR-12 | Go toolchain support | Built with Go 1.24+, `CGO_ENABLED=0`, single static binary (matching TAG's build baseline) |

---

## 10. Technical Design

### 10.1 New Files

All template logic lives under the `internal/sandbox` package of module `github.com/tag-agent/tag` (Go 1.24+, `CGO_ENABLED=0`).

| File | Purpose |
|------|---------|
| `internal/sandbox/template.go` | `SandboxTemplate` / `TemplateBuildResult` structs, `EnsureTemplateSchema()`, `ResolveTemplate()`, `ValidateTemplateName()` |
| `internal/sandbox/template_docker.go` | Docker-backend build (`buildTemplateDocker`) via moby client image commit, and `runFromTemplateDocker` |
| `internal/sandbox/template_firecracker.go` | Firecracker/microVM tier build + VM-snapshot restore (`firecracker-go-sdk`), Linux-only, build-tagged |
| `internal/sandbox/catalog.go` | `modernc.org/sqlite` catalog access (single-writer, WAL); embedded DDL via `//go:embed schema.sql` |
| `internal/cli/sandbox_template.go` | `go-chi/chi`-registered subcommands / cobra commands `create`, `list`, `inspect`, `delete`, and the `--template` extension to `run` |

The embedded DDL is applied by `EnsureTemplateSchema()`. All exec of untrusted or long-running processes uses `os/exec.CommandContext` with `Setpgid` for process-group kill.

### 10.2 SQLite DDL

Backed by `modernc.org/sqlite` (pure-Go, no CGO), opened in WAL mode with a single writer. The DDL is `//go:embed`-ed as `schema.sql` and applied by `EnsureTemplateSchema()` inside a transaction.

```sql
-- Migration: add sandbox_templates table to ~/.tag/runtime/tag.sqlite3
-- Applied via EnsureTemplateSchema() (modernc.org/sqlite, WAL mode)

CREATE TABLE IF NOT EXISTS sandbox_templates (
    id              TEXT PRIMARY KEY,           -- 'tmpl_' + 8 hex chars
    name            TEXT NOT NULL UNIQUE,       -- human slug [a-z0-9_-]{1,64}
    backend         TEXT NOT NULL,              -- 'docker' | 'e2b'
    base_image      TEXT NOT NULL,              -- e.g. 'python:3.11-slim'
    artifact_ref    TEXT NOT NULL,              -- docker: 'tag-template:<name>-<id>'
                                                -- e2b: e2b template ID string
    install_cmd     TEXT,                       -- original --install argument
    install_manifest TEXT,                      -- pip freeze output post-build
    setup_cmd       TEXT,                       -- original --setup argument
    workdir         TEXT NOT NULL DEFAULT '/workspace',
    env_vars        TEXT NOT NULL DEFAULT '{}', -- JSON object of baked env vars
    tags            TEXT NOT NULL DEFAULT '{}', -- JSON object of user tags
    size_bytes      INTEGER,                    -- Docker image size in bytes
    run_count       INTEGER NOT NULL DEFAULT 0,
    last_used_at    TEXT,                       -- ISO-8601 UTC, nullable
    created_at      TEXT NOT NULL,              -- ISO-8601 UTC
    updated_at      TEXT NOT NULL               -- ISO-8601 UTC
);

CREATE INDEX IF NOT EXISTS idx_st_name ON sandbox_templates(name);
CREATE INDEX IF NOT EXISTS idx_st_backend ON sandbox_templates(backend);
CREATE INDEX IF NOT EXISTS idx_st_created ON sandbox_templates(created_at DESC);

-- Extend sandbox_runs to reference templates
-- Add column if not exists (handled at migration time):
ALTER TABLE sandbox_runs ADD COLUMN template_id TEXT REFERENCES sandbox_templates(id);
ALTER TABLE sandbox_runs ADD COLUMN cold_start_ms INTEGER;

CREATE INDEX IF NOT EXISTS idx_sr_template ON sandbox_runs(template_id, created_at DESC);
```

Note: SQLite does not support `ADD COLUMN IF NOT EXISTS`. `EnsureTemplateSchema()` queries `PRAGMA table_info(sandbox_runs)` and only issues the `ALTER TABLE` statements when the columns are absent, keeping the migration idempotent.

### 10.3 Core Structs

JSON schema for the `--json` surface is generated with `invopop/jsonschema`. `env_vars` and `tags` are persisted as JSON `TEXT` columns and (de)serialized with `encoding/json`.

```go
package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type Backend string

const (
	BackendDocker      Backend = "docker"
	BackendFirecracker Backend = "firecracker"
)

// SandboxTemplate is a pre-baked, snapshotted sandbox environment.
type SandboxTemplate struct {
	ID              string            `json:"id"`               // "tmpl_" + 8 hex chars
	Name            string            `json:"name"`             // human slug
	Backend         Backend           `json:"backend"`          // docker | firecracker
	BaseImage       string            `json:"base_image"`       // base image / rootfs reference
	ArtifactRef     string            `json:"artifact_ref"`     // committed image tag or VM-snapshot id
	InstallCmd      string            `json:"install_cmd,omitempty"`
	InstallManifest string            `json:"install_manifest,omitempty"` // resolved dependency lock captured post-build
	SetupCmd        string            `json:"setup_cmd,omitempty"`
	Workdir         string            `json:"workdir"`
	EnvVars         map[string]string `json:"env_vars"`
	Tags            map[string]string `json:"tags"`
	SizeBytes       int64             `json:"size_bytes,omitempty"`
	RunCount        int64             `json:"run_count"`
	LastUsedAt      *time.Time        `json:"last_used_at,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// NewTemplate mints an ID and derives the backend-specific artifact reference.
func NewTemplate(name string, backend Backend, baseImage string) (*SandboxTemplate, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	id := "tmpl_" + hex.EncodeToString(b[:])
	now := time.Now().UTC()
	t := &SandboxTemplate{
		ID:        id,
		Name:      name,
		Backend:   backend,
		BaseImage: baseImage,
		Workdir:   "/workspace",
		EnvVars:   map[string]string{},
		Tags:      map[string]string{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Firecracker artifact_ref is assigned after the VM snapshot is taken.
	if backend == BackendDocker {
		t.ArtifactRef = fmt.Sprintf("tag-template:%s-%s", name, id[len("tmpl_"):])
	}
	return t, nil
}

// marshalMaps returns the JSON encodings persisted to the env_vars / tags columns.
func (t *SandboxTemplate) marshalMaps() (envJSON, tagsJSON string, err error) {
	e, err := json.Marshal(t.EnvVars)
	if err != nil {
		return "", "", err
	}
	g, err := json.Marshal(t.Tags)
	if err != nil {
		return "", "", err
	}
	return string(e), string(g), nil
}

// TemplateBuildResult summarizes a completed build for progress reporting.
type TemplateBuildResult struct {
	Template        *SandboxTemplate
	BuildDuration   time.Duration
	InstallDuration time.Duration
	CommitDuration  time.Duration
	LogLines        []string
}
```

### 10.4 Core Algorithms

#### 10.4.1 Template Create (Docker/Container Tier)

Uses the `docker/docker` (moby) client for `ImagePull` → `ContainerCreate`/`ContainerStart`/`ContainerWait` → `ContainerCommit`. The build container's lifetime is bounded by `context.WithTimeout` and always removed via `defer`. When the Docker runtime is configured for gVisor (`runsc`), the same code path yields a gVisor-isolated build with no changes.

```go
package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dclient "github.com/docker/docker/client"
)

// LogFunc streams a progress line to the terminal.
type LogFunc func(string)

// buildTemplateDocker builds a container-tier template:
//  1. ImagePull(base)
//  2. ContainerCreate + Start running the install/setup script
//  3. ContainerWait for exit
//  4. ContainerCommit -> artifact_ref
//  5. run the committed image once to capture the dependency lock -> InstallManifest
func buildTemplateDocker(
	ctx context.Context,
	cli dclient.APIClient,
	t *SandboxTemplate,
	installSpec, setupCmd string,
	timeout time.Duration,
	log LogFunc,
) (*TemplateBuildResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	buildCtr := "tag-template-build-" + t.ID

	// Step 1: pull base image.
	log(fmt.Sprintf("  [1/4] Pulling base image %s", t.BaseImage))
	tPull := time.Now()
	rc, err := cli.ImagePull(ctx, t.BaseImage, image.PullOptions{})
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", t.BaseImage, err)
	}
	_ = drain(rc) // surface layer progress to log; closes rc
	log(fmt.Sprintf("        done in %.1fs", time.Since(tPull).Seconds()))

	// Step 2: assemble and run the install/setup script.
	log("  [2/4] Installing packages")
	tInstall := time.Now()
	script := buildInstallScript(installSpec, setupCmd, t.Workdir) // safe: no shell interpolation of user args

	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image:      t.BaseImage,
		WorkingDir: t.Workdir,
		Cmd:        []string{"/bin/sh", "-c", script},
	}, nil, nil, nil, buildCtr)
	if err != nil {
		return nil, fmt.Errorf("create build container: %w", err)
	}
	// Always clean up the build container (even on failure/timeout).
	defer cli.ContainerRemove(context.Background(), created.ID,
		container.RemoveOptions{Force: true})

	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("start build container: %w", err)
	}
	if err := waitExitZero(ctx, cli, created.ID); err != nil {
		return nil, err
	}
	installDur := time.Since(tInstall)
	log(fmt.Sprintf("        done in %.1fs", installDur.Seconds()))

	// Step 3: commit the stopped container as the template image.
	log("  [3/4] Committing snapshot")
	tCommit := time.Now()
	commitResp, err := cli.ContainerCommit(ctx, created.ID, container.CommitOptions{
		Reference: t.ArtifactRef,
	})
	if err != nil {
		return nil, fmt.Errorf("commit snapshot: %w", err)
	}
	commitDur := time.Since(tCommit)
	log(fmt.Sprintf("        done in %.1fs (image %s)", commitDur.Seconds(), commitResp.ID))

	// Step 4: capture the resolved dependency lock and image size.
	log("  [4/4] Registering in catalog")
	if manifest, err := captureManifest(ctx, cli, t.ArtifactRef); err == nil {
		t.InstallManifest = strings.TrimSpace(manifest)
	}
	if insp, _, err := cli.ImageInspectWithRaw(ctx, t.ArtifactRef); err == nil {
		t.SizeBytes = insp.Size
	}

	return &TemplateBuildResult{
		Template:        t,
		BuildDuration:   time.Since(start),
		InstallDuration: installDur,
		CommitDuration:  commitDur,
	}, nil
}
```

#### 10.4.2 Run from Template (Container Tier) with Cold-Start Measurement

`docker run --rm <artifact_ref>` is driven with `os/exec.CommandContext` so the run is bounded by the caller's context. `Setpgid` lets us kill the whole process group on timeout. First-byte latency is captured by an `io.Writer` shim on stdout; the read pumps run as goroutines coordinated with `errgroup`.

```go
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
)

type RunResult struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	ColdStart time.Duration // CLI invocation -> first stdout byte
}

// firstByteWriter records the elapsed time to the first byte written.
type firstByteWriter struct {
	start time.Time
	once  sync.Once
	at    time.Duration
	buf   bytes.Buffer
}

func (w *firstByteWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.once.Do(func() { w.at = time.Since(w.start) })
	}
	return w.buf.Write(p)
}

func runFromTemplateDocker(
	ctx context.Context,
	t *SandboxTemplate,
	code, filePath string,
	timeout time.Duration,
	extraEnv map[string]string,
) (*RunResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"run", "--rm", "--workdir", t.Workdir}
	for k, v := range mergeEnv(t.EnvVars, extraEnv) { // extraEnv wins; not persisted
		args = append(args, "--env", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, t.ArtifactRef)
	switch {
	case code != "":
		args = append(args, "python3", "-c", code)
	case filePath != "":
		args = append(args, "python3", "/workspace/script.py")
	default:
		args = append(args, "/bin/bash")
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // process-group kill on cancel
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	out := &firstByteWriter{start: start}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	g, _ := errgroup.WithContext(ctx)
	g.Go(cmd.Wait)
	_ = g.Wait() // exit code captured via ProcessState below

	cold := out.at
	if cold == 0 { // no stdout produced
		cold = time.Since(start)
	}
	return &RunResult{
		ExitCode:  cmd.ProcessState.ExitCode(),
		Stdout:    out.buf.String(),
		Stderr:    stderr.String(),
		ColdStart: cold,
	}, nil
}

var _ = io.Discard // stdout/stderr wired above
```

#### 10.4.3 Template Resolution

```go
package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrTemplateNotFound = errors.New("template not found")

// ResolveTemplate resolves a template by ID or name, returning a wrapped
// ErrTemplateNotFound (with suggestions) when absent.
func ResolveTemplate(ctx context.Context, db *sql.DB, nameOrID string) (*SandboxTemplate, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, name, backend, base_image, artifact_ref, install_cmd,
		        install_manifest, setup_cmd, workdir, env_vars, tags,
		        size_bytes, run_count, last_used_at, created_at, updated_at
		 FROM sandbox_templates WHERE id = ? OR name = ? LIMIT 1`,
		nameOrID, nameOrID)

	t, err := scanTemplate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("template %q: %w.%s", nameOrID, ErrTemplateNotFound, suggestNames(ctx, db))
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func suggestNames(ctx context.Context, db *sql.DB) string {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sandbox_templates ORDER BY name LIMIT 5`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return " Available templates: " + strings.Join(names, ", ")
}
```

### 10.5 Integration Points

| Integration | How |
|-------------|-----|
| `internal/sandbox` — existing `RunSandbox()` | Add a `TemplateID string` field to the run options struct. When set, delegate to `runFromTemplate*` instead of the cold-start path. |
| `internal/cli` — `sandbox run` command | Parse `--template` flag; pass `TemplateID` into the run options. Add mutually-exclusive check with `--image` (returns `ErrConflictingFlags`). |
| `internal/cli` — `sandbox template` commands | New command group registered under the `sandbox template` cobra/chi subcommand tree. |
| `internal/telemetry` — OTEL tracer (`otel-go`) | Wrap template create, run, and delete in spans; set `sandbox.template_id`, `sandbox.template_name`, `sandbox.cold_start_ms` attributes. |
| `internal/budget` — `RecordCost()` | Record template build compute time as a cost event with `operation=sandbox_template_build`. |
| `OpenDB()` — existing helper (`modernc.org/sqlite`) | Call `EnsureTemplateSchema(ctx, db)` at the start of each template command; idempotent migration. |

### 10.6 Name Validation

```go
package sandbox

import (
	"errors"
	"fmt"
	"regexp"
)

var (
	templateNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	ErrInvalidName  = errors.New("invalid template name")
)

// ValidateTemplateName enforces the lowercase-slug naming rule.
func ValidateTemplateName(name string) error {
	if !templateNameRE.MatchString(name) {
		return fmt.Errorf("%w %q: names must start with a lowercase letter or digit, "+
			"contain only lowercase letters, digits, hyphens, and underscores, "+
			"and be 1-64 characters long", ErrInvalidName, name)
	}
	return nil
}
```

### 10.7 Firecracker microVM Tier (self-hosted; replaces managed E2B)

In the native single-binary model TAG owns the strongest isolation tier directly rather than delegating to a managed cloud (E2B). The microVM tier uses `firecracker-microvm/firecracker-go-sdk` to boot a VM from a base rootfs, run the install/setup steps over vsock, then take a **VM snapshot** (memory + device state) via the pause/snapshot/resume API. The snapshot plus a copy-on-write `overlayfs` diff over the base rootfs *is* the template artifact — resuming a snapshot yields E2B-comparable ~150ms restore, but on infrastructure TAG runs itself.

> **Linux-only.** Firecracker (and KVM `/dev/kvm`, vsock, overlayfs) exist only on Linux. This tier is compiled behind a `//go:build linux` tag and feature-detected at runtime; on macOS/Windows the binary degrades to the container tier (Docker) automatically. See the isolation ladder in `docs/GO_MIGRATION_RESEARCH.md`.

```go
//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// buildTemplateFirecracker boots a microVM from the base rootfs, provisions it,
// then persists a VM snapshot + COW overlay as the template artifact.
func buildTemplateFirecracker(
	ctx context.Context,
	t *SandboxTemplate,
	installSpec, setupCmd string,
	timeout time.Duration,
	log LogFunc,
) (*TemplateBuildResult, error) {
	if err := requireKVM(); err != nil { // /dev/kvm present + accessible
		return nil, fmt.Errorf("firecracker tier unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()

	log(fmt.Sprintf("  [1/3] Booting microVM from rootfs %s", t.BaseImage))
	overlay, err := newOverlay(t.ID, t.BaseImage) // COW overlayfs over base rootfs
	if err != nil {
		return nil, err
	}
	cfg := fc.Config{
		SocketPath:      overlay.apiSock,
		KernelImagePath: overlay.kernel,
		Drives: []models.Drive{{
			DriveID:      strPtr("rootfs"),
			PathOnHost:   strPtr(overlay.rootfs),
			IsRootDevice: boolPtr(true),
		}},
		VsockDevices: []fc.VsockDevice{{CID: 3, Path: overlay.vsock}},
	}
	m, err := fc.NewMachine(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := m.Start(ctx); err != nil {
		return nil, err
	}
	defer m.StopVMM() // always tear down the VMM

	log("  [2/3] Installing packages over vsock")
	if err := guestExec(ctx, overlay, installScript(installSpec, setupCmd, t.Workdir)); err != nil {
		return nil, err
	}
	if manifest, err := guestCapture(ctx, overlay); err == nil {
		t.InstallManifest = strings.TrimSpace(manifest)
	}

	log("  [3/3] Pausing VM and creating snapshot")
	if err := m.PauseVM(ctx); err != nil {
		return nil, err
	}
	snapID := "snap_" + t.ID
	if err := m.CreateSnapshot(ctx, overlay.memFile(snapID), overlay.stateFile(snapID)); err != nil {
		return nil, err
	}
	t.ArtifactRef = snapID // catalog stores the snapshot handle
	t.SizeBytes = overlay.snapshotSize(snapID)

	return &TemplateBuildResult{Template: t, BuildDuration: time.Since(start)}, nil
}
```

Running from a Firecracker template resumes the snapshot with `firecracker-go-sdk`'s `LoadSnapshot` + `ResumeVM` instead of a cold boot; `runFromTemplateFirecracker` measures cold-start identically to the container tier. The snapshot memory/state files and the COW overlay are content-addressed with `crypto/sha256` so identical template definitions can share base layers.

---

## 11. Security Considerations

1. **No credential baking**: The `--env` flag at template create time does NOT accept values matching TAG's credential patterns (`*SECRET*`, `*KEY*`, `*TOKEN*`, `*PASSWORD*`, `AWS_*`, `ANTHROPIC_*`). Attempting to bake these into a template returns a sentinel `ErrCredentialInEnv` (non-zero exit). Runtime `--env` on `sandbox run` accepts any key but does not persist to the template.

2. **Artifact namespace isolation**: All container-tier images created by the template system use the `tag-template:` prefix. The `template delete` command only removes images matching this prefix, preventing accidental deletion of user images. `ContainerCommit` does not run any new processes; it snapshots the stopped container's filesystem.

3. **Build container cleanup**: The build container (`tag-template-build-<id>`) is always removed in a `finally` block, even on build failure or timeout. This prevents abandoned containers with potentially sensitive build contexts from persisting on the host.

4. **Template name injection prevention**: Template names are strictly validated against `[a-z0-9][a-z0-9_-]{0,63}` before use in image tags and VM snapshot handles. All process invocation goes through `os/exec.CommandContext` with an explicit argv slice (no shell string, no `sh -c` over user input), so shell metacharacter injection is structurally impossible.

5. **Mount validation inheritance**: When running from a template, all mount validation rules from PRD-028 (blocking paths matching `*.env`, `*.key`, `~/.ssh/*`, `~/.aws/*`) remain in force. Templates do not bypass the existing sandbox mount security layer.

6. **microVM host privileges**: The Firecracker tier requires access to `/dev/kvm` and vsock. TAG runs the VMM as an unprivileged user via a jailer-style setup and reads no cloud credentials — there is no managed-service API key to leak. Host paths for rootfs/snapshot files are confined to `~/.tag/runtime/firecracker/` and never logged in OTEL span attributes.

7. **Disk exhaustion guard**: Template creation checks available disk space (`golang.org/x/sys/unix.Statfs`) before `ContainerCommit` / snapshot write. If available space is less than 2× the base image (or rootfs) size, the build aborts with a clear error. The image/rootfs size query runs before the commit step.

8. **Concurrent create isolation**: Two concurrent `template create` calls with the same name race on the `UNIQUE` constraint on `sandbox_templates.name`. The modernc.org/sqlite driver surfaces the second write as a `SQLITE_CONSTRAINT` error, which is wrapped as `ErrTemplateExists` suggesting `--force`. The catalog uses a single writer under WAL to avoid interleaved commits.

9. **Snapshot handle opacity**: VM snapshot handles (`snap_<id>`) and container image digests are treated as opaque identifiers stored as-is in `artifact_ref`. No user-controlled input is concatenated into filesystem paths without validation; all moby / firecracker-go-sdk calls use typed parameters rather than shell strings.

10. **Audit trail**: All template lifecycle events (create, delete, run-from-template) are appended to `~/.tag/runtime/sandbox-audit.jsonl` (same file as PRD-028), with `template_id`, `template_name`, `backend`, and `outcome` fields. This provides a complete record for post-incident forensics.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/sandbox/template_test.go`)

Standard `testing` package with table-driven subtests (`t.Run`); assertions via `stretchr/testify`.

- **Schema migration**: Open an in-memory `modernc.org/sqlite` DB, call `EnsureTemplateSchema()` twice, assert idempotency. Assert `sandbox_runs` gains `template_id` and `cold_start_ms` columns via `PRAGMA table_info`.
- **Name validation**: Table-driven cases — 20 valid and 15 invalid names; assert `ValidateTemplateName()` returns `ErrInvalidName` (`errors.Is`) for all invalid inputs.
- **Template resolution**: Insert 3 templates, assert `ResolveTemplate()` finds by both `id` and `name`; assert wrapped `ErrTemplateNotFound` with suggestion string for unknown name.
- **Row serialization**: Construct a `SandboxTemplate`, persist and re-scan via the catalog, assert round-trip equality (including `EnvVars` / `Tags` maps) with `reflect.DeepEqual` / `testify.Equal`.
- **Mutual exclusion**: Invoke the cobra command with both `--template` and `--image`; assert `errors.Is(err, ErrConflictingFlags)`.
- **Credential env guard**: Assert `errors.Is(err, ErrCredentialInEnv)` when `--env AWS_SECRET_ACCESS_KEY=foo` is passed to `template create`.
- **`--force` overwrite**: Inject a fake moby `client.APIClient`; assert the existing template is deleted and a new one inserted when `--force` is set.
- **Timeout kill**: Use a fake exec/runner whose command blocks; drive with a short `context.WithTimeout`; assert the `defer` cleanup (`ContainerRemove` / `StopVMM`) runs and the error is `context.DeadlineExceeded`.

### 12.2 Integration Tests (`internal/sandbox/template_integration_test.go`)

Gated behind `//go:build integration` and skipped via `t.Skip` when `docker info` (or `/dev/kvm`) is unavailable.

- **End-to-end create and run**: Create `python-minimal` template with `--install "uuid"`; run with `--code "import uuid; print(uuid.uuid4())"`. Assert exit 0, UUID-format output, `ColdStart < 500ms` recorded in `sandbox_runs`.
- **Dependency-lock manifest**: After create, assert `install_manifest` in `sandbox_templates` contains `uuid==` with a version string.
- **Reproducibility**: Run from template 5 times; assert identical stdout across all runs (`pip freeze | sort`).
- **Template delete cleans image**: After `create` + `delete`, assert no `tag-template:*` image remains (moby `ImageList` filter returns empty).
- **`--env` override**: Create template, run with `--env MYVAR=hello`, assert `MYVAR` visible inside sandbox; assert `sandbox_templates.env_vars` unchanged.
- **`inspect` run cross-reference**: Create template, run 3 times, call `inspect`; assert `run_count == 3` and `last_used_at` recent.
- **Concurrent runs**: Launch 5 concurrent `sandbox run --template` goroutines (via `errgroup`); assert all 5 exit 0 with independent output.

### 12.3 Benchmarks (`internal/sandbox/template_bench_test.go`)

- **Latency benchmark** (`func BenchmarkColdStart`, `-benchtime`): After creating a `python-datascience` template, run consecutive `sandbox run --template` invocations. Assert p50 `ColdStart < 200ms` and p95 `< 400ms` (percentiles computed over `b.N` samples).
- **Baseline comparison**: Benchmark 5 cold-start runs (no template, fresh install). Assert template p50 is at least 30× faster than cold-start p50.
- **`list` with 100 templates**: Seed 100 rows; assert `template list` completes in <100ms (`testing.B` with `b.ReportMetric`).

### 12.4 OTEL Span Tests

- Use the OTEL `tracetest.SpanRecorder` (in-memory `SpanExporter`); assert `template_create`, `template_delete`, and `sandbox_run_from_template` spans are emitted with correct attributes after each operation.
- Assert the `sandbox.cold_start_ms` attribute is set on `sandbox_run_from_template` spans after a container-tier run.

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag sandbox template create python-datascience --base python:3.11-slim --install "numpy pandas scikit-learn"` completes without error and creates a row in `sandbox_templates` and a Docker image `tag-template:python-datascience-*`. | Run command; check `tag sandbox template list` and `docker image ls`. |
| AC-02 | `tag sandbox run --template python-datascience --code "import numpy as np; print(np.__version__)"` outputs a numpy version string and `cold_start_ms` in the status line. | Run command; verify output format and non-empty version string. |
| AC-03 | Cold start time reported in `sandbox_runs.cold_start_ms` for the above run is <400ms on a machine where the template image is already pulled. | Query SQLite: `SELECT cold_start_ms FROM sandbox_runs ORDER BY created_at DESC LIMIT 1`. |
| AC-04 | Running the same template 5 times with `--code "pip freeze | sort"` produces identical stdout each time. | Diff outputs from 5 runs; assert zero diff. |
| AC-05 | `tag sandbox template list --json` emits valid JSON array; each element contains `id`, `name`, `backend`, `base_image`, `artifact_ref`, `install_manifest`, `size_bytes`, `run_count`, `created_at`. | `jq '.[0] | keys'` includes all required fields. |
| AC-06 | `tag sandbox template inspect python-datascience` shows `run_count` matching the actual number of `sandbox_runs` rows with `template_id = 'tmpl_*'`. | Cross-reference `inspect` output with SQLite count. |
| AC-07 | `tag sandbox template delete python-datascience` removes the SQLite row and removes the Docker image. After deletion, `docker image ls | grep tag-template:python-datascience` returns empty. | Run delete; verify via `docker image ls` and `tag sandbox template list`. |
| AC-08 | `tag sandbox template delete <recently-used-template>` prompts for confirmation when last use was within 7 days; proceeds with `--force` without prompt. | Test interactively and with `--force` flag. |
| AC-09 | `tag sandbox run --template nonexistent` exits non-zero with a clear error message naming the missing template and listing available templates. | Run command; assert exit code 1 and error message format. |
| AC-10 | `tag sandbox run --template foo --image python:3.11` exits non-zero (`ErrConflictingFlags`) with message containing "Cannot specify both --template and --image". | Run command; assert non-zero exit and message. |
| AC-11 | `tag sandbox template create UPPERCASE` exits non-zero (`ErrInvalidName`) describing valid name format. | Run command; assert exit code 1 and message. |
| AC-12 | `tag sandbox template create` with `--env AWS_SECRET_ACCESS_KEY=test` exits non-zero (`ErrCredentialInEnv`) before any container/VM operations are initiated. | Run command; verify no containers were created (via `docker ps -a`). |
| AC-13 | After `tag sandbox template create`, the `install_manifest` column in `sandbox_templates` contains `numpy==` followed by a version string. | `SELECT install_manifest FROM sandbox_templates WHERE name='python-datascience'` and grep for `numpy==`. |
| AC-14 | A template run emits a span with `sandbox.template_id`, `sandbox.template_name`, and `sandbox.cold_start_ms` attributes to the OTEL exporter. | Run with OTEL exporter configured to stdout; grep output for span attributes. |
| AC-15 | All existing `tag sandbox run` commands (without `--template`) produce identical output and latency before and after this feature is deployed. | Run PRD-028 integration test suite; assert all tests pass. |

---

## 14. Dependencies

| Dependency | Type | Required / Optional | Notes |
|------------|------|---------------------|-------|
| Docker Engine (v24+) | Runtime | Required for container tier | Detected via moby client `Ping`; graceful degrade if absent |
| `github.com/docker/docker` (moby client) | Go module | Required (container tier) | `ImagePull` / `ContainerCommit` / `ImageList`; optional gVisor `runsc` runtime |
| `github.com/firecracker-microvm/firecracker-go-sdk` | Go module | Optional (microVM tier) | Linux-only, `//go:build linux`; needs `/dev/kvm`, vsock, overlayfs |
| `github.com/landlock-lsm/go-landlock`, `github.com/elastic/go-seccomp-bpf`, `github.com/google/nftables` | Go module | Optional (restricted tier) | Lowest rung of the isolation ladder for lightweight runs; Linux-only |
| `modernc.org/sqlite` | Go module | Required | Pure-Go (no CGO) template/snapshot catalog; single-writer, WAL |
| PRD-028 (Sandbox Code Execution) | Feature | Required | Template system extends `internal/sandbox`; base `sandbox_runs` table and `RunSandbox()` must exist |
| PRD-013 (Agent Tracing) | Feature | Required | OTEL tracer (`go.opentelemetry.io/otel`) used for lifecycle spans |
| PRD-034 (Secret Scanning) | Feature | Recommended | Credential pattern matching for `--env` guard re-uses existing patterns |
| PRD-012 (Cost Tracking) | Feature | Optional | Template build cost attribution via `internal/budget` |
| `os/exec` (stdlib) | Go stdlib | Required | `CommandContext` + `Setpgid` for process-group kill |
| `golang.org/x/sync/errgroup` | Go module | Required | Concurrent stream pumps and concurrent-run tests |
| `github.com/invopop/jsonschema` | Go module | Required | JSON schema generation for `--json` surface |
| `crypto/rand`, `crypto/sha256`, `encoding/json` (stdlib) | Go stdlib | Required | Template ID generation, content-addressed hashing, catalog (de)serialization |

---

## 15. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|-------------------|
| OQ-1 | Should `tag sandbox template create` support a `--from-running <sandbox_id>` flag that snapshots a currently-running sandbox (analogous to E2B's pause/snapshot workflow) rather than rebuilding from scratch? This would enable "install packages interactively, then freeze" workflows. | Engineering | Evaluate after v1 ships; tracked in #348 comments |
| OQ-2 | Should the microVM tier snapshot full VM memory (`CreateSnapshot` diff snapshots via firecracker-go-sdk) or fall back to overlayfs-only filesystem snapshots when memory snapshotting is unavailable on the host kernel? Memory snapshots give the ~150ms resume but require a matching kernel/uffd configuration. | Engineering | Confirm minimum kernel + firecracker-go-sdk snapshot support on target hosts |
| OQ-3 | Should template images use a content-addressable name (hash of base_image + install_cmd) to enable transparent deduplication when two templates have identical definitions? | Engineering | Deferred to v2 if disk usage becomes a concern |
| OQ-4 | Since both tiers are now self-hosted, all templates live in the local SQLite catalog. Should `list` also reconcile against on-disk artifacts (dangling images / orphaned snapshot files) and flag drift, or trust the catalog as source of truth? | Product | Decide before GA; likely add a `template gc` reconcile command |
| OQ-5 | What is the maximum allowed `size_bytes` for a Docker-backend template before `template create` warns the user? 2 GB is a reasonable default but may need to be configurable. | Engineering | Set default at 2 GB with `--max-size` flag for override |
| OQ-6 | Should `tag sandbox run --template` support `--mount <host_path>:<container_path>` for injecting read-only data into a template-based run without modifying the template? This is a common pattern for data science (mount dataset directory). | Product | Likely yes; implement with same mount validation rules from PRD-028 |
| OQ-7 | Is there a use case for sharing templates across a team via a shared SQLite database (e.g., on a network mount or synced via git-lfs)? Or is per-developer local storage sufficient? | Product | Survey users after v1 ships |
| OQ-8 | Should `tag sandbox template create` accept a `Dockerfile` path as an alternative to `--base + --install`, for users who already have Dockerfiles describing their environments? | Engineering | Low complexity; consider for v1.1 |

---

## 16. Complexity and Timeline

### Phase 1 — Core Container Tier (Days 1-4)

- `EnsureTemplateSchema()` with idempotent migration for `sandbox_templates` and `ALTER TABLE sandbox_runs` (Day 1)
- `SandboxTemplate` struct, `ValidateTemplateName()`, `ResolveTemplate()` (Day 1)
- `buildTemplateDocker()`: pull → install → commit → manifest → size via moby client (Days 1-2)
- `sandbox template create` command in `internal/cli` with progress streaming (Day 2)
- `sandbox template list` with `--backend` and `--tag` filters, `--json` output (Day 2)
- `sandbox template delete` with recency guard and `--force` (Day 3)
- `sandbox template inspect` with run cross-reference (Day 3)
- `runFromTemplateDocker()` with cold-start measurement via first-byte writer + `errgroup` (Day 3)
- Extend `sandbox run` with `--template` flag and mutual-exclusion check (Day 4)
- Unit tests for all above (`go test ./internal/sandbox/...`) (Day 4)

### Phase 2 — OTEL, Security, and Integration (Days 5-7)

- OTEL span instrumentation for all template lifecycle events (Day 5)
- Credential pattern guard for `--env` in `template create` (Day 5)
- Disk space pre-check (`unix.Statfs`) before `ContainerCommit` (Day 5)
- Integration tests (`//go:build integration`) for end-to-end create/run/delete/inspect (Day 6)
- Benchmarks (`testing.B`, latency vs. baseline) (Day 6)
- Audit log appender to `sandbox-audit.jsonl` (Day 6)
- Budget integration for template build cost attribution (Day 7)
- CLI help text, error message polish (Day 7)

### Phase 3 — Firecracker microVM Tier (Days 8-10, Linux-only)

- `buildTemplateFirecracker()` using firecracker-go-sdk boot + pause + `CreateSnapshot` over COW overlayfs (Day 8)
- `runFromTemplateFirecracker()` using `LoadSnapshot` + `ResumeVM` (Day 8)
- Feature-detection / degrade-off-Linux plumbing behind `//go:build linux`; unit tests with a fake VMM (Day 9)
- microVM integration tests (gated on `/dev/kvm` availability) (Day 9)
- Documentation: update `docs/prd/INDEX.md`, add template examples to `README`; GoReleaser + cosign + SLSA build wiring for the static binary (Day 10)
- Final review, edge case hardening, and merge (Day 10)

**Total: 10 working days (2 calendar weeks)**

The container tier (Phase 1-2) is independently shippable as a v1 milestone and delivers the primary user value of <200ms cold starts on any OS with a Docker daemon. The Firecracker microVM tier (Phase 3) is **Linux-only** and ships as a follow-on without blocking Phase 1-2 users; on non-Linux hosts the binary transparently degrades to the container tier.

