package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/sandbox"
)

// registerSandbox wires `tag sandbox run` with two selectable backends
// (`--backend`, default `restricted`):
//   - restricted (Go port of src/tag/sandbox.py's `restricted` backend): runs a
//     shell command confined to a working directory with a timeout and a minimal
//     environment.
//   - docker: runs the command inside a `docker run --rm` container with hardened
//     resource/network defaults (`--memory/--cpus/--network`, `--image` required);
//     see internal/sandbox/docker.go.
//
// Both capture stdout/stderr/exit and share the same Result shape.
func registerSandbox(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "sandbox", Short: "Run commands in a restricted sandbox", GroupID: "tools"}

	var timeoutSec int
	var dir string
	var backend string
	var image string
	var memory string
	var cpus string
	var network string
	run := &cobra.Command{Use: "run <command>", Short: "Execute a command in the sandbox (restricted or docker backend)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			timeout := time.Duration(timeoutSec) * time.Second
			var res *sandbox.Result
			var err error
			switch backend {
			case "", "restricted":
				res, err = sandbox.Exec(context.Background(), sandbox.Options{
					Command: args[0],
					Dir:     dir,
					Timeout: timeout,
				})
			case "docker":
				res, err = sandbox.ExecDocker(context.Background(), sandbox.DockerOptions{
					Command: args[0],
					Image:   image,
					Dir:     dir,
					Timeout: timeout,
					Memory:  memory,
					CPUs:    cpus,
					Network: network,
				})
			default:
				return fmt.Errorf("unknown backend %q: use 'restricted' or 'docker'", backend)
			}
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
	run.Flags().StringVar(&dir, "dir", "", "working directory (default: current dir; container workdir for docker)")
	run.Flags().StringVar(&backend, "backend", "restricted", "sandbox backend: 'restricted' (host sh) or 'docker'")
	run.Flags().StringVar(&image, "image", "", "container image (required for --backend docker)")
	run.Flags().StringVar(&memory, "memory", sandbox.DefaultDockerMemory, "docker memory limit (docker backend)")
	run.Flags().StringVar(&cpus, "cpus", sandbox.DefaultDockerCPUs, "docker CPU limit (docker backend)")
	run.Flags().StringVar(&network, "network", sandbox.DefaultDockerNetwork, "docker network mode (docker backend; 'none' isolates)")

	c.AddCommand(run)
	root.AddCommand(c)
}
