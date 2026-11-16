// 前端页面地址的唯一来源：路径解析、旧地址兼容、类型化 URL 构造与项目导航定义。
// 本文件不依赖 DOM / React，可用 `node --test` 直接验证（见 routes.test.ts）。
// 这里只定义前端页面地址，不改变任何 /api/... 后端路由。

export type TaskPage =
  | 'overview' | 'documents' | 'requirements' | 'research' | 'outline' | 'writing'
  | 'matrix' | 'consistency' | 'report' | 'export' | 'monitor' | 'elements' | 'approvals' | 'llm' | 'evidence' | 'members'

export const TASK_PAGES: readonly TaskPage[] = [
  'overview', 'documents', 'requirements', 'research', 'outline', 'writing',
  'matrix', 'consistency', 'report', 'export', 'monitor', 'elements', 'approvals', 'llm', 'evidence', 'members',
]

export type Route =
  | { name: 'dashboard' }
  | { name: 'tasks' }
  | { name: 'task'; taskId: number; page: TaskPage; sectionId?: number }
  | { name: 'findings' }
  | { name: 'assets' }
  | { name: 'llm-records' }
  | { name: 'intercepts' }
  | { name: 'intercept-rules' }
  | { name: 'credentials' }
  | { name: 'knowledge' }
  | { name: 'llm' }
  | { name: 'agents' }
  | { name: 'skills' }
  | { name: 'tools' }
  | { name: 'admin-users' }
  | { name: 'proposal'; intent: 'outline' | 'export' }
  | { name: 'redirect'; to: string }
  | { name: 'notfound'; reason: string }

/** 正整数标识；其余（0、负数、小数、前导零、超大值）一律视为无效。 */
export function parseId(s: string | undefined): number | undefined {
  if (!s || !/^[1-9]\d{0,14}$/.test(s)) return undefined
  return Number(s)
}

