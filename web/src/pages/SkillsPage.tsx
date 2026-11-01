import { useMemo, useState } from 'react'
import {
  Check, ChevronDown, ChevronRight, Copy, Cpu, FileText, Folder, FolderOpen,
  Layers, LayoutGrid, RefreshCw, Search, Sparkles, Wrench,
} from 'lucide-react'
import { api } from '../api/client'
import type { CatalogSkill, CatalogTool } from '../api/types'
import { GlobalHeader, LLMStatusText } from '../components/layout/AppShell'
import { Badge, Button, Card, Empty, ErrorBanner, IconButton, Input, Pre, Spinner, Tabs, cx } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import {
  formatParamDescription,
  formatSkillTitle,
  formatToolDescription,
  formatToolTitle,
  pretty,
  schemaTypeLabel,
} from '../lib/format'
import { Markdown } from '../lib/markdown'
import { linkProps } from '../lib/router'

type SelectedItem =
  | null
  | { kind: 'skill'; skillName: string }
  | { kind: 'tool'; skillName: string; toolName: string }

function CopyButton({ text }: { text: string }) {
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
      {copied ? '已复制' : '复制内容'}
    </Button>
  )
}

function SkillsOverview({
  skills,
  tools,
  onSelectSkill,
  onSelectTool,
}: {
  skills: CatalogSkill[]
  tools: CatalogTool[]
  onSelectSkill: (name: string) => void
  onSelectTool: (skillName: string, toolName: string) => void
}) {
  const deferredTools = tools.filter((t) => t.category === 'deferred')
  const residentTools = tools.filter((t) => t.category === 'resident')
  const ranked = useMemo(() => [...skills].sort((a, b) => (b.Tools?.length ?? 0) - (a.Tools?.length ?? 0)), [skills])
  const maxTools = ranked[0]?.Tools?.length || 1

  if (skills.length === 0) {
    return <Empty>暂无已注册的领域技能包</Empty>
  }

  const stats = [
    { label: 'Skill 总数', value: skills.length, hint: '招投标全流程专项 Skill' },
    { label: '按需解锁工具', value: deferredTools.length, hint: '激活 Skill 后动态注入 Worker' },
    { label: '常驻基础工具', value: residentTools.length, hint: '无需解锁默认下发' },
    { label: 'SKILL.md 覆盖率', value: `${skills.filter((s) => !!(s.Body || s.Prompt)).length}/${skills.length}`, hint: '配备标准作业规程文档' },
  ]

  return (
    <div className="mx-auto w-full max-w-4xl space-y-6">
      <div>
        <h2 className="text-base font-semibold">Skill 技能库总览</h2>
        <p className="mt-0.5 text-sm text-muted">
          选择左侧目录树中的 Skill 查看 `SKILL.md` 作业规程与解锁工具 Schema，或从下方快速浏览全部招投标领域 Skill。
        </p>
      </div>

      {/* 4 项指标卡 */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {stats.map((s) => (
          <div key={s.label} className="rounded-[12px] border border-line bg-panel p-3.5">
            <p className="text-2xl font-semibold tabular-nums">{s.value}</p>
            <p className="mt-0.5 text-xs font-medium text-fg">{s.label}</p>
            {s.hint && <p className="mt-0.5 text-[11px] text-muted">{s.hint}</p>}
          </div>
        ))}
      </div>

      {/* 解锁工具规模分布条形图 */}
      <div className="space-y-2.5 rounded-[12px] border border-line bg-panel p-4">
        <div className="flex items-center justify-between">
          <span className="text-xs font-semibold text-muted">各 Skill 解锁工具数量排行</span>
          <a {...linkProps('/tools')} className="text-xs text-link hover:underline">查看全部工具清单 →</a>
        </div>
        <div className="space-y-1.5">
          {ranked.map((s) => {
            const count = s.Tools?.length ?? 0
            return (
              <button
                key={s.Name}
                type="button"
                onClick={() => onSelectSkill(s.Name)}
                className="group flex w-full items-center gap-3 rounded-lg px-2.5 py-1.5 text-left transition-colors hover:bg-soft"
              >
                <span className="w-52 shrink-0 truncate text-xs font-medium text-fg" title={`${formatSkillTitle(s.Name)} (${s.Name})`}>
                  {formatSkillTitle(s.Name)}
                </span>
                <span className="relative h-2 flex-1 overflow-hidden rounded-full bg-soft">
                  <span
                    className="absolute inset-y-0 left-0 rounded-full bg-fg/75 transition-all group-hover:bg-fg"
                    style={{ width: `${Math.max(8, (count / maxTools) * 100)}%` }}
                  />
                </span>
                <span className="w-20 shrink-0 text-right text-xs text-muted tabular-nums">{count} 个工具</span>
              </button>
            )
          })}
        </div>
      </div>

      {/* 领域技能卡片矩阵 */}
      <div className="space-y-2.5">
        <div className="text-xs font-semibold text-muted">领域技能包明细</div>
        <div className="grid gap-3 sm:grid-cols-2">
          {skills.map((sk) => (
            <div
              key={sk.Name}
              className="flex flex-col justify-between rounded-[12px] border border-line bg-panel p-4 transition-colors hover:border-fg/30"
            >
              <div>
                <div className="flex items-start justify-between gap-2">
                  <div>
                    <button
                      type="button"
                      onClick={() => onSelectSkill(sk.Name)}
                      className="inline-flex items-center gap-1.5 text-sm font-semibold text-fg hover:text-link"
                    >
                      <Sparkles className="size-3.5 text-muted" />
                      {formatSkillTitle(sk.Name)}
                    </button>
                    <div className="mt-0.5">
                      <code className="rounded bg-soft px-1.5 py-0.5 font-mono text-[11px] text-muted">{sk.Name}</code>
                    </div>
                  </div>
                  <Badge tone="purple">{sk.Tools?.length ?? 0} 个工具</Badge>
                </div>
                <p className="mt-2 text-xs leading-relaxed text-muted">{sk.Description}</p>
              </div>
              <div className="mt-3 border-t border-line pt-2.5">
                <div className="mb-1.5 text-[11px] text-muted">点击查看规程或直接检查解锁工具：</div>
                <div className="flex flex-wrap gap-1.5">
                  {(sk.Tools ?? []).map((tn) => (
                    <button
                      key={tn}
                      type="button"
                      title={tn}
                      onClick={() => onSelectTool(sk.Name, tn)}
                      className="rounded-md border border-line bg-soft/60 px-2 py-0.5 text-[11px] text-fg transition-colors hover:border-fg/40 hover:bg-soft"
                    >
                      {formatToolTitle(tn)}
                    </button>
                  ))}
                </div>
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

export default function SkillsPage() {
  const catalog = useAsync(() => api.systemCatalog(), [])
  const [search, setSearch] = useState('')
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const [selected, setSelected] = useState<SelectedItem>(null)
  const [skillTab, setSkillTab] = useState<'prompt' | 'tools'>('prompt')
  const [rawPrompt, setRawPrompt] = useState(false)

  const skills = catalog.data?.skills ?? []
  const tools = catalog.data?.tools ?? []

  const filteredSkills = useMemo(() => {
    const q = search.trim().toLowerCase()
    if (!q) return skills
    return skills.filter((s) => {
      const toolTitles = (s.Tools ?? []).map((t) => `${formatToolTitle(t)} ${t}`).join(' ')
      return `${formatSkillTitle(s.Name)} ${s.Name} ${s.Description} ${toolTitles}`.toLowerCase().includes(q)
    })
  }, [skills, search])

  const isExpanded = (name: string) => expanded[name] ?? true
  const toggleExpand = (name: string) => setExpanded((prev) => ({ ...prev, [name]: !isExpanded(name) }))

  const activeSkill = selected ? skills.find((s) => s.Name === selected.skillName) : undefined
  const activeTool = selected?.kind === 'tool' ? tools.find((t) => t.name === selected.toolName) : undefined

  return (
    <>
      <GlobalHeader status={<LLMStatusText />} />
      <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
        {/* 顶栏标题区 */}
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-3 md:px-6">
          <div className="min-w-0">
            <h1 className="flex items-center gap-2 text-lg font-semibold">
              <Sparkles className="size-4 text-muted" />
              <span>Skill</span>
              {skills.length > 0 && <Badge tone="slate">{skills.length}</Badge>}
            </h1>
            <p className="text-xs text-muted">
              招投标领域 Skill 按需向 Worker 注入标准作业规程（`SKILL.md`）并解锁专业工具。
            </p>
          </div>
          <div className="flex items-center gap-2">
            {selected !== null && (
              <Button size="sm" variant="secondary" icon={<LayoutGrid className="size-3.5" />} onClick={() => setSelected(null)}>
                Skill 总览
              </Button>
            )}
            <IconButton label="刷新 Skill" onClick={() => void catalog.reload()}>
              <RefreshCw className="size-4" />
            </IconButton>
          </div>
        </div>

        {catalog.error && <div className="p-4"><ErrorBanner error={catalog.error} onRetry={catalog.reload} /></div>}

        {catalog.loading && !catalog.data ? (
          <div className="p-8"><Spinner /></div>
        ) : (
          <div className="flex min-h-0 min-w-0 flex-1 flex-col md:flex-row">
            {/* 左侧文件树风格目录栏 */}
            <aside className="flex w-full shrink-0 flex-col border-b border-line bg-soft/20 md:w-72 md:border-r md:border-b-0">
              <div className="border-b border-line p-3">
                <div className="relative">
                  <Search className="pointer-events-none absolute top-2 left-2.5 size-3.5 text-muted" />
                  <Input
                    aria-label="搜索 Skill 或工具"
                    className="h-8 pl-8 text-xs"
                    placeholder="过滤 Skill 或解锁工具…"
                    value={search}
                    onChange={(e) => setSearch(e.target.value)}
                  />
                </div>
              </div>
              <div className="min-h-0 flex-1 overflow-y-auto p-2">
                <button
                  type="button"
                  onClick={() => setSelected(null)}
                  className={cx(
                    'mb-1 flex w-full items-center gap-2 rounded-lg px-2.5 py-1.5 text-left text-xs font-medium transition-colors',
                    selected === null ? 'bg-soft font-semibold text-fg' : 'text-muted hover:bg-soft/60 hover:text-fg',
                  )}
                >
                  <LayoutGrid className="size-3.5 shrink-0" />
                  <span>Skill 技能库总览</span>
                </button>

                {filteredSkills.length === 0 ? (
                  <p className="px-2.5 py-6 text-center text-xs text-muted">无匹配的技能包</p>
                ) : (
                  filteredSkills.map((sk) => {
                    const open = isExpanded(sk.Name)
                    const isSkillSelected = selected?.kind === 'skill' && selected.skillName === sk.Name
                    return (
                      <div key={sk.Name} className="mb-1">
                        <div
                          className={cx(
                            'flex items-center gap-1 rounded-lg px-2 py-1.5 text-xs transition-colors',
                            isSkillSelected ? 'bg-soft font-semibold text-fg' : 'text-fg hover:bg-soft/60',
                          )}
                        >
                          <button
                            type="button"
                            aria-label={open ? `折叠 ${formatSkillTitle(sk.Name)}` : `展开 ${formatSkillTitle(sk.Name)}`}
                            onClick={() => toggleExpand(sk.Name)}
                            className="grid size-4 shrink-0 place-items-center text-muted hover:text-fg"
                          >
                            {open ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
                          </button>
                          <button
                            type="button"
                            onClick={() => {
                              setSelected({ kind: 'skill', skillName: sk.Name })
                              if (!open) toggleExpand(sk.Name)
                            }}
                            className="flex min-w-0 flex-1 items-center gap-1.5 text-left"
                            title={`${formatSkillTitle(sk.Name)} (${sk.Name})`}
                          >
                            {open ? <FolderOpen className="size-3.5 shrink-0 text-amber-500" /> : <Folder className="size-3.5 shrink-0 text-amber-500" />}
                            <span className="truncate">{formatSkillTitle(sk.Name)}</span>
                          </button>
                          <span className="rounded bg-soft px-1.5 py-0.5 text-[10px] text-muted tabular-nums">
                            {sk.Tools?.length ?? 0}
                          </span>
                        </div>

                        {open && (
                          <div className="mt-0.5 ml-4 space-y-0.5 border-l border-line pl-2">
                            <button
                              type="button"
                              onClick={() => {
                                setSelected({ kind: 'skill', skillName: sk.Name })
                                setSkillTab('prompt')
                              }}
                              className={cx(
                                'flex w-full items-center gap-1.5 rounded-md px-2 py-1 text-left font-mono text-xs transition-colors',
                                isSkillSelected && skillTab === 'prompt' ? 'bg-soft font-semibold text-fg' : 'text-muted hover:bg-soft/60 hover:text-fg',
                              )}
                            >
                              <FileText className="size-3.5 shrink-0 text-link" />
                              <span className="truncate">SKILL.md</span>
                            </button>
                            {(sk.Tools ?? []).map((tn) => {
                              const isToolSelected = selected?.kind === 'tool' && selected.skillName === sk.Name && selected.toolName === tn
                              return (
                                <button
                                  key={tn}
                                  type="button"
                                  title={tn}
                                  onClick={() => setSelected({ kind: 'tool', skillName: sk.Name, toolName: tn })}
                                  className={cx(
                                    'flex w-full items-center gap-1.5 rounded-md px-2 py-1 text-left text-xs transition-colors',
                                    isToolSelected ? 'bg-soft font-semibold text-fg' : 'text-muted hover:bg-soft/60 hover:text-fg',
                                  )}
                                >
                                  <Wrench className="size-3 shrink-0 text-purple-600" />
                                  <span className="truncate">{formatToolTitle(tn)}</span>
                                </button>
                              )
                            })}
                          </div>
                        )}
                      </div>
                    )
                  })
                )}
              </div>
            </aside>

            {/* 右侧主内容区 */}
            <div className="min-h-0 min-w-0 flex-1 overflow-y-auto p-4 md:p-6">
              {selected === null ? (
                <SkillsOverview
                  skills={skills}
                  tools={tools}
                  onSelectSkill={(name) => {
                    setSelected({ kind: 'skill', skillName: name })
                    setSkillTab('prompt')
                  }}
                  onSelectTool={(skillName, toolName) => setSelected({ kind: 'tool', skillName, toolName })}
                />
              ) : selected.kind === 'skill' && activeSkill ? (
                <div className="mx-auto max-w-4xl space-y-5">
                  <div className="flex flex-wrap items-start justify-between gap-3 border-b border-line pb-4">
                    <div>
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="text-lg font-semibold text-fg">{formatSkillTitle(activeSkill.Name)}</span>
                        <code className="rounded bg-soft px-1.5 py-0.5 font-mono text-xs text-muted">{activeSkill.Name}</code>
                        <Badge tone="purple">内置技能</Badge>
                        <Badge tone="blue">解锁 {activeSkill.Tools?.length ?? 0} 个专业工具</Badge>
                      </div>
                      <p className="mt-1.5 text-sm text-muted">{activeSkill.Description}</p>
                    </div>
                    {(activeSkill.Body || activeSkill.Prompt) && <CopyButton text={activeSkill.Body || activeSkill.Prompt || ''} />}
                  </div>

                  <Tabs
                    tabs={[
                      { key: 'prompt', label: '作业规程 (SKILL.md)' },
                      { key: 'tools', label: `解锁工具清单 (${activeSkill.Tools?.length ?? 0})` },
                    ]}
                    value={skillTab}
                    onChange={setSkillTab}
                  />

                  {skillTab === 'prompt' ? (
                    <Card
                      title={
                        <span className="font-mono text-xs text-muted">
                          skills/{activeSkill.Name}/SKILL.md
                        </span>
                      }
                      actions={
                        <div className="flex items-center gap-2">
                          <Button size="sm" variant={rawPrompt ? 'primary' : 'secondary'} onClick={() => setRawPrompt((v) => !v)}>
                            {rawPrompt ? '预览排版' : '原始源码'}
                          </Button>
                        </div>
                      }
                    >
                      {!(activeSkill.Body || activeSkill.Prompt) ? (
                        <Empty>该技能包未定义额外的作业规程文本</Empty>
                      ) : rawPrompt ? (
                        <Pre className="max-h-[540px]">{activeSkill.Body || activeSkill.Prompt}</Pre>
                      ) : (
                        <div className="prose-sm max-w-none text-sm leading-relaxed">
                          <Markdown source={activeSkill.Body || activeSkill.Prompt || ''} />
                        </div>
                      )}
                    </Card>
                  ) : (
                    <div className="space-y-3">
                      {(activeSkill.Tools ?? []).map((tn) => {
                        const toolObj = tools.find((t) => t.name === tn)
                        return (
                          <Card
                            key={tn}
                            title={
                              <span className="inline-flex flex-wrap items-center gap-2">
                                <Cpu className="size-4 text-muted" />
                                <span className="text-sm font-semibold">{formatToolTitle(tn)}</span>
                                <code className="rounded bg-soft px-1.5 py-0.5 font-mono text-xs text-muted">{tn}</code>
                                <Badge tone="purple">
                                  <Layers className="mr-1 inline size-3" />
                                  由「{formatSkillTitle(activeSkill.Name)}」解锁
                                </Badge>
                              </span>
                            }
                            actions={
                              <Button
                                size="sm"
                                variant="ghost"
                                onClick={() => setSelected({ kind: 'tool', skillName: activeSkill.Name, toolName: tn })}
                              >
                                查看参数说明 →
                              </Button>
                            }
                          >
                            <p className="text-xs leading-relaxed text-muted">
                              {formatToolDescription(tn, toolObj?.description)}
                            </p>
                          </Card>
                        )
                      })}
                    </div>
                  )}
                </div>
              ) : selected.kind === 'tool' ? (
                <div className="mx-auto max-w-4xl space-y-4">
                  <div className="flex flex-wrap items-center justify-between gap-2 border-b border-line pb-4">
                    <div>
                      <div className="flex flex-wrap items-center gap-2">
                        <button
                          type="button"
                          onClick={() => setSelected({ kind: 'skill', skillName: selected.skillName })}
                          className="text-sm font-medium text-link hover:underline"
                        >
                          {formatSkillTitle(selected.skillName)}
                        </button>
                        <span className="text-muted">/</span>
                        <span className="text-lg font-semibold">{formatToolTitle(selected.toolName)}</span>
                        <code className="rounded bg-soft px-1.5 py-0.5 font-mono text-xs text-muted">{selected.toolName}</code>
                        <Badge tone="purple">技能按需解锁工具</Badge>
                      </div>
                      <p className="mt-1.5 text-sm leading-relaxed text-muted">
                        {formatToolDescription(selected.toolName, activeTool?.description)}
                      </p>
                    </div>
                    {activeTool?.input_schema && <CopyButton text={pretty(activeTool.input_schema)} />}
                  </div>

                  {activeTool?.input_schema && (() => {
                    const props = (activeTool.input_schema.properties ?? {}) as Record<string, Record<string, unknown>>
                    const reqSet = new Set<string>(Array.isArray(activeTool.input_schema.required) ? (activeTool.input_schema.required as string[]) : [])
                    const entries = Object.entries(props)
                    return entries.length > 0 ? (
                      <Card title={`调用参数说明（共 ${entries.length} 项）`} bodyClassName="p-0">
                        <table className="w-full text-left text-xs">
                          <thead className="border-b border-line bg-soft/50 text-muted">
                            <tr>
                              <th className="px-4 py-2.5 font-medium">参数名</th>
                              <th className="px-4 py-2.5 font-medium">类型</th>
                              <th className="px-4 py-2.5 font-medium">是否必填</th>
                              <th className="px-4 py-2.5 font-medium">参数说明</th>
                            </tr>
                          </thead>
                          <tbody className="divide-y divide-line">
                            {entries.map(([pName, pDef]) => {
                              const pType = String(pDef?.type ?? 'any')
                              return (
                                <tr key={pName}>
                                  <td className="px-4 py-2.5 font-mono font-medium text-fg">{pName}</td>
                                  <td className="px-4 py-2.5 whitespace-nowrap text-muted">{schemaTypeLabel[pType] || pType}</td>
                                  <td className="px-4 py-2.5 whitespace-nowrap">
                                    {reqSet.has(pName) ? <Badge tone="amber">必填</Badge> : <span className="text-muted">选填</span>}
                                  </td>
                                  <td className="px-4 py-2.5 leading-relaxed text-muted">
                                    {formatParamDescription(pName, String(pDef?.description ?? ''))}
                                  </td>
                                </tr>
                              )
                            })}
                          </tbody>
                        </table>
                      </Card>
                    ) : null
                  })()}

                  <Card title="参数结构定义（JSON 格式 · 供开发调试参考）">
                    {activeTool?.input_schema ? (
                      <Pre className="max-h-[540px]">{pretty(activeTool.input_schema)}</Pre>
                    ) : (
                      <Empty>该工具当前未返回独立参数结构快照</Empty>
                    )}
                  </Card>
                </div>
              ) : null}
            </div>
          </div>
        )}
      </div>
    </>
  )
}
