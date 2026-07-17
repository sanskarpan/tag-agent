// Package agent implements the native agent loop (Track B): it drives a provider-
// neutral llm.Provider through tool-calling turns until the model stops requesting
// tools or a step cap is hit. This is the core of runtime ownership — it replaces
// the delegated Hermes loop. It is fully exercisable offline via llm.EchoProvider.
package agent

import (
	"context"
	"fmt"
	"sort"

	"github.com/tag-agent/tag/internal/llm"
)

// ToolFunc executes a tool call and returns its result text.
type ToolFunc func(ctx context.Context, input map[string]any) (string, error)

// Tool couples a tool definition with its executor.
type Tool struct {
	Def  llm.ToolDef
	Exec ToolFunc
}

// Registry maps tool name -> tool.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry builds an empty tool registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Add registers a tool.
func (r *Registry) Add(t Tool) { r.tools[t.Def.Name] = t }

// Defs returns the tool definitions for the provider request, sorted by name
// so the rendered prompt prefix is stable across requests (prompt caching).
func (r *Registry) Defs() []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.Def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Step is one turn of the loop (assistant text + any tool calls executed).
type Step struct {
	Text      string
	ToolCalls []ExecutedCall
	Usage     llm.Usage
}

// ExecutedCall records a tool call and its result.
type ExecutedCall struct {
	Name   string
	Input  map[string]any
	Result string
	Err    string
}

// Result is the outcome of a full loop run.
type Result struct {
	Steps      []Step
	FinalText  string
	TotalUsage llm.Usage
	Stopped    string // "done" | "max_steps"
}

// Options configures a Run.
type Options struct {
	Model     string
	System    string
	MaxSteps  int
	CacheHint bool
}

// Loop drives a provider through tool-calling turns.
type Loop struct {
	Provider llm.Provider
	Tools    *Registry
}

// Run executes the agent loop for a user message.
func (l *Loop) Run(ctx context.Context, userMessage string, opts Options) (*Result, error) {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 8
	}
	msgs := []llm.Message{}
	if opts.System != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: opts.System})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: userMessage})

	res := &Result{}
	for step := 0; step < opts.MaxSteps; step++ {
		req := llm.Request{Model: opts.Model, Messages: msgs, CacheHint: opts.CacheHint}
		if l.Tools != nil {
			req.Tools = l.Tools.Defs()
		}
		ch, err := l.Provider.Stream(ctx, req)
		if err != nil {
			return nil, err
		}
		var text string
		var calls []*llm.ToolCall
		var usage llm.Usage
		for ev := range ch {
			switch ev.Type {
			case llm.EventTextDelta:
				text += ev.Text
			case llm.EventToolCall:
				if ev.ToolCall != nil {
					calls = append(calls, ev.ToolCall)
				}
			case llm.EventUsage:
				// A single turn may emit usage in multiple events (Anthropic sends
				// prompt/cache in message_start and completion in message_delta);
				// accumulate each field rather than overwriting.
				if ev.Usage != nil {
					usage.PromptTokens += ev.Usage.PromptTokens
					usage.CompletionTokens += ev.Usage.CompletionTokens
					usage.CacheReadTokens += ev.Usage.CacheReadTokens
					usage.CacheCreationTokens += ev.Usage.CacheCreationTokens
				}
			case llm.EventError:
				if ev.Err != nil {
					return nil, ev.Err
				}
			}
		}
		res.TotalUsage.PromptTokens += usage.PromptTokens
		res.TotalUsage.CompletionTokens += usage.CompletionTokens
		res.TotalUsage.CacheReadTokens += usage.CacheReadTokens
		res.TotalUsage.CacheCreationTokens += usage.CacheCreationTokens

		s := Step{Text: text, Usage: usage}
		if len(calls) == 0 {
			// no tools requested -> loop is done
			s.ToolCalls = nil
			res.Steps = append(res.Steps, s)
			res.FinalText = text
			res.Stopped = "done"
			return res, nil
		}
		// Record the assistant turn WITH its tool calls so the provider replays
		// the tool_use/tool_calls linkage before the tool results (else Anthropic/
		// OpenAI reject the next request). Always append this message even when
		// text is empty — the tool calls must be present.
		asst := llm.Message{Role: llm.RoleAssistant, Content: text}
		for _, c := range calls {
			asst.ToolCalls = append(asst.ToolCalls, *c)
		}
		msgs = append(msgs, asst)
		// execute each requested tool, feed results back as tool messages
		for _, call := range calls {
			ec := ExecutedCall{Name: call.Name, Input: call.Input}
			tool, ok := l.tool(call.Name)
			if !ok {
				ec.Err = fmt.Sprintf("unknown tool %q", call.Name)
			} else {
				out, err := tool.Exec(ctx, call.Input)
				if err != nil {
					ec.Err = err.Error()
				} else {
					ec.Result = out
				}
			}
			s.ToolCalls = append(s.ToolCalls, ec)
			resultText := ec.Result
			if ec.Err != "" {
				resultText = "ERROR: " + ec.Err
			}
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, Content: resultText, ToolCallID: call.ID})
		}
		res.Steps = append(res.Steps, s)
	}
	res.Stopped = "max_steps"
	if n := len(res.Steps); n > 0 {
		res.FinalText = res.Steps[n-1].Text
	}
	return res, nil
}

func (l *Loop) tool(name string) (Tool, bool) {
	if l.Tools == nil {
		return Tool{}, false
	}
	t, ok := l.Tools.tools[name]
	return t, ok
}
