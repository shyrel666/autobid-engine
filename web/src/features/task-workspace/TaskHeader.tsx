import { useEffect, useRef, useState } from 'react'
import { ArrowLeft, ChevronDown, PanelLeft } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { Task, TaskView } from '../../api/types'
import { Badge, Button, Field, IconButton, Input, Menu, Modal, cx, statusTone, useToast } from '../../components/ui'
import { useShell } from '../../components/layout/AppShell'
import { fmtBeijing, manifestStatusLabel, taskStatusLabel } from '../../lib/format'
import { linkProps } from '../../lib/router'
import { TASK_NAV, navGroupOf, taskUrl } from '../../lib/routes'
import type { TaskPage } from '../../lib/routes'
import { useTaskWorkspace } from './TaskWorkspaceProvider'

export function RunStatusBadge({ task }: { task: Pick<Task, 'status' | 'paused' | 'queued'> }) {
  const paused = task.paused && task.status !== 'paused'
  return (
    <span className="inline-flex items-center gap-1">
      <Badge dot tone={statusTone(task.status)}>{taskStatusLabel[task.status] ?? task.status}</Badge>
      {paused && <Badge dot tone="amber">已暂停</Badge>}
      {task.queued && <Badge>排队</Badge>}
    </span>
  )
}

function TextPrompt({ open, title, label, hint, placeholder, onClose, onSubmit }:
  { open: boolean; title: string; label: string; hint?: string; placeholder?: string; onClose: () => void; onSubmit: (v: string) => Promise<void> }) {
  const [v, setV] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => { if (open) setV('') }, [open])
  const submit = async () => {
    if (!v.trim() || busy) return
    setBusy(true)
    try { await onSubmit(v.trim()); onClose() } catch { /* 错误已提示，保留输入 */ } finally { setBusy(false) }
  }
  return (
    <Modal open={open} title={title} onClose={onClose} dirty={!!v.trim()}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={submit} disabled={!v.trim()}>提交</Button></>}>
      <Field label={label} hint={hint}><Input autoFocus value={v} placeholder={placeholder} onChange={(e) => setV(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && submit()} /></Field>
    </Modal>
  )
}

/** 执行操作：运行控制与项目级目标 / 提示集中在一个菜单，不在写作标题区堆满运维按钮。 */
function ExecutionMenu({ task }: { task: TaskView }) {
  const toast = useToast()
  const { taskId, invalidate } = useTaskWorkspace()
  const [prompt, setPrompt] = useState<'' | 'goal' | 'hint'>('')
  const [busy, setBusy] = useState(false)
  const run = async (action: 'start' | 'pause' | 'resume' | 'revive', label: string) => {
    if (busy) return
    setBusy(true)
    try { await api.taskAction(taskId, action); toast('ok', `项目 #${taskId} 已${label}`); invalidate('run_controlled') }
    catch (e) { toast('error', errorText(e)) } finally { setBusy(false) }
  }
  const terminal = ['done', 'failed', 'timeout'].includes(task.status)
  return (
    <>
      <Menu label="执行操作" trigger={<span className={cx('inline-flex h-7 items-center gap-1.5 rounded-lg border border-line bg-panel px-2 text-[13px] font-medium hover:bg-soft', busy && 'opacity-50')}>
        <ChevronDown className="size-4" aria-hidden /><span className="hidden sm:inline">执行操作</span></span>}
        items={[
          task.status === 'created' && { key: 'start', label: '启动推演', onSelect: () => void run('start', '启动') },
          task.status === 'running' && !task.paused && { key: 'pause', label: '暂停运行', onSelect: () => void run('pause', '暂停') },
          (task.paused || task.status === 'paused') && { key: 'resume', label: '恢复运行', onSelect: () => void run('resume', '恢复') },
          terminal && { key: 'revive', label: '复活运行', onSelect: () => void run('revive', '复活') },
          task.status === 'running' && !task.running && { key: 'mount', label: '挂载运行时', onSelect: () => void run('start', '挂载运行时') },
          { key: 'goal', label: '追加目标…', onSelect: () => setPrompt('goal') },
          { key: 'hint', label: '补充提示（Planner）…', onSelect: () => setPrompt('hint') },
          { key: 'refresh', label: '刷新项目状态', onSelect: () => invalidate(['task', 'overview', 'intercepts', 'business']) },
        ]} />
      <TextPrompt open={prompt === 'goal'} title="追加项目目标" label="目标描述" placeholder="例如：核验全部 ★ 条款均有对应响应"
        hint="这是项目级推演目标，不是聊天回复；终态任务追加目标后会复活运行。"
        onClose={() => setPrompt('')} onSubmit={async (v) => {
          try {
            const r = await api.addGoal(taskId, v)
            toast('ok', `已追加目标 #${r.goal_id}${r.revived ? '，项目已复活运行' : ''}`)
            invalidate('run_controlled')
          } catch (e) { toast('error', errorText(e)); throw e }
        }} />
      <TextPrompt open={prompt === 'hint'} title="补充提示（Planner）" label="提示内容" placeholder="例如：优先核查第三章评分办法中的业绩要求"
        hint="提示写入推演图供 Planner 下一轮参考，不会自动批准要求或章节。"
        onClose={() => setPrompt('')} onSubmit={async (v) => {
          try {
            const r = await api.addHint(taskId, v)
            toast('ok', `已记录提示 #${r.hint_id}`)
            invalidate(['overview'])
          } catch (e) { toast('error', errorText(e)); throw e }
        }} />
    </>
  )
}

