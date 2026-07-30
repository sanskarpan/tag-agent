package swarm

import (
	"context"
	"fmt"
	"strings"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/trace"
)

// coordinatorPrompt is Python's _COORDINATOR_SYSTEM_PROMPT verbatim (only the
// format placeholders differ).
const coordinatorPrompt = `You are a task coordinator for a multi-agent system. Your sole output must be a
valid JSON object matching the schema below. Do not output any prose, markdown
fences, or explanatory text outside the JSON object.

Rules for context_slice assignment:
1. Each task must have a non-overlapping context slice. Two tasks cannot share
   file paths, directories, or URL domains in their selectors.
2. Assign tasks based on context ownership — which agent "owns" that domain of
   knowledge or files — not by task type alone.
3. Do not create more than %d tasks. Fewer is usually better.
4. context_bus_writes must be minimal — only keys downstream tasks genuinely need.
5. task_id values must be lowercase alphanumeric with underscores/dashes only.

Goal: %s
Available profiles: %s
Swarm ID: %s

Output exactly one JSON object with this shape:
{
  "swarm_id": "%s",
  "goal": "<the goal>",
  "tasks": [
    {
      "task_id": "unique_snake_case_id",
      "description": "What this agent should do",
      "context_slice": {
        "type": "file_paths|directory|url_list|key_list|free_text",
        "selector": ["path/or/url", ...] or "free text description"
      },
      "profile": "profile_name_from_config",
      "depends_on": [],
      "context_bus_reads": [],
      "context_bus_writes": []
    }
  ],
  "failure_policy": "best_effort",
  "synthesis_profile": "profile_name_or_null"
}`

// Coordinator decomposes a goal into a task manifest.
//
// Python spawns `hermes chat -q <prompt> -Q` and scrapes stdout. Go runs the
// same prompt through the in-process agent loop with NO tools — a planner that
// can touch the filesystem is a needless blast radius — and scrapes the final
// text with the identical fence-strip + first-brace-to-last-brace extraction.
type Coordinator struct {
	Provider llm.Provider
	Model    string
	Profile  string
	Profiles []string
	Tracer   *trace.Recorder
	// ParentSpanID nests the coordinator turn under the swarm root span.
	ParentSpanID string
}

// ProduceManifest runs the coordinator and validates its output. Like Python it
// retries ONCE on unusable output before giving up.
func (c *Coordinator) ProduceManifest(ctx context.Context, goal, swarmID string, maxAgents int) (*Manifest, error) {
	prompt := fmt.Sprintf(coordinatorPrompt, maxAgents, goal,
		strings.Join(c.Profiles, ", "), swarmID, swarmID)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := c.invoke(ctx, prompt)
		if err != nil {
			lastErr = err
			continue
		}
		blob, ok := extractJSONObject(raw)
		if !ok {
			lastErr = ManifestError{"Coordinator produced no usable JSON output"}
			continue
		}
		m, err := ParseManifest([]byte(blob), maxAgents)
		if err != nil {
			lastErr = err
			continue
		}
		// Normalise exactly as Python does after validation.
		m.SwarmID = swarmID
		if m.FailurePolicy == "" {
			m.FailurePolicy = PolicyBestEffort
		}
		return m, nil
	}
	if lastErr == nil {
		lastErr = ManifestError{"Coordinator produced no usable JSON output after 2 attempts"}
	}
	return nil, lastErr
}

func (c *Coordinator) invoke(ctx context.Context, prompt string) (string, error) {
	if c.Provider == nil {
		return "", ManifestError{"no LLM provider configured for the coordinator"}
	}
	loop := &agent.Loop{Provider: c.Provider, Tracer: c.Tracer, ParentSpanID: c.ParentSpanID}
	res, err := loop.Run(ctx, prompt+"\n\nUser: Decompose the goal into a task manifest now.",
		agent.Options{Model: c.Model, MaxSteps: 1})
	if err != nil {
		return "", err
	}
	return res.FinalText, nil
}

// extractJSONObject strips a markdown fence and returns the widest {...} span,
// a direct port of the tail of Python's _invoke_coordinator.
func extractJSONObject(out string) (string, bool) {
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "```") {
		lines := strings.Split(out, "\n")
		if len(lines) >= 3 {
			out = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
		}
	}
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start != -1 && end != -1 && end > start {
		return out[start : end+1], true
	}
	return "", false
}

// FallbackManifest is the DEGRADED plan used when the coordinator cannot produce
// a manifest — most importantly under `--provider echo`, which has no model and
// simply echoes the prompt back.
//
// Python has no equivalent: it raises SwarmManifestError and exits 1, so
// `swarm run` is unusable without live credentials. That fails this project's
// offline-honesty bar. The Go path instead runs a single, real sub-agent on the
// whole goal and marks the run degraded everywhere it is reported (stderr note,
// `"degraded": true` in --json, and a "(degraded" suffix on the human status
// line). It is a genuinely reduced plan that is announced as one — not a fake
// success, and not a silent hang.
func FallbackManifest(goal, swarmID, profile, reason string) *Manifest {
	return &Manifest{
		SwarmID: swarmID,
		Goal:    goal,
		Tasks: []Task{{
			TaskID:       "solo",
			Description:  goal,
			ContextSlice: ContextSlice{Type: "free_text", Selector: goal},
			Profile:      profile,
		}},
		FailurePolicy:      PolicyBestEffort,
		CoordinatorProfile: profile,
	}
}
