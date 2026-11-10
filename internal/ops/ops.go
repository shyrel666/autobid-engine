// Package ops implements the operator commands: read-only checks, backup, verify and restore.
// It never runs migrations, and restore only writes to an empty database and empty directories.
package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/buildinfo"
	"github.com/autobid/autobid-engine/toolhealth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const KeyReminder = "模型主密钥不在本备份中。请单独安全保管 AUTOBID_LLM_MASTER_KEY，或 AUTOBID_LLM_KEY_FILE 指定的文件（默认 data/llm-master.key）；丢失后无法解密已保存的模型凭据。"

type Config struct {
	DatabaseURL, PGBin string
	Dirs               map[string]string
	KeyFile            string
}

func FromEnv() Config {
	c := Config{DatabaseURL: os.Getenv("AUTOBID_DATABASE_URL"), PGBin: pgBin(), Dirs: map[string]string{}, KeyFile: os.Getenv("AUTOBID_LLM_KEY_FILE")}
	for _, k := range []string{"filework", "exports", "evidence"} {
		env := map[string]string{"filework": "AUTOBID_FILEWORK_DIR", "exports": "AUTOBID_EXPORT_DIR", "evidence": "AUTOBID_EVIDENCE_DIR"}[k]
		c.Dirs[k] = os.Getenv(env)
		if c.Dirs[k] == "" {
			c.Dirs[k] = filepath.Join("data", k)
		}
	}
	if c.KeyFile == "" {
		c.KeyFile = filepath.Join("data", "llm-master.key")
	}
	return c
}

// pgBin finds pg_dump: an explicit setting, then PATH, then the local PostgreSQL 16 install.
func pgBin() string {
	if v := os.Getenv("AUTOBID_PG_BIN"); v != "" {
		return v
	}
	if _, e := exec.LookPath("pg_dump"); e == nil {
		return ""
	}
	const local = `C:\Program Files\PostgreSQL\16\bin`
	if _, e := os.Stat(local); e == nil {
		return local
	}
	return ""
}

func ParseDatabase(dsn string) (*pgx.ConnConfig, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("未配置数据库连接")
	}
	c, e := pgx.ParseConfig(dsn)
	if e != nil || c.Database == "" {
		return nil, errors.New("数据库连接配置无效")
	}
	return c, nil
}
func Open(ctx context.Context, dsn string) (*db.DB, error) {
	c, e := ParseDatabase(dsn)
	if e != nil {
		return nil, e
	}
	d := &db.DB{DB: stdlib.OpenDB(*c)}
	if e = d.PingContext(ctx); e != nil {
		d.Close()
		return nil, errors.New("数据库不可达，请检查连接配置与数据库服务")
	}
	return d, nil
}

type CheckItem struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Required items make check exit non-zero: the machine cannot be stopped, backed up or
	// upgraded safely. Other items only affect the matching capability.
	Required  bool    `json:"required"`
	Message   string  `json:"message"`
	Count     *int64  `json:"count,omitempty"`
	FreeBytes *uint64 `json:"free_bytes,omitempty"`
}
type CheckReport struct {
	Version        buildinfo.Version `json:"version"`
	Schema         string            `json:"schema_checksum"`
	ExpectedSchema string            `json:"expected_schema_checksum"`
	CanStop        bool              `json:"can_stop"`
	Items          []CheckItem       `json:"items"`
}

