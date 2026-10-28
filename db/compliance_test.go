package db

// Phase 6A 集成测试（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：双层响应矩阵、全文一致性核验、
// 四条失效路径（新修订 / 证据 stale / 要求变更 / 大纲 superseded）、导出就绪度阻断原因与核验期间并发写回。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/compliance"
)

type complianceFixture struct {
	*proposalFixture
	reqs map[string]int64 // 编码 -> 要求 ID
	secs map[string]int64 // 章节编码 -> 章节 ID
	ver  int
}

func strp(s string) *string { return &s }

// 章节正文：第一章工期 90 天，第五章工期 6 个月 / 180 日（矛盾）；第三章只挂 ★ 条款名（漏答）。
var complianceBodies = map[string]string{
	"1": "## 总体技术方案\n\n本项目采用云边协同总体架构，总体架构合理、技术路线先进，我方承诺总工期 90 天，严格按计划交付。",
	"2": "## 售后服务\n\n我方承诺提供 7×24 小时运维服务，故障响应时间不超过 1 小时，4 小时内到场。",
	"3": "## 质量保证\n\n★ 质保期\n\n详见附件。",
	"4": "## 投标文件格式\n\n我方投标文件已逐页签字并加盖公章，正本一份、副本四份。",
	"5": "## 实施进度计划\n\n实施进度安排：工期 6 个月，分三个阶段完成。\n\n全部工作于 180 日内完成。",
}

func newComplianceFixture(t *testing.T) *complianceFixture {
	t.Helper()
	p := &proposalFixture{fixture: newFixture(t)}
	ctx := context.Background()
	f := &complianceFixture{proposalFixture: p, reqs: map[string]int64{}}
	for _, r := range []struct{ code, cat, title, desc string }{
		{"★1", ReqMandatoryClause, "★ 总工期", "★ 投标人须承诺总工期不超过120日历天，否则投标无效。"},
		{"★2", ReqMandatoryClause, "★ 售后服务响应", "★ 投标人须提供7×24小时运维服务，故障响应时间不超过2小时。"},
		{"★3", ReqMandatoryClause, "★ 质保期", "★ 投标人须承诺免费质保期不少于3年。"},
		{"S1", ReqScoringItem, "技术方案", "总体架构合理、技术路线先进得10分。"},
		{"F1", ReqFormatAttachment, "投标文件签章", "投标文件须逐页签字盖章。"},
	} {
		mand := r.cat == ReqMandatoryClause
		it, err := p.d.CreateRequirement(ctx, DefaultTenant, p.task.ID, "reviewer", RequirementInput{Code: strp(r.code), Category: strp(r.cat),
			Title: strp(r.title), Description: strp(r.desc), IsMandatory: &mand})
		if err != nil {
			t.Fatalf("CreateRequirement %s: %v", r.code, err)
		}
		f.reqs[r.code] = it.ID
	}
	if _, err := p.d.ConfirmRequirements(ctx, DefaultTenant, p.task.ID, "reviewer", 0); err != nil {
		t.Fatal(err)
	}
	det, err := p.d.CreateOutline(ctx, DefaultTenant, p.task.ID, "editor", OutlineInput{Sections: f.sectionInputs()})
	if err != nil {
		t.Fatalf("CreateOutline: %v", err)
	}
	if _, err := p.d.SubmitOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatal(err)
	}
	if det, err = p.d.ApproveOutline(ctx, DefaultTenant, p.task.ID, det.Outline.Version, "approver"); err != nil {
		t.Fatal(err)
	}
	f.secs, f.ver = sectionIDs(det), det.Outline.Version
	return f
}

func (f *complianceFixture) sectionInputs() []SectionInput {
	return []SectionInput{
		{Code: "1", Title: "总体技术方案", Level: 1, RequirementIDs: []int64{f.reqs["★1"], f.reqs["S1"]}},
		{Code: "2", Title: "售后服务", Level: 1, RequirementIDs: []int64{f.reqs["★2"]}},
		{Code: "3", Title: "质量保证", Level: 1, RequirementIDs: []int64{f.reqs["★3"]}},
		{Code: "4", Title: "投标文件格式", Level: 1, RequirementIDs: []int64{f.reqs["F1"]}},
		{Code: "5", Title: "实施进度计划", Level: 1},
	}
}

