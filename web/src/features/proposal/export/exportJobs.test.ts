import { test } from 'node:test'
import assert from 'node:assert/strict'
import { exportInputSig, exportJobView, exportRequestView, groupExportJobs, keepKeyAfter } from './exportJobs.ts'
import { keyForInput } from './chartJobs.ts'
import { jobView } from '../../execution/agentJobs.ts'
import { resourcesForActivity } from '../../task-workspace/resources.ts'
import { blockerLink } from '../../task-workspace/blocker-links.ts'

const job = (over: Record<string, unknown> = {}) => ({
  id: 7, task_id: 1, outline_id: 2, outline_version: 3, format: 'docx', status: 'succeeded', business_state: 'final_ready', strict_runtime: true,
  export_request_id: 4, executor: 'WPS 12.1.0.29161', office_engine: 'wps', office_version: 'WPS 12.1.0.29161', template_key: 'default', template_hash: 'h',
  rev_snapshot: {}, chart_snapshot: [], matrix_snapshot: [], doc_snapshot: { task_name: 't', outline_version: 3, sections: [], created_at: '' },
  artifact_name: 'a.docx', artifact_hash: 'sha', artifact_size: 10, toc_refreshed: true, warnings: [], error: '', attempts: 1, max_attempts: 3,
  created_by: 'u', created_at: '2026-11-04T08:00:00Z', updated_at: '', ...over,
}) as never

const request = (jobs: unknown[], over: Record<string, unknown> = {}) => ({
  id: 4, task_id: 1, request_key: 'ui-export-1', snapshot_hash: 'abc', formats: ['docx', 'pdf'], template_key: 'default', template_hash: 'h',
  outline_id: 2, outline_version: 3, strict_runtime: true, policy_version: 'export-final/v1', deadline_at: '', created_by: 'u',
  created_at: '2026-11-04T08:00:00Z', updated_at: '', status: 'succeeded', business_state: 'final_ready', jobs, ...over,
}) as never

test('严格最终稿与预览稿分开显示，预览稿不冒充最终交付', () => {
  const final = exportJobView(job())
  assert.equal(final.label, '最终稿')
  assert.equal(final.final, true)
  assert.match(final.note ?? '', /WPS 12\.1/)
  const preview = exportJobView(job({ strict_runtime: false, business_state: 'preview_only', toc_refreshed: false, office_engine: '' }))
  assert.equal(preview.label, '仅预览稿')
  assert.equal(preview.final, false)
  assert.equal(preview.canDownload, true)
  assert.match(preview.note ?? '', /目录页码未刷新/)
})

test('活跃作业可取消、轮询；失败显示错误码含义；取消后不能下载', () => {
  const running = exportJobView(job({ status: 'running', business_state: 'running', attempts: 2, deadline_at: '2026-11-04T09:00:00Z' }))
  assert.equal(running.active, true)
  assert.equal(running.canDownload, false)
  assert.match(running.note ?? '', /第 2\/3 次执行 · 截止/)
  // 暂停退回排队是说明而不是失败；可重试失败回到排队时才显示“上次失败”。
  const paused = exportJobView(job({ status: 'queued', business_state: 'queued', error: '项目已暂停，等待恢复' }))
  assert.match(paused.note ?? '', /等待执行 · 项目已暂停/)
  assert.doesNotMatch(paused.note ?? '', /上次失败/)
  const retrying = exportJobView(job({ status: 'queued', business_state: 'queued', error: '引擎崩溃', error_code: 'EXPORT_FAILED' }))
  assert.match(retrying.note ?? '', /上次失败：引擎崩溃/)
  const failed = exportJobView(job({ status: 'failed', business_state: 'failed', error_code: 'READBACK_FAILED', error: '目录条目没有页码' }))
  assert.equal(failed.tone, 'red')
  assert.match(failed.note ?? '', /成稿回读未通过：目录条目没有页码/)
  assert.equal(exportJobView(job({ status: 'cancelled', business_state: 'cancelled' })).canDownload, false)
})

test('多格式部分失败逐项展示；旧作业没有请求组时单独成组，新者在前', () => {
  const partial = request([job(), job({ id: 8, format: 'pdf', status: 'failed', business_state: 'failed' })], { status: 'partial', business_state: 'partial' })
  assert.equal(exportRequestView(partial).label, '部分完成')
  assert.equal(exportRequestView(request([job()])).label, '最终稿已生成')
  assert.equal(exportRequestView(request([job({ status: 'queued' })], { status: 'queued' })).active, true)
  const legacy = job({ id: 2, export_request_id: undefined, strict_runtime: false, business_state: 'preview_only', created_at: '2026-11-01T08:00:00Z' })
  const groups = groupExportJobs([partial], [job(), legacy])
  assert.deepEqual(groups.map((g) => g.key), ['r4', 'j2'])
  assert.equal(groups[0].jobs.length, 2)
})

test('请求键：同一输入（格式顺序无关）复用，输入变化换新键；服务器明确答复后再次点击换新键', () => {
  let n = 0
  const fresh = () => `k${++n}`
  const sig = exportInputSig({ formats: ['pdf', 'docx'], mode: 'final', expected_outline_version: 3, bidder: ' 甲公司 ' })
  assert.equal(sig, exportInputSig({ formats: ['docx', 'pdf', 'docx'], mode: 'final', expected_outline_version: 3, bidder: '甲公司' }))
  const a = keyForInput(undefined, sig, fresh)
  assert.equal(keyForInput(a, sig, fresh).key, a.key)
  assert.notEqual(keyForInput(a, exportInputSig({ formats: ['docx'], mode: 'preview' }), fresh).key, a.key)
  assert.equal(keepKeyAfter(new TypeError('Failed to fetch')), true) // 网络中断：保留原键，重发时服务器返回原请求组
  assert.equal(keepKeyAfter({ status: 502 }), true)
  assert.equal(keepKeyAfter({ status: 409 }), false)
  assert.equal(keepKeyAfter({ status: 503 }), false)
})

test('导出活动重读门禁与作业；Agent 导出作业显示最终稿业务状态；最终稿阻断可跳转', () => {
  const rs = resourcesForActivity({ worker: 'export', kind: 'state' })
  for (const r of ['export', 'exportJobs', 'agentJobs'] as const) assert.ok(rs.includes(r), r)
  const v = jobView({ id: 1, task_id: 1, intent_id: 7, created_intent_epoch: 1, job_ref: { kind: 'proposal_export', id: 4 }, target_ref: {},
    required: true, state: 'acknowledged', created_by: 'worker', created_at: '', updated_at: '', job_status: 'succeeded', job_attempts: 1,
    job_max_attempts: 3, event: { id: 2, terminal_status: 'succeeded', business_state: 'final_ready', payload: {}, state: 'acknowledged', created_at: '' } } as never)
  assert.equal(v.tone, 'green')
  assert.match(v.status, /严格最终稿已生成/)
  assert.equal(blockerLink(1, { kind: 'chart_not_final', message: 'm', section_id: 5 })?.label, '去渲染章节图表')
  assert.ok(blockerLink(1, { kind: 'office_unavailable', message: 'm' }))
})
