import { useCallback, useEffect, useState } from 'react'
import {
  BookOpen, Building2, ClipboardList, Cpu, FileOutput, KeyRound, LayoutList, PenTool, ShieldCheck, Wifi, WifiOff,
} from 'lucide-react'
import type { ReactNode } from 'react'
import { api, getToken, setToken } from './api/client'
import type { LLMHealth } from './api/types'
import { Badge, Button, Field, Input, Modal, cx } from './components/ui'
import { useInterval } from './hooks/useAsync'
import { matchRoute, navigate, usePath } from './lib/router'
import TasksPage from './pages/TasksPage'
import TaskWorkspace from './pages/TaskWorkspace'
import InterceptsPage from './pages/InterceptsPage'
import CredentialsPage from './pages/CredentialsPage'
import KnowledgePage from './pages/KnowledgePage'
import LLMPage from './pages/LLMPage'
import RoadmapPage from './pages/RoadmapPage'
import ProposalEntryPage from './pages/ProposalEntryPage'

function NavItem({ to, icon, label, active, badge, muted }:
  { to: string; icon: ReactNode; label: string; active: boolean; badge?: ReactNode; muted?: boolean }) {
  return (
    <a href={to} onClick={(e) => { e.preventDefault(); navigate(to) }}
      className={cx('flex items-center gap-2.5 rounded-md px-3 py-2 text-sm transition-colors',
        active ? 'bg-blue-50 font-medium text-blue-700' : muted ? 'text-slate-400 hover:bg-slate-100 hover:text-slate-600' : 'text-slate-600 hover:bg-slate-100 hover:text-slate-900')}>
      <span className="shrink-0">{icon}</span>
      <span className="flex-1 truncate">{label}</span>
      {badge}
    </a>
  )
}

function TokenDialog({ open, onClose, reason }: { open: boolean; onClose: () => void; reason?: string }) {
  const [value, setValue] = useState(getToken())
  useEffect(() => { if (open) setValue(getToken()) }, [open])
  const save = () => { setToken(value.trim()); onClose() }
  return (
    <Modal open={open} title="API 访问令牌" onClose={onClose}
      footer={<>
        <Button variant="ghost" onClick={() => { setToken(''); onClose() }}>清除</Button>
        <Button variant="primary" onClick={save}>保存</Button>
      </>}>
      {reason && <p className="mb-3 rounded-md bg-amber-50 px-3 py-2 text-sm text-amber-800">{reason}</p>}
      <Field label="Bearer Token" hint="对应服务端 AUTOBID_API_TOKEN；保存在本浏览器 localStorage，请求时以 Authorization: Bearer 发送，SSE 以 ?token= 发送。服务端未配置令牌时可留空。">
        <Input type="password" value={value} autoFocus onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && save()} placeholder="未设置" />
      </Field>
    </Modal>
  )
}

function StatusBar({ onToken }: { onToken: () => void }) {
  const [healthy, setHealthy] = useState<boolean | null>(null)
  const [llm, setLLM] = useState<LLMHealth[] | null>(null)
  const [llmErr, setLLMErr] = useState('')
  const [hasToken, setHasToken] = useState(!!getToken())

  const poll = useCallback(async () => {
    setHealthy(await api.healthz())
    try {
      setLLM(await api.llmHealth())
      setLLMErr('')
    } catch (e) {
      setLLM(null)
      setLLMErr(e instanceof Error ? e.message : String(e))
    }
  }, [])
  useEffect(() => { void poll() }, [poll])
  useInterval(poll, 15000)
  useEffect(() => {
    const on = () => { setHasToken(!!getToken()); void poll() }
    window.addEventListener('autobid:token-changed', on)
    return () => window.removeEventListener('autobid:token-changed', on)
  }, [poll])

  const open = llm?.filter((h) => h.open).length ?? 0
  return (
    <div className="flex items-center gap-3 text-xs">
      <span className="flex items-center gap-1.5" title="GET /healthz（服务与数据库连通性）">
        {healthy === null ? <Wifi className="size-3.5 text-slate-400" /> : healthy ? <Wifi className="size-3.5 text-emerald-600" /> : <WifiOff className="size-3.5 text-red-600" />}
        <span className={healthy === false ? 'text-red-600' : 'text-slate-600'}>{healthy === null ? '检测中' : healthy ? '服务正常' : '服务不可用'}</span>
      </span>
      <span className="flex items-center gap-1.5" title={llmErr || llm?.map((h) => `#${h.profile_id} fails=${h.fails} ${h.open ? '熔断中' : ''} ${h.last_error}`).join('\n')}>
        <Cpu className="size-3.5 text-slate-500" />
        {llmErr ? <span className="text-red-600">LLM 状态读取失败</span>
          : llm === null ? <span className="text-slate-400">LLM —</span>
          : open > 0 ? <span className="text-red-600">{open} 个模型熔断中</span>
          : <span className="text-slate-600">LLM {llm.length ? `${llm.length} 个配置健康` : '暂无调用记录'}</span>}
      </span>
      <Button size="sm" variant={hasToken ? 'ghost' : 'secondary'} icon={<KeyRound className="size-3.5" />} onClick={onToken}>
        {hasToken ? '令牌已设置' : '设置令牌'}
      </Button>
    </div>
  )
}

