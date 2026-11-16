package export

// S4 导出请求组的服务层与执行器（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：
//   - 严格最终稿端到端（需要文档引擎 WPS / Word，未安装时跳过）：真实刷新目录，记录实际引擎，DOCX / PDF 回读目录、图片与表格，
//     同键重放与同键异参；图表优先用真实 mermaid-cli（未配置时用替身 PNG 并在日志中写明）；
//   - 严格策略拒绝降级（替身文档引擎，不依赖 WPS）：提交时无引擎 503 语义、重放不受运行能力影响、执行时引擎缺失 / 版本不符、
//     目录未刷新的回读失败、PNG 哈希不符，均失败且不留下产物；预览稿保持旧行为（preview_only）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
)

// copyOffice 是文档引擎的测试替身：不刷新目录，只把输入原样另存（PDF 输出为非 PDF 字节），用来验证严格回读会拒绝它。
type copyOffice struct{ label string }

func (o copyOffice) Name() string    { return "copy" }
func (o copyOffice) Version() string { return o.label }
func (o copyOffice) Refresh(_ context.Context, in, outDocx, outPDF string) (*RefreshResult, error) {
	data, err := os.ReadFile(in)
	if err != nil {
		return nil, err
	}
	if outDocx != "" {
		if err := os.WriteFile(outDocx, data, 0o644); err != nil {
			return nil, err
		}
	}
	if outPDF != "" {
		if err := os.WriteFile(outPDF, []byte("%PDF-1.4 not really"), 0o644); err != nil {
			return nil, err
		}
	}
	return &RefreshResult{Passes: 1}, nil
}

func finalRequest(key string, formats ...string) ExportRequest {
	return ExportRequest{Formats: formats, Mode: ExportModeFinal, RequestKey: key, Bidder: "示例科技有限公司"}
}

func jobDirFiles(t *testing.T, s *Service, j *db.ExportJob) []string {
	t.Helper()
	root, _ := s.Store.Abs(s.Store.jobRel(db.DefaultTenant, j.TaskID, j.ID))
	var files []string
	_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files
}

