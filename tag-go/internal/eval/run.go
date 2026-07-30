package eval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/evaljudge"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/paths"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/pricing"
	"github.com/tag-agent/tag/internal/tool"
	"github.com/tag-agent/tag/internal/trace"
)

// DefaultCaseTimeout mirrors the 300s subprocess timeout Python's cmd_eval puts
// on each case.
const DefaultCaseTimeout = 300 * time.Second

// DefaultWorkRootName is the directory under TAG_HOME holding per-case working
// directories. Mirrors worker.DefaultWorkRootName; eval gets its own subtree so
// a case cannot collide with a queue job that happens to share an id.
const DefaultWorkRootName = "eval-work"

// OfflineNote is printed (and returned in --json) whenever the run used the
// offline echo provider. Echo streams the last user message straight back, so
// any expect_contains whose keyword also appears in the prompt passes for free.
const OfflineNote = "provider 'echo' is an offline stub: it echoes the case input back as the model output. " +
	"Scores from this run are NOT meaningful — keyword checks can pass purely because the keyword is in the prompt. " +
	"Use --provider anthropic|openai for a real evaluation."

// Options configures a run.
type Options struct {
	// Provider drives the cases. Nil defaults to the offline echo provider.
	Provider llm.Provider
	// Model is the fallback model id; ModelForProfile overrides it when set.
	Model           string
	ModelForProfile func(profile string) string
	Profile         string
	System          string
	MaxSteps        int

	// WithTools registers the built-in tools for each case.
	WithTools bool
	// Guard is the consent gate. It MUST be non-interactive: an eval run is
	// unattended, so an `ask` has to resolve to a deny instead of blocking on a
	// prompt (see cli.buildEvalGuard, which sets permFlags.noPrompt like
	// buildWorkerOptions does). A nil Guard is not "ungated" — tool.Register
	// substitutes the secure default policy, which fails closed on bash and
	// write_file.
	Guard *permission.Guard
	// WorkRoot is the parent of the per-case working directories. Empty defaults
	// to <TAG_HOME>/eval-work.
	WorkRoot string

	// Concurrency is how many cases run at once (default/min 1). Each case gets
	// its own working directory, so concurrent cases never share a cwd (#591).
	Concurrency int
	// CaseTimeout bounds a single case (default DefaultCaseTimeout).
	CaseTimeout time.Duration

	// Judge adds an LLM-as-judge check (internal/evaljudge) on top of the
	// deterministic expectations. A case passes only if BOTH agree.
	Judge          bool
	JudgeProvider  llm.Provider
	JudgeModel     string
	JudgeThreshold float64

	// OnCase, when set, is called as each case finishes (progress reporting).
	// It is invoked under a lock, so it may write to stdout safely.
	OnCase func(CaseResult)
}

// CaseResult is one graded case (also the eval_cases row).
type CaseResult struct {
	RunID            string   `json:"-"`
	CaseID           string   `json:"case_id"`
	Input            string   `json:"-"`
	Output           string   `json:"-"`
	Passed           bool     `json:"passed"`
	Score            float64  `json:"score"`
	FailureReason    string   `json:"failure_reason,omitempty"`
	Checks           int      `json:"checks"`
	Unchecked        bool     `json:"unchecked,omitempty"`
	Trivial          bool     `json:"trivial,omitempty"`
	JudgeScore       *float64 `json:"judge_score,omitempty"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model,omitempty"`
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	Errored          bool     `json:"errored,omitempty"`
}

// Summary is the outcome of a whole run. Field names `eval_run_id`/`total`/
// `passed`/`failed` match Python's finalize_eval_run payload, which is what
// `eval run --json` printed.
type Summary struct {
	EvalRunID  string       `json:"eval_run_id"`
	SuiteName  string       `json:"suite_name"`
	SuitePath  string       `json:"suite_path"`
	Profile    string       `json:"profile"`
	Provider   string       `json:"provider"`
	Status     string       `json:"status"`
	Total      int          `json:"total"`
	Passed     int          `json:"passed"`
	Failed     int          `json:"failed"`
	Unchecked  int          `json:"unchecked"`
	Trivial    int          `json:"trivial"`
	Errored    int          `json:"errored"`
	CostUSD    *float64     `json:"cost_usd"`
	Offline    bool         `json:"offline"`
	Meaningful bool         `json:"results_meaningful"`
	Notes      []string     `json:"notes"`
	Cases      []CaseResult `json:"cases"`
}

