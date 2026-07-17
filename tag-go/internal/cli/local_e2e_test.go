package cli_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// mockLocalServer emulates a local OpenAI-compatible inference server that
// streams a fixed reply, so the real `tag run --provider local` subprocess can
// talk to it over HTTP without any model download.
func mockLocalServer(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "nf", 404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", reply)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runWithEnv(t *testing.T, home string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	env := append(os.Environ(), "TAG_HOME="+home)
	// scrub cloud keys so a fallback test reaches the local step deterministically
	clean := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") || strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			continue
		}
		clean = append(clean, kv)
	}
	cmd.Env = append(clean, extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// TestE2ELocalProvider runs `tag run --provider local` against a mock local
// server (gap #4) and then proves `local` works as the bottom of a fallback
// chain: openai (no key) fails over to the local server.
func TestE2ELocalProvider(t *testing.T) {
	h := newHome(t)
	srv := mockLocalServer(t, "served by local llama")
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1"}

	// direct: --provider local hits the mock server
	out, c := runWithEnv(t, h, env, "run", "hello local", "--provider", "local")
	if c != 0 {
		t.Fatalf("run --provider local failed (%d): %s", c, out)
	}
	if !strings.Contains(out, "served by local llama") {
		t.Errorf("local provider reply missing: %s", out)
	}

	// fallback: openai (no key) -> local/llama-3.2-3b via the chain
	runWithEnv(t, h, env, "set-model", "coder", "openai/gpt-4o-mini")
	runWithEnv(t, h, env, "route-fallback", "add", "--profile", "coder",
		"--primary", "openai/gpt-4o-mini", "--fallback", "local/llama-3.2-3b", "--condition", "always")
	out, c = runWithEnv(t, h, env, "run", "chain to local", "--provider", "openai", "--profile", "coder", "--fallback")
	if c != 0 {
		t.Fatalf("fallback-to-local failed (%d): %s", c, out)
	}
	if !strings.Contains(out, "served by local llama") {
		t.Errorf("expected fallback to reach the local server: %s", out)
	}
	if !strings.Contains(out, "fallback: step 0") {
		t.Errorf("expected the primary (openai) to fail over: %s", out)
	}
}
