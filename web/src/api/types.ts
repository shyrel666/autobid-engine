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

// 解析元数据（tender_documents.parse_meta / knowledge_documents.parse_meta，对应 ingest.ParseMeta）
export interface ParseMeta {
  format: string
  parser: string
  pages: number
  lines: number
  tables: number
  styled_headings?: number
  encoding?: string
  delimiter?: string
  parser_version?: string
  sheets?: SheetCoverage[]
  ocr?: string
  ocr_pages?: number[]
  warnings?: string[]
}

/** 表格类文档的工作表覆盖状态；无物理页码。 */
export interface SheetCoverage {
  name: string
  hidden?: boolean
  rows: number
  cols: number
  windows?: number
  empty?: boolean
  note?: string
}

/** 表格来源定位；kind=spreadsheet 时没有物理页码。 */
export interface SourceLocator {
  kind: 'page' | 'spreadsheet'
  file_id?: number
  sha256?: string
  sheet?: string
  range?: string
  row?: number
  column?: string
}

// 还原的结构化表格（ingest.Table）：cells 为合并区域左上角原始单元格，grid 为展开网格。
export interface TableCell {
  row: number
  col: number
  row_span: number
  col_span: number
  text: string
  header?: boolean
  page: number
  raw?: string
  display?: string
  formula?: string
  formula_cached?: boolean
  hidden?: boolean
}

export interface ParsedTable {
  rows: number
  cols: number
  header_rows: number
  page_start: number
  page_end: number
  source: string
  mandatory?: boolean
  cells: TableCell[]
  grid: string[][]
  markdown: string
  sheet?: string
  sheet_index?: number
  sheet_hidden?: boolean
  range?: string
  merges?: string[]
  hidden_rows?: number[]
  hidden_cols?: string[]
  page_is_physical?: boolean
  source_locator?: SourceLocator
}

export interface BBox { page: number; x0: number; y0: number; x1: number; y1: number; confidence?: number }

export interface BBoxMeta {
  source?: string
  block_start?: number
  block_end?: number
  boxes?: BBox[]
  page_is_physical?: boolean
  source_locator?: SourceLocator
}

export interface TenderDocument {
  id: number
  tenant_id?: string
  task_id: number
  tender_no: string
  lot_no?: string
  title: string
  file_name?: string
  mime_type?: string
  page_count?: number
  parse_meta?: ParseMeta | null
  content_hash: string
  chunk_count: number
  created_by?: string
  created_at: string
  /** 招标包内的文件角色；后发布的更正 / 澄清优先于原招标文件。 */
  doc_role: 'tender' | 'correction' | 'clarification' | 'qa' | 'attachment'
  published_at?: string
  /** F1 原件登记；旧数据没有原件时为空。parse_revision 为重新解析修订的乐观锁版本。 */
  original_file_id?: number
  parse_revision?: number
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
  page_start: number
  page_end: number
  content?: string
  token_estimate: number
  is_mandatory: boolean
  table_count: number
  tables?: ParsedTable[]
  bbox_meta?: BBoxMeta
  /** 为 false 时 page_no/page_start 只是表格行窗口的兼容占位值，不得展示为页码。 */
  page_is_physical: boolean
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
  bootstrap_goals?: boolean
  /** 招标包内的文件角色（空时按标题推断）与公告发布时间（2006-01-02），决定效力先后。 */
  doc_role?: string
  published_at?: string
}

export interface UploadDocumentResp {
  result: {
    document: TenderDocument
    created: boolean
    chunks: number
    mandatory_chunks: number
    tables: number
    page_count: number
    format: string
    warnings?: string[]
    tender_asset_id: number
    lot_asset_id?: number
  }
  planner_notified: boolean
  goals_bootstrapped?: RFPGoalDecompResult
}

export interface ArchiveEntry {
  path: string
  size: number
  compressed_size: number
  ratio?: number
  sha256: string
  format: string
  media_type?: string
  file_id?: number
}

/** ZIP 招标包只登记原件和子文件，不自动变成正式文档。 */
export interface UploadArchiveResp {
  result: {
    original_file_id: number
    archive: { entries: ArchiveEntry[]; total_bytes: number; warnings?: string[] }
    child_file_ids: number[]
    note: string
  }
  planner_notified: boolean
}

// 多格式文件上传（multipart/form-data）。
export interface UploadDocumentFileReq {
  tender_no: string
  lot_no?: string
  title?: string
  wake_planner?: boolean
  bootstrap_goals?: boolean
  /** 招标包内的文件角色（空时按标题推断）与公告发布时间（2006-01-02），决定效力先后。 */
  doc_role?: string
  published_at?: string
  file: File
}

// ---------------------------------------------------------------------------
// 企业方案知识库（/api/knowledge/*）
// ---------------------------------------------------------------------------

export type KnowledgeCategory = 'historical_bid' | 'whitepaper' | 'product_spec' | 'standard_solution'

