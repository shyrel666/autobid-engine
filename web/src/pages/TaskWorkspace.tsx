import { useCallback, useEffect, useRef, useState } from 'react'
import { ArrowLeft, Flag, Lightbulb, Play, RefreshCw } from 'lucide-react'
import { api, errorText } from '../api/client'
import { Badge, Button, ErrorBanner, Field, Input, Modal, Spinner, Tabs, statusTone, useToast } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { useTaskEvents } from '../hooks/useTaskEvents'
import type { StreamEvent } from '../hooks/useTaskEvents'
import { fmtPct, fmtTime, taskStatusLabel } from '../lib/format'
import { navigate } from '../lib/router'
import { TaskActions } from './TasksPage'
import DocumentsTab from './task/DocumentsTab'
import MonitorTab from './task/MonitorTab'
import ElementsTab from './task/ElementsTab'
import ReportTab from './task/ReportTab'
import TaskLLMTab from './task/TaskLLMTab'
import { InterceptList, InterceptAuditTable } from '../components/Intercepts'

type Tab = 'documents' | 'monitor' | 'elements' | 'approvals' | 'report' | 'llm'

function TextPrompt({ open, title, label, placeholder, onClose, onSubmit }:
  { open: boolean; title: string; label: string; placeholder?: string; onClose: () => void; onSubmit: (v: string) => Promise<void> }) {
  const [v, setV] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => { if (open) setV('') }, [open])
  const submit = async () => {
    if (!v.trim()) return
    setBusy(true)
    try { await onSubmit(v.trim()); onClose() } finally { setBusy(false) }
  }
  return (
    <Modal open={open} title={title} onClose={onClose}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={submit} disabled={!v.trim()}>提交</Button></>}>
      <Field label={label}><Input autoFocus value={v} placeholder={placeholder} onChange={(e) => setV(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && submit()} /></Field>
    </Modal>
  )
}

