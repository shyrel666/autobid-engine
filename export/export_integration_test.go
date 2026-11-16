package export

// Phase 6B 验收（真实 PostgreSQL + 文档引擎 WPS / Word）：
//   3. 导出的 .docx 与 .pdf 均能被 ingest 解析器回读，章节标题、偏离表行数与快照一致；
//   4. PDF 目录每个条目的页码 = 正文中同名标题实际所在页；.docx 的 TOC 域缓存结果非空且不是占位文字；
//   5. 13 列宽表、架构图、宽甘特图、长偏离表（45 行）：文字与图片不越出版芯、图片不重叠、无空白页；
//   以及导出后再编辑章节不影响已有任务快照、文档引擎不可用时 PDF 明确失败 / DOCX 标注目录未刷新、
//   running 任务租约过期后被回收重试且产物只生成一次。
// 依赖缺失时跳过并写明缺少的依赖；mmdc 不可用时图表渲染使用测试替身 PNG（stubRenderer），不代表 mmdc 已验证。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/export/officetest"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/toolhealth"
)

// requireOffice 先排队再探测：真实文档引擎的用例跨包串行，用例结束才释放，进程快照断言看不到别的包的进程。
func requireOffice(t *testing.T) Office {
	t.Helper()
	officetest.Hold(t)
	o, err := DetectOffice(context.Background(), OfficeOptions{Engine: os.Getenv("AUTOBID_OFFICE_ENGINE"), Timeout: 3 * time.Minute})
	if err != nil {
		t.Skipf("依赖缺失：文档引擎（WPS / Word）不可用，无法验证目录页码刷新与 PDF 版式：%v", err)
	}
	return o
}

// stubRenderer 是 mmdc 的测试替身：按图表类型返回真实尺寸比例的 PNG（架构图 16:11、甘特图 10:3）。
type stubRenderer struct{}

func (stubRenderer) Name() string { return "stub-png（测试替身，非 mermaid-cli）" }
func (stubRenderer) RenderPNG(_ context.Context, src string) ([]byte, error) {
	switch {
	case strings.HasPrefix(src, "gantt"):
		return stubPNG(2400, 720, 2), nil
	case strings.HasPrefix(src, "flowchart TD"): // 组织架构图
		return stubPNG(1200, 500, 3), nil
	}
	return stubPNG(1600, 1100, 1), nil
}

func runAll(t *testing.T, r *Runner) {
	t.Helper()
	for i := 0; i < 10; i++ {
		ok, err := r.RunOnce(context.Background())
		if err != nil {
			t.Logf("RunOnce: %v", err)
		}
		if !ok {
			return
		}
	}
	t.Fatal("runner did not drain the queue")
}

