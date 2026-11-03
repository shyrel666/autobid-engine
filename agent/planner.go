package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

type PlannerConfig struct {
	Provider   llm.Provider
	Model      string
	MaxTurns   int           // 默认 30
	RunTimeout time.Duration // 单轮规划预算，0 不限（仍受任务 deadline 夹逼）
	MaxTokens  int
	KillWork   func(intentID int64) error
	SteerWork  func(intentID int64, msg string) error
	Hooks      harness.HookRunner
	// ExtraTools 是宿主装配的只读业务工具（如 tool_runtime_status），终局轮同样可用；不得装配提交或审批类工具。
	ExtraTools []tool.CoreTool
}

// Planner 每轮新建会话；跨轮连续性依赖“图 + 触发事件 + 共享 TodoStore”（§2.4）。
type Planner struct {
	cfg    PlannerConfig
	todoMu sync.Mutex
	todos  map[int64]*tool.TodoStore
}

func NewPlanner(cfg PlannerConfig) *Planner {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 30
	}
	return &Planner{cfg: cfg, todos: map[int64]*tool.TodoStore{}}
}

// todoFor 返回推演空间的待办便签；仅存在于本 Planner 实例内存。
func (p *Planner) todoFor(expID int64) *tool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s, ok := p.todos[expID]
	if !ok {
		s = tool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// Plan 执行一轮规划。met 表示全部（非 abandoned）目标已达成；reason 为 harness 终止原因。
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	clock, _ := TaskClockFrom(ctx)
	set := newToolSet(as, ts, AgentPlanner, taskID, 0)
	set.killWork, set.steerWork = p.cfg.KillWork, p.cfg.SteerWork
	todos := p.todoFor(ts.ID())

	// 每轮推进规划轮次并冷却久未变化的节点；冷节点进入态势的 cold_index，可经 compact_nodes 折叠（§1.6）。
	round, err := ts.AdvanceRound(ctx)
	if err == nil {
		set.round = round
		_, err = ts.StampColdNodes(ctx, round, db.DefaultColdAfterRounds)
	}
	if err != nil && emit != nil {
		emit(db.Activity{Worker: AgentPlanner, Kind: "error", Summary: "推进规划轮次/冷却节点失败", Detail: err.Error()})
	}

	overview, err := set.overview(ctx, true)
	if err != nil {
		return false, "", err
	}
	outcomes := map[int64]intentOutcome{}
	for _, t := range triggers {
		if t.Kind != TriggerIntentDone || t.IntentID <= 0 {
			continue
		}
		o := intentOutcome{ID: t.IntentID}
		if n, err := ts.GetNode(ctx, t.IntentID); err == nil {
			o.State, o.Summary = n.State, summaryOf(n, 200)
			if n.BlockedReason != "" {
				o.State += "：" + n.BlockedReason
			}
		}
		if ys, err := ts.YieldedNodes(ctx, t.IntentID, 20); err == nil {
			o.Yields = db.Briefs(ys, false)
		}
		outcomes[t.IntentID] = o
	}

	opts := harness.Options{
		Provider:     p.cfg.Provider,
		Model:        p.cfg.Model,
		SystemPrompt: plannerSystem(clock.Final),
		Tools:        append(set.plannerTools(todos, clock.Final), p.cfg.ExtraTools...),
		MaxTurns:     p.cfg.MaxTurns,
		MaxTokens:    p.cfg.MaxTokens,
		Hooks:        p.cfg.Hooks,
		SessionID:    plannerSessionID(ts.ID()),
	}
	if clock.Final {
		opts.MaxDuration = p.cfg.RunTimeout
		opts.Settlement = wrapupSettlement(AgentPlanner, nil, nil)
	} else {
		var clamped bool
		opts.MaxDuration, clamped = clampMaxDuration(clock.DeadlineUnix, p.cfg.RunTimeout)
		opts.Settlement = wrapupSettlementForTask(AgentPlanner, []string{ToolAddIntent}, nil, clamped)
	}

	input := renderPlannerInput(goal, triggers, outcomes, overview, todos.Render(), clock.Final)
	term := harness.Run(ctx, opts, []llm.Message{llm.UserText(input)}, activityEmitter(AgentPlanner, nil, emit))

	total, metCount, statErr := ts.GoalStats(context.WithoutCancel(ctx))
	met = set.GoalMet() || (statErr == nil && total > 0 && metCount == total)
	return met, string(term.Reason), terminalErr(term)
}

func plannerSessionID(expID int64) string {
	return "exp" + itoa(expID) + "-planner"
}

// terminalErr 把非正常终止转换为错误；预算耗尽（含收尾）与正常完成不算错误。
func terminalErr(t *harness.Terminal) error {
	switch t.Reason {
	case harness.ReasonModelError, harness.ReasonAbortedStreaming, harness.ReasonAbortedTools:
		if t.Err != nil {
			return t.Err
		}
		return errors.New("agent: run ended with " + string(t.Reason))
	}
	return nil
}
