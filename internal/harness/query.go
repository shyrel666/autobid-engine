package harness

import (
	"context"
	"errors"
	"iter"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

var errConsumerStopped = errors.New("harness: event consumer stopped")

// Query 运行一次查询循环。事件按发生顺序产出，最后一个事件恒为 KindResult（消费者提前停止时除外）。
// messages 为既有会话（不含 system），调用方负责在末尾放入本轮 user 输入。
func Query(ctx context.Context, opts Options, messages []llm.Message) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		l := newQueryLoop(ctx, opts, messages, yield)
		t := l.run()
		l.emit(Event{Kind: KindResult, Terminal: t})
	}
}

// Run 消费 Query 的全部事件并返回终态；onEvent 可为 nil。
func Run(ctx context.Context, opts Options, messages []llm.Message, onEvent func(Event)) *Terminal {
	var t *Terminal
	for ev := range Query(ctx, opts, messages) {
		if onEvent != nil {
			onEvent(ev)
		}
		if ev.Kind == KindResult {
			t = ev.Terminal
		}
	}
	return t
}

type queryLoop struct {
	ctx           context.Context
	opts          Options
	registry      *tool.Registry
	deferredNames map[string]bool
	activeTools   []tool.CoreTool
	messages      []llm.Message
	usage         llm.Usage
	yield         func(Event) bool
	stopped       bool

	turn           int
	settling       bool
	settleReason   TerminalReason
	settleTurns    int
	stopHookActive bool
}

func newQueryLoop(ctx context.Context, opts Options, messages []llm.Message, yield func(Event) bool) *queryLoop {
	reg := tool.NewRegistry()
	visible := map[string]bool{}
	var names []string
	for _, t := range opts.Tools {
		if t == nil {
			continue
		}
		if !visible[t.Name()] {
			names = append(names, t.Name())
		}
		visible[t.Name()] = true
		reg.Register(t)
	}
	deferred := map[string]bool{}
	for _, t := range opts.DeferredTools {
		if t == nil || visible[t.Name()] {
			continue
		}
		deferred[t.Name()] = true
		reg.Register(t)
	}
	active := make([]tool.CoreTool, 0, len(names))
	for _, n := range names {
		t, _ := reg.Get(n)
		active = append(active, t)
	}
	return &queryLoop{
		ctx:           ctx,
		opts:          opts,
		registry:      reg,
		deferredNames: deferred,
		activeTools:   active,
		messages:      append([]llm.Message(nil), messages...),
		yield:         yield,
	}
}

func (l *queryLoop) emit(ev Event) bool {
	if l.stopped {
		return false
	}
	if !l.yield(ev) {
		l.stopped = true
	}
	return !l.stopped
}

