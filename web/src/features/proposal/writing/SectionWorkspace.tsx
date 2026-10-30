import { useCallback, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { CheckCircle2, Eye, History, ListChecks, PanelRight, Pencil, RotateCcw, Save, ShieldCheck, Sparkles, XCircle } from 'lucide-react'
import { ApiError, api, errorText } from '../../../api/client'
import type { SaveRevisionResult, SectionDetail, SectionRevision } from '../../../api/types'
import {
  Badge, Button, Drawer, Empty, ErrorBanner, Field, Input, Modal, Notice, Spinner, TextArea, cx, useToast,
} from '../../../components/ui'
import { useAsync, useInterval } from '../../../hooks/useAsync'
import { fmtTime, pageRange, revisionSourceLabel, revisionStatusLabel, reviewStatusLabel, sectionStatusLabel } from '../../../lib/format'
import { Markdown } from '../../../lib/markdown'
import { linkProps, registerLeaveGuard, useSearchParams } from '../../../lib/router'
import { WRITING_PANELS, optionalId, pickEnum, taskUrl } from '../../../lib/routes'
import type { WritingPanel } from '../../../lib/routes'
import { ChartsCard } from '../export/ExportParts'
import { EvidenceDetailDrawer, EvidencePicker, EvidenceStateRow } from '../evidence/Evidence'
import { sectionTone } from '../outline/OutlinePage'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import RevisionCompare from './RevisionCompare'
import { draftKey, draftState, isDirty, loadDraft, newDraft, rebase, saveDraft, toggleEvidence } from './drafts'
import type { Draft } from './drafts'

// 章节写作：正文（编辑 / 预览）与写作依据（要求 / 证据 / 历史 / 图表）并列。
// 保存严格提交编辑基线；409 候选稿可打开、比较，不自动重试覆盖。AI 发起成功只表示派发成功，写回后才显示新正文。

function revisionTone(s: string) {
  return s === 'approved' ? 'green' as const : s === 'conflict_candidate' || s === 'stale_outline' || s === 'stale_evidence' ? 'red' as const : s === 'reviewing' ? 'purple' as const : 'slate' as const
}

type Choice = 'save' | 'discard' | 'cancel'
interface ChoiceReq { title: string; message: ReactNode; saveLabel: string; resolve: (c: Choice) => void }

function ChoiceDialog({ req, saving }: { req: ChoiceReq | null; saving: boolean }) {
  if (!req) return null
  return (
    <Modal open title={req.title} onClose={() => req.resolve('cancel')}
      footer={<>
        <Button onClick={() => req.resolve('cancel')} data-autofocus>继续编辑</Button>
        <Button variant="danger" onClick={() => req.resolve('discard')}>放弃修改</Button>
        <Button variant="primary" loading={saving} onClick={() => req.resolve('save')}>{req.saveLabel}</Button>
      </>}>
      <div className="text-sm">{req.message}</div>
    </Modal>
  )
}

const panelMeta: Record<WritingPanel, { label: string; icon: ReactNode }> = {
  requirements: { label: '要求', icon: <ListChecks className="size-3.5" /> },
  evidence: { label: '证据', icon: <ShieldCheck className="size-3.5" /> },
  history: { label: '历史', icon: <History className="size-3.5" /> },
  charts: { label: '图表', icon: <Eye className="size-3.5" /> },
}

export default function SectionWorkspace({ sectionId, layout, tocButton }: { sectionId: number; layout: 'three' | 'two' | 'one'; tocButton?: ReactNode }) {
  const toast = useToast()
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('sections', 'evidence')
  const det = useAsync(() => api.section(taskId, sectionId), [taskId, sectionId, tick])
  const [params, setQuery] = useSearchParams()
  const panel = pickEnum(params.get('panel'), WRITING_PANELS, 'requirements')
  const revParam = optionalId(params, 'rev')
  const evidenceParam = optionalId(params, 'evidence')
  const comparing = params.get('compare') === '1'

  const key = draftKey(taskId, sectionId)
  const [draft, setDraftState] = useState<Draft | null>(() => loadDraft(key) ?? null)
  const setDraft = useCallback((d: Draft | null | ((p: Draft | null) => Draft | null)) => {
    setDraftState((prev) => {
      const next = typeof d === 'function' ? d(prev) : d
      saveDraft(key, next)
      return next
    })
  }, [key])
  const [mode, setMode] = useState<'preview' | 'edit'>(() => (loadDraft(key) ? 'edit' : 'preview'))
  const [saving, setSaving] = useState(false)
  const [busy, setBusy] = useState('')
  const [conflict, setConflict] = useState<SaveRevisionResult>()
  const [saveError, setSaveError] = useState('')
  const [lastSaved, setLastSaved] = useState<number>()
  const [choice, setChoice] = useState<ChoiceReq | null>(null)
  const [aiOpen, setAIOpen] = useState(false)
  const [instruction, setInstruction] = useState('')
  const [evidenceView, setEvidenceView] = useState<{ run: number; id: number }>()
  const [panelDrawer, setPanelDrawer] = useState(false)

  const d: SectionDetail | undefined = det.data
  const s = d?.section
  const dirty = isDirty(draft)
  const state = s ? draftState(draft, s) : 'clean'
  useInterval(() => void det.reload(), s?.writing ? 3000 : null)

  // 保存草稿：成功只确认已发送的快照；409 时保留草稿并展示候选稿。
  const save = useCallback(async (): Promise<boolean> => {
    if (!draft || !s || saving) return false
    setSaving(true); setSaveError(''); setConflict(undefined)
    const sent = draft
    try {
      const r = await api.saveRevision(taskId, sectionId, {
        base_rev_no: sent.baseRev, based_on_outline_version: sent.baseOutline, content_md: sent.content,
        summary_note: sent.summary, diff_summary: sent.diffNote, evidence_ids: sent.evidenceIds,
      })
      toast('ok', `已保存为修订 ${r.revision.rev_no}${r.revision.gaps.length ? `（记录 ${r.revision.gaps.length} 项证据缺口）` : ''}`)
      setDraft(null); setMode('preview'); setLastSaved(Date.now())
      invalidate('section_saved')
      return true
    } catch (e) {
      if (e instanceof ApiError && e.status === 409 && e.data && typeof e.data === 'object' && 'revision' in e.data) {
        setConflict(e.data as SaveRevisionResult)
        toast('error', '版本冲突：你的修改已归档为候选稿，未覆盖当前版本')
        invalidate(['sections'])
      } else setSaveError(errorText(e))
      return false
    } finally { setSaving(false) }
  }, [draft, s, saving, taskId, sectionId, toast, setDraft, invalidate])

  const ask = useCallback((title: string, message: ReactNode, saveLabel: string) =>
    new Promise<Choice>((resolve) => setChoice({ title, message, saveLabel, resolve: (c) => { setChoice(null); resolve(c) } })), [])

  /** 有未保存修改时先明确处理草稿：保存、放弃或取消。返回 true 表示可以继续。 */
  const settleDraft = useCallback(async (what: string): Promise<boolean> => {
    if (!isDirty(draft)) return true
    const c = await ask('当前章节有未保存的修改', <>继续{what}前，请先处理草稿。保存会提交为新修订（基于修订 {draft!.baseRev}）。</>, '保存后继续')
    if (c === 'cancel') return false
    if (c === 'discard') { setDraft(null); setMode('preview'); setConflict(undefined); setSaveError(''); return true }
    return save()
  }, [draft, ask, save, setDraft])

  // 离开保护：切换章节、页面或浏览器后退 / 前进时提示；同一章节内只改查询参数（面板、历史查看）不拦截。
  const draftRef = useRef(draft)
  draftRef.current = draft
  const settleRef = useRef(settleDraft)
  settleRef.current = settleDraft
  useEffect(() => registerLeaveGuard(async (to) => {
    if (!isDirty(draftRef.current)) return true
    if (new URL(to, window.location.origin).pathname === window.location.pathname) return true
    return settleRef.current('离开本章节')
  }), [])
  useEffect(() => {
    if (!dirty) return
    const on = (e: BeforeUnloadEvent) => { e.preventDefault(); e.returnValue = '' }
    window.addEventListener('beforeunload', on)
    return () => window.removeEventListener('beforeunload', on)
  }, [dirty])

  if (det.loading && !d) return <div className="col-span-full"><Spinner /></div>
  if (!d || !s) {
    const missing = det.error.startsWith('HTTP 404')
    return (
      <div className="col-span-full space-y-3">
        <Notice tone="danger">{missing ? `本项目中没有章节 #${sectionId}（可能已被新版本大纲取代或不属于本项目）。` : '章节读取失败。'}</Notice>
        <ErrorBanner error={det.error} onRetry={missing ? undefined : det.reload} />
      </div>
    )
  }

  const startEdit = () => {
    if (!draft) setDraft(newDraft(d.current_revision, s.current_rev_no, s.outline_version))
    setMode('edit')
    if (revParam) setQuery({ rev: undefined, compare: undefined })
  }
  const updateDraft = (patch: Partial<Draft>) => setDraft((p) => (p ? { ...p, ...patch } : p))
  const viewing: SectionRevision | undefined = revParam && !Number.isNaN(revParam) ? d.revisions.find((r) => r.rev_no === revParam) : undefined

  const act = async (k: string, what: string, fn: () => Promise<unknown>, ok: string, action: Parameters<typeof invalidate>[0]) => {
    if (!(await settleDraft(what))) return
    setBusy(k)
    try { await fn(); toast('ok', ok); setConflict(undefined); invalidate(action) } catch (e) { toast('error', errorText(e)); invalidate(['sections']) } finally { setBusy('') }
  }

  const dispatchDisabled = !d.writable ? (d.blocked_reason || '当前不可写作') : s.writing ? 'AI 正在撰写本章节' : !s.dependencies_ready ? `前置章节未就绪：${s.unmet_dependencies.join('、')}` : ''
  const current = d.current_revision
  const canApprove = !!current && current.rev_no === s.current_rev_no && (current.status === 'draft' || current.status === 'reviewing') && s.approved_rev_no !== current.rev_no
  const statusText = saving ? '保存中…' : state === 'behind' ? `有未保存修改 · 当前版本已更新为修订 ${s.current_rev_no}` : dirty ? '有未保存修改'
    : conflict ? '修改已归档为冲突候选稿' : lastSaved ? '已保存' : current ? `当前正文 · 修订 ${s.current_rev_no}` : '尚无正文'

  // ---------- 正文区 ----------
  const center = (
    <section className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-[10px] border border-line bg-panel" aria-label="章节正文">
      <div className="flex min-h-12 flex-wrap items-center gap-2 border-b border-line px-4 py-2">
        {tocButton}
        <h2 className="min-w-0 flex-1 truncate text-sm font-semibold" title={`${s.section_code} ${s.title}`}>{s.section_code} {s.title}</h2>
        <Badge dot tone={sectionTone(s.status)}>{sectionStatusLabel[s.status] ?? s.status}</Badge>
        {s.writing && <Badge dot tone="purple">AI 撰写中</Badge>}
        {layout !== 'three' && <Button size="sm" icon={<PanelRight className="size-3.5" />} onClick={() => setPanelDrawer(true)}>写作依据</Button>}
        <Button size="sm" variant="primary" icon={<Save className="size-3.5" />} loading={saving} disabled={!dirty || !d.writable}
          title={!d.writable ? d.blocked_reason : !dirty ? '没有未保存的修改' : ''} onClick={() => void save()}>保存</Button>
      </div>
      {(state === 'behind' || conflict || saveError || d.warnings.length > 0 || (d.blocked_reason && !d.writable)) && (
        <div className="space-y-2 border-b border-line p-3">
          {d.blocked_reason && !d.writable && <Notice>{d.blocked_reason}</Notice>}
          {state === 'behind' && !conflict && (
            <Notice action={<span className="flex flex-wrap gap-1">
              <Button size="sm" onClick={() => setQuery({ rev: s.current_rev_no, compare: '1' })}>查看最新</Button>
              <Button size="sm" onClick={() => setDraft((p) => (p ? rebase(p, d.current_revision, s.current_rev_no, s.outline_version) : p))}>以最新为基线继续编辑</Button>
            </span>}>
              编辑期间主版本已更新为修订 {s.current_rev_no}{draft && draft.baseOutline !== s.outline_version ? `（大纲 v${s.outline_version}）` : ''}；你的草稿仍基于修订 {draft?.baseRev}，保存会被识别为冲突并存为候选稿。
            </Notice>
          )}
          {conflict && (
            <Notice tone="danger" action={<span className="flex flex-wrap gap-1">
              <Button size="sm" onClick={() => setQuery({ rev: conflict.revision.rev_no, compare: '1', panel: 'history' })}>对比候选稿</Button>
              <Button size="sm" onClick={() => { setDraft((p) => (p ? rebase(p, d.current_revision, s.current_rev_no, s.outline_version) : p)); setConflict(undefined) }}>以最新为基线重新编辑</Button>
            </span>}>
              {conflict.outcome === 'stale_outline' ? '大纲已变更' : '版本冲突'}：修改已归档为候选稿修订 {conflict.revision.rev_no}（{conflict.reason}），当前主版本仍为修订 {conflict.section.current_rev_no}，未被覆盖。草稿保留在编辑器中。
            </Notice>
          )}
          {saveError && <ErrorBanner error={`保存失败，草稿仍保留：${saveError}`} />}
          {d.warnings.length > 0 && <Notice tone="danger"><div className="font-medium">当前修订的证据存在问题，不能作为无缺口定稿：</div>
            <ul className="mt-1 list-disc pl-5 text-xs">{d.warnings.map((w) => <li key={w}>{w}</li>)}</ul></Notice>}
        </div>
      )}
      {viewing && comparing ? (
        <>
          <div className="flex flex-wrap items-center gap-2 border-b border-line bg-soft px-4 py-2 text-xs">
            <span>对比：{draft && viewing.status !== 'conflict_candidate' ? '当前草稿' : `当前主版本修订 ${s.current_rev_no}`} ↔ 修订 {viewing.rev_no}（{revisionStatusLabel[viewing.status] ?? viewing.status}）</span>
            <span className="flex-1" /><Button size="sm" onClick={() => setQuery({ rev: undefined, compare: undefined })}>关闭对比</Button>
          </div>
          <RevisionCompare left={draft && viewing.status !== 'conflict_candidate' ? draft.content : (current ?? '')} right={viewing}
            leftLabel={draft && viewing.status !== 'conflict_candidate' ? '当前草稿（未保存）' : `当前主版本 · 修订 ${s.current_rev_no}`} rightLabel={`修订 ${viewing.rev_no} · ${revisionSourceLabel[viewing.source]}`} />
        </>
      ) : viewing ? (
        <>
          <div className="flex flex-wrap items-center gap-2 border-b border-line bg-soft px-4 py-2 text-xs">
            <Badge tone={revisionTone(viewing.status)}>{revisionStatusLabel[viewing.status] ?? viewing.status}</Badge>
            <span>只读历史修订 {viewing.rev_no} · {revisionSourceLabel[viewing.source]} · {viewing.created_by || '—'} {fmtTime(viewing.created_at)}</span>
            <span className="flex-1" />
            {viewing.rev_no !== s.current_rev_no && <Button size="sm" onClick={() => setQuery({ compare: '1' })}>与当前对比</Button>}
            <Button size="sm" onClick={() => setQuery({ rev: undefined, compare: undefined })}>返回当前</Button>
          </div>
          <div className="min-h-0 flex-1 overflow-auto p-6"><Markdown source={viewing.content_md} /></div>
        </>
      ) : mode === 'edit' && draft ? (
        <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-auto p-4">
          <TextArea aria-label="章节正文（Markdown）" className="min-h-[320px] flex-1 font-mono text-[13px] leading-6" value={draft.content} readOnly={saving}
            onChange={(e) => updateDraft({ content: e.target.value })} placeholder="Markdown 正文" />
          <div className="grid gap-3 md:grid-cols-2">
            <Field label="衔接摘要" hint="供依赖本章节的后续章节参考"><TextArea rows={2} value={draft.summary} readOnly={saving} onChange={(e) => updateDraft({ summary: e.target.value })} /></Field>
            <Field label="修改说明" hint="作者填写的说明，不是自动计算的差异"><TextArea rows={2} value={draft.diffNote} readOnly={saving} onChange={(e) => updateDraft({ diffNote: e.target.value })} /></Field>
          </div>
        </div>
      ) : (
        <div className="min-h-0 flex-1 overflow-auto p-6">
          {revParam !== undefined && !viewing && <Notice tone="danger" className="mb-3">本章节没有修订 {String(params.get('rev'))}。</Notice>}
          <div className={cx('mb-4 text-xs', draft ? 'text-warn' : 'text-muted')}>{draft ? `草稿预览（未保存，基于修订 ${draft.baseRev}）` : `当前正文 · 修订 ${s.current_rev_no}`}
            {s.approved_rev_no ? ` · 定稿修订 ${s.approved_rev_no}` : ' · 未定稿'}{s.target_words ? ` · 目标 ${s.target_words} 字` : ''}</div>
          {s.writing_guide && <p className="mb-4 rounded-lg bg-soft px-3 py-2 text-xs text-muted">写作要点：{s.writing_guide}</p>}
          {(draft ? draft.content : current?.content_md) ? <Markdown source={draft ? draft.content : current!.content_md} />
            : <Empty>本章节尚无正文。可「编辑正文」人工撰写，或在前置条件满足时派发 AI 撰写。</Empty>}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2 border-t border-line px-3 py-2.5 text-xs">
        <span className={cx('min-w-0 flex-1 truncate', state === 'behind' || conflict ? 'text-warn' : 'text-muted')} role="status">{statusText}{mode === 'edit' && !viewing ? ' · 编辑' : ' · 预览'}</span>
        {mode === 'edit' && draft
          ? <Button size="sm" icon={<Eye className="size-3.5" />} onClick={() => setMode('preview')}>预览草稿</Button>
          : <Button size="sm" icon={<Pencil className="size-3.5" />} disabled={!d.writable} title={!d.writable ? d.blocked_reason : ''} onClick={startEdit}>{draft ? '继续编辑' : '编辑正文'}</Button>}
        <Button size="sm" icon={<Sparkles className="size-3.5" />} disabled={!!dispatchDisabled} title={dispatchDisabled} loading={busy === 'dispatch'}
          onClick={() => setAIOpen(true)}>{s.current_rev_no ? 'AI 重写' : 'AI 撰写初稿'}</Button>
        <Button size="sm" icon={<CheckCircle2 className="size-3.5" />} disabled={!canApprove || !d.writable}
          title={!canApprove ? (s.approved_rev_no === s.current_rev_no && s.current_rev_no ? '当前修订已定稿' : '当前修订不可批准') : ''}
          loading={busy === 'approve'}
          onClick={() => void act('approve', '批准定稿', () => api.approveRevision(taskId, sectionId, s.current_rev_no), `修订 ${s.current_rev_no} 已批准定稿`, 'section_approved')}>批准定稿</Button>
      </div>
      {dispatchDisabled && <p className="border-t border-line px-3 py-1.5 text-xs text-muted">AI 撰写不可用：{dispatchDisabled}</p>}
    </section>
  )

  // ---------- 写作依据 ----------
  const preferredRuns = [...new Set([...d.requirements.map((r) => r.evidence_run_id).filter((x): x is number => !!x), ...d.evidence.map((e) => e.run_id)])]
  const panelBody: Record<WritingPanel, ReactNode> = {
    requirements: (
      <div className="space-y-3 p-3">
        {d.dependencies.length > 0 && (
          <section>
            <h3 className="mb-1.5 text-xs font-medium text-muted">前置依赖</h3>
            <ul className="space-y-1">{d.dependencies.map((dep) => (
              <li key={dep.id} className="text-xs">
                <a {...linkProps(taskUrl(taskId, 'writing', { sectionId: dep.id }))} className="hover:underline">
                  {dep.ready ? <CheckCircle2 className="mr-1 inline size-3.5 text-ok" aria-label="已就绪" /> : <XCircle className="mr-1 inline size-3.5 text-warn" aria-label="未就绪" />}
                  {dep.section_code} {dep.title} · 修订 {dep.current_rev_no}</a>
                {dep.summary_note && <p className="ml-5 text-muted">{dep.summary_note}</p>}
              </li>))}</ul>
          </section>
        )}
        <h3 className="text-xs font-medium text-muted">绑定要求（{d.requirements.length}）</h3>
        {d.requirements.length === 0 ? <p className="text-xs text-muted">无（概述 / 衔接章节）</p> : d.requirements.map((r) => (
          <article key={r.id} className="rounded-[10px] border border-line p-3">
            <div className="flex items-start justify-between gap-2">
              <a {...linkProps(taskUrl(taskId, 'requirements', { query: { requirement: r.id } }))} className="font-semibold hover:underline">{r.is_mandatory && <span className="text-danger">★</span>}{r.code}</a>
              <Badge dot tone={r.review_status === 'confirmed' ? 'green' : r.review_status === 'needs_revision' ? 'red' : 'amber'}>{reviewStatusLabel[r.review_status]}</Badge>
            </div>
            <p className="my-1.5 text-sm">{r.title}</p>
            {r.source_chunk_id ? <a {...linkProps(taskUrl(taskId, 'documents', { query: { doc: r.source_loc.doc_id ?? r.source_doc_id, chunk: r.source_chunk_id } }))} className="text-xs text-link hover:underline">
              招标原文 {r.source_loc.section_code} {pageRange(r.source_loc.page_start, r.source_loc.page_end)}</a> : <span className="text-xs text-muted">无原文绑定</span>}
            {r.evidence_gap ? <p className="mt-1 text-xs text-warn">证据缺口：{r.evidence_gap}</p>
              : !r.evidence_ids.length ? <p className="mt-1 text-xs text-warn">尚未检索证据 · <a {...linkProps(taskUrl(taskId, 'research'))} className="underline">去检索</a></p>
              : <p className="mt-1 text-xs text-muted">已绑定 {r.evidence_ids.length} 条证据（检索运行 #{r.evidence_run_id}）</p>}
          </article>
        ))}
      </div>
    ),
    evidence: (
      <div className="space-y-4 p-3">
        <section className="space-y-2">
          <h3 className="text-xs font-medium text-muted">当前修订引用的证据（{d.evidence.length}）</h3>
          {d.evidence.length === 0 ? <p className="text-xs text-muted">当前修订未引用证据。</p> : d.evidence.map((e) => (
            <EvidenceStateRow key={e.id} e={e} highlight={evidenceParam === e.id} onOpen={() => setEvidenceView({ run: e.run_id, id: e.id })} />))}
          {evidenceParam !== undefined && !d.evidence.some((e) => e.id === evidenceParam) && <Notice tone="warn">当前修订没有引用证据 {String(params.get('evidence'))}；它可能属于旧修订或已被删除。</Notice>}
          <p className="text-xs text-muted">「指标已核验 · 支持」只说明该条指标已核验，不代表整项要求已完全满足。</p>
        </section>
        <section className="space-y-2">
          <h3 className="text-xs font-medium text-muted">草稿引用证据{draft ? `（已选 ${draft.evidenceIds.length}）` : ''}</h3>
          {draft ? (
            <EvidencePicker taskId={taskId} preferredRuns={preferredRuns} selected={draft.evidenceIds}
              onToggle={(id, on) => setDraft((p) => (p ? toggleEvidence(p, id, on) : p))} onOpen={(run, id) => setEvidenceView({ run, id })} />
          ) : <Button size="sm" disabled={!d.writable} onClick={startEdit}>开始编辑以选择证据</Button>}
        </section>
      </div>
    ),
    history: (
      <div className="space-y-2 p-3">
        {d.revisions.length === 0 ? <Empty>尚无修订</Empty> : d.revisions.map((r) => {
          const isCurrent = r.rev_no === s.current_rev_no
          const candidate = r.status === 'conflict_candidate'
          return (
            <article key={r.id} className={cx('rounded-[10px] border p-3', viewing?.rev_no === r.rev_no ? 'border-fg' : 'border-line', isCurrent && 'bg-soft')}>
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="font-semibold">修订 {r.rev_no}</span>
                {isCurrent && <Badge tone="blue">当前</Badge>}
                {s.approved_rev_no === r.rev_no && <Badge tone="green">定稿</Badge>}
                <Badge tone={revisionTone(r.status)}>{revisionStatusLabel[r.status] ?? r.status}</Badge>
                {r.gaps.length > 0 && <Badge tone="amber">{r.gaps.length} 项缺口</Badge>}
              </div>
              <div className="mt-1 text-xs text-muted">{revisionSourceLabel[r.source]}{r.rollback_of_rev_no ? ` ← 修订 ${r.rollback_of_rev_no}` : ''} · 基于修订 {r.base_rev_no} · 大纲 v{r.based_on_outline_version}</div>
              <div className="text-xs text-muted">{r.created_by || '—'} · {fmtTime(r.created_at)}</div>
              {r.diff_summary && <p className="mt-1 text-xs">修改说明：{r.diff_summary}</p>}
              <div className="mt-2 flex flex-wrap gap-1">
                <Button size="sm" onClick={() => setQuery({ rev: r.rev_no, compare: undefined })}>查看</Button>
                {!isCurrent && <Button size="sm" onClick={() => setQuery({ rev: r.rev_no, compare: '1' })}>对比</Button>}
                {!isCurrent && r.status !== 'stale_outline' && <Button size="sm" icon={<RotateCcw className="size-3.5" />} loading={busy === `b${r.rev_no}`} disabled={!d.writable}
                  onClick={() => void act(`b${r.rev_no}`, candidate ? '采用候选稿' : '回滚', () => api.rollbackRevision(taskId, sectionId, r.rev_no, s.current_rev_no),
                    candidate ? `已采用候选稿修订 ${r.rev_no}` : `已回滚至修订 ${r.rev_no}`, 'section_rolled_back')}>{candidate ? '采用候选稿' : '回滚至此'}</Button>}
              </div>
            </article>
          )
        })}
        <p className="text-xs text-muted">修订记录不可篡改；回滚 / 采用候选稿会基于当前主版本生成新的回滚修订，并撤销原有定稿批准。</p>
      </div>
    ),
    charts: <div className="p-3"><ChartsCard taskID={taskId} sectionID={sectionId} currentRevNo={s.current_rev_no} writable={d.writable} tick={tick} /></div>,
  }
  const panelTabs = (
    <div role="tablist" aria-label="写作依据" className="scrollbar-none flex gap-4 overflow-x-auto border-b border-line px-4">
      {WRITING_PANELS.map((p) => (
        <button key={p} type="button" role="tab" aria-selected={panel === p} onClick={() => setQuery({ panel: p === 'requirements' ? undefined : p })}
          className={cx('flex h-8 items-center gap-1 border-b-2 text-xs font-medium whitespace-nowrap', panel === p ? 'border-fg text-fg' : 'border-transparent text-muted hover:text-fg')}>
          {panelMeta[p].icon}{panelMeta[p].label}
          {p === 'evidence' && d.evidence.some((e) => !e.usable) && <span className="size-1.5 rounded-full bg-danger" aria-label="有不可用证据" />}
        </button>
      ))}
    </div>
  )
  const side = (
    <aside className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-[10px] border border-line bg-panel" aria-label="写作依据">
      <div className="border-b border-line px-4 py-3"><h2 className="text-sm font-semibold">写作依据</h2><p className="text-xs text-muted">要求与证据分别核对</p></div>
      {panelTabs}
      <div className="min-h-0 flex-1 overflow-y-auto" role="tabpanel">{panelBody[panel]}</div>
    </aside>
  )

  return (
    <>
      {center}
      {layout === 'three' ? side : (
        <Drawer open={panelDrawer} title="写作依据" onClose={() => setPanelDrawer(false)} width={360}>
          {panelTabs}
          {panelBody[panel]}
        </Drawer>
      )}
      <ChoiceDialog req={choice} saving={saving} />
      <EvidenceDetailDrawer runId={evidenceView?.run} evidenceId={evidenceView?.id} onClose={() => setEvidenceView(undefined)} onChanged={() => invalidate('evidence_verified')} />
      <Modal open={aiOpen} title={s.current_rev_no ? 'AI 重写本章节' : 'AI 撰写初稿'} onClose={() => setAIOpen(false)}
        footer={<><Button onClick={() => setAIOpen(false)}>取消</Button>
          <Button variant="primary" icon={<Sparkles className="size-4" />} loading={busy === 'dispatch'} disabled={!!dispatchDisabled}
            onClick={async () => {
              setAIOpen(false)
              await act('dispatch', '派发 AI 撰写', () => api.dispatchSection(taskId, sectionId, { base_rev_no: s.current_rev_no, instruction }),
                '已派发 AI 撰写；正文写回后才会显示，结果以服务器返回为准', ['sections'])
              setInstruction('')
            }}>派发</Button></>}>
        <div className="space-y-3">
          <Field label="重写指令（可选）"><Input autoFocus value={instruction} onChange={(e) => setInstruction(e.target.value)} placeholder="如：补充运维响应时限，压缩到 1500 字" /></Field>
          <p className="text-xs text-muted">派发时快照修订 {s.current_rev_no} 与大纲 v{s.outline_version}；期间若有人保存新版本或大纲变更，AI 稿只会成为候选稿，不覆盖主稿。会调用模型并产生费用；模型不可用时返回错误，不会自动重试。</p>
        </div>
      </Modal>
    </>
  )
}
