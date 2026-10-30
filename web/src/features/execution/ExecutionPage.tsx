import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import {
  AlertTriangle, ArrowDown, ChevronDown, CircleCheck, Clock3, Ellipsis, ListTree, PanelLeft, Pause, Send, ShieldAlert, Square, Wrench, XCircle,
} from 'lucide-react'
import { ApiError, api, errorText } from '../../api/client'
import type { NodeBrief } from '../../api/types'
import { Badge, Button, Drawer, Empty, Input, Menu, Notice, Select, Spinner, cx, statusTone, useConfirm, useToast } from '../../components/ui'
import { useAsync, useElementWidth } from '../../hooks/useAsync'
import type { StreamEvent } from '../../hooks/useTaskEvents'
import { pretty } from '../../lib/format'
import { linkProps, useSearchParams } from '../../lib/router'
import { ACTIVITY_KINDS, parseWork, pickEnum, taskUrl } from '../../lib/routes'
import type { ActivityKind } from '../../lib/routes'
import { useTaskWorkspace } from '../task-workspace/TaskWorkspaceProvider'
import { NodeDetailModal, OverviewSummary } from './NodeParts'

// 执行记录 · 实时过程（图 2 / 图 4 / 图 7，V02 / V04 / V07）：
// 选择工作 → 查看对应活动 → 展开工具详情 → 发送工作引导。左栏 288px 工作列表，右栏三行：标题控制 / 独立滚动活动 / 底部引导区。
// 活动只是本次打开期间收到的实时事件；overview 是有界推演摘要，不代表全部工作历史。

const stateLabel: Record<string, string> = {
  open: '待执行', running: '运行中', paused: '已暂停', done: '已完成', blocked: '已阻塞', exhausted: '配额耗尽', stopped: '已停止', deleted: '已撤销',
}
const kindLabel: Record<string, string> = {
  all: '全部类型', round: '规划轮次', text: '阶段结论', tool: '工具调用', tool_error: '工具错误', state: '状态', error: '错误',
  intercept_request: '审批请求', intercept_decided: '审批结果',
}
const kindIcon: Record<string, ReactNode> = {
  tool: <Wrench className="size-4" aria-hidden />, tool_error: <XCircle className="size-4 text-danger" aria-hidden />,
  error: <AlertTriangle className="size-4 text-danger" aria-hidden />, intercept_request: <ShieldAlert className="size-4 text-warn" aria-hidden />,
}

type Group = { key: string; label: string; nodes: NodeBrief[] }

// 引导草稿按 任务 + 工作 在内存中隔离，切换工作不会把 A 的草稿发给 B。
const steerDrafts = new Map<string, string>()

function EventItem({ e, open, onToggle }: { e: StreamEvent; open: boolean; onToggle: () => void }) {
  const t = new Date(e.at)
  const time = Number.isNaN(t.getTime()) ? '—' : t.toLocaleTimeString('zh-CN', { hour12: false, timeZone: 'Asia/Shanghai' })
  const detail = useMemo(() => (open && e.detail ? pretty(e.detail) : ''), [open, e.detail])
  const toolish = e.kind === 'tool' || e.kind === 'tool_error'
  return (
    <li className={cx('rounded-[10px] bg-soft p-3', e.kind === 'intercept_request' && 'bg-[#fffbeb]', (e.kind === 'tool_error' || e.kind === 'error') && 'bg-[#fef2f2]')}>
      <div className="mb-1.5 text-xs leading-4 text-muted">
        {time} · {e.node_id ? `工作 #${e.node_id}` : '项目'} · {kindLabel[e.kind] ?? e.kind}{e.worker ? ` · ${e.worker}` : ''}
      </div>
      {toolish || e.detail ? (
        <div className="flex items-start justify-between gap-3">
          <span className="flex min-w-0 items-start gap-2">{kindIcon[e.kind] ?? null}<span className="min-w-0 break-words">{e.summary}</span></span>
          {e.detail && <button type="button" aria-expanded={open} onClick={onToggle} className="shrink-0 text-xs text-muted hover:text-fg">{open ? '收起详情' : '展开详情'}</button>}
        </div>
      ) : <p className="break-words">{e.summary}</p>}
      {open && e.detail && <>
        <pre className="mt-2.5 max-h-80 overflow-auto rounded-lg border border-line bg-panel p-3 font-mono text-xs leading-[18px] whitespace-pre-wrap [overflow-wrap:anywhere]">{detail}</pre>
        <p className="mt-2 text-xs text-muted">活动摘要；可能经过截断，不代表完整工具输入或返回。</p>
      </>}
    </li>
  )
}

