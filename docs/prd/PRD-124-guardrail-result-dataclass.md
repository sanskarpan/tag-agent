# PRD-124: GuardrailResult Type (`tag guardrail result`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (1-2 days)
**Category:** Security/Guardrails
**Affects:** `internal/runtime/guardrail/result.go`
**Depends on:** (foundational — no dependencies)
**Inspired by:** Guardrails AI `ValidationResult`, OpenAPI error response schemas; Go idioms: value structs + string-typed enums + `encoding/json`

---

## 1. Overview

The TAG guardrail system (PRD-121, PRD-122, PRD-123) requires a shared, typed return type for all guardrail check functions. Without a common type, each guardrail returns a different structure — some return tuples, some return errors, some return maps — making it impossible to build a composable pipeline that uniformly handles pass/block/sanitize/warn/interrupt decisions.

`GuardrailResult` is a lightweight Go value struct that standardizes the return contract of all guardrail implementations. It carries: the action taken (PASS/BLOCK/SANITIZE/WARN/INTERRUPT), the reason string, the originating guardrail name, an optional sanitized text (for SANITIZE action), an optional message (for INTERRUPT action), and metadata for audit logging.

This PRD is intentionally minimal — it defines the shared data structure that all other guardrail PRDs (121-125) depend on. It is a pure package (`internal/runtime/guardrail`) with no CLI surface and minimal logic, imported by the guardrail middleware that wraps the agent loop.

---

## 2. Problem Statement

### 2.1 No standard guardrail return type

PRD-121 (output) and PRD-122 (input) both need to return a result indicating whether the guardrail passed, blocked, or requested sanitization. Without a shared type, the pipeline composition code must handle heterogeneous return types.

### 2.2 Audit logging requires consistent structure

The `guardrail_events` SQLite table needs to record the action, reason, and guardrail name from every check. A shared struct with JSON tags makes this serialization deterministic.

### 2.3 Type safety for pipeline composition

A Go interface `OutputGuardrail.Process(...) GuardrailResult` gives the compiler-enforced contract for free — mismatches are caught at build time by the Go compiler and `go vet`/`staticcheck`, not at runtime.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Define `GuardrailAction` as a string-typed const set with values: PASS, BLOCK, SANITIZE, WARN, INTERRUPT. |
| G2 | Define `GuardrailResult` struct with fields: `Action`, `Reason`, `Guardrail`, `SanitizedText`, `Message`, `Metadata`. |
| G3 | Provide `GuardrailResult.IsBlocking() bool` helper that returns true for BLOCK and INTERRUPT actions. |
| G4 | Marshal to JSON for the audit log via `encoding/json` struct tags (no custom serializer). |
| G5 | Package imports the Go stdlib only (`encoding/json`); zero external module dependencies. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Guardrail logic implementation (belongs in PRD-121/122/123). |
| NG2 | CLI surface. |
| NG3 | SQLite persistence (belongs in the audit log infrastructure of PRD-121). |
| NG4 | `context.Context` plumbing / concurrency primitives (results are plain values; callers own goroutines). |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Import weight | stdlib-only (`encoding/json`); no external modules | `go mod graph` check |
| Vet cleanliness | Zero `go vet`/`staticcheck` findings on the package | CI check |
| Value semantics | Passed/returned by value; no shared mutable state across guardrails | Unit test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Guardrail implementor | Return `guardrail.Block("PII_DETECTED:email", "pii")` | I have a typed, consistent return contract |
| US2 | Pipeline developer | Call `result.IsBlocking()` to check if execution should halt | I avoid string comparison bugs |
| US3 | Audit log writer | `json.Marshal(result)` to get a serializable representation | I write to `guardrail_events` without custom serialization |

---

## 6. CLI Surface

None. This is a pure library module.

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `GuardrailAction` is defined as `type GuardrailAction string` (string-backed for JSON-serializability), not an integer iota. |
| FR-02 | `GuardrailResult` is a struct with JSON-tagged fields: `Action GuardrailAction`, `Reason string`, `Guardrail string`, `SanitizedText *string` (omitempty), `Message *string` (omitempty), `Metadata map[string]any`. |
| FR-03 | `GuardrailResult.IsBlocking() bool` returns true if `Action` is `ActionBlock` or `ActionInterrupt`. |
| FR-04 | `GuardrailResult` marshals via `encoding/json` to an object with all fields; nil pointers serialize as `null` (or omitted where `omitempty` is set). |
| FR-05 | `GuardrailResult.ShouldSanitize() bool` returns true if `Action == ActionSanitize && SanitizedText != nil`. |
| FR-06 | Package `guardrail` exports: `GuardrailAction`, `GuardrailResult`, the five action consts, and the convenience constructors. |
| FR-07 | `ActionPass`, `ActionBlock`, `ActionSanitize`, `ActionWarn`, `ActionInterrupt` are the five values; no others. |
| FR-08 | The zero value of `GuardrailResult` has `Action == ""`; a clean pass is constructed via `Pass(guardrail)` (or by explicitly setting `ActionPass`). |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Zero external module dependencies; Go stdlib only (`encoding/json`). |
| NFR-02 | Value semantics: `GuardrailResult` is passed and returned by value; guardrails never share a mutable result. (`Metadata` is a map — treat as read-only after construction; constructors allocate a fresh map.) |
| NFR-03 | Zero `go vet` / `staticcheck` findings under the project lint config. |
| NFR-04 | Package size: < 80 lines of production code. |

