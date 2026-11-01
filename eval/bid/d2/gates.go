package main

// D2-G01 ~ D2-G14 门禁正反例。每个门禁使用独立租户与任务；写入拒绝与持久化 readiness blocker 分开记录。

import (
	"context"
	"strconv"
	"time"
	"errors"
	"fmt"
	"strings"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

func (a *app) runGates(ctx context.Context, f *flow) {
	seen := map[string]bool{}
	for _, fn := range []func(context.Context, *flow) *Gate{
		gateG01, gateG02, gateG03, gateG04, gateG05, gateG06, gateG07,
		gateG08, gateG09, gateG10, gateG11, gateG12, gateG13, gateG14,
	} {
		gctx, cancel := context.WithTimeout(ctx, 120*time.Second)
		g := fn(gctx, f)
		cancel()
		seen[g.ID] = true
		f.r.gate(g)
	}
	if a.defects != nil {
		var missing []string
		for _, d := range a.defects.Defects {
			if !seen[d.Gate] {
				missing = append(missing, d.Gate)
			}
		}
		f.r.check("GATES", "GATES.defects_covered", strconv.Itoa(len(a.defects.Defects)),
			strconv.Itoa(len(a.defects.Defects)-len(missing)), len(missing) == 0, strings.Join(missing, ","))
		f.r.check("GATES", "GATES.fixture_version", strconv.Itoa(a.golden.FixtureVersion), strconv.Itoa(a.defects.FixtureVersion),
			a.defects.FixtureVersion == a.golden.FixtureVersion, "")
	}
}

func finishGate(g *Gate, err error) *Gate {
	if err != nil {
		g.Error = err.Error()
		g.Remediated = false
	}
	return g
}

func addGateCheck(g *Gate, id, expected, actual string, ok bool, detail string) {
	c := Check{ID: id, Expected: expected, Actual: actual, Detail: detail, Status: statusPass}
	if !ok {
		c.Status = statusFail
		g.Remediated = false
	}
	g.Checks = append(g.Checks, c)
}

func itemVerdict(run *db.AdmissibilityRun, domain string, id int64) string {
	if it := findAdmissibilityItem(run, domain, id); it != nil {
		return it.Verdict
	}
	return "missing"
}

func (f *flow) g01(ctx context.Context) *Gate {
	g := &Gate{ID: "D2-G01", Gate: "bid_deadline_extension_invalidates_certificate", SignalType: "write_rejected",
		Sentinel: "verdict=inadmissible", Trigger: "证书在原截止日有效、更正顺延后过期；重评"}
	if err := f.g01run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g01run(ctx context.Context, g *Gate) error {
	if _, _, err := f.newTask(ctx, "g01", "D2-G01 截止日顺延导致证书失效"); err != nil {
		return err
	}
	if _, err := f.ingestTender(ctx); err != nil {
		return err
	}
	if _, err := f.app.d.ExtractRequirements(ctx, f.tenant, f.task.ID, actor); err != nil {
		return err
	}
	if err := f.patchRequirements(ctx); err != nil {
		return err
	}
	if _, err := f.confirmAndOutline(ctx); err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainCredential, es.QualCritical.ID); err != nil {
		return err
	}
	run1, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	item1 := findAdmissibilityItem(run1, db.EvidenceDomainCredential, es.QualCritical.ID)
	addGateCheck(g, "G01.original_deadline", "admissible", runOrItem(run1, item1), run1.Verdict == compliance.Admissible, "")

	if _, err := f.ingestCorrection(ctx); err != nil {
		return err
	}
	if _, err := f.app.d.ExtractRequirements(ctx, f.tenant, f.task.ID, actor); err != nil {
		return err
	}
	if err := f.patchRequirements(ctx); err != nil {
		return err
	}
	req2, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	old, err := f.app.d.ListAdmissibilityRuns(ctx, f.tenant, f.task.ID, req.ID, 20)
	if err != nil {
		return err
	}
	staleSeen := false
	for _, r := range old {
		if r.ID == run1.ID && r.Status != "current" {
			staleSeen = true
		}
	}
	addGateCheck(g, "G01.old_run_stale", "not current", boolText(staleSeen), staleSeen, "导入更正后旧运行失效")
	run2, err := f.evaluate(ctx, req2.ID)
	if err != nil {
		return err
	}
	item2 := findAdmissibilityItem(run2, db.EvidenceDomainCredential, es.QualCritical.ID)
	addGateCheck(g, "G01.final_deadline", "inadmissible", runOrItem(run2, item2), run2.Verdict == compliance.Inadmissible, itemReasons(item2))
	if err := f.app.d.UnbindRequirementEvidence(ctx, f.tenant, f.task.ID, req2.ID, db.EvidenceDomainCredential, es.QualCritical.ID); err != nil {
		return err
	}
	if err := f.bind(ctx, req2.ID, db.EvidenceDomainCredential, es.QualValid.ID); err != nil {
		return err
	}
	run3, err := f.evaluate(ctx, req2.ID)
	if err != nil {
		return err
	}
	addGateCheck(g, "G01.remediated", "admissible", run3.Verdict, run3.Verdict == compliance.Admissible, "换为 2027-06-30 到期的有效证书")
	g.Observed = "原截止日 admissible -> 顺延后 stale/inadmissible -> 更换有效证书后 admissible"
	g.Remediated = run3.Verdict == compliance.Admissible
	return nil
}

func runOrItem(run *db.AdmissibilityRun, it *db.AdmissibilityItem) string {
	if it != nil {
		return it.Verdict
	}
	return run.Verdict
}

func gateG01(ctx context.Context, f *flow) *Gate { return f.g01(ctx) }

func (f *flow) g02run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g02", false)
	if err != nil {
		return err
	}
	_ = det
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "业绩要求")
	if err != nil {
		return err
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainPerformance, es.PerfSub.ID); err != nil {
		return err
	}
	run1, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	it1 := findAdmissibilityItem(run1, db.EvidenceDomainPerformance, es.PerfSub.ID)
	addGateCheck(g, "G02.bidder_only", "inadmissible", runOrItem(run1, it1), run1.Verdict == compliance.Inadmissible && strings.Contains(itemReasons(it1), "bidder_only"), itemReasons(it1))
	if err := f.app.d.UnbindRequirementEvidence(ctx, f.tenant, f.task.ID, req.ID, db.EvidenceDomainPerformance, es.PerfSub.ID); err != nil {
		return err
	}
	for _, id := range []int64{es.Perf1.ID, es.Perf2.ID} {
		if err := f.bind(ctx, req.ID, db.EvidenceDomainPerformance, id); err != nil {
			return err
		}
	}
	run2, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	addGateCheck(g, "G02.remediated", "admissible", run2.Verdict, run2.Verdict == compliance.Admissible, "")
	g.Observed = "子公司业绩 bidder_only 失败 -> 换两条投标人自身业绩后 admissible"
	g.Remediated = run2.Verdict == compliance.Admissible
	return nil
}

