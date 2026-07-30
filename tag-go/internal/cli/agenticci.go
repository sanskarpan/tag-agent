package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/ciauto"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/solver"
)

// exitFindings is the process exit status for "the command ran fine and FOUND
// PROBLEMS". A CI gate has to distinguish that from "the command crashed"
// (exit 1) and from "bad invocation" (exit 2), so findings get their own code
// rather than being folded into 1.
//
// Applies to: fix-vuln (vulnerabilities left unfixed), flaky-fix (flaky tests
// detected), review (a requested signal class reported findings). Pass
// --exit-zero on any of the three to force 0 for advisory (non-gating) use.
// test-gen, ci-diagnose and gen-pipeline are advisory by nature and always exit
// 0 on success.
const exitFindings = 3

// registerAgenticCI wires `tag agentic-ci`:
//
//   - the LEGACY form `tag agentic-ci <task> [--check CMD]` (parity roadmap
//     #527), which still runs the real check→fix→re-check loop, and
//   - six subcommands ported from src/tag/cmd/prd_clusters.py:cmd_ci_ext —
//     test-gen (PRD-057), fix-vuln (PRD-059), ci-diagnose (PRD-060),
//     review (PRD-061), gen-pipeline (PRD-062) and flaky-fix (PRD-063).
//
// Cobra dispatches a first argument that matches a subcommand name to that
// subcommand and otherwise falls through to the parent's RunE, so both forms
// coexist. `tag agentic-ci "fix the flaky build"` still works.
func registerAgenticCI(root *cobra.Command, app *App) {
	var provider, check, repo string
	var maxIters int
	c := &cobra.Command{
		Use:     "agentic-ci <task>",
		Short:   "Agentic CI: check→fix loop, test-gen, fix-vuln, ci-diagnose, review, gen-pipeline, flaky-fix",
		GroupID: "orch",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return jsonErrorMaybe(usageErrorf("unknown provider %q (available: %v)", provider, providerNames()))
			}
			db, _ := app.OpenDB()
			model := app.Cfg.String("profiles."+app.profile("")+".config.model.default", "")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			res, err := solver.Solve(ctx, db, prov, model, solver.Options{
				Kind:     solver.KindCI,
				Task:     args[0],
				MaxIters: maxIters,
				CheckCmd: check,
				RepoPath: repo,
			})
			if err != nil {
				return jsonErrorMaybe(err)
			}
			return emitSolveResult(res)
		},
	}
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().IntVar(&maxIters, "max-iters", 1, "max check→fix iterations")
	c.Flags().StringVar(&check, "check", "", "check command (build/test); enables the real check→fix loop")
	c.Flags().StringVar(&repo, "repo", "", "working directory for the check command")

	c.AddCommand(
		newTestGenCmd(app),
		newFixVulnCmd(app),
		newCIDiagnoseCmd(app, "ci-diagnose"),
		newReviewCmd(app),
		newGenPipelineCmd(app),
		newFlakyFixCmd(app),
	)
	root.AddCommand(c)
}

// ---------------------------------------------------------------------------
// shared plumbing
// ---------------------------------------------------------------------------

// aciCommon carries the flags every LLM-backed agentic-ci subcommand accepts.
type aciCommon struct {
	provider string
	profile  string
	maxSteps int
	perms    permFlags
}

func (a *aciCommon) bind(c *cobra.Command) {
	c.Flags().StringVar(&a.provider, "provider", "echo", "llm provider (echo = offline, no API calls)")
	c.Flags().StringVar(&a.profile, "profile", "", "TAG profile (selects the model)")
	c.Flags().IntVar(&a.maxSteps, "max-steps", 6, "max agent-loop steps per LLM pass (budget stop condition)")
	a.perms.bind(c)
}

// runner builds the agent runner for one invocation: provider lookup, model from
// the profile, a trace recorder wired to the spans table, and a permission Guard
// forced NON-INTERACTIVE exactly as buildWorkerOptions does for the queue worker
// — these commands run unattended in CI and must never block on a consent prompt.
// `ask` therefore resolves to an immediate, explained deny.
func (a *aciCommon) runner(app *App, toolRoot string) (*ciauto.Runner, error) {
	prov, ok := llm.Registry[a.provider]
	if !ok {
		return nil, usageErrorf("unknown provider %q (available: %v)", a.provider, providerNames())
	}
	pf := a.perms
	pf.noPrompt = true
	guard, err := pf.guard(app, a.profile, "", os.Stderr)
	if err != nil {
		return nil, err
	}
	db, _ := app.OpenDB()
	model := app.Cfg.String("profiles."+app.profile(a.profile)+".config.model.default", "")
	r := ciauto.NewRunner(db, prov, model)
	r.Guard = guard
	r.ToolRoot = toolRoot
	if a.maxSteps > 0 {
		r.MaxSteps = a.maxSteps
	}
	return r, nil
}

// aciContext returns a context cancelled on SIGINT/SIGTERM so a terminated run
// never strands a half-written file or an unflushed span batch (same pattern as
// `queue worker` / `dag run --execute`).
func aciContext(cmd *cobra.Command) (context.Context, func()) {
	return signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
}

// aciEmit renders a result payload: JSON when --json, otherwise the text
// callback. It returns exitCodeErr{exitFindings} when findings were reported and
// the caller did not opt out, so the process exit code carries the verdict
// WITHOUT an extra "error:" line (the report is already on stdout).
func aciEmit(payload any, text func(), findings, exitZero bool) error {
	if flagJSON {
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	} else {
		text()
	}
	if findings && !exitZero {
		return exitCodeErr{code: exitFindings}
	}
	return nil
}

