package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/solver"
)

// registerIssueSolve wires `tag issue-solve <issue>` — the issue-solving agentic
// solver (parity roadmap #527). The issue body is supplied inline, via --file, or
// as a GitHub reference (`#123`, `owner/repo#123`, or a GitHub URL). References
// are fetched via the `gh` CLI (`gh issue view`); if gh is missing/unauthenticated
// the raw reference is passed through and the solver records an honest note rather
// than faking a fetch. Defaults to the offline `echo` provider.
func registerIssueSolve(root *cobra.Command, app *App) {
	var provider, file, repo string
	c := &cobra.Command{
		Use:     "issue-solve <issue>",
		Short:   "Solve an issue (inline body, --file, or a GitHub reference) with the agent loop",
		GroupID: "orch",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			var task string
			var fetchNote string
			switch {
			case file != "":
				b, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("reading issue file: %w", err)
				}
				task = string(b)
			case len(args) == 1:
				task = args[0]
				// If the argument is a GitHub issue reference, try to fetch its
				// title+body via gh. On any failure we keep the raw reference so the
				// solver emits its honest "could not fetch" note (never fake).
				if body, note, ok := fetchIssueViaGH(args[0], repo); ok {
					task = body
					fetchNote = note
				} else if note != "" {
					fetchNote = note
				}
			default:
				return fmt.Errorf("provide an issue body/reference as an argument or via --file")
			}
			if strings.TrimSpace(task) == "" {
				return fmt.Errorf("issue text is empty")
			}
			db, _ := app.OpenDB()
			model := app.Cfg.String("profiles."+app.profile("")+".config.model.default", "")
			res, err := solver.Solve(context.Background(), db, prov, model, solver.Options{
				Kind: solver.KindIssue,
				Task: task,
			})
			if err != nil {
				return err
			}
			if fetchNote != "" {
				res.Notes = append([]string{fetchNote}, res.Notes...)
			}
			return emitSolveResult(res)
		},
	}
	c.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	c.Flags().StringVar(&file, "file", "", "read the issue body from a file")
	c.Flags().StringVar(&repo, "repo", "", "owner/repo for a bare #123 reference (else inferred from cwd)")
	root.AddCommand(c)
}

var (
	// #123
	reBareIssue = regexp.MustCompile(`^#(\d+)$`)
	// owner/repo#123
	reRepoIssue = regexp.MustCompile(`^([\w.-]+/[\w.-]+)#(\d+)$`)
	// https://github.com/owner/repo/issues/123
	reURLIssue = regexp.MustCompile(`^https?://github\.com/([\w.-]+/[\w.-]+)/issues/(\d+)`)
)

// parseIssueRef extracts (repo, number) from a GitHub issue reference. repo may
// be empty for a bare `#123` (then the caller's --repo or gh's cwd default is
// used). ok is false when s is not an issue reference at all.
func parseIssueRef(s string) (repo, number string, ok bool) {
	s = strings.TrimSpace(s)
	if m := reURLIssue.FindStringSubmatch(s); m != nil {
		return m[1], m[2], true
	}
	if m := reRepoIssue.FindStringSubmatch(s); m != nil {
		return m[1], m[2], true
	}
	if m := reBareIssue.FindStringSubmatch(s); m != nil {
		return "", m[1], true
	}
	return "", "", false
}

// fetchIssueViaGH fetches an issue's title+body via the gh CLI. It returns the
// combined body, a note describing what happened, and ok=true only when the
// fetch succeeded. On any failure (not a ref, gh missing, unauthenticated, gh
// error) it returns ok=false with a note the caller surfaces (or empty when the
// input was not a reference at all).
func fetchIssueViaGH(arg, repoFlag string) (body, note string, ok bool) {
	repo, number, isRef := parseIssueRef(arg)
	if !isRef {
		return "", "", false // inline body: nothing to fetch, no note
	}
	if repoFlag != "" {
		repo = repoFlag
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return "", "gh CLI not found on PATH: could not fetch the issue; pass the body inline or via --file. Install and authenticate gh (`gh auth login`) to fetch references.", false
	}
	ghArgs := []string{"issue", "view", number, "--json", "title,body"}
	if repo != "" {
		ghArgs = append(ghArgs, "--repo", repo)
	}
	out, err := exec.Command("gh", ghArgs...).Output()
	if err != nil {
		detail := strings.TrimSpace(string(exitStderr(err)))
		if detail == "" {
			detail = err.Error()
		}
		return "", "gh could not fetch the issue (missing auth or network?): " + truncateNote(detail, 200) + " — falling back to the raw reference (no fake fetch).", false
	}
	var parsed struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if jerr := json.Unmarshal(out, &parsed); jerr != nil {
		return "", "gh returned output that could not be parsed as issue JSON; using the raw reference.", false
	}
	ref := arg
	if repo != "" {
		ref = repo + "#" + number
	}
	combined := "# " + strings.TrimSpace(parsed.Title) + "\n\n" + parsed.Body
	return combined, "fetched issue " + ref + " via gh (title+body).", true
}

// exitStderr returns the captured stderr from an *exec.ExitError, if any.
func exitStderr(err error) []byte {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.Stderr
	}
	return nil
}

func truncateNote(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
