# PRD-121: Output Guardrail Processor (`tag guardrail output`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Security/Guardrails
**Affects:** `internal/agent (output guardrail middleware chain) + internal/store (audit log)`
**Depends on:** PRD-124 (GuardrailResult type), PRD-034 (secret scanning), PRD-013 (agent tracing — span instrumentation)
**Inspired by:** Guardrails AI output validators, LlamaGuard, Nemo Guardrails output rails, AWS Bedrock Guardrails

---

## 1. Overview

Agent outputs can contain harmful, policy-violating, or sensitive content: generated code that is insecure, outputs that contain PII or credentials, responses that violate business rules, or content that fails safety classifiers. Without a structured post-processing layer, these outputs flow directly to downstream consumers — users, databases, APIs — without any opportunity for detection or remediation.

Output Guardrail Processor (`tag guardrail output`) introduces a composable output validation pipeline modeled as a **middleware chain wrapped around the hand-rolled agent loop** (`internal/agent`). Each guardrail is a Go type implementing the `OutputGuardrail` interface; the chain inspects every agent response before it is returned to the caller. Every guardrail returns a typed `Decision` (a `GuardrailResult`, PRD-124) indicating pass/block/rewrite, gated in the same style as the tool permission service (`internal/tool`). The chain short-circuits on the first block decision and can optionally rewrite the output using a remediation model behind the `internal/llm` provider interface.

The design is inspired by Guardrails AI's output validators (regex, PII detection, JSON schema validation, topic filtering), AWS Bedrock Guardrails' output filters (profanity, PII, topic denial, grounding), and Nemo Guardrails' output rails (fact-checking, output filtering flows). TAG's implementation is local-first: built-in guardrails cover the most common cases and are compiled into the static binary as Go interface implementations registered in a slice/map. **Extensibility note:** because TAG ships as a single static Go binary, custom guardrails are NOT dynamic user-supplied code plugins. Regex- and config-declared guardrails port directly; arbitrary custom logic is expressed as (a) compiled-in Go `OutputGuardrail` implementations, (b) shell/HTTP hooks, or (c) MCP tools — matching GO_MIGRATION_PLAN decision #6.

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
| G1 | `OutputChain` executes the registered slice of `OutputGuardrail` implementations in order, short-circuiting on the first `block` decision. |
| G2 | Built-in output guardrails: PII detector, secret scanner, JSON schema validator, topic filter, profanity filter, toxicity classifier. |
| G3 | `GuardrailResult.action == "rewrite"` triggers an LLM-based remediation call to fix the output before returning. |
| G4 | All guardrail decisions logged to the `guardrail_events` SQLite table for auditability. |
| G5 | `tag guardrail output list` shows all configured output guardrails for a profile. |
| G6 | `tag guardrail output test --input TEXT` dry-runs the output pipeline against a test string. |
| G7 | Output guardrail chain integrable with the agent loop (`internal/agent`) as post-response middleware. |

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
| FR-01 | `OutputChain.Process(ctx context.Context, resp *Response) (Decision, error)` executes the registered `OutputGuardrail` slice in priority order; returns the first non-pass `Decision` or a final pass. |
| FR-02 | PII guardrail: detect names (NER), emails (RE2 regexp), phone numbers (RE2 regexp), SSNs (RE2 regexp), credit card numbers (Luhn check); return a block `Decision` with `PII_DETECTED` reason. |
| FR-03 | Secret guardrail: reuse the PRD-034 secret scanner; return a block `Decision` with `SECRET_DETECTED` reason. |
| FR-04 | JSON schema guardrail: `json.Unmarshal` the output, then validate against a provided JSON Schema (schema authored/validated with `invopop/jsonschema`); return a block `Decision` with `SCHEMA_INVALID` reason on failure. |
| FR-05 | Topic filter guardrail: compute embedding cosine similarity between output and each forbidden-topic embedding (embeddings via the `internal/llm` Embedder interface); block if similarity > threshold. |
| FR-06 | Rewrite action: call the remediation model through the `internal/llm` provider interface (`Complete`) with a `"Rewrite the following to comply with policy: {output}"` system prompt; return the rewritten output. |
| FR-07 | All guardrail decisions written to the `guardrail_events` table (`internal/store`, `modernc.org/sqlite`): profile, guardrail_type, action, reason, input_hash, output_hash, timestamp. |
| FR-08 | The agent loop in `internal/agent` invokes `OutputChain.Process` as post-response middleware after each turn; a block `Decision` maps to a `continue|compact|stop` outcome (stop) and a sanitized error is returned to the caller — expressed as a `Decision`/`error` return, not a panic. |
| FR-09 | `tag guardrail output test` runs the chain on the provided `--input` and prints each guardrail's `Decision` (pass/block/rewrite) with reason. |
| FR-10 | `tag guardrail output add` persists a new `output_guardrail_configs` row via `internal/store`; `list` queries and renders it. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Non-LLM guardrails (regex, schema) must complete in < 20ms per output. |
| NFR-02 | LLM-based guardrails (toxicity classifier, topic filter with embedding) must complete in < 2s. |
| NFR-03 | PII detection must not require network calls (local RE2 regexp/NER only). |
| NFR-04 | Guardrail chain is goroutine-safe; concurrent agent turns do not produce corrupted audit-log entries (single-writer SQLite via `internal/store`). |

