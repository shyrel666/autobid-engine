import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { CheckCircle2, FileSearch, Pencil, Plus, RefreshCw, ShieldCheck, Trash2 } from 'lucide-react'
import { api, errorText } from '../../../api/client'
import type { ExtractRequirementsResp, RequirementCategory, RequirementInput, RequirementItem, ScoreGroup, ScoringSummary } from '../../../api/types'
import {
  Badge, Button, Card, Empty, ErrorBanner, Field, IconButton, Input, Modal, Notice, Select, Spinner, Stat, TextArea, cx, useConfirm, useToast,
} from '../../../components/ui'
import type { Tone } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'
import { useScrollTo } from '../../../hooks/useScrollTo'
import {
  evalKindLabel, fmtTime, manifestStatusLabel, pageRange, requirementCategoryLabel, reviewStatusLabel, scoreGroupLabel,
} from '../../../lib/format'
import { linkProps, useSearchParams } from '../../../lib/router'
import { REQUIREMENT_FILTERS, optionalId, pickEnum, taskUrl } from '../../../lib/routes'
import type { RequirementFilter } from '../../../lib/routes'
import { BidScopeCard, VetoChecklist } from '../../../pages/task/VetoPanel'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import { EvidenceDetailDrawer, EvidencePicker } from '../evidence/Evidence'

// 招标解读 · 要求与否决项：投标范围、否决项、评分汇总与可复核清单（原「① 投标要求清单复核」）。
// 统计优先使用服务端口径；价格分、不投分包与否决项不计入技术标章节覆盖分母。

const categories: RequirementCategory[] = ['veto_item', 'mandatory_clause', 'scoring_item', 'non_scoring_required', 'format_attachment']
const scoreGroups: ScoreGroup[] = ['technical', 'commercial', 'price', '']

const filterLabel: Record<RequirementFilter, string> = {
  all: '全部', veto: '否决项', scoring: '评分项', needs_revision: '原文变更待复核', evidence_gap: '证据缺口 / 未检索',
}

function scoreGroupTone(g: ScoreGroup): Tone {
  return g === 'technical' ? 'blue' : g === 'commercial' ? 'purple' : g === 'price' ? 'slate' : 'amber'
}

function matches(it: RequirementItem, f: RequirementFilter): boolean {
  switch (f) {
    case 'veto': return it.category === 'veto_item'
    case 'scoring': return it.category === 'scoring_item'
    case 'needs_revision': return it.review_status === 'needs_revision'
    case 'evidence_gap': return !!it.evidence_gap || it.evidence_ids.length === 0
    default: return true
  }
}

function ScoringSummaryCard({ scoring }: { scoring: ScoringSummary[] }) {
  return (
    <Card title="评分表分值核对" subtitle="评分项由规则从评分表提取，未经人工标注评测；请对照原文复核名称、分值与分组。多分包招标请在「投标范围」中选择本次投标分包。">
      <div className="space-y-2">
        {scoring.map((s) => (
          <div key={s.lot} className="rounded-[10px] border border-line px-3 py-2 text-sm">
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-medium">{s.lot ? `包${s.lot}` : '评分表'}</span>
              <Badge dot tone={s.total === 100 && s.warnings.length === 0 ? 'green' : 'amber'}>合计 {s.total} 分 · {s.items} 项</Badge>
              <span className="text-xs text-muted">技术 {s.technical} · 商务 {s.commercial} · 价格 {s.price}{s.unclassified ? ` · 未分类 ${s.unclassified}` : ''}</span>
              {s.groups.filter((g) => g.declared > 0).map((g) => (
                <span key={g.group} className={cx('text-xs', Math.abs(g.declared - g.extracted) > 0.05 ? 'text-warn' : 'text-muted')}>{g.group} {g.extracted}/{g.declared}</span>
              ))}
            </div>
            {s.warnings.map((w) => <p key={w} className="mt-1 text-xs text-warn">⚠ {w}</p>)}
          </div>
        ))}
      </div>
    </Card>
  )
}

