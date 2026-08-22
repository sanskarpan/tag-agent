package swarm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/paths"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/pricing"
	"github.com/tag-agent/tag/internal/tool"
	"github.com/tag-agent/tag/internal/trace"
)

// DefaultTimeoutPerAgent mirrors Python's --timeout-per-agent default.
const DefaultTimeoutPerAgent = 300 * time.Second

// abortPollInterval is how often a running swarm checks whether another process
// issued `tag swarm abort`.
const abortPollInterval = 500 * time.Millisecond

// TaskResult is the outcome of one sub-agent. Field names match the JSON keys
// Python emits from `vars(TaskResult)`.
type TaskResult struct {
	TaskID           string   `json:"task_id"`
	Status           string   `json:"status"`
	Output           string   `json:"output"`
	ErrorMessage     string   `json:"error_message"`
	TokensPrompt     int      `json:"tokens_prompt"`
	TokensCompletion int      `json:"tokens_completion"`
	CostUSD          float64  `json:"cost_usd"`
	Model            string   `json:"model"`
	Artifacts        []string `json:"artifacts"`
	// WorkDir is Go-only: the private directory this sub-agent's tools were
	// confined to, so an operator can find what it produced.
	WorkDir string `json:"work_dir,omitempty"`
}

// Result is the outcome of a whole swarm run.
type Result struct {
	SwarmID        string       `json:"swarm_id"`
	Status         string       `json:"status"`
	FinalOutput    string       `json:"final_output"`
	Results        []TaskResult `json:"results"`
	Degraded       bool         `json:"degraded"`
	DegradedReason string       `json:"degraded_reason,omitempty"`
}

// ApproveDecision is a --approve answer for one task.
type ApproveDecision int

const (
	// ApproveDispatch runs the task.
	ApproveDispatch ApproveDecision = iota
	// ApproveSkip marks the task skipped and continues.
	ApproveSkip
	// ApproveAbort stops the whole swarm (Python's default for any answer that
	// is neither "y" nor "skip").
	ApproveAbort
)

// Options configures a Runner.
type Options struct {
	// Provider is the LLM provider each sub-agent runs against. nil => offline echo.
	Provider llm.Provider
	// Model is the fallback model id.
	Model string
	// ModelForProfile resolves a per-task model from the task's profile.
	ModelForProfile func(profile string) string
	// System is an optional system prompt prepended to every sub-agent.
	System string
	// MaxSteps caps agent-loop turns per sub-agent (0 = loop default).
	MaxSteps int
	// WithTools enables the built-in tools for sub-agents.
	WithTools bool
	// Guard is the consent gate for those tools. Nil is NOT "ungated":
	// tool.Register substitutes permission's secure default policy, which is
	// headless and denies anything that would need a prompt.
	Guard *permission.Guard
	// WorkRoot is the parent under which each sub-agent gets its OWN directory
	// (<WorkRoot>/swarm/<swarm-id>/<task-id>). Empty => <TAG_HOME>/work. Swarm
	// agents are concurrent by design and therefore the single most likely thing
	// in the codebase to clobber each other's files — this is the #591 fix
	// applied to the swarm topology.
	WorkRoot string
	// MaxAgents bounds concurrency AND the wave size (clamped to MaxAgents const).
	MaxAgents int
	// FailurePolicy is one of the Policy* constants.
	FailurePolicy string
	// Sequential dispatches one task at a time (Python --sequential).
	Sequential bool
	// TimeoutPerAgent bounds a single sub-agent.
	TimeoutPerAgent time.Duration
	// Approve, when non-nil, is consulted before each task is dispatched.
	Approve func(Task) (ApproveDecision, error)
	// Tracer records spans for the run; sub-agent loops nest under the root span.
	Tracer *trace.Recorder
	// Out receives human-readable progress (nil = discard).
	Out io.Writer
	// Degraded/DegradedReason propagate a coordinator fallback into the Result.
	Degraded       bool
	DegradedReason string
}

// Runner executes a validated manifest.
type Runner struct {
	db   *sql.DB
	bus  *ContextBus
	opts Options
	m    *Manifest

	mu      sync.Mutex
	aborted bool
	reason  string
}

