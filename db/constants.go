package db

import "time"

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
)

// IsTerminalIntentState 报告 Intent 状态是否需要 completed_at。
func IsTerminalIntentState(state string) bool {
	switch state {
	case StateIntentDone, StateIntentBlocked, StateIntentExhausted, StateIntentStopped:
		return true
	}
	return false
}
