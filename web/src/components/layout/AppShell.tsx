import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import {
  BookOpen, Bot, Brain, Bug, ClipboardList, LayoutDashboard,
  Library, Network, PanelLeft, Radio, Search, Settings2, ShieldAlert, Sparkles, Target, Wrench,
} from 'lucide-react'
import { api, getToken, setToken } from '../../api/client'
import type { LLMHealth } from '../../api/types'
import { Button, Field, IconButton, Input, Modal, cx, useFocusTrap } from '../ui'
import { useInterval } from '../../hooks/useAsync'
import { linkProps, navigate } from '../../lib/router'
import { TASK_NAV, taskUrl } from '../../lib/routes'
import { forgetProjects, loadedProjects } from '../../lib/projectCache'
import { ThemeToggle } from '../../lib/theme'

// 应用框架：240 / 64px 侧栏、#fafafa 画布上 8px 外边距的白色主容器、48px 全局顶栏（项目页由项目标题替代）。
// 尺寸与断点按 FRONTEND_REFACTOR_PLAN.md 第 3.1、3.3 节：≥1200 默认展开，768–1199 默认折叠，<768 改为 288px 抽屉。

const PREF_KEY = 'autobid.sidebar'

type Viewport = 'wide' | 'mid' | 'narrow'
function viewportOf(w: number): Viewport {
  return w >= 1200 ? 'wide' : w >= 768 ? 'mid' : 'narrow'
}

function readPref(): 'expanded' | 'collapsed' | null {
  try {
    const v = localStorage.getItem(PREF_KEY)
    return v === 'expanded' || v === 'collapsed' ? v : null
  } catch {
    return null
  }
}

interface ShellCtx {
  toggleSidebar: () => void
  collapsed: boolean
  narrow: boolean
  openToken: () => void
  pendingCount: number
  refreshPending: () => void
}
const Ctx = createContext<ShellCtx>({ toggleSidebar: () => {}, collapsed: false, narrow: false, openToken: () => {}, pendingCount: 0, refreshPending: () => {} })
export const useShell = () => useContext(Ctx)

export interface NavKey {
  key:
    | 'dashboard' | 'tasks' | 'findings' | 'assets' | 'llm-records' | 'credentials' | 'knowledge'
    | 'llm' | 'agents' | 'skills' | 'tools' | 'intercept-rules' | 'intercepts' | ''
}

const globalNav = [
  { group: '功能', items: [
    { key: 'dashboard', to: '/dashboard', label: '仪表盘', icon: <LayoutDashboard className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'tasks', to: '/tasks', label: '投标项目', icon: <Target className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'findings', to: '/findings', label: '审查发现', icon: <Bug className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'assets', to: '/assets', label: '招标要素', icon: <Network className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'llm-records', to: '/llm-records', label: 'LLM 录制', icon: <Radio className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'credentials', to: '/credentials', label: '企业资信库', icon: <BookOpen className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'knowledge', to: '/knowledge', label: '企业方案知识库', icon: <Library className="size-4 shrink-0" strokeWidth={1.7} /> },
  ] },
  { group: '系统', items: [
    { key: 'llm', to: '/llm', label: 'LLM', icon: <Brain className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'agents', to: '/agents', label: 'Agent', icon: <Bot className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'skills', to: '/skills', label: 'Skill', icon: <Sparkles className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'tools', to: '/tools', label: '工具', icon: <Wrench className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'intercept-rules', to: '/intercepts/rules', label: '拦截规则', icon: <ShieldAlert className="size-4 shrink-0" strokeWidth={1.7} /> },
    { key: 'intercepts', to: '/intercepts', label: '审批记录', icon: <ClipboardList className="size-4 shrink-0" strokeWidth={1.7} /> },
  ] },
] as const