// aciFail maps an error to the right exit code and, under --json, to a
// structured {"error": ...} object. Malformed/absent input is a USAGE error
// (exit 2) — a broken SARIF or an empty log is the operator's mistake, and a CI
// gate must be able to tell it apart from a genuine runtime failure.
func aciFail(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ciauto.ErrMalformedSARIF) || errors.Is(err, ciauto.ErrEmptyLog) || errors.Is(err, os.ErrNotExist) {
		err = usageErr{err}
	}
	return jsonErrorMaybe(err)
}

// aciWrite writes a file through the permission guard. Everything these
// commands put on disk — generated tests, rewritten sources, .gitlab-ci.yml —
// goes through here, so the consent model governs the CLI's own writes and not
// just the model's tool calls. A nil guard is a wiring bug and fails CLOSED
// (permission.Guard.decide denies for a nil receiver).
func aciWrite(ctx context.Context, guard *permission.Guard, path string, content []byte) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	req := permission.Request{
		Tool:    permission.ToolWriteFile,
		Kind:    permission.KindPath,
		Subject: abs,
		Args:    map[string]any{"path": abs, "bytes": len(content)},
	}
	if d := guard.Check(ctx, req); !d.Allowed() {
		return fmt.Errorf("refusing to write %s: %s (grant it with --allow-tool write_file, or --allow-tool %s)",
			path, d.Reason, permission.ToolWriteFile+":'"+filepath.Base(abs)+"'")
	}
	if dir := filepath.Dir(abs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(abs, content, 0o644)
}

// notes accumulates honesty notes in order, skipping empties.
type notes []string

func (n *notes) add(format string, a ...any) { *n = append(*n, fmt.Sprintf(format, a...)) }
func (n notes) slice() []string {
	if n == nil {
		return []string{}
	}
	return n
}

func printNotes(n []string) {
	for _, s := range n {
		fmt.Printf("note: %s\n", s)
	}
}

// ---------------------------------------------------------------------------
// PRD-057: test-gen
// ---------------------------------------------------------------------------

// testGenResult is the --json contract for test-gen.
type testGenResult struct {
	Command   string   `json:"command"`
	TraceID   string   `json:"trace_id"`
	Provider  string   `json:"provider"`
	Framework string   `json:"framework"`
	DiffBytes int      `json:"diff_bytes"`
	Tests     string   `json:"tests"`
	OutputPat string   `json:"output_path,omitempty"`
	Written   bool     `json:"written"`
	Degraded  bool     `json:"degraded"`
	Steps     int      `json:"steps"`
	Stopped   string   `json:"stopped"`
	Notes     []string `json:"notes"`
}

func newTestGenCmd(app *App) *cobra.Command {
	var common aciCommon
	var diffArg, outPath, framework, repo string
	var staged, force bool
	c := &cobra.Command{
		Use:   "test-gen",
		Short: "Generate tests for a code diff (PRD-057)",
		Long: "Generate tests for a unified diff.\n\n" +
			"The diff comes from --diff (a file path, or the diff text itself), --staged\n" +
			"(git diff --cached), or stdin when --diff is '-'. Exit 0 on success, 2 on a\n" +
			"usage error, 1 on a runtime failure.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := aciContext(cmd)
			defer stop()

			diff, err := readDiffInput(cmd, diffArg, staged, repo)
			if err != nil {
				return aciFail(err)
			}
			if strings.TrimSpace(diff) == "" {
				// Python generated "tests" from an empty diff and printed whatever the
				// model returned: a confidently-worded answer about nothing. Refuse.
				return aciFail(usageErrorf("empty diff: nothing to generate tests for (use --diff FILE|TEXT, --diff - for stdin, or --staged)"))
			}
			if framework == "" {
				framework = ciauto.DetectTestFramework(strOr(repo, "."))
			}

			r, err := common.runner(app, "")
			if err != nil {
				return aciFail(err)
			}
			defer r.Flush()

			text, steps, stopped, err := r.Run(ctx, ciauto.TestGenSystemPrompt, ciauto.BuildTestGenPrompt(framework, diff))
			if err != nil {
				return aciFail(err)
			}

			var ns notes
			res := testGenResult{
				Command: "test-gen", TraceID: r.TraceID, Provider: r.Provider.Name(),
				Framework: framework, DiffBytes: len(diff), Tests: text,
				OutputPat: outPath, Steps: steps, Stopped: stopped,
			}
			if r.Offline() {
				res.Degraded = true
				ns.add("%s", ciauto.EchoNote)
				ns.add("the emitted text is ECHOED CONTEXT, not generated tests — do not commit it.")
			}
			if outPath != "" {
				switch {
				case r.Offline() && !force:
					ns.add("refusing to write %s: provider=echo produced echoed context, not tests. Re-run with a real --provider, or --force-write to save the echo anyway.", outPath)
				case strings.TrimSpace(text) == "":
					ns.add("refusing to write %s: the model returned no text.", outPath)
				default:
					if werr := aciWrite(ctx, r.Guard, outPath, []byte(text+"\n")); werr != nil {
						ns.add("write failed: %v", werr)
					} else {
						res.Written = true
						ns.add("wrote %d byte(s) of generated tests to %s", len(text)+1, outPath)
					}
				}
			}
			res.Notes = ns.slice()
			return aciEmit(res, func() {
				fmt.Printf("[test-gen] trace %s via %s (framework=%s, %d diff bytes, %d step(s), %s)\n",
					res.TraceID, res.Provider, res.Framework, res.DiffBytes, res.Steps, res.Stopped)
				fmt.Println(text)
				printNotes(res.Notes)
			}, false, false)
		},
	}
	common.bind(c)
	c.Flags().StringVar(&diffArg, "diff", "", "diff file path, literal diff text, or '-' for stdin")
	c.Flags().BoolVar(&staged, "staged", false, "use `git diff --cached` from --repo")
	c.Flags().StringVar(&repo, "repo", "", "repository root (default: cwd)")
	c.Flags().StringVar(&outPath, "out", "", "write the generated tests to this file (needs --allow-tool write_file)")
	c.Flags().StringVar(&framework, "framework", "", "test framework (default: auto-detected)")
	c.Flags().BoolVar(&force, "force-write", false, "write --out even when the provider is the offline echo stub")
	return c
}

