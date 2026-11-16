package db

// D4 集成测试（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：成稿附表快照（评分索引 / 承诺事项 / 证明材料索引）及其
// 不可变性、以承诺台账为基准的一致性核验、提交检查清单（系统可查项、人工确认、职责边界、成稿过期后的失效与审计）。
// 导出执行用固定的产物元数据代替真实 DOCX / PDF；真实文档引擎下的页码核对见 export 包。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/compliance"
)

// confirmCommitment 录入并确认一条承诺台账。
func (f *complianceFixture) confirmCommitment(t *testing.T, in CommitmentInput) *Commitment {
	t.Helper()
	ctx := context.Background()
	if in.Owner == "" {
		in.Owner = "交付负责人"
	}
	if in.Rationale == "" {
		in.Rationale = "实施计划"
	}
	if _, err := f.d.UpsertCommitment(ctx, DefaultTenant, f.task.ID, in, "bid-manager"); err != nil {
		t.Fatalf("UpsertCommitment %s: %v", in.Code, err)
	}
	c, err := f.d.ConfirmCommitment(ctx, DefaultTenant, f.task.ID, in.Code, "bid-manager")
	if err != nil {
		t.Fatalf("ConfirmCommitment %s: %v", in.Code, err)
	}
	return c
}

func checkItem(t *testing.T, cl *SubmissionChecklist, key string) SubmissionItem {
	t.Helper()
	for _, it := range cl.Items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("checklist has no item %q: %+v", key, cl.Items)
	return SubmissionItem{}
}

// goodSelfCheck 是一份通过全部系统项的回读自检结果。
func goodSelfCheck() *ExportSelfCheck {
	return &ExportSelfCheck{Pages: 24, TOCEntries: 6, ScoreRows: 1, PageRefs: 6, PageRefsVerified: 6, VerifiedAgainst: "pdf",
		MetadataChecked: true, Author: "示例科技有限公司", LastModifiedBy: "示例科技有限公司", LayoutChecked: true, Images: 1, Tables: 2}
}

func (f *complianceFixture) finishStrict(t *testing.T, sub *ExportSubmission, check func(format string) *ExportSelfCheck) {
	t.Helper()
	ctx := context.Background()
	for range sub.Jobs {
		ref, err := f.d.ClaimExportJob(ctx, "runner", 0, f.task.ID)
		if err != nil || ref == nil {
			t.Fatalf("ClaimExportJob: %v %v", ref, err)
		}
		res := finalResult("t/" + ref.Format)
		res.SelfCheck = check(ref.Format)
		if err := f.d.CompleteExportJob(ctx, ref.ID, ref.LeaseToken, res); err != nil {
			t.Fatalf("CompleteExportJob: %v", err)
		}
	}
}

func TestD4PackageSnapshotScoreIndex(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()

	sub := f.submitExport(ctx, t, "d4-score", "h", false, ExportDOCX, ExportPDF)
	for _, j := range sub.Jobs {
		p := j.Package
		if len(p.ScoreIndex) != 1 || p.ScoreIndex[0].Code != "S1" || p.ScoreIndex[0].RequirementID != f.reqs["S1"] ||
			len(p.ScoreIndex[0].Sections) != 1 || p.ScoreIndex[0].Sections[0].SectionID != f.secs["1"] || p.ScoreIndex[0].Sections[0].Code != "1" {
			t.Fatalf("score index = %+v", p.ScoreIndex)
		}
		if len(p.Commitments) != 0 || len(p.Evidence) != 0 {
			t.Fatalf("task without ledger / evidence has appendix rows: %+v", p)
		}
	}
	// 附表快照与其他快照一样不可修改。
	if _, err := f.d.ExecContext(ctx, `UPDATE export_jobs SET package_snapshot = '{}'::jsonb WHERE id = $1`, sub.Jobs[0].ID); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("package snapshot rewrite: %v", err)
	}

	// 评分项后来被划出投标范围 / 标成价格分，都不影响已固化的快照；新请求按当时的清单取数。
	if _, err := f.d.ExecContext(ctx, `UPDATE requirement_items SET score_group = 'price' WHERE id = $1`, f.reqs["S1"]); err != nil {
		t.Fatal(err)
	}
	old, err := f.d.GetExportJob(ctx, DefaultTenant, f.task.ID, sub.Jobs[0].ID)
	if err != nil || len(old.Package.ScoreIndex) != 1 {
		t.Fatalf("existing snapshot changed: %+v %v", old, err)
	}
	// 价格评分项由报价文件响应：不进技术标的评分索引。
	// （改动评分项会让批准大纲被取代，所以这里直接按快照里的大纲取数。）
	pkg, err := buildPackageSnapshotTx(ctx, f.d, DefaultTenant, f.task.ID, old.OutlineID, old.Doc.Sections)
	if err != nil || len(pkg.ScoreIndex) != 0 {
		t.Fatalf("price item in the score index: %+v %v", pkg.ScoreIndex, err)
	}
}

