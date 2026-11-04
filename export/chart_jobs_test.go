package export

// S3 图表渲染作业的服务层与执行器（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：提交幂等与运行能力门禁、
// 可重试失败、原生设备表、执行版本固定、租约丢失清理本次产物、图表计划校验；真实 mermaid-cli 用例在未配置 mmdc 时跳过。
// 替身渲染器只验证框架，不作为图表运行验收。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
)

// flakyRenderer 前 fails 次渲染失败，之后返回替身 PNG。
type flakyRenderer struct {
	fails atomic.Int32
	calls atomic.Int32
}

func (*flakyRenderer) Name() string { return "stub-flaky（测试替身）" }
func (f *flakyRenderer) RenderPNG(context.Context, string) ([]byte, error) {
	f.calls.Add(1)
	if f.fails.Add(-1) >= 0 {
		return nil, errors.New("chrome crashed")
	}
	return stubPNG(900, 500, 1), nil
}

// hookRenderer 在返回 PNG 前执行 hook（用于在渲染与写回之间注入取消）。
type hookRenderer struct{ hook func() }

func (hookRenderer) Name() string { return "stub-hook（测试替身）" }
func (h hookRenderer) RenderPNG(context.Context, string) ([]byte, error) {
	h.hook()
	return stubPNG(400, 300, 2), nil
}

const archSpec = `{"direction":"TB","layers":[{"name":"感知层","nodes":["高清摄像机","门禁控制器"]},{"name":"平台层","nodes":["视频管理平台"]}],
	"edges":[{"from":"高清摄像机","to":"视频管理平台","label":"RTSP"}]}`

