package claudecode

import (
	"os/exec"
	"syscall"
)

// killProcessGroup runs cmd in its own process group, so a timeout also stops
// its subprocesses; Pdeathsig stops claude if the runner itself dies.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
