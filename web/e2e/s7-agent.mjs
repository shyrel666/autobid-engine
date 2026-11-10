// S7 Agent 路径：脚本化替身模型驱动真实 Planner / Worker，走 Skill 解锁、后台作业和事件唤醒。
// 判据：Agent 提交的作业在页面上可见；候选只能由人工采用；Agent 不能提交最终稿。
import { readFile } from 'node:fs/promises'
import { BASE, STUB, api, assert, ensureStubProfile, session, shot, stepper, waitFor } from './s7-lib.mjs'
import { TOKEN, seedProject } from './lib.mjs'

const { page, finish } = await session()
const step = stepper(page)
const modal = page.getByRole('dialog')
const stub = (path, body) => fetch(`${STUB}${path}`, { method: 'POST', body: JSON.stringify(body ?? {}) })
let task, file, section

await step('环境：配置替身模型（非真实模型），准备项目与原件', async () => {
  await ensureStubProfile()
  task = await seedProject('writing', 'S7 Agent 路径（替身模型，非真实模型）', { goals: ['解析招标原件', '生成总体架构图'], timeout_seconds: 900 })
  section = task.sections[0]
  const body = await readFile('web/e2e/fixtures/s7-tender.md')
  const res = await fetch(`${BASE}/api/tasks/${task.id}/files?filename=agent-tender.md`, { method: 'POST', headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'text/markdown' }, body })
  assert(res.ok, `上传原件失败：${res.status}`)
  file = await res.json()
  await api('POST', `/api/tasks/${task.id}/sections/${section.id}/revisions`, { base_rev_no: 0, based_on_outline_version: task.outlineVersion, content_md: `## ${section.title}\n\n本章说明总体架构。`, summary_note: '环境准备' })
})

const script = () => [
  { name: 'Skill', args: { name: 'document-processing' } },
  { name: 'document_parse_submit', args: { source_kind: 'tender_file', operation: 'import', file_id: file.id, request_key: 's7-agent-parse', tender_no: 'SPJK-2026-018', title: 'Agent 导入的招标文件' } },
  { name: 'document_parse_submit', args: { source_kind: 'knowledge_file', operation: 'import', file_id: file.id, request_key: 's7-agent-knowledge' } },
  { name: 'Skill', args: { name: 'diagram-generation' } },
  { name: 'chart_render_submit', args: { section_id: section.id, expected_rev_no: 1, chart_type: 'architecture', title: 'Agent 生成的总体架构图', request_key: 's7-agent-chart', spec: { layers: [{ name: '管理层', nodes: ['运维工作台', '状态展示'] }] } } },
  { name: 'Skill', args: { name: 'proposal-export' } },
  { name: 'proposal_export_submit', args: { formats: ['docx'], expected_outline_version: task.outlineVersion, request_key: 's7-agent-export' } },
]

await step('启动项目：替身 Worker 经 Skill 提交解析和图表作业，并被拒绝越界操作', async () => {
  await stub('/config', { agent_script: script(), tool_results: [] })
  await api('POST', `/api/tasks/${task.id}/start`, {})
  const results = await waitFor('脚本执行到最后一步', async () => {
    const s = await (await fetch(`${STUB}/state`)).json()
    return s.tool_results?.length >= script().length ? s.tool_results : undefined
  }, { timeout: 180000 })
  const text = results.join('\n')
  assert(/FEATURE_DISABLED/.test(text), `Agent 导出应被服务器配置拒绝：${text.slice(-600)}`)
  assert(results[2].includes('error') || /false/.test(results[2]), `S5b 来源不应被接受：${results[2]}`)
  return '导出 FEATURE_DISABLED；knowledge_file 来源被拒绝'
})

await step('Agent 提交的解析候选在页面可见，标明 Agent 提交，未进入正式文档', async () => {
  const record = await waitFor('候选就绪', async () => {
    const list = await api('GET', `/api/tasks/${task.id}/file-processing-records`)
    return list.find((r) => r.status === 'ready' && r.created_by === 'worker')
  })
  await page.goto(`${BASE}/tasks/${task.id}/documents`)
  await page.getByRole('button', { name: `核对候选 #${record.id}`, exact: true }).waitFor()
  await page.getByText('Agent 提交', { exact: false }).first().waitFor()
  const docs = await api('GET', `/api/tasks/${task.id}/documents`)
  assert(docs.length === 1, `Agent 候选不得自动成为正式文档，实际 ${docs.length} 份`)
  await shot(page, 'agent-candidate')
})

await step('页面拒绝候选：Agent 没有采用工具，决定权在人', async () => {
  await page.getByRole('button', { name: /^核对候选/ }).first().click()
  await modal.getByLabel('拒绝原因', { exact: false }).fill('工程样本')
  await modal.getByRole('button', { name: /拒绝/ }).click()
  await modal.waitFor({ state: 'hidden' })
  // Skill 的结果列出它解锁的工具：解析技能里不能有采用工具。
  const s = await (await fetch(`${STUB}/state`)).json()
  assert(/document_parse_submit/.test(s.tool_results[0]) && !/adopt/i.test(s.tool_results[0]), `解析技能不应解锁采用工具：${s.tool_results[0].slice(0, 300)}`)
})

await step('Agent 提交的图表由真实 Mermaid 渲染，页面可见', async () => {
  await waitFor('图表渲染完成', async () => {
    const charts = await api('GET', `/api/tasks/${task.id}/charts?section_id=${section.id}`)
    return (charts.charts ?? charts).some((c) => c.render_status === 'rendered')
  }, { timeout: 120000 })
  await page.goto(`${BASE}/tasks/${task.id}/writing/${section.id}?panel=charts`)
  await page.getByRole('img', { name: /Agent 生成的总体架构图/ }).first().waitFor()
})

await step('后台作业卡片显示 Agent 提交的作业，Intent 收尾', async () => {
  const jobs = await api('GET', `/api/tasks/${task.id}/agent-jobs`)
  const list = jobs.jobs ?? jobs.subscriptions ?? jobs
  assert(list.length >= 2, `应有解析和图表两个订阅：${JSON.stringify(list).slice(0, 300)}`)
  await page.goto(`${BASE}/tasks/${task.id}/monitor`)
  await page.getByText(/后台作业|等待后台处理/).first().waitFor()
  await shot(page, 'agent-jobs')
  await waitFor('意图不再运行', async () => {
    const t = await api('GET', `/api/tasks/${task.id}`)
    return t.inflight === 0
  }, { timeout: 120000 })
  return `${list.length} 个后台作业订阅`
})

await api('POST', `/api/tasks/${task.id}/pause`, undefined, { ok: [404, 409] }).catch(() => {})
await finish()
console.log('S7 AGENT COMPLETE（模型为替身）')
