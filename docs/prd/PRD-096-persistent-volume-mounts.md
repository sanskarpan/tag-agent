# PRD-096: Persistent Volume Mounts Across Sandbox Runs (`tag sandbox volume`)
> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox + sandbox_volumes SQLite (modernc.org/sqlite)`
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

**Behavior:** Performs a full recursive directory copy of the volume to `~/.tag/volumes/.snapshots/<volume-name>/<snapshot-name>/`. Snapshot creation is **atomic**: written to a temp directory first, then renamed into place. Records the snapshot in `sandbox_volume_snapshots` with `content_hash` (SHA-256 of sorted file paths + sizes as a lightweight fingerprint).

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
| FR-03 | `--volume <name>:<target>:ro` mounts the volume read-only. For the Docker backend, this appends a `mount.Mount{Type: mount.TypeBind, Source, Target, ReadOnly: true}` to the container's host config via the docker/moby Go client. For E2B, pre-sync uploads but post-sync is skipped. For the restricted backend, the resolved host path is granted read-only access via a `go-landlock` path rule (read-only ruleset) rather than a writable one. |
| FR-04 | Multiple `--volume` flags are supported in a single `tag sandbox run` invocation. Each is resolved and mounted independently. Duplicate `<target>` paths in the same invocation are rejected with exit code 1. |
| FR-05 | `tag sandbox volume snapshot <name> --name <snap>` performs a full recursive directory copy (Go `filepath.WalkDir` + `os.MkdirAll`/`io.Copy`, recreating symlinks as symlinks) to `~/.tag/volumes/.snapshots/<name>/<snap>/`, inserts a row in `sandbox_volume_snapshots`, and records `content_hash` (SHA-256 of newline-joined `<relative_path>:<size_bytes>` for all files, sorted). Creates `~/.tag/volumes/.snapshots/` if absent. |
| FR-06 | Snapshot names must be unique per volume. Attempting to create a snapshot with a name that already exists for that volume returns exit code 1 with the message: `"Snapshot '<snap>' already exists for volume '<name>'. Use a different name or delete the existing snapshot first."` |
| FR-07 | Per-volume `quota_bytes` is enforced as a **soft** quota: `tag sandbox volume create --size 10G` records `quota_bytes = 10737418240`. A pre-flight size check runs before `tag sandbox run` mounts the volume: if `current_size_bytes >= quota_bytes`, the run is blocked with exit code 1 and message: `"Volume '<name>' is at or above its quota (X GB / Y GB). Free space or increase the quota before running."` The check uses a Go directory-size walk (`filepath.WalkDir` accumulating `d.Info().Size()` for regular files, skipping symlinks). |
| FR-08 | Mounting a volume whose host path contains any file or directory matching the PRD-028 `BLOCKED_VOLUME_PATTERNS` list is blocked. The check scans one level of the volume root directory (not recursively, for performance). Blocked patterns include: `*.env`, `*.key`, `*.pem`, `*.p12`, `*.pfx`, `*secret*`, `*credential*`, `.ssh`, `.aws`, `.gnupg`. Exit code 1 with message: `"Mount blocked: volume '<name>' contains path matching blocked pattern '<pattern>'. Remove sensitive files before mounting."` |
| FR-09 | `tag sandbox volume restore <name> --snapshot <snap> --confirm` automatically creates a `pre-restore-<ISO8601-UTC>` snapshot of current volume state before overwriting it. The restore copies snapshot contents to a temp directory `~/.tag/volumes/<name>.restore_tmp/` and atomically replaces the live directory via `os.Rename` (atomic on the same filesystem). If interrupted mid-copy, the temp directory is cleaned up and no data is lost. |
| FR-10 | `tag sandbox volume delete <name> --force --purge-snapshots` removes the volume directory, all snapshot directories under `~/.tag/volumes/.snapshots/<name>/`, sets `sandbox_volumes.status = 'deleted'` and `deleted_at = <now>` (soft delete), and removes all `sandbox_volume_snapshots` rows for this volume. Physical directory deletion is permanent; the SQLite row is soft-deleted for audit purposes. |
| FR-11 | Concurrent read-only mounts of the same volume across multiple simultaneous `tag sandbox run` invocations are permitted without locking. The `sandbox_run_mounts` table records each active mount. Read-write concurrent mounts of the same volume by more than one run produce a WARNING (not an error) on stderr: `"Warning: volume '<name>' is mounted read-write by run <run_id>. Concurrent writes may cause data corruption."` |
| FR-12 | Every volume lifecycle event (create, mount-start, mount-end, snapshot, restore, delete) emits an OTEL span via `internal/obs` (using `go.opentelemetry.io/otel` spans). Span attributes follow PRD-013 conventions: `tag.volume.name`, `tag.volume.id`, `tag.volume.operation`, `tag.volume.size_bytes`. |
| FR-13 | All `tag sandbox volume` subcommands produce `--json` output as a stable JSON schema (documented in Section 9). The schema version is included in every JSON response as `"schema_version": "1"`. |
| FR-14 | `tag sandbox volume list` reads only from SQLite (`sandbox_volumes` table) and does not scan the filesystem. `size_bytes` is the last-recorded value from the most recent mount-end or snapshot event. A `--refresh-sizes` flag triggers a live filesystem scan and updates `sandbox_volumes.size_bytes`. |
| FR-15 | The `internal/queue` job dispatcher recognizes a `volumes` key in job YAML (list of `"<name>:<target>[:<options>]"` strings), decoded via koanf v2 + `gopkg.in/yaml.v3` into `[]VolumeMount`, and passes it as the `volumeMounts` argument to the sandbox run call. This enables persistent volumes in fully autonomous queue-dispatched agent jobs. |
| FR-16 | Volume names are scoped to the local TAG installation (`~/.tag/volumes/`). No two active volumes may share the same name; the `sandbox_volumes` table has a `UNIQUE` constraint on `(name, status)` filtered to `status='active'`. Attempting to create a duplicate name returns exit code 1. |
| FR-17 | `tag sandbox volume snapshot list <name>` queries `sandbox_volume_snapshots` and lists all snapshots for the volume in reverse chronological order (newest first). |
| FR-18 | `tag sandbox volume inspect <name>` returns a combined view joining `sandbox_volumes` and `sandbox_volume_snapshots`. If the volume name does not exist in `sandbox_volumes` (or `status = 'deleted'`), exit code 1 with: `"Volume '<name>' not found."` |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Mount latency:** For the Docker backend, the end-to-end overhead of resolving a named volume and passing it as a bind mount must add less than 50 ms to container startup time. Measured as the delta between `sandbox_runs.completed_at - sandbox_runs.created_at` for equivalent runs with and without a volume mount. |
| NFR-02 | **Snapshot throughput:** Snapshot creation (FR-05) must achieve >= 500 MB/s on NVMe SSD for the recursive copy step. This is achievable because the Go `os`/`io.Copy` path uses `copy_file_range`/`sendfile` on Linux (and the platform-optimized copy where available) under the hood for large regular files. Snapshots of volumes > 1 GB must display a progress indicator (a `bubbletea`/`progress` bar, or a simple periodic stderr counter — NOT Python Rich) to avoid appearing frozen. |
| NFR-03 | **SQLite WAL concurrency:** All reads and writes to `sandbox_volumes` and `sandbox_volume_snapshots` go through the `internal/store` open path (`*sql.DB` over `modernc.org/sqlite`) with WAL mode enabled via `PRAGMA journal_mode=WAL` (consistent with existing TAG SQLite usage). Concurrent readers are never blocked by a single writer. |
| NFR-04 | **Disk safety on interrupted snapshot:** If the snapshot copy is interrupted (SIGINT, process kill, power loss), the incomplete snapshot directory `~/.tag/volumes/.snapshots/<name>/<snap>.tmp/` is left in place but is never renamed to the final path. On next startup, TAG's `doctor` command (`internal/cli`) identifies and reports orphaned `.tmp` snapshot directories. |
| NFR-05 | **No mandatory new dependencies:** Volume operations use the Go standard library (`os`, `io`, `crypto/sha256`, `path/filepath`, `database/sql`) plus already-vendored TAG modules. Backend-specific volume APIs (Modal, E2B/firecracker filesystem sync) are compile-time-optional providers (build-tagged provider implementations or out-of-process/HTTP calls), not runtime lazy-imports — Go has no dynamic import. |
| NFR-06 | **Backward compatibility:** Adding `--volume` to `tag sandbox run` is purely additive. Existing invocations without `--volume` behave identically to pre-PRD-096 behavior. Existing `sandbox_runs` table rows are unaffected; only new `sandbox_run_mounts` rows are added. |
| NFR-07 | **Audit trail:** Every mount event appends a JSON line to `~/.tag/runtime/sandbox-audit.jsonl` (existing audit log from PRD-028) with fields: `event="volume_mount"`, `volume_id`, `volume_name`, `target_path`, `mode` (`ro`/`rw`), `run_id`, `backend`, `timestamp`. |
| NFR-08 | **Cross-platform paths:** Volume host paths use Go `path/filepath` (cross-platform join/clean) throughout. On macOS, `~/.tag/volumes/` resolves to `/Users/<user>/.tag/volumes/`. On Linux, `/home/<user>/.tag/volumes/`. Windows is not a supported platform for v1 (consistent with PRD-028). |
| NFR-09 | **Quota check performance:** The pre-flight quota check (FR-07) must complete in < 200 ms for volumes up to 50 GB. Use `os.ReadDir` returning `[]os.DirEntry` and `DirEntry.Info().Size()` (avoiding an extra `lstat` per entry) rather than repeated full-tree stat passes. |

---

## 10. Technical Design

### 10.1 New and Modified Files

| File / Package | Change |
|------|--------|
| `internal/sandbox` | Add `EnsureVolumeSchema()`, `VolumeMount`, `SandboxVolume`, `VolumeSnapshot` structs; add `CreateVolume()`, `DeleteVolume()`, `SnapshotVolume()`, `RestoreVolume()`, `ListVolumes()`, `InspectVolume()`, `ResolveMounts()`, `checkCredentialPatterns()`, `checkQuota()`, `computeDirSize()`, `contentHash()` functions; extend the `RunInSandbox()` call to accept `volumeMounts []VolumeMount`. |
| `internal/cli` | Add cobra commands under the `sandbox volume` group: `create`, `list`, `inspect`, `snapshot`, `restore`, `delete`, `snapshot list`, `snapshot delete`; wire into the `tag sandbox volume` command tree. |
| `internal/queue` | Extend job YAML decoding (koanf v2 + `gopkg.in/yaml.v3`) to recognize the `volumes:` list field; pass the resolved `[]VolumeMount` to the sandbox run call. |
| `internal/obs` | OTEL span helpers (`go.opentelemetry.io/otel`) for volume lifecycle events. |
| `~/.tag/runtime/tag.sqlite3` | Add `sandbox_volumes`, `sandbox_volume_snapshots`, `sandbox_run_mounts` tables via `EnsureVolumeSchema()` (DDL executed through `internal/store` over `modernc.org/sqlite`). |

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

### 10.3 Core Structs

Structs carry exported fields with `json` tags because every subcommand emits `--json`. Each JSON-emitting struct includes a `SchemaVersion` field pinned to `"1"` (FR-13). `Optional[X]` becomes either a pointer (`*string`) or a zero value; `dict` labels become `map[string]string`.

```go
// internal/sandbox (additions)

package sandbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// VolumeMount is the parsed representation of a --volume flag value.
type VolumeMount struct {
	SchemaVersion    string `json:"schema_version"` // always "1"
	Source           string `json:"source"`         // TAG volume name (no '/') or absolute host path
	Target           string `json:"target"`         // absolute path inside the container
	Mode             string `json:"mode"`           // "ro" or "rw"
	IsNamed          bool   `json:"is_named"`       // true if Source is a TAG volume name
	ResolvedHostPath string `json:"resolved_host_path,omitempty"` // set after ResolveMounts()
}

// ParseVolumeMount parses "source:target[:options]" into a VolumeMount.
//
//	"my-data:/workspace/data"      -> named, rw
//	"my-data:/workspace/data:ro"   -> named, ro
//	"/host/path:/container/path"   -> host path, rw
func ParseVolumeMount(spec string) (VolumeMount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return VolumeMount{}, fmt.Errorf(
			"invalid volume spec %q: must be 'source:target' or 'source:target:options'", spec)
	}
	source, target := parts[0], parts[1]
	mode := "rw"
	if len(parts) >= 3 {
		for _, opt := range strings.Split(parts[2], ",") {
			if opt == "ro" {
				mode = "ro"
			}
		}
	}
	return VolumeMount{
		SchemaVersion: "1",
		Source:        source,
		Target:        target,
		Mode:          mode,
		IsNamed:       !strings.HasPrefix(source, "/"),
	}, nil
}

