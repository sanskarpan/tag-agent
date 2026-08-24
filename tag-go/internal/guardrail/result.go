package guardrail

import "fmt"

// PRD-124: the shared, typed result contract returned by every content-guardrail
// check (input PRD-122 / output PRD-121). Distinct from the tripwire Verdict —
// this is the per-check value used by the content-guardrail chains.

// GuardrailAction is the action a guardrail check decided to take. String-backed
// so it JSON-marshals to a stable wire value.
type GuardrailAction string

const (
	GActionPass      GuardrailAction = "pass"
	GActionBlock     GuardrailAction = "block"
	GActionSanitize  GuardrailAction = "sanitize"
	GActionWarn      GuardrailAction = "warn"
	GActionInterrupt GuardrailAction = "interrupt"
	GActionRewrite   GuardrailAction = "rewrite" // PRD-121 output-only remediation
)

// GuardrailResult is the standardized return type for all content-guardrail
// checks. It is passed and returned BY VALUE; guardrails never share a mutable
// result.
type GuardrailResult struct {
	Action        GuardrailAction `json:"action"`
	Reason        string          `json:"reason"`
	Guardrail     string          `json:"guardrail"`
	SanitizedText *string         `json:"sanitized_text"` // set only when Action == GActionSanitize
	Message       *string         `json:"message"`        // human-readable, shown on interrupt
	Metadata      map[string]any  `json:"metadata,omitempty"`
}

// IsBlocking reports whether this result should stop downstream processing.
func (r GuardrailResult) IsBlocking() bool {
	return r.Action == GActionBlock || r.Action == GActionInterrupt
}

// ShouldSanitize reports whether SanitizedText should replace the original text.
func (r GuardrailResult) ShouldSanitize() bool {
	return r.Action == GActionSanitize && r.SanitizedText != nil
}

// Fired reports whether the check did anything other than pass.
func (r GuardrailResult) Fired() bool { return r.Action != GActionPass }

// Pass is the convenience constructor for a clean pass.
func Pass(guardrail string) GuardrailResult {
	return GuardrailResult{Action: GActionPass, Guardrail: guardrail}
}

// Block is the convenience constructor for a block result.
func Block(reason, guardrail string, message ...string) GuardrailResult {
	r := GuardrailResult{Action: GActionBlock, Reason: reason, Guardrail: guardrail}
	if len(message) > 0 {
		r.Message = &message[0]
	}
	return r
}

// Sanitize is the convenience constructor for a sanitize result.
func Sanitize(sanitized, reason, guardrail string) GuardrailResult {
	return GuardrailResult{Action: GActionSanitize, Reason: reason, Guardrail: guardrail, SanitizedText: &sanitized}
}

// Warn is the convenience constructor for a warn result.
func Warn(reason, guardrail string) GuardrailResult {
	return GuardrailResult{Action: GActionWarn, Reason: reason, Guardrail: guardrail}
}

// String implements fmt.Stringer for log-friendly output.
func (r GuardrailResult) String() string {
	return fmt.Sprintf("GuardrailResult(action=%s, reason=%q, guardrail=%q)", r.Action, r.Reason, r.Guardrail)
}
