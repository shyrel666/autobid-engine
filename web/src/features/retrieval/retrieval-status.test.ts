import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { KnowledgeIndex, ResearchRequest, RetrievalDiagnostics, RetrievalStatus } from '../../api/types.ts'
import {
  buildProgress, canResumeResearch, degradationSummary, degradedReasonText, evidenceSummary, hitSourceText, indexBuildFailureText,
  indexStateText,
  indexBuildSignature, indexRequestKey, indexStateLabel, indexStateTone, newIndexRequestKey, researchGapText,
  retrievalErrorText, retrievalStateLabel, retrievalStateTone, runStatusLabel, runStatusTone, statusNeedsPoll, statusSummary,
} from './retrieval-status.ts'

function status(overrides: Partial<RetrievalStatus> = {}): RetrievalStatus {
  const model = (capability: 'embedding' | 'reranker') => ({
    configured: true,
    health: { capability, state: 'healthy' as const, label: '', available: true },
    info: { id: 'test-model@r1', name: 'test-model', revision: 'r1', format: 'tei', dimension: 512,
      hosting: 'local' as const, timeout_seconds: 20, tokens_used: 0 },
  })
  return { state: 'semantic', reasons: [], index_state: 'current', epoch: 1, chunks: 100, strategy: 'test',
    preprocessing: 'prep-v1', multi_agent_enabled: false, embedding: model('embedding'), reranker: model('reranker'),
    indexes: [], checked_at: '2026-11-06T00:00:00Z', ...overrides }
}

const progress = (embedded: number, reused: number, total: number) => ({ embedded_chunks: embedded, reused_chunks: reused, total_chunks: total })
const diagnostics = (mode: 'hybrid' | 'lexical_only'): RetrievalDiagnostics => ({ mode, index_state: 'current', fused_candidates: 10, reranked_candidates: 10 })

for (const [state, label, tone] of [
  ['semantic', '语义检索正常', 'green'], ['degraded', '部分降级', 'amber'],
  ['lexical_only', '仅全文检索', 'amber'], ['empty', '知识库暂无文档', 'slate'],
] as const) {
  test(`检索状态 ${state} 显示业务文案和颜色`, () => {
    assert.equal(retrievalStateLabel(state), label)
    assert.equal(retrievalStateTone(state), tone)
    const summary = statusSummary(status({ state }))
    assert.equal(summary.title, label)
    assert.equal(summary.tone, tone)
    assert.ok(summary.detail)
    assert.ok(summary.nextAction)
    if (state === 'empty') assert.match(summary.nextAction, /上传/)
  })
}

const reasons = [
  'semantic_model_unconfigured', 'semantic_index_missing_or_stale', 'semantic_channel_failed',
  'reranker_unconfigured', 'reranker_failed', 'rerank_truncated', 'index_missing', 'index_model_changed',
  'index_dimension_changed', 'index_preprocessing_changed', 'index_documents_changed', 'index_unconfigured',
  'embedding_degraded', 'embedding_self_test_failed', 'embedding_starting',
  'reranker_degraded', 'reranker_self_test_failed', 'reranker_starting',
]
for (const reason of reasons) {
  test(`降级原因 ${reason} 已翻译且不回显原始码`, () => {
    const text = degradedReasonText(reason)
    assert.notEqual(text, '检索能力受限')
    assert.match(text, /[\u4e00-\u9fff]/)
    assert.ok(!text.includes(reason))
    const summary = statusSummary(status({ state: 'degraded', reasons: [reason] }))
    assert.ok(summary.detail.includes(text))
    assert.ok(!JSON.stringify(summary).includes(reason))
  })
}

test('未知状态与未知降级码保守展示，不伪装正常或泄漏代码', () => {
  for (const code of ['future_reason', '', '__proto__', 'toString']) assert.equal(degradedReasonText(code), '检索能力受限')
  assert.equal(retrievalStateTone('future'), 'slate')
  assert.equal(retrievalStateLabel('future'), '检索状态待确认')
  assert.equal(statusSummary(undefined).tone, 'slate')
  assert.equal(statusSummary(undefined).title, '检索状态待确认')
  assert.equal(indexStateLabel('future'), '索引状态待确认')
  assert.equal(runStatusLabel('future'), '检索记录状态待确认')
})

