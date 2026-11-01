import { Fragment, useState } from 'react'
import { Bot, Check, ChevronDown, ChevronRight, Copy, ExternalLink, RefreshCw, ShieldAlert, X } from 'lucide-react'
import { ApiError, api, errorText } from '../api/client'
import type { InterceptApprovalRow, InterceptAudit, InterceptPending } from '../api/types'
import { useAsync } from '../hooks/useAsync'
import { fmtTime, pretty } from '../lib/format'
import { linkProps } from '../lib/router'
import { taskUrl } from '../lib/routes'
import { Badge, Button, Card, Empty, ErrorBanner, Pre, Select, Spinner, TextArea, statusTone, useConfirm, useToast } from './ui'

const statusLabel: Record<string, string> = {
  pending: '待审批',
  allowed: '已允许',
  denied: '已拒绝',
  timeout: '已超时',
}

const execOutcomeLabel: Record<string, string> = {
  executed: '执行成功',
  not_executed: '未执行',
  error: '执行出错',
}

function CopyButton({ text, label = '复制' }: { text: string; label?: string }) {
  const toast = useToast()
  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      toast('ok', '已复制到剪贴板')
    } catch {
      toast('error', '复制失败，请手动选择复制')
    }
  }
  return (
    <Button size="sm" variant="ghost" icon={<Copy className="size-3.5" />} onClick={() => void onCopy()}>
      {label}
    </Button>
  )
}

function PendingCard({ row, onDecided, showTask }: { row: InterceptPending; onDecided: () => void; showTask?: boolean }) {
  const toast = useToast()
  const confirm = useConfirm()
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState<'' | 'allow' | 'deny'>('')
  const decide = async (allow: boolean) => {
    if (!allow && !reason.trim() && !(await confirm({ title: '未填写拒绝原因，继续拒绝？', message: 'Worker 将只收到“人工审批未通过”。', confirmLabel: '拒绝', danger: true }))) return
    setBusy(allow ? 'allow' : 'deny')
    try {
      const r = await api.decideIntercept(row.id, allow, reason.trim())
      toast('ok', `审批 #${row.id} 已${allow ? '允许' : '拒绝'}（${r.status}）`)
      onDecided()
    } catch (e) {
      if (e instanceof ApiError && e.status === 410) toast('error', `审批 #${row.id} 已失效：等待方已结束（工作取消、超时或服务重启）`)
      else if (e instanceof ApiError && e.status === 409) toast('error', `审批 #${row.id} 已被处理或已超时回收：${e.message}`)
      else toast('error', errorText(e))
      onDecided()
    } finally {
      setBusy('')
    }
  }
  const formattedInput = pretty(row.tool_input)
  return (
    <div className="rounded-lg border border-amber-200 bg-amber-50/40 p-3">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <ShieldAlert className="size-4 text-amber-600" />
        <span className="font-semibold">#{row.id} · {row.tool_name}</span>
        <Badge tone="amber">{statusLabel[row.status] ?? row.status}</Badge>
        {row.rule_id > 0 ? <Badge tone="slate">规则 #{row.rule_id}</Badge> : <Badge tone="purple"><Bot className="mr-1 inline size-3" />模型判定</Badge>}
        <span className="text-xs text-muted">{row.agent_name} · Intent #{row.intent_id}</span>
        {showTask && row.task_id > 0 && (
          <a {...linkProps(`/tasks/${row.task_id}/approvals`)} className="text-xs text-link hover:underline">项目 #{row.task_id}</a>
        )}
        {row.task_id > 0 && (
          <a
            {...linkProps(taskUrl(row.task_id, 'monitor', row.intent_id ? { query: { work: row.intent_id } } : undefined))}
            className="inline-flex items-center gap-1 text-xs text-link hover:underline"
          >
            定位执行节点 <ExternalLink className="size-3" />
          </a>
        )}
        <span className="ml-auto text-xs text-muted">提交 {fmtTime(row.created_at)} · 等待方心跳至 {fmtTime(row.waiter_until)}</span>
      </div>
      {row.message && <p className="mt-2 text-sm text-fg">{row.message}</p>}
      <div className="mt-2 flex items-center justify-between text-xs text-muted">
        <span>工具入参</span>
        <CopyButton text={formattedInput} label="复制入参" />
      </div>
      <Pre className="mt-1 bg-panel">{formattedInput}</Pre>
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
    <Card title={<><ShieldAlert className="size-4 text-warn" /> 待人工审批的工具调用 {rows && rows.length > 0 && <Badge tone="red">{rows.length}</Badge>}</>}
      actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={onReload}>刷新</Button>}>
      <ErrorBanner error={error} onRetry={onReload} />
      {loading && !rows ? <Spinner /> : !rows || rows.length === 0 ? (
        <Empty>{compact ? '暂无待审批请求' : '暂无待审批请求。启用拦截规则或模型裁判的工具调用在执行前会出现在这里，并实时推送到活动流。'}</Empty>
      ) : (
        <div className="space-y-3">{rows.map((r) => <PendingCard key={r.id} row={r} onDecided={onDecided} showTask={showTask} />)}</div>
      )}
    </Card>
  )
}

