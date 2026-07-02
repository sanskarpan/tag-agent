# PRD-113: Time-Travel Debugging (`tag workflow rewind`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `internal/queue + internal/cli + internal/tui`
**Depends on:** PRD-110 (state serialization/checkpointing), PRD-112 (graph-based workflow engine)
**Inspired by:** LangGraph time-travel debugging, Redux DevTools state inspector, Temporal.io workflow replay, rr (Mozilla record-replay)

---

## 1. Overview

Debugging complex agent workflows is notoriously difficult: when a 50-step workflow produces a wrong answer at step 47, the engineer must re-run the entire workflow (incurring time and cost) to reproduce the bug. LangGraph's Studio offers "time-travel" debugging — the ability to rewind a workflow to a previous step, modify the state, and re-execute from that point. TAG has no equivalent.

Time-Travel Debugging (`tag workflow rewind`) builds on the checkpoint infrastructure (PRD-110) to add full workflow replay and fork capabilities. Engineers can inspect any historical state snapshot, modify the state dict, and fork a new execution branch from any checkpoint. The fork creates a new session that diverges from the original at the chosen step, allowing "what if" experiments without re-running prior steps.

The design is inspired by LangGraph's `update_state()` + `graph.stream(None, config, stream_mode="values")` time-travel pattern, Redux DevTools' step inspector, and rr's record-replay debugging. TAG's implementation is terminal-first: a TUI inspector (`tag workflow rewind <session-id>`) walks through checkpoints interactively.

---

## 2. Problem Statement

### 2.1 No way to inspect intermediate workflow state

When a workflow fails at step 47, the engineer can only see the final error. Checkpoints (PRD-110) store intermediate state, but there is no user-facing tool to inspect them.

### 2.2 Re-running from scratch is expensive

A 60-step workflow costs $15 and 40 minutes. Re-running from scratch to reproduce a bug at step 47 wastes $12 and 35 minutes of prior steps that ran correctly.

### 2.3 No "what if" experimentation

Engineers often want to test "what if I had given the agent different context at step 30?" This requires forking the execution from step 30 — impossible without time-travel.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag workflow rewind <session-id>` opens an interactive TUI showing all checkpoint steps with timestamps and brief state summaries. |
| G2 | From the TUI, select any step to inspect the full state dict at that checkpoint. |
| G3 | `tag workflow rewind <session-id> --step N --fork` creates a new session that begins executing from step N with the original state at that checkpoint. |
| G4 | `tag workflow rewind <session-id> --step N --fork --patch '{"key": "value"}'` forks with a modified state dict (patches applied before re-execution). |
| G5 | Fork sessions maintain a `forked_from` reference to the original session for lineage tracking. |
| G6 | `tag workflow rewind show <session-id> --step N` non-interactively prints the state at step N as JSON. |
| G7 | Support diff view: `--diff N1 N2` shows the state changes between step N1 and N2. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Visual GUI time-travel debugger. Terminal TUI only. |
| NG2 | Replaying individual tool calls (only full node replay). |
| NG3 | Modifying the graph structure on fork (only state modification). |
| NG4 | Distributed session forking across machines. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Fork launch time | Fork session starts in < 500ms (checkpoint load + new session init) | Benchmark test |
| State inspection latency | TUI renders any checkpoint state in < 100ms | Benchmark test |
| Fork lineage tracking | `forked_from` session reference visible in `tag workflow graph list` | Integration test |
| Diff accuracy | `--diff N1 N2` shows exactly the keys that changed between steps | Unit test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Inspect intermediate workflow state at any step | I diagnose what went wrong without re-running |
| US2 | Developer | Fork from step 30 to test a different agent prompt | I experiment without re-executing 30 steps |
| US3 | Developer | Apply a state patch before forking | I correct a bad intermediate state and continue |
| US4 | Developer | See the diff between two steps | I understand what changed between checkpoints |

---

## 6. CLI Surface

```
tag workflow rewind <session-id> [options]

Options:
  --step N              Step number to rewind to (omit for interactive TUI)
  --fork                Create a new forked session from step N
  --patch JSON          JSON patch to apply to state before forking
  --diff N1 N2          Show state diff between steps N1 and N2
  --format json|table   Output format for state inspection

