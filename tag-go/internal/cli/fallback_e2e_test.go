package cli_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runNoKeys runs the built binary with a scrubbed env (no provider API keys) so
// the openai/anthropic adapters deterministically error offline — the trigger
// for exercising the runtime fallback chain (gap #2).
func runNoKeys(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	env := []string{"TAG_HOME=" + home}
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "OPENAI_API_KEY="),
			strings.HasPrefix(kv, "ANTHROPIC_API_KEY="),
			strings.HasPrefix(kv, "GEMINI_API_KEY="),
			strings.HasPrefix(kv, "TAG_HOME="):
			// drop — force the offline error path
		default:
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// TestE2EFallbackChain verifies the route-fallback chain is actually WALKED at
// inference time (gap #2): a primary provider that errors (no API key) falls
// back to the echo provider, which succeeds — but only with --fallback.
func TestE2EFallbackChain(t *testing.T) {
	h := newHome(t)
	if _, c := runNoKeys(t, h, "bootstrap"); c != 0 {
		t.Fatalf("bootstrap failed: %d", c)
	}
	runNoKeys(t, h, "set-model", "coder", "openai/gpt-4o-mini")
	if out, c := runNoKeys(t, h, "route-fallback", "add", "--profile", "coder",
		"--primary", "openai/gpt-4o-mini", "--fallback", "echo/local", "--condition", "always"); c != 0 {
		t.Fatalf("route-fallback add failed: %q %d", out, c)
	}

	// Without --fallback: the missing-key primary hard-fails (exit 1).
	if out, c := runNoKeys(t, h, "run", "hello world", "--provider", "openai", "--profile", "coder"); c == 0 {
		t.Errorf("expected hard failure without --fallback, got exit 0: %q", out)
	}

	// With --fallback: openai errors ("not set") → chain advances to echo, which
	// echoes the prompt back and exits 0. The fallback notice goes to stderr.
	out, c := runNoKeys(t, h, "run", "hello world", "--provider", "openai", "--profile", "coder", "--fallback")
	if c != 0 {
		t.Fatalf("expected fallback success (exit 0), got %d: %q", c, out)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("echo fallback should return the prompt, got: %q", out)
	}
	if !strings.Contains(out, "fallback: step 0") {
		t.Errorf("expected a fallback notice on the primary failure, got: %q", out)
	}

	// Multi-hop: a depth-2 chain (openai -> anthropic -> echo) must be walked
	// transitively. openai and anthropic both hard-fail (no keys); only echo, two
	// hops down, can serve — so reaching it proves the chain is followed past the
	// primary's direct fallback.
	runNoKeys(t, h, "set-model", "reviewer", "openai/gpt-4o-mini")
	runNoKeys(t, h, "route-fallback", "add", "--profile", "reviewer",
		"--primary", "openai/gpt-4o-mini", "--fallback", "anthropic/claude-haiku-4-5", "--condition", "always")
	runNoKeys(t, h, "route-fallback", "add", "--profile", "reviewer",
		"--primary", "anthropic/claude-haiku-4-5", "--fallback", "echo/local", "--condition", "always")
	out, c = runNoKeys(t, h, "run", "deep hello", "--provider", "openai", "--profile", "reviewer", "--fallback")
	if c != 0 {
		t.Fatalf("expected multi-hop fallback success (exit 0), got %d: %q", c, out)
	}
	if !strings.Contains(out, "deep hello") {
		t.Errorf("echo (2 hops down) should return the prompt, got: %q", out)
	}
	if !strings.Contains(out, "fallback: step 1") {
		t.Errorf("expected the second hop (step 1) to also fall back, got: %q", out)
	}

	// A configured-but-condition-mismatched chain must NOT rescue: gate the
	// fallback on rate_limit while the error is an auth/"not set" error.
	runNoKeys(t, h, "route-fallback", "add", "--profile", "researcher",
		"--primary", "openai/gpt-4o-mini", "--fallback", "echo/local", "--condition", "rate_limit")
	runNoKeys(t, h, "set-model", "researcher", "openai/gpt-4o-mini")
	if _, c := runNoKeys(t, h, "run", "hi", "--provider", "openai", "--profile", "researcher", "--fallback"); c == 0 {
		t.Error("condition=rate_limit must NOT rescue a no-key(auth) error")
	}
}
