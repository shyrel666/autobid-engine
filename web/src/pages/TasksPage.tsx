import { useEffect, useMemo, useState } from 'react'
import { ArrowRight, ChevronDown, ChevronRight, Ellipsis, Plus, RefreshCw, Search, Trash2 } from 'lucide-react'
import { api, errorText } from '../api/client'
import type { CreateTaskReq, LLMProfile, Task } from '../api/types'
import { PageFrame } from '../components/layout/AppShell'
import { Button, Empty, ErrorBanner, Field, IconButton, Input, Menu, Modal, Select, Spinner, TextArea, cx, useConfirm, useToast } from '../components/ui'
import { RunStatusBadge } from '../features/task-workspace/TaskHeader'
import { useAsync } from '../hooks/useAsync'
import { fmtTime, taskStatusLabel } from '../lib/format'
import { rememberProjects } from '../lib/projectCache'
import { linkProps, navigate, useSearchParams } from '../lib/router'
import { LIST_STATUSES, PAGE_SIZES, listQueryToQuery, parseListQuery, taskUrl } from '../lib/routes'
import type { ListQuery } from '../lib/routes'

// 投标项目列表（图 1 / V01）：搜索、状态筛选、排序与分页只处理已加载的任务集合（客户端上限 500 条），
// 后端尚无总数与分页协议，页面写「已加载 N 个项目」，不当作全部项目总数。

type RunAction = 'start' | 'pause' | 'resume' | 'revive'
const actionLabel: Record<RunAction, string> = { start: '启动推演', pause: '暂停运行', resume: '恢复运行', revive: '复活运行' }

function RowMenu({ task, onDone }: { task: Task; onDone: () => void }) {
  const toast = useToast()
  const confirm = useConfirm()
  const run = async (action: RunAction) => {
    try {
      await api.taskAction(task.id, action)
      toast('ok', `项目 #${task.id} 已${actionLabel[action].replace('运行', '').replace('推演', '')}`)
      onDone()
    } catch (e) { toast('error', errorText(e)) }
  }
  const remove = async () => {
    const ok = await confirm({
      title: `删除项目 #${task.id}「${task.name}」？`, danger: true, confirmLabel: '删除项目',
      message: '项目的推演图、招标文档、要求清单、大纲与章节修订、核验结果和导出记录将一并删除，且不可恢复。',
    })
    if (!ok) return
    try { await api.deleteTask(task.id); toast('ok', `已删除项目 #${task.id}`); onDone() } catch (e) { toast('error', errorText(e)) }
  }
  const terminal = ['done', 'failed', 'timeout'].includes(task.status)
  return (
    <Menu label={`项目 #${task.id} 更多操作`} trigger={<span className="grid size-8 place-items-center rounded-[10px] hover:bg-soft"><Ellipsis className="size-4" /></span>}
      items={[
        task.status === 'created' && { key: 'start', label: actionLabel.start, onSelect: () => void run('start') },
        task.status === 'running' && !task.paused && { key: 'pause', label: actionLabel.pause, onSelect: () => void run('pause') },
        (task.paused || task.status === 'paused') && { key: 'resume', label: actionLabel.resume, onSelect: () => void run('resume') },
        terminal && { key: 'revive', label: actionLabel.revive, onSelect: () => void run('revive') },
        { key: 'delete', label: <><Trash2 className="size-4" />删除项目…</>, danger: true, onSelect: () => void remove() },
      ]} />
  )
}