function Sidebar({ active, collapsed, pendingCount, healthy, drawer, onNavigate }:
  { active: NavKey['key']; collapsed: boolean; pendingCount: number; healthy: boolean | null; drawer?: boolean; onNavigate?: () => void }) {
  return (
    <aside data-region="sidebar" aria-label="全局导航"
      className={cx('flex h-full shrink-0 flex-col overflow-y-auto p-3.5', collapsed ? 'w-16 gap-3' : 'gap-3.5', drawer ? 'w-full' : !collapsed && 'w-[240px]')}>
      <div className={cx('flex h-9 items-center gap-3 text-base font-semibold tracking-tight', collapsed ? 'justify-center px-0' : 'px-2')}>
        <img src="/autobid-logo.svg" width={28} height={28} className="size-7 shrink-0" alt={collapsed ? 'AutoBid' : ''} />
        {!collapsed && <span>AutoBid</span>}
      </div>
      {globalNav.map((g) => (
        <nav key={g.group} aria-label={g.group}>
          {!collapsed && <div className="flex h-6 items-center px-2 text-xs font-medium text-muted">{g.group}</div>}
          {g.items.map((it) => {
            const on = active === it.key
            const count = it.key === 'intercepts' && pendingCount > 0 ? pendingCount : 0
            return (
              <a key={it.key} {...linkProps(it.to)} onClickCapture={onNavigate} aria-current={on ? 'page' : undefined}
                aria-label={collapsed ? `${it.label}${count ? `（${count} 项待审批）` : ''}` : undefined} title={collapsed ? it.label : undefined}
                data-region="nav-item"
                className={cx('relative mb-0.5 flex h-8 items-center gap-2.5 rounded-lg text-sm transition-colors hover:bg-soft', collapsed ? 'justify-center px-2' : 'px-2.5',
                  on ? 'bg-soft font-semibold text-fg' : 'text-fg')}>
                {it.icon}
                {!collapsed && <span className="min-w-0 flex-1 truncate">{it.label}</span>}
                {count > 0 && (collapsed
                  ? <span className="absolute top-1 right-1 size-2 rounded-full bg-warn" aria-hidden />
                  : <span className="ml-auto flex h-4.5 min-w-[18px] items-center justify-center rounded-full bg-amber-500 px-1.5 text-[11px] leading-none font-semibold text-white">{count > 99 ? '99+' : count}</span>)}
              </a>
            )
          })}
        </nav>
      ))}
      <div className={cx('mt-auto flex items-center gap-2.5 rounded-lg p-2', collapsed && 'justify-center')} title={healthy === null ? '检测中' : healthy ? '服务已连接' : '服务不可用'}>
        <span className="relative grid size-7 shrink-0 place-items-center rounded-full bg-soft text-xs font-semibold text-fg">
          A
          <span className={cx('absolute right-0 bottom-0 size-2 rounded-full ring-2 ring-canvas', healthy === null ? 'bg-slate-400' : healthy ? 'bg-ok' : 'bg-danger')} aria-hidden />
        </span>
        {!collapsed ? (
          <div className="min-w-0 flex-1">
            <div className={cx('truncate text-xs font-medium', healthy === false ? 'text-danger' : 'text-fg')}>
              {healthy === null ? '检测服务中' : healthy ? 'AutoBid · 已连接' : '服务不可用'}
            </div>
            <div className="truncate text-[11px] text-muted">辅助起草与检查</div>
          </div>
        ) : <span className="sr-only">{healthy ? '服务已连接' : '服务不可用'}</span>}
      </div>
    </aside>
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
      {reason && <p className="mb-3 rounded-[10px] border border-[#fde68a] bg-[#fffbeb] px-3 py-2 text-sm text-[#92400e]">{reason}</p>}
      <Field label="Bearer Token" hint="对应服务端 AUTOBID_API_TOKEN；保存在本浏览器 localStorage，请求时以 Authorization: Bearer 发送，SSE 以 ?token= 发送。服务端未配置令牌时可留空。令牌变更后各页面以新令牌重新读取，未保存的编辑内容保留。">
        <Input type="password" value={value} autoFocus onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && save()} placeholder="未设置" />
      </Field>
    </Modal>
  )
}

