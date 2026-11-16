// S7 关闭能力与权限：每种配置真实重启服务，在页面上确认用户看到原因与下一步，没有假成功。
// 服务由 e2e 测试启动；E2E_CONTROL 提供真实杀进程与按配置重启。
import { readFile } from 'node:fs/promises'
import { BASE, TOKEN, launch, newPage, seedProject } from './lib.mjs'
import { api, assert, session, shot, stepper, waitFor } from './s7-lib.mjs'

const control = async (path) => {
  const r = await fetch(`${process.env.E2E_CONTROL}${path}`, { method: 'POST' })
  assert(r.ok, `控制服务失败 ${path}：${await r.text()}`)
}
const restart = (...env) => control(`/restart?${env.map((e) => `env=${encodeURIComponent(e)}`).join('&')}`)

const { page, finish, browser } = await session()
const step = stepper(page)
const modal = page.getByRole('dialog')
let ready, other, finalJob

await step('准备：带图表的可导出项目、另一项目，用真实文档引擎导出一份最终稿', async () => {
  ready = await seedProject('ready', 'S7 关闭能力·可导出项目')
  other = await seedProject('writing', 'S7 关闭能力·另一项目')
  const section = ready.sections[0]
  const d = await api('GET', `/api/tasks/${ready.id}/sections/${section.id}`)
  const job = await api('POST', `/api/tasks/${ready.id}/chart-jobs`, { request_key: 's7-cap-chart', section_id: section.id, expected_rev_no: d.section.current_rev_no, chart_type: 'architecture', title: '可用图表', spec: { layers: [{ name: '管理层', nodes: ['工作台', '状态展示'] }] } })
  await waitFor('图表渲染', async () => (await api('GET', `/api/tasks/${ready.id}/chart-jobs/${job.job.id}`)).job.status === 'succeeded')
  await api('POST', `/api/tasks/${ready.id}/compliance/verify`, {})
  await api('POST', `/api/tasks/${ready.id}/consistency/runs`)
  const sub = await api('POST', `/api/tasks/${ready.id}/exports`, { request_key: 's7-cap-final', formats: ['docx'], mode: 'final', bidder: '合成档案服务公司' })
  const id = (sub.jobs ?? sub.request?.jobs)[0].id
  finalJob = await waitFor('最终稿完成', async () => {
    const j = await api('GET', `/api/tasks/${ready.id}/exports/${id}`)
    assert(j.status !== 'failed', `最终稿失败：${j.error}`)
    return j.status === 'succeeded' ? j : undefined
  }, { timeout: 180000 })
  assert(finalJob.business_state === 'final_ready', '准备阶段应得到真实最终稿')
})

await step('令牌：缺失或错误时不显示业务数据，接口和活动流都 401', async () => {
  for (const [label, token] of [['缺失', ''], ['错误', 'wrong-token']]) {
    const s = await newPage(browser, { token })
    await s.page.goto(`${BASE}/tasks`)
    await s.page.getByRole('dialog', { name: 'API 访问令牌' }).waitFor()
    assert(!(await s.page.locator('body').innerText()).includes('S7 关闭能力'), `令牌${label}时不应显示项目`)
    await s.context.close()
  }
  for (const path of ['/api/tasks', `/api/tasks/${ready.id}/events`, `/api/tasks/${ready.id}/exports/${finalJob.id}/download`]) {
    const r = await fetch(`${BASE}${path}`)
    assert(r.status === 401, `${path} 无令牌应为 401，实际 ${r.status}`)
  }
})