func chartTask(t *testing.T) (*db.DB, *dbtest.Proposal, int64, int) {
	t.Helper()
	d := dbtest.Open(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	sid := p.Sections["1.1"]
	det, err := d.GetSection(context.Background(), db.DefaultTenant, p.TaskID, sid)
	if err != nil {
		t.Fatal(err)
	}
	return d, p, sid, det.Section.CurrentRevNo
}

func runChartJobs(t *testing.T, r *ChartRunner) {
	t.Helper()
	for i := 0; i < 20; i++ {
		ok, err := r.RunOnce(context.Background())
		if err != nil {
			t.Logf("RunOnce: %v", err)
		}
		if !ok {
			return
		}
	}
	t.Fatal("chart runner did not drain")
}

func TestChartRunnerRetriesRendersAndPinsExecutor(t *testing.T) {
	d, p, sid, rev := chartTask(t)
	ctx := context.Background()
	flaky := &flakyRenderer{}
	flaky.fails.Store(1)
	var woken atomic.Int32
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, Renderer: flaky, ChartWake: func() { woken.Add(1) }}
	r := &ChartRunner{Service: svc, Owner: "test-charts", TaskID: p.TaskID, Lease: time.Minute}

	req := ChartRenderRequest{SectionID: sid, ExpectedRevNo: rev, ChartType: db.ChartArchitecture, Title: " 系统总体架构图 ", Spec: json.RawMessage(archSpec), RequestKey: "arch-1"}
	sub, err := svc.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", req)
	if err != nil || sub.Replayed || sub.Job.Executor != RendererLabel(flaky) || woken.Load() != 1 {
		t.Fatalf("submit: %+v %v (woken %d)", sub, err, woken.Load())
	}
	// 规格的空白与字段顺序不同，规范化后是同一请求。
	same := req
	same.Title, same.Spec = "系统总体架构图", json.RawMessage(`{"layers":[{"name":"感知层","nodes":["高清摄像机","门禁控制器"]},{"name":"平台层","nodes":["视频管理平台"]}],
		"direction":"TB","edges":[{"label":"RTSP","from":"高清摄像机","to":"视频管理平台"}]}`)
	if again, err := svc.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", same); err != nil || !again.Replayed || again.Job.ID != sub.Job.ID {
		t.Fatalf("normalized replay: %+v %v", again, err)
	}
	changed := req
	changed.Title = "另一张图"
	if _, err := svc.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", changed); !errors.Is(err, db.ErrIdempotencyKeyConflict) {
		t.Fatalf("same key with a different title: %v", err)
	}

	runChartJobs(t, r)
	if flaky.calls.Load() != 2 {
		t.Fatalf("renderer calls = %d, want a retry after the first failure", flaky.calls.Load())
	}
	j, c, err := d.GetChartRenderJob(ctx, db.DefaultTenant, p.TaskID, sub.Job.ID)
	if err != nil || j.Status != db.ChartJobSucceeded || j.Attempts != 2 || c.RenderStatus != db.ChartRendered || c.RenderFormat != db.ChartFormatPNG ||
		c.WidthPx != 900 || c.HeightPx != 500 || c.RevNo != rev {
		t.Fatalf("after run: %+v %+v %v", j, c, err)
	}
	if !strings.Contains(c.ArtifactPath, fmt.Sprintf("/job-%d/t", j.ID)) {
		t.Fatalf("artifact is not isolated per job and attempt: %s", c.ArtifactPath)
	}
	png, err := svc.ChartImage(c)
	if err != nil || sha256Hex(png) != c.ArtifactHash {
		t.Fatalf("chart image: %v", err)
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(png)); err != nil || cfg.Width != 900 {
		t.Fatalf("artifact is not the rendered PNG: %v", err)
	}

	// 渲染器不可用：Mermaid 新请求被拒绝，设备选型表照常原生生成；同键重放不受当前运行能力影响。
	offline := &Service{DB: d, Store: svc.Store, RendererReason: "测试：mmdc 未安装"}
	if again, err := offline.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", req); err != nil || !again.Replayed {
		t.Fatalf("replay with the renderer offline: %+v %v", again, err)
	}
	mermaid := req
	mermaid.RequestKey = "arch-offline"
	if _, err := offline.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", mermaid); !errors.Is(err, ErrRendererMissing) {
		t.Fatalf("mermaid submit without a renderer: %v", err)
	}
	table, err := offline.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", ChartRenderRequest{SectionID: sid, ExpectedRevNo: rev,
		ChartType: db.ChartEquipmentTable, Title: "核心设备选型表", Spec: json.RawMessage(`{"columns":["序号","设备名称"],"rows":[["1","核心交换机"]]}`), RequestKey: "table-1"})
	if err != nil || table.Job.Executor != ExecutorNative {
		t.Fatalf("table submit without a renderer: %+v %v", table, err)
	}
	runChartJobs(t, &ChartRunner{Service: offline, Owner: "offline", TaskID: p.TaskID})
	if _, c, _ := d.GetChartRenderJob(ctx, db.DefaultTenant, p.TaskID, table.Job.ID); c.RenderStatus != db.ChartRendered || c.RenderFormat != db.ChartFormatNative {
		t.Fatalf("native table = %+v", c)
	}

	// 执行版本在首次提交时固定：换了渲染器的执行者不渲染，作业以 RUNTIME_UNAVAILABLE 失败、图表标记未渲染。
	pinned, err := svc.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", ChartRenderRequest{SectionID: sid, ExpectedRevNo: rev,
		ChartType: db.ChartArchitecture, Title: "部署拓扑", Spec: json.RawMessage(archSpec), RequestKey: "arch-pinned"})
	if err != nil {
		t.Fatal(err)
	}
	other := &Service{DB: d, Store: svc.Store, Renderer: stubRenderer{}}
	runChartJobs(t, &ChartRunner{Service: other, Owner: "other", TaskID: p.TaskID})
	j, c, _ = d.GetChartRenderJob(ctx, db.DefaultTenant, p.TaskID, pinned.Job.ID)
	if j.Status != db.ChartJobFailed || j.ErrorCode != db.JobErrRuntimeUnavailable || c.RenderStatus != db.ChartUnrendered {
		t.Fatalf("executor mismatch: %+v %+v", j, c)
	}
	// 重试按新请求语义提交，固定当前执行版本。
	retry, err := other.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", ChartRenderRequest{ChartID: c.ID, RequestKey: "arch-pinned-retry"})
	if err != nil || retry.Job.Executor != RendererLabel(stubRenderer{}) {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	runChartJobs(t, &ChartRunner{Service: other, Owner: "other", TaskID: p.TaskID})
	if _, c, _ := d.GetChartRenderJob(ctx, db.DefaultTenant, p.TaskID, retry.Job.ID); c.RenderStatus != db.ChartRendered {
		t.Fatalf("retried chart = %+v", c)
	}
	// 兼容的同步渲染入口在作业终态后照常可用。
	if _, err := other.RenderChart(ctx, db.DefaultTenant, p.TaskID, c, "writer"); err != nil {
		t.Fatalf("sync render after the job ended: %v", err)
	}
}

