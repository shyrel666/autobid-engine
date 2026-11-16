// 关键数据安全与状态场景（FRONTEND_REFACTOR_PLAN.md 第 10.2 节）：对真实服务与隔离测试库执行。
// 用法：node web/e2e/scenarios.mjs [场景名 ...]；不带参数时执行除 export-runtime-failure 以外的全部场景。
// export-runtime-failure 需要服务端以 AUTOBID_OFFICE_TIMEOUT_SECONDS=1 启动（让文档引擎刷新必然超时；启动自检不受此值影响）。
import { BASE, account, api, assert, launch, newPage, recorder, saveSection, seedProject, shot } from './lib.mjs'

const rec = recorder('scenarios')
const browser = await launch()
const only = process.argv.slice(2).length ? process.argv.slice(2) : (process.env.E2E_SCENARIOS ? process.env.E2E_SCENARIOS.split(',') : [])
const want = (n) => (only.length ? only.includes(n) : n !== 'export-runtime-failure')
const stamp = new Date().toISOString().slice(11, 19).replaceAll(':', '')
const textarea = (page) => page.getByLabel('章节正文（Markdown）')

async function openWriting(page, id, sectionId) {
  await page.goto(`${BASE}/tasks/${id}/writing/${sectionId}`)
  await page.getByRole('region', { name: '章节正文' }).waitFor()
}
async function startEdit(page, text) {
  await page.getByRole('button', { name: /^(编辑正文|继续编辑)$/ }).click()
  await textarea(page).fill(text)
}
// 聚焦重读至多每 15 秒一次：等待后触发一次 focus，模拟用户切回浏览器。
async function refocus(page) {
  await page.waitForTimeout(15500)
  await page.evaluate(() => window.dispatchEvent(new Event('focus')))
}
const toast = (page, re) => page.locator('[role=status], [role=alert]').filter({ hasText: re }).first()

let writing
if (['history-guard', 'token-expiry', 'concurrent-save'].some(want)) writing = await seedProject('writing', `界面联调·写作 ${stamp}`)

