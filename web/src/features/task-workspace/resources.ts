// 资源失效矩阵（FRONTEND_REFACTOR_PLAN.md 第 7.3 节）：某个动作成功后需要重新读取的前端资源。
// 只定义前端重读范围，不重写后端失效规则；页面通过 TaskWorkspaceProvider 的 tick 订阅对应资源。

export type Resource =
  | 'task' | 'overview' | 'documents' | 'requirements' | 'outline' | 'sections' | 'charts'
  | 'matrix' | 'consistency' | 'export' | 'exportJobs' | 'templates' | 'evidence' | 'research' | 'intercepts' | 'business'

export type ResourceAction =
  | 'document_imported' | 'requirements_changed' | 'bid_scope_changed' | 'veto_checked'
  | 'manifest_confirmed' | 'outline_changed'
  | 'section_saved' | 'section_ai_written' | 'section_rolled_back' | 'section_approved'
  | 'evidence_verified' | 'source_invalidated'
  | 'matrix_verified' | 'consistency_changed'
  | 'chart_changed' | 'template_changed' | 'export_created'
  | 'intercept_decided' | 'run_controlled'

const afterDraftChange: Resource[] = ['sections', 'business', 'charts', 'matrix', 'consistency', 'export']
const afterRequirements: Resource[] = ['documents', 'requirements', 'business', 'outline', 'sections', 'matrix', 'consistency', 'export']

export const INVALIDATION: Record<ResourceAction, readonly Resource[]> = {
  document_imported: afterRequirements,
  requirements_changed: afterRequirements,
  bid_scope_changed: afterRequirements,
  veto_checked: ['requirements', 'business', 'export'],
  manifest_confirmed: ['requirements', 'outline', 'business', 'sections', 'matrix', 'consistency', 'export'],
  outline_changed: ['requirements', 'outline', 'business', 'sections', 'matrix', 'consistency', 'export'],
  section_saved: afterDraftChange,
  section_ai_written: afterDraftChange,
  section_rolled_back: afterDraftChange,
  section_approved: afterDraftChange,
  evidence_verified: ['evidence', 'requirements', 'sections', 'business', 'matrix', 'consistency', 'export'],
  source_invalidated: ['evidence', 'requirements', 'sections', 'business', 'matrix', 'consistency', 'export'],
  matrix_verified: ['matrix', 'business', 'export'],
  consistency_changed: ['consistency', 'business', 'export'],
  chart_changed: ['charts', 'export'],
  template_changed: ['templates', 'export'],
  export_created: ['export', 'exportJobs'],
  intercept_decided: ['intercepts', 'overview', 'task'],
  run_controlled: ['intercepts', 'overview', 'task'],
}

/** 活动流事件到资源失效：章节写作 / 大纲事件字段不足以定位章节，做有界的整组重读，不猜测章节 ID。 */
export function resourcesForActivity(a: { worker: string; kind: string }): Resource[] {
  const out: Resource[] = []
  if (a.worker === 'section-writer' || a.worker === 'proposal') out.push(...INVALIDATION.section_ai_written, 'outline')
  if (a.kind === 'intercept_request' || a.kind === 'intercept_decided') out.push('intercepts')
  if (a.kind !== 'tool') out.push('overview', 'task')
  return [...new Set(out)]
}
