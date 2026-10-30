import type { ExportReadiness, OutlineDetail, OutlineList, RequirementsView } from '../../api/types.ts'
import { taskUrl } from '../../lib/routes.ts'
import type { TaskPage } from '../../lib/routes.ts'

// 项目概览的展示状态派生（FRONTEND_REFACTOR_PLAN.md 第 6.1 节）：只用于导航建议，保留原始业务状态与来源。
// 未加载 / 读取失败 / 没有数据 / 确实完成是四种不同情况；分母为零不显示完成，失效不沿用绿色状态。

export type DerivedState = 'unknown' | 'empty' | 'action_required' | 'in_progress' | 'complete' | 'stale' | 'error'

export interface Loadable<T> { data?: T; error?: string }

export interface Step {
  key: 'requirements' | 'veto' | 'outline' | 'sections' | 'matrix' | 'consistency' | 'export'
  label: string
  state: DerivedState
  detail: string
  page: TaskPage
  href: string
}

export interface ProgressInputs {
  requirements: Loadable<RequirementsView>
  outlines: Loadable<OutlineList>
  /** 当前批准大纲的详情（无批准大纲时可为空）。 */
  approvedOutline: Loadable<OutlineDetail | null>
  readiness: Loadable<ExportReadiness>
}

const step = (taskId: number, key: Step['key'], label: string, page: TaskPage, state: DerivedState, detail: string, query?: Record<string, string>): Step =>
  ({ key, label, state, detail, page, href: taskUrl(taskId, page, { query }) })