export function parseRoute(pathname: string, search = ''): Route {
  const parts = pathname.split('/').filter(Boolean).map((p) => decodeURIComponent(p))
  const q = new URLSearchParams(search)
  if (parts.length === 0) return { name: 'redirect', to: '/tasks' }
  switch (parts[0]) {
    case 'dashboard': return parts.length === 1 ? { name: 'dashboard' } : notFound()
    case 'tasks': {
      if (parts.length === 1) return { name: 'tasks' }
      const taskId = parseId(parts[1])
      if (!taskId) return { name: 'notfound', reason: `无效的项目编号「${parts[1]}」` }
      if (parts.length === 2) return { name: 'redirect', to: taskUrl(taskId, 'documents') }
      // 旧写作入口：保留原「首次打开即准备要求」的语义
      if (parts[2] === 'proposal' && parts.length === 3) return { name: 'redirect', to: taskUrl(taskId, 'requirements') }
      const page = parts[2] as TaskPage
      if (!TASK_PAGES.includes(page)) return { name: 'notfound', reason: `项目页面「${parts[2]}」不存在` }
      if (page === 'writing' && parts.length === 4) {
        const sectionId = parseId(parts[3])
        if (!sectionId) return { name: 'notfound', reason: `无效的章节编号「${parts[3]}」` }
        return { name: 'task', taskId, page, sectionId }
      }
      if (parts.length > 3) return { name: 'notfound', reason: '页面地址不存在' }
      return { name: 'task', taskId, page }
    }
    case 'findings': return parts.length === 1 ? { name: 'findings' } : notFound()
    case 'assets': return parts.length === 1 ? { name: 'assets' } : notFound()
    case 'llm-records': return parts.length === 1 ? { name: 'llm-records' } : notFound()
    case 'intercepts':
      if (parts.length === 1) return { name: 'intercepts' }
      if (parts.length === 2 && parts[1] === 'rules') return { name: 'intercept-rules' }
      return notFound()
    case 'credentials': return parts.length === 1 ? { name: 'credentials' } : notFound()
    case 'knowledge': return parts.length === 1 ? { name: 'knowledge' } : notFound()
    case 'llm': return parts.length === 1 ? { name: 'llm' } : notFound()
    case 'agents': return parts.length === 1 ? { name: 'agents' } : notFound()
    case 'skills': return parts.length === 1 ? { name: 'skills' } : notFound()
    case 'tools': return parts.length === 1 ? { name: 'tools' } : notFound()
    case 'admin': return parts.length === 2 && parts[1] === 'users' ? { name: 'admin-users' } : notFound()
    case 'function':
      if (parts.length === 2 && parts[1] === 'tasks') return { name: 'redirect', to: '/tasks' }
      if (parts.length === 2 && parts[1] === 'findings') return { name: 'redirect', to: '/findings' }
      if (parts.length === 2 && parts[1] === 'assets') return { name: 'redirect', to: '/assets' }
      if (parts.length === 2 && parts[1] === 'llm-records') return { name: 'redirect', to: '/llm-records' }
      return notFound()
    case 'system':
      if (parts.length === 2 && parts[1] === 'llm') return { name: 'redirect', to: '/llm' }
      if (parts.length === 2 && parts[1] === 'agents') return { name: 'redirect', to: '/agents' }
      if (parts.length === 2 && parts[1] === 'skills') return { name: 'redirect', to: '/skills' }
      if (parts.length === 2 && parts[1] === 'tools') return { name: 'redirect', to: '/tools' }
      if (parts.length === 2 && parts[1] === 'intercept') return { name: 'redirect', to: '/intercepts/rules' }
      if (parts.length === 3 && parts[1] === 'intercept' && parts[2] === 'approvals') return { name: 'redirect', to: '/intercepts' }
      return notFound()
    case 'proposal':
      return parts.length === 1 ? { name: 'proposal', intent: q.get('intent') === 'export' ? 'export' : 'outline' } : notFound()
    case 'roadmap':
      if (parts.length === 2 && parts[1] === 'phase4') return { name: 'redirect', to: '/knowledge' }
      if (parts.length === 2 && parts[1] === 'phase5') return { name: 'redirect', to: '/proposal' }
      if (parts.length === 2 && parts[1] === 'phase6') return { name: 'redirect', to: '/proposal?intent=export' }
      return notFound()
    default:
      return notFound()
  }
}

function notFound(): Route {
  return { name: 'notfound', reason: '页面地址不存在' }
}

export type Query = Record<string, string | number | undefined | null | false>

/** withQuery 丢弃空值并按键名固定顺序输出，保证同一状态生成同一 URL。 */
export function withQuery(path: string, query?: Query): string {
  if (!query) return path
  const p = new URLSearchParams()
  for (const k of Object.keys(query).sort()) {
    const v = query[k]
    if (v === undefined || v === null || v === false || v === '') continue
    p.set(k, String(v))
  }
  const s = p.toString()
  return s ? `${path}?${s}` : path
}

export function taskUrl(taskId: number, page: TaskPage = 'overview', opts: { sectionId?: number; query?: Query } = {}): string {
  const base = page === 'writing' && opts.sectionId ? `/tasks/${taskId}/writing/${opts.sectionId}` : `/tasks/${taskId}/${page}`
  return withQuery(base, opts.query)
}

// ---------------------------------------------------------------------------
// 项目导航：一级最多六组，同组多页时显示紧凑二级导航。分组只是导航，不是业务状态机。
// ---------------------------------------------------------------------------

export interface NavPage { page: TaskPage; label: string }
export interface NavGroup { key: string; label: string; pages: NavPage[] }

