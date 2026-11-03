package db

// D3 集成测试（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：
// 章节故事板与粉队评审状态机（作者/评审人分离、要求覆盖校验）、引用标记确定性渲染、
// 句子级证据绑定与写作核验门禁、红队审批职责分离、上游证据/承诺变更失效传播与完整恢复。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/compliance"
)

func TestD3StoryboardPinkReviewAndWritingVerification(t *testing.T) {
	f, tenant, store, _ := d2Setup(t)
	ctx := context.Background()

	valid := "2027-12-31"
	cred, _, err := f.d.UpsertCompanyCredential(ctx, &CompanyCredential{
		TenantID:      tenant,
		Category:      CredentialQualification,
		Code:          "CRED-ISO27001",
		Title:         "ISO/IEC 27001信息安全管理体系认证证书",
		SubjectName:   "演示智联科技有限公司（虚构）",
		SubjectType:   "bidder",
		CertificateNo: "ISO27001-2025-8899",
		Scope:         "智慧园区安全态势感知与零信任接入平台",
		ValidUntil:    &valid,
		Attributes:    json.RawMessage(`{"level":"一级"}`),
	})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	d2Original(t, f.d, store, tenant, EvidenceDomainCredential, cred.ID, DocKindCertificate, "附件3-P12", "docx", 1)

	code := "REQ-D3-01"
	title := "★智慧园区安全态势感知与零信任接入平台要求"
	desc := "支持智慧园区安全态势感知、零信任微隔离与物联边缘节点接入，具备ISO/IEC 27001认证，总工期不超过90日历天。"
	cat := ReqMandatoryClause
	req, err := f.d.CreateRequirement(ctx, tenant, f.task.ID, "reviewer", RequirementInput{
		Code:        &code,
		Category:    &cat,
		Title:       &title,
		Description: &desc,
		ProofRules:  &ProofRules{BidderOnly: true, ValidOnBidDeadline: true, RequiredDocs: []string{"certificate"}, ScopeKeywords: []string{"态势感知"}, MinCount: 1},
	})
	if err != nil {
		t.Fatalf("create requirement: %v", err)
	}
	if _, err := f.d.ConfirmRequirements(ctx, tenant, f.task.ID, "reviewer", 0); err != nil {
		t.Fatalf("confirm requirements: %v", err)
	}

	det, err := f.d.CreateOutline(ctx, tenant, f.task.ID, "editor", OutlineInput{
		Sections: []SectionInput{
			{Code: "1", Title: "智慧园区安全态势感知与零信任接入方案", Level: 1, RequirementIDs: []int64{req.ID}},
		},
	})
	if err != nil {
		t.Fatalf("create outline: %v", err)
	}
	if _, err := f.d.SubmitOutline(ctx, tenant, f.task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatal(err)
	}
	if det, err = f.d.ApproveOutline(ctx, tenant, f.task.ID, det.Outline.Version, "approver"); err != nil {
		t.Fatal(err)
	}
	secID := det.Sections[0].ID

	// 绑定并评估企业证据可采性
	if _, err := f.d.BindRequirementEvidence(ctx, tenant, f.task.ID, req.ID, EvidenceDomainCredential, cred.ID, "d3-test"); err != nil {
		t.Fatal(err)
	}
	run, err := f.d.EvaluateAdmissibility(ctx, tenant, f.task.ID, req.ID, store, "d3-test")
	if err != nil || run.Verdict != compliance.Admissible || len(run.Items) != 1 {
		t.Fatalf("admissibility run: %+v err=%v", run, err)
	}
	admItemID := run.Items[0].ID

	// 登记并确认承诺台账
	threshold := 90.0
	committed := 90.0
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, CommitmentInput{
		Code:                 "C-DUR-D3",
		RequirementID:        &req.ID,
		Metric:               MetricDuration,
		MetricLabel:          "总工期",
		Unit:                 "日历天",
		ThresholdCmp:         "<=",
		TenderThresholdRaw:   "不超过90日历天",
		TenderThresholdValue: &threshold,
		CommittedRaw:         "90日历天",
		CommittedValue:       &committed,
		Owner:                "项目经理",
		Rationale:            "并行敏捷交付计划",
	}, "planner"); err != nil {
		t.Fatal(err)
	}
	commitRow, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-DUR-D3", "planner")
	if err != nil {
		t.Fatal(err)
	}

	// 1. 创建不完整故事板（未覆盖 REQ-D3-01）-> 提交粉队评审应被拒绝
	sb, err := f.d.UpsertStoryboard(ctx, tenant, "writer-alice", UpsertStoryboardInput{
		TaskID:               f.task.ID,
		SectionID:            secID,
		ThemeSentence:        "采用云边协同的零信任微隔离与安全态势感知架构，保障智慧园区物联节点高可靠接入。",
		ScoringPoints:        []StoryboardScoringPoint{},
		AdmissibilityItemIDs: []int64{admItemID},
		CommitmentIDs:        []int64{commitRow.ID},
	})
	if err != nil {
		t.Fatalf("upsert storyboard: %v", err)
	}
	if _, err := f.d.SubmitStoryboard(ctx, tenant, f.task.ID, secID, "writer-alice"); !errors.Is(err, ErrStoryboardInvalid) {
		t.Fatalf("expected ErrStoryboardInvalid when requirement is uncovered, got %v", err)
	}

	// 2. 故事板处于 draft 状态时，派发写作或保存修订必须被拦截（ErrStoryboardNotApproved）
	if _, err := f.d.ClaimSectionWrite(ctx, tenant, f.task.ID, secID, "writer-alice", 30*time.Second); !errors.Is(err, ErrStoryboardNotApproved) {
		t.Fatalf("expected ErrStoryboardNotApproved on ClaimSectionWrite, got %v", err)
	}

	// 3. 补齐得分点后提交粉队评审 -> 作者自审必须被拦截（ErrPinkSelfApproval）-> 独立粉队评审人批准通过
	sb, err = f.d.UpsertStoryboard(ctx, tenant, "writer-alice", UpsertStoryboardInput{
		TaskID:        f.task.ID,
		SectionID:     secID,
		ThemeSentence: "采用云边协同的零信任微隔离与安全态势感知架构，保障智慧园区物联节点高可靠接入。",
		ScoringPoints: []StoryboardScoringPoint{
			{
				RequirementCode: "REQ-D3-01",
				ScoringPoint:    "智慧园区安全态势感知与零信任接入",
				OurApproach:     "部署流式安全态势感知底座与零信任微隔离网关",
				BuyerBenefit:    "实现物联边缘节点毫秒级异常阻断并确保90日历天内交付",
				EvidenceRefs:    []string{"CRED-ISO27001", "C-DUR-D3"},
			},
		},
		AdmissibilityItemIDs: []int64{admItemID},
		CommitmentIDs:        []int64{commitRow.ID},
		ChartPlans: []StoryboardChartPlan{
			{Title: "智慧园区零信任与态势感知总体架构图", ChartType: "architecture", Purpose: "展示边缘节点接入与微隔离控制面"},
		},
	})
	if err != nil {
		t.Fatalf("update storyboard: %v", err)
	}
	if _, err := f.d.SubmitStoryboard(ctx, tenant, f.task.ID, secID, "writer-alice"); err != nil {
		t.Fatalf("submit storyboard: %v", err)
	}
	if _, err := f.d.ApproveStoryboard(ctx, tenant, f.task.ID, secID, "self approve attempt", "writer-alice"); !errors.Is(err, ErrPinkSelfApproval) {
		t.Fatalf("expected ErrPinkSelfApproval, got %v", err)
	}
	sb, err = f.d.ApproveStoryboard(ctx, tenant, f.task.ID, secID, "论点清晰、证据与台账齐备", "pink-reviewer-bob")
	if err != nil || sb.Status != StoryboardPinkApproved {
		t.Fatalf("approve storyboard: %+v err=%v", sb, err)
	}

	// 4. 保存含未绑定事实断言的草稿 -> 必须被 ErrUngroundedClaim 拦截
	badBody := "响应要求 REQ-D3-01：我公司持有CMMI5级认证证书（证书编号：CMMI5-9999），并承担过某智慧城市项目合同金额5000万元。"
	if _, err := f.d.SaveSectionRevision(ctx, tenant, "writer-alice", SaveRevisionInput{
		TaskID:                f.task.ID,
		SectionID:             secID,
		BaseRevNo:             0,
		BasedOnOutlineVersion: det.Outline.Version,
		Source:                RevHumanEdit,
		ContentMD:             badBody,
	}); !errors.Is(err, ErrUngroundedClaim) {
		t.Fatalf("expected ErrUngroundedClaim on ungrounded fact, got %v", err)
	}

	// 5. 保存通用套话/低针对性草稿 -> 必须被 ErrWritingVerificationFailed 拦截
	boilerBody := "我公司高度重视本项目，本着科学管理、精心组织、质量第一、用户至上的原则，选派经验丰富的项目经理和技术骨干组建项目团队，确保按期高质量完成本项目全部建设内容。"
	if _, err := f.d.SaveSectionRevision(ctx, tenant, "writer-alice", SaveRevisionInput{
		TaskID:                f.task.ID,
		SectionID:             secID,
		BaseRevNo:             0,
		BasedOnOutlineVersion: det.Outline.Version,
		Source:                RevHumanEdit,
		ContentMD:             boilerBody,
	}); !errors.Is(err, ErrWritingVerificationFailed) {
		t.Fatalf("expected ErrWritingVerificationFailed on boilerplate, got %v", err)
	}

	// 6. 使用 [[EVID:...]] 与 [[COMMIT:...]] 结构化引用 token 保存章节 -> 自动展开并通过句子级证据绑定与写作核验
	goodTemplate := strings.Join([]string{
		"响应要求 REQ-D3-01：针对采购人智慧园区安全态势感知与零信任接入平台建设需求，本方案设计云边协同流式态势感知架构与零信任微隔离策略，全面支持物联边缘节点高可靠接入。",
		"[[EVID:CRED-ISO27001]]。",
		"[[COMMIT:C-DUR-D3]]。",
	}, "\n")
	saveRes, err := f.d.SaveSectionRevision(ctx, tenant, "writer-alice", SaveRevisionInput{
		TaskID:                f.task.ID,
		SectionID:             secID,
		BaseRevNo:             0,
		BasedOnOutlineVersion: det.Outline.Version,
		Source:                RevHumanEdit,
		ContentMD:             goodTemplate,
	})
	if err != nil || saveRes.Outcome != SaveApplied {
		t.Fatalf("save grounded revision: %v %+v", err, saveRes)
	}
	if !strings.Contains(saveRes.Revision.ContentMD, "[EVID:") || !strings.Contains(saveRes.Revision.ContentMD, "[COMMIT:") {
		t.Fatalf("expected expanded citation markers in saved revision, got:\n%s", saveRes.Revision.ContentMD)
	}

	wv, err := f.d.GetSectionWritingVerification(ctx, tenant, f.task.ID, secID)
	if err != nil {
		t.Fatalf("get writing verification: %v", err)
	}
	if wv.Verdict != compliance.WritingVerdictPassed || wv.UngroundedClaimCount != 0 || wv.FactBindingRate != 1.0 || wv.CommitmentBindingRate != 1.0 {
		t.Fatalf("unexpected writing verification metrics: %+v", wv)
	}

	// 7. 红队审批章节修订：作者自批必须被 ErrRedSelfApproval 拦截；独立红队审批人通过
	if _, err := f.d.ApproveSectionRevision(ctx, tenant, f.task.ID, secID, saveRes.Revision.RevNo, "writer-alice"); !errors.Is(err, ErrRedSelfApproval) {
		t.Fatalf("expected ErrRedSelfApproval, got %v", err)
	}
	if _, err := f.d.ApproveSectionRevision(ctx, tenant, f.task.ID, secID, saveRes.Revision.RevNo, "red-reviewer-carol"); err != nil {
		t.Fatalf("approve section revision by red team: %v", err)
	}

	// 跑通矩阵与一致性核验 -> ExportReadiness 应为 Ready=true
	if mres, err := f.d.VerifyComplianceMatrix(ctx, tenant, f.task.ID, 0, compliance.RuleVerifier{}, "verifier"); err != nil || mres.Verified != 1 {
		t.Fatalf("matrix verify: %+v err=%v", mres, err)
	}
	if _, err := f.d.RunConsistencyCheck(ctx, tenant, f.task.ID, "verifier"); err != nil {
		t.Fatalf("consistency check: %v", err)
	}
	rd, err := f.d.ExportReadiness(ctx, tenant, f.task.ID)
	if err != nil || !rd.Ready {
		t.Fatalf("expected ExportReadiness Ready=true, got ready=%v blockers=%+v err=%v", rd != nil && rd.Ready, blockerText(rd), err)
	}

	// 8. 上游承诺台账变更 -> 故事板与写作核验级联置为 stale，ExportReadiness 出现 storyboard_stale 与 writing_verification_stale
	newCommitted := 80.0
	if _, err := f.d.UpsertCommitment(ctx, tenant, f.task.ID, CommitmentInput{
		Code:                 "C-DUR-D3",
		RequirementID:        &req.ID,
		Metric:               MetricDuration,
		MetricLabel:          "总工期",
		Unit:                 "日历天",
		ThresholdCmp:         "<=",
		TenderThresholdRaw:   "不超过90日历天",
		TenderThresholdValue: &threshold,
		CommittedRaw:         "80日历天",
		CommittedValue:       &newCommitted,
		Owner:                "项目经理",
		Rationale:            "压缩工期",
	}, "planner"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ConfirmCommitment(ctx, tenant, f.task.ID, "C-DUR-D3", "planner"); err != nil {
		t.Fatal(err)
	}

	rd2, err := f.d.ExportReadiness(ctx, tenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rd2.Ready || !hasBlocker(rd2, BlockStoryboardStale) || !hasBlocker(rd2, BlockWritingVerificationStale) {
		t.Fatalf("expected storyboard_stale and writing_verification_stale blockers, got %+v", rd2.Blockers)
	}

	// 9. 重新提交并批准故事板 -> 重新渲染保存章节修订 -> 红队批准 -> 矩阵与一致性核验 -> 恢复 Ready=true
	if _, err := f.d.UpsertStoryboard(ctx, tenant, "writer-alice", UpsertStoryboardInput{
		TaskID:        f.task.ID,
		SectionID:     secID,
		ThemeSentence: "采用云边协同的零信任微隔离与安全态势感知架构，保障智慧园区物联节点高可靠接入。",
		ScoringPoints: []StoryboardScoringPoint{
			{
				RequirementCode: "REQ-D3-01",
				ScoringPoint:    "智慧园区安全态势感知与零信任接入",
				OurApproach:     "部署流式安全态势感知底座与零信任微隔离网关",
				BuyerBenefit:    "实现物联边缘节点毫秒级异常阻断并确保80日历天内交付",
				EvidenceRefs:    []string{"CRED-ISO27001", "C-DUR-D3"},
			},
		},
		AdmissibilityItemIDs: []int64{admItemID},
		CommitmentIDs:        []int64{commitRow.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.SubmitStoryboard(ctx, tenant, f.task.ID, secID, "writer-alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ApproveStoryboard(ctx, tenant, f.task.ID, secID, "工期更新后复核通过", "pink-reviewer-bob"); err != nil {
		t.Fatal(err)
	}
	curSec, _ := f.d.GetSection(ctx, tenant, f.task.ID, secID)
	saveRes2, err := f.d.SaveSectionRevision(ctx, tenant, "writer-alice", SaveRevisionInput{
		TaskID:                f.task.ID,
		SectionID:             secID,
		BaseRevNo:             curSec.Section.CurrentRevNo,
		BasedOnOutlineVersion: det.Outline.Version,
		Source:                RevHumanEdit,
		ContentMD:             goodTemplate,
	})
	if err != nil || saveRes2.Outcome != SaveApplied {
		t.Fatalf("re-save revision: %v %+v", err, saveRes2)
	}
	if _, err := f.d.ApproveSectionRevision(ctx, tenant, f.task.ID, secID, saveRes2.Revision.RevNo, "red-reviewer-carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.VerifyComplianceMatrix(ctx, tenant, f.task.ID, 0, compliance.RuleVerifier{}, "verifier"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.RunConsistencyCheck(ctx, tenant, f.task.ID, "verifier"); err != nil {
		t.Fatal(err)
	}
	rd3, err := f.d.ExportReadiness(ctx, tenant, f.task.ID)
	if err != nil || !rd3.Ready {
		t.Fatalf("expected recovery Ready=true, got ready=%v blockers=%+v err=%v", rd3 != nil && rd3.Ready, blockerText(rd3), err)
	}

	// 11. 旧规则版本下的"通过"不能沿用：规则升级后须重新核验才能导出
	if _, err := f.d.ExecContext(ctx, `UPDATE section_writing_verifications SET rule_version = 'd3-writing-v1'
		WHERE tenant_id = $1 AND section_id = $2`, tenant, secID); err != nil {
		t.Fatal(err)
	}
	rd4, err := f.d.ExportReadiness(ctx, tenant, f.task.ID)
	if err != nil || rd4.Ready || !hasBlocker(rd4, BlockWritingVerificationStale) {
		t.Fatalf("expected writing_verification_stale for outdated rule version, got ready=%v blockers=%+v err=%v", rd4 != nil && rd4.Ready, blockerText(rd4), err)
	}
	rewv, err := f.d.EvaluateSectionWritingVerification(ctx, tenant, f.task.ID, secID, "verifier")
	if err != nil || rewv.RuleVersion != compliance.WritingVerificationRuleVersion || rewv.Verdict != compliance.WritingVerdictPassed {
		t.Fatalf("re-verify under current rule: %+v err=%v", rewv, err)
	}
	rd5, err := f.d.ExportReadiness(ctx, tenant, f.task.ID)
	if err != nil || !rd5.Ready {
		t.Fatalf("expected Ready=true after re-verification, got ready=%v blockers=%+v err=%v", rd5 != nil && rd5.Ready, blockerText(rd5), err)
	}
}
