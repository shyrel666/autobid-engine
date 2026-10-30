import { useEffect } from 'react'
import type { ReactNode } from 'react'
import AppShell, { PageFrame } from './components/layout/AppShell'
import type { NavKey } from './components/layout/AppShell'
import { navigate, useLocation, linkProps } from './lib/router'
import { parseRoute } from './lib/routes'
import TasksPage from './pages/TasksPage'
import TaskWorkspace from './pages/TaskWorkspace'
import InterceptsPage from './pages/InterceptsPage'
import CredentialsPage from './pages/CredentialsPage'
import KnowledgePage from './pages/KnowledgePage'
import LLMPage from './pages/LLMPage'
import ProposalEntryPage from './pages/ProposalEntryPage'

function NotFound({ reason }: { reason: string }) {
  return (
    <PageFrame title="页面不存在" description={reason}>
      <a {...linkProps('/tasks')} className="text-link hover:underline">返回投标项目</a>
    </PageFrame>
  )
}

export default function App() {
  const loc = useLocation()
  const route = parseRoute(loc.pathname, loc.search)

  // 旧地址归一化用 replace，不新增历史记录；不经过离开保护（地址只是同义改写）。
  useEffect(() => {
    if (route.name === 'redirect') void navigate(route.to, { replace: true, force: true })
  }, [route.name, route.name === 'redirect' ? route.to : '']) // eslint-disable-line react-hooks/exhaustive-deps

  let active: NavKey['key'] = ''
  let page: ReactNode = null
  let taskId: number | undefined
  switch (route.name) {
    case 'tasks': active = 'tasks'; page = <TasksPage />; break
    case 'task': active = 'tasks'; taskId = route.taskId; page = <TaskWorkspace taskId={route.taskId} page={route.page} sectionId={route.sectionId} />; break
    case 'intercepts': active = 'intercepts'; page = <InterceptsPage />; break
    case 'credentials': active = 'credentials'; page = <CredentialsPage />; break
    case 'knowledge': active = 'knowledge'; page = <KnowledgePage />; break
    case 'llm': active = 'llm'; page = <LLMPage />; break
    case 'proposal': active = 'tasks'; page = <ProposalEntryPage intent={route.intent} />; break
    case 'redirect': page = null; break
    case 'notfound': page = <NotFound reason={route.reason} />; break
  }
  return <AppShell active={active} taskId={taskId}>{page}</AppShell>
}