/** 快捷跳转：检索页面名称与当前已加载的项目；不是全文搜索。 */
function QuickJump({ open, onClose, taskId }: { open: boolean; onClose: () => void; taskId?: number }) {
  const [q, setQ] = useState('')
  const [sel, setSel] = useState(0)
  const ref = useRef<HTMLDivElement>(null)
  useFocusTrap(ref, open)
  useEffect(() => { if (open) { setQ(''); setSel(0) } }, [open])
  const entries = useMemo(() => {
    const list: { key: string; label: string; hint: string; to: string }[] = []
    for (const g of globalNav) for (const it of g.items) list.push({ key: it.key, label: it.label, hint: g.group, to: it.to })
    if (taskId) for (const g of TASK_NAV) for (const p of g.pages) list.push({ key: `p-${p.page}`, label: p.label, hint: `当前项目 #${taskId} · ${g.label}`, to: taskUrl(taskId, p.page) })
    for (const t of loadedProjects()) list.push({ key: `t-${t.id}`, label: t.name, hint: `项目 #${t.id}`, to: taskUrl(t.id, 'overview') })
    return list
  }, [taskId, open]) // eslint-disable-line react-hooks/exhaustive-deps
  const s = q.trim().toLowerCase()
  const hits = (s ? entries.filter((e) => `${e.label} ${e.hint}`.toLowerCase().includes(s)) : entries).slice(0, 30)
  const go = (to: string) => { onClose(); void navigate(to) }
  if (!open) return null
  return (
    <div className="fixed inset-0 z-[60] flex items-start justify-center bg-[#17171766] p-4 pt-24" onMouseDown={onClose}>
      <div ref={ref} role="dialog" aria-modal="true" aria-label="快捷跳转" className="w-full max-w-lg overflow-hidden rounded-[14px] border border-line bg-panel shadow-xl"
        onMouseDown={(e) => e.stopPropagation()}
        onKeyDown={(e) => {
          if (e.key === 'Escape') onClose()
          else if (e.key === 'ArrowDown') { e.preventDefault(); setSel((n) => Math.min(n + 1, hits.length - 1)) }
          else if (e.key === 'ArrowUp') { e.preventDefault(); setSel((n) => Math.max(n - 1, 0)) }
          else if (e.key === 'Enter' && hits[sel]) go(hits[sel].to)
        }}>
        <div className="flex items-center gap-2 border-b border-line px-3">
          <Search className="size-4 text-muted" aria-hidden />
          <input autoFocus value={q} onChange={(e) => { setQ(e.target.value); setSel(0) }} placeholder="查找页面或已加载的项目"
            aria-label="查找页面或已加载的项目" role="combobox" aria-expanded="true" aria-controls="quickjump-list"
            className="h-12 min-w-0 flex-1 bg-transparent text-sm outline-none" />
        </div>
        <ul id="quickjump-list" role="listbox" className="max-h-80 overflow-y-auto p-1">
          {hits.length === 0 ? <li className="px-3 py-6 text-center text-sm text-muted">没有匹配的页面或已加载项目</li> : hits.map((h, i) => (
            <li key={h.key} role="option" aria-selected={i === sel}>
              <button type="button" onClick={() => go(h.to)} onMouseEnter={() => setSel(i)}
                className={cx('flex w-full items-center justify-between gap-3 rounded-lg px-3 py-2 text-left text-sm', i === sel && 'bg-soft')}>
                <span className="truncate">{h.label}</span><span className="shrink-0 text-xs text-muted">{h.hint}</span>
              </button>
            </li>
          ))}
        </ul>
        <p className="border-t border-line px-3 py-2 text-xs text-muted">仅检索页面名称与本次已加载的项目，不搜索招标原文。</p>
      </div>
    </div>
  )
}

/** 全局顶栏（列表与资料页）。项目详情页由 TaskHeader 替代，同页只有一套页头。 */
export function GlobalHeader({ status }: { status?: ReactNode }) {
  const shell = useShell()
  return (
    <header data-region="global-header" className="flex h-12 shrink-0 items-center justify-between gap-3 border-b border-line px-4 md:px-6">
      <div className="flex min-w-0 items-center gap-2">
        <IconButton label={shell.narrow ? '打开导航' : shell.collapsed ? '展开侧栏' : '折叠侧栏'} onClick={shell.toggleSidebar}>
          <PanelLeft className="size-4" strokeWidth={1.7} />
        </IconButton>
        <span className="mx-2 h-4 w-px bg-line" aria-hidden />
        <button type="button" onClick={() => window.dispatchEvent(new Event('autobid:quickjump'))}
          className="flex min-w-0 items-center gap-2 text-muted hover:text-fg">
          <Search className="size-4 shrink-0" strokeWidth={1.7} aria-hidden />
          <span className="truncate">查找页面或项目</span>
          <kbd className="hidden rounded border border-line bg-soft px-[5px] font-sans text-[11px] leading-[18px] sm:inline">Ctrl K</kbd>
        </button>
      </div>
      <div className="flex items-center gap-2">
        <span className="hidden text-xs text-muted md:inline">{status}</span>
        <ThemeToggle />
        <IconButton label="令牌设置" onClick={shell.openToken}><Settings2 className="size-4" strokeWidth={1.7} /></IconButton>
      </div>
    </header>
  )
}

export function LLMStatusText() {
  const [llm, setLLM] = useState<LLMHealth[] | null>(null)
  const [err, setErr] = useState(false)
  const poll = useCallback(async () => {
    try { setLLM(await api.llmHealth()); setErr(false) } catch { setErr(true) }
  }, [])
  useEffect(() => { void poll() }, [poll])
  useInterval(poll, 15000)
  if (err) return <span className="text-danger">模型状态读取失败</span>
  if (!llm) return <>模型状态读取中</>
  const open = llm.filter((h) => h.open).length
  return open > 0 ? <span className="text-danger" title={llm.filter((h) => h.open).map((h) => `#${h.profile_id} ${h.last_error}`).join('\n')}>{open} 个模型配置熔断中</span>
    : <>{llm.length ? `${llm.length} 个模型配置无熔断` : '模型暂无调用记录'}</>
}

