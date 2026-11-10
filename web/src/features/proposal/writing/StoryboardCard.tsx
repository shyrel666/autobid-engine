import { useState } from 'react'
import type { ReactNode } from 'react'
import { api, errorText } from '../../../api/client'
import type { SectionDetail } from '../../../api/types'
import { Badge, Button, ErrorBanner, Field, Modal, Notice, TextArea } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'
import { linkProps } from '../../../lib/router'
import { taskUrl } from '../../../lib/routes'
import { admissibleOptions, commitmentOptions, editableStatuses, storyboardForm, storyboardRequest, storyboardStatusLabel, toggleId } from './storyboard'
import type { ReferenceOption, StoryboardForm } from './storyboard'

// 故事板：写作前先定核心论点、逐条要求的做法与收益，以及本章可以引用的证据和承诺。
// 流程是作者保存并提交，另一位评审人批准或退回；没有这一步，D3 门禁下的章节既不能写也不能派发 AI。

function storyboardTone(status: string) {
  return status === 'pink_approved' ? 'green' as const : status === 'pending_pink_review' ? 'purple' as const : status === 'stale' || status === 'pink_rejected' ? 'red' as const : 'amber' as const
}

function PickList({ title, empty, options, selected, onToggle }: { title: string; empty: ReactNode; options: ReferenceOption[]; selected: number[]; onToggle: (id: number, on: boolean) => void }) {
  return (
    <fieldset className="space-y-1">
      <legend className="text-xs font-medium text-muted">{title}</legend>
      {options.length === 0 ? <p className="text-xs text-muted">{empty}</p> : options.map((o) => (
        <label key={o.id} className="flex items-start gap-2 text-sm">
          <input type="checkbox" className="mt-1" checked={selected.includes(o.id)} onChange={(e) => onToggle(o.id, e.target.checked)} aria-label={`引用 ${o.label}`} />
          <span>{o.label}</span>
        </label>
      ))}
    </fieldset>
  )
}

