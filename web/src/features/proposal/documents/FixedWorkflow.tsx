import { useEffect, useRef, useState } from 'react'
import { api, errorText } from '../../../api/client'
import type { FixedJob, ProcessingFile, TaskMaterial } from '../../../api/processing-types'
import { Badge, Button, ErrorBanner, Field, Input, Notice, Select } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'

const materialLabels = { missing: '尚未上传或绑定', unavailable: '原件不可用', processing: '正在处理', review: '待采用修订', not_imported: '尚未正式导入', complete: '已完成' }
const jobLabels = { queued: '排队中', running: '处理中', succeeded: '处理完成', failed: '失败', cancelled: '已取消' }

function MaterialRow({ material, files, save }: { material: TaskMaterial; files: ProcessingFile[]; save: (m: TaskMaterial) => Promise<void> }) {
  const [draft, setDraft] = useState(material)
  const [busy, setBusy] = useState(false)
  useEffect(() => setDraft(material), [material.id, material.version])
  return <div className="space-y-2 rounded border border-line p-3">
    <div className="flex flex-wrap items-center gap-2"><strong className="text-sm">{material.name}</strong><Badge tone={material.state === 'complete' ? 'green' : material.required ? 'red' : 'slate'}>{material.required ? '必需 · ' : '非必需 · '}{materialLabels[material.state]}</Badge></div>
    <div className="grid gap-2 md:grid-cols-3">
      <Field label="材料名称"><Input value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} /></Field>
      <Field label="绑定原件"><Select aria-label="绑定原件" value={draft.file_id || ''} onChange={(e) => setDraft({ ...draft, file_id: Number(e.target.value) || null })}>
        <option value="">尚未上传 / 未绑定</option>
        {draft.file_id && !files.some((f) => f.id === draft.file_id) && <option value={draft.file_id}>原件 #{draft.file_id}（未在当前页）</option>}
        {files.filter((f) => f.kind !== 'script' && f.format !== 'zip').map((f) => <option key={f.id} value={f.id}>#{f.id} {f.archive_path || f.original_name}</option>)}
      </Select></Field>
      <Field label="说明（取消必需时必填）"><Input value={draft.note} onChange={(e) => setDraft({ ...draft, note: e.target.value })} /></Field>
    </div>
    <div className="flex items-center gap-3 text-sm"><label><input type="checkbox" checked={draft.required} onChange={(e) => setDraft({ ...draft, required: e.target.checked })} /> 必需材料</label>
      <Button size="sm" loading={busy} onClick={async () => { setBusy(true); try { await save(draft) } finally { setBusy(false) } }}>保存材料</Button>
      <span className="text-xs text-muted">版本 {material.version}{material.updated_by ? ` · ${material.updated_by}` : ''}</span>
    </div>
  </div>
}

export function FixedWorkflow({ taskId, files, tick, refresh, show }: { taskId: number; files: ProcessingFile[]; tick: number; refresh: () => void; show: (id: number) => Promise<void> }) {
  const materials = useAsync(() => api.materials(taskId), [taskId, tick])
  const jobs = useAsync(() => api.fixedJobs(taskId), [taskId, tick])
  const [older, setOlder] = useState<FixedJob[]>([])
  const [more, setMore] = useState<boolean>()
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const [message, setMessage] = useState('')
  const [busy, setBusy] = useState(false)
  const upload = useRef<HTMLInputElement>(null)
  const refreshRef = useRef(refresh); refreshRef.current = refresh
  const jobsRef = useRef(jobs.reload); jobsRef.current = jobs.reload
  const materialsRef = useRef(materials.reload); materialsRef.current = materials.reload
  const signatures = useRef('')
  const allJobs = [...(jobs.data ?? []), ...older].filter((j, i, a) => a.findIndex((v) => v.id === j.id) === i)
  useEffect(() => {
    const timer = setInterval(() => { void jobsRef.current(); void materialsRef.current() }, 2000)
    return () => clearInterval(timer)
  }, [taskId])
  useEffect(() => {
    const next = (jobs.data ?? []).map((j) => `${j.id}:${j.status}`).join(',')
    if (next !== signatures.current) { const previous = signatures.current; signatures.current = next; if (previous) refreshRef.current() }
  }, [jobs.data])
  const run = async (fn: () => Promise<void>) => { setBusy(true); setError(''); try { await fn(); await materials.reload(); await jobs.reload(); refresh() } catch (e) { setError(errorText(e)) } finally { setBusy(false) } }
  return <div className="space-y-4 border-b border-line pb-4">
    <ErrorBanner error={error || materials.error || jobs.error} />
    {message && <Notice tone="info">{message}</Notice>}
    <div className="space-y-2">
      <p className="font-medium">必需材料清单</p>
      <p className="text-xs text-muted">可先登记尚未收到的材料。必需材料未上传、未导入或有待采用修订时，导出会被阻断。ZIP 请绑定其中的具体子文件。</p>
      <div className="flex gap-2"><Input aria-label="新增必需材料名称" placeholder="例如：设备清单附件" value={name} onChange={(e) => setName(e.target.value)} /><Button size="sm" disabled={!name.trim()} loading={busy} onClick={() => run(async () => { await api.saveMaterial(taskId, { id: 0, name: name.trim(), required: true, file_id: null, version: 0, note: '' }); setName('') })}>登记必需材料</Button></div>
      {materials.data?.map((m) => <MaterialRow key={m.id} material={m} files={files} save={(draft) => run(async () => { await api.saveMaterial(taskId, draft) })} />)}
    </div>
    <div className="space-y-2">
      <p className="font-medium">上传原件后处理</p>
      <p className="text-xs text-muted">原件上传完成后可关闭页面。ZIP 在后台展开；其他文件在下方选择导入。单文件上限 64 MiB。</p>
      <input ref={upload} type="file" aria-label="选择后台处理原件" disabled={busy} />
      <Button size="sm" loading={busy} onClick={() => run(async () => {
        const file = upload.current?.files?.[0]; if (!file) throw new Error('请选择文件'); if (file.size > 64 * 1024 * 1024) throw new Error('文件超过 64 MiB')
        const stored = await api.retainProcessingFile(taskId, file)
        setMessage(`已留存原件 #${stored.id} ${stored.original_name}，请在下方选择导入并绑定材料清单。`)
        if (stored.format === 'zip') { await api.enqueueFixedJob(taskId, { file_id: stored.id, operation: 'archive', request_key: crypto.randomUUID() }); setMessage(`原件 #${stored.id} 已留存，ZIP 正在后台展开。`) }
        if (upload.current) upload.current.value = ''
      })}>上传原件</Button>
    </div>
    <div className="space-y-2">
      <p className="font-medium">后台处理作业</p>
      {allJobs.length === 0 && <p className="text-sm text-muted">尚无后台作业。</p>}
      {allJobs.map((j) => <div key={j.id} className="flex flex-wrap items-center gap-2 border-t border-line py-2 text-sm">
        <span>作业 #{j.id} · 原件 #{j.file_id}</span><Badge tone={j.status === 'failed' ? 'red' : j.status === 'succeeded' ? 'green' : 'slate'}>{jobLabels[j.status]}</Badge><span className="text-xs text-muted">尝试 {j.attempts}/{j.max_attempts}</span>
        {j.error && <span className="break-all text-xs text-red-600">{j.error}</span>}
        {(j.status === 'queued' || j.status === 'running') && <Button size="sm" disabled={busy} onClick={() => run(async () => { await api.cancelFixedJob(taskId, j.id) })}>取消处理</Button>}
        {(j.status === 'failed' || j.status === 'cancelled') && <Button size="sm" disabled={busy} onClick={() => run(async () => { await api.enqueueFixedJob(taskId, { ...j.input, request_key: crypto.randomUUID() }) })}>重新处理</Button>}
        {j.status === 'succeeded' && j.record_id && j.input.document_id && <Button size="sm" onClick={() => void show(j.record_id!)}>查看候选修订</Button>}
      </div>)}
      {(more ?? jobs.data?.length === 50) && <Button size="sm" onClick={() => run(async () => { const next = await api.fixedJobs(taskId, allJobs.at(-1)?.id); setOlder((v) => [...v, ...next]); setMore(next.length === 50) })}>更早作业</Button>}
    </div>
  </div>
}
