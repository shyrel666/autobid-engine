import { useMemo, useState } from 'react'
import { ArrowDown, ArrowUp, Pencil, Plus, RefreshCw, Send, ShieldCheck, Sparkles, XCircle } from 'lucide-react'
import { api, errorText } from '../../../api/client'
import type { OutlineDetail, RequirementsView, SectionInput } from '../../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, IconButton, Input, Modal, Notice, Spinner, Stat, cx, useToast } from '../../../components/ui'
import type { Tone } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'
import { useScrollTo } from '../../../hooks/useScrollTo'
import { fmtTime, manifestStatusLabel, outlineStatusLabel, sectionStatusLabel } from '../../../lib/format'
import { linkProps, useSearchParams } from '../../../lib/router'
import { optionalId, taskUrl } from '../../../lib/routes'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'

// 大纲与写作 · 大纲及审批：版本选择、章节树、要求映射、依赖、提交与批准（原「② 技术标大纲与审批」）。
// 当前批准版本与正在修订的草案分别标注，最新版本不会被自动当作已批准版本。

export function outlineTone(s: string): Tone {
  return s === 'approved' ? 'green' : s === 'pending_approval' ? 'amber' : s === 'superseded' ? 'slate' : 'blue'
}

export function sectionTone(s: string): Tone {
  switch (s) {
    case 'approved': return 'green'
    case 'ready': case 'draft': return 'blue'
    case 'writing': case 'reviewing': return 'purple'
    case 'pending': return 'amber'
    case 'stale_outline': case 'stale_evidence': case 'rejected': return 'red'
    default: return 'slate'
  }
}

const parseCodes = (s: string) => s.split(/[,，\s]+/).map((x) => x.trim()).filter(Boolean)

interface Row { code: string; title: string; level: string; deps: string; reqs: string; guide: string; words: string }

