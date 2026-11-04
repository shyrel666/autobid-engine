package db

import (
	"encoding/json"
	"time"
)

// 推演节点类别（附录 B.7）。完整合法 kind/state 组合以 schema.sql 的 ck_node_state 为准。
const (
	KindBegin   = "begin"   // DEPRECATED: 旧任务根；新任务以 origin Fact（KindFact + StateOrigin）作根
	KindGoal    = "goal"    // 任务目标，如“完成 A 标段合规性核验”
	KindIntent  = "intent"  // 推演方向（Planner 生成），如“核对资质要求 4.2 与我方证书”
	KindFact    = "fact"    // 标书核验过程事实（含否定结论），挂在其 Intent 下
	KindFinding = "finding" // 经确认的投策产出物/合规结论（report_finding），区别于 fact
	KindHint    = "hint"    // 人工补充上下文
	KindDigest  = "digest"  // 冷节点压缩摘要；成员通过 covers 保留，可按 id 还原
)

const (
	StateIntentOpen      = "open"
	StateIntentRunning   = "running"
	StateIntentPaused    = "paused"
	StateIntentDone      = "done"
	StateIntentBlocked   = "blocked"
	StateIntentExhausted = "exhausted"
	StateIntentStopped   = "stopped"
	StateIntentDeleted   = "deleted"
)

const (
	StateGoalOpen      = "open"
	StateGoalMet       = "met"
	StateGoalAbandoned = "abandoned"
)

const (
	StateConfirmed = "confirmed" // fact / finding
	StateDismissed = "dismissed" // fact / finding
	StateOrigin    = "origin"    // 任务根事实
)

const (
	StateHintActive   = "active"
	StateHintConsumed = "consumed"
)

const (
	StateDigestActive     = "active"
	StateDigestSuperseded = "superseded"
)

// 推演边：方向比名称更重要（§1.3）。
const (
	RelSpawns      = "spawns"       // origin Fact -> Goal
	RelDerivedFrom = "derived_from" // 已有 Fact/Finding -> 新 Intent
	RelYields      = "yields"       // Intent -> Fact/Finding
	RelProves      = "proves"       // Fact/Finding -> Goal
	RelCovers      = "covers"       // Digest -> 被压缩成员
)

// 静态实体类别（招标文件要素）。
const (
	AssetTender         = "tender"
	AssetLot            = "lot"
	AssetClause         = "clause"
	AssetQualification  = "qualification"
	AssetScoringItem    = "scoring_item"
	AssetCommercialItem = "commercial_item"
)

// 静态实体边推荐关系名（entity_edges.rel 只要求非空，以下为约定词表）。
const (
	EntityRelContains   = "contains"       // tender -> lot / clause；lot -> 子要素
	EntityRelRequires   = "requires"       // lot -> qualification
	EntityRelScoredBy   = "scored_by"      // lot -> scoring_item
	EntityRelPricedBy   = "priced_by"      // lot -> commercial_item
	EntityRelReferences = "references"     // clause -> clause / scoring_item
	EntityRelConflicts  = "conflicts_with" // 条款间矛盾（如工期、付款条件冲突）
)

// 产出物分类与严重度（findings 表 CHECK）。
const (
	FindingCompliance = "compliance" // 合规/废标风险结论
	FindingRisk       = "risk"
	FindingScoring    = "scoring" // 评分项得分策略
	FindingPricing    = "pricing"
	FindingStrategy   = "strategy"

	SeverityInfo     = "info"
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical" // 否决项/实质性不响应
)

const (
	TaskCreated = "created"
	TaskRunning = "running"
	TaskPaused  = "paused"
	TaskDone    = "done"
	TaskFailed  = "failed"
	TaskTimeout = "timeout"
)

