package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
)

// PromptOverride 允许宿主按角色替换内置 system 正文（附录 A.20）；调用端追加的固定契约不受影响。
var PromptOverride func(agentKey string) (string, bool)

const plannerBase = `你是企业投标推演引擎的 Planner。你面对的是一份或多份招标文件，目标是在有限时间内把“能否投、怎么投、哪里有废标风险”推演清楚。

工作方式：
- 推演图由 Goal（任务目标）、Intent（可执行的核验方向）、Fact（核验过程事实）、Finding（经确认的合规/投策结论）组成，静态要素（条款、资质要求、评分项、商务标的）在共享实体图中，用 asset id 锚定。
- 你不亲自阅读文件或核验，只负责投放方向：每个 add_intent 必须是一个 Worker 能在一次会话内完成的有界任务，并写清验证对象与产出期望；尽量用 parents 指向它所依赖的已有事实，并通过 mandate（allowed_section_ids / allowed_requirement_ids / target_words_min / target_words_max / max_tool_calls / require_writeback）设定硬件执行边界。
- 收到新上传或修订的招标文件时，可先调用 decompose_rfp_goals 一键拆解评分标准、否决项、★技术偏离要求并初始化四大图谱 Goal 根节点与目录大纲草案。
- 先看触发事件与态势，再决定：投放新方向、用 kill_work 止损跑偏的工作、用 steer_work 纠偏，或在证据充分时 prove_goal。
- 不要重复投放与 open/running 意图等价的方向。尚不能投放的后续步骤用 TodoWrite 记下，下一轮会带回给你。
- 方向依赖本机运行组件（扫描件 OCR、图表渲染、PDF 与最终稿导出、语义检索）时，可先用 tool_runtime_status 查看能力状态；能力不可用时如实说明缺口，不要反复投放同类方向。
- 章节图表方向（总体架构、实施进度、组织架构、设备选型）派给 Worker 经 diagram-generation 技能提交图表渲染作业；可在 mandate.required_job_kinds 写 chart_render，要求完成前图表已真实生成并绑定章节当前修订。
- 态势中的 waiting_intents 是已提交后台作业（解析、图表、导出）并释放执行槽的意图：它们仍在进行，作业结束后会自动唤醒原 Worker 收尾。不要为它们重复派发等价方向，也不要 reopen 或 kill；需要细节时用 agent_job_status 查看（可传 intent_id）。
- 人工补充（hints）处理完后用 consume_hint 标记；招标文件揭示出需要单独核验的新目标时用 add_goal。
- 态势中的 cold_index 是久未变化的已结束节点；当它们的结论已稳定时，用 compact_nodes 折叠为一条摘要，保持态势精简（成员仍可用 node_detail 查看）。
- 本轮决策完成后简短说明你做了什么、为什么，然后结束。`

const plannerFinalContract = `

【终局轮】任务时间已到。禁止投放新意图。请只核对每个 Goal 是否已有充分的 Fact/Finding 支撑：有则 prove_goal，没有则如实说明缺口，然后结束。`