function OutlineEditor({ taskId, detail, requirements, onClose, onSaved }:
  { taskId: number; detail: OutlineDetail; requirements?: RequirementsView; onClose: () => void; onSaved: (d: OutlineDetail) => void }) {
  const toast = useToast()
  const codeByID = useMemo(() => new Map((requirements?.items ?? []).map((r) => [r.id, r.code])), [requirements])
  const idByCode = useMemo(() => new Map((requirements?.items ?? []).map((r) => [r.code, r.id])), [requirements])
  const initial = useMemo(() => detail.sections.map((s) => ({
    code: s.section_code, title: s.title, level: String(s.level), deps: s.depends_on.join(', '),
    reqs: s.requirement_item_ids.map((id) => codeByID.get(id) ?? `#${id}`).join(', '), guide: s.writing_guide, words: String(s.target_words),
  })), [detail, codeByID])
  const [rows, setRows] = useState<Row[]>(initial)
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const derives = detail.outline.status === 'approved' || detail.outline.status === 'superseded'
  const mapped = new Set(rows.flatMap((r) => parseCodes(r.reqs)))
  // 技术标范围内的要求（排除否决项、价格分、不投分包）才需要映射。
  const inScope = (requirements?.items ?? []).filter((r) => r.category !== 'veto_item' && !r.out_of_bid && !(r.category === 'scoring_item' && r.score_group === 'price'))
  const unmapped = inScope.filter((r) => !mapped.has(r.code))
  const upd = (i: number, k: keyof Row, v: string) => setRows(rows.map((r, j) => (j === i ? { ...r, [k]: v } : r)))
  const move = (i: number, d: number) => {
    const j = i + d
    if (j < 0 || j >= rows.length) return
    const next = [...rows];
    [next[i], next[j]] = [next[j], next[i]]
    setRows(next)
  }
  const save = async () => {
    setError('')
    const sections: SectionInput[] = []
    for (const r of rows) {
      const ids: number[] = []
      for (const c of parseCodes(r.reqs)) {
        const id = c.startsWith('#') ? Number(c.slice(1)) : idByCode.get(c)
        if (!id) { setError(`章节 ${r.code}：未知要求编码 ${c}`); return }
        ids.push(id)
      }
      sections.push({ section_code: r.code.trim(), title: r.title.trim(), level: Number(r.level) || undefined, depends_on: parseCodes(r.deps),
        requirement_item_ids: ids, writing_guide: r.guide, target_words: Number(r.words) || 0 })
    }
    setBusy(true)
    try {
      const d = await api.updateOutline(taskId, detail.outline.version, { note, sections })
      toast('ok', d.outline.version !== detail.outline.version ? `已派生新版本 v${d.outline.version}，v${detail.outline.version} 的批准已失效` : '目录已保存')
      onSaved(d)
    } catch (e) { setError(errorText(e)) } finally { setBusy(false) }
  }
  return (
    <Modal open wide title={`编辑大纲目录 · v${detail.outline.version}`} onClose={onClose} dirty={!!note || JSON.stringify(rows) !== JSON.stringify(initial)}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={save}>{derives ? `保存为 v${detail.outline.version + 1} 草案` : '保存'}</Button></>}>
      <div className="space-y-3">
        {derives && <Notice>保存将派生 v{detail.outline.version + 1} 草案{detail.outline.status === 'approved' ? `；当前批准的 v${detail.outline.version} 立即失效：在途 AI 写回只能归档为候选稿，新版本需重新提交审批。` : '。'}</Notice>}
        {unmapped.length > 0 && <Notice tone="danger">未映射（{unmapped.length}）：{unmapped.map((r) => r.code).join('、')}</Notice>}
        <Field label="修订说明"><Input value={note} onChange={(e) => setNote(e.target.value)} /></Field>
        <div className="space-y-2">
          {rows.map((r, i) => (
            <div key={i} className="grid grid-cols-12 gap-1.5 rounded-[10px] border border-line p-2">
              <Input aria-label="章节编码" className="col-span-2 font-mono text-xs md:col-span-1" value={r.code} placeholder="编码" onChange={(e) => upd(i, 'code', e.target.value)} />
              <Input aria-label="章节标题" className="col-span-10 md:col-span-4" value={r.title} placeholder="标题" onChange={(e) => upd(i, 'title', e.target.value)} />
              <Input aria-label="层级" className="col-span-2 md:col-span-1" value={r.level} placeholder="层级" onChange={(e) => upd(i, 'level', e.target.value)} />
              <Input aria-label="前置章节编码" className="col-span-5 text-xs md:col-span-2" value={r.deps} placeholder="前置章节编码" onChange={(e) => upd(i, 'deps', e.target.value)} />
              <Input aria-label="映射要求编码" className="col-span-5 text-xs md:col-span-3" value={r.reqs} placeholder="映射要求编码" onChange={(e) => upd(i, 'reqs', e.target.value)} />
              <div className="col-span-12 flex items-center justify-end md:col-span-1">
                <IconButton label="上移" onClick={() => move(i, -1)}><ArrowUp className="size-3.5" /></IconButton>
                <IconButton label="下移" onClick={() => move(i, 1)}><ArrowDown className="size-3.5" /></IconButton>
                <IconButton label={`删除章节 ${r.code}`} onClick={() => setRows(rows.filter((_, j) => j !== i))}><XCircle className="size-3.5 text-danger" /></IconButton>
              </div>
              <Input aria-label="写作要点" className="col-span-9 text-xs md:col-span-10" value={r.guide} placeholder="写作要点" onChange={(e) => upd(i, 'guide', e.target.value)} />
              <Input aria-label="目标字数" className="col-span-3 text-xs md:col-span-2" value={r.words} placeholder="目标字数" onChange={(e) => upd(i, 'words', e.target.value)} />
            </div>
          ))}
        </div>
        <Button size="sm" icon={<Plus className="size-3.5" />}
          onClick={() => setRows([...rows, { code: String(rows.length + 1), title: '', level: '1', deps: '', reqs: '', guide: '', words: '0' }])}>添加章节</Button>
        <p className="text-xs text-muted">前置依赖填同一大纲内的章节编码（逗号分隔），由后端校验无环；依赖章节未产出有效稿前，本章节不能派发写作。前端不会自动消除依赖。</p>
        <ErrorBanner error={error} />
      </div>
    </Modal>
  )
}

