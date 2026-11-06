// Package officetest 让依赖真实文档引擎（WPS / Word）的测试在本机排队执行。
//
// go test 按包并行，每个包是一个独立进程，comOffice 的进程内排队管不到别的包。两个包同时驱动真实引擎时：
//   - “调用前已存在的进程不受影响”“没有残留自动化进程”这类进程快照断言，会把对方的进程算成自己的；
//   - office_com.ps1 与 comOffice.sweep 回收“本次开始后出现的自动化进程”，会真的结束对方正在刷新的进程。
//
// 因此把用例排成一队，而不是放宽断言：快照里只剩用户自己的进程，上述断言仍按原样判定。
// 这里不得导入 export，export 包内的测试也要用它。
package officetest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	pollInterval   = 100 * time.Millisecond
	waitCeiling    = 30 * time.Minute // go test -timeout 0 时的等待上限
	deadlineMargin = 30 * time.Second // 留给失败信息，避免被 go test 的超时 panic 抢先
)

// Hold 独占本机的真实文档引擎，直到用例结束（含它之后注册的 t.Cleanup）。
// 必须在探测引擎之前调用，探测的自检同样启动真实进程。一个用例只调用一次：锁不可重入。
// 本机全部工作树共用这把锁；cmd/server 不走这把锁，真实服务运行或自检时不要跑这些用例。
func Hold(t *testing.T) {
	t.Helper()
	path, err := lockPath()
	if err != nil {
		t.Fatalf("文档引擎测试锁：%v", err)
	}
	began := time.Now()
	limit := began.Add(waitCeiling)
	if d, ok := t.Deadline(); ok && d.Add(-deadlineMargin).Before(limit) {
		limit = d.Add(-deadlineMargin)
	}
	for {
		release, locked, err := tryLock(path)
		if err != nil {
			t.Fatalf("文档引擎测试锁 %s：%v", path, err)
		}
		if locked {
			t.Cleanup(release)
			if waited := time.Since(began); waited >= time.Second {
				t.Logf("等待其它用例释放文档引擎 %s", waited.Round(time.Second))
			}
			return
		}
		if time.Now().After(limit) {
			t.Fatalf("等待文档引擎测试锁 %s 超时（已等 %s）：有其它用例正在占用，或同一用例重复调用了 Hold",
				path, time.Since(began).Round(time.Second))
		}
		time.Sleep(pollInterval)
	}
}

// lockPath 优先用用户缓存目录：临时目录会随进程的 TMP 变化，不同工作树或沙箱里的测试可能拿到不同的锁文件。
func lockPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "autobid-engine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create lock directory: %w", err)
	}
	return filepath.Join(dir, "office-test.lock"), nil
}
