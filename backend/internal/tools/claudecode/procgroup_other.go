//go:build !unix

package claudecode

import "os/exec"

// killProcessGroup is a no-op off Unix: only claude itself is killed.
func killProcessGroup(*exec.Cmd) {}