func TestChartRunnerLeaseLostRemovesAttempt(t *testing.T) {
	d, p, sid, rev := chartTask(t)
	ctx := context.Background()
	var jobID atomic.Int64
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}}
	svc.Renderer = hookRenderer{hook: func() { // 渲染完成、写回之前被取消
		if _, err := d.CancelChartRenderJob(ctx, db.DefaultTenant, p.TaskID, jobID.Load(), "operator"); err != nil {
			t.Error(err)
		}
	}}
	sub, err := svc.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", ChartRenderRequest{SectionID: sid, ExpectedRevNo: rev,
		ChartType: db.ChartArchitecture, Title: "取消演练", Spec: json.RawMessage(archSpec), RequestKey: "cancel-race"})
	if err != nil {
		t.Fatal(err)
	}
	jobID.Store(sub.Job.ID)
	r := &ChartRunner{Service: svc, Owner: "test", TaskID: p.TaskID, Lease: time.Minute}
	if ok, err := r.RunOnce(ctx); !ok || !errors.Is(err, db.ErrLeaseLost) {
		t.Fatalf("late success after cancel: %v %v", ok, err)
	}
	j, c, _ := d.GetChartRenderJob(ctx, db.DefaultTenant, p.TaskID, sub.Job.ID)
	if j.Status != db.ChartJobCancelled || c.RenderStatus != db.ChartPending || c.ArtifactPath != "" {
		t.Fatalf("cancelled job restored a result: %+v %+v", j, c)
	}
	dir, _ := svc.Store.Abs(svc.Store.chartJobDir(db.DefaultTenant, p.TaskID, j.ID))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("orphan attempt files left: %v", entries)
	}
}

func TestBuildChartPlansValidatesBeforeSave(t *testing.T) {
	var nilSvc *Service
	batch, err := nilSvc.BuildChartPlans([]ChartPlan{
		{Code: "C1", Title: "总体架构", ChartType: db.ChartArchitecture, Spec: json.RawMessage(archSpec)},
		{Code: "C2", Title: "设备", ChartType: db.ChartEquipmentTable, Spec: json.RawMessage(`{"columns":["a"],"rows":[["1"]]}`)},
	})
	if err != nil || len(batch.Plans) != 2 || batch.Plans[0].Executor != "" || batch.Plans[1].Executor != ExecutorNative || batch.Deadline != ChartJobDeadline() {
		t.Fatalf("plans: %+v %v", batch, err)
	}
	if b, err := nilSvc.BuildChartPlans(nil); b != nil || err != nil {
		t.Fatalf("no plans: %+v %v", b, err)
	}
	if _, err := nilSvc.BuildChartPlans([]ChartPlan{{Code: "C1", Title: "组织", ChartType: db.ChartOrg, Spec: json.RawMessage(`{"nodes":[{"id":"a","name":"A","parent":"x"}]}`)}}); !errors.Is(err, ErrChartSpec) {
		t.Fatalf("invalid spec accepted: %v", err)
	}
	if _, err := nilSvc.BuildChartPlans([]ChartPlan{{Code: "C1", Title: "a", ChartType: db.ChartArchitecture, Spec: json.RawMessage(archSpec)},
		{Code: "C1", Title: "b", ChartType: db.ChartArchitecture, Spec: json.RawMessage(archSpec)}}); !errors.Is(err, db.ErrInvalidChart) {
		t.Fatalf("duplicate codes accepted: %v", err)
	}
	many := make([]ChartPlan, db.MaxChartPlans+1)
	for i := range many {
		many[i] = ChartPlan{Code: fmt.Sprintf("C%d", i), Title: "t", ChartType: db.ChartArchitecture, Spec: json.RawMessage(archSpec)}
	}
	if _, err := nilSvc.BuildChartPlans(many); !errors.Is(err, ErrChartSpec) {
		t.Fatalf("too many plans accepted: %v", err)
	}
}