await step('跨项目改编号：读取、下载、取消别的项目的对象都返回 404', async () => {
  const a = other.id
  const cases = [
    ['GET', `/api/tasks/${a}/exports/${finalJob.id}`],
    ['GET', `/api/tasks/${a}/exports/${finalJob.id}/download`],
    ['POST', `/api/tasks/${a}/exports/${finalJob.id}/cancel`],
    ['GET', `/api/tasks/${a}/sections/${ready.sections[0].id}`],
    ['POST', `/api/tasks/${a}/sections/${ready.sections[0].id}/storyboard/approve`],
  ]
  for (const [method, path] of cases) {
    const r = await fetch(`${BASE}${path}`, { method, headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json', 'X-Autobid-User': 'probe' }, body: method === 'POST' ? '{}' : undefined })
    assert(r.status === 404, `${method} ${path} 应为 404，实际 ${r.status}`)
  }
  return `${cases.length} 个越界请求`
})

await step('文档引擎关闭：最终稿被拒绝不降级，DOCX 只能是预览稿，已有最终稿仍可下载', async () => {
  await restart('AUTOBID_OFFICE_ENGINE=off')
  await page.goto(`${BASE}/tasks/${ready.id}/export`)
  await page.getByText(/文档引擎不可用/).first().waitFor()
  await page.getByRole('radio', { name: /最终稿/ }).check()
  assert(await page.getByRole('button', { name: /锁定版本并导出最终稿/ }).isDisabled(), '引擎关闭时最终稿入口应禁用')
  const r = await fetch(`${BASE}/api/tasks/${ready.id}/exports`, { method: 'POST', headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ request_key: 's7-cap-off', formats: ['docx'], mode: 'final', bidder: '合成档案服务公司' }) })
  const body = await r.json()
  assert(!r.ok && /runtime_unavailable|export_blocked/.test(body.code ?? ''), `接口应拒绝最终稿：${r.status} ${JSON.stringify(body).slice(0, 200)}`)
  await page.getByRole('radio', { name: /预览稿/ }).check()
  await page.getByRole('checkbox', { name: 'PDF', exact: true }).uncheck()
  await page.getByRole('button', { name: /锁定版本并导出预览稿/ }).click()
  const preview = await waitFor('预览稿完成', async () => {
    const list = await api('GET', `/api/tasks/${ready.id}/exports`)
    return list.jobs.find((j) => j.status === 'succeeded' && j.business_state === 'preview_only')
  })
  assert(preview.format === 'docx', '引擎关闭时只能生成 DOCX 预览稿')
  await page.reload()
  await page.getByText('仅预览稿', { exact: false }).first().waitFor()
  const old = await fetch(`${BASE}/api/tasks/${ready.id}/exports/${finalJob.id}/download`, { headers: { Authorization: `Bearer ${TOKEN}` } })
  assert(old.ok && old.headers.get('X-Export-Business-State') === 'final_ready', `已有最终稿应仍可下载：${old.status}`)
  await shot(page, 'office-off')
})

await step('Mermaid 不可用：新图表被拒绝并说明，已有图表不误禁导出', async () => {
  await restart('AUTOBID_MMDC_PATH=D:/s7-does-not-exist/cli.js')
  const section = ready.sections[0]
  await page.goto(`${BASE}/tasks/${ready.id}/writing/${section.id}?panel=charts`)
  await page.getByRole('button', { name: '生成图表', exact: true }).click()
  await modal.getByLabel('题注', { exact: true }).fill('不会成功的图表')
  await modal.getByRole('button', { name: '提交渲染', exact: true }).click()
  await page.getByText(/渲染|Mermaid|mmdc/i).first().waitFor()
  const list = await api('GET', `/api/tasks/${ready.id}/chart-jobs`)
  assert(!(list.jobs ?? list).some((j) => j.title === '不会成功的图表'), '渲染器不可用时不应创建作业')
  await page.goto(`${BASE}/tasks/${ready.id}/export`)
  await page.getByRole('radio', { name: /最终稿/ }).check()
  const pre = await api('GET', `/api/tasks/${ready.id}/exports/preflight?mode=final`)
  assert(!pre.blockers?.some?.((b) => b.kind === 'chart_not_final') && !pre.readiness?.blockers?.some?.((b) => b.kind === 'chart_not_final'), '已有有效图表不应被判未渲染')
})

await step('OCR 未配置：扫描件不被静默当空白导入，文本文件正常', async () => {
  await restart('AUTOBID_OCR_URL=')
  const task = await api('POST', '/api/tasks', { name: 'S7 关闭能力·OCR', goal: '核对扫描件处理', start: false })
  const up = async (name, type) => {
    const body = await readFile(name)
    const r = await fetch(`${BASE}/api/tasks/${task.id}/files?filename=${encodeURIComponent(name.split('/').pop())}`, { method: 'POST', headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': type }, body })
    assert(r.ok, `上传失败 ${r.status}`)
    return r.json()
  }
  const scan = await up('ingest/testdata/scan_tender_page.jpg', 'image/jpeg')
  const r = await fetch(`${BASE}/api/tasks/${task.id}/file-jobs`, { method: 'POST', headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ file_id: scan.id, request_key: 's7-cap-ocr', tender_no: 'S7', parser_profile: 'auto', candidate_only: true }) })
  const body = await r.json()
  assert(r.status === 503 && body.code === 'runtime_unavailable', `扫描件应被拒绝：${r.status} ${JSON.stringify(body).slice(0, 200)}`)
  await page.goto(`${BASE}/tasks/${task.id}/documents`)
  const option = page.getByLabel('解析配置', { exact: true }).first().locator('option', { hasText: '不可用' })
  await waitFor('OCR 选项显示不可用', async () => await option.count() > 0)
  assert((await api('GET', `/api/tasks/${task.id}/documents`)).length === 0, '被拒绝的扫描件不得成为正式文档')
  const text = await up('web/e2e/fixtures/s7-tender.md', 'text/markdown')
  const job = await api('POST', `/api/tasks/${task.id}/file-jobs`, { file_id: text.id, request_key: 's7-cap-text', tender_no: 'S7', parser_profile: 'auto', candidate_only: true })
  await waitFor('文本解析完成', async () => (await api('GET', `/api/tasks/${task.id}/file-jobs/${job.id}`)).status === 'succeeded')
})

