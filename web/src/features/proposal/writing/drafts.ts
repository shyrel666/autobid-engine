// 章节草稿（FRONTEND_REFACTOR_PLAN.md 第 7.2 节）：记录正文、摘要、修改说明、证据集合与开始编辑时的
// base_rev_no / based_on_outline_version。后台刷新只更新远端状态，不覆盖本地文本，也不悄悄改换编辑基线。
// 纯逻辑，可用 node --test 验证（drafts.test.ts）。

export interface RevisionLike { content_md: string; summary_note: string; evidence_ids: number[] }

export interface Draft {
  content: string
  summary: string
  diffNote: string
  evidenceIds: number[]
  /** 编辑基线：开始编辑（或用户明确改换基线）时的主版本修订号。 */
  baseRev: number
  baseOutline: number
  /** 基线修订的内容，用于判断是否有未保存修改。 */
  origin: { content: string; summary: string; evidenceIds: number[] }
}

const sameIDs = (a: number[], b: number[]) => a.length === b.length && [...a].sort((x, y) => x - y).every((v, i) => v === [...b].sort((x, y) => x - y)[i])

export function newDraft(rev: RevisionLike | undefined, baseRev: number, baseOutline: number): Draft {
  const origin = { content: rev?.content_md ?? '', summary: rev?.summary_note ?? '', evidenceIds: [...(rev?.evidence_ids ?? [])] }
  return { content: origin.content, summary: origin.summary, diffNote: '', evidenceIds: [...origin.evidenceIds], baseRev, baseOutline, origin }
}

export function isDirty(d: Draft | null | undefined): boolean {
  if (!d) return false
  return d.content !== d.origin.content || d.summary !== d.origin.summary || d.diffNote.trim() !== '' || !sameIDs(d.evidenceIds, d.origin.evidenceIds)
}

export type DraftState = 'clean' | 'dirty' | 'behind'

/** behind：草稿有修改且编辑期间主版本或大纲已变化；此时保存会被后端识别为冲突并归档为候选稿。 */
export function draftState(d: Draft | null | undefined, current: { current_rev_no: number; outline_version: number }): DraftState {
  if (!isDirty(d)) return 'clean'
  return d!.baseRev !== current.current_rev_no || d!.baseOutline !== current.outline_version ? 'behind' : 'dirty'
}

/** rebase 只在用户明确选择后调用：保留本地文本，把编辑基线改为最新修订。 */
export function rebase(d: Draft, latest: RevisionLike | undefined, latestRev: number, latestOutline: number): Draft {
  return {
    ...d, baseRev: latestRev, baseOutline: latestOutline,
    origin: { content: latest?.content_md ?? '', summary: latest?.summary_note ?? '', evidenceIds: [...(latest?.evidence_ids ?? [])] },
  }
}

export function toggleEvidence(d: Draft, id: number, on: boolean): Draft {
  const set = new Set(d.evidenceIds)
  if (on) set.add(id)
  else set.delete(id)
  return { ...d, evidenceIds: [...set] }
}

// 内存草稿仓库：按 任务 + 章节 隔离，布局切换、面板抽屉或令牌变更导致组件重新挂载时不丢稿。不写入 localStorage。
const store = new Map<string, Draft>()
export const draftKey = (taskId: number, sectionId: number) => `${taskId}:${sectionId}`
export const loadDraft = (k: string) => store.get(k)
export function saveDraft(k: string, d: Draft | null) {
  if (d) store.set(k, d)
  else store.delete(k)
}
