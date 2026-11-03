//go:build windows

package export

// WPS / Word 的 COM 自动化引擎：每次刷新在独立临时目录写出内嵌脚本 office_com.ps1，由 Windows PowerShell 5.1（STA）
// 打开文档、刷新目录、另存 DOCX / 导出 PDF，结果以 UTF-8 JSON 文件返回。
// COM 启动的 WINWORD / wps 进程不是脚本的子进程：脚本结束前回收本次新增的自动化进程；脚本超时或异常退出时，
// 再按“命令行含 /Automation 或 -Embedding、且创建于本次开始之后”清理一次。用户自己打开的文档与 WPS 常驻插件进程
// 没有这些参数，不会被结束。同一引擎同一时间只处理一个文档。

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

//go:embed office_com.ps1
var officeScript []byte

type comEngine struct {
	name, progID, image, label string
}

var comEngines = map[string]comEngine{
	EngineWPS:  {name: EngineWPS, progID: "KWPS.Application", image: "wps.exe", label: "WPS"},
	EngineWord: {name: EngineWord, progID: "Word.Application", image: "WINWORD.EXE", label: "Word"},
}

type comOffice struct {
	eng     comEngine
	ps      string
	timeout time.Duration
	slot    chan struct{}

	mu      sync.Mutex
	version string
}

func newOffice(name string, timeout time.Duration) (officeEngine, error) {
	eng, ok := comEngines[name]
	if !ok {
		return nil, fmt.Errorf("unknown document engine %q", name)
	}
	if !progIDRegistered(eng.progID) {
		return nil, fmt.Errorf("%w: %s (COM class %s is not registered)", ErrNotInstalled, eng.label, eng.progID)
	}
	ps := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if st, err := os.Stat(ps); err != nil || st.IsDir() {
		p, err := exec.LookPath("powershell.exe")
		if err != nil {
			return nil, fmt.Errorf("Windows PowerShell is not available: %w", err)
		}
		ps = p
	}
	return &comOffice{eng: eng, ps: ps, timeout: timeout, slot: make(chan struct{}, 1)}, nil
}

// progIDRegistered 检查 COM 类是否已注册（HKEY_CLASSES_ROOT\<ProgID>\CLSID）。
func progIDRegistered(progID string) bool {
	key, err := syscall.UTF16PtrFromString(progID + `\CLSID`)
	if err != nil {
		return false
	}
	var h syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_CLASSES_ROOT, key, 0, syscall.KEY_READ, &h) != nil {
		return false
	}
	syscall.RegCloseKey(h)
	return true
}

func (o *comOffice) Name() string { return o.eng.name }

// setTimeout 只在引擎交给调用方之前调用（自检结束后改为作业超时）。
func (o *comOffice) setTimeout(d time.Duration) { o.timeout = d }

func (o *comOffice) Version() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.version != "" {
		return o.version
	}
	return o.eng.label
}

// Refresh 打开 inDocx，更新全部目录直到稳定，然后另存 outDocx 与 / 或 outPDF（为空表示不生成）。
func (o *comOffice) Refresh(ctx context.Context, inDocx, outDocx, outPDF string) (*RefreshResult, error) {
	select {
	case o.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-o.slot }()

	dir, err := os.MkdirTemp("", "autobid-office-*")
	if err != nil {
		return nil, err
	}
	defer removeAllRetry(dir)
	script := filepath.Join(dir, "office_com.ps1")
	if err := os.WriteFile(script, officeScript, 0o600); err != nil {
		return nil, err
	}
	result := filepath.Join(dir, "result.json")
	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script,
		"-Prog", o.eng.progID, "-Image", o.eng.image, "-In", inDocx, "-Result", result}
	if outDocx != "" {
		args = append(args, "-OutDocx", outDocx)
	}
	if outPDF != "" {
		args = append(args, "-OutPdf", outPDF)
	}
	started := time.Now()
	_, stderr, runErr := runWithTimeout(ctx, o.timeout, dir, nil, o.ps, args...)
	raw, readErr := os.ReadFile(result)
	if runErr != nil || readErr != nil {
		// 脚本没有走到自己的清理步骤：结束本次开始后启动的自动化进程。
		o.sweep(started)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s refresh cancelled: %w", o.eng.label, ctx.Err())
		}
		if runErr == nil {
			runErr = readErr
		}
		return nil, fmt.Errorf("%s refresh failed: %v: %s", o.eng.label, runErr, tail(stderr, 400))
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		RefreshResult
	}
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &res); err != nil {
		return nil, fmt.Errorf("%s refresh returned an unreadable result: %w", o.eng.label, err)
	}
	if res.Version != "" {
		res.Version = o.eng.label + " " + res.Version
		o.mu.Lock()
		o.version = res.Version
		o.mu.Unlock()
	}
	if !res.OK {
		return nil, fmt.Errorf("%s refresh failed: %s", o.eng.label, res.Error)
	}
	for _, p := range []string{outDocx, outPDF} {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return nil, fmt.Errorf("%s did not write %s", o.eng.label, filepath.Base(p))
		}
	}
	return &res.RefreshResult, nil
}

// sweep 结束 since 之后创建的本引擎自动化进程（命令行含 /Automation 或 -Embedding）。
func (o *comOffice) sweep(since time.Time) {
	cmd := fmt.Sprintf(`$since = [datetime]::ParseExact('%s', 's', $null); `+
		`Get-CimInstance Win32_Process -Filter "Name='%s'" | `+
		`Where-Object { [string]$_.CommandLine -match '/Automation|-Embedding' -and $_.CreationDate -ge $since } | `+
		`ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`,
		since.Add(-2*time.Second).Format("2006-01-02T15:04:05"), o.eng.image)
	_, _, _ = runWithTimeout(context.Background(), 30*time.Second, "", nil, o.ps, "-NoProfile", "-NonInteractive", "-Command", cmd)
}
