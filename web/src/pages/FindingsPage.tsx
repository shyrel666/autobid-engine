import { useEffect, useMemo, useState } from 'react'
import { ArrowUpRight, Bug, RefreshCw, Search } from 'lucide-react'
import { api } from '../api/client'
import type { Finding, Task } from '../api/types'
import { PageFrame } from '../components/layout/AppShell'
import { Badge, Button, Card, Empty, ErrorBanner, IconButton, Input, Modal, Pre, Select, Spinner, Stat, severityTone } from '../components/ui'
import { NodeDetailModal } from '../features/execution/NodeParts'
import { useAsync } from '../hooks/useAsync'
import { findingCategoryLabel, fmtTime, pretty, severityLabel, severityOrder } from '../lib/format'
import { linkProps } from '../lib/router'
import { taskUrl } from '../lib/routes'

interface TaskFinding extends Finding {
  taskName: string
}

export default function FindingsPage() {
  const tasks = useAsync(() => api.listTasks(), [])
  const [taskFilter, setTaskFilter] = useState<string>('all')
  const [severityFilter, setSeverityFilter] = useState<string>('')
  const [categoryFilter, setCategoryFilter] = useState<string>('')
  const [query, setQuery] = useState('')
  const [selectedFinding, setSelectedFinding] = useState<TaskFinding | null>(null)
  const [nodeModal, setNodeModal] = useState<{ taskId: number; nodeId: number } | null>(null)
  const [loadingFindings, setLoadingFindings] = useState(false)
  const [findingsError, setFindingsError] = useState<string>('')
  const [items, setItems] = useState<TaskFinding[]>([])
  const [reloadTick, setReloadTick] = useState(0)

  const taskList: Task[] = tasks.data ?? []

  useEffect(() => {
    if (!tasks.data) return
    let alive = true
    setLoadingFindings(true)
    setFindingsError('')
    const targets = taskFilter === 'all' ? tasks.data.slice(0, 15) : tasks.data.filter((t) => String(t.id) === taskFilter)
    Promise.all(
      targets.map(async (t) => {
        const list = (await api.findings(t.id, categoryFilter || undefined)) ?? []
        return list.map((f) => ({ ...f, taskName: t.name }))
      }),
    )
      .then((groups) => {
        if (!alive) return
        setItems(groups.flat())
      })
      .catch((e) => {
        if (!alive) return
        setFindingsError(e instanceof Error ? e.message : String(e))
      })
      .finally(() => {
        if (alive) setLoadingFindings(false)
      })
    return () => {
      alive = false
    }
  }, [tasks.data, taskFilter, categoryFilter, reloadTick])

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return items
      .filter((f) => {
        if (severityFilter && f.severity !== severityFilter) return false
        if (!q) return true
        return `${f.id} ${f.title} ${f.summary ?? ''} ${f.taskName} ${f.category}`.toLowerCase().includes(q)
      })
      .sort((a, b) => {
        const ia = severityOrder.indexOf(a.severity)
        const ib = severityOrder.indexOf(b.severity)
        const ra = ia === -1 ? 99 : ia
        const rb = ib === -1 ? 99 : ib
        return ra - rb || b.id - a.id
      })
  }, [items, severityFilter, query])

  const counts = useMemo(() => {
    const c = { total: items.length, critical: 0, high: 0, medium: 0, low: 0 }
    for (const f of items) {
      if (f.severity === 'critical') c.critical++
      else if (f.severity === 'high') c.high++
      else if (f.severity === 'medium') c.medium++
      else c.low++
    }
    return c
  }, [items])

  return (
    <PageFrame
      title="审查发现"
      description="汇总各投标项目在推演审查过程中产出的风险结论、资格/报价核验发现与证据溯源节点"
      actions={
        <IconButton label="刷新审查发现" onClick={() => { void tasks.reload(); setReloadTick((n) => n + 1) }}>
          <RefreshCw className="size-4" />
        </IconButton>
      }
    >
      <ErrorBanner error={tasks.error || findingsError} onRetry={() => { void tasks.reload(); setReloadTick((n) => n + 1) }} />

      {/* 统计指标栏 */}
      <div className="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Stat label="发现总数" value={counts.total} hint={taskFilter === 'all' ? `近期 ${Math.min(taskList.length, 15)} 个项目汇总` : `项目 #${taskFilter}`} />
        <Stat label="严重 / 高危 (Critical & High)" value={counts.critical + counts.high} tone={counts.critical + counts.high > 0 ? 'red' : undefined} hint={`严重 ${counts.critical} · 高危 ${counts.high}`} />
        <Stat label="中风险 (Medium)" value={counts.medium} tone={counts.medium > 0 ? 'amber' : undefined} />
        <Stat label="低风险 / 提示 (Low & Info)" value={counts.low} />
      </div>

      {/* 筛选工具栏 */}
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <div className="relative min-w-56 flex-1 sm:max-w-xs">
          <Search className="pointer-events-none absolute top-2 left-2.5 size-4 text-muted" />
          <Input
            aria-label="搜索审查发现"
            className="pl-8"
            placeholder="搜索标题、摘要或项目名…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
        <Select
          aria-label="按投标项目筛选"
          className="w-56"
          value={taskFilter}
          onChange={(e) => setTaskFilter(e.target.value)}
        >
          <option value="all">全部近期项目 ({Math.min(taskList.length, 15)})</option>
          {taskList.map((t) => (
            <option key={t.id} value={String(t.id)}>
              #{t.id} {t.name}
            </option>
          ))}
        </Select>
        <Select
          aria-label="按风险等级筛选"
          className="w-40"
          value={severityFilter}
          onChange={(e) => setSeverityFilter(e.target.value)}
        >
          <option value="">全部等级</option>
          {['critical', 'high', 'medium', 'low', 'info'].map((s) => (
            <option key={s} value={s}>{severityLabel[s] ?? s}</option>
          ))}
        </Select>
        <Select
          aria-label="按发现类别筛选"
          className="w-44"
          value={categoryFilter}
          onChange={(e) => setCategoryFilter(e.target.value)}
        >
          <option value="">全部类别</option>
          {Object.entries(findingCategoryLabel).map(([k, v]) => (
            <option key={k} value={k}>{v}</option>
          ))}
        </Select>
      </div>

      {/* 审查发现列表 */}
      <Card bodyClassName="p-0">
        {(tasks.loading || loadingFindings) && items.length === 0 ? (
          <Spinner />
        ) : filtered.length === 0 ? (
          <Empty>暂无匹配的审查发现记录</Empty>
        ) : (
          <div className="divide-y divide-line">
            {filtered.map((f) => (
              <div key={`${f.task_id}-${f.id}`} className="flex flex-wrap items-start justify-between gap-3 px-4 py-3.5 hover:bg-soft/40">
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <Bug className="size-4 shrink-0 text-muted" />
                    <Badge tone={severityTone(f.severity)}>{severityLabel[f.severity] ?? f.severity}</Badge>
                    <Badge>{findingCategoryLabel[f.category] ?? f.category}</Badge>
                    <button
                      type="button"
                      onClick={() => setSelectedFinding(f)}
                      className="text-left text-sm font-semibold text-fg hover:text-link"
                    >
                      {f.title}
                    </button>
                  </div>
                  {f.summary && <p className="mt-1.5 text-xs leading-relaxed text-muted">{f.summary}</p>}
                  <div className="mt-2 flex flex-wrap items-center gap-3 text-[11px] text-muted">
                    <a {...linkProps(taskUrl(f.task_id, 'elements'))} className="inline-flex items-center gap-0.5 font-medium text-link hover:underline">
                      项目 #{f.task_id} · {f.taskName} <ArrowUpRight className="size-3" />
                    </a>
                    <span>发现 #{f.id}</span>
                    <button
                      type="button"
                      onClick={() => setNodeModal({ taskId: f.task_id, nodeId: f.node_id })}
                      className="text-link hover:underline"
                    >
                      推演节点 #{f.node_id}
                    </button>
                    {f.evidence_node_ids?.length > 0 && <span>证据节点：{f.evidence_node_ids.join(', ')}</span>}
                    <span>{fmtTime(f.created_at)}</span>
                  </div>
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  <Button size="sm" variant="secondary" onClick={() => setSelectedFinding(f)}>
                    详情
                  </Button>
                </div>
              </div>
            ))}
          </div>
        )}
      </Card>

      {selectedFinding && (
        <Modal
          open
          wide
          title={`审查发现 #${selectedFinding.id} · ${selectedFinding.title}`}
          onClose={() => setSelectedFinding(null)}
        >
          <div className="space-y-3 text-sm">
            <div className="flex flex-wrap items-center gap-2">
              <Badge tone={severityTone(selectedFinding.severity)}>
                {severityLabel[selectedFinding.severity] ?? selectedFinding.severity}
              </Badge>
              <Badge>{findingCategoryLabel[selectedFinding.category] ?? selectedFinding.category}</Badge>
              <a
                {...linkProps(taskUrl(selectedFinding.task_id, 'elements'))}
                className="text-xs text-link hover:underline"
              >
                前往项目 #{selectedFinding.task_id}（{selectedFinding.taskName}）→
              </a>
            </div>
            {selectedFinding.summary && (
              <div className="rounded-lg border border-line bg-soft/30 p-3 text-xs leading-relaxed">
                {selectedFinding.summary}
              </div>
            )}
            {selectedFinding.payload && Object.keys(selectedFinding.payload).length > 0 && (
              <div>
                <div className="mb-1 text-xs font-medium text-muted">结构化载荷 (Payload)</div>
                <Pre>{pretty(selectedFinding.payload)}</Pre>
              </div>
            )}
          </div>
        </Modal>
      )}

      {nodeModal && (
        <NodeDetailModal
          taskID={nodeModal.taskId}
          nodeID={nodeModal.nodeId}
          onClose={() => setNodeModal(null)}
          onOpen={(id) => setNodeModal({ taskId: nodeModal.taskId, nodeId: id })}
        />
      )}
    </PageFrame>
  )
}
