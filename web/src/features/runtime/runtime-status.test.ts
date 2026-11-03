import { test } from 'node:test'
import assert from 'node:assert/strict'
import { runtimeNeedsPoll, summarizeRuntime } from './runtime-status.ts'

const cap = (capability: string, state: string) => ({ capability, state, label: capability, available: state === 'healthy' || state === 'degraded' }) as never

test('读取前不判定可用', () => {
  assert.equal(summarizeRuntime(undefined).tone, 'slate')
})

test('关键能力异常优先于检查中与未配置', () => {
  const s = summarizeRuntime([cap('office', 'self_test_failed'), cap('mermaid', 'starting'), cap('ocr', 'unconfigured')])
  assert.equal(s.tone, 'red')
  assert.deepEqual(s.problems.map((c: { capability: string }) => c.capability), ['office', 'ocr'])
})

test('检查中与未配置分别可见', () => {
  assert.equal(summarizeRuntime([cap('office', 'starting'), cap('mermaid', 'healthy'), cap('ocr', 'healthy')]).tone, 'blue')
  const s = summarizeRuntime([cap('office', 'healthy'), cap('mermaid', 'healthy'), cap('ocr', 'unconfigured')])
  assert.equal(s.tone, 'amber')
  assert.equal(s.label, '1 项运行能力未配置')
})

test('可选能力未配置不影响顶栏汇总', () => {
  const s = summarizeRuntime([cap('office', 'healthy'), cap('mermaid', 'healthy'), cap('ocr', 'healthy'), cap('enhanced_pdf', 'unconfigured'), cap('embedding', 'self_test_failed')])
  assert.equal(s.tone, 'green')
})

test('只有自检进行中才轮询', () => {
  assert.equal(runtimeNeedsPoll([cap('ocr', 'starting')]), true)
  assert.equal(runtimeNeedsPoll([cap('ocr', 'unconfigured')]), false)
  assert.equal(runtimeNeedsPoll(undefined), false)
})
