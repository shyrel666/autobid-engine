import { useEffect, useState } from 'react'
import { api, errorText } from '../../../api/client'
import type {
  AdmissibilityRun, BidQuote, BidQuoteValidation, Commitment, ExportReadiness, ObjectiveScoreRun,
  RequirementItem, CompanyPerformance, CompanyCredential,
} from '../../../api/types'
import { Button, ErrorBanner, Spinner } from '../../../components/ui'
import { linkProps } from '../../../lib/router'
import { taskUrl } from '../../../lib/routes'
import { OperatorField } from '../../task-workspace/OperatorField'
import { EvidenceInputs } from './EvidenceInputs'

// D2 最小 UI：企业证据普查、要求级可采性、承诺台账、报价勾稽与导出阻断链接。
// 浏览器端到端验收另行执行；本页与 server/d2.go 的真实路由保持一致。

export default function D2EvidencePage({ taskId }: { taskId: number }) {
  const [requirements, setRequirements] = useState<RequirementItem[]>([])
  const [runs, setRuns] = useState<AdmissibilityRun[]>([])
  const [commitments, setCommitments] = useState<Commitment[]>([])
  const [quotes, setQuotes] = useState<BidQuote[]>([])
  const [scores, setScores] = useState<ObjectiveScoreRun[]>([])
  const [performance, setPerformance] = useState<CompanyPerformance[]>([])
  const [credentials, setCredentials] = useState<CompanyCredential[]>([])
  const [readiness, setReadiness] = useState<ExportReadiness | null>(null)
  const [validation, setValidation] = useState<BidQuoteValidation | null>(null)
  const [error, setError] = useState('')
  const [message, setMessage] = useState('')
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)

  useEffect(() => {
    let alive = true
    setLoading(true)
    Promise.all([
      api.requirements(taskId), api.listAdmissibility(taskId), api.listCommitments(taskId),
      api.listQuotes(taskId), api.listObjectiveScores(taskId), api.listPerformance(), api.exportReadiness(taskId),
      api.listCredentials({ limit: 500 }),
    ]).then(([reqs, ads, cmts, qts, obs, perf, rd, creds]) => {
      if (!alive) return
      setRequirements((reqs.items ?? []).filter((r) => !r.superseded_by_id))
      setRuns(ads ?? [])
      setCommitments(cmts ?? [])
      setQuotes(qts ?? [])
      setScores(obs ?? [])
      setPerformance(perf ?? [])
      setCredentials(creds ?? [])
      setReadiness(rd)
      setError('')
    }).catch((e) => { if (alive) setError(errorText(e)) })
      .finally(() => { if (alive) setLoading(false) })
    return () => { alive = false }
  }, [taskId, tick])

  const reload = (msg = '') => { setMessage(msg); setTick((x) => x + 1) }
  const act = async (fn: () => Promise<unknown>, ok: string) => {
    setError('')
    try { await fn(); reload(ok) } catch (e) { setError(errorText(e)) }
  }
  const currentRun = (reqId: number) => runs.find((r) => r.requirement_item_id === reqId && r.status === 'current')

  return (
    <div className="mx-auto max-w-7xl space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">D2 证据、可采性与承诺台账</h1>
          <p className="text-sm text-muted">证据必须可采；manual 不是通过；承诺台账未确认或被修改都会阻断导出。</p>
        </div>
        <OperatorField />
      </div>
      {error && <ErrorBanner error={error} />}
      {message && <div className="rounded-xl border border-line bg-panel px-3 py-2 text-sm">{message}</div>}
      {loading && <Spinner />}
      <EvidenceInputs taskId={taskId} requirements={requirements} credentials={credentials} performance={performance} onSaved={reload} />

      <section className="rounded-2xl border border-line bg-panel p-4">
        <div className="flex items-center justify-between gap-3">
          <h2 className="font-medium">导出就绪</h2>
          <a {...linkProps(taskUrl(taskId, 'export'))} className="text-link text-sm hover:underline">前往导出</a>
        </div>
        {readiness ? (
          readiness.ready ? <p className="mt-2 text-sm text-ok">Ready：无 D2 阻断。</p> : (
            <ul className="mt-2 space-y-1 text-sm text-danger">
              {readiness.blockers.map((b, i) => <li key={`${b.kind}-${i}`}>{b.kind}：{b.message}</li>)}
            </ul>
          )
        ) : <p className="mt-2 text-sm text-muted">尚未读取。</p>}
      </section>

      <section className="rounded-2xl border border-line bg-panel p-4">
        <h2 className="font-medium">要求级可采性</h2>
        <table className="mt-3 w-full text-sm">
          <thead className="text-left text-muted"><tr><th className="py-2">要求</th><th>当前结论</th><th>逐证据</th><th></th></tr></thead>
          <tbody>
            {requirements.map((r) => {
              const run = currentRun(r.id)
              return (
                <tr key={r.id} className="border-t border-line">
                  <td className="py-2 pr-3">{r.code} {r.title}</td>
                  <td>{run ? <span className={run.verdict === 'admissible' ? 'text-ok' : 'text-danger'}>{run.verdict}/{run.status}</span> : <span className="text-muted">未评估</span>}</td>
                  <td className="text-muted">
                    {run?.items?.map((it) => `${it.evidence_domain}:${it.evidence_id}=${it.verdict}`).join('；') || '-'}
                    {[...new Set([...(run?.reasons ?? []), ...(run?.items ?? []).flatMap((it) => it.reasons)].map((x) => x.message))].map((m) => <p key={m} className="text-xs text-danger">{m}</p>)}
                  </td>
                  <td className="text-right">
                    <Button size="sm" onClick={() => act(() => api.evaluateAdmissibility(taskId, r.id), `已评估 ${r.code}`)}>评估</Button>
                    {run?.verdict === 'manual' && !run.items?.some((it) => it.verdict === 'manual') && (
                      <span className="ml-2 text-xs text-muted">未绑定证据，先绑定再评估</span>
                    )}
                    {run?.verdict === 'manual' && run.items?.some((it) => it.verdict === 'manual') && (
                      <Button size="sm" variant="primary" className="ml-2" onClick={() => {
                        const manual = (run.items ?? []).filter((it) => it.verdict === 'manual')
                        const reason = window.prompt('人工复核理由（将记录操作人）')
                        if (!reason) return
                        void act(() => api.resolveAdmissibility(taskId, run.id, manual.map((it) => ({ item_id: it.id, verdict: 'admissible' as const, reason }))), `已人工复核 ${r.code}`)
                      }}>人工复核</Button>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </section>

      <section className="rounded-2xl border border-line bg-panel p-4">
        <h2 className="font-medium">承诺台账</h2>
        <table className="mt-3 w-full text-sm">
          <thead className="text-left text-muted"><tr><th className="py-2">编号</th><th>指标</th><th>阈值</th><th>承诺</th><th>状态</th><th></th></tr></thead>
          <tbody>
            {commitments.map((c) => (
              <tr key={c.id} className="border-t border-line">
                <td className="py-2 pr-3">{c.code}</td>
                <td>{c.metric_label}</td>
                <td>{c.threshold_cmp} {c.tender_threshold_raw}</td>
                <td>{c.committed_raw} {c.unit}</td>
                <td>{c.status}{c.signed_off_by ? `（${c.signed_off_by}）` : ''}</td>
                <td className="text-right">
                  {c.status === 'draft' && <Button size="sm" onClick={() => act(() => api.confirmCommitment(taskId, c.code), `已确认 ${c.code}`)}>确认</Button>}
                  {c.status !== 'draft' && c.status !== 'rejected' && !c.signed_off_by && (
                    <Button size="sm" className="ml-2" onClick={() => {
                      const note = window.prompt('会签说明（会签人须与创建人不同）')
                      if (note === null) return
                      void act(() => api.signOffCommitment(taskId, c.code, note), `已会签 ${c.code}`)
                    }}>会签</Button>
                  )}
                </td>
              </tr>
            ))}
            {commitments.length === 0 && <tr><td className="py-3 text-muted" colSpan={6}>尚无承诺台账。</td></tr>}
          </tbody>
        </table>
      </section>

      <section className="rounded-2xl border border-line bg-panel p-4">
        <h2 className="font-medium">报价勾稽</h2>
        <table className="mt-3 w-full text-sm">
          <thead className="text-left text-muted"><tr><th className="py-2">版本</th><th>声明总额</th><th>限价</th><th>状态</th><th></th></tr></thead>
          <tbody>
            {quotes.map((q) => (
              <tr key={q.id} className="border-t border-line">
                <td className="py-2 pr-3">v{q.version}</td>
                <td>{q.declared_total} {q.currency}</td>
                <td>{q.ceiling ?? '-'}</td>
                <td>{q.status}</td>
                <td className="text-right">
                  <Button size="sm" onClick={() => act(async () => setValidation(await api.validateQuote(taskId, q.id)), `已校验 v${q.version}`)}>校验</Button>
                  <Button size="sm" variant="primary" className="ml-2" onClick={() => act(() => api.confirmQuote(taskId, q.id), `已确认 v${q.version}`)}>确认</Button>
                </td>
              </tr>
            ))}
            {quotes.length === 0 && <tr><td className="py-3 text-muted" colSpan={5}>尚无报价。</td></tr>}
          </tbody>
        </table>
        {validation && (
          <ul className="mt-2 space-y-1 text-sm">
            {validation.checks.map((c, i) => <li key={`${c.rule}-${i}`} className={c.status === 'pass' ? 'text-ok' : 'text-danger'}>{c.rule}：{c.status} {c.detail}</li>)}
          </ul>
        )}
      </section>

      <section className="rounded-2xl border border-line bg-panel p-4">
        <div className="flex items-center justify-between gap-3">
          <h2 className="font-medium">客观分与业绩证据</h2>
          <Button size="sm" onClick={() => act(() => api.computeObjectiveScores(taskId), '客观分已计算')}>计算客观分</Button>
        </div>
        <table className="mt-3 w-full text-sm">
          <thead className="text-left text-muted"><tr><th className="py-2">评分项</th><th>状态</th><th>得分</th><th>可采证据</th><th>缺口</th></tr></thead>
          <tbody>
            {scores.map((s) => (
              <tr key={s.id} className="border-t border-line">
                <td className="py-2 pr-3">{s.requirement_title}</td><td>{s.status}</td>
                <td>{s.score}/{s.max_score}</td><td>{s.evidence_ids.length}</td><td>{s.gaps.join(',') || '-'}</td>
              </tr>
            ))}
            {scores.length === 0 && <tr><td className="py-3 text-muted" colSpan={5}>尚未计算客观分。</td></tr>}
          </tbody>
        </table>
        <table className="mt-4 w-full text-sm">
          <thead className="text-left text-muted"><tr><th className="py-2">证据编号</th><th>名称</th><th>主体</th><th>主体类型</th><th>有效期</th></tr></thead>
          <tbody>
            {credentials.map((c) => (
              <tr key={c.id} className="border-t border-line">
                <td className="py-2 pr-3">{c.code || c.id}</td><td>{c.title}</td>
                <td>{c.subject_name ?? '-'}</td><td>{c.subject_type ?? '-'}</td><td>{c.valid_until ?? '-'}</td>
              </tr>
            ))}
            {credentials.length === 0 && <tr><td className="py-3 text-muted" colSpan={5}>尚无企业证据记录。</td></tr>}
          </tbody>
        </table>
        <table className="mt-4 w-full text-sm">
          <thead className="text-left text-muted"><tr><th className="py-2">业绩编号</th><th>项目</th><th>主体</th><th>验收日期</th><th>范围</th></tr></thead>
          <tbody>
            {performance.map((p) => (
              <tr key={p.id} className="border-t border-line">
                <td className="py-2 pr-3">{p.code}</td><td>{p.project_name}</td><td>{p.subject_name}</td>
                <td>{p.acceptance_date ?? '-'}</td><td>{p.scope ?? '-'}</td>
              </tr>
            ))}
            {performance.length === 0 && <tr><td className="py-3 text-muted" colSpan={5}>尚无业绩证据。</td></tr>}
          </tbody>
        </table>
      </section>
    </div>
  )
}
