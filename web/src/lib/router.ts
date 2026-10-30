import { useEffect, useState } from 'react'

// History 路由：订阅 pathname + search + hash，支持 push / replace 与离开保护（应用内导航与浏览器后退/前进）。
// 后端 server/static.go 对未知前端路径做 SPA 回退，深层地址刷新可直接打开。

export interface Loc { pathname: string; search: string; hash: string }

function current(): Loc {
  const { pathname, search, hash } = window.location
  return { pathname, search, hash }
}

const href = (l: Loc) => l.pathname + l.search + l.hash
let committed = href(current())
const NAV_EVENT = 'autobid:navigate'

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
  const u = new URL(to, window.location.origin)
  return u.pathname + u.search + u.hash
}

export interface NavOpts { replace?: boolean; force?: boolean }

/** navigate 用户主动切换页面用 push；自动补全、地址归一化与临时筛选用 replace。force 跳过离开保护。 */
export async function navigate(to: string, opts: NavOpts = {}): Promise<boolean> {
  const target = sameDoc(to)
  if (target === href(current())) return true
  if (!opts.force && !(await allowed(target))) return false
  if (opts.replace) window.history.replaceState(null, '', target)
  else window.history.pushState(null, '', target)
  committed = target
  window.dispatchEvent(new Event(NAV_EVENT))
  return true
}

window.addEventListener('popstate', () => {
  const target = href(current())
  if (guards.size === 0) { committed = target; window.dispatchEvent(new Event(NAV_EVENT)); return }
  // 浏览器已经改变地址：先恢复到受保护页面，守卫允许后再前往目标。
  // 受 History API 限制，被拦下后重新前往会新增一条历史记录。
  const from = committed
  window.history.pushState(null, '', from)
  void allowed(target).then((ok) => {
    if (!ok) return
    window.history.pushState(null, '', target)
    committed = target
    window.dispatchEvent(new Event(NAV_EVENT))
  })
})

export function useLocation(): Loc {
  const [loc, setLoc] = useState(current)
  useEffect(() => {
    const on = () => setLoc((prev) => {
      const next = current()
      return href(prev) === href(next) ? prev : next
    })
    window.addEventListener(NAV_EVENT, on)
    window.addEventListener('popstate', on)
    return () => {
      window.removeEventListener(NAV_EVENT, on)
      window.removeEventListener('popstate', on)
    }
  }, [])
  return loc
}

/** useSearchParams 返回当前查询参数；setQuery 以 replace 方式合并更新（用于筛选、面板切换等临时状态）。 */
export function useSearchParams(): [URLSearchParams, (patch: Record<string, string | number | undefined | null>, opts?: NavOpts) => void] {
  const loc = useLocation()
  const params = new URLSearchParams(loc.search)
  const set = (patch: Record<string, string | number | undefined | null>, opts: NavOpts = { replace: true }) => {
    const p = new URLSearchParams(window.location.search)
    for (const [k, v] of Object.entries(patch)) {
      if (v === undefined || v === null || v === '') p.delete(k)
      else p.set(k, String(v))
    }
    p.sort()
    const s = p.toString()
    void navigate(window.location.pathname + (s ? `?${s}` : '') + window.location.hash, opts)
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
