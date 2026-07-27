package cli

import "testing"

// TestParseSpecRejectsPromptPlaceholder feeds parseSpec exactly what the echo
// provider returns — architectPrompt replayed verbatim — and asserts the
// prompt's own example is not mistaken for a plan.
func TestParseSpecRejectsPromptPlaceholder(t *testing.T) {
	task := "add retry logic to the http client"
	got := parseSpec(task, architectPrompt(task))
	if got.Task != task {
		t.Errorf("spec.task = %q, want %q", got.Task, task)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want the 1-item fallback: %+v", len(got.Items), got.Items)
	}
	if got.Items[0].File == placeholderFile {
		t.Errorf("item file = %q, the prompt's placeholder", got.Items[0].File)
	}
	if got.Items[0].Description == placeholderEllipsis {
		t.Errorf("item description = %q, the prompt's placeholder", got.Items[0].Description)
	}
	if got.Rationale == placeholderEllipsis || got.Rationale == "" {
		t.Errorf("rationale = %q, want the honest fallback rationale", got.Rationale)
	}
}

func TestParseSpecKeepsGenuineSpecs(t *testing.T) {
	task := "add retry logic"
	out := `Sure! {"task":"add retry logic","rationale":"backoff","items":[` +
		`{"id":"item-1","file":"internal/llm/httpclient.go","description":"add backoff","action":"modify"}]}`
	got := parseSpec(task, out)
	if len(got.Items) != 1 || got.Items[0].File != "internal/llm/httpclient.go" {
		t.Errorf("genuine spec was discarded: %+v", got)
	}
	if got.Rationale != "backoff" {
		t.Errorf("rationale = %q, want %q", got.Rationale, "backoff")
	}
}

// A partly-placeholder spec still carries real work and must be kept.
func TestParseSpecKeepsPartiallyConcreteSpecs(t *testing.T) {
	task := "t"
	out := `{"task":"t","rationale":"r","items":[` +
		`{"id":"item-1","file":"path","description":"..."},` +
		`{"id":"item-2","file":"internal/real.go","description":"do a real thing"}]}`
	got := parseSpec(task, out)
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2 (spec kept): %+v", len(got.Items), got.Items)
	}
}

func TestIsPlaceholderSpec(t *testing.T) {
	cases := []struct {
		name string
		spec splitSpec
		want bool
	}{
		{"empty", splitSpec{}, true},
		{"exact template", splitSpec{Items: []splitItem{{File: "path", Description: "..."}}}, true},
		{"real file", splitSpec{Items: []splitItem{{File: "a.go", Description: "..."}}}, false},
		{"real description", splitSpec{Items: []splitItem{{File: "path", Description: "do it"}}}, false},
	}
	for _, tc := range cases {
		if got := isPlaceholderSpec(tc.spec); got != tc.want {
			t.Errorf("%s: isPlaceholderSpec = %v, want %v", tc.name, got, tc.want)
		}
	}
}