const workerBase = `你是企业投标推演引擎的 Worker（%s）。你只负责执行 system 末尾给出的“当前意图”，不要扩展到其他方向。

工作方式：
- 先用 graph_overview / list_assets / node_detail 了解已有事实与招标要素，避免重复劳动。
- 招标原文用 document_outline / search_document / read_chunk 精读；引用原文时把 read_chunk 返回的 source_ref 原样写入 insert_assets、record_fact 的 source_ref 或 report_finding 的 source_refs。
- 需要专业核验或标书编审（资格比对 qualification-audit、评分模拟 scoring-simulation、报价验算 commercial-pricing、知识库复用与证据核验 knowledge-reuse、技术标章节写作 proposal-writing、技术标正文审查 proposal-review）时，先调用 Skill 加载对应技能，再用 SearchExtraTools("select:<tool>") 获取解锁工具的 Schema 并直接调用该工具。
- 需要本机运行组件（OCR、图表渲染、文档引擎、语义检索模型）时，可用 tool_runtime_status 查看能力状态；available=false 时把缺口 record_fact 记下，不要反复重试。
- 本意图提交过后台作业时，可用 agent_job_status 查看状态；作业结束后结果会作为新回合送达，不要循环轮询。提交本身不是交付物，结论仍须基于作业的真实结果写回。
- 识别出的新招标要素用 insert_assets 写入（会自动锚定到当前意图），要素关系用 link_assets 写入。
- 每个核实的事实（包括“不满足”“未找到”这类否定结论）都用 record_fact 记录，并锚定相关要素。
- 能形成可交付结论（废标风险、合规缺口、得分策略、报价建议）时，用 report_finding 报告，并把支撑它的事实作为 evidence。
- 没写回推演图的内容等于没做。完成后用两三句话总结结论与遗留问题，然后结束。`

func basePrompt(agentKey, builtin string) string {
	if PromptOverride != nil {
		if s, ok := PromptOverride(agentKey); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return builtin
}

func plannerSystem(final bool) string {
	s := basePrompt(AgentPlanner, plannerBase)
	if final {
		s += plannerFinalContract
	}
	return s
}

// workerSystem 把当前 Intent 放在 system 末尾，降低压缩时丢失执行边界的风险（§2.6）。
func workerSystem(name string, intent *db.Node, deferredBlock string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(basePrompt(AgentWorker, workerBase), name))
	if deferredBlock != "" {
		b.WriteString("\n\n")
		b.WriteString(deferredBlock)
	}
	b.WriteString("\n\n## 当前意图\n")
	fmt.Fprintf(&b, "- intent_id: %d\n- 优先级: %d\n", intent.ID, intent.Priority)
	var p struct {
		Summary   string `json:"summary"`
		Rationale string `json:"rationale"`
	}
	_ = json.Unmarshal(intent.Payload, &p)
	fmt.Fprintf(&b, "- 内容: %s\n", p.Summary)
	if p.Rationale != "" {
		fmt.Fprintf(&b, "- 背景: %s\n", p.Rationale)
	}
	if len(intent.Anchors) > 0 {
		fmt.Fprintf(&b, "- 相关要素 asset_id: %v\n", intent.Anchors)
	}
	if m := db.NodeMandate(intent); m != nil {
		b.WriteString("\n## 执行边界（Mandate 硬约束）\n")
		if len(m.AllowedSectionIDs) > 0 {
			fmt.Fprintf(&b, "- 允许修改章节 section_id 白名单: %v（越界写回将被硬件门禁拦截）\n", m.AllowedSectionIDs)
		}
		if len(m.AllowedRequirementIDs) > 0 {
			fmt.Fprintf(&b, "- 允许绑定要求 requirement_id 白名单: %v\n", m.AllowedRequirementIDs)
		}
		if m.TargetWordsMin > 0 || m.TargetWordsMax > 0 {
			fmt.Fprintf(&b, "- 章节正文字数/字符区间要求: [%d, %d]\n", m.TargetWordsMin, m.TargetWordsMax)
		}
		if m.MaxToolCalls > 0 {
			fmt.Fprintf(&b, "- 最大工具调用次数上限: %d\n", m.MaxToolCalls)
		}
		if len(m.ForbiddenTools) > 0 {
			fmt.Fprintf(&b, "- 禁用工具列表: %v\n", m.ForbiddenTools)
		}
		if m.RequireWriteback {
			b.WriteString("- 完结约束: 结束前必须至少成功调用一次推演图或标书域写回工具\n")
		}
	}
	return b.String()
}

// TriggerEvent 是 Planner 唤醒原因（附录 §2.2 原文结构）；Manager 生产，Planner 消费。
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string
	Goals    []string
	OldGoal  string
	NewGoal  string
	Hints    []string
}

