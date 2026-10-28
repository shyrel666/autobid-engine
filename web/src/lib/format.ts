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

export const reviewStatusLabel: Record<string, string> = { draft: '待复核', confirmed: '已确认', needs_revision: '原文已变·需复核' }

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