func reloadJob(t *testing.T, d *db.DB, taskID, id int64) *db.ExportJob {
	t.Helper()
	j, err := d.GetExportJob(context.Background(), db.DefaultTenant, taskID, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func artifact(t *testing.T, s *Service, j *db.ExportJob) []byte {
	t.Helper()
	p, err := s.ArtifactPath(j)
	if err != nil {
		t.Fatalf("job %d (%s) %s: %v (error: %s)", j.ID, j.Format, j.Status, err, j.Error)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if sha256Hex(data) != j.ArtifactHash || int64(len(data)) != j.ArtifactSize {
		t.Fatalf("job %d artifact hash/size mismatch", j.ID)
	}
	return data
}

func TestExportEndToEndDOCXAndPDF(t *testing.T) {
	d := dbtest.Open(t)
	office := requireOffice(t)
	ctx := context.Background()
	spec := dbtest.DefaultSpec(40).WithWideTable("1.1", 13, 18)
	p := dbtest.Ready(t, d, spec)
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, Renderer: stubRenderer{}, Office: office}
	t.Logf("document engine: %s (%s)", office.Name(), office.Version())

	gen := func(code, typ, title, raw string) *db.ProposalChart {
		t.Helper()
		c, err := svc.GenerateChart(ctx, db.DefaultTenant, p.TaskID, "writer", ChartRequest{SectionID: p.Sections[code], ChartType: typ, Title: title, Spec: []byte(raw)})
		if err != nil {
			t.Fatalf("GenerateChart %s: %v", typ, err)
		}
		return c
	}
	arch := gen("1.1", db.ChartArchitecture, "系统总体架构图", `{"layers":[{"name":"感知层","nodes":["高清摄像机","门禁控制器"]},{"name":"平台层","nodes":["视频管理平台","统一运维平台"]}],"edges":[{"from":"高清摄像机","to":"视频管理平台","label":"RTSP"}]}`)
	gantt := gen("5", db.ChartGantt, "实施进度甘特图", `{"start":"2026-11-01","tasks":[{"id":"t1","name":"需求调研","phase":"准备阶段","days":10},{"id":"t2","name":"设备采购","after":"t1","days":30},{"name":"安装调试","phase":"实施阶段","after":"t2","days":40},{"name":"竣工验收","phase":"验收阶段","days":10}]}`)
	// 同一章节连续两张图：验证同页图片不重叠。
	ops := gen("1.1", db.ChartOrg, "运维保障组织图", `{"nodes":[{"id":"lead","name":"运维负责人"},{"id":"l1","name":"一线值守","parent":"lead"},{"id":"l2","name":"二线专家","parent":"lead"}]}`)
	table := gen("6", db.ChartEquipmentTable, "核心设备选型表", `{"columns":["序号","设备名称","品牌型号","数量"],"rows":[["1","核心交换机","华为 S7706","2"],["2","存储服务器","R5300 G5","4"]]}`)
	svc.Renderer, svc.RendererReason = nil, "测试：模拟 mmdc 不可用"
	org := gen("6", db.ChartOrg, "项目组织架构图", `{"nodes":[{"id":"pm","name":"项目经理","person":"张三"},{"id":"tech","name":"技术组","parent":"pm"},{"id":"qa","name":"质量组","parent":"pm"}]}`)
	svc.Renderer = stubRenderer{}
	for _, c := range []*db.ProposalChart{arch, ops, gantt, table} {
		if c.RenderStatus != db.ChartRendered || c.Stale {
			t.Fatalf("chart %s = %+v", c.ChartType, c)
		}
	}
	if org.RenderStatus != db.ChartUnrendered || !strings.Contains(org.RenderError, "mmdc") {
		t.Fatalf("org chart without mmdc = %+v", org)
	}

	pre, err := d.ExportPreflight(ctx, db.DefaultTenant, p.TaskID)
	if err != nil || !pre.Readiness.Ready || pre.Charts != 5 || len(pre.Warnings) != 1 || pre.Warnings[0].Kind != db.WarnChartUnrendered {
		t.Fatalf("preflight = %+v %v", pre, err)
	}
	jobs, _, err := d.CreateExportJobs(ctx, db.DefaultTenant, p.TaskID, "exporter", db.CreateExportInput{Formats: []string{db.ExportDOCX, db.ExportPDF},
		DefaultTemplateHash: DefaultTemplateHash(), Bidder: "示例科技有限公司"})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("CreateExportJobs: %v", err)
	}
	for code, id := range p.Sections {
		if jobs[0].RevSnapshot[strconv.FormatInt(id, 10)] != 1 {
			t.Fatalf("rev_snapshot[%s] = %v", code, jobs[0].RevSnapshot)
		}
	}
	if len(jobs[0].Charts) != 5 || len(jobs[0].Matrix) != len(spec.Reqs) || len(jobs[0].Doc.Sections) != len(spec.Sections) {
		t.Fatalf("snapshot charts=%d matrix=%d sections=%d", len(jobs[0].Charts), len(jobs[0].Matrix), len(jobs[0].Doc.Sections))
	}

	runAll(t, &Runner{Service: svc, Owner: "e2e", Lease: 5 * time.Minute, TaskID: p.TaskID})
	docxJob, pdfJob := reloadJob(t, d, p.TaskID, jobs[0].ID), reloadJob(t, d, p.TaskID, jobs[1].ID)
	for _, j := range []*db.ExportJob{docxJob, pdfJob} {
		if j.Status != db.ExportSucceeded || !j.TOCRefreshed || j.Attempts != 1 {
			t.Fatalf("job %d = %s attempts=%d toc=%v error=%s", j.ID, j.Status, j.Attempts, j.TOCRefreshed, j.Error)
		}
		for _, w := range j.Warnings {
			if w.Kind == db.WarnSelfCheck {
				t.Errorf("job %d self-check: %s", j.ID, w.Message)
			}
		}
	}
	tpl, _ := DefaultTemplate()
	pdf := artifact(t, svc, pdfJob)
	docx := artifact(t, svc, docxJob)
	wantHeadings := append(append([]string{ScoreIndexTitle}, p.Headings()...), DeviationTitle) // D4：目录之后先是评分索引表

	// 3. 文件合法性：两种格式均能被 ingest 解析器回读。
	if _, err := ingest.Parse(ctx, ingest.File{Name: "out.pdf", Data: pdf}, ingest.ParseOptions{}); err != nil {
		t.Fatalf("ingest.Parse(pdf): %v", err)
	}
	drep, err := CheckDOCX(ctx, docx)
	if err != nil {
		t.Fatalf("CheckDOCX: %v", err)
	}
	if strings.Join(drep.Headings, "|") != strings.Join(wantHeadings, "|") {
		t.Fatalf("docx headings\n got %q\nwant %q", drep.Headings, wantHeadings)
	}
	var devRows int
	for _, tb := range drep.Tables {
		if len(tb.Grid) > 0 && len(tb.Grid[0]) == len(DeviationHeader) && tb.Grid[0][1] == DeviationHeader[1] {
			devRows = tb.Rows - 1
		}
	}
	if devRows != len(pdfJob.Matrix) {
		t.Fatalf("docx deviation table rows = %d, snapshot = %d", devRows, len(pdfJob.Matrix))
	}

	// 4. 目录页码一致：PDF 目录条目页码 = 标题实际页；DOCX TOC 缓存结果非空、非占位、带页码，并与 PDF 一致。
	rep, err := CheckPDF(pdf, tpl.Geometry)
	if err != nil {
		t.Fatalf("CheckPDF: %v", err)
	}
	for _, is := range rep.Issues {
		t.Errorf("PDF issue (page %d) %s: %s", is.Page, is.Kind, is.Detail)
	}
	if len(rep.TOC) != len(wantHeadings) {
		t.Fatalf("PDF TOC entries = %+v", rep.TOC)
	}
	for i, e := range rep.TOC {
		if e.Title != normText(wantHeadings[i]) || e.Listed != e.Actual || e.Actual == 0 {
			t.Fatalf("PDF TOC[%d] = %+v want %q", i, e, wantHeadings[i])
		}
	}
	for _, is := range drep.Issues {
		t.Errorf("DOCX issue %s: %s", is.Kind, is.Detail)
	}
	if len(drep.TOC) != len(wantHeadings) {
		t.Fatalf("docx TOC = %+v", drep.TOC)
	}
	for i, e := range drep.TOC {
		if normText(e.Title) != normText(wantHeadings[i]) || e.Page != strconv.Itoa(rep.TOC[i].Actual) {
			t.Errorf("docx TOC[%d] = %+v, PDF heading page %d", i, e, rep.TOC[i].Actual)
		}
	}

	// 5. 版式：宽表与甘特图在横向页，三张 PNG 图（其中两张同页；未渲染的组织架构图为源码占位框），偏离表每条要求都在附录页中。
	if len(rep.Landscape) < 2 || rep.Images != 3 {
		t.Fatalf("landscape pages %v, images %d", rep.Landscape, rep.Images)
	}
	samePage := false
	for _, pg := range rep.Layout {
		samePage = samePage || len(pg.Images) >= 2
	}
	if !samePage {
		t.Fatal("expected two charts on one page to exercise the overlap check")
	}
	appendix := rep.TOC[len(rep.TOC)-1].Actual
	seen := map[string]bool{}
	placeholder := false
	for _, pg := range rep.Layout {
		for _, l := range pg.Lines {
			if strings.Contains(l.Text, "【图表未渲染") {
				placeholder = true
			}
			if pg.Number >= appendix {
				for _, s := range l.Segments {
					seen[strings.TrimSpace(s)] = true
				}
			}
		}
	}
	if !placeholder {
		t.Fatal("unrendered org chart has no source placeholder in the PDF")
	}
	for _, m := range pdfJob.Matrix {
		if !seen[m.Code] {
			t.Fatalf("deviation row %s missing from the PDF appendix", m.Code)
		}
	}
	t.Logf("PDF: %d pages, TOC pages %v, landscape %v, %d images; deviation rows %d", rep.Pages, rep.TOCPages, rep.Landscape, rep.Images, devRows)

	// 导出后再编辑章节：已有任务快照与产物不变；新导出被门禁拦截。
	p.Save(t, "1", strings.Replace(spec.Sections[0].Body, "技术路线先进", "技术路线先进、成熟", 1))
	after := reloadJob(t, d, p.TaskID, docxJob.ID)
	if after.RevSnapshot[strconv.FormatInt(p.Sections["1"], 10)] != 1 || after.ArtifactHash != docxJob.ArtifactHash {
		t.Fatalf("snapshot changed after edit: %v", after.RevSnapshot)
	}
	if !bytesEqualFile(t, svc, after, docx) {
		t.Fatal("artifact on disk changed after edit")
	}
	_, blockedPre, err := d.CreateExportJobs(ctx, db.DefaultTenant, p.TaskID, "exporter", db.CreateExportInput{Formats: []string{db.ExportDOCX}})
	var blocked *db.ExportBlockedError
	if !errors.As(err, &blocked) || blockedPre.Readiness.Counts[db.BlockSectionUnapproved] != 1 || blockedPre.Readiness.Counts[db.BlockMatrixStale] == 0 {
		t.Fatalf("export after edit = %v %+v", err, blockedPre)
	}
	if _, err := d.ExecContext(ctx, `UPDATE export_jobs SET rev_snapshot = '{}' WHERE id = $1`, docxJob.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("snapshot update not rejected: %v", err)
	}
}

func bytesEqualFile(t *testing.T, s *Service, j *db.ExportJob, want []byte) bool {
	t.Helper()
	got := artifact(t, s, j)
	return string(got) == string(want)
}

// 文档引擎不可用：PDF 任务明确失败并写明原因（不重试），DOCX 成功但标注目录页码未刷新、TOC 缓存为占位文字。
func TestExportWithoutOfficeEngine(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, OfficeReason: "测试：模拟文档引擎不可用"}
	jobs, _, err := d.CreateExportJobs(ctx, db.DefaultTenant, p.TaskID, "exporter", db.CreateExportInput{Formats: []string{db.ExportDOCX, db.ExportPDF},
		DefaultTemplateHash: DefaultTemplateHash()})
	if err != nil {
		t.Fatal(err)
	}
	runAll(t, &Runner{Service: svc, Owner: "no-office", Lease: time.Minute, TaskID: p.TaskID})
	dj, pj := reloadJob(t, d, p.TaskID, jobs[0].ID), reloadJob(t, d, p.TaskID, jobs[1].ID)
	if pj.Status != db.ExportFailed || pj.Attempts != 1 || !strings.Contains(pj.Error, "文档引擎（WPS / Word）不可用") || pj.ArtifactPath != "" {
		t.Fatalf("pdf job = %s attempts=%d error=%q", pj.Status, pj.Attempts, pj.Error)
	}
	if dj.Status != db.ExportSucceeded || dj.TOCRefreshed {
		t.Fatalf("docx job = %s toc=%v error=%s", dj.Status, dj.TOCRefreshed, dj.Error)
	}
	var flagged bool
	for _, w := range dj.Warnings {
		flagged = flagged || (w.Kind == db.WarnTOCNotRefreshed && strings.Contains(w.Message, "目录页码未刷新"))
	}
	if !flagged {
		t.Fatalf("docx warnings = %+v", dj.Warnings)
	}
	rep, err := CheckDOCX(ctx, artifact(t, svc, dj))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, is := range rep.Issues {
		kinds[is.Kind] = true
	}
	if !kinds[IssueTOCPlaceholder] {
		t.Fatalf("unrefreshed docx TOC not detected as placeholder: %+v", rep)
	}
}

