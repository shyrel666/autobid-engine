import { test } from 'node:test'
import assert from 'node:assert/strict'
import { chartStateView, keyForInput, retryRequestKey, splitCharts } from './chartJobs.ts'
import { jobView } from '../../execution/agentJobs.ts'
import { resourcesForActivity } from '../../task-workspace/resources.ts'

const chart = (over: Record<string, unknown> = {}) => ({
  id: 5, task_id: 1, section_id: 2, section_code: '5', section_title: '实施进度', section_rev_id: 9, rev_no: 3, current_rev_no: 3,
  stale: false, chart_type: 'gantt', title: '实施进度计划', spec: {}, source_format: 'mermaid', source: 'gantt', source_hash: 'h',
  render_status: 'pending', render_format: '', render_error: '', artifact_hash: '', artifact_size: 0, width_px: 0, height_px: 0,
  created_by: 'u', updated_by: 'u', created_at: '', updated_at: '', ...over,
}) as never

const job = (status: string, over: Record<string, unknown> = {}) =>
  ({ id: 11, status, attempts: 1, max_attempts: 3, deadline_at: '', created_by: 'u', created_at: '', ...over })

test('活跃渲染作业可取消、轮询刷新，不能并发重试', () => {
  const v = chartStateView(chart({ latest_job: job('running', { attempts: 2, error: 'chrome crashed' }) }))
  assert.equal(v.active, true)
  assert.equal(v.canRetry, false)
  assert.equal(v.tone, 'blue')
  assert.match(v.note ?? '', /第 2\/3 次.*chrome crashed/)
})

test('旧修订图表标为已过期，不显示为当前图，也不能按旧修订重试', () => {
  const old = chart({ stale: true, rev_no: 2, render_status: 'rendered', render_format: 'png', stale_reason: '章节 5 已更新到 rev 3' })
  const v = chartStateView(old)
  assert.equal(v.label, '已过期')
  assert.equal(v.canRetry, false)
  const { current, stale } = splitCharts([old, chart()])
  assert.equal(current.length, 1)
  assert.equal(stale.length, 1)
})

test('取消或失败后可重试；成功图表的重新渲染失败保留原图', () => {
  assert.equal(chartStateView(chart({ latest_job: job('cancelled') })).label, '已取消')
  assert.equal(chartStateView(chart({ latest_job: job('cancelled') })).canRetry, true)
  const failed = chartStateView(chart({ render_status: 'failed', latest_job: job('failed', { error: 'RENDER' }) }))
  assert.equal(failed.tone, 'red')
  assert.equal(failed.canRetry, true)
  const kept = chartStateView(chart({ render_status: 'rendered', render_format: 'png', latest_job: job('failed', { error: 'timeout' }) }))
  assert.equal(kept.label, '已渲染')
  assert.match(kept.note ?? '', /保留原有渲染结果/)
  assert.equal(chartStateView(chart({ chart_type: 'equipment_table', source_format: 'json', render_status: 'rendered', render_format: 'native' })).canRetry, false)
})

test('请求键：同一输入复用，输入变化或作业变化后换新键', () => {
  let n = 0
  const fresh = () => `k${++n}`
  const a = keyForInput(undefined, 'sig-1', fresh)
  assert.equal(keyForInput(a, 'sig-1', fresh).key, a.key)
  assert.notEqual(keyForInput(a, 'sig-2', fresh).key, a.key)
  const c = chart({ latest_job: job('failed') })
  assert.equal(retryRequestKey(c), retryRequestKey(c))
  assert.notEqual(retryRequestKey(c), retryRequestKey(chart({ latest_job: job('failed', { id: 12 }) })))
  assert.ok(!retryRequestKey(c).startsWith('sys:'))
})

test('图表渲染活动重读图表、导出门禁与作业列表；Agent 图表作业显示业务状态', () => {
  const rs = resourcesForActivity({ worker: 'chart-render', kind: 'state' })
  for (const r of ['charts', 'export', 'agentJobs'] as const) assert.ok(rs.includes(r), r)
  const stale = jobView({ id: 1, task_id: 1, intent_id: 7, created_intent_epoch: 1, job_ref: { kind: 'chart_render', id: 4 }, target_ref: {},
    required: true, state: 'acknowledged', created_by: 'worker', created_at: '', updated_at: '', job_status: 'succeeded', job_attempts: 1,
    job_max_attempts: 3, event: { id: 2, terminal_status: 'succeeded', business_state: 'chart_stale', payload: {}, state: 'acknowledged', created_at: '' } } as never)
  assert.equal(stale.title, '图表渲染 #4')
  assert.equal(stale.tone, 'amber')
  assert.match(stale.status, /已过期/)
})
