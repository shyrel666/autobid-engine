package main

// D3-G01 ~ D3-G10 门禁正反例。每个门禁使用独立租户与任务；写入拒绝与持久化 readiness blocker 分开记录。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

func (a *app) runGates(ctx context.Context, f *flow) {
	seen := map[string]bool{}
	for _, fn := range []func(context.Context, *flow) *Gate{
		gateG01, gateG02, gateG03, gateG04, gateG05,
		gateG06, gateG07, gateG08, gateG09, gateG10,
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

func (f *flow) sectionPlanFor(ctx context.Context, det *db.OutlineDetail, sec *db.ProposalSection, runs map[int64]*db.AdmissibilityRun, commits []*db.Commitment) (sectionPlan, error) {
	_, byID, err := f.requirements(ctx)
	if err != nil {
		return sectionPlan{}, err
	}
	reqs := make([]*db.RequirementItem, 0, len(sec.RequirementIDs))
	for _, rid := range sec.RequirementIDs {
		if it := byID[rid]; it != nil && !it.IsSuperseded() {
			reqs = append(reqs, it)
		}
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Code < reqs[j].Code })
	plan := buildSectionPlan(renderInput{Section: sec, Requirements: reqs, Runs: runs, Commitments: commits})
	plan.StoryboardIn.TaskID = f.task.ID
	plan.StoryboardIn.SectionID = sec.ID
	return plan, nil
}

// D3-G01：未通过粉队评审即领用或保存章节正文。
func gateG01(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G01", Gate: "storyboard_required_before_writing", SignalType: "write_rejected",
		Sentinel: "ErrStoryboardMissing / ErrStoryboardNotApproved", Trigger: "D3 启用后，故事板未创建或处于 draft 时尝试 ClaimSectionWrite 与 SaveSectionRevision"}
	if err := f.g01run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g01run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g01", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		return err
	}
	if _, err := f.seedCommitments(ctx, defaultCommitmentSeeds()); err != nil {
		return err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	sec := sectionForRequirement(det, req.ID)
	if sec == nil || len(det.Sections) < 2 {
		return errors.New("G01 section not found")
	}
	rootSec := det.Sections[0]
	plan, err := f.sectionPlanFor(ctx, det, sec, runs, commits)
	if err != nil {
		return err
	}
	rootPlan, err := f.sectionPlanFor(ctx, det, rootSec, runs, commits)
	if err != nil {
		return err
	}
	// 先在 sec 创建 draft 故事板以激活任务的 D3 门禁，此时 rootSec 尚无故事板且无前置章节依赖。
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
		return err
	}
	_, claimMissingErr := f.app.d.ClaimSectionWrite(ctx, f.tenant, f.task.ID, rootSec.ID, actor, time.Minute)
	addGateCheck(g, "G01.claim_missing_storyboard", "ErrStoryboardMissing", errCode(claimMissingErr),
		errors.Is(claimMissingErr, db.ErrStoryboardMissing), "")

	// 为 rootSec 创建 draft 故事板（仍未通过粉队评审），验证 ClaimSectionWrite 返回 ErrStoryboardNotApproved。
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, rootPlan.StoryboardIn); err != nil {
		return err
	}
	_, claimDraftErr := f.app.d.ClaimSectionWrite(ctx, f.tenant, f.task.ID, rootSec.ID, actor, time.Minute)
	addGateCheck(g, "G01.claim_unapproved_storyboard", "ErrStoryboardNotApproved", errCode(claimDraftErr),
		errors.Is(claimDraftErr, db.ErrStoryboardNotApproved), "")

	_, saveDraftErr := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: plan.TemplateMD,
	})
	addGateCheck(g, "G01.save_unapproved_storyboard", "ErrStoryboardNotApproved", errCode(saveDraftErr),
		errors.Is(saveDraftErr, db.ErrStoryboardNotApproved), "")

	rd, err := f.readiness(ctx)
	if err != nil {
		return err
	}
	addGateCheck(g, "G01.readiness_blockers", "storyboard_missing+storyboard_unapproved",
		boolText(hasBlocker(rd, db.BlockStoryboardMissing) && hasBlocker(rd, db.BlockStoryboardUnapproved)),
		hasBlocker(rd, db.BlockStoryboardMissing) && hasBlocker(rd, db.BlockStoryboardUnapproved), kindList(rd.Blockers))

	if _, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, rootSec.ID, actor); err != nil {
		return err
	}
	if _, err := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, rootSec.ID, "粉队批准", pinkReviewer); err != nil {
		return err
	}
	claim, err := f.app.d.ClaimSectionWrite(ctx, f.tenant, f.task.ID, rootSec.ID, actor, time.Minute)
	addGateCheck(g, "G01.remediated_claim", "claimed", boolText(err == nil && claim != nil && claim.LeaseToken > 0),
		err == nil && claim != nil && claim.LeaseToken > 0, "")
	g.Observed = "无故事板/未批准故事板时领用与保存均被拒绝 -> 粉队批准后领用成功"
	g.Remediated = err == nil && claim != nil && claim.LeaseToken > 0
	return nil
}

