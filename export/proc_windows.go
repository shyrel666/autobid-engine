//go:build windows

package export

import (
	"os/exec"
	"strconv"
	"syscall"
)

// prepareTree 让子进程不弹出控制台窗口。
func prepareTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// killTree 杀掉进程及其全部子进程（soffice.exe 会再启动 soffice.bin，只杀父进程不够）。
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	_ = cmd.Process.Kill()
}