function ExtractSummary({ taskId, resp, onClose }: { taskId: number; resp: ExtractRequirementsResp; onClose: () => void }) {
  const r = resp.result
  const goals = resp.goals ?? []
  return (
    <Notice tone={r.orphaned ? 'warn' : 'info'} action={<Button size="sm" variant="ghost" onClick={onClose}>收起</Button>}>
      <div className="font-medium">提取与目标分解完成：遍历 {r.scanned_chunks} 个原文分块（提取成功不等于清单已确认）</div>
      <div className="mt-1 text-xs">新增 {r.created} · 原文变更 {r.updated} · 未变 {r.unchanged} · 保留人工项 {r.skipped_manual} · 评分项 {r.scoring_items} · 否决项 {r.veto_items}
        {r.superseded ? ` · 更正/澄清覆盖旧要求 ${r.superseded}` : ''}
        {r.bid_deadline ? ' · 识别到投标截止时间' : ''}
        {resp.synced_assets ? ` · 同步招标静态要素 ${resp.synced_assets} 个` : ''}</div>
      {goals.length > 0 && (
        <div className="mt-1 text-xs">
          已初始化推演图谱 Goal 根节点：{goals.map((g) => `${g.code}「${g.title}」(#${g.node_id})`).join('、')}
        </div>
      )}
      {resp.outline_created && resp.outline_version && (
        <div className="mt-1 text-xs">
          已自动生成技术标目录大纲草案 <a {...linkProps(taskUrl(taskId, 'outline'))} className="font-medium text-link underline">v{resp.outline_version}（去查看）</a>
        </div>
      )}
      {r.orphaned > 0 && <div className="mt-1 text-xs">{r.orphaned} 条旧提取项本次未再产出，已标记需复核：{r.orphan_codes.join('、')}</div>}
    </Notice>
  )
}

