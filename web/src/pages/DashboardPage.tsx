import { useCallback } from 'react'
import {
  Activity, ArrowUpRight, BookOpen, Bot, Brain, ClipboardList,
  Plus, Radio, RefreshCw, ShieldCheck, Sparkles, Target, Wrench, Zap,
} from 'lucide-react'
import { api } from '../api/client'
import { PageFrame } from '../components/layout/AppShell'
import { Badge, Button, Card, Empty, ErrorBanner, IconButton, Spinner, statusTone } from '../components/ui'
import { useAsync, useInterval } from '../hooks/useAsync'
import { fmtTime, taskStatusLabel } from '../lib/format'
import { linkProps, navigate } from '../lib/router'
import { taskUrl } from '../lib/routes'
import { fmtTokens } from './task/TaskLLMTab'

export default function DashboardPage() {
  const tasks = useAsync(() => api.listTasks(), [])
  const pending = useAsync(() => api.listIntercepts(), [])
  const pool = useAsync(() => api.llmPool(), [])
  const settings = useAsync(() => api.llmSettings(), [])
  const records = useAsync(() => api.llmRecords({ limit: 6 }), [])
  const catalog = useAsync(() => api.systemCatalog(), [])
  const interceptCfg = useAsync(() => api.interceptConfig(), [])
  const creds = useAsync(() => api.listCredentials({ limit: 500 }), [])
  const kbDocs = useAsync(() => api.listKnowledge({ limit: 500 }), [])

  const reloadAll = useCallback(async () => {
    await Promise.all([
      tasks.reload(),
      pending.reload(),
      pool.reload(),
      settings.reload(),
      records.reload(),
      catalog.reload(),
      interceptCfg.reload(),
      creds.reload(),
      kbDocs.reload(),
    ])
  }, [tasks, pending, pool, settings, records, catalog, interceptCfg, creds, kbDocs])

  useInterval(() => {
    void tasks.reload()
    void pending.reload()
    void records.reload()
  }, 10000)

  const taskList = tasks.data ?? []
  const runningCount = taskList.filter((t) => t.status === 'running').length
  const pausedCount = taskList.filter((t) => t.status === 'paused').length
  const doneCount = taskList.filter((t) => t.status === 'done').length
  const failedCount = taskList.filter((t) => t.status === 'failed' || t.status === 'timeout').length

  const pendingList = pending.data ?? []
  const chain = pool.data?.chain ?? []
  const activeProfile = chain.find((m) => m.active)
  const trippedCount = chain.filter((m) => m.state === 'tripped').length
  const healthyCount = chain.filter((m) => !m.excluded && m.state !== 'tripped').length

  const recTotal = records.data?.total ?? 0
  const recItems = records.data?.items ?? []
  const recEnabled = !!settings.data?.record_bodies

  const judgeUsage = interceptCfg.data?.judge_usage
  const judgeTokens = (judgeUsage?.input_tokens ?? 0) + (judgeUsage?.output_tokens ?? 0)

  const credCount = creds.data?.length ?? 0
  const kbCount = kbDocs.data?.length ?? 0

  const agents = catalog.data?.agents ?? []
  const skills = catalog.data?.skills ?? []
  const tools = catalog.data?.tools ?? []

  return (
    <PageFrame
      title="总览"
      description="系统全局状态 · 实时刷新"
      actions={
        <>
          <IconButton label="刷新仪表盘" onClick={() => void reloadAll()}>
            <RefreshCw className="size-4" />
          </IconButton>
          <Button variant="primary" icon={<Plus className="size-4" />} onClick={() => void navigate('/tasks')}>
            新建 / 管理项目
          </Button>
        </>
      }
    >
      <ErrorBanner error={tasks.error || pool.error} onRetry={() => void reloadAll()} />

      {/* 第一行：5 张核心指标卡（对齐 artex-reference Dashboard 顶部 5 卡网格） */}
      <div className="grid grid-cols-1 gap-3.5 sm:grid-cols-2 lg:grid-cols-5">
        <a {...linkProps('/tasks')} className="rounded-[14px] border border-line bg-panel p-4 transition-colors hover:border-fg/30">
          <div className="flex items-center gap-1.5 text-xs text-muted">
            <Target className="size-3.5" />
            <span>活跃投标项目</span>
          </div>
          <div className="mt-2 flex items-baseline gap-1.5">
            <span className="text-2xl font-semibold tabular-nums">{runningCount}</span>
            <span className="text-sm text-muted">/ {taskList.length}</span>
          </div>
          <div className="mt-2 flex flex-wrap gap-2 text-[11px]">
            <span className="text-link">运行 {runningCount}</span>
            <span className="text-warn">暂停 {pausedCount}</span>
            <span className="text-ok">完成 {doneCount}</span>
            {failedCount > 0 && <span className="text-danger">异常 {failedCount}</span>}
          </div>
        </a>

        <a {...linkProps('/intercepts')} className="rounded-[14px] border border-line bg-panel p-4 transition-colors hover:border-fg/30">
          <div className="flex items-center gap-1.5 text-xs text-muted">
            <ClipboardList className="size-3.5" />
            <span>待审批工具调用</span>
          </div>
          <div className="mt-2 flex items-baseline gap-2">
            <span className="text-2xl font-semibold tabular-nums">{pendingList.length}</span>
            {pendingList.length > 0 && <Badge tone="amber">待人工裁决</Badge>}
          </div>
          <div className="mt-2 text-[11px] text-muted">
            受控工具 {interceptCfg.data?.enabled_tools?.length ?? 0} 个 · AI 裁判{interceptCfg.data?.judge?.enabled ? '已开启' : '未开启'}
          </div>
        </a>

        <a {...linkProps('/knowledge')} className="rounded-[14px] border border-line bg-panel p-4 transition-colors hover:border-fg/30">
          <div className="flex items-center gap-1.5 text-xs text-muted">
            <BookOpen className="size-3.5" />
            <span>企业资料与知识库</span>
          </div>
          <div className="mt-2 flex items-baseline gap-1.5">
            <span className="text-2xl font-semibold tabular-nums">{kbCount + credCount}</span>
            <span className="text-xs text-muted">项主数据</span>
          </div>
          <div className="mt-2 flex flex-wrap gap-2 text-[11px] text-muted">
            <span>方案知识库 {kbCount} 篇</span>
            <span>·</span>
            <span>资信档案 {credCount} 条</span>
          </div>
        </a>

        <a {...linkProps('/llm-records')} className="rounded-[14px] border border-line bg-panel p-4 transition-colors hover:border-fg/30">
          <div className="flex items-center gap-1.5 text-xs text-muted">
            <Radio className="size-3.5" />
            <span>LLM 调用录制</span>
          </div>
          <div className="mt-2 flex items-baseline gap-1.5">
            <span className="text-2xl font-semibold tabular-nums">{recTotal}</span>
            <span className="text-xs text-muted">条录制</span>
          </div>
          <div className="mt-2 flex items-center gap-1.5 text-[11px]">
            <span className={`size-2 rounded-full ${recEnabled ? 'animate-pulse bg-ok' : 'bg-slate-400'}`} />
            <span className="text-muted">{recEnabled ? '录制进行中' : '录制未开启'}</span>
          </div>
        </a>

        <a {...linkProps('/llm')} className="rounded-[14px] border border-line bg-panel p-4 transition-colors hover:border-fg/30">
          <div className="flex items-center gap-1.5 text-xs text-muted">
            <Zap className="size-3.5" />
            <span>模型池与裁判用量</span>
          </div>
          <div className="mt-2 flex items-baseline gap-1.5">
            <span className="text-2xl font-semibold tabular-nums">{healthyCount}</span>
            <span className="text-sm text-muted">/ {chain.length} 可用</span>
          </div>
          <div className="mt-2 truncate text-[11px] text-muted" title={activeProfile?.model}>
            {trippedCount > 0 ? <span className="text-danger">{trippedCount} 个熔断中</span> : activeProfile ? `默认：${activeProfile.name}` : `裁判累计 ${fmtTokens(judgeTokens)}`}
          </div>
        </a>
      </div>

      {/* 第二行：模型池与多智能体能力架构（对齐 artex-reference 中部大图卡片区） */}
      <div className="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card
          className="lg:col-span-2"
          title={
            <span className="inline-flex items-center gap-2">
              <Brain className="size-4 text-muted" />
              <span>LLM 模型配置池与运行状态</span>
            </span>
          }
          actions={
            <div className="flex items-center gap-2">
              <Badge tone={pool.data?.enabled ? 'green' : 'slate'}>
                {pool.data?.enabled ? '故障轮询已开启' : '单活默认模式'}
              </Badge>
              <a {...linkProps('/llm')} className="text-xs text-link hover:underline">管理模型 →</a>
            </div>
          }
        >
          {pool.loading && !pool.data ? (
            <Spinner />
          ) : chain.length === 0 ? (
            <Empty>尚未配置 LLM 模型，请前往「系统 → LLM」新建并激活模型配置。</Empty>
          ) : (
            <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2">
              {chain.map((m) => (
                <div
                  key={m.profile_id}
                  className="flex items-center justify-between gap-2 rounded-[10px] border border-line bg-soft/30 px-3 py-2.5 text-xs"
                >
                  <div className="min-w-0">
                    <div className="flex items-center gap-1.5 font-semibold text-fg">
                      <span className="truncate">{m.name}</span>
                      {m.active && <Badge tone="amber">默认</Badge>}
                    </div>
                    <div className="mt-0.5 truncate font-mono text-[11px] text-muted">{m.model}</div>
                  </div>
                  <Badge tone={m.state === 'ok' ? 'green' : m.state === 'tripped' ? 'red' : m.state === 'degraded' ? 'amber' : 'slate'}>
                    {m.state === 'ok' ? '正常' : m.state === 'tripped' ? '熔断中' : m.state === 'degraded' ? '有失败' : m.state === 'missing_key' ? '缺 Key' : m.state === 'disabled' ? '已停用' : '待调用'}
                  </Badge>
                </div>
              ))}
            </div>
          )}
        </Card>

        <Card
          title={
            <span className="inline-flex items-center gap-2">
              <ShieldCheck className="size-4 text-muted" />
              <span>Agent、Skill 与工具装配</span>
            </span>
          }
        >
          <div className="space-y-2.5 text-xs">
            <a {...linkProps('/agents')} className="flex items-center justify-between rounded-[10px] border border-line p-3 transition-colors hover:bg-soft/50">
              <div className="flex items-center gap-2.5">
                <Bot className="size-4 text-muted" />
                <div>
                  <div className="font-semibold text-fg">Agent 协同角色</div>
                  <div className="text-[11px] text-muted">Planner · Worker · LLM Judge</div>
                </div>
              </div>
              <Badge tone="blue">{agents.length} 个 Agent</Badge>
            </a>

            <a {...linkProps('/skills')} className="flex items-center justify-between rounded-[10px] border border-line p-3 transition-colors hover:bg-soft/50">
              <div className="flex items-center gap-2.5">
                <Sparkles className="size-4 text-muted" />
                <div>
                  <div className="font-semibold text-fg">招投标领域 Skill</div>
                  <div className="text-[11px] text-muted">资格审查 · 知识复用 · 章节写作 · 矩阵核验</div>
                </div>
              </div>
              <Badge tone="purple">{skills.length} 个 Skill</Badge>
            </a>

            <a {...linkProps('/tools')} className="flex items-center justify-between rounded-[10px] border border-line p-3 transition-colors hover:bg-soft/50">
              <div className="flex items-center gap-2.5">
                <Wrench className="size-4 text-muted" />
                <div>
                  <div className="font-semibold text-fg">招投标业务工具箱</div>
                  <div className="text-[11px] text-muted">常驻基础工具 + Skill 按需解锁</div>
                </div>
              </div>
              <Badge tone="green">{tools.length} 个工具</Badge>
            </a>
          </div>
        </Card>
      </div>

      {/* 第三行：左右双栏（近期投标项目 & 近期 LLM 报文 / 拦截动态，对齐 artex-reference 底部双栏） */}
      <div className="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Card
          title={
            <span className="inline-flex items-center gap-2">
              <Target className="size-4 text-muted" />
              <span>近期投标项目</span>
            </span>
          }
          actions={
            <a {...linkProps('/tasks')} className="inline-flex items-center gap-0.5 text-xs text-link hover:underline">
              全部项目 ({taskList.length}) <ArrowUpRight className="size-3.5" />
            </a>
          }
          bodyClassName="p-0"
        >
          {tasks.loading && !tasks.data ? (
            <Spinner />
          ) : taskList.length === 0 ? (
            <Empty>暂无投标项目，点击右上角「新建 / 管理项目」开始</Empty>
          ) : (
            <div className="divide-y divide-line">
              {taskList.slice(0, 6).map((t) => (
                <div key={t.id} className="flex flex-wrap items-center justify-between gap-2 px-4 py-3 hover:bg-soft/40">
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="rounded bg-soft px-1.5 py-0.5 font-mono text-[11px] text-muted">#{t.id}</span>
                      <a {...linkProps(taskUrl(t.id, 'overview'))} className="truncate text-sm font-semibold text-fg hover:text-link">
                        {t.name}
                      </a>
                      <Badge tone={statusTone(t.status)} dot>{taskStatusLabel[t.status] ?? t.status}</Badge>
                    </div>
                    {t.description && <p className="mt-0.5 truncate text-xs text-muted">{t.description}</p>}
                  </div>
                  <div className="flex items-center gap-2 text-xs">
                    <a {...linkProps(taskUrl(t.id, 'writing'))} className="text-muted hover:text-fg">写作</a>
                    <span className="text-line">·</span>
                    <a {...linkProps(taskUrl(t.id, 'matrix'))} className="text-muted hover:text-fg">核验</a>
                    <span className="text-line">·</span>
                    <a {...linkProps(taskUrl(t.id, 'overview'))} className="font-medium text-link hover:underline">进入 →</a>
                  </div>
                </div>
              ))}
            </div>
          )}
        </Card>

        <Card
          title={
            <span className="inline-flex items-center gap-2">
              <Activity className="size-4 text-muted" />
              <span>近期 LLM 报文录制与待审批动态</span>
            </span>
          }
          actions={
            <a {...linkProps('/llm-records')} className="inline-flex items-center gap-0.5 text-xs text-link hover:underline">
              查看录制 ({recTotal}) <ArrowUpRight className="size-3.5" />
            </a>
          }
          bodyClassName="p-0"
        >
          {pendingList.length > 0 && (
            <div className="border-b border-line bg-[#fffbeb] px-4 py-2.5 text-xs text-[#92400e]">
              <div className="flex items-center justify-between">
                <span>当前有 <strong>{pendingList.length}</strong> 项受控工具调用等待人工审批</span>
                <a {...linkProps('/intercepts')} className="font-semibold underline">立即处理 →</a>
              </div>
            </div>
          )}
          {records.loading && !records.data ? (
            <Spinner />
          ) : recItems.length === 0 ? (
            <Empty>
              暂无 LLM 原始报文录制记录（可在「LLM 录制」页面开启报文录制开关）
            </Empty>
          ) : (
            <div className="divide-y divide-line">
              {recItems.map((r) => (
                <div key={r.id} className="flex items-center justify-between gap-2 px-4 py-2.5 text-xs hover:bg-soft/40">
                  <div className="flex min-w-0 items-center gap-2">
                    <Badge tone={r.status_code >= 200 && r.status_code < 300 ? 'green' : 'red'}>
                      {r.status_code || 'ERR'}
                    </Badge>
                    <span className="truncate font-medium text-fg">{r.profile_name || `#${r.profile_id}`}</span>
                    {r.task_id > 0 && <span className="font-mono text-[11px] text-muted">项目#{r.task_id}</span>}
                    {r.worker && <span className="font-mono text-[11px] text-muted">{r.worker}</span>}
                  </div>
                  <div className="flex shrink-0 items-center gap-2 text-[11px] text-muted tabular-nums">
                    <span>{r.latency_ms}ms</span>
                    <span>{fmtTime(r.created_at)}</span>
                  </div>
                </div>
              ))}
            </div>
          )}
        </Card>
      </div>
    </PageFrame>
  )
}