// writeAll 为每个章节保存一版人工正文。
func (f *complianceFixture) writeAll(t *testing.T, evidence map[string][]int64) {
	t.Helper()
	for code, body := range complianceBodies {
		f.save(t, f.secs[code], 0, f.ver, RevHumanEdit, body, 0, evidence[code]...)
	}
}

func (f *complianceFixture) verify(t *testing.T, sectionID int64) *VerifyMatrixResult {
	t.Helper()
	res, err := f.d.VerifyComplianceMatrix(context.Background(), DefaultTenant, f.task.ID, sectionID, compliance.RuleVerifier{}, "verifier")
	if err != nil {
		t.Fatalf("VerifyComplianceMatrix: %v", err)
	}
	return res
}

func (f *complianceFixture) matrix(t *testing.T) map[int64]*ComplianceRow {
	t.Helper()
	v, err := f.d.ListComplianceMatrix(context.Background(), DefaultTenant, f.task.ID, MatrixFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]*ComplianceRow{}
	for _, r := range v.Rows {
		out[r.RequirementID] = r
	}
	return out
}

func (f *complianceFixture) consistency(t *testing.T) *ConsistencyRunDetail {
	t.Helper()
	det, err := f.d.RunConsistencyCheck(context.Background(), DefaultTenant, f.task.ID, "reviewer")
	if err != nil {
		t.Fatalf("RunConsistencyCheck: %v", err)
	}
	return det
}

