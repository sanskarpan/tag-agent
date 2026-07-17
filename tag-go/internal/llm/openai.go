package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// OpenAIProvider calls the OpenAI Chat Completions API (streaming SSE) directly
// over net/http. Implements Provider. Key from OPENAI_API_KEY (or the struct).
// SSE decoding (parseOpenAISSE) is a pure function, unit-tested offline.
type OpenAIProvider struct {
	APIKey     string
	BaseURL    string // default https://api.openai.com/v1
	HTTPClient *http.Client
}

func (OpenAIProvider) Name() string { return "openai" }

func (p OpenAIProvider) key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	return os.Getenv("OPENAI_API_KEY")
}

// Stream sends the request and decodes the SSE response into provider-neutral events.
func (p OpenAIProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if p.key() == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is not set")
	}
	base := p.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	b, err := json.Marshal(buildOpenAIBody(req))
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+p.key())
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("openai API %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	ch := make(chan Event, 16)
	go func() {
		defer resp.Body.Close()
		parseOpenAISSE(resp.Body, ch)
	}()
	return ch, nil
}

// buildOpenAIBody maps a provider-neutral Request onto Chat Completions shape.
// System stays as a system-role message; tool results become tool-role messages.
func buildOpenAIBody(req Request) map[string]any {
	var messages []map[string]any
	for _, m := range req.Messages {
		switch m.Role {
		case RoleTool:
			messages = append(messages, map[string]any{"role": "tool", "content": m.Content, "tool_call_id": m.ToolCallID})
		case RoleAssistant:
			// An assistant turn that requested tools must carry a tool_calls array
			// so the following tool-role messages link by tool_call_id.
			if len(m.ToolCalls) > 0 {
				var tcs []map[string]any
				for _, tc := range m.ToolCalls {
					argsJSON, _ := json.Marshal(tc.Input)
					if tc.Input == nil {
						argsJSON = []byte("{}")
					}
					tcs = append(tcs, map[string]any{
						"id": tc.ID, "type": "function",
						"function": map[string]any{"name": tc.Name, "arguments": string(argsJSON)},
					})
				}
				msg := map[string]any{"role": "assistant", "tool_calls": tcs}
				if m.Content != "" {
					msg["content"] = m.Content
				}
				messages = append(messages, msg)
			} else {
				messages = append(messages, map[string]any{"role": "assistant", "content": m.Content})
			}
		default:
			messages = append(messages, map[string]any{"role": string(m.Role), "content": m.Content})
		}
	}
	body := map[string]any{
		"model":          req.Model,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			schema := t.Schema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": schema,
			}})
		}
		body["tools"] = tools
	}
	return body
}

// parseOpenAISSE decodes the Chat Completions event stream into Events.
func parseOpenAISSE(r io.Reader, ch chan<- Event) {
	defer close(ch)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// tool calls are assembled by index: id/name arrive once, arguments stream
	type acc struct {
		id, name string
		args     strings.Builder
	}
	toolAcc := map[int]*acc{}
	var order []int

	flush := func() {
		for _, idx := range order {
			a := toolAcc[idx]
			input := map[string]any{}
			if s := a.args.String(); strings.TrimSpace(s) != "" {
				_ = json.Unmarshal([]byte(s), &input)
			}
			ch <- Event{Type: EventToolCall, ToolCall: &ToolCall{ID: a.id, Name: a.name, Input: input}}
		}
		toolAcc = map[int]*acc{}
		order = nil
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			// A mid-stream error chunk must surface, not be silently finished.
			ch <- Event{Type: EventError, Err: fmt.Errorf("openai stream error (%s): %s", chunk.Error.Type, chunk.Error.Message)}
			return
		}
		if chunk.Usage != nil {
			ch <- Event{Type: EventUsage, Usage: &Usage{PromptTokens: chunk.Usage.PromptTokens, CompletionTokens: chunk.Usage.CompletionTokens}}
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				ch <- Event{Type: EventTextDelta, Text: c.Delta.Content}
			}
			for _, tc := range c.Delta.ToolCalls {
				a := toolAcc[tc.Index]
				if a == nil {
					a = &acc{}
					toolAcc[tc.Index] = a
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					a.id = tc.ID
				}
				if tc.Function.Name != "" {
					a.name = tc.Function.Name
				}
				a.args.WriteString(tc.Function.Arguments)
			}
			if c.FinishReason != nil && *c.FinishReason == "tool_calls" {
				flush()
			}
		}
	}
	if err := sc.Err(); err != nil {
		ch <- Event{Type: EventError, Err: fmt.Errorf("openai stream read: %w", err)}
		return
	}
	flush()
	ch <- Event{Type: EventFinish}
}

func init() { Register(OpenAIProvider{}) }
