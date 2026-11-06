import { useRef, useState } from 'react'
import { api } from '../../api/client'
import type { ResearchRequest } from '../../api/types'
import { useAsync, useInterval } from '../../hooks/useAsync'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Spinner, TextArea } from '../../components/ui'
import { RetrievalEvidence } from '../../components/RetrievalEvidence'
import { DegradedNotice, SemanticStatusBar, useRetrievalStatus } from '../../features/retrieval/SemanticStatus'
import { canResumeResearch, degradationSummary, researchGapText, retrievalErrorText, runStatusLabel, runStatusTone } from '../../features/retrieval/retrieval-status'

export default function ResearchTab({ taskID }: { taskID: number }) {
  return <ResearchWorkspace key={taskID} taskID={taskID} />
}

function ResearchWorkspace({ taskID }: { taskID: number }) {
  const retrieval = useRetrievalStatus()
  const runs = useAsync(() => api.taskRetrievalRuns(taskID), [taskID])
  useInterval(() => { if (!runs.loading) void runs.reload() }, runs.data?.some((run) => run.status === 'running') ? 3000 : null)
  const [question, setQuestion] = useState(''), [product, setProduct] = useState('')
  const [multi, setMulti] = useState(false), [busy, setBusy] = useState(false)
  const inFlight = useRef(false)
  const [error, setError] = useState<unknown>(), [selected, setSelected] = useState<number>()
  const [evidenceVersion, setEvidenceVersion] = useState(0)
  const [response, setResponse] = useState<Awaited<ReturnType<typeof api.research>>>()
  const [pending, setPending] = useState<ResearchRequest>()
  // 状态未读到或读取失败时不请求多 Agent；继续/重试始终保留原请求及其请求键。
  const multiEnabled = !retrieval.error && !retrieval.loading && retrieval.data?.multi_agent_enabled === true
  const execute = async (request: ResearchRequest) => {
    if (inFlight.current || !canResumeResearch(request, multiEnabled)) return
    inFlight.current = true
    setPending(request); setBusy(true); setError(undefined); setResponse(undefined)
    try {
      const result = await api.research(taskID, request)
      setSelected(result.run_id); setResponse(result); setPending(undefined)
      setEvidenceVersion((version) => version + 1)
    } catch (e) { setError(e) }
    finally { inFlight.current = false; setBusy(false); void runs.reload(); void retrieval.reload() }
  }
  const cancel = async (runID: number) => {
    setError(undefined)
    try { await api.cancelResearch(runID); void runs.reload() }
    catch (e) { setError(e) }
  }

  return <div className="space-y-4">
    <SemanticStatusBar retrieval={retrieval} />
    <Card title="证据检索">
      <Field label="待查问题"><TextArea value={question} onChange={(event) => setQuestion(event.target.value)} rows={3} /></Field>
      <Field label="准确产品型号（可选）"><Input value={product} onChange={(event) => setProduct(event.target.value)} /></Field>
      <label className="my-3 flex flex-wrap items-center gap-2 text-sm">
        <input type="checkbox" checked={multi && multiEnabled} disabled={!multiEnabled || busy} onChange={(event) => setMulti(event.target.checked)} />
        请求多 Agent 协作
        {!multiEnabled && <span className="text-muted">{retrieval.error ? '无法确认服务端是否启用，请刷新状态' : retrieval.data?.multi_agent_enabled === false ? '服务端未启用' : '正在确认服务端是否启用，暂不可选'}</span>}
      </label>
      <p className="mb-3 text-sm text-muted">检索保留证据和缺口；复杂语义仍需复核。中断后可继续同一记录，原有预算与截止时间继续生效。</p>
      <div className="flex flex-wrap gap-2">
        <Button loading={busy} disabled={!question.trim()} onClick={() => void execute({
          request_key: crypto.randomUUID(), question, query: { must_terms: product.trim() ? [product.trim()] : [] },
          ...(multi && multiEnabled ? { multi_agent: true } : {}),
        })}>开始检索</Button>
        {pending && !busy && <Button disabled={!canResumeResearch(pending, multiEnabled)} onClick={() => void execute(pending)}>重试原请求</Button>}
      </div>
      {pending?.multi_agent && !multiEnabled && <p className="mt-2 text-sm text-muted">服务端未启用或尚未确认多 Agent，暂不能重试此前的协作请求。可重新发起普通检索。</p>}
      <div className="mt-3"><ErrorBanner error={error ? retrievalErrorText(error) : ''} /></div>
      {response && <div className="mt-3 space-y-3">
        <p className="text-sm">检索记录 #{response.run_id} · {runStatusLabel(response.status)}</p>
        <DegradedNotice degraded={response.degraded} diagnostics={response.diagnostics} />
        {(response.gaps?.length ?? 0) > 0 && <ul className="list-disc space-y-1 pl-5 text-sm text-warn">
          {response.gaps?.map((gap, i) => <li key={`gap:${i}`}>{researchGapText(gap)}</li>)}
        </ul>}
        {!!response.gaps?.length && <details className="text-xs text-muted"><summary className="cursor-pointer">诊断详情</summary>
          <pre className="mt-2 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-soft p-3">{JSON.stringify({ gaps: response.gaps }, null, 2)}</pre>
        </details>}
      </div>}
      {error !== undefined && <details className="mt-3 text-xs text-muted"><summary className="cursor-pointer">诊断详情</summary>
        <pre className="mt-2 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-soft p-3">{JSON.stringify({
          error: error instanceof Error ? error.message : String(error), ...(error && typeof error === 'object' ? error : {}),
        }, null, 2)}</pre>
      </details>}
    </Card>
    <Card title="最近检索记录" actions={<Button loading={runs.loading} onClick={() => void runs.reload()}>刷新</Button>}>
      <ErrorBanner error={runs.error ? '检索记录读取失败，请刷新重试。' : ''} onRetry={runs.reload} />
      {runs.loading && !runs.data && <Spinner />}
      {runs.data?.length === 0 && <Empty>暂无检索记录</Empty>}
      <div className="space-y-3">{runs.data?.map((run) => {
        const limited = degradationSummary(run.result?.degraded, run.result?.diagnostics).limited
        return <div key={run.id} className="space-y-2 rounded-[10px] border border-line p-3 text-sm">
          <div className="flex flex-wrap items-center gap-2">
            <span className="min-w-0 flex-1 break-words">#{run.id} {run.request.question ?? '知识库查询'}</span>
            <Badge tone={runStatusTone(run.status)}>{runStatusLabel(run.status)}</Badge>
            {limited && <Badge tone="amber">已降级</Badge>}
            <Button size="sm" onClick={() => { setSelected(run.id); setEvidenceVersion((version) => version + 1) }}>证据</Button>
            {run.kind === 'research' && run.status === 'running' && <>
              <Button size="sm" disabled={busy || !canResumeResearch(run.request, multiEnabled)} onClick={() => void execute(run.request)}>继续</Button>
              <Button size="sm" onClick={() => void cancel(run.id)}>取消</Button>
            </>}
          </div>
          {run.kind === 'research' && run.status === 'running' && !canResumeResearch(run.request, multiEnabled) && <p className="text-xs text-muted">服务端未启用或尚未确认多 Agent，暂不能继续此前的协作请求。</p>}
          {(limited || !!run.result?.gaps?.length) && <details>
            <summary className="cursor-pointer text-muted">查看降级与缺口</summary>
            <div className="mt-2 space-y-2">
              <DegradedNotice degraded={run.result?.degraded} diagnostics={run.result?.diagnostics} />
              {!!run.result?.gaps?.length && <ul className="list-disc pl-5 text-warn">{run.result.gaps.map((gap, i) => <li key={i}>{researchGapText(gap)}</li>)}</ul>}
            </div>
          </details>}
          <details className="text-xs text-muted"><summary className="cursor-pointer">诊断详情</summary>
            <pre className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-soft p-3">{JSON.stringify({
              status: run.status, used_calls: run.used_calls, max_calls: run.max_calls, reserved_tokens: run.reserved_tokens,
              actual_tokens: run.actual_tokens, max_tokens: run.max_tokens, deadline: run.deadline, gaps: run.result?.gaps,
            }, null, 2)}</pre>
          </details>
        </div>
      })}</div>
    </Card>
    {selected && <RetrievalEvidence key={`${selected}:${evidenceVersion}`} runID={selected} />}
  </div>
}
