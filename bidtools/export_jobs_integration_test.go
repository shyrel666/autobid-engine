package bidtools

// proposal-export 工具对真实 PostgreSQL 的集成测试（需要 AUTOBID_TEST_DATABASE_URL）：严格最终稿预检、提交门禁错误码
// （FEATURE_DISABLED：未配置或宿主未授予；SCOPE_VIOLATION：无宿主意图、作业类型 / 模板越界；INVALID_ARGUMENT；
// CONFLICT_STALE_REVISION；RUNTIME_UNAVAILABLE；IDEMPOTENCY_KEY_CONFLICT；PRECONDITION_FAILED 附 blockers）、受理后订阅当前意图、
// 同键重放、状态查询与整组取消的意图范围。只提交作业，不执行导出（真实 WPS 成稿见 export 与 server 包）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/internal/tool"
)

// labelOffice 只提供执行版本标签（提交时固定），不执行刷新。
type labelOffice struct{}

func (labelOffice) Name() string    { return "wps" }
func (labelOffice) Version() string { return "WPS 12.1.0.29161" }
func (labelOffice) Refresh(context.Context, string, string, string) (*export.RefreshResult, error) {
	return nil, errors.New("not used")
}

func TestProposalExportTools(t *testing.T) {
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
		id, err := es.AddNode(ctx, db.KindIntent, db.StateIntentOpen, map[string]any{"summary": "导出技术标最终稿"}, 1, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		epoch, ok, err := es.ClaimIntentWithLease(ctx, id, "worker", time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim intent: %v %v", ok, err)
		}
		return db.WithIntentExecution(ctx, id, epoch)
	}
	newKit := func(svc *export.Service, grant bool) map[string]tool.CoreTool {
		k := New(d, p.TaskID, db.NewAssetStore(d, p.TaskID), es).WithExportSubmit(grant)
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
	withOffice := &export.Service{DB: d, Store: &export.Store{Root: t.TempDir()}, Office: labelOffice{}}
	noOffice := &export.Service{DB: d, Store: &export.Store{Root: t.TempDir()}, OfficeReason: "测试：文档引擎未安装"}
	tools := newKit(withOffice, true)

	// 预检：严格最终稿口径，返回批准大纲版本；文档引擎不可用时显示为阻断。
	pre := call(tools, ctx, ToolProposalExportCheck, map[string]any{"formats": []string{"pdf", "docx"}})
	version := pre["outline_version"]
	if pre["ready"] != true || pre["strict_runtime"] != true || pre["policy_version"] != db.ExportPolicyFinal || version == float64(0) {
		t.Fatalf("check = %v", pre)
	}
	offline := call(newKit(noOffice, true), ctx, ToolProposalExportCheck, map[string]any{"formats": []string{"docx"}})
	if offline["ready"] != false || !strings.Contains(fmt.Sprint(offline["blockers"]), "office_unavailable") {
		t.Fatalf("check without engine = %v", offline)
	}
	wantCode(call(tools, ctx, ToolProposalExportCheck, map[string]any{"formats": []string{"html"}}), "INVALID_ARGUMENT")

	submit := map[string]any{"formats": []string{"docx", "pdf"}, "bidder_name": "示例科技有限公司", "expected_outline_version": version, "request_key": "agent-final-1"}
	wantCode(call(newKit(nil, true), intent(), ToolProposalExportSubmit, submit), "FEATURE_DISABLED")
	wantCode(call(newKit(withOffice, false), intent(), ToolProposalExportSubmit, submit), "FEATURE_DISABLED") // 宿主未授予任务导出
	wantCode(call(tools, ctx, ToolProposalExportSubmit, submit), "SCOPE_VIOLATION")                           // 没有宿主绑定的意图
	bound := intent()
	wantCode(call(tools, bound, ToolProposalExportSubmit, map[string]any{"formats": []string{"docx"}, "request_key": "k"}), "INVALID_ARGUMENT")
	wantCode(call(tools, bound, ToolProposalExportSubmit, map[string]any{"formats": []string{"docx"}, "expected_outline_version": version, "request_key": "sys:x"}), "INVALID_ARGUMENT")
	// D4：最终稿必须带投标人名称（Agent 提交强制最终稿）。
	wantCode(call(tools, bound, ToolProposalExportSubmit, map[string]any{"formats": []string{"docx"}, "expected_outline_version": version, "request_key": "agent-nobidder"}), "INVALID_ARGUMENT")
	stale := map[string]any{"formats": []string{"docx"}, "bidder_name": "示例科技有限公司", "expected_outline_version": version.(float64) + 1, "request_key": "agent-stale"}
	wantCode(call(tools, bound, ToolProposalExportSubmit, stale), "CONFLICT_STALE_REVISION")
	wantCode(call(newKit(noOffice, true), bound, ToolProposalExportSubmit, submit), "RUNTIME_UNAVAILABLE")
	noExport := db.WithExecutionScope(bound, &db.IntentMandate{AllowedJobKinds: []string{db.JobKindChartRender}})
	wantCode(call(tools, noExport, ToolProposalExportSubmit, submit), "SCOPE_VIOLATION")
	tplScope := db.WithExecutionScope(bound, &db.IntentMandate{AllowedTemplateIDs: []int64{1}})
	wantCode(call(tools, tplScope, ToolProposalExportSubmit, map[string]any{"formats": []string{"docx"}, "template_id": 2, "bidder_name": "示例科技有限公司", "expected_outline_version": version, "request_key": "agent-tpl"}), "SCOPE_VIOLATION")

	out := call(tools, bound, ToolProposalExportSubmit, submit)
	jobs, _ := out["jobs"].([]any)
	if out["accepted"] != true || out["replayed"] != false || out["strict_runtime"] != true || out["executor"] != "WPS 12.1.0.29161" ||
		len(jobs) != 2 || out["next_action"] != "wait_for_job_event" || out["snapshot_hash"] == "" {
		t.Fatalf("submit = %v", out)
	}
	subs, err := d.ListAgentJobs(bound, db.DefaultTenant, p.TaskID, db.AgentJobFilter{})
	if err != nil || len(subs) != 2 || subs[0].State != "waiting" || subs[0].Job.Kind != db.JobKindProposalExport {
		t.Fatalf("submit did not subscribe the current intent: %+v %v", subs, err)
	}
	if again := call(tools, bound, ToolProposalExportSubmit, submit); again["replayed"] != true || again["export_request_id"] != out["export_request_id"] {
		t.Fatalf("replay = %v", again)
	}
	changed := map[string]any{"formats": []string{"docx"}, "expected_outline_version": version, "request_key": "agent-final-1"}
	wantCode(call(tools, bound, ToolProposalExportSubmit, changed), "IDEMPOTENCY_KEY_CONFLICT")

	// 查询与取消的意图范围。
	first := jobs[0].(map[string]any)
	jobRef := map[string]any{"job_ref": first["job_ref"]}
	st := call(tools, bound, ToolProposalExportStatus, jobRef)
	if st["status"] != db.ExportQueued || st["strict_runtime"] != true || st["business_state"] != db.ExportQueued {
		t.Fatalf("status = %v", st)
	}
	group := map[string]any{"export_request_id": out["export_request_id"]}
	wantCode(call(tools, bound, ToolProposalExportStatus, map[string]any{"job_ref": first["job_ref"], "export_request_id": out["export_request_id"]}), "INVALID_ARGUMENT")
	wantCode(call(tools, bound, ToolProposalExportStatus, map[string]any{"job_ref": map[string]any{"kind": "chart_render", "id": 1}}), "INVALID_ARGUMENT")
	other := db.WithExecutionScope(intent(), &db.IntentMandate{RestrictObjects: true, AllowedJobKinds: []string{db.JobKindProposalExport}})
	wantCode(call(tools, other, ToolProposalExportStatus, group), "SCOPE_VIOLATION")
	wantCode(call(tools, other, ToolProposalExportCancel, jobRef), "SCOPE_VIOLATION")
	cancelled := call(tools, bound, ToolProposalExportCancel, group)
	res, _ := cancelled["results"].([]any)
	if cancelled["status"] != db.ExportCancelled || len(res) != 2 || res[0].(map[string]any)["cancelled"] != true || cancelled["next_action"] != "replan_if_still_needed" {
		t.Fatalf("cancel group = %v", cancelled)
	}
	if again := call(tools, bound, ToolProposalExportCancel, jobRef); again["cancelled"] != false || again["status"] != db.ExportCancelled {
		t.Fatalf("second cancel = %v", again)
	}

	// 门禁未通过：PRECONDITION_FAILED 附 blockers，不插入作业。
	p.Save(t, "1", "## 总体技术方案\n\n本项目采用云边协同总体架构，我方承诺总工期 90 天。")
	blocked := call(tools, bound, ToolProposalExportSubmit, map[string]any{"formats": []string{"docx"}, "bidder_name": "示例科技有限公司", "expected_outline_version": version, "request_key": "agent-blocked"})
	wantCode(blocked, "PRECONDITION_FAILED")
	if !strings.Contains(fmt.Sprint(blocked["blockers"]), db.BlockSectionUnapproved) {
		t.Fatalf("blocked submission = %v", blocked)
	}

	found := false
	for _, s := range Skills() {
		if s.Name == SkillProposalExport {
			found = strings.Join(s.Tools, ",") == strings.Join([]string{ToolProposalExportCheck, ToolProposalExportSubmit, ToolProposalExportStatus, ToolProposalExportCancel}, ",")
		}
	}
	if !found {
		t.Fatal("proposal-export skill does not unlock the export tools")
	}
	if tl := New(d, p.TaskID, nil, nil).ExportCheckTools(); len(tl) != 1 || tl[0].Name() != ToolProposalExportCheck || !tl[0].IsReadOnly(nil) {
		t.Fatal("Planner export check tool must be the read-only proposal_export_check")
	}
}
