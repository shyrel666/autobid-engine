import type { AdmissibilityRun, Commitment, RequirementItem, SectionStoryboard, StoryboardScoringPoint, UpsertStoryboardReq } from '../../../api/types'

// 故事板编辑器的纯逻辑：把章节映射的要求和已有故事板合成表单，再转成保存请求。单测在 storyboard.test.ts。

export interface PointForm { code: string; title: string; approach: string; benefit: string }
export interface StoryboardForm { theme: string; points: PointForm[]; itemIds: number[]; commitmentIds: number[] }

export const storyboardStatusLabel: Record<string, string> = {
  draft: '草稿', pending_pink_review: '待粉队评审', pink_approved: '已通过粉队评审', pink_rejected: '已退回', stale: '已失效', superseded: '已被新版本取代',
}

/** 已批准的故事板不能原地修改，保存会生成新版本并要求重新评审，所以编辑前要提示。 */
export const editableStatuses = ['draft', 'pink_rejected', 'stale', 'pink_approved']

/** 每条映射要求一行；没有映射要求的概述章节也要有一条要点，否则提交时会被拒绝。 */
export function storyboardForm(requirements: Pick<RequirementItem, 'code' | 'title'>[], sb?: SectionStoryboard): StoryboardForm {
  const saved = new Map<string, StoryboardScoringPoint>((sb?.scoring_points ?? []).map((p) => [p.requirement_code, p]))
  const points: PointForm[] = requirements.map((r) => ({
    code: r.code, title: r.title, approach: saved.get(r.code)?.our_approach ?? '', benefit: saved.get(r.code)?.buyer_benefit ?? '',
  }))
  if (points.length === 0) {
    const first = sb?.scoring_points[0]
    points.push({ code: '', title: first?.scoring_point || '本章要点', approach: first?.our_approach ?? '', benefit: first?.buyer_benefit ?? '' })
  }
  return { theme: sb?.theme_sentence ?? '', points, itemIds: sb?.admissibility_item_ids ?? [], commitmentIds: sb?.commitment_ids ?? [] }
}

export function storyboardRequest(form: StoryboardForm, evidenceIds: number[] = []): UpsertStoryboardReq {
  return {
    theme_sentence: form.theme.trim(),
    scoring_points: form.points.map((p) => ({ requirement_code: p.code, scoring_point: p.title, our_approach: p.approach.trim(), buyer_benefit: p.benefit.trim() })),
    admissibility_item_ids: form.itemIds,
    evidence_ids: evidenceIds,
    commitment_ids: form.commitmentIds,
  }
}

export function toggleId(ids: number[], id: number, on: boolean): number[] {
  return on ? [...new Set([...ids, id])] : ids.filter((x) => x !== id)
}

export interface ReferenceOption { id: number; label: string; token: string }

/** 只列当前可采的明细：故事板不接受已失效或未通过的证据。 */
export function admissibleOptions(runs: AdmissibilityRun[]): ReferenceOption[] {
  return runs.filter((r) => r.status === 'current').flatMap((r) => (r.items ?? [])
    .filter((it) => it.verdict === 'admissible')
    .map((it) => ({ id: it.id, label: `${r.requirement_code} ${r.requirement_title}`, token: `[[EVID:${it.id}]]` })))
}

/** 只列已确认的承诺：草稿或已退回的承诺写进故事板会被后端拒绝。 */
export function commitmentOptions(commitments: Commitment[]): ReferenceOption[] {
  return commitments.filter((c) => c.status === 'confirmed' || c.status === 'signed_off')
    .map((c) => ({ id: c.id, label: `${c.code} ${c.metric_label} ${c.committed_raw}${c.unit}`, token: `[[COMMIT:${c.id}]]` }))
}

export function insertAt(text: string, token: string, start: number | undefined, end: number | undefined): { text: string; cursor: number } {
  const from = start ?? text.length
  const to = end ?? from
  return { text: text.slice(0, from) + token + text.slice(to), cursor: from + token.length }
}
