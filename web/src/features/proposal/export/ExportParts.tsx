// Phase 6B：章节写作视图中的「章节图表」卡片，以及「⑥ 导出成稿」视图（门禁明细、格式与模板、发起导出、任务进度、历史与下载）。
// 全部数据来自 server/export.go 的真实路由；门禁、快照固化与 LibreOffice / mmdc 调用都在后端。

import { useEffect, useState } from 'react'
import {
  AlertTriangle, CheckCircle2, Download, FileDown, FileText, ImageOff, Link2, Lock, RefreshCw, ShieldAlert, Trash2, Upload, Wand2,
} from 'lucide-react'
import { api, errorText, ApiError, saveBlob } from '../../../api/client'
import type {
  ChartType, ExportFormat, ExportJob, ExportPreflight, ExportReadiness, ExportWarning, ProposalChart,
} from '../../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Modal, Pre, Select, Spinner, TextArea, cx, useToast } from '../../../components/ui'
import type { Tone } from '../../../components/ui'
import { useAsync, useInterval } from '../../../hooks/useAsync'
import {
  chartRenderLabel, chartTypeLabel, exportBlockerLabel, exportStatusLabel, exportWarningLabel, fmtBytes, fmtTime,
} from '../../../lib/format'

// ---------------------------------------------------------------------------
// 章节图表
// ---------------------------------------------------------------------------

const chartExamples: Record<ChartType, { title: string; spec: unknown }> = {
  architecture: {
    title: '系统总体架构图',
    spec: {
      direction: 'TB',
      layers: [
        { name: '感知层', nodes: ['高清摄像机', '门禁控制器'] },
        { name: '平台层', nodes: ['视频管理平台', '统一运维平台'] },
        { name: '应用层', nodes: ['指挥调度', '移动应用'] },
      ],
      edges: [{ from: '高清摄像机', to: '视频管理平台', label: 'RTSP' }, { from: '视频管理平台', to: '指挥调度' }],
    },
  },
  gantt: {
    title: '实施进度甘特图',
    spec: {
      start: '2026-11-01',
      tasks: [
        { id: 't1', name: '需求调研', phase: '准备阶段', days: 10 },
        { id: 't2', name: '设备采购', phase: '准备阶段', after: 't1', days: 30 },
        { name: '安装调试', phase: '实施阶段', after: 't2', days: 40 },
        { name: '竣工验收', phase: '验收阶段', days: 10 },
      ],
    },
  },
  org: {
    title: '项目组织架构图',
    spec: { nodes: [{ id: 'pm', name: '项目经理', person: '张三' }, { id: 'tech', name: '技术组', parent: 'pm' }, { id: 'qa', name: '质量组', parent: 'pm' }] },
  },
  equipment_table: {
    title: '核心设备选型表',
    spec: { columns: ['序号', '设备名称', '品牌型号', '主要参数', '数量'], rows: [['1', '核心交换机', '华为 S7706', '交换容量≥86Tbps', '2']] },
  },
}

function renderTone(c: ProposalChart): Tone {
  if (c.stale) return 'amber'
  return c.render_status === 'rendered' ? 'green' : c.render_status === 'failed' ? 'red' : 'amber'
}

function ChartImage({ taskID, chart }: { taskID: number; chart: ProposalChart }) {
  const [url, setURL] = useState('')
  const [err, setErr] = useState('')
  useEffect(() => {
    let alive = true
    let made = ''
    api.chartImage(taskID, chart.id).then(({ blob }) => {
      made = URL.createObjectURL(blob)
      if (alive) setURL(made)
    }).catch((e) => alive && setErr(errorText(e)))
    return () => { alive = false; if (made) URL.revokeObjectURL(made) }
  }, [taskID, chart.id, chart.artifact_hash])
  if (err) return <p className="text-xs text-red-600">{err}</p>
  if (!url) return <Spinner />
  return <img src={url} alt={chart.title} className="max-h-72 w-full rounded border border-slate-200 bg-white object-contain" />
}

