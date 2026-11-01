import { useEffect, useState } from 'react'
import { Bot, ListFilter, Pencil, Plus, RefreshCw, RotateCcw, Save, Trash2 } from 'lucide-react'
import { api, errorText } from '../api/client'
import type { InterceptAction, InterceptRule, InterceptRuleInput, JudgeConfig, JudgeDayUsage } from '../api/types'
import { ApprovalRecordsTable, InterceptList } from '../components/Intercepts'
import { PageFrame, useShell } from '../components/layout/AppShell'
import { Badge, Button, Card, Empty, ErrorBanner, Field, Input, Modal, Select, Spinner, Stat, Tabs, TextArea, cx, useConfirm, useToast } from '../components/ui'
import { useAsync, useInterval } from '../hooks/useAsync'
import { fmtNum, formatToolTitle } from '../lib/format'
import { fmtTokens } from './task/TaskLLMTab'

const TOOL_GROUPS: { label: string; tools: { key: string; desc: string }[] }[] = [
  {
    label: '系统执行与外部请求类（默认受控）',
    tools: [
      { key: 'Bash', desc: '在受控环境中执行外部命令' },
      { key: 'WebFetch', desc: '发起外部 HTTP/HTTPS 请求' },
      { key: 'web_search', desc: '外部网络检索' },
      { key: 'session_open', desc: '开启持久交互会话' },
      { key: 'session_send', desc: '向交互会话发送指令' },
    ],
  },
  {
    label: '文件写入与编辑类（默认受控）',
    tools: [
      { key: 'Write', desc: '写入新文件或覆盖产物' },
      { key: 'Edit', desc: '精确替换修改文件' },
      { key: 'MultiEdit', desc: '批量替换修改文件' },
    ],
  },
  {
    label: '招标文件与资信商务核验工具',
    tools: [
      { key: 'verify_commercial_bid', desc: '商务报价合价、最高限价与利润率验算（默认受控）' },
      { key: 'query_company_credentials', desc: '对照招标要求核验企业资质、项目业绩与人员证书' },
      { key: 'simulate_scoring_table', desc: '模拟测算技术与商务评标得分及失分点' },
      { key: 'document_outline', desc: '列出招标文件章节目录大纲与强制条款标记' },
      { key: 'search_document', desc: '按关键词或正则全文检索招标文件切片' },
      { key: 'read_chunk', desc: '读取招标文件切片完整正文与结构化表格' },
    ],
  },
  {
    label: '知识库检索、技术标写作与合规审查工具',
    tools: [
      { key: 'kb_search_proposals', desc: '检索企业方案知识库中的历史标书与标准技术方案' },
      { key: 'kb_search_products', desc: '检索产品规格书与技术白皮书参数指标' },
      { key: 'kb_read_chunk', desc: '读取知识库文档切片正文与参数表格' },
      { key: 'section_read_context', desc: '读取技术标章节写作上下文与绑定证据状态' },
      { key: 'section_write_draft', desc: '提交技术标章节修订草稿（带大纲与版本双门禁）' },
      { key: 'chart_generate', desc: '生成技术标架构图、实施甘特图、组织图或设备选型表' },
      { key: 'compliance_verify', desc: '逐条核验技术标正文对招标要求的实质性响应' },
      { key: 'fulltext_consistency_check', desc: '横向比对全文章节定量承诺矛盾与 ★/▲ 漏项' },
    ],
  },
]

function ActionBadge({ action }: { action: InterceptAction }) {
  if (action === 'allow') return <Badge tone="green">允许放行</Badge>
  if (action === 'deny') return <Badge tone="red">禁止阻断</Badge>
  return <Badge tone="amber">转人工审批</Badge>
}

function JudgeSparkbars({ daily }: { daily: JudgeDayUsage[] }) {
  const max = Math.max(1, ...daily.map((d) => d.input_tokens + d.output_tokens))
  return (
    <div className="flex h-16 items-end gap-1 rounded-lg border border-line bg-soft/40 p-2">
      {daily.map((d) => {
        const total = d.input_tokens + d.output_tokens
        const h = Math.max(6, Math.round((total / max) * 100))
        return (
          <div
            key={d.date}
            title={`${d.date} · ${d.calls} 次调用 · ${fmtTokens(total)} tokens`}
            className="min-w-[4px] flex-1 rounded-sm bg-fg/70 transition-colors hover:bg-fg"
            style={{ height: `${h}%` }}
          />
        )
      })}
    </div>
  )
}

