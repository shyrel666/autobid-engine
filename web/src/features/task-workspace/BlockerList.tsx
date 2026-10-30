import { ArrowRight } from 'lucide-react'
import type { ExportBlocker } from '../../api/types'
import { Badge } from '../../components/ui'
import { exportBlockerLabel } from '../../lib/format'
import { linkProps } from '../../lib/router'
import { blockerLink } from './blocker-links'

// 导出阻断明细：按类别分组，每项给出原因与「去处理」。未知类型保留服务器原文、不猜测目标。
export default function BlockerList({ taskId, blockers, max }: { taskId: number; blockers: ExportBlocker[]; max?: number }) {
  const groups = new Map<string, ExportBlocker[]>()
  for (const b of blockers) groups.set(b.kind, [...(groups.get(b.kind) ?? []), b])
  let shown = 0
  return (
    <div className="space-y-3">
      {[...groups.entries()].map(([kind, list]) => {
        const label = exportBlockerLabel[kind]
        const rest = max === undefined ? list : list.slice(0, Math.max(0, max - shown))
        shown += rest.length
        if (rest.length === 0) return null
        return (
          <section key={kind} aria-label={label ?? kind}>
            <div className="mb-1 flex items-center gap-2 text-xs">
              <Badge tone={label ? 'red' : 'amber'}>{label ?? `未识别的阻断类型 ${kind}`}</Badge>
              <span className="text-muted">{list.length} 项</span>
            </div>
            <ul className="divide-y divide-line-soft overflow-hidden rounded-[10px] border border-line">
              {rest.map((b, i) => {
                const link = blockerLink(taskId, b)
                return (
                  <li key={i} className="flex flex-wrap items-center gap-2 px-3 py-2 text-sm">
                    <span className="min-w-0 flex-1 break-words">{b.message}</span>
                    {link ? (
                      <a {...linkProps(link.href)} className="inline-flex h-7 shrink-0 items-center gap-1 rounded-lg border border-line px-2 text-[13px] font-medium hover:bg-soft">
                        {link.label}<ArrowRight className="size-3.5" aria-hidden />
                      </a>
                    ) : <span className="text-xs text-muted">未知类型：请刷新门禁或联系管理员确认</span>}
                  </li>
                )
              })}
            </ul>
          </section>
        )
      })}
      {max !== undefined && blockers.length > shown && <p className="text-xs text-muted">另有 {blockers.length - shown} 项，见「成稿导出」页。</p>}
    </div>
  )
}
