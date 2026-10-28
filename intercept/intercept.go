// Package intercept 实现工具调用拦截与人工审批挂起控制面（§6）：
// 规则表（priority DESC, id ASC，第一条命中即返回）→ 无命中时可选 LLM Judge → allow / deny / ask；
// ask 以 DB 状态为准、内存 Channel 只负责唤醒（骨架 4）。
package intercept

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
)

var (
	// ErrAlreadyDecided：审批已由另一方（人工 / 超时 / 取消）决定，本次决定未生效。
	ErrAlreadyDecided = errors.New("intercept: already decided")
	// ErrWaiterGone：等待方已失联（进程重启或工作取消），记录已按拒绝落库，批准不会执行。
	ErrWaiterGone = errors.New("intercept: waiter is gone; request recorded as denied")
	ErrNotFound   = errors.New("intercept: pending request not found")
)

const (
	ActionAllow = "allow"
	ActionDeny  = "deny"
	ActionAsk   = "ask"

	SourceRule    = "rule"
	SourceJudge   = "judge"
	SourceDefault = "default"
	SourceAsk     = "ask"
)

// DefaultEnabledTools 是默认启用拦截的工具集合（§6.2），可由 Options 或 intercept_config 覆盖。
// 未启用的工具不进入规则/Judge 链。
var DefaultEnabledTools = []string{"Bash", "WebFetch", "web_search", "session_open", "session_send", "Write", "Edit", "MultiEdit"}

// Store 是 Interceptor 依赖的持久化契约；*db.DB 实现它，单元测试可替换为内存实现。
type Store interface {
	EnabledInterceptRules(ctx context.Context) ([]db.InterceptRule, error)
	GetInterceptConfig(ctx context.Context) (db.InterceptConfig, error)
	CreateInterceptPending(ctx context.Context, p db.NewInterceptPending) (int64, error)
	GetInterceptPending(ctx context.Context, id int64) (*db.InterceptPendingRow, error)
	ResolveIntercept(ctx context.Context, id int64, status, effectiveAction, reason string, requireLiveWaiter bool) (bool, error)
	TouchInterceptWaiter(ctx context.Context, id int64, ttl time.Duration) (bool, error)
	CompleteIntercept(ctx context.Context, id int64, outcome, detail string) error
	ReapOrphanedIntercepts(ctx context.Context) ([]int64, error)
	InsertInterceptAudit(ctx context.Context, a db.InterceptAuditRow) error
}

// ---------------------------------------------------------------------------
// 执行上下文（附录 A.14 WithTask / WithAgent 模式）
// ---------------------------------------------------------------------------

type taskKey struct{}
type agentKey struct{}
type emitKey struct{}

type taskInfo struct{ taskID, intentID int64 }

// WithTask 标注拦截上下文所属任务与意图（intentID=0 表示 Planner 等非意图执行）。
func WithTask(ctx context.Context, taskID, intentID int64) context.Context {
	return context.WithValue(ctx, taskKey{}, taskInfo{taskID, intentID})
}

// WithAgent 标注发起工具调用的 Agent 名（如 work#1 / planner）。
func WithAgent(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, agentKey{}, name)
}

// WithEmit 注入审批等待活动的投递函数（关联任务的探索流）。
func WithEmit(ctx context.Context, emit func(db.Activity)) context.Context {
	return context.WithValue(ctx, emitKey{}, emit)
}

func taskInfoFromCtx(ctx context.Context) (taskID, intentID int64, agent string) {
	ti, _ := ctx.Value(taskKey{}).(taskInfo)
	agent, _ = ctx.Value(agentKey{}).(string)
	return ti.taskID, ti.intentID, agent
}

func taskEmitFromCtx(ctx context.Context) func(db.Activity) {
	f, _ := ctx.Value(emitKey{}).(func(db.Activity))
	return f
}

// ---------------------------------------------------------------------------
// 决策与 Judge 配置（附录 A.14 / A.20）
// ---------------------------------------------------------------------------

// Decision 是一次拦截判定。RuleID=0 表示非规则来源（Judge / 默认）。
type Decision struct {
	ModelInput       json.RawMessage
	ModelInputDigest string
	ModelFallback    bool
	RuleName         string
	ConfigDigest     string
	ProfileID        int64
	Action           string
	Message          string
	RuleID           int64
	TimeoutEnabled   bool
	TimeoutSeconds   int
	TimeoutAction    string
	Source           string
}

