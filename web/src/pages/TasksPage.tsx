import { useState } from 'react'
import { Pause, Play, Plus, RefreshCw, RotateCcw, Trash2 } from 'lucide-react'
import { api, errorText } from '../api/client'
import type { CreateTaskReq, LLMProfile, Task } from '../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Modal, Select, Spinner, TextArea, statusTone, useToast } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { fmtTime, taskStatusLabel } from '../lib/format'
import { navigate } from '../lib/router'

export function TaskActions({ task, onDone, size = 'sm' }: { task: Task; onDone: () => void; size?: 'sm' | 'md' }) {
  const toast = useToast()
  const [busy, setBusy] = useState('')
  const run = async (action: 'start' | 'pause' | 'resume' | 'revive') => {
    setBusy(action)
    try {
      await api.taskAction(task.id, action)
      toast('ok', `任务 #${task.id} 已${{ start: '启动', pause: '暂停', resume: '恢复', revive: '复活' }[action]}`)
      onDone()
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setBusy('')
    }
  }
  const terminal = ['done', 'failed', 'timeout'].includes(task.status)
  return (
    <div className="flex items-center gap-1.5">
      {task.status === 'created' && <Button size={size} variant="primary" icon={<Play className="size-3.5" />} loading={busy === 'start'} onClick={() => run('start')}>启动</Button>}
      {task.status === 'running' && !task.paused && <Button size={size} icon={<Pause className="size-3.5" />} loading={busy === 'pause'} onClick={() => run('pause')}>暂停</Button>}
      {(task.paused || task.status === 'paused') && <Button size={size} variant="primary" icon={<Play className="size-3.5" />} loading={busy === 'resume'} onClick={() => run('resume')}>恢复</Button>}
      {terminal && <Button size={size} icon={<RotateCcw className="size-3.5" />} loading={busy === 'revive'} onClick={() => run('revive')}>复活</Button>}
    </div>
  )
}

function CreateTaskDialog({ open, onClose, onCreated }: { open: boolean; onClose: () => void; onCreated: (id: number) => void }) {
  const toast = useToast()
  const profiles = useAsync(() => api.listProfiles(), [], open)
  const [form, setForm] = useState({ name: '', description: '', goal: '', goals: '', timeout: '0', start: false })
  const [scopes, setScopes] = useState([{ tender_no: '', lot_no: '' }])
  const [chain, setChain] = useState<number[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const submit = async () => {
    setError('')
    if (!form.name.trim() || !form.goal.trim()) { setError('任务名称与审查总目标为必填项'); return }
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
      toast('ok', `已创建任务 #${t.id}`)
      onCreated(t.id)
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  const enabledProfiles: LLMProfile[] = (profiles.data ?? []).filter((p) => p.enabled)

  return (
    <Modal open={open} wide title="新建审查任务" onClose={onClose}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={submit}>创建</Button></>}>
      <div className="grid gap-4 md:grid-cols-2">
        <Field label="任务名称 *"><Input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="政务云平台采购项目 资格与符合性审查" /></Field>
        <Field label="超时（秒）" hint="0 表示不限时"><Input type="number" min={0} value={form.timeout} onChange={(e) => setForm({ ...form, timeout: e.target.value })} /></Field>
        <Field label="审查总目标 *" className="md:col-span-2"><Input value={form.goal} onChange={(e) => setForm({ ...form, goal: e.target.value })} placeholder="完成资格审查、符合性审查与评分项响应核验" /></Field>
        <Field label="分项目标（每行一个，可选）" className="md:col-span-2">
          <TextArea rows={3} value={form.goals} onChange={(e) => setForm({ ...form, goals: e.target.value })} placeholder={'★ 条款无废标风险\n评分项逐条有响应策略'} />
        </Field>
        <Field label="说明" className="md:col-span-2"><Input value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} /></Field>
        <div className="md:col-span-2">
          <div className="mb-1 text-sm font-medium text-slate-700">招标范围（tender_no / lot_no）</div>
          <div className="space-y-2">
            {scopes.map((s, i) => (
              <div key={i} className="flex gap-2">
                <Input placeholder="招标编号 tender_no" value={s.tender_no} onChange={(e) => setScopes(scopes.map((x, j) => j === i ? { ...x, tender_no: e.target.value } : x))} />
                <Input placeholder="标段 lot_no（可空）" className="max-w-48" value={s.lot_no} onChange={(e) => setScopes(scopes.map((x, j) => j === i ? { ...x, lot_no: e.target.value } : x))} />
                <Button variant="ghost" onClick={() => setScopes(scopes.filter((_, j) => j !== i))} disabled={scopes.length === 1}><Trash2 className="size-4" /></Button>
              </div>
            ))}
            <Button size="sm" icon={<Plus className="size-3.5" />} onClick={() => setScopes([...scopes, { tender_no: '', lot_no: '' }])}>添加范围</Button>
          </div>
          <p className="mt-1 text-xs text-slate-400">上传招标文档时也会自动把文档的招标编号补入任务范围。</p>
        </div>
        <div className="md:col-span-2">
          <div className="mb-1 text-sm font-medium text-slate-700">LLM 配额链（按选择顺序，可空则使用全局池）</div>
          {profiles.loading ? <Spinner /> : profiles.error ? <ErrorBanner error={profiles.error} /> : enabledProfiles.length === 0 ? (
            <p className="text-xs text-slate-400">暂无启用的 LLM 配置，可在“LLM 配置”页添加。</p>
          ) : (
            <div className="flex flex-wrap gap-2">
              {enabledProfiles.map((p) => {
                const idx = chain.indexOf(p.id)
                return (
                  <button key={p.id} onClick={() => setChain(idx >= 0 ? chain.filter((x) => x !== p.id) : [...chain, p.id])}
                    className={`rounded-md border px-2 py-1 text-xs ${idx >= 0 ? 'border-blue-500 bg-blue-50 text-blue-700' : 'border-slate-300 text-slate-600'}`}>
                    {idx >= 0 && <b className="mr-1">{idx + 1}.</b>}{p.name} <span className="text-slate-400">({p.model})</span>
                  </button>
                )
              })}
            </div>
          )}
        </div>
        <label className="flex items-center gap-2 text-sm md:col-span-2">
          <input type="checkbox" checked={form.start} onChange={(e) => setForm({ ...form, start: e.target.checked })} />
          创建后立即启动推演（建议先上传招标文档再启动）
        </label>
      </div>
      <div className="mt-3"><ErrorBanner error={error} /></div>
    </Modal>
  )
}