test('索引变化的下一步按实际原因指导重建与启用', () => {
  const next = (reason: string) => statusSummary(status({ state: 'lexical_only', reasons: ['semantic_index_missing_or_stale', reason] })).nextAction
  assert.match(next('index_documents_changed'), /文档已更新.*重建.*未变化的内容不会重新计算/)
  assert.match(next('index_missing'), /语义索引页构建并启用/)
  assert.match(next('index_model_changed'), /模型已更换.*新模型重建后启用/)
  assert.match(next('index_dimension_changed'), /配置已变化.*重建/)
  assert.match(next('index_preprocessing_changed'), /配置已变化.*重建/)
  assert.match(next('index_unconfigured'), /管理员配置/)
  assert.match(statusSummary(status({ state: 'lexical_only', reasons: ['semantic_model_unconfigured'] })).nextAction, /管理员配置/)
})

test('语义模型异常退回全文，排序异常保留语义检索的业务边界', () => {
  for (const reason of ['embedding_degraded', 'embedding_self_test_failed', 'embedding_starting', 'semantic_channel_failed']) {
    assert.match(statusSummary(status({ state: 'degraded', reasons: [reason] })).nextAction, /自动退回全文检索.*恢复后自动恢复/)
  }
  for (const reason of ['reranker_degraded', 'reranker_self_test_failed', 'reranker_starting', 'reranker_failed']) {
    const next = statusSummary(status({ state: 'degraded', reasons: [reason] })).nextAction
    assert.match(next, /基础排序.*自动恢复/)
    assert.ok(!next.includes('退回全文'))
  }
  assert.match(statusSummary(status({ state: 'degraded', reasons: ['reranker_unconfigured'] })).nextAction, /管理员启用/)
})

for (const [state, label, tone] of [
  ['current', '当前生效', 'green'], ['rollback_ready', '可切换/回滚', 'blue'],
  ['documents_changed', '文档已变化，需重建', 'amber'], ['model_mismatch', '模型不一致', 'amber'],
  ['dimension_mismatch', '维度不一致', 'amber'], ['preprocessing_mismatch', '预处理版本不一致', 'amber'],
  ['queued', '排队中', 'blue'], ['building', '构建中', 'blue'], ['failed', '失败', 'red'], ['cancelled', '已取消', 'slate'],
] as const) {
  test(`索引状态 ${state} 已翻译`, () => {
    assert.equal(indexStateLabel(state), label)
    assert.equal(indexStateTone(state), tone)
  })
}

test('构建进度累计新编码与复用分块，总量为零时不除零', () => {
  assert.deepEqual(buildProgress(progress(40, 80, 500)), { percent: 24, label: '已处理 120 / 500 分块（复用 80）' })
  assert.deepEqual(buildProgress(progress(0, 0, 0)), { percent: 0, label: '已处理 0 / 0 分块（复用 0）' })
  assert.equal(buildProgress(progress(10, 0, 0)).percent, 0)
  assert.equal(buildProgress(progress(20, 80, 100)).percent, 100)
  assert.equal(buildProgress(progress(10, 100, 100)).percent, 100)
  assert.equal(buildProgress(progress(999, 0, 1000)).percent, 99)
  assert.equal(buildProgress(progress(1, 0, 3)).percent, 33)
  assert.equal(buildProgress(progress(-5, -2, 100)).percent, 0)
  assert.equal(buildProgress(progress(NaN, Infinity, 0)).percent, 0)
})

for (const [state, label] of [
  ['running', '进行中'], ['completed', '已完成'], ['partial', '部分完成（有缺口）'],
  ['failed', '失败'], ['cancelled', '已取消'], ['budget_exhausted', '预算耗尽'],
] as const) {
  test(`检索记录状态 ${state} 已翻译`, () => {
    assert.equal(runStatusLabel(state), label)
    assert.ok(runStatusTone(state))
  })
}

test('证据摘要分别统计已核验、待核验与来源失效，未知状态仍待核验', () => {
  assert.deepEqual(evidenceSummary(undefined), { verified: 0, pending: 0, stale: 0 })
  assert.deepEqual(evidenceSummary([]), { verified: 0, pending: 0, stale: 0 })
  assert.deepEqual(evidenceSummary(['verified', 'unverified', 'stale', 'verified', 'future'].map((s) => ({ status: s }))), { verified: 2, pending: 2, stale: 1 })
})

