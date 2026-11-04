import { useRef, useState } from 'react'
import { api, ApiError, errorText } from '../../../api/client'
import type { DocumentWithChunks } from '../../../api/types'
import type { ParserProfile, ProcessingDetail, ProcessingRecord, ProcessingFile } from '../../../api/processing-types'
import { Badge, Button, Card, ErrorBanner, Field, Input, Modal, Notice, Select } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'
import { docRoleLabel, fmtTime } from '../../../lib/format'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import { useRuntimeTools } from '../../runtime/RuntimeStatus'
import { FixedWorkflow } from './FixedWorkflow'
import { executorLabel, gapText, keyForInput, locatorText, profileOptions, recordStatusView, usesOCR } from './parseJobs'

const CANDIDATE_PAGE = 20

/** 候选核对视图（S5a）：元数据、执行版本、缺口、分页正文与原件定位；OCR 文字标明未经人工核验。 */
function CandidateView({ detail, fileName, onPage }: { detail: ProcessingDetail; fileName: string; onPage: (cursor: number) => void }) {
  const c = detail.candidate
  if (!c) return <Notice tone="danger">该记录没有可核对的候选（解析失败）。</Notice>
  const ocr = usesOCR(c.parser_version)
  const doc = c.document
  return <div className="space-y-3">
    <div className="grid gap-1 text-sm md:grid-cols-2">
      <p>原件：{fileName} · SHA-256 <span className="font-mono text-xs" title={c.source_hash}>{c.source_hash.slice(0, 12)}…</span></p>
      <p>招标编号 {doc.tender_no}{doc.lot_no ? ` · 标段 ${doc.lot_no}` : ''} · {doc.doc_role ? docRoleLabel[doc.doc_role] ?? doc.doc_role : '按文件名推断角色'}</p>
      <p>标题：{doc.title}{doc.page_count ? ` · ${doc.page_count} 页` : ''} · 共 {c.total_sections} 段</p>
      <p title={c.parser_version}>解析配置 {c.parser_profile || 'auto'} · {executorLabel(c.parser_version)}</p>
    </div>
    {ocr && <Notice tone="warn">第 {(c.ocr_pages ?? []).join('、')} 页为 OCR 机器识别文字，未经人工核验：请对照原件页码与识别行核对数字、条款编号与 ★ 标记后再采用；采用后也不等于证据已核验。</Notice>}
    {c.gaps.length > 0 && <Notice tone="danger"><p className="font-medium">解析缺口（{c.gaps.length}）</p><ul className="list-disc pl-5">{c.gaps.map((g, i) => <li key={i}>{gapText(g)}</li>)}</ul></Notice>}
    {c.sections.map((s) => <div key={s.seq} className="space-y-1 border-t border-line pt-2">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="font-medium">片段 {s.seq + 1}{s.section_code ? ` · ${s.section_code}` : ''}{s.section_title ? ` ${s.section_title}` : ''}</span>
        {s.is_mandatory && <Badge tone="red">★ 实质性</Badge>}
        {s.locator.ocr && <Badge tone="amber">OCR 识别</Badge>}
        {!!s.tables && <Badge tone="slate">表格 {s.tables}</Badge>}
        <span className="text-xs text-muted">{locatorText(s.locator)}</span>
      </div>
      <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded bg-soft p-2 text-xs">{s.content || '（空）'}{s.truncated ? '\n（本段预览已截断）' : ''}</pre>
    </div>)}
    <div className="flex items-center gap-2">
      <Button size="sm" disabled={c.cursor === 0} onClick={() => onPage(Math.max(0, c.cursor - CANDIDATE_PAGE))}>上一页</Button>
      <span className="text-xs text-muted">第 {c.cursor + 1}–{c.cursor + c.sections.length} 段 / 共 {c.total_sections} 段</span>
      <Button size="sm" disabled={!c.next_cursor} onClick={() => onPage(c.next_cursor)}>下一页</Button>
    </div>
  </div>
}

