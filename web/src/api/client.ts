// AutoBid 控制面 HTTP 客户端：所有 /api/* 请求携带 Authorization: Bearer <token>；
// SSE（EventSource 无法设置请求头）改用 ?token=。401 时广播 autobid:unauthorized 以弹出令牌设置。

import type { ProcessingFile, ProcessingRecord, ProcessingDetail, TaskMaterial, FixedJobInput, FixedJob } from './processing-types'
import type {
  Activity, Asset, CompanyCredential, CreateTaskReq, CredentialReq, DocumentWithChunks, EntityEdge, Finding,
  InterceptApprovalsResp, InterceptAudit, InterceptConfigResp, InterceptPending, InterceptRule, InterceptRuleInput, JudgeConfig,
  KnowledgeChunk, KnowledgeDocument, KnowledgeFileReq, KnowledgeSearchReq, RetrievalResult, RetrievalEvidence, KnowledgeIndex, EvidenceClaim,
  KnowledgeTextReq, KnowledgeUploadResp, LLMChain, LLMHealth, LLMProfile, LLMUsage, NodeDetail, Overview, Coverage,
  Task, TaskReport, TaskView, UploadDocumentFileReq, UploadDocumentReq, UploadDocumentResp, UploadArchiveResp, ResearchRequest, RetrievalRun,
  ExtractRequirementsResp, RFPGoalDecompResult, OutlineDetail, OutlineInput, OutlineList, ProposalSection, RequirementInput, RequirementItem, RequirementManifest,
  RequirementsView, SaveRevisionReq, SaveRevisionResult, SectionDetail, BidScopeInput, VetoStatus, SectionWriteJob,
  ComplianceCoverage, ConsistencyFinding, ConsistencyRun, ConsistencyRunDetail, ExportReadiness, MatrixView, ResolveFindingReq, VerifyMatrixResult,
  ChartReq, CreateExportReq, ExportJob, ExportPreflight, ExportStatus, ExportTemplate, ExportWarning, ProposalChart,
  RuntimeCapability, RuntimeCapabilityStatus,
  LLMSettings, LLMPoolStatus, LLMProfileInput, LLMTestResult, LLMRecordDetail, LLMRecordsResp, SystemCatalog, AgentRoleKey, TaskGraph,
  CompanyPerformance, EvidenceOriginal, EvidenceBinding, AdmissibilityRun, AdmissibilityDecision,
  Commitment, CommitmentInput, BidQuote, BidQuoteValidation, ObjectiveScoreRun,
  SectionStoryboard, UpsertStoryboardReq, GroundedDraftRenderResult, SectionWritingVerification,
} from './types'

const TOKEN_KEY = 'autobid.token'
const OPERATOR_KEY = 'autobid.operator'

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    return ''
  }
}

export function setOperator(name: string) {
  try {
    if (name) localStorage.setItem(OPERATOR_KEY, name)
    else localStorage.removeItem(OPERATOR_KEY)
  } catch {
    /* 隐私模式等场景下仅本次会话生效 */
  }
}

