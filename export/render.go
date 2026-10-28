package export

// Mermaid 渲染：mermaid-cli（mmdc）在独立临时目录中运行，有超时（超时杀掉整个进程树），结束后清理临时目录。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var ErrRendererMissing = errors.New("export: mermaid-cli (mmdc) is not available")

// Renderer 把 Mermaid 源码渲染为 PNG。
type Renderer interface {
	Name() string
	RenderPNG(ctx context.Context, mermaid string) ([]byte, error)
}

// MMDC 调用 mermaid-cli。
type MMDC struct {
	Path    string
	Timeout time.Duration
	Width   int
}

func (m *MMDC) Name() string { return "mmdc:" + m.Path }

// DetectMMDC 依次查找 AUTOBID_MMDC_PATH、PATH 中的 mmdc；都没有时返回 ErrRendererMissing（附查找说明）。
func DetectMMDC() (*MMDC, error) {
	if p := strings.TrimSpace(os.Getenv("AUTOBID_MMDC_PATH")); p != "" {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return nil, fmt.Errorf("%w: AUTOBID_MMDC_PATH=%s is not a file", ErrRendererMissing, p)
		}
		return &MMDC{Path: p, Timeout: 90 * time.Second, Width: 1600}, nil
	}
	if p, err := exec.LookPath("mmdc"); err == nil {
		return &MMDC{Path: p, Timeout: 90 * time.Second, Width: 1600}, nil
	}
	return nil, fmt.Errorf("%w: AUTOBID_MMDC_PATH is not set and mmdc is not on PATH", ErrRendererMissing)
}

func (m *MMDC) RenderPNG(ctx context.Context, src string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "autobid-mmdc-*")
	if err != nil {
		return nil, err
	}
	defer removeAllRetry(dir)
	in, out := filepath.Join(dir, "chart.mmd"), filepath.Join(dir, "chart.png")
	if err := os.WriteFile(in, []byte(src), 0o600); err != nil {
		return nil, err
	}
	args := []string{"-i", in, "-o", out, "-b", "white", "-s", "2", "-w", fmt.Sprint(m.Width)}
	if runtime.GOOS != "windows" {
		cfg := filepath.Join(dir, "puppeteer.json")
		_ = os.WriteFile(cfg, []byte(`{"args":["--no-sandbox"]}`), 0o600)
		args = append(args, "-p", cfg)
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	if _, stderr, err := runWithTimeout(ctx, timeout, dir, m.Path, args...); err != nil {
		return nil, fmt.Errorf("mmdc: %v: %s", err, tail(stderr, 400))
	}
	png, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("mmdc produced no output: %w", err)
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(png)); err != nil {
		return nil, fmt.Errorf("mmdc output is not a PNG: %w", err)
	}
	return png, nil
}