export default function App() {
  const path = usePath()
  const route = matchRoute(path)
  const [tokenOpen, setTokenOpen] = useState(false)
  const [tokenReason, setTokenReason] = useState('')
  const [pendingCount, setPendingCount] = useState(0)
  // 令牌变更后重新挂载当前页面，使其以新令牌重拉数据。
  const [tokenEpoch, setTokenEpoch] = useState(0)
  useEffect(() => {
    const on = () => setTokenEpoch((n) => n + 1)
    window.addEventListener('autobid:token-changed', on)
    return () => window.removeEventListener('autobid:token-changed', on)
  }, [])

  useEffect(() => {
    const on = () => { setTokenReason('服务端返回 401 未授权，请设置正确的 API Token。'); setTokenOpen(true) }
    window.addEventListener('autobid:unauthorized', on)
    return () => window.removeEventListener('autobid:unauthorized', on)
  }, [])

  const pollPending = useCallback(async () => {
    try {
      setPendingCount((await api.listIntercepts()).length)
    } catch {
      /* 状态栏另有错误提示 */
    }
  }, [])
  useEffect(() => { void pollPending() }, [pollPending, path, tokenEpoch])
  useInterval(pollPending, 10000)

  let page: ReactNode
  switch (route.name) {
    case 'tasks': page = <TasksPage />; break
    case 'task': page = <TaskWorkspace key={route.id} id={route.id} tab={route.tab} onInterceptsChanged={pollPending} />; break
    case 'intercepts': page = <InterceptsPage onChanged={pollPending} />; break
    case 'credentials': page = <CredentialsPage />; break
    case 'knowledge': page = <KnowledgePage />; break
    case 'llm': page = <LLMPage />; break
    case 'proposal': page = <ProposalEntryPage />; break
    case 'roadmap': page = <RoadmapPage phase={route.phase} />; break
    default: page = <div className="p-8 text-slate-500">页面不存在。<a className="text-blue-600" href="/" onClick={(e) => { e.preventDefault(); navigate('/') }}>返回任务列表</a></div>
  }

  const isRoadmap = (p: string) => route.name === 'roadmap' && route.phase === p
  return (
    <div className="flex h-screen bg-slate-50 text-slate-800">
      <aside className="flex w-60 shrink-0 flex-col border-r border-slate-200 bg-white">
        <div className="flex items-center gap-2 px-4 py-4">
          <div className="flex size-8 items-center justify-center rounded-lg bg-blue-600 text-white"><ShieldCheck className="size-5" /></div>
          <div>
            <div className="text-sm font-semibold">AutoBid Engine</div>
            <div className="text-xs text-slate-400">标书合规审查工作台</div>
          </div>
        </div>
        <nav className="flex-1 space-y-4 overflow-y-auto px-3 pb-4">
          <div className="space-y-0.5">
            <div className="px-3 pb-1 text-xs font-medium text-slate-400">审查工作区</div>
            <NavItem to="/tasks" icon={<LayoutList className="size-4" />} label="任务与招标文档" active={route.name === 'tasks' || (route.name === 'task' && route.tab !== 'proposal')} />
            <NavItem to="/proposal" icon={<PenTool className="size-4" />} label="技术标大纲与写作"
              active={route.name === 'proposal' || (route.name === 'task' && route.tab === 'proposal')} />
            <NavItem to="/intercepts" icon={<ClipboardList className="size-4" />} label="工具调用审批" active={route.name === 'intercepts'}
              badge={pendingCount > 0 ? <Badge tone="red">{pendingCount}</Badge> : undefined} />
          </div>
          <div className="space-y-0.5">
            <div className="px-3 pb-1 text-xs font-medium text-slate-400">资料与配置</div>
            <NavItem to="/credentials" icon={<Building2 className="size-4" />} label="企业资信库" active={route.name === 'credentials'} />
            <NavItem to="/knowledge" icon={<BookOpen className="size-4" />} label="企业方案知识库" active={route.name === 'knowledge'} />
            <NavItem to="/llm" icon={<Cpu className="size-4" />} label="LLM 配置" active={route.name === 'llm'} />
          </div>
          <div className="space-y-0.5">
            <div className="px-3 pb-1 text-xs font-medium text-slate-400">规划中模块</div>
            <NavItem to="/roadmap/phase6" icon={<FileOutput className="size-4" />} label="成稿套版导出" muted active={isRoadmap('phase6')} badge={<Badge>P6B</Badge>} />
          </div>
        </nav>
        <div className="border-t border-slate-100 px-4 py-3 text-xs text-slate-400">Phase 5 · 单团队版</div>
      </aside>
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-12 shrink-0 items-center justify-end border-b border-slate-200 bg-white px-4">
          <StatusBar onToken={() => { setTokenReason(''); setTokenOpen(true) }} />
        </header>
        <main key={tokenEpoch} className="min-h-0 flex-1 overflow-y-auto">{page}</main>
      </div>
      <TokenDialog open={tokenOpen} reason={tokenReason} onClose={() => setTokenOpen(false)} />
    </div>
  )
}
