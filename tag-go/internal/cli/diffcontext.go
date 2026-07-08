package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/diffcontext"
	"github.com/tag-agent/tag/internal/paths"
)

// registerDiffContext wires `diff-context` — inject a filtered git diff as agent
// context. Port of src/tag/cmd/agent_tools.py:cmd_diff_inject + diff_context.py.
// The --pr/--repo GitHub path (needs gh/API) is not ported; use the local ref/staged modes.
func registerDiffContext(root *cobra.Command, app *App) {
	var ref, workdir string
	var staged, outputOnly bool
	var contextLines, maxFiles int
	var blocked []string

	c := &cobra.Command{
		Use:     "diff-context",
		Short:   "Inject a filtered git diff as agent context",
		GroupID: "tools",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := filepath.Abs(strOr(workdir, "."))
			if err != nil {
				return err
			}
			res, err := diffcontext.Build(strOr(ref, "HEAD"), staged, contextLines, maxFiles, blocked, wd)
			if err != nil {
				if flagJSON {
					return emitJSON(map[string]any{"error": err.Error(), "files": []string{}, "content": "", "estimated_tokens": 0})
				}
				return err
			}
			if res.Warn {
				fmt.Fprintf(os.Stderr, "⚠ Warning: diff context is large (%d estimated tokens).\n", res.EstimatedTokens)
			}
			if len(res.FilesSkipped) > 0 {
				n := len(res.FilesSkipped)
				show := res.FilesSkipped
				if n > 5 {
					show = show[:5]
				}
				fmt.Fprintf(os.Stderr, "Skipped %d file(s): %v\n", n, show)
			}
			if len(res.Content) == 0 {
				if flagJSON {
					return emitJSON(res)
				}
				fmt.Println("No diff content to inject (no changed files in scope).")
				return nil
			}
			if outputOnly || flagJSON {
				if flagJSON {
					return emitJSON(res)
				}
				fmt.Println(res.Content)
				return nil
			}
			// Default: save to the runtime context dir (picked up by `submit`).
			dbPath := app.Cfg.String("runtime.db_path", "")
			ctxDir := filepath.Join(filepath.Dir(paths.RuntimeDBPath(dbPath)), "context")
			if err := os.MkdirAll(ctxDir, 0o755); err != nil {
				return err
			}
			ctxFile := filepath.Join(ctxDir, "diff_context.md")
			if err := os.WriteFile(ctxFile, []byte(res.Content), 0o644); err != nil {
				return err
			}
			fmt.Printf("Diff context: %d file(s), ~%d tokens\n", len(res.FilesIncluded), res.EstimatedTokens)
			fmt.Printf("Diff context saved to %s\n", ctxFile)
			return nil
		},
	}
	c.Flags().StringVar(&ref, "ref", "HEAD", "git ref to diff against")
	c.Flags().BoolVar(&staged, "staged", false, "diff staged changes only")
	c.Flags().IntVar(&contextLines, "context-lines", 3, "unified diff context lines")
	c.Flags().IntVar(&maxFiles, "max-files", 10, "max files to include")
	c.Flags().StringArrayVar(&blocked, "blocked", nil, "extra blocked glob patterns")
	c.Flags().BoolVar(&outputOnly, "output-only", false, "print diff content without saving")
	c.Flags().StringVar(&workdir, "workdir", ".", "working directory")
	root.AddCommand(c)
}