export interface KnowledgeDocument {
  id: number
  tenant_id: string
  category: KnowledgeCategory
  title: string
  tags: string[]
  file_name?: string
  mime_type?: string
  page_count: number
  content_hash: string
  parse_meta?: ParseMeta | null
  extra?: Record<string, unknown> | null
  chunk_count: number
  table_count: number
  created_by?: string
  updated_by?: string
  created_at: string
  updated_at: string
}

export interface KnowledgeChunk {
  id: number
  doc_id: number
  seq: number
  level: number
  section_code: string
  section_title: string
  section_path: string
  page_start: number
  page_end: number
  content?: string
  token_estimate: number
  is_mandatory: boolean
  table_count: number
  tables?: ParsedTable[]
  bbox_meta?: BBoxMeta
}

export interface KnowledgeCitation {
  doc_id: number
  doc_title: string
  category: KnowledgeCategory
  file_name?: string
  chunk_id: number
  seq: number
  section_code: string
  section_title: string
  section_path: string
  page_start: number
  page_end: number
}

export interface KnowledgeHit {
  chunk: KnowledgeChunk
  citation: KnowledgeCitation
  tags: string[]
  score: number
  fts_rank: number
  vector_score: number
  keyword_score: number
  matched_terms: string[]
}

export interface KnowledgeUploadResp {
  document: KnowledgeDocument
  created: boolean
  chunks: number
  tables: number
  page_count: number
  format: string
  warnings?: string[]
}

export interface KnowledgeTextReq {
  category: KnowledgeCategory
  title?: string
  tags?: string[]
  extra?: Record<string, unknown>
  text: string
  file_name?: string
}

export interface KnowledgeFileReq {
  category: KnowledgeCategory
  title?: string
  tags?: string[]
  extra?: Record<string, unknown>
  file: File
}

export interface KnowledgeSearchReq {
  q: string
  product?: string
  category?: KnowledgeCategory[]
  tags?: string[]
  limit?: number
  prefer_tables?: boolean
}

export interface CompanyCredential {
  id: number
  tenant_id?: string
  category: string
  code?: string
  title: string
  subject_name?: string
  subject_type?: string
  certificate_no?: string
  issuer?: string
  issued_on?: string | null
  scope?: string
  attributes?: Record<string, unknown> | null
  valid_until?: string | null
  created_at: string
  updated_at: string
}

export interface CredentialReq {
  category: string
  code?: string
  title: string
  subject_name?: string
  subject_type?: string
  certificate_no?: string
  issuer?: string
  issued_on?: string | null
  scope?: string
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
  is_default?: boolean
  key_configured?: boolean
  api_key_hint?: string
  thinking_type?: string
  streaming?: boolean
  max_tokens_field?: string
  rate_per_second?: number
  rate_per_minute?: number
  retry?: LLMRetryOverride
}

export interface LLMRetryRule { attempts: number; interval_ms: number }
export interface LLMRetryOverride { connect: LLMRetryRule; empty: LLMRetryRule; stream: LLMRetryRule }
export interface LLMRetryPolicy extends LLMRetryOverride { breaker: LLMRetryRule; intent: LLMRetryRule }
export interface LLMSettings { pool_enabled: boolean; bind_fallback: boolean; record_bodies?: boolean; retry: LLMRetryPolicy }

export interface LLMRecordSummary {
  id: number
  task_id: number
  exploration_id: number
  session_id: string
  worker: string
  profile_id: number
  profile_name: string
  method: string
  url: string
  status_code: number
  latency_ms: number
  truncated: boolean
  error: string
  created_at: string
}

export interface LLMRecordDetail extends LLMRecordSummary {
  request_body: string
  response_body: string
}

export interface LLMRecordsResp {
  items: LLMRecordSummary[]
  total: number
}

export type AgentRoleKey = 'goals' | 'planner' | 'worker' | 'watchdog' | 'judge'

export interface CatalogAgent {
  key: AgentRoleKey
  name: string
  description: string
  bound_profile_id: number
}

export interface CatalogSkill {
  Name: string
  Description: string
  Body?: string
  Prompt?: string
  Tools: string[]
}

export interface CatalogTool {
  name: string
  description: string
  category: 'resident' | 'deferred' | 'system'
  unlocked_by?: string
  input_schema?: Record<string, unknown>
}

export interface SystemCatalog {
  agents: CatalogAgent[]
  skills: CatalogSkill[]
  tools: CatalogTool[]
}

export interface TaskGraph {
  nodes: GraphNode[] | null
  edges: GraphEdge[] | null
}
export interface LLMPoolMember {
  profile_id: number; name: string; model: string; active: boolean; priority: number; excluded: boolean
  order: number; state: 'untested' | 'ok' | 'degraded' | 'tripped' | 'missing_key' | 'disabled'
  fails: number; trips: number; cooldown_secs: number; last_error: string
}
export interface LLMPoolStatus { enabled: boolean; bind_fallback: boolean; chain: LLMPoolMember[] }
export type LLMProfileInput = Partial<LLMProfile> & { api_key?: string; clear_api_key?: boolean; profile_id?: number }
export interface LLMTestResult { ok: boolean; latency_ms?: number; model?: string; reply?: string; error?: string }

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
  active_profile_id: number | null
  revision: number
  failover_state?: string
  failover_reason?: string
  reopened_intents?: number
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

