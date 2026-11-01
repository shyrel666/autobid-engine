// Package openai 把 OpenAI 兼容的 Chat Completions 接口（OpenAI 及兼容网关/厂商）适配为 llm.Provider。
// 不做任何内部重试：重试、退避与切链统一由任务 LLM 运行时和 llmpool 负责。
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/internal/llm"
)

const (
	DefaultBaseURL = "https://api.openai.com/v1"
	providerName   = "openai"
	maxSSELine     = 16 << 20
)

type Config struct {
	APIKey  string
	BaseURL string // 默认 https://api.openai.com/v1；兼容端填其 /v1 根地址
	Model   string // 必填
	// MaxTokens>0 时发送 max_tokens；为 0 时不发送，由服务端决定。
	MaxTokens int
	// ReasoningEffort 为 reasoning_effort（low|medium|high）；为空时不发送（并非所有兼容端支持）。
	ReasoningEffort string
	ThinkingType    string
	MaxTokensField  string
	HTTPClient      *http.Client
	ExtraHeaders    map[string]string
}

type Provider struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) *Provider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{}
	}
	return &Provider{cfg: cfg, client: c}
}

// ---------------------------------------------------------------------------
// 请求编码
// ---------------------------------------------------------------------------

type chatRequest struct {
	Model               string            `json:"model"`
	Messages            []chatMessage     `json:"messages"`
	Tools               []chatTool        `json:"tools,omitempty"`
	Stream              bool              `json:"stream"`
	StreamOptions       *streamOptions    `json:"stream_options,omitempty"`
	MaxTokens           int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens int               `json:"max_completion_tokens,omitempty"`
	Thinking            map[string]string `json:"thinking,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

func strPtr(s string) *string { return &s }

// encodeMessages 把通用消息转换为 Chat Completions 消息：
// tool_result 拆为独立的 role=tool 消息；thinking / redacted_thinking 不回传（兼容端不接受其他模型的推理块）。
func encodeMessages(system string, in []llm.Message) []chatMessage {
	var out []chatMessage
	if system != "" {
		out = append(out, chatMessage{Role: "system", Content: strPtr(system)})
	}
	for _, m := range in {
		var text strings.Builder
		var calls []chatToolCall
		var toolMsgs []chatMessage
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				if text.Len() > 0 && b.Text != "" {
					text.WriteString("\n")
				}
				text.WriteString(b.Text)
			case llm.BlockToolUse:
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				calls = append(calls, chatToolCall{ID: b.ID, Type: "function", Function: chatFunctionCall{Name: b.Name, Arguments: args}})
			case llm.BlockToolResult:
				content := b.Text
				if b.IsError {
					content = "ERROR: " + content
				}
				toolMsgs = append(toolMsgs, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: strPtr(content)})
			}
		}
		switch m.Role {
		case llm.RoleAssistant:
			msg := chatMessage{Role: "assistant", ToolCalls: calls}
			if text.Len() > 0 || len(calls) == 0 {
				msg.Content = strPtr(text.String())
			}
			out = append(out, msg)
		default:
			// 工具结果必须紧跟在发起调用的 assistant 消息之后，附带的 user 文本放在其后。
			out = append(out, toolMsgs...)
			if text.Len() > 0 || len(toolMsgs) == 0 {
				out = append(out, chatMessage{Role: "user", Content: strPtr(text.String())})
			}
		}
	}
	return out
}

func encodeTools(specs []llm.ToolSpec) []chatTool {
	out := make([]chatTool, 0, len(specs))
	for _, s := range specs {
		params := s.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, chatTool{Type: "function", Function: chatFunction{Name: s.Name, Description: s.Description, Parameters: params}})
	}
	return out
}

func (p *Provider) buildRequest(ctx context.Context, req llm.CompletionRequest, stream bool) (*http.Request, error) {
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.cfg.MaxTokens
	}
	body := chatRequest{
		Model:           model,
		Messages:        encodeMessages(req.System, req.Messages),
		Tools:           encodeTools(req.Tools),
		MaxTokens:       maxTokens,
		ReasoningEffort: p.cfg.ReasoningEffort,
	}
	if p.cfg.MaxTokensField == "max_completion_tokens" {
		body.MaxTokens = 0
		body.MaxCompletionTokens = maxTokens
	}
	if p.cfg.ThinkingType != "" {
		body.Thinking = map[string]string{"type": p.cfg.ThinkingType}
	}
	if stream {
		body.Stream = true
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
	if p.cfg.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	for k, v := range p.cfg.ExtraHeaders {
		hr.Header.Set(k, v)
	}
	return hr, nil
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

type apiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

func (b apiErrorBody) typeAndMessage() (string, string) {
	typ, msg := b.Type, b.Message
	if code := fmt.Sprint(b.Code); b.Code != nil && code != "" {
		if typ == "" {
			typ = code
		}
		if !strings.Contains(msg, code) {
			msg += " (" + code + ")"
		}
	}
	return typ, msg
}

func httpError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	ae := &llm.APIError{Provider: providerName, StatusCode: resp.StatusCode, RequestID: resp.Header.Get("x-request-id")}
	var body struct {
		Error apiErrorBody `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && (body.Error.Message != "" || body.Error.Type != "") {
		ae.Type, ae.Message = body.Error.typeAndMessage()
	} else {
		ae.Message = strings.TrimSpace(string(raw))
	}
	if s := resp.Header.Get("retry-after"); s != "" {
		if sec, err := strconv.Atoi(s); err == nil {
			ae.RetryAfter = time.Duration(sec) * time.Second
		}
	}
	return ae
}

