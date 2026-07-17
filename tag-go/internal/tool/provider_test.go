package tool

import (
	"context"

	"github.com/tag-agent/tag/internal/llm"
)

type oneShotProvider struct {
	name  string
	input map[string]any
	calls int
}

func (p *oneShotProvider) Name() string { return "oneshot" }
func (p *oneShotProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 4)
	i := p.calls
	p.calls++
	go func() {
		defer close(ch)
		if i == 0 {
			ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: p.name, Input: p.input}}
		} else {
			ch <- llm.Event{Type: llm.EventTextDelta, Text: "done"}
		}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}
