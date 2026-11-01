import { useEffect, useState } from 'react'
import { Check, ChevronLeft, ChevronRight, Copy, Radio, RefreshCw, Search, Trash2, X } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { LLMRecordDetail, LLMRecordSummary, LLMSettings } from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Input, Pre, Select, Spinner, cx, useConfirm, useToast } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import { fmtTime, pretty } from '../../lib/format'

function fmtLatency(ms: number) {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(2)}s`
}

function CopySnippetButton({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    if (!text) return
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      /* ignore */
    }
  }
  return (
    <Button size="sm" variant="ghost" disabled={!text} onClick={() => void copy()} title={`复制 ${label}`}>
      {copied ? <Check className="size-3.5 text-ok" /> : <Copy className="size-3.5" />}
      {copied ? '已复制' : '复制'}
    </Button>
  )
}

export function LLMRecordsPanel({
  settings,
  onSettingsUpdated,
}: {
  settings?: LLMSettings
  onSettingsUpdated: (s: LLMSettings) => void
}) {
  const toast = useToast()
  const confirm = useConfirm()
  const [sessionFilter, setSessionFilter] = useState('')
  const [profileFilter, setProfileFilter] = useState('')
  const [taskFilter, setTaskFilter] = useState<string>('')
  const [pageSize, setPageSize] = useState(50)
  const [page, setPage] = useState(0)
  const [selected, setSelected] = useState<LLMRecordSummary | null>(null)
  const [detail, setDetail] = useState<LLMRecordDetail | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [rawMode, setRawMode] = useState(false)
  const [toggling, setToggling] = useState(false)

  const tasks = useAsync(() => api.listTasks(), [])
  const list = useAsync(
    () =>
      api.llmRecords({
        task_id: taskFilter ? Number(taskFilter) : undefined,
        session_id: sessionFilter.trim() || undefined,
        profile_name: profileFilter.trim() || undefined,
        limit: pageSize,
        offset: page * pageSize,
      }),
    [taskFilter, sessionFilter, profileFilter, pageSize, page],
  )

  useEffect(() => {
    setPage(0)
  }, [taskFilter, sessionFilter, profileFilter, pageSize])

  useEffect(() => {
    if (!selected) {
      setDetail(null)
      return
    }
    let alive = true
    setDetailLoading(true)
    api
      .llmRecordDetail(selected.id)
      .then((d) => {
        if (alive) setDetail(d)
      })
      .catch((e) => {
        if (alive) toast('error', errorText(e))
      })
      .finally(() => {
        if (alive) setDetailLoading(false)
      })
    return () => {
      alive = false
    }
  }, [selected]) // eslint-disable-line react-hooks/exhaustive-deps

  const toggleRecording = async (next: boolean) => {
    if (!settings) return
    setToggling(true)
    try {
      const updated = await api.saveLLMSettings({ ...settings, record_bodies: next })
      onSettingsUpdated(updated)
      toast('ok', next ? '已开启 LLM 原始报文录制' : '已关闭 LLM 原始报文录制')
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setToggling(false)
    }
  }

  const clearRecords = async () => {
    const tid = taskFilter ? Number(taskFilter) : undefined
    const ok = await confirm({
      title: tid ? `删除项目 #${tid} 的全部 LLM 录制记录？` : '清空全部项目的 LLM 原始录制报文？',
      message: '将永久删除对应的 HTTP 请求与响应正文记录（Token 用量账本不受影响），此操作不可撤销。',
      confirmLabel: '确认清理',
      danger: true,
    })
    if (!ok) return
    try {
      const r = await api.deleteLLMRecords(tid)
      toast('ok', `已清理 ${r.deleted} 条录制记录`)
      setSelected(null)
      setDetail(null)
      void list.reload()
    } catch (e) {
      toast('error', errorText(e))
    }
  }

  const records = list.data?.items ?? []
  const total = list.data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const recEnabled = !!settings?.record_bodies

  const reqText = detail?.request_body ? (rawMode ? detail.request_body : pretty(detail.request_body)) : ''
  const respText = detail?.response_body ? (rawMode ? detail.response_body : pretty(detail.response_body)) : ''

  return (
    <div className="space-y-4">
      {/* 顶栏录制开关与过滤控制条 */}
      <div
        className={cx(
          'flex flex-wrap items-center justify-between gap-3 rounded-[12px] border px-4 py-3',
          recEnabled ? 'border-emerald-300 bg-emerald-50/40' : 'border-line bg-panel',
        )}
      >
        <div className="flex items-center gap-2.5">
          <Radio className={cx('size-5 shrink-0', recEnabled ? 'animate-pulse text-ok' : 'text-muted')} />
          <div>
            <div className="flex items-center gap-2 text-sm font-semibold">
              <span>HTTP 原始报文录制（Burp 风格回放）</span>
              <Badge tone={recEnabled ? 'green' : 'slate'}>{recEnabled ? '录制中' : '已关闭'}</Badge>
              {total > 0 && <Badge tone="blue">{total} 条记录</Badge>}
            </div>
            <p className="text-xs text-muted">
              开启后自动记录与模型供应商收发的 HTTP 请求体与响应体（不记录请求头，自动脱敏 API Key），用于排查工具调用 Schema 与推理输出。
            </p>
          </div>
        </div>
        <label className="inline-flex cursor-pointer items-center gap-2 rounded-lg border border-line bg-panel px-3 py-1.5 text-xs font-medium">
          <input
            type="checkbox"
            disabled={!settings || toggling}
            checked={recEnabled}
            onChange={(e) => void toggleRecording(e.target.checked)}
            className="size-4 accent-fg"
          />
          {recEnabled ? '正在录制报文' : '开启报文录制'}
        </label>
      </div>

      {/* 过滤器与分页工具栏 */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-48 flex-1 sm:max-w-xs">
          <Search className="pointer-events-none absolute top-2 left-2.5 size-4 text-muted" />
          <Input
            aria-label="搜索 Session ID"
            className="pl-8"
            placeholder="搜索 Session ID…"
            value={sessionFilter}
            onChange={(e) => setSessionFilter(e.target.value)}
          />
        </div>
        <Input
          aria-label="按配置名筛选"
          className="w-44"
          placeholder="配置名筛选…"
          value={profileFilter}
          onChange={(e) => setProfileFilter(e.target.value)}
        />
        <Select
          aria-label="按项目筛选"
          className="w-52"
          value={taskFilter}
          onChange={(e) => setTaskFilter(e.target.value)}
        >
          <option value="">全部项目</option>
          {(tasks.data ?? []).map((t) => (
            <option key={t.id} value={t.id}>
              #{t.id} {t.name}
            </option>
          ))}
        </Select>
        <Select
          aria-label="每页条数"
          className="w-28"
          value={pageSize}
          onChange={(e) => setPageSize(Number(e.target.value))}
        >
          <option value={25}>25 条/页</option>
          <option value={50}>50 条/页</option>
          <option value={100}>100 条/页</option>
        </Select>
        <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => list.reload()}>
          刷新
        </Button>
        <Button
          size="sm"
          variant="danger"
          icon={<Trash2 className="size-3.5" />}
          disabled={total === 0}
          onClick={() => void clearRecords()}
        >
          {taskFilter ? `清理项目 #${taskFilter}` : '清空全部'}
        </Button>

        {total > 0 && (
          <div className="ml-auto flex items-center gap-1.5 text-xs text-muted tabular-nums">
            <span>
              {page * pageSize + 1}–{page * pageSize + records.length} / {total}
            </span>
            <Button size="sm" disabled={page <= 0} onClick={() => setPage((p) => Math.max(0, p - 1))}>
              <ChevronLeft className="size-3.5" />
            </Button>
            <span>
              {page + 1} / {totalPages}
            </span>
            <Button size="sm" disabled={page + 1 >= totalPages} onClick={() => setPage((p) => Math.min(totalPages - 1, p + 1))}>
              <ChevronRight className="size-3.5" />
            </Button>
          </div>
        )}
      </div>

      <ErrorBanner error={list.error} onRetry={list.reload} />

      {/* 列表 + 底部 Request / Response 分栏 */}
      <Card bodyClassName="p-0">
        {list.loading && !list.data ? (
          <Spinner />
        ) : records.length === 0 ? (
          <Empty>
            {recEnabled
              ? '暂无录制记录。触发模型测试或项目推演后，HTTP 往返报文将显示在此处。'
              : '当前未开启报文录制。打开右上角「开启报文录制」即可捕获后续模型请求与响应。'}
          </Empty>
        ) : (
          <div className="max-h-96 overflow-auto">
            <table className="w-full text-sm">
              <thead className="sticky top-0 z-10 bg-soft text-left text-xs text-muted">
                <tr>
                  <th className="px-3 py-2">序号 / 时间</th>
                  <th className="px-3 py-2">项目</th>
                  <th className="px-3 py-2">Worker / Session</th>
                  <th className="px-3 py-2">模型配置</th>
                  <th className="px-3 py-2">HTTP 接口</th>
                  <th className="px-3 py-2 text-right">耗时</th>
                  <th className="px-3 py-2">状态</th>
                </tr>
              </thead>
              <tbody className="tabular-nums">
                {records.map((r) => {
                  const isSel = selected?.id === r.id
                  const ok = r.status_code >= 200 && r.status_code < 300 && !r.error
                  return (
                    <tr
                      key={r.id}
                      onClick={() => setSelected(r)}
                      className={cx(
                        'cursor-pointer border-t border-line-soft transition-colors',
                        isSel ? 'bg-soft font-medium' : 'hover:bg-soft/50',
                      )}
                    >
                      <td className="px-3 py-2 text-xs">
                        <span className="mr-1.5 font-mono text-muted">#{r.id}</span>
                        <span>{fmtTime(r.created_at)}</span>
                      </td>
                      <td className="px-3 py-2 font-mono text-xs text-muted">{r.task_id ? `#${r.task_id}` : '—'}</td>
                      <td className="px-3 py-2 text-xs">
                        <Badge tone="slate">{r.worker || 'default'}</Badge>
                        {r.session_id && <span className="ml-1.5 font-mono text-[11px] text-muted">{r.session_id.slice(0, 16)}</span>}
                      </td>
                      <td className="px-3 py-2 text-xs">{r.profile_name || (r.profile_id ? `#${r.profile_id}` : '—')}</td>
                      <td className="max-w-xs truncate px-3 py-2 font-mono text-xs text-muted" title={r.url}>
                        {r.method} {r.url}
                      </td>
                      <td className={cx('px-3 py-2 text-right text-xs', r.latency_ms > 25000 && 'text-warn font-semibold')}>
                        {fmtLatency(r.latency_ms)}
                      </td>
                      <td className="px-3 py-2">
                        <div className="flex items-center gap-1">
                          <Badge tone={ok ? 'green' : 'red'}>{r.status_code || 'ERR'}</Badge>
                          {r.truncated && <Badge tone="amber">已截断</Badge>}
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {/* Burp 风格底部 Request / Response 双栏对比回放面板 */}
      {selected && (
        <Card
          title={
            <div className="flex flex-wrap items-center gap-2 text-xs">
              <Badge tone="blue">录制 #{selected.id}</Badge>
              <span className="font-mono">{selected.profile_name || `#${selected.profile_id}`}</span>
              {selected.task_id > 0 && <Badge tone="slate">项目 #{selected.task_id}</Badge>}
              <span className="text-muted">{fmtTime(selected.created_at)} · {fmtLatency(selected.latency_ms)}</span>
              {selected.truncated && <Badge tone="amber">正文超长已截断</Badge>}
            </div>
          }
          actions={
            <div className="flex items-center gap-2">
              <Button size="sm" variant={rawMode ? 'primary' : 'ghost'} onClick={() => setRawMode((v) => !v)}>
                {rawMode ? '原始报文' : 'JSON 格式化'}
              </Button>
              <Button size="sm" variant="ghost" aria-label="关闭详情" onClick={() => setSelected(null)}>
                <X className="size-4" />
              </Button>
            </div>
          }
          bodyClassName="p-0"
        >
          {detailLoading ? (
            <Spinner />
          ) : (
            <div className="grid divide-y divide-line lg:grid-cols-2 lg:divide-x lg:divide-y-0">
              <div className="flex min-w-0 flex-col">
                <div className="flex items-center justify-between border-b border-line bg-soft/50 px-3 py-1.5 text-xs font-semibold text-muted">
                  <span>Request Body（{selected.method} {selected.url}）</span>
                  <CopySnippetButton text={reqText} label="Request Body" />
                </div>
                <Pre className="max-h-96 rounded-none border-0 bg-panel p-3">{reqText || '（空请求体）'}</Pre>
              </div>
              <div className="flex min-w-0 flex-col">
                <div className="flex items-center justify-between border-b border-line bg-soft/50 px-3 py-1.5 text-xs font-semibold text-muted">
                  <span>Response Body（HTTP {selected.status_code || 'ERR'}）</span>
                  <CopySnippetButton text={respText || selected.error} label="Response Body" />
                </div>
                {selected.error && (
                  <div className="border-b border-danger/20 bg-[#fef2f2] px-3 py-2 font-mono text-xs text-danger">
                    错误：{selected.error}
                  </div>
                )}
                <Pre className="max-h-96 rounded-none border-0 bg-panel p-3">{respText || '（空响应体）'}</Pre>
              </div>
            </div>
          )}
        </Card>
      )}
    </div>
  )
}
