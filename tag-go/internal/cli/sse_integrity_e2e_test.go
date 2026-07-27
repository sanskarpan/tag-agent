package cli_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mock200Server answers every POST with HTTP 200 and the given content-type +
// body — the shape real proxies/CDNs return when the upstream is unhealthy.
func mock200Server(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(200)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestE2ENonSSE200IsNotFakeSuccess pins F1 at the CLI boundary: a 200 that is
// not a terminated SSE stream must exit non-zero with a diagnosable error, not
// exit 0 with an empty answer.
func TestE2ENonSSE200IsNotFakeSuccess(t *testing.T) {
	cases := []struct {
		name, ct, body, want string
	}{
		{"stream flag ignored", "application/json",
			`{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"THE REAL ANSWER"}}]}`,
			"not text/event-stream"},
		{"200 quota error", "application/json",
			`{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`,
			"quota exceeded"},
		{"cdn html page", "text/html", "<html><body>502 Bad Gateway</body></html>", "not text/event-stream"},
		{"empty body", "text/event-stream", "", "no SSE data frames"},
		{"no terminator", "text/event-stream",
			"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", "without a [DONE] terminator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHome(t)
			srv := mock200Server(t, tc.ct, tc.body)
			out, code := runWithEnv(t, h, []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1"},
				"run", "hi", "--provider", "local")
			if code == 0 {
				t.Fatalf("exit 0 on a non-SSE 200 is a fake success; output: %s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("error should mention %q; got: %s", tc.want, out)
			}
		})
	}
}

// TestE2EQuotaError200TriggersFallback pins F1's worst consequence: a
// quota-exhausted provider that answers 200 with an error payload must fail
// over to the next step of the chain instead of burning the request on a
// silent, empty "success".
func TestE2EQuotaError200TriggersFallback(t *testing.T) {
	h := newHome(t)
	quota := mock200Server(t, "application/json",
		`{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`)
	good := mockLocalServer(t, "served by the fallback")
	env := []string{
		"OPENAI_API_KEY=sk-mock-not-a-real-key",
		"OPENAI_BASE_URL=" + quota.URL + "/v1",
		"TAG_LOCAL_BASE_URL=" + good.URL + "/v1",
	}
	runWithEnv(t, h, env, "set-model", "coder", "openai/gpt-4o-mini")
	runWithEnv(t, h, env, "route-fallback", "add", "--profile", "coder",
		"--primary", "openai/gpt-4o-mini", "--fallback", "local/llama-3.2-3b", "--condition", "always")
	out, code := runWithEnv(t, h, env, "run", "chain", "--provider", "openai", "--profile", "coder", "--fallback")
	if code != 0 {
		t.Fatalf("fallback run failed (%d): %s", code, out)
	}
	if !strings.Contains(out, "fallback: step 0") {
		t.Errorf("a 200-with-error-payload primary must fail over: %s", out)
	}
	if !strings.Contains(out, "served by the fallback") {
		t.Errorf("expected the fallback step's answer: %s", out)
	}
}
