import { useMemo, useState } from 'react'
import { Check, Copy, Cpu, Layers, RefreshCw, Search, ShieldAlert, Wrench } from 'lucide-react'
import { api } from '../api/client'
import type { CatalogTool } from '../api/types'
import { PageFrame } from '../components/layout/AppShell'
import { Badge, Button, Card, Drawer, Empty, ErrorBanner, IconButton, Input, Pre, Select, Spinner } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import {
  formatParamDescription,
  formatSkillTitle,
  formatToolDescription,
  formatToolTitle,
  pretty,
  schemaTypeLabel,
} from '../lib/format'
import { linkProps } from '../lib/router'

interface ParamRow {
  name: string
  type: string
  required: boolean
  description: string
  parentKey?: string
}

// 展开 JSON Schema 的 properties（及数组项 items.properties）
function extractParamRows(schema?: Record<string, unknown>): ParamRow[] {
  if (!schema) return []
  const props = (schema.properties ?? {}) as Record<string, Record<string, unknown>>
  const reqSet = new Set<string>(Array.isArray(schema.required) ? (schema.required as string[]) : [])
  const rows: ParamRow[] = []
  for (const [name, p] of Object.entries(props)) {
    const type = String(p?.type ?? 'any')
    rows.push({
      name,
      type,
      required: reqSet.has(name),
      description: formatParamDescription(name, String(p?.description ?? '')),
    })
    if (type === 'array' && p?.items && typeof p.items === 'object') {
      const itemsObj = p.items as Record<string, unknown>
      const itemProps = (itemsObj.properties ?? {}) as Record<string, Record<string, unknown>>
      const itemReq = new Set<string>(Array.isArray(itemsObj.required) ? (itemsObj.required as string[]) : [])
      for (const [subName, subP] of Object.entries(itemProps)) {
        rows.push({
          name: subName,
          type: String(subP?.type ?? 'any'),
          required: itemReq.has(subName),
          description: formatParamDescription(subName, String(subP?.description ?? '')),
          parentKey: name,
        })
      }
    }
  }
  return rows
}

function CopySchemaButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch { /* ignore */ }
  }
  return (
    <Button size="sm" variant="ghost" icon={copied ? <Check className="size-3.5 text-ok" /> : <Copy className="size-3.5" />} onClick={() => void copy()}>
      {copied ? '已复制' : '复制 Schema'}
    </Button>
  )
}

