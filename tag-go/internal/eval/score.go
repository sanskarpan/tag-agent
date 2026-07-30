package eval

import (
	"fmt"
	"strings"
)

// Score is the outcome of grading one case's output.
type Score struct {
	Passed bool
	// Score is passed_checks/total_checks in [0,1] (Python's score_case shape).
	Score float64
	// Reason is the "; "-joined list of failed checks, empty when all passed.
	Reason string
	// Checks is how many assertions the case actually made. Zero means the case
	// asserts nothing and therefore cannot fail — see Unchecked.
	Checks int
	// Unchecked is true when Checks == 0. Python returns (True, 1.0) for such a
	// case with no signal to the caller, so a suite of assertion-less cases
	// reports "all passed". The runner surfaces this instead of hiding it.
	Unchecked bool
}

// ScoreCase grades model output against a case's deterministic expectations.
// Check-for-check identical to eval_framework.score_case (case-insensitive
// substring checks, regex search, length bounds; score is the fraction of
// checks that passed, but `passed` requires ALL of them), with one addition:
// expected_output is treated as a containment check.
//
// Divergence from Python (deliberate): Python ignores expected_output entirely.
// `eval-dataset export` emits cases whose ONLY field is expected_output, so in
// Python every dataset-derived suite has zero checks and passes unconditionally
// — fabricated success. Here it is a real check.
func ScoreCase(c Case, output string) Score {
	var reasons []string
	checks, passedChecks := 0, 0
	lower := strings.ToLower(output)

	for _, kw := range c.ExpectContains {
		checks++
		if strings.Contains(lower, strings.ToLower(kw)) {
			passedChecks++
		} else {
			reasons = append(reasons, fmt.Sprintf("missing '%s'", kw))
		}
	}
	for _, kw := range c.ExpectNotContain {
		checks++
		if !strings.Contains(lower, strings.ToLower(kw)) {
			passedChecks++
		} else {
			reasons = append(reasons, fmt.Sprintf("should not contain '%s'", kw))
		}
	}
	for i, re := range c.regexes {
		checks++
		if re.MatchString(output) {
			passedChecks++
		} else {
			reasons = append(reasons, fmt.Sprintf("regex not matched: %q", c.ExpectRegex[i]))
		}
	}
	if c.ExpectedOutput != nil {
		checks++
		if strings.Contains(lower, strings.ToLower(*c.ExpectedOutput)) {
			passedChecks++
		} else {
			reasons = append(reasons, fmt.Sprintf("expected_output not found: %q", truncate(*c.ExpectedOutput, 60)))
		}
	}
	if c.MinLength != nil {
		checks++
		if len(output) >= *c.MinLength {
			passedChecks++
		} else {
			reasons = append(reasons, fmt.Sprintf("output too short (%d < %d)", len(output), *c.MinLength))
		}
	}
	if c.MaxLength != nil {
		checks++
		if len(output) <= *c.MaxLength {
			passedChecks++
		} else {
			reasons = append(reasons, fmt.Sprintf("output too long (%d > %d)", len(output), *c.MaxLength))
		}
	}

	if checks == 0 {
		return Score{Passed: true, Score: 1.0, Checks: 0, Unchecked: true}
	}
	return Score{
		Passed: len(reasons) == 0,
		Score:  float64(passedChecks) / float64(checks),
		Reason: strings.Join(reasons, "; "),
		Checks: checks,
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
