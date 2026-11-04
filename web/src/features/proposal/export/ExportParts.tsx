// Phase 6B：章节写作视图中的「章节图表」卡片（S3 起经图表渲染作业异步生成、取消与重试），以及「⑥ 导出成稿」视图
// （门禁明细、最终稿 / 预览稿模式、格式与模板、S4 起以请求组提交并逐项显示格式作业、取消、历史与下载）。
// 全部数据来自 server/export.go 的真实路由；门禁、快照固化与文档引擎（WPS / Word）/ mmdc 调用都在后端。

import { useEffect, useRef, useState } from 'react'
import {
  AlertTriangle, CheckCircle2, Download, FileDown, FileText, ImageOff, Link2, Loader2, Lock, RefreshCw, ShieldAlert, Trash2, Upload, Wand2, XCircle,
} from 'lucide-react'
import { api, errorText, ApiError, saveBlob } from '../../../api/client'
import type {
  ChartType, ExportFormat, ExportJob, ExportMode, ExportPreflight, ExportRequest, ExportWarning, ProposalChart,
} from '../../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, IconButton, Input, Modal, Notice, Pre, Select, Spinner, TextArea, cx, useConfirm, useToast } from '../../../components/ui'
import BlockerList from '../../task-workspace/BlockerList'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import { chartStateView, keyForInput, retryRequestKey, splitCharts } from './chartJobs'
import type { InputKey } from './chartJobs'
import { exportInputSig, exportJobView, exportRequestView, groupExportJobs, keepKeyAfter } from './exportJobs'
import type { ExportInput } from './exportJobs'
import { RuntimeNotice } from '../../runtime/RuntimeStatus'
import { useAsync, useInterval } from '../../../hooks/useAsync'
import {
  chartRenderLabel, chartTypeLabel, exportStatusLabel, exportWarningLabel, fmtBytes, fmtTime,
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
        尚无渲染图：导出时插入下列 Mermaid 源码占位框</div>
      <Pre className="max-h-48 text-[11px]">{chart.source}</Pre>
    </div>
  )
}

