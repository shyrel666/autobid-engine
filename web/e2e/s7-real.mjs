// S7 真实招标文件：一份公开招标 DOCX（开发集，不入库）从上传走到大纲批准。
// 真实文件要求数量大，这里不走到导出；要证明的是解析、提取、清单和大纲在真实规模下走得通。
import { BASE, api, assert, session, shot, stepper, waitFor } from './s7-lib.mjs'

const file = process.env.E2E_REAL_TENDER
const name = file.split(/[\\/]/).pop()
const { page, finish } = await session()
const step = stepper(page)
const modal = page.getByRole('dialog')
let taskID

await step('新建项目，上传真实招标文件，解析为候选', async () => {
  await page.goto(`${BASE}/tasks`)
  await page.getByRole('button', { name: '新建项目', exact: true }).click()
  await page.getByLabel('项目名称 *', { exact: true }).fill('S7 真实招标文件（开发集）')
  await page.getByLabel('总目标 *', { exact: false }).fill('按招标文件完成技术标')
  await page.getByRole('button', { name: '创建项目', exact: true }).click()
  await page.waitForURL(/\/tasks\/\d+\/documents/)
  taskID = Number(new URL(page.url()).pathname.split('/')[2])
  await page.reload()
  await page.getByLabel('选择后台处理原件', { exact: true }).setInputFiles(file)
  await page.getByRole('button', { name: '上传原件', exact: true }).click()
  await page.getByText(/已留存原件 #/).waitFor({ timeout: 60000 })
  await page.getByLabel('附件招标编号', { exact: true }).fill('S7-REAL')
  await page.getByLabel(`选择 ${name}`, { exact: true }).check()
  await page.getByRole('button', { name: '解析为候选（1）', exact: true }).click()
  await waitFor('候选就绪', async () => (await api('GET', `/api/tasks/${taskID}/file-processing-records`)).some((r) => r.status === 'ready'), { timeout: 180000 })
  await page.reload()
})

await step('核对候选并采用', async () => {
  await page.getByRole('button', { name: /^核对候选/ }).first().click()
  await modal.getByLabel('复核说明', { exact: false }).fill('开发集文件，工程验收')
  await modal.getByRole('button', { name: /确认采用/ }).click()
  await modal.waitFor({ state: 'hidden', timeout: 60000 })
  const docs = await api('GET', `/api/tasks/${taskID}/documents`)
  assert(docs.length === 1, '采用后应有 1 份正式文档')
  return `${docs[0].page_count} 页`
})

await step('提取要求，逐条核对否决项，决定投标，确认清单', async () => {
  await page.goto(`${BASE}/tasks/${taskID}/requirements`)
  await page.getByRole('button', { name: '从招标文件提取', exact: true }).click()
  await page.getByText('提取与目标分解完成：', { exact: false }).waitFor({ timeout: 120000 })
  const vetoes = page.getByRole('button', { name: /^核对通过：/ })
  await vetoes.first().waitFor()
  const total = await vetoes.count()
  for (let i = 0; i < total; i++) {
    await vetoes.nth(i).click()
    await modal.getByLabel('证明材料（文件名与页码，或核对方式）').fill('开发集工程验收，未做真实资格审定')
    await modal.getByRole('button', { name: '确定', exact: true }).click()
    await modal.waitFor({ state: 'hidden' })
  }
  await page.getByRole('button', { name: '决定投标', exact: true }).click()
  await page.getByRole('button', { name: '确认决策', exact: true }).click()
  await page.getByRole('button', { name: '确认清单完整', exact: true }).click()
  await page.getByText('要求清单已确认完整', { exact: true }).waitFor({ timeout: 60000 })
  const view = await api('GET', `/api/tasks/${taskID}/requirements`)
  await shot(page, 'real-requirements')
  return `要求 ${view.items.length} 条，否决项 ${total} 项`
})

await step('生成大纲、提交审批、批准，刷新后保留', async () => {
  await page.goto(`${BASE}/tasks/${taskID}/outline`)
  await page.getByRole('button', { name: /生成.*(大纲|目录)/ }).first().click()
  await page.getByRole('button', { name: '提交审批', exact: true }).click({ timeout: 60000 })
  await page.getByRole('button', { name: '批准大纲', exact: true }).click({ timeout: 60000 })
  await page.getByText('大纲已批准', { exact: false }).first().waitFor({ timeout: 60000 })
  await page.reload()
  const list = await api('GET', `/api/tasks/${taskID}/outlines`)
  const version = list.approved_version ?? list.outlines?.find((o) => o.status === 'approved')?.version
  const detail = await api('GET', `/api/tasks/${taskID}/outlines/${version}`)
  await shot(page, 'real-outline')
  return `${detail.sections.length} 个章节`
})

await finish()
console.log('S7 REAL TENDER COMPLETE（开发集，只到大纲批准）')
