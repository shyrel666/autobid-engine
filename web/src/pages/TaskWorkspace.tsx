import { useEffect } from 'react'
import type { ReactNode } from 'react'
import { ArrowLeft } from 'lucide-react'
import { Button, ErrorBanner, Spinner, cx } from '../components/ui'
import { InterceptAuditTable, InterceptList } from '../components/Intercepts'
import { useAsync } from '../hooks/useAsync'
import { api } from '../api/client'
import { linkProps, useLocation } from '../lib/router'
import { navGroupOf } from '../lib/routes'
import type { TaskPage } from '../lib/routes'
import { TaskWorkspaceProvider, useTaskWorkspace, useTick } from '../features/task-workspace/TaskWorkspaceProvider'
import TaskHeader from '../features/task-workspace/TaskHeader'
import OverviewPage from '../features/task-workspace/OverviewPage'
import DocumentsTab from './task/DocumentsTab'
import ProposalTab from './task/ProposalTab'
import MonitorTab from './task/MonitorTab'
import ElementsTab from './task/ElementsTab'
import ReportTab from './task/ReportTab'
import TaskLLMTab from './task/TaskLLMTab'
import ResearchTab from './task/ResearchTab'

function ApprovalsPage() {
  const { taskId, intercepts, invalidate } = useTaskWorkspace()
  const t = useTick('intercepts')
  const audit = useAsync(() => api.interceptAudit(taskId), [taskId, t])
  return (
    <div className="mx-auto max-w-6xl space-y-4">
      <p className="text-sm text-muted">这里是 Worker 在途<b className="font-medium text-fg">工具调用</b>的短时审批；大纲与章节的业务审批在「大纲与写作」中进行，二者互不替代。</p>
      <InterceptList rows={intercepts.data} loading={intercepts.loading} error={intercepts.error} onReload={intercepts.reload}
        onDecided={() => invalidate('intercept_decided')} />
      <InterceptAuditTable rows={audit.data ?? undefined} loading={audit.loading} error={audit.error} onReload={audit.reload} />
    </div>
  )
}

function Body({ page }: { page: TaskPage; sectionId?: number }) {
  const { task, taskId, lastInGroup, invalidate, overview, stream, intercepts } = useTaskWorkspace()
  const loc = useLocation()
  const proposalTick = useTick('requirements', 'outline', 'sections', 'matrix', 'consistency', 'export', 'business')
  useEffect(() => { lastInGroup.set(navGroupOf(page).key, loc.pathname + loc.search) }, [page, loc, lastInGroup])

  if (!task.data && task.loading) return <Spinner />
  if (!task.data) {
    const missing = task.error.startsWith('HTTP 404')
    return (
      <div className="space-y-3 p-6">
        <h1 className="text-2xl leading-8 font-semibold">{missing ? `项目 #${taskId} 不存在或不可访问` : '项目读取失败'}</h1>
        <ErrorBanner error={task.error} onRetry={missing ? undefined : task.reload} />
        <a {...linkProps('/tasks')}><Button icon={<ArrowLeft className="size-4" />}>返回投标项目</Button></a>
      </div>
    )
  }
  // 写作与执行记录使用固定标题、面板各自滚动；其余页面主区域滚动。
  const fill = false
  let content: ReactNode
  switch (page) {
    case 'overview': content = <OverviewPage />; break
    case 'documents': content = <div className="mx-auto max-w-6xl"><DocumentsTab taskID={taskId} onUploaded={() => invalidate('document_imported')} /></div>; break
    case 'requirements': content = <div className="mx-auto max-w-7xl"><ProposalTab taskID={taskId} tick={proposalTick} /></div>; break
    case 'research': content = <div className="mx-auto max-w-6xl"><ResearchTab taskID={taskId} /></div>; break
    case 'outline': content = <div className="mx-auto max-w-7xl"><ProposalTab taskID={taskId} tick={proposalTick} /></div>; break
    case 'writing': content = <div className="mx-auto max-w-7xl"><ProposalTab taskID={taskId} tick={proposalTick} /></div>; break
    case 'matrix': content = <div className="mx-auto max-w-7xl"><ProposalTab taskID={taskId} tick={proposalTick} /></div>; break
    case 'consistency': content = <div className="mx-auto max-w-7xl"><ProposalTab taskID={taskId} tick={proposalTick} /></div>; break
    case 'report': content = <div className="mx-auto max-w-6xl"><ReportTab taskID={taskId} /></div>; break
    case 'export': content = <div className="mx-auto max-w-7xl"><ProposalTab taskID={taskId} tick={proposalTick} /></div>; break
    case 'monitor': content = <MonitorTab taskID={taskId} overview={overview} stream={stream} pending={intercepts.data ?? []} onDecided={() => invalidate('intercept_decided')} />; break
    case 'elements': content = <div className="mx-auto max-w-6xl"><ElementsTab taskID={taskId} /></div>; break
    case 'approvals': content = <ApprovalsPage />; break
    case 'llm': content = <div className="mx-auto max-w-6xl"><TaskLLMTab taskID={taskId} /></div>; break
  }
  return (
    <>
      <TaskHeader page={page} />
      <div className={cx('min-h-0 min-w-0 flex-1 p-4 md:p-6', fill ? 'flex flex-col overflow-hidden' : 'overflow-auto')}>{content}</div>
    </>
  )
}

export default function TaskWorkspace({ taskId, page, sectionId }: { taskId: number; page: TaskPage; sectionId?: number }) {
  return (
    <TaskWorkspaceProvider key={taskId} taskId={taskId}>
      <Body page={page} sectionId={sectionId} />
    </TaskWorkspaceProvider>
  )
}
