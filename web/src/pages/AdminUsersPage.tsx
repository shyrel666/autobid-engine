import { useState } from 'react'
import { api, errorText } from '../api/client'
import type { LegacyActor, UserAccount } from '../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Modal, Notice, Select, Spinner, useConfirm, useToast } from '../components/ui'
import { PageFrame } from '../components/layout/AppShell'
import { useAsync } from '../hooks/useAsync'
import { fmtTime } from '../lib/format'
import { useSession } from '../features/auth/session'

// 管理员页：账号（创建、停用 / 启用、管理员标记、重置口令）与升级前遗留姓名的映射。
// 服务端只对管理员放行这些接口；本页只是界面。

function CreateUserDialog({ open, onClose, onSaved }: { open: boolean; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const [f, setF] = useState({ login: '', display_name: '', password: '', is_admin: false })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const save = async () => {
    setBusy(true)
    setErr('')
    try {
      await api.createUser({ ...f, login: f.login.trim().toLowerCase(), display_name: f.display_name.trim() })
      toast('ok', '账号已创建')
      setF({ login: '', display_name: '', password: '', is_admin: false })
      onSaved()
      onClose()
    } catch (e) {
      setErr(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal open={open} title="新建账号" onClose={onClose}
      footer={<><Button variant="ghost" onClick={onClose}>取消</Button>
        <Button variant="primary" loading={busy} disabled={!f.login.trim() || !f.password} onClick={() => void save()}>创建</Button></>}>
      <div className="flex flex-col gap-3">
        <Field label="账号" hint="2~32 位小写字母、数字或 . _ -，创建后不能修改，审计记录写的就是它">
          <Input aria-label="账号" value={f.login} onChange={(e) => setF({ ...f, login: e.target.value })} autoComplete="off" />
        </Field>
        <Field label="显示名" hint="页面上显示的姓名，可以随时修改"><Input aria-label="显示名" value={f.display_name} onChange={(e) => setF({ ...f, display_name: e.target.value })} /></Field>
        <Field label="初始口令" hint="至少 10 个字符；请线下告知本人并让其登录后修改">
          <Input aria-label="初始口令" type="password" autoComplete="new-password" value={f.password} onChange={(e) => setF({ ...f, password: e.target.value })} />
        </Field>
        <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={f.is_admin} onChange={(e) => setF({ ...f, is_admin: e.target.checked })} />管理员（管理账号与系统设置；不因此获得任何项目的内容权限）</label>
        {err && <Notice tone="danger">{err}</Notice>}
      </div>
    </Modal>
  )
}

function ResetPasswordDialog({ user, onClose }: { user: UserAccount | null; onClose: () => void }) {
  const toast = useToast()
  const [pw, setPw] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const save = async () => {
    if (!user) return
    setBusy(true)
    setErr('')
    try {
      await api.resetUserPassword(user.id, pw)
      toast('ok', '口令已重置，该账号原有登录全部退出')
      setPw('')
      onClose()
    } catch (e) {
      setErr(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal open={!!user} title={`重置口令：${user?.display_name ?? ''}`} onClose={onClose}
      footer={<><Button variant="ghost" onClick={onClose}>取消</Button><Button variant="primary" loading={busy} disabled={!pw} onClick={() => void save()}>重置</Button></>}>
      <Field label="新口令"><Input aria-label="新口令" type="password" autoComplete="new-password" value={pw} onChange={(e) => setPw(e.target.value)} /></Field>
      {err && <Notice tone="danger" className="mt-3">{err}</Notice>}
    </Modal>
  )
}

function LegacyActors({ users }: { users: UserAccount[] }) {
  const toast = useToast()
  const list = useAsync(() => api.legacyActors(), [])
  const [pick, setPick] = useState<Record<string, string>>({})
  const map = async (a: LegacyActor) => {
    try {
      await api.mapLegacyActor(a.name, pick[a.name] ?? '')
      toast('ok', '已映射')
      void list.reload()
    } catch (e) { toast('error', errorText(e)) }
  }
  const unmap = async (a: LegacyActor) => {
    try {
      await api.unmapLegacyActor(a.name)
      toast('ok', '已撤销映射')
      void list.reload()
    } catch (e) { toast('error', errorText(e)) }
  }
  return (
    <Card title="升级前的声明式姓名" subtitle="升级前页面上随手填写的“当前操作人”。未映射的姓名不参与作者与批准人分离判定：相关故事板、修订、承诺须由作者重新保存，或在这里映射到确实是同一人的账号。">
      {list.error && <ErrorBanner error={list.error} onRetry={list.reload} />}
      {!list.data ? <Spinner /> : list.data.length === 0 ? <Empty>没有需要处理的旧记录</Empty> : (
        <table className="w-full text-sm">
          <thead><tr className="text-left text-xs text-muted"><th className="py-1.5">旧姓名</th><th>涉及记录</th><th>映射到账号</th><th /></tr></thead>
          <tbody>
            {list.data.map((a) => (
              <tr key={a.name} className="border-t border-line">
                <td className="py-2">{a.name}</td>
                <td className="text-xs text-muted">故事板 {a.storyboards} · 修订 {a.revisions} · 承诺 {a.commitments}</td>
                <td>{a.mapped_to ? <>{a.mapped_to} <span className="text-xs text-muted">（{a.mapped_by} 映射）</span></> : (
                  <Select aria-label={`映射 ${a.name}`} value={pick[a.name] ?? ''} onChange={(e) => setPick({ ...pick, [a.name]: e.target.value })}>
                    <option value="">选择账号</option>
                    {users.map((u) => <option key={u.login} value={u.login}>{u.display_name}（{u.login}）</option>)}
                  </Select>
                )}</td>
                <td className="text-right">{a.mapped_to
                  ? <Button size="sm" variant="ghost" onClick={() => void unmap(a)}>撤销</Button>
                  : <Button size="sm" disabled={!pick[a.name]} onClick={() => void map(a)}>映射</Button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Card>
  )
}

export default function AdminUsersPage() {
  const { user: me } = useSession()
  const toast = useToast()
  const confirm = useConfirm()
  const users = useAsync(() => api.listUsers(), [], me.is_admin)
  const events = useAsync(() => api.userEvents(), [], me.is_admin)
  const [creating, setCreating] = useState(false)
  const [resetting, setResetting] = useState<UserAccount | null>(null)
  const reload = () => { void users.reload(); void events.reload() }
  const patch = async (u: UserAccount, req: { is_admin?: boolean; status?: 'active' | 'disabled' }, what: string) => {
    if (req.status === 'disabled' && !(await confirm({ title: `停用 ${u.display_name}？`, message: '停用后该账号立即退出所有登录、不能再登录；已留下的记录不受影响。', confirmLabel: '停用', danger: true }))) return
    try {
      await api.updateUser(u.id, req)
      toast('ok', what)
      reload()
    } catch (e) { toast('error', errorText(e)) }
  }
  if (!me.is_admin) return <PageFrame title="账号管理"><Notice tone="warn">需要管理员账号。</Notice></PageFrame>
  return (
    <PageFrame title="账号管理" description="账号与口令只在本机管理；没有默认账号。忘记口令的管理员可在本机运行 ops.bat user reset-password。"
      actions={<Button variant="primary" onClick={() => setCreating(true)}>新建账号</Button>}>
      <div className="flex flex-col gap-4">
        <Card title="账号">
          {users.error && <ErrorBanner error={users.error} onRetry={users.reload} />}
          {!users.data ? <Spinner /> : (
            <table className="w-full text-sm">
              <thead><tr className="text-left text-xs text-muted"><th className="py-1.5">账号</th><th>显示名</th><th>状态</th><th>口令修改于</th><th /></tr></thead>
              <tbody>
                {users.data.map((u) => (
                  <tr key={u.id} className="border-t border-line" data-testid={`user-${u.login}`}>
                    <td className="py-2">{u.login}{u.is_admin && <Badge tone="purple" className="ml-1">管理员</Badge>}</td>
                    <td>{u.display_name}</td>
                    <td>{u.status === 'active' ? (u.locked_until && new Date(u.locked_until) > new Date() ? <span className="text-warn">暂时锁定</span> : '可用') : <span className="text-danger">已停用</span>}</td>
                    <td className="text-xs text-muted">{fmtTime(u.password_changed_at)}</td>
                    <td className="text-right whitespace-nowrap">
                      <Button size="sm" variant="ghost" onClick={() => setResetting(u)}>重置口令</Button>
                      <Button size="sm" variant="ghost" onClick={() => void patch(u, { is_admin: !u.is_admin }, u.is_admin ? '已取消管理员' : '已设为管理员')}>{u.is_admin ? '取消管理员' : '设为管理员'}</Button>
                      {u.status === 'active'
                        ? <Button size="sm" variant="ghost" disabled={u.id === me.id} onClick={() => void patch(u, { status: 'disabled' }, '已停用')}>停用</Button>
                        : <Button size="sm" variant="ghost" onClick={() => void patch(u, { status: 'active' }, '已启用')}>启用</Button>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>
        {users.data && <LegacyActors users={users.data.filter((u) => u.status === 'active')} />}
        <Card title="账号变更记录" subtitle="只追加；不记录口令或任何口令派生值">
          {!events.data ? <Spinner /> : events.data.length === 0 ? <Empty>暂无记录</Empty> : (
            <ul className="flex flex-col gap-1 text-xs text-muted">
              {events.data.map((e) => <li key={e.id}>{fmtTime(e.created_at)} · {e.created_by}：{e.action} {e.login}</li>)}
            </ul>
          )}
        </Card>
      </div>
      <CreateUserDialog open={creating} onClose={() => setCreating(false)} onSaved={reload} />
      <ResetPasswordDialog user={resetting} onClose={() => setResetting(null)} />
    </PageFrame>
  )
}
