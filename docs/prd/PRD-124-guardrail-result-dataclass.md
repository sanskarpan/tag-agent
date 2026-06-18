# PRD-124: GuardrailResult Dataclass (`tag guardrail result`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (1-2 days)
**Category:** Security/Guardrails
**Affects:** `guardrail_result.py`
**Depends on:** (foundational — no dependencies)
**Inspired by:** Guardrails AI `ValidationResult`, Pydantic validation errors, Python `dataclasses`, OpenAPI error response schemas

---

## 1. Overview

The TAG guardrail system (PRD-121, PRD-122, PRD-123) requires a shared, typed return type for all guardrail check functions. Without a common dataclass, each guardrail returns a different structure — some return tuples, some return dicts, some raise exceptions — making it impossible to build a composable pipeline that uniformly handles pass/block/sanitize/warn/interrupt decisions.

`GuardrailResult` is a lightweight dataclass that standardizes the return contract of all guardrail implementations. It carries: the action taken (PASS/BLOCK/SANITIZE/WARN/INTERRUPT), the reason string, the originating guardrail name, an optional sanitized text (for SANITIZE action), an optional message (for INTERRUPT action), and metadata for audit logging.

This PRD is intentionally minimal — it defines the shared data structure that all other guardrail PRDs (121-125) depend on. It is a pure library module with no CLI surface and minimal logic.

---

## 2. Problem Statement

### 2.1 No standard guardrail return type

PRD-121 (output) and PRD-122 (input) both need to return a result indicating whether the guardrail passed, blocked, or requested sanitization. Without a shared type, the pipeline composition code must handle heterogeneous return types.

### 2.2 Audit logging requires consistent structure

The `guardrail_events` SQLite table needs to record the action, reason, and guardrail name from every check. A shared dataclass makes this serialization deterministic.

### 2.3 Type safety for pipeline composition

Python type hints on `OutputGuardrailPipeline.process() -> GuardrailResult` provide static analysis benefits — editors and mypy can catch mismatches at development time.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Define `GuardrailAction` enum with values: PASS, BLOCK, SANITIZE, WARN, INTERRUPT. |
| G2 | Define `GuardrailResult` dataclass with fields: `action`, `reason`, `guardrail`, `sanitized_text`, `message`, `metadata`. |
| G3 | Provide `GuardrailResult.is_blocking()` helper that returns True for BLOCK and INTERRUPT actions. |
| G4 | Provide `GuardrailResult.to_dict()` for JSON serialization to the audit log. |
| G5 | Module is importable with zero external dependencies (stdlib only). |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Guardrail logic implementation (belongs in PRD-121/122/123). |
| NG2 | CLI surface. |
| NG3 | SQLite persistence (belongs in the audit log infrastructure of PRD-121). |
| NG4 | Async support. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Import time | < 5ms (no heavy imports) | Benchmark test |
| mypy compatibility | Zero mypy errors on the module | CI check |
| Dataclass immutability | `frozen=True` prevents accidental mutation | Unit test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Guardrail implementor | Return `GuardrailResult(action=GuardrailAction.BLOCK, reason="PII_DETECTED", guardrail="pii")` | I have a typed, consistent return contract |
| US2 | Pipeline developer | Call `result.is_blocking()` to check if execution should halt | I avoid string comparison bugs |
| US3 | Audit log writer | Call `result.to_dict()` to get a JSON-serializable representation | I write to `guardrail_events` without custom serialization |

---

## 6. CLI Surface

None. This is a pure library module.

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `GuardrailAction` is a `str` enum (not IntEnum) for JSON-serializability. |
| FR-02 | `GuardrailResult` is a `frozen=True` dataclass with fields: `action: GuardrailAction`, `reason: str = ""`, `guardrail: str = ""`, `sanitized_text: Optional[str] = None`, `message: Optional[str] = None`, `metadata: dict = field(default_factory=dict)`. |
| FR-03 | `GuardrailResult.is_blocking() -> bool` returns True if `action in (BLOCK, INTERRUPT)`. |
| FR-04 | `GuardrailResult.to_dict() -> dict` returns a JSON-serializable dict of all fields (None values included as null). |
| FR-05 | `GuardrailResult.should_sanitize() -> bool` returns True if `action == SANITIZE and sanitized_text is not None`. |
| FR-06 | Module exports: `GuardrailAction`, `GuardrailResult`. |
| FR-07 | `GuardrailAction.PASS`, `.BLOCK`, `.SANITIZE`, `.WARN`, `.INTERRUPT` are the five values; no others. |
| FR-08 | Default `GuardrailResult` (no arguments except `action=PASS`) is valid and represents a clean pass. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Zero runtime dependencies beyond the Python stdlib (`dataclasses`, `enum`, `typing`). |
| NFR-02 | `frozen=True` on the dataclass for hashability and immutability. |
| NFR-03 | Full mypy strict-mode compatibility. |
| NFR-04 | Module size: < 50 lines of production code. |

