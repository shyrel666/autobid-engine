import { useState } from 'react'
import { ArrowRight, CheckCircle2, Download, FileJson, FileText, GitBranch, RefreshCw, Star } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { ReportAnchor, ReportFinding } from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Pre, Spinner, Stat, cx, severityTone, useToast } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import { assetTypeLabel, downloadText, findingCategoryLabel, fmtPct, fmtTime, pretty, severityLabel, severityOrder } from '../../lib/format'
import { NodeDetailModal } from '../../features/execution/NodeParts'

function refText(v: unknown): string {
  if (v == null) return ''
  if (typeof v === 'object' && Object.keys(v as object).length === 0) return ''
  return JSON.stringify(v)
}

function AnchorChip({ a }: { a: ReportAnchor }) {
  return (
    <Badge tone={a.is_mandatory ? 'red' : 'slate'}>
      {a.is_mandatory && '★ '}{assetTypeLabel[a.type] ?? a.type} {a.code} {a.title}
    </Badge>
  )
}

function FindingCard({ f, onInspectNode }: { f: ReportFinding; onInspectNode: (id: number) => void }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="rounded-md border border-slate-200 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone={severityTone(f.severity)}>{severityLabel[f.severity] ?? f.severity}</Badge>
        <span className="font-medium">{f.title}</span>
        <span className="ml-auto text-xs text-slate-400">#{f.id} · 证据 {f.evidence.length}</span>
      </div>
      {f.summary && <p className="mt-1 text-sm text-slate-600">{f.summary}</p>}

      {/* Compact visual lineage summary bar */}
      <div className="mt-2 flex flex-wrap items-center gap-1.5 rounded-md border border-slate-200/80 bg-slate-50/70 px-2.5 py-1.5 text-xs">
        <GitBranch className="size-3.5 text-slate-400" />
        <button type="button" onClick={() => onInspectNode(f.node_id)} className="rounded bg-red-50 px-1.5 py-0.5 font-mono text-red-700 hover:underline">
          结论节点 #{f.node_id}
        </button>
        {f.evidence.length > 0 && (
          <>
            <ArrowRight className="size-3 text-slate-400" />
            <span className="text-slate-500">事实依据 ({f.evidence.length})：</span>
            {f.evidence.map((ev) => (
              <button
                key={ev.node_id}
                type="button"
                onClick={() => onInspectNode(ev.node_id)}
                className="rounded bg-emerald-50 px-1.5 py-0.5 font-mono text-emerald-700 hover:underline"
              >
                #{ev.node_id}
              </button>
            ))}
          </>
        )}
        {(f.anchors?.length ?? 0) > 0 && (
          <>
            <ArrowRight className="size-3 text-slate-400" />
            <span className="text-slate-500">锚定要素 ({f.anchors!.length})</span>
          </>
        )}
        <button className="ml-auto text-xs text-blue-600 hover:underline" onClick={() => setOpen(!open)}>
          {open ? '收起证据血缘' : '展开证据血缘'}
        </button>
      </div>

      {(f.anchors?.length ?? 0) > 0 && <div className="mt-2 flex flex-wrap gap-1">{f.anchors!.map((a) => <AnchorChip key={a.asset_id} a={a} />)}</div>}
      {open && (
        <div className="mt-2.5 grid gap-2.5 rounded-lg border border-blue-100 bg-blue-50/20 p-3 md:grid-cols-3">
          {/* Column 1: Finding */}
          <div className="rounded border border-slate-200 bg-white p-2.5 text-xs">
            <div className="mb-1 font-semibold text-slate-700">1. 审查结论 (Finding)</div>
            <div className="text-slate-600">{f.title}</div>
            {f.summary && <div className="mt-1 text-slate-500">{f.summary}</div>}
          </div>
          {/* Column 2: Confirmed Facts */}
          <div className="rounded border border-slate-200 bg-white p-2.5 text-xs">
            <div className="mb-1 font-semibold text-slate-700">2. 确认事实证据 (Facts)</div>
            {f.evidence.length === 0 ? <p className="text-slate-400">无事实证据</p> : (
              <div className="space-y-2">
                {f.evidence.map((ev) => (
                  <div key={ev.node_id} className="border-b border-slate-100 pb-1.5 last:border-b-0 last:pb-0">
                    <div className="flex flex-wrap items-center gap-1">
                      <button type="button" onClick={() => onInspectNode(ev.node_id)} className="font-mono text-blue-600 hover:underline">#{ev.node_id}</button>
                      <Badge tone={ev.state === 'confirmed' ? 'green' : 'slate'}>{ev.state}</Badge>
                      {ev.inherited && <Badge tone="amber">继承</Badge>}
                    </div>
                    <div className="mt-0.5 text-slate-700">{ev.summary}</div>
                    {ev.evidence && <div className="mt-0.5 text-slate-500">依据：{ev.evidence}</div>}
                    {refText(ev.source_ref) && <div className="text-slate-400">出处：<code className="break-all">{refText(ev.source_ref)}</code></div>}
                  </div>
                ))}
              </div>
            )}
          </div>
          {/* Column 3: Anchored Tender Elements & Source Refs */}
          <div className="rounded border border-slate-200 bg-white p-2.5 text-xs">
            <div className="mb-1 font-semibold text-slate-700">3. 招标要素与原文出处</div>
            {(f.anchors?.length ?? 0) === 0 && (f.source_refs?.length ?? 0) === 0 ? (
              <p className="text-slate-400">无锚定要素或原文定位</p>
            ) : (
              <div className="space-y-1.5">
                {(f.anchors ?? []).map((a) => (
                  <div key={a.asset_id} className="flex flex-wrap items-center gap-1">
                    <AnchorChip a={a} />
                  </div>
                ))}
                {(f.source_refs ?? []).map((r, i) => (
                  <div key={i} className="text-slate-500">
                    原文出处：<code className="break-all">{refText(r)}</code>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

export default function ReportTab({ taskID }: { taskID: number }) {
  const toast = useToast()
  const rep = useAsync(() => api.report(taskID), [taskID])
  const [view, setView] = useState<'visual' | 'markdown'>('visual')
  const md = useAsync(() => api.reportMarkdown(taskID), [taskID], view === 'markdown')
  const [busy, setBusy] = useState('')
  const [nodeID, setNodeID] = useState<number | null>(null)

  const dlJSON = () => {
    if (!rep.data) return
    downloadText(`task-${taskID}-report.json`, JSON.stringify(rep.data, null, 2), 'application/json')
  }
  const dlMD = async () => {
    setBusy('md')
    try {
      downloadText(`task-${taskID}-report.md`, await api.reportMarkdown(taskID), 'text/markdown;charset=utf-8')
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setBusy('')
    }
  }

  if (rep.loading && !rep.data) return <Spinner label="正在生成报告…" />
  if (!rep.data) return <ErrorBanner error={rep.error} onRetry={rep.reload} />
  const r = rep.data
  const s = r.summary
  const mandatory = r.elements.filter((e) => e.is_mandatory)
  const scoring = r.elements.filter((e) => e.type === 'scoring_item')
  const groups = new Map<string, ReportFinding[]>()
  for (const f of r.findings) groups.set(f.category, [...(groups.get(f.category) ?? []), f])
  const mandatoryPct = s.mandatory_elements ? s.mandatory_verified / s.mandatory_elements : null

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="text-sm text-slate-500">生成于 {fmtTime(r.generated_at)} · 审查目标：{r.goal}</div>
        <div className="flex gap-2">
          <Button size="sm" variant={view === 'visual' ? 'primary' : 'secondary'} onClick={() => setView('visual')}>可视化</Button>
          <Button size="sm" variant={view === 'markdown' ? 'primary' : 'secondary'} onClick={() => setView('markdown')}>Markdown 预览</Button>
          <Button size="sm" icon={<RefreshCw className="size-3.5" />} onClick={() => { void rep.reload(); if (view === 'markdown') void md.reload() }}>重新生成</Button>
          <Button size="sm" icon={<FileJson className="size-3.5" />} onClick={dlJSON}>下载 JSON</Button>
          <Button size="sm" variant="primary" icon={<Download className="size-3.5" />} loading={busy === 'md'} onClick={dlMD}>下载 Markdown</Button>
        </div>
      </div>
      <ErrorBanner error={rep.error} onRetry={rep.reload} />

      {view === 'markdown' ? (
        <Card title={<><FileText className="size-4" /> GET /api/tasks/{taskID}/report?format=markdown</>}>
          <ErrorBanner error={md.error} onRetry={md.reload} />
          {md.loading && !md.data ? <Spinner /> : <Pre className="max-h-[70vh]">{md.data}</Pre>}
        </Card>
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
            <Stat label="要素覆盖率" value={fmtPct(r.coverage.pct)} hint={`${r.coverage.tested} / ${r.coverage.target}`} />
            <Stat label="强制条款核验" value={`${s.mandatory_verified} / ${s.mandatory_elements}`} hint={fmtPct(mandatoryPct)}
              tone={s.mandatory_elements === 0 ? undefined : s.mandatory_verified === s.mandatory_elements ? 'green' : 'amber'} />
            <Stat label="目标达成" value={`${s.goals_met} / ${s.goals_total}`} tone={s.goals_total > 0 && s.goals_met === s.goals_total ? 'green' : undefined} />
            <Stat label="已确认事实" value={s.confirmed_facts} hint={`${s.documents} 份文档 · ${s.elements} 个要素`} />
            <Stat label="审查结论" value={s.findings}
              hint={severityOrder.filter((k) => s.findings_by_severity[k]).map((k) => `${severityLabel[k]} ${s.findings_by_severity[k]}`).join(' · ') || '—'}
              tone={(s.findings_by_severity.critical ?? 0) + (s.findings_by_severity.high ?? 0) > 0 ? 'red' : undefined} />
          </div>

          <Card title={<><Star className="size-4 fill-red-500 text-red-500" /> 强制性条款核验清单</>} bodyClassName="p-0">
            {mandatory.length === 0 ? <Empty>范围内暂无强制性条款要素</Empty> : (
              <table className="w-full text-sm">
                <thead className="bg-slate-50 text-left text-xs text-slate-500">
                  <tr><th className="px-3 py-2">编号</th><th className="px-3 py-2">条款</th><th className="px-3 py-2">类型</th><th className="px-3 py-2">状态</th><th className="px-3 py-2">事实 / 结论</th><th className="px-3 py-2">出处</th></tr>
                </thead>
                <tbody>
                  {mandatory.map((e) => (
                    <tr key={e.asset_id} className={cx('border-t border-slate-100', !e.verified && 'bg-amber-50/50')}>
                      <td className="px-3 py-2 font-mono text-xs">{e.code || '—'}</td>
                      <td className="px-3 py-2">{e.title}</td>
                      <td className="px-3 py-2"><Badge>{assetTypeLabel[e.type] ?? e.type}</Badge></td>
                      <td className="px-3 py-2">{e.verified ? <Badge tone="green"><CheckCircle2 className="size-3" />已核验</Badge> : <Badge tone="amber">待核验</Badge>}</td>
                      <td className="px-3 py-2 text-xs text-slate-500">{(e.fact_ids ?? []).map((id) => `#${id}`).join(', ') || '—'} / {(e.finding_ids ?? []).map((id) => `#${id}`).join(', ') || '—'}</td>
                      <td className="max-w-xs truncate px-3 py-2 font-mono text-xs text-slate-400" title={pretty(e.source_ref)}>{refText(e.source_ref) || '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Card>

          <Card title="评分项" bodyClassName="p-0">
            {scoring.length === 0 ? <Empty>范围内暂无评分项要素</Empty> : (
              <table className="w-full text-sm">
                <thead className="bg-slate-50 text-left text-xs text-slate-500">
                  <tr><th className="px-3 py-2">编号</th><th className="px-3 py-2">评分项</th><th className="px-3 py-2">分类</th><th className="px-3 py-2 text-right">分值</th><th className="px-3 py-2">状态</th><th className="px-3 py-2">关联结论</th></tr>
                </thead>
                <tbody>
                  {scoring.map((e) => (
                    <tr key={e.asset_id} className="border-t border-slate-100">
                      <td className="px-3 py-2 font-mono text-xs">{e.code || '—'}</td>
                      <td className="px-3 py-2">{e.title}</td>
                      <td className="px-3 py-2 text-xs text-slate-500">{e.category || '—'}</td>
                      <td className="px-3 py-2 text-right tabular-nums">{e.score_max ?? '—'}</td>
                      <td className="px-3 py-2">{e.verified ? <Badge tone="green">已核验</Badge> : <Badge tone="amber">待核验</Badge>}</td>
                      <td className="px-3 py-2 text-xs text-slate-500">{(e.finding_ids ?? []).map((id) => `#${id}`).join(', ') || '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {scoring.length > 0 && (
              <div className="border-t border-slate-100 px-3 py-2 text-right text-xs text-slate-500">
                评分项总分 {scoring.reduce((sum, e) => sum + (e.score_max ?? 0), 0)} · 已核验 {scoring.filter((e) => e.verified).length} / {scoring.length}
              </div>
            )}
          </Card>

          <Card title="审查结论与证据链">
            {r.findings.length === 0 ? <Empty>暂无审查结论</Empty> : (
              <div className="space-y-5">
                {[...groups.entries()].sort(([a], [b]) => Object.keys(findingCategoryLabel).indexOf(a) - Object.keys(findingCategoryLabel).indexOf(b)).map(([cat, fs]) => (
                  <div key={cat}>
                    <h4 className="mb-2 text-sm font-semibold text-slate-700">{findingCategoryLabel[cat] ?? cat}（{fs.length}）</h4>
                    <div className="space-y-2">{fs.map((f) => <FindingCard key={f.id} f={f} onInspectNode={setNodeID} />)}</div>
                  </div>
                ))}
              </div>
            )}
          </Card>

          <Card title="要素分布 / 招标文档">
            <div className="flex flex-wrap gap-2">
              {Object.entries(s.elements_by_type).map(([k, v]) => <Badge key={k}>{assetTypeLabel[k] ?? k} {v}</Badge>)}
            </div>
            <ul className="mt-3 space-y-1 text-sm text-slate-600">
              {r.documents.map((d) => <li key={d.id}>#{d.id} {d.title} · {d.tender_no}{d.lot_no ? ` / ${d.lot_no}` : ''} · {d.chunk_count} 段</li>)}
            </ul>
          </Card>
        </>
      )}
      <NodeDetailModal taskID={taskID} nodeID={nodeID} onClose={() => setNodeID(null)} onOpen={setNodeID} />
    </div>
  )
}