---

## 9. Technical Design

### 9.1 SQLite DDL

Owned by `internal/store` (`modernc.org/sqlite`, pure-Go, CGO_ENABLED=0, single-writer). DDL is unchanged from the original design:

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

### 9.2 Go core (`internal/agent`)

Guardrails are Go interfaces (replacing Python ABCs/subclasses); the registry is a `[]OutputGuardrail` slice ordered by severity. `Decision` carries the tripwire/blocking semantics as a value type — blocking is a returned `Decision`, never a panic. The chain is invoked as output middleware around the hand-rolled agent loop and returns a `continue|compact|stop` outcome to the loop.

```go
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/tag/internal/store"
)

// Action mirrors PRD-124 GuardrailResult actions.
type Action string

const (
	ActionPass    Action = "pass"
	ActionBlock   Action = "block"
	ActionRewrite Action = "rewrite"
	ActionWarn    Action = "warn"
)

// Decision is the typed result of a guardrail (PRD-124 GuardrailResult).
type Decision struct {
	Action    Action
	Reason    string // e.g. "PII_DETECTED:email"
	Guardrail string
	Rewritten string // set when Action == ActionRewrite
}

// OutputGuardrail is the interface every output guardrail implements
// (Go interface in place of a Python ABC). Registered implementations
// are compiled into the binary.
type OutputGuardrail interface {
	Name() string
	Severity() string // "high" | "medium" | "low"
	Process(ctx context.Context, resp *Response) (Decision, error)
}

// --- PII (RE2 regexp; no backreferences/lookahead needed here) ---

var (
	reEmail = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
	reSSN   = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	rePhone = regexp.MustCompile(`\b(\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]\d{3}[-.\s]\d{4}\b`)
)

type PIIGuardrail struct {
	action   Action
	severity string
}

func (g *PIIGuardrail) Name() string     { return "pii" }
func (g *PIIGuardrail) Severity() string  { return g.severity }

func (g *PIIGuardrail) Process(_ context.Context, resp *Response) (Decision, error) {
	for _, p := range []struct {
		name string
		re   *regexp.Regexp
	}{{"email", reEmail}, {"SSN", reSSN}, {"phone", rePhone}} {
		if p.re.MatchString(resp.Text) {
			return Decision{Action: g.action, Reason: "PII_DETECTED:" + p.name, Guardrail: g.Name()}, nil
		}
	}
	return Decision{Action: ActionPass, Guardrail: g.Name()}, nil
}

// --- Secret (reuses PRD-034 scanner) ---

type SecretGuardrail struct {
	action   Action
	severity string
	scanner  SecretScanner // PRD-034
}

func (g *SecretGuardrail) Name() string    { return "secret" }
func (g *SecretGuardrail) Severity() string { return g.severity }

func (g *SecretGuardrail) Process(_ context.Context, resp *Response) (Decision, error) {
	if findings := g.scanner.ScanText(resp.Text); len(findings) > 0 {
		return Decision{Action: g.action, Reason: "SECRET_DETECTED:" + findings[0].Type, Guardrail: g.Name()}, nil
	}
	return Decision{Action: ActionPass, Guardrail: g.Name()}, nil
}

// --- JSON schema (invopop/jsonschema-authored schema + explicit validation) ---

type JSONSchemaGuardrail struct {
	action   Action
	severity string
	schema   *jsonschema.Schema
}

func (g *JSONSchemaGuardrail) Name() string    { return "json-schema" }
func (g *JSONSchemaGuardrail) Severity() string { return g.severity }

func (g *JSONSchemaGuardrail) Process(_ context.Context, resp *Response) (Decision, error) {
	var obj any
	if err := json.Unmarshal([]byte(resp.Text), &obj); err != nil {
		return Decision{Action: g.action, Reason: fmt.Sprintf("SCHEMA_INVALID:%.100s", err), Guardrail: g.Name()}, nil
	}
	if err := g.schema.Validate(obj); err != nil {
		return Decision{Action: g.action, Reason: fmt.Sprintf("SCHEMA_INVALID:%.100s", err), Guardrail: g.Name()}, nil
	}
	return Decision{Action: ActionPass, Guardrail: g.Name()}, nil
}

// --- Chain / middleware around the agent loop ---

type OutputChain struct {
	guardrails []OutputGuardrail
	store      *store.DB
}

func NewOutputChain(gs []OutputGuardrail, db *store.DB) *OutputChain {
	rank := map[string]int{"high": 0, "medium": 1, "low": 2}
	sort.SliceStable(gs, func(i, j int) bool { return rank[gs[i].Severity()] < rank[gs[j].Severity()] })
	return &OutputChain{guardrails: gs, store: db}
}

// Process runs guardrails in priority order, short-circuiting on the first
// non-pass Decision. Every decision is appended to guardrail_events.
func (c *OutputChain) Process(ctx context.Context, resp *Response, runID string) (Decision, error) {
	for _, g := range c.guardrails {
		d, err := g.Process(ctx, resp)
		if err != nil {
			return Decision{}, err
		}
		_ = c.store.AppendGuardrailEvent(ctx, "output", runID, d)
		if d.Action != ActionPass {
			return d, nil
		}
	}
	return Decision{Action: ActionPass, Guardrail: "chain"}, nil
}
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
| Unit | Table-driven `go test` for `PIIGuardrail` email/SSN/phone detection; `JSONSchemaGuardrail` pass/fail on known pairs |
| Benchmark | `testing.B` for chain latency on a 1000-token output (NFR-01 < 20ms non-LLM) |
| Integration | Full chain: PII in output → block `Decision` → `guardrail_events` audit row |
| Security | Encoded PII detection; rewritten output re-run through the chain |

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
| PRD-124 GuardrailResult type (`Decision`) | Shared result type |
| PRD-034 secret scanning | Secret scanner reuse |
| `invopop/jsonschema` | JSON schema authoring/validation |
| `modernc.org/sqlite` (via `internal/store`) | Audit-log persistence |
| `internal/llm` provider interface | Remediation model for rewrite action + topic-filter embeddings |

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
| 1 | `Decision`, `OutputGuardrail` interface, `PIIGuardrail`, `SecretGuardrail`, table-driven unit tests | 2 |
| 2 | `JSONSchemaGuardrail`, `OutputChain`, `internal/store` audit log | 2 |
| 3 | CLI commands (`internal/cli`), rewrite action via `internal/llm`, agent-loop middleware integration | 2 |
| 4 | Integration tests, documentation | 1 |

