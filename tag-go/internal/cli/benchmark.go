package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/benchmark"
	"github.com/tag-agent/tag/internal/llm"
)

// registerBenchmark wires `tag benchmark` — a suite runner that drives prompt
// cases through the native agent loop (Track B) and scores each by expected
// substring. Defaults to the offline `echo` provider so it is safe without keys.
// Runs are persisted to the self-ensured benchmark_runs table.
func registerBenchmark(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "benchmark", Short: "Run and inspect agent benchmark suites", GroupID: "obs"}

	var provider, suitePath, profile string
	run := &cobra.Command{Use: "run", Short: "Run a benchmark suite through the agent loop", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			suite, err := benchmark.LoadSuite(suitePath)
			if err != nil {
				return err
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			r := &benchmark.Runner{
				DB:       db.DB,
				Provider: prov,
				Model:    app.Cfg.String("profiles."+app.profile(profile)+".config.model.default", ""),
			}
			res, err := r.Run(context.Background(), suite)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(res)
			}
			fmt.Printf("Benchmark run %s — suite %q — provider %s\n", res.ID, res.Suite, res.Provider)
			fmt.Printf("%-24s %s\n", "Case", "Result")
			for _, cr := range res.Cases {
				status := "PASS"
				if !cr.Pass {
					status = "FAIL"
				}
				fmt.Printf("%-24s %s\n", truncate(cr.ID, 24), status)
			}
			fmt.Printf("\n%d/%d passed (%d failed)\n", res.Passed, res.Total, res.Failed)
			return nil
		}}
	run.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	run.Flags().StringVar(&suitePath, "suite", "", "path to a suite YAML (default: embedded suite)")
	run.Flags().StringVar(&profile, "profile", "", "profile (for model resolution)")

	var listLimit int
	list := &cobra.Command{Use: "list", Short: "List recent benchmark runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			runs, err := benchmark.List(db.DB, listLimit)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(runs)
			}
			if len(runs) == 0 {
				fmt.Println("No benchmark runs found.")
				return nil
			}
			fmt.Printf("%-14s %-20s %-10s %-16s %s\n", "ID", "Created", "Provider", "Suite", "Passed/Total")
			for _, r := range runs {
				fmt.Printf("%-14s %-20s %-10s %-16s %d/%d\n",
					r.ID, r.CreatedAt, truncate(r.Provider, 10), truncate(r.Suite, 16), r.Passed, r.Total)
			}
			return nil
		}}
	list.Flags().IntVar(&listLimit, "limit", 20, "max runs to list")

	show := &cobra.Command{Use: "show <id>", Short: "Show a benchmark run's per-case results", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			r, err := benchmark.Show(db.DB, args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(r)
			}
			fmt.Printf("Benchmark run %s — suite %q — provider %s — %s\n", r.ID, r.Suite, r.Provider, r.CreatedAt)
			fmt.Printf("%d/%d passed (%d failed)\n\n", r.Passed, r.Total, r.Failed)
			for _, cr := range r.Cases {
				status := "PASS"
				if !cr.Pass {
					status = "FAIL"
				}
				fmt.Printf("[%s] %s\n", status, cr.ID)
				fmt.Printf("      expected: %q\n", cr.Expected)
				fmt.Printf("      output:   %q\n", truncate(cr.Output, 200))
			}
			return nil
		}}

	c.AddCommand(run, list, show)
	root.AddCommand(c)
}
