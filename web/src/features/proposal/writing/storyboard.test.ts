import assert from 'node:assert/strict'
import { test } from 'node:test'
import { admissibleOptions, commitmentOptions, insertAt, storyboardForm, storyboardRequest, toggleId } from './storyboard.ts'

const run = (over = {}) => ({ id: 1, status: 'current', requirement_code: 'R-1', requirement_title: '资质', items: [{ id: 7, verdict: 'admissible' }, { id: 8, verdict: 'manual' }], ...over })

test('form has one row per mapped requirement and restores saved text', () => {
  const sb = { scoring_points: [{ requirement_code: 'R-1', scoring_point: 'x', our_approach: '做法', buyer_benefit: '收益' }], theme_sentence: '论点', admissibility_item_ids: [7], commitment_ids: [3] }
  const f = storyboardForm([{ code: 'R-1', title: '资质' }, { code: 'R-2', title: '工期' }], sb as never)
  assert.deepEqual(f.points.map((p) => [p.code, p.approach]), [['R-1', '做法'], ['R-2', '']])
  assert.deepEqual([f.theme, f.itemIds, f.commitmentIds], ['论点', [7], [3]])
})

test('sections without mapped requirements still get a point, or submit would be refused', () => {
  const f = storyboardForm([])
  assert.equal(f.points.length, 1)
  assert.equal(f.points[0].code, '')
})

test('request trims text and carries references', () => {
  const r = storyboardRequest({ theme: ' 论点 ', points: [{ code: 'R-1', title: '资质', approach: ' 做 ', benefit: ' 益 ' }], itemIds: [7], commitmentIds: [] })
  assert.equal(r.theme_sentence, '论点')
  assert.deepEqual(r.scoring_points[0], { requirement_code: 'R-1', scoring_point: '资质', our_approach: '做', buyer_benefit: '益' })
  assert.deepEqual(r.admissibility_item_ids, [7])
})

test('only current admissible evidence and confirmed commitments can be referenced', () => {
  assert.deepEqual(admissibleOptions([run(), run({ status: 'stale' })] as never).map((o) => o.token), ['[[EVID:7]]'])
  const c = (status: string, id: number) => ({ id, status, code: `C${id}`, metric_label: '工期', committed_raw: '90', unit: '天' })
  assert.deepEqual(commitmentOptions([c('draft', 1), c('confirmed', 2), c('signed_off', 3), c('rejected', 4)] as never).map((o) => o.id), [2, 3])
})

test('toggle and insert', () => {
  assert.deepEqual(toggleId([1], 2, true), [1, 2])
  assert.deepEqual(toggleId([1, 2], 1, false), [2])
  assert.deepEqual(toggleId([1], 1, true), [1])
  assert.deepEqual(insertAt('ab', 'X', 1, 1), { text: 'aXb', cursor: 2 })
  assert.deepEqual(insertAt('ab', 'X', undefined, undefined), { text: 'abX', cursor: 3 })
  assert.deepEqual(insertAt('abc', 'X', 1, 2), { text: 'aXc', cursor: 2 })
})