function CreateTaskDialog({ open, onClose, onCreated }: { open: boolean; onClose: () => void; onCreated: (id: number) => void }) {
  const profiles = useAsync(() => api.listProfiles(), [], open)
  const blank = { name: '', description: '', goal: '', goals: '', timeout: '0', start: false }
  const [form, setForm] = useState(blank)
  const [scopes, setScopes] = useState([{ tender_no: '', lot_no: '' }])
  const [chain, setChain] = useState<number[]>([])
  const [advanced, setAdvanced] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const toast = useToast()
  useEffect(() => {
    if (!open) return
    setForm(blank); setScopes([{ tender_no: '', lot_no: '' }]); setChain([]); setError(''); setAdvanced(false)
  }, [open]) // eslint-disable-line react-hooks/exhaustive-deps
  const dirty = !!(form.name || form.goal || form.description || form.goals || scopes.some((s) => s.tender_no || s.lot_no) || chain.length)

  const submit = async () => {
    setError('')
    if (!form.name.trim() || !form.goal.trim()) { setError('项目名称与总目标为必填项'); return }
    const req: CreateTaskReq = {
      name: form.name.trim(), description: form.description.trim(), goal: form.goal.trim(),
      goals: form.goals.split('\n').map((s) => s.trim()).filter(Boolean),
      timeout_seconds: Number(form.timeout) || 0,
      scopes: scopes.filter((s) => s.tender_no.trim()).map((s) => ({ tender_no: s.tender_no.trim(), lot_no: s.lot_no.trim() })),
      llm_profile_ids: chain, start: form.start,
    }
    setBusy(true)
    try {
      const t = await api.createTask(req)
      toast('ok', `已创建项目 #${t.id}${form.start ? '，推演已启动' : ''}`)
      onCreated(t.id)
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  const enabledProfiles: LLMProfile[] = (profiles.data ?? []).filter((p) => p.enabled)

  return (
    <Modal open={open} wide title="新建投标项目" onClose={onClose} dirty={dirty}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={submit}>创建项目</Button></>}>
      <div className="space-y-5">
        <section className="grid gap-4 md:grid-cols-2">
          <h3 className="text-sm font-semibold md:col-span-2">基本信息</h3>
          <Field label="项目名称 *"><Input autoFocus value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="如：市政设施运维服务项目" /></Field>
          <Field label="说明"><Input value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} placeholder="可选" /></Field>
          <Field label="总目标 *" className="md:col-span-2" hint="推演运行围绕的总目标，例如完成资格、符合性审查与评分项响应核验。">
            <Input value={form.goal} onChange={(e) => setForm({ ...form, goal: e.target.value })} placeholder="完成资格审查、符合性审查与评分项响应核验" />
          </Field>
        </section>
        <section className="rounded-[10px] border border-line">
          <button type="button" aria-expanded={advanced} onClick={() => setAdvanced((v) => !v)}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm font-semibold hover:bg-soft">
            {advanced ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}高级运行配置
            <span className="font-normal text-muted">分项目标、招标编号与标段、运行超时、模型配额链</span>
          </button>
          {advanced && (
            <div className="grid gap-4 border-t border-line p-3 md:grid-cols-2">
              <Field label="分项目标（每行一个，可选）" className="md:col-span-2">
                <TextArea rows={3} value={form.goals} onChange={(e) => setForm({ ...form, goals: e.target.value })} placeholder={'★ 条款无废标风险\n评分项逐条有响应策略'} />
              </Field>
              <div className="md:col-span-2">
                <div className="mb-1.5 text-sm">招标编号与标段</div>
                <div className="space-y-2">
                  {scopes.map((s, i) => (
                    <div key={i} className="flex gap-2">
                      <Input aria-label="招标编号" placeholder="招标编号" value={s.tender_no} onChange={(e) => setScopes(scopes.map((x, j) => j === i ? { ...x, tender_no: e.target.value } : x))} />
                      <Input aria-label="标段（可空）" placeholder="标段（可空）" className="max-w-48" value={s.lot_no} onChange={(e) => setScopes(scopes.map((x, j) => j === i ? { ...x, lot_no: e.target.value } : x))} />
                      <Button variant="ghost" aria-label="移除该范围" onClick={() => setScopes(scopes.filter((_, j) => j !== i))} disabled={scopes.length === 1}><Trash2 className="size-4" /></Button>
                    </div>
                  ))}
                  <Button size="sm" icon={<Plus className="size-3.5" />} onClick={() => setScopes([...scopes, { tender_no: '', lot_no: '' }])}>添加范围</Button>
                </div>
                <p className="mt-1 text-xs text-muted">导入招标文件时会自动把文件的招标编号补入项目范围（字段 tender_no / lot_no）。</p>
              </div>
              <Field label="运行超时（秒）" hint="推演运行的超时期限，0 表示不限时。它不是投标截止时间；投标截止在导入文件后于「要求与否决项」中确认。">
                <Input type="number" min={0} value={form.timeout} onChange={(e) => setForm({ ...form, timeout: e.target.value })} />
              </Field>
              <div className="md:col-span-2">
                <div className="mb-1.5 text-sm">模型配额链（按点选顺序；留空使用全局模型池）</div>
                {profiles.loading && !profiles.data ? <Spinner /> : profiles.error ? <ErrorBanner error={profiles.error} onRetry={profiles.reload} /> : enabledProfiles.length === 0 ? (
                  <p className="text-xs text-muted">暂无启用的模型配置，可在「模型配置」页添加。</p>
                ) : (
                  <div className="flex flex-wrap gap-2">
                    {enabledProfiles.map((p) => {
                      const idx = chain.indexOf(p.id)
                      return (
                        <button key={p.id} type="button" aria-pressed={idx >= 0} onClick={() => setChain(idx >= 0 ? chain.filter((x) => x !== p.id) : [...chain, p.id])}
                          className={cx('h-7 rounded-lg border px-2 text-xs', idx >= 0 ? 'border-fg bg-soft font-medium' : 'border-line hover:bg-soft')}>
                          {idx >= 0 && <b className="mr-1">{idx + 1}.</b>}{p.name} <span className="text-muted">（{p.model}）</span>
                        </button>
                      )
                    })}
                  </div>
                )}
              </div>
            </div>
          )}
        </section>
        <label className="flex items-start gap-2 text-sm">
          <input type="checkbox" className="mt-1" checked={form.start} onChange={(e) => setForm({ ...form, start: e.target.checked })} />
          <span>创建后立即启动推演<span className="block text-xs text-muted">会调用模型并产生费用。建议先导入招标文件、复核要求后再在「执行操作」中启动。</span></span>
        </label>
        <ErrorBanner error={error} />
      </div>
    </Modal>
  )
}

