# PRD-110: Loop State Serialization (`tag workflow checkpoint`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `internal/agent` (loop state machine) + `internal/store` (checkpoint persistence)
**Depends on:** PRD-112 (graph-based workflow engine), PRD-109 (HITL interrupt), PRD-113 (time-travel debugging)
**Inspired by:** LangGraph SqliteSaver checkpoint, LangChain checkpoint serializers, Redis checkpoint for AI agents, Temporal.io workflow state persistence; opencode/crush Thread/Turn/Item durable-session model

---

## 1. Overview

Long-running agent workflows are brittle in the face of process crashes, network timeouts, and deliberate pauses. TAG's current execution model is run-to-completion with no intermediate state persistence — if the process dies on step 47 of a 60-step workflow, all 47 steps must be re-executed from scratch. This wastes API credits, time, and produces non-deterministic results when re-execution takes different code paths.

Loop State Serialization (`tag workflow checkpoint`) introduces a `Checkpointer` (in `internal/store`) modeled after LangGraph's `SqliteSaver` and the opencode/crush durable-session pattern — every workflow step writes a complete, serialized snapshot of the workflow state graph to a SQLite `workflow_checkpoints` table. On restart, the workflow engine detects the most recent checkpoint and resumes the agent loop from that exact step, replaying no prior work. Checkpoints are also the foundation for HITL interrupt/resume (PRD-109) and time-travel debugging (PRD-113).

