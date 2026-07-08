// Package solver implements the agentic-solver family (parity roadmap #527):
// swe-solve, issue-solve, agentic-ci and review-pr. Each command gathers a
// specific kind of context (a repo working directory, an issue body, a task
// description or a unified diff), drives the native agent loop (internal/agent)
// with an appropriate system prompt, records a run, and returns a structured
// result.
//
// The default provider is the offline, deterministic "echo" adapter, so the
// whole package is exercisable without API keys or network access. Where a
// faithful implementation would require a live model or an external system
// (applying code edits, fetching a GitHub issue/PR, posting review comments),
// the solver is HONEST: it records a note describing what was skipped rather
// than pretending the step succeeded.
package solver

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/store"
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
	// MaxIters is the number of agent-loop passes for agentic-ci (default 1).
	MaxIters int
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
}

// Solve gathers context for opts.Kind, drives the native agent loop through the
// given provider, records the run (best-effort; skipped when db is nil), and
// returns a structured Result. It performs NO live API calls unless the caller
// passes a live provider, and NO external-system access (git, gh, network).
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

	system, userMsg, notes, err := buildContext(opts)
	if err != nil {
		return nil, err
	}

	loop := &agent.Loop{Provider: prov}
	started := time.Now().UTC()

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

	id := uuid.NewString()[:16]
	summary := summarize(opts.Kind, final)
	if prov.Name() == "echo" {
		notes = append(notes, "provider=echo is offline and deterministic: it echoes context rather than reasoning; select --provider openai|anthropic (with credentials) for a real solve.")
	}

	if db != nil {
		if rerr := recordRun(db, id, string(opts.Kind), model, userMsg, started); rerr != nil {
			return nil, fmt.Errorf("recording run: %w", rerr)
		}
	}

	return &Result{
		ID:       id,
		Kind:     string(opts.Kind),
		Provider: prov.Name(),
		Summary:  summary,
		Output:   final,
		Steps:    steps,
		Stopped:  stopped,
		Notes:    notes,
	}, nil
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
			notes = append(notes, "swe-solve gathered a shallow repo listing only; actually reading/editing files and running tests requires the tool-enabled loop with a live model.")
		} else {
			notes = append(notes, "no --repo given: swe-solve reasoned about the task text alone (no repo context gathered).")
		}
		system = "You are a software engineering agent. Solve the task by proposing concrete file changes, " +
			"then summarizing the diff and how to verify it." + repoInfo
		userMsg = "# Task\n" + task
		return system, userMsg, notes, nil

	case KindIssue:
		if looksLikeIssueRef(task) {
			notes = append(notes, "input looks like an issue reference ("+truncate(task, 40)+"); fetching it from GitHub/Linear needs network + a token. Pass the issue body inline or via --file for an offline solve.")
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
		notes = append(notes, "review-pr reviewed the supplied diff only; fetching a live PR diff/metadata and posting comments needs the gh CLI + network and was not attempted.")
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
