//go:build windows

package export

// 本机运行组件的故障演练（S1）：在 mmdc 的 Chrome、WPS / Word 自动化进程确实启动后取消调用，
// 检查本次启动的进程全部结束、临时目录被清理，且调用前已存在的用户进程（浏览器主进程、WPS / Word）不受影响。
// 依赖真实组件：未配置 mmdc 或文档引擎时跳过。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

type winProc struct {
	PID     int
	Name    string
	Created time.Time
	Cmd     string
}

// listProcs 列出指定映像名的进程（含命令行与创建时间）。
func listProcs(t *testing.T, names ...string) []winProc {
	t.Helper()
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}
	script := `[Console]::OutputEncoding = [Text.Encoding]::UTF8; Get-CimInstance Win32_Process | Where-Object { @(` + strings.Join(quoted, ",") +
		`) -contains $_.Name } | ForEach-Object { "{0}` + "`t" + `{1}` + "`t" + `{2}` + "`t" + `{3}" -f $_.ProcessId, $_.Name, $_.CreationDate.ToString('o'), $_.CommandLine }`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	var ps []winProc
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		created, _ := time.Parse(time.RFC3339Nano, f[2])
		ps = append(ps, winProc{PID: pid, Name: f[1], Created: created, Cmd: f[3]})
	}
	return ps
}

func alive(ps []winProc, pid int) bool {
	for _, p := range ps {
		if p.PID == pid {
			return true
		}
	}
	return false
}

// waitFor 轮询直到 pred 返回非空结果或超时。
func waitFor(t *testing.T, d time.Duration, pred func() []winProc) []winProc {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := pred(); len(got) > 0 {
			return got
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil
}

var userDataDir = regexp.MustCompile(`--user-data-dir="?([^"]+?autobid-mmdc-[^\\/"]+)`)

func TestMMDCCancelLeavesNoProcesses(t *testing.T) {
	m, err := DetectMMDC()
	if err != nil {
		t.Skipf("依赖缺失：mermaid-cli 未配置：%v", err)
	}
	// 用户自己的浏览器主进程（不是本次 mmdc 启动的）必须不受影响。
	var userBrowsers []int
	for _, p := range listProcs(t, "chrome.exe", "msedge.exe") {
		if !strings.Contains(p.Cmd, "--type=") && !strings.Contains(p.Cmd, "autobid-mmdc-") {
			userBrowsers = append(userBrowsers, p.PID)
		}
	}
	var src strings.Builder
	src.WriteString("flowchart LR\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&src, "  n%d[节点%d] --> n%d[节点%d]\n", i, i, i+1, i+1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.RenderPNG(ctx, src.String())
		done <- err
	}()
	ours := waitFor(t, 30*time.Second, func() []winProc {
		var got []winProc
		for _, p := range listProcs(t, "chrome.exe") {
			if strings.Contains(p.Cmd, "autobid-mmdc-") {
				got = append(got, p)
			}
		}
		return got
	})
	if len(ours) == 0 {
		cancel()
		t.Fatalf("mmdc 的浏览器进程没有出现（渲染结果：%v）", <-done)
	}
	var tmpDir string
	for _, p := range ours {
		if mm := userDataDir.FindStringSubmatch(p.Cmd); mm != nil {
			tmpDir = mm[1]
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "killed") {
			t.Fatalf("cancelled render returned %v, want a killed error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled render did not return")
	}
	left := waitFor(t, 5*time.Second, func() []winProc {
		var got []winProc
		for _, p := range listProcs(t, "chrome.exe", "node.exe") {
			if strings.Contains(p.Cmd, "autobid-mmdc-") {
				got = append(got, p)
			}
		}
		if len(got) == 0 {
			return []winProc{{}} // 已清空
		}
		return nil
	})
	if len(left) != 1 || left[0].PID != 0 {
		t.Fatalf("mmdc 残留进程：%+v", listProcs(t, "chrome.exe", "node.exe"))
	}
	after := listProcs(t, "chrome.exe", "msedge.exe")
	for _, pid := range userBrowsers {
		if !alive(after, pid) {
			t.Errorf("用户浏览器进程 %d 被结束", pid)
		}
	}
	if tmpDir != "" {
		if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
			t.Errorf("临时目录未清理：%v", err)
		}
	}
	t.Logf("killed %d mmdc browser processes; %d user browser processes untouched", len(ours), len(userBrowsers))
}

func TestOfficeCancelLeavesNoProcesses(t *testing.T) {
	o := requireOffice(t)
	eng := comEngines[o.Name()]
	before := listProcs(t, eng.image)
	tpl, _ := DefaultTemplate()
	res, err := Build(tpl, sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.docx")
	if err := os.WriteFile(in, res.Data, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := o.Refresh(ctx, in, filepath.Join(dir, "out.docx"), filepath.Join(dir, "out.pdf"))
		done <- err
	}()
	automation := func(ps []winProc) []winProc {
		var got []winProc
		for _, p := range ps {
			if (strings.Contains(p.Cmd, "/Automation") || strings.Contains(p.Cmd, "-Embedding")) && !p.Created.Before(start.Add(-2*time.Second)) {
				got = append(got, p)
			}
		}
		return got
	}
	ours := waitFor(t, 60*time.Second, func() []winProc { return automation(listProcs(t, eng.image)) })
	if len(ours) == 0 {
		cancel()
		t.Fatalf("%s 自动化进程没有出现（刷新结果：%v）", eng.label, <-done)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Skip("刷新在取消前已完成，无法演练中途取消")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("cancelled refresh did not return")
	}
	if left := automation(listProcs(t, eng.image)); len(left) > 0 {
		time.Sleep(3 * time.Second)
		if left = automation(listProcs(t, eng.image)); len(left) > 0 {
			t.Fatalf("%s 残留自动化进程：%+v", eng.label, left)
		}
	}
	after := listProcs(t, eng.image)
	for _, p := range before {
		if !alive(after, p.PID) {
			t.Errorf("调用前已存在的 %s 进程 %d 被结束（%s）", eng.label, p.PID, p.Cmd)
		}
	}
	t.Logf("killed %d %s automation processes; %d pre-existing processes untouched", len(ours), eng.label, len(before))
}
