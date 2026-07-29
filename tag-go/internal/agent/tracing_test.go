package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/trace"
)

// spanTree indexes recorded spans by id for parent assertions.
func spanTree(t *testing.T, rec *trace.Recorder) (map[string]trace.Span, []trace.Span) {
	t.Helper()
	spans := rec.Spans()
	byID := make(map[string]trace.Span, len(spans))
	for _, s := range spans {
		byID[s.ID] = s
	}
	return byID, spans
}

// TestLoopEmitsAgentAndLLMSpans: every Run must produce an agent root plus one
// llm span per provider turn (#590 — the loop produced none at all).
func TestLoopEmitsAgentAndLLMSpans(t *testing.T) {
	rec := trace.NewRecorder("tr-1", "coder")
	l := &Loop{Provider: llm.EchoProvider{}, Tracer: rec}
	if _, err := l.Run(context.Background(), "hello", Options{Model: "openai/gpt-4o-mini"}); err != nil {
		t.Fatal(err)
	}
	byID, spans := spanTree(t, rec)
	if len(spans) != 2 {
		t.Fatalf("want agent+llm spans, got %d: %+v", len(spans), spans)
	}
	root, llmSpan := spans[0], spans[1]
	if root.Kind != trace.KindAgent || root.Name != "agent.run" {
		t.Errorf("root = %s/%s, want agent/agent.run", root.Kind, root.Name)
	}
	if root.ParentID != "" {
		t.Errorf("root must have no parent, got %q", root.ParentID)
	}
	if llmSpan.Kind != trace.KindLLM || llmSpan.Name != "llm.call" {
		t.Errorf("turn span = %s/%s, want llm/llm.call", llmSpan.Kind, llmSpan.Name)
	}
	if _, ok := byID[llmSpan.ParentID]; !ok || llmSpan.ParentID != root.ID {
		t.Errorf("llm parent = %q, want root %q", llmSpan.ParentID, root.ID)
	}
	if root.TraceID != "tr-1" || llmSpan.TraceID != "tr-1" {
		t.Error("spans must carry the caller's trace id")
	}
	if root.Profile != "coder" {
		t.Errorf("profile = %q, want coder", root.Profile)
	}
	if root.Status != trace.StatusOK || llmSpan.Status != trace.StatusOK {
		t.Error("a clean run must record ok spans")
	}
	if llmSpan.CostUSD == nil || *llmSpan.CostUSD <= 0 {
		t.Errorf("llm span must carry a cost for a priced model, got %v", llmSpan.CostUSD)
	}
	// The root must not restate the turn's tokens (double-count guard, #586).
	if root.PromptTokens != 0 || root.CompletionTokens != 0 {
		t.Errorf("root tokens = %d/%d, want 0/0", root.PromptTokens, root.CompletionTokens)
	}
}

// TestLoopEmitsToolChildSpans: PRD-048 — a tool call is a child of the llm turn
// that requested it, not a sibling.
func TestLoopEmitsToolChildSpans(t *testing.T) {
	p := &scriptedProvider{batches: [][]llm.Event{
		{{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "echo_tool", Input: map[string]any{}}}},
		{{Type: llm.EventTextDelta, Text: "done"}},
	}}
	reg := NewRegistry()
	reg.Add(Tool{
		Def:  llm.ToolDef{Name: "echo_tool"},
		Exec: func(ctx context.Context, in map[string]any) (string, error) { return "ok", nil },
	})
	rec := trace.NewRecorder("tr-2", "coder")
	l := &Loop{Provider: p, Tools: reg, Tracer: rec}
	if _, err := l.Run(context.Background(), "go", Options{Model: "openai/gpt-4o-mini"}); err != nil {
		t.Fatal(err)
	}
	byID, spans := spanTree(t, rec)
	var tool *trace.Span
	for i := range spans {
		if spans[i].Kind == trace.KindTool {
			tool = &spans[i]
		}
	}
	if tool == nil {
		t.Fatalf("no tool span recorded: %+v", spans)
	}
	if tool.Name != "echo_tool" {
		t.Errorf("tool span name = %q", tool.Name)
	}
	parent, ok := byID[tool.ParentID]
	if !ok || parent.Kind != trace.KindLLM {
		t.Fatalf("tool parent %q is not the llm turn", tool.ParentID)
	}
	grand, ok := byID[parent.ParentID]
	if !ok || grand.Kind != trace.KindAgent {
		t.Fatalf("llm turn must nest under the agent root")
	}
	// The tool call happened in turn 1, so its parent must be the FIRST llm span.
	if parent.Attributes["tag.step"] != 1 {
		t.Errorf("tool parent step = %v, want turn 1", parent.Attributes["tag.step"])
	}
	if tool.Attributes["gen_ai.tool.call.id"] != "c1" {
		t.Errorf("tool span lost the provider call id: %v", tool.Attributes)
	}
}

