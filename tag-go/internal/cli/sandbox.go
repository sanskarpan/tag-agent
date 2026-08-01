package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/sandbox"
)

// EXIT-CODE CONVENTION for `tag sandbox run`: the process exits with the
// sandboxed command's own exit status, so the shell sees what the command saw.
// In particular the documented security codes now reach the process: 127 when
// the sandbox failed closed (no sandbox-exec, or a run dir too broad to
// confine) and 124 on timeout. Previously RunE always returned nil, so
// `tag sandbox run ... && next-step` ran next-step even when the sandbox had
// refused to isolate. Output (including --json) is unchanged; only the process
// status is new.
//
// sandboxExitErr maps a Result.Exit onto that process exit status. Zero means
// success. A negative value (child killed by a signal, ExitCode() == -1) is
// normalised to 1 rather than wrapping to 255 through os.Exit.
func sandboxExitErr(code int) error {
	switch {
	case code == 0:
		return nil
	case code < 0 || code > 255:
		return exitCodeErr{code: 1}
	default:
		return exitCodeErr{code: code}
	}
}

// registerSandbox wires `tag sandbox run` with two selectable backends
// (`--backend`, default `restricted`):
//   - restricted (Go port of src/tag/sandbox.py's `restricted` backend): runs a
//     shell command under real OS-level confinement -- `sandbox-exec` with an
//     SBPL profile on macOS, Landlock + rlimits (+ a network namespace when the
//     kernel permits) on Linux -- confined to a working directory with a timeout
//     and a minimal environment. Platforms with no confinement primitive fail
//     closed instead of running the command unprotected. The confinement that
//     was actually applied is reported back as `isolation`.
//   - docker: runs the command inside a `docker run --rm` container with hardened
//     resource/network defaults (`--memory/--cpus/--network`, `--image` required);
//     see internal/sandbox/docker.go.
//
// Both capture stdout/stderr/exit and share the same Result shape, and both
// propagate the command's exit status to the process (see sandboxExitErr).
func registerSandbox(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "sandbox", Short: "Run commands in an OS-isolated sandbox", GroupID: "tools"}

	var timeoutSec int
	var dir string
	var backend string
	var image string
	var memory string
	var cpus string
	var network string
	var allowUnconfined bool
	var egress egressFlags
	var egressImage string
	run := &cobra.Command{Use: "run <command>", Short: "Execute a command in the sandbox (restricted or docker backend)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			timeout := time.Duration(timeoutSec) * time.Second
			// A malformed or self-contradictory egress policy is a usage error
			// (exit 2), and it is resolved BEFORE anything is executed: a
			// firewall the user got wrong must stop the run, not be dropped.
			policy, err := egress.policy()
			if err != nil {
				return usageErrorf("%v", err)
			}
			var res *sandbox.Result
			switch backend {
			case "", "restricted":
				res, err = sandbox.Exec(context.Background(), sandbox.Options{
					Command:         args[0],
					Dir:             dir,
					Timeout:         timeout,
					AllowUnconfined: allowUnconfined,
					Egress:          policy,
				})
			case "docker":
				res, err = sandbox.ExecDocker(context.Background(), sandbox.DockerOptions{
					Command:           args[0],
					Image:             image,
					Dir:               dir,
					Timeout:           timeout,
					Memory:            memory,
					CPUs:              cpus,
					Network:           network,
					NetworkExplicit:   cmd.Flags().Changed("network"),
					Egress:            policy,
					EgressHelperImage: egressImage,
				})
			default:
				return fmt.Errorf("unknown backend %q: use 'restricted' or 'docker'", backend)
			}
			if err != nil {
				return err
			}
			if flagJSON {
				if err := emitJSON(map[string]any{
					"stdout":    res.Stdout,
					"stderr":    res.Stderr,
					"exit":      res.Exit,
					"timed_out": res.TimedOut,
					"isolation": res.Isolation,
				}); err != nil {
					return err
				}
				return sandboxExitErr(res.Exit)
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
			if res.Isolation != "" {
				fmt.Printf("(isolation: %s)\n", res.Isolation)
			}
			return sandboxExitErr(res.Exit)
		}}
	run.Flags().IntVar(&timeoutSec, "timeout", 60, "timeout in seconds (must be > 0)")
	run.Flags().StringVar(&dir, "dir", "", "working directory (default: current dir; container workdir for docker)")
	run.Flags().StringVar(&backend, "backend", "restricted",
		"sandbox backend: 'restricted' (macOS: sandbox-exec profile - no network, no /etc or $HOME reads, "+
			"run dir writable; Linux: Landlock filesystem allow-list + rlimits, network blocked only when the "+
			"kernel allows a user namespace or Landlock ABI>=4 - see the reported 'isolation' line; a kernel "+
			"without Landlock fails closed unless --allow-unconfined; other OSes: "+
			"unsupported, fails closed) or 'docker'")
	run.Flags().BoolVar(&allowUnconfined, "allow-unconfined", false,
		"DANGEROUS (Linux only): run even when the kernel cannot confine the filesystem (no Landlock). "+
			"You get rlimits and a network namespace but NO filesystem isolation - every file you can read "+
			"or write on the host is reachable from inside. Without this flag such a kernel fails closed "+
			"with exit 127; prefer --backend docker")
	run.Flags().StringVar(&image, "image", "", "container image (required for --backend docker)")
	run.Flags().StringVar(&memory, "memory", sandbox.DefaultDockerMemory, "docker memory limit (docker backend)")
	run.Flags().StringVar(&cpus, "cpus", sandbox.DefaultDockerCPUs, "docker CPU limit (docker backend)")
	run.Flags().StringVar(&network, "network", sandbox.DefaultDockerNetwork, "docker network mode (docker backend; 'none' isolates)")
	egress.bind(run)
	run.Flags().StringVar(&egressImage, "egress-helper-image", sandbox.DefaultEgressHelperImage,
		"image for the network-namespace helper that installs egress rules (docker backend; needs a shell and `ip`)")

	c.AddCommand(run)
	registerSandboxFirewall(c)
	root.AddCommand(c)
}
