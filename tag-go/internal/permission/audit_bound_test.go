package permission

import (
	"strings"
	"testing"
)

// TestSummarizeArgsTruncatesNestedValues is the regression: only TOP-LEVEL
// strings were truncated, so a payload one level down went into the audit row
// verbatim — from an argument map the model controls.
func TestSummarizeArgsTruncatesNestedValues(t *testing.T) {
	big := strings.Repeat("A", 100_000)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"nested map", map[string]any{"data": map[string]any{"blob": big}}},
		{"nested slice", map[string]any{"items": []any{big}}},
		{"map in slice", map[string]any{"items": []any{map[string]any{"b": big}}}},
		{"top level", map[string]any{"content": big}},
	}
	for _, tc := range cases {
		got := SummarizeArgs(tc.args)
		if len(got) > maxSummaryBytes {
			t.Errorf("%s: summary is %d bytes, over the %d ceiling", tc.name, len(got), maxSummaryBytes)
		}
		if strings.Contains(got, strings.Repeat("A", 1000)) {
			t.Errorf("%s: the payload reached the audit row untruncated (%d bytes)", tc.name, len(got))
		}
	}
}

// Many small values still add up, so the whole-summary ceiling has to hold on
// its own — and what it returns must still be parseable.
func TestSummarizeArgsHasAWholeSummaryCeiling(t *testing.T) {
	args := map[string]any{}
	for i := 0; i < 500; i++ {
		args[string(rune('a'+i%26))+strings.Repeat("k", i%50)] = strings.Repeat("v", 150)
	}
	got := SummarizeArgs(args)
	if len(got) > maxSummaryBytes {
		t.Errorf("summary is %d bytes, over the %d ceiling", len(got), maxSummaryBytes)
	}
	if !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") {
		t.Errorf("an over-ceiling summary must still be JSON, got %q", got)
	}
}

// Deep nesting is a peer-supplied shape too; the walk must terminate.
func TestSummarizeArgsBoundsDepth(t *testing.T) {
	var v any = "leaf"
	for i := 0; i < 200; i++ {
		v = map[string]any{"n": v}
	}
	got := SummarizeArgs(map[string]any{"deep": v})
	if len(got) > maxSummaryBytes {
		t.Errorf("summary is %d bytes, over the ceiling", len(got))
	}
	if !strings.Contains(got, "[nested]") {
		t.Errorf("expected the depth bound to be visible in the output: %q", got)
	}
}

// Ordinary arguments must round-trip readably — a bound that mangles normal
// input trades one problem for another.
func TestSummarizeArgsKeepsOrdinaryInputReadable(t *testing.T) {
	got := SummarizeArgs(map[string]any{
		"path":    "internal/tool/tools.go",
		"count":   3,
		"flags":   []any{"-a", "-b"},
		"nested":  map[string]any{"k": "v"},
		"enabled": true,
	})
	for _, want := range []string{`"path":"internal/tool/tools.go"`, `"count":3`, `"-a"`, `"k":"v"`, `"enabled":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %q", want, got)
		}
	}
	if strings.Contains(got, "_truncated") {
		t.Errorf("ordinary input must not trip the ceiling: %q", got)
	}
}