// verifiedEvidence 创建一条知识库来源的 verified + supports 证据，返回证据 ID 与知识库文档 ID（删除文档即令证据 stale）。
func (f *complianceFixture) verifiedEvidence(t *testing.T) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	kid, _, err := f.d.InsertKnowledgeDocument(ctx, &KnowledgeDocument{TenantID: DefaultTenant, Category: KnowledgeProductSpec,
		Title: uniq("6A 运维规范"), RawText: uniq("响应 1 小时")}, []KnowledgeChunk{{Seq: 0, Content: "故障响应时间 1 小时", PageStart: 1, PageEnd: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := f.d.KnowledgeChunks(ctx, DefaultTenant, kid, false)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks: %v %v", chunks, err)
	}
	taskID := f.task.ID
	run, _, err := f.d.CreateRetrievalRun(ctx, RetrievalRun{TenantID: DefaultTenant, TaskID: &taskID, RequestKey: uniq("6a"), Request: json.RawMessage(`{}`),
		Kind: "search", MaxCalls: 1, MaxTokens: 1, Deadline: time.Now().Add(time.Hour)}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	ev := &RetrievalEvidence{RunID: run.ID, Domain: "knowledge", SourceID: chunks[0].ID, DocID: kid, Hash: "h", Snapshot: json.RawMessage(`{}`),
		Claim: json.RawMessage(`{"text":"响应≤1小时"}`), Relation: "supports", Status: "verified"}
	if err := f.d.SaveRetrievalEvidence(ctx, DefaultTenant, "tester", ev); err != nil {
		t.Fatal(err)
	}
	return ev.ID, kid
}

func findingsBy(det *ConsistencyRunDetail, kind string) []*ConsistencyFinding {
	var out []*ConsistencyFinding
	for _, f := range det.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func blockerKinds(r *ExportReadiness) map[string]int { return r.Counts }

// 验收 1：第一章工期 90 天、第五章「工期 6 个月 / 180 日」的矛盾与漏答的 ★ 条款必须 100% 检出。
func TestConsistencyDetectsContradictionAndOmission(t *testing.T) {
	f := newComplianceFixture(t)
	f.writeAll(t, nil)
	res := f.verify(t, 0)
	if res.Checked != 5 || res.OutlineVersion != f.ver {
		t.Fatalf("verify = %+v", res)
	}
	m := f.matrix(t)
	for _, code := range []string{"★1", "★2", "S1", "F1"} {
		if r := m[f.reqs[code]]; r == nil || r.VerifyStatus != compliance.StatusVerified || r.SectionRevID == nil || r.RevNo == nil || *r.RevNo != 1 || r.Anchor == "" {
			t.Fatalf("%s row = %+v", code, r)
		}
	}
	if r := m[f.reqs["★1"]]; r.Deviation != compliance.DeviationPositive {
		t.Fatalf("90 天 vs ≤120 天 should be a positive deviation: %+v", r)
	}
	det := f.consistency(t)
	if !det.Current || det.Run.Status != RunCompleted || len(det.Run.Snapshot) != 5 {
		t.Fatalf("run = %+v current=%v", det.Run, det.Current)
	}
	cons := findingsBy(det, FindingContradiction)
	var dur *ConsistencyFinding
	for _, c := range cons {
		if c.Metric == compliance.MetricDuration {
			if dur != nil {
				t.Fatalf("duplicate duration contradiction: %+v", cons)
			}
			dur = c
		}
	}
	if dur == nil || *dur.ASectionID != f.secs["1"] || *dur.BSectionID != f.secs["5"] || *dur.ARevNo != 1 || *dur.BRevNo != 1 ||
		!strings.Contains(dur.AExcerpt, "90") || !strings.Contains(dur.BExcerpt, "6 个月") || dur.ASectionCode != "1" || dur.BSectionCode != "5" {
		t.Fatalf("duration contradiction = %+v (all: %+v)", dur, cons)
	}
	if len(cons) != 1 {
		t.Fatalf("unexpected extra contradictions: %+v", cons)
	}
	om := findingsBy(det, FindingOmission)
	if len(om) != 1 || *om[0].RequirementID != f.reqs["★3"] || om[0].RequirementKind != compliance.KindStar || len(om[0].SectionIDs) != 1 ||
		om[0].SectionIDs[0] != f.secs["3"] {
		t.Fatalf("omissions = %+v", om)
	}
	if det.Run.Contradictions != 1 || det.Run.Omissions != 1 {
		t.Fatalf("run counts = %+v", det.Run)
	}

	// 处理结论：乐观锁。
	ctx := context.Background()
	if _, err := f.d.ResolveConsistencyFinding(ctx, DefaultTenant, f.task.ID, dur.ID, ResolveFindingInput{Version: dur.Version, Kind: "accepted"}, "x"); !errors.Is(err, ErrInvalidFinding) {
		t.Fatalf("accepted without note: %v", err)
	}
	got, err := f.d.ResolveConsistencyFinding(ctx, DefaultTenant, f.task.ID, dur.ID, ResolveFindingInput{Version: dur.Version, Kind: "false_positive", Note: "测试"}, "reviewer2")
	if err != nil || got.Status != FindingResolved || got.ResolvedBy != "reviewer2" || got.Version != dur.Version+1 {
		t.Fatalf("resolve: %+v %v", got, err)
	}
	if _, err := f.d.ResolveConsistencyFinding(ctx, DefaultTenant, f.task.ID, dur.ID, ResolveFindingInput{Version: dur.Version, Kind: "fixed"}, "x"); !errors.Is(err, ErrFindingConflict) {
		t.Fatalf("stale version accepted: %v", err)
	}
}

// 验收 2：只挂章节名 / 「详见附件」的正文，即使引用了已核验证据，也不能判定为 verified。
func TestMatrixTitleOnlyIsNotVerified(t *testing.T) {
	f := newComplianceFixture(t)
	evID, _ := f.verifiedEvidence(t)
	f.writeAll(t, map[string][]int64{"3": {evID}})
	f.verify(t, f.secs["3"])
	r := f.matrix(t)[f.reqs["★3"]]
	if r == nil || r.VerifyStatus != compliance.StatusFailed || !strings.Contains(r.FailureReason, "实质") || len(r.EvidenceIDs) != 1 {
		t.Fatalf("title-only row = %+v", r)
	}
	cov, err := f.d.ComplianceCoverage(context.Background(), DefaultTenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Mapped != 5 || cov.MappingRate != 1 || cov.Verified != 0 {
		t.Fatalf("coverage = %+v", cov)
	}
	var star *KindCoverage
	for i := range cov.ByKind {
		if cov.ByKind[i].Kind == compliance.KindStar {
			star = &cov.ByKind[i]
		}
	}
	if star == nil || star.Total != 3 || star.Failed != 1 || star.Unchecked != 2 {
		t.Fatalf("star coverage = %+v", cov.ByKind)
	}
}

// 验收 3：全部就绪后可导出；章节保存新修订 -> 矩阵行与包含该章节的一致性结论 stale，导出就绪度给出阻断原因。
func TestNewRevisionStalesMatrixAndBlocksExport(t *testing.T) {
	f := newComplianceFixture(t)
	ctx := context.Background()
	f.writeAll(t, nil)
	// 修正第三章（实质应答 ★3）与第五章（工期改为 90 天）。
	f.save(t, f.secs["3"], 1, f.ver, RevHumanEdit, "我方承诺免费质保期 5 年，优于招标要求。", 0)
	f.save(t, f.secs["5"], 1, f.ver, RevHumanEdit, "实施进度安排：工期 90 天，分三个阶段完成。", 0)
	r0, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r0.Ready || r0.Counts[BlockSectionUnapproved] != 5 || r0.Counts[BlockMatrixUnverified] != 5 || r0.Counts[BlockConsistencyMissing] != 1 {
		t.Fatalf("initial readiness = %+v", r0)
	}
	for code, id := range f.secs {
		det := f.section(t, id)
		if _, err := f.d.ApproveSectionRevision(ctx, DefaultTenant, f.task.ID, id, det.Section.CurrentRevNo, "approver"); err != nil {
			t.Fatalf("approve %s: %v", code, err)
		}
	}
	if res := f.verify(t, 0); res.Verified != 5 {
		t.Fatalf("verify = %+v", res.Rows)
	}
	det := f.consistency(t)
	if len(det.Findings) != 0 {
		t.Fatalf("fixed proposal still has findings: %+v", det.Findings)
	}
	ready, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID)
	if err != nil || !ready.Ready || ready.ConsistencyRun != det.Run.ID {
		t.Fatalf("readiness = %+v %v", ready, err)
	}

	// 第一章新修订（工期改为 100 天）：第一章两行矩阵 stale、一致性运行 stale、导出被阻断。
	f.save(t, f.secs["1"], 1, f.ver, RevHumanEdit, "本项目采用云边协同总体架构，总体架构合理、技术路线先进，我方承诺总工期 100 天。", 0)
	m := f.matrix(t)
	for _, code := range []string{"★1", "S1"} {
		if r := m[f.reqs[code]]; r.VerifyStatus != compliance.StatusStale || *r.RevNo != 1 || r.CurrentRevNo != 2 || !strings.Contains(r.StaleReason, "revised") {
			t.Fatalf("%s row after revision = %+v", code, r)
		}
	}
	if r := m[f.reqs["★2"]]; r.VerifyStatus != compliance.StatusVerified {
		t.Fatalf("unrelated row changed: %+v", r)
	}
	run, err := f.d.GetConsistencyRun(ctx, DefaultTenant, f.task.ID, det.Run.ID, "")
	if err != nil || run.Run.Status != RunStale || run.Current {
		t.Fatalf("run after revision = %+v %v", run, err)
	}
	blocked, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Ready || blocked.Counts[BlockMatrixStale] != 2 || blocked.Counts[BlockConsistencyStale] != 1 || blocked.Counts[BlockSectionUnapproved] != 1 {
		t.Fatalf("blocked readiness = %+v", blocked)
	}
	for _, b := range blocked.Blockers {
		if b.Kind == BlockSectionUnapproved && b.SectionID != f.secs["1"] {
			t.Fatalf("wrong section blocker: %+v", b)
		}
	}

	// 一致性结论随章节失效：新一轮核验产生矛盾（100 天 vs 90 天），再改第一章 -> 该结论 stale 且不可处理。
	f.verify(t, f.secs["1"])
	det2 := f.consistency(t)
	cons := findingsBy(det2, FindingContradiction)
	if len(cons) != 1 || cons[0].Status != FindingOpen {
		t.Fatalf("contradiction 100 vs 90: %+v", det2.Findings)
	}
	f.save(t, f.secs["1"], 2, f.ver, RevHumanEdit, "本项目采用云边协同总体架构，总体架构合理、技术路线先进，我方承诺总工期 90 天。", 0)
	fd, err := f.d.GetConsistencyFinding(ctx, DefaultTenant, f.task.ID, cons[0].ID)
	if err != nil || fd.Status != FindingStale {
		t.Fatalf("finding after revision = %+v %v", fd, err)
	}
	if _, err := f.d.ResolveConsistencyFinding(ctx, DefaultTenant, f.task.ID, fd.ID, ResolveFindingInput{Version: fd.Version, Kind: "fixed"}, "x"); !errors.Is(err, ErrFindingStale) {
		t.Fatalf("stale finding resolved: %v", err)
	}
}

// 验收 4a：Phase 4.3 证据 stale -> 引用它的矩阵行 stale，导出就绪度报告失效证据。
func TestMatrixStaleOnEvidenceInvalidated(t *testing.T) {
	f := newComplianceFixture(t)
	ctx := context.Background()
	evID, kid := f.verifiedEvidence(t)
	f.writeAll(t, map[string][]int64{"2": {evID}})
	f.verify(t, 0)
	if r := f.matrix(t)[f.reqs["★2"]]; r.VerifyStatus != compliance.StatusVerified || len(r.EvidenceIDs) != 1 || r.EvidenceIDs[0] != evID {
		t.Fatalf("row with evidence = %+v", r)
	}
	if err := f.d.DeleteKnowledgeDocument(ctx, DefaultTenant, kid); err != nil {
		t.Fatal(err)
	}
	r := f.matrix(t)[f.reqs["★2"]]
	if r.VerifyStatus != compliance.StatusStale || !strings.Contains(r.StaleReason, "evidence") {
		t.Fatalf("row after evidence stale = %+v", r)
	}
	if other := f.matrix(t)[f.reqs["★1"]]; other.VerifyStatus != compliance.StatusVerified {
		t.Fatalf("unrelated row changed: %+v", other)
	}
	// 再次核验：证据仍失效，结论为 failed（或被守卫落为 stale），不会回到 verified。
	f.verify(t, f.secs["2"])
	if r := f.matrix(t)[f.reqs["★2"]]; r.VerifyStatus == compliance.StatusVerified {
		t.Fatalf("stale evidence re-verified: %+v", r)
	}
	ready, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID)
	if err != nil || ready.Counts[BlockEvidenceInvalid] != 1 {
		t.Fatalf("readiness = %+v %v", ready, err)
	}
}

// 验收 4b：要求实质变更 -> 该要求的矩阵行 stale（原因记录为要求变更），漏项结论 stale。
func TestMatrixStaleOnRequirementChange(t *testing.T) {
	f := newComplianceFixture(t)
	ctx := context.Background()
	f.writeAll(t, nil)
	f.verify(t, 0)
	det := f.consistency(t)
	om := findingsBy(det, FindingOmission)
	if len(om) != 1 {
		t.Fatalf("omissions = %+v", om)
	}
	// 只改 ★3：它的矩阵行与漏项结论记录「要求变更」；同一事务内要求变更还使大纲 superseded，其余行随之 stale。
	if _, err := f.d.UpdateRequirement(ctx, DefaultTenant, f.task.ID, f.reqs["★3"], "reviewer", RequirementInput{
		Description: strp("★ 投标人须承诺免费质保期不少于5年。")}); err != nil {
		t.Fatal(err)
	}
	m := f.matrix(t)
	if r := m[f.reqs["★3"]]; r.VerifyStatus != compliance.StatusStale || !strings.Contains(r.StaleReason, "requirement") {
		t.Fatalf("row after requirement change = %+v", r)
	}
	if r := m[f.reqs["★2"]]; r.VerifyStatus != compliance.StatusStale || !strings.Contains(r.StaleReason, "superseded") {
		t.Fatalf("other rows after requirement change = %+v", r)
	}
	if fd, _ := f.d.GetConsistencyFinding(ctx, DefaultTenant, f.task.ID, om[0].ID); fd.Status != FindingStale || !strings.Contains(fd.StaleReason, "requirement") {
		t.Fatalf("omission after requirement change = %+v", fd)
	}
	if _, err := f.d.VerifyComplianceMatrix(ctx, DefaultTenant, f.task.ID, 0, nil, "x"); !errors.Is(err, ErrOutlineNotApproved) {
		t.Fatalf("verify without approved outline: %v", err)
	}
}

// 验收 4c：大纲 superseded -> 该大纲下全部矩阵行与一致性运行 stale。
func TestMatrixStaleOnOutlineSuperseded(t *testing.T) {
	f := newComplianceFixture(t)
	ctx := context.Background()
	f.writeAll(t, nil)
	f.verify(t, 0)
	det := f.consistency(t)
	if _, err := f.d.UpdateOutline(ctx, DefaultTenant, f.task.ID, f.ver, "editor", OutlineInput{Note: "调整目录", Sections: f.sectionInputs()}); err != nil {
		t.Fatal(err)
	}
	v, err := f.d.ListComplianceMatrix(ctx, DefaultTenant, f.task.ID, MatrixFilter{})
	if err != nil || v.Approved || v.OutlineVersion != f.ver || len(v.Rows) != 5 {
		t.Fatalf("matrix view = %+v %v", v, err)
	}
	for _, r := range v.Rows {
		if r.VerifyStatus != compliance.StatusStale || !strings.Contains(r.StaleReason, "superseded") {
			t.Fatalf("row after supersede = %+v", r)
		}
	}
	run, _ := f.d.GetConsistencyRun(ctx, DefaultTenant, f.task.ID, det.Run.ID, "")
	if run.Run.Status != RunStale {
		t.Fatalf("run after supersede = %+v", run.Run)
	}
	for _, fd := range run.Findings {
		if fd.Status != FindingStale {
			t.Fatalf("finding after supersede = %+v", fd)
		}
	}
	ready, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID)
	if err != nil || ready.Ready || ready.Counts[BlockNoApprovedOutline] != 1 {
		t.Fatalf("readiness = %+v %v", ready, err)
	}
	// 写入守卫：即使直接把旧大纲的行改回 verified，也会被强制落为 stale。
	if _, err := f.d.ExecContext(ctx, `UPDATE compliance_matrix SET verify_status = 'verified' WHERE task_id = $1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.d.ListComplianceMatrix(ctx, DefaultTenant, f.task.ID, MatrixFilter{Status: compliance.StatusVerified})
	if len(v.Rows) != 0 {
		t.Fatalf("guard let verified rows through: %+v", v.Rows)
	}
}

// blockingVerifier 在第一次核验时阻塞，制造「核验进行中」窗口。
type blockingVerifier struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingVerifier) Name() string { return compliance.VerifierRule }

func (b *blockingVerifier) Verify(ctx context.Context, in compliance.Input) (compliance.Verdict, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return compliance.RuleVerifier{}.Verify(ctx, in)
}

// 验收 5：核验进行中章节被人工保存新修订 —— 写回等待核验事务提交，随后旧结论被置为 stale，
// 旧核验结果绝不会挂到新修订上。
func TestConcurrentRevisionDuringVerification(t *testing.T) {
	f := newComplianceFixture(t)
	ctx := context.Background()
	f.writeAll(t, nil)
	sid := f.secs["2"]
	bv := &blockingVerifier{started: make(chan struct{}), release: make(chan struct{})}
	verifyDone := make(chan error, 1)
	go func() {
		_, err := f.d.VerifyComplianceMatrix(ctx, DefaultTenant, f.task.ID, sid, bv, "verifier")
		verifyDone <- err
	}()
	<-bv.started
	saveDone := make(chan *SaveRevisionResult, 1)
	saveErr := make(chan error, 1)
	go func() {
		res, err := f.d.SaveSectionRevision(ctx, DefaultTenant, "editor", SaveRevisionInput{TaskID: f.task.ID, SectionID: sid, BaseRevNo: 1,
			BasedOnOutlineVersion: f.ver, Source: RevHumanEdit, ContentMD: "我方承诺故障响应时间 3 小时。"})
		saveDone <- res
		saveErr <- err
	}()
	select {
	case <-saveDone:
		t.Fatal("save must wait for the in-flight verification transaction")
	case <-time.After(300 * time.Millisecond):
	}
	close(bv.release)
	if err := <-verifyDone; err != nil {
		t.Fatalf("verify: %v", err)
	}
	res := <-saveDone
	if err := <-saveErr; err != nil || res.Outcome != SaveApplied || res.Revision.RevNo != 2 {
		t.Fatalf("save after verify: %+v %v", res, err)
	}
	r := f.matrix(t)[f.reqs["★2"]]
	if r.VerifyStatus != compliance.StatusStale || r.RevNo == nil || *r.RevNo != 1 || r.CurrentRevNo != 2 {
		t.Fatalf("old verdict leaked onto new revision: %+v", r)
	}
	var revID int64
	if err := f.d.QueryRowContext(ctx, `SELECT id FROM section_revisions WHERE section_id = $1 AND rev_no = 1`, sid).Scan(&revID); err != nil || *r.SectionRevID != revID {
		t.Fatalf("row must stay bound to rev 1 (%d): %+v %v", revID, r, err)
	}
	// 重新核验新修订：3 小时 > 2 小时门槛 -> 负偏离 failed，并绑定 rev 2。
	f.verify(t, sid)
	r = f.matrix(t)[f.reqs["★2"]]
	if r.VerifyStatus != compliance.StatusFailed || r.Deviation != compliance.DeviationNegative || *r.RevNo != 2 {
		t.Fatalf("re-verify new revision: %+v", r)
	}
}
