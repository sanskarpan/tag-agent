package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func registerShell(root *cobra.Command, app *App) {
	sh := &cobra.Command{
		Use:     "shell",
		Short:   "Stub REPL: reads commands from STDIN, dispatch-only (no model called)",
		GroupID: "system",
		RunE: func(cmd *cobra.Command, args []string) error {
			return shRun(cmd.OutOrStdout(), os.Stdin)
		},
	}
	root.AddCommand(sh)
}

// shRun reads lines from r and prints a small acknowledgement for each.
// It exits on EOF or an "exit"/"quit" line. It never calls a model — this is
// a non-interactive-friendly stub of src/tag/shell_mode.py.
func shRun(w io.Writer, r io.Reader) error {
	fmt.Fprintln(w, "TAG shell (stub) — type commands, 'exit' or 'quit' to leave.")
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if lower == "exit" || lower == "quit" || lower == "/exit" || lower == "/quit" {
			fmt.Fprintln(w, "Goodbye.")
			return nil
		}
		fmt.Fprintf(w, "[stub] would dispatch: %s\n", line)
	}
	return scanner.Err()
}