The checkpointer serializes the full workflow state (agent outputs, tool call results, intermediate artifacts, and the agent loop's `continue|compact|stop` state-machine position) as **versioned JSON** via `encoding/json`, with an optional transparent `zstd` compression layer to keep on-disk size small. Checkpoint writes are transactional (`BEGIN IMMEDIATE`) and append-only; the most recent checkpoint per session is the canonical resume point.

> **Serialization re-framed for Go.** The original Python design used `msgpack` (primary) with a JSON fallback. In Go we standardise on `encoding/json` as the single canonical codec — cross-version-safe, human-debuggable, and free of the reflection/pickle deserialization risk class (the migration explicitly forbids `encoding/gob` for cross-version state). Compactness — msgpack's original motivation — is recovered with an optional `klauspost/compress/zstd` wrapper recorded in the `state_format` column (`json` | `json+zstd`). See §9.3 and Open Question OQ-01.

---

## 2. Problem Statement

### 2.1 No crash recovery for long workflows

A workflow running 60 LLM calls over 45 minutes can fail at step 47 due to a network timeout, OOM error, or deliberate Ctrl-C. Without checkpointing, the engineer must restart from scratch — re-incurring ~$8 of API cost and 40 minutes of wall time.

### 2.2 HITL interrupt requires durable state

PRD-109 HITL interrupts require that workflow state is durably stored while waiting for human approval (which may take hours or days). Without a checkpointer, the TAG process must remain running during the approval window — blocking resources and failing on any server restart.

### 2.3 No time-travel for debugging

PRD-113 time-travel debugging requires a history of all intermediate states to allow rewinding to a previous step. Without per-step checkpoints, there is no history to rewind.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Write a complete serialized snapshot of the workflow state to SQLite after every step. |
| G2 | On workflow startup, detect and offer to resume from the most recent checkpoint for the given session. |
| G3 | Support `tag workflow checkpoint list <session-id>` to show all checkpoints with step number and timestamp. |
| G4 | Support `tag workflow checkpoint restore <session-id> --step N` to restore state to a specific step (time-travel, PRD-113). |
| G5 | Serialize workflow state as versioned JSON (`encoding/json`) as the single canonical codec, with an optional transparent `zstd` compression layer for compact storage. (Re-framed from the Python msgpack-primary/JSON-fallback design.) |
| G6 | Checkpoint pruning: keep only the last N checkpoints per session to bound disk usage. |
| G7 | Serialize checkpoint writes using SQLite WAL mode and `BEGIN IMMEDIATE` transactions under the single-writer + `gofrs/flock` discipline. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Remote checkpoint storage (Redis, S3). SQLite local only. |
| NG2 | Incremental/delta checkpoints. Each checkpoint is a full state snapshot. |
| NG3 | Checkpoint encryption. State may contain sensitive agent outputs; encryption is a future enhancement. |
| NG4 | Cross-session state sharing via checkpoints. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Checkpoint write latency | < 50ms per checkpoint for a 100KB state | `testing.B` benchmark |
| Resume accuracy | 100% state fidelity after checkpoint/restore cycle | Integration test |
| Disk usage | Checkpoint for a 1MB state stored in < 1.5MB on disk (with `json+zstd` compression) | Disk measurement |
| Concurrent write safety | Zero corruption with 2 concurrent checkpoint writers on the same session | Concurrency test (`errgroup`) |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Have workflows automatically resume from the last checkpoint after a crash | I don't lose hours of work |
| US2 | Developer | List all checkpoints for a workflow session | I can understand where a workflow was at each step |
| US3 | Developer | Restore a workflow to step 20 to debug what went wrong | I can replay from a known good state |
| US4 | Platform engineer | Set checkpoint retention to 5 to bound disk usage | Long workflows don't fill the disk |

---

## 6. CLI Surface

```
tag workflow checkpoint <subcommand> [options]

Subcommands:
  list       List all checkpoints for a session
  show       Show the state at a specific checkpoint
  restore    Restore workflow state to a specific step
  delete     Delete checkpoints for a session
  prune      Delete all but the last N checkpoints

tag workflow checkpoint list <session-id>
tag workflow checkpoint show <session-id> --step N
tag workflow checkpoint restore <session-id> --step N
tag workflow checkpoint delete <session-id> [--step N | --all]
tag workflow checkpoint prune <session-id> --keep N

# Workflow execution options:
tag workflow run --goal GOAL [--checkpoint-interval 1] [--no-checkpoint] [--resume <session-id>]

Options:
  --checkpoint-interval N   Checkpoint every N steps (default: 1)
  --no-checkpoint           Disable checkpointing
  --resume SESSION_ID       Resume from most recent checkpoint for this session
  --keep N                  Number of checkpoints to retain per session (default: 100)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | After each workflow step, `Checkpointer.Save(ctx, sessionID, stepNum, state)` serializes the state and writes a row to `workflow_checkpoints`. |
| FR-02 | Serialization: encode the state with `encoding/json` (canonical). If the JSON exceeds a configurable threshold, transparently compress with `klauspost/compress/zstd` and record `state_format = "json+zstd"`; otherwise `state_format = "json"`. A `schema_version` field is embedded so future readers can migrate. |
| FR-03 | On `tag workflow run --resume <session-id>`: query the most recent checkpoint for the session, deserialize state, and start the agent loop / workflow graph at `stepNum + 1`. |
| FR-04 | `tag workflow checkpoint list` renders a table of (step, timestamp, state_size_bytes, state_format). |
| FR-05 | `tag workflow checkpoint show --step N` deserializes the checkpoint and renders the state as indented JSON (`json.MarshalIndent`), truncating large values. |
| FR-06 | `tag workflow checkpoint restore --step N` returns the deserialized state (a typed struct / `map[string]any`) to the caller (used by PRD-113 time-travel debugging). |
| FR-07 | Checkpoint pruning: after writing checkpoint step N, delete all checkpoints where `step_num <= N - keep`. |
| FR-08 | Thread safety: use a `BEGIN IMMEDIATE` transaction for all checkpoint writes to serialize concurrent access; the store already holds the single-writer `gofrs/flock` lock. |
| FR-09 | On serialization failure (a state value that cannot be JSON-encoded): log a warning, substitute a `"<non-serializable>"` placeholder for that value, and continue. |
| FR-10 | `--no-checkpoint` disables all checkpoint writes; the workflow streams without crash recovery. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Checkpoint table indexed on `(session_id, step_num DESC)` for fast latest-checkpoint queries. |
| NFR-02 | Checkpoint blobs stored as SQLite `BLOB` columns (not external files) for atomicity. |
| NFR-03 | Checkpoint write must complete before the step function returns; async/buffered writes are not allowed (they would lose state on crash). The `Save` call is synchronous within the step. |
| NFR-04 | Maximum checkpoint blob size 10MB; larger states trigger a warning and a size reduction (drop large artifact blobs). |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS workflow_checkpoints (
  id              TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL,
  step_num        INTEGER NOT NULL,
  state_blob      BLOB NOT NULL,
  state_format    TEXT NOT NULL DEFAULT 'json',   -- 'json' | 'json+zstd'
  schema_version  INTEGER NOT NULL DEFAULT 1,
  state_size      INTEGER NOT NULL,               -- encoded (post-compression) byte length
  created_at      TEXT NOT NULL,                  -- RFC 3339 UTC
  UNIQUE(session_id, step_num)
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_session_step
  ON workflow_checkpoints(session_id, step_num DESC);
```

The migration is registered in the `internal/store` migration chain and runs under the single-writer WAL discipline shared by the rest of `tag.sqlite3`.

### 9.2 Go core

`Checkpointer` lives in `internal/store` and operates on the shared `*sql.DB` (`modernc.org/sqlite`, WAL, `busy_timeout`). It is dependency-injected with a `Clock` and an optional compression codec so tests are deterministic.

```go
// internal/store/checkpoint.go
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/klauspost/compress/zstd"
)

// SchemaVersion is bumped when the state envelope changes; readers migrate on load.
const SchemaVersion = 1

// State is the serializable workflow snapshot. The agent loop's state-machine
// position (continue|compact|stop) and per-step outputs live under Values.
type State struct {
	SchemaVersion int            `json:"schema_version"`
	Phase         string         `json:"phase"` // "continue" | "compact" | "stop"
	Values        map[string]any `json:"values"`
}

// Checkpointer persists per-step workflow snapshots to workflow_checkpoints.
type Checkpointer struct {
	db          *sql.DB
	keep        int
	now         func() time.Time
	zstdMinSize int             // compress when encoded JSON exceeds this
	enc         *zstd.Encoder
	dec         *zstd.Decoder
}

func NewCheckpointer(db *sql.DB, keep int) *Checkpointer {
	enc, _ := zstd.NewWriter(nil)
	dec, _ := zstd.NewReader(nil)
	return &Checkpointer{
		db: db, keep: keep, now: func() time.Time { return time.Now().UTC() },
		zstdMinSize: 64 * 1024, enc: enc, dec: dec,
	}
}

// Save serializes state (canonical JSON, optional zstd) and writes one row,
// then prunes checkpoints older than the retention window — all inside one
// BEGIN IMMEDIATE transaction so concurrent writers serialize cleanly.
func (c *Checkpointer) Save(ctx context.Context, sessionID string, stepNum int, state State) error {
	state.SchemaVersion = SchemaVersion
	raw, err := json.Marshal(state)
	if err != nil {
		// FR-09: substitute non-serializable values and retry once.
		raw = mustJSON(sanitizeState(state))
	}

	blob, format := raw, "json"
	if len(raw) > c.zstdMinSize {
		blob, format = c.enc.EncodeAll(raw, nil), "json+zstd"
	}

	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{}) // driver issues BEGIN IMMEDIATE for write txns
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err = tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO workflow_checkpoints
		  (id, session_id, step_num, state_blob, state_format, schema_version, state_size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		newID(), sessionID, stepNum, blob, format, SchemaVersion, len(blob),
		c.now().Format(time.RFC3339)); err != nil {
		return err
	}
	// FR-07: prune old checkpoints.
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM workflow_checkpoints WHERE session_id = ? AND step_num <= ?`,
		sessionID, stepNum-c.keep); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadLatest returns the most recent checkpoint's step number and state.
