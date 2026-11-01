import { useRef, useState } from 'react'
import { PlugZap, Save } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { LLMProfile, LLMProfileInput, LLMTestResult } from '../../api/types'
import { Badge, Button, Drawer, ErrorBanner, Field, Input, Select, useToast } from '../../components/ui'
import { RetryFields, Toggle, useModelDraftGuard, zeroRetry } from './ModelFields'
import { ModelPicker } from './ModelPicker'

const draftOf = (p?: LLMProfile): LLMProfileInput => ({
  name: p?.name ?? '', format: p?.format ?? 'anthropic', model: p?.model ?? '', base_url: p?.base_url ?? '',
  api_key_env: p?.api_key_env ?? '', api_key: '', clear_api_key: false, proxy: p?.proxy ?? '', session_header_key: p?.session_header_key ?? '',
  enabled: p?.enabled ?? true, in_global_pool: p?.in_global_pool ?? true, rank: p?.rank ?? 0, priority: p?.priority ?? 0,
  max_tokens: p?.max_tokens ?? 0, window_tokens: p?.window_tokens ?? 0, effort: p?.effort ?? '', thinking_display: p?.thinking_display ?? '',
  thinking_type: p?.thinking_type ?? '', streaming: p?.streaming ?? true, max_tokens_field: p?.max_tokens_field ?? '',
  rate_per_second: p?.rate_per_second ?? 0, rate_per_minute: p?.rate_per_minute ?? 0, retry: p?.retry ?? zeroRetry(),
})

