import { useState } from 'react'
import { api, errorText } from '../api/client'
import type { EvidenceClaim, RetrievalEvidence as Evidence } from '../api/types'
import { useAsync } from '../hooks/useAsync'
import { Button, Card, ErrorBanner, Field, Input, Select, Badge } from './ui'
import { TableView } from './TableView'

function EvidenceItem({ evidence, refresh }: { evidence: Evidence; refresh: () => void }) {
  const [claim, setClaim] = useState<EvidenceClaim>({ text: '', parameter: '', operator: '<=', value: 0, unit: '秒', product: '', version: '' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const verify = async () => {
    setBusy(true); setError('')
    try { await api.verifyEvidence(evidence.run_id, evidence.id, claim); refresh() } catch(e) { setError(errorText(e)) } finally { setBusy(false) }
  }
  const labels: Record<string, string> = { verified: '结构化指标已核验', unverified: '待核验', stale: '来源已失效', supports: '支持', contradicts: '矛盾', insufficient: '证据不足' }
  return <details className="rounded border border-slate-200 p-3">
    <summary className="cursor-pointer text-sm">证据 #{evidence.id} · {evidence.snapshot.document?.title} <Badge tone={evidence.status === 'verified' && evidence.relation === 'supports' ? 'green' : 'amber'}>{labels[evidence.status]} · {labels[evidence.relation]}</Badge></summary>
    <p className="my-2 text-xs text-slate-500">{evidence.reason}</p>
    <pre className="whitespace-pre-wrap rounded bg-slate-50 p-2 text-xs">{evidence.snapshot.chunk?.content}</pre>
    {evidence.snapshot.chunk?.tables?.map((table, i) => <TableView key={i} table={table} index={i} />)}
    {evidence.claim?.verdict?.quote && <p className="my-2 text-sm">核验原文：{evidence.claim.verdict.quote}</p>}
    <div className="mt-3 grid gap-2 md:grid-cols-3">
      {(['text', 'parameter', 'product', 'version', 'unit'] as const).map((key) => <Field key={key} label={{ text: '待核验陈述', parameter: '参数原名', product: '准确型号', version: '版本', unit: '阈值单位' }[key]}><Input value={claim[key] ?? ''} onChange={e => setClaim({ ...claim, [key]: e.target.value })} /></Field>)}
      <Field label="数值阈值"><Input type="number" value={claim.value} onChange={e => setClaim({ ...claim, value: Number(e.target.value) })} /></Field>
      <Field label="比较方式"><Select value={claim.operator} onChange={e => setClaim({ ...claim, operator: e.target.value })}><option value="<=">不超过</option><option value=">=">不低于</option><option value="=">等于</option></Select></Field>
    </div>
    <p className="my-2 text-xs text-slate-500">仅核验明确的标量指标；型号、条件或表格存在歧义时保留待复核状态。此结果不代表整项要求满足。</p>
    <Button loading={busy} disabled={evidence.status === 'stale' || !claim.text || !claim.parameter} onClick={() => void verify()}>核验指标</Button>
    <ErrorBanner error={error} />
  </details>
}
export function RetrievalEvidence({ runID }: { runID: number }) {
  const data = useAsync(() => api.retrievalEvidence(runID), [runID])
  return <Card title="证据与核验" actions={<Button onClick={() => void data.reload()}>刷新有效性</Button>}>
    <ErrorBanner error={data.error} />
    <div className="space-y-2">{data.data?.map(e => <EvidenceItem key={e.id} evidence={e} refresh={() => void data.reload()} />)}</div>
  </Card>
}

export function KnowledgeIndexes() {
  const data = useAsync(() => api.knowledgeIndexes(), [])
  const [dimension, setDimension] = useState(1024)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const run = async (action: () => Promise<unknown>) => { setBusy(true); setError(''); try { await action(); await data.reload() } catch(e) { setError(errorText(e)) } finally { setBusy(false) } }
  return <Card title="语义索引">
    <p className="mb-3 text-sm text-slate-500">服务端配置模型后按实际维度构建。构建完成再激活；来源变更后需重建。可激活同一来源快照的旧索引以回滚。</p>
    <div className="flex items-end gap-2"><Field label="模型维度"><Input type="number" min={1} max={8192} value={dimension} onChange={e => setDimension(Number(e.target.value))} /></Field><Button loading={busy} onClick={() => void run(() => api.buildKnowledgeIndex(dimension))}>构建</Button><Button onClick={() => void data.reload()}>刷新</Button></div>
    <ErrorBanner error={error || data.error} />
    <div className="mt-3 space-y-2">{data.data?.map(i => <div key={i.id} className="flex items-center gap-2 text-sm"><span>#{i.id} · {i.model} · {i.dimension} 维 · {i.status}</span>{i.active ? <Badge tone="green">当前索引</Badge> : <Button disabled={busy || i.status !== 'ready'} onClick={() => void run(() => api.activateKnowledgeIndex(i.id))}>激活</Button>}{i.error && <span>{i.error}</span>}</div>)}</div>
  </Card>
}
