package server

// S4 合稿 Skill 的 HTTP 与引擎集成测试（需要 AUTOBID_TEST_DATABASE_URL 与本机文档引擎 WPS / Word，未安装时跳过）：
//   - /exports 带 request_key 的请求组（202）、同键重放、Idempotency-Key 头、同键异参 / 大纲版本已变化 409、保留键与无键最终稿 400、
//     旧调用兼容（201，预览稿）、/export-requests 列表 / 详情、真实刷新后的最终稿下载头、整组与单作业取消、暂停时拒绝新请求、
//     文档引擎不可用时最终稿 503（同键重放不受影响）且 preflight?mode=final 显示阻断；
//   - Worker：经 proposal_export_submit 提交并订阅 → 意图等待后台作业 → 真实文档引擎生成最终稿后唤醒 →
//     必需作业（proposal_export）校验严格最终稿后 done，Planner 未被告知“已结束”。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	"github.com/autobid/autobid-engine/export/officetest"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

// requireOfficeEngine 先排队再探测：与 export 包的真实引擎用例跨包串行，见 export/officetest。
func requireOfficeEngine(t *testing.T) export.Office {
	t.Helper()
	officetest.Hold(t)
	o, err := export.DetectOffice(context.Background(), export.OfficeOptions{Engine: os.Getenv("AUTOBID_OFFICE_ENGINE"), Timeout: 3 * time.Minute})
	if err != nil {
		t.Skipf("依赖缺失：文档引擎（WPS / Word）不可用，无法验证最终稿：%v", err)
	}
	return o
}

func newExportRequestAPI(t *testing.T, svc *export.Service, runner *export.Runner) *apiClient {
	t.Helper()
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(pg)
	e := NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil }, Config{Workers: 1})
	api := NewAPI(ctx, m, e, NewLLMService(pg, LLMServiceConfig{}), nil, NewHub(), APIConfig{Token: testToken, Export: svc, ExportRunner: runner})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		e.StopAll()
		cancel()
	})
	return &apiClient{t: t, url: srv.URL}
}

type exportSubmitResp struct {
	Accepted bool                    `json:"accepted"`
	Replayed bool                    `json:"replayed"`
	Request  *db.ExportRequest       `json:"request"`
	Jobs     []*db.ExportJob         `json:"jobs"`
	Results  []db.ExportCancelResult `json:"results"`
	Error    string                  `json:"error"`
	Code     string                  `json:"code"`
}

func drainExports(t *testing.T, r *export.Runner) {
	t.Helper()
	for i := 0; i < 20; i++ {
		ok, err := r.RunOnce(context.Background())
		if err != nil {
			t.Logf("export runner: %v", err)
		}
		if !ok {
			return
		}
	}
	t.Fatal("export runner did not drain")
}