func (c *Checkpointer) LoadLatest(ctx context.Context, sessionID string) (int, *State, error) {
	var stepNum int
	var blob []byte
	var format string
	err := c.db.QueryRowContext(ctx, `
		SELECT step_num, state_blob, state_format
		  FROM workflow_checkpoints
		 WHERE session_id = ? ORDER BY step_num DESC LIMIT 1`, sessionID).
		Scan(&stepNum, &blob, &format)
	if err == sql.ErrNoRows {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	st, err := c.deserialize(blob, format)
	return stepNum, st, err
}

// LoadStep returns the state for a specific step (time-travel, PRD-113).
func (c *Checkpointer) LoadStep(ctx context.Context, sessionID string, stepNum int) (*State, error) {
	var blob []byte
	var format string
	err := c.db.QueryRowContext(ctx,
		`SELECT state_blob, state_format FROM workflow_checkpoints WHERE session_id = ? AND step_num = ?`,
		sessionID, stepNum).Scan(&blob, &format)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c.deserialize(blob, format)
}

// Row is one line of `tag workflow checkpoint list`.
type Row struct {
	StepNum   int    `json:"step_num"`
	StateSize int    `json:"state_size"`
	Format    string `json:"state_format"`
	CreatedAt string `json:"created_at"`
}

func (c *Checkpointer) List(ctx context.Context, sessionID string) ([]Row, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT step_num, state_size, state_format, created_at
		  FROM workflow_checkpoints WHERE session_id = ? ORDER BY step_num`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.StepNum, &r.StateSize, &r.Format, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (c *Checkpointer) deserialize(blob []byte, format string) (*State, error) {
	if format == "json+zstd" {
		var err error
		if blob, err = c.dec.DecodeAll(blob, nil); err != nil {
			return nil, err
		}
	}
	var st State
	if err := json.Unmarshal(blob, &st); err != nil {
		return nil, err
	}
	// Future: migrate st across schema_version deltas here.
	return &st, nil
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
```

Key Go swaps from the Python design:

- `SqliteCheckpointer` class → `Checkpointer` struct on the shared `internal/store` `*sql.DB` (no per-call `sqlite3.connect`; connection pooling and WAL are owned by the store).
- `msgpack.packb` / JSON fallback → canonical `encoding/json` + optional `klauspost/compress/zstd`. No `msgpack` dependency; no `encoding/gob` (banned for cross-version state).
- `with conn:` transaction → `db.BeginTx` (driver issues `BEGIN IMMEDIATE` for write transactions) with `defer tx.Rollback()` + explicit `Commit`.
- `default=str` "encode anything" fallback → explicit `sanitizeState` that replaces non-encodable values with `"<non-serializable>"` (FR-09), keeping the state envelope typed and predictable.
- Serialization is coupled to the agent loop's `continue|compact|stop` state machine (`State.Phase`): a checkpoint captures both the workflow values and the loop's position so resume re-enters the correct state-machine branch.

### 9.3 Serialization format (re-framed)

| Concern | Python (original) | Go (this PRD) |
|---------|-------------------|---------------|
| Primary codec | `msgpack` (binary) | `encoding/json` (text, versioned) |
| Fallback codec | JSON with `default=str` | `sanitizeState` placeholder + JSON |
| Compactness | msgpack packing | optional `zstd` over the JSON (`state_format='json+zstd'`) |
| Cross-version safety | implicit | explicit `schema_version` column + envelope field; `gob` forbidden |
| Deserialization risk | none (msgpack) | none (JSON); no reflection/pickle decode path |

Rationale: the migration mandates JSON for durable cross-version state (opencode/crush Thread/Turn/Item pattern) and explicitly bans `gob`. JSON is human-debuggable for `checkpoint show`, and `zstd` restores the size win that motivated msgpack without introducing a binary, schema-opaque format.

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Sensitive agent outputs in checkpoints | Checkpoints stored in `~/.tag/runtime/tag.sqlite3` (mode 0600); BLOB (compressed JSON) not casually human-readable |
| Checkpoint blob injection | Deserialization uses `encoding/json` (+ `zstd` decompress) only — never `gob`, `pickle`, or any reflection-driven decoder; no arbitrary code execution path |
| Disk exhaustion | `--keep N` pruning + 10MB blob size limit + warning on oversized states |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `Save` + `LoadLatest` round-trip; `json` vs `json+zstd` path selection; pruning logic; `sanitizeState` on non-serializable values — all table-driven with an injected `Clock` |
| Integration | Simulate a crash mid-workflow; `--resume` from checkpoint; verify step outputs and loop phase match |
| Concurrency | Two goroutines (`errgroup`) writing checkpoints on the same session; verify no corruption under WAL + `BEGIN IMMEDIATE` |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | After each workflow step, a checkpoint row exists in `workflow_checkpoints` |
| AC-02 | Killing the process at step 5 and running `tag workflow run --resume <id>` continues from step 5 |
| AC-03 | `tag workflow checkpoint list <id>` shows all checkpoint steps with timestamps |
| AC-04 | Checkpoint writes complete in < 50ms for a 100KB state (`testing.B`) |
| AC-05 | Checkpoints beyond `--keep N` are pruned automatically |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-109 HITL interrupt | Interrupt/resume requires checkpoint |
| PRD-112 graph-based workflow | Step execution framework |
| PRD-113 time-travel debugging | Restore-to-step functionality |
| `modernc.org/sqlite` + `database/sql` (stdlib) | WAL checkpoint persistence via `internal/store` |
| `encoding/json` (stdlib) | Canonical state serialization |
| `github.com/klauspost/compress/zstd` | Optional transparent compression (replaces msgpack's compactness role) |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should `zstd` compression be always-on above the size threshold, or a per-session opt-in? (`json` alone is simpler to debug; `json+zstd` bounds disk.) |
| OQ-02 | Should there be a checkpoint size budget per session (beyond `--keep N`) to avoid unbounded growth? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | SQLite DDL + migration, `Checkpointer` Save/Load (JSON + zstd), table-driven unit tests | 2 |
| 2 | Pruning, list/show/restore cobra commands under `internal/cli` | 1 |
| 3 | Integration with the agent loop / workflow engine (PRD-112), resume + loop-phase restore logic | 2 |
| 4 | Integration tests, concurrency tests (`errgroup`), documentation | 1 |
