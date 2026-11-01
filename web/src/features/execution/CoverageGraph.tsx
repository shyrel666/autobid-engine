import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { CheckCircle2, ChevronDown, ChevronRight, FolderTree, Layers, RefreshCw, Star } from 'lucide-react'
import { api } from '../../api/client'
import type { Asset, Coverage, Finding, ReportElement } from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Pre, Spinner, cx, severityTone } from '../../components/ui'
import { assetTypeLabel, findingCategoryLabel, fmtPct, pretty, severityLabel } from '../../lib/format'

const FOLD_LIMIT = 20
const FOLD_STEP = 20

interface FoldNode {
  id: string
  parentId: string
  childType: string
  remaining: number
  total: number
}

interface TreeItem {
  id: string
  parentId: string | null
  kind: 'root' | 'tender' | 'lot' | 'type' | 'asset'
  label: string
  sublabel?: string
  typeKey?: string
  asset?: Asset
  verified?: boolean
  mandatoryCount: number
  verifiedCount: number
  totalCount: number
  findingIds: number[]
  factIds: number[]
}

export default function CoverageGraph({ taskID }: { taskID: number }) {
  const [assets, setAssets] = useState<Asset[]>([])
  const [coverage, setCoverage] = useState<Coverage | null>(null)
  const [reportElements, setReportElements] = useState<Map<number, ReportElement>>(new Map())
  const [findings, setFindings] = useState<Finding[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [onlyMandatory, setOnlyMandatory] = useState(false)
  const [onlyUnverified, setOnlyUnverified] = useState(false)
  const [selectedAssetId, setSelectedAssetId] = useState<number | null>(null)

  // Cascading fold state: collapsed node IDs & per-parent+type visible limits
  const [collapsedIds, setCollapsedIds] = useState<Set<string>>(() => new Set())
  const [unfoldLimits, setUnfoldLimits] = useState<Map<string, number>>(() => new Map())
  const sigRef = useRef('')

  const load = useCallback(async (force = false) => {
    try {
      if (force) setLoading(true)
      const [assetList, cov, findList, rep] = await Promise.all([
        api.assets(taskID),
        api.coverage(taskID).catch(() => null),
        api.findings(taskID).catch(() => [] as Finding[]),
        api.report(taskID).catch(() => null),
      ])
      const assetsArr = assetList ?? []
      const findingsArr = findList ?? []
      const elemMap = new Map<number, ReportElement>()
      for (const el of rep?.elements ?? []) {
        elemMap.set(el.asset_id, el)
      }
      const sig = `A:${assetsArr.map((a) => `${a.id}:${elemMap.get(a.id)?.verified ? 1 : 0}`).join(',')}|F:${findingsArr.length}|C:${cov?.tested ?? 0}/${cov?.target ?? 0}`
      if (force || sig !== sigRef.current) {
        sigRef.current = sig
        setAssets(assetsArr)
        setCoverage(cov)
        setReportElements(elemMap)
        setFindings(findingsArr)
      }
      setError('')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [taskID])

  useEffect(() => {
    sigRef.current = ''
    void load(true)
  }, [load])

  const filteredAssets = useMemo(() => {
    return assets.filter((a) => {
      if (onlyMandatory && !a.is_mandatory) return false
      if (onlyUnverified) {
        const el = reportElements.get(a.id)
        if (el?.verified) return false
      }
      return true
    })
  }, [assets, onlyMandatory, onlyUnverified, reportElements])

  // Build hierarchical tree: root -> tender_no -> lot_no -> asset.type -> asset
  const { itemsById, childrenOf } = useMemo(() => {
    const items = new Map<string, TreeItem>()
    const children = new Map<string, string[]>()
    const addChild = (p: string, c: string) => {
      const list = children.get(p) ?? []
      if (!list.includes(c)) list.push(c)
      children.set(p, list)
    }

    const rootId = `root:${taskID}`
    items.set(rootId, {
      id: rootId,
      parentId: null,
      kind: 'root',
      label: `项目 #${taskID}`,
      mandatoryCount: 0,
      verifiedCount: 0,
      totalCount: 0,
      findingIds: [],
      factIds: [],
    })

    for (const a of filteredAssets) {
      const tenderKey = a.tender_no || '默认招标编号'
      const lotKey = a.lot_no || '默认标段'
      const typeKey = a.type || 'clause'

      const tenderId = `tender:${tenderKey}`
      const lotId = `lot:${tenderKey}:${lotKey}`
      const typeId = `type:${tenderKey}:${lotKey}:${typeKey}`
      const assetId = `asset:${a.id}`

      const repEl = reportElements.get(a.id)
      const verified = Boolean(repEl?.verified)
      const fIds = repEl?.finding_ids ?? []
      const factIds = repEl?.fact_ids ?? []

      if (!items.has(tenderId)) {
        items.set(tenderId, {
          id: tenderId,
          parentId: rootId,
          kind: 'tender',
          label: tenderKey,
          sublabel: '招标编号',
          mandatoryCount: 0,
          verifiedCount: 0,
          totalCount: 0,
          findingIds: [],
          factIds: [],
        })
        addChild(rootId, tenderId)
      }

      if (!items.has(lotId)) {
        items.set(lotId, {
          id: lotId,
          parentId: tenderId,
          kind: 'lot',
          label: lotKey,
          sublabel: '标段',
          mandatoryCount: 0,
          verifiedCount: 0,
          totalCount: 0,
          findingIds: [],
          factIds: [],
        })
        addChild(tenderId, lotId)
      }

      if (!items.has(typeId)) {
        items.set(typeId, {
          id: typeId,
          parentId: lotId,
          kind: 'type',
          label: assetTypeLabel[typeKey] ?? typeKey,
          typeKey,
          mandatoryCount: 0,
          verifiedCount: 0,
          totalCount: 0,
          findingIds: [],
          factIds: [],
        })
        addChild(lotId, typeId)
      }

      items.set(assetId, {
        id: assetId,
        parentId: typeId,
        kind: 'asset',
        label: `${a.code ? `${a.code} ` : ''}${a.title || a.tender_no}`,
        sublabel: a.score_max != null ? `${a.score_max} 分` : a.amount ? `${a.amount} ${a.currency ?? ''}` : undefined,
        typeKey,
        asset: a,
        verified,
        mandatoryCount: a.is_mandatory ? 1 : 0,
        verifiedCount: verified ? 1 : 0,
        totalCount: 1,
        findingIds: fIds,
        factIds,
      })
      addChild(typeId, assetId)

      // Roll up counts to ancestors
      for (const ancId of [rootId, tenderId, lotId, typeId]) {
        const anc = items.get(ancId)!
        anc.totalCount += 1
        if (a.is_mandatory) anc.mandatoryCount += 1
        if (verified) anc.verifiedCount += 1
      }
    }

    return { itemsById: items, childrenOf: children }
  }, [filteredAssets, reportElements, taskID])

  // Cascading computeVisible with FOLD_LIMIT = 20 per (parentId, childType)
  const visibleTree = useMemo(() => {
    const visibleItems = new Set<string>()
    const foldsByParent = new Map<string, FoldNode[]>()

    const visit = (nodeId: string) => {
      visibleItems.add(nodeId)
      if (collapsedIds.has(nodeId)) return
      const kids = childrenOf.get(nodeId) ?? []
      if (kids.length === 0) return

      // Group children by kind/typeKey for FOLD_LIMIT folding
      const byGroup = new Map<string, string[]>()
      for (const cid of kids) {
        const item = itemsById.get(cid)
        if (!item) continue
        const gKey = item.kind === 'asset' ? (item.typeKey ?? 'asset') : item.kind
        const list = byGroup.get(gKey) ?? []
        list.push(cid)
        byGroup.set(gKey, list)
      }

      for (const [gKey, cids] of byGroup.entries()) {
        const foldKey = `fold:${nodeId}:${gKey}`
        const limit = unfoldLimits.get(foldKey) ?? FOLD_LIMIT
        const shown = cids.slice(0, limit)
        for (const cid of shown) visit(cid)
        if (cids.length > limit) {
          const flist = foldsByParent.get(nodeId) ?? []
          flist.push({
            id: foldKey,
            parentId: nodeId,
            childType: gKey,
            remaining: cids.length - limit,
            total: cids.length,
          })
          foldsByParent.set(nodeId, flist)
        }
      }
    }

    const rootId = `root:${taskID}`
    if (itemsById.has(rootId)) visit(rootId)
    return { visibleItems, foldsByParent }
  }, [childrenOf, collapsedIds, itemsById, taskID, unfoldLimits])

  const toggleCollapse = (id: string) => {
    setCollapsedIds((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const expandFold = (foldId: string) => {
    setUnfoldLimits((prev) => {
      const next = new Map(prev)
      const cur = next.get(foldId) ?? FOLD_LIMIT
      next.set(foldId, cur + FOLD_STEP)
      return next
    })
  }

  const selectedAsset = useMemo(
    () => (selectedAssetId != null ? assets.find((a) => a.id === selectedAssetId) ?? null : null),
    [assets, selectedAssetId],
  )
  const selectedRepEl = selectedAssetId != null ? reportElements.get(selectedAssetId) : undefined
  const selectedFindings = useMemo(() => {
    if (!selectedRepEl?.finding_ids?.length) return []
    const idSet = new Set(selectedRepEl.finding_ids)
    return findings.filter((f) => idSet.has(f.id))
  }, [findings, selectedRepEl])

  const renderSubtree = (nodeId: string, depth: number): React.ReactNode => {
    if (!visibleTree.visibleItems.has(nodeId)) return null
    const item = itemsById.get(nodeId)
    if (!item) return null
    const kids = (childrenOf.get(nodeId) ?? []).filter((cid) => visibleTree.visibleItems.has(cid))
    const folds = visibleTree.foldsByParent.get(nodeId) ?? []
    const hasChildren = (childrenOf.get(nodeId)?.length ?? 0) > 0
    const isCollapsed = collapsedIds.has(nodeId)
    const pct = item.totalCount > 0 ? Math.round((item.verifiedCount / item.totalCount) * 100) : 0

    return (
      <div key={nodeId} className={cx(depth > 0 && 'ml-5 border-l border-slate-200 pl-3')}>
        <div
          className={cx(
            'my-1 flex items-center justify-between gap-2 rounded-lg border px-3 py-2 text-sm transition-colors',
            item.kind === 'asset'
              ? cx(
                  'cursor-pointer',
                  selectedAssetId === item.asset?.id
                    ? 'border-blue-500 bg-blue-50/70'
                    : item.verified
                      ? 'border-emerald-200 bg-emerald-50/40 hover:bg-emerald-50/80'
                      : 'border-slate-200 bg-white hover:bg-slate-50',
                )
              : 'border-slate-200 bg-slate-50/80',
          )}
          onClick={() => {
            if (item.kind === 'asset' && item.asset) {
              setSelectedAssetId(item.asset.id)
            } else if (hasChildren) {
              toggleCollapse(nodeId)
            }
          }}
        >
          <div className="flex min-w-0 items-center gap-2">
            {hasChildren && (
              <button
                type="button"
                onClick={(e) => { e.stopPropagation(); toggleCollapse(nodeId) }}
                className="rounded p-0.5 text-slate-500 hover:bg-slate-200/60"
              >
                {isCollapsed ? <ChevronRight className="size-4" /> : <ChevronDown className="size-4" />}
              </button>
            )}
            {item.kind === 'asset' && item.asset?.is_mandatory && (
              <Star className="size-3.5 shrink-0 fill-red-500 text-red-500" />
            )}
            {item.kind === 'asset' && item.verified && (
              <CheckCircle2 className="size-3.5 shrink-0 text-emerald-600" />
            )}
            <span className={cx('truncate font-medium', item.kind === 'asset' && item.asset?.is_mandatory && 'text-red-900')}>
              {item.label}
            </span>
            {item.sublabel && <Badge tone="slate">{item.sublabel}</Badge>}
          </div>

          <div className="flex shrink-0 items-center gap-1.5 text-xs">
            {item.kind !== 'asset' ? (
              <>
                {item.mandatoryCount > 0 && <Badge tone="red">★ 强制 {item.mandatoryCount}</Badge>}
                <Badge tone={item.verifiedCount === item.totalCount && item.totalCount > 0 ? 'green' : 'blue'}>
                  已核验 {item.verifiedCount}/{item.totalCount} ({pct}%)
                </Badge>
              </>
            ) : (
              <>
                {item.findingIds.length > 0 && <Badge tone="amber">结论 {item.findingIds.length}</Badge>}
                <Badge tone={item.verified ? 'green' : 'slate'}>{item.verified ? '已核验' : '待核验'}</Badge>
              </>
            )}
          </div>
        </div>

        {!isCollapsed && (
          <div>
            {kids.map((cid) => renderSubtree(cid, depth + 1))}
            {folds.map((f) => (
              <div key={f.id} className="my-1 ml-5 border-l border-dashed border-slate-300 pl-3">
                <button
                  type="button"
                  onClick={() => expandFold(f.id)}
                  className="inline-flex items-center gap-1.5 rounded-lg border border-dashed border-blue-300 bg-blue-50/50 px-3 py-1.5 text-xs font-medium text-blue-700 hover:bg-blue-100/60"
                >
                  <Layers className="size-3.5" />
                  <span>还有 {f.remaining} 个{assetTypeLabel[f.childType] ?? f.childType}要素未展开（共 {f.total} 个）· 点击加载 +{ Math.min(FOLD_STEP, f.remaining) }</span>
                </button>
              </div>
            ))}
          </div>
        )}
      </div>
    )
  }

  return (
    <Card
      title={<><FolderTree className="size-4 text-link" /> 要素覆盖级联折叠图（Coverage Graph）</>}
      actions={
        <div className="flex flex-wrap items-center gap-2">
          <label className="inline-flex cursor-pointer items-center gap-1.5 text-xs text-slate-600">
            <input type="checkbox" checked={onlyMandatory} onChange={(e) => setOnlyMandatory(e.target.checked)} />
            <span>只看 ★ 强制性条款</span>
          </label>
          <label className="inline-flex cursor-pointer items-center gap-1.5 text-xs text-slate-600">
            <input type="checkbox" checked={onlyUnverified} onChange={(e) => setOnlyUnverified(e.target.checked)} />
            <span>只看待核验</span>
          </label>
          <Button size="sm" variant="ghost" onClick={() => setCollapsedIds(new Set())}>全部展开</Button>
          <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} loading={loading} onClick={() => void load(true)} />
        </div>
      }
    >
      <ErrorBanner error={error} onRetry={() => void load(true)} />

      {/* Coverage Summary Bar */}
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2 rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 text-xs text-slate-600">
        <div className="flex flex-wrap items-center gap-3">
          <span>总覆盖率：<b className="text-slate-900">{fmtPct(coverage?.pct)}</b>（{coverage?.tested ?? 0} / {coverage?.target ?? assets.length}）</span>
          <span>当前筛选：<b className="text-slate-900">{filteredAssets.length}</b> 个要素</span>
          <span>单节点同类型超过 {FOLD_LIMIT} 个自动级联折叠</span>
        </div>
      </div>

      {loading && assets.length === 0 ? (
        <Spinner label="正在生成要素覆盖树…" />
      ) : filteredAssets.length === 0 ? (
        <Empty>暂无匹配的招标静态要素</Empty>
      ) : (
        <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_360px]">
          <div className="max-h-[560px] overflow-y-auto pr-1">
            {renderSubtree(`root:${taskID}`, 0)}
          </div>

          <div className="rounded-xl border border-slate-200 bg-slate-50/50 p-3.5">
            {!selectedAsset ? (
              <div className="py-16 text-center text-xs text-slate-400">
                点击左侧任意具体要素节点，查看原文要求、核验状态与关联审查结论
              </div>
            ) : (
              <div className="space-y-3 text-sm">
                <div className="flex flex-wrap items-center gap-1.5">
                  <Badge tone="purple">{assetTypeLabel[selectedAsset.type] ?? selectedAsset.type}</Badge>
                  {selectedAsset.is_mandatory && <Badge tone="red">★ 强制性</Badge>}
                  <Badge tone={selectedRepEl?.verified ? 'green' : 'amber'}>
                    {selectedRepEl?.verified ? '已核验' : '待核验'}
                  </Badge>
                  <span className="ml-auto font-mono text-xs text-slate-400">#{selectedAsset.id}</span>
                </div>
                <div className="font-semibold text-slate-900">
                  {selectedAsset.code ? `${selectedAsset.code} ` : ''}{selectedAsset.title || selectedAsset.tender_no}
                </div>
                <div className="text-xs text-slate-500">
                  招标编号：{selectedAsset.tender_no}{selectedAsset.lot_no ? ` / 标段：${selectedAsset.lot_no}` : ''}
                  {selectedAsset.score_max != null ? ` · 分值：${selectedAsset.score_max}` : ''}
                </div>
                {selectedAsset.content && (
                  <div>
                    <div className="mb-1 text-xs font-medium text-slate-500">条款/要素原文</div>
                    <Pre className="max-h-48">{selectedAsset.content}</Pre>
                  </div>
                )}
                {selectedAsset.source_ref != null && (
                  <div>
                    <div className="mb-1 text-xs font-medium text-slate-500">原文定位出处</div>
                    <Pre className="max-h-32">{pretty(selectedAsset.source_ref)}</Pre>
                  </div>
                )}
                <div>
                  <div className="mb-1 text-xs font-medium text-slate-500">
                    关联事实 ({selectedRepEl?.fact_ids?.length ?? 0}) & 审查结论 ({selectedFindings.length})
                  </div>
                  {(selectedRepEl?.fact_ids?.length ?? 0) > 0 && (
                    <div className="mb-2 flex flex-wrap gap-1">
                      {selectedRepEl!.fact_ids!.map((fid) => (
                        <Badge key={fid} tone="blue">事实节点 #{fid}</Badge>
                      ))}
                    </div>
                  )}
                  {selectedFindings.length === 0 ? (
                    <p className="text-xs text-slate-400">暂无关联审查结论</p>
                  ) : (
                    <ul className="space-y-1.5">
                      {selectedFindings.map((f) => (
                        <li key={f.id} className="rounded border border-slate-200 bg-white p-2 text-xs">
                          <div className="flex items-center gap-1.5">
                            <Badge tone={severityTone(f.severity)}>{severityLabel[f.severity] ?? f.severity}</Badge>
                            <Badge>{findingCategoryLabel[f.category] ?? f.category}</Badge>
                            <span className="font-medium text-slate-800">{f.title}</span>
                          </div>
                          {f.summary && <p className="mt-1 text-slate-600">{f.summary}</p>}
                        </li>
                      ))}
                    </ul>
                  )}
                </div>
              </div>
            )}
          </div>
        </div>
      )}
    </Card>
  )
}
