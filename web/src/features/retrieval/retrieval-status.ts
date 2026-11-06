// 检索状态的业务文案与请求规则。原始状态、模型配置与错误只供折叠诊断使用。
import type { KnowledgeIndex, ResearchRequest, RetrievalDiagnostics, RetrievalEvidence, RetrievalStatus } from '../../api/types'

export type RetrievalTone = 'green' | 'amber' | 'slate' | 'blue' | 'red'

export function retrievalStateLabel(state: string): string {
  switch (state) {
    case 'semantic': return '语义检索正常'
    case 'degraded': return '部分降级'
    case 'lexical_only': return '仅全文检索'
    case 'empty': return '知识库暂无文档'
    default: return '检索状态待确认'
  }
}

export function retrievalStateTone(state: string): RetrievalTone {
  return state === 'semantic' ? 'green' : state === 'degraded' || state === 'lexical_only' ? 'amber' : 'slate'
}

const degradedReasons: Record<string, string> = {
  semantic_model_unconfigured: '尚未配置语义检索，当前使用全文和关键词查找',
  semantic_index_missing_or_stale: '语义索引尚未就绪，当前使用全文和关键词查找',
  semantic_channel_failed: '本次语义检索未能完成，已保留全文和关键词结果',
  reranker_unconfigured: '尚未启用智能排序，结果按基础相关性排列',
  reranker_failed: '本次智能排序未能完成，已保留基础检索结果',
  rerank_truncated: '本次预算仅支持对部分候选结果进行智能排序',
  index_missing: '尚无已启用的语义索引',
  index_model_changed: '检索模型已更换，原语义索引需要重建',
  index_dimension_changed: '检索配置已变化，原语义索引需要重建',
  index_preprocessing_changed: '文档处理方式已变化，原语义索引需要重建',
  index_documents_changed: '知识库文档已更新，语义索引需要重建',
  index_unconfigured: '尚未配置语义检索服务',
  embedding_degraded: '语义检索服务最近调用失败，暂用全文和关键词查找',
  embedding_self_test_failed: '语义检索服务检查未通过，暂用全文和关键词查找',
  embedding_starting: '语义检索服务正在准备，暂用全文和关键词查找',
  reranker_degraded: '智能排序服务最近调用失败，暂用基础排序',
  reranker_self_test_failed: '智能排序服务检查未通过，暂用基础排序',
  reranker_starting: '智能排序服务正在准备，暂用基础排序',
}

export function degradedReasonText(code: string): string {
  return Object.hasOwn(degradedReasons, code) ? degradedReasons[code] : '检索能力受限'
}

export function statusSummary(status: RetrievalStatus | undefined): { tone: RetrievalTone; title: string; detail: string; nextAction: string } {
  if (!status) return { tone: 'slate', title: '检索状态待确认', detail: '正在读取服务器检索状态。', nextAction: '请稍候；读取失败时可刷新重试。' }
  const reasons = status.reasons ?? []
  let nextAction = '可开始检索；引用前请核验原文与适用条件。'
  if (status.state === 'empty') nextAction = '请先到文档库上传资料，再构建并启用语义索引。'
  else if (reasons.includes('index_documents_changed')) nextAction = '文档已更新，请重建语义索引（未变化的内容不会重新计算）。'
  else if (reasons.includes('index_missing')) nextAction = '请到语义索引页构建并启用。'
  else if (reasons.includes('index_model_changed')) nextAction = '模型已更换，请用新模型重建后启用。'
  else if (reasons.includes('index_dimension_changed') || reasons.includes('index_preprocessing_changed')) nextAction = '检索配置已变化，请重建语义索引后启用。'
  else if (reasons.includes('semantic_model_unconfigured') || reasons.includes('index_unconfigured')) nextAction = '请联系管理员配置语义检索服务，再构建并启用语义索引。'
  else if (reasons.some((r) => r.startsWith('embedding_')) || reasons.includes('semantic_channel_failed')) nextAction = '检索已自动退回全文检索，模型恢复后自动恢复。'
  else if (reasons.includes('reranker_unconfigured')) nextAction = '可继续检索；如需智能排序，请联系管理员启用。'
  else if (reasons.some((r) => r.startsWith('reranker_'))) nextAction = '可继续使用基础排序，智能排序服务恢复后自动恢复。'
  else if (status.state !== 'semantic') nextAction = '可继续全文检索；请到语义索引页检查状态或联系管理员。'
  const detail = status.state === 'empty' ? '暂无可检索的知识库内容。'
    : reasons.length ? [...new Set(reasons.map(degradedReasonText))].join('；')
    : status.state === 'semantic' ? '全文与语义检索及智能排序均可用；命中内容仍需核验。' : '检索能力受限，结果可能不完整。'
  return { tone: retrievalStateTone(status.state), title: retrievalStateLabel(status.state), detail, nextAction }
}