// readDiffInput resolves the diff from --diff / --staged / stdin. Mirrors the
// Python behaviour ("--diff is a path if it exists, else the diff text itself")
// and adds explicit stdin and --staged sources.
func readDiffInput(cmd *cobra.Command, diffArg string, staged bool, repo string) (string, error) {
	if staged && diffArg != "" {
		return "", usageErrorf("use either --staged or --diff, not both")
	}
	if staged {
		g := exec.Command("git", "diff", "--cached")
		if repo != "" {
			g.Dir = repo
		}
		out, err := g.Output()
		if err != nil {
			return "", fmt.Errorf("git diff --cached failed (not a git repo?): %v", err)
		}
		return string(out), nil
	}
	if diffArg == "-" {
		b, err := readAllStdin(cmd)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if diffArg == "" {
		return "", nil
	}
	if st, err := os.Stat(diffArg); err == nil && !st.IsDir() {
		b, rerr := os.ReadFile(diffArg)
		if rerr != nil {
			return "", rerr
		}
		return string(b), nil
	}
	return diffArg, nil
}

func readAllStdin(cmd *cobra.Command) ([]byte, error) {
	var buf strings.Builder
	b := make([]byte, 32*1024)
	in := cmd.InOrStdin()
	for {
		n, err := in.Read(b)
		buf.Write(b[:n])
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			if n == 0 {
				break
			}
		}
		if n == 0 {
			break
		}
	}
	return []byte(buf.String()), nil
}

// ---------------------------------------------------------------------------
// PRD-059: fix-vuln
// ---------------------------------------------------------------------------

// vulnFix is one finding plus what was done about it.
type vulnFix struct {
	Finding     ciauto.Finding `json:"finding"`
	FixProposed bool           `json:"fix_proposed"`
	FixApplied  bool           `json:"fix_applied"`
	Skipped     string         `json:"skipped,omitempty"`
	Diagnosis   string         `json:"diagnosis,omitempty"`
}

type fixVulnResult struct {
	Command   string    `json:"command"`
	TraceID   string    `json:"trace_id"`
	Provider  string    `json:"provider"`
	SARIF     string    `json:"sarif"`
	Total     int       `json:"total_findings"`
	Selected  int       `json:"selected_findings"`
	Proposed  int       `json:"fixes_proposed"`
	Applied   int       `json:"fixes_applied"`
	Unfixed   int       `json:"unfixed"`
	Apply     bool      `json:"apply"`
	Degraded  bool      `json:"degraded"`
	Results   []vulnFix `json:"results"`
	Notes     []string  `json:"notes"`
	ExitCodes string    `json:"exit_code_meaning"`
}