test('只有活跃构建或模型准备中才轮询', () => {
  assert.equal(statusNeedsPoll(undefined), false)
  assert.equal(statusNeedsPoll(status()), false)
  for (const buildStatus of ['queued', 'building'] as const) {
    assert.equal(statusNeedsPoll(status({ active_build: { status: buildStatus } as KnowledgeIndex })), true)
  }
  for (const model of ['embedding', 'reranker'] as const) {
    for (const state of ['unconfigured', 'starting', 'healthy', 'degraded', 'self_test_failed'] as const) {
      const s = status()
      s[model].health.state = state
      assert.equal(statusNeedsPoll(s), state === 'starting')
    }
  }
})

test('一次检索的降级提示按实际通道工作情况展示并去重', () => {
  assert.deepEqual(degradationSummary(null), { limited: false, lexicalOnly: false, reasons: [] })
  assert.equal(degradationSummary([], diagnostics('lexical_only')).lexicalOnly, true)
  assert.equal(degradationSummary([], diagnostics('lexical_only')).limited, true)
  assert.equal(degradationSummary(['semantic_channel_failed']).lexicalOnly, true)
  assert.equal(degradationSummary(['reranker_failed'], diagnostics('hybrid')).lexicalOnly, false)
  assert.equal(degradationSummary(['reranker_failed'], diagnostics('hybrid')).limited, true)
  assert.equal(degradationSummary([], diagnostics('hybrid')).limited, false)
  assert.equal(degradationSummary(['unknown', 'unknown', 'another']).reasons.length, 1)
  assert.deepEqual(degradationSummary(['unknown']).reasons, ['检索能力受限'])
})

test('同一构建的重复点击与响应丢失重试复用键，确认后新操作换键', () => {
  let calls = 0
  const fresh = () => `request-${++calls}`
  const initial = indexRequestKey(undefined, 'input-A', fresh)
  assert.equal(indexRequestKey(initial, 'input-A', fresh), initial)
  assert.equal(indexRequestKey(initial, 'input-A', fresh).key, 'request-1')
  assert.equal(calls, 1)
  assert.equal(indexRequestKey(initial, 'input-B', fresh).key, 'request-2')
  // 确认服务端已登记（包括终态重放）后清空 previous，下一次是新构建。
  assert.equal(indexRequestKey(undefined, 'input-A', fresh).key, 'request-3')
})

test('模型、维度、预处理变化均视为新构建，已声明维度时忽略人工输入', () => {
  const initial = status(), signature = indexBuildSignature(initial)
  assert.equal(indexBuildSignature(initial, 1024), signature)
  for (const change of ['model', 'dimension', 'preprocessing']) {
    const changed = status()
    if (change === 'model') changed.embedding.info!.id = 'other@revision2'
    if (change === 'dimension') changed.embedding.info!.dimension = 1024
    if (change === 'preprocessing') changed.preprocessing = 'prep-v2'
    assert.notEqual(indexBuildSignature(changed), signature)
    const key = indexRequestKey({ signature, key: 'old' }, indexBuildSignature(changed), () => 'new')
    assert.equal(key.key, 'new')
  }
  const manual = status()
  delete manual.embedding.info!.dimension
  assert.notEqual(indexBuildSignature(manual, 512), indexBuildSignature(manual, 1024))
})

test('新请求键唯一且不使用服务端保留前缀', () => {
  const first = newIndexRequestKey(), second = newIndexRequestKey()
  assert.notEqual(first, second)
  assert.match(first, /^ui:index:/)
  assert.ok(!first.startsWith('sys:'))
  assert.ok(first.length < 128)
})

test('多 Agent 未启用时不能继续协作请求，不改写原请求造成幂等冲突', () => {
  const request: ResearchRequest = { request_key: 'original', question: '原问题', multi_agent: true }
  assert.equal(canResumeResearch(request, false), false)
  assert.equal(canResumeResearch(request, true), true)
  assert.deepEqual(request, { request_key: 'original', question: '原问题', multi_agent: true })
  assert.equal(canResumeResearch({ request_key: 'single', question: '问题' }, false), true)
  assert.equal(canResumeResearch({ ...request, multi_agent: false }, false), true)
})

