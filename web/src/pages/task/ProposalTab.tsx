import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import {
  AlertTriangle, ArrowDown, ArrowUp, CheckCircle2, FileSearch, GitBranch, History, ListChecks, Pencil, Plus, RefreshCw, RotateCcw,
  Save, Send, ShieldCheck, Sparkles, Trash2, XCircle,
} from 'lucide-react'
import { ApiError, api, errorText } from '../../api/client'
import type {
  OutlineDetail, RequirementCategory, RequirementInput, RequirementItem, RequirementsView, SaveRevisionResult,
  SectionInput, SectionRevision,
} from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Modal, Pre, Select, Spinner, Stat, TextArea, cx, useToast } from '../../components/ui'
import type { Tone } from '../../components/ui'
import { RetrievalEvidence } from '../../components/RetrievalEvidence'
import { useAsync, useInterval } from '../../hooks/useAsync'
import {
  fmtTime, manifestStatusLabel, outlineStatusLabel, pageRange, requirementCategoryLabel, reviewStatusLabel, revisionSourceLabel,
  revisionStatusLabel, sectionStatusLabel,
} from '../../lib/format'

type View = 'requirements' | 'outline' | 'writing'

const categories: RequirementCategory[] = ['mandatory_clause', 'scoring_item', 'non_scoring_required', 'format_attachment']

function outlineTone(s: string): Tone {
  return s === 'approved' ? 'green' : s === 'pending_approval' ? 'amber' : s === 'superseded' ? 'slate' : 'blue'
}

function sectionTone(s: string): Tone {
  switch (s) {
    case 'approved': return 'green'
    case 'ready': case 'draft': return 'blue'
    case 'writing': case 'reviewing': return 'purple'
    case 'pending': return 'amber'
    case 'stale_outline': case 'stale_evidence': case 'rejected': return 'red'
    default: return 'slate'
  }
}

function revisionTone(s: string): Tone {
  return s === 'approved' ? 'green' : s === 'conflict_candidate' || s === 'stale_outline' || s === 'stale_evidence' ? 'red' : s === 'reviewing' ? 'purple' : 'slate'
}

const parseIDs = (s: string) => s.split(/[,，\s]+/).map((x) => Number(x)).filter((n) => Number.isInteger(n) && n > 0)
const parseCodes = (s: string) => s.split(/[,，\s]+/).map((x) => x.trim()).filter(Boolean)

export default function ProposalTab({ taskID, tick }: { taskID: number; tick: number }) {
  const [view, setView] = useState<View>('requirements')
  const [sectionID, setSectionID] = useState<number>()
  const openSection = (id: number) => { setSectionID(id); setView('writing') }
  const views: { key: View; label: string; icon: ReactNode }[] = [
    { key: 'requirements', label: '① 投标要求清单复核', icon: <ListChecks className="size-4" /> },
    { key: 'outline', label: '② 技术标大纲与审批', icon: <GitBranch className="size-4" /> },
    { key: 'writing', label: '③ 章节写作与版本', icon: <Pencil className="size-4" /> },
  ]
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap gap-2">
        {views.map((v) => (
          <button key={v.key} onClick={() => setView(v.key)}
            className={cx('flex items-center gap-1.5 rounded-md border px-3 py-1.5 text-sm font-medium',
              view === v.key ? 'border-blue-600 bg-blue-50 text-blue-700' : 'border-slate-200 bg-white text-slate-600 hover:bg-slate-50')}>
            {v.icon}{v.label}
          </button>
        ))}
      </div>
      <p className="text-xs text-slate-500">
        流程：要求清单遍历提取并人工确认完整 → 生成大纲并提交审批（状态持久化，跨重启有效）→ 批准后按前置依赖派发章节写作。
        写回由数据库同时校验 <code>based_on_outline_version</code> 与 <code>base_rev_no</code>：大纲已变或他人已保存时只存为候选稿，不覆盖当前版本。
      </p>
      {view === 'requirements' && <RequirementsPanel taskID={taskID} tick={tick} />}
      {view === 'outline' && <OutlinePanel taskID={taskID} tick={tick} onOpenSection={openSection} />}
      {view === 'writing' && <WritingPanel taskID={taskID} tick={tick} sectionID={sectionID} onSelect={setSectionID} />}
    </div>
  )
}

// ---------------------------------------------------------------------------
// ① 要求清单
// ---------------------------------------------------------------------------

