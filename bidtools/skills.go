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
	SkillDiagramGeneration  = "diagram-generation"
	SkillProposalExport     = "proposal-export"
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
			Description: "企业方案知识库复用与证据核验：检索历史标书、标准技术方案、产品规格书与白皮书中可复用的段落与参数表，对数值/范围/枚举断言执行确定性证据核验（kb_verify_evidence），并支持预算受控的多步检索（kb_research）。",
			Tools:       []string{ToolKBSearchProposals, ToolKBSearchProducts, ToolKBReadChunk, ToolKBReadEvidence, ToolKBVerifyEvidence, ToolKBResearch},
			Body: `# 企业方案知识库复用与证据核验

1. 先用 search_document / read_chunk 读清招标技术要求（含 ★/▲ 参数），把每条要求拆成检索关键词与行业标签。
2. 方案类内容（总体架构、实施方案、运维保障、类似项目经验）：先 SearchExtraTools("select:kb_search_proposals") 取 Schema，
   再调用 kb_search_proposals（query 为技术需求关键词，tags 为行业标签如 ["智慧园区"]），在历史标书与标准方案中找可复用段落。
3. 产品与技术指标：SearchExtraTools("select:kb_search_products") 后调用 kb_search_products（product 为产品/型号名，必须出现在命中文档中；
   specs 为指标关键词如 ["分辨率","400万像素"]），优先命中含参数表的分块。注意保留返回结果中的 evidence_id。
4. 跨多类资料或复杂技术指标组合检索时，可先 SearchExtraTools("select:kb_research") 再调用 kb_research(request_key, question, product, multi_agent) 执行受预算控制的多步研究。
5. 复用或比对前必须读取完整正文与表格：按文档位置先 SearchExtraTools("select:kb_read_chunk") 调用 kb_read_chunk(doc_id, seq)，
   或按检索运行 ID 先 SearchExtraTools("select:kb_read_evidence") 调用 kb_read_evidence(run_id) 读取证据快照，不得凭检索摘要下结论。
6. 凡是将知识库参数或资质作为标书实质性响应依据（usable_evidence）前，必须先 SearchExtraTools("select:kb_verify_evidence")，
   再调用 kb_verify_evidence(run_id, evidence_id, claim={text, parameter, operator, value, unit, product}) 对数值指标做确定性核验，使其状态更新为 verified + supports。
7. 记录结论时 record_fact 的 summary 写明“知识库出处：<cite>”，source_ref 原样写入工具返回的 citation
   {doc_id, doc_title, category, section_code, section_path, page_start, page_end, seq}；没有出处的知识库内容不得写入事实或结论。
8. 知识库中找不到满足 ★ 参数的产品或方案时，report_finding（category=risk）说明缺口，提示人工补充资料。`,
		},
		{
			Name:        SkillProposalWriting,
			Description: "技术标章节写作：在已批准大纲下读取章节写作上下文（要求原文、前置章节摘要、证据有效性），编制章节故事板（D3 粉队评审对象），对受控引用标记执行确定性展开，撰写或重写章节草稿并经数据库门禁写回；需要图表时引用 diagram-generation 技能。",
			Tools:       []string{ToolSectionReadContext, ToolRequirementBindEvidence, ToolSectionUpsertStoryboard, ToolSectionRenderGrounded, ToolSectionWriteDraft, ToolChartGenerate},
			Body: `# 技术标章节写作

1. 先 SearchExtraTools("select:section_read_context") 取 Schema，调用 section_read_context（section_id 或已批准大纲中的 section_code）。
   writable=false 时停止写作并 record_fact 说明 blocked_reason（大纲未批准 / 已被取代 / 前置章节尚无有效稿），不得绕过。
2. 逐条对照 requirements：★/▲ 条款逐条明确响应；评分项按评分点组织内容；source_excerpt 是招标原文，引用时保持一致。
3. 事实性内容只能来自 usable_evidence（verified + supports）与 admissible 企业证据，并把引用的证据 id 写入 evidence_ids / admissibility_item_ids；unusable_evidence 与没有证据的指标、
   资质、业绩只能写成待确认事项并列入 gaps，严禁改写成已证实承诺。需要补证据时先用 knowledge-reuse 技能检索与 kb_verify_evidence 核验，
   再通过 SearchExtraTools("select:requirement_bind_evidence") 调用 requirement_bind_evidence 将 verified + supports 的 evidence_ids 或 evidence_gap 绑定到对应 requirement_item。
4. D3 故事板与受控引用渲染：动笔前可先 SearchExtraTools("select:section_upsert_storyboard") 调用 section_upsert_storyboard 创建或更新章节故事板（含 theme_sentence、scoring_points、白名单证据与承诺 ID），交由粉队评审；
   草稿中的投标人事实与承诺应使用 [[EVID:<id_or_code>]]、[[KB:<evidence_id>]]、[[COMMIT:<code_or_id>]] 标记，并可先通过 SearchExtraTools("select:section_render_grounded_draft") 调用 section_render_grounded_draft 预览确定性展开结果。
5. 与 dependencies 中前置章节的 summary_note 保持术语与承诺一致；在 summary_note 中写 3~6 句衔接摘要供后续章节读取。
6. SearchExtraTools("select:section_write_draft") 后调用 section_write_draft，base_rev_no 与 based_on_outline_version 必须原样使用
   section_read_context 的返回值（派发时给了 lease_token 也一并传入）。
7. outcome 为 stale_outline（大纲已在你写作期间被修改）或 conflict_candidate（他人已保存新版本）时，草稿只是候选稿，没有覆盖当前版本：
   不要换个版本号强行重试，重新读取上下文并报告结果，由人工决定是否采用。
8. 需要图表时（总体架构、实施进度、项目组织、设备选型），在章节草稿写回成功后调用 Skill("diagram-generation")，按其步骤用
   chart_render_submit 为章节当前修订提交后台渲染作业（不要自己写 Mermaid；设备型号、数量等必须来自 usable_evidence）。
   图表绑定提交时的修订，章节再次修订后旧图表自动失效、不进入导出，需要为新修订重新提交。
   SearchExtraTools("select:chart_generate") 可取得同步兼容工具 chart_generate：它在本次调用内渲染、不登记后台作业，
   不满足 required_job_kinds=chart_render，新图表一律走 diagram-generation。`,
		},
		{
			Name:        SkillDiagramGeneration,
			Description: "章节图表生成（proposal-writing 引用）：按结构化规格为章节当前修订提交持久化图表渲染作业（技术架构图、实施甘特图、组织架构图、设备选型表），查询或取消作业；结果以作业事件唤醒当前意图。",
			Tools:       []string{ToolChartRenderSubmit, ToolChartRenderStatus, ToolChartRenderCancel},
			Body: `# 章节图表生成

1. 只为已写回成功的章节当前修订生成图表：先用 section_read_context 读取章节，base_rev_no 即章节当前修订号；章节尚无修订时先用 proposal-writing 写草稿。
2. SearchExtraTools("select:chart_render_submit") 取 Schema，再按结构化 spec 调用 chart_render_submit：
   chart_type 为 architecture / gantt / org / equipment_table，不要自己写 Mermaid；设备型号、数量、工期、人员等必须来自 usable_evidence 或已确认的承诺台账。
   expected_rev_no 填章节当前修订号；request_key 自己生成（如 "chart-2.1-architecture-1"），同一次提交重发时复用，改变规格或重新执行时换新键。
3. accepted=true 后作业在服务器后台渲染。当前意图会在作业结束后被自动唤醒并收到作业事件：期间不要轮询，也不要以相同参数重复提交，本轮总结后即可结束。
4. 被拒绝（accepted=false）时按 error_code 处理：CONFLICT_STALE_REVISION 重新读取章节后以新键提交；INVALID_ARGUMENT 修正 spec；
   RUNTIME_UNAVAILABLE 表示服务器没有可用的 mermaid-cli（设备选型表不受影响），record_fact 记录缺口；PRECONDITION_FAILED 表示章节尚无修订或大纲未批准；
   CAPACITY_BUSY 稍后重试；IDEMPOTENCY_KEY_CONFLICT 换新键。未纠正原因前不要盲目重试。
5. 收到作业事件后：business_state=chart_ready 表示图表已绑定章节当前修订；chart_stale 或 superseded 表示章节已更新，旧图不进入导出，需按新修订重新提交；
   failed 时 record_fact 记录失败原因与 error_code。
6. 查询单个作业：SearchExtraTools("select:chart_render_status") 后调用 chart_render_status（或公共工具 agent_job_status）；
   不再需要排队或执行中的作业时，SearchExtraTools("select:chart_render_cancel") 后调用 chart_render_cancel 取消，取消后迟到的结果不会写回。`,
		},
		{
			Name:        SkillProposalExport,
			Description: "技术标合稿导出：按严格最终稿策略预检导出门禁，提交锁定版本的 DOCX / PDF 导出请求组（文档引擎 WPS / Word 真实刷新目录、图表已渲染、产物回读），查询或取消；结果以作业事件唤醒当前意图。",
			Tools:       []string{ToolProposalExportCheck, ToolProposalExportSubmit, ToolProposalExportStatus, ToolProposalExportCancel},
			Body: `# 技术标合稿导出

1. 先 SearchExtraTools("select:proposal_export_check") 取 Schema，调用 proposal_export_check（formats 为 ["docx","pdf"] 的子集，可选 template_id）。
   ready=false 时不要提交：blockers 指向未批准章节、失效的响应矩阵 / 一致性核验、未渲染图表（chart_not_final）或文档引擎不可用（office_unavailable），
   用对应技能处理或 record_fact 记录缺口并交由人工处理；不要改写核验结论来“通过”门禁。
2. ready=true 后 SearchExtraTools("select:proposal_export_submit") 取 Schema，调用 proposal_export_submit：
   expected_outline_version 填预检返回的 outline_version；request_key 自己生成（如 "export-final-v3-1"），同一次提交重发时复用，改变格式、模板或投标人时换新键。
   导出一律是严格最终稿，模型不能选择预览稿；服务器未开放 Agent 导出时返回 FEATURE_DISABLED，此时 record_fact 说明需由人工在导出页导出，不要反复重试。
3. accepted=true 后各格式作业在服务器后台执行；当前意图会在作业结束后被自动唤醒并收到作业事件：期间不要轮询，也不要以相同参数重复提交，本轮总结后即可结束。
4. 被拒绝（accepted=false）时按 error_code 处理：PRECONDITION_FAILED 看 blockers；CONFLICT_STALE_REVISION 重新预检后以新键提交；
   RUNTIME_UNAVAILABLE 表示没有可用的文档引擎；IDEMPOTENCY_KEY_CONFLICT 换新键；CAPACITY_BUSY 稍后重试；SCOPE_VIOLATION 表示模板或作业超出当前意图范围。
5. 收到作业事件后：business_state=final_ready 表示该格式的严格最终稿已生成（目录已由文档引擎刷新，sha256 与下载路由见 proposal_export_status）；
   failed 时 record_fact 记录 error_code（READBACK_FAILED 为成稿回读未通过，RUNTIME_UNAVAILABLE 为引擎不可用或与提交时固定的版本不符）。
   导出成功只表示产物已生成，不代表已签章或已被交易平台接收，也不代表内容已通过外部评审。
6. 查询：SearchExtraTools("select:proposal_export_status") 后按 job_ref 或 export_request_id 调用；不再需要排队或执行中的导出时，
   SearchExtraTools("select:proposal_export_cancel") 后调用 proposal_export_cancel，整组取消会逐项返回已取消或已终态。`,
		},
		{
			Name:        SkillProposalReview,
			Description: "技术标正文审查：逐条核验要求在章节当前修订中的实质响应（响应矩阵），执行 D3 句子级证据绑定与写作核验，并做全文关键承诺一致性与 ★/▲ 漏项核验。",
			Tools:       []string{ToolComplianceVerify, ToolFulltextConsistent, ToolSectionVerifyWriting},
			Body: `# 技术标正文审查

1. 先 SearchExtraTools("select:compliance_verify") 取 Schema，调用 compliance_verify（可按 section_code 限定单章）。
   它只核验已批准大纲下各章节的当前有效修订；verified 要求正文有实质应答，只挂章节名、只有 citation 或复述要求都不算。
2. 对单章执行 D3 句子级落证与针对性审查时，先 SearchExtraTools("select:section_verify_writing") 再调用 section_verify_writing（section_id 或 section_code），
   核查 ungrounded_claim_count、fact_binding_rate、commitment_binding_rate、specificity_ratio 与 boilerplate_similarity_max。
3. failed / pending 行说明正文缺少实质应答、存在未绑定事实/承诺或数值不满足门槛：用 proposal-writing 技能修改对应章节，而不是改写核验结论。
4. SearchExtraTools("select:fulltext_consistency_check") 后调用 fulltext_consistency_check，比对总工期、质保期、SLA、团队人数、
   核心软硬件型号与数量是否前后一致，并列出没有通过核验的 ★/▲ 条款。
5. 对每条 contradiction 与 omission 用 record_fact 记录（写明双方章节、rev_no 与原文片段）并提示人工处理；
   规则抽取的召回率与准确率未经评测，「无结论」不等于全文一致，不得据此宣称可以导出。`,
		},
	}
}