// ActiveJobs counts work that a stop would interrupt: queued or running jobs and running tasks.
// Paused tasks are counted separately under "paused_tasks": pausing cancels their file jobs and
// requeues the rest, so they hold no work in flight and survive a restart unchanged.
func ActiveJobs(ctx context.Context, d *db.DB) (map[string]int64, error) {
	counts := map[string]int64{}
	for _, table := range []string{"export_jobs", "chart_render_jobs", "fixed_file_jobs", "section_write_jobs", "knowledge_indexes", "tasks"} {
		var exists bool
		if e := d.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); e != nil {
			return nil, errors.New("无法检查在途作业")
		}
		if !exists {
			counts[table] = 0
			continue
		}
		states := "('queued','running')"
		if table == "knowledge_indexes" {
			states = "('queued','building')"
		}
		if table == "tasks" {
			states = "('running')"
			var paused int64
			if e := d.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE status='paused'`).Scan(&paused); e != nil {
				return nil, errors.New("无法读取暂停的任务计数")
			}
			counts["paused_tasks"] = paused
		}
		var count int64
		if e := d.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE status IN `+states).Scan(&count); e != nil {
			return nil, errors.New("无法读取在途作业计数")
		}
		counts[table] = count
	}
	return counts, nil
}
func Check(ctx context.Context, c Config) (CheckReport, error) {
	r := CheckReport{Version: buildinfo.Current(), ExpectedSchema: db.SchemaChecksum(), CanStop: true}
	d, e := Open(ctx, c.DatabaseURL)
	if e != nil {
		return r, e
	}
	defer d.Close()
	// All database statements in this command are reads; it never calls Migrate.
	r.Items = append(r.Items, CheckItem{Name: "数据库", OK: true, Message: "数据库可达；检查不执行迁移"})
	r.Schema, e = d.AppliedSchemaChecksum(ctx)
	if e != nil {
		return r, errors.New("无法读取数据库版本")
	}
	msg := "数据库版本一致"
	if r.Schema != r.ExpectedSchema {
		msg = "数据库待迁移或版本不同；请先停机备份，再使用匹配的程序"
	}
	r.Items = append(r.Items, CheckItem{Name: "数据库版本", OK: r.Schema == r.ExpectedSchema, Required: true, Message: msg})
	counts, e := ActiveJobs(ctx, d)
	if e != nil {
		return r, e
	}
	labels := map[string]string{"export_jobs": "导出作业", "chart_render_jobs": "图表作业", "fixed_file_jobs": "解析作业", "section_write_jobs": "章节写作作业", "knowledge_indexes": "索引构建", "tasks": "运行中的任务"}
	for _, k := range []string{"export_jobs", "chart_render_jobs", "fixed_file_jobs", "section_write_jobs", "knowledge_indexes", "tasks"} {
		n := counts[k]
		if n > 0 {
			r.CanStop = false
		}
		msg := fmt.Sprintf("在途 %d 项", n)
		if k == "tasks" && n > 0 {
			msg += "；请在页面暂停这些项目，或等它们完成"
		}
		r.Items = append(r.Items, CheckItem{Name: labels[k], OK: n == 0, Required: true, Count: &n, Message: msg})
	}
	paused := counts["paused_tasks"]
	r.Items = append(r.Items, CheckItem{Name: "暂停的任务", OK: true, Count: &paused, Message: fmt.Sprintf("暂停 %d 个；暂停时没有在途工作，重启后仍为暂停", paused)})
	dirLabels := map[string]string{"filework": "招标原件目录", "exports": "导出产物目录", "evidence": "证明材料目录"}
	for _, k := range []string{"filework", "exports", "evidence"} {
		p := c.Dirs[k]
		item := CheckItem{Name: dirLabels[k], Required: true, Message: "数据目录不可写"}
		s, e := os.Stat(p)
		if errors.Is(e, os.ErrNotExist) {
			item.OK, item.Message = true, "目录尚未创建，首次写入时由服务创建"
		} else if e == nil && s.IsDir() {
			if f, e := os.CreateTemp(p, ".ops-check-"); e == nil {
				n := f.Name()
				f.Close()
				item.OK = os.Remove(n) == nil
				item.Message = "数据目录存在且可写"
			}
			if n, e := diskFree(p); e == nil {
				item.FreeBytes = &n
			} else {
				item.OK = false
				item.Message = "无法读取磁盘剩余空间"
			}
		}
		r.Items = append(r.Items, item)
	}
	for _, name := range []string{"pg_dump", "pg_restore", "node", "go"} {
		program := name
		if strings.HasPrefix(name, "pg_") && c.PGBin != "" {
			program = filepath.Join(c.PGBin, name)
		}
		arg := "--version"
		if name == "go" {
			arg = "version"
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		b, e := exec.CommandContext(probe, program, arg).Output()
		cancel()
		msg := "未找到程序或版本不可读"
		if e == nil {
			msg = strings.TrimSpace(string(b))
		}
		r.Items = append(r.Items, CheckItem{Name: name, OK: e == nil, Message: msg})
	}
	// No COM object is instantiated here. Registry lookup only, on Windows.
	r.Items = append(r.Items, officeInstalled(ctx))
	for _, v := range []string{"AUTOBID_MMDC_PATH", "AUTOBID_MMDC_BROWSER"} {
		p := os.Getenv(v)
		_, e := os.Stat(p)
		r.Items = append(r.Items, CheckItem{Name: v, OK: p != "" && e == nil, Message: map[bool]string{true: "已找到配置的程序；实际能力以服务自检为准", false: "未配置或程序不存在"}[p != "" && e == nil]})
	}
	for _, v := range []string{"AUTOBID_OCR_URL", "AUTOBID_EMBEDDING_URL", "AUTOBID_RERANKER_URL"} {
		ok := os.Getenv(v) != ""
		r.Items = append(r.Items, CheckItem{Name: v, OK: ok, Message: map[bool]string{true: "已配置；本检查不调用模型或识别服务，实际能力以服务自检为准", false: "未配置；对应能力不就绪"}[ok]})
	}
	return r, nil
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Manifest struct {
	Format    int               `json:"format"`
	CreatedAt time.Time         `json:"created_at"`
	Version   buildinfo.Version `json:"version"`
	Schema    string            `json:"schema_checksum"`
	Dump      File              `json:"dump"`
	Files     []File            `json:"files"`
	KeyNotice string            `json:"key_notice"`
}

func HashFile(root, rel string) (File, error) {
	f, e := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if e != nil {
		return File{}, errors.New("无法读取备份文件")
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, f)
	if e != nil {
		return File{}, errors.New("读取备份文件失败")
	}
	return File{Path: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}
func emptyDir(p string) error {
	s, e := os.Lstat(p)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 {
		return errors.New("目标必须是空目录，不能是文件或链接")
	}
	entries, e := os.ReadDir(p)
	if e != nil || len(entries) > 0 {
		return errors.New("目标目录非空，拒绝覆盖")
	}
	return nil
}
func within(parent, child string) bool {
	a, _ := filepath.Abs(parent)
	b, _ := filepath.Abs(child)
	r, e := filepath.Rel(a, b)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
func copyFile(src, dst string) error {
	in, e := os.Open(src)
	if e != nil {
		return errors.New("读取源文件失败")
	}
	defer in.Close()
	if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
		return errors.New("创建文件目录失败")
	}
	out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return errors.New("创建备份文件失败")
	}
	_, e = io.Copy(out, in)
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e != nil || ce != nil {
		return errors.New("写入备份文件失败")
	}
	return nil
}
func RunPG(ctx context.Context, c Config, dsn, name string, args ...string) error {
	cfg, e := ParseDatabase(dsn)
	if e != nil {
		return e
	}
	program := name
	if c.PGBin != "" {
		program = filepath.Join(c.PGBin, name)
	}
	if name == "pg_restore" {
		args = append([]string{"--dbname=" + cfg.Database, "--exit-on-error", "--single-transaction", "--no-owner", "--no-privileges"}, args...)
	}
	cmd := exec.CommandContext(ctx, program, args...)
	ssl := "disable"
	if cfg.TLSConfig != nil {
		ssl = "require"
	}
	// Drop inherited PG* settings instead of blanking them: pg_dump reads an empty PGSERVICE as a service name.
	env := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PG") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "PGHOST="+cfg.Host, fmt.Sprintf("PGPORT=%d", cfg.Port), "PGUSER="+cfg.User, "PGPASSWORD="+cfg.Password, "PGDATABASE="+cfg.Database, "PGSSLMODE="+ssl)
	// PostgreSQL stderr may carry paths or connection details, so only a sanitized tail is shown.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if e = cmd.Run(); e != nil {
		detail := strings.TrimSpace(toolhealth.Sanitize(stderr.String()))
		if len(detail) > 300 {
			detail = detail[len(detail)-300:]
		}
		return fmt.Errorf("%s 执行失败：%s（未输出连接信息）", name, detail)
	}
	return nil
}
func Backup(ctx context.Context, c Config, out string) (Manifest, error) {
	m := Manifest{Format: 1, CreatedAt: time.Now().UTC(), Version: buildinfo.Current(), KeyNotice: KeyReminder, Files: []File{}}
	if e := emptyDir(out); e != nil {
		return m, e
	}
	for _, k := range []string{"filework", "exports", "evidence"} {
		p := c.Dirs[k]
		if p == "" || within(p, out) || within(out, p) {
			return m, errors.New("备份目录与数据目录必须相互独立")
		}
		s, e := os.Lstat(p)
		if errors.Is(e, os.ErrNotExist) {
			continue // 这类资料还没有写入过，备份里就是空的。
		}
		if e != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 {
			return m, errors.New("数据目录不可读或是链接，无法备份")
		}
	}
	d, e := Open(ctx, c.DatabaseURL)
	if e != nil {
		return m, e
	}
	defer d.Close()
	m.Schema, e = d.AppliedSchemaChecksum(ctx)
	if e != nil || m.Schema == "" {
		return m, errors.New("缺少已应用数据库版本，拒绝备份")
	}
	if e = os.MkdirAll(out, 0700); e != nil {
		return m, errors.New("创建备份目录失败")
	}
	// Database FIRST, then files: the filesystem snapshot includes all referenced objects.
	if e = RunPG(ctx, c, c.DatabaseURL, "pg_dump", "--format=custom", "--file="+filepath.Join(out, "database.dump")); e != nil {
		return m, e
	}
	m.Dump, e = HashFile(out, "database.dump")
	if e != nil {
		return m, e
	}
	key, _ := filepath.Abs(c.KeyFile)
	keyInfo, _ := os.Stat(key)
	for _, k := range []string{"filework", "exports", "evidence"} {
		if e = os.MkdirAll(filepath.Join(out, k), 0700); e != nil {
			return m, errors.New("创建备份目录失败")
		}
		if _, e = os.Stat(c.Dirs[k]); errors.Is(e, os.ErrNotExist) {
			continue
		}
		e = filepath.WalkDir(c.Dirs[k], func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return errors.New("数据目录读取失败")
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("数据目录含链接，拒绝备份")
			}
			if entry.IsDir() {
				return nil
			}
			info, e := entry.Info()
			if e != nil || !info.Mode().IsRegular() {
				return errors.New("数据目录含非普通文件")
			}
			a, _ := filepath.Abs(p)
			if strings.EqualFold(a, key) || (keyInfo != nil && os.SameFile(info, keyInfo)) {
				return nil
			}
			rel, e := filepath.Rel(c.Dirs[k], p)
			if e != nil {
				return e
			}
			rel = filepath.ToSlash(filepath.Join(k, rel))
			if e = copyFile(p, filepath.Join(out, filepath.FromSlash(rel))); e != nil {
				return e
			}
			f, e := HashFile(out, rel)
			if e == nil {
				m.Files = append(m.Files, f)
			}
			return e
		})
		if e != nil {
			return m, e
		}
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	b, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		return m, e
	}
	if e = os.WriteFile(filepath.Join(out, "manifest.json"), append(b, '\n'), 0600); e != nil {
		return m, errors.New("保存备份清单失败")
	}
	_, e = Verify(out)
	return m, e
}
func safeRelative(s string) bool {
	return s != "" && !strings.ContainsAny(s, `\:`) && !strings.HasPrefix(s, "/") && filepath.ToSlash(filepath.Clean(s)) == s && s != ".." && !strings.HasPrefix(s, "../")
}
func Verify(dir string) (Manifest, error) {
	var m Manifest
	b, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if e != nil {
		return m, errors.New("无法读取备份清单")
	}
	if e = json.Unmarshal(b, &m); e != nil || m.Format != 1 || m.Schema == "" || m.Dump.Path != "database.dump" || m.KeyNotice == "" {
		return m, errors.New("备份清单无效")
	}
	expected := map[string]File{"database.dump": m.Dump}
	for _, f := range m.Files {
		if !safeRelative(f.Path) || !(strings.HasPrefix(f.Path, "filework/") || strings.HasPrefix(f.Path, "exports/") || strings.HasPrefix(f.Path, "evidence/")) {
			return m, errors.New("备份含非法路径")
		}
		if _, ok := expected[f.Path]; ok {
			return m, errors.New("备份清单路径重复")
		}
		expected[f.Path] = f
	}
	seen := map[string]bool{}
	e = filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return errors.New("备份目录不可读")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("备份含链接，拒绝读取")
		}
		if entry.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if rel == "manifest.json" {
			return nil
		}
		f, ok := expected[rel]
		if !ok {
			return errors.New("备份含清单外文件")
		}
		if len(f.SHA256) != 64 || f.Size < 0 {
			return errors.New("文件校验信息无效")
		}
		actual, e := HashFile(dir, rel)
		if e != nil {
			return e
		}
		if actual != f {
			return errors.New("备份文件哈希或大小不一致")
		}
		seen[rel] = true
		return nil
	})
	if e != nil {
		return m, e
	}
	if len(seen) != len(expected) {
		return m, errors.New("备份文件缺失")
	}
	return m, nil
}
func Restore(ctx context.Context, c Config, dir, dsn, root string) error {
	m, e := Verify(dir)
	if e != nil {
		return e
	}
	if e = emptyDir(root); e != nil {
		return e
	}
	if within(dir, root) || within(root, dir) {
		return errors.New("恢复目录与备份目录必须相互独立")
	}
	d, e := Open(ctx, dsn)
	if e != nil {
		return e
	}
	defer d.Close()
	var count int
	e = d.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%') + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema')) + (SELECT count(*) FROM pg_namespace WHERE nspname NOT IN ('public','pg_catalog','information_schema') AND nspname NOT LIKE 'pg_toast%' AND nspname NOT LIKE 'pg_temp%')`).Scan(&count)
	if e != nil {
		return errors.New("无法确认目标数据库为空")
	}
	if count != 0 {
		return errors.New("目标数据库非空，拒绝覆盖；请新建空库")
	}
	// Allocate the directory before modifying the database. A failed restore remains
	// visibly incomplete and cannot be silently retried over nonempty destinations.
	for _, k := range []string{"filework", "exports", "evidence"} {
		if e = os.MkdirAll(filepath.Join(root, k), 0700); e != nil {
			return errors.New("无法创建恢复目录")
		}
	}
	for _, f := range m.Files {
		if e = copyFile(filepath.Join(dir, filepath.FromSlash(f.Path)), filepath.Join(root, filepath.FromSlash(f.Path))); e != nil {
			return e
		}
		got, e := HashFile(root, f.Path)
		if e != nil || got != f {
			return errors.New("恢复文件校验失败")
		}
	}
	if e = RunPG(ctx, c, dsn, "pg_restore", filepath.Join(dir, "database.dump")); e != nil {
		return e
	}
	schema, e := d.AppliedSchemaChecksum(ctx)
	if e != nil || schema != m.Schema {
		return errors.New("恢复后的数据库版本与备份不一致")
	}
	return nil
}
