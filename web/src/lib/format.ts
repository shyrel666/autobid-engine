export function fmtTime(s?: string | null): string {
  if (!s) return '—'
  const d = new Date(s)
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return '—'
  return d.toLocaleString('zh-CN', { hour12: false })
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

export const taskStatusLabel: Record<string, string> = {
  created: '未启动', running: '运行中', paused: '已暂停', done: '已完成', failed: '失败', timeout: '超时',
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
}

export const requirementCategoryLabel: Record<string, string> = {
  scoring_item: '评分项', mandatory_clause: '★ 强制条款', non_scoring_required: '非评分必答项', format_attachment: '格式与附件',
}

/** 评分项分组：价格分由报价文件响应，不进入技术标大纲。 */
export const scoreGroupLabel: Record<string, string> = { '': '未分类', technical: '技术分', commercial: '商务分', price: '价格分' }

export const reviewStatusLabel: Record<string, string> = { draft: '待复核', confirmed: '已确认', needs_revision: '原文变更或未再提取·需复核' }

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
}

export const chartTypeLabel: Record<string, string> = {
  architecture: '技术架构图', gantt: '实施甘特图', org: '组织架构图', equipment_table: '设备选型表',
}

export const chartRenderLabel: Record<string, string> = { pending: '待渲染', rendered: '已渲染', unrendered: '未渲染（mmdc 不可用）', failed: '渲染失败' }

export const exportStatusLabel: Record<string, string> = { queued: '排队中', running: '执行中', succeeded: '已完成', failed: '失败' }

export const exportWarningLabel: Record<string, string> = {
  chart_unrendered: '图表未渲染', chart_failed: '图表渲染失败', chart_stale: '图表已过期', toc_not_refreshed: '目录页码未刷新',
  chart_artifact_missing: '图表产物缺失', retried: '回收重试', self_check: '成稿自检', office_unavailable: 'LibreOffice 不可用',
  renderer_unavailable: 'mmdc 不可用',
}

export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}