export default function TaskWorkspace({ id, tab, onInterceptsChanged }: { id: number; tab: string; onInterceptsChanged: () => void }) {
  const toast = useToast()
  const task = useAsync(() => api.getTask(id), [id])
  const overview = useAsync(() => api.overview(id), [id])
  const intercepts = useAsync(() => api.listIntercepts(id), [id])
  const audit = useAsync(() => api.interceptAudit(id), [id], tab === 'approvals')
  const [prompt, setPrompt] = useState<'' | 'goal' | 'hint'>('')
  const current = (['documents', 'monitor', 'elements', 'approvals', 'report', 'llm'].includes(tab) ? tab : 'documents') as Tab

  const { reload: reloadOverview } = overview
  const { reload: reloadTask } = task
  const { reload: reloadIntercepts } = intercepts
  const { reload: reloadAudit } = audit

  // 活动流驱动的状态刷新：合并 1.2s 内的多次事件，避免每个工具调用都重拉。
  const refreshTimer = useRef<ReturnType<typeof setTimeout>>(undefined)
  const scheduleRefresh = useCallback(() => {
    clearTimeout(refreshTimer.current)
    refreshTimer.current = setTimeout(() => { void reloadOverview(); void reloadTask() }, 1200)
  }, [reloadOverview, reloadTask])
  useEffect(() => () => clearTimeout(refreshTimer.current), [])

  const refreshIntercepts = useCallback(() => {
    void reloadIntercepts()
    void reloadAudit()
    onInterceptsChanged()
  }, [reloadIntercepts, reloadAudit, onInterceptsChanged])

  const stream = useTaskEvents(id, {
    onActivity: (a: StreamEvent) => {
      if (a.kind === 'intercept_request') {
        toast('info', a.summary)
        refreshIntercepts()
      } else if (a.kind === 'intercept_decided') {
        refreshIntercepts()
      }
      if (a.kind !== 'tool') scheduleRefresh()
    },
    onReconnect: () => {
      toast('info', '活动流已重连，正在恢复推演态势与待审批列表')
      void overview.reload()
      void task.reload()
      refreshIntercepts()
    },
  })

  const reloadAll = () => { void task.reload(); void overview.reload(); refreshIntercepts() }
  const setTab = (k: Tab) => navigate(`/tasks/${id}/${k}`)

  if (task.loading && !task.data) return <Spinner />
  if (!task.data) {
    return (
      <div className="space-y-3 p-6">
        <ErrorBanner error={task.error || '任务不存在'} onRetry={task.reload} />
        <Button icon={<ArrowLeft className="size-4" />} onClick={() => navigate('/tasks')}>返回任务列表</Button>
      </div>
    )
  }
  const t = task.data
  const pending = intercepts.data?.length ?? 0
  const cov = overview.data?.coverage

  return (
    <div className="mx-auto max-w-7xl space-y-4 p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <button className="mb-1 flex items-center gap-1 text-xs text-slate-500 hover:text-slate-800" onClick={() => navigate('/tasks')}>
            <ArrowLeft className="size-3.5" /> 任务列表
          </button>
          <h1 className="flex flex-wrap items-center gap-2 text-xl font-semibold">
            <span className="text-slate-400">#{t.id}</span> {t.name}
            <Badge tone={statusTone(t.status)}>{taskStatusLabel[t.status] ?? t.status}</Badge>
            {t.running ? <Badge tone="blue">运行时在线 · 在途 {t.inflight}</Badge> : <Badge>运行时未挂载</Badge>}
            {t.settling && <Badge tone="purple">收尾中</Badge>}
          </h1>
          <p className="mt-1 text-sm text-slate-600">总目标：{t.goal}</p>
          <p className="text-xs text-slate-400">创建 {fmtTime(t.created_at)} · 首次运行 {fmtTime(t.first_run_at)} · 截止 {fmtTime(t.deadline_at)} · 覆盖率 {fmtPct(cov?.pct)}（{cov?.tested ?? 0}/{cov?.target ?? 0}）</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <TaskActions task={t} size="md" onDone={reloadAll} />
          {t.status === 'running' && !t.running && (
            <Button icon={<Play className="size-4" />} onClick={async () => {
              try { await api.taskAction(id, 'start'); reloadAll() } catch (e) { toast('error', errorText(e)) }
            }}>挂载运行时</Button>
          )}
          <Button icon={<Flag className="size-4" />} onClick={() => setPrompt('goal')}>追加目标</Button>
          <Button icon={<Lightbulb className="size-4" />} onClick={() => setPrompt('hint')}>提示 Planner</Button>
          <Button variant="ghost" icon={<RefreshCw className="size-4" />} onClick={reloadAll}>刷新</Button>
        </div>
      </div>

      <Tabs<Tab> value={current} onChange={setTab} tabs={[
        { key: 'documents', label: '招标文档' },
        { key: 'monitor', label: <>推演监控 <StreamDot status={stream.status} /></> },
        { key: 'elements', label: '要素与结论' },
        { key: 'approvals', label: <>工具审批 {pending > 0 && <Badge tone="red">{pending}</Badge>}</> },
        { key: 'report', label: '审查报告' },
        { key: 'llm', label: 'LLM 用量' },
      ]} />

      {current === 'documents' && <DocumentsTab taskID={id} onUploaded={reloadAll} />}
      {current === 'monitor' && <MonitorTab taskID={id} overview={overview} stream={stream} pending={intercepts.data ?? []}
        onDecided={refreshIntercepts} />}
      {current === 'elements' && <ElementsTab taskID={id} />}
      {current === 'approvals' && (
        <div className="space-y-4">
          <InterceptList rows={intercepts.data} loading={intercepts.loading} error={intercepts.error} onReload={intercepts.reload} onDecided={refreshIntercepts} />
          <InterceptAuditTable rows={audit.data ?? undefined} loading={audit.loading} error={audit.error} onReload={audit.reload} />
        </div>
      )}
      {current === 'report' && <ReportTab taskID={id} />}
      {current === 'llm' && <TaskLLMTab taskID={id} />}

      <TextPrompt open={prompt === 'goal'} title="追加审查目标" label="目标描述" placeholder="例如：核验全部 ★ 条款均有对应响应"
        onClose={() => setPrompt('')} onSubmit={async (v) => {
          try {
            const r = await api.addGoal(id, v)
            toast('ok', `已追加目标 #${r.goal_id}${r.revived ? '，任务已复活' : ''}`)
            reloadAll()
          } catch (e) { toast('error', errorText(e)); throw e }
        }} />
      <TextPrompt open={prompt === 'hint'} title="向 Planner 提示" label="提示内容" placeholder="例如：优先核查第三章评分办法中的业绩要求"
        onClose={() => setPrompt('')} onSubmit={async (v) => {
          try {
            const r = await api.addHint(id, v)
            toast('ok', `已记录提示 #${r.hint_id}`)
            void overview.reload()
          } catch (e) { toast('error', errorText(e)); throw e }
        }} />
    </div>
  )
}

function StreamDot({ status }: { status: string }) {
  const cls = status === 'open' ? 'bg-emerald-500' : status === 'closed' ? 'bg-slate-300' : 'bg-amber-500 animate-pulse'
  return <span className={`inline-block size-2 rounded-full ${cls}`} title={`活动流：${status}`} />
}
