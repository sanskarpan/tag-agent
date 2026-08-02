package llm

import "fmt"

// Bounds on what a streaming provider peer can make this process allocate.
//
// The SSE scanners already cap a single LINE (4 MiB, see sc.Buffer). That is a
// different question from what these bound: tool-call arguments arrive as many
// small, individually-legal fragments that are ACCUMULATED across frames, so a
// peer can stay far under the line cap forever and still grow the process
// without limit. Three separate dimensions were unbounded (CWE-400):
//
//   - per-argument size — one tool call whose arguments never stop streaming
//   - tool-call count   — the OpenAI accumulator is keyed by a peer-supplied
//     `index`, so a hostile peer can mint unlimited distinct accumulators
//   - total across all accumulators in one response
//
// A provider is not a trusted peer. It may be a local proxy, an
// OpenAI-compatible gateway, or a misbehaving server; "our vendor wouldn't do
// that" is not a memory bound.
//
// Exceeding a bound is an ERROR, never a truncation. Silently cutting tool-call
// arguments would hand the model a JSON fragment that parses to something
// *different* from what the provider sent — a wrong tool invocation is worse
// than a failed one.
const (
	// MaxToolArgBytes caps one tool call's accumulated arguments.
	MaxToolArgBytes = 1 << 20 // 1 MiB
	// MaxToolCallsPerResponse caps distinct tool calls in a single response.
	MaxToolCallsPerResponse = 64
	// MaxToolArgTotalBytes caps the sum across every accumulator in a response.
	MaxToolArgTotalBytes = 4 << 20 // 4 MiB
)

// toolAccountant tracks streamed tool-call growth for one response and reports
// the first breach. The zero value is ready to use.
type toolAccountant struct {
	total int
	calls int
}

// addCall registers a newly-seen tool call. Returns an error once the peer has
// exceeded MaxToolCallsPerResponse.
func (t *toolAccountant) addCall(label string) error {
	t.calls++
	if t.calls > MaxToolCallsPerResponse {
		return fmt.Errorf("%s stream exceeded the tool-call limit (%d calls); "+
			"refusing to keep accumulating", label, MaxToolCallsPerResponse)
	}
	return nil
}

// addArgs registers n more argument bytes for a call that already holds cur.
// Returns an error on the first breach of either the per-call or total bound.
func (t *toolAccountant) addArgs(label string, cur, n int) error {
	if cur+n > MaxToolArgBytes {
		return fmt.Errorf("%s stream exceeded the per-tool-call argument limit "+
			"(%d bytes); refusing to keep accumulating", label, MaxToolArgBytes)
	}
	t.total += n
	if t.total > MaxToolArgTotalBytes {
		return fmt.Errorf("%s stream exceeded the total tool-argument limit "+
			"(%d bytes across all tool calls); refusing to keep accumulating",
			label, MaxToolArgTotalBytes)
	}
	return nil
}