// SandboxVolume is a named persistent volume record from sandbox_volumes.
type SandboxVolume struct {
	SchemaVersion  string            `json:"schema_version"` // always "1"
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Path           string            `json:"path"`
	QuotaBytes     int64             `json:"quota_bytes"`
	SizeBytes      int64             `json:"size_bytes"`
	Status         string            `json:"status"`
	Labels         map[string]string `json:"labels"`
	CreatedAt      string            `json:"created_at"`
	LastMountedAt  *string           `json:"last_mounted_at"`
	LastModifiedAt *string           `json:"last_modified_at"`
	DeletedAt      *string           `json:"-"` // internal; omitted from --json (soft-delete audit field)
	MountCount     int64             `json:"mount_count"`
}

// QuotaHuman renders the quota as a human-readable size (e.g. "10.0 GB").
func (v SandboxVolume) QuotaHuman() string { return humanBytes(v.QuotaBytes) }

// SizeHuman renders the current size as a human-readable size.
func (v SandboxVolume) SizeHuman() string { return humanBytes(v.SizeBytes) }

// VolumeSnapshot is a point-in-time snapshot of a volume.
type VolumeSnapshot struct {
	SchemaVersion string  `json:"schema_version"` // always "1"
	ID            string  `json:"id"`
	VolumeID      string  `json:"volume_id"`
	VolumeName    string  `json:"volume_name"`
	Name          string  `json:"name"`
	Path          string  `json:"path"`
	SizeBytes     int64   `json:"size_bytes"`
	ContentHash   *string `json:"content_hash"`
	Description   *string `json:"description"`
	CreatedAt     string  `json:"created_at"`
	AutoCreated   bool    `json:"auto_created"`
}
```

### 10.4 Core Algorithms

#### Volume Name Validation

Regexes are compiled once into package-level vars with `regexp.MustCompile`; the patterns are unchanged. Validators return an `error` instead of raising.

```go
var (
	volumeNameRE   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)
	snapshotNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
)

// blockedVolumePatterns are the credential-protection globs (extends the PRD-028 blocklist).
var blockedVolumePatterns = []string{
	"*.env", "*.key", "*.pem", "*.p12", "*.pfx",
	"*secret*", "*credential*", "*password*", "*passwd*",
	".ssh", ".aws", ".gnupg", ".netrc",
	"id_rsa", "id_ed25519", "id_ecdsa",
}

func validateVolumeName(name string) error {
	if !volumeNameRE.MatchString(name) {
		return fmt.Errorf(
			"invalid volume name %q: names must start with an alphanumeric "+
				"character and contain only [a-zA-Z0-9_-], max 63 characters", name)
	}
	return nil
}
```

#### Directory Size Computation (fast, NFR-09)

```go
// computeDirSize sums the size of all regular files under root, skipping
// symlinks and ignoring unreadable entries. Uses filepath.WalkDir, which
// reads directories via os.ReadDir and exposes fs.DirEntry.Info() (one stat
// per entry, no extra lstat).
func computeDirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable dirs/files (permission errors), keep walking
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil // do not follow or count symlinks
		}
		if d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
```

#### Content Hash for Snapshot Fingerprinting

```go
// contentHash computes a lightweight fingerprint: SHA-256 over the sorted set
// of "<relpath>:<size>" lines for all regular files under root.
func contentHash(root string) (string, error) {
	var lines []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // skip dirs and symlinks
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		lines = append(lines, fmt.Sprintf("%s:%d", rel, info.Size()))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
```

#### Credential Pattern Check (FR-08)

Go note: `filepath.Match` treats the OS path separator specially, and neither `filepath.Match` nor `path.Match` implement Python `fnmatch`'s case-folding. For the "surround" patterns (`*secret*`, `*credential*`, ...) `path.Match` does work for bare filenames (no separators), but to match `fnmatch` semantics exactly we detect the `*x*` form and fall back to a `strings.Contains` check on the inner literal. Entries are read one level deep via `os.ReadDir` (not recursively, for performance).

```go
// checkCredentialPatterns scans the top level of dir for blocked credential
// patterns and returns an error on the first match.
func checkCredentialPatterns(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // empty or inaccessible volume: allow mount; sandbox sees empty dir
	}
	for _, e := range entries {
		name := e.Name()
		for _, pattern := range blockedVolumePatterns {
			if matchGlob(name, pattern) {
				return fmt.Errorf(
					"mount blocked: volume at %q contains %q matching blocked pattern %q; "+
						"remove sensitive files before mounting", dir, name, pattern)
			}
		}
	}
	return nil
}

