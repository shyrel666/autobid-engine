package db

// S4 集成测试（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：导出请求组的幂等（终态重放、同键异参、并发同键、快照变化后
// 重放原请求）、期望大纲版本、严格最终稿策略（图表门禁、数据库最终稿约束）、取消 / 暂停 / 截止时间围栏与迟到写回、宿主授权与
// 范围、订阅与终态事件、结果有效性，以及请求组与全部格式作业的原子登记（故障注入）。
// 导出执行用固定的产物元数据代替真实 DOCX / PDF；真实 WPS / Word 成稿见 export 包的运行时用例。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func (f *complianceFixture) submitExport(ctx context.Context, t *testing.T, key, hash string, strict bool, formats ...string) *ExportSubmission {
	t.Helper()
	in := SubmitExportInput{RequestKey: key, RequestHash: hash, Formats: formats, DefaultTemplateHash: "tpl", StrictRuntime: strict, Actor: "exporter"}
	if strict {
		in.Executor = "WPS 12.1.0.29161"
	}
	sub, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, in)
	if err != nil {
		t.Fatalf("SubmitExport(%s): %v", key, err)
	}
	return sub
}

func (f *complianceFixture) countRows(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.d.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func finalResult(path string) ExportResult {
	return ExportResult{ArtifactPath: path, ArtifactName: "x.docx", ArtifactHash: "sha-" + path, ArtifactSize: 2048, TOCRefreshed: true,
		OfficeEngine: "wps", OfficeVersion: "WPS 12.1.0.29161"}
}

func jobIDs(jobs []*ExportJob) string {
	var parts []string
	for _, j := range jobs {
		parts = append(parts, strconv.FormatInt(j.ID, 10)+":"+j.Format+":"+j.Status)
	}
	return strings.Join(parts, ",")
}

func TestExportRequestIdempotencyAndSnapshot(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()

	first := f.submitExport(ctx, t, "ui-export-1", "h1", false, ExportPDF, ExportDOCX, ExportPDF)
	q := first.Request
	if first.Replayed || len(first.Jobs) != 2 || first.Jobs[0].Format != ExportDOCX || first.Jobs[1].Format != ExportPDF ||
		q.SnapshotHash == "" || q.PolicyVersion != ExportPolicyPreviewV1 || q.StrictRuntime || q.Status != ExportQueued || q.OutlineVersion != f.ver {
		t.Fatalf("first submission = %+v jobs %s", q, jobIDs(first.Jobs))
	}
	for _, j := range first.Jobs {
		if j.ExportRequestID == nil || *j.ExportRequestID != q.ID || j.DeadlineAt == nil || !j.DeadlineAt.Equal(q.DeadlineAt) || j.StrictRuntime {
			t.Fatalf("job = %+v", j)
		}
	}
	// 同键同参重发（含响应丢失后的重发）返回原请求组，不新增作业。
	again := f.submitExport(ctx, t, "ui-export-1", "h1", false, ExportDOCX, ExportPDF)
	if !again.Replayed || again.Request.ID != q.ID || jobIDs(again.Jobs) != jobIDs(first.Jobs) || again.Preflight != nil {
		t.Fatalf("replay = %+v", again)
	}
	if n := f.countRows(t, `SELECT count(*) FROM export_jobs WHERE export_request_id = $1`, q.ID); n != 2 {
		t.Fatalf("jobs after replay = %d", n)
	}
	// 同键异参拒绝；保留前缀与空键拒绝。
	if _, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "ui-export-1", RequestHash: "h2", Formats: []string{ExportDOCX}}); !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("same key, different input: %v", err)
	}
	for _, key := range []string{"sys:legacy:1", "", strings.Repeat("k", 201)} {
		if _, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: key, RequestHash: "h", Formats: []string{ExportDOCX}}); !errors.Is(err, ErrInvalidExport) {
			t.Fatalf("key %q: %v", key, err)
		}
	}
	// 期望大纲版本不符：CONFLICT_STALE_REVISION，不登记请求。
	if _, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "stale", RequestHash: "h", Formats: []string{ExportDOCX},
		ExpectedOutlineVersion: f.ver + 1}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("expected outline version mismatch: %v", err)
	}
	if n := f.countRows(t, `SELECT count(*) FROM export_requests WHERE task_id = $1 AND request_key = 'stale'`, f.task.ID); n != 0 {
		t.Fatalf("stale request registered: %d", n)
	}

	// 6 路并发同一新键：只登记一个请求组，全部调用得到同一组作业。
	var wg sync.WaitGroup
	got := make([]string, 6)
	errs := make([]error, 6)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sub, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "par", RequestHash: "hp", Formats: []string{ExportDOCX},
				DefaultTemplateHash: "tpl", Actor: "exporter"})
			errs[i] = err
			if err == nil {
				got[i] = strconv.FormatInt(sub.Request.ID, 10) + "/" + jobIDs(sub.Jobs)
			}
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != got[0] {
			t.Fatalf("concurrent submission %d = %q %v (first %q)", i, got[i], errs[i], got[0])
		}
	}
	if n := f.countRows(t, `SELECT count(*) FROM export_requests WHERE task_id = $1 AND request_key = 'par'`, f.task.ID); n != 1 {
		t.Fatalf("concurrent same-key requests = %d", n)
	}

	// 终态重放：成功、失败、取消之后同键重发仍返回原请求组与原作业。
	docx, err := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID)
	if err != nil || docx == nil || docx.ID != first.Jobs[0].ID {
		t.Fatalf("claim = %+v %v", docx, err)
	}
	if err := f.d.CompleteExportJob(ctx, docx.ID, docx.LeaseToken, ExportResult{ArtifactPath: "a", ArtifactName: "a.docx", ArtifactHash: "h", ArtifactSize: 1}); err != nil {
		t.Fatal(err)
	}
	if _, cancelled, err := f.d.CancelExportJob(ctx, DefaultTenant, f.task.ID, first.Jobs[1].ID, "reviewer"); err != nil || !cancelled {
		t.Fatalf("cancel pdf: %v %v", cancelled, err)
	}
	// 快照变化（章节被编辑，门禁转为阻断）不影响原请求的重放；新键被门禁拒绝。
	f.save(t, f.secs["1"], 1, f.ver, RevHumanEdit, "本项目采用云边协同总体架构，总体架构合理、技术路线先进，我方承诺总工期 100 天。", 0)
	again = f.submitExport(ctx, t, "ui-export-1", "h1", false, ExportDOCX, ExportPDF)
	if !again.Replayed || again.Request.SnapshotHash != q.SnapshotHash || again.Request.Status != "partial" ||
		again.Jobs[0].BusinessState != ExportPreviewOnly || again.Jobs[1].Status != ExportCancelled {
		t.Fatalf("terminal replay = %+v %s", again.Request, jobIDs(again.Jobs))
	}
	var blocked *ExportBlockedError
	if _, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "after-edit", RequestHash: "h", Formats: []string{ExportDOCX}}); !errors.As(err, &blocked) {
		t.Fatalf("new key after edit: %v", err)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE export_requests SET snapshot_hash = 'x' WHERE id = $1`, q.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("request snapshot rewrite: %v", err)
	}
	list, err := f.d.ListExportRequests(ctx, DefaultTenant, f.task.ID, 0, 10)
	if err != nil || len(list) < 2 || list[0].ID < list[1].ID || len(list[len(list)-1].Jobs) == 0 {
		t.Fatalf("list = %d %v", len(list), err)
	}
}

func TestExportStrictPolicyAndFinalConstraint(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	if _, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "final-0", RequestHash: "h", Formats: []string{ExportDOCX},
		StrictRuntime: true}); !errors.Is(err, ErrInvalidExport) {
		t.Fatalf("strict without executor: %v", err)
	}
	c, err := f.d.CreateChart(ctx, DefaultTenant, f.task.ID, "writer", ChartInput{SectionID: f.secs["1"], ChartType: ChartArchitecture, Title: "总体架构图",
		SourceFormat: ChartSourceMermaid, Source: "flowchart TB\n  A-->B", SourceHash: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	// 未渲染的 Mermaid 图：预览稿照常（提示源码占位框），最终稿被 chart_not_final 阻断。
	pre, err := f.d.ExportPreflightPolicy(ctx, DefaultTenant, f.task.ID, true)
	if err != nil || pre.Readiness.Ready || pre.Readiness.Counts[BlockChartNotFinal] != 1 || !pre.StrictRuntime {
		t.Fatalf("strict preflight = %+v %v", pre.Readiness, err)
	}
	if pre, err := f.d.ExportPreflight(ctx, DefaultTenant, f.task.ID); err != nil || !pre.Readiness.Ready || len(pre.Warnings) != 1 {
		t.Fatalf("preview preflight = %+v %v", pre, err)
	}
	var blocked *ExportBlockedError
	_, err = f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "final-1", RequestHash: "h", Formats: []string{ExportDOCX},
		StrictRuntime: true, Executor: "WPS 12.1.0.29161"})
	if !errors.As(err, &blocked) || blocked.Preflight.Readiness.Counts[BlockChartNotFinal] != 1 {
		t.Fatalf("strict submission with unrendered chart: %v", err)
	}
	// 渲染出已登记哈希的 PNG 后最终稿可以提交。
	if _, err := f.d.SetChartRender(ctx, DefaultTenant, f.task.ID, c.ID, c.SourceHash, ChartRender{Status: ChartRendered, Format: ChartFormatPNG,
		ArtifactPath: "charts/a.png", ArtifactHash: "pngsha", ArtifactSize: 10, WidthPx: 800, HeightPx: 600}, "runner"); err != nil {
		t.Fatal(err)
	}
	sub := f.submitExport(ctx, t, "final-1", "h-final", true, ExportDOCX, ExportPDF)
	q := sub.Request
	if !q.StrictRuntime || q.PolicyVersion != ExportPolicyFinal || q.Executor != "WPS 12.1.0.29161" || len(sub.Jobs[0].Charts) != 1 {
		t.Fatalf("strict request = %+v", q)
	}
	// 数据库约束：严格作业未刷新目录或未记录引擎时不能成功。
	j, err := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID)
	if err != nil || j == nil || !j.StrictRuntime || j.Executor != q.Executor {
		t.Fatalf("claim strict = %+v %v", j, err)
	}
	if err := f.d.CompleteExportJob(ctx, j.ID, j.LeaseToken, ExportResult{ArtifactPath: "p", ArtifactHash: "h", ArtifactSize: 1}); err == nil || !strings.Contains(err.Error(), "ck_export_jobs_final") {
		t.Fatalf("unrefreshed strict completion: %v", err)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE export_jobs SET strict_runtime = false WHERE id = $1`, j.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("strict flag rewrite: %v", err)
	}
	if err := f.d.CompleteExportJob(ctx, j.ID, j.LeaseToken, finalResult("docx")); err != nil {
		t.Fatal(err)
	}
	pdf, err := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID)
	if err != nil || pdf == nil {
		t.Fatalf("claim pdf = %+v %v", pdf, err)
	}
	if err := f.d.CompleteExportJob(ctx, pdf.ID, pdf.LeaseToken, finalResult("pdf")); err != nil {
		t.Fatal(err)
	}
	got, err := f.d.GetExportRequest(ctx, DefaultTenant, f.task.ID, q.ID)
	if err != nil || got.Status != ExportSucceeded || got.BusinessState != ExportFinalReady || got.Jobs[0].OfficeEngine != "wps" {
		t.Fatalf("final request = %+v %v", got, err)
	}
	if ok, err := f.d.agentJobResultValid(ctx, f.task.ID, JobRef{Kind: JobKindProposalExport, ID: j.ID}); err != nil || !ok {
		t.Fatalf("strict final result valid = %v %v", ok, err)
	}
	// 预览稿成功只是 preview_only，不满足 required_job_kinds=proposal_export。
	prev := f.submitExport(ctx, t, "preview-1", "h-prev", false, ExportDOCX)
	pj, _ := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID)
	if err := f.d.CompleteExportJob(ctx, pj.ID, pj.LeaseToken, finalResult("prev")); err != nil {
		t.Fatal(err)
	}
	pv, _ := f.d.GetExportJob(ctx, DefaultTenant, f.task.ID, prev.Jobs[0].ID)
	if pv.BusinessState != ExportPreviewOnly {
		t.Fatalf("preview business state = %s", pv.BusinessState)
	}
	if ok, err := f.d.agentJobResultValid(ctx, f.task.ID, JobRef{Kind: JobKindProposalExport, ID: pv.ID}); err != nil || ok {
		t.Fatalf("preview result counted as delivery: %v %v", ok, err)
	}
}