func newFixVulnCmd(app *App) *cobra.Command {
	var common aciCommon
	var repo string
	var severity []string
	var maxFixes int
	var apply, dryRun, exitZero bool
	c := &cobra.Command{
		Use:   "fix-vuln <SARIF_FILE>",
		Short: "Auto-remediate SAST findings from a SARIF 2.1.0 file (PRD-059)",
		Long: "Parse a SARIF 2.1.0 file and propose (or with --apply, write) a fix per finding.\n\n" +
			"Exit codes: 0 = ran fine, nothing left unfixed; " + strconv.Itoa(exitFindings) +
			" = ran fine, findings remain; 1 = runtime failure; 2 = usage error (incl. malformed SARIF).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := aciContext(cmd)
			defer stop()

			if apply && dryRun {
				return aciFail(usageErrorf("--apply and --dry-run are mutually exclusive"))
			}
			findings, err := ciauto.ParseSARIF(args[0])
			if err != nil {
				return aciFail(err)
			}
			total := len(findings)
			selected, err := ciauto.FilterBySeverity(findings, severity)
			if err != nil {
				return aciFail(usageErr{err})
			}
			ciauto.SortFindings(selected)

			root := strOr(repo, ".")
			r, err := common.runner(app, root)
			if err != nil {
				return aciFail(err)
			}
			defer r.Flush()

			var ns notes
			res := fixVulnResult{
				Command: "fix-vuln", TraceID: r.TraceID, Provider: r.Provider.Name(),
				SARIF: args[0], Total: total, Selected: len(selected), Apply: apply,
				Results: []vulnFix{},
				ExitCodes: fmt.Sprintf("0=clean, %d=findings remain, 1=runtime error, 2=usage error",
					exitFindings),
			}
			if len(severity) > 0 && len(selected) != total {
				ns.add("severity filter %v selected %d of %d finding(s); the rest were NOT examined.",
					severity, len(selected), total)
			}
			if r.Offline() {
				res.Degraded = true
				ns.add("%s", ciauto.EchoNote)
				ns.add("with provider=echo no real remediation is possible: findings are reported and left UNFIXED.")
			}
			if !apply {
				ns.add("report-only mode: no file was modified. Pass --apply (plus --allow-tool write_file) to write fixes.")
			}

			budget := maxFixes
			for i := range selected {
				f := selected[i]
				rec := vulnFix{Finding: f}
				if err := ctx.Err(); err != nil {
					rec.Skipped = "interrupted before this finding was processed"
					res.Results = append(res.Results, rec)
					continue
				}
				target, rerr := ciauto.ResolveInRepo(root, f.Path)
				switch {
				case f.Path == "":
					rec.Skipped = "finding has no file location"
				case rerr != nil:
					rec.Skipped = rerr.Error()
				default:
					if st, serr := os.Stat(target); serr != nil || st.IsDir() {
						rec.Skipped = fmt.Sprintf("file not found in repo: %s", f.Path)
					}
				}
				if rec.Skipped == "" && budget == 0 {
					rec.Skipped = fmt.Sprintf("--max-fixes %d budget exhausted", maxFixes)
				}
				if rec.Skipped != "" {
					res.Results = append(res.Results, rec)
					continue
				}
				if budget > 0 {
					budget--
				}
				content, cerr := os.ReadFile(target)
				if cerr != nil {
					rec.Skipped = "cannot read file: " + cerr.Error()
					res.Results = append(res.Results, rec)
					continue
				}
				text, _, _, aerr := r.Run(ctx, ciauto.VulnFixSystemPrompt, ciauto.BuildVulnFixPrompt(f, string(content)))
				if aerr != nil {
					if ctx.Err() != nil {
						rec.Skipped = "interrupted: " + aerr.Error()
						res.Results = append(res.Results, rec)
						break
					}
					rec.Skipped = "agent failed: " + aerr.Error()
					res.Results = append(res.Results, rec)
					continue
				}
				if strings.TrimSpace(text) == "" {
					rec.Skipped = "the model returned no content"
					res.Results = append(res.Results, rec)
					continue
				}
				// The echo provider returns the prompt, never a fix. Recording that as
				// a diagnosis/proposed fix would be exactly the fabricated-success
				// pattern, so the field is left empty and the finding stays UNFIXED.
				if r.Offline() {
					rec.Skipped = "provider=echo cannot produce a fix (echoed context only)"
					res.Results = append(res.Results, rec)
					continue
				}
				rec.Diagnosis = text
				rec.FixProposed = true
				res.Proposed++
				if apply {
					if werr := aciWrite(ctx, r.Guard, target, []byte(text)); werr != nil {
						rec.Skipped = werr.Error()
					} else {
						rec.FixApplied = true
						res.Applied++
					}
				}
				res.Results = append(res.Results, rec)
			}
			for _, rr := range res.Results {
				if !rr.FixApplied {
					res.Unfixed++
				}
			}
			if ctx.Err() != nil {
				ns.add("interrupted by a signal: processing stopped early; already-written files were left in place.")
			}
			res.Notes = ns.slice()

			return aciEmit(res, func() {
				fmt.Printf("[fix-vuln] trace %s via %s — %d finding(s), %d selected, %d proposed, %d applied, %d unfixed\n",
					res.TraceID, res.Provider, res.Total, res.Selected, res.Proposed, res.Applied, res.Unfixed)
				for _, rr := range res.Results {
					loc := rr.Finding.Path
					if rr.Finding.StartLine > 0 {
						loc = fmt.Sprintf("%s:%d", loc, rr.Finding.StartLine)
					}
					status := "UNFIXED"
					if rr.FixApplied {
						status = "FIXED"
					} else if rr.FixProposed {
						status = "PROPOSED"
					}
					fmt.Printf("  [%-7s] %-8s %-24s %s\n", rr.Finding.Severity, status, rr.Finding.RuleID, loc)
					if rr.Skipped != "" {
						fmt.Printf("            skipped: %s\n", rr.Skipped)
					}
				}
				printNotes(res.Notes)
			}, res.Unfixed > 0, exitZero)
		},
	}
	common.bind(c)
	c.Flags().StringVar(&repo, "repo", "", "repository root the SARIF paths are relative to (default: cwd)")
	c.Flags().StringSliceVar(&severity, "severity", nil, "only process these SARIF levels (error,warning,note,none)")
	c.Flags().IntVar(&maxFixes, "max-fixes", 10, "max findings to send to the model (0 = report only)")
	c.Flags().BoolVar(&apply, "apply", false, "WRITE the fixes to disk (needs --allow-tool write_file)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "explicit no-write mode (this is already the default)")
	c.Flags().BoolVar(&exitZero, "exit-zero", false, "exit 0 even when findings remain (advisory, non-gating)")
	return c
}

// ---------------------------------------------------------------------------
// PRD-060: ci-diagnose
// ---------------------------------------------------------------------------

type diagnoseResult struct {
	Command   string         `json:"command"`
	TraceID   string         `json:"trace_id"`
	Provider  string         `json:"provider"`
	Log       string         `json:"log"`
	Parsed    bool           `json:"parsed"`
	Failure   ciauto.Failure `json:"failure"`
	Diagnosis string         `json:"diagnosis"`
	Degraded  bool           `json:"degraded"`
	Steps     int            `json:"steps"`
	Stopped   string         `json:"stopped"`
	Notes     []string       `json:"notes"`
}