const defaultRuleForm = (): InterceptRuleInput => ({
  name: '',
  tool_filter: '',
  match_target: 'tool_name',
  match_type: 'string',
  pattern: '',
  action: 'ask',
  message: '',
  priority: 10,
  enabled: true,
  timeout_enabled: true,
  timeout_seconds: 300,
  timeout_action: 'deny',
})

export default function InterceptsPage({ mode = 'approvals' }: { mode?: 'approvals' | 'rules' }) {
  const shell = useShell()
  const toast = useToast()
  const confirm = useConfirm()
  const [tab, setTab] = useState<'approvals' | 'rules' | 'judge'>(mode === 'rules' ? 'rules' : 'approvals')

  useEffect(() => {
    setTab(mode === 'rules' ? 'rules' : 'approvals')
  }, [mode])

  // 1. 待审批轮询
  const pending = useAsync(() => api.listIntercepts(), [])
  useInterval(() => void pending.reload(), 5000)

  // 2. 拦截规则与控制面配置
  const rules = useAsync(() => api.interceptRules(), [])
  const cfg = useAsync(() => api.interceptConfig(), [])
  const profiles = useAsync(() => api.listProfiles(), [])

  // 规则弹窗状态
  const [ruleModal, setRuleModal] = useState(false)
  const [editingRule, setEditingRule] = useState<InterceptRule | null>(null)
  const [form, setForm] = useState<InterceptRuleInput>(defaultRuleForm)
  const [regexWarn, setRegexWarn] = useState(false)
  const [savingRule, setSavingRule] = useState(false)

  // 受控工具范围弹窗状态
  const [scopeModal, setScopeModal] = useState(false)
  const [selectedTools, setSelectedTools] = useState<string[]>([])
  const [savingScope, setSavingScope] = useState(false)

  // AI 裁判表单状态
  const [judgeDraft, setJudgeDraft] = useState<JudgeConfig>({
    enabled: false,
    profile_id: 0,
    prompt: '',
    timeout_seconds: 15,
    fail_action: 'allow',
    ask_timeout_seconds: 300,
    ask_timeout_action: 'deny',
  })
  const [savingJudge, setSavingJudge] = useState(false)

  useEffect(() => {
    if (cfg.data?.judge) setJudgeDraft(cfg.data.judge)
    if (cfg.data?.enabled_tools) setSelectedTools(cfg.data.enabled_tools)
  }, [cfg.data])

  useEffect(() => {
    if (form.match_type !== 'regex' || !form.pattern) {
      setRegexWarn(false)
      return
    }
    try {
      new RegExp(form.pattern)
      setRegexWarn(false)
    } catch {
      setRegexWarn(true)
    }
  }, [form.match_type, form.pattern])

  const openNewRule = () => {
    setEditingRule(null)
    setForm(defaultRuleForm())
    setRuleModal(true)
  }

  const openEditRule = (r: InterceptRule) => {
    setEditingRule(r)
    setForm({
      name: r.name,
      tool_filter: r.tool_filter ?? '',
      match_target: r.match_target,
      match_type: r.match_type,
      pattern: r.pattern,
      action: r.action,
      message: r.message ?? '',
      priority: r.priority,
      enabled: r.enabled,
      timeout_enabled: r.timeout_enabled,
      timeout_seconds: r.timeout_seconds || 300,
      timeout_action: r.timeout_action || 'deny',
    })
    setRuleModal(true)
  }

  const saveRule = async () => {
    if (!form.name.trim() || !form.pattern.trim()) {
      toast('error', '请填写规则名称与匹配模式')
      return
    }
    setSavingRule(true)
    try {
      if (editingRule) {
        await api.updateInterceptRule(editingRule.id, form)
        toast('ok', `已更新拦截规则「${form.name}」`)
      } else {
        await api.createInterceptRule(form)
        toast('ok', `已创建拦截规则「${form.name}」`)
      }
      setRuleModal(false)
      void rules.reload()
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setSavingRule(false)
    }
  }

  const toggleRule = async (r: InterceptRule) => {
    try {
      await api.toggleInterceptRule(r.id, !r.enabled)
      void rules.reload()
    } catch (e) {
      toast('error', errorText(e))
    }
  }

  const removeRule = async (r: InterceptRule) => {
    const ok = await confirm({
      title: `删除拦截规则「${r.name}」？`,
      message: '删除后下一次工具调用将立即按更新后的规则表匹配。',
      confirmLabel: '删除规则',
      danger: true,
    })
    if (!ok) return
    try {
      await api.deleteInterceptRule(r.id)
      toast('ok', '规则已删除')
      void rules.reload()
    } catch (e) {
      toast('error', errorText(e))
    }
  }

  const saveScope = async (reset = false) => {
    setSavingScope(true)
    try {
      const updated = await api.saveInterceptConfig(reset ? { reset_tools: true } : { enabled_tools: selectedTools })
      cfg.setData(updated)
      setScopeModal(false)
      toast('ok', reset ? '已恢复默认受控工具范围' : '受控工具范围已保存')
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setSavingScope(false)
    }
  }

  const saveJudge = async (restorePrompt = false) => {
    setSavingJudge(true)
    try {
      const nextJudge = restorePrompt ? { ...judgeDraft, prompt: '' } : judgeDraft
      const updated = await api.saveInterceptConfig({ judge: nextJudge })
      cfg.setData(updated)
      setJudgeDraft(updated.judge)
      toast('ok', restorePrompt ? '已恢复内置默认裁判提示词' : 'AI 裁判配置已保存')
    } catch (e) {
      toast('error', errorText(e))
    } finally {
      setSavingJudge(false)
    }
  }

  const scopeTools = cfg.data?.enabled_tools ?? []
  const usage = cfg.data?.judge_usage

  const isRulesMode = mode === 'rules'

  return (
    <PageFrame
      title={isRulesMode ? '拦截规则' : '审批记录'}
      description={
        isRulesMode
          ? '统一管理受控工具范围、静态/正则拦截规则与 LLM 兜底安全裁判（Judge）。'
          : '处理在途受控工具调用人工审批与历史审批审计记录。大纲与章节的业务审批在各项目的「大纲与写作」中进行。'
      }
    >
      <div className="space-y-4">
        {/* 顶栏受控工具范围提示条 */}
        <div
          className={cx(
            'flex flex-wrap items-center justify-between gap-3 rounded-[12px] border px-4 py-3',
            scopeTools.length === 0 ? 'border-amber-300 bg-amber-50/50' : 'border-line bg-panel',
          )}
        >
          <div className="flex min-w-0 flex-1 items-center gap-2 text-sm">
            <ListFilter className="size-4 shrink-0 text-muted" />
            <span className="shrink-0 font-semibold">拦截受控范围</span>
            {scopeTools.length === 0 ? (
              <span className="text-xs text-warn">未启用任何受控工具 — 拦截规则与 AI 裁判均不会介入</span>
            ) : (
              <>
                <Badge tone="blue">{scopeTools.length} 个工具</Badge>
                <span className="truncate text-xs text-muted" title={scopeTools.map((k) => `${formatToolTitle(k)} (${k})`).join('、')}>
                  {scopeTools.map((k) => formatToolTitle(k)).join('、')}
                </span>
              </>
            )}
          </div>
          <Button
            size="sm"
            icon={<ListFilter className="size-3.5" />}
            onClick={() => {
              setSelectedTools(cfg.data?.enabled_tools ?? [])
              setScopeModal(true)
            }}
          >
            调整受控范围
          </Button>
        </div>

        {isRulesMode ? (
          <Tabs
            value={tab === 'approvals' ? 'rules' : tab}
            onChange={(k) => setTab(k as 'rules' | 'judge')}
            tabs={[
              { key: 'rules', label: `拦截规则管理${rules.data?.length ? `（${rules.data.length}）` : ''}` },
              { key: 'judge', label: `AI 裁判兜底${judgeDraft.enabled ? '（已启用）' : ''}` },
            ]}
          />
        ) : (
          <Tabs
            value={tab}
            onChange={(k) => setTab(k as 'approvals' | 'rules' | 'judge')}
            tabs={[
              { key: 'approvals', label: `待审批与审批记录${pending.data?.length ? `（${pending.data.length}）` : ''}` },
              { key: 'rules', label: `拦截规则管理${rules.data?.length ? `（${rules.data.length}）` : ''}` },
              { key: 'judge', label: `AI 裁判兜底${judgeDraft.enabled ? '（已启用）' : ''}` },
            ]}
          />
        )}

        {tab === 'approvals' && (
          <div className="space-y-4">
            <InterceptList
              rows={pending.data}
              loading={pending.loading}
              error={pending.error}
              onReload={pending.reload}
              showTask
              onDecided={() => {
                void pending.reload()
                shell.refreshPending()
              }}
            />
            <ApprovalRecordsTable
              onDecided={() => {
                void pending.reload()
                shell.refreshPending()
              }}
            />
          </div>
        )}

        {tab === 'rules' && (
          <Card
            title="工具拦截规则（按优先级从高到低匹配，首条命中生效）"
            actions={
              <div className="flex items-center gap-2">
                <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => rules.reload()}>
                  刷新
                </Button>
                <Button size="sm" variant="primary" icon={<Plus className="size-3.5" />} onClick={openNewRule}>
                  新建规则
                </Button>
              </div>
            }
            bodyClassName="p-0"
          >
            <div className="p-3 empty:hidden"><ErrorBanner error={rules.error} onRetry={rules.reload} /></div>
            {rules.loading && !rules.data ? (
              <Spinner />
            ) : (rules.data ?? []).length === 0 ? (
              <Empty>
                <div className="space-y-2">
                  <div>暂无自定义拦截规则。未命中规则的受控工具将按默认策略或 AI 裁判处理。</div>
                  <Button size="sm" icon={<Plus className="size-3.5" />} onClick={openNewRule}>新建第一条规则</Button>
                </div>
              </Empty>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead className="bg-soft text-left text-xs text-muted">
                    <tr>
                      <th className="w-20 px-3 py-2">优先级</th>
                      <th className="px-3 py-2">规则名称</th>
                      <th className="px-3 py-2">限定工具</th>
                      <th className="px-3 py-2">匹配目标 / 类型</th>
                      <th className="px-3 py-2">匹配模式</th>
                      <th className="px-3 py-2">策略动作</th>
                      <th className="px-3 py-2 text-center">启用</th>
                      <th className="w-24 px-3 py-2 text-right">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rules.data!.map((r) => (
                      <tr key={r.id} className={cx('border-t border-line-soft', !r.enabled && 'opacity-50')}>
                        <td className="px-3 py-2 font-mono text-xs tabular-nums">{r.priority}</td>
                        <td className="px-3 py-2 font-medium">
                          <div>{r.name}</div>
                          {r.message && <div className="text-xs text-muted">{r.message}</div>}
                        </td>
                        <td className="px-3 py-2 font-mono text-xs text-muted">{r.tool_filter || '全部受控工具'}</td>
                        <td className="px-3 py-2 text-xs text-muted">
                          {r.match_target === 'tool_name' ? '工具名' : '入参 JSON'} · {r.match_type === 'regex' ? '正则' : '字符串包含'}
                        </td>
                        <td className="max-w-xs px-3 py-2">
                          <code className="block truncate rounded bg-soft px-1.5 py-0.5 font-mono text-xs">{r.pattern}</code>
                        </td>
                        <td className="px-3 py-2">
                          <ActionBadge action={r.action} />
                          {r.action === 'ask' && r.timeout_enabled && (
                            <span className="ml-1.5 text-[11px] text-muted">{r.timeout_seconds}s 后{r.timeout_action === 'allow' ? '放行' : '拒绝'}</span>
                          )}
                        </td>
                        <td className="px-3 py-2 text-center">
                          <input
                            type="checkbox"
                            aria-label={`启用规则 ${r.name}`}
                            checked={r.enabled}
                            onChange={() => void toggleRule(r)}
                            className="size-4 cursor-pointer accent-fg"
                          />
                        </td>
                        <td className="px-3 py-2 text-right">
                          <div className="inline-flex items-center gap-1">
                            <Button size="sm" variant="ghost" aria-label={`编辑规则 ${r.name}`} onClick={() => openEditRule(r)}>
                              <Pencil className="size-3.5" />
                            </Button>
                            <Button size="sm" variant="ghost" aria-label={`删除规则 ${r.name}`} onClick={() => void removeRule(r)}>
                              <Trash2 className="size-3.5 text-danger" />
                            </Button>
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Card>
        )}

        {tab === 'judge' && (
          <div className="space-y-4">
            <div
              className={cx(
                'flex flex-wrap items-center justify-between gap-3 rounded-[12px] border px-4 py-3',
                judgeDraft.enabled ? 'border-purple-300 bg-purple-50/40' : 'border-line bg-panel',
              )}
            >
              <div className="flex items-center gap-2.5">
                <Bot className={cx('size-5 shrink-0', judgeDraft.enabled ? 'text-purple-700' : 'text-muted')} />
                <div>
                  <div className="text-sm font-semibold">AI 裁判兜底审批（LLM Judge）</div>
                  <p className="text-xs text-muted">
                    在受控工具范围内、且没有任何静态规则命中的工具调用，交由裁判模型做语义判定（allow 放行 / ask 转人工审批 / deny 阻断）。
                  </p>
                </div>
              </div>
              <label className="inline-flex cursor-pointer items-center gap-2 text-sm font-medium">
                <input
                  type="checkbox"
                  checked={judgeDraft.enabled}
                  onChange={(e) => setJudgeDraft({ ...judgeDraft, enabled: e.target.checked })}
                  className="size-4 accent-fg"
                />
                {judgeDraft.enabled ? '已启用' : '未启用'}
              </label>
            </div>

            {usage && (
              <Card
                title="AI 裁判 Token 消耗统计（worker=judge 独立计量）"
                actions={
                  <Button size="sm" variant="ghost" icon={<RefreshCw className="size-3.5" />} onClick={() => cfg.reload()}>
                    刷新
                  </Button>
                }
              >
                <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
                  <Stat label="裁判调用次数" value={fmtNum(usage.calls)} />
                  <Stat label="输入 Token" value={fmtTokens(usage.input_tokens)} />
                  <Stat label="输出 Token" value={fmtTokens(usage.output_tokens)} />
                  <Stat label="缓存读 Token" value={fmtTokens(usage.cache_read_tokens)} />
                  <Stat label="缓存写 Token" value={fmtTokens(usage.cache_write_tokens)} />
                </div>
                {usage.daily && usage.daily.length > 0 && (
                  <div className="mt-4">
                    <div className="mb-1.5 text-xs font-medium text-muted">近 30 天每日裁判 Token 消耗（输入 + 输出）</div>
                    <JudgeSparkbars daily={usage.daily} />
                  </div>
                )}
              </Card>
            )}

            <div className="grid gap-4 lg:grid-cols-5">
              <Card
                className="lg:col-span-3"
                title="裁判系统提示词（System Prompt）"
                actions={
                  <Button size="sm" variant="ghost" icon={<RotateCcw className="size-3.5" />} loading={savingJudge} onClick={() => void saveJudge(true)}>
                    恢复内置模板
                  </Button>
                }
              >
                <TextArea
                  rows={12}
                  value={judgeDraft.prompt}
                  onChange={(e) => setJudgeDraft({ ...judgeDraft, prompt: e.target.value })}
                  placeholder="留空保存时自动恢复内置默认模板"
                  className="font-mono text-xs leading-relaxed"
                />
                <div className="mt-1.5 flex items-center justify-between text-xs text-muted">
                  <span>要求模型输出严格 JSON：<code>{`{"action":"allow|deny|ask","reason":"一句话理由"}`}</code></span>
                  <span>{judgeDraft.prompt.length} 字符</span>
                </div>
              </Card>

              <Card
                className="lg:col-span-2"
                title="裁判模型与超时回退策略"
                actions={
                  <Button size="sm" variant="primary" icon={<Save className="size-3.5" />} loading={savingJudge} onClick={() => void saveJudge(false)}>
                    保存配置
                  </Button>
                }
              >
                <div className="space-y-4">
                  <Field label="指定裁判模型配置" hint="选择“跟随角色绑定/全局池”时，优先取 judge 角色绑定，未绑定时取全局模型池。">
                    <Select
                      value={judgeDraft.profile_id || 0}
                      onChange={(e) => setJudgeDraft({ ...judgeDraft, profile_id: Number(e.target.value) })}
                    >
                      <option value={0}>跟随 judge 角色绑定 / 全局池</option>
                      {(profiles.data ?? []).map((p) => (
                        <option key={p.id} value={p.id}>
                          {p.name}（{p.model}）{p.enabled ? '' : ' [已停用]'}
                        </option>
                      ))}
                    </Select>
                  </Field>
                  <Field label="模型判定超时（秒）">
                    <Input
                      type="number"
                      min={1}
                      value={judgeDraft.timeout_seconds}
                      onChange={(e) => setJudgeDraft({ ...judgeDraft, timeout_seconds: Math.max(1, Number(e.target.value) || 15) })}
                    />
                  </Field>
                  <Field label="模型调用失败 / 超时 / 无法解析时的回退动作">
                    <Select
                      value={judgeDraft.fail_action}
                      onChange={(e) => setJudgeDraft({ ...judgeDraft, fail_action: e.target.value as InterceptAction })}
                    >
                      <option value="allow">放行（allow）</option>
                      <option value="ask">转人工审批（ask）</option>
                      <option value="deny">直接阻断（deny）</option>
                    </Select>
                  </Field>
                  <div className="border-t border-line pt-3">
                    <div className="mb-2 text-xs font-semibold text-muted">当模型判定为「转人工审批（ask）」时</div>
                    <div className="grid grid-cols-2 gap-3">
                      <Field label="人工审批等待超时（秒）">
                        <Input
                          type="number"
                          min={5}
                          value={judgeDraft.ask_timeout_seconds}
                          onChange={(e) => setJudgeDraft({ ...judgeDraft, ask_timeout_seconds: Math.max(5, Number(e.target.value) || 300) })}
                        />
                      </Field>
                      <Field label="人工审批超时动作">
                        <Select
                          value={judgeDraft.ask_timeout_action}
                          onChange={(e) => setJudgeDraft({ ...judgeDraft, ask_timeout_action: e.target.value as 'allow' | 'deny' })}
                        >
                          <option value="deny">自动拒绝（deny）</option>
                          <option value="allow">自动允许（allow）</option>
                        </Select>
                      </Field>
                    </div>
                  </div>
                </div>
              </Card>
            </div>
          </div>
        )}
      </div>

      {/* 新建/编辑拦截规则弹窗 */}
      <Modal
        open={ruleModal}
        wide
        title={editingRule ? `编辑拦截规则 #${editingRule.id}` : '新建拦截规则'}
        onClose={() => setRuleModal(false)}
        footer={
          <>
            <Button onClick={() => setRuleModal(false)}>取消</Button>
            <Button variant="primary" loading={savingRule} onClick={saveRule}>保存规则</Button>
          </>
        }
      >
        <div className="grid gap-4 md:grid-cols-2">
          <Field label="规则名称 *">
            <Input
              autoFocus
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              placeholder="例如：商务报价验算前需人工复核"
            />
          </Field>
          <Field label="优先级（数字越大越先匹配）">
            <Input
              type="number"
              value={form.priority}
              onChange={(e) => setForm({ ...form, priority: Number(e.target.value) || 0 })}
            />
          </Field>
          <Field label="限定工具名（可选，留空匹配所有受控工具）">
            <Input
              value={form.tool_filter}
              onChange={(e) => setForm({ ...form, tool_filter: e.target.value })}
              placeholder="例如：verify_commercial_bid 或 Bash"
            />
          </Field>
          <Field label="拦截动作 *">
            <Select value={form.action} onChange={(e) => setForm({ ...form, action: e.target.value as InterceptAction })}>
              <option value="ask">转人工审批（ask — 挂起等待裁决）</option>
              <option value="deny">禁止阻断（deny — 直接拒绝并返回原因）</option>
              <option value="allow">允许放行（allow — 直接通过，跳过后续规则与裁判）</option>
            </Select>
          </Field>
          <Field label="匹配目标">
            <Select value={form.match_target} onChange={(e) => setForm({ ...form, match_target: e.target.value as 'tool_name' | 'tool_input' })}>
              <option value="tool_name">工具名称（tool_name）</option>
              <option value="tool_input">工具入参 JSON（tool_input）</option>
            </Select>
          </Field>
          <Field label="匹配方式">
            <Select value={form.match_type} onChange={(e) => setForm({ ...form, match_type: e.target.value as 'string' | 'regex' })}>
              <option value="string">子串包含（不区分大小写）</option>
              <option value="regex">Go RE2 正则表达式</option>
            </Select>
          </Field>
          <Field label="匹配模式（Pattern）*" className="md:col-span-2">
            <Input
              value={form.pattern}
              onChange={(e) => setForm({ ...form, pattern: e.target.value })}
              placeholder={form.match_type === 'regex' ? '^verify_commercial_bid$' : 'verify_commercial_bid'}
              className="font-mono"
            />
            {regexWarn && (
              <p className="mt-1 text-xs text-warn">提示：该表达式包含浏览器不支持的 Go RE2 语法（如 (?i)），保存时由后端校验。</p>
            )}
          </Field>
          {form.action !== 'allow' && (
            <Field label={form.action === 'deny' ? '拒绝提示消息（返回给 Agent）' : '人工审批提示说明'} className="md:col-span-2">
              <TextArea
                rows={2}
                value={form.message}
                onChange={(e) => setForm({ ...form, message: e.target.value })}
                placeholder={form.action === 'deny' ? '该操作已被安全规则禁止' : '请核对工具调用入参是否符合本项目招标范围'}
              />
            </Field>
          )}
          {form.action === 'ask' && (
            <div className="grid gap-3 rounded-lg border border-line bg-soft/40 p-3 md:col-span-2 md:grid-cols-3">
              <label className="flex items-center gap-2 text-sm font-medium">
                <input
                  type="checkbox"
                  checked={form.timeout_enabled}
                  onChange={(e) => setForm({ ...form, timeout_enabled: e.target.checked })}
                  className="size-4 accent-fg"
                />
                启用审批等待超时
              </label>
              <Field label="超时秒数">
                <Input
                  type="number"
                  min={5}
                  disabled={!form.timeout_enabled}
                  value={form.timeout_seconds}
                  onChange={(e) => setForm({ ...form, timeout_seconds: Math.max(5, Number(e.target.value) || 60) })}
                />
              </Field>
              <Field label="超时处置动作">
                <Select
                  disabled={!form.timeout_enabled}
                  value={form.timeout_action}
                  onChange={(e) => setForm({ ...form, timeout_action: e.target.value as 'allow' | 'deny' })}
                >
                  <option value="deny">自动拒绝（deny）</option>
                  <option value="allow">自动放行（allow）</option>
                </Select>
              </Field>
            </div>
          )}
          <label className="flex items-center gap-2 text-sm font-medium md:col-span-2">
            <input
              type="checkbox"
              checked={form.enabled}
              onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
              className="size-4 accent-fg"
            />
            立即启用此规则
          </label>
        </div>
      </Modal>

      {/* 受控工具范围选择弹窗 */}
      <Modal
        open={scopeModal}
        wide
        title="调整拦截受控工具范围"
        onClose={() => setScopeModal(false)}
        footer={
          <>
            <Button onClick={() => void saveScope(true)} loading={savingScope}>恢复系统默认集合</Button>
            <span className="flex-1" />
            <Button onClick={() => setScopeModal(false)}>取消</Button>
            <Button variant="primary" onClick={() => void saveScope(false)} loading={savingScope}>保存范围</Button>
          </>
        }
      >
        <div className="space-y-4">
          <p className="text-xs text-muted">
            只有勾选的工具才会进入静态拦截规则与 AI 裁判链；未勾选的工具调用将直接放行。
          </p>
          {TOOL_GROUPS.map((g) => (
            <div key={g.label} className="rounded-lg border border-line p-3">
              <div className="mb-2 text-xs font-semibold text-muted">{g.label}</div>
              <div className="grid gap-2 sm:grid-cols-2">
                {g.tools.map((t) => {
                  const checked = selectedTools.includes(t.key)
                  return (
                    <label
                      key={t.key}
                      className={cx(
                        'flex cursor-pointer items-start gap-2.5 rounded-lg border p-2 text-xs transition-colors',
                        checked ? 'border-fg bg-soft/60' : 'border-line-soft hover:bg-soft/30',
                      )}
                    >
                      <input
                        type="checkbox"
                        checked={checked}
                        onChange={(e) =>
                          setSelectedTools(
                            e.target.checked ? [...selectedTools, t.key] : selectedTools.filter((k) => k !== t.key),
                          )
                        }
                        className="mt-0.5 size-4 accent-fg"
                      />
                      <div className="min-w-0 flex-1">
                        <div className="font-semibold text-fg">
                          {formatToolTitle(t.key)}
                          <code className="ml-1.5 font-mono text-[11px] font-normal text-muted">{t.key}</code>
                        </div>
                        <div className="mt-0.5 text-[11px] text-muted">{t.desc}</div>
                      </div>
                    </label>
                  )
                })}
              </div>
            </div>
          ))}
        </div>
      </Modal>
    </PageFrame>
  )
}
