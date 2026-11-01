package main

// 确定性替身渲染器：从当前可采性明细与已确认承诺台账生成正文。不调用大模型，不代表写作质量；
// 台账值原样写入正文，供 db.SaveSectionRevision 的数值一致性门禁复核。

import (
	"fmt"
	"sort"
	"strings"

	"github.com/autobid/autobid-engine/db"
)

const genericOverall = "我方在充分理解本项目采购需求的基础上，采用分层解耦、云边协同的总体技术路线组织技术标编制。" +
	"总体方案覆盖感知接入、网络传输、平台服务与应用交互等层次，各层之间通过标准接口衔接，便于统一运维与持续扩展。"

const genericIndex = "本章按招标评分表顺序概述各评分点的响应策略。技术方案评分点从总体架构、技术路线与功能完整性组织论述；" +
	"类似项目业绩评分点按评分标准编排证明材料与索引。报价内容由报价文件单独响应。"

func extraSentence(req *db.RequirementItem) string {
	text := req.Title + " " + req.Description
	switch {
	case strings.Contains(text, "工期"):
		return "我方将按里程碑计划组织交付，确保按期完工。"
	case strings.Contains(text, "质保"):
		return "我方将建立备件保障与定期巡检机制，确保质保服务可追溯。"
	case strings.Contains(text, "可用性"):
		return "我方将部署监控告警与容量预警，保障业务平稳运行。"
	case strings.Contains(text, "响应"):
		return "我方将保持服务热线畅通并记录处置过程。"
	case strings.Contains(text, "修复"):
		return "我方将同步提交故障分析报告并跟踪闭环。"
	case strings.Contains(text, "团队"):
		return "我方将为项目组建立人员到岗与驻场管理台账。"
	case strings.Contains(text, "项目经理"):
		return "项目经理证书与社保记录原件备查，复印件编入投标文件。"
	case strings.Contains(text, "型号"), strings.Contains(text, "摄像机"):
		return "产品彩页与参数页原件备查，所投型号与招标要求一致。"
	case strings.Contains(text, "资质"):
		return "相关资质证书原件备查，复印件编入投标文件。"
	case strings.Contains(text, "业绩"):
		return "相关合同与验收材料原件备查。"
	case req.Category == db.ReqScoringItem:
		return "我方将按评分标准编排证明材料并编制响应索引。"
	default:
		return "我方将明确责任人与交付节点，形成可检查的过程记录，接受采购人监督。"
	}
}

// renderInput 是渲染一个章节所需的只读数据。
type renderInput struct {
	Section      *db.ProposalSection
	Requirements []*db.RequirementItem
	Runs         map[int64]*db.AdmissibilityRun
	Commitments  []*db.Commitment
}

// renderedSection 是渲染结果与引用。
type renderedSection struct {
	Content       string
	Admissibility []int64
	Commitments   []int64
}

// renderSection 生成章节正文：每个要求一段，包含要求原文、明确响应句、可采性引用与承诺值。
func renderSection(in renderInput) renderedSection {
	out := renderedSection{Admissibility: []int64{}, Commitments: []int64{}}
	if len(in.Requirements) == 0 {
		switch in.Section.Code {
		case "1":
			out.Content = genericOverall
		case "2":
			out.Content = genericIndex
		default:
			out.Content = genericOverall
		}
		return out
	}
	commitByReq := map[int64][]*db.Commitment{}
	for _, c := range in.Commitments {
		if c.RequirementID == nil || (c.Status != db.CommitmentConfirmed && c.Status != db.CommitmentSignedOff) {
			continue
		}
		commitByReq[*c.RequirementID] = append(commitByReq[*c.RequirementID], c)
	}
	reqs := append([]*db.RequirementItem{}, in.Requirements...)
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Code < reqs[j].Code })
	var b strings.Builder
	for i, req := range reqs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "响应要求 %s「%s」：\n%s\n\n", req.Code, req.Title, strings.TrimSpace(req.Description))
		b.WriteString("我方承诺完全满足上述要求。")
		b.WriteString(extraSentence(req))
		if run := in.Runs[req.ID]; run != nil {
			ids := []int64{}
			for _, it := range run.Items {
				if it.Verdict == "admissible" {
					ids = append(ids, it.ID)
					out.Admissibility = append(out.Admissibility, it.ID)
				}
			}
			if len(ids) > 0 {
				b.WriteString("\n\n上述响应所依据的证明材料已按招标文件要求通过可采性核验，原件及页码编入投标文件附件。")
			}
		}
		commits := commitByReq[req.ID]
		sort.Slice(commits, func(i, j int) bool { return commits[i].Code < commits[j].Code })
		for _, c := range commits {
			fmt.Fprintf(&b, "\n\n我方承诺%s %s。", c.MetricLabel, c.CommittedRaw)
			out.Commitments = append(out.Commitments, c.ID)
		}
	}
	out.Admissibility = uniqueIDs(out.Admissibility)
	out.Commitments = uniqueIDs(out.Commitments)
	out.Content = b.String()
	return out
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

// requirementRun 是要求当前运行；不存在返回 nil。
func requirementRun(runs map[int64]*db.AdmissibilityRun, reqID int64) *db.AdmissibilityRun {
	return runs[reqID]
}