export default function StoryboardCard({ taskId, sectionId, detail, gated, busy, act, onSaved }: {
  taskId: number; sectionId: number; detail: SectionDetail; gated: boolean; busy: string
  act: (key: string, what: string, fn: () => Promise<unknown>, ok: string, action: ['sections']) => Promise<void>
  onSaved: () => void
}) {
  const sb = detail.storyboard
  const [form, setForm] = useState<StoryboardForm>()
  const [reject, setReject] = useState<string>()
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const open = !!form
  const adm = useAsync(() => api.listAdmissibility(taskId), [taskId, open], open)
  const com = useAsync(() => api.listCommitments(taskId), [taskId, open], open)

  // 旧项目没有开启写作门禁时不主动显示入口：创建故事板会让整个项目进入门禁。
  if (!sb && !gated) return null
  const canEdit = !sb || editableStatuses.includes(sb.status)
  const update = (patch: Partial<StoryboardForm>) => setForm((f) => (f ? { ...f, ...patch } : f))
  const save = async () => {
    if (!form) return
    setSaving(true); setError('')
    try {
      await api.upsertSectionStoryboard(taskId, sectionId, storyboardRequest(form, sb?.evidence_ids ?? []))
      setForm(undefined); onSaved()
    } catch (e) { setError(errorText(e)) } finally { setSaving(false) }
  }

  return (
    <section className="space-y-1.5 rounded-[10px] border border-line bg-soft p-3 text-xs" aria-label="章节故事板">
      <div className="flex items-center justify-between gap-2">
        <span className="font-semibold">章节故事板{sb ? ` v${sb.version}` : ''}</span>
        {sb ? <Badge tone={storyboardTone(sb.status)}>{storyboardStatusLabel[sb.status] ?? sb.status}</Badge> : <Badge tone="amber">未编写</Badge>}
      </div>
      {!sb && <p className="text-muted">写作前要先定本章的核心论点和逐条做法，通过粉队评审后才能写作或派发 AI。</p>}
      {sb && <p className="font-medium text-fg">核心论点：{sb.theme_sentence}</p>}
      {sb?.stale_reason && <p className="text-danger">失效原因：{sb.stale_reason}</p>}
      {sb?.review_note && <p className="text-muted">评审意见：{sb.review_note}</p>}
      {sb && sb.scoring_points.length > 0 && (
        <ul className="list-disc space-y-1 pl-4 text-muted">
          {sb.scoring_points.map((sp, i) => <li key={i}><span className="font-medium text-fg">{sp.requirement_code || sp.scoring_point}</span>：{sp.our_approach} → {sp.buyer_benefit}</li>)}
        </ul>
      )}
      {sb && <p className="text-muted">作者 {sb.created_by || '未记录'}{sb.submitted_by ? ` · 提交人 ${sb.submitted_by}` : ''}{sb.approved_by ? ` · 批准人 ${sb.approved_by}` : ''}</p>}
      <div className="flex flex-wrap gap-1 pt-1">
        {canEdit && <Button size="sm" disabled={!detail.writable} title={!detail.writable ? detail.blocked_reason : ''} onClick={() => { setError(''); setForm(storyboardForm(detail.requirements, sb)) }}>{sb ? '修改故事板' : '编写故事板'}</Button>}
        {sb && (sb.status === 'draft' || sb.status === 'pink_rejected' || sb.status === 'stale') && (
          <Button size="sm" loading={busy === 'sb_submit'} onClick={() => void act('sb_submit', '提交粉队评审', () => api.submitSectionStoryboard(taskId, sectionId), '已提交粉队评审', ['sections'])}>提交粉队评审</Button>
        )}
        {sb?.status === 'pending_pink_review' && <>
          <Button size="sm" variant="primary" loading={busy === 'sb_approve'} onClick={() => void act('sb_approve', '批准故事板', () => api.approveSectionStoryboard(taskId, sectionId, '粉队评审通过'), '故事板已通过粉队评审', ['sections'])}>粉队批准</Button>
          <Button size="sm" onClick={() => setReject('')}>退回修改</Button>
        </>}
      </div>

      <Modal wide open={open} title="编写故事板" onClose={() => { if (!saving) setForm(undefined) }}
        footer={<><Button disabled={saving} onClick={() => setForm(undefined)}>取消</Button><Button variant="primary" loading={saving} onClick={() => void save()}>保存故事板</Button></>}>
        {form && <div className="space-y-3">
          <ErrorBanner error={error} />
          {sb?.status === 'pink_approved' && <Notice>这版故事板已通过评审。保存修改会生成新版本，需要重新评审。</Notice>}
          <Field label="核心论点" hint="一句话说明本章要让评委相信什么，至少 12 个字">
            <TextArea rows={2} value={form.theme} onChange={(e) => update({ theme: e.target.value })} />
          </Field>
          {form.points.map((p, i) => (
            <fieldset key={p.code || i} className="grid gap-2 rounded-lg border border-line p-2 md:grid-cols-2">
              <legend className="px-1 text-xs font-medium">{p.code} {p.title}</legend>
              <Field label={`我方做法 ${p.code}`}><TextArea rows={2} value={p.approach} onChange={(e) => update({ points: form.points.map((x, j) => j === i ? { ...x, approach: e.target.value } : x) })} /></Field>
              <Field label={`采购人收益 ${p.code}`}><TextArea rows={2} value={p.benefit} onChange={(e) => update({ points: form.points.map((x, j) => j === i ? { ...x, benefit: e.target.value } : x) })} /></Field>
            </fieldset>
          ))}
          <ErrorBanner error={adm.error || com.error} />
          <PickList title="本章可引用的企业证据（可采）" selected={form.itemIds} onToggle={(id, on) => update({ itemIds: toggleId(form.itemIds, id, on) })}
            options={admissibleOptions(adm.data ?? [])}
            empty={<>暂无可采证据。如需引用，先到 <a {...linkProps(taskUrl(taskId, 'evidence'))} className="underline">证据与承诺</a> 绑定并评估。</>} />
          <PickList title="本章可引用的承诺（已确认）" selected={form.commitmentIds} onToggle={(id, on) => update({ commitmentIds: toggleId(form.commitmentIds, id, on) })}
            options={commitmentOptions(com.data ?? [])}
            empty={<>暂无已确认的承诺。如需引用，先到 <a {...linkProps(taskUrl(taskId, 'evidence'))} className="underline">证据与承诺</a> 确认。</>} />
        </div>}
      </Modal>

      <Modal open={reject !== undefined} title="退回故事板" onClose={() => setReject(undefined)}
        footer={<><Button onClick={() => setReject(undefined)}>取消</Button><Button variant="primary" disabled={!reject?.trim()} loading={busy === 'sb_reject'}
          onClick={() => { const note = reject!.trim(); setReject(undefined); void act('sb_reject', '退回故事板', () => api.rejectSectionStoryboard(taskId, sectionId, note), '已退回故事板', ['sections']) }}>确认退回</Button></>}>
        <Field label="退回原因" hint="作者会看到这段说明"><TextArea rows={3} value={reject ?? ''} onChange={(e) => setReject(e.target.value)} /></Field>
      </Modal>
    </section>
  )
}

/** 正文编辑时插入受控引用；保存时后端把引用展开成标准表述，这样数值和证书号不靠人手抄。 */
export function ReferenceBar({ taskId, detail, onInsert }: { taskId: number; detail: SectionDetail; onInsert: (token: string) => void }) {
  const sb = detail.storyboard
  const adm = useAsync(() => api.listAdmissibility(taskId), [taskId])
  const com = useAsync(() => api.listCommitments(taskId), [taskId])
  if (!sb) return null
  const items = admissibleOptions(adm.data ?? []).filter((o) => sb.admissibility_item_ids.includes(o.id))
  const commits = commitmentOptions(com.data ?? []).filter((o) => sb.commitment_ids.includes(o.id))
  if (items.length + commits.length === 0) return null
  return (
    <div className="flex flex-wrap items-center gap-1 text-xs" role="group" aria-label="插入受控引用">
      <span className="text-muted">插入引用：</span>
      {[...items, ...commits].map((o) => <Button key={o.token} size="sm" onClick={() => onInsert(o.token)}>{o.label}</Button>)}
      <span className="w-full text-muted">引用单独成段时，保存后展开成带编号的标准表述；放在句子末尾时，只为这句话标注出处。</span>
    </div>
  )
}