// newCIDiagnoseCmd builds the diagnose command under the given name, so it can
// be attached as both `agentic-ci ci-diagnose` and `ci diagnose` (Python exposes
// both spellings and dispatches them identically).
func newCIDiagnoseCmd(app *App, name string) *cobra.Command {
	var common aciCommon
	c := &cobra.Command{
		Use:   name + " <LOG_FILE>",
		Short: "Diagnose a CI failure from its log (PRD-060)",
		Long: "Extract the failure signal from a CI log and produce a root-cause diagnosis.\n\n" +
			"This is an ADVISORY command: it reports the diagnosis and the concrete change\n" +
			"to make; it never edits your repository. Exit 0 on success, 2 on a usage error\n" +
			"(missing/empty log), 1 on a runtime failure.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := aciContext(cmd)
			defer stop()

			log, err := ciauto.ReadCILog(args[0])
			if err != nil {
				return aciFail(err)
			}
			failure := ciauto.ParseCIFailure(log)

			r, err := common.runner(app, "")
			if err != nil {
				return aciFail(err)
			}
			defer r.Flush()

			text, steps, stopped, err := r.Run(ctx, ciauto.DiagnoseSystemPrompt, ciauto.BuildDiagnosePrompt(failure, log))
			if err != nil {
				return aciFail(err)
			}
			var ns notes
			res := diagnoseResult{
				Command: "ci-diagnose", TraceID: r.TraceID, Provider: r.Provider.Name(),
				Log: args[0], Parsed: !failure.Empty(), Failure: failure,
				Diagnosis: text, Steps: steps, Stopped: stopped,
			}
			if failure.Empty() {
				ns.add("no known failure pattern matched this log (pytest/jest/go/rust/python traceback): the diagnosis below is based on the raw log tail alone.")
			}
			if r.Offline() {
				res.Degraded = true
				ns.add("%s", ciauto.EchoNote)
				ns.add("with provider=echo the 'diagnosis' section is the echoed prompt, NOT an analysis. The structured `failure` fields ARE real — they come from parsing your log, not from a model.")
			}
			ns.add("no repository file was modified; apply the suggested change yourself, or drive it with `tag agentic-ci \"<task>\" --check \"<cmd>\"`.")
			res.Notes = ns.slice()

			return aciEmit(res, func() {
				fmt.Printf("[ci-diagnose] trace %s via %s (%d step(s), %s)\n", res.TraceID, res.Provider, res.Steps, res.Stopped)
				fmt.Printf("failure signal (parsed=%v): type=%s test=%s file=%s line=%d\n",
					res.Parsed, orDash(failure.ErrorType), orDash(failure.TestName), orDash(failure.FilePath), failure.LineNumber)
				if failure.ErrorMessage != "" {
					fmt.Printf("  message: %s\n", failure.ErrorMessage)
				}
				fmt.Println("--- diagnosis ---")
				fmt.Println(text)
				printNotes(res.Notes)
			}, false, false)
		},
	}
	common.bind(c)
	return c
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// ---------------------------------------------------------------------------
// PRD-061: review --signals
// ---------------------------------------------------------------------------

type reviewResult struct {
	Command       string   `json:"command"`
	TraceID       string   `json:"trace_id"`
	Provider      string   `json:"provider"`
	PRRef         string   `json:"pr_ref"`
	Repo          string   `json:"repo,omitempty"`
	PRNumber      int      `json:"pr_number,omitempty"`
	Signals       []string `json:"signals"`
	SignalsFound  []string `json:"signals_found"`
	VerdictParsed bool     `json:"verdict_parsed"`
	Review        string   `json:"review_text"`
	Posted        bool     `json:"posted"`
	Degraded      bool     `json:"degraded"`
	Steps         int      `json:"steps"`
	Stopped       string   `json:"stopped"`
	Notes         []string `json:"notes"`
}

// prRefRe matches `owner/repo#42`, `#42` and a bare `42`.
var prRefRe = regexp.MustCompile(`^(?:([\w.\-]+/[\w.\-]+))?#?(\d+)$`)

func parsePRRef(ref string) (repo string, num int, err error) {
	m := prRefRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return "", 0, usageErrorf("bad PR reference %q (expected owner/repo#42, #42 or 42)", ref)
	}
	n, _ := strconv.Atoi(m[2])
	if n <= 0 {
		return "", 0, usageErrorf("bad PR number in %q", ref)
	}
	return m[1], n, nil
}

