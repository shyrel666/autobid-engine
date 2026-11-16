// D4 提交检查清单的展示规则（纯函数，单测见 submission.test.ts）。
// 清单的判定全部在服务器：系统项由成稿的回读结果得出，人工项由人确认并留痕。页面只决定怎么说、给哪些操作——
// 尤其不把“清单就绪”说成“已提交”，不让预览稿或导出后又改过正文的成稿显得可以交。

import type { ExportJob, ExportRequest, SubmissionChecklist, SubmissionItem, SubmissionItemStatus } from '../../../api/types'
import type { Tone } from '../../../components/ui'

export const SUBMISSION_STATUS: Record<SubmissionItemStatus, { label: string; tone: Tone }> = {
  pass: { label: '通过', tone: 'green' },
  fail: { label: '不通过', tone: 'red' },
  attention: { label: '需确认知悉', tone: 'amber' },
  pending: { label: '待确认', tone: 'amber' },
  confirmed: { label: '已确认', tone: 'green' },
  failed: { label: '检查未通过', tone: 'red' },
  not_applicable: { label: '不适用', tone: 'slate' },
}

export const SUBMISSION_ACTION: Record<string, string> = {
  confirmed: '确认', failed: '记为未通过', not_applicable: '标为不适用', withdrawn: '撤回确认',
}

/** 只有严格最终稿的请求组才有提交检查清单；预览稿不能当作提交文件，页面不为它展示清单入口。 */
export function hasChecklist(q: ExportRequest | undefined): boolean {
  return !!q && q.strict_runtime && q.business_state === 'final_ready'
}

export interface ChecklistSummary {
  label: string
  tone: Tone
  /** 说明当前状态与下一步；不出现“已提交”字样。 */
  note: string
  /** 可以写入人工确认。 */
  writable: boolean
}

export function checklistSummary(cl: SubmissionChecklist): ChecklistSummary {
  if (!cl.final) {
    return { label: '不是最终稿', tone: 'red', writable: false, note: '只有全部格式都成功的严格最终稿才能作为提交文件；请以最终稿重新导出。' }
  }
  if (!cl.current) {
    return { label: '成稿已过期', tone: 'red', writable: false,
      note: `${cl.stale_reason || '导出之后正文或大纲有变化'}。这份成稿不能再作为提交文件，已有确认只保留为记录；请重新导出后再核对。` }
  }
  const failed = cl.items.filter((it) => it.status === 'fail' || it.status === 'failed')
  if (failed.length) {
    return { label: `${failed.length} 项不通过`, tone: 'red', writable: true,
      note: `${failed.map((it) => it.label).join('、')}不通过。系统判定不通过的须修复后重新导出；人工检查未通过的，处理后可改判。` }
  }
  if (cl.ready) {
    return { label: '清单已全部处理', tone: 'green', writable: true,
      note: '系统可查项通过，人工项均已确认或标为不适用。清单只是核对记录：是否已在交易平台完成上传，以平台回执为准。' }
  }
  return { label: `还有 ${cl.open} 项待处理`, tone: 'amber', writable: true, note: '逐项核对后确认；每次确认、改判与撤回都会记录操作人与时间。' }
}

export interface ItemView {
  label: string
  tone: Tone
  /** 可执行的操作：confirm（确认 / 知悉）、fail、not_applicable、withdraw。 */
  actions: ('confirm' | 'fail' | 'not_applicable' | 'withdraw')[]
  /** 确认时是否必须写说明（知悉类）。 */
  noteRequired: boolean
  confirmLabel: string
}

export function itemView(it: SubmissionItem, writable: boolean): ItemView {
  const s = SUBMISSION_STATUS[it.status] ?? { label: it.status, tone: 'slate' as Tone }
  const v: ItemView = { ...s, actions: [], noteRequired: false, confirmLabel: '确认' }
  if (!it.confirmable) return v
  const attention = it.kind === 'auto'
  if (attention) { v.noteRequired = true; v.confirmLabel = '确认知悉' }
  const decided = it.status === 'confirmed' || it.status === 'failed' || it.status === 'not_applicable'
  // 撤回不要求仍是当前最终稿：成稿过期后仍可清理错误的确认。
  if (decided) v.actions.push('withdraw')
  if (!writable) return v
  if (it.status !== 'confirmed') v.actions.unshift('confirm')
  if (!attention) {
    if (it.status !== 'failed') v.actions.push('fail')
    if (it.status !== 'not_applicable') v.actions.push('not_applicable')
  }
  return v
}

/** 系统项在前、人工项在后；各自保持服务器给出的顺序。 */
export function splitItems(cl: SubmissionChecklist): { auto: SubmissionItem[]; manual: SubmissionItem[] } {
  return { auto: cl.items.filter((it) => it.kind === 'auto'), manual: cl.items.filter((it) => it.kind === 'manual') }
}

/** 作业行里的一句回读摘要：评分索引与页码核对的结果，没有回读记录（旧作业、未成功）时为空。 */
export function selfCheckNote(j: ExportJob): string {
  const sc = j.self_check
  if (j.status !== 'succeeded' || !sc) return ''
  const parts: string[] = []
  if (sc.pages) parts.push(`${sc.pages} 页`)
  if (sc.score_rows > 0) parts.push(`评分索引 ${sc.score_rows} 项`)
  if (sc.page_refs > 0) {
    parts.push(sc.page_ref_issues === 0 && sc.page_refs_verified >= sc.page_refs
      ? `页码引用 ${sc.page_refs} 处已核对${sc.verified_against === 'pdf' ? '' : '（仅与目录比对）'}`
      : `页码引用 ${sc.page_refs} 处，核对一致 ${sc.page_refs_verified} 处`)
  }
  return parts.join(' · ')
}