func TestExportCancelPauseAndDeadlineFences(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()

	// 取消整组：执行中与排队中的作业都写为 cancelled，旧执行者的心跳、成功与失败都被拒绝；再次取消原样返回。
	g := f.submitExport(ctx, t, "cancel-1", "h", false, ExportDOCX, ExportPDF)
	running, err := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID)
	if err != nil || running == nil {
		t.Fatalf("claim = %v", err)
	}
	q, res, err := f.d.CancelExportRequest(ctx, DefaultTenant, f.task.ID, g.Request.ID, "reviewer")
	if err != nil || len(res) != 2 || !res[0].Cancelled || !res[1].Cancelled || q.Status != ExportCancelled {
		t.Fatalf("cancel group = %+v %+v %v", q, res, err)
	}
	if err := f.d.HeartbeatExportJob(ctx, running.ID, running.LeaseToken, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("heartbeat after cancel: %v", err)
	}
	if err := f.d.CompleteExportJob(ctx, running.ID, running.LeaseToken, finalResult("late")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late success after cancel: %v", err)
	}
	if err := f.d.FailExportJobCode(ctx, running.ID, running.LeaseToken, JobErrExportFailed, "late failure", true); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late failure after cancel: %v", err)
	}
	if _, res, err := f.d.CancelExportRequest(ctx, DefaultTenant, f.task.ID, g.Request.ID, "reviewer"); err != nil || res[0].Cancelled || res[0].Status != ExportCancelled {
		t.Fatalf("second cancel = %+v %v", res, err)
	}
	if j, _ := f.d.GetExportJob(ctx, DefaultTenant, f.task.ID, running.ID); j.Status != ExportCancelled || j.ErrorCode != JobErrCancelled || j.ArtifactPath != "" {
		t.Fatalf("cancelled job = %+v", j)
	}

	// 暂停：在途执行回到 queued 并推进令牌（尝试次数与截止时间不变），旧令牌写回被拒绝；暂停期间不领取、不受理新请求，
	// 但同键重放原请求；恢复后重新领取。
	p := f.submitExport(ctx, t, "pause-1", "h", false, ExportDOCX)
	a, _ := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID)
	if a == nil || a.ID != p.Jobs[0].ID {
		t.Fatalf("claim = %+v", a)
	}
	if err := f.d.SetTaskPaused(ctx, f.task.ID, true); err != nil {
		t.Fatal(err)
	}
	paused, _ := f.d.GetExportJob(ctx, DefaultTenant, f.task.ID, a.ID)
	if paused.Status != ExportQueued || paused.LeaseToken != a.LeaseToken+1 || paused.Attempts != 1 || !paused.DeadlineAt.Equal(*a.DeadlineAt) {
		t.Fatalf("paused job = %+v", paused)
	}
	if err := f.d.CompleteExportJob(ctx, a.ID, a.LeaseToken, finalResult("paused")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("write-back after pause: %v", err)
	}
	if x, _ := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID); x != nil {
		t.Fatalf("claimed while paused: %+v", x)
	}
	if _, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "pause-2", RequestHash: "h", Formats: []string{ExportDOCX}}); !errors.Is(err, ErrJobPrecondition) {
		t.Fatalf("new request while paused: %v", err)
	}
	if r := f.submitExport(ctx, t, "pause-1", "h", false, ExportDOCX); !r.Replayed {
		t.Fatal("replay while paused was not a replay")
	}
	if err := f.d.SetTaskPaused(ctx, f.task.ID, false); err != nil {
		t.Fatal(err)
	}
	b, _ := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID)
	if b == nil || b.ID != a.ID || b.Attempts != 2 || !b.DeadlineAt.Equal(*a.DeadlineAt) || b.LeaseUntil.After(*b.DeadlineAt) {
		t.Fatalf("resumed claim = %+v", b)
	}
	if err := f.d.CompleteExportJob(ctx, b.ID, b.LeaseToken, finalResult("resumed")); err != nil {
		t.Fatal(err)
	}

	// 截止时间在首次提交时固定：过期后领取时以 BUDGET_EXHAUSTED 结束，不再执行。
	sub, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "deadline-1", RequestHash: "h", Formats: []string{ExportDOCX},
		DefaultTemplateHash: "tpl", Deadline: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if x, _ := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID); x != nil {
		t.Fatalf("expired job claimed: %+v", x)
	}
	if j, _ := f.d.GetExportJob(ctx, DefaultTenant, f.task.ID, sub.Jobs[0].ID); j.Status != ExportFailed || j.ErrorCode != JobErrBudgetExhausted {
		t.Fatalf("expired job = %s %s %q", j.Status, j.ErrorCode, j.Error)
	}
}

