// S4 导出请求组的展示与请求键规则（纯函数，单测见 exportJobs.test.ts）：
// 作业状态与业务状态分开——严格最终稿成功才显示“最终稿”，其余成功产物（含 S4 之前的旧作业）一律“仅预览稿”，不冒充最终交付；
// 多格式部分失败逐项展示；同一输入的重复点击 / 响应丢失后重发复用请求键，服务器已明确答复（受理或拒绝）后，再次点击视为新的执行。

import type { ExportFormat, ExportJob, ExportMode, ExportRequest } from '../../../api/types'
import type { Tone } from '../../../components/ui'

export const EXPORT_ERROR_LABEL: Record<string, string> = {
  RUNTIME_UNAVAILABLE: '文档引擎不可用或与提交时固定的版本不符', READBACK_FAILED: '成稿回读未通过', BUDGET_EXHAUSTED: '截止时间或执行次数已耗尽',
  CANCELLED: '已取消', EXPORT_FAILED: '生成失败',
}

export interface ExportJobView {
  label: string
  tone: Tone
  /** 严格最终稿已生成（目录已由文档引擎刷新并记录引擎）。 */
  final: boolean
  /** 排队或执行中：页面轮询刷新，可取消。 */
  active: boolean
  canDownload: boolean
  note?: string
}

const fmtDeadline = (s?: string) => (s ? new Date(s).toLocaleString('zh-CN', { hour12: false, timeZone: 'Asia/Shanghai' }) : '')

export function exportJobView(j: ExportJob): ExportJobView {
  switch (j.status) {
    case 'queued':
    case 'running': {
      const parts = [j.status === 'queued' ? '等待执行' : `第 ${Math.max(j.attempts, 1)}/${j.max_attempts} 次执行`]
      if (j.deadline_at) parts.push(`截止 ${fmtDeadline(j.deadline_at)}`)
      // 暂停退回排队只写说明不写错误码，不算失败；可重试失败回到排队时带错误码。
      if (j.error) parts.push(j.error_code ? `上次失败：${j.error}` : j.error)
      return { label: j.status === 'queued' ? '排队中' : '执行中', tone: 'blue', final: false, active: true, canDownload: false, note: parts.join(' · ') }
    }
    case 'succeeded':
      if (j.business_state === 'final_ready') {
        return { label: '最终稿', tone: 'green', final: true, active: false, canDownload: true,
          note: `目录已由 ${j.office_version || j.office_engine || '文档引擎'} 刷新，产物已回读核对` }
      }
      return { label: '仅预览稿', tone: 'amber', final: false, active: false, canDownload: true,
        note: j.toc_refreshed ? '按预览稿策略导出，不作为最终交付' : '目录页码未刷新，不作为最终交付' }
    case 'cancelled':
      return { label: '已取消', tone: 'slate', final: false, active: false, canDownload: false,
        note: j.error && j.error !== '已取消' ? j.error : undefined }
    default: {
      const reason = j.error_code ? EXPORT_ERROR_LABEL[j.error_code] ?? j.error_code : ''
      return { label: '失败', tone: 'red', final: false, active: false, canDownload: false, note: [reason, j.error].filter(Boolean).join('：') || undefined }
    }
  }
}

/** 请求组汇总：任一格式未结束即为进行中；部分格式失败 / 取消时为部分完成，逐项看作业。 */
export function exportRequestView(q: ExportRequest): { label: string; tone: Tone; active: boolean } {
  const active = q.jobs.some((j) => j.status === 'queued' || j.status === 'running')
  if (active) return { label: q.status === 'running' ? '执行中' : '排队中', tone: 'blue', active }
  switch (q.status) {
    case 'succeeded':
      return q.business_state === 'final_ready' ? { label: '最终稿已生成', tone: 'green', active } : { label: '预览稿已生成', tone: 'amber', active }
    case 'partial':
      return { label: '部分完成', tone: 'amber', active }
    case 'cancelled':
      return { label: '已取消', tone: 'slate', active }
    default:
      return { label: '失败', tone: 'red', active }
  }
}

export interface ExportGroup {
  /** 列表键：请求组 r<id>，S4 之前没有请求组的旧作业 j<id>。 */
  key: string
  request?: ExportRequest
  jobs: ExportJob[]
  createdAt: string
}

/** 合并请求组与没有请求组的旧作业，新者在前（以服务器列表为准，不在页面推断状态）。 */
export function groupExportJobs(requests: ExportRequest[], jobs: ExportJob[]): ExportGroup[] {
  const out: ExportGroup[] = requests.map((q) => ({ key: `r${q.id}`, request: q, jobs: q.jobs, createdAt: q.created_at }))
  for (const j of jobs) if (!j.export_request_id) out.push({ key: `j${j.id}`, jobs: [j], createdAt: j.created_at })
  const newest = (g: ExportGroup) => Math.max(...g.jobs.map((j) => j.id), 0)
  return out.sort((a, b) => b.createdAt.localeCompare(a.createdAt) || newest(b) - newest(a))
}

export interface ExportInput {
  formats: ExportFormat[]; template_id?: number; bidder?: string; mode: ExportMode; expected_outline_version?: number
}

/** 规范化的输入签名：格式去重排序，空投标人与默认模板归一；签名相同的提交复用请求键。 */
export function exportInputSig(in_: ExportInput): string {
  const formats = [...new Set(in_.formats)].sort()
  return JSON.stringify([formats, in_.template_id || 0, (in_.bidder ?? '').trim(), in_.mode, in_.expected_outline_version || 0])
}

/** 服务器已明确答复（受理或拒绝）时，下一次点击是新的执行，应换新键；网络中断等未得到答复时保留原键重发。 */
export function keepKeyAfter(err: unknown): boolean {
  const status = (err as { status?: number } | undefined)?.status
  return !status || (status >= 500 && status !== 503)
}
