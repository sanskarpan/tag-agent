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
	fmt.Fprintf(&b, "        # Version-bounded deliberately. An unbounded install lets a PyPI release\n")
	fmt.Fprintf(&b, "        # change this pipeline's behaviour -- including its exit codes -- with no\n")
	fmt.Fprintf(&b, "        # review on your side.\n")
	fmt.Fprintf(&b, "        #\n")
	fmt.Fprintf(&b, "        # A compatible-release bound rather than ==: patch and minor fixes still\n")
	fmt.Fprintf(&b, "        # reach you, while the next major (which is where exit codes change) does\n")
	fmt.Fprintf(&b, "        # not. Dependabot cannot see a pin inside a run: line, so the marker below\n")
	fmt.Fprintf(&b, "        # is what lets Renovate bump it for you.\n")
	fmt.Fprintf(&b, "        # renovate: datasource=pypi depName=tag-agent\n")
	fmt.Fprintf(&b, "        run: pip install 'tag-agent~=%s'\n", ScaffoldPinnedVersion)
	b.WriteString("\n")
	fmt.Fprintf(&b, "      - name: Run TAG %s\n", title)
	b.WriteString("        env:\n")
	b.WriteString("          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}\n")
	b.WriteString("          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}\n")
	b.WriteString(exitCodeComment(workflowType))
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

// ScaffoldPinnedVersion is the tag-agent release the generated workflow installs.
//
// The generated workflow used to say `pip install tag-agent`, unpinned. That
// handed every user's CI whatever PyPI served at run time -- so a release that
// changes an exit code (0.10.0 changed several) silently changes the meaning of
// their pipeline, with no review on their side and no way to notice until a
// build goes red or, worse, stops going red.
//
// This is the same reasoning pyproject.toml already applies to TAG's own
// dependencies, where every pin is exact because "ranges allow PyPI to ship a
// fresh version at any time without a code review on our side". Generating
// unpinned installs for other people while pinning our own was inconsistent.
//
// Keep this in step with the released version when cutting a release.
const ScaffoldPinnedVersion = "0.12.0"

// exitCodeComment documents what a non-zero exit means for the generated step,
// so a red build is self-explaining rather than a mystery.
//
// It is keyed on the COMMAND the step actually runs, not on the workflow name.
// An earlier version keyed on the workflow name and advertised --exit-zero next
// to `tag review-pr`, which has no such flag on either harness -- the exit-3
// contract lives in `agentic-ci review`, a different command. A comment that
// describes a flag the step does not accept is worse than no comment.
func exitCodeComment(workflowType string) string {
	switch workflowType {
	case "fix-vuln":
		// runCommand emits `tag agentic-ci fix-vuln`, which does gate.
		return "        # Exit codes: 0 = ok; 3 = ran fine but vulnerabilities remain unfixed\n" +
			"        # (this fails the step on purpose — add --exit-zero to make it advisory);\n" +
			"        # 1 = the run itself failed; 2 = usage error.\n"
	case "eval":
		// `tag eval-ci run` fails the build with exit 1 when the suite is under
		// threshold. Calling that advisory would be wrong.
		return "        # Exit codes: 0 = the suite met the threshold; 1 = it did not, OR the run\n" +
			"        # failed (eval-ci does not distinguish the two); 2 = usage error.\n"
	case "review":
		// `tag review-pr` reports and returns 0; it is not a gate.
		return "        # Exit codes: 0 = the review ran (findings are reported, not gated);\n" +
			"        # 1 = the run failed; 2 = usage error. For a gating review use\n" +
			"        # `tag agentic-ci review <PR_REF>`, which exits 3 on findings.\n"
	default:
		// test-gen and anything else are advisory by nature.
		return "        # Exit codes: 0 = ok, 1 = the run failed, 2 = usage error.\n"
	}
}