export function ProcessingPanel({ documents, selected }: { documents: DocumentWithChunks[]; selected?: DocumentWithChunks }) {
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('documents')
  const files = useAsync(() => api.processingFiles(taskId), [taskId, tick])
  const records = useAsync(() => api.processingRecords(taskId), [taskId, tick])
  const runtime = useRuntimeTools()
  const ocrStatus = runtime.data?.find((c) => c.capability === 'ocr')
  const profiles = profileOptions(ocrStatus)
  const [older, setOlder] = useState<ProcessingRecord[]>([])
  const [extraFiles, setExtraFiles] = useState<ProcessingFile[]>([])
  const [filesHasMore, setFilesHasMore] = useState<boolean>()
  const [recordsHasMore, setRecordsHasMore] = useState<boolean>()
  const [selectedFiles, setSelectedFiles] = useState<number[]>([])
  const [form, setForm] = useState({ tender_no: '', lot_no: '', doc_role: '', published_at: '' })
  const [profile, setProfile] = useState<ParserProfile>('auto')
  const [candidateOnly, setCandidateOnly] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [detail, setDetail] = useState<ProcessingDetail>()
  const [note, setNote] = useState('')
  const [reason, setReason] = useState('')
  const [message, setMessage] = useState('')
  const keys = useRef(new Map<string, string>())
  const allRecords = [...(records.data ?? []), ...older].filter((v, i, a) => a.findIndex((x) => x.id === v.id) === i)
  const refresh = () => { setExtraFiles([]); setFilesHasMore(undefined); setOlder([]); setRecordsHasMore(undefined); invalidate('document_imported'); invalidate(['evidence']); void records.reload(); void files.reload() }
  const show = async (id: number, offset = 0, cursor = 0) => { setError(''); try { setDetail(await api.processingDetail(taskId, id, offset, cursor)) } catch (e) { setError(errorText(e)) } }
  const importSelected = async () => {
    const values = { ...form, tender_no: (form.tender_no || selected?.tender_no || '').trim() }
    if (!values.tender_no) { setError('请填写附件所属的招标编号'); return }
    setBusy(true); setError(''); setMessage('')
    const failures: string[] = []; const done: number[] = []
    for (const id of selectedFiles) {
      const input = { ...values, file_id: id, published_at: values.published_at ? `${values.published_at}T00:00:00Z` : undefined, candidate_only: candidateOnly, parser_profile: profile }
      const key = keyForInput(keys.current, input)
      try {
        await api.enqueueFixedJob(taskId, { ...input, request_key: key })
        done.push(id); keys.current.delete(JSON.stringify(input))
      } catch (e) {
        failures.push(`文件 #${id}：${errorText(e)}`)
        if (e instanceof ApiError && (e.status === 422 || e.status === 503)) keys.current.delete(JSON.stringify(input))
      }
    }
    setSelectedFiles((ids) => ids.filter((id) => !done.includes(id)))
    setMessage(candidateOnly
      ? `已提交后台解析 ${done.length} 份，提交失败 ${failures.length} 份。完成后在“后台处理作业”或“处理记录”中核对候选并采用。`
      : `已提交后台处理 ${done.length} 份，提交失败 ${failures.length} 份。处理完成后请重新提取并复核要求。`)
    setError(failures.join('\n')); setBusy(false); refresh()
  }
  const reparse = async () => {
    if (!selected) return
    setBusy(true); setError(''); setMessage('')
    const input = { document_id: selected.id, expected_revision: selected.parse_revision ?? 0, parser_profile: profile }
    const key = keyForInput(keys.current, input)
    try {
      await api.enqueueFixedJob(taskId, { ...input, request_key: key })
      keys.current.delete(JSON.stringify(input)); setNote(''); setMessage('重新解析已提交后台处理；完成后点击“核对候选”对照采用。'); refresh()
    } catch (e) { setError(errorText(e)); if (e instanceof ApiError && (e.status === 422 || e.status === 503)) keys.current.delete(JSON.stringify(input)); refresh() }
    finally { setBusy(false) }
  }
  const adopt = async () => {
    if (!detail) return
    setBusy(true); setError('')
    try {
      const r = await api.adoptProcessingRecord(taskId, detail.record.id, detail.record.expected_revision, note)
      setMessage(detail.record.operation === 'import'
        ? '候选已采用为正式招标文档：来源已变化，请重新提取并复核要求后再确认、导出。'
        : r.result.source_bumped ? '修订已采用，请重新提取并复核要求后再确认、导出。' : '修订已采用，解析内容无实质变化。')
      setDetail(undefined); refresh()
    } catch (e) { await show(detail.record.id); setError(errorText(e)) }
    finally { setBusy(false) }
  }
  const reject = async () => {
    if (!detail) return
    if (!reason.trim()) { setError('请填写拒绝原因'); return }
    setBusy(true); setError('')
    try {
      await api.rejectProcessingRecord(taskId, detail.record.id, reason.trim())
      setMessage(`候选 #${detail.record.id} 已拒绝，正式招标文档未改变。`); setDetail(undefined); setReason(''); refresh()
    } catch (e) { await show(detail.record.id); setError(errorText(e)) }
    finally { setBusy(false) }
  }
  if (!files.data && !files.error) return null
  if (files.data && !files.data.enabled) return <Notice tone="info">材料处理未启用，暂不能导入 ZIP 子文件或重新解析留存原件。</Notice>
  const allFiles = [...(files.data?.files ?? []), ...extraFiles].filter((v, i, a) => a.findIndex((x) => x.id === v.id) === i)
  const materials = allFiles.filter((f) => f.kind !== 'script' && f.format !== 'zip')
  const currentProfile = profiles.find((o) => o.value === profile)
  const profileField = <Field label="解析配置" hint={currentProfile?.hint}>
    <Select aria-label="解析配置" value={profile} onChange={(e) => setProfile(e.target.value as ParserProfile)}>
      {profiles.map((o) => <option key={o.value} value={o.value} disabled={o.disabled}>{o.label}{o.disabled ? '（不可用）' : ''}</option>)}
    </Select>
  </Field>
  const fileName = (id: number) => { const f = allFiles.find((x) => x.id === id); return f?.archive_path || f?.original_name || `文件 #${id}` }
  return <Card title="材料处理" actions={<Button size="sm" disabled={busy} onClick={refresh}>刷新处理记录</Button>}>
    <FixedWorkflow taskId={taskId} files={allFiles} tick={tick} refresh={refresh} show={show} />
    <ErrorBanner error={files.error || records.error || error} />
    {message && <Notice tone="info">{message} <a className="underline" href={`/tasks/${taskId}/requirements`}>前往要求清单</a></Notice>}
    {selected && <div className="mb-3 flex flex-wrap items-end gap-2 text-sm">
      <span className="pb-2">当前文档：{selected.title} · 解析修订 {selected.parse_revision}</span>
      <div className="w-64">{profileField}</div>
      <Button size="sm" loading={busy} disabled={!selected.original_file_id} onClick={reparse}>重新解析并预览</Button>
      {!selected.original_file_id && <span className="pb-2 text-muted">原件未留存，需先上传相同原件</span>}
    </div>}
    {allFiles.length > 0 && <details open={documents.length === 0}>
      <summary className="cursor-pointer text-sm font-medium">原件与 ZIP 子文件（{materials.length}）</summary>
      <div className="my-3 grid gap-2 md:grid-cols-4">
        <Field label="附件招标编号"><Input value={form.tender_no || selected?.tender_no || ''} onChange={(e) => setForm({ ...form, tender_no: e.target.value })} /></Field>
        <Field label="附件标段"><Input value={form.lot_no} onChange={(e) => setForm({ ...form, lot_no: e.target.value })} /></Field>
        <Field label="附件角色"><Select value={form.doc_role} onChange={(e) => setForm({ ...form, doc_role: e.target.value })}><option value="">按文件名推断</option>{Object.entries(docRoleLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}</Select></Field>
        <Field label="附件公告日期"><Input type="date" value={form.published_at} onChange={(e) => setForm({ ...form, published_at: e.target.value })} /></Field>
      </div>
      <div className="grid gap-2 md:grid-cols-2">
        {profileField}
        <label className="flex items-center gap-2 pt-5 text-sm"><input type="checkbox" checked={candidateOnly} onChange={(e) => setCandidateOnly(e.target.checked)} />
          先生成候选，核对原件后再采用（扫描件 / OCR 建议勾选）</label>
      </div>
      <p className="text-xs text-muted">所选文件使用以上招标编号、标段、角色和日期；不同效力的文件请分批导入。不勾选候选模式时解析成功即直接成为正式招标文档。</p>
      <ul className="my-2 max-h-64 overflow-auto divide-y divide-line">
        {materials.map((f) => {
          const imported = documents.some((d) => d.original_file_id === f.id) || allRecords.some((r) => r.file_id === f.id && r.operation === 'import' && r.status === 'adopted')
          const unsupported = f.format === 'unsupported' || f.availability !== 'available'
          const latest = allRecords.find((r) => r.file_id === f.id && r.operation === 'import')
          const failed = latest?.status === 'failed'
          const pending = latest?.status === 'ready'
          return <li key={f.id} className="flex flex-wrap items-center gap-2 py-2 text-sm">
            <label className="flex min-w-0 flex-1 items-center gap-2"><input type="checkbox" aria-label={`选择 ${f.archive_path || f.original_name}`} checked={selectedFiles.includes(f.id)} disabled={busy || imported || unsupported}
              onChange={(e) => setSelectedFiles((ids) => e.target.checked ? [...ids, f.id] : ids.filter((id) => id !== f.id))} />
              <span className="break-all">{f.archive_path || f.original_name}</span></label>
            <span className="text-xs text-muted">{f.parent_file_id ? `来自 ZIP #${f.parent_file_id} · ` : ''}{f.format}</span>
            <Badge tone={imported ? 'green' : unsupported || failed ? 'red' : pending ? 'amber' : 'slate'}>{imported ? '已导入' : unsupported ? '不可处理' : failed ? '处理失败，可重试' : pending ? '候选待核对' : '待导入'}</Badge>
            {pending && latest && <Button size="sm" onClick={() => { setNote(''); setReason(''); void show(latest.id) }}>核对候选 #{latest.id}</Button>}
          </li>
        })}
      </ul>
      {(filesHasMore ?? files.data?.has_more) && <Button size="sm" onClick={async () => { try { const more = await api.processingFiles(taskId, allFiles.at(-1)?.id); setExtraFiles((v) => [...v, ...more.files]); setFilesHasMore(more.has_more) } catch (e) { setError(errorText(e)) } }}>更多文件</Button>}
      <Button size="sm" variant="primary" disabled={selectedFiles.length === 0 || !!currentProfile?.disabled} loading={busy} onClick={importSelected}>{candidateOnly ? '解析为候选' : '导入所选文件'}（{selectedFiles.length}）</Button>
    </details>}
    <div className="mt-3 space-y-2">
      <p className="text-sm font-medium">处理记录</p>
      {allRecords.length === 0 && <p className="text-sm text-muted">尚无处理记录。</p>}
      {allRecords.map((r) => {
        const v = recordStatusView(r)
        return <div key={r.id} className="space-y-1 border-t border-line py-2 text-sm">
          <div className="flex flex-wrap items-center gap-2">
            <span>#{r.id} · {r.operation === 'import' ? (r.candidate_only ? '导入候选' : '文件导入') : '解析修订'} · {fileName(r.file_id)}</span>
            <Badge tone={v.tone}>{v.label}</Badge>
            <span className="text-xs text-muted">{fmtTime(r.created_at)}{r.created_by === 'worker' ? ' · Agent 提交' : ''}</span>
            {r.status !== 'failed' && (r.operation === 'reparse' || r.candidate_only) && <Button size="sm" onClick={() => { setNote(''); setReason(''); void show(r.id) }}>{r.status === 'ready' ? '核对候选' : '查看'} #{r.id}</Button>}
          </div>
          {v.note && <p className={`break-all text-xs ${v.tone === 'red' ? 'text-red-600' : 'text-muted'}`}>{v.note}</p>}
        </div>
      })}
      {(recordsHasMore ?? records.data?.length === 50) && <Button size="sm" onClick={async () => { try { const more = await api.processingRecords(taskId, allRecords.at(-1)?.id); setOlder((v) => [...v, ...more]); setRecordsHasMore(more.length === 50) } catch (e) { setError(errorText(e)) } }}>更早记录</Button>}
    </div>
    <Modal open={!!detail} wide title={detail?.record.operation === 'import' ? '解析候选核对' : '解析修订预览'} onClose={() => { if (!busy) setDetail(undefined) }} dirty={() => busy ? '正在处理候选，请等待完成。' : false}>
      {detail && <div className="space-y-3">
        <ErrorBanner error={error} />
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <Badge tone={recordStatusView(detail.record).tone}>{recordStatusView(detail.record).label}</Badge>
          {detail.record.operation === 'reparse' && <span className="text-muted">基于解析修订 {detail.record.expected_revision}</span>}
          {detail.record.created_by === 'worker' && <Badge tone="blue">Agent 提交</Badge>}
        </div>
        {detail.stale && <Notice tone="danger">{detail.record.operation === 'import' ? '原件已变化或不可用，此候选已失效，请关闭后重新解析。' : '文档版本已变化，此候选已失效，请关闭后重新解析。'}</Notice>}
        {detail.record.operation === 'reparse' && <>
          <p>新增 {detail.preview.inserted} 块，更新 {detail.preview.updated} 块，删除 {detail.preview.deleted} 块，未变 {detail.preview.unchanged} 块；关联 {detail.preview.affected_requirements} 条要求需复核。</p>
          <Notice tone="warn">新增或变更内容须重新提取要求；关联数量不包含尚未提取的新要求。</Notice>
          {detail.preview.changes.map((c) => <div key={c.seq} className="space-y-1 border-t border-line pt-2">
            <p className="text-sm font-medium">片段 {c.seq + 1} · {({ insert: '新增', update: '更新', delete: '删除' } as Record<string, string>)[c.kind]}</p>
            <div className="grid gap-2 md:grid-cols-2">{[c.before, c.after].map((v, i) => <div key={i}><p className="text-xs text-muted">{i === 0 ? '当前解析' : '候选解析'}</p><pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded bg-soft p-2 text-xs">{v?.content || '（无）'}</pre></div>)}</div>
          </div>)}
          {detail.total_changes === 0 && <p className="text-sm text-muted">没有分块差异。</p>}
          <div className="flex gap-2">
            <Button size="sm" disabled={detail.offset === 0} onClick={() => void show(detail.record.id, detail.offset - 20)}>上一页差异</Button>
            <Button size="sm" disabled={!detail.has_more} onClick={() => void show(detail.record.id, detail.offset + 20)}>下一页差异</Button>
          </div>
          {detail.gaps && detail.gaps.length > 0 && <Notice tone="danger"><p className="font-medium">候选解析缺口（{detail.gaps.length}）</p><ul className="list-disc pl-5">{detail.gaps.map((g, i) => <li key={i}>{gapText(g)}</li>)}</ul></Notice>}
        </>}
        {detail.record.operation === 'import' && <CandidateView detail={detail} fileName={fileName(detail.record.file_id)} onPage={(cursor) => void show(detail.record.id, 0, cursor)} />}
        {detail.record.status === 'ready' && <div className="space-y-2 border-t border-line pt-3">
          <div className="grid gap-2 md:grid-cols-2">
            <Field label="复核说明（采用时记录）"><Input value={note} onChange={(e) => setNote(e.target.value)} /></Field>
            <Field label="拒绝原因（拒绝时必填）"><Input value={reason} onChange={(e) => setReason(e.target.value)} /></Field>
          </div>
          <div className="flex flex-wrap gap-2">
            <Button variant="primary" disabled={detail.stale} loading={busy} onClick={adopt}>{detail.record.operation === 'import' ? '确认采用为正式招标文档' : '确认采用此修订'}</Button>
            <Button disabled={busy} onClick={reject}>拒绝此候选</Button>
          </div>
        </div>}
        {detail.record.status === 'adopted' && <Notice tone="info">此候选已采用{detail.record.approved_by ? `（${detail.record.approved_by}）` : ''}。</Notice>}
        {detail.record.status === 'rejected' && <Notice tone="info">此候选已拒绝：{detail.record.reject_reason}{detail.record.rejected_by ? `（${detail.record.rejected_by}）` : ''}。</Notice>}
      </div>}
    </Modal>
  </Card>
}
