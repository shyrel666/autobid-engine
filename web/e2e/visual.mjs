// V01～V07 视觉对照的实际截图（FRONTEND_REFACTOR_PLAN.md 第 10.3 节）：开发服务器的 /visual.html fixture，
// 与 docs/frontend-refactor/design/capture-manifest.json 相同的视口、DPR=2、浅色、zh-CN、Asia/Shanghai、减少动态效果。
// 输出 Vxx-actual.png 与 actual-geometry.json；像素差异由 docs/frontend-refactor/verification/compare.py 计算。
// 用法：VISUAL_BASE=http://127.0.0.1:5173 E2E_OUT=docs/frontend-refactor/verification node web/e2e/visual.mjs
import { mkdir, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { chromium } from './lib.mjs'

const BASE = process.env.VISUAL_BASE || 'http://127.0.0.1:5173'
const OUT = process.env.E2E_OUT || join(process.cwd(), 'visual-out')
const cases = [
  ['V01', '/tasks', 'expanded', 1440, 900],
  ['V02', '/tasks/101/monitor?work=201', 'expanded', 1440, 900],
  ['V03', '/llm', 'expanded', 1440, 900],
  ['V04', '/tasks/101/monitor?work=201', 'expanded', 1440, 900, 'expand-tool'],
  ['V05', '/tasks', 'collapsed', 1440, 900],
  ['V06', '/tasks/101/writing/2', 'collapsed', 1440, 900],
  ['V07', '/tasks/101/monitor?work=201', '', 390, 844],
]

await mkdir(OUT, { recursive: true })
const browser = await chromium.launch({ headless: true, ...(process.env.BROWSER_EXECUTABLE ? { executablePath: process.env.BROWSER_EXECUTABLE } : {}) })
const geometry = { browser: browser.version(), deviceScaleFactor: 2, captures: {} }
try {
  for (const [id, route, sidebar, width, height, action] of cases) {
    const context = await browser.newContext({ viewport: { width, height }, deviceScaleFactor: 2, colorScheme: 'light', locale: 'zh-CN', timezoneId: 'Asia/Shanghai', reducedMotion: 'reduce' })
    const page = await context.newPage()
    const failures = []
    page.on('pageerror', (e) => failures.push(e.message))
    await page.goto(`${BASE}/visual.html?route=${encodeURIComponent(route)}${sidebar ? `&sidebar=${sidebar}` : ''}`)
    await page.locator('[data-region="main-frame"]').waitFor()
    await page.waitForLoadState('networkidle')
    if (route.includes('monitor')) await page.locator('[data-region="activity-scroll"] li').nth(2).waitFor()
    if (route.includes('writing')) await page.locator('[data-region="writing"] h2').first().waitFor()
    if (action === 'expand-tool') {
      await page.locator('[data-region="activity-scroll"] li').filter({ has: page.locator('button[aria-expanded]') }).first().getByRole('button', { name: '展开详情' }).click()
      await page.locator('[data-region="activity-scroll"] pre').first().waitFor()
    }
    // 截图不含悬停与焦点：指针移到主容器外侧的 8px 边距，焦点移回文档。
    await page.mouse.move(width - 2, height - 2)
    await page.evaluate(() => { (document.activeElement)?.blur?.(); return document.fonts.ready })
    await page.waitForTimeout(300)
    const measured = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      regions: [...document.querySelectorAll('[data-region]')].map((el) => { const r = el.getBoundingClientRect(); return { name: el.dataset.region, x: r.x, y: r.y, width: r.width, height: r.height } }),
    }))
    if (failures.length) throw new Error(`${id}: ${failures.join('; ')}`)
    await page.screenshot({ path: join(OUT, `${id}-actual.png`), fullPage: false, animations: 'disabled', caret: 'hide' })
    geometry.captures[id] = { route, sidebar: sidebar || 'default', viewport: { width, height }, ...measured }
    console.log(`${id}: ${route} ${width}x${height}@2x scrollWidth=${measured.scrollWidth}`)
    await context.close()
  }
} finally {
  await browser.close()
}
await writeFile(join(OUT, 'actual-geometry.json'), JSON.stringify(geometry, null, 2) + '\n')
