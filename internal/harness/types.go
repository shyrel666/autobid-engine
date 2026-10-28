// Package harness 是自研的 Agent 查询循环内核（norma@v0.4.3 等价层），内建 v2 Patch ①②③。
package harness

import (
	"context"
	"time"

	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/permission"
	"github.com/autobid/autobid-engine/internal/tool"
)

type TerminalReason string

const (
	ReasonCompleted        TerminalReason = "completed"
	ReasonMaxTurns         TerminalReason = "max_turns"
	ReasonTimeout          TerminalReason = "timeout"
	ReasonAbortedStreaming TerminalReason = "aborted_streaming"
	ReasonAbortedTools     TerminalReason = "aborted_tools"
	ReasonStopHook         TerminalReason = "stop_hook"
	// ReasonModelError：父 Context 存活时模型请求失败；上层 runIntent 据此决定会话级续跑。
	ReasonModelError TerminalReason = "model_error"
)

const (
	defaultSettlementWritebackTimeout = 30 * time.Second
	defaultSettlementMaxTurns         = 10
)

// Settlement 配置预算耗尽后的有限写回阶段（§3）。
type Settlement struct {
	Prompt         string
	MaxTurns       int // <=0 取默认 10
	DisabledTools  []string
	PromptByReason map[TerminalReason]string

	// Patch ②：收尾写回白名单；非空时收尾阶段只允许列表内工具（Schema 与 Dispatch 双重收缩）。
	AllowedTools []string
	// Patch ①：收尾阶段工具执行的独立预算，基于存活父 Context 派生；<=0 取默认 30s。
	WritebackTimeout time.Duration
}

type HookRunner interface {
	PreToolUse(ctx context.Context, name string, input []byte) (block bool, message string, updated []byte)
	PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool)
	Stop(ctx context.Context, messages []llm.Message) (prevent bool, blockingErrors []string, message string)
}

type Options struct {
	Provider llm.Provider
	Model    string

	SystemPrompt    string
	DynamicBoundary int

	// Tools 的 Schema 下发给模型。DeferredTools 进入注册表但不下发 Schema，
	// 按名直调须已在 UnlockSet 中解锁（Patch ③）；Schema 经 tool.NewSearchExtraTools 按需披露。
	Tools         []tool.CoreTool
	DeferredTools []tool.CoreTool
	UnlockSet     *tool.UnlockSet

	MaxTurns    int           // <=0 不限
	MaxDuration time.Duration // <=0 不限；只约束回合边界检查与工具执行 Context
	MaxTokens   int

	Hooks        HookRunner
	Settlement   *Settlement
	Permission   permission.Context
	SessionID    string
	NonStreaming bool
}

type EventKind int

const (
	KindText EventKind = iota + 1
	KindThinking
	KindToolUse
	KindToolResult
	KindUsage
	KindResult
)

type ToolResultEvent struct {
	ToolUseID string
	Name      string
	Content   string
	IsError   bool
}

type Event struct {
	Kind       EventKind
	Text       string           // KindText / KindThinking 增量
	ToolUse    *llm.ToolUse     // KindToolUse（参数完整）
	ToolResult *ToolResultEvent // KindToolResult
	Usage      *llm.Usage       // KindUsage（单次模型调用）
	Terminal   *Terminal        // KindResult
}

type Terminal struct {
	Reason   TerminalReason
	Text     string        // 最后一条 assistant 文本；ReasonStopHook 时为 Hook 消息
	Messages []llm.Message // 完整会话（不含 system）
	Usage    llm.Usage     // 本次运行累计
	Err      error
}
