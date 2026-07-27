package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HTTP 200 is not proof of a stream. Several real upstreams answer 200 with
// something that is not an SSE event stream at all:
//
//   - the server ignored "stream":true and returned a plain chat.completion /
//     messages JSON object (the real answer is in there, and silently lost);
//   - a proxy/gateway (LiteLLM et al.) returned 200 carrying {"error":{...}};
//   - a CDN returned a 200 HTML error page;
//   - the body is empty.
//
// Treating any of these as a finished-but-empty completion is worse than a
// crash: the run "succeeds" with no answer, and — because there is no error —
// the route-fallback chain never fires, so a quota-exhausted provider burns the
// request instead of failing over. checkStreamResponse rejects those bodies at
// the transport, before a single event is emitted.
//
// The caller owns resp.Body and must close it when a non-nil error is returned.
func checkStreamResponse(resp *http.Response, errLabel string) error {
	ct := resp.Header.Get("Content-Type")
	mt := strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	// An absent Content-Type is tolerated (some minimal local servers omit it);
	// the parser's terminator check still guards that path.
	if mt == "" || mt == "text/event-stream" {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if msg := topLevelErrorMessage(body); msg != "" {
		return fmt.Errorf("%s API returned HTTP 200 carrying an error payload: %s", errLabel, msg)
	}
	return fmt.Errorf("%s API returned HTTP 200 with content-type %q, not text/event-stream "+
		"(the upstream ignored stream:true, or a proxy replaced the response): %s",
		errLabel, ct, snippet(body))
}

// topLevelErrorMessage extracts the human-readable message from an OpenAI- or
// Anthropic-shaped top-level error object, tolerating the string form some
// proxies emit ({"error":"quota exceeded"}). It returns "" when the body is not
// an error object.
func topLevelErrorMessage(body []byte) string {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || len(env.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(env.Error, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(env.Error, &obj) != nil {
		return ""
	}
	msg := strings.TrimSpace(obj.Message)
	if msg == "" {
		msg = strings.TrimSpace(obj.Code)
	}
	if msg == "" {
		return ""
	}
	if obj.Type != "" {
		return fmt.Sprintf("%s (%s)", msg, obj.Type)
	}
	return msg
}

// snippet renders an opaque body as a short single-line excerpt so the error is
// diagnosable without dumping an HTML page into the terminal.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}
