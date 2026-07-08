package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/llm"
)

// scriptedProvider emits a scripted sequence of event-batches, one per Stream call.
type scriptedProvider struct {
	batches [][]llm.Event
	calls   int
	seen    [][]llm.Message // messages received on each Stream call
}

func (p *scriptedProvider) Name() string { return "scripted" }
func (p *scriptedProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 8)
	i := p.calls
	p.calls++
	p.seen = append(p.seen, req.Messages)
	go func() {
		defer close(ch)
		if i < len(p.batches) {
			for _, ev := range p.batches[i] {
				ch <- ev
			}
		}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

func TestRegistryDefsSortedByName(t *testing.T) {
	reg := NewRegistry()
	for _, n := range []string{"zeta", "alpha", "mid"} {
		reg.Add(Tool{Def: llm.ToolDef{Name: n}})
	}
	defs := reg.Defs()
	want := []string{"alpha", "mid", "zeta"}
	if len(defs) != len(want) {
		t.Fatalf("expected %d defs, got %d", len(want), len(defs))
	}
	for i, n := range want {
		if defs[i].Name != n {
			t.Fatalf("Defs() must be sorted by name, got %v at %d (want %s)", defs[i].Name, i, n)
		}
	}
}

func TestLoopEchoTerminates(t *testing.T) {
	// EchoProvider never requests tools -> loop finishes in one step.
	l := &Loop{Provider: llm.EchoProvider{}}
	res, err := l.Run(context.Background(), "hello there", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "done" || res.FinalText != "hello there" || len(res.Steps) != 1 {
		t.Errorf("echo loop: stopped=%s final=%q steps=%d", res.Stopped, res.FinalText, len(res.Steps))
	}
}

func TestLoopExecutesToolThenFinishes(t *testing.T) {
	// Turn 1: model asks for the "adder" tool. Turn 2: model returns final text.
	prov := &scriptedProvider{batches: [][]llm.Event{
		{
			{Type: llm.EventTextDelta, Text: "let me add"},
			{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "call1", Name: "adder", Input: map[string]any{"a": 2.0, "b": 3.0}}},
			{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5}},
		},
		{
			{Type: llm.EventTextDelta, Text: "the sum is 5"},
			{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 8, CompletionTokens: 4}},
		},
	}}
	reg := NewRegistry()
	var toolGotInput map[string]any
	reg.Add(Tool{
		Def: llm.ToolDef{Name: "adder", Description: "add two numbers"},
		Exec: func(ctx context.Context, in map[string]any) (string, error) {
			toolGotInput = in
			return "5", nil
		},
	})
	l := &Loop{Provider: prov, Tools: reg}
	res, err := l.Run(context.Background(), "what is 2+3?", Options{MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "done" {
		t.Errorf("should finish after tool call, stopped=%s", res.Stopped)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(res.Steps))
	}
	if len(res.Steps[0].ToolCalls) != 1 || res.Steps[0].ToolCalls[0].Result != "5" {
		t.Errorf("step 1 should execute the adder tool: %+v", res.Steps[0].ToolCalls)
	}
	if toolGotInput["a"] != 2.0 {
		t.Errorf("tool should receive its input, got %+v", toolGotInput)
	}
	if !strings.Contains(res.FinalText, "sum is 5") {
		t.Errorf("final text wrong: %q", res.FinalText)
	}
	// usage accumulates across both turns
	if res.TotalUsage.PromptTokens != 18 || res.TotalUsage.CompletionTokens != 9 {
		t.Errorf("usage should accumulate: %+v", res.TotalUsage)
	}
}

func TestLoopUnknownToolReportsError(t *testing.T) {
	prov := &scriptedProvider{batches: [][]llm.Event{
		{{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "ghost"}}},
		{{Type: llm.EventTextDelta, Text: "done"}},
	}}
	l := &Loop{Provider: prov, Tools: NewRegistry()}
	res, err := l.Run(context.Background(), "go", Options{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) == 0 || len(res.Steps[0].ToolCalls) != 1 || !strings.Contains(res.Steps[0].ToolCalls[0].Err, "unknown tool") {
		t.Errorf("unknown tool should be reported: %+v", res.Steps)
	}
}

func TestLoopRespectsMaxSteps(t *testing.T) {
	// A provider that always requests a tool -> loop must stop at MaxSteps.
	loopEv := [][]llm.Event{}
	for i := 0; i < 10; i++ {
		loopEv = append(loopEv, []llm.Event{{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "c", Name: "noop"}}})
	}
	prov := &scriptedProvider{batches: loopEv}
	reg := NewRegistry()
	reg.Add(Tool{Def: llm.ToolDef{Name: "noop"}, Exec: func(ctx context.Context, in map[string]any) (string, error) { return "ok", nil }})
	l := &Loop{Provider: prov, Tools: reg}
	res, err := l.Run(context.Background(), "spin", Options{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "max_steps" || len(res.Steps) != 3 {
		t.Errorf("should stop at max_steps=3, got stopped=%s steps=%d", res.Stopped, len(res.Steps))
	}
}

// TestLoopSendsToolCallLinkage asserts the assistant turn that requested tools
// is replayed WITH its tool calls before the tool results (the CRITICAL bug:
// without this, Anthropic/OpenAI reject the follow-up request).
func TestLoopSendsToolCallLinkage(t *testing.T) {
	prov := &scriptedProvider{batches: [][]llm.Event{
		{
			{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "call1", Name: "adder", Input: map[string]any{"a": 2.0}}},
			{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 10}},
			{Type: llm.EventUsage, Usage: &llm.Usage{CompletionTokens: 5}}, // two usage events, one turn
		},
		{{Type: llm.EventTextDelta, Text: "done"}},
	}}
	reg := NewRegistry()
	reg.Add(Tool{Def: llm.ToolDef{Name: "adder"}, Exec: func(ctx context.Context, in map[string]any) (string, error) { return "5", nil }})
	l := &Loop{Provider: prov, Tools: reg}
	if _, err := l.Run(context.Background(), "2+? ", Options{MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	// Two Stream calls happened; the SECOND request's messages must contain an
	// assistant message carrying the tool call, positioned before the tool result.
	if len(prov.seen) < 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(prov.seen))
	}
	turn2 := prov.seen[1]
	var asstIdx, toolIdx = -1, -1
	for i, m := range turn2 {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "call1" {
			asstIdx = i
		}
		if m.Role == llm.RoleTool && m.ToolCallID == "call1" {
			toolIdx = i
		}
	}
	if asstIdx < 0 {
		t.Fatalf("turn-2 messages must include an assistant message with the tool call: %+v", turn2)
	}
	if toolIdx < 0 || toolIdx <= asstIdx {
		t.Errorf("tool result must follow the assistant tool-call message (asst=%d tool=%d)", asstIdx, toolIdx)
	}
}

// TestLoopAccumulatesMultiUsageEvents: two usage events in one turn are summed.
func TestLoopAccumulatesMultiUsageEvents(t *testing.T) {
	prov := &scriptedProvider{batches: [][]llm.Event{{
		{Type: llm.EventTextDelta, Text: "hi"},
		{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 100, CacheReadTokens: 20}}, // message_start style
		{Type: llm.EventUsage, Usage: &llm.Usage{CompletionTokens: 50}},                   // message_delta style
	}}}
	l := &Loop{Provider: prov}
	res, err := l.Run(context.Background(), "x", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalUsage.PromptTokens != 100 || res.TotalUsage.CompletionTokens != 50 || res.TotalUsage.CacheReadTokens != 20 {
		t.Errorf("usage should accumulate across events, got %+v", res.TotalUsage)
	}
}