// TestLoopToolErrorMarksSpanError: a failing tool must produce an error span
// rather than a silently ok one.
func TestLoopToolErrorMarksSpanError(t *testing.T) {
	p := &scriptedProvider{batches: [][]llm.Event{
		{{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "boom", Input: map[string]any{}}}},
		{{Type: llm.EventTextDelta, Text: "recovered"}},
	}}
	reg := NewRegistry()
	reg.Add(Tool{
		Def:  llm.ToolDef{Name: "boom"},
		Exec: func(ctx context.Context, in map[string]any) (string, error) { return "", errors.New("kaboom") },
	})
	rec := trace.NewRecorder("tr-3", "")
	l := &Loop{Provider: p, Tools: reg, Tracer: rec}
	if _, err := l.Run(context.Background(), "go", Options{}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range rec.Spans() {
		if s.Kind != trace.KindTool {
			continue
		}
		found = true
		if s.Status != trace.StatusError {
			t.Errorf("failed tool span status = %q, want error", s.Status)
		}
		if s.ErrorMsg != "kaboom" {
			t.Errorf("error_msg = %q, want kaboom", s.ErrorMsg)
		}
	}
	if !found {
		t.Fatal("no tool span")
	}
}

// errProvider fails the Stream call outright.
type errProvider struct{}

func (errProvider) Name() string { return "err" }
func (errProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	return nil, errors.New("provider exploded")
}

// TestLoopTracesProviderFailure: an untraced failure is the exact blind spot
// #590 describes, so a provider error must still close the spans as errors.
func TestLoopTracesProviderFailure(t *testing.T) {
	rec := trace.NewRecorder("tr-4", "")
	l := &Loop{Provider: errProvider{}, Tracer: rec}
	if _, err := l.Run(context.Background(), "go", Options{}); err == nil {
		t.Fatal("expected a provider error")
	}
	spans := rec.Spans()
	if len(spans) == 0 {
		t.Fatal("a failed run must still be traced")
	}
	for _, s := range spans {
		if s.Status != trace.StatusError {
			t.Errorf("span %s status = %q, want error", s.Name, s.Status)
		}
		if s.FinishedAt.IsZero() {
			t.Errorf("span %s left open", s.Name)
		}
	}
}

// TestLoopWithoutTracerIsUnchanged: tracing is opt-in and a nil Tracer must not
// panic anywhere on the hot path.
func TestLoopWithoutTracerIsUnchanged(t *testing.T) {
	l := &Loop{Provider: llm.EchoProvider{}}
	res, err := l.Run(context.Background(), "hello", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != "hello" || res.Stopped != "done" {
		t.Fatalf("untraced run changed behaviour: %+v", res)
	}
}

// TestLoopNestsUnderParentSpan: a caller-owned parent (e.g. a solver iteration)
// must become the root span's parent.
func TestLoopNestsUnderParentSpan(t *testing.T) {
	rec := trace.NewRecorder("tr-5", "")
	outer := rec.Start("solve", trace.KindAgent, "", "")
	l := &Loop{Provider: llm.EchoProvider{}, Tracer: rec, ParentSpanID: outer.ID}
	if _, err := l.Run(context.Background(), "hi", Options{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range rec.Spans() {
		if s.Name == "agent.run" && s.ParentID != outer.ID {
			t.Fatalf("agent.run parent = %q, want %q", s.ParentID, outer.ID)
		}
	}
}
