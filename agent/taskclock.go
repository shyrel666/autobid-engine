package agent

import (
	"context"
	"time"
)

// TaskClock 以 Context value 传递任务绝对截止时间；它不取消父 Context，只用于夹逼单次运行预算。
type TaskClock struct {
	DeadlineUnix int64
	Final        bool // 超时收尾的终局 Planner 轮次
}

type taskClockKey struct{}

func WithTaskClock(ctx context.Context, c TaskClock) context.Context {
	return context.WithValue(ctx, taskClockKey{}, c)
}

func TaskClockFrom(ctx context.Context) (TaskClock, bool) {
	c, ok := ctx.Value(taskClockKey{}).(TaskClock)
	return c, ok
}

// clampMaxDuration 把单次运行预算夹到任务 deadline 剩余时间（骨架 2-A）。
// 过期时最少给 1 秒：harness 将 0 视为无墙钟限制。
func clampMaxDuration(deadlineUnix int64, ownBudget time.Duration) (eff time.Duration, clamped bool) {
	if deadlineUnix <= 0 {
		return ownBudget, false
	}
	remaining := time.Until(time.Unix(deadlineUnix, 0))
	if remaining < time.Second {
		remaining = time.Second
	}
	clamped = ownBudget <= 0 || remaining <= ownBudget
	eff = remaining
	if ownBudget > 0 && ownBudget < remaining {
		eff = ownBudget
	}
	return eff, clamped
}