if (want('upload-guard')) await rec.check('导入招标文件对话框：有填写或上传中关闭需确认，导入成功后直接关闭', async () => {
  // 导入新文件会让已批准的大纲失效，所以这里用单独的项目，不影响后面的写作场景。
  const uploads = await seedProject('writing', `界面联调·导入 ${stamp}`)
  const { page, context, errors } = await newPage(browser)
  await page.goto(`${BASE}/tasks/${uploads.id}/documents`)
  const open = async () => { await page.getByRole('button', { name: '导入文件' }).click(); await dlg.waitFor() }
  const dlg = page.getByRole('dialog', { name: '导入招标文件' })
  const ask = page.getByRole('alertdialog', { name: '放弃未保存的修改？' })
  await open()
  await page.keyboard.press('Escape')
  await dlg.waitFor({ state: 'hidden' })
  await open()
  await dlg.getByPlaceholder('ZB-2026-001').fill('SPJK-2026-018-C1')
  await page.keyboard.press('Escape')
  await ask.waitFor()
  await ask.getByRole('button', { name: '取消' }).click()
  assert(await dlg.isVisible(), '取消后导入对话框应保持打开')
  assert(await dlg.getByPlaceholder('ZB-2026-001').inputValue() === 'SPJK-2026-018-C1', '取消后填写内容应保留')
  await page.mouse.click(5, 300) // 点击遮罩
  await ask.waitFor()
  await ask.getByRole('button', { name: '放弃修改' }).click()
  await dlg.waitFor({ state: 'hidden' })
  // 上传进行中：拦截上传请求使其挂起，关闭时提示导入仍在进行。
  let release
  await page.route('**/api/tasks/*/documents', async (route) => {
    if (route.request().method() !== 'POST') return route.continue()
    await new Promise((r) => { release = r })
    await route.continue()
  })
  await open()
  await dlg.getByPlaceholder('ZB-2026-001').fill('SPJK-2026-018-C1')
  await dlg.getByRole('tab', { name: '粘贴文本' }).click()
  await dlg.locator('textarea').fill('视频监控扩容工程澄清公告\n一、投标截止时间顺延至2026年12月27日9时30分。\n- 1 -')
  await dlg.getByRole('button', { name: '导入并解析' }).click()
  await dlg.getByRole('button', { name: '正在上传并解析' }).waitFor()
  await dlg.getByRole('button', { name: '关闭' }).click()
  await ask.waitFor()
  const busyMsg = await ask.innerText()
  assert(busyMsg.includes('关闭对话框不会取消这次导入'), `上传中应说明不会取消导入：${busyMsg}`)
  await ask.getByRole('button', { name: '取消' }).click()
  release()
  await dlg.getByText(/文档 #\d+「/).waitFor({ timeout: 30000 })
  await shot(page, 'S1-upload-done')
  await page.keyboard.press('Escape')
  await dlg.waitFor({ state: 'hidden' })
  assert(!(await ask.isVisible()), '导入成功且未再修改时关闭不应确认')
  assert(errors.length === 0, errors.join('\n'))
  await context.close()
  return '无填写直接关闭；有填写时 Esc/遮罩/关闭均确认；上传中确认文案说明导入继续；成功后直接关闭'
})

if (want('history-guard')) await rec.check('浏览器后退被草稿保护拦截：继续编辑不丢稿，允许后不新增历史记录、前进记录保留', async () => {
  const { page, context, errors } = await newPage(browser)
  await page.goto(`${BASE}/tasks/${writing.id}/overview`)
  await page.getByRole('link', { name: '大纲与写作' }).first().click()
  await page.waitForURL(/\/outline/)
  await page.getByRole('link', { name: '章节写作' }).click()
  await page.waitForURL(/\/writing\/\d+/)
  const writingURL = page.url()
  await startEdit(page, '## 草稿\n\n后退保护测试正文。')
  const len = await page.evaluate(() => history.length)
  const choice = page.getByRole('dialog', { name: '当前章节有未保存的修改' })
  await page.evaluate(() => history.back())
  await choice.waitFor()
  assert(page.url() === writingURL, `拦截期间地址应退回写作页：${page.url()}`)
  await choice.getByRole('button', { name: '继续编辑' }).click()
  await choice.waitFor({ state: 'hidden' })
  assert(page.url() === writingURL, '继续编辑后应停留在写作页')
  assert(await textarea(page).inputValue() === '## 草稿\n\n后退保护测试正文。', '继续编辑后草稿应保留')
  assert(await page.evaluate(() => history.length) === len, '拦截后不应新增历史记录')
  await page.evaluate(() => history.back())
  await choice.waitFor()
  await choice.getByRole('button', { name: '放弃修改' }).click()
  await page.waitForURL(/\/outline/)
  assert(await page.evaluate(() => history.length) === len, `允许后退后历史长度应不变（${len}）`)
  await page.evaluate(() => history.forward())
  await page.waitForURL(writingURL)
  await page.evaluate(() => history.back())
  await page.waitForURL(/\/outline/)
  await page.evaluate(() => history.back())
  await page.waitForURL(/\/overview/)
  assert(errors.length === 0, errors.join('\n'))
  await context.close()
  return `history.length 始终为 ${len}；前进可回到写作页，再后退两次回到概览`
})

if (want('token-expiry')) await rec.check('会话失效：保存返回 401 时弹出重新登录，草稿保留，登录后保存成功', async () => {
  const { page, context } = await newPage(browser)
  const sec = writing.sections[0]
  await openWriting(page, writing.id, sec.id)
  const text = `## ${sec.title}\n\n会话失效期间编辑的正文（${stamp}）。`
  await startEdit(page, text)
  // 模拟会话在别处被吊销（例如本人在另一台电脑上改了口令）：用同一会话调用登出。
  const csrf = (await (await context.request.get(`${BASE}/api/auth/session`)).json()).csrf_token
  const out = await context.request.post(`${BASE}/api/auth/logout`, { headers: { 'X-Autobid-CSRF': csrf } })
  assert(out.status() === 204, `登出应成功：${out.status()}`)
  await page.getByRole('region', { name: '章节正文' }).getByRole('button', { name: '保存', exact: true }).click()
  const dlg = page.getByRole('dialog', { name: '登录已失效' })
  await dlg.waitFor()
  await shot(page, 'S3-session-401')
  assert(await textarea(page).inputValue() === text, '401 后草稿应保留')
  await dlg.getByLabel('口令', { exact: true }).fill(account('lead').password)
  await dlg.getByRole('button', { name: '重新登录', exact: true }).click()
  await dlg.waitFor({ state: 'hidden' })
  await page.waitForTimeout(800) // 重新登录后各页面重读
  assert(await textarea(page).inputValue() === text, '重新登录、页面重读后草稿应保留')
  await page.getByRole('region', { name: '章节正文' }).getByRole('button', { name: '保存', exact: true }).click()
  await toast(page, /已保存为修订/).waitFor()
  const d = await api('GET', `/api/tasks/${writing.id}/sections/${sec.id}`)
  assert(d.current_revision?.content_md === text, '服务端当前修订应为会话失效期间的草稿')
  assert(d.current_revision?.created_by === account('lead').login, `修订作者应是登录账号：${d.current_revision?.created_by}`)
  await context.close()
  return `修订 ${d.section.current_rev_no} 内容与草稿一致`
})

if (want('concurrent-save')) await rec.check('并发保存：两个客户端同时保存，一方成功、另一方得到候选稿且草稿保留，改换基线后可保存', async () => {
  const sec = writing.sections[1]
  const a = await newPage(browser)
  const b = await newPage(browser)
  await Promise.all([openWriting(a.page, writing.id, sec.id), openWriting(b.page, writing.id, sec.id)])
  const before = (await api('GET', `/api/tasks/${writing.id}/sections/${sec.id}`)).section.current_rev_no
  const ta = `## ${sec.title}\n\n客户端 A 的正文。`
  const tb = `## ${sec.title}\n\n客户端 B 的正文。`
  await startEdit(a.page, ta)
  await startEdit(b.page, tb)
  const saveBtn = (p) => p.getByRole('region', { name: '章节正文' }).getByRole('button', { name: '保存', exact: true })
  await Promise.all([saveBtn(a.page).click(), saveBtn(b.page).click()])
  const conflictOf = (p) => p.getByText(/版本冲突：修改已归档为候选稿修订 \d+/)
  const savedOf = (p) => toast(p, /已保存为修订/)
  const outcome = await Promise.all([a, b].map(async ({ page }) => {
    const r = await Promise.race([conflictOf(page).waitFor().then(() => 'conflict'), savedOf(page).waitFor().then(() => 'saved')])
    return r
  }))
  assert(outcome.filter((o) => o === 'saved').length === 1 && outcome.includes('conflict'), `应恰好一方成功：${outcome}`)
  const loser = outcome[0] === 'conflict' ? a : b
  const loserText = loser === a ? ta : tb
  await shot(loser.page, 'S4-conflict')
  assert(await textarea(loser.page).inputValue() === loserText, '冲突方草稿应保留在编辑器')
  const after = await api('GET', `/api/tasks/${writing.id}/sections/${sec.id}`)
  assert(after.section.current_rev_no === before + 1, `主版本只前进一次：${before} → ${after.section.current_rev_no}`)
  const cand = after.revisions.find((r) => r.status === 'conflict_candidate')
  assert(cand && cand.content_md === loserText, '服务端应归档冲突方的候选稿')
  await loser.page.getByRole('button', { name: '对比候选稿' }).click()
  await loser.page.waitForURL(/compare=1/)
  await loser.page.getByRole('button', { name: '关闭对比' }).click()
  await loser.page.getByRole('button', { name: '以最新为基线重新编辑' }).click()
  await saveBtn(loser.page).click()
  await savedOf(loser.page).waitFor()
  const final = await api('GET', `/api/tasks/${writing.id}/sections/${sec.id}`)
  // 候选稿也占用修订号，所以改换基线后的主版本是候选稿之后的下一个修订。
  assert(final.current_revision.content_md === loserText && final.section.current_rev_no === cand.rev_no + 1,
    `改换基线后应保存为新的主版本：rev ${before} → ${final.section.current_rev_no}，内容 ${JSON.stringify(final.current_revision.content_md.slice(0, 60))}`)

  // 后台新修订：编辑中的客户端聚焦重读后提示基线落后，不覆盖草稿。
  const mine = `## ${sec.title}\n\n后台更新期间的本地草稿。`
  await startEdit(a.page, mine)
  await saveSection(writing.id, sec.id, { extra: '另一位编辑者的更新。' })
  await refocus(a.page)
  await a.page.getByText(/编辑期间主版本已更新为修订/).waitFor()
  assert(await textarea(a.page).inputValue() === mine, '后台新修订不应覆盖本地草稿')
  await shot(a.page, 'S4-behind')
  await a.context.close(); await b.context.close()
  return `结果 ${outcome.join(' / ')}；候选稿修订 ${cand.rev_no}；改换基线后修订 ${final.section.current_rev_no}；后台更新提示基线落后`
})

let ready
if (['export-failure', 'export-runtime-failure', 'evidence-stale'].some(want)) ready = await seedProject('ready', `界面联调·导出 ${stamp}`)

if (want('export-failure')) await rec.check('导出失败：创建请求失败保留选项，下载失败给出提示，均不伪造成功', async () => {
  const { page, context } = await newPage(browser)
  await page.goto(`${BASE}/tasks/${ready.id}/export`)
  await page.getByText('可以导出').waitFor()
  await page.getByPlaceholder('如：某某科技有限公司').fill('联调样例科技有限公司')
  await page.route('**/api/tasks/*/exports', (route) => route.request().method() === 'POST'
    ? route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: 'injected: database unavailable' }) })
    : route.continue())
  await page.getByRole('button', { name: '锁定版本并导出' }).click()
  await toast(page, /HTTP 500/).waitFor()
  assert(await page.getByPlaceholder('如：某某科技有限公司').inputValue() === '联调样例科技有限公司', '失败后表单应保留')
  assert((await api('GET', `/api/tasks/${ready.id}/exports`)).jobs.length === 0, '失败时不应有导出任务')
  await page.unroute('**/api/tasks/*/exports')
  await page.getByRole('button', { name: '锁定版本并导出' }).click()
  await toast(page, /已提交.*导出请求组/).waitFor()
  const row = page.locator('tr').filter({ hasText: 'DOCX' }).first()
  await row.getByRole('button', { name: /KB|MB|B$/ }).waitFor({ timeout: 240000 })
  await page.route('**/api/tasks/*/exports/*/download', (route) => route.fulfill({ status: 410, contentType: 'application/json', body: JSON.stringify({ error: 'artifact missing' }) }))
  await row.getByRole('button', { name: /KB|MB|B$/ }).click()
  await toast(page, /HTTP 410/).waitFor()
  await shot(page, 'S5-export-download-failed')
  await context.close()
  return '注入 500 / 410；表单保留、无任务被创建、下载失败提示'
})

