import { useRef, useState } from 'react'
import { AlertTriangle, BookOpen, Eye, FileUp, RefreshCw, Search, Star, Table2, Trash2, Upload } from 'lucide-react'
import { api, errorText } from '../api/client'
import type { KnowledgeCategory, KnowledgeDocument, KnowledgeHit, KnowledgeUploadResp, RetrievalResult } from '../api/types'
import { RetrievalEvidence } from '../components/RetrievalEvidence'
import { DegradedNotice, SemanticStatusBar, SemanticStatusCard, useRetrievalStatus } from '../features/retrieval/SemanticStatus'
import { IndexManager } from '../features/retrieval/IndexManager'
import { hitSourceText } from '../features/retrieval/retrieval-status'
import { TableView } from '../components/TableView'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Modal, Select, Spinner, Tabs, TextArea, cx, useConfirm, useToast } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { PageFrame } from '../components/layout/AppShell'
import { docFormatLabel, fmtNum, fmtTime, knowledgeCategoryLabel, pageRange } from '../lib/format'

const CATEGORIES: KnowledgeCategory[] = ['historical_bid', 'standard_solution', 'product_spec', 'whitepaper']
const categoryTone: Record<string, 'blue' | 'green' | 'purple' | 'amber'> = {
  historical_bid: 'blue', standard_solution: 'green', product_spec: 'purple', whitepaper: 'amber',
}
const MAX_FILE_BYTES = 64 << 20
const FILE_ACCEPT = '.docx,.pdf,.txt,.md,.markdown,.png,.jpg,.jpeg,.tif,.tiff,.bmp,.webp'

function splitTags(s: string): string[] {
  return s.split(/[,，、;；]/).map((t) => t.trim()).filter(Boolean)
}

function CategoryBadge({ c }: { c: string }) {
  return <Badge tone={categoryTone[c] ?? 'slate'}>{knowledgeCategoryLabel[c] ?? c}</Badge>
}

