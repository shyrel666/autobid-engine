import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { api } from '../../api/client'
import type { InterceptPending, OutlineList, Overview, TaskView } from '../../api/types'
import { useToast } from '../../components/ui'
import { useShell } from '../../components/layout/AppShell'
import { useAsync } from '../../hooks/useAsync'
import type { AsyncState } from '../../hooks/useAsync'
import { useTaskEvents } from '../../hooks/useTaskEvents'
import type { StreamEvent, TaskStream } from '../../hooks/useTaskEvents'
import { rememberProjects } from '../../lib/projectCache'
import { INVALIDATION, resourcesForActivity } from './resources'
import type { Resource, ResourceAction } from './resources'

// 项目上下文：同一任务内切换页面时保持挂载，统一管理任务信息、活动流（一次只订阅当前任务）与资源失效。
// 不同任务以 key={taskId} 重新挂载，旧连接与定时器随卸载清理，上一任务的数据不会渲染到下一任务。

interface TaskCtx {
  taskId: number
  task: AsyncState<TaskView>
  overview: AsyncState<Overview>
  intercepts: AsyncState<InterceptPending[]>
  /** 大纲版本列表（含要求清单 manifest：投标截止、分包、清单状态），供项目标题与概览使用。 */
  outlines: AsyncState<OutlineList>
  stream: TaskStream
  ticks: Record<Resource, number>
  invalidate: (what: ResourceAction | Resource[]) => void
  /** 各导航分组最近访问的地址（内存），切回分组时恢复工作位置。 */
  lastInGroup: Map<string, string>
}

const Ctx = createContext<TaskCtx | null>(null)

export function useTaskWorkspace(): TaskCtx {
  const c = useContext(Ctx)
  if (!c) throw new Error('useTaskWorkspace must be used inside TaskWorkspaceProvider')
  return c
}

/** useTick 返回资源的失效计数，放进 useAsync 依赖即可在相关动作后重读。 */
export function useTick(...rs: Resource[]): number {
  const { ticks } = useTaskWorkspace()
  return rs.reduce((n, r) => n + ticks[r], 0)
}

const ALL: Resource[] = ['task', 'overview', 'documents', 'requirements', 'outline', 'sections', 'charts', 'matrix', 'consistency', 'export',
  'exportJobs', 'templates', 'evidence', 'research', 'intercepts', 'business']
const zero = () => Object.fromEntries(ALL.map((r) => [r, 0])) as Record<Resource, number>

export function TaskWorkspaceProvider({ taskId, children }: { taskId: number; children: ReactNode }) {
  const toast = useToast()
  const shell = useShell()
  const [ticks, setTicks] = useState(zero)
  const lastInGroup = useRef(new Map<string, string>()).current

  const task = useAsync(() => api.getTask(taskId), [taskId, ticks.task])
  const overview = useAsync(() => api.overview(taskId), [taskId, ticks.overview])
  const intercepts = useAsync(() => api.listIntercepts(taskId), [taskId, ticks.intercepts])
  const outlines = useAsync(() => api.outlines(taskId), [taskId, ticks.outline + ticks.requirements + ticks.business])

  useEffect(() => { if (task.data) rememberProjects([task.data]) }, [task.data])

  const bump = useCallback((rs: Iterable<Resource>) => {
    setTicks((t) => {
      const n = { ...t }
      for (const r of rs) n[r]++
      return n
    })
  }, [])

  const invalidate = useCallback((what: ResourceAction | Resource[]) => {
    const rs = Array.isArray(what) ? what : INVALIDATION[what]
    bump(rs)
    if (rs.includes('intercepts')) shell.refreshPending()
  }, [bump, shell])

  // 活动流驱动的刷新：合并 1.2s 内的多次事件，避免每个工具调用都触发全部接口重读。
  const pending = useRef(new Set<Resource>())
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  const schedule = useCallback((rs: Resource[]) => {
    for (const r of rs) pending.current.add(r)
    clearTimeout(timer.current)
    timer.current = setTimeout(() => {
      const list = [...pending.current]
      pending.current.clear()
      if (list.length) invalidate(list)
    }, 1200)
  }, [invalidate])
  useEffect(() => () => clearTimeout(timer.current), [])

  const stream = useTaskEvents(taskId, {
    onActivity: (a: StreamEvent) => {
      if (a.kind === 'intercept_request') toast('info', a.summary)
      if (a.worker === 'section-writer' && a.kind !== 'state' || (a.worker === 'section-writer' || a.worker === 'proposal') && (a.summary.includes('已写回') || a.summary.includes('候选稿')))
        toast(a.kind === 'error' ? 'error' : 'info', a.summary)
      schedule(resourcesForActivity(a))
    },
    // 重连后断开期间的活动可能缺失：重读业务快照与工具审批，不宣称历史已恢复。
    onReconnect: () => {
      toast('info', '活动流已重连：正在重读项目状态与待审批列表；断开期间的活动可能未显示')
      invalidate(ALL)
    },
  })

  // 浏览器重新聚焦时重读当前可见的关键业务状态（至多每 15 秒一次）。
  const lastFocus = useRef(Date.now())
  useEffect(() => {
    const on = () => {
      if (document.visibilityState !== 'visible' || Date.now() - lastFocus.current < 15000) return
      lastFocus.current = Date.now()
      invalidate(['task', 'overview', 'intercepts', 'business', 'requirements', 'outline', 'sections', 'matrix', 'consistency', 'export', 'exportJobs'])
    }
    window.addEventListener('focus', on)
    document.addEventListener('visibilitychange', on)
    return () => { window.removeEventListener('focus', on); document.removeEventListener('visibilitychange', on) }
  }, [invalidate])

  const value = useMemo<TaskCtx>(() => ({ taskId, task, overview, intercepts, outlines, stream, ticks, invalidate, lastInGroup }),
    [taskId, task, overview, intercepts, outlines, stream, ticks, invalidate, lastInGroup])
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}
