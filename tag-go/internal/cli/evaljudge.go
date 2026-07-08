package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/evaljudge"
	"github.com/tag-agent/tag/internal/llm"
)

// registerEvalJudge wires the LLM-as-judge evaluator: eval-judge run/list/show
// (parity roadmap #527 bucket B, port of src/tag/eval_judge.py). Defaults to the
// offline echo provider so it is exercisable without API keys.
func registerEvalJudge(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "eval-judge", Short: "LLM-as-judge answer scoring", GroupID: "obs"}

	var provider, model, profile, question, answer, reference string
	var threshold float64
	run := &cobra.Command{Use: "run", Short: "Judge a candidate answer against a question", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if question == "" || answer == "" {
				return usageErrorf("--question and --answer are required")
			}
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if err := evaljudge.EnsureSchema(db.DB); err != nil {
				return err
			}
			m := model
			if m == "" {
				m = app.Cfg.String("profiles."+app.profile(profile)+".config.model.default", "")
			}
			j, err := evaljudge.Judge(context.Background(), db.DB, prov, m, question, answer, reference, threshold)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(j)
			}
			verdict := "FAIL"
			if j.Passed {
				verdict = "PASS"
			}
			fmt.Printf("Judgment %s — provider %s — score %.2f (threshold %.2f) → %s\n",
				j.ID, j.Provider, j.Score, j.Threshold, verdict)
			if j.Reasoning != "" {
				fmt.Printf("  %s\n", j.Reasoning)
			}
			return nil
		}}
	run.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	run.Flags().StringVar(&model, "model", "", "judge model (default: profile config.model.default)")
	run.Flags().StringVar(&profile, "profile", "", "profile (for model resolution)")
	run.Flags().StringVar(&question, "question", "", "the question/task being evaluated")
	run.Flags().StringVar(&answer, "answer", "", "the candidate answer to score")
	run.Flags().StringVar(&reference, "reference", "", "optional reference answer / rubric")
	run.Flags().Float64Var(&threshold, "threshold", 0, "pass threshold 0..1 (default 0.7)")

	var limit int
	list := &cobra.Command{Use: "list", Short: "List past judgments", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if err := evaljudge.EnsureSchema(db.DB); err != nil {
				return err
			}
			js, err := evaljudge.List(db.DB, limit)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(js)
			}
			if len(js) == 0 {
				fmt.Println("No judgments recorded.")
				return nil
			}
			fmt.Printf("%-14s %-20s %-10s %-6s %s\n", "ID", "Created", "Provider", "Score", "Passed")
			for _, j := range js {
				fmt.Printf("%-14s %-20s %-10s %-6.2f %v\n", j.ID, j.CreatedAt, truncate(j.Provider, 10), j.Score, j.Passed)
			}
			return nil
		}}
	list.Flags().IntVar(&limit, "limit", 20, "max judgments")

	show := &cobra.Command{Use: "show <id>", Short: "Show a judgment", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if err := evaljudge.EnsureSchema(db.DB); err != nil {
				return err
			}
			j, err := evaljudge.Show(db.DB, args[0])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(j)
			}
			fmt.Printf("Judgment %s (%s)\n  provider: %s  model: %s\n  score: %.2f  threshold: %.2f  passed: %v\n  question: %s\n  answer: %s\n  reasoning: %s\n",
				j.ID, j.CreatedAt, j.Provider, j.Model, j.Score, j.Threshold, j.Passed, j.Question, j.Answer, j.Reasoning)
			return nil
		}}

	c.AddCommand(run, list, show)
	root.AddCommand(c)
}
