import { test } from 'node:test'
import assert from 'node:assert/strict'
import { canManageMembers, hasRole, memberEventText } from './members.ts'

const labels = { bid_manager: '投标经理', tech_writer: '技术撰写人', reviewer: '审核专家' }

test('only bid managers and administrators manage members', () => {
  assert.equal(canManageMembers(['bid_manager'], false), true)
  assert.equal(canManageMembers(['tech_writer', 'reviewer'], false), false)
  assert.equal(canManageMembers([], true), true)
})

test('hasRole matches any of the wanted roles', () => {
  assert.equal(hasRole(['tech_writer'], 'reviewer', 'tech_writer'), true)
  assert.equal(hasRole(['commercial'], 'reviewer'), false)
  assert.equal(hasRole(undefined, 'reviewer'), false)
})

test('member events read as one sentence', () => {
  const base = { id: 1, task_id: 1, user_id: 2, login: 'li', created_by: 'wang', created_at: '' }
  assert.equal(memberEventText({ ...base, action: 'added', roles_before: [], roles_after: ['tech_writer', 'reviewer'] }, labels),
    'wang 添加 li：技术撰写人、审核专家')
  assert.equal(memberEventText({ ...base, action: 'roles_changed', roles_before: ['tech_writer'], roles_after: ['bid_manager'] }, labels),
    'wang 把 li 的角色从 技术撰写人 改为 投标经理')
  assert.equal(memberEventText({ ...base, action: 'removed', roles_before: ['reviewer'], roles_after: [] }, labels),
    'wang 移除 li（原角色：审核专家）')
})
