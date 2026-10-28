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