func gateG02(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G02", Gate: "subsidiary_performance", SignalType: "write_rejected", Sentinel: "verdict=inadmissible",
		Trigger: "子公司业绩绑定到投标人自身业绩要求"}
	if err := f.g02run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g03run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g03", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "业绩要求")
	if err != nil {
		return err
	}
	contract, acceptance := "2025-01-10", "2025-06-20"
	p, _, err := f.app.d.UpsertCompanyPerformance(ctx, &db.CompanyPerformance{TenantID: f.tenant, Code: "PERF-G03", ProjectName: "G03 缺页码业绩（虚构）",
		ContractDate: &contract, AcceptanceDate: &acceptance, SubjectName: bidderName, SubjectType: "bidder", Scope: "安防系统集成", CreatedBy: actor})
	if err != nil {
		return err
	}
	if err := f.addOriginal(ctx, db.EvidenceDomainPerformance, p.ID, "perf_noacc_contract"); err != nil {
		return err
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainPerformance, p.ID); err != nil {
		return err
	}
	run1, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	it1 := findAdmissibilityItem(run1, db.EvidenceDomainPerformance, p.ID)
	reasons := itemReasons(it1)
	ok1 := it1 != nil && it1.Verdict == compliance.Inadmissible &&
		(strings.Contains(reasons, "contract_pages") || strings.Contains(reasons, "required_docs") || strings.Contains(reasons, "need_acceptance"))
	addGateCheck(g, "G03.missing_pages", "inadmissible", runOrItem(run1, it1), ok1, reasons)
	sec := sectionForRequirement(det, req.ID)
	if sec == nil {
		return errors.New("G03 section not found")
	}
	_, saveErr := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{TaskID: f.task.ID, SectionID: sec.ID,
		BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit, ContentMD: "响应要求：提供业绩证明材料。",
		AdmissibilityItemIDs: []int64{it1.ID}})
	addGateCheck(g, "G03.write_rejected", "ErrEvidenceInadmissible", errCode(saveErr), errors.Is(saveErr, db.ErrEvidenceInadmissible), "")
	for _, key := range []string{"perf1_amount", "perf1_seal", "perf1_accept"} {
		if err := f.addOriginal(ctx, db.EvidenceDomainPerformance, p.ID, key); err != nil {
			return err
		}
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainPerformance, es.Perf1.ID); err != nil {
		return err
	}
	run2, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	it2 := findAdmissibilityItem(run2, db.EvidenceDomainPerformance, p.ID)
	addGateCheck(g, "G03.remediated", "admissible", run2.Verdict, run2.Verdict == compliance.Admissible && it2 != nil && it2.Verdict == compliance.Admissible, "")
	g.Observed = "缺合同页码/验收原件 -> 逐证据 inadmissible、引用被拒 -> 补齐原件后 admissible"
	g.Remediated = run2.Verdict == compliance.Admissible
	return nil
}

