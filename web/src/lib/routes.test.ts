import { test } from 'node:test'
import assert from 'node:assert/strict'
import { listQueryToQuery, optionalId, parseListQuery, parseRoute, parseWork, taskUrl, withQuery } from './routes.ts'

test('旧地址兼容与规范地址', () => {
  assert.deepEqual(parseRoute('/'), { name: 'redirect', to: '/tasks' })
  assert.deepEqual(parseRoute('/tasks'), { name: 'tasks' })
  assert.deepEqual(parseRoute('/tasks/12'), { name: 'redirect', to: '/tasks/12/documents' })
  assert.deepEqual(parseRoute('/tasks/12/proposal'), { name: 'redirect', to: '/tasks/12/requirements' })
  assert.deepEqual(parseRoute('/roadmap/phase4'), { name: 'redirect', to: '/knowledge' })
  assert.deepEqual(parseRoute('/roadmap/phase5'), { name: 'redirect', to: '/proposal' })
  assert.deepEqual(parseRoute('/roadmap/phase6'), { name: 'redirect', to: '/proposal?intent=export' })
  assert.deepEqual(parseRoute('/proposal', '?intent=export'), { name: 'proposal', intent: 'export' })
  assert.deepEqual(parseRoute('/proposal', '?intent=x'), { name: 'proposal', intent: 'outline' })
  assert.deepEqual(parseRoute('/tasks/3/overview'), { name: 'task', taskId: 3, page: 'overview' })
  assert.deepEqual(parseRoute('/tasks/3/writing/9'), { name: 'task', taskId: 3, page: 'writing', sectionId: 9 })
  for (const p of ['/tasks/3/monitor', '/tasks/3/elements', '/tasks/3/approvals', '/tasks/3/llm', '/tasks/3/report', '/tasks/3/research', '/tasks/3/documents'])
    assert.equal(parseRoute(p).name, 'task', p)
})

test('非法任务、章节与未知路径显示不存在页面，不回退到其他对象', () => {
  for (const p of ['/tasks/0', '/tasks/-1', '/tasks/abc', '/tasks/01', '/tasks/1.5', '/tasks/3/nope', '/tasks/3/writing/0',
    '/tasks/3/writing/x', '/tasks/3/overview/extra', '/foo', '/roadmap/phase7', '/llm/x'])
    assert.equal(parseRoute(p).name, 'notfound', p)
})

test('URL 构造稳定且丢弃空值', () => {
  assert.equal(taskUrl(5, 'writing', { sectionId: 7, query: { panel: 'evidence', evidence: undefined } }), '/tasks/5/writing/7?panel=evidence')
  assert.equal(withQuery('/x', { b: 2, a: 1, c: '' }), '/x?a=1&b=2')
  assert.equal(taskUrl(5, 'requirements', { query: { requirement: 3, filter: 'veto' } }), '/tasks/5/requirements?filter=veto&requirement=3')
})

test('列表查询参数白名单', () => {
  assert.deepEqual(parseListQuery('?q=abc&status=bogus&sort=zzz&page=-2&pageSize=7'), { q: 'abc', status: '', sort: 'created_desc', page: 1, pageSize: 20 })
  assert.deepEqual(parseListQuery('?status=running&sort=name_asc&page=3&pageSize=50'), { q: '', status: 'running', sort: 'name_asc', page: 3, pageSize: 50 })
  assert.deepEqual(listQueryToQuery({ q: '', status: '', sort: 'created_desc', page: 1, pageSize: 20 }),
    { q: undefined, status: undefined, sort: undefined, page: undefined, pageSize: undefined })
})

test('work 与可选 ID 参数', () => {
  assert.equal(parseWork(null), 'all')
  assert.equal(parseWork('all'), 'all')
  assert.equal(parseWork('201'), 201)
  assert.equal(parseWork('worker-a'), undefined)
  assert.equal(optionalId(new URLSearchParams('a=4'), 'a'), 4)
  assert.equal(optionalId(new URLSearchParams(''), 'a'), undefined)
  assert.ok(Number.isNaN(optionalId(new URLSearchParams('a=x'), 'a')))
})
