package server

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/internal/llm"
)

const thinkingOnlyNudge = "你上一轮没有输出任何结论，也没有调用工具。请立即采取一个具体步骤（调用工具核验或写回推演图），" +
	"或者给出明确结论后结束。"

// steerHooks 包裹控制面 Hook：PreToolUse 优先投递纠偏指令；Stop 在 inner 未裁决时对空转回合轻推（§5.1）。
type steerHooks struct {
	inner  harness.HookRunner
	drain  func() (string, bool)
	events func() (string, bool) // 后台作业终态事件；可为 nil
	nudges *atomic.Int64
	limit  int // <0 关闭轻推
	label  string
}

func (h *steerHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if msg, ok := h.drain(); ok {
		return true, "操作员纠偏指令（本次工具调用未执行，请按指令调整后继续）：" + msg, nil
	}
	if h.events != nil {
		if msg, ok := h.events(); ok {
			return true, "后台作业状态更新（本次工具调用未执行，请先处理该结果再继续）：" + msg, nil
		}
	}
	if h.inner != nil {
		return h.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

func (h *steerHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}

func (h *steerHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if h.drain != nil {
		if msg, ok := h.drain(); ok {
			return false, []string{"操作员补充指令：" + msg}, ""
		}
	}
	if h.events != nil {
		if msg, ok := h.events(); ok {
			return false, []string{"后台作业状态更新：" + msg}, ""
		}
	}
	if h.inner != nil {
		prevent, blocking, msg := h.inner.Stop(ctx, messages)
		if prevent || len(blocking) > 0 {
			return prevent, blocking, msg
		}
	}
	if h.limit >= 0 && isThinkingOnlyTurn(messages) && h.nudges.Add(1) <= int64(h.limit) {
		return false, []string{thinkingOnlyNudge}, ""
	}
	return false, nil, ""
}

// isThinkingOnlyTurn：最近一条 assistant 既无文本也无工具调用（不要求存在 thinking 块）。
func isThinkingOnlyTurn(messages []llm.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		return strings.TrimSpace(m.Text()) == "" && len(m.ToolUses()) == 0
	}
	return false
}