// streamError 映射流中途下发的 error 对象：明确的配额类映射为 429（可被配额分类器识别），其余按 500 处理。
func streamError(b apiErrorBody) error {
	typ, msg := b.typeAndMessage()
	status := http.StatusInternalServerError
	lower := strings.ToLower(typ + " " + msg)
	if strings.Contains(lower, "quota") || strings.Contains(lower, "rate_limit") {
		status = http.StatusTooManyRequests
	}
	return &llm.APIError{Provider: providerName, StatusCode: status, Type: typ, Message: msg}
}

func transportErr(ctx context.Context, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	return &llm.APIError{Provider: providerName, Message: err.Error(), Err: err}
}

func stopReason(finish string) (string, error) {
	switch finish {
	case "tool_calls", "function_call":
		return llm.StopToolUse, nil
	case "length":
		return llm.StopMaxTokens, nil
	case "content_filter":
		return "", &llm.APIError{Provider: providerName, StatusCode: http.StatusBadRequest, Type: "refusal", Message: "response blocked by content filter"}
	case "", "stop":
		return llm.StopEndTurn, nil
	}
	return finish, nil
}

type usageBody struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// DeepSeek 风格的缓存命中字段。
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
}

// toUsage：InputTokens 采用提示总数（与 Claude 口径不同，缓存命中部分另记 CacheReadTokens）。
func (u *usageBody) toUsage() llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	out := llm.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, CacheReadTokens: u.PromptCacheHitTokens}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	return out
}

// ---------------------------------------------------------------------------
// Complete
// ---------------------------------------------------------------------------

func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	hr, err := p.buildRequest(ctx, req, false)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	resp, err := p.client.Do(hr)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, transportErr(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return llm.Message{}, "", llm.Usage{}, httpError(resp)
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content          *string `json:"content"`
				ReasoningContent string  `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string           `json:"id"`
					Function chatFunctionCall `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *usageBody    `json:"usage"`
		Error *apiErrorBody `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return llm.Message{}, "", llm.Usage{}, transportErr(ctx, err)
	}
	if body.Error != nil {
		return llm.Message{}, "", llm.Usage{}, streamError(*body.Error)
	}
	if len(body.Choices) == 0 {
		return llm.Message{}, "", llm.Usage{}, &llm.APIError{Provider: providerName, StatusCode: http.StatusBadGateway, Message: "response has no choices"}
	}
	c := body.Choices[0]
	stop, err := stopReason(c.FinishReason)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	msg := llm.Message{Role: llm.RoleAssistant}
	if c.Message.ReasoningContent != "" {
		msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockThinking, Text: c.Message.ReasoningContent})
	}
	if c.Message.Content != nil && *c.Message.Content != "" {
		msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockText, Text: *c.Message.Content})
	}
	for _, tc := range c.Message.ToolCalls {
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockToolUse, ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args)})
	}
	return msg, stop, body.Usage.toUsage(), nil
}

// ---------------------------------------------------------------------------
// Stream
// ---------------------------------------------------------------------------

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageBody    `json:"usage"`
	Error *apiErrorBody `json:"error"`
}

func (p *Provider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		hr, err := p.buildRequest(ctx, req, true)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		resp, err := p.client.Do(hr)
		if err != nil {
			yield(llm.StreamEvent{}, transportErr(ctx, err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			yield(llm.StreamEvent{}, httpError(resp))
			return
		}

		var (
			started  bool
			openTool = -1 // 当前未结束的工具调用 index
			usage    llm.Usage
			finish   string
		)
		emit := func(ev llm.StreamEvent) bool { return yield(ev, nil) }
		closeTool := func() bool {
			if openTool < 0 {
				return true
			}
			openTool = -1
			return emit(llm.StreamEvent{Type: llm.SEToolUseEnd})
		}

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), maxSSELine)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				break
			}
			var ch streamChunk
			if err := json.Unmarshal([]byte(data), &ch); err != nil {
				yield(llm.StreamEvent{}, &llm.APIError{Provider: providerName, StatusCode: http.StatusBadGateway, Message: "malformed stream chunk: " + err.Error()})
				return
			}
			if ch.Error != nil {
				yield(llm.StreamEvent{}, streamError(*ch.Error))
				return
			}
			if !started {
				started = true
				if !emit(llm.StreamEvent{Type: llm.SEMessageStart}) {
					return
				}
			}
			if ch.Usage != nil {
				usage = ch.Usage.toUsage()
			}
			for _, c := range ch.Choices {
				d := c.Delta
				if r := d.ReasoningContent + d.Reasoning; r != "" {
					if !closeTool() || !emit(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: r}) {
						return
					}
				}
				if d.Content != "" {
					if !closeTool() || !emit(llm.StreamEvent{Type: llm.SETextDelta, Text: d.Content}) {
						return
					}
				}
				for _, tc := range d.ToolCalls {
					if tc.Index != openTool {
						if !closeTool() {
							return
						}
						openTool = tc.Index
						if !emit(llm.StreamEvent{Type: llm.SEToolUseStart, ToolUse: &llm.ToolUse{ID: tc.ID, Name: tc.Function.Name}}) {
							return
						}
					}
					if tc.Function.Arguments != "" && !emit(llm.StreamEvent{Type: llm.SEToolUseDelta, Text: tc.Function.Arguments}) {
						return
					}
				}
				if c.FinishReason != nil && *c.FinishReason != "" {
					finish = *c.FinishReason
				}
			}
		}
		if err := sc.Err(); err != nil {
			yield(llm.StreamEvent{}, transportErr(ctx, err))
			return
		}
		if finish == "" {
			yield(llm.StreamEvent{}, &llm.APIError{Provider: providerName, StatusCode: http.StatusBadGateway, Message: "stream ended before finish_reason"})
			return
		}
		if !closeTool() {
			return
		}
		stop, err := stopReason(finish)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop, Usage: &usage, StopReason: stop}, nil)
	}
}
