package bidtools

import "github.com/autobid/autobid-engine/internal/tool"

// 技能名。
const (
	SkillQualificationAudit = "qualification-audit"
	SkillScoringSimulation  = "scoring-simulation"
	SkillCommercialPricing  = "commercial-pricing"
	SkillKnowledgeReuse     = "knowledge-reuse"
	SkillProposalWriting    = "proposal-writing"
	SkillProposalReview     = "proposal-review"
)

// Skills 返回内置招投标专业技能；每个技能解锁其所属的延迟工具（§4.3 阶段 B）。
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
		{
			Name:        SkillKnowledgeReuse,
			Description: "企业方案知识库复用：检索历史标书、标准技术方案、产品规格书与白皮书中可复用的段落与参数表，并强制附带来源引用。",
			Tools:       []string{ToolKBSearchProposals, ToolKBSearchProducts, ToolKBReadChunk},
			Body: `# 企业方案知识库复用

1. 先用 search_document / read_chunk 读清招标技术要求（含 ★/▲ 参数），把每条要求拆成检索关键词与行业标签。
2. 方案类内容（总体架构、实施方案、运维保障、类似项目经验）：先 SearchExtraTools("select:kb_search_proposals") 取 Schema，
   再调用 kb_search_proposals（query 为技术需求关键词，tags 为行业标签如 ["智慧园区"]），在历史标书与标准方案中找可复用段落。
3. 产品与技术指标：SearchExtraTools("select:kb_search_products") 后调用 kb_search_products（product 为产品/型号名，必须出现在命中文档中；
   specs 为指标关键词如 ["分辨率","400万像素"]），优先命中含参数表的分块。
4. 复用或比对前必须 SearchExtraTools("select:kb_read_chunk") 并调用 kb_read_chunk(doc_id, seq) 读取完整正文与 tables 表格 JSON，
   逐项核对指标值是否满足招标要求（数值门槛按 ≥ / ≤ 比较），不得凭检索摘要下结论。
5. 记录结论时 record_fact 的 summary 写明“知识库出处：<cite>”，source_ref 原样写入工具返回的 citation
   {doc_id, doc_title, category, section_code, section_path, page_start, page_end, seq}；没有出处的知识库内容不得写入事实或结论。
6. 知识库中找不到满足 ★ 参数的产品或方案时，report_finding（category=risk）说明缺口，提示人工补充资料。`,
		},
		{
			Name:        SkillProposalWriting,
			Description: "技术标章节写作：在已批准大纲下读取章节写作上下文（要求原文、前置章节摘要、证据有效性），撰写或重写章节草稿并经数据库门禁写回。",
			Tools:       []string{ToolSectionReadContext, ToolSectionWriteDraft},
			Body: `# 技术标章节写作

1. 先 SearchExtraTools("select:section_read_context") 取 Schema，调用 section_read_context（section_id 或已批准大纲中的 section_code）。
   writable=false 时停止写作并 record_fact 说明 blocked_reason（大纲未批准 / 已被取代 / 前置章节尚无有效稿），不得绕过。
2. 逐条对照 requirements：★/▲ 条款逐条明确响应；评分项按评分点组织内容；source_excerpt 是招标原文，引用时保持一致。
3. 事实性内容只能来自 usable_evidence（verified + supports），并把引用的证据 id 写入 evidence_ids；unusable_evidence 与没有证据的指标、
   资质、业绩只能写成待确认事项并列入 gaps，严禁改写成已证实承诺。需要补证据时先用 knowledge-reuse 技能检索与核验。
4. 与 dependencies 中前置章节的 summary_note 保持术语与承诺一致；在 summary_note 中写 3~6 句衔接摘要供后续章节读取。
5. SearchExtraTools("select:section_write_draft") 后调用 section_write_draft，base_rev_no 与 based_on_outline_version 必须原样使用
   section_read_context 的返回值（派发时给了 lease_token 也一并传入）。
6. outcome 为 stale_outline（大纲已在你写作期间被修改）或 conflict_candidate（他人已保存新版本）时，草稿只是候选稿，没有覆盖当前版本：
   不要换个版本号强行重试，重新读取上下文并报告结果，由人工决定是否采用。`,
		},
		{
			Name:        SkillProposalReview,
			Description: "技术标正文审查：逐条核验要求在章节当前修订中的实质响应（响应矩阵），并做全文关键承诺一致性与 ★/▲ 漏项核验。",
			Tools:       []string{ToolComplianceVerify, ToolFulltextConsistent},
			Body: `# 技术标正文审查

1. 先 SearchExtraTools("select:compliance_verify") 取 Schema，调用 compliance_verify（可按 section_code 限定单章）。
   它只核验已批准大纲下各章节的当前有效修订；verified 要求正文有实质应答，只挂章节名、只有 citation 或复述要求都不算。
2. failed / pending 行说明正文缺少实质应答或数值不满足门槛：用 proposal-writing 技能修改对应章节，而不是改写核验结论。
3. SearchExtraTools("select:fulltext_consistency_check") 后调用 fulltext_consistency_check，比对总工期、质保期、SLA、团队人数、
   核心软硬件型号与数量是否前后一致，并列出没有通过核验的 ★/▲ 条款。
4. 对每条 contradiction 与 omission 用 record_fact 记录（写明双方章节、rev_no 与原文片段）并提示人工处理；
   规则抽取的召回率与准确率未经评测，「无结论」不等于全文一致，不得据此宣称可以导出。`,
		},
	}
}
