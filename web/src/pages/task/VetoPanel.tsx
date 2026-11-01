import { useState } from 'react'
import { CalendarClock, CheckCircle2, CircleDashed, Gavel, Layers, XCircle } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { BidDecision, RequirementItem, RequirementsView, VetoStatus } from '../../api/types'
import { Badge, Button, Card, Empty, Field, Input, Modal, TextArea, cx, useToast } from '../../components/ui'
import type { Tone } from '../../components/ui'
import { bidDecisionLabel, bidRecommendationLabel, docRoleLabel, fmtTime, vetoKindLabel, vetoStatusLabel } from '../../lib/format'

// D1：投标范围（截止时间、投标分包）、Bid/No-Bid 评估决策与否决项核对（server/proposal.go 的 bid-scope / veto 路由）。

function vetoTone(s: VetoStatus): Tone {
  return s === 'passed' ? 'green' : s === 'failed' ? 'red' : 'amber'
}

function decisionTone(d: BidDecision): Tone {
  return d === 'bid' ? 'green' : d === 'no_bid' ? 'red' : 'amber'
}

function recommendationTone(r: string): Tone {
  return r === 'bid' || r === 'can_bid' ? 'green' : r === 'no_bid' ? 'red' : 'amber'
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
  const ev = view.bid_evaluation
  const [deadline, setDeadline] = useState<string>()
  const [decisionDraft, setDecisionDraft] = useState<{ decision: BidDecision; note: string }>()
  const [busy, setBusy] = useState(false)
  const src = m.bid_deadline_source ?? {}
  const save = async (req: Parameters<typeof api.setBidScope>[1], ok: string) => {
    setBusy(true)
    try {
      await api.setBidScope(taskID, req)
      toast('ok', ok)
      setDeadline(undefined)
      setDecisionDraft(undefined)
      onChanged()
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setBusy(false)
    }
  }
  const toggleLot = (lot: string) => {
    const current = m.bid_lots.length ? m.bid_lots : lots
    const next = current.includes(lot) ? current.filter((l) => l !== lot) : [...current, lot]
    if (next.length === 0) { toast('error', '至少保留一个投标分包；全部都投时请点击「全部分包」'); return }
    void save({ bid_lots: next.length === lots.length ? [] : next }, '投标分包已更新，清单与大纲需重新确认')
  }
  const curDecision: BidDecision = m.bid_decision || 'pending'
  return (
    <Card title={<span className="flex items-center gap-1.5"><CalendarClock className="size-4" />投标范围与 Bid / No-Bid 决策</span>}>
      <div className="grid gap-4 md:grid-cols-2">
        <div className="text-sm">
          <div className="text-xs text-slate-500">投标截止时间（北京时间）</div>
          {deadline === undefined ? (
            <div className="mt-1 flex flex-wrap items-center gap-2">
              <span className={cx('font-medium', !m.bid_deadline && 'text-amber-700')}>{m.bid_deadline ? toLocalInput(m.bid_deadline) : '尚未确认'}</span>
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

      {ev && (
        <div className="mt-4 border-t border-slate-100 pt-3 text-sm">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className="flex items-center gap-1 text-xs text-slate-500"><Gavel className="size-3.5" />Bid / No-Bid 评估与决策</span>
              <Badge dot tone={decisionTone(curDecision)}>{bidDecisionLabel[curDecision] ?? curDecision}</Badge>
              <Badge tone={recommendationTone(ev.recommendation)}>系统评估：{bidRecommendationLabel[ev.recommendation] ?? ev.recommendation}</Badge>
              <span className="text-xs text-slate-500">
                客观分 {ev.objective_score} · 主观分 {ev.subjective_score} · 价格分 {ev.price_score} · 证据支撑分 {ev.evidence_backed_score}/{ev.total_score} · ★ 条款证据 {ev.mandatory_with_evidence}/{ev.mandatory_count}
              </span>
            </div>
            {!decisionDraft && (
              <div className="flex flex-wrap items-center gap-1.5">
                <Button size="sm" variant={curDecision === 'bid' ? 'primary' : 'ghost'} disabled={busy || view.stats.veto.failed > 0}
                  title={view.stats.veto.failed > 0 ? '存在不通过的否决项，不能决策为投标' : ''}
                  onClick={() => setDecisionDraft({ decision: 'bid', note: m.bid_decision_note || '' })}>决定投标</Button>
                <Button size="sm" variant={curDecision === 'no_bid' ? 'danger' : 'ghost'} disabled={busy}
                  onClick={() => setDecisionDraft({ decision: 'no_bid', note: m.bid_decision_note || '' })}>放弃投标</Button>
                {curDecision !== 'pending' && (
                  <Button size="sm" variant="ghost" loading={busy}
                    onClick={() => void save({ bid_decision: 'pending', bid_decision_note: '' }, '已重置为待决策')}>重置</Button>
                )}
              </div>
            )}
          </div>
          {m.bid_decision_note && !decisionDraft && (
            <p className="mt-1 text-xs text-slate-600">
              决策依据：{m.bid_decision_note}
              {m.bid_decided_by && <span className="text-slate-400"> · {m.bid_decided_by} {m.bid_decided_at ? fmtTime(m.bid_decided_at) : ''}</span>}
            </p>
          )}
          {ev.gaps && ev.gaps.length > 0 && (
            <ul className="mt-1.5 list-disc pl-5 text-xs text-amber-700">
              {ev.gaps.map((g) => <li key={g}>{g}</li>)}
            </ul>
          )}
          {decisionDraft && (
            <div className="mt-2 flex flex-wrap items-center gap-2 rounded-lg bg-slate-50 p-2.5">
              <Badge tone={decisionTone(decisionDraft.decision)}>{bidDecisionLabel[decisionDraft.decision]}</Badge>
              <Input className="min-w-[260px] flex-1" value={decisionDraft.note}
                placeholder={decisionDraft.decision === 'no_bid' ? '必填：放弃投标原因（如关键资格不满足、核心案例缺失）' : '可选：投标决策备注或风险应对说明'}
                onChange={(e) => setDecisionDraft({ ...decisionDraft, note: e.target.value })} />
              <Button size="sm" variant={decisionDraft.decision === 'no_bid' ? 'danger' : 'primary'} loading={busy}
                disabled={decisionDraft.decision === 'no_bid' && !decisionDraft.note.trim()}
                onClick={() => void save({ bid_decision: decisionDraft.decision, bid_decision_note: decisionDraft.note.trim() }, `已保存决策：${bidDecisionLabel[decisionDraft.decision]}`)}>确认决策</Button>
              <Button size="sm" variant="ghost" onClick={() => setDecisionDraft(undefined)}>取消</Button>
            </div>
          )}
        </div>
      )}
    </Card>
  )
}

export function VetoChecklist({ taskID, items, onChanged, highlightId }: { taskID: number; items: RequirementItem[]; onChanged: () => void; highlightId?: number }) {
  const [checking, setChecking] = useState<{ item: RequirementItem; status: VetoStatus }>()
  const vetoes = items.filter((i) => i.category === 'veto_item' && !i.out_of_bid && !i.superseded_by_id && i.review_status !== 'stale')
  const failed = vetoes.filter((i) => i.veto_status === 'failed').length
  const pending = vetoes.filter((i) => i.veto_status === 'pending').length
  return (
    <Card title={<>否决项核对 <Badge tone={failed ? 'red' : pending ? 'amber' : vetoes.length ? 'green' : 'slate'}>
      通过 {vetoes.length - failed - pending}/{vetoes.length}{failed ? ` · 不通过 ${failed}` : ''}</Badge></>}>
      {vetoes.length === 0 ? <Empty>尚无否决项。上传招标文件后点击「从招标文件提取」：资格性 / 符合性审查表逐行生成否决项，另有 87 号令法定无效情形 6 项。</Empty> : (
        <div className="divide-y divide-slate-100">
          {vetoes.map((it) => (
            <div key={it.id} id={`req-${it.id}`} className={cx('flex flex-wrap items-start gap-3 py-2 text-sm', highlightId === it.id && '-mx-2 rounded-lg bg-soft px-2 ring-1 ring-fg')}>
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-1.5">
                  <Badge dot tone={vetoTone(it.veto_status)}>{vetoStatusLabel[it.veto_status]}</Badge>
                  <Badge>{vetoKindLabel[it.veto_kind] ?? it.veto_kind}</Badge>
                  <span className="font-medium">{it.title}</span>
                  {it.lot && <span className="text-xs text-slate-400">包{it.lot}</span>}
                </div>
                {it.proof_rules?.raw_summary && (
                  <p className="mt-0.5 text-xs text-amber-700">证明要求：{it.proof_rules.raw_summary}</p>
                )}
                <details><summary className="cursor-pointer text-xs text-slate-400">审查内容</summary>
                  <p className="whitespace-pre-wrap text-xs text-slate-600">{it.description}</p>
                  {it.source_loc.covered_by && it.source_loc.covered_by.length > 0 &&
                    <p className="text-xs text-slate-400">对应审查表：{it.source_loc.covered_by.join('、')}</p>}</details>
                {it.veto_note && <p className="text-xs text-slate-600">依据：{it.veto_note}
                  <span className="text-slate-400"> · {it.veto_checked_by} {it.veto_checked_at ? fmtTime(it.veto_checked_at) : ''}</span></p>}
              </div>
              <div className="flex shrink-0 gap-1">
                <Button size="sm" variant="ghost" title="核对通过" aria-label={`核对通过：${it.title}`} onClick={() => setChecking({ item: it, status: 'passed' })}><CheckCircle2 className="size-4 text-green-600" /></Button>
                <Button size="sm" variant="ghost" title="核对不通过" aria-label={`核对不通过：${it.title}`} onClick={() => setChecking({ item: it, status: 'failed' })}><XCircle className="size-4 text-red-600" /></Button>
                {it.veto_status !== 'pending' && <Button size="sm" variant="ghost" title="退回待核对" aria-label={`退回待核对：${it.title}`} onClick={() => setChecking({ item: it, status: 'pending' })}><CircleDashed className="size-4 text-slate-400" /></Button>}
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
    <Modal open title={`${vetoStatusLabel[status]}：${item.title}`} onClose={onClose} dirty={note.trim() !== (status === item.veto_status ? item.veto_note : '').trim()}
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
