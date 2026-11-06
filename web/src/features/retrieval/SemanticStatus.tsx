import { RefreshCw } from 'lucide-react'
import { api } from '../../api/client'
import type { RetrievalDiagnostics } from '../../api/types'
import { Badge, Button, Card, Notice, Spinner } from '../../components/ui'
import { useAsync, useInterval } from '../../hooks/useAsync'
import { fmtTime } from '../../lib/format'
import { degradationSummary, statusNeedsPoll, statusSummary } from './retrieval-status'

export function useRetrievalStatus() {
  const state = useAsync(() => api.retrievalStatus(), [])
  useInterval(() => { if (!state.loading) void state.reload() }, statusNeedsPoll(state.data) ? 3000 : null)
  return state
}

export type RetrievalStatusState = ReturnType<typeof useRetrievalStatus>

function RefreshStatus({ retrieval }: { retrieval: RetrievalStatusState }) {
  return <Button size="sm" variant="ghost" loading={retrieval.loading} icon={<RefreshCw className="size-3.5" />}
    onClick={() => void retrieval.reload()}>刷新状态</Button>
}

export function SemanticStatusCard({ retrieval }: { retrieval: RetrievalStatusState }) {
  const summary = statusSummary(retrieval.data)
  return <Card title="检索能力" actions={<RefreshStatus retrieval={retrieval} />}>
    {retrieval.error && <Notice tone="warn" className="mb-3">检索状态读取失败，当前能力待确认。请刷新重试或联系管理员。{retrieval.data && '下方保留上次读取的状态。'}</Notice>}
    {retrieval.loading && !retrieval.data ? <Spinner label="正在读取检索能力…" /> : <div className="space-y-2 text-sm">
      <Badge dot tone={retrieval.error ? 'slate' : summary.tone}>{retrieval.error ? '检索状态待确认' : summary.title}</Badge>
      <p>{summary.detail}</p>
      <p className="text-muted">下一步：{summary.nextAction}</p>
      {retrieval.data && <p className="text-xs text-muted">{retrieval.data.chunks} 个可检索分块 · 状态检查于 {fmtTime(retrieval.data.checked_at)}</p>}
    </div>}
    {(retrieval.data || retrieval.error) && <details className="mt-3 text-xs text-muted">
      <summary className="cursor-pointer">诊断详情</summary>
      <pre className="mt-2 max-h-96 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-soft p-3">{JSON.stringify({ status: retrieval.data, read_error: retrieval.error || undefined }, null, 2)}</pre>
    </details>}
  </Card>
}

/** 紧凑提示仍保留影响与下一步，读取失败时不把旧快照显示成当前正常状态。 */
export function SemanticStatusBar({ retrieval }: { retrieval: RetrievalStatusState }) {
  const summary = statusSummary(retrieval.data)
  const detail = retrieval.error ? '状态读取失败，请刷新重试或联系管理员。' : `${summary.detail} 下一步：${summary.nextAction}`
  return <div className="flex items-center gap-2 rounded-[10px] border border-line bg-panel px-3 py-2 text-xs">
    <Badge dot tone={retrieval.error ? 'slate' : summary.tone}>{retrieval.error ? '检索状态待确认' : retrieval.loading && !retrieval.data ? '检索状态读取中' : summary.title}</Badge>
    <span className="min-w-0 flex-1 truncate text-muted" title={detail}>{detail}</span>
    <RefreshStatus retrieval={retrieval} />
  </div>
}

export function DegradedNotice({ degraded, diagnostics }: { degraded?: string[] | null; diagnostics?: RetrievalDiagnostics }) {
  const summary = degradationSummary(degraded, diagnostics)
  if (!summary.limited && !diagnostics) return null
  return <div className="space-y-2">
    {summary.limited && <Notice tone="warn">
      {summary.lexicalOnly && <p className="font-medium">本次只做了全文检索，没有命中不代表知识库没有相关内容。</p>}
      {summary.reasons.length > 0 && <ul className="list-disc space-y-1 pl-5">{summary.reasons.map((reason) => <li key={reason}>{reason}</li>)}</ul>}
      <p className="mt-1 text-xs">下一步：{summary.lexicalOnly ? '可调整关键词继续查找，并到语义索引页检查状态；重要结论请核对原文。' : '可继续使用现有结果；重要结论请核对原文，必要时稍后重试。'}</p>
    </Notice>}
    <details className="text-xs text-muted">
      <summary className="cursor-pointer">诊断详情</summary>
      <pre className="mt-2 max-h-80 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-soft p-3">{JSON.stringify({ degraded: degraded ?? [], diagnostics }, null, 2)}</pre>
    </details>
  </div>
}
