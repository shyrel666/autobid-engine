// 视觉测试 fixture（FRONTEND_REFACTOR_PLAN.md 第 10.3 节）：只在开发服务器的 /visual.html 入口加载，
// 以设计样稿 v1 的固定投标示例替换 fetch 与 EventSource，用于 V01–V07 的目标 / 实现对照。
// 生产构建只打包 index.html，本文件不会进入 web/dist；这里的数据不得用作业务验收。

const at = (hms: string) => `2026-10-29T${hms}+08:00`
const created = at('09:00:00')

const tasks = [
  { id: 104, name: '道路照明维护服务项目', description: '核对章节内容与引用依据', status: 'done' },
  { id: 103, name: '公共场馆保洁服务项目', description: '补充人员安排与质量措施', status: 'paused' },
  { id: 102, name: '园区设备维护服务项目', description: '复核设备清单与服务标准', status: 'running' },
  { id: 101, name: '市政设施运维服务项目', description: '整理技术要求与服务方案', status: 'running' },
].map((t) => ({ ...t, exploration_id: t.id, paused: t.status === 'paused', queued: false, timeout_seconds: 0, plan_heartbeat_seconds: 0,
  // 同一分钟内按秒区分，使默认「最新创建在前」与样稿的 101→104 顺序一致
  created_at: at(`09:00:0${105 - t.id}`), updated_at: created }))

const manifest = { tenant_id: 'default', task_id: 101, epoch: 2, status: 'confirmed', approved_by: '示例', updated_by: '示例', updated_at: created,
  bid_deadline: '2026-11-30T09:00:00+08:00', bid_deadline_source: { origin: 'extracted', page: 3 }, bid_lots: [] }
const outline = { id: 2, task_id: 101, version: 2, requirements_epoch: 2, status: 'approved', note: '', submitted_by: '', approved_by: '示例', approved_at: created,
  created_by: '', updated_by: '', created_at: created, updated_at: created }

const sectionTitles = ['项目理解', '服务实施方案', '人员与资源安排', '质量保障措施']
const sections = sectionTitles.map((title, i) => ({
  id: i + 1, task_id: 101, outline_id: 2, outline_version: 2, section_code: String(i + 1), title, level: 1, seq: i + 1, writing_guide: '', target_words: 0,
  depends_on_section_ids: [], current_rev_no: i === 0 ? 2 : i === 1 ? 3 : 1, approved_rev_no: i === 0 ? 2 : undefined, status: i === 0 ? 'approved' : 'draft',
  lease_token: 0, created_by: '', updated_by: '', approved_by: '', updated_at: created, requirement_item_ids: i === 1 ? [21] : [], depends_on: [], unmet_dependencies: [],
  dependencies_ready: true, writing: false,
}))

const body = '## 服务实施方案\n\n本章节按招标要求说明服务组织、工作步骤和检查记录。以下文字仅用于展示正文排版。\n\n### 2.1 服务组织\n\n按区域划分服务单元，明确现场负责人、工作安排与信息反馈方式。\n\n### 2.2 执行步骤\n\n先核对现场需求，再编制工作计划；执行过程中记录处理情况，并安排复核。\n\n### 2.3 质量检查\n\n建立问题登记与复核记录。待相关依据核验完成后，再确认正文中的具体承诺。'
const requirement = { id: 21, tenant_id: 'default', task_id: 101, code: '评分要求 2.1', category: 'scoring_item', title: '服务方案应包含组织安排、实施步骤及质量检查。',
  description: '', is_mandatory: false, score_weight: 10, score_group: 'technical', origin: 'extracted', source_doc_id: 1, source_chunk_id: 11, source_hash: '',
  source_loc: { doc_id: 1, section_code: '招标文件', page_start: 12, page_end: 12 }, evidence_run_id: 5, evidence_ids: [31], evidence_gap: '', review_status: 'confirmed',
  created_by: '', updated_by: '', approved_by: '', created_at: created, updated_at: created, mapped_section_codes: ['2'], veto_kind: '', veto_status: 'pending',
  veto_note: '', veto_checked_by: '', lot: '', out_of_bid: false }

const overview = {
  goals: [], open_intents: [],
  running_intents: [{ id: 201, state: 'running', owner: 'worker-1', summary: '解析评分条款' }, { id: 202, state: 'running', owner: 'worker-2', summary: '复核证明材料' }],
  paused_intents: [{ id: 203, state: 'paused', summary: '补充服务计划' }],
  recent_terminal_intents: [{ id: 204, state: 'done', summary: '整理文件目录' }],
  recent_facts: [], recent_findings: [], folded_nodes: 0,
}

const profiles = [
  { id: 1, name: '写作模型配置', format: 'openai', base_url: '', api_key_env: 'BID_WRITER_API_KEY', model: 'bid-writer-demo', max_tokens: 4096, effort: '', thinking_display: '',
    priority: 10, rank: 0, window_tokens: 128000, enabled: true, in_global_pool: true, proxy: '', session_header_key: '' },
  { id: 2, name: '核验模型配置', format: 'openai', base_url: '', api_key_env: 'BID_REVIEW_API_KEY', model: 'bid-review-demo', max_tokens: 4096, effort: '', thinking_display: '',
    priority: 20, rank: 0, window_tokens: 128000, enabled: true, in_global_pool: true, proxy: '', session_header_key: '' },
]

