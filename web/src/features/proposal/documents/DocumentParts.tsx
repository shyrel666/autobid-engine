import { useEffect, useMemo, useRef, useState } from 'react'
import { AlertTriangle, ChevronDown, ChevronRight, FileText, FileUp, Star, Table2, Upload } from 'lucide-react'
import { api, errorText } from '../../../api/client'
import type { DocumentChunk, DocumentWithChunks, UploadArchiveResp, UploadDocumentResp } from '../../../api/types'
import { TableView } from '../../../components/TableView'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Select, Tabs, TextArea, cx, useToast } from '../../../components/ui'
import { docFormatLabel, docRoleLabel, fmtNum, fmtTime, pageRange } from '../../../lib/format'

const MAX_TEXT_BYTES = 32 << 20 // 与后端 maxDocumentBody 一致（JSON 文本上传）
const MAX_FILE_BYTES = 64 << 20 // 与后端 maxUploadBytes 一致（multipart 文件上传）
const FILE_ACCEPT = '.docx,.pdf,.txt,.md,.markdown,.png,.jpg,.jpeg,.tif,.tiff,.bmp,.webp,.xlsx,.csv,.tsv,.zip'
const FILE_RE = /\.(docx|pdf|txt|md|markdown|png|jpe?g|tiff?|bmp|webp|xlsx|csv|tsv|zip)$/i

const EMPTY_FORM = { tender_no: '', lot_no: '', title: '', text: '', wake: true, bootstrap_goals: false, doc_role: '', published_at: '' }

