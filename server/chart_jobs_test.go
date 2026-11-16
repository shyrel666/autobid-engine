package server

// S3 图表 Skill 的 HTTP 与引擎集成测试（需要 AUTOBID_TEST_DATABASE_URL）：
//   - /chart-jobs 提交（202）、同键重放、同键异参 / 修订已变化 409、保留键 400、兼容入口在作业活跃时 409、取消与按新请求重试、
//     渲染器不可用 503（设备选型表不受影响）；
//   - HTTP 章节 Writer：草稿随附图表计划，修订与图表作业同事务登记后由执行器渲染；无效规格时修订不保存；
//   - Worker：经 chart_render_submit 提交并订阅 → 意图等待后台作业 → 渲染完成唤醒 → 校验真实图表后 done。
// 配置了 mermaid-cli（AUTOBID_MMDC_PATH）时用真实 mmdc 渲染，否则用测试替身并在日志中注明。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

// lastToolResultText 返回末条 user 消息中第一个工具结果的正文。
func lastToolResultText(req llm.CompletionRequest) string {
	for _, b := range req.Messages[len(req.Messages)-1].Content {
		if b.Type == llm.BlockToolResult {
			return b.Text
		}
	}
	return ""
}

// chartRenderer 优先使用本机真实 mermaid-cli。
func chartRenderer(t *testing.T) export.Renderer {
	t.Helper()
	if m, err := export.DetectMMDC(); err == nil {
		t.Logf("使用真实 mermaid-cli %s", m.Version())
		return m
	}
	t.Log("mermaid-cli 未配置：使用测试替身 PNG（不作为图表运行验收）")
	return pngRenderer{}
}

func newChartJobAPI(t *testing.T, taskID int64, svc *export.Service, writer SectionWriter) (*apiClient, *export.ChartRunner) {
	t.Helper()
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(pg)
	e := NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil }, Config{Workers: 1})
	runner := &export.ChartRunner{Service: svc, Owner: "http-test", TaskID: taskID, Lease: time.Minute}
	api := testAPI(ctx, m, e, NewLLMService(pg, LLMServiceConfig{}), nil, NewHub(), APIConfig{Export: svc, SectionWriter: writer})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		e.StopAll()
		cancel()
	})
	return &apiClient{t: t, url: srv.URL}, runner
}

func drainCharts(t *testing.T, r *export.ChartRunner) {
	t.Helper()
	for i := 0; i < 20; i++ {
		ok, err := r.RunOnce(context.Background())
		if err != nil {
			t.Logf("chart runner: %v", err)
		}
		if !ok {
			return
		}
	}
	t.Fatal("chart runner did not drain")
}

type chartJobResp struct {
	Accepted bool               `json:"accepted"`
	Replayed bool               `json:"replayed"`
	Job      *db.ChartRenderJob `json:"job"`
	Chart    *db.ProposalChart  `json:"chart"`
	Error    string             `json:"error"`
	Code     string             `json:"code"`
}

var e2eArch = map[string]any{"direction": "LR", "layers": []map[string]any{
	{"name": "感知层", "nodes": []string{"高清摄像机", "门禁控制器"}}, {"name": "平台层", "nodes": []string{"视频管理平台", "统一运维平台"}}},
	"edges": []map[string]any{{"from": "高清摄像机", "to": "视频管理平台", "label": "RTSP"}}}