type JudgeConfig struct {
	Enabled           bool   `json:"enabled"`
	ProfileID         int64  `json:"profile_id"`
	Prompt            string `json:"prompt"`
	TimeoutSeconds    int    `json:"timeout_seconds"`
	FailAction        string `json:"fail_action"`
	AskTimeoutSeconds int    `json:"ask_timeout_seconds"`
	AskTimeoutAction  string `json:"ask_timeout_action"`
}

// withDefaults 补齐 §6.4 默认：Judge 超时 15s、失败 allow、ask 超时 300s 且超时 deny。
// 这些默认是当前实现策略，不是强制 fail-closed。
func (c JudgeConfig) withDefaults() JudgeConfig {
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = 15
	}
	if c.FailAction != ActionDeny && c.FailAction != ActionAsk {
		c.FailAction = ActionAllow
	}
	if c.AskTimeoutSeconds <= 0 {
		c.AskTimeoutSeconds = 300
	}
	if c.AskTimeoutAction != ActionAllow {
		c.AskTimeoutAction = ActionDeny
	}
	return c
}

// ReviewInput 是交给裁判模型的单次工具调用上下文。
type ReviewInput struct {
	TaskID   int64           `json:"task_id"`
	IntentID int64           `json:"intent_id,omitempty"`
	Agent    string          `json:"agent"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

// Reviewer 是无规则命中时的 LLM Judge（附录 A.20）；profileID=0 表示使用 judge 角色绑定/默认。
type Reviewer func(ctx context.Context, profileID int64, prompt string, input ReviewInput) (Decision, error)

// ---------------------------------------------------------------------------
// pendingManager（骨架 4）
// ---------------------------------------------------------------------------

type pendingManager struct {
	mu sync.Mutex
	ch map[int64]chan bool
}

func (p *pendingManager) add(id int64) chan bool {
	ch := make(chan bool, 1) // 容量 1：确保锁外发送永不阻塞 Decide 调用方
	p.mu.Lock()
	p.ch[id] = ch
	p.mu.Unlock()
	return ch
}

func (p *pendingManager) resolve(id int64, allowed bool) {
	p.mu.Lock()
	ch, ok := p.ch[id]
	delete(p.ch, id)
	p.mu.Unlock()
	if ok {
		ch <- allowed // 锁外投递，避免持锁等待接收协程调度
	}
}

func (p *pendingManager) remove(id int64) {
	p.mu.Lock()
	delete(p.ch, id)
	p.mu.Unlock()
}

func (p *pendingManager) waiting(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.ch[id]
	return ok
}

// ---------------------------------------------------------------------------
// Interceptor
// ---------------------------------------------------------------------------

type Options struct {
	// EnabledTools 非 nil 时覆盖 intercept_config 与默认集合。
	EnabledTools []string
	Reviewer     Reviewer
	// WaiterTTL 是 ask 等待方心跳有效期（默认 30s，每 TTL/3 续期）；过期的 pending 由回收器落为 denied。
	WaiterTTL time.Duration
	// Waiter 标识本进程（写入 intercept_pending.waiter，仅作审计）。
	Waiter string
	// OnActivity 是 Context 未注入 WithEmit 时的兜底活动投递。
	OnActivity func(db.Activity)
}

type compiledRule struct {
	db.InterceptRule
	re *regexp.Regexp
}

type Interceptor struct {
	db           Store
	mu           sync.RWMutex
	loaded       bool
	cached       []compiledRule
	enabledTools map[string]bool
	judge        JudgeConfig
	pending      *pendingManager
	reviewer     Reviewer
	opts         Options
}

func New(store Store, opts Options) *Interceptor {
	if opts.WaiterTTL <= 0 {
		opts.WaiterTTL = 30 * time.Second
	}
	return &Interceptor{
		db:       store,
		pending:  &pendingManager{ch: map[int64]chan bool{}},
		reviewer: opts.Reviewer,
		opts:     opts,
	}
}

// Invalidate 清空规则与配置缓存，下一次调用重新加载（§6.1）。
func (i *Interceptor) Invalidate() {
	i.mu.Lock()
	i.loaded, i.cached, i.enabledTools, i.judge = false, nil, nil, JudgeConfig{}
	i.mu.Unlock()
}

type snapshot struct {
	rules   []compiledRule
	enabled map[string]bool
	judge   JudgeConfig
}

// rules 读锁检查、写锁重查后加载（§6.1）。正则编译失败的规则被跳过。
func (i *Interceptor) rules(ctx context.Context) (snapshot, error) {
	i.mu.RLock()
	if i.loaded {
		s := snapshot{i.cached, i.enabledTools, i.judge}
		i.mu.RUnlock()
		return s, nil
	}
	i.mu.RUnlock()

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.loaded {
		return snapshot{i.cached, i.enabledTools, i.judge}, nil
	}
	rows, err := i.db.EnabledInterceptRules(ctx)
	if err != nil {
		return snapshot{}, err
	}
	cfg, err := i.db.GetInterceptConfig(ctx)
	if err != nil {
		return snapshot{}, err
	}
	cached := make([]compiledRule, 0, len(rows))
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		cr := compiledRule{InterceptRule: r}
		if r.MatchType == "regex" {
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				continue
			}
			cr.re = re
		}
		cached = append(cached, cr)
	}
	names := DefaultEnabledTools
	if cfg.EnabledTools != nil {
		names = cfg.EnabledTools
	}
	if i.opts.EnabledTools != nil {
		names = i.opts.EnabledTools
	}
	enabled := make(map[string]bool, len(names))
	for _, n := range names {
		enabled[n] = true
	}
	var judge JudgeConfig
	if len(cfg.Judge) > 0 {
		_ = json.Unmarshal(cfg.Judge, &judge)
	}
	i.cached, i.enabledTools, i.judge, i.loaded = cached, enabled, judge.withDefaults(), true
	return snapshot{i.cached, i.enabledTools, i.judge}, nil
}

// ruleMatches：match_target 选择工具名或原始 JSON 入参文本；string 为包含匹配，regex 用预编译表达式（§6.1）。
func ruleMatches(r compiledRule, toolName string, input []byte) bool {
	if r.ToolFilter != "" && r.ToolFilter != toolName {
		return false
	}
	subject := toolName
	if r.MatchTarget == "tool_input" {
		subject = string(input)
	}
	if r.MatchType == "regex" {
		return r.re != nil && r.re.MatchString(subject)
	}
	return strings.Contains(subject, r.Pattern)
}

// matchRule 返回按优先级第一条命中的规则（包括 allow：高优先级 allow 截断后续 deny/ask）。
func matchRule(rules []compiledRule, toolName string, input []byte) (compiledRule, bool) {
	for _, r := range rules {
		if ruleMatches(r, toolName, input) {
			return r, true
		}
	}
	return compiledRule{}, false
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func normalizeAction(a string) (string, bool) {
	switch a = strings.ToLower(strings.TrimSpace(a)); a {
	case ActionAllow, ActionDeny, ActionAsk:
		return a, true
	}
	return "", false
}

// Evaluate 对一次工具调用给出判定；enabled=false 表示该工具未启用拦截（不进入规则/Judge 链）。
// 规则/配置加载失败时对启用拦截的工具 fail-closed（deny），不同于蓝图中“表现为未启用”的现状。
func (i *Interceptor) Evaluate(ctx context.Context, toolName string, input []byte) (dec Decision, enabled bool) {
	snap, err := i.rules(ctx)
	if err != nil {
		enabled := i.opts.EnabledTools
		if enabled == nil {
			enabled = DefaultEnabledTools
		}
		for _, n := range enabled {
			if n == toolName {
				return Decision{Action: ActionDeny, Source: SourceDefault, Message: "拦截规则加载失败，按拒绝处理：" + err.Error()}, true
			}
		}
		return Decision{}, false
	}
	if !snap.enabled[toolName] {
		return Decision{}, false
	}
	if r, ok := matchRule(snap.rules, toolName, input); ok {
		return Decision{
			RuleID: r.ID, RuleName: r.Name, Action: r.Action, Message: r.Message, Source: SourceRule,
			TimeoutEnabled: r.TimeoutEnabled, TimeoutSeconds: r.TimeoutSeconds, TimeoutAction: r.TimeoutAction,
		}, true
	}
	if snap.judge.Enabled && i.reviewer != nil {
		return i.runJudge(ctx, snap.judge, toolName, input), true
	}
	return Decision{Action: ActionAllow, Source: SourceDefault}, true
}

// runJudge（§6.4）：入参不完整直接 ask；否则带超时调用 Reviewer，错误或不可解析动作按 FailAction。
func (i *Interceptor) runJudge(ctx context.Context, cfg JudgeConfig, toolName string, input []byte) Decision {
	taskID, intentID, agent := taskInfoFromCtx(ctx)
	cfgRaw, _ := json.Marshal(cfg)
	dec := Decision{
		Source: SourceJudge, ProfileID: cfg.ProfileID, ConfigDigest: digest(cfgRaw),
		ModelInput: json.RawMessage(input), ModelInputDigest: digest(input),
		TimeoutEnabled: true, TimeoutSeconds: cfg.AskTimeoutSeconds, TimeoutAction: cfg.AskTimeoutAction,
	}
	if !json.Valid(input) {
		dec.Action, dec.Message = ActionAsk, "工具入参不完整，转人工审批"
		return dec
	}
	jctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	out, err := i.reviewer(jctx, cfg.ProfileID, cfg.Prompt, ReviewInput{
		TaskID: taskID, IntentID: intentID, Agent: agent, ToolName: toolName, Input: json.RawMessage(input),
	})
	action, ok := normalizeAction(out.Action)
	if err != nil || !ok {
		dec.Action, dec.ModelFallback = cfg.FailAction, true
		if err != nil {
			dec.Message = "裁判模型失败，按配置处理：" + err.Error()
		} else {
			dec.Message = fmt.Sprintf("裁判模型返回不可解析动作 %q，按配置处理", out.Action)
		}
		return dec
	}
	dec.Action, dec.Message = action, out.Message
	if out.ProfileID != 0 {
		dec.ProfileID = out.ProfileID
	}
	return dec
}

// Verdict 是 Check 的结果；PendingID 非 0 表示经过人工审批挂起（供执行结果回填）。
type Verdict struct {
	Allowed   bool
	Message   string
	PendingID int64
	Decision  Decision
	Enabled   bool
}

// Check 是 PreToolUse 的入口：判定 → allow/deny 写已决审计 → ask 进入 HandleAsk 挂起。
func (i *Interceptor) Check(ctx context.Context, toolName string, input []byte) Verdict {
	dec, enabled := i.Evaluate(ctx, toolName, input)
	if !enabled {
		return Verdict{Allowed: true}
	}
	v := Verdict{Decision: dec, Enabled: true}
	switch dec.Action {
	case ActionAllow:
		v.Allowed = true
		i.audit(ctx, dec, toolName, input, ActionAllow, 0, dec.Message)
	case ActionAsk:
		v.Allowed, v.PendingID, v.Message = i.HandleAsk(ctx, dec, toolName, input)
	default:
		v.Message = dec.Message
		if v.Message == "" {
			v.Message = "拦截规则拒绝执行"
		}
		i.audit(ctx, dec, toolName, input, ActionDeny, 0, v.Message)
	}
	return v
}

// dbCtx 返回不随运行 Context 取消的短超时 Context：取消路径上的拒绝必须落库。
func dbCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
}

func (i *Interceptor) audit(ctx context.Context, dec Decision, toolName string, input []byte, effective string, pendingID int64, reason string) {
	taskID, intentID, agent := taskInfoFromCtx(ctx)
	source := dec.Source
	if pendingID != 0 {
		source = SourceAsk
	}
	c, cancel := dbCtx(ctx)
	defer cancel()
	_ = i.db.InsertInterceptAudit(c, db.InterceptAuditRow{
		TaskID: taskID, IntentID: intentID, AgentName: agent, ToolName: toolName, InputDigest: digest(input),
		Source: source, RuleID: dec.RuleID, ProfileID: dec.ProfileID, Action: dec.Action, EffectiveAction: effective,
		PendingID: pendingID, Reason: reason,
	})
}

func (i *Interceptor) emit(ctx context.Context, a db.Activity) {
	taskID, intentID, agent := taskInfoFromCtx(ctx)
	if a.TaskID == 0 {
		a.TaskID = taskID
	}
	if a.Worker == "" {
		a.Worker = agent
	}
	if a.NodeID == nil && intentID != 0 {
		a.NodeID = &intentID
	}
	if a.At.IsZero() {
		a.At = time.Now()
	}
	if f := taskEmitFromCtx(ctx); f != nil {
		f(a)
	} else if i.opts.OnActivity != nil {
		i.opts.OnActivity(a)
	}
}

// HandleAsk 挂起当前工具调用等待人工审批（骨架 4），返回 (是否允许, pendingID, 拒绝理由)。
// ctx 是 harness 传给 PreToolUse 的工具 Context：其取消（工作取消、任务暂停、单次运行预算耗尽）
// 都按拒绝落库，工具不执行。
func (i *Interceptor) HandleAsk(ctx context.Context, dec Decision, toolName string, input []byte) (bool, int64, string) {
	taskID, intentID, agent := taskInfoFromCtx(ctx)
	auditRaw, _ := json.Marshal(map[string]any{
		"source": dec.Source, "rule_id": dec.RuleID, "rule_name": dec.RuleName, "profile_id": dec.ProfileID,
		"model_input_digest": dec.ModelInputDigest, "config_digest": dec.ConfigDigest, "model_fallback": dec.ModelFallback,
		"timeout_enabled": dec.TimeoutEnabled, "timeout_seconds": dec.TimeoutSeconds,
	})
	timeoutAction := dec.TimeoutAction
	if timeoutAction != ActionAllow {
		timeoutAction = ActionDeny
	}

	// 1. 先持久化 status='pending' 行；落库失败直接按拒绝处理（fail-closed）。
	c, cancel := dbCtx(ctx)
	pendingID, err := i.db.CreateInterceptPending(c, db.NewInterceptPending{
		RuleID: dec.RuleID, TaskID: taskID, IntentID: intentID, AgentName: agent, ToolName: toolName,
		ToolInput: input, Message: dec.Message, TimeoutAction: timeoutAction, Audit: auditRaw,
		Waiter: i.opts.Waiter, WaiterTTL: i.opts.WaiterTTL,
	})
	cancel()
	if err != nil {
		return false, 0, "审批请求落库失败，按拒绝处理：" + err.Error()
	}

	// 2. 注册内存唤醒 Channel，并在 defer 中兜底移除。
	ch := i.pending.add(pendingID)
	defer i.pending.remove(pendingID)

	finish := func(allowed bool, reason string) (bool, int64, string) {
		effective := ActionDeny
		if allowed {
			effective = ActionAllow
		}
		i.audit(ctx, dec, toolName, input, effective, pendingID, reason)
		i.emit(ctx, db.Activity{Kind: "intercept_decided",
			Summary: fmt.Sprintf("工具 %s 审批结果：%s (#%d)", toolName, effective, pendingID), Detail: reason})
		if !allowed {
			c, cancel := dbCtx(ctx)
			_ = i.db.CompleteIntercept(c, pendingID, "not_executed", reason)
			cancel()
			return false, pendingID, "人工审批未通过：" + reason
		}
		return true, pendingID, ""
	}
	// fromDB 以 DB 中获胜方的真实决定为准。
	fromDB := func() (bool, string, bool) {
		c, cancel := dbCtx(ctx)
		defer cancel()
		row, err := i.db.GetInterceptPending(c, pendingID)
		if err != nil || row == nil {
			return false, "读取审批记录失败", true
		}
		if row.Status == db.InterceptPending {
			return false, "", false
		}
		return row.Allowed(), row.Reason, true
	}
	cancelled := func() (bool, int64, string) {
		c, cancel := dbCtx(ctx)
		won, _ := i.db.ResolveIntercept(c, pendingID, db.InterceptDenied, ActionDeny,
			fmt.Sprintf("工作已取消（%v），按拒绝处理", context.Cause(ctx)), false)
		cancel()
		reason := "工作已取消，审批等待终止"
		if !won {
			if _, r, ok := fromDB(); ok && r != "" {
				reason = r + "；但工作已取消，工具不执行"
			}
		}
		return finish(false, reason)
	}

	// 3. 二次读库防竞态：闭合「INSERT 已提交、但 Channel 尚未注册」间隙内到达的极速 Decide。
	if allowed, reason, decided := fromDB(); decided {
		return finish(allowed, reason)
	}

	// 4. 向关联任务探索流广播 intercept_request 活动卡片。
	detail, _ := json.Marshal(map[string]any{"pending_id": pendingID, "tool": toolName, "input": json.RawMessage(input), "message": dec.Message})
	if !json.Valid(input) {
		detail, _ = json.Marshal(map[string]any{"pending_id": pendingID, "tool": toolName, "input": string(input), "message": dec.Message})
	}
	i.emit(ctx, db.Activity{Kind: "intercept_request", Summary: fmt.Sprintf("工具 %s 请求审批 (#%d)", toolName, pendingID), Detail: string(detail)})

	// 5/6. 三路等待 + 心跳：Channel（本进程 Decide）/ Timer（可选超时）/ ctx.Done()；
	// 心跳续期 waiter_until，并在发现记录已非 pending 时（他实例 Decide）重新读库取得决定。
	var timeout <-chan time.Time
	if dec.TimeoutEnabled {
		secs := dec.TimeoutSeconds
		if secs <= 0 {
			secs = 60
		}
		timer := time.NewTimer(time.Duration(secs) * time.Second)
		defer timer.Stop()
		timeout = timer.C
	}
	beat := time.NewTicker(max(i.opts.WaiterTTL/3, 10*time.Millisecond))
	defer beat.Stop()

	for {
		select {
		case allowed := <-ch:
			_, reason, _ := fromDB()
			return finish(allowed, reason)
		case <-ctx.Done():
			return cancelled()
		case <-timeout:
			allowed := timeoutAction == ActionAllow
			c, cancel := dbCtx(ctx)
			won, err := i.db.ResolveIntercept(c, pendingID, db.InterceptTimeout, timeoutAction, "审批超时，按超时策略处理："+timeoutAction, false)
			cancel()
			if err != nil {
				return finish(false, "审批超时落库失败："+err.Error())
			}
			if !won {
				// CAS 落败：人工 Decide 先一步提交，以获胜方的真实决定为准。
				a, reason, _ := fromDB()
				return finish(a, reason)
			}
			return finish(allowed, "审批超时，按超时策略处理："+timeoutAction)
		case <-beat.C:
			c, cancel := dbCtx(ctx)
			still, err := i.db.TouchInterceptWaiter(c, pendingID, i.opts.WaiterTTL)
			cancel()
			if err == nil && !still {
				if allowed, reason, decided := fromDB(); decided {
					return finish(allowed, reason)
				}
			}
		}
	}
}

// Decide 人工审批：严格「先持久化 DB CAS（WHERE status='pending'），胜出后再唤醒内存 Channel」。
// 重复审批返回 ErrAlreadyDecided；等待方已失联时记录落为拒绝并返回 ErrWaiterGone。
func (i *Interceptor) Decide(ctx context.Context, pendingID int64, allowed bool, reason string) error {
	status, action, fallback := db.InterceptDenied, ActionDeny, "人工拒绝执行"
	if allowed {
		status, action, fallback = db.InterceptAllowed, ActionAllow, "人工允许执行"
	}
	if reason == "" {
		reason = fallback
	}
	resolved, err := i.db.ResolveIntercept(ctx, pendingID, status, action, reason, true)
	if err != nil {
		return err
	}
	if !resolved {
		row, err := i.db.GetInterceptPending(ctx, pendingID)
		if errors.Is(err, db.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if row.Status == db.InterceptPending {
			// 等待方心跳已过期：不允许批准一个无人执行的调用，落为拒绝。
			_, _ = i.db.ResolveIntercept(ctx, pendingID, db.InterceptDenied, ActionDeny, "等待方已失联，审批无法送达，按拒绝处理", false)
			_ = i.db.CompleteIntercept(ctx, pendingID, "not_executed", "等待方已失联")
			return ErrWaiterGone
		}
		return ErrAlreadyDecided
	}
	i.pending.resolve(pendingID, allowed)
	return nil
}

// Waiting 报告本进程是否有等待该审批的工具调用。
func (i *Interceptor) Waiting(pendingID int64) bool { return i.pending.waiting(pendingID) }

// CompleteExecution 回填审批放行后工具的实际执行结果。
func (i *Interceptor) CompleteExecution(ctx context.Context, pendingID int64, isErr bool, result string) {
	outcome := "executed"
	if isErr {
		outcome = "error"
	}
	c, cancel := dbCtx(ctx)
	defer cancel()
	_ = i.db.CompleteIntercept(c, pendingID, outcome, result)
}

// ReapOrphans 立即回收等待方失联的 pending（启动时调用一次即可覆盖“上次进程遗留”的请求）。
func (i *Interceptor) ReapOrphans(ctx context.Context) ([]int64, error) {
	return i.db.ReapOrphanedIntercepts(ctx)
}

// Start 启动孤儿 pending 回收循环：启动时立即执行一次，此后每 WaiterTTL 一次；ctx 结束时退出。
func (i *Interceptor) Start(ctx context.Context) {
	_, _ = i.ReapOrphans(ctx)
	go func() {
		t := time.NewTicker(i.opts.WaiterTTL)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_, _ = i.ReapOrphans(ctx)
			}
		}
	}()
}
