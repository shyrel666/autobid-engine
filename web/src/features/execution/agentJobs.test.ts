import { test } from 'node:test'
import assert from 'node:assert/strict'
import { blockedNotice, isWaitingWork, jobView } from './agentJobs.ts'
import { resourcesForActivity } from '../task-workspace/resources.ts'

const job = (over: Record<string, unknown> = {}) => ({
  id: 1, task_id: 1, intent_id: 7, created_intent_epoch: 1, job_ref: { kind: 'tender_parse', id: 42 }, target_ref: {},
  required: true, state: 'waiting', created_by: 'worker', created_at: '', updated_at: '',
  job_status: 'queued', job_attempts: 0, job_max_attempts: 3, ...over,
}) as never

test('等待后台处理与执行失败分开', () => {
  assert.equal(isWaitingWork({ state: 'blocked', blocked_reason: 'waiting_job' }), true)
  assert.equal(isWaitingWork({ state: 'blocked', blocked_reason: 'required_job_failed' }), false)
  assert.equal(isWaitingWork({ state: 'blocked' }), false)
  assert.equal(blockedNotice('waiting_job')?.tone, 'info')
  assert.equal(blockedNotice('required_job_failed')?.tone, 'danger')
  assert.equal(blockedNotice(undefined), null)
})

test('作业状态与业务状态分开，预览稿不显示为最终成功', () => {
  const waiting = jobView(job())
  assert.equal(waiting.title, '招标解析 #42')
  assert.equal(waiting.tone, 'blue')
  assert.match(waiting.detail[0], /等待结果/)
  const preview = jobView(job({ job_ref: { kind: 'proposal_export', id: 9 }, job_status: 'succeeded', state: 'acknowledged',
    event: { id: 3, terminal_status: 'succeeded', business_state: 'preview_only', payload: {}, state: 'acknowledged', created_at: '' } }))
  assert.equal(preview.tone, 'amber')
  assert.match(preview.status, /仅预览稿/)
  const failed = jobView(job({ job_status: 'failed', job_error: 'OCR 未配置' }))
  assert.equal(failed.tone, 'red')
  assert.ok(failed.detail.some((d) => d.includes('OCR 未配置')))
})

test('作业与等待/唤醒活动触发作业列表重读', () => {
  for (const worker of ['agent-jobs', 'file-processing', 'export']) {
    assert.ok(resourcesForActivity({ worker, kind: 'state' }).includes('agentJobs'), worker)
  }
  assert.ok(resourcesForActivity({ worker: 'agent-jobs', kind: 'state' }).includes('overview'))
  assert.ok(!resourcesForActivity({ worker: 'work#1', kind: 'tool' }).includes('agentJobs'))
})