// D3-G02：故事板未完整覆盖章节已映射要求或缺少做法/收益/论点。
func gateG02(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G02", Gate: "storyboard_missing_requirement_coverage", SignalType: "write_rejected",
		Sentinel: "ErrStoryboardInvalid", Trigger: "故事板遗漏已映射要求或缺少我方做法/买方收益时提交粉队评审"}
	if err := f.g02run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g02run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g02", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		return err
	}
	if _, err := f.seedCommitments(ctx, defaultCommitmentSeeds()); err != nil {
		return err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	sec := sectionForRequirement(det, req.ID)
	if sec == nil {
		return errors.New("G02 section not found")
	}
	// 1) 遗漏已映射要求
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, db.UpsertStoryboardInput{
		TaskID:        f.task.ID,
		SectionID:     sec.ID,
		ThemeSentence: "围绕智慧园区安防系统采购项目构建资格审查与证明材料响应体系",
		ScoringPoints: []db.StoryboardScoringPoint{},
	}); err != nil {
		return err
	}
	_, subErr1 := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor)
	addGateCheck(g, "G02.missing_mapped_reqs", "ErrStoryboardInvalid", errCode(subErr1),
		errors.Is(subErr1, db.ErrStoryboardInvalid), "")

	// 2) 缺少买方收益
	plan, err := f.sectionPlanFor(ctx, det, sec, runs, commits)
	if err != nil {
		return err
	}
	badPlan := plan.StoryboardIn
	badPoints := append([]db.StoryboardScoringPoint{}, badPlan.ScoringPoints...)
	badPoints[0].BuyerBenefit = ""
	badPlan.ScoringPoints = badPoints
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, badPlan); err != nil {
		return err
	}
	_, subErr2 := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor)
	addGateCheck(g, "G02.missing_buyer_benefit", "ErrStoryboardInvalid", errCode(subErr2),
		errors.Is(subErr2, db.ErrStoryboardInvalid), "")

	// 3) 补齐全部映射要求与收益后提交通过
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
		return err
	}
	sb, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor)
	if err != nil {
		return err
	}
	addGateCheck(g, "G02.remediated", db.StoryboardPendingPink, sb.Status, sb.Status == db.StoryboardPendingPink, "")
	g.Observed = "遗漏映射要求或买方收益提交被拒 -> 补齐后进入 pending_pink_review"
	g.Remediated = sb.Status == db.StoryboardPendingPink
	return nil
}

// D3-G03：粉队评审自审自批（作者与评审人相同）。
func gateG03(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G03", Gate: "pink_team_self_approval_blocked", SignalType: "write_rejected",
		Sentinel: "ErrPinkSelfApproval", Trigger: "故事板创建人尝试自批粉队评审"}
	if err := f.g03run(ctx, g); err != nil {
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
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		return err
	}
	if _, err := f.seedCommitments(ctx, defaultCommitmentSeeds()); err != nil {
		return err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	sec := sectionForRequirement(det, req.ID)
	plan, err := f.sectionPlanFor(ctx, det, sec, runs, commits)
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
		return err
	}
	if _, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor); err != nil {
		return err
	}
	_, selfErr := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, sec.ID, "作者自批", actor)
	addGateCheck(g, "G03.self_approve_rejected", "ErrPinkSelfApproval", errCode(selfErr),
		errors.Is(selfErr, db.ErrPinkSelfApproval), "")

	sb, err := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, sec.ID, "独立粉队评审通过", pinkReviewer)
	if err != nil {
		return err
	}
	addGateCheck(g, "G03.remediated", db.StoryboardPinkApproved, sb.Status,
		sb.Status == db.StoryboardPinkApproved && sb.ApprovedBy == pinkReviewer, "")
	g.Observed = "作者自批返回 ErrPinkSelfApproval -> 独立粉队评审人批准成功"
	g.Remediated = sb.Status == db.StoryboardPinkApproved
	return nil
}

