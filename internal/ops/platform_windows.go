//go:build windows

package ops

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// ReadSecret 在控制台读取一行且不回显（关闭 ENABLE_ECHO_INPUT）；标准输入不是控制台时按普通行读取。
func ReadSecret(prompt string) (string, error) {
	os.Stderr.WriteString(prompt)
	h := syscall.Handle(os.Stdin.Fd())
	k32 := syscall.NewLazyDLL("kernel32.dll")
	get, set := k32.NewProc("GetConsoleMode"), k32.NewProc("SetConsoleMode")
	var mode uint32
	if r, _, _ := get.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r != 0 {
		const enableEcho = 0x0004
		set.Call(uintptr(h), uintptr(mode&^enableEcho))
		defer func() {
			set.Call(uintptr(h), uintptr(mode))
			os.Stderr.WriteString("\n")
		}()
	}
	return readLine(os.Stdin)
}

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
