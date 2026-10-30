import { test } from 'node:test'
import assert from 'node:assert/strict'
import { deriveSteps, nextStep } from './progress.ts'

const reqs = (over: Record<string, unknown> = {}, veto = { total: 2, passed: 2, failed: 0, pending: 0 }, manifest = 'confirmed') => ({
  manifest: { status: manifest, epoch: 3 },
  stats: { total: 10, confirmed: 10, needs_revision: 0, veto, ...over },
  items: [], scoring: [],
}) as never

test('读取失败与未加载不显示为完成', () => {
  const steps = deriveSteps(1, { requirements: { error: 'boom' }, outlines: {}, approvedOutline: {}, readiness: {} })
  assert.equal(steps[0].state, 'error')
  assert.ok(steps.every((s) => s.state !== 'complete'))
  assert.equal(nextStep(steps)?.key, 'requirements')
})

test('零要求是空态而不是 100% 完成', () => {
  const steps = deriveSteps(1, {
    requirements: { data: reqs({ total: 0, confirmed: 0 }, { total: 0, passed: 0, failed: 0, pending: 0 }, 'draft') },
    outlines: {}, approvedOutline: {}, readiness: {},
  })
  assert.equal(steps[0].state, 'empty')
  assert.equal(steps[1].state, 'empty')
})

test('无批准大纲时核验与导出步骤不被判完成', () => {
  const steps = deriveSteps(1, {
    requirements: { data: reqs() },
    outlines: { data: { outlines: [], approved_version: 0, open_version: 0 } as never },
    approvedOutline: { data: null },
    readiness: { data: { ready: false, blockers: [{ kind: 'no_approved_outline', message: 'x' }] } as never },
  })
  const by = Object.fromEntries(steps.map((s) => [s.key, s.state]))
  assert.equal(by.outline, 'empty')
  assert.equal(by.matrix, 'unknown')
  assert.equal(by.consistency, 'unknown')
  assert.equal(by.export, 'action_required')
  assert.equal(nextStep(steps)?.key, 'outline')
})

test('失效清单、章节与核验显示为 stale，不沿用绿色状态', () => {
  const steps = deriveSteps(1, {
    requirements: { data: reqs({}, undefined, 'stale') },
    outlines: { data: { outlines: [{ version: 2, status: 'approved' }], approved_version: 2, open_version: 0 } as never },
    approvedOutline: { data: { sections: [{ status: 'approved', current_rev_no: 1 }, { status: 'stale_evidence', current_rev_no: 2 }] } as never },
    readiness: { data: { ready: false, blockers: [{ kind: 'matrix_stale', message: 'm' }] } as never },
  })
  const by = Object.fromEntries(steps.map((s) => [s.key, s.state]))
  assert.equal(by.requirements, 'stale')
  assert.equal(by.sections, 'stale')
  assert.equal(by.matrix, 'stale')
  assert.equal(by.consistency, 'complete')
})
