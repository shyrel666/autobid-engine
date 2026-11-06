import { useEffect, useRef, useState } from 'react'
import { api } from '../../api/client'
import type { KnowledgeIndex, KnowledgeIndexView } from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Notice, Spinner, useConfirm, useToast } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import { fmtTime } from '../../lib/format'
import type { RetrievalStatusState } from './SemanticStatus'
import { buildProgress, indexBuildFailureText, indexBuildSignature, indexRequestKey, indexStateLabel, indexStateText, indexStateTone, retrievalErrorText } from './retrieval-status'
import type { IndexRequestKeyState } from './retrieval-status'

function BuildProgress({ index }: { index: KnowledgeIndex }) {
  const progress = buildProgress(index)
  return <div className="space-y-1.5">
    <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted"><span>{progress.label}</span><span>{progress.percent}%</span></div>
    <progress aria-label={`索引 #${index.id} 构建进度`} className="block h-2 w-full accent-primary" max={100} value={progress.percent} />
  </div>
}

/** 状态与权限取自 retrieval-status；提交成功只表示作业已登记，启用需人工确认。 */
export function IndexManager({ retrieval }: { retrieval: RetrievalStatusState }) {
  const confirm = useConfirm(), toast = useToast()
  const [dimension, setDimension] = useState('')
  const [busy, setBusy] = useState('')
  const inFlight = useRef(false)
  const requestKey = useRef<IndexRequestKeyState | undefined>(undefined)
  const [unconfirmed, setUnconfirmed] = useState(false)
  const [validation, setValidation] = useState('')
  const [actionError, setActionError] = useState<unknown>()
  const [diagnosticsOpen, setDiagnosticsOpen] = useState(false)
  const events = useAsync(() => api.knowledgeIndexEvents(), [retrieval.loadedAt], diagnosticsOpen)
  const status = retrieval.data
  const manualDimension = !!status?.embedding.configured && !status.embedding.info?.dimension
  const signature = status ? indexBuildSignature(status, manualDimension ? Number(dimension) : undefined) : ''
  const retrying = unconfirmed && requestKey.current?.signature === signature
  const disabled = !!busy || !status || !!retrieval.error || retrieval.loading

  // 响应丢失后，状态列表也可确认同一个请求已经登记，之后可开始新的构建。
  useEffect(() => {
    if (status?.indexes?.some((index) => index.request_key === requestKey.current?.key)) {
      requestKey.current = undefined
      setUnconfirmed(false)
    }
  }, [status])

  const mutate = async (operation: string, action: () => Promise<string>) => {
    if (inFlight.current) return
    inFlight.current = true
    setBusy(operation); setValidation(''); setActionError(undefined)
    try { toast('ok', await action()) }
    catch (error) { setActionError(error) }
    finally {
      await retrieval.reload()
      inFlight.current = false
      setBusy('')
    }
  }

  const build = async () => {
    if (inFlight.current || !status || disabled || status.active_build || !status.embedding.configured || status.chunks === 0) return
    const value = manualDimension ? Number(dimension) : undefined
    if (value !== undefined && (!Number.isInteger(value) || value < 1 || value > 8192)) {
      setValidation('请由管理员确认索引维度，填写 1 至 8192 的整数。')
      return
    }
    requestKey.current = indexRequestKey(requestKey.current, signature)
    const request = { request_key: requestKey.current.key, ...(value === undefined ? {} : { dimension: value }) }
    setUnconfirmed(true)
    await mutate('build', async () => {
      const result = await api.buildKnowledgeIndex(request)
      requestKey.current = undefined
      setUnconfirmed(false)
      return result.replayed ? `已找到此前提交的构建记录 #${result.index.id}，请查看服务器状态。`
        : `构建请求 #${result.index.id} 已提交，完成后请确认启用。`
    })
  }

  const activate = async (index: KnowledgeIndexView) => {
    const rollback = !!status?.active_index && index.id < status.active_index.id
    if (!(await confirm({ title: `${rollback ? '回滚到' : '启用'}语义索引 #${index.id}？`,
      message: '确认后，后续检索将使用此索引。服务器会再次核对当前文档与检索配置是否一致。', confirmLabel: rollback ? '确认回滚' : '确认启用' }))) return
    await mutate(`activate:${index.id}`, async () => { await api.activateKnowledgeIndex(index.id); return `语义索引 #${index.id} 已${rollback ? '回滚启用' : '启用'}。` })
  }

  const remove = async (index: KnowledgeIndexView) => {
    if (!(await confirm({ title: `删除语义索引 #${index.id}？`, message: '删除后不能再切换到此索引；知识库原文仍可检索。', confirmLabel: '删除索引', danger: true }))) return
    await mutate(`delete:${index.id}`, async () => { await api.deleteKnowledgeIndex(index.id); return `语义索引 #${index.id} 已删除。` })
  }

  return <Card title="语义索引管理" actions={<Button size="sm" loading={retrieval.loading} onClick={() => void retrieval.reload()}>刷新</Button>}>
    <p className="mb-3 text-sm text-muted">构建在后台执行，关闭页面后仍会继续。完成后需确认启用；文档更新后重建会复用未变化的内容。</p>
    <div className="flex flex-wrap items-end gap-2">
      {manualDimension && <Field label="索引维度（请由管理员填写）" hint="服务端未声明此参数，确认后方可构建。">
        <Input type="number" min={1} max={8192} step={1} value={dimension} disabled={!!busy || !!status?.active_build}
          onChange={(event) => setDimension(event.target.value)} />
      </Field>}
      <Button variant="primary" loading={busy === 'build'} disabled={disabled || !!status?.active_build || !status?.embedding.configured || status.chunks === 0}
        onClick={() => void build()}>{retrying ? '重试本次提交' : '构建语义索引'}</Button>
      {retrying && <Button disabled={disabled} onClick={() => { requestKey.current = undefined; setUnconfirmed(false); setActionError(undefined) }}>开始新的构建请求</Button>}
    </div>
    {status && !status.embedding.configured && <p className="mt-2 text-sm text-muted">请先联系管理员配置语义检索服务。</p>}
    {status?.chunks === 0 && <p className="mt-2 text-sm text-muted">请先到文档库上传资料。</p>}
    {retrieval.error && <div className="mt-3"><Notice tone="warn">当前状态读取失败，操作已暂时停用。请刷新状态后再试。</Notice></div>}
    <div className="mt-3"><ErrorBanner error={validation || (actionError ? retrievalErrorText(actionError) : '')} /></div>
    {retrying && <p className="mt-2 text-xs text-muted">本次提交尚未确认。重试会复用原请求；也可先刷新查看是否已登记。</p>}
    {status?.active_build && <div className="my-4 space-y-3 rounded-[10px] border border-line bg-soft p-3">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="flex-1 font-medium">索引 #{status.active_build.id}</span>
        <Badge tone={indexStateTone(status.active_build.status)}>{indexStateLabel(status.active_build.status)}</Badge>
        <Button size="sm" disabled={disabled} loading={busy === `cancel:${status.active_build.id}`} onClick={() => {
          const index = status.active_build!
          void mutate(`cancel:${index.id}`, async () => { await api.cancelKnowledgeIndex(index.id); return `索引 #${index.id} 的构建已取消。` })
        }}>取消构建</Button>
      </div>
      <BuildProgress index={status.active_build} />
      {status.active_build.status === 'queued' && status.active_build.retry_after && <p className="text-xs text-muted">服务暂不可用，系统将自动重试；下次尝试时间：{fmtTime(status.active_build.retry_after)}。</p>}
      <p className="text-xs text-muted">处理完成后仍需确认启用，才能用于语义检索。</p>
    </div>}
    {retrieval.loading && !status ? <Spinner /> : status && <div className="mt-4 space-y-3">
      {!status.indexes?.length && <Empty>暂无语义索引，请构建并在完成后启用。</Empty>}
      {status.indexes?.map((index) => <div key={index.id} className="space-y-2 rounded-[10px] border border-line p-3">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="font-medium">索引 #{index.id}</span>
          <Badge tone={indexStateTone(index.state)}>{indexStateText(index, status.active_index?.id)}</Badge>
          <div className="ml-auto flex flex-wrap gap-2">
            {index.can_activate && <Button size="sm" disabled={disabled} loading={busy === `activate:${index.id}`} onClick={() => void activate(index)}>
              {status.active_index && index.id < status.active_index.id ? '回滚到此索引' : '启用'}</Button>}
            {index.can_delete && <Button size="sm" variant="ghost" className="text-danger" disabled={disabled} loading={busy === `delete:${index.id}`} onClick={() => void remove(index)}>删除</Button>}
          </div>
        </div>
        {(index.status === 'queued' || index.status === 'building') && index.id !== status.active_build?.id && <BuildProgress index={index} />}
        {index.status === 'failed' && <p className="text-sm text-warn">{indexBuildFailureText(index.error_code)}</p>}
      </div>)}
    </div>}
    <details className="mt-4 text-xs text-muted" onToggle={(event) => setDiagnosticsOpen(event.currentTarget.open)}>
      <summary className="cursor-pointer">诊断详情</summary>
      {actionError !== undefined && <pre className="mt-2 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-soft p-3">{JSON.stringify({
        error: actionError instanceof Error ? actionError.message : String(actionError),
        ...(actionError && typeof actionError === 'object' ? actionError : {}), request: requestKey.current,
      }, null, 2)}</pre>}
      <p className="mt-3 font-medium">索引记录</p>
      <pre className="mt-2 max-h-80 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-soft p-3">{JSON.stringify(status?.indexes ?? [], null, 2)}</pre>
      <p className="mt-3 font-medium">事件记录</p>
      {events.loading && !events.data && <Spinner label="正在读取事件…" />}
      {events.error && <p className="mt-2">事件读取失败：{events.error}</p>}
      {events.data && <pre className="mt-2 max-h-80 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-soft p-3">{JSON.stringify(events.data, null, 2)}</pre>}
    </details>
  </Card>
}