function RequirementsPanel({ taskID, tick }: { taskID: number; tick: number }) {
  const toast = useToast()
  const data = useAsync(() => api.requirements(taskID), [taskID, tick])
  const [filter, setFilter] = useState<'' | RequirementCategory>('')
  const [editing, setEditing] = useState<RequirementItem | 'new'>()
  const [evidenceRun, setEvidenceRun] = useState<number>()
  const [busy, setBusy] = useState('')

  const run = async (key: string, fn: () => Promise<unknown>, ok: string) => {
    setBusy(key)
    try { await fn(); toast('ok', ok); await data.reload() } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  if (data.loading && !data.data) return <Spinner />
  const v = data.data
  if (!v) return <ErrorBanner error={data.error} onRetry={data.reload} />
  const s = v.stats
  const items = v.items.filter((i) => !filter || i.category === filter)

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-6">
        <Stat label="要求总数" value={s.total} hint={`★ 强制 ${s.mandatory}`} />
        <Stat label="已复核确认" value={`${s.confirmed}/${s.total}`} tone={s.total > 0 && s.confirmed === s.total ? 'green' : 'amber'} />
        <Stat label="原文变更待复核" value={s.needs_revision} tone={s.needs_revision ? 'red' : undefined} />
        <Stat label="已绑定证据" value={s.with_evidence} hint={`已记录缺口 ${s.evidence_gaps}`} />
        <Stat label="尚未检索/复核证据" value={s.no_evidence} tone={s.no_evidence ? 'amber' : undefined} />
        <Stat label="大纲映射" value={`${s.mapped}/${s.total}`} hint={s.outline_version ? `依据大纲 v${s.outline_version}` : '尚无大纲'}
          tone={s.unmapped.length ? 'amber' : s.total ? 'green' : undefined} />
      </div>

      <Card title={<>要求清单 <Badge tone={v.manifest.status === 'confirmed' ? 'green' : v.manifest.status === 'stale' ? 'red' : 'amber'}>
        {manifestStatusLabel[v.manifest.status]} · epoch {v.manifest.epoch}</Badge>
        {v.manifest.confirmed_at && <span className="text-xs font-normal text-slate-400">由 {v.manifest.approved_by || '—'} 于 {fmtTime(v.manifest.confirmed_at)} 确认</span>}</>}
        actions={<>
          <Button size="sm" icon={<FileSearch className="size-3.5" />} loading={busy === 'extract'}
            onClick={() => run('extract', async () => {
              const r = await api.extractRequirements(taskID)
              toast('info', `遍历 ${r.result.scanned_chunks} 个原文分块：新增 ${r.result.created}，原文变更 ${r.result.updated}，未变 ${r.result.unchanged}`)
            }, '提取完成')}>从招标文件提取</Button>
          <Button size="sm" icon={<Plus className="size-3.5" />} onClick={() => setEditing('new')}>新增要求</Button>
          <Button size="sm" variant="success" icon={<ShieldCheck className="size-3.5" />} loading={busy === 'confirm'}
            disabled={s.total === 0 || s.needs_revision > 0 || v.manifest.status === 'confirmed'}
            onClick={() => run('confirm', () => api.confirmRequirements(taskID, v.manifest.epoch), '要求清单已确认完整')}>确认要求清单完整</Button>
          <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => void data.reload()} />
        </>}>
        <ErrorBanner error={data.error} />
        {v.manifest.status === 'stale' && (
          <div className="mb-3 flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800">
            <AlertTriangle className="mt-0.5 size-4 shrink-0" />清单在确认后发生了实质变更（或招标原文变更），已批准 / 待审批大纲已失效，请复核后重新确认。
          </div>
        )}
        <div className="mb-3 flex flex-wrap gap-1.5">
          <FilterChip active={!filter} onClick={() => setFilter('')}>全部 {s.total}</FilterChip>
          {categories.map((c) => (
            <FilterChip key={c} active={filter === c} onClick={() => setFilter(c)}>{requirementCategoryLabel[c]} {s.by_category[c] ?? 0}</FilterChip>
          ))}
        </div>
        {items.length === 0 ? <Empty>暂无要求。请先上传招标文件并点击「从招标文件提取」，或人工新增。</Empty> : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead><tr className="border-b border-slate-200 text-left text-xs text-slate-500">
                <th className="py-2 pr-2">编码</th><th className="pr-2">类别</th><th className="pr-2">要求</th><th className="pr-2">原文出处</th>
                <th className="pr-2">证据 / 缺口</th><th className="pr-2">映射章节</th><th className="pr-2">复核</th><th />
              </tr></thead>
              <tbody>
                {items.map((it) => (
                  <tr key={it.id} className="border-b border-slate-100 align-top">
                    <td className="py-2 pr-2 font-mono text-xs">{it.code}{it.origin === 'manual' && <div><Badge>人工</Badge></div>}</td>
                    <td className="pr-2"><Badge tone={it.category === 'mandatory_clause' ? 'red' : it.category === 'scoring_item' ? 'blue' : 'slate'}>{requirementCategoryLabel[it.category]}</Badge></td>
                    <td className="max-w-md pr-2">
                      <div className="font-medium">{it.is_mandatory && <span className="text-red-600">★ </span>}{it.title}
                        {it.score_weight > 0 && <span className="ml-1 text-xs text-blue-600">{it.score_weight} 分</span>}</div>
                      {it.description && it.description !== it.title && <details><summary className="cursor-pointer text-xs text-slate-400">原文</summary>
                        <p className="whitespace-pre-wrap text-xs text-slate-600">{it.description}</p></details>}
                    </td>
                    <td className="pr-2 text-xs text-slate-500">
                      {it.source_chunk_id ? <>{it.source_loc.section_code} {pageRange(it.source_loc.page_start, it.source_loc.page_end)}
                        <div className="text-slate-400">分块 #{it.source_chunk_id}{it.source_loc.table !== undefined && ` · 表${Number(it.source_loc.table) + 1}行${Number(it.source_loc.row) + 1}`}</div></>
                        : <span className="text-slate-400">无原文绑定</span>}
                    </td>
                    <td className="pr-2 text-xs">
                      {it.evidence_ids.length > 0
                        ? <button className="text-blue-600 hover:underline" disabled={!it.evidence_run_id} onClick={() => setEvidenceRun(it.evidence_run_id)}>
                          {it.evidence_ids.length} 条证据{it.evidence_run_id ? `（运行 #${it.evidence_run_id}）` : ''}</button>
                        : !it.evidence_gap && <span className="text-amber-600">未检索</span>}
                      {it.evidence_gap && <p className="whitespace-pre-wrap text-amber-700">缺口：{it.evidence_gap}</p>}
                    </td>
                    <td className="pr-2 text-xs">{it.mapped_section_codes.length ? it.mapped_section_codes.join('、') : <span className="text-amber-600">未映射</span>}</td>
                    <td className="pr-2"><Badge tone={it.review_status === 'confirmed' ? 'green' : it.review_status === 'needs_revision' ? 'red' : 'amber'}>{reviewStatusLabel[it.review_status]}</Badge></td>
                    <td className="whitespace-nowrap">
                      {it.review_status === 'needs_revision' && <Button size="sm" variant="ghost" title="已核对原文变更"
                        onClick={() => run(`r${it.id}`, () => api.updateRequirement(taskID, it.id, { review_status: 'confirmed' }), '已标记复核')}><CheckCircle2 className="size-3.5" /></Button>}
                      <Button size="sm" variant="ghost" onClick={() => setEditing(it)}><Pencil className="size-3.5" /></Button>
                      <Button size="sm" variant="ghost" onClick={() => {
                        if (confirm(`删除要求 ${it.code}？清单将变更，已批准大纲会失效。`)) void run(`d${it.id}`, () => api.deleteRequirement(taskID, it.id), '已删除')
                      }}><Trash2 className="size-3.5 text-red-500" /></Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <p className="mt-3 text-xs text-slate-400">完整清单来自逐分块遍历与人工复核，不以检索 Top-K 代替；检索不到证据的要求请记录缺口，不要删除。修改编码、类别、标题、描述、强制性、分值或原文来源会使清单 epoch 递增并使大纲失效；仅绑定证据不会。</p>
      </Card>
      {evidenceRun && <RetrievalEvidence key={evidenceRun} runID={evidenceRun} />}
      <RequirementEditor taskID={taskID} item={editing} onClose={() => setEditing(undefined)} onSaved={() => { setEditing(undefined); void data.reload() }} />
    </div>
  )
}

function FilterChip({ active, onClick, children }: { active: boolean; onClick: () => void; children: ReactNode }) {
  return <button onClick={onClick} className={cx('rounded-full border px-2.5 py-0.5 text-xs',
    active ? 'border-blue-600 bg-blue-600 text-white' : 'border-slate-300 bg-white text-slate-600 hover:bg-slate-50')}>{children}</button>
}

function RequirementEditor({ taskID, item, onClose, onSaved }:
  { taskID: number; item?: RequirementItem | 'new'; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const blank = { code: '', category: 'non_scoring_required' as RequirementCategory, title: '', description: '', is_mandatory: false, score_weight: '0',
    source_chunk_id: '', evidence_run_id: '', evidence_ids: '', evidence_gap: '', review_status: 'draft' }
  const [f, setF] = useState(blank)
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (!item) return
    if (item === 'new') { setF(blank); return }
    setF({ code: item.code, category: item.category, title: item.title, description: item.description, is_mandatory: item.is_mandatory,
      score_weight: String(item.score_weight), source_chunk_id: item.source_chunk_id ? String(item.source_chunk_id) : '',
      evidence_run_id: item.evidence_run_id ? String(item.evidence_run_id) : '', evidence_ids: item.evidence_ids.join(', '),
      evidence_gap: item.evidence_gap, review_status: item.review_status })
  }, [item]) // eslint-disable-line react-hooks/exhaustive-deps
  if (!item) return null
  const save = async () => {
    const req: RequirementInput = {
      code: f.code.trim(), category: f.category, title: f.title.trim(), description: f.description, is_mandatory: f.is_mandatory,
      score_weight: Number(f.score_weight) || 0, source_chunk_id: f.source_chunk_id ? Number(f.source_chunk_id) : 0,
      evidence_run_id: f.evidence_run_id ? Number(f.evidence_run_id) : 0, evidence_ids: parseIDs(f.evidence_ids), evidence_gap: f.evidence_gap,
      review_status: f.review_status,
    }
    setBusy(true)
    try {
      if (item === 'new') await api.createRequirement(taskID, req)
      else await api.updateRequirement(taskID, item.id, req)
      toast('ok', '已保存要求')
      onSaved()
    } catch (e) { toast('error', errorText(e)) } finally { setBusy(false) }
  }
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value })
  return (
    <Modal open title={item === 'new' ? '新增投标要求' : `编辑要求 ${item.code}`} onClose={onClose} wide
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={save} disabled={!f.code.trim() || !f.title.trim()}>保存</Button></>}>
      <div className="grid gap-3 md:grid-cols-3">
        <Field label="编码"><Input value={f.code} onChange={set('code')} placeholder="如 SC-3.1、MAN-01" /></Field>
        <Field label="类别"><Select value={f.category} onChange={set('category')}>
          {categories.map((c) => <option key={c} value={c}>{requirementCategoryLabel[c]}</option>)}</Select></Field>
        <Field label="分值"><Input type="number" min={0} value={f.score_weight} onChange={set('score_weight')} /></Field>
        <Field label="标题" className="md:col-span-3"><Input value={f.title} onChange={set('title')} /></Field>
        <Field label="要求原文 / 说明" className="md:col-span-3"><TextArea rows={3} value={f.description} onChange={set('description')} /></Field>
        <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={f.is_mandatory} onChange={(e) => setF({ ...f, is_mandatory: e.target.checked })} />★/▲ 实质性（废标）条款</label>
        <Field label="原文分块 ID" hint="须属于本任务的招标原文；0 / 空为解除绑定"><Input value={f.source_chunk_id} onChange={set('source_chunk_id')} /></Field>
        <Field label="复核状态"><Select value={f.review_status} onChange={set('review_status')}>
          {Object.entries(reviewStatusLabel).map(([k, l]) => <option key={k} value={k}>{l}</option>)}</Select></Field>
        <Field label="证据检索运行 ID" hint="来自「证据检索」页"><Input value={f.evidence_run_id} onChange={set('evidence_run_id')} /></Field>
        <Field label="证据 ID（逗号分隔）" className="md:col-span-2"><Input value={f.evidence_ids} onChange={set('evidence_ids')} /></Field>
        <Field label="证据缺口说明" hint="查无证据时必须记录缺口，不得删除要求" className="md:col-span-3"><TextArea rows={2} value={f.evidence_gap} onChange={set('evidence_gap')} /></Field>
      </div>
    </Modal>
  )
}

