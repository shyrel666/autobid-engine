//go:build !windows

package ops

import (
	"context"
	"errors"
	"os"
)

// ReadSecret 读取一行口令（非 Windows 平台不做回显控制，仅供开发）。
func ReadSecret(prompt string) (string, error) {
	os.Stderr.WriteString(prompt)
	return readLine(os.Stdin)
}

func diskFree(string) (uint64, error) { return 0, errors.New("磁盘检查目前仅支持 Windows") }
func officeInstalled(context.Context) CheckItem {
	return CheckItem{Name: "文档引擎", Message: "文档引擎需要 Windows 已登录桌面会话"}
}
