//go:build windows

package officetest

import (
	"errors"
	"syscall"
)

// errSharingViolation 是 ERROR_SHARING_VIOLATION，syscall 包没有导出它。
const errSharingViolation = syscall.Errno(32)

// tryLock 以不共享方式打开锁文件：另一个句柄打开期间返回 locked=false。
// 句柄随进程退出关闭，用例超时或被强杀后不会留下陈旧的锁。
func tryLock(path string) (release func(), locked bool, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, errSharingViolation) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() { _ = syscall.CloseHandle(h) }, true, nil
}
