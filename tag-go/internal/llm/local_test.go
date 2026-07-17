package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockOpenAICompatServer emulates a local llama.cpp/ollama /v1 server: it accepts
// a chat-completions POST and streams an OpenAI-style SSE response. It records
// the Authorization header it saw so tests can assert the no-key behavior.
func mockOpenAICompatServer(t *testing.T, reply string, gotAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, part := range []string{reply[:1], reply[1:]} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func drainText(ch <-chan Event) (string, *Usage, error) {
	var sb strings.Builder
	var u *Usage
	for ev := range ch {
		switch ev.Type {
		case EventTextDelta:
			sb.WriteString(ev.Text)
		case EventUsage:
			u = ev.Usage
		case EventError:
			return sb.String(), u, ev.Err
		}
	}
	return sb.String(), u, nil
}

func TestLocalProviderStreamsWithoutKey(t *testing.T) {
	var gotAuth string
	srv := mockOpenAICompatServer(t, "local model reply", &gotAuth)
	p := LocalProvider{BaseURL: srv.URL + "/v1"}
	ch, err := p.Stream(context.Background(), Request{Model: "llama-3.2-3b", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	text, u, err := drainText(ch)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if text != "local model reply" {
		t.Errorf("want reply, got %q", text)
	}
	if u == nil || u.PromptTokens != 2 || u.CompletionTokens != 3 {
		t.Errorf("usage not parsed: %+v", u)
	}
	// No key configured → no Authorization header sent (local servers don't need one).
	if gotAuth != "" {
		t.Errorf("expected no Authorization header, got %q", gotAuth)
	}
}

func TestLocalProviderSendsOptionalKey(t *testing.T) {
	var gotAuth string
	srv := mockOpenAICompatServer(t, "ok", &gotAuth)
	p := LocalProvider{BaseURL: srv.URL + "/v1", APIKey: "local-secret"}
	ch, err := p.Stream(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	drainText(ch)
	if gotAuth != "Bearer local-secret" {
		t.Errorf("optional key should be sent when set, got %q", gotAuth)
	}
}

func TestLocalProviderUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", 503)
	}))
	t.Cleanup(srv.Close)
	p := LocalProvider{BaseURL: srv.URL + "/v1"}
	_, err := p.Stream(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "local API 503") {
		t.Fatalf("expected a labeled 503 error, got %v", err)
	}
	// The 503 must be classified as retryable so a fallback chain skips past a
	// local server that isn't ready.
	if !DefaultRetryable(err) {
		t.Errorf("local 503 should be retryable, got not-retryable for %v", err)
	}
}

func TestLocalProviderRegistered(t *testing.T) {
	if _, ok := Registry["local"]; !ok {
		t.Error("LocalProvider must self-register as 'local'")
	}
}

func TestLocalProviderDefaultBaseURL(t *testing.T) {
	t.Setenv("TAG_LOCAL_BASE_URL", "")
	if got := (LocalProvider{}).base(); got != "http://localhost:8080/v1" {
		t.Errorf("default base should be llama.cpp's, got %q", got)
	}
	t.Setenv("TAG_LOCAL_BASE_URL", "http://localhost:11434/v1")
	if got := (LocalProvider{}).base(); got != "http://localhost:11434/v1" {
		t.Errorf("env override not honored, got %q", got)
	}
}
