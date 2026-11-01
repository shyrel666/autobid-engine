// 浏览器端到端检查的公共部分：加载 Playwright、调用真实 API 构造隔离测试项目、记录检查结果。
// 不属于应用运行依赖，也不新增 npm 依赖：Playwright 模块与浏览器通过环境变量指定。
//   PLAYWRIGHT_MODULE   playwright 或 playwright-core 模块路径（缺省 require('playwright')）
//   BROWSER_EXECUTABLE  Chrome / Chromium 可执行文件（缺省用 Playwright 自带浏览器）
//   E2E_BASE            被测服务地址，缺省 http://127.0.0.1:18080（服务须连接隔离的测试库）
//   E2E_TOKEN_FILE      服务端 AUTOBID_API_TOKEN 所在文件（令牌不写入仓库与日志）
//   E2E_OUT             截图与结果输出目录
import { createRequire } from 'node:module'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { join } from 'node:path'

const require = createRequire(import.meta.url)
export const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright')
export const BASE = process.env.E2E_BASE || 'http://127.0.0.1:18080'
export const OUT = process.env.E2E_OUT || join(process.cwd(), 'e2e-out')
export const TOKEN = process.env.E2E_TOKEN_FILE ? (await readFile(process.env.E2E_TOKEN_FILE, 'utf8')).trim() : ''