// 重启恢复：执行者领取后崩溃（租约过期、留下半成品目录）-> 回收重试 -> 产物只生成一次；旧执行者的迟到写入被围栏令牌拒绝。
func TestExportJobRecoveredAfterCrash(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, OfficeReason: "测试：恢复逻辑与文档引擎无关"}
	jobs, _, err := d.CreateExportJobs(ctx, db.DefaultTenant, p.TaskID, "exporter", db.CreateExportInput{Formats: []string{db.ExportDOCX},
		DefaultTemplateHash: DefaultTemplateHash()})
	if err != nil {
		t.Fatal(err)
	}
	crashed, err := d.ClaimExportJob(ctx, "crashed-worker", time.Second, p.TaskID)
	if err != nil || crashed == nil || crashed.ID != jobs[0].ID || crashed.LeaseToken != 1 || crashed.Attempts != 1 {
		t.Fatalf("claim = %+v %v", crashed, err)
	}
	// 崩溃前留下的半成品。
	partial, _ := svc.Store.Abs(svc.Store.jobRel(db.DefaultTenant, p.TaskID, crashed.ID) + "/t1/artifact.docx")
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("PK\x03\x04 half written"), 0o644); err != nil {
		t.Fatal(err)
	}
	if again, _ := d.ClaimExportJob(ctx, "other", time.Minute, p.TaskID); again != nil {
		t.Fatalf("job with a live lease was claimed twice: %+v", again)
	}
	time.Sleep(1500 * time.Millisecond)
	ids, err := d.ReclaimExpiredExportJobs(ctx, p.TaskID)
	if err != nil || len(ids) != 1 || ids[0] != crashed.ID {
		t.Fatalf("reclaim = %v %v", ids, err)
	}
	if j := reloadJob(t, d, p.TaskID, crashed.ID); j.Status != db.ExportQueued {
		t.Fatalf("reclaimed job status = %s", j.Status)
	}
	runAll(t, &Runner{Service: svc, Owner: "restarted", Lease: time.Minute, TaskID: p.TaskID})
	j := reloadJob(t, d, p.TaskID, crashed.ID)
	if j.Status != db.ExportSucceeded || j.Attempts != 2 || j.LeaseToken != 2 {
		t.Fatalf("retried job = %s attempts=%d token=%d error=%s", j.Status, j.Attempts, j.LeaseToken, j.Error)
	}
	retried := false
	for _, w := range j.Warnings {
		retried = retried || w.Kind == db.WarnRetried
	}
	if !retried {
		t.Fatalf("retry not recorded: %+v", j.Warnings)
	}
	// 旧执行者迟到的心跳与完成写入被拒绝，记录不变。
	if err := d.HeartbeatExportJob(ctx, j.ID, 1, time.Minute); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	if err := d.CompleteExportJob(ctx, j.ID, 1, db.ExportResult{ArtifactPath: "x", ArtifactHash: "y"}); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatalf("stale completion: %v", err)
	}
	if again := reloadJob(t, d, p.TaskID, j.ID); again.ArtifactHash != j.ArtifactHash || again.ArtifactPath != j.ArtifactPath {
		t.Fatal("stale completion overwrote the result")
	}
	if ok, _ := (&Runner{Service: svc, Owner: "idle", TaskID: p.TaskID}).RunOnce(ctx); ok {
		t.Fatal("succeeded job was executed again")
	}
	// 产物只有一份：任务目录下只剩本次令牌目录中的一个文件，半成品已清理。
	root, _ := svc.Store.Abs(svc.Store.jobRel(db.DefaultTenant, p.TaskID, j.ID))
	var files []string
	_ = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if len(files) != 1 || files[0] != "t2/artifact.docx" {
		t.Fatalf("artifacts on disk = %v", files)
	}
	if data := artifact(t, svc, j); len(data) < 1000 {
		t.Fatal("artifact too small")
	}
}

