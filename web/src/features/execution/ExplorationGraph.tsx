import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Maximize2, RefreshCw, Search, ZoomIn, ZoomOut } from 'lucide-react'
import { api } from '../../api/client'
import type { GraphEdge as Edge, GraphNode as Node } from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Input, Select, Spinner, cx, statusTone } from '../../components/ui'
import { useInterval } from '../../hooks/useAsync'
import { NodeDetailModal } from './NodeParts'

const KIND_META: Record<string, { label: string; fill: string; stroke: string; text: string }> = {
  begin:   { label: '起点',   fill: '#f1f5f9', stroke: '#64748b', text: '#334155' },
  goal:    { label: '目标',   fill: '#eff6ff', stroke: '#2563eb', text: '#1d4ed8' },
  intent:  { label: '工作',   fill: '#fefce8', stroke: '#ca8a04', text: '#a16207' },
  fact:    { label: '事实',   fill: '#f0fdf4', stroke: '#16a34a', text: '#15803d' },
  finding: { label: '结论',   fill: '#fef2f2', stroke: '#dc2626', text: '#b91c1c' },
  hint:    { label: '提示',   fill: '#faf5ff', stroke: '#9333ea', text: '#7e22ce' },
  digest:  { label: '冷摘要', fill: '#f8fafc', stroke: '#94a3b8', text: '#475569' },
}

const STATE_DOT: Record<string, string> = {
  running: '#2563eb',
  open: '#94a3b8',
  paused: '#d97706',
  done: '#16a34a',
  confirmed: '#16a34a',
  achieved: '#16a34a',
  blocked: '#dc2626',
  refuted: '#dc2626',
  exhausted: '#9333ea',
  stopped: '#64748b',
}

interface PositionedNode {
  node: Node
  x: number
  y: number
  layer: number
  summary: string
}

const NODE_W = 210
const NODE_H = 66
const LAYER_GAP_X = 270
const NODE_GAP_Y = 86

function nodeSummary(n: Node): string {
  const p = n.payload ?? {}
  const raw = String(p.title ?? p.summary ?? p.goal ?? p.statement ?? p.text ?? `#${n.id}`)
  return raw.length > 42 ? `${raw.slice(0, 42)}…` : raw
}

function computeDAGLayout(nodes: Node[], edges: Edge[]): { positioned: PositionedNode[]; width: number; height: number } {
  if (nodes.length === 0) return { positioned: [], width: 800, height: 480 }
  const byId = new Map<number, Node>()
  for (const n of nodes) byId.set(n.id, n)

  const outMap = new Map<number, number[]>()
  const inDegree = new Map<number, number>()
  for (const n of nodes) {
    outMap.set(n.id, [])
    inDegree.set(n.id, 0)
  }
  for (const e of edges) {
    if (!byId.has(e.From) || !byId.has(e.To) || e.From === e.To) continue
    outMap.get(e.From)!.push(e.To)
    inDegree.set(e.To, (inDegree.get(e.To) ?? 0) + 1)
  }

  // Assign layer by kind baseline + longest-path relaxation (capped to avoid cycles)
  const kindBaseLayer: Record<string, number> = {
    begin: 0,
    goal: 1,
    hint: 1,
    intent: 2,
    fact: 3,
    digest: 3,
    finding: 4,
  }
  const layerOf = new Map<number, number>()
  for (const n of nodes) {
    layerOf.set(n.id, kindBaseLayer[n.kind] ?? 2)
  }
  // Relax edges up to 6 passes so parent -> child flows left -> right
  for (let pass = 0; pass < 6; pass++) {
    let changed = false
    for (const e of edges) {
      if (!byId.has(e.From) || !byId.has(e.To) || e.From === e.To) continue
      const u = layerOf.get(e.From) ?? 0
      const v = layerOf.get(e.To) ?? 0
      if (v <= u && u < 7) {
        layerOf.set(e.To, u + 1)
        changed = true
      }
    }
    if (!changed) break
  }

  // Group by layer and normalize contiguous layer indices
  const rawLayers = [...new Set(layerOf.values())].sort((a, b) => a - b)
  const normLayer = new Map<number, number>(rawLayers.map((l, idx) => [l, idx]))

  const buckets = new Map<number, Node[]>()
  for (const n of nodes) {
    const l = normLayer.get(layerOf.get(n.id) ?? 0) ?? 0
    const arr = buckets.get(l) ?? []
    arr.push(n)
    buckets.set(l, arr)
  }

  const maxBucket = Math.max(1, ...[...buckets.values()].map((b) => b.length))
  const totalHeight = Math.max(460, maxBucket * NODE_GAP_Y + 80)
  const totalWidth = Math.max(760, (rawLayers.length) * LAYER_GAP_X + 80)

  const positioned: PositionedNode[] = []
  for (const [l, list] of buckets.entries()) {
    list.sort((a, b) => a.id - b.id)
    const colHeight = list.length * NODE_GAP_Y
    const startY = Math.max(40, (totalHeight - colHeight) / 2)
    list.forEach((n, idx) => {
      positioned.push({
        node: n,
        layer: l,
        x: 40 + l * LAYER_GAP_X,
        y: startY + idx * NODE_GAP_Y,
        summary: nodeSummary(n),
      })
    })
  }
  return { positioned, width: totalWidth, height: totalHeight }
}

