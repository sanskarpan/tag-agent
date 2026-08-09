// Package redact removes credential-shaped substrings from text that is about
// to be persisted, logged, or shown.
//
// It lives in its own package because both the permission audit and the LLM
// providers need it, and permission cannot import llm (nor the reverse) without
// a cycle. A shared helper in a leaf package is the fix; duplicating the
// patterns in two places would guarantee they drift.
package redact

import (
	"regexp"
	"strings"
)

// secretPatterns match credential SHAPES in free text. A secret does not
// announce itself by the variable it was stored in, and an audit row records
// whatever the model typed.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(bearer|token|apikey|api[_-]?key|password|passwd|secret)\s*[:=]?\s*["']?([A-Za-z0-9_\-\.]{12,})`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
	regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

// Secrets replaces credential-shaped substrings with a marker. It is
// deliberately eager: an over-redacted audit line still records WHAT happened
// and to which tool, while an under-redacted one publishes a key.
func Secrets(s string) string {
	if s == "" {
		return s
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			// Keep the leading keyword so the line stays readable.
			if i := strings.IndexAny(m, ":= "); i > 0 && i < 20 {
				return m[:i+1] + "<redacted>"
			}
			return "<redacted>"
		})
	}
	return s
}
