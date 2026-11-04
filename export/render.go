package export

// Mermaid 渲染：mermaid-cli（mmdc）在独立临时目录中运行，有超时（超时杀掉整个进程树）与并发上限，结束后清理临时目录。
// 子进程的 TEMP / TMP 指向该临时目录，puppeteer 的浏览器配置目录随之落在里面，被杀后也一并清理。
// 浏览器使用 AUTOBID_MMDC_BROWSER 指定的本机 Chrome / Chromium（puppeteer executablePath），不在业务请求中联网下载。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
)

var ErrRendererMissing = errors.New("export: mermaid-cli (mmdc) is not available")

// ErrNotInstalled 标记外部依赖没有安装或没有配置（区别于已配置但自检失败）。
var ErrNotInstalled = errors.New("not installed")

// Renderer 把 Mermaid 源码渲染为 PNG。
type Renderer interface {
	Name() string
	RenderPNG(ctx context.Context, mermaid string) ([]byte, error)
}

// versioned 是可报告版本的渲染器（MMDC 实现）。
type versioned interface{ Version() string }

// defaultMermaidFont 优先使用 Windows 自带中文字体，保证中文标签字形稳定。
const defaultMermaidFont = `"Microsoft YaHei", "微软雅黑", SimHei, "Noto Sans CJK SC", sans-serif`

// MMDC 调用 mermaid-cli。Path 为 mmdc 可执行文件或 cli.js（.js 时经 Node 执行）。
type MMDC struct {
	Path        string
	Node        string // Path 为 .js 时使用；为空时查 PATH 中的 node
	Browser     string // puppeteer executablePath；为空时使用 puppeteer 自带浏览器
	Timeout     time.Duration
	Width       int
	FontFamily  string
	Concurrency int

	once    sync.Once
	slot    chan struct{}
	version string
}

func (m *MMDC) Name() string { return "mmdc" }

// Version 是 `mmdc --version` 的输出（探测时读取）。
func (m *MMDC) Version() string { return m.version }

// MMDCTimeout 是单次 mmdc 渲染的上限（AUTOBID_MMDC_TIMEOUT_SECONDS，默认 90 秒）。
func MMDCTimeout() time.Duration {
	return time.Duration(envInt("AUTOBID_MMDC_TIMEOUT_SECONDS", 90)) * time.Second
}

