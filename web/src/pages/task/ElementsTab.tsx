import { useMemo, useState } from 'react'
import { ArrowRight, FolderTree, GitBranch, ListFilter, Network, RefreshCw, Search, Star } from 'lucide-react'
import { api } from '../../api/client'
import type { Asset, Finding } from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Input, Modal, Pre, Select, Spinner, cx, severityTone } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import { assetTypeLabel, findingCategoryLabel, fmtTime, pretty, severityLabel, severityOrder } from '../../lib/format'
import { SortableHeaderButton, nextSortSpec, useStoredSortPreference } from '../../lib/sort-preference'
import { NodeDetailModal } from '../../features/execution/NodeParts'
import CoverageGraph from '../../features/execution/CoverageGraph'
import ExplorationGraph from '../../features/execution/ExplorationGraph'

function AssetModal({ asset, onClose }: { asset: Asset | null; onClose: () => void }) {
  const out = useAsync(() => api.assetEdges(asset!.id, 'out'), [asset?.id], asset !== null)
  const inc = useAsync(() => api.assetEdges(asset!.id, 'in'), [asset?.id], asset !== null)
  if (!asset) return null
  return (
    <Modal open wide title={`招标要素 #${asset.id} ${asset.code ?? ''} ${asset.title ?? ''}`} onClose={onClose}>
      <div className="space-y-3 text-sm">
        <div className="flex flex-wrap gap-2">
          <Badge tone="purple">{assetTypeLabel[asset.type] ?? asset.type}</Badge>
          {asset.is_mandatory && <Badge tone="red">★ 强制性</Badge>}
          {asset.category && <Badge>{asset.category}</Badge>}
          <Badge>招标编号 {asset.tender_no}{asset.lot_no ? ` / ${asset.lot_no}` : ''}</Badge>
          {asset.score_max != null && <Badge tone="blue">分值 {asset.score_max}</Badge>}
          {asset.amount && <Badge tone="blue">金额 {asset.amount} {asset.currency}</Badge>}
          {asset.quantity && <Badge>数量 {asset.quantity} {asset.unit}</Badge>}
        </div>
        {asset.content && <Pre>{asset.content}</Pre>}
        {asset.source_ref != null && <div><div className="mb-1 text-xs text-slate-500">原文出处</div><Pre>{pretty(asset.source_ref)}</Pre></div>}
        {asset.extra != null && <div><div className="mb-1 text-xs text-slate-500">扩展属性</div><Pre>{pretty(asset.extra)}</Pre></div>}
        <div className="grid gap-3 md:grid-cols-2">
          {[{ title: '出边', s: out, other: (e: { dst_asset_id: number }) => e.dst_asset_id }, { title: '入边', s: inc, other: (e: { src_asset_id: number }) => e.src_asset_id }].map(({ title, s, other }) => (
            <div key={title}>
              <div className="mb-1 text-xs font-medium text-slate-500">{title}</div>
              <ErrorBanner error={s.error} />
              {s.loading ? <Spinner /> : (s.data ?? []).length === 0 ? <p className="text-xs text-slate-400">无</p> : (
                <ul className="space-y-1 text-xs">{s.data!.map((e) => <li key={e.id}><span className="text-blue-600">{e.rel}</span> → 要素 #{other(e)} <span className="text-slate-400">（权重 {e.weight}）</span></li>)}</ul>
              )}
            </div>
          ))}
        </div>
        <div className="text-xs text-slate-400">最近出现 {fmtTime(asset.last_seen)} · 关联任务 {asset.task_ids?.join(', ') || '—'}</div>
      </div>
    </Modal>
  )
}

/** Supports lightweight DSL tokens like `type:scoring_item`, `mandatory:true`, `lot:标段1` plus free-text keywords */
function matchAssetDSL(a: Asset, rawQuery: string): boolean {
  const q = rawQuery.trim()
  if (!q) return true
  const tokens = q.split(/\s+/)
  for (const rawTok of tokens) {
    const tok = rawTok.toLowerCase()
    if (tok.startsWith('type:')) {
      const val = tok.slice(5)
      if (val && a.type.toLowerCase() !== val && !(assetTypeLabel[a.type] ?? '').toLowerCase().includes(val)) return false
    } else if (tok === 'mandatory:true' || tok === 'is:mandatory' || tok === '强制') {
      if (!a.is_mandatory) return false
    } else if (tok === 'mandatory:false') {
      if (a.is_mandatory) return false
    } else if (tok.startsWith('lot:')) {
      const val = tok.slice(4)
      if (val && !(a.lot_no ?? '默认标段').toLowerCase().includes(val)) return false
    } else if (tok.startsWith('tender:')) {
      const val = tok.slice(7)
      if (val && !(a.tender_no ?? '').toLowerCase().includes(val)) return false
    } else {
      const hay = `${a.id} ${a.code ?? ''} ${a.title ?? ''} ${a.content ?? ''} ${a.tender_no ?? ''} ${a.lot_no ?? ''} ${a.category ?? ''}`.toLowerCase()
      if (!hay.includes(tok)) return false
    }
  }
  return true
}

type AssetSortField = 'id' | 'type' | 'score'
type FindingSortField = 'created_at' | 'severity'

export default function ElementsTab({ taskID }: { taskID: number }) {
  const [viewMode, setViewMode] = useState<'table' | 'coverage' | 'graph'>('table')
  const [type, setType] = useState('')
  const [category, setCategory] = useState('')
  const [dslQuery, setDslQuery] = useState('')
  const [findingQuery, setFindingQuery] = useState('')
  const [treeFilter, setTreeFilter] = useState<{ tender?: string; lot?: string; type?: string }>({})
  const [showTreeSidebar, setShowTreeSidebar] = useState(true)
  const [asset, setAsset] = useState<Asset | null>(null)
  const [nodeID, setNodeID] = useState<number | null>(null)

  const [assetSort, setAssetSort] = useStoredSortPreference<AssetSortField>({
    storageKey: 'autobid.elements.asset_sort',
    defaultSort: { field: 'id', direction: 'asc' },
    allowedFields: ['id', 'type', 'score'] as const,
  })
  const [findingSort, setFindingSort] = useStoredSortPreference<FindingSortField>({
    storageKey: 'autobid.elements.finding_sort',
    defaultSort: { field: 'severity', direction: 'desc' },
    allowedFields: ['created_at', 'severity'] as const,
  })

  const assets = useAsync(() => api.assets(taskID, type ? [type] : undefined), [taskID, type])
  const findings = useAsync(() => api.findings(taskID, category || undefined), [taskID, category])

  // Build hierarchical tree structure from all loaded assets: tender_no -> lot_no -> type
  const hierarchy = useMemo(() => {
    const list = assets.data ?? []
    const tenders = new Map<string, {
      count: number
      mandatory: number
      lots: Map<string, { count: number; mandatory: number; types: Map<string, { count: number; mandatory: number }> }>
    }>()

    for (const a of list) {
      const tKey = a.tender_no || '默认招标编号'
      const lKey = a.lot_no || '默认标段'
      const tyKey = a.type || 'clause'

      if (!tenders.has(tKey)) {
        tenders.set(tKey, { count: 0, mandatory: 0, lots: new Map() })
      }
      const tEntry = tenders.get(tKey)!
      tEntry.count += 1
      if (a.is_mandatory) tEntry.mandatory += 1

      if (!tEntry.lots.has(lKey)) {
        tEntry.lots.set(lKey, { count: 0, mandatory: 0, types: new Map() })
      }
      const lEntry = tEntry.lots.get(lKey)!
      lEntry.count += 1
      if (a.is_mandatory) lEntry.mandatory += 1

      const tyEntry = lEntry.types.get(tyKey) ?? { count: 0, mandatory: 0 }
      tyEntry.count += 1
      if (a.is_mandatory) tyEntry.mandatory += 1
      lEntry.types.set(tyKey, tyEntry)
    }
    return tenders
  }, [assets.data])

  const filteredAssets = useMemo(() => {
    const base = (assets.data ?? []).filter((a) => {
      const tKey = a.tender_no || '默认招标编号'
      const lKey = a.lot_no || '默认标段'
      if (treeFilter.tender && tKey !== treeFilter.tender) return false
      if (treeFilter.lot && lKey !== treeFilter.lot) return false
      if (treeFilter.type && a.type !== treeFilter.type) return false
      return matchAssetDSL(a, dslQuery)
    })

    const dir = assetSort.direction === 'asc' ? 1 : -1
    return [...base].sort((a, b) => {
      if (assetSort.field === 'type') {
        return a.type.localeCompare(b.type) * dir || (a.id - b.id)
      }
      if (assetSort.field === 'score') {
        const sa = a.score_max ?? -1
        const sb = b.score_max ?? -1
        return (sa - sb) * dir || (a.id - b.id)
      }
      return (a.id - b.id) * dir
    })
  }, [assets.data, treeFilter, dslQuery, assetSort])

  const filteredFindings = useMemo(() => {
    const q = findingQuery.trim().toLowerCase()
    const base = (findings.data ?? []).filter((f: Finding) => {
      if (!q) return true
      return `${f.id} ${f.title} ${f.summary ?? ''} ${f.category} ${f.severity}`.toLowerCase().includes(q)
    })
    const dir = findingSort.direction === 'asc' ? 1 : -1
    return [...base].sort((a, b) => {
      if (findingSort.field === 'severity') {
        const ia = severityOrder.indexOf(a.severity)
        const ib = severityOrder.indexOf(b.severity)
        const rankA = ia === -1 ? 99 : ia
        const rankB = ib === -1 ? 99 : ib
        // Lower index in severityOrder means higher severity ('critical' is 0)
        return (rankB - rankA) * dir || (b.id - a.id)
      }
      const ta = new Date(a.created_at).getTime() || 0
      const tb = new Date(b.created_at).getTime() || 0
      return (ta - tb) * dir || (b.id - a.id)
    })
  }, [findings.data, findingQuery, findingSort])

  const activeTreeLabel = useMemo(() => {
    const parts: string[] = []
    if (treeFilter.tender) parts.push(treeFilter.tender)
    if (treeFilter.lot) parts.push(treeFilter.lot)
    if (treeFilter.type) parts.push(assetTypeLabel[treeFilter.type] ?? treeFilter.type)
    return parts.join(' / ')
  }, [treeFilter])

  return (
    <div className="space-y-4">
      {/* Top View Switcher */}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-1.5">
          <Button
            size="sm"
            variant={viewMode === 'table' ? 'primary' : 'secondary'}
            icon={<ListFilter className="size-3.5" />}
            onClick={() => setViewMode('table')}
          >
            要素树与审查结论
          </Button>
          <Button
            size="sm"
            variant={viewMode === 'coverage' ? 'primary' : 'secondary'}
            icon={<FolderTree className="size-3.5" />}
            onClick={() => setViewMode('coverage')}
          >
            要素覆盖折叠图
          </Button>
          <Button
            size="sm"
            variant={viewMode === 'graph' ? 'primary' : 'secondary'}
            icon={<Network className="size-3.5" />}
            onClick={() => setViewMode('graph')}
          >
            推演探索图 (DAG)
          </Button>
        </div>

        {viewMode === 'table' && (
          <div className="flex items-center gap-2">
            <Button
              size="sm"
              variant="ghost"
              icon={<FolderTree className="size-3.5" />}
              onClick={() => setShowTreeSidebar((v) => !v)}
            >
              {showTreeSidebar ? '隐藏要素目录树' : '显示要素目录树'}
            </Button>
          </div>
        )}
      </div>

      {viewMode === 'coverage' ? (
        <CoverageGraph taskID={taskID} />
      ) : viewMode === 'graph' ? (
        <ExplorationGraph taskID={taskID} />
      ) : (
        <div className={cx('grid gap-4', showTreeSidebar ? 'xl:grid-cols-[240px_minmax(0,1fr)_minmax(0,1fr)]' : 'xl:grid-cols-2')}>
          {/* Left Hierarchy Tree Sidebar */}
          {showTreeSidebar && (
            <Card
              title="要素层级导航"
              bodyClassName="p-2"
              actions={
                activeTreeLabel ? (
                  <button
                    type="button"
                    onClick={() => setTreeFilter({})}
                    className="text-xs text-blue-600 hover:underline"
                  >
                    重置
                  </button>
                ) : null
              }
            >
              <div className="max-h-[640px] space-y-1 overflow-y-auto text-xs">
                <button
                  type="button"
                  onClick={() => setTreeFilter({})}
                  className={cx(
                    'flex w-full items-center justify-between rounded-md px-2 py-1.5 text-left font-medium',
                    !activeTreeLabel ? 'bg-blue-50 text-blue-700' : 'text-slate-700 hover:bg-slate-100',
                  )}
                >
                  <span>全部招标要素</span>
                  <span className="font-mono text-slate-400">{(assets.data ?? []).length}</span>
                </button>

                {[...hierarchy.entries()].map(([tenderKey, tEntry]) => (
                  <div key={tenderKey} className="space-y-0.5 pt-1">
                    <button
                      type="button"
                      onClick={() => setTreeFilter({ tender: tenderKey })}
                      className={cx(
                        'flex w-full items-center justify-between rounded-md px-2 py-1 text-left font-medium',
                        treeFilter.tender === tenderKey && !treeFilter.lot ? 'bg-blue-50 text-blue-700' : 'text-slate-700 hover:bg-slate-100',
                      )}
                    >
                      <span className="truncate" title={tenderKey}>{tenderKey}</span>
                      <span className="flex items-center gap-1 font-mono text-[11px] text-slate-400">
                        {tEntry.mandatory > 0 && <span className="text-red-500">★{tEntry.mandatory}</span>}
                        <span>{tEntry.count}</span>
                      </span>
                    </button>

                    {[...tEntry.lots.entries()].map(([lotKey, lEntry]) => (
                      <div key={lotKey} className="ml-2.5 border-l border-slate-200 pl-2">
                        <button
                          type="button"
                          onClick={() => setTreeFilter({ tender: tenderKey, lot: lotKey })}
                          className={cx(
                            'flex w-full items-center justify-between rounded-md px-2 py-1 text-left',
                            treeFilter.tender === tenderKey && treeFilter.lot === lotKey && !treeFilter.type
                              ? 'bg-blue-50 font-medium text-blue-700'
                              : 'text-slate-600 hover:bg-slate-100',
                          )}
                        >
                          <span className="truncate">{lotKey}</span>
                          <span className="font-mono text-[11px] text-slate-400">{lEntry.count}</span>
                        </button>

                        <div className="ml-2 space-y-0.5">
                          {[...lEntry.types.entries()].map(([typeKey, tyEntry]) => (
                            <button
                              key={typeKey}
                              type="button"
                              onClick={() => setTreeFilter({ tender: tenderKey, lot: lotKey, type: typeKey })}
                              className={cx(
                                'flex w-full items-center justify-between rounded px-2 py-0.5 text-left',
                                treeFilter.tender === tenderKey && treeFilter.lot === lotKey && treeFilter.type === typeKey
                                  ? 'bg-blue-50 font-medium text-blue-700'
                                  : 'text-slate-500 hover:bg-slate-100',
                              )}
                            >
                              <span className="truncate">{assetTypeLabel[typeKey] ?? typeKey}</span>
                              <span className="flex items-center gap-1 font-mono text-[11px] text-slate-400">
                                {tyEntry.mandatory > 0 && <span className="text-red-500">★{tyEntry.mandatory}</span>}
                                <span>{tyEntry.count}</span>
                              </span>
                            </button>
                          ))}
                        </div>
                      </div>
                    ))}
                  </div>
                ))}
              </div>
            </Card>
          )}

          {/* Middle/Left Card: Static Tender Elements Table */}
          <Card
            title={
              <div className="flex flex-wrap items-center gap-2">
                <span>招标静态要素（{filteredAssets.length}）</span>
                {activeTreeLabel && <Badge tone="blue">{activeTreeLabel}</Badge>}
              </div>
            }
            bodyClassName="p-0"
            actions={
              <div className="flex flex-wrap items-center gap-1.5">
                <div className="relative">
                  <Search className="pointer-events-none absolute top-2 left-2 size-3.5 text-slate-400" />
                  <Input
                    value={dslQuery}
                    onChange={(e) => setDslQuery(e.target.value)}
                    placeholder="过滤：强制 / type:scoring_item / 关键词"
                    className="h-7 w-48 pl-7 text-xs"
                  />
                </div>
                <Select className="h-7 w-28 py-0 text-xs" value={type} onChange={(e) => setType(e.target.value)}>
                  <option value="">全部类型</option>
                  {Object.entries(assetTypeLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
                </Select>
                <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => assets.reload()} />
              </div>
            }
          >
            <div className="px-4 pt-3 empty:hidden"><ErrorBanner error={assets.error} onRetry={assets.reload} /></div>
            {assets.loading && !assets.data ? <Spinner /> : filteredAssets.length === 0 ? <Empty>暂无匹配的招标静态要素</Empty> : (
              <div className="max-h-[640px] overflow-y-auto">
                <table className="w-full text-sm">
                  <thead className="sticky top-0 bg-slate-50 text-left text-xs text-slate-500">
                    <tr>
                      <th className="px-3 py-2">
                        <SortableHeaderButton
                          field="id"
                          sort={assetSort}
                          onToggle={(f) => setAssetSort((cur) => nextSortSpec(cur, f, 'asc'))}
                          label="#"
                        />
                      </th>
                      <th className="px-3 py-2">
                        <SortableHeaderButton
                          field="type"
                          sort={assetSort}
                          onToggle={(f) => setAssetSort((cur) => nextSortSpec(cur, f, 'asc'))}
                          label="类型"
                        />
                      </th>
                      <th className="px-3 py-2">编号 / 标题</th>
                      <th className="px-3 py-2">
                        <SortableHeaderButton
                          field="score"
                          sort={assetSort}
                          onToggle={(f) => setAssetSort((cur) => nextSortSpec(cur, f, 'desc'))}
                          label="分值/金额"
                        />
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {filteredAssets.map((a) => (
                      <tr key={a.id} onClick={() => setAsset(a)} className={cx('cursor-pointer border-t border-slate-100 hover:bg-slate-50', a.is_mandatory && 'bg-red-50/40')}>
                        <td className="px-3 py-2 text-xs text-slate-400">{a.id}</td>
                        <td className="px-3 py-2"><Badge>{assetTypeLabel[a.type] ?? a.type}</Badge></td>
                        <td className="px-3 py-2">
                          <div className="flex items-center gap-1">
                            {a.is_mandatory && <Star className="size-3.5 shrink-0 fill-red-500 text-red-500" />}
                            <span className="font-mono text-xs text-slate-500">{a.code}</span>
                            <span className={cx('line-clamp-1', a.is_mandatory && 'text-red-800')}>{a.title || a.tender_no}</span>
                          </div>
                        </td>
                        <td className="px-3 py-2 text-xs text-slate-500">{a.score_max ?? a.amount ?? '—'}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Card>

          {/* Right Card: Findings with Evidence Lineage & Sort Preference */}
          <Card
            title={`审查结论（${filteredFindings.length}）`}
            bodyClassName="p-0"
            actions={
              <div className="flex flex-wrap items-center gap-1.5">
                <div className="relative">
                  <Search className="pointer-events-none absolute top-2 left-2 size-3.5 text-slate-400" />
                  <Input
                    value={findingQuery}
                    onChange={(e) => setFindingQuery(e.target.value)}
                    placeholder="过滤结论标题/摘要…"
                    className="h-7 w-36 pl-7 text-xs"
                  />
                </div>
                <Select className="h-7 w-28 py-0 text-xs" value={category} onChange={(e) => setCategory(e.target.value)}>
                  <option value="">全部类别</option>
                  {Object.entries(findingCategoryLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
                </Select>
                <SortableHeaderButton
                  field="severity"
                  sort={findingSort}
                  onToggle={(f) => setFindingSort((cur) => nextSortSpec(cur, f, 'desc'))}
                  label="严重度"
                  className="text-xs"
                />
                <SortableHeaderButton
                  field="created_at"
                  sort={findingSort}
                  onToggle={(f) => setFindingSort((cur) => nextSortSpec(cur, f, 'desc'))}
                  label="时间"
                  className="text-xs"
                />
                <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => findings.reload()} />
              </div>
            }
          >
            <div className="px-4 pt-3 empty:hidden"><ErrorBanner error={findings.error} onRetry={findings.reload} /></div>
            {findings.loading && !findings.data ? <Spinner /> : filteredFindings.length === 0 ? <Empty>暂无审查结论</Empty> : (
              <ul className="max-h-[640px] divide-y divide-slate-100 overflow-y-auto">
                {filteredFindings.map((f) => (
                  <li key={f.id} className="px-4 py-3 text-sm">
                    <div className="flex flex-wrap items-center gap-2">
                      <Badge tone={severityTone(f.severity)}>{severityLabel[f.severity] ?? f.severity}</Badge>
                      <Badge>{findingCategoryLabel[f.category] ?? f.category}</Badge>
                      <span className="font-medium">{f.title}</span>
                      <span className="ml-auto text-xs text-slate-400">{fmtTime(f.created_at)}</span>
                    </div>
                    {f.summary && <p className="mt-1 text-slate-600">{f.summary}</p>}
                    {/* Visual Evidence Lineage Chain */}
                    <div className="mt-2 flex flex-wrap items-center gap-1.5 rounded-md border border-slate-200/80 bg-slate-50/70 px-2.5 py-1.5 text-xs">
                      <GitBranch className="size-3.5 text-slate-400" />
                      <span className="text-slate-500">证据血缘：</span>
                      <button
                        type="button"
                        className="rounded bg-red-50 px-1.5 py-0.5 font-mono text-red-700 hover:underline"
                        onClick={() => setNodeID(f.node_id)}
                      >
                        结论节点 #{f.node_id}
                      </button>
                      {(f.evidence_node_ids?.length ?? 0) > 0 && (
                        <>
                          <ArrowRight className="size-3 text-slate-400" />
                          {f.evidence_node_ids.map((id) => (
                            <button
                              key={id}
                              type="button"
                              className="rounded bg-emerald-50 px-1.5 py-0.5 font-mono text-emerald-700 hover:underline"
                              onClick={() => setNodeID(id)}
                            >
                              事实证据 #{id}
                            </button>
                          ))}
                        </>
                      )}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>
      )}

      <AssetModal asset={asset} onClose={() => setAsset(null)} />
      <NodeDetailModal taskID={taskID} nodeID={nodeID} onClose={() => setNodeID(null)} onOpen={setNodeID} />
    </div>
  )
}
