package db

// D2 集成测试（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：证据原件与可采性、人工复核、
// 失效传播、承诺台账状态机、报价定点勾稽。所有数据为虚构测试数据。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/evidence"
)

func d2Setup(t *testing.T) (*fixture, string, *evidence.Store, time.Time) {
	t.Helper()
	f := newFixture(t)
	tenant := uniq("d2-tenant")
	ctx := context.Background()
	if _, err := f.d.RequirementManifest(ctx, tenant, f.task.ID); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	deadline := time.Date(2026, 12, 5, 9, 0, 0, 0, beijing)
	if _, err := f.d.ExecContext(ctx, `UPDATE requirement_manifests SET bid_deadline=$3 WHERE tenant_id=$1 AND task_id=$2`,
		tenant, f.task.ID, deadline); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	store := evidence.New(t.TempDir())
	if _, _, err := f.d.UpsertCompanyCredential(ctx, &CompanyCredential{TenantID: tenant, Category: "company_profile", Code: "profile",
		Title: "企业主体（虚构）", Attributes: json.RawMessage(`{"legal_name":"演示智联科技有限公司（虚构）"}`),
		SubjectName: "演示智联科技有限公司（虚构）", SubjectType: "bidder"}); err != nil {
		t.Fatalf("profile: %v", err)
	}
	return f, tenant, store, deadline
}

func d2Req(t *testing.T, f *fixture, tenant, code, title string, cat string, rules *ProofRules, evalKind string, weight float64) *RequirementItem {
	t.Helper()
	in := RequirementInput{Code: &code, Category: &cat, Title: &title, Description: &title, ProofRules: rules}
	if evalKind != "" {
		in.EvalKind = &evalKind
	}
	if weight > 0 {
		in.ScoreWeight = &weight
		group := ScoreGroupTechnical
		in.ScoreGroup = &group
	}
	it, err := f.d.CreateRequirement(context.Background(), tenant, f.task.ID, "d2-test", in)
	if err != nil {
		t.Fatalf("create requirement %s: %v", code, err)
	}
	return it
}

func d2Original(t *testing.T, d *DB, store *evidence.Store, tenant, domain string, ownerID int64, kind, label, ext string, page int) *CompanyEvidenceOriginal {
	t.Helper()
	rel, sum, err := store.Put(tenant, ext, []byte(kind+"-"+label+"-"+uniq("orig")))
	if err != nil {
		t.Fatalf("put original: %v", err)
	}
	o, err := d.AddCompanyEvidenceOriginal(context.Background(), &CompanyEvidenceOriginal{TenantID: tenant, OwnerDomain: domain, OwnerID: ownerID,
		DocumentKind: kind, PageLabel: label, OriginalPath: rel, OriginalSHA256: sum, PageStart: page, PageEnd: page + 1, CreatedBy: "d2-test"}, false)
	if err != nil {
		t.Fatalf("add original: %v", err)
	}
	return o
}

