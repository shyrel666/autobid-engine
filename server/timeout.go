package server

import (
	"context"
	"fmt"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
)

// deadlineCoordinator 独立于 Planner/Worker 轮询任务截止时间；到期后执行 settleTask（§3.4）。
// 暂停不会让墙钟停走。任务复活（Revive）会清空截止时间与结算标记，协调器随之重新等待。
func (e *Engine) deadlineCoordinator(ctx context.Context, t *Task) {
	for ctx.Err() == nil {
		lc := t.lifecycleSnapshot()
		if db.IsTerminalTaskStatus(lc.Status) || e.isSettling(t.ID) || lc.DeadlineAt == nil {
			sleepCtx(ctx, e.cfg.DeadlinePoll)
			continue
		}
		if wait := time.Until(*lc.DeadlineAt); wait > 0 {
			sleepCtx(ctx, min(wait, e.cfg.DeadlineMaxSleep))
			continue
		}
		e.settleTask(ctx, t)
	}
}

func (e *Engine) markSettling(taskID int64) bool {
	_, loaded := e.settling.LoadOrStore(taskID, struct{}{})
	return !loaded
}

// settleTask：①置 settling（阻断新领取与常规规划）-> ②等待在途工作借夹逼预算自行收尾
// -> ③超出 grace 以 AbortSettleDrainTimeout 强制取消 -> ④终局 Planner -> ⑤guarded 落终态（骨架 2-B）。
func (e *Engine) settleTask(ctx context.Context, t *Task) {
	if !e.markSettling(t.ID) {
		return
	}
	e.emit(t, db.Activity{Worker: "system", Kind: "state", Summary: "任务到达截止时间，进入超时收尾"})

	hardStop := time.Now().Add(e.cfg.SettleDrainGrace)
	for e.inflightCount(t.ID) > 0 {
		if time.Now().After(hardStop) {
			e.cancelExec(t.ID, agent.AbortSettleDrainTimeout)
			sleepCtx(ctx, e.cfg.SettleCancelWait) // 留给被取消 Worker 完成状态 CAS
			break
		}
		if sleepCtx(ctx, 200*time.Millisecond) {
			return
		}
	}

	met := e.runFinalPlannerRound(ctx, t)
	if !e.beginTaskOperation(t.ID) {
		return
	}
	defer e.decInflight(t.ID)
	status := db.TaskTimeout
	if met {
		status = db.TaskDone
	}
	c, cancel := dbCtx(ctx)
	defer cancel()
	if won, err := e.m.SetTaskStatusGuarded(c, t, status); err == nil && won {
		e.emit(t, db.Activity{Worker: "system", Kind: "state", Summary: "超时收尾完成，任务状态：" + status})
		waitingState := db.StateIntentExhausted
		if met {
			waitingState = db.StateIntentStopped
		}
		e.settleWaitingIntents(c, t, waitingState)
	}
}

// runFinalPlannerRound 使用协调器的活 Context（而非可能已被取消的 execCtx）运行一轮终局规划。
func (e *Engine) runFinalPlannerRound(ctx context.Context, t *Task) (met bool) {
	if e.IsDeleting(t.ID) {
		return false
	}
	planner, _ := e.resolve(t)
	for planner == nil {
		if sleepCtx(ctx, e.cfg.DeadlinePoll) {
			return false
		}
		if db.IsTerminalTaskStatus(t.lifecycleSnapshot().Status) || e.IsDeleting(t.ID) {
			return false
		}
		planner, _ = e.resolve(t)
	}
	if !e.beginTaskOperation(t.ID) {
		return false
	}
	defer e.decInflight(t.ID)

	fctx := e.interceptCtx(e.clockCtx(ctx, t, true), t, 0, "planner")
	e.emit(t, db.Activity{Worker: "planner", Kind: "round",
		Summary: fmt.Sprintf("任务超时收尾·终局判定（第 %d 轮）", e.nextPlannerRound(t.ID))})
	met, _, err := planner.Plan(fctx, t.ID, t.Assets, t.Store, t.Goal, t.drainTriggers(), func(a db.Activity) { e.emit(t, a) })
	if err != nil {
		e.emit(t, db.Activity{Worker: "planner", Kind: "error", Summary: "终局规划失败", Detail: err.Error()})
	}
	return met
}