function UploadCard({ onUploaded }: { onUploaded: () => void }) {
  const toast = useToast()
  const fileRef = useRef<HTMLInputElement>(null)
  const [mode, setMode] = useState<'file' | 'text'>('file')
  const [form, setForm] = useState({ category: 'historical_bid' as KnowledgeCategory, title: '', tags: '', product: '', text: '', file_name: '' })
  const [file, setFile] = useState<File>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<KnowledgeUploadResp>()

  const pickFile = (f: File | undefined) => {
    if (!f) return
    if (/\.(doc|wps)$/i.test(f.name)) { setError('旧版 .doc / .wps 格式不支持，请另存为 .docx 后上传'); return }
    if (f.size > MAX_FILE_BYTES) { setError('文件超过 64 MiB 上限'); return }
    setFile(f)
    setError('')
  }

  const submit = async () => {
    setError('')
    if (mode === 'file' && !file) { setError('请选择要上传的文件'); return }
    if (mode === 'text' && !form.text.trim()) { setError('请粘贴文档正文'); return }
    const extra = form.product.trim() ? { product: form.product.trim() } : undefined
    const base = { category: form.category, title: form.title.trim() || undefined, tags: splitTags(form.tags), extra }
    setBusy(true)
    try {
      const r = mode === 'file' && file
        ? await api.uploadKnowledgeFile({ ...base, file })
        : await api.uploadKnowledgeText({ ...base, text: form.text, file_name: form.file_name.trim() || undefined })
      setResult(r)
      toast(r.created ? 'ok' : 'info', r.created ? `已入库：${r.chunks} 个分块、${r.tables} 张表格` : '相同内容已在知识库中，未重复入库')
      onUploaded()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card title={<><Upload className="size-4 text-blue-600" /> 上传到知识库</>}>
      <div className="grid gap-3 md:grid-cols-4">
        <Field label="类别 *">
          <Select value={form.category} onChange={(e) => setForm({ ...form, category: e.target.value as KnowledgeCategory })}>
            {CATEGORIES.map((c) => <option key={c} value={c}>{knowledgeCategoryLabel[c]}</option>)}
          </Select>
        </Field>
        <Field label="标题"><Input value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} placeholder="缺省取文档标题属性或首行" /></Field>
        <Field label="行业标签" hint="逗号分隔，如 智慧园区, 政务"><Input value={form.tags} onChange={(e) => setForm({ ...form, tags: e.target.value })} placeholder="智慧园区, 安防" /></Field>
        <Field label="产品 / 型号" hint="写入 extra.product，供产品检索精确限定"><Input value={form.product} onChange={(e) => setForm({ ...form, product: e.target.value })} placeholder="可空" /></Field>
      </div>
      <div className="mt-3">
        <Tabs value={mode} onChange={setMode} tabs={[{ key: 'file', label: '上传文件（Word / PDF / 扫描件 / 文本）' }, { key: 'text', label: '粘贴文本 / Markdown' }]} />
      </div>
      {mode === 'file' ? (
        <div className="mt-3">
          <input ref={fileRef} type="file" accept={FILE_ACCEPT} className="hidden" onChange={(e) => { pickFile(e.target.files?.[0]); e.target.value = '' }} />
          <button type="button" onClick={() => fileRef.current?.click()}
            onDragOver={(e) => e.preventDefault()} onDrop={(e) => { e.preventDefault(); pickFile(e.dataTransfer.files?.[0]) }}
            className="flex w-full flex-col items-center gap-1 rounded-md border-2 border-dashed border-slate-300 px-4 py-5 text-sm text-slate-500 hover:border-blue-400 hover:bg-blue-50/40">
            <FileUp className="size-6 text-slate-400" />
            {file ? <span className="font-medium text-slate-700">{file.name}（{fmtNum(Math.round(file.size / 1024))} KB）</span> : <span>点击选择或拖入文件</span>}
            <span className="text-xs text-slate-400">同一文件（相同字节）重复上传自动去重</span>
            <span className="text-xs text-amber-700">知识库上传不经过候选核对：扫描件的识别结果直接入库，可能有错漏，写进标书前须按证据核验。增强解析尚未开放。</span>
          </button>
        </div>
      ) : (
        <div className="mt-3 space-y-2">
          <TextArea rows={8} value={form.text} onChange={(e) => setForm({ ...form, text: e.target.value })}
            placeholder={'# 统一视频平台技术白皮书\n## 1 平台能力\n支持 GB/T 28181 级联……\n\n| 指标 | 参数 |\n| --- | --- |\n| 接入路数 | 10000 |'} />
          <Input value={form.file_name} onChange={(e) => setForm({ ...form, file_name: e.target.value })} placeholder="来源文件名（可选，用于引用展示）" />
        </div>
      )}
      <div className="mt-3 flex justify-end">
        <Button variant="primary" icon={<Upload className="size-4" />} loading={busy} onClick={submit}>解析并入库</Button>
      </div>
      <div className="mt-3 space-y-2">
        <ErrorBanner error={error} />
        {result && (
          <div className="rounded-md bg-slate-50 px-3 py-2 text-sm text-slate-600">
            知识库文档 #{result.document.id}「{result.document.title}」{result.created ? '新建' : '已存在（去重）'} · {docFormatLabel[result.format] ?? result.format} ·
            {' '}{result.page_count} 页 · {result.chunks} 个分块 · {result.tables} 张表格
            {result.warnings?.map((w) => <div key={w} className="mt-1 flex items-center gap-1 text-amber-700"><AlertTriangle className="size-3.5" />{w}</div>)}
          </div>
        )}
      </div>
    </Card>
  )
}

