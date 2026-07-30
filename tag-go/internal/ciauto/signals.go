package ciauto

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SignalClasses are the PR-review concern areas (PRD-061). Descriptions are
// verbatim from src/tag/ci.py:SIGNAL_CLASSES.
var SignalClasses = map[string]string{
	"security":      "security vulnerabilities, injection, auth issues",
	"correctness":   "logic errors, off-by-one, null pointer, race conditions",
	"coverage":      "untested code paths, missing edge cases",
	"style":         "naming conventions, code style, readability",
	"performance":   "N+1 queries, unnecessary allocations, blocking I/O",
	"documentation": "missing docstrings, unclear variable names",
}

// SignalNames returns the signal class keys, sorted.
func SignalNames() []string {
	out := make([]string, 0, len(SignalClasses))
	for k := range SignalClasses {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NormalizeSignals validates and de-duplicates a requested signal list. An
// empty request means "all classes". Unknown names are an error (usage), never
// silently dropped.
func NormalizeSignals(req []string) ([]string, error) {
	// Accept both `--signals security,style` and repeated `--signals`.
	var flat []string
	for _, r := range req {
		for _, part := range strings.Split(r, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part != "" {
				flat = append(flat, part)
			}
		}
	}
	if len(flat) == 0 {
		return SignalNames(), nil
	}
	var unknown, out []string
	seen := map[string]bool{}
	for _, s := range flat {
		if _, ok := SignalClasses[s]; !ok {
			unknown = append(unknown, s)
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown signal class(es) %v; valid: %s",
			unknown, strings.Join(SignalNames(), ", "))
	}
	return out, nil
}

// PRMetadata is the subset of `gh pr view` output the review prompt uses.
type PRMetadata struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// ReviewSystemPrompt is the system message for `agentic-ci review`.
const ReviewSystemPrompt = "You are a meticulous code reviewer."

// findingsMarker is the literal a reviewer must emit for a class WITH problems.
// It is deliberately NOT one of the class names, see DetectSignalFindings.
const findingsMarker = "FINDINGS"

// BuildReviewPrompt renders the signal-scoped review prompt. Mirrors
// build_review_prompt_with_signals, plus the explicit per-class verdict line
// that makes DetectSignalFindings possible.
func BuildReviewPrompt(diff string, meta PRMetadata, signals []string, maxDiffChars int) string {
	if maxDiffChars <= 0 {
		maxDiffChars = 8000
	}
	if len(diff) > maxDiffChars {
		diff = diff[:maxDiffChars] + fmt.Sprintf("\n\n[... diff truncated at %d chars ...]", maxDiffChars)
	}
	var sb strings.Builder
	for _, s := range signals {
		fmt.Fprintf(&sb, "- **%s**: %s\n", s, SignalClasses[s])
	}
	title := meta.Title
	if title == "" {
		title = "(no title)"
	}
	body := meta.Body
	if strings.TrimSpace(body) == "" {
		body = "(no description)"
	}
	author := meta.Author.Login
	if author == "" {
		author = "unknown"
	}
	labels := make([]string, 0, len(meta.Labels))
	for _, l := range meta.Labels {
		if l.Name != "" {
			labels = append(labels, l.Name)
		}
	}
	labelStr := "(none)"
	if len(labels) > 0 {
		labelStr = strings.Join(labels, ", ")
	}
	return fmt.Sprintf(`Review the following pull request diff focusing ONLY on the signal classes
listed below. Ignore concerns outside the listed signal classes entirely.

## Review Focus Areas

%s
## Pull Request Metadata

- **Title**: %s
- **Author**: %s
- **Base -> Head**: %s -> %s
- **Labels**: %s

### Description

%s

## Diff

`+"```diff"+`
%s
`+"```"+`

For EACH signal class above emit exactly one verdict line of the form:

    <class>: %s   (when you found at least one issue in that class)
    <class>: CLEAN      (when you found none)

Follow each %s line with the specific findings, each with a file:line
reference. End with an overall recommendation: APPROVE, REQUEST_CHANGES, or COMMENT.
`, sb.String(), title, author, meta.BaseRefName, meta.HeadRefName, labelStr, body, diff,
		findingsMarker, findingsMarker)
}

// DetectSignalFindings reports which signal classes the review flagged.
//
// PYTHON BUG NOT PORTED. review_pr_with_signals computed
//
//	signals_found = [s for s in SIGNAL_CLASSES if s.lower() in review_text.lower()]
//
// i.e. it substring-matched the CLASS NAME against the review text. But the
// prompt itself instructs the model to name every class and to write "None" for
// classes with no issues -- so a completely clean review reports every class as
// "found". The field could therefore never be false, which makes it useless as
// a CI gate. This port requires an explicit per-class "<class>: FINDINGS"
// verdict line instead, and returns parsed=false when the model emitted no
// verdict lines at all so the caller can say "could not determine" rather than
// asserting a clean bill of health.
func DetectSignalFindings(reviewText string, signals []string) (found []string, parsed bool) {
	found = []string{}
	lower := strings.ToLower(reviewText)
	sawAnyVerdict := false
	for _, s := range signals {
		verdict := regexp.MustCompile(`(?mi)^\s*[-*#\s]*` + regexp.QuoteMeta(s) + `\s*:\s*(findings|clean)\b`)
		m := verdict.FindStringSubmatch(lower)
		if m == nil {
			continue
		}
		sawAnyVerdict = true
		if strings.EqualFold(m[1], findingsMarker) {
			found = append(found, s)
		}
	}
	return found, sawAnyVerdict
}
