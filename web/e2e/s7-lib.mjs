// S7 浏览器验收的公共步骤。全部写操作走页面；这里的 api 调用只用于断言读取和环境准备（替身模型配置）。
import { writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { launch, newPage, api, BASE, OUT, assert, shot } from './lib.mjs'

export const STUB = process.env.E2E_STUB || 'http://127.0.0.1:18090'
export const AUTHOR = '撰写人甲'
export const REVIEWER = '评审人乙'

/** session 打开浏览器并记录外部请求：页面不得访问本机以外的任何地址。 */
export async function session(opts) {
  const browser = await launch()
  const { context, page, errors } = await newPage(browser, opts)
  const external = []
  context.on('request', (r) => {
    const host = new URL(r.url()).hostname
    if (!['127.0.0.1', 'localhost', ''].includes(host) && !r.url().startsWith('data:') && !r.url().startsWith('blob:')) external.push(r.url())
  })
  page.setDefaultTimeout(20000)
  const finish = async () => {
    assert(external.length === 0, `页面访问了本机以外的地址：${external.join(' ')}`)
    assert(errors.length === 0, `页面有未捕获错误：${errors.join(' | ')}`)
    await context.close()
    await browser.close()
  }
  return { browser, context, page, errors, external, finish }
}

/** step 按顺序执行；后面的步骤依赖前面的结果，所以失败立即停止并留下现场。 */
export function stepper(page) {
  let n = 0
  return async function step(name, fn) {
    n += 1
    const started = Date.now()
    try {
      const note = await fn()
      console.log(`PASS ${n}. ${name}${note ? ` — ${note}` : ''} (${Math.round((Date.now() - started) / 100) / 10}s)`)
    } catch (e) {
      console.log(`FAIL ${n}. ${name}\n${String(e?.message || e).slice(0, 1200)}`)
      await shot(page, `failure-${n}`).catch(() => {})
      await writeFile(join(OUT, `failure-${n}.txt`), await page.locator('body').innerText().catch(() => '')).catch(() => {})
      throw e
    }
  }
}

export async function setOperator(page, name) {
  await page.getByLabel('当前操作人', { exact: true }).fill(name)
}

/** 替身模型只是运行环境：用接口配置并标明，不计入“业务用户在页面上完成的步骤”。 */
export async function ensureStubProfile() {
  const name = '界面联调·脚本替身模型（非真实模型）'
  const list = await api('GET', '/api/llm/profiles') ?? []
  const profile = list.find((p) => p.name === name) ?? await api('POST', '/api/llm/profiles', {
    name, format: 'openai', base_url: `${STUB}/v1`, api_key_env: 'STUB_LLM_KEY', model: 'stub-scripted', max_tokens: 4096, enabled: true, in_global_pool: true,
  })
  await api('POST', '/api/llm/profiles/active', { id: profile.id })
  return profile
}

export async function waitFor(what, fn, { timeout = 120000, every = 500 } = {}) {
  const end = Date.now() + timeout
  let last
  while (Date.now() < end) {
    last = await fn()
    if (last) return last
    await new Promise((r) => setTimeout(r, every))
  }
  throw new Error(`等待超时：${what}`)
}

export { api, BASE, OUT, assert, shot }
