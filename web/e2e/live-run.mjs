// 执行工作台的实时运行检查：真实服务 + 真实 Planner / Worker / SSE 链路，模型换成脚本化的 OpenAI 兼容替身
// （web/e2e/stub_llm.py，固定节奏调用工具、回显收到的引导），不是对真实模型的验证。
// 前置：替身监听 E2E_STUB（缺省 http://127.0.0.1:18090），服务端进程环境中设置 STUB_LLM_KEY。
// 断线重连：设置 E2E_RESTART（重启被测服务的命令）时重启服务制造真实断线，否则用浏览器离线仿真。
// 用法：node web/e2e/live-run.mjs
import { execSync } from 'node:child_process'
import { BASE, api, assert, launch, newPage, recorder, shot } from './lib.mjs'

const STUB = process.env.E2E_STUB || 'http://127.0.0.1:18090'
const rec = recorder('live-run')
const stamp = new Date().toISOString().slice(11, 19).replaceAll(':', '')
const WORK_A = '核对投标人资格条款与资信证明'
const WORK_B = '核对评分表技术部分的响应要点'

await fetch(`${STUB}/reset`, { method: 'POST' })
const PROFILE = '界面联调·脚本替身模型（非真实模型）'
// 模型配置只给管理员（R2）。
let profile = (await api('GET', '/api/llm/profiles', undefined, { as: 'admin' }) ?? []).find((p) => p.name === PROFILE)
if (!profile) profile = await api('POST', '/api/llm/profiles', { name: PROFILE, format: 'openai', base_url: `${STUB}/v1`, api_key_env: 'STUB_LLM_KEY', model: 'stub-scripted', max_tokens: 1024, enabled: true, in_global_pool: false }, { as: 'admin' })
const created = await api('POST', '/api/tasks', { name: `界面联调·实时运行 ${stamp}`, description: '脚本替身模型驱动的实时运行检查（隔离测试库）',
  goal: '核对视频监控扩容工程招标文件的资格条款与评分要点', goals: ['资格条款逐条有资信证明', '评分表技术部分有响应要点'], timeout_seconds: 1800, llm_profile_ids: [profile.id], start: true })
const taskId = created.task?.id ?? created.id