func TestHTTPExportRequests(t *testing.T) {
	d := dbtest.Open(t)
	office := requireOfficeEngine(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	ctx := context.Background()
	svc := &export.Service{DB: d, Store: &export.Store{Root: t.TempDir()}, Office: office}
	runner := &export.Runner{Service: svc, Owner: "http-s4", TaskID: p.TaskID, Lease: 5 * time.Minute}
	c := newExportRequestAPI(t, svc, runner)
	base := fmt.Sprintf("/api/tasks/%d", p.TaskID)
	call := func(api *apiClient, method, path string, body any) (int, exportSubmitResp) {
		t.Helper()
		var out exportSubmitResp
		return api.raw(method, path, body, &out), out
	}

	var pre struct {
		Readiness     db.ExportReadiness `json:"readiness"`
		StrictRuntime bool               `json:"strict_runtime"`
		PolicyVersion string             `json:"policy_version"`
	}
	if code := c.raw("GET", base+"/exports/preflight?mode=final", nil, &pre); code != 200 || !pre.Readiness.Ready || !pre.StrictRuntime || pre.PolicyVersion != db.ExportPolicyFinal {
		t.Fatalf("final preflight: %d %+v", code, pre)
	}
	if code, r := call(c, "GET", base+"/exports/preflight?mode=draft", nil); code != 400 {
		t.Fatalf("bad mode: %d %+v", code, r)
	}
	if code, r := call(c, "POST", base+"/exports", map[string]any{"formats": []string{"docx"}, "mode": "final"}); code != 400 {
		t.Fatalf("final without request_key: %d %+v", code, r)
	}
	// D4：最终稿必须带投标人名称，不登记请求组。
	if code, r := call(c, "POST", base+"/exports", map[string]any{"formats": []string{"docx"}, "mode": "final", "request_key": "ui-nobidder"}); code != 400 || r.Code != "invalid_export" {
		t.Fatalf("final without a bidder: %d %+v", code, r)
	}
	// 旧调用（不带请求键）保持兼容：201、预览稿。
	var legacy struct {
		Jobs []*db.ExportJob `json:"jobs"`
	}
	if code := c.raw("POST", base+"/exports", map[string]any{"formats": []string{"docx"}}, &legacy); code != 201 || len(legacy.Jobs) != 1 || legacy.Jobs[0].StrictRuntime {
		t.Fatalf("legacy export: %d %+v", code, legacy)
	}
	if code, r := call(c, "POST", fmt.Sprintf("%s/exports/%d/cancel", base, legacy.Jobs[0].ID), nil); code != 200 {
		t.Fatalf("cancel legacy: %d %+v", code, r)
	}

	final := map[string]any{"formats": []string{"pdf", "docx"}, "mode": "final", "bidder": "示例科技有限公司", "request_key": "ui-final-1",
		"expected_outline_version": pre.Readiness.OutlineVersion}
	code, first := call(c, "POST", base+"/exports", final)
	if code != 202 || !first.Accepted || first.Replayed || !first.Request.StrictRuntime || first.Request.Executor != export.OfficeLabel(office) || len(first.Jobs) != 2 {
		t.Fatalf("final submit: %d %+v", code, first)
	}
	if code, r := call(c, "POST", base+"/exports", final); code != 202 || !r.Replayed || r.Request.ID != first.Request.ID || r.Jobs[0].ID != first.Jobs[0].ID {
		t.Fatalf("replay: %d %+v", code, r)
	}
	for name, body := range map[string]map[string]any{
		"idempotency_conflict": {"formats": []string{"docx"}, "mode": "final", "request_key": "ui-final-1", "expected_outline_version": pre.Readiness.OutlineVersion},
		"stale_revision":       {"formats": []string{"docx"}, "mode": "final", "bidder": "示例科技有限公司", "request_key": "ui-stale", "expected_outline_version": pre.Readiness.OutlineVersion + 1},
	} {
		if code, r := call(c, "POST", base+"/exports", body); code != 409 || r.Code != name {
			t.Fatalf("%s: %d %+v", name, code, r)
		}
	}
	if code, r := call(c, "POST", base+"/exports", map[string]any{"formats": []string{"docx"}, "request_key": "sys:forged"}); code != 400 {
		t.Fatalf("reserved key: %d %+v", code, r)
	}
	// Idempotency-Key 头与请求体中的 request_key 等价。
	headerKey := func() (int, exportSubmitResp) {
		b, _ := json.Marshal(map[string]any{"formats": []string{"docx"}})
		req, _ := http.NewRequest("POST", c.url+base+"/exports", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Idempotency-Key", "hdr-preview-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out exportSubmitResp
		_ = json.Unmarshal(data, &out)
		return resp.StatusCode, out
	}
	hc, hdr := headerKey()
	if hc != 202 || hdr.Request.RequestKey != "hdr-preview-1" || hdr.Request.StrictRuntime {
		t.Fatalf("Idempotency-Key header: %d %+v", hc, hdr)
	}
	if hc, again := headerKey(); hc != 202 || !again.Replayed || again.Request.ID != hdr.Request.ID {
		t.Fatalf("Idempotency-Key replay: %d %+v", hc, again)
	}
	// 整组取消；再次取消单个作业原样返回。
	if code, r := call(c, "POST", fmt.Sprintf("%s/export-requests/%d/cancel", base, hdr.Request.ID), nil); code != 200 || len(r.Results) != 1 || !r.Results[0].Cancelled ||
		r.Request.Status != db.ExportCancelled {
		t.Fatalf("cancel group: %d %+v", code, r)
	}
	var single struct {
		Job       *db.ExportJob `json:"job"`
		Cancelled bool          `json:"cancelled"`
	}
	if code := c.raw("POST", fmt.Sprintf("%s/exports/%d/cancel", base, hdr.Jobs[0].ID), nil, &single); code != 200 || single.Cancelled || single.Job.Status != db.ExportCancelled {
		t.Fatalf("second cancel: %d %+v", code, single)
	}

	// 真实文档引擎执行：两种格式都是最终稿，下载头标明业务状态与哈希。
	drainExports(t, runner)
	var got db.ExportRequest
	if code := c.raw("GET", fmt.Sprintf("%s/export-requests/%d", base, first.Request.ID), nil, &got); code != 200 || got.Status != db.ExportSucceeded ||
		got.BusinessState != db.ExportFinalReady {
		t.Fatalf("request after run: %d %+v", code, got)
	}
	for _, j := range got.Jobs {
		code, h, data := c.download(fmt.Sprintf("%s/exports/%d/download", base, j.ID))
		if code != 200 || h.Get("X-Export-Business-State") != db.ExportFinalReady || h.Get("X-Artifact-SHA256") != j.ArtifactHash || int64(len(data)) != j.ArtifactSize {
			t.Fatalf("download %s: %d %v %d bytes", j.Format, code, h, len(data))
		}
	}
	var list struct {
		Requests []*db.ExportRequest `json:"requests"`
	}
	if code := c.raw("GET", base+"/export-requests?limit=10", nil, &list); code != 200 || len(list.Requests) < 3 || list.Requests[0].ID < list.Requests[1].ID {
		t.Fatalf("list: %d %+v", code, list)
	}

	// 文档引擎不可用：最终稿新请求 503，同键重放不受影响；最终稿预检显示阻断。
	offline := newExportRequestAPI(t, &export.Service{DB: d, Store: svc.Store, OfficeReason: "测试：文档引擎未安装"}, nil)
	if code, r := call(offline, "POST", base+"/exports", map[string]any{"formats": []string{"docx"}, "mode": "final", "bidder": "示例科技有限公司", "request_key": "offline-final"}); code != 503 ||
		r.Code != "runtime_unavailable" {
		t.Fatalf("final without engine: %d %+v", code, r)
	}
	if code, r := call(offline, "POST", base+"/exports", final); code != 202 || !r.Replayed || r.Request.BusinessState != db.ExportFinalReady {
		t.Fatalf("replay without engine: %d %+v", code, r)
	}
	if code := offline.raw("GET", base+"/exports/preflight?mode=final", nil, &pre); code != 200 || pre.Readiness.Ready ||
		!strings.Contains(fmt.Sprint(pre.Readiness.Blockers), "office_unavailable") {
		t.Fatalf("final preflight without engine: %d %+v", code, pre.Readiness)
	}
	// 暂停时拒绝新请求。
	if err := d.SetTaskPaused(ctx, p.TaskID, true); err != nil {
		t.Fatal(err)
	}
	if code, r := call(c, "POST", base+"/exports", map[string]any{"formats": []string{"docx"}, "request_key": "paused-1"}); code != 422 || r.Code != "precondition_failed" {
		t.Fatalf("submit while paused: %d %+v", code, r)
	}
}

// Worker 路径：proposal_export_submit 提交并订阅 → 等待 → 真实文档引擎生成最终稿 → 唤醒 → 必需作业（proposal_export）校验通过后 done。
func TestEngineProposalExportSubmitWaitsForFinalDraft(t *testing.T) {
	pg := testDB(t)
	office := requireOfficeEngine(t)
	svc := &export.Service{DB: pg, Store: &export.Store{Root: t.TempDir()}, Office: office}
	var (
		envRef    atomic.Pointer[harnessEnv]
		version   int
		plannerMu sync.Mutex
		seen      []string
	)
	submit := tool.New(tool.Spec{Name: bidtools.ToolProposalExportSubmit, Description: "提交技术标最终稿导出"},
		func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			task := envRef.Load().task
			for _, tl := range bidtools.New(pg, task.ID, task.Assets, task.Store).WithCharts(svc).WithExportSubmit(true).ExportJobTools() {
				if tl.Name() == bidtools.ToolProposalExportSubmit {
					return tl.Call(ctx, in, tc)
				}
			}
			return tool.Errorf("proposal_export_submit not registered"), nil
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
				return use(agent.ToolAddIntent, map[string]any{"summary": "导出技术标最终稿（DOCX + PDF）",
					"mandate": map[string]any{"require_writeback": true, "allowed_job_kinds": []string{db.JobKindProposalExport},
						"required_job_kinds": []string{db.JobKindProposalExport}}})
			}
			return say("暂无新方向")
		}
	}
	var lastResult atomic.Value
	worker := brain(func(req llm.CompletionRequest) llm.Message {
		prev, isResult := lastToolResult(req)
		switch {
		case strings.Contains(lastUserText(req), "后台作业事件 #"):
			return say("最终稿已生成，目录已由文档引擎刷新")
		case isResult && prev == bidtools.ToolProposalExportSubmit:
			lastResult.Store(lastToolResultText(req))
			return say("已提交导出请求组，等待后台处理")
		case isResult:
			return say("工具已返回")
		default:
			return use(bidtools.ToolProposalExportSubmit, map[string]any{"formats": []string{"docx", "pdf"}, "bidder_name": "示例科技有限公司",
				"expected_outline_version": version, "request_key": "agent-final-export-1"})
		}
	})
	env := newEnvWith(t, 0, false, worker, Config{JobWakePoll: 100 * time.Millisecond}, envOpts{extraTools: []tool.CoreTool{submit}, planner: planner,
		legacyWriting: true, beforeRun: func(task *Task) {
			version = dbtest.ReadyTask(t, pg, task.ID, dbtest.DefaultSpec(0)).OutlineVersion
		}})
	ctx := context.Background()

	var in *db.Node
	waitFor(t, 15*time.Second, "intent waiting for its export jobs", func() bool {
		ns, _ := env.task.Store.NodesByKind(ctx, db.KindIntent, nil, 1)
		if len(ns) == 1 && db.IsWaitingIntent(ns[0]) {
			in = ns[0]
		}
		return in != nil
	})
	if s, _ := lastResult.Load().(string); !strings.Contains(s, `"accepted":true`) || !strings.Contains(s, `"strict_runtime":true`) {
		t.Fatalf("submit result: %s", s)
	}
	runner := &export.Runner{Service: svc, Owner: "engine-s4", TaskID: env.task.ID, Poll: 100 * time.Millisecond, Lease: 5 * time.Minute,
		OnChange: func(*db.ExportJob, string) { env.e.WakeAgentJobs() }}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	runner.Start(runCtx)
	waitFor(t, 120*time.Second, "intent done after the final draft was exported", func() bool {
		n, _ := env.task.Store.GetNode(ctx, in.ID)
		return n != nil && n.State == db.StateIntentDone
	})
	subs, err := pg.ListAgentJobs(ctx, db.DefaultTenant, env.task.ID, db.AgentJobFilter{IntentID: in.ID})
	if err != nil || len(subs) != 2 {
		t.Fatalf("subscriptions: %+v %v", subs, err)
	}
	for _, s := range subs {
		if s.Job.Kind != db.JobKindProposalExport || s.Event == nil || s.Event.BusinessState != db.ExportFinalReady || s.State != "acknowledged" {
			t.Fatalf("subscription: %+v event %+v", s, s.Event)
		}
		j, err := pg.GetExportJob(ctx, db.DefaultTenant, env.task.ID, s.Job.ID)
		if err != nil || j.BusinessState != db.ExportFinalReady || j.OfficeEngine != office.Name() || j.CreatedBy != "worker" || !j.StrictRuntime {
			t.Fatalf("job: %+v %v", j, err)
		}
	}
	t.Logf("Worker 最终稿：%d 个作业，引擎 %s", len(subs), export.OfficeLabel(office))
	plannerMu.Lock()
	defer plannerMu.Unlock()
	for _, s := range seen {
		if strings.Contains(s, fmt.Sprintf("意图 #%d 已结束（blocked", in.ID)) {
			t.Fatalf("planner told a waiting intent had ended:\n%s", s)
		}
	}
}
