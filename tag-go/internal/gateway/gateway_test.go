package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tag-agent/tag/internal/llm"
)

// fixedProvider streams a fixed text (or an error) as provider-neutral events.
type fixedProvider struct {
	text     string
	errText  string
	gotModel *string
}

func (p *fixedProvider) Name() string { return "fixed" }
func (p *fixedProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	if p.gotModel != nil {
		*p.gotModel = req.Model
	}
	ch := make(chan llm.Event, 8)
	go func() {
		defer close(ch)
		if p.errText != "" {
			ch <- llm.Event{Type: llm.EventError, Err: &strErr{p.errText}}
			return
		}
		// stream the text a few chars at a time to exercise SSE chunking
		for _, part := range []string{p.text[:1], p.text[1:]} {
			ch <- llm.Event{Type: llm.EventTextDelta, Text: part}
		}
		ch <- llm.Event{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 3, CompletionTokens: 4}}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }

func newTestServer(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	if opts.Now == nil {
		opts.Now = func() int64 { return 1700000000 }
	}
	srv := httptest.NewServer(Handler(opts))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t, Options{AllowUnauthenticated: true, Resolve: nil})
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health should be 200, got %d", resp.StatusCode)
	}
}

func TestChatCompletionNonStream(t *testing.T) {
	var gotModel string
	srv := newTestServer(t, Options{
		AllowUnauthenticated: true,
		DefaultModel:         "tag-default",
		Resolve: func(model string) (llm.Provider, string, error) {
			return &fixedProvider{text: "hello there", gotModel: &gotModel}, "bare-model", nil
		},
	})
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role, Content string
			}
			FinishReason string `json:"finish_reason"`
		}
		Usage usage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || out.Model != "gpt-4o-mini" {
		t.Errorf("bad envelope: %+v", out)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "hello there" {
		t.Errorf("bad content: %+v", out.Choices)
	}
	if out.Choices[0].Message.Role != "assistant" || out.Choices[0].FinishReason != "stop" {
		t.Errorf("bad role/finish: %+v", out.Choices[0])
	}
	if out.Usage.TotalTokens != 7 {
		t.Errorf("usage total should be 7, got %d", out.Usage.TotalTokens)
	}
	if gotModel != "bare-model" {
		t.Errorf("resolver's send-model must reach the provider, got %q", gotModel)
	}
}

func TestChatCompletionStream(t *testing.T) {
	srv := newTestServer(t, Options{
		AllowUnauthenticated: true,
		Resolve: func(model string) (llm.Provider, string, error) {
			return &fixedProvider{text: "streamed!"}, model, nil
		},
	})
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"go"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("want SSE content-type, got %q", ct)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	full := string(buf[:n])
	// keep reading to EOF
	for {
		m, err := resp.Body.Read(buf)
		full += string(buf[:m])
		if err != nil {
			break
		}
	}
	if !strings.Contains(full, `"delta":{"role":"assistant"}`) {
		t.Errorf("missing opening role chunk: %s", full)
	}
	if !strings.Contains(full, `"content":"s"`) || !strings.Contains(full, `"content":"treamed!"`) {
		t.Errorf("missing content deltas: %s", full)
	}
	if !strings.Contains(full, `"finish_reason":"stop"`) {
		t.Errorf("missing finish chunk: %s", full)
	}
	if !strings.Contains(full, "data: [DONE]") {
		t.Errorf("missing [DONE] sentinel: %s", full)
	}
	if strings.Contains(full, "chat.completion.chunk") == false {
		t.Errorf("chunks must be object chat.completion.chunk: %s", full)
	}
}

