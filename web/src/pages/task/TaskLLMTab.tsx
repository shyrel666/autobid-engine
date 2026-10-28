import { useEffect, useState } from 'react'
import { ArrowDown, ArrowUp, RefreshCw, RotateCcw, Save, X } from 'lucide-react'
import { api, errorText } from '../../api/client'
import { Badge, Button, Card, Empty, ErrorBanner, Select, Spinner, useToast } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import { fmtNum } from '../../lib/format'

export default function TaskLLMTab({ taskID }: { taskID: number }) {
  const toast = useToast()
  const usage = useAsync(() => api.llmUsage(taskID), [taskID])
  const chain = useAsync(() => api.llmChain(taskID), [taskID])
  const profiles = useAsync(() => api.listProfiles(), [])
  const [draft, setDraft] = useState<number[]>([])
  const [busy, setBusy] = useState('')
  useEffect(() => { setDraft((chain.data?.chain ?? []).map((c) => c.profile_id)) }, [chain.data])

  const name = (id: number) => profiles.data?.find((p) => p.id === id)?.name ?? `#${id}`
  const totals = (usage.data ?? []).reduce((a, u) => ({ calls: a.calls + u.calls, in: a.in + u.input_tokens, out: a.out + u.output_tokens }), { calls: 0, in: 0, out: 0 })
  const move = (i: number, d: number) => {
    const n = [...draft]; const j = i + d
    if (j < 0 || j >= n.length) return
    ;[n[i], n[j]] = [n[j], n[i]]; setDraft(n)
  }
  const save = async () => {
    setBusy('save')
    try { chain.setData(await api.setLLMChain(taskID, draft)); toast('ok', '配额链已更新') } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  const reset = async () => {
    setBusy('reset')
    try { chain.setData(await api.resetLLMChain(taskID)); toast('ok', '配额链已重置，因配额耗尽阻塞的意图已重新开放') } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }

  return (
    <div className="grid gap-4 xl:grid-cols-2">
      <Card title="模型调用用量" actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => usage.reload()}>刷新</Button>} bodyClassName="p-0">
        <div className="px-4 pt-3 empty:hidden"><ErrorBanner error={usage.error} onRetry={usage.reload} /></div>
        {usage.loading && !usage.data ? <Spinner /> : (usage.data ?? []).length === 0 ? <Empty>暂无调用记录</Empty> : (
          <table className="w-full text-sm">
            <thead className="bg-slate-50 text-left text-xs text-slate-500">
              <tr><th className="px-3 py-2">模型</th><th className="px-3 py-2 text-right">调用</th><th className="px-3 py-2 text-right">输入</th><th className="px-3 py-2 text-right">输出</th><th className="px-3 py-2 text-right">缓存读/写</th></tr>
            </thead>
            <tbody className="tabular-nums">
              {usage.data!.map((u) => (
                <tr key={u.model} className="border-t border-slate-100">
                  <td className="px-3 py-2 font-mono text-xs">{u.model}</td>
                  <td className="px-3 py-2 text-right">{fmtNum(u.calls)}</td>
                  <td className="px-3 py-2 text-right">{fmtNum(u.input_tokens)}</td>
                  <td className="px-3 py-2 text-right">{fmtNum(u.output_tokens)}</td>
                  <td className="px-3 py-2 text-right text-xs text-slate-500">{fmtNum(u.cache_read)} / {fmtNum(u.cache_write)}</td>
                </tr>
              ))}
              <tr className="border-t border-slate-200 font-medium">
                <td className="px-3 py-2">合计</td><td className="px-3 py-2 text-right">{fmtNum(totals.calls)}</td>
                <td className="px-3 py-2 text-right">{fmtNum(totals.in)}</td><td className="px-3 py-2 text-right">{fmtNum(totals.out)}</td><td />
              </tr>
            </tbody>
          </table>
        )}
      </Card>
      <Card title="任务 LLM 配额链" actions={<Button size="sm" icon={<RotateCcw className="size-3.5" />} loading={busy === 'reset'} onClick={reset}>重置链路状态</Button>}>
        <ErrorBanner error={chain.error || profiles.error} onRetry={chain.reload} />
        {chain.loading && !chain.data ? <Spinner /> : (
          <div className="space-y-3">
            <p className="text-xs text-slate-500">按顺序尝试；配额耗尽时自动切换下一个。链为空时使用全局池。当前活动配置：{chain.data?.active_profile_id ? name(chain.data.active_profile_id) : '—'} · 修订 {chain.data?.revision ?? 0}</p>
            {(chain.data?.chain ?? []).length > 0 && (
              <div className="flex flex-wrap gap-1.5">
                {chain.data!.chain!.map((c) => <Badge key={c.position} tone={c.status === 'ready' ? 'green' : 'red'}>{c.position + 1}. {name(c.profile_id)} · {c.status === 'ready' ? '可用' : '配额耗尽'}</Badge>)}
              </div>
            )}
            <ul className="space-y-1">
              {draft.map((id, i) => (
                <li key={id} className="flex items-center gap-2 rounded border border-slate-200 px-2 py-1 text-sm">
                  <span className="w-5 text-xs text-slate-400">{i + 1}.</span><span className="flex-1">{name(id)}</span>
                  <Button size="sm" variant="ghost" onClick={() => move(i, -1)}><ArrowUp className="size-3" /></Button>
                  <Button size="sm" variant="ghost" onClick={() => move(i, 1)}><ArrowDown className="size-3" /></Button>
                  <Button size="sm" variant="ghost" onClick={() => setDraft(draft.filter((x) => x !== id))}><X className="size-3" /></Button>
                </li>
              ))}
            </ul>
            <div className="flex gap-2">
              <Select value="" onChange={(e) => e.target.value && setDraft([...draft, Number(e.target.value)])}>
                <option value="">+ 添加模型配置…</option>
                {(profiles.data ?? []).filter((p) => !draft.includes(p.id)).map((p) => <option key={p.id} value={p.id}>{p.name}（{p.model}）{p.enabled ? '' : ' [已停用]'}</option>)}
              </Select>
              <Button variant="primary" icon={<Save className="size-4" />} loading={busy === 'save'} onClick={save}>保存</Button>
            </div>
          </div>
        )}
      </Card>
    </div>
  )
}
