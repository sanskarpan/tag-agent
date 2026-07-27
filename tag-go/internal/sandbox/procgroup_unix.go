//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// SetProcGroup puts the child in its own process group so the whole tree it
// spawns can be signalled as a unit. Without it, killing the direct child (e.g.
// `sandbox-exec`) leaves any backgrounded grandchild running -- and holding the
// stdout/stderr pipe, which makes cmd.Wait block forever.
//
// plan.SysProcAttr is never mutated: it belongs to a shared isolationPlan and
// may be reused by a retry with plan.Alt.
func SetProcGroup(cmd *exec.Cmd, base *syscall.SysProcAttr) {
	var attr syscall.SysProcAttr
	if base != nil {
		attr = *base
	}
	attr.Setpgid = true
	cmd.SysProcAttr = &attr
}

// KillProcessGroup SIGKILLs the child's entire process group. It is called both
// on timeout (via cmd.Cancel) and after Wait, so a command that backgrounded
// something and exited cleanly does not leak an orphan onto the host.
func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative pid == "the process group led by pid" (Setpgid made the child the
	// leader). Fall back to the single process if the group is already gone.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
