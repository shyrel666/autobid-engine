// 本机运行能力状态（GET /api/runtime/tools）：顶栏汇总指示器与入口处的能力提示。
// 状态由服务端功能自检得出（不是“程序已安装”）；原因已脱敏。自检进行中时每 4 秒重读，结束后停止。

import { useEffect, useRef, useState } from 'react'
import { Cpu, RefreshCw } from 'lucide-react'
import { api } from '../../api/client'
import type { RuntimeCapability, RuntimeCapabilityStatus } from '../../api/types'
import { Badge, IconButton, Notice, cx } from '../../components/ui'
import { useAsync, useInterval } from '../../hooks/useAsync'
import { fmtTime } from '../../lib/format'
import { runtimeNeedsPoll, runtimeStateLabel, runtimeStateTone, summarizeRuntime } from './runtime-status'

export function useRuntimeTools() {
  const state = useAsync(() => api.runtimeTools().then((r) => r.capabilities), [])
  useInterval(() => { void state.reload() }, runtimeNeedsPoll(state.data) ? 4000 : null)
  return state
}

function CapabilityRow({ c }: { c: RuntimeCapabilityStatus }) {
  return (
    <li className="space-y-0.5 border-t border-line py-2 first:border-t-0">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium">{c.label}</span>
        <Badge tone={runtimeStateTone(c.state)} dot>{runtimeStateLabel[c.state] ?? c.state}</Badge>
        {c.version && <span className="text-xs text-muted">{c.version}</span>}
      </div>
      {c.state !== 'healthy' && c.impact && <p className="text-xs text-fg">{c.impact}</p>}
      {c.reason && <p className="break-all text-xs text-muted">原因：{c.reason}</p>}
      {c.state !== 'healthy' && c.next_action && <p className="text-xs text-link">下一步：{c.next_action}</p>}
      {c.checked_at && <p className="text-[11px] text-muted">自检于 {fmtTime(c.checked_at)}{c.duration_ms ? `，用时 ${(c.duration_ms / 1000).toFixed(1)} 秒` : ''}
        {c.failed_at ? `；最近失败 ${fmtTime(c.failed_at)}` : ''}</p>}
    </li>
  )
}

const toneClass: Record<string, string> = {
  green: 'border-line bg-panel text-ok',
  blue: 'border-line bg-panel text-link',
  amber: 'border-[#fde68a] bg-[#fffbeb] text-warn',
  red: 'border-danger/50 bg-[#fef2f2] text-danger',
  slate: 'border-line bg-panel text-muted',
}

/** 顶栏运行能力指示器：汇总文档引擎 / 图表渲染 / OCR，展开后列出全部能力的状态、影响与下一步。 */
export function RuntimeIndicator() {
  const rt = useRuntimeTools()
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  const sum = summarizeRuntime(rt.data)
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false) }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    window.addEventListener('mousedown', onDown)
    window.addEventListener('keydown', onKey)
    return () => { window.removeEventListener('mousedown', onDown); window.removeEventListener('keydown', onKey) }
  }, [open])
  const label = rt.error && !rt.data ? '运行能力读取失败' : sum.label
  return (
    <div ref={ref} className="relative">
      <button type="button" aria-label={`本机运行能力：${label}`} title={label} aria-expanded={open}
        onClick={() => { setOpen(!open); if (!open) void rt.reload() }}
        className={cx('inline-flex h-7 items-center gap-1.5 rounded-lg border px-2 text-[13px] font-medium hover:bg-soft', toneClass[rt.error && !rt.data ? 'red' : sum.tone])}>
        <Cpu className="size-3.5 shrink-0" aria-hidden />
        <span className="hidden max-w-36 truncate lg:inline">{label}</span>
      </button>
      {open && (
        <div role="dialog" aria-label="本机运行能力" className="absolute right-0 z-50 mt-1.5 w-[min(28rem,calc(100vw-2rem))] rounded-[12px] border border-line bg-panel p-4 shadow-xl">
          <div className="mb-2 flex items-start gap-2">
            <div className="flex-1">
              <div className="text-sm font-semibold">本机运行能力</div>
              <p className="mt-0.5 text-xs text-muted">服务启动时用固定小样本实际自检；状态由服务器给出，与 Agent 看到的一致。</p>
            </div>
            <IconButton label="重新读取运行能力" onClick={() => void rt.reload()}><RefreshCw className={cx('size-4', rt.loading && 'animate-spin')} /></IconButton>
          </div>
          {rt.error && <p className="mb-2 text-xs text-danger">读取失败：{rt.error}</p>}
          <ul className="max-h-[60vh] overflow-y-auto">{(rt.data ?? []).map((c) => <CapabilityRow key={c.capability} c={c} />)}</ul>
          <VersionDetails />
        </div>
      )}
    </div>
  )
}

function VersionDetails() {
  const version = useAsync(() => api.runtimeVersion(), [])
  return <details className="mt-2 border-t border-line pt-2 text-xs text-muted">
    <summary className="cursor-pointer">诊断详情</summary>
    {version.error && <p>版本信息读取失败：{version.error}</p>}
    {version.data && <dl className="mt-2 space-y-1 break-all">
      <div><dt className="inline">运行版本：</dt><dd className="inline">{version.data.revision}{version.data.modified ? '（含未提交改动）' : ''}</dd></div>
      <div><dt className="inline">Go 版本：</dt><dd className="inline">{version.data.go_version}</dd></div>
      <div><dt className="inline">数据库校验和：</dt><dd className="inline">{version.data.schema_checksum}</dd></div>
      <div><dt className="inline">启动时间：</dt><dd className="inline">{fmtTime(version.data.started_at)}</dd></div>
    </dl>}
  </details>
}

/** 入口处的能力提示：只在相关能力不是 healthy 时显示影响与下一步（检查中、未配置、自检失败、最近失败分别可见）。 */
export function RuntimeNotice({ capabilities, className }: { capabilities: RuntimeCapability[]; className?: string }) {
  const rt = useRuntimeTools()
  const list = (rt.data ?? []).filter((c) => capabilities.includes(c.capability) && c.state !== 'healthy')
  if (!list.length) return null
  const tone = list.some((c) => c.state === 'self_test_failed' || c.state === 'degraded') ? 'danger'
    : list.every((c) => c.state === 'starting') ? 'info' : 'warn'
  return (
    <Notice tone={tone} className={className}>
      <ul className="space-y-1.5">
        {list.map((c) => (
          <li key={c.capability}>
            <span className="font-medium">{c.label}：{runtimeStateLabel[c.state] ?? c.state}</span>
            {c.impact && <span>。{c.impact}</span>}
            {c.next_action && <span className="text-muted"> 下一步：{c.next_action}</span>}
          </li>
        ))}
      </ul>
    </Notice>
  )
}
