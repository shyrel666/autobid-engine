package server

// Phase 6B 导出 HTTP 集成测试（需要 AUTOBID_TEST_DATABASE_URL）：导出门禁明细（422）、创建导出（201）、任务列表与详情、
// 下载（未就绪 409、成功 200、跨任务 404），以及模板上传 / 默认模板下载 / 引用中删除 409。
// 文档引擎在此不启用（DOCX 路径，目录标注未刷新）；目录页码与 PDF 版式验收在 export 包。

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/export"
)

func newExportAPI(t *testing.T, taskID int64) (*apiClient, *export.Service, *export.Runner) {
	t.Helper()
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(pg)
	e := NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil }, Config{Workers: 1})
	svc := &export.Service{DB: pg, Store: &export.Store{Root: t.TempDir()}, Renderer: pngRenderer{}, OfficeReason: "测试：不启用文档引擎"}
	runner := &export.Runner{Service: svc, Owner: "http-test", Lease: time.Minute, TaskID: taskID}
	api := NewAPI(ctx, m, e, NewLLMService(pg, LLMServiceConfig{}), nil, NewHub(), APIConfig{Token: testToken, Export: svc, ExportRunner: runner})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		e.StopAll()
		cancel()
	})
	return &apiClient{t: t, url: srv.URL}, svc, runner
}