// NewRunner builds a Runner. It normalises the option defaults the same way
// Python's SwarmRunner.__init__ does (max_agents clamped to the hard cap).
func NewRunner(db *sql.DB, m *Manifest, bus *ContextBus, opts Options) *Runner {
	if opts.Provider == nil {
		opts.Provider = llm.EchoProvider{}
	}
	if opts.MaxAgents <= 0 {
		opts.MaxAgents = DefaultMaxAgents
	}
	if opts.MaxAgents > MaxAgents {
		opts.MaxAgents = MaxAgents
	}
	if opts.FailurePolicy == "" {
		opts.FailurePolicy = PolicyBestEffort
	}
	if opts.TimeoutPerAgent <= 0 {
		opts.TimeoutPerAgent = DefaultTimeoutPerAgent
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	return &Runner{db: db, m: m, bus: bus, opts: opts}
}

func (r *Runner) abort(reason string) {
	r.mu.Lock()
	if !r.aborted {
		r.aborted = true
		r.reason = reason
	}
	r.mu.Unlock()
}

func (r *Runner) isAborted() (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aborted, r.reason
}

// Run executes the manifest wave by wave and returns the run outcome.
//
// The scheduling is Python's: repeatedly take the set of tasks whose
// dependencies have all reached a terminal state, dispatch up to MaxAgents of
// them, fold the results in, repeat. Tasks whose dependencies can never be
// satisfied are stranded VISIBLY as 'skipped' rather than dropped, so the run
// cannot report 'completed' with tasks missing (Python B049).
func (r *Runner) Run(ctx context.Context, swarmID string) (*Result, error) {
	if err := markRunning(ctx, r.db, swarmID); err != nil {
		return nil, err
	}

	// Watch for an out-of-band `tag swarm abort` and for ctx cancellation
	// (SIGTERM). Both funnel into the same abort flag so the wave loop stops and
	// every in-flight task is finalised instead of being stranded in 'running'.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		t := time.NewTicker(abortPollInterval)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if runAborted(runCtx, r.db, swarmID) {
					r.abort("aborted by user")
					cancelRun()
					return
				}
			}
		}
	}()

	root := r.opts.Tracer.Start("swarm.run", trace.KindAgent, "", r.opts.Model)
	r.opts.Tracer.Attr(root, "tag.swarm_id", swarmID)
	r.opts.Tracer.Attr(root, "tag.max_agents", r.opts.MaxAgents)
	r.opts.Tracer.Attr(root, "tag.failure_policy", r.opts.FailurePolicy)
	r.opts.Tracer.Attr(root, "tag.task_count", len(r.m.Tasks))

	taskByID := map[string]Task{}
	depMap := map[string][]string{}
	remaining := map[string]bool{}
	for _, t := range r.m.Tasks {
		taskByID[t.TaskID] = t
		depMap[t.TaskID] = t.DependsOn
		remaining[t.TaskID] = true
	}
	completed := map[string]bool{}
	failed := map[string]bool{}
	var results []TaskResult

	for len(remaining) > 0 {
		if ab, _ := r.isAborted(); ab {
			break
		}
		if err := ctx.Err(); err != nil {
			r.abort("cancelled: " + err.Error())
			break
		}
		var ready []string
		for tid := range remaining {
			satisfied := true
			for _, d := range depMap[tid] {
				if !completed[d] && !failed[d] {
					satisfied = false
					break
				}
			}
			if satisfied {
				ready = append(ready, tid)
			}
		}
		// Deterministic wave ordering. Python takes `list(ready)[:max_agents]` off
		// a Python set, so WHICH tasks run first — and, when ready > max_agents,
		// which run at all — is unspecified. Sorting makes the wave reproducible.
		sort.Strings(ready)

		if len(ready) == 0 {
			for tid := range remaining {
				_ = setTaskStatus(r.db, swarmID, tid, "skipped", "dependencies unsatisfiable")
				results = append(results, TaskResult{TaskID: tid, Status: "skipped",
					ErrorMessage: "dependencies unsatisfiable"})
				failed[tid] = true
			}
			remaining = map[string]bool{}
			break
		}

		wave := ready
		if len(wave) > r.opts.MaxAgents {
			wave = wave[:r.opts.MaxAgents]
		}
		waveResults := r.runWave(runCtx, swarmID, wave, taskByID, root)
		for _, res := range waveResults {
			results = append(results, res)
			if res.Status == "done" || res.Status == "partial" {
				completed[res.TaskID] = true
			} else {
				failed[res.TaskID] = true
				if r.opts.FailurePolicy == PolicyAbortOnAny {
					r.abort(fmt.Sprintf("task %s failed under abort_on_any", res.TaskID))
				}
			}
		}
		// Retire ONLY the tasks that were actually dispatched.
		//
		// Python does `remaining -= ready`, which discards every ready task even
		// though only the first max_agents of them were dispatched: with 5 ready
		// tasks and --max-agents 2, three tasks silently vanish — never run, never
		// reported, left 'pending' in swarm_tasks while the run reports
		// 'completed'. That is a data-loss bug, not a behaviour to port.
		for _, tid := range wave {
			delete(remaining, tid)
		}
	}

	// A cancellation that landed *inside* the last wave (rather than between
	// waves) leaves the loop with nothing remaining, so the top-of-loop check
	// never fires. Without this the run would be scored on its cancelled tasks
	// and reported 'partial' — a cancelled swarm mis-labelled as a result.
	if err := ctx.Err(); err != nil {
		r.abort("cancelled: " + err.Error())
	}

	// Anything still un-dispatched because we aborted must not be left 'pending'
	// with no explanation.
	if ab, reason := r.isAborted(); ab {
		for tid := range remaining {
			_ = setTaskStatus(r.db, swarmID, tid, "skipped", reason)
			results = append(results, TaskResult{TaskID: tid, Status: "skipped", ErrorMessage: reason})
		}
	}

	cancelRun()
	<-pollDone

	totalPrompt, totalCompletion := 0, 0
	totalCost := 0.0
	var successful []TaskResult
	for _, res := range results {
		totalPrompt += res.TokensPrompt
		totalCompletion += res.TokensCompletion
		totalCost += res.CostUSD
		if res.Status == "done" || res.Status == "partial" {
			successful = append(successful, res)
		}
	}

	out := &Result{SwarmID: swarmID, Results: results,
		Degraded: r.opts.Degraded, DegradedReason: r.opts.DegradedReason}
	if out.Results == nil {
		out.Results = []TaskResult{}
	}

	if ab, reason := r.isAborted(); ab {
		status := "failed"
		// A user-issued abort (or a SIGTERM) is 'aborted'; a policy abort is a
		// failure. Python conflates both into 'failed'; keeping them apart is what
		// makes `swarm abort` observable in `swarm list`.
		if strings.HasPrefix(reason, "aborted") || strings.HasPrefix(reason, "cancelled") {
			status = "aborted"
		}
		out.Status = status
		r.opts.Tracer.Attr(root, "tag.status", status)
		r.opts.Tracer.End(root, trace.StatusError, reason, totalPrompt, totalCompletion)
		if err := finishRun(r.db, swarmID, status, "", totalPrompt, totalCompletion, totalCost); err != nil {
			return out, err
		}
		return out, nil
	}

	// require_majority: strictly MORE than half must have succeeded.
	if r.opts.FailurePolicy == PolicyRequireMajority && float64(len(successful)) <= float64(len(results))/2 {
		out.Status = "failed"
		r.opts.Tracer.Attr(root, "tag.status", "failed")
		r.opts.Tracer.End(root, trace.StatusError, "require_majority not met", totalPrompt, totalCompletion)
		if err := finishRun(r.db, swarmID, "failed", "", totalPrompt, totalCompletion, totalCost); err != nil {
			return out, err
		}
		return out, nil
	}

	final := r.synthesize(ctx, successful, root)
	status := "partial"
	if len(successful) == len(results) {
		status = "completed"
	}
	out.Status = status
	out.FinalOutput = final
	r.opts.Tracer.Attr(root, "tag.status", status)
	r.opts.Tracer.End(root, trace.StatusOK, "", totalPrompt, totalCompletion)
	if err := finishRun(r.db, swarmID, status, final, totalPrompt, totalCompletion, totalCost); err != nil {
		return out, err
	}
	// Persist a coordinator fallback so `swarm list/results --json` shows the run
	// was degraded, not a real decomposed swarm — the human status already
	// carries a "(degraded ...)" suffix, but machine consumers saw only
	// "completed".
	if out.Degraded {
		if err := markDegraded(r.db, swarmID, out.DegradedReason); err != nil {
			return out, err
		}
	}
	return out, nil
}

