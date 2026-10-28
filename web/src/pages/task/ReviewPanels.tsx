// Phase 6A：任务页「技术标大纲与写作」下的「响应矩阵」「一致性核验」子视图与导出就绪度卡片。
// 全部数据来自 server/compliance.go 的真实路由；核验与失效判定都在后端，前端只展示与触发。

import { useState } from 'react'
import type { ReactNode } from 'react'
import { AlertTriangle, CheckCircle2, ClipboardCheck, RefreshCw, ScanSearch, ShieldAlert, ShieldCheck } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type {
  ComplianceRow, ConsistencyFinding, Deviation, ExportReadiness, RequirementComplianceStatus, RequirementKind, ResolutionKind, VerifyStatus,
} from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Modal, Select, Spinner, Stat, TextArea, cx, useToast } from '../../components/ui'
import type { Tone } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import {
  deviationLabel, exportBlockerLabel, findingStatusLabel, fmtPct, fmtTime, requirementKindLabel, resolutionKindLabel, verifyStatusLabel,
} from '../../lib/format'

const kinds: RequirementKind[] = ['star', 'triangle', 'scoring', 'required', 'format']
const statuses: VerifyStatus[] = ['failed', 'stale', 'pending', 'verified']

export function verifyTone(s: RequirementComplianceStatus | string): Tone {
  switch (s) {
    case 'verified': return 'green'
    case 'failed': case 'stale': case 'unmapped': return 'red'
    case 'pending': case 'unchecked': return 'amber'
    default: return 'slate'
  }
}

function deviationTone(d: Deviation): Tone {
  return d === 'negative' ? 'red' : d === 'positive' ? 'green' : 'slate'
}

function Chip({ active, onClick, children }: { active: boolean; onClick: () => void; children: ReactNode }) {
  return <button onClick={onClick} className={cx('rounded-full border px-2.5 py-0.5 text-xs',
    active ? 'border-blue-600 bg-blue-600 text-white' : 'border-slate-300 bg-white text-slate-600 hover:bg-slate-50')}>{children}</button>
}

// ---------------------------------------------------------------------------
// 导出就绪度（只读；发起导出在「⑥ 导出成稿」视图）
// ---------------------------------------------------------------------------

