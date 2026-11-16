// 核验：响应矩阵与一致性核验的独立页面（原「④ 响应矩阵」「⑤ 一致性核验」）。
// 全部数据来自 server/compliance.go 的真实路由；核验与失效判定都在后端，前端只展示、触发与定位。
// 映射完成、实质响应通过、一致性无待处理问题是不同判据，分别展示。

import { useState } from 'react'
import type { ReactNode } from 'react'
import { AlertTriangle, CheckCircle2, ClipboardCheck, RefreshCw, ScanSearch, ShieldCheck } from 'lucide-react'
import { ApiError, api, errorText } from '../../../api/client'
import type {
  ComplianceRow, ConsistencyFinding, Deviation, RequirementComplianceStatus, RequirementKind, ResolutionKind, VerifyStatus,
} from '../../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, IconButton, Modal, Notice, Select, Spinner, Stat, TextArea, cx, useToast } from '../../../components/ui'
import type { Tone } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'
import { useScrollTo } from '../../../hooks/useScrollTo'
import {
  deviationLabel, findingStatusLabel, fmtPct, fmtTime, requirementKindLabel, resolutionKindLabel, verifyStatusLabel,
} from '../../../lib/format'
import { linkProps, useSearchParams } from '../../../lib/router'
import { optionalId, pickEnum, taskUrl } from '../../../lib/routes'
import BlockerList from '../../task-workspace/BlockerList'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'

const kinds: RequirementKind[] = ['star', 'triangle', 'scoring', 'required', 'format']
const STATUS_FILTERS = ['all', 'unmapped', 'unchecked', 'failed', 'stale', 'pending', 'verified', 'negative'] as const
type StatusFilter = (typeof STATUS_FILTERS)[number]
const statusFilterLabel: Record<StatusFilter, string> = {
  all: '全部状态', unmapped: '未映射', unchecked: '未核验', failed: '未通过', stale: '已失效', pending: '章节无正文', verified: '实质响应', negative: '负偏离',
}

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
  return <button type="button" aria-pressed={active} onClick={onClick} className={cx('h-7 rounded-lg border px-2 text-xs',
    active ? 'border-fg bg-fg text-white' : 'border-line hover:bg-soft')}>{children}</button>
}

/** 导出就绪度（只读）：阻断按类别分组并可跳到修复位置；发起导出在「成稿导出」页。 */
export function ExportReadinessCard() {
  const { taskId } = useTaskWorkspace()
  const tick = useTick('export', 'matrix', 'consistency', 'sections')
  const data = useAsync(() => api.exportReadiness(taskId), [taskId, tick])
  const r = data.data
  return (
    <Card title={<>导出就绪度 {r && (r.ready ? <Badge dot tone="green">无阻断项</Badge> : <Badge dot tone="red">{r.blockers.length} 项阻止导出</Badge>)}</>}
      subtitle={r ? `大纲 v${r.outline_version || '—'} · 检查于 ${fmtTime(r.checked_at)}` : undefined}
      actions={<IconButton label="刷新导出就绪度" onClick={() => void data.reload()}><RefreshCw className="size-4" /></IconButton>}>
      <ErrorBanner error={data.error} onRetry={data.reload} />
      {!r ? (!data.error && <Spinner />) : r.ready
        ? <p className="text-sm text-ok">全部章节已批准当前修订，响应矩阵全部实质响应且未失效，最新一致性核验无待处理结论，证据均有效。</p>
        : <details><summary className="cursor-pointer text-sm">查看 {r.blockers.length} 项阻断与处理入口</summary><div className="mt-3"><BlockerList taskId={taskId} blockers={r.blockers} /></div></details>}
    </Card>
  )
}

// ---------------------------------------------------------------------------
// 响应矩阵
// ---------------------------------------------------------------------------

