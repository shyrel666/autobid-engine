import { useRef, useState } from 'react'
import { RefreshCw, Upload } from 'lucide-react'
import { api } from '../../../api/client'
import type { UploadArchiveResp, UploadDocumentResp } from '../../../api/types'
import { Badge, Button, Card, ErrorBanner, IconButton, Modal, Notice, Spinner, cx } from '../../../components/ui'
import { useAsync } from '../../../hooks/useAsync'
import { docRoleLabel, fmtNum } from '../../../lib/format'
import { useSearchParams } from '../../../lib/router'
import { optionalId } from '../../../lib/routes'
import { useTaskWorkspace, useTick } from '../../task-workspace/TaskWorkspaceProvider'
import { DocumentTree, UploadCard } from './DocumentParts'
import { ProcessingPanel } from './ProcessingPanel'

// 招标解读 · 招标包文件：左侧文件列表、右侧解析内容；?doc= 选择文件，?chunk= 定位解析片段（来自要求的原文位置）。
// 上传与解析期间只显示真实请求的等待状态，不显示没有接口支撑的解析百分比。
export default function DocumentsPage() {
  const { taskId, invalidate } = useTaskWorkspace()
  const tick = useTick('documents')
  const docs = useAsync(() => api.listDocuments(taskId, true), [taskId, tick])
  const [params, setQuery] = useSearchParams()
  const [importing, setImporting] = useState(false)
  const importDirty = useRef<boolean | string>(false)
  const docParam = optionalId(params, 'doc')
  const chunkParam = optionalId(params, 'chunk')
  const list = docs.data ?? []
  const selected = docParam && !Number.isNaN(docParam) ? list.find((d) => d.id === docParam) : list[0]
  const onUploaded = (r: UploadDocumentResp | UploadArchiveResp) => {
    invalidate('document_imported')
    if ('document' in r.result) setQuery({ doc: r.result.document.id, chunk: undefined })
  }

  if (docs.loading && !docs.data) return <Spinner />
  if (!docs.data) return <ErrorBanner error={docs.error} onRetry={docs.reload} />
  if (list.length === 0) {
    return (
      <div className="mx-auto max-w-4xl space-y-3">
        <Notice tone="info">尚未导入招标文件。导入原招标文件后，再导入更正、澄清或答疑公告（后发布的文件优先，例如顺延的投标截止时间）。</Notice>
        <UploadCard taskID={taskId} onUploaded={onUploaded} />
        <ProcessingPanel key={taskId} documents={list} />
      </div>
    )
  }
  return (
    <div className="grid min-w-0 gap-4 lg:grid-cols-[288px_minmax(0,1fr)]">
      <Card title={<>招标包文件 <span className="font-normal text-muted">{list.length}</span></>} bodyClassName="p-2"
        actions={<>
          <IconButton label="刷新文件列表" onClick={() => void docs.reload()}><RefreshCw className="size-4" /></IconButton>
          <Button size="sm" variant="primary" icon={<Upload className="size-3.5" />} onClick={() => setImporting(true)}>导入文件</Button>
        </>}>
        <ErrorBanner error={docs.error} onRetry={docs.reload} />
        <ul className="space-y-1">
          {list.map((d) => {
            const on = d.id === selected?.id
            const pages = d.page_count ?? d.chunks.reduce((m, c) => c.page_is_physical === false ? m : Math.max(m, c.page_end || c.page_no), 0)
            return (
              <li key={d.id}>
                <button type="button" aria-current={on || undefined} onClick={() => setQuery({ doc: d.id, chunk: undefined }, { replace: false })}
                  className={cx('w-full rounded-lg p-2 text-left hover:bg-soft', on && 'bg-soft')}>
                  <div className="flex items-center gap-2">
                    <span className="min-w-0 flex-1 truncate text-sm font-medium" title={d.title}>{d.title}</span>
                    <Badge tone={d.doc_role === 'tender' ? 'slate' : 'purple'}>{docRoleLabel[d.doc_role] ?? d.doc_role}</Badge>
                  </div>
                  <div className="mt-0.5 truncate text-xs text-muted" title={d.file_name}>
                    {d.published_at ? `${d.published_at.slice(0, 10)} 发布 · ` : ''}招标编号 {d.tender_no}{d.lot_no ? ` · 标段 ${d.lot_no}` : ''}
                  </div>
                  <div className="text-xs text-muted">{pages > 0 ? `${pages} 页` : '表格文档'} · {fmtNum(d.chunk_count)} 个章节/条款</div>
                </button>
              </li>
            )
          })}
        </ul>
      </Card>
      <div className="min-w-0 space-y-3">
        {docParam !== undefined && !selected && <Notice tone="danger">本项目中没有文件 #{String(params.get('doc'))}，已显示文件列表；请从左侧选择。</Notice>}
        {selected && chunkParam !== undefined && (Number.isNaN(chunkParam) || !selected.chunks.some((c) => c.id === chunkParam)) &&
          <Notice tone="warn">该文件中没有解析片段 #{String(params.get('chunk'))}：原文可能已重新解析或属于其他文件。</Notice>}
        {selected && <DocumentTree key={selected.id} doc={selected} focusChunk={chunkParam && !Number.isNaN(chunkParam) ? chunkParam : undefined} />}
        <ProcessingPanel key={taskId} documents={list} selected={selected} />
      </div>
      <Modal open={importing} wide title="导入招标文件" dirty={() => importDirty.current} onClose={() => { setImporting(false); importDirty.current = false }}>
        <UploadCard bare taskID={taskId} dirtyRef={importDirty} onUploaded={onUploaded} />
      </Modal>
    </div>
  )
}