function useScrollActiveIntoView(dep: unknown) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const box = ref.current
    const cur = box?.querySelector<HTMLElement>('[aria-current="page"]')
    if (!box || !cur) return
    const b = box.getBoundingClientRect(), c = cur.getBoundingClientRect()
    if (c.right > b.right) box.scrollLeft += c.right - b.right + 4
    else if (c.left < b.left) box.scrollLeft -= b.left - c.left + 4
  }, [dep])
  return ref
}

/** 项目标题：32px 首行 / 16px 元信息行 / 32px 一级导航 / 32px 二级导航，行间距 8px（第 3.1 节）。 */
export default function TaskHeader({ page }: { page: TaskPage }) {
  const shell = useShell()
  const { taskId, task, outlines, lastInGroup } = useTaskWorkspace()
  const group = navGroupOf(page)
  const primaryRef = useScrollActiveIntoView(group.key)
  const secondaryRef = useScrollActiveIntoView(page)
  const t = task.data
  const m = outlines.data?.manifest
  const deadline = fmtBeijing(m?.bid_deadline)
  const ol = outlines.data
  const business = !ol ? (outlines.error ? '业务状态读取失败' : '业务状态读取中')
    : [m ? ({ draft: '清单未确认', confirmed: '清单已确认', stale: '清单需重新确认' } as Record<string, string>)[m.status] ?? manifestStatusLabel[m.status] : '',
      ol.approved_version ? `批准大纲 v${ol.approved_version}` : ol.outlines.length ? '大纲未批准' : '尚无大纲',
      ol.open_version && ol.open_version !== ol.approved_version ? `v${ol.open_version} 修订中` : ''].filter(Boolean).join(' · ')

  return (
    <header data-region="task-header" className="flex shrink-0 flex-col gap-2 border-b border-line px-4 pt-2.5 md:px-6">
      <div className="flex min-h-8 flex-wrap items-center gap-2">
        <IconButton label={shell.narrow ? '打开导航' : shell.collapsed ? '展开侧栏' : '折叠侧栏'} onClick={shell.toggleSidebar}><PanelLeft className="size-4" strokeWidth={1.7} /></IconButton>
        <a {...linkProps('/tasks')} aria-label="返回投标项目列表" title="返回投标项目列表"
          className="grid size-8 shrink-0 place-items-center rounded-[10px] hover:bg-soft"><ArrowLeft className="size-4" strokeWidth={1.7} /></a>
        <h1 className="min-w-0 max-w-[60vw] truncate text-sm font-semibold" title={t?.name}>{t?.name ?? (task.error ? '项目读取失败' : '加载中…')}</h1>
        <span className="hidden rounded bg-soft px-1.5 py-px text-xs text-muted sm:inline">项目 #{taskId}</span>
        {t && <RunStatusBadge task={t} />}
        {t?.settling && <Badge tone="purple">收尾中</Badge>}
        <span className="flex-1" />
        {t && <ExecutionMenu task={t} />}
      </div>
      <p className="text-xs leading-4 text-muted">
        投标截止：{deadline || <span className="text-warn">尚未确认</span>}
        {deadline && m?.bid_deadline_source?.origin === 'manual' ? '（人工指定）' : ''}
        {' · '}{business}
      </p>
      <div ref={primaryRef} className="scrollbar-none overflow-x-auto">
        <nav aria-label="项目导航" data-region="project-tabs" className="inline-flex h-8 gap-px rounded-[10px] bg-soft p-[3px] whitespace-nowrap">
          {TASK_NAV.map((g) => {
            const on = g.key === group.key
            const to = on ? taskUrl(taskId, g.pages[0].page) : lastInGroup.get(g.key) ?? taskUrl(taskId, g.pages[0].page)
            return (
              <a key={g.key} {...linkProps(on && g.pages.length === 1 ? taskUrl(taskId, page) : to)} aria-current={on ? 'page' : undefined}
                className={cx('rounded-lg px-2 text-sm leading-6 font-medium', on ? 'bg-panel text-fg shadow-[0_1px_2px_#00000015]' : 'text-muted hover:text-fg')}>
                {g.label}
              </a>
            )
          })}
        </nav>
      </div>
      {group.pages.length > 1 ? (
        <div ref={secondaryRef} className="scrollbar-none overflow-x-auto">
          <nav aria-label={`${group.label}页面`} className="flex h-8 gap-4 whitespace-nowrap">
            {group.pages.map((p) => (
              <a key={p.page} {...linkProps(taskUrl(taskId, p.page))} aria-current={p.page === page ? 'page' : undefined}
                className={cx('flex items-center border-b-2 text-xs font-medium', p.page === page ? 'border-fg text-fg' : 'border-transparent text-muted hover:text-fg')}>
                {p.label}
              </a>
            ))}
          </nav>
        </div>
      ) : <div className="h-0" />}
    </header>
  )
}

