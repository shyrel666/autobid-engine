// AutoBid 控制面 HTTP 客户端：所有 /api/* 请求携带 Authorization: Bearer <token>；
// SSE（EventSource 无法设置请求头）改用 ?token=。401 时广播 autobid:unauthorized 以弹出令牌设置。

import type {
  Activity, Asset, CompanyCredential, CreateTaskReq, CredentialReq, DocumentWithChunks, EntityEdge, Finding,
  InterceptAudit, InterceptPending, KnowledgeChunk, KnowledgeDocument, KnowledgeFileReq, KnowledgeSearchReq, RetrievalResult, RetrievalEvidence, KnowledgeIndex, EvidenceClaim,
  KnowledgeTextReq, KnowledgeUploadResp, LLMChain, LLMHealth, LLMProfile, LLMUsage, NodeDetail, Overview, Coverage,
  Task, TaskReport, TaskView, UploadDocumentFileReq, UploadDocumentReq, UploadDocumentResp, ResearchRequest, RetrievalRun,
  ExtractResult, OutlineDetail, OutlineInput, OutlineList, ProposalSection, RequirementInput, RequirementItem, RequirementManifest,
  RequirementsView, SaveRevisionReq, SaveRevisionResult, SectionClaim, SectionDetail,
  ComplianceCoverage, ConsistencyFinding, ConsistencyRun, ConsistencyRunDetail, ExportReadiness, MatrixView, ResolveFindingReq, VerifyMatrixResult,
  ChartReq, CreateExportReq, ExportJob, ExportPreflight, ExportStatus, ExportTemplate, ExportWarning, ProposalChart,
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
  code?: string
  // data 为错误响应的 JSON 体（例如 409 时归档的候选稿）。
  data?: unknown
  constructor(status: number, message: string, data?: unknown) {
    super(message)
    this.status = status
    this.data = data
    if (data && typeof data === 'object' && typeof (data as { code?: unknown }).code === 'string') this.code = (data as { code: string }).code
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

// send 发送请求：FormData 以 multipart/form-data 发送（由浏览器生成 boundary），其余请求体按 JSON 序列化。
async function send(method: string, path: string, body?: unknown): Promise<Response> {
  const headers: Record<string, string> = {}
  const token = getToken()
  if (token) headers.Authorization = `Bearer ${token}`
  const form = body instanceof FormData
  if (body !== undefined && !form) headers['Content-Type'] = 'application/json'
  let resp: Response
  try {
    resp = await fetch(path, { method, headers, body: body === undefined ? undefined : form ? body : JSON.stringify(body) })
  } catch (e) {
    throw new ApiError(0, e instanceof Error ? e.message : '无法连接服务')
  }
  if (!resp.ok) {
    let msg = resp.statusText
    let data: unknown
    try {
      data = await resp.json()
      const j = data as { error?: unknown } | null
      if (j && typeof j.error === 'string') msg = j.error
    } catch {
      /* 非 JSON 错误体 */
    }
    if (resp.status === 401) window.dispatchEvent(new Event('autobid:unauthorized'))
    throw new ApiError(resp.status, msg, data)
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

function formData(fields: Record<string, string | boolean | undefined>, file: File): FormData {
  const fd = new FormData()
  for (const [k, v] of Object.entries(fields)) if (v !== undefined && v !== '') fd.set(k, String(v))
  fd.set('file', file, file.name)
  return fd
}

// blob 下载需要鉴权的二进制资源（产物、图表 PNG），从 Content-Disposition 解析文件名。
async function blob(path: string): Promise<{ blob: Blob; filename: string }> {
  const resp = await send('GET', path)
  const cd = resp.headers.get('Content-Disposition') ?? ''
  let filename = ''
  const star = /filename\*=(?:UTF-8|utf-8)''([^;]+)/.exec(cd)
  if (star) filename = decodeURIComponent(star[1])
  else {
    const plain = /filename="?([^";]+)"?/.exec(cd)
    if (plain) filename = plain[1]
  }
  return { blob: await resp.blob(), filename }
}

// saveBlob 触发浏览器下载。
export function saveBlob(b: Blob, filename: string) {
  const url = URL.createObjectURL(b)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 30000)
}

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
  uploadDocumentFile: (id: number, req: UploadDocumentFileReq) => post<UploadDocumentResp>(`${t(id)}/documents`,
    formData({ tender_no: req.tender_no, lot_no: req.lot_no, title: req.title, wake_planner: req.wake_planner }, req.file)),
  listDocuments: (id: number, content = true) =>
    get<DocumentWithChunks[]>(`${t(id)}/documents${content ? '?content=1' : ''}`),

  // 企业资信库
  listCredentials: (f: { category?: string; q?: string; valid_on?: string; limit?: number }) =>
    get<CompanyCredential[]>(`/api/company/credentials${qs(f)}`),
  createCredential: (req: CredentialReq) =>
    post<(CompanyCredential & { created: boolean })[]>('/api/company/credentials', req),
  updateCredential: (id: number, req: CredentialReq) => put<CompanyCredential>(`/api/company/credentials/${id}`, req),
  deleteCredential: (id: number) => del(`/api/company/credentials/${id}`),

  // 企业方案知识库
  listKnowledge: (f: { category?: string; tags?: string; q?: string; limit?: number }) =>
    get<KnowledgeDocument[]>(`/api/knowledge/documents${qs(f)}`),
  getKnowledge: (id: number, content = true) =>
    get<{ document: KnowledgeDocument; chunks: KnowledgeChunk[] }>(`/api/knowledge/documents/${id}${content ? '?content=1' : ''}`),
  uploadKnowledgeText: (req: KnowledgeTextReq) => post<KnowledgeUploadResp>('/api/knowledge/documents', req),
  uploadKnowledgeFile: (req: KnowledgeFileReq) => post<KnowledgeUploadResp>('/api/knowledge/documents',
    formData({ category: req.category, title: req.title, tags: req.tags?.join(','), extra: req.extra ? JSON.stringify(req.extra) : undefined }, req.file)),
  deleteKnowledge: (id: number) => del(`/api/knowledge/documents/${id}`),
  searchKnowledge: (r: KnowledgeSearchReq) => get<RetrievalResult>(`/api/knowledge/search${qs({
    q: r.q, product: r.product, category: r.category?.join(','), tags: r.tags?.join(','), limit: r.limit, prefer_tables: r.prefer_tables ? 1 : undefined,
  })}`),
  retrievalEvidence: (id: number) => get<RetrievalEvidence[]>(`/api/retrieval/runs/${id}/evidence`),
  taskRetrievalRuns: (id: number) => get<RetrievalRun[]>(`/api/tasks/${id}/retrieval-runs`),
  research: (id: number, req: ResearchRequest) => post<{run_id: number; status: string; gaps: string[]; degraded: string[] | null}>(`/api/tasks/${id}/research`, req),
  cancelResearch: (id: number) => send('POST', `/api/retrieval/runs/${id}/cancel`),
  verifyEvidence: (run: number, id: number, claim: EvidenceClaim) => post<RetrievalEvidence>(`/api/retrieval/runs/${run}/evidence/${id}/verify`, claim),
  knowledgeIndexes: () => get<KnowledgeIndex[]>('/api/knowledge/indexes'),
  buildKnowledgeIndex: (dimension: number) => post<KnowledgeIndex>('/api/knowledge/indexes', { dimension }),
  activateKnowledgeIndex: (id: number) => send('POST', `/api/knowledge/indexes/${id}/activate`),

  // 投标要求清单、技术标大纲审批与章节写作（Phase 5）
  requirements: (id: number) => get<RequirementsView>(`${t(id)}/requirements`),
  extractRequirements: (id: number) => post<{ result: ExtractResult; requirements: RequirementsView }>(`${t(id)}/requirements/extract`),
  createRequirement: (id: number, req: RequirementInput) => post<RequirementItem>(`${t(id)}/requirements`, req),
  updateRequirement: (id: number, reqID: number, req: RequirementInput) => put<RequirementItem>(`${t(id)}/requirements/${reqID}`, req),
  deleteRequirement: (id: number, reqID: number) => del(`${t(id)}/requirements/${reqID}`),
  confirmRequirements: (id: number, epoch?: number) => post<RequirementManifest>(`${t(id)}/requirements/confirm`, { epoch: epoch ?? 0 }),
  outlines: (id: number) => get<OutlineList>(`${t(id)}/outlines`),
  createOutline: (id: number, req: OutlineInput = {}) => post<OutlineDetail>(`${t(id)}/outlines`, req),
  getOutline: (id: number, version: number) => get<OutlineDetail>(`${t(id)}/outlines/${version}`),
  updateOutline: (id: number, version: number, req: OutlineInput) => put<OutlineDetail>(`${t(id)}/outlines/${version}`, req),
  submitOutline: (id: number, version: number) => post<OutlineDetail>(`${t(id)}/outlines/${version}/submit`),
  approveOutline: (id: number, version: number) => post<OutlineDetail>(`${t(id)}/outlines/${version}/approve`),
  section: (id: number, sectionID: number) => get<SectionDetail>(`${t(id)}/sections/${sectionID}`),
  dispatchSection: (id: number, sectionID: number, req: { base_rev_no?: number; instruction?: string }) =>
    post<SectionClaim>(`${t(id)}/sections/${sectionID}/dispatch`, req),
  saveRevision: (id: number, sectionID: number, req: SaveRevisionReq) => post<SaveRevisionResult>(`${t(id)}/sections/${sectionID}/revisions`, req),
  approveRevision: (id: number, sectionID: number, revNo: number) =>
    post<ProposalSection>(`${t(id)}/sections/${sectionID}/revisions/${revNo}/approve`),
  rollbackRevision: (id: number, sectionID: number, revNo: number, baseRevNo?: number) =>
    post<SaveRevisionResult>(`${t(id)}/sections/${sectionID}/revisions/${revNo}/rollback`, baseRevNo === undefined ? {} : { base_rev_no: baseRevNo }),

  // 双层响应核验矩阵、全文一致性核验与导出就绪度（Phase 6A）
  verifyCompliance: (id: number, sectionID?: number) =>
    post<VerifyMatrixResult>(`${t(id)}/compliance/verify`, sectionID ? { section_id: sectionID } : {}),
  complianceMatrix: (id: number, f: { status?: string; kind?: string; section_id?: number } = {}) =>
    get<MatrixView>(`${t(id)}/compliance/matrix${qs(f)}`),
  complianceCoverage: (id: number) => get<ComplianceCoverage>(`${t(id)}/compliance/coverage`),
  runConsistency: (id: number) => post<ConsistencyRunDetail>(`${t(id)}/consistency/runs`),
  consistencyRuns: (id: number) => get<{ runs: ConsistencyRun[] }>(`${t(id)}/consistency/runs`),
  consistencyFindings: (id: number, f: { run_id?: number; status?: string } = {}) =>
    get<ConsistencyRunDetail>(`${t(id)}/consistency/findings${qs(f)}`),
  resolveFinding: (id: number, findingID: number, req: ResolveFindingReq) =>
    post<ConsistencyFinding>(`${t(id)}/consistency/findings/${findingID}/resolve`, req),
  exportReadiness: (id: number) => get<ExportReadiness>(`${t(id)}/export-readiness`),

  // 章节图表与锁定版本导出（Phase 6B）
  charts: (id: number, sectionID?: number) => get<{ charts: ProposalChart[]; renderer: ExportStatus }>(`${t(id)}/charts${qs({ section_id: sectionID })}`),
  generateChart: (id: number, req: ChartReq) => post<ProposalChart>(`${t(id)}/charts`, req),
  renderChart: (id: number, chartID: number) => post<ProposalChart>(`${t(id)}/charts/${chartID}/render`),
  rebindChart: (id: number, chartID: number) => post<ProposalChart>(`${t(id)}/charts/${chartID}/rebind`),
  deleteChart: (id: number, chartID: number) => del(`${t(id)}/charts/${chartID}`),
  chartImage: (id: number, chartID: number) => blob(`${t(id)}/charts/${chartID}/image`),
  exportPreflight: (id: number) => get<ExportPreflight>(`${t(id)}/exports/preflight`),
  createExport: (id: number, req: CreateExportReq) => post<{ jobs: ExportJob[]; warnings: ExportWarning[] }>(`${t(id)}/exports`, req),
  exportJobs: (id: number) => get<{ jobs: ExportJob[] }>(`${t(id)}/exports`),
  downloadExport: (id: number, jobID: number) => blob(`${t(id)}/exports/${jobID}/download`),
  exportTemplates: () => get<{ templates: ExportTemplate[]; default_template_hash: string }>('/api/export/templates'),
  uploadTemplate: (file: File, name: string) => json<ExportTemplate>('POST', '/api/export/templates', formData({ name }, file)),
  deleteTemplate: (templateID: number) => del(`/api/export/templates/${templateID}`),
  defaultTemplate: () => blob('/api/export/templates/default/file'),

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
