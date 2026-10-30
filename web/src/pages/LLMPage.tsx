import { useEffect, useMemo, useState } from 'react'
import type { ChangeEvent } from 'react'
import { ChevronDown, ChevronRight, Link2, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { api, errorText } from '../api/client'
import type { LLMHealth, LLMProfile } from '../api/types'
import { PageFrame } from '../components/layout/AppShell'
import { Badge, Button, Card, Empty, ErrorBanner, Field, IconButton, Input, Select, Spinner, cx, useConfirm, useToast } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { fmtTime } from '../lib/format'
import { registerLeaveGuard, useSearchParams } from '../lib/router'

// 模型配置（图 3 / V03）：左侧 336px 配置列表、右侧编辑表单，间距 24px；保存在表单底部，切换配置前保护未保存修改。
// 仍编辑既有 LLMProfile 字段（环境变量名、模型池、优先级等）；密钥只从服务端环境变量读取，不在前端输入。

type Draft = Omit<LLMProfile, 'id'>

const blank: Draft = {
  name: '', format: 'anthropic', base_url: '', api_key_env: 'ANTHROPIC_API_KEY', model: '', max_tokens: 0,
  effort: '', thinking_display: '', priority: 0, rank: 0, window_tokens: 0, enabled: true, in_global_pool: true, proxy: '', session_header_key: '',
}

const toDraft = (p: LLMProfile | 'new' | undefined): Draft => {
  if (!p || p === 'new') return blank
  const { id: _id, ...rest } = p
  void _id
  return { ...blank, ...rest }
}

function HealthLine({ h }: { h?: LLMHealth }) {
  if (!h) return <span className="text-muted">暂无调用记录</span>
  if (h.open) return <span className="text-danger" title={h.last_error}>熔断至 {fmtTime(h.open_until)}</span>
  return <span className={h.fails ? 'text-warn' : 'text-ok'} title={h.last_error}>失败 {h.fails} · 熔断 {h.trips} 次</span>
}

function Bindings({ profiles }: { profiles: LLMProfile[] }) {
  const toast = useToast()
  const [sel, setSel] = useState<Record<string, string>>({ planner: '', worker: '', judge: '' })
  const [busy, setBusy] = useState('')
  const save = async (agent: 'planner' | 'worker' | 'judge') => {
    setBusy(agent)
    try {
      const r = await api.bindAgent(agent, Number(sel[agent]) || 0)
      toast('ok', r.profile_id ? `${labels[agent]} 已绑定到配置 #${r.profile_id}` : `${labels[agent]} 已解绑，回落到项目配额链 / 全局模型池`)
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setBusy('')
    }
  }
  const labels = { planner: 'Planner（主编审）', worker: 'Worker（执行单元）', judge: 'Judge（工具调用审查）' } as const
  return (
    <Card title={<><Link2 className="size-4" /> Agent 模型绑定</>} subtitle="为某类 Agent 指定专用模型配置；选择「不绑定」并应用即解绑。服务端暂无绑定查询接口，此处不回显当前绑定。">
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
  const confirm = useConfirm()
  const profiles = useAsync(() => api.listProfiles(), [])
  const health = useAsync(() => api.llmHealth(), [])
  const [params, setQuery] = useSearchParams()
  const list = useMemo(() => [...(profiles.data ?? [])].sort((a, b) => a.priority - b.priority || a.rank - b.rank || a.id - b.id), [profiles.data])
  const param = params.get('profile')
  const current: LLMProfile | 'new' | undefined = param === 'new' ? 'new' : list.find((p) => String(p.id) === param) ?? list[0]
  const currentKey = current === 'new' ? 'new' : current?.id
  const [d, setD] = useState<Draft>(blank)
  const [base, setBase] = useState<Draft>(blank)
  const [advanced, setAdvanced] = useState(false)
  const [busy, setBusy] = useState('')
  const [error, setError] = useState('')
  const dirty = JSON.stringify(d) !== JSON.stringify(base)

  useEffect(() => {
    const v = toDraft(current)
    setD(v); setBase(v); setError('')
  }, [currentKey, profiles.data]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (!dirty) return
    const off = registerLeaveGuard(async (to) => new URL(to, window.location.origin).pathname === window.location.pathname
      || confirm({ title: '离开前放弃未保存的模型配置？', message: '当前表单的修改尚未保存。', confirmLabel: '放弃修改', danger: true }))
    const onUnload = (e: BeforeUnloadEvent) => { e.preventDefault(); e.returnValue = '' }
    window.addEventListener('beforeunload', onUnload)
    return () => { off(); window.removeEventListener('beforeunload', onUnload) }
  }, [dirty, confirm])

  const choose = async (key: number | 'new') => {
    if (key === currentKey) return
    if (dirty && !(await confirm({ title: '切换前放弃未保存的修改？', message: `「${d.name || '新配置'}」的修改尚未保存。`, confirmLabel: '放弃并切换', danger: true }))) return
    setBase(d) // 避免守卫重复询问；随后 effect 会载入新配置
    setQuery({ profile: key })
  }
  const num = (k: keyof Draft) => (e: ChangeEvent<HTMLInputElement>) => setD({ ...d, [k]: Number(e.target.value) || 0 })
  const str = (k: keyof Draft) => (e: ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setD({ ...d, [k]: e.target.value })

  const save = async () => {
    setError('')
    if (!d.name.trim() || !d.model.trim()) { setError('名称与模型为必填项'); return }
    setBusy('save')
    try {
      const saved = current === 'new' ? await api.createProfile(d) : await api.updateProfile(current!.id, d)
      toast('ok', '模型配置已保存')
      setBase(d)
      await profiles.reload()
      setQuery({ profile: saved.id })
    } catch (e) { setError(errorText(e)) } finally { setBusy('') }
  }
  const remove = async () => {
    if (!current || current === 'new') return
    if (!(await confirm({ title: `删除模型配置「${current.name}」？`, message: '引用它的项目配额链条目会一并移除。', confirmLabel: '删除', danger: true }))) return
    setBusy('delete')
    try { await api.deleteProfile(current.id); toast('ok', '已删除'); setBase(d); setQuery({ profile: undefined }); void profiles.reload() }
    catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }
  const healthOf = (id: number) => health.data?.find((h) => h.profile_id === id)
  const editingName = current === 'new' ? '新配置' : current?.name

  return (
    <PageFrame title="模型配置" description="管理写作与核验所用的模型配置及配额链">
      <ErrorBanner error={profiles.error || health.error} onRetry={() => { void profiles.reload(); void health.reload() }} />
      <div className="grid items-start gap-6 lg:grid-cols-[336px_minmax(0,1fr)]">
        <section data-region="profile-list" className="rounded-[14px] border border-line bg-panel p-4">
          <div className="flex items-center justify-between gap-2">
            <h2 className="text-base font-semibold">配置列表</h2>
            <span className="flex items-center gap-1">
              <IconButton label="刷新配置与健康状态" onClick={() => { void profiles.reload(); void health.reload() }}><RefreshCw className="size-4" /></IconButton>
              <Button size="sm" icon={<Plus className="size-4" />} onClick={() => void choose('new')}>新建</Button>
            </span>
          </div>
          <p className="mt-2 text-xs text-muted">选择配置以查看和编辑</p>
          {profiles.loading && !profiles.data ? <Spinner /> : list.length === 0 && current !== 'new' ? <Empty>暂无模型配置，点击「新建」。</Empty> : (
            <ul>
              {current === 'new' && (
                <li className="mt-3 rounded-[10px] border border-fg bg-warm p-3 shadow-[0_0_0_1px_var(--color-fg)]"><strong>新配置</strong><p className="mt-1.5 text-xs text-muted">尚未保存</p></li>
              )}
              {list.map((p) => {
                const on = current !== 'new' && current?.id === p.id
                return (
                  <li key={p.id} className={cx('mt-3 rounded-[10px] border p-3', on ? 'border-fg bg-warm shadow-[0_0_0_1px_var(--color-fg)]' : 'border-line')}>
                    <div className="flex items-center justify-between gap-2">
                      <strong className="min-w-0 truncate" title={p.name}>{p.name}</strong>
                      {p.enabled ? <Badge dot tone="amber">已启用</Badge> : <Badge dot>已停用</Badge>}
                    </div>
                    <p className="my-1.5 truncate font-mono text-xs text-muted" title={p.model}>{p.model}</p>
                    <p className="my-1.5 text-xs text-muted">优先级 {p.priority} · {p.in_global_pool ? '全局模型池' : '不在全局模型池'}</p>
                    <p className="my-1.5 text-xs"><HealthLine h={healthOf(p.id)} /></p>
                    <Button size="sm" aria-pressed={on} onClick={() => void choose(p.id)}>{on ? '当前配置' : '选择配置'}</Button>
                  </li>
                )
              })}
            </ul>
          )}
          <p className="mt-4 text-xs text-muted">健康状态来自最近调用的熔断记录；无记录不代表配置可用。</p>
        </section>

        <section data-region="profile-form" className="rounded-[14px] border border-line bg-panel p-4">
          {!current ? <Empty>选择或新建一个配置</Empty> : (
            <form onSubmit={(e) => { e.preventDefault(); void save() }}>
              <div className="flex flex-wrap items-center gap-2">
                <h2 className="text-base font-semibold">编辑配置：{editingName}</h2>
                {current !== 'new' && (d.enabled ? <Badge dot tone="amber">已启用</Badge> : <Badge dot>已停用</Badge>)}
                {dirty && <Badge tone="blue">有未保存修改</Badge>}
              </div>
              <p className="mt-2 text-muted">保存后按现有模型池与项目配置链规则生效</p>
              <div className="mt-5 grid gap-4 md:grid-cols-2">
                <Field label="名称"><Input value={d.name} onChange={str('name')} required /></Field>
                <Field label="协议格式"><Select value={d.format} onChange={str('format')}><option value="anthropic">anthropic</option><option value="openai">openai</option></Select></Field>
                <Field label="模型" className="md:col-span-2"><Input value={d.model} onChange={str('model')} required /></Field>
                <Field label="Base URL（可选）" className="md:col-span-2" hint="留空使用该协议的默认端点"><Input value={d.base_url} onChange={str('base_url')} /></Field>
                <Field label="API Key 环境变量名" className="md:col-span-2" hint="只填写变量名；密钥由服务端读取。"><Input value={d.api_key_env} onChange={str('api_key_env')} /></Field>
                <Field label="优先级"><Input type="number" value={d.priority} onChange={num('priority')} /></Field>
                <Field label="同级顺序"><Input type="number" value={d.rank} onChange={num('rank')} /></Field>
                <Field label="最大输出 tokens"><Input type="number" value={d.max_tokens ?? 0} onChange={num('max_tokens')} /></Field>
                <Field label="上下文窗口 tokens"><Input type="number" value={d.window_tokens ?? 0} onChange={num('window_tokens')} /></Field>
                <div className="flex flex-wrap items-center gap-5 md:col-span-2">
                  <label className="flex items-center gap-2"><input type="checkbox" checked={d.enabled} onChange={(e) => setD({ ...d, enabled: e.target.checked })} />启用配置</label>
                  <label className="flex items-center gap-2"><input type="checkbox" checked={d.in_global_pool} onChange={(e) => setD({ ...d, in_global_pool: e.target.checked })} />加入全局模型池</label>
                </div>
                <div className="md:col-span-2">
                  <button type="button" aria-expanded={advanced} onClick={() => setAdvanced((v) => !v)} className="flex items-center gap-1 text-sm text-muted hover:text-fg">
                    {advanced ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}高级配置（effort、thinking、出站代理、会话亲和请求头）
                  </button>
                  {advanced && (
                    <div className="mt-3 grid gap-4 md:grid-cols-2">
                      <Field label="effort"><Input value={d.effort} onChange={str('effort')} placeholder="low / medium / high" /></Field>
                      <Field label="thinking_display"><Input value={d.thinking_display} onChange={str('thinking_display')} /></Field>
                      <Field label="出站代理" hint="留空沿用环境代理"><Input value={d.proxy} onChange={str('proxy')} placeholder="http://127.0.0.1:7890" /></Field>
                      <Field label="会话亲和请求头"><Input value={d.session_header_key} onChange={str('session_header_key')} /></Field>
                    </div>
                  )}
                </div>
              </div>
              <div className="mt-3"><ErrorBanner error={error} /></div>
              <div className="mt-6 flex flex-wrap items-center gap-2">
                {current !== 'new' && <Button variant="ghost" className="text-danger" icon={<Trash2 className="size-4" />} loading={busy === 'delete'} onClick={() => void remove()}>删除配置</Button>}
                <span className="flex-1" />
                <Button disabled={!dirty} onClick={() => { setD(base); setError('') }}>取消</Button>
                <Button type="submit" variant="primary" loading={busy === 'save'} disabled={!dirty && current !== 'new'}>保存配置</Button>
              </div>
            </form>
          )}
        </section>
      </div>
      <div className="mt-6"><Bindings profiles={list} /></div>
    </PageFrame>
  )
}
