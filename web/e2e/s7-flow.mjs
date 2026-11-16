// S7 全流程：一个业务用户只用浏览器，从新建项目走到严格最终稿。
// 判据：除断言读取和替身模型配置外，所有写操作都通过页面完成；需要接口才能完成的步骤就是产品缺口。
// 模型是脚本化替身，不是真实模型；OCR、Mermaid、WPS 是真实程序。
import { AUTHOR, BASE, OUT, REVIEWER, api, assert, ensureStubProfile, session, setOperator, shot, stepper, waitFor } from './s7-lib.mjs'
import { readFile } from 'node:fs/promises'
import { join } from 'node:path'

const { page, finish } = await session()
const step = stepper(page)
const modal = page.getByRole('dialog')
page.on('dialog', (d) => d.accept(process.env.E2E_PROMPT_TEXT || '已对照原件逐项复核（工程样本）'))
let taskID, cert
const BIDDER = '合成档案服务公司'

try {
  await step('环境：配置替身模型（非真实模型）', async () => { await ensureStubProfile() })

  await step('新建项目（默认带写作门禁）', async () => {
    await page.goto(`${BASE}/tasks`)
    await page.getByRole('button', { name: '新建项目', exact: true }).click()
    await page.getByLabel('项目名称 *', { exact: true }).fill('S7 全流程（替身模型，非真实模型）')
    await page.getByLabel('总目标 *', { exact: false }).fill('完成单分包技术标，逐条核对要求和证据')
    await page.getByRole('button', { name: '创建项目', exact: true }).click()
    await page.waitForURL(/\/tasks\/\d+\/documents/)
    taskID = Number(new URL(page.url()).pathname.split('/')[2])
    const task = await api('GET', `/api/tasks/${taskID}`)
    assert(task.grounded_writing_required === true, '新项目应默认开启写作门禁')
  })

  await step('上传招标原件，解析为候选，人工采用，刷新后保留', async () => {
    await page.reload()
    await page.getByLabel('选择后台处理原件', { exact: true }).setInputFiles('web/e2e/fixtures/s7-tender.md')
    await page.getByRole('button', { name: '上传原件', exact: true }).click()
    await page.getByText(/已留存原件 #/).waitFor()
    await page.getByLabel('附件招标编号', { exact: true }).fill('S7-001')
    await page.getByLabel('选择 s7-tender.md', { exact: true }).check()
    await page.getByRole('button', { name: '解析为候选（1）', exact: true }).click()
    await page.getByRole('button', { name: '核对候选', exact: true }).first().click({ timeout: 60000 })
    await modal.getByText('档案扫描', { exact: false }).first().waitFor()
    await modal.getByLabel('复核说明', { exact: false }).fill('已按合成招标原件逐项核对')
    await modal.getByRole('button', { name: /确认采用/ }).click()
    await modal.waitFor({ state: 'hidden' })
    await page.reload()
    const docs = await api('GET', `/api/tasks/${taskID}/documents`)
    assert(docs.length === 1, `采用后应有 1 份正式文档，实际 ${docs.length}`)
  })

  await step('扫描件走真实 OCR：候选显示未核验提示，人工拒绝后不入库', async () => {
    await page.getByLabel('选择后台处理原件', { exact: true }).setInputFiles('ingest/testdata/scan_tender_page.jpg')
    await page.getByRole('button', { name: '上传原件', exact: true }).click()
    await page.getByLabel('选择 scan_tender_page.jpg', { exact: true }).check({ timeout: 30000 })
    await page.getByRole('button', { name: '解析为候选（1）', exact: true }).click()
    await waitFor('OCR 候选就绪', async () => {
      const list = await api('GET', `/api/tasks/${taskID}/file-processing-records`)
      return list.find((r) => r.status === 'ready' && r.candidate_only)
    }, { timeout: 180000 })
    await page.reload()
    await page.getByRole('button', { name: '核对候选', exact: true }).first().click()
    await modal.getByText(/OCR/).first().waitFor()
    await shot(page, 'ocr-candidate')
    const body = await modal.innerText()
    assert(/未核验|人工|对照原件/.test(body), `OCR 候选应提示须人工核对：${body.slice(0, 200)}`)
    await modal.getByLabel('拒绝原因', { exact: false }).fill('工程样本，不纳入本项目')
    await modal.getByRole('button', { name: /拒绝/ }).click()
    await modal.waitFor({ state: 'hidden' })
    const docs = await api('GET', `/api/tasks/${taskID}/documents`)
    assert(docs.length === 1, '被拒绝的候选不得进入正式文档')
  })

  await step('提取要求、核对否决项、投标决策、确认清单', async () => {
    await page.goto(`${BASE}/tasks/${taskID}/requirements`)
    await page.getByRole('button', { name: '从招标文件提取', exact: true }).click()
    await page.getByText('提取与目标分解完成：', { exact: false }).waitFor()
    const vetoes = page.getByRole('button', { name: /^核对通过：/ })
    await vetoes.first().waitFor()
    const total = await vetoes.count()
    for (let i = 0; i < total; i++) {
      await vetoes.nth(i).click()
      await modal.getByLabel('证明材料（文件名与页码，或核对方式）').fill('合成验收材料第1页与条款逐项核对（仅工程样本）')
      await modal.getByRole('button', { name: '确定', exact: true }).click()
      await modal.waitFor({ state: 'hidden' })
    }
    await page.getByRole('button', { name: '决定投标', exact: true }).click()
    await page.getByPlaceholder('可选：投标决策备注或风险应对说明').fill('已核对本次单分包范围')
    await page.getByRole('button', { name: '确认决策', exact: true }).click()
    await page.getByRole('button', { name: '确认清单完整', exact: true }).click()
    await page.getByText('要求清单已确认完整', { exact: true }).waitFor()
    await page.reload()
    await page.getByText('清单已确认', { exact: false }).first().waitFor()
    const view = await api('GET', `/api/tasks/${taskID}/requirements`)
    assert(view.stats.veto.pending === 0, `否决项应全部核对：${JSON.stringify(view.stats.veto)}`)
    return `否决项 ${total} 项`
  })

  await step('录入资信、业绩、证明原件，绑定要求并评估可采性', async () => {
    await page.goto(`${BASE}/tasks/${taskID}/evidence`)
    await setOperator(page, AUTHOR)
    await page.getByRole('heading', { name: /证据、可采性与承诺台账/ }).waitFor()
    const entry = async (button, values, extra) => {
      await page.getByRole('button', { name: button, exact: true }).click()
      for (const [label, value] of Object.entries(values)) await modal.getByLabel(label, { exact: true }).fill(value)
      if (extra) await extra()
      await modal.getByRole('button', { name: '保存', exact: true }).click()
      await modal.waitFor({ state: 'hidden' })
    }
    await entry('新增资信', { 证据编号: 'S7-CERT', 资信名称: '质量管理体系认证证书', 证据主体名称: '合成档案服务公司', 证书编号: 'S7-QMS-001', 签发机构: '合成认证机构', 签发日期: '2025-01-01', 有效期至: '2028-12-31', 业务范围: '档案数字化服务' })
    await entry('新增业绩', { 证据编号: 'S7-PERF', 业绩项目名称: '档案扫描服务', 证据主体名称: '合成档案服务公司', 合同编号: 'S7-CONTRACT', 采购人: '合成档案馆', 合同日期: '2025-01-01', 验收日期: '2025-12-31', 业务范围: '档案数字化服务', '合同金额（元）': '100000' })
    await entry('上传证明原件', { 页码说明: '工程样本第1页' }, async () => {
      await modal.getByLabel('证据记录', { exact: true }).selectOption({ label: '质量管理体系认证证书' })
      await modal.getByLabel('原件文件', { exact: true }).setInputFiles('ingest/testdata/scan_tender_page.jpg')
      assert(await modal.getByText(/不做 OCR 候选识别或自动核验/).count(), '证明原件上传必须说明未做 OCR 核验')
    })
    for (const [kind, label] of [['contract', '合同第1页'], ['acceptance_report', '验收报告第1页']]) {
      await entry('上传证明原件', { 页码说明: label }, async () => {
        await modal.getByLabel('证据类别', { exact: true }).selectOption('performance')
        await modal.getByLabel('证据记录', { exact: true }).selectOption({ label: '档案扫描服务' })
        await modal.getByLabel('证明材料类型', { exact: true }).selectOption(kind)
        await modal.getByLabel('原件文件', { exact: true }).setInputFiles('ingest/testdata/scan_tender_page.jpg')
      })
    }
    const reqs = await api('GET', `/api/tasks/${taskID}/requirements`)
    cert = reqs.items.find((r) => /质量管理体系/.test(`${r.title} ${r.description}`))
    assert(cert, '夹具的资质要求未被提取')
    await entry('绑定要求证据', {}, async () => {
      await modal.getByLabel('证据记录', { exact: true }).selectOption({ label: '质量管理体系认证证书' })
      await modal.getByLabel('关联要求', { exact: true }).selectOption(String(cert.id))
    })
    // 技术与交付要求用同类业绩作证明：规则不能自动判定，要走人工复核，这正是门禁的本意。
    const proven = reqs.items.filter((x) => /技术方案|交付方案|数据交付/.test(x.title))
    for (const r of proven) {
      await entry('绑定要求证据', {}, async () => {
        await modal.getByLabel('证据类别', { exact: true }).selectOption('performance')
        await modal.getByLabel('证据记录', { exact: true }).selectOption({ label: '档案扫描服务' })
        await modal.getByLabel('关联要求', { exact: true }).selectOption(String(r.id))
      })
    }
    // 每条要求都要有可采性结论，缺评估会阻断导出。
    for (const r of [cert, ...proven]) {
      await page.getByRole('row').filter({ hasText: r.code }).getByRole('button', { name: '评估', exact: true }).click()
      await page.getByText(`已评估 ${r.code}`, { exact: true }).waitFor()
      const manual = page.getByRole('row').filter({ hasText: r.code }).getByRole('button', { name: '人工复核', exact: true })
      if (await manual.count()) {
        await manual.click()
        await page.getByText(`已人工复核 ${r.code}`, { exact: true }).waitFor()
      }
    }
    await page.getByRole('button', { name: '计算客观分', exact: true }).click()
    await page.getByText('客观分已计算', { exact: true }).waitFor()
    await page.reload()
    await shot(page, 'evidence')
  })

  await step('录入并确认承诺、录入并确认报价', async () => {
    const entry = async (button, values, extra) => {
      await page.getByRole('button', { name: button, exact: true }).click()
      for (const [label, value] of Object.entries(values)) await modal.getByLabel(label, { exact: true }).fill(value)
      if (extra) await extra()
      await modal.getByRole('button', { name: '保存', exact: true }).click()
      await modal.waitFor({ state: 'hidden' })
    }
    const reqs = await api('GET', `/api/tasks/${taskID}/requirements`)
    const delivery = reqs.items.find((r) => /数据交付/.test(r.title))
    assert(delivery, '夹具的交付要求未被提取')
    await entry('新增承诺', { 承诺编号: 'S7-DELIVERY', 承诺指标: '目录索引交付', 单位: '份', 招标阈值: '1', 承诺值: '1', 能力与资源依据: '本项目按批次配置目录复核岗位', 交付责任人: '合成交付负责人', 承诺说明: '每批交付目录索引与备份清单' }, async () => {
      await modal.getByLabel('关联要求', { exact: true }).selectOption(String(delivery.id))
    })
    await page.getByRole('row').filter({ hasText: 'S7-DELIVERY' }).getByRole('button', { name: '确认', exact: true }).click()
    await page.getByText('已确认 S7-DELIVERY', { exact: true }).waitFor()
    await entry('录入报价', { 报价项目: '档案扫描服务', '单价（元）': '10000', 数量: '1', '行金额（元）': '10000', '声明总额（元）': '10000', '最高限价（元，可空）': '20000' })
    const quote = page.getByRole('row').filter({ hasText: /^v1/ })
    await quote.getByRole('button', { name: '校验', exact: true }).click()
    await page.getByText('已校验 v1', { exact: true }).waitFor()
    await quote.getByRole('button', { name: '确认', exact: true }).click()
    await page.getByText('已确认 v1', { exact: true }).waitFor()
    await page.reload()
    await shot(page, 'evidence-complete')
  })

  await step('生成大纲、提交审批、批准', async () => {
    await page.goto(`${BASE}/tasks/${taskID}/outline`)
    await page.getByRole('button', { name: /生成.*(大纲|目录)/ }).first().click()
    await page.getByRole('button', { name: '提交审批', exact: true }).click()
    await page.getByRole('button', { name: '批准大纲', exact: true }).click()
    await page.getByText('大纲已批准', { exact: false }).first().waitFor()
    await page.reload()
    await shot(page, 'outline')
  })

  const outline = await api('GET', `/api/tasks/${taskID}/outlines`)
  const version = outline.approved_version ?? outline.outlines?.find((o) => o.status === 'approved')?.version
  const detail = await api('GET', `/api/tasks/${taskID}/outlines/${version}`)
  const sections = detail.sections
  assert(sections.length > 0, '批准的大纲没有章节')
  const commitment = (await api('GET', `/api/tasks/${taskID}/commitments`))[0]

  for (const s of sections) {
    await step(`故事板：${s.section_code} ${s.title}，作者提交，评审人批准`, async () => {
      await page.goto(`${BASE}/tasks/${taskID}/writing/${s.id}`)
      await setOperator(page, AUTHOR)
      await page.getByRole('button', { name: '编写故事板', exact: true }).click()
      await modal.getByLabel('核心论点', { exact: false }).fill(`${s.title}按档案扫描与目录索引的交付节奏组织，让采购人逐批验收`)
      for (const area of await modal.getByLabel(/^我方做法/).all()) await area.fill('按目录逐项登记并在扫描后复核图像清晰度')
      for (const area of await modal.getByLabel(/^采购人收益/).all()) await area.fill('采购人可按批次核对目录索引和备份清单')
      for (const box of await modal.getByRole('checkbox', { name: /^引用 / }).all()) await box.check()
      await modal.getByRole('button', { name: '保存故事板', exact: true }).click()
      await modal.waitFor({ state: 'hidden' })
      await page.getByRole('button', { name: '提交粉队评审', exact: true }).click()
      await page.getByRole('button', { name: '粉队批准', exact: true }).waitFor()
      await page.getByRole('button', { name: '粉队批准', exact: true }).click()
      await page.getByText('粉队批准人不能是故事板的作者或提交人', { exact: false }).first().waitFor()
      await setOperator(page, REVIEWER)
      await page.getByRole('button', { name: '粉队批准', exact: true }).click()
      await page.getByText('已通过粉队评审', { exact: true }).first().waitFor()
    })
  }

  const [first, ...rest] = sections
  await step(`AI 派发：${first.section_code}（替身模型，真实作业链路）`, async () => {
    await page.goto(`${BASE}/tasks/${taskID}/writing/${first.id}`)
    await setOperator(page, AUTHOR)
    await page.getByRole('button', { name: 'AI 撰写初稿', exact: true }).click()
    await modal.getByRole('button', { name: '派发', exact: true }).click()
    await waitFor('AI 写回修订', async () => {
      const d = await api('GET', `/api/tasks/${taskID}/sections/${first.id}`)
      return d.section.current_rev_no > 0
    })
    await page.reload()
  })

  for (const s of rest) {
    await step(`人工撰写：${s.section_code} ${s.title}，含受控引用`, async () => {
      await page.goto(`${BASE}/tasks/${taskID}/writing/${s.id}`)
      await setOperator(page, AUTHOR)
      await page.getByRole('button', { name: '编辑正文', exact: true }).click()
      const area = page.getByLabel('章节正文（Markdown）', { exact: true })
      const before = await api('GET', `/api/tasks/${taskID}/sections/${s.id}`)
      const certSection = before.requirements.some((r) => r.id === cert.id)
      // 证书名是企业事实，必须和证据绑定在同一句里：引用单独成段时展开成标准表述，所以这一句不手写。
      const plain = before.requirements.filter((r) => r.id !== cert.id).map((r) => `本章响应 ${r.code}：${r.title}。`)
      const lead = `## ${s.title}\n\n档案扫描按目录逐项登记，扫描后进行图像清晰度复核。数据交付提供目录索引和备份清单，按批次执行验收。\n\n${plain.join('\n\n')}\n\n`
      await area.fill(lead)
      const refs = page.getByRole('group', { name: '插入受控引用' }).getByRole('button')
      await refs.filter({ hasText: certSection ? cert.code : 'S7-DELIVERY' }).click()
      await page.getByRole('button', { name: '保存', exact: true }).click()
      await page.getByText(/已保存为修订/).first().waitFor()
      const d = await api('GET', `/api/tasks/${taskID}/sections/${s.id}`)
      assert(!/\[\[(COMMIT|EVID):/.test(d.current_revision.content_md), '保存后受控引用应展开为标准表述')
      if (certSection) assert(/S7-QMS-001/.test(d.current_revision.content_md), '证据引用应展开出证书编号')
      else assert(d.current_revision.commitment_ids.includes(commitment.id), '修订应记录引用的承诺')
    })
  }

  for (const s of sections) {
    await step(`写作核验与定稿：${s.section_code}，定稿人不能是作者`, async () => {
      await page.goto(`${BASE}/tasks/${taskID}/writing/${s.id}`)
      await setOperator(page, REVIEWER)
      await page.getByRole('button', { name: '运行写作核验', exact: true }).click()
      await page.getByText(/写作核验：通过/).first().waitFor()
      await page.getByRole('button', { name: '批准定稿', exact: true }).click()
      await page.getByText(/已批准定稿/).first().waitFor()
    })
  }

  await step(`图表：${first.section_code} 提交，真实 Mermaid 渲染，刷新后保留`, async () => {
    await page.goto(`${BASE}/tasks/${taskID}/writing/${first.id}?panel=charts`)
    await page.getByRole('button', { name: '生成图表', exact: true }).click()
    await modal.getByLabel('题注', { exact: true }).fill('档案数字化服务总体架构')
    await modal.getByRole('button', { name: '提交渲染', exact: true }).click()
    await modal.waitFor({ state: 'hidden' })
    await waitFor('图表渲染完成', async () => {
      const charts = await api('GET', `/api/tasks/${taskID}/charts?section_id=${first.id}`)
      return (charts.charts ?? charts).some((c) => c.render_status === 'rendered')
    }, { timeout: 180000 })
    await page.reload()
    await page.getByRole('img', { name: /档案数字化服务总体架构/ }).first().waitFor()
    await shot(page, 'chart')
  })

  await step('响应矩阵与一致性核验', async () => {
    await page.goto(`${BASE}/tasks/${taskID}/matrix`)
    await page.getByRole('button', { name: '核验全部章节', exact: true }).click()
    await page.getByText(/核验 \d+ 行：通过 \d+，未通过 0/).first().waitFor()
    await page.goto(`${BASE}/tasks/${taskID}/consistency`)
    await page.getByRole('button', { name: '重检矩阵并核验', exact: true }).click()
    await waitFor('一致性运行完成', async () => {
      const r = await api('GET', `/api/tasks/${taskID}/consistency/runs`)
      return (r.runs ?? r).length > 0
    })
    await page.reload()
    // D4：一致性以承诺台账为基准，按当前规则版本运行；页面单列“与承诺台账不一致”。
    const latest = await api('GET', `/api/tasks/${taskID}/consistency/findings`)
    assert(latest.current && latest.run.rule_version === 'consistency-v2', `一致性运行应按当前规则：${JSON.stringify(latest.run).slice(0, 300)}`)
    assert(latest.run.summary.ledger_commitments >= 1, '一致性核验应读到已确认的承诺台账')
    assert(latest.run.ledger_mismatches === 0, `受控引用渲染的正文不应与台账不一致：${JSON.stringify(latest.findings).slice(0, 400)}`)
    await page.getByRole('region', { name: '与承诺台账不一致' }).getByText(/已确认的承诺为基准/).waitFor()
    await shot(page, 'consistency')
  })

  let jobs
  await step('导出门禁全部通过后，提交严格最终稿 DOCX + PDF（真实文档引擎）', async () => {
    await page.goto(`${BASE}/tasks/${taskID}/export`)
    const ready = await api('GET', `/api/tasks/${taskID}/export-readiness`)
    assert(ready.ready, `导出门禁未通过：${JSON.stringify(ready.blockers).slice(0, 800)}`)
    await page.getByRole('radio', { name: /最终稿/ }).check()
    // D4：最终稿的封面与文档属性都用投标人名称，不填不能提交。
    await page.getByRole('button', { name: /提交导出|开始导出|锁定/ }).first().click()
    await page.getByText('最终稿请填写封面投标人名称', { exact: false }).first().waitFor()
    await page.getByLabel(/封面投标人/).fill(BIDDER)
    await page.getByRole('button', { name: /提交导出|开始导出|锁定/ }).first().click()
    jobs = await waitFor('导出完成', async () => {
      const list = await api('GET', `/api/tasks/${taskID}/exports`)
      const all = list.jobs ?? list
      if (all.some((j) => j.status === 'failed')) throw new Error(`导出失败：${JSON.stringify(all).slice(0, 600)}`)
      return all.length >= 2 && all.every((j) => j.status === 'succeeded') ? all : undefined
    }, { timeout: 300000 })
    assert(jobs.every((j) => j.business_state === 'final_ready'), `应为最终稿：${JSON.stringify(jobs.map((j) => j.business_state))}`)
    await page.reload()
    await shot(page, 'export')
  })

  await step('页面下载两个文件，内容是真实 DOCX / PDF', async () => {
    const group = page.getByRole('region', { name: /导出请求组/ }).first()
    const buttons = group.getByRole('button', { name: /^最终稿 / })
    await buttons.first().waitFor()
    const count = await buttons.count()
    assert(count === 2, `最终稿应有 DOCX 和 PDF 两个下载入口，实际 ${count} 个`)
    const seen = new Set()
    for (let i = 0; i < count; i++) {
      const [dl] = await Promise.all([page.waitForEvent('download'), buttons.nth(i).click()])
      const name = dl.suggestedFilename()
      const path = join(OUT, name)
      await dl.saveAs(path)
      const b = await readFile(path)
      const magic = b.subarray(0, 4).toString('latin1')
      assert(b.length > 2000, `下载文件过小：${name} ${b.length} 字节`)
      if (name.endsWith('.pdf')) { assert(magic === '%PDF', `${name} 不是 PDF`); seen.add('pdf') }
      if (name.endsWith('.docx')) { assert(magic.startsWith('PK'), `${name} 不是 DOCX`); seen.add('docx') }
      console.log(`  下载 ${name} ${b.length} 字节`)
    }
    assert(seen.has('pdf') && seen.has('docx'), `应下载到 DOCX 和 PDF：${[...seen]}`)
    return [...seen].join(' + ')
  })

  let rid
  await step('D4 成稿回读：章节页码引用逐处与 PDF 版面一致，文档属性只含投标人', async () => {
    const reqs = await api('GET', `/api/tasks/${taskID}/export-requests`)
    const q = reqs.requests[0]
    rid = q.id
    assert(q.policy_version === 'export-final/v2', `最终稿策略应为 v2：${q.policy_version}`)
    let refs = 0
    for (const j of q.jobs) {
      const sc = j.self_check
      assert(sc, `${j.format} 没有回读自检记录`)
      assert(sc.page_refs > 0 && sc.page_refs_verified === sc.page_refs && sc.page_ref_issues === 0 && sc.verified_against === 'pdf',
        `${j.format} 页码引用未全部核对：${JSON.stringify(sc)}`)
      assert(sc.toc_issues === 0 && sc.toc_entries > 0 && sc.layout_checked, `${j.format} 目录或版面未核对：${JSON.stringify(sc)}`)
      assert(sc.metadata_checked && sc.metadata_issues === 0 && sc.author === BIDDER, `${j.format} 文档属性：${JSON.stringify(sc)}`)
      refs = sc.page_refs
    }
    const pkg = q.jobs[0].package_snapshot
    return `页码引用 ${refs} 处，评分索引 ${pkg.score_index?.length ?? 0} 项，承诺 ${pkg.commitments?.length ?? 0} 条，证明材料 ${pkg.evidence?.length ?? 0} 件`
  })

  await step('D4 提交检查清单：系统项按回读判定，必须写明操作人，逐项处理后就绪，刷新后保留', async () => {
    const list = page.getByRole('region', { name: `提交检查清单（请求组 #${rid}）` })
    await list.getByRole('button', { name: '提交检查清单', exact: true }).click()
    const cl = (await api('GET', `/api/tasks/${taskID}/export-requests/${rid}/submission`)).checklist
    assert(cl.final && cl.current && !cl.ready, `清单初始状态：${JSON.stringify(cl).slice(0, 400)}`)
    const failed = cl.items.filter((it) => it.status === 'fail')
    assert(failed.length === 0, `系统项不通过：${JSON.stringify(failed)}`)
    for (const key of ['final_draft', 'current_content', 'bidder_name', 'toc_pages', 'layout', 'page_numbers', 'metadata']) {
      await list.locator(`[data-check="${key}"]`).getByText('通过', { exact: true }).waitFor()
      assert(await list.locator(`[data-check="${key}"]`).getByRole('button').count() === 0, `系统判定项 ${key} 不应有手工操作`)
    }
    const todo = cl.items.filter((it) => it.confirmable)
    assert(todo.some((it) => it.key === 'own_template' && it.status === 'attention'), '使用内置模板应要求确认知悉')
    // 没有操作人不能确认。
    await setOperator(page, '')
    const cover = todo.find((it) => it.key === 'cover_check')
    await list.getByRole('button', { name: `确认：${cover.label}`, exact: true }).click()
    await modal.getByRole('button', { name: '确认', exact: true }).click()
    await modal.getByText('请先在页面顶部填写“当前操作人”', { exact: false }).waitFor()
    await modal.getByRole('button', { name: '取消', exact: true }).click()
    await modal.waitFor({ state: 'hidden' })
    await setOperator(page, REVIEWER)
    for (const it of todo) {
      const row = list.locator(`[data-check="${it.key}"]`)
      if (it.kind === 'auto') {
        await row.getByRole('button', { name: `确认知悉：${it.label}`, exact: true }).click()
        // 知悉必须写明理由，空着不能提交。
        assert(await modal.getByRole('button', { name: '确认知悉', exact: true }).isDisabled(), '知悉类不写说明不应能提交')
        await modal.getByLabel(/^说明/).fill('工程验收样本：接受该提示')
        await modal.getByRole('button', { name: '确认知悉', exact: true }).click()
      } else if (/^platform_/.test(it.key)) {
        // 验收环境没有真实交易平台：如实标为不适用，而不是确认。
        await row.getByRole('button', { name: `不适用：${it.label}`, exact: true }).click()
        await modal.getByLabel(/^说明/).fill('工程验收环境没有接入交易平台，未做导入与上传')
        await modal.getByRole('button', { name: '标为不适用', exact: true }).click()
      } else {
        await row.getByRole('button', { name: `确认：${it.label}`, exact: true }).click()
        await modal.getByRole('button', { name: '确认', exact: true }).click()
      }
      await modal.waitFor({ state: 'hidden' })
    }
    await list.getByText('清单已全部处理', { exact: true }).waitFor()
    await shot(page, 'submission')
    await page.reload()
    const again = page.getByRole('region', { name: `提交检查清单（请求组 #${rid}）` })
    await again.getByText('清单已全部处理', { exact: true }).waitFor()
    const after = await api('GET', `/api/tasks/${taskID}/export-requests/${rid}/submission`)
    assert(after.checklist.ready && after.events.length === todo.length, `刷新后清单应保持就绪并留有 ${todo.length} 条记录：${after.events.length}`)
    assert(after.events.every((e) => e.actor === REVIEWER), '确认记录应写明操作人')
    assert(!/已提交|提交成功/.test(await again.innerText()), '清单就绪不能说成已提交')
    return `${todo.length} 项人工处理（其中平台导入 / 上传标为不适用）`
  })

  await step('D4 导出后再改正文：这份成稿显示已过期，不能再确认', async () => {
    const s = rest[0] ?? first
    await page.goto(`${BASE}/tasks/${taskID}/writing/${s.id}`)
    await setOperator(page, AUTHOR)
    await page.getByRole('button', { name: '编辑正文', exact: true }).click()
    const area = page.getByLabel('章节正文（Markdown）', { exact: true })
    await area.fill(`${await area.inputValue()}\n\n各批次交付物以目录索引为准逐项清点。`)
    await page.getByRole('button', { name: '保存', exact: true }).click()
    await page.getByText(/已保存为修订/).first().waitFor()
    await page.goto(`${BASE}/tasks/${taskID}/export`)
    const list = page.getByRole('region', { name: `提交检查清单（请求组 #${rid}）` })
    await list.getByText('成稿已过期', { exact: true }).waitFor()
    await list.getByRole('button', { name: '提交检查清单', exact: true }).click()
    await list.locator('[data-check="current_content"]').getByText('不通过', { exact: true }).waitFor()
    assert(await list.getByRole('button', { name: /^(确认|确认知悉|未通过|不适用)：/ }).count() === 0, '过期的成稿不应再提供确认操作')
    const cl = (await api('GET', `/api/tasks/${taskID}/export-requests/${rid}/submission`)).checklist
    assert(!cl.current && !cl.ready && cl.stale_reason, `过期清单：${JSON.stringify(cl).slice(0, 300)}`)
    await shot(page, 'submission-stale')
    return cl.stale_reason
  })

  await finish()
  console.log('S7 FLOW COMPLETE（模型为替身，OCR / Mermaid / 文档引擎为真实程序）')
} catch (e) {
  await page.context().browser()?.close().catch(() => {})
  throw e
}