func TestD4PackageSnapshotEvidenceAndCommitments(t *testing.T) {
	f, tenant, store, _ := d2Setup(t)
	ctx := context.Background()
	valid := "2027-12-31"
	cred, _, err := f.d.UpsertCompanyCredential(ctx, &CompanyCredential{TenantID: tenant, Category: CredentialQualification, Code: "QUAL-D4",
		Title: "安防资质（虚构）", SubjectName: "演示智联科技有限公司（虚构）", SubjectType: "bidder", Scope: "安防工程", CertificateNo: "AF-2026-0001",
		ValidUntil: &valid, Attributes: json.RawMessage(`{"level":"二级"}`)})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	d2Original(t, f.d, store, tenant, EvidenceDomainCredential, cred.ID, DocKindCertificate, "证书", "docx", 1)

	code, title, cat := "REQ-D4", "★资质：安防二级（截止日有效）", ReqMandatoryClause
	req, err := f.d.CreateRequirement(ctx, tenant, f.task.ID, "reviewer", RequirementInput{Code: &code, Category: &cat, Title: &title,
		Description: &title, ProofRules: &ProofRules{BidderOnly: true, ValidOnBidDeadline: true, RequiredDocs: []string{"资质证书"}, MinCount: 1}})
	if err != nil {
		t.Fatalf("requirement: %v", err)
	}
	scode, stitle, scat := "S-D4", "质保期承诺", ReqScoringItem
	score, err := f.d.CreateRequirement(ctx, tenant, f.task.ID, "reviewer", RequirementInput{Code: &scode, Category: &scat, Title: &stitle,
		Description: strpTest("质保期每延长一年得 1 分，最高 5 分。")})
	if err != nil {
		t.Fatalf("scoring requirement: %v", err)
	}
	if _, err := f.d.ConfirmRequirements(ctx, tenant, f.task.ID, "reviewer", 0); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	det, err := f.d.CreateOutline(ctx, tenant, f.task.ID, "editor", OutlineInput{Sections: []SectionInput{
		{Code: "1", Title: "资格证明", Level: 1, RequirementIDs: []int64{req.ID}},
		{Code: "2", Title: "质保与售后", Level: 1, RequirementIDs: []int64{score.ID}},
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
	if _, err := f.d.BindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainCredential, cred.ID, "d4-test"); err != nil {
		t.Fatal(err)
	}
	run, err := f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d4-test")
	if err != nil || run.Verdict != compliance.Admissible || len(run.Items) != 1 {
		t.Fatalf("run: %+v err=%v", run, err)
	}
	// 承诺台账：质保 5 年（被第 2 章引用）、价格承诺（不得进入技术标附表）、未确认的草稿（不进附表）。
	five := 5.0
	in := CommitmentInput{Code: "C-WARRANTY", RequirementID: &score.ID, Metric: MetricWarranty, MetricLabel: "免费质保期", Unit: "年",
		TenderThresholdRaw: "", CommittedRaw: "5", CommittedValue: &five, Owner: "交付负责人", Rationale: "原厂质保 + 自有维保团队"}
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, in, "bid-manager"); err != nil {
		t.Fatal(err)
	}
	warranty, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-WARRANTY", "bid-manager")
	if err != nil {
		t.Fatalf("confirm commitment: %v", err)
	}
	price := 980000.0
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, CommitmentInput{Code: "C-PRICE", Metric: MetricPrice, MetricLabel: "投标总价", Unit: "元",
		CommittedRaw: "980000", CommittedValue: &price, Owner: "商务负责人", Rationale: "成本测算"}, "bid-manager"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-PRICE", "bid-manager"); err != nil {
		t.Fatalf("confirm price: %v", err)
	}
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, CommitmentInput{Code: "C-DRAFT", RequirementID: &score.ID, Metric: MetricSLAResponse,
		MetricLabel: "响应时限", Unit: "小时", CommittedRaw: "1"}, "bid-manager"); err != nil {
		t.Fatal(err)
	}

	save := func(secID int64, body string, in SaveRevisionInput) {
		t.Helper()
		in.TaskID, in.SectionID, in.BasedOnOutlineVersion, in.Source, in.ContentMD = f.task.ID, secID, det.Outline.Version, RevHumanEdit, body
		res, err := f.d.SaveSectionRevision(ctx, tenant, "writer", in)
		if err != nil || res.Outcome != SaveApplied {
			t.Fatalf("save: %v %+v", err, res)
		}
		if _, err := f.d.ApproveSectionRevision(ctx, tenant, f.task.ID, secID, res.Revision.RevNo, "approver"); err != nil {
			t.Fatalf("approve: %v", err)
		}
	}
	save(det.Sections[0].ID, "响应要求 REQ-D4：投标人具备安防二级资质，证书在投标截止日有效。\n\n资质证书原件见附件。",
		SaveRevisionInput{AdmissibilityItemIDs: []int64{run.Items[0].ID}})
	save(det.Sections[1].ID, "响应评分项 S-D4 质保期承诺：我方承诺免费质保期 5 年，质保期内免费上门维修。", SaveRevisionInput{CommitmentIDs: []int64{warranty.ID}})
	if _, err := f.d.VerifyComplianceMatrix(ctx, tenant, f.task.ID, 0, compliance.RuleVerifier{}, "verifier"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.RunConsistencyCheck(ctx, tenant, f.task.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	sub, err := f.d.SubmitExport(ctx, tenant, f.task.ID, SubmitExportInput{RequestKey: "d4-pkg", RequestHash: "h", Formats: []string{ExportDOCX},
		DefaultTemplateHash: "tpl", Actor: "exporter"})
	if err != nil {
		var be *ExportBlockedError
		if errors.As(err, &be) {
			t.Fatalf("export blocked: %s", blockerText(be.Preflight.Readiness))
		}
		t.Fatalf("SubmitExport: %v", err)
	}
	p := sub.Jobs[0].Package
	if len(p.ScoreIndex) != 1 || p.ScoreIndex[0].Code != "S-D4" || p.ScoreIndex[0].Sections[0].Code != "2" {
		t.Fatalf("score index = %+v", p.ScoreIndex)
	}
	if len(p.Commitments) != 1 || p.Commitments[0].Code != "C-WARRANTY" || p.Commitments[0].Committed != "5" || p.Commitments[0].Unit != "年" ||
		p.Commitments[0].RequirementCode != "S-D4" || len(p.Commitments[0].Sections) != 1 || p.Commitments[0].Sections[0].Code != "2" {
		t.Fatalf("commitments = %+v (price and draft entries must be left out)", p.Commitments)
	}
	if len(p.Evidence) != 1 {
		t.Fatalf("evidence = %+v", p.Evidence)
	}
	e := p.Evidence[0]
	if e.Domain != EvidenceDomainCredential || e.EvidenceID != cred.ID || e.Name != "安防资质（虚构）" || e.Number != "AF-2026-0001" ||
		e.Subject != "演示智联科技有限公司（虚构）" || len(e.Originals) != 1 || e.Originals[0].DocumentKind != DocKindCertificate ||
		e.Originals[0].PageStart != 1 || e.Originals[0].SHA256 == "" || len(e.RequirementCodes) != 1 || e.RequirementCodes[0] != "REQ-D4" ||
		len(e.Sections) != 1 || e.Sections[0].Code != "1" {
		t.Fatalf("evidence row = %+v", e)
	}
	// 快照指纹含附表：台账改了之后同键重放仍是原快照，新键得到新指纹。
	again, err := f.d.SubmitExport(ctx, tenant, f.task.ID, SubmitExportInput{RequestKey: "d4-pkg", RequestHash: "h", Formats: []string{ExportDOCX},
		DefaultTemplateHash: "tpl", Actor: "exporter"})
	if err != nil || !again.Replayed || again.Request.SnapshotHash != sub.Request.SnapshotHash {
		t.Fatalf("replay = %+v %v", again, err)
	}
}

func TestD4ConsistencyUsesLedgerAsBaseline(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	star3 := f.reqs["★3"]
	// 章节 3 写的是「免费质保期 5 年」，台账确认的是 3 年：章节之间没有矛盾，只有与台账比对才能发现。
	f.confirmCommitment(t, CommitmentInput{Code: "C-WARRANTY", RequirementID: &star3, Metric: MetricWarranty, MetricLabel: "免费质保期", Unit: "年",
		TenderThresholdRaw: "3 年", CommittedRaw: "3 年"})
	if r, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID); err != nil || r.Ready || r.Counts[BlockConsistencyStale] != 1 {
		t.Fatalf("ledger change must stale the consistency run: %+v %v", r, err)
	}
	det := f.consistency(t)
	led := findingsBy(det, FindingLedger)
	if det.Run.LedgerMismatches != 1 || len(led) != 1 || det.Run.Contradictions != 0 {
		t.Fatalf("run = %+v findings = %+v", det.Run, det.Findings)
	}
	lf := led[0]
	if lf.ASectionID == nil || *lf.ASectionID != f.secs["3"] || lf.BSectionID != nil || lf.CommitmentCode != "C-WARRANTY" || lf.CommitmentVersion == nil ||
		lf.Metric != compliance.MetricWarranty || !strings.Contains(lf.BValue, "3 年") || !strings.Contains(lf.Detail, "C-WARRANTY") || lf.Status != FindingOpen {
		t.Fatalf("ledger finding = %+v", lf)
	}
	r, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID)
	if err != nil || r.Ready || r.Counts[BlockFindingOpen] != 1 {
		t.Fatalf("open ledger mismatch must block export: %+v %v", r, err)
	}
	// 修复方式一：正文改成与台账一致（重写、批准、核验后恢复）。
	sec := f.section(t, f.secs["3"])
	f.save(t, f.secs["3"], sec.Section.CurrentRevNo, f.ver, RevHumanEdit, "我方承诺免费质保期 3 年，满足招标要求。", 0)
	sec = f.section(t, f.secs["3"])
	if _, err := f.d.ApproveSectionRevision(ctx, DefaultTenant, f.task.ID, f.secs["3"], sec.Section.CurrentRevNo, "approver"); err != nil {
		t.Fatal(err)
	}
	f.verify(t, 0)
	det = f.consistency(t)
	if det.Run.LedgerMismatches != 0 || len(findingsBy(det, FindingLedger)) != 0 {
		t.Fatalf("after fixing the text: %+v", det.Findings)
	}
	consistencyClear(t, f, "after fixing the text")
	// 台账里没有对应指标（工期）或值不可比时不产生结论，并在运行摘要里如实记录不可比的条目。
	star1 := f.reqs["★1"]
	f.confirmCommitment(t, CommitmentInput{Code: "C-DURATION", RequirementID: &star1, Metric: MetricDuration, MetricLabel: "总工期", Unit: "批",
		CommittedRaw: "分两批交付", CommittedValue: floatp(2)})
	det = f.consistency(t)
	var summary struct {
		Ledger       int      `json:"ledger_commitments"`
		Uncomparable []string `json:"ledger_uncomparable"`
	}
	if err := json.Unmarshal(det.Run.Summary, &summary); err != nil || summary.Ledger != 2 || len(summary.Uncomparable) != 1 || summary.Uncomparable[0] != "C-DURATION" ||
		det.Run.LedgerMismatches != 0 {
		t.Fatalf("summary = %+v run = %+v err = %v", summary, det.Run, err)
	}
	// 修复方式二：确认是误报时由人处理结论（与矛盾结论同一流程）。
	f.confirmCommitment(t, CommitmentInput{Code: "C-DURATION", RequirementID: &star1, Metric: MetricDuration, MetricLabel: "总工期", Unit: "天",
		TenderThresholdRaw: "120 天", ThresholdCmp: "<=", CommittedRaw: "60 天"})
	det = f.consistency(t)
	led = findingsBy(det, FindingLedger)
	if len(led) == 0 || led[0].CommitmentCode != "C-DURATION" {
		t.Fatalf("duration mismatch not found: %+v", det.Findings)
	}
	for _, x := range led {
		if _, err := f.d.ResolveConsistencyFinding(ctx, DefaultTenant, f.task.ID, x.ID, ResolveFindingInput{Kind: "false_positive",
			Note: "正文为分阶段工期，台账为总工期", Version: x.Version}, "reviewer"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	consistencyClear(t, f, "after resolving")
}

// consistencyClear 断言导出就绪度里没有一致性类阻断。（台账一旦存在，D2 证据门禁随之启用，夹具里带证明规则的要求会另有
// 可采性阻断，与本用例无关，所以不直接断言 Ready。）
func consistencyClear(t *testing.T, f *complianceFixture, when string) {
	t.Helper()
	r, err := f.d.ExportReadiness(context.Background(), DefaultTenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{BlockConsistencyMissing, BlockConsistencyStale, BlockFindingOpen} {
		if r.Counts[k] != 0 {
			t.Fatalf("%s: consistency still blocks export (%s): %s", when, k, blockerText(r))
		}
	}
}

func TestD4SubmissionChecklist(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	set := func(reqID int64, key string, in SubmissionCheckInput, actor string) (*SubmissionChecklist, error) {
		return f.d.SetSubmissionCheck(ctx, DefaultTenant, f.task.ID, reqID, key, in, actor)
	}

	// 预览稿：清单可读，但不是最终稿，不能确认。
	preview := f.submitExport(ctx, t, "d4-preview", "hp", false, ExportDOCX)
	cl, err := f.d.SubmissionChecklist(ctx, DefaultTenant, f.task.ID, preview.Request.ID)
	if err != nil || cl.Final || cl.Ready || checkItem(t, cl, "final_draft").Status != CheckFail {
		t.Fatalf("preview checklist = %+v %v", cl, err)
	}
	if _, err := set(preview.Request.ID, "cover_check", SubmissionCheckInput{Status: CheckConfirmed}, "张三"); !errors.Is(err, ErrSubmissionNotFinal) {
		t.Fatalf("confirming a preview: %v", err)
	}
	if _, _, err := f.d.CancelExportRequest(ctx, DefaultTenant, f.task.ID, preview.Request.ID, "tester"); err != nil {
		t.Fatalf("cancel preview: %v", err)
	}

	// 严格最终稿，两种格式；投标人已填。
	sub, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "d4-final", RequestHash: "hf",
		Formats: []string{ExportDOCX, ExportPDF}, DefaultTemplateHash: "tpl", StrictRuntime: true, Executor: "WPS 12.1.0.29161",
		Bidder: "示例科技有限公司", Actor: "exporter"})
	if err != nil {
		t.Fatalf("SubmitExport: %v", err)
	}
	rid := sub.Request.ID
	// 作业尚未完成：不是最终稿，系统项不通过。
	cl, _ = f.d.SubmissionChecklist(ctx, DefaultTenant, f.task.ID, rid)
	if cl.Final || cl.Ready || checkItem(t, cl, "toc_pages").Status != CheckFail {
		t.Fatalf("checklist before completion = %+v", cl)
	}
	f.finishStrict(t, sub, func(string) *ExportSelfCheck { return goodSelfCheck() })
	cl, err = f.d.SubmissionChecklist(ctx, DefaultTenant, f.task.ID, rid)
	if err != nil || !cl.Final || !cl.Current || cl.Ready || len(cl.ArtifactHashes) != 2 || cl.Version != SubmissionChecklistVersion {
		t.Fatalf("final checklist = %+v %v", cl, err)
	}
	for _, key := range []string{"final_draft", "current_content", "bidder_name", "toc_pages", "score_index", "layout", "page_numbers", "metadata"} {
		if it := checkItem(t, cl, key); it.Status != CheckPass || it.Kind != CheckAuto || it.Confirmable {
			t.Fatalf("auto item %s = %+v", key, it)
		}
	}
	// 内置模板：需要人确认知悉；任务没有引用企业证据，清单里没有“证明材料原件已附入”。
	if it := checkItem(t, cl, "own_template"); it.Status != CheckAttention || !it.Confirmable {
		t.Fatalf("own_template = %+v", it)
	}
	for _, it := range cl.Items {
		if it.Key == "evidence_attached" {
			t.Fatal("evidence item listed for a task without company evidence")
		}
	}
	manual := []string{"cover_check", "price_isolation", "signature_seal", "platform_import", "platform_upload"}
	if cl.Open != len(manual)+1 {
		t.Fatalf("open = %d, want %d", cl.Open, len(manual)+1)
	}

	// 确认规则。
	for name, tc := range map[string]struct {
		key   string
		in    SubmissionCheckInput
		actor string
		want  error
	}{
		"unknown item":               {"nope", SubmissionCheckInput{Status: CheckConfirmed}, "张三", ErrSubmissionCheckInvalid},
		"system pass item":           {"toc_pages", SubmissionCheckInput{Status: CheckConfirmed, Note: "看过了"}, "张三", ErrSubmissionCheckInvalid},
		"no operator":                {"cover_check", SubmissionCheckInput{Status: CheckConfirmed}, " ", ErrSubmissionCheckInvalid},
		"unknown status":             {"cover_check", SubmissionCheckInput{Status: "ok"}, "张三", ErrSubmissionCheckInvalid},
		"failed without note":        {"cover_check", SubmissionCheckInput{Status: CheckFailed}, "张三", ErrSubmissionCheckInvalid},
		"n/a without note":           {"platform_upload", SubmissionCheckInput{Status: CheckNotApplicable}, "张三", ErrSubmissionCheckInvalid},
		"attention without note":     {"own_template", SubmissionCheckInput{Status: CheckConfirmed}, "张三", ErrSubmissionCheckInvalid},
		"attention marked n/a":       {"own_template", SubmissionCheckInput{Status: CheckNotApplicable, Note: "x"}, "张三", ErrSubmissionCheckInvalid},
		"withdraw nothing":           {"cover_check", SubmissionCheckInput{}, "张三", ErrSubmissionCheckInvalid},
		"stale expected version":     {"cover_check", SubmissionCheckInput{Status: CheckConfirmed, ExpectedVersion: 3}, "张三", ErrRevisionConflict},
		"request of another task id": {"cover_check", SubmissionCheckInput{Status: CheckConfirmed}, "张三", ErrNotFound},
	} {
		req := rid
		if name == "request of another task id" {
			req = rid + 100000
		}
		if _, err := set(req, tc.key, tc.in, tc.actor); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if n := f.countRows(t, `SELECT count(*) FROM submission_checks WHERE export_request_id = $1`, rid); n != 0 {
		t.Fatalf("rejected confirmations left %d rows", n)
	}

	// 逐项确认；平台上传记为未通过后改判为通过（版本递增，事件各记一条）。
	for _, key := range manual[:4] {
		if cl, err = set(rid, key, SubmissionCheckInput{Status: CheckConfirmed, Reference: "凭证-" + key}, "张三"); err != nil {
			t.Fatalf("confirm %s: %v", key, err)
		}
	}
	if cl, err = set(rid, "platform_upload", SubmissionCheckInput{Status: CheckFailed, Note: "上传超时，未取得回执"}, "张三"); err != nil || cl.Ready {
		t.Fatalf("failed manual item: %+v %v", cl, err)
	}
	if it := checkItem(t, cl, "platform_upload"); it.Status != CheckFailed || it.Version != 1 || it.ConfirmedBy != "张三" || it.ConfirmedAt == nil {
		t.Fatalf("platform_upload = %+v", it)
	}
	if cl, err = set(rid, "platform_upload", SubmissionCheckInput{Status: CheckConfirmed, Reference: "回执 HZ-20261111-0042", ExpectedVersion: 1}, "李四"); err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	if it := checkItem(t, cl, "platform_upload"); it.Status != CheckConfirmed || it.Version != 2 || it.ConfirmedBy != "李四" || it.Reference != "回执 HZ-20261111-0042" {
		t.Fatalf("platform_upload after re-confirm = %+v", it)
	}
	if cl.Ready || cl.Open != 1 {
		t.Fatalf("attention item still open: ready=%v open=%d", cl.Ready, cl.Open)
	}
	if cl, err = set(rid, "own_template", SubmissionCheckInput{Status: CheckConfirmed, Note: "招标文件未限定版式，经投标经理同意使用通用版式"}, "李四"); err != nil || !cl.Ready || cl.Open != 0 {
		t.Fatalf("after acknowledging the template: %+v %v", cl, err)
	}
	// 确认记录绑定当时的产物哈希。
	var hashes string
	if err := f.d.QueryRowContext(ctx, `SELECT artifact_hashes::text FROM submission_checks WHERE export_request_id = $1 AND item_key = 'platform_upload'`, rid).Scan(&hashes); err != nil ||
		!strings.Contains(hashes, "sha-t/docx") || !strings.Contains(hashes, "sha-t/pdf") {
		t.Fatalf("artifact hashes = %s %v", hashes, err)
	}
	// 撤回一项后不再就绪；审计事件完整且不可改写。
	if cl, err = set(rid, "signature_seal", SubmissionCheckInput{}, "李四"); err != nil || cl.Ready || checkItem(t, cl, "signature_seal").Status != CheckPending {
		t.Fatalf("withdraw: %+v %v", cl, err)
	}
	events, err := f.d.ListSubmissionCheckEvents(ctx, DefaultTenant, f.task.ID, rid, 0)
	if err != nil || len(events) != 8 || events[0].Action != "withdrawn" || events[0].ItemKey != "signature_seal" || events[0].Actor != "李四" {
		t.Fatalf("events = %d %+v %v", len(events), events, err)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE submission_check_events SET note = 'x' WHERE id = $1`, events[0].ID); err == nil {
		t.Fatal("audit event was rewritten")
	}
	// 成功作业的自检结果与产物哈希不可再改：确认始终对应同一份文件。
	for _, q := range []string{`UPDATE export_jobs SET self_check = '{}'::jsonb WHERE id = $1`, `UPDATE export_jobs SET artifact_hash = 'other' WHERE id = $1`} {
		if _, err := f.d.ExecContext(ctx, q, sub.Jobs[0].ID); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err = set(rid, "signature_seal", SubmissionCheckInput{Status: CheckConfirmed}, "张三"); err != nil {
		t.Fatal(err)
	}

	// 导出之后正文又改了：成稿不再是当前内容，清单不就绪，新的确认被拒绝，已有记录保留、可撤回。
	sec := f.section(t, f.secs["2"])
	f.save(t, f.secs["2"], sec.Section.CurrentRevNo, f.ver, RevHumanEdit, "我方承诺提供 7×24 小时运维服务，故障响应时间不超过 30 分钟。", 0)
	cl, err = f.d.SubmissionChecklist(ctx, DefaultTenant, f.task.ID, rid)
	if err != nil || cl.Current || cl.Ready || !cl.Final || checkItem(t, cl, "current_content").Status != CheckFail || !strings.Contains(cl.StaleReason, "章节 2") {
		t.Fatalf("after editing a section: %+v %v", cl, err)
	}
	if _, err := set(rid, "cover_check", SubmissionCheckInput{Status: CheckConfirmed}, "张三"); !errors.Is(err, ErrSubmissionNotFinal) {
		t.Fatalf("confirming an outdated draft: %v", err)
	}
	if cl, err = set(rid, "cover_check", SubmissionCheckInput{}, "张三"); err != nil || checkItem(t, cl, "cover_check").Status != CheckPending {
		t.Fatalf("withdraw on an outdated draft: %+v %v", cl, err)
	}
}

// 回读自检不合格或缺失时，系统项不通过，且不能由人工确认放行。
func TestD4SubmissionChecklistSystemFailures(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	sub, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "d4-bad", RequestHash: "hb",
		Formats: []string{ExportDOCX, ExportPDF}, DefaultTemplateHash: "tpl", StrictRuntime: true, Executor: "WPS 12.1.0.29161", Actor: "exporter"})
	if err != nil {
		t.Fatal(err)
	}
	f.finishStrict(t, sub, func(format string) *ExportSelfCheck {
		if format == ExportDOCX {
			return nil // 旧版本执行器：没有自检记录
		}
		sc := goodSelfCheck()
		sc.PageRefsVerified, sc.PageRefIssues = 4, 2
		sc.MetadataIssues, sc.LastModifiedBy = 1, "WPS_1639839416"
		sc.OutOfBounds, sc.BlankPages = 3, 1
		return sc
	})
	cl, err := f.d.SubmissionChecklist(ctx, DefaultTenant, f.task.ID, sub.Request.ID)
	if err != nil || !cl.Final || cl.Ready {
		t.Fatalf("checklist = %+v %v", cl, err)
	}
	for _, key := range []string{"bidder_name", "toc_pages", "score_index", "metadata", "layout", "page_numbers"} {
		it := checkItem(t, cl, key)
		if it.Status != CheckFail || it.Confirmable {
			t.Fatalf("%s = %+v, want fail and not confirmable", key, it)
		}
		if _, err := f.d.SetSubmissionCheck(ctx, DefaultTenant, f.task.ID, sub.Request.ID, key, SubmissionCheckInput{Status: CheckConfirmed, Note: "放行"}, "张三"); !errors.Is(err, ErrSubmissionCheckInvalid) {
			t.Fatalf("hand-waving %s: %v", key, err)
		}
	}
	if d := checkItem(t, cl, "score_index").Detail; !strings.Contains(d, "DOCX 没有回读自检记录") || !strings.Contains(d, "核对一致 4 处") {
		t.Fatalf("score_index detail = %q", d)
	}
	// 只有 PDF 的请求：版式提示是 attention（可确认知悉），不是 fail。
	one, err := f.d.SubmitExport(ctx, DefaultTenant, f.task.ID, SubmitExportInput{RequestKey: "d4-layout", RequestHash: "hl", Formats: []string{ExportPDF},
		DefaultTemplateHash: "tpl", StrictRuntime: true, Executor: "WPS 12.1.0.29161", Bidder: "示例科技有限公司", Actor: "exporter"})
	if err != nil {
		t.Fatal(err)
	}
	f.finishStrict(t, one, func(string) *ExportSelfCheck {
		sc := goodSelfCheck()
		sc.OutOfBounds, sc.FooterIssues = 2, 1
		return sc
	})
	cl, _ = f.d.SubmissionChecklist(ctx, DefaultTenant, f.task.ID, one.Request.ID)
	if l, p := checkItem(t, cl, "layout"), checkItem(t, cl, "page_numbers"); l.Status != CheckAttention || !l.Confirmable || p.Status != CheckAttention {
		t.Fatalf("layout = %+v page_numbers = %+v", l, p)
	}
	if cl, err = f.d.SetSubmissionCheck(ctx, DefaultTenant, f.task.ID, one.Request.ID, "layout", SubmissionCheckInput{Status: CheckConfirmed,
		Note: "两处为宽表右边框贴边，已逐页翻阅"}, "张三"); err != nil || checkItem(t, cl, "layout").Status != CheckConfirmed {
		t.Fatalf("acknowledge layout: %+v %v", cl, err)
	}
}

// 台账里的阈值、承诺值、能力上限必须同口径：从原文解析出来的数换算到声明的单位，而不是另行归一到天 / 分钟。
func TestCommitmentParsedValuesUseTheDeclaredUnit(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	star3 := f.reqs["★3"]
	five := 5.0
	// 阈值只给原文、承诺值直接填数：此前被比成 5 < 1095（天）而拒绝确认。
	c, err := f.d.UpsertCommitment(ctx, DefaultTenant, f.task.ID, CommitmentInput{Code: "C-UNIT", RequirementID: &star3, Metric: MetricWarranty,
		MetricLabel: "免费质保期", Unit: "年", TenderThresholdRaw: "不少于 3 年", CommittedRaw: "5", CommittedValue: &five, Owner: "交付负责人",
		Rationale: "原厂质保"}, "bid-manager")
	if err != nil || c.TenderThresholdValue == nil || *c.TenderThresholdValue != 3 || *c.CommittedValue != 5 {
		t.Fatalf("upsert = %+v %v (threshold must be 3, in years)", c, err)
	}
	if _, err := f.d.ConfirmCommitment(ctx, DefaultTenant, f.task.ID, "C-UNIT", "bid-manager"); err != nil {
		t.Fatalf("5 年 against 不少于 3 年 must confirm: %v", err)
	}
	// 两个值都从原文解析、单位不同于原文：换算后仍然可比（6 个月 < 3 年）。
	c, err = f.d.UpsertCommitment(ctx, DefaultTenant, f.task.ID, CommitmentInput{Code: "C-UNIT-LOW", RequirementID: &star3, Metric: MetricWarranty,
		MetricLabel: "免费质保期", Unit: "年", TenderThresholdRaw: "不少于 36 个月", CommittedRaw: "6 个月", Owner: "交付负责人", Rationale: "原厂质保"}, "bid-manager")
	if err != nil || *c.TenderThresholdValue < 2.9 || *c.TenderThresholdValue > 3.0 || *c.CommittedValue > 0.5 {
		t.Fatalf("upsert = %+v %v", c, err)
	}
	if _, err := f.d.ConfirmCommitment(ctx, DefaultTenant, f.task.ID, "C-UNIT-LOW", "bid-manager"); !errors.Is(err, ErrCommitmentBelowThreshold) {
		t.Fatalf("6 个月 against 不少于 36 个月: %v", err)
	}
	// 没有声明单位：保持归一值（天），两边同样可比。
	c, err = f.d.UpsertCommitment(ctx, DefaultTenant, f.task.ID, CommitmentInput{Code: "C-NOUNIT", RequirementID: &star3, Metric: MetricWarranty,
		MetricLabel: "免费质保期", TenderThresholdRaw: "不少于 3 年", CommittedRaw: "5 年", Owner: "交付负责人", Rationale: "原厂质保"}, "bid-manager")
	if err != nil || *c.TenderThresholdValue != 3*365 || *c.CommittedValue != 5*365 {
		t.Fatalf("upsert without unit = %+v %v", c, err)
	}
	// 一致性比对读的是同一口径：5 年的台账与正文「免费质保期 5 年」一致。
	det := f.consistency(t)
	if n := len(findingsBy(det, FindingLedger)); n != 0 {
		t.Fatalf("ledger mismatches = %d: %+v", n, det.Findings)
	}
}

// 按旧规则版本完成的一致性运行（升级前的结论，没有与承诺台账比对过）不算当前结论：导出被阻断，重新核验后恢复。
func TestD4OldRuleVersionConsistencyRunIsStale(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	det := f.consistency(t)
	if det.Run.RuleVersion != compliance.ConsistencyRuleVersion || !det.Current {
		t.Fatalf("new run = %+v current=%v", det.Run, det.Current)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE consistency_runs SET rule_version = 'consistency-v1' WHERE id = $1`, det.Run.ID); err != nil {
		t.Fatal(err)
	}
	old, err := f.d.GetConsistencyRun(ctx, DefaultTenant, f.task.ID, det.Run.ID, "")
	if err != nil || old.Current || old.Run.Status != RunCompleted {
		t.Fatalf("v1 run = %+v current=%v err=%v", old.Run, old.Current, err)
	}
	r, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID)
	if err != nil || r.Ready || r.Counts[BlockConsistencyStale] != 1 || !strings.Contains(blockerText(r), "核验规则已升级") {
		t.Fatalf("readiness with a v1 run = %s (%v)", blockerText(r), err)
	}
	f.consistency(t)
	if r, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID); err != nil || !r.Ready {
		t.Fatalf("not ready after re-running: %s (%v)", blockerText(r), err)
	}
}
