// Package solver implements the agentic-solver family (parity roadmap #527):
// swe-solve, issue-solve, agentic-ci and review-pr. Each command gathers a
// specific kind of context (a repo working directory, an issue body, a task
// description or a unified diff), drives the native agent loop (internal/agent)
// with an appropriate system prompt, records a run, and returns a structured
// result.
//
// The default provider is the offline, deterministic "echo" adapter, so the
// whole package is exercisable without API keys or network access. When a
// live provider is selected AND tools are enabled, swe-solve confines the
// built-in file tools (internal/tool) to --repo so the agent actually reads
// and edits files; --run-tests runs a command afterwards and reports pass/fail;
// agentic-ci runs a real check→fix→re-check loop. Where a step genuinely
// cannot run offline (e.g. fetching a live issue/PR when the gh CLI is absent),
// the solver stays HONEST: it records a note describing what was skipped rather
// than pretending the step succeeded.
package solver

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/store"
	"github.com/tag-agent/tag/internal/tool"
)

// Kind identifies which agentic-solver command produced a result.
type Kind string

const (
	KindSWE    Kind = "swe-solve"
	KindIssue  Kind = "issue-solve"
	KindCI     Kind = "agentic-ci"
	KindReview Kind = "review-pr"
)

// Options configures a Solve call. Only the fields relevant to Kind are read.
type Options struct {
	Kind Kind
	// Task carries the primary context text: the SWE task, the issue body, the
	// CI task description, or the unified diff (for review-pr).
	Task string
	// RepoPath is the working directory for swe-solve (optional).
	RepoPath string
	// MaxSteps caps the agent loop steps per pass (default 8).
	MaxSteps int
	// MaxIters is the number of check→fix passes for agentic-ci (default 1).
	MaxIters int

	// EnableTools registers the root-confined built-in file tools (read_file,
	// write_file, list_dir) so the agent can actually read and edit files under
	// RepoPath. Off by default: the offline echo provider never emits tool calls,
	// so this is opt-in depth for real (--provider) or scripted solves.
	EnableTools bool
	// EnableBash additionally registers the bash tool (unrestricted host exec,
	// working dir = RepoPath). Opt-in on top of EnableTools.
	EnableBash bool
	// RunTests, when set, is a shell command run after the loop (working dir =
	// RepoPath) whose pass/fail is reported in Result.TestResult.
	RunTests string
	// CheckCmd is the agentic-ci check command (build/test). When set, Solve runs
	// it, and on failure feeds the output to the loop for a fix, re-checking up to
	// MaxIters times. Reported in Result.Iterations / Converged.
	CheckCmd string
	// CmdTimeout bounds RunTests / CheckCmd execution (default 2m).
	CmdTimeout time.Duration
}

// CmdOutcome is the result of running a shell command (tests or a CI check).
type CmdOutcome struct {
	Command string `json:"command"`
	Passed  bool   `json:"passed"`
	Output  string `json:"output"`
}

// Iteration records one agentic-ci check→fix pass.
type Iteration struct {
	Iteration int    `json:"iteration"`
	Passed    bool   `json:"passed"`
	Output    string `json:"output"`
	Fix       string `json:"fix,omitempty"`
}

// Result is the structured outcome of a Solve call.
type Result struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Provider string   `json:"provider"`
	Summary  string   `json:"summary"`
	Output   string   `json:"output"`
	Steps    int      `json:"steps"`
	Stopped  string   `json:"stopped"`
	Notes    []string `json:"notes,omitempty"`

	// TestResult is set when RunTests ran (swe-solve --run-tests).
	TestResult *CmdOutcome `json:"test_result,omitempty"`
	// Iterations / Converged describe an agentic-ci check→fix loop.
	Iterations []Iteration `json:"iterations,omitempty"`
	Converged  bool        `json:"converged,omitempty"`
}