export type InterceptAction = 'allow' | 'deny' | 'ask'

export interface InterceptRule {
  id: number
  name: string
  tool_filter: string
  match_target: 'tool_name' | 'tool_input'
  match_type: 'string' | 'regex'
  pattern: string
  action: InterceptAction
  message: string
  priority: number
  enabled: boolean
  timeout_enabled: boolean
  timeout_seconds: number
  timeout_action: 'allow' | 'deny'
}

export type InterceptRuleInput = Omit<InterceptRule, 'id'>

export interface JudgeConfig {
  enabled: boolean
  profile_id: number
  prompt: string
  timeout_seconds: number
  fail_action: InterceptAction
  ask_timeout_seconds: number
  ask_timeout_action: 'allow' | 'deny'
}

export interface JudgeDayUsage {
  date: string
  calls: number
  input_tokens: number
  output_tokens: number
}

export interface JudgeUsageStats {
  calls: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  daily: JudgeDayUsage[]
}

export interface InterceptConfigResp {
  enabled_tools: string[]
  default_tools: string[]
  judge: JudgeConfig
  default_prompt: string
  judge_usage: JudgeUsageStats
}

export interface InterceptApprovalRow extends InterceptPending {
  rule_name?: string
  decision_source?: 'rule' | 'model' | string
}

