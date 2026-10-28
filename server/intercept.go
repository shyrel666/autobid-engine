package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/autobid/autobid-engine/intercept"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/llmrec"
)

// judgeAgentKey 是裁判模型的 usage lane / 角色绑定键（agent_llm_bindings.agent_key 允许 'judge'）。
const judgeAgentKey = "judge"

const defaultJudgePrompt = `你是投标推演引擎的工具调用安全裁判。根据给出的工具名与入参判断该调用应当：
- allow：只读或影响可控，可直接执行；
- deny：越权、破坏性或与投标核验无关的外部副作用；
- ask：存在不确定的外部副作用，需要人工审批。
只输出一个 JSON 对象：{"action":"allow|deny|ask","reason":"一句话理由"}，不要输出其他内容。`

// Reviewer 返回 intercept.Reviewer（§6.4）：选择裁判 Profile（profileID=0 时依次取 judge 角色绑定、全局池），
// 以 judge lane 计量，执行单次无工具的模型分类（MaxTokens=1024），解析 JSON 裁决。
// 内部 llm.CompletionRequest 不暴露 temperature / thinking 开关，此处沿用 Profile 默认设置。
func (s *LLMService) Reviewer() intercept.Reviewer {
	return func(ctx context.Context, profileID int64, prompt string, in intercept.ReviewInput) (intercept.Decision, error) {
		prov, pid, err := s.judgeProvider(ctx, profileID)
		if err != nil {
			return intercept.Decision{}, err
		}
		if strings.TrimSpace(prompt) == "" {
			prompt = defaultJudgePrompt
		}
		payload, _ := json.Marshal(in)
		ctx = llmrec.WithAttribution(ctx, llmrec.Attribution{TaskID: in.TaskID, Worker: judgeAgentKey})
		req := llm.CompletionRequest{
			System:    prompt,
			Messages:  []llm.Message{llm.UserText("待裁决的工具调用：\n" + string(payload))},
			MaxTokens: 1024,
		}
		acc := llm.NewAccumulator()
		for ev, err := range prov.Stream(ctx, req) {
			if err != nil {
				return intercept.Decision{}, err
			}
			acc.Add(ev)
		}
		msg, _, _ := acc.Result()
		dec, err := parseJudgeVerdict(msg.Text())
		dec.ProfileID = pid
		return dec, err
	}
}

func (s *LLMService) judgeProvider(ctx context.Context, profileID int64) (llm.Provider, int64, error) {
	if profileID == 0 {
		bound, err := s.pg.AgentLLMBinding(ctx, judgeAgentKey)
		if err != nil {
			return nil, 0, err
		}
		profileID = bound
	}
	if profileID > 0 {
		p, err := s.profileProvider(ctx, profileID)
		return p, profileID, err
	}
	pool, err := s.globalPool(ctx)
	return pool, 0, err
}

// parseJudgeVerdict 从模型文本中提取第一个 JSON 对象；无法解析时返回错误，由 Interceptor 按 FailAction 处理。
func parseJudgeVerdict(text string) (intercept.Decision, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return intercept.Decision{}, errors.New("judge: no JSON verdict in model output")
	}
	var v struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &v); err != nil {
		return intercept.Decision{}, fmt.Errorf("judge: decode verdict: %w", err)
	}
	return intercept.Decision{Action: strings.ToLower(strings.TrimSpace(v.Action)), Message: v.Reason}, nil
}
