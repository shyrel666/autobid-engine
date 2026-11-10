import { useState } from 'react'
import { api, errorText } from '../../../api/client'
import type { CompanyCredential, CompanyPerformance, RequirementItem } from '../../../api/types'
import { Button, ErrorBanner, Field, Input, Modal, Notice, Select } from '../../../components/ui'

type Mode = 'credential' | 'performance' | 'original' | 'binding' | 'commitment' | 'quote'
const names: Record<Mode, string> = { credential: '新增资信', performance: '新增业绩', original: '上传证明原件', binding: '绑定要求证据', commitment: '新增承诺', quote: '录入报价' }

/** Existing D2 state machines, entered by a human from the browser. No OCR/adoption claims. */
export function EvidenceInputs({ taskId, requirements, credentials, performance, onSaved }: {
  taskId: number; requirements: RequirementItem[]; credentials: CompanyCredential[]; performance: CompanyPerformance[]; onSaved: (message: string) => void
}) {
  const [mode, setMode] = useState<Mode>()
  const [f, setF] = useState<Record<string, string>>({})
  const [file, setFile] = useState<File>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const field = (key: string, label: string, type = 'text') => <Field label={label} key={key}><Input type={type} value={f[key] || ''} onChange={e => setF({ ...f, [key]: e.target.value })} /></Field>
  const domain = f.domain === 'performance' ? 'performance' : 'credential'
  const owners = domain === 'credential' ? credentials.map(c => ({ id: c.id, title: c.title })) : performance.map(p => ({ id: p.id, title: p.project_name }))
  const ownerField = <><Field label="证据类别"><Select aria-label="证据类别" value={domain} onChange={e => setF({ ...f, domain: e.target.value, owner: '' })}><option value="credential">企业资信</option><option value="performance">项目业绩</option></Select></Field>
    <Field label="证据记录"><Select aria-label="证据记录" value={f.owner || ''} onChange={e => setF({ ...f, owner: e.target.value })}><option value="">请选择</option>{owners.map(o => <option key={o.id} value={o.id}>{o.title}</option>)}</Select></Field></>
  const requirementField = <Field label="关联要求"><Select aria-label="关联要求" value={f.requirement || ''} onChange={e => setF({ ...f, requirement: e.target.value })}><option value="">请选择</option>{requirements.map(r => <option key={r.id} value={r.id}>{r.code} {r.title}</option>)}</Select></Field>
  const save = async () => {
    setBusy(true); setError('')
    try {
      if (mode === 'credential') {
        if (!f.code?.trim() || !f.title?.trim() || !f.subject?.trim()) throw new Error('请填写编号、名称和证书主体')
        await api.createCredential({ code: f.code, category: 'qualification', title: f.title, subject_name: f.subject, subject_type: 'bidder', certificate_no: f.number, issuer: f.issuer, issued_on: f.issued || null, valid_until: f.valid || null, scope: f.scope })
      } else if (mode === 'performance') {
        if (!f.code?.trim() || !f.title?.trim() || !f.subject?.trim()) throw new Error('请填写编号、项目名称和合同主体')
        await api.savePerformance({ code: f.code, project_name: f.title, subject_name: f.subject, subject_type: 'bidder', contract_no: f.number, buyer: f.buyer, contract_date: f.issued || undefined, acceptance_date: f.valid || undefined, scope: f.scope, amount: f.amount || undefined, currency: 'CNY' })
      } else if (mode === 'original') {
        if (!file || !f.owner) throw new Error('请选择证据记录和证明材料原件')
        await api.uploadEvidenceOriginal(domain, Number(f.owner), file, { document_kind: f.kind, page_label: f.label || '', page_start: Number(f.start), page_end: Number(f.end) })
      } else if (mode === 'binding') {
        if (!f.owner || !f.requirement) throw new Error('请选择要求和对应的证据记录')
        await api.bindEvidence(taskId, Number(f.requirement), domain, Number(f.owner))
      } else if (mode === 'commitment') {
        if (!f.code?.trim() || !f.metric?.trim() || !f.ownerName?.trim() || !f.basis?.trim() || !f.value?.trim() || !f.threshold?.trim()) throw new Error('请填写承诺编号、指标、阈值、承诺值、责任人和能力依据')
        if ((f.metricType || 'other') !== 'price' && !f.requirement) throw new Error('非价格承诺必须关联一条招标要求')
        const threshold = Number(f.threshold), value = Number(f.value), limit = f.limit ? Number(f.limit) : undefined
        if (![threshold, value, ...(limit === undefined ? [] : [limit])].every(Number.isFinite)) throw new Error('阈值、承诺值和能力上限须为数字')
        await api.upsertCommitment(taskId, { code: f.code, requirement_item_id: Number(f.requirement) || undefined, metric: f.metricType || 'other', metric_label: f.metric, unit: f.unit || '', threshold_cmp: f.cmp || '>=', tender_threshold_raw: f.threshold, tender_threshold_value: threshold, committed_raw: f.value, committed_value: value, capability_limit: limit, capability_basis: f.basis, owner: f.ownerName, rationale: f.reason || '' })
      } else if (mode === 'quote') {
        if (!f.title?.trim() || !f.total?.trim() || !f.price?.trim() || !f.quantity?.trim() || !f.line?.trim()) throw new Error('请填写报价项目、单价、数量、行金额和总额')
        await api.createQuote(taskId, { lot: f.lot || '', declared_total: f.total, ceiling: f.ceiling || undefined, currency: 'CNY', note: f.reason || '', items: [{ line_no: 1, code: '1', name: f.title, unit_price: f.price, quantity: f.quantity, line_amount: f.line }] })
      }
      onSaved(`${mode ? names[mode] : '保存'}成功；请继续核对并确认。`); setMode(undefined)
    } catch (e) { setError(errorText(e)) } finally { setBusy(false) }
  }
  return <>
    <div className="flex flex-wrap gap-2">{(Object.keys(names) as Mode[]).map(m => <Button key={m} onClick={() => { setMode(m); setF({ domain: 'credential', kind: 'certificate', start: '1', end: '1', cmp: '>=' }); setFile(undefined); setError('') }}>{names[m]}</Button>)}</div>
    <Modal open={!!mode} title={mode ? names[mode] : ''} onClose={() => { if (!busy) setMode(undefined) }} footer={<><Button disabled={busy} onClick={() => setMode(undefined)}>取消</Button><Button variant="primary" loading={busy} onClick={() => void save()}>保存</Button></>}>
      <ErrorBanner error={error} />
      <div className="grid gap-3 md:grid-cols-2">
        {(mode === 'credential' || mode === 'performance') && <>
          {field('code', '证据编号')}{field('title', mode === 'credential' ? '资信名称' : '业绩项目名称')}{field('subject', '证据主体名称')}{field('number', mode === 'credential' ? '证书编号' : '合同编号')}
          {mode === 'credential' ? field('issuer', '签发机构') : field('buyer', '采购人')}
          {field('issued', mode === 'credential' ? '签发日期' : '合同日期', 'date')}{field('valid', mode === 'credential' ? '有效期至' : '验收日期', 'date')}{field('scope', '业务范围')}
          {mode === 'performance' && field('amount', '合同金额（元）')}
        </>}
        {(mode === 'original' || mode === 'binding') && ownerField}
        {(mode === 'binding' || mode === 'commitment') && requirementField}
        {mode === 'original' && <>
          <Notice>此处仅留存企业证明原件，不做 OCR 候选识别或自动核验。扫描材料也须人工核对页码、内容与证据主体，再运行可采性评估。</Notice>
          <Field label="原件文件"><input type="file" aria-label="原件文件" onChange={e => setFile(e.target.files?.[0])} /></Field>
          <Field label="证明材料类型"><Select aria-label="证明材料类型" value={f.kind} onChange={e => setF({ ...f, kind: e.target.value })}>{Object.entries({ certificate: '证书', contract: '合同', acceptance_report: '验收报告', social_security: '社保证明', product_catalog: '产品资料', other: '其他证明' }).map(([k, v]) => <option key={k} value={k}>{v}</option>)}</Select></Field>
          {field('label', '页码说明')}{field('start', '起始页', 'number')}{field('end', '结束页', 'number')}
        </>}
        {mode === 'commitment' && <>
          <Field label="指标类别"><Select aria-label="指标类别" value={f.metricType || 'other'} onChange={e => setF({ ...f, metricType: e.target.value })}>{Object.entries({ duration: '工期', warranty: '质保', sla_response: '响应时间', sla_onsite: '到场时间', sla_repair: '修复时间', availability: '可用率', team_size: '人员数量', price: '价格', other: '其他' }).map(([k, v]) => <option key={k} value={k}>{v}</option>)}</Select></Field>
          {field('code', '承诺编号')}{field('metric', '承诺指标')}{field('unit', '单位')}
          <Field label="阈值方向"><Select aria-label="阈值方向" value={f.cmp} onChange={e => setF({ ...f, cmp: e.target.value })}><option value=">=">不少于</option><option value="<=">不超过</option><option value="=">等于</option></Select></Field>
          {field('threshold', '招标阈值')}{field('value', '承诺值')}{field('limit', '能力上限（可选）')}{field('basis', '能力与资源依据')}{field('ownerName', '交付责任人')}{field('reason', '承诺说明')}
        </>}
        {mode === 'quote' && <>
          {field('lot', '报价分包（可空）')}{field('title', '报价项目')}{field('price', '单价（元）')}{field('quantity', '数量')}{field('line', '行金额（元）')}{field('total', '声明总额（元）')}{field('ceiling', '最高限价（元，可空）')}{field('reason', '报价说明')}
        </>}
      </div>
    </Modal>
  </>
}
