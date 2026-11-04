import { test } from 'node:test'
import assert from 'node:assert/strict'
import { executorLabel, gapText, isAgentJob, keyForInput, locatorText, parseJobView, profileOptions, recordStatusView } from './parseJobs.ts'
import { jobView } from '../../execution/agentJobs.ts'
import { resourcesForActivity } from '../../task-workspace/resources.ts'

const job = (status: string, over: Record<string, unknown> = {}) =>
  ({ id: 7, task_id: 1, file_id: 3, input: { request_key: 'k' }, status, attempts: 1, max_attempts: 3, created_at: '', ...over }) as never
const record = (status: string, over: Record<string, unknown> = {}) =>
  ({ id: 9, file_id: 3, operation: 'import', status, expected_revision: 0, created_at: '', approved_by: '', result: {}, ...over }) as never

test('处理中、候选待核对、已采用、已拒绝、失败与取消分开显示，作业成功不等于已导入', () => {
  assert.deepEqual([parseJobView(job('queued')).label, parseJobView(job('queued')).canCancel], ['排队中', true])
  const ready = parseJobView(job('succeeded', { record_id: 9, record_status: 'ready' }))
  assert.equal(ready.label, '候选待核对')
  assert.equal(ready.tone, 'amber')
  assert.equal(ready.canReview, true)
  assert.equal(parseJobView(job('succeeded', { record_status: 'adopted' })).tone, 'green')
  assert.equal(parseJobView(job('succeeded', { record_status: 'rejected' })).label, '候选已拒绝')
  const failed = parseJobView(job('failed', { error_code: 'RUNTIME_UNAVAILABLE', error: 'pinned v1, now v2' }))
  assert.match(failed.label, /OCR 不可用或版本已变化/)
  assert.equal(failed.canRetry, true)
  assert.match(parseJobView(job('failed', { error_code: 'BUDGET_EXHAUSTED' })).label, /耗尽/)
  assert.equal(parseJobView(job('cancelled', { error: '用户取消' })).note, undefined)
  const retry = parseJobView(job('queued', { error_code: 'RUNTIME_UNAVAILABLE', attempts: 1 }))
  assert.equal(retry.label, '等待重试')
  assert.match(retry.note ?? '', /原截止时间内重试/)
})

test('OCR 识别结果标明未经人工核验；拒绝记录显示拒绝人与原因', () => {
  const ocr = recordStatusView(record('ready', { parser_version: 'fixed-parser-v1+ocr:paddleocr 3.7.0; paddle 3.3.1' }))
  assert.equal(ocr.label, '导入候选待核对')
  assert.match(ocr.note ?? '', /未经人工核验/)
  assert.equal(recordStatusView(record('ready', { parser_version: 'fixed-parser-v1' })).note, undefined)
  const rej = recordStatusView(record('rejected', { rejected_by: '王工', reject_reason: '页码不符' }))
  assert.match(rej.note ?? '', /王工.*页码不符/)
  assert.equal(recordStatusView(record('ready', { operation: 'reparse' })).label, '解析修订待核对')
})

test('解析配置：增强后端不出现；OCR 不可用时禁用全部 OCR，检查中仍可提交', () => {
  const values = profileOptions(undefined).map((o) => o.value)
  assert.deepEqual(values, ['auto', 'native', 'ocr_only'])
  assert.equal(profileOptions(undefined).find((o) => o.value === 'ocr_only')?.disabled, true)
  const healthy = { capability: 'ocr', label: 'OCR', state: 'healthy', available: true } as never
  assert.equal(profileOptions(healthy).find((o) => o.value === 'ocr_only')?.disabled, false)
  const starting = { capability: 'ocr', label: 'OCR', state: 'starting', available: false } as never
  assert.equal(profileOptions(starting).find((o) => o.value === 'ocr_only')?.disabled, false)
})

test('执行版本、原件定位与缺口的文字说明', () => {
  assert.equal(executorLabel('fixed-parser-v1'), 'fixed-parser-v1（不使用 OCR）')
  assert.equal(executorLabel('fixed-parser-v1+ocr:paddleocr 3.7.0; paddle 3.3.1; PP-OCRv5_mobile_det@3206; PP-OCRv5_mobile_rec@b9c2; cpu'),
    'fixed-parser-v1 + paddleocr 3.7.0（PP-OCRv5_mobile_det / PP-OCRv5_mobile_rec）')
  assert.match(executorLabel(undefined), /旧作业/)
  assert.equal(locatorText({ file_id: 1, sha256: 'h', page_start: 2, page_end: 2, page_is_physical: true, ocr: true, ocr_lines: 18, min_confidence: 0.973 }),
    '第 2 页 · OCR 18 行 · 最低置信度 0.97')
  assert.equal(locatorText({ file_id: 1, sha256: 'h', page_is_physical: false, source_locator: { sheet: '报价表', range: 'A1:D40' } }), '工作表「报价表」 A1:D40')
  assert.equal(gapText({ page: 3, kind: 'ocr_not_requested' }), '第 3 页：扫描页未识别（仅文本层配置）')
  assert.equal(isAgentJob({ created_by: 'worker' }), true)
})

test('请求键：同一输入重发复用，输入变化换新键', () => {
  const keys = new Map<string, string>()
  let n = 0
  const make = () => `k${++n}`
  const a = keyForInput(keys, { file: 1, profile: 'auto' }, make)
  assert.equal(keyForInput(keys, { file: 1, profile: 'auto' }, make), a)
  assert.notEqual(keyForInput(keys, { file: 1, profile: 'ocr_only' }, make), a)
})

test('解析作业事件重读文档、要求、大纲与导出门禁；被拒绝的候选在执行页不显示为成功', () => {
  const res = resourcesForActivity({ worker: 'file-processing', kind: 'state' })
  for (const r of ['documents', 'requirements', 'outline', 'export', 'agentJobs']) assert.ok(res.includes(r as never), r)
  const v = jobView({ job_ref: { kind: 'tender_parse', id: 7 }, job_status: 'succeeded', job_attempts: 1, job_max_attempts: 3, state: 'acknowledged',
    event: { business_state: 'rejected' } } as never)
  assert.equal(v.tone, 'amber')
  assert.match(v.status, /候选已被人工拒绝/)
})
