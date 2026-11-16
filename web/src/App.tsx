import { useEffect } from 'react'
import type { ReactNode } from 'react'
import AppShell, { PageFrame } from './components/layout/AppShell'
import type { NavKey } from './components/layout/AppShell'
import { navigate, useLocation, linkProps } from './lib/router'
import { parseRoute } from './lib/routes'
import TasksPage from './pages/TasksPage'
import TaskWorkspace from './pages/TaskWorkspace'
import DashboardPage from './pages/DashboardPage'
import FindingsPage from './pages/FindingsPage'
import AssetsPage from './pages/AssetsPage'
import LLMRecordsPage from './pages/LLMRecordsPage'
import InterceptsPage from './pages/InterceptsPage'
import CredentialsPage from './pages/CredentialsPage'
import KnowledgePage from './pages/KnowledgePage'
import LLMPage from './pages/LLMPage'
import AgentsPage from './pages/AgentsPage'
import SkillsPage from './pages/SkillsPage'
import ToolsPage from './pages/ToolsPage'
import ProposalEntryPage from './pages/ProposalEntryPage'
import AdminUsersPage from './pages/AdminUsersPage'
import { SessionGate } from './features/auth/session'

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
    case 'dashboard': active = 'dashboard'; page = <DashboardPage />; break
    case 'tasks': active = 'tasks'; page = <TasksPage />; break
    case 'task': active = 'tasks'; taskId = route.taskId; page = <TaskWorkspace taskId={route.taskId} page={route.page} sectionId={route.sectionId} />; break
    case 'findings': active = 'findings'; page = <FindingsPage />; break
    case 'assets': active = 'assets'; page = <AssetsPage />; break
    case 'llm-records': active = 'llm-records'; page = <LLMRecordsPage />; break
    case 'intercepts': active = 'intercepts'; page = <InterceptsPage mode="approvals" />; break
    case 'intercept-rules': active = 'intercept-rules'; page = <InterceptsPage mode="rules" />; break
    case 'credentials': active = 'credentials'; page = <CredentialsPage />; break
    case 'knowledge': active = 'knowledge'; page = <KnowledgePage />; break
    case 'llm': active = 'llm'; page = <LLMPage />; break
    case 'agents': active = 'agents'; page = <AgentsPage />; break
    case 'skills': active = 'skills'; page = <SkillsPage />; break
    case 'tools': active = 'tools'; page = <ToolsPage />; break
    case 'admin-users': active = 'admin-users'; page = <AdminUsersPage />; break
    case 'proposal': active = 'tasks'; page = <ProposalEntryPage intent={route.intent} />; break
    case 'redirect': page = null; break
    case 'notfound': page = <NotFound reason={route.reason} />; break
  }
  return <SessionGate><AppShell active={active} taskId={taskId}>{page}</AppShell></SessionGate>
}
