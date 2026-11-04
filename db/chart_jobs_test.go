package db

// S3 集成测试（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：图表渲染作业的提交门禁与幂等、领取 / 心跳 / 写回围栏、
// superseded / 取消 / 暂停 / 预算耗尽、终态事件与结果有效性，以及 HTTP 章节 Writer 图表计划与修订同事务登记（含故障注入）。
// 渲染结果用固定的产物元数据代替真实 PNG；真实 mmdc 渲染见 export 包的运行时用例。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// testChart 返回一张已规范化的 Mermaid 图表规格（源码哈希随 tag 变化）。
func testChart(tag string) ChartRenderSpec {
	return ChartRenderSpec{ChartType: ChartArchitecture, Title: "系统总体架构图 " + tag, Spec: json.RawMessage(`{"layers":[]}`),
		SourceFormat: ChartSourceMermaid, Source: "flowchart TB\n  a-->b %% " + tag, SourceHash: "hash-" + tag, Executor: "mmdc 11.17.0"}
}

func (p *proposalFixture) submitChart(ctx context.Context, t *testing.T, sid int64, rev int, key, tag string) *ChartRenderSubmission {
	t.Helper()
	sub, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, SubmitChartRenderInput{SectionID: sid, ExpectedRevNo: rev, Chart: testChart(tag),
		RequestKey: key, RequestHash: "req-" + tag, Actor: "writer"})
	if err != nil {
		t.Fatalf("SubmitChartRender(%s): %v", key, err)
	}
	return sub
}

func renderedPNG(path string) ChartRenderResult {
	return ChartRenderResult{Render: ChartRender{Status: ChartRendered, Format: ChartFormatPNG, ArtifactPath: path, ArtifactHash: "png-" + path,
		ArtifactSize: 1024, WidthPx: 800, HeightPx: 600}}
}

func (p *proposalFixture) claimChart(t *testing.T) *ChartRenderClaim {
	t.Helper()
	cl, done, err := p.d.ClaimChartRenderJob(context.Background(), "runner", time.Minute, p.task.ID)
	if err != nil || cl == nil || done != nil {
		t.Fatalf("claim: %+v %+v %v", cl, done, err)
	}
	return cl
}