export const TASK_NAV: readonly NavGroup[] = [
  { key: 'overview', label: '项目概览', pages: [{ page: 'overview', label: '项目概览' }, { page: 'members', label: '成员与质量策略' }] },
  { key: 'tender', label: '招标解读', pages: [
    { page: 'documents', label: '招标包文件' }, { page: 'requirements', label: '要求与否决项' }, { page: 'research', label: '证据检索' }, { page: 'evidence', label: '证据与承诺' },
  ] },
  { key: 'writing', label: '大纲与写作', pages: [{ page: 'outline', label: '大纲及审批' }, { page: 'writing', label: '章节写作' }] },
  { key: 'review', label: '核验', pages: [
    { page: 'matrix', label: '响应矩阵' }, { page: 'consistency', label: '一致性核验' }, { page: 'report', label: '推演审查报告' },
  ] },
  { key: 'export', label: '成稿导出', pages: [{ page: 'export', label: '成稿导出' }] },
  { key: 'records', label: '执行记录', pages: [
    { page: 'monitor', label: '实时过程' }, { page: 'elements', label: '要素与结论' }, { page: 'approvals', label: '工具调用审批' }, { page: 'llm', label: '模型用量' },
  ] },
]

export function navGroupOf(page: TaskPage): NavGroup {
  return TASK_NAV.find((g) => g.pages.some((p) => p.page === page)) ?? TASK_NAV[0]
}

export function pageLabel(page: TaskPage): string {
  for (const g of TASK_NAV) for (const p of g.pages) if (p.page === page) return p.label
  return page
}

// ---------------------------------------------------------------------------
// 查询参数白名单：非法值回落为缺省值，不把任意字符串传进页面状态。
// ---------------------------------------------------------------------------

export const LIST_STATUSES = ['created', 'running', 'paused', 'done', 'failed', 'timeout'] as const
export const LIST_SORTS = ['created_desc', 'created_asc', 'name_asc', 'id_desc'] as const
export const PAGE_SIZES = [20, 50, 100] as const

export interface ListQuery { q: string; status: string; sort: (typeof LIST_SORTS)[number]; page: number; pageSize: number }

export function parseListQuery(search: string): ListQuery {
  const p = new URLSearchParams(search)
  const status = p.get('status') ?? ''
  const sort = p.get('sort') ?? ''
  const size = Number(p.get('pageSize'))
  const page = parseId(p.get('page') ?? undefined) ?? 1
  return {
    q: (p.get('q') ?? '').slice(0, 200),
    status: (LIST_STATUSES as readonly string[]).includes(status) ? status : '',
    sort: (LIST_SORTS as readonly string[]).includes(sort) ? sort as ListQuery['sort'] : 'created_desc',
    page: Math.min(page, 10000),
    pageSize: (PAGE_SIZES as readonly number[]).includes(size) ? size : 20,
  }
}

export function listQueryToQuery(l: Partial<ListQuery>): Query {
  return {
    q: l.q || undefined, status: l.status || undefined,
    sort: l.sort && l.sort !== 'created_desc' ? l.sort : undefined,
    page: l.page && l.page > 1 ? l.page : undefined,
    pageSize: l.pageSize && l.pageSize !== 20 ? l.pageSize : undefined,
  }
}

export const REQUIREMENT_FILTERS = ['all', 'veto', 'scoring', 'needs_revision', 'evidence_gap'] as const
export type RequirementFilter = (typeof REQUIREMENT_FILTERS)[number]

export function pickEnum<T extends string>(v: string | null, allowed: readonly T[], fallback: T): T {
  return v !== null && (allowed as readonly string[]).includes(v) ? v as T : fallback
}

export const WRITING_PANELS = ['requirements', 'evidence', 'history', 'charts'] as const
export type WritingPanel = (typeof WRITING_PANELS)[number]

export const ACTIVITY_KINDS = ['all', 'round', 'text', 'tool', 'tool_error', 'state', 'error', 'intercept_request', 'intercept_decided'] as const
export type ActivityKind = (typeof ACTIVITY_KINDS)[number]

/** work 参数：'all' 或正整数工作节点 ID；非法值返回 undefined，由页面明确提示。 */
export function parseWork(v: string | null): 'all' | number | undefined {
  if (v === null || v === '' || v === 'all') return 'all'
  return parseId(v)
}

/** 读取可选的正整数查询参数；存在但非法时返回 NaN，便于页面提示「定位失败」。 */
export function optionalId(p: URLSearchParams, key: string): number | undefined {
  const v = p.get(key)
  if (v === null || v === '') return undefined
  return parseId(v) ?? Number.NaN
}
