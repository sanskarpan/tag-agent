# PRD-121: Output Guardrail Processor (`tag guardrail output`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Security/Guardrails
**Affects:** `guardrails.py + controller.py`
**Depends on:** PRD-124 (GuardrailResult dataclass), PRD-034 (secret scanning), PRD-013 (agent tracing — span instrumentation)
**Inspired by:** Guardrails AI output validators, LlamaGuard, Nemo Guardrails output rails, AWS Bedrock Guardrails

---

## 1. Overview

Agent outputs can contain harmful, policy-violating, or sensitive content: generated code that is insecure, outputs that contain PII or credentials, responses that violate business rules, or content that fails safety classifiers. Without a structured post-processing layer, these outputs flow directly to downstream consumers — users, databases, APIs — without any opportunity for detection or remediation.

Output Guardrail Processor (`tag guardrail output`) introduces a composable output validation pipeline: a chain of `OutputGuardrail` implementations that inspect every agent output before it is returned to the caller. Each guardrail is a typed Python class that receives the output string and returns a `GuardrailResult` (PRD-124) indicating pass/block/rewrite. The pipeline short-circuits on any block result and can optionally rewrite the output using a remediation model.

The design is inspired by Guardrails AI's output validators (regex, PII detection, JSON schema validation, topic filtering), AWS Bedrock Guardrails' output filters (profanity, PII, topic denial, grounding), and Nemo Guardrails' output rails (fact-checking, output filtering flows). TAG's implementation is local-first and extensible: built-in guardrails cover the most common cases, and custom guardrails can be added as Python classes.

---

## 2. Problem Statement

### 2.1 No structured output validation

Agent outputs pass directly from the model API to the caller. There is no interception point for validating output content, detecting policy violations, or remediating unsafe outputs.

### 2.2 PII and secret leakage in outputs

Models sometimes hallucinate or echo PII and secrets in outputs. PRD-034 scans inputs; there is no equivalent for outputs.

### 2.3 No schema validation for structured outputs

When an agent is expected to return JSON, there is no automatic validation that the returned JSON conforms to the expected schema — leading to downstream parsing errors.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `OutputGuardrailPipeline` executes a list of `OutputGuardrail` instances in order, short-circuiting on the first `block` result. |
| G2 | Built-in output guardrails: PII detector, secret scanner, JSON schema validator, topic filter, profanity filter, toxicity classifier. |
| G3 | `GuardrailResult.action == "rewrite"` triggers an LLM-based remediation call to fix the output before returning. |
| G4 | All guardrail decisions logged to the `guardrail_events` SQLite table for auditability. |
| G5 | `tag guardrail output list` shows all configured output guardrails for a profile. |
| G6 | `tag guardrail output test --input TEXT` dry-runs the output pipeline against a test string. |
| G7 | Output guardrail pipeline integrable with `cmd_run` in `controller.py` as a post-processing hook. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Real-time streaming output validation (validates full output only). |
| NG2 | Input guardrails (PRD-122). |
| NG3 | Runtime/tool-call guardrails (PRD-123). |
| NG4 | Custom guardrail GUI or no-code configuration. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Pipeline latency (no LLM guardrails) | < 50ms for a 1000-token output | Benchmark test |
| PII detection recall | > 90% on standard PII benchmark (names, emails, phone numbers) | Eval test |
| Secret detection precision | > 95% precision on secret scanner (no false positives on non-credential text) | Eval test |
| JSON schema validation accuracy | 100% correct pass/fail on 20 curated schema+output pairs | Unit test |
| Audit log completeness | 100% of guardrail decisions written to SQLite | Integration test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Platform engineer | Block any output containing PII before it reaches the user | I comply with data privacy requirements |
| US2 | Developer | Validate that JSON outputs conform to a schema | I catch malformed structured outputs before they cause downstream errors |
| US3 | Security engineer | Block outputs containing API keys or tokens | I prevent credential leakage through the model |
| US4 | Developer | Rewrite policy-violating outputs using a remediation model | I get a safe output instead of a blocked response |
| US5 | Compliance engineer | Audit all guardrail decisions for a profile | I demonstrate compliance to reviewers |

---

## 6. CLI Surface

