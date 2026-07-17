package ciauto

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScaffoldAllTypesContainWorkflowKeys(t *testing.T) {
	keys := []string{"on:", "jobs:", "runs-on:", "pull_request:", "steps:"}
	for _, wf := range WorkflowTypes {
		yaml := ScaffoldGitHubAction(wf)
		for _, k := range keys {
			if !strings.Contains(yaml, k) {
				t.Errorf("type %q: scaffold missing %q\n%s", wf, k, yaml)
			}
		}
		if !strings.Contains(yaml, "tag-"+wf+":") {
			t.Errorf("type %q: missing job id tag-%s:", wf, wf)
		}
	}
}

func TestScaffoldRunCommands(t *testing.T) {
	cases := map[string]string{
		"eval":     "tag eval-ci run tests/eval_suite.yaml --threshold 0.85",
		"review":   "tag review-pr --repo ${{ github.repository }}",
		"test-gen": "tag agentic-ci test-gen --diff diff.patch --profile coder",
		"fix-vuln": "tag agentic-ci fix-vuln results.sarif --profile reviewer",
	}
	for wf, want := range cases {
		got := ScaffoldGitHubAction(wf)
		if !strings.Contains(got, want) {
			t.Errorf("type %q: expected run cmd %q in\n%s", wf, want, got)
		}
	}
}

func TestScaffoldDefaultsToEval(t *testing.T) {
	if ScaffoldGitHubAction("") != ScaffoldGitHubAction("eval") {
		t.Error("empty type should default to eval")
	}
	if !strings.Contains(ScaffoldGitHubAction("bogus"), "eval_suite.yaml") {
		t.Error("unknown type should fall back to eval run command")
	}
}

func TestScaffoldTitle(t *testing.T) {
	got := ScaffoldGitHubAction("test-gen")
	if !strings.Contains(got, "name: TAG Test Gen") {
		t.Errorf("expected titleized name, got:\n%s", got)
	}
}

func TestLoadSuite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "suite.yaml")
	content := "name: my-suite\ncases:\n  - id: c1\n    prompt: hi\n  - id: c2\n    input: yo\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSuite(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "my-suite" || len(s.Cases) != 2 {
		t.Errorf("unexpected suite: %+v", s)
	}
}

func TestLoadSuiteErrors(t *testing.T) {
	if _, err := LoadSuite("/no/such/file.yaml"); err == nil {
		t.Error("expected not-found error")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.yaml")
	os.WriteFile(p, []byte("name: x\ncases: []\n"), 0o644)
	if _, err := LoadSuite(p); err == nil {
		t.Error("expected empty-cases error")
	}
}

func TestRunLoopEchoTerminates(t *testing.T) {
	res, err := RunLoop(context.Background(), "echo", "hello world", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 iterations, got %d", len(res))
	}
	for _, r := range res {
		if r.FinalText != "hello world" {
			t.Errorf("echo should return prompt, got %q", r.FinalText)
		}
		if r.Stopped != "done" {
			t.Errorf("expected stopped=done, got %q", r.Stopped)
		}
	}
}

func TestRunLoopUnknownProvider(t *testing.T) {
	if _, err := RunLoop(context.Background(), "nope", "x", 1); err == nil {
		t.Error("expected unknown-provider error")
	}
}