test('既有缺口的降级码与英文证据状态转成业务语言，业务陈述保留', () => {
  assert.equal(researchGapText('reranker_failed'), degradedReasonText('reranker_failed'))
  assert.equal(researchGapText('检索降级：unknown'), '检索能力受限')
  assert.equal(researchGapText('retrieval degraded: embedding_degraded'), degradedReasonText('embedding_degraded'))
  assert.match(researchGapText('no evidence found'), /尚未找到证据/)
  assert.match(researchGapText('evidence 3: missing (stale)'), /证据 #3.*来源已失效/)
  assert.match(researchGapText('evidence 4: pending (unverified)'), /证据 #4.*尚待核验/)
  assert.match(researchGapText('evidence 5: mismatch (verified)'), /证据 #5.*不足以支持/)
  assert.match(researchGapText('context deadline exceeded'), /部分检索未能完成/)
  assert.equal(researchGapText('需要补充软件版本的证明材料'), '需要补充软件版本的证明材料')
})

test('索引 API 的全部业务错误码均有业务提示，不直接显示服务端错误', () => {
  const codes = ['runtime_unavailable', 'build_active', 'idempotency_conflict', 'nothing_to_index', 'model_unconfigured',
    'model_mismatch', 'dimension_mismatch', 'preprocessing_mismatch', 'not_ready', 'documents_changed',
    'coverage_incomplete', 'already_terminal', 'index_in_use']
  for (const code of codes) {
    const text = retrievalErrorText({ code, message: 'raw internal error' })
    assert.ok(!text.includes(code))
    assert.ok(!text.includes('raw internal error'))
    assert.notEqual(text, retrievalErrorText(undefined))
  }
  assert.match(retrievalErrorText({ status: 400 }), /1 至 8192/)
  assert.match(retrievalErrorText({ status: 401 }), /未获授权/)
  assert.match(retrievalErrorText({ status: 403 }), /未获授权/)
  assert.match(retrievalErrorText({ status: 404 }), /不可用/)
  assert.equal(retrievalErrorText({ code: 'future' }), retrievalErrorText(undefined))
  assert.equal(retrievalErrorText(new Error('secret-model-name')), retrievalErrorText(undefined))
})

test('构建失败的全部终态原因转译为影响与下一步', () => {
  for (const code of ['MODEL_ERROR', 'INTERRUPTED', 'BUDGET_EXHAUSTED', 'RUNTIME_UNAVAILABLE', 'future']) {
    assert.ok(!indexBuildFailureText(code).includes(code))
    assert.match(indexBuildFailureText(code), /构建/)
  }
})

test('a citation never claims a semantic source the search did not use', () => {
  assert.equal(hitSourceText({ matched_terms: [], fts_rank: 0.4, vector_score: 0 }), '全文召回')
  assert.equal(hitSourceText({ matched_terms: [], fts_rank: 0, vector_score: 0.7 }), '语义召回')
  assert.equal(hitSourceText({ matched_terms: [], fts_rank: 0.4, vector_score: 0.7 }), '全文与语义召回')
  assert.equal(hitSourceText({ matched_terms: ['运维', '响应'], fts_rank: 0.4, vector_score: 0 }), '命中词 运维、响应')
})

test('only an index older than the active one is offered as a rollback', () => {
  assert.equal(indexStateText({ id: 5, state: 'rollback_ready' }, 2), '已就绪，待启用')
  assert.equal(indexStateText({ id: 5, state: 'rollback_ready' }, undefined), '已就绪，待启用')
  assert.equal(indexStateText({ id: 2, state: 'rollback_ready' }, 5), '可切换/回滚')
  assert.equal(indexStateText({ id: 5, state: 'current' }, 5), '当前生效')
})

test('research gaps from the server are translated to business language', () => {
  assert.equal(researchGapText('retrieval budget exhausted'), '本次检索预算已用完，后续补查未执行；可缩小问题范围后重新检索。')
  assert.equal(researchGapText('follow-up budget exhausted'), '本次检索预算已用完，后续补查未执行；可缩小问题范围后重新检索。')
  assert.equal(researchGapText('duplicate query stopped'), '后续查询与已查内容重复，已停止补查。')
  assert.equal(researchGapText('no research model; evidence requires review'), '未配置研究模型，未做后续补查，证据需人工复核。')
  assert.equal(researchGapText('no evidence for facet: 恢复时间'), '未找到与“恢复时间”相关的证据。')
})
