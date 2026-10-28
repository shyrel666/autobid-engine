// AutoBid 控制面 HTTP 客户端：所有 /api/* 请求携带 Authorization: Bearer <token>；
// SSE（EventSource 无法设置请求头）改用 ?token=。401 时广播 autobid:unauthorized 以弹出令牌设置。

import type {
  Activity, Asset, CompanyCredential, CreateTaskReq, CredentialReq, DocumentWithChunks, EntityEdge, Finding,
  InterceptAudit, InterceptPending, LLMChain, LLMHealth, LLMProfile, LLMUsage, NodeDetail, Overview, Coverage,
  Task, TaskReport, TaskView, UploadDocumentReq, UploadDocumentResp,
} from './types'

const TOKEN_KEY = 'autobid.token'

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    return ''
  }
}

export function setToken(token: string) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {
    /* 隐私模式等场景下 localStorage 不可用，仅本次会话生效 */
  }
  window.dispatchEvent(new Event('autobid:token-changed'))
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export function errorText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return `网络错误：${err.message}`
    if (err.status === 401) return '未授权：请检查 API Token'
    return `HTTP ${err.status}：${err.message}`
  }
  return err instanceof Error ? err.message : String(err)
}

async function send(method: string, path: string, body?: unknown): Promise<Response> {
  const headers: Record<string, string> = {}
  const token = getToken()
  if (token) headers.Authorization = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  let resp: Response
  try {
    resp = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  } catch (e) {
    throw new ApiError(0, e instanceof Error ? e.message : '无法连接服务')
  }
  if (!resp.ok) {
    let msg = resp.statusText
    try {
      const j = await resp.json()
      if (j && typeof j.error === 'string') msg = j.error
    } catch {
      /* 非 JSON 错误体 */
    }
    if (resp.status === 401) window.dispatchEvent(new Event('autobid:unauthorized'))
    throw new ApiError(resp.status, msg)
  }
  return resp
}

async function json<T>(method: string, path: string, body?: unknown): Promise<T> {
  const resp = await send(method, path, body)
  if (resp.status === 204) return undefined as T
  const text = await resp.text()
  return (text ? JSON.parse(text) : undefined) as T
}

const get = <T>(path: string) => json<T>('GET', path)
const post = <T>(path: string, body?: unknown) => json<T>('POST', path, body ?? {})
const put = <T>(path: string, body: unknown) => json<T>('PUT', path, body)
const del = (path: string) => json<void>('DELETE', path)

function qs(params: Record<string, string | number | undefined>): string {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '') p.set(k, String(v))
  const s = p.toString()
  return s ? `?${s}` : ''
}

const t = (id: number) => `/api/tasks/${id}`