export function getOperator(): string {
  try {
    return localStorage.getItem(OPERATOR_KEY) ?? ''
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
async function send(method: string, path: string, body?: unknown, extraHeaders?: Record<string, string>): Promise<Response> {
  const headers: Record<string, string> = { ...extraHeaders }
  const token = getToken()
  if (token) headers.Authorization = `Bearer ${token}`
  const operator = getOperator()
  if (operator) headers['X-Autobid-User'] = operator
  const form = body instanceof FormData
  const blob = body instanceof Blob
  if (blob) headers['Content-Type'] = body.type || 'application/octet-stream'
  else if (body !== undefined && !form) headers['Content-Type'] = 'application/json'
  let resp: Response
  try {
    resp = await fetch(path, { method, headers, body: body === undefined ? undefined : form || blob ? body as BodyInit : JSON.stringify(body) })
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

async function json<T>(method: string, path: string, body?: unknown, headers?: Record<string, string>): Promise<T> {
  const resp = await send(method, path, body, headers)
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
  taskGraph: (id: number) => get<TaskGraph>(`${t(id)}/graph`),
  node: (id: number, nodeID: number) => get<NodeDetail>(`${t(id)}/nodes/${nodeID}`),
  findings: (id: number, category?: string) => get<Finding[] | null>(`${t(id)}/findings${qs({ category })}`),
  assets: (id: number, types?: string[]) => get<Asset[] | null>(`${t(id)}/assets${qs({ types: types?.join(',') })}`),
  assetEdges: (assetID: number, dir: 'out' | 'in' = 'out') => get<EntityEdge[] | null>(`/api/assets/${assetID}/edges${qs({ dir })}`),

  // 报告
  report: (id: number) => get<TaskReport>(`${t(id)}/report`),
  reportMarkdown: async (id: number) => (await send('GET', `${t(id)}/report?format=markdown`)).text(),

  // 招标文档
  uploadDocument: (id: number, req: UploadDocumentReq) => post<UploadDocumentResp>(`${t(id)}/documents`, req),
  processingFiles: (id: number, after = 0) => get<{ enabled: boolean; files: ProcessingFile[]; has_more: boolean }>(`${t(id)}/files?after=${after}`),
  retainProcessingFile: (id: number, file: File) => post<ProcessingFile>(`${t(id)}/files?filename=${encodeURIComponent(file.name)}`, file),
  materials: (id: number) => get<TaskMaterial[]>(`${t(id)}/materials`),
  saveMaterial: (id: number, input: Pick<TaskMaterial, 'id' | 'name' | 'required' | 'file_id' | 'version' | 'note'>) => post<TaskMaterial>(`${t(id)}/materials`, input),
  fixedJobs: (id: number, before = 0) => get<FixedJob[]>(`${t(id)}/file-jobs?before=${before}`),
  enqueueFixedJob: (id: number, input: FixedJobInput) => post<FixedJob>(`${t(id)}/file-jobs`, input),
  cancelFixedJob: (id: number, job: number) => post<FixedJob>(`${t(id)}/file-jobs/${job}/cancel`, {}),
  processingRecords: (id: number, before = 0) => get<ProcessingRecord[]>(`${t(id)}/file-processing-records?before=${before}`),
  processingDetail: (id: number, record: number, offset = 0) => get<ProcessingDetail>(`${t(id)}/file-processing-records/${record}?offset=${offset}`),
  importProcessingFile: (id: number, file: number, req: { request_key: string; tender_no: string; lot_no: string; doc_role: string; published_at: string }) =>
    post<ProcessingRecord>(`${t(id)}/files/${file}/import`, req),
  reparseDocument: (id: number, doc: number, expected_revision: number, request_key: string) =>
    post<ProcessingRecord>(`${t(id)}/documents/${doc}/reparse`, { expected_revision, request_key }),
  adoptProcessingRecord: (id: number, record: number, expected_revision: number, review_note: string) =>
    post<ProcessingRecord>(`${t(id)}/file-processing-records/${record}/adopt`, { expected_revision, review_note }),
  uploadDocumentFile: (id: number, req: UploadDocumentFileReq) => post<UploadDocumentResp | UploadArchiveResp>(`${t(id)}/documents`,
    formData({ tender_no: req.tender_no, lot_no: req.lot_no, title: req.title, wake_planner: req.wake_planner,
      bootstrap_goals: req.bootstrap_goals, doc_role: req.doc_role, published_at: req.published_at }, req.file)),
  listDocuments: (id: number, content = true) =>
    get<DocumentWithChunks[]>(`${t(id)}/documents${content ? '?content=1' : ''}`),

  // 企业资信库
  listCredentials: (f: { category?: string; q?: string; valid_on?: string; task_id?: number; limit?: number }) =>
    get<CompanyCredential[]>(`/api/company/credentials${qs(f)}`),
  createCredential: (req: CredentialReq) =>
    post<(CompanyCredential & { created: boolean })[]>('/api/company/credentials', req),
  updateCredential: (id: number, req: CredentialReq) => put<CompanyCredential>(`/api/company/credentials/${id}`, req),
  deleteCredential: (id: number) => del(`/api/company/credentials/${id}`),

  // D2：企业证据、可采性、承诺台账与报价
  listPerformance: (includeDeleted = false) => get<CompanyPerformance[]>(`/api/company/performance${includeDeleted ? '?include_deleted=1' : ''}`),
  listOriginals: (domain: 'credential' | 'performance', ownerId: number) =>
    get<EvidenceOriginal[]>(`/api/company/evidence/${domain}/${ownerId}/originals`),
  listBindings: (taskId: number, reqId: number) => get<EvidenceBinding[]>(`${t(taskId)}/requirements/${reqId}/evidence-bindings`),
  bindEvidence: (taskId: number, reqId: number, evidence_domain: string, evidence_id: number) =>
    post<EvidenceBinding>(`${t(taskId)}/requirements/${reqId}/evidence-bindings`, { evidence_domain, evidence_id }),
  listAdmissibility: (taskId: number, reqId?: number) =>
    get<AdmissibilityRun[]>(`${t(taskId)}/admissibility${qs({ requirement_id: reqId })}`),
  evaluateAdmissibility: (taskId: number, requirementId: number) =>
    post<AdmissibilityRun>(`${t(taskId)}/admissibility/evaluate`, { requirement_id: requirementId }),
  resolveAdmissibility: (taskId: number, runId: number, decisions: AdmissibilityDecision[]) =>
    post<AdmissibilityRun>(`${t(taskId)}/admissibility/${runId}/resolve`, { decisions }),
  listCommitments: (taskId: number) => get<Commitment[]>(`${t(taskId)}/commitments`),
  upsertCommitment: (taskId: number, input: CommitmentInput) =>
    put<Commitment>(`${t(taskId)}/commitments/${encodeURIComponent(input.code)}`, input),
  confirmCommitment: (taskId: number, code: string) =>
    post<Commitment>(`${t(taskId)}/commitments/${encodeURIComponent(code)}/confirm`),
  signOffCommitment: (taskId: number, code: string, note: string) =>
    post<Commitment>(`${t(taskId)}/commitments/${encodeURIComponent(code)}/signoff`, { note }),
  listQuotes: (taskId: number) => get<BidQuote[]>(`${t(taskId)}/quotes`),
  validateQuote: (taskId: number, quoteId: number) => post<BidQuoteValidation>(`${t(taskId)}/quotes/${quoteId}/validate`),
  confirmQuote: (taskId: number, quoteId: number) =>
    post<{ quote: BidQuote; validation: BidQuoteValidation }>(`${t(taskId)}/quotes/${quoteId}/confirm`),
  listObjectiveScores: (taskId: number) => get<ObjectiveScoreRun[]>(`${t(taskId)}/objective-scores`),
  computeObjectiveScores: (taskId: number) => post<ObjectiveScoreRun[]>(`${t(taskId)}/objective-scores/compute`, {}),

  // D3：章节故事板（粉队评审）、受控引用渲染与句子级写作核验
  listStoryboards: (taskId: number) => get<SectionStoryboard[]>(`${t(taskId)}/storyboards`),
  getSectionStoryboard: (taskId: number, sectionId: number) => get<SectionStoryboard>(`${t(taskId)}/sections/${sectionId}/storyboard`),
  upsertSectionStoryboard: (taskId: number, sectionId: number, req: UpsertStoryboardReq) =>
    put<SectionStoryboard>(`${t(taskId)}/sections/${sectionId}/storyboard`, req),
  submitSectionStoryboard: (taskId: number, sectionId: number) =>
    post<SectionStoryboard>(`${t(taskId)}/sections/${sectionId}/storyboard/submit`, {}),
  approveSectionStoryboard: (taskId: number, sectionId: number, reviewNote = '') =>
    post<SectionStoryboard>(`${t(taskId)}/sections/${sectionId}/storyboard/approve`, { review_note: reviewNote }),
  rejectSectionStoryboard: (taskId: number, sectionId: number, reviewNote: string) =>
    post<SectionStoryboard>(`${t(taskId)}/sections/${sectionId}/storyboard/reject`, { review_note: reviewNote }),
  renderSectionGroundedDraft: (taskId: number, sectionId: number, templateMD: string) =>
    post<GroundedDraftRenderResult>(`${t(taskId)}/sections/${sectionId}/storyboard/render`, { template_md: templateMD }),
  listWritingVerifications: (taskId: number) => get<SectionWritingVerification[]>(`${t(taskId)}/writing-verifications`),
  getSectionWritingVerification: (taskId: number, sectionId: number) =>
    get<SectionWritingVerification>(`${t(taskId)}/sections/${sectionId}/writing-verification`),
  evaluateSectionWritingVerification: (taskId: number, sectionId: number) =>
    post<SectionWritingVerification>(`${t(taskId)}/sections/${sectionId}/writing-verification/evaluate`, {}),

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
  extractRequirements: (id: number, initOutline = false) => post<ExtractRequirementsResp>(`${t(id)}/requirements/extract`, { init_outline: initOutline }),
  bootstrapRFPGoals: (id: number, initOutline = true) => post<RFPGoalDecompResult>(`${t(id)}/goals/bootstrap`, { init_outline: initOutline }),
  createRequirement: (id: number, req: RequirementInput) => post<RequirementItem>(`${t(id)}/requirements`, req),
  updateRequirement: (id: number, reqID: number, req: RequirementInput) => put<RequirementItem>(`${t(id)}/requirements/${reqID}`, req),
  deleteRequirement: (id: number, reqID: number) => del(`${t(id)}/requirements/${reqID}`),
  confirmRequirements: (id: number, epoch?: number) => post<RequirementManifest>(`${t(id)}/requirements/confirm`, { epoch: epoch ?? 0 }),
  // D1：否决项核对（通过 / 不通过须写依据）与投标范围（分包、人工截止时间）
  setVetoStatus: (id: number, reqID: number, status: VetoStatus, note: string) => put<RequirementItem>(`${t(id)}/requirements/${reqID}/veto`, { status, note }),
  setBidScope: (id: number, req: BidScopeInput) => put<RequirementsView>(`${t(id)}/requirements/bid-scope`, req),
  outlines: (id: number) => get<OutlineList>(`${t(id)}/outlines`),
  createOutline: (id: number, req: OutlineInput = {}) => post<OutlineDetail>(`${t(id)}/outlines`, req),
  getOutline: (id: number, version: number) => get<OutlineDetail>(`${t(id)}/outlines/${version}`),
  updateOutline: (id: number, version: number, req: OutlineInput) => put<OutlineDetail>(`${t(id)}/outlines/${version}`, req),
  submitOutline: (id: number, version: number) => post<OutlineDetail>(`${t(id)}/outlines/${version}/submit`),
  approveOutline: (id: number, version: number) => post<OutlineDetail>(`${t(id)}/outlines/${version}/approve`),
  section: (id: number, sectionID: number) => get<SectionDetail>(`${t(id)}/sections/${sectionID}`),
  dispatchSection: (id: number, sectionID: number, req: { base_rev_no?: number; instruction?: string }, requestKey: string = crypto.randomUUID()) =>
    json<SectionWriteJob>('POST', `${t(id)}/sections/${sectionID}/dispatch`, req, { 'Idempotency-Key': requestKey }),
  sectionJobs: (id: number) => get<SectionWriteJob[]>(`${t(id)}/section-jobs`),
  cancelSectionJob: (id: number, jobId: number) => post(`${t(id)}/section-jobs/${jobId}/cancel`),
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
  runtimeTools: (capability?: RuntimeCapability) =>
    get<{ capabilities: RuntimeCapabilityStatus[]; now: string }>(`/api/runtime/tools${qs({ capability })}`),
  uploadTemplate: (file: File, name: string) => json<ExportTemplate>('POST', '/api/export/templates', formData({ name }, file)),
  deleteTemplate: (templateID: number) => del(`/api/export/templates/${templateID}`),
  defaultTemplate: () => blob('/api/export/templates/default/file'),

  // LLM 配置
  listProfiles: () => get<LLMProfile[] | null>('/api/llm/profiles'),
  createProfile: (p: LLMProfileInput) => post<LLMProfile>('/api/llm/profiles', p),
  updateProfile: (id: number, p: LLMProfileInput) => put<LLMProfile>(`/api/llm/profiles/${id}`, p),
  activateProfile: (id: number) => post<{ ok: boolean }>('/api/llm/profiles/active', { id }),
  llmSettings: () => get<LLMSettings>('/api/llm/settings'),
  saveLLMSettings: (s: LLMSettings) => put<LLMSettings>('/api/llm/settings', s),
  llmPool: () => get<LLMPoolStatus>('/api/llm/pool'),
  resetLLMPool: (id = 0) => post<LLMPoolStatus>('/api/llm/pool/reset', { id }),
  llmBindings: () => get<Record<AgentRoleKey, number>>('/api/llm/bindings'),
  testLLM: (p: LLMProfileInput) => post<LLMTestResult>('/api/llm/test', p),
  discoverModels: (p: LLMProfileInput) => post<{ ok: boolean; models?: string[]; error?: string }>('/api/llm/models', p),
  deleteProfile: (id: number) => del(`/api/llm/profiles/${id}`),
  bindAgent: (agent: AgentRoleKey, profileID: number) =>
    put<{ agent: string; profile_id: number }>(`/api/llm/bindings/${agent}`, { profile_id: profileID }),
  llmUsage: (id: number) => get<LLMUsage[]>(`${t(id)}/llm-usage`),
  llmChain: (id: number) => get<LLMChain>(`${t(id)}/llm-chain`),
  setLLMChain: (id: number, profileIDs: number[], activeProfileID?: number) =>
    put<LLMChain>(`${t(id)}/llm-chain`, { profile_ids: profileIDs, active_profile_id: activeProfileID ?? 0 }),
  resetLLMChain: (id: number) => post<LLMChain>(`${t(id)}/llm-chain/reset`),
  llmRecords: (f: { task_id?: number; session_id?: string; profile_name?: string; limit?: number; offset?: number } = {}) =>
    get<LLMRecordsResp>(`/api/llm/records${qs(f)}`),
  llmRecordDetail: (recordID: number) => get<LLMRecordDetail>(`/api/llm/records/${recordID}`),
  deleteLLMRecords: (taskID?: number) => json<{ deleted: number }>('DELETE', `/api/llm/records${qs({ task_id: taskID })}`),
  systemCatalog: () => get<SystemCatalog>('/api/system/catalog'),

  // 拦截审批与控制面
  listIntercepts: (taskID?: number) => get<InterceptPending[]>(`/api/intercepts${qs({ task_id: taskID })}`),
  decideIntercept: (pendingID: number, allow: boolean, reason: string) =>
    post<InterceptPending>(`/api/intercepts/${pendingID}/decide`, { allow, reason }),
  interceptAudit: (id: number) => get<InterceptAudit[] | null>(`${t(id)}/intercept-audit`),
  interceptRules: () => get<InterceptRule[]>('/api/intercept/rules'),
  createInterceptRule: (r: Partial<InterceptRuleInput>) => post<InterceptRule>('/api/intercept/rules', r),
  updateInterceptRule: (id: number, r: Partial<InterceptRuleInput>) => put<InterceptRule>(`/api/intercept/rules/${id}`, r),
  toggleInterceptRule: (id: number, enabled: boolean) => put<{ id: number; enabled: boolean }>(`/api/intercept/rules/${id}`, { enabled }),
  deleteInterceptRule: (id: number) => del(`/api/intercept/rules/${id}`),
  interceptConfig: () => get<InterceptConfigResp>('/api/intercept/config'),
  saveInterceptConfig: (req: { enabled_tools?: string[]; reset_tools?: boolean; judge?: JudgeConfig }) =>
    put<InterceptConfigResp>('/api/intercept/config', req),
  interceptApprovals: (f: { task_id?: number; status?: string; limit?: number; offset?: number } = {}) =>
    get<InterceptApprovalsResp>(`/api/intercept/approvals${qs(f)}`),
}

export function eventsURL(taskID: number): string {
  const token = getToken()
  return `${t(taskID)}/events${token ? `?token=${encodeURIComponent(token)}` : ''}`
}

export type { Activity }