// mmdc 渲染（依赖 mermaid-cli，未安装时跳过）。
func TestMMDCRendersGantt(t *testing.T) {
	m, err := DetectMMDC()
	if err != nil {
		t.Skipf("依赖缺失：mermaid-cli（mmdc）未安装或 AUTOBID_MMDC_PATH 未设置：%v", err)
	}
	src, err := BuildChartSource(db.ChartGantt, "实施进度", []byte(`{"start":"2026-11-01","tasks":[{"id":"a","name":"调研","days":5},{"name":"实施","after":"a","days":20}]}`))
	if err != nil {
		t.Fatal(err)
	}
	png, err := m.RenderPNG(context.Background(), src.Source)
	if err != nil || len(png) < 100 {
		t.Fatalf("mmdc render: %v", err)
	}
	if m.Version() == "" || rendererVersion(m) == "" {
		t.Fatalf("mmdc version not detected: %q", m.Version())
	}
	if err := RendererSelfTest(context.Background(), m); err != nil {
		t.Fatalf("mmdc self-test: %v", err)
	}
}

type failingRenderer struct{ err error }

func (failingRenderer) Name() string { return "failing" }
func (f failingRenderer) RenderPNG(context.Context, string) ([]byte, error) {
	return nil, f.err
}

// 自检用真实生成器构造中文架构图与甘特图；渲染失败或输出不是 PNG 时自检失败，未安装与自检失败分别记录。
func TestRendererSelfTestAndProbeStates(t *testing.T) {
	if err := RendererSelfTest(context.Background(), stubRenderer{}); err != nil {
		t.Fatalf("stub self-test: %v", err)
	}
	if err := RendererSelfTest(context.Background(), failingRenderer{errors.New("chrome crashed")}); err == nil {
		t.Fatal("failing renderer passed the self-test")
	}
	reg := toolhealth.New()
	recordProbe(reg, toolhealth.Mermaid, toolhealth.Result{Provider: "mmdc"}, 0, fmt.Errorf("%w: %w: not on PATH", ErrRendererMissing, ErrNotInstalled))
	recordProbe(reg, toolhealth.Office, toolhealth.Result{}, 0, fmt.Errorf("%w: wps: self-test PDF TOC check failed", ErrOfficeUnavailable))
	if st := reg.Get(toolhealth.Mermaid); st.State != toolhealth.Unconfigured {
		t.Fatalf("mermaid = %+v", st)
	}
	if st := reg.Get(toolhealth.Office); st.State != toolhealth.SelfTestFailed {
		t.Fatalf("office = %+v", st)
	}
	recordProbe(nil, toolhealth.Office, toolhealth.Result{}, 0, nil) // 未注入注册表时不报错
}

