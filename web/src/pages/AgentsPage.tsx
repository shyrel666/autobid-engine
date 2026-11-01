import { useState } from 'react'
import { Bot, Cpu, RefreshCw, ShieldAlert, Sparkles, Wrench } from 'lucide-react'
import { api, errorText } from '../api/client'
import type { AgentRoleKey, CatalogAgent } from '../api/types'
import { PageFrame } from '../components/layout/AppShell'
import { Badge, Button, Card, Drawer, Empty, ErrorBanner, Field, IconButton, Select, Spinner, useToast } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { agentRoleLabel, formatSkillTitle, formatToolTitle } from '../lib/format'
import { linkProps } from '../lib/router'

const AGENT_WORKFLOW_NOTES: Record<string, { loop: string; inputs: string; outputs: string }> = {
  goals: {
    loop: '遍历招标原文分块与框线表格 → 拆解资格/否决项、★实质性条款与评分表分档规则 → 同步 assets 静态要素 → 幂等创建四大推演 Goal 根节点并生成技术标 v1 大纲草案',
    inputs: '招标文件原文切片、跨页框线表格、后发更正/澄清公告、既有要求清单版本',
    outputs: '结构化要求清单 (requirement_items)、招标静态要素 (assets)、四大推演 Goal 根节点 (G-VETO / G-DEVIATION / G-SCORING / G-OUTLINE)、技术标目录大纲草案 (proposal_outlines)',
  },
  planner: {
    loop: '读取有界推演概览与历史冷摘要 → 评估四大 Goal 根节点完成度与阻塞意图 → 派生带 Mandate 边界约束的新一轮任务执行意图',
    inputs: '招标要求清单、推演图活跃前沿、近期审查发现、Watchdog 熔断告警与人工引导指令',
    outputs: '带 Mandate 约束的推演意图节点 (exploration_nodes)、目标完成度判定 (prove_goal)',
  },
  worker: {
    loop: '争抢意图租约 → 在 Inline & Spill 上下文预算内探测常驻工具 + 按需解锁业务技能 → 提交结构化核验事实或章节草稿',
    inputs: '分配的推演意图与 Mandate 硬约束、招标原文切片、企业资信库、企业方案知识库与本章节内联/溢出证据',
    outputs: '招标静态要素、审查发现结论、证据核验记录、技术标章节修订草稿',
  },
  watchdog: {
    loop: 'PreToolUse 校验 Mandate 章节/要求/字数/工具白名单 → PostToolUse 追踪工具调用签名与报错轨迹 → 达软阈值自动注入 Steer 纠偏 → 达硬阈值触发 AbortKilledByWatchdog 熔断并唤醒 Planner',
    inputs: 'Intent Mandate 载荷约束、Worker 实时工具调用名与参数签名、工具返回状态与写回计数',
    outputs: '越界工具调用实时拦截、自动纠偏 Steer 指令、异常 Intent 熔断 (stopped) 与 TriggerWatchdogAlert 告警',
  },
  judge: {
    loop: '拦截未命中静态规则的受控工具调用 → 执行单轮无工具安全语义评估 → 返回允许放行 / 转人工审批 / 禁止阻断裁决',
    inputs: '待执行工具名称、序列化调用参数摘要、系统内置或自定义安全裁判提示词',
    outputs: '自动放行、阻断拦截或转入人工审批队列',
  },
}