func gateG03(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G03", Gate: "missing_originals_or_pages", SignalType: "write_rejected", Sentinel: "ErrEvidenceInadmissible",
		Trigger: "业绩只有合同记录，缺少验收报告与合同页码"}
	if err := f.g03run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g04run(ctx context.Context, g *Gate) error {
	_, err := f.prepareTask(ctx, "g04", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "业绩要求")
	if err != nil {
		return err
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainPerformance, es.PerfNoAcc.ID); err != nil {
		return err
	}
	run1, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	it1 := findAdmissibilityItem(run1, db.EvidenceDomainPerformance, es.PerfNoAcc.ID)
	addGateCheck(g, "G04.need_acceptance", "inadmissible", runOrItem(run1, it1), it1 != nil && it1.Verdict == compliance.Inadmissible &&
		strings.Contains(itemReasons(it1), "need_acceptance"), itemReasons(it1))
	acceptance := "2025-09-01"
	updated := *es.PerfNoAcc
	updated.AcceptanceDate = &acceptance
	if _, _, err := f.app.d.UpsertCompanyPerformance(ctx, &updated); err != nil {
		return err
	}
	for _, key := range []string{"perf1_amount", "perf1_seal", "perf1_accept"} {
		if err := f.addOriginal(ctx, db.EvidenceDomainPerformance, es.PerfNoAcc.ID, key); err != nil {
			return err
		}
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainPerformance, es.Perf1.ID); err != nil {
		return err
	}
	run2, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	it2 := findAdmissibilityItem(run2, db.EvidenceDomainPerformance, es.PerfNoAcc.ID)
	addGateCheck(g, "G04.remediated", "admissible", run2.Verdict, run2.Verdict == compliance.Admissible && it2 != nil && it2.Verdict == compliance.Admissible, "")
	g.Observed = "缺验收日期/验收页 -> inadmissible -> 补验收记录与验收页后 admissible"
	g.Remediated = run2.Verdict == compliance.Admissible
	return nil
}

func gateG04(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G04", Gate: "missing_acceptance", SignalType: "write_rejected", Sentinel: "verdict=inadmissible",
		Trigger: "业绩有合同、无验收日期或验收原件"}
	if err := f.g04run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g05run(ctx context.Context, g *Gate) error {
	_, err := f.prepareTask(ctx, "g05", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "业绩要求")
	if err != nil {
		return err
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainPerformance, es.PerfOld.ID); err != nil {
		return err
	}
	run1, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	it1 := findAdmissibilityItem(run1, db.EvidenceDomainPerformance, es.PerfOld.ID)
	addGateCheck(g, "G05.date_window", "inadmissible", runOrItem(run1, it1), it1 != nil && it1.Verdict == compliance.Inadmissible &&
		strings.Contains(itemReasons(it1), "date_window"), itemReasons(it1))
	if err := f.app.d.UnbindRequirementEvidence(ctx, f.tenant, f.task.ID, req.ID, db.EvidenceDomainPerformance, es.PerfOld.ID); err != nil {
		return err
	}
	for _, id := range []int64{es.Perf1.ID, es.Perf2.ID} {
		if err := f.bind(ctx, req.ID, db.EvidenceDomainPerformance, id); err != nil {
			return err
		}
	}
	run2, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	addGateCheck(g, "G05.remediated", "admissible", run2.Verdict, run2.Verdict == compliance.Admissible, "")
	g.Observed = "近三年窗口外业绩 date_window 失败 -> 换窗口内业绩后 admissible"
	g.Remediated = run2.Verdict == compliance.Admissible
	return nil
}

func gateG05(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G05", Gate: "date_window", SignalType: "write_rejected", Sentinel: "verdict=inadmissible",
		Trigger: "业绩在近三年窗口之外"}
	if err := f.g05run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g06run(ctx context.Context, g *Gate) error {
	_, err := f.prepareTask(ctx, "g06", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "摄像机型号")
	if err != nil {
		return err
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainCredential, es.ProductBad.ID); err != nil {
		return err
	}
	run1, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	it1 := findAdmissibilityItem(run1, db.EvidenceDomainCredential, es.ProductBad.ID)
	addGateCheck(g, "G06.model_param", "inadmissible", runOrItem(run1, it1), it1 != nil && it1.Verdict == compliance.Inadmissible &&
		strings.Contains(itemReasons(it1), "model_param"), itemReasons(it1))
	if err := f.app.d.UnbindRequirementEvidence(ctx, f.tenant, f.task.ID, req.ID, db.EvidenceDomainCredential, es.ProductBad.ID); err != nil {
		return err
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainCredential, es.ProductOK.ID); err != nil {
		return err
	}
	run2, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	addGateCheck(g, "G06.remediated", "admissible", run2.Verdict, run2.Verdict == compliance.Admissible, "")
	g.Observed = "错误型号/参数 model_param 失败 -> 换正确型号后 admissible"
	g.Remediated = run2.Verdict == compliance.Admissible
	return nil
}

