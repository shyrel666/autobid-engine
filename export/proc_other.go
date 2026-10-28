//go:build !windows

package export

import (
	"os/exec"
	"syscall"
)

// prepareTree 让子进程成为新进程组的组长，便于整组杀掉。
func prepareTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree 杀掉子进程所在的整个进程组。
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