---

## 9. Technical Design

### 9.1 Target file

| File | Change |
|------|--------|
| `internal/runtime/guardrail/result.go` | New package `guardrail`: `GuardrailAction`, `GuardrailResult`, constructors |

### 9.2 Implementation

```go
// Package guardrail defines the shared result contract returned by every
// TAG guardrail implementation (input/output/runtime/constitutional).
package guardrail

import "fmt"

// GuardrailAction is the action a guardrail check has decided to take.
// String-backed so it JSON-marshals to a stable wire value.
type GuardrailAction string

const (
	ActionPass      GuardrailAction = "pass"
	ActionBlock     GuardrailAction = "block"
	ActionSanitize  GuardrailAction = "sanitize"
	ActionWarn      GuardrailAction = "warn"
	ActionInterrupt GuardrailAction = "interrupt"
)

// GuardrailResult is the standardized return type for all guardrail checks.
// It is passed and returned BY VALUE; guardrails never share a mutable result.
type GuardrailResult struct {
	Action        GuardrailAction `json:"action"`
	Reason        string          `json:"reason"`
	Guardrail     string          `json:"guardrail"`
	SanitizedText *string         `json:"sanitized_text"` // set only when Action == ActionSanitize
	Message       *string         `json:"message"`        // human-readable, shown on ActionInterrupt
	Metadata      map[string]any  `json:"metadata,omitempty"`
}

// IsBlocking reports whether this result should stop downstream processing.
func (r GuardrailResult) IsBlocking() bool {
	return r.Action == ActionBlock || r.Action == ActionInterrupt
}

// ShouldSanitize reports whether SanitizedText should replace the original text.
func (r GuardrailResult) ShouldSanitize() bool {
	return r.Action == ActionSanitize && r.SanitizedText != nil
}

// Pass is the convenience constructor for a clean pass.
func Pass(guardrail string) GuardrailResult {
	return GuardrailResult{Action: ActionPass, Guardrail: guardrail}
}

// Block is the convenience constructor for a block result.
func Block(reason, guardrail string, message ...string) GuardrailResult {
	r := GuardrailResult{Action: ActionBlock, Reason: reason, Guardrail: guardrail}
	if len(message) > 0 {
		r.Message = &message[0]
	}
	return r
}

// Sanitize is the convenience constructor for a sanitize result.
func Sanitize(sanitized, reason, guardrail string) GuardrailResult {
	return GuardrailResult{
		Action: ActionSanitize, Reason: reason, Guardrail: guardrail, SanitizedText: &sanitized,
	}
}

// String implements fmt.Stringer for log-friendly output.
func (r GuardrailResult) String() string {
	return fmt.Sprintf("GuardrailResult(action=%s, reason=%q, guardrail=%q)", r.Action, r.Reason, r.Guardrail)
}
```

`encoding/json` handles audit-log serialization directly (`json.Marshal(result)`) using the struct tags — no `to_dict()` equivalent is needed. Downstream guardrail interfaces (PRD-121/122/123) return this value type; a Go interface such as `type OutputGuardrail interface { Process(ctx context.Context, text string) GuardrailResult }` gives compiler-checked composition.

---

## 10. Security Considerations

None. This is a pure data structure with no execution logic or external I/O.

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Table-driven test over all five `GuardrailAction` values; `IsBlocking()` for each; `ShouldSanitize()` with and without `SanitizedText`; `json.Marshal`/`Unmarshal` round-trip |
| Static analysis | `go vet ./internal/runtime/guardrail/...` and `staticcheck` pass with zero findings |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `GuardrailResult{Action: ActionBlock}.IsBlocking()` returns `true` |
| AC-02 | `GuardrailResult{Action: ActionPass}.IsBlocking()` returns `false` |
| AC-03 | `Sanitize("x", "", "").ShouldSanitize()` returns `true` |
| AC-04 | `json.Marshal(result)` produces an object with all six fields |
| AC-05 | Passing a `GuardrailResult` by value into a guardrail does not mutate the caller's copy (value semantics) |
| AC-06 | Package `github.com/tag-agent/tag/internal/runtime/guardrail` builds and exports `GuardrailAction`, `GuardrailResult`, the five action consts |

---

## 13. Dependencies

None. This is a foundational library module.

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should `GuardrailResult` support chaining (multiple guardrails in a composite result)? |
| OQ-02 | Should `Metadata` include a severity level for UI rendering? |
| OQ-03 | Should the zero value (`Action == ""`) be treated as an implicit pass, or should callers always use the `Pass()` constructor (current spec: use the constructor)? |

---

## 15. Complexity & Timeline

**Complexity:** Trivial (XS)
**Estimated effort:** 1–2 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `GuardrailAction` consts, `GuardrailResult` struct + constructors, table-driven unit tests | 1 |
| 2 | `go vet`/`staticcheck`, review, import in PRD-121/122/123 | 0.5 |