export const api = {
  // 健康检查（/healthz 不需要鉴权）
  healthz: async () => {
    try {
      const r = await fetch('/healthz')
      return r.ok
    } catch {
      return false
    }
  },
  llmHealth: () => get<LLMHealth[]>('/api/llm/health'),

  // 任务生命周期
  listTasks: (status?: string) => get<Task[] | null>(`/api/tasks${qs({ status, limit: 500 })}`),
  createTask: (req: CreateTaskReq) => post<TaskView>('/api/tasks', req),
  getTask: (id: number) => get<TaskView>(t(id)),
  deleteTask: (id: number) => del(t(id)),
  taskAction: (id: number, action: 'start' | 'pause' | 'resume' | 'revive') => post<TaskView>(`${t(id)}/${action}`),
  addGoal: (id: number, summary: string) => post<{ goal_id: number; revived: boolean }>(`${t(id)}/goals`, { summary }),
  addHint: (id: number, text: string, anchors?: number[]) =>
    post<{ hint_id: number }>(`${t(id)}/hints`, { text, anchors: anchors ?? [] }),
  coverage: (id: number) => get<Coverage>(`${t(id)}/coverage`),

  // 单工作槽控制
  controlWork: (id: number, intentID: number, action: 'pause' | 'cancel') =>
    post(`${t(id)}/works/${intentID}/control`, { action }),
  steerWork: (id: number, intentID: number, message: string) =>
    post<{ queued: boolean }>(`${t(id)}/works/${intentID}/steer`, { message }),
  messageWork: (id: number, intentID: number, message: string) =>
    post<{ resumed: boolean }>(`${t(id)}/works/${intentID}/message`, { message }),

  // 推演图与产出物
  overview: (id: number) => get<Overview>(`${t(id)}/overview`),
  node: (id: number, nodeID: number) => get<NodeDetail>(`${t(id)}/nodes/${nodeID}`),
  findings: (id: number, category?: string) => get<Finding[] | null>(`${t(id)}/findings${qs({ category })}`),
  assets: (id: number, types?: string[]) => get<Asset[] | null>(`${t(id)}/assets${qs({ types: types?.join(',') })}`),
  assetEdges: (assetID: number, dir: 'out' | 'in' = 'out') => get<EntityEdge[] | null>(`/api/assets/${assetID}/edges${qs({ dir })}`),

  // 报告
  report: (id: number) => get<TaskReport>(`${t(id)}/report`),
  reportMarkdown: async (id: number) => (await send('GET', `${t(id)}/report?format=markdown`)).text(),

  // 招标文档
  uploadDocument: (id: number, req: UploadDocumentReq) => post<UploadDocumentResp>(`${t(id)}/documents`, req),
  listDocuments: (id: number, content = true) =>
    get<DocumentWithChunks[]>(`${t(id)}/documents${content ? '?content=1' : ''}`),

  // 企业资信库
  listCredentials: (f: { category?: string; q?: string; valid_on?: string; limit?: number }) =>
    get<CompanyCredential[]>(`/api/company/credentials${qs(f)}`),
  createCredential: (req: CredentialReq) =>
    post<(CompanyCredential & { created: boolean })[]>('/api/company/credentials', req),
  updateCredential: (id: number, req: CredentialReq) => put<CompanyCredential>(`/api/company/credentials/${id}`, req),
  deleteCredential: (id: number) => del(`/api/company/credentials/${id}`),

  // LLM 配置
  listProfiles: () => get<LLMProfile[] | null>('/api/llm/profiles'),
  createProfile: (p: Partial<LLMProfile>) => post<LLMProfile>('/api/llm/profiles', p),
  updateProfile: (id: number, p: Partial<LLMProfile>) => put<LLMProfile>(`/api/llm/profiles/${id}`, p),
  deleteProfile: (id: number) => del(`/api/llm/profiles/${id}`),
  bindAgent: (agent: 'planner' | 'worker' | 'judge', profileID: number) =>
    put<{ agent: string; profile_id: number }>(`/api/llm/bindings/${agent}`, { profile_id: profileID }),
  llmUsage: (id: number) => get<LLMUsage[]>(`${t(id)}/llm-usage`),
  llmChain: (id: number) => get<LLMChain>(`${t(id)}/llm-chain`),
  setLLMChain: (id: number, profileIDs: number[]) => put<LLMChain>(`${t(id)}/llm-chain`, { profile_ids: profileIDs }),
  resetLLMChain: (id: number) => post<LLMChain>(`${t(id)}/llm-chain/reset`),

  // 拦截审批
  listIntercepts: (taskID?: number) => get<InterceptPending[]>(`/api/intercepts${qs({ task_id: taskID })}`),
  decideIntercept: (pendingID: number, allow: boolean, reason: string) =>
    post<InterceptPending>(`/api/intercepts/${pendingID}/decide`, { allow, reason }),
  interceptAudit: (id: number) => get<InterceptAudit[] | null>(`${t(id)}/intercept-audit`),
}

export function eventsURL(taskID: number): string {
  const token = getToken()
  return `${t(taskID)}/events${token ? `?token=${encodeURIComponent(token)}` : ''}`
}

export type { Activity }
