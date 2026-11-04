import type { ExportBlocker } from '../../api/types.ts'
import { taskUrl } from '../../lib/routes.ts'

// 导出阻断 → 修复位置（FRONTEND_REFACTOR_PLAN.md 第 5.8 节阻断跳转表），集中维护。
// 有具体标识时定位具体对象；缺失标识时进入对应列表；未知类型返回 null，由页面保留服务器原文与刷新操作。

export interface BlockerLink { href: string; label: string }

const pos = (n?: number) => (n && n > 0 ? n : undefined)

export function blockerLink(taskId: number, b: ExportBlocker): BlockerLink | null {
  const req = pos(b.requirement_item_id)
  const sec = pos(b.section_id)
  switch (b.kind) {
    case 'required_material_incomplete':
      return { href: taskUrl(taskId, 'documents'), label: '去补齐必需材料' }
    case 'no_approved_outline':
      return { href: taskUrl(taskId, 'outline'), label: '去审批大纲' }
    case 'requirements_not_confirmed':
      return { href: taskUrl(taskId, 'requirements'), label: '去确认要求清单' }
    case 'requirements_source_changed':
      return { href: taskUrl(taskId, 'requirements'), label: '去重新提取要求' }
    case 'no_bid_decision':
      return { href: taskUrl(taskId, 'requirements'), label: '去修改投标决策' }
    case 'requirement_unmapped':
    case 'requirement_superseded':
      return { href: taskUrl(taskId, 'outline', { query: { requirement: req } }), label: '去调整大纲映射' }
    case 'veto_pending':
    case 'veto_failed':
      return { href: taskUrl(taskId, 'requirements', { query: { filter: 'veto', requirement: req } }), label: '去核对否决项' }
    case 'section_not_approved':
    case 'storyboard_missing':
    case 'storyboard_unapproved':
    case 'storyboard_stale':
    case 'ungrounded_claim':
    case 'writing_verification_stale':
    case 'writing_verification_failed':
    case 'section_self_approved':
      return sec
        ? { href: taskUrl(taskId, 'writing', { sectionId: sec }), label: '去定稿章节' }
        : { href: taskUrl(taskId, 'writing'), label: '去章节列表' }
    case 'matrix_stale':
    case 'matrix_failed':
    case 'matrix_unverified':
      return { href: taskUrl(taskId, 'matrix', { query: { requirement: req, section: sec } }), label: '去响应矩阵' }
    case 'consistency_not_run':
    case 'consistency_stale':
    case 'finding_open':
      return { href: taskUrl(taskId, 'consistency', { query: { finding: pos(b.finding_id) } }), label: '去一致性核验' }
    case 'chart_not_final':
      return sec
        ? { href: taskUrl(taskId, 'writing', { sectionId: sec }), label: '去渲染章节图表' }
        : { href: taskUrl(taskId, 'writing'), label: '去章节列表' }
    case 'office_unavailable':
      return { href: taskUrl(taskId, 'export'), label: '查看运行能力' }
    case 'evidence_invalid':
      if (sec) return { href: taskUrl(taskId, 'writing', { sectionId: sec, query: { panel: 'evidence', evidence: pos(b.evidence_id) } }), label: '去章节证据' }
      if (req) return { href: taskUrl(taskId, 'requirements', { query: { requirement: req } }), label: '去相关要求' }
      return { href: taskUrl(taskId, 'research'), label: '去证据检索' }
    default:
      return null
  }
}