const sorters: Record<ListQuery['sort'], (a: Task, b: Task) => number> = {
  created_desc: (a, b) => b.created_at.localeCompare(a.created_at) || b.id - a.id,
  created_asc: (a, b) => a.created_at.localeCompare(b.created_at) || a.id - b.id,
  name_asc: (a, b) => a.name.localeCompare(b.name, 'zh-CN'),
  id_desc: (a, b) => b.id - a.id,
}

export default function TasksPage() {
  const [params, setQuery] = useSearchParams()
  const lq = parseListQuery(params.toString() ? `?${params.toString()}` : '')
  const tasks = useAsync(() => api.listTasks(), [])
  const [creating, setCreating] = useState(false)
  const [q, setQ] = useState(lq.q)
  useEffect(() => { if (tasks.data) rememberProjects(tasks.data) }, [tasks.data])
  // 输入搜索防抖后以 replace 写入地址，避免每个字符一条后退记录。
  useEffect(() => {
    if (q === lq.q) return
    const id = setTimeout(() => setQuery({ ...listQueryToQuery({ ...lq, q, page: 1 }) } as Record<string, string | number | undefined>), 250)
    return () => clearTimeout(id)
  }, [q]) // eslint-disable-line react-hooks/exhaustive-deps
  const update = (patch: Partial<ListQuery>) => setQuery(listQueryToQuery({ ...lq, ...patch }) as Record<string, string | number | undefined>)

  const all = tasks.data ?? []
  const filtered = useMemo(() => {
    const s = lq.q.trim().toLowerCase()
    return all.filter((t) => (!lq.status || t.status === lq.status) &&
      (!s || `${t.id} ${t.name} ${t.description ?? ''}`.toLowerCase().includes(s))).sort(sorters[lq.sort])
  }, [all, lq.q, lq.status, lq.sort])
  const pages = Math.max(1, Math.ceil(filtered.length / lq.pageSize))
  const page = Math.min(lq.page, pages)
  const rows = filtered.slice((page - 1) * lq.pageSize, page * lq.pageSize)
  const from = filtered.length ? (page - 1) * lq.pageSize + 1 : 0

  return (
    <PageFrame>
      <section data-region="project-table" className="overflow-hidden rounded-[14px] border border-line bg-panel">
        <div className="flex min-h-[88px] flex-wrap items-center gap-2 p-4 md:p-6">
          <div className="relative w-full sm:w-80">
            <Search className="pointer-events-none absolute top-2 left-2.5 size-4 text-muted" aria-hidden />
            <Input aria-label="搜索项目" className="pl-8" placeholder="搜索项目名称 / 说明 / 编号" value={q} onChange={(e) => setQ(e.target.value)} />
          </div>
          <Select aria-label="运行状态" className="w-36" value={lq.status} onChange={(e) => update({ status: e.target.value, page: 1 })}>
            <option value="">全部运行状态</option>
            {LIST_STATUSES.map((k) => <option key={k} value={k}>{taskStatusLabel[k]}</option>)}
          </Select>
          <Select aria-label="排序" className="w-36" value={lq.sort} onChange={(e) => update({ sort: e.target.value as ListQuery['sort'], page: 1 })}>
            <option value="created_desc">最新创建在前</option><option value="created_asc">最早创建在前</option>
            <option value="name_asc">按名称</option><option value="id_desc">按编号</option>
          </Select>
          <span className="text-xs text-muted">{tasks.data ? `已加载 ${all.length} 个项目${filtered.length !== all.length ? `，匹配 ${filtered.length} 个` : ''}` : ''}</span>
          <span className="flex-1" />
          <IconButton label="刷新项目列表" onClick={() => void tasks.reload()} disabled={tasks.loading}><RefreshCw className="size-4" /></IconButton>
          <Button variant="primary" icon={<Plus className="size-4" />} onClick={() => setCreating(true)}>新建项目</Button>
        </div>
        {tasks.error && <div className="px-6 pb-3"><ErrorBanner error={tasks.error} onRetry={tasks.reload} /></div>}
        <div className="overflow-x-auto">
          {tasks.loading && !tasks.data ? <Spinner /> : !tasks.data ? null : all.length === 0 ? <Empty>暂无投标项目。点击「新建项目」，再导入招标文件。</Empty>
            : filtered.length === 0 ? <Empty>已加载的项目中没有匹配项。</Empty> : (
            <table className="w-full min-w-[880px] table-fixed border-collapse text-sm">
              <colgroup><col className="w-[7%]" /><col className="w-[26%]" /><col className="w-[25%]" /><col className="w-[13%]" /><col className="w-[15%]" /><col className="w-[14%]" /></colgroup>
              <thead>
                <tr className="text-left">
                  {['编号', '项目名称', '说明', '运行状态', '创建时间', '操作'].map((h) => (
                    <th key={h} scope="col" className="h-10 border-y border-line px-4 font-medium">{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {rows.map((t) => (
                  <tr key={t.id} className="border-b border-line-soft last:border-b-0 hover:bg-[#fafafa]">
                    <td className="h-12 px-4"><span className="rounded bg-soft px-1.5 py-px text-xs text-muted tabular-nums">{t.id}</span></td>
                    <td className="truncate px-4 font-semibold" title={t.name}>
                      <a {...linkProps(taskUrl(t.id, 'overview'))} className="hover:underline">{t.name}</a>
                    </td>
                    <td className="truncate px-4 text-muted" title={t.description}>{t.description || '—'}</td>
                    <td className="px-4"><RunStatusBadge task={t} /></td>
                    <td className="px-4 text-xs text-muted tabular-nums">{fmtTime(t.created_at)}</td>
                    <td className="px-4">
                      <div className="flex items-center gap-2">
                        <a {...linkProps(taskUrl(t.id, 'overview'))}
                          className="inline-flex h-7 items-center gap-1.5 rounded-lg bg-primary px-2 text-[13px] font-medium text-[#fafafa] hover:bg-primary-hover">
                          <ArrowRight className="size-4" aria-hidden />进入</a>
                        <RowMenu task={t} onDone={() => void tasks.reload()} />
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
        {tasks.data && filtered.length > 0 && (
          <div className="flex min-h-[60px] flex-wrap items-center gap-2 border-t border-line px-4 py-4 md:px-6">
            <Select aria-label="每页条数" className="h-7 w-16 rounded-lg px-2 pr-5" value={lq.pageSize} onChange={(e) => update({ pageSize: Number(e.target.value), page: 1 })}>
              {PAGE_SIZES.map((n) => <option key={n} value={n}>{n}</option>)}
            </Select>
            <span className="text-xs text-muted">条/页 · {from}–{from + rows.length - 1} / 已加载 {filtered.length} 条</span>
            {pages > 1 && <>
              <span className="flex-1" />
              <Button size="sm" disabled={page <= 1} onClick={() => update({ page: page - 1 })}>上一页</Button>
              <span className="text-xs text-muted tabular-nums">{page} / {pages}</span>
              <Button size="sm" disabled={page >= pages} onClick={() => update({ page: page + 1 })}>下一页</Button>
            </>}
          </div>
        )}
      </section>
      <CreateTaskDialog open={creating} onClose={() => setCreating(false)}
        onCreated={(id) => { setCreating(false); void tasks.reload(); void navigate(taskUrl(id, 'documents')) }} />
    </PageFrame>
  )
}