func gateG06(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G06", Gate: "model_param", SignalType: "write_rejected", Sentinel: "verdict=inadmissible",
		Trigger: "产品证据型号/参数与要求不符"}
	if err := f.g06run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func durationInput(f *flow, ctx context.Context, committed float64, raw string) (db.CommitmentInput, error) {
	req, err := f.requirementBy(ctx, "工期要求")
	if err != nil {
		return db.CommitmentInput{}, err
	}
	threshold, cap := 90.0, 90.0
	return db.CommitmentInput{Code: "C-DURATION-G07", RequirementID: &req.ID, Metric: db.MetricDuration, MetricLabel: "总工期",
		Unit: "天", ThresholdCmp: "<=", TenderThresholdRaw: "不超过 90 日历天", TenderThresholdValue: &threshold,
		CommittedRaw: raw, CommittedValue: &committed, CapabilityLimit: &cap, CapabilityBasis: "实施计划",
		Owner: "项目经理", Rationale: "实施进度计划"}, nil
}

func (f *flow) g07run(ctx context.Context, g *Gate) error {
	if _, err := f.prepareTask(ctx, "g07", false); err != nil {
		return err
	}
	in, err := durationInput(f, ctx, 120, "120 天")
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertCommitment(ctx, f.tenant, f.task.ID, in, actor); err != nil {
		return err
	}
	_, confirmErr := f.app.d.ConfirmCommitment(ctx, f.tenant, f.task.ID, in.Code, actor)
	addGateCheck(g, "G07.below_threshold", "ErrCommitmentBelowThreshold", errCode(confirmErr), errors.Is(confirmErr, db.ErrCommitmentBelowThreshold), "")
	in, err = durationInput(f, ctx, 90, "90 天")
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertCommitment(ctx, f.tenant, f.task.ID, in, actor); err != nil {
		return err
	}
	c, err := f.app.d.ConfirmCommitment(ctx, f.tenant, f.task.ID, in.Code, actor)
	if err != nil {
		return err
	}
	addGateCheck(g, "G07.remediated", "confirmed", c.Status, c.Status == db.CommitmentConfirmed, "")
	g.Observed = "低于 ★ 阈值确认被拒 -> 改为 90 天后 confirmed"
	g.Remediated = c.Status == db.CommitmentConfirmed
	return nil
}

func gateG07(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G07", Gate: "commitment_below_threshold", SignalType: "write_rejected", Sentinel: "ErrCommitmentBelowThreshold",
		Trigger: "保存低于 ★ 阈值的承诺后尝试确认"}
	if err := f.g07run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func teamInput(f *flow, ctx context.Context, committed float64) (db.CommitmentInput, error) {
	req, err := f.requirementBy(ctx, "团队人数要求")
	if err != nil {
		return db.CommitmentInput{}, err
	}
	threshold, cap := 12.0, 12.0
	return db.CommitmentInput{Code: "C-TEAM-G08", RequirementID: &req.ID, Metric: db.MetricTeamSize, MetricLabel: "团队人数",
		Unit: "人", ThresholdCmp: ">=", TenderThresholdRaw: "不少于 12 人", TenderThresholdValue: &threshold,
		CommittedRaw: fmt.Sprintf("%.0f 人", committed), CommittedValue: &committed, CapabilityLimit: &cap,
		CapabilityBasis: "在岗人员 12 人", Owner: "交付负责人", Rationale: "人员配置计划"}, nil
}

func (f *flow) g08run(ctx context.Context, g *Gate) error {
	if _, err := f.prepareTask(ctx, "g08", false); err != nil {
		return err
	}
	in, err := teamInput(f, ctx, 20)
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertCommitment(ctx, f.tenant, f.task.ID, in, actor); err != nil {
		return err
	}
	_, confirmErr := f.app.d.ConfirmCommitment(ctx, f.tenant, f.task.ID, in.Code, actor)
	addGateCheck(g, "G08.over_capability", "ErrCommitmentSignoffRequired", errCode(confirmErr), errors.Is(confirmErr, db.ErrCommitmentSignoffRequired), "")
	_, selfErr := f.app.d.SignOffCommitment(ctx, f.tenant, f.task.ID, in.Code, actor, "自签")
	addGateCheck(g, "G08.self_signoff", "ErrCommitmentSignoffRequired", errCode(selfErr), errors.Is(selfErr, db.ErrCommitmentSignoffRequired), "")
	if _, err := f.app.d.SignOffCommitment(ctx, f.tenant, f.task.ID, in.Code, signer, "已核对在岗人员与外包补充计划"); err != nil {
		return err
	}
	c, err := f.app.d.ConfirmCommitment(ctx, f.tenant, f.task.ID, in.Code, actor)
	if err != nil {
		return err
	}
	addGateCheck(g, "G08.remediated", "signed_off", c.Status+" signer="+c.SignedOffBy,
		c.Status == db.CommitmentSignedOff && c.SignedOffBy == signer, "")
	g.Observed = "超能力未会签被拒 -> 由不同操作人会签后 signed_off"
	g.Remediated = c.Status == db.CommitmentSignedOff
	return nil
}

