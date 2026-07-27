package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// drainStrict reports the error event (if any) and whether EventFinish arrived.
func drainStrict(ch <-chan Event) (text string, err error, finished bool) {
	for ev := range ch {
		switch ev.Type {
		case EventTextDelta:
			text += ev.Text
		case EventError:
			if err == nil {
				err = ev.Err
			}
		case EventFinish:
			finished = true
		}
	}
	return
}

// --- F1: parser-level. A body that is not a terminated SSE stream must never
// be reported as a successful, empty completion. ---

func TestOpenAISSEBodyWithoutDataFramesIsError(t *testing.T) {
	cases := map[string]string{
		"empty body":      "",
		"html error page": "<html><head><title>502 Bad Gateway</title></head><body>cdn</body></html>",
		"chat.completion": `{"id":"c1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"THE REAL ANSWER"}}]}`,
		"error object":    `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ch := make(chan Event, 32)
			go parseOpenAISSE(strings.NewReader(body), ch, "openai")
			_, err, finished := drainStrict(ch)
			if finished {
				t.Error("a non-SSE 200 body must NOT emit EventFinish (fake success)")
			}
			if err == nil {
				t.Fatal("a non-SSE 200 body must emit EventError")
			}
		})
	}
}

func TestOpenAISSEWithoutTerminatorIsError(t *testing.T) {
	// Well-formed frames, but the body ends with no [DONE] and no finish_reason:
	// a truncated stream, not a completed one.
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	ch := make(chan Event, 32)
	go parseOpenAISSE(strings.NewReader(sse), ch, "openai")
	text, err, finished := drainStrict(ch)
	if text != "partial" {
		t.Errorf("text = %q", text)
	}
	if finished {
		t.Error("an unterminated stream must NOT emit EventFinish")
	}
	if err == nil {
		t.Fatal("an unterminated stream must emit EventError")
	}
}

func TestOpenAISSEFinishReasonIsAValidTerminator(t *testing.T) {
	// Some OpenAI-compatible servers close after the finish_reason chunk without
	// sending [DONE]. That is a complete response and must still succeed.
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n"
	ch := make(chan Event, 32)
	go parseOpenAISSE(strings.NewReader(sse), ch, "openai")
	text, err, finished := drainStrict(ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "ok" || !finished {
		t.Errorf("text=%q finished=%v", text, finished)
	}
}

func TestAnthropicSSEBodyWithoutDataFramesIsError(t *testing.T) {
	cases := map[string]string{
		"empty body":      "",
		"html error page": "<html><body>cdn error</body></html>",
		"messages object": `{"id":"msg_1","type":"message","content":[{"type":"text","text":"THE REAL ANSWER"}]}`,
		"error object":    `{"type":"error","error":{"type":"rate_limit_error","message":"quota exceeded"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ch := make(chan Event, 32)
			go parseAnthropicSSE(strings.NewReader(body), ch)
			_, err, finished := drainStrict(ch)
			if finished {
				t.Error("a non-SSE 200 body must NOT emit EventFinish (fake success)")
			}
			if err == nil {
				t.Fatal("a non-SSE 200 body must emit EventError")
			}
		})
	}
}

func TestAnthropicSSEWithoutMessageStopIsError(t *testing.T) {
	sse := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n"
	ch := make(chan Event, 32)
	go parseAnthropicSSE(strings.NewReader(sse), ch)
	text, err, finished := drainStrict(ch)
	if text != "partial" {
		t.Errorf("text = %q", text)
	}
	if finished {
		t.Error("a stream with no message_stop must NOT emit EventFinish")
	}
	if err == nil {
		t.Fatal("a stream with no message_stop must emit EventError")
	}
}

// --- F1: transport-level. A 200 whose Content-Type is not text/event-stream is
// not a stream; a 200 carrying {"error":...} is an upstream failure. Both must
// surface as errors from Stream so the fallback chain can fire. ---

func serveOnce(t *testing.T, ct, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(200)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIStream200NonSSEContentTypeIsError(t *testing.T) {
	for name, tc := range map[string]struct{ ct, body, want string }{
		"ignored stream:true": {"application/json", `{"object":"chat.completion","choices":[{"message":{"content":"REAL"}}]}`, "text/event-stream"},
		"200 error object":    {"application/json", `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`, "quota exceeded"},
		"cdn html page":       {"text/html", "<html>502</html>", "text/event-stream"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := serveOnce(t, tc.ct, tc.body)
			ch, err := OpenAIProvider{APIKey: "k", BaseURL: srv.URL + "/v1"}.Stream(context.Background(), Request{Model: "m"})
			if err == nil {
				_, serr, finished := drainStrict(ch)
				t.Fatalf("Stream returned no error (finished=%v, streamErr=%v) — fake success", finished, serr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestAnthropicStream200NonSSEContentTypeIsError(t *testing.T) {
	for name, tc := range map[string]struct{ ct, body, want string }{
		"ignored stream:true": {"application/json", `{"type":"message","content":[{"type":"text","text":"REAL"}]}`, "text/event-stream"},
		"200 error object":    {"application/json", `{"type":"error","error":{"type":"rate_limit_error","message":"quota exceeded"}}`, "quota exceeded"},
		"cdn html page":       {"text/html", "<html>502</html>", "text/event-stream"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := serveOnce(t, tc.ct, tc.body)
			ch, err := AnthropicProvider{APIKey: "k", BaseURL: srv.URL}.Stream(context.Background(), Request{Model: "m"})
			if err == nil {
				_, serr, finished := drainStrict(ch)
				t.Fatalf("Stream returned no error (finished=%v, streamErr=%v) — fake success", finished, serr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// TestOpenAIStream200EmptySSEBodyIsError covers the Content-Length: 0 case: the
// content-type is right, so only the parser can catch it.
func TestOpenAIStream200EmptySSEBodyIsError(t *testing.T) {
	srv := serveOnce(t, "text/event-stream", "")
	ch, err := OpenAIProvider{APIKey: "k", BaseURL: srv.URL + "/v1"}.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		return // acceptable: rejected at the transport
	}
	_, serr, finished := drainStrict(ch)
	if finished {
		t.Error("an empty 200 SSE body must NOT emit EventFinish")
	}
	if serr == nil {
		t.Fatal("an empty 200 SSE body must emit EventError")
	}
}
