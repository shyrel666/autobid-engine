// D4：提交检查清单（白手套清单）。挂在导出页每个严格最终稿请求组之下：
// 系统可查项由服务器按成稿的回读结果判定（页面不能改），人工项与“需确认知悉”的系统项由人确认并留痕。
// 清单只是核对记录——就绪不等于已在交易平台提交；成稿过期（导出后正文又变了）后不能再确认。

import { useState } from 'react'
import { ChevronDown, ChevronRight, ClipboardCheck, History, RefreshCw } from 'lucide-react'
import { api, errorText, ApiError } from '../../../api/client'
import type { SubmissionCheckReq, SubmissionItem, SubmissionResp } from '../../../api/types'
import { Badge, Button, ErrorBanner, Field, IconButton, Input, Modal, Notice, Spinner, TextArea, cx, useToast } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'
import { fmtTime } from '../../../lib/format'
import { SUBMISSION_ACTION, checklistSummary, itemView, splitItems } from './submission'

type Action = 'confirm' | 'fail' | 'not_applicable'
const actionStatus: Record<Action, SubmissionCheckReq['status']> = { confirm: 'confirmed', fail: 'failed', not_applicable: 'not_applicable' }
const actionLabel: Record<Action, string> = { confirm: '确认', fail: '未通过', not_applicable: '不适用' }

function checkError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.code === 'not_final') return `这份成稿现在不能确认：${e.message.replace(/^submission:[^:]*:\s*/, '')}。请重新导出最终稿后再核对。`
    if (e.code === 'stale_revision') return '这一项刚被其他人修改过，已刷新为最新状态，请核对后重试。'
    if (/operator name is required/.test(e.message)) return '登录已失效，请重新登录后再确认。'
    if (e.code === 'invalid_check') return `不能这样处理该项：${e.message.replace(/^submission:[^:]*:\s*/, '')}`
  }
  return errorText(e)
}

export function SubmissionChecklist({ taskID, requestID, tick }: { taskID: number; requestID: number; tick: number }) {
  const toast = useToast()
  const [open, setOpen] = useState(false)
  const [refresh, setRefresh] = useState(0)
  const [override, setOverride] = useState<SubmissionResp>()
  const [editing, setEditing] = useState<{ item: SubmissionItem; action: Action }>()
  const [showLog, setShowLog] = useState(false)
  const [busy, setBusy] = useState('')
  const data = useAsync(async () => { const r = await api.submission(taskID, requestID); setOverride(undefined); return r }, [taskID, requestID, tick, refresh])
  const resp = override ?? data.data
  const cl = resp?.checklist
  const label = `提交检查清单（请求组 #${requestID}）`

  if (!cl) {
    return (
      <div className="border-t border-slate-100 px-3 py-2 text-xs" role="region" aria-label={label}>
        {data.error ? <ErrorBanner error={data.error} onRetry={data.reload} /> : <Spinner label="读取提交检查清单…" />}
      </div>
    )
  }
  const sum = checklistSummary(cl)
  const { auto, manual } = splitItems(cl)
  const withdraw = async (it: SubmissionItem) => {
    setBusy(it.key)
    try {
      setOverride(await api.withdrawSubmissionCheck(taskID, requestID, it.key, it.version))
      toast('ok', `已撤回「${it.label}」的确认`)
    } catch (e) { toast('error', checkError(e)); setRefresh((n) => n + 1) } finally { setBusy('') }
  }
  const row = (it: SubmissionItem) => {
    const v = itemView(it, sum.writable)
    return (
      <li key={it.key} data-check={it.key} className="flex flex-wrap items-start gap-x-3 gap-y-1 px-3 py-2">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium text-slate-800">{it.label}</span>
            <Badge dot tone={v.tone}>{v.label}</Badge>
          </div>
          {it.help && <p className="mt-0.5 text-xs text-slate-500">{it.help}</p>}
          {it.detail && <p className={cx('mt-0.5 text-xs', it.status === 'fail' ? 'text-red-600' : it.status === 'attention' ? 'text-amber-700' : 'text-slate-600')}>{it.detail}</p>}
          {it.confirmed_by && (
            <p className="mt-0.5 text-xs text-slate-500">
              {it.confirmed_by} · {fmtTime(it.confirmed_at)}{it.reference && ` · 凭证：${it.reference}`}{it.note && ` · ${it.note}`}
            </p>
          )}
        </div>
        <div className="flex shrink-0 flex-wrap gap-1.5">
          {v.actions.map((a) => a === 'withdraw'
            ? <Button key={a} size="sm" variant="ghost" loading={busy === it.key} onClick={() => void withdraw(it)}>撤回</Button>
            : <Button key={a} size="sm" variant={a === 'confirm' ? 'primary' : 'secondary'} aria-label={`${a === 'confirm' ? v.confirmLabel : actionLabel[a]}：${it.label}`}
                onClick={() => setEditing({ item: it, action: a })}>{a === 'confirm' ? v.confirmLabel : actionLabel[a]}</Button>)}
        </div>
      </li>
    )
  }

  return (
    <div className="border-t border-slate-100" role="region" aria-label={label}>
      <div className="flex flex-wrap items-center gap-2 px-3 py-2 text-xs">
        <button type="button" className="inline-flex items-center gap-1 text-sm font-medium text-slate-800" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
          {open ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}<ClipboardCheck className="size-4" />提交检查清单
        </button>
        <Badge dot tone={sum.tone}>{sum.label}</Badge>
        <span className="min-w-0 flex-1 text-slate-500">{sum.note}</span>
        <IconButton label="刷新提交检查清单" onClick={() => setRefresh((n) => n + 1)}><RefreshCw className="size-4" /></IconButton>
      </div>
      {open && (
        <div className="space-y-3 px-3 pb-3">
          <div>
            <div className="mb-1 text-xs font-medium text-slate-500">系统可查项（按这份成稿的回读结果判定，不能手工改）</div>
            <ul className="divide-y divide-slate-100 rounded-md border border-slate-200">{auto.map(row)}</ul>
          </div>
          <div>
            <div className="mb-1 text-xs font-medium text-slate-500">人工确认项（系统看不到，由核对人确认并记录）</div>
            <ul className="divide-y divide-slate-100 rounded-md border border-slate-200">{manual.map(row)}</ul>
          </div>
          <div className="text-xs text-slate-500">
            <button type="button" className="inline-flex items-center gap-1 hover:underline" aria-expanded={showLog} onClick={() => setShowLog((s) => !s)}>
              <History className="size-3.5" />确认记录 {resp.events.length} 条{showLog ? '，点击收起' : '，点击展开'}
            </button>
            {showLog && (resp.events.length === 0 ? <p className="mt-1">尚无确认记录。</p> : (
              <ul className="mt-1 space-y-0.5">
                {resp.events.map((e) => (
                  <li key={e.id}>
                    {fmtTime(e.created_at)} · {e.actor || '—'} · {SUBMISSION_ACTION[e.action] ?? e.action}「{cl.items.find((it) => it.key === e.item_key)?.label ?? e.item_key}」
                    {e.reference && ` · 凭证：${e.reference}`}{e.note && ` · ${e.note}`}
                  </li>
                ))}
              </ul>
            ))}
          </div>
          <p className="text-xs text-slate-400">确认记录绑定这份成稿的文件指纹，确认人是当前登录账号。</p>
        </div>
      )}
      {editing && <CheckModal taskID={taskID} requestID={requestID} item={editing.item} action={editing.action}
        onClose={() => setEditing(undefined)}
        onDone={(r) => { setOverride(r); setEditing(undefined) }}
        onStale={() => { setEditing(undefined); setRefresh((n) => n + 1) }} />}
    </div>
  )
}