func gateG08(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G08", Gate: "commitment_over_capability", SignalType: "write_rejected", Sentinel: "ErrCommitmentSignoffRequired",
		Trigger: "承诺超过 capability_limit 且未会签"}
	if err := f.g08run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g09run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g09", false)
	if err != nil {
		return err
	}
	in, err := durationInput(f, ctx, 90, "90 天")
	if err != nil {
		return err
	}
	draft, err := f.app.d.UpsertCommitment(ctx, f.tenant, f.task.ID, in, actor)
	if err != nil {
		return err
	}
	req, _ := f.requirementBy(ctx, "工期要求")
	sec := sectionForRequirement(det, req.ID)
	if sec == nil {
		return errors.New("G09 section not found")
	}
	correct := "我方承诺总工期 90 天，并按实施计划组织交付。"
	_, err1 := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{TaskID: f.task.ID, SectionID: sec.ID,
		BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit, ContentMD: correct,
		CommitmentIDs: []int64{draft.ID}})
	addGateCheck(g, "G09.a_unconfirmed", "ErrCommitmentUnconfirmed", errCode(err1), errors.Is(err1, db.ErrCommitmentUnconfirmed), "")
	if _, err := f.app.d.ConfirmCommitment(ctx, f.tenant, f.task.ID, in.Code, actor); err != nil {
		return err
	}
	conf, _ := f.app.d.GetCommitment(ctx, f.tenant, f.task.ID, in.Code)
	_, err2 := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{TaskID: f.task.ID, SectionID: sec.ID,
		BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit, ContentMD: "我方承诺总工期 120 天。",
		CommitmentIDs: []int64{conf.ID}})
	addGateCheck(g, "G09.b_mismatch", "ErrCommitmentMismatch", errCode(err2), errors.Is(err2, db.ErrCommitmentMismatch), "")
	res, err := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{TaskID: f.task.ID, SectionID: sec.ID,
		BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit, ContentMD: correct,
		CommitmentIDs: []int64{conf.ID}})
	if err != nil {
		return err
	}
	addGateCheck(g, "G09.remediated", "applied", res.Outcome, res.Outcome == db.SaveApplied, "")
	g.Observed = "draft 台账引用被拒 + 正文值与台账冲突被拒 -> 确认并按台账渲染后 applied"
	g.Remediated = res.Outcome == db.SaveApplied
	return nil
}

func gateG09(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G09", Gate: "commitment_unconfirmed_or_text_mismatch", SignalType: "write_rejected",
		Sentinel: "ErrCommitmentUnconfirmed / ErrCommitmentMismatch", Trigger: "章节引用 draft 台账；正文台账值与确认值不同"}
	if err := f.g09run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g10run(ctx context.Context, g *Gate) error {
	if _, err := f.prepareTask(ctx, "g10", false); err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "投标报价")
	if err != nil {
		return err
	}
	threshold, committed := 500000.0, 300000.0
	in := db.CommitmentInput{Code: "C-PRICE-G10", RequirementID: &req.ID, Metric: db.MetricPrice, MetricLabel: "投标总价", Unit: "元",
		ThresholdCmp: "<=", TenderThresholdRaw: "最高限价 500000 元", TenderThresholdValue: &threshold,
		CommittedRaw: "300000.00 元", CommittedValue: &committed, Owner: "商务负责人", Rationale: "成本测算"}
	if _, err := f.app.d.UpsertCommitment(ctx, f.tenant, f.task.ID, in, actor); err != nil {
		return err
	}
	if _, err := f.app.d.ConfirmCommitment(ctx, f.tenant, f.task.ID, in.Code, actor); err != nil {
		return err
	}
	q1, val1, err := f.createQuote(ctx, "inconsistent")
	if err != nil {
		return err
	}
	_, _, confErr1 := f.app.d.ConfirmBidQuote(ctx, f.tenant, f.task.ID, q1.ID, "commercial")
	addGateCheck(g, "G10.line_mismatch", "ErrQuoteInconsistent", errCode(confErr1), errors.Is(confErr1, db.ErrQuoteInconsistent) && !val1.OK, "")
	q2, val2, err := f.createQuote(ctx, "over_ceiling")
	if err != nil {
		return err
	}
	_, _, confErr2 := f.app.d.ConfirmBidQuote(ctx, f.tenant, f.task.ID, q2.ID, "commercial")
	ceilingFail := false
	for _, c := range val2.Checks {
		if c.Rule == "ceiling" && c.Status == "fail" {
			ceilingFail = true
		}
	}
	addGateCheck(g, "G10.over_ceiling", "ErrQuoteInconsistent + ceiling fail", errCode(confErr2),
		errors.Is(confErr2, db.ErrQuoteInconsistent) && ceilingFail, "")
	q3, _, err := f.createQuote(ctx, "")
	if err != nil {
		return err
	}
	addGateCheck(g, "G10.remediated", "confirmed", q3.Status, q3.Status == "confirmed", "")
	g.Observed = "行金额不一致/超限价均被拒 -> 修正后报价 confirmed"
	g.Remediated = q3.Status == "confirmed"
	return nil
}

