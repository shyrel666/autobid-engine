// 与后端 Go 结构体（db/*.go、server/http.go、ingest/splitter.go）一一对应的 JSON 契约。

export interface Task {
  id: number
  name: string
  description?: string
  exploration_id: number
  status: string // running / paused / done / failed / timeout …
  paused: boolean
  queued: boolean
  timeout_seconds: number
  plan_heartbeat_seconds: number
  first_run_at?: string
  deadline_at?: string
  created_at: string
  updated_at: string
}

// server/http.go taskView
export interface TaskView extends Task {
  goal: string
  running: boolean
  settling: boolean
  inflight: number
}

export interface CreateTaskReq {
  name: string
  description?: string
  goal: string
  goals?: string[]
  timeout_seconds?: number
  plan_heartbeat_seconds?: number
  scopes?: { tender_no: string; lot_no?: string }[]
  source_task_ids?: number[]
  llm_profile_ids?: number[]
  start?: boolean
}

export interface Coverage {
  target: number
  tested: number
  pct: number | null
}

export interface NodeBrief {
  id: number
  state?: string
  priority?: number
  owner?: string
  blocked_reason?: string
  summary: string
}

export interface DigestBrief {
  id: number
  title: string
  summary: string
  member_ids: number[]
  round: number
  stale?: boolean
  changed_members?: number[]
}

export interface ColdEntry {
  id: number
  kind: string
  state: string
  summary: string
}

export interface Overview {
  goals: NodeBrief[] | null
  hints?: NodeBrief[] | null
  open_intents: NodeBrief[] | null
  running_intents: NodeBrief[] | null
  paused_intents?: NodeBrief[] | null
  recent_terminal_intents: NodeBrief[] | null
  recent_facts: NodeBrief[] | null
  recent_findings: NodeBrief[] | null
  digests?: DigestBrief[] | null
  cold_index?: ColdEntry[] | null
  folded_nodes?: number
  coverage?: Coverage
}

export interface GraphNode {
  finding_id?: number
  id: number
  exploration_id: number
  kind: string
  payload: Record<string, unknown> | null
  priority: number
  state: string
  origin?: string
  owner?: string
  blocked_reason?: string
  delete_reason?: string
  content_version: number
  active_round: number
  cold_since_round?: number
  lease_until?: string
  lease_epoch?: number
  anchors?: number[]
  created_at: string
  updated_at: string
  completed_at?: string
  source_task_id?: number
  inherited?: boolean
}

// db.Edge 无 json 标签，按 Go 字段名序列化。
export interface GraphEdge {
  From: number
  Rel: string
  To: number
}

export interface NodeDetail {
  node: GraphNode
  edges: GraphEdge[] | null
}

export interface Finding {
  id: number
  task_id: number
  node_id: number
  intent_id?: number
  category: string
  severity: string
  title: string
  summary: string
  payload: Record<string, unknown> | null
  evidence_node_ids: number[]
  created_at: string
}

export interface Asset {
  id: number
  type: string
  tender_no: string
  lot_no?: string
  code?: string
  title?: string
  content?: string
  category?: string
  is_mandatory?: boolean
  score_max?: number
  amount?: string
  currency?: string
  quantity?: string
  unit?: string
  source_ref?: unknown
  extra?: unknown
  task_ids?: number[]
  created_at: string
  last_seen: string
}

export interface EntityEdge {
  id: number
  src_asset_id: number
  rel: string
  dst_asset_id: number
  task_ids?: number[]
  source_node_id?: number
  weight: number
  props?: unknown
  last_seen: string
}

export interface TenderDocument {
  id: number
  task_id: number
  tender_no: string
  lot_no?: string
  title: string
  content_hash: string
  chunk_count: number
  created_at: string
}

export interface DocumentChunk {
  id: number
  doc_id: number
  task_id: number
  seq: number
  level: number
  section_code: string
  section_title: string
  page_no: number
  content?: string
  token_estimate: number
  is_mandatory: boolean
}

