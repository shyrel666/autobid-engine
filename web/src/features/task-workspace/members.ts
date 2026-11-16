// 成员与质量策略页的纯逻辑（单测在 members.test.ts）。按钮按角色显示只是方便，服务端才是判定方。

import type { MemberEvent } from '../../api/types'

export const POLICY_LABELS: Record<string, string> = {
  'grounded/v1': '落证写作（故事板、粉队 / 红队与句子级核验强制生效）',
  'legacy/v1': '旧写作门禁（不启用故事板与写作核验）',
}

/** 投标经理或管理员可以管理成员。 */
export function canManageMembers(myRoles: readonly string[], isAdmin: boolean): boolean {
  return isAdmin || myRoles.includes('bid_manager')
}

/** 当前账号是否担任任一所需角色。 */
export function hasRole(myRoles: readonly string[] | undefined, ...want: string[]): boolean {
  return !!myRoles && want.some((r) => myRoles.includes(r))
}

/** 成员事件的一句话说明。 */
export function memberEventText(e: MemberEvent, labels: Record<string, string>): string {
  const names = (rs: string[]) => rs.map((r) => labels[r] ?? r).join('、') || '无'
  const who = e.created_by || '系统'
  switch (e.action) {
    case 'added': return `${who} 添加 ${e.login}：${names(e.roles_after)}`
    case 'removed': return `${who} 移除 ${e.login}（原角色：${names(e.roles_before)}）`
    default: return `${who} 把 ${e.login} 的角色从 ${names(e.roles_before)} 改为 ${names(e.roles_after)}`
  }
}