func TestHTTPExport(t *testing.T) {
	d := dbtest.Open(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(3))
	c, _, runner := newExportAPI(t, p.TaskID)
	base := fmt.Sprintf("/api/tasks/%d", p.TaskID)
	var e struct {
		Error     string              `json:"error"`
		Code      string              `json:"code"`
		Preflight *db.ExportPreflight `json:"preflight"`
	}

	// 两张图表进入导出快照。
	for _, ch := range []map[string]any{
		{"section_id": p.Sections["5"], "chart_type": "gantt", "title": "实施进度甘特图",
			"spec": map[string]any{"start": "2026-11-01", "tasks": []map[string]any{{"id": "a", "name": "调研", "days": 5}, {"name": "实施", "after": "a", "days": 20}}}},
		{"section_id": p.Sections["6"], "chart_type": "equipment_table", "title": "设备选型",
			"spec": map[string]any{"columns": []string{"设备", "数量"}, "rows": [][]string{{"交换机", "2"}}}},
	} {
		if code := c.raw("POST", base+"/charts", ch, nil); code != 201 {
			t.Fatalf("generate chart: %d", code)
		}
	}

	// 门禁明细；导出前产物下载 409；创建导出 201，执行后下载 200。
	var pre struct {
		Readiness *db.ExportReadiness `json:"readiness"`
		Warnings  []db.ExportWarning  `json:"warnings"`
		Charts    int                 `json:"charts"`
		Status    export.Status       `json:"status"`
	}
	if code := c.raw("GET", base+"/exports/preflight", nil, &pre); code != 200 || !pre.Readiness.Ready || pre.Charts != 2 || pre.Status.Office {
		t.Fatalf("preflight: %d %+v", code, pre)
	}
	officeWarn := false
	for _, w := range pre.Warnings {
		officeWarn = officeWarn || w.Kind == "office_unavailable"
	}
	if !officeWarn {
		t.Fatalf("office warning missing: %+v", pre.Warnings)
	}
	if code := c.raw("POST", base+"/exports", map[string]any{"formats": []string{"html"}}, &e); code != 400 {
		t.Fatalf("invalid format: %d", code)
	}
	var created struct {
		Jobs []db.ExportJob `json:"jobs"`
	}
	if code := c.raw("POST", base+"/exports", map[string]any{"formats": []string{"docx"}, "bidder": "示例科技有限公司"}, &created); code != 201 || len(created.Jobs) != 1 {
		t.Fatalf("create export: %d %+v", code, created)
	}
	job := created.Jobs[0]
	if job.Doc.Bidder != "示例科技有限公司" || len(job.RevSnapshot) != len(p.Sections) || len(job.Charts) != 2 {
		t.Fatalf("job snapshot = %+v", job)
	}
	dl := fmt.Sprintf("%s/exports/%d/download", base, job.ID)
	if code := c.raw("GET", dl, nil, &e); code != 409 || e.Code != "not_ready" {
		t.Fatalf("download before run: %d %+v", code, e)
	}
	if ok, err := runner.RunOnce(context.Background()); !ok || err != nil {
		t.Fatalf("RunOnce: %v %v", ok, err)
	}
	var got db.ExportJob
	if code := c.raw("GET", fmt.Sprintf("%s/exports/%d", base, job.ID), nil, &got); code != 200 || got.Status != db.ExportSucceeded || got.TOCRefreshed {
		t.Fatalf("job after run: %d %+v", code, got)
	}
	code, h, body := c.download(dl)
	if code != 200 || !bytes.HasPrefix(body, []byte("PK")) || int64(len(body)) != got.ArtifactSize || h.Get("X-Artifact-SHA256") != got.ArtifactHash ||
		!strings.Contains(h.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d %d bytes %v", code, len(body), h)
	}
	if code := c.raw("GET", fmt.Sprintf("/api/tasks/%d/exports/%d/download", p.TaskID+100000, job.ID), nil, &e); code != 404 {
		t.Fatalf("download from other task: %d", code)
	}
	var jobs struct {
		Jobs []db.ExportJob `json:"jobs"`
	}
	if code := c.raw("GET", base+"/exports", nil, &jobs); code != 200 || len(jobs.Jobs) != 1 {
		t.Fatalf("list exports: %d %d", code, len(jobs.Jobs))
	}

	// 编辑章节后：门禁 422 列出全部明细。
	p.Save(t, "3", "我方承诺免费质保期 5 年，优于招标要求，并提供备品备件。")
	if code := c.raw("POST", base+"/exports", map[string]any{"formats": []string{"pdf"}}, &e); code != 422 || e.Code != "export_blocked" || e.Preflight == nil ||
		e.Preflight.Readiness.Counts[db.BlockSectionUnapproved] != 1 || e.Preflight.Readiness.Counts[db.BlockMatrixStale] == 0 {
		t.Fatalf("blocked export: %d %+v", code, e)
	}

	// 模板：非 docx 400；上传 201；引用后删除 409；未引用的可删除。
	if code := c.multipart("/api/export/templates", map[string]string{"name": "x"}, "x.txt", []byte("hi"), nil); code != 400 {
		t.Fatalf("txt template: %d", code)
	}
	if code := c.multipart("/api/export/templates", map[string]string{"name": "x"}, "bad.docx", []byte("not a zip"), nil); code != 400 {
		t.Fatalf("invalid docx template: %d", code)
	}
	custom := customTemplate(t)
	if code, _, raw := c.download("/api/export/templates/default/file"); code != 200 || !bytes.Equal(raw, export.DefaultTemplateBytes()) {
		t.Fatalf("default template download: %d", code)
	}
	var up db.ExportTemplate
	if code := c.multipart("/api/export/templates", map[string]string{"name": "公司模板"}, "company.docx", custom, &up); code != 201 || up.ID == 0 {
		t.Fatalf("upload template: %d", code)
	}
	p.ApproveAll(t)
	if code := c.raw("POST", base+"/compliance/verify", map[string]any{}, nil); code != 200 {
		t.Fatalf("reverify: %d", code)
	}
	if code := c.raw("POST", base+"/consistency/runs", nil, nil); code != 201 && code != 200 {
		t.Fatalf("consistency: %d", code)
	}
	if code := c.raw("POST", base+"/exports", map[string]any{"formats": []string{"docx"}, "template_id": up.ID}, &created); code != 201 || created.Jobs[0].TemplateID == nil {
		t.Fatalf("export with template: %d %+v", code, e)
	}
	if code := c.raw("DELETE", fmt.Sprintf("/api/export/templates/%d", up.ID), nil, &e); code != 409 || e.Code != "template_in_use" {
		t.Fatalf("delete referenced template: %d %+v", code, e)
	}
	if ok, err := runner.RunOnce(context.Background()); !ok || err != nil {
		t.Fatalf("RunOnce with custom template: %v %v", ok, err)
	}
	if code := c.raw("GET", fmt.Sprintf("%s/exports/%d", base, created.Jobs[0].ID), nil, &got); code != 200 || got.Status != db.ExportSucceeded {
		t.Fatalf("custom template job: %+v", got)
	}
}

// customTemplate 构造一个「客户模板」：没有 {{content}} 占位（正文追加在模板内容之后）、标题样式 ID 为 "1" / "2"
// （中文 Word 常见），题注样式缺失，用于验证按样式名解析与缺失样式补齐。
func customTemplate(t *testing.T) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(export.DefaultTemplateBytes()), int64(len(export.DefaultTemplateBytes())))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		s := string(data)
		switch f.Name {
		case "word/document.xml":
			s = strings.Replace(s, `<w:t>{{content}}</w:t>`, `<w:t>以下为技术标正文。</w:t>`, 1)
		case "word/styles.xml":
			s = strings.NewReplacer(`w:styleId="Heading1"`, `w:styleId="1"`, `w:styleId="Heading2"`, `w:styleId="2"`).Replace(s)
			i := strings.Index(s, `<w:style w:type="paragraph" w:styleId="Caption">`)
			j := strings.Index(s[i:], `</w:style>`) + i + len(`</w:style>`)
			s = s[:i] + s[j:]
		}
		w, _ := zw.Create(f.Name)
		_, _ = w.Write([]byte(s))
	}
	_ = zw.Close()
	return buf.Bytes()
}
