package llm

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func collect(ch <-chan Event) (text string, calls []*ToolCall, usage Usage, finished bool) {
	for ev := range ch {
		switch ev.Type {
		case EventTextDelta:
			text += ev.Text
		case EventToolCall:
			calls = append(calls, ev.ToolCall)
		case EventUsage:
			if ev.Usage != nil {
				usage.PromptTokens += ev.Usage.PromptTokens
				usage.CompletionTokens += ev.Usage.CompletionTokens
				usage.CacheReadTokens += ev.Usage.CacheReadTokens
				usage.CacheCreationTokens += ev.Usage.CacheCreationTokens
			}
		case EventFinish:
			finished = true
		}
	}
	return
}

func TestAnthropicSSETextStream(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":4,"cache_creation_input_tokens":3}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello "}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	ch := make(chan Event, 32)
	go parseAnthropicSSE(strings.NewReader(sse), ch)
	text, calls, usage, finished := collect(ch)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 0 {
		t.Errorf("unexpected tool calls: %+v", calls)
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 7 || usage.CacheReadTokens != 4 || usage.CacheCreationTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
	if !finished {
		t.Error("stream should finish")
	}
}

func TestAnthropicSSEToolUse(t *testing.T) {
	// tool_use block with input JSON streamed across two input_json_delta events
	sse := strings.Join([]string{
		`data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
		`data: {"type":"content_block_stop"}`,
		`data: {"type":"message_stop"}`,
	}, "\n")
	ch := make(chan Event, 32)
	go parseAnthropicSSE(strings.NewReader(sse), ch)
	_, calls, _, finished := collect(ch)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" || calls[0].ID != "toolu_1" {
		t.Errorf("tool call meta wrong: %+v", calls[0])
	}
	if calls[0].Input["city"] != "Paris" {
		t.Errorf("tool input should be assembled from streamed JSON: %+v", calls[0].Input)
	}
	if !finished {
		t.Error("stream should finish")
	}
}

func TestBuildAnthropicBody(t *testing.T) {
	req := Request{
		Model: "claude-opus-4-8",
		Messages: []Message{
			{Role: RoleSystem, Content: "be terse"},
			{Role: RoleUser, Content: "hi"},
			{Role: RoleTool, Content: "42", ToolCallID: "toolu_x"},
		},
		Tools: []ToolDef{{Name: "calc", Description: "math"}},
	}
	body := buildAnthropicBody(req)
	if body["system"] != "be terse" {
		t.Errorf("system should be hoisted: %v", body["system"])
	}
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 2 {
		t.Fatalf("system must not be in messages; got %d messages", len(msgs))
	}
	// tool result becomes a user tool_result block
	last := msgs[1]
	if last["role"] != "user" {
		t.Errorf("tool result should be a user message: %+v", last)
	}
	if body["stream"] != true {
		t.Error("stream should be true")
	}
	if _, ok := body["tools"]; !ok {
		t.Error("tools should be included")
	}
}

func TestAnthropicSSETruncationSurfacesError(t *testing.T) {
	// A reader that fails mid-stream (connection reset) must yield EventError,
	// never a clean EventFinish.
	sse := `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}` + "\n"
	r := io.MultiReader(strings.NewReader(sse), iotest.ErrReader(errors.New("connection reset")))
	ch := make(chan Event, 32)
	go parseAnthropicSSE(r, ch)
	var gotErr error
	finished := false
	for ev := range ch {
		switch ev.Type {
		case EventError:
			gotErr = ev.Err
		case EventFinish:
			finished = true
		}
	}
	if gotErr == nil {
		t.Fatal("truncated stream must emit EventError")
	}
	if finished {
		t.Error("truncated stream must not emit EventFinish")
	}
}

func TestBuildAnthropicBodyCacheHint(t *testing.T) {
	req := Request{
		Model: "claude-opus-4-8",
		Messages: []Message{
			{Role: RoleSystem, Content: "be terse"},
			{Role: RoleUser, Content: "hi"},
		},
		Tools:     []ToolDef{{Name: "alpha"}, {Name: "beta"}},
		CacheHint: true,
	}
	body := buildAnthropicBody(req)
	sys, ok := body["system"].([]map[string]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("CacheHint system should be a content-block array: %v", body["system"])
	}
	if sys[0]["type"] != "text" || sys[0]["text"] != "be terse" {
		t.Errorf("system block wrong: %+v", sys[0])
	}
	cc, ok := sys[0]["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Errorf("system block must carry an ephemeral cache_control: %+v", sys[0])
	}
	tools := body["tools"].([]map[string]any)
	if _, ok := tools[0]["cache_control"]; ok {
		t.Errorf("only the last tool gets the breakpoint: %+v", tools[0])
	}
	tcc, ok := tools[1]["cache_control"].(map[string]any)
	if !ok || tcc["type"] != "ephemeral" {
		t.Errorf("last tool must carry an ephemeral cache_control: %+v", tools[1])
	}
}

func TestAnthropicRequiresKey(t *testing.T) {
	p := AnthropicProvider{} // no key, and we never set ANTHROPIC_API_KEY in tests
	if key := p.key(); key == "" {
		// Stream must refuse without a key (no network attempted)
		_, err := p.Stream(context.Background(), Request{Model: "x"})
		if err == nil {
			t.Error("Stream without an API key should error, not call the network")
		}
	}
}

func TestAnthropicRegistered(t *testing.T) {
	if _, ok := Registry["anthropic"]; !ok {
		t.Error("anthropic provider should self-register")
	}
}

func TestBuildAnthropicBodyToolUseLinkage(t *testing.T) {
	// An assistant message carrying a tool call must render as a tool_use block.
	req := Request{
		Model: "claude-opus-4-8",
		Messages: []Message{
			{Role: RoleUser, Content: "add 2 and 3"},
			{Role: RoleAssistant, Content: "sure", ToolCalls: []ToolCall{{ID: "tu1", Name: "add", Input: map[string]any{"a": 2.0}}}},
			{Role: RoleTool, Content: "5", ToolCallID: "tu1"},
		},
	}
	body := buildAnthropicBody(req)
	msgs := body["messages"].([]map[string]any)
	// find the assistant message and verify it has a tool_use block with id tu1
	found := false
	for _, m := range msgs {
		if m["role"] != "assistant" {
			continue
		}
		blocks, ok := m["content"].([]map[string]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			if b["type"] == "tool_use" && b["id"] == "tu1" && b["name"] == "add" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("assistant message must render a tool_use block for tu1: %+v", msgs)
	}
}