```
tag guardrail output <subcommand> [options]

Subcommands:
  list       List configured output guardrails for a profile
  add        Add an output guardrail to a profile
  remove     Remove an output guardrail from a profile
  test       Test the output pipeline against a string
  history    Show recent guardrail decision log

tag guardrail output add \
  --profile default \
  --type pii|secret|json-schema|topic-filter|profanity|toxicity|custom \
  --action block|rewrite|warn \
  [--schema PATH]              # for json-schema type
  [--topics "politics,violence"]  # for topic-filter type
  [--remediation-model MODEL]  # for rewrite action
  [--severity high|medium|low]

tag guardrail output test \
  --profile default \
  --input "My email is user@example.com and SSN is 123-45-6789"

tag guardrail output list [--profile PROFILE]
tag guardrail output history [--profile PROFILE] [--since 7d] [--action block|warn|pass]

Options:
  --type          Guardrail type
  --action        On-violation action: block (stop), rewrite (fix), warn (log only)
  --severity      Guardrail severity for priority ordering
  --profile       Target profile
  --remediation-model  Model for rewrite action (default: claude-haiku-4-5)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `OutputGuardrailPipeline.process(output: str) -> GuardrailResult` executes guardrails in priority order; returns first non-pass result or final pass. |
| FR-02 | PII guardrail: detect names (NER), emails (regex), phone numbers (regex), SSNs (regex), credit card numbers (Luhn check); return `block` with `PII_DETECTED` reason. |
| FR-03 | Secret guardrail: reuse PRD-034 `SecretScanner`; return `block` with `SECRET_DETECTED` reason. |
| FR-04 | JSON schema guardrail: parse output as JSON; validate against provided JSON Schema (jsonschema library); return `block` with `SCHEMA_INVALID` reason on failure. |
| FR-05 | Topic filter guardrail: compute embedding cosine similarity between output and each forbidden topic embedding; block if similarity > threshold. |
| FR-06 | Rewrite action: call remediation model with `"Rewrite the following to comply with policy: {output}"` system prompt; return the rewritten output. |
| FR-07 | All guardrail decisions written to `guardrail_events` table: profile, guardrail_type, action, reason, input_hash, output_hash, timestamp. |
| FR-08 | `cmd_run` in `controller.py` calls `OutputGuardrailPipeline.process(output)` after each agent call; if `blocked`, return a sanitized error message to the caller. |
| FR-09 | `tag guardrail output test` runs the pipeline on the provided `--input` and prints each guardrail's result (pass/block/rewrite) with reason. |
| FR-10 | `tag guardrail output add` persists a new `output_guardrail_configs` SQLite row; `list` queries and renders it. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Non-LLM guardrails (regex, schema) must complete in < 20ms per output. |
| NFR-02 | LLM-based guardrails (toxicity classifier, topic filter with embedding) must complete in < 2s. |
| NFR-03 | PII detection must not require network calls (local regex/NER only). |
| NFR-04 | Guardrail pipeline is thread-safe; concurrent `cmd_run` calls do not produce corrupted audit log entries. |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS output_guardrail_configs (
  id              TEXT PRIMARY KEY,
  profile         TEXT NOT NULL,
  guardrail_type  TEXT NOT NULL,
  action          TEXT NOT NULL DEFAULT 'block',
  config_json     TEXT,  -- type-specific config (schema, topics, threshold)
  severity        TEXT NOT NULL DEFAULT 'high',
  enabled         INTEGER NOT NULL DEFAULT 1,
  remediation_model TEXT,
  created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS guardrail_events (
  id              TEXT PRIMARY KEY,
  profile         TEXT,
  direction       TEXT NOT NULL DEFAULT 'output',  -- 'input'|'output'|'runtime'
  guardrail_type  TEXT NOT NULL,
  action          TEXT NOT NULL,
  reason          TEXT,
  run_id          TEXT,
  input_hash      TEXT,
  created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_guardrail_events_profile
  ON guardrail_events(profile, created_at DESC);
```

### 9.2 Python core

