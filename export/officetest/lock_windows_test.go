//go:build windows

package officetest

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const holderEnv = "AUTOBID_OFFICETEST_HOLD"

func TestLockIsExclusiveUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	release, locked, err := tryLock(path)
	if err != nil || !locked {
		t.Fatalf("first lock: locked=%v err=%v", locked, err)
	}
	if _, locked, err := tryLock(path); err != nil || locked {
		t.Fatalf("second lock while held: locked=%v err=%v", locked, err)
	}
	release()
	again, locked, err := tryLock(path)
	if err != nil || !locked {
		t.Fatalf("lock after release: locked=%v err=%v", locked, err)
	}
	again()
}

// TestHelperHoldLock 只在子进程里运行：持有锁并等待被结束。
func TestHelperHoldLock(t *testing.T) {
	path := os.Getenv(holderEnv)
	if path == "" {
		t.Skip("只作为 TestLockFreedWhenHolderProcessDies 的子进程运行")
	}
	if _, locked, err := tryLock(path); err != nil || !locked {
		t.Fatalf("holder lock: locked=%v err=%v", locked, err)
	}
	os.Stdout.WriteString("HOLDING\n")
	time.Sleep(time.Minute)
}

// 持锁进程被强杀（如 go test 超时）后，排队的进程不能永远等下去。
func TestLockFreedWhenHolderProcessDies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$", "-test.v")
	cmd.Env = append(os.Environ(), holderEnv+"="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	held := make(chan bool, 1)
	go func() {
		s := bufio.NewScanner(out)
		for s.Scan() {
			if strings.Contains(s.Text(), "HOLDING") {
				held <- true
				return
			}
		}
		held <- false
	}()
	select {
	case ok := <-held:
		if !ok {
			t.Fatal("holder process exited without taking the lock")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("holder process did not take the lock")
	}
	if _, locked, err := tryLock(path); err != nil || locked {
		t.Fatalf("lock held by another process: locked=%v err=%v", locked, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		release, locked, err := tryLock(path)
		if err != nil {
			t.Fatal(err)
		}
		if locked {
			release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("lock not freed after its holder process was killed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
