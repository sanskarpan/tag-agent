package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestE2ESplitPlanOfflineDoesNotPersistPlaceholder is the "no fake success"
// guard for `split plan`.
//
// architectPrompt embeds an EXAMPLE spec so the shape is unambiguous, and the
// offline echo provider replays the last user message verbatim — so parseSpec
// happily parsed the example back out and persisted it as a real plan:
// spec.task == "...", items[0].file == "path", description == "...", with
// "status":"planned" and exit 0. Nothing was planned, but every signal said it
// had been. `mem2 extract` ("Extracted 0 memories", exit 0) is the model for
// honest offline behaviour.
func TestE2ESplitPlanOfflineDoesNotPersistPlaceholder(t *testing.T) {
	h := newHome(t)
	task := "add retry logic to the http client"
	out, code := runJSONOK(t, h, "split", "plan", task)
	if code != 0 {
		t.Fatalf("split plan: exit %d: %s", code, out)
	}
	var res struct {
		RunID string `json:"run_id"`
		Task  string `json:"task"`
		Spec  struct {
			Task      string `json:"task"`
			Rationale string `json:"rationale"`
			Items     []struct {
				ID          string `json:"id"`
				File        string `json:"file"`
				Description string `json:"description"`
				Action      string `json:"action"`
			} `json:"items"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("split plan --json is not valid JSON (%v): %s", err, out)
	}
	if res.Spec.Task == "..." {
		t.Errorf("persisted spec.task is the prompt's placeholder %q", res.Spec.Task)
	}
	if res.Spec.Task != task {
		t.Errorf("persisted spec.task = %q, want the real task %q", res.Spec.Task, task)
	}
	if len(res.Spec.Items) == 0 {
		t.Fatalf("no items persisted: %s", out)
	}
	for i, it := range res.Spec.Items {
		if it.File == "path" {
			t.Errorf("item %d file is the prompt's placeholder %q", i, it.File)
		}
		if it.Description == "..." {
			t.Errorf("item %d description is the prompt's placeholder %q", i, it.Description)
		}
	}
	// The stored run must agree with what was reported.
	show, showCode := runJSONOK(t, h, "split", "show", res.RunID)
	if showCode != 0 {
		t.Fatalf("split show: exit %d: %s", showCode, show)
	}
	if strings.Contains(show, `"file": "path"`) || strings.Contains(show, `"file":"path"`) {
		t.Errorf("split show still reports the placeholder file: %s", show)
	}
}

// TestE2ESplitPlanOfflineIsHonestInHumanOutput: a user reading the default
// (non-JSON) output must be able to tell nothing was really planned.
func TestE2ESplitPlanOfflineIsHonestInHumanOutput(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "split", "plan", "add retry logic to the http client")
	if code != 0 {
		t.Fatalf("split plan: exit %d: %s", code, out)
	}
	if strings.Contains(out, "path") && !strings.Contains(out, "TBD") {
		t.Errorf("human output looks like a real plan: %s", out)
	}
	if !strings.Contains(out, "architect returned no structured spec") {
		t.Errorf("human output does not disclose that no real plan was produced: %s", out)
	}
}

// TestE2ESplitPlanKeepsRealSpecs: the placeholder rejection must not throw away
// a genuine spec that happens to reuse one placeholder token.
func TestE2ESplitPlanKeepsRealSpecs(t *testing.T) {
	h := newHome(t)
	spec := `{"task":"real task","rationale":"because","items":[` +
		`{"id":"item-1","file":"internal/llm/httpclient.go","description":"add backoff","action":"modify"}]}`
	out, code := runJSONOK(t, h, "split", "plan", "real task", "--spec-json", spec)
	if code != 0 {
		t.Fatalf("split plan --spec-json: exit %d: %s", code, out)
	}
	if !strings.Contains(out, "internal/llm/httpclient.go") {
		t.Errorf("a real spec was discarded: %s", out)
	}
	if strings.Contains(out, "architect returned no structured spec") {
		t.Errorf("a real spec was replaced by the fallback: %s", out)
	}
}

// runJSONOK runs a command with --json and returns stdout only.
func runJSONOK(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	stdout, _, code := runIO(t, home, append([]string{"--json"}, args...)...)
	return stdout, code
}
