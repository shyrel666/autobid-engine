import { useState } from 'react'
import { Bot, ChevronDown, ChevronRight, Cpu, Layers, Search, Sparkles, Wrench } from 'lucide-react'
import { api, errorText } from '../../api/client'
import type { AgentRoleKey, LLMProfile } from '../../api/types'
import { Badge, Card, Empty, ErrorBanner, Input, Pre, Select, Spinner, useToast } from '../../components/ui'
import { useAsync } from '../../hooks/useAsync'
import {
  agentRoleLabel,
  formatSkillTitle,
  formatToolDescription,
  formatToolTitle,
  pretty,
} from '../../lib/format'

export function SystemCatalogPanel({ profiles }: { profiles: LLMProfile[] }) {
  const toast = useToast()
  const catalog = useAsync(() => api.systemCatalog(), [])
  const [toolQuery, setToolQuery] = useState('')
  const [categoryFilter, setCategoryFilter] = useState<'all' | 'resident' | 'deferred'>('all')
  const [expandedTool, setExpandedTool] = useState<string | null>(null)
  const [expandedSkill, setExpandedSkill] = useState<string | null>(null)
  const [bindingAgent, setBindingAgent] = useState<string>('')

  const handleBind = async (agentKey: AgentRoleKey, profileID: number) => {
    setBindingAgent(agentKey)
    try {
      await api.bindAgent(agentKey, profileID)
      toast('ok', `已更新「${agentRoleLabel[agentKey] || agentKey}」的模型绑定`)
      void catalog.reload()
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setBindingAgent('')
    }
  }

  if (catalog.loading && !catalog.data) return <Spinner />
  if (catalog.error) return <ErrorBanner error={catalog.error} onRetry={catalog.reload} />

  const agents = catalog.data?.agents ?? []
  const skills = catalog.data?.skills ?? []
  const tools = (catalog.data?.tools ?? []).filter((t) => {
    if (categoryFilter !== 'all' && t.category !== categoryFilter) return false
    const q = toolQuery.trim().toLowerCase()
    if (!q) return true
    return `${formatToolTitle(t.name)} ${t.name} ${formatToolDescription(t.name, t.description)} ${formatSkillTitle(t.unlocked_by)}`.toLowerCase().includes(q)
  })

  return (
    <div className="space-y-6">
      {/* 1. 智能体角色与模型绑定面板 */}
      <Card
        title={
          <span className="inline-flex items-center gap-2">
            <Bot className="size-4" /> 多智能体角色职责与专属模型绑定
          </span>
        }
      >
        <div className="grid gap-3 md:grid-cols-3">
          {agents.map((ag) => (
            <div key={ag.key} className="flex flex-col justify-between rounded-[12px] border border-line bg-soft/30 p-3.5">
              <div>
                <div className="flex items-center justify-between gap-2">
                  <span className="text-sm font-semibold">{agentRoleLabel[ag.key] || ag.name}</span>
                  <Badge tone="blue">{ag.key}</Badge>
                </div>
                <p className="mt-1.5 text-xs leading-relaxed text-muted">{ag.description}</p>
              </div>
              <div className="mt-3 border-t border-line pt-2.5">
                <div className="mb-1 text-[11px] font-medium text-muted">绑定专属模型配置</div>
                <Select
                  aria-label={`${agentRoleLabel[ag.key] || ag.name} 模型绑定`}
                  value={ag.bound_profile_id || 0}
                  disabled={bindingAgent === ag.key}
                  onChange={(e) => void handleBind(ag.key, Number(e.target.value))}
                >
                  <option value={0}>不绑定（跟随任务链 / 全局激活池）</option>
                  {profiles.filter((p) => p.enabled).map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}（{p.model}）
                    </option>
                  ))}
                </Select>
              </div>
            </div>
          ))}
        </div>
      </Card>

      {/* 2. 领域技能包与渐进式工具解锁 */}
      <Card
        title={
          <span className="inline-flex items-center gap-2">
            <Sparkles className="size-4" /> 招投标业务技能包（按需渐进解锁专业工具）
          </span>
        }
      >
        <div className="grid gap-3 lg:grid-cols-3">
          {skills.map((sk) => {
            const isExp = expandedSkill === sk.Name
            return (
              <div key={sk.Name} className="flex flex-col rounded-[12px] border border-line bg-panel p-3.5">
                <div className="flex items-start justify-between gap-2">
                  <div>
                    <div className="text-sm font-semibold">{formatSkillTitle(sk.Name)}</div>
                    <code className="font-mono text-[11px] text-muted">{sk.Name}</code>
                    <p className="mt-1 text-xs text-muted">{sk.Description}</p>
                  </div>
                  <Badge tone="purple">{sk.Tools?.length ?? 0} 个工具</Badge>
                </div>
                <div className="mt-2.5 flex flex-wrap gap-1.5">
                  {(sk.Tools ?? []).map((tn) => (
                    <span key={tn} title={tn} className="rounded bg-soft px-2 py-0.5 text-[11px] text-fg">
                      {formatToolTitle(tn)}
                    </span>
                  ))}
                </div>
                {sk.Prompt && (
                  <div className="mt-3 border-t border-line pt-2">
                    <button
                      type="button"
                      onClick={() => setExpandedSkill(isExp ? null : sk.Name)}
                      className="inline-flex items-center gap-1 text-xs font-medium text-link hover:underline"
                    >
                      {isExp ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
                      {isExp ? '收起技能作业规程' : '查看技能作业规程'}
                    </button>
                    {isExp && <Pre className="mt-2 max-h-56 text-[11px]">{sk.Prompt}</Pre>}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      </Card>

      {/* 3. 业务工具箱与参数定义清单 */}
      <Card
        title={
          <span className="inline-flex items-center gap-2">
            <Wrench className="size-4" /> 招投标业务工具清单（共 {tools.length} 个）
          </span>
        }
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative w-52">
              <Search className="pointer-events-none absolute top-2 left-2.5 size-3.5 text-muted" />
              <Input
                aria-label="搜索工具"
                className="h-7 pl-8 text-xs"
                placeholder="搜索工具名称或说明…"
                value={toolQuery}
                onChange={(e) => setToolQuery(e.target.value)}
              />
            </div>
            <Select
              aria-label="工具分类筛选"
              className="h-7 w-40 text-xs"
              value={categoryFilter}
              onChange={(e) => setCategoryFilter(e.target.value as 'all' | 'resident' | 'deferred')}
            >
              <option value="all">全部工具类型</option>
              <option value="resident">常驻基础工具</option>
              <option value="deferred">技能解锁工具</option>
            </Select>
          </div>
        }
        bodyClassName="p-0"
      >
        {tools.length === 0 ? (
          <Empty>没有匹配的业务工具</Empty>
        ) : (
          <div className="divide-y divide-line-soft">
            {tools.map((t) => {
              const open = expandedTool === t.name
              return (
                <div key={t.name} className="px-4 py-3">
                  <div
                    onClick={() => setExpandedTool(open ? null : t.name)}
                    className="flex cursor-pointer flex-wrap items-center justify-between gap-2"
                  >
                    <div className="flex min-w-0 items-center gap-2">
                      {open ? <ChevronDown className="size-4 text-muted" /> : <ChevronRight className="size-4 text-muted" />}
                      <Cpu className="size-4 shrink-0 text-muted" />
                      <span className="text-sm font-semibold">{formatToolTitle(t.name)}</span>
                      <code className="rounded bg-soft px-1.5 py-0.5 font-mono text-xs text-muted">{t.name}</code>
                      {t.category === 'resident' ? (
                        <Badge tone="green">常驻下发</Badge>
                      ) : (
                        <Badge tone="purple">
                          <Layers className="mr-1 inline size-3" />
                          由「{formatSkillTitle(t.unlocked_by)}」解锁
                        </Badge>
                      )}
                    </div>
                    <span className="text-xs text-muted">{open ? '点击收起参数定义' : '查看调用参数定义'}</span>
                  </div>
                  <p className="mt-1 pl-6 text-xs leading-relaxed text-muted">
                    {formatToolDescription(t.name, t.description)}
                  </p>
                  {open && t.input_schema && (
                    <div className="mt-2 pl-6">
                      <Pre className="max-h-64 text-xs">{pretty(t.input_schema)}</Pre>
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        )}
      </Card>
    </div>
  )
}
