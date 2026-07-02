# PRD-109: HITL interrupt()+Command(resume=) (`tag workflow interrupt`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Workflow State
**Affects:** `internal/agent (interrupt flag) + internal/runtime + internal/store + internal/server + internal/cli`
**Depends on:** PRD-110 (state serialization/checkpointing), PRD-112 (graph-based workflow), PRD-082 (multi-agent team primitives)
**Inspired by:** LangGraph interrupt()+Command(resume=), CrewAI human input, AutoGen human proxy agent, OpenAI Swarm handoff

---

## 1. Overview

Fully autonomous agents are unsuitable for high-stakes decisions: deleting files, making purchases, sending emails, deploying to production. TAG's current execution model runs agents to completion without any mechanism to pause mid-execution and request human approval or input. When an agent reaches a decision point requiring human judgment, the entire run must be aborted and restarted manually.

Human-in-the-Loop interrupt (`tag workflow interrupt`) introduces a first-class pause mechanism modeled after LangGraph's `interrupt()` / `Command(resume=)` pattern. When an agent workflow reaches an interrupt point, execution is suspended, state is checkpointed (PRD-110), and the operator is prompted for input. On resume (`tag workflow resume <session-id> --input "approved"`), the workflow continues exactly from the interrupt point with the human's input injected into the agent's context — no replay, no restart.

The design is directly inspired by LangGraph's 2024 `interrupt()` primitive (which pauses graph execution and stores the interrupt value in the checkpoint), LangGraph's `Command(resume=value)` pattern (which passes the human response back into the graph), and CrewAI's `human_input=True` task parameter. Unlike those frameworks, TAG's implementation works with any workflow graph and persists interrupt state to SQLite for durability across process restarts.

---

## 2. Problem Statement

### 2.1 No pause mechanism for approval gates

Production agent workflows need approval gates: "before calling this tool, confirm with a human." Currently, TAG has no mechanism to pause mid-execution at a defined point and wait for human input before continuing.

### 2.2 Interrupted runs lose all progress

If an engineer manually kills a TAG process mid-run (e.g., to review intermediate output), all state is lost and the run must restart from the beginning — re-executing all prior steps and incurring their cost.

### 2.3 No structured human input injection

Even when engineers manually inspect runs, there is no mechanism to inject a correction or approval decision back into the running agent. Human feedback must be re-fed as a new prompt in a new run.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Provide an `interrupt(question, context)` function that workflow nodes can call to pause execution and prompt the operator. |
| G2 | On interrupt: checkpoint all current state to SQLite (PRD-110), print the interrupt question and context to the terminal, and exit the workflow process. |
| G3 | `tag workflow resume <session-id>` resumes execution from the checkpoint with the operator's input available via `get_interrupt_response()`. |
| G4 | Support `--auto-approve` mode for CI/CD pipelines that should not block on human input. |
| G5 | Support timeout-based auto-escalation: if no response within N seconds, trigger the escalation handler (e.g., abort or skip). |
| G6 | Multiple consecutive interrupts in a single workflow session (approval gates at multiple decision points). |
| G7 | Interrupt state visible in `tag workflow list` as status `interrupted` with the pending question shown. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Web-based approval interface. Terminal only. |
| NG2 | Multi-approver consensus (only single operator input). |
| NG3 | Email/Slack-based interrupt delivery. |
| NG4 | Interrupt points in non-workflow runs (only graph-based workflows). |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Resume latency | `tag workflow resume` continues execution in < 500ms after operator input | Benchmark test |
| State fidelity | All prior step outputs and tool call results available after resume | Integration test |
| Interrupt overhead | Adding interrupt() to a node adds < 5ms per step | Benchmark test |
| Auto-approve CI compatibility | `--auto-approve` skips all interrupts and completes workflow without blocking | Integration test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Define an interrupt point before a destructive tool call | A human must approve before files are deleted |
| US2 | Operator | Resume a paused workflow after reviewing the interrupt context | I can approve or reject with my input |
| US3 | CI engineer | Run workflows with `--auto-approve` in CI | Pipelines don't block on human input |
| US4 | Developer | See all interrupted workflows in `tag workflow list` | I know which workflows are waiting for my input |
| US5 | Developer | Set a timeout so interrupted workflows auto-abort after 1 hour | I prevent workflows from waiting indefinitely |

