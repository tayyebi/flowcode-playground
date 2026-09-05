//go:build unix

package engine

import (
	"os/exec"
	"syscall"
)

// configureSandbox puts the child in a fresh process group. That is what makes
// killProcessGroup below able to take down descendants along with the leader.
func configureSandbox(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the child's entire process group.
//
// Negating the pid addresses the group. Setpgid above guarantees the group id
// equals the child's pid, so this can never reach the server's own group — but
// the Process nil check matters, because Cancel can fire before the fork
// completes and killing group 0 would signal every process we own.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