// D3-G04：章节正文包含未绑定证据的投标人事实断言。
func gateG04(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G04", Gate: "unbound_bidder_fact_rejected", SignalType: "write_rejected",
		Sentinel: "ErrUngroundedClaim", Trigger: "正文含未绑定 [EVID:...] 的资质/业绩事实断言"}
	if err := f.g04run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g04run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g04", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		return err
	}
	if _, err := f.seedCommitments(ctx, defaultCommitmentSeeds()); err != nil {
		return err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	sec := sectionForRequirement(det, req.ID)
	plan, err := f.sectionPlanFor(ctx, det, sec, runs, commits)
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
		return err
	}
	if _, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor); err != nil {
		return err
	}
	if _, err := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, sec.ID, "粉队批准", pinkReviewer); err != nil {
		return err
	}
	ungroundedMD := plan.TemplateMD + "\n\n我方具备安全技术防范系统设计施工一级资质证书且承建过多个千万级类似安防项目。"
	_, saveErr := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: ungroundedMD,
	})
	addGateCheck(g, "G04.unbound_fact_rejected", "ErrUngroundedClaim", errCode(saveErr),
		errors.Is(saveErr, db.ErrUngroundedClaim), "")

	res, err := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: plan.TemplateMD,
	})
	if err != nil {
		return err
	}
	wv, err := f.app.d.GetSectionWritingVerification(ctx, f.tenant, f.task.ID, sec.ID)
	if err != nil {
		return err
	}
	addGateCheck(g, "G04.remediated", "passed/0", fmt.Sprintf("%s/%d", wv.Verdict, wv.UngroundedClaimCount),
		res.Outcome == db.SaveApplied && wv.Verdict == compliance.WritingVerdictPassed && wv.UngroundedClaimCount == 0, "")
	g.Observed = "未绑定 [EVID:...] 的事实句被拒（ErrUngroundedClaim） -> 改为受控引用标记后核验通过"
	g.Remediated = wv.Verdict == compliance.WritingVerdictPassed && wv.UngroundedClaimCount == 0
	return nil
}

// D3-G05：已绑定 [EVID:...] 但捏造或篡改证书编号/合同金额。
func gateG05(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G05", Gate: "mismatched_fact_citation_rejected", SignalType: "write_rejected",
		Sentinel: "ErrUngroundedClaim", Trigger: "句子带 [EVID:...] 但证书编号与证据库记录不一致"}
	if err := f.g05run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g05run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g05", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		return err
	}
	if _, err := f.seedCommitments(ctx, defaultCommitmentSeeds()); err != nil {
		return err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	sec := sectionForRequirement(det, req.ID)
	plan, err := f.sectionPlanFor(ctx, det, sec, runs, commits)
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
		return err
	}
	if _, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor); err != nil {
		return err
	}
	if _, err := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, sec.ID, "粉队批准", pinkReviewer); err != nil {
		return err
	}
	qualItem := findAdmissibilityItem(runs[req.ID], db.EvidenceDomainCredential, es.QualValid.ID)
	if qualItem == nil {
		return errors.New("G05 qualItem not found")
	}
	tamperedMD := fmt.Sprintf("%s\n\n我方持有安全技术防范系统设计施工二级资质证书（证书编号：DEMO-AF-9999）[EVID:%d]。", plan.TemplateMD, qualItem.ID)
	_, saveErr := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: tamperedMD,
	})
	addGateCheck(g, "G05.tampered_cert_no_rejected", "ErrUngroundedClaim", errCode(saveErr),
		errors.Is(saveErr, db.ErrUngroundedClaim), "")

	res, err := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: plan.TemplateMD,
	})
	if err != nil {
		return err
	}
	addGateCheck(g, "G05.remediated", db.SaveApplied, res.Outcome, res.Outcome == db.SaveApplied, "")
	g.Observed = "篡改证书编号 DEMO-AF-9999 被拒（ErrUngroundedClaim） -> 使用 [[EVID:...]] 确定性展开后保存成功"
	g.Remediated = res.Outcome == db.SaveApplied
	return nil
}

