package llm

import (
	"fmt"
	"strings"
	"testing"
)

// drainFirstErr runs a parser over body and returns the first EventError text.
func drainFirstErr(parse func(r *strings.Reader, ch chan<- Event), body string) string {
	ch := make(chan Event, 256)
	go parse(strings.NewReader(body), ch)
	errText := ""
	for ev := range ch {
		if ev.Type == EventError && ev.Err != nil && errText == "" {
			errText = ev.Err.Error()
		}
	}
	return errText
}

// TestOpenAIToolArgsAreBounded: the 4 MiB per-LINE scanner cap does not bound
// arguments that arrive as many small frames and are accumulated across them.
// Pre-fix this grew without limit (CWE-400).
func TestOpenAIToolArgsAreBounded(t *testing.T) {
	chunk := strings.Repeat("A", 32*1024)
	var b strings.Builder
	// Enough frames to blow past MaxToolArgBytes on a single call.
	for i := 0; i < (MaxToolArgBytes/len(chunk))+4; i++ {
		fmt.Fprintf(&b, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`+"\n", chunk)
	}
	errText := drainFirstErr(func(r *strings.Reader, ch chan<- Event) { parseOpenAISSE(r, ch, "openai") }, b.String())
	if !strings.Contains(errText, "per-tool-call argument limit") {
		t.Fatalf("unbounded tool arguments were accepted; got error %q", errText)
	}
}

// TestOpenAIToolCallCountIsBounded: the accumulator map is keyed by a
// PEER-SUPPLIED index, so a hostile peer could mint unlimited accumulators.
func TestOpenAIToolCallCountIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < MaxToolCallsPerResponse+8; i++ {
		fmt.Fprintf(&b, `data: {"choices":[{"delta":{"tool_calls":[{"index":%d,"id":"t","function":{"name":"f","arguments":"{}"}}]}}]}`+"\n", i)
	}
	errText := drainFirstErr(func(r *strings.Reader, ch chan<- Event) { parseOpenAISSE(r, ch, "openai") }, b.String())
	if !strings.Contains(errText, "tool-call limit") {
		t.Fatalf("unbounded tool-call count was accepted; got error %q", errText)
	}
}

// TestOpenAIToolArgTotalIsBounded: each call stays under the per-call cap, but
// the sum across calls must also be bounded.
func TestOpenAIToolArgTotalIsBounded(t *testing.T) {
	half := strings.Repeat("B", MaxToolArgBytes/2)
	var b strings.Builder
	for i := 0; i < MaxToolCallsPerResponse; i++ {
		fmt.Fprintf(&b, `data: {"choices":[{"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%q}}]}}]}`+"\n", i, half)
	}
	errText := drainFirstErr(func(r *strings.Reader, ch chan<- Event) { parseOpenAISSE(r, ch, "openai") }, b.String())
	if !strings.Contains(errText, "total tool-argument limit") &&
		!strings.Contains(errText, "per-tool-call argument limit") {
		t.Fatalf("unbounded total tool arguments were accepted; got error %q", errText)
	}
}

// TestAnthropicToolArgsAreBounded: same class on the Messages API, where
// arguments stream as input_json_delta fragments.
func TestAnthropicToolArgsAreBounded(t *testing.T) {
	chunk := strings.Repeat("C", 32*1024)
	var b strings.Builder
	b.WriteString(`data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"t1","name":"f"}}` + "\n")
	for i := 0; i < (MaxToolArgBytes/len(chunk))+4; i++ {
		fmt.Fprintf(&b, `data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":%q}}`+"\n", chunk)
	}
	errText := drainFirstErr(func(r *strings.Reader, ch chan<- Event) { parseAnthropicSSE(r, ch) }, b.String())
	if !strings.Contains(errText, "per-tool-call argument limit") {
		t.Fatalf("unbounded anthropic tool arguments were accepted; got error %q", errText)
	}
}

// TestBoundsDoNotBreakOrdinaryStreams guards against a cap that cries wolf: a
// normal tool call well under every bound must still parse and emit its call.
func TestBoundsDoNotBreakOrdinaryStreams(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}` + "\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}` + "\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n" +
		"data: [DONE]\n"
	ch := make(chan Event, 64)
	go func() { parseOpenAISSE(strings.NewReader(body), ch, "openai") }()
	sawCall, sawErr := false, ""
	for ev := range ch {
		switch ev.Type {
		case EventToolCall:
			sawCall = true
			if ev.ToolCall.Name != "read_file" {
				t.Errorf("tool name = %q, want read_file", ev.ToolCall.Name)
			}
			if got := ev.ToolCall.Input["path"]; got != "a.txt" {
				t.Errorf("reassembled args wrong: path=%v", got)
			}
		case EventError:
			if ev.Err != nil {
				sawErr = ev.Err.Error()
			}
		}
	}
	if !sawCall {
		t.Fatalf("an ordinary tool call was not emitted (err=%q)", sawErr)
	}
	if sawErr != "" {
		t.Fatalf("an ordinary stream produced an error: %s", sawErr)
	}
}
