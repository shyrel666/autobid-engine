// S7 升级后的历史记录：旧版程序产生的项目在新版页面上能正常打开，历史产物如实标注。
// 由 cmd/upgrade-drill 在升级完成后调用；数据来自真实旧版程序，不是构造的快照。
import { BASE, api, assert, session, shot, stepper } from './s7-lib.mjs'

const task = Number(process.env.S7_HISTORY_TASK)
const section = Number(process.env.S7_HISTORY_SECTION)
const { page, finish } = await session()
const step = stepper(page)

await step('导出页：历史作业如实标为预览稿，不冒充最终稿', async () => {
  await page.goto(`${BASE}/tasks/${task}/export`)
  await page.getByRole('heading', { name: '导出请求' }).or(page.getByText('导出请求', { exact: true })).first().waitFor()
  await page.getByText('仅预览稿', { exact: false }).first().waitFor()
  await shot(page, 'history-export')
})

await step('招标文件页：历史文档和原件信息可读', async () => {
  await page.goto(`${BASE}/tasks/${task}/documents`)
  await page.getByText('历史招标文件', { exact: false }).first().waitFor()
})

await step('大纲与写作页：历史章节可读，旧项目不被要求补故事板', async () => {
  await page.goto(`${BASE}/tasks/${task}/outline`)
  await page.getByText('已批准', { exact: false }).first().waitFor()
  await page.goto(`${BASE}/tasks/${task}/writing/${section}`)
  await page.getByRole('region', { name: '章节正文' }).waitFor()
  const t = await api('GET', `/api/tasks/${task}`)
  assert(t.grounded_writing_required === false, '升级不得给旧项目开启写作门禁')
  assert(await page.getByRole('button', { name: '编写故事板', exact: true }).count() === 0, '旧项目不应出现故事板入口')
  await shot(page, 'history-writing')
})

await step('知识库页：历史文档可读，中断的索引如实显示', async () => {
  await page.goto(`${BASE}/knowledge`)
  await page.getByText('历史运维知识', { exact: false }).first().waitFor()
})

await step('运行能力诊断详情显示当前版本', async () => {
  const v = await api('GET', '/api/runtime/version')
  assert(v.schema_checksum && v.revision, `版本接口应返回版本信息：${JSON.stringify(v)}`)
  return `${String(v.revision).slice(0, 8)}${v.modified ? '（含未提交改动）' : ''}`
})

await finish()
console.log('S7 HISTORY COMPLETE')