// runWave dispatches one wave, honouring --approve and --sequential.
func (r *Runner) runWave(ctx context.Context, swarmID string, wave []string, taskByID map[string]Task, root *trace.Span) []TaskResult {
	if r.opts.Approve != nil {
		var approved []string
		for _, tid := range wave {
			d, err := r.opts.Approve(taskByID[tid])
			if err != nil {
				r.abort("approval failed: " + err.Error())
				return nil
			}
			switch d {
			case ApproveDispatch:
				approved = append(approved, tid)
			case ApproveSkip:
				_ = setTaskStatus(r.db, swarmID, tid, "skipped", "skipped at approval prompt")
			default:
				r.abort("aborted at approval prompt")
				return nil
			}
		}
		wave = approved
	}
	if len(wave) == 0 {
		return nil
	}

	if r.opts.Sequential {
		out := make([]TaskResult, 0, len(wave))
		for _, tid := range wave {
			if ab, _ := r.isAborted(); ab {
				break
			}
			out = append(out, r.runTask(ctx, swarmID, taskByID[tid], root))
		}
		return out
	}

	// Bounded goroutine pool: the Go-native equivalent of Python's
	// ThreadPoolExecutor(max_workers=max_agents) over subprocesses. The wave is
	// already clamped to MaxAgents, so the semaphore is belt-and-braces — it keeps
	// the bound correct if the wave sizing ever changes.
	sem := make(chan struct{}, r.opts.MaxAgents)
	out := make([]TaskResult, len(wave))
	var wg sync.WaitGroup
	for i, tid := range wave {
		wg.Add(1)
		go func(i int, tid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = r.runTask(ctx, swarmID, taskByID[tid], root)
		}(i, tid)
	}
	wg.Wait()
	return out
}