export interface DocumentWithChunks extends TenderDocument {
  chunks: DocumentChunk[]
}

export interface UploadDocumentReq {
  tender_no: string
  lot_no?: string
  title?: string
  text: string
  wake_planner?: boolean
}

export interface UploadDocumentResp {
  result: {
    document: TenderDocument
    created: boolean
    chunks: number
    mandatory_chunks: number
    tender_asset_id: number
    lot_asset_id?: number
  }
  planner_notified: boolean
}

export interface CompanyCredential {
  id: number
  category: string
  code?: string
  title: string
  attributes?: Record<string, unknown> | null
  valid_until?: string | null
  created_at: string
  updated_at: string
}

export interface CredentialReq {
  category: string
  code?: string
  title: string
  attributes?: Record<string, unknown>
  valid_until?: string | null
}

export interface LLMProfile {
  id: number
  name: string
  format: string
  base_url?: string
  api_key_env: string
  model: string
  max_tokens?: number
  effort?: string
  thinking_display?: string
  priority: number
  rank: number
  window_tokens?: number
  enabled: boolean
  in_global_pool: boolean
  proxy?: string
  session_header_key?: string
}

export interface LLMHealth {
  profile_id: number
  fails: number
  trips: number
  open: boolean
  open_until: string
  last_error: string
  last_at: string
}

export interface LLMUsage {
  model: string
  calls: number
  input_tokens: number
  output_tokens: number
  cache_read: number
  cache_write: number
}

export interface ChainEntry {
  position: number
  profile_id: number
  status: string
}

export interface LLMChain {
  chain: ChainEntry[] | null
  active_profile_id: number
  revision: number
}

export interface InterceptPending {
  id: number
  rule_id: number
  task_id: number
  intent_id: number
  agent_name: string
  tool_name: string
  tool_input: string
  message: string
  status: string
  effective_action: string
  reason: string
  timeout_action: string
  audit: unknown
  waiter: string
  waiter_until: string | null
  exec_outcome: string
  exec_detail: string
  created_at: string
  decided_at: string | null
}

export interface InterceptAudit {
  task_id: number
  intent_id: number
  agent_name: string
  tool_name: string
  input_digest: string
  source: string
  rule_id: number
  profile_id: number
  action: string
  effective_action: string
  pending_id: number
  reason: string
}

// db.Activity，经 SSE event: activity 推送。
export interface Activity {
  task_id: number
  worker: string
  kind: string // round / text / tool / tool_error / state / error / intercept_request / intercept_decided
  summary: string
  detail?: string
  node_id?: number
  at: string
}

export interface ReportAnchor {
  asset_id: number
  type: string
  lot_no?: string
  code?: string
  title?: string
  is_mandatory?: boolean
  source_ref?: unknown
}

export interface ReportEvidence {
  node_id: number
  kind: string
  state: string
  summary: string
  evidence?: string
  source_ref?: unknown
  owner?: string
  inherited?: boolean
  anchors?: ReportAnchor[]
}

export interface ReportFinding extends Finding {
  source_refs?: unknown[]
  anchors?: ReportAnchor[]
  evidence: ReportEvidence[]
}

export interface ReportElement extends ReportAnchor {
  category?: string
  score_max?: number
  amount?: string
  quantity?: string
  unit?: string
  verified: boolean
  fact_ids?: number[]
  finding_ids?: number[]
}

export interface TaskReport {
  task: Task
  goal: string
  generated_at: string
  coverage: Coverage
  summary: {
    documents: number
    elements: number
    elements_by_type: Record<string, number>
    mandatory_elements: number
    mandatory_verified: number
    confirmed_facts: number
    findings: number
    findings_by_severity: Record<string, number>
    findings_by_category: Record<string, number>
    goals_total: number
    goals_met: number
  }
  documents: TenderDocument[]
  elements: ReportElement[]
  findings: ReportFinding[]
}
