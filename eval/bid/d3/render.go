package main

// D3 故事板构造器与受控引用标记模板渲染器：
// 1. buildStoryboardInput 为章节生成包含核心论点、得分点做法/买方收益与证据/承诺白名单的故事板；
// 2. buildGroundedTemplate 为章节生成含 [[EVID:<id>]] 与 [[COMMIT:<code>]] 的受控引用模板，
//    交由 db.RenderSectionGroundedDraft 与 db.SaveSectionRevision 确定性展开并执行句子级断言核验。

import (
	"fmt"
	"sort"
	"strings"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

const genericOverall = "针对智慧园区安防系统采购项目，本章围绕云边协同架构、视频监控接入、门禁联动与停车诱导系统集成阐述总体技术方案。" +
	"整体架构覆盖前端感知接入层、边缘智能分析层、安防综合管理平台层与统一运维监管层，各层通过标准协议互联互通。"

const genericIndex = "针对智慧园区安防系统采购项目评分标准，本章逐项梳理技术方案评分点、商务资信评分点与服务保障评分点的响应路径及章节索引。" +
	"各评分点围绕云边协同架构、视频监控接入、门禁联动与停车诱导集成逐条建立评审导航，便于评标委员会按章节快速核查。"

func extraNarrativeSentence(req *db.RequirementItem) string {
	text := req.Title + " " + req.Description
	switch {
	case strings.Contains(text, "工期"):
		return "智慧园区安防系统采购项目按设备到货、前端点位施工、平台联调与试运行四个里程碑倒排进度计划。"
	case strings.Contains(text, "质保"):
		return "智慧园区安防系统采购项目配置本地备品备件周转库与季度预防性巡检机制，保障视频监控与门禁子系统稳定运行。"
	case strings.Contains(text, "可用性"):
		return "智慧园区安防系统平台采用双机热备集群、负载均衡与自动故障切换架构，消除单点故障隐患。"
	case strings.Contains(text, "响应"):
		return "智慧园区安防系统运维中心开通全天候故障受理专线并配置驻场与机动工程师梯队。"
	case strings.Contains(text, "修复"):
		return "智慧园区安防系统建立分级故障处置规程与整机备件先行替换机制，每次故障修复后提交根因分析报告。"
	case strings.Contains(text, "团队"):
		return "智慧园区安防系统实施团队按深化设计、前端施工、平台集成、测试验收与运维移交五个专业组配置人员。"
	case strings.Contains(text, "项目经理"):
		return "智慧园区安防系统项目经理驻场统筹深化设计、施工质量控制、安全文明施工与竣工归档。"
	case strings.Contains(text, "型号"), strings.Contains(text, "摄像机"):
		return "智慧园区安防系统前端感知层统一规划高清视频采集点位与标准协议接入网关，保障全天候画面清晰稳定。"
	case strings.Contains(text, "资质"):
		return "智慧园区安防系统设计施工严格执行安全技术防范行业标准，企业资格证明原件首页及有效期页已编入资格审查索引。"
	case strings.Contains(text, "业绩"):
		return "智慧园区安防系统实施方案充分复用既有安防视频监控、门禁与停车诱导集成工程的成熟交付经验。"
	case req.Category == db.ReqScoringItem:
		return "智慧园区安防系统技术方案与商务资信得分点均按评标办法逐条建立评审索引与证明材料映射。"
	default:
		return "智慧园区安防系统各实施阶段均明确责任人、交付物与核验节点，全面满足招标文件要求。"
	}
}

type renderInput struct {
	Section      *db.ProposalSection
	Requirements []*db.RequirementItem
	Runs         map[int64]*db.AdmissibilityRun
	Commitments  []*db.Commitment
}

type sectionPlan struct {
	StoryboardIn  db.UpsertStoryboardInput
	TemplateMD    string
	Admissibility []int64
	Commitments   []int64
}

func buildSectionPlan(in renderInput) sectionPlan {
	plan := sectionPlan{Admissibility: []int64{}, Commitments: []int64{}}
	commitByReq := map[int64][]*db.Commitment{}
	for _, c := range in.Commitments {
		if c.RequirementID == nil || (c.Status != db.CommitmentConfirmed && c.Status != db.CommitmentSignedOff) {
			continue
		}
		commitByReq[*c.RequirementID] = append(commitByReq[*c.RequirementID], c)
	}
	reqs := append([]*db.RequirementItem{}, in.Requirements...)
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Code < reqs[j].Code })

	var points []db.StoryboardScoringPoint
	var b strings.Builder

	if len(reqs) == 0 {
		theme := fmt.Sprintf("围绕智慧园区安防系统采购项目构建分层解耦、云边协同的「%s」总体架构与评分响应索引", in.Section.Title)
		points = append(points, db.StoryboardScoringPoint{
			RequirementCode: "OVERVIEW",
			ScoringPoint:    in.Section.Title,
			OurApproach:     "采用感知接入、边缘分析、安防平台与统一运维四层解耦架构组织技术方案",
			BuyerBenefit:    "确保智慧园区视频监控、门禁与停车诱导子系统互联互通、平滑演进且易于评审核对",
		})
		if in.Section.Code == "2" {
			b.WriteString(genericIndex)
		} else {
			b.WriteString(genericOverall)
		}
		plan.StoryboardIn = db.UpsertStoryboardInput{
			ThemeSentence:        theme,
			ScoringPoints:        points,
			AdmissibilityItemIDs: []int64{},
			CommitmentIDs:        []int64{},
		}
		plan.TemplateMD = b.String()
		return plan
	}

	for i, req := range reqs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		var refs []string
		var reqAdmIDs []int64
		if run := in.Runs[req.ID]; run != nil {
			for _, it := range run.Items {
				if it.Verdict == compliance.Admissible {
					reqAdmIDs = append(reqAdmIDs, it.ID)
					plan.Admissibility = append(plan.Admissibility, it.ID)
					refs = append(refs, fmt.Sprintf("EVID:%d", it.ID))
				}
			}
		}
		commits := commitByReq[req.ID]
		sort.Slice(commits, func(i, j int) bool { return commits[i].Code < commits[j].Code })
		for _, c := range commits {
			plan.Commitments = append(plan.Commitments, c.ID)
			refs = append(refs, fmt.Sprintf("COMMIT:%s", c.Code))
		}

		points = append(points, db.StoryboardScoringPoint{
			RequirementCode: req.Code,
			ScoringPoint:    req.Title,
			OurApproach:     extraNarrativeSentence(req),
			BuyerBenefit:    fmt.Sprintf("全面满足「%s」要求，实现指标可量化、原件可核查、交付可追溯", req.Title),
			EvidenceRefs:    refs,
		})

		descSingleSentence := strings.NewReplacer("。", "，", "；", "，", ";", "，", "！", "，", "？", "，", "\n", "，").Replace(strings.TrimSpace(req.Description))
		descSingleSentence = strings.TrimRight(descSingleSentence, "，, ")
		fmt.Fprintf(&b, "响应要求 %s「%s」（招标文件要求：%s）。\n", req.Code, req.Title, descSingleSentence)
		b.WriteString("我方承诺完全满足上述要求。")
		b.WriteString(extraNarrativeSentence(req))
		for _, id := range reqAdmIDs {
			fmt.Fprintf(&b, "\n[[EVID:%d]]。", id)
		}
		for _, c := range commits {
			fmt.Fprintf(&b, "\n[[COMMIT:%s]]。", c.Code)
		}
	}

	plan.Admissibility = uniqueIDs(plan.Admissibility)
	plan.Commitments = uniqueIDs(plan.Commitments)
	plan.StoryboardIn = db.UpsertStoryboardInput{
		ThemeSentence:        fmt.Sprintf("针对智慧园区安防系统采购项目「%s」，以经核验企业证据与锁定承诺台账逐项闭环响应全部要求", in.Section.Title),
		ScoringPoints:        points,
		AdmissibilityItemIDs: plan.Admissibility,
		CommitmentIDs:        plan.Commitments,
	}
	plan.TemplateMD = b.String()
	return plan
}

func uniqueIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func sectionSummary(sec *db.ProposalSection) string {
	if len(sec.RequirementIDs) == 0 {
		return fmt.Sprintf("本章「%s」说明总体技术路线与实施组织安排，术语与架构名称供后续章节沿用。", sec.Title)
	}
	return fmt.Sprintf("本章「%s」逐条响应已映射的招标要求，响应口径与承诺边界以本章正文为准。", sec.Title)
}
