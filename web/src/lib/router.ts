import { useEffect, useState } from 'react'

// 极简 History 路由：后端 SPA 回退保证任意前端路径刷新后都能回到 index.html。
export function navigate(to: string) {
  if (to === window.location.pathname + window.location.search) return
  window.history.pushState(null, '', to)
  window.dispatchEvent(new Event('autobid:navigate'))
}

export function usePath(): string {
  const [path, setPath] = useState(window.location.pathname)
  useEffect(() => {
    const on = () => setPath(window.location.pathname)
    window.addEventListener('popstate', on)
    window.addEventListener('autobid:navigate', on)
    return () => {
      window.removeEventListener('popstate', on)
      window.removeEventListener('autobid:navigate', on)
    }
  }, [])
  return path
}

export type Route =
  | { name: 'tasks' }
  | { name: 'task'; id: number; tab: string }
  | { name: 'intercepts' }
  | { name: 'credentials' }
  | { name: 'llm' }
  | { name: 'roadmap'; phase: string }
  | { name: 'notfound' }

export function matchRoute(path: string): Route {
  const parts = path.split('/').filter(Boolean)
  if (parts.length === 0 || (parts[0] === 'tasks' && parts.length === 1)) return { name: 'tasks' }
  if (parts[0] === 'tasks' && /^\d+$/.test(parts[1] ?? '')) {
    return { name: 'task', id: Number(parts[1]), tab: parts[2] ?? 'documents' }
  }
  if (parts[0] === 'intercepts') return { name: 'intercepts' }
  if (parts[0] === 'credentials') return { name: 'credentials' }
  if (parts[0] === 'llm') return { name: 'llm' }
  if (parts[0] === 'roadmap') return { name: 'roadmap', phase: parts[1] ?? 'phase4' }
  return { name: 'notfound' }
}