func TestD2CredentialTenantIsolationAndAdmissibility(t *testing.T) {
	f, tenant, store, _ := d2Setup(t)
	ctx := context.Background()
	other := uniq("d2-tenant")
	valid := "2027-12-31"
	a, _, err := f.d.UpsertCompanyCredential(ctx, &CompanyCredential{TenantID: tenant, Category: CredentialQualification, Code: "QUAL-1",
		Title: "安防资质（虚构）", SubjectName: "演示智联科技有限公司（虚构）", SubjectType: "bidder", Scope: "安防工程设计施工",
		ValidUntil: &valid, Attributes: json.RawMessage(`{"level":"二级"}`)})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	b, created, err := f.d.UpsertCompanyCredential(ctx, &CompanyCredential{TenantID: other, Category: CredentialQualification, Code: "QUAL-1",
		Title: "其他租户资质（虚构）", SubjectName: "其他企业（虚构）", SubjectType: "bidder", ValidUntil: &valid})
	if err != nil || !created || b.ID == a.ID {
		t.Fatalf("tenant isolation upsert: %+v created=%v err=%v", b, created, err)
	}
	listA, _ := f.d.ListCompanyCredentials(ctx, CredentialFilter{Tenant: tenant, Terms: []string{"QUAL-1"}})
	if len(listA) != 1 || listA[0].ID != a.ID || listA[0].TenantID != tenant {
		t.Fatalf("tenant filter: %+v", listA)
	}
	d2Original(t, f.d, store, tenant, EvidenceDomainCredential, a.ID, DocKindCertificate, "证书首页", "docx", 1)

	req := d2Req(t, f, tenant, "REQ-QUAL", "★资质要求：安防二级，截止日有效", ReqMandatoryClause,
		&ProofRules{BidderOnly: true, ValidOnBidDeadline: true, RequiredDocs: []string{"资质证书"}, ScopeKeywords: []string{"安防"}, MinCount: 1}, "", 0)
	if _, err := f.d.BindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainCredential, a.ID, "d2-test"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	run, err := f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d2-test")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if run.Verdict != compliance.Admissible || len(run.Items) != 1 || run.Items[0].Verdict != compliance.Admissible {
		t.Fatalf("expected admissible run: %+v items=%+v", run, run.Items)
	}
	if run.BidDeadline == nil || !run.BidDeadline.Equal(time.Date(2026, 12, 5, 9, 0, 0, 0, beijing)) {
		t.Fatalf("bid deadline: %v", run.BidDeadline)
	}

	// 证据源更新 -> 当前运行 stale，不留下可直接引用的 current 结论。
	expired := "2026-01-01"
	a.ValidUntil = &expired
	if _, err := f.d.UpdateCompanyCredential(ctx, a); err != nil {
		t.Fatalf("update credential: %v", err)
	}
	if _, err := f.d.CurrentAdmissibilityRun(ctx, tenant, f.task.ID, req.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale run must not remain current: %v", err)
	}
	run2, err := f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d2-test")
	if err != nil {
		t.Fatalf("re-evaluate: %v", err)
	}
	if run2.Verdict != compliance.Inadmissible || run2.Version != 2 {
		t.Fatalf("expired credential must be inadmissible: verdict=%s version=%d", run2.Verdict, run2.Version)
	}
	if run2.ID == run.ID {
		t.Fatal("new evaluation must create a new run")
	}
	old, err := f.d.CurrentAdmissibilityRun(ctx, tenant, f.task.ID, req.ID)
	if err != nil || old.ID != run2.ID {
		t.Fatalf("current run: %+v err=%v", old, err)
	}
}