if (want('export-runtime-failure')) await rec.check('导出执行失败（文档引擎刷新超时）：显示重试与最终失败原因，停止轮询', async () => {
  const { page, context } = await newPage(browser)
  await page.goto(`${BASE}/tasks/${ready.id}/export`)
  await page.getByText('可以导出').waitFor()
  await page.getByPlaceholder('如：某某科技有限公司').fill('联调样例科技有限公司')
  await page.getByRole('button', { name: '锁定版本并导出' }).click()
  await toast(page, /已提交.*导出请求组/).waitFor()
  const pdf = page.locator('tr').filter({ hasText: 'PDF' }).first()
  await pdf.getByText('失败', { exact: true }).waitFor({ timeout: 300000 })
  const text = await pdf.innerText()
  await shot(page, 'S5-export-runtime-failed')
  const jobs = (await api('GET', `/api/tasks/${ready.id}/exports`)).jobs
  const j = jobs.find((x) => x.format === 'pdf')
  assert(j.status === 'failed' && j.attempts === j.max_attempts, `PDF 应在用尽重试后失败：${j.status} ${j.attempts}/${j.max_attempts}`)
  assert(text.includes(`第 ${j.attempts} 次执行`) && text.includes(j.error.slice(0, 10)), '行内应显示执行次数与失败原因')
  const before = await page.evaluate(() => performance.getEntriesByType('resource').filter((e) => e.name.includes('/exports')).length)
  await page.waitForTimeout(5000)
  const after = await page.evaluate(() => performance.getEntriesByType('resource').filter((e) => e.name.includes('/exports')).length)
  assert(after - before === 0, `全部结束后应停止轮询（5 秒内新增 ${after - before} 次请求）`)
  await context.close()
  return `PDF ${j.attempts}/${j.max_attempts} 次后失败：${j.error.slice(0, 60)}`
})

