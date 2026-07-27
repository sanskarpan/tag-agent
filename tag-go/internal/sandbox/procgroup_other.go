//go:build !unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// SetProcGroup / KillProcessGroup have no portable equivalent outside unix.
// buildIsolation fails closed on those platforms, so these are never reached at
// runtime; they exist only so the package still compiles everywhere.
func SetProcGroup(cmd *exec.Cmd, base *syscall.SysProcAttr) { cmd.SysProcAttr = base }

func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
