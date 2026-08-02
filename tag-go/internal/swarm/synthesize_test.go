package swarm

import (
	"context"
	"strings"
	"testing"
)

// TestSynthesizeWithoutProfileReturnsResultsNotThePrompt is the F1 regression.
// With no synthesis profile the runner returned the synthesis PROMPT — an
// instruction telling an LLM to write the answer — as final_output, with
// status=completed and degraded=false. The caller received scaffolding
// presented as a deliverable.
func TestSynthesizeWithoutProfileReturnsResultsNotThePrompt(t *testing.T) {
	r := &Runner{m: &Manifest{Goal: "ship it"}}
	successful := []TaskResult{
		{TaskID: "alpha", Output: "ALPHA FINDINGS"},
		{TaskID: "beta", Output: "BETA FINDINGS"},
	}
	got := r.synthesize(context.Background(), successful, nil)

	if strings.Contains(got, "Synthesize the above results") {
		t.Errorf("final output must not be the un-executed synthesis prompt:\n%s", got)
	}
	for _, want := range []string{"ALPHA FINDINGS", "BETA FINDINGS"} {
		if !strings.Contains(got, want) {
			t.Errorf("final output must carry the real agent result %q:\n%s", want, got)
		}
	}
}

// A single success is that task's own output, unchanged.
func TestSynthesizeSingleSuccessIsVerbatim(t *testing.T) {
	r := &Runner{m: &Manifest{Goal: "g"}}
	got := r.synthesize(context.Background(), []TaskResult{{TaskID: "solo", Output: "THE ANSWER"}}, nil)
	if got != "THE ANSWER" {
		t.Errorf("got %q", got)
	}
}

// A manifest carrying only a coordinator profile still gets the concatenation,
// never the prompt. (Python resolves coordinator_profile as the synthesizer;
// Go prefers the free, honest fallback over an unasked-for extra model call.)
func TestSynthesisWithOnlyCoordinatorProfile(t *testing.T) {
	r := &Runner{m: &Manifest{Goal: "g", CoordinatorProfile: "coder"}}
	successful := []TaskResult{{TaskID: "a", Output: "A-OUT"}, {TaskID: "b", Output: "B-OUT"}}
	got := r.synthesize(context.Background(), successful, nil)
	if strings.Contains(got, "Synthesize the above results") {
		t.Errorf("prompt leaked as output:\n%s", got)
	}
	if !strings.Contains(got, "A-OUT") || !strings.Contains(got, "B-OUT") {
		t.Errorf("both results must survive:\n%s", got)
	}
}
