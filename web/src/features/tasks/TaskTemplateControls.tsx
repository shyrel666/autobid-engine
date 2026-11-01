import { useEffect, useState } from 'react'
import { Library, Plus, Save, Settings2, Trash2 } from 'lucide-react'
import { Badge, Button, Field, Input, Modal, Select, TextArea, cx, useToast } from '../../components/ui'

export interface BidTaskTemplate {
  id: string
  name: string
  description: string
  goal: string
  goals: string
  timeout_seconds: number
  llm_profile_ids: number[]
  builtin?: boolean
}

const STORAGE_KEY = 'autobid.task_templates.v1'

const BUILTIN_TEMPLATES: BidTaskTemplate[] = [
  {
    id: 'builtin-full-bid',
    name: '标准公开招标全流程（审查 + 技术标编制）',
    description: '适用于政府采购与国企公开招标项目，覆盖资格符合性核验、评分项对齐与技术标章节撰写。',
    goal: '完成资格审查、符合性审查、实质性 ★ 条款核验与评分项响应策略对齐，并生成高分技术标方案',
    goals: '识别并逐条核验全部 ★ 实质性否决条款，确保零废标风险\n拆解全部商务与技术评分项（满分100分），建立章节映射与取分策略\n完成技术标各章节初稿撰写并通过双层合规矩阵与全文一致性校验',
    timeout_seconds: 0,
    llm_profile_ids: [],
    builtin: true,
  },
  {
    id: 'builtin-it-integration',
    name: '政企信息化与软件开发集成投标',
    description: '聚焦架构设计、信创适配、性能指标正偏离证明、项目实施与售后运维方案。',
    goal: '完成信息化招标需求参数核验，匹配企业历史业绩与白皮书证据，输出架构图与技术方案',
    goals: '核查全部 ★ 与 ▲ 关键技术参数，关联合规检测报告与产品白皮书证据\n设计系统总体架构图、实施甘特图与项目组织架构图\n校验工期、质保期、SLA 响应时长及人员资质证书有效性',
    timeout_seconds: 0,
    llm_profile_ids: [],
    builtin: true,
  },
  {
    id: 'builtin-fast-audit',
    name: '招标文件风险快审与废标雷区排查',
    description: '快速提取招标文件中的资格门槛、特定业绩要求、商务苛刻条款与评分倾向性。',
    goal: '快速排查招标文件中的所有资格门槛、★ 否决项与评分细则风险点，输出投标可行性评估结论',
    goals: '提取全部资格性审查与符合性审查否决项并与企业资信库比对\n标注存在围标倾向、苛刻参数或缺失证明材料的风险项',
    timeout_seconds: 1800,
    llm_profile_ids: [],
    builtin: true,
  },
]

function loadUserTemplates(): BidTaskTemplate[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
}

function saveUserTemplates(list: BidTaskTemplate[]) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(list.filter((t) => !t.builtin)))
  } catch {
    /* ignore quota errors */
  }
}

export interface TaskTemplateSeed {
  description: string
  goal: string
  goals: string
  timeout_seconds: number
  llm_profile_ids: number[]
}

