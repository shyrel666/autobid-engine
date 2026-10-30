import { useMemo, useState } from 'react'
import { ExternalLink } from 'lucide-react'
import { api, errorText } from '../../../api/client'
import type { EvidenceClaim, EvidenceState, RetrievalEvidence, RetrievalRun } from '../../../api/types'
import { TableView } from '../../../components/TableView'
import { Badge, Button, Drawer, Empty, ErrorBanner, Field, Input, Select, Spinner, cx } from '../../../components/ui'
import type { Tone } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'
import { pretty } from '../../../lib/format'

// 证据呈现与选择（FRONTEND_REFACTOR_PLAN.md 第 5.6 节）：核验状态与支持关系分开展示；
// 未核验、证据不足、矛盾、失效与缺失分别呈现；verified + supports 只表示该条指标已核验，不代表整项要求已满足。

export const evidenceStatusLabel: Record<string, string> = { verified: '指标已核验', unverified: '待核验', stale: '来源已失效' }
export const evidenceRelationLabel: Record<string, string> = { supports: '支持', contradicts: '矛盾', insufficient: '证据不足' }
export const sourceDomainLabel: Record<string, string> = { knowledge: '企业方案知识库', tender: '招标原文', credential: '企业资信', finding: '推演结论' }

export function evidenceTone(status: string, relation: string, missing?: boolean): Tone {
  if (missing || status === 'stale' || relation === 'contradicts') return 'red'
  if (status === 'verified' && relation === 'supports') return 'green'
  return 'amber'
}

export function EvidenceBadges({ status, relation, missing }: { status: string; relation: string; missing?: boolean }) {
  if (missing) return <Badge dot tone="red">证据缺失</Badge>
  return (
    <span className="inline-flex flex-wrap gap-1">
      <Badge dot tone={status === 'verified' ? 'green' : status === 'stale' ? 'red' : 'amber'}>{evidenceStatusLabel[status] ?? status}</Badge>
      <Badge tone={relation === 'supports' ? 'slate' : relation === 'contradicts' ? 'red' : 'amber'}>{evidenceRelationLabel[relation] ?? (relation || '关系未判定')}</Badge>
    </span>
  )
}

/** EvidenceStateRow 章节当前修订引用的证据有效性（来自 SectionDetail.evidence）。 */
export function EvidenceStateRow({ e, onOpen, highlight }: { e: EvidenceState; onOpen?: () => void; highlight?: boolean }) {
  return (
    <div id={`evidence-${e.id}`} className={cx('rounded-[10px] border p-3', highlight ? 'border-fg ring-1 ring-fg' : 'border-line')}>
      <div className="flex flex-wrap items-center gap-2">
        <EvidenceBadges status={e.status} relation={e.relation} missing={e.missing} />
        {!e.usable && <span className="text-xs text-danger">不可作为已证实依据</span>}
      </div>
      <div className="mt-1.5 text-xs text-muted">{sourceDomainLabel[e.source_domain] ?? e.source_domain} · 检索运行 #{e.run_id} · 证据 ID {e.id}</div>
      {e.reason && <p className="mt-1 text-sm">{e.reason}</p>}
      {onOpen && !e.missing && <Button size="sm" className="mt-2" icon={<ExternalLink className="size-3.5" />} onClick={onOpen}>查看来源</Button>}
    </div>
  )
}

