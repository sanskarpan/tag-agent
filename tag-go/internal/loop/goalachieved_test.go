package loop

import "testing"

// TestGoalAchievedNeedsADeclaration is the F2 regression: a model that RESTATES
// the sentinel instruction while saying it is not finished must not complete the
// loop. The prompt-strip alone only defended against a verbatim echo.
func TestGoalAchievedNeedsADeclaration(t *testing.T) {
	prompt := buildPrompt("fix the bug", 1, "")

	mentions := []string{
		"I am working on it. I will output GOAL_ACHIEVED when done. Not finished yet, still analysing.",
		"Once the tests pass I'll say GOAL_ACHIEVED.",
		"The instruction says to output GOAL_ACHIEVED if the goal has been met; it has not.",
		"GOAL_ACHIEVED is what I will print later, but first I need to read the file.",
	}
	for _, out := range mentions {
		if goalAchieved(prompt, out) {
			t.Errorf("a mention must not complete the loop: %q", out)
		}
	}

	declarations := []string{
		"GOAL_ACHIEVED",
		"Fixed the off-by-one in parse().\n\nGOAL_ACHIEVED",
		"All tests pass now. GOAL_ACHIEVED",
		"Done. **GOAL_ACHIEVED**",
		"  GOAL_ACHIEVED  \n",
		"Patched and verified. GOAL_ACHIEVED!",
	}
	for _, out := range declarations {
		if !goalAchieved(prompt, out) {
			t.Errorf("a genuine declaration must complete the loop: %q", out)
		}
	}

	// The original defence must still hold: a full echo of the prompt is inert.
	if goalAchieved(prompt, prompt) {
		t.Error("an echoed prompt must not complete the loop")
	}
	if goalAchieved(prompt, "still thinking") {
		t.Error("output with no marker must not complete the loop")
	}
}
