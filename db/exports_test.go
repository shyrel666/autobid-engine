package db

// Phase 6B 导出集成测试（真实 PostgreSQL）：导出门禁（未批准章节 / stale 矩阵 / open 矛盾 -> 阻断并列出明细；全部满足 -> 固化快照）、
// 导出后编辑章节不影响快照、快照列不可修改、领取 / 心跳 / 完成的围栏令牌。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (f *complianceFixture) export(t *testing.T, formats ...string) ([]*ExportJob, *ExportPreflight, error) {
	t.Helper()
	return f.d.CreateExportJobs(context.Background(), DefaultTenant, f.task.ID, "exporter", CreateExportInput{Formats: formats, DefaultTemplateHash: "h"})
}

func mustBlocked(t *testing.T, err error, pre *ExportPreflight, kinds ...string) {
	t.Helper()
	var be *ExportBlockedError
	if !errors.As(err, &be) || pre == nil || pre.Readiness.Ready {
		t.Fatalf("export not blocked: %v %+v", err, pre)
	}
	for _, k := range kinds {
		if pre.Readiness.Counts[k] == 0 {
			t.Fatalf("blocker %s missing: %+v", k, pre.Readiness.Blockers)
		}
	}
	for _, b := range pre.Readiness.Blockers {
		if b.Message == "" {
			t.Fatalf("blocker without detail: %+v", b)
		}
	}
}