// Solve gathers context for opts.Kind, drives the native agent loop through the
// given provider, records the run (best-effort; skipped when db is nil), and
// returns a structured Result. It performs NO live API calls unless the caller
// passes a live provider. External-system access (running the repo's tests, a CI
// check) happens only when the caller explicitly opts in via RunTests/CheckCmd.
func Solve(ctx context.Context, db *store.DB, prov llm.Provider, model string, opts Options) (*Result, error) {
	if prov == nil {
		return nil, fmt.Errorf("solver: nil provider")
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 8
	}
	if opts.MaxIters <= 0 {
		opts.MaxIters = 1
	}
	if opts.CmdTimeout <= 0 {
		opts.CmdTimeout = 2 * time.Minute
	}

	system, userMsg, notes, err := buildContext(opts)
	if err != nil {
		return nil, err
	}

	loop := &agent.Loop{Provider: prov}
	// Enable the root-confined file tools for swe-solve when requested. The echo
	// provider never requests tools, so this is inert offline; a real/scripted
	// provider can now read_file/write_file/list_dir under RepoPath.
	if opts.EnableTools && opts.Kind == KindSWE {
		reg := agent.NewRegistry()
		topts := tool.DefaultOptions()
		topts.Root = opts.RepoPath
		topts.DisableBash = !opts.EnableBash
		tool.Register(reg, topts)
		loop.Tools = reg
		if opts.EnableBash {
			notes = append(notes, "bash tool enabled: it runs UNRESTRICTED host commands (not confined to --repo); the working dir is --repo only.")
		}
	}

	started := time.Now().UTC()
	id := uuid.NewString()[:16]

	// agentic-ci with a real --check command runs its own check→fix loop.
	if opts.Kind == KindCI && strings.TrimSpace(opts.CheckCmd) != "" {
		res, cerr := runCILoop(ctx, loop, model, system, opts)
		if cerr != nil {
			return nil, cerr
		}
		res.ID = id
		res.Kind = string(opts.Kind)
		res.Provider = prov.Name()
		res.Notes = append(notes, res.Notes...)
		if prov.Name() == "echo" {
			res.Notes = append(res.Notes, echoNote)
		}
		if db != nil {
			if rerr := recordRun(db, id, string(opts.Kind), model, userMsg, started); rerr != nil {
				return nil, fmt.Errorf("recording run: %w", rerr)
			}
		}
		return res, nil
	}

	var final string
	var steps int
	stopped := "done"
	passes := 1
	if opts.Kind == KindCI {
		passes = opts.MaxIters
	}
	for i := 0; i < passes; i++ {
		res, rerr := loop.Run(ctx, userMsg, agent.Options{Model: model, System: system, MaxSteps: opts.MaxSteps})
		if rerr != nil {
			return nil, rerr
		}
		final = res.FinalText
		steps += len(res.Steps)
		stopped = res.Stopped
	}

	summary := summarize(opts.Kind, final)
	if prov.Name() == "echo" {
		notes = append(notes, echoNote)
	}

	result := &Result{
		ID:       id,
		Kind:     string(opts.Kind),
		Provider: prov.Name(),
		Summary:  summary,
		Output:   final,
		Steps:    steps,
		Stopped:  stopped,
		Notes:    notes,
	}

	// swe-solve --run-tests: run the test command after the loop and report.
	if opts.Kind == KindSWE && strings.TrimSpace(opts.RunTests) != "" {
		outcome := runCmd(ctx, opts.RepoPath, opts.RunTests, opts.CmdTimeout)
		result.TestResult = &outcome
	}

	if db != nil {
		if rerr := recordRun(db, id, string(opts.Kind), model, userMsg, started); rerr != nil {
			return nil, fmt.Errorf("recording run: %w", rerr)
		}
	}

	return result, nil
}

const echoNote = "provider=echo is offline and deterministic: it echoes context rather than reasoning; select --provider openai|anthropic (with credentials) for a real solve."

// runCILoop runs a real check→fix→re-check loop: run CheckCmd; if it fails, feed
// its output to the agent loop for a fix suggestion (surfaced in each Iteration's
// Fix; agentic-ci does not register file tools, so it never edits files itself),
// then re-run CheckCmd. Repeats up to MaxIters times. Converged is true once the
// check passes.
func runCILoop(ctx context.Context, loop *agent.Loop, model, system string, opts Options) (*Result, error) {
	res := &Result{Stopped: "done"}
	var lastFix string
	for i := 0; i < opts.MaxIters; i++ {
		outcome := runCmd(ctx, opts.RepoPath, opts.CheckCmd, opts.CmdTimeout)
		it := Iteration{Iteration: i + 1, Passed: outcome.Passed, Output: outcome.Output}
		if outcome.Passed {
			res.Iterations = append(res.Iterations, it)
			res.Converged = true
			res.Output = fmt.Sprintf("check %q passed on iteration %d", opts.CheckCmd, i+1)
			res.Summary = summarize(KindCI, res.Output)
			return res, nil
		}
		// Check failed: ask the loop for a fix, feeding it the failure output.
		userMsg := "# CI task\n" + strings.TrimSpace(opts.Task) +
			"\n\n# Failing check\n$ " + opts.CheckCmd +
			"\n\n# Output\n" + truncate(outcome.Output, 8000)
		lres, rerr := loop.Run(ctx, userMsg, agent.Options{Model: model, System: system, MaxSteps: opts.MaxSteps})
		if rerr != nil {
			return nil, rerr
		}
		lastFix = lres.FinalText
		res.Steps += len(lres.Steps)
		res.Stopped = lres.Stopped
		it.Fix = lastFix
		res.Iterations = append(res.Iterations, it)
	}
	res.Converged = false
	res.Output = fmt.Sprintf("check %q still failing after %d iteration(s)", opts.CheckCmd, opts.MaxIters)
	res.Summary = summarize(KindCI, res.Output)
	res.Notes = append(res.Notes, fmt.Sprintf("agentic-ci did not converge in %d iteration(s); the last fix suggestion was recorded but the check still fails.", opts.MaxIters))
	return res, nil
}

