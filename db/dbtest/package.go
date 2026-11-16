package dbtest

// D4：构造带成稿附表数据的可导出标书——评分项映射章节（评分索引表）、已确认的承诺台账并被章节引用（承诺事项一览表）、
// 带原件页码的企业证据经可采性评估后被章节引用（证明材料索引）。只经 db / evidence 包公开 API，默认租户，
// 证据与承诺编号唯一，不影响同库的其他用例。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/evidence"
)

// LongSectionTitle 是 ReadyWithAppendices 第 4 章的标题（不含章节编号）。
const LongSectionTitle = "项目组织机构、人员配置、岗位职责与驻场服务保障安排（含项目经理及主要技术人员）"

// Appendices 是 ReadyWithAppendices 额外登记的对象。
type Appendices struct {
	CredentialID   int64
	CredentialName string
	CertificateNo  string
	Warranty       *db.Commitment // 质保期 5 年，被章节 3 引用
	Duration       *db.Commitment // 总工期 90 天，被章节 1 引用
}

// ReadyWithAppendices 构造一份导出就绪、三张附表都有数据的标书；evidenceRoot 是企业证据原件目录（通常为 t.TempDir()）。
func ReadyWithAppendices(t testing.TB, d *db.DB, evidenceRoot string) (*Proposal, *Appendices) {
	t.Helper()
	ctx := context.Background()
	tenant := db.DefaultTenant
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: Uniq("D4 智慧园区安防项目"), Goal: "技术标成稿", LegacyWriting: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	p := &Proposal{DB: d, TaskID: task.ID, Reqs: map[string]int64{}, Sections: map[string]int64{}}
	if _, err := d.RequirementManifest(ctx, tenant, task.ID); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	deadline := time.Now().AddDate(0, 1, 0)
	if _, err := d.ExecContext(ctx, `UPDATE requirement_manifests SET bid_deadline = $3 WHERE tenant_id = $1 AND task_id = $2`, tenant, task.ID, deadline); err != nil {
		t.Fatalf("set bid deadline: %v", err)
	}

	// 企业证据 + 原件（第 1-2 页）。
	a := &Appendices{CredentialName: "安全技术防范工程设计施工资质（虚构）", CertificateNo: Uniq("AF")}
	valid := time.Now().AddDate(2, 0, 0).Format("2006-01-02")
	cred, _, err := d.UpsertCompanyCredential(ctx, &db.CompanyCredential{TenantID: tenant, Category: db.CredentialQualification, Code: Uniq("QUAL-D4"),
		Title: a.CredentialName, SubjectName: "示例科技有限公司", SubjectType: "bidder", Scope: "安防工程设计施工", CertificateNo: a.CertificateNo,
		ValidUntil: &valid, Attributes: json.RawMessage(`{"level":"二级"}`)})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	a.CredentialID = cred.ID
	store := evidence.New(evidenceRoot)
	rel, sum, err := store.Put(tenant, "pdf", []byte("certificate scan "+a.CertificateNo))
	if err != nil {
		t.Fatalf("put original: %v", err)
	}
	if _, err := d.AddCompanyEvidenceOriginal(ctx, &db.CompanyEvidenceOriginal{TenantID: tenant, OwnerDomain: db.EvidenceDomainCredential, OwnerID: cred.ID,
		DocumentKind: db.DocKindCertificate, PageLabel: "正本", OriginalPath: rel, OriginalSHA256: sum, PageStart: 1, PageEnd: 2, CreatedBy: "d4-test"}, false); err != nil {
		t.Fatalf("add original: %v", err)
	}

	// 要求：资质（带证明规则）、工期与质保（★）、两条技术评分项（带分值）。
	tech := db.ScoreGroupTechnical
	f := func(v float64) *float64 { return &v }
	b := func(v bool) *bool { return &v }
	for _, r := range []struct {
		code string
		in   db.RequirementInput
	}{
		{"★1", db.RequirementInput{Category: strp(db.ReqMandatoryClause), Title: strp("★ 资质要求"), IsMandatory: b(true),
			Description: strp("★ 投标人具备安全技术防范工程设计施工二级及以上资质，证书在投标截止日有效。"),
			ProofRules:  &db.ProofRules{ValidOnBidDeadline: true, RequiredDocs: []string{"资质证书"}, MinCount: 1}}},
		{"★2", db.RequirementInput{Category: strp(db.ReqMandatoryClause), Title: strp("★ 总工期"), IsMandatory: b(true),
			Description: strp("★ 投标人须承诺总工期不超过120日历天，否则投标无效。")}},
		{"★3", db.RequirementInput{Category: strp(db.ReqMandatoryClause), Title: strp("★ 质保期"), IsMandatory: b(true),
			Description: strp("★ 投标人须承诺免费质保期不少于3年。")}},
		{"S1", db.RequirementInput{Category: strp(db.ReqScoringItem), Title: strp("技术方案"), ScoreWeight: f(10), ScoreGroup: &tech,
			Description: strp("总体架构合理、技术路线先进得10分。")}},
		{"S2", db.RequirementInput{Category: strp(db.ReqScoringItem), Title: strp("质保期承诺"), ScoreWeight: f(5), ScoreGroup: &tech,
			Description: strp("质保期每延长一年得 1 分，最高 5 分。")}},
		{"S3", db.RequirementInput{Category: strp(db.ReqScoringItem), Title: strp("项目组织与人员"), ScoreWeight: f(5), ScoreGroup: &tech,
			Description: strp("项目组织机构合理、人员配置齐全、岗位职责明确得5分。")}},
	} {
		r.in.Code = strp(r.code)
		it, err := d.CreateRequirement(ctx, tenant, task.ID, "reviewer", r.in)
		if err != nil {
			t.Fatalf("CreateRequirement %s: %v", r.code, err)
		}
		p.Reqs[r.code] = it.ID
	}
	if _, err := d.ConfirmRequirements(ctx, tenant, task.ID, "reviewer", 0); err != nil {
		t.Fatalf("ConfirmRequirements: %v", err)
	}
	long := strings.Repeat("本系统采用分层解耦架构，感知层、网络层、平台层与应用层之间通过标准接口交互，保证系统的可扩展性与可维护性。", 14)
	p.Spec = Spec{Name: task.Name, Sections: []Section{
		{Code: "1", Title: "总体技术方案", Level: 1, Reqs: []string{"★2", "S1"},
			Body: "## 总体技术方案\n\n本项目采用云边协同总体架构，总体架构合理、技术路线先进，我方承诺总工期 90 天，严格按计划交付。\n\n" + long},
		{Code: "1.1", Title: "系统总体架构", Level: 2, Body: long + "\n\n" + long},
		{Code: "2", Title: "资格证明", Level: 1, Reqs: []string{"★1"},
			Body: "响应要求 ★1：投标人具备安全技术防范工程设计施工二级资质，证书在投标截止日有效，资质证书原件见证明材料索引。\n\n" + long},
		{Code: "3", Title: "质保与售后", Level: 1, Reqs: []string{"★3", "S2"},
			Body: "响应评分项 S2 质保期承诺：我方承诺免费质保期 5 年，优于招标要求，质保期内免费上门维修。\n\n" + long},
		// 标题故意长到一行排不下：成稿里它在标题、目录和评分索引表里都会折行，页码仍要能核对。
		{Code: "4", Title: LongSectionTitle, Level: 1, Reqs: []string{"S3"},
			Body: "响应评分项 S3 项目组织与人员：我方承诺项目组织机构合理、人员配置齐全、岗位职责明确，由项目经理统筹实施与驻场服务。\n\n" + long},
	}}
	var secs []db.SectionInput
	for _, s := range p.Spec.Sections {
		in := db.SectionInput{Code: s.Code, Title: s.Title, Level: s.Level}
		for _, c := range s.Reqs {
			in.RequirementIDs = append(in.RequirementIDs, p.Reqs[c])
		}
		secs = append(secs, in)
	}
	det, err := d.CreateOutline(ctx, tenant, task.ID, "editor", db.OutlineInput{Sections: secs})
	if err != nil {
		t.Fatalf("CreateOutline: %v", err)
	}
	if _, err := d.SubmitOutline(ctx, tenant, task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatalf("SubmitOutline: %v", err)
	}
	if det, err = d.ApproveOutline(ctx, tenant, task.ID, det.Outline.Version, "approver"); err != nil {
		t.Fatalf("ApproveOutline: %v", err)
	}
	p.OutlineVersion = det.Outline.Version
	for _, s := range det.Sections {
		p.Sections[s.Code] = s.ID
	}

	// 证据绑定与可采性。
	if _, err := d.BindRequirementEvidence(ctx, tenant, task.ID, p.Reqs["★1"], db.EvidenceDomainCredential, cred.ID, "d4-test"); err != nil {
		t.Fatalf("bind evidence: %v", err)
	}
	run, err := d.EvaluateAdmissibility(ctx, tenant, task.ID, p.Reqs["★1"], store, "d4-test")
	if err != nil || run.Verdict != compliance.Admissible || len(run.Items) != 1 {
		t.Fatalf("admissibility run = %+v %v", run, err)
	}

	// 承诺台账：先定基线，正文引用。
	commit := func(in db.CommitmentInput) *db.Commitment {
		in.Code, in.Owner, in.Rationale = Uniq(in.Code), "交付负责人", "实施计划与原厂服务协议"
		if _, err := d.UpsertCommitment(ctx, tenant, task.ID, in, "bid-manager"); err != nil {
			t.Fatalf("UpsertCommitment %s: %v", in.Code, err)
		}
		c, err := d.ConfirmCommitment(ctx, tenant, task.ID, in.Code, "bid-manager")
		if err != nil {
			t.Fatalf("ConfirmCommitment %s: %v", in.Code, err)
		}
		return c
	}
	s2, star2 := p.Reqs["S2"], p.Reqs["★2"]
	a.Warranty = commit(db.CommitmentInput{Code: "C-WARRANTY", RequirementID: &s2, Metric: db.MetricWarranty, MetricLabel: "免费质保期", Unit: "年",
		TenderThresholdRaw: "不少于 3 年", CommittedRaw: "5", CommittedValue: f(5)})
	a.Duration = commit(db.CommitmentInput{Code: "C-DURATION", RequirementID: &star2, Metric: db.MetricDuration, MetricLabel: "总工期", Unit: "天",
		TenderThresholdRaw: "不超过 120 日历天", ThresholdCmp: "<=", CommittedRaw: "90 天"})

	extra := map[string]db.SaveRevisionInput{
		"1": {CommitmentIDs: []int64{a.Duration.ID}},
		"2": {AdmissibilityItemIDs: []int64{run.Items[0].ID}},
		"3": {CommitmentIDs: []int64{a.Warranty.ID}},
	}
	for _, s := range p.Spec.Sections {
		in := extra[s.Code]
		in.TaskID, in.SectionID, in.BasedOnOutlineVersion, in.Source, in.ContentMD, in.SummaryNote = task.ID, p.Sections[s.Code], p.OutlineVersion,
			db.RevHumanEdit, s.Body, "摘要"
		res, err := d.SaveSectionRevision(ctx, tenant, "writer", in)
		if err != nil || res.Outcome != db.SaveApplied {
			t.Fatalf("SaveSectionRevision %s: %v %+v", s.Code, err, res)
		}
	}
	p.ApproveAll(t)
	if res, err := d.VerifyComplianceMatrix(ctx, tenant, task.ID, 0, compliance.RuleVerifier{}, "verifier"); err != nil {
		t.Fatalf("VerifyComplianceMatrix: %v", err)
	} else if res.Verified != res.Checked {
		for _, r := range res.Rows {
			if r.VerifyStatus != compliance.StatusVerified {
				t.Errorf("matrix row %s @ %s: %s %s", r.RequirementCode, r.SectionCode, r.VerifyStatus, r.FailureReason)
			}
		}
		t.Fatalf("matrix not fully verified: %d/%d", res.Verified, res.Checked)
	}
	cons, err := d.RunConsistencyCheck(ctx, tenant, task.ID, "reviewer")
	if err != nil {
		t.Fatalf("RunConsistencyCheck: %v", err)
	}
	if len(cons.Findings) != 0 {
		t.Fatalf("consistency finding: %+v", cons.Findings[0])
	}
	p.AssertReady(t)
	return p, a
}