function VerifyForm({ evidence, onVerified }: { evidence: RetrievalEvidence; onVerified: () => void }) {
  const [claim, setClaim] = useState<EvidenceClaim>({ text: '', parameter: '', operator: '<=', value: 0, unit: '秒', product: '', version: '' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const verify = async () => {
    setBusy(true); setError('')
    try { await api.verifyEvidence(evidence.run_id, evidence.id, claim); onVerified() } catch (e) { setError(errorText(e)) } finally { setBusy(false) }
  }
  return (
    <section className="space-y-2 rounded-[10px] border border-line p-3">
      <h4 className="text-sm font-semibold">数值指标核验</h4>
      <p className="text-xs text-muted">仅核验明确型号的标量指标（参数、阈值、单位）；型号、条件或表格存在歧义时保持待复核。核验结果不代表整项要求满足。</p>
      <div className="grid gap-2 sm:grid-cols-2">
        {(['text', 'parameter', 'product', 'version', 'unit'] as const).map((key) => (
          <Field key={key} label={{ text: '待核验陈述', parameter: '参数原名', product: '准确型号', version: '版本', unit: '阈值单位' }[key]}>
            <Input value={claim[key] ?? ''} onChange={(e) => setClaim({ ...claim, [key]: e.target.value })} />
          </Field>
        ))}
        <Field label="数值阈值"><Input type="number" value={claim.value} onChange={(e) => setClaim({ ...claim, value: Number(e.target.value) })} /></Field>
        <Field label="比较方式"><Select value={claim.operator} onChange={(e) => setClaim({ ...claim, operator: e.target.value })}>
          <option value="<=">不超过</option><option value=">=">不低于</option><option value="=">等于</option></Select></Field>
      </div>
      <Button loading={busy} disabled={evidence.status === 'stale' || !claim.text || !claim.parameter} onClick={() => void verify()}>核验指标</Button>
      <ErrorBanner error={error} />
    </section>
  )
}

/** EvidenceDetailDrawer 按需读取检索运行的证据，展示来源标题、解析片段、页码可靠性与核验结论。 */
export function EvidenceDetailDrawer({ runId, evidenceId, onClose, onChanged }: { runId?: number; evidenceId?: number; onClose: () => void; onChanged?: () => void }) {
  const open = !!runId && !!evidenceId
  const data = useAsync(() => api.retrievalEvidence(runId!), [runId], open)
  const e = data.data?.find((x) => x.id === evidenceId)
  return (
    <Drawer open={open} title={`证据 ${evidenceId ?? ''} · 检索运行 #${runId ?? ''}`} onClose={onClose} width={520}>
      <div className="space-y-3 p-4">
        <ErrorBanner error={data.error} onRetry={data.reload} />
        {data.loading && !data.data ? <Spinner /> : !e ? (data.data && <Empty>该检索运行中没有证据 {evidenceId}，可能已删除或不属于本项目。</Empty>) : (
          <>
            <div className="flex flex-wrap items-center gap-2"><EvidenceBadges status={e.status} relation={e.relation} /></div>
            <div>
              <div className="font-semibold">{e.snapshot.document?.title ?? '（来源标题缺失）'}</div>
              <div className="text-xs text-muted">
                {e.snapshot.chunk?.section_title && `${e.snapshot.chunk.section_title} · `}
                页码定位可靠性：{e.snapshot.page_reliability || '未标注'}
              </div>
            </div>
            {e.reason && <p className="text-sm">{e.reason}</p>}
            <div className="text-xs text-muted">来源：{sourceDomainLabel[e.source_domain ?? 'knowledge'] ?? e.source_domain} · 解析原文片段（不等于原始 PDF 版式）</div>
            {e.source_domain && e.source_domain !== 'knowledge' && !e.snapshot.chunk ? (
              <pre className="max-h-80 overflow-auto rounded-lg border border-line bg-soft p-3 text-xs leading-5 whitespace-pre-wrap">{pretty(e.snapshot)}</pre>
            ) : <>
              <pre className="max-h-80 overflow-auto rounded-lg border border-line bg-soft p-3 text-xs leading-5 whitespace-pre-wrap">{e.snapshot.chunk?.content || '（无正文）'}</pre>
              {e.snapshot.chunk?.tables?.map((t, i) => <TableView key={i} table={t} index={i} />)}
            </>}
            {e.claim?.verdict?.quote && <p className="text-sm">核验原文：{e.claim.verdict.quote}</p>}
            <VerifyForm evidence={e} onVerified={() => { void data.reload(); onChanged?.() }} />
          </>
        )}
      </div>
    </Drawer>
  )
}

/** 证据选择：候选来源为指定运行（要求绑定 / 已有证据）与本项目的检索运行列表，按需读取选中运行。 */
export function EvidencePicker({ taskId, preferredRuns, selected, onToggle, runId: fixedRun, onRunChange, onOpen }:
  { taskId: number; preferredRuns: number[]; selected: number[]; onToggle: (id: number, on: boolean) => void
    runId?: number; onRunChange?: (run: number | undefined) => void; onOpen: (run: number, id: number) => void }) {
  const runs = useAsync(() => api.taskRetrievalRuns(taskId), [taskId])
  const [localRun, setLocalRun] = useState<number | undefined>(fixedRun ?? preferredRuns[0])
  const run = fixedRun ?? localRun
  const setRun = (r: number | undefined) => { setLocalRun(r); onRunChange?.(r) }
  const ev = useAsync(() => api.retrievalEvidence(run!), [run], !!run)
  const options = useMemo(() => {
    const m = new Map<number, string>()
    for (const id of preferredRuns) m.set(id, `检索运行 #${id}（已绑定）`)
    for (const r of runs.data ?? []) m.set(r.id, `#${r.id} ${(r.request?.question ?? '知识库查询').slice(0, 40)} · ${r.status}${preferredRuns.includes(r.id) ? ' · 已绑定' : ''}`)
    return [...m.entries()]
  }, [preferredRuns, runs.data])
  const sel = new Set(selected)
  return (
    <div className="space-y-2">
      <Field label="检索运行">
        <Select value={run ?? ''} onChange={(e) => setRun(e.target.value ? Number(e.target.value) : undefined)}>
          <option value="">选择检索运行…</option>
          {options.map(([id, label]) => <option key={id} value={id}>{label}</option>)}
        </Select>
      </Field>
      <ErrorBanner error={runs.error} onRetry={runs.reload} />
      {options.length === 0 && runs.data && <p className="text-xs text-muted">本项目还没有检索运行。请先在「招标解读 → 证据检索」发起检索，或记录证据缺口。</p>}
      {run && (ev.loading && !ev.data ? <Spinner /> : ev.error ? <ErrorBanner error={ev.error} onRetry={ev.reload} /> : !ev.data?.length ? <Empty>该运行没有证据记录</Empty> : (
        <ul className="space-y-2">
          {ev.data.map((e) => (
            <li key={e.id} className={cx('rounded-[10px] border p-2.5', sel.has(e.id) ? 'border-fg bg-soft' : 'border-line')}>
              <label className="flex items-start gap-2">
                <input type="checkbox" className="mt-1" checked={sel.has(e.id)} onChange={(x) => onToggle(e.id, x.target.checked)}
                  aria-label={`选择证据 ${e.id}`} />
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-sm font-medium">{e.snapshot.document?.title ?? '（来源标题缺失）'}</span>
                  <span className="mt-0.5 block line-clamp-2 text-xs text-muted">{e.snapshot.chunk?.content?.slice(0, 160) || '（无正文片段）'}</span>
                  <span className="mt-1 flex flex-wrap items-center gap-1.5"><EvidenceBadges status={e.status} relation={e.relation} />
                    <span className="text-[11px] text-muted">ID {e.id}</span></span>
                </span>
              </label>
              <Button size="sm" variant="ghost" className="mt-1" onClick={() => onOpen(e.run_id, e.id)}>查看来源</Button>
            </li>
          ))}
        </ul>
      ))}
      <p className="text-xs text-muted">选择只修改当前草稿；保存时提交，不会批准证据。未核验、证据不足、矛盾或失效的证据会在保存后记为缺口，不作为已证实依据。</p>
    </div>
  )
}

export type { RetrievalRun }