/** 多状态审批历史与详情面板（对标 approval-records.tsx）。 */
export function ApprovalRecordsTable({ taskID, onDecided }: { taskID?: number; onDecided?: () => void }) {
  const [status, setStatus] = useState('')
  const [expanded, setExpanded] = useState<number | null>(null)
  const records = useAsync(() => api.interceptApprovals({ task_id: taskID, status, limit: 100 }), [taskID, status])
  const items: InterceptApprovalRow[] = records.data?.items ?? []

  return (
    <Card
      title={<>全部审批记录 {records.data ? <Badge tone="slate">{records.data.total}</Badge> : null}</>}
      actions={
        <div className="flex items-center gap-2">
          <Select aria-label="审批状态筛选" className="h-7 w-32 text-xs" value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="">全部状态</option>
            <option value="pending">待审批</option>
            <option value="allowed">已允许</option>
            <option value="denied">已拒绝</option>
            <option value="timeout">已超时</option>
          </Select>
          <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => records.reload()}>刷新</Button>
        </div>
      }
      bodyClassName="p-0"
    >
      <div className="p-3 empty:hidden"><ErrorBanner error={records.error} onRetry={records.reload} /></div>
      {records.loading && !records.data ? (
        <Spinner />
      ) : items.length === 0 ? (
        <Empty>暂无符合条件的工具审批记录</Empty>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="bg-soft text-left text-xs text-muted">
              <tr>
                <th className="w-8 px-3 py-2" />
                <th className="px-3 py-2">单号 / 工具</th>
                {!taskID && <th className="px-3 py-2">所属项目</th>}
                <th className="px-3 py-2">Agent / Intent</th>
                <th className="px-3 py-2">触发来源</th>
                <th className="px-3 py-2">审批状态</th>
                <th className="px-3 py-2">执行结果</th>
                <th className="px-3 py-2">提交时间</th>
              </tr>
            </thead>
            <tbody>
              {items.map((r) => {
                const open = expanded === r.id
                const isModel = r.decision_source === 'model' || (!r.rule_id && r.reason?.includes('模型'))
                return (
                  <Fragment key={r.id}>
                    <tr
                      onClick={() => setExpanded(open ? null : r.id)}
                      className="cursor-pointer border-t border-line-soft hover:bg-soft/50"
                    >
                      <td className="px-3 py-2 text-muted">
                        {open ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
                      </td>
                      <td className="px-3 py-2 font-medium">
                        <span className="mr-1.5 text-xs text-muted tabular-nums">#{r.id}</span>
                        <code className="font-mono text-xs">{r.tool_name}</code>
                      </td>
                      {!taskID && (
                        <td className="px-3 py-2 text-xs" onClick={(e) => e.stopPropagation()}>
                          {r.task_id ? (
                            <a
                              {...linkProps(taskUrl(r.task_id, 'monitor', r.intent_id ? { query: { work: r.intent_id } } : undefined))}
                              className="inline-flex items-center gap-1 text-link hover:underline"
                            >
                              项目 #{r.task_id} <ExternalLink className="size-3" />
                            </a>
                          ) : '—'}
                        </td>
                      )}
                      <td className="px-3 py-2 text-xs text-muted">{r.agent_name} · #{r.intent_id || '—'}</td>
                      <td className="px-3 py-2">
                        {isModel ? (
                          <Badge tone="purple"><Bot className="mr-1 inline size-3" />模型判定</Badge>
                        ) : (
                          <span className="text-xs">{r.rule_name || (r.rule_id ? `规则 #${r.rule_id}` : '默认规则')}</span>
                        )}
                      </td>
                      <td className="px-3 py-2">
                        <Badge tone={statusTone(r.status)}>{statusLabel[r.status] ?? r.status}</Badge>
                      </td>
                      <td className="px-3 py-2 text-xs text-muted">
                        {r.exec_outcome ? (execOutcomeLabel[r.exec_outcome] ?? r.exec_outcome) : '—'}
                      </td>
                      <td className="px-3 py-2 text-xs text-muted tabular-nums">{fmtTime(r.created_at)}</td>
                    </tr>
                    {open && (
                      <tr className="border-t border-line-soft bg-soft/30">
                        <td colSpan={taskID ? 7 : 8} className="p-4">
                          {r.status === 'pending' ? (
                            <PendingCard
                              row={r}
                              showTask={!taskID}
                              onDecided={() => {
                                void records.reload()
                                onDecided?.()
                              }}
                            />
                          ) : (
                            <div className="space-y-3 text-xs">
                              <div className="grid gap-2 sm:grid-cols-3">
                                <div className="rounded-lg border border-line bg-panel p-2.5">
                                  <div className="text-muted">生效动作 / 裁决时间</div>
                                  <div className="mt-1 font-medium">
                                    <Badge tone={statusTone(r.effective_action)}>{r.effective_action || '—'}</Badge>
                                    <span className="ml-2 text-muted">{fmtTime(r.decided_at)}</span>
                                  </div>
                                </div>
                                <div className="rounded-lg border border-line bg-panel p-2.5">
                                  <div className="text-muted">审批意见 / 触发说明</div>
                                  <div className="mt-1 font-medium">{r.reason || r.message || '未记录理由'}</div>
                                </div>
                                <div className="rounded-lg border border-line bg-panel p-2.5">
                                  <div className="text-muted">执行详情</div>
                                  <div className="mt-1 font-medium">{r.exec_detail || execOutcomeLabel[r.exec_outcome] || '无额外输出'}</div>
                                </div>
                              </div>
                              <div>
                                <div className="mb-1 flex items-center justify-between">
                                  <span className="font-medium text-muted">工具入参快照</span>
                                  <CopyButton text={pretty(r.tool_input)} label="复制入参" />
                                </div>
                                <Pre className="bg-panel">{pretty(r.tool_input)}</Pre>
                              </div>
                              {r.audit != null && Object.keys(r.audit as Record<string, unknown>).length > 0 ? (
                                <div>
                                  <div className="mb-1 font-medium text-muted">模型审查与审计上下文</div>
                                  <Pre className="bg-panel">{pretty(r.audit)}</Pre>
                                </div>
                              ) : null}
                            </div>
                          )}
                        </td>
                      </tr>
                    )}
                  </Fragment>
                )
              })}
            </tbody>
          </table>
        </div>
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
            <thead className="bg-soft text-left text-xs text-muted">
              <tr><th className="px-3 py-2">工具</th><th className="px-3 py-2">Agent / Intent</th><th className="px-3 py-2">来源</th><th className="px-3 py-2">判定</th><th className="px-3 py-2">生效</th><th className="px-3 py-2">审批单</th><th className="px-3 py-2">原因</th></tr>
            </thead>
            <tbody>
              {rows.map((a, i) => (
                <tr key={i} className="border-t border-line-soft align-top">
                  <td className="px-3 py-2 font-mono text-xs font-medium">{a.tool_name}</td>
                  <td className="px-3 py-2 text-xs text-muted">{a.agent_name} · #{a.intent_id || '—'}</td>
                  <td className="px-3 py-2"><Badge tone={a.source === 'judge' ? 'purple' : 'slate'}>{a.source === 'judge' ? '模型裁判' : a.source}</Badge></td>
                  <td className="px-3 py-2"><Badge tone={statusTone(a.action)}>{a.action}</Badge></td>
                  <td className="px-3 py-2"><Badge tone={statusTone(a.effective_action)}>{a.effective_action}</Badge></td>
                  <td className="px-3 py-2 text-xs text-muted">{a.pending_id ? `#${a.pending_id}` : '—'}</td>
                  <td className="max-w-md px-3 py-2 text-xs break-all text-muted">{a.reason || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}
