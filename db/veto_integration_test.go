package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 否决项核对：待核对与不通过都阻断导出；核对结论不改变清单 epoch；否决项内容变化后结论退回 pending。
func TestVetoStatusGateAndReset(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	p.approvedOutline(t)
	v, err := p.d.ListRequirements(ctx, DefaultTenant, p.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var vetoes []*RequirementItem
	var other *RequirementItem
	for _, it := range v.Items {
		switch {
		case it.Category == ReqVetoItem:
			vetoes = append(vetoes, it)
		case other == nil:
			other = it
		}
	}
	if len(vetoes) != 6 || other == nil {
		t.Fatalf("veto items = %d", len(vetoes))
	}
	r, err := p.d.ExportReadiness(ctx, DefaultTenant, p.task.ID)
	if err != nil || r.Ready || r.Counts[BlockVetoPending] != 6 {
		t.Fatalf("pending veto items must block export: %+v %v", r.Counts, err)
	}
	if _, err := p.d.SetVetoStatus(ctx, DefaultTenant, p.task.ID, vetoes[0].ID, "checker", VetoPassed, "  "); !errors.Is(err, ErrInvalidRequirement) {
		t.Fatalf("pass without a note: %v", err)
	}
	if _, err := p.d.SetVetoStatus(ctx, DefaultTenant, p.task.ID, other.ID, "checker", VetoPassed, "依据"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("veto status on a non-veto requirement: %v", err)
	}
	epoch := v.Manifest.Epoch
	for i, it := range vetoes {
		status := VetoPassed
		if i == 0 {
			status = VetoFailed
		}
		got, err := p.d.SetVetoStatus(ctx, DefaultTenant, p.task.ID, it.ID, "checker", status, "依据：资格证明文件第 3 页")
		if err != nil || got.VetoStatus != status || got.VetoCheckedBy != "checker" || got.VetoCheckedAt == nil {
			t.Fatalf("SetVetoStatus: %+v %v", got, err)
		}
	}
	if m, _ := p.d.RequirementManifest(ctx, DefaultTenant, p.task.ID); m.Epoch != epoch || m.Status != ManifestConfirmed {
		t.Fatalf("a veto check must not change the manifest: %+v", m)
	}
	r, _ = p.d.ExportReadiness(ctx, DefaultTenant, p.task.ID)
	if r.Counts[BlockVetoPending] != 0 || r.Counts[BlockVetoFailed] != 1 {
		t.Fatalf("failed veto item must block export: %+v", r.Counts)
	}
	upd, err := p.d.UpdateRequirement(ctx, DefaultTenant, p.task.ID, vetoes[1].ID, "reviewer", RequirementInput{Description: strp(vetoes[1].Description + "（补充）")})
	if err != nil || upd.VetoStatus != VetoPending || upd.VetoCheckedBy != "" || upd.VetoNote != "" {
		t.Fatalf("changed veto item keeps its old check: %+v %v", upd, err)
	}
}

// 投标范围：后发布的更正公告决定截止时间；人工指定优先且不被重新提取覆盖；不投的分包退出技术标范围并使清单 epoch 递增。
func TestBidScopeLotsAndDeadlinePrecedence(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	cst := time.FixedZone("CST", 8*3600)
	corrected, original := time.Date(2026, 8, 10, 0, 0, 0, 0, cst), time.Date(2026, 8, 1, 0, 0, 0, 0, cst)
	// 更正公告先上传（文档 ID 较小），但发布时间较晚，仍以更正为准。
	if _, _, err := p.d.InsertTenderDocument(ctx, &TenderDocument{TaskID: p.task.ID, TenderNo: uniq("GZ"), Title: "园区安防项目更正公告",
		RawText: uniq("c"), PublishedAt: &corrected}, []DocumentChunk{{Seq: 0, Content: "二、更正信息\n投标截止时间：2026年8月31日北京时间9时30分。"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.d.InsertTenderDocument(ctx, &TenderDocument{TaskID: p.task.ID, TenderNo: uniq("ZB"), Title: "招标文件",
		RawText: uniq("t"), PublishedAt: &original}, []DocumentChunk{{Seq: 0, Content: "（七）投标截止时间：2026年8月26日北京时间10时00分；"}}); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 31, 9, 30, 0, 0, cst)
	res, err := p.d.ExtractRequirements(ctx, DefaultTenant, p.task.ID, "reviewer")
	if err != nil || res.BidDeadline == nil || !res.BidDeadline.Equal(want) {
		t.Fatalf("extract deadline: %+v %v", res, err)
	}
	m, _ := p.d.RequirementManifest(ctx, DefaultTenant, p.task.ID)
	if m.BidDeadline == nil || !m.BidDeadline.Equal(want) || !strings.Contains(string(m.BidDeadlineSource), `"correction"`) {
		t.Fatalf("manifest deadline: %v %s", m.BidDeadline, m.BidDeadlineSource)
	}
	manual := time.Date(2026, 9, 1, 10, 0, 0, 0, cst)
	if m, err = p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "manager", BidScopeInput{Deadline: strp("2026-09-01 10:00")}); err != nil || !m.BidDeadline.Equal(manual) {
		t.Fatalf("manual deadline: %+v %v", m, err)
	}
	if _, err := p.d.ExtractRequirements(ctx, DefaultTenant, p.task.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if m, _ = p.d.RequirementManifest(ctx, DefaultTenant, p.task.ID); !m.BidDeadline.Equal(manual) {
		t.Fatalf("re-extraction overwrote the manual deadline: %v", m.BidDeadline)
	}
	if _, err := p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "manager", BidScopeInput{Deadline: strp("")}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.d.ExtractRequirements(ctx, DefaultTenant, p.task.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if m, _ = p.d.RequirementManifest(ctx, DefaultTenant, p.task.ID); m.BidDeadline == nil || !m.BidDeadline.Equal(want) {
		t.Fatalf("cleared manual deadline not re-extracted: %v", m.BidDeadline)
	}

	tech := ScoreGroupTechnical
	weight := 10.0
	for code, lot := range map[string]string{"L1": "包1", "L2": "2"} {
		if _, err := p.d.CreateRequirement(ctx, DefaultTenant, p.task.ID, "reviewer", RequirementInput{Code: strp(code), Category: strp(ReqScoringItem),
			Title: strp("分包评分项 " + code), ScoreWeight: &weight, ScoreGroup: &tech, Lot: strp(lot)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "manager", BidScopeInput{Lots: &[]string{"9"}}); !errors.Is(err, ErrInvalidRequirement) {
		t.Fatalf("unknown lot accepted: %v", err)
	}
	m, _ = p.d.RequirementManifest(ctx, DefaultTenant, p.task.ID)
	epoch := m.Epoch
	if m, err = p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "manager", BidScopeInput{Lots: &[]string{"第一包"}}); err != nil ||
		strings.Join(m.BidLots, ",") != "1" || m.Epoch <= epoch {
		t.Fatalf("bid lots: %+v %v", m, err)
	}
	v, err := p.d.ListRequirements(ctx, DefaultTenant, p.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range v.Items {
		if (it.Code == "L2") != it.OutOfBid || (it.Code == "L2" && it.InTechnicalScope()) {
			t.Fatalf("out_of_bid: %+v", it)
		}
	}
	if v.Stats.OutOfBid != 1 || strings.Join(v.Stats.Lots, ",") != "1,2" || containsString(v.Stats.Unmapped, "L2") {
		t.Fatalf("stats = %+v", v.Stats)
	}
	if m, err = p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "manager", BidScopeInput{Lots: &[]string{}}); err != nil || len(m.BidLots) != 0 {
		t.Fatalf("clear lots: %+v %v", m, err)
	}
	if v, _ = p.d.ListRequirements(ctx, DefaultTenant, p.task.ID); v.Stats.OutOfBid != 0 {
		t.Fatalf("lots cleared but items still out of bid: %+v", v.Stats)
	}
}

// D1：更正公告对旧招标文件评分项与 ★ 强制条款的逐条覆盖、大纲与已定稿章节失敏降级、响应矩阵失效以及派生大纲自动替换新要求 ID。
func TestAmendmentSupersedesRequirementAndInvalidatesOutline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cst := time.FixedZone("CST", 8*3600)
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, cst)
	t1 := time.Date(2026, 8, 12, 0, 0, 0, 0, cst)

	scoreTbl, _ := json.Marshal([]map[string]any{{
		"rows": 4, "cols": 4,
		"grid": [][]string{
			{"序号", "评审因素", "分值", "评分标准"},
			{"1", "核心平台整体架构设计方案", "40", "优：双活容灾架构设计完整得[30,40]分；良：得[20,30)分；差：得[0,20)分"},
			{"2", "同类政务云项目业绩", "30", "每提供1个同类政务云项目业绩得10分，最高得30分；须提供合同首页、签字盖章页及竣工验收报告"},
			{"3", "投标报价", "30", "投标报价得分=(评标基准价/投标报价)×30×100%"},
		},
	}})
	if _, _, err := f.d.InsertTenderDocument(ctx, &TenderDocument{
		TaskID: f.task.ID, TenderNo: uniq("ZB-MAIN"), Title: "智慧政务云平台招标文件", DocRole: DocRoleTender,
		RawText: uniq("main"), PublishedAt: &t0,
	}, []DocumentChunk{
		{Seq: 0, SectionCode: "1", SectionTitle: "投标邀请", Content: "（七）投标截止时间：2026年8月26日北京时间10时00分；"},
		{Seq: 1, SectionCode: "3.1", SectionTitle: "采购需求", Content: "★3.1 核心平台整体架构设计：平台须支持双活容灾架构，RTO≤5分钟，服务可用性≥99.9%。", IsMandatory: true},
		{Seq: 2, SectionCode: "4.2", SectionTitle: "评标办法及评分标准", Content: "评标办法及评分标准（满分100分）", TableCount: 1, Tables: scoreTbl},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.d.ExtractRequirements(ctx, DefaultTenant, f.task.ID, "reviewer"); err != nil {
		t.Fatalf("initial extract: %v", err)
	}
	v1, err := f.d.ListRequirements(ctx, DefaultTenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var archScore, perfScore, starClause *RequirementItem
	for _, it := range v1.Items {
		switch {
		case it.Category == ReqScoringItem && strings.Contains(it.Title, "核心平台整体架构设计方案"):
			archScore = it
		case it.Category == ReqScoringItem && strings.Contains(it.Title, "同类政务云项目业绩"):
			perfScore = it
		case it.Category == ReqMandatoryClause && strings.Contains(it.Description, "双活容灾架构"):
			starClause = it
		}
	}
	if archScore == nil || perfScore == nil || starClause == nil {
		t.Fatalf("missing extracted items: arch=%+v perf=%+v star=%+v", archScore, perfScore, starClause)
	}
	if archScore.EvalKind != EvalSubjective || len(archScore.ScoringTiers) < 3 {
		t.Fatalf("archScore tiers/evalKind not decomposed: %+v", archScore)
	}
	if perfScore.EvalKind != EvalObjective || perfScore.ProofRules.IsZero() || !perfScore.ProofRules.NeedAcceptance {
		t.Fatalf("perfScore proof rules not decomposed: %+v", perfScore)
	}
	if len(archScore.LinkedRequirementIDs) == 0 || archScore.LinkedRequirementIDs[0] != starClause.ID {
		t.Fatalf("archScore should link to procurement demand starClause (%d): got %v", starClause.ID, archScore.LinkedRequirementIDs)
	}

	// 确认清单 -> 生成并批准大纲 v1 -> 为架构章节写入并定稿正文 -> 运行响应矩阵核验。
	if _, err := f.d.ConfirmRequirements(ctx, DefaultTenant, f.task.ID, "approver", 0); err != nil {
		t.Fatalf("ConfirmRequirements v1: %v", err)
	}
	od1, err := f.d.CreateOutline(ctx, DefaultTenant, f.task.ID, "planner", OutlineInput{})
	if err != nil {
		t.Fatalf("CreateOutline v1: %v", err)
	}
	if _, err := f.d.SubmitOutline(ctx, DefaultTenant, f.task.ID, od1.Outline.Version, "planner"); err != nil {
		t.Fatalf("SubmitOutline v1: %v", err)
	}
	od1, err = f.d.ApproveOutline(ctx, DefaultTenant, f.task.ID, od1.Outline.Version, "approver")
	if err != nil {
		t.Fatalf("ApproveOutline v1: %v", err)
	}
	var archSec *ProposalSection
	for i := range od1.Sections {
		for _, rid := range od1.Sections[i].RequirementIDs {
			if rid == archScore.ID {
				archSec = od1.Sections[i]
				break
			}
		}
	}
	if archSec == nil {
		t.Fatalf("no section mapped to archScore %d", archScore.ID)
	}
	saved, err := f.d.SaveSectionRevision(ctx, DefaultTenant, "writer", SaveRevisionInput{
		TaskID: f.task.ID, SectionID: archSec.ID, BaseRevNo: 0, BasedOnOutlineVersion: od1.Outline.Version, Source: RevHumanEdit,
		ContentMD: fmt.Sprintf("## 核心平台整体架构设计方案\n\n<!-- req:%s -->\n本方案采用双活容灾架构设计，核心数据库实时同步切换，RTO≤3分钟，服务可用性达到99.99%%，配备4台高性能应用服务器与负载均衡集群。\n\n<!-- req:%s -->\n完全满足★3.1双活容灾架构要求，RTO≤3分钟（优于招标要求的5分钟），服务可用性≥99.99%%，提供双活仲裁节点与自动故障切换机制。", archScore.Code, starClause.Code),
		SummaryNote: "双活容灾架构定稿",
	})
	if err != nil || saved.Outcome != SaveApplied {
		t.Fatalf("SaveSectionRevision: %+v %v", saved, err)
	}
	if _, err := f.d.ApproveSectionRevision(ctx, DefaultTenant, f.task.ID, archSec.ID, saved.Revision.RevNo, "approver"); err != nil {
		t.Fatalf("ApproveSectionRevision: %v", err)
	}
	if _, err := f.d.VerifyComplianceMatrix(ctx, DefaultTenant, f.task.ID, 0, nil, "checker"); err != nil {
		t.Fatalf("VerifyComplianceMatrix: %v", err)
	}

	// 上传后发更正公告：延长投标截止时间，并将评分项与 ★3.1 强制条款升级为「两地三中心容灾架构」。
	corrTbl, _ := json.Marshal([]map[string]any{{
		"rows": 2, "cols": 4,
		"grid": [][]string{
			{"序号", "评审因素", "分值", "评分标准"},
			{"1", "核心平台整体架构设计方案", "40", "优：两地三中心容灾架构设计完整得[30,40]分；良：得[20,30)分；差：得[0,20)分"},
		},
	}})
	if _, _, err := f.d.InsertTenderDocument(ctx, &TenderDocument{
		TaskID: f.task.ID, TenderNo: uniq("GZ-01"), Title: "智慧政务云平台更正公告", DocRole: DocRoleCorrection,
		RawText: uniq("corr"), PublishedAt: &t1,
	}, []DocumentChunk{
		{Seq: 0, SectionCode: "1", SectionTitle: "更正事项", Content: "一、投标截止时间：2026年9月10日北京时间09时30分；\n二、原招标文件第三章“★3.1 核心平台整体架构设计”更正为：\n★3.1 核心平台整体架构设计：平台须支持两地三中心容灾架构，RTO≤1分钟，RPO=0，服务可用性≥99.99%。", IsMandatory: true},
		{Seq: 1, SectionCode: "2", SectionTitle: "评分标准更正", Content: "评标办法及评分标准更正如下：", TableCount: 1, Tables: corrTbl},
	}); err != nil {
		t.Fatal(err)
	}

	res2, err := f.d.ExtractRequirements(ctx, DefaultTenant, f.task.ID, "reviewer")
	if err != nil {
		t.Fatalf("amendment extract: %v", err)
	}
	if res2.Superseded < 2 {
		t.Fatalf("expected at least 2 superseded items (scoring + mandatory clause), got %+v", res2)
	}
	v2, err := f.d.ListRequirements(ctx, DefaultTenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Manifest.Status != ManifestStale || v2.Stats.Superseded < 2 {
		t.Fatalf("manifest should be stale and stats.superseded >= 2: manifest=%+v stats=%+v", v2.Manifest, v2.Stats)
	}
	// 评分汇总只统计有效项，不重复累加已被覆盖的 40 分旧项（总分仍为 100 分）。
	if len(v2.Scoring) != 1 || v2.Scoring[0].Total != 100 {
		t.Fatalf("scoring summary should remain 100 (excluding superseded item), got %+v", v2.Scoring)
	}
	byID := map[int64]*RequirementItem{}
	for _, it := range v2.Items {
		byID[it.ID] = it
	}
	oldArch := byID[archScore.ID]
	oldStar := byID[starClause.ID]
	if oldArch == nil || oldArch.ReviewStatus != ReviewStale || oldArch.SupersededByID == nil || oldArch.SupersededByCode == "" {
		t.Fatalf("oldArch not marked stale/superseded: %+v", oldArch)
	}
	if oldStar == nil || oldStar.ReviewStatus != ReviewStale || oldStar.SupersededByID == nil {
		t.Fatalf("oldStar not marked stale/superseded: %+v", oldStar)
	}
	newArch := byID[*oldArch.SupersededByID]
	if newArch == nil || !strings.Contains(newArch.Description, "两地三中心") {
		t.Fatalf("newArch replacement invalid: %+v", newArch)
	}

	// 验证大纲 v1 自动降为 superseded，章节定稿被撤销，响应矩阵行变为 stale。
	od1After, err := f.d.GetOutline(ctx, DefaultTenant, f.task.ID, od1.Outline.Version)
	if err != nil {
		t.Fatal(err)
	}
	if od1After.Outline.Status != OutlineSuperseded {
		t.Fatalf("outline v1 should be superseded, got %s", od1After.Outline.Status)
	}
	for _, s := range od1After.Sections {
		if s.ID == archSec.ID && s.ApprovedRevNo != nil {
			t.Fatalf("section approved_rev_no should be cleared when requirement superseded: %+v", s)
		}
	}
	mv, err := f.d.ListComplianceMatrix(ctx, DefaultTenant, f.task.ID, MatrixFilter{})
	if err != nil {
		t.Fatal(err)
	}
	staleMatrix := false
	for _, r := range mv.Rows {
		if r.RequirementID == archScore.ID && r.VerifyStatus == "stale" {
			staleMatrix = true
		}
	}
	if !staleMatrix {
		t.Fatalf("compliance matrix row for superseded requirement should be stale: %+v", mv.Rows)
	}

	// 确认新清单并检查自动派生的 v2 草案：afterRequirementChangeTx -> deriveOutlineTx 已自动将旧要求 ID 替换为新要求 ID。
	for _, it := range v2.Items {
		if it.ReviewStatus == ReviewNeedsRevision {
			if _, err := f.d.UpdateRequirement(ctx, DefaultTenant, f.task.ID, it.ID, "reviewer", RequirementInput{ReviewStatus: strp(ReviewConfirmed)}); err != nil {
				t.Fatalf("confirm orphan item %s: %v", it.Code, err)
			}
		}
	}
	v2, _ = f.d.ListRequirements(ctx, DefaultTenant, f.task.ID)
	if _, err := f.d.ConfirmRequirements(ctx, DefaultTenant, f.task.ID, "approver", v2.Manifest.Epoch); err != nil {
		t.Fatalf("ConfirmRequirements v2: %v", err)
	}
	od2, err := f.d.GetOutline(ctx, DefaultTenant, f.task.ID, od1.Outline.Version+1)
	if err != nil {
		t.Fatalf("GetOutline auto-derived v2: %v", err)
	}
	hasNewArch := false
	for _, s := range od2.Sections {
		for _, rid := range s.RequirementIDs {
			if rid == archScore.ID {
				t.Fatalf("derived outline v2 still references superseded requirement ID %d", rid)
			}
			if rid == newArch.ID {
				hasNewArch = true
			}
		}
	}
	if !hasNewArch {
		t.Fatalf("derived outline v2 did not auto-swap to replacement requirement ID %d", newArch.ID)
	}
	if _, err := f.d.SubmitOutline(ctx, DefaultTenant, f.task.ID, od2.Outline.Version, "planner"); err != nil {
		t.Fatalf("SubmitOutline v2 should succeed after auto-swap: %v", err)
	}
}

// D1：Bid / No-Bid 决策门禁与证书按投标截止日（含澄清顺延）核验有效期。
func TestBidDecisionAndCredentialValidityOnBidDeadline(t *testing.T) {
	p := newProposalFixture(t)
	ctx := context.Background()
	p.approvedOutline(t)

	// 1. 放弃投标须填写原因，且阻断导出（BlockNoBid）。
	noBid := BidDecisionNoBid
	if _, err := p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "director", BidScopeInput{Decision: &noBid}); !errors.Is(err, ErrInvalidRequirement) {
		t.Fatalf("no_bid without note must fail: %v", err)
	}
	if m, err := p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "director", BidScopeInput{
		Decision: &noBid, DecisionNote: strp("核心特定资格不满足，决定放弃投标"),
	}); err != nil || m.BidDecision != BidDecisionNoBid || m.BidDecidedBy != "director" || m.BidDecidedAt == nil {
		t.Fatalf("SetBidScope no_bid: %+v %v", m, err)
	}
	r, err := p.d.ExportReadiness(ctx, DefaultTenant, p.task.ID)
	if err != nil || r.Counts[BlockNoBid] != 1 {
		t.Fatalf("no_bid must block export with BlockNoBid: %+v %v", r.Counts, err)
	}

	// 2. 存在不通过的否决项时，禁止将决策设为 bid。
	v, _ := p.d.ListRequirements(ctx, DefaultTenant, p.task.ID)
	var firstVeto *RequirementItem
	for _, it := range v.Items {
		if it.Category == ReqVetoItem {
			firstVeto = it
			break
		}
	}
	if _, err := p.d.SetVetoStatus(ctx, DefaultTenant, p.task.ID, firstVeto.ID, "checker", VetoFailed, "资质不满足"); err != nil {
		t.Fatal(err)
	}
	bid := BidDecisionBid
	if _, err := p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "director", BidScopeInput{Decision: &bid, DecisionNote: strp("强行投标")}); !errors.Is(err, ErrInvalidRequirement) {
		t.Fatalf("bid decision with failed veto must fail: %v", err)
	}
	if _, err := p.d.SetVetoStatus(ctx, DefaultTenant, p.task.ID, firstVeto.ID, "checker", VetoPassed, "已补充资质证明"); err != nil {
		t.Fatal(err)
	}
	if m, err := p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "director", BidScopeInput{Decision: &bid, DecisionNote: strp("资格审查通过，决定投标")}); err != nil || m.BidDecision != BidDecisionBid {
		t.Fatalf("SetBidScope bid: %+v %v", m, err)
	}
	if r, _ = p.d.ExportReadiness(ctx, DefaultTenant, p.task.ID); r.Counts[BlockNoBid] != 0 {
		t.Fatalf("BlockNoBid should be cleared after switching to bid: %+v", r.Counts)
	}

	// 3. 证书有效期默认按项目投标截止日（含顺延）核验。
	credTitle := uniq("ISO27001认证")
	validUntil := "2026-09-01"
	if _, _, err := p.d.UpsertCompanyCredential(ctx, &CompanyCredential{
		Category: "certificate", Title: credTitle, ValidUntil: &validUntil,
	}); err != nil {
		t.Fatal(err)
	}
	// 截止日为 2026-08-26 时，2026-09-01 到期的证书有效。
	if _, err := p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "director", BidScopeInput{Deadline: strp("2026-08-26 10:00")}); err != nil {
		t.Fatal(err)
	}
	creds, err := p.d.ListCompanyCredentials(ctx, CredentialFilter{Terms: []string{credTitle}, TaskID: p.task.ID})
	if err != nil || len(creds) != 1 {
		t.Fatalf("credential valid on 2026-08-26 should be returned: %+v %v", creds, err)
	}
	// 澄清公告顺延截止日到 2026-09-10 后，2026-09-01 到期的证书在开标前已过期，自动被过滤。
	if _, err := p.d.SetBidScope(ctx, DefaultTenant, p.task.ID, "director", BidScopeInput{Deadline: strp("2026-09-10 10:00")}); err != nil {
		t.Fatal(err)
	}
	credsAfter, err := p.d.ListCompanyCredentials(ctx, CredentialFilter{Terms: []string{credTitle}, TaskID: p.task.ID})
	if err != nil || len(credsAfter) != 0 {
		t.Fatalf("credential expiring before extended bid_deadline (2026-09-10) must be filtered out: %+v %v", credsAfter, err)
	}
}
