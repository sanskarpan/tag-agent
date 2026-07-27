package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/llm"
)

// capturingProvider records the llm.Request it was handed so a test can assert
// how the gateway flattened the inbound messages.
type capturingProvider struct{ got *llm.Request }

func (p *capturingProvider) Name() string { return "capture" }
func (p *capturingProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	*p.got = req
	ch := make(chan llm.Event, 4)
	go func() {
		defer close(ch)
		ch <- llm.Event{Type: llm.EventTextDelta, Text: "ok"}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

func postChat(t *testing.T, url string, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	dec := json.NewDecoder(resp.Body)
	var v any
	if dec.Decode(&v) == nil {
		b, _ := json.Marshal(v)
		sb.Write(b)
	}
	return resp.StatusCode, sb.String()
}

// TestGatewayAcceptsContentParts pins F6: the array ("content parts") form of
// `content` is the NORMATIVE OpenAI shape emitted by LangChain, Open WebUI, the
// OpenAI JS SDK and most proxies. The gateway used to reject all of them with a
// hard 400, which makes "OpenAI-compatible" untrue for real clients.
func TestGatewayAcceptsContentParts(t *testing.T) {
	var got llm.Request
	prov := &capturingProvider{got: &got}
	srv := newTestServer(t, Options{
		AllowUnauthenticated: true,
		Resolve:              func(m string) (llm.Provider, string, error) { return prov, m, nil },
	})

	cases := []struct {
		name, body, wantText string
	}{
		{"single text part",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi there"}]}]}`,
			"hi there"},
		{"multiple text parts",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
			"a\nb"},
		{"plain string still works",
			`{"model":"m","messages":[{"role":"user","content":"plain"}]}`,
			"plain"},
		{"empty parts array",
			`{"model":"m","messages":[{"role":"user","content":[]}]}`,
			""},
		{"null content",
			`{"model":"m","messages":[{"role":"user","content":null}]}`,
			""},
		{"non-text parts are skipped",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/y.png"}},{"type":"text","text":"caption?"}]}]}`,
			"caption?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got = llm.Request{}
			code, body := postChat(t, srv.URL, tc.body)
			if code != 200 {
				t.Fatalf("expected 200, got %d: %s", code, body)
			}
			if len(got.Messages) != 1 {
				t.Fatalf("got %d messages", len(got.Messages))
			}
			if got.Messages[0].Content != tc.wantText {
				t.Errorf("flattened content = %q, want %q", got.Messages[0].Content, tc.wantText)
			}
		})
	}
}

// TestGatewayRejectsBadContentWithoutLeakingGoTypes pins the second half of F6:
// genuinely invalid content must still 400, but the message must not leak a Go
// struct field path / type name.
func TestGatewayRejectsBadContentWithoutLeakingGoTypes(t *testing.T) {
	var got llm.Request
	prov := &capturingProvider{got: &got}
	srv := newTestServer(t, Options{
		AllowUnauthenticated: true,
		Resolve:              func(m string) (llm.Provider, string, error) { return prov, m, nil },
	})
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":42}]}`,
		`{"model":"m","messages":[{"role":"user","content":{"text":"x"}}]}`,
		`{"model":"m","messages":"not-an-array"}`,
		`{not json`,
	} {
		code, resp := postChat(t, srv.URL, body)
		if code != 400 {
			t.Errorf("body %s: expected 400, got %d (%s)", body, code, resp)
		}
		for _, leak := range []string{"Go struct", "chatMessage", "chatRequest", "of type string", "[]gateway."} {
			if strings.Contains(resp, leak) {
				t.Errorf("error leaks internals (%q): %s", leak, resp)
			}
		}
	}
}
