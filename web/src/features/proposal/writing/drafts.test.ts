import { test } from 'node:test'
import assert from 'node:assert/strict'
import { draftKey, draftState, isDirty, loadDraft, newDraft, rebase, saveDraft, toggleEvidence } from './drafts.ts'

const rev = (content: string, ids: number[] = []) => ({ content_md: content, summary_note: 's', evidence_ids: ids })

test('新草稿不脏；修改正文、摘要、说明或证据即为未保存', () => {
  const d = newDraft(rev('a', [1, 2]), 3, 2)
  assert.equal(isDirty(d), false)
  assert.equal(isDirty({ ...d, content: 'b' }), true)
  assert.equal(isDirty({ ...d, summary: 'x' }), true)
  assert.equal(isDirty({ ...d, diffNote: '改了' }), true)
  assert.equal(isDirty(toggleEvidence(d, 3, true)), true)
  assert.equal(isDirty({ ...d, evidenceIds: [2, 1] }), false, '证据顺序不影响')
})

test('后台出现新修订时草稿保持原基线并标为 behind，不被覆盖', () => {
  const d = { ...newDraft(rev('a'), 3, 2), content: '我的修改' }
  assert.equal(draftState(d, { current_rev_no: 3, outline_version: 2 }), 'dirty')
  assert.equal(draftState(d, { current_rev_no: 4, outline_version: 2 }), 'behind')
  assert.equal(draftState(d, { current_rev_no: 3, outline_version: 5 }), 'behind')
  assert.equal(d.baseRev, 3)
  assert.equal(draftState(newDraft(rev('a'), 3, 2), { current_rev_no: 9, outline_version: 9 }), 'clean')
})

test('用户明确改换基线时保留本地文本', () => {
  const d = { ...newDraft(rev('a'), 3, 2), content: '我的修改' }
  const r = rebase(d, rev('别人保存的'), 4, 2)
  assert.equal(r.content, '我的修改')
  assert.equal(r.baseRev, 4)
  assert.equal(draftState(r, { current_rev_no: 4, outline_version: 2 }), 'dirty')
  assert.equal(isDirty(rebase({ ...d, content: '别人保存的' }, rev('别人保存的'), 4, 2)), false)
})

test('草稿仓库按任务与章节隔离', () => {
  saveDraft(draftKey(1, 2), newDraft(rev('x'), 1, 1))
  assert.ok(loadDraft(draftKey(1, 2)))
  assert.equal(loadDraft(draftKey(2, 2)), undefined)
  saveDraft(draftKey(1, 2), null)
  assert.equal(loadDraft(draftKey(1, 2)), undefined)
})