func TestD2AdmissibilityPerformanceManualAndMinCount(t *testing.T) {
	f, tenant, store, _ := d2Setup(t)
	ctx := context.Background()
	rules := &ProofRules{BidderOnly: true, NeedAcceptance: true, DateAnchor: "acceptance_date", YearsWindow: 3,
		RequiredDocs: []string{"合同", "验收报告"}, ContractPages: []string{"标的页", "金额页"}, MinCount: 2,
		ScopeKeywords: []string{"视频监控"}}

	addPerf := func(code, subjectType string, subsidiary bool, accept bool) *CompanyPerformance {
		t.Helper()
		contract, acceptance := "2025-01-10", "2025-06-20"
		var acc *string
		if accept {
			acc = &acceptance
		}
		p, _, err := f.d.UpsertCompanyPerformance(ctx, &CompanyPerformance{TenantID: tenant, Code: code, ProjectName: "虚构安防项目 " + code,
			Buyer: "虚构采购人", ContractNo: "HT-" + code, ContractDate: &contract, AcceptanceDate: acc, Amount: "8600000.0000",
			SubjectName: "演示智联科技有限公司（虚构）", SubjectType: subjectType, IsSubsidiary: subsidiary, Scope: "视频监控与门禁系统"})
		if err != nil {
			t.Fatalf("performance %s: %v", code, err)
		}
		d2Original(t, f.d, store, tenant, EvidenceDomainPerformance, p.ID, DocKindContract, "合同标的页", "docx", 1)
		d2Original(t, f.d, store, tenant, EvidenceDomainPerformance, p.ID, DocKindContract, "合同金额页", "docx", 3)
		if accept {
			d2Original(t, f.d, store, tenant, EvidenceDomainPerformance, p.ID, DocKindAcceptanceReport, "验收结论", "docx", 1)
		}
		return p
	}
	req := d2Req(t, f, tenant, "REQ-PERF", "类似项目业绩：近三年 2 个", ReqScoringItem, rules, EvalObjective, 30)

	// 子公司业绩：bidder_only 失败。
	sub := addPerf("SUB-1", "subsidiary", true, true)
	if _, err := f.d.BindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainPerformance, sub.ID, "d2-test"); err != nil {
		t.Fatal(err)
	}
	run, err := f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d2-test")
	if err != nil {
		t.Fatalf("evaluate subsidiary: %v", err)
	}
	if run.Verdict != compliance.Inadmissible {
		t.Fatalf("subsidiary performance must be inadmissible: %+v", run)
	}
	if err := f.d.UnbindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainPerformance, sub.ID); err != nil {
		t.Fatal(err)
	}

	// 缺验收：need_acceptance / required_docs 失败。
	noAccept := addPerf("NOACC-1", "bidder", false, false)
	if _, err := f.d.BindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainPerformance, noAccept.ID, "d2-test"); err != nil {
		t.Fatal(err)
	}
	run, err = f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d2-test")
	if err != nil || run.Verdict != compliance.Inadmissible {
		t.Fatalf("missing acceptance must be inadmissible: %+v err=%v", run, err)
	}
	_ = f.d.UnbindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainPerformance, noAccept.ID)

	// 一条可采 + MinCount=2 -> 要求级 inadmissible，原因 min_count。
	ok1 := addPerf("OK-1", "bidder", false, true)
	if _, err := f.d.BindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainPerformance, ok1.ID, "d2-test"); err != nil {
		t.Fatal(err)
	}
	run, err = f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d2-test")
	if err != nil || run.Verdict != compliance.Inadmissible {
		t.Fatalf("min_count shortage: %+v err=%v", run, err)
	}
	foundMin := false
	for _, c := range run.Checks {
		if c.Rule == "min_count" && c.Status == "fail" {
			foundMin = true
		}
	}
	if !foundMin {
		t.Fatalf("min_count check missing: %+v", run.Checks)
	}

	// 补足第二条可采业绩 -> admissible。
	ok2 := addPerf("OK-2", "bidder", false, true)
	if _, err := f.d.BindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainPerformance, ok2.ID, "d2-test"); err != nil {
		t.Fatal(err)
	}
	run, err = f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d2-test")
	if err != nil || run.Verdict != compliance.Admissible {
		t.Fatalf("two valid performances: %+v err=%v", run, err)
	}

	// manual：主体未知 -> 人工复核生成新运行。
	manualReq := d2Req(t, f, tenant, "REQ-MANUAL", "同类项目范围待人工确认", ReqScoringItem, &ProofRules{BidderOnly: true, MinCount: 1}, "", 0)
	unknown, _, err := f.d.UpsertCompanyPerformance(ctx, &CompanyPerformance{TenantID: tenant, Code: "UNK-1", ProjectName: "虚构未知主体项目",
		SubjectName: "", SubjectType: "unknown", Scope: "视频监控", ContractDate: strpTest("2025-01-01"), AcceptanceDate: strpTest("2025-06-01")})
	if err != nil {
		t.Fatal(err)
	}
	d2Original(t, f.d, store, tenant, EvidenceDomainPerformance, unknown.ID, DocKindContract, "合同", "docx", 1)
	if _, err := f.d.BindRequirementEvidence(ctx, tenant, f.task.ID, manualReq.ID, EvidenceDomainPerformance, unknown.ID, "d2-test"); err != nil {
		t.Fatal(err)
	}
	mrun, err := f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, manualReq.ID, store, "d2-test")
	if err != nil || mrun.Verdict != compliance.Manual {
		t.Fatalf("unknown subject must be manual: %+v err=%v", mrun, err)
	}
	if len(mrun.Items) != 1 || mrun.Items[0].Verdict != compliance.Manual {
		t.Fatalf("manual item missing: %+v", mrun.Items)
	}
	resolved, err := f.d.ResolveAdmissibility(ctx, tenant, f.task.ID, mrun.ID, []AdmissibilityDecision{
		{ItemID: mrun.Items[0].ID, Verdict: compliance.Admissible, Reason: "人工核对合同主体为投标人自身（虚构演示）"},
	}, "reviewer")
	if err != nil || resolved.Verdict != compliance.Admissible || resolved.Version != mrun.Version+1 {
		t.Fatalf("resolve: %+v err=%v", resolved, err)
	}
	old, _ := f.d.ListAdmissibilityRuns(ctx, tenant, f.task.ID, manualReq.ID, 10)
	if len(old) < 2 || old[1].Status != "superseded" {
		t.Fatalf("old run must be superseded: %+v", old)
	}
}

