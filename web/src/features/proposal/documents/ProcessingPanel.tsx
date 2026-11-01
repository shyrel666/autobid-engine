import { useRef, useState } from 'react'
import { api, ApiError, errorText } from '../../../api/client'
import type { DocumentWithChunks } from '../../../api/types'
import type { ProcessingDetail, ProcessingRecord, ProcessingFile } from '../../../api/processing-types'
import { Badge, Button, Card, ErrorBanner, Field, Input, Modal, Notice, Select } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'
import { docRoleLabel, fmtTime } from '../../../lib/format'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import { FixedWorkflow } from './FixedWorkflow'

export function ProcessingPanel({ documents, selected }: { documents: DocumentWithChunks[]; selected?: DocumentWithChunks }) {
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('documents')
  const files = useAsync(() => api.processingFiles(taskId), [taskId, tick])
  const records = useAsync(() => api.processingRecords(taskId), [taskId, tick])
  const [older, setOlder] = useState<ProcessingRecord[]>([])
  const [extraFiles, setExtraFiles] = useState<ProcessingFile[]>([])
  const [filesHasMore, setFilesHasMore] = useState<boolean>()
  const [recordsHasMore, setRecordsHasMore] = useState<boolean>()
  const [selectedFiles, setSelectedFiles] = useState<number[]>([])
  const [form, setForm] = useState({ tender_no: '', lot_no: '', doc_role: '', published_at: '' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [detail, setDetail] = useState<ProcessingDetail>()
  const [note, setNote] = useState('')
  const [message, setMessage] = useState('')
  const keys = useRef(new Map<string, string>())
  const allRecords = [...(records.data ?? []), ...older].filter((v, i, a) => a.findIndex((x) => x.id === v.id) === i)
  const refresh = () => { setExtraFiles([]); setFilesHasMore(undefined); setOlder([]); setRecordsHasMore(undefined); invalidate('document_imported'); invalidate(['evidence']); void records.reload(); void files.reload() }
  const show = async (id: number, offset = 0) => { setError(''); try { setDetail(await api.processingDetail(taskId, id, offset)) } catch (e) { setError(errorText(e)) } }
  const importSelected = async () => {
    const values = { ...form, tender_no: (form.tender_no || selected?.tender_no || '').trim() }
    if (!values.tender_no) { setError('请填写附件所属的招标编号'); return }
    setBusy(true); setError(''); setMessage('')
    const failures: string[] = []; const done: number[] = []
    for (const id of selectedFiles) {
      const key = JSON.stringify([id, values])
      if (!keys.current.has(key)) keys.current.set(key, crypto.randomUUID())
      try {
        await api.enqueueFixedJob(taskId, { ...values, file_id: id, published_at: values.published_at ? `${values.published_at}T00:00:00Z` : undefined, request_key: keys.current.get(key)! })
        done.push(id); keys.current.delete(key)
      } catch (e) {
        failures.push(`文件 #${id}：${errorText(e)}`)
        if (e instanceof ApiError && e.status === 422) keys.current.delete(key)
      }
    }
    setSelectedFiles((ids) => ids.filter((id) => !done.includes(id)))
    setMessage(`已提交后台处理 ${done.length} 份，提交失败 ${failures.length} 份。处理完成后请重新提取并复核要求。`)
    setError(failures.join('\n')); setBusy(false); refresh()
  }
  const reparse = async () => {
    if (!selected) return
    setBusy(true); setError(''); setMessage('')
    const key = `reparse:${selected.id}:${selected.parse_revision}`
    if (!keys.current.has(key)) keys.current.set(key, crypto.randomUUID())
    try {
      await api.enqueueFixedJob(taskId, { document_id: selected.id, expected_revision: selected.parse_revision ?? 0, request_key: keys.current.get(key)! })
      keys.current.delete(key); setNote(''); setMessage('重新解析已提交后台处理；完成后点击“查看候选修订”核对采用。'); refresh()
    } catch (e) { setError(errorText(e)); if (e instanceof ApiError && e.status === 422) keys.current.delete(key); refresh() }
    finally { setBusy(false) }
  }
  const adopt = async () => {
    if (!detail) return
    setBusy(true); setError('')
    try {
      const r = await api.adoptProcessingRecord(taskId, detail.record.id, detail.record.expected_revision, note)
      setMessage(r.result.source_bumped ? '修订已采用，请重新提取并复核要求后再确认、导出。' : '修订已采用，解析内容无实质变化。')
      setDetail(undefined); refresh()
    } catch (e) { await show(detail.record.id); setError(errorText(e)) }
    finally { setBusy(false) }
  }
  if (!files.data && !files.error) return null
  if (files.data && !files.data.enabled) return <Notice tone="info">材料处理未启用，暂不能导入 ZIP 子文件或重新解析留存原件。</Notice>
  const allFiles = [...(files.data?.files ?? []), ...extraFiles].filter((v, i, a) => a.findIndex((x) => x.id === v.id) === i)
  const materials = allFiles.filter((f) => f.kind !== 'script' && f.format !== 'zip')
  const statusLabel = { ready: '待核对采用', adopted: '已完成', failed: '处理失败' }
  return <Card title="材料处理" actions={<Button size="sm" disabled={busy} onClick={refresh}>刷新处理记录</Button>}>
    <FixedWorkflow taskId={taskId} files={allFiles} tick={tick} refresh={refresh} show={show} />
    <ErrorBanner error={files.error || records.error || error} />
    {message && <Notice tone="info">{message} <a className="underline" href={`/tasks/${taskId}/requirements`}>前往要求清单</a></Notice>}
    {selected && <div className="mb-3 flex flex-wrap items-center gap-2 text-sm">
      <span>当前文档：{selected.title} · 解析修订 {selected.parse_revision}</span>
      <Button size="sm" loading={busy} disabled={!selected.original_file_id} onClick={reparse}>重新解析并预览</Button>
      {!selected.original_file_id && <span className="text-muted">原件未留存，需先上传相同原件</span>}
    </div>}
    {allFiles.length > 0 && <details open={documents.length === 0}>
      <summary className="cursor-pointer text-sm font-medium">原件与 ZIP 子文件（{materials.length}）</summary>
      <div className="my-3 grid gap-2 md:grid-cols-4">
        <Field label="附件招标编号"><Input value={form.tender_no || selected?.tender_no || ''} onChange={(e) => setForm({ ...form, tender_no: e.target.value })} /></Field>
        <Field label="附件标段"><Input value={form.lot_no} onChange={(e) => setForm({ ...form, lot_no: e.target.value })} /></Field>
        <Field label="附件角色"><Select value={form.doc_role} onChange={(e) => setForm({ ...form, doc_role: e.target.value })}><option value="">按文件名推断</option>{Object.entries(docRoleLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}</Select></Field>
        <Field label="附件公告日期"><Input type="date" value={form.published_at} onChange={(e) => setForm({ ...form, published_at: e.target.value })} /></Field>
      </div>
      <p className="text-xs text-muted">所选文件使用以上招标编号、标段、角色和日期；不同效力的文件请分批导入。</p>
      <ul className="my-2 max-h-64 overflow-auto divide-y divide-line">
        {materials.map((f) => {
          const imported = documents.some((d) => d.original_file_id === f.id) || allRecords.some((r) => r.file_id === f.id && r.operation === 'import' && r.status === 'adopted')
          const unsupported = f.format === 'unsupported' || f.availability !== 'available'
          const failed = allRecords.find((r) => r.file_id === f.id && r.operation === 'import')?.status === 'failed'
          return <li key={f.id} className="flex flex-wrap items-center gap-2 py-2 text-sm">
            <label className="flex min-w-0 flex-1 items-center gap-2"><input type="checkbox" aria-label={`选择 ${f.archive_path || f.original_name}`} checked={selectedFiles.includes(f.id)} disabled={busy || imported || unsupported}
              onChange={(e) => setSelectedFiles((ids) => e.target.checked ? [...ids, f.id] : ids.filter((id) => id !== f.id))} />
              <span className="break-all">{f.archive_path || f.original_name}</span></label>
            <span className="text-xs text-muted">{f.parent_file_id ? `来自 ZIP #${f.parent_file_id} · ` : ''}{f.format}</span>
            <Badge tone={imported ? 'green' : unsupported || failed ? 'red' : 'slate'}>{imported ? '已导入' : unsupported ? '不可处理' : failed ? '处理失败，可重试' : '待导入'}</Badge>
          </li>
        })}
      </ul>
      {(filesHasMore ?? files.data?.has_more) && <Button size="sm" onClick={async () => { try { const more = await api.processingFiles(taskId, allFiles.at(-1)?.id); setExtraFiles((v) => [...v, ...more.files]); setFilesHasMore(more.has_more) } catch (e) { setError(errorText(e)) } }}>更多文件</Button>}
      <Button size="sm" variant="primary" disabled={selectedFiles.length === 0} loading={busy} onClick={importSelected}>导入所选文件（{selectedFiles.length}）</Button>
    </details>}
    <div className="mt-3 space-y-2">
      <p className="text-sm font-medium">处理记录</p>
      {allRecords.length === 0 && <p className="text-sm text-muted">尚无处理记录。</p>}
      {allRecords.map((r) => <div key={r.id} className="flex flex-wrap items-center gap-2 border-t border-line py-2 text-sm">
        <span>#{r.id} · {r.operation === 'import' ? '文件导入' : '解析修订'} · 文件 #{r.file_id}</span>
        <Badge tone={r.status === 'failed' ? 'red' : r.status === 'ready' ? 'amber' : 'green'}>{statusLabel[r.status]}</Badge>
        <span className="text-xs text-muted">{fmtTime(r.created_at)}</span>
        {r.error && <span className="break-all text-xs text-red-600">{r.error}</span>}
        {r.operation === 'reparse' && r.status !== 'failed' && <Button size="sm" onClick={() => { setNote(''); void show(r.id) }}>查看修订 #{r.id}</Button>}
      </div>)}
      {(recordsHasMore ?? records.data?.length === 50) && <Button size="sm" onClick={async () => { try { const more = await api.processingRecords(taskId, allRecords.at(-1)?.id); setOlder((v) => [...v, ...more]); setRecordsHasMore(more.length === 50) } catch (e) { setError(errorText(e)) } }}>更早记录</Button>}
    </div>
    <Modal open={!!detail} wide title="解析修订预览" onClose={() => { if (!busy) setDetail(undefined) }} dirty={() => busy ? '正在采用修订，请等待完成。' : false}>
      {detail && <div className="space-y-3">
        <ErrorBanner error={error} />
        <p className="text-sm text-muted">原件：{allFiles.find((f) => f.id === detail.record.file_id)?.original_name || `文件 #${detail.record.file_id}`} · 基于解析修订 {detail.record.expected_revision}</p>
        <p>新增 {detail.preview.inserted} 块，更新 {detail.preview.updated} 块，删除 {detail.preview.deleted} 块，未变 {detail.preview.unchanged} 块；关联 {detail.preview.affected_requirements} 条要求需复核。</p>
        <Notice tone="warn">新增或变更内容须重新提取要求；关联数量不包含尚未提取的新要求。</Notice>
        {detail.stale && <Notice tone="danger">文档版本已变化，此候选已失效，请关闭后重新解析。</Notice>}
        {detail.warnings?.map((w) => <Notice key={w} tone="warn">{w}</Notice>)}
        {detail.preview.changes.map((c) => <div key={c.seq} className="space-y-1 border-t border-line pt-2">
          <p className="text-sm font-medium">片段 {c.seq + 1} · {({ insert: '新增', update: '更新', delete: '删除' } as Record<string, string>)[c.kind]}</p>
          <div className="grid gap-2 md:grid-cols-2">{[c.before, c.after].map((v, i) => <div key={i}><p className="text-xs text-muted">{i === 0 ? '当前解析' : '候选解析'}</p><pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded bg-soft p-2 text-xs">{v?.content || '（无）'}</pre></div>)}</div>
        </div>)}
        {detail.total_changes === 0 && <p className="text-sm text-muted">没有分块差异。</p>}
        <div className="flex gap-2">
          <Button size="sm" disabled={detail.offset === 0} onClick={() => void show(detail.record.id, detail.offset - 20)}>上一页差异</Button>
          <Button size="sm" disabled={!detail.has_more} onClick={() => void show(detail.record.id, detail.offset + 20)}>下一页差异</Button>
        </div>
        {detail.record.status === 'ready' && <><Field label="复核说明"><Input value={note} onChange={(e) => setNote(e.target.value)} /></Field><Button variant="primary" disabled={detail.stale} loading={busy} onClick={adopt}>确认采用此修订</Button></>}
        {detail.record.status === 'adopted' && <Notice tone="info">此修订已采用。</Notice>}
      </div>}
    </Modal>
  </Card>
}