// D3-G06：承诺句未绑定 [COMMIT:...] 或承诺数值与台账不一致。
func gateG06(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G06", Gate: "unbound_or_mismatched_commitment_rejected", SignalType: "write_rejected",
		Sentinel: "ErrUngroundedClaim", Trigger: "承诺句未带 [COMMIT:...] 或带 [COMMIT:...] 但工期数值与台账不一致"}
	if err := f.g06run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g06run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g06", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		return err
	}
	commitMap, err := f.seedCommitments(ctx, defaultCommitmentSeeds())
	if err != nil {
		return err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "工期要求")
	if err != nil {
		return err
	}
	sec := sectionForRequirement(det, req.ID)
	plan, err := f.sectionPlanFor(ctx, det, sec, runs, commits)
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
		return err
	}
	if _, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor); err != nil {
		return err
	}
	if _, err := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, sec.ID, "粉队批准", pinkReviewer); err != nil {
		return err
	}
	// 1) 未带 [COMMIT:...] 的承诺句
	unboundMD := strings.ReplaceAll(plan.TemplateMD, "[[COMMIT:C-DURATION]]", "我方郑重承诺本项目总工期不超过 90 天。")
	_, saveErr1 := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: unboundMD,
	})
	addGateCheck(g, "G06.unbound_commitment_rejected", "ErrUngroundedClaim", errCode(saveErr1),
		errors.Is(saveErr1, db.ErrUngroundedClaim), "")

	// 2) 带 [COMMIT:...] 但工期改成 60 天（与台账 90 天不一致）
	durCommit := commitMap[db.MetricDuration]
	mismatchMD := strings.ReplaceAll(plan.TemplateMD, "[[COMMIT:C-DURATION]]",
		fmt.Sprintf("我方郑重承诺总工期：60 天 [COMMIT:%d]。", durCommit.ID))
	_, saveErr2 := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: mismatchMD,
	})
	okMismatch := errors.Is(saveErr2, db.ErrUngroundedClaim) || errors.Is(saveErr2, db.ErrCommitmentMismatch)
	addGateCheck(g, "G06.mismatch_commitment_rejected", "ErrUngroundedClaim", errCode(saveErr2), okMismatch, "")

	// 3) 使用 [[COMMIT:C-DURATION]] 确定性展开
	res, err := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: plan.TemplateMD,
	})
	if err != nil {
		return err
	}
	addGateCheck(g, "G06.remediated", db.SaveApplied, res.Outcome, res.Outcome == db.SaveApplied, "")
	g.Observed = "未绑定承诺或工期数值篡改均被拒 -> 使用 [[COMMIT:C-DURATION]] 确定性展开后通过"
	g.Remediated = res.Outcome == db.SaveApplied
	return nil
}