func newReviewCmd(app *App) *cobra.Command {
	var common aciCommon
	var signals []string
	var diffFile string
	var post, exitZero bool
	c := &cobra.Command{
		Use:   "review <PR_REF>",
		Short: "Review a PR scoped to specific signal classes (PRD-061)",
		Long: "Review a pull request, restricting the review to the requested signal classes.\n\n" +
			"The diff and metadata are fetched with the `gh` CLI (same pattern as `tag\n" +
			"review-pr`); pass --diff FILE to review offline with no gh at all.\n\n" +
			"Exit codes: 0 = no signal class reported findings; " + strconv.Itoa(exitFindings) +
			" = at least one did; 1 = runtime failure; 2 = usage error.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := aciContext(cmd)
			defer stop()

			repo, num, err := parsePRRef(args[0])
			if err != nil {
				return aciFail(err)
			}
			active, err := ciauto.NormalizeSignals(signals)
			if err != nil {
				return aciFail(usageErr{err})
			}
			// Validate --post BEFORE touching the network: an invocation that can
			// never legitimately post must fail as a usage error, not after a gh
			// round-trip that could itself fail for an unrelated reason.
			if post {
				if diffFile != "" {
					return aciFail(usageErrorf("--post needs a real PR (it cannot be combined with --diff)"))
				}
				if common.provider == "echo" {
					return aciFail(usageErrorf("--post refuses provider=echo: it echoes the diff rather than reviewing it; pass a real --provider to post a genuine review"))
				}
			}

			var ns notes
			var diff string
			var meta ciauto.PRMetadata
			if diffFile != "" {
				b, rerr := os.ReadFile(diffFile)
				if rerr != nil {
					return aciFail(rerr)
				}
				diff = string(b)
				ns.add("reviewed the local diff %s; PR metadata (title/author/labels) was NOT fetched.", diffFile)
			} else {
				d, ferr := fetchPRDiffViaGH(num, repo)
				if ferr != nil {
					return aciFail(fmt.Errorf("%w (or pass --diff FILE to review a local diff offline)", ferr))
				}
				diff = d
				m, merr := fetchPRMetadataViaGH(num, repo)
				if merr != nil {
					ns.add("PR metadata could not be fetched (%v); reviewing the diff without title/author/labels.", merr)
				} else {
					meta = m
				}
			}
			if strings.TrimSpace(diff) == "" {
				return aciFail(usageErrorf("the diff for PR %s is empty; nothing to review", args[0]))
			}
			r, err := common.runner(app, "")
			if err != nil {
				return aciFail(err)
			}
			defer r.Flush()

			text, steps, stopped, err := r.Run(ctx, ciauto.ReviewSystemPrompt,
				ciauto.BuildReviewPrompt(diff, meta, active, 8000))
			if err != nil {
				return aciFail(err)
			}
			found, parsed := ciauto.DetectSignalFindings(text, active)

			res := reviewResult{
				Command: "review", TraceID: r.TraceID, Provider: r.Provider.Name(),
				PRRef: args[0], Repo: repo, PRNumber: num,
				Signals: active, SignalsFound: found, VerdictParsed: parsed,
				Review: text, Steps: steps, Stopped: stopped,
			}
			if r.Offline() {
				res.Degraded = true
				ns.add("%s", ciauto.EchoNote)
			}
			if !parsed {
				ns.add("the reviewer emitted no '<class>: FINDINGS|CLEAN' verdict lines, so signals_found could NOT be determined; treating this run as inconclusive (exit 0), NOT as a clean bill of health.")
			}
			if post {
				body := fmt.Sprintf("## TAG code review (signals: %s)\n\n%s\n\n---\n*Generated by tag agentic-ci review*",
					strings.Join(active, ", "), text)
				if perr := postPRReviewViaGH(num, repo, body); perr != nil {
					ns.add("review generated but posting failed: %v", perr)
				} else {
					res.Posted = true
					ns.add("posted the review as a comment on PR #%d via gh.", num)
				}
			} else {
				ns.add("review NOT posted; pass --post to comment on the PR.")
			}
			res.Notes = ns.slice()

			return aciEmit(res, func() {
				fmt.Printf("[review] trace %s via %s — PR %s, signals: %s\n",
					res.TraceID, res.Provider, res.PRRef, strings.Join(active, ", "))
				fmt.Println(text)
				if parsed {
					if len(found) == 0 {
						fmt.Println("verdict: CLEAN on every requested signal class")
					} else {
						fmt.Printf("verdict: FINDINGS in %s\n", strings.Join(found, ", "))
					}
				} else {
					fmt.Println("verdict: INCONCLUSIVE (no machine-readable verdict lines in the review)")
				}
				printNotes(res.Notes)
			}, parsed && len(found) > 0, exitZero)
		},
	}
	common.bind(c)
	c.Flags().StringSliceVar(&signals, "signals", nil,
		"signal classes to review (comma-separated or repeated; default: all). Valid: "+strings.Join(ciauto.SignalNames(), ", "))
	c.Flags().StringVar(&diffFile, "diff", "", "review this local diff file instead of fetching the PR via gh")
	c.Flags().BoolVar(&post, "post", false, "post the review as a PR comment via gh")
	c.Flags().BoolVar(&exitZero, "exit-zero", false, "exit 0 even when a signal class reports findings")
	return c
}

// fetchPRMetadataViaGH fetches PR title/body/author/branches/labels via
// `gh pr view --json`. Reuses the degrade-honestly pattern of reviewpr.go.
func fetchPRMetadataViaGH(pr int, repo string) (ciauto.PRMetadata, error) {
	var meta ciauto.PRMetadata
	if _, err := exec.LookPath("gh"); err != nil {
		return meta, fmt.Errorf("gh CLI not found on PATH")
	}
	args := []string{"pr", "view", strconv.Itoa(pr), "--json", "title,body,author,baseRefName,headRefName,labels"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		detail := strings.TrimSpace(string(exitStderr(err)))
		if detail == "" {
			detail = err.Error()
		}
		return meta, fmt.Errorf("gh pr view failed: %s", truncateNote(detail, 200))
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return meta, fmt.Errorf("gh pr view returned unparseable JSON: %v", err)
	}
	return meta, nil
}

// ---------------------------------------------------------------------------
// PRD-062: gen-pipeline
// ---------------------------------------------------------------------------

type genPipelineResult struct {
	Command  string   `json:"command"`
	Repo     string   `json:"repo"`
	Stack    []string `json:"stack"`
	Stages   []string `json:"stages"`
	Out      string   `json:"out"`
	Written  bool     `json:"written"`
	DryRun   bool     `json:"dry_run"`
	Pipeline string   `json:"pipeline"`
	Notes    []string `json:"notes"`
}

