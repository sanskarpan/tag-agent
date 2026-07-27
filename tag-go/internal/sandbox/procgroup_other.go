//go:build !unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// setProcGroup / killProcessGroup have no portable equivalent outside unix.
// buildIsolation fails closed on those platforms, so these are never reached at
// runtime; they exist only so the package still compiles everywhere.
func setProcGroup(cmd *exec.Cmd, base *syscall.SysProcAttr) { cmd.SysProcAttr = base }

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
