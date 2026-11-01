import { useCallback, useState } from 'react'
import { Plus, RefreshCw, Star, Trash2, Zap } from 'lucide-react'
import { api, errorText } from '../api/client'
import type { LLMProfile } from '../api/types'
import { PageFrame } from '../components/layout/AppShell'
import { Badge, Button, Empty, ErrorBanner, IconButton, Spinner, Tabs, useConfirm, useToast } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { useSearchParams } from '../lib/router'
import { ProfileEditor } from '../features/models/ProfileEditor'
import { Bindings, ModelHealth, PoolPanel, RetryPanel } from '../features/models/SettingsPanels'

type LLMTabKey = 'profiles' | 'retry'

export default function LLMPage() {
  const profiles = useAsync(() => api.listProfiles(), [])
  const settings = useAsync(() => api.llmSettings(), [])
  const pool = useAsync(() => api.llmPool(), [])
  const [params, setQuery] = useSearchParams()
  const [tab, setTab] = useState<LLMTabKey>('profiles')
  const [poolOpen, setPoolOpen] = useState(false)
  const [busy, setBusy] = useState(0)
  const [retryDirty, setRetryDirty] = useState(false)
  const confirm = useConfirm()
  const toast = useToast()
  const list = profiles.data ?? []
  const param = params.get('profile')
  const editing = list.find((p) => String(p.id) === param)
  const reload = useCallback(async () => { await Promise.all([profiles.reload(), settings.reload(), pool.reload()]) }, [profiles.reload, settings.reload, pool.reload])
  const changeTab = async (next: LLMTabKey) => {
    if (next === tab) return
    if (retryDirty && !(await confirm({ title: '放弃未保存的重试策略？', confirmLabel: '放弃修改', danger: true }))) return
    setRetryDirty(false)
    setTab(next)
  }
  const activate = async (p: LLMProfile) => {
    setBusy(p.id)
    try { await api.activateProfile(p.id); await reload(); toast('ok', `已激活：${p.name}`) } catch (e) { toast('error', errorText(e)) } finally { setBusy(0) }
  }
  const remove = async (p: LLMProfile) => {
    if (!(await confirm({ title: `删除模型配置「${p.name}」？`, message: '引用它的项目配额链及 Agent 绑定会移除。', confirmLabel: '删除', danger: true }))) return
    setBusy(p.id)
    try { await api.deleteProfile(p.id); await reload(); toast('ok', '模型配置已删除') } catch (e) { toast('error', errorText(e)) } finally { setBusy(0) }
  }
  return <PageFrame title="LLM" description="管理写作与核验 LLM 配置，星标为全局默认激活配置" actions={<>
    <IconButton label="刷新模型配置" disabled={retryDirty} onClick={() => void reload()}><RefreshCw className="size-4" /></IconButton>
    <Button icon={<Zap className="size-4" />} disabled={!settings.data || retryDirty} onClick={() => setPoolOpen(true)}>轮询配置{settings.data?.pool_enabled && <Badge tone="green">已开启</Badge>}</Button>
    <Button icon={<Plus className="size-4" />} disabled={retryDirty} onClick={() => setQuery({ profile: 'new' })}>新建</Button>
  </>}>
    <ErrorBanner error={profiles.error || settings.error || pool.error} onRetry={() => void reload()} />
    <Tabs
      tabs={[
        { key: 'profiles', label: '模型配置' },
        { key: 'retry', label: '重试与退避' },
      ]}
      value={tab}
      onChange={(v) => void changeTab(v)}
    />
    <div className="mt-5">
      {tab === 'profiles' ? <>
        {profiles.loading && !profiles.data ? <Spinner /> : !list.length ? <div className="rounded-[14px] border border-dashed border-line p-8"><Empty>还没有模型配置，点击右上角「新建」创建第一个。</Empty></div> : <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {list.map((p) => <article key={p.id} className={`flex min-w-0 flex-col rounded-[14px] border p-4 transition-colors ${p.is_default ? 'border-amber-400/60 bg-amber-400/5' : 'border-line bg-panel hover:border-fg/30'}`}>
            <button type="button" aria-label={`编辑 ${p.name}`} onClick={() => setQuery({ profile: p.id })} className="min-w-0 flex-1 text-left">
              <div className="flex items-start gap-2"><Star className={`mt-0.5 size-4 shrink-0 ${p.is_default ? 'fill-amber-400 text-amber-500' : 'text-muted'}`} /><div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><strong className="break-all text-sm">{p.name}</strong><span className="ml-auto"><ModelHealth member={pool.data?.chain.find((m) => m.profile_id === p.id)} /></span></div><p className="mt-2 truncate font-mono text-xs text-muted" title={p.model}>{p.model}</p></div></div>
              <div className="mt-3 flex flex-wrap gap-2 pl-6"><Badge>{p.format}</Badge>{!p.enabled && <Badge>已停用</Badge>}</div>
              <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1 pl-6 text-xs text-muted"><span>{p.api_key_hint || '未配置 Key'}</span><span>{p.rate_per_second ?? 0}/s · {p.rate_per_minute ?? 0}/min</span>{p.effort && <span>思考 {p.effort}</span>}{settings.data?.pool_enabled && !p.is_default && <span>{p.in_global_pool ? `优先级 ${p.priority}` : '不参与轮询'}</span>}</div>
            </button>
            <div className="mt-4 flex gap-2"><Button className="flex-1" disabled={!!busy || p.is_default || !p.enabled} loading={busy === p.id} onClick={() => void activate(p)}>{p.is_default ? '已激活' : '设为激活'}</Button><IconButton label={`删除 ${p.name}`} disabled={!!busy || p.is_default} onClick={() => void remove(p)}><Trash2 className="size-4 text-danger" /></IconButton></div>
          </article>)}
        </div>}
        <div className="mt-6"><Bindings key={list.map((p) => p.id).join(',')} profiles={list} /></div>
      </> : (
        settings.data ? <RetryPanel settings={settings.data} onSaved={(s) => { settings.setData(s); void pool.reload() }} onDirty={setRetryDirty} /> : <Spinner />
      )}
    </div>
    {(param === 'new' || editing) && <ProfileEditor key={param} profile={editing} onClose={() => setQuery({ profile: undefined })} onSaved={() => void reload()} />}
    {param && param !== 'new' && !editing && !profiles.loading && <ErrorBanner error="该模型配置不存在或已删除" onRetry={() => setQuery({ profile: undefined })} />}
    {poolOpen && settings.data && <PoolPanel settings={settings.data} pool={pool.data} reload={pool.reload} onSettings={settings.setData} onClose={() => setPoolOpen(false)} />}
  </PageFrame>
}