func newGenPipelineCmd(app *App) *cobra.Command {
	var perms permFlags
	var repo, out string
	var stages []string
	var force, dryRun, includeDeploy bool
	c := &cobra.Command{
		Use:   "gen-pipeline",
		Short: "Generate a GitLab CI pipeline from the detected stack (PRD-062)",
		Long: "Detect the repository's technology stack and render a .gitlab-ci.yml.\n\n" +
			"This command is fully deterministic and makes NO model calls, so it behaves\n" +
			"identically online and offline. Exit 0 on success, 2 on a usage error\n" +
			"(including refusing to overwrite an existing file without --force).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := aciContext(cmd)
			defer stop()

			root := strOr(repo, ".")
			if st, err := os.Stat(root); err != nil || !st.IsDir() {
				return aciFail(usageErrorf("--repo %q is not a directory", root))
			}
			stack := ciauto.DetectStack(root)
			if len(stages) == 0 {
				stages = []string{"build", "test", "deploy"}
			}
			yaml := ciauto.GenerateGitLabPipeline(stack, ciauto.PipelineOptions{
				Stages: stages, IncludeDeploy: includeDeploy,
			})
			dest := out
			if dest == "" {
				dest = filepath.Join(root, ".gitlab-ci.yml")
			}

			var ns notes
			res := genPipelineResult{
				Command: "gen-pipeline", Repo: root, Stack: stack, Stages: stages,
				Out: dest, DryRun: dryRun, Pipeline: yaml,
			}
			if res.Stack == nil {
				res.Stack = []string{}
			}
			if len(stack) == 0 {
				ns.add("no known stack detected under %s: the generated pipeline is a placeholder you must fill in.", root)
			}
			switch {
			case dryRun:
				ns.add("--dry-run: nothing was written to disk.")
			default:
				if _, err := os.Stat(dest); err == nil && !force {
					return aciFail(usageErrorf("%s already exists; pass --force to overwrite (or --dry-run to print it)", dest))
				}
				guard, gerr := guardFor(app, &perms)
				if gerr != nil {
					return aciFail(gerr)
				}
				if werr := aciWrite(ctx, guard, dest, []byte(yaml)); werr != nil {
					return aciFail(werr)
				}
				res.Written = true
				ns.add("wrote %s (%d bytes)", dest, len(yaml))
			}
			res.Notes = ns.slice()

			return aciEmit(res, func() {
				if dryRun {
					fmt.Print(yaml)
				}
				fmt.Printf("[gen-pipeline] repo=%s stack=[%s] stages=[%s] out=%s written=%v\n",
					root, strings.Join(stack, " "), strings.Join(stages, " "), dest, res.Written)
				printNotes(res.Notes)
			}, false, false)
		},
	}
	perms.bind(c)
	c.Flags().StringVar(&repo, "repo", "", "repository root to analyse (default: cwd)")
	c.Flags().StringVar(&out, "out", "", "output path (default: <repo>/.gitlab-ci.yml)")
	c.Flags().StringSliceVar(&stages, "stages", nil, "pipeline stages (default: build,test,deploy)")
	c.Flags().BoolVar(&includeDeploy, "include-deploy", false, "append a manual deploy-to-staging job")
	c.Flags().BoolVar(&force, "force", false, "overwrite an existing output file")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the pipeline to stdout without writing it")
	return c
}

// guardFor builds a non-interactive guard for the LLM-free subcommands.
func guardFor(app *App, perms *permFlags) (*permission.Guard, error) {
	pf := permFlags{}
	if perms != nil {
		pf = *perms
	}
	pf.noPrompt = true
	return pf.guard(app, "", "", os.Stderr)
}

// ---------------------------------------------------------------------------
// PRD-063: flaky-fix
// ---------------------------------------------------------------------------

type flakyFix struct {
	ciauto.FlakyTest
	TestFile    string `json:"test_file,omitempty"`
	FixProposed bool   `json:"fix_proposed"`
	FixApplied  bool   `json:"fix_applied"`
	Explanation string `json:"explanation,omitempty"`
	Skipped     string `json:"skipped,omitempty"`
}

type flakyFixResult struct {
	Command    string     `json:"command"`
	TraceID    string     `json:"trace_id"`
	Provider   string     `json:"provider"`
	Log        string     `json:"log"`
	FlakyCount int        `json:"flaky_count"`
	Quarantine int        `json:"quarantine_count"`
	Proposed   int        `json:"fixes_proposed"`
	Applied    int        `json:"fixes_applied"`
	Apply      bool       `json:"apply"`
	Degraded   bool       `json:"degraded"`
	Results    []flakyFix `json:"results"`
	Notes      []string   `json:"notes"`
}

