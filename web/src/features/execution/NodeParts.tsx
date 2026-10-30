import { useState } from 'react'
import type { ReactNode } from 'react'
import { CheckCircle2, FileSearch, Layers, MessageSquare, RefreshCw, Snowflake, Target } from 'lucide-react'
import { api } from '../../api/client'
import type { NodeBrief, Overview } from '../../api/types'
import { Badge, Button, Card, ErrorBanner, Modal, Pre, Spinner, Stat, statusTone } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import type { AsyncState } from '../../hooks/useAsync'
import { fmtPct, fmtTime, pretty } from '../../lib/format'

export function NodeDetailModal({ taskID, nodeID, onClose, onOpen }: { taskID: number; nodeID: number | null; onClose: () => void; onOpen: (id: number) => void }) {
  const d = useAsync(() => api.node(taskID, nodeID!), [taskID, nodeID], nodeID !== null)
  const n = d.data?.node
  return (
    <Modal open={nodeID !== null} wide title={`推演节点 #${nodeID}`} onClose={onClose}>
      <ErrorBanner error={d.error} onRetry={d.reload} />
      {d.loading && !n ? <Spinner /> : n && (
        <div className="space-y-3 text-sm">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone="purple">{n.kind}</Badge>
            <Badge tone={statusTone(n.state)}>{n.state}</Badge>
            {n.priority ? <Badge>优先级 {n.priority}</Badge> : null}
            {n.owner && <Badge tone="blue">{n.owner}</Badge>}
            {n.inherited && <Badge tone="amber">继承自任务 #{n.source_task_id}</Badge>}
            {n.finding_id ? <Badge tone="green">Finding #{n.finding_id}</Badge> : null}
          </div>
          {n.blocked_reason && <p className="text-amber-700">阻塞原因：{n.blocked_reason}</p>}
          {n.delete_reason && <p className="text-red-700">撤销原因：{n.delete_reason}</p>}
          <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-slate-500 md:grid-cols-4">
            <span>创建 {fmtTime(n.created_at)}</span><span>更新 {fmtTime(n.updated_at)}</span>
            <span>完成 {fmtTime(n.completed_at)}</span><span>内容版本 v{n.content_version} · 轮次 {n.active_round}</span>
            {n.lease_until && <span>租约至 {fmtTime(n.lease_until)}（epoch {n.lease_epoch}）</span>}
            {n.cold_since_round != null && <span>冷节点（自第 {n.cold_since_round} 轮）</span>}
            {n.anchors && n.anchors.length > 0 && <span className="col-span-2">锚定招标要素：{n.anchors.map((a) => `#${a}`).join(', ')}</span>}
          </div>
          <div>
            <div className="mb-1 text-xs font-medium text-slate-500">Payload</div>
            <Pre>{pretty(n.payload)}</Pre>
          </div>
          <div>
            <div className="mb-1 text-xs font-medium text-slate-500">关系边（{d.data?.edges?.length ?? 0}）</div>
            {(d.data?.edges ?? []).length === 0 ? <p className="text-xs text-slate-400">无</p> : (
              <div className="flex flex-wrap gap-1.5">
                {d.data!.edges!.map((e, i) => {
                  const other = e.From === n.id ? e.To : e.From
                  return (
                    <button key={i} onClick={() => onOpen(other)} className="rounded border border-slate-200 px-2 py-0.5 text-xs hover:bg-slate-50">
                      #{e.From} <span className="text-blue-600">—{e.Rel}→</span> #{e.To}
                    </button>
                  )
                })}
              </div>
            )}
          </div>
        </div>
      )}
    </Modal>
  )
}

export function NodeList({ title, icon, nodes, onOpen, empty, render }:
  { title: string; icon: ReactNode; nodes: NodeBrief[] | null | undefined; onOpen: (id: number) => void; empty?: string; render?: (n: NodeBrief) => ReactNode }) {
  const list = nodes ?? []
  return (
    <Card title={<>{icon} {title} <span className="font-normal text-slate-400">{list.length}</span></>} bodyClassName="p-0">
      {list.length === 0 ? <div className="px-4 py-3 text-xs text-slate-400">{empty ?? '无'}</div> : (
        <ul className="max-h-72 divide-y divide-slate-100 overflow-y-auto">
          {list.map((n) => (
            <li key={n.id} className="px-4 py-2 text-sm">
              <div className="flex items-start gap-2">
                <button className="shrink-0 font-mono text-xs text-blue-600 hover:underline" onClick={() => onOpen(n.id)}>#{n.id}</button>
                <span className="flex-1 break-all text-slate-700">{n.summary || '（无摘要）'}</span>
                {n.state && <Badge tone={statusTone(n.state)}>{n.state}</Badge>}
                {n.priority ? <span className="text-xs text-slate-400">P{n.priority}</span> : null}
              </div>
              {n.blocked_reason && <div className="mt-0.5 pl-10 text-xs text-amber-700">阻塞：{n.blocked_reason}</div>}
              {render?.(n)}
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}

/** OverviewSummary 有界推演摘要（overview）：不是完整探索图，也不能据此判断全部要求已处理。 */
export function OverviewSummary({ taskID, overview }: { taskID: number; overview: AsyncState<Overview> }) {
  const [nodeID, setNodeID] = useState<number | null>(null)
  const coverage = useAsync(() => api.coverage(taskID), [taskID])
  const o = overview.data
  const reload = () => { void overview.reload(); void coverage.reload() }
  return (
    <div className="space-y-4">
      <ErrorBanner error={overview.error} onRetry={overview.reload} />
      <div className="grid gap-3 sm:grid-cols-2">
        <Stat label="招标要素覆盖率（含来源任务）" value={fmtPct(coverage.data?.pct)} hint={`${coverage.data?.tested ?? 0} / ${coverage.data?.target ?? 0} 个要素已有事实`} />
        <Stat label="已折叠冷节点" value={o?.folded_nodes ?? 0} hint="概览为有界摘要" />
      </div>
      <div className="flex justify-end"><Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={reload} loading={overview.loading}>刷新</Button></div>
      {overview.loading && !o ? <Spinner /> : o && (
        <>
          <NodeList title="目标" icon={<Target className="size-4 text-link" />} nodes={o.goals} onOpen={setNodeID} />
          <NodeList title="最近事实" icon={<FileSearch className="size-4 text-muted" />} nodes={o.recent_facts} onOpen={setNodeID} />
          <NodeList title="最近结论" icon={<CheckCircle2 className="size-4 text-ok" />} nodes={o.recent_findings} onOpen={setNodeID} />
          {(o.hints?.length ?? 0) > 0 && <NodeList title="人工提示" icon={<MessageSquare className="size-4 text-muted" />} nodes={o.hints} onOpen={setNodeID} />}
          {(o.digests?.length ?? 0) > 0 && (
            <Card title={<><Layers className="size-4 text-muted" /> 冷节点摘要（Digest）</>} bodyClassName="p-0">
              <ul className="divide-y divide-line-soft">
                {o.digests!.map((d) => (
                  <li key={d.id} className="px-4 py-2 text-sm">
                    <div className="flex flex-wrap items-center gap-2">
                      <button type="button" className="font-mono text-xs text-link hover:underline" onClick={() => setNodeID(d.id)}>#{d.id}</button>
                      <span className="font-medium">{d.title}</span>
                      <span className="text-xs text-muted">第 {d.round} 轮 · 折叠 {d.member_ids.length} 个节点</span>
                      {d.stale && <Badge tone="amber">已过期（{d.changed_members?.length ?? 0} 个成员变更）</Badge>}
                    </div>
                    <p className="mt-0.5 text-xs text-muted">{d.summary}</p>
                  </li>
                ))}
              </ul>
            </Card>
          )}
          {(o.cold_index?.length ?? 0) > 0 && (
            <Card title={<><Snowflake className="size-4 text-muted" /> 冷节点索引</>} bodyClassName="p-0">
              <ul className="divide-y divide-line-soft">
                {o.cold_index!.map((c) => (
                  <li key={c.id} className="flex items-center gap-2 px-4 py-1.5 text-sm">
                    <button type="button" className="font-mono text-xs text-link hover:underline" onClick={() => setNodeID(c.id)}>#{c.id}</button>
                    <Badge>{c.kind}</Badge><Badge tone={statusTone(c.state)}>{c.state}</Badge>
                    <span className="truncate text-muted">{c.summary}</span>
                  </li>
                ))}
              </ul>
            </Card>
          )}
        </>
      )}
      <NodeDetailModal taskID={taskID} nodeID={nodeID} onClose={() => setNodeID(null)} onOpen={setNodeID} />
    </div>
  )
}