func TestStrictFinalExportEndToEnd(t *testing.T) {
	d := dbtest.Open(t)
	office := requireOffice(t)
	ctx := context.Background()
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(10))
	var renderer Renderer = stubRenderer{}
	if m, err := DetectMMDC(); err == nil {
		renderer = m
	} else {
		t.Logf("mermaid-cli 未配置，图表使用替身 PNG（不作为图表运行验收）：%v", err)
	}
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, Renderer: renderer, Office: office}
	t.Logf("document engine %s, renderer %s", OfficeLabel(office), RendererLabel(renderer))
	for _, c := range []ChartRequest{
		{SectionID: p.Sections["1.1"], ChartType: db.ChartArchitecture, Title: "系统总体架构图",
			Spec: []byte(`{"layers":[{"name":"感知层","nodes":["高清摄像机","门禁控制器"]},{"name":"平台层","nodes":["视频管理平台"]}],"edges":[{"from":"高清摄像机","to":"视频管理平台"}]}`)},
		{SectionID: p.Sections["5"], ChartType: db.ChartGantt, Title: "实施进度甘特图",
			Spec: []byte(`{"start":"2026-11-01","tasks":[{"id":"t1","name":"需求调研","phase":"准备阶段","days":10},{"name":"安装调试","phase":"实施阶段","after":"t1","days":40}]}`)},
		{SectionID: p.Sections["6"], ChartType: db.ChartEquipmentTable, Title: "核心设备选型表",
			Spec: []byte(`{"columns":["序号","设备名称","品牌型号","数量"],"rows":[["1","核心交换机","华为 S7706","2"]]}`)},
	} {
		chart, err := svc.GenerateChart(ctx, db.DefaultTenant, p.TaskID, "writer", c)
		if err != nil || chart.RenderStatus != db.ChartRendered {
			t.Fatalf("GenerateChart %s: %+v %v", c.ChartType, chart, err)
		}
	}
	pre, err := d.ExportPreflightPolicy(ctx, db.DefaultTenant, p.TaskID, true)
	if err != nil || !pre.Readiness.Ready {
		t.Fatalf("strict preflight = %+v %v", pre, err)
	}
	req := finalRequest("final-e2e-1", db.ExportPDF, db.ExportDOCX)
	req.ExpectedOutlineVersion = pre.Readiness.OutlineVersion
	sub, err := svc.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", req)
	if err != nil || len(sub.Jobs) != 2 || sub.Request.Executor != OfficeLabel(office) || sub.Request.PolicyVersion != db.ExportPolicyFinal {
		t.Fatalf("SubmitExport = %+v %v", sub, err)
	}
	runAll(t, &Runner{Service: svc, Owner: "s4-e2e", Lease: 5 * time.Minute, TaskID: p.TaskID})
	docxJob, pdfJob := reloadJob(t, d, p.TaskID, sub.Jobs[0].ID), reloadJob(t, d, p.TaskID, sub.Jobs[1].ID)
	for _, j := range []*db.ExportJob{docxJob, pdfJob} {
		if j.Status != db.ExportSucceeded || j.BusinessState != db.ExportFinalReady || !j.TOCRefreshed || j.OfficeEngine != office.Name() ||
			j.OfficeVersion != office.Version() || j.Attempts != 1 {
			t.Fatalf("job %d (%s) = %s/%s toc=%v engine=%s/%s error=%s", j.ID, j.Format, j.Status, j.BusinessState, j.TOCRefreshed, j.OfficeEngine, j.OfficeVersion, j.Error)
		}
		for _, w := range j.Warnings {
			if w.Kind == db.WarnSelfCheck || w.ChartID > 0 {
				t.Errorf("job %d warning: %s", j.ID, w.Message)
			}
		}
	}
	q, err := d.GetExportRequest(ctx, db.DefaultTenant, p.TaskID, sub.Request.ID)
	if err != nil || q.BusinessState != db.ExportFinalReady {
		t.Fatalf("request = %+v %v", q, err)
	}

	// 真实回读：目录、两张渲染图、设备选型表与偏离表都在成稿中。
	tpl, _ := DefaultTemplate()
	pdf, docx := artifact(t, svc, pdfJob), artifact(t, svc, docxJob)
	rep, err := CheckPDF(pdf, tpl.Geometry)
	if err != nil || len(rep.Issues) != 0 || rep.Images != 2 || len(rep.TOC) == 0 {
		t.Fatalf("PDF readback = images %d toc %d issues %+v %v", rep.Images, len(rep.TOC), rep.Issues, err)
	}
	for _, e := range rep.TOC {
		if e.Listed != e.Actual {
			t.Fatalf("PDF TOC entry %+v", e)
		}
	}
	drep, err := CheckDOCX(ctx, docx)
	if err != nil || len(drep.Issues) != 0 || len(drep.TOC) != len(rep.TOC) {
		t.Fatalf("DOCX readback = %+v %v", drep, err)
	}
	images, err := DOCXBodyImages(docx)
	if err != nil || images != 2 {
		t.Fatalf("DOCX images = %d %v", images, err)
	}
	var equipment, deviation bool
	for _, tb := range drep.Tables {
		for _, row := range tb.Grid {
			equipment = equipment || slices.Contains(row, "核心交换机")
		}
		if len(tb.Grid) > 0 && len(tb.Grid[0]) == len(DeviationHeader) && tb.Grid[0][1] == DeviationHeader[1] {
			deviation = tb.Rows-1 == len(docxJob.Matrix)
		}
	}
	if !equipment || !deviation {
		t.Fatalf("DOCX tables: equipment=%v deviation=%v (%d tables)", equipment, deviation, len(drep.Tables))
	}
	t.Logf("final draft: PDF %d pages, %d TOC entries, %d images; DOCX %d tables, %d images; engine %s", rep.Pages, len(rep.TOC), rep.Images,
		len(drep.Tables), images, docxJob.OfficeVersion)

	// 终态后同键重发返回原请求组，不重新生成；同键异参拒绝；改变输入用新键。
	again, err := svc.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", req)
	if err != nil || !again.Replayed || again.Request.ID != sub.Request.ID || again.Jobs[0].ArtifactHash != docxJob.ArtifactHash {
		t.Fatalf("replay = %+v %v", again, err)
	}
	other := req
	other.Formats = []string{db.ExportDOCX}
	if _, err := svc.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", other); !errors.Is(err, db.ErrIdempotencyKeyConflict) {
		t.Fatalf("same key, different formats: %v", err)
	}
	if ok, _ := (&Runner{Service: svc, Owner: "idle", TaskID: p.TaskID}).RunOnce(ctx); ok {
		t.Fatal("replay queued a new export")
	}
}

