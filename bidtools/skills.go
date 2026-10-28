package bidtools

import "github.com/autobid/autobid-engine/internal/tool"

// 技能名。
const (
	SkillQualificationAudit = "qualification-audit"
	SkillScoringSimulation  = "scoring-simulation"
	SkillCommercialPricing  = "commercial-pricing"
)

// Skills 返回内置招投标专业技能；每个技能解锁一个延迟工具（§4.3 阶段 B）。
func Skills() []tool.Skill {
	return []tool.Skill{
		{
			Name:        SkillQualificationAudit,
			Description: "资格与否决项核验：逐条比对招标资格/★条款与本企业资质、业绩、人员证书，给出匹配度与缺口证据。",
			Tools:       []string{ToolQueryCredentials},
			Body: `# 资格与否决项核验

1. 用 search_document（mandatory_only=true，或关键词“资格 资质 业绩 项目经理”）找出资格条款与 ★/废标/否决条款，用 read_chunk 读原文并保留 source_ref。
2. 把每条要求拆成可核验的检索条件：category（qualification 资质 / performance 业绩 / personnel 人员 / financial 财务 / certificate 认证）、
   keywords（证书名称、专业、等级关键词），attributes（数量或等级门槛，如 {"level":"一级"}、{"amount":5000000}、{"years":3}），min_count（需要几项）。
3. 先用 SearchExtraTools("select:query_company_credentials") 取得 Schema，再调用 query_company_credentials；它会对每条候选给出 score、satisfied、gaps 与 verdict。
4. 对每条要求 record_fact：summary 写“满足/部分满足/不满足 + 依据”，source_ref 用 read_chunk 返回的原文出处，anchors 锚定对应 qualification/clause 要素（没有就先 insert_assets）。
5. verdict 为 missing 或 partial 且条款为 ★/否决项时，report_finding（category=compliance，severity=critical），evidence 引用上一步的 fact，source_refs 附原文出处。`,
		},
		{
			Name:        SkillScoringSimulation,
			Description: "商务技术评分项模拟：汇总已提取的评分项与已确认事实，计算预期得分、满分占比与核心失分项。",
			Tools:       []string{ToolSimulateScoring},
			Body: `# 评分项模拟

1. 用 list_assets(types=["scoring_item"]) 检查评分项是否已入图；缺失的先用 document_outline / search_document("评分 分值") + read_chunk 读评分表，
   再 insert_assets（type=scoring_item，code 用评分表编号，score_max 为满分，category 为 技术/商务/价格，source_ref 用 read_chunk 返回值）。
2. 对每个评分项核实我方能拿到的分数，用 record_fact 记录（anchors 锚定该评分项；可在 summary 中写明预计得分），
   或在调用时通过 assumptions 给出 expected_score 与 fact_ids。
3. 先用 SearchExtraTools("select:simulate_scoring_table") 取得 Schema，再调用 simulate_scoring_table。
4. 结果中的 top_losses 是失分最多的评分项、unevidenced 是没有任何事实支撑的评分项：针对前者 report_finding（category=scoring）提出提分策略，
   针对后者 record_fact 说明缺口或建议 Planner 追加核验。`,
		},
		{
			Name:        SkillCommercialPricing,
			Description: "报价清单与限价合规验算：对 commercial_item 做最高限价、分项合价勾稽、异常低价与风险利润率验算。",
			Tools:       []string{ToolVerifyCommercial},
			Body: `# 报价合规验算

1. 用 search_document("最高限价 控制价 报价") 与 read_chunk 找出最高限价、报价清单与报价规则（含超限价废标条款），保留 source_ref。
2. 用 list_assets(types=["commercial_item","tender","lot"]) 确认清单项已入图；缺失的用 insert_assets 写入（type=commercial_item，quantity/unit，amount 为分项限价；
   项目或标段最高限价写在 tender/lot 要素的 amount）。
3. 先用 SearchExtraTools("select:verify_commercial_bid") 取得 Schema，再调用 verify_commercial_bid，传入拟报价 quotes（unit_price、quantity、line_total）、
   declared_total（投标总价）与 cost_estimate（成本测算，可选）。该工具受审批拦截，可能需要等待人工批准。
4. 每个 severity 为 critical/high 的 issue 都要 report_finding（category=pricing；超最高限价、低于成本属于废标风险，severity=critical），
   evidence 引用相关 fact，source_refs 附限价条款原文出处；验算通过也 record_fact 记录结论。`,
		},
	}
}
