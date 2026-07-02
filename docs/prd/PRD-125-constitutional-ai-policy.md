# PRD-125: Constitutional AI Policy (`tag constitutional`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Security/Guardrails
**Affects:** `internal/runtime/guardrail/constitutional.go` + `internal/cli` + go:embed templates
**Depends on:** PRD-124 (GuardrailResult type), PRD-121 (output guardrail processor), PRD-122 (input guardrail validator)
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
| G1 | `ConstitutionalPolicy` loads a set of principles (from SQLite, seeded by go:embed YAML templates) and makes them available for critique evaluation. |
| G2 | `CritiqueRevisionLoop.Evaluate(ctx, output string, policy *ConstitutionalPolicy) guardrail.GuardrailResult` runs the critique-revision cycle. |
| G3 | Support configurable max revision passes (default: 2) to bound computational cost. |
| G4 | `tag constitutional add-principle --text TEXT --profile PROFILE` adds a principle to the active policy. |
| G5 | `tag constitutional list` shows all active principles for a profile. |
| G6 | `tag constitutional test --input TEXT` runs the critique-revision loop against a test string. |
| G7 | Built-in constitutional templates: `default` (Anthropic-inspired), `medical`, `educational`, `financial`, `legal`. |
| G8 | Critique and revision models are configurable and dispatched through the `internal/llm` provider interface (default: `claude-haiku-4-5` for both, via the anthropic-sdk-go adapter). |

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
| FR-02 | `CritiqueRevisionLoop.Evaluate(ctx, output, policy)`: for each revision pass, call the critique model (via `internal/llm`) with the output + all principles; parse the critique to determine if a violation was detected. |
| FR-03 | Critique prompt: `"Please critique the following output for compliance with these principles: {principles_list}\n\nOutput: {output}\n\nIdentify any violations and explain why."` |
| FR-04 | Revision prompt (only when critique finds a violation): `"Revise the following output to comply with these principles while preserving the useful information: {principles_list}\n\nOriginal: {output}\nCritique: {critique}\n\nRevised:"` |
| FR-05 | After max passes, if the last revision still has critique-detected violations, return `GuardrailResult(action=BLOCK, reason=CONSTITUTION_VIOLATION)`. |
| FR-06 | If revision produces a compliant output, return `GuardrailResult(action=SANITIZE, sanitized_text=revised_output, reason=CONSTITUTION_REVISED)`. |
| FR-07 | All critique-revision cycles logged to `constitutional_events` table: profile, original_output_hash, critique, revised_output, pass_num, final_action. |
| FR-08 | `tag constitutional load-template` inserts the template principles into `constitutional_principles` for the profile. |
| FR-09 | Integration with PRD-121 output guardrail pipeline: `ConstitutionalGuardrail` satisfies the `OutputGuardrail` Go interface (`Process(ctx, string) guardrail.GuardrailResult`) and is chainable in the pipeline. |
| FR-10 | `tag constitutional test` runs the loop and prints each pass: critique text, revision text, final action. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Default max 2 revision passes to bound LLM API cost; configurable up to 5. |
| NFR-02 | Principles list in critique prompt truncated to 2000 tokens; excess principles omitted (with warning). |
| NFR-03 | `constitutional_events` table uses append-only inserts; never updated after creation. |
| NFR-04 | Built-in templates are YAML files compiled into the binary via `go:embed` (`//go:embed data/constitutions/*.yaml`), parsed with `gopkg.in/yaml.v3`; no on-disk data directory needed for the single static binary. |

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

Templates live under `internal/runtime/guardrail/data/constitutions/*.yaml` and are embedded with `//go:embed data/constitutions/*.yaml` into an `embed.FS`, then decoded with `gopkg.in/yaml.v3` at `load-template` time. No files ship alongside the binary.

```yaml
# data/constitutions/default.yaml (embedded via go:embed)
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

### 9.3 Go core (`internal/runtime/guardrail`)

The critique and revision calls go through the `internal/llm` provider interface (`Stream(ctx, Request) -> <-chan Event`), so the loop is provider-neutral — the default model `claude-haiku-4-5` resolves to the anthropic-sdk-go adapter, but any registered provider works. Non-streaming completions accumulate the `TextDelta` events into a single string.

```go
package guardrail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/store"
)

type ConstitutionalPolicy struct {
	Profile    string
	Principles []string
}

// LoadPolicy reads enabled principles for a profile from the single-writer store.
func LoadPolicy(ctx context.Context, st *store.Store, profile string) (*ConstitutionalPolicy, error) {
	texts, err := st.ConstitutionalPrinciples(ctx, profile) // WHERE profile=? AND enabled=1
	if err != nil {
		return nil, err
	}
	return &ConstitutionalPolicy{Profile: profile, Principles: texts}, nil
}

