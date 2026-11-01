// Package anthropic 把 Claude Messages API（官方 Go SDK）适配为 llm.Provider。
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/autobid/autobid-engine/internal/llm"
)

const (
	DefaultModel = "claude-opus-5-5"
	providerName = "anthropic"

	defaultStreamMaxTokens   = 64000
	defaultCompleteMaxTokens = 16000

	refusalFallbackBeta = "server-side-fallback-2026-07-01"
)

// 支持 `fallbacks: "default"` 服务端拒答回退的模型（Claude API）。
var defaultFallbackModels = map[string]bool{
	"claude-fable-5-1":  true,
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-sonnet-5-5": true,
}

type Config struct {
	APIKey  string // 为空时 SDK 按 ANTHROPIC_API_KEY 等默认来源解析凭据
	BaseURL string
	Model   string // 默认 claude-opus-5-5
	// MaxTokens 为单次响应上限；<=0 时流式取 64000、非流式取 16000。
	MaxTokens int64
	// Effort 为 output_config.effort（low|medium|high|xhigh|max）；为空时不发送，使用模型默认。
	Effort string
	// ThinkingDisplay 为 adaptive thinking 的 display（summarized|omitted）；为空时不发送 thinking 参数。
	ThinkingDisplay string
	ThinkingType    string
	// DisableRefusalFallback 关闭默认开启的服务端拒答回退（fallbacks: "default"）。
	DisableRefusalFallback bool
	Timeout                time.Duration
	// HTTPClient 承载每 Profile 的代理、会话请求头与原始报文录制 Transport；为空时使用 SDK 默认客户端。
	HTTPClient *http.Client
}

type Provider struct {
	client sdk.Client
	cfg    Config
}

func New(cfg Config) *Provider {
	// 重试与退避完全交由任务 LLM 运行时（§5.2/5.3）与 llmpool（§5.6）：SDK 内部对 429/5xx 的退避
	// 会让一次逻辑调用变成多次计费且不可观测，也会拖慢配额耗尽后的切链。
	opts := []option.RequestOption{option.WithMaxRetries(0)}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.Timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(cfg.Timeout))
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	return &Provider{client: sdk.NewClient(opts...), cfg: cfg}
}

func (p *Provider) params(req llm.CompletionRequest, stream bool) (sdk.MessageNewParams, []option.RequestOption) {
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}
	maxTokens := int64(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = p.cfg.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = defaultCompleteMaxTokens
		if stream {
			maxTokens = defaultStreamMaxTokens
		}
	}
	params := sdk.MessageNewParams{
		Model:     sdk.Model(model),
		MaxTokens: maxTokens,
		System:    systemBlocks(req.System, req.DynamicBoundary),
		Messages:  messages(req.Messages),
		Tools:     tools(req.Tools, stream),
		// 自动缓存断点放在最后一个可缓存块上，配合 system 静态前缀断点覆盖多轮 Agent 循环。
		CacheControl: sdk.NewCacheControlEphemeralParam(),
	}
	if p.cfg.Effort != "" {
		params.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(p.cfg.Effort)}
	}
	if p.cfg.ThinkingDisplay != "" {
		adaptive := sdk.ThinkingConfigAdaptiveParam{Display: sdk.ThinkingConfigAdaptiveDisplay(p.cfg.ThinkingDisplay)}
		params.Thinking = sdk.ThinkingConfigParamUnion{OfAdaptive: &adaptive}
	}
	if p.cfg.ThinkingType != "" {
		thinking := map[string]any{"type": p.cfg.ThinkingType}
		if p.cfg.ThinkingType == "enabled" {
			thinking["budget_tokens"] = max(int64(1024), min(int64(8192), maxTokens/2))
		}
		params.SetExtraFields(map[string]any{"thinking": thinking})
	}
	var opts []option.RequestOption
	if !p.cfg.DisableRefusalFallback && defaultFallbackModels[model] {
		extra := map[string]any{"fallbacks": "default"}
		if p.cfg.ThinkingType != "" {
			thinking := map[string]any{"type": p.cfg.ThinkingType}
			if p.cfg.ThinkingType == "enabled" {
				thinking["budget_tokens"] = max(int64(1024), min(int64(8192), maxTokens/2))
			}
			extra["thinking"] = thinking
		}
		params.SetExtraFields(extra)
		opts = append(opts, option.WithHeaderAdd("anthropic-beta", refusalFallbackBeta))
	}
	return params, opts
}