// matchGlob mirrors the subset of fnmatch semantics we need. For "*x*" patterns
// (leading and trailing '*') it does a substring test on the inner literal;
// otherwise it uses path.Match (safe for bare filenames, which contain no '/').
func matchGlob(name, pattern string) bool {
	if len(pattern) >= 2 && strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
		return strings.Contains(name, strings.Trim(pattern, "*"))
	}
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}
```

#### Atomic Snapshot Creation (FR-05, NFR-04)

Go has no `shutil.copytree`, so a small recursive `copyTree` helper walks the source with `filepath.WalkDir`, creating directories with `os.MkdirAll`, copying regular files with `io.Copy`, and recreating symlinks as symlinks via `os.Readlink`/`os.Symlink` (never following them — see §11). The copy is written to `<snap>.tmp` and then `os.Rename`d into place (atomic on the same filesystem).

```go
func atomicSnapshot(src, destParent, snapName string) (string, error) {
	if err := os.MkdirAll(destParent, 0o755); err != nil {
		return "", err
	}
	tmpDest := filepath.Join(destParent, snapName+".tmp")
	finalDest := filepath.Join(destParent, snapName)

	_ = os.RemoveAll(tmpDest) // clean orphaned tmp from a prior interrupted run
	if _, err := os.Stat(finalDest); err == nil {
		return "", fmt.Errorf("snapshot %q already exists at %s", snapName, finalDest)
	}

	if err := copyTree(src, tmpDest); err != nil {
		_ = os.RemoveAll(tmpDest) // leave no partial final; drop the tmp on failure
		return "", err
	}
	if err := os.Rename(tmpDest, finalDest); err != nil { // atomic on same filesystem
		return "", err
	}
	return finalDest, nil
}

// copyTree recursively copies src into dst, recreating symlinks as symlinks
// (not following them) and preserving the directory structure.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target) // recreate, do not dereference
		default:
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, in)
			return err
		}
	})
}
```

#### ResolveMounts() — Core Mount Resolution

Named-volume lookups go through `db.QueryRowContext`; `sql.ErrNoRows` is the "unknown volume" case. `dataclasses.replace` becomes a plain struct-field set, and raised exceptions become returned errors.

```go
func ResolveMounts(ctx context.Context, db *sql.DB, mounts []VolumeMount, volumesBase string) ([]VolumeMount, error) {
	resolved := make([]VolumeMount, 0, len(mounts))
	seenTargets := make(map[string]struct{})

	for _, m := range mounts {
		if _, dup := seenTargets[m.Target]; dup {
			return nil, fmt.Errorf(
				"duplicate mount target %q: each container path may only be mounted once per sandbox run", m.Target)
		}
		seenTargets[m.Target] = struct{}{}

		if m.IsNamed {
			var (
				volID      string
				volPath    string
				quotaBytes int64
				sizeBytes  int64
			)
			err := db.QueryRowContext(ctx,
				`SELECT id, path, quota_bytes, size_bytes FROM sandbox_volumes
				 WHERE name = ? AND status = 'active'`, m.Source,
			).Scan(&volID, &volPath, &quotaBytes, &sizeBytes)
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf(
					"unknown volume %q; create it first with: tag sandbox volume create %s", m.Source, m.Source)
			}
			if err != nil {
				return nil, err
			}

			// Credential pattern check.
			if err := checkCredentialPatterns(volPath); err != nil {
				return nil, err
			}

			// Quota pre-flight (rw mounts only).
			if m.Mode == "rw" && sizeBytes >= quotaBytes {
				return nil, fmt.Errorf(
					"volume %q is at or above its quota (%s / %s); free space or run: "+
						"tag sandbox volume create %s --size <larger>",
					m.Source, humanBytes(sizeBytes), humanBytes(quotaBytes), m.Source)
			}

			m.ResolvedHostPath = volPath
		} else {
			// Host-path mount — apply the PRD-028 host-path credential blocklist.
			hostPath := filepath.Clean(m.Source)
			if err := checkCredentialPatterns(hostPath); err != nil {
				return nil, err
			}
			m.ResolvedHostPath = hostPath
		}

		resolved = append(resolved, m)
	}
	return resolved, nil
}
```

### 10.5 Backend Integration

#### Docker Backend

The Docker runner in `internal/sandbox` is extended to accept `volumeMounts []VolumeMount`. Rather than building `docker run --mount ...` strings via subprocess, it uses the docker/moby Go client (`github.com/docker/docker/client`) and passes typed mount specs (`mount.Mount` from `github.com/docker/docker/api/types/mount`) in the container's `HostConfig`:

```go
import (
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func buildMounts(volumeMounts []VolumeMount) []mount.Mount {
	mounts := make([]mount.Mount, 0, len(volumeMounts))
	for _, vm := range volumeMounts {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   vm.ResolvedHostPath,
			Target:   vm.Target,
			ReadOnly: vm.Mode == "ro",
		})
	}
	return mounts
}

// ... later, when creating the container:
hostConfig := &container.HostConfig{Mounts: buildMounts(volumeMounts)}
```

#### E2B / microVM Backend

microVM sandboxes cannot directly access host directories via a bind mount. Where a block device can be attached, the `firecracker-go-sdk` drive API (`AddDrive` / a drive spec on the machine config) attaches a backing device to the microVM. Otherwise, a pre/post file-sync pattern is used — the host directory is walked and files are copied over the guest file API (or a `vsock`/`9p` share). Both sync helpers take a `context.Context` and return an `error`:

```go
// e2bUploadVolume walks the resolved host dir and pushes each regular file into
// the guest at vm.Target (over the guest file API / vsock share).
func e2bUploadVolume(ctx context.Context, sbx GuestFS, vm VolumeMount) error {
	return filepath.WalkDir(vm.ResolvedHostPath, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(vm.ResolvedHostPath, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return sbx.Write(ctx, path.Join(vm.Target, rel), data)
	})
}

