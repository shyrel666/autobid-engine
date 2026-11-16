package server

// D4 提交检查清单的 HTTP 测试（需要 AUTOBID_TEST_DATABASE_URL；不需要文档引擎）：
// 预览稿只读不可确认；最终稿的系统项由作业的回读自检结果判定；人工确认 / 知悉 / 不适用 / 未通过 / 撤回的状态码与留痕；
// 必须写明操作人；系统判定项不能手工改写；版本冲突；清单就绪与撤回后退回。
// 最终稿作业在这里用固定的回读结果完成，真实文档引擎下的回读结果见 export.TestD4FinalDraftWithRealEngine。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/export"
)

type submissionResp struct {
	Checklist *db.SubmissionChecklist    `json:"checklist"`
	Events    []*db.SubmissionCheckEvent `json:"events"`
	Error     string                     `json:"error"`
	Code      string                     `json:"code"`
}

func (r submissionResp) item(t *testing.T, key string) db.SubmissionItem {
	t.Helper()
	if r.Checklist != nil {
		for _, it := range r.Checklist.Items {
			if it.Key == key {
				return it
			}
		}
	}
	t.Fatalf("checklist has no item %q: %+v", key, r.Checklist)
	return db.SubmissionItem{}
}

func TestHTTPSubmissionChecklist(t *testing.T) {
	d := dbtest.Open(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	ctx := context.Background()
	svc := &export.Service{DB: d, Store: &export.Store{Root: t.TempDir()}, OfficeReason: "测试：不使用文档引擎"}
	runner := &export.Runner{Service: svc, Owner: "http-d4", TaskID: p.TaskID, Lease: time.Minute}
	c := newExportRequestAPI(t, svc, runner)
	base := fmt.Sprintf("/api/tasks/%d", p.TaskID)
	sub := func(rid int64) string { return fmt.Sprintf("%s/export-requests/%d/submission", base, rid) }
	call := func(method, path string, body any) (int, submissionResp) {
		t.Helper()
		var out submissionResp
		return c.raw(method, path, body, &out), out
	}

	// 预览稿：清单可读，写明不是最终稿；不能确认。
	var created exportSubmitResp
	if code := c.raw("POST", base+"/exports", map[string]any{"formats": []string{"docx"}, "request_key": "d4-http-preview", "mode": "preview"}, &created); code != 202 {
		t.Fatalf("preview submit: %d %+v", code, created)
	}
	drainExports(t, runner)
	code, pre := call("GET", sub(created.Request.ID), nil)
	if code != 200 || pre.Checklist.Final || pre.Checklist.Ready || pre.item(t, "final_draft").Status != db.CheckFail {
		t.Fatalf("preview checklist: %d %+v", code, pre.Checklist)
	}
	if code, r := call("PUT", sub(created.Request.ID)+"/cover_check", map[string]any{"status": "confirmed"}); code != 409 || r.Code != "not_final" {
		t.Fatalf("confirming a preview: %d %+v", code, r)
	}

	// 最终稿：两种格式，以固定的回读结果完成。
	fin, err := d.SubmitExport(ctx, db.DefaultTenant, p.TaskID, db.SubmitExportInput{RequestKey: "d4-http-final", RequestHash: "h", StrictRuntime: true,
		Formats: []string{db.ExportDOCX, db.ExportPDF}, DefaultTemplateHash: export.DefaultTemplateHash(), Executor: "WPS 12.1.0.29690",
		Bidder: "示例科技有限公司", Actor: "exporter"})
	if err != nil {
		t.Fatalf("SubmitExport: %v", err)
	}
	rid := fin.Request.ID
	if code, r := call("GET", sub(rid), nil); code != 200 || r.Checklist.Final || r.item(t, "toc_pages").Status != db.CheckFail {
		t.Fatalf("checklist before the jobs finish: %d %+v", code, r.Checklist)
	}
	for range fin.Jobs {
		ref, err := d.ClaimExportJob(ctx, "runner", 0, p.TaskID)
		if err != nil || ref == nil {
			t.Fatalf("ClaimExportJob: %v %v", ref, err)
		}
		res := db.ExportResult{ArtifactPath: "t/" + ref.Format, ArtifactName: "final." + ref.Format, ArtifactHash: "sha-" + ref.Format, ArtifactSize: 4096,
			TOCRefreshed: true, OfficeEngine: "wps", OfficeVersion: "WPS 12.1.0.29690",
			SelfCheck: &db.ExportSelfCheck{Pages: 12, TOCEntries: 8, ScoreRows: 1, PageRefs: 6, PageRefsVerified: 6, VerifiedAgainst: "pdf",
				MetadataChecked: true, Author: "示例科技有限公司", LayoutChecked: true, Tables: 2}}
		if err := d.CompleteExportJob(ctx, ref.ID, ref.LeaseToken, res); err != nil {
			t.Fatalf("CompleteExportJob: %v", err)
		}
	}
	code, got := call("GET", sub(rid), nil)
	if code != 200 || !got.Checklist.Final || !got.Checklist.Current || got.Checklist.Ready || len(got.Events) != 0 ||
		got.Checklist.ArtifactHashes["pdf"] != "sha-pdf" || got.Checklist.Version != db.SubmissionChecklistVersion {
		t.Fatalf("final checklist: %d %+v", code, got.Checklist)
	}
	for _, k := range []string{"final_draft", "current_content", "bidder_name", "toc_pages", "score_index", "layout", "page_numbers", "metadata"} {
		if it := got.item(t, k); it.Status != db.CheckPass || it.Confirmable {
			t.Errorf("system item %s = %+v", k, it)
		}
	}
	if it := got.item(t, "own_template"); it.Status != db.CheckAttention || !it.Confirmable {
		t.Fatalf("built-in template must need acknowledgement: %+v", it)
	}

	// 写入校验。
	for name, tc := range map[string]struct {
		method, path string
		body         any
		status       int
		code         string
	}{
		"unknown item":                   {"PUT", sub(rid) + "/no_such_item", map[string]any{"status": "confirmed"}, 400, "invalid_check"},
		"system item decided":            {"PUT", sub(rid) + "/toc_pages", map[string]any{"status": "confirmed", "note": "x"}, 400, "invalid_check"},
		"attention without a note":       {"PUT", sub(rid) + "/own_template", map[string]any{"status": "confirmed"}, 400, "invalid_check"},
		"attention cannot be failed":     {"PUT", sub(rid) + "/own_template", map[string]any{"status": "not_applicable", "note": "x"}, 400, "invalid_check"},
		"failed without a note":          {"PUT", sub(rid) + "/signature_seal", map[string]any{"status": "failed"}, 400, "invalid_check"},
		"unknown status":                 {"PUT", sub(rid) + "/signature_seal", map[string]any{"status": "done"}, 400, "invalid_check"},
		"empty status on PUT":            {"PUT", sub(rid) + "/signature_seal", map[string]any{"status": ""}, 400, ""},
		"withdraw nothing":               {"DELETE", sub(rid) + "/signature_seal", nil, 400, "invalid_check"},
		"unknown request":                {"GET", sub(rid + 1000000), nil, 404, ""},
		"request of another task's path": {"GET", fmt.Sprintf("/api/tasks/%d/export-requests/%d/submission", p.TaskID+1000000, rid), nil, 404, ""},
	} {
		if code, r := call(tc.method, tc.path, tc.body); code != tc.status || (tc.code != "" && r.Code != tc.code) {
			t.Errorf("%s: %d %q (%s), want %d %q", name, code, r.Code, r.Error, tc.status, tc.code)
		}
	}
	// 没有写明操作人：拒绝（c.do 不带 X-Autobid-User）。
	if code := c.do("PUT", sub(rid)+"/cover_check", map[string]any{"status": "confirmed"}, nil); code != 400 {
		t.Fatalf("confirmation without an operator: %d", code)
	}

	// 逐项处理：知悉内置模板、确认、不适用、未通过后改判。
	if code, r := call("PUT", sub(rid)+"/own_template", map[string]any{"status": "confirmed", "note": "本项目招标文件未要求版式，接受内置模板"}); code != 200 ||
		r.item(t, "own_template").Status != db.CheckConfirmed || r.item(t, "own_template").ConfirmedBy != "http-reviewer" {
		t.Fatalf("acknowledge template: %d %+v", code, r)
	}
	if code, r := call("PUT", sub(rid)+"/signature_seal", map[string]any{"status": "failed", "note": "授权委托书未盖章"}); code != 200 ||
		r.item(t, "signature_seal").Status != db.CheckFailed || r.Checklist.Ready {
		t.Fatalf("failed check: %d %+v", code, r)
	}
	var last submissionResp
	for key, body := range map[string]map[string]any{
		"cover_check":     {"status": "confirmed"},
		"price_isolation": {"status": "not_applicable", "note": "招标文件未要求技术标与报价分册"},
		"signature_seal":  {"status": "confirmed", "note": "已补盖章", "expected_version": 1},
		"platform_import": {"status": "confirmed", "reference": "某省公共资源交易平台 投标文件制作工具 V8.2"},
		"platform_upload": {"status": "confirmed", "reference": "回执 HZ-2026-000123"},
	} {
		if code, last = call("PUT", sub(rid)+"/"+key, body); code != 200 {
			t.Fatalf("confirm %s: %d %+v", key, code, last)
		}
	}
	code, got = call("GET", sub(rid), nil)
	if code != 200 || !got.Checklist.Ready || got.Checklist.Open != 0 || len(got.Events) != 7 {
		t.Fatalf("ready checklist: %d ready=%v open=%d events=%d", code, got.Checklist.Ready, got.Checklist.Open, len(got.Events))
	}
	if it := got.item(t, "platform_upload"); it.Reference != "回执 HZ-2026-000123" || it.ConfirmedAt == nil || it.Version != 1 {
		t.Fatalf("platform_upload = %+v", it)
	}
	if it := got.item(t, "signature_seal"); it.Version != 2 || it.Status != db.CheckConfirmed {
		t.Fatalf("signature_seal after re-confirming = %+v", it)
	}
	// 成稿没有引用企业证据：不出现“证明材料原件已附入”。
	for _, it := range got.Checklist.Items {
		if it.Key == "evidence_attached" {
			t.Fatalf("evidence item on a draft without cited evidence: %+v", it)
		}
	}

	// 版本冲突与撤回：撤回后不再就绪，审计多一条。
	if code, r := call("PUT", sub(rid)+"/cover_check", map[string]any{"status": "confirmed", "expected_version": 9}); code != 409 || r.Code != "stale_revision" {
		t.Fatalf("stale expected_version: %d %+v", code, r)
	}
	if code, r := call("DELETE", sub(rid)+"/cover_check?expected_version=1", nil); code != 200 || r.Checklist.Ready ||
		r.item(t, "cover_check").Status != db.CheckPending || len(r.Events) != 8 || r.Events[0].Action != "withdrawn" || r.Events[0].Actor != "http-reviewer" {
		t.Fatalf("withdraw: %d %+v", code, r)
	}
}