export default function AppShell({ active, taskId, children }: { active: NavKey['key']; taskId?: number; children: ReactNode }) {
  const [vw, setVW] = useState(() => viewportOf(window.innerWidth))
  const [pref, setPref] = useState(readPref)
  const [drawer, setDrawer] = useState(false)
  const [token, setTokenOpen] = useState<{ open: boolean; reason: string }>({ open: false, reason: '' })
  const [jump, setJump] = useState(false)
  const [healthy, setHealthy] = useState<boolean | null>(null)
  const [pendingCount, setPendingCount] = useState(0)
  const drawerRef = useRef<HTMLDivElement>(null)
  useFocusTrap(drawerRef, drawer)

  useEffect(() => {
    const on = () => setVW(viewportOf(window.innerWidth))
    window.addEventListener('resize', on)
    return () => window.removeEventListener('resize', on)
  }, [])
  useEffect(() => { if (vw !== 'narrow') setDrawer(false) }, [vw])

  const narrow = vw === 'narrow'
  // 用户主动设置的桌面偏好优先，窗口变化不反复覆盖。
  const collapsed = !narrow && (pref ? pref === 'collapsed' : vw === 'mid')
  const toggleSidebar = useCallback(() => {
    if (narrow) { setDrawer((v) => !v); return }
    const next = collapsed ? 'expanded' : 'collapsed'
    setPref(next)
    try { localStorage.setItem(PREF_KEY, next) } catch { /* 隐私模式下仅本次有效 */ }
  }, [narrow, collapsed])

  const pollHealth = useCallback(async () => setHealthy(await api.healthz()), [])
  const refreshPending = useCallback(async () => {
    try { setPendingCount((await api.listIntercepts()).length) } catch { /* 侧栏数量读取失败时保持原值 */ }
  }, [])
  useEffect(() => { void pollHealth(); void refreshPending() }, [pollHealth, refreshPending])
  useInterval(pollHealth, 15000)
  useInterval(refreshPending, 10000)

  useEffect(() => {
    const onUnauthorized = () => setTokenOpen((t) => (t.open ? t : { open: true, reason: '服务端返回 401 未授权，请设置正确的 API Token。未保存的编辑内容仍保留在页面中。' }))
    const onToken = () => { forgetProjects(); void refreshPending() }
    const onJump = () => setJump(true)
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); setJump(true) }
    }
    window.addEventListener('autobid:unauthorized', onUnauthorized)
    window.addEventListener('autobid:token-changed', onToken)
    window.addEventListener('autobid:quickjump', onJump)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('autobid:unauthorized', onUnauthorized)
      window.removeEventListener('autobid:token-changed', onToken)
      window.removeEventListener('autobid:quickjump', onJump)
      window.removeEventListener('keydown', onKey)
    }
  }, [refreshPending])

  const ctx: ShellCtx = {
    toggleSidebar, collapsed, narrow, pendingCount,
    openToken: () => setTokenOpen({ open: true, reason: '' }),
    refreshPending: () => void refreshPending(),
  }
  return (
    <Ctx.Provider value={ctx}>
      <div className="flex h-dvh overflow-hidden bg-canvas text-fg">
        {!narrow && <Sidebar active={active} collapsed={collapsed} pendingCount={pendingCount} healthy={healthy} />}
        {narrow && drawer && (
          <div className="fixed inset-0 z-40 bg-[#17171740]" onMouseDown={() => setDrawer(false)}>
            <div ref={drawerRef} role="dialog" aria-modal="true" aria-label="全局导航" onMouseDown={(e) => e.stopPropagation()}
              onKeyDown={(e) => e.key === 'Escape' && setDrawer(false)}
              className="h-full w-[288px] max-w-[calc(100vw-32px)] bg-canvas shadow-xl">
              <Sidebar drawer active={active} collapsed={false} pendingCount={pendingCount} healthy={healthy} onNavigate={() => setDrawer(false)} />
            </div>
          </div>
        )}
        <main data-region="main-frame"
          className={cx('flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden bg-panel',
            narrow ? 'm-0' : cx('my-2 mr-2 rounded-[14px] border border-line shadow-[0_1px_3px_#00000014]', collapsed ? 'ml-2' : 'ml-0'))}>
          {children}
        </main>
      </div>
      <TokenDialog open={token.open} reason={token.reason} onClose={() => setTokenOpen({ open: false, reason: '' })} />
      <QuickJump open={jump} onClose={() => setJump(false)} taskId={taskId} />
    </Ctx.Provider>
  )
}

/** PageFrame 列表与资料页：全局顶栏 + 24px 内边距的主滚动区。 */
export function PageFrame({ children, title, description, actions }: { children: ReactNode; title?: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <>
      <GlobalHeader status={<LLMStatusText />} />
      <div className="min-h-0 min-w-0 flex-1 overflow-auto p-4 md:p-6">
        {(title || actions) && (
          <div className="mb-6 flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              {title && <h1 className="text-2xl leading-8 font-semibold">{title}</h1>}
              {description && <p className="mt-1 text-muted">{description}</p>}
            </div>
            {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
          </div>
        )}
        {children}
      </div>
    </>
  )
}
