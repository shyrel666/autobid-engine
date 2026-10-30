import { useState } from 'react'
import { RefreshCw, Star } from 'lucide-react'
import { api } from '../../api/client'
import type { Asset } from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Modal, Pre, Select, Spinner, cx, severityTone } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import { assetTypeLabel, findingCategoryLabel, fmtTime, pretty, severityLabel } from '../../lib/format'
import { NodeDetailModal } from '../../features/execution/NodeParts'

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

export default function ElementsTab({ taskID }: { taskID: number }) {
  const [type, setType] = useState('')
  const [category, setCategory] = useState('')
  const [asset, setAsset] = useState<Asset | null>(null)
  const [nodeID, setNodeID] = useState<number | null>(null)
  const assets = useAsync(() => api.assets(taskID, type ? [type] : undefined), [taskID, type])
  const findings = useAsync(() => api.findings(taskID, category || undefined), [taskID, category])

  return (
    <div className="grid gap-4 xl:grid-cols-2">
      <Card title="招标静态要素（任务范围）" bodyClassName="p-0"
        actions={<>
          <Select className="w-32 py-1 text-xs" value={type} onChange={(e) => setType(e.target.value)}>
            <option value="">全部类型</option>
            {Object.entries(assetTypeLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
          </Select>
          <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => assets.reload()} />
        </>}>
        <div className="px-4 pt-3 empty:hidden"><ErrorBanner error={assets.error} onRetry={assets.reload} /></div>
        {assets.loading && !assets.data ? <Spinner /> : (assets.data ?? []).length === 0 ? <Empty>暂无要素（导入招标文档或推演后生成）</Empty> : (
          <div className="max-h-[640px] overflow-y-auto">
            <table className="w-full text-sm">
              <thead className="sticky top-0 bg-slate-50 text-left text-xs text-slate-500">
                <tr><th className="px-3 py-2">#</th><th className="px-3 py-2">类型</th><th className="px-3 py-2">编号 / 标题</th><th className="px-3 py-2">分值/金额</th></tr>
              </thead>
              <tbody>
                {assets.data!.map((a) => (
                  <tr key={a.id} onClick={() => setAsset(a)} className={cx('cursor-pointer border-t border-slate-100 hover:bg-slate-50', a.is_mandatory && 'bg-red-50/40')}>
                    <td className="px-3 py-2 text-xs text-slate-400">{a.id}</td>
                    <td className="px-3 py-2"><Badge>{assetTypeLabel[a.type] ?? a.type}</Badge></td>
                    <td className="px-3 py-2">
                      <div className="flex items-center gap-1">
                        {a.is_mandatory && <Star className="size-3.5 fill-red-500 text-red-500" />}
                        <span className="font-mono text-xs text-slate-500">{a.code}</span>
                        <span className={cx(a.is_mandatory && 'text-red-800')}>{a.title || a.tender_no}</span>
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
      <Card title="审查结论（Findings）" bodyClassName="p-0"
        actions={<>
          <Select className="w-32 py-1 text-xs" value={category} onChange={(e) => setCategory(e.target.value)}>
            <option value="">全部类别</option>
            {Object.entries(findingCategoryLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
          </Select>
          <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => findings.reload()} />
        </>}>
        <div className="px-4 pt-3 empty:hidden"><ErrorBanner error={findings.error} onRetry={findings.reload} /></div>
        {findings.loading && !findings.data ? <Spinner /> : (findings.data ?? []).length === 0 ? <Empty>暂无审查结论</Empty> : (
          <ul className="max-h-[640px] divide-y divide-slate-100 overflow-y-auto">
            {findings.data!.map((f) => (
              <li key={f.id} className="px-4 py-3 text-sm">
                <div className="flex flex-wrap items-center gap-2">
                  <Badge tone={severityTone(f.severity)}>{severityLabel[f.severity] ?? f.severity}</Badge>
                  <Badge>{findingCategoryLabel[f.category] ?? f.category}</Badge>
                  <span className="font-medium">{f.title}</span>
                </div>
                {f.summary && <p className="mt-1 text-slate-600">{f.summary}</p>}
                <div className="mt-1 flex flex-wrap gap-2 text-xs text-slate-500">
                  <button className="text-blue-600 hover:underline" onClick={() => setNodeID(f.node_id)}>结论节点 #{f.node_id}</button>
                  {f.evidence_node_ids.map((id) => <button key={id} className="text-blue-600 hover:underline" onClick={() => setNodeID(id)}>证据 #{id}</button>)}
                  <span>{fmtTime(f.created_at)}</span>
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>
      <AssetModal asset={asset} onClose={() => setAsset(null)} />
      <NodeDetailModal taskID={taskID} nodeID={nodeID} onClose={() => setNodeID(null)} onOpen={setNodeID} />
    </div>
  )
}