func gateG10(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G10", Gate: "quote_inconsistent_or_over_ceiling", SignalType: "write_rejected", Sentinel: "ErrQuoteInconsistent",
		Trigger: "行金额≠单价×数量、声明总额≠行合计或超最高限价"}
	if err := f.g10run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g11run(ctx context.Context, g *Gate) error {
	det, runs, es, err := f.buildReady(ctx, "g11", nil, true)
	if err != nil {
		return fmt.Errorf("build ready: %w", err)
	}
	qual, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	expired := "2026-01-01"
	updated := *es.QualValid
	updated.ValidUntil = &expired
	updated.UpdatedBy = actor
	if _, err := f.app.d.UpdateCompanyCredential(ctx, &updated); err != nil {
		return err
	}
	rd, err := f.readiness(ctx)
	if err != nil {
		return err
	}
	addGateCheck(g, "G11.blocker", "admissibility_stale", kindList(rd.Blockers), hasBlocker(rd, db.BlockAdmissibilityStale) && !rd.Ready, kindList(rd.Blockers))
	sec := sectionForRequirement(det, qual.ID)
	cur, err := f.app.d.GetSection(ctx, f.tenant, f.task.ID, sec.ID)
	if err != nil {
		return err
	}
	addGateCheck(g, "G11.section_demoted", "stale_evidence / no approved rev", cur.Section.Status+" approved="+boolText(cur.Section.ApprovedRevNo != nil),
		cur.Section.ApprovedRevNo == nil && (cur.Section.Status == db.SectionStaleEvidence || cur.Section.Status == db.SectionStaleCommitment), "")
	valid := "2027-06-30"
	updated.ValidUntil = &valid
	if _, err := f.app.d.UpdateCompanyCredential(ctx, &updated); err != nil {
		return err
	}
	run, err := f.evaluate(ctx, qual.ID)
	if err != nil {
		return err
	}
	runs[qual.ID] = run
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	if err := f.writeSections(ctx, det, runs, commits); err != nil {
		return err
	}
	if _, _, err := f.matrixAndConsistency(ctx); err != nil {
		return err
	}
	rd2, err := f.readiness(ctx)
	if err != nil {
		return err
	}
	addGateCheck(g, "G11.full_ready", "true", boolText(rd2.Ready), rd2.Ready, blockerText(rd2))
	g.Observed = "更新已引用证书 -> admissibility_stale + 章节降级 -> 重评/重写/批准/矩阵/一致性 -> Ready=true"
	g.Remediated = rd2.Ready
	g.FullReady = rd2.Ready
	return nil
}

func gateG11(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G11", Gate: "evidence_source_changed", SignalType: "readiness_blocker", Sentinel: "admissibility_stale",
		Trigger: "在 ready 基线上更新已引用证书字段"}
	if err := f.g11run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

