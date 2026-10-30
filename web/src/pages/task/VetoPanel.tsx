import { useState } from 'react'
import { CalendarClock, CheckCircle2, CircleDashed, Layers, XCircle } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { RequirementItem, RequirementsView, VetoStatus } from '../../api/types'
import { Badge, Button, Card, Empty, Field, Input, Modal, TextArea, cx, useToast } from '../../components/ui'
import type { Tone } from '../../components/ui'
import { docRoleLabel, fmtTime, vetoKindLabel, vetoStatusLabel } from '../../lib/format'

// D1：投标范围（截止时间、投标分包）与否决项核对（server/proposal.go 的 bid-scope / veto 路由）。

function vetoTone(s: VetoStatus): Tone {
  return s === 'passed' ? 'green' : s === 'failed' ? 'red' : 'amber'
}

// toLocalInput 把 RFC 3339 时间转成「2006-01-02 15:04」（北京时间），供人工修改。
function toLocalInput(iso?: string): string {
  if (!iso) return ''
  const d = new Date(new Date(iso).getTime() + 8 * 3600 * 1000)
  return d.toISOString().slice(0, 16).replace('T', ' ')
}

export function BidScopeCard({ taskID, view, onChanged }: { taskID: number; view: RequirementsView; onChanged: () => void }) {
  const toast = useToast()
  const m = view.manifest
  const lots = view.stats.lots
  const [deadline, setDeadline] = useState<string>()
  const [busy, setBusy] = useState(false)
  const src = m.bid_deadline_source ?? {}
  const save = async (req: Parameters<typeof api.setBidScope>[1], ok: string) => {
    setBusy(true)
    try { await api.setBidScope(taskID, req); toast('ok', ok); setDeadline(undefined); onChanged() } catch (e) { toast('error', errorText(e)) } finally { setBusy(false) }
  }
  const toggleLot = (lot: string) => {
    const current = m.bid_lots.length ? m.bid_lots : lots
    const next = current.includes(lot) ? current.filter((l) => l !== lot) : [...current, lot]
    if (next.length === 0) { toast('error', '至少保留一个投标分包；全部都投时请点击「全部分包」'); return }
    void save({ bid_lots: next.length === lots.length ? [] : next }, '投标分包已更新，清单与大纲需重新确认')
  }
  return (
    <Card title={<span className="flex items-center gap-1.5"><CalendarClock className="size-4" />投标范围</span>}>
      <div className="grid gap-4 md:grid-cols-2">
        <div className="text-sm">
          <div className="text-xs text-slate-500">投标截止时间（北京时间）</div>
          {deadline === undefined ? (
            <div className="mt-1 flex flex-wrap items-center gap-2">
              <span className={cx('font-medium', !m.bid_deadline && 'text-amber-700')}>{m.bid_deadline ? toLocalInput(m.bid_deadline) : '未识别'}</span>
              {src.origin === 'manual' && <Badge tone="purple">人工指定 · {String(src.by ?? '')}</Badge>}
              {src.origin === 'extracted' && <Badge tone="blue">原文{src.doc_role ? ` · ${docRoleLabel[String(src.doc_role)] ?? src.doc_role}` : ''}{src.page ? ` · 第 ${src.page} 页` : ''}</Badge>}
              <Button size="sm" variant="ghost" onClick={() => setDeadline(toLocalInput(m.bid_deadline))}>修改</Button>
              {src.origin === 'manual' && <Button size="sm" variant="ghost" loading={busy} onClick={() => void save({ bid_deadline: '' }, '已恢复为原文抽取，下次提取时重新识别')}>恢复原文</Button>}
            </div>
          ) : (
            <div className="mt-1 flex flex-wrap items-center gap-2">
              <Input className="w-44" value={deadline} placeholder="2026-08-26 10:00" onChange={(e) => setDeadline(e.target.value)} />
              <Button size="sm" loading={busy} disabled={!deadline.trim()} onClick={() => void save({ bid_deadline: deadline.trim() }, '投标截止时间已人工指定')}>保存</Button>
              <Button size="sm" variant="ghost" onClick={() => setDeadline(undefined)}>取消</Button>
            </div>
          )}
          {src.origin === 'extracted' && src.text && <p className="mt-1 text-xs text-slate-400">原文：{String(src.text)}</p>}
          <p className="mt-1 text-xs text-slate-400">有更正 / 澄清公告时以后发布的为准；资质、证书有效期按此时间判断。</p>
        </div>
        <div className="text-sm">
          <div className="flex items-center gap-1.5 text-xs text-slate-500"><Layers className="size-3.5" />本次投标分包</div>
          {lots.length === 0 ? <p className="mt-1 text-slate-500">要求清单中没有分包。</p> : (
            <div className="mt-1 flex flex-wrap items-center gap-1.5">
              {lots.map((lot) => {
                const on = m.bid_lots.length === 0 || m.bid_lots.includes(lot)
                return (
                  <button key={lot} disabled={busy} onClick={() => toggleLot(lot)}
                    className={cx('rounded-full border px-2.5 py-0.5 text-xs', on ? 'border-blue-600 bg-blue-50 text-blue-700' : 'border-slate-200 text-slate-400 line-through')}>
                    包{lot}
                  </button>
                )
              })}
              {m.bid_lots.length > 0 && <Button size="sm" variant="ghost" loading={busy} onClick={() => void save({ bid_lots: [] }, '已恢复为全部分包')}>全部分包</Button>}
            </div>
          )}
          {view.stats.out_of_bid > 0 && <p className="mt-1 text-xs text-slate-500">{view.stats.out_of_bid} 条要求属于不投的分包，不进入大纲、覆盖率与否决项门禁。</p>}
          <p className="mt-1 text-xs text-slate-400">改变投标分包会使清单 epoch 递增、已批准大纲失效。</p>
        </div>
      </div>
    </Card>
  )
}

