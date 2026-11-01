import { useEffect, useState } from 'react'

// History 路由：订阅 pathname + search + hash，支持 push / replace 与离开保护（应用内导航与浏览器后退/前进）。
// 后端 server/static.go 对未知前端路径做 SPA 回退，深层地址刷新可直接打开。

export interface Loc { pathname: string; search: string; hash: string }

function current(): Loc {
  const { pathname, search, hash } = window.location
  return { pathname, search, hash }
}

const href = (l: Loc) => l.pathname + l.search + l.hash
function parse(h: string): Loc {
  const u = new URL(h, window.location.origin)
  return { pathname: u.pathname, search: u.search, hash: u.hash }
}

// committed 是页面实际渲染的地址。浏览器后退/前进被离开保护拦截期间，window.location 会短暂指向目标地址，
// 页面只跟随 committed，因此受保护页面不会被提前卸载。
let committed = href(current())
const NAV_EVENT = 'autobid:navigate'

// 每条历史记录在 state.idx 中带序号，后退/前进时据此算出步数，拦截时用 history.go 原位退回，
// 允许后再用 history.go 前往目标，不新增历史记录、不截断前进记录。
type HistState = { idx?: unknown } | null
const idxOf = (st: HistState): number | undefined => (typeof st?.idx === 'number' ? st.idx : undefined)
let curIdx = idxOf(window.history.state) ?? 0
if (idxOf(window.history.state) === undefined) {
  const prev: unknown = window.history.state
  window.history.replaceState({ ...(prev && typeof prev === 'object' ? prev : {}), idx: curIdx }, '')
}

function commit(target: string, idx?: number) {
  committed = target
  if (idx !== undefined) curIdx = idx
  window.dispatchEvent(new Event(NAV_EVENT))
}

// ---------------------------------------------------------------------------
// 离开保护：守卫返回 false 时取消导航。守卫可以弹出「保存并离开 / 放弃修改 / 继续编辑」对话框。
// ---------------------------------------------------------------------------

export type LeaveGuard = (to: string) => boolean | Promise<boolean>
const guards = new Set<LeaveGuard>()

export function registerLeaveGuard(g: LeaveGuard): () => void {
  guards.add(g)
  return () => { guards.delete(g) }
}

async function allowed(to: string): Promise<boolean> {
  for (const g of [...guards]) if (!(await g(to))) return false
  return true
}

function sameDoc(to: string) {
  return href(parse(to))
}

export interface NavOpts { replace?: boolean; force?: boolean }

/** navigate 用户主动切换页面用 push；自动补全、地址归一化与临时筛选用 replace。force 跳过离开保护。 */
export async function navigate(to: string, opts: NavOpts = {}): Promise<boolean> {
  const target = sameDoc(to)
  if (target === committed) return true
  if (!opts.force && !(await allowed(target))) return false
  if (opts.replace) window.history.replaceState({ idx: curIdx }, '', target)
  else window.history.pushState({ idx: curIdx + 1 }, '', target)
  commit(target, opts.replace ? curIdx : curIdx + 1)
  return true
}

// 拦截浏览器后退/前进的两步过程：restore 表示正在退回受保护页面，redo 表示守卫已允许、正在前往目标。
let pending: { kind: 'restore'; target: string; delta: number } | { kind: 'redo' } | null = null

window.addEventListener('popstate', (e: PopStateEvent) => {
  const target = href(current())
  const idx = idxOf(e.state as HistState)
  if (pending?.kind === 'restore') {
    const { target: to, delta } = pending
    pending = null
    void allowed(to).then((ok) => {
      if (!ok) return
      pending = { kind: 'redo' }
      window.history.go(delta)
    })
    return
  }
  if (pending?.kind === 'redo') { pending = null; commit(target, idx); return }
  if (guards.size === 0 || target === committed) { commit(target, idx); return }
  if (idx === undefined || idx === curIdx) {
    // 没有序号的条目（如页面内锚点产生的记录）无法计算步数：先压回受保护页面，允许后再前往目标。
    // 这种情况下会新增一条历史记录。
    window.history.pushState({ idx: curIdx + 1 }, '', committed)
    curIdx += 1
    void allowed(target).then((ok) => {
      if (!ok) return
      window.history.pushState({ idx: curIdx + 1 }, '', target)
      commit(target, curIdx + 1)
    })
    return
  }
  pending = { kind: 'restore', target, delta: idx - curIdx }
  window.history.go(curIdx - idx)
})

export function useLocation(): Loc {
  const [loc, setLoc] = useState(() => parse(committed))
  useEffect(() => {
    const on = () => setLoc((prev) => (href(prev) === committed ? prev : parse(committed)))
    on()
    window.addEventListener(NAV_EVENT, on)
    return () => window.removeEventListener(NAV_EVENT, on)
  }, [])
  return loc
}

/** useSearchParams 返回当前查询参数；setQuery 以 replace 方式合并更新（用于筛选、面板切换等临时状态）。 */
export function useSearchParams(): [URLSearchParams, (patch: Record<string, string | number | undefined | null>, opts?: NavOpts) => void] {
  const loc = useLocation()
  const params = new URLSearchParams(loc.search)
  const set = (patch: Record<string, string | number | undefined | null>, opts: NavOpts = { replace: true }) => {
    const at = parse(committed)
    const p = new URLSearchParams(at.search)
    for (const [k, v] of Object.entries(patch)) {
      if (v === undefined || v === null || v === '') p.delete(k)
      else p.set(k, String(v))
    }
    p.sort()
    const s = p.toString()
    void navigate(at.pathname + (s ? `?${s}` : '') + at.hash, opts)
  }
  return [params, set]
}

/** linkProps 让 <a> 保留链接语义（Ctrl/Cmd/中键新窗口打开），普通点击走应用内导航。 */
export function linkProps(to: string, opts: NavOpts = {}) {
  return {
    href: to,
    onClick: (e: { defaultPrevented: boolean; button: number; metaKey: boolean; ctrlKey: boolean; shiftKey: boolean; altKey: boolean; preventDefault: () => void }) => {
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      e.preventDefault()
      void navigate(to, opts)
    },
  }
}
