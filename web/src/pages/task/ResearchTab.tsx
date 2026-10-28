import { useState } from 'react'
import { api, errorText } from '../../api/client'
import type { ResearchRequest } from '../../api/types'
import { useAsync } from '../../hooks/useAsync'
import { Button, Card, ErrorBanner, Field, Input, TextArea, Badge } from '../../components/ui'
import { RetrievalEvidence } from '../../components/RetrievalEvidence'

export default function ResearchTab({taskID}: {taskID: number}) {
 const runs=useAsync(() => api.taskRetrievalRuns(taskID),[taskID])
 const [question,setQuestion]=useState(''), [product,setProduct]=useState('')
 const [multi,setMulti]=useState(false),[busy,setBusy]=useState(false)
 const [error,setError]=useState(''),[selected,setSelected]=useState<number>()
 const [gaps,setGaps]=useState<string[]>([])
 const [pending,setPending]=useState<ResearchRequest>()
 const execute=async(req: ResearchRequest) => {
  setPending(req);setBusy(true);setError('');setGaps([])
  try {const r=await api.research(taskID,req);setSelected(r.run_id);setGaps([...r.gaps,...(r.degraded ?? []).map(d=>`检索降级：${d}`)]);setPending(undefined)}catch(e){setError(errorText(e))}finally{setBusy(false);void runs.reload()}
 }
 return <div className="space-y-4">
  <Card title="证据检索">
   <Field label="待查问题"><TextArea value={question} onChange={e=>setQuestion(e.target.value)} rows={3}/></Field>
   <Field label="准确产品型号（可选）"><Input value={product} onChange={e=>setProduct(e.target.value)}/></Field>
   <label className="my-3 block text-sm"><input type="checkbox" checked={multi} onChange={e=>setMulti(e.target.checked)}/> 请求多 Agent 协作（需服务端评测后启用）</label>
   <p className="mb-3 text-sm text-slate-500">检索保留证据和缺口；复杂语义仍需复核。中断后可继续同一记录，原有预算与截止时间继续生效。</p>
   <Button loading={busy} disabled={!question.trim()} onClick={()=>void execute({request_key:crypto.randomUUID(),question,query:{must_terms:product.trim()?[product.trim()]:[]},multi_agent:multi})}>开始检索</Button>
   {pending && !busy && <Button onClick={()=>void execute(pending)}>重试原请求</Button>}
   <ErrorBanner error={error}/>
   {gaps.length>0 && <ul className="mt-2 list-disc pl-5 text-sm text-amber-700">{gaps.map((g,i)=><li key={i}>{g}</li>)}</ul>}
  </Card>
  <Card title="最近检索记录" actions={<Button onClick={()=>void runs.reload()}>刷新</Button>}>
   <ErrorBanner error={runs.error}/>
   <div className="space-y-2">{runs.data?.map(run=><div key={run.id} className="flex flex-wrap items-center gap-2 text-sm">
    <span>#{run.id} {run.request.question ?? '知识库查询'}</span><Badge>{run.status}</Badge><span>调用 {run.used_calls}/{run.max_calls} · 预留 token {run.reserved_tokens}/{run.max_tokens}</span>
    <Button onClick={()=>setSelected(run.id)}>证据</Button>
    {run.kind==='research' && run.status==='running' && <><Button disabled={busy} onClick={()=>void execute(run.request)}>继续</Button><Button onClick={async()=>{try{await api.cancelResearch(run.id);void runs.reload()}catch(e){setError(errorText(e))}}}>取消</Button></>}
   </div>)}</div>
  </Card>
  {selected && <RetrievalEvidence key={selected} runID={selected}/>}
 </div>
}