export default function AgentsPage() {
  const toast = useToast()
  const catalog = useAsync(() => api.systemCatalog(), [])
  const profiles = useAsync(() => api.listProfiles(), [])
  const interceptCfg = useAsync(() => api.interceptConfig(), [])
  const [selectedKey, setSelectedKey] = useState<CatalogAgent['key'] | null>(null)
  const [bindingAgent, setBindingAgent] = useState<string>('')

  const reload = async () => {
    await Promise.all([catalog.reload(), profiles.reload(), interceptCfg.reload()])
  }

  const handleBind = async (agentKey: AgentRoleKey, profileID: number) => {
    setBindingAgent(agentKey)
    try {
      await api.bindAgent(agentKey, profileID)
      toast('ok', `已更新「${agentRoleLabel[agentKey] || agentKey}」的模型绑定`)
      await catalog.reload()
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setBindingAgent('')
    }
  }

  const agents = catalog.data?.agents ?? []
  const skills = catalog.data?.skills ?? []
  const tools = catalog.data?.tools ?? []
  const residentTools = tools.filter((t) => t.category === 'resident')
  const deferredTools = tools.filter((t) => t.category === 'deferred')
  const controlledTools = interceptCfg.data?.enabled_tools ?? []
  const profileList = profiles.data ?? []
  const selectedAgent = agents.find((a) => a.key === selectedKey) ?? null

  const getBadgeCounts = (key: CatalogAgent['key']) => {
    if (key === 'goals') {
      return [
        { label: '四域联动拆解' },
        { label: '4 类图谱 Goal 根节点' },
        { label: '大纲草案 v1 生成' },
      ]
    }
    if (key === 'worker') {
      return [
        { label: `Skill ${skills.length}` },
        { label: `常驻工具 ${residentTools.length}` },
        { label: `解锁工具 ${deferredTools.length}` },
      ]
    }
    if (key === 'planner') {
      return [
        { label: 'Skill 0' },
        { label: '推演图调度' },
        { label: 'Mandate 意图编排' },
      ]
    }
    if (key === 'watchdog') {
      return [
        { label: 'Mandate 硬件门禁' },
        { label: '3 次重复自动 Steer' },
        { label: '5 次重复强行熔断' },
      ]
    }
    return [
      { label: `受控工具 ${controlledTools.length}` },
      { label: '无工具纯语义裁决' },
      { label: interceptCfg.data?.judge?.enabled ? 'AI 裁判已开启' : 'AI 裁判默认关闭' },
    ]
  }

  return (
    <PageFrame
      title="Agent"
      description="内置 Agent 角色职责、专属 LLM 绑定与 Skill / 工具装配清单"
      actions={
        <IconButton label="刷新 Agent 清单" onClick={() => void reload()}>
          <RefreshCw className="size-4" />
        </IconButton>
      }
    >
      <ErrorBanner error={catalog.error || profiles.error} onRetry={() => void reload()} />

      {catalog.loading && !catalog.data ? (
        <Spinner />
      ) : agents.length === 0 ? (
        <Empty>暂无已注册的 Agent</Empty>
      ) : (
        <Card
          title="Agent 清单"
          subtitle={`共 ${agents.length} 个内置协同 Agent`}
        >
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
            {agents.map((ag) => {
              const boundProfile = profileList.find((p) => p.id === ag.bound_profile_id)
              const tags = getBadgeCounts(ag.key)
              const displayTitle = agentRoleLabel[ag.key] || ag.name
              return (
                <div
                  key={ag.key}
                  className="group relative flex flex-col justify-between gap-3 rounded-[12px] border border-line bg-panel p-4 transition-colors hover:border-fg/40"
                >
                  <button
                    type="button"
                    onClick={() => setSelectedKey(ag.key)}
                    className="flex flex-col gap-2 text-left"
                  >
                    <div className="flex flex-wrap items-center gap-2">
                      <Bot className="size-4 text-muted" />
                      <span className="text-sm font-semibold text-fg">{displayTitle}</span>
                      <code className="rounded bg-soft px-1.5 py-0.5 font-mono text-[11px] text-muted">{ag.key}</code>
                      <Badge tone="slate">内置</Badge>
                    </div>
                    <p className="line-clamp-2 min-h-8 text-xs leading-relaxed text-muted">
                      {ag.description}
                    </p>
                    <div className="mt-1 flex flex-wrap gap-1.5 text-[11px] text-muted">
                      {tags.map((t) => (
                        <span key={t.label} className="rounded-md border border-line bg-soft/40 px-2 py-0.5">
                          {t.label}
                        </span>
                      ))}
                    </div>
                  </button>

                  <div className="flex items-center justify-between gap-2 border-t border-line pt-2.5 text-xs">
                    <span className="truncate text-muted">
                      模型：{boundProfile ? <strong className="font-medium text-fg">{boundProfile.name}</strong> : '跟随任务链 / 全局池'}
                    </span>
                    <Button size="sm" variant="ghost" onClick={() => setSelectedKey(ag.key)}>
                      配置详情 →
                    </Button>
                  </div>
                </div>
              )
            })}
          </div>
        </Card>
      )}

      {/* 右侧 Agent 详情与配置抽屉 */}
      <Drawer
        open={selectedAgent !== null}
        title={selectedAgent ? `Agent 配置 · ${agentRoleLabel[selectedAgent.key] || selectedAgent.name}` : 'Agent 配置'}
        width={580}
        onClose={() => setSelectedKey(null)}
      >
        {selectedAgent && (
          <div className="space-y-5 p-4">
            <div className="rounded-[12px] border border-line bg-soft/30 p-3.5">
              <div className="flex flex-wrap items-center gap-2">
                <Bot className="size-4 text-muted" />
                <span className="text-sm font-semibold">{agentRoleLabel[selectedAgent.key] || selectedAgent.name}</span>
                <Badge tone="blue">{selectedAgent.key}</Badge>
                <Badge tone="slate">内置常驻 Agent</Badge>
              </div>
              <p className="mt-2 text-xs leading-relaxed text-muted">{selectedAgent.description}</p>
            </div>

            <Field
              label="专属 LLM 配置绑定"
              hint="为该 Agent 指定优先使用的 LLM 配置；留空（不绑定）时自动使用当前项目的配额链或全局激活池。"
            >
              <Select
                aria-label={`${agentRoleLabel[selectedAgent.key] || selectedAgent.name} 模型绑定`}
                value={selectedAgent.bound_profile_id || 0}
                disabled={bindingAgent === selectedAgent.key}
                onChange={(e) => void handleBind(selectedAgent.key, Number(e.target.value))}
              >
                <option value={0}>不绑定（跟随任务链 / 全局激活池）</option>
                {profileList.filter((p) => p.enabled).map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}（{p.model}）
                  </option>
                ))}
              </Select>
            </Field>

            {AGENT_WORKFLOW_NOTES[selectedAgent.key] && (
              <div className="space-y-2.5 rounded-[12px] border border-line p-3.5 text-xs">
                <div className="font-semibold text-fg">执行机制与数据流</div>
                <div>
                  <span className="font-medium text-muted">调度闭环：</span>
                  <span className="text-fg">{AGENT_WORKFLOW_NOTES[selectedAgent.key].loop}</span>
                </div>
                <div>
                  <span className="font-medium text-muted">上下文输入：</span>
                  <span className="text-fg">{AGENT_WORKFLOW_NOTES[selectedAgent.key].inputs}</span>
                </div>
                <div>
                  <span className="font-medium text-muted">持久化产出：</span>
                  <span className="text-fg">{AGENT_WORKFLOW_NOTES[selectedAgent.key].outputs}</span>
                </div>
              </div>
            )}

            {selectedAgent.key === 'worker' && (
              <>
                <div className="space-y-2">
                  <div className="flex items-center justify-between text-xs font-semibold">
                    <span className="inline-flex items-center gap-1.5">
                      <Sparkles className="size-3.5 text-muted" /> 可调用的领域 Skill（{skills.length}）
                    </span>
                    <a {...linkProps('/skills')} className="text-link hover:underline">管理 Skill →</a>
                  </div>
                  <div className="grid gap-2 sm:grid-cols-2">
                    {skills.map((sk) => (
                      <div key={sk.Name} className="rounded-lg border border-line bg-panel p-2.5 text-xs">
                        <div className="flex items-center justify-between gap-1">
                          <span className="font-semibold text-fg">{formatSkillTitle(sk.Name)}</span>
                          <Badge tone="purple">{sk.Tools?.length ?? 0} 工具</Badge>
                        </div>
                        <p className="mt-1 line-clamp-2 text-[11px] text-muted">{sk.Description}</p>
                      </div>
                    ))}
                  </div>
                </div>

                <div className="space-y-2">
                  <div className="flex items-center justify-between text-xs font-semibold">
                    <span className="inline-flex items-center gap-1.5">
                      <Wrench className="size-3.5 text-muted" /> 装配的招投标业务工具（{tools.length}）
                    </span>
                    <a {...linkProps('/tools')} className="text-link hover:underline">查看全部工具 →</a>
                  </div>
                  <div className="flex flex-wrap gap-1.5 rounded-[12px] border border-line bg-soft/20 p-3">
                    {tools.map((t) => (
                      <span
                        key={t.name}
                        title={t.name}
                        className="inline-flex items-center gap-1 rounded-md border border-line bg-panel px-2 py-0.5 text-[11px]"
                      >
                        <Cpu className="size-3 text-muted" />
                        <span>{formatToolTitle(t.name)}</span>
                      </span>
                    ))}
                  </div>
                </div>
              </>
            )}

            {selectedAgent.key === 'goals' && (
              <div className="space-y-3 rounded-[12px] border border-line bg-soft/20 p-3.5 text-xs">
                <div className="font-semibold text-fg">初始化的四大图谱 Goal 根节点（exploration_nodes）</div>
                <div className="grid gap-2 sm:grid-cols-2">
                  {[
                    { code: 'G-VETO', title: '资格审查与否决项零废标防线', desc: '聚合资格/符合性审查表与 87 号令法定否决项，绑定资信核验要求。' },
                    { code: 'G-DEVIATION', title: '★实质性条款与技术偏离响应', desc: '聚合强制性条款与关键承诺基线，驱动零负偏离核查。' },
                    { code: 'G-SCORING', title: '评分标准拆解与高分策略应答', desc: '按评分表分档规则锚定技术与商务得分点，测算客观分与补证方向。' },
                    { code: 'G-OUTLINE', title: '技术标目录大纲与章节结构生成', desc: '按技术评分项与实质性需求自动生成 v1 draft 大纲并建立要求映射。' },
                  ].map((g) => (
                    <div key={g.code} className="rounded-lg border border-line bg-panel p-2.5">
                      <div className="flex items-center justify-between gap-1">
                        <span className="font-semibold text-fg">{g.title}</span>
                        <Badge tone="blue">{g.code}</Badge>
                      </div>
                      <p className="mt-1 text-[11px] leading-relaxed text-muted">{g.desc}</p>
                    </div>
                  ))}
                </div>
                <p className="text-[11px] text-muted">
                  触发方式：① 招标文件导入勾选「自动拆解招标目标」；② 要求页点击「从招标文件提取 / 提取并生成大纲草案」；③ Planner 调用 <code>decompose_rfp_goals</code> 工具。
                </p>
              </div>
            )}

            {selectedAgent.key === 'watchdog' && (
              <div className="space-y-2.5 rounded-[12px] border border-line bg-soft/20 p-3.5 text-xs">
                <div className="font-semibold text-fg">确定性门禁与跑偏熔断阈值（WatchdogHooks）</div>
                <ul className="list-disc space-y-1.5 pl-4 text-muted">
                  <li><strong className="text-fg">PreToolUse 硬件边界校验：</strong>拦截越界 <code>allowed_section_ids</code>、<code>allowed_requirement_ids</code>、字数区间 <code>target_words_min/max</code>、<code>forbidden_tools</code> 及超限只读工具调用。</li>
                  <li><strong className="text-fg">重复调用监控：</strong>同一工具+参数签名连续调用 <strong>3 次</strong>自动注入 Steer 纠偏指令；连续 <strong>5 次</strong>触发 <code>AbortKilledByWatchdog</code> 强行熔断。</li>
                  <li><strong className="text-fg">连续报错与空转监控：</strong>连续工具报错 <strong>4 次纠偏 / 7 次熔断</strong>；要求写回但连续只读调用 <strong>15 次纠偏 / 25 次熔断</strong>。</li>
                  <li><strong className="text-fg">Stop 完结门禁：</strong>当 <code>require_writeback=true</code> 且 Worker 未产生任何实质写回时，拒绝结束回合。</li>
                </ul>
              </div>
            )}

            {selectedAgent.key === 'judge' && (
              <div className="rounded-[12px] border border-line bg-soft/20 p-3.5 text-xs">
                <div className="flex items-center justify-between">
                  <span className="inline-flex items-center gap-1.5 font-semibold text-fg">
                    <ShieldAlert className="size-4 text-muted" /> AI 裁判提示词与受控工具范围
                  </span>
                  <a {...linkProps('/intercepts/rules')} className="text-link hover:underline">前往拦截规则配置 →</a>
                </div>
                <p className="mt-1.5 leading-relaxed text-muted">
                  当前受控工具共 {controlledTools.length} 个（{controlledTools.map((t) => formatToolTitle(t)).join('、') || '无'}）。可在「拦截规则」页面修改裁判提示词、超时降级动作及受控工具范围。
                </p>
              </div>
            )}
          </div>
        )}
      </Drawer>
    </PageFrame>
  )
}
