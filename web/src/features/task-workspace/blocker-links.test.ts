import { test } from 'node:test'
import assert from 'node:assert/strict'
import { blockerLink } from './blocker-links.ts'

const b = (kind: string, extra: Record<string, number> = {}) => ({ kind, message: 'm', ...extra })

test('每种阻断都有确定的修复位置', () => {
  assert.equal(blockerLink(1, b('no_approved_outline'))?.href, '/tasks/1/outline')
  assert.equal(blockerLink(1, b('requirements_not_confirmed'))?.href, '/tasks/1/requirements')
  assert.equal(blockerLink(1, b('requirement_unmapped', { requirement_item_id: 8 }))?.href, '/tasks/1/outline?requirement=8')
  assert.equal(blockerLink(1, b('veto_pending', { requirement_item_id: 8 }))?.href, '/tasks/1/requirements?filter=veto&requirement=8')
  assert.equal(blockerLink(1, b('veto_failed'))?.href, '/tasks/1/requirements?filter=veto')
  assert.equal(blockerLink(1, b('section_not_approved', { section_id: 4 }))?.href, '/tasks/1/writing/4')
  assert.equal(blockerLink(1, b('section_not_approved'))?.href, '/tasks/1/writing')
  for (const k of ['matrix_stale', 'matrix_failed', 'matrix_unverified'])
    assert.equal(blockerLink(1, b(k, { requirement_item_id: 2, section_id: 3 }))?.href, '/tasks/1/matrix?requirement=2&section=3')
  assert.equal(blockerLink(1, b('consistency_not_run'))?.href, '/tasks/1/consistency')
  assert.equal(blockerLink(1, b('consistency_stale'))?.href, '/tasks/1/consistency')
  assert.equal(blockerLink(1, b('finding_open', { finding_id: 6 }))?.href, '/tasks/1/consistency?finding=6')
  assert.equal(blockerLink(1, b('evidence_invalid', { section_id: 4, evidence_id: 9 }))?.href, '/tasks/1/writing/4?evidence=9&panel=evidence')
  assert.equal(blockerLink(1, b('evidence_invalid', { requirement_item_id: 2 }))?.href, '/tasks/1/requirements?requirement=2')
  assert.equal(blockerLink(1, b('evidence_invalid'))?.href, '/tasks/1/research')
})

test('缺失或零值标识不构造无效链接，未知类型不猜测', () => {
  assert.equal(blockerLink(1, b('section_not_approved', { section_id: 0 }))?.href, '/tasks/1/writing')
  assert.equal(blockerLink(1, b('something_new')), null)
})
