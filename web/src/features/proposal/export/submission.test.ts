import { test } from 'node:test'
import assert from 'node:assert/strict'
import { checklistSummary, hasChecklist, itemView, selfCheckNote, splitItems } from './submission.ts'

const item = (over: Record<string, unknown> = {}) => ({ key: 'cover_check', kind: 'manual', label: '封面与基本信息核对', status: 'pending', confirmable: true, ...over }) as never
const checklist = (over: Record<string, unknown> = {}) => ({
  task_id: 1, export_request_id: 4, checklist_version: 'white-glove/v1', business_state: 'final_ready', final: true, current: true,
  artifact_hashes: { docx: 'a', pdf: 'b' }, items: [], ready: false, open: 0, ...over,
}) as never

test('只有严格最终稿的请求组展示清单入口', () => {
  assert.equal(hasChecklist({ strict_runtime: true, business_state: 'final_ready' } as never), true)
  assert.equal(hasChecklist({ strict_runtime: false, business_state: 'preview_only' } as never), false)
  assert.equal(hasChecklist({ strict_runtime: true, business_state: 'partial' } as never), false)
  assert.equal(hasChecklist({ strict_runtime: true, business_state: 'running' } as never), false)
  assert.equal(hasChecklist(undefined), false)
})

test('清单就绪不说成已提交；过期与预览稿不可确认', () => {
  const ready = checklistSummary(checklist({ ready: true }))
  assert.equal(ready.tone, 'green')
  assert.equal(ready.writable, true)
  assert.doesNotMatch(ready.label + ready.note, /已提交|提交成功/)
  assert.match(ready.note, /以平台回执为准/)

  const stale = checklistSummary(checklist({ current: false, stale_reason: '章节 3 当前批准的是 rev 3，成稿锁定的是 rev 2' }))
  assert.equal(stale.label, '成稿已过期')
  assert.equal(stale.writable, false)
  assert.match(stale.note, /rev 3.*重新导出/)

  const preview = checklistSummary(checklist({ final: false }))
  assert.equal(preview.writable, false)
  assert.match(preview.note, /最终稿重新导出/)

  const open = checklistSummary(checklist({ open: 3 }))
  assert.equal(open.label, '还有 3 项待处理')
  assert.equal(open.tone, 'amber')
})

test('不通过的条目点名，并区分系统判定与人工检查', () => {
  const s = checklistSummary(checklist({ open: 2, items: [
    item({ key: 'metadata', kind: 'auto', label: '文档属性', status: 'fail', confirmable: false }),
    item({ key: 'signature_seal', label: '签字盖章 / 电子签章', status: 'failed' }),
    item({ status: 'confirmed' }),
  ] }))
  assert.equal(s.label, '2 项不通过')
  assert.equal(s.tone, 'red')
  assert.match(s.note, /文档属性、签字盖章 \/ 电子签章不通过/)
  assert.match(s.note, /重新导出/)
})

test('系统判定的条目没有手工操作；知悉类必须写说明且不能判未通过', () => {
  assert.deepEqual(itemView(item({ kind: 'auto', status: 'pass', confirmable: false }), true).actions, [])
  assert.deepEqual(itemView(item({ kind: 'auto', status: 'fail', confirmable: false }), true).actions, [])
  const attention = itemView(item({ key: 'own_template', kind: 'auto', status: 'attention' }), true)
  assert.deepEqual(attention.actions, ['confirm'])
  assert.equal(attention.noteRequired, true)
  assert.equal(attention.confirmLabel, '确认知悉')
  assert.deepEqual(itemView(item({ key: 'own_template', kind: 'auto', status: 'confirmed' }), true).actions, ['withdraw'])
})

test('人工项：待确认、已确认、未通过、不适用各自可做的操作', () => {
  assert.deepEqual(itemView(item(), true).actions, ['confirm', 'fail', 'not_applicable'])
  assert.deepEqual(itemView(item({ status: 'confirmed' }), true).actions, ['withdraw', 'fail', 'not_applicable'])
  assert.deepEqual(itemView(item({ status: 'failed' }), true).actions, ['confirm', 'withdraw', 'not_applicable'])
  assert.deepEqual(itemView(item({ status: 'not_applicable' }), true).actions, ['confirm', 'withdraw', 'fail'])
  assert.equal(itemView(item(), true).noteRequired, false)
})

test('成稿过期后只能撤回，不能新增确认', () => {
  assert.deepEqual(itemView(item(), false).actions, [])
  assert.deepEqual(itemView(item({ status: 'confirmed' }), false).actions, ['withdraw'])
})

test('系统项与人工项分组并保持顺序', () => {
  const { auto, manual } = splitItems(checklist({ items: [
    item({ key: 'final_draft', kind: 'auto' }), item({ key: 'cover_check' }), item({ key: 'toc_pages', kind: 'auto' }), item({ key: 'signature_seal' }),
  ] }))
  assert.deepEqual(auto.map((i) => i.key), ['final_draft', 'toc_pages'])
  assert.deepEqual(manual.map((i) => i.key), ['cover_check', 'signature_seal'])
})

test('回读摘要：页码逐处核对才说已核对，未全部一致时给出数字', () => {
  const sc = { pages: 12, toc_entries: 9, toc_issues: 0, score_rows: 2, page_refs: 10, page_refs_verified: 10, page_ref_issues: 0, verified_against: 'pdf',
    metadata_checked: true, metadata_issues: 0, layout_checked: true, out_of_bounds: 0, blank_pages: 0, footer_issues: 0, image_issues: 0, images: 0, tables: 4 }
  assert.equal(selfCheckNote({ status: 'succeeded', self_check: sc } as never), '12 页 · 评分索引 2 项 · 页码引用 10 处已核对')
  assert.match(selfCheckNote({ status: 'succeeded', self_check: { ...sc, verified_against: 'toc' } } as never), /仅与目录比对/)
  assert.match(selfCheckNote({ status: 'succeeded', self_check: { ...sc, page_refs_verified: 7 } } as never), /页码引用 10 处，核对一致 7 处/)
  assert.equal(selfCheckNote({ status: 'succeeded' } as never), '')
  assert.equal(selfCheckNote({ status: 'failed', self_check: sc } as never), '')
})