export function deriveSteps(taskId: number, inp: ProgressInputs): Step[] {
  const out: Step[] = []
  const r = inp.requirements
  // 1. 资料与要求清单复核
  if (r.error) out.push(step(taskId, 'requirements', '要求清单复核', 'requirements', 'error', `读取失败：${r.error}`))
  else if (!r.data) out.push(step(taskId, 'requirements', '要求清单复核', 'requirements', 'unknown', '加载中'))
  else {
    const s = r.data.stats, m = r.data.manifest
    if (s.total === 0) out.push(step(taskId, 'requirements', '要求清单复核', 'documents', 'empty', '尚无要求：先导入招标文件，再从原文提取'))
    else if (m.status === 'stale') out.push(step(taskId, 'requirements', '要求清单复核', 'requirements', 'stale', `清单确认后发生变更（epoch ${m.epoch}），需重新复核确认`))
    else if (s.needs_revision > 0) out.push(step(taskId, 'requirements', '要求清单复核', 'requirements', 'action_required', `${s.needs_revision} 条要求原文变更待复核`, { filter: 'needs_revision' }))
    else if (m.status !== 'confirmed') out.push(step(taskId, 'requirements', '要求清单复核', 'requirements', 'action_required', `已复核 ${s.confirmed}/${s.total} 条，清单尚未确认完整`))
    else out.push(step(taskId, 'requirements', '要求清单复核', 'requirements', 'complete', `清单已确认（epoch ${m.epoch}，${s.total} 条）`))
  }
  // 2. 否决项核对
  if (r.error) out.push(step(taskId, 'veto', '否决项核对', 'requirements', 'error', '要求清单读取失败', { filter: 'veto' }))
  else if (!r.data) out.push(step(taskId, 'veto', '否决项核对', 'requirements', 'unknown', '加载中', { filter: 'veto' }))
  else {
    const v = r.data.stats.veto
    if (v.total === 0) out.push(step(taskId, 'veto', '否决项核对', 'requirements', 'empty', '未提取到否决项：请对照原文审查表确认是否漏项', { filter: 'veto' }))
    else if (v.failed > 0) out.push(step(taskId, 'veto', '否决项核对', 'requirements', 'action_required', `${v.failed} 项不通过，投标将被判无效`, { filter: 'veto' }))
    else if (v.pending > 0) out.push(step(taskId, 'veto', '否决项核对', 'requirements', 'action_required', `待核对 ${v.pending} 项（通过 ${v.passed}/${v.total}）`, { filter: 'veto' }))
    else out.push(step(taskId, 'veto', '否决项核对', 'requirements', 'complete', `全部 ${v.total} 项已核对通过`, { filter: 'veto' }))
  }
  // 3. 大纲审批
  const o = inp.outlines
  let approved = 0
  if (o.error) out.push(step(taskId, 'outline', '大纲审批', 'outline', 'error', `读取失败：${o.error}`))
  else if (!o.data) out.push(step(taskId, 'outline', '大纲审批', 'outline', 'unknown', '加载中'))
  else {
    const l = o.data
    approved = l.approved_version
    const open = l.outlines.find((x) => x.version === l.open_version)
    if (l.outlines.length === 0) out.push(step(taskId, 'outline', '大纲审批', 'outline', 'empty', '尚无大纲：确认要求清单后生成'))
    else if (!approved) out.push(step(taskId, 'outline', '大纲审批', 'outline', open?.status === 'pending_approval' ? 'action_required' : 'in_progress',
      open ? `v${open.version} ${open.status === 'pending_approval' ? '待审批' : '草稿编辑中'}，尚无批准版本` : '尚无批准版本'))
    else if (open) out.push(step(taskId, 'outline', '大纲审批', 'outline', 'in_progress', `当前批准 v${approved}；v${open.version} ${open.status === 'pending_approval' ? '待审批' : '修订中'}`))
    else out.push(step(taskId, 'outline', '大纲审批', 'outline', 'complete', `当前批准大纲 v${approved}`))
  }
  // 4. 章节写作与定稿（以当前批准大纲为准）
  const a = inp.approvedOutline
  if (!approved) out.push(step(taskId, 'sections', '章节写作与定稿', 'writing', o.data ? 'unknown' : o.error ? 'error' : 'unknown', '批准大纲后可写作'))
  else if (a.error) out.push(step(taskId, 'sections', '章节写作与定稿', 'writing', 'error', `读取失败：${a.error}`))
  else if (!a.data) out.push(step(taskId, 'sections', '章节写作与定稿', 'writing', 'unknown', '加载中'))
  else {
    const secs = a.data.sections
    const done = secs.filter((s) => s.status === 'approved').length
    const stale = secs.filter((s) => s.status === 'stale_outline' || s.status === 'stale_evidence').length
    const empty = secs.filter((s) => s.current_rev_no === 0).length
    if (secs.length === 0) out.push(step(taskId, 'sections', '章节写作与定稿', 'writing', 'empty', '批准大纲没有章节'))
    else if (stale > 0) out.push(step(taskId, 'sections', '章节写作与定稿', 'writing', 'stale', `${stale} 个章节因大纲或证据失效需重新处理；已定稿 ${done}/${secs.length}`))
    else if (done === secs.length) out.push(step(taskId, 'sections', '章节写作与定稿', 'writing', 'complete', `全部 ${secs.length} 个章节已定稿`))
    else out.push(step(taskId, 'sections', '章节写作与定稿', 'writing', done || secs.length > empty ? 'in_progress' : 'action_required',
      `已定稿 ${done}/${secs.length}${empty ? `，${empty} 个章节尚无正文` : ''}`))
  }
  // 5–7. 核验与导出门禁：读取导出就绪度；无批准大纲时后端只返回 no_approved_outline，不能据此判定后续步骤完成。
  const rd = inp.readiness
  const gate = (key: Step['key'], label: string, page: TaskPage, kinds: string[], staleKinds: string[], okText: string) => {
    if (rd.error) return step(taskId, key, label, page, 'error', `导出就绪度读取失败：${rd.error}`)
    if (!rd.data) return step(taskId, key, label, page, 'unknown', '加载中')
    if (rd.data.blockers.some((b) => b.kind === 'no_approved_outline')) return step(taskId, key, label, page, 'unknown', '批准大纲后判定')
    const hits = rd.data.blockers.filter((b) => kinds.includes(b.kind))
    if (hits.length === 0) return step(taskId, key, label, page, 'complete', okText)
    const stale = hits.some((b) => staleKinds.includes(b.kind))
    return step(taskId, key, label, page, stale ? 'stale' : 'action_required', `${hits.length} 项：${hits[0].message}`)
  }
  out.push(gate('matrix', '响应矩阵核验', 'matrix', ['matrix_stale', 'matrix_failed', 'matrix_unverified', 'requirement_unmapped'], ['matrix_stale'], '映射与正文实质响应均无阻断'))
  out.push(gate('consistency', '一致性核验', 'consistency', ['consistency_not_run', 'consistency_stale', 'finding_open'], ['consistency_stale'], '最新核验适用于当前正文，无待处理结论'))
  if (rd.error) out.push(step(taskId, 'export', '成稿导出门禁', 'export', 'error', `读取失败：${rd.error}`))
  else if (!rd.data) out.push(step(taskId, 'export', '成稿导出门禁', 'export', 'unknown', '加载中'))
  else if (rd.data.ready) out.push(step(taskId, 'export', '成稿导出门禁', 'export', 'complete', '当前可导出；创建导出时服务端会再次校验'))
  else out.push(step(taskId, 'export', '成稿导出门禁', 'export', 'action_required', `${rd.data.blockers.length} 项阻止导出`))
  return out
}

/** 下一步建议：按资料与清单、否决项、大纲、章节、矩阵、一致性、导出的顺序取第一个需要处理的步骤；只是导航建议。 */
export function nextStep(steps: Step[]): Step | undefined {
  return steps.find((s) => s.state === 'error' || s.state === 'action_required' || s.state === 'stale' || s.state === 'empty' || s.state === 'in_progress')
}