// D3-G07：受控引用标记指向不可采证据或未确认承诺台账。
func gateG07(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G07", Gate: "render_inadmissible_or_unconfirmed_token_rejected", SignalType: "write_rejected",
		Sentinel: "ErrEvidenceInadmissible / ErrCommitmentUnconfirmed", Trigger: "RenderSectionGroundedDraft 引用 inadmissible 证据或 draft 承诺台账"}
	if err := f.g07run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g07run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g07", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	reqQual, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	// 先绑定过期证书评估出 inadmissible item
	if err := f.bind(ctx, reqQual.ID, db.EvidenceDomainCredential, es.QualExpired.ID); err != nil {
		return err
	}
	badRun, err := f.evaluate(ctx, reqQual.ID)
	if err != nil {
		return err
	}
	badItem := findAdmissibilityItem(badRun, db.EvidenceDomainCredential, es.QualExpired.ID)
	if badItem == nil {
		return errors.New("G07 badItem not found")
	}
	sec := sectionForRequirement(det, reqQual.ID)
	_, renderErr1 := f.app.d.RenderSectionGroundedDraft(ctx, f.tenant, f.task.ID, sec.ID, fmt.Sprintf("[[EVID:%d]]", badItem.ID))
	addGateCheck(g, "G07.inadmissible_token_rejected", "ErrEvidenceInadmissible", errCode(renderErr1),
		errors.Is(renderErr1, db.ErrEvidenceInadmissible), "")

	// 创建一条 draft 状态的承诺台账
	reqDur, err := f.requirementBy(ctx, "工期要求")
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertCommitment(ctx, f.tenant, f.task.ID, db.CommitmentInput{
		Code: "C-DRAFT-G07", RequirementID: &reqDur.ID, Metric: db.MetricDuration, MetricLabel: "总工期",
		Unit: "天", ThresholdCmp: "<=", TenderThresholdRaw: "不超过 90 日历天", TenderThresholdValue: floatp(90),
		CommittedRaw: "90 天", CommittedValue: floatp(90), Owner: "项目经理", Rationale: "实施计划",
	}, actor); err != nil {
		return err
	}
	_, renderErr2 := f.app.d.RenderSectionGroundedDraft(ctx, f.tenant, f.task.ID, sec.ID, "[[COMMIT:C-DRAFT-G07]]")
	addGateCheck(g, "G07.unconfirmed_commit_token_rejected", "ErrCommitmentUnconfirmed", errCode(renderErr2),
		errors.Is(renderErr2, db.ErrCommitmentUnconfirmed), "")

	// 解绑过期证书，绑定有效证书并确认承诺后重新渲染
	if err := f.app.d.UnbindRequirementEvidence(ctx, f.tenant, f.task.ID, reqQual.ID, db.EvidenceDomainCredential, es.QualExpired.ID); err != nil {
		return err
	}
	if err := f.bind(ctx, reqQual.ID, db.EvidenceDomainCredential, es.QualValid.ID); err != nil {
		return err
	}
	goodRun, err := f.evaluate(ctx, reqQual.ID)
	if err != nil {
		return err
	}
	goodItem := findAdmissibilityItem(goodRun, db.EvidenceDomainCredential, es.QualValid.ID)
	if _, err := f.app.d.ConfirmCommitment(ctx, f.tenant, f.task.ID, "C-DRAFT-G07", actor); err != nil {
		return err
	}
	res, err := f.app.d.RenderSectionGroundedDraft(ctx, f.tenant, f.task.ID, sec.ID,
		fmt.Sprintf("[[EVID:%d]]\n[[COMMIT:C-DRAFT-G07]]", goodItem.ID))
	if err != nil {
		return err
	}
	ok := strings.Contains(res.RenderedMD, "DEMO-AF-0001") && strings.Contains(res.RenderedMD, "90 天")
	addGateCheck(g, "G07.remediated", "rendered", boolText(ok), ok, "")
	g.Observed = "inadmissible 证据与 draft 承诺渲染被拒 -> 切换有效证据并确认台账后确定性展开成功"
	g.Remediated = ok
	return nil
}

// D3-G08：章节正文充斥通用八股套话或缺乏招标特定要素。
func gateG08(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G08", Gate: "boilerplate_or_low_specificity_rejected", SignalType: "write_rejected",
		Sentinel: "ErrWritingVerifyFailed", Trigger: "保存充斥八股套话、缺乏项目特定要素的章节修订"}
	if err := f.g08run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g08run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g08", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		return err
	}
	if _, err := f.seedCommitments(ctx, defaultCommitmentSeeds()); err != nil {
		return err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	sec := sectionForRequirement(det, req.ID)
	plan, err := f.sectionPlanFor(ctx, det, sec, runs, commits)
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
		return err
	}
	if _, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor); err != nil {
		return err
	}
	if _, err := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, sec.ID, "粉队批准", pinkReviewer); err != nil {
		return err
	}
	boilerplateMD := "我公司高度重视本项目，本着科学管理、精心组织、质量第一、用户至上的原则，选派经验丰富的项目经理和技术骨干组建项目团队，确保按期高质量完成本项目全部建设内容。"
	_, saveErr := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: boilerplateMD,
	})
	addGateCheck(g, "G08.boilerplate_rejected", "ErrWritingVerificationFailed", errCode(saveErr),
		errors.Is(saveErr, db.ErrWritingVerificationFailed), "")

	res, err := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: plan.TemplateMD,
	})
	if err != nil {
		return err
	}
	addGateCheck(g, "G08.remediated", db.SaveApplied, res.Outcome, res.Outcome == db.SaveApplied, "")
	g.Observed = "八股套话/低针对性正文被拒（ErrWritingVerifyFailed） -> 写入针对性受控引用初稿后通过"
	g.Remediated = res.Outcome == db.SaveApplied
	return nil
}

