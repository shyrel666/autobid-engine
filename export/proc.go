package export

// 子进程工具：超时杀进程树、截取输出、清理临时目录。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runWithTimeout 运行子进程；超时或 ctx 取消时杀掉整个进程树。
func runWithTimeout(ctx context.Context, timeout time.Duration, dir, name string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	prepareTree(cmd)
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return stdout.String(), stderr.String(), err
	case <-ctx.Done():
		killTree(cmd)
		<-done
		return stdout.String(), stderr.String(), fmt.Errorf("killed after timeout: %w", ctx.Err())
	}
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

// removeAllRetry 删除临时目录（Windows 上进程刚退出时文件可能仍被短暂占用）。
func removeAllRetry(dir string) {
	for i := 0; i < 20; i++ {
		if err := os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}