const routes: [RegExp, (m: RegExpMatchArray) => unknown][] = [
  [/^\/api\/tasks$/, () => tasks],
  [/^\/api\/tasks\/(\d+)$/, (m) => ({ ...tasks.find((t) => t.id === Number(m[1]))!, goal: '编写技术标', running: true, settling: false, inflight: 1 })],
  [/^\/api\/tasks\/\d+\/overview$/, () => overview],
  [/^\/api\/tasks\/\d+\/outlines$/, () => ({ outlines: [outline], approved_version: 2, open_version: 0, manifest })],
  [/^\/api\/tasks\/\d+\/outlines\/2$/, () => ({ outline, sections, coverage: { total: 1, mapped: 1, unmapped: [], out_of_scope: 0 }, manifest, approved_version: 2, requirements_epoch_current: true })],
  [/^\/api\/tasks\/\d+\/sections\/(\d+)$/, (m) => {
    const s = sections[Number(m[1]) - 1]
    const rev = { id: 90, section_id: s.id, rev_no: s.current_rev_no, base_rev_no: s.current_rev_no - 1, based_on_outline_version: 2, source: 'human_edit', content_md: body,
      summary_note: '', diff_summary: '', evidence_ids: [31], gaps: [], status: 'draft', created_by: '示例', approved_by: '', created_at: created }
    return { section: s, outline, approved_version: 2, writable: true, requirements: s.id === 2 ? [requirement] : [], dependencies: [], current_revision: rev, revisions: [rev],
      evidence: [{ id: 31, run_id: 5, source_domain: 'knowledge', source_id: 1, doc_id: 1, relation: 'insufficient', status: 'unverified', reason: '服务流程说明：用于辅助编写服务流程，尚未核验对本项目的适用性。', usable: false }], warnings: [] }
  }],
  [/^\/api\/tasks\/\d+\/charts/, () => ({ charts: [], renderer: { renderer: false, office: true } })],
  [/^\/api\/intercepts/, () => [{ id: 1, task_id: 101, intent_id: 202, agent_name: 'worker-2', tool_name: 'read_chunk', tool_input: {}, message: '', status: 'pending', created_at: created, waiter_until: created }]],
  [/^\/api\/intercept\/rules$/, () => []],
  [/^\/api\/intercept\/config$/, () => ({ enabled_tools: ['Bash', 'verify_commercial_bid'], default_tools: ['Bash', 'verify_commercial_bid'], judge: { enabled: false, profile_id: 0, prompt: '', timeout_seconds: 15, fail_action: 'allow', ask_timeout_seconds: 300, ask_timeout_action: 'deny' }, default_prompt: '', judge_usage: { calls: 0, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, daily: [] } })],
  [/^\/api\/intercept\/approvals/, () => ({ items: [], total: 0 })],
  [/^\/api\/llm\/health$/, () => []],
  [/^\/api\/llm\/profiles$/, () => profiles],
  [/^\/api\/tasks\/\d+\/llm-chain$/, () => ({ chain: [], active_profile_id: null, revision: 0 })],
  [/^\/api\/tasks\/\d+\/llm-usage$/, () => []],
  [/^\/api\/tasks\/\d+\/graph$/, () => ({ nodes: [], edges: [] })],
  [/^\/api\/tasks\/\d+\/agent-jobs/, () => []],
  [/^\/api\/tasks\/\d+\/retrieval-runs$/, () => []],
  [/^\/api\/llm\/records$/, () => []],
  [/^\/api\/system\/catalog$/, () => ({ skills: [], tools: [], agents: [] })],
]

const realFetch = window.fetch.bind(window)
window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
  const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.origin)
  if (url.pathname === '/healthz') return new Response('ok')
  if (url.pathname.startsWith('/api/')) {
    if ((init?.method ?? 'GET') !== 'GET') return new Response(JSON.stringify({ error: '视觉 fixture 不执行业务动作' }), { status: 405 })
    for (const [re, fn] of routes) {
      const m = url.pathname.match(re)
      if (m) return new Response(JSON.stringify(fn(m)), { headers: { 'Content-Type': 'application/json' } })
    }
    return new Response(JSON.stringify({ error: 'fixture 未提供该接口' }), { status: 404 })
  }
  return realFetch(input, init)
}

const activities = [
  { worker: '', kind: 'state', summary: '工作已开始，正在读取招标文件中的评分要求。', node_id: 201, at: at('09:00:12') },
  { worker: '', kind: 'tool', summary: '读取要求清单', node_id: 201, at: at('09:00:18'), detail: JSON.stringify({ document_id: 1, section_code: '2.1', category: '评分要求' }) },
  { worker: '', kind: 'text', summary: '已整理服务质量、人员安排和进度计划相关条款，等待人工复核。', node_id: 201, at: at('09:00:25') },
]

class FixtureEventSource extends EventTarget {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2
  readyState = 0
  onopen: ((e: Event) => void) | null = null
  onerror: ((e: Event) => void) | null = null
  url: string
  constructor(url: string) {
    super()
    this.url = url
    setTimeout(() => {
      if (this.readyState === 2) return
      this.readyState = 1
      this.onopen?.(new Event('open'))
      activities.forEach((a, i) => this.dispatchEvent(new MessageEvent('activity', { data: JSON.stringify({ task_id: 101, ...a }), lastEventId: String(i + 1) })))
    }, 0)
  }
  close() { this.readyState = 2 }
}
;(window as unknown as { EventSource: unknown }).EventSource = FixtureEventSource

try {
  const p = new URLSearchParams(location.search)
  const sb = p.get('sidebar')
  if (sb === 'collapsed' || sb === 'expanded') localStorage.setItem('autobid.sidebar', sb)
  else localStorage.removeItem('autobid.sidebar') // 未指定时按视口宽度取默认值
  const route = p.get('route')
  if (route) history.replaceState(null, '', route)
} catch { /* ignore */ }

void import('../main')
