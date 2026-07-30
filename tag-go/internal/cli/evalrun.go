package cli

import (
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/eval"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/store"
)

// registerEvalRun adds `tag eval run` — the executing centre of the eval
// framework (PRD-027). Port of src/tag/cmd/marketplace.py:cmd_eval's "run"
// branch plus src/tag/eval_framework.py.
//
// Divergences from Python, each deliberate (see the report / comments):
//   - Python shells out to `python -m tag submit ...` and scores that
//     subprocess's STDOUT — which is the queue-submission acknowledgement, not
//     the model's answer. Every score in Python is computed against the wrong
//     text. Go drives the case through the native agent loop and scores the
//     model's final text.
//   - Python's --dry-run WRITES a full set of passed=1, score=1.0 case rows, so
//     a dry run is indistinguishable from a real all-green run in `eval list`.
//     Go's --dry-run validates and plans, and persists nothing.
//   - Python leaves a crashed/interrupted run stranded in status 'running'
//     forever. Go always finalizes ('completed' | 'cancelled').
func registerEvalRun(parent *cobra.Command, app *App) {
	var (
		suitePath   string
		datasetName string
		profile     string
		provider    string
		model       string
		system      string
		dryRun      bool
		maxSteps    int
		concurrency int
		caseTimeout time.Duration
		withTools   bool
		judge       bool
		judgeProv   string
		judgeModel  string
		judgeThresh float64
		perms       permFlags
	)

	run := &cobra.Command{
		Use:   "run",
		Short: "Run an eval suite against a profile",
		Long: "Run an eval suite (YAML via --suite, or a stored dataset via --dataset) through the\n" +
			"agent loop, score each case, and persist the results for `tag eval list` / `tag eval show`.\n\n" +
			"Exit codes: 0 = every case passed, 1 = at least one case failed OR the run crashed\n" +
			"(Python's convention; a failing run prints a results table and a summary, a crashed run\n" +
			"prints `error: ...` / a JSON {\"error\": ...} object), 2 = usage error.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if suitePath == "" && datasetName == "" {
				return usageErrorf("--suite SUITE_PATH (or --dataset NAME) required")
			}
			if suitePath != "" && datasetName != "" {
				return usageErrorf("--suite and --dataset are mutually exclusive")
			}
			if concurrency < 1 {
				return usageErrorf("--concurrency must be >= 1")
			}
			prov, ok := llm.Registry[provider]
			if !ok {
				return usageErrorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			var jprov llm.Provider = prov
			if judgeProv != "" {
				jp, ok := llm.Registry[judgeProv]
				if !ok {
					return usageErrorf("unknown --judge-provider %q (available: %v)", judgeProv, providerNames())
				}
				jprov = jp
			}

			db, err := app.OpenDB()
			if err != nil {
				return err
			}

			var suite *eval.Suite
			label := suitePath
			if datasetName != "" {
				suite, err = suiteFromDataset(db, datasetName)
				if err != nil {
					return jsonErrorMaybe(err)
				}
				label = "dataset:" + datasetName
			} else {
				suite, err = eval.LoadSuite(suitePath)
				if err != nil {
					// A malformed suite is bad input, not a runtime fault: exit 2.
					return jsonErrorMaybe(usageErr{err})
				}
			}

			prof := app.profile(profile)
			suiteName := suite.Name
			if suiteName == "" {
				suiteName = label
			}

			if dryRun {
				if flagJSON {
					ids := make([]string, 0, len(suite.Cases))
					for _, c := range suite.Cases {
						ids = append(ids, c.ID)
					}
					return emitJSON(map[string]any{
						"dry_run": true, "suite_name": suiteName, "suite_path": label,
						"profile": prof, "provider": prov.Name(),
						"total": len(suite.Cases), "cases": ids,
						"note": "validation only: no model calls, no eval run recorded",
					})
				}
				fmt.Printf("DRY RUN — suite validated, no model calls, nothing recorded.\n")
				fmt.Printf("Suite: %s  (%s)  Profile: %s\n", suiteName, label, prof)
				fmt.Printf("Would run %d case(s):\n", len(suite.Cases))
				for _, c := range suite.Cases {
					fmt.Printf("  - %s\n", c.ID)
				}
				return nil
			}

			guard, err := buildEvalGuard(app, &perms, withTools, prof)
			if err != nil {
				return err
			}

			// SIGINT/SIGTERM must not strand the run in 'running' (same pattern as
			// `queue worker` / `dag run --execute` in queue.go).
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if !flagJSON {
				fmt.Printf("Eval suite: %s  profile: %s  provider: %s\n", suiteName, prof, prov.Name())
				if prov.Name() == "echo" {
					fmt.Fprintf(os.Stderr, "WARNING: %s\n", eval.OfflineNote)
				}
				fmt.Printf("Running %d case(s)...\n", len(suite.Cases))
			}

			opts := eval.Options{
				Provider: prov,
				Model:    model,
				ModelForProfile: func(p string) string {
					return app.Cfg.String("profiles."+p+".config.model.default", "")
				},
				Profile: prof, System: system, MaxSteps: maxSteps,
				WithTools: withTools, Guard: guard,
				Concurrency: concurrency, CaseTimeout: caseTimeout,
				Judge: judge, JudgeProvider: jprov, JudgeModel: judgeModel, JudgeThreshold: judgeThresh,
			}
			if !flagJSON {
				opts.OnCase = func(r eval.CaseResult) {
					icon := "✗"
					if r.Passed {
						icon = "✓"
					}
					line := fmt.Sprintf("  [%s] %s  score=%.2f", icon, r.CaseID, r.Score)
					if r.Trivial {
						line += "  (TRIVIAL: same checks pass against the prompt alone — not a real pass)"
					}
					if r.Unchecked {
						line += "  (NO CHECKS: this case cannot fail)"
					}
					if r.FailureReason != "" {
						line += "  " + r.FailureReason
					}
					fmt.Println(line)
				}
			}

			sum, err := eval.Run(ctx, db.DB, suite, label, opts)
			if err != nil {
				return jsonErrorMaybe(err)
			}

			if flagJSON {
				if err := emitJSON(sum); err != nil {
					return err
				}
			} else {
				fmt.Printf("\nResults: %d/%d passed", sum.Passed, sum.Total)
				if sum.CostUSD != nil {
					fmt.Printf("  cost=$%.6f", *sum.CostUSD)
				}
				fmt.Printf("  run=%s\n", sum.EvalRunID)
				for _, n := range sum.Notes {
					if n == eval.OfflineNote {
						continue // already shown as the pre-run WARNING
					}
					fmt.Fprintf(os.Stderr, "note: %s\n", n)
				}
			}
			// Python: `return 0 if failed == 0 else 1`. A cancelled run is also a
			// non-success even if the cases it managed to run all passed.
			if sum.Failed > 0 || sum.Status != eval.StatusCompleted {
				return exitCodeErr{1}
			}
			return nil
		},
	}

	run.Flags().StringVar(&suitePath, "suite", "", "path to YAML eval suite")
	run.Flags().StringVar(&datasetName, "dataset", "", "run a stored eval-dataset instead of a YAML suite")
	run.Flags().StringVar(&profile, "profile", "", "profile to evaluate")
	run.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline stub; results are NOT meaningful)")
	run.Flags().StringVar(&model, "model", "", "model id (default: profile config.model.default)")
	run.Flags().StringVar(&system, "system", "", "system prompt for every case")
	run.Flags().BoolVar(&dryRun, "dry-run", false, "validate the suite and list the cases without calling a model or recording a run")
	run.Flags().IntVar(&maxSteps, "max-steps", 0, "cap agent-loop turns per case (0 = loop default)")
	run.Flags().IntVar(&concurrency, "concurrency", 1, "run this many cases at once (each gets its own working directory)")
	run.Flags().DurationVar(&caseTimeout, "case-timeout", eval.DefaultCaseTimeout, "per-case timeout")
	run.Flags().BoolVar(&withTools, "tools", false, "enable built-in tools (bash/read_file/write_file/list_dir) for each case")
	run.Flags().BoolVar(&judge, "judge", false, "additionally score each case with the LLM judge (eval-judge); a case must pass BOTH")
	run.Flags().StringVar(&judgeProv, "judge-provider", "", "provider for the judge (default: --provider)")
	run.Flags().StringVar(&judgeModel, "judge-model", "", "model for the judge")
	run.Flags().Float64Var(&judgeThresh, "judge-threshold", 0, "judge pass threshold 0..1 (default 0.7)")
	perms.bind(run)

	parent.AddCommand(run)
}