func TestHTTPChartJobs(t *testing.T) {
	d := dbtest.Open(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	svc := &export.Service{DB: d, Store: &export.Store{Root: t.TempDir()}, Renderer: chartRenderer(t)}
	c, runner := newChartJobAPI(t, p.TaskID, svc, nil)
	base := fmt.Sprintf("/api/tasks/%d", p.TaskID)
	sid := p.Sections["1.1"]
	det, err := d.GetSection(context.Background(), db.DefaultTenant, p.TaskID, sid)
	if err != nil {
		t.Fatal(err)
	}
	rev := det.Section.CurrentRevNo
	req := map[string]any{"section_id": sid, "expected_rev_no": rev, "chart_type": "architecture", "title": "系统总体架构图", "spec": e2eArch, "request_key": "ui-arch-1"}

	call := func(api *apiClient, method, path string, body any) (int, chartJobResp) {
		t.Helper()
		var out chartJobResp
		return api.raw(method, path, body, &out), out
	}
	if code, r := call(c, "POST", base+"/chart-jobs", map[string]any{"section_id": sid, "expected_rev_no": rev, "chart_type": "architecture", "title": "t", "spec": e2eArch}); code != 400 {
		t.Fatalf("missing request_key: %d %+v", code, r)
	}
	if code, r := call(c, "POST", base+"/chart-jobs", req); code != 202 || !r.Accepted || r.Replayed || r.Job.Status != db.ChartJobQueued ||
		r.Chart.RenderStatus != db.ChartPending || r.Chart.RevNo != rev {
		t.Fatalf("submit: %d %+v", code, r)
	}
	_, first := call(c, "POST", base+"/chart-jobs", req)
	if code, r := call(c, "POST", base+"/chart-jobs", req); code != 202 || !r.Replayed || r.Job.ID != first.Job.ID {
		t.Fatalf("replay: %d %+v", code, r)
	}
	for name, body := range map[string]map[string]any{
		"idempotency_conflict": {"section_id": sid, "expected_rev_no": rev, "chart_type": "architecture", "title": "另一个标题", "spec": e2eArch, "request_key": "ui-arch-1"},
		"stale_revision":       {"section_id": sid, "expected_rev_no": rev + 1, "chart_type": "architecture", "title": "t", "spec": e2eArch, "request_key": "ui-stale"},
	} {
		if code, r := call(c, "POST", base+"/chart-jobs", body); code != 409 || r.Code != name {
			t.Fatalf("%s: %d %+v", name, code, r)
		}
	}
	if code, r := call(c, "POST", base+"/chart-jobs", map[string]any{"section_id": sid, "expected_rev_no": rev, "chart_type": "architecture", "title": "t",
		"spec": e2eArch, "request_key": "sys:forged"}); code != 400 {
		t.Fatalf("reserved key: %d %+v", code, r)
	}
	chartPath := fmt.Sprintf("%s/charts/%d", base, first.Chart.ID)
	for _, compat := range []struct{ method, path string }{{"POST", chartPath + "/render"}, {"POST", chartPath + "/rebind"}, {"DELETE", chartPath}} {
		if code, r := call(c, compat.method, compat.path, nil); code != 409 || r.Code != "chart_job_active" {
			t.Fatalf("%s %s with an active job: %d %+v", compat.method, compat.path, code, r)
		}
	}
	var list struct {
		Jobs []*db.ChartRenderJob `json:"jobs"`
	}
	if code := c.raw("GET", fmt.Sprintf("%s/chart-jobs?section_id=%d", base, sid), nil, &list); code != 200 || len(list.Jobs) != 1 || list.Jobs[0].ID != first.Job.ID {
		t.Fatalf("list: %d %+v", code, list)
	}

	drainCharts(t, runner)
	if code, r := call(c, "GET", fmt.Sprintf("%s/chart-jobs/%d", base, first.Job.ID), nil); code != 200 || r.Job.Status != db.ChartJobSucceeded ||
		r.Chart.RenderStatus != db.ChartRendered || r.Chart.WidthPx == 0 {
		t.Fatalf("after run: %d %+v", code, r)
	}
	if code, hdr, data := c.download(chartPath + "/image"); code != 200 || hdr.Get("Content-Type") != "image/png" || len(data) < 100 {
		t.Fatalf("chart image: %d %d bytes", code, len(data))
	}

	// 取消后迟到的执行不恢复成功；重试按新请求语义提交（新请求键、同一图表）。
	gantt := map[string]any{"section_id": sid, "expected_rev_no": rev, "chart_type": "gantt", "title": "实施进度计划", "request_key": "ui-gantt-1",
		"spec": map[string]any{"start": "2026-11-02", "tasks": []map[string]any{{"id": "a", "name": "需求调研", "days": 10}, {"name": "安装调试", "after": "a", "days": 30}}}}
	code, g := call(c, "POST", base+"/chart-jobs", gantt)
	if code != 202 {
		t.Fatalf("gantt submit: %d %+v", code, g)
	}
	if code, r := call(c, "POST", fmt.Sprintf("%s/chart-jobs/%d/cancel", base, g.Job.ID), nil); code != 200 || r.Job.Status != db.ChartJobCancelled {
		t.Fatalf("cancel: %d %+v", code, r)
	}
	drainCharts(t, runner)
	if code, r := call(c, "POST", fmt.Sprintf("%s/chart-jobs/%d/cancel", base, g.Job.ID), nil); code != 200 || r.Job.Status != db.ChartJobCancelled ||
		r.Chart.RenderStatus != db.ChartPending {
		t.Fatalf("cancelled job after a runner pass: %d %+v", code, r)
	}
	code, retry := call(c, "POST", base+"/chart-jobs", map[string]any{"chart_id": g.Chart.ID, "request_key": "ui-gantt-1-retry"})
	if code != 202 || retry.Job.ID == g.Job.ID || retry.Job.ChartID != g.Chart.ID {
		t.Fatalf("retry: %d %+v", code, retry)
	}
	drainCharts(t, runner)
	if code, r := call(c, "GET", fmt.Sprintf("%s/chart-jobs/%d", base, retry.Job.ID), nil); code != 200 || r.Chart.RenderStatus != db.ChartRendered {
		t.Fatalf("retried gantt: %d %+v", code, r)
	}

	// 渲染器不可用：Mermaid 新请求 503，设备选型表照常受理。
	offline := &export.Service{DB: d, Store: svc.Store, RendererReason: "测试：mmdc 未安装"}
	oc, _ := newChartJobAPI(t, p.TaskID, offline, nil)
	arch := map[string]any{"section_id": sid, "expected_rev_no": rev, "chart_type": "architecture", "title": "t", "spec": e2eArch, "request_key": "offline-arch"}
	if code, r := call(oc, "POST", base+"/chart-jobs", arch); code != 503 || r.Code != "runtime_unavailable" {
		t.Fatalf("mermaid without a renderer: %d %+v", code, r)
	}
	table := map[string]any{"section_id": sid, "expected_rev_no": rev, "chart_type": "equipment_table", "title": "核心设备选型表", "request_key": "offline-table",
		"spec": map[string]any{"columns": []string{"设备", "数量"}, "rows": [][]string{{"核心交换机", "2"}}}}
	if code, r := call(oc, "POST", base+"/chart-jobs", table); code != 202 || r.Job.Executor != export.ExecutorNative {
		t.Fatalf("table without a renderer: %d %+v", code, r)
	}
}

// planWriter 返回带图表计划的章节草稿。
type planWriter struct {
	charts []export.ChartPlan
}

func (w *planWriter) WriteSection(_ context.Context, _ *Task, sc *bidtools.SectionContext, _ string) (*bidtools.SectionDraft, error) {
	return &bidtools.SectionDraft{ContentMD: "实施进度安排：工期 90 天，分三个阶段完成，进度见下图。\n\n" + strings.Repeat("本方案按计划组织实施。", 20),
		SummaryNote: "实施进度摘要", DiffSummary: "补充进度图", Charts: w.charts}, nil
}

func TestHTTPSectionWriterChartPlans(t *testing.T) {
	d := dbtest.Open(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	svc := &export.Service{DB: d, Store: &export.Store{Root: t.TempDir()}, Renderer: chartRenderer(t)}
	writer := &planWriter{charts: []export.ChartPlan{
		{Code: "C1", Title: "项目实施进度计划", ChartType: db.ChartGantt, Spec: json.RawMessage(`{"start":"2026-11-02","tasks":[{"id":"a","name":"需求调研","phase":"准备阶段","days":10},{"name":"安装调试","phase":"实施阶段","after":"a","days":30}]}`)},
		{Code: "C2", Title: "核心设备选型表", ChartType: db.ChartEquipmentTable, Spec: json.RawMessage(`{"columns":["序号","设备名称","数量"],"rows":[["1","核心交换机","2"]]}`)},
	}}
	c, runner := newChartJobAPI(t, p.TaskID, svc, writer)
	base := fmt.Sprintf("/api/tasks/%d", p.TaskID)
	sid := p.Sections["5"]
	waitJob := func(id int64) *db.SectionWriteJob {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var jobs []*db.SectionWriteJob
			c.raw("GET", base+"/section-jobs", nil, &jobs)
			for _, j := range jobs {
				if j.ID == id && j.Status != "queued" && j.Status != "running" {
					return j
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("section job %d did not finish", id)
		return nil
	}

	var job db.SectionWriteJob
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/dispatch", base, sid), map[string]any{"instruction": "补充实施进度图"}, &job); code != 202 {
		t.Fatalf("dispatch: %d", code)
	}
	done := waitJob(job.ID)
	if done.Status != "awaiting_review" || done.ResultRevNo == nil {
		t.Fatalf("section job: %+v", done)
	}
	var charts struct {
		Charts []*db.ProposalChart `json:"charts"`
	}
	c.raw("GET", fmt.Sprintf("%s/charts?section_id=%d", base, sid), nil, &charts)
	if len(charts.Charts) != 2 {
		t.Fatalf("plan charts = %d", len(charts.Charts))
	}
	for _, ch := range charts.Charts {
		if ch.RevNo != *done.ResultRevNo || ch.PlanCode == "" || !ch.LatestJob.Active() || ch.Stale {
			t.Fatalf("plan chart before render: %+v", ch)
		}
	}
	drainCharts(t, runner)
	c.raw("GET", fmt.Sprintf("%s/charts?section_id=%d", base, sid), nil, &charts)
	for _, ch := range charts.Charts {
		if ch.RenderStatus != db.ChartRendered || ch.LatestJob.Status != db.ChartJobSucceeded {
			t.Fatalf("plan chart after render: %+v", ch)
		}
		t.Logf("%s %s《%s》%s %dx%d", ch.PlanCode, ch.ChartType, ch.Title, ch.RenderFormat, ch.WidthPx, ch.HeightPx)
	}

	// 规格无效：整份草稿不保存，作业以原因阻塞，图表不增加。
	writer.charts = []export.ChartPlan{{Code: "C1", Title: "进度", ChartType: db.ChartGantt, Spec: json.RawMessage(`{"tasks":[{"name":"实施","after":"missing","days":5}]}`)}}
	if code := c.raw("POST", fmt.Sprintf("%s/sections/%d/dispatch", base, sid), map[string]any{"instruction": "再改一版"}, &job); code != 202 {
		t.Fatalf("dispatch 2: %d", code)
	}
	if bad := waitJob(job.ID); bad.Status != "blocked" || !strings.Contains(bad.Error, "图表计划无效") {
		t.Fatalf("invalid plan job: %+v", bad)
	}
	det, _ := d.GetSection(context.Background(), db.DefaultTenant, p.TaskID, sid)
	if det.Section.CurrentRevNo != *done.ResultRevNo {
		t.Fatalf("revision saved despite an invalid chart plan: rev %d", det.Section.CurrentRevNo)
	}
	c.raw("GET", fmt.Sprintf("%s/charts?section_id=%d", base, sid), nil, &charts)
	if len(charts.Charts) != 2 {
		t.Fatalf("charts after invalid plan = %d", len(charts.Charts))
	}
}

// Worker 路径：chart_render_submit 提交并订阅 → 等待 → 渲染完成唤醒 → 必需作业（chart_render）校验通过后 done。
func TestEngineChartRenderSubmitWaitsForRealChart(t *testing.T) {
	pg := testDB(t)
	svc := &export.Service{DB: pg, Store: &export.Store{Root: t.TempDir()}, Renderer: chartRenderer(t)}
	var (
		envRef    atomic.Pointer[harnessEnv]
		proposal  *dbtest.Proposal
		rev       int
		plannerMu sync.Mutex
		seen      []string
	)
	submit := tool.New(tool.Spec{Name: bidtools.ToolChartRenderSubmit, Description: "提交章节图表渲染作业"},
		func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			task := envRef.Load().task
			for _, tl := range bidtools.New(pg, task.ID, task.Assets, task.Store).WithCharts(svc).ChartJobTools() {
				if tl.Name() == bidtools.ToolChartRenderSubmit {
					return tl.Call(ctx, in, tc)
				}
			}
			return tool.Errorf("chart_render_submit not registered"), nil
		})
	planner := func(env *harnessEnv) brain {
		envRef.Store(env)
		return func(req llm.CompletionRequest) llm.Message {
			plannerMu.Lock()
			seen = append(seen, lastUserText(req))
			plannerMu.Unlock()
			if _, ok := lastToolResult(req); ok {
				return say("本轮规划完成")
			}
			if ns, _ := env.task.Store.NodesByKind(context.Background(), db.KindIntent, nil, 1); len(ns) == 0 {
				return use(agent.ToolAddIntent, map[string]any{"summary": "为第 5 章实施进度计划生成甘特图",
					"mandate": map[string]any{"require_writeback": true, "allowed_job_kinds": []string{db.JobKindChartRender},
						"required_job_kinds": []string{db.JobKindChartRender}}})
			}
			return say("暂无新方向")
		}
	}
	var lastResult atomic.Value
	worker := brain(func(req llm.CompletionRequest) llm.Message {
		prev, isResult := lastToolResult(req)
		switch {
		case strings.Contains(lastUserText(req), "后台作业事件 #"):
			return say("甘特图已渲染并绑定章节当前修订，结果已核对")
		case isResult && prev == bidtools.ToolChartRenderSubmit:
			lastResult.Store(lastToolResultText(req))
			return say("已提交图表渲染作业，等待后台处理")
		case isResult:
			return say("工具已返回")
		default:
			return use(bidtools.ToolChartRenderSubmit, map[string]any{"section_code": "5", "expected_rev_no": rev, "chart_type": "gantt",
				"title": "项目实施进度计划", "request_key": "agent-gantt-5", "spec": map[string]any{"start": "2026-11-02", "tasks": []map[string]any{
					{"id": "a", "name": "需求调研与深化设计", "phase": "准备阶段", "days": 10}, {"name": "安装调试与联调", "phase": "实施阶段", "after": "a", "days": 30}}}})
		}
	})
	env := newEnvWith(t, 0, false, worker, Config{JobWakePoll: 100 * time.Millisecond}, envOpts{extraTools: []tool.CoreTool{submit}, planner: planner,
		legacyWriting: true, beforeRun: func(task *Task) {
			proposal = dbtest.ReadyTask(t, pg, task.ID, dbtest.DefaultSpec(0))
			det, err := pg.GetSection(context.Background(), db.DefaultTenant, task.ID, proposal.Sections["5"])
			if err != nil {
				t.Fatal(err)
			}
			rev = det.Section.CurrentRevNo
		}})
	ctx := context.Background()

	var in *db.Node
	waitFor(t, 15*time.Second, "intent waiting for its chart job", func() bool {
		ns, _ := env.task.Store.NodesByKind(ctx, db.KindIntent, nil, 1)
		if len(ns) == 1 && db.IsWaitingIntent(ns[0]) {
			in = ns[0]
		}
		return in != nil
	})
	if s, _ := lastResult.Load().(string); !strings.Contains(s, `"accepted":true`) || !strings.Contains(s, `"kind":"chart_render"`) {
		t.Fatalf("submit result: %s", s)
	}
	runner := &export.ChartRunner{Service: svc, Owner: "engine-test", TaskID: env.task.ID, Poll: 100 * time.Millisecond,
		OnChange: func(*db.ChartRenderJob, string) { env.e.WakeAgentJobs() }}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	runner.Start(runCtx)
	waitFor(t, 60*time.Second, "intent done after the chart rendered", func() bool {
		n, _ := env.task.Store.GetNode(ctx, in.ID)
		return n != nil && n.State == db.StateIntentDone
	})
	subs, err := pg.ListAgentJobs(ctx, db.DefaultTenant, env.task.ID, db.AgentJobFilter{IntentID: in.ID})
	if err != nil || len(subs) != 1 || subs[0].Job.Kind != db.JobKindChartRender || subs[0].Event == nil ||
		subs[0].Event.BusinessState != "chart_ready" || subs[0].State != "acknowledged" {
		t.Fatalf("subscription: %+v %v", subs, err)
	}
	j, c, err := pg.GetChartRenderJob(ctx, db.DefaultTenant, env.task.ID, subs[0].Job.ID)
	if err != nil || j.Status != db.ChartJobSucceeded || c.RenderStatus != db.ChartRendered || c.RevNo != rev || c.SectionID != proposal.Sections["5"] || c.CreatedBy != "worker" {
		t.Fatalf("chart: %+v %+v %v", j, c, err)
	}
	t.Logf("Worker 图表 %s《%s》%dx%d via %s", c.ChartType, c.Title, c.WidthPx, c.HeightPx, j.Executor)
	plannerMu.Lock()
	defer plannerMu.Unlock()
	for _, s := range seen {
		if strings.Contains(s, fmt.Sprintf("意图 #%d 已结束（blocked", in.ID)) {
			t.Fatalf("planner told a waiting intent had ended:\n%s", s)
		}
	}
}
