package swarm

import (
	"strings"
	"testing"
)

// mustJSON is a tiny readability helper for the table cases below.
func parse(t *testing.T, body string, maxAgents int) (*Manifest, error) {
	t.Helper()
	return ParseManifest([]byte(body), maxAgents)
}

const okManifest = `{
  "swarm_id": "s1", "goal": "g",
  "tasks": [
    {"task_id":"a","description":"da","profile":"p","context_slice":{"type":"file_paths","selector":["x.go"]}},
    {"task_id":"b","description":"db","profile":"p","context_slice":{"type":"file_paths","selector":["y.go"]},"depends_on":["a"]}
  ],
  "failure_policy": "best_effort"
}`

func TestParseManifestAcceptsValid(t *testing.T) {
	m, err := parse(t, okManifest, 4)
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if len(m.Tasks) != 2 || m.Tasks[1].DependsOn[0] != "a" {
		t.Fatalf("manifest decoded wrong: %+v", m)
	}
}

// Every rejection rule from Python's _validate_manifest must survive the port.
func TestParseManifestRejections(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"not an object", `[]`, "must be a JSON object"},
		{"missing keys", `{"goal":"g"}`, "missing required keys"},
		{"empty tasks", `{"swarm_id":"s","goal":"g","tasks":[]}`, "non-empty array"},
		{"task missing keys",
			`{"swarm_id":"s","goal":"g","tasks":[{"task_id":"a"}]}`, "Task missing keys"},
		{"bad task id",
			`{"swarm_id":"s","goal":"g","tasks":[{"task_id":"a b","description":"d","profile":"p","context_slice":{"type":"free_text","selector":"x"}}]}`,
			"task_id must be alphanumeric"},
		{"all-punctuation task id",
			`{"swarm_id":"s","goal":"g","tasks":[{"task_id":"___","description":"d","profile":"p","context_slice":{"type":"free_text","selector":"x"}}]}`,
			"task_id must be alphanumeric"},
		{"duplicate task id",
			`{"swarm_id":"s","goal":"g","tasks":[
			  {"task_id":"a","description":"d","profile":"p","context_slice":{"type":"free_text","selector":"x"}},
			  {"task_id":"a","description":"d","profile":"p","context_slice":{"type":"free_text","selector":"y"}}]}`,
			"Duplicate task_id"},
		{"bad slice type",
			`{"swarm_id":"s","goal":"g","tasks":[{"task_id":"a","description":"d","profile":"p","context_slice":{"type":"nope","selector":"x"}}]}`,
			"Invalid context_slice.type"},
		{"overlapping selectors",
			`{"swarm_id":"s","goal":"g","tasks":[
			  {"task_id":"a","description":"d","profile":"p","context_slice":{"type":"file_paths","selector":["x.go"]}},
			  {"task_id":"b","description":"d","profile":"p","context_slice":{"type":"file_paths","selector":["x.go"]}}]}`,
			"Overlapping context_slice selector"},
		{"unknown dependency",
			`{"swarm_id":"s","goal":"g","tasks":[{"task_id":"a","description":"d","profile":"p","context_slice":{"type":"free_text","selector":"x"},"depends_on":["zz"]}]}`,
			"depends_on unknown task_id"},
		{"dependency cycle",
			`{"swarm_id":"s","goal":"g","tasks":[
			  {"task_id":"a","description":"d","profile":"p","context_slice":{"type":"free_text","selector":"x"},"depends_on":["b"]},
			  {"task_id":"b","description":"d","profile":"p","context_slice":{"type":"free_text","selector":"y"},"depends_on":["a"]}]}`,
			"Dependency cycle detected"},
		{"not json", `not json at all`, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.body, 4)
			if err == nil {
				t.Fatalf("%s: expected rejection, got none", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: error %q does not mention %q", tc.name, err, tc.want)
			}
		})
	}
}

// A manifest with more tasks than --max-agents is rejected, not silently
// truncated (Python: "Manifest has N tasks but max_agents=M").
func TestParseManifestRejectsMoreTasksThanMaxAgents(t *testing.T) {
	if _, err := parse(t, okManifest, 1); err == nil ||
		!strings.Contains(err.Error(), "max_agents=1") {
		t.Fatalf("2-task manifest accepted at --max-agents 1: %v", err)
	}
}

// A free_text (string) selector is never compared for overlap, matching Python,
// so two free_text tasks with the same selector are legal.
func TestFreeTextSelectorsDoNotOverlap(t *testing.T) {
	body := `{"swarm_id":"s","goal":"g","tasks":[
	  {"task_id":"a","description":"d","profile":"p","context_slice":{"type":"free_text","selector":"same"}},
	  {"task_id":"b","description":"d","profile":"p","context_slice":{"type":"free_text","selector":"same"}}]}`
	if _, err := parse(t, body, 4); err != nil {
		t.Fatalf("free_text selectors must not trip overlap detection: %v", err)
	}
}

func TestExtractJSONObjectStripsFence(t *testing.T) {
	got, ok := extractJSONObject("```json\n{\"a\":1}\n```")
	if !ok || got != `{"a":1}` {
		t.Fatalf("fence strip = %q ok=%v", got, ok)
	}
	if _, ok := extractJSONObject("no braces here"); ok {
		t.Fatal("prose must not be reported as a JSON object")
	}
}

func TestValidFailurePolicy(t *testing.T) {
	for _, p := range ValidFailurePolicies {
		if !ValidFailurePolicy(p) {
			t.Errorf("%s should be valid", p)
		}
	}
	if ValidFailurePolicy("yolo") {
		t.Error("unknown policy accepted")
	}
}