export default function ExecutionPage() {
  const toast = useToast()
  const confirm = useConfirm()
  const { taskId, overview, stream, intercepts, invalidate } = useTaskWorkspace()
  const [params, setQuery] = useSearchParams()
  const work = parseWork(params.get('work'))
  const kind = pickEnum<ActivityKind>(params.get('kind'), ACTIVITY_KINDS, 'all')
  const [boxRef, width] = useElementWidth<HTMLDivElement>()
  const narrow = width > 0 && width < 800
  const [pickerOpen, setPickerOpen] = useState(false)
  const [summaryOpen, setSummaryOpen] = useState(false)
  const [nodeDetail, setNodeDetail] = useState<number | null>(null)
  const [busy, setBusy] = useState('')
  const [sending, setSending] = useState<string>('')
  const [, rerender] = useState(0)

  const o = overview.data
  const groups: Group[] = [
    { key: 'running', label: '正在执行', nodes: o?.running_intents ?? [] },
    { key: 'open', label: '待执行', nodes: o?.open_intents ?? [] },
    { key: 'paused', label: '已暂停', nodes: o?.paused_intents ?? [] },
    { key: 'terminal', label: '最近结束', nodes: o?.recent_terminal_intents ?? [] },
  ]
  const groupOf = new Map<number, { node: NodeBrief; group: string }>()
  for (const g of groups) for (const n of g.nodes) if (!groupOf.has(n.id)) groupOf.set(n.id, { node: n, group: g.key })
  const total = groupOf.size

  // 不在摘要中的工作 ID：用 api.node 校验任务归属与节点类型，不静默选中其他工作。
  const selectedId = typeof work === 'number' ? work : undefined
  const known = selectedId ? groupOf.get(selectedId) : undefined
  const probe = useAsync(() => api.node(taskId, selectedId!), [taskId, selectedId], !!selectedId && !known && !!o)
  const probedNode = probe.data?.node
  const probeError = selectedId && !known && o ? (probe.error ? (probe.error.startsWith('HTTP 404') ? `本项目中没有节点 #${selectedId}` : probe.error)
    : probedNode && probedNode.kind !== 'intent' ? `节点 #${selectedId} 不是工作节点（类型 ${probedNode.kind}）` : '') : ''
  const selected: NodeBrief | undefined = known?.node ?? (probedNode && probedNode.kind === 'intent'
    ? { id: probedNode.id, state: probedNode.state, owner: probedNode.owner, blocked_reason: probedNode.blocked_reason, summary: String(probedNode.payload?.summary ?? probedNode.payload?.title ?? '') }
    : undefined)
  const state = known ? (known.group === 'running' ? 'running' : known.group === 'paused' ? 'paused' : known.node.state ?? '') : selected?.state ?? ''

  // 活动：只按 node_id 精确匹配当前工作；不按 worker 名称或相似文字归入。不修改共享事件数组。
  const events = useMemo(() => stream.events.filter((e) => (selectedId === undefined || e.node_id === selectedId) && (kind === 'all' || e.kind === kind)),
    [stream.events, selectedId, kind])

  // 滚动：位于底部时跟随新事件；向上阅读时保持位置并提示「有新活动」。每个工作保留各自的滚动位置与展开项。
  const scrollRef = useRef<HTMLDivElement>(null)
  const viewKey = `${selectedId ?? 'all'}:${kind}`
  const positions = useRef(new Map<string, number>())
  const expanded = useRef(new Map<string, Set<string>>())
  const atBottom = useRef(true)
  const [unseen, setUnseen] = useState(0)
  const lastCount = useRef(events.length)
  const openSet = expanded.current.get(viewKey) ?? new Set<string>()
  useLayoutEffect(() => {
    const el = scrollRef.current
    if (!el) return
    const saved = positions.current.get(viewKey)
    el.scrollTop = saved ?? el.scrollHeight
    atBottom.current = saved === undefined || el.scrollHeight - el.scrollTop - el.clientHeight < 24
    lastCount.current = events.length
    setUnseen(0)
  }, [viewKey]) // eslint-disable-line react-hooks/exhaustive-deps
  useLayoutEffect(() => {
    const el = scrollRef.current
    if (!el) return
    const added = events.length - lastCount.current
    lastCount.current = events.length
    if (added <= 0) return
    if (atBottom.current) el.scrollTop = el.scrollHeight
    else setUnseen((n) => n + added)
  }, [events.length])
  const onScroll = () => {
    const el = scrollRef.current
    if (!el) return
    positions.current.set(viewKey, el.scrollTop)
    atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
    if (atBottom.current) setUnseen(0)
  }
  const toggle = (uid: string) => {
    const set = new Set(openSet)
    if (set.has(uid)) set.delete(uid)
    else set.add(uid)
    expanded.current.set(viewKey, set)
    rerender((n) => n + 1)
  }
  // 被缓存淘汰的条目不再显示；其展开状态随之丢弃。
  useEffect(() => {
    const live = new Set(stream.events.map((e) => e.uid))
    for (const [k, set] of expanded.current) expanded.current.set(k, new Set([...set].filter((u) => live.has(u))))
  }, [stream.events])

  const select = (w: 'all' | number) => { setQuery({ work: w === 'all' ? undefined : w }, { replace: false }); setPickerOpen(false) }

  const control = async (action: 'pause' | 'cancel') => {
    if (!selectedId) return
    if (action === 'cancel' && !(await confirm({ title: `取消工作 #${selectedId}？`, message: '工作将停止，已产生的事实与结论保留。', confirmLabel: '取消工作', danger: true }))) return
    setBusy(action)
    try { await api.controlWork(taskId, selectedId, action); toast('ok', action === 'pause' ? `已暂停工作 #${selectedId}` : `已取消工作 #${selectedId}`); invalidate('run_controlled') }
    catch (e) { toast('error', errorText(e)); invalidate(['overview']) } finally { setBusy('') }
  }

  const draftKey = selectedId ? `${taskId}:${selectedId}` : ''
  const [text, setText] = useState(() => (draftKey ? steerDrafts.get(draftKey) ?? '' : ''))
  useEffect(() => { setText(draftKey ? steerDrafts.get(draftKey) ?? '' : '') }, [draftKey])
  const mode: 'steer' | 'message' | 'readonly' | 'all' = !selectedId ? 'all' : state === 'running' ? 'steer' : state === 'paused' ? 'message' : 'readonly'
  const send = async () => {
    // 使用提交瞬间的工作 ID 与文本；在途禁止重复提交，成功只清除对应工作的草稿。
    const id = selectedId, msg = text.trim(), k = draftKey, m = mode
    if (!id || !msg || sending || (m !== 'steer' && m !== 'message')) return
    setSending(k)
    try {
      if (m === 'steer') { await api.steerWork(taskId, id, msg); toast('ok', `引导已排队（工作 #${id}）`) }
      else { await api.messageWork(taskId, id, msg); toast('ok', `已发送并恢复工作 #${id}`) }
      steerDrafts.delete(k)
      if (k === draftKeyRef.current) setText('')
      invalidate(['overview', 'task'])
    } catch (e) {
      toast('error', e instanceof ApiError && e.status === 409 ? `工作 #${id} 状态已变化，未发送：${e.message}` : errorText(e))
      invalidate(['overview'])
    } finally { setSending('') }
  }
  const draftKeyRef = useRef(draftKey)
  draftKeyRef.current = draftKey

  const statusLabel = { connecting: '连接中', open: '已连接', reconnecting: '重连中', closed: '已断开' }[stream.status]
  // 待审批提示只在全部活动或所属工作下显示，避免把其他工作的审批串到当前工作。
  const pending = (intercepts.data ?? []).filter((p) => selectedId === undefined || p.intent_id === selectedId).length
  const title = selectedId ? `工作 #${selectedId}${selected?.summary ? ` · ${selected.summary}` : ''}` : '全部实时活动'

  const row = (n: NodeBrief, group: string) => (
    <button key={`${group}-${n.id}`} type="button" aria-current={selectedId === n.id ? 'true' : undefined} onClick={() => select(n.id)}
      title={`#${n.id} ${n.summary}${n.blocked_reason ? `\n阻塞：${n.blocked_reason}` : ''}`}
      className={cx('flex min-h-11 w-full items-center gap-2 rounded-lg p-2 text-left hover:bg-soft', selectedId === n.id && 'bg-soft')}>
      {group === 'paused' ? <Pause className="size-3.5 shrink-0" aria-hidden /> : group === 'terminal' ? <CircleCheck className="size-3.5 shrink-0" aria-hidden /> : <Clock3 className="size-3.5 shrink-0" aria-hidden />}
      <span className="min-w-0 flex-1 truncate">#{n.id} {n.summary || '（无摘要）'}</span>
      {group === 'running' && <Badge dot tone="blue">实时</Badge>}
      {group === 'terminal' && n.state && <span className="shrink-0 text-xs text-muted">{stateLabel[n.state] ?? n.state}</span>}
    </button>
  )
  const list = (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        <div className="px-2 pt-3 pb-1 text-xs text-muted">项目动态</div>
        <button type="button" aria-current={selectedId === undefined ? 'true' : undefined} onClick={() => select('all')}
          className={cx('flex min-h-11 w-full items-center gap-2 rounded-lg p-2 text-left hover:bg-soft', selectedId === undefined && 'bg-soft')}>
          <Clock3 className="size-3.5" aria-hidden /><span className="flex-1">全部实时活动</span>
          {stream.events.length > 0 && <span className="text-xs text-muted">{stream.events.length}</span>}
        </button>
        {overview.loading && !o ? <Spinner /> : overview.error && !o ? <p className="px-2 py-3 text-xs text-danger">工作摘要读取失败：{overview.error}</p> : groups.filter((g) => g.nodes.length > 0).map((g) => (
          <div key={g.key}>
            <div className="px-2 pt-3 pb-1 text-xs text-muted">{g.label}</div>
            {g.nodes.map((n) => row(n, g.key))}
          </div>
        ))}
        {o && total === 0 && <p className="px-2 py-3 text-xs text-muted">当前摘要中没有待执行、执行中或最近结束的工作。</p>}
        {selected && !known && (
          <div><div className="px-2 pt-3 pb-1 text-xs text-muted">不在当前摘要中</div>{row(selected, 'other')}</div>
        )}
      </div>
      <div className="flex items-center justify-between gap-2 border-t border-line px-4 py-3 text-xs text-muted">
        <span>本次打开期间的实时活动</span>
        <button type="button" className="inline-flex items-center gap-1 hover:text-fg" onClick={() => setSummaryOpen(true)}><ListTree className="size-3.5" aria-hidden />推演摘要</button>
      </div>
    </div>
  )

  return (
    <div ref={boxRef} data-region="execution" className={cx('grid min-h-0 flex-1', narrow ? 'grid-cols-[minmax(0,1fr)] grid-rows-[32px_minmax(0,1fr)] gap-2' : 'grid-cols-[288px_minmax(0,1fr)] gap-4')}>
      {narrow ? (
        <Button className="w-full" icon={<PanelLeft className="size-4" />} onClick={() => setPickerOpen(true)} aria-haspopup="dialog">
          <span className="truncate">工作列表 · {selectedId ? (selected?.summary || `#${selectedId}`) : '全部实时活动'}</span><ChevronDown className="size-4" aria-hidden />
        </Button>
      ) : (
        <aside data-region="work-list" aria-label="工作列表" className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-[10px] border border-line bg-panel">
          <div className="min-h-12 border-b border-line px-4 py-3">
            <h2 className="text-sm font-semibold">工作列表</h2>
            <p className="text-xs text-muted">当前摘要 · {total} 项工作</p>
          </div>
          {list}
        </aside>
      )}

      <section data-region="activity-pane" aria-label={title} className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-[10px] border border-line bg-panel">
        <div className="flex min-h-12 flex-wrap items-center justify-between gap-2 border-b border-line px-4 py-3">
          <div className="flex min-w-0 items-center gap-2">
            <h2 className={cx('min-w-0 truncate font-semibold', narrow ? 'text-[13px]' : 'text-sm')} title={title}>{title}</h2>
            {selectedId ? state && <Badge dot tone={statusTone(state)}>{stateLabel[state] ?? state}</Badge>
              : <Badge dot tone={stream.status === 'open' ? 'green' : stream.status === 'closed' ? 'slate' : 'amber'}>{statusLabel}</Badge>}
          </div>
          {narrow ? (
            <div className="flex items-center gap-1.5">
              {selectedId && state === 'running' && <Button size="sm" icon={<Pause className="size-4" />} loading={busy === 'pause'} onClick={() => void control('pause')}>暂停</Button>}
              <Menu label="更多活动操作" trigger={<span className="grid size-7 place-items-center rounded-lg border border-line"><Ellipsis className="size-4" aria-hidden /></span>}
                items={[
                  ...ACTIVITY_KINDS.map((k) => ({ key: `k-${k}`, label: `${kind === k ? '✓ ' : ''}只看：${kindLabel[k]}`, onSelect: () => setQuery({ kind: k === 'all' ? undefined : k }) })),
                  !!selectedId && (state === 'running' || state === 'paused') && { key: 'cancel', label: '取消工作…', danger: true, onSelect: () => void control('cancel') },
                  !!selectedId && { key: 'detail', label: '节点详情', onSelect: () => setNodeDetail(selectedId!) },
                  !selectedId && { key: 'clear', label: '清空显示', onSelect: stream.clear },
                ]} />
            </div>
          ) : (
          <div className="flex flex-wrap items-center gap-1.5">
            <Select aria-label="活动类型" className="h-7 w-28 text-xs" value={kind} onChange={(e) => setQuery({ kind: e.target.value === 'all' ? undefined : e.target.value })}>
              {ACTIVITY_KINDS.map((k) => <option key={k} value={k}>{kindLabel[k]}</option>)}
            </Select>
            {selectedId ? <>
              {state === 'running' && <Button size="sm" icon={<Pause className="size-4" />} loading={busy === 'pause'} onClick={() => void control('pause')}>暂停</Button>}
              {(state === 'running' || state === 'paused') && <Button size="sm" icon={<Square className="size-3.5" />} loading={busy === 'cancel'} onClick={() => void control('cancel')}>取消</Button>}
              <Button size="sm" variant="ghost" onClick={() => setNodeDetail(selectedId)}>节点详情</Button>
            </> : <Button size="sm" variant="ghost" onClick={stream.clear} title="只清空本页显示的活动，不删除业务记录">清空显示</Button>}
          </div>
          )}
        </div>
        <div ref={scrollRef} onScroll={onScroll} data-region="activity-scroll" className="relative flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-4">
          {probeError && <Notice tone="danger" action={<Button size="sm" onClick={() => select('all')}>查看全部活动</Button>}>{probeError}</Notice>}
          {work === undefined && <Notice tone="danger" action={<Button size="sm" onClick={() => select('all')}>查看全部活动</Button>}>工作参数无效：work 只能是 all 或正整数工作 ID。</Notice>}
          {pending > 0 && <Notice tone="warn" action={<a {...linkProps(taskUrl(taskId, 'approvals'))} className="text-sm font-medium underline">去审批</a>}>有 {pending} 项工具调用等待人工审批。</Notice>}
          {stream.dropped > 0 && <Notice>服务端因消费过慢已丢弃 {stream.dropped} 条活动事件；业务状态以刷新结果为准。</Notice>}
          {stream.trimmed > 0 && <p className="text-center text-xs text-muted">客户端最多保留最近 {stream.max} 条活动，已淘汰 {stream.trimmed} 条较早的活动。</p>}
          {stream.reconnects > 0 && <p className="text-center text-xs text-muted">活动流已重连 {stream.reconnects} 次；断开期间的活动可能缺失，不会补拉。</p>}
          {selected?.blocked_reason && <Notice>阻塞原因：{selected.blocked_reason}</Notice>}
          {events.length === 0 ? (
            <Empty>{stream.status !== 'open' ? `活动流${statusLabel}` : selectedId ? '本次打开期间尚未收到该工作的活动。没有工作编号的项目级活动在「全部实时活动」中查看。' : '等待活动事件…（Worker 的工具调用、阶段结论与审批请求会实时出现）'}</Empty>
          ) : (
            <ul className="flex flex-col gap-3">{events.map((e) => <EventItem key={e.uid} e={e} open={openSet.has(e.uid)} onToggle={() => toggle(e.uid)} />)}</ul>
          )}
          <p className="text-center text-xs text-muted">仅展示本次收到的活动，业务状态以刷新结果为准。</p>
          {unseen > 0 && (
            <button type="button" onClick={() => { const el = scrollRef.current; if (el) el.scrollTop = el.scrollHeight; setUnseen(0) }}
              className="sticky bottom-0 mx-auto inline-flex items-center gap-1 rounded-full border border-line bg-panel px-3 py-1 text-xs shadow">
              <ArrowDown className="size-3.5" aria-hidden />有 {unseen} 条新活动</button>
          )}
        </div>
        <div data-region="composer" className="shrink-0 border-t border-line p-3">
          <form className="flex gap-2" onSubmit={(e) => { e.preventDefault(); void send() }}>
            <Input aria-label="引导内容" className="h-9" value={text} disabled={mode === 'all' || mode === 'readonly'}
              placeholder={mode === 'all' ? '选择一个工作后可发送引导' : mode === 'readonly' ? `该工作${stateLabel[state] ?? state ?? ''}，不能发送引导` : mode === 'message' ? '补充说明并恢复该工作…' : '给当前工作补充要求…'}
              onChange={(e) => { setText(e.target.value); if (draftKey) steerDrafts.set(draftKey, e.target.value) }} />
            <Button type="submit" variant="primary" className="h-9" icon={<Send className="size-4" />} loading={!!sending && sending === draftKey}
              disabled={(mode !== 'steer' && mode !== 'message') || !text.trim() || !!sending}>{mode === 'message' ? '发送并恢复' : '引导'}</Button>
          </form>
          <p className="mt-1.5 text-xs leading-5 text-muted">{mode === 'all' ? '项目级「补充提示」「追加目标」在标题栏的「执行操作」中，不是聊天回复。'
            : mode === 'readonly' ? '待执行或已结束的工作只读；不提供会误发的引导。' : '发送后排队处理；不会自动批准要求或章节。'}</p>
        </div>
      </section>

      <Drawer open={pickerOpen} side="left" title="工作列表" onClose={() => setPickerOpen(false)} width={320}>{list}</Drawer>
      <Drawer open={summaryOpen} title="推演摘要（有界概览）" onClose={() => setSummaryOpen(false)} width={560}>
        <div className="p-4"><OverviewSummary taskID={taskId} overview={overview} /></div>
      </Drawer>
      <NodeDetailModal taskID={taskId} nodeID={nodeDetail} onClose={() => setNodeDetail(null)} onOpen={setNodeDetail} />
    </div>
  )
}