/** dirtyRef 同步报告关闭保护状态：false 无需确认；true 有未导入的填写；字符串为上传进行中的说明。 */
export function UploadCard({ taskID, onUploaded, bare, dirtyRef }:
  { taskID: number; onUploaded: (r: UploadDocumentResp | UploadArchiveResp) => void; bare?: boolean; dirtyRef?: { current: boolean | string } }) {
  const toast = useToast()
  const fileRef = useRef<HTMLInputElement>(null)
  const [mode, setMode] = useState<'file' | 'text'>('file')
  const [form, setForm] = useState(EMPTY_FORM)
  const [file, setFile] = useState<File>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<UploadDocumentResp | UploadArchiveResp>()
  // 成功导入后以当时的填写为基线：未再修改就关闭不需要确认。
  const [saved, setSaved] = useState<{ form: typeof EMPTY_FORM; file?: File }>({ form: EMPTY_FORM })
  const dirty: boolean | string = busy
    ? '文件仍在上传解析。关闭对话框不会取消这次导入，完成后文件列表会自动刷新，但不再显示解析结果摘要。'
    : file !== saved.file || JSON.stringify(form) !== JSON.stringify(saved.form)
  if (dirtyRef) dirtyRef.current = dirty

  const pickFile = (f: File | undefined) => {
    if (!f) return
    if (/\.(doc|wps|rtf|xls|xlsm|docm)$/i.test(f.name)) { setError('旧版 / 宏格式不支持，请另存为 .docx / .xlsx 后上传'); return }
    if (!FILE_RE.test(f.name)) { setError('支持 .docx / .pdf / .xlsx / .csv / .tsv / .zip / .txt / .md 与 PNG/JPEG/TIFF 等扫描图片'); return }
    if (f.size > MAX_FILE_BYTES) { setError('文件超过 64 MiB 上限'); return }
    setFile(f)
    setError('')
  }

  const submit = async () => {
    setError('')
    if (!form.tender_no.trim()) { setError('招标编号为必填项'); return }
    if (mode === 'file' && !file) { setError('请选择要上传的文件'); return }
    if (mode === 'text') {
      if (!form.text.trim()) { setError('招标文件正文为必填项'); return }
      if (new Blob([form.text]).size > MAX_TEXT_BYTES) { setError('正文超过 32 MiB 上限'); return }
    }
    setBusy(true)
    try {
      const base = { tender_no: form.tender_no.trim(), lot_no: form.lot_no.trim(), title: form.title.trim(), wake_planner: form.wake,
        bootstrap_goals: form.bootstrap_goals, doc_role: form.doc_role, published_at: form.published_at }
      const r = mode === 'file' && file
        ? await api.uploadDocumentFile(taskID, { ...base, file })
        : await api.uploadDocument(taskID, { ...base, text: form.text })
      setResult(r)
      setSaved({ form, file })
      if ('document' in r.result) {
        toast(r.result.created ? 'ok' : 'info', r.result.created
          ? `已解析 ${r.result.page_count} 页，切分 ${r.result.chunks} 个章节/条款（${r.result.mandatory_chunks} 个强制性条款、${r.result.tables} 张表格）`
          : '相同文件已导入过，未重复解析')
      } else {
        toast('ok', `已登记招标包原件 #${r.result.original_file_id}，清单 ${r.result.archive.entries.length} 个子文件`)
      }
      onUploaded(r)
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  const body = (
    <>
      <div className="grid gap-3 md:grid-cols-3">
        <Field label="招标编号 *" hint="字段 tender_no"><Input value={form.tender_no} onChange={(e) => setForm({ ...form, tender_no: e.target.value })} placeholder="ZB-2026-001" /></Field>
        <Field label="标段" hint="字段 lot_no"><Input value={form.lot_no} onChange={(e) => setForm({ ...form, lot_no: e.target.value })} placeholder="可空，表示整个项目" /></Field>
        <Field label="文档标题"><Input value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} placeholder="缺省取文档标题属性或首行" /></Field>
        <Field label="文件角色"><Select value={form.doc_role} onChange={(e) => setForm({ ...form, doc_role: e.target.value })}>
          <option value="">按标题推断（更正 / 澄清 / 答疑）</option>
          {Object.entries(docRoleLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
        </Select></Field>
        <Field label="公告发布日期"><Input type="date" value={form.published_at} onChange={(e) => setForm({ ...form, published_at: e.target.value })} /></Field>
      </div>
      <p className="mt-1 text-xs text-slate-400">更正、澄清与原招标文件一起导入同一任务：后发布的文件优先（例如投标截止时间被顺延）。</p>
      <div className="mt-3">
        <Tabs value={mode} onChange={setMode} tabs={[{ key: 'file', label: '上传文件（Word / PDF / 扫描件 / 文本）' }, { key: 'text', label: '粘贴文本' }]} />
      </div>
      {mode === 'file' ? (
        <div className="mt-3">
          <input ref={fileRef} type="file" accept={FILE_ACCEPT} className="hidden"
            onChange={(e) => { pickFile(e.target.files?.[0]); e.target.value = '' }} />
          <button type="button" onClick={() => fileRef.current?.click()}
            onDragOver={(e) => e.preventDefault()} onDrop={(e) => { e.preventDefault(); pickFile(e.dataTransfer.files?.[0]) }}
            className="flex w-full flex-col items-center gap-1 rounded-md border-2 border-dashed border-slate-300 px-4 py-6 text-sm text-slate-500 hover:border-blue-400 hover:bg-blue-50/40">
            <FileUp className="size-6 text-slate-400" />
            {file ? <span className="font-medium text-slate-700">{file.name}（{fmtNum(Math.round(file.size / 1024))} KB）</span> : <span>点击选择或拖入文件</span>}
            <span className="text-xs text-slate-400">.docx 按标题样式分级并还原合并单元格表格；.pdf 还原页码范围与跨页表格；扫描件经服务端 OCR（未配置时返回 422）</span>
          </button>
        </div>
      ) : (
        <div className="mt-3">
          <TextArea rows={10} value={form.text} onChange={(e) => setForm({ ...form, text: e.target.value })}
            placeholder={'政务云平台采购项目招标文件\n第一章 投标人须知\n1.1 投标人资格要求\n★1.1.2 投标人须具备信息系统集成一级资质，否则投标无效。\n- 1 -\n第二章 评标办法 …'} />
          <div className="mt-1 text-xs text-slate-400">{fmtNum(form.text.length)} 字符 · 以“- N -”“第 N 页”“Page N”页码行分页；★ / ▲ / * 前缀及“废标”“否决”“投标无效”“实质性要求”等识别为强制性条款。</div>
        </div>
      )}
      <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-4">
          <label className="flex items-center gap-2 text-sm text-slate-600">
            <input type="checkbox" checked={form.wake} onChange={(e) => setForm({ ...form, wake: e.target.checked })} />
            导入后通知推演 Planner 重新规划（运行中的项目才会生效）
          </label>
          <label className="flex items-center gap-2 text-sm text-slate-600">
            <input type="checkbox" checked={form.bootstrap_goals} onChange={(e) => setForm({ ...form, bootstrap_goals: e.target.checked })} />
            自动拆解招标目标（否决项 / 偏离表 / 评分项）并生成大纲草案
          </label>
        </div>
        <Button variant="primary" icon={<Upload className="size-4" />} loading={busy} onClick={submit}>{busy ? '正在上传并解析' : '导入并解析'}</Button>
      </div>
      <div className="mt-3 space-y-2">
        <ErrorBanner error={error} />
        {result && ('document' in result.result ? (
          <div className="rounded-md bg-slate-50 px-3 py-2 text-sm text-slate-600">
            文档 #{result.result.document.id}「{result.result.document.title}」{result.result.created ? '新建' : '已存在（幂等）'} ·
            {' '}{docFormatLabel[result.result.format] ?? result.result.format} · {result.result.page_count > 0 ? `${result.result.page_count} 页` : '表格文档（无物理页码）'} · {result.result.chunks} 个章节/条款 ·
            {' '}{result.result.mandatory_chunks} 个强制性条款 · {result.result.tables} 张表格 · 招标实体 #{result.result.tender_asset_id}
            {result.result.lot_asset_id ? ` · 标段实体 #${result.result.lot_asset_id}` : ''}{result.planner_notified ? ' · 已通知 Planner' : ''}
            {'goals_bootstrapped' in result && result.goals_bootstrapped && (
              <div className="mt-1 text-xs text-slate-600">
                目标分解：提取要求 {result.goals_bootstrapped.extract.created + result.goals_bootstrapped.extract.updated + result.goals_bootstrapped.extract.unchanged} 条 ·
                同步要素 {result.goals_bootstrapped.synced_assets} 个 ·
                初始化图谱目标节点 {(result.goals_bootstrapped.goals ?? []).map((g) => `${g.code}(#${g.node_id})`).join('、') || '无'}
                {result.goals_bootstrapped.outline_created ? ` · 已生成大纲草案 v${result.goals_bootstrapped.outline_version}` : ''}
              </div>
            )}
            {result.result.warnings?.map((w) => <div key={w} className="mt-1 flex items-center gap-1 text-amber-700"><AlertTriangle className="size-3.5" />{w}</div>)}
          </div>
        ) : (
          <div className="rounded-md bg-slate-50 px-3 py-2 text-sm text-slate-600">
            招标包原件 #{result.result.original_file_id} · 已登记 {result.result.archive.entries.length} 个子文件 · 共 {fmtNum(result.result.archive.total_bytes)} 字节
            <div className="mt-1 text-xs text-slate-500">{result.result.note}</div>
            <ul className="mt-2 space-y-1 text-xs">
              {result.result.archive.entries.map((e) => (
                <li key={e.path} className="flex flex-wrap items-center gap-1.5">
                  <span className="font-mono text-slate-700">{e.path}</span>
                  <Badge tone="slate">{docFormatLabel[e.format] ?? e.format}</Badge>
                  <span>{fmtNum(e.size)} B</span>
                  <span className="font-mono text-slate-400" title="子文件 SHA-256">{e.sha256.slice(0, 12)}</span>
                  {e.file_id ? <span>文件 #{e.file_id}</span> : null}
                </li>
              ))}
            </ul>
            {result.result.archive.warnings?.map((w) => <div key={w} className="mt-1 flex items-center gap-1 text-amber-700"><AlertTriangle className="size-3.5" />{w}</div>)}
          </div>
        ))}
      </div>
    </>
  )
  if (bare) return body
  return <Card title={<><Upload className="size-4" /> 导入招标文件</>}>{body}</Card>
}

function ChunkRow({ c, open, onToggle, focused }: { c: DocumentChunk; open: boolean; onToggle: () => void; focused?: boolean }) {
  const boxes = c.bbox_meta?.boxes ?? []
  return (
    <div id={`chunk-${c.id}`} className={cx('border-l-2', c.is_mandatory ? 'border-red-500 bg-red-50/40' : 'border-transparent', focused && 'outline outline-1 outline-fg')}>
      <button type="button" aria-expanded={open} onClick={onToggle} className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm hover:bg-slate-50"
        style={{ paddingLeft: `${12 + Math.max(c.level - 1, 0) * 18}px` }}>
        {open ? <ChevronDown className="size-3.5 shrink-0 text-slate-400" /> : <ChevronRight className="size-3.5 shrink-0 text-slate-400" />}
        {c.is_mandatory && <Star className="size-3.5 shrink-0 fill-red-500 text-red-500" />}
        <span className={cx('shrink-0 font-mono text-xs', c.is_mandatory ? 'text-red-700' : 'text-slate-500')}>{c.section_code || '—'}</span>
        <span className={cx('flex-1 truncate', c.level <= 1 && 'font-semibold', c.is_mandatory && 'text-red-800')}>{c.section_title || '（无标题段落）'}</span>
        {c.table_count > 0 && <Badge tone="blue"><Table2 className="size-3" />{c.table_count}</Badge>}
        {c.is_mandatory && <Badge tone="red">强制条款</Badge>}
        <span className={cx('shrink-0 text-xs tabular-nums', c.page_is_physical === false ? 'text-blue-600' : c.page_end > c.page_start ? 'text-violet-600' : 'text-slate-400')}>
          {c.page_is_physical === false ? `表格 ${c.tables?.[0]?.range ?? ''}` : pageRange(c.page_start || c.page_no, c.page_end)}
        </span>
        <span className="w-20 shrink-0 text-right text-xs text-slate-400 tabular-nums">~{fmtNum(c.token_estimate)} tok</span>
      </button>
      {open && (
        <div className="space-y-2 px-3 pb-2" style={{ paddingLeft: `${36 + Math.max(c.level - 1, 0) * 18}px` }}>
          {c.tables?.map((t, i) => <TableView key={i} table={t} index={i} />)}
          <pre className="rounded-md bg-white p-2 text-xs leading-relaxed whitespace-pre-wrap text-slate-700 ring-1 ring-slate-200">{c.content || '（无正文）'}</pre>
          <div className="text-xs text-slate-400">
            chunk #{c.id} · seq {c.seq} · level {c.level} · {c.page_is_physical === false
              ? `工作表 ${c.tables?.[0]?.sheet ?? '—'} · ${c.tables?.[0]?.range ?? '—'}`
              : pageRange(c.page_start || c.page_no, c.page_end)}
            {c.bbox_meta?.block_start !== undefined && ` · 来源块 ${c.bbox_meta.block_start}–${c.bbox_meta.block_end}`}
            {boxes.map((b) => ` · 第${b.page}页 [${Math.round(b.x0)},${Math.round(b.y0)},${Math.round(b.x1)},${Math.round(b.y1)}]${b.confidence ? ` 置信度 ${(b.confidence * 100).toFixed(0)}%` : ''}`).join('')}
          </div>
        </div>
      )}
    </div>
  )
}

export function DocumentTree({ doc, focusChunk }: { doc: DocumentWithChunks; focusChunk?: number }) {
  const [openIDs, setOpen] = useState<Set<number>>(() => new Set(focusChunk ? [focusChunk] : []))
  useEffect(() => {
    if (!focusChunk) return
    setOpen((s) => new Set([...s, focusChunk]))
    requestAnimationFrame(() => document.getElementById(`chunk-${focusChunk}`)?.scrollIntoView({ block: 'center' }))
  }, [focusChunk, doc.id])
  const [onlyMandatory, setOnlyMandatory] = useState(false)
  const [onlyTables, setOnlyTables] = useState(false)
  const [q, setQ] = useState('')
  const chunks = useMemo(() => doc.chunks.filter((c) => (!onlyMandatory || c.is_mandatory) && (!onlyTables || c.table_count > 0) &&
    (!q || `${c.section_code} ${c.section_title} ${c.content ?? ''}`.toLowerCase().includes(q.toLowerCase()))), [doc, onlyMandatory, onlyTables, q])
  const mandatory = doc.chunks.filter((c) => c.is_mandatory).length
  const tables = doc.chunks.reduce((s, c) => s + c.table_count, 0)
  const tokens = doc.chunks.reduce((s, c) => s + c.token_estimate, 0)
  const pages = doc.page_count || doc.chunks.reduce((m, c) => c.page_is_physical === false ? m : Math.max(m, c.page_end || c.page_no), 0)
  const meta = doc.parse_meta
  const toggle = (id: number) => setOpen((s) => { const n = new Set(s); if (n.has(id)) n.delete(id); else n.add(id); return n })

  return (
    <Card bodyClassName="p-0" title={<><FileText className="size-4 text-muted" /> {doc.title}</>} subtitle="解析原文片段：按章节 / 条款切分，页码为解析结果，不等于原始 PDF 版式"
      actions={<span className="text-xs text-slate-500">招标编号 {doc.tender_no}{doc.lot_no ? ` / 标段 ${doc.lot_no}` : ''} · {fmtTime(doc.created_at)}</span>}>
      <div className="flex flex-wrap items-center gap-3 border-b border-slate-100 px-4 py-2 text-xs text-slate-500">
        <Badge tone={doc.doc_role === 'tender' ? 'slate' : 'purple'}>{docRoleLabel[doc.doc_role] ?? doc.doc_role}{doc.published_at ? ` · ${doc.published_at.slice(0, 10)}` : ''}</Badge>
        {doc.file_name && <Badge tone="blue">{doc.file_name}</Badge>}
        {doc.original_file_id
          ? <Badge tone="green">原件已留存 #{doc.original_file_id}{doc.parse_revision ? ` · 修订 ${doc.parse_revision}` : ''}</Badge>
          : <Badge tone="amber">原件未留存</Badge>}
        {meta && <span>{docFormatLabel[meta.format] ?? meta.format} · 解析器 {meta.parser}{meta.ocr ? ` · OCR ${meta.ocr}（第 ${meta.ocr_pages?.join('、')} 页）` : ''}</span>}
        <span>{doc.chunk_count} 个章节/条款</span>
        <span className="text-red-600">{mandatory} 个强制性条款</span>
        <span>{tables} 张表格</span>
        {pages > 0 ? <span>{pages} 页</span> : <span>表格文档（无物理页码）</span>}
        <span>~{fmtNum(tokens)} tokens</span>
        <span className="font-mono" title="内容哈希（幂等键）">{doc.content_hash.slice(0, 12)}</span>
        <div className="ml-auto flex items-center gap-2">
          <Input className="w-48 py-1 text-xs" placeholder="搜索章节/正文" value={q} onChange={(e) => setQ(e.target.value)} />
          <label className="flex items-center gap-1 whitespace-nowrap"><input type="checkbox" checked={onlyMandatory} onChange={(e) => setOnlyMandatory(e.target.checked)} />仅强制条款</label>
          <label className="flex items-center gap-1 whitespace-nowrap"><input type="checkbox" checked={onlyTables} onChange={(e) => setOnlyTables(e.target.checked)} />仅含表格</label>
          <Button size="sm" variant="ghost" onClick={() => setOpen(openIDs.size ? new Set() : new Set(chunks.map((c) => c.id)))}>{openIDs.size ? '全部收起' : '全部展开'}</Button>
        </div>
      </div>
      {meta?.warnings && meta.warnings.length > 0 && (
        <div className="space-y-0.5 border-b border-amber-100 bg-amber-50 px-4 py-1.5 text-xs text-amber-800">
          {meta.warnings.map((w) => <div key={w} className="flex items-center gap-1"><AlertTriangle className="size-3.5" />{w}</div>)}
        </div>
      )}
      <div className="py-1">
        {chunks.length === 0 ? <Empty>没有匹配的段落</Empty> : chunks.map((c) => <ChunkRow key={c.id} c={c} focused={c.id === focusChunk} open={openIDs.has(c.id)} onToggle={() => toggle(c.id)} />)}
      </div>
    </Card>
  )
}
