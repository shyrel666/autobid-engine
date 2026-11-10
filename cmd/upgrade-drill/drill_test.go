package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export/officetest"
	"github.com/autobid/autobid-engine/internal/ops"
	"github.com/autobid/autobid-engine/toolhealth"
	"github.com/jackc/pgx/v5"
)

const baseline = "8b48a4b"

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail any    `json:"detail,omitempty"`
}
type rehearsal struct {
	t                                                               *testing.T
	ctx                                                             context.Context
	root, tmp, out, oldBin, newBin, opsBin, token, tokenFile, model string
	admin                                                           *db.DB
	cfg                                                             *pgx.ConnConfig
	created                                                         []string
	checks                                                          []checkResult
	block                                                           atomic.Bool
	entered                                                         chan struct{}
	release                                                         chan struct{}
}
type process struct {
	cmd      *exec.Cmd
	base     string
	done     chan error
	stopOnce sync.Once
	log      *safeLog
}
type safeLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *safeLog) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *safeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return toolhealth.Sanitize(l.b.String())
}
func (p *process) stop() { p.stopOnce.Do(func() { _ = p.cmd.Process.Kill(); <-p.done }) }
func TestUpgradeDrill(t *testing.T) {
	if os.Getenv("AUTOBID_S7_DRILL") != "1" {
		t.Skip("显式设置 AUTOBID_S7_DRILL=1 才执行真实服务、数据库与文档引擎演练")
	}
	officetest.Hold(t)
	root, e := filepath.Abs(filepath.Join("..", ".."))
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Minute)
	defer cancel()
	r := &rehearsal{t: t, ctx: ctx, root: root, entered: make(chan struct{}, 1), release: make(chan struct{})}
	r.tmp, e = os.MkdirTemp(filepath.Join(root, ".s7-tmp"), "upgrade-")
	r.must(e)
	r.out = filepath.Join(root, "data", "s7-checks", "upgrade", filepath.Base(r.tmp))
	r.must(os.MkdirAll(r.out, 0700))
	defer func() {
		r.write("report.json", map[string]any{"baseline": baseline, "at": time.Now().UTC(), "checks": r.checks, "passed": !t.Failed(), "model": "替身模型，非真实模型；只用于知识索引协议", "identity": "X-Autobid-User 仅为审计声明，非可信身份", "limits": "合成历史数据；未做真实断网演练；不是 V/P 验收"})
	}()
	r.cfg, e = ops.ParseDatabase(os.Getenv("AUTOBID_TEST_DATABASE_URL"))
	r.must(e)
	r.assert(!strings.EqualFold(r.cfg.Database, "autobid"), "演练不得连接名为 autobid 的库")
	r.admin, e = ops.Open(ctx, os.Getenv("AUTOBID_TEST_DATABASE_URL"))
	r.must(e)
	defer r.admin.Close()
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		for _, name := range r.created {
			_, e := r.admin.ExecContext(cleanup, `DROP DATABASE `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
			if e != nil {
				t.Errorf("清理本次临时库失败：%s", name)
			} else {
				t.Log("已删除本次临时库", name)
			}
		}
	}()
	b := make([]byte, 32)
	_, e = rand.Read(b)
	r.must(e)
	r.token = hex.EncodeToString(b)
	r.tokenFile = filepath.Join(r.tmp, "api-token")
	r.must(os.WriteFile(r.tokenFile, []byte(r.token), 0600))
	defer os.Remove(r.tokenFile)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if r.block.Load() {
			select {
			case r.entered <- struct{}{}:
			default:
			}
			select {
			case <-r.release:
			case <-q.Context().Done():
				return
			}
		}
		var in struct {
			Input []string `json:"input"`
		}
		if json.NewDecoder(q.Body).Decode(&in) != nil {
			http.Error(w, "invalid model request", 400)
			return
		}
		data := []map[string]any{}
		for i := range in.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float64{1, 0.5, 0.25}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "s7-scripted-embedding", "usage": map[string]int{"total_tokens": len(in.Input)}})
	}))
	defer model.Close()
	defer close(r.release)
	r.model = model.URL
	r.build()
	// Measure the dangerous path even if a later history fixture is blocked.
	r.pathB()
	r.upgrade()
}
func (r *rehearsal) must(e error) {
	r.t.Helper()
	if e != nil {
		r.checks = append(r.checks, checkResult{Name: "blocked", Status: "FAIL", Detail: toolhealth.Sanitize(e.Error())})
		r.t.Fatal(toolhealth.Sanitize(e.Error()))
	}
}
func (r *rehearsal) assert(ok bool, msg string) {
	r.t.Helper()
	if !ok {
		r.must(fmt.Errorf("%s", msg))
	}
}
func (r *rehearsal) pass(name string, detail any) {
	r.checks = append(r.checks, checkResult{Name: name, Status: "PASS", Detail: detail})
	r.t.Log("PASS", name)
}
func (r *rehearsal) write(name string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	if e == nil {
		e = os.WriteFile(filepath.Join(r.out, name), append(b, '\n'), 0600)
	}
	if e != nil {
		r.t.Errorf("保存演练证据失败")
	}
}
func (r *rehearsal) create(suffix string) string {
	name := fmt.Sprintf("s7_%d_%s", time.Now().UnixNano(), suffix)
	_, e := r.admin.ExecContext(r.ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()+` TEMPLATE template0`)
	r.must(e)
	r.created = append(r.created, name)
	u := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(r.cfg.Host, fmt.Sprint(r.cfg.Port)), Path: "/" + name, User: url.UserPassword(r.cfg.User, r.cfg.Password)}
	q := url.Values{}
	mode := "disable"
	if r.cfg.TLSConfig != nil {
		mode = "require"
	}
	q.Set("sslmode", mode)
	u.RawQuery = q.Encode()
	return u.String()
}
func (r *rehearsal) build() {
	archive := filepath.Join(r.tmp, "baseline.tar")
	c := exec.CommandContext(r.ctx, "git", "archive", "--format=tar", "--output="+archive, baseline)
	c.Dir = r.root
	r.must(c.Run())
	src := filepath.Join(r.tmp, "baseline")
	r.must(os.MkdirAll(src, 0700))
	f, e := os.Open(archive)
	r.must(e)
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		r.must(e)
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue
		}
		p := filepath.Clean(filepath.FromSlash(h.Name))
		r.assert(filepath.IsLocal(p), "基线归档含越界路径")
		dst := filepath.Join(src, p)
		if h.Typeflag == tar.TypeDir {
			r.must(os.MkdirAll(dst, 0700))
			continue
		}
		r.assert(h.Typeflag == tar.TypeReg, "基线归档含非普通文件")
		r.must(os.MkdirAll(filepath.Dir(dst), 0700))
		o, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		r.must(e)
		_, e = io.Copy(o, tr)
		ce := o.Close()
		r.must(e)
		r.must(ce)
	}
	r.oldBin = filepath.Join(r.tmp, "old-server.exe")
	r.newBin = filepath.Join(r.tmp, "new-server.exe")
	r.opsBin = filepath.Join(r.tmp, "autobid-ops.exe")
	for _, b := range []struct{ dir, out, pkg string }{{src, r.oldBin, "./cmd/server"}, {r.root, r.newBin, "./cmd/server"}, {r.root, r.opsBin, "./cmd/autobid-ops"}} {
		c := exec.CommandContext(r.ctx, "go", "build", "-o", b.out, b.pkg)
		c.Dir = b.dir
		c.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
		out, e := c.CombinedOutput()
		if e != nil {
			r.must(fmt.Errorf("构建失败：%s", toolhealth.Sanitize(string(out))))
		}
	}
	r.pass("git archive 构建旧版与当前真实程序", baseline)
}
func (r *rehearsal) env(dsn, data string) []string {
	env := []string{}
	for _, s := range os.Environ() {
		k, _, _ := strings.Cut(s, "=")
		if strings.HasPrefix(k, "AUTOBID_") || strings.HasPrefix(k, "E2E_") {
			continue
		}
		env = append(env, s)
	}
	env = append(env, "AUTOBID_DATABASE_URL="+dsn, "AUTOBID_API_TOKEN="+r.token, "AUTOBID_FILE_PROCESSING_ENABLED=1", "AUTOBID_FILEWORK_DIR="+filepath.Join(data, "filework"), "AUTOBID_EXPORT_DIR="+filepath.Join(data, "exports"), "AUTOBID_EVIDENCE_DIR="+filepath.Join(data, "evidence"), "AUTOBID_LLM_KEY_FILE="+filepath.Join(r.tmp, "llm-master.key"), "AUTOBID_WEB_DIR="+filepath.Join(r.root, "web", "dist"), "AUTOBID_OFFICE_ENGINE=auto", "AUTOBID_AGENT_EXPORT_ENABLED=0", "AUTOBID_EMBEDDING_URL="+r.model, "AUTOBID_EMBEDDING_MODEL=s7-scripted-embedding", "AUTOBID_EMBEDDING_REVISION=s7-v1", "AUTOBID_EMBEDDING_DIMENSION=3", "AUTOBID_EMBEDDING_FORMAT=compatible")
	for _, k := range []string{"AUTOBID_MMDC_PATH", "AUTOBID_MMDC_BROWSER", "AUTOBID_OCR_URL", "AUTOBID_OCR_TOKEN", "AUTOBID_PG_BIN"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}
func (r *rehearsal) start(bin, dsn, data, label string) (*process, error) {
	for _, k := range []string{"filework", "exports", "evidence"} {
		if e := os.MkdirAll(filepath.Join(data, k), 0700); e != nil {
			return nil, e
		}
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return nil, e
	}
	addr := ln.Addr().String()
	ln.Close()
	p := &process{cmd: exec.CommandContext(r.ctx, bin), base: "http://" + addr, done: make(chan error, 1), log: &safeLog{}}
	p.cmd.Dir = r.root
	p.cmd.Env = append(r.env(dsn, data), "AUTOBID_HTTP_ADDR="+addr)
	if bin == r.oldBin {
		// The baseline predates running cli.js through node, so it needs the mmdc launcher.
		if cli := os.Getenv("AUTOBID_MMDC_PATH"); strings.HasSuffix(cli, "cli.js") {
			modules := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(cli))))
			p.cmd.Env = append(p.cmd.Env, "AUTOBID_MMDC_PATH="+filepath.Join(modules, ".bin", "mmdc.cmd"), "PUPPETEER_EXECUTABLE_PATH="+os.Getenv("AUTOBID_MMDC_BROWSER"))
		}
	}
	p.cmd.Stdout = p.log
	p.cmd.Stderr = p.log
	if e = p.cmd.Start(); e != nil {
		return nil, fmt.Errorf("启动 %s 失败", label)
	}
	go func() { p.done <- p.cmd.Wait() }()
	r.t.Cleanup(p.stop)
	deadline := time.NewTimer(4 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		select {
		case e := <-p.done:
			p.done <- e
			p.stop()
			r.write(label+"-startup.json", map[string]string{"log": p.log.String()})
			return nil, fmt.Errorf("%s 启动退出：%s", label, p.log.String())
		case <-deadline.C:
			p.stop()
			r.write(label+"-startup.json", map[string]string{"log": p.log.String()})
			return nil, fmt.Errorf("%s 就绪超时：%s", label, p.log.String())
		case <-tick.C:
			resp, e := client.Get(p.base + "/healthz")
			if e == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					r.write(label+"-startup.json", map[string]string{"log": p.log.String()})
					return p, nil
				}
			}
		}
	}
}
func (r *rehearsal) api(p *process, method, path string, body any) json.RawMessage {
	r.t.Helper()
	b := []byte(nil)
	if body != nil {
		var e error
		b, e = json.Marshal(body)
		r.must(e)
	}
	req, e := http.NewRequestWithContext(r.ctx, method, p.base+path, bytes.NewReader(b))
	r.must(e)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("X-Autobid-User", "s7-history-operator")
	resp, e := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	r.must(e)
	defer resp.Body.Close()
	b, e = io.ReadAll(resp.Body)
	r.must(e)
	if resp.StatusCode >= 300 {
		r.must(fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, string(b)))
	}
	return b
}
func decode[T any](r *rehearsal, b []byte) T { var v T; r.must(json.Unmarshal(b, &v)); return v }
func (r *rehearsal) ops(dsn, data string, args ...string) []byte {
	r.t.Helper()
	c := exec.CommandContext(r.ctx, r.opsBin, args...)
	c.Dir = r.root
	c.Env = r.env(dsn, data)
	b, e := c.CombinedOutput()
	if e != nil {
		r.must(fmt.Errorf("运维命令 %s 失败：%s", args[0], string(b)))
	}
	return b
}
func (r *rehearsal) catalog(d *db.DB) map[string]string {
	out := map[string]string{}
	rows, e := d.QueryContext(r.ctx, `SELECT 'constraint:'||c.conrelid::regclass::text||':'||c.conname,pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname='public' UNION ALL SELECT 'function:'||p.proname||':'||pg_get_function_identity_arguments(p.oid),pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.prokind='f' UNION ALL SELECT 'trigger:'||t.tgrelid::regclass::text||':'||t.tgname,pg_get_triggerdef(t.oid) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND NOT t.tgisinternal`)
	r.must(e)
	defer rows.Close()
	for rows.Next() {
		var k, v string
		r.must(rows.Scan(&k, &v))
		out[k] = v
	}
	r.must(rows.Err())
	return out
}
func (r *rehearsal) pathB() {
	dsn := r.create("pathb")
	d, e := ops.Open(r.ctx, dsn)
	r.must(e)
	defer d.Close()
	r.must(d.Migrate(r.ctx))
	before := r.catalog(d)
	sum, e := d.AppliedSchemaChecksum(r.ctx)
	r.must(e)
	p, startErr := r.start(r.oldBin, dsn, filepath.Join(r.tmp, "pathb-data"), "rollback-b-old")
	if p != nil {
		p.stop()
	}
	after := r.catalog(d)
	newSum, e := d.AppliedSchemaChecksum(r.ctx)
	r.must(e)
	changed := map[string]any{}
	for k, v := range before {
		if v != after[k] {
			changed[k] = map[string]string{"before": v, "after": after[k]}
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			changed[k] = map[string]string{"after": v}
		}
	}
	result := map[string]any{"old_server_ready": startErr == nil, "checksum_before": sum, "checksum_after": newSum, "changed_definitions": changed, "one_time_database": true, "old_program_has_generation_guard": false}
	if startErr != nil {
		result["startup_error"] = toolhealth.Sanitize(startErr.Error())
	}
	r.write("rollback-b.json", result)
	r.pass("路径 B：旧程序实际连接已升级的一次性库", result)
}
