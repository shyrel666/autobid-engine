package main

// 确定性「替身写作」：不调用大模型，按映射要求生成可被规则核验器与一致性核验接受的正文。
// 只证明流程与门禁，不代表写作质量；不得把这里的文本当成真实投标正文。

import (
	"fmt"
	"strings"

	"github.com/autobid/autobid-engine/db"
)

const genericOverall = "我方在充分理解本项目采购需求的基础上，采用分层解耦、云边协同的总体技术路线组织技术标编制。" +
	"总体方案覆盖感知接入、网络传输、平台服务与应用交互等层次，各层之间通过标准接口衔接，便于统一运维与持续扩展。\n\n" +
	"在实施组织上，我方将按设计、供货、安装、调试、验收等阶段推进，并以统一的配置基线和文档基线保证交付质量；" +
	"对关键过程形成可追溯记录，配合采购人完成各阶段确认。"

const genericIndex = "本章按招标评分表顺序概述各评分点的响应策略。技术方案评分点从总体架构、技术路线与功能完整性组织论述；" +
	"类似项目业绩评分点按评分标准编排证明材料与索引。报价内容由报价文件单独响应。"

// extraSentence 为每条要求补充一句不复述要求的实质内容；刻意不写任何数值与否定表述，避免制造一致性噪声。
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
	case strings.Contains(text, "驻场"):
		return "相关认证证书原件备查，复印件编入投标文件。"
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

// stubSectionContent 生成一个章节的替身正文：每个要求一段，包含要求标题、描述原文、明确响应句与额外实质内容。
func stubSectionContent(sec *db.ProposalSection, reqs []*db.RequirementItem) string {
	if len(reqs) == 0 {
		switch sec.Code {
		case "1":
			return genericOverall
		case "2":
			return genericIndex
		}
		return genericOverall
	}
	var b strings.Builder
	for i, req := range reqs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		desc := strings.TrimSpace(req.Description)
		fmt.Fprintf(&b, "响应要求 %s「%s」：\n%s\n\n", req.Code, req.Title, desc)
		b.WriteString("我方承诺完全满足上述要求。")
		b.WriteString(extraSentence(req))
	}
	return b.String()
}

// stubDurationSection 生成 G09 专用章节正文：描述原文 + 指定的一个总工期承诺值。
func stubDurationSection(req *db.RequirementItem, days int) string {
	return fmt.Sprintf("响应要求 %s「%s」：\n%s\n\n我方承诺总工期 %d 天，并按实施计划组织交付。",
		req.Code, req.Title, strings.TrimSpace(req.Description), days)
}

// sectionSummary 生成固定衔接摘要（不参与一致性抽取）。
func sectionSummary(sec *db.ProposalSection) string {
	if len(sec.RequirementIDs) == 0 {
		return fmt.Sprintf("本章「%s」说明总体技术路线与实施组织安排，术语与架构名称供后续章节沿用。", sec.Title)
	}
	return fmt.Sprintf("本章「%s」逐条响应已映射的招标要求，响应口径与承诺边界以本章正文为准。", sec.Title)
}