// 上传的客户模板（无 {{content}} 占位、标题样式 ID 为 "1" / "2"、缺少题注样式）经文档引擎刷新后，目录页码与版式同样一致。
func TestCustomTemplateLayout(t *testing.T) {
	office := requireOffice(t)
	tpl, err := LoadTemplate("customer", customerTemplate(t))
	if err != nil {
		t.Fatal(err)
	}
	if m := tpl.Meta(); m.HasContentTag || len(m.MissingStyles) == 0 {
		t.Fatalf("customer template meta = %+v", m)
	}
	res, err := Build(tpl, sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in, outDocx, outPDF := filepath.Join(dir, "in.docx"), filepath.Join(dir, "out.docx"), filepath.Join(dir, "out.pdf")
	if err := os.WriteFile(in, res.Data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := office.Refresh(context.Background(), in, outDocx, outPDF); err != nil {
		t.Fatal(err)
	}
	pdf, _ := os.ReadFile(outPDF)
	rep, err := CheckPDF(pdf, tpl.Geometry)
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range rep.Issues {
		t.Errorf("PDF issue (page %d) %s: %s", is.Page, is.Kind, is.Detail)
	}
	if len(rep.TOC) != len(sampleInput().Doc.Sections)+1 {
		t.Fatalf("TOC = %+v", rep.TOC)
	}
	docx, _ := os.ReadFile(outDocx)
	drep, err := CheckDOCX(context.Background(), docx)
	if err != nil || len(drep.Issues) != 0 || len(drep.Headings) != len(rep.TOC) {
		t.Fatalf("docx = %+v %v", drep, err)
	}
}

// customerTemplate 改写内置模板：去掉 {{content}}、标题样式改用数字 ID、删除题注样式。
func customerTemplate(t *testing.T) []byte {
	t.Helper()
	tpl, err := DefaultTemplate()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for k, v := range tpl.files {
		files[k] = v
	}
	doc := strings.Replace(string(files["word/document.xml"]), `<w:t>{{content}}</w:t>`, `<w:t>以下为技术标正文。</w:t>`, 1)
	files["word/document.xml"] = []byte(doc)
	st := strings.NewReplacer(`w:styleId="Heading1"`, `w:styleId="1"`, `w:styleId="Heading2"`, `w:styleId="2"`).Replace(string(files["word/styles.xml"]))
	i := strings.Index(st, `<w:style w:type="paragraph" w:styleId="Caption">`)
	j := strings.Index(st[i:], `</w:style>`) + i + len(`</w:style>`)
	files["word/styles.xml"] = []byte(st[:i] + st[j:])
	data, err := writeZip(tpl.names, files)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 真实 mmdc 渲染的中文架构图、甘特图、组织图插入成稿，经文档引擎刷新后回读 PDF：无版式问题、三张图都在、目录页码一致，
// 运行能力状态在成功调用后保持 healthy。依赖 mmdc 与文档引擎，缺一则跳过。
func TestExportWithRealMermaidCharts(t *testing.T) {
	m, err := DetectMMDC()
	if err != nil {
		t.Skipf("依赖缺失：mermaid-cli 未配置：%v", err)
	}
	d := dbtest.Open(t)
	office := requireOffice(t)
	ctx := context.Background()
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(20))
	health := toolhealth.New()
	health.Record(toolhealth.Mermaid, toolhealth.Result{Provider: "mmdc", Version: m.Version()}, 0, nil)
	health.Record(toolhealth.Office, toolhealth.Result{Provider: office.Name(), Engine: office.Name(), Version: office.Version()}, 0, nil)
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, Renderer: m, Office: office, Health: health}
	specs := []struct{ code, typ, title, raw string }{
		{"1.1", db.ChartArchitecture, "系统总体架构图", `{"layers":[{"name":"感知层","nodes":["高清摄像机","门禁控制器","环境传感器"]},{"name":"平台层","nodes":["视频管理平台","统一运维平台"]},{"name":"应用层","nodes":["指挥调度","数据分析"]}],"edges":[{"from":"高清摄像机","to":"视频管理平台","label":"RTSP"},{"from":"统一运维平台","to":"指挥调度"}]}`},
		{"5", db.ChartGantt, "实施进度甘特图", `{"start":"2026-11-01","tasks":[{"id":"t1","name":"需求调研","phase":"准备阶段","days":10},{"id":"t2","name":"设备采购","after":"t1","days":30},{"id":"t3","name":"安装调试","phase":"实施阶段","after":"t2","days":40},{"name":"试运行","after":"t3","days":20},{"name":"竣工验收","phase":"验收阶段","days":10,"milestone":true}]}`},
		{"6", db.ChartOrg, "项目组织架构图", `{"nodes":[{"id":"pm","name":"项目经理","person":"张三"},{"id":"tech","name":"技术组","parent":"pm"},{"id":"qa","name":"质量组","parent":"pm"},{"id":"ops","name":"运维组","parent":"pm"}]}`},
	}
	for _, s := range specs {
		c, err := svc.GenerateChart(ctx, db.DefaultTenant, p.TaskID, "writer", ChartRequest{SectionID: p.Sections[s.code], ChartType: s.typ, Title: s.title, Spec: []byte(s.raw)})
		if err != nil || c.RenderStatus != db.ChartRendered || c.RenderFormat != db.ChartFormatPNG || c.WidthPx < 100 {
			t.Fatalf("real mmdc chart %s: %v %+v", s.typ, err, c)
		}
		t.Logf("%s: %dx%d px, %d bytes", s.typ, c.WidthPx, c.HeightPx, c.ArtifactSize)
	}
	jobs, _, err := d.CreateExportJobs(ctx, db.DefaultTenant, p.TaskID, "exporter", db.CreateExportInput{Formats: []string{db.ExportPDF}, DefaultTemplateHash: DefaultTemplateHash()})
	if err != nil {
		t.Fatal(err)
	}
	runAll(t, &Runner{Service: svc, Owner: "real-mmdc", Lease: 5 * time.Minute, TaskID: p.TaskID})
	j := reloadJob(t, d, p.TaskID, jobs[0].ID)
	if j.Status != db.ExportSucceeded || !j.TOCRefreshed {
		t.Fatalf("export = %s: %s", j.Status, j.Error)
	}
	for _, w := range j.Warnings {
		t.Errorf("warning %s: %s", w.Kind, w.Message)
	}
	// 这次导出没有填投标人：PDF 的作者必须为空，不能是本机办公软件账号（WPS 在另存 DOCX 时会把空作者填成登录账号，
	// 所以引擎脚本先导出 PDF 再另存 DOCX）。
	if md, issues, err := CheckPDFMetadata(artifact(t, svc, j), ""); err != nil || len(issues) != 0 || md.Author != "" {
		t.Fatalf("PDF metadata without a bidder = %+v issues = %+v %v", md, issues, err)
	}
	tpl, _ := DefaultTemplate()
	rep, err := CheckPDF(artifact(t, svc, j), tpl.Geometry)
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range rep.Issues {
		t.Errorf("PDF issue (page %d) %s: %s", is.Page, is.Kind, is.Detail)
	}
	if rep.Images != len(specs) {
		t.Fatalf("PDF has %d images, want %d", rep.Images, len(specs))
	}
	for i, e := range rep.TOC {
		if e.Listed != e.Actual || e.Actual == 0 {
			t.Fatalf("PDF TOC[%d] = %+v", i, e)
		}
	}
	for _, c := range []toolhealth.Capability{toolhealth.Mermaid, toolhealth.Office} {
		if st := health.Get(c); st.State != toolhealth.Healthy {
			t.Fatalf("%s after successful use = %+v", c, st)
		}
	}
	t.Logf("PDF: %d pages, landscape %v, %d images, engine %s", rep.Pages, rep.Landscape, rep.Images, office.Version())
}