export function indexStateLabel(state: string): string {
  switch (state) {
    case 'current': return '当前生效'
    case 'rollback_ready': return '可切换/回滚'
    case 'documents_changed': return '文档已变化，需重建'
    case 'model_mismatch': return '模型不一致'
    case 'dimension_mismatch': return '维度不一致'
    case 'preprocessing_mismatch': return '预处理版本不一致'
    case 'queued': return '排队中'
    case 'building': return '构建中'
    case 'failed': return '失败'
    case 'cancelled': return '已取消'
    case 'ready': return '已构建，待启用'
    default: return '索引状态待确认'
  }
}

export function indexStateTone(state: string): RetrievalTone {
  switch (state) {
    case 'current': return 'green'
    case 'rollback_ready': case 'queued': case 'building': return 'blue'
    case 'failed': return 'red'
    case 'cancelled': return 'slate'
    default: return 'amber'
  }
}

/** 新建好的索引尚未用过，称“回滚”会误导；只有比当前索引旧的才是回滚目标。 */
export function indexStateText(index: Pick<KnowledgeIndex, 'id'> & { state: string }, activeId: number | undefined): string {
  if (index.state === 'rollback_ready' && (activeId === undefined || index.id > activeId)) return '已就绪，待启用'
  return indexStateLabel(index.state)
}

export function buildProgress(index: Pick<KnowledgeIndex, 'embedded_chunks' | 'reused_chunks' | 'total_chunks'>): { percent: number; label: string } {
  const count = (n: number) => Number.isFinite(n) ? Math.max(0, n) : 0
  const total = count(index.total_chunks), reused = count(index.reused_chunks)
  const processed = count(index.embedded_chunks) + reused
  return { percent: total ? Math.min(100, Math.floor(processed / total * 100)) : 0,
    label: `已处理 ${processed} / ${total} 分块（复用 ${reused}）` }
}

export function runStatusLabel(status: string): string {
  switch (status) {
    case 'running': return '进行中'
    case 'completed': return '已完成'
    case 'partial': return '部分完成（有缺口）'
    case 'failed': return '失败'
    case 'cancelled': return '已取消'
    case 'budget_exhausted': return '预算耗尽'
    default: return '检索记录状态待确认'
  }
}

export function runStatusTone(status: string): RetrievalTone {
  return status === 'running' ? 'blue' : status === 'completed' ? 'green' : status === 'failed' ? 'red'
    : status === 'partial' || status === 'budget_exhausted' ? 'amber' : 'slate'
}

export function evidenceSummary(list: Pick<RetrievalEvidence, 'status'>[] | undefined): { verified: number; pending: number; stale: number } {
  const summary = { verified: 0, pending: 0, stale: 0 }
  for (const evidence of list ?? []) {
    if (evidence.status === 'verified') summary.verified++
    else if (evidence.status === 'stale') summary.stale++
    else summary.pending++
  }
  return summary
}

export function statusNeedsPoll(status: RetrievalStatus | undefined): boolean {
  return !!status && (!!status.active_build || status.embedding.health.state === 'starting' || status.reranker.health.state === 'starting')
}

export function degradationSummary(degraded: string[] | null | undefined, diagnostics?: RetrievalDiagnostics) {
  const codes = [...new Set(degraded ?? [])]
  const lexicalOnly = diagnostics?.mode === 'lexical_only' || (!diagnostics && codes.some((code) => code.startsWith('semantic_')))
  return { limited: codes.length > 0 || lexicalOnly, lexicalOnly, reasons: [...new Set(codes.map(degradedReasonText))] }
}

export function newIndexRequestKey(): string { return `ui:index:${crypto.randomUUID()}` }
export interface IndexRequestKeyState { signature: string; key: string }

/** 相同输入的失败重试、响应丢失与重复点击复用键；输入变化或已确认提交后开始新构建时换键。 */
export function indexRequestKey(previous: IndexRequestKeyState | undefined, signature: string, fresh = newIndexRequestKey): IndexRequestKeyState {
  return previous?.signature === signature ? previous : { signature, key: fresh() }
}

export function indexBuildSignature(status: RetrievalStatus, dimension?: number): string {
  return JSON.stringify([status.embedding.info?.id ?? '', status.embedding.info?.dimension || dimension || 0, status.preprocessing])
}

