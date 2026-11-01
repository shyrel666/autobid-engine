package db

// D2 章节绑定与失效传播集成测试：章节只引用当前可采明细与已确认台账；源证据或台账变化后章节降级、
// 导出就绪返回对应阻断；重评、重写、批准、矩阵与一致性核验后可完整恢复。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/autobid/autobid-engine/compliance"
)

func TestD2SectionBindingAndInvalidation(t *testing.T) {
	f, tenant, store, _ := d2Setup(t)
	ctx := context.Background()
	valid := "2027-12-31"
	cred, _, err := f.d.UpsertCompanyCredential(ctx, &CompanyCredential{TenantID: tenant, Category: CredentialQualification, Code: "QUAL-SEC",
		Title: "安防资质（虚构）", SubjectName: "演示智联科技有限公司（虚构）", SubjectType: "bidder", Scope: "安防工程",
		ValidUntil: &valid, Attributes: json.RawMessage(`{"level":"二级"}`)})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	orig := d2Original(t, f.d, store, tenant, EvidenceDomainCredential, cred.ID, DocKindCertificate, "证书", "docx", 1)
	_ = orig

	code, title, cat := "REQ-SEC", "★资质：安防二级（截止日有效）", ReqMandatoryClause
	req, err := f.d.CreateRequirement(ctx, tenant, f.task.ID, "reviewer", RequirementInput{Code: &code, Category: &cat, Title: &title,
		Description: &title, ProofRules: &ProofRules{BidderOnly: true, ValidOnBidDeadline: true, RequiredDocs: []string{"资质证书"}, MinCount: 1}})
	if err != nil {
		t.Fatalf("requirement: %v", err)
	}
	if _, err := f.d.ConfirmRequirements(ctx, tenant, f.task.ID, "reviewer", 0); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	det, err := f.d.CreateOutline(ctx, tenant, f.task.ID, "editor", OutlineInput{Sections: []SectionInput{
		{Code: "1", Title: "资格证明", Level: 1, RequirementIDs: []int64{req.ID}},
	}})
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	if _, err := f.d.SubmitOutline(ctx, tenant, f.task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatal(err)
	}
	if det, err = f.d.ApproveOutline(ctx, tenant, f.task.ID, det.Outline.Version, "approver"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.BindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainCredential, cred.ID, "d2-test"); err != nil {
		t.Fatal(err)
	}
	run, err := f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d2-test")
	if err != nil || run.Verdict != compliance.Admissible || len(run.Items) != 1 {
		t.Fatalf("run: %+v err=%v", run, err)
	}
	secID := det.Sections[0].ID
	body := "响应要求 REQ-SEC：投标人具备安防二级资质，证书在投标截止日有效。\n\n资质证书原件见附件。"
	res, err := f.d.SaveSectionRevision(ctx, tenant, "writer", SaveRevisionInput{TaskID: f.task.ID, SectionID: secID,
		BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version, Source: RevHumanEdit, ContentMD: body,
		AdmissibilityItemIDs: []int64{run.Items[0].ID}})
	if err != nil || res.Outcome != SaveApplied {
		t.Fatalf("save with admissible item: %v %+v", err, res)
	}
	if _, err := f.d.ApproveSectionRevision(ctx, tenant, f.task.ID, secID, res.Revision.RevNo, "approver"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// 证据源更新 -> 运行 stale；引用它的章节修订与章节降级，导出就绪出现 admissibility_stale。
	cred.ValidUntil = strpTest("2026-01-01")
	if _, err := f.d.UpdateCompanyCredential(ctx, cred); err != nil {
		t.Fatalf("update credential: %v", err)
	}
	cur, err := f.d.GetSection(ctx, tenant, f.task.ID, secID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Section.Status != SectionStaleEvidence || cur.Section.ApprovedRevNo != nil || cur.Current == nil || cur.Current.Status != RevStaleEvid {
		t.Fatalf("section must be demoted after run invalidation: %+v rev=%+v", cur.Section, cur.Current)
	}
	rd, err := f.d.ExportReadiness(ctx, tenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Ready || !hasBlocker(rd, BlockAdmissibilityStale) {
		t.Fatalf("expected admissibility_stale blocker: ready=%v blockers=%+v", rd.Ready, rd.Blockers)
	}

	// 修正证书 -> 重评 -> 新明细 -> 重写并批准 -> 矩阵 -> 一致性 -> 完整恢复 ready。
	cred.ValidUntil = &valid
	if _, err := f.d.UpdateCompanyCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	run2, err := f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d2-test")
	if err != nil || run2.Verdict != compliance.Admissible || len(run2.Items) != 1 {
		t.Fatalf("re-evaluate: %+v err=%v", run2, err)
	}
	cur, _ = f.d.GetSection(ctx, tenant, f.task.ID, secID)
	res2, err := f.d.SaveSectionRevision(ctx, tenant, "writer", SaveRevisionInput{TaskID: f.task.ID, SectionID: secID,
		BaseRevNo: cur.Section.CurrentRevNo, BasedOnOutlineVersion: det.Outline.Version, Source: RevHumanEdit, ContentMD: body,
		AdmissibilityItemIDs: []int64{run2.Items[0].ID}})
	if err != nil || res2.Outcome != SaveApplied {
		t.Fatalf("re-save: %v %+v", err, res2)
	}
	if _, err := f.d.ApproveSectionRevision(ctx, tenant, f.task.ID, secID, res2.Revision.RevNo, "approver"); err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if res, err := f.d.VerifyComplianceMatrix(ctx, tenant, f.task.ID, 0, compliance.RuleVerifier{}, "verifier"); err != nil || res.Verified != 1 {
		t.Fatalf("matrix: %+v err=%v", res, err)
	}
	if _, err := f.d.RunConsistencyCheck(ctx, tenant, f.task.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	rd, err = f.d.ExportReadiness(ctx, tenant, f.task.ID)
	if err != nil || !rd.Ready {
		t.Fatalf("expected full recovery: ready=%v blockers=%+v err=%v", rd != nil && rd.Ready, blockerText(rd), err)
	}

	// 承诺台账：草稿不可引用；确认后可引用；台账变更使章节 stale_commitment 并阻断导出。
	threshold := 90.0
	committed := 90.0
	commitIn := CommitmentInput{Code: "C-DUR-SEC", RequirementID: &req.ID, Metric: MetricDuration, MetricLabel: "总工期", Unit: "天",
		ThresholdCmp: "<=", TenderThresholdRaw: "不超过 90 日历天", TenderThresholdValue: &threshold, CommittedRaw: "90 天",
		CommittedValue: &committed, Owner: "项目经理", Rationale: "实施计划"}
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, commitIn, "planner"); err != nil {
		t.Fatal(err)
	}
	draft, _ := f.d.GetCommitment(ctx, tenant, f.task.ID, "C-DUR-SEC")
	commitBody := body + "\n\n我方承诺总工期 90 天。"
	cur, _ = f.d.GetSection(ctx, tenant, f.task.ID, secID)
	if _, err := f.d.SaveSectionRevision(ctx, tenant, "writer", SaveRevisionInput{TaskID: f.task.ID, SectionID: secID,
		BaseRevNo: cur.Section.CurrentRevNo, BasedOnOutlineVersion: det.Outline.Version, Source: RevHumanEdit, ContentMD: commitBody,
		AdmissibilityItemIDs: []int64{run2.Items[0].ID}, CommitmentIDs: []int64{draft.ID}}); !errors.Is(err, ErrCommitmentUnconfirmed) {
		t.Fatalf("draft commitment reference must be rejected: %v", err)
	}
	if _, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-DUR-SEC", "planner"); err != nil {
		t.Fatal(err)
	}
	conf, _ := f.d.GetCommitment(ctx, tenant, f.task.ID, "C-DUR-SEC")
	cur, _ = f.d.GetSection(ctx, tenant, f.task.ID, secID)
	res3, err := f.d.SaveSectionRevision(ctx, tenant, "writer", SaveRevisionInput{TaskID: f.task.ID, SectionID: secID,
		BaseRevNo: cur.Section.CurrentRevNo, BasedOnOutlineVersion: det.Outline.Version, Source: RevHumanEdit, ContentMD: commitBody,
		AdmissibilityItemIDs: []int64{run2.Items[0].ID}, CommitmentIDs: []int64{conf.ID}})
	if err != nil || res3.Outcome != SaveApplied {
		t.Fatalf("confirmed commitment reference: %v %+v", err, res3)
	}
	if _, err := f.d.ApproveSectionRevision(ctx, tenant, f.task.ID, secID, res3.Revision.RevNo, "approver"); err != nil {
		t.Fatal(err)
	}
	// 正文值与台账不一致 -> ErrCommitmentMismatch。
	cur, _ = f.d.GetSection(ctx, tenant, f.task.ID, secID)
	if _, err := f.d.SaveSectionRevision(ctx, tenant, "writer", SaveRevisionInput{TaskID: f.task.ID, SectionID: secID,
		BaseRevNo: cur.Section.CurrentRevNo, BasedOnOutlineVersion: det.Outline.Version, Source: RevHumanEdit,
		ContentMD: body + "\n\n我方承诺总工期 120 天。", AdmissibilityItemIDs: []int64{run2.Items[0].ID},
		CommitmentIDs: []int64{conf.ID}}); !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("commitment text mismatch must be rejected: %v", err)
	}
	// 修改台账 -> 章节 stale_commitment，导出就绪出现 commitment_stale。
	commitIn.CommittedRaw = "80 天"
	eighty := 80.0
	commitIn.CommittedValue = &eighty
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, commitIn, "planner"); err != nil {
		t.Fatal(err)
	}
	cur, _ = f.d.GetSection(ctx, tenant, f.task.ID, secID)
	if cur.Section.Status != SectionStaleCommitment || cur.Section.ApprovedRevNo != nil {
		t.Fatalf("section must be demoted on commitment change: %+v", cur.Section)
	}
	rd, err = f.d.ExportReadiness(ctx, tenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Ready || !hasBlocker(rd, BlockCommitmentStale) {
		t.Fatalf("expected commitment_stale blocker: ready=%v blockers=%+v", rd.Ready, rd.Blockers)
	}
}

func hasBlocker(r *ExportReadiness, kind string) bool {
	for _, b := range r.Blockers {
		if b.Kind == kind {
			return true
		}
	}
	return false
}

func blockerText(r *ExportReadiness) string {
	if r == nil {
		return ""
	}
	out := ""
	for _, b := range r.Blockers {
		out += b.Kind + ":" + b.Message + "; "
	}
	return out
}