function ChartBody({ taskID, chart }: { taskID: number; chart: ProposalChart }) {
  if (chart.render_status === 'rendered' && chart.render_format === 'png') return <ChartImage taskID={taskID} chart={chart} />
  if (chart.chart_type === 'equipment_table') {
    let t: { columns: string[]; rows: string[][] } | undefined
    try { t = JSON.parse(chart.source) } catch { /* 源码无效时下方显示原文 */ }
    if (t) return (
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <thead><tr className="bg-slate-50">{t.columns.map((c) => <th key={c} className="border border-slate-200 px-2 py-1 text-left">{c}</th>)}</tr></thead>
          <tbody>{t.rows.map((r, i) => <tr key={i}>{r.map((v, j) => <td key={j} className="border border-slate-200 px-2 py-1">{v}</td>)}</tr>)}</tbody>
        </table>
      </div>
    )
  }
  return (
    <div className="rounded border border-dashed border-amber-300 bg-amber-50/40 p-2">
      <div className="mb-1 flex items-center gap-1 text-xs text-amber-800"><ImageOff className="size-3.5" />
        未渲染：导出时插入下列 Mermaid 源码占位框{chart.render_error ? `（${chart.render_error}）` : ''}</div>
      <Pre className="max-h-48 text-[11px]">{chart.source}</Pre>
    </div>
  )
}

export function ChartsCard({ taskID, sectionID, currentRevNo, writable, tick }:
  { taskID: number; sectionID: number; currentRevNo: number; writable: boolean; tick: number }) {
  const toast = useToast()
  const [refresh, setRefresh] = useState(0)
  const data = useAsync(() => api.charts(taskID, sectionID), [taskID, sectionID, tick, refresh, currentRevNo])
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState('')
  const charts = data.data?.charts ?? []
  const renderer = data.data?.renderer

  const act = async (key: string, fn: () => Promise<unknown>, ok: string) => {
    setBusy(key)
    try { await fn(); toast('ok', ok); setRefresh((n) => n + 1) } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }

  return (
    <Card title={<>章节图表 <span className="text-xs font-normal text-slate-400">绑定修订；章节产生新修订后旧图表失效、不进入导出</span></>}
      actions={<>
        <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => setRefresh((n) => n + 1)} />
        <Button size="sm" variant="primary" icon={<Wand2 className="size-3.5" />} disabled={!currentRevNo} onClick={() => setOpen(true)}>生成图表</Button>
      </>}>
      <ErrorBanner error={data.error} onRetry={data.reload} />
      {renderer && !renderer.renderer && (
        <div className="mb-3 flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" />服务器没有 mermaid-cli（mmdc）：架构图 / 甘特图 / 组织架构图只保存源码（unrendered），导出时插入源码占位框；
          安装 mmdc 并设置 AUTOBID_MMDC_PATH 后可点「重新渲染」。设备选型表不受影响（导出为原生 Word 表格）。
        </div>
      )}
      {!currentRevNo && <p className="text-xs text-slate-500">章节尚无正文修订，先保存正文再生成图表。</p>}
      {data.loading && !data.data ? <Spinner /> : charts.length === 0 ? <Empty>本章节暂无图表</Empty> : (
        <div className="grid gap-3 md:grid-cols-2">
          {charts.map((c) => (
            <div key={c.id} className={cx('space-y-2 rounded-md border p-3', c.stale ? 'border-amber-300 bg-amber-50/30' : 'border-slate-200')}>
              <div className="flex flex-wrap items-center gap-1.5 text-sm">
                <span className="font-medium text-slate-800">{c.title}</span>
                <Badge>{chartTypeLabel[c.chart_type]}</Badge>
                <Badge tone={renderTone(c)}>{c.stale ? '已过期' : chartRenderLabel[c.render_status]}</Badge>
                <span className="text-xs text-slate-400">rev {c.rev_no} · #{c.id}</span>
              </div>
              {c.stale && <p className="text-xs text-amber-700">{c.stale_reason}；确认图表仍适用于新正文后「绑定当前修订」，否则重新生成。</p>}
              <ChartBody taskID={taskID} chart={c} />
              <div className="flex flex-wrap gap-1.5">
                {c.stale && <Button size="sm" icon={<Link2 className="size-3.5" />} loading={busy === `b${c.id}`} disabled={!writable}
                  onClick={() => act(`b${c.id}`, () => api.rebindChart(taskID, c.id), `已绑定到 rev ${currentRevNo}`)}>绑定当前修订</Button>}
                {c.source_format === 'mermaid' && <Button size="sm" icon={<RefreshCw className="size-3.5" />} loading={busy === `r${c.id}`}
                  onClick={() => act(`r${c.id}`, () => api.renderChart(taskID, c.id), '已重新渲染')}>重新渲染</Button>}
                <Button size="sm" variant="ghost" icon={<Trash2 className="size-3.5" />} loading={busy === `d${c.id}`}
                  onClick={() => { if (confirm(`删除图表《${c.title}》？`)) void act(`d${c.id}`, () => api.deleteChart(taskID, c.id), '已删除') }}>删除</Button>
              </div>
            </div>
          ))}
        </div>
      )}
      {open && <ChartModal taskID={taskID} sectionID={sectionID} onClose={() => setOpen(false)} onDone={() => { setOpen(false); setRefresh((n) => n + 1) }} />}
    </Card>
  )
}