// runTask executes one sub-agent through the native agent loop.
func (r *Runner) runTask(ctx context.Context, swarmID string, t Task, root *trace.Span) TaskResult {
	res := TaskResult{TaskID: t.TaskID, Artifacts: []string{}}

	profile := t.Profile
	if profile == "" {
		profile = r.m.CoordinatorProfile
	}
	model := r.opts.Model
	if r.opts.ModelForProfile != nil {
		if m := r.opts.ModelForProfile(profile); m != "" {
			model = m
		}
	}
	res.Model = model

	// An already-cancelled context must not produce a task stuck in 'running'.
	if err := ctx.Err(); err != nil {
		res.Status = "skipped"
		res.ErrorMessage = "cancelled before dispatch"
		_ = setTaskStatus(r.db, swarmID, t.TaskID, "skipped", res.ErrorMessage)
		return res
	}

	if err := setTaskRunning(ctx, r.db, swarmID, t.TaskID, profile, t.Description, t.ContextSlice); err != nil {
		res.Status = "failed"
		res.ErrorMessage = err.Error()
		_ = setTaskStatus(r.db, swarmID, t.TaskID, "failed", res.ErrorMessage)
		return res
	}

	snapshot := r.bus.ReadSnapshot(ctx, t.ContextBusReads)
	prompt := buildTaskPrompt(r.m.Goal, t, snapshot)

	// Per-agent timeout, from a context that is ALSO cancelled by abort/SIGTERM.
	taskCtx, cancel := context.WithTimeout(ctx, r.opts.TimeoutPerAgent)
	defer cancel()

	loop := &agent.Loop{Provider: r.opts.Provider, Tracer: r.opts.Tracer, ParentSpanID: spanID(root)}
	if r.opts.WithTools {
		dir, cleanup, derr := taskWorkDir(r.opts.WorkRoot, swarmID, t.TaskID)
		if derr != nil {
			res.Status = "failed"
			res.ErrorMessage = fmt.Sprintf("preparing work dir for task %s: %v", t.TaskID, derr)
			_ = completeTask(r.db, swarmID, res)
			return res
		}
		defer cleanup()
		res.WorkDir = dir
		reg := agent.NewRegistry()
		topts := tool.DefaultOptions()
		topts.Root = dir
		// Every tool a sub-agent can reach is routed through the consent gate. A
		// nil Guard here is NOT an open gate: tool.Register substitutes the secure
		// default policy, which is headless and denies anything needing a prompt.
		topts.Guard = r.opts.Guard
		tool.Register(reg, topts)
		loop.Tools = reg
	}

	out, err := loop.Run(taskCtx, prompt, agent.Options{
		Model: model, System: r.opts.System, MaxSteps: r.opts.MaxSteps,
	})
	switch {
	case err != nil && errors.Is(taskCtx.Err(), context.DeadlineExceeded):
		res.Status = "timed_out"
		res.ErrorMessage = fmt.Sprintf("Timeout after %s", r.opts.TimeoutPerAgent)
	case err != nil && ctx.Err() != nil:
		// Parent cancelled (SIGTERM or abort) — record it, do not leave 'running'.
		res.Status = "failed"
		res.ErrorMessage = "cancelled: " + ctx.Err().Error()
	case err != nil:
		res.Status = "failed"
		res.ErrorMessage = err.Error()
	default:
		res.Status = "done"
		res.Output = out.FinalText
		res.TokensPrompt = out.TotalUsage.PromptTokens
		res.TokensCompletion = out.TotalUsage.CompletionTokens
		if c, ok := pricing.CostUSD(model, res.TokensPrompt, res.TokensCompletion); ok {
			res.CostUSD = c
		}
		if res.WorkDir != "" {
			res.Artifacts = listArtifacts(res.WorkDir)
		}
	}

	// Publish to the context bus.
	//
	// Python's sub-agent writes ctx_out.json and the parent applies it. Go has no
	// such file: instead the task's final output is published under each key it
	// declared in context_bus_writes, still subject to the write-once and
	// permitted-key rules. Downstream tasks that declare the key in
	// context_bus_reads therefore see the upstream result — which is the entire
	// observable purpose of the bus.
	if res.Status == "done" {
		for _, k := range t.ContextBusWrites {
			r.bus.Write(ctx, k, res.Output, inferValueType(res.Output), t.TaskID, t.ContextBusWrites)
		}
	}

	_ = completeTask(r.db, swarmID, res)
	fmt.Fprintf(r.opts.Out, "  [%s] %s\n", res.Status, t.TaskID)
	return res
}