export function VetoChecklist({ taskID, items, onChanged }: { taskID: number; items: RequirementItem[]; onChanged: () => void }) {
  const [checking, setChecking] = useState<{ item: RequirementItem; status: VetoStatus }>()
  const vetoes = items.filter((i) => i.category === 'veto_item' && !i.out_of_bid)
  const failed = vetoes.filter((i) => i.veto_status === 'failed').length
  const pending = vetoes.filter((i) => i.veto_status === 'pending').length
  return (
    <Card title={<>否决项核对 <Badge tone={failed ? 'red' : pending ? 'amber' : vetoes.length ? 'green' : 'slate'}>
      通过 {vetoes.length - failed - pending}/{vetoes.length}{failed ? ` · 不通过 ${failed}` : ''}</Badge></>}>
      {vetoes.length === 0 ? <Empty>尚无否决项。上传招标文件后点击「从招标文件提取」：资格性 / 符合性审查表逐行生成否决项，另有 87 号令法定无效情形 6 项。</Empty> : (
        <div className="divide-y divide-slate-100">
          {vetoes.map((it) => (
            <div key={it.id} className="flex flex-wrap items-start gap-3 py-2 text-sm">
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-1.5">
                  <Badge tone={vetoTone(it.veto_status)}>{vetoStatusLabel[it.veto_status]}</Badge>
                  <Badge>{vetoKindLabel[it.veto_kind] ?? it.veto_kind}</Badge>
                  <span className="font-medium">{it.title}</span>
                  {it.lot && <span className="text-xs text-slate-400">包{it.lot}</span>}
                </div>
                <details><summary className="cursor-pointer text-xs text-slate-400">审查内容</summary>
                  <p className="whitespace-pre-wrap text-xs text-slate-600">{it.description}</p>
                  {it.source_loc.covered_by && it.source_loc.covered_by.length > 0 &&
                    <p className="text-xs text-slate-400">对应审查表：{it.source_loc.covered_by.join('、')}</p>}</details>
                {it.veto_note && <p className="text-xs text-slate-600">依据：{it.veto_note}
                  <span className="text-slate-400"> · {it.veto_checked_by} {it.veto_checked_at ? fmtTime(it.veto_checked_at) : ''}</span></p>}
              </div>
              <div className="flex shrink-0 gap-1">
                <Button size="sm" variant="ghost" title="核对通过" onClick={() => setChecking({ item: it, status: 'passed' })}><CheckCircle2 className="size-4 text-green-600" /></Button>
                <Button size="sm" variant="ghost" title="核对不通过" onClick={() => setChecking({ item: it, status: 'failed' })}><XCircle className="size-4 text-red-600" /></Button>
                {it.veto_status !== 'pending' && <Button size="sm" variant="ghost" title="退回待核对" onClick={() => setChecking({ item: it, status: 'pending' })}><CircleDashed className="size-4 text-slate-400" /></Button>}
              </div>
            </div>
          ))}
        </div>
      )}
      <p className="mt-2 text-xs text-slate-400">任一否决项待核对或不通过都会阻断导出。通过须写明证明材料（文件与页码），不通过须写明不满足的条件；否决项内容变化后结论自动退回待核对。
        审查表由规则解析，未经人工标注评测，请对照原文核对是否漏项。</p>
      {checking && <VetoDialog taskID={taskID} item={checking.item} status={checking.status} onClose={() => setChecking(undefined)}
        onSaved={() => { setChecking(undefined); onChanged() }} />}
    </Card>
  )
}

function VetoDialog({ taskID, item, status, onClose, onSaved }: { taskID: number; item: RequirementItem; status: VetoStatus; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const [note, setNote] = useState(status === item.veto_status ? item.veto_note : '')
  const [busy, setBusy] = useState(false)
  const needNote = status !== 'pending'
  const submit = async () => {
    setBusy(true)
    try { await api.setVetoStatus(taskID, item.id, status, note); toast('ok', `已标记为「${vetoStatusLabel[status]}」`); onSaved() } catch (e) { toast('error', errorText(e)) } finally { setBusy(false) }
  }
  return (
    <Modal open title={`${vetoStatusLabel[status]}：${item.title}`} onClose={onClose}
      footer={<><Button variant="ghost" onClick={onClose}>取消</Button>
        <Button variant={status === 'failed' ? 'danger' : 'primary'} loading={busy} disabled={needNote && !note.trim()} onClick={() => void submit()}>确定</Button></>}>
      <p className="whitespace-pre-wrap text-xs text-slate-600">{item.description}</p>
      {needNote && <Field label={status === 'passed' ? '证明材料（文件名与页码，或核对方式）' : '不满足的条件'}>
        <TextArea rows={3} value={note} onChange={(e) => setNote(e.target.value)}
          placeholder={status === 'passed' ? '例：营业执照副本，投标文件第 12 页' : '例：社保缴纳证明只有 3 个月，招标要求近 6 个月'} />
      </Field>}
    </Modal>
  )
}
