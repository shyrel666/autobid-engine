package harness

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/permission"
	"github.com/autobid/autobid-engine/internal/tool"
)

// runTools 执行一个回合的全部工具调用并按原顺序返回 tool_result 块。
// 连续的并发安全调用并行执行，其余串行；ctx 结束后尚未开始的调用以错误结果占位，保持 tool_use/tool_result 配对。
func (l *queryLoop) runTools(ctx context.Context, uses []llm.ContentBlock) []llm.ContentBlock {
	results := make([]llm.ContentBlock, len(uses))
	for i := 0; i < len(uses); {
		j := i + 1
		if l.concurrencySafe(uses[i]) {
			for j < len(uses) && l.concurrencySafe(uses[j]) {
				j++
			}
		}
		var wg sync.WaitGroup
		for k := i; k < j; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				results[k] = l.execute(ctx, uses[k])
			}(k)
		}
		wg.Wait()
		for k := i; k < j; k++ {
			r := results[k]
			l.emit(Event{Kind: KindToolResult, ToolResult: &ToolResultEvent{
				ToolUseID: r.ToolUseID, Name: uses[k].Name, Content: r.Text, IsError: r.IsError,
			}})
		}
		i = j
	}
	return results
}

func (l *queryLoop) concurrencySafe(b llm.ContentBlock) bool {
	t, ok := l.registry.Get(b.Name)
	return ok && t.IsConcurrencySafe(b.Input)
}

func (l *queryLoop) execute(ctx context.Context, b llm.ContentBlock) llm.ContentBlock {
	res := l.dispatchTool(ctx, b)
	return llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: b.ID, Text: res.Content, IsError: res.IsError}
}

// dispatchTool 是唯一的工具执行入口。门禁顺序：
// 存在性 -> 收尾闸门（Patch ②）-> 延迟工具解锁（Patch ③）-> Schema 校验 -> 工具自检权限 -> PreToolUse Hook -> Call -> PostToolUse。
// 任何拒绝都以 is_error=true 的 tool_result 反馈模型，不产生副作用。
func (l *queryLoop) dispatchTool(ctx context.Context, call llm.ContentBlock) (res tool.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = tool.Errorf("tool %q panicked: %v", call.Name, r)
		}
	}()
	if ctx.Err() != nil {
		return tool.Errorf("tool %q skipped: tool execution budget exhausted (%v)", call.Name, context.Cause(ctx))
	}
	t, ok := l.registry.Get(call.Name)
	if !ok {
		return tool.Errorf("unknown tool %q", call.Name)
	}
	input := call.Input
	if len(input) == 0 {
		input = []byte("{}")
	}
	if err := l.checkGates(call.Name, input); err != nil {
		return tool.Errorf("%v", err)
	}
	if err := tool.ValidateInput(t.InputSchema(), input); err != nil {
		return tool.Errorf("invalid input for %s: %v", call.Name, err)
	}
	if l.opts.Permission.Mode != permission.ModeBypass {
		d := t.CheckPermissions(ctx, input, l.opts.Permission)
		switch d.Behavior {
		case permission.BehaviorDeny:
			return tool.Errorf("permission denied for %s: %s", call.Name, d.Message)
		case permission.BehaviorAsk:
			if l.opts.Hooks == nil {
				return tool.Errorf("permission required for %s: %s", call.Name, d.Message)
			}
		}
		if len(d.UpdatedInput) > 0 {
			input = d.UpdatedInput
		}
	}
	if l.opts.Hooks != nil {
		block, msg, updated := l.opts.Hooks.PreToolUse(ctx, call.Name, input)
		if block {
			return tool.Errorf("tool %s blocked: %s", call.Name, msg)
		}
		if len(updated) > 0 {
			// Hook 改写后的输入须重新过闸与验参，防止改写绕过门禁。
			if err := l.checkGates(call.Name, updated); err != nil {
				return tool.Errorf("%v", err)
			}
			if err := tool.ValidateInput(t.InputSchema(), updated); err != nil {
				return tool.Errorf("invalid input for %s after hook: %v", call.Name, err)
			}
			input = updated
		}
	}
	tc := &tool.ToolContext{
		SessionID:  l.opts.SessionID,
		ToolUseID:  call.ID,
		Turn:       l.turn,
		Settling:   l.settling,
		Permission: l.opts.Permission,
	}
	res, err := t.Call(ctx, input, tc)
	if err != nil {
		res = tool.Errorf("%s failed: %v", call.Name, err)
	}
	if l.opts.Hooks != nil {
		// PostToolUse 使用父 Context：工具预算耗尽不应吞掉审计/计量回调。
		l.opts.Hooks.PostToolUse(l.ctx, call.Name, input, []byte(res.Content), res.IsError)
	}
	return res
}

// checkGates 对工具名做收尾与解锁门禁；ExecuteExtraTool 按其目标工具名判定。
func (l *queryLoop) checkGates(name string, input []byte) error {
	gateName := name
	if target, ok := tool.IsDeferredWrapper(name, input); ok {
		gateName = target
	}
	if err := l.checkSettlementToolGate(gateName); err != nil {
		return err
	}
	return l.checkDeferredUnlockGate(gateName)
}

// checkSettlementToolGate（Patch ②）：收尾阶段在 Dispatch 层硬拦截禁用工具，并在配置白名单时拒绝一切非写回工具。
func (l *queryLoop) checkSettlementToolGate(toolName string) error {
	if !l.settling || l.opts.Settlement == nil {
		return nil
	}
	st := l.opts.Settlement
	if slices.Contains(st.DisabledTools, toolName) {
		return fmt.Errorf("settlement phase violation: tool %q is disabled during wrap-up; only graph write-back is permitted", toolName)
	}
	if len(st.AllowedTools) > 0 && !slices.Contains(st.AllowedTools, toolName) {
		return fmt.Errorf("settlement phase violation: tool %q is not in write-back allowlist %v", toolName, st.AllowedTools)
	}
	return nil
}

// checkDeferredUnlockGate（Patch ③）：延迟工具未在 UnlockSet 中解锁时拒绝执行，防止模型盲猜工具名绕过 Skill 直调。
func (l *queryLoop) checkDeferredUnlockGate(toolName string) error {
	if !l.deferredNames[toolName] {
		return nil
	}
	if !l.opts.UnlockSet.IsUnlocked(toolName) {
		return fmt.Errorf("permission denied: deferred tool %q is locked; invoke its owning Skill first", toolName)
	}
	return nil
}

// filterSettlementTools 按与 checkSettlementToolGate 相同的规则收缩模型可见工具。
func filterSettlementTools(tools []tool.CoreTool, st *Settlement) []tool.CoreTool {
	out := make([]tool.CoreTool, 0, len(tools))
	for _, t := range tools {
		if slices.Contains(st.DisabledTools, t.Name()) {
			continue
		}
		if len(st.AllowedTools) > 0 && !slices.Contains(st.AllowedTools, t.Name()) {
			continue
		}
		out = append(out, t)
	}
	return out
}
