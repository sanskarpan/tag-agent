// Package gateway serves TAG's native agent loop as an OpenAI-compatible HTTP
// API (gap #1 from the hermes-octo parity review): POST /v1/chat/completions
// (streaming SSE + non-stream), GET /v1/models, and GET /health, behind optional
// bearer-token auth. It is decoupled from the CLI/store: the caller supplies a
// Resolve function that maps a requested model to an llm.Provider (which may be
// a FallbackProvider), so the whole package is testable offline with a mock.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tag-agent/tag/internal/llm"
)

// idCounter makes response ids distinct within a process even when many
// completions land in the same wall-clock second.
var idCounter atomic.Uint64

// maxRequestBytes caps the chat-completions request body to defend against
// memory exhaustion from an oversized (possibly unauthenticated) request.
const maxRequestBytes = 4 << 20 // 4 MiB

// Options configures the gateway handler.
type Options struct {
	// Key is the bearer token required in the Authorization header. When empty,
	// requests are accepted only if AllowUnauthenticated is true (the CLI binds
	// loopback-only in that case).
	Key                  string
	AllowUnauthenticated bool
	// Resolve maps a requested model id to the provider that should serve it and
	// the (bare) model id to send to that provider's adapter.
	Resolve func(model string) (prov llm.Provider, sendModel string, err error)
	// DefaultModel is used when a request omits "model".
	DefaultModel string
	// Models is the list advertised by GET /v1/models.
	Models []string
	// MaxTokens caps completion length when a request doesn't specify one.
	MaxTokens int
	// Now returns the current unix time; injected for deterministic tests.
	Now func() int64
}

// chatRequest is the subset of the OpenAI chat-completions request we honor.
type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Handler builds the OpenAI-compatible mux.
func Handler(opts Options) http.Handler {
	if opts.Now == nil {
		opts.Now = func() int64 { return time.Now().Unix() }
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(opts, r) {
			writeErr(w, 401, "invalid_api_key", "missing or invalid bearer token")
			return
		}
		data := []map[string]any{}
		for _, m := range opts.Models {
			data = append(data, map[string]any{"id": m, "object": "model", "created": opts.Now(), "owned_by": "tag"})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
	})

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, 405, "method_not_allowed", "use POST")
			return
		}
		if !authOK(opts, r) {
			writeErr(w, 401, "invalid_api_key", "missing or invalid bearer token")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "invalid_request_error", "invalid JSON body: "+err.Error())
			return
		}
		if len(req.Messages) == 0 {
			writeErr(w, 400, "invalid_request_error", "messages is required and must be non-empty")
			return
		}
		model := req.Model
		if model == "" {
			model = opts.DefaultModel
		}
		prov, sendModel, err := opts.Resolve(model)
		if err != nil {
			writeErr(w, 400, "invalid_request_error", err.Error())
			return
		}
		maxTok := req.MaxTokens
		if maxTok <= 0 {
			maxTok = opts.MaxTokens
		}
		llmReq := llm.Request{Model: sendModel, Messages: toLLMMessages(req.Messages), MaxTokens: maxTok}

		if req.Stream {
			serveStream(w, r, opts, model, prov, llmReq)
			return
		}
		serveComplete(w, r.Context(), opts, model, prov, llmReq)
	})

	return mux
}

// serveComplete drains the provider stream and returns a single OpenAI
// chat.completion object.
func serveComplete(w http.ResponseWriter, ctx context.Context, opts Options, model string, prov llm.Provider, req llm.Request) {
	ch, err := prov.Stream(ctx, req)
	if err != nil {
		writeErr(w, 502, "upstream_error", err.Error())
		return
	}
	var sb strings.Builder
	var u llm.Usage
	for ev := range ch {
		switch ev.Type {
		case llm.EventTextDelta:
			sb.WriteString(ev.Text)
		case llm.EventUsage:
			if ev.Usage != nil {
				u.PromptTokens += ev.Usage.PromptTokens
				u.CompletionTokens += ev.Usage.CompletionTokens
			}
		case llm.EventError:
			if ev.Err != nil {
				writeErr(w, 502, "upstream_error", ev.Err.Error())
				return
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"id":      "chatcmpl-" + randID(opts.Now()),
		"object":  "chat.completion",
		"created": opts.Now(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": sb.String()},
			"finish_reason": "stop",
		}},
		"usage": usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.PromptTokens + u.CompletionTokens},
	})
}

// serveStream emits OpenAI-style SSE chat.completion.chunk events.
func serveStream(w http.ResponseWriter, r *http.Request, opts Options, model string, prov llm.Provider, req llm.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "server_error", "streaming unsupported")
		return
	}
	ch, err := prov.Stream(r.Context(), req)
	if err != nil {
		writeErr(w, 502, "upstream_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	id := "chatcmpl-" + randID(opts.Now())
	created := opts.Now()

	chunk := func(delta map[string]any, finish any) {
		obj := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(obj)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	// Opening chunk with the role, per OpenAI's streaming contract.
	chunk(map[string]any{"role": "assistant"}, nil)
	for ev := range ch {
		select {
		case <-r.Context().Done():
			return // client disconnected
		default:
		}
		switch ev.Type {
		case llm.EventTextDelta:
			if ev.Text != "" {
				chunk(map[string]any{"content": ev.Text}, nil)
			}
		case llm.EventError:
			if ev.Err != nil {
				// Surface as a final SSE error frame, then close the stream.
				b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": ev.Err.Error(), "type": "upstream_error"}})
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
		}
	}
	chunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func authOK(opts Options, r *http.Request) bool {
	if opts.Key == "" {
		return opts.AllowUnauthenticated
	}
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(h, p))
	return subtle.ConstantTimeCompare([]byte(got), []byte(opts.Key)) == 1
}

func toLLMMessages(in []chatMessage) []llm.Message {
	out := make([]llm.Message, 0, len(in))
	for _, m := range in {
		role := llm.RoleUser
		switch strings.ToLower(m.Role) {
		case "system":
			role = llm.RoleSystem
		case "assistant":
			role = llm.RoleAssistant
		case "tool":
			role = llm.RoleTool
		}
		out = append(out, llm.Message{Role: role, Content: m.Content})
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": typ}})
}

// randID derives a short, non-cryptographic id for response ids by combining the
// timestamp with a monotonic per-process counter, so distinct responses (even
// within the same second, or concurrent) get distinct ids without depending on
// math/rand (unavailable in some sandboxes).
func randID(seed int64) string {
	n := idCounter.Add(1)
	return strconv.FormatInt(seed, 36) + strconv.FormatUint(n, 36)
}