export interface InterceptApprovalsResp {
  items: InterceptApprovalRow[]
  total: number
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
export interface RetrievalResult {
  total: number; hits: KnowledgeHit[]; run_id: number; evidence_ids: number[] | null
  degraded: string[]; index_id: number; strategy: string; cached: boolean
}
export interface EvidenceClaim {
  text: string; parameter: string; operator: string; value: number; unit: string
  product?: string; version?: string; as_of?: string
}
export interface RetrievalEvidence {
  id: number; run_id: number; status: string; relation: string; reason: string; content_hash: string
  /** 证据来源域（knowledge / tender / credential …），不同域的快照结构不同。 */
  source_domain?: string
  snapshot: { document?: KnowledgeDocument; chunk?: KnowledgeChunk; page_reliability?: string; [k: string]: unknown }
  claim: { verdict?: { quote?: string; table?: number; row?: number; column?: number } }
}
export interface KnowledgeIndex {
  id: number; model: string; dimension: number; preprocessing: string; epoch: number
  status: string; active: boolean; error: string
}
export interface ResearchRequest {
 request_key: string; question: string; multi_agent?: boolean
 query?: { must_terms?: string[] }; claim?: EvidenceClaim
}
export interface RetrievalRun {
 id: number; status: string; kind: string; request: ResearchRequest
 used_calls: number; max_calls: number; reserved_tokens: number; max_tokens: number; deadline: string
}

// ---------------------------------------------------------------------------
// Phase 5：要求清单、技术标大纲与章节修订（db/requirements.go、db/proposal.go、server/proposal.go）
// ---------------------------------------------------------------------------
export type RequirementCategory = 'scoring_item' | 'mandatory_clause' | 'non_scoring_required' | 'format_attachment' | 'veto_item'
/** 否决项来源：资格性审查表 / 符合性审查表 / 87 号令法定无效情形。 */
export type VetoKind = '' | 'qualification' | 'conformity' | 'statutory'
/** 否决项核对结论：待核对与不通过都阻断导出。 */
export type VetoStatus = 'pending' | 'passed' | 'failed'
/** D1：原子要求类型与主客观属性。 */
export type RequirementAtomType = 'scoring_item' | 'scoring_point' | 'demand_item' | 'format_rule' | 'veto_item'
export type EvalKind = '' | 'objective' | 'subjective' | 'mixed'
export type BidDecision = 'pending' | 'bid' | 'no_bid'

export interface ScoringTier {
  seq: number
  label: string
  kind: 'tier' | 'point' | 'deduction' | 'formula'
  eval_kind?: EvalKind
  score_min?: number
  score_max?: number
  criteria: string
  proof_hint?: string
}

export interface ProofRules {
  required_docs?: string[]
  contract_pages?: string[]
  need_acceptance?: boolean
  date_anchor?: string
  years_window?: number
  since_date?: string
  valid_on_bid_deadline?: boolean
  social_security_months?: number
  bidder_only?: boolean
  min_count?: number
  raw_summary?: string
}

export interface BidEvaluation {
  decision: BidDecision; decision_note: string; decided_by: string; decided_at?: string
  veto_gate: 'passed' | 'pending' | 'blocked'
  total_score: number; price_score: number; objective_score: number; subjective_score: number
  evidence_backed_score: number; mandatory_count: number; mandatory_with_evidence: number
  superseded_count: number; recommendation: 'bid' | 'caution' | 'no_bid'
  gaps: string[]
}

export interface RequirementItem {
  id: number; tenant_id: string; task_id: number; code: string; category: RequirementCategory
  atom_type: RequirementAtomType; parent_id?: number
  title: string; description: string; is_mandatory: boolean; score_weight: number; score_min: number
  score_group: ScoreGroup; eval_kind: EvalKind
  scoring_tiers: ScoringTier[]; proof_rules?: ProofRules; linked_requirement_ids: number[]
  superseded_by_id?: number; superseded_by_code?: string
  origin: 'extracted' | 'manual'
  source_asset_id?: number; source_doc_id?: number; source_chunk_id?: number; source_hash: string
  source_loc: { doc_id?: number; chunk_id?: number; section_code?: string; section_title?: string; page_start?: number; page_end?: number; doc_title?: string; kind?: 'page' | 'spreadsheet'; file_id?: number; sha256?: string; sheet?: string; range?: string; table?: number; row?: number; covered_by?: string[]; [k: string]: unknown }
  evidence_run_id?: number; evidence_ids: number[]; evidence_gap: string
  review_status: 'draft' | 'confirmed' | 'needs_revision' | 'stale'
  created_by: string; updated_by: string; approved_by: string; created_at: string; updated_at: string
  mapped_section_codes: string[]
  veto_kind: VetoKind; veto_status: VetoStatus; veto_note: string; veto_checked_by: string; veto_checked_at?: string
  /** 所属分包（空串为不分包）；out_of_bid 表示不在本次投标分包内。 */
  lot: string; out_of_bid: boolean
}
export interface RequirementManifest {
  tenant_id: string; task_id: number; epoch: number; status: 'draft' | 'confirmed' | 'stale'
  source_epoch: number; extracted_source_epoch: number
  confirmed_at?: string; approved_by: string; updated_by: string; updated_at: string
  /** 投标截止时间；bid_deadline_source.origin 为 extracted（附原文位置）或 manual。 */
  bid_deadline?: string
  bid_deadline_source: { origin?: 'extracted' | 'manual'; text?: string; page?: number; doc_role?: string; by?: string; [k: string]: unknown }
  /** 本次投标分包，空数组表示全部分包。 */
  bid_lots: string[]
  /** D1：Bid / No-Bid 决策与留痕。 */
  bid_decision: BidDecision; bid_decision_note: string; bid_decided_by: string; bid_decided_at?: string
}
export interface VetoStats { total: number; passed: number; failed: number; pending: number }
export interface RequirementStats {
  total: number; by_category: Record<string, number>; mandatory: number; confirmed: number; needs_revision: number
  superseded: number
  with_evidence: number; evidence_gaps: number; no_evidence: number; outline_version: number; mapped: number; unmapped: string[]
  out_of_scope: number
  out_of_bid: number; veto: VetoStats; lots: string[]
}
/** PUT /api/tasks/{id}/requirements/bid-scope：未给出的字段不变；bid_deadline 为空串时清除人工值。 */
export interface BidScopeInput {
  bid_lots?: string[]; bid_deadline?: string
  bid_decision?: BidDecision; bid_decision_note?: string
}
/** 评分项分组：technical 技术 / commercial 商务 / price 价格（由报价文件响应，不进入技术标大纲）；空串为未分类。 */
export type ScoreGroup = '' | 'technical' | 'commercial' | 'price'
export interface ScoringGroupSummary { group: string; score_group: ScoreGroup; declared: number; extracted: number; items: number }
/** 按分包汇总的评分分值；warnings 提示合计不是 100 分或与类别声明分值不符。 */
export interface ScoringSummary {
  lot: string; items: number; total: number; technical: number; commercial: number; price: number; unclassified: number
  groups: ScoringGroupSummary[]; warnings: string[]
}
export interface RequirementsView {
  manifest: RequirementManifest; items: RequirementItem[]; stats: RequirementStats; scoring: ScoringSummary[]
  bid_evaluation: BidEvaluation
}
export interface ExtractResult {
  created: number; updated: number; unchanged: number; skipped_manual: number; scanned_chunks: number; codes: string[]
  scoring_items: number; orphaned: number; orphan_codes: string[]
  veto_items: number; superseded: number; bid_deadline?: string
}
export interface IntentMandate {
  allowed_section_ids?: number[]
  allowed_requirement_ids?: number[]
  target_words_min?: number
  target_words_max?: number
  max_tool_calls?: number
  forbidden_tools?: string[]
  require_writeback?: boolean
}
export interface RFPGoalBootstrap {
  code: string; node_id: number; title: string; summary: string; anchors?: number[]; created: boolean
}
export interface RFPGoalDecompResult {
  extract: ExtractResult; synced_assets: number; goals: RFPGoalBootstrap[]
  outline_id?: number; outline_version?: number; outline_created: boolean
}
export interface ExtractRequirementsResp {
  result: ExtractResult; requirements: RequirementsView
  synced_assets?: number; goals?: RFPGoalBootstrap[]
  outline_id?: number; outline_version?: number; outline_created?: boolean
}
export interface RequirementInput {
  code?: string; category?: RequirementCategory; atom_type?: RequirementAtomType; parent_id?: number
  title?: string; description?: string; is_mandatory?: boolean; score_weight?: number; score_min?: number
  score_group?: ScoreGroup; eval_kind?: EvalKind; scoring_tiers?: ScoringTier[]; proof_rules?: ProofRules
  linked_requirement_ids?: number[]; superseded_by_id?: number
  source_chunk_id?: number; evidence_run_id?: number; evidence_ids?: number[]; evidence_gap?: string; review_status?: string
  veto_kind?: VetoKind; lot?: string
}

export type OutlineStatus = 'draft' | 'pending_approval' | 'approved' | 'superseded'
export interface ProposalOutline {
  id: number; task_id: number; version: number; requirements_epoch: number; status: OutlineStatus
  derived_from_version?: number; note: string; submitted_by: string; submitted_at?: string
  approved_by: string; approved_at?: string; superseded_at?: string; created_by: string; updated_by: string; created_at: string; updated_at: string
}
export interface ProposalSection {
  id: number; task_id: number; outline_id: number; outline_version: number; section_code: string; title: string
  level: number; seq: number; writing_guide: string; target_words: number; depends_on_section_ids: number[]
  derived_from_section_id?: number; current_rev_no: number; approved_rev_no?: number; status: string
  lease_token: number; lease_until?: string; created_by: string; updated_by: string; approved_by: string; updated_at: string
  requirement_item_ids: number[]; depends_on: string[]; unmet_dependencies: string[]; dependencies_ready: boolean; writing: boolean
}
export interface RequirementRef { id: number; code: string; title: string; category: string; is_mandatory: boolean }
export interface OutlineDetail {
  outline: ProposalOutline; sections: ProposalSection[]
  coverage: { total: number; mapped: number; unmapped: RequirementRef[]; out_of_scope: number }
  manifest: RequirementManifest; approved_version: number; requirements_epoch_current: boolean
}
export interface OutlineList { outlines: ProposalOutline[]; approved_version: number; open_version: number; manifest: RequirementManifest }
export interface SectionInput {
  section_code: string; title: string; level?: number; writing_guide?: string; target_words?: number
  requirement_item_ids?: number[]; depends_on?: string[]
}
export interface OutlineInput { note?: string; from_version?: number; sections?: SectionInput[] }

export interface RevisionGap { evidence_id?: number; requirement_id?: number; kind: string; note: string }
export interface SectionRevision {
  id: number; section_id: number; rev_no: number; base_rev_no: number; based_on_outline_version: number
  source: 'ai_initial' | 'ai_rewrite' | 'human_edit' | 'rollback'; rollback_of_rev_no?: number
  content_md: string; summary_note: string; diff_summary: string; evidence_ids: number[]; gaps: RevisionGap[]
  status: string; created_by: string; approved_by: string; approved_at?: string; created_at: string
}
export interface EvidenceState {
  id: number; run_id: number; source_domain: string; source_id: number; doc_id: number
  relation: string; status: string; reason: string; usable: boolean; missing?: boolean
}
export interface DependencyState { id: number; section_code: string; title: string; current_rev_no: number; status: string; ready: boolean; summary_note?: string }
export interface SectionDetail {
  section: ProposalSection; outline: ProposalOutline; approved_version: number; writable: boolean; blocked_reason?: string
  requirements: RequirementItem[]; dependencies: DependencyState[]; current_revision?: SectionRevision
  revisions: SectionRevision[]; evidence: EvidenceState[]; warnings: string[]
  storyboard?: SectionStoryboard; writing_verification?: SectionWritingVerification
}
export interface SectionClaim {
  task_id: number; section_id: number; section_code: string; lease_token: number; lease_until: string
  base_rev_no: number; based_on_outline_version: number
}
export interface SectionWriteJob extends SectionClaim {
  job_id: number; request_key: string; instruction: string; status: string; attempts: number
  max_attempts: number; deadline_at: string; error?: string; result_rev_no?: number; created_by: string
}
export interface SaveRevisionResult {
  outcome: 'applied' | 'stale_outline' | 'conflict_candidate'; revision: SectionRevision; section: ProposalSection; reason?: string
}
export interface SaveRevisionReq {
  base_rev_no: number; based_on_outline_version: number; content_md: string; summary_note?: string
  diff_summary?: string; evidence_ids?: number[]; gaps?: RevisionGap[]
}

// ---------------------------------------------------------------------------
// Phase 6A：双层响应核验矩阵、全文一致性核验与导出就绪度（db/compliance.go、db/consistency.go、db/export_readiness.go、server/compliance.go）
// ---------------------------------------------------------------------------
export type VerifyStatus = 'pending' | 'verified' | 'stale' | 'failed'
export type Deviation = 'none' | 'positive' | 'negative'
export type RequirementKind = 'star' | 'triangle' | 'scoring' | 'required' | 'format'

export interface ComplianceCheck { name: string; passed: boolean; detail?: string }
export interface ComplianceRow {
  id: number; task_id: number; outline_id: number; outline_version: number
  requirement_item_id: number; requirement_code: string; requirement_title: string; category: RequirementCategory; kind: RequirementKind; is_mandatory: boolean
  section_id: number; section_code: string; section_title: string; section_rev_id?: number; rev_no?: number; current_rev_no: number
  anchor: string; anchor_excerpt: string; response_summary: string; deviation: Deviation; verify_status: VerifyStatus
  failure_reason: string; stale_reason: string; evidence_ids: number[]; checks: ComplianceCheck[] | null; verifier: 'rule' | 'llm'
  verified_at?: string; created_by: string; updated_by: string; updated_at: string
}
export interface MatrixView { outline_version: number; approved: boolean; rows: ComplianceRow[] }
export interface VerifyMatrixResult {
  outline_version: number; section_id?: number; checked: number; verified: number; failed: number; pending: number; stale: number; rows: ComplianceRow[]
}
export type RequirementComplianceStatus = VerifyStatus | 'unmapped' | 'unchecked'
export interface RequirementCompliance {
  id: number; code: string; title: string; category: RequirementCategory; kind: RequirementKind; is_mandatory: boolean
  mapped: boolean; section_codes: string[]; status: RequirementComplianceStatus; deviation: Deviation; reason?: string
}
export interface KindCoverage { kind: RequirementKind; total: number; mapped: number; verified: number; failed: number; stale: number; pending: number; unchecked: number }
export interface ComplianceCoverage {
  outline_version: number; approved: boolean; total: number; mapped: number; mapping_rate: number; verified: number; response_rate: number
  negative_deviations: number; by_kind: KindCoverage[]; requirements: RequirementCompliance[]
}

export type FindingStatus = 'open' | 'resolved' | 'stale'
export type ResolutionKind = 'fixed' | 'accepted' | 'false_positive'
export interface ConsistencyRun {
  id: number; task_id: number; outline_id: number; outline_version: number; snapshot: Record<string, number>
  status: 'completed' | 'stale'; stale_reason: string; contradictions: number; omissions: number
  summary: { sections?: number; sections_with_text?: number; mentions?: number; by_metric?: Record<string, number> }
  created_by: string; created_at: string; updated_at: string
}
export interface ConsistencyFinding {
  id: number; run_id: number; kind: 'contradiction' | 'omission'; metric: string; metric_label: string
  requirement_item_id?: number; requirement_code?: string; requirement_title?: string; requirement_kind?: RequirementKind
  section_ids: number[]
  a_section_id?: number; a_section_code?: string; a_section_title?: string; a_rev_no?: number; a_value: string; a_excerpt: string
  b_section_id?: number; b_section_code?: string; b_section_title?: string; b_rev_no?: number; b_value: string; b_excerpt: string
  detail: string; status: FindingStatus; stale_reason: string; resolution_kind: '' | ResolutionKind; resolution_note: string
  resolved_by: string; resolved_at?: string; version: number; created_at: string; updated_at: string
}
export interface ConsistencyRunDetail { run?: ConsistencyRun | null; findings: ConsistencyFinding[]; current: boolean }
export interface ResolveFindingReq { version: number; resolution_kind: ResolutionKind; note?: string }

export interface ExportBlocker {
  kind: string; message: string; section_id?: number; section_code?: string
  requirement_item_id?: number; requirement_code?: string; finding_id?: number; evidence_id?: number
}
export interface ExportReadiness {
  ready: boolean; outline_version: number; consistency_run_id?: number; checked_at: string
  counts: Record<string, number>; blockers: ExportBlocker[]
}

// ---------------------------------------------------------------------------
// Phase 6B：章节图表与锁定版本导出（server/export.go）
// ---------------------------------------------------------------------------

export type ChartType = 'architecture' | 'gantt' | 'org' | 'equipment_table'
export type ChartRenderStatus = 'pending' | 'rendered' | 'unrendered' | 'failed'

export interface ProposalChart {
  id: number; task_id: number; section_id: number; section_code: string; section_title: string
  section_rev_id: number; rev_no: number; current_rev_no: number; stale: boolean; stale_reason?: string
  chart_type: ChartType; title: string; spec: unknown; source_format: 'mermaid' | 'json'; source: string; source_hash: string
  render_status: ChartRenderStatus; render_format: '' | 'png' | 'native'; render_error: string
  artifact_hash: string; artifact_size: number; width_px: number; height_px: number; rendered_at?: string
  created_by: string; updated_by: string; created_at: string; updated_at: string
}

export interface ChartReq { section_id: number; chart_type: ChartType; title: string; spec: unknown }

export interface ExportStatus {
  office: boolean; office_version?: string; office_path?: string; office_reason?: string
  renderer: boolean; renderer_name?: string; renderer_reason?: string
}

export interface ExportWarning { kind: string; message: string; section_id?: number; chart_id?: number }

export interface ExportTemplate {
  id: number; name: string; file_name: string; sha256: string; size: number
  meta: { page_width_mm?: number; page_height_mm?: number; margins_mm?: number[]; has_content_placeholder?: boolean; missing_styles?: string[] }
  created_by: string; created_at: string
}

export interface ExportPreflight {
  readiness: ExportReadiness; warnings: ExportWarning[]; charts: number; status: ExportStatus
  templates: ExportTemplate[]; default_template_hash: string
}

export type ExportFormat = 'docx' | 'pdf'
export type ExportJobStatus = 'queued' | 'running' | 'succeeded' | 'failed'

export interface SnapshotSection { section_id: number; section_code: string; title: string; level: number; seq: number; rev_no: number; rev_id: number }

export interface ExportJob {
  id: number; task_id: number; outline_id: number; outline_version: number; format: ExportFormat; status: ExportJobStatus
  template_id?: number; template_key: string; template_hash: string
  rev_snapshot: Record<string, number>
  chart_snapshot: { chart_id: number; section_id: number; rev_no: number; chart_type: ChartType; title: string; render_status: ChartRenderStatus }[]
  matrix_snapshot: { requirement_code: string; section_code: string; deviation: string }[]
  doc_snapshot: { task_name: string; bidder?: string; outline_version: number; sections: SnapshotSection[]; created_at: string }
  artifact_name: string; artifact_hash: string; artifact_size: number; toc_refreshed: boolean
  warnings: ExportWarning[]; error: string; attempts: number; max_attempts: number
  lease_owner?: string; lease_until?: string; started_at?: string; finished_at?: string
  created_by: string; created_at: string; updated_at: string
}

export interface CreateExportReq { formats: ExportFormat[]; template_id?: number; bidder?: string }

// ---------------------------------------------------------------------------
// D2：企业证据可采性、承诺台账与最小报价门禁
// ---------------------------------------------------------------------------

export interface CompanyPerformance {
  id: number; tenant_id: string; code: string; project_name: string; buyer?: string; contract_no?: string
  contract_date?: string; acceptance_date?: string; amount?: string; currency?: string
  subject_name?: string; subject_type?: string; is_subsidiary: boolean; scope?: string
  created_by?: string; updated_by?: string; created_at: string; updated_at: string
}

export interface EvidenceOriginal {
  id: number; tenant_id: string; owner_domain: 'credential' | 'performance'; owner_id: number
  document_kind: string; page_label: string; original_path: string; original_sha256: string
  page_start: number; page_end: number; superseded_at?: string; created_at: string
}

export interface EvidenceBinding {
  id: number; tenant_id: string; task_id: number; requirement_item_id: number
  evidence_domain: 'credential' | 'performance'; evidence_id: number; created_at: string
}

export interface AdmissibilityItem {
  id: number; run_id: number; binding_id: number; evidence_domain: 'credential' | 'performance'
  evidence_id: number; verdict: 'admissible' | 'inadmissible' | 'manual'
  checks: { rule: string; status: string; expected?: string; actual?: string; detail?: string }[]
  reasons: { code: string; kind?: string; message: string }[]
}

export interface AdmissibilityRun {
  id: number; task_id: number; requirement_item_id: number; requirement_code: string; requirement_title: string
  rule_version: string; bid_deadline?: string; source_fingerprint: string
  verdict: 'admissible' | 'inadmissible' | 'manual'; status: 'current' | 'stale' | 'superseded'
  checks: { rule: string; status: string; expected?: string; actual?: string; detail?: string }[]
  reasons: { code: string; kind?: string; message: string }[]
  evaluated_by: string; evaluated_at: string; version: number; items?: AdmissibilityItem[]
}

export interface AdmissibilityDecision { item_id: number; verdict: 'admissible' | 'inadmissible'; reason: string }

export interface Commitment {
  id: number; task_id: number; code: string; requirement_item_id?: number; requirement_code?: string
  metric: string; metric_label: string; unit: string; threshold_cmp: string
  tender_threshold_raw: string; tender_threshold_value?: number; committed_raw: string; committed_value?: number
  capability_limit?: number; capability_basis: string; owner: string; rationale: string
  source_admissibility_item_ids: number[]; status: 'draft' | 'confirmed' | 'signed_off' | 'rejected'
  requires_delivery_signoff: boolean; signed_off_by?: string; signed_off_at?: string; version: number
  created_by?: string; updated_by?: string; created_at: string; updated_at: string
}

export interface CommitmentInput {
  code: string; requirement_item_id?: number; metric: string; metric_label: string; unit: string; threshold_cmp: string
  tender_threshold_raw: string; tender_threshold_value?: number; committed_raw: string; committed_value?: number
  capability_limit?: number; capability_basis: string; owner: string; rationale: string; source_admissibility_item_ids?: number[]
}

export interface BidQuoteItem {
  id: number; quote_id: number; line_no: number; code: string; name: string
  unit_price: string; quantity: string; line_amount: string; requirement_item_id?: number; model?: string
}

export interface BidQuote {
  id: number; task_id: number; lot: string; version: number; declared_total: string; ceiling?: string; currency: string
  status: 'draft' | 'confirmed' | 'rejected'; note?: string; confirmed_by?: string; confirmed_at?: string
  created_at: string; updated_at: string; items?: BidQuoteItem[]
}

export interface BidQuoteValidationCheck { rule: string; status: 'pass' | 'fail'; detail?: string }
export interface BidQuoteValidation {
  quote_id?: number; lot: string; version: number; declared_total: string
  checks: BidQuoteValidationCheck[]; ok: boolean
}

export interface ObjectiveScoreRun {
  id: number; task_id: number; requirement_item_id: number; requirement_code: string; requirement_title: string
  admissibility_run_id?: number; rule_version: string; status: 'computed' | 'manual'
  score: number; max_score: number; evidence_ids: number[]; gaps: string[]; computed_at: string
}


// ---------------------------------------------------------------------------
// D3：故事板（Pink Team 粉队评审）、受控引用渲染与句子级写作核验 E 级指标
// ---------------------------------------------------------------------------

export type StoryboardStatus = 'draft' | 'pending_pink_review' | 'pink_approved' | 'pink_rejected' | 'stale' | 'superseded'

export interface StoryboardScoringPoint {
  requirement_id: number; requirement_code: string; our_approach: string; buyer_benefit: string; evidence_refs: string[]
}

export interface StoryboardChartPlan {
  chart_type: string; title: string; purpose?: string
}

export interface SectionStoryboard {
  id: number; tenant_id: string; task_id: number; section_id: number; section_code: string; section_title: string
  outline_version: number; version: number; theme_sentence: string
  scoring_points: StoryboardScoringPoint[]; admissibility_item_ids: number[]; evidence_ids: number[]; commitment_ids: number[]
  chart_plans: StoryboardChartPlan[]; status: StoryboardStatus; stale_reason?: string; review_note?: string
  created_by: string; submitted_by?: string; submitted_at?: string; approved_by?: string; approved_at?: string
  created_at: string; updated_at: string
}

export interface UpsertStoryboardReq {
  theme_sentence: string
  scoring_points: StoryboardScoringPoint[]
  admissibility_item_ids?: number[]
  evidence_ids?: number[]
  commitment_ids?: number[]
  chart_plans?: StoryboardChartPlan[]
}

export interface GroundedDraftRenderResult {
  rendered_md: string
  evidence_ids: number[]
  admissibility_item_ids: number[]
  commitment_ids: number[]
}

export type SentenceClaimClass = 'bidder_fact' | 'commitment' | 'tender_restatement' | 'general_narrative'

export interface SentenceClaim {
  index: number; text: string; kind: SentenceClaimClass; grounded: boolean
  admissibility_item_ids?: number[]; retrieval_evidence_ids?: number[]; commitment_ids?: number[]
  extracted_metrics?: string[]; extracted_entities?: string[]; reasons?: string[]
}

export interface WritingVerificationReason {
  code: string; sentence_index?: number; message: string
}

export interface WritingVerificationMetrics {
  total_sentences: number
  bidder_fact_sentences: number
  grounded_fact_sentences: number
  ungrounded_fact_sentences: number
  commitment_sentences: number
  grounded_commitment_sentences: number
  ungrounded_commitment_sentences: number
  tender_restatement_sentences: number
  general_narrative_sentences: number
  ungrounded_claim_count: number
  fact_binding_rate: number
  commitment_binding_rate: number
  tender_fact_hits: number
  tender_fact_total: number
  specificity_ratio: number
  kb_similarity_max: number
  boilerplate_similarity_max: number
  matched_tender_terms?: string[]
  missing_tender_terms?: string[]
}

export interface SectionWritingVerification {
  id: number; tenant_id: string; task_id: number; section_id: number; section_code?: string; section_title?: string
  section_rev_id: number; rev_no: number; storyboard_id?: number
  rule_version: string; verdict: 'passed' | 'failed' | 'stale' | 'superseded'; stale_reason?: string
  ungrounded_claim_count: number; fact_binding_rate: number; commitment_binding_rate: number
  specificity_ratio: number; kb_similarity_max: number; boilerplate_similarity_max: number
  claims: SentenceClaim[]; metrics: WritingVerificationMetrics; reasons: WritingVerificationReason[]
  created_by: string; updated_by: string; approved_by?: string; created_at: string; updated_at: string
}
