package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// registerPromptSize wires `tag prompt-size`, which estimates the token count
// of a prompt supplied via --text or --file.
//
// Python parity: the CLI's `prompt-size` subcommand delegates to the hermes
// binary, but every native token estimate in the Python source uses the same
// heuristic, `max(1, len(text) // 4)` (see src/tag/diff_context.py:_estimate_tokens
// and src/tag/eval_judge.py). We mirror that heuristic exactly here — chars/4,
// floored at 1 for non-empty input. This is an approximation of tiktoken-style
// tokenization and may diverge from a live tokenizer for non-English text.
func registerPromptSize(root *cobra.Command, app *App) {
	var text, file string
	c := &cobra.Command{Use: "prompt-size", Short: "Estimate token count for a prompt", GroupID: "tools", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if text != "" && file != "" {
				return fmt.Errorf("provide only one of --text or --file")
			}
			if text == "" && file == "" {
				return fmt.Errorf("provide a prompt via --text or --file")
			}
			content := text
			if file != "" {
				b, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("could not read --file %q: %w", file, err)
				}
				content = string(b)
			}
			chars := len(content)
			tokens := estimateTokens(content)
			if flagJSON {
				return emitJSON(map[string]any{"chars": chars, "tokens": tokens})
			}
			fmt.Printf("chars:  %d\n", chars)
			fmt.Printf("tokens: %d\n", tokens)
			return nil
		}}
	c.Flags().StringVar(&text, "text", "", "prompt text to size")
	c.Flags().StringVar(&file, "file", "", "path to a file whose contents are sized")
	root.AddCommand(c)
}

// estimateTokens mirrors Python's _estimate_tokens: max(1, len(text)//4) for
// non-empty input, and 0 for empty input (len 0 -> no tokens).
func estimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	t := len(s) / 4
	if t < 1 {
		t = 1
	}
	return t
}
