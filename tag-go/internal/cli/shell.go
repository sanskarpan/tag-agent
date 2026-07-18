package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

// registerShell wires `tag shell` — a real REPL over the native agent loop
// (Track B, Go port of src/tag/shell_mode.py). Each non-empty input line is run
// through the agent loop exactly like `tag run`: the response is printed and the
// loop continues until EOF or an exit/quit line. It defaults to the offline
// `echo` provider (no keys, no network) and is non-interactive friendly (reads
// piped STDIN).
func registerShell(root *cobra.Command, app *App) {
	var provider, system, profile string
	sh := &cobra.Command{
		Use:     "shell",
		Short:   "Interactive REPL over the native agent loop (echo default; --provider for real)",
		GroupID: "system",
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, ok := llm.Registry[provider]
			if !ok {
				return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
			}
			model := app.Cfg.String("profiles."+app.profile(profile)+".config.model.default", "")
			return shRun(cmd.OutOrStdout(), os.Stdin, &agent.Loop{Provider: prov}, model, system)
		},
	}
	sh.Flags().StringVar(&provider, "provider", "echo", "llm provider (echo = offline)")
	sh.Flags().StringVar(&system, "system", "", "system prompt")
	sh.Flags().StringVar(&profile, "profile", "", "profile")
	root.AddCommand(sh)
}

// shRun reads lines from r and runs each through the agent loop, printing the
// response. It exits on EOF or an "exit"/"quit" line. Real model calls happen
// only when the loop's provider is a real adapter; the default echo provider
// keeps it offline-safe.
func shRun(w io.Writer, r io.Reader, loop *agent.Loop, model, system string) error {
	fmt.Fprintf(w, "TAG shell (%s) — type a prompt, 'exit' or 'quit' to leave.\n", loop.Provider.Name())
	scanner := bufio.NewScanner(r)
	// Allow long pasted lines (default bufio cap is 64KB).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
		res, err := loop.Run(context.Background(), line, agent.Options{Model: model, System: system})
		if err != nil {
			fmt.Fprintf(w, "error: %v\n", err)
			continue
		}
		fmt.Fprintln(w, res.FinalText)
	}
	return scanner.Err()
}