function ChartModal({ taskID, sectionID, onClose, onDone }: { taskID: number; sectionID: number; onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const [type, setType] = useState<ChartType>('architecture')
  const [title, setTitle] = useState(chartExamples.architecture.title)
  const [spec, setSpec] = useState(JSON.stringify(chartExamples.architecture.spec, null, 2))
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const pick = (t: ChartType) => { setType(t); setTitle(chartExamples[t].title); setSpec(JSON.stringify(chartExamples[t].spec, null, 2)); setErr('') }
  const submit = async () => {
    let parsed: unknown
    try { parsed = JSON.parse(spec) } catch (e) { setErr(`规格不是合法 JSON：${(e as Error).message}`); return }
    setBusy(true); setErr('')
    try {
      const c = await api.generateChart(taskID, { section_id: sectionID, chart_type: type, title, spec: parsed })
      toast(c.render_status === 'rendered' ? 'ok' : 'info', c.render_status === 'rendered' ? '图表已生成并渲染' : `图表已保存（${chartRenderLabel[c.render_status]}）`)
      onDone()
    } catch (e) { setErr(errorText(e)) } finally { setBusy(false) }
  }
  return (
    <Modal open wide title="生成章节图表" onClose={onClose}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={submit}>生成</Button></>}>
      <div className="space-y-3">
        <div className="grid gap-3 md:grid-cols-2">
          <Field label="图表类型"><Select value={type} onChange={(e) => pick(e.target.value as ChartType)}>
            {(Object.keys(chartExamples) as ChartType[]).map((k) => <option key={k} value={k}>{chartTypeLabel[k]}</option>)}</Select></Field>
          <Field label="题注"><Input value={title} onChange={(e) => setTitle(e.target.value)} /></Field>
        </div>
        <Field label="结构化规格（JSON）" hint="Mermaid 源码由服务器按规格生成并校验；设备型号、数量须与已核验证据一致">
          <TextArea rows={14} className="font-mono text-xs" value={spec} onChange={(e) => setSpec(e.target.value)} />
        </Field>
        {err && <p className="text-sm text-red-600">{err}</p>}
      </div>
    </Modal>
  )
}

// ---------------------------------------------------------------------------
// ⑥ 导出成稿
// ---------------------------------------------------------------------------

function jobTone(s: string): Tone {
  return s === 'succeeded' ? 'green' : s === 'failed' ? 'red' : s === 'running' ? 'blue' : 'slate'
}

