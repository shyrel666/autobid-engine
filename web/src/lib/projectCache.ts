import type { Task } from '../api/types'

// 已加载项目的内存缓存，仅供快捷跳转（Ctrl/Cmd+K）检索名称；不代表全部项目，也不持久化。
let cached: Pick<Task, 'id' | 'name'>[] = []

export function rememberProjects(list: Pick<Task, 'id' | 'name'>[]) {
  const byID = new Map(cached.map((t) => [t.id, t]))
  for (const t of list) byID.set(t.id, { id: t.id, name: t.name })
  cached = [...byID.values()].sort((a, b) => b.id - a.id)
}

export function loadedProjects() {
  return cached
}

export function forgetProjects() {
  cached = []
}
