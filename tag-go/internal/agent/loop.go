// Package agent implements the native agent loop (Track B): it drives a provider-
// neutral llm.Provider through tool-calling turns until the model stops requesting
// tools or a step cap is hit. This is the core of runtime ownership — it replaces
// the delegated Hermes loop. It is fully exercisable offline via llm.EchoProvider.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/trace"
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
	// Tracer, when non-nil, makes the loop emit spans: one `agent.run` root per
	// Run, one `llm.call` child per provider turn, and one child of that turn per
	// tool call (PRD-013/046/048). Nil disables tracing entirely — every
	// recorder method is nil-safe, so no call site needs a branch.
	//
	// Only the `llm.call` spans carry tokens/cost. The root span deliberately
	// reports zero so summing the `spans` population does not double-count a run
	// against its own turns (`tag costs` already labels runs and spans as
	// separate populations with an overlap_warning — see #586).
	Tracer *trace.Recorder
	// ParentSpanID nests this Run's root span under a caller-owned span (e.g. a
	// solver iteration). Empty makes `agent.run` the trace root.
	ParentSpanID string
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
	// Root span for the whole loop. Every early return below closes it, so a
	// provider error still produces a traced (status=error) run rather than a
	// silently missing trace.
	root := l.Tracer.Start("agent.run", trace.KindAgent, l.ParentSpanID, opts.Model)
	l.Tracer.Attr(root, "tag.max_steps", opts.MaxSteps)
	if l.Provider != nil {
		l.Tracer.Attr(root, "tag.provider", l.Provider.Name())
	}
	// The attribute above already treats a nil provider as reachable; the stream
	// call below would then panic. A misconfigured loop must fail as an error the
	// caller can report, not as a crash.
	if l.Provider == nil {
		err := errors.New("agent: no provider configured")
		l.Tracer.End(root, trace.StatusError, err.Error(), 0, 0)
		return nil, err
	}
	closeRoot := func(err error) {
		if err != nil {
			l.Tracer.End(root, trace.StatusError, err.Error(), 0, 0)
			return
		}
		l.Tracer.Attr(root, "tag.steps", len(res.Steps))
		l.Tracer.Attr(root, "tag.stopped", res.Stopped)
		l.Tracer.End(root, trace.StatusOK, "", 0, 0)
	}
	for step := 0; step < opts.MaxSteps; step++ {
		req := llm.Request{Model: opts.Model, Messages: msgs, CacheHint: opts.CacheHint}
		if l.Tools != nil {
			req.Tools = l.Tools.Defs()
		}
		llmSpan := l.Tracer.Start("llm.call", trace.KindLLM, spanID(root), opts.Model)
		l.Tracer.Attr(llmSpan, "gen_ai.request.model", opts.Model)
		l.Tracer.Attr(llmSpan, "tag.step", step+1)
		ch, err := l.Provider.Stream(ctx, req)
		if err != nil {
			l.Tracer.End(llmSpan, trace.StatusError, err.Error(), 0, 0)
			closeRoot(err)
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
					l.Tracer.End(llmSpan, trace.StatusError, ev.Err.Error(), usage.PromptTokens, usage.CompletionTokens)
					closeRoot(ev.Err)
					return nil, ev.Err
				}
			}
		}
		// The turn is complete: close its span with the usage it reported, which
		// is what gives PRD-046 a per-span cost.
		l.Tracer.Attr(llmSpan, "gen_ai.usage.input_tokens", usage.PromptTokens)
		l.Tracer.Attr(llmSpan, "gen_ai.usage.output_tokens", usage.CompletionTokens)
		l.Tracer.Attr(llmSpan, "tag.tool_calls", len(calls))
		l.Tracer.End(llmSpan, trace.StatusOK, "", usage.PromptTokens, usage.CompletionTokens)
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
			closeRoot(nil)
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
			// PRD-048: a tool call is a real child span of the turn that requested
			// it, so the trace tree reflects causality rather than a flat list.
			toolSpan := l.Tracer.Start(call.Name, trace.KindTool, spanID(llmSpan), opts.Model)
			l.Tracer.Attr(toolSpan, "gen_ai.tool.name", call.Name)
			l.Tracer.Attr(toolSpan, "tag.step", step+1)
			if call.ID != "" {
				l.Tracer.Attr(toolSpan, "gen_ai.tool.call.id", call.ID)
			}
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
			if ec.Err != "" {
				l.Tracer.End(toolSpan, trace.StatusError, ec.Err, 0, 0)
			} else {
				l.Tracer.End(toolSpan, trace.StatusOK, "", 0, 0)
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
	closeRoot(nil)
	return res, nil
}

// spanID is the parent id to hand to a child span: the span's id, or "" when
// tracing is off (nil span), which makes the child a no-op too.
func spanID(s *trace.Span) string {
	if s == nil {
		return ""
	}
	return s.ID
}

func (l *Loop) tool(name string) (Tool, bool) {
	if l.Tools == nil {
		return Tool{}, false
	}
	t, ok := l.Tools.tools[name]
	return t, ok
}
