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
//
// When a Screener is installed the wrapper is also the POST hook (PRD-123
// FR-01/G4): the tool's result is screened before it is handed back to the
// model. The side effect has already happened by then — a post-hook cannot
// un-write a file — so a blocked result is WITHHELD and replaced with an honest
// explanation rather than presented as a success or as an empty read.
func Wrap(g *Guard, t agent.Tool, subject SubjectFunc) agent.Tool {
	if subject == nil {
		subject = NoSubject
	}
	inner := t.Exec
	name := t.Def.Name
	t.Exec = func(ctx context.Context, in map[string]any) (string, error) {
		kind, subj := subject(in)
		req := Request{Tool: name, Kind: kind, Subject: subj, Args: in}
		d := g.Check(ctx, req)
		if !d.Allowed() {
			return "", &DeniedError{Request: req, Decision: d}
		}
		out, err := inner(ctx, in)
		if err != nil {
			return out, err
		}
		if g != nil && g.Screener != nil {
			if blocked, reason := g.Screener.ScreenToolResult(ctx, name, out); blocked {
				if reason == "" {
					reason = "the tool result was blocked by a content guardrail"
				}
				post := Decision{Action: Deny, Via: ViaTripwire,
					Rule:   Rule{Tool: name, Action: Deny, Source: SourceTripwire},
					Reason: reason + " (the tool already ran; its output is withheld from the model)"}
				if g.Recorder != nil {
					g.Recorder.Record(req, post)
				}
				return "", &DeniedError{Request: req, Decision: post}
			}
		}
		return out, nil
	}
	return t
}
