# PRD-096: Persistent Volume Mounts Across Sandbox Runs (`tag sandbox volume`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py + sandbox_volumes SQLite`
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-034 (Secret Scanning / security.py), PRD-013 (Agent Tracing/Observability), PRD-005 (Execution Backend Selection), PRD-094 (Per-Sandbox Egress Firewall), PRD-097 (Sandbox Secrets Vault), PRD-012 (Cost Tracking / Budget), PRD-020 (CI/CD Integration)
**Inspired by:** Modal volumes, E2B filesystem, Daytona persistent workspace
**GitHub Issue:** #348

---

## 1. Overview

TAG's sandbox execution layer (PRD-028) provides strong ephemeral isolation for agent-generated code: containers and micro-VMs are created fresh per invocation and torn down completely on exit. This design correctly eliminates state leakage between untrusted runs. However, it creates a critical usability gap for any workflow that generates artifacts worth keeping — trained model checkpoints, compiled binaries, downloaded datasets, processed corpora, shared dependency caches — that must survive sandbox termination and be available to subsequent runs.

Today, every `tag sandbox run` invocation begins with a cold filesystem. An agent workflow that fine-tunes a model, saves the checkpoint, and then immediately terminates loses that checkpoint permanently. If the user wants to evaluate the checkpoint in a second sandbox run, they must either fold both steps into a single monolithic run (losing isolation) or manually extract and re-inject data via host filesystem mounts (breaking the security model). Neither approach scales to multi-step agent pipelines or concurrent multi-sandbox experiments.

Persistent Volume Mounts solves this by introducing **named volumes** (`sandbox_volumes`) as a first-class TAG resource. A volume is a content-addressed, host-local directory that lives under `~/.tag/volumes/<name>/` and is bind-mounted into sandbox containers at a user-specified path on each run. Volumes survive sandbox termination by design. They support point-in-time git-like **snapshots** so users can mark safe restore points before destructive operations (e.g., "before-training"). Multiple concurrent sandbox runs can mount the same volume simultaneously for read-only sharing (e.g., a shared dataset volume across a fine-tuning swarm). Volumes are catalogued in the existing `tag.sqlite3` WAL-mode database via a new `sandbox_volumes` table, enabling full history, quota enforcement, and CLI inspection.

The design is intentionally backend-agnostic. For the Docker backend, volumes become Docker bind mounts or named Docker volumes. For the E2B backend, volume contents are synchronized to the sandbox filesystem before execution and retrieved after — enabling the same volume semantics on cloud micro-VM sandboxes that cannot directly access host paths. For the Modal backend, volume contents are synchronized via Modal's own Volume API when available. The restricted subprocess backend treats volumes as simple host directory binds with a path-safety check.

This feature closes the gap between TAG's ephemeral sandbox safety model and the practical needs of multi-step agent workflows, ML experiment pipelines, and shared-state concurrent agent swarms. It does so without weakening the host isolation guarantees established in PRD-028: volumes are explicitly opted-in named resources, subject to the same credential-pattern blocking, quota limits, and audit logging as the rest of the sandbox subsystem.

---

## 2. Problem Statement

### 2.1 Ephemeral Isolation Destroys Agent-Generated Artifacts

TAG's sandbox design mandates ephemeral containers: every run starts clean and leaves nothing on the host unless the user explicitly copies output. This is the correct security posture for untrusted code. However, agent workflows often span multiple sandbox invocations where the output of run N is the input to run N+1. A fine-tuning pipeline might: (1) preprocess data in run 1, (2) fine-tune in run 2 for 6 hours, (3) evaluate in run 3, (4) deploy in run 4. With the current ephemeral model, none of these runs can share state — the user must manually intervene after each step to transfer artifacts. This is not acceptable for autonomous agent workflows dispatched through `queue_worker.py` without human supervision.

### 2.2 No Shared State for Concurrent Sandbox Swarms

PRD-023 (multi-agent swarm context routing) and PRD-082 (multi-agent team primitives) enable concurrent agent swarms where multiple TAG agents work in parallel. In practice these swarms frequently need to read from shared data (a common embedding index, a dataset shard map, a compiled library) while writing to isolated per-agent outputs. The current sandbox model provides no primitive for "one read-only dataset volume shared across N containers simultaneously." Users work around this by duplicating large datasets into each container's ephemeral filesystem — wasting time, bandwidth, and disk space for every run.

### 2.3 No Reproducibility or Rollback at the Filesystem Level

ML experiments and agent research workflows require the ability to roll back to a known-good filesystem state before attempting a destructive operation (e.g., a fine-tuning run that corrupts the model weights due to a bug in the training script). Today, users manually `cp -r` directories before risky sandbox runs — a manual, error-prone process with no integration into TAG's CLI or audit trail. Git handles code rollback elegantly with commits and branches; there is no analogous primitive for sandbox filesystem state in TAG.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Provide named persistent volumes that survive sandbox termination and are mounted into subsequent runs via `--volume name:/path` flag on `tag sandbox run`. |
| G2 | Implement git-like point-in-time snapshots (`tag sandbox volume snapshot`) that capture the full volume state and allow restore, enabling rollback before destructive operations. |
| G3 | Support concurrent read-only mounts of the same volume across multiple simultaneous sandbox runs, enabling shared dataset/cache volumes for swarm workflows. |
| G4 | Track all volumes and snapshots in `sandbox_volumes` and `sandbox_volume_snapshots` SQLite tables following the existing `open_db` / WAL-mode pattern. |
| G5 | Enforce per-volume size quotas at creation time; block writes that would exceed the declared `--size` limit. |
| G6 | Block mounting of volumes containing paths matching the credential-pattern blocklist from PRD-028 security model; emit an explicit error on blocked mounts. |
| G7 | Emit OTEL spans for all volume lifecycle operations (create, mount, snapshot, restore, delete) integrating with PRD-013 tracing. |
| G8 | Support all three primary sandbox backends: Docker (bind mount), E2B (pre/post sync), Modal (Modal.Volume API or rsync fallback). |
| G9 | Provide `--json` output on all `tag sandbox volume` subcommands for scripting and CI pipeline integration. |
| G10 | Zero new mandatory dependencies: volumes use the host filesystem + existing SQLite; backend-specific volume APIs are lazy-imported optional extras. |

---

## 4. Non-Goals