export default function TasksPage() {
  const toast = useToast()
  const [status, setStatus] = useState('')
  const tasks = useAsync(() => api.listTasks(status), [status])
  const [creating, setCreating] = useState(false)

  const remove = async (t: Task) => {
    if (!confirm(`确认删除任务 #${t.id}「${t.name}」？推演图、文档与产出物将一并删除，且不可恢复。`)) return
    try {
      await api.deleteTask(t.id)
      toast('ok', `已删除任务 #${t.id}`)
      void tasks.reload()
    } catch (e) {
      toast('error', errorText(e))
    }
  }

  const list = tasks.data ?? []
  return (
    <div className="mx-auto max-w-6xl space-y-4 p-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">审查任务</h1>
          <p className="text-sm text-slate-500">每个任务对应一次招标文件审查推演：上传招标文档 → 启动推演 → 监控与审批 → 导出审查报告。</p>
        </div>
        <div className="flex items-center gap-2">
          <Select value={status} onChange={(e) => setStatus(e.target.value)} className="w-32">
            <option value="">全部状态</option>
            {Object.entries(taskStatusLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
          </Select>
          <Button icon={<RefreshCw className="size-4" />} onClick={() => tasks.reload()}>刷新</Button>
          <Button variant="primary" icon={<Plus className="size-4" />} onClick={() => setCreating(true)}>新建任务</Button>
        </div>
      </div>
      <ErrorBanner error={tasks.error} onRetry={tasks.reload} />
      <Card bodyClassName="p-0">
        {tasks.loading && !tasks.data ? <Spinner /> : list.length === 0 ? <Empty>暂无任务，点击右上角“新建任务”开始。</Empty> : (
          <table className="w-full text-sm">
            <thead className="bg-slate-50 text-left text-xs text-slate-500">
              <tr><th className="px-4 py-2">#</th><th className="px-4 py-2">任务</th><th className="px-4 py-2">状态</th><th className="px-4 py-2">创建时间</th><th className="px-4 py-2">截止</th><th className="px-4 py-2 text-right">操作</th></tr>
            </thead>
            <tbody>
              {list.map((t) => (
                <tr key={t.id} className="border-t border-slate-100 hover:bg-slate-50">
                  <td className="px-4 py-2.5 text-slate-400 tabular-nums">{t.id}</td>
                  <td className="px-4 py-2.5">
                    <a href={`/tasks/${t.id}`} onClick={(e) => { e.preventDefault(); navigate(`/tasks/${t.id}`) }} className="font-medium text-blue-700 hover:underline">{t.name}</a>
                    {t.description && <div className="max-w-md truncate text-xs text-slate-400">{t.description}</div>}
                  </td>
                  <td className="px-4 py-2.5">
                    <Badge tone={statusTone(t.status)}>{taskStatusLabel[t.status] ?? t.status}</Badge>
                    {t.paused && t.status !== 'paused' && <Badge tone="amber" className="ml-1">已暂停</Badge>}
                    {t.queued && <Badge className="ml-1">排队</Badge>}
                  </td>
                  <td className="px-4 py-2.5 text-xs text-slate-500">{fmtTime(t.created_at)}</td>
                  <td className="px-4 py-2.5 text-xs text-slate-500">{fmtTime(t.deadline_at)}</td>
                  <td className="px-4 py-2.5">
                    <div className="flex justify-end gap-1.5">
                      <TaskActions task={t} onDone={() => tasks.reload()} />
                      <Button size="sm" variant="ghost" onClick={() => remove(t)} title="删除任务"><Trash2 className="size-3.5 text-red-500" /></Button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
      <CreateTaskDialog open={creating} onClose={() => setCreating(false)} onCreated={(id) => { setCreating(false); navigate(`/tasks/${id}`) }} />
    </div>
  )
}
