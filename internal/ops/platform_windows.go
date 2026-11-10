//go:build windows

package ops

import (
	"context"
	"os/exec"
	"syscall"
	"unsafe"
)

func diskFree(path string) (uint64, error) {
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return 0, e
	}
	var available, total, free uint64
	r, _, e := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW").Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&available)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return 0, e
	}
	return available, nil
}
func officeInstalled(ctx context.Context) CheckItem {
	for _, id := range []string{"KWPS.Application", "Word.Application"} {
		if exec.CommandContext(ctx, "reg.exe", "query", `HKCR\`+id+`\CLSID`, "/ve").Run() == nil {
			return CheckItem{Name: "文档引擎", OK: true, Message: id + " 已注册；未启动文档程序，实际能力以服务自检为准"}
		}
	}
	return CheckItem{Name: "文档引擎", OK: false, Message: "未找到 WPS 或 Word 的注册信息"}
}