// D3-G09：红队定稿自写自批（修订作者与定稿批准人相同）。
func gateG09(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G09", Gate: "red_team_self_approval_blocked", SignalType: "write_rejected",
		Sentinel: "ErrRedSelfApproval", Trigger: "章节修订创建人尝试调用 ApproveSectionRevision 自批定稿"}
	if err := f.g09run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g09run(ctx context.Context, g *Gate) error {
	det, err := f.prepareTask(ctx, "g09", false)
	if err != nil {
		return err
	}
	es, err := f.importEvidence(ctx)
	if err != nil {
		return err
	}
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		return err
	}
	if _, err := f.seedCommitments(ctx, defaultCommitmentSeeds()); err != nil {
		return err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	req, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return err
	}
	sec := sectionForRequirement(det, req.ID)
	plan, err := f.sectionPlanFor(ctx, det, sec, runs, commits)
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
		return err
	}
	if _, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor); err != nil {
		return err
	}
	if _, err := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, sec.ID, "粉队批准", pinkReviewer); err != nil {
		return err
	}
	res, err := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: plan.TemplateMD,
	})
	if err != nil {
		return err
	}
	_, selfErr := f.app.d.ApproveSectionRevision(ctx, f.tenant, f.task.ID, sec.ID, res.Revision.RevNo, actor)
	addGateCheck(g, "G09.self_approve_rejected", "ErrRedSelfApproval", errCode(selfErr),
		errors.Is(selfErr, db.ErrRedSelfApproval), "")

	approvedSec, err := f.app.d.ApproveSectionRevision(ctx, f.tenant, f.task.ID, sec.ID, res.Revision.RevNo, redReviewer)
	if err != nil {
		return err
	}
	ok := approvedSec.ApprovedRevNo != nil && *approvedSec.ApprovedRevNo == res.Revision.RevNo && approvedSec.ApprovedBy == redReviewer
	addGateCheck(g, "G09.remediated", "approved by redReviewer", approvedSec.ApprovedBy, ok, "")
	g.Observed = "章节作者自批定稿返回 ErrRedSelfApproval -> 独立红队评审人批准成功"
	g.Remediated = ok
	return nil
}

// D3-G10：上游承诺台账变更级联失效故事板与写作核验，阻断导出并完整恢复 Ready=true。
func gateG10(ctx context.Context, f *flow) *Gate {
	g := &Gate{ID: "D3-G10", Gate: "upstream_change_invalidates_storyboard_and_writing_verification", SignalType: "readiness_blocker",
		Sentinel: "storyboard_stale + writing_verification_stale", Trigger: "已就绪 D3 任务上修改工期承诺台账，触发故事板与写作核验级联失效"}
	if err := f.g10run(ctx, g); err != nil {
		return finishGate(g, err)
	}
	return g
}