if (want('evidence-stale')) await rec.check('证据失效：来源删除后章节提示证据问题、草稿保留，导出门禁阻断并可跳转修复位置', async () => {
  const sec = ready.sections[0]
  const kd = await api('POST', '/api/knowledge/documents', { category: 'product_spec', title: `联调样例·网络摄像机技术白皮书 ${stamp}`, file_name: `vx4200-${stamp}.md`, tags: ['e2e'],
    text: '第一章 产品概述\n型号 VX-4200 网络摄像机，分辨率 400 万像素，支持 H.265 编码与 GB/T 28181 接入。\n第二章 存储\n配套存储节点支持视频存储 90 天。' })
  const run = await api('POST', `/api/tasks/${ready.id}/research`, { request_key: `e2e-stale-${stamp}`, question: '400万像素网络摄像机 H.265 编码' })
  const ev = run.evidence.find((e) => e.doc_id === kd.document.id)
  assert(ev, '检索应返回知识库证据')
  await saveSection(ready.id, sec.id, { evidenceIds: [ev.id], approve: true })
  await api('POST', `/api/tasks/${ready.id}/compliance/verify`, {})
  await api('POST', `/api/tasks/${ready.id}/consistency/runs`)
  const { page, context, errors } = await newPage(browser)
  await openWriting(page, ready.id, sec.id)
  const mine = `## ${sec.title}\n\n证据失效期间的本地草稿。`
  await startEdit(page, mine)
  await api('DELETE', `/api/knowledge/documents/${kd.document.id}`)
  await refocus(page)
  await page.getByText('当前修订的证据存在问题，不能作为无缺口定稿').waitFor()
  assert(await textarea(page).inputValue() === mine, '证据失效刷新后草稿应保留')
  const basis = page.getByRole('button', { name: '写作依据' })
  if (await basis.isVisible()) await basis.click() // 1440px 展开侧栏时为两栏，依据面板在抽屉中
  await page.getByRole('tab', { name: /证据/ }).click()
  await page.getByLabel('有不可用证据').waitFor()
  await page.getByText('来源已失效').first().waitFor()
  await shot(page, 'S6-evidence-stale-writing')
  if (await basis.isVisible()) await page.keyboard.press('Escape') // 关闭依据抽屉
  await page.getByRole('region', { name: '章节正文' }).getByRole('button', { name: '保存', exact: true }).click() // 保存草稿后再离开
  await toast(page, /已保存为修订/).waitFor()
  const readiness = await api('GET', `/api/tasks/${ready.id}/export-readiness`)
  assert(!readiness.ready, '证据失效后导出门禁应阻断')
  await page.getByRole('link', { name: '成稿导出' }).first().click()
  await page.waitForURL(/\/export/)
  await page.getByText(/项阻止(最终稿|导出)/).waitFor()
  await shot(page, 'S6-evidence-stale-export')
  const kinds = [...new Set(readiness.blockers.map((b) => b.kind))]
  const link = page.locator('section[aria-label] li a').first()
  const label = (await link.innerText()).trim()
  await link.click()
  await page.waitForURL((u) => !u.pathname.endsWith('/export'))
  const to = new URL(page.url())
  await shot(page, 'S6-evidence-stale-fix-target')
  assert(errors.length === 0, errors.join('\n'))
  await context.close()
  return `阻断类型 ${kinds.join('、')}；「${label}」跳转到 ${to.pathname}${to.search}`
})

await browser.close()
process.exit(await rec.save())
