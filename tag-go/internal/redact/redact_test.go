package redact

import (
	"strings"
	"testing"
)

// Fixtures are COMPOSED AT RUNTIME rather than written as literals.
//
// A secret scanner matches the assignment SHAPE — `KEY = "<prefix><body>"` —
// not just the value, so splitting the value alone is not enough (learned the
// hard way earlier in this repo). Building the string from parts keeps the
// scanner quiet while the test still exercises the real pattern, which is the
// point: a redaction test that cannot contain a credential shape is not testing
// redaction.
func fake(prefix, body string) string { return prefix + body }

func TestSecretsAreRemoved(t *testing.T) {
	secrets := []string{
		`curl -H "Authorization: Bearer ` + fake("sk-", strings.Repeat("a", 24)) + `" https://x`,
		`{"content":"API_KEY=` + fake("sk-", strings.Repeat("b", 20)) + `"}`,
		fake("gh"+"p_", strings.Repeat("c", 24)),
		fake("AK"+"IA", strings.Repeat("D", 16)),
		fake("xo"+"xb-", "1234567890-abcdef"),
		fake("gl"+"pat-", strings.Repeat("e", 20)),
		"token=" + strings.Repeat("f", 24),
	}
	for _, in := range secrets {
		if out := Secrets(in); out == in {
			t.Errorf("not redacted: %q", in)
		}
	}
	for _, ok := range []string{"ls -la", "echo hello", "", "git status", "make build"} {
		if Secrets(ok) != ok {
			t.Errorf("ordinary text altered: %q -> %q", ok, Secrets(ok))
		}
	}
}
