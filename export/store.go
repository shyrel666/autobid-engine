// Package export 是 Phase 6B 的图表生成与锁定版本套版导出模块：
//   - 图表：按类型把结构化规格确定性地生成 Mermaid 源码（架构图 / 甘特图 / 组织架构图）或结构化 JSON（设备选型表），
//     Mermaid 经 mermaid-cli（mmdc）渲染为 PNG；mmdc 不可用时图表标记为 unrendered，导出时插入源码占位框；
//   - 导出：套用 .docx 模板（内置默认模板或上传模板）自行生成 WordprocessingML（archive/zip + encoding/xml），
//     包括多级标题、TOC 域、页眉页脚与页码、Markdown 正文、嵌入图表与《点对点应答及偏离表》，宽表格自适应 / 横向分节；
//   - LibreOffice headless（UNO 脚本）刷新全部索引后另存 .docx 与 .pdf，使目录页码在成稿中已经是实际页码；
//   - Runner 从 export_jobs 领取任务（围栏令牌 + 租约），产物按租户 / 任务隔离存放在导出目录。
package export

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Store 是导出目录（AUTOBID_EXPORT_DIR）。布局：
//
//	<root>/<tenant>/charts/task-<id>/<sha256>.png        图表渲染产物（内容寻址，重绘不覆盖旧产物）
//	<root>/<tenant>/templates/<sha256>.docx              上传的模板
//	<root>/<tenant>/tasks/task-<id>/job-<id>/t<token>/   每次执行（围栏令牌）一个目录，成功后清理其他令牌目录
type Store struct {
	Root string
}

var reSafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func safeSegment(s string) string {
	s = reSafe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "._")
	if s == "" {
		return "default"
	}
	return s
}

// Abs 把相对路径（数据库里保存的形式）解析为绝对路径，并拒绝越出导出目录的路径。
func (s *Store) Abs(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) || strings.Contains(rel, ":") ||
		strings.Contains(rel, "..") {
		return "", fmt.Errorf("export: invalid artifact path %q", rel)
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	if !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("export: artifact path %q escapes the export directory", rel)
	}
	return p, nil
}

func (s *Store) tenantRel(tenant string, parts ...string) string {
	return filepath.ToSlash(filepath.Join(append([]string{safeSegment(tenant)}, parts...)...))
}

func (s *Store) chartRel(tenant string, taskID int64, sum string) string {
	return s.tenantRel(tenant, "charts", "task-"+strconv.FormatInt(taskID, 10), sum+".png")
}

func (s *Store) templateRel(tenant, sum string) string {
	return s.tenantRel(tenant, "templates", sum+".docx")
}

func (s *Store) jobRel(tenant string, taskID, jobID int64) string {
	return s.tenantRel(tenant, "tasks", "task-"+strconv.FormatInt(taskID, 10), "job-"+strconv.FormatInt(jobID, 10))
}

// WriteFile 原子写入（临时文件 + rename），返回 SHA-256。
func (s *Store) WriteFile(rel string, data []byte) (string, error) {
	p, err := s.Abs(rel)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(p, data); err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

// ReadFile 读取文件并校验 SHA-256（wantHash 为空时不校验）。
func (s *Store) ReadFile(rel, wantHash string) ([]byte, error) {
	p, err := s.Abs(rel)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if wantHash != "" && sha256Hex(data) != wantHash {
		return nil, fmt.Errorf("export: %s content hash mismatch", rel)
	}
	return data, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