// buildObjectiveShort 构造客观分证据不足状态：客观评分项 MinCount=2，只绑定 1 条可采业绩。
func (f *flow) buildObjectiveShort(ctx context.Context, scenario string) (*db.OutlineDetail, map[int64]*db.AdmissibilityRun, *db.RequirementItem, *evidenceSet, error) {
	det, err := f.prepareTask(ctx, scenario, true)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	qual, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	perf, err := f.requirementBy(ctx, "业绩要求")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	obj, err := f.requirementBy(ctx, "类似项目业绩")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	pm, err := f.requirementBy(ctx, "项目经理")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	prod, err := f.requirementBy(ctx, "摄像机型号")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, b := range []struct {
		req    *db.RequirementItem
		domain string
		id     int64
	}{
		{qual, db.EvidenceDomainCredential, es.QualValid.ID},
		{perf, db.EvidenceDomainPerformance, es.Perf1.ID},
		{perf, db.EvidenceDomainPerformance, es.Perf2.ID},
		{obj, db.EvidenceDomainPerformance, es.Perf1.ID},
		{pm, db.EvidenceDomainCredential, es.Person.ID},
		{prod, db.EvidenceDomainCredential, es.ProductOK.ID},
	} {
		if err := f.bind(ctx, b.req.ID, b.domain, b.id); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	runs := map[int64]*db.AdmissibilityRun{}
	for _, req := range []*db.RequirementItem{qual, perf, obj, pm, prod} {
		run, err := f.evaluate(ctx, req.ID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		runs[req.ID] = run
	}
	if runs[obj.ID].Verdict != compliance.Inadmissible {
		return nil, nil, nil, nil, fmt.Errorf("objective run should be inadmissible with 1/2 evidence, got %s", runs[obj.ID].Verdict)
	}
	if _, err := f.seedCommitments(ctx, defaultCommitmentSeeds()); err != nil {
		return nil, nil, nil, nil, err
	}
	if _, err := f.computeObjective(ctx); err != nil {
		return nil, nil, nil, nil, err
	}
	if _, _, err := f.createQuote(ctx, ""); err != nil {
		return nil, nil, nil, nil, err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := f.writeSections(ctx, det, runs, commits); err != nil {
		return nil, nil, nil, nil, err
	}
	if _, _, err := f.matrixAndConsistency(ctx); err != nil {
		return nil, nil, nil, nil, err
	}
	return det, runs, obj, es, nil
}

func (f *flow) recoverObjective(ctx context.Context, det *db.OutlineDetail, runs map[int64]*db.AdmissibilityRun, obj *db.RequirementItem, es *evidenceSet) (*db.ExportReadiness, error) {
	if err := f.bind(ctx, obj.ID, db.EvidenceDomainPerformance, es.Perf2.ID); err != nil {
		return nil, err
	}
	run, err := f.evaluate(ctx, obj.ID)
	if err != nil {
		return nil, err
	}
	runs[obj.ID] = run
	if _, err := f.computeObjective(ctx); err != nil {
		return nil, err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return nil, err
	}
	if err := f.writeSections(ctx, det, runs, commits); err != nil {
		return nil, err
	}
	if _, _, err := f.matrixAndConsistency(ctx); err != nil {
		return nil, err
	}
	return f.readiness(ctx)
}

func (f *flow) g12run(ctx context.Context, g *Gate) error {
	det, runs, obj, es, err := f.buildObjectiveShort(ctx, "g12")
	if err != nil {
		return err
	}
	rd, err := f.readiness(ctx)
	if err != nil {
		return err
	}
	addGateCheck(g, "G12.blocker", "objective_evidence_missing", kindList(rd.Blockers),
		hasBlocker(rd, db.BlockObjectiveEvidenceMissing) && !rd.Ready, blockerText(rd))
	rd2, err := f.recoverObjective(ctx, det, runs, obj, es)
	if err != nil {
		return err
	}
	addGateCheck(g, "G12.full_ready", "true", boolText(rd2.Ready), rd2.Ready, blockerText(rd2))
	g.Observed = "MinCount=2 仅有 1 条可采证据 -> objective_evidence_missing -> 补足/重算/重写/批准/矩阵/一致性 -> Ready=true"
	g.Remediated = rd2.Ready
	g.FullReady = rd2.Ready
	return nil
}

func gateG12(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G12", Gate: "objective_evidence_missing", SignalType: "readiness_blocker", Sentinel: "objective_evidence_missing",
		Trigger: "要求 MinCount=2 的客观评分项只有 1 条 admissible 证据"}
	if err := f.g12run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g13run(ctx context.Context, g *Gate) error {
	det, runs, obj, es, err := f.buildObjectiveShort(ctx, "g13")
	if err != nil {
		return err
	}
	jobs, _, err := f.createExport(ctx)
	var be *db.ExportBlockedError
	blocked := errors.As(err, &be) && len(jobs) == 0
	var queued int
	_ = f.app.d.QueryRowContext(ctx, `SELECT COUNT(*) FROM export_jobs WHERE tenant_id=$1 AND task_id=$2 AND status='queued'`,
		f.tenant, f.task.ID).Scan(&queued)
	addGateCheck(g, "G13.blocked", "ExportBlockedError; jobs=0; queued=0", fmt.Sprintf("%s jobs=%d queued=%d", errCode(err), len(jobs), queued),
		blocked && queued == 0, "")
	rd2, err := f.recoverObjective(ctx, det, runs, obj, es)
	if err != nil {
		return err
	}
	jobs2, _, err := f.createExport(ctx)
	if err != nil {
		return err
	}
	addGateCheck(g, "G13.remediated", "1 queued job", fmt.Sprintf("jobs=%d ready=%v", len(jobs2), rd2.Ready),
		len(jobs2) == 1 && jobs2[0].Status == db.ExportQueued && rd2.Ready, "")
	g.Observed = "持久化 D2 门禁拒绝导出且不产生 queued 任务 -> 完整恢复 ready 后创建 1 个 queued 任务"
	g.Remediated = len(jobs2) == 1 && rd2.Ready
	g.FullReady = rd2.Ready
	return nil
}

func gateG13(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G13", Gate: "export_blocked_by_d2_gate", SignalType: "readiness_blocker", Sentinel: "ExportBlockedError",
		Trigger: "在 D2 证据不足状态下调用 CreateExportJobs"}
	if err := f.g13run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g14run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g14", false)
	if err != nil {
		return err
	}
	manual, _, err := f.app.d.UpsertCompanyCredential(ctx, &db.CompanyCredential{TenantID: f.tenant, Category: db.CredentialQualification,
		Code: "QUAL-MANUAL", Title: "安防资质（虚构·主体待人工确认）", SubjectName: "", SubjectType: "unknown",
		ValidUntil: strp("2027-06-30"), Scope: "", CreatedBy: actor})
	if err != nil {
		return err
	}
	for _, key := range []string{"qual_valid", "qual_valid_validity"} {
		if err := f.addOriginal(ctx, db.EvidenceDomainCredential, manual.ID, key); err != nil {
			return err
		}
	}
	req, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	if err := f.bind(ctx, req.ID, db.EvidenceDomainCredential, manual.ID); err != nil {
		return err
	}
	run, err := f.evaluate(ctx, req.ID)
	if err != nil {
		return err
	}
	it := findAdmissibilityItem(run, db.EvidenceDomainCredential, manual.ID)
	addGateCheck(g, "G14.manual", "manual", runOrItem(run, it), run.Verdict == compliance.Manual && it != nil && it.Verdict == compliance.Manual, itemReasons(it))
	sec := sectionForRequirement(det, req.ID)
	_, saveErr := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{TaskID: f.task.ID, SectionID: sec.ID,
		BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit, ContentMD: "响应要求：提供资质证明。",
		AdmissibilityItemIDs: []int64{it.ID}})
	addGateCheck(g, "G14.write_rejected", "ErrAdmissibilityManual", errCode(saveErr), errors.Is(saveErr, db.ErrAdmissibilityManual), "")
	resolved, err := f.app.d.ResolveAdmissibility(ctx, f.tenant, f.task.ID, run.ID, []db.AdmissibilityDecision{
		{ItemID: it.ID, Verdict: compliance.Admissible, Reason: "人工核对：证书主体为投标人自身，范围覆盖本项目（虚构演示）"},
	}, "reviewer")
	if err != nil {
		return err
	}
	addGateCheck(g, "G14.remediated", "admissible", resolved.Verdict, resolved.Verdict == compliance.Admissible, "")
	saveRes, err := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{TaskID: f.task.ID, SectionID: sec.ID,
		BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit, ContentMD: "响应要求：提供资质证明，主体与范围见人工复核记录。",
		AdmissibilityItemIDs: []int64{resolved.Items[0].ID}})
	if err != nil {
		return err
	}
	addGateCheck(g, "G14.section_applied", "applied", saveRes.Outcome, saveRes.Outcome == db.SaveApplied, "")
	g.Observed = "主体/范围只能人工判断 -> manual + ErrAdmissibilityManual -> 人工复核新版本后 admissible"
	g.Remediated = resolved.Verdict == compliance.Admissible && saveRes.Outcome == db.SaveApplied
	return nil
}