/** 继续/重试保留原请求，不能通过改写 multi_agent 来复用同一个幂等键。 */
export function canResumeResearch(request: ResearchRequest, multiAgentEnabled: boolean): boolean {
  return !request.multi_agent || multiAgentEnabled
}

/** 引用行说明这条结果从哪个通道来；不能在语义通道没有参与时写“向量召回”。 */
export function hitSourceText(hit: { matched_terms: string[]; fts_rank: number; vector_score: number }): string {
  if (hit.matched_terms.length) return `命中词 ${hit.matched_terms.join('、')}`
  if (hit.fts_rank > 0 && hit.vector_score !== 0) return '全文与语义召回'
  return hit.vector_score !== 0 ? '语义召回' : '全文召回'
}

/** 服务端已有的结构化缺口可能带英文状态；业务陈述保留，状态说明统一转译。 */
export function researchGapText(gap: string): string {
  if (Object.hasOwn(degradedReasons, gap)) return degradedReasonText(gap)
  const degraded = /^(?:检索降级：|retrieval degraded:\s*)(.+)$/.exec(gap)
  if (degraded) return degradedReasonText(degraded[1])
  if (gap === 'no evidence found') return '尚未找到证据，请调整检索条件或核对原文。'
  if (/budget exhausted/i.test(gap)) return '本次检索预算已用完，后续补查未执行；可缩小问题范围后重新检索。'
  if (gap === 'duplicate query stopped') return '后续查询与已查内容重复，已停止补查。'
  if (/^no research model/i.test(gap)) return '未配置研究模型，未做后续补查，证据需人工复核。'
  const facet = /^no evidence for facet: (.+)$/.exec(gap)
  if (facet) return `未找到与“${facet[1]}”相关的证据。`
  const evidence = /^evidence (\d+):.*\((verified|unverified|stale)\)$/.exec(gap)
  if (evidence) return evidence[2] === 'stale' ? `证据 #${evidence[1]} 的来源已失效，请重新检索。`
    : evidence[2] === 'unverified' ? `证据 #${evidence[1]} 尚待核验，请核对原文。` : `证据 #${evidence[1]} 尚不足以支持本次结论，请检查核验结果。`
  if (/^(retrieval:|context (deadline|canceled)|research model)/i.test(gap)) return '部分检索未能完成，请核对现有证据并稍后重试。'
  return gap
}

const indexErrors: Record<string, string> = {
  runtime_unavailable: '语义检索服务尚不可用，请联系管理员配置或恢复服务后重试。',
  build_active: '已有索引正在构建，请查看进度或取消后再提交。',
  idempotency_conflict: '本次请求与已提交的构建参数不同，请刷新状态并重新构建。',
  nothing_to_index: '知识库暂无可构建的内容，请先上传文档。',
  model_unconfigured: '尚未配置语义检索服务，请联系管理员。',
  model_mismatch: '模型已更换，此索引不能启用，请重建后启用。',
  dimension_mismatch: '检索配置与此索引不一致，请重建后启用。',
  preprocessing_mismatch: '文档处理方式与此索引不一致，请重建后启用。',
  not_ready: '此索引尚未构建完成，请等待完成后再启用。',
  documents_changed: '文档已更新，此索引不能启用，请重建。',
  coverage_incomplete: '此索引未覆盖全部内容，请重建后再启用。',
  already_terminal: '此构建已经结束，请刷新查看最终状态。',
  index_in_use: '此索引正在使用或构建中，暂时无法删除。',
}

export function retrievalErrorText(error: unknown): string {
  const e = error as { code?: string; status?: number } | null
  if (e?.code && Object.hasOwn(indexErrors, e.code)) return indexErrors[e.code]
  if (e?.status === 401 || e?.status === 403) return '访问未获授权，请检查访问凭据与权限后重试。'
  if (e?.status === 400) return '提交参数无效，请核对后重试；索引维度需为 1 至 8192 的整数。'
  if (e?.status === 404) return '所需记录或检索接口不可用，请刷新或联系管理员。'
  return '请求未能完成，请刷新查看服务器状态后重试。'
}

export function indexBuildFailureText(code: string): string {
  switch (code) {
    case 'MODEL_ERROR': return '检索服务未能完成构建，请联系管理员检查服务后重新构建。'
    case 'INTERRUPTED': return '构建已中断，请重新构建。'
    case 'BUDGET_EXHAUSTED': return '构建已达到时间或重试上限，请检查服务后重新构建。'
    case 'RUNTIME_UNAVAILABLE': return '检索配置已变化或服务不可用，请检查服务后重新构建。'
    default: return '构建未能完成，请检查服务后重新构建。'
  }
}
