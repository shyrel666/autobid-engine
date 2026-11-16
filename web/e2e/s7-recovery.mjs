// S7 刷新与重启恢复：在作业运行中真实结束服务进程，重启后页面恢复到一致状态。
// 判据：不重复产物（条数断言）、原截止时间不被延长、页面刷新后显示真实终态。模型是脚本化替身。
import { readFile } from 'node:fs/promises'
import { BASE, TOKEN, seedProject } from './lib.mjs'
import { STUB, api, assert, ensureStubProfile, session, shot, stepper, waitFor } from './s7-lib.mjs'

const control = async (path) => {
  const r = await fetch(`${process.env.E2E_CONTROL}${path}`, { method: 'POST' })
  assert(r.ok, `控制服务失败 ${path}：${await r.text()}`)
}
const envQuery = () => ['AUTOBID_OFFICE_TIMEOUT_SECONDS=20', 'AUTOBID_MMDC_TIMEOUT_SECONDS=20', `AUTOBID_EMBEDDING_URL=${STUB}/v1/embeddings`, 'AUTOBID_EMBEDDING_MODEL=s7-scripted', 'AUTOBID_EMBEDDING_REVISION=s7', 'AUTOBID_EMBEDDING_DIMENSION=3', 'AUTOBID_EMBEDDING_FORMAT=compatible', 'AUTOBID_EMBEDDING_MAX_BATCH=1'].map((e) => `env=${encodeURIComponent(e)}`).join('&')
const crash = async () => {
  await control('/kill')
  await new Promise((r) => setTimeout(r, 500))
  await control(`/start?${envQuery()}`)
}
const stub = (path, body) => fetch(`${STUB}${path}`, { method: 'POST', body: JSON.stringify(body ?? {}) })

const { page, finish } = await session()
const step = stepper(page)
const modal = page.getByRole('dialog')
let project

await step('环境：替身模型与可导出项目', async () => {
  await ensureStubProfile()
  project = await seedProject('ready', 'S7 恢复（替身模型，非真实模型）')
})

await step('章节写作作业运行中杀进程：重启后只写回一个修订，期限不延长', async () => {
  const fresh = await seedProject('writing', 'S7 恢复·章节写作')
  const s = fresh.sections[0]
  await stub('/config', { writer_delay: 12 })
  await page.goto(`${BASE}/tasks/${fresh.id}/writing/${s.id}`)
  await page.getByRole('button', { name: 'AI 撰写初稿', exact: true }).click()
  await modal.getByRole('button', { name: '派发', exact: true }).click()
  const jobs = async () => ((await api('GET', `/api/tasks/${fresh.id}/section-jobs`)) ?? []).filter((j) => j.section_id === s.id)
  const first = await waitFor('作业开始执行', async () => (await jobs()).find((j) => j.status === 'running'), { timeout: 30000 })
  await crash()
  await stub('/config', { writer_delay: 0 })
  await page.reload()
  const done = await waitFor('写作作业完成', async () => (await jobs()).find((j) => j.job_id === first.job_id && !['queued', 'running'].includes(j.status)), { timeout: 240000 })
  assert(done.status === 'awaiting_review', `写作作业应恢复成功：${done.status} ${done.error ?? ''}`)
  assert(done.deadline_at === first.deadline_at, `截止时间不得延长：${first.deadline_at} → ${done.deadline_at}`)
  assert(done.attempts >= 2, `应记录被打断的尝试：attempts=${done.attempts}`)
  const d = await api('GET', `/api/tasks/${fresh.id}/sections/${s.id}`)
  assert(d.revisions.length === 1, `只应有一个修订，实际 ${d.revisions.length}`)
  await page.reload()
  await page.getByText('脚本替身模型输出', { exact: false }).first().waitFor()
  return `attempts=${done.attempts}`
})

await step('解析作业运行中杀进程：重启后只产生一条候选，期限不延长', async () => {
  const task = await api('POST', '/api/tasks', { name: 'S7 恢复·解析', goal: '核对解析恢复', start: false })
  const body = await readFile('ingest/testdata/scan_tender_page.jpg')
  const up = await fetch(`${BASE}/api/tasks/${task.id}/files?filename=scan.jpg`, { method: 'POST', headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'image/jpeg' }, body })
  const file = await up.json()
  const job = await api('POST', `/api/tasks/${task.id}/file-jobs`, { file_id: file.id, request_key: 's7-recover-parse', tender_no: 'S7', parser_profile: 'auto', candidate_only: true })
  const first = await waitFor('解析开始', async () => {
    const j = await api('GET', `/api/tasks/${task.id}/file-jobs/${job.id}`)
    return j.status === 'running' ? j : undefined
  }, { timeout: 30000, every: 50 })
  await crash()
  const done = await waitFor('解析完成', async () => {
    const j = await api('GET', `/api/tasks/${task.id}/file-jobs/${job.id}`)
    return ['succeeded', 'failed'].includes(j.status) ? j : undefined
  }, { timeout: 240000 })
  assert(done.status === 'succeeded', `解析应恢复成功：${done.status} ${done.error ?? ''}`)
  assert(done.deadline_at === first.deadline_at, '解析截止时间不得延长')
  const records = (await api('GET', `/api/tasks/${task.id}/file-processing-records`)).filter((r) => r.candidate_only)
  assert(records.length === 1, `只应有一条候选，实际 ${records.length}`)
})