await step('材料处理关闭：新作业被拒绝；关闭前的作业与必需材料仍可查看，材料可修改；粘贴导入仍可用', async () => {
  // 关闭前：留存原件并完成一次解析，再登记一项必需材料（必需材料未齐会阻断导出，关闭后也要能处理）。
  const task = await api('POST', '/api/tasks', { name: 'S7 关闭能力·材料处理', goal: '核对关闭状态', start: false })
  const up = await fetch(`${BASE}/api/tasks/${task.id}/files?filename=s7-tender.md`, { method: 'POST', headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'text/markdown' }, body: await readFile('web/e2e/fixtures/s7-tender.md') })
  assert(up.ok, `留存原件失败 ${up.status}`)
  const file = await up.json()
  const job = await api('POST', `/api/tasks/${task.id}/file-jobs`, { file_id: file.id, request_key: 's7-cap-before-off', tender_no: 'S7', parser_profile: 'auto', candidate_only: true })
  await waitFor('关闭前的解析完成', async () => (await api('GET', `/api/tasks/${task.id}/file-jobs/${job.id}`)).status === 'succeeded')
  await api('POST', `/api/tasks/${task.id}/materials`, { id: 0, name: 'S7 必需附件', required: true, file_id: null, version: 0, note: '' })

  await restart('AUTOBID_FILE_PROCESSING_ENABLED=0')
  await page.goto(`${BASE}/tasks/${task.id}/documents`)
  await page.getByText('材料处理未启用', { exact: false }).first().waitFor()
  await page.getByText(`作业 #${job.id}`, { exact: false }).waitFor()
  assert(await page.getByLabel('选择后台处理原件').count() === 0, '关闭时不应提供上传入口')
  assert(await page.getByRole('button', { name: '核对候选' }).count() === 0, '关闭时不应提供核对候选入口')
  await page.getByRole('checkbox', { name: '必需材料', exact: true }).uncheck()
  await page.getByLabel('说明（取消必需时必填）', { exact: true }).fill('本项目不需要该附件（S7 关闭能力验收）')
  await page.getByRole('button', { name: '保存材料', exact: true }).click()
  await waitFor('材料改为非必需', async () => (await api('GET', `/api/tasks/${task.id}/materials`)).some((m) => m.name === 'S7 必需附件' && !m.required))
  await shot(page, 'file-processing-off')
  const r = await fetch(`${BASE}/api/tasks/${task.id}/file-jobs`, { method: 'POST', headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ file_id: file.id, request_key: 's7-cap-off2' }) })
  assert(r.status === 503, `材料处理关闭时不得接受固定作业：${r.status}`)
  await api('POST', `/api/tasks/${task.id}/documents`, { tender_no: 'S7-TEXT', title: '粘贴文本', text: '第一章 投标须知\n1.1 投标截止时间：2026年12月20日9时30分。\n', wake_planner: false })
  assert((await api('GET', `/api/tasks/${task.id}/documents`)).length === 1, '旧上传路径应继续可用')
  return `关闭前的作业 #${job.id} 可见，材料已在页面改为非必需`
})

await step('语义模型不可达：检索如实退回全文，状态不冒充语义检索', async () => {
  await restart('AUTOBID_EMBEDDING_URL=http://127.0.0.1:9/embed', 'AUTOBID_EMBEDDING_MODEL=s7-unreachable', 'AUTOBID_EMBEDDING_REVISION=s7', 'AUTOBID_EMBEDDING_DIMENSION=3', 'AUTOBID_EMBEDDING_FORMAT=tei', 'AUTOBID_EMBEDDING_TIMEOUT_SECONDS=2')
  await api('POST', '/api/knowledge/documents', { category: 'whitepaper', title: 'S7 知识样本', text: '档案扫描按目录逐项登记，扫描后进行图像清晰度复核。' })
  await page.goto(`${BASE}/knowledge`)
  const status = await api('GET', '/api/knowledge/retrieval-status')
  assert(status.state !== 'semantic' && status.state !== 'healthy', `模型不可达不应报告语义可用：${status.state}`)
  await page.getByText('知识库上传不经过候选核对', { exact: false }).waitFor()
  const body = await page.locator('body').innerText()
  assert(!/语义检索(已启用|正常)/.test(body), '状态条不应冒充语义检索')
  await shot(page, 'semantic-off')
  return `状态 ${status.state}`
})

await step('恢复默认配置后能力回到可用', async () => {
  await restart()
  await page.goto(`${BASE}/tasks/${ready.id}/export`)
  await page.getByText(/WPS|Word/).first().waitFor()
})

await finish()
console.log('S7 CAPABILITIES COMPLETE')
