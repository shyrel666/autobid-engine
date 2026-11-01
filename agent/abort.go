// Package agent 实现 Planner / Worker 两类角色：组装工具、提示与预算，调用 harness 执行，并把结果写回推演图。
package agent

import "errors"

// AbortCause 是 context.CancelCauseFunc 使用的命名取消原因；上层用 errors.Is 精确判定分流（§2.7）。
type AbortCause struct {
	Code    string
	Message string
}

func (c *AbortCause) Error() string {
	if c.Message == "" {
		return "abort: " + c.Code
	}
	return "abort: " + c.Code + ": " + c.Message
}

func (c *AbortCause) Is(target error) bool {
	if c == nil || target == nil {
		return c == target
	}
	t, ok := target.(*AbortCause)
	return ok && t != nil && c.Code != "" && c.Code == t.Code
}

var (
	AbortGoalMet             = &AbortCause{Code: "goal_met", Message: "all goals met"}
	AbortKilledByPlanner     = &AbortCause{Code: "killed_by_planner"}
	AbortKilledByWatchdog    = &AbortCause{Code: "killed_by_watchdog"}
	AbortWorkPausedByUser    = &AbortCause{Code: "work_paused_by_user"}
	AbortWorkCancelledByUser = &AbortCause{Code: "work_cancelled_by_user"}
	AbortWorkFinished        = &AbortCause{Code: "work_finished"}
	AbortSettleDrainTimeout  = &AbortCause{Code: "settle_drain_timeout"}
	// AbortLeaseLost：Patch ④ 租约被回收或抢占，本 Worker 不再拥有该 Intent，不得写回状态。
	AbortLeaseLost = &AbortCause{Code: "lease_lost"}

	// 以下原因表示“任务执行被暂停”，在途 Intent 回到 open 供恢复后重领。
	AbortPausedByUser         = &AbortCause{Code: "paused_by_user"}
	AbortPausedByOrchestrator = &AbortCause{Code: "paused_by_orchestrator"}
	AbortPausedOnReload       = &AbortCause{Code: "paused_on_reload"}
	AbortPausedRaceGuard      = &AbortCause{Code: "paused_race_guard"}
	AbortQueuedForAdmission   = &AbortCause{Code: "queued_for_admission"}
	AbortLLMUnavailableQueued = &AbortCause{Code: "llm_unavailable_queued"}
	AbortTaskDeleted          = &AbortCause{Code: "task_deleted"}
)

var pausedCodes = map[string]bool{
	AbortPausedByUser.Code:         true,
	AbortPausedByOrchestrator.Code: true,
	AbortPausedOnReload.Code:       true,
	AbortPausedRaceGuard.Code:      true,
	AbortQueuedForAdmission.Code:   true,
	AbortLLMUnavailableQueued.Code: true,
	AbortTaskDeleted.Code:          true,
}

// IsTaskExecutionPaused 报告取消原因是否属于“任务执行暂停”集合（taskExecutionPaused）。
func IsTaskExecutionPaused(cause error) bool {
	var ac *AbortCause
	return errors.As(cause, &ac) && pausedCodes[ac.Code]
}
