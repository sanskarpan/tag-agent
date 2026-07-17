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

// AnthropicProvider calls the Anthropic Messages API (streaming SSE) directly
// over net/http — no SDK dependency, keeping the single static binary lean. It
// implements Provider. The API key comes from ANTHROPIC_API_KEY (or the struct).
// Network I/O only happens inside Stream; the SSE decoding is a pure function
// (parseAnthropicSSE) unit-tested offline.
type AnthropicProvider struct {
	APIKey     string
	BaseURL    string // default https://api.anthropic.com
	HTTPClient *http.Client
	Version    string // anthropic-version header, default 2023-06-01
}

func (AnthropicProvider) Name() string { return "anthropic" }

func (p AnthropicProvider) key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	return os.Getenv("ANTHROPIC_API_KEY")
}

// Stream sends the request and decodes the SSE response into provider-neutral events.
func (p AnthropicProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if p.key() == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY is not set")
	}
	base := p.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	version := p.Version
	if version == "" {
		version = "2023-06-01"
	}
	body := buildAnthropicBody(req)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", p.key())
	httpReq.Header.Set("anthropic-version", version)
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
		return nil, fmt.Errorf("anthropic API %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	ch := make(chan Event, 16)
	go func() {
		defer resp.Body.Close()
		parseAnthropicSSE(resp.Body, ch)
	}()
	return ch, nil
}

// buildAnthropicBody maps a provider-neutral Request onto the Messages API shape.
// The system prompt is hoisted to the top-level "system" field (Anthropic keeps
// it out of the messages array); tool results become user-role tool_result blocks.
func buildAnthropicBody(req Request) map[string]any {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	var system string
	var messages []map[string]any
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
		case RoleTool:
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content,
				}},
			})
		case RoleAssistant:
			// An assistant turn that requested tools must be sent as content
			// blocks: [optional text] + one tool_use block per call, so the
			// following tool_result blocks have a matching tool_use_id.
			if len(m.ToolCalls) > 0 {
				var blocks []map[string]any
				if m.Content != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
				}
				for _, tc := range m.ToolCalls {
					input := tc.Input
					if input == nil {
						input = map[string]any{}
					}
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": input})
				}
				messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
			} else {
				messages = append(messages, map[string]any{"role": "assistant", "content": m.Content})
			}
		default:
			messages = append(messages, map[string]any{"role": string(m.Role), "content": m.Content})
		}
	}
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": maxTokens,
		"messages":   messages,
		"stream":     true,
	}
	if system != "" {
		if req.CacheHint {
			// A breakpoint on the last system block caches the tools+system prefix
			// (tools render before system in the Messages API prompt).
			body["system"] = []map[string]any{{
				"type": "text", "text": system,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
		} else {
			body["system"] = system
		}
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			schema := t.Schema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": schema})
		}
		if req.CacheHint {
			tools[len(tools)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		body["tools"] = tools
	}
	return body
}

// parseAnthropicSSE decodes the Messages API event stream into Events and closes ch.
func parseAnthropicSSE(r io.Reader, ch chan<- Event) {
	defer close(ch)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// accumulate the current tool_use block (id/name + streamed partial JSON)
	var curToolID, curToolName string
	var curToolJSON strings.Builder
	inToolBlock := false

	flushTool := func() {
		if !inToolBlock {
			return
		}
		input := map[string]any{}
		if s := curToolJSON.String(); strings.TrimSpace(s) != "" {
			_ = json.Unmarshal([]byte(s), &input)
		}
		ch <- Event{Type: EventToolCall, ToolCall: &ToolCall{ID: curToolID, Name: curToolName, Input: input}}
		inToolBlock = false
		curToolID, curToolName = "", ""
		curToolJSON.Reset()
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
		var ev struct {
			Type         string `json:"type"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage *struct {
				InputTokens     int `json:"input_tokens"`
				OutputTokens    int `json:"output_tokens"`
				CacheReadTokens int `json:"cache_read_input_tokens"`
			} `json:"usage"`
			Message *struct {
				Usage *struct {
					InputTokens         int `json:"input_tokens"`
					CacheReadTokens     int `json:"cache_read_input_tokens"`
					CacheCreationTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "error":
			// A mid-stream error frame must surface, not be silently finished.
			msg := "anthropic stream error"
			if ev.Error != nil {
				msg = fmt.Sprintf("anthropic stream error (%s): %s", ev.Error.Type, ev.Error.Message)
			}
			ch <- Event{Type: EventError, Err: fmt.Errorf("%s", msg)}
			return
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				ch <- Event{Type: EventUsage, Usage: &Usage{
					PromptTokens:        ev.Message.Usage.InputTokens,
					CacheReadTokens:     ev.Message.Usage.CacheReadTokens,
					CacheCreationTokens: ev.Message.Usage.CacheCreationTokens,
				}}
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				flushTool()
				inToolBlock = true
				curToolID = ev.ContentBlock.ID
				curToolName = ev.ContentBlock.Name
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					ch <- Event{Type: EventTextDelta, Text: ev.Delta.Text}
				}
			case "input_json_delta":
				curToolJSON.WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			flushTool()
		case "message_delta":
			if ev.Usage != nil {
				ch <- Event{Type: EventUsage, Usage: &Usage{CompletionTokens: ev.Usage.OutputTokens}}
			}
		case "message_stop":
			flushTool()
			ch <- Event{Type: EventFinish}
			return
		}
	}
	if err := sc.Err(); err != nil {
		ch <- Event{Type: EventError, Err: fmt.Errorf("anthropic stream read: %w", err)}
		return
	}
	flushTool()
	ch <- Event{Type: EventFinish}
}

func init() { Register(AnthropicProvider{}) }
