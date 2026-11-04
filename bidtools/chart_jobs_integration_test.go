package bidtools

// diagram-generation 工具对真实 PostgreSQL 的集成测试（需要 AUTOBID_TEST_DATABASE_URL）：提交门禁错误码（FEATURE_DISABLED、
// SCOPE_VIOLATION、INVALID_ARGUMENT、CONFLICT_STALE_REVISION、IDEMPOTENCY_KEY_CONFLICT、RUNTIME_UNAVAILABLE）、受理后订阅当前意图、
// 同键重放、状态查询与取消的意图范围。只提交作业，不渲染。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/internal/tool"
)

type namedRenderer struct{}

func (namedRenderer) Name() string { return "stub-renderer" }
func (namedRenderer) RenderPNG(context.Context, string) ([]byte, error) {
	return nil, fmt.Errorf("not used")
}

func TestChartRenderTools(t *testing.T) {
	d := dbtest.Open(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	ctx := context.Background()
	task, err := d.GetTask(ctx, p.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	es := db.NewExplorationStore(d, task.ExplorationID)
	intent := func() context.Context {
		t.Helper()
		id, err := es.AddNode(ctx, db.KindIntent, db.StateIntentOpen, map[string]any{"summary": "生成章节图表"}, 1, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		epoch, ok, err := es.ClaimIntentWithLease(ctx, id, "worker", time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim intent: %v %v", ok, err)
		}
		return db.WithIntentExecution(ctx, id, epoch)
	}
	det, err := d.GetSection(ctx, db.DefaultTenant, p.TaskID, p.Sections["5"])
	if err != nil {
		t.Fatal(err)
	}
	rev := det.Section.CurrentRevNo
	newKit := func(svc *export.Service) map[string]tool.CoreTool {
		k := New(d, p.TaskID, db.NewAssetStore(d, p.TaskID), es)
		if svc != nil {
			k.WithCharts(svc)
		}
		out := map[string]tool.CoreTool{}
		for _, tl := range k.DeferredTools() {
			out[tl.Name()] = tl
		}
		return out
	}
	call := func(tools map[string]tool.CoreTool, c context.Context, name string, in any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(in)
		res, err := tools[name].Call(c, raw, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
			t.Fatalf("%s returned non-JSON %q", name, res.Content)
		}
		if res.IsError != (out["accepted"] == false) {
			t.Fatalf("%s: IsError=%v but body %s", name, res.IsError, res.Content)
		}
		return out
	}
	wantCode := func(out map[string]any, code string) {
		t.Helper()
		if out["error_code"] != code {
			t.Fatalf("error_code = %v, want %s: %v", out["error_code"], code, out)
		}
	}
	gantt := map[string]any{"start": "2026-11-02", "tasks": []map[string]any{{"id": "a", "name": "调研", "days": 5}, {"name": "实施", "after": "a", "days": 20}}}
	submit := map[string]any{"section_code": "5", "expected_rev_no": rev, "chart_type": "gantt", "title": "实施进度计划", "spec": gantt, "request_key": "tool-gantt-1"}

	wantCode(call(newKit(nil), intent(), ToolChartRenderSubmit, submit), "FEATURE_DISABLED")
	tools := newKit(&export.Service{DB: d, Store: &export.Store{Root: t.TempDir()}, Renderer: namedRenderer{}})
	wantCode(call(tools, ctx, ToolChartRenderSubmit, submit), "SCOPE_VIOLATION") // 没有宿主绑定的意图
	bound := intent()
	wantCode(call(tools, bound, ToolChartRenderSubmit, map[string]any{"section_code": "5", "chart_type": "gantt", "title": "t", "spec": gantt}), "INVALID_ARGUMENT")
	stale := map[string]any{"section_code": "5", "expected_rev_no": rev + 1, "chart_type": "gantt", "title": "t", "spec": gantt, "request_key": "tool-stale"}
	wantCode(call(tools, bound, ToolChartRenderSubmit, stale), "CONFLICT_STALE_REVISION")
	badSpec := map[string]any{"section_code": "5", "expected_rev_no": rev, "chart_type": "org", "title": "t", "request_key": "tool-bad",
		"spec": map[string]any{"nodes": []map[string]any{{"id": "a", "name": "A", "parent": "x"}}}}
	wantCode(call(tools, bound, ToolChartRenderSubmit, badSpec), "INVALID_ARGUMENT")
	outside := db.WithExecutionScope(bound, &db.IntentMandate{AllowedSectionIDs: []int64{p.Sections["1"]}})
	wantCode(call(tools, outside, ToolChartRenderSubmit, submit), "SCOPE_VIOLATION")
	noCharts := db.WithExecutionScope(bound, &db.IntentMandate{AllowedJobKinds: []string{db.JobKindTenderParse}})
	wantCode(call(tools, noCharts, ToolChartRenderSubmit, submit), "SCOPE_VIOLATION")

	out := call(tools, bound, ToolChartRenderSubmit, submit)
	ref, _ := out["job_ref"].(map[string]any)
	if out["accepted"] != true || out["replayed"] != false || ref["kind"] != db.JobKindChartRender || out["status"] != db.ChartJobQueued ||
		out["business_state"] != "pending" || out["next_action"] != "wait_for_job_event" || out["rev_no"] != float64(rev) {
		t.Fatalf("submit = %v", out)
	}
	jobRef := map[string]any{"job_ref": ref}
	subs, err := d.ListAgentJobs(bound, db.DefaultTenant, p.TaskID, db.AgentJobFilter{})
	if err != nil || len(subs) != 1 || subs[0].Job.ID != int64(ref["id"].(float64)) || subs[0].State != "waiting" {
		t.Fatalf("submit did not subscribe the current intent: %+v %v", subs, err)
	}
	if again := call(tools, bound, ToolChartRenderSubmit, submit); again["replayed"] != true || again["job_ref"].(map[string]any)["id"] != ref["id"] {
		t.Fatalf("replay = %v", again)
	}
	changed := map[string]any{"section_code": "5", "expected_rev_no": rev, "chart_type": "gantt", "title": "另一个标题", "spec": gantt, "request_key": "tool-gantt-1"}
	wantCode(call(tools, bound, ToolChartRenderSubmit, changed), "IDEMPOTENCY_KEY_CONFLICT")
	offline := newKit(&export.Service{DB: d, Store: &export.Store{Root: t.TempDir()}, RendererReason: "测试：mmdc 未安装"})
	fresh := map[string]any{"section_code": "5", "expected_rev_no": rev, "chart_type": "gantt", "title": "t", "spec": gantt, "request_key": "tool-offline"}
	wantCode(call(offline, bound, ToolChartRenderSubmit, fresh), "RUNTIME_UNAVAILABLE")

	st := call(tools, bound, ToolChartRenderStatus, jobRef)
	chart, _ := st["chart"].(map[string]any)
	if st["status"] != db.ChartJobQueued || chart["section_code"] != "5" || chart["stale"] != false || chart["render_status"] != db.ChartPending {
		t.Fatalf("status = %v", st)
	}
	wantCode(call(tools, bound, ToolChartRenderStatus, map[string]any{"job_ref": map[string]any{"kind": "tender_parse", "id": ref["id"]}}), "INVALID_ARGUMENT")
	other := db.WithExecutionScope(intent(), &db.IntentMandate{RestrictObjects: true, AllowedSectionIDs: []int64{p.Sections["5"]},
		AllowedJobKinds: []string{db.JobKindChartRender}})
	wantCode(call(tools, other, ToolChartRenderStatus, jobRef), "SCOPE_VIOLATION")
	wantCode(call(tools, other, ToolChartRenderCancel, jobRef), "SCOPE_VIOLATION")
	cancelled := call(tools, bound, ToolChartRenderCancel, jobRef)
	if cancelled["status"] != db.ChartJobCancelled || cancelled["next_action"] != "replan_if_still_needed" || !strings.Contains(fmt.Sprint(cancelled["error"]), "已取消") {
		t.Fatalf("cancel = %v", cancelled)
	}

	found := false
	for _, s := range Skills() {
		if s.Name == SkillDiagramGeneration {
			found = strings.Join(s.Tools, ",") == strings.Join([]string{ToolChartRenderSubmit, ToolChartRenderStatus, ToolChartRenderCancel}, ",")
		}
		if s.Name == SkillProposalWriting && !strings.Contains(s.Body, `Skill("diagram-generation")`) {
			t.Fatal("proposal-writing does not point to diagram-generation")
		}
	}
	if !found {
		t.Fatal("diagram-generation skill does not unlock the chart render tools")
	}
}