// runCmd runs a shell command (dir = repo, or cwd when empty) and reports pass
// (exit 0) or fail with combined output.
func runCmd(ctx context.Context, dir, command string, timeout time.Duration) CmdOutcome {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", command)
	if dir != "" {
		c.Dir = dir
	}
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	out := buf.String()
	if cctx.Err() == context.DeadlineExceeded {
		return CmdOutcome{Command: command, Passed: false, Output: out + "\n(timed out after " + timeout.String() + ")"}
	}
	return CmdOutcome{Command: command, Passed: err == nil, Output: out}
}

// buildContext assembles the system prompt, the user message and any honesty
// notes for the given options.
func buildContext(opts Options) (system, userMsg string, notes []string, err error) {
	task := strings.TrimSpace(opts.Task)
	if task == "" {
		return "", "", nil, fmt.Errorf("solver: empty task/context for %s", opts.Kind)
	}
	switch opts.Kind {
	case KindSWE:
		repoInfo := ""
		if opts.RepoPath != "" {
			listing, lerr := listRepo(opts.RepoPath)
			if lerr != nil {
				return "", "", nil, lerr
			}
			repoInfo = "\n\n# Repository contents (" + opts.RepoPath + ")\n" + listing
			if opts.EnableTools {
				repoInfo += "\n\nUse the read_file/write_file/list_dir tools (confined to the repo) to inspect and edit files."
			} else {
				notes = append(notes, "swe-solve gathered a shallow repo listing only; pass --tools (with a real --provider) to let the agent actually read and edit files.")
			}
		} else {
			notes = append(notes, "no --repo given: swe-solve reasoned about the task text alone (no repo context gathered).")
		}
		system = "You are a software engineering agent. Solve the task by proposing concrete file changes, " +
			"then summarizing the diff and how to verify it." + repoInfo
		userMsg = "# Task\n" + task
		return system, userMsg, notes, nil

	case KindIssue:
		if looksLikeIssueRef(task) {
			notes = append(notes, "input still looks like an issue reference ("+truncate(task, 40)+"): fetching it via the gh CLI was not done here. Pass the issue body inline or via --file, or ensure gh is installed and authenticated.")
		}
		system = "You are solving a software issue. Analyze it, then provide: (1) a clear fix plan, " +
			"(2) the specific files and changes needed, (3) edge cases to consider."
		userMsg = "# Issue\n" + task
		return system, userMsg, notes, nil

	case KindCI:
		system = "You are a CI automation agent. Diagnose the task/failure, propose a fix, and describe how to verify it in CI."
		userMsg = "# CI task\n" + task
		return system, userMsg, notes, nil

	case KindReview:
		system = "You are a meticulous code reviewer. Review the unified diff for correctness bugs, " +
			"security issues, and style. Report findings grouped by severity, with file:line references."
		userMsg = "# Diff under review\n" + task
		return system, userMsg, notes, nil

	default:
		return "", "", nil, fmt.Errorf("solver: unknown kind %q", opts.Kind)
	}
}

// listRepo returns a sorted, shallow listing of the repo's top-level entries.
func listRepo(dir string) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("repo path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repo path %q is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(empty directory)", nil
	}
	return strings.Join(names, "\n"), nil
}

// looksLikeIssueRef reports whether s is plausibly a bare issue reference (a URL
// or a "#123" / "owner/repo#123" token) rather than an actual issue body.
func looksLikeIssueRef(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "\n") || len(s) > 80 {
		return false
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return true
	}
	if strings.HasPrefix(s, "#") && !strings.Contains(s, " ") {
		return true
	}
	if strings.Contains(s, "#") && !strings.Contains(s, " ") {
		return true
	}
	return false
}

// summarize builds a one-line summary from the agent's final text.
func summarize(kind Kind, final string) string {
	head := strings.TrimSpace(final)
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	head = truncate(head, 120)
	if head == "" {
		head = "(no output)"
	}
	return fmt.Sprintf("%s: %s", kind, head)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// recordRun persists a completed solver run to the runs table (best-effort;
// mirrors internal/cli/run.go's recording convention).
func recordRun(db *store.DB, id, kind, model, prompt string, started time.Time) error {
	_, err := db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status,
		model_id,prompt_tokens,completion_tokens,cache_read_tokens,duration_ms,completed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, started.Format(time.RFC3339), kind, "solve", "native", "default", "default",
		prompt, "{}", "completed", model, 0, 0, 0, time.Since(started).Milliseconds(),
		time.Now().UTC().Format(time.RFC3339))
	return err
}