func TestExportSubmissionGrantScopeSubscriptionsAndEvents(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	iid, epoch := f.runningIntent(t, &IntentMandate{RequiredJobKinds: []string{JobKindProposalExport}})
	bound := WithIntentExecution(ctx, iid, epoch)
	in := SubmitExportInput{RequestKey: "agent-1", RequestHash: "ha", Formats: []string{ExportDOCX, ExportPDF}, DefaultTemplateHash: "tpl",
		ExpectedOutlineVersion: f.ver, StrictRuntime: true, Executor: "WPS 12.1.0.29161", Actor: "worker"}
	// 宿主未授予任务导出：拒绝；mandate 只能收窄（作业类型、模板）。
	if _, err := f.d.SubmitExport(bound, DefaultTenant, f.task.ID, in); !errors.Is(err, ErrExportNotGranted) {
		t.Fatalf("submission without host grant: %v", err)
	}
	granted := WithTaskExportGrant(bound)
	if _, err := f.d.SubmitExport(WithExecutionScope(granted, &IntentMandate{AllowedJobKinds: []string{JobKindChartRender}}), DefaultTenant, f.task.ID, in); err == nil ||
		!strings.Contains(err.Error(), "execution scope") {
		t.Fatalf("job kind outside scope: %v", err)
	}
	tplIn := in
	tplIn.TemplateID = 999999
	if _, err := f.d.SubmitExport(WithExecutionScope(granted, &IntentMandate{RestrictObjects: true, AllowedJobKinds: []string{JobKindProposalExport}}), DefaultTenant, f.task.ID, tplIn); err == nil ||
		!strings.Contains(err.Error(), "template 999999 is not authorized") {
		t.Fatalf("template outside scope: %v", err)
	}
	sub, err := f.d.SubmitExport(granted, DefaultTenant, f.task.ID, in)
	if err != nil || len(sub.Subscriptions) != 2 || sub.Subscriptions[0].State != "waiting" || *sub.Subscriptions[0].ExpectedVersion != int64(f.ver) {
		t.Fatalf("bound submission = %+v %v", sub, err)
	}
	again, err := f.d.SubmitExport(granted, DefaultTenant, f.task.ID, in)
	if err != nil || !again.Replayed || again.Subscriptions[0].ID != sub.Subscriptions[0].ID {
		t.Fatalf("bound replay = %+v %v", again, err)
	}
	if n := f.countRows(t, `SELECT count(*) FROM agent_job_subscriptions WHERE intent_id = $1`, iid); n != 2 {
		t.Fatalf("subscriptions after replay = %d", n)
	}
	// 范围限定对象时：只能读取 / 取消本意图订阅过的请求组与作业。
	other := f.submitExport(ctx, t, "human-1", "hh", false, ExportDOCX)
	restricted := WithExecutionScope(bound, &IntentMandate{RestrictObjects: true, AllowedJobKinds: []string{JobKindProposalExport}})
	if _, err := f.d.GetExportRequest(restricted, DefaultTenant, f.task.ID, other.Request.ID); err == nil || !strings.Contains(err.Error(), "execution scope") {
		t.Fatalf("read foreign request: %v", err)
	}
	if _, _, err := f.d.CancelExportJob(restricted, DefaultTenant, f.task.ID, other.Jobs[0].ID, "worker"); err == nil || !strings.Contains(err.Error(), "execution scope") {
		t.Fatalf("cancel foreign job: %v", err)
	}
	if _, err := f.d.GetExportRequest(restricted, DefaultTenant, f.task.ID, sub.Request.ID); err != nil {
		t.Fatalf("read own request: %v", err)
	}
	if _, _, err := f.d.CancelExportRequest(ctx, DefaultTenant, f.task.ID, other.Request.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}

	// 终态事件：成功的严格作业为 final_ready；取消后迟到的成功不再发布事件，也不改写终态。
	docx, _ := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID)
	if docx == nil || docx.ID != sub.Jobs[0].ID {
		t.Fatalf("claim = %+v", docx)
	}
	if err := f.d.CompleteExportJob(ctx, docx.ID, docx.LeaseToken, finalResult("agent-docx")); err != nil {
		t.Fatal(err)
	}
	pdf, _ := f.d.ClaimExportJob(ctx, "runner", time.Minute, f.task.ID)
	if _, cancelled, err := f.d.CancelExportJob(restricted, DefaultTenant, f.task.ID, pdf.ID, "worker"); err != nil || !cancelled {
		t.Fatalf("cancel own job: %v %v", cancelled, err)
	}
	if err := f.d.CompleteExportJob(ctx, pdf.ID, pdf.LeaseToken, finalResult("late")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late completion: %v", err)
	}
	subs := f.jobSubs(t, iid)
	states := map[int64]string{}
	for _, s := range subs {
		if s.Event == nil || s.State != "notified" {
			t.Fatalf("subscription without terminal event: %+v", s)
		}
		states[s.Job.ID] = s.Event.TerminalStatus + "/" + s.Event.BusinessState
	}
	if states[docx.ID] != "succeeded/final_ready" || states[pdf.ID] != "cancelled/cancelled" {
		t.Fatalf("events = %v", states)
	}
	for _, s := range subs {
		if n := f.eventCount(t, s.ID); n != 1 {
			t.Fatalf("subscription %d events = %d", s.ID, n)
		}
	}
	// 必需作业中有一个被取消：交付物校验不通过（ACK 取消事件不代表工作成功）。
	n, err := f.es.GetNode(ctx, iid)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.checkJobDeliverables(ctx, f.task.ID, n, NodeMandate(n)); err == nil || !strings.Contains(err.Error(), "ended cancelled") {
		t.Fatalf("deliverables with a cancelled required export: %v", err)
	}
}

