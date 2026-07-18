package cli

import "testing"

func TestParseSpec_ValidJSON(t *testing.T) {
	out := `Sure, here is the plan: {"task":"t","rationale":"r","items":[{"id":"a","file":"x.go","description":"d","action":"create"}]} done`
	s := parseSpec("orig task", out)
	if s.Task != "t" || len(s.Items) != 1 {
		t.Fatalf("parseSpec = %+v", s)
	}
	if s.Items[0].File != "x.go" || s.Items[0].Action != "create" || s.Items[0].ID != "a" {
		t.Errorf("item = %+v", s.Items[0])
	}
}

func TestParseSpec_FallbackOnProse(t *testing.T) {
	s := parseSpec("do a thing", "I cannot produce JSON right now.")
	if len(s.Items) != 1 {
		t.Fatalf("fallback should yield 1 item, got %d", len(s.Items))
	}
	if s.Task != "do a thing" || s.Items[0].Description != "do a thing" {
		t.Errorf("fallback spec = %+v", s)
	}
	if s.Items[0].ID == "" || s.Items[0].Action != "modify" {
		t.Errorf("fallback item defaults wrong: %+v", s.Items[0])
	}
}

func TestParseSpec_EmptyItemsFallsBack(t *testing.T) {
	// Valid JSON but no items -> deterministic fallback.
	s := parseSpec("task", `{"task":"t","items":[]}`)
	if len(s.Items) != 1 {
		t.Errorf("empty items should fall back to 1 item, got %d", len(s.Items))
	}
}

func TestNormalizeSpec_FillsDefaults(t *testing.T) {
	s := normalizeSpec("fallback-task", splitSpec{
		Items: []splitItem{{Description: "no id no action no file"}},
	})
	if s.Task != "fallback-task" {
		t.Errorf("task = %q, want fallback-task", s.Task)
	}
	it := s.Items[0]
	if it.ID != "item-1" || it.Action != "modify" || it.File != "TBD" {
		t.Errorf("defaults not filled: %+v", it)
	}
}