func TestChartRenderSubmitGatesAndIdempotency(t *testing.T) {
	p := newProposalFixture(t)
	det, ids := p.approvedOutline(t)
	ctx := context.Background()
	sid, v := ids["1"], det.Outline.Version
	in := SubmitChartRenderInput{SectionID: sid, ExpectedRevNo: 1, Chart: testChart("a"), RequestKey: "k1", RequestHash: "h1", Actor: "writer"}
	if _, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, in); !errors.Is(err, ErrChartNoRevision) {
		t.Fatalf("submit before any revision: %v", err)
	}
	p.save(t, sid, 0, v, RevHumanEdit, "第一版正文", 0)
	stale := in
	stale.ExpectedRevNo = 2
	if _, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, stale); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("wrong expected_rev_no accepted: %v", err)
	}
	reserved := in
	reserved.RequestKey = ReservedRequestKeyPrefix + "x"
	if _, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, reserved); !errors.Is(err, ErrInvalidChart) {
		t.Fatalf("reserved key accepted: %v", err)
	}
	scoped := WithExecutionScope(ctx, &IntentMandate{AllowedSectionIDs: []int64{ids["2"]}})
	if _, err := p.d.SubmitChartRender(scoped, DefaultTenant, p.task.ID, in); err == nil || !strings.Contains(err.Error(), "execution scope") {
		t.Fatalf("section outside host scope accepted: %v", err)
	}

	// 并发同键提交只产生一个作业。
	var wg sync.WaitGroup
	got := make(chan int64, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, in)
			if err != nil {
				t.Error(err)
				return
			}
			got <- sub.Job.ID
		}()
	}
	wg.Wait()
	close(got)
	var first int64
	for id := range got {
		if first == 0 {
			first = id
		} else if id != first {
			t.Fatalf("concurrent same-key submits created jobs %d and %d", first, id)
		}
	}
	sub, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, in)
	if err != nil || !sub.Replayed || sub.Job.ID != first {
		t.Fatalf("replay: %+v %v", sub, err)
	}
	j, c := sub.Job, sub.Chart
	if j.Status != ChartJobQueued || j.Attempts != 0 || j.MaxAttempts != DefaultChartJobMaxAttempts || j.Executor != "mmdc 11.17.0" ||
		time.Until(j.DeadlineAt) < 9*time.Minute || time.Until(j.DeadlineAt) > 11*time.Minute {
		t.Fatalf("job = %+v", j)
	}
	if c.RevNo != 1 || c.RenderStatus != ChartPending || !c.LatestJob.Active() || c.LatestJob.ID != j.ID || c.Stale {
		t.Fatalf("chart = %+v", c)
	}
	conflict := in
	conflict.RequestHash = "h2"
	if _, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, conflict); !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("same key, different input: %v", err)
	}

	// 有活跃作业时，重试与兼容入口一律拒绝并发改写。
	retry := SubmitChartRenderInput{ChartID: c.ID, RequestKey: "retry-1", RequestHash: "r1", Chart: ChartRenderSpec{Executor: "mmdc 11.17.0"}}
	if _, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, retry); !errors.Is(err, ErrChartJobActive) {
		t.Fatalf("retry with an active job: %v", err)
	}
	if _, err := p.d.RebindChart(ctx, DefaultTenant, p.task.ID, c.ID, "writer"); !errors.Is(err, ErrChartJobActive) {
		t.Fatalf("rebind with an active job: %v", err)
	}
	if err := p.d.DeleteChart(ctx, DefaultTenant, p.task.ID, c.ID); !errors.Is(err, ErrChartJobActive) {
		t.Fatalf("delete with an active job: %v", err)
	}
	if _, err := p.d.SetChartRender(ctx, DefaultTenant, p.task.ID, c.ID, c.SourceHash, ChartRender{Status: ChartRendered, Format: ChartFormatNative}, "writer"); !errors.Is(err, ErrChartJobActive) {
		t.Fatalf("sync render with an active job: %v", err)
	}

	// 暂停后拒绝新请求，但同键重放仍返回原作业。
	if err := p.d.SetTaskPaused(ctx, p.task.ID, true); err != nil {
		t.Fatal(err)
	}
	other := in
	other.RequestKey, other.RequestHash, other.Chart = "k-paused", "hp", testChart("paused")
	if _, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, other); !errors.Is(err, ErrJobPrecondition) {
		t.Fatalf("submit while paused: %v", err)
	}
	if sub, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, in); err != nil || sub.Job.ID != first {
		t.Fatalf("replay while paused: %+v %v", sub, err)
	}
	if err := p.d.SetTaskPaused(ctx, p.task.ID, false); err != nil {
		t.Fatal(err)
	}
	jobs, err := p.d.ListChartRenderJobs(ctx, DefaultTenant, p.task.ID, ChartJobFilter{SectionID: sid})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
}