// 图表渲染作业（S3）：生成 / 重新渲染只提交作业，渲染由服务器后台完成；有活跃作业时每 2 秒刷新，
// SSE 的 chart-render 活动同样触发重读。取消后迟到的渲染结果由服务器围栏拒绝，页面以服务器状态为准。
export function ChartsCard({ taskID, sectionID, currentRevNo, writable, tick }:
  { taskID: number; sectionID: number; currentRevNo: number; writable: boolean; tick: number }) {
  const toast = useToast()
  const confirm = useConfirm()
  const { invalidate } = useTaskWorkspace()
  const chartTick = useTick('charts')
  const [refresh, setRefresh] = useState(0)
  const data = useAsync(() => api.charts(taskID, sectionID), [taskID, sectionID, tick, chartTick, refresh, currentRevNo])
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState('')
  const [showStale, setShowStale] = useState(false)
  const charts = data.data?.charts ?? []
  const renderer = data.data?.renderer
  const { current, stale } = splitCharts(charts)
  const anyActive = charts.some((c) => chartStateView(c).active)
  useInterval(() => { void data.reload() }, anyActive ? 2000 : null)

  const act = async (key: string, fn: () => Promise<unknown>, ok: string) => {
    setBusy(key)
    try { await fn(); toast('ok', ok); invalidate('chart_changed') } catch (e) { toast('error', errorText(e)); setRefresh((n) => n + 1) } finally { setBusy('') }
  }
  const item = (c: ProposalChart) => {
    const v = chartStateView(c)
    const job = c.latest_job
    return (
      <div key={c.id} className={cx('space-y-2 rounded-md border p-3', c.stale ? 'border-amber-300 bg-amber-50/30' : 'border-slate-200')}>
        <div className="flex flex-wrap items-center gap-1.5 text-sm">
          <span className="font-medium text-slate-800">{c.title}</span>
          <Badge>{chartTypeLabel[c.chart_type]}</Badge>
          <Badge tone={v.tone}>{v.active && <Loader2 className="size-3 animate-spin" aria-hidden />}{v.label}</Badge>
          {c.plan_code && <Badge tone="purple" title="章节撰写随草稿给出的图表计划">写作计划 {c.plan_code}</Badge>}
          <span className="text-xs text-slate-400">rev {c.rev_no} · #{c.id}</span>
        </div>
        {v.note && <p className={cx('text-xs', v.tone === 'red' ? 'text-red-600' : c.stale || v.tone === 'amber' ? 'text-amber-700' : 'text-slate-500')}>{v.note}</p>}
        <ChartBody taskID={taskID} chart={c} />
        <div className="flex flex-wrap gap-1.5">
          {v.active && job && <Button size="sm" icon={<XCircle className="size-3.5" />} loading={busy === `c${c.id}`}
            onClick={() => act(`c${c.id}`, () => api.cancelChartJob(taskID, job.id), `已取消渲染作业 #${job.id}`)}>取消渲染</Button>}
          {c.stale && <Button size="sm" icon={<Link2 className="size-3.5" />} loading={busy === `b${c.id}`} disabled={!writable || v.active}
            onClick={() => act(`b${c.id}`, () => api.rebindChart(taskID, c.id), `已绑定到 rev ${currentRevNo}`)}>绑定当前修订</Button>}
          {v.canRetry && <Button size="sm" icon={<RefreshCw className="size-3.5" />} loading={busy === `r${c.id}`}
            onClick={() => act(`r${c.id}`, () => api.submitChartJob(taskID, { chart_id: c.id, expected_rev_no: c.rev_no, request_key: retryRequestKey(c) }),
              '已提交重新渲染，后台完成后自动刷新')}>{c.render_status === 'rendered' ? '重新渲染' : '重试渲染'}</Button>}
          <Button size="sm" variant="ghost" icon={<Trash2 className="size-3.5" />} loading={busy === `d${c.id}`} disabled={v.active}
            onClick={async () => { if (await confirm({ title: `删除图表《${c.title}》？`, message: '删除后不会进入导出；已固化在历史导出任务中的快照不受影响。', confirmLabel: '删除', danger: true })) void act(`d${c.id}`, () => api.deleteChart(taskID, c.id), '已删除') }}>删除</Button>
        </div>
      </div>
    )
  }

  return (
    <Card title={<>章节图表 <span className="text-xs font-normal text-slate-400">绑定修订；后台渲染，章节产生新修订后旧图表失效、不进入导出</span></>}
      actions={<>
        <IconButton label="刷新章节图表" onClick={() => setRefresh((n) => n + 1)}><RefreshCw className="size-4" /></IconButton>
        <Button size="sm" variant="primary" icon={<Wand2 className="size-3.5" />} disabled={!currentRevNo} onClick={() => setOpen(true)}>生成图表</Button>
      </>}>
      <ErrorBanner error={data.error} onRetry={data.reload} />
      {renderer && !renderer.renderer && (
        <div className="mb-3 flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" />服务器没有可用的 mermaid-cli（mmdc）：架构图 / 甘特图 / 组织架构图暂不能提交渲染，已有未渲染图表导出时插入源码占位框；
          安装 mmdc 并设置 AUTOBID_MMDC_PATH 后可「重试渲染」。设备选型表不受影响（导出为原生 Word 表格）。
        </div>
      )}
      {!currentRevNo && <p className="text-xs text-slate-500">章节尚无正文修订，先保存正文再生成图表。</p>}
      {data.loading && !data.data ? <Spinner /> : current.length === 0 && stale.length === 0 ? <Empty>本章节暂无图表</Empty> : (
        <div className="space-y-3">
          {current.length === 0 ? <Empty>当前修订（rev {currentRevNo}）暂无图表</Empty> : <div className="grid gap-3 md:grid-cols-2">{current.map(item)}</div>}
          {stale.length > 0 && (
            <div className="rounded-md border border-amber-200">
              <button type="button" className="flex w-full items-center gap-1.5 px-3 py-2 text-left text-xs text-amber-800" onClick={() => setShowStale((s) => !s)}>
                <AlertTriangle className="size-3.5" />旧修订图表 {stale.length} 张（不进入导出）{showStale ? '，点击收起' : '，点击展开'}
              </button>
              {showStale && <div className="grid gap-3 border-t border-amber-200 p-3 md:grid-cols-2">{stale.map(item)}</div>}
            </div>
          )}
        </div>
      )}
      {open && <ChartModal taskID={taskID} sectionID={sectionID} revNo={currentRevNo} onClose={() => setOpen(false)}
        onDone={() => { setOpen(false); invalidate('chart_changed') }} />}
    </Card>
  )
}

