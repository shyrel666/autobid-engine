import { useEffect, useState } from 'react'
import { ListTree, Search } from 'lucide-react'
import { api } from '../../../api/client'
import { Button, Drawer, Empty, ErrorBanner, Input, Spinner, cx } from '../../../components/ui'
import { useAsync, useElementWidth } from '../../../hooks/useAsync'
import { outlineStatusLabel, sectionStatusLabel } from '../../../lib/format'
import { linkProps, navigate } from '../../../lib/router'
import { taskUrl } from '../../../lib/routes'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import { loadDraft, draftKey, isDirty } from './drafts'
import SectionWorkspace from './SectionWorkspace'

// 章节写作工作台（图 6 / V06）：按扣除侧栏、边框与内边距后的实际内容宽度切换布局——
// ≥1120px 三栏（目录 240 / 正文 minmax(520,1fr) / 依据 320，栏间距 16）；800–1119px 目录 + 正文，依据改抽屉；<800px 只显示正文。

type Layout = 'three' | 'two' | 'one'
const layoutOf = (w: number): Layout => (w >= 1120 ? 'three' : w >= 800 ? 'two' : 'one')

export default function WritingPage({ sectionId }: { sectionId?: number }) {
  const { taskId, outlines } = useTaskWorkspace()
  const tick = useTick('outline', 'sections')
  const l = outlines.data
  const version = l?.approved_version || l?.open_version || l?.outlines[0]?.version
  const outline = useAsync(() => (version ? api.getOutline(taskId, version) : Promise.resolve(undefined)), [taskId, version, tick])
  const [ref, width] = useElementWidth<HTMLDivElement>()
  const layout = layoutOf(width || 1200)
  const [q, setQ] = useState('')
  const [tocOpen, setTocOpen] = useState(false)
  const sections = outline.data?.sections ?? []

  // 未指定章节时以 replace 补全为第一个章节，不新增后退记录。
  useEffect(() => {
    if (!sectionId && sections.length) void navigate(taskUrl(taskId, 'writing', { sectionId: sections[0].id }), { replace: true, force: true })
  }, [sectionId, sections, taskId])

  if (outlines.loading && !l) return <Spinner />
  if (!l) return <ErrorBanner error={outlines.error} onRetry={outlines.reload} />
  if (!version) {
    return <Empty>尚无大纲。请先在「大纲及审批」中生成并批准大纲，再按章节写作。<br />
      <a {...linkProps(taskUrl(taskId, 'outline'))} className="text-link hover:underline">前往大纲及审批</a></Empty>
  }
  const filtered = sections.filter((s) => !q.trim() || `${s.section_code} ${s.title}`.toLowerCase().includes(q.trim().toLowerCase()))
  const status = outline.data?.outline.status
  const toc = (
    <nav aria-label="章节目录" className="flex min-h-0 min-w-0 flex-1 flex-col">
      <div className="border-b border-line px-4 py-3">
        <h2 className="text-sm font-semibold">章节目录</h2>
        <p className="text-xs text-muted">{l.approved_version === version ? '批准' : outlineStatusLabel[status ?? ''] ?? ''}大纲 v{version} · {sections.length} 个章节</p>
        <div className="relative mt-2">
          <Search className="pointer-events-none absolute top-2 left-2.5 size-4 text-muted" aria-hidden />
          <Input aria-label="按章节标题或编码过滤" className="pl-8" placeholder="过滤章节" value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
      </div>
      <ErrorBanner error={outline.error} onRetry={outline.reload} />
      {l.approved_version !== version && <p className="border-b border-line px-4 py-2 text-xs text-warn">大纲尚未批准：写作派发与写回会被后端拒绝。</p>}
      <ul className="min-h-0 flex-1 overflow-y-auto py-1">
        {outline.loading && !outline.data ? <li><Spinner /></li> : filtered.length === 0 ? <li className="px-4 py-6 text-center text-xs text-muted">没有匹配的章节</li> : filtered.map((s) => {
          const on = s.id === sectionId
          const unsaved = isDirty(loadDraft(draftKey(taskId, s.id)))
          const bad = s.status === 'stale_outline' || s.status === 'stale_evidence'
          return (
            <li key={s.id}>
              <a {...linkProps(taskUrl(taskId, 'writing', { sectionId: s.id }))} onClick={(e) => { linkProps(taskUrl(taskId, 'writing', { sectionId: s.id })).onClick(e); setTocOpen(false) }}
                aria-current={on ? 'page' : undefined}
                className={cx('mx-2 my-1 block rounded-lg px-3 py-2.5 hover:bg-soft', on && 'bg-soft')}
                style={{ paddingLeft: 12 + (s.level - 1) * 12 }}>
                <span className="block truncate">{s.section_code} {s.title}</span>
                <span className={cx('text-xs', bad ? 'text-danger' : s.status === 'approved' ? 'text-ok' : 'text-muted')}>
                  {sectionStatusLabel[s.status] ?? s.status}{s.current_rev_no === 0 ? ' · 无正文' : ''}{s.writing ? ' · AI 撰写中' : ''}{unsaved ? ' · 有未保存修改' : ''}
                </span>
              </a>
            </li>
          )
        })}
      </ul>
    </nav>
  )
  const grid = layout === 'three' ? 'grid-cols-[240px_minmax(520px,1fr)_320px]' : layout === 'two' ? 'grid-cols-[240px_minmax(0,1fr)]' : 'grid-cols-[minmax(0,1fr)]'
  return (
    <div ref={ref} data-region="writing" data-layout={layout} className={cx('grid min-h-0 flex-1 gap-4', grid)}>
      {layout !== 'one' && <aside className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-[10px] border border-line bg-panel">{toc}</aside>}
      {sectionId ? (
        <SectionWorkspace key={sectionId} sectionId={sectionId} layout={layout}
          tocButton={layout === 'one' ? <Button size="sm" icon={<ListTree className="size-3.5" />} onClick={() => setTocOpen(true)}>目录</Button> : undefined} />
      ) : <div className="grid place-items-center text-sm text-muted">{sections.length ? '正在打开第一个章节…' : '该大纲没有章节'}</div>}
      {layout === 'one' && <Drawer open={tocOpen} side="left" title="章节目录" onClose={() => setTocOpen(false)} width={300}>{toc}</Drawer>}
    </div>
  )
}
