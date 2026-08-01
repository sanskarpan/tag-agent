package worker

import (
	"strings"
	"testing"
)

// PRD-112 unit tests for the flow primitives. These pin the two properties the
// E2E tests can only observe indirectly: a reducer that cannot honour its own
// contract errors rather than degrading, and a guard whose source key is absent
// is false rather than an error (that is the "producing branch was itself
// skipped" case, which must prune quietly, not fail the run).

func TestReduceKinds(t *testing.T) {
	cases := []struct {
		kind, prev, next, want string
	}{
		{"", "a", "b", "b"}, // default is last
		{ReduceLast, "a", "b", "b"},
		{ReduceFirst, "a", "b", "a"},
		{ReduceAppend, "a", "b", "a\nb"},
		{ReduceConcat, "a", "b", "ab"},
		{ReduceSum, "2", "3.5", "5.5"},
		{ReduceMerge, `{"a":1}`, `{"b":2}`, `{"a":1,"b":2}`},
		{ReduceMerge, `{"a":1}`, `{"a":2}`, `{"a":2}`}, // next wins on a clash
	}
	for _, c := range cases {
		got, err := Reduce(c.kind, c.prev, c.next)
		if err != nil {
			t.Errorf("Reduce(%q,%q,%q) errored: %v", c.kind, c.prev, c.next, err)
			continue
		}
		if got != c.want {
			t.Errorf("Reduce(%q,%q,%q) = %q, want %q", c.kind, c.prev, c.next, got, c.want)
		}
	}
}

func TestReduceRefusesToDegrade(t *testing.T) {
	// sum over non-numbers must NOT silently fall back to concatenation, and
	// merge over non-objects must not fall back either: a plausible-looking
	// wrong answer is worse than a reported failure.
	if got, err := Reduce(ReduceSum, "2", "not a number"); err == nil {
		t.Errorf("Reduce(sum, 2, %q) = %q, want an error", "not a number", got)
	}
	if got, err := Reduce(ReduceMerge, `{"a":1}`, `[1,2]`); err == nil {
		t.Errorf("Reduce(merge, obj, array) = %q, want an error", got)
	}
	if _, err := Reduce("frobnicate", "a", "b"); err == nil {
		t.Error("Reduce with an unknown reducer must error")
	}
}

func TestGuardEval(t *testing.T) {
	state := map[string]string{"v": "build PASSED in 3s", "empty": "  "}
	cases := []struct {
		op, source, value string
		want              bool
	}{
		{"contains", "v", "PASSED", true},
		{"contains", "v", "FAILED", false},
		{"not_contains", "v", "FAILED", true},
		{"equals", "v", "build PASSED in 3s", true},
		{"not_equals", "v", "x", true},
		{"prefix", "v", "build", true},
		{"suffix", "v", "3s", true},
		{"matches", "v", `PASS(ED)?`, true},
		{"matches", "v", `^nope`, false},
		{"empty", "empty", "", true},
		{"non_empty", "v", "", true},
		// The key is absent: the producing branch was itself not taken, so the
		// guard is FALSE (prune quietly) rather than an error (fail the run).
		{"contains", "missing", "anything", false},
		{"non_empty", "missing", "", false},
	}
	for _, c := range cases {
		g := &Guard{Source: c.source, Op: c.op, Value: c.value}
		got, err := g.Eval(state)
		if err != nil {
			t.Errorf("Guard{%s %s %q}.Eval errored: %v", c.source, c.op, c.value, err)
			continue
		}
		if got != c.want {
			t.Errorf("Guard{%s %s %q}.Eval = %v, want %v", c.source, c.op, c.value, got, c.want)
		}
	}
	if _, err := (&Guard{Source: "v", Op: "bogus"}).Eval(state); err == nil {
		t.Error("an unknown guard op must error")
	}
	if ok, err := (*Guard)(nil).Eval(state); err != nil || !ok {
		t.Errorf("a nil guard must be true (unconditional edge), got %v %v", ok, err)
	}
}

func TestInterpolate(t *testing.T) {
	state := map[string]string{"a": "ALPHA", "b.c-d": "X"}
	got, err := Interpolate("say {{state.a}} and {{ state.b.c-d }}", state)
	if err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if got != "say ALPHA and X" {
		t.Errorf("Interpolate = %q", got)
	}
	// An unresolvable reference must fail loudly, naming the key.
	_, err = Interpolate("say {{state.ghost}}", state)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("Interpolate with an unknown key = %v, want an error naming 'ghost'", err)
	}
	if refs := StateRefs("{{state.a}} {{state.b.c-d}}"); len(refs) != 2 || refs[0] != "a" || refs[1] != "b.c-d" {
		t.Errorf("StateRefs = %v", refs)
	}
	if refs := StateRefs("no refs here"); len(refs) != 0 {
		t.Errorf("StateRefs on a plain task = %v, want none", refs)
	}
}
