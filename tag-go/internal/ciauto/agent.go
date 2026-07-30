package ciauto

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/paths"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/store"
	"github.com/tag-agent/tag/internal/tool"
	"github.com/tag-agent/tag/internal/trace"
)

// EchoNote is the honesty note attached to every result produced with the
// offline echo provider. Identical wording to internal/solver's echoNote so the
// two families read the same.
const EchoNote = "provider=echo is offline and deterministic: it echoes context rather than reasoning; " +
	"select --provider openai|anthropic (with credentials) for a real result."

// Runner drives the native agent loop for the agentic-ci subcommands. It owns
// the trace recorder for one command invocation, so every LLM turn issued by
// every subcommand lands in the `spans` table under a single trace id
// (mirroring internal/solver and internal/worker).
type Runner struct {
	Provider llm.Provider
	Model    string
	MaxSteps int

	// Guard is the consent gate for any tool the loop registers. A nil Guard is
	// NOT "ungated": tool.Register substitutes the secure default policy with no
	// prompter, which denies bash and write_file.
	Guard *permission.Guard
	// ToolRoot, when non-empty, registers the root-confined file tools (read_file/
	// write_file/list_dir) rooted there. bash is never registered by agentic-ci.
	ToolRoot string

	// TraceID is the id shared by every span this Runner emits.
	TraceID string
	rec     *trace.Recorder
	db      *store.DB
	calls   int
}

// NewRunner builds a Runner with a fresh trace id and recorder.
func NewRunner(db *store.DB, prov llm.Provider, model string) *Runner {
	id := uuid.NewString()[:16]
	return &Runner{
		Provider: prov,
		Model:    model,
		MaxSteps: 6,
		TraceID:  id,
		rec:      trace.NewRecorder(id, "default"),
		db:       db,
	}
}

// Offline reports whether the selected provider is the offline echo adapter.
func (r *Runner) Offline() bool { return r != nil && r.Provider != nil && r.Provider.Name() == "echo" }

// Calls returns the number of agent turns this Runner has driven.
func (r *Runner) Calls() int { return r.calls }

// Run drives one agent-loop pass and returns the final text.
func (r *Runner) Run(ctx context.Context, system, user string) (text string, steps int, stopped string, err error) {
	if r == nil || r.Provider == nil {
		return "", 0, "", fmt.Errorf("ciauto: nil provider")
	}
	if err := ctx.Err(); err != nil {
		return "", 0, "", err
	}
	l := &agent.Loop{Provider: r.Provider, Tracer: r.rec}
	if r.ToolRoot != "" {
		reg := agent.NewRegistry()
		topts := tool.DefaultOptions()
		topts.Root = r.ToolRoot
		topts.DisableBash = true // agentic-ci never grants shell access to the model
		topts.Guard = r.Guard
		tool.Register(reg, topts)
		l.Tools = reg
	}
	res, rerr := l.Run(ctx, user, agent.Options{Model: r.Model, System: system, MaxSteps: r.MaxSteps})
	if rerr != nil {
		return "", 0, "", rerr
	}
	r.calls++
	return res.FinalText, len(res.Steps), res.Stopped, nil
}

// Flush persists the recorded spans (best-effort: telemetry must never fail a
// command). Safe to call with a nil db.
func (r *Runner) Flush() {
	if r == nil || r.db == nil || r.rec == nil {
		return
	}
	_ = r.rec.Save(r.db.DB)
}

// DefaultWorkRootName is the directory under TAG_HOME holding per-invocation
// working directories. The #591 helper it used to carry its own copy of now
// lives in internal/paths, shared with worker, swarm, eval and loop.
const DefaultWorkRootName = paths.DefaultWorkRootName

// WorkDir creates a private working directory for one command invocation at
// <workRoot>/<id> and returns it with a cleanup func that removes the dir only
// if it was left empty. id is sanitised so it can never steer the path outside
// workRoot; a degenerate id falls back to "job".
func WorkDir(workRoot, id string) (string, func(), error) {
	return paths.WorkDir(workRoot, paths.SafeSegment(id, "job"))
}