export default function OutlinePage() {
  const toast = useToast()
  const { taskId, outlines: list, invalidate } = useTaskWorkspace()
  const tick = useTick('outline', 'sections', 'requirements')
  const [params, setQuery] = useSearchParams()
  const vParam = optionalId(params, 'version')
  const target = optionalId(params, 'requirement')
  const l = list.data
  const current = vParam && !Number.isNaN(vParam) ? vParam : (l?.approved_version || l?.open_version || l?.outlines[0]?.version)
  const det = useAsync(() => (current ? api.getOutline(taskId, current) : Promise.resolve(undefined)), [taskId, current, tick])
  const reqs = useAsync(() => api.requirements(taskId), [taskId, tick])
  const [editing, setEditing] = useState(false)
  const [busy, setBusy] = useState('')
  useScrollTo(target && !Number.isNaN(target) ? `map-${target}` : undefined, !!det.data)

  const act = async (key: string, fn: () => Promise<OutlineDetail>, ok: string) => {
    setBusy(key)
    try { const d = await fn(); toast('ok', ok); setQuery({ version: d.outline.version }); invalidate('outline_changed') }
    catch (e) { toast('error', errorText(e)); invalidate(['outline']) } finally { setBusy('') }
  }
  if (list.loading && !l) return <Spinner />
  if (!l) return <ErrorBanner error={list.error} onRetry={list.reload} />
  const d = det.data
  const reqByID = new Map((reqs.data?.items ?? []).map((r) => [r.id, r]))
  const targetReq = target && !Number.isNaN(target) ? reqByID.get(target) : undefined

  return (
    <div className="mx-auto max-w-7xl space-y-4">
      <Card title="大纲版本" subtitle={l.approved_version ? `当前批准版本 v${l.approved_version}${l.open_version ? `；v${l.open_version} 正在修订` : ''}` : '尚无批准版本'}
        actions={<>
          <Button size="sm" variant="primary" icon={<Sparkles className="size-3.5" />} disabled={!!l.open_version} loading={busy === 'create'}
            title={l.open_version ? `已有在编大纲 v${l.open_version}，请直接编辑它` : ''}
            onClick={() => act('create', () => api.createOutline(taskId, l.approved_version ? { from_version: l.approved_version, note: `基于 v${l.approved_version} 修订` } : { note: '按要求清单生成' }),
              '已生成大纲草案')}>{l.approved_version ? '基于批准版本新建草案' : '按要求清单生成大纲'}</Button>
          <IconButton label="刷新大纲" onClick={() => { void list.reload(); void det.reload() }}><RefreshCw className="size-4" /></IconButton>
        </>}>
        <ErrorBanner error={list.error} />
        {l.outlines.length === 0 ? <Empty>尚无大纲。确认要求清单后可「按要求清单生成大纲」：默认目录覆盖全部技术标要求，评分项逐项成章，其余章节以「总体技术方案」为前置依赖。</Empty> : (
          <div className="flex flex-wrap gap-2" role="group" aria-label="选择大纲版本">
            {l.outlines.map((o) => (
              <button key={o.id} type="button" aria-pressed={current === o.version} onClick={() => setQuery({ version: o.version }, { replace: false })}
                className={cx('rounded-[10px] border px-3 py-1.5 text-left text-sm', current === o.version ? 'border-fg bg-soft' : 'border-line hover:bg-soft')}>
                <div className="flex items-center gap-1.5 font-medium">v{o.version} <Badge dot tone={outlineTone(o.status)}>{outlineStatusLabel[o.status]}</Badge>
                  {o.version === l.approved_version && <span className="text-xs text-ok">当前批准</span>}
                  {o.version === l.open_version && <span className="text-xs text-link">正在修订</span>}</div>
                <div className="text-xs text-muted">{o.status === 'approved' ? `${o.approved_by || '—'} 批准于 ${fmtTime(o.approved_at)}` : `更新于 ${fmtTime(o.updated_at)}`}</div>
              </button>
            ))}
          </div>
        )}
      </Card>

      {vParam !== undefined && (Number.isNaN(vParam) || !l.outlines.some((o) => o.version === vParam)) && <Notice tone="danger">本项目没有大纲版本 {String(params.get('version'))}。</Notice>}
      {target !== undefined && !targetReq && reqs.data && <Notice tone="danger">本项目中未找到要求 #{String(params.get('requirement'))}。</Notice>}
      {targetReq && d && <Notice tone="info">定位要求 {targetReq.code} {targetReq.title}：{d.sections.some((s) => s.requirement_item_ids.includes(targetReq.id))
        ? `已映射到 ${d.sections.filter((s) => s.requirement_item_ids.includes(targetReq.id)).map((s) => s.section_code).join('、')}` : '在本版本中尚未映射，请编辑目录补齐'}。</Notice>}
      {det.error && <ErrorBanner error={det.error} onRetry={det.reload} />}
      {current && !d && det.loading && <Spinner />}
      {d && (
        <Card title={<>大纲 v{d.outline.version} <Badge dot tone={outlineTone(d.outline.status)}>{outlineStatusLabel[d.outline.status]}</Badge>
          {d.outline.derived_from_version && <span className="text-xs font-normal text-muted">派生自 v{d.outline.derived_from_version}</span>}</>}
          subtitle={d.outline.note || undefined}
          actions={<>
            <Button size="sm" icon={<Pencil className="size-3.5" />} onClick={() => setEditing(true)}
              disabled={(d.outline.status === 'approved' || d.outline.status === 'superseded') && !!l.open_version}
              title={l.open_version && l.open_version !== d.outline.version ? `已有在编草案 v${l.open_version}，请切换到该版本编辑` : ''}>
              {d.outline.status === 'approved' || d.outline.status === 'superseded' ? '调整目录（派生新版本）' : '编辑目录'}</Button>
            {d.outline.status === 'draft' && <Button size="sm" variant="primary" icon={<Send className="size-3.5" />} loading={busy === 'submit'}
              disabled={d.coverage.unmapped.length > 0} title={d.coverage.unmapped.length ? '仍有要求未映射' : ''}
              onClick={() => act('submit', () => api.submitOutline(taskId, d.outline.version), '已提交审批')}>提交审批</Button>}
            {d.outline.status === 'pending_approval' && <Button size="sm" variant="primary" icon={<ShieldCheck className="size-3.5" />} loading={busy === 'approve'}
              onClick={() => act('approve', () => api.approveOutline(taskId, d.outline.version), '大纲已批准，可派发章节写作')}>批准大纲</Button>}
          </>}>
          <div className="space-y-3">
            <div className="grid gap-3 md:grid-cols-4">
              <Stat label="要求映射覆盖" value={d.coverage.total ? `${d.coverage.mapped}/${d.coverage.total}` : '—'} tone={!d.coverage.total ? undefined : d.coverage.unmapped.length ? 'red' : 'green'}
                hint={d.coverage.out_of_scope ? `价格分 ${d.coverage.out_of_scope} 项由报价文件响应，不计入` : undefined} />
              <Stat label="章节数" value={d.sections.length} />
              <Stat label="清单状态" value={<span className="text-base">{manifestStatusLabel[d.manifest.status]}</span>}
                hint={`清单 epoch ${d.manifest.epoch} · 本版本基于 ${d.outline.requirements_epoch}`} tone={d.manifest.status === 'confirmed' && d.requirements_epoch_current ? 'green' : 'amber'} />
              <Stat label="当前批准版本" value={d.approved_version ? `v${d.approved_version}` : '无'} tone={d.approved_version ? 'green' : 'amber'} />
            </div>
            {d.coverage.unmapped.length > 0 && (
              <Notice tone="danger" action={<Button size="sm" onClick={() => setEditing(true)}>编辑目录补齐</Button>}>
                <div className="font-medium">{d.coverage.unmapped.length} 条要求未映射到任何章节，补齐前不能提交审批：</div>
                <ul className="mt-1 flex flex-wrap gap-x-3 gap-y-1 text-xs">{d.coverage.unmapped.map((r) => (
                  <li key={r.id} id={`map-${r.id}`} className={cx(target === r.id && 'rounded bg-panel px-1 ring-1 ring-fg')}>{r.is_mandatory ? '★' : ''}{r.code} {r.title}</li>))}</ul>
              </Notice>
            )}
            {!d.requirements_epoch_current && d.outline.status === 'draft' && <Notice>要求清单在本草案生成后已变更：保存一次目录即可同步到最新 epoch，并需重新确认清单。</Notice>}
            {d.outline.status === 'superseded' && <Notice tone="info">该版本已被取代，仅供追溯；其章节的在途写回只会归档为大纲过期候选稿。</Notice>}
            {d.outline.status === 'pending_approval' && <Notice>待审批期间章节写作的派发与写回均被后端拒绝；状态持久化在数据库中，服务重启后保持。</Notice>}
            <div className="overflow-x-auto">
              <table className="w-full min-w-[760px] text-sm">
                <thead><tr className="border-b border-line text-left text-xs text-muted">
                  <th className="py-2 pr-2 font-medium">章节</th><th className="pr-2 font-medium">映射要求</th><th className="pr-2 font-medium">前置依赖</th><th className="pr-2 font-medium">状态</th><th className="pr-2 font-medium">修订</th><th><span className="sr-only">操作</span></th>
                </tr></thead>
                <tbody>
                  {d.sections.map((s) => (
                    <tr key={s.id} className={cx('border-b border-line-soft align-top', targetReq && s.requirement_item_ids.includes(targetReq.id) && 'bg-soft')}>
                      <td className="py-2 pr-2" style={{ paddingLeft: (s.level - 1) * 16 }}>
                        <span className="font-mono text-xs text-muted">{s.section_code}</span> <span className="font-medium">{s.title}</span>
                        {s.writing_guide && <div className="text-xs text-muted">{s.writing_guide}</div>}
                      </td>
                      <td className="pr-2 text-xs">{s.requirement_item_ids.map((id) => {
                        const r = reqByID.get(id)
                        return <span key={id} id={target === id ? `map-${id}` : undefined} className={cx('mr-1 inline-block', target === id && 'rounded bg-panel px-1 ring-1 ring-fg')}>
                          {r ? <a {...linkProps(taskUrl(taskId, 'requirements', { query: { requirement: id } }))} className="hover:underline">{r.is_mandatory && '★'}{r.code}</a> : `#${id}`}</span>
                      })}</td>
                      <td className="pr-2 text-xs">{s.depends_on.length ? s.depends_on.map((c) => (
                        <span key={c} className={cx('mr-1', s.unmet_dependencies.includes(c) ? 'text-warn' : 'text-ok')} title={s.unmet_dependencies.includes(c) ? '尚未就绪' : '已就绪'}>
                          {c}{s.unmet_dependencies.includes(c) ? '（未就绪）' : ''}</span>)) : '—'}</td>
                      <td className="pr-2"><Badge dot tone={sectionTone(s.status)}>{sectionStatusLabel[s.status] ?? s.status}</Badge>{s.writing && <Badge tone="purple">撰写中</Badge>}</td>
                      <td className="pr-2 text-xs text-muted">修订 {s.current_rev_no}{s.approved_rev_no ? ` · 定稿修订 ${s.approved_rev_no}` : ''}</td>
                      <td><a {...linkProps(taskUrl(taskId, 'writing', { sectionId: s.id }))} className="text-sm text-link hover:underline">写作 →</a></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </Card>
      )}
      {d && editing && <OutlineEditor taskId={taskId} detail={d} requirements={reqs.data} onClose={() => setEditing(false)}
        onSaved={(nd) => { setEditing(false); setQuery({ version: nd.outline.version }); invalidate('outline_changed') }} />}
    </div>
  )
}
