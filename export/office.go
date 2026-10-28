package export

// LibreOffice headless（soffice + UNO 脚本 office_refresh.py）：每次转换使用独立临时目录、独立的 -env:UserInstallation
// profile 与私有管道，避免并发实例争用同一用户配置而互相阻塞或崩溃；有并发上限与超时（超时杀掉整个进程树），结束后清理临时目录。

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed office_refresh.py
var officeScript []byte

var ErrOfficeUnavailable = errors.New("export: LibreOffice (soffice) is not available")

// DefaultSofficeWindows 是 Windows 下最后尝试的 LibreOffice 安装位置。
const DefaultSofficeWindows = `F:\LibreOffice\program\soffice.exe`

// Office 是 LibreOffice headless 转换器。
type Office struct {
	Soffice string
	Python  string
	Version string
	Timeout time.Duration
	sem     chan struct{}
	seq     atomic.Int64
}

// OfficeOptions 配置并发与超时。
type OfficeOptions struct {
	Concurrency int
	Timeout     time.Duration
}

// FindSoffice 依次查找 AUTOBID_SOFFICE_PATH、PATH 中的 soffice、Windows 默认安装路径。
func FindSoffice() (string, error) {
	if p := strings.TrimSpace(os.Getenv("AUTOBID_SOFFICE_PATH")); p != "" {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return "", fmt.Errorf("%w: AUTOBID_SOFFICE_PATH=%s is not a file", ErrOfficeUnavailable, p)
		}
		return p, nil
	}
	for _, name := range []string{"soffice", "libreoffice"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	if runtime.GOOS == "windows" {
		if st, err := os.Stat(DefaultSofficeWindows); err == nil && !st.IsDir() {
			return DefaultSofficeWindows, nil
		}
	}
	return "", fmt.Errorf("%w: AUTOBID_SOFFICE_PATH is not set, soffice is not on PATH and %s does not exist", ErrOfficeUnavailable, DefaultSofficeWindows)
}

// DetectOffice 定位 soffice 与可导入 uno 的 Python（LibreOffice 自带的 python 优先，可用 AUTOBID_SOFFICE_PYTHON 覆盖），
// 并读取版本号。
func DetectOffice(opt OfficeOptions) (*Office, error) {
	soffice, err := FindSoffice()
	if err != nil {
		return nil, err
	}
	if abs, err := filepath.Abs(soffice); err == nil {
		soffice = abs
	}
	dir := filepath.Dir(soffice)
	py := strings.TrimSpace(os.Getenv("AUTOBID_SOFFICE_PYTHON"))
	if py == "" {
		for _, c := range []string{"python.exe", "python", "python3"} {
			if st, err := os.Stat(filepath.Join(dir, c)); err == nil && !st.IsDir() {
				py = filepath.Join(dir, c)
				break
			}
		}
	}
	if py == "" {
		if p, err := exec.LookPath("python3"); err == nil {
			py = p
		}
	}
	if py == "" {
		return nil, fmt.Errorf("%w: no Python with the uno module next to %s (set AUTOBID_SOFFICE_PYTHON)", ErrOfficeUnavailable, soffice)
	}
	if opt.Concurrency <= 0 {
		opt.Concurrency = 2
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 3 * time.Minute
	}
	o := &Office{Soffice: soffice, Python: py, Timeout: opt.Timeout, sem: make(chan struct{}, opt.Concurrency)}
	o.Version = officeVersion(dir)
	return o, nil
}

// officeVersion 从安装目录的 version.ini / versionrc 读取版本标识（不启动 soffice）。
func officeVersion(dir string) string {
	for _, name := range []string{"version.ini", "versionrc"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var updateID, build string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "UpdateID="); ok {
				if i := strings.Index(v, "_"); i >= 0 {
					v = v[i+1:]
					if j := strings.Index(v, "_"); j >= 0 {
						v = v[:j]
					}
				}
				updateID = v
			}
			if v, ok := strings.CutPrefix(line, "buildid="); ok {
				build = v
			}
		}
		f.Close()
		if updateID != "" || build != "" {
			s := "LibreOffice " + updateID
			if build != "" {
				s += " (build " + build[:min(len(build), 12)] + ")"
			}
			return strings.TrimSpace(s)
		}
	}
	return "LibreOffice (version unknown)"
}

// RefreshResult 是一次 UNO 刷新的结果。
type RefreshResult struct {
	Passes  int    `json:"passes"`
	Indexes int    `json:"indexes"`
	TOC     string `json:"toc"`
}

// Refresh 在 LibreOffice 中打开 inDocx，更新全部索引直到稳定，然后另存 outDocx 与 / 或 outPDF（为空表示不生成）。
func (o *Office) Refresh(ctx context.Context, inDocx, outDocx, outPDF string) (*RefreshResult, error) {
	select {
	case o.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-o.sem }()

	dir, err := os.MkdirTemp("", "autobid-soffice-*")
	if err != nil {
		return nil, err
	}
	defer removeAllRetry(dir)
	script := filepath.Join(dir, "office_refresh.py")
	if err := os.WriteFile(script, officeScript, 0o600); err != nil {
		return nil, err
	}
	profile := filepath.Join(dir, "profile")
	pipe := fmt.Sprintf("autobid_%d_%d_%d", os.Getpid(), o.seq.Add(1), time.Now().UnixNano()%1e6)
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	so := exec.Command(o.Soffice, "-env:UserInstallation="+fileURL(profile), "--headless", "--invisible", "--nologo", "--norestore",
		"--nodefault", "--nolockcheck", "--nofirststartwizard", "--accept=pipe,name="+pipe+";urp;")
	so.Dir = dir
	var soErr bytes.Buffer
	so.Stdout, so.Stderr = &soErr, &soErr
	prepareTree(so)
	if err := so.Start(); err != nil {
		return nil, fmt.Errorf("start soffice: %w", err)
	}
	soDone := make(chan error, 1)
	go func() { soDone <- so.Wait() }()
	defer func() {
		select {
		case <-soDone:
		case <-time.After(15 * time.Second):
			killTree(so)
			<-soDone
		}
	}()

	args := []string{script, "--pipe", pipe, "--in", inDocx, "--connect-timeout", fmt.Sprint(int(o.Timeout.Seconds()))}
	if outDocx != "" {
		args = append(args, "--out-docx", outDocx)
	}
	if outPDF != "" {
		args = append(args, "--out-pdf", outPDF)
	}
	stdout, stderr, runErr := runWithTimeout(ctx, o.Timeout, dir, o.Python, args...)
	if runErr != nil || ctx.Err() != nil {
		killTree(so)
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		RefreshResult
	}
	line := lastJSONLine(stdout)
	if line == "" || json.Unmarshal([]byte(line), &res) != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("LibreOffice refresh timed out after %s", o.Timeout)
		}
		return nil, fmt.Errorf("LibreOffice refresh failed: %v: %s %s", runErr, tail(stderr, 400), tail(soErr.String(), 200))
	}
	if !res.OK {
		return nil, fmt.Errorf("LibreOffice refresh failed: %s", res.Error)
	}
	for _, p := range []string{outDocx, outPDF} {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return nil, fmt.Errorf("LibreOffice did not write %s", filepath.Base(p))
		}
	}
	return &res.RefreshResult, nil
}
