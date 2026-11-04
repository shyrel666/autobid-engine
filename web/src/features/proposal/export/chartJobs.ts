// S3 章节图表的展示与请求键规则（纯函数，单测见 chartJobs.test.ts）：
// 图表状态以最近一次渲染作业为准，作业状态与图表渲染状态分开；旧修订图表单独列出、不显示为当前图；
// 请求键在同一次提交的重发 / 重复点击时复用，用户改变输入或明确重新执行时换新键（服务端按键幂等，按钮禁用只改善体验）。

import type { ChartJobStatus, ProposalChart } from '../../../api/types'
import type { Tone } from '../../../components/ui'

export const CHART_JOB_STATUS_LABEL: Record<ChartJobStatus, string> = {
  queued: '排队渲染', running: '渲染中', succeeded: '已渲染', failed: '渲染失败', cancelled: '已取消', superseded: '已被新修订取代',
}

export interface ChartStateView {
  label: string
  tone: Tone
  /** 有排队 / 执行中的渲染作业：页面轮询刷新，可取消。 */
  active: boolean
  /** 可按新请求重新渲染：图表仍绑定章节当前修订且没有活跃作业。 */
  canRetry: boolean
  note?: string
}

function isActive(s: ChartJobStatus | undefined) {
  return s === 'queued' || s === 'running'
}

export function chartStateView(c: ProposalChart): ChartStateView {
  const j = c.latest_job
  const active = isActive(j?.status)
  if (c.stale) {
    return { label: '已过期', tone: 'amber', active, canRetry: false,
      note: `${c.stale_reason || '章节已更新'}；旧修订图表不进入导出，确认仍适用后「绑定当前修订」，否则重新生成。` }
  }
  if (j && active) {
    return { label: CHART_JOB_STATUS_LABEL[j.status], tone: 'blue', active, canRetry: false,
      note: `渲染作业 #${j.id} · 第 ${Math.max(j.attempts, 1)}/${j.max_attempts} 次${j.error ? ` · 上次失败：${j.error}` : ''}` }
  }
  const lastFailed = j && (j.status === 'failed' || j.status === 'cancelled')
  switch (c.render_status) {
    case 'rendered':
      return { label: '已渲染', tone: 'green', active, canRetry: c.source_format === 'mermaid',
        note: j && lastFailed ? `最近一次重新渲染${j.status === 'failed' ? `失败（${j.error || '无详情'}）` : '已取消'}，保留原有渲染结果。` : undefined }
    case 'failed':
      return { label: '渲染失败', tone: 'red', active, canRetry: true, note: j?.error || c.render_error || undefined }
    case 'unrendered':
      return { label: '未渲染', tone: 'amber', active, canRetry: true,
        note: `${j?.error || c.render_error || '渲染器不可用'}；导出时插入源码占位框。` }
  }
  if (j?.status === 'cancelled') return { label: '已取消', tone: 'slate', active, canRetry: true, note: '渲染作业已取消，可重新渲染。' }
  return { label: '待渲染', tone: 'amber', active, canRetry: true }
}

/** 当前修订图表在前；绑定旧修订的图表单独列出，不显示为当前图。 */
export function splitCharts(cs: ProposalChart[]): { current: ProposalChart[]; stale: ProposalChart[] } {
  return { current: cs.filter((c) => !c.stale), stale: cs.filter((c) => c.stale) }
}

/** 重试键由图表与其最近一次作业确定：响应丢失后再点复用原键（服务端返回原作业），作业变化后的重试自然换新键。 */
export function retryRequestKey(c: Pick<ProposalChart, 'id' | 'latest_job'>): string {
  return `ui-retry-${c.id}-after-${c.latest_job?.id ?? 0}`
}

export interface InputKey { sig: string; key: string }

/** 同一输入复用请求键（网络重试 / 重复提交），输入变化时换新键。 */
export function keyForInput(prev: InputKey | undefined, sig: string, fresh: () => string): InputKey {
  return prev && prev.sig === sig ? prev : { sig, key: fresh() }
}