function CheckModal({ taskID, requestID, item, action, onClose, onDone, onStale }:
  { taskID: number; requestID: number; item: SubmissionItem; action: Action; onClose: () => void; onDone: (r: SubmissionResp) => void; onStale: () => void }) {
  const toast = useToast()
  const [note, setNote] = useState('')
  const [reference, setReference] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const v = itemView(item, true)
  const noteRequired = action !== 'confirm' || v.noteRequired
  const title = action === 'confirm' ? v.confirmLabel : action === 'fail' ? '记为检查未通过' : '标为不适用'
  const submit = async () => {
    setBusy(true); setError('')
    try {
      const r = await api.setSubmissionCheck(taskID, requestID, item.key,
        { status: actionStatus[action], note: note.trim() || undefined, reference: reference.trim() || undefined, expected_version: item.version || undefined })
      toast('ok', `「${item.label}」${action === 'confirm' ? '已确认' : action === 'fail' ? '已记为未通过' : '已标为不适用'}`)
      onDone(r)
    } catch (e) {
      if (e instanceof ApiError && (e.code === 'stale_revision' || e.code === 'not_final')) { toast('error', checkError(e)); onStale() }
      else setError(checkError(e))
    } finally { setBusy(false) }
  }
  return (
    <Modal open title={`${title}：${item.label}`} onClose={onClose} dirty={!!(note.trim() || reference.trim())}
      footer={<><Button onClick={onClose}>取消</Button>
        <Button variant="primary" loading={busy} disabled={noteRequired && !note.trim()} onClick={() => void submit()}>{title}</Button></>}>
      <div className="space-y-3">
        {item.help && <p className="text-sm">{item.help}</p>}
        {item.detail && <Notice tone={item.status === 'attention' ? 'warn' : 'info'}>{item.detail}</Notice>}
        {action === 'confirm' && item.kind === 'manual' && (
          <Field label="凭证（可选）" hint="如平台名称与工具版本、回执编号、签章时间">
            <Input value={reference} onChange={(e) => setReference(e.target.value)} />
          </Field>
        )}
        <Field label="说明" hint={noteRequired ? (action === 'confirm' ? '必填：写明为什么可以接受' : action === 'fail' ? '必填：写明哪里没有通过' : '必填：写明为什么不适用') : '可选'}>
          <TextArea rows={3} value={note} onChange={(e) => setNote(e.target.value)} />
        </Field>
        <p className="text-xs text-muted">确认会记录当前登录账号与时间，并绑定这份成稿的文件指纹。</p>
        <ErrorBanner error={error} />
      </div>
    </Modal>
  )
}