export function TaskTemplateControls({
  seed,
  onApply,
}: {
  seed: TaskTemplateSeed
  onApply: (tpl: BidTaskTemplate) => void
}) {
  const toast = useToast()
  const [userTemplates, setUserTemplates] = useState<BidTaskTemplate[]>(() => loadUserTemplates())
  const [selectedID, setSelectedID] = useState('')
  const [managerOpen, setManagerOpen] = useState(false)
  const [editID, setEditID] = useState<string | null>(null)
  const [draft, setDraft] = useState({ name: '', description: '', goal: '', goals: '', timeout: '0' })

  const allTemplates = [...BUILTIN_TEMPLATES, ...userTemplates]

  useEffect(() => {
    saveUserTemplates(userTemplates)
  }, [userTemplates])

  const openSaveCurrent = () => {
    setEditID(null)
    setDraft({
      name: '',
      description: seed.description,
      goal: seed.goal,
      goals: seed.goals,
      timeout: String(seed.timeout_seconds || 0),
    })
    setManagerOpen(true)
  }

  const openManage = () => {
    const firstUser = userTemplates[0]
    if (firstUser) {
      setEditID(firstUser.id)
      setDraft({
        name: firstUser.name,
        description: firstUser.description,
        goal: firstUser.goal,
        goals: firstUser.goals,
        timeout: String(firstUser.timeout_seconds || 0),
      })
    } else {
      setEditID(null)
      setDraft({ name: '', description: seed.description, goal: seed.goal, goals: seed.goals, timeout: '0' })
    }
    setManagerOpen(true)
  }

  const handleSelectChange = (id: string) => {
    setSelectedID(id)
    const found = allTemplates.find((t) => t.id === id)
    if (found) {
      onApply(found)
      toast('ok', `已应用模板「${found.name}」`)
    }
  }

  const saveTemplate = () => {
    if (!draft.name.trim() || !draft.goal.trim()) {
      toast('error', '请填写模板名称与总目标')
      return
    }
    if (editID) {
      const next = userTemplates.map((t) =>
        t.id === editID
          ? {
              ...t,
              name: draft.name.trim(),
              description: draft.description.trim(),
              goal: draft.goal.trim(),
              goals: draft.goals.trim(),
              timeout_seconds: Number(draft.timeout) || 0,
            }
          : t,
      )
      setUserTemplates(next)
      toast('ok', '模板已更新')
    } else {
      const created: BidTaskTemplate = {
        id: `tpl-${Date.now()}`,
        name: draft.name.trim(),
        description: draft.description.trim(),
        goal: draft.goal.trim(),
        goals: draft.goals.trim(),
        timeout_seconds: Number(draft.timeout) || 0,
        llm_profile_ids: seed.llm_profile_ids,
      }
      setUserTemplates([...userTemplates, created])
      setEditID(created.id)
      setSelectedID(created.id)
      toast('ok', '已保存为新模板')
    }
  }

  const deleteTemplate = (id: string) => {
    const next = userTemplates.filter((t) => t.id !== id)
    setUserTemplates(next)
    if (selectedID === id) setSelectedID('')
    if (editID === id) {
      setEditID(next[0]?.id ?? null)
      if (next[0]) {
        setDraft({
          name: next[0].name,
          description: next[0].description,
          goal: next[0].goal,
          goals: next[0].goals,
          timeout: String(next[0].timeout_seconds || 0),
        })
      } else {
        setDraft({ name: '', description: '', goal: '', goals: '', timeout: '0' })
      }
    }
    toast('ok', '自定义模板已删除')
  }

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-2 rounded-[10px] border border-line bg-soft/60 px-3 py-2">
        <div className="flex min-w-0 flex-1 items-center gap-2">
          <Library className="size-4 shrink-0 text-muted" aria-hidden />
          <span className="shrink-0 text-xs font-medium text-muted">项目模板</span>
          <Select
            aria-label="选择项目模板"
            className="h-7 max-w-xs flex-1 text-xs"
            value={selectedID}
            onChange={(e) => handleSelectChange(e.target.value)}
          >
            <option value="">选择预设或自定义模板快速填充…</option>
            <optgroup label="内置行业模板">
              {BUILTIN_TEMPLATES.map((t) => (
                <option key={t.id} value={t.id}>{t.name}</option>
              ))}
            </optgroup>
            {userTemplates.length > 0 && (
              <optgroup label="自定义模板">
                {userTemplates.map((t) => (
                  <option key={t.id} value={t.id}>{t.name}</option>
                ))}
              </optgroup>
            )}
          </Select>
        </div>
        <div className="flex items-center gap-1.5">
          <Button size="sm" variant="ghost" icon={<Save className="size-3.5" />} onClick={openSaveCurrent}>
            另存为模板
          </Button>
          <Button size="sm" variant="ghost" icon={<Settings2 className="size-3.5" />} onClick={openManage}>
            管理 ({userTemplates.length})
          </Button>
        </div>
      </div>

      <Modal
        open={managerOpen}
        wide
        title="投标项目模板管理"
        onClose={() => setManagerOpen(false)}
        footer={
          <>
            {editID && (
              <Button variant="danger" icon={<Trash2 className="size-3.5" />} onClick={() => deleteTemplate(editID)}>
                删除此模板
              </Button>
            )}
            <span className="flex-1" />
            <Button onClick={() => setManagerOpen(false)}>关闭</Button>
            <Button variant="primary" icon={<Save className="size-3.5" />} onClick={saveTemplate}>
              {editID ? '保存修改' : '创建模板'}
            </Button>
          </>
        }
      >
        <div className="grid gap-4 md:grid-cols-[220px_minmax(0,1fr)]">
          <div className="flex flex-col gap-1.5 border-b border-line pb-3 md:border-r md:border-b-0 md:pr-3 md:pb-0">
            <Button
              size="sm"
              icon={<Plus className="size-3.5" />}
              onClick={() => {
                setEditID(null)
                setDraft({ name: '', description: seed.description, goal: seed.goal, goals: seed.goals, timeout: '0' })
              }}
            >
              新建自定义模板
            </Button>
            <div className="mt-1 text-[11px] font-medium text-muted">自定义模板 ({userTemplates.length})</div>
            {userTemplates.length === 0 ? (
              <p className="py-3 text-xs text-muted">尚无自定义模板，可在右侧填写并保存。</p>
            ) : (
              userTemplates.map((t) => (
                <button
                  key={t.id}
                  type="button"
                  onClick={() => {
                    setEditID(t.id)
                    setDraft({
                      name: t.name,
                      description: t.description,
                      goal: t.goal,
                      goals: t.goals,
                      timeout: String(t.timeout_seconds || 0),
                    })
                  }}
                  className={cx(
                    'rounded-lg px-2.5 py-1.5 text-left text-xs transition-colors',
                    editID === t.id ? 'bg-soft font-semibold text-fg' : 'text-muted hover:bg-soft/60 hover:text-fg',
                  )}
                >
                  <div className="truncate">{t.name}</div>
                  <div className="truncate text-[11px] text-muted">{t.goal}</div>
                </button>
              ))
            )}
            <div className="mt-2 text-[11px] font-medium text-muted">内置模板（只读，点击可应用）</div>
            {BUILTIN_TEMPLATES.map((t) => (
              <button
                key={t.id}
                type="button"
                onClick={() => {
                  onApply(t)
                  setSelectedID(t.id)
                  setManagerOpen(false)
                  toast('ok', `已应用内置模板「${t.name}」`)
                }}
                className="flex items-center justify-between gap-1 rounded-lg px-2.5 py-1.5 text-left text-xs text-muted hover:bg-soft/60 hover:text-fg"
              >
                <span className="truncate">{t.name}</span>
                <Badge tone="slate">内置</Badge>
              </button>
            ))}
          </div>

          <div className="space-y-3">
            <Field label="模板名称 *">
              <Input
                value={draft.name}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                placeholder="例如：智慧园区弱电智能化工程投标模板"
              />
            </Field>
            <Field label="项目说明">
              <Input
                value={draft.description}
                onChange={(e) => setDraft({ ...draft, description: e.target.value })}
                placeholder="模板适用场景说明"
              />
            </Field>
            <Field label="默认总目标 *">
              <Input
                value={draft.goal}
                onChange={(e) => setDraft({ ...draft, goal: e.target.value })}
                placeholder="完成资格审查、符合性审查与评分项响应核验"
              />
            </Field>
            <Field label="默认分项目标（每行一条）">
              <TextArea
                rows={4}
                value={draft.goals}
                onChange={(e) => setDraft({ ...draft, goals: e.target.value })}
                placeholder={'★ 条款无废标风险\n评分项逐条有响应策略'}
              />
            </Field>
            <Field label="默认运行超时（秒，0 表示不限时）">
              <Input
                type="number"
                min={0}
                value={draft.timeout}
                onChange={(e) => setDraft({ ...draft, timeout: e.target.value })}
              />
            </Field>
          </div>
        </div>
      </Modal>
    </>
  )
}
