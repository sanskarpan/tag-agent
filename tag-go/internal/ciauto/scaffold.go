// Package ciauto holds offline CI-automation logic ported from Python's
// src/tag/eval_ci.py and src/tag/cmd/ci_loop.py: GitHub Actions workflow
// scaffolding, eval-suite parsing (dry-run), and thin agent-loop orchestration.
// Everything here is exercisable offline (no model/network calls).
package ciauto

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// WorkflowTypes are the valid `eval-ci scaffold --type` choices, mirroring the
// Python argparse choices in cmd/prd_clusters.py.
var WorkflowTypes = []string{"eval", "review", "test-gen", "fix-vuln"}

// DefaultThreshold matches scaffold_github_action's default threshold.
const DefaultThreshold = 0.85

// ValidWorkflowType reports whether t is a known workflow type.
func ValidWorkflowType(t string) bool {
	for _, v := range WorkflowTypes {
		if v == t {
			return true
		}
	}
	return false
}

// runCommands maps workflow type -> the `run:` command, faithfully reproducing
// scaffold_github_action(wf_type). Unknown types fall back to "eval".
func runCommand(workflowType string, threshold float64) string {
	cmds := map[string]string{
		"eval": fmt.Sprintf("tag eval-ci run tests/eval_suite.yaml --threshold %s",
			formatThreshold(threshold)),
		"review": "tag review-pr --repo ${{ github.repository }} " +
			"--pr ${{ github.event.number }} --post-comments",
		"test-gen": "tag agentic-ci test-gen --diff diff.patch --profile coder",
		"fix-vuln": "tag agentic-ci fix-vuln results.sarif --profile reviewer",
	}
	if c, ok := cmds[workflowType]; ok {
		return c
	}
	return cmds["eval"]
}

// formatThreshold renders the threshold the way Python's f-string would (0.85,
// not 0.850000 — Python prints the repr of the float).
func formatThreshold(t float64) string {
	s := strconv.FormatFloat(t, 'f', -1, 64)
	return s
}

// titleize reproduces workflow_type.replace('-', ' ').title().
func titleize(workflowType string) string {
	words := strings.Split(strings.ReplaceAll(workflowType, "-", " "), " ")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// ScaffoldGitHubAction returns the GitHub Actions workflow YAML for the given
// workflow type, a faithful port of scaffold_github_action(wf_type).
func ScaffoldGitHubAction(workflowType string) string {
	if workflowType == "" {
		workflowType = "eval"
	}
	title := titleize(workflowType)
	runCmd := runCommand(workflowType, DefaultThreshold)
	var b strings.Builder
	fmt.Fprintf(&b, "name: TAG %s\n", title)
	b.WriteString("\n")
	b.WriteString("on:\n")
	b.WriteString("  pull_request:\n")
	b.WriteString("    branches: [main, master]\n")
	b.WriteString("\n")
	b.WriteString("jobs:\n")
	fmt.Fprintf(&b, "  tag-%s:\n", workflowType)
	b.WriteString("    runs-on: ubuntu-latest\n")
	b.WriteString("    permissions:\n")
	b.WriteString("      pull-requests: write\n")
	b.WriteString("      contents: read\n")
	b.WriteString("\n")
	b.WriteString("    steps:\n")
	b.WriteString("      - uses: actions/checkout@v4\n")
	b.WriteString("\n")
	b.WriteString("      - name: Set up Python\n")
	b.WriteString("        uses: actions/setup-python@v5\n")
	b.WriteString("        with:\n")
	b.WriteString("          python-version: '3.11'\n")
	b.WriteString("\n")
	b.WriteString("      - name: Install TAG\n")
	b.WriteString("        run: pip install tag-agent\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "      - name: Run TAG %s\n", title)
	b.WriteString("        env:\n")
	b.WriteString("          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}\n")
	b.WriteString("          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}\n")
	fmt.Fprintf(&b, "        run: %s\n", runCmd)
	return b.String()
}

// WorkflowFileName returns the conventional workflow filename (tag-<type>.yml),
// matching install_github_action.
func WorkflowFileName(workflowType string) string {
	return "tag-" + workflowType + ".yml"
}

// TypesHint returns the sorted valid types for error messages.
func TypesHint() string {
	ts := append([]string(nil), WorkflowTypes...)
	sort.Strings(ts)
	return strings.Join(ts, ", ")
}
