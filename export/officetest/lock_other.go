//go:build !windows

package officetest

// tryLock 在其它平台没有真实文档引擎可争用，直接成功。
func tryLock(string) (release func(), locked bool, err error) {
	return func() {}, true, nil
}