// buildEvalGuard resolves the consent gate for an eval run. An eval run is
// unattended by definition, so it forces the non-interactive path exactly as
// buildWorkerOptions does in queue.go: `ask` resolves to an immediate deny with
// a reason instead of blocking the run on a prompt nobody is there to answer.
// When tools are off there is nothing to gate; tool.Register's nil-Guard
// fallback (the secure default policy) still applies if that ever changes.
func buildEvalGuard(app *App, perms *permFlags, withTools bool, profile string) (*permission.Guard, error) {
	if !withTools {
		return nil, nil
	}
	pf := permFlags{}
	if perms != nil {
		pf = *perms
	}
	pf.noPrompt = true
	return pf.guard(app, profile, "", os.Stderr)
}

// suiteFromDataset builds an in-memory suite from a stored eval dataset
// (eval-dataset create/add-case/import), so a dataset can be executed without a
// round-trip through `eval-dataset export`. expected_output becomes a real
// check — see eval.ScoreCase.
func suiteFromDataset(db *store.DB, nameOrID string) (*eval.Suite, error) {
	dsID, ok, err := getDataset(db, nameOrID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("dataset not found: %q", nameOrID)
	}
	var name, description string
	if err := db.QueryRow(`SELECT name, description FROM eval_datasets WHERE id=?`, dsID).Scan(&name, &description); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT case_id, input, expected_output, reference_context
		FROM eval_dataset_cases WHERE dataset_id=? ORDER BY created_at, case_id`, dsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	s := &eval.Suite{Name: name, Description: description}
	seen := map[string]bool{}
	for rows.Next() {
		var caseID, input string
		var expected, ref sql.NullString
		if err := rows.Scan(&caseID, &input, &expected, &ref); err != nil {
			return nil, err
		}
		if strings.TrimSpace(caseID) == "" {
			caseID = fmt.Sprintf("case_%d", len(s.Cases)+1)
		}
		if seen[caseID] {
			return nil, fmt.Errorf("dataset %q has duplicate case id %q", nameOrID, caseID)
		}
		seen[caseID] = true
		c := eval.Case{ID: caseID, Input: input, Reference: ref.String}
		if expected.Valid {
			v := expected.String
			c.ExpectedOutput = &v
		}
		s.Cases = append(s.Cases, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(s.Cases) == 0 {
		return nil, fmt.Errorf("dataset %q has no cases", nameOrID)
	}
	return s, nil
}
