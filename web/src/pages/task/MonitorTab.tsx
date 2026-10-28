import { useState } from 'react'
import type { ReactNode } from 'react'
import {
  AlertTriangle, Bot, CheckCircle2, CircleDot, FileSearch, Layers, MessageSquare, Pause, RefreshCw, ShieldAlert, Snowflake,
  Square, Target, Terminal, Wrench, XCircle,
} from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { InterceptPending, NodeBrief, Overview } from '../../api/types'
import { InterceptList } from '../../components/Intercepts'
import { Badge, Button, Card, Empty, ErrorBanner, Input, Modal, Pre, Spinner, Stat, cx, statusTone, useToast } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import type { AsyncState } from '../../hooks/useAsync'
import type { StreamEvent, StreamStatus } from '../../hooks/useTaskEvents'
import { fmtPct, fmtTime, pretty } from '../../lib/format'

interface Stream { events: StreamEvent[]; status: StreamStatus; dropped: number; reconnects: number; clear: () => void }

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

function NodeList({ title, icon, nodes, onOpen, empty, render }:
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

function WorkControls({ taskID, n, paused, onDone }: { taskID: number; n: NodeBrief; paused?: boolean; onDone: () => void }) {
  const toast = useToast()
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState('')
  const act = async (kind: string, fn: () => Promise<unknown>, ok: string) => {
    setBusy(kind)
    try { await fn(); toast('ok', ok); setMsg(''); onDone() } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  return (
    <div className="mt-1.5 flex flex-wrap items-center gap-1.5 pl-10">
      {n.owner && <Badge tone="blue"><Bot className="size-3" />{n.owner}</Badge>}
      {!paused && <Button size="sm" icon={<Pause className="size-3" />} loading={busy === 'pause'}
        onClick={() => act('pause', () => api.controlWork(taskID, n.id, 'pause'), `已暂停工作 #${n.id}`)}>暂停</Button>}
      <Button size="sm" variant="ghost" icon={<Square className="size-3 text-red-500" />} loading={busy === 'cancel'}
        onClick={() => confirm(`取消工作 #${n.id}？`) && act('cancel', () => api.controlWork(taskID, n.id, 'cancel'), `已取消工作 #${n.id}`)}>取消</Button>
      <Input className="h-7 min-w-40 flex-1 py-0.5 text-xs" value={msg} onChange={(e) => setMsg(e.target.value)}
        placeholder={paused ? '补充说明并恢复此工作…' : '插话引导正在执行的 Worker…'} />
      {paused ? (
        <Button size="sm" variant="primary" icon={<MessageSquare className="size-3" />} disabled={!msg.trim()} loading={busy === 'msg'}
          onClick={() => act('msg', () => api.messageWork(taskID, n.id, msg.trim()), `已发送并恢复工作 #${n.id}`)}>发送并恢复</Button>
      ) : (
        <Button size="sm" variant="primary" icon={<MessageSquare className="size-3" />} disabled={!msg.trim()} loading={busy === 'steer'}
          onClick={() => act('steer', () => api.steerWork(taskID, n.id, msg.trim()), `引导消息已排队（#${n.id}）`)}>引导</Button>
      )}
    </div>
  )
}

const kindMeta: Record<string, { label: string; tone: 'slate' | 'blue' | 'green' | 'amber' | 'red' | 'purple'; icon: ReactNode }> = {
  tool: { label: '工具调用', tone: 'blue', icon: <Wrench className="size-3.5" /> },
  tool_error: { label: '工具错误', tone: 'red', icon: <XCircle className="size-3.5" /> },
  text: { label: '阶段结论', tone: 'green', icon: <CheckCircle2 className="size-3.5" /> },
  intercept_request: { label: '审批请求', tone: 'amber', icon: <ShieldAlert className="size-3.5" /> },
  intercept_decided: { label: '审批结果', tone: 'purple', icon: <ShieldAlert className="size-3.5" /> },
  round: { label: '规划轮次', tone: 'slate', icon: <Target className="size-3.5" /> },
  state: { label: '状态', tone: 'slate', icon: <CircleDot className="size-3.5" /> },
  error: { label: '错误', tone: 'red', icon: <AlertTriangle className="size-3.5" /> },
}

function ActivityFeed({ stream, onOpen, onGoApprovals }: { stream: Stream; onOpen: (id: number) => void; onGoApprovals: () => void }) {
  const [filter, setFilter] = useState('')
  const [expanded, setExpanded] = useState<string | null>(null)
  const list = filter ? stream.events.filter((e) => e.kind === filter) : stream.events
  const statusText = { connecting: '连接中', open: '实时', reconnecting: '重连中', closed: '已断开' }[stream.status]
  return (
    <Card className="flex flex-col" bodyClassName="p-0"
      title={<><Terminal className="size-4 text-slate-500" /> 实时活动流
        <Badge tone={stream.status === 'open' ? 'green' : stream.status === 'closed' ? 'slate' : 'amber'}>{statusText}</Badge>
        {stream.reconnects > 0 && <span className="text-xs font-normal text-slate-400">已重连 {stream.reconnects} 次</span>}</>}
      actions={<>
        <select className="rounded border border-slate-300 px-1 py-0.5 text-xs" value={filter} onChange={(e) => setFilter(e.target.value)}>
          <option value="">全部</option>
          {Object.entries(kindMeta).map(([k, v]) => <option key={k} value={k}>{v.label}</option>)}
        </select>
        <Button size="sm" variant="ghost" onClick={stream.clear}>清空</Button>
      </>}>
      {stream.dropped > 0 && (
        <div className="flex items-center gap-2 border-b border-amber-200 bg-amber-50 px-3 py-1.5 text-xs text-amber-800">
          <AlertTriangle className="size-3.5" /> 客户端消费过慢，服务端已丢弃 {stream.dropped} 条活动事件；推演态势以“刷新”结果为准。
        </div>
      )}
      <div className="max-h-[720px] min-h-64 overflow-y-auto">
        {list.length === 0 ? <Empty>{stream.status === 'open' ? '等待活动事件…（任务运行时 Worker 的工具调用、结论与审批请求会实时出现）' : '活动流未连接'}</Empty> : (
          <ul className="divide-y divide-slate-100">
            {list.map((e, i) => {
              const m = kindMeta[e.kind] ?? { label: e.kind, tone: 'slate' as const, icon: <CircleDot className="size-3.5" /> }
              const key = `${e.seq}-${i}`
              const isOpen = expanded === key
              return (
                <li key={key} className={cx('px-3 py-2 text-sm', e.kind === 'intercept_request' && 'bg-amber-50', e.kind === 'tool_error' && 'bg-red-50/50')}>
                  <div className="flex items-start gap-2">
                    <Badge tone={m.tone}>{m.icon}{m.label}</Badge>
                    <div className="min-w-0 flex-1">
                      <div className={cx('break-all', e.kind === 'text' ? 'text-slate-800' : 'text-slate-700', e.kind === 'tool' && 'font-mono text-xs')}>{e.summary}</div>
                      <div className="mt-0.5 flex flex-wrap gap-2 text-xs text-slate-400">
                        <span>{new Date(e.at).toLocaleTimeString('zh-CN', { hour12: false })}</span>
                        {e.worker && <span>{e.worker}</span>}
                        {e.node_id ? <button className="text-blue-600 hover:underline" onClick={() => onOpen(e.node_id!)}>#{e.node_id}</button> : null}
                        {e.detail && <button className="text-slate-500 hover:underline" onClick={() => setExpanded(isOpen ? null : key)}>{isOpen ? '收起' : '详情'}</button>}
                        {e.kind === 'intercept_request' && <button className="font-medium text-amber-700 hover:underline" onClick={onGoApprovals}>去审批 →</button>}
                      </div>
                      {isOpen && e.detail && <Pre className="mt-1">{pretty(e.detail)}</Pre>}
                    </div>
                  </div>
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </Card>
  )
}

export default function MonitorTab({ taskID, overview, stream, pending, onDecided }:
  { taskID: number; overview: AsyncState<Overview>; stream: Stream; pending: InterceptPending[]; onDecided: () => void }) {
  const [nodeID, setNodeID] = useState<number | null>(null)
  const coverage = useAsync(() => api.coverage(taskID), [taskID])
  const o = overview.data
  const reload = () => { void overview.reload(); void coverage.reload() }
  const goApprovals = () => document.getElementById('monitor-approvals')?.scrollIntoView({ behavior: 'smooth' })

  return (
    <div className="space-y-4">
      <ErrorBanner error={overview.error} onRetry={overview.reload} />
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
        <Stat label="要素覆盖率（含来源任务）" value={fmtPct(coverage.data?.pct)} hint={`${coverage.data?.tested ?? 0} / ${coverage.data?.target ?? 0} 个要素已有事实`} />
        <Stat label="进行中工作" value={o?.running_intents?.length ?? 0} hint={`待领取 ${o?.open_intents?.length ?? 0} · 暂停 ${o?.paused_intents?.length ?? 0}`} />
        <Stat label="最近事实" value={o?.recent_facts?.length ?? 0} />
        <Stat label="最近结论" value={o?.recent_findings?.length ?? 0} />
        <Stat label="待审批" value={pending.length} tone={pending.length ? 'amber' : undefined} hint={`已折叠 ${o?.folded_nodes ?? 0} 个节点`} />
      </div>
      <div className="grid gap-4 xl:grid-cols-5">
        <div className="space-y-4 xl:col-span-3">
          <div className="flex items-center justify-between">
            <h2 className="text-sm font-semibold text-slate-700">推演态势（有界概览）</h2>
            <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={reload} loading={overview.loading}>刷新</Button>
          </div>
          {overview.loading && !o ? <Spinner /> : o && (
            <>
              <NodeList title="目标" icon={<Target className="size-4 text-blue-600" />} nodes={o.goals} onOpen={setNodeID} />
              <NodeList title="执行中的工作槽" icon={<Bot className="size-4 text-emerald-600" />} nodes={o.running_intents} onOpen={setNodeID}
                empty="当前没有正在执行的 Worker" render={(n) => <WorkControls taskID={taskID} n={n} onDone={reload} />} />
              {(o.paused_intents?.length ?? 0) > 0 && (
                <NodeList title="已暂停的工作" icon={<Pause className="size-4 text-amber-600" />} nodes={o.paused_intents} onOpen={setNodeID}
                  render={(n) => <WorkControls taskID={taskID} n={n} paused onDone={reload} />} />
              )}
              <NodeList title="待领取意图" icon={<CircleDot className="size-4 text-slate-500" />} nodes={o.open_intents} onOpen={setNodeID} />
              <div className="grid gap-4 md:grid-cols-2">
                <NodeList title="最近事实" icon={<FileSearch className="size-4 text-slate-500" />} nodes={o.recent_facts} onOpen={setNodeID} />
                <NodeList title="最近结论" icon={<CheckCircle2 className="size-4 text-emerald-600" />} nodes={o.recent_findings} onOpen={setNodeID} />
              </div>
              <NodeList title="最近结束的意图" icon={<Square className="size-4 text-slate-400" />} nodes={o.recent_terminal_intents} onOpen={setNodeID} />
              {(o.hints?.length ?? 0) > 0 && <NodeList title="人工提示" icon={<MessageSquare className="size-4 text-violet-600" />} nodes={o.hints} onOpen={setNodeID} />}
              {(o.digests?.length ?? 0) > 0 && (
                <Card title={<><Layers className="size-4 text-slate-500" /> 冷节点摘要（Digest）</>} bodyClassName="p-0">
                  <ul className="divide-y divide-slate-100">
                    {o.digests!.map((d) => (
                      <li key={d.id} className="px-4 py-2 text-sm">
                        <div className="flex items-center gap-2">
                          <button className="font-mono text-xs text-blue-600 hover:underline" onClick={() => setNodeID(d.id)}>#{d.id}</button>
                          <span className="font-medium">{d.title}</span>
                          <span className="text-xs text-slate-400">第 {d.round} 轮 · 折叠 {d.member_ids.length} 个节点</span>
                          {d.stale && <Badge tone="amber">已过期（{d.changed_members?.length ?? 0} 个成员变更）</Badge>}
                        </div>
                        <p className="mt-0.5 text-xs text-slate-600">{d.summary}</p>
                      </li>
                    ))}
                  </ul>
                </Card>
              )}
              {(o.cold_index?.length ?? 0) > 0 && (
                <Card title={<><Snowflake className="size-4 text-sky-500" /> 冷节点索引</>} bodyClassName="p-0">
                  <ul className="divide-y divide-slate-100">
                    {o.cold_index!.map((c) => (
                      <li key={c.id} className="flex items-center gap-2 px-4 py-1.5 text-sm">
                        <button className="font-mono text-xs text-blue-600 hover:underline" onClick={() => setNodeID(c.id)}>#{c.id}</button>
                        <Badge>{c.kind}</Badge><Badge tone={statusTone(c.state)}>{c.state}</Badge>
                        <span className="truncate text-slate-600">{c.summary}</span>
                      </li>
                    ))}
                  </ul>
                </Card>
              )}
            </>
          )}
        </div>
        <div className="space-y-4 xl:col-span-2">
          <div id="monitor-approvals">
            <InterceptList rows={pending} loading={false} error="" compact onReload={onDecided} onDecided={onDecided} />
          </div>
          <ActivityFeed stream={stream} onOpen={setNodeID} onGoApprovals={goApprovals} />
        </div>
      </div>
      <NodeDetailModal taskID={taskID} nodeID={nodeID} onClose={() => setNodeID(null)} onOpen={setNodeID} />
    </div>
  )
}