export async function api(method, path, body, { token = TOKEN, ok = [] } = {}) {
  const headers = {}
  if (token) headers.Authorization = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const r = await fetch(BASE + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  const text = await r.text()
  const data = text ? JSON.parse(text) : null
  if (!r.ok && !ok.includes(r.status)) throw new Error(`${method} ${path} -> ${r.status}: ${text.slice(0, 300)}`)
  return data
}

const TENDER = `市公共安全视频监控扩容工程招标文件
第一章 投标人须知
1.1 投标截止时间：2026年12月20日9时30分（北京时间）
1.2 投标人资格要求
★1.2.1 投标人须具有安全技术防范工程设计施工一级资质，否则投标无效。
★1.2.2 投标人须提供近三年内经营活动中无重大违法记录的书面声明，否则投标无效。
- 1 -
第二章 技术要求
2.1 前端摄像机
★2.1.1 前端摄像机分辨率不低于 400 万像素，否则投标无效。
2.1.2 摄像机须支持 H.265 编码。
2.2 存储系统
★2.2.1 视频存储时间不少于 90 天，否则投标无效。
2.3 平台接入
2.3.1 系统平台须支持 GB/T 28181 协议接入。
- 2 -
第三章 服务要求
3.1 质保期不少于 3 年，故障响应时间不超过 2 小时。
3.2 投标人须提供项目实施进度计划。
- 3 -
`

/** sectionBody 逐条响应章节映射的要求，使规则核验可以通过。 */
export function sectionBody(detail, extra = '') {
  const reqs = detail.requirements.map((r) => `- 响应 ${r.code}：${r.title}。我方完全满足：${r.description || r.title}`)
  return `## ${detail.section.title}\n\n本章逐条响应 ${detail.requirements.map((r) => r.code).join('、') || '本章'}。${extra}\n\n${reqs.join('\n')}\n`
}

/**
 * seedProject 通过公开 API 构造测试项目。stage=writing：要求已确认、大纲已批准；
 * stage=ready：每章保存并定稿、矩阵与一致性核验完成，可以导出。
 */
export async function seedProject(stage, name) {
  const t = await api('POST', '/api/tasks', { name, description: '界面联调样例（隔离测试库）', goal: '完成视频监控扩容工程技术标的合规核对与编写', start: false })
  const id = t.task?.id ?? t.id
  await api('POST', `/api/tasks/${id}/documents`, { tender_no: 'SPJK-2026-018', title: '视频监控扩容工程招标文件', text: TENDER, wake_planner: false })
  const ex = await api('POST', `/api/tasks/${id}/requirements/extract`)
  for (const it of ex.requirements.items) {
    if (it.category === 'veto_item') await api('PUT', `/api/tasks/${id}/requirements/${it.id}/veto`, { status: 'passed', note: '已核对企业资质证书与声明函（联调样例）' })
  }
  const view = await api('GET', `/api/tasks/${id}/requirements`)
  await api('POST', `/api/tasks/${id}/requirements/confirm`, { epoch: view.manifest.epoch })
  const o = await api('POST', `/api/tasks/${id}/outlines`, {})
  const v = o.outline.version
  await api('POST', `/api/tasks/${id}/outlines/${v}/submit`)
  const approved = await api('POST', `/api/tasks/${id}/outlines/${v}/approve`)
  const sections = approved.sections.map((s) => ({ id: s.id, code: s.section_code, title: s.title }))
  if (stage === 'ready') {
    for (const s of sections) await saveSection(id, s.id, { approve: true })
    await api('POST', `/api/tasks/${id}/compliance/verify`, {})
    await api('POST', `/api/tasks/${id}/consistency/runs`)
  }
  return { id, outlineVersion: v, sections }
}

/** saveSection 以当前主版本为基线保存一条修订（模拟另一位编辑者），可选定稿。 */
export async function saveSection(taskId, sectionId, { extra = '', evidenceIds, approve = false } = {}) {
  const d = await api('GET', `/api/tasks/${taskId}/sections/${sectionId}`)
  const r = await api('POST', `/api/tasks/${taskId}/sections/${sectionId}/revisions`, {
    base_rev_no: d.section.current_rev_no, based_on_outline_version: d.section.outline_version,
    content_md: sectionBody(d, extra), summary_note: '联调样例正文', evidence_ids: evidenceIds,
  })
  if (approve) await api('POST', `/api/tasks/${taskId}/sections/${sectionId}/revisions/${r.revision.rev_no}/approve`)
  return r
}

export async function launch() {
  return chromium.launch({ headless: true, ...(process.env.BROWSER_EXECUTABLE ? { executablePath: process.env.BROWSER_EXECUTABLE } : {}) })
}

/** newPage 打开带令牌的上下文；viewport / DPR / 时区与视觉验收一致。 */
export async function newPage(browser, { token = TOKEN, width = 1440, height = 900, dpr = 1 } = {}) {
  const context = await browser.newContext({ viewport: { width, height }, deviceScaleFactor: dpr, colorScheme: 'light', locale: 'zh-CN', timezoneId: 'Asia/Shanghai', reducedMotion: 'reduce' })
  await context.addInitScript((t) => {
    try { if (!sessionStorage.getItem('e2e.init')) { sessionStorage.setItem('e2e.init', '1'); if (t) localStorage.setItem('autobid.token', t); else localStorage.removeItem('autobid.token') } } catch { /* ignore */ }
  }, token)
  const page = await context.newPage()
  const errors = []
  page.on('pageerror', (e) => errors.push(e.message))
  page.on('console', (m) => { if (m.type() === 'error' && !/401|409|422|500|Failed to load resource/.test(m.text())) errors.push(m.text()) })
  return { context, page, errors }
}

export function recorder(suite) {
  const results = []
  return {
    results,
    async check(name, fn) {
      const started = Date.now()
      try {
        const note = await fn()
        results.push({ name, ok: true, ms: Date.now() - started, note: note ?? '' })
        console.log(`PASS ${name}${note ? ` — ${note}` : ''}`)
      } catch (e) {
        results.push({ name, ok: false, ms: Date.now() - started, error: String(e?.stack || e).slice(0, 1500) })
        console.log(`FAIL ${name}\n  ${String(e?.message || e).split('\n')[0]}`)
      }
    },
    async save() {
      await mkdir(OUT, { recursive: true })
      await writeFile(join(OUT, `${suite}-results.json`), JSON.stringify({ suite, base: BASE, at: new Date().toISOString(), results }, null, 2) + '\n')
      const failed = results.filter((r) => !r.ok).length
      console.log(`${suite}: ${results.length - failed}/${results.length} passed`)
      return failed
    },
  }
}

export function assert(cond, msg) { if (!cond) throw new Error(msg) }
export const shot = async (page, name) => { await mkdir(OUT, { recursive: true }); await page.screenshot({ path: join(OUT, `${name}.png`) }) }