func newFlakyFixCmd(app *App) *cobra.Command {
	var common aciCommon
	var repo string
	var maxFixes int
	var threshold float64
	var apply, dryRun, exitZero bool
	c := &cobra.Command{
		Use:   "flaky-fix <LOG_FILE>",
		Short: "Detect flaky tests in a multi-run log and propose fixes (PRD-063)",
		Long: "Parse a concatenated multi-run test log (pytest / jest / go test -v / cargo test)\n" +
			"and report every test that both PASSED and FAILED, then propose a deterministic\n" +
			"rewrite for each.\n\n" +
			"Exit codes: 0 = no flaky tests; " + strconv.Itoa(exitFindings) +
			" = flaky tests detected; 1 = runtime failure; 2 = usage error (missing/empty log).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := aciContext(cmd)
			defer stop()

			if apply && dryRun {
				return aciFail(usageErrorf("--apply and --dry-run are mutually exclusive"))
			}
			if threshold < 0 || threshold > 1 {
				return aciFail(usageErrorf("--quarantine-threshold must be between 0 and 1 (got %v)", threshold))
			}
			log, err := ciauto.ReadTestLog(args[0])
			if err != nil {
				return aciFail(err)
			}
			flaky := ciauto.DetectFlaky(log, threshold)

			root := strOr(repo, ".")
			r, err := common.runner(app, root)
			if err != nil {
				return aciFail(err)
			}
			defer r.Flush()

			var ns notes
			res := flakyFixResult{
				Command: "flaky-fix", TraceID: r.TraceID, Provider: r.Provider.Name(),
				Log: args[0], FlakyCount: len(flaky), Apply: apply, Results: []flakyFix{},
			}
			for _, f := range flaky {
				if f.Quarantine {
					res.Quarantine++
				}
			}
			if len(flaky) == 0 {
				ns.add("no test both passed AND failed in %s. That means either the suite is stable, or the log holds only ONE run — flakiness cannot be detected from a single run.", args[0])
			}
			if r.Offline() {
				res.Degraded = true
				ns.add("%s", ciauto.EchoNote)
				ns.add("with provider=echo no rewrite is possible: flaky tests are DETECTED (that part is real, it is pure log parsing) but not fixed.")
			}
			if !apply {
				ns.add("report-only mode: no test file was modified. Pass --apply (plus --allow-tool write_file) to write rewrites.")
			}

			budget := maxFixes
			for _, f := range flaky {
				rec := flakyFix{FlakyTest: f}
				if err := ctx.Err(); err != nil {
					rec.Skipped = "interrupted before this test was processed"
					res.Results = append(res.Results, rec)
					continue
				}
				file := ciauto.ResolveTestFile(root, f.TestName)
				rec.TestFile = file
				switch {
				case file == "":
					rec.Skipped = fmt.Sprintf("could not locate a source file for %q under %s", f.TestName, root)
				case budget == 0:
					rec.Skipped = fmt.Sprintf("--max-fixes %d budget exhausted", maxFixes)
				case r.Offline():
					rec.Skipped = "provider=echo cannot produce a rewrite (echoed context only)"
				}
				if rec.Skipped != "" {
					res.Results = append(res.Results, rec)
					continue
				}
				if budget > 0 {
					budget--
				}
				content, cerr := os.ReadFile(file)
				if cerr != nil {
					rec.Skipped = "cannot read test file: " + cerr.Error()
					res.Results = append(res.Results, rec)
					continue
				}
				text, _, _, aerr := r.Run(ctx, ciauto.FlakySystemPrompt, ciauto.BuildFlakyFixPrompt(f, file, string(content)))
				if aerr != nil {
					if ctx.Err() != nil {
						rec.Skipped = "interrupted: " + aerr.Error()
						res.Results = append(res.Results, rec)
						break
					}
					rec.Skipped = "agent failed: " + aerr.Error()
					res.Results = append(res.Results, rec)
					continue
				}
				code, expl, ok := ciauto.SplitFlakyFix(text)
				if !ok || strings.TrimSpace(code) == "" {
					// Python wrote the whole raw response to the test file whenever the
					// EXPLANATION marker was absent, i.e. it could clobber a test with
					// prose. Refuse instead.
					rec.Skipped = "the model did not follow the '<file>\\nEXPLANATION: ...' contract; refusing to overwrite the test file with unparseable output"
					res.Results = append(res.Results, rec)
					continue
				}
				rec.FixProposed = true
				rec.Explanation = expl
				res.Proposed++
				if apply {
					if werr := aciWrite(ctx, r.Guard, file, []byte(code+"\n")); werr != nil {
						rec.Skipped = werr.Error()
					} else {
						rec.FixApplied = true
						res.Applied++
					}
				}
				res.Results = append(res.Results, rec)
			}
			if ctx.Err() != nil {
				ns.add("interrupted by a signal: processing stopped early; already-written files were left in place.")
			}
			res.Notes = ns.slice()

			return aciEmit(res, func() {
				fmt.Printf("[flaky-fix] trace %s via %s — %d flaky test(s), %d over the quarantine threshold, %d proposed, %d applied\n",
					res.TraceID, res.Provider, res.FlakyCount, res.Quarantine, res.Proposed, res.Applied)
				for _, rr := range res.Results {
					q := ""
					if rr.Quarantine {
						q = " QUARANTINE"
					}
					fmt.Printf("  %-50s pass=%d fail=%d score=%.4f fail_rate=%.2f%s\n",
						rr.TestName, rr.PassCount, rr.FailCount, rr.FlakinessScore, rr.FailRate, q)
					if rr.TestFile != "" {
						fmt.Printf("      file: %s\n", rr.TestFile)
					}
					if rr.Explanation != "" {
						fmt.Printf("      cause: %s\n", rr.Explanation)
					}
					if rr.Skipped != "" {
						fmt.Printf("      skipped: %s\n", rr.Skipped)
					}
				}
				printNotes(res.Notes)
			}, res.FlakyCount > 0, exitZero)
		},
	}
	common.bind(c)
	c.Flags().StringVar(&repo, "repo", "", "repository root used to resolve test names to files (default: cwd)")
	c.Flags().IntVar(&maxFixes, "max-fixes", 5, "max flaky tests to send to the model (0 = detect only)")
	c.Flags().Float64Var(&threshold, "quarantine-threshold", ciauto.DefaultQuarantineThreshold,
		"fail-rate at or above which a test is flagged quarantine=true")
	c.Flags().BoolVar(&apply, "apply", false, "WRITE the rewrites to disk (needs --allow-tool write_file)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "explicit no-write mode (this is already the default)")
	c.Flags().BoolVar(&exitZero, "exit-zero", false, "exit 0 even when flaky tests are detected")
	return c
}