func (f *flow) g10run(ctx context.Context, g *Gate) error {
	det, runs, _, err := f.buildReadyD3(ctx, "g10", nil, false)
	if err != nil {
		return err
	}
	reqDur, err := f.requirementBy(ctx, "工期要求")
	if err != nil {
		return err
	}
	sec := sectionForRequirement(det, reqDur.ID)
	if sec == nil {
		return errors.New("G10 section not found")
	}
	// 修改工期承诺台账（90 天 -> 85 天），状态回到 draft，触发 d3_commitment_changed 级联失效。
	if _, err := f.app.d.UpsertCommitment(ctx, f.tenant, f.task.ID, db.CommitmentInput{
		Code: "C-DURATION", RequirementID: &reqDur.ID, Metric: db.MetricDuration, MetricLabel: "总工期",
		Unit: "天", ThresholdCmp: "<=", TenderThresholdRaw: "不超过 90 日历天", TenderThresholdValue: floatp(90),
		CommittedRaw: "85 天", CommittedValue: floatp(85), CapabilityLimit: floatp(90),
		CapabilityBasis: "自有施工班组与加密排期", Owner: "项目经理", Rationale: "优化实施进度计划",
	}, actor); err != nil {
		return err
	}
	sbStale, err := f.app.d.GetSectionStoryboard(ctx, f.tenant, f.task.ID, sec.ID)
	if err != nil {
		return err
	}
	addGateCheck(g, "G10.storyboard_stale", db.StoryboardStale, sbStale.Status, sbStale.Status == db.StoryboardStale, sbStale.StaleReason)

	wvStale, err := f.app.d.GetSectionWritingVerification(ctx, f.tenant, f.task.ID, sec.ID)
	if err != nil {
		return err
	}
	addGateCheck(g, "G10.writing_verification_stale", compliance.WritingVerdictStale, wvStale.Verdict,
		wvStale.Verdict == compliance.WritingVerdictStale, wvStale.StaleReason)

	rdBlocked, err := f.readiness(ctx)
	if err != nil {
		return err
	}
	hasD3Blockers := !rdBlocked.Ready && hasBlocker(rdBlocked, db.BlockStoryboardStale) && hasBlocker(rdBlocked, db.BlockWritingVerificationStale)
	addGateCheck(g, "G10.readiness_blocked", "storyboard_stale+writing_verification_stale",
		boolText(hasD3Blockers), hasD3Blockers, kindList(rdBlocked.Blockers))

	jobsBlocked, _, expErr := f.createExport(ctx)
	var blockedErr *db.ExportBlockedError
	addGateCheck(g, "G10.export_blocked", "ExportBlockedError/0", fmt.Sprintf("%v/%d", errors.As(expErr, &blockedErr), len(jobsBlocked)),
		errors.As(expErr, &blockedErr) && len(jobsBlocked) == 0, "")

	// 恢复链路：确认新承诺 -> 重新提交并由粉队批准故事板 -> 重新渲染保存章节修订 -> 红队批准 -> 矩阵与一致性 -> Ready=true
	if _, err := f.app.d.ConfirmCommitment(ctx, f.tenant, f.task.ID, "C-DURATION", actor); err != nil {
		return err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return err
	}
	plan, err := f.sectionPlanFor(ctx, det, sec, runs, commits)
	if err != nil {
		return err
	}
	if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
		return err
	}
	if _, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor); err != nil {
		return err
	}
	if _, err := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, sec.ID, "粉队复核通过", pinkReviewer); err != nil {
		return err
	}
	cur, err := f.app.d.GetSection(ctx, f.tenant, f.task.ID, sec.ID)
	if err != nil {
		return err
	}
	res, err := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
		TaskID: f.task.ID, SectionID: sec.ID, BaseRevNo: cur.Section.CurrentRevNo, BasedOnOutlineVersion: det.Outline.Version,
		Source: db.RevHumanEdit, ContentMD: plan.TemplateMD, SummaryNote: sectionSummary(sec), DiffSummary: "同步 85 天工期承诺并重验",
	})
	if err != nil {
		return err
	}
	if _, err := f.app.d.ApproveSectionRevision(ctx, f.tenant, f.task.ID, sec.ID, res.Revision.RevNo, redReviewer); err != nil {
		return err
	}
	if _, _, err := f.matrixAndConsistency(ctx); err != nil {
		return err
	}
	rdReady, err := f.readiness(ctx)
	if err != nil {
		return err
	}
	addGateCheck(g, "G10.recovered_ready", "true", boolText(rdReady.Ready), rdReady.Ready && len(rdReady.Blockers) == 0, kindList(rdReady.Blockers))
	jobsOK, _, err := f.createExport(ctx)
	if err != nil {
		return err
	}
	addGateCheck(g, "G10.export_created", "1", strconv.Itoa(len(jobsOK)), len(jobsOK) == 1, "")
	g.Observed = "承诺台账变更 -> storyboard_stale + writing_verification_stale 阻断导出 -> 粉队重批、重渲染核验、红队重批后恢复 Ready=true"
	g.Remediated = rdReady.Ready && len(jobsOK) == 1
	g.FullReady = rdReady.Ready
	return nil
}