func (l *queryLoop) run() *Terminal {
	if l.opts.Provider == nil {
		return l.finish(ReasonModelError, errors.New("harness: nil provider"))
	}
	start := time.Now()

	// 工具执行 Context：MaxDuration>0 时带墙钟截止；LLM 请求本身使用父 l.ctx。
	var (
		toolCtx    context.Context
		toolCancel context.CancelFunc
	)
	if l.opts.MaxDuration > 0 {
		toolCtx, toolCancel = context.WithDeadline(l.ctx, start.Add(l.opts.MaxDuration))
	} else {
		toolCtx, toolCancel = context.WithCancel(l.ctx)
	}
	defer func() {
		if toolCancel != nil {
			toolCancel()
		}
	}()

	l.turn = 1
	for {
		if l.ctx.Err() != nil {
			return l.finish(ReasonAbortedStreaming, context.Cause(l.ctx))
		}
		msg, err := l.callModel()
		if l.stopped {
			return l.finish(ReasonAbortedStreaming, errConsumerStopped)
		}
		if err != nil {
			if l.ctx.Err() != nil {
				return l.finish(ReasonAbortedStreaming, context.Cause(l.ctx))
			}
			return l.finish(ReasonModelError, err)
		}
		l.messages = append(l.messages, msg)

		uses := msg.ToolUses()
		if len(uses) == 0 {
			// 收尾阶段自然停止：返回原始预算原因，不走 Stop Hook 轻推。
			if l.settling {
				return l.finish(l.settleReason, nil)
			}
			// stopHookActive：同一连续空转链最多一次续跑指令，真实工具回合后才重置。
			if l.opts.Hooks != nil && !l.stopHookActive {
				prevent, blocking, hookMsg := l.opts.Hooks.Stop(l.ctx, l.snapshot())
				if prevent {
					t := l.finish(ReasonStopHook, nil)
					t.Text = hookMsg
					return t
				}
				if len(blocking) > 0 {
					l.stopHookActive = true
					l.appendUserText(strings.Join(blocking, "\n"))
					continue
				}
			}
			return l.finish(ReasonCompleted, nil)
		}

		l.stopHookActive = false
		results := l.runTools(toolCtx, uses)
		l.messages = append(l.messages, llm.Message{Role: llm.RoleUser, Content: results})
		if l.stopped {
			return l.finish(ReasonAbortedTools, errConsumerStopped)
		}
		// 父 Context 已取消（暂停/删除/Kill）：直接结束，不保证结算。
		if l.ctx.Err() != nil {
			return l.finish(ReasonAbortedTools, context.Cause(l.ctx))
		}
		// 工具 Context 因自身 deadline 结束而父 Context 存活。
		if toolCtx.Err() != nil {
			if l.settling {
				return l.finish(l.settleReason, nil) // 写回预算耗尽
			}
			if !l.beginSettlement(ReasonTimeout, &toolCtx, &toolCancel) {
				return l.finish(ReasonTimeout, nil)
			}
			continue
		}
		if l.settling {
			l.settleTurns++
			if l.settleTurns >= l.settleMaxTurns() {
				return l.finish(l.settleReason, nil)
			}
			continue
		}

		// 回合边界预算检查。
		next := l.turn + 1
		var reason TerminalReason
		switch {
		case l.opts.MaxTurns > 0 && next > l.opts.MaxTurns:
			reason = ReasonMaxTurns
		case l.opts.MaxDuration > 0 && time.Since(start) >= l.opts.MaxDuration:
			reason = ReasonTimeout
		}
		if reason != "" {
			if !l.beginSettlement(reason, &toolCtx, &toolCancel) {
				return l.finish(reason, nil)
			}
			continue
		}
		l.turn = next
	}
}

// beginSettlement 进入收尾写回阶段；未配置 Settlement 时返回 false。
func (l *queryLoop) beginSettlement(reason TerminalReason, toolCtx *context.Context, toolCancel *context.CancelFunc) bool {
	st := l.opts.Settlement
	if st == nil {
		return false
	}
	// Patch ①：无论从工具超时中断进入，还是从回合边界（MaxTurns / MaxDuration）进入，
	// 一律先释放旧 toolCtx，再基于存活父 Context 派生独立写回预算。
	if *toolCancel != nil {
		(*toolCancel)()
		*toolCancel = nil
	}
	budget := st.WritebackTimeout
	if budget <= 0 {
		budget = defaultSettlementWritebackTimeout
	}
	*toolCtx, *toolCancel = context.WithTimeout(l.ctx, budget)

	l.settleReason = reason
	l.settling = true
	l.settleTurns = 0
	prompt := st.Prompt
	if byReason, ok := st.PromptByReason[reason]; ok && byReason != "" {
		prompt = byReason
	}
	if prompt != "" {
		l.appendUserText(prompt)
	}
	// Patch ②：模型可见 Schema 与 Dispatch 闸门使用同一规则收缩。
	l.activeTools = filterSettlementTools(l.activeTools, st)
	return true
}

func (l *queryLoop) settleMaxTurns() int {
	if n := l.opts.Settlement.MaxTurns; n > 0 {
		return n
	}
	return defaultSettlementMaxTurns
}