function RequirementEditor({ taskId, item, onClose, onSaved }: { taskId: number; item?: RequirementItem | 'new'; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const blank = { code: '', category: 'non_scoring_required' as RequirementCategory, title: '', description: '', is_mandatory: false, score_weight: '0',
    score_group: '' as ScoreGroup, source_chunk_id: '', evidence_run_id: undefined as number | undefined, evidence_ids: [] as number[], evidence_gap: '', review_status: 'draft' }
  const [f, setF] = useState(blank)
  const [initial, setInitial] = useState(JSON.stringify(blank))
  const [busy, setBusy] = useState(false)
  const [detail, setDetail] = useState<{ run: number; id: number }>()
  useEffect(() => {
    if (!item) return
    const v = item === 'new' ? blank : {
      code: item.code, category: item.category, title: item.title, description: item.description, is_mandatory: item.is_mandatory,
      score_weight: String(item.score_weight), score_group: item.score_group, source_chunk_id: item.source_chunk_id ? String(item.source_chunk_id) : '',
      evidence_run_id: item.evidence_run_id, evidence_ids: item.evidence_ids, evidence_gap: item.evidence_gap, review_status: item.review_status,
    }
    setF(v); setInitial(JSON.stringify(v))
  }, [item]) // eslint-disable-line react-hooks/exhaustive-deps
  if (!item) return null
  const save = async () => {
    const req: RequirementInput = {
      code: f.code.trim(), category: f.category, title: f.title.trim(), description: f.description, is_mandatory: f.is_mandatory,
      score_weight: Number(f.score_weight) || 0, score_group: f.category === 'scoring_item' ? f.score_group : '',
      source_chunk_id: f.source_chunk_id ? Number(f.source_chunk_id) : 0,
      evidence_run_id: f.evidence_run_id ?? 0, evidence_ids: f.evidence_ids, evidence_gap: f.evidence_gap, review_status: f.review_status,
    }
    setBusy(true)
    try {
      if (item === 'new') await api.createRequirement(taskId, req)
      else await api.updateRequirement(taskId, item.id, req)
      toast('ok', '已保存要求')
      onSaved()
    } catch (e) { toast('error', errorText(e)) } finally { setBusy(false) }
  }
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value })
  return (
    <Modal open title={item === 'new' ? '新增投标要求' : `编辑要求 ${item.code}`} onClose={onClose} wide dirty={JSON.stringify(f) !== initial}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={save} disabled={!f.code.trim() || !f.title.trim()}>保存</Button></>}>
      <div className="grid gap-3 md:grid-cols-3">
        <Field label="编码"><Input value={f.code} onChange={set('code')} placeholder="如 SC-3.1、MAN-01" /></Field>
        <Field label="类别"><Select value={f.category} onChange={set('category')}>
          {categories.map((c) => <option key={c} value={c}>{requirementCategoryLabel[c]}</option>)}</Select></Field>
        <Field label="分值"><Input type="number" min={0} value={f.score_weight} onChange={set('score_weight')} /></Field>
        <Field label="评分分组" hint="价格分由报价文件响应，不进入技术标大纲">
          <Select value={f.score_group} onChange={set('score_group')} disabled={f.category !== 'scoring_item'}>
            {scoreGroups.map((g) => <option key={g} value={g}>{scoreGroupLabel[g]}</option>)}</Select></Field>
        <Field label="标题" className="md:col-span-2"><Input value={f.title} onChange={set('title')} /></Field>
        <Field label="要求原文 / 说明" className="md:col-span-3"><TextArea rows={3} value={f.description} onChange={set('description')} /></Field>
        <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={f.is_mandatory} onChange={(e) => setF({ ...f, is_mandatory: e.target.checked })} />★/▲ 实质性（废标）条款</label>
        <Field label="原文分块编号" hint="须属于本项目的招标原文；留空为解除绑定"><Input value={f.source_chunk_id} onChange={set('source_chunk_id')} /></Field>
        <Field label="复核状态"><Select value={f.review_status} onChange={set('review_status')}>
          {Object.entries(reviewStatusLabel).map(([k, l]) => <option key={k} value={k}>{l}</option>)}</Select></Field>
        <div className="md:col-span-3">
          <div className="mb-1.5 text-sm">绑定证据</div>
          <EvidencePicker taskId={taskId} preferredRuns={f.evidence_run_id ? [f.evidence_run_id] : []} runId={f.evidence_run_id}
            onRunChange={(r) => setF((x) => ({ ...x, evidence_run_id: r, evidence_ids: r === x.evidence_run_id ? x.evidence_ids : [] }))}
            selected={f.evidence_ids} onToggle={(id, on) => setF((x) => ({ ...x, evidence_ids: on ? [...new Set([...x.evidence_ids, id])] : x.evidence_ids.filter((y) => y !== id) }))}
            onOpen={(run, id) => setDetail({ run, id })} />
          <p className="mt-1 text-xs text-muted">一条要求绑定一个检索运行内的证据；更换运行会清空已选证据。</p>
        </div>
        <Field label="证据缺口说明" hint="查无证据或证据不足时记录缺口，不得删除要求" className="md:col-span-3"><TextArea rows={2} value={f.evidence_gap} onChange={set('evidence_gap')} /></Field>
      </div>
      <EvidenceDetailDrawer runId={detail?.run} evidenceId={detail?.id} onClose={() => setDetail(undefined)} />
    </Modal>
  )
}

function SourceLink({ taskId, it }: { taskId: number; it: RequirementItem }) {
  if (!it.source_chunk_id) return <span className="text-muted">无原文绑定</span>
  const doc = it.source_loc.doc_id ?? it.source_doc_id
  const spreadsheet = it.source_loc.kind === 'spreadsheet'
  return (
    <a {...linkProps(taskUrl(taskId, 'documents', { query: { doc, chunk: it.source_chunk_id } }))} className="text-link hover:underline">
      {spreadsheet
        ? `${it.source_loc.sheet ?? '工作表'} ${it.source_loc.range ?? ''}`
        : `${it.source_loc.section_code || '原文'} ${pageRange(it.source_loc.page_start, it.source_loc.page_end)}`}
      {it.source_loc.table !== undefined && ` · 表${Number(it.source_loc.table) + 1}行${Number(it.source_loc.row) + 1}`}
    </a>
  )
}

