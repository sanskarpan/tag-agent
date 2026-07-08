package llm

import (
	"context"
	"strings"
	"testing"
)

func TestOpenAISSETextStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello "}}]}`,
		`data: {"choices":[{"delta":{"content":"there"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3}}`,
		`data: [DONE]`,
	}, "\n")
	ch := make(chan Event, 32)
	go parseOpenAISSE(strings.NewReader(sse), ch)
	text, calls, usage, finished := collect(ch)
	if text != "Hello there" {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 0 {
		t.Errorf("unexpected tool calls: %+v", calls)
	}
	if usage.PromptTokens != 9 || usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
	if !finished {
		t.Error("should finish")
	}
}

func TestOpenAISSEToolCall(t *testing.T) {
	// tool call assembled across chunks by index; arguments stream as fragments
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")
	ch := make(chan Event, 32)
	go parseOpenAISSE(strings.NewReader(sse), ch)
	_, calls, _, finished := collect(ch)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" || calls[0].ID != "call_1" || calls[0].Input["city"] != "Paris" {
		t.Errorf("tool call assembled wrong: %+v", calls[0])
	}
	if !finished {
		t.Error("should finish")
	}
}

func TestBuildOpenAIBody(t *testing.T) {
	req := Request{
		Model: "gpt-5",
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
			{Role: RoleTool, Content: "42", ToolCallID: "call_x"},
		},
		Tools: []ToolDef{{Name: "calc"}},
	}
	body := buildOpenAIBody(req)
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 3 || msgs[0]["role"] != "system" {
		t.Errorf("openai keeps system in messages: %+v", msgs)
	}
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "call_x" {
		t.Errorf("tool message wrong: %+v", msgs[2])
	}
	if body["stream"] != true {
		t.Error("stream should be true")
	}
}

func TestOpenAIRequiresKey(t *testing.T) {
	p := OpenAIProvider{}
	if p.key() == "" {
		if _, err := p.Stream(context.Background(), Request{Model: "x"}); err == nil {
			t.Error("Stream without a key should error, not hit the network")
		}
	}
}

func TestOpenAIRegistered(t *testing.T) {
	if _, ok := Registry["openai"]; !ok {
		t.Error("openai provider should self-register")
	}
}

func TestBuildOpenAIBodyToolCallLinkage(t *testing.T) {
	req := Request{
		Model: "gpt-5",
		Messages: []Message{
			{Role: RoleUser, Content: "add"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "add", Input: map[string]any{"a": 2.0}}}},
			{Role: RoleTool, Content: "5", ToolCallID: "c1"},
		},
	}
	body := buildOpenAIBody(req)
	msgs := body["messages"].([]map[string]any)
	found := false
	for _, m := range msgs {
		if m["role"] != "assistant" {
			continue
		}
		tcs, ok := m["tool_calls"].([]map[string]any)
		if ok && len(tcs) == 1 && tcs[0]["id"] == "c1" {
			found = true
		}
	}
	if !found {
		t.Errorf("assistant message must carry a tool_calls array: %+v", msgs)
	}
}