| ID | Non-Goal |
|-----|----------|
| NG1 | **Networked distributed storage** — volumes are host-local directories. NFS, S3-backed volumes, and distributed filesystems (Ceph, GlusterFS) are out of scope for v1. |
| NG2 | **Block device or FUSE mounts** — volumes are bind-mounted host directories, not block devices, encrypted filesystems, or FUSE-based virtual filesystems. |
| NG3 | **Cross-machine volume replication** — volumes are not synced between TAG installations on different machines. Cloud backup of volume contents is left to the user. |
| NG4 | **Fine-grained per-file access control inside volumes** — the sandbox process inside a container gets read/write access to the entire mount point. Per-file ACLs within a volume are not enforced by TAG. |
| NG5 | **Automatic volume garbage collection** — v1 does not auto-delete volumes older than N days or volumes that exceed disk pressure thresholds. This is deferred to a follow-on PRD. |
| NG6 | **Kubernetes PersistentVolumeClaims** — TAG does not generate or manage K8s PVCs. This feature targets local Docker and cloud sandbox providers, not K8s cluster workloads. |
| NG7 | **Encryption at rest** — volume contents on the host filesystem are stored in plaintext. Users relying on full-disk encryption at the OS level are responsible for that layer. |
| NG8 | **Content-addressed deduplication between snapshots** — snapshots are full directory copies (via `shutil.copytree`). Delta-compressed or content-addressed snapshot storage (like git's packfile format) is deferred. |

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Volume create latency (empty, no pre-population) | < 100 ms | `time tag sandbox volume create` |
| Volume mount overhead per sandbox run (Docker bind mount path) | < 50 ms added to container startup | `sandbox_runs.completed_at - sandbox_runs.created_at` delta vs. non-mounted run |
| Snapshot create throughput | >= 500 MB/s on local NVMe (SSD) | `tag sandbox volume snapshot` on a 1 GB volume |
| Snapshot restore latency (1 GB volume) | < 3 seconds | `tag sandbox volume restore` wall time |
| SQLite write per volume operation | < 5 ms (WAL mode) | Instrumented with OTEL spans |
| Concurrent read-only mounts, correctness | Zero data corruption across 16 simultaneous readers | Integration test: 16 concurrent `tag sandbox run` with same read-only volume |
| Credential-path blocking coverage | 100% of PRD-028 blocked patterns rejected | Unit test suite over `BLOCKED_VOLUME_PATTERNS` |
| CLI `--json` output schema stability | Zero breaking schema changes in v1 | JSON schema snapshot test |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | ML engineer | create a named volume `my-data`, mount it into a preprocessing sandbox, and then mount the same volume into a fine-tuning sandbox | my preprocessed dataset is available in the next run without manual data transfer |
| U2 | Agent workflow author | take a snapshot named `before-training` before starting a fine-tuning run | I can restore the volume to a clean state if the training script corrupts the data |
| U3 | Swarm orchestrator | mount a shared `embeddings-cache` volume as read-only across 8 concurrent agent sandboxes | all agents share the same embedding index without duplicating 20 GB per container |
| U4 | DevOps engineer | run `tag sandbox volume list --json` in a CI step | I can assert that specific named volumes exist and have the expected sizes before dispatching agent jobs |
| U5 | Developer | run `tag sandbox volume create deps-cache --size 5G` once and then reference it in all sandbox runs for the project | pip and npm dependency downloads are cached across runs, reducing install time from 3 minutes to 10 seconds |
| U6 | Security engineer | observe that mounting a volume path containing `*.env` files is blocked with a clear error message | I know that the sandbox credential-path protections from PRD-028 extend to the volume subsystem |
| U7 | Agent pipeline author | inspect `tag sandbox volume inspect my-data --json` | I can see the volume's current size, creation date, snapshot count, and last-mount time to verify pipeline state before proceeding |
| U8 | Developer | run `tag sandbox volume snapshot my-data --name v1.0` and later `tag sandbox volume restore my-data --snapshot v1.0` | I can maintain multiple named experiment checkpoints and reproduce any past state exactly |
| U9 | Queue worker author | reference a named volume in a queue job YAML (`volumes: [my-data:/workspace/data]`) | agent jobs dispatched via `queue_worker.py` automatically receive persistent data mounts without code changes |
| U10 | Developer | run `tag sandbox volume delete old-volume --purge-snapshots` | I can reclaim disk space from volumes and all their associated snapshots in a single command |

---

## 7. Proposed CLI Surface

All volume management subcommands live under `tag sandbox volume`. The `--volume` flag on `tag sandbox run` is extended to accept named TAG volumes in addition to existing host-path syntax.

### 7.1 `tag sandbox volume create`

Create a new named persistent volume.

```
tag sandbox volume create <name> \
  [--size <limit>] \
  [--label <key=value>...] \
  [--from-snapshot <volume>:<snapshot>] \
  [--json]
```

**Options:**
- `<name>`: Required. Volume name. Must match `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`.
- `--size <limit>`: Soft quota. Accepts human-readable sizes: `10G`, `500M`, `2T`. Stored as bytes in SQLite. Writes that would exceed this limit produce a warning; hard enforcement requires `--hard-quota` (see FR-07). Default: `10G`.
- `--label <key=value>`: Arbitrary key-value metadata stored in `sandbox_volumes.labels_json`. Repeatable. Example: `--label project=my-ml-experiment --label owner=alice`.
- `--from-snapshot <volume>:<snapshot>`: Clone an existing snapshot as the starting state of the new volume (full copy). Errors if the source snapshot does not exist.
- `--json`: Print the created volume record as JSON to stdout.

**Output (default):**
```
Volume created: my-data
  Path:     /Users/alice/.tag/volumes/my-data/
  Size:     0 B / 10.0 GB quota
  Created:  2026-06-17T10:23:01Z
  ID:       vol_01j3k...
```

**Output (`--json`):**
```json
{
  "id": "vol_01j3kx5m2p",
  "name": "my-data",
  "path": "/Users/alice/.tag/volumes/my-data/",
  "size_bytes": 0,
  "quota_bytes": 10737418240,
  "labels": {"project": "my-ml-experiment"},
  "created_at": "2026-06-17T10:23:01Z",
  "snapshot_count": 0
}
```

---

### 7.2 `tag sandbox run` — extended `--volume` flag

The existing `tag sandbox run` command gains support for named TAG volumes via an extended `--volume` syntax.

```
tag sandbox run \
  --code "<code>" \
  --volume my-data:/workspace/data \
  --volume deps-cache:/root/.cache:ro \
  [--volume /host/path:/container/path] \
  [--backend docker|e2b|modal|restricted] \
  [--image <image>]
```

**Volume mount syntax:** `<source>:<target>[:<options>]`
- `<source>`: Either a TAG volume name (no `/` prefix) or an absolute host path (`/` prefix). Host paths are subject to the PRD-028 credential-pattern blocklist.
- `<target>`: Absolute path inside the sandbox where the volume is mounted.
- `<options>`: Optional comma-separated mount options. Supported: `ro` (read-only), `rw` (read-write, default).

**Examples:**
```bash
# Named volume, read-write
tag sandbox run --code "python train.py" --volume my-data:/workspace/data

# Named volume, read-only (safe for shared dataset access)
tag sandbox run --code "python eval.py" --volume embeddings-cache:/data:ro

# Mix of named volume and host path
tag sandbox run \
  --code "bash build.sh" \
  --volume deps-cache:/root/.cache \
  --volume /tmp/myproject:/src:ro

# Multiple named volumes
tag sandbox run \
  --code "python pipeline.py" \
  --volume raw-data:/data/raw:ro \
  --volume processed-data:/data/processed:rw \
  --volume model-checkpoints:/models
```

**Mount resolution:** When a `--volume` source is a recognized TAG volume name (found in `sandbox_volumes` table with `status = 'active'`), TAG resolves it to `~/.tag/volumes/<name>/` and passes the resolved host path to the backend. This resolution happens before any backend-specific mount logic.

---

### 7.3 `tag sandbox volume list`

List all named volumes.

```
tag sandbox volume list \
  [--label <key=value>...] \
  [--sort name|size|created|last_used] \
  [--json]
```

**Output (default):**
```
NAME             SIZE        QUOTA    SNAPSHOTS  LAST USED              STATUS
my-data          4.2 GB      10.0 GB  3          2026-06-17T09:10:00Z   active
deps-cache       890 MB      5.0 GB   1          2026-06-17T08:55:00Z   active
embeddings-cache 18.3 GB     20.0 GB  0          2026-06-17T07:00:00Z   active
old-experiment   2.1 GB      10.0 GB  5          2026-06-01T12:00:00Z   active
```

**Output (`--json`):** Array of volume objects matching the `--json` schema from `volume create`.

---

### 7.4 `tag sandbox volume inspect`

Show full details for a single volume including all snapshots.

```
tag sandbox volume inspect <name> [--json]
```

**Output (default):**
```
Volume: my-data
  ID:           vol_01j3kx5m2p
  Path:         /Users/alice/.tag/volumes/my-data/
  Size:         4.2 GB / 10.0 GB quota (42%)
  Created:      2026-06-17T10:23:01Z
  Last mounted: 2026-06-17T09:10:00Z  (sandbox run run_abc123)
  Labels:       project=my-ml-experiment, owner=alice
  Mount count:  7

  Snapshots (3):
  NAME             SIZE        CREATED                  DESCRIPTION
  before-training  4.0 GB      2026-06-16T22:00:00Z     Before fine-tuning run 3
  v1.0             3.5 GB      2026-06-15T18:30:00Z     First clean checkpoint
  initial          100 MB      2026-06-14T10:00:00Z     Raw dataset only
```

---

### 7.5 `tag sandbox volume snapshot`

Create a point-in-time snapshot of a volume.

```
tag sandbox volume snapshot <volume-name> \
  --name <snapshot-name> \
  [--description <text>] \
  [--json]
```

**Options:**
- `<volume-name>`: Required. Name of an existing active volume.
- `--name <snapshot-name>`: Required. Unique name for the snapshot within this volume. Must match `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`.
- `--description <text>`: Optional human-readable description (stored in `sandbox_volume_snapshots.description`).
- `--json`: Output snapshot record as JSON.

**Behavior:** Performs a full `shutil.copytree` of the volume directory to `~/.tag/volumes/.snapshots/<volume-name>/<snapshot-name>/`. Snapshot creation is **atomic**: written to a temp directory first, then renamed into place. Records the snapshot in `sandbox_volume_snapshots` with `content_hash` (SHA-256 of sorted file paths + sizes as a lightweight fingerprint).

**Output:**
```
Snapshot created: my-data:before-training
  Path:     /Users/alice/.tag/volumes/.snapshots/my-data/before-training/
  Size:     4.0 GB
  Hash:     sha256:a3f2c1...
  Created:  2026-06-17T10:30:00Z
```

---

### 7.6 `tag sandbox volume restore`

Restore a volume to a named snapshot state.

```
tag sandbox volume restore <volume-name> \
  --snapshot <snapshot-name> \
  [--confirm] \
  [--json]
```

**Options:**
- `--snapshot <snapshot-name>`: Required. Snapshot to restore from.
- `--confirm`: Required flag (safety gate). Without it, restore prints the plan and exits with a warning. With it, the restore proceeds. This prevents accidental data loss from scripting errors.

**Behavior:** Atomically replaces the volume directory contents by:
1. Automatically creating a `pre-restore-<timestamp>` snapshot of current state (so restore is itself reversible).
2. Copying snapshot contents to a temp directory.
3. Atomically swapping via `os.rename()` / directory replacement.
4. Updating `sandbox_volumes.size_bytes` and `last_modified_at`.

**Output:**
```
Restore plan: my-data → before-training
  Current state will be preserved as snapshot: pre-restore-20260617T103200Z
  Volume will be reset to state from: 2026-06-16T22:00:00Z (4.0 GB)

  Run with --confirm to proceed.

(with --confirm)
✓ Auto-snapshot created: pre-restore-20260617T103200Z
✓ Volume restored to: before-training
  Path: /Users/alice/.tag/volumes/my-data/
  Size: 4.0 GB
```

---

### 7.7 `tag sandbox volume delete`

Delete a named volume and optionally its snapshots.

```
tag sandbox volume delete <name> \
  [--purge-snapshots] \
  [--force] \
  [--json]
```

**Options:**
- `--purge-snapshots`: Also delete all snapshots associated with this volume. Without this flag, deletion fails if snapshots exist (safety gate).
- `--force`: Skip confirmation prompt. Use in CI/scripts.

**Safety:** Deletion is blocked if any sandbox run with `status = 'running'` currently has this volume mounted (checked via `sandbox_run_mounts` join). Blocked mounts must be terminated first.

---

### 7.8 `tag sandbox volume snapshot list`

List snapshots for a volume.

```
tag sandbox volume snapshot list <volume-name> [--json]
```

**Output:**
```
SNAPSHOT             SIZE      CREATED                  DESCRIPTION
before-training      4.0 GB    2026-06-16T22:00:00Z     Before fine-tuning run 3
v1.0                 3.5 GB    2026-06-15T18:30:00Z     First clean checkpoint
initial              100 MB    2026-06-14T10:00:00Z     Raw dataset only
```

---

### 7.9 `tag sandbox volume snapshot delete`

Delete a named snapshot.

```
tag sandbox volume snapshot delete <volume-name>:<snapshot-name> [--force]
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag sandbox volume create <name>` creates a directory at `~/.tag/volumes/<name>/`, inserts a row in `sandbox_volumes` with `status='active'`, and returns exit code 0. Names not matching `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$` are rejected with exit code 1 and a descriptive error before any filesystem operations. |
| FR-02 | `tag sandbox run --volume <name>:<target>` resolves `<name>` against `sandbox_volumes` (by `name` and `status='active'`). If found, the host path `~/.tag/volumes/<name>/` is used as the bind-mount source. If not found, the command errors with: `"Unknown volume '<name>'. Create it first with: tag sandbox volume create <name>"`. |
| FR-03 | `--volume <name>:<target>:ro` mounts the volume read-only. For the Docker backend, this passes `--mount type=bind,src=<path>,dst=<target>,readonly` to `docker run`. For E2B, pre-sync uploads but post-sync is skipped. For restricted subprocess, the directory is made accessible read-only via OS-level `chmod -w` applied to a temporary copy (or via the bind-mount if available). |
| FR-04 | Multiple `--volume` flags are supported in a single `tag sandbox run` invocation. Each is resolved and mounted independently. Duplicate `<target>` paths in the same invocation are rejected with exit code 1. |
| FR-05 | `tag sandbox volume snapshot <name> --name <snap>` performs a full `shutil.copytree` to `~/.tag/volumes/.snapshots/<name>/<snap>/`, inserts a row in `sandbox_volume_snapshots`, and records `content_hash` (SHA-256 of `|`-joined `<relative_path>:<size_bytes>` for all files, sorted). Creates `~/.tag/volumes/.snapshots/` if absent. |
| FR-06 | Snapshot names must be unique per volume. Attempting to create a snapshot with a name that already exists for that volume returns exit code 1 with the message: `"Snapshot '<snap>' already exists for volume '<name>'. Use a different name or delete the existing snapshot first."` |
| FR-07 | Per-volume `quota_bytes` is enforced as a **soft** quota: `tag sandbox volume create --size 10G` records `quota_bytes = 10737418240`. A pre-flight size check runs before `tag sandbox run` mounts the volume: if `current_size_bytes >= quota_bytes`, the run is blocked with exit code 1 and message: `"Volume '<name>' is at or above its quota (X GB / Y GB). Free space or increase the quota before running."` The check uses `sum(f.stat().st_size for f in Path(vol_path).rglob('*') if f.is_file())`. |
| FR-08 | Mounting a volume whose host path contains any file or directory matching the PRD-028 `BLOCKED_VOLUME_PATTERNS` list is blocked. The check scans one level of the volume root directory (not recursively, for performance). Blocked patterns include: `*.env`, `*.key`, `*.pem`, `*.p12`, `*.pfx`, `*secret*`, `*credential*`, `.ssh`, `.aws`, `.gnupg`. Exit code 1 with message: `"Mount blocked: volume '<name>' contains path matching blocked pattern '<pattern>'. Remove sensitive files before mounting."` |
| FR-09 | `tag sandbox volume restore <name> --snapshot <snap> --confirm` automatically creates a `pre-restore-<ISO8601-UTC>` snapshot of current volume state before overwriting it. The restore copies snapshot contents to a temp directory `~/.tag/volumes/<name>.restore_tmp/` and atomically replaces the live directory via `shutil.move`. If interrupted mid-copy, the temp directory is cleaned up and no data is lost. |
| FR-10 | `tag sandbox volume delete <name> --force --purge-snapshots` removes the volume directory, all snapshot directories under `~/.tag/volumes/.snapshots/<name>/`, sets `sandbox_volumes.status = 'deleted'` and `deleted_at = <now>` (soft delete), and removes all `sandbox_volume_snapshots` rows for this volume. Physical directory deletion is permanent; the SQLite row is soft-deleted for audit purposes. |
| FR-11 | Concurrent read-only mounts of the same volume across multiple simultaneous `tag sandbox run` invocations are permitted without locking. The `sandbox_run_mounts` table records each active mount. Read-write concurrent mounts of the same volume by more than one run produce a WARNING (not an error) on stderr: `"Warning: volume '<name>' is mounted read-write by run <run_id>. Concurrent writes may cause data corruption."` |
| FR-12 | Every volume lifecycle event (create, mount-start, mount-end, snapshot, restore, delete) emits an OTEL span via `tracing.py`. Span attributes follow PRD-013 conventions: `tag.volume.name`, `tag.volume.id`, `tag.volume.operation`, `tag.volume.size_bytes`. |
| FR-13 | All `tag sandbox volume` subcommands produce `--json` output as a stable JSON schema (documented in Section 9). The schema version is included in every JSON response as `"schema_version": "1"`. |
| FR-14 | `tag sandbox volume list` reads only from SQLite (`sandbox_volumes` table) and does not scan the filesystem. `size_bytes` is the last-recorded value from the most recent mount-end or snapshot event. A `--refresh-sizes` flag triggers a live filesystem scan and updates `sandbox_volumes.size_bytes`. |
| FR-15 | The `queue_worker.py` job dispatcher recognizes a `volumes` key in job YAML (list of `"<name>:<target>[:<options>]"` strings) and passes it as the `volume_mounts` argument to `run_in_sandbox()`. This enables persistent volumes in fully autonomous queue-dispatched agent jobs. |
| FR-16 | Volume names are scoped to the local TAG installation (`~/.tag/volumes/`). No two active volumes may share the same name; the `sandbox_volumes` table has a `UNIQUE` constraint on `(name, status)` filtered to `status='active'`. Attempting to create a duplicate name returns exit code 1. |
| FR-17 | `tag sandbox volume snapshot list <name>` queries `sandbox_volume_snapshots` and lists all snapshots for the volume in reverse chronological order (newest first). |
| FR-18 | `tag sandbox volume inspect <name>` returns a combined view joining `sandbox_volumes` and `sandbox_volume_snapshots`. If the volume name does not exist in `sandbox_volumes` (or `status = 'deleted'`), exit code 1 with: `"Volume '<name>' not found."` |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Mount latency:** For the Docker backend, the end-to-end overhead of resolving a named volume and passing it as a bind mount must add less than 50 ms to container startup time. Measured as the delta between `sandbox_runs.completed_at - sandbox_runs.created_at` for equivalent runs with and without a volume mount. |
| NFR-02 | **Snapshot throughput:** Snapshot creation (FR-05) must achieve >= 500 MB/s on NVMe SSD for the `shutil.copytree` step. This is achievable since `shutil.copytree` uses `os.sendfile()` on Linux and `fcopyfile()` on macOS. Snapshots of volumes > 1 GB must display a progress bar via Rich `Progress` to avoid appearing frozen. |
| NFR-03 | **SQLite WAL concurrency:** All reads and writes to `sandbox_volumes` and `sandbox_volume_snapshots` use `open_db()` with WAL mode enabled (consistent with existing TAG SQLite usage). Concurrent readers are never blocked by a single writer. |
| NFR-04 | **Disk safety on interrupted snapshot:** If snapshot `copytree` is interrupted (SIGINT, process kill, power loss), the incomplete snapshot directory `~/.tag/volumes/.snapshots/<name>/<snap>.tmp/` is left in place but is never renamed to the final path. On next startup, TAG's `cmd_doctor` check identifies and reports orphaned `.tmp` snapshot directories. |
| NFR-05 | **No mandatory new dependencies:** Volume operations use only Python standard library (`shutil`, `hashlib`, `pathlib`, `os`, `sqlite3`) and existing TAG dependencies. Backend-specific volume APIs (Modal.Volume, E2B filesystem sync) are lazy-imported in `try/except ImportError` blocks. |
| NFR-06 | **Backward compatibility:** Adding `--volume` to `tag sandbox run` is purely additive. Existing invocations without `--volume` behave identically to pre-PRD-096 behavior. Existing `sandbox_runs` table rows are unaffected; only new `sandbox_run_mounts` rows are added. |
| NFR-07 | **Audit trail:** Every mount event appends a JSON line to `~/.tag/runtime/sandbox-audit.jsonl` (existing audit log from PRD-028) with fields: `event="volume_mount"`, `volume_id`, `volume_name`, `target_path`, `mode` (`ro`/`rw`), `run_id`, `backend`, `timestamp`. |
| NFR-08 | **Cross-platform paths:** Volume host paths use `pathlib.Path` throughout. On macOS, `~/.tag/volumes/` resolves to `/Users/<user>/.tag/volumes/`. On Linux, `/home/<user>/.tag/volumes/`. Windows is not a supported platform for v1 (consistent with PRD-028). |
| NFR-09 | **Quota check performance:** The pre-flight quota check (FR-07) must complete in < 200 ms for volumes up to 50 GB. Use `os.scandir()` with `st_size` from `DirEntry.stat()` (single syscall per entry, no `lstat` overhead) rather than `os.walk()`. |

---

## 10. Technical Design

### 10.1 New and Modified Files

| File | Change |
|------|--------|
| `src/tag/sandbox.py` | Add `ensure_volume_schema()`, `VolumeMount`, `SandboxVolume`, `VolumeSnapshot` dataclasses; add `create_volume()`, `delete_volume()`, `snapshot_volume()`, `restore_volume()`, `list_volumes()`, `inspect_volume()`, `resolve_mounts()`, `_check_credential_patterns()`, `_check_quota()`, `_compute_dir_size()`, `_content_hash()` functions; extend `run_in_sandbox()` to accept `volume_mounts: list[VolumeMount]`. |
| `src/tag/controller.py` | Add `cmd_sandbox_volume_create`, `cmd_sandbox_volume_list`, `cmd_sandbox_volume_inspect`, `cmd_sandbox_volume_snapshot`, `cmd_sandbox_volume_restore`, `cmd_sandbox_volume_delete`, `cmd_sandbox_volume_snapshot_list`, `cmd_sandbox_volume_snapshot_delete` functions; wire into the `tag sandbox volume` subcommand group. |
| `src/tag/queue_worker.py` | Extend job YAML parsing to recognize `volumes:` list field; pass resolved `VolumeMount` list to `run_in_sandbox()`. |
| `~/.tag/runtime/tag.sqlite3` | Add `sandbox_volumes`, `sandbox_volume_snapshots`, `sandbox_run_mounts` tables via `ensure_volume_schema()`. |

### 10.2 SQLite DDL

```sql
-- sandbox_volumes: registry of named persistent volumes
CREATE TABLE IF NOT EXISTS sandbox_volumes (
    id              TEXT PRIMARY KEY,               -- 'vol_' + ulid()
    name            TEXT NOT NULL,                  -- user-facing name, e.g. 'my-data'
    path            TEXT NOT NULL,                  -- absolute host path, e.g. '/Users/alice/.tag/volumes/my-data/'
    quota_bytes     INTEGER NOT NULL DEFAULT 10737418240, -- 10 GB default
    size_bytes      INTEGER NOT NULL DEFAULT 0,    -- last measured size; updated on mount-end and snapshot
    status          TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active', 'deleted')),
    labels_json     TEXT NOT NULL DEFAULT '{}',    -- JSON object of user-defined key-value labels
    created_at      TEXT NOT NULL,                 -- ISO8601 UTC
    last_mounted_at TEXT,                          -- ISO8601 UTC of most recent mount start
    last_modified_at TEXT,                         -- ISO8601 UTC of most recent write-mount end or restore
    deleted_at      TEXT,                          -- ISO8601 UTC; non-null means soft-deleted
    mount_count     INTEGER NOT NULL DEFAULT 0     -- total number of times this volume has been mounted
);
-- Enforce uniqueness of active volume names
CREATE UNIQUE INDEX IF NOT EXISTS idx_sv_name_active
    ON sandbox_volumes(name)
    WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_sv_status ON sandbox_volumes(status, created_at);

-- sandbox_volume_snapshots: point-in-time snapshots
CREATE TABLE IF NOT EXISTS sandbox_volume_snapshots (
    id              TEXT PRIMARY KEY,               -- 'snap_' + ulid()
    volume_id       TEXT NOT NULL REFERENCES sandbox_volumes(id),
    volume_name     TEXT NOT NULL,                  -- denormalized for query convenience
    name            TEXT NOT NULL,                  -- user-facing snapshot name, unique per volume
    path            TEXT NOT NULL,                  -- absolute path to snapshot directory
    size_bytes      INTEGER NOT NULL DEFAULT 0,
    content_hash    TEXT,                           -- SHA-256 fingerprint (see FR-05)
    description     TEXT,                           -- optional human description
    created_at      TEXT NOT NULL,                  -- ISO8601 UTC
    auto_created    INTEGER NOT NULL DEFAULT 0      -- 1 if auto-created by restore (pre-restore safety snapshot)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_svs_volume_name
    ON sandbox_volume_snapshots(volume_id, name);
CREATE INDEX IF NOT EXISTS idx_svs_volume_id ON sandbox_volume_snapshots(volume_id, created_at DESC);

-- sandbox_run_mounts: join table recording which volumes are mounted by which sandbox runs
CREATE TABLE IF NOT EXISTS sandbox_run_mounts (
    id              TEXT PRIMARY KEY,               -- 'mnt_' + ulid()
    run_id          TEXT NOT NULL REFERENCES sandbox_runs(id),
    volume_id       TEXT NOT NULL REFERENCES sandbox_volumes(id),
    volume_name     TEXT NOT NULL,
    target_path     TEXT NOT NULL,                  -- container-side mount path
    mode            TEXT NOT NULL DEFAULT 'rw' CHECK(mode IN ('ro', 'rw')),
    mounted_at      TEXT NOT NULL,                  -- ISO8601 UTC
    unmounted_at    TEXT                            -- NULL while active; set on run completion
);
CREATE INDEX IF NOT EXISTS idx_srm_run_id ON sandbox_run_mounts(run_id);
CREATE INDEX IF NOT EXISTS idx_srm_volume_active ON sandbox_run_mounts(volume_id, unmounted_at)
    WHERE unmounted_at IS NULL;
```

### 10.3 Core Dataclasses

```python
# src/tag/sandbox.py (additions)

from __future__ import annotations
import dataclasses
import hashlib
import os
import shutil
import sqlite3
import uuid
from pathlib import Path
from typing import Optional


@dataclasses.dataclass
class VolumeMount:
    """Parsed representation of a --volume flag value."""
    source: str          # TAG volume name (no '/') or absolute host path
    target: str          # Absolute path inside the container
    mode: str = "rw"     # 'ro' or 'rw'
    is_named: bool = False   # True if source is a TAG volume name
    resolved_host_path: Optional[Path] = None  # set after resolve_mounts()

    @classmethod
    def parse(cls, spec: str) -> "VolumeMount":
        """Parse 'source:target[:options]' into a VolumeMount.

        Examples:
            'my-data:/workspace/data'       -> named, rw
            'my-data:/workspace/data:ro'    -> named, ro
            '/host/path:/container/path'    -> host path, rw
        """
        parts = spec.split(":")
        if len(parts) < 2:
            raise ValueError(
                f"Invalid volume spec '{spec}': must be 'source:target' or "
                "'source:target:options'"
            )
        source = parts[0]
        target = parts[1]
        mode = "rw"
        if len(parts) >= 3:
            opts = parts[2].split(",")
            if "ro" in opts:
                mode = "ro"
        is_named = not source.startswith("/")
        return cls(source=source, target=target, mode=mode, is_named=is_named)


@dataclasses.dataclass
class SandboxVolume:
    """A named persistent volume record from sandbox_volumes."""
    id: str
    name: str
    path: Path
    quota_bytes: int
    size_bytes: int
    status: str
    labels: dict
    created_at: str
    last_mounted_at: Optional[str]
    last_modified_at: Optional[str]
    deleted_at: Optional[str]
    mount_count: int

    @property
    def quota_human(self) -> str:
        return _human_bytes(self.quota_bytes)

    @property
    def size_human(self) -> str:
        return _human_bytes(self.size_bytes)

    def to_dict(self) -> dict:
        return {
            "schema_version": "1",
            "id": self.id,
            "name": self.name,
            "path": str(self.path),
            "quota_bytes": self.quota_bytes,
            "size_bytes": self.size_bytes,
            "status": self.status,
            "labels": self.labels,
            "created_at": self.created_at,
            "last_mounted_at": self.last_mounted_at,
            "last_modified_at": self.last_modified_at,
            "mount_count": self.mount_count,
        }


@dataclasses.dataclass
class VolumeSnapshot:
    """A point-in-time snapshot of a volume."""
    id: str
    volume_id: str
    volume_name: str
    name: str
    path: Path
    size_bytes: int
    content_hash: Optional[str]
    description: Optional[str]
    created_at: str
    auto_created: bool

    def to_dict(self) -> dict:
        return {
            "schema_version": "1",
            "id": self.id,
            "volume_id": self.volume_id,
            "volume_name": self.volume_name,
            "name": self.name,
            "path": str(self.path),
            "size_bytes": self.size_bytes,
            "content_hash": self.content_hash,
            "description": self.description,
            "created_at": self.created_at,
            "auto_created": self.auto_created,
        }
```

### 10.4 Core Algorithms

#### Volume Name Validation

```python
import re

_VOLUME_NAME_RE = re.compile(r'^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$')
_SNAPSHOT_NAME_RE = re.compile(r'^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$')

# Blocked path patterns (credential protection, extends PRD-028 blocklist)
BLOCKED_VOLUME_PATTERNS = [
    "*.env", "*.key", "*.pem", "*.p12", "*.pfx",
    "*secret*", "*credential*", "*password*", "*passwd*",
    ".ssh", ".aws", ".gnupg", ".netrc",
    "id_rsa", "id_ed25519", "id_ecdsa",
]

def _validate_volume_name(name: str) -> None:
    if not _VOLUME_NAME_RE.match(name):
        raise ValueError(
            f"Invalid volume name '{name}'. Names must start with an alphanumeric "
            "character and contain only [a-zA-Z0-9_-], max 63 characters."
        )
```

#### Directory Size Computation (fast, NFR-09)

```python
def _compute_dir_size(path: Path) -> int:
    """Fast directory size using os.scandir() for O(1) stat calls per entry."""
    total = 0
    stack = [path]
    while stack:
        current = stack.pop()
        try:
            with os.scandir(current) as it:
                for entry in it:
                    try:
                        if entry.is_file(follow_symlinks=False):
                            total += entry.stat(follow_symlinks=False).st_size
                        elif entry.is_dir(follow_symlinks=False):
                            stack.append(Path(entry.path))
                    except (PermissionError, OSError):
                        continue
        except (PermissionError, OSError):
            continue
    return total
```

#### Content Hash for Snapshot Fingerprinting

```python
def _content_hash(path: Path) -> str:
    """Compute a lightweight fingerprint: SHA-256 of sorted 'relpath:size' pairs."""
    entries = []
    for f in sorted(path.rglob("*")):
        if f.is_file():
            try:
                rel = str(f.relative_to(path))
                size = f.stat().st_size
                entries.append(f"{rel}:{size}")
            except (OSError, ValueError):
                continue
    digest = hashlib.sha256("\n".join(entries).encode()).hexdigest()
    return f"sha256:{digest}"
```

#### Credential Pattern Check (FR-08)

```python
import fnmatch

def _check_credential_patterns(vol_path: Path) -> None:
    """Scan top level of volume dir for blocked credential patterns."""
    try:
        with os.scandir(vol_path) as it:
            for entry in it:
                for pattern in BLOCKED_VOLUME_PATTERNS:
                    if fnmatch.fnmatch(entry.name, pattern):
                        raise PermissionError(
                            f"Mount blocked: volume at '{vol_path}' contains "
                            f"'{entry.name}' matching blocked pattern '{pattern}'. "
                            "Remove sensitive files before mounting."
                        )
    except (FileNotFoundError, OSError):
        pass  # Empty or inaccessible volume — allow mount; sandbox will see empty dir
```

#### Atomic Snapshot Creation (FR-05, NFR-04)

```python
def _atomic_snapshot(src: Path, dest_parent: Path, snap_name: str) -> Path:
    """Copy src to dest_parent/snap_name atomically via a .tmp intermediate."""
    dest_parent.mkdir(parents=True, exist_ok=True)
    tmp_dest = dest_parent / f"{snap_name}.tmp"
    final_dest = dest_parent / snap_name

    if tmp_dest.exists():
        shutil.rmtree(tmp_dest)  # Clean orphaned tmp from prior interrupted run
    if final_dest.exists():
        raise FileExistsError(f"Snapshot '{snap_name}' already exists at {final_dest}")

    shutil.copytree(src, tmp_dest, symlinks=True)
    tmp_dest.rename(final_dest)  # atomic on POSIX when on same filesystem
    return final_dest
```

#### resolve_mounts() — Core Mount Resolution

```python
def resolve_mounts(
    conn: sqlite3.Connection,
    mounts: list[VolumeMount],
    *,
    volumes_base: Path,
) -> list[VolumeMount]:
    """Resolve named volume mounts to host paths; validate credential patterns and quota."""
    resolved = []
    seen_targets: set[str] = set()

    for m in mounts:
        if m.target in seen_targets:
            raise ValueError(
                f"Duplicate mount target '{m.target}': each container path may only "
                "be mounted once per sandbox run."
            )
        seen_targets.add(m.target)

        if m.is_named:
            row = conn.execute(
                "SELECT id, path, quota_bytes, size_bytes FROM sandbox_volumes "
                "WHERE name = ? AND status = 'active'",
                (m.source,),
            ).fetchone()
            if row is None:
                raise ValueError(
                    f"Unknown volume '{m.source}'. Create it first with: "
                    f"tag sandbox volume create {m.source}"
                )
            vol_id, vol_path_str, quota_bytes, size_bytes = row
            vol_path = Path(vol_path_str)

            # Credential pattern check
            _check_credential_patterns(vol_path)

            # Quota pre-flight (for rw mounts)
            if m.mode == "rw" and size_bytes >= quota_bytes:
                raise PermissionError(
                    f"Volume '{m.source}' is at or above its quota "
                    f"({_human_bytes(size_bytes)} / {_human_bytes(quota_bytes)}). "
                    "Free space or run: tag sandbox volume create "
                    f"{m.source} --size <larger>"
                )

            m = dataclasses.replace(m, resolved_host_path=vol_path)
        else:
            # Host path mount — apply PRD-028 host-path credential blocklist
            host_path = Path(m.source)
            _check_credential_patterns(host_path)
            m = dataclasses.replace(m, resolved_host_path=host_path)

        resolved.append(m)
    return resolved
```

### 10.5 Backend Integration

#### Docker Backend

`_run_docker()` in `sandbox.py` is extended to accept `volume_mounts: list[VolumeMount]`. Each resolved mount becomes a `--mount` argument:

```python
for vm in volume_mounts:
    readonly = ",readonly" if vm.mode == "ro" else ""
    docker_cmd += [
        "--mount",
        f"type=bind,src={vm.resolved_host_path},dst={vm.target}{readonly}",
    ]
```

#### E2B Backend

E2B micro-VMs cannot directly access host directories via bind mount. Instead, a pre/post sync pattern is used:

```python
# Pre-run: upload volume contents to E2B sandbox filesystem
async def _e2b_upload_volume(sbx, vm: VolumeMount) -> None:
    for local_file in vm.resolved_host_path.rglob("*"):
        if local_file.is_file():
            rel = local_file.relative_to(vm.resolved_host_path)
            remote_path = f"{vm.target}/{rel}"
            sbx.files.write(remote_path, local_file.read_bytes())

# Post-run (rw mounts only): download modified contents back to host
async def _e2b_download_volume(sbx, vm: VolumeMount) -> None:
    if vm.mode == "rw":
        # List remote files and download changed ones (by size comparison)
        remote_files = sbx.files.list(vm.target)
        for rf in remote_files:
            local_path = vm.resolved_host_path / rf.name
            content = sbx.files.read(rf.path)
            local_path.parent.mkdir(parents=True, exist_ok=True)
            local_path.write_bytes(content)
```

#### Modal Backend

When the Modal SDK is available and a `modal.Volume` with the same name exists, TAG uses it directly. Otherwise, falls back to the same pre/post rsync pattern as E2B:

```python
def _get_modal_volume(name: str):
    try:
        import modal
        return modal.Volume.from_name(name, create_if_missing=False)
    except Exception:
        return None  # Fall back to rsync-style sync
```

### 10.6 ULID Generation

```python
import time
import os

def _ulid() -> str:
    """Generate a ULID-like sortable unique ID."""
    ts = int(time.time() * 1000)
    rand = os.urandom(10).hex()
    return f"{ts:013x}{rand}"
```

### 10.7 Human-Readable Byte Formatting

```python
def _human_bytes(n: int) -> str:
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024 or unit == "TB":
            return f"{n:.1f} {unit}"
        n /= 1024
```

### 10.8 Integration with `queue_worker.py`

Queue job YAML gains a `volumes` key:

```yaml
# Example queue job with persistent volume mounts
id: fine-tune-job-001
type: sandbox
command: python train.py --epochs 10
image: pytorch/pytorch:2.3.0-cuda12.1-cudnn9-runtime
backend: docker
volumes:
  - raw-data:/data/raw:ro
  - model-checkpoints:/models:rw
  - pip-cache:/root/.cache/pip:rw
timeout: 7200
```

`queue_worker.py:_run_job()` parses `job.get("volumes", [])` into `list[VolumeMount]` and passes to `run_in_sandbox(conn, cmd, volume_mounts=mounts, ...)`.

---

## 11. Security Considerations

1. **Credential pattern blocking (FR-08):** Volume mounts undergo the same `BLOCKED_VOLUME_PATTERNS` check as host-path mounts in PRD-028. Patterns are applied to the top-level directory entries of the volume root. This provides a defense-in-depth layer: even if a user inadvertently puts `.env` in a volume, the mount is blocked before any sandbox process can read it.

2. **Read-only enforcement for concurrent swarm mounts:** When `--volume name:/path:ro` is specified, the Docker backend passes `readonly` to the bind-mount options. This is enforced at the container runtime level (Docker daemon), not just by TAG convention. An agent process inside the container cannot escalate to write access on a `ro` mount without a container-escape exploit.

3. **No host path traversal via symlinks in snapshots:** `_atomic_snapshot` uses `shutil.copytree(symlinks=True)`, which copies symlinks as symlinks rather than following them. This prevents a malicious volume from containing a symlink pointing to `/etc/passwd` that gets followed during snapshot, exfiltrating host data into the snapshot store.

4. **Quota enforcement prevents disk exhaustion:** The pre-flight quota check (FR-07) prevents agent-generated writes from filling the host disk. The soft-quota model means existing data is not deleted, but new sandbox runs mounting the over-quota volume are blocked until the user takes explicit action.

5. **Atomic snapshot write prevents TOCTOU:** The `.tmp` intermediate directory pattern in `_atomic_snapshot` prevents time-of-check/time-of-use races: a snapshot is either fully committed (renamed to final path) or entirely absent. There is no observable intermediate state that another process could read a corrupt partial snapshot from.

6. **Soft delete preserves audit trail:** `sandbox_volumes.status = 'deleted'` with `deleted_at` timestamp means deleted volumes are never fully purged from SQLite. Combined with the `sandbox_run_mounts` history, this enables post-incident forensics: which agent runs accessed which volumes, and when.

7. **Concurrent write-mount warning (FR-11):** Multiple simultaneous read-write mounts of the same volume are warned about (not silently allowed) because Docker bind mounts do not provide filesystem-level locking. Users who need concurrent safe writes should use separate volumes and merge results explicitly.

8. **No volume contents sent to LLM context:** Volume data is never automatically included in any agent prompt or LLM API call. Volumes are opaque binary directories as far as the TAG agent loop is concerned; the agent cannot enumerate volume contents unless the sandbox code explicitly reads and prints them.

9. **Snapshot directory isolation:** All snapshots are stored under `~/.tag/volumes/.snapshots/` (a dot-prefixed hidden directory), separate from live volume data under `~/.tag/volumes/<name>/`. This separation ensures that an agent process inside a sandbox container with `name` mounted at `/workspace/data` cannot enumerate or access the snapshot store, which is never mounted into sandboxes.

10. **Path canonicalization before pattern matching:** Before applying `BLOCKED_VOLUME_PATTERNS`, all paths are resolved via `Path.resolve()` to eliminate symlink traversal and `..` components that could bypass pattern matching. Example: `~/.tag/volumes/myvolume/../../../home/user/.ssh/` would resolve to `/home/user/.ssh/` and match the `.ssh` block pattern.

---

## 12. Testing Strategy

### 12.1 Unit Tests

```
tests/test_sandbox_volumes.py
```

| Test | Coverage |
|------|----------|
| `test_volume_name_validation_valid` | All valid name patterns pass `_validate_volume_name()` |
| `test_volume_name_validation_invalid` | Names with `/`, `..`, spaces, leading `-`, > 63 chars all raise `ValueError` |
| `test_snapshot_name_validation` | Dots and hyphens allowed in snapshot names; `..` rejected |
| `test_volume_mount_parse_named_rw` | `VolumeMount.parse("my-data:/workspace")` produces `is_named=True, mode='rw'` |
| `test_volume_mount_parse_named_ro` | `VolumeMount.parse("my-data:/workspace:ro")` produces `mode='ro'` |
| `test_volume_mount_parse_host_path` | `VolumeMount.parse("/tmp/foo:/bar")` produces `is_named=False` |
| `test_volume_mount_parse_duplicate_target` | `resolve_mounts` with two mounts to same target raises `ValueError` |
| `test_credential_pattern_block_env` | Volume dir containing `secrets.env` raises `PermissionError` |
| `test_credential_pattern_block_ssh` | Volume dir containing `.ssh/` raises `PermissionError` |
| `test_credential_pattern_allow_clean` | Volume dir with only `.txt` and `.py` files passes without error |
| `test_content_hash_deterministic` | Same directory structure always produces the same hash |
| `test_content_hash_differs_on_change` | Adding one byte to any file changes the hash |
| `test_compute_dir_size_accuracy` | `_compute_dir_size` returns exact byte count for a synthetic directory tree |
| `test_compute_dir_size_handles_permission_error` | Unreadable subdirectory is skipped without raising |
| `test_human_bytes_formatting` | 1024 → `"1.0 KB"`, 1073741824 → `"1.0 GB"`, 0 → `"0.0 B"` |
| `test_atomic_snapshot_success` | `_atomic_snapshot` creates final directory, removes tmp |
| `test_atomic_snapshot_no_partial_on_interrupt` | Monkeypatched `copytree` raises mid-copy; only `.tmp` exists, not final |
| `test_atomic_snapshot_raises_on_duplicate` | Second call with same `snap_name` raises `FileExistsError` |
| `test_quota_check_blocks_at_limit` | `resolve_mounts` with `size_bytes >= quota_bytes` raises `PermissionError` |
| `test_quota_check_allows_under_limit` | `resolve_mounts` with `size_bytes < quota_bytes` succeeds |

### 12.2 Integration Tests

```
tests/test_sandbox_volumes_integration.py
```

| Test | Setup | Assert |
|------|-------|--------|
| `test_create_and_list` | Call `create_volume(conn, "test-vol", quota_bytes=1GB)` | Row in `sandbox_volumes`; directory at `~/.tag/volumes/test-vol/` |
| `test_mount_in_docker_run` | Create volume, write file into it, run Docker sandbox reading the file | File content in sandbox stdout |
| `test_snapshot_and_restore` | Create volume, write file A, snapshot `v1`, write file B, restore `v1`, read dir | Only file A present after restore |
| `test_concurrent_ro_mounts` | Create volume with 1 MB data; 8 threads each run Docker sandbox with `:ro` | All 8 runs succeed; no data corruption |
| `test_queue_worker_volume_yaml` | Write queue job YAML with `volumes:` key; dispatch via `_run_job()` | Mounted volume data accessible inside job container |
| `test_delete_blocked_while_mounted` | Start long-running sandbox with volume mounted; attempt `delete_volume()` | `ValueError` raised; volume still exists |
| `test_soft_delete_preserves_audit` | Delete a volume; query `sandbox_volumes WHERE status='deleted'` | Row still exists with `deleted_at` set |
| `test_pre_restore_auto_snapshot` | Restore a volume; list snapshots | `pre-restore-*` snapshot created automatically |

### 12.3 Performance Tests

```
tests/test_sandbox_volumes_perf.py
```

| Test | Threshold |
|------|-----------|
| `test_snapshot_throughput_1gb` | `shutil.copytree` of 1 GB synthetic data completes in < 2 seconds on NVMe |
| `test_compute_dir_size_50gb` | `_compute_dir_size` on 50 GB (10,000 files × 5 MB) completes in < 200 ms |
| `test_sqlite_volume_write_latency` | `INSERT INTO sandbox_volumes` + `COMMIT` in WAL mode < 5 ms (p99 over 100 iterations) |
| `test_mount_resolution_latency` | `resolve_mounts()` for 4 named volumes < 10 ms (4 SQLite reads + 4 scandir calls) |

### 12.4 Security Tests

```
tests/test_sandbox_volume_security.py
```

| Test | Description |
|------|-------------|
| `test_symlink_not_followed_in_snapshot` | Volume contains symlink to `/etc/passwd`; snapshot does not copy `/etc/passwd` contents |
| `test_path_canonicalization_bypasses_dotdot` | `resolve_mounts` with `source="../../../etc"` raises `ValueError` (not a valid volume name due to regex) |
| `test_all_blocked_patterns_rejected` | For each pattern in `BLOCKED_VOLUME_PATTERNS`, a volume containing a matching file is blocked |
| `test_ro_mount_blocks_write_in_container` | Docker run with `:ro` volume; `echo foo > /mounted/file` inside container exits non-zero |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag sandbox volume create my-data --size 10G` creates `~/.tag/volumes/my-data/`, inserts a row in `sandbox_volumes` with `quota_bytes=10737418240`, and prints the volume path. | `ls ~/.tag/volumes/my-data/ && sqlite3 ~/.tag/runtime/tag.sqlite3 "SELECT name,quota_bytes FROM sandbox_volumes WHERE name='my-data'"` |
| AC-02 | `tag sandbox run --code "ls /data" --volume my-data:/data` lists the contents of `~/.tag/volumes/my-data/` inside the sandbox. | Run command; verify stdout contains expected filenames. |
| AC-03 | `tag sandbox run --code "touch /data/test.txt" --volume my-data:/data` creates `~/.tag/volumes/my-data/test.txt` on the host after the run completes. | `ls ~/.tag/volumes/my-data/test.txt` |
| AC-04 | `tag sandbox run --code "touch /data/x" --volume my-data:/data:ro` fails with a non-zero exit code from the container (permission denied writing to read-only mount). | Run command; assert exit code != 0; `ls ~/.tag/volumes/my-data/x` → file does not exist. |
| AC-05 | `tag sandbox volume snapshot my-data --name before-training` creates `~/.tag/volumes/.snapshots/my-data/before-training/` and inserts a row in `sandbox_volume_snapshots`. | `ls ~/.tag/volumes/.snapshots/my-data/before-training/ && sqlite3 ... "SELECT name FROM sandbox_volume_snapshots WHERE volume_name='my-data'"` |
| AC-06 | After writing a new file to `my-data`, `tag sandbox volume restore my-data --snapshot before-training --confirm` removes the new file and the volume matches the snapshot state. | File count before and after restore match the snapshot's file count. |
| AC-07 | `tag sandbox volume restore my-data --snapshot before-training --confirm` automatically creates a `pre-restore-*` snapshot before overwriting. | `tag sandbox volume snapshot list my-data` shows a `pre-restore-*` entry. |
| AC-08 | Attempting to mount a volume containing `secrets.env` produces exit code 1 and the message `"Mount blocked: volume 'X' contains 'secrets.env' matching blocked pattern '*.env'."` | Place `secrets.env` in volume dir; run `tag sandbox run --volume X:/data`; assert stderr. |
| AC-09 | `tag sandbox volume list --json` returns a JSON array where each element has `schema_version: "1"` and fields: `id`, `name`, `path`, `quota_bytes`, `size_bytes`, `status`, `created_at`. | `jq '.[].schema_version' <(tag sandbox volume list --json)` → all `"1"`. |
| AC-10 | 8 concurrent `tag sandbox run --volume shared-data:/data:ro` invocations all complete successfully with identical stdout, no errors, and no data in `shared-data` modified. | Run 8 processes in parallel via Python `multiprocessing`; assert all exit 0 and volume mtime unchanged. |
| AC-11 | `tag sandbox volume delete my-data --force --purge-snapshots` removes the host directory, all snapshot directories, and sets `sandbox_volumes.status='deleted'` but preserves the SQLite row. | `ls ~/.tag/volumes/my-data` → no such file; `sqlite3 ... "SELECT status,deleted_at FROM sandbox_volumes WHERE name='my-data'"` → `deleted|<timestamp>`. |
| AC-12 | A queue job YAML with `volumes: [my-data:/workspace:rw]` dispatched via `queue_worker._run_job()` mounts the named volume into the sandbox. | Write a job that creates a file at `/workspace/output.txt`; after job completion, verify `~/.tag/volumes/my-data/output.txt` exists on host. |
| AC-13 | `tag sandbox volume create my-data` (duplicate name) returns exit code 1 with message `"Volume 'my-data' already exists."` without creating a new directory or row. | Create volume twice; assert second call exit code 1. |
| AC-14 | Attempting to run `tag sandbox run --volume over-quota-vol:/data` when the volume's `size_bytes >= quota_bytes` returns exit code 1 with a quota exceeded message. | Manually set `size_bytes = quota_bytes` in SQLite; attempt mount; assert error. |

---

## 14. Dependencies

| Dependency | Type | Version / Constraint | Notes |
|------------|------|---------------------|-------|
| Python `shutil` | stdlib | >= 3.11 | `copytree`, `move`, `rmtree` — no new dependency |
| Python `hashlib` | stdlib | >= 3.11 | SHA-256 content hashing |
| Python `pathlib` | stdlib | >= 3.11 | All path operations |
| Python `os` (scandir) | stdlib | >= 3.5 | Fast directory size computation |
| Python `sqlite3` | stdlib | >= 3.11 | WAL-mode database, existing `open_db()` |
| Python `dataclasses` | stdlib | >= 3.7 | `VolumeMount`, `SandboxVolume`, `VolumeSnapshot` |
| Docker CLI / Engine API | optional | >= 20.10 | Bind mount support for Docker backend |
| `e2b` SDK | optional extra | >= 0.17.0 | E2B filesystem sync for cloud backend |
| `modal` SDK | optional extra | >= 0.63.0 | `modal.Volume.from_name()` for Modal backend |
| PRD-028 sandbox.py | internal | v1 (existing) | `run_in_sandbox()`, `ensure_schema()`, `BACKENDS` — extended by this PRD |
| PRD-013 tracing.py | internal | v1 (existing) | OTEL span emission for volume lifecycle events |
| PRD-034 security.py | internal | v1 (existing) | `BLOCKED_VOLUME_PATTERNS` — extended/referenced |
| PRD-012 budget.py | internal | v1 (existing) | Quota tracking hooks (future: disk quota → budget alerts) |
| PRD-020 ci.py | internal | v1 (existing) | `--json` output consumed by CI pipelines |
| `rich` | existing dep | >= 13.0 | Progress bar for large snapshot operations (NFR-02) |

---

## 15. Open Questions

| ID | Question | Owner | Resolution Deadline |
|----|----------|-------|---------------------|
| OQ-01 | Should the E2B pre/post sync use E2B's native file API (`sbx.files.write`) or `rsync` over SSH? E2B's file API has a ~1 MB/s throughput limit for large volumes; rsync requires the sandbox to have SSH accessible. Need to benchmark both approaches for 1 GB volumes. | Sandbox team | Before implementation start |
| OQ-02 | Should `quota_bytes` be a hard limit enforced inside the container (via `docker run --storage-opt size=10G`) or only a pre-flight soft check? Hard enforcement requires Docker daemon with `overlay2` storage driver and `dm.basesize` config — not available on all platforms. | Platform team | Sprint 1 |
| OQ-03 | Should snapshot storage be content-addressed (deduplicated between snapshots that share files) to save disk space? Full `copytree` is simple and correct but expensive for large volumes with small changes. A COW filesystem (btrfs `cp --reflink`, APFS clonefile) could make snapshots near-instant and near-zero-cost. macOS APFS supports `fcopyfile(COPYFILE_CLONE)` already available in Python 3.13's `shutil.copy2`. | Architecture | Sprint 2 |
| OQ-04 | Should volume names be namespaced by TAG profile (e.g., `coder::my-data`) to prevent accidental cross-profile data sharing in multi-profile installations? Or is global namespace simpler and more useful? | Product | Before CLI design is finalized |
| OQ-05 | When a Modal sandbox is used and a `modal.Volume` with the same name exists in the user's Modal workspace, should TAG automatically use it (providing true cloud persistence)? This requires `modal auth` credentials at volume-create time. The discovery UX needs design. | Integrations team | Sprint 2 |
| OQ-06 | Should `tag sandbox volume` support volume templates — pre-populated volumes created from a public URL (e.g., `tag sandbox volume create my-data --from-url s3://bucket/dataset.tar.gz`)? This would enable standard ML benchmark datasets to be instantiated in one command. | Product | Post-v1 |
| OQ-07 | What is the correct behavior when the host filesystem runs low on disk space (< 1 GB free) during a snapshot? Should TAG fail fast with a clear error, or attempt the snapshot and let `shutil.copytree` fail with `OSError: [Errno 28] No space left on device`? | Engineering | Sprint 1 |
| OQ-08 | Should `sandbox_run_mounts.unmounted_at` be set by a cleanup sweep (in case TAG is killed mid-run and the mount record is never closed) or only set synchronously at run completion? A startup sweep that closes stale mount records would fix orphaned entries. | Engineering | Sprint 1 |

---

## 16. Complexity and Timeline

**Total estimated effort:** 9–11 engineering days

### Phase 1 — Core Volume Lifecycle (Days 1–3)

- SQLite DDL: `sandbox_volumes`, `sandbox_volume_snapshots`, `sandbox_run_mounts` tables via `ensure_volume_schema()`.
- `SandboxVolume`, `VolumeSnapshot`, `VolumeMount` dataclasses.
- `create_volume()`, `delete_volume()`, `list_volumes()`, `inspect_volume()`.
- `_validate_volume_name()`, `_compute_dir_size()`, `_human_bytes()`.
- `cmd_sandbox_volume_create`, `cmd_sandbox_volume_list`, `cmd_sandbox_volume_delete` in `controller.py`.
- Unit tests for name validation, size computation, SQLite schema.

### Phase 2 — Snapshot Engine (Days 4–5)

- `_atomic_snapshot()`, `_content_hash()`, `snapshot_volume()`, `restore_volume()`.
- Auto-snapshot on restore (FR-09).
- `cmd_sandbox_volume_snapshot`, `cmd_sandbox_volume_restore`, `cmd_sandbox_volume_snapshot_list`, `cmd_sandbox_volume_snapshot_delete`.
- Unit tests for atomic snapshot, content hash, duplicate detection.
- Integration test: snapshot → modify → restore → verify.

### Phase 3 — Mount Integration (Days 6–8)

- `VolumeMount.parse()`, `resolve_mounts()`.
- `_check_credential_patterns()`, `_check_quota()`.
- Extend `_run_docker()` with `--mount` flags for resolved volumes.
- Extend `run_in_sandbox()` signature with `volume_mounts: list[VolumeMount]`.
- `sandbox_run_mounts` insert/update on run start/end.
- Extend `queue_worker.py` to parse `volumes:` YAML key.
- Integration tests: Docker read-write mount, Docker read-only mount, concurrent ro mounts, credential block, quota block.

### Phase 4 — Backend Extensions and Polish (Days 9–11)

- E2B pre/post sync (`_e2b_upload_volume`, `_e2b_download_volume`).
- Modal.Volume integration with rsync fallback.
- OTEL span emission for all volume lifecycle events (FR-12).
- `--json` output on all subcommands; JSON schema snapshot test.
- `cmd_sandbox_volume_inspect` (full detail view).
- Performance tests: snapshot throughput, dir-size speed, SQLite write latency.
- Security tests: symlink safety, path canonicalization, all blocked patterns.
- Rich progress bar for large snapshot/restore operations (NFR-02).
- `cmd_doctor` check for orphaned `.tmp` snapshot directories (NFR-04).

### Milestone: PR Ready for Review — Day 11

All acceptance criteria (AC-01 through AC-14) passing in CI. `tag sandbox volume --help` producing correct documentation. JSON schema stable and snapshot-tested.

