// Package llm defines the provider-neutral LLM interface and event stream that
// the native agent loop consumes (Track B — replaces the Hermes runtime). Real
// provider adapters (anthropic-sdk-go, openai-go) implement Provider.
package llm

import "context"

// Role is a message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a conversation (provider-neutral).
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// ToolCallID links a tool result (RoleTool) to the call that produced it.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls carries the tool calls an assistant turn requested. Providers
	// must replay these on the assistant message BEFORE the matching tool
	// results, or the API rejects the conversation (Anthropic tool_use /
	// OpenAI tool_calls linkage).
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolDef describes a callable tool exposed to the model.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"input_schema"`
}

// Request is a provider-neutral completion request.
type Request struct {
	Model     string
	Messages  []Message
	Tools     []ToolDef
	MaxTokens int
	// CacheHint asks the adapter to place Anthropic prompt-cache breakpoints on
	// the stable prompt prefix (the last tool definition and the system prompt),
	// so repeated turns reuse the cached prefix. On by default in agent.Options.
	CacheHint bool
}

// EventType tags a streamed event.
type EventType string

const (
	EventTextDelta  EventType = "text_delta"
	EventReasoning  EventType = "reasoning"
	EventToolCall   EventType = "tool_call"
	EventStepFinish EventType = "step_finish"
	EventFinish     EventType = "finish"
	EventUsage      EventType = "usage"
	EventError      EventType = "error"
)

// ToolCall is a requested tool invocation.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// Usage carries token accounting.
type Usage struct {
	PromptTokens, CompletionTokens, CacheReadTokens, CacheCreationTokens int
}

// Event is one item in the provider-neutral output stream.
type Event struct {
	Type     EventType
	Text     string
	ToolCall *ToolCall
	Usage    *Usage
	Err      error
}

// Provider streams a completion as provider-neutral events.
type Provider interface {
	// Name is the provider slug (e.g. "anthropic", "openai").
	Name() string
	// Stream runs one turn, emitting events on the returned channel and closing it when done.
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}

// Registry maps a provider slug to its adapter (populated by adapters at init).
var Registry = map[string]Provider{}

// Register adds a provider adapter.
func Register(p Provider) { Registry[p.Name()] = p }
