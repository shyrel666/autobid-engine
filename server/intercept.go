package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/autobid/autobid-engine/db"
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
// 以 judge lane 计量，输出上限随模型配置，执行无工具的模型分类并解析 JSON 裁决。
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
			System:   prompt,
			Messages: []llm.Message{llm.UserText("待裁决的工具调用：\n" + string(payload))},
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
		p, err := s.boundProvider(ctx, profileID)
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

// ---------------------------------------------------------------------------
// 拦截规则、受控工具范围、AI 裁判配置与全状态审批记录 HTTP 控制面
// ---------------------------------------------------------------------------

func validateInterceptRule(r *db.InterceptRule) error {
	r.Name = strings.TrimSpace(r.Name)
	r.Pattern = strings.TrimSpace(r.Pattern)
	if r.Name == "" {
		return badRequest("rule name is required")
	}
	if r.Pattern == "" {
		return badRequest("rule pattern is required")
	}
	if r.MatchTarget == "" {
		r.MatchTarget = "tool_name"
	}
	if r.MatchTarget != "tool_name" && r.MatchTarget != "tool_input" {
		return badRequest("match_target must be tool_name or tool_input")
	}
	if r.MatchType == "" {
		r.MatchType = "string"
	}
	if r.MatchType != "string" && r.MatchType != "regex" {
		return badRequest("match_type must be string or regex")
	}
	if r.MatchType == "regex" {
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return badRequest("invalid regex pattern: %v", err)
		}
	}
	if r.Action != "allow" && r.Action != "deny" && r.Action != "ask" {
		return badRequest("action must be allow, deny or ask")
	}
	if r.TimeoutAction == "" {
		r.TimeoutAction = "deny"
	}
	if r.TimeoutAction != "allow" && r.TimeoutAction != "deny" {
		return badRequest("timeout_action must be allow or deny")
	}
	if r.TimeoutSeconds <= 0 {
		r.TimeoutSeconds = 60
	}
	return nil
}

func (a *API) listInterceptRules(w http.ResponseWriter, r *http.Request) {
	rules, err := a.pg.ListInterceptRules(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func (a *API) createInterceptRule(w http.ResponseWriter, r *http.Request) {
	var rule db.InterceptRule
	if err := decodeBody(r, &rule); err != nil {
		fail(w, err)
		return
	}
	if err := validateInterceptRule(&rule); err != nil {
		fail(w, err)
		return
	}
	id, err := a.pg.CreateInterceptRule(r.Context(), rule)
	if err != nil {
		fail(w, err)
		return
	}
	rule.ID = id
	if a.ic != nil {
		a.ic.Invalidate()
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (a *API) updateInterceptRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "ruleID")
	if err != nil {
		fail(w, err)
		return
	}
	var payload map[string]any
	if err := decodeBody(r, &payload); err != nil {
		fail(w, err)
		return
	}
	// 仅传 enabled 时走快捷开关更新
	if len(payload) == 1 {
		if en, ok := payload["enabled"].(bool); ok {
			if err := a.pg.SetInterceptRuleEnabled(r.Context(), id, en); err != nil {
				fail(w, err)
				return
			}
			if a.ic != nil {
				a.ic.Invalidate()
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": en})
			return
		}
	}
	raw, _ := json.Marshal(payload)
	var rule db.InterceptRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		fail(w, badRequest("%v", err))
		return
	}
	rule.ID = id
	if err := validateInterceptRule(&rule); err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.UpdateInterceptRule(r.Context(), rule); err != nil {
		fail(w, err)
		return
	}
	if a.ic != nil {
		a.ic.Invalidate()
	}
	writeJSON(w, http.StatusOK, rule)
}

func (a *API) deleteInterceptRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "ruleID")
	if err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.DeleteInterceptRule(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	if a.ic != nil {
		a.ic.Invalidate()
	}
	w.WriteHeader(http.StatusNoContent)
}

func normalizeJudgeConfig(raw json.RawMessage) intercept.JudgeConfig {
	var jc intercept.JudgeConfig
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &jc)
	}
	if jc.TimeoutSeconds <= 0 {
		jc.TimeoutSeconds = 15
	}
	if jc.FailAction != "deny" && jc.FailAction != "ask" {
		jc.FailAction = "allow"
	}
	if jc.AskTimeoutSeconds <= 0 {
		jc.AskTimeoutSeconds = 300
	}
	if jc.AskTimeoutAction != "allow" {
		jc.AskTimeoutAction = "deny"
	}
	if strings.TrimSpace(jc.Prompt) == "" {
		jc.Prompt = defaultJudgePrompt
	}
	return jc
}

func (a *API) getInterceptConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.pg.GetInterceptConfig(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	tools := cfg.EnabledTools
	if tools == nil {
		tools = append([]string(nil), intercept.DefaultEnabledTools...)
	}
	jc := normalizeJudgeConfig(cfg.Judge)
	usage, _ := a.pg.JudgeUsage(r.Context(), 30)
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled_tools":  tools,
		"default_tools":  intercept.DefaultEnabledTools,
		"judge":          jc,
		"default_prompt": defaultJudgePrompt,
		"judge_usage":    usage,
	})
}

func (a *API) putInterceptConfig(w http.ResponseWriter, r *http.Request) {
	cur, err := a.pg.GetInterceptConfig(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		EnabledTools *[]string              `json:"enabled_tools"`
		ResetTools   bool                   `json:"reset_tools"`
		Judge        *intercept.JudgeConfig `json:"judge"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.ResetTools {
		cur.EnabledTools = nil
	} else if req.EnabledTools != nil {
		cur.EnabledTools = *req.EnabledTools
	}
	if req.Judge != nil {
		jc := *req.Judge
		if jc.TimeoutSeconds <= 0 {
			jc.TimeoutSeconds = 15
		}
		if jc.FailAction != "deny" && jc.FailAction != "ask" {
			jc.FailAction = "allow"
		}
		if jc.AskTimeoutSeconds <= 0 {
			jc.AskTimeoutSeconds = 300
		}
		if jc.AskTimeoutAction != "allow" {
			jc.AskTimeoutAction = "deny"
		}
		raw, err := json.Marshal(jc)
		if err != nil {
			fail(w, err)
			return
		}
		cur.Judge = raw
	}
	if err := a.pg.SaveInterceptConfig(r.Context(), cur); err != nil {
		fail(w, err)
		return
	}
	if a.ic != nil {
		a.ic.Invalidate()
	}
	a.getInterceptConfig(w, r)
}

func (a *API) listInterceptApprovals(w http.ResponseWriter, r *http.Request) {
	var f db.InterceptApprovalFilter
	if v := r.URL.Query().Get("task_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			fail(w, badRequest("invalid task_id"))
			return
		}
		f.TaskID = id
	}
	f.Status = strings.TrimSpace(r.URL.Query().Get("status"))
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Offset = n
		}
	}
	if id := identityFrom(r.Context()); id != nil && !id.Admin {
		f.MemberUserID = id.UserID
	}
	items, total, err := a.pg.ListInterceptApprovals(r.Context(), f)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}
