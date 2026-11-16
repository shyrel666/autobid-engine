import { ROLE_LABELS } from '../../api/client'
import { useOptionalSession } from '../auth/session'
import { useTaskWorkspace } from './TaskWorkspaceProvider'

// R2：记录与评审人分离都按登录账号判定，页面不再有可改写的"当前操作人"。这里只显示当前账号与本项目角色。
export function CurrentAccount() {
  const session = useOptionalSession()
  const { task } = useTaskWorkspace()
  if (!session) return null
  const roles = (task.data?.my_roles ?? []).map((r) => ROLE_LABELS[r] ?? r)
  return (
    <span className="text-xs text-muted" data-testid="current-account-roles"
      title="写入审计记录、粉队 / 红队评审人分离都按登录账号判定">
      当前账号：{session.user.display_name}（{session.user.login}）{roles.length ? ` · ${roles.join('、')}` : ''}
    </span>
  )
}