// 故障注入：请求组的第二个格式作业插入失败时，请求与第一个作业一起回滚，不会半组入队；修复后同键可以正常提交。
func TestExportRequestGroupIsAtomic(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	tid := strconv.FormatInt(f.task.ID, 10)
	if _, err := f.d.ExecContext(ctx, `CREATE OR REPLACE FUNCTION test_s4_export_fault() RETURNS trigger AS $$
		BEGIN IF NEW.task_id = `+tid+` AND NEW.format = 'pdf' THEN RAISE EXCEPTION 'injected pdf insert failure'; END IF; RETURN NEW; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ExecContext(ctx, `CREATE TRIGGER trg_test_s4_export_fault_`+tid+` BEFORE INSERT ON export_jobs FOR EACH ROW EXECUTE FUNCTION test_s4_export_fault()`); err != nil {
		t.Fatal(err)
	}
	dropped := false
	drop := func() {
		if !dropped {
			dropped = true
			_, _ = f.d.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_test_s4_export_fault_`+tid+` ON export_jobs`)
		}
	}
	t.Cleanup(drop)
	if _, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "atomic-1", RequestHash: "h", Formats: []string{ExportDOCX, ExportPDF},
		DefaultTemplateHash: "tpl"}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("faulty submission: %v", err)
	}
	if n := f.countRows(t, `SELECT count(*) FROM export_requests WHERE task_id = $1`, f.task.ID); n != 0 {
		t.Fatalf("requests after rollback = %d", n)
	}
	if n := f.countRows(t, `SELECT count(*) FROM export_jobs WHERE task_id = $1`, f.task.ID); n != 0 {
		t.Fatalf("jobs after rollback = %d", n)
	}
	drop()
	sub := f.submitExport(ctx, t, "atomic-1", "h", false, ExportDOCX, ExportPDF)
	if sub.Replayed || len(sub.Jobs) != 2 {
		t.Fatalf("submission after fix = %+v", sub)
	}
}
