package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/sandbox"
)

// registerSandbox wires `tag sandbox` — a restricted command-execution backend
// (Go port of src/tag/sandbox.py's `restricted` backend). It runs a shell
// command confined to a working directory with a timeout and a minimal
// environment, capturing stdout/stderr/exit.
func registerSandbox(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "sandbox", Short: "Run commands in a restricted sandbox", GroupID: "tools"}

	var timeoutSec int
	var dir string
	run := &cobra.Command{Use: "run <command>", Short: "Execute a command in the restricted sandbox", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := sandbox.Exec(context.Background(), sandbox.Options{
				Command: args[0],
				Dir:     dir,
				Timeout: time.Duration(timeoutSec) * time.Second,
			})
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{
					"stdout":    res.Stdout,
					"stderr":    res.Stderr,
					"exit":      res.Exit,
					"timed_out": res.TimedOut,
				})
			}
			if res.Stdout != "" {
				fmt.Print(res.Stdout)
			}
			if res.Stderr != "" {
				fmt.Fprint(cmd.ErrOrStderr(), res.Stderr)
			}
			if res.TimedOut {
				fmt.Printf("\n(sandbox: timed out after %ds, exit %d)\n", timeoutSec, res.Exit)
			} else {
				fmt.Printf("\n(sandbox: exit %d)\n", res.Exit)
			}
			return nil
		}}
	run.Flags().IntVar(&timeoutSec, "timeout", 60, "timeout in seconds (must be > 0)")
	run.Flags().StringVar(&dir, "dir", "", "working directory (default: current dir)")

	c.AddCommand(run)
	root.AddCommand(c)
}