// systemBlocks 在 DynamicBoundary 处切分 system：静态前缀加缓存断点，动态尾部不缓存。
func systemBlocks(system string, boundary int) []sdk.TextBlockParam {
	if system == "" {
		return nil
	}
	if boundary <= 0 || boundary >= len(system) {
		return []sdk.TextBlockParam{{Text: system, CacheControl: sdk.NewCacheControlEphemeralParam()}}
	}
	for boundary > 0 && !utf8.RuneStart(system[boundary]) {
		boundary--
	}
	blocks := []sdk.TextBlockParam{{Text: system[:boundary], CacheControl: sdk.NewCacheControlEphemeralParam()}}
	if rest := system[boundary:]; strings.TrimSpace(rest) != "" {
		blocks = append(blocks, sdk.TextBlockParam{Text: rest})
	}
	return blocks
}

const emptyPlaceholder = "(no output)"

func messages(in []llm.Message) []sdk.MessageParam {
	out := make([]sdk.MessageParam, 0, len(in))
	for _, m := range in {
		var blocks []sdk.ContentBlockParamUnion
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				if b.Text != "" {
					blocks = append(blocks, sdk.NewTextBlock(b.Text))
				}
			case llm.BlockThinking:
				// 无签名的 thinking 来自其他格式的模型（如 OpenAI 兼容端的 reasoning_content），Claude 不接受，跳过。
				if b.Signature != "" {
					blocks = append(blocks, sdk.NewThinkingBlock(b.Signature, b.Text))
				}
			case llm.BlockRedactedThinking:
				blocks = append(blocks, sdk.NewRedactedThinkingBlock(b.Text))
			case llm.BlockToolUse:
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, sdk.NewToolUseBlock(b.ID, input, b.Name))
			case llm.BlockToolResult:
				content := b.Text
				if content == "" {
					content = emptyPlaceholder
				}
				blocks = append(blocks, sdk.NewToolResultBlock(b.ToolUseID, content, b.IsError))
			}
		}
		if len(blocks) == 0 {
			blocks = append(blocks, sdk.NewTextBlock(emptyPlaceholder))
		}
		if m.Role == llm.RoleAssistant {
			out = append(out, sdk.NewAssistantMessage(blocks...))
		} else {
			out = append(out, sdk.NewUserMessage(blocks...))
		}
	}
	return out
}

func tools(specs []llm.ToolSpec, stream bool) []sdk.ToolUnionParam {
	out := make([]sdk.ToolUnionParam, 0, len(specs))
	for _, s := range specs {
		schema := sdk.ToolInputSchemaParam{Properties: s.InputSchema["properties"]}
		switch r := s.InputSchema["required"].(type) {
		case []string:
			schema.Required = r
		case []any:
			for _, v := range r {
				if name, ok := v.(string); ok {
					schema.Required = append(schema.Required, name)
				}
			}
		}
		for k, v := range s.InputSchema {
			if k != "type" && k != "properties" && k != "required" {
				if schema.ExtraFields == nil {
					schema.ExtraFields = map[string]any{}
				}
				schema.ExtraFields[k] = v
			}
		}
		t := sdk.ToolParam{Name: s.Name, InputSchema: schema}
		if s.Description != "" {
			t.Description = sdk.String(s.Description)
		}
		// 流式时让大参数随生成增量到达；API 不再代为校验，harness 在执行前做 JSON 与 Schema 校验。
		if stream {
			t.EagerInputStreaming = sdk.Bool(true)
		}
		out = append(out, sdk.ToolUnionParam{OfTool: &t})
	}
	return out
}

func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	params, opts := p.params(req, false)
	resp, err := p.client.Messages.New(ctx, params, opts...)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, wrapErr(ctx, err)
	}
	if resp.StopReason == sdk.StopReasonRefusal {
		return llm.Message{}, "", llm.Usage{}, refusalErr(string(resp.StopDetails.Category), resp.StopDetails.Explanation)
	}
	msg := llm.Message{Role: llm.RoleAssistant}
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockText, Text: b.Text})
		case sdk.ThinkingBlock:
			msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockThinking, Text: b.Thinking, Signature: b.Signature})
		case sdk.RedactedThinkingBlock:
			msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockRedactedThinking, Text: b.Data})
		case sdk.ToolUseBlock:
			msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockToolUse, ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	u := resp.Usage
	usage := llm.Usage{
		InputTokens: int(u.InputTokens), OutputTokens: int(u.OutputTokens),
		CacheReadTokens: int(u.CacheReadInputTokens), CacheWriteTokens: int(u.CacheCreationInputTokens),
	}
	return msg, string(resp.StopReason), usage, nil
}