// buildTaskPrompt renders the sub-agent brief. It carries the same payload as
// Python's TAG_SWARM_TASK_INPUT json file, inlined into the prompt because Go's
// sub-agent is in-process and has nothing to read a file for.
func buildTaskPrompt(goal string, t Task, snapshot map[string]BusEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Swarm goal: %s\n\n", goal)
	fmt.Fprintf(&b, "Your task (%s): %s\n", t.TaskID, t.Description)
	fmt.Fprintf(&b, "Context slice (%s): %v\n", t.ContextSlice.Type, t.ContextSlice.Selector)
	if len(snapshot) > 0 {
		b.WriteString("\nContext bus (read-only):\n")
		for _, k := range sortedCopy(keysOf(snapshot)) {
			fmt.Fprintf(&b, "  %s = %v\n", k, snapshot[k].Value)
		}
	}
	b.WriteString("\nWork only within your context slice. Report your result.")
	return b.String()
}

func keysOf(m map[string]BusEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// synthesize combines the successful results (Python: SwarmRunner._synthesize).
func (r *Runner) synthesize(ctx context.Context, successful []TaskResult, root *trace.Span) string {
	if len(successful) == 0 {
		return ""
	}
	if len(successful) == 1 {
		return successful[0].Output
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n\nAgent results:\n\n", r.m.Goal)
	for _, s := range successful {
		fmt.Fprintf(&b, "--- %s ---\n%s\n\n", s.TaskID, s.Output)
	}
	b.WriteString("\nSynthesize the above results into a comprehensive final answer.")
	prompt := b.String()

	// Concatenation is the honest fallback (and is exactly what Python falls back
	// to when the synthesis subprocess fails or no synthesis profile is set).
	concat := make([]string, 0, len(successful))
	for _, s := range successful {
		concat = append(concat, s.Output)
	}
	fallback := strings.Join(concat, "\n\n---\n\n")

	// With no synthesis profile the answer is the concatenation, NOT the prompt.
	// Returning `prompt` handed the caller an instruction telling an LLM to write
	// the answer, presented as the answer, with status=completed and
	// degraded=false — fake success. (Python reaches the same branch but resolves
	// synthesis_profile or coordinator_profile first, so it rarely lands here.
	// Falling back to the concatenation instead of silently spending an extra
	// model call is the more honest of the two repairs.)
	if r.m.SynthesisProfile == "" {
		return fallback
	}
	// The synthesizer never gets tools: it summarises text.
	sctx, cancel := context.WithTimeout(ctx, r.opts.TimeoutPerAgent)
	defer cancel()
	loop := &agent.Loop{Provider: r.opts.Provider, Tracer: r.opts.Tracer, ParentSpanID: spanID(root)}
	model := r.opts.Model
	if r.opts.ModelForProfile != nil {
		if m := r.opts.ModelForProfile(r.m.SynthesisProfile); m != "" {
			model = m
		}
	}
	out, err := loop.Run(sctx, prompt, agent.Options{Model: model, MaxSteps: 1})
	if err != nil || strings.TrimSpace(out.FinalText) == "" {
		return fallback
	}
	return out.FinalText
}

func spanID(s *trace.Span) string {
	if s == nil {
		return ""
	}
	return s.ID
}

// taskWorkDir gives one sub-agent its own tool root: paths.WorkDir's #591
// contract (empty scratch dir, confined segments, removed on exit only if the
// agent left it empty) under a swarm-scoped layout,
// <WorkRoot>/swarm/<swarm-id>/<task-id>.
func taskWorkDir(workRoot, swarmID, taskID string) (string, func(), error) {
	return paths.WorkDir(workRoot,
		"swarm", paths.SafeSegment(swarmID, "swarm"), paths.SafeSegment(taskID, "task"))
}

// listArtifacts names the files a sub-agent left in its working directory.
func listArtifacts(dir string) []string {
	out := []string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}