func TestAuthRequired(t *testing.T) {
	srv := newTestServer(t, Options{
		Key: "secret-token",
		Resolve: func(model string) (llm.Provider, string, error) {
			return &fixedProvider{text: "x"}, model, nil
		},
	})
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	// no auth -> 401
	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if resp.StatusCode != 401 {
		t.Errorf("no token should be 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// wrong token -> 401
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong")
	r2, _ := http.DefaultClient.Do(req)
	if r2.StatusCode != 401 {
		t.Errorf("wrong token should be 401, got %d", r2.StatusCode)
	}
	r2.Body.Close()
	// correct token -> 200
	req3, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req3.Header.Set("Authorization", "Bearer secret-token")
	r3, _ := http.DefaultClient.Do(req3)
	if r3.StatusCode != 200 {
		t.Errorf("correct token should be 200, got %d", r3.StatusCode)
	}
	r3.Body.Close()
}

func TestModelsEndpoint(t *testing.T) {
	srv := newTestServer(t, Options{AllowUnauthenticated: true, Models: []string{"gpt-4o-mini", "claude-haiku"}})
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Object != "list" || len(out.Data) != 2 || out.Data[0].ID != "gpt-4o-mini" || out.Data[0].Object != "model" {
		t.Errorf("bad models list: %+v", out)
	}
}

func TestBadRequests(t *testing.T) {
	srv := newTestServer(t, Options{AllowUnauthenticated: true, Resolve: func(m string) (llm.Provider, string, error) {
		return &fixedProvider{text: "x"}, m, nil
	}})
	// empty messages -> 400
	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if resp.StatusCode != 400 {
		t.Errorf("empty messages should be 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// bad JSON -> 400
	r2, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{bad`))
	if r2.StatusCode != 400 {
		t.Errorf("bad JSON should be 400, got %d", r2.StatusCode)
	}
	r2.Body.Close()
	// GET -> 405
	r3, _ := http.Get(srv.URL + "/v1/chat/completions")
	if r3.StatusCode != 405 {
		t.Errorf("GET should be 405, got %d", r3.StatusCode)
	}
	r3.Body.Close()
}

// blockingProvider streams events on a buffered channel until either its total
// budget is exhausted or ctx is done, then closes `done` to signal the producer
// goroutine terminated. It deliberately does NOT watch ctx while blocked on a
// channel send, reproducing the real SSE parsers (parseOpenAISSE /
// parseAnthropicSSE) which send with no ctx awareness (#565). The producer can
// only unblock if the consumer drains the channel.
type blockingProvider struct {
	total int
	done  chan struct{}
}

func (p *blockingProvider) Name() string { return "blocking" }
func (p *blockingProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 4) // small buffer so it fills fast
	go func() {
		defer close(ch)
		defer close(p.done)
		for i := 0; i < p.total; i++ {
			// Plain send — no <-ctx.Done() select. Blocks once the buffer fills and
			// the consumer stops reading, exactly like the production parsers.
			ch <- llm.Event{Type: llm.EventTextDelta, Text: "x"}
		}
	}()
	return ch, nil
}

// TestStreamClientDisconnectUnblocksProducer covers #565: when the client
// disconnects mid-stream, serveStream must drain the provider channel so the
// producer goroutine terminates promptly instead of blocking on a full buffer
// until the request timeout. Before the fix, serveStream returned WITHOUT
// draining and this test would hang (producer never closes `done`).
func TestStreamClientDisconnectUnblocksProducer(t *testing.T) {
	done := make(chan struct{})
	prov := &blockingProvider{total: 100000, done: done}
	srv := newTestServer(t, Options{
		AllowUnauthenticated: true,
		Resolve: func(model string) (llm.Provider, string, error) {
			return prov, model, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"go"}]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	// Read one chunk so the stream is live, then simulate a client disconnect.
	buf := make([]byte, 256)
	_, _ = resp.Body.Read(buf)
	cancel()
	resp.Body.Close()

	// The producer must terminate (drain lets its blocked send proceed). If the
	// fix is absent, `done` never closes and this times out → the test fails.
	select {
	case <-done:
		// producer goroutine exited — success
	case <-time.After(5 * time.Second):
		t.Fatal("producer goroutine did not terminate after client disconnect (#565: serveStream must drain the channel)")
	}
}

func TestUpstreamError(t *testing.T) {
	srv := newTestServer(t, Options{AllowUnauthenticated: true, Resolve: func(m string) (llm.Provider, string, error) {
		return &fixedProvider{errText: "429 rate limit"}, m, nil
	}})
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("upstream error should be 502, got %d", resp.StatusCode)
	}
}