func gateG14(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D2-G14", Gate: "admissibility_manual", SignalType: "write_rejected", Sentinel: "ErrAdmissibilityManual",
		Trigger: "业绩/资质同类性只能人工判断，当前运行含 manual 明细"}
	if err := f.g14run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func errCode(err error) string {
	if err == nil {
		return "nil"
	}
	switch {
	case errors.Is(err, db.ErrCommitmentBelowThreshold):
		return "ErrCommitmentBelowThreshold"
	case errors.Is(err, db.ErrCommitmentSignoffRequired):
		return "ErrCommitmentSignoffRequired"
	case errors.Is(err, db.ErrCommitmentUnconfirmed):
		return "ErrCommitmentUnconfirmed"
	case errors.Is(err, db.ErrCommitmentMismatch):
		return "ErrCommitmentMismatch"
	case errors.Is(err, db.ErrQuoteInconsistent):
		return "ErrQuoteInconsistent"
	case errors.Is(err, db.ErrEvidenceInadmissible):
		return "ErrEvidenceInadmissible"
	case errors.Is(err, db.ErrAdmissibilityManual):
		return "ErrAdmissibilityManual"
	case errors.Is(err, db.ErrAdmissibilityStale):
		return "ErrAdmissibilityStale"
	}
	var be *db.ExportBlockedError
	if errors.As(err, &be) {
		return "ExportBlockedError"
	}
	s := err.Error()
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[:i]
	}
	return s
}