function DocumentDialog({ id, onClose }: { id: number | null; onClose: () => void }) {
  const detail = useAsync(() => api.getKnowledge(id ?? 0, true), [id], id !== null)
  const [open, setOpen] = useState<Set<number>>(new Set())
  const doc = detail.data?.document
  return (
    <Modal wide open={id !== null} onClose={onClose} title={doc ? <span className="flex items-center gap-2">#{doc.id} {doc.title} <CategoryBadge c={doc.category} /></span> : '知识库文档'}>
      <ErrorBanner error={detail.error} onRetry={detail.reload} />
      {detail.loading && !detail.data ? <Spinner /> : doc && (
        <div className="space-y-3">
          <div className="flex flex-wrap gap-2 text-xs text-slate-500">
            {doc.file_name && <Badge tone="blue">{doc.file_name}</Badge>}
            {doc.tags.map((t) => <Badge key={t}>{t}</Badge>)}
            <span>{doc.page_count} 页 · {doc.chunk_count} 个分块 · {doc.table_count} 张表格</span>
            {doc.parse_meta && <span>解析器 {doc.parse_meta.parser}</span>}
            {doc.extra && Object.keys(doc.extra).length > 0 && <span className="font-mono">{JSON.stringify(doc.extra)}</span>}
            <span>上传 {fmtTime(doc.created_at)}{doc.created_by ? ` · ${doc.created_by}` : ''}</span>
          </div>
          <div className="divide-y divide-slate-100 rounded-md ring-1 ring-slate-200">
            {detail.data?.chunks.map((c) => (
              <div key={c.id}>
                <button className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm hover:bg-slate-50"
                  style={{ paddingLeft: `${12 + Math.max(c.level - 1, 0) * 14}px` }}
                  onClick={() => setOpen((s) => { const n = new Set(s); if (n.has(c.id)) n.delete(c.id); else n.add(c.id); return n })}>
                  <span className="font-mono text-xs text-slate-400">#{c.seq}</span>
                  {c.is_mandatory && <Star className="size-3.5 fill-red-500 text-red-500" />}
                  <span className="flex-1 truncate">{c.section_path || c.section_title || '（无标题）'}</span>
                  {c.table_count > 0 && <Badge tone="blue"><Table2 className="size-3" />{c.table_count}</Badge>}
                  <span className="text-xs text-slate-400 tabular-nums">{pageRange(c.page_start, c.page_end)}</span>
                </button>
                {open.has(c.id) && (
                  <div className="space-y-2 px-3 pb-2">
                    {c.tables?.map((t, i) => <TableView key={i} table={t} index={i} />)}
                    <pre className="rounded-md bg-slate-50 p-2 text-xs whitespace-pre-wrap text-slate-700">{c.content}</pre>
                  </div>
                )}
              </div>
            ))}
          </div>
        </div>
      )}
    </Modal>
  )
}

function LibraryTab() {
  const toast = useToast()
  const confirm = useConfirm()
  const [filter, setFilter] = useState({ category: '', tags: '', q: '' })
  const [applied, setApplied] = useState(filter)
  const [viewing, setViewing] = useState<number | null>(null)
  const docs = useAsync(() => api.listKnowledge({ category: applied.category, tags: splitTags(applied.tags).join(','), q: applied.q, limit: 500 }), [applied])

  const remove = async (d: KnowledgeDocument) => {
    if (!(await confirm({ title: `删除知识库文档 #${d.id}「${d.title}」？`, message: '其全部分块将一并删除，且不可恢复；引用它的证据会失效。', confirmLabel: '删除', danger: true }))) return
    try {
      await api.deleteKnowledge(d.id)
      toast('ok', `已删除 #${d.id}`)
      void docs.reload()
    } catch (e) {
      toast('error', errorText(e))
    }
  }

  return (
    <div className="space-y-4">
      <UploadCard onUploaded={() => void docs.reload()} />
      <Card bodyClassName="p-0" title={<><BookOpen className="size-4 text-slate-500" /> 知识库文档</>}
        actions={<Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => docs.reload()}>刷新</Button>}>
        <div className="flex flex-wrap items-end gap-2 border-b border-slate-100 px-4 py-2">
          <div className="w-36">
            <Select className="py-1 text-xs" value={filter.category} onChange={(e) => { const f = { ...filter, category: e.target.value }; setFilter(f); setApplied(f) }}>
              <option value="">全部类别</option>
              {CATEGORIES.map((c) => <option key={c} value={c}>{knowledgeCategoryLabel[c]}</option>)}
            </Select>
          </div>
          <div className="w-52">
            <Input className="py-1 text-xs" placeholder="标签（逗号分隔，任一命中）" value={filter.tags} onChange={(e) => setFilter({ ...filter, tags: e.target.value })}
              onKeyDown={(e) => e.key === 'Enter' && setApplied(filter)} />
          </div>
          <div className="w-56">
            <Input className="py-1 text-xs" placeholder="标题 / 文件名" value={filter.q} onChange={(e) => setFilter({ ...filter, q: e.target.value })}
              onKeyDown={(e) => e.key === 'Enter' && setApplied(filter)} />
          </div>
          <Button size="sm" icon={<Search className="size-3.5" />} onClick={() => setApplied(filter)}>筛选</Button>
        </div>
        <ErrorBanner error={docs.error} onRetry={docs.reload} />
        {docs.loading && !docs.data ? <Spinner /> : !docs.data || docs.data.length === 0 ? <Empty>知识库中暂无匹配的文档</Empty> : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="bg-slate-50 text-left text-xs text-slate-500">
                <tr>
                  <th className="px-4 py-2">文档</th><th className="px-2 py-2">类别</th><th className="px-2 py-2">标签</th>
                  <th className="px-2 py-2 text-right">页 / 分块 / 表格</th><th className="px-2 py-2">上传时间</th><th className="px-4 py-2 text-right">操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {docs.data.map((d) => (
                  <tr key={d.id} className="hover:bg-slate-50">
                    <td className="px-4 py-2">
                      <div className="font-medium text-slate-800">#{d.id} {d.title}</div>
                      <div className="text-xs text-slate-400">{d.file_name || '文本录入'}{d.extra && typeof d.extra.product === 'string' ? ` · 产品 ${d.extra.product}` : ''}</div>
                    </td>
                    <td className="px-2 py-2"><CategoryBadge c={d.category} /></td>
                    <td className="px-2 py-2"><div className="flex flex-wrap gap-1">{d.tags.map((t) => <Badge key={t}>{t}</Badge>)}</div></td>
                    <td className="px-2 py-2 text-right text-xs text-slate-600 tabular-nums">{d.page_count} / {d.chunk_count} / {d.table_count}</td>
                    <td className="px-2 py-2 text-xs text-slate-500">{fmtTime(d.created_at)}{d.created_by ? <div>{d.created_by}</div> : null}</td>
                    <td className="px-4 py-2 text-right">
                      <div className="flex justify-end gap-1">
                        <Button size="sm" variant="ghost" icon={<Eye className="size-3.5" />} onClick={() => setViewing(d.id)}>查看</Button>
                        <Button size="sm" variant="ghost" className="text-red-600" icon={<Trash2 className="size-3.5" />} onClick={() => void remove(d)}>删除</Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
      <DocumentDialog id={viewing} onClose={() => setViewing(null)} />
    </div>
  )
}

function ScoreBar({ label, value, tone }: { label: string; value: number; tone: string }) {
  return (
    <span className="flex items-center gap-1" title={`${label} ${value.toFixed(4)}`}>
      <span className="whitespace-nowrap text-slate-400">{label}</span>
      <span className="h-1.5 w-12 overflow-hidden rounded bg-slate-100"><span className={cx('block h-full', tone)} style={{ width: `${Math.min(Math.max(value, 0), 1) * 100}%` }} /></span>
    </span>
  )
}

function HitCard({ h, maxFts }: { h: KnowledgeHit; maxFts: number }) {
  const [open, setOpen] = useState(false)
  const c = h.citation
  return (
    <div className="rounded-md border border-slate-200 bg-white">
      <div className="flex flex-wrap items-center gap-2 px-3 py-2">
        <span className="rounded bg-blue-600 px-1.5 py-0.5 text-xs font-semibold text-white tabular-nums">{h.score.toFixed(3)}</span>
        <CategoryBadge c={c.category} />
        <span className="font-medium text-slate-800">《{c.doc_title}》</span>
        <span className="text-sm text-slate-600">{c.section_path || c.section_title}</span>
        <Badge tone="purple">{pageRange(c.page_start, c.page_end)}</Badge>
        {h.chunk.table_count > 0 && <Badge tone="blue"><Table2 className="size-3" />{h.chunk.table_count}</Badge>}
        <div className="ml-auto flex items-center gap-3 text-xs">
          <ScoreBar label="全文" value={maxFts > 0 ? h.fts_rank / maxFts : 0} tone="bg-blue-500" />
          <ScoreBar label="向量" value={h.vector_score} tone="bg-violet-500" />
          <ScoreBar label="关键词" value={h.keyword_score} tone="bg-emerald-500" />
        </div>
      </div>
      <div className="border-t border-slate-100 px-3 py-2 text-xs text-slate-500">
        引用：doc #{c.doc_id} · seq {c.seq} · {c.section_code || '—'} · {c.file_name || '文本录入'} · {hitSourceText(h)}
        {h.tags.length > 0 && ` · 标签 ${h.tags.join('、')}`}
        <Button size="sm" variant="ghost" className="ml-2" onClick={() => setOpen(!open)}>{open ? '收起' : '查看正文与表格'}</Button>
      </div>
      {open ? (
        <div className="space-y-2 px-3 pb-3">
          {h.chunk.tables?.map((t, i) => <TableView key={i} table={t} index={i} />)}
          <pre className="rounded-md bg-slate-50 p-2 text-xs whitespace-pre-wrap text-slate-700">{h.chunk.content}</pre>
        </div>
      ) : (
        <p className="line-clamp-2 px-3 pb-2 text-xs text-slate-600">{h.chunk.content}</p>
      )}
    </div>
  )
}

function SearchTab() {
  const retrieval = useRetrievalStatus()
  const [form, setForm] = useState({ q: '', product: '', tags: '', prefer_tables: false, categories: new Set<KnowledgeCategory>() })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [hits, setHits] = useState<KnowledgeHit[]>()
  const [result, setResult] = useState<RetrievalResult>()

  const run = async () => {
    setError('')
    if (!form.q.trim() && !form.product.trim()) { setError('请输入检索词或产品名'); return }
    setBusy(true)
    try {
      const r = await api.searchKnowledge({ q: form.q, product: form.product.trim() || undefined, tags: splitTags(form.tags),
        category: [...form.categories], prefer_tables: form.prefer_tables, limit: 20 })
      setHits(r.hits)
      setResult(r)
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
      // 这次检索本身可能发现模型故障或恢复：状态条不能和刚返回的结果相互矛盾。
      void retrieval.reload()
    }
  }
  const toggleCat = (c: KnowledgeCategory) => setForm((f) => {
    const n = new Set(f.categories)
    if (n.has(c)) n.delete(c); else n.add(c)
    return { ...f, categories: n }
  })

  return (
    <div className="space-y-4">
      <SemanticStatusBar retrieval={retrieval} />
      <Card title={<><Search className="size-4 text-blue-600" /> 混合检索预览</>}>
        <div className="grid gap-3 md:grid-cols-3">
          <Field label="检索词" hint="空格分隔；词间 OR，命中越多越靠前" className="md:col-span-2">
            <Input value={form.q} onChange={(e) => setForm({ ...form, q: e.target.value })} onKeyDown={(e) => e.key === 'Enter' && void run()} placeholder="视频监控 云边协同 等保三级" />
          </Field>
          <Field label="产品 / 型号（必须出现）"><Input value={form.product} onChange={(e) => setForm({ ...form, product: e.target.value })} placeholder="可空" /></Field>
          <Field label="行业标签"><Input value={form.tags} onChange={(e) => setForm({ ...form, tags: e.target.value })} placeholder="逗号分隔，任一命中" /></Field>
          <div className="md:col-span-2">
            <span className="mb-1 block text-sm font-medium text-slate-700">类别（不选为全部）</span>
            <div className="flex flex-wrap items-center gap-3 text-sm text-slate-600">
              {CATEGORIES.map((c) => (
                <label key={c} className="flex items-center gap-1"><input type="checkbox" checked={form.categories.has(c)} onChange={() => toggleCat(c)} />{knowledgeCategoryLabel[c]}</label>
              ))}
              <label className="flex items-center gap-1"><input type="checkbox" checked={form.prefer_tables} onChange={(e) => setForm({ ...form, prefer_tables: e.target.checked })} />含参数表优先</label>
            </div>
          </div>
        </div>
        <div className="mt-3 flex items-center justify-between gap-2">
          <span className="text-xs text-slate-400">候选按全文与语义召回融合，再重排。服务不可用时会说明降级原因；命中与引用均需核验。</span>
          <Button variant="primary" icon={<Search className="size-4" />} loading={busy} onClick={run}>检索</Button>
        </div>
        <div className="mt-2"><ErrorBanner error={error} /></div>
      </Card>
      {result && <Card><div className="space-y-3"><p className="text-sm">检索记录 #{result.run_id} · {result.cached ? '缓存命中' : '新检索'}</p><DegradedNotice degraded={result.degraded} diagnostics={result.diagnostics} /></div></Card>}
      {result && <RetrievalEvidence key={result.run_id} runID={result.run_id} />}
      {hits && (hits.length === 0 ? <Card><Empty>没有命中，尝试更少或更宽泛的关键词</Empty></Card> : (
        <div className="space-y-2">
          <div className="text-sm text-slate-500">共 {hits.length} 条命中</div>
          {hits.map((h) => <HitCard key={h.chunk.id} h={h} maxFts={Math.max(...hits.map((x) => x.fts_rank))} />)}
        </div>
      ))}
    </div>
  )
}

function IndexesTab() {
  const retrieval = useRetrievalStatus()
  return <div className="space-y-4"><SemanticStatusCard retrieval={retrieval} /><IndexManager retrieval={retrieval} /></div>
}

export default function KnowledgePage() {
  const [tab, setTab] = useState<'library' | 'search' | 'indexes'>('library')
  return (
    <PageFrame title="企业方案知识库" description="沉淀历史标书、标准技术方案、产品规格书与技术白皮书；写作检索复用时引用附带文档、章节与页码。历史承诺不等于当前能力，引用前请核验。">
      <div className="space-y-4">
        <Tabs value={tab} onChange={setTab} tabs={[{ key: 'library', label: '文档库' }, { key: 'search', label: '检索预览' }, { key: 'indexes', label: '语义索引' }]} />
        {tab === 'library' ? <LibraryTab /> : tab === 'search' ? <SearchTab /> : <IndexesTab />}
      </div>
    </PageFrame>
  )
}