await step('图表作业运行中杀进程：重启后只产生一张图', async () => {
  const s = project.sections[0]
  const d = await api('GET', `/api/tasks/${project.id}/sections/${s.id}`)
  const sub = await api('POST', `/api/tasks/${project.id}/chart-jobs`, { request_key: 's7-recover-chart', section_id: s.id, expected_rev_no: d.section.current_rev_no, chart_type: 'architecture', title: '恢复用图表', spec: { layers: [{ name: '管理层', nodes: ['工作台', '状态展示'] }] } })
  const path = `/api/tasks/${project.id}/chart-jobs/${sub.job.id}`
  await waitFor('图表开始渲染', async () => (await api('GET', path)).job.status === 'running', { timeout: 30000, every: 50 })
  await crash()
  const done = await waitFor('图表完成', async () => {
    const j = (await api('GET', path)).job
    return ['succeeded', 'failed'].includes(j.status) ? j : undefined
  }, { timeout: 240000 })
  assert(done.status === 'succeeded', `图表应恢复成功：${done.status} ${done.error ?? ''}`)
  assert(done.deadline_at === sub.job.deadline_at, '图表截止时间不得延长')
  const charts = await api('GET', `/api/tasks/${project.id}/charts?section_id=${s.id}`)
  assert((charts.charts ?? charts).filter((c) => c.title === '恢复用图表').length === 1, '只应有一张图')
})

await step('导出作业运行中杀进程：重启后同一请求组完成，不产生重复作业', async () => {
  await api('POST', `/api/tasks/${project.id}/compliance/verify`, {})
  await api('POST', `/api/tasks/${project.id}/consistency/runs`)
  const sub = await api('POST', `/api/tasks/${project.id}/exports`, { request_key: 's7-recover-export', formats: ['docx', 'pdf'], mode: 'final', bidder: '合成档案服务公司' })
  const jobs = sub.jobs ?? sub.request.jobs
  await waitFor('导出开始执行', async () => {
    const list = await api('GET', `/api/tasks/${project.id}/exports`)
    return list.jobs.some((j) => j.status === 'running')
  }, { timeout: 60000, every: 50 })
  await crash()
  await page.goto(`${BASE}/tasks/${project.id}/export`)
  const done = await waitFor('导出完成', async () => {
    const list = (await api('GET', `/api/tasks/${project.id}/exports`)).jobs.filter((j) => jobs.some((x) => x.id === j.id))
    assert(!list.some((j) => j.status === 'failed'), `导出失败：${JSON.stringify(list.map((j) => j.error))}`)
    return list.length === jobs.length && list.every((j) => j.status === 'succeeded') ? list : undefined
  }, { timeout: 280000 })
  assert(done.every((j) => j.business_state === 'final_ready'), '恢复后应仍是严格最终稿')
  const again = await api('POST', `/api/tasks/${project.id}/exports`, { request_key: 's7-recover-export', formats: ['docx', 'pdf'], mode: 'final', bidder: '合成档案服务公司' })
  assert(again.replayed === true, '同键重发应重放原请求组，不产生新作业')
  await page.reload()
  await page.getByText('最终稿已生成', { exact: false }).first().waitFor()
  await shot(page, 'export-recovered')
})

await step('索引构建中杀进程：重启后接着完成，不新建第二个索引，期限不延长', async () => {
  for (let i = 0; i < 6; i++) await api('POST', '/api/knowledge/documents', { category: 'whitepaper', title: `S7 恢复知识 ${i}`, text: `档案扫描第 ${i} 批按目录逐项登记。\n\n${'图像清晰度复核与批次验收说明。'.repeat(20)}` })
  await stub('/config', { embed_delay: 5 })
  const sub = await api('POST', '/api/knowledge/indexes', { request_key: 's7-recover-index', dimension: 3 })
  const id = sub.index?.id ?? sub.id
  const first = await waitFor('构建开始', async () => {
    const list = await api('GET', '/api/knowledge/indexes')
    const x = (list.indexes ?? list).find((v) => v.id === id)
    return x?.status === 'building' ? x : undefined
  }, { timeout: 60000, every: 100 })
  await crash()
  await stub('/config', { embed_delay: 0 })
  const done = await waitFor('构建完成', async () => {
    const list = await api('GET', '/api/knowledge/indexes')
    const x = (list.indexes ?? list).find((v) => v.id === id)
    return ['ready', 'failed'].includes(x?.status) ? x : undefined
  }, { timeout: 240000 })
  assert(done.status === 'ready', `索引应恢复成功：${done.status} ${done.error ?? ''}`)
  assert(done.deadline_at === first.deadline_at, '索引构建截止时间不得延长')
  const all = await api('GET', '/api/knowledge/indexes')
  assert((all.indexes ?? all).filter((v) => v.request_key === 's7-recover-index').length === 1, '同一请求只应有一个索引')
  // 按游标接续的细节由 retrieval 的 S6 用例覆盖；这里验证进程真实死亡后作业能接管并完成。
  return `共 ${done.total_chunks} 块`
})

await finish()
console.log('S7 RECOVERY COMPLETE（模型为替身）')
