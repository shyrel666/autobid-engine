import { useState } from 'react'
import { Check, RefreshCw, ShieldAlert, X } from 'lucide-react'
import { api, errorText } from '../api/client'
import type { InterceptAudit, InterceptPending } from '../api/types'
import { fmtTime, pretty } from '../lib/format'
import { navigate } from '../lib/router'
import { Badge, Button, Card, Empty, ErrorBanner, Pre, Spinner, TextArea, statusTone, useToast } from './ui'

function PendingCard({ row, onDecided, showTask }: { row: InterceptPending; onDecided: () => void; showTask?: boolean }) {
  const toast = useToast()
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState<'' | 'allow' | 'deny'>('')
  const decide = async (allow: boolean) => {
    if (!allow && !reason.trim() && !confirm('未填写拒绝原因，Worker 将只收到“人工审批未通过”。继续拒绝？')) return
    setBusy(allow ? 'allow' : 'deny')
    try {
      const r = await api.decideIntercept(row.id, allow, reason.trim())
      toast('ok', `审批 #${row.id} 已${allow ? '允许' : '拒绝'}（${r.status}）`)
      onDecided()
    } catch (e) {
      // 409：已被他人/超时处理；410：等待方已离开（工作已取消或服务重启）
      toast('error', errorText(e))
      onDecided()
    } finally {
      setBusy('')
    }
  }
  return (
    <div className="rounded-lg border border-amber-200 bg-amber-50/40 p-3">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <ShieldAlert className="size-4 text-amber-600" />
        <span className="font-semibold">#{row.id} · {row.tool_name}</span>
        <Badge tone="amber">{row.status}</Badge>
        <span className="text-xs text-slate-500">{row.agent_name} · Intent #{row.intent_id}</span>
        {showTask && (
          <a href={`/tasks/${row.task_id}/approvals`} className="text-xs text-blue-600 hover:underline"
            onClick={(e) => { e.preventDefault(); navigate(`/tasks/${row.task_id}/approvals`) }}>任务 #{row.task_id}</a>
        )}
        <span className="ml-auto text-xs text-slate-400">提交 {fmtTime(row.created_at)} · 等待方心跳至 {fmtTime(row.waiter_until)}</span>
      </div>
      {row.message && <p className="mt-2 text-sm text-slate-700">{row.message}</p>}
      <div className="mt-2 text-xs text-slate-500">工具入参</div>
      <Pre className="mt-1 bg-white">{pretty(row.tool_input)}</Pre>
      <div className="mt-2 flex flex-col gap-2 md:flex-row md:items-end">
        <TextArea rows={2} className="font-sans" placeholder="审批意见 / 拒绝原因（将回传给 Worker）" value={reason} onChange={(e) => setReason(e.target.value)} />
        <div className="flex gap-2">
          <Button variant="success" icon={<Check className="size-4" />} loading={busy === 'allow'} disabled={!!busy} onClick={() => decide(true)}>允许</Button>
          <Button variant="danger" icon={<X className="size-4" />} loading={busy === 'deny'} disabled={!!busy} onClick={() => decide(false)}>拒绝</Button>
        </div>
      </div>
    </div>
  )
}

export function InterceptList({ rows, loading, error, onReload, onDecided, showTask, compact }:
  { rows?: InterceptPending[]; loading: boolean; error: string; onReload: () => void; onDecided: () => void; showTask?: boolean; compact?: boolean }) {
  return (
    <Card title={<><ShieldAlert className="size-4 text-amber-600" /> 待人工审批的工具调用 {rows && rows.length > 0 && <Badge tone="red">{rows.length}</Badge>}</>}
      actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={onReload}>刷新</Button>}>
      <ErrorBanner error={error} onRetry={onReload} />
      {loading && !rows ? <Spinner /> : !rows || rows.length === 0 ? (
        <Empty>{compact ? '暂无待审批请求' : '暂无待审批请求。启用拦截规则的工具调用在执行前会出现在这里，并实时推送到活动流。'}</Empty>
      ) : (
        <div className="space-y-3">{rows.map((r) => <PendingCard key={r.id} row={r} onDecided={onDecided} showTask={showTask} />)}</div>
      )}
    </Card>
  )
}

export function InterceptAuditTable({ rows, loading, error, onReload }: { rows?: InterceptAudit[]; loading: boolean; error: string; onReload: () => void }) {
  return (
    <Card title="拦截审计记录" actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={onReload}>刷新</Button>} bodyClassName="p-0">
      <div className="p-3 empty:hidden"><ErrorBanner error={error} onRetry={onReload} /></div>
      {loading && !rows ? <Spinner /> : !rows || rows.length === 0 ? <Empty>暂无审计记录</Empty> : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="bg-slate-50 text-left text-xs text-slate-500">
              <tr><th className="px-3 py-2">工具</th><th className="px-3 py-2">Agent / Intent</th><th className="px-3 py-2">来源</th><th className="px-3 py-2">判定</th><th className="px-3 py-2">生效</th><th className="px-3 py-2">审批单</th><th className="px-3 py-2">原因</th></tr>
            </thead>
            <tbody>
              {rows.map((a, i) => (
                <tr key={i} className="border-t border-slate-100 align-top">
                  <td className="px-3 py-2 font-medium">{a.tool_name}</td>
                  <td className="px-3 py-2 text-xs text-slate-500">{a.agent_name} · #{a.intent_id || '—'}</td>
                  <td className="px-3 py-2"><Badge>{a.source}</Badge></td>
                  <td className="px-3 py-2"><Badge tone={statusTone(a.action)}>{a.action}</Badge></td>
                  <td className="px-3 py-2"><Badge tone={statusTone(a.effective_action)}>{a.effective_action}</Badge></td>
                  <td className="px-3 py-2 text-xs text-slate-500">{a.pending_id ? `#${a.pending_id}` : '—'}</td>
                  <td className="max-w-md px-3 py-2 text-xs break-all text-slate-600">{a.reason || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}