func (p *Provider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		params, opts := p.params(req, true)
		stream := p.client.Messages.NewStreaming(ctx, params, opts...)
		defer stream.Close()

		var (
			usage     llm.Usage
			stop      string
			refusal   sdk.RefusalStopDetails
			openTools = map[int64]bool{}
		)
		emit := func(ev llm.StreamEvent) bool { return yield(ev, nil) }
		for stream.Next() {
			switch e := stream.Current().AsAny().(type) {
			case sdk.MessageStartEvent:
				u := e.Message.Usage
				usage.InputTokens = int(u.InputTokens)
				usage.CacheReadTokens = int(u.CacheReadInputTokens)
				usage.CacheWriteTokens = int(u.CacheCreationInputTokens)
				if !emit(llm.StreamEvent{Type: llm.SEMessageStart}) {
					return
				}
			case sdk.ContentBlockStartEvent:
				ok := true
				switch b := e.ContentBlock.AsAny().(type) {
				case sdk.TextBlock:
					ok = b.Text == "" || emit(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text})
				case sdk.ThinkingBlock:
					// 空文本的 thinking 增量标志一个新块开始（display=omitted 时正文恒为空，仅有签名）。
					ok = emit(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: b.Thinking})
					if ok && b.Signature != "" {
						ok = emit(llm.StreamEvent{Type: llm.SESignatureDelta, Text: b.Signature})
					}
				case sdk.RedactedThinkingBlock:
					ok = emit(llm.StreamEvent{Type: llm.SERedactedThinking, Text: b.Data})
				case sdk.ToolUseBlock:
					openTools[e.Index] = true
					ok = emit(llm.StreamEvent{Type: llm.SEToolUseStart, ToolUse: &llm.ToolUse{ID: b.ID, Name: b.Name}})
				}
				if !ok {
					return
				}
			case sdk.ContentBlockDeltaEvent:
				var ev llm.StreamEvent
				switch d := e.Delta.AsAny().(type) {
				case sdk.TextDelta:
					ev = llm.StreamEvent{Type: llm.SETextDelta, Text: d.Text}
				case sdk.ThinkingDelta:
					ev = llm.StreamEvent{Type: llm.SEThinkingDelta, Text: d.Thinking}
				case sdk.SignatureDelta:
					ev = llm.StreamEvent{Type: llm.SESignatureDelta, Text: d.Signature}
				case sdk.InputJSONDelta:
					ev = llm.StreamEvent{Type: llm.SEToolUseDelta, Text: d.PartialJSON}
				default:
					continue
				}
				if ev.Text == "" && ev.Type != llm.SEToolUseDelta {
					continue
				}
				if !emit(ev) {
					return
				}
			case sdk.ContentBlockStopEvent:
				if openTools[e.Index] {
					delete(openTools, e.Index)
					if !emit(llm.StreamEvent{Type: llm.SEToolUseEnd}) {
						return
					}
				}
			case sdk.MessageDeltaEvent:
				stop = string(e.Delta.StopReason)
				refusal = e.Delta.StopDetails
				usage.OutputTokens = int(e.Usage.OutputTokens)
				if n := int(e.Usage.InputTokens); n > usage.InputTokens {
					usage.InputTokens = n
				}
				if n := int(e.Usage.CacheReadInputTokens); n > usage.CacheReadTokens {
					usage.CacheReadTokens = n
				}
				if n := int(e.Usage.CacheCreationInputTokens); n > usage.CacheWriteTokens {
					usage.CacheWriteTokens = n
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(llm.StreamEvent{}, wrapErr(ctx, err))
			return
		}
		if stop == string(sdk.StopReasonRefusal) {
			yield(llm.StreamEvent{}, refusalErr(string(refusal.Category), refusal.Explanation))
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop, Usage: &usage, StopReason: stop}, nil)
	}
}

// refusalErr 把（回退后仍然）拒答映射为不可重试、不可换池的 400 类错误。
func refusalErr(category, explanation string) error {
	msg := "model declined the request"
	if category != "" {
		msg += " (" + category + ")"
	}
	if explanation != "" {
		msg += ": " + explanation
	}
	return &llm.APIError{Provider: providerName, StatusCode: http.StatusBadRequest, Type: "refusal", Message: msg}
}

// wrapErr 把 SDK 错误映射为 llm.APIError；Context 取消原样返回，供上层识别。
func wrapErr(ctx context.Context, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	var apierr *sdk.Error
	if !errors.As(err, &apierr) {
		return &llm.APIError{Provider: providerName, Message: err.Error(), Err: err}
	}
	out := &llm.APIError{
		Provider:   providerName,
		StatusCode: apierr.StatusCode,
		Type:       string(apierr.Type()),
		Message:    errorMessage(apierr),
		RequestID:  apierr.RequestID,
		Err:        err,
	}
	if apierr.Response != nil {
		if s := apierr.Response.Header.Get("retry-after"); s != "" {
			if sec, err := strconv.Atoi(s); err == nil {
				out.RetryAfter = time.Duration(sec) * time.Second
			}
		}
	}
	return out
}

func errorMessage(apierr *sdk.Error) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if raw := apierr.RawJSON(); raw != "" && json.Unmarshal([]byte(raw), &body) == nil && body.Error.Message != "" {
		return body.Error.Message
	}
	return apierr.Error()
}
