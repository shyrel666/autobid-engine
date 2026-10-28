import { useMemo, useRef, useState } from 'react'
import { ChevronDown, ChevronRight, FileText, FileUp, RefreshCw, Star, Upload } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { DocumentChunk, DocumentWithChunks, UploadDocumentResp } from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Spinner, TextArea, cx, useToast } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import { fmtNum, fmtTime } from '../../lib/format'

const MAX_TEXT_BYTES = 32 << 20 // 与后端 maxDocumentBody 一致

function UploadCard({ taskID, onUploaded }: { taskID: number; onUploaded: () => void }) {
  const toast = useToast()
  const fileRef = useRef<HTMLInputElement>(null)
  const [form, setForm] = useState({ tender_no: '', lot_no: '', title: '', text: '', wake: true })
  const [fileName, setFileName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<UploadDocumentResp>()

  const pickFile = async (f: File | undefined) => {
    if (!f) return
    if (!/\.(txt|md|markdown)$/i.test(f.name)) { setError('当前仅支持 .txt / .md 纯文本文件（PDF/DOCX/扫描件解析将在 Phase 4 提供）'); return }
    if (f.size > MAX_TEXT_BYTES) { setError('文件超过 32 MiB 上限'); return }
    const text = await f.text()
    setFileName(f.name)
    setForm((s) => ({ ...s, text, title: s.title || f.name.replace(/\.(txt|md|markdown)$/i, '') }))
    setError('')
  }

  const submit = async () => {
    setError('')
    if (!form.tender_no.trim() || !form.text.trim()) { setError('招标编号与招标文件正文为必填项'); return }
    if (new Blob([form.text]).size > MAX_TEXT_BYTES) { setError('正文超过 32 MiB 上限'); return }
    setBusy(true)
    try {
      const r = await api.uploadDocument(taskID, {
        tender_no: form.tender_no.trim(), lot_no: form.lot_no.trim(), title: form.title.trim(), text: form.text, wake_planner: form.wake,
      })
      setResult(r)
      toast(r.result.created ? 'ok' : 'info', r.result.created
        ? `已切分 ${r.result.chunks} 个章节/条款，其中 ${r.result.mandatory_chunks} 个强制性条款`
        : '相同原文已导入过，未重复切分')
      onUploaded()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card title={<><Upload className="size-4 text-blue-600" /> 导入招标文件</>}>
      <div className="grid gap-3 md:grid-cols-3">
        <Field label="招标编号 tender_no *"><Input value={form.tender_no} onChange={(e) => setForm({ ...form, tender_no: e.target.value })} placeholder="ZB-2026-001" /></Field>
        <Field label="标段 lot_no"><Input value={form.lot_no} onChange={(e) => setForm({ ...form, lot_no: e.target.value })} placeholder="可空，表示整个项目" /></Field>
        <Field label="文档标题"><Input value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} placeholder="缺省取正文首行" /></Field>
      </div>
      <div className="mt-3">
        <div className="mb-1 flex items-center justify-between">
          <span className="text-sm font-medium text-slate-700">招标文件正文 *（粘贴文本，或读取本地 .txt / .md）</span>
          <div className="flex items-center gap-2">
            {fileName && <span className="text-xs text-slate-500">已读取 {fileName}</span>}
            <input ref={fileRef} type="file" accept=".txt,.md,.markdown,text/plain,text/markdown" className="hidden"
              onChange={(e) => { void pickFile(e.target.files?.[0]); e.target.value = '' }} />
            <Button size="sm" icon={<FileUp className="size-3.5" />} onClick={() => fileRef.current?.click()}>选择文件</Button>
          </div>
        </div>
        <TextArea rows={10} value={form.text} onChange={(e) => setForm({ ...form, text: e.target.value })}
          placeholder={'政务云平台采购项目招标文件\n第一章 投标人须知\n1.1 投标人资格要求\n★1.1.2 投标人须具备信息系统集成一级资质，否则投标无效。\n- 1 -\n第二章 评标办法 …'} />
        <div className="mt-1 text-xs text-slate-400">{fmtNum(form.text.length)} 字符 · 以“- N -”“第 N 页”“Page N”页码行分页；★ / ▲ / * 前缀及“废标”“否决”“投标无效”“实质性要求”等识别为强制性条款。</div>
      </div>
      <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
        <label className="flex items-center gap-2 text-sm text-slate-600">
          <input type="checkbox" checked={form.wake} onChange={(e) => setForm({ ...form, wake: e.target.checked })} />
          导入新文档后唤醒 Planner 重新规划（wake_planner）
        </label>
        <Button variant="primary" icon={<Upload className="size-4" />} loading={busy} onClick={submit}>导入并切分</Button>
      </div>
      <div className="mt-3 space-y-2">
        <ErrorBanner error={error} />
        {result && (
          <div className="rounded-md bg-slate-50 px-3 py-2 text-sm text-slate-600">
            文档 #{result.result.document.id}「{result.result.document.title}」{result.result.created ? '新建' : '已存在（幂等）'} ·
            {' '}{result.result.chunks} 个章节/条款 · {result.result.mandatory_chunks} 个强制性条款 · 招标实体 #{result.result.tender_asset_id}
            {result.result.lot_asset_id ? ` · 标段实体 #${result.result.lot_asset_id}` : ''}{result.planner_notified ? ' · 已通知 Planner' : ''}
          </div>
        )}
      </div>
    </Card>
  )
}

function ChunkRow({ c, open, onToggle }: { c: DocumentChunk; open: boolean; onToggle: () => void }) {
  return (
    <div className={cx('border-l-2', c.is_mandatory ? 'border-red-500 bg-red-50/40' : 'border-transparent')}>
      <button onClick={onToggle} className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm hover:bg-slate-50"
        style={{ paddingLeft: `${12 + Math.max(c.level - 1, 0) * 18}px` }}>
        {open ? <ChevronDown className="size-3.5 shrink-0 text-slate-400" /> : <ChevronRight className="size-3.5 shrink-0 text-slate-400" />}
        {c.is_mandatory && <Star className="size-3.5 shrink-0 fill-red-500 text-red-500" />}
        <span className={cx('shrink-0 font-mono text-xs', c.is_mandatory ? 'text-red-700' : 'text-slate-500')}>{c.section_code || '—'}</span>
        <span className={cx('flex-1 truncate', c.level <= 1 && 'font-semibold', c.is_mandatory && 'text-red-800')}>{c.section_title || '（无标题段落）'}</span>
        {c.is_mandatory && <Badge tone="red">强制条款</Badge>}
        <span className="shrink-0 text-xs text-slate-400 tabular-nums">P{c.page_no}</span>
        <span className="w-20 shrink-0 text-right text-xs text-slate-400 tabular-nums">~{fmtNum(c.token_estimate)} tok</span>
      </button>
      {open && (
        <div className="px-3 pb-2" style={{ paddingLeft: `${36 + Math.max(c.level - 1, 0) * 18}px` }}>
          <pre className="rounded-md bg-white p-2 text-xs leading-relaxed whitespace-pre-wrap text-slate-700 ring-1 ring-slate-200">{c.content || '（无正文）'}</pre>
          <div className="mt-1 text-xs text-slate-400">chunk #{c.id} · seq {c.seq} · level {c.level}</div>
        </div>
      )}
    </div>
  )
}

function DocumentTree({ doc }: { doc: DocumentWithChunks }) {
  const [openIDs, setOpen] = useState<Set<number>>(new Set())
  const [onlyMandatory, setOnlyMandatory] = useState(false)
  const [q, setQ] = useState('')
  const chunks = useMemo(() => doc.chunks.filter((c) => (!onlyMandatory || c.is_mandatory) &&
    (!q || `${c.section_code} ${c.section_title} ${c.content ?? ''}`.toLowerCase().includes(q.toLowerCase()))), [doc, onlyMandatory, q])
  const mandatory = doc.chunks.filter((c) => c.is_mandatory).length
  const tokens = doc.chunks.reduce((s, c) => s + c.token_estimate, 0)
  const pages = doc.chunks.reduce((m, c) => Math.max(m, c.page_no), 0)
  const toggle = (id: number) => setOpen((s) => { const n = new Set(s); if (n.has(id)) n.delete(id); else n.add(id); return n })

  return (
    <Card bodyClassName="p-0" title={<><FileText className="size-4 text-slate-500" /> #{doc.id} {doc.title}</>}
      actions={<span className="text-xs text-slate-500">招标编号 {doc.tender_no}{doc.lot_no ? ` / 标段 ${doc.lot_no}` : ''} · {fmtTime(doc.created_at)}</span>}>
      <div className="flex flex-wrap items-center gap-3 border-b border-slate-100 px-4 py-2 text-xs text-slate-500">
        <span>{doc.chunk_count} 个章节/条款</span>
        <span className="text-red-600">{mandatory} 个强制性条款</span>
        <span>约 {pages} 页</span>
        <span>~{fmtNum(tokens)} tokens</span>
        <span className="font-mono" title="内容哈希（幂等键）">{doc.content_hash.slice(0, 12)}</span>
        <div className="ml-auto flex items-center gap-2">
          <Input className="w-48 py-1 text-xs" placeholder="搜索章节/正文" value={q} onChange={(e) => setQ(e.target.value)} />
          <label className="flex items-center gap-1"><input type="checkbox" checked={onlyMandatory} onChange={(e) => setOnlyMandatory(e.target.checked)} />仅强制条款</label>
          <Button size="sm" variant="ghost" onClick={() => setOpen(openIDs.size ? new Set() : new Set(chunks.map((c) => c.id)))}>{openIDs.size ? '全部收起' : '全部展开'}</Button>
        </div>
      </div>
      <div className="max-h-[600px] overflow-y-auto py-1">
        {chunks.length === 0 ? <Empty>没有匹配的段落</Empty> : chunks.map((c) => <ChunkRow key={c.id} c={c} open={openIDs.has(c.id)} onToggle={() => toggle(c.id)} />)}
      </div>
    </Card>
  )
}

export default function DocumentsTab({ taskID, onUploaded }: { taskID: number; onUploaded: () => void }) {
  const docs = useAsync(() => api.listDocuments(taskID, true), [taskID])
  return (
    <div className="space-y-4">
      <UploadCard taskID={taskID} onUploaded={() => { void docs.reload(); onUploaded() }} />
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-semibold text-slate-700">已导入的招标文档（章节切分目录）</h2>
        <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => docs.reload()}>刷新</Button>
      </div>
      <ErrorBanner error={docs.error} onRetry={docs.reload} />
      {docs.loading && !docs.data ? <Spinner /> : !docs.data || docs.data.length === 0 ? <Card><Empty>尚未导入招标文档</Empty></Card>
        : docs.data.map((d) => <DocumentTree key={d.id} doc={d} />)}
    </div>
  )
}
