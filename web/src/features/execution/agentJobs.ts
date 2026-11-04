// S2 执行页的后台作业展示规则（纯函数，单测见 agentJobs.test.ts）：
// “等待后台处理”（blocked + waiting_job）与执行失败分开；作业状态与业务状态分开显示，预览稿不冒充最终稿。

import type { AgentJob, NodeBrief } from '../../api/types'
import type { Tone } from '../../components/ui'

export const JOB_KIND_LABEL: Record<string, string> = { tender_parse: '招标解析', proposal_export: '标书导出', chart_render: '图表渲染' }
const JOB_STATUS_LABEL: Record<string, string> = {
  queued: '排队中', running: '处理中', succeeded: '已成功', failed: '失败', cancelled: '已取消', superseded: '已被新修订取代',
}
const BUSINESS_LABEL: Record<string, string> = {
  candidate_pending_review: '候选待人工采用', adopted: '已采用', preview_only: '仅预览稿，非最终稿', final_ready: '严格最终稿已生成', completed: '已完成',
  chart_ready: '图表已绑定当前修订', chart_stale: '章节已更新，图表已过期',
}
// 成功但结果待人工处理或已不适用于当前修订的业务状态，不显示为最终成功。
const PENDING_BUSINESS = new Set(['preview_only', 'candidate_pending_review', 'chart_stale'])
const SUBSCRIPTION_LABEL: Record<string, string> = {
  waiting: '等待结果', notified: '结果待送达工作', acknowledged: '结果已送达工作', cancelled: '任务结束，已取消等待',
}

/** 工作是否在等待后台作业（仍在进行，作业结束后自动继续）。 */
export function isWaitingWork(n: Pick<NodeBrief, 'state' | 'blocked_reason'> | undefined): boolean {
  return !!n && n.state === 'blocked' && n.blocked_reason === 'waiting_job'
}

/** 阻塞原因的说明；等待后台处理不是失败。 */
export function blockedNotice(reason: string | undefined): { tone: 'info' | 'danger' | 'warn'; text: string } | null {
  switch (reason) {
    case undefined: case '': return null
    case 'waiting_job': return { tone: 'info', text: '等待后台处理：该工作已提交后台作业并释放执行槽，作业结束后会自动继续，无需操作。' }
    case 'required_job_failed': return { tone: 'danger', text: '必需的后台作业失败、被取消或结果已失效，工作未完成，等待主编审重新规划。' }
    case 'llm_quota_exhausted': return { tone: 'warn', text: '模型配额耗尽，配额恢复后可批量重开。' }
    default: return { tone: 'warn', text: `阻塞原因：${reason}` }
  }
}

export interface JobView {
  title: string
  status: string
  tone: Tone
  detail: string[]
}

/** 单条作业的展示：标题、作业状态（含业务状态）、色调与说明行。 */
export function jobView(j: AgentJob): JobView {
  const biz = j.event?.business_state ?? ''
  const status = JOB_STATUS_LABEL[j.job_status] ?? (j.job_status || '未知')
  const tone: Tone = j.job_status === 'failed' || j.job_status === 'cancelled' ? 'red'
    : j.job_status === 'superseded' ? 'amber'
      : j.job_status === 'succeeded' ? (PENDING_BUSINESS.has(biz) ? 'amber' : 'green')
        : 'blue'
  const detail: string[] = [`${SUBSCRIPTION_LABEL[j.state] ?? j.state} · 第 ${j.job_attempts}/${j.job_max_attempts} 次`]
  if (j.job_deadline_at) detail.push(`截止 ${new Date(j.job_deadline_at).toLocaleString('zh-CN', { hour12: false, timeZone: 'Asia/Shanghai' })}`)
  if (j.job_error) detail.push(`原因：${j.job_error}`)
  return {
    title: `${JOB_KIND_LABEL[j.job_ref.kind] ?? j.job_ref.kind} #${j.job_ref.id}`,
    status: biz && BUSINESS_LABEL[biz] ? `${status} · ${BUSINESS_LABEL[biz]}` : status,
    tone,
    detail,
  }
}
