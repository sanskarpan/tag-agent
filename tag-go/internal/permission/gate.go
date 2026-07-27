package permission

import (
	"context"

	"github.com/tag-agent/tag/internal/agent"
)

// SubjectFunc extracts the permission subject from a tool's raw input. It must
// return an ABSOLUTE, lexically cleaned path for KindPath so traversal syntax
// cannot dodge a rule.
type SubjectFunc func(in map[string]any) (Kind, string)

// NoSubject is the SubjectFunc for tools with no gateable argument.
func NoSubject(map[string]any) (Kind, string) { return KindNone, "" }

// Wrap returns t with its Exec routed through the guard. A denied call returns
// a *DeniedError; the agent loop turns that into an "ERROR: permission denied…"
// tool result, so the model sees an honest refusal (not a fabricated success)
// and the loop continues to the next step.
//
// A nil guard wraps the tool in a fail-CLOSED gate rather than leaving it
// ungated: a wiring mistake must not silently produce an unguarded tool.
func Wrap(g *Guard, t agent.Tool, subject SubjectFunc) agent.Tool {
	if subject == nil {
		subject = NoSubject
	}
	inner := t.Exec
	t.Exec = func(ctx context.Context, in map[string]any) (string, error) {
		kind, subj := subject(in)
		req := Request{Tool: t.Def.Name, Kind: kind, Subject: subj, Args: in}
		d := g.Check(ctx, req)
		if !d.Allowed() {
			return "", &DeniedError{Request: req, Decision: d}
		}
		return inner(ctx, in)
	}
	return t
}