export default function RequirementsPage() {
  const toast = useToast()
  const confirm = useConfirm()
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('requirements', 'documents')
  const data = useAsync(() => api.requirements(taskId), [taskId, tick])
  const [params, setQuery] = useSearchParams()
  const filter = pickEnum(params.get('filter'), REQUIREMENT_FILTERS, 'all')
  const target = optionalId(params, 'requirement')
  const [category, setCategory] = useState<'' | RequirementCategory>('')
  const [editing, setEditing] = useState<RequirementItem | 'new'>()
  const [evidence, setEvidence] = useState<{ run: number; id: number }>()
  const [busy, setBusy] = useState('')
  const [extracted, setExtracted] = useState<ExtractRequirementsResp>()

  const v = data.data
  const sourcePending = !!v && v.manifest.source_epoch > v.manifest.extracted_source_epoch
  const items = (v?.items ?? []).filter((i) => matches(i, filter) && (!category || i.category === category))
  const targetItem = target && !Number.isNaN(target) ? v?.items.find((i) => i.id === target) : undefined
  useScrollTo(targetItem ? `req-${targetItem.id}` : undefined, !!v)

  const run = async (key: string, fn: () => Promise<unknown>, ok: string, action: Parameters<typeof invalidate>[0] = 'requirements_changed') => {
    setBusy(key)
    try { await fn(); toast('ok', ok); invalidate(action) } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  if (data.loading && !v) return <Spinner />
  if (!v) return <ErrorBanner error={data.error} onRetry={data.reload} />
  const s = v.stats
  const codeById = new Map(v.items.map((i) => [i.id, i.code]))
  const denominator = s.total - s.out_of_scope - s.out_of_bid - s.veto.total - (s.superseded ?? 0)

  let locate: ReactNode = null
  if (target !== undefined) {
    if (Number.isNaN(target)) locate = <Notice tone="danger">定位参数无效：requirement 必须是正整数。</Notice>
    else if (!targetItem) locate = <Notice tone="danger">本项目中未找到要求 #{target}（可能已删除或属于其他项目）。</Notice>
    else if (!items.includes(targetItem)) locate = (
      <Notice tone="warn" action={<Button size="sm" onClick={() => { setCategory(''); setQuery({ filter: undefined }) }}>显示全部</Button>}>
        要求 {targetItem.code} 不在当前筛选结果中。</Notice>)
  }

  const rowTable = (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[960px] text-sm">
        <thead><tr className="border-b border-line text-left text-xs text-muted">
          <th className="py-2 pr-2 font-medium">编码</th><th className="pr-2 font-medium">类别</th><th className="pr-2 font-medium">要求</th><th className="pr-2 font-medium">原文位置</th>
          <th className="pr-2 font-medium">证据 / 缺口</th><th className="pr-2 font-medium">映射章节</th><th className="pr-2 font-medium">复核</th><th><span className="sr-only">操作</span></th>
        </tr></thead>
        <tbody>
          {items.map((it) => (
            <tr key={it.id} id={`req-${it.id}`} className={cx('border-b border-line-soft align-top', !!it.superseded_by_id && 'opacity-60', target === it.id && 'bg-soft outline outline-1 outline-fg')}>
              <td className="py-2 pr-2 font-mono text-xs">
                {it.code}
                {it.origin === 'manual' && <div className="mt-1"><Badge>人工</Badge></div>}
                {it.superseded_by_id && (
                  <div className="mt-1">
                    <Badge tone="slate">已被 {it.superseded_by_code || `#${it.superseded_by_id}`} 覆盖</Badge>
                  </div>
                )}
              </td>
              <td className="pr-2">
                <Badge tone={it.category === 'mandatory_clause' || it.category === 'veto_item' ? 'red' : it.category === 'scoring_item' ? 'blue' : 'slate'}>{requirementCategoryLabel[it.category]}</Badge>
                {it.category === 'scoring_item' && <div className="mt-1"><Badge tone={scoreGroupTone(it.score_group)}>{scoreGroupLabel[it.score_group]}</Badge></div>}
                {it.eval_kind && evalKindLabel[it.eval_kind] && (
                  <div className="mt-1"><Badge tone={it.eval_kind === 'objective' ? 'green' : 'amber'}>{evalKindLabel[it.eval_kind]}</Badge></div>
                )}
                {it.lot && <div className="mt-1 text-xs text-muted">包{it.lot}{it.out_of_bid ? '（不投）' : ''}</div>}
              </td>
              <td className="max-w-md pr-2">
                <div className="font-medium">{it.is_mandatory && <span className="text-danger">★ </span>}{it.title}
                  {it.score_weight > 0 && <span className="ml-1 text-xs text-link">{it.score_weight} 分</span>}</div>
                {it.proof_rules?.raw_summary && (
                  <p className="mt-0.5 text-xs text-amber-700">证明要求：{it.proof_rules.raw_summary}</p>
                )}
                {it.scoring_tiers && it.scoring_tiers.length > 0 && (
                  <details className="mt-0.5">
                    <summary className="cursor-pointer text-xs text-link">评分档次与判据（{it.scoring_tiers.length} 档）</summary>
                    <ul className="mt-1 list-disc pl-4 text-xs text-slate-600">
                      {it.scoring_tiers.map((t, idx) => (
                        <li key={idx}>
                          <span className="font-medium">{t.label}（{t.score_min && t.score_min !== t.score_max ? `${t.score_min}–${t.score_max ?? 0}分` : `${t.score_max ?? 0}分`}）</span>：{t.criteria}
                        </li>
                      ))}
                    </ul>
                  </details>
                )}
                {it.linked_requirement_ids && it.linked_requirement_ids.length > 0 && (
                  <p className="mt-0.5 text-xs text-muted">
                    关联采购需求：{it.linked_requirement_ids.map((id) => codeById.get(id) ?? `#${id}`).join('、')}
                  </p>
                )}
                {it.description && it.description !== it.title && <details><summary className="cursor-pointer text-xs text-muted">原文摘录</summary>
                  <p className="text-xs whitespace-pre-wrap text-slate-600">{it.description}</p></details>}
              </td>
              <td className="pr-2 text-xs"><SourceLink taskId={taskId} it={it} /></td>
              <td className="pr-2 text-xs">
                {it.evidence_ids.length > 0
                  ? <button type="button" className="text-link hover:underline disabled:text-muted" disabled={!it.evidence_run_id}
                    onClick={() => it.evidence_run_id && setEvidence({ run: it.evidence_run_id, id: it.evidence_ids[0] })}>{it.evidence_ids.length} 条证据</button>
                  : !it.evidence_gap && <span className="text-warn">未检索</span>}
                {it.evidence_gap && <p className="whitespace-pre-wrap text-warn">缺口：{it.evidence_gap}</p>}
              </td>
              <td className="pr-2 text-xs">{it.mapped_section_codes.length
                ? <a {...linkProps(taskUrl(taskId, 'outline', { query: { requirement: it.id } }))} className="text-link hover:underline">{it.mapped_section_codes.join('、')}</a>
                : it.superseded_by_id || it.review_status === 'stale' ? <span className="text-muted">已覆盖不计入</span>
                : it.out_of_bid ? <span className="text-muted">不投分包</span>
                : it.category === 'veto_item' ? <span className="text-muted">否决项核对</span>
                : it.category === 'scoring_item' && it.score_group === 'price' ? <span className="text-muted">报价文件响应</span>
                : <a {...linkProps(taskUrl(taskId, 'outline', { query: { requirement: it.id } }))} className="text-warn hover:underline">未映射</a>}</td>
              <td className="pr-2"><Badge dot tone={it.review_status === 'confirmed' ? 'green' : it.review_status === 'needs_revision' ? 'red' : it.review_status === 'stale' ? 'slate' : 'amber'}>{reviewStatusLabel[it.review_status] ?? it.review_status}</Badge></td>
              <td className="whitespace-nowrap">
                {it.review_status === 'needs_revision' && <IconButton label={`已核对原文变更：${it.code}`}
                  onClick={() => run(`r${it.id}`, () => api.updateRequirement(taskId, it.id, { review_status: 'confirmed' }), '已标记复核')}><CheckCircle2 className="size-4" /></IconButton>}
                <IconButton label={`编辑要求 ${it.code}`} onClick={() => setEditing(it)}><Pencil className="size-4" /></IconButton>
                <IconButton label={`删除要求 ${it.code}`} onClick={async () => {
                  if (await confirm({ title: `删除要求 ${it.code}？`, message: '清单将变更（epoch 递增），已批准或待审批的大纲会失效，需要重新确认与审批。', confirmLabel: '删除', danger: true }))
                    void run(`d${it.id}`, () => api.deleteRequirement(taskId, it.id), '已删除')
                }}><Trash2 className="size-4 text-danger" /></IconButton>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )

  return (
    <div className="mx-auto max-w-7xl space-y-4">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4 xl:grid-cols-7">
        <Stat label="要求总数" value={s.total} hint={`★ 强制 ${s.mandatory}`} />
        <Stat label="已复核确认" value={`${s.confirmed}/${s.total}`} tone={s.total > 0 && s.confirmed === s.total ? 'green' : 'amber'} />
        <Stat label="原文变更待复核" value={s.needs_revision} tone={s.needs_revision ? 'red' : undefined} />
        <Stat label="已绑定证据" value={s.with_evidence} hint={`已记录缺口 ${s.evidence_gaps}`} />
        <Stat label="尚未检索 / 复核证据" value={s.no_evidence} tone={s.no_evidence ? 'amber' : undefined} />
        <Stat label="技术标大纲映射" value={denominator > 0 ? `${s.mapped}/${denominator}` : '—'}
          hint={`${s.outline_version ? `依据大纲 v${s.outline_version}` : '尚无大纲'}${s.out_of_scope ? ` · 价格分 ${s.out_of_scope} 项不计入` : ''}${s.out_of_bid ? ` · 不投分包 ${s.out_of_bid} 项不计入` : ''}${s.veto.total ? ` · 否决项 ${s.veto.total} 项不计入` : ''}`}
          tone={denominator <= 0 ? undefined : s.unmapped.length ? 'amber' : 'green'} />
        <Stat label="否决项核对" value={s.veto.total ? `${s.veto.passed}/${s.veto.total}` : '—'} hint={s.veto.failed ? `不通过 ${s.veto.failed} 项` : `待核对 ${s.veto.pending} 项`}
          tone={s.veto.failed ? 'red' : s.veto.pending ? 'amber' : s.veto.total ? 'green' : undefined} />
      </div>
      {locate}
      {extracted && <ExtractSummary taskId={taskId} resp={extracted} onClose={() => setExtracted(undefined)} />}
      <BidScopeCard taskID={taskId} view={v} onChanged={() => invalidate('bid_scope_changed')} />
      <VetoChecklist taskID={taskId} items={v.items} onChanged={() => invalidate('veto_checked')} highlightId={filter === 'veto' ? target : undefined} />
      {v.scoring.length > 0 && filter !== 'veto' && <ScoringSummaryCard scoring={v.scoring} />}

      <Card title={<>要求清单 <Badge dot tone={v.manifest.status === 'confirmed' ? 'green' : v.manifest.status === 'stale' ? 'red' : 'amber'}>
        {manifestStatusLabel[v.manifest.status]} · epoch {v.manifest.epoch}</Badge>
        {sourcePending && <Badge tone="amber">原文已变更 · 待重新提取</Badge>}</>}
        subtitle={v.manifest.confirmed_at ? `由 ${v.manifest.approved_by || '—'} 于 ${fmtTime(v.manifest.confirmed_at)} 确认` : '提取与人工复核后，确认清单完整才能审批大纲'}
        actions={<>
          <Button size="sm" icon={<FileSearch className="size-3.5" />} loading={busy === 'extract'}
            onClick={() => run('extract', async () => { const r = await api.extractRequirements(taskId, false); setExtracted(r) }, '提取与目标分解完成，请复核结果')}>从招标文件提取</Button>
          {s.outline_version === 0 && (
            <Button size="sm" loading={busy === 'bootstrap'}
              onClick={() => run('bootstrap', async () => { const r = await api.extractRequirements(taskId, true); setExtracted(r) }, '已拆解目标并生成大纲草案')}>提取并生成大纲草案</Button>
          )}
          <Button size="sm" icon={<Plus className="size-3.5" />} onClick={() => setEditing('new')}>新增要求</Button>
          <Button size="sm" variant="primary" icon={<ShieldCheck className="size-3.5" />} loading={busy === 'confirm'}
            disabled={s.total === 0 || s.needs_revision > 0 || v.manifest.status === 'confirmed' || sourcePending}
            title={sourcePending ? '招标原文已变更，需先重新提取' : s.needs_revision > 0 ? '仍有原文变更待复核的要求' : v.manifest.status === 'confirmed' ? '清单已确认' : ''}
            onClick={() => run('confirm', () => api.confirmRequirements(taskId, v.manifest.epoch), '要求清单已确认完整', 'manifest_confirmed')}>确认清单完整</Button>
          <IconButton label="刷新要求清单" onClick={() => void data.reload()}><RefreshCw className="size-4" /></IconButton>
        </>}>
        <div className="space-y-3">
          <ErrorBanner error={data.error} />
          {v.manifest.status === 'stale' && <Notice>清单在确认后发生了实质变更（或招标原文变更），已批准 / 待审批大纲已失效，请复核后重新确认。</Notice>}
          {sourcePending && <Notice>招标原文已变更（source epoch {v.manifest.source_epoch}，已提取 {v.manifest.extracted_source_epoch}）。请先“从招标文件提取”，复核新增/变化的要求后再确认；导出会持续阻断。</Notice>}
          <div className="flex flex-wrap items-center gap-1.5" role="group" aria-label="筛选">
            {REQUIREMENT_FILTERS.map((k) => {
              const n = k === 'all' ? s.total : v.items.filter((i) => matches(i, k)).length
              return (
                <button key={k} type="button" aria-pressed={filter === k} onClick={() => setQuery({ filter: k === 'all' ? undefined : k })}
                  className={cx('h-7 rounded-lg border px-2 text-xs', filter === k ? 'border-fg bg-fg text-white' : 'border-line hover:bg-soft')}>
                  {filterLabel[k]} {n}
                </button>
              )
            })}
            <span className="mx-1 h-4 w-px bg-line" aria-hidden />
            <Select aria-label="按类别" className="h-7 w-40 text-xs" value={category} onChange={(e) => setCategory(e.target.value as RequirementCategory | '')}>
              <option value="">全部类别</option>
              {categories.map((c) => <option key={c} value={c}>{requirementCategoryLabel[c]} {s.by_category[c] ?? 0}</option>)}
            </Select>
          </div>
          {items.length === 0 ? <Empty>{s.total === 0 ? '暂无要求。请先导入招标文件并点击「从招标文件提取」，或人工新增。' : '当前筛选下没有要求。'}</Empty> : rowTable}
          <p className="text-xs text-muted">完整清单来自逐分块遍历与人工复核，不以检索 Top-K 代替；查无证据的要求请记录缺口，不要删除。
            修改编码、类别、标题、描述、强制性、分值、评分分组或原文来源会使清单 epoch 递增并使大纲失效；仅绑定证据不会。
            后发更正公告、澄清或答疑修改同一要求时，旧要求自动标记为「已被更正/澄清覆盖」并指向新要求，同时触发大纲、关联已定稿章节与响应矩阵失敏重检。</p>
        </div>
      </Card>
      <RequirementEditor taskId={taskId} item={editing} onClose={() => setEditing(undefined)} onSaved={() => { setEditing(undefined); invalidate('requirements_changed') }} />
      <EvidenceDetailDrawer runId={evidence?.run} evidenceId={evidence?.id} onClose={() => setEvidence(undefined)} onChanged={() => invalidate('evidence_verified')} />
    </div>
  )
}