```python
from __future__ import annotations
import dataclasses
import re
from typing import List, Optional
from tag.guardrail_result import GuardrailResult, GuardrailAction

@dataclasses.dataclass
class OutputGuardrail:
    guardrail_type: str
    action: GuardrailAction
    config: dict

    def check(self, output: str) -> GuardrailResult:
        raise NotImplementedError

class PIIGuardrail(OutputGuardrail):
    EMAIL_RE = re.compile(r"[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}")
    SSN_RE = re.compile(r"\b\d{3}-\d{2}-\d{4}\b")
    PHONE_RE = re.compile(r"\b(\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]\d{3}[-.\s]\d{4}\b")

    def check(self, output: str) -> GuardrailResult:
        for pattern_name, pattern in [("email", self.EMAIL_RE), ("SSN", self.SSN_RE), ("phone", self.PHONE_RE)]:
            if pattern.search(output):
                return GuardrailResult(action=self.action, reason=f"PII_DETECTED:{pattern_name}", guardrail=self.guardrail_type)
        return GuardrailResult(action=GuardrailAction.PASS, guardrail=self.guardrail_type)

class SecretGuardrail(OutputGuardrail):
    def check(self, output: str) -> GuardrailResult:
        from tag.secret_scanner import SecretScanner
        findings = SecretScanner().scan_text(output)
        if findings:
            return GuardrailResult(action=self.action, reason=f"SECRET_DETECTED:{findings[0].type}", guardrail=self.guardrail_type)
        return GuardrailResult(action=GuardrailAction.PASS, guardrail=self.guardrail_type)

class JSONSchemaGuardrail(OutputGuardrail):
    def check(self, output: str) -> GuardrailResult:
        import json
        try:
            import jsonschema
            obj = json.loads(output.strip())
            jsonschema.validate(obj, self.config.get("schema", {}))
            return GuardrailResult(action=GuardrailAction.PASS, guardrail=self.guardrail_type)
        except (json.JSONDecodeError, Exception) as e:
            return GuardrailResult(action=self.action, reason=f"SCHEMA_INVALID:{str(e)[:100]}", guardrail=self.guardrail_type)

class OutputGuardrailPipeline:
    def __init__(self, guardrails: List[OutputGuardrail]) -> None:
        self.guardrails = sorted(guardrails, key=lambda g: {"high": 0, "medium": 1, "low": 2}.get(getattr(g, "severity", "high"), 1))

    def process(self, output: str, run_id: Optional[str] = None) -> GuardrailResult:
        for guardrail in self.guardrails:
            result = guardrail.check(output)
            if result.action != GuardrailAction.PASS:
                return result
        return GuardrailResult(action=GuardrailAction.PASS, guardrail="pipeline")
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Bypass via output encoding (base64, unicode escape) | Decode common encodings before PII/secret scan |
| Rewrite model introducing new violations | Run output guardrails on rewritten output before returning |
| Guardrail audit log manipulation | `guardrail_events` rows never updated after insert; append-only |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `PIIGuardrail` email/SSN/phone detection; `JSONSchemaGuardrail` pass/fail on known pairs |
| Integration | Full pipeline: PII in output → block → audit log entry |
| Security | Encoded PII detection; rewrite output re-validated |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | Output with email address is blocked by `PIIGuardrail` with `block` action |
| AC-02 | Output with `sk-...` API key is blocked by `SecretGuardrail` |
| AC-03 | Invalid JSON output is blocked by `JSONSchemaGuardrail` when schema configured |
| AC-04 | All block events written to `guardrail_events` table |
| AC-05 | `tag guardrail output test --input "email@test.com"` prints `BLOCK:PII_DETECTED:email` |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-124 GuardrailResult dataclass | Shared result type |
| PRD-034 secret scanning | `SecretScanner` reuse |
| `jsonschema` (optional) | JSON schema validation |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should guardrail results be included in OTel spans for observability? |
| OQ-02 | Should there be a "soft block" mode that logs but does not block (for gradual rollout)? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `GuardrailResult`, `PIIGuardrail`, `SecretGuardrail`, unit tests | 2 |
| 2 | `JSONSchemaGuardrail`, `OutputGuardrailPipeline`, audit log | 2 |
| 3 | CLI commands, rewrite action, `cmd_run` integration | 2 |
| 4 | Integration tests, documentation | 1 |