// Run executes every case in the suite, persists the results, and finalizes the
// run. It never returns with the run left in 'running': a cancelled context
// finalizes as 'cancelled' and an internal failure as 'failed'.
func Run(ctx context.Context, db *sql.DB, suite *Suite, suitePath string, opts Options) (*Summary, error) {
	if suite == nil || len(suite.Cases) == 0 {
		return nil, fmt.Errorf("suite has no cases")
	}
	if opts.Provider == nil {
		opts.Provider = llm.EchoProvider{}
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if opts.CaseTimeout <= 0 {
		opts.CaseTimeout = DefaultCaseTimeout
	}
	if opts.Judge && opts.JudgeProvider == nil {
		opts.JudgeProvider = opts.Provider
	}
	model := opts.Model
	if opts.ModelForProfile != nil {
		if m := opts.ModelForProfile(opts.Profile); m != "" {
			model = m
		}
	}

	suiteName := suite.Name
	if suiteName == "" {
		suiteName = strings.TrimSuffix(filepath.Base(suitePath), filepath.Ext(suitePath))
	}
	runID, err := CreateRun(db, suitePath, opts.Profile, suiteName, opts.Provider.Name())
	if err != nil {
		return nil, err
	}

	// The run id doubles as the trace id (as in internal/worker and
	// internal/solver), so `tag trace show <eval-run-id>` resolves the run's
	// spans and per-case cost. Telemetry is best-effort.
	rec := trace.NewRecorder(runID, opts.Profile)
	defer func() { _ = rec.Save(db) }()
	rootSpan := rec.Start("eval.run", trace.KindAgent, "", model)
	rec.Attr(rootSpan, "tag.eval.suite", suiteName)
	rec.Attr(rootSpan, "tag.eval.cases", len(suite.Cases))
	rec.Attr(rootSpan, "tag.provider", opts.Provider.Name())

	results := make([]CaseResult, len(suite.Cases))
	var mu sync.Mutex

	// finish records and reports one case. Held under mu so RecordCase and the
	// OnCase progress callback are serialized.
	finish := func(i int, r CaseResult) {
		mu.Lock()
		defer mu.Unlock()
		if err := RecordCase(db, r); err != nil {
			// Persisting the verdict is part of the contract for eval
			// list/show; surface it rather than reporting a pass we did not
			// store.
			r.Errored = true
			r.Passed = false
			r.FailureReason = joinReason(r.FailureReason, "recording result: "+err.Error())
		}
		results[i] = r
		if opts.OnCase != nil {
			opts.OnCase(r)
		}
	}

	if opts.Concurrency == 1 {
		// Serial is the default and must be deterministic: cases run and report
		// in suite order.
		for i, c := range suite.Cases {
			if ctx.Err() != nil {
				break
			}
			finish(i, runCase(ctx, db, rec, rootSpan.ID, runID, model, c, opts))
		}
	} else {
		var wg sync.WaitGroup
		sem := make(chan struct{}, opts.Concurrency)
		for i, c := range suite.Cases {
			// Once the context is cancelled (SIGTERM), stop dispatching. Cases
			// already in flight see the same cancellation through their own ctx.
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(i int, c Case) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if ctx.Err() != nil {
					return
				}
				finish(i, runCase(ctx, db, rec, rootSpan.ID, runID, model, c, opts))
			}(i, c)
		}
		wg.Wait()
	}

	// ctx.Err() latches, so this is true whether the interrupt arrived before
	// dispatch, mid-case, or after the last case.
	cancelled := ctx.Err() != nil

	sum := &Summary{
		EvalRunID: runID, SuiteName: suiteName, SuitePath: suitePath,
		Profile: opts.Profile, Provider: opts.Provider.Name(),
		Cases: []CaseResult{}, Notes: []string{},
	}
	var cost float64
	haveCost := false
	for _, r := range results {
		if r.CaseID == "" { // never ran (cancelled before dispatch)
			continue
		}
		sum.Cases = append(sum.Cases, r)
		if r.Unchecked {
			sum.Unchecked++
		}
		if r.Trivial {
			sum.Trivial++
		}
		if r.Errored {
			sum.Errored++
		}
		if r.CostUSD != nil {
			cost += *r.CostUSD
			haveCost = true
		}
	}
	if haveCost {
		sum.CostUSD = &cost
	}

	status := StatusCompleted
	if cancelled {
		status = StatusCancelled
	}
	total, passed, failed, ferr := FinalizeRun(db, runID, status)
	if ferr != nil {
		rec.End(rootSpan, trace.StatusError, ferr.Error(), 0, 0)
		return nil, ferr
	}
	sum.Status, sum.Total, sum.Passed, sum.Failed = status, total, passed, failed

	sum.Offline = opts.Provider.Name() == "echo"
	sum.Meaningful = !sum.Offline
	if sum.Offline {
		sum.Notes = append(sum.Notes, OfflineNote)
	}
	if sum.Trivial > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf(
			"%d case(s) passed only because the provider echoed the input back — the same checks pass against the prompt alone. These are NOT real passes.", sum.Trivial))
	}
	if sum.Unchecked > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf(
			"%d case(s) declare no expectations (no expect_contains/expect_regex/expected_output/length bounds) and therefore cannot fail.", sum.Unchecked))
	}
	if cancelled {
		sum.Notes = append(sum.Notes, fmt.Sprintf(
			"run was interrupted: %d of %d case(s) executed; the run is recorded as 'cancelled'.", len(sum.Cases), len(suite.Cases)))
	}
	rec.Attr(rootSpan, "tag.eval.passed", passed)
	rec.Attr(rootSpan, "tag.eval.failed", failed)
	rec.End(rootSpan, trace.StatusOK, "", 0, 0)
	return sum, nil
}