export default function ExplorationGraph({ taskID, active = true }: { taskID: number; active?: boolean }) {
  const [nodes, setNodes] = useState<Node[]>([])
  const [edges, setEdges] = useState<Edge[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [kindFilter, setKindFilter] = useState('')
  const [stateFilter, setStateFilter] = useState('')
  const [query, setQuery] = useState('')
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [zoom, setZoom] = useState(1)
  const [pan, setPan] = useState<{ x: number; y: number }>({ x: 0, y: 0 })
  const dragging = useRef<{ startX: number; startY: number; panX: number; panY: number } | null>(null)
  const sigRef = useRef('')

  const load = useCallback(async (force = false) => {
    try {
      if (force) setLoading(true)
      const data = await api.taskGraph(taskID)
      const ns = data.nodes ?? []
      const es = data.edges ?? []
      const sig = `N:${ns.map((n) => `${n.id}:${n.kind}:${n.state}:${n.finding_id ?? 0}`).join(',')}|E:${es.map((e) => `${e.From}>${e.To}:${e.Rel}`).join(',')}`
      if (force || sig !== sigRef.current) {
        sigRef.current = sig
        setNodes(ns)
        setEdges(es)
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

  useInterval(() => {
    if (active) void load(false)
  }, 6000)

  const filteredNodes = useMemo(() => {
    const q = query.trim().toLowerCase()
    return nodes.filter((n) => {
      if (kindFilter && n.kind !== kindFilter) return false
      if (stateFilter && n.state !== stateFilter) return false
      if (q) {
        const s = `${n.id} ${n.kind} ${n.state} ${nodeSummary(n)}`.toLowerCase()
        if (!s.includes(q)) return false
      }
      return true
    })
  }, [nodes, kindFilter, stateFilter, query])

  const filteredIds = useMemo(() => new Set(filteredNodes.map((n) => n.id)), [filteredNodes])
  const filteredEdges = useMemo(
    () => edges.filter((e) => filteredIds.has(e.From) && filteredIds.has(e.To)),
    [edges, filteredIds],
  )

  const layout = useMemo(() => computeDAGLayout(filteredNodes, filteredEdges), [filteredNodes, filteredEdges])
  const posById = useMemo(() => new Map(layout.positioned.map((p) => [p.node.id, p])), [layout.positioned])

  const countsByKind = useMemo(() => {
    const m: Record<string, number> = {}
    for (const n of nodes) m[n.kind] = (m[n.kind] ?? 0) + 1
    return m
  }, [nodes])

  const resetView = () => {
    setZoom(1)
    setPan({ x: 0, y: 0 })
  }

  return (
    <Card
      title="推演探索图（Exploration DAG）"
      bodyClassName="p-0"
      actions={
        <div className="flex flex-wrap items-center gap-2">
          <div className="relative">
            <Search className="pointer-events-none absolute top-2 left-2.5 size-3.5 text-slate-400" />
            <Input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="搜索节点 #ID / 摘要…"
              className="h-7 w-44 pl-7 text-xs"
            />
          </div>
          <Select className="h-7 w-28 py-0 text-xs" value={kindFilter} onChange={(e) => setKindFilter(e.target.value)}>
            <option value="">全部类型 ({nodes.length})</option>
            {Object.entries(KIND_META).map(([k, m]) => (
              <option key={k} value={k}>{m.label} ({countsByKind[k] ?? 0})</option>
            ))}
          </Select>
          <Select className="h-7 w-28 py-0 text-xs" value={stateFilter} onChange={(e) => setStateFilter(e.target.value)}>
            <option value="">全部状态</option>
            <option value="running">运行中 (running)</option>
            <option value="open">待执行 (open)</option>
            <option value="paused">已暂停 (paused)</option>
            <option value="done">已完成 (done)</option>
            <option value="confirmed">已确认 (confirmed)</option>
            <option value="achieved">已达成 (achieved)</option>
            <option value="blocked">已阻塞 (blocked)</option>
            <option value="stopped">已停止/熔断 (stopped)</option>
            <option value="exhausted">配额耗尽 (exhausted)</option>
          </Select>
          <Button size="sm" variant="ghost" icon={<ZoomOut className="size-3.5" />} onClick={() => setZoom((z) => Math.max(0.45, +(z - 0.15).toFixed(2)))} title="缩小" />
          <span className="w-11 text-center font-mono text-xs text-slate-500">{Math.round(zoom * 100)}%</span>
          <Button size="sm" variant="ghost" icon={<ZoomIn className="size-3.5" />} onClick={() => setZoom((z) => Math.min(1.8, +(z + 0.15).toFixed(2)))} title="放大" />
          <Button size="sm" variant="ghost" icon={<Maximize2 className="size-3.5" />} onClick={resetView} title="重置视图" />
          <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} loading={loading} onClick={() => void load(true)} title="刷新探索图" />
        </div>
      }
    >
      <div className="px-4 pt-3 empty:hidden"><ErrorBanner error={error} onRetry={() => void load(true)} /></div>

      {/* Legend Bar */}
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-slate-100 bg-slate-50/70 px-4 py-2 text-xs text-slate-600">
        <div className="flex flex-wrap items-center gap-3">
          {Object.entries(KIND_META).map(([k, m]) => (
            <span key={k} className="inline-flex items-center gap-1.5">
              <span className="inline-block size-2.5 rounded-sm border" style={{ backgroundColor: m.fill, borderColor: m.stroke }} />
              <span>{m.label}</span>
              <span className="font-mono text-slate-400">({countsByKind[k] ?? 0})</span>
            </span>
          ))}
        </div>
        <span className="text-slate-400">
          共 {filteredNodes.length} 个节点 · {filteredEdges.length} 条推演边 · 拖拽空白处平移，点击节点查看详情
        </span>
      </div>

      {loading && nodes.length === 0 ? (
        <div className="py-16"><Spinner label="正在加载推演探索图…" /></div>
      ) : filteredNodes.length === 0 ? (
        <Empty>暂无匹配的推演节点</Empty>
      ) : (
        <div
          className="relative h-[540px] w-full cursor-grab overflow-hidden bg-[#f8fafc] select-none active:cursor-grabbing"
          onMouseDown={(e) => {
            if ((e.target as HTMLElement).closest('[data-graph-node]')) return
            dragging.current = { startX: e.clientX, startY: e.clientY, panX: pan.x, panY: pan.y }
          }}
          onMouseMove={(e) => {
            if (!dragging.current) return
            setPan({
              x: dragging.current.panX + (e.clientX - dragging.current.startX),
              y: dragging.current.panY + (e.clientY - dragging.current.startY),
            })
          }}
          onMouseUp={() => { dragging.current = null }}
          onMouseLeave={() => { dragging.current = null }}
        >
          <svg
            width="100%"
            height="100%"
            viewBox={`0 0 ${layout.width} ${layout.height}`}
            className="h-full w-full"
          >
            <defs>
              <marker id="dag-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
                <path d="M 0 1 L 10 5 L 0 9 z" fill="#94a3b8" />
              </marker>
            </defs>
            <g transform={`translate(${pan.x}, ${pan.y}) scale(${zoom})`}>
              {/* Edges */}
              {filteredEdges.map((e, idx) => {
                const src = posById.get(e.From)
                const dst = posById.get(e.To)
                if (!src || !dst) return null
                const x1 = src.x + NODE_W
                const y1 = src.y + NODE_H / 2
                const x2 = dst.x
                const y2 = dst.y + NODE_H / 2
                const dx = Math.max(40, Math.abs(x2 - x1) * 0.45)
                const d = `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`
                const midX = (x1 + x2) / 2
                const midY = (y1 + y2) / 2 - 6
                return (
                  <g key={`${e.From}-${e.To}-${e.Rel}-${idx}`}>
                    <path d={d} fill="none" stroke="#cbd5e1" strokeWidth={1.6} markerEnd="url(#dag-arrow)" />
                    {e.Rel && (
                      <text x={midX} y={midY} textAnchor="middle" fontSize="10" fill="#64748b">
                        {e.Rel}
                      </text>
                    )}
                  </g>
                )
              })}

              {/* Nodes */}
              {layout.positioned.map(({ node: n, x, y, summary }) => {
                const meta = KIND_META[n.kind] ?? KIND_META.intent
                const dotColor = STATE_DOT[n.state] ?? '#94a3b8'
                return (
                  <g
                    key={n.id}
                    data-graph-node={n.id}
                    transform={`translate(${x}, ${y})`}
                    onClick={() => setSelectedId(n.id)}
                    className="cursor-pointer transition-opacity hover:opacity-90"
                  >
                    <rect
                      width={NODE_W}
                      height={NODE_H}
                      rx={10}
                      fill={meta.fill}
                      stroke={selectedId === n.id ? '#171717' : meta.stroke}
                      strokeWidth={selectedId === n.id ? 2.4 : 1.4}
                    />
                    <circle cx={14} cy={18} r={4.5} fill={dotColor} />
                    <text x={24} y={22} fontSize="11" fontWeight="600" fill={meta.text}>
                      #{n.id} · {meta.label}{typeof n.payload?.goal_code === 'string' ? ` [${n.payload.goal_code}]` : n.payload?.mandate ? ' [Mandate]' : ''}
                    </text>
                    <text x={NODE_W - 10} y={21} textAnchor="end" fontSize="10" fill="#64748b">
                      {n.state}
                    </text>
                    <foreignObject x={10} y={28} width={NODE_W - 20} height={34}>
                      <div className={cx('line-clamp-2 text-[11px] leading-4 text-slate-700')}>
                        {summary}
                      </div>
                    </foreignObject>
                  </g>
                )
              })}
            </g>
          </svg>
        </div>
      )}

      {/* Node list quick bar for accessibility */}
      {filteredNodes.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 border-t border-slate-100 px-4 py-2.5 text-xs">
          <span className="text-slate-400">快捷检视：</span>
          {filteredNodes.slice(0, 12).map((n) => (
            <button
              key={n.id}
              type="button"
              onClick={() => setSelectedId(n.id)}
              className="inline-flex items-center gap-1 rounded border border-slate-200 bg-white px-2 py-0.5 hover:bg-slate-50"
            >
              <span className="font-mono text-blue-600">#{n.id}</span>
              <Badge tone={statusTone(n.state)}>{n.kind}</Badge>
            </button>
          ))}
        </div>
      )}

      <NodeDetailModal taskID={taskID} nodeID={selectedId} onClose={() => setSelectedId(null)} onOpen={setSelectedId} />
    </Card>
  )
}