export default function ToolsPage() {
  const catalog = useAsync(() => api.systemCatalog(), [])
  const interceptCfg = useAsync(() => api.interceptConfig(), [])
  const [query, setQuery] = useState('')
  const [categoryFilter, setCategoryFilter] = useState<'all' | 'resident' | 'deferred' | 'controlled'>('all')
  const [skillFilter, setSkillFilter] = useState<string>('all')
  const [selectedTool, setSelectedTool] = useState<CatalogTool | null>(null)

  const reload = async () => {
    await Promise.all([catalog.reload(), interceptCfg.reload()])
  }

  const allTools = catalog.data?.tools ?? []
  const skills = catalog.data?.skills ?? []
  const controlledSet = useMemo(() => new Set(interceptCfg.data?.enabled_tools ?? []), [interceptCfg.data?.enabled_tools])

  const filteredTools = useMemo(() => {
    return allTools.filter((t) => {
      if (categoryFilter === 'resident' && t.category !== 'resident') return false
      if (categoryFilter === 'deferred' && t.category !== 'deferred') return false
      if (categoryFilter === 'controlled' && !controlledSet.has(t.name)) return false
      if (skillFilter !== 'all' && t.unlocked_by !== skillFilter) return false
      const q = query.trim().toLowerCase()
      if (!q) return true
      const cnTitle = formatToolTitle(t.name)
      const cnDesc = formatToolDescription(t.name, t.description)
      const cnSkill = formatSkillTitle(t.unlocked_by)
      return `${cnTitle} ${t.name} ${cnDesc} ${t.description} ${cnSkill} ${t.unlocked_by ?? ''}`.toLowerCase().includes(q)
    })
  }, [allTools, categoryFilter, skillFilter, query, controlledSet])

  const residentCount = allTools.filter((t) => t.category === 'resident').length
  const deferredCount = allTools.filter((t) => t.category === 'deferred').length
  const controlledBizCount = allTools.filter((t) => controlledSet.has(t.name)).length
  const paramRows = useMemo(() => extractParamRows(selectedTool?.input_schema), [selectedTool])

  return (
    <PageFrame
      title="工具"
      description="招投标业务工具注册表（常驻基础工具、Skill 按需解锁工具与入参 Schema 清单）"
      actions={
        <IconButton label="刷新工具列表" onClick={() => void reload()}>
          <RefreshCw className="size-4" />
        </IconButton>
      }
    >
      <ErrorBanner error={catalog.error || interceptCfg.error} onRetry={() => void reload()} />

      {/* 顶部筛选栏 */}
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <div className="relative min-w-56 flex-1 sm:max-w-xs">
          <Search className="pointer-events-none absolute top-2 left-2.5 size-4 text-muted" />
          <Input
            aria-label="搜索工具"
            className="pl-8"
            placeholder="搜索工具名称、标识、功能说明或所属 Skill…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
        <Select
          aria-label="工具类型筛选"
          className="w-48"
          value={categoryFilter}
          onChange={(e) => setCategoryFilter(e.target.value as typeof categoryFilter)}
        >
          <option value="all">全部工具 ({allTools.length})</option>
          <option value="resident">常驻基础工具 ({residentCount})</option>
          <option value="deferred">Skill 解锁工具 ({deferredCount})</option>
          <option value="controlled">受控拦截工具 ({controlledBizCount})</option>
        </Select>
        <Select
          aria-label="按所属 Skill 筛选"
          className="w-56"
          value={skillFilter}
          onChange={(e) => setSkillFilter(e.target.value)}
        >
          <option value="all">全部所属 Skill</option>
          {skills.map((s) => (
            <option key={s.Name} value={s.Name}>
              Skill：{formatSkillTitle(s.Name)}
            </option>
          ))}
        </Select>
      </div>

      {catalog.loading && !catalog.data ? (
        <Spinner />
      ) : filteredTools.length === 0 ? (
        <Card><Empty>没有匹配的业务工具</Empty></Card>
      ) : (
        <Card
          title="工具清单"
          subtitle={`当前显示 ${filteredTools.length} / 共 ${allTools.length} 个招投标业务工具`}
        >
          <div className="grid grid-cols-1 gap-3.5 md:grid-cols-2 xl:grid-cols-3">
            {filteredTools.map((t) => {
              const rows = extractParamRows(t.input_schema).filter((r) => !r.parentKey)
              const reqCount = rows.filter((r) => r.required).length
              const isControlled = controlledSet.has(t.name)
              const cnTitle = formatToolTitle(t.name)
              const cnDesc = formatToolDescription(t.name, t.description)
              return (
                <button
                  key={t.name}
                  type="button"
                  onClick={() => setSelectedTool(t)}
                  className="flex flex-col justify-between gap-3 rounded-[12px] border border-line bg-panel p-4 text-left transition-colors hover:border-fg/40"
                >
                  <div>
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <div className="flex min-w-0 items-center gap-2">
                        <Cpu className="size-4 shrink-0 text-muted" />
                        <span className="truncate text-sm font-semibold text-fg">{cnTitle}</span>
                      </div>
                      <div className="flex flex-wrap items-center gap-1.5">
                        {t.category === 'resident' ? (
                          <Badge tone="green">常驻下发</Badge>
                        ) : (
                          <Badge tone="purple">
                            <Layers className="mr-1 inline size-3" />
                            {formatSkillTitle(t.unlocked_by)}
                          </Badge>
                        )}
                        {isControlled && <Badge tone="amber">受控拦截</Badge>}
                      </div>
                    </div>
                    <div className="mt-1">
                      <code className="rounded bg-soft px-1.5 py-0.5 font-mono text-[11px] text-muted">{t.name}</code>
                    </div>
                    <p className="mt-2 line-clamp-2 text-xs leading-relaxed text-muted">
                      {cnDesc}
                    </p>
                  </div>
                  <div className="flex flex-wrap items-center justify-between gap-2 border-t border-line pt-2.5 text-[11px] text-muted">
                    <span>
                      入参：{rows.length} 项{reqCount > 0 ? `（${reqCount} 项必填）` : ''}
                    </span>
                    <span className="text-link">查看 Schema →</span>
                  </div>
                </button>
              )
            })}
          </div>
        </Card>
      )}

      {/* 右侧工具参数与 Schema 详情抽屉 */}
      <Drawer
        open={selectedTool !== null}
        title={selectedTool ? `工具详情 · ${formatToolTitle(selectedTool.name)}` : '工具详情'}
        width={640}
        onClose={() => setSelectedTool(null)}
      >
        {selectedTool && (
          <div className="space-y-5 p-4">
            <div className="rounded-[12px] border border-line bg-soft/30 p-3.5">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-2">
                  <Wrench className="size-4 text-muted" />
                  <span className="text-base font-semibold text-fg">{formatToolTitle(selectedTool.name)}</span>
                  <code className="rounded bg-soft px-1.5 py-0.5 font-mono text-xs text-muted">{selectedTool.name}</code>
                  {selectedTool.category === 'resident' ? (
                    <Badge tone="green">常驻基础工具</Badge>
                  ) : (
                    <Badge tone="purple">Skill 按需解锁</Badge>
                  )}
                  {controlledSet.has(selectedTool.name) && <Badge tone="amber">受控拦截中</Badge>}
                </div>
                {selectedTool.input_schema && <CopySchemaButton text={pretty(selectedTool.input_schema)} />}
              </div>
              <p className="mt-2 text-xs leading-relaxed text-muted">
                {formatToolDescription(selectedTool.name, selectedTool.description)}
              </p>
              <div className="mt-3 flex flex-wrap items-center gap-3 border-t border-line pt-2.5 text-xs text-muted">
                <span>适用 Agent：<strong className="font-mono text-fg">worker</strong></span>
                {selectedTool.unlocked_by && (
                  <span>
                    所属 Skill：
                    <a {...linkProps('/skills')} className="ml-1 font-medium text-link hover:underline">
                      {formatSkillTitle(selectedTool.unlocked_by)} ({selectedTool.unlocked_by})
                    </a>
                  </span>
                )}
                <a {...linkProps('/intercepts/rules')} className="ml-auto inline-flex items-center gap-1 text-link hover:underline">
                  <ShieldAlert className="size-3.5" /> 配置拦截规则 →
                </a>
              </div>
            </div>

            {/* 结构化入参说明表 */}
            <div className="space-y-2">
              <div className="text-xs font-semibold text-fg">入参 Schema 明细（共 {paramRows.length} 项）</div>
              {paramRows.length === 0 ? (
                <div className="rounded-lg border border-dashed border-line p-4 text-center text-xs text-muted">
                  该工具无需调用参数或未声明结构化属性
                </div>
              ) : (
                <div className="overflow-hidden rounded-[12px] border border-line">
                  <table className="w-full text-left text-xs">
                    <thead className="border-b border-line bg-soft/50 text-muted">
                      <tr>
                        <th className="px-3 py-2 font-medium">参数名</th>
                        <th className="px-3 py-2 font-medium">类型</th>
                        <th className="px-3 py-2 font-medium">是否必填</th>
                        <th className="px-3 py-2 font-medium">参数说明</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-line">
                      {paramRows.map((r) => (
                        <tr key={r.parentKey ? `${r.parentKey}.${r.name}` : r.name}>
                          <td className="px-3 py-2 font-mono font-medium text-fg">
                            {r.parentKey ? <span className="text-muted">{r.parentKey}[].</span> : null}
                            {r.name}
                          </td>
                          <td className="px-3 py-2 whitespace-nowrap text-muted">
                            {schemaTypeLabel[r.type] || r.type}
                          </td>
                          <td className="px-3 py-2 whitespace-nowrap">
                            {r.required ? <Badge tone="amber">必填</Badge> : <span className="text-muted">选填</span>}
                          </td>
                          <td className="px-3 py-2 leading-relaxed text-muted">{r.description || '—'}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </div>

            {/* 原始 JSON Schema */}
            <div className="space-y-2">
              <div className="text-xs font-semibold text-fg">原始 JSON Schema</div>
              {selectedTool.input_schema ? (
                <Pre className="max-h-96 text-xs">{pretty(selectedTool.input_schema)}</Pre>
              ) : (
                <Empty>无 Schema 定义</Empty>
              )}
            </div>
          </div>
        )}
      </Drawer>
    </PageFrame>
  )
}
