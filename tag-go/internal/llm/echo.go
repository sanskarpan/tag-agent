package llm

import "context"

// EchoProvider is a deterministic offline provider for tests/dev — it streams
// the last user message back as text. Real adapters (anthropic/openai) replace it.
type EchoProvider struct{}

func (EchoProvider) Name() string { return "echo" }

func (EchoProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	ch := make(chan Event, 4)
	go func() {
		defer close(ch)
		var last string
		for _, m := range req.Messages {
			if m.Role == RoleUser {
				last = m.Content
			}
		}
		ch <- Event{Type: EventTextDelta, Text: last}
		ch <- Event{Type: EventUsage, Usage: &Usage{PromptTokens: len(last) / 4, CompletionTokens: len(last) / 4}}
		ch <- Event{Type: EventFinish}
	}()
	return ch, nil
}

func init() { Register(EchoProvider{}) }
