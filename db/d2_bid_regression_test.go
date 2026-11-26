package db

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/evidence"
	"github.com/autobid/autobid-engine/ingest/ingesttest"
)

func TestD2SyntheticEvidenceScoreAndBidByLot(t *testing.T) {
	const actor = "d2-synthetic-reviewer"
	const bidder = "合成演示科技有限公司（仅供工程回归）"
	f := newFixture(t)
	d, taskID := f.d, f.task.ID
	tenant := uniq("d2-synthetic-bid")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store := evidence.New(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := d.ExecContext(ctx, "DELETE FROM tasks WHERE id=$1", taskID); err != nil {
			t.Error(err)
		}
		for _, table := range []string{"company_evidence_originals", "company_performance", "company_credentials"} {
			if _, err := d.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id=$1", tenant); err != nil {
				t.Error(err)
			}
		}
	})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	setScope := func(input BidScopeInput) *RequirementManifest {
		t.Helper()
		m, err := d.SetBidScope(ctx, tenant, taskID, actor, input)
		must(err)
		return m
	}
	deadline := "2026-12-05T09:00:00+08:00"
	setScope(BidScopeInput{Deadline: &deadline})
	_, _, err := d.UpsertCompanyCredential(ctx, &CompanyCredential{TenantID: tenant, Category: "company_profile", Code: "SYN-PROFILE",
		Title: "合成投标人资料", SubjectName: bidder, SubjectType: "bidder",
		Attributes: json.RawMessage(`{"synthetic":true,"legal_name":"合成演示科技有限公司（仅供工程回归）"}`)})
	must(err)
	createScore := func(code, lot, title, criteria string, points float64, rules ProofRules) *RequirementItem {
		t.Helper()
		req, err := d.CreateRequirement(ctx, tenant, taskID, actor, RequirementInput{
			Code: &code, Lot: &lot, Title: &title, Description: &criteria, Category: strpTest(ReqScoringItem),
			ScoreWeight: &points, ScoreGroup: strpTest(ScoreGroupCommercial), EvalKind: strpTest(EvalObjective), ProofRules: &rules,
		})
		must(err)
		return req
	}
	createVeto := func(code, lot, title string) *RequirementItem {
		t.Helper()
		req, err := d.CreateRequirement(ctx, tenant, taskID, actor, RequirementInput{
			Code: &code, Lot: &lot, Title: &title, Category: strpTest(ReqVetoItem), VetoKind: strpTest(VetoQualification),
		})
		must(err)
		return req
	}
	certA := createScore("SYN-A-CERT", "A", "合成 A 包资质", "每提供 1 份有效资质证书得 4 分，最高 4 分。", 4,
		ProofRules{BidderOnly: true, ValidOnBidDeadline: true, RequiredDocs: []string{"资质证书"}, MinCount: 1})
	perfRules := ProofRules{BidderOnly: true, NeedAcceptance: true, RequiredDocs: []string{"合同", "验收报告"},
		ContractPages: []string{"标的页"}, YearsWindow: 3, DateAnchor: "acceptance_date", MinCount: 2}
	perfA := createScore("SYN-A-PERF", "A", "合成 A 包安防运维业绩", "每提供 1 个合格业绩得 3 分，最高 6 分。", 6, perfRules)
	perfRules.MinCount = 1
	perfB := createScore("SYN-B-PERF", "B", "合成 B 包物业巡检业绩", "每提供 1 个合格业绩得 5 分，最高 5 分。", 5, perfRules)
	for _, config := range []struct {
		lot  string
		tech float64
	}{{"A", 80}, {"B", 85}} {
		_, err := d.CreateRequirement(ctx, tenant, taskID, actor, RequirementInput{
			Code: strpTest("SYN-" + config.lot + "-TECH"), Lot: &config.lot, Title: strpTest("合成技术方案"), Category: strpTest(ReqScoringItem),
			ScoreWeight: &config.tech, ScoreGroup: strpTest(ScoreGroupTechnical), EvalKind: strpTest(EvalSubjective),
		})
		must(err)
		_, err = d.CreateRequirement(ctx, tenant, taskID, actor, RequirementInput{
			Code: strpTest("SYN-" + config.lot + "-PRICE"), Lot: &config.lot, Title: strpTest("合成报价评分"), Category: strpTest(ReqScoringItem),
			ScoreWeight: floatp(10), ScoreGroup: strpTest(ScoreGroupPrice), EvalKind: strpTest(EvalObjective),
		})
		must(err)
	}
	vetoA := createVeto("SYN-A-VETO", "A", "合成 A 包资格审查")
	vetoB := createVeto("SYN-B-VETO", "B", "合成 B 包资格审查")

	addOriginal := func(domain string, id int64, code, kind, pageLabel string) {
		t.Helper()
		title := "合成测试材料 " + code + " " + pageLabel
		doc := ingesttest.BuildDOCX(title, ingesttest.Para{Text: title, Heading: 1},
			ingesttest.Para{Text: "仅用于 AutoBid D2 工程回归。投标人、项目与材料均为虚构。"})
		path, hash, err := store.Put(tenant, "docx", doc)
		must(err)
		_, err = d.AddCompanyEvidenceOriginal(ctx, &CompanyEvidenceOriginal{
			TenantID: tenant, OwnerDomain: domain, OwnerID: id, DocumentKind: kind, PageLabel: pageLabel,
			OriginalPath: path, OriginalSHA256: hash, PageStart: 1, PageEnd: 1, CreatedBy: actor,
		}, false)
		must(err)
	}
	credential, _, err := d.UpsertCompanyCredential(ctx, &CompanyCredential{TenantID: tenant, Category: CredentialQualification,
		Code: "SYN-CERT-A", CertificateNo: "SYNTHETIC-CERT-A-01", Title: "合成 A 包运维资质（仅供测试）",
		SubjectName: bidder, SubjectType: "bidder", Scope: "安防运维",
		Attributes: json.RawMessage(`{"synthetic":true}`)})
	must(err)
	addOriginal(EvidenceDomainCredential, credential.ID, credential.Code, DocKindCertificate, "合成资质证书首页")
	addPerformance := func(code, scope string, withAcceptance bool) *CompanyPerformance {
		t.Helper()
		p, _, err := d.UpsertCompanyPerformance(ctx, &CompanyPerformance{TenantID: tenant, Code: code, ContractNo: "SYNTHETIC-" + code,
			ProjectName: "合成项目 " + code, Buyer: "合成采购单位（仅供测试）", SubjectName: bidder, SubjectType: "bidder",
			Scope: scope, ContractDate: strpTest("2025-03-01"), AcceptanceDate: strpTest("2025-07-01")})
		must(err)
		addOriginal(EvidenceDomainPerformance, p.ID, code, DocKindContract, "合成合同标的页")
		if withAcceptance {
			addOriginal(EvidenceDomainPerformance, p.ID, code, DocKindAcceptanceReport, "合成验收报告")
		}
		return p
	}
	validA := addPerformance("SYN-PERF-A1", "安防运维", true)
	missingA := addPerformance("SYN-PERF-A2", "安防运维", false)
	validB := addPerformance("SYN-PERF-B1", "物业巡检", true)
	bind := func(req *RequirementItem, domain string, id int64) {
		t.Helper()
		_, err := d.BindRequirementEvidence(ctx, tenant, taskID, req.ID, domain, id, actor)
		must(err)
	}
	evaluate := func(req *RequirementItem, verdict string) *AdmissibilityRun {
		t.Helper()
		run, err := d.EvaluateAdmissibility(ctx, tenant, taskID, req.ID, store, actor)
		must(err)
		if run.Verdict != verdict {
			t.Fatalf("%s admissibility = %s, want %s; reasons=%+v", req.Code, run.Verdict, verdict, run.Reasons)
		}
		return run
	}
	checkItem := func(run *AdmissibilityRun, id int64, verdict, rule, state string) {
		t.Helper()
		for _, item := range run.Items {
			if item.EvidenceID == id && item.Verdict == verdict &&
				slices.ContainsFunc(item.Checks, func(c compliance.AdmissibilityCheck) bool { return c.Rule == rule && c.Status == state }) {
				return
			}
		}
		t.Fatalf("%s evidence %d did not produce %s / %s %s: %+v", run.RequirementCode, id, verdict, rule, state, run.Items)
	}
	type scoreWant struct {
		status   string
		score    float64
		evidence []int64
	}
	checkScores := func(wants map[string]scoreWant) {
		t.Helper()
		scores, err := d.ComputeObjectiveScores(ctx, tenant, taskID, 0, actor)
		must(err)
		if len(scores) != len(wants) {
			t.Fatalf("objective score rows=%d, want %d for current lot selection", len(scores), len(wants))
		}
		for _, score := range scores {
			want, ok := wants[score.RequirementCode]
			if !ok || score.Status != want.status || score.Score != want.score || !slices.Equal(score.EvidenceIDs, want.evidence) {
				t.Fatalf("objective score: got %+v, want %+v", score, want)
			}
		}
	}
	checkBid := func(phase, lot string, nominal, confirmed, possible float64, gate, recommendation string) *RequirementsView {
		t.Helper()
		v, err := d.ListRequirements(ctx, tenant, taskID)
		must(err)
		ev := v.BidEvaluation
		if !slices.Equal(v.Manifest.BidLots, []string{lot}) || ev.TotalScore != 100 || ev.PriceScore != 10 || ev.ObjectiveScore != nominal ||
			ev.ObjectiveScoreLower != confirmed || ev.ObjectiveScoreUpper != possible || ev.EvidenceBackedScore != confirmed ||
			ev.VetoGate != gate || ev.Recommendation != recommendation {
			t.Fatalf("%s: selected=%v, evidence=%+v; want lot %s, nominal %g, objective=[%g,%g], gate=%s, recommendation=%s",
				phase, v.Manifest.BidLots, ev, lot, nominal, confirmed, possible, gate, recommendation)
		}
		t.Logf("%s lot=%s nominal=%g objective=[%g,%g] qualification=%s recommendation=%s decision=%s",
			phase, lot, nominal, ev.ObjectiveScoreLower, ev.ObjectiveScoreUpper, gate, recommendation, ev.Decision)
		return v
	}

	bind(perfB, EvidenceDomainPerformance, validB.ID)
	evaluate(perfB, compliance.Admissible)
	_, err = d.ComputeObjectiveScores(ctx, tenant, taskID, perfB.ID, actor)
	must(err)
	setScope(BidScopeInput{Lots: &[]string{"A"}})
	_, err = d.SetVetoStatus(ctx, tenant, taskID, vetoA.ID, actor, VetoPassed, "合成 A 包资格材料审核通过")
	must(err)
	_, err = d.SetVetoStatus(ctx, tenant, taskID, vetoB.ID, actor, VetoFailed, "合成 B 包独立资格条件不满足")
	must(err)
	bind(perfA, EvidenceDomainPerformance, validA.ID)
	bind(perfA, EvidenceDomainPerformance, missingA.ID)
	absent := evaluate(certA, compliance.Manual)
	if len(absent.Items) != 0 || !slices.ContainsFunc(absent.Reasons, func(r compliance.AdmissibilityReason) bool { return r.Code == "no_evidence_bound" }) {
		t.Fatalf("unbound certificate was not identified: %+v", absent)
	}
	checkItem(evaluate(perfA, compliance.Inadmissible), missingA.ID, compliance.Inadmissible, "need_acceptance", compliance.CheckFail)
	checkScores(map[string]scoreWant{certA.Code: {"manual", 0, nil}, perfA.Code: {"computed", 3, []int64{validA.ID}}})
	checkBid("缺绑定与验收原件", "A", 10, 3, 3, "passed", "caution")

	bind(certA, EvidenceDomainCredential, credential.ID)
	checkItem(evaluate(certA, compliance.Manual), credential.ID, compliance.Manual, "valid_on_bid_deadline", compliance.CheckManual)
	checkScores(map[string]scoreWant{certA.Code: {"manual", 0, nil}, perfA.Code: {"computed", 3, []int64{validA.ID}}})
	checkBid("有效期待人工核查", "A", 10, 3, 7, "passed", "caution")

	credential.ValidUntil = strpTest("2026-12-05")
	_, err = d.UpdateCompanyCredential(ctx, credential)
	must(err)
	addOriginal(EvidenceDomainPerformance, missingA.ID, missingA.Code, DocKindAcceptanceReport, "补充合成验收报告")
	checkItem(evaluate(certA, compliance.Admissible), credential.ID, compliance.Admissible, "valid_on_bid_deadline", compliance.CheckPass)
	evaluate(perfA, compliance.Admissible)
	checkScores(map[string]scoreWant{
		certA.Code: {"computed", 4, []int64{credential.ID}},
		perfA.Code: {"computed", 6, []int64{validA.ID, missingA.ID}},
	})
	checkBid("补齐证据，同日到期仍有效", "A", 10, 10, 10, "passed", "bid")
	bid := BidDecisionBid
	aDecision := setScope(BidScopeInput{Decision: &bid, DecisionNote: strpTest("合成 A 包资格通过，证据已复核")})
	if aDecision.BidDecision != bid || aDecision.BidDecidedBy != actor || aDecision.BidDecidedAt == nil {
		t.Fatalf("A lot bid decision did not persist: %+v", aDecision)
	}

	setScope(BidScopeInput{Deadline: strpTest("2026-12-06T09:00:00+08:00")})
	checkBid("截止顺延，旧运行失效", "A", 10, 0, 0, "passed", "caution")
	checkItem(evaluate(certA, compliance.Inadmissible), credential.ID, compliance.Inadmissible, "valid_on_bid_deadline", compliance.CheckFail)
	evaluate(perfA, compliance.Admissible)
	checkScores(map[string]scoreWant{
		certA.Code: {"computed", 0, nil},
		perfA.Code: {"computed", 6, []int64{validA.ID, missingA.ID}},
	})
	checkBid("重算后排除过期资质", "A", 10, 6, 6, "passed", "caution")

	_, err = d.SetBidScope(ctx, tenant, taskID, actor, BidScopeInput{Lots: &[]string{"B"}, Decision: &bid})
	if !errors.Is(err, ErrInvalidRequirement) {
		t.Fatalf("failed B lot veto must block bid: %v", err)
	}
	if v := checkBid("拒绝改投 B，原选择保留", "A", 10, 6, 6, "passed", "caution"); v.Manifest.BidDecision != bid {
		t.Fatalf("failed lot switch changed A lot decision: %+v", v.Manifest)
	}
	preflight, err := d.ExportReadiness(ctx, tenant, taskID)
	must(err)
	if preflight.Counts[BlockNoBid] != 0 {
		t.Fatalf("A bid unexpectedly has no_bid export blocker: %+v", preflight.Counts)
	}
	noBid := BidDecisionNoBid
	note := "合成 B 包资格不满足，决定不投"
	setScope(BidScopeInput{Lots: &[]string{"B"}, Decision: &noBid, DecisionNote: &note})
	evaluate(perfB, compliance.Admissible)
	checkScores(map[string]scoreWant{perfB.Code: {"computed", 5, []int64{validB.ID}}})
	v := checkBid("只核算 B 证据并记录不投", "B", 5, 5, 5, "blocked", "no_bid")
	if v.Manifest.BidDecision != noBid || v.Manifest.BidDecisionNote != note || v.Manifest.BidDecidedBy != actor || v.Manifest.BidDecidedAt == nil {
		t.Fatalf("B lot no-bid decision did not persist: %+v", v.Manifest)
	}
	readiness, err := d.ExportReadiness(ctx, tenant, taskID)
	must(err)
	if readiness.Counts[BlockNoBid] != 1 {
		t.Fatalf("B no_bid must block export: %+v", readiness.Counts)
	}
}