---

## 6. CLI Surface

```
tag workflow interrupt show <session-id>
tag workflow resume <session-id> --input "approved" [--timeout-action abort|skip]
tag workflow list --filter interrupted

# In a workflow node (Go API, internal/agent):
func reviewNode(ctx context.Context, s *workflow.State) (workflow.Update, error) {
    humanInput, err := workflow.Interrupt(ctx, workflow.InterruptRequest{
        Question: "About to delete 47 files. Proceed?",
        Context:  map[string]any{"files": s.FilesToDelete, "count": 47},
    })
    if errors.Is(err, workflow.ErrInterrupt) {
        return workflow.Update{}, err // engine checkpoints + suspends
    }
    switch strings.ToLower(humanInput) {
    case "yes", "y", "approved":
        return workflow.Update{"approved": true}, nil
    default:
        return workflow.Update{"approved": false, "reason": humanInput}, nil
    }
}

Options:
  --input TEXT          Human response to inject at the interrupt point
  --timeout N           Auto-escalate after N seconds (default: 3600)
  --timeout-action      What to do on timeout: abort|skip (default: abort)
  --auto-approve        Automatically approve all interrupts (CI mode)
  --auto-input TEXT     Input to inject in --auto-approve mode (default: "approved")
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `workflow.Interrupt(ctx, req)`: checkpoints current state, writes a `workflow_interrupts` row with status `pending`, and returns the sentinel `ErrInterrupt` so the node returns early and the call stack unwinds via Go error propagation. |
| FR-02 | The workflow engine detects `ErrInterrupt` (via `errors.Is`), serializes state to SQLite (PRD-110), emits the interrupt question + context over the runtime seam to the terminal (rendered by `internal/cli`, optionally the `internal/tui` bubbletea front-end), then returns cleanly (`ctx`-scoped, no leaked goroutines). |
| FR-03 | `tag workflow resume` fetches the checkpoint (PRD-110), stores the operator's `--input` in the `workflow_interrupts` row, and re-executes the graph from the checkpoint. |
| FR-04 | `workflow.Interrupt` returns `(response, nil)` on re-execution: after resume, the same call within the re-run node returns the stored operator input string immediately (no new checkpoint). |
| FR-05 | On resume, the node that called `Interrupt` is re-executed from its beginning (Go nodes are pure `func(ctx, *State) (Update, error)`, not resumable mid-body); `Interrupt` short-circuits with the stored response on that re-execution. |
| FR-06 | `--auto-approve` sets a field on the session's `InterruptGate` config (threaded via `context`, not a package global) that causes `Interrupt` to return the `--auto-input` value immediately without checkpointing or terminal prompt. |
| FR-07 | Timeout: if the `workflow_interrupts` row remains `pending` beyond `--timeout` seconds, a `context.WithTimeout`-driven check (evaluated on the next `resume` poll, per NFR-02) marks it `timed_out` and triggers the `--timeout-action`. |
| FR-08 | `tag workflow list --filter interrupted` queries `workflow_sessions` WHERE status='interrupted' and shows session ID, goal, interrupt question, pending duration. |
| FR-09 | Multiple interrupt points: after resume and re-execution past the first interrupt, subsequent `Interrupt` calls follow the same checkpoint/resume flow. |
| FR-10 | Interrupt context is `encoding/json`-serialized into `workflow_interrupts.context`; `tag workflow interrupt show` renders it formatted. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Checkpoint/resume must preserve exact workflow state via an `encoding/json`-serializable `State` struct (no pickle/gob; JSON keeps checkpoints inspectable and cross-version-safe), persisted to SQLite by PRD-110. |
| NFR-02 | No background goroutine/daemon required for interrupt detection; polling from the CLI on `resume` is sufficient. |
| NFR-03 | Interrupt state must survive TAG process crash; state checkpointed before process exits. |
| NFR-04 | `--auto-approve` must be opt-in via explicit flag; never the default. |

---

## 9. Technical Design

### 9.1 SQLite DDL (`internal/store`, modernc.org/sqlite)

Persisted to the single pure-Go `modernc.org/sqlite` store (CGO_ENABLED=0, WAL); migration lives in `internal/store/migrate/`.

```sql
CREATE TABLE IF NOT EXISTS workflow_interrupts (
  id              TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL,
  step_id         TEXT NOT NULL,
  question        TEXT NOT NULL,
  context         TEXT,          -- JSON
  status          TEXT NOT NULL DEFAULT 'pending',  -- 'pending'|'responded'|'timed_out'
  operator_input  TEXT,
  timeout_s       INTEGER NOT NULL DEFAULT 3600,
  created_at      TEXT NOT NULL,
  responded_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_workflow_interrupts_session
  ON workflow_interrupts(session_id, status);
```

### 9.2 Mechanism — cooperative interrupt over `context`, not exceptions

Go has no exceptions, no `pickle`, and no thread-local globals. The LangGraph `interrupt()`/`Command(resume=)` pattern maps onto the Hermes-style **cooperative interrupt flag + `context.Context` cancellation** already used by the `internal/agent` bounded loop:

- Session-scoped state (current session ID, `--auto-approve`/`--auto-input`, the resume-response lookup, the DB handle) is threaded through the call chain via a typed `context` value (`InterruptGate`) — never a package-level global. This makes it goroutine-safe and testable.
- `Interrupt` returns a sentinel error `ErrInterrupt` instead of raising; the engine unwinds via ordinary Go error propagation (`errors.Is`) rather than stack-unwinding an exception.
- The pending interrupt is delivered to the operator over the `internal/runtime` wire seam. The terminal `internal/cli` client renders the prompt (and, when running under `tag serve`, the same event streams to the bubbletea `internal/tui` client via `tmaxmax/go-sse` on the `net/http`+`chi`/`huma` server). Per NG1 the operator-facing surface is terminal only; SSE is merely the transport to that terminal/TUI client, not a web approval UI.
- On resume, `tag workflow resume` writes the operator input and re-runs the graph; `Interrupt` finds the `responded` row and returns the stored string immediately — the channel-based approval gate degenerates to a fast DB lookup, so no live blocking or background goroutine is needed (NFR-02).

### 9.3 Go core (`internal/agent`, `internal/store`)

```go
package workflow

// ErrInterrupt is the sentinel returned by Interrupt when a new pause is
// recorded; the engine detects it with errors.Is and suspends the session.
var ErrInterrupt = errors.New("workflow interrupted")

// InterruptGate carries per-session interrupt config; threaded via context,
// never a package global.
type InterruptGate struct {
    SessionID   string
    AutoApprove bool
    AutoInput   string   // default "approved"
    Store       *store.DB
}

type InterruptRequest struct {
    Question string         `json:"question"`
    Context  map[string]any `json:"context,omitempty"`
}

func Interrupt(ctx context.Context, req InterruptRequest) (string, error) {
    g, ok := gateFrom(ctx) // typed context-key lookup
    if !ok {
        return "", errors.New("Interrupt called outside a workflow session")
    }
    if g.AutoApprove {
        return g.AutoInput, nil // FR-06: no checkpoint, no prompt
    }
    // Deterministic step id (hash of node + call ordinal) so a resumed re-run
    // matches the same interrupt row.
    stepID := stepIDFrom(ctx)

    // Resume path: this interrupt already answered?
    if in, err := g.Store.RespondedInput(ctx, g.SessionID, stepID); err == nil && in.Valid {
        return in.String, nil // FR-04/FR-05
    }

    // New interrupt — persist pending row (single-writer store) and signal suspend.
    ctxJSON, _ := json.Marshal(req.Context)
    if err := g.Store.InsertInterrupt(ctx, store.Interrupt{
        ID: newID(), SessionID: g.SessionID, StepID: stepID,
        Question: req.Question, Context: ctxJSON,
        Status: "pending", CreatedAt: time.Now().UTC(),
    }); err != nil {
        return "", err
    }
    return "", ErrInterrupt // FR-01
}
```

The engine wraps node execution:

```go
if update, err := node(ctx, state); err != nil {
    if errors.Is(err, ErrInterrupt) {
        checkpoint(ctx, session, state) // PRD-110, JSON state -> SQLite
        emitPrompt(ctx, session)        // over runtime seam -> terminal / TUI
        return nil                      // clean return; ctx-scoped, no leaked goroutines
    }
    return err
}
```

All writes go through the single-writer `internal/store` (`modernc.org/sqlite`, flock + atomic RMW); the workflow engine never opens `tag.sqlite3` directly.

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Auto-approve bypassing safety gates | `--auto-approve` requires explicit flag; warn prominently in docs |
| Operator input injection (prompt injection via terminal) | Operator input is passed as raw string, not interpolated into system prompt |
| Long-lived interrupted sessions accumulating sensitive state | Implement TTL on interrupt records (default 7 days) |

---

## 11. Testing Strategy

Go `testing` (table-driven); `go test -race` on any path where the gate is read across goroutines; `testing.B` for the resume-latency and per-node overhead metrics. Store-backed tests run against a tmp/in-memory `modernc.org/sqlite` DB.

| Layer | Tests |
|-------|-------|
| Unit | Table-driven: `Interrupt` returns `ErrInterrupt` (assert `errors.Is`) on a new pause; returns `(stored, nil)` on the responded re-run; `--auto-approve` returns `AutoInput` immediately with no row written; deterministic `stepID` stable across re-execution |
| Integration | Full workflow over a stub graph: interrupt → checkpoint (PRD-110 JSON state) → `resume --input` → continuation; assert prior node state preserved; multiple sequential interrupts (FR-09) |
| Concurrency | `-race` over a session whose gate is read from node goroutines; assert `ErrInterrupt` return leaks no goroutines and honors `ctx` cancellation |
| Security | Assert `--auto-approve` defaults false (config field, not global); `testing.B`/table test for timeout escalation evaluated on resume poll |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | Workflow with `interrupt()` pauses, prints question, and exits cleanly |
| AC-02 | `tag workflow resume <id> --input "yes"` continues from interrupt point with "yes" as the response |
| AC-03 | All prior step state is available after resume |
| AC-04 | `tag workflow list --filter interrupted` shows the paused workflow with the pending question |
| AC-05 | `--auto-approve` completes the workflow without pausing |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-110 state serialization | JSON checkpoint/restore for suspend/resume |
| PRD-112 graph-based workflow | Workflow node execution framework (`internal/agent`) |
| `internal/store` (`modernc.org/sqlite`) | `workflow_interrupts` persistence, single-writer contract, WAL |
| `internal/runtime` seam + `internal/cli`/`internal/tui` | Deliver the interrupt prompt to the terminal / bubbletea client |
| `net/http`+`go-chi/chi` + `tmaxmax/go-sse` (huma) | SSE transport of the interrupt prompt to the client under `tag serve` (NG1: terminal-only surface) |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should `interrupt()` support structured approval forms (multiple fields) or only free-text input? |
| OQ-02 | Should interrupted workflows appear in a TUI dashboard for easier review? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `workflow.Interrupt` / `ErrInterrupt` sentinel / `InterruptGate` context value / `internal/store` DDL + migration | 1 |
| 2 | Checkpoint integration (PRD-110 JSON state), resume re-execution + deterministic `stepID`, prompt emit over runtime seam | 2 |
| 3 | `internal/cli` commands, `--auto-approve`, timeout handling (resume-poll) | 2 |
| 4 | Table-driven + `-race` + integration tests, documentation | 1 |