func TestExportGateBlocksAndLocksSnapshot(t *testing.T) {
	f := newComplianceFixture(t)
	ctx := context.Background()
	f.writeAll(t, nil)

	// 未批准章节 + 矩阵未核验 + 一致性未运行：阻断并列出全部明细，不创建任务。
	_, pre, err := f.export(t, ExportDOCX)
	mustBlocked(t, err, pre, BlockSectionUnapproved, BlockMatrixUnverified, BlockConsistencyMissing)
	if pre.Readiness.Counts[BlockSectionUnapproved] != 5 {
		t.Fatalf("unapproved sections = %+v", pre.Readiness.Counts)
	}
	if jobs, _ := f.d.ListExportJobs(ctx, DefaultTenant, f.task.ID, 10); len(jobs) != 0 {
		t.Fatalf("blocked export created jobs: %d", len(jobs))
	}

	// 批准后仍有矛盾（第一章 90 天 vs 第五章 6 个月 / 180 日）与漏答：open 结论阻断。
	for _, id := range f.secs {
		det := f.section(t, id)
		if _, err := f.d.ApproveSectionRevision(ctx, DefaultTenant, f.task.ID, id, det.Section.CurrentRevNo, "approver"); err != nil {
			t.Fatal(err)
		}
	}
	f.verify(t, 0)
	f.consistency(t)
	_, pre, err = f.export(t, ExportDOCX)
	mustBlocked(t, err, pre, BlockFindingOpen, BlockMatrixFailed)

	// 修正后全部满足：创建成功并固化快照。
	f.save(t, f.secs["3"], 1, f.ver, RevHumanEdit, "我方承诺免费质保期 5 年，优于招标要求。", 0)
	f.save(t, f.secs["5"], 1, f.ver, RevHumanEdit, "实施进度安排：工期 90 天，分三个阶段完成。", 0)
	for _, code := range []string{"3", "5"} {
		if _, err := f.d.ApproveSectionRevision(ctx, DefaultTenant, f.task.ID, f.secs[code], 2, "approver"); err != nil {
			t.Fatal(err)
		}
	}
	f.verify(t, 0)
	f.consistency(t)
	jobs, pre, err := f.export(t, ExportDOCX, ExportPDF, ExportDOCX)
	if err != nil || len(jobs) != 2 || !pre.Readiness.Ready {
		t.Fatalf("export = %v %d jobs %+v", err, len(jobs), pre)
	}
	want := map[string]int{}
	for code, id := range f.secs {
		rev := 1
		if code == "3" || code == "5" {
			rev = 2
		}
		want[strconv.FormatInt(id, 10)] = rev
	}
	for _, j := range jobs {
		if j.Status != ExportQueued || len(j.RevSnapshot) != len(want) {
			t.Fatalf("job = %+v", j)
		}
		for k, v := range want {
			if j.RevSnapshot[k] != v {
				t.Fatalf("rev_snapshot = %v want %v", j.RevSnapshot, want)
			}
		}
		if len(j.Matrix) != len(f.reqs) || len(j.Doc.Sections) != 5 || j.Doc.Sections[0].Code != "1" || j.Doc.Sections[4].RevNo != 2 {
			t.Fatalf("matrix=%d doc=%+v", len(j.Matrix), j.Doc.Sections)
		}
		for _, m := range j.Matrix {
			if m.VerifyStatus != "verified" || m.RevNo != want[strconv.FormatInt(m.SectionID, 10)] {
				t.Fatalf("matrix snapshot row = %+v", m)
			}
		}
	}

	// 导出后编辑章节：已有快照（含偏离表数据）不变，新的导出被 stale 矩阵 / 一致性 / 未批准章节阻断。
	f.save(t, f.secs["1"], 1, f.ver, RevHumanEdit, "本项目采用云边协同总体架构，总体架构合理、技术路线先进，我方承诺总工期 100 天。", 0)
	again, err := f.d.GetExportJob(ctx, DefaultTenant, f.task.ID, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.RevSnapshot[strconv.FormatInt(f.secs["1"], 10)] != 1 || len(again.Matrix) != len(jobs[0].Matrix) || again.Matrix[0].VerifyStatus != "verified" {
		t.Fatalf("snapshot changed after edit: %+v", again.RevSnapshot)
	}
	_, pre, err = f.export(t, ExportDOCX)
	mustBlocked(t, err, pre, BlockMatrixStale, BlockConsistencyStale, BlockSectionUnapproved)

	// 快照列不可修改；状态列可以。
	if _, err := f.d.ExecContext(ctx, `UPDATE export_jobs SET matrix_snapshot = '[]' WHERE id = $1`, jobs[0].ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("matrix snapshot update: %v", err)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE export_jobs SET error = 'x' WHERE id = $1`, jobs[0].ID); err != nil {
		t.Fatalf("status update rejected: %v", err)
	}
	if _, _, err := f.d.CreateExportJobs(ctx, DefaultTenant, f.task.ID, "x", CreateExportInput{Formats: []string{"html"}}); !errors.Is(err, ErrInvalidExport) {
		t.Fatalf("invalid format: %v", err)
	}
}

func TestExportJobLeaseFencing(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	jobs, _, err := f.export(t, ExportDOCX)
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.d.ClaimExportJob(ctx, "worker-a", time.Minute, f.task.ID)
	if err != nil || a == nil || a.ID != jobs[0].ID || a.Status != ExportRunning || a.Tenant != DefaultTenant {
		t.Fatalf("claim = %+v %v", a, err)
	}
	if b, _ := f.d.ClaimExportJob(ctx, "worker-b", time.Minute, f.task.ID); b != nil {
		t.Fatalf("double claim: %+v", b)
	}
	if err := f.d.HeartbeatExportJob(ctx, a.ID, a.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	// 可重试失败回到队列；再次领取令牌递增，旧令牌的完成被拒绝。
	if err := f.d.FailExportJob(ctx, a.ID, a.LeaseToken, "document engine crashed", true); err != nil {
		t.Fatal(err)
	}
	b, err := f.d.ClaimExportJob(ctx, "worker-b", time.Minute, f.task.ID)
	if err != nil || b == nil || b.LeaseToken != a.LeaseToken+1 || b.Attempts != 2 {
		t.Fatalf("reclaim = %+v %v", b, err)
	}
	if err := f.d.CompleteExportJob(ctx, a.ID, a.LeaseToken, ExportResult{ArtifactPath: "p", ArtifactHash: "h"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale completion: %v", err)
	}
	if err := f.d.CompleteExportJob(ctx, b.ID, b.LeaseToken, ExportResult{ArtifactPath: "p", ArtifactName: "x.docx", ArtifactHash: "h", ArtifactSize: 9}); err != nil {
		t.Fatal(err)
	}
	j, _ := f.d.GetExportJob(ctx, DefaultTenant, f.task.ID, b.ID)
	if j.Status != ExportSucceeded || j.Error != "" || j.LeaseUntil != nil {
		t.Fatalf("completed job = %+v", j)
	}
	// 不可重试失败直接 failed；用尽次数的过期任务被置为 failed。
	jobs, _, _ = f.export(t, ExportPDF)
	c, _ := f.d.ClaimExportJob(ctx, "w", time.Minute, f.task.ID)
	if err := f.d.FailExportJob(ctx, c.ID, c.LeaseToken, "文档引擎不可用", false); err != nil {
		t.Fatal(err)
	}
	if j, _ := f.d.GetExportJob(ctx, DefaultTenant, f.task.ID, c.ID); j.Status != ExportFailed || j.FinishedAt == nil {
		t.Fatalf("permanent failure = %+v", j)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE export_jobs SET status = 'running', attempts = max_attempts, lease_until = now() - interval '1 second' WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if x, _ := f.d.ClaimExportJob(ctx, "w", time.Minute, f.task.ID); x != nil {
		t.Fatalf("exhausted job claimed: %+v", x)
	}
	if j, _ := f.d.GetExportJob(ctx, DefaultTenant, f.task.ID, c.ID); j.Status != ExportFailed || !strings.Contains(j.Error, "文档引擎") {
		t.Fatalf("exhausted job = %s %q", j.Status, j.Error)
	}
	_ = jobs
}
