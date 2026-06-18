# PRD-125: Constitutional AI Policy (`tag constitutional`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Security/Guardrails
**Affects:** `constitutional_ai.py + controller.py`
**Depends on:** PRD-124 (GuardrailResult dataclass), PRD-121 (output guardrail processor), PRD-122 (input guardrail validator)
**Inspired by:** Anthropic Constitutional AI (CAI), OpenAI policy spec, Guardrails AI validators, Nemo Guardrails policy engine

---

## 1. Overview

TAG's guardrail system (PRD-121, PRD-122, PRD-123) provides rule-based and classifier-based safety checks. However, rule lists and regex patterns cannot express nuanced behavioral policies: "always be honest even when inconvenient," "never provide instructions that could harm a minor," "respect user autonomy in personal choices." These require a different approach — one where the policy itself is expressed in natural language and evaluated by a critique model.

Constitutional AI Policy (`tag constitutional`) introduces a policy-as-text system modeled after Anthropic's Constitutional AI research: a set of natural-language principles (a "constitution") that the agent must follow. For each agent output, a lightweight critique model evaluates whether the output complies with the constitution and revises it if necessary. The critique-revision loop runs automatically before the output reaches the caller, transforming policy-violating outputs into policy-compliant ones.

The design is directly inspired by Anthropic's CAI paper (2022): a constitution of 16 principles from the UN Declaration of Human Rights, nonviolence principles, etc. used for self-critique and revision. TAG's implementation allows operators to define custom constitutions appropriate for their domain (medical, legal, financial, educational) and to configure the critique model, revision model, max revision passes, and fallback behavior when revision fails.

This is the capstone of TAG's Security/Guardrails cluster (Cluster J) — it provides the highest-level, most expressive policy enforcement layer, sitting above all rule-based guardrails and complementing them with judgment-based evaluation.

---

## 2. Problem Statement

### 2.1 Rule-based guardrails cannot encode nuanced values

A regex can detect "output contains the word 'bomb'" but cannot distinguish "how to build a bomb" (harmful) from "the atomic bomb was dropped in 1945" (historical fact). Constitutional AI's critique-revision approach handles nuanced cases that rules cannot.

### 2.2 No domain-specific policy customization

TAG agents are deployed in diverse domains: medical diagnosis assistance, legal research, financial advising, education. Each domain has different behavioral requirements that cannot be expressed with a universal rule set. A customizable constitution allows domain-specific policy.

### 2.3 No self-improvement mechanism for policy compliance

Rule-based guardrails block outputs without improvement. Constitutional AI's revision step transforms a policy-violating output into a compliant one — preserving the useful information while correcting the problematic element.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `ConstitutionalPolicy` loads a set of principles from a YAML/JSON file and makes them available for critique evaluation. |
| G2 | `CritiqueRevisionLoop.evaluate(output: str, policy: ConstitutionalPolicy) -> GuardrailResult` runs the critique-revision cycle. |
| G3 | Support configurable max revision passes (default: 2) to bound computational cost. |
| G4 | `tag constitutional add-principle --text TEXT --profile PROFILE` adds a principle to the active policy. |
| G5 | `tag constitutional list` shows all active principles for a profile. |
| G6 | `tag constitutional test --input TEXT` runs the critique-revision loop against a test string. |
| G7 | Built-in constitutional templates: `default` (Anthropic-inspired), `medical`, `educational`, `financial`, `legal`. |
| G8 | Critique model and revision model are configurable (default: `claude-haiku-4-5` for critique, same for revision). |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Fine-tuning a model on the constitution. Inference-time critique only. |
| NG2 | Multi-turn constitutional conversations. Single output evaluation only. |
| NG3 | Formal verification of policy compliance. LLM-based best-effort only. |
| NG4 | Distributed policy governance or multi-stakeholder policy management. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Critique accuracy | Critique correctly identifies policy violations in 90%+ of 20 curated test cases | Eval test |
| Revision quality | Revised outputs score higher on policy compliance than original in 85%+ of cases | Eval test |
| Critique latency | Single critique pass completes in < 3s with claude-haiku-4-5 | Benchmark test |
| False positive rate | < 10% of policy-compliant outputs incorrectly flagged as violating | Eval test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Platform engineer | Define a custom constitution for my medical AI deployment | Agent outputs comply with medical ethics principles |
| US2 | Developer | Have policy-violating outputs automatically revised before reaching users | I get useful outputs instead of blocked responses |
| US3 | Compliance officer | See the critique and revision history for auditing | I can demonstrate policy compliance |
| US4 | Developer | Use the built-in educational constitution | I deploy an educational agent with appropriate content guardrails without writing my own policy |

---

## 6. CLI Surface

