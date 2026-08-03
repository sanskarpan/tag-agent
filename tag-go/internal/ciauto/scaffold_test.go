package ciauto

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
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

// TestScaffoldPinsTheInstall is the supply-chain regression: the generated
// workflow used to say `pip install tag-agent`, unpinned, so every user's CI
// picked up whatever PyPI served at run time. A release that changes an exit
// code then changes the meaning of their pipeline with no review on their side.
func TestScaffoldPinsTheInstall(t *testing.T) {
	for _, wf := range []string{"eval", "review", "test-gen", "fix-vuln"} {
		out := ScaffoldGitHubAction(wf)
		if strings.Contains(out, "pip install tag-agent\n") {
			t.Errorf("%s: the generated workflow installs tag-agent unpinned", wf)
		}
		if !strings.Contains(out, "tag-agent~="+ScaffoldPinnedVersion) {
			t.Errorf("%s: expected a pinned install of %s:\n%s", wf, ScaffoldPinnedVersion, out)
		}
	}
}

// A gate that fails a build without saying which of "the tool broke", "you
// invoked it wrong" and "it worked and found problems" happened is a gate people
// disable. The gating workflows must explain their exit codes.
func TestScaffoldDocumentsExitCodes(t *testing.T) {
	// fix-vuln is the only generated workflow whose command actually gates.
	out := ScaffoldGitHubAction("fix-vuln")
	if !strings.Contains(out, "3 = ran fine but") {
		t.Errorf("fix-vuln: exit 3 is not explained:\n%s", out)
	}
	if !strings.Contains(out, "--exit-zero") {
		t.Errorf("fix-vuln: the advisory escape hatch is not mentioned")
	}

	// The comment must describe the command the step RUNS. review-pr has no
	// --exit-zero on either harness, and eval-ci fails the build with exit 1 —
	// an earlier version advertised a flag the step does not accept and called
	// a real gate advisory.
	for wf, mustNot := range map[string]string{
		"review":   "--exit-zero",
		"test-gen": "3 = ran fine but",
		"eval":     "--exit-zero",
	} {
		out := ScaffoldGitHubAction(wf)
		if strings.Contains(out, mustNot) {
			t.Errorf("%s: comment mentions %q, which does not apply to the command it runs:\n%s",
				wf, mustNot, out)
		}
	}
	if out := ScaffoldGitHubAction("eval"); !strings.Contains(out, "1 = it did not") {
		t.Errorf("eval gates with exit 1 and the comment must say so:\n%s", out)
	}
}

// The pin must track the PYTHON release (the generated workflow runs
// `pip install tag-agent`), or it silently freezes every user on an old version
// — which is the opposite failure to the one pinning fixes, and just as quiet.
//
// Deliberately reads pyproject.toml rather than comparing against
// internal/version: that constant is the GO harness's version, a separate
// release track, and asserting against it would pass while being wrong.
func TestScaffoldPinMatchesPythonRelease(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "pyproject.toml"))
	if err != nil {
		t.Fatalf("pyproject.toml not readable from here: %v — a guard that skips is not a guard", err)
	}
	m := regexp.MustCompile(`(?m)^version = "([^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("could not find the version in pyproject.toml")
	}
	if got := string(m[1]); got != ScaffoldPinnedVersion {
		t.Errorf("ScaffoldPinnedVersion = %q but pyproject.toml is at %q — the generated "+
			"workflow would install a version that is not this release", ScaffoldPinnedVersion, got)
	}
}
