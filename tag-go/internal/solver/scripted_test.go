package solver

import (
	"context"

	"github.com/tag-agent/tag/internal/llm"
)

// scriptedProvider is an offline test double that plays a fixed sequence of
// turns. Each turn emits optional text and optional tool calls. Once the script
// is exhausted it emits a final text and stops (no tool calls), so the agent
// loop terminates. It lets the solver unit tests exercise the tool-enabled path
// (write_file etc.) without any network or live model — mirroring what a real
// --provider would do when it decides to edit a file.
type scriptedProvider struct {
	name  string
	turns []scriptedTurn
	final string
	i     int
}

type scriptedTurn struct {
	text  string
	calls []llm.ToolCall
}

func (p *scriptedProvider) Name() string {
	if p.name == "" {
		return "scripted"
	}
	return p.name
}

func (p *scriptedProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 8)
	turn := scriptedTurn{}
	final := false
	if p.i < len(p.turns) {
		turn = p.turns[p.i]
		p.i++
	} else {
		final = true
	}
	go func() {
		defer close(ch)
		if final {
			ch <- llm.Event{Type: llm.EventTextDelta, Text: p.final}
			ch <- llm.Event{Type: llm.EventFinish}
			return
		}
		if turn.text != "" {
			ch <- llm.Event{Type: llm.EventTextDelta, Text: turn.text}
		}
		for i := range turn.calls {
			c := turn.calls[i]
			ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &c}
		}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}
