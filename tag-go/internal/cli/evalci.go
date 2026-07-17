package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/ciauto"
)

// registerEvalCI wires the `eval-ci` command (scaffold + offline run).
// Port of src/tag/cmd/prd_clusters.py:cmd_eval_ci. The `run` path is dry-run
// only (no model calls) since the Go build must stay offline in tests.
func registerEvalCI(root *cobra.Command, app *App) {
	ev := &cobra.Command{Use: "eval-ci", Short: "Eval CI gate and GitHub Action scaffold", GroupID: "obs"}

	var cixScaffoldType, cixScaffoldOut string
	scaffold := &cobra.Command{
		Use:   "scaffold",
		Short: "Scaffold a GitHub Actions workflow YAML",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !ciauto.ValidWorkflowType(cixScaffoldType) {
				return fmt.Errorf("invalid --type %q (choose one of: %s)", cixScaffoldType, ciauto.TypesHint())
			}
			yaml := ciauto.ScaffoldGitHubAction(cixScaffoldType)
			if cixScaffoldOut != "" {
				if err := os.WriteFile(cixScaffoldOut, []byte(yaml), 0o644); err != nil {
					return err
				}
				outJSON(map[string]any{"wrote": cixScaffoldOut, "type": cixScaffoldType}, "Wrote "+cixScaffoldOut)
				return nil
			}
			if flagJSON {
				return emitJSON(map[string]any{"type": cixScaffoldType, "yaml": yaml})
			}
			fmt.Print(yaml)
			return nil
		},
	}
	scaffold.Flags().StringVar(&cixScaffoldType, "type", "eval", "workflow type: eval|review|test-gen|fix-vuln")
	scaffold.Flags().StringVar(&cixScaffoldOut, "out", "", "write YAML to this path instead of stdout")

	var cixRunProfile string
	var cixRunDryRun bool
	run := &cobra.Command{
		Use:   "run <suite-path>",
		Short: "Run an eval suite as a CI gate (dry-run offline: plans cases, no model calls)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			suite, err := ciauto.LoadSuite(args[0])
			if err != nil {
				return err
			}
			// No live provider is configured in this native build, so `run` is
			// always a dry-run: we plan how many cases WOULD run and exit 0
			// without invoking any model.
			profile := app.profile(cixRunProfile)
			name := suite.Name
			if name == "" {
				name = args[0]
			}
			if flagJSON {
				return emitJSON(map[string]any{
					"dry_run": true, "suite": name, "suite_path": args[0],
					"profile": profile, "cases_planned": len(suite.Cases),
				})
			}
			fmt.Printf("DRY RUN — no provider configured, no model calls made.\n")
			fmt.Printf("Suite: %s  (%s)\n", name, args[0])
			fmt.Printf("Profile: %s\n", profile)
			fmt.Printf("Would run %d case(s):\n", len(suite.Cases))
			for _, c := range suite.Cases {
				id := c.ID
				if id == "" {
					id = "(unnamed)"
				}
				fmt.Printf("  - %s\n", id)
			}
			return nil
		},
	}
	run.Flags().StringVar(&cixRunProfile, "profile", "", "profile to attribute the run to")
	run.Flags().BoolVar(&cixRunDryRun, "dry-run", true, "plan cases without calling a model (default; the only supported mode offline)")

	ev.AddCommand(scaffold, run)
	root.AddCommand(ev)
}
