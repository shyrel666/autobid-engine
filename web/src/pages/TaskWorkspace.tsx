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
import DocumentsPage from '../features/proposal/documents/DocumentsPage'
import RequirementsPage from '../features/proposal/requirements/RequirementsPage'
import OutlinePage from '../features/proposal/outline/OutlinePage'
import WritingPage from '../features/proposal/writing/WritingPage'
import { ConsistencyPage, MatrixPage } from '../features/proposal/review/ReviewPages'
import ExportPage from '../features/proposal/export/ExportPage'
import ExecutionPage from '../features/execution/ExecutionPage'
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

function Body({ page, sectionId }: { page: TaskPage; sectionId?: number }) {
  const { task, taskId, lastInGroup } = useTaskWorkspace()
  const loc = useLocation()
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
  const fill = page === 'writing' || page === 'monitor'
  let content: ReactNode
  switch (page) {
    case 'overview': content = <OverviewPage />; break
    case 'documents': content = <DocumentsPage />; break
    case 'requirements': content = <RequirementsPage />; break
    case 'research': content = <div className="mx-auto max-w-6xl"><ResearchTab taskID={taskId} /></div>; break
    case 'outline': content = <OutlinePage />; break
    case 'writing': content = <WritingPage sectionId={sectionId} />; break
    case 'matrix': content = <MatrixPage />; break
    case 'consistency': content = <ConsistencyPage />; break
    case 'report': content = <div className="mx-auto max-w-6xl"><ReportTab taskID={taskId} /></div>; break
    case 'export': content = <ExportPage />; break
    case 'monitor': content = <ExecutionPage />; break
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
