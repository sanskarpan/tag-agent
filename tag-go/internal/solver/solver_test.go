package solver

import (
	"context"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/llm"
)

// echo is the offline provider; Solve must run fully without a DB or network.
func TestSolve_SWE_Offline(t *testing.T) {
	res, err := Solve(context.Background(), nil, llm.EchoProvider{}, "", Options{
		Kind: KindSWE,
		Task: "fix the off-by-one in pagination",
	})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if res.Kind != string(KindSWE) {
		t.Errorf("kind = %q", res.Kind)
	}
	if res.Provider != "echo" {
		t.Errorf("provider = %q", res.Provider)
	}
	if res.ID == "" {
		t.Error("empty id")
	}
	if res.Steps < 1 {
		t.Errorf("steps = %d, want >=1", res.Steps)
	}
	// echo streams back the user message, which contains the task text.
	if !strings.Contains(res.Output, "off-by-one") {
		t.Errorf("output missing task echo: %q", res.Output)
	}
	if len(res.Notes) == 0 {
		t.Error("expected honesty notes (offline echo + no repo)")
	}
}

func TestSolve_SWE_WithRepo(t *testing.T) {
	dir := t.TempDir()
	res, err := Solve(context.Background(), nil, llm.EchoProvider{}, "", Options{
		Kind:     KindSWE,
		Task:     "add a healthcheck endpoint",
		RepoPath: dir,
	})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	// The repo listing note must be present when --repo is supplied.
	if !hasNote(res.Notes, "shallow repo listing") {
		t.Errorf("missing repo-listing note: %v", res.Notes)
	}
}

func TestSolve_SWE_BadRepo(t *testing.T) {
	_, err := Solve(context.Background(), nil, llm.EchoProvider{}, "", Options{
		Kind:     KindSWE,
		Task:     "x",
		RepoPath: "/no/such/path/definitely/missing",
	})
	if err == nil {
		t.Fatal("expected error for missing repo path")
	}
}

func TestSolve_Issue_RefNote(t *testing.T) {
	res, err := Solve(context.Background(), nil, llm.EchoProvider{}, "", Options{
		Kind: KindIssue,
		Task: "https://github.com/acme/repo/issues/42",
	})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if !hasNote(res.Notes, "issue reference") {
		t.Errorf("expected issue-reference honesty note, got %v", res.Notes)
	}
}

func TestSolve_Issue_InlineBody(t *testing.T) {
	body := "Users report the export button does nothing on Safari.\nSteps: click export, nothing downloads."
	res, err := Solve(context.Background(), nil, llm.EchoProvider{}, "", Options{
		Kind: KindIssue,
		Task: body,
	})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if hasNote(res.Notes, "issue reference") {
		t.Errorf("inline body should not trigger issue-reference note: %v", res.Notes)
	}
}

func TestSolve_CI_Iterations(t *testing.T) {
	res, err := Solve(context.Background(), nil, llm.EchoProvider{}, "", Options{
		Kind:     KindCI,
		Task:     "the lint job fails on unused imports",
		MaxIters: 3,
	})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	// Each pass runs the loop at least once; 3 iterations => >=3 steps.
	if res.Steps < 3 {
		t.Errorf("steps = %d, want >=3 for 3 iterations", res.Steps)
	}
}

func TestSolve_Review(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n+func Add(a,b int) int { return a-b }"
	res, err := Solve(context.Background(), nil, llm.EchoProvider{}, "", Options{
		Kind: KindReview,
		Task: diff,
	})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if !hasNote(res.Notes, "review-pr reviewed the supplied diff only") {
		t.Errorf("expected review honesty note: %v", res.Notes)
	}
	if !strings.HasPrefix(res.Summary, string(KindReview)+":") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestSolve_EmptyTask(t *testing.T) {
	_, err := Solve(context.Background(), nil, llm.EchoProvider{}, "", Options{Kind: KindSWE, Task: "  "})
	if err == nil {
		t.Fatal("expected error for empty task")
	}
}

func TestSolve_NilProvider(t *testing.T) {
	_, err := Solve(context.Background(), nil, nil, "", Options{Kind: KindSWE, Task: "x"})
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestLooksLikeIssueRef(t *testing.T) {
	cases := map[string]bool{
		"#123":                                true,
		"acme/repo#7":                         true,
		"https://github.com/a/b/issues/1":     true,
		"Export button broken on Safari":      false,
		"line one\nline two":                  false,
		strings.Repeat("very long text ", 10): false,
	}
	for in, want := range cases {
		if got := looksLikeIssueRef(in); got != want {
			t.Errorf("looksLikeIssueRef(%q) = %v, want %v", in, got, want)
		}
	}
}

func hasNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