func strpTest(s string) *string { return &s }

func TestD2CommitmentStateMachine(t *testing.T) {
	f, tenant, _, _ := d2Setup(t)
	ctx := context.Background()
	req := d2Req(t, f, tenant, "REQ-DUR", "工期要求不超过 90 天", ReqMandatoryClause, &ProofRules{}, "", 0)
	v := 90.0
	below := 120.0
	cap := 90.0
	in := CommitmentInput{Code: "C-DUR", RequirementID: &req.ID, Metric: MetricDuration, MetricLabel: "总工期", Unit: "天",
		ThresholdCmp: "<=", TenderThresholdRaw: "不超过 90 日历天", TenderThresholdValue: &v, CommittedRaw: "90 天",
		CommittedValue: &below, CapabilityLimit: &cap, CapabilityBasis: "自有施工班组", Owner: "项目经理", Rationale: "实施计划"}
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, in, "creator"); err != nil {
		t.Fatalf("upsert draft: %v", err)
	}
	if _, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-DUR", "creator"); !errors.Is(err, ErrCommitmentBelowThreshold) {
		t.Fatalf("below threshold must be rejected: %v", err)
	}
	in.CommittedRaw = "90 天"
	in.CommittedValue = &v
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, in, "creator"); err != nil {
		t.Fatalf("fix commitment: %v", err)
	}
	// 超能力：团队 20 人 > 能力上限 12 人。
	teamReq := d2Req(t, f, tenant, "REQ-TEAM", "团队不少于 12 人", ReqMandatoryClause, &ProofRules{}, "", 0)
	min := 12.0
	committed := 20.0
	capTeam := 12.0
	team := CommitmentInput{Code: "C-TEAM", RequirementID: &teamReq.ID, Metric: MetricTeamSize, MetricLabel: "团队人数", Unit: "人",
		ThresholdCmp: ">=", TenderThresholdRaw: "不少于 12 人", TenderThresholdValue: &min, CommittedRaw: "20 人",
		CommittedValue: &committed, CapabilityLimit: &capTeam, CapabilityBasis: "在岗人员 12 人", Owner: "交付负责人", Rationale: "人员名册"}
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, team, "creator"); err != nil {
		t.Fatalf("team upsert: %v", err)
	}
	if _, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-TEAM", "creator"); !errors.Is(err, ErrCommitmentSignoffRequired) {
		t.Fatalf("over capability must require signoff: %v", err)
	}
	if _, err := f.d.SignOffCommitment(ctx, tenant, f.task.ID, "C-TEAM", "creator", "自签"); !errors.Is(err, ErrCommitmentSignoffRequired) {
		t.Fatalf("self sign-off must be rejected: %v", err)
	}
	if _, err := f.d.SignOffCommitment(ctx, tenant, f.task.ID, "C-TEAM", "delivery-lead", "已核对在岗人员与外包补充计划"); err != nil {
		t.Fatalf("signoff: %v", err)
	}
	signed, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-TEAM", "creator")
	if err != nil || signed.Status != CommitmentSignedOff || signed.SignedOffBy != "delivery-lead" {
		t.Fatalf("signed commitment: %+v err=%v", signed, err)
	}
	confirmed, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-DUR", "creator")
	if err != nil || confirmed.Status != CommitmentConfirmed {
		t.Fatalf("confirm duration: %+v err=%v", confirmed, err)
	}
	// 修改已确认承诺 -> 退回 draft，并要求重新确认。
	in.CommittedRaw = "100 天"
	hundred := 100.0
	in.CommittedValue = &hundred
	changed, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, in, "creator")
	if err != nil || changed.Status != CommitmentDraft {
		t.Fatalf("changed confirmed commitment must return to draft: %+v err=%v", changed, err)
	}
}