function Blockers({ r }: { r: ExportReadiness }) {
  const groups = new Map<string, ExportReadiness['blockers']>()
  for (const b of r.blockers) groups.set(b.kind, [...(groups.get(b.kind) ?? []), b])
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-1.5">{[...groups.entries()].map(([k, list]) => <Badge key={k} tone="red">{exportBlockerLabel[k] ?? k} {list.length}</Badge>)}</div>
      <ul className="max-h-64 space-y-1 overflow-y-auto text-sm">
        {r.blockers.map((b, i) => (
          <li key={i} className="flex gap-2 rounded border border-red-100 bg-red-50/50 px-2 py-1">
            <span className="shrink-0 text-xs font-medium text-red-700">{exportBlockerLabel[b.kind] ?? b.kind}</span>
            <span className="text-slate-700">{b.message}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

function Warnings({ list }: { list: ExportWarning[] }) {
  if (!list.length) return null
  return (
    <ul className="space-y-1 text-xs">
      {list.map((w, i) => (
        <li key={i} className="flex gap-2 rounded border border-amber-100 bg-amber-50/60 px-2 py-1">
          <span className="shrink-0 font-medium text-amber-800">{exportWarningLabel[w.kind] ?? w.kind}</span>
          <span className="text-slate-700">{w.message}</span>
        </li>
      ))}
    </ul>
  )
}

export function ExportPanel({ taskID, tick }: { taskID: number; tick: number }) {
  const toast = useToast()
  const [refresh, setRefresh] = useState(0)
  const pre = useAsync(() => api.exportPreflight(taskID), [taskID, tick, refresh])
  const jobs = useAsync(() => api.exportJobs(taskID), [taskID, tick, refresh])
  const [formats, setFormats] = useState<Record<ExportFormat, boolean>>({ docx: true, pdf: true })
  const [template, setTemplate] = useState(0)
  const [bidder, setBidder] = useState('')
  const [busy, setBusy] = useState('')
  const [blocked, setBlocked] = useState<ExportPreflight>()
  const [detail, setDetail] = useState<ExportJob>()
  const p = pre.data
  const list = jobs.data?.jobs ?? []
  const active = list.some((j) => j.status === 'queued' || j.status === 'running')
  useInterval(() => { void jobs.reload() }, active ? 2000 : null)

  const create = async () => {
    const fs = (Object.keys(formats) as ExportFormat[]).filter((f) => formats[f])
    if (!fs.length) { toast('error', '至少选择一种格式'); return }
    setBusy('create'); setBlocked(undefined)
    try {
      const r = await api.createExport(taskID, { formats: fs, template_id: template || undefined, bidder })
      toast('ok', `已创建导出任务 ${r.jobs.map((j) => `#${j.id} ${j.format.toUpperCase()}`).join('、')}，已锁定 ${Object.keys(r.jobs[0].rev_snapshot).length} 个章节修订`)
      setRefresh((n) => n + 1)
    } catch (e) {
      if (e instanceof ApiError && e.status === 422 && e.code === 'export_blocked') {
        setBlocked((e.data as { preflight: ExportPreflight }).preflight)
        toast('error', '导出门禁未通过，明细见上方')
        void pre.reload()
      } else toast('error', errorText(e))
    } finally { setBusy('') }
  }
  const download = async (j: ExportJob) => {
    setBusy(`dl${j.id}`)
    try { const { blob, filename } = await api.downloadExport(taskID, j.id); saveBlob(blob, filename || j.artifact_name) } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  const upload = async (file: File | undefined) => {
    if (!file) return
    setBusy('upload')
    try { const t = await api.uploadTemplate(file, file.name.replace(/\.docx$/i, '')); toast('ok', `模板「${t.name}」已上传`); setTemplate(t.id); setRefresh((n) => n + 1) }
    catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  const removeTemplate = async (id: number) => {
    setBusy(`t${id}`)
    try { await api.deleteTemplate(id); toast('ok', '模板已删除'); if (template === id) setTemplate(0); setRefresh((n) => n + 1) }
    catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  const downloadDefault = async () => {
    try { const { blob } = await api.defaultTemplate(); saveBlob(blob, 'autobid-default-template.docx') } catch (e) { toast('error', errorText(e)) }
  }

  const r = blocked?.readiness ?? p?.readiness
  const warnings = blocked?.warnings ?? p?.warnings ?? []
  return (
    <div className="space-y-4">
      <Card title={<>导出门禁 {r && (r.ready
        ? <Badge tone="green"><CheckCircle2 className="size-3" />可以导出</Badge>
        : <Badge tone="red"><ShieldAlert className="size-3" />{r.blockers.length} 项阻止导出</Badge>)}
        {r && <span className="text-xs font-normal text-slate-400">大纲 v{r.outline_version || '—'} · 检查于 {fmtTime(r.checked_at)}</span>}</>}
        actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => { setBlocked(undefined); setRefresh((n) => n + 1) }} />}>
        <ErrorBanner error={pre.error} onRetry={pre.reload} />
        {!r ? <Spinner /> : (
          <div className="space-y-3">
            {r.ready
              ? <p className="text-sm text-emerald-700">全部章节已批准当前修订，响应矩阵全部实质响应且未失效，最新一致性核验无待处理结论，证据均有效。
                创建导出时在同一数据库事务内固化章节修订、图表与偏离表快照，之后再编辑不影响该次导出。</p>
              : <Blockers r={r} />}
            <Warnings list={warnings} />
            {p && <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-slate-500">
              <span>LibreOffice：{p.status.office ? <span className="text-emerald-700">{p.status.office_version}</span> : <span className="text-red-600">不可用（PDF 将失败，DOCX 目录页码不刷新）</span>}</span>
              <span>mermaid-cli：{p.status.renderer ? <span className="text-emerald-700">可用</span> : <span className="text-amber-700">不可用（图表为源码占位）</span>}</span>
              <span>纳入导出的图表 {p.charts} 张</span>
            </div>}
          </div>
        )}
      </Card>

      <Card title="发起导出" bodyClassName="p-4 space-y-3">
        <div className="grid gap-3 md:grid-cols-3">
          <Field label="格式">
            <div className="flex gap-4 pt-1.5 text-sm">
              {(['docx', 'pdf'] as ExportFormat[]).map((f) => (
                <label key={f} className="flex items-center gap-1.5">
                  <input type="checkbox" checked={formats[f]} onChange={(e) => setFormats({ ...formats, [f]: e.target.checked })} />{f.toUpperCase()}
                </label>
              ))}
            </div>
          </Field>
          <Field label="套版模板">
            <Select value={template} onChange={(e) => setTemplate(Number(e.target.value))}>
              <option value={0}>内置默认模板（A4 · 宋体正文 · 黑体标题）</option>
              {(p?.templates ?? []).map((t) => <option key={t.id} value={t.id}>{t.name}（{t.file_name}）</option>)}
            </Select>
          </Field>
          <Field label="封面投标人（可选）"><Input value={bidder} onChange={(e) => setBidder(e.target.value)} placeholder="如：某某科技有限公司" /></Field>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="primary" icon={<Lock className="size-4" />} loading={busy === 'create'} disabled={!r?.ready} onClick={create}>锁定版本并导出</Button>
          <label className={cx('inline-flex cursor-pointer items-center gap-1.5 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm text-slate-700 hover:bg-slate-50',
            busy === 'upload' && 'opacity-50')}>
            <Upload className="size-4" />上传 .docx 模板
            <input type="file" accept=".docx" className="hidden" onChange={(e) => { void upload(e.target.files?.[0]); e.target.value = '' }} />
          </label>
          <Button icon={<FileDown className="size-4" />} onClick={downloadDefault}>下载默认模板</Button>
        </div>
        {(p?.templates ?? []).length > 0 && (
          <ul className="space-y-1 text-xs">
            {p!.templates.map((t) => (
              <li key={t.id} className="flex flex-wrap items-center gap-2 rounded border border-slate-100 px-2 py-1">
                <FileText className="size-3.5 text-slate-400" /><span className="font-medium">{t.name}</span>
                <span className="text-slate-400">{fmtBytes(t.size)} · {t.meta.page_width_mm}×{t.meta.page_height_mm}mm · {t.meta.has_content_placeholder ? '含 {{content}} 占位' : '正文追加在模板内容之后'}</span>
                {!!t.meta.missing_styles?.length && <span className="text-amber-700">将补齐样式：{t.meta.missing_styles.join('、')}</span>}
                <Button size="sm" variant="ghost" icon={<Trash2 className="size-3" />} loading={busy === `t${t.id}`} onClick={() => removeTemplate(t.id)} />
              </li>
            ))}
          </ul>
        )}
        <p className="text-xs text-slate-400">模板中可使用 {'{{content}}'}（正文插入位置）、{'{{title}}'}、{'{{bidder}}'}、{'{{date}}'}、{'{{outline_version}}'} 占位符（须在同一文字段内）；
          标题按样式名「heading 1~6」匹配。目录与页码由 LibreOffice 刷新，Word 打开时无需手动更新域。</p>
      </Card>

      <Card title="导出任务" actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => void jobs.reload()} />}>
        <ErrorBanner error={jobs.error} onRetry={jobs.reload} />
        {jobs.loading && !jobs.data ? <Spinner /> : list.length === 0 ? <Empty>尚无导出任务</Empty> : (
          <table className="w-full text-sm">
            <thead><tr className="border-b border-slate-200 text-left text-xs text-slate-500">
              <th className="py-2 pr-2">任务</th><th className="pr-2">状态</th><th className="pr-2">快照</th><th className="pr-2">目录</th><th className="pr-2">发起 / 完成</th><th className="pr-2">提示</th><th />
            </tr></thead>
            <tbody>{list.map((j) => (
              <tr key={j.id} className="border-b border-slate-100 align-top">
                <td className="py-2 pr-2 font-medium">#{j.id} {j.format.toUpperCase()}<div className="text-xs font-normal text-slate-400">{j.template_key === 'default' ? '默认模板' : j.template_key}</div></td>
                <td className="pr-2"><Badge tone={jobTone(j.status)}>{exportStatusLabel[j.status]}</Badge>
                  {j.attempts > 1 && <div className="text-xs text-slate-400">第 {j.attempts} 次执行</div>}
                  {j.error && <div className="max-w-xs text-xs text-red-600">{j.error}</div>}</td>
                <td className="pr-2 text-xs text-slate-500">大纲 v{j.outline_version} · {Object.keys(j.rev_snapshot).length} 章 · {j.chart_snapshot.length} 图 · 偏离表 {j.matrix_snapshot.length} 行</td>
                <td className="pr-2 text-xs">{j.status === 'succeeded' ? (j.toc_refreshed ? <span className="text-emerald-700">页码已刷新</span> : <span className="text-amber-700">页码未刷新</span>) : '—'}</td>
                <td className="pr-2 text-xs text-slate-500">{j.created_by || '—'} · {fmtTime(j.created_at)}{j.finished_at && <div>{fmtTime(j.finished_at)}</div>}</td>
                <td className="pr-2 text-xs">{j.warnings.length ? <Badge tone="amber">{j.warnings.length} 项</Badge> : '—'}</td>
                <td className="whitespace-nowrap">
                  <Button size="sm" variant="ghost" onClick={() => setDetail(j)}>详情</Button>
                  {j.status === 'succeeded' && <Button size="sm" variant="primary" icon={<Download className="size-3.5" />} loading={busy === `dl${j.id}`}
                    onClick={() => download(j)}>{fmtBytes(j.artifact_size)}</Button>}
                </td>
              </tr>
            ))}</tbody>
          </table>
        )}
      </Card>
      {detail && <JobModal job={detail} onClose={() => setDetail(undefined)} />}
    </div>
  )
}

function JobModal({ job, onClose }: { job: ExportJob; onClose: () => void }) {
  return (
    <Modal open wide title={`导出任务 #${job.id}（${job.format.toUpperCase()}）`} onClose={onClose}>
      <div className="space-y-4 text-sm">
        <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-slate-500">
          <span>状态 {exportStatusLabel[job.status]}</span><span>执行 {job.attempts}/{job.max_attempts} 次</span>
          <span>模板 {job.template_key}（{job.template_hash.slice(0, 12)}）</span>
          {job.artifact_hash && <span>产物 SHA-256 {job.artifact_hash.slice(0, 16)}…</span>}
        </div>
        {job.error && <p className="text-red-600">{job.error}</p>}
        <Warnings list={job.warnings} />
        <div>
          <div className="mb-1 text-xs font-medium text-slate-500">锁定的章节修订（rev_snapshot）</div>
          <table className="w-full text-xs">
            <tbody>{job.doc_snapshot.sections.map((s) => (
              <tr key={s.section_id} className="border-b border-slate-100">
                <td className="py-1 pr-2 font-mono text-slate-400" style={{ paddingLeft: (s.level - 1) * 12 }}>{s.section_code}</td>
                <td className="pr-2">{s.title}</td><td className="text-slate-500">rev {s.rev_no}</td>
              </tr>
            ))}</tbody>
          </table>
        </div>
        {job.chart_snapshot.length > 0 && <div>
          <div className="mb-1 text-xs font-medium text-slate-500">图表快照</div>
          <ul className="space-y-0.5 text-xs">{job.chart_snapshot.map((c) => (
            <li key={c.chart_id}>#{c.chart_id} {c.title} · {chartTypeLabel[c.chart_type]} · rev {c.rev_no} · {chartRenderLabel[c.render_status]}</li>
          ))}</ul>
        </div>}
      </div>
    </Modal>
  )
}