```
tag constitutional <subcommand> [options]

Subcommands:
  list           List active principles for a profile
  add-principle  Add a principle to the policy
  remove         Remove a principle
  load-template  Load a built-in constitutional template
  test           Test the critique-revision loop against a string
  history        Show critique-revision history

tag constitutional load-template \
  --template default|medical|educational|financial|legal \
  --profile default

tag constitutional add-principle \
  --text "Always provide balanced perspectives on controversial topics" \
  --profile default \
  [--category safety|honesty|harm-avoidance|autonomy]

tag constitutional test \
  --profile default \
  --input "Here is how to hack into a system..." \
  [--max-passes 2] \
  [--model claude-haiku-4-5]

tag constitutional list [--profile PROFILE]
tag constitutional history [--profile PROFILE] [--since 7d]

Options:
  --template       Built-in template name
  --text TEXT      Principle text (natural language)
  --category       Principle category for organization
  --max-passes N   Max critique-revision iterations (default: 2)
  --model MODEL    Critique and revision model (default: claude-haiku-4-5)
  --profile        Target profile
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `ConstitutionalPolicy` loads principles from SQLite `constitutional_principles` table for the given profile. |
| FR-02 | `CritiqueRevisionLoop.evaluate(output, policy)`: for each revision pass, call the critique model with the output + all principles; parse the critique to determine if a violation was detected. |
| FR-03 | Critique prompt: `"Please critique the following output for compliance with these principles: {principles_list}\n\nOutput: {output}\n\nIdentify any violations and explain why."` |
| FR-04 | Revision prompt (only when critique finds a violation): `"Revise the following output to comply with these principles while preserving the useful information: {principles_list}\n\nOriginal: {output}\nCritique: {critique}\n\nRevised:"` |
| FR-05 | After max passes, if the last revision still has critique-detected violations, return `GuardrailResult(action=BLOCK, reason=CONSTITUTION_VIOLATION)`. |
| FR-06 | If revision produces a compliant output, return `GuardrailResult(action=SANITIZE, sanitized_text=revised_output, reason=CONSTITUTION_REVISED)`. |
| FR-07 | All critique-revision cycles logged to `constitutional_events` table: profile, original_output_hash, critique, revised_output, pass_num, final_action. |
| FR-08 | `tag constitutional load-template` inserts the template principles into `constitutional_principles` for the profile. |
| FR-09 | Integration with PRD-121 output guardrail pipeline: `ConstitutionalGuardrail` implements `OutputGuardrail` interface and is chainable. |
| FR-10 | `tag constitutional test` runs the loop and prints each pass: critique text, revision text, final action. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Default max 2 revision passes to bound LLM API cost; configurable up to 5. |
| NFR-02 | Principles list in critique prompt truncated to 2000 tokens; excess principles omitted (with warning). |
| NFR-03 | `constitutional_events` table uses append-only inserts; never updated after creation. |
| NFR-04 | Built-in templates stored as YAML files in the package `data/constitutions/` directory. |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS constitutional_principles (
  id          TEXT PRIMARY KEY,
  profile     TEXT NOT NULL,
  text        TEXT NOT NULL,
  category    TEXT,
  template    TEXT,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_principles_profile
  ON constitutional_principles(profile, enabled);

CREATE TABLE IF NOT EXISTS constitutional_events (
  id              TEXT PRIMARY KEY,
  profile         TEXT,
  run_id          TEXT,
  pass_num        INTEGER NOT NULL,
  original_hash   TEXT,
  critique        TEXT,
  revised_output  TEXT,
  final_action    TEXT NOT NULL,
  created_at      TEXT NOT NULL
);
```

### 9.2 Built-in templates

```yaml
# data/constitutions/default.yaml
name: default
description: General-purpose Anthropic-inspired principles
principles:
  - text: "Choose the response that is least likely to cause harm to humans or society."
    category: harm-avoidance
  - text: "Choose the response that is most honest, including being honest about uncertainty."
    category: honesty
  - text: "Avoid providing instructions for illegal activities or activities that could harm others."
    category: safety
  - text: "Respect user autonomy in personal choices that don't harm others."
    category: autonomy
  - text: "Be helpful while avoiding content that could be used to discriminate against protected groups."
    category: fairness
```

### 9.3 Python core

