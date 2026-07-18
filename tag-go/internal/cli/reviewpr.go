package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/solver"
)

// registerReviewPR wires `tag review-pr` — the PR-review agentic solver (parity
// roadmap #527). It reviews a unified diff from --diff (or stdin), OR fetches a
// live PR diff with --pr <n> via `gh pr diff`. With --post it posts the review as
// a PR comment via `gh pr comment` (guarded: --post requires --pr and a working
// gh). Defaults to the offline `echo` provider.
func registerReviewPR(root *cobra.Command, app *App) {
	var provider, diffFile, repo string
	var prNum int
	var post bool
	c := &cobra.Command{
		Use:     "review-pr",
		Short:   "Review a unified diff (--diff/stdin or --pr <n> via gh) with the agent loop",
		GroupID: "orch",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			if post && prNum == 0 {
				return fmt.Errorf("--post requires --pr <n> (refusing to post a review with no target PR)")
			}
			if prNum != 0 && diffFile != "" {
				return fmt.Errorf("use either --pr or --diff, not both")
			}

			var diff string
			var fetchNote string
			switch {
			case prNum != 0:
				d, err := fetchPRDiffViaGH(prNum, repo)
				if err != nil {
					return err
				}
				diff = d
				fetchNote = fmt.Sprintf("fetched diff for PR #%d via gh.", prNum)
			case diffFile != "":
				b, err := os.ReadFile(diffFile)
				if err != nil {
					return fmt.Errorf("reading diff file: %w", err)
				}
				diff = string(b)
			default:
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("reading diff from stdin: %w", err)
				}
				diff = string(b)
			}
			if strings.TrimSpace(diff) == "" {
				return fmt.Errorf("no diff supplied (use --diff FILE, --pr N, or pipe a diff on stdin)")
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
			if fetchNote != "" {
				res.Notes = append([]string{fetchNote}, res.Notes...)
			}

			if post {
				if err := postPRReviewViaGH(prNum, repo, res.Output); err != nil {
					res.Notes = append(res.Notes, "review generated but posting failed: "+err.Error())
				} else {
					res.Notes = append(res.Notes, fmt.Sprintf("posted the review as a comment on PR #%d via gh.", prNum))
				}
			} else {
				res.Notes = append(res.Notes, "review NOT posted (dry-run); pass --post with --pr <n> to comment on the PR.")
			}
			return emitSolveResult(res)
		},
	}
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().StringVar(&diffFile, "diff", "", "path to a unified diff file (default: stdin)")
	c.Flags().IntVar(&prNum, "pr", 0, "fetch the diff for this PR number via gh")
	c.Flags().StringVar(&repo, "repo", "", "owner/repo for --pr (else inferred from cwd by gh)")
	c.Flags().BoolVar(&post, "post", false, "post the review as a PR comment via gh (requires --pr)")
	root.AddCommand(c)
}

// fetchPRDiffViaGH fetches a PR's unified diff via `gh pr diff <n>`.
func fetchPRDiffViaGH(pr int, repo string) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", fmt.Errorf("gh CLI not found on PATH: cannot fetch PR #%d; supply --diff instead or install/authenticate gh", pr)
	}
	args := []string{"pr", "diff", fmt.Sprintf("%d", pr)}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		detail := strings.TrimSpace(string(exitStderr(err)))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("gh pr diff failed (auth/network/unknown PR?): %s", truncateNote(detail, 200))
	}
	return string(out), nil
}

// postPRReviewViaGH posts a review body as a PR comment via `gh pr comment`.
func postPRReviewViaGH(pr int, repo, body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("empty review body; nothing to post")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh CLI not found on PATH")
	}
	args := []string{"pr", "comment", fmt.Sprintf("%d", pr), "--body", "Automated review (tag review-pr):\n\n" + body}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := exec.Command("gh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr comment failed: %s", truncateNote(strings.TrimSpace(string(out)), 200))
	}
	return nil
}
