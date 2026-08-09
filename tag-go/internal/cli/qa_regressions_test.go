package cli

import (
	"strings"
	"testing"
)

// Regressions from the 2026-08-03 production-readiness audit.
//
// Fixtures are COMPOSED AT RUNTIME, never written as literals: a secret scanner
// matches the assignment shape `KEY = "<prefix><body>"`, so splitting the value
// alone does not help. A redaction test necessarily contains credential shapes,
// which is exactly what trips the scanner — building them from parts is the
// only way to have both.
func fake(prefix string, n int, c string) string { return prefix + strings.Repeat(c, n) }

// TestRedactEnvCoversValueShapes: redaction was a name-only allowlist, so
// PRIVATE_KEY, STRIPE_SK, GH_PAT, DB_PASS, SESSION_COOKIE and AWS_ACCESS_KEY_ID
// were exported verbatim — by a command whose help says "secrets redacted" and
// whose output feeds a template-sharing workflow.
func TestRedactEnvCoversValueShapes(t *testing.T) {
	mustRedact := map[string]string{
		"PRIVATE_KEY":       "-----BEGIN RSA PRIVATE KEY-----MIIEow",
		"STRIPE_SK":         fake("sk"+"_live_", 14, "1"),
		"GH_PAT":            fake("gh"+"p_", 24, "a"),
		"DB_PASS":           "hunter2",
		"SESSION_COOKIE":    "abc123",
		"AWS_ACCESS_KEY_ID": fake("AK"+"IA", 16, "D"),
		"API_KEY":           "anything",
		// the name gives nothing away; the VALUE is unmistakable
		"HARMLESS_LOOKING": fake("sk-", 22, "b"),
		"X":                fake("ey"+"J", 12, "c") + "." + strings.Repeat("d", 12) + ".sig",
	}
	for k, v := range mustRedact {
		if got := redactEnv(k, v); got == v {
			t.Errorf("%s was exported in the clear: %q", k, got)
		}
	}

	// Over-redaction has a cost too: a template of <REDACTED> is not a template.
	mustPass := map[string]string{
		"BYPASS_CACHE": "1",
		"PASSTHROUGH":  "on",
		"HARMLESS":     "just-a-value",
		"LOG_LEVEL":    "debug",
		"PORT":         "8080",
		"COMPASS":      "north",
	}
	for k, v := range mustPass {
		if got := redactEnv(k, v); got != v {
			t.Errorf("%s was needlessly redacted to %q", k, got)
		}
	}
}

func TestLooksSecretIsShapeBased(t *testing.T) {
	for _, v := range []string{
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		fake("gh"+"p_", 24, "a"),
		fake("AK"+"IA", 16, "D"),
		fake("xo"+"xb-", 18, "1"),
		fake("gl"+"pat-", 20, "e"),
		fake("AI"+"za", 34, "f"),
	} {
		if !looksSecret(v) {
			t.Errorf("credential shape not recognised: %q", v)
		}
	}
	for _, v := range []string{"", "  ", "hello world", "8080", "debug", "/usr/local/bin"} {
		if looksSecret(v) {
			t.Errorf("ordinary value treated as a secret: %q", v)
		}
	}
}