export function MatrixPage() {
  const toast = useToast()
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('matrix', 'sections')
  const [params, setQuery] = useSearchParams()
  const kind = pickEnum<RequirementKind | 'all'>(params.get('kind'), ['all', ...kinds], 'all')
  const status = pickEnum(params.get('status'), STATUS_FILTERS, 'all')
  const focusReq = optionalId(params, 'requirement')
  const focusSec = optionalId(params, 'section')
  const [busy, setBusy] = useState('')
  const [detail, setDetail] = useState<ComplianceRow>()
  const cov = useAsync(() => api.complianceCoverage(taskId), [taskId, tick])
  const apiStatus = status === 'failed' || status === 'stale' || status === 'pending' || status === 'verified' ? status as VerifyStatus : undefined
  const matrix = useAsync(() => api.complianceMatrix(taskId, { kind: kind === 'all' ? undefined : kind, status: apiStatus }), [taskId, tick, kind, apiStatus])
  const focusRow = matrix.data?.rows.find((r) => (focusReq === undefined || r.requirement_item_id === focusReq) && (focusSec === undefined || r.section_id === focusSec))
  useScrollTo(focusRow ? `row-${focusRow.id}` : focusReq ? `unmapped-${focusReq}` : undefined, !!matrix.data && !!cov.data)

  const verify = async (sectionID?: number) => {
    setBusy(sectionID ? `s${sectionID}` : 'all')
    try {
      const r = await api.verifyCompliance(taskId, sectionID)
      toast(r.stale ? 'info' : 'ok', `核验 ${r.checked} 行：通过 ${r.verified}，未通过 ${r.failed}，章节无正文 ${r.pending}${r.stale ? `，写入时已失效 ${r.stale}` : ''}`)
      invalidate('matrix_verified')
    } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }

  const c = cov.data
  const unmapped = (c?.requirements ?? []).filter((r) => r.status === 'unmapped' && (kind === 'all' || r.kind === kind))
  const unchecked = (c?.requirements ?? []).filter((r) => r.status === 'unchecked' && (kind === 'all' || r.kind === kind))
  let rows = matrix.data?.rows ?? []
  if (status === 'negative') rows = rows.filter((r) => r.deviation === 'negative')
  const showRows = status !== 'unmapped' && status !== 'unchecked'
  const reqLink = (id: number, label: ReactNode) => <a {...linkProps(taskUrl(taskId, 'requirements', { query: { requirement: id } }))} className="hover:underline">{label}</a>

  return (
    <div className="mx-auto max-w-7xl space-y-4">
      <ExportReadinessCard />
      {cov.loading && !c ? <Spinner /> : !c ? <ErrorBanner error={cov.error} onRetry={cov.reload} /> : (
        <>
          <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
            <Stat label="第一层 · 大纲映射" value={c.total ? fmtPct(c.mapping_rate) : '—'} hint={`${c.mapped}/${c.total} 条要求已映射${c.approved ? ` · 批准大纲 v${c.outline_version}` : ' · 尚无批准大纲'}`}
              tone={!c.total ? undefined : c.mapped === c.total ? 'green' : 'amber'} />
            <Stat label="第二层 · 正文实质响应" value={c.total ? fmtPct(c.response_rate) : '—'} hint={`${c.verified}/${c.total} 条要求通过核验`}
              tone={!c.total ? undefined : c.verified === c.total ? 'green' : c.verified ? 'amber' : 'red'} />
            <Stat label="负偏离" value={c.negative_deviations} tone={c.negative_deviations ? 'red' : undefined} />
            <Stat label="★/▲ 未通过" value={c.by_kind.filter((k) => k.kind === 'star' || k.kind === 'triangle').reduce((n, k) => n + k.total - k.verified, 0)}
              tone={c.by_kind.some((k) => (k.kind === 'star' || k.kind === 'triangle') && k.total > k.verified) ? 'red' : c.total ? 'green' : undefined} />
          </div>
          <Card title="按要求类型的两层覆盖" subtitle="基准为全量经复核要求（排除价格分、不投分包与否决项），不以检索 Top-K 代替">
            <div className="overflow-x-auto">
              <table className="w-full min-w-[640px] text-sm">
                <thead className="text-left text-xs text-muted">
                  <tr>{['类型', '要求数', '已映射', '实质响应', '未通过', '已失效', '章节无正文', '未核验'].map((h) => <th key={h} className="py-1 pr-3 font-medium">{h}</th>)}</tr>
                </thead>
                <tbody className="divide-y divide-line-soft">
                  {c.by_kind.map((k) => (
                    <tr key={k.kind}>
                      <td className="py-1.5 pr-3 font-medium">{requirementKindLabel[k.kind]}</td>
                      <td className="pr-3 tabular-nums">{k.total}</td>
                      <td className={cx('pr-3 tabular-nums', k.mapped < k.total && 'text-danger')}>{k.mapped}</td>
                      <td className="pr-3 tabular-nums text-ok">{k.verified}</td>
                      <td className={cx('pr-3 tabular-nums', k.failed > 0 && 'text-danger')}>{k.failed}</td>
                      <td className={cx('pr-3 tabular-nums', k.stale > 0 && 'text-danger')}>{k.stale}</td>
                      <td className="pr-3 tabular-nums">{k.pending}</td>
                      <td className="tabular-nums">{k.unchecked}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
        </>
      )}

      <Card title={<>逐条响应矩阵 {matrix.data && <span className="text-xs font-normal text-muted">
        大纲 v{matrix.data.outline_version || '—'}{matrix.data.outline_version > 0 && !matrix.data.approved && '（已被取代，全部待重新核验）'}</span>}</>}
        actions={<>
          <Button size="sm" variant="primary" icon={<ScanSearch className="size-3.5" />} loading={busy === 'all'} onClick={() => void verify()}>核验全部章节</Button>
          <IconButton label="刷新响应矩阵" onClick={() => { void matrix.reload(); void cov.reload() }}><RefreshCw className="size-4" /></IconButton>
        </>}>
        <div className="space-y-3">
          <ErrorBanner error={matrix.error} />
          {(focusReq !== undefined || focusSec !== undefined) && matrix.data && !focusRow && !unmapped.some((r) => r.id === focusReq) &&
            <Notice tone="warn" action={<Button size="sm" onClick={() => setQuery({ requirement: undefined, section: undefined, kind: undefined, status: undefined })}>清除定位</Button>}>
              当前筛选下没有定位的矩阵行（要求 {String(params.get('requirement') ?? '—')} / 章节 {String(params.get('section') ?? '—')}）。</Notice>}
          <div className="flex flex-wrap items-center gap-1.5">
            <Chip active={kind === 'all'} onClick={() => setQuery({ kind: undefined })}>全部类型</Chip>
            {kinds.map((k) => <Chip key={k} active={kind === k} onClick={() => setQuery({ kind: k })}>{requirementKindLabel[k]}</Chip>)}
            <span className="mx-1 h-4 w-px bg-line" aria-hidden />
            {STATUS_FILTERS.map((s) => <Chip key={s} active={status === s} onClick={() => setQuery({ status: s === 'all' ? undefined : s })}>{statusFilterLabel[s]}</Chip>)}
          </div>
          <p className="text-xs text-muted">核验对象是各章节<b className="font-medium text-fg">当前有效修订</b>的正文；只挂章节名、只有引用或复述要求不算实质响应。规则核验的召回率 / 准确率未经人工标注评测，结论须人工复核。</p>
          {(status === 'all' || status === 'unmapped') && unmapped.length > 0 && (
            <Notice tone="danger"><div className="font-medium">未映射到批准大纲的要求（{unmapped.length}）</div>
              <ul className="mt-1 flex flex-wrap gap-x-3 gap-y-1 text-xs">{unmapped.map((r) => (
                <li key={r.id} id={`unmapped-${r.id}`} className={cx(focusReq === r.id && 'rounded bg-panel px-1 ring-1 ring-fg')}>
                  <a {...linkProps(taskUrl(taskId, 'outline', { query: { requirement: r.id } }))} className="underline">{r.code}（{requirementKindLabel[r.kind]}）</a></li>))}</ul></Notice>
          )}
          {status === 'unchecked' && (unchecked.length === 0 ? <Empty>没有未核验的要求</Empty> : (
            <ul className="divide-y divide-line-soft rounded-[10px] border border-line text-sm">{unchecked.map((r) => (
              <li key={r.id} className="flex flex-wrap items-center gap-2 px-3 py-2">{reqLink(r.id, <span className="font-medium">{r.code}</span>)}<span className="flex-1 text-muted">{r.title}</span>
                <span className="text-xs text-muted">章节 {r.section_codes.join('、') || '—'}</span></li>))}</ul>))}
          {showRows && (matrix.loading && !matrix.data ? <Spinner /> : rows.length === 0 ? <Empty>{status === 'all' ? '暂无矩阵记录。批准大纲并保存章节正文后点击「核验全部章节」。' : '当前筛选下没有矩阵行。'}</Empty> : (
            <div className="overflow-x-auto">
              <table className="w-full min-w-[880px] text-sm">
                <thead className="text-left text-xs text-muted">
                  <tr><th className="py-1 pr-3 font-medium">要求</th><th className="pr-3 font-medium">章节 / 修订</th><th className="pr-3 font-medium">状态</th><th className="pr-3 font-medium">偏离</th>
                    <th className="pr-3 font-medium">响应摘要 / 原因</th><th><span className="sr-only">操作</span></th></tr>
                </thead>
                <tbody className="divide-y divide-line-soft">
                  {rows.map((r) => {
                    const bad = r.verify_status === 'stale' || r.verify_status === 'failed'
                    return (
                      <tr key={r.id} id={`row-${r.id}`} className={cx(bad && 'bg-[#fef2f2]/60', focusRow?.id === r.id && 'outline outline-1 outline-fg')}>
                        <td className="py-1.5 pr-3 align-top">
                          <div className="font-medium">{reqLink(r.requirement_item_id, r.requirement_code)} <Badge tone={r.kind === 'star' || r.kind === 'triangle' ? 'red' : 'slate'}>{requirementKindLabel[r.kind]}</Badge></div>
                          <div className="text-xs text-muted">{r.requirement_title}</div>
                        </td>
                        <td className="pr-3 align-top whitespace-nowrap">
                          <a {...linkProps(taskUrl(taskId, 'writing', { sectionId: r.section_id }))} className="text-link hover:underline">{r.section_code} {r.section_title}</a>
                          <div className="text-xs text-muted">核验修订 {r.rev_no ?? '—'} · 当前修订 {r.current_rev_no}</div>
                        </td>
                        <td className="pr-3 align-top"><Badge dot tone={verifyTone(r.verify_status)}>{verifyStatusLabel[r.verify_status]}</Badge></td>
                        <td className="pr-3 align-top"><Badge tone={deviationTone(r.deviation)}>{deviationLabel[r.deviation]}</Badge></td>
                        <td className="max-w-md pr-3 align-top text-xs">
                          {r.verify_status === 'stale' ? <span className="text-danger">需要重新核验：{r.stale_reason}</span>
                            : r.failure_reason ? <span className="text-danger">{r.failure_reason}</span>
                            : <span>{r.response_summary}</span>}
                          {r.anchor && <div className="text-muted">锚点 {r.anchor}</div>}
                        </td>
                        <td className="align-top whitespace-nowrap">
                          <Button size="sm" variant="ghost" onClick={() => setDetail(r)}>明细</Button>
                          <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3" />} loading={busy === `s${r.section_id}`} onClick={() => void verify(r.section_id)}>重检</Button>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          ))}
        </div>
      </Card>
      <Modal open={!!detail} wide title={detail ? `${detail.requirement_code} × 章节 ${detail.section_code}` : ''} onClose={() => setDetail(undefined)}>
        {detail && (
          <div className="space-y-3 text-sm">
            <div className="flex flex-wrap gap-2">
              <Badge dot tone={verifyTone(detail.verify_status)}>{verifyStatusLabel[detail.verify_status]}</Badge>
              <Badge tone={deviationTone(detail.deviation)}>{deviationLabel[detail.deviation]}</Badge>
              <Badge>{detail.verifier === 'llm' ? '模型核验' : '规则核验'}</Badge>
              <span className="text-xs text-muted">修订 {detail.rev_no ?? '—'} · {detail.verified_at ? fmtTime(detail.verified_at) : '未核验'} · {detail.updated_by || '—'}</span>
            </div>
            {detail.anchor_excerpt && <blockquote className="border-l-4 border-line bg-soft px-3 py-2">
              <div className="mb-1 text-xs text-muted">命中段落 {detail.anchor}</div>{detail.anchor_excerpt}</blockquote>}
            {detail.stale_reason && <p className="text-danger">失效原因：{detail.stale_reason}</p>}
            {detail.failure_reason && <p className="text-danger">未通过原因：{detail.failure_reason}</p>}
            {detail.evidence_ids.length > 0 && <p className="text-xs text-muted">引用证据：{detail.evidence_ids.join('、')}（<a {...linkProps(taskUrl(taskId, 'writing', { sectionId: detail.section_id, query: { panel: 'evidence' } }))} className="text-link underline">在章节中查看</a>）</p>}
            <ul className="space-y-1">
              {(detail.checks ?? []).map((ck, i) => (
                <li key={i} className="flex gap-2">
                  {ck.passed ? <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-ok" aria-label="通过" /> : <AlertTriangle className="mt-0.5 size-4 shrink-0 text-danger" aria-label="未通过" />}
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
// 一致性核验
// ---------------------------------------------------------------------------

function Side({ taskId, id, code, title, rev, value, excerpt }: { taskId: number; id?: number; code?: string; title?: string; rev?: number; value: string; excerpt: string }) {
  return (
    <div className="min-w-0 flex-1 rounded-[10px] border border-line bg-panel p-2">
      <div className="text-xs text-muted">{id ? <a {...linkProps(taskUrl(taskId, 'writing', { sectionId: id }))} className="text-link hover:underline">章节 {code} {title}</a> : `章节 ${code ?? '—'} ${title ?? ''}`} · 修订 {rev ?? '—'}</div>
      <div className="my-1 font-semibold">{value}</div>
      <div className="text-xs text-muted">「{excerpt}」</div>
    </div>
  )
}

const resolutionMeaning: Record<ResolutionKind, string> = {
  fixed: '已修正：正文已改正。通常应修改章节并保存新修订，旧结论会自动失效后重新核验，而不是在此直接标记。',
  accepted: '接受风险：人工确认后保留差异，矛盾并未被客观消除，须说明原因。',
  false_positive: '误报：规则抽取误判，两处表述实际不构成矛盾，须说明原因。',
}

export function ConsistencyPage() {
  const toast = useToast()
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('consistency', 'sections')
  const [params, setQuery] = useSearchParams()
  const status = pickEnum(params.get('status'), ['', 'open', 'resolved', 'stale'] as const, '')
  const runParam = optionalId(params, 'run')
  const focus = optionalId(params, 'finding')
  const [busy, setBusy] = useState('')
  const [resolving, setResolving] = useState<ConsistencyFinding>()
  const runId = runParam && !Number.isNaN(runParam) ? runParam : undefined
  const data = useAsync(() => api.consistencyFindings(taskId, { status: status || undefined, run_id: runId }), [taskId, tick, status, runId])
  const d = data.data
  useScrollTo(focus ? `finding-${focus}` : undefined, !!d)

  const run = async (reverify: boolean) => {
    setBusy(reverify ? 'reverify' : 'run')
    try {
      if (reverify) await api.verifyCompliance(taskId)
      const r = await api.runConsistency(taskId)
      toast('ok', `一致性核验 #${r.run?.id}：矛盾 ${r.run?.contradictions ?? 0}，与承诺台账不一致 ${r.run?.ledger_mismatches ?? 0}，漏项 ${r.run?.omissions ?? 0}`)
      setQuery({ run: undefined, finding: undefined })
      invalidate(reverify ? ['matrix', 'consistency', 'business', 'export'] : 'consistency_changed')
    } catch (e) { toast('error', errorText(e)) } finally { setBusy('') }
  }

  const contradictions = d?.findings.filter((f) => f.kind === 'contradiction') ?? []
  const omissions = d?.findings.filter((f) => f.kind === 'omission') ?? []
  const ledger = d?.findings.filter((f) => f.kind === 'ledger_mismatch') ?? []
  const ledgerCount = d?.run?.summary?.ledger_commitments ?? 0
  const uncomparable = d?.run?.summary?.ledger_uncomparable ?? []
  const fTone = (s: string): Tone => (s === 'open' ? 'amber' : s === 'resolved' ? 'green' : 'red')
  const actions = (f: ConsistencyFinding) => f.status === 'open'
    ? <Button size="sm" icon={<ClipboardCheck className="size-3.5" />} onClick={() => setResolving(f)}>处理</Button>
    : f.status === 'resolved'
      ? <span className="text-xs text-muted">{resolutionKindLabel[f.resolution_kind] ?? f.resolution_kind} · {f.resolved_by || '—'} · {fmtTime(f.resolved_at)}{f.resolution_note && `：${f.resolution_note}`}</span>
      : <span className="text-xs text-danger">已失效：{f.stale_reason}</span>
  const focused = focus !== undefined && d?.findings.some((f) => f.id === focus)

  return (
    <div className="mx-auto max-w-7xl space-y-4">
      <ExportReadinessCard />
      <Card title={<>全文一致性核验 {d?.run && <Badge dot tone={d.current ? 'green' : 'red'}>{d.current ? '适用于当前正文' : '已过期 · 需重新核验'}</Badge>}</>}
        subtitle={d?.run ? `运行 #${d.run.id} · 大纲 v${d.run.outline_version} · ${Object.keys(d.run.snapshot).length} 个章节修订 · ${fmtTime(d.run.created_at)} · ${d.run.created_by || '—'}` : undefined}
        actions={<>
          <Button size="sm" variant="primary" icon={<ShieldCheck className="size-3.5" />} loading={busy === 'reverify'} onClick={() => void run(true)}>重检矩阵并核验</Button>
          <Button size="sm" icon={<ScanSearch className="size-3.5" />} loading={busy === 'run'} onClick={() => void run(false)}>仅一致性核验</Button>
          <IconButton label="刷新一致性结论" onClick={() => void data.reload()}><RefreshCw className="size-4" /></IconButton>
        </>}>
        <div className="space-y-3">
          <ErrorBanner error={data.error} onRetry={data.reload} />
          {runId && <Notice tone="info" action={<Button size="sm" onClick={() => setQuery({ run: undefined })}>查看最新运行</Button>}>正在查看核验运行 #{runId}，它可能不是最新一次。</Notice>}
          {focus !== undefined && d && !focused && <Notice tone="warn">当前筛选下没有结论 #{String(params.get('finding'))}；它可能已处理、已失效或属于其他运行。</Notice>}
          <p className="text-xs text-muted">只比较同一指标的数值（总工期、质保期、SLA、团队人数、核心型号与数量等），不理解分阶段 / 分子系统语境；正文里的承诺另与已确认的承诺台账逐处比对；★/▲ 条款在响应矩阵中没有当前修订上的实质响应即记为漏项。规则抽取未经人工标注评测，无结论不代表全文一致。</p>
          {d?.run && d.run.stale_reason && !d.current && <Notice>{d.run.stale_reason}</Notice>}
          <div className="flex flex-wrap gap-1.5">
            {(['', 'open', 'resolved', 'stale'] as const).map((s) => <Chip key={s} active={status === s} onClick={() => setQuery({ status: s || undefined })}>{s ? findingStatusLabel[s] : '全部'}</Chip>)}
          </div>
          {data.loading && !d ? <Spinner /> : !d?.run ? <Empty>尚未执行一致性核验。</Empty> : (
            <div className="space-y-5">
              <section aria-label="与承诺台账不一致">
                <h3 className="mb-1 text-sm font-semibold">与承诺台账不一致（{ledger.length}）</h3>
                <p className="mb-2 text-xs text-muted">
                  {ledgerCount > 0
                    ? `本次以 ${ledgerCount} 条已确认的承诺为基准。承诺台账是先定下来、经交付确认的基线：不一致时改正文，不要改台账去迁就正文。`
                    : <>本项目还没有已确认的承诺台账，这一项没有可比对的基准。到<a {...linkProps(taskUrl(taskId, 'evidence'))} className="underline">证据与承诺</a>录入并确认后重新核验。</>}
                  {uncomparable.length > 0 && ` 另有 ${uncomparable.length} 条台账的承诺值无法换算成数值，未参与比对：${uncomparable.join('、')}。`}
                </p>
                {ledger.length === 0 ? (ledgerCount > 0 && <Empty>当前筛选下没有与台账不一致的结论。</Empty>) : ledger.map((f) => (
                  <div key={f.id} id={`finding-${f.id}`} className={cx('mb-2 rounded-[10px] border p-3', f.status === 'open' ? 'border-[#fde68a] bg-[#fffbeb]/60' : 'border-line bg-soft', focus === f.id && 'ring-1 ring-fg')}>
                    <div className="mb-2 flex flex-wrap items-center gap-2">
                      <span className="font-medium">{f.metric_label}</span><Badge dot tone={fTone(f.status)}>{findingStatusLabel[f.status]}</Badge>
                      <span className="ml-auto">{actions(f)}</span>
                    </div>
                    <div className="flex flex-col gap-2 md:flex-row">
                      <Side taskId={taskId} id={f.a_section_id} code={f.a_section_code} title={f.a_section_title} rev={f.a_rev_no} value={f.a_value} excerpt={f.a_excerpt} />
                      <div className="self-center text-xs font-semibold text-danger" aria-label="不一致">≠</div>
                      <div className="min-w-0 flex-1 rounded-[10px] border border-line bg-panel p-2 text-sm">
                        <div className="mb-1 flex flex-wrap items-center gap-2 text-xs text-muted">
                          <a {...linkProps(taskUrl(taskId, 'evidence'))} className="font-medium text-fg hover:underline">承诺台账 {f.commitment_code || `#${f.commitment_id}`}</a>
                          {f.commitment_version && <span>v{f.commitment_version}</span>}
                        </div>
                        <div className="font-medium">{f.b_value}</div>
                      </div>
                    </div>
                  </div>
                ))}
              </section>
              <section>
                <h3 className="mb-2 text-sm font-semibold">前后矛盾（{contradictions.length}）</h3>
                {contradictions.length === 0 ? <Empty>当前筛选下没有矛盾结论。</Empty> : contradictions.map((f) => (
                  <div key={f.id} id={`finding-${f.id}`} className={cx('mb-2 rounded-[10px] border p-3', f.status === 'open' ? 'border-[#fde68a] bg-[#fffbeb]/60' : 'border-line bg-soft', focus === f.id && 'ring-1 ring-fg')}>
                    <div className="mb-2 flex flex-wrap items-center gap-2">
                      <span className="font-medium">{f.metric_label}</span><Badge dot tone={fTone(f.status)}>{findingStatusLabel[f.status]}</Badge>
                      <span className="ml-auto">{actions(f)}</span>
                    </div>
                    <div className="flex flex-col gap-2 md:flex-row">
                      <Side taskId={taskId} id={f.a_section_id} code={f.a_section_code} title={f.a_section_title} rev={f.a_rev_no} value={f.a_value} excerpt={f.a_excerpt} />
                      <div className="self-center text-xs font-semibold text-danger" aria-label="不一致">≠</div>
                      <Side taskId={taskId} id={f.b_section_id} code={f.b_section_code} title={f.b_section_title} rev={f.b_rev_no} value={f.b_value} excerpt={f.b_excerpt} />
                    </div>
                  </div>
                ))}
              </section>
              <section>
                <h3 className="mb-2 text-sm font-semibold">★/▲ 漏项（{omissions.length}）</h3>
                {omissions.length === 0 ? <Empty>当前筛选下没有漏项结论。</Empty> : (
                  <ul className="divide-y divide-line-soft rounded-[10px] border border-line">
                    {omissions.map((f) => (
                      <li key={f.id} id={`finding-${f.id}`} className={cx('flex flex-wrap items-start gap-2 px-3 py-2 text-sm', focus === f.id && 'bg-soft ring-1 ring-fg')}>
                        <Badge tone="red">{f.requirement_kind ? requirementKindLabel[f.requirement_kind] : '漏项'}</Badge>
                        {f.requirement_item_id
                          ? <a {...linkProps(taskUrl(taskId, 'matrix', { query: { requirement: f.requirement_item_id } }))} className="font-medium hover:underline">{f.requirement_code}</a>
                          : <span className="font-medium">{f.requirement_code}</span>}
                        <span className="min-w-0 flex-1 text-muted">{f.detail}</span>
                        <Badge dot tone={fTone(f.status)}>{findingStatusLabel[f.status]}</Badge>
                        {actions(f)}
                      </li>
                    ))}
                  </ul>
                )}
              </section>
            </div>
          )}
        </div>
      </Card>
      {resolving && <ResolveModal taskId={taskId} finding={resolving} onClose={() => setResolving(undefined)}
        onDone={() => { setResolving(undefined); invalidate('consistency_changed') }} onStale={() => { setResolving(undefined); void data.reload() }} />}
    </div>
  )
}

function ResolveModal({ taskId, finding, onClose, onDone, onStale }: { taskId: number; finding: ConsistencyFinding; onClose: () => void; onDone: () => void; onStale: () => void }) {
  const toast = useToast()
  const [kind, setKind] = useState<ResolutionKind>('accepted')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const submit = async () => {
    setBusy(true); setError('')
    try {
      await api.resolveFinding(taskId, finding.id, { version: finding.version, resolution_kind: kind, note })
      toast('ok', `结论已标记为「${resolutionKindLabel[kind]}」`)
      onDone()
    } catch (e) {
      // 409：结论已被他人处理或已失效；读取服务器当前结果，不自动用最新版本重放原决策。
      if (e instanceof ApiError && e.status === 409) { toast('error', `结论已变更或已失效，未提交本次处理：${e.message}`); onStale() }
      else setError(errorText(e))
    } finally { setBusy(false) }
  }
  return (
    <Modal open title={`处理结论：${finding.kind === 'omission' ? finding.requirement_code : finding.metric_label}`} onClose={onClose} dirty={!!note.trim()}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={() => void submit()} disabled={kind !== 'fixed' && !note.trim()}>确认处理</Button></>}>
      <div className="space-y-3">
        <p className="text-sm">{finding.detail}</p>
        <Field label="处理方式">
          <Select value={kind} onChange={(e) => setKind(e.target.value as ResolutionKind)}>
            {(['accepted', 'false_positive', 'fixed'] as const).map((k) => <option key={k} value={k}>{resolutionKindLabel[k]}</option>)}
          </Select>
        </Field>
        <Notice tone="info">{resolutionMeaning[kind]}</Notice>
        <Field label="说明" hint={kind === 'fixed' ? '可选' : '必填：说明为何保留或为何不构成问题'}>
          <TextArea rows={3} value={note} onChange={(e) => setNote(e.target.value)} />
        </Field>
        <p className="text-xs text-muted">提交带版本校验（v{finding.version}）；他人已处理或结论已失效时会提示冲突，并重新读取服务器结果。</p>
        <ErrorBanner error={error} />
      </div>
    </Modal>
  )
}