const (
	TriggerIntentDone  = "intent_done"
	TriggerFinding     = "finding"
	TriggerGoalAdded   = "goal_added"
	TriggerHintAdded   = "hint_added"
	TriggerLeaseReaped = "lease_reaped"
	TriggerWorkStopped = "work_stopped"
	// TriggerWatchdogAlert：在途 Worker 因死循环、连续报错或长回合零产出被独立 Watchdog 熔断。
	TriggerWatchdogAlert = "watchdog_alert"
	// TriggerDocumentAdded：上传了新的招标文档；Summary 为文档说明，Detail 为切分统计。
	TriggerDocumentAdded = "document_added"
)

type intentOutcome struct {
	ID      int64
	State   string
	Summary string
	Yields  []db.NodeBrief
}

func renderPlannerInput(goal string, triggers []TriggerEvent, outcomes map[int64]intentOutcome, overview *db.Overview, todos string, final bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "当前时间：%s\n任务总目标：%s\n\n", time.Now().Format(time.RFC3339), goal)
	if len(triggers) > 0 {
		b.WriteString("## 本轮触发事件\n")
		for _, t := range triggers {
			switch t.Kind {
			case TriggerIntentDone:
				o := outcomes[t.IntentID]
				fmt.Fprintf(&b, "- 意图 #%d 已结束（%s）：%s\n", t.IntentID, o.State, o.Summary)
				for _, y := range o.Yields {
					fmt.Fprintf(&b, "  - 产出 #%d：%s\n", y.ID, y.Summary)
				}
			case TriggerFinding:
				fmt.Fprintf(&b, "- 意图 #%d 报告了结论：%s\n", t.IntentID, t.Summary)
			case TriggerGoalAdded:
				fmt.Fprintf(&b, "- 新增目标：%s\n", strings.Join(t.Goals, "；"))
			case TriggerHintAdded:
				fmt.Fprintf(&b, "- 人工补充信息：%s\n", strings.Join(t.Hints, "；"))
			case TriggerWatchdogAlert:
				fmt.Fprintf(&b, "- 【Watchdog 熔断告警】意图 #%d 因执行偏离或死循环被强制中止：%s。请审查原因，必要时缩小 mandate 范围或拆分后重新投放。\n", t.IntentID, t.Detail)
			case TriggerDocumentAdded:
				fmt.Fprintf(&b, "- 新上传招标文档：%s（%s）。Worker 可用 document_outline / search_document / read_chunk 精读，请据此投放要素提取与核验方向。\n", t.Summary, t.Detail)
			default:
				fmt.Fprintf(&b, "- %s #%d %s\n", t.Kind, t.IntentID, t.Detail)
			}
		}
		b.WriteString("\n")
	} else {
		b.WriteString("## 本轮触发事件\n- 无（定时心跳或首轮）\n\n")
	}
	if overview != nil {
		raw, _ := json.MarshalIndent(overview, "", "  ")
		b.WriteString("## 推演图态势\n```json\n")
		b.Write(raw)
		b.WriteString("\n```\n\n")
	}
	if todos != "" {
		b.WriteString("## 你上一轮留下的待办\n")
		b.WriteString(todos)
		b.WriteString("\n")
	}
	if final {
		b.WriteString("这是终局轮，只做目标核验与总结。\n")
	} else {
		b.WriteString("请据此完成本轮规划。\n")
	}
	return b.String()
}

func renderWorkerInput(overview *db.Overview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "当前时间：%s\n请开始执行当前意图。\n", time.Now().Format(time.RFC3339))
	if overview != nil {
		raw, _ := json.MarshalIndent(overview, "", "  ")
		b.WriteString("\n## 推演图态势\n```json\n")
		b.Write(raw)
		b.WriteString("\n```\n")
	}
	return b.String()
}

const workerContinueInput = "你之前在执行这个意图时被中断了。请基于已有对话继续，避免重复已写回的内容；完成后总结并结束。"
