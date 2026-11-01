import { useEffect, useRef } from 'react'
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from '../../api/types'
import { Field, Input, useConfirm } from '../../components/ui'
import { registerLeaveGuard } from '../../lib/router'

export const zeroRule = (): LLMRetryRule => ({ attempts: 0, interval_ms: 0 })
export const zeroRetry = (): LLMRetryOverride => ({ connect: zeroRule(), empty: zeroRule(), stream: zeroRule() })
export const zeroPolicy = (): LLMRetryPolicy => ({ ...zeroRetry(), breaker: zeroRule(), intent: zeroRule() })

export function Toggle({ label, hint, value, onChange, disabled }: { label: string; hint?: string; value: boolean; onChange: (v: boolean) => void; disabled?: boolean }) {
  return <label className="flex items-center justify-between gap-4 rounded-[10px] border border-line p-3">
    <span><span className="block text-sm font-medium">{label}</span>{hint && <span className="mt-1 block text-xs leading-relaxed text-muted">{hint}</span>}</span>
    <span className="relative shrink-0">
      <input type="checkbox" role="switch" aria-label={label} checked={value} disabled={disabled} onChange={(e) => onChange(e.target.checked)} className="peer absolute inset-0 z-10 m-0 h-full w-full cursor-pointer opacity-0 disabled:cursor-not-allowed" />
      <span aria-hidden="true" className="block h-5 w-9 rounded-full bg-muted/30 transition-colors peer-checked:bg-primary peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-primary peer-disabled:opacity-40" />
      <span aria-hidden="true" className="pointer-events-none absolute left-0.5 top-0.5 size-4 rounded-full bg-white shadow-sm transition-transform peer-checked:translate-x-4" />
    </span>
  </label>
}

const layers = {
  connect: ['建连重试', '连接超时、HTTP 408 / 429 / 5xx；默认 3 次。'],
  empty: ['空响应重试', 'Chat Completions 正常结束但没有内容；默认 2 次。'],
  stream: ['安全窗口重试', '尚未交付正文、思考或工具调用时重试；默认 2 次。'],
  breaker: ['轮询熔断', '瞬时错误连续 3 次熔断；冷却默认 1 / 5 / 30 分钟。Key 失效、余额不足等立即熔断。'],
  intent: ['Worker 续跑', '模型错误后沿同一会话续跑；默认 2 次，间隔 3 秒。'],
} as const

export function RetryFields({ value, onChange, global = false }: { value: LLMRetryPolicy | LLMRetryOverride; onChange: (v: LLMRetryPolicy) => void; global?: boolean }) {
  const keys: (keyof LLMRetryPolicy)[] = global ? ['connect', 'empty', 'stream', 'breaker', 'intent'] : ['connect', 'empty', 'stream']
  return <div className="grid gap-3">
    <p className="text-xs leading-relaxed text-muted">次数：0 = {global ? '内置默认' : '跟随全局'}，-1 = 关闭，正数 = 重试次数。间隔单位毫秒，0 = {global ? '默认退避' : '跟随全局'}。已交付的模型输出不会原地重放。</p>
    {keys.map((key) => {
      const rule = (value as LLMRetryPolicy)[key] ?? zeroRule()
      const set = (field: keyof LLMRetryRule, n: number) => onChange({ ...zeroPolicy(), ...value, [key]: { ...rule, [field]: n } })
      return <section key={key} className="rounded-[10px] border border-line p-3">
        <h3 className="text-sm font-medium">{layers[key][0]}</h3><p className="mt-1 text-xs text-muted">{layers[key][1]}</p>
        <div className="mt-3 grid grid-cols-2 gap-3">
          <Field label={key === 'breaker' ? '连续失败阈值' : '重试次数'}><Input aria-label={`${layers[key][0]}次数`} type="number" min={-1} max={20} value={rule.attempts} onChange={(e) => set('attempts', Number(e.target.value))} /></Field>
          <Field label={key === 'breaker' ? '冷却时间（ms）' : '间隔（ms）'}><Input aria-label={`${layers[key][0]}间隔`} type="number" min={0} max={3600000} value={rule.interval_ms} onChange={(e) => set('interval_ms', Number(e.target.value))} /></Field>
        </div>
      </section>
    })}
  </div>
}

export function useModelDraftGuard(dirty: boolean, busy = false) {
  const confirm = useConfirm()
  const bypass = useRef(false)
  const mayLeave = async () => bypass.current || (!busy && (!dirty || await confirm({ title: '放弃未保存的设置？', message: '当前修改尚未保存。', confirmLabel: '放弃修改', danger: true })))
  useEffect(() => {
    if (!dirty && !busy) return
    const off = registerLeaveGuard(mayLeave)
    const unload = (e: BeforeUnloadEvent) => { if (!bypass.current) { e.preventDefault(); e.returnValue = '' } }
    window.addEventListener('beforeunload', unload)
    return () => { off(); window.removeEventListener('beforeunload', unload) }
  }, [dirty, busy, confirm]) // eslint-disable-line react-hooks/exhaustive-deps
  return { mayLeave, bypass }
}