// ---------------------------------------------------------------------------
// ② 大纲与审批
// ---------------------------------------------------------------------------

function OutlinePanel({ taskID, tick, onOpenSection }: { taskID: number; tick: number; onOpenSection: (id: number) => void }) {
  const toast = useToast()
  const list = useAsync(() => api.outlines(taskID), [taskID, tick])
  const [version, setVersion] = useState<number>()
  const current = version ?? list.data?.open_version ?? list.data?.approved_version ?? list.data?.outlines[0]?.version
  const det = useAsync(() => (current ? api.getOutline(taskID, current) : Promise.resolve(undefined)), [taskID, current, tick])
  const reqs = useAsync(() => api.requirements(taskID), [taskID, tick])
  const [editing, setEditing] = useState(false)
  const [busy, setBusy] = useState('')

  const act = async (key: string, fn: () => Promise<OutlineDetail>, ok: string) => {
    setBusy(key)
    try { const d = await fn(); toast('ok', ok); setVersion(d.outline.version); await list.reload(); await det.reload() } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  if (list.loading && !list.data) return <Spinner />
  if (!list.data) return <ErrorBanner error={list.error} onRetry={list.reload} />
  const l = list.data
  const d = det.data
  const reqByID = new Map((reqs.data?.items ?? []).map((r) => [r.id, r]))

  return (
    <div className="space-y-4">
      <Card title="大纲版本" actions={<>
        <Button size="sm" variant="primary" icon={<Sparkles className="size-3.5" />} disabled={!!l.open_version} loading={busy === 'create'}
          title={l.open_version ? `已有在编大纲 v${l.open_version}` : ''}
          onClick={() => act('create', () => api.createOutline(taskID, l.approved_version ? { from_version: l.approved_version, note: `基于 v${l.approved_version} 修订` } : { note: '按要求清单生成' }),
            '已生成大纲草案')}>{l.approved_version ? '基于批准版本新建草案' : '按要求清单生成大纲'}</Button>
        <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => { void list.reload(); void det.reload() }} />
      </>}>
        <ErrorBanner error={list.error} />
        {l.outlines.length === 0 ? <Empty>尚无大纲。确认要求清单后可「按要求清单生成大纲」：默认目录覆盖全部要求，评分项逐项成章，其余章节以「总体技术方案」为前置依赖。</Empty> : (
          <div className="flex flex-wrap gap-2">
            {l.outlines.map((o) => (
              <button key={o.id} onClick={() => setVersion(o.version)}
                className={cx('rounded-md border px-3 py-1.5 text-left text-sm', current === o.version ? 'border-blue-600 bg-blue-50' : 'border-slate-200 bg-white hover:bg-slate-50')}>
                <div className="flex items-center gap-1.5 font-medium">v{o.version} <Badge tone={outlineTone(o.status)}>{outlineStatusLabel[o.status]}</Badge></div>
                <div className="text-xs text-slate-400">{o.status === 'approved' ? `${o.approved_by || '—'} 批准 ${fmtTime(o.approved_at)}` : fmtTime(o.updated_at)}</div>
              </button>
            ))}
          </div>
        )}
      </Card>

      {det.error && <ErrorBanner error={det.error} onRetry={det.reload} />}
      {d && (
        <Card title={<>大纲 v{d.outline.version} <Badge tone={outlineTone(d.outline.status)}>{outlineStatusLabel[d.outline.status]}</Badge>
          {d.outline.derived_from_version && <span className="text-xs font-normal text-slate-400">派生自 v{d.outline.derived_from_version}</span>}</>}
          actions={<>
            <Button size="sm" icon={<Pencil className="size-3.5" />} onClick={() => setEditing(true)}
              disabled={(d.outline.status === 'approved' || d.outline.status === 'superseded') && !!l.open_version}
              title={l.open_version && l.open_version !== d.outline.version ? `已有在编草案 v${l.open_version}，请直接编辑它` : ''}>
              {d.outline.status === 'approved' || d.outline.status === 'superseded' ? '调整目录（派生新版本）' : '编辑目录'}</Button>
            {d.outline.status === 'draft' && <Button size="sm" variant="primary" icon={<Send className="size-3.5" />} loading={busy === 'submit'}
              onClick={() => act('submit', () => api.submitOutline(taskID, d.outline.version), '已提交审批')}>提交审批</Button>}
            {d.outline.status === 'pending_approval' && <Button size="sm" variant="success" icon={<ShieldCheck className="size-3.5" />} loading={busy === 'approve'}
              onClick={() => act('approve', () => api.approveOutline(taskID, d.outline.version), '大纲已批准，可派发章节写作')}>批准大纲</Button>}
          </>}>
          <div className="mb-3 grid gap-3 md:grid-cols-4">
            <Stat label="要求映射覆盖" value={`${d.coverage.mapped}/${d.coverage.total}`} tone={d.coverage.unmapped.length ? 'red' : 'green'} />
            <Stat label="章节数" value={d.sections.length} />
            <Stat label="清单状态" value={<span className="text-base">{manifestStatusLabel[d.manifest.status]}</span>}
              hint={`清单 epoch ${d.manifest.epoch} · 大纲基于 ${d.outline.requirements_epoch}`} tone={d.manifest.status === 'confirmed' && d.requirements_epoch_current ? 'green' : 'amber'} />
            <Stat label="当前批准版本" value={d.approved_version ? `v${d.approved_version}` : '无'} tone={d.approved_version ? 'green' : 'amber'} />
          </div>
          {d.coverage.unmapped.length > 0 && (
            <div className="mb-3 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">
              <div className="flex items-center gap-1.5 font-medium"><AlertTriangle className="size-4" />{d.coverage.unmapped.length} 条要求未映射到任何章节，补齐前不能提交审批：</div>
              <div className="mt-1 text-xs">{d.coverage.unmapped.map((r) => `${r.is_mandatory ? '★' : ''}${r.code} ${r.title}`).join('；')}</div>
            </div>
          )}
          {!d.requirements_epoch_current && d.outline.status === 'draft' && (
            <p className="mb-3 text-xs text-amber-700">要求清单在本草案生成后已变更：保存一次目录即可同步到最新 epoch，并需重新确认清单。</p>
          )}
          {d.outline.status === 'superseded' && <p className="mb-3 text-xs text-slate-500">该版本已被取代，仅供追溯；其章节的在途写回只会归档为 stale_outline 候选稿。</p>}
          {d.outline.status === 'pending_approval' && <p className="mb-3 text-xs text-amber-700">待审批期间所有章节写作派发与写回均被后端拒绝；此状态持久化在数据库中，服务重启后保持。</p>}
          <table className="w-full text-sm">
            <thead><tr className="border-b border-slate-200 text-left text-xs text-slate-500">
              <th className="py-2 pr-2">章节</th><th className="pr-2">映射要求</th><th className="pr-2">前置依赖</th><th className="pr-2">状态</th><th className="pr-2">版本</th><th />
            </tr></thead>
            <tbody>
              {d.sections.map((s) => (
                <tr key={s.id} className="border-b border-slate-100 align-top">
                  <td className="py-2 pr-2" style={{ paddingLeft: (s.level - 1) * 16 }}>
                    <span className="font-mono text-xs text-slate-500">{s.section_code}</span> <span className="font-medium">{s.title}</span>
                    {s.writing_guide && <div className="text-xs text-slate-400">{s.writing_guide}</div>}
                  </td>
                  <td className="pr-2 text-xs">{s.requirement_item_ids.map((id) => {
                    const r = reqByID.get(id)
                    return <span key={id} className="mr-1 inline-block">{r ? <>{r.is_mandatory && '★'}{r.code}</> : `#${id}`}</span>
                  })}</td>
                  <td className="pr-2 text-xs">{s.depends_on.length ? s.depends_on.map((c) => (
                    <span key={c} className={cx('mr-1', s.unmet_dependencies.includes(c) ? 'text-amber-600' : 'text-emerald-600')}>{c}</span>)) : '—'}</td>
                  <td className="pr-2"><Badge tone={sectionTone(s.status)}>{sectionStatusLabel[s.status] ?? s.status}</Badge>{s.writing && <Badge tone="purple">租约中</Badge>}</td>
                  <td className="pr-2 text-xs text-slate-500">rev {s.current_rev_no}{s.approved_rev_no ? ` · 定稿 rev ${s.approved_rev_no}` : ''}</td>
                  <td><Button size="sm" variant="ghost" onClick={() => onOpenSection(s.id)}>写作 →</Button></td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
      {d && editing && <OutlineEditor taskID={taskID} detail={d} requirements={reqs.data} onClose={() => setEditing(false)}
        onSaved={(nd) => { setEditing(false); setVersion(nd.outline.version); void list.reload(); void det.reload() }} />}
    </div>
  )
}

interface Row { code: string; title: string; level: string; deps: string; reqs: string; guide: string; words: string }

function OutlineEditor({ taskID, detail, requirements, onClose, onSaved }:
  { taskID: number; detail: OutlineDetail; requirements?: RequirementsView; onClose: () => void; onSaved: (d: OutlineDetail) => void }) {
  const toast = useToast()
  const codeByID = useMemo(() => new Map((requirements?.items ?? []).map((r) => [r.id, r.code])), [requirements])
  const idByCode = useMemo(() => new Map((requirements?.items ?? []).map((r) => [r.code, r.id])), [requirements])
  const [rows, setRows] = useState<Row[]>(() => detail.sections.map((s) => ({
    code: s.section_code, title: s.title, level: String(s.level), deps: s.depends_on.join(', '),
    reqs: s.requirement_item_ids.map((id) => codeByID.get(id) ?? `#${id}`).join(', '), guide: s.writing_guide, words: String(s.target_words),
  })))
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const derives = detail.outline.status === 'approved' || detail.outline.status === 'superseded'
  const mapped = new Set(rows.flatMap((r) => parseCodes(r.reqs)))
  const unmapped = (requirements?.items ?? []).filter((r) => !mapped.has(r.code))
  const upd = (i: number, k: keyof Row, v: string) => setRows(rows.map((r, j) => (j === i ? { ...r, [k]: v } : r)))
  const move = (i: number, d: number) => {
    const j = i + d
    if (j < 0 || j >= rows.length) return
    const next = [...rows];
    [next[i], next[j]] = [next[j], next[i]]
    setRows(next)
  }
  const save = async () => {
    const sections: SectionInput[] = []
    for (const r of rows) {
      const ids: number[] = []
      for (const c of parseCodes(r.reqs)) {
        const id = c.startsWith('#') ? Number(c.slice(1)) : idByCode.get(c)
        if (!id) { toast('error', `章节 ${r.code}：未知要求编码 ${c}`); return }
        ids.push(id)
      }
      sections.push({ section_code: r.code.trim(), title: r.title.trim(), level: Number(r.level) || undefined, depends_on: parseCodes(r.deps),
        requirement_item_ids: ids, writing_guide: r.guide, target_words: Number(r.words) || 0 })
    }
    setBusy(true)
    try {
      const d = await api.updateOutline(taskID, detail.outline.version, { note, sections })
      toast('ok', d.outline.version !== detail.outline.version ? `已派生新版本 v${d.outline.version}，v${detail.outline.version} 已失效` : '目录已保存')
      onSaved(d)
    } catch (e) { toast('error', errorText(e)) } finally { setBusy(false) }
  }
  return (
    <Modal open wide title={`编辑大纲目录 · v${detail.outline.version}`} onClose={onClose}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={save}>{derives ? '派生新版本' : '保存'}</Button></>}>
      {derives && <div className="mb-3 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800">
        保存将创建 v{detail.outline.version + 1} 草案{detail.outline.status === 'approved' ? `，当前批准的 v${detail.outline.version} 立即失效：在途 AI 写回将只能归档为候选稿，新版本需重新提交审批。` : '。'}
      </div>}
      {unmapped.length > 0 && <p className="mb-2 text-xs text-red-600">未映射：{unmapped.map((r) => r.code).join('、')}</p>}
      <Field label="修订说明"><Input value={note} onChange={(e) => setNote(e.target.value)} /></Field>
      <div className="mt-3 space-y-2">
        {rows.map((r, i) => (
          <div key={i} className="grid grid-cols-12 gap-1.5 rounded-md border border-slate-200 p-2">
            <Input className="col-span-1 font-mono text-xs" value={r.code} placeholder="编码" onChange={(e) => upd(i, 'code', e.target.value)} />
            <Input className="col-span-4" value={r.title} placeholder="标题" onChange={(e) => upd(i, 'title', e.target.value)} />
            <Input className="col-span-1" value={r.level} placeholder="层级" onChange={(e) => upd(i, 'level', e.target.value)} />
            <Input className="col-span-2 text-xs" value={r.deps} placeholder="前置章节编码" onChange={(e) => upd(i, 'deps', e.target.value)} />
            <Input className="col-span-3 text-xs" value={r.reqs} placeholder="映射要求编码" onChange={(e) => upd(i, 'reqs', e.target.value)} />
            <div className="col-span-1 flex items-center justify-end">
              <button onClick={() => move(i, -1)} className="p-0.5 text-slate-400 hover:text-slate-700"><ArrowUp className="size-3.5" /></button>
              <button onClick={() => move(i, 1)} className="p-0.5 text-slate-400 hover:text-slate-700"><ArrowDown className="size-3.5" /></button>
              <button onClick={() => setRows(rows.filter((_, j) => j !== i))} className="p-0.5 text-red-400 hover:text-red-600"><XCircle className="size-3.5" /></button>
            </div>
            <Input className="col-span-10 text-xs" value={r.guide} placeholder="写作要点" onChange={(e) => upd(i, 'guide', e.target.value)} />
            <Input className="col-span-2 text-xs" value={r.words} placeholder="目标字数" onChange={(e) => upd(i, 'words', e.target.value)} />
          </div>
        ))}
      </div>
      <Button className="mt-2" size="sm" icon={<Plus className="size-3.5" />}
        onClick={() => setRows([...rows, { code: String(rows.length + 1), title: '', level: '1', deps: '', reqs: '', guide: '', words: '0' }])}>添加章节</Button>
      <p className="mt-2 text-xs text-slate-400">前置依赖填同一大纲内的章节编码（逗号分隔），后端校验无环；依赖章节未产出有效稿前，本章节不能派发写作。</p>
    </Modal>
  )
}

// ---------------------------------------------------------------------------
// ③ 章节写作与版本
// ---------------------------------------------------------------------------

function WritingPanel({ taskID, tick, sectionID, onSelect }:
  { taskID: number; tick: number; sectionID?: number; onSelect: (id: number) => void }) {
  const list = useAsync(() => api.outlines(taskID), [taskID, tick])
  const v = list.data?.approved_version || list.data?.open_version || list.data?.outlines[0]?.version
  const outline = useAsync(() => (v ? api.getOutline(taskID, v) : Promise.resolve(undefined)), [taskID, v, tick])
  const sections = outline.data?.sections ?? []
  const selected = sectionID ?? sections[0]?.id

  if (list.loading && !list.data) return <Spinner />
  if (!v) return <Card><Empty>尚无大纲。请先在「② 技术标大纲与审批」中生成并批准大纲。</Empty></Card>
  return (
    <div className="grid gap-4 lg:grid-cols-[16rem_1fr]">
      <Card title={<>章节 · 大纲 v{v} <Badge tone={outlineTone(outline.data?.outline.status ?? '')}>{outlineStatusLabel[outline.data?.outline.status ?? ''] ?? '—'}</Badge></>} bodyClassName="p-2">
        <ErrorBanner error={outline.error || list.error} />
        <div className="space-y-0.5">
          {sections.map((s) => (
            <button key={s.id} onClick={() => onSelect(s.id)}
              className={cx('flex w-full items-center gap-1.5 rounded px-2 py-1.5 text-left text-sm', selected === s.id ? 'bg-blue-50 text-blue-700' : 'hover:bg-slate-50')}
              style={{ paddingLeft: 8 + (s.level - 1) * 12 }}>
              <span className="font-mono text-xs text-slate-400">{s.section_code}</span>
              <span className="flex-1 truncate">{s.title}</span>
              <span className={cx('size-2 shrink-0 rounded-full', {
                green: 'bg-emerald-500', blue: 'bg-blue-500', purple: 'bg-violet-500', amber: 'bg-amber-400', red: 'bg-red-500', slate: 'bg-slate-300',
              }[sectionTone(s.status)])} title={sectionStatusLabel[s.status]} />
            </button>
          ))}
        </div>
      </Card>
      {selected ? <SectionEditor key={selected} taskID={taskID} sectionID={selected} tick={tick} onChanged={() => void outline.reload()} /> : <Card><Empty>请选择章节</Empty></Card>}
    </div>
  )
}

function SectionEditor({ taskID, sectionID, tick, onChanged }: { taskID: number; sectionID: number; tick: number; onChanged: () => void }) {
  const toast = useToast()
  const det = useAsync(() => api.section(taskID, sectionID), [taskID, sectionID, tick])
  const [content, setContent] = useState('')
  const [summary, setSummary] = useState('')
  const [diffNote, setDiffNote] = useState('')
  const [evidence, setEvidence] = useState('')
  const [baseRev, setBaseRev] = useState(0)
  const [dirty, setDirty] = useState(false)
  const [instruction, setInstruction] = useState('')
  const [busy, setBusy] = useState('')
  const [conflict, setConflict] = useState<SaveRevisionResult>()
  const [viewing, setViewing] = useState<SectionRevision>()

  const d = det.data
  // 载入（或他人更新后在未编辑时同步）当前主版本作为编辑基线。
  useEffect(() => {
    if (!d || dirty) return
    setContent(d.current_revision?.content_md ?? '')
    setSummary(d.current_revision?.summary_note ?? '')
    setEvidence((d.current_revision?.evidence_ids ?? []).join(', '))
    setDiffNote('')
    setBaseRev(d.section.current_rev_no)
  }, [d, dirty])
  useInterval(() => void det.reload(), d?.section.writing ? 3000 : null)

  if (det.loading && !d) return <Spinner />
  if (!d) return <ErrorBanner error={det.error} onRetry={det.reload} />
  const s = d.section
  const behind = dirty && baseRev !== s.current_rev_no

  const refresh = async () => { await det.reload(); onChanged() }
  const save = async () => {
    setBusy('save'); setConflict(undefined)
    try {
      const r = await api.saveRevision(taskID, sectionID, { base_rev_no: baseRev, based_on_outline_version: s.outline_version, content_md: content,
        summary_note: summary, diff_summary: diffNote, evidence_ids: parseIDs(evidence) })
      toast('ok', `已保存 rev ${r.revision.rev_no}${r.revision.gaps.length ? `（${r.revision.gaps.length} 项缺口）` : ''}`)
      setDirty(false)
      await refresh()
    } catch (e) {
      if (e instanceof ApiError && e.status === 409 && e.data && typeof e.data === 'object' && 'revision' in e.data) {
        setConflict(e.data as SaveRevisionResult)
        toast('error', '版本冲突：你的修改已保存为候选稿，未覆盖当前版本')
        await det.reload()
      } else toast('error', errorText(e))
    } finally { setBusy('') }
  }
  const act = async (key: string, fn: () => Promise<unknown>, ok: string) => {
    setBusy(key)
    try { await fn(); toast('ok', ok); setDirty(false); await refresh() } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }

  return (
    <div className="space-y-4">
      <Card title={<><span className="font-mono text-slate-400">{s.section_code}</span> {s.title}
        <Badge tone={sectionTone(s.status)}>{sectionStatusLabel[s.status] ?? s.status}</Badge>{s.writing && <Badge tone="purple">AI 撰写中…</Badge>}</>}
        actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => void refresh()} />}>
        <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-slate-500">
          <span>所属大纲 v{s.outline_version}（当前批准 {d.approved_version ? `v${d.approved_version}` : '无'}）</span>
          <span>当前主版本 rev {s.current_rev_no}</span>
          <span>定稿 {s.approved_rev_no ? `rev ${s.approved_rev_no}（${s.approved_by}）` : '未批准'}</span>
          {s.target_words > 0 && <span>目标 {s.target_words} 字</span>}
        </div>
        {s.writing_guide && <p className="mt-2 text-sm text-slate-600">写作要点：{s.writing_guide}</p>}
        {d.blocked_reason && <div className="mt-3 flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" />{d.blocked_reason}</div>}
        {d.warnings.length > 0 && <div className="mt-3 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">
          <div className="flex items-center gap-1.5 font-medium"><XCircle className="size-4" />当前版本的证据存在问题，不能作为无缺口定稿：</div>
          <ul className="mt-1 list-disc pl-5 text-xs">{d.warnings.map((w) => <li key={w}>{w}</li>)}</ul>
        </div>}
        <div className="mt-3 grid gap-3 md:grid-cols-2">
          <div>
            <div className="mb-1 text-xs font-medium text-slate-500">前置依赖</div>
            {d.dependencies.length === 0 ? <p className="text-xs text-slate-400">无</p> : d.dependencies.map((dep) => (
              <div key={dep.id} className="text-xs">
                {dep.ready ? <CheckCircle2 className="inline size-3.5 text-emerald-600" /> : <XCircle className="inline size-3.5 text-amber-500" />}{' '}
                {dep.section_code} {dep.title} · rev {dep.current_rev_no}
                {dep.summary_note && <p className="ml-5 text-slate-400">{dep.summary_note}</p>}
              </div>
            ))}
          </div>
          <div>
            <div className="mb-1 text-xs font-medium text-slate-500">绑定要求</div>
            {d.requirements.length === 0 ? <p className="text-xs text-slate-400">无（概述 / 衔接章节）</p> : d.requirements.map((r) => (
              <div key={r.id} className="text-xs">
                <span className="font-mono text-slate-400">{r.code}</span> {r.is_mandatory && <span className="text-red-600">★</span>}{r.title}
                {r.evidence_gap && <span className="text-amber-600"> · 缺口：{r.evidence_gap}</span>}
                {!r.evidence_ids.length && !r.evidence_gap && <span className="text-amber-600"> · 未检索证据</span>}
              </div>
            ))}
          </div>
        </div>
      </Card>

      <Card title="AI 撰写 / 局部重写" bodyClassName="p-4 space-y-2">
        <Field label="重写指令（可选）"><Input value={instruction} onChange={(e) => setInstruction(e.target.value)} placeholder="如：补充运维响应时限，压缩到 1500 字" /></Field>
        <div className="flex items-center gap-2">
          <Button variant="primary" icon={<Sparkles className="size-4" />} loading={busy === 'dispatch'} disabled={!d.writable || s.writing || !s.dependencies_ready}
            onClick={() => act('dispatch', () => api.dispatchSection(taskID, sectionID, { base_rev_no: s.current_rev_no, instruction }),
              s.current_rev_no ? '已派发 AI 重写' : '已派发 AI 撰写初稿')}>{s.current_rev_no ? 'AI 重写' : 'AI 撰写初稿'}</Button>
          <span className="text-xs text-slate-500">派发时快照 rev {s.current_rev_no} 与大纲 v{s.outline_version}；期间若有人保存新版本或大纲变更，AI 稿只会成为候选稿。</span>
        </div>
      </Card>

      <Card title={<>编辑正文 <span className="text-xs font-normal text-slate-400">基于 rev {baseRev}</span></>}
        actions={<Button size="sm" variant="primary" icon={<Save className="size-3.5" />} loading={busy === 'save'} disabled={!d.writable || !content.trim()} onClick={save}>保存新版本</Button>}>
        {behind && <div className="mb-2 flex items-center gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          <AlertTriangle className="size-4" />编辑期间主版本已更新为 rev {s.current_rev_no}；保存将被识别为冲突并存为候选稿。
          <Button size="sm" onClick={() => setDirty(false)}>放弃修改并载入最新</Button></div>}
        {conflict && <div className="mb-2 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-700">
          {conflict.outcome === 'stale_outline' ? '大纲已变更' : '版本冲突'}：你的修改已归档为候选稿 rev {conflict.revision.rev_no}（{conflict.reason}），当前主版本仍为 rev {conflict.section.current_rev_no}，内容未被覆盖。
          可在下方历史中对比后「采用候选稿」。</div>}
        <TextArea rows={16} value={content} onChange={(e) => { setContent(e.target.value); setDirty(true) }} placeholder="Markdown 正文" />
        <div className="mt-2 grid gap-2 md:grid-cols-3">
          <Field label="衔接摘要 summary_note" className="md:col-span-2"><TextArea rows={2} value={summary} onChange={(e) => { setSummary(e.target.value); setDirty(true) }} /></Field>
          <Field label="引用证据 ID" hint="仅 verified + supports 可作为已证实依据；其余自动记为缺口">
            <Input value={evidence} onChange={(e) => { setEvidence(e.target.value); setDirty(true) }} /></Field>
          <Field label="修改说明" className="md:col-span-3"><Input value={diffNote} onChange={(e) => { setDiffNote(e.target.value); setDirty(true) }} /></Field>
        </div>
      </Card>

      {d.evidence.length > 0 && <Card title="当前版本证据有效性">
        <div className="space-y-1">{d.evidence.map((e) => (
          <div key={e.id} className="flex flex-wrap items-center gap-2 text-xs">
            <span className="font-mono">#{e.id}</span>
            <Badge tone={e.status === 'stale' ? 'red' : e.usable ? 'green' : e.relation === 'contradicts' ? 'red' : 'amber'}>{e.status} · {e.relation}</Badge>
            <span className="text-slate-500">{e.source_domain} · 运行 #{e.run_id}</span><span className="text-slate-400">{e.reason}</span>
          </div>))}</div>
      </Card>}

      <Card title={<><History className="size-4" />修订历史</>}>
        {d.revisions.length === 0 ? <Empty>尚无修订</Empty> : (
          <table className="w-full text-sm">
            <thead><tr className="border-b border-slate-200 text-left text-xs text-slate-500">
              <th className="py-2 pr-2">版本</th><th className="pr-2">来源</th><th className="pr-2">状态</th><th className="pr-2">基线</th><th className="pr-2">作者 / 时间</th><th className="pr-2">说明</th><th />
            </tr></thead>
            <tbody>{d.revisions.map((r) => {
              const isCurrent = r.rev_no === s.current_rev_no
              const candidate = r.status === 'conflict_candidate'
              return (
                <tr key={r.id} className={cx('border-b border-slate-100 align-top', isCurrent && 'bg-blue-50/50')}>
                  <td className="py-2 pr-2 font-medium">rev {r.rev_no}{isCurrent && <Badge tone="blue">当前</Badge>}{s.approved_rev_no === r.rev_no && <Badge tone="green">定稿</Badge>}</td>
                  <td className="pr-2 text-xs">{revisionSourceLabel[r.source]}{r.rollback_of_rev_no ? ` ← rev ${r.rollback_of_rev_no}` : ''}</td>
                  <td className="pr-2"><Badge tone={revisionTone(r.status)}>{revisionStatusLabel[r.status] ?? r.status}</Badge>
                    {r.gaps.length > 0 && <Badge tone="amber">{r.gaps.length} 缺口</Badge>}</td>
                  <td className="pr-2 text-xs text-slate-500">rev {r.base_rev_no} · v{r.based_on_outline_version}</td>
                  <td className="pr-2 text-xs text-slate-500">{r.created_by || '—'}<div>{fmtTime(r.created_at)}</div></td>
                  <td className="max-w-xs pr-2 text-xs text-slate-500">{r.diff_summary}</td>
                  <td className="whitespace-nowrap">
                    <Button size="sm" variant="ghost" onClick={() => setViewing(r)}>查看/对比</Button>
                    {isCurrent && (r.status === 'draft' || r.status === 'reviewing') && s.approved_rev_no !== r.rev_no &&
                      <Button size="sm" variant="success" loading={busy === `a${r.rev_no}`} disabled={!d.writable}
                        onClick={() => act(`a${r.rev_no}`, () => api.approveRevision(taskID, sectionID, r.rev_no), `rev ${r.rev_no} 已批准定稿`)}>批准</Button>}
                    {!isCurrent && r.status !== 'stale_outline' &&
                      <Button size="sm" icon={<RotateCcw className="size-3.5" />} loading={busy === `b${r.rev_no}`} disabled={!d.writable}
                        onClick={() => act(`b${r.rev_no}`, () => api.rollbackRevision(taskID, sectionID, r.rev_no, s.current_rev_no),
                          candidate ? `已采用候选稿 rev ${r.rev_no}` : `已回滚至 rev ${r.rev_no}`)}>{candidate ? '采用候选稿' : '回滚至此'}</Button>}
                  </td>
                </tr>
              )
            })}</tbody>
          </table>
        )}
        <p className="mt-2 text-xs text-slate-400">修订记录不可篡改；回滚 / 采用候选稿会基于当前主版本生成新的 rollback 修订，并撤销原有定稿批准。</p>
      </Card>
      {viewing && <RevisionViewer rev={viewing} current={d.current_revision} onClose={() => setViewing(undefined)} />}
    </div>
  )
}

// diffLines 基于 LCS 的逐行对比（章节正文规模有限，O(n·m) 可接受）。
function diffLines(a: string, b: string): { op: ' ' | '-' | '+'; line: string }[] {
  const x = a.split('\n').slice(0, 1500), y = b.split('\n').slice(0, 1500)
  const dp = Array.from({ length: x.length + 1 }, () => new Array<number>(y.length + 1).fill(0))
  for (let i = x.length - 1; i >= 0; i--) for (let j = y.length - 1; j >= 0; j--)
    dp[i][j] = x[i] === y[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
  const out: { op: ' ' | '-' | '+'; line: string }[] = []
  let i = 0, j = 0
  while (i < x.length && j < y.length) {
    if (x[i] === y[j]) { out.push({ op: ' ', line: x[i] }); i++; j++ }
    else if (dp[i + 1][j] >= dp[i][j + 1]) out.push({ op: '-', line: x[i++] })
    else out.push({ op: '+', line: y[j++] })
  }
  while (i < x.length) out.push({ op: '-', line: x[i++] })
  while (j < y.length) out.push({ op: '+', line: y[j++] })
  return out
}

function RevisionViewer({ rev, current, onClose }: { rev: SectionRevision; current?: SectionRevision; onClose: () => void }) {
  const [mode, setMode] = useState<'content' | 'diff'>('content')
  const diff = useMemo(() => (current ? diffLines(current.content_md, rev.content_md) : []), [current, rev])
  return (
    <Modal open wide title={<>rev {rev.rev_no} · {revisionSourceLabel[rev.source]} <Badge tone={revisionTone(rev.status)}>{revisionStatusLabel[rev.status] ?? rev.status}</Badge></>} onClose={onClose}>
      {current && current.rev_no !== rev.rev_no && <div className="mb-2 flex gap-2">
        <Button size="sm" variant={mode === 'content' ? 'primary' : 'secondary'} onClick={() => setMode('content')}>正文</Button>
        <Button size="sm" variant={mode === 'diff' ? 'primary' : 'secondary'} onClick={() => setMode('diff')}>对比当前 rev {current.rev_no}</Button>
      </div>}
      {mode === 'content' ? <Pre className="max-h-[50vh]">{rev.content_md}</Pre> : (
        <pre className="max-h-[50vh] overflow-auto rounded-md bg-slate-50 p-2 text-xs leading-relaxed">
          {diff.map((d, i) => <div key={i} className={d.op === '+' ? 'bg-emerald-50 text-emerald-800' : d.op === '-' ? 'bg-red-50 text-red-700 line-through' : 'text-slate-600'}>{d.op} {d.line}</div>)}
        </pre>
      )}
      {rev.summary_note && <p className="mt-2 text-sm"><span className="text-slate-500">衔接摘要：</span>{rev.summary_note}</p>}
      {rev.gaps.length > 0 && <div className="mt-2 text-xs text-amber-700"><div className="font-medium">缺口</div>
        <ul className="list-disc pl-5">{rev.gaps.map((g, i) => <li key={i}>{g.evidence_id ? `证据 #${g.evidence_id} ` : ''}{g.requirement_id ? `要求 #${g.requirement_id} ` : ''}[{g.kind}] {g.note}</li>)}</ul></div>}
      <p className="mt-2 text-xs text-slate-400">基于 rev {rev.base_rev_no} · 大纲 v{rev.based_on_outline_version} · 证据 {rev.evidence_ids.join(', ') || '无'} · {rev.created_by} {fmtTime(rev.created_at)}</p>
    </Modal>
  )
}