export function ProfileEditor({ profile, onClose, onSaved }: { profile?: LLMProfile; onClose: () => void; onSaved: () => void }) {
  const [draft, setDraft] = useState(() => draftOf(profile))
  const base = useRef(JSON.stringify(draft))
  const [busy, setBusy] = useState('')
  const [error, setError] = useState('')
  const [discoveryRevision, setDiscoveryRevision] = useState(0)
  const [test, setTest] = useState<LLMTestResult>()
  const [advanced, setAdvanced] = useState(false)
  const toast = useToast()
  const dirty = base.current !== JSON.stringify(draft)
  const guard = useModelDraftGuard(dirty, !!busy)
  const patch = (p: Partial<LLMProfileInput>) => {
    setDraft((d) => ({ ...d, ...p })); setTest(undefined)
    if (['format', 'base_url', 'api_key', 'api_key_env', 'clear_api_key', 'proxy'].some((key) => key in p)) setDiscoveryRevision((v) => v + 1)
  }
  const str = (key: keyof LLMProfileInput) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => patch({ [key]: e.target.value })
  const num = (key: keyof LLMProfileInput) => (e: React.ChangeEvent<HTMLInputElement>) => patch({ [key]: Number(e.target.value) })
  const close = async () => { if (await guard.mayLeave()) { guard.bypass.current = true; onClose() } }
  const payload = { ...draft, profile_id: profile?.id }

  const save = async () => {
    if (!draft.name?.trim() || !draft.model?.trim()) { setError('名称与模型为必填项'); return }
    setBusy('save'); setError('')
    try {
      if (profile) await api.updateProfile(profile.id, draft)
      else await api.createProfile(draft)
      guard.bypass.current = true
      toast('ok', profile ? '配置已保存，下次调用生效' : '配置已创建，请在卡片上设为激活')
      onSaved(); onClose()
    } catch (e) { setError(errorText(e)) } finally { setBusy('') }
  }
  const discover = async () => {
    setBusy('models')
    try { const r = await api.discoverModels(payload); if (!r.ok) throw new Error(r.error); return r.models ?? [] }
    finally { setBusy('') }
  }
  const connect = async () => {
    if (!draft.model?.trim()) { setError('请先填写模型'); return }
    setBusy('test'); setError(''); setTest(undefined)
    try { setTest(await api.testLLM(payload)) } catch (e) { setError(errorText(e)) } finally { setBusy('') }
  }

  return <Drawer open title={<>{profile ? `编辑：${profile.name}` : '新建模型配置'} {profile?.is_default && <Badge tone="amber">激活中</Badge>}</>} width={576} onClose={() => void close()}
    footer={<><Button icon={<PlugZap className="size-4" />} loading={busy === 'test'} disabled={!!busy} onClick={() => void connect()}>测试连接</Button><Button icon={<Save className="size-4" />} variant="primary" loading={busy === 'save'} disabled={!!busy || (!!profile && !dirty)} onClick={() => void save()}>{profile ? '保存配置' : '新建配置'}</Button></>}>
    <fieldset disabled={!!busy} className="grid min-w-0 gap-4 p-4">
      <p className="text-xs text-muted">{profile ? '保存后对后续模型调用生效。' : '新建后，在模型卡片上设为激活。'}</p>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <Field label="名称"><Input aria-label="名称" value={draft.name} onChange={str('name')} placeholder="例如：写作主模型" /></Field>
        <Field label="协议格式"><Select aria-label="协议格式" value={draft.format} onChange={(e) => patch({ format: e.target.value, max_tokens_field: '' })}><option value="anthropic">Anthropic</option><option value="openai">OpenAI · Chat Completions</option><option value="openai-responses">OpenAI · Responses API</option></Select></Field>
      </div>
      <ModelPicker key={discoveryRevision} value={draft.model ?? ''} onChange={(model) => patch({ model })} onRefresh={discover} loading={busy === 'models'} disabled={!!busy} />
      <Field label="Base URL（可选）" hint="留空使用协议默认地址，也可填写完整接口地址"><Input aria-label="Base URL" value={draft.base_url} onChange={str('base_url')} placeholder="https://api.example.com/v1" /></Field>
      <Field label="API Key" hint={profile?.api_key_hint ? `已配置：${profile.api_key_hint}；留空保留` : '密钥由服务端加密保存'}><Input aria-label="API Key" type="password" autoComplete="new-password" value={draft.api_key} onChange={str('api_key')} placeholder={profile?.key_configured ? '留空保持不变' : '输入 API Key'} /></Field>
      {profile?.api_key_hint && profile.api_key_hint !== '环境变量已配置' && <label className="flex items-center gap-2 text-xs text-muted"><input type="checkbox" checked={draft.clear_api_key} onChange={(e) => patch({ clear_api_key: e.target.checked })} />清除已保存的 Key（如配置了环境变量，将使用环境变量）</label>}
      <div className="grid grid-cols-3 gap-3">
        <Field label="每秒限速"><Input aria-label="每秒限速" type="number" min={0} step="any" value={draft.rate_per_second} onChange={num('rate_per_second')} /></Field>
        <Field label="每分钟限速"><Input aria-label="每分钟限速" type="number" min={0} step="any" value={draft.rate_per_minute} onChange={num('rate_per_minute')} /></Field>
        <Field label="上下文窗口（K）"><Input aria-label="上下文窗口（K）" type="number" min={0} max={1000} value={(draft.window_tokens ?? 0) / 1000} onChange={(e) => patch({ window_tokens: Math.round(Number(e.target.value) * 1000) })} /></Field>
      </div>
      <p className="-mt-2 text-xs text-muted">限速 0 = 不限，同一配置的 Agent 共享。窗口 0 = 200K，可设置 32–1000K。</p>
      <Toggle label="流式输出" hint="关闭后等待完整回复，适用于 SSE 不稳定的网关。" value={draft.streaming ?? true} onChange={(v) => patch({ streaming: v })} />
      <div className="grid gap-3 rounded-[10px] border border-line p-3">
        <Field label="最大输出 tokens" hint="0 = 协议默认值；与上下文窗口独立"><Input aria-label="最大输出 tokens" type="number" min={0} value={draft.max_tokens} onChange={num('max_tokens')} /></Field>
        <Field label="输出上限字段"><Select aria-label="输出上限字段" value={draft.max_tokens_field} disabled={draft.format !== 'openai'} onChange={str('max_tokens_field')}><option value="">{draft.format === 'openai-responses' ? 'max_output_tokens（固定）' : 'max_tokens（默认）'}</option>{draft.format === 'openai' && <option value="max_completion_tokens">max_completion_tokens</option>}</Select></Field>
      </div>
      <div className="grid grid-cols-2 gap-3">
        <Field label="思考开关"><Select aria-label="思考开关" value={draft.thinking_type} onChange={str('thinking_type')}><option value="">不发送（默认）</option><option value="disabled">关闭</option><option value="enabled">开启</option></Select></Field>
        <Field label="思考强度"><Select aria-label="思考强度" value={draft.effort} onChange={str('effort')}><option value="">不发送（默认）</option>{['low', 'medium', 'high', 'xhigh', 'max'].map((v) => <option key={v}>{v}</option>)}</Select></Field>
      </div>
      <p className="-mt-2 text-xs text-muted">思考开关与强度独立，按端点支持情况选择，可通过测试连接验证。</p>
      <Field label="轮询优先级" hint="数字越大越优先；激活模型始终第一，同优先级轮转"><Input aria-label="轮询优先级" type="number" value={draft.priority} onChange={num('priority')} /></Field>
      <Toggle label="参与全局轮询" hint="关闭后仍可供 Agent 或项目指定使用。" value={draft.in_global_pool ?? true} onChange={(v) => patch({ in_global_pool: v })} />
      <Toggle label="启用配置" disabled={profile?.is_default} value={draft.enabled ?? true} onChange={(v) => patch({ enabled: v })} />
      <button type="button" className="text-left text-sm font-medium" aria-expanded={advanced} onClick={() => setAdvanced(!advanced)}>{advanced ? '收起高级配置' : '高级配置：代理、环境变量、会话头与重试'}</button>
      {advanced && <div className="grid gap-4">
        <Field label="代理（可选）" hint="支持 http / https / socks5；留空沿用服务端环境代理"><Input aria-label="代理" value={draft.proxy} onChange={str('proxy')} /></Field>
        <Field label="API Key 环境变量名" hint="未保存 Key 时使用；留空按协议使用 ANTHROPIC_API_KEY 或 OPENAI_API_KEY"><Input aria-label="API Key 环境变量名" value={draft.api_key_env} onChange={str('api_key_env')} /></Field>
        <Field label="自定义会话头" hint="头值自动填入当前会话 ID"><Input aria-label="自定义会话头" value={draft.session_header_key} onChange={str('session_header_key')} /></Field>
        {draft.format === 'anthropic' && <Field label="自适应思考展示（兼容旧配置）"><Select value={draft.thinking_display} onChange={str('thinking_display')}><option value="">默认</option><option value="summarized">summarized</option><option value="omitted">omitted</option></Select></Field>}
        <h3 className="text-sm font-medium">当前模型重试覆盖</h3><RetryFields value={draft.retry ?? zeroRetry()} onChange={({ connect, empty, stream }) => patch({ retry: { connect, empty, stream } })} />
      </div>}
    </fieldset>
    <div className="px-4 pb-4"><ErrorBanner error={error} />{test && <div role="status" className={`rounded-[10px] border p-3 text-sm ${test.ok ? 'border-ok text-ok' : 'border-danger text-danger'}`}><p>{test.ok ? `连接成功 · ${test.latency_ms} ms · ${test.model}` : `连接失败：${test.error}`}</p>{test.reply && <p className="mt-2 break-words text-fg">回复：{test.reply}</p>}</div>}</div>
  </Drawer>
}