export function ExportReadinessCard({ taskID, tick }: { taskID: number; tick: number }) {
  const data = useAsync(() => api.exportReadiness(taskID), [taskID, tick])
  const r: ExportReadiness | undefined = data.data
  const groups = new Map<string, ExportReadiness['blockers']>()
  for (const b of r?.blockers ?? []) groups.set(b.kind, [...(groups.get(b.kind) ?? []), b])
  return (
    <Card title={<>导出就绪度 {r && (r.ready
      ? <Badge tone="green"><CheckCircle2 className="size-3" />无阻断项</Badge>
      : <Badge tone="red"><ShieldAlert className="size-3" />{r.blockers.length} 项阻止导出</Badge>)}
      {r && <span className="text-xs font-normal text-slate-400">大纲 v{r.outline_version || '—'} · 检查于 {fmtTime(r.checked_at)}</span>}</>}
      actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => void data.reload()} />}>
      <ErrorBanner error={data.error} onRetry={data.reload} />
      {!r ? <Spinner /> : r.ready ? (
        <p className="text-sm text-emerald-700">全部章节已批准当前修订，响应矩阵全部实质响应且未失效，最新一致性核验无待处理结论，证据均有效。可在「⑥ 导出成稿」锁定版本并导出 DOCX / PDF。</p>
      ) : (
        <div className="space-y-3">
          <div className="flex flex-wrap gap-1.5">
            {[...groups.entries()].map(([k, list]) => <Badge key={k} tone="red">{exportBlockerLabel[k] ?? k} {list.length}</Badge>)}
          </div>
          <ul className="max-h-64 space-y-1 overflow-y-auto text-sm">
            {r.blockers.map((b, i) => (
              <li key={i} className="flex gap-2 rounded border border-red-100 bg-red-50/50 px-2 py-1">
                <span className="shrink-0 text-xs font-medium text-red-700">{exportBlockerLabel[b.kind] ?? b.kind}</span>
                <span className="text-slate-700">{b.message}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </Card>
  )
}

// ---------------------------------------------------------------------------
// ④ 响应矩阵
// ---------------------------------------------------------------------------

export function MatrixPanel({ taskID, tick, onOpenSection }: { taskID: number; tick: number; onOpenSection: (id: number) => void }) {
  const toast = useToast()
  const [refresh, setRefresh] = useState(0)
  const [kind, setKind] = useState<'' | RequirementKind>('')
  const [status, setStatus] = useState<'' | VerifyStatus>('')
  const [busy, setBusy] = useState('')
  const [detail, setDetail] = useState<ComplianceRow>()
  const cov = useAsync(() => api.complianceCoverage(taskID), [taskID, tick, refresh])
  const matrix = useAsync(() => api.complianceMatrix(taskID, { kind, status }), [taskID, tick, refresh, kind, status])

  const verify = async (sectionID?: number) => {
    setBusy(sectionID ? `s${sectionID}` : 'all')
    try {
      const r = await api.verifyCompliance(taskID, sectionID)
      toast(r.stale ? 'info' : 'ok', `核验 ${r.checked} 行：通过 ${r.verified}，未通过 ${r.failed}，章节无正文 ${r.pending}${r.stale ? `，写入时已失效 ${r.stale}` : ''}`)
      setRefresh((n) => n + 1)
    } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }

  const c = cov.data
  const rows = matrix.data?.rows ?? []
  return (
    <div className="space-y-4">
      <ExportReadinessCard taskID={taskID} tick={tick + refresh} />
      {cov.loading && !c ? <Spinner /> : !c ? <ErrorBanner error={cov.error} onRetry={cov.reload} /> : (
        <>
          <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
            <Stat label="第一层 · 大纲映射率" value={fmtPct(c.mapping_rate)} hint={`${c.mapped}/${c.total} 条要求已映射${c.approved ? ` · 批准大纲 v${c.outline_version}` : ' · 尚无批准大纲'}`}
              tone={c.total && c.mapped === c.total ? 'green' : 'amber'} />
            <Stat label="第二层 · 正文实质响应率" value={fmtPct(c.response_rate)} hint={`${c.verified}/${c.total} 条要求通过核验`}
              tone={c.total && c.verified === c.total ? 'green' : c.verified ? 'amber' : 'red'} />
            <Stat label="负偏离" value={c.negative_deviations} tone={c.negative_deviations ? 'red' : undefined} />
            <Stat label="★/▲ 未通过" value={c.by_kind.filter((k) => k.kind === 'star' || k.kind === 'triangle').reduce((n, k) => n + k.total - k.verified, 0)}
              tone={c.by_kind.some((k) => (k.kind === 'star' || k.kind === 'triangle') && k.total > k.verified) ? 'red' : 'green'} />
          </div>
          <Card title="按要求类型的两层覆盖">
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead className="text-left text-xs text-slate-500">
                  <tr><th className="py-1 pr-3">类型</th><th className="pr-3">要求数</th><th className="pr-3">已映射</th><th className="pr-3">实质响应</th>
                    <th className="pr-3">未通过</th><th className="pr-3">已失效</th><th className="pr-3">章节无正文</th><th>未核验</th></tr>
                </thead>
                <tbody className="divide-y divide-slate-100">
                  {c.by_kind.map((k) => (
                    <tr key={k.kind}>
                      <td className="py-1 pr-3 font-medium">{requirementKindLabel[k.kind]}</td>
                      <td className="pr-3 tabular-nums">{k.total}</td>
                      <td className={cx('pr-3 tabular-nums', k.mapped < k.total && 'text-red-600')}>{k.mapped}</td>
                      <td className="pr-3 tabular-nums text-emerald-700">{k.verified}</td>
                      <td className={cx('pr-3 tabular-nums', k.failed > 0 && 'text-red-600')}>{k.failed}</td>
                      <td className={cx('pr-3 tabular-nums', k.stale > 0 && 'text-red-600')}>{k.stale}</td>
                      <td className="pr-3 tabular-nums">{k.pending}</td>
                      <td className="tabular-nums">{k.unchecked}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {c.requirements.some((r) => r.status === 'unmapped') && (
              <div className="mt-3 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-800">
                未映射到批准大纲的要求：{c.requirements.filter((r) => r.status === 'unmapped').map((r) => `${r.code}（${requirementKindLabel[r.kind]}）`).join('、')}
              </div>
            )}
          </Card>
        </>
      )}

      <Card title={<>逐条响应矩阵 {matrix.data && <span className="text-xs font-normal text-slate-400">
        大纲 v{matrix.data.outline_version || '—'}{matrix.data.outline_version > 0 && !matrix.data.approved && '（已被取代，全部待重检）'}</span>}</>}
        actions={<>
          <Button size="sm" variant="primary" icon={<ScanSearch className="size-3.5" />} loading={busy === 'all'} onClick={() => void verify()}>核验全部章节</Button>
          <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => setRefresh((n) => n + 1)} />
        </>}>
        <ErrorBanner error={matrix.error} />
        <div className="mb-3 flex flex-wrap items-center gap-1.5">
          <Chip active={!kind} onClick={() => setKind('')}>全部类型</Chip>
          {kinds.map((k) => <Chip key={k} active={kind === k} onClick={() => setKind(k)}>{requirementKindLabel[k]}</Chip>)}
          <span className="mx-1 h-4 w-px bg-slate-200" />
          <Chip active={!status} onClick={() => setStatus('')}>全部状态</Chip>
          {statuses.map((s) => <Chip key={s} active={status === s} onClick={() => setStatus(s)}>{verifyStatusLabel[s]}</Chip>)}
        </div>
        <p className="mb-2 text-xs text-slate-500">核验对象是各章节<b>当前有效修订</b>的正文；只挂章节名、只有引用或复述要求不算实质响应。规则核验的召回率 / 准确率未经人工标注评测，结论须人工复核。</p>
        {matrix.loading && !matrix.data ? <Spinner /> : rows.length === 0 ? <Empty>暂无矩阵记录。请在批准大纲并保存章节正文后点击「核验全部章节」。</Empty> : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-slate-500">
                <tr><th className="py-1 pr-3">要求</th><th className="pr-3">章节 / 修订</th><th className="pr-3">状态</th><th className="pr-3">偏离</th>
                  <th className="pr-3">响应摘要 / 原因</th><th /></tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {rows.map((r) => {
                  const bad = r.verify_status === 'stale' || r.verify_status === 'failed'
                  return (
                    <tr key={r.id} className={cx(bad && 'bg-red-50/60')}>
                      <td className="py-1.5 pr-3 align-top">
                        <div className="font-medium">{r.requirement_code} <Badge tone={r.kind === 'star' || r.kind === 'triangle' ? 'red' : 'slate'}>{requirementKindLabel[r.kind]}</Badge></div>
                        <div className="text-xs text-slate-500">{r.requirement_title}</div>
                      </td>
                      <td className="pr-3 align-top whitespace-nowrap">
                        <button className="text-blue-700 hover:underline" onClick={() => onOpenSection(r.section_id)}>{r.section_code} {r.section_title}</button>
                        <div className="text-xs text-slate-500">核验 rev {r.rev_no ?? '—'} · 当前 rev {r.current_rev_no}</div>
                      </td>
                      <td className="pr-3 align-top"><Badge tone={verifyTone(r.verify_status)}>{verifyStatusLabel[r.verify_status]}</Badge></td>
                      <td className="pr-3 align-top"><Badge tone={deviationTone(r.deviation)}>{deviationLabel[r.deviation]}</Badge></td>
                      <td className="max-w-md pr-3 align-top text-xs">
                        {r.verify_status === 'stale' ? <span className="text-red-700">{r.stale_reason}</span>
                          : r.failure_reason ? <span className="text-red-700">{r.failure_reason}</span>
                          : <span className="text-slate-700">{r.response_summary}</span>}
                        {r.anchor && <div className="text-slate-400">锚点 {r.anchor}</div>}
                      </td>
                      <td className="align-top whitespace-nowrap">
                        <Button size="sm" variant="ghost" onClick={() => setDetail(r)}>明细</Button>
                        <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3" />} loading={busy === `s${r.section_id}`}
                          onClick={() => void verify(r.section_id)}>重检</Button>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>
      <Modal open={!!detail} wide title={detail ? `${detail.requirement_code} × 章节 ${detail.section_code}` : ''} onClose={() => setDetail(undefined)}>
        {detail && (
          <div className="space-y-3 text-sm">
            <div className="flex flex-wrap gap-2">
              <Badge tone={verifyTone(detail.verify_status)}>{verifyStatusLabel[detail.verify_status]}</Badge>
              <Badge tone={deviationTone(detail.deviation)}>{deviationLabel[detail.deviation]}</Badge>
              <Badge>核验器 {detail.verifier}</Badge>
              <span className="text-xs text-slate-500">rev {detail.rev_no ?? '—'}（section_rev_id {detail.section_rev_id ?? '—'}）· {detail.verified_at ? fmtTime(detail.verified_at) : '未核验'} · {detail.updated_by || '—'}</span>
            </div>
            {detail.anchor_excerpt && <blockquote className="border-l-4 border-blue-200 bg-slate-50 px-3 py-2 text-slate-700">
              <div className="mb-1 text-xs text-slate-400">命中段落 {detail.anchor}</div>{detail.anchor_excerpt}</blockquote>}
            {detail.stale_reason && <p className="text-red-700">失效原因：{detail.stale_reason}</p>}
            {detail.failure_reason && <p className="text-red-700">未通过原因：{detail.failure_reason}</p>}
            {detail.evidence_ids.length > 0 && <p className="text-xs text-slate-500">引用证据：{detail.evidence_ids.join('、')}</p>}
            <ul className="space-y-1">
              {(detail.checks ?? []).map((ck, i) => (
                <li key={i} className="flex gap-2">
                  {ck.passed ? <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-emerald-600" /> : <AlertTriangle className="mt-0.5 size-4 shrink-0 text-red-500" />}
                  <span><b className="font-mono text-xs">{ck.name}</b> {ck.detail}</span>
                </li>
              ))}
            </ul>
          </div>
        )}
      </Modal>
    </div>
  )
}

// ---------------------------------------------------------------------------
// ⑤ 一致性核验
// ---------------------------------------------------------------------------

function Side({ code, title, rev, value, excerpt }: { code?: string; title?: string; rev?: number; value: string; excerpt: string }) {
  return (
    <div className="flex-1 rounded-md border border-slate-200 bg-white p-2">
      <div className="text-xs text-slate-500">章节 {code} {title} · rev {rev}</div>
      <div className="my-1 font-semibold text-slate-900">{value}</div>
      <div className="text-xs text-slate-600">「{excerpt}」</div>
    </div>
  )
}

export function ConsistencyPanel({ taskID, tick }: { taskID: number; tick: number }) {
  const toast = useToast()
  const [refresh, setRefresh] = useState(0)
  const [status, setStatus] = useState<'' | 'open' | 'resolved' | 'stale'>('')
  const [busy, setBusy] = useState('')
  const [resolving, setResolving] = useState<ConsistencyFinding>()
  const data = useAsync(() => api.consistencyFindings(taskID, { status }), [taskID, tick, refresh, status])

  const run = async (reverify: boolean) => {
    setBusy(reverify ? 'reverify' : 'run')
    try {
      if (reverify) await api.verifyCompliance(taskID)
      const d = await api.runConsistency(taskID)
      toast('ok', `一致性核验 #${d.run?.id}：矛盾 ${d.run?.contradictions ?? 0}，漏项 ${d.run?.omissions ?? 0}`)
      setRefresh((n) => n + 1)
    } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }

  const d = data.data
  const contradictions = d?.findings.filter((f) => f.kind === 'contradiction') ?? []
  const omissions = d?.findings.filter((f) => f.kind === 'omission') ?? []
  const statusTone = (s: string): Tone => (s === 'open' ? 'amber' : s === 'resolved' ? 'green' : 'red')
  const actions = (f: ConsistencyFinding) => f.status === 'open'
    ? <Button size="sm" icon={<ClipboardCheck className="size-3.5" />} onClick={() => setResolving(f)}>处理</Button>
    : f.status === 'resolved'
      ? <span className="text-xs text-slate-500">{resolutionKindLabel[f.resolution_kind] ?? f.resolution_kind} · {f.resolved_by || '—'} · {fmtTime(f.resolved_at)}{f.resolution_note && `：${f.resolution_note}`}</span>
      : <span className="text-xs text-red-700">{f.stale_reason}</span>

  return (
    <div className="space-y-4">
      <ExportReadinessCard taskID={taskID} tick={tick + refresh} />
      <Card title={<>全文一致性核验 {d?.run && <>
        <Badge tone={d.current ? 'green' : 'red'}>{d.current ? '当前快照' : '已过期·需重新核验'}</Badge>
        <span className="text-xs font-normal text-slate-400">#{d.run.id} · 大纲 v{d.run.outline_version} · {Object.keys(d.run.snapshot).length} 个章节 · {fmtTime(d.run.created_at)} · {d.run.created_by || '—'}</span></>}</>}
        actions={<>
          <Button size="sm" variant="primary" icon={<ShieldCheck className="size-3.5" />} loading={busy === 'reverify'} onClick={() => void run(true)}>重检矩阵并核验</Button>
          <Button size="sm" icon={<ScanSearch className="size-3.5" />} loading={busy === 'run'} onClick={() => void run(false)}>仅一致性核验</Button>
          <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => setRefresh((n) => n + 1)} />
        </>}>
        <ErrorBanner error={data.error} />
        <p className="mb-3 text-xs text-slate-500">
          比对当前批准大纲下各章节有效修订中的总工期、质保期、SLA（响应 / 到场 / 修复时限、可用性）、团队人数、核心型号与数量；
          ★/▲ 条款在响应矩阵中没有当前修订上的「实质响应」即记为漏项。规则抽取（含单位归一与中文数字）未经人工标注评测，无结论不代表全文一致。
        </p>
        {d?.run && d.run.stale_reason && !d.current && <div className="mb-3 flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" />{d.run.stale_reason}</div>}
        <div className="mb-3 flex flex-wrap gap-1.5">
          {(['', 'open', 'resolved', 'stale'] as const).map((s) => <Chip key={s} active={status === s} onClick={() => setStatus(s)}>{s ? findingStatusLabel[s] : '全部'}</Chip>)}
        </div>
        {data.loading && !d ? <Spinner /> : !d?.run ? <Empty>尚未执行一致性核验。</Empty> : (
          <div className="space-y-5">
            <section>
              <h4 className="mb-2 text-sm font-semibold text-slate-700">前后矛盾（{contradictions.length}）</h4>
              {contradictions.length === 0 ? <Empty>未检出矛盾。</Empty> : contradictions.map((f) => (
                <div key={f.id} className={cx('mb-2 rounded-md border p-3', f.status === 'open' ? 'border-amber-200 bg-amber-50/50' : 'border-slate-200 bg-slate-50')}>
                  <div className="mb-2 flex flex-wrap items-center gap-2">
                    <span className="font-medium">{f.metric_label}</span><Badge tone={statusTone(f.status)}>{findingStatusLabel[f.status]}</Badge>
                    <span className="ml-auto">{actions(f)}</span>
                  </div>
                  <div className="flex flex-col gap-2 md:flex-row">
                    <Side code={f.a_section_code} title={f.a_section_title} rev={f.a_rev_no} value={f.a_value} excerpt={f.a_excerpt} />
                    <div className="self-center text-xs font-semibold text-red-600">≠</div>
                    <Side code={f.b_section_code} title={f.b_section_title} rev={f.b_rev_no} value={f.b_value} excerpt={f.b_excerpt} />
                  </div>
                </div>
              ))}
            </section>
            <section>
              <h4 className="mb-2 text-sm font-semibold text-slate-700">★/▲ 漏项（{omissions.length}）</h4>
              {omissions.length === 0 ? <Empty>未检出漏项。</Empty> : (
                <ul className="divide-y divide-slate-100 rounded-md border border-slate-200">
                  {omissions.map((f) => (
                    <li key={f.id} className="flex flex-wrap items-start gap-2 px-3 py-2 text-sm">
                      <Badge tone="red">{f.requirement_kind ? requirementKindLabel[f.requirement_kind] : '漏项'}</Badge>
                      <span className="font-medium">{f.requirement_code}</span>
                      <span className="min-w-0 flex-1 text-slate-600">{f.detail}</span>
                      <Badge tone={statusTone(f.status)}>{findingStatusLabel[f.status]}</Badge>
                      {actions(f)}
                    </li>
                  ))}
                </ul>
              )}
            </section>
          </div>
        )}
      </Card>
      {resolving && <ResolveModal taskID={taskID} finding={resolving} onClose={() => setResolving(undefined)}
        onDone={() => { setResolving(undefined); setRefresh((n) => n + 1) }} />}
    </div>
  )
}

function ResolveModal({ taskID, finding, onClose, onDone }: { taskID: number; finding: ConsistencyFinding; onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const [kind, setKind] = useState<ResolutionKind>('fixed')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async () => {
    setBusy(true)
    try {
      await api.resolveFinding(taskID, finding.id, { version: finding.version, resolution_kind: kind, note })
      toast('ok', '结论已处理')
      onDone()
    } catch (e) { toast('error', errorText(e)) } finally { setBusy(false) }
  }
  return (
    <Modal open title={`处理结论：${finding.kind === 'contradiction' ? finding.metric_label : finding.requirement_code}`} onClose={onClose}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={() => void submit()}
        disabled={kind !== 'fixed' && !note.trim()}>确认处理</Button></>}>
      <div className="space-y-3">
        <p className="text-sm text-slate-600">{finding.detail}</p>
        <p className="text-xs text-slate-500">提示：修改章节正文并保存新修订后，该结论会自动失效，应重新核验，而不是在此标为已修正。处理带版本校验（v{finding.version}），他人已处理时会提示冲突。</p>
        <Field label="处理方式">
          <Select value={kind} onChange={(e) => setKind(e.target.value as ResolutionKind)}>
            {(['fixed', 'accepted', 'false_positive'] as const).map((k) => <option key={k} value={k}>{resolutionKindLabel[k]}</option>)}
          </Select>
        </Field>
        <Field label="说明" hint={kind === 'fixed' ? '可选' : '必填：说明为何不构成问题'}>
          <TextArea rows={3} value={note} onChange={(e) => setNote(e.target.value)} />
        </Field>
      </div>
    </Modal>
  )
}