---

## 9. Technical Design

### 9.1 Target file

| File | Change |
|------|--------|
| `src/tag/guardrail_result.py` | New module: `GuardrailAction`, `GuardrailResult` |

### 9.2 Implementation

```python
"""Shared result types for all TAG guardrail implementations."""
from __future__ import annotations

import dataclasses
from enum import Enum
from typing import Any, Dict, Optional


class GuardrailAction(str, Enum):
    """The action a guardrail check has decided to take."""
    PASS = "pass"
    BLOCK = "block"
    SANITIZE = "sanitize"
    WARN = "warn"
    INTERRUPT = "interrupt"


@dataclasses.dataclass(frozen=True)
class GuardrailResult:
    """Standardized return type for all guardrail check() methods.

    Attributes:
        action:         What the guardrail decided to do.
        reason:         Machine-readable reason code (e.g. "PII_DETECTED:email").
        guardrail:      Name of the guardrail that produced this result.
        sanitized_text: Replacement text when action is SANITIZE.
        message:        Human-readable message (shown on INTERRUPT).
        metadata:       Optional extra data for audit logging.
    """
    action: GuardrailAction = GuardrailAction.PASS
    reason: str = ""
    guardrail: str = ""
    sanitized_text: Optional[str] = None
    message: Optional[str] = None
    metadata: Dict[str, Any] = dataclasses.field(default_factory=dict)

    def is_blocking(self) -> bool:
        """Return True if this result should stop downstream processing."""
        return self.action in (GuardrailAction.BLOCK, GuardrailAction.INTERRUPT)

    def should_sanitize(self) -> bool:
        """Return True if sanitized_text should replace the original text."""
        return self.action == GuardrailAction.SANITIZE and self.sanitized_text is not None

    def to_dict(self) -> Dict[str, Any]:
        """Return a JSON-serializable dict for audit log storage."""
        return {
            "action": self.action.value,
            "reason": self.reason,
            "guardrail": self.guardrail,
            "sanitized_text": self.sanitized_text,
            "message": self.message,
            "metadata": self.metadata,
        }

    @classmethod
    def pass_result(cls, guardrail: str = "") -> "GuardrailResult":
        """Convenience constructor for a clean pass."""
        return cls(action=GuardrailAction.PASS, guardrail=guardrail)

    @classmethod
    def block_result(cls, reason: str, guardrail: str = "",
                     message: Optional[str] = None) -> "GuardrailResult":
        """Convenience constructor for a block result."""
        return cls(action=GuardrailAction.BLOCK, reason=reason,
                   guardrail=guardrail, message=message)

    @classmethod
    def sanitize_result(cls, sanitized_text: str, reason: str = "",
                        guardrail: str = "") -> "GuardrailResult":
        """Convenience constructor for a sanitize result."""
        return cls(action=GuardrailAction.SANITIZE, reason=reason,
                   guardrail=guardrail, sanitized_text=sanitized_text)

    def __str__(self) -> str:
        parts = [f"GuardrailResult(action={self.action.value}"]
        if self.reason:
            parts.append(f", reason={self.reason!r}")
        if self.guardrail:
            parts.append(f", guardrail={self.guardrail!r}")
        parts.append(")")
        return "".join(parts)
```

---

## 10. Security Considerations

None. This is a pure data structure with no execution logic or external I/O.

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | All five `GuardrailAction` values; `is_blocking()` for each; `should_sanitize()` with and without `sanitized_text`; `to_dict()` round-trip; `frozen=True` immutability |
| Type checking | `mypy --strict src/tag/guardrail_result.py` passes with zero errors |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `GuardrailResult(action=GuardrailAction.BLOCK).is_blocking()` returns `True` |
| AC-02 | `GuardrailResult(action=GuardrailAction.PASS).is_blocking()` returns `False` |
| AC-03 | `GuardrailResult(action=GuardrailAction.SANITIZE, sanitized_text="x").should_sanitize()` returns `True` |
| AC-04 | `result.to_dict()` returns a JSON-serializable dict with all 6 fields |
| AC-05 | Attempting to mutate a frozen `GuardrailResult` raises `FrozenInstanceError` |
| AC-06 | `from tag.guardrail_result import GuardrailAction, GuardrailResult` succeeds with no errors |

---

## 13. Dependencies

None. This is a foundational library module.

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should `GuardrailResult` support chaining (multiple guardrails in a composite result)? |
| OQ-02 | Should metadata include a severity level for UI rendering? |

---

## 15. Complexity & Timeline

**Complexity:** Trivial (XS)
**Estimated effort:** 1–2 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `GuardrailAction` enum, `GuardrailResult` dataclass, unit tests | 1 |
| 2 | Type checking, review, import in PRD-121/122/123 | 0.5 |