Examples:
  tag workflow rewind abc123                          # Interactive TUI
  tag workflow rewind abc123 --step 30               # Show state at step 30
  tag workflow rewind abc123 --step 30 --fork         # Fork new session from step 30
  tag workflow rewind abc123 --step 30 --fork --patch '{"query": "different query"}'
  tag workflow rewind abc123 --diff 29 31            # Show changes from step 29 to 31

TUI controls:
  j/k     Navigate steps
  Enter   Inspect state at current step
  f       Fork from current step
  p       Fork with patch (opens editor)
  d       Diff with previous step
  q       Quit
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag workflow rewind <session-id>` (no step) opens TUI showing all checkpoint steps with step number, timestamp, and top-3 changed keys. |
| FR-02 | TUI step selection loads the state map from the PRD-110 `Checkpointer.LoadStep()` store interface and renders it as a paginated JSON view (bubbles viewport + glamour). |
| FR-03 | `--fork` creates a new `workflow_sessions` row with `forked_from = original_session_id, forked_at_step = N`. |
| FR-04 | Fork execution: load state at step N, apply optional `--patch`, resume `Graph.Run()` from step N+1 using the forked state. |
| FR-05 | `--patch JSON` is applied as a shallow merge over the checkpoint state dict; conflicting keys overwritten. |
| FR-06 | `--diff N1 N2` loads both checkpoint states, computes the symmetric difference of keys and changed values, and renders as a colored diff table. |
| FR-07 | `tag workflow graph list` shows `forked_from` column for forked sessions. |
| FR-08 | Fork maintains all prior checkpoints from the original session for further time-travel debugging. |
| FR-09 | TUI renders key-value state summary (truncated to 80 chars per value) for navigation; full state shown on Enter. |
| FR-10 | `tag workflow rewind show <session-id> --step N` is a non-interactive version of TUI step inspection. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | TUI must work in any 80×24 terminal; rendered with charmbracelet bubbletea v2 + lipgloss v2 + bubbles v2 (viewport/table) and glamour for JSON/markdown display. |
| NFR-02 | State diffs computed in-process in pure Go (`reflect`/map comparison), without external diff tools. |
| NFR-03 | Fork creates a new SQLite session row in the event-sourced `internal/queue` store; does not copy checkpoint blobs (they remain referenced by original session ID + step). |
| NFR-04 | `--patch` validated as valid JSON via `encoding/json` before fork; malformed JSON returns a clear `error`. |

---

## 9. Technical Design

Time-travel is built directly on the event-sourced state persisted by the bespoke SQLite-backed DAG scheduler in `internal/queue` (GO_MIGRATION_PLAN decision #5). Every workflow node transition is appended as an event row to the single `tag.sqlite3` store (`modernc.org/sqlite`, pure-Go, CGO_ENABLED=0), so any historical checkpoint can be reconstructed by replaying events up to a given step. Rewind, fork, and diff are read-side projections and re-executions over that durable event log; no second datastore is involved.

### 9.1 SQLite changes

SQL DDL below is DB-neutral but targets `modernc.org/sqlite`. Writes go through the single-writer store layer (`internal/store`), which serializes read-modify-write with `gofrs/flock` + `os.Rename` atomic swaps.

```sql
-- Add forked_from column to workflow_sessions:
ALTER TABLE workflow_sessions ADD COLUMN forked_from TEXT;
ALTER TABLE workflow_sessions ADD COLUMN forked_at_step INTEGER;
```

### 9.2 Go core

The `TimeTravel` type lives in `internal/queue` (package `timetravel`). `Checkpointer` is the store-layer interface that reads/writes event-sourced step state; `Graph` is the compiled workflow from PRD-112. State is a `map[string]any` decoded from the persisted JSON event payload; cloning is explicit (marshal/unmarshal) rather than `copy.deepcopy`. Session IDs come from `google/uuid` (or `crypto/rand` hex).

```go
package timetravel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// State is the workflow state dict reconstructed from the event log.
type State = map[string]any

// Checkpointer is implemented by the internal/store event-sourced layer.
type Checkpointer interface {
	// LoadStep replays events up to step and returns the projected state,
	// or (nil, nil) if no checkpoint exists at that step.
	LoadStep(ctx context.Context, sessionID string, step int) (State, error)
}