// e2bDownloadVolume (rw mounts only) pulls modified guest files back to the host.
func e2bDownloadVolume(ctx context.Context, sbx GuestFS, vm VolumeMount) error {
	if vm.Mode != "rw" {
		return nil
	}
	remote, err := sbx.List(ctx, vm.Target)
	if err != nil {
		return err
	}
	for _, rf := range remote {
		local := filepath.Join(vm.ResolvedHostPath, rf.Name)
		data, err := sbx.Read(ctx, rf.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(local, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
```

#### Modal Backend

Modal is an **optional provider**. Go has no dynamic import, so the Python `modal.Volume.from_name` lazy-import becomes a compile-time-optional provider: a `VolumeProvider` interface with a Modal implementation that is either guarded behind a build tag or invoked out-of-process/over HTTP against the Modal API. If the provider is not compiled in (or the named volume is not resolvable), TAG falls back to the same file-sync path as the E2B/microVM backend.

```go
// VolumeProvider is implemented by optional cloud backends (Modal, ...).
// The Modal implementation is compiled in behind a build tag; when absent,
// providerFor returns (nil, false) and callers use the file-sync fallback.
type VolumeProvider interface {
	Resolve(ctx context.Context, name string) (handle any, ok bool, err error)
}

func modalVolume(ctx context.Context, p VolumeProvider, name string) (any, bool) {
	if p == nil {
		return nil, false // provider not compiled in -> fall back to file sync
	}
	h, ok, err := p.Resolve(ctx, name)
	if err != nil || !ok {
		return nil, false
	}
	return h, true
}
```

#### Restricted Backend

The restricted (in-process/subprocess) backend has no container runtime to enforce mount options, so it uses Linux Landlock (`github.com/landlock-lsm/go-landlock`) to grant the sandboxed process filesystem access scoped to the resolved volume host path — a read-write path rule for `rw` mounts, and a read-only path rule for `ro` mounts — instead of the Python `chmod -w` approach. The read-only concept is preserved at the LSM level.

```go
import "github.com/landlock-lsm/go-landlock/landlock"

func landlockRules(volumeMounts []VolumeMount) []landlock.Rule {
	var rules []landlock.Rule
	for _, vm := range volumeMounts {
		if vm.Mode == "ro" {
			rules = append(rules, landlock.RODirs(vm.ResolvedHostPath))
		} else {
			rules = append(rules, landlock.RWDirs(vm.ResolvedHostPath))
		}
	}
	return rules
}
```

### 10.6 Sortable ID Generation

IDs use `github.com/oklog/ulid/v2` — lexicographically sortable, time-ordered, and a good fit for the `vol_`/`snap_`/`mnt_` prefixed primary keys.

```go
import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

// newID returns a sortable ULID with the given prefix, e.g. newID("vol").
func newID(prefix string) string {
	id := ulid.MustNew(ulid.Timestamp(time.Now()), ulid.Monotonic(rand.Reader, 0))
	return prefix + "_" + id.String()
}
```

### 10.7 Human-Readable Byte Formatting

```go
func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	for i, unit := range units {
		if f < 1024 || i == len(units)-1 {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1f TB", f) // unreachable
}
```

### 10.8 Integration with `internal/queue`

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

The `internal/queue` job runner decodes the job with koanf v2 (using the `gopkg.in/yaml.v3` parser), reads the `volumes` list of `"<name>:<target>[:<options>]"` strings, maps each through `ParseVolumeMount` into `[]VolumeMount`, and passes it to the sandbox run call (`RunInSandbox(ctx, db, cmd, WithVolumeMounts(mounts), ...)`).

```go
type sandboxJob struct {
	Command string   `koanf:"command" yaml:"command"`
	Image   string   `koanf:"image"   yaml:"image"`
	Backend string   `koanf:"backend" yaml:"backend"`
	Volumes []string `koanf:"volumes" yaml:"volumes"`
	Timeout int      `koanf:"timeout" yaml:"timeout"`
}

func mountsFromJob(j sandboxJob) ([]VolumeMount, error) {
	mounts := make([]VolumeMount, 0, len(j.Volumes))
	for _, spec := range j.Volumes {
		vm, err := ParseVolumeMount(spec)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, vm)
	}
	return mounts, nil
}
```

---

## 11. Security Considerations

1. **Credential pattern blocking (FR-08):** Volume mounts undergo the same `BLOCKED_VOLUME_PATTERNS` check as host-path mounts in PRD-028. Patterns are applied to the top-level directory entries of the volume root. This provides a defense-in-depth layer: even if a user inadvertently puts `.env` in a volume, the mount is blocked before any sandbox process can read it.

2. **Read-only enforcement for concurrent swarm mounts:** When `--volume name:/path:ro` is specified, the Docker backend passes `readonly` to the bind-mount options. This is enforced at the container runtime level (Docker daemon), not just by TAG convention. An agent process inside the container cannot escalate to write access on a `ro` mount without a container-escape exploit.

3. **No host path traversal via symlinks in snapshots:** `atomicSnapshot`'s `copyTree` helper recreates symlinks as symlinks (via `os.Readlink` + `os.Symlink`) rather than dereferencing them. This prevents a malicious volume from containing a symlink pointing to `/etc/passwd` that gets followed during snapshot, exfiltrating host data into the snapshot store.

4. **Quota enforcement prevents disk exhaustion:** The pre-flight quota check (FR-07) prevents agent-generated writes from filling the host disk. The soft-quota model means existing data is not deleted, but new sandbox runs mounting the over-quota volume are blocked until the user takes explicit action.

5. **Atomic snapshot write prevents TOCTOU:** The `.tmp` intermediate directory pattern in `_atomic_snapshot` prevents time-of-check/time-of-use races: a snapshot is either fully committed (renamed to final path) or entirely absent. There is no observable intermediate state that another process could read a corrupt partial snapshot from.

6. **Soft delete preserves audit trail:** `sandbox_volumes.status = 'deleted'` with `deleted_at` timestamp means deleted volumes are never fully purged from SQLite. Combined with the `sandbox_run_mounts` history, this enables post-incident forensics: which agent runs accessed which volumes, and when.

7. **Concurrent write-mount warning (FR-11):** Multiple simultaneous read-write mounts of the same volume are warned about (not silently allowed) because Docker bind mounts do not provide filesystem-level locking. Users who need concurrent safe writes should use separate volumes and merge results explicitly.

8. **No volume contents sent to LLM context:** Volume data is never automatically included in any agent prompt or LLM API call. Volumes are opaque binary directories as far as the TAG agent loop is concerned; the agent cannot enumerate volume contents unless the sandbox code explicitly reads and prints them.

9. **Snapshot directory isolation:** All snapshots are stored under `~/.tag/volumes/.snapshots/` (a dot-prefixed hidden directory), separate from live volume data under `~/.tag/volumes/<name>/`. This separation ensures that an agent process inside a sandbox container with `name` mounted at `/workspace/data` cannot enumerate or access the snapshot store, which is never mounted into sandboxes.

10. **Path canonicalization before pattern matching:** Before applying `blockedVolumePatterns`, all paths are canonicalized via `filepath.Clean` and `filepath.EvalSymlinks` to eliminate symlink traversal and `..` components that could bypass pattern matching. Example: `~/.tag/volumes/myvolume/../../../home/user/.ssh/` would resolve to `/home/user/.ssh/` and match the `.ssh` block pattern.

---

## 12. Testing Strategy

### 12.1 Unit Tests

```
internal/sandbox/volumes_test.go
```

Written with the standard `testing` package, table-driven where a set of inputs share one assertion. Errors are checked via returned `error` values (no exception assertions).

| Test | Coverage |
|------|----------|
| `TestVolumeNameValidationValid` | All valid name patterns pass `validateVolumeName()` (returns nil) |
| `TestVolumeNameValidationInvalid` | Names with `/`, `..`, spaces, leading `-`, > 63 chars all return a non-nil error (table-driven) |
| `TestSnapshotNameValidation` | Dots and hyphens allowed in snapshot names; `..` rejected |
| `TestParseVolumeMountNamedRW` | `ParseVolumeMount("my-data:/workspace")` produces `IsNamed=true, Mode="rw"` |
| `TestParseVolumeMountNamedRO` | `ParseVolumeMount("my-data:/workspace:ro")` produces `Mode="ro"` |
| `TestParseVolumeMountHostPath` | `ParseVolumeMount("/tmp/foo:/bar")` produces `IsNamed=false` |
| `TestResolveMountsDuplicateTarget` | `ResolveMounts` with two mounts to the same target returns an error |
| `TestCredentialPatternBlockEnv` | Volume dir containing `secrets.env` returns an error |
| `TestCredentialPatternBlockSSH` | Volume dir containing `.ssh/` returns an error |
| `TestCredentialPatternAllowClean` | Volume dir with only `.txt` and `.py` files returns nil |
| `TestContentHashDeterministic` | Same directory structure always produces the same hash |
| `TestContentHashDiffersOnChange` | Adding one byte to any file changes the hash |
| `TestComputeDirSizeAccuracy` | `computeDirSize` returns exact byte count for a synthetic directory tree |
| `TestComputeDirSizeHandlesPermissionError` | Unreadable subdirectory is skipped without error |
| `TestHumanBytesFormatting` | 1024 → `"1.0 KB"`, 1073741824 → `"1.0 GB"`, 0 → `"0.0 B"` (table-driven) |
| `TestAtomicSnapshotSuccess` | `atomicSnapshot` creates the final directory and removes the tmp |
| `TestAtomicSnapshotNoPartialOnInterrupt` | An injected copy failure mid-run leaves no final dir (tmp cleaned up) |
| `TestAtomicSnapshotErrorsOnDuplicate` | Second call with the same `snapName` returns an error |
| `TestQuotaCheckBlocksAtLimit` | `ResolveMounts` with `SizeBytes >= QuotaBytes` returns an error |
| `TestQuotaCheckAllowsUnderLimit` | `ResolveMounts` with `SizeBytes < QuotaBytes` succeeds |

### 12.2 Integration Tests

```
internal/sandbox/volumes_integration_test.go
```

Backends are injected via Go interfaces (dependency injection) rather than monkeypatching; a fake in-memory backend implements the sandbox-run interface for tests that do not need real Docker. SQLite uses a temp `modernc.org/sqlite` DB.

| Test | Setup | Assert |
|------|-------|--------|
| `TestCreateAndList` | Call `CreateVolume(ctx, db, "test-vol", WithQuotaBytes(1<<30))` | Row in `sandbox_volumes`; directory at `~/.tag/volumes/test-vol/` |
| `TestMountInDockerRun` | Create volume, write file into it, run Docker sandbox reading the file | File content in sandbox stdout |
| `TestSnapshotAndRestore` | Create volume, write file A, snapshot `v1`, write file B, restore `v1`, read dir | Only file A present after restore |
| `TestConcurrentROMounts` | Create volume with 1 MB data; 8 goroutines each run a Docker sandbox with `:ro` | All 8 runs succeed; no data corruption |
| `TestQueueWorkerVolumeYAML` | Write queue job YAML with `volumes:` key; dispatch via the `internal/queue` job runner | Mounted volume data accessible inside job container |
| `TestDeleteBlockedWhileMounted` | Start long-running sandbox with volume mounted; call `DeleteVolume()` | Returns an error; volume still exists |
| `TestSoftDeletePreservesAudit` | Delete a volume; query `sandbox_volumes WHERE status='deleted'` | Row still exists with `deleted_at` set |
| `TestPreRestoreAutoSnapshot` | Restore a volume; list snapshots | `pre-restore-*` snapshot created automatically |

### 12.3 Performance Tests

```
internal/sandbox/volumes_bench_test.go
```

Expressed as Go benchmarks (`func BenchmarkX(b *testing.B)`) and timing tests; thresholds are asserted against `b.Elapsed()` / measured wall time.

| Test | Threshold |
|------|-----------|
| `BenchmarkSnapshotThroughput1GB` | `copyTree` of 1 GB synthetic data completes in < 2 seconds on NVMe |
| `BenchmarkComputeDirSize50GB` | `computeDirSize` on 50 GB (10,000 files × 5 MB) completes in < 200 ms |
| `BenchmarkSQLiteVolumeWriteLatency` | `INSERT INTO sandbox_volumes` via `ExecContext` in WAL mode < 5 ms (p99 over 100 iterations) |
| `BenchmarkMountResolutionLatency` | `ResolveMounts()` for 4 named volumes < 10 ms (4 SQLite reads + 4 `os.ReadDir` calls) |

### 12.4 Security Tests

```
internal/sandbox/volumes_security_test.go
```

| Test | Description |
|------|-------------|
| `TestSymlinkNotFollowedInSnapshot` | Volume contains symlink to `/etc/passwd`; snapshot recreates the symlink and does not copy `/etc/passwd` contents |
| `TestPathCanonicalizationBypassesDotDot` | `ResolveMounts` with `Source="../../../etc"` returns an error (not a valid volume name due to regex) |
| `TestAllBlockedPatternsRejected` | Table-driven over `blockedVolumePatterns`: a volume containing a matching file is blocked for each pattern |
| `TestROMountBlocksWriteInContainer` | Docker run with `:ro` volume; `echo foo > /mounted/file` inside container exits non-zero |

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
| AC-10 | 8 concurrent `tag sandbox run --volume shared-data:/data:ro` invocations all complete successfully with identical stdout, no errors, and no data in `shared-data` modified. | Launch 8 parallel processes (via `os/exec` from a test, or a shell `for` loop with `&`); assert all exit 0 and volume mtime unchanged. |
| AC-11 | `tag sandbox volume delete my-data --force --purge-snapshots` removes the host directory, all snapshot directories, and sets `sandbox_volumes.status='deleted'` but preserves the SQLite row. | `ls ~/.tag/volumes/my-data` → no such file; `sqlite3 ... "SELECT status,deleted_at FROM sandbox_volumes WHERE name='my-data'"` → `deleted|<timestamp>`. |
| AC-12 | A queue job YAML with `volumes: [my-data:/workspace:rw]` dispatched via the `internal/queue` job runner mounts the named volume into the sandbox. | Write a job that creates a file at `/workspace/output.txt`; after job completion, verify `~/.tag/volumes/my-data/output.txt` exists on host. |
| AC-13 | `tag sandbox volume create my-data` (duplicate name) returns exit code 1 with message `"Volume 'my-data' already exists."` without creating a new directory or row. | Create volume twice; assert second call exit code 1. |
| AC-14 | Attempting to run `tag sandbox run --volume over-quota-vol:/data` when the volume's `size_bytes >= quota_bytes` returns exit code 1 with a quota exceeded message. | Manually set `size_bytes = quota_bytes` in SQLite; attempt mount; assert error. |

---

## 14. Dependencies

| Dependency | Type | Version / Constraint | Notes |
|------------|------|---------------------|-------|
| Go `os` / `io` | stdlib | Go >= 1.22 | Recursive copy, `os.Rename`, `os.ReadDir`, `filepath.WalkDir` |
| Go `crypto/sha256` + `encoding/hex` | stdlib | Go >= 1.22 | Content-hash fingerprinting |
| Go `path/filepath` | stdlib | Go >= 1.22 | Cross-platform path ops, `filepath.Clean`/`EvalSymlinks` |
| Go `database/sql` | stdlib | Go >= 1.22 | `*sql.DB`, `QueryContext`/`ExecContext`, `sql.Tx` |
| Go `regexp` | stdlib | Go >= 1.22 | Name-validation regexes |
| `modernc.org/sqlite` | vendored | latest | Pure-Go SQLite (CGO_ENABLED=0), WAL via PRAGMA, FTS5 built-in |
| Docker Engine API (host) | optional | >= 20.10 | Bind-mount support for the Docker backend |
| `github.com/docker/docker/client` (+ `.../api/types/mount`) | module | moby client | Typed mount specs for the Docker backend (no `docker run` subprocess) |
| `github.com/firecracker-microvm/firecracker-go-sdk` | optional module | latest | microVM drive attach + guest file sync for the E2B/microVM backend |
| Modal provider | optional | — | Compile-time-optional `VolumeProvider` (build-tagged) or out-of-process/HTTP call to the Modal API; not a runtime import |
| `go.opentelemetry.io/otel` | module | latest | OTEL span emission for volume lifecycle events (via `internal/obs`) |
| `github.com/spf13/cobra` | module | latest | `tag sandbox volume` command group in `internal/cli` |
| `github.com/knadh/koanf/v2` + `gopkg.in/yaml.v3` | module | latest | Queue job YAML decoding of the `volumes:` list |
| `github.com/oklog/ulid/v2` | module | latest | Sortable `vol_`/`snap_`/`mnt_` IDs |
| `github.com/landlock-lsm/go-landlock` | module | latest | Path-scoped read/read-write rules for the restricted backend (Linux) |
| PRD-028 `internal/sandbox` | internal | v1 (existing) | `RunInSandbox()`, `EnsureSchema()`, backends — extended by this PRD |
| PRD-013 `internal/obs` | internal | v1 (existing) | OTEL span emission for volume lifecycle events |
| PRD-034 credential validation | internal | v1 (existing) | `blockedVolumePatterns` in `internal/sandbox`/`internal/credentials` — extended/referenced |
| PRD-012 budget | internal | v1 (existing) | Quota tracking hooks (future: disk quota → budget alerts) |
| PRD-020 CI | internal | v1 (existing) | `--json` output consumed by CI pipelines |
| Progress indicator | Go | — | `bubbletea`/`progress` bar or a simple stderr counter for large snapshots (NFR-02) — replaces Python `rich` |

---

## 15. Open Questions

| ID | Question | Owner | Resolution Deadline |
|----|----------|-------|---------------------|
| OQ-01 | Should the E2B pre/post sync use E2B's native file API (`sbx.files.write`) or `rsync` over SSH? E2B's file API has a ~1 MB/s throughput limit for large volumes; rsync requires the sandbox to have SSH accessible. Need to benchmark both approaches for 1 GB volumes. | Sandbox team | Before implementation start |
| OQ-02 | Should `quota_bytes` be a hard limit enforced inside the container (via `docker run --storage-opt size=10G`) or only a pre-flight soft check? Hard enforcement requires Docker daemon with `overlay2` storage driver and `dm.basesize` config — not available on all platforms. | Platform team | Sprint 1 |
| OQ-03 | Should snapshot storage be content-addressed (deduplicated between snapshots that share files) to save disk space? A full recursive copy is simple and correct but expensive for large volumes with small changes. A COW filesystem (btrfs `cp --reflink`, APFS clonefile) could make snapshots near-instant and near-zero-cost. From Go, reflink clones are reachable via the `FICLONE`/`copy_file_range` ioctls on Linux and `clonefile(2)` on macOS APFS (through `golang.org/x/sys/unix`). | Architecture | Sprint 2 |
| OQ-04 | Should volume names be namespaced by TAG profile (e.g., `coder::my-data`) to prevent accidental cross-profile data sharing in multi-profile installations? Or is global namespace simpler and more useful? | Product | Before CLI design is finalized |
| OQ-05 | When a Modal sandbox is used and a `modal.Volume` with the same name exists in the user's Modal workspace, should TAG automatically use it (providing true cloud persistence)? This requires `modal auth` credentials at volume-create time. The discovery UX needs design. | Integrations team | Sprint 2 |
| OQ-06 | Should `tag sandbox volume` support volume templates — pre-populated volumes created from a public URL (e.g., `tag sandbox volume create my-data --from-url s3://bucket/dataset.tar.gz`)? This would enable standard ML benchmark datasets to be instantiated in one command. | Product | Post-v1 |
| OQ-07 | What is the correct behavior when the host filesystem runs low on disk space (< 1 GB free) during a snapshot? Should TAG fail fast with a clear error, or attempt the snapshot and let the recursive copy's `io.Copy` surface a wrapped `ENOSPC` ("no space left on device") error? | Engineering | Sprint 1 |
| OQ-08 | Should `sandbox_run_mounts.unmounted_at` be set by a cleanup sweep (in case TAG is killed mid-run and the mount record is never closed) or only set synchronously at run completion? A startup sweep that closes stale mount records would fix orphaned entries. | Engineering | Sprint 1 |

---

## 16. Complexity and Timeline

**Total estimated effort:** 9–11 engineering days

### Phase 1 — Core Volume Lifecycle (Days 1–3)

- SQLite DDL: `sandbox_volumes`, `sandbox_volume_snapshots`, `sandbox_run_mounts` tables via `EnsureVolumeSchema()` (executed through `internal/store` over `modernc.org/sqlite`).
- `SandboxVolume`, `VolumeSnapshot`, `VolumeMount` Go structs (json-tagged).
- `CreateVolume()`, `DeleteVolume()`, `ListVolumes()`, `InspectVolume()` in `internal/sandbox`.
- `validateVolumeName()`, `computeDirSize()`, `humanBytes()`.
- `create`, `list`, `delete` cobra subcommands under `sandbox volume` in `internal/cli`.
- Unit tests for name validation, size computation, SQLite schema.

### Phase 2 — Snapshot Engine (Days 4–5)

- `atomicSnapshot()` (+ `copyTree()`), `contentHash()`, `SnapshotVolume()`, `RestoreVolume()`.
- Auto-snapshot on restore (FR-09).
- `snapshot`, `restore`, `snapshot list`, `snapshot delete` cobra subcommands in `internal/cli`.
- Unit tests for atomic snapshot, content hash, duplicate detection.
- Integration test: snapshot → modify → restore → verify.

### Phase 3 — Mount Integration (Days 6–8)

- `ParseVolumeMount()`, `ResolveMounts()`.
- `checkCredentialPatterns()`, `checkQuota()`.
- Extend the Docker runner to attach typed `mount.Mount` specs (moby client) for resolved volumes.
- Extend the `RunInSandbox()` signature with `volumeMounts []VolumeMount`.
- `sandbox_run_mounts` insert/update on run start/end.
- Extend `internal/queue` to decode the `volumes:` YAML key (koanf + yaml.v3).
- Integration tests: Docker read-write mount, Docker read-only mount, concurrent ro mounts, credential block, quota block.

### Phase 4 — Backend Extensions and Polish (Days 9–11)

- E2B/microVM pre/post file sync (`e2bUploadVolume`, `e2bDownloadVolume`) + firecracker drive attach.
- Modal optional `VolumeProvider` (build-tagged / HTTP) with file-sync fallback.
- OTEL span emission for all volume lifecycle events (FR-12) via `internal/obs`.
- `--json` output on all subcommands; JSON schema snapshot test.
- `inspect` subcommand (full detail view) in `internal/cli`.
- Performance tests: snapshot throughput, dir-size speed, SQLite write latency (Go benchmarks).
- Security tests: symlink safety, path canonicalization, all blocked patterns.
- Go progress indicator (bubbletea/progress or stderr counter) for large snapshot/restore operations (NFR-02).
- `doctor` command check (`internal/cli`) for orphaned `.tmp` snapshot directories (NFR-04).

### Milestone: PR Ready for Review — Day 11

All acceptance criteria (AC-01 through AC-14) passing in CI. `tag sandbox volume --help` producing correct documentation. JSON schema stable and snapshot-tested.

