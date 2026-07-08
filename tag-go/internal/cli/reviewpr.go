package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/solver"
)

// registerReviewPR wires `tag review-pr` — the PR-review agentic solver (parity
// roadmap #527). It reviews a unified diff read from --diff (or stdin) with the
// native agent loop. Fetching a live PR diff/metadata and posting comments needs
// the gh CLI + network and is reported honestly rather than faked. Defaults to
// the offline `echo` provider.
func registerReviewPR(root *cobra.Command, app *App) {
	var provider, diffFile string
	c := &cobra.Command{
		Use:     "review-pr",
		Short:   "Review a unified diff (--diff file or stdin) with the agent loop",
		GroupID: "orch",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			var diff string
			if diffFile != "" {
				b, err := os.ReadFile(diffFile)
				if err != nil {
					return fmt.Errorf("reading diff file: %w", err)
				}
				diff = string(b)
			} else {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("reading diff from stdin: %w", err)
				}
				diff = string(b)
			}
			if strings.TrimSpace(diff) == "" {
				return fmt.Errorf("no diff supplied (use --diff FILE or pipe a diff on stdin)")
			}
			db, _ := app.OpenDB()
			model := app.Cfg.String("profiles."+app.profile("")+".config.model.default", "")
			res, err := solver.Solve(context.Background(), db, prov, model, solver.Options{
				Kind: solver.KindReview,
				Task: diff,
			})
			if err != nil {
				return err
			}
			return emitSolveResult(res)
		},
	}
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().StringVar(&diffFile, "diff", "", "path to a unified diff file (default: stdin)")
	root.AddCommand(c)
}