// 真实 mermaid-cli：中文架构图与甘特图经作业渲染为 PNG 并绑定章节当前修订（未配置 mmdc 时跳过）。
func TestChartRunnerRealMMDC(t *testing.T) {
	m, err := DetectMMDC()
	if err != nil {
		t.Skipf("依赖缺失：mermaid-cli（mmdc）未安装或 AUTOBID_MMDC_PATH 未设置：%v", err)
	}
	d, p, sid, rev := chartTask(t)
	ctx := context.Background()
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, Renderer: m}
	specs := []ChartRenderRequest{
		{ChartType: db.ChartArchitecture, Title: "智慧园区安防系统总体架构图", Spec: json.RawMessage(archSpec), RequestKey: "real-arch"},
		{ChartType: db.ChartGantt, Title: "项目实施进度计划", RequestKey: "real-gantt", Spec: json.RawMessage(`{"start":"2026-11-02","tasks":[
			{"id":"a","name":"需求调研与深化设计","phase":"准备阶段","days":10},{"id":"b","name":"设备采购与到货验收","phase":"准备阶段","after":"a","days":20},
			{"name":"安装调试与系统联调","phase":"实施阶段","after":"b","days":30},{"name":"竣工验收","phase":"验收阶段","days":5,"milestone":true,"start":"2026-12-31"}]}`)},
	}
	var ids []int64
	for _, s := range specs {
		s.SectionID, s.ExpectedRevNo = sid, rev
		sub, err := svc.SubmitChartRender(ctx, db.DefaultTenant, p.TaskID, "writer", s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(sub.Job.Executor, "mmdc ") {
			t.Fatalf("executor = %q", sub.Job.Executor)
		}
		ids = append(ids, sub.Job.ID)
	}
	runChartJobs(t, &ChartRunner{Service: svc, Owner: "real-mmdc", TaskID: p.TaskID})
	for _, id := range ids {
		j, c, err := d.GetChartRenderJob(ctx, db.DefaultTenant, p.TaskID, id)
		if err != nil || j.Status != db.ChartJobSucceeded || c.RenderStatus != db.ChartRendered || c.RevNo != rev || c.Stale {
			t.Fatalf("real render: %+v %+v %v", j, c, err)
		}
		png, err := svc.ChartImage(c)
		if err != nil {
			t.Fatal(err)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(png))
		if err != nil || cfg.Width < 300 || cfg.Height < 100 || cfg.Width != c.WidthPx || cfg.Height != c.HeightPx {
			t.Fatalf("%s PNG %dx%d (recorded %dx%d): %v", c.ChartType, cfg.Width, cfg.Height, c.WidthPx, c.HeightPx, err)
		}
		t.Logf("%s《%s》%dx%d %d bytes via %s", c.ChartType, c.Title, cfg.Width, cfg.Height, len(png), j.Executor)
	}
}
