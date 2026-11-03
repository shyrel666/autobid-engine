/** fmtTime 统一以北京时间显示业务时间（与投标截止口径一致），不随浏览器所在时区变化。 */
export function fmtTime(s?: string | null): string {
  return fmtBeijing(s) || '—'
}

export function fmtPct(p: number | null | undefined): string {
  return p === null || p === undefined ? '—' : `${(p * 100).toFixed(1)}%`
}

export function fmtNum(n: number | undefined): string {
  return n === undefined ? '—' : n.toLocaleString('zh-CN')
}

export function pretty(v: unknown): string {
  if (v === undefined || v === null) return ''
  if (typeof v === 'string') {
    try {
      return JSON.stringify(JSON.parse(v), null, 2)
    } catch {
      return v
    }
  }
  return JSON.stringify(v, null, 2)
}

export function payloadSummary(p: Record<string, unknown> | null | undefined): string {
  if (!p) return ''
  const s = p.summary ?? p.title
  return typeof s === 'string' ? s : ''
}

export function downloadText(filename: string, text: string, type: string) {
  const blob = new Blob([text], { type })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

/** 任务运行状态：只表示推演运行生命周期，与标书完成度无关（运行结束 ≠ 标书完成）。 */
export const taskStatusLabel: Record<string, string> = {
  created: '未启动', running: '运行中', paused: '已暂停', done: '运行已结束', failed: '运行失败', timeout: '运行超时',
}

/** fmtBeijing 以北京时间显示「2006-01-02 15:04」，用于投标截止时间等业务时刻。 */
export function fmtBeijing(s?: string | null): string {
  if (!s) return ''
  const d = new Date(s)
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return ''
  const p = Object.fromEntries(new Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', hour12: false }).formatToParts(d).map((x) => [x.type, x.value]))
  return `${p.year}-${p.month}-${p.day} ${p.hour === '24' ? '00' : p.hour}:${p.minute}`
}

export const assetTypeLabel: Record<string, string> = {
  tender: '招标项目', lot: '标段', clause: '条款', qualification: '资格要求', scoring_item: '评分项', commercial_item: '商务项',
}

export const findingCategoryLabel: Record<string, string> = {
  compliance: '合规性', risk: '风险', scoring: '评分策略', pricing: '报价', strategy: '投标策略',
}

export const severityLabel: Record<string, string> = {
  critical: '致命', high: '高', medium: '中', low: '低', info: '提示',
}

export const severityOrder = ['critical', 'high', 'medium', 'low', 'info']

export const credentialCategoryLabel: Record<string, string> = {
  qualification: '企业资质', performance: '项目业绩', personnel: '人员证书', financial: '财务信誉', certificate: '体系认证',
}

export const knowledgeCategoryLabel: Record<string, string> = {
  historical_bid: '历史标书', whitepaper: '技术白皮书', product_spec: '产品规格书', standard_solution: '标准技术方案',
}

// pageRange 把页码范围格式化为 "P3" 或 "P3–5"。
export function pageRange(start?: number, end?: number): string {
  if (!start) return 'P—'
  return end && end > start ? `P${start}–${end}` : `P${start}`
}

export const docFormatLabel: Record<string, string> = {
  text: '纯文本', markdown: 'Markdown', docx: 'Word (.docx)', pdf: 'PDF', image: '扫描图片 (OCR)',
  xlsx: 'Excel (.xlsx)', csv: 'CSV', tsv: 'TSV', zip: '招标包 (.zip)',
}

export const requirementCategoryLabel: Record<string, string> = {
  scoring_item: '评分项', mandatory_clause: '★ 强制条款', non_scoring_required: '非评分必答项', format_attachment: '格式与附件',
  veto_item: '否决项',
}

/** 评分项分组：价格分由报价文件响应，不进入技术标大纲。 */
export const scoreGroupLabel: Record<string, string> = { '': '未分类', technical: '技术分', commercial: '商务分', price: '价格分' }

export const evalKindLabel: Record<string, string> = { '': '', objective: '客观分', subjective: '主观分', mixed: '主客观结合' }

export const bidDecisionLabel: Record<string, string> = { pending: '待决策', bid: '决定投标', no_bid: '放弃投标' }

export const bidRecommendationLabel: Record<string, string> = {
  bid: '建议投标', caution: '存在风险·需补齐', no_bid: '不建议投标',
  can_bid: '建议投标', high_risk: '存在风险·需补齐',
}

export const reviewStatusLabel: Record<string, string> = {
  draft: '待复核', confirmed: '已确认', needs_revision: '原文变更或未再提取·需复核', stale: '已被更正/澄清覆盖',
}

export const manifestStatusLabel: Record<string, string> = { draft: '未确认', confirmed: '已确认完整', stale: '已变更·需重新确认' }

export const outlineStatusLabel: Record<string, string> = {
  draft: '草稿', pending_approval: '待审批', approved: '已批准', superseded: '已取代',
}

export const sectionStatusLabel: Record<string, string> = {
  pending: '等待前置章节', ready: '可写作', writing: '撰写中', draft: '草稿', reviewing: '待审核', approved: '已定稿',
  rejected: '已驳回', stale_outline: '大纲已失效', stale_evidence: '证据已失效',
}

export const revisionSourceLabel: Record<string, string> = {
  ai_initial: 'AI 初稿', ai_rewrite: 'AI 重写', human_edit: '人工编辑', rollback: '回滚',
}

export const revisionStatusLabel: Record<string, string> = {
  draft: '草稿', reviewing: '待审核', approved: '曾批准', rejected: '已驳回', stale_outline: '大纲过期候选稿',
  stale_evidence: '证据失效', conflict_candidate: '冲突候选稿',
}

// Phase 6A：响应矩阵与一致性核验
export const requirementKindLabel: Record<string, string> = {
  star: '★ 条款', triangle: '▲ 条款', scoring: '评分项', required: '必答项', format: '格式项',
}

export const verifyStatusLabel: Record<string, string> = {
  verified: '实质响应', failed: '未通过', stale: '已失效·待重检', pending: '章节无正文', unchecked: '未核验', unmapped: '未映射',
}

export const deviationLabel: Record<string, string> = { none: '无偏离', positive: '正偏离', negative: '负偏离' }

export const findingStatusLabel: Record<string, string> = { open: '待处理', resolved: '已处理', stale: '已失效' }

export const resolutionKindLabel: Record<string, string> = { fixed: '已修正', accepted: '确认无误（说明原因）', false_positive: '误报' }

export const exportBlockerLabel: Record<string, string> = {
  no_approved_outline: '无批准大纲', requirements_not_confirmed: '要求清单未确认', requirement_unmapped: '要求未映射',
  section_not_approved: '章节未定稿', matrix_stale: '响应核验失效', matrix_failed: '响应核验未通过', matrix_unverified: '尚未核验',
  consistency_not_run: '未做一致性核验', consistency_stale: '一致性核验过期', finding_open: '一致性结论待处理', evidence_invalid: '证据失效',
  veto_pending: '否决项待核对', veto_failed: '否决项不通过', no_bid_decision: '已决策放弃投标', requirement_superseded: '章节仍引用已覆盖旧要求',
}

// D1：否决项与招标包
export const vetoKindLabel: Record<string, string> = { qualification: '资格性审查', conformity: '符合性审查', statutory: '法定无效情形' }

export const vetoStatusLabel: Record<string, string> = { pending: '待核对', passed: '通过', failed: '不通过' }

export const docRoleLabel: Record<string, string> = {
  tender: '招标文件', correction: '更正公告', clarification: '澄清', qa: '答疑', attachment: '附件',
}

export const chartTypeLabel: Record<string, string> = {
  architecture: '技术架构图', gantt: '实施甘特图', org: '组织架构图', equipment_table: '设备选型表',
}

export const chartRenderLabel: Record<string, string> = { pending: '待渲染', rendered: '已渲染', unrendered: '未渲染（mmdc 不可用）', failed: '渲染失败' }

export const exportStatusLabel: Record<string, string> = { queued: '排队中', running: '执行中', succeeded: '已生成', failed: '失败' }

export const exportWarningLabel: Record<string, string> = {
  chart_unrendered: '图表未渲染', chart_failed: '图表渲染失败', chart_stale: '图表已过期', toc_not_refreshed: '目录页码未刷新',
  chart_artifact_missing: '图表产物缺失', retried: '回收重试', self_check: '成稿自检', office_unavailable: '文档引擎不可用',
  renderer_unavailable: 'mmdc 不可用',
}

export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

/** 业务工具与系统受控工具的中文名称映射 */
export const toolNameLabel: Record<string, string> = {
  document_outline: '招标文档章节大纲',
  search_document: '招标原文全文检索',
  read_chunk: '读取招标原文切片',
  query_company_credentials: '企业资信库核验查询',
  simulate_scoring_table: '评标得分模拟测算',
  verify_commercial_bid: '商务报价合规验算',
  kb_search_proposals: '历史标书与方案检索',
  kb_search_products: '产品规格与白皮书检索',
  kb_read_chunk: '读取知识库文档切片',
  kb_read_evidence: '读取检索证据快照',
  kb_verify_evidence: '核验技术指标声明',
  kb_research: '受控多步证据研究',
  chart_generate: '生成技术标章节图表',
  section_read_context: '读取章节写作上下文',
  requirement_bind_evidence: '绑定要求支撑证据与缺口',
  section_write_draft: '提交章节修订草稿',
  compliance_verify: '响应矩阵实质核验',
  fulltext_consistency_check: '跨章节全文一致性检查',
  Bash: '外部命令执行',
  WebFetch: '外部网页抓取',
  web_search: '外部网络检索',
  session_open: '开启持久交互会话',
  session_send: '发送交互会话指令',
  Write: '写入或覆盖文件',
  Edit: '精确替换修改文件',
  MultiEdit: '批量替换修改文件',
  extract_requirements: '抽取招标要求清单',
  credential_match: '企业资信匹配核对',
  kb_search: '企业方案知识库检索',
  section_write: '提交技术标章节草稿',
  consistency_check: '跨章节全文一致性校验',
}

/** 领域技能包（Skill）的中文名称映射 */
export const skillNameLabel: Record<string, string> = {
  'qualification-audit': '资格与否决项核验',
  'scoring-simulation': '商务技术评分模拟',
  'commercial-pricing': '商务报价合规验算',
  'knowledge-reuse': '企业方案知识库复用',
  'proposal-writing': '技术标章节写作',
  'proposal-review': '技术标正文合规审查',
}

/** Agent 角色的中文名称映射 */
export const agentRoleLabel: Record<string, string> = {
  goals: '目标分解 Agent (Goals)',
  planner: '推演规划 Agent (Planner)',
  worker: '任务执行 Agent (Worker)',
  watchdog: '跑偏监控 Agent (Watchdog)',
  judge: '安全裁判 Agent (LLM Judge)',
}

/** JSON Schema 入参类型的中文显示名称 */
export const schemaTypeLabel: Record<string, string> = {
  string: '文本',
  integer: '整数',
  number: '数值',
  boolean: '布尔值',
  array: '列表',
  object: '对象',
  any: '任意类型',
}

/** 工具功能说明中文兜底（兼容未重启的旧版后端进程） */
export const toolChineseDescription: Record<string, string> = {
  document_outline:
    '列出当前投标项目下各招标文件（含更正公告、澄清、答疑）的章节目录大纲（返回文档 ID、切片 ID、章节编号、标题、页码范围与层级）。标记含 ★/否决/废标等强制条款及含结构化表格的切片，用于快速定位需要精读的章节。',
  search_document:
    '按关键词（空格分隔，须同时命中）或正则表达式全文检索当前项目的招标文件切片。支持仅筛选含 ★/废标/否决/实质性响应的强制条款；返回匹配摘要与切片 ID，配合「读取招标原文切片」获取完整正文与溯源引用。',
  read_chunk:
    '读取单个招标文件切片的完整正文，可选附带前后各最多 3 个相邻切片上下文，同时返回切片内的结构化表格（行列结构、合并单元格与页码）及标准溯源引用 source_ref，确保所有提取要素与审查结论有据可查。',
  query_company_credentials:
    '对照招标资格要求或评分项核验本企业资信库（企业资质、项目业绩、人员证书、财务信誉、体系认证）。返回每条候选资信的匹配得分、有效期核查与差距说明，并给出总体满足度判定（满足 / 部分满足 / 缺失）。',
  simulate_scoring_table:
    '基于当前项目已提取的评分项要素及锚定其上的已确认事实节点，模拟测算技术与商务评标得分。返回逐项预期得分、总分、得分率、按类别小计、前五大失分项及尚无证据支撑的评分项列表。',
  verify_commercial_bid:
    '对照当前项目的商务报价清单要素使用精确小数运算全面验算拟投标报价：逐行核对“单价 × 数量 = 合价”、单项最高限价、漏项与重复报价、分项合计与投标总价一致性、最高投标限价（超限否决）、异常低价风险与预期利润率。',
  kb_search_proposals:
    '检索企业方案知识库中的历史中标标书与标准技术方案，查找匹配技术需求关键词的可复用方案段落，支持按行业标签过滤。采用全文检索、向量语义相似度与关键词混合排序，所有命中结果强制附带标准来源引用。',
  kb_search_products:
    '按产品型号名称与技术指标检索企业方案知识库中的产品规格书与技术白皮书。支持限定产品型号精确出现，含结构化参数表的切片优先排序；引用具体指标数值前需进一步读取完整表格。',
  kb_read_chunk:
    '按知识库文档 ID 与切片序号读取知识库切片完整正文及结构化参数表格，可选附带前后各最多 2 个相邻切片。超长正文与大表支持按偏移量分页读取并保留原始单元格坐标，同时返回标准来源引用。',
  kb_read_evidence:
    '读取当前项目下某次知识库检索运行保存的证据快照及其实时有效状态。检索命中仅代表相关，未经显式核验前状态为未核验（unverified）。',
  kb_verify_evidence:
    '对照原始证据快照、产品元数据、计量单位与时效性，对结构化数值或技术指标响应声明执行确定性核验。带附加条件或存在歧义的指标将保持“证据不足”状态并提示人工复核。',
  kb_research:
    '受控多步证据研究工具：基于幂等请求键、共享检索预算与最多两轮追问补查知识库证据，返回持久化检索运行记录、证据快照 ID 与明确的证据缺口。',
  chart_generate:
    '按结构化规格生成技术标章节图表并绑定到该章节当前主版本。支持技术架构图、实施甘特图、项目组织架构图与设备选型表四类图表，由服务端确定性生成并校验 Mermaid 源码与图表产物。',
  section_read_context:
    '读取技术标指定章节的完整写作上下文：绑定的招标要求及原文摘录、前置依赖章节的衔接摘要、当前主版本正文、已绑定证据的实时有效性与待补缺口，并返回写回草稿所需的基准版本号。',
  requirement_bind_evidence:
    '将已核验通过的证据 ID（verified + supports）或待补缺口说明绑定到指定的招标要求项（requirement_item），打通知识库/资信检索证据与技术标章节写作上下文，且不会推翻已批准大纲。',
  section_write_draft:
    '将技术标章节修订草稿写回数据库。数据库在同一事务内校验批准大纲版本与乐观锁基准修订号：若大纲已变更或发生并发冲突，草稿将自动归档为候选稿，绝不覆盖当前主版本。',
  compliance_verify:
    '对当前批准大纲（或指定章节）绑定的全部招标要求逐条核验正文是否给出实质性响应（拒绝仅复述标题或照搬要求原文），将核验结果与段落锚点写入响应矩阵，并返回偏离类型与不通过原因。',
  fulltext_consistency_check:
    '横向比对当前批准大纲下所有章节有效修订稿中的关键定量承诺（总工期、质保期、SLA 时限与可用性、团队人数、核心设备型号与数量），检出跨章节前后矛盾项，并汇总 ★/▲ 强制条款漏项。',
}

/** 工具入参字段的中文说明映射（兼容未重启的旧版后端进程） */
export const toolParamChineseDesc: Record<string, string> = {
  doc_id: '指定文档 ID（在招标检索中为招标文档 ID，在知识库读取中为知识库文档 ID）',
  max_level: '限制返回的最大标题层级（1 为一级章，2 为二级节，3+ 为编号条款；不填则返回全部层级）',
  mandatory: '为 true 时仅返回含 ★/*/废标/否决/无效投标/实质性 等强制性条款的分块',
  query: '检索关键词（多个词以空格分隔），或当 regex=true 时为正则表达式',
  regex: '是否将检索词作为 Go/RE2 正则表达式解析',
  mandatory_only: '是否仅检索 ★/否决/废标/实质性响应等强制条款',
  limit: '返回结果条数上限',
  chunk_id: '要读取的招标文件切片 ID（来自章节大纲或原文检索结果）',
  before: '附带读取前序相邻切片的数量',
  after: '附带读取后续相邻切片的数量',
  requirement: '待核验的招标资格或评分要求原文摘录或概括描述',
  category: '资信类别过滤：企业资质(qualification) / 项目业绩(performance) / 人员证书(personnel) / 财务信誉(financial) / 体系认证(certificate)',
  keywords: '匹配资信或文档必须包含的关键词列表（如 ["信息系统集成", "一级"]）',
  attributes: '结构化属性阈值要求（数值按 >= 比对，文本按包含关系比对，如 {"level":"一级","amount":5000000}）',
  min_count: '要求完全满足条件的资信最低数量（如要求 3 个同类业绩则填 3，默认 1）',
  reference_date: '证书有效期核验基准日期 YYYY-MM-DD（如投标截止日，默认今天）',
  as_of: '有效性核验基准日期 YYYY-MM-DD',
  lot_no: '指定标段编号（所有标段共用的要素会自动包含；不填则处理全部标段）',
  assumptions: '对特定评分项的预期得分覆盖列表（用于多方案对比测算）',
  asset_id: '关联的招标要素 ID（评分项或商务清单项 ID）',
  expected_score: '该评分项的预期可得分数（自动限制在 0 到满分之间）',
  fact_ids: '支撑该得分的已确认事实节点 ID 列表',
  note: '说明备注（得分依据或缺口说明）',
  ceiling_price: '最高投标限价（不填时优先取标段要素限价，其次取招标项目总限价）',
  quotes: '拟填报的分项报价明细列表',
  code: '清单项或要素编码',
  unit_price: '填报含税单价',
  quantity: '报价数量（不填则默认取招标清单要素数量）',
  line_total: '报价表填报的单项合价',
  declared_total: '开标一览表/报价总表填报的投标总价',
  cost_estimate: '内部测算总成本（用于核验是否低于成本报价及测算利润率）',
  min_profit_margin: '最低要求利润率（0~1 之间小数，默认 0.03 即 3%）',
  abnormal_low_ratio: '异常低价预警占最高限价比例阈值（默认 0.70 即 70%）',
  tolerance: '金额勾稽允许的最大绝对尾差（默认 0.01 元）',
  tags: '行业或业务标签过滤列表，命中任一标签即符合（如 ["智慧园区", "政务"]）',
  categories: '限定检索的知识文档类别列表',
  product: '必须精确出现的产品或设备型号名称（如 "DS-2CD7A47" 或 "统一视频平台"）',
  specs: '技术指标或参数关键词列表（如 ["分辨率", "400万像素", "H.265"]）',
  seq: '知识库文档内的切片序号（来自检索结果 citation.seq）',
  text_offset: '正文 UTF-8 字节分页偏移量（翻页时填入上一页返回的 next_text_offset）',
  table_index: '大表分页时指定的表格索引号，默认 0',
  row_offset: '大表分页时的起始行偏移量（自动保留表头行）',
  context_budget: '单次读取的正文字节预算上限，默认 16000，最大 32000',
  expected_hash: '上一页返回的 content_hash（用于校验翻页期间源文档未被替换）',
  run_id: '检索运行记录 ID',
  evidence_id: '证据快照 ID',
  claim: '待核验的结构化数值或技术指标声明对象',
  request_key: '当前任务与研究问题的唯一幂等键（传入相同键可恢复已有研究进度）',
  question: '完整保留所有技术约束条件的研究问题',
  multi_agent: '是否请求独立证据子智能体协同检索（默认 false）',
  section_id: '技术标章节 ID（与 section_code 二选一）',
  section_code: '批准大纲中的章节编号（如 "2.1"，与 section_id 二选一）',
  chart_type: '图表类型：技术架构图(architecture) / 实施甘特图(gantt) / 组织架构图(org) / 设备选型表(equipment_table)',
  title: '图表标题/图注（如 "系统总体技术架构图"）',
  spec: '结构化图表规格对象（按图表类型定义层级、甘特任务、组织节点或设备行列）',
  base_rev_no: '基准修订版本号（来自读取章节上下文返回的 base_rev_no）',
  based_on_outline_version: '所依据的批准大纲版本号（来自读取章节上下文返回的 based_on_outline_version）',
  content_md: '章节完整 Markdown 正文',
  summary_note: '3~6 句章节衔接摘要（含统一术语、关键架构结论与承诺边界，供后续依赖章节读取）',
  diff_summary: '相对于上一主版本的修改要点说明',
  evidence_ids: '实际引用的有效证据 ID（usable_evidence）列表',
  gaps: '正文中未能证实、需显式披露的证据缺口列表',
  kind: '缺口类型，如 gap（缺少支撑证据）或 unverified（证据未核验）',
  requirement_id: '关联的招标要求 ID（可选）',
  lease_token: '章节写作派发租约令牌（可选）',
  reverify: '检查前是否先对全部章节重新运行响应矩阵核验（默认 true）',
}

/** 获取工具的中文显示标题 */
export function formatToolTitle(name: string): string {
  return toolNameLabel[name] || name
}

/** 获取技能的中文显示标题 */
export function formatSkillTitle(name?: string): string {
  if (!name) return '技能解锁'
  return skillNameLabel[name] || name
}

/** 获取工具的中文功能说明（若后端返回英文描述则自动使用中文说明） */
export function formatToolDescription(name: string, rawDesc?: string): string {
  if (rawDesc && /[\u4e00-\u9fa5]/.test(rawDesc) && !/^[A-Za-z]{3,}/.test(rawDesc.trim())) {
    return rawDesc
  }
  return toolChineseDescription[name] || rawDesc || '招投标业务工具'
}

/** 获取工具入参字段的中文说明（若后端返回英文描述则自动使用中文说明） */
export function formatParamDescription(paramName: string, rawDesc?: string): string {
  if (rawDesc && /[\u4e00-\u9fa5]/.test(rawDesc) && !/^[A-Za-z]{3,}/.test(rawDesc.trim())) {
    return rawDesc
  }
  return toolParamChineseDesc[paramName] || rawDesc || '—'
}
