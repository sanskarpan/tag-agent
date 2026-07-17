package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

// scriptedProvider drives the agent loop deterministically: turn 1 requests the
// web_search tool; once it sees the tool result (a tool-role message) it emits a
// final answer. This exercises the Exa tool through the REAL agent loop.
type scriptedProvider struct{ toolResult *string }

func (scriptedProvider) Name() string { return "scripted" }
func (p scriptedProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 4)
	sawTool := false
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			sawTool = true
			if p.toolResult != nil {
				*p.toolResult = m.Content
			}
		}
	}
	go func() {
		defer close(ch)
		if !sawTool {
			ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "web_search", Input: map[string]any{"query": "latest go release"}}}
			ch <- llm.Event{Type: llm.EventFinish}
			return
		}
		ch <- llm.Event{Type: llm.EventTextDelta, Text: "done: I searched the web."}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

func TestExaToolThroughAgentLoop(t *testing.T) {
	// mock Exa endpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/search") {
			http.Error(w, "nf", 404)
			return
		}
		w.Write([]byte(`{"results":[{"title":"Go 1.24","url":"https://go.dev","text":"released"}]}`))
	}))
	t.Cleanup(srv.Close)

	reg := agent.NewRegistry()
	Register(reg, Options{DisableBash: true, EnableExa: true, ExaAPIKey: "k", ExaBaseURL: srv.URL})
	if !toolPresent(reg, "web_search") {
		t.Fatal("web_search should be registered")
	}

	var toolResult string
	loop := &agent.Loop{Provider: scriptedProvider{toolResult: &toolResult}, Tools: reg}
	res, err := loop.Run(context.Background(), "what is the latest go release?", agent.Options{MaxSteps: 4})
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	// The tool result fed back to the model must contain the Exa result.
	if !strings.Contains(toolResult, "Go 1.24") || !strings.Contains(toolResult, "https://go.dev") {
		t.Errorf("web_search result not fed back to the model: %q", toolResult)
	}
	if !strings.Contains(res.FinalText, "I searched the web") {
		t.Errorf("final answer missing after tool use: %q", res.FinalText)
	}
}

func toolPresent(reg *agent.Registry, name string) bool {
	for _, d := range reg.Defs() {
		if d.Name == name {
			return true
		}
	}
	return false
}