// FormatForPrompt renders principles, truncating to a rough token budget
// (len(fields)*1.3 heuristic — no local Claude tokenizer exists; see obs pkg).
func (p *ConstitutionalPolicy) FormatForPrompt(maxTokens int) string {
	var b strings.Builder
	tokenEst := 0.0
	for i, pr := range p.Principles {
		line := fmt.Sprintf("%d. %s", i+1, pr)
		tokenEst += float64(len(strings.Fields(line))) * 1.3
		if int(tokenEst) > maxTokens {
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

const (
	critiqueTmpl = "Critique the following output for compliance with these principles:\n" +
		"%s\n\nOutput:\n%s\n\nIdentify any violations. If compliant, say 'COMPLIANT'. " +
		"If there are violations, describe them briefly."
	revisionTmpl = "Revise the following output to comply with these principles " +
		"while preserving the useful information:\n%s\n\nOriginal output:\n%s\n\nCritique:\n%s\n\nRevised output:"
)

type CritiqueRevisionLoop struct {
	Provider  llm.Provider // internal/llm interface; default resolves to claude-haiku-4-5
	Model     string
	MaxPasses int
	Store     *store.Store // nil => no audit logging
}

func (l *CritiqueRevisionLoop) Evaluate(ctx context.Context, output string, policy *ConstitutionalPolicy, runID string) (GuardrailResult, error) {
	principles := policy.FormatForPrompt(2000)
	current := output
	var critique string
	for pass := 1; pass <= l.MaxPasses; pass++ {
		var err error
		critique, err = llm.Complete(ctx, l.Provider, l.Model, 512,
			fmt.Sprintf(critiqueTmpl, principles, current))
		if err != nil {
			return GuardrailResult{}, err
		}
		compliant := strings.Contains(strings.ToUpper(critique), "COMPLIANT") &&
			!strings.Contains(strings.ToLower(critique), "violation")
		if compliant {
			l.logEvent(ctx, policy.Profile, runID, pass, output, critique, current, "pass")
			if current != output {
				return Sanitize(current, "CONSTITUTION_REVISED", "constitutional"), nil
			}
			return Pass("constitutional"), nil
		}
		if pass < l.MaxPasses {
			revised, err := llm.Complete(ctx, l.Provider, l.Model, 1024,
				fmt.Sprintf(revisionTmpl, principles, current, critique))
			if err != nil {
				return GuardrailResult{}, err
			}
			current = strings.TrimSpace(revised)
		}
	}
	l.logEvent(ctx, policy.Profile, runID, l.MaxPasses, output, critique, current, "block")
	return Block("CONSTITUTION_VIOLATION", "constitutional"), nil
}

func (l *CritiqueRevisionLoop) logEvent(ctx context.Context, profile, runID string, pass int, original, critique, revised, action string) {
	if l.Store == nil {
		return
	}
	sum := sha256.Sum256([]byte(original))
	_ = l.Store.InsertConstitutionalEvent(ctx, store.ConstitutionalEvent{
		Profile: profile, RunID: runID, PassNum: pass,
		OriginalHash: hex.EncodeToString(sum[:])[:16],
		Critique:     truncate(critique, 2000),
		RevisedOutput: truncate(revised, 4000),
		FinalAction:  action,
	}) // append-only insert through the single-writer store
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
```

`llm.Complete` is a small helper that drains the provider's `Event` channel, concatenating `TextDelta`s into a string with a `max_tokens` cap. Timestamps and event IDs are assigned by `internal/store` (via the `strftime` DDL defaults / a `crypto/rand` id), so the loop stays pure.

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Critique model itself producing harmful guidance | Critique is pinned to trusted providers behind the `internal/llm` interface (default anthropic-sdk-go); base-URL override to arbitrary external endpoints is disallowed for the critique path |
| Constitution bypassed by adversarial inputs | Constitutional AI is complementary to rule-based guardrails (PRD-121/122), not a replacement |
| Cost overrun from critique loops | Max 2 passes by default; hard cap at 5; cost tracked per session |
| Revision introducing new violations | After revision, the output passes through standard output guardrails (PRD-121) |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `ConstitutionalPolicy.FormatForPrompt` token truncation; `CritiqueRevisionLoop` with a fake `llm.Provider` (table-driven, scripted critique/revision responses) |
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
| PRD-121 output guardrail | Pipeline integration (`OutputGuardrail` interface) |
| `internal/llm` provider iface | Critique + revision calls (default anthropic-sdk-go adapter) |
| `gopkg.in/yaml.v3` + `embed` | Parse go:embed'd constitution templates |

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