// appendUserText 追加 user 文本；末条已是 user（如 tool_result）时并入同一消息，避免连续同角色消息。
func (l *queryLoop) appendUserText(text string) {
	block := llm.ContentBlock{Type: llm.BlockText, Text: text}
	if n := len(l.messages); n > 0 && l.messages[n-1].Role == llm.RoleUser {
		last := &l.messages[n-1]
		last.Content = append(append([]llm.ContentBlock(nil), last.Content...), block)
		return
	}
	l.messages = append(l.messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{block}})
}

func (l *queryLoop) snapshot() []llm.Message {
	return append([]llm.Message(nil), l.messages...)
}

func (l *queryLoop) toolSpecs() []llm.ToolSpec {
	specs := make([]llm.ToolSpec, 0, len(l.activeTools))
	for _, t := range l.activeTools {
		desc := t.Prompt()
		if desc == "" {
			desc = t.Description()
		}
		specs = append(specs, llm.ToolSpec{Name: t.Name(), Description: desc, InputSchema: t.InputSchema()})
	}
	return specs
}

func (l *queryLoop) callModel() (llm.Message, error) {
	req := llm.CompletionRequest{
		Model:           l.opts.Model,
		System:          l.opts.SystemPrompt,
		Messages:        l.snapshot(),
		Tools:           l.toolSpecs(),
		MaxTokens:       l.opts.MaxTokens,
		DynamicBoundary: l.opts.DynamicBoundary,
	}

	var (
		msg   llm.Message
		usage llm.Usage
	)
	if l.opts.NonStreaming {
		m, _, u, err := l.opts.Provider.Complete(llm.WithSessionID(l.ctx, l.opts.SessionID), req)
		if err != nil {
			return llm.Message{}, err
		}
		m.Role = llm.RoleAssistant
		for _, b := range m.Content {
			ok := true
			switch b.Type {
			case llm.BlockText:
				ok = b.Text == "" || l.emit(Event{Kind: KindText, Text: b.Text})
			case llm.BlockThinking:
				ok = b.Text == "" || l.emit(Event{Kind: KindThinking, Text: b.Text})
			}
			if !ok {
				return llm.Message{}, errConsumerStopped
			}
		}
		msg, usage = m, u
	} else {
		acc := llm.NewAccumulator()
		for ev, err := range l.opts.Provider.Stream(llm.WithSessionID(l.ctx, l.opts.SessionID), req) {
			if err != nil {
				return llm.Message{}, err
			}
			acc.Add(ev)
			ok := true
			switch ev.Type {
			case llm.SETextDelta:
				ok = ev.Text == "" || l.emit(Event{Kind: KindText, Text: ev.Text})
			case llm.SEThinkingDelta:
				ok = ev.Text == "" || l.emit(Event{Kind: KindThinking, Text: ev.Text})
			}
			if !ok {
				return llm.Message{}, errConsumerStopped
			}
		}
		msg, _, usage = acc.Result()
	}

	for _, b := range msg.ToolUses() {
		if !l.emit(Event{Kind: KindToolUse, ToolUse: &llm.ToolUse{ID: b.ID, Name: b.Name, Input: b.Input}}) {
			return llm.Message{}, errConsumerStopped
		}
	}
	l.usage.Add(usage)
	u := usage
	if !l.emit(Event{Kind: KindUsage, Usage: &u}) {
		return llm.Message{}, errConsumerStopped
	}
	return msg, nil
}

func (l *queryLoop) finish(reason TerminalReason, err error) *Terminal {
	t := &Terminal{Reason: reason, Messages: l.snapshot(), Usage: l.usage, Err: err}
	for i := len(l.messages) - 1; i >= 0; i-- {
		if l.messages[i].Role == llm.RoleAssistant {
			t.Text = l.messages[i].Text()
			break
		}
	}
	return t
}