const (
	DefaultIntentPriority = 5
	DefaultIntentLeaseTTL = 90 * time.Second
	// BlockedReasonQuotaExhausted 标记因 LLM 配额链耗尽而阻塞的 Intent，供配额恢复后批量重开。
	BlockedReasonQuotaExhausted = "llm_quota_exhausted"
	// BlockedReasonWaitingJob 标记释放了执行槽、等待持久化后台作业事件的 Intent（S2）；
	// 只有作业事件唤醒协调器能把它重新置为 open，Reopen / 配额批量重开都不得绕过。
	BlockedReasonWaitingJob = "waiting_job"
	// BlockedReasonRequiredJobFailed 标记必需后台作业失败、取消或结果已失效而不能完成的 Intent，交由 Planner 重规划。
	BlockedReasonRequiredJobFailed = "required_job_failed"
)

// IsTerminalIntentState 报告 Intent 状态是否需要 completed_at。
func IsTerminalIntentState(state string) bool {
	switch state {
	case StateIntentDone, StateIntentBlocked, StateIntentExhausted, StateIntentStopped:
		return true
	}
	return false
}

// IntentMandate 定义 Planner 赋予单个 Intent 的硬性执行边界（Mandate）。
// 存储在 exploration_nodes.payload.mandate 中，由 Worker PreToolUse / Stop 门禁与 Watchdog 强制执行。
type IntentMandate struct {
	RestrictObjects       bool     `json:"restrict_objects,omitempty"` // empty allowed lists deny access when true
	RequiredSectionIDs    []int64  `json:"required_section_ids,omitempty"`
	AllowedSectionIDs     []int64  `json:"allowed_section_ids,omitempty"`
	AllowedRequirementIDs []int64  `json:"allowed_requirement_ids,omitempty"`
	TargetWordsMin        int      `json:"target_words_min,omitempty"`
	TargetWordsMax        int      `json:"target_words_max,omitempty"`
	MaxToolCalls          int      `json:"max_tool_calls,omitempty"`
	ForbiddenTools        []string `json:"forbidden_tools,omitempty"`
	RequireWriteback      bool     `json:"require_writeback,omitempty"`
	// AllowedJobKinds 收窄可提交/订阅/查询的后台作业类型；RestrictObjects 为真时空列表表示无权限。
	AllowedJobKinds []string `json:"allowed_job_kinds,omitempty"`
	// RequiredJobKinds 要求完成前每类至少有一个本意图订阅的作业成功且结果仍有效。
	RequiredJobKinds []string `json:"required_job_kinds,omitempty"`
	// AllowedTemplateIDs 收窄导出可用的上传模板（内置默认模板不受限）；RestrictObjects 为真时空列表表示只能用默认模板。
	AllowedTemplateIDs []int64 `json:"allowed_template_ids,omitempty"`
}

// IsEmpty 判断 Mandate 是否未设置任何约束条件。
func (m *IntentMandate) IsEmpty() bool {
	if m == nil {
		return true
	}
	return len(m.AllowedSectionIDs) == 0 &&
		!m.RestrictObjects &&
		len(m.RequiredSectionIDs) == 0 &&
		len(m.AllowedRequirementIDs) == 0 &&
		m.TargetWordsMin <= 0 &&
		m.TargetWordsMax <= 0 &&
		m.MaxToolCalls <= 0 &&
		len(m.ForbiddenTools) == 0 &&
		!m.RequireWriteback &&
		len(m.AllowedJobKinds) == 0 &&
		len(m.RequiredJobKinds) == 0 &&
		len(m.AllowedTemplateIDs) == 0
}

// NodeMandate 从节点 payload 中解析可选的 IntentMandate。
func NodeMandate(n *Node) *IntentMandate {
	if n == nil || len(n.Payload) == 0 {
		return nil
	}
	var wrap struct {
		Mandate *IntentMandate `json:"mandate"`
	}
	if err := json.Unmarshal(n.Payload, &wrap); err != nil || wrap.Mandate.IsEmpty() {
		return nil
	}
	return wrap.Mandate
}