func TestChartRenderLifecycleEventsAndResultValidity(t *testing.T) {
	p := newProposalFixture(t)
	det, ids := p.approvedOutline(t)
	sid, v := ids["1"], det.Outline.Version
	p.save(t, sid, 0, v, RevHumanEdit, "第一版正文", 0)
	iid, epoch := p.runningIntent(t, &IntentMandate{RequiredJobKinds: []string{JobKindChartRender}})
	bound := WithIntentExecution(context.Background(), iid, epoch)
	ctx := context.Background()

	sub := p.submitChart(bound, t, sid, 1, "agent-1", "agent")
	if sub.Subscription == nil || sub.Subscription.State != "waiting" || sub.Subscription.ExpectedVersion == nil || *sub.Subscription.ExpectedVersion != 1 ||
		sub.Subscription.JobStatus != ChartJobQueued {
		t.Fatalf("subscription = %+v", sub.Subscription)
	}
	if again := p.submitChart(bound, t, sid, 1, "agent-1", "agent"); !again.Replayed || again.Subscription == nil || again.Subscription.ID != sub.Subscription.ID {
		t.Fatalf("replayed subscription = %+v", again.Subscription)
	}
	// 宿主限定对象时，只能读取 / 取消本意图已订阅的作业。
	other := p.submitChart(ctx, t, ids["2"], 0+p.saveRev(t, ids["2"], v), "user-1", "user")
	restricted := WithExecutionScope(bound, &IntentMandate{RestrictObjects: true, AllowedSectionIDs: []int64{sid, ids["2"]}, AllowedJobKinds: []string{JobKindChartRender}})
	if _, _, err := p.d.GetChartRenderJob(restricted, DefaultTenant, p.task.ID, other.Job.ID); err == nil || !strings.Contains(err.Error(), "execution scope") {
		t.Fatalf("unsubscribed job readable: %v", err)
	}
	if _, err := p.d.CancelChartRenderJob(restricted, DefaultTenant, p.task.ID, other.Job.ID, "worker"); err == nil {
		t.Fatal("unsubscribed job cancelled")
	}
	if _, _, err := p.d.GetChartRenderJob(restricted, DefaultTenant, p.task.ID, sub.Job.ID); err != nil {
		t.Fatalf("own job not readable: %v", err)
	}

	cl := p.claimChart(t)
	if cl.Job.ID != sub.Job.ID || cl.Job.Status != ChartJobRunning || cl.Job.Attempts != 1 || cl.Chart.Source != testChart("agent").Source {
		t.Fatalf("claim = %+v", cl.Job)
	}
	if err := p.d.HeartbeatChartRenderJob(ctx, cl.Job.ID, cl.Job.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := p.d.HeartbeatChartRenderJob(ctx, cl.Job.ID, cl.Job.LeaseToken+1, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-token heartbeat: %v", err)
	}
	out, err := p.d.FinishChartRenderJob(ctx, cl, renderedPNG("p1.png"))
	if err != nil || out.Status != ChartJobSucceeded || out.FinishedAt == nil {
		t.Fatalf("finish: %+v %v", out, err)
	}
	if _, err := p.d.FinishChartRenderJob(ctx, cl, renderedPNG("p2.png")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("duplicate finish: %v", err)
	}
	c, err := p.d.GetChart(ctx, DefaultTenant, p.task.ID, sub.Chart.ID)
	if err != nil || c.RenderStatus != ChartRendered || c.ArtifactPath != "p1.png" || c.WidthPx != 800 || c.LatestJob.Status != ChartJobSucceeded {
		t.Fatalf("chart after finish: %+v %v", c, err)
	}
	subs, err := p.d.ListAgentJobs(ctx, DefaultTenant, p.task.ID, AgentJobFilter{IntentID: iid})
	if err != nil || len(subs) != 1 || subs[0].State != "notified" || subs[0].Event == nil || subs[0].Event.TerminalStatus != ChartJobSucceeded ||
		subs[0].Event.BusinessState != "chart_ready" || subs[0].JobStatus != ChartJobSucceeded {
		t.Fatalf("event after finish: %+v %v", subs, err)
	}
	var payload map[string]any
	_ = json.Unmarshal(subs[0].Event.Payload, &payload)
	if payload["width_px"] != float64(800) || payload["rev_no"] != float64(1) || payload["render_status"] != ChartRendered {
		t.Fatalf("event payload = %v", payload)
	}
	ref := JobRef{Kind: JobKindChartRender, ID: sub.Job.ID}
	if ok, err := p.d.agentJobResultValid(ctx, p.task.ID, ref); err != nil || !ok {
		t.Fatalf("result valid before new revision: %v %v", ok, err)
	}
	if _, err := p.d.ExecContext(ctx, `UPDATE chart_render_jobs SET status = 'failed' WHERE id = $1`, sub.Job.ID); err == nil {
		t.Fatal("terminal job status was rewritten")
	}
	if _, err := p.d.ExecContext(ctx, `UPDATE chart_render_jobs SET deadline_at = now() WHERE id = $1`, sub.Job.ID); err == nil {
		t.Fatal("frozen deadline was rewritten")
	}
	p.save(t, sid, 1, v, RevHumanEdit, "第二版正文", 0)
	if ok, err := p.d.agentJobResultValid(ctx, p.task.ID, ref); err != nil || ok {
		t.Fatalf("result still valid after the section moved on: %v %v", ok, err)
	}
	if c, _ := p.d.GetChart(ctx, DefaultTenant, p.task.ID, sub.Chart.ID); !c.Stale {
		t.Fatalf("old revision chart not stale: %+v", c)
	}
}

// saveRev 为章节保存下一版人工正文并返回新修订号。
func (p *proposalFixture) saveRev(t *testing.T, sid int64, outlineVersion int) int {
	t.Helper()
	cur := p.section(t, sid).Section.CurrentRevNo
	return p.save(t, sid, cur, outlineVersion, RevHumanEdit, "正文 rev "+string(rune('a'+cur)), 0).Revision.RevNo
}

func TestChartRenderSupersededCancelPauseAndBudget(t *testing.T) {
	p := newProposalFixture(t)
	det, ids := p.approvedOutline(t)
	ctx := context.Background()
	sid, v := ids["1"], det.Outline.Version
	rev := p.saveRev(t, sid, v)

	// 写回时章节已前进：superseded，不改写图表。
	a := p.submitChart(ctx, t, sid, rev, "a", "a")
	cl := p.claimChart(t)
	rev = p.saveRev(t, sid, v)
	out, err := p.d.FinishChartRenderJob(ctx, cl, renderedPNG("late.png"))
	if err != nil || out.Status != ChartJobSuperseded || out.ErrorCode != JobErrStaleRevision {
		t.Fatalf("finish after the section moved on: %+v %v", out, err)
	}
	if c, _ := p.d.GetChart(ctx, DefaultTenant, p.task.ID, a.Chart.ID); c.RenderStatus != ChartPending || c.ArtifactPath != "" {
		t.Fatalf("superseded result was attached: %+v", c)
	}
	// 领取前章节已前进：领取直接结束为 superseded，不渲染。
	p.submitChart(ctx, t, sid, rev, "b", "b")
	rev = p.saveRev(t, sid, v)
	if cl, done, err := p.d.ClaimChartRenderJob(ctx, "runner", time.Minute, p.task.ID); err != nil || cl != nil || done == nil || done.Status != ChartJobSuperseded {
		t.Fatalf("claim of a stale chart: %+v %+v %v", cl, done, err)
	}

	// 取消：推进令牌，旧执行者的心跳与写回都被拒绝；重复取消原样返回。
	c := p.submitChart(ctx, t, sid, rev, "c", "c")
	cl = p.claimChart(t)
	cancelled, err := p.d.CancelChartRenderJob(ctx, DefaultTenant, p.task.ID, c.Job.ID, "operator")
	if err != nil || cancelled.Status != ChartJobCancelled || cancelled.ErrorCode != JobErrCancelled {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if err := p.d.HeartbeatChartRenderJob(ctx, cl.Job.ID, cl.Job.LeaseToken, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("heartbeat after cancel: %v", err)
	}
	if _, err := p.d.FinishChartRenderJob(ctx, cl, renderedPNG("after-cancel.png")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late success after cancel: %v", err)
	}
	if again, err := p.d.CancelChartRenderJob(ctx, DefaultTenant, p.task.ID, c.Job.ID, "operator"); err != nil || again.Status != ChartJobCancelled {
		t.Fatalf("repeat cancel: %+v %v", again, err)
	}
	if ch, _ := p.d.GetChart(ctx, DefaultTenant, p.task.ID, c.Chart.ID); ch.RenderStatus != ChartPending {
		t.Fatalf("cancelled job changed the chart: %+v", ch)
	}

	// 重试同一图表（新请求键）→ 暂停撤销在途写回资格；恢复后重新领取，尝试次数与原截止时间不变。
	retry, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, SubmitChartRenderInput{ChartID: c.Chart.ID, RequestKey: "c-retry", RequestHash: "cr",
		Chart: ChartRenderSpec{Executor: "mmdc 11.17.0"}})
	if err != nil || retry.Job.ChartID != c.Chart.ID || retry.Job.ID == c.Job.ID {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	cl = p.claimChart(t)
	if err := p.d.SetTaskPaused(ctx, p.task.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, done, err := p.d.ClaimChartRenderJob(ctx, "runner", time.Minute, p.task.ID); err != nil || got != nil || done != nil {
		t.Fatalf("claim while paused: %+v %+v %v", got, done, err)
	}
	if err := p.d.SetTaskPaused(ctx, p.task.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.d.FinishChartRenderJob(ctx, cl, renderedPNG("before-pause.png")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("pre-pause executor wrote back: %v", err)
	}
	again := p.claimChart(t)
	if again.Job.ID != retry.Job.ID || again.Job.Attempts != 2 || !again.Job.DeadlineAt.Equal(retry.Job.DeadlineAt) || again.Job.LeaseToken == cl.Job.LeaseToken {
		t.Fatalf("resumed claim lost its budget: %+v", again.Job)
	}
	// 可重试失败回到 queued（非终态，不发事件），图表不改写。
	queued, err := p.d.FinishChartRenderJob(ctx, again, ChartRenderResult{Render: ChartRender{Status: ChartFailed, Error: "mmdc crashed"}, ErrorCode: JobErrRenderFailed, Retry: true})
	if err != nil || queued.Status != ChartJobQueued || queued.Error != "mmdc crashed" {
		t.Fatalf("retryable failure: %+v %v", queued, err)
	}
	third := p.claimChart(t)
	if out, err := p.d.FinishChartRenderJob(ctx, third, renderedPNG("ok.png")); err != nil || out.Status != ChartJobSucceeded {
		t.Fatalf("third attempt: %+v %v", out, err)
	}

	// 预算耗尽：尝试次数用完与截止时间已过都在领取时结束为 failed + BUDGET_EXHAUSTED，图表标记为渲染失败。
	rev = p.section(t, sid).Section.CurrentRevNo
	one, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, SubmitChartRenderInput{SectionID: sid, ExpectedRevNo: rev, Chart: testChart("one"),
		RequestKey: "one", RequestHash: "one", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	p.claimChart(t)
	if _, err := p.d.ExecContext(ctx, `UPDATE chart_render_jobs SET lease_until = now() - interval '1 second' WHERE id = $1`, one.Job.ID); err != nil {
		t.Fatal(err)
	}
	if cl, done, err := p.d.ClaimChartRenderJob(ctx, "runner", time.Minute, p.task.ID); err != nil || cl != nil || done == nil ||
		done.Status != ChartJobFailed || done.ErrorCode != JobErrBudgetExhausted {
		t.Fatalf("attempts exhausted: %+v %+v %v", cl, done, err)
	}
	if ch, _ := p.d.GetChart(ctx, DefaultTenant, p.task.ID, one.Chart.ID); ch.RenderStatus != ChartFailed {
		t.Fatalf("exhausted chart = %+v", ch)
	}
	short, err := p.d.SubmitChartRender(ctx, DefaultTenant, p.task.ID, SubmitChartRenderInput{SectionID: sid, ExpectedRevNo: rev, Chart: testChart("short"),
		RequestKey: "short", RequestHash: "short", Deadline: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if cl, done, err := p.d.ClaimChartRenderJob(ctx, "runner", time.Minute, p.task.ID); err != nil || cl != nil || done == nil ||
		done.ID != short.Job.ID || done.ErrorCode != JobErrBudgetExhausted {
		t.Fatalf("deadline exhausted: %+v %+v %v", cl, done, err)
	}
}

func TestFinishSectionWriteJobRegistersChartPlansAtomically(t *testing.T) {
	p := newProposalFixture(t)
	_, ids := p.approvedOutline(t)
	ctx := context.Background()
	sid := ids["1"]
	plans := func(tag string) *ChartPlanBatch {
		table := ChartRenderSpec{ChartType: ChartEquipmentTable, Title: "核心设备选型表", Spec: json.RawMessage(`{"columns":["序号"],"rows":[["1"]]}`),
			SourceFormat: ChartSourceJSON, Source: `{"columns":["序号"],"rows":[["1"]]}`, SourceHash: "table-" + tag, Executor: "native"}
		return &ChartPlanBatch{Plans: []ChartPlan{{Code: "C1", RequestHash: "p1-" + tag, ChartRenderSpec: testChart(tag)},
			{Code: "C2", RequestHash: "p2-" + tag, ChartRenderSpec: table}}}
	}
	claim := func(key string) *SectionWriteJob {
		t.Helper()
		if _, err := p.d.EnqueueSectionWrite(ctx, DefaultTenant, p.task.ID, sid, key, "", "writer", nil); err != nil {
			t.Fatal(err)
		}
		j, err := p.d.ClaimSectionWriteJob(ctx, p.task.ID, 1)
		if err != nil || j == nil {
			t.Fatalf("claim section job: %+v %v", j, err)
		}
		return j
	}
	if err := ValidateChartPlans([]ChartPlan{{Code: "C1", ChartRenderSpec: testChart("x")}, {Code: "C1", ChartRenderSpec: testChart("y")}}); !errors.Is(err, ErrInvalidChart) {
		t.Fatalf("duplicate plan codes accepted: %v", err)
	}

	// 故障注入：第二张图表的作业插入失败时，修订与第一张图表一起回滚，章节作业仍可重试。
	if _, err := p.d.ExecContext(ctx, `CREATE OR REPLACE FUNCTION test_s3_chart_fault() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected chart job failure'; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.d.ExecContext(ctx, `CREATE TRIGGER trg_test_s3_chart_fault BEFORE INSERT ON chart_render_jobs
		FOR EACH ROW WHEN (NEW.source_hash LIKE 'table-fault-%') EXECUTE FUNCTION test_s3_chart_fault()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.d.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_test_s3_chart_fault ON chart_render_jobs`)
		_, _ = p.d.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS test_s3_chart_fault()`)
	})
	j := claim("plans-1")
	in := SaveRevisionInput{Source: RevAIInitial, ContentMD: "带图表计划的章节正文", SummaryNote: "摘要"}
	fault := plans("fault-" + uniq("f"))
	if _, err := p.d.FinishSectionWriteJob(ctx, j, in, fault); err == nil || !strings.Contains(err.Error(), "injected chart job failure") {
		t.Fatalf("injected failure not surfaced: %v", err)
	}
	if det := p.section(t, sid); len(det.Revisions) != 0 || det.Section.CurrentRevNo != 0 {
		t.Fatalf("revision committed without its charts: %+v", det.Revisions)
	}
	if cs, _ := p.d.ListCharts(ctx, DefaultTenant, p.task.ID, sid); len(cs) != 0 {
		t.Fatalf("charts committed without the revision: %d", len(cs))
	}
	if got, _ := p.d.GetSectionWriteJob(ctx, DefaultTenant, p.task.ID, j.ID); got.Status != "running" {
		t.Fatalf("failed finish changed the section job: %+v", got)
	}

	// 正常写回：修订与两张图表、两个渲染作业同事务登记，请求键位于服务端保留命名空间。
	res, err := p.d.FinishSectionWriteJob(ctx, j, in, plans("ok"))
	if err != nil || res.Outcome != SaveApplied {
		t.Fatalf("finish with plans: %+v %v", res, err)
	}
	cs, err := p.d.ListCharts(ctx, DefaultTenant, p.task.ID, sid)
	if err != nil || len(cs) != 2 {
		t.Fatalf("charts = %d %v", len(cs), err)
	}
	for _, c := range cs {
		if c.RevNo != res.Revision.RevNo || c.SectionRevID != res.Revision.ID || c.PlanCode == "" || !c.LatestJob.Active() {
			t.Fatalf("plan chart = %+v", c)
		}
	}
	jobs, _ := p.d.ListChartRenderJobs(ctx, DefaultTenant, p.task.ID, ChartJobFilter{SectionID: sid})
	for _, jb := range jobs {
		if !strings.HasPrefix(jb.RequestKey, ReservedRequestKeyPrefix+"section-job:") || jb.CreatedBy != "ai:writer" {
			t.Fatalf("plan job = %+v", jb)
		}
	}
	if done, _ := p.d.GetSectionWriteJob(ctx, DefaultTenant, p.task.ID, j.ID); done.Status != "awaiting_review" || !strings.Contains(string(done.ChartPlans), `"C2"`) {
		t.Fatalf("section job after finish: %+v", done)
	}

	// 冲突候选：不登记正式图表，计划随作业留存。
	j2 := claim("plans-2")
	p.save(t, sid, res.Revision.RevNo, j2.BasedOnOutlineVersion, RevHumanEdit, "人工抢先保存", 0)
	res2, err := p.d.FinishSectionWriteJob(ctx, j2, in, plans("candidate"))
	if err != nil || res2.Outcome != SaveConflict {
		t.Fatalf("candidate finish: %+v %v", res2, err)
	}
	if cs, _ := p.d.ListCharts(ctx, DefaultTenant, p.task.ID, sid); len(cs) != 2 {
		t.Fatalf("candidate registered charts: %d", len(cs))
	}
	if cand, _ := p.d.GetSectionWriteJob(ctx, DefaultTenant, p.task.ID, j2.ID); cand.Status != "candidate" || !strings.Contains(string(cand.ChartPlans), "table-candidate") {
		t.Fatalf("candidate kept no plans: %+v", cand)
	}
}