```python
from __future__ import annotations
import hashlib
import sqlite3
import uuid
from typing import List, Optional
from tag.guardrail_result import GuardrailResult, GuardrailAction

class ConstitutionalPolicy:
    def __init__(self, conn: sqlite3.Connection, profile: str) -> None:
        rows = conn.execute(
            "SELECT text FROM constitutional_principles WHERE profile=? AND enabled=1",
            (profile,)
        ).fetchall()
        self.principles: List[str] = [r["text"] for r in rows]
        self.profile = profile

    def format_for_prompt(self, max_tokens: int = 2000) -> str:
        lines = []
        token_est = 0
        for i, p in enumerate(self.principles, 1):
            line = f"{i}. {p}"
            token_est += len(line.split()) * 1.3
            if token_est > max_tokens:
                break
            lines.append(line)
        return "\n".join(lines)

class CritiqueRevisionLoop:
    CRITIQUE_TMPL = (
        "Critique the following output for compliance with these principles:\n"
        "{principles}\n\nOutput:\n{output}\n\n"
        "Identify any violations. If compliant, say 'COMPLIANT'. "
        "If there are violations, describe them briefly."
    )
    REVISION_TMPL = (
        "Revise the following output to comply with these principles "
        "while preserving the useful information:\n"
        "{principles}\n\nOriginal output:\n{output}\n\nCritique:\n{critique}\n\n"
        "Revised output:"
    )

    def __init__(self, model: str = "claude-haiku-4-5", max_passes: int = 2,
                 conn: Optional[sqlite3.Connection] = None) -> None:
        self.model = model
        self.max_passes = max_passes
        self.conn = conn

    def evaluate(self, output: str, policy: ConstitutionalPolicy,
                 run_id: Optional[str] = None) -> GuardrailResult:
        import anthropic
        client = anthropic.Anthropic()
        principles = policy.format_for_prompt()
        current = output
        for pass_num in range(1, self.max_passes + 1):
            critique = client.messages.create(
                model=self.model,
                max_tokens=512,
                messages=[{"role": "user", "content": self.CRITIQUE_TMPL.format(
                    principles=principles, output=current)}]
            ).content[0].text
            compliant = "COMPLIANT" in critique.upper() and "violation" not in critique.lower()
            if compliant:
                self._log(policy.profile, run_id, pass_num, output, critique, current, "pass")
                if current != output:
                    return GuardrailResult(action=GuardrailAction.SANITIZE,
                                           sanitized_text=current, reason="CONSTITUTION_REVISED",
                                           guardrail="constitutional")
                return GuardrailResult(action=GuardrailAction.PASS, guardrail="constitutional")
            if pass_num < self.max_passes:
                current = client.messages.create(
                    model=self.model,
                    max_tokens=1024,
                    messages=[{"role": "user", "content": self.REVISION_TMPL.format(
                        principles=principles, output=current, critique=critique)}]
                ).content[0].text.strip()
        self._log(policy.profile, run_id, self.max_passes, output, critique, current, "block")
        return GuardrailResult(action=GuardrailAction.BLOCK, reason="CONSTITUTION_VIOLATION",
                               guardrail="constitutional")

    def _log(self, profile: str, run_id: Optional[str], pass_num: int,
             original: str, critique: str, revised: str, action: str) -> None:
        if not self.conn:
            return
        h = hashlib.sha256(original.encode()).hexdigest()[:16]
        self.conn.execute(
            "INSERT INTO constitutional_events(id,profile,run_id,pass_num,original_hash,critique,revised_output,final_action,created_at) "
            "VALUES(?,?,?,?,?,?,?,?,?)",
            (uuid.uuid4().hex[:8], profile, run_id, pass_num, h, critique[:2000],
             revised[:4000], action, _utc_now())
        )
        self.conn.commit()

def _utc_now() -> str:
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).isoformat()
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Critique model itself producing harmful guidance | Use only Anthropic models for critique; not user-configurable to external endpoints |
| Constitution bypassed by adversarial inputs | Constitutional AI is complementary to rule-based guardrails (PRD-121/122), not a replacement |
| Cost overrun from critique loops | Max 2 passes by default; hard cap at 5; cost tracked per session |
| Revision introducing new violations | After revision, the output passes through standard output guardrails (PRD-121) |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `ConstitutionalPolicy.format_for_prompt` token truncation; `CritiqueRevisionLoop` with mock model |
| Integration | Full loop: harmful input → critique detects violation → revision → re-critique → SANITIZE result |
| Evaluation | 20-case test set with known-violating/known-compliant outputs; recall and precision metrics |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag constitutional load-template --template default` adds default principles to profile |
| AC-02 | `tag constitutional test --input "harmful content"` runs critique and prints violation + revision |
| AC-03 | Policy-compliant output returns `GuardrailResult(action=PASS)` |
| AC-04 | After max passes with persistent violations, returns `GuardrailResult(action=BLOCK)` |
| AC-05 | All critique-revision cycles logged to `constitutional_events` |
| AC-06 | `tag constitutional list` shows all active principles for the profile |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-124 GuardrailResult | Shared result type |
| PRD-121 output guardrail | Pipeline integration |
| `anthropic` SDK | Critique and revision model API |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should the critique model be required to respond in a structured format (JSON) for reliable compliance detection? |
| OQ-02 | Should there be a "soft constitutional" mode that only warns without blocking, for gradual policy rollout? |
| OQ-03 | Should multi-principle violations require all principles to be violated before blocking, or any single principle? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `ConstitutionalPolicy`, built-in templates (YAML), `constitutional_principles` DDL | 1 |
| 2 | `CritiqueRevisionLoop` core (critique prompt, revision prompt, compliance detection) | 2 |
| 3 | SQLite audit log, PRD-121 integration, CLI commands | 2 |
| 4 | Evaluation tests (20-case set), documentation | 2 |

