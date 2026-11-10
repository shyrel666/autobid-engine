//go:build !windows

package ops

import (
	"context"
	"errors"
)

func diskFree(string) (uint64, error) { return 0, errors.New("磁盘检查目前仅支持 Windows") }
func officeInstalled(context.Context) CheckItem {
	return CheckItem{Name: "文档引擎", Message: "文档引擎需要 Windows 已登录桌面会话"}
}