// Graph is the compiled workflow engine (PRD-112).
type Graph interface {
	Run(ctx context.Context, state State, sessionID string) (State, error)
}

type TimeTravel struct {
	cp    Checkpointer
	graph Graph
}

func New(cp Checkpointer, graph Graph) *TimeTravel {
	return &TimeTravel{cp: cp, graph: graph}
}

// Fork loads the state at step, applies an optional shallow patch, creates a
// new forked session, and resumes execution from that point.
func (tt *TimeTravel) Fork(ctx context.Context, sessionID string, step int, patch State) (string, error) {
	state, err := tt.cp.LoadStep(ctx, sessionID, step)
	if err != nil {
		return "", err
	}
	if state == nil {
		return "", fmt.Errorf("no checkpoint at step %d for session %s", step, sessionID)
	}
	// Explicit clone so we never mutate the replayed projection.
	forked := cloneState(state)
	for k, v := range patch { // shallow merge; conflicting keys overwritten
		forked[k] = v
	}
	forkID := uuid.NewString()[:8]
	// The store records forked_from / forked_at_step when the new session row
	// is created; graph.Run appends fresh events under forkID from step+1.
	if _, err := tt.graph.Run(ctx, forked, forkID); err != nil {
		return "", err
	}
	return forkID, nil
}

// Diff computes the key-level state change between two steps in pure Go.
func (tt *TimeTravel) Diff(ctx context.Context, sessionID string, step1, step2 int) (map[string]Change, error) {
	s1, err := tt.cp.LoadStep(ctx, sessionID, step1)
	if err != nil {
		return nil, err
	}
	s2, err := tt.cp.LoadStep(ctx, sessionID, step2)
	if err != nil {
		return nil, err
	}
	changes := map[string]Change{}
	for k := range union(s1, s2) {
		v1, v2 := s1[k], s2[k]
		if !equalJSON(v1, v2) {
			changes[k] = Change{Before: v1, After: v2}
		}
	}
	return changes, nil
}

type Change struct {
	Before any `json:"before"`
	After  any `json:"after"`
}

func cloneState(s State) State {
	b, _ := json.Marshal(s)
	var out State
	_ = json.Unmarshal(b, &out)
	if out == nil {
		out = State{}
	}
	return out
}

func union(a, b State) map[string]struct{} {
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

// equalJSON compares two arbitrary decoded-JSON values by canonical marshaling.
func equalJSON(a, b any) bool {
	ba, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ba) == string(bb)
}
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Patch injection enabling malicious state | `--patch` only merges top-level keys; deeply nested injection requires access to the fork command |
| Forking re-executing expensive operations | Fork continues from step N, not from START; no re-execution of prior steps |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Table-driven Go tests for `Diff()` correctness on known state pairs and `Fork()` shallow-patch application; benchmarks (`testing.B`) for fork launch + state-render latency |
| Integration | Fork from step 5 against an event-sourced `modernc.org/sqlite` fixture; verify new session runs from step 6 with patched state |
| TUI | bubbletea `teatest` model harness: send key msgs to navigate steps, press `f`, assert fork created |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag workflow rewind <id>` shows all checkpoint steps in TUI |
| AC-02 | `--step N --fork` creates a new session that runs from step N+1 |
| AC-03 | `--patch '{"x": 1}'` applies x=1 to state before fork |
| AC-04 | `--diff 5 10` shows keys changed between steps 5 and 10 |
| AC-05 | Forked session shows `forked_from` in `tag workflow graph list` |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-110 state serialization | Checkpoint load/restore infrastructure |
| PRD-112 graph-based workflow | `Graph.Run()` for fork re-execution |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should forks be limited in depth (fork-of-fork-of-fork)? |
| OQ-02 | Should there be a cost estimate before forking a long workflow? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `TimeTravel.Fork()`, `Diff()`, event-sourced session updates in `internal/queue`/`internal/store` | 2 |
| 2 | bubbletea TUI inspector (navigation, state render, keyboard shortcuts) | 2 |
| 3 | CLI commands, `--patch` validation, `--diff` rendering | 2 |
| 4 | Integration tests, documentation | 1 |