// DetectMMDC 依次查找 AUTOBID_MMDC_PATH、PATH 中的 mmdc，并读取版本；都没有时返回 ErrRendererMissing（附查找说明）。
// AUTOBID_MMDC_BROWSER 指定浏览器，AUTOBID_MMDC_TIMEOUT_SECONDS（默认 90）与 AUTOBID_MMDC_CONCURRENCY（默认 2，上限 8）限制执行。
func DetectMMDC() (*MMDC, error) {
	m := &MMDC{Timeout: MMDCTimeout(), Width: 1600,
		Concurrency: min(envInt("AUTOBID_MMDC_CONCURRENCY", 2), 8), Browser: strings.TrimSpace(os.Getenv("AUTOBID_MMDC_BROWSER"))}
	if p := strings.TrimSpace(os.Getenv("AUTOBID_MMDC_PATH")); p != "" {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return nil, fmt.Errorf("%w: AUTOBID_MMDC_PATH is not a file", ErrRendererMissing)
		}
		m.Path = p
	} else if p, err := exec.LookPath("mmdc"); err == nil {
		m.Path = p
	} else {
		return nil, fmt.Errorf("%w: %w: AUTOBID_MMDC_PATH is not set and mmdc is not on PATH", ErrRendererMissing, ErrNotInstalled)
	}
	if strings.EqualFold(filepath.Ext(m.Path), ".js") {
		node := strings.TrimSpace(os.Getenv("AUTOBID_NODE_PATH"))
		if node == "" {
			node = "node"
		}
		p, err := exec.LookPath(node)
		if err != nil {
			return nil, fmt.Errorf("%w: Node.js is required to run cli.js: %v", ErrRendererMissing, err)
		}
		m.Node = p
	}
	if m.Browser != "" {
		if st, err := os.Stat(m.Browser); err != nil || st.IsDir() {
			return nil, fmt.Errorf("%w: AUTOBID_MMDC_BROWSER is not a file", ErrRendererMissing)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name, args := m.command("--version")
	out, stderr, err := runWithTimeout(ctx, 30*time.Second, "", nil, name, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: mmdc --version failed: %v: %s", ErrRendererMissing, err, tail(stderr, 200))
	}
	m.version = strings.TrimSpace(out)
	return m, nil
}

func (m *MMDC) command(args ...string) (string, []string) {
	if m.Node != "" {
		return m.Node, append([]string{m.Path}, args...)
	}
	return m.Path, args
}

func (m *MMDC) acquire(ctx context.Context) error {
	m.once.Do(func() { m.slot = make(chan struct{}, max(m.Concurrency, 1)) })
	select {
	case m.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *MMDC) RenderPNG(ctx context.Context, src string) ([]byte, error) {
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-m.slot }()
	dir, err := os.MkdirTemp("", "autobid-mmdc-*")
	if err != nil {
		return nil, err
	}
	defer removeAllRetry(dir)
	in, out := filepath.Join(dir, "chart.mmd"), filepath.Join(dir, "chart.png")
	if err := os.WriteFile(in, []byte(src), 0o600); err != nil {
		return nil, err
	}
	pcfg := map[string]any{"headless": true}
	if m.Browser != "" {
		pcfg["executablePath"] = m.Browser
	}
	if runtime.GOOS != "windows" {
		pcfg["args"] = []string{"--no-sandbox"}
	}
	font := m.FontFamily
	if font == "" {
		font = defaultMermaidFont
	}
	pjson, _ := json.Marshal(pcfg)
	mjson, _ := json.Marshal(map[string]any{"fontFamily": font, "themeVariables": map[string]any{"fontFamily": font}})
	puppeteer, mermaidCfg := filepath.Join(dir, "puppeteer.json"), filepath.Join(dir, "mermaid.json")
	if err := os.WriteFile(puppeteer, pjson, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(mermaidCfg, mjson, 0o600); err != nil {
		return nil, err
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	width := m.Width
	if width <= 0 {
		width = 1600
	}
	name, args := m.command("-i", in, "-o", out, "-b", "white", "-s", "2", "-w", fmt.Sprint(width), "-p", puppeteer, "-c", mermaidCfg, "-q")
	env := append(os.Environ(), "TEMP="+dir, "TMP="+dir, "TMPDIR="+dir, "PUPPETEER_SKIP_DOWNLOAD=true")
	if _, stderr, err := runWithTimeout(ctx, timeout, dir, env, name, args...); err != nil {
		return nil, fmt.Errorf("mmdc: %v: %s", err, tail(stripPaths(stderr, dir), 400))
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

// stripPaths 去掉错误输出中的本次临时目录，避免宿主路径进入图表错误与状态说明。
func stripPaths(s, dir string) string {
	s = strings.ReplaceAll(s, dir, "<tmp>")
	return strings.ReplaceAll(s, filepath.ToSlash(dir), "<tmp>")
}

// RendererSelfTest 用固定的中文架构图与甘特图样本实际渲染一次，检查 PNG 可解码且尺寸合理。
func RendererSelfTest(ctx context.Context, r Renderer) error {
	samples := []struct{ kind, title, spec string }{
		{db.ChartArchitecture, "自检架构图", `{"layers":[{"name":"接入层","nodes":["统一门户"]},{"name":"业务层","nodes":["标书编审","证据核验"]}],"edges":[{"from":"统一门户","to":"标书编审","label":"调用"}]}`},
		{db.ChartGantt, "自检进度计划", `{"start":"2026-01-05","tasks":[{"id":"a","name":"需求调研","days":5},{"name":"系统实施","after":"a","days":20,"phase":"实施阶段"}]}`},
	}
	for _, s := range samples {
		src, err := BuildChartSource(s.kind, s.title, json.RawMessage(s.spec))
		if err != nil {
			return err
		}
		png, err := r.RenderPNG(ctx, src.Source)
		if err != nil {
			return fmt.Errorf("self-test %s: %w", s.kind, err)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(png))
		if err != nil {
			return fmt.Errorf("self-test %s: output is not a PNG: %w", s.kind, err)
		}
		if cfg.Width < 100 || cfg.Height < 50 {
			return fmt.Errorf("self-test %s: PNG is only %dx%d", s.kind, cfg.Width, cfg.Height)
		}
	}
	return nil
}

var versionLike = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// rendererVersion 返回渲染器自报版本（替身等不报告版本的实现返回空）。
func rendererVersion(r Renderer) string {
	if v, ok := r.(versioned); ok {
		if s := versionLike.FindString(v.Version()); s != "" {
			return s
		}
	}
	return ""
}
