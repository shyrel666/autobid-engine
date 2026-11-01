import { useEffect, useState } from 'react'
import { AlertCircle, RefreshCw, RotateCcw, Save } from 'lucide-react'
import { api, errorText } from '../../api/client'
import { Badge, Button, Card, Empty, ErrorBanner, Spinner, Stat, useToast } from '../../components/ui'
import { TaskLLMProfileChain } from '../../features/models/TaskLLMProfileChain'
import { useAsync } from '../../hooks/useAsync'
import { fmtNum } from '../../lib/format'

export function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`
  if (n >= 10_000) return `${(n / 1_000).toFixed(1)}k`
  return fmtNum(n)
}

export function cacheHitRate(cacheRead: number, inputTokens: number): string {
  if (inputTokens <= 0 || cacheRead <= 0) return '0%'
  return `${Math.min(100, (cacheRead / inputTokens) * 100).toFixed(1)}%`
}

export default function TaskLLMTab({ taskID }: { taskID: number }) {
  const toast = useToast()
  const usage = useAsync(() => api.llmUsage(taskID), [taskID])
  const chain = useAsync(() => api.llmChain(taskID), [taskID])
  const profiles = useAsync(() => api.listProfiles(), [])
  const [draft, setDraft] = useState<number[]>([])
  const [activeID, setActiveID] = useState<number>(0)
  const [busy, setBusy] = useState('')

  useEffect(() => {
    const ids = (chain.data?.chain ?? []).map((c) => c.profile_id)
    setDraft(ids)
    setActiveID(chain.data?.active_profile_id ?? ids[0] ?? 0)
  }, [chain.data])

  const name = (id: number) => profiles.data?.find((p) => p.id === id)?.name ?? `#${id}`
  const totals = (usage.data ?? []).reduce(
    (a, u) => ({
      calls: a.calls + u.calls,
      in: a.in + u.input_tokens,
      out: a.out + u.output_tokens,
      cacheRead: a.cacheRead + u.cache_read,
      cacheWrite: a.cacheWrite + u.cache_write,
    }),
    { calls: 0, in: 0, out: 0, cacheRead: 0, cacheWrite: 0 },
  )

  const exhausted =
    chain.data?.failover_state === 'exhausted' ||
    ((chain.data?.chain ?? []).length > 0 && (chain.data?.chain ?? []).every((c) => c.status === 'quota_exhausted'))

  const save = async () => {
    setBusy('save')
    try {
      const res = await api.setLLMChain(taskID, draft, activeID || undefined)
      chain.setData(res)
      toast(
        'ok',
        res.reopened_intents && res.reopened_intents > 0
          ? `配额链已更新，并恢复 ${res.reopened_intents} 条配额阻塞意图`
          : '配额链已更新',
      )
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setBusy('')
    }
  }

  const reset = async () => {
    setBusy('reset')
    try {
      const res = await api.resetLLMChain(taskID)
      chain.setData(res)
      toast('ok', '配额链已重置，因配额耗尽阻塞的意图已重新开放')
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setBusy('')
    }
  }

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="模型调用次数" value={fmtNum(totals.calls)} hint={`${(usage.data ?? []).length} 个模型`} />
        <Stat label="输入 Token" value={fmtTokens(totals.in)} hint={`精确 ${fmtNum(totals.in)}`} />
        <Stat label="输出 Token" value={fmtTokens(totals.out)} hint={`精确 ${fmtNum(totals.out)}`} />
        <Stat label="缓存命中率" value={cacheHitRate(totals.cacheRead, totals.in)} hint={`读 ${fmtTokens(totals.cacheRead)} / 写 ${fmtTokens(totals.cacheWrite)}`} />
      </div>

      <div className="grid gap-4 xl:grid-cols-2">
        <Card
          title="按模型 Token 用量明细"
          actions={
            <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => usage.reload()}>
              刷新
            </Button>
          }
          bodyClassName="p-0"
        >
          <div className="px-4 pt-3 empty:hidden">
            <ErrorBanner error={usage.error} onRetry={usage.reload} />
          </div>
          {usage.loading && !usage.data ? (
            <Spinner />
          ) : (usage.data ?? []).length === 0 ? (
            <Empty>暂无模型调用记录</Empty>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead className="bg-soft text-left text-xs text-muted">
                  <tr>
                    <th className="px-3 py-2">模型</th>
                    <th className="px-3 py-2 text-right">调用</th>
                    <th className="px-3 py-2 text-right">输入</th>
                    <th className="px-3 py-2 text-right">输出</th>
                    <th className="px-3 py-2 text-right">缓存读 / 写</th>
                    <th className="px-3 py-2 text-right">缓存命中率</th>
                  </tr>
                </thead>
                <tbody className="tabular-nums">
                  {usage.data!.map((u) => (
                    <tr key={u.model} className="border-t border-line-soft">
                      <td className="px-3 py-2 font-mono text-xs">{u.model}</td>
                      <td className="px-3 py-2 text-right">{fmtNum(u.calls)}</td>
                      <td className="px-3 py-2 text-right" title={fmtNum(u.input_tokens)}>{fmtTokens(u.input_tokens)}</td>
                      <td className="px-3 py-2 text-right" title={fmtNum(u.output_tokens)}>{fmtTokens(u.output_tokens)}</td>
                      <td className="px-3 py-2 text-right text-xs text-muted">
                        {fmtTokens(u.cache_read)} / {fmtTokens(u.cache_write)}
                      </td>
                      <td className="px-3 py-2 text-right">
                        <Badge tone={u.cache_read > 0 ? 'green' : 'slate'}>{cacheHitRate(u.cache_read, u.input_tokens)}</Badge>
                      </td>
                    </tr>
                  ))}
                  <tr className="border-t border-line font-semibold">
                    <td className="px-3 py-2">合计</td>
                    <td className="px-3 py-2 text-right">{fmtNum(totals.calls)}</td>
                    <td className="px-3 py-2 text-right">{fmtTokens(totals.in)}</td>
                    <td className="px-3 py-2 text-right">{fmtTokens(totals.out)}</td>
                    <td className="px-3 py-2 text-right text-xs text-muted">
                      {fmtTokens(totals.cacheRead)} / {fmtTokens(totals.cacheWrite)}
                    </td>
                    <td className="px-3 py-2 text-right">{cacheHitRate(totals.cacheRead, totals.in)}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <Card
          title="任务 LLM 配额链与故障转移"
          actions={
            <div className="flex items-center gap-2">
              <Button size="sm" icon={<RotateCcw className="size-3.5" />} loading={busy === 'reset'} onClick={reset}>
                重置链路状态
              </Button>
              <Button size="sm" variant="primary" icon={<Save className="size-3.5" />} loading={busy === 'save'} onClick={save}>
                {exhausted ? '保存并重置' : '保存配置链'}
              </Button>
            </div>
          }
        >
          <ErrorBanner error={chain.error || profiles.error} onRetry={chain.reload} />
          {chain.loading && !chain.data ? (
            <Spinner />
          ) : (
            <div className="space-y-3">
              <p className="text-xs text-muted">
                按顺序尝试；配额耗尽时自动切换到下一个可用配置。链为空时使用 Agent 绑定或全局池。当前活动配置：
                <b className="ml-1 text-fg">{chain.data?.active_profile_id ? name(chain.data.active_profile_id) : '默认模型池'}</b> · 修订 #{chain.data?.revision ?? 0}
              </p>

              {exhausted && (
                <div className="flex items-start gap-2 rounded-lg border border-danger/30 bg-[#fef2f2] p-2.5 text-xs text-danger">
                  <AlertCircle className="mt-0.5 size-4 shrink-0" />
                  <div>
                    <div className="font-semibold">配额链已全部耗尽</div>
                    <div className="mt-0.5">{chain.data?.failover_reason || '链中所有配置均被标记为配额耗尽，点击「重置链路状态」或「保存并重置」可恢复运行。'}</div>
                  </div>
                </div>
              )}

              <TaskLLMProfileChain
                profiles={profiles.data ?? []}
                value={draft}
                onValueChange={(next) => {
                  setDraft(next)
                  setActiveID((cur) => (next.includes(cur) ? cur : (next[0] ?? 0)))
                }}
                activeProfileId={activeID}
                onActiveProfileChange={setActiveID}
                chainEntries={chain.data?.chain}
                disabled={busy !== ''}
                inputId="tab-task-llm-profiles"
              />
            </div>
          )}
        </Card>
      </div>
    </div>
  )
}