func TestStrictExportRefusesDegradedRuntime(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	store := &Store{Root: t.TempDir()}
	fake := copyOffice{label: "copy 1.0（测试替身，不刷新目录）"}
	withFake := &Service{DB: d, Store: store, Renderer: stubRenderer{}, Office: fake}
	none := &Service{DB: d, Store: store, Renderer: stubRenderer{}, OfficeReason: "测试：模拟文档引擎不可用"}
	run := func(s *Service) {
		t.Helper()
		runAll(t, &Runner{Service: s, Owner: "s4", Lease: time.Minute, TaskID: p.TaskID})
	}
	failedWith := func(id int64, code, contains string) {
		t.Helper()
		j := reloadJob(t, d, p.TaskID, id)
		if j.Status != db.ExportFailed || j.ErrorCode != code || !strings.Contains(j.Error, contains) || j.ArtifactPath != "" || j.Attempts != 1 {
			t.Fatalf("job %d = %s %s attempts=%d %q", id, j.Status, j.ErrorCode, j.Attempts, j.Error)
		}
		if files := jobDirFiles(t, withFake, j); len(files) != 0 {
			t.Fatalf("job %d left files %v", id, files)
		}
	}

	// 提交时没有文档引擎：最终稿拒绝（RUNTIME_UNAVAILABLE），不降级为预览稿。
	if _, err := none.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", finalRequest("final-none", db.ExportDOCX)); !errors.Is(err, ErrOfficeUnavailable) {
		t.Fatalf("final without engine: %v", err)
	}
	// D4：新的最终稿必须带投标人名称（封面与文档属性都用它）；预览稿不要求。
	noBidder := finalRequest("final-nobidder", db.ExportDOCX)
	noBidder.Bidder = "  "
	if _, err := withFake.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", noBidder); !errors.Is(err, db.ErrInvalidExport) {
		t.Fatalf("final draft without a bidder: %v", err)
	}
	if _, err := d.FindExportRequest(ctx, db.DefaultTenant, p.TaskID, "final-nobidder"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("a rejected final draft left a request behind: %v", err)
	}
	// 请求哈希随策略版本变化；重放时按原请求的策略版本求哈希（Service.SubmitExport），策略升级不会把重发判成同键异参。
	req := finalRequest("k", db.ExportDOCX)
	if err := req.normalize(); err != nil {
		t.Fatal(err)
	}
	if req.hash(db.ExportPolicyFinalV1) == req.hash(db.ExportPolicyFinalV2) || req.hash(db.ExportPolicyFinal) != req.hash(req.ExportPolicy()) {
		t.Fatal("request hash must depend on the policy version it was submitted under")
	}
	// 执行时引擎缺失：失败而不是生成未刷新目录的 DOCX；同键重放不受当前运行能力影响。
	a, err := withFake.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", finalRequest("final-a", db.ExportDOCX))
	if err != nil || a.Request.Executor != fake.label {
		t.Fatalf("submit = %+v %v", a, err)
	}
	if r, err := none.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", finalRequest("final-a", db.ExportDOCX)); err != nil || !r.Replayed {
		t.Fatalf("replay without engine = %+v %v", r, err)
	}
	run(none)
	failedWith(a.Jobs[0].ID, db.JobErrRuntimeUnavailable, "严格最终稿要求文档引擎")

	// 执行时引擎版本与提交时固定的版本不符：失败，不换用另一个引擎。
	b, err := withFake.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", finalRequest("final-b", db.ExportDOCX))
	if err != nil {
		t.Fatal(err)
	}
	run(&Service{DB: d, Store: store, Office: copyOffice{label: "copy 2.0"}})
	failedWith(b.Jobs[0].ID, db.JobErrRuntimeUnavailable, "执行版本不随配置变化")

	// 引擎没有真正刷新目录（替身原样另存）：严格回读拒绝，DOCX 与 PDF 都不登记产物。
	c, err := withFake.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", finalRequest("final-c", db.ExportDOCX, db.ExportPDF))
	if err != nil {
		t.Fatal(err)
	}
	run(withFake)
	// （D4 起严格 DOCX 也要对照同一次排版的 PDF 核对页码：替身给不出可读的 PDF，两种格式都在回读这一步失败。）
	failedWith(c.Jobs[0].ID, db.JobErrReadbackFailed, "最终稿回读失败")
	failedWith(c.Jobs[1].ID, db.JobErrReadbackFailed, "最终稿回读失败")

	// 快照中的 PNG 被篡改（哈希不符）：最终稿失败，不插入源码占位框。
	chart, err := withFake.GenerateChart(ctx, db.DefaultTenant, p.TaskID, "writer", ChartRequest{SectionID: p.Sections["1.1"], ChartType: db.ChartArchitecture,
		Title: "系统总体架构图", Spec: []byte(`{"layers":[{"name":"平台层","nodes":["视频管理平台"]}]}`)})
	if err != nil || chart.RenderStatus != db.ChartRendered {
		t.Fatalf("chart = %+v %v", chart, err)
	}
	e, err := withFake.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", finalRequest("final-e", db.ExportDOCX))
	if err != nil {
		t.Fatal(err)
	}
	png, _ := store.Abs(chart.ArtifactPath)
	if err := os.WriteFile(png, stubPNG(10, 10, 9), 0o644); err != nil {
		t.Fatal(err)
	}
	run(withFake)
	failedWith(e.Jobs[0].ID, db.JobErrExportFailed, "内容哈希与登记不符")
	if j := reloadJob(t, d, p.TaskID, e.Jobs[0].ID); strings.Contains(j.Error, "charts/") || strings.Contains(j.Error, store.Root) {
		t.Fatalf("job error leaks the artifact path: %q", j.Error)
	}

	// 预览稿保持旧行为：无引擎时 DOCX 成功但只是 preview_only（目录未刷新），PDF 失败。
	pv, err := none.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", ExportRequest{Formats: []string{db.ExportDOCX}, RequestKey: "preview-1"})
	if err != nil || pv.Request.StrictRuntime || pv.Request.PolicyVersion != db.ExportPolicyPreviewV1 {
		t.Fatalf("preview submit = %+v %v", pv, err)
	}
	if err := os.Remove(png); err != nil { // 预览稿遇到不可读图片时仍按旧行为插入源码占位框
		t.Fatal(err)
	}
	run(none)
	if j := reloadJob(t, d, p.TaskID, pv.Jobs[0].ID); j.Status != db.ExportSucceeded || j.BusinessState != db.ExportPreviewOnly || j.TOCRefreshed || j.OfficeEngine != "" {
		t.Fatalf("preview job = %s/%s toc=%v engine=%q %s", j.Status, j.BusinessState, j.TOCRefreshed, j.OfficeEngine, j.Error)
	}
	// 最终稿遇到缺失的渲染图：失败并只说明“文件缺失”，错误里不带宿主路径。
	f, err := withFake.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", finalRequest("final-f", db.ExportDOCX))
	if err != nil {
		t.Fatal(err)
	}
	run(withFake)
	failedWith(f.Jobs[0].ID, db.JobErrExportFailed, "渲染图文件缺失")
	if j := reloadJob(t, d, p.TaskID, f.Jobs[0].ID); strings.Contains(j.Error, store.Root) || strings.Contains(j.Error, ":\\") {
		t.Fatalf("job error leaks a host path: %q", j.Error)
	}
	if _, err := none.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", ExportRequest{Formats: []string{"html"}, RequestKey: "bad"}); !errors.Is(err, db.ErrInvalidExport) {
		t.Fatalf("invalid format: %v", err)
	}
	if _, err := none.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", ExportRequest{Formats: []string{db.ExportDOCX}, Mode: "draft", RequestKey: "bad"}); !errors.Is(err, db.ErrInvalidExport) {
		t.Fatalf("invalid mode: %v", err)
	}
}