function ChartModal({ taskID, sectionID, revNo, onClose, onDone }: { taskID: number; sectionID: number; revNo: number; onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const [type, setType] = useState<ChartType>('architecture')
  const [title, setTitle] = useState(chartExamples.architecture.title)
  const [spec, setSpec] = useState(JSON.stringify(chartExamples.architecture.spec, null, 2))
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  // 同一输入的重复提交（含响应丢失后重发）复用请求键；改了类型、题注或规格即换新键。
  const key = useRef<InputKey>(undefined)
  const pick = (t: ChartType) => { setType(t); setTitle(chartExamples[t].title); setSpec(JSON.stringify(chartExamples[t].spec, null, 2)); setErr('') }
  const submit = async () => {
    let parsed: unknown
    try { parsed = JSON.parse(spec) } catch (e) { setErr(`规格不是合法 JSON：${(e as Error).message}`); return }
    key.current = keyForInput(key.current, JSON.stringify([sectionID, revNo, type, title.trim(), parsed]),
      () => `ui-chart-${sectionID}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`)
    setBusy(true); setErr('')
    try {
      const r = await api.submitChartJob(taskID, { request_key: key.current.key, section_id: sectionID, expected_rev_no: revNo, chart_type: type, title, spec: parsed })
      toast(r.replayed ? 'info' : 'ok', r.replayed ? `该请求已提交过，返回原渲染作业 #${r.job.id}` : `已提交渲染作业 #${r.job.id}，后台完成后自动刷新`)
      onDone()
    } catch (e) {
      if (e instanceof ApiError && e.code === 'runtime_unavailable') setErr(`服务器没有可用的 mermaid-cli，无法渲染该类型图表（设备选型表不受影响）：${e.message}`)
      else if (e instanceof ApiError && e.code === 'stale_revision') setErr('章节已产生新修订，请关闭后刷新章节再生成图表。')
      else setErr(errorText(e))
    } finally { setBusy(false) }
  }
  return (
    <Modal open wide title={`生成章节图表（绑定 rev ${revNo}）`} onClose={onClose}
      dirty={title !== chartExamples[type].title || spec !== JSON.stringify(chartExamples[type].spec, null, 2)}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={submit}>提交渲染</Button></>}>
      <div className="space-y-3">
        <div className="grid gap-3 md:grid-cols-2">
          <Field label="图表类型"><Select value={type} onChange={(e) => pick(e.target.value as ChartType)}>
            {(Object.keys(chartExamples) as ChartType[]).map((k) => <option key={k} value={k}>{chartTypeLabel[k]}</option>)}</Select></Field>
          <Field label="题注"><Input value={title} onChange={(e) => setTitle(e.target.value)} /></Field>
        </div>
        <Field label="结构化规格（JSON）" hint="Mermaid 源码由服务器按规格生成并校验，渲染在后台完成；设备型号、数量须与已核验证据一致">
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

const modeOptions: { mode: ExportMode; title: string; desc: string }[] = [
  { mode: 'final', title: '最终稿（严格）', desc: '文档引擎（WPS / Word）真实刷新目录页码；图表必须已渲染，不插入源码占位框；成稿回读核对目录、图片与表格。引擎不可用时拒绝，不降级。' },
  { mode: 'preview', title: '预览稿', desc: '用于内部审阅：引擎不可用时 DOCX 目录页码不刷新，未渲染图表为源码占位框；结果标为“仅预览稿”，不作为最终交付。' },
]

// S4：导出以请求组提交（同一输入的重复点击 / 响应丢失后重发复用请求键，服务器返回原请求组），作业状态与“最终稿 / 仅预览稿”分开显示；
// 有排队或执行中的作业时每 2 秒刷新，SSE 的 export 活动同样触发重读；刷新页面后以 /export-requests 恢复。
export function ExportPanel({ taskID, tick, onCreated }: { taskID: number; tick: number; onCreated?: () => void }) {
  const toast = useToast()
  const confirm = useConfirm()
  const [refresh, setRefresh] = useState(0)
  const [mode, setMode] = useState<ExportMode>('final')
  const pre = useAsync(() => api.exportPreflight(taskID, mode), [taskID, tick, refresh, mode])
  const reqs = useAsync(() => api.exportRequests(taskID), [taskID, tick, refresh])
  const jobs = useAsync(() => api.exportJobs(taskID), [taskID, tick, refresh])
  const [formats, setFormats] = useState<Record<ExportFormat, boolean>>({ docx: true, pdf: true })
  const [template, setTemplate] = useState(0)
  const [bidder, setBidder] = useState('')
  const [busy, setBusy] = useState('')
  const [blocked, setBlocked] = useState<ExportPreflight>()
  const [detail, setDetail] = useState<{ job: ExportJob; request?: ExportRequest }>()
  // 同一输入的请求键：服务器已明确答复后清空，再次点击即新的执行。
  const key = useRef<InputKey>(undefined)
  const p = pre.data
  const groups = groupExportJobs(reqs.data?.requests ?? [], jobs.data?.jobs ?? [])
  const active = groups.some((g) => g.jobs.some((j) => exportJobView(j).active))
  useInterval(() => { void reqs.reload(); void jobs.reload() }, active ? 2000 : null)
  const reloadAll = () => { setBlocked(undefined); setRefresh((n) => n + 1) }

  const r = blocked?.readiness ?? p?.readiness
  const create = async () => {
    const fs = (Object.keys(formats) as ExportFormat[]).filter((f) => formats[f])
    if (!fs.length) { toast('error', '至少选择一种格式'); return }
    const input: ExportInput = { formats: fs, template_id: template || undefined, bidder, mode, expected_outline_version: r?.outline_version || undefined }
    key.current = keyForInput(key.current, exportInputSig(input),
      () => `ui-export-${taskID}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`)
    setBusy('create'); setBlocked(undefined)
    try {
      const res = await api.createExport(taskID, { ...input, request_key: key.current.key })
      key.current = undefined
      const q = res.request
      toast(res.replayed ? 'info' : 'ok', res.replayed
        ? `该请求已提交过，返回原导出请求组 #${q?.id}（${q ? exportRequestView(q).label : ''}）`
        : `已提交${mode === 'final' ? '最终稿' : '预览稿'}导出请求组 #${q?.id}：${res.jobs.map((j) => j.format.toUpperCase()).join('、')}，已锁定 ${Object.keys(res.jobs[0]?.rev_snapshot ?? {}).length} 个章节修订`)
      setRefresh((n) => n + 1)
      onCreated?.()
    } catch (e) {
      if (!keepKeyAfter(e)) key.current = undefined
      if (e instanceof ApiError && e.status === 422 && e.code === 'export_blocked') {
        setBlocked((e.data as { preflight: ExportPreflight }).preflight)
        toast('error', '导出门禁未通过，明细见上方')
        void pre.reload()
      } else if (e instanceof ApiError && e.code === 'stale_revision') {
        toast('error', '批准大纲版本已变化：已刷新导出门禁，请确认后重新导出')
        void pre.reload()
      } else if (e instanceof ApiError && e.code === 'runtime_unavailable') {
        toast('error', `文档引擎不可用，不能生成最终稿（${e.message}）；可改为导出预览稿`)
      } else if (e instanceof ApiError && e.code === 'precondition_failed') {
        toast('error', /paused/.test(e.message) ? '项目已暂停：恢复项目后才能提交新的导出（已提交的导出可以查看与取消）' : `暂不能导出：${e.message}`)
      } else toast('error', errorText(e))
    } finally { setBusy('') }
  }
  const cancelGroup = async (q: ExportRequest) => {
    if (!await confirm({ title: `取消导出请求组 #${q.id}？`, message: '排队或执行中的格式立即停止，已完成的格式保留；取消后迟到的执行结果不会写回。', confirmLabel: '取消导出', danger: true })) return
    setBusy(`g${q.id}`)
    try {
      const res = await api.cancelExportRequest(taskID, q.id)
      const n = res.results.filter((x) => x.cancelled).length
      toast(n ? 'ok' : 'info', n ? `已取消 ${n} 个作业` : '组内作业均已结束，没有可取消的作业')
    } catch (e) { toast('error', errorText(e)) } finally { setBusy(''); setRefresh((x) => x + 1) }
  }
  const cancelJob = async (j: ExportJob) => {
    setBusy(`c${j.id}`)
    try {
      const res = await api.cancelExport(taskID, j.id)
      toast(res.cancelled ? 'ok' : 'info', res.cancelled ? `已取消导出作业 #${j.id}` : `作业 #${j.id} 已结束（${exportStatusLabel[res.job.status] ?? res.job.status}）`)
    } catch (e) { toast('error', errorText(e)) } finally { setBusy(''); setRefresh((x) => x + 1) }
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

  const warnings = blocked?.warnings ?? p?.warnings ?? []
  const finalMode = mode === 'final'
  return (
    <div className="space-y-4">
      <Card title={<>导出门禁 {r && (r.ready
        ? <Badge tone="green"><CheckCircle2 className="size-3" />{finalMode ? '可以导出最终稿' : '可以导出预览稿'}</Badge>
        : <Badge tone="red"><ShieldAlert className="size-3" />{r.blockers.length} 项阻止{finalMode ? '最终稿' : '导出'}</Badge>)}
        {r && <span className="text-xs font-normal text-slate-400">大纲 v{r.outline_version || '—'} · {finalMode ? '严格最终稿策略' : '预览稿策略'} · 检查于 {fmtTime(r.checked_at)}</span>}</>}
        subtitle="以服务器 exports/preflight 结果为准；提交时服务端会在锁定快照的同一事务内再次校验，预检不承诺提交成功"
        actions={<IconButton label="刷新导出门禁" onClick={reloadAll}><RefreshCw className="size-4" /></IconButton>}>
        <ErrorBanner error={pre.error} onRetry={pre.reload} />
        {blocked && <Notice tone="danger" className="mb-3">提交被拒绝：服务端在锁定快照时重新校验发现阻断，下方已改用服务器返回的最新门禁；已选格式与模板保留，未生成导出作业。</Notice>}
        {!r ? <Spinner /> : (
          <div className="space-y-3">
            {r.ready
              ? <p className="text-sm text-emerald-700">全部章节已批准当前修订，响应矩阵全部实质响应且未失效，最新一致性核验无待处理结论，证据均有效{finalMode ? '，纳入导出的图表均已渲染，文档引擎可用' : ''}。
                提交时在同一数据库事务内固化章节修订、图表与偏离表快照，之后再编辑不影响该次导出。</p>
              : <BlockerList taskId={taskID} blockers={r.blockers} />}
            <Warnings list={warnings} />
            <RuntimeNotice capabilities={['office', 'mermaid']} />
            {p && <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-slate-500">
              <span>文档引擎：{p.status.office ? <span className="text-emerald-700">{p.status.office_version || p.status.office_engine}</span> : <span className="text-red-600">不可用（最终稿与 PDF 被拒绝，预览稿 DOCX 目录页码不刷新）</span>}</span>
              <span>mermaid-cli：{p.status.renderer ? <span className="text-emerald-700">可用</span> : <span className="text-amber-700">不可用（已有渲染图仍可用于最终稿）</span>}</span>
              <span>纳入导出的图表 {p.charts} 张</span>
            </div>}
          </div>
        )}
      </Card>

      <Card title="发起导出" bodyClassName="p-4 space-y-3">
        <div role="radiogroup" aria-label="导出模式" className="grid gap-2 md:grid-cols-2">
          {modeOptions.map((o) => (
            <label key={o.mode} className={cx('flex cursor-pointer gap-2 rounded-md border p-3 text-sm',
              mode === o.mode ? 'border-blue-400 bg-blue-50/50' : 'border-slate-200 hover:bg-slate-50')}>
              <input type="radio" name="export-mode" aria-label={o.title} className="mt-1" checked={mode === o.mode} onChange={() => { setMode(o.mode); setBlocked(undefined) }} />
              <span><span className="font-medium text-slate-800">{o.title}</span><span className="mt-0.5 block text-xs text-slate-500">{o.desc}</span></span>
            </label>
          ))}
        </div>
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
          <Button variant="primary" icon={<Lock className="size-4" />} loading={busy === 'create'} disabled={!r?.ready} onClick={create}>
            {finalMode ? '锁定版本并导出最终稿' : '锁定版本并导出预览稿'}</Button>
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
          标题按样式名「heading 1~6」匹配。目录与页码由文档引擎（WPS / Word）刷新，打开时无需手动更新域。</p>
      </Card>

      <Card title="导出请求" subtitle={active ? '有排队或执行中的作业，每 2 秒刷新；全部结束后停止轮询' : '导出成功只表示产物已生成，不代表已签章、已被交易平台接收或已通过外部评审'}
        actions={<IconButton label="刷新导出请求" onClick={() => { void reqs.reload(); void jobs.reload() }}><RefreshCw className="size-4" /></IconButton>}>
        <ErrorBanner error={reqs.error ?? jobs.error} onRetry={() => { void reqs.reload(); void jobs.reload() }} />
        {(reqs.loading && !reqs.data) || (jobs.loading && !jobs.data) ? <Spinner /> : groups.length === 0 ? <Empty>尚无导出请求</Empty> : (
          <div className="space-y-3">
            {groups.map((g) => {
              const q = g.request
              const gv = q ? exportRequestView(q) : undefined
              return (
                <section key={g.key} aria-label={q ? `导出请求组 #${q.id}` : `导出任务 #${g.jobs[0].id}`} className="rounded-md border border-slate-200">
                  <div className="flex flex-wrap items-center gap-2 border-b border-slate-100 bg-slate-50/60 px-3 py-2 text-xs text-slate-500">
                    <span className="text-sm font-medium text-slate-800">{q ? `请求组 #${q.id}` : '旧导出任务'}</span>
                    {q ? <Badge tone={q.strict_runtime ? 'purple' : 'slate'}>{q.strict_runtime ? '最终稿（严格）' : '预览稿'}</Badge>
                      : <Badge tone="slate" title="S4 之前创建的导出没有请求组，按预览稿对待">预览稿（旧）</Badge>}
                    {gv && <Badge tone={gv.tone}>{gv.active && <Loader2 className="size-3 animate-spin" aria-hidden />}{gv.label}</Badge>}
                    <span>大纲 v{q?.outline_version ?? g.jobs[0].outline_version}</span>
                    {q && <span title={q.snapshot_hash}>快照 {q.snapshot_hash.slice(0, 10)}</span>}
                    {q?.executor && <span>执行版本 {q.executor}</span>}
                    <span>{(q?.created_by ?? g.jobs[0].created_by) || '—'} · {fmtTime(g.createdAt)}</span>
                    {q && gv?.active && <Button size="sm" icon={<XCircle className="size-3.5" />} loading={busy === `g${q.id}`} onClick={() => cancelGroup(q)}>取消整组</Button>}
                  </div>
                  <table className="w-full text-sm">
                    <tbody>{g.jobs.map((j) => {
                      const v = exportJobView(j)
                      return (
                        <tr key={j.id} className="border-b border-slate-100 align-top last:border-0">
                          <td className="w-28 px-3 py-2 font-medium">#{j.id} {j.format.toUpperCase()}<div className="text-xs font-normal text-slate-400">{j.template_key === 'default' ? '默认模板' : j.template_key}</div></td>
                          <td className="pr-2 py-2"><Badge tone={v.tone}>{v.active && <Loader2 className="size-3 animate-spin" aria-hidden />}{v.label}</Badge>
                            {v.note && <div className={cx('max-w-md text-xs', v.tone === 'red' ? 'text-red-600' : v.tone === 'amber' ? 'text-amber-700' : 'text-slate-500')}>{v.note}</div>}</td>
                          <td className="pr-2 py-2 text-xs text-slate-500">{Object.keys(j.rev_snapshot).length} 章 · {j.chart_snapshot.length} 图 · 偏离表 {j.matrix_snapshot.length} 行
                            {j.finished_at && <div>完成 {fmtTime(j.finished_at)}</div>}</td>
                          <td className="pr-2 py-2 text-xs">{j.warnings.length ? <Badge tone="amber">{j.warnings.length} 项提示</Badge> : ''}</td>
                          <td className="whitespace-nowrap py-2 pr-3 text-right">
                            <Button size="sm" variant="ghost" onClick={() => setDetail({ job: j, request: q })}>详情</Button>
                            {v.active && <Button size="sm" icon={<XCircle className="size-3.5" />} loading={busy === `c${j.id}`} onClick={() => cancelJob(j)}>取消</Button>}
                            {v.canDownload && <Button size="sm" variant={v.final ? 'primary' : 'secondary'} icon={<Download className="size-3.5" />} loading={busy === `dl${j.id}`}
                              title={v.final ? '严格最终稿' : '仅预览稿，不作为最终交付'} onClick={() => download(j)}>{v.final ? '最终稿' : '预览稿'} {fmtBytes(j.artifact_size)}</Button>}
                          </td>
                        </tr>
                      )
                    })}</tbody>
                  </table>
                </section>
              )
            })}
          </div>
        )}
      </Card>
      {detail && <JobModal job={detail.job} request={detail.request} onClose={() => setDetail(undefined)} />}
    </div>
  )
}

function JobModal({ job, request, onClose }: { job: ExportJob; request?: ExportRequest; onClose: () => void }) {
  const v = exportJobView(job)
  return (
    <Modal open wide title={`导出作业 #${job.id}（${job.format.toUpperCase()}）`} onClose={onClose}>
      <div className="space-y-4 text-sm">
        <div className="flex flex-wrap items-center gap-x-6 gap-y-1 text-xs text-slate-500">
          <Badge tone={v.tone}>{v.label}</Badge>
          <span>作业状态 {exportStatusLabel[job.status] ?? job.status}</span><span>执行 {job.attempts}/{job.max_attempts} 次</span>
          <span>策略 {request?.policy_version ?? (job.strict_runtime ? '最终稿' : '预览稿（S4 之前）')}</span>
          {job.executor && <span>固定执行版本 {job.executor}</span>}
          {job.office_version && <span>实际刷新引擎 {job.office_version}</span>}
          {job.deadline_at && <span>截止 {fmtTime(job.deadline_at)}</span>}
          <span>模板 {job.template_key}（{job.template_hash.slice(0, 12)}）</span>
          {job.artifact_hash && <span>产物 SHA-256 {job.artifact_hash.slice(0, 16)}…</span>}
          {request && <span title={request.snapshot_hash}>请求键 {request.request_key} · 快照 {request.snapshot_hash.slice(0, 16)}…</span>}
        </div>
        {v.note && <p className={v.tone === 'red' ? 'text-red-600' : 'text-slate-600'}>{v.note}</p>}
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
