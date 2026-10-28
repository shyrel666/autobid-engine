// Package llm 定义模型无关的消息、流事件与 Provider 契约。
package llm

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

type BlockType string

const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	// BlockRedactedThinking 的加密内容放在 Text；须原样回传。
	BlockRedactedThinking BlockType = "redacted_thinking"
)

// ContentBlock 是消息内容块；按 Type 使用对应字段。
// thinking / redacted_thinking 块必须原样随会话回传，不得改写（Provider 以签名校验会话前缀）。
type ContentBlock struct {
	Type BlockType `json:"type"`

	// BlockText / BlockThinking / BlockRedactedThinking；BlockToolResult 的结果正文也放在 Text。
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"` // thinking 签名，透传给支持的 Provider

	// BlockToolUse
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// BlockToolResult
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// Text 拼接全部 text 块（不含 thinking）。
func (m Message) Text() string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == BlockText {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// ToolUses 返回全部 tool_use 块，保持原顺序。
func (m Message) ToolUses() []ContentBlock {
	var out []ContentBlock
	for _, c := range m.Content {
		if c.Type == BlockToolUse {
			out = append(out, c)
		}
	}
	return out
}

func UserText(text string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{{Type: BlockText, Text: text}}}
}

// ToolUse 是流事件中携带的工具调用；Input 在 SEToolUseEnd 时为完整 JSON。
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
}

// ToolSpec 是下发给模型的工具 Schema。
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type CompletionRequest struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
	// DynamicBoundary 是 System 中静态前缀的字节长度，Provider 可据此设置缓存断点；0 表示不分段。
	DynamicBoundary int
}

const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
)

type EventType int

const (
	SEMessageStart     EventType = iota + 1 // 握手元事件，不构成可观察输出
	SETextDelta                             // Text 为增量
	SEThinkingDelta                         // Text 为增量
	SEToolUseStart                          // ToolUse.ID / ToolUse.Name
	SEToolUseDelta                          // Text 为参数 JSON 片段
	SEToolUseEnd                            // ToolUse 可携带完整 Input；为空时以累积片段为准
	SEMessageStop                           // Usage / StopReason
	SESignatureDelta                        // Text 为当前 thinking 块签名片段
	SERedactedThinking                      // Text 为整块加密 thinking
)

type StreamEvent struct {
	Type       EventType
	Text       string
	ToolUse    *ToolUse
	Usage      *Usage
	StopReason string
}

// Provider 是单个模型端点；Complete 返回 (消息, stop reason, 用量, 错误)。
type Provider interface {
	Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error)
	Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error]
}

// CommitsOutput 判定事件交付下游后是否构成不可撤回的可观察输出（§5.3 streamEventCommitsOutput）。
// 首次 commit 之前的事件可缓冲，流失败时可安全丢弃并重试。
func CommitsOutput(ev StreamEvent) bool {
	switch ev.Type {
	case SETextDelta, SEThinkingDelta:
		return ev.Text != ""
	case SEToolUseStart, SEToolUseDelta, SEToolUseEnd, SERedactedThinking:
		return true
	}
	return false
}

type sessionKey struct{}

// WithSessionID 把会话 ID 注入请求 Context，供计量等包装层归属调用。
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, id)
}

func SessionIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(sessionKey{}).(string)
	return s
}
