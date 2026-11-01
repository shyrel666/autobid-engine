import { useEffect, useState } from 'react'
import { Pencil, Plus, RefreshCw, Search, Trash2 } from 'lucide-react'
import { ApiError, api, errorText } from '../api/client'
import type { CompanyCredential, CredentialReq } from '../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Modal, Select, Spinner, useConfirm, useToast } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { PageFrame } from '../components/layout/AppShell'
import { credentialCategoryLabel, fmtTime } from '../lib/format'

interface AttrRow { key: string; value: string }

function toRows(attrs: Record<string, unknown> | null | undefined): AttrRow[] {
  return Object.entries(attrs ?? {}).map(([key, v]) => ({ key, value: typeof v === 'string' ? v : JSON.stringify(v) }))
}

// 属性值：能解析为 JSON 数字/布尔/对象/数组时按 JSON 保存，否则按字符串保存。
function parseValue(v: string): unknown {
  const t = v.trim()
  if (/^(-?\d+(\.\d+)?|true|false|null)$/.test(t) || /^[[{]/.test(t)) {
    try { return JSON.parse(t) } catch { /* 按字符串 */ }
  }
  return v
}

function today() {
  const d = new Date()
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

function CredentialDialog({ editing, onClose, onSaved }: { editing: CompanyCredential | 'new' | null; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const [form, setForm] = useState({ category: 'qualification', code: '', title: '', valid_until: '' })
  const [attrs, setAttrs] = useState<AttrRow[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [initial, setInitial] = useState('')

  useEffect(() => {
    if (!editing) return
    setError('')
    const f = editing === 'new'
      ? { category: 'qualification', code: '', title: '', valid_until: '' }
      : { category: editing.category, code: editing.code ?? '', title: editing.title, valid_until: editing.valid_until ?? '' }
    const a = editing === 'new' ? [{ key: '', value: '' }] : toRows(editing.attributes)
    setForm(f)
    setAttrs(a)
    setInitial(JSON.stringify([f, a]))
  }, [editing])
  const dirty = editing !== null && JSON.stringify([form, attrs]) !== initial

  const submit = async () => {
    setError('')
    if (!form.category.trim() || !form.title.trim()) { setError('类别与名称为必填项'); return }
    const attributes: Record<string, unknown> = {}
    for (const r of attrs) {
      const k = r.key.trim()
      if (!k) continue
      if (k in attributes) { setError(`属性键重复：${k}`); return }
      attributes[k] = parseValue(r.value)
    }
    const req: CredentialReq = { category: form.category.trim(), code: form.code.trim(), title: form.title.trim(), attributes, valid_until: form.valid_until || null }
    setBusy(true)
    try {
      if (editing === 'new') {
        const [saved] = await api.createCredential(req)
        toast('ok', saved.created ? `已新增 #${saved.id}` : `已按 (类别, 编号) 合并更新 #${saved.id}`)
      } else if (editing) {
        await api.updateCredential(editing.id, req)
        toast('ok', `已更新 #${editing.id}`)
      }
      onSaved()
    } catch (e) {
      setError(e instanceof ApiError && e.status === 409
        ? `保存失败：类别「${req.category}」下已存在编号「${req.code}」的其他记录` : errorText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={editing !== null} wide title={editing === 'new' ? '新增企业资信' : `编辑资信 #${editing?.id ?? ''}`} onClose={onClose} dirty={dirty}
      footer={<><Button onClick={onClose}>取消</Button><Button variant="primary" loading={busy} onClick={submit}>保存</Button></>}>
      <div className="grid gap-3 md:grid-cols-2">
        <Field label="类别 *" hint="约定词表：qualification / performance / personnel / financial / certificate，也可自定义">
          <Input list="cred-categories" value={form.category} onChange={(e) => setForm({ ...form, category: e.target.value })} />
          <datalist id="cred-categories">
            {Object.entries(credentialCategoryLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
          </datalist>
        </Field>
        <Field label="编号 code" hint={editing === 'new' ? '非空时按 (类别, 编号) 幂等合并；人员等记录可留空' : '按 ID 全量更新，可修改或清空编号'}>
          <Input value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })} placeholder="证书编号 / 合同编号" />
        </Field>
        <Field label="名称 *" className="md:col-span-2"><Input value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} placeholder="信息系统集成及服务资质（一级）" /></Field>
        <Field label="有效期至" hint="留空表示长期有效">
          <div className="flex gap-2">
            <Input type="date" value={form.valid_until} onChange={(e) => setForm({ ...form, valid_until: e.target.value })} />
            {form.valid_until && <Button variant="ghost" onClick={() => setForm({ ...form, valid_until: '' })}>清除</Button>}
          </div>
        </Field>
      </div>
      <div className="mt-4">
        <div className="mb-1 flex items-center justify-between">
          <span className="text-sm font-medium text-slate-700">属性（attributes）</span>
          <Button size="sm" icon={<Plus className="size-3.5" />} onClick={() => setAttrs([...attrs, { key: '', value: '' }])}>添加属性</Button>
        </div>
        <p className="mb-2 text-xs text-slate-400">
          {editing === 'new' ? '数字、true/false 与 JSON 对象/数组按 JSON 保存，其余按文本保存。' : '保存时以下属性整体替换原有属性：删除的键会从记录中移除。数字、true/false 与 JSON 对象按 JSON 保存。'}
        </p>
        <div className="space-y-2">
          {attrs.length === 0 && <p className="text-xs text-slate-400">无属性</p>}
          {attrs.map((r, i) => (
            <div key={i} className="flex gap-2">
              <Input className="max-w-48" placeholder="键，如 level" value={r.key} onChange={(e) => setAttrs(attrs.map((x, j) => j === i ? { ...x, key: e.target.value } : x))} />
              <Input placeholder="值，如 一级 / 1200 / true" value={r.value} onChange={(e) => setAttrs(attrs.map((x, j) => j === i ? { ...x, value: e.target.value } : x))} />
              <Button variant="ghost" onClick={() => setAttrs(attrs.filter((_, j) => j !== i))} title="删除属性"><Trash2 className="size-4 text-red-500" /></Button>
            </div>
          ))}
        </div>
      </div>
      <div className="mt-3"><ErrorBanner error={error} /></div>
    </Modal>
  )
}

export default function CredentialsPage() {
  const toast = useToast()
  const confirm = useConfirm()
  const [filter, setFilter] = useState({ category: '', q: '', valid_on: '' })
  const [applied, setApplied] = useState(filter)
  const creds = useAsync(() => api.listCredentials({ ...applied, limit: 1000 }), [applied])
  const [editing, setEditing] = useState<CompanyCredential | 'new' | null>(null)

  const remove = async (c: CompanyCredential) => {
    if (!(await confirm({ title: `删除资信 #${c.id}「${c.title}」？`, message: '此操作不可恢复。', confirmLabel: '删除', danger: true }))) return
    try {
      await api.deleteCredential(c.id)
      toast('ok', `已删除 #${c.id}`)
      void creds.reload()
    } catch (e) {
      toast('error', errorText(e))
      void creds.reload()
    }
  }

  const list = creds.data ?? []
  const now = today()
  return (
    <PageFrame title="企业资信库" description="企业资质、类似业绩与人员证书档案；写作与核验时用于比对招标资格要求。"
      actions={<Button variant="primary" icon={<Plus className="size-4" />} onClick={() => setEditing('new')}>新增资信</Button>}>
    <div className="space-y-4">
      <Card>
        <form className="flex flex-wrap items-end gap-3" onSubmit={(e) => { e.preventDefault(); setApplied(filter) }}>
          <Field label="类别" className="w-40">
            <Select value={filter.category} onChange={(e) => { const f = { ...filter, category: e.target.value }; setFilter(f); setApplied(f) }}>
              <option value="">全部</option>
              {Object.entries(credentialCategoryLabel).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
            </Select>
          </Field>
          <Field label="关键词（空格分隔，任一命中）" className="min-w-60 flex-1">
            <Input value={filter.q} onChange={(e) => setFilter({ ...filter, q: e.target.value })} placeholder="编号 / 名称 / 属性文本" />
          </Field>
          <Field label="在该日期仍有效" className="w-44">
            <Input type="date" value={filter.valid_on} onChange={(e) => setFilter({ ...filter, valid_on: e.target.value })} />
          </Field>
          <Button type="button" onClick={() => { const f = { ...filter, valid_on: filter.valid_on ? '' : now }; setFilter(f); setApplied(f) }}>
            {filter.valid_on ? '不限有效期' : '仅今日有效'}
          </Button>
          <Button type="submit" variant="primary" icon={<Search className="size-4" />}>查询</Button>
          <Button type="button" variant="ghost" icon={<RefreshCw className="size-4" />} onClick={() => creds.reload()} />
        </form>
      </Card>
      <ErrorBanner error={creds.error} onRetry={creds.reload} />
      <Card bodyClassName="p-0">
        {creds.loading && !creds.data ? <Spinner /> : list.length === 0 ? <Empty>暂无记录</Empty> : (
          <table className="w-full text-sm">
            <thead className="bg-slate-50 text-left text-xs text-slate-500">
              <tr><th className="px-3 py-2">#</th><th className="px-3 py-2">类别</th><th className="px-3 py-2">编号</th><th className="px-3 py-2">名称</th><th className="px-3 py-2">属性</th><th className="px-3 py-2">有效期</th><th className="px-3 py-2">更新</th><th className="px-3 py-2" /></tr>
            </thead>
            <tbody>
              {list.map((c) => {
                const expired = !!c.valid_until && c.valid_until < now
                return (
                  <tr key={c.id} className="border-t border-slate-100 align-top hover:bg-slate-50">
                    <td className="px-3 py-2 text-xs text-slate-400">{c.id}</td>
                    <td className="px-3 py-2"><Badge tone="blue">{credentialCategoryLabel[c.category] ?? c.category}</Badge></td>
                    <td className="px-3 py-2 font-mono text-xs">{c.code || <span className="text-slate-300">—</span>}</td>
                    <td className="px-3 py-2 font-medium">{c.title}</td>
                    <td className="px-3 py-2">
                      <div className="flex max-w-sm flex-wrap gap-1">
                        {Object.entries(c.attributes ?? {}).map(([k, v]) => <Badge key={k}>{k}: {typeof v === 'string' ? v : JSON.stringify(v)}</Badge>)}
                      </div>
                    </td>
                    <td className="px-3 py-2 text-xs">{c.valid_until ? <span className={expired ? 'text-red-600' : 'text-slate-600'}>{c.valid_until}{expired && '（已过期）'}</span> : <span className="text-slate-400">长期</span>}</td>
                    <td className="px-3 py-2 text-xs text-slate-400">{fmtTime(c.updated_at)}</td>
                    <td className="px-3 py-2">
                      <div className="flex justify-end gap-1">
                        <Button size="sm" variant="ghost" onClick={() => setEditing(c)} title="编辑"><Pencil className="size-3.5" /></Button>
                        <Button size="sm" variant="ghost" onClick={() => remove(c)} title="删除"><Trash2 className="size-3.5 text-red-500" /></Button>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </Card>
      <CredentialDialog editing={editing} onClose={() => setEditing(null)} onSaved={() => { setEditing(null); void creds.reload() }} />
    </div>
    </PageFrame>
  )
}