func TestD2QuoteFixedPointAndCommitment(t *testing.T) {
	f, tenant, _, _ := d2Setup(t)
	ctx := context.Background()
	req := d2Req(t, f, tenant, "REQ-PRICE", "投标报价", ReqScoringItem, &ProofRules{}, EvalObjective, 20)
	price := 300000.0
	pc := CommitmentInput{Code: "C-PRICE", RequirementID: &req.ID, Metric: MetricPrice, MetricLabel: "投标总价", Unit: "元",
		ThresholdCmp: "<=", TenderThresholdRaw: "最高限价 500000 元", TenderThresholdValue: floatp(500000), CommittedRaw: "300000.00 元",
		CommittedValue: &price, Owner: "商务负责人", Rationale: "成本测算"}
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, pc, "creator"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-PRICE", "creator"); err != nil {
		t.Fatalf("price commitment: %v", err)
	}
	in := BidQuoteInput{Lot: "", DeclaredTotal: "300000.00", Ceiling: strpTest("500000.00"), Items: []BidQuoteItemInput{
		{LineNo: 1, Code: "L1", Name: "摄像机", UnitPrice: "1000.00", Quantity: "300", LineAmount: "300000.00"},
	}}
	q, err := f.d.CreateBidQuote(ctx, tenant, f.task.ID, in, "commercial")
	if err != nil {
		t.Fatalf("create quote: %v", err)
	}
	val, err := f.d.ValidateBidQuote(ctx, tenant, f.task.ID, q.ID)
	if err != nil || !val.OK {
		t.Fatalf("valid quote: %+v err=%v", val, err)
	}
	confirmed, val2, err := f.d.ConfirmBidQuote(ctx, tenant, f.task.ID, q.ID, "commercial")
	if err != nil || confirmed.Status != "confirmed" || !val2.OK {
		t.Fatalf("confirm quote: %+v err=%v", confirmed, err)
	}
	bad := BidQuoteInput{Lot: "", DeclaredTotal: "300000.00", Ceiling: strpTest("500000.00"), Items: []BidQuoteItemInput{
		{LineNo: 1, Code: "L1", Name: "摄像机", UnitPrice: "1000.00", Quantity: "300", LineAmount: "300001.00"},
	}}
	q2, err := f.d.CreateBidQuote(ctx, tenant, f.task.ID, bad, "commercial")
	if err != nil {
		t.Fatal(err)
	}
	val3, err := f.d.ValidateBidQuote(ctx, tenant, f.task.ID, q2.ID)
	if err != nil || val3.OK {
		t.Fatalf("inconsistent quote must fail: %+v err=%v", val3, err)
	}
	if _, _, err := f.d.ConfirmBidQuote(ctx, tenant, f.task.ID, q2.ID, "commercial"); !errors.Is(err, ErrQuoteInconsistent) {
		t.Fatalf("confirm inconsistent quote: %v", err)
	}
	over := BidQuoteInput{Lot: "", DeclaredTotal: "600000.00", Ceiling: strpTest("500000.00"), Items: []BidQuoteItemInput{
		{LineNo: 1, Code: "L1", Name: "摄像机", UnitPrice: "2000.00", Quantity: "300", LineAmount: "600000.00"},
	}}
	q3, err := f.d.CreateBidQuote(ctx, tenant, f.task.ID, over, "commercial")
	if err != nil {
		t.Fatal(err)
	}
	val4, err := f.d.ValidateBidQuote(ctx, tenant, f.task.ID, q3.ID)
	if err != nil || val4.OK {
		t.Fatalf("over-ceiling quote must fail: %+v err=%v", val4, err)
	}
	hasCeiling := false
	for _, c := range val4.Checks {
		if c.Rule == "ceiling" && c.Status == "fail" {
			hasCeiling = true
		}
	}
	if !hasCeiling {
		t.Fatalf("ceiling check missing: %+v", val4.Checks)
	}
	if !strings.Contains(formatAmount(10005000), "1000.5000") {
		t.Fatalf("fixed point format: %s", formatAmount(10005000))
	}
}

func floatp(v float64) *float64 { return &v }
