import { useState } from 'react'
import { ROLE_LABELS, api, errorText } from '../../api/client'
import type { ProjectMember } from '../../api/types'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Modal, Notice, Select, Spinner, TextArea, useConfirm, useToast } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import { fmtTime } from '../../lib/format'
import { useSession } from '../auth/session'
import { useTaskWorkspace } from './TaskWorkspaceProvider'
import { POLICY_LABELS, canManageMembers, memberEventText } from './members'

const ALL_ROLES = ['bid_manager', 'tech_writer', 'commercial', 'reviewer']

function RolePicker({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  return (
    <div className="flex flex-wrap gap-3" role="group" aria-label="项目角色">
      {ALL_ROLES.map((r) => (
        <label key={r} className="flex items-center gap-1.5 text-sm">
          <input type="checkbox" checked={value.includes(r)} aria-label={ROLE_LABELS[r]}
            onChange={(e) => onChange(e.target.checked ? [...value, r] : value.filter((x) => x !== r))} />
          {ROLE_LABELS[r]}
        </label>
      ))}
    </div>
  )
}

function MemberDialog({ taskId, editing, onClose, onSaved }: { taskId: number; editing: ProjectMember | 'new' | null; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const dir = useAsync(() => api.userDirectory(), [], editing === 'new')
  const [login, setLogin] = useState('')
  const [roles, setRoles] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [key, setKey] = useState<unknown>(null)
  if (editing !== key) {
    setKey(editing)
    setLogin(editing && editing !== 'new' ? editing.login : '')
    setRoles(editing && editing !== 'new' ? editing.roles : [])
    setErr('')
  }
  const save = async () => {
    setBusy(true)
    setErr('')
    try {
      await api.setMember(taskId, login.trim(), roles)
      toast('ok', '成员已保存')
      onSaved()
      onClose()
    } catch (e) {
      setErr(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal open={editing !== null} title={editing === 'new' ? '添加成员' : '修改角色'} onClose={onClose}
      footer={<><Button variant="ghost" onClick={onClose}>取消</Button>
        <Button variant="primary" loading={busy} disabled={!login.trim() || roles.length === 0} onClick={() => void save()}>保存</Button></>}>
      <div className="flex flex-col gap-3">
        {editing === 'new' ? (
          <Field label="账号">
            <Select value={login} onChange={(e) => setLogin(e.target.value)} aria-label="账号">
              <option value="">选择账号</option>
              {(dir.data ?? []).filter((d) => d.status === 'active').map((d) => <option key={d.login} value={d.login}>{d.display_name}（{d.login}）</option>)}
            </Select>
          </Field>
        ) : <p className="text-sm">{editing?.display_name}（{editing?.login}）</p>}
        <Field label="项目角色" hint="一个人可以兼多个角色；作者与批准人分离仍按每份稿件逐条判定，兼任审核专家也不能批准自己的稿子。">
          <RolePicker value={roles} onChange={setRoles} />
        </Field>
        {err && <Notice tone="danger">{err}</Notice>}
      </div>
    </Modal>
  )
}

function QualityPolicyCard({ taskId, manager }: { taskId: number; manager: boolean }) {
  const toast = useToast()
  const { invalidate } = useTaskWorkspace()
  const pv = useAsync(() => api.qualityPolicy(taskId), [taskId])
  const [target, setTarget] = useState('')
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const save = async () => {
    setBusy(true)
    setErr('')
    try {
      pv.setData(await api.setQualityPolicy(taskId, target, reason.trim()))
      toast('ok', '质量策略已修改')
      setTarget('')
      setReason('')
      invalidate(['task', 'export', 'sections'])
    } catch (e) {
      setErr(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  const cur = pv.data?.policy ?? ''
  return (
    <Card title="质量策略" subtitle="写作门禁只看这里的显式策略；修改须由投标经理填写原因并留痕">
      {pv.error && <ErrorBanner error={pv.error} onRetry={pv.reload} />}
      {!pv.data ? <Spinner /> : (
        <div className="flex flex-col gap-3">
          <p className="text-sm">当前策略：<Badge tone={cur === 'grounded/v1' ? 'green' : 'amber'}>{POLICY_LABELS[cur] ?? cur}</Badge> <span className="text-xs text-muted">{cur}</span></p>
          {manager && (
            <div className="flex flex-col gap-2 rounded-[10px] border border-line p-3">
              <Field label="改为">
                <Select value={target} onChange={(e) => setTarget(e.target.value)} aria-label="新的质量策略">
                  <option value="">选择策略</option>
                  {pv.data.policies.filter((p) => p !== cur).map((p) => <option key={p} value={p}>{POLICY_LABELS[p] ?? p}（{p}）</option>)}
                </Select>
              </Field>
              <Field label="原因"><TextArea rows={2} value={reason} onChange={(e) => setReason(e.target.value)} aria-label="修改原因" /></Field>
              {err && <Notice tone="danger">{err}</Notice>}
              <div><Button variant="primary" loading={busy} disabled={!target || !reason.trim()} onClick={() => void save()}>修改质量策略</Button></div>
            </div>
          )}
          <ul className="flex flex-col gap-1 text-xs text-muted">
            {pv.data.events.map((e) => (
              <li key={e.id}>{fmtTime(e.created_at)} · {e.created_by || '系统'}：{e.from_policy ? `${e.from_policy} → ` : ''}{e.to_policy}{e.reason ? `（${e.reason}）` : ''}</li>
            ))}
          </ul>
        </div>
      )}
    </Card>
  )
}

export default function MembersPage() {
  const { taskId, task } = useTaskWorkspace()
  const { user } = useSession()
  const confirm = useConfirm()
  const toast = useToast()
  const members = useAsync(() => api.members(taskId), [taskId])
  const events = useAsync(() => api.memberEvents(taskId), [taskId])
  const [editing, setEditing] = useState<ProjectMember | 'new' | null>(null)
  const myRoles = task.data?.my_roles ?? []
  const manage = canManageMembers(myRoles, user.is_admin)
  const reload = () => { void members.reload(); void events.reload() }
  const remove = async (m: ProjectMember) => {
    if (!(await confirm({ title: `移除成员 ${m.display_name}？`, message: '移除后该账号看不到本项目；已留下的记录不受影响。', confirmLabel: '移除', danger: true }))) return
    try {
      await api.removeMember(taskId, m.login)
      toast('ok', '已移除')
      reload()
    } catch (e) {
      toast('error', errorText(e))
    }
  }
  return (
    <div className="mx-auto flex max-w-5xl flex-col gap-4">
      <Card title="项目成员" subtitle="只有成员能看到本项目；按角色放行操作，判定以服务端为准"
        actions={manage && <Button variant="primary" onClick={() => setEditing('new')}>添加成员</Button>}>
        {members.error && <ErrorBanner error={members.error} onRetry={members.reload} />}
        {!members.data ? <Spinner /> : members.data.length === 0 ? <Empty>尚无成员</Empty> : (
          <table className="w-full text-sm">
            <thead><tr className="text-left text-xs text-muted"><th className="py-1.5">成员</th><th>角色</th><th>状态</th><th>最近修改</th><th /></tr></thead>
            <tbody>
              {members.data.map((m) => (
                <tr key={m.login} className="border-t border-line" data-testid={`member-${m.login}`}>
                  <td className="py-2">{m.display_name}<span className="ml-1 text-xs text-muted">{m.login}</span></td>
                  <td>{m.roles.map((r) => <Badge key={r} className="mr-1">{ROLE_LABELS[r] ?? r}</Badge>)}</td>
                  <td>{m.status === 'active' ? '可用' : <span className="text-danger">已停用</span>}</td>
                  <td className="text-xs text-muted">{fmtTime(m.updated_at)} · {m.updated_by}</td>
                  <td className="text-right">
                    {manage && <>
                      <Button size="sm" variant="ghost" onClick={() => setEditing(m)}>修改角色</Button>
                      <Button size="sm" variant="ghost" onClick={() => void remove(m)}>移除</Button>
                    </>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
      <QualityPolicyCard taskId={taskId} manager={myRoles.includes('bid_manager')} />
      <Card title="成员变更记录" subtitle="只追加，不能修改或删除">
        {!events.data ? <Spinner /> : events.data.length === 0 ? <Empty>暂无记录</Empty> : (
          <ul className="flex flex-col gap-1 text-xs text-muted">
            {events.data.map((e) => <li key={e.id}>{fmtTime(e.created_at)} · {memberEventText(e, ROLE_LABELS)}</li>)}
          </ul>
        )}
      </Card>
      <MemberDialog taskId={taskId} editing={editing} onClose={() => setEditing(null)} onSaved={reload} />
    </div>
  )
}
