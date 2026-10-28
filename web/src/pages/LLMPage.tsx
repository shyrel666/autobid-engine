import { useEffect, useState } from 'react'
import type { ChangeEvent } from 'react'
import { Link2, Pencil, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { api, errorText } from '../api/client'
import type { LLMProfile } from '../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Modal, Select, Spinner, useToast } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { fmtTime } from '../lib/format'

type Draft = Omit<LLMProfile, 'id'>

const blank: Draft = {
  name: '', format: 'anthropic', base_url: '', api_key_env: 'ANTHROPIC_API_KEY', model: 'claude-opus-5-5', max_tokens: 0,
  effort: '', thinking_display: '', priority: 0, rank: 0, window_tokens: 0, enabled: true, in_global_pool: true, proxy: '', session_header_key: '',
}

function ProfileDialog({ editing, onClose, onSaved }: { editing: LLMProfile | 'new' | null; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const [d, setD] = useState<Draft>(blank)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    if (!editing) return
    setError('')
    if (editing === 'new') setD(blank)
    else { const { id: _id, ...rest } = editing; void _id; setD({ ...blank, ...rest }) }
  }, [editing])
  const num = (k: keyof Draft) => (e: ChangeEvent<HTMLInputElement>) => setD({ ...d, [k]: Number(e.target.value) || 0 })
  const str = (k: keyof Draft) => (e: ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setD({ ...d, [k]: e.target.value })

  const submit = async () => {
    if (!d.name.trim() || !d.model.trim()) { setError('名称与模型为必填项'); return }
    setBusy(true)
    try {
      if (editing === 'new') await api.createProfile(d)
      else if (editing) await api.updateProfile(editing.id, d)
      toast('ok', '模型配置已保存')
      onSaved()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal open={editing !== null} wide title={editing === 'new' ? '新增模型配置' : `编辑模型配置 #${editing?.id ?? ''}`} onClose={onClose}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={submit}>保存</Button></>}>
      <div className="grid gap-3 md:grid-cols-3">
        <Field label="名称 *"><Input value={d.name} onChange={str('name')} /></Field>
        <Field label="协议格式"><Select value={d.format} onChange={str('format')}><option value="anthropic">anthropic</option><option value="openai">openai</option></Select></Field>
        <Field label="模型 *"><Input value={d.model} onChange={str('model')} /></Field>
        <Field label="Base URL" className="md:col-span-2" hint="留空使用官方端点"><Input value={d.base_url} onChange={str('base_url')} placeholder="https://api.anthropic.com" /></Field>
        <Field label="API Key 环境变量名" hint="密钥只从服务端环境变量读取，不经过前端"><Input value={d.api_key_env} onChange={str('api_key_env')} /></Field>
        <Field label="max_tokens"><Input type="number" value={d.max_tokens ?? 0} onChange={num('max_tokens')} /></Field>
        <Field label="effort"><Input value={d.effort} onChange={str('effort')} placeholder="low / medium / high" /></Field>
        <Field label="thinking_display"><Input value={d.thinking_display} onChange={str('thinking_display')} /></Field>
        <Field label="priority"><Input type="number" value={d.priority} onChange={num('priority')} /></Field>
        <Field label="rank"><Input type="number" value={d.rank} onChange={num('rank')} /></Field>
        <Field label="上下文窗口 tokens"><Input type="number" value={d.window_tokens ?? 0} onChange={num('window_tokens')} /></Field>
        <Field label="出站代理" className="md:col-span-2" hint="留空沿用环境代理"><Input value={d.proxy} onChange={str('proxy')} placeholder="http://127.0.0.1:7890" /></Field>
        <Field label="会话亲和请求头"><Input value={d.session_header_key} onChange={str('session_header_key')} /></Field>
        <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={d.enabled} onChange={(e) => setD({ ...d, enabled: e.target.checked })} />启用</label>
        <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={d.in_global_pool} onChange={(e) => setD({ ...d, in_global_pool: e.target.checked })} />加入全局池</label>
      </div>
      <div className="mt-3"><ErrorBanner error={error} /></div>
    </Modal>
  )
}

function Bindings({ profiles }: { profiles: LLMProfile[] }) {
  const toast = useToast()
  const [sel, setSel] = useState<Record<string, string>>({ planner: '', worker: '', judge: '' })
  const [busy, setBusy] = useState('')
  const save = async (agent: 'planner' | 'worker' | 'judge') => {
    setBusy(agent)
    try {
      const r = await api.bindAgent(agent, Number(sel[agent]) || 0)
      toast('ok', r.profile_id ? `${agent} 已绑定到配置 #${r.profile_id}` : `${agent} 已解绑，回落到任务配额链/全局池`)
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setBusy('')
    }
  }
  const labels = { planner: 'Planner（主编审）', worker: 'Worker（执行单元）', judge: 'Judge（拦截审查）' } as const
  return (
    <Card title={<><Link2 className="size-4" /> Agent 模型绑定</>}>
      <p className="mb-3 text-xs text-slate-500">PUT /api/llm/bindings/&#123;agent&#125;：为某类 Agent 指定专用模型配置；选择“不绑定”提交即解绑。服务端暂无绑定查询接口，此处不回显当前绑定。</p>
      <div className="grid gap-3 md:grid-cols-3">
        {(Object.keys(labels) as (keyof typeof labels)[]).map((a) => (
          <Field key={a} label={labels[a]}>
            <div className="flex gap-2">
              <Select value={sel[a]} onChange={(e) => setSel({ ...sel, [a]: e.target.value })}>
                <option value="">不绑定</option>
                {profiles.map((p) => <option key={p.id} value={p.id}>{p.name}（{p.model}）</option>)}
              </Select>
              <Button loading={busy === a} onClick={() => save(a)}>应用</Button>
            </div>
          </Field>
        ))}
      </div>
    </Card>
  )
}

export default function LLMPage() {
  const toast = useToast()
  const profiles = useAsync(() => api.listProfiles(), [])
  const health = useAsync(() => api.llmHealth(), [])
  const [editing, setEditing] = useState<LLMProfile | 'new' | null>(null)
  const list = profiles.data ?? []
  const healthOf = (id: number) => health.data?.find((h) => h.profile_id === id)

  const remove = async (p: LLMProfile) => {
    if (!confirm(`确认删除模型配置「${p.name}」？引用它的任务配额链条目会一并移除。`)) return
    try { await api.deleteProfile(p.id); toast('ok', '已删除'); void profiles.reload() } catch (e) { toast('error', errorText(e)) }
  }

  return (
    <div className="mx-auto max-w-6xl space-y-4 p-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">LLM 配置</h1>
          <p className="text-sm text-slate-500">模型配置、全局池、熔断健康状态与 Agent 绑定。任务级配额链与用量见任务工作区的“LLM 用量”页。</p>
        </div>
        <div className="flex gap-2">
          <Button icon={<RefreshCw className="size-4" />} onClick={() => { void profiles.reload(); void health.reload() }}>刷新</Button>
          <Button variant="primary" icon={<Plus className="size-4" />} onClick={() => setEditing('new')}>新增配置</Button>
        </div>
      </div>
      <ErrorBanner error={profiles.error || health.error} onRetry={() => { void profiles.reload(); void health.reload() }} />
      <Card bodyClassName="p-0">
        {profiles.loading && !profiles.data ? <Spinner /> : list.length === 0 ? <Empty>暂无模型配置</Empty> : (
          <table className="w-full text-sm">
            <thead className="bg-slate-50 text-left text-xs text-slate-500">
              <tr><th className="px-3 py-2">#</th><th className="px-3 py-2">名称</th><th className="px-3 py-2">模型</th><th className="px-3 py-2">密钥变量</th><th className="px-3 py-2">状态</th><th className="px-3 py-2">健康</th><th className="px-3 py-2" /></tr>
            </thead>
            <tbody>
              {list.map((p) => {
                const h = healthOf(p.id)
                return (
                  <tr key={p.id} className="border-t border-slate-100 align-top">
                    <td className="px-3 py-2 text-xs text-slate-400">{p.id}</td>
                    <td className="px-3 py-2 font-medium">{p.name}<div className="text-xs font-normal text-slate-400">{p.base_url || '官方端点'} · P{p.priority}/R{p.rank}</div></td>
                    <td className="px-3 py-2"><Badge>{p.format}</Badge> <span className="font-mono text-xs">{p.model}</span></td>
                    <td className="px-3 py-2 font-mono text-xs">{p.api_key_env}</td>
                    <td className="px-3 py-2 space-x-1">
                      {p.enabled ? <Badge tone="green">启用</Badge> : <Badge>停用</Badge>}
                      {p.in_global_pool && <Badge tone="blue">全局池</Badge>}
                    </td>
                    <td className="px-3 py-2 text-xs">
                      {!h ? <span className="text-slate-400">无记录</span> : h.open ? (
                        <span className="text-red-600" title={h.last_error}>熔断至 {fmtTime(h.open_until)}</span>
                      ) : (
                        <span className={h.fails ? 'text-amber-600' : 'text-emerald-600'} title={h.last_error}>失败 {h.fails} · 熔断 {h.trips} 次</span>
                      )}
                      {h?.last_error && <div className="max-w-xs truncate text-slate-400" title={h.last_error}>{h.last_error}</div>}
                    </td>
                    <td className="px-3 py-2">
                      <div className="flex justify-end gap-1">
                        <Button size="sm" variant="ghost" onClick={() => setEditing(p)}><Pencil className="size-3.5" /></Button>
                        <Button size="sm" variant="ghost" onClick={() => remove(p)}><Trash2 className="size-3.5 text-red-500" /></Button>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </Card>
      <Bindings profiles={list} />
      <ProfileDialog editing={editing} onClose={() => setEditing(null)} onSaved={() => { setEditing(null); void profiles.reload() }} />
    </div>
  )
}
