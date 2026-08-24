package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/docs"
)

// registerDoc wires `tag doc` — document ingestion (PRD-133).
//
// The command exists separately from the read_document tool so the capability
// is inspectable without an agent loop: `tag doc check` answers "is this
// installed and would it read my file" in one line, which is the question an
// operator has when read_file refuses their PDF.
func registerDoc(root *cobra.Command, app *App) {
	c := &cobra.Command{
		Use:     "doc",
		Short:   "Read documents (PDF) as text",
		GroupID: "orch",
	}

	var maxBytes int
	var skipVerify bool

	read := &cobra.Command{
		Use:   "read <file>",
		Short: "Extract a document as markdown",
		Long: "Extract a document as markdown.\n\n" +
			"Requires the pdf-inspector engine. Exit codes: 0 = the whole document was read; " +
			strconv.Itoa(exitFindings) + " = it was read only in part (pages needing OCR, or " +
			"truncated output) — the text is still printed; 1 = the run failed; 2 = usage error " +
			"or the engine is not installed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, ok := docs.Available(); !ok {
				return jsonErrorMaybe(usageErrorf("pdf-inspector is not installed — %s", docs.InstallHint))
			}
			doc, err := docs.Extract(cmd.Context(), args[0], docs.Options{
				MaxBytes:   maxBytes,
				SkipVerify: skipVerify,
			})
			if err != nil {
				// A missing file or a directory is the operator naming the wrong
				// thing — a usage error (exit 2), matching this command's --help and
				// the fix-vuln sibling. A genuine engine failure stays exit 1.
				if errors.Is(err, docs.ErrBadInput) {
					return jsonErrorMaybe(usageErr{err})
				}
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				if err := emitJSON(doc); err != nil {
					return err
				}
			} else {
				// No separate title header: the extracted markdown already opens
				// with the document title as an H1, and printing both produced it
				// twice.
				fmt.Fprintln(cmd.OutOrStdout(), doc.Markdown)
				// Caveats go to stderr so stdout stays a usable document — the
				// obvious use of this command is a redirect into a file.
				for _, n := range doc.Notes {
					fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n)
				}
			}
			// A partial read is "ran fine, and what you got is not everything".
			// Same contract as the rest of the family: reporting success for a
			// document with unreadable pages is how a caller ends up acting on
			// half a spec.
			if !doc.Complete {
				return exitCodeErr{code: exitFindings}
			}
			return nil
		},
	}
	read.Flags().IntVar(&maxBytes, "max-bytes", 0, "cap the extracted text (0 = default 1 MiB)")
	read.Flags().BoolVar(&skipVerify, "skip-verify", false,
		"skip the per-page check that confirms each page produced text")

	check := &cobra.Command{
		Use:   "check [file]",
		Short: "Report whether document support is available",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, ok := docs.Available()
			out := map[string]any{"available": ok, "engine": path}
			if !ok {
				out["hint"] = docs.InstallHint
			}
			// For a file arg, `supported` must reflect a file that actually
			// exists AND has a supported type — reporting `supported: true` for a
			// nonexistent path reads as a direct contradiction of 'NOT available'
			// (#763).
			exists := false
			if len(args) == 1 {
				_, statErr := os.Stat(args[0])
				exists = statErr == nil
				out["file"] = args[0]
				out["supported"] = exists && docs.Supported(args[0])
				if !exists {
					out["file_error"] = "file not found"
				}
			}
			if flagJSON {
				return emitJSON(out)
			}
			if !ok {
				fmt.Fprintf(cmd.OutOrStdout(), "document support: NOT available\n  %s\n", docs.InstallHint)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "document support: available (%s)\n", path)
			}
			if len(args) == 1 {
				switch {
				case !exists:
					fmt.Fprintf(cmd.OutOrStdout(), "%s: not found\n", args[0])
				case docs.Supported(args[0]):
					fmt.Fprintf(cmd.OutOrStdout(), "%s: supported\n", args[0])
				default:
					fmt.Fprintf(cmd.OutOrStdout(), "%s: not a supported document type\n", args[0])
				}
			}
			// Absence is information, not failure: a caller scripting around
			// this needs to branch on it, not catch it.
			return nil
		},
	}

	c.AddCommand(read, check)
	root.AddCommand(c)
}