const browser = await launch()
const { page, context, errors } = await newPage(browser)
const workList = page.getByRole('complementary', { name: '工作列表' })
const workButton = (name) => workList.getByRole('button').filter({ hasText: name })
const items = page.locator('[data-region="activity-scroll"] li')
const composer = page.getByLabel('引导内容')
// 活动条目元信息为「时间 · 工作 #id | 项目 · 类型」。
// 工具输入在条目详情中：展开 record_fact / 被拦下的工具调用，直到详情出现指定文字。
async function waitDetail(page, re, timeout = 60000) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    for (const b of await page.locator('[data-region="activity-scroll"] li button[aria-expanded="false"]').all()) await b.click().catch(() => {})
    if (await page.locator('[data-region="activity-scroll"] pre').filter({ hasText: re }).count()) return
    await page.waitForTimeout(1000)
  }
  throw new Error(`活动详情中未出现 ${re}`)
}
const nodesOf = () => items.evaluateAll((els) => els.map((e) => (/工作 #(\d+)/.exec(e.innerText)?.[1]) ?? 'project'))

await rec.check('运行开始：工作列表按状态分组并随活动流更新，全部实时活动持续到达', async () => {
  await page.goto(`${BASE}/tasks/${taskId}/monitor`)
  await workButton(WORK_A).waitFor({ timeout: 90000 })
  await workButton(WORK_B).waitFor({ timeout: 90000 })
  await page.getByRole('heading', { name: '全部实时活动' }).waitFor()
  await items.nth(2).waitFor({ timeout: 60000 })
  const running = await workList.innerText()
  assert(running.includes('正在执行'), `运行中的工作应在“正在执行”分组：${running.slice(0, 200)}`)
  await shot(page, 'L1-running-all')
  return `任务 #${taskId}；已收到 ${await items.count()} 条活动`
})

await rec.check('工作切换：活动按 node_id 精确过滤，地址 work 参数可后退恢复', async () => {
  await workButton(WORK_A).click()
  await page.waitForURL(/work=\d+/)
  const a = Number(new URL(page.url()).searchParams.get('work'))
  await page.getByRole('heading', { name: new RegExp(WORK_A) }).waitFor()
  await items.first().waitFor({ timeout: 30000 })
  const nodesA = await nodesOf()
  assert(nodesA.every((n) => n === String(a)), `工作 #${a} 只应显示自己的活动：${nodesA}`)
  await workButton(WORK_B).click()
  await page.waitForURL((u) => u.searchParams.get('work') !== String(a))
  const b = Number(new URL(page.url()).searchParams.get('work'))
  // 地址先变、列表后重绘：等工作 B 的标题出现，再等列表里不再有上一项工作的条目，否则读到的是旧画面。
  await page.getByRole('heading', { name: new RegExp(WORK_B) }).waitFor()
  await items.first().waitFor({ timeout: 30000 })
  await page.waitForFunction((id) => [...document.querySelectorAll('[data-region="activity-scroll"] li')].every((e) => (/工作 #(\d+)/.exec(e.innerText)?.[1] ?? 'project') === id), String(b), { timeout: 10000 }).catch(() => {})
  const nodesB = await nodesOf()
  assert(nodesB.every((n) => n === String(b)), `工作 #${b} 只应显示自己的活动：${nodesB}`)
  await page.evaluate(() => history.back())
  await page.waitForURL((u) => u.searchParams.get('work') === String(a))
  await page.getByRole('heading', { name: new RegExp(WORK_A) }).waitFor()
  await shot(page, 'L2-work-a')
  return `工作 #${a}：${nodesA.length} 条；工作 #${b}：${nodesB.length} 条；后退回到 #${a}`
})

await rec.check('引导：运行中的工作发送引导，模型下一轮收到并体现在该工作的活动中', async () => {
  const msg = `请优先核对一级资质证书有效期（${stamp}）`
  await composer.fill(msg)
  await page.getByRole('button', { name: '引导', exact: true }).click()
  await waitDetail(page, /收到引导：请优先核对一级资质证书有效期/)
  const st = await (await fetch(`${STUB}/state`)).json()
  assert(st.guidance.some((g) => g.includes(msg)), '替身模型应收到引导原文')
  assert(await composer.inputValue() === '', '发送成功后应清空输入')
  await shot(page, 'L3-steer')
  return '引导已进入 Worker 会话并作为活动显示'
})

await rec.check('暂停后恢复：暂停后输入区切换为“发送并恢复”，发送后工作恢复执行', async () => {
  const pane = page.locator('[data-region="activity-pane"]')
  await pane.getByRole('button', { name: '暂停' }).click()
  await pane.getByText('已暂停').first().waitFor({ timeout: 60000 })
  await page.getByRole('button', { name: '发送并恢复' }).waitFor()
  assert((await composer.getAttribute('placeholder'))?.includes('补充说明并恢复'), '暂停后输入提示应为恢复说明')
  await shot(page, 'L4-paused')
  const msg = `恢复后请补充评分表交叉核对（${stamp}）`
  await composer.fill(msg)
  await page.getByRole('button', { name: '发送并恢复' }).click()
  await waitDetail(page, /收到引导：恢复后请补充评分表交叉核对/)
  await pane.getByText('运行中').first().waitFor({ timeout: 30000 })
  await shot(page, 'L4-resumed')
  return '暂停 → 发送并恢复 → 替身模型收到恢复说明 → 工作重新执行'
})

await rec.check('活动流断开重连：显示重连状态与提示，恢复后继续接收活动并重读业务状态', async () => {
  await page.getByRole('button', { name: /全部实时活动/ }).click()
  await page.getByRole('heading', { name: '全部实时活动' }).waitFor()
  const before = await items.count()
  const reconnecting = page.locator('[data-region="activity-pane"]').getByText(/重连中|已断开|连接中/).first()
  if (process.env.E2E_RESTART) {
    execSync(process.env.E2E_RESTART, { stdio: 'ignore' })
    await reconnecting.waitFor({ timeout: 30000 }).catch(() => {}) // 重启很快时可能来不及截到中间态
  } else {
    await context.setOffline(true)
    await reconnecting.waitFor({ timeout: 30000 })
    await shot(page, 'L5-offline')
    await page.waitForTimeout(3000)
    await context.setOffline(false)
  }
  await page.getByText(/活动流已重连：正在重读项目状态/).first().waitFor({ timeout: 60000 })
  await page.getByText(/活动流已重连 \d+ 次/).waitFor()
  await page.locator('[data-region="activity-pane"]').getByText('已连接').first().waitFor({ timeout: 30000 })
  const at = await items.count()
  // 重启后在途工作要等旧进程的租约过期（90 秒）被回收后才继续，新活动最长约 2～3 分钟后到达。
  await page.waitForFunction((n) => document.querySelectorAll('[data-region="activity-scroll"] li').length > n, at, { timeout: process.env.E2E_RESTART ? 240000 : 60000 })
  await shot(page, 'L5-reconnected')
  return `${process.env.E2E_RESTART ? '重启服务' : '离线仿真'}；断开前 ${before} 条，重连后继续增加至 ${await items.count()} 条`
})

await rec.check('运行中无页面错误', async () => { assert(errors.length === 0, errors.join('\n')) })

await context.close()
await api('POST', `/api/tasks/${taskId}/pause`, undefined, { ok: [409] }).catch(() => {})
await browser.close()
process.exit(await rec.save())
