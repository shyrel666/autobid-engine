import { useEffect, useState } from 'react'
import { Link2, RotateCcw, Save } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { LLMPoolMember, LLMPoolStatus, LLMProfile, LLMSettings } from '../../api/types'
import { Badge, Button, Card, Drawer, ErrorBanner, Field, Select, useToast } from '../../components/ui'
import { useAsync, useInterval } from '../../hooks/useAsync'
import { RetryFields, Toggle, useModelDraftGuard, zeroPolicy } from './ModelFields'

export function ModelHealth({ member }: { member?: LLMPoolMember }) {
  const state = member?.state ?? 'untested'
  const text = { untested: '暂无调用记录', ok: '最近调用成功', degraded: '调用异常', tripped: `已熔断 · ${Math.ceil((member?.cooldown_secs ?? 0) / 60)} 分钟`, missing_key: '未配置 Key', disabled: '已停用' }[state]
  return <Badge dot tone={state === 'ok' ? 'green' : state === 'tripped' || state === 'degraded' ? 'red' : 'slate'}>{text}</Badge>
}

export function PoolPanel({ settings, pool, reload, onClose, onSettings }: { settings: LLMSettings; pool?: LLMPoolStatus; reload: () => Promise<void>; onClose: () => void; onSettings: (s: LLMSettings) => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  useInterval(() => void reload(), 10000)
  const change = async (patch: Partial<LLMSettings>) => {
    setBusy(true); setError('')
    try { const saved = await api.saveLLMSettings({ ...settings, ...patch }); onSettings(saved); await reload() }
    catch (e) { setError(errorText(e)) } finally { setBusy(false) }
  }
  const reset = async (id = 0) => {
    setBusy(true); setError('')
    try { await api.resetLLMPool(id); await reload() } catch (e) { setError(errorText(e)) } finally { setBusy(false) }
  }
  return <Drawer open title="轮询配置 · 故障转移" width={512} onClose={() => { if (!busy) onClose() }}>
    <div className="grid gap-4 p-4">
      <p className="text-sm text-muted">当前模型不可用时，按顺序尝试其他配置。</p><ErrorBanner error={error} />
      <Toggle label="启用轮询" hint="关闭后，全局调用只使用激活模型。项目显式配额链仍按自身规则运行。" disabled={busy} value={settings.pool_enabled} onChange={(v) => void change({ pool_enabled: v })} />
      <Toggle label="Agent 绑定失败时兜底" hint="开启后，绑定配置失败时可回落到全局轮询链。项目配额链耗尽仍明确停止。" disabled={busy || !settings.pool_enabled} value={settings.bind_fallback} onChange={(v) => void change({ bind_fallback: v })} />
      <div className="flex items-center justify-between"><h3 className="text-sm font-medium">轮询顺序</h3><Button size="sm" disabled={busy} icon={<RotateCcw className="size-3.5" />} onClick={() => void reset()}>全部恢复</Button></div>
      {(pool?.chain ?? []).map((m) => <div key={m.profile_id} className={`rounded-[10px] border border-line p-3 ${m.excluded ? 'opacity-60' : ''}`}>
        <div className="flex flex-wrap items-center gap-2"><span className="font-mono text-xs text-muted">{m.order || '—'}</span><strong className="text-sm">{m.name}</strong>{m.active && <Badge tone="amber">激活</Badge>}<span className="ml-auto"><ModelHealth member={m} /></span></div>
        <p className="mt-1.5 break-all font-mono text-xs text-muted">{m.model}</p><p className="mt-1 text-xs text-muted">{m.excluded ? '不参与轮询' : m.active ? '固定第一顺位' : `优先级 ${m.priority}`}</p>
        {m.last_error && <p className="mt-2 break-words text-xs text-danger">{m.last_error}</p>}
        {(m.state === 'tripped' || m.state === 'degraded') && <Button size="sm" className="mt-2" disabled={busy} onClick={() => void reset(m.profile_id)}>立即恢复</Button>}
      </div>)}
      {!pool?.chain.length && <p className="py-4 text-center text-sm text-muted">暂无模型配置</p>}
      <p className="text-xs leading-relaxed text-muted">激活配置优先，其余按优先级降序，同优先级轮流使用。恢复只清除熔断记录，下一次调用仍会重新检查端点。</p>
    </div>
  </Drawer>
}

export function RetryPanel({ settings, onSaved, onDirty }: { settings: LLMSettings; onSaved: (s: LLMSettings) => void; onDirty: (v: boolean) => void }) {
  const [draft, setDraft] = useState(() => ({ ...zeroPolicy(), ...settings.retry }))
  const [base, setBase] = useState(() => JSON.stringify(draft))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const toast = useToast()
  const dirty = base !== JSON.stringify(draft)
  useModelDraftGuard(dirty, busy)
  useEffect(() => { onDirty(dirty); return () => onDirty(false) }, [dirty, onDirty])
  const save = async () => {
    setBusy(true); setError('')
    try { const s = await api.saveLLMSettings({ ...settings, retry: draft }); setBase(JSON.stringify(draft)); onSaved(s); toast('ok', '重试策略已保存') }
    catch (e) { setError(errorText(e)) } finally { setBusy(false) }
  }
  return <Card title="全局重试与退避" subtitle="单个模型可以覆盖建连、空响应与安全窗口重试" className="max-w-3xl">
    <fieldset disabled={busy}><RetryFields global value={draft} onChange={setDraft} /></fieldset><div className="mt-4"><ErrorBanner error={error} /></div>
    <div className="mt-4 flex justify-end gap-2"><Button disabled={!dirty || busy} onClick={() => setDraft(JSON.parse(base))}>取消修改</Button><Button variant="primary" icon={<Save className="size-4" />} loading={busy} disabled={!dirty} onClick={() => void save()}>保存策略</Button></div>
  </Card>
}

export function Bindings({ profiles }: { profiles: LLMProfile[] }) {
  const data = useAsync(() => api.llmBindings(), [])
  const [selected, setSelected] = useState<Record<string, number>>({})
  const [busy, setBusy] = useState('')
  const toast = useToast()
  const labels = { planner: 'Planner（主编审）', worker: 'Worker（执行单元）', judge: 'Judge（工具调用审查）' } as const
  const save = async (key: keyof typeof labels) => {
    setBusy(key)
    try { await api.bindAgent(key, selected[key] ?? data.data?.[key] ?? 0); await data.reload(); setSelected((s) => { const next = { ...s }; delete next[key]; return next }); toast('ok', '模型绑定已保存') }
    catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  return <Card title={<><Link2 className="size-4" /> Agent 模型绑定</>} subtitle="优先使用绑定模型；不绑定时使用项目配额链或全局激活模型。">
    <ErrorBanner error={data.error} onRetry={() => void data.reload()} />
    <div className="grid gap-3 lg:grid-cols-3">{(Object.keys(labels) as (keyof typeof labels)[]).map((key) => <Field key={key} label={labels[key]}>
      <div className="flex gap-2"><Select aria-label={labels[key]} value={selected[key] ?? data.data?.[key] ?? 0} disabled={!!busy || data.loading || !data.data} onChange={(e) => setSelected({ ...selected, [key]: Number(e.target.value) })}><option value={0}>不绑定</option>{profiles.map((p) => <option key={p.id} value={p.id} disabled={!p.enabled}>{p.name}{!p.enabled ? '（已停用）' : ''}</option>)}</Select><Button loading={busy === key} disabled={!!busy || !data.data} onClick={() => void save(key)}>应用</Button></div>
    </Field>)}</div>
  </Card>
}
