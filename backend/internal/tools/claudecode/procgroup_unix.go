//go:build unix

package claudecode

import (
	"os/exec"
	"syscall"
)

// killProcessGroup runs cmd in its own process group, so a timeout also stops
// the subprocesses Claude Code started.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