// runCase executes and grades one case. It never returns an error: a case that
// blows up is a FAILING case with the error as its reason, which is what an
// eval harness must report (a crashed case silently counted as a pass is the
// exact failure mode this port exists to avoid).
func runCase(ctx context.Context, db *sql.DB, rec *trace.Recorder, parentSpan, runID, model string, c Case, opts Options) CaseResult {
	r := CaseResult{
		RunID: runID, CaseID: c.ID, Input: c.Input,
		Provider: opts.Provider.Name(), Model: model,
	}
	span := rec.Start("eval.case", trace.KindAgent, parentSpan, model)
	rec.Attr(span, "tag.eval.case_id", c.ID)

	cctx, cancel := context.WithTimeout(ctx, opts.CaseTimeout)
	defer cancel()

	loop := &agent.Loop{Provider: opts.Provider, Tracer: rec, ParentSpanID: span.ID}
	if opts.WithTools {
		reg := agent.NewRegistry()
		topts := tool.DefaultOptions()
		topts.Guard = opts.Guard
		// #591: each case gets its OWN root. Concurrent cases that write the same
		// filename must not clobber one another, and a serial run must not leak
		// artifacts from case N into case N+1's expectations.
		dir, cleanup, derr := caseWorkDir(opts.WorkRoot, runID, c.ID)
		if derr != nil {
			rec.End(span, trace.StatusError, derr.Error(), 0, 0)
			r.Passed, r.Errored = false, true
			r.FailureReason = "preparing work dir: " + derr.Error()
			return r
		}
		defer cleanup()
		topts.Root = dir
		tool.Register(reg, topts)
		loop.Tools = reg
	}

	res, err := loop.Run(cctx, c.Input, agent.Options{Model: model, System: opts.System, MaxSteps: opts.MaxSteps})
	if err != nil {
		reason := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			reason = fmt.Sprintf("case timed out after %s", opts.CaseTimeout)
		} else if errors.Is(err, context.Canceled) {
			reason = "case cancelled (interrupted)"
		}
		rec.End(span, trace.StatusError, reason, 0, 0)
		r.Passed, r.Errored, r.FailureReason = false, true, reason
		return r
	}
	r.Output = res.FinalText
	r.PromptTokens = res.TotalUsage.PromptTokens
	r.CompletionTokens = res.TotalUsage.CompletionTokens
	// Cost comes from internal/pricing (the same table `tag pricing`/`tag costs`
	// read), never a locally re-derived rate. An unknown model leaves it nil
	// rather than a misleading $0. The echo provider spends nothing, so pricing
	// its synthetic token counts against the profile's real model id would
	// invent a charge that was never incurred.
	if opts.Provider.Name() != "echo" {
		if cst, ok := pricing.CostUSD(model, r.PromptTokens, r.CompletionTokens); ok {
			r.CostUSD = &cst
		}
	}

	sc := ScoreCase(c, r.Output)
	r.Passed, r.Score, r.FailureReason, r.Checks, r.Unchecked = sc.Passed, sc.Score, sc.Reason, sc.Checks, sc.Unchecked

	// Trivial-pass detection: with a provider that echoes its prompt, a keyword
	// check passes whenever the keyword is in the input. Re-score the case
	// against the INPUT alone; if that also passes, the "pass" carries no signal.
	if sc.Passed && sc.Checks > 0 && ScoreCase(c, c.Input).Passed {
		r.Trivial = true
	}

	if opts.Judge {
		ref := c.Reference
		if ref == "" && c.ExpectedOutput != nil {
			ref = *c.ExpectedOutput
		}
		j, jerr := evaljudge.Judge(cctx, db, opts.JudgeProvider, opts.JudgeModel, c.Input, r.Output, ref, opts.JudgeThreshold)
		if jerr != nil {
			r.Passed, r.Errored = false, true
			r.FailureReason = joinReason(r.FailureReason, "judge failed: "+jerr.Error())
		} else {
			r.JudgeScore = &j.Score
			// A case passes only if the deterministic checks AND the judge agree.
			if !j.Passed {
				r.Passed = false
				why := j.Reasoning
				if why == "" {
					why = "no reasoning returned"
				}
				r.FailureReason = joinReason(r.FailureReason,
					fmt.Sprintf("judge scored %.2f < %.2f (%s)", j.Score, j.Threshold, truncate(why, 120)))
			}
			// The judge is a second opinion, not a second scale: average it in so
			// `score` still reflects everything that was checked.
			if sc.Checks > 0 {
				r.Score = (r.Score + j.Score) / 2
			} else {
				r.Score = j.Score
				r.Unchecked = false
			}
		}
	}

	// "Trivial" qualifies a PASS ("this pass means nothing"). On a case that
	// ended up failing — e.g. the judge overruled the keyword checks — the label
	// would only confuse the report.
	if !r.Passed {
		r.Trivial = false
	}

	st := trace.StatusOK
	if !r.Passed {
		st = trace.StatusError
	}
	rec.Attr(span, "tag.eval.passed", r.Passed)
	rec.Attr(span, "tag.eval.score", r.Score)
	rec.End(span, st, r.FailureReason, 0, 0)
	return r
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// caseWorkDir creates the private working directory for one case, mirroring
// worker.jobWorkDir (which is unexported): an EMPTY scratch dir per case at
// <WorkRoot>/<run-id>/<case-id>, stable and derivable so artifacts are findable
// afterwards, removed on exit only if the case left it empty.
func caseWorkDir(workRoot, runID, caseID string) (string, func(), error) {
	if workRoot == "" {
		workRoot = filepath.Join(paths.Home(), DefaultWorkRootName)
	}
	dir := filepath.Join(workRoot, safeSegment(runID, "run"), safeSegment(caseID, "case"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, err
	}
	return dir, func() {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
			_ = os.Remove(dir)
			_ = os.Remove(filepath.Dir(dir)) // removes the run dir only when empty
		}
	}, nil
}

// safeSegment reduces an id to a single path element so a case id from a suite
// file can never steer the working directory outside WorkRoot.
func safeSegment(s, fallback string) string {
	s = strings.ReplaceAll(s, string(os.PathSeparator), "_")
	s = strings.ReplaceAll(s, "/", "_")
	seg := filepath.Base(filepath.Clean("/" + s))
	if seg == "." || seg == string(os.PathSeparator) || seg == "" {
		return fallback
	}
	return seg
}
