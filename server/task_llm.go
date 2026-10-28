package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/autobid/autobid-engine/retrieval"
	"iter"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/llm/anthropic"
	"github.com/autobid/autobid-engine/internal/llm/llmhttp"
	"github.com/autobid/autobid-engine/internal/llm/openai"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/llmpool"
	"github.com/autobid/autobid-engine/llmrec"
)

// ProviderFactory 按 Profile 构造底层 Provider（未包装计量）。httpClient 已承载该 Profile 的代理、
// 会话请求头与原始报文录制 Transport，适配器必须使用它发起请求。
type ProviderFactory func(p *db.LLMProfile, httpClient *http.Client) (llm.Provider, error)

// DefaultProviderFactory 按 format 构造 Claude Messages API 或 OpenAI 兼容 Provider；
// 密钥从 Profile 指定的环境变量读取（anthropic 变量为空时交由 SDK 默认凭据解析）。
func DefaultProviderFactory(p *db.LLMProfile, httpClient *http.Client) (llm.Provider, error) {
	switch p.Format {
	case "anthropic", "":
		return anthropic.New(anthropic.Config{
			APIKey:          os.Getenv(p.APIKeyEnv),
			BaseURL:         p.BaseURL,
			Model:           p.Model,
			MaxTokens:       int64(p.MaxTokens),
			Effort:          p.Effort,
			ThinkingDisplay: p.ThinkingDisplay,
			HTTPClient:      httpClient,
		}), nil
	case "openai":
		if p.Model == "" {
			return nil, fmt.Errorf("llm profile %q: model is required for openai format", p.Name)
		}
		effort := p.Effort
		if effort != "low" && effort != "medium" && effort != "high" {
			effort = "" // reasoning_effort 只接受 low/medium/high
		}
		return openai.New(openai.Config{
			APIKey:          os.Getenv(p.APIKeyEnv),
			BaseURL:         p.BaseURL,
			Model:           p.Model,
			MaxTokens:       p.MaxTokens,
			ReasoningEffort: effort,
			HTTPClient:      httpClient,
		}), nil
	}
	return nil, fmt.Errorf("unsupported llm profile format %q", p.Format)
}

// RetryConfig 是同 Provider 提交前重试策略：Attempts=0 取默认 2，负数关闭；Backoff=0 为 500ms 起指数退避、上限 4s。
type RetryConfig struct {
	Attempts int
	Backoff  time.Duration
}

func (c RetryConfig) policy() (int, func(attempt int) time.Duration) {
	retries := c.Attempts
	switch {
	case retries < 0:
		retries = 0
	case retries == 0:
		retries = 2
	}
	return retries, func(attempt int) time.Duration {
		if c.Backoff > 0 {
			return c.Backoff
		}
		d := 500 * time.Millisecond << attempt
		return min(d, 4*time.Second)
	}
}

type LLMServiceConfig struct {
	MultiRetrieval bool
	Retrieval      *retrieval.Service
	Factory        ProviderFactory
	Retry          RetryConfig
	Planner        agent.PlannerConfig // Provider / KillWork / SteerWork 由服务注入
	Worker         agent.WorkerConfig  // Provider / Transcripts 由服务注入
	OnActivity     func(taskID int64, a db.Activity)
	OnLedgerErr    func(error)
	// RecordBodies 是原始报文录制的全局开关（可随时切换，每个请求时读取）；为 nil 表示关闭。
	// token 账本不受它影响，始终写入。
	RecordBodies func() bool
}

// LLMService 管理 Profile Provider 缓存、全局池熔断、任务 Agent 实例，并作为 Engine 的 Resolver。
type LLMService struct {
	pg     *db.DB
	cfg    LLMServiceConfig
	health *llmpool.Registry
	engine *Engine

	provCacheMu  sync.Mutex
	provCache    map[int64]llm.Provider
	provCacheGen uint64
	pool         *llmpool.Pool
	poolGen      uint64

	taskAgentMu sync.Mutex
	taskAgents  map[int64]*taskAgentBundle

	readyMu    sync.Mutex
	readyAt    time.Time
	readyValue bool
}

type taskAgentBundle struct {
	plannerRuntime *taskLLMRuntime
	workerRuntime  *taskLLMRuntime
	pl             *agent.Planner
	wk             *agent.Worker
}

func NewLLMService(pg *db.DB, cfg LLMServiceConfig) *LLMService {
	if cfg.Factory == nil {
		cfg.Factory = DefaultProviderFactory
	}
	s := &LLMService{pg: pg, cfg: cfg, provCache: map[int64]llm.Provider{}, taskAgents: map[int64]*taskAgentBundle{}}
	// 熔断持久化在解锁后异步写库，不阻塞调用路径。
	s.health = llmpool.NewRegistry(
		func(id int64, st llmpool.State) {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = pg.SaveLLMHealth(ctx, db.LLMHealth{ProfileID: id, Fails: st.Fails, Trips: st.Trips,
					OpenUntil: st.OpenUntil, LastError: st.LastError, LastAt: st.LastAt})
			}()
		},
		func(id int64) {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = pg.DeleteLLMHealth(ctx, id)
			}()
		},
	)
	return s
}

// Bind 关联 Engine，使 Planner 的 kill_work / steer_work 能作用于在途工作。
func (s *LLMService) Bind(e *Engine) { s.engine = e }

// RestoreHealth 在启动时恢复仍有效的熔断状态。
func (s *LLMService) RestoreHealth(ctx context.Context) error {
	hs, err := s.pg.ActiveLLMHealth(ctx)
	if err != nil {
		return err
	}
	for _, h := range hs {
		s.health.Restore(h.ProfileID, llmpool.State{Fails: h.Fails, Trips: h.Trips, OpenUntil: h.OpenUntil, LastError: h.LastError, LastAt: h.LastAt})
	}
	return nil
}

// InvalidateProviders 在 Profile 配置变更后丢弃缓存的 Provider 与全局池。
// 自动配额切换不调用它：稳定的任务 Agent 每次调用都会重新读取 active Profile（§0.5）。
func (s *LLMService) InvalidateProviders() {
	s.provCacheMu.Lock()
	s.provCache = map[int64]llm.Provider{}
	s.provCacheGen++
	s.pool = nil
	s.provCacheMu.Unlock()
	s.readyMu.Lock()
	s.readyAt = time.Time{}
	s.readyMu.Unlock()
}

// InvalidateTaskAgents 丢弃任务 Agent 实例；Planner 内存待办随之丢失，Worker 可沿稳定 Session ID 找回历史。
func (s *LLMService) InvalidateTaskAgents(taskID int64) {
	s.taskAgentMu.Lock()
	delete(s.taskAgents, taskID)
	s.taskAgentMu.Unlock()
}

// profileProvider 返回带计量包装的 Profile Provider；构造在锁外进行，插入时校验 generation，
// 防止配置失效期间构造的旧 Provider 回填缓存；并发构造输掉的一方采用已缓存赢家。
func (s *LLMService) profileProvider(ctx context.Context, id int64) (llm.Provider, error) {
	s.provCacheMu.Lock()
	if p, ok := s.provCache[id]; ok {
		s.provCacheMu.Unlock()
		return p, nil
	}
	gen := s.provCacheGen
	s.provCacheMu.Unlock()

	prof, err := s.pg.GetLLMProfile(ctx, id)
	if err != nil {
		return nil, err
	}
	if !prof.Enabled {
		return nil, fmt.Errorf("llm profile %q is disabled", prof.Name)
	}
	recProfile := llmrec.Profile{ID: prof.ID, Name: prof.Name, Model: prof.Model, Effort: prof.Effort}
	httpClient, err := llmhttp.NewClient(llmhttp.Options{
		Proxy:            prof.Proxy,
		SessionHeaderKey: prof.SessionHeaderKey,
		Wrap: func(next http.RoundTripper) http.RoundTripper {
			return llmrec.NewTransport(next, s.pg.InsertLLMRecord, recProfile, s.cfg.RecordBodies, s.cfg.OnLedgerErr)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("llm profile %q: %w", prof.Name, err)
	}
	base, err := s.cfg.Factory(prof, httpClient)
	if err != nil {
		return nil, err
	}
	var sink llmrec.Sink = func(ctx context.Context, u db.LLMUsage) error { return s.pg.InsertLLMUsage(ctx, u) }
	wrapped := llmrec.New(base, sink, recProfile, s.cfg.OnLedgerErr)

	s.provCacheMu.Lock()
	defer s.provCacheMu.Unlock()
	if existing, ok := s.provCache[id]; ok {
		return existing, nil
	}
	if gen == s.provCacheGen {
		s.provCache[id] = wrapped
	}
	return wrapped, nil
}

func (s *LLMService) globalPool(ctx context.Context) (*llmpool.Pool, error) {
	s.provCacheMu.Lock()
	if s.pool != nil && s.poolGen == s.provCacheGen {
		p := s.pool
		s.provCacheMu.Unlock()
		return p, nil
	}
	gen := s.provCacheGen
	s.provCacheMu.Unlock()

	profiles, err := s.pg.GlobalPoolProfiles(ctx)
	if err != nil {
		return nil, err
	}
	members := make([]*llmpool.Member, 0, len(profiles))
	for _, p := range profiles {
		prov, err := s.profileProvider(ctx, p.ID)
		if err != nil {
			continue
		}
		members = append(members, &llmpool.Member{ID: p.ID, Name: p.Name, Model: p.Model, Format: p.Format,
			Priority: p.Priority, Active: p.Enabled, Rank: p.Rank, WindowTokens: p.WindowTokens, Prov: prov})
	}
	if len(members) == 0 {
		return nil, errors.New("no enabled llm profile in the global pool")
	}
	pool := llmpool.New(members, s.health)
	s.provCacheMu.Lock()
	if gen == s.provCacheGen {
		s.pool, s.poolGen = pool, gen
	}
	s.provCacheMu.Unlock()
	return pool, nil
}

// ready 报告是否至少存在一个启用的 Profile（5 秒缓存），未就绪时调度循环等待而不是空转失败。
func (s *LLMService) ready() bool {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if time.Since(s.readyAt) < 5*time.Second {
		return s.readyValue
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var ok bool
	_ = s.pg.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM llm_profiles WHERE enabled)`).Scan(&ok)
	s.readyAt, s.readyValue = time.Now(), ok
	return ok
}

// Resolve 实现 Engine 的 Resolver：同一任务复用稳定的 Planner / Worker（绑定稳定的任务运行时）。
func (s *LLMService) Resolve(t *Task) (*agent.Planner, *agent.Worker) {
	if !s.ready() {
		return nil, nil
	}
	s.taskAgentMu.Lock()
	defer s.taskAgentMu.Unlock()
	if b, ok := s.taskAgents[t.ID]; ok {
		return b.pl, b.wk
	}
	b := &taskAgentBundle{
		plannerRuntime: &taskLLMRuntime{s: s, task: t, agentKey: agent.AgentPlanner},
		workerRuntime:  &taskLLMRuntime{s: s, task: t, agentKey: agent.AgentWorker},
	}
	pc := s.cfg.Planner
	pc.Provider = b.plannerRuntime
	if s.engine != nil {
		pc.KillWork, pc.SteerWork = s.engine.KillWork, s.engine.SteerWork
	}
	if pc.Hooks == nil {
		pc.Hooks = t.Hooks
	}
	wc := s.cfg.Worker
	wc.Provider = b.workerRuntime
	if wc.Transcripts == nil {
		wc.Transcripts = db.NewTranscriptStore(s.pg)
	}
	// 招投标领域工具按任务绑定：常驻文档精读工具直接下发，专业工具经 Skill 解锁（§4.3）。
	kit := bidtools.New(s.pg, t.ID, t.Assets, t.Store)
	kit.WithRetrieval(s.cfg.Retrieval)
	if s.cfg.Retrieval != nil {
		kit.WithResearch(&retrieval.Coordinator{Service: s.cfg.Retrieval, Reasoner: &agent.RetrievalReasoner{Planner: b.plannerRuntime, Worker: b.workerRuntime}, MultiEnabled: s.cfg.MultiRetrieval})
	}
	wc.ExtraTools = append(append([]tool.CoreTool(nil), wc.ExtraTools...), kit.ResidentTools()...)
	wc.Deferred = append(append([]tool.CoreTool(nil), wc.Deferred...), kit.DeferredTools()...)
	wc.Skills = append(append([]tool.Skill(nil), wc.Skills...), bidtools.Skills()...)
	if s.cfg.Retrieval != nil {
		for i := range wc.Skills {
			if wc.Skills[i].Name == bidtools.SkillKnowledgeReuse {
				wc.Skills[i].Tools = append(append([]string(nil), wc.Skills[i].Tools...), "kb_read_evidence", "kb_verify_evidence", "kb_research")
			}
		}
	}
	b.pl, b.wk = agent.NewPlanner(pc), agent.NewWorker(wc)
	s.taskAgents[t.ID] = b
	return b.pl, b.wk
}

// ---------------------------------------------------------------------------
// 任务 LLM 运行时（骨架 3）
// ---------------------------------------------------------------------------

// taskLLMRuntime 绑定（任务, 角色）；每次调用动态选择 Profile：角色绑定 -> 任务显式链 active -> 全局池。
type taskLLMRuntime struct {
	s        *LLMService
	task     *Task
	agentKey string
}

type taskLLMSelection struct {
	profileID int64 // 仅任务显式链路径非 0，参与配额链推进
	revision  int64
	provider  llm.Provider
	retry     RetryConfig
}

type taskLLMStreamHooks struct {
	current    func() (taskLLMSelection, error)
	exhaust    func(taskLLMSelection, error) (db.TaskLLMTransition, error)
	transition func(taskLLMSelection, db.TaskLLMTransition, error)
}

// taskLLMError 表示运行时已处理过的配额链错误；Engine 不对其做会话级整轮重试（避免重放已提交输出的副作用）。
type taskLLMError struct {
	taskID         int64
	chainExhausted bool
	cause          error
}

func (e *taskLLMError) Error() string {
	if e.chainExhausted {
		return fmt.Sprintf("task %d: llm profile chain exhausted: %v", e.taskID, e.cause)
	}
	return fmt.Sprintf("task %d: llm quota exhausted after output was committed: %v", e.taskID, e.cause)
}
func (e *taskLLMError) Unwrap() error             { return e.cause }
func (e *taskLLMError) TaskLLMRuntimeError() bool { return true }
func (e *taskLLMError) ChainExhausted() bool      { return e.chainExhausted }

func (r *taskLLMRuntime) hooks(ctx context.Context) taskLLMStreamHooks {
	s, t := r.s, r.task
	return taskLLMStreamHooks{
		current: func() (taskLLMSelection, error) { return r.current(ctx) },
		exhaust: func(sel taskLLMSelection, cause error) (db.TaskLLMTransition, error) {
			c, cancel := dbCtx(ctx)
			defer cancel()
			return s.pg.MarkTaskLLMProfileQuotaExhaustedAtRevision(c, t.ID, sel.profileID, sel.revision, cause.Error())
		},
		transition: func(sel taskLLMSelection, tr db.TaskLLMTransition, cause error) {
			if s.cfg.OnActivity == nil {
				return
			}
			summary := "模型配额耗尽，已切换到下一个配置"
			if tr.ChainExhausted {
				summary = "模型配置链已全部耗尽"
			}
			s.cfg.OnActivity(t.ID, db.Activity{TaskID: t.ID, Worker: r.agentKey, Kind: "state", Summary: summary,
				Detail: fmt.Sprintf("profile %d -> %v: %v", tr.PreviousProfileID, derefID(tr.NextProfileID), cause), At: time.Now()})
		},
	}
}

func derefID(p *int64) any {
	if p == nil {
		return "none"
	}
	return *p
}

func (r *taskLLMRuntime) current(ctx context.Context) (taskLLMSelection, error) {
	s, t := r.s, r.task
	sel := taskLLMSelection{retry: s.cfg.Retry}
	if pid, err := s.pg.AgentLLMBinding(ctx, r.agentKey); err != nil {
		return sel, err
	} else if pid > 0 {
		sel.provider, err = s.profileProvider(ctx, pid)
		return sel, err
	}
	st, err := s.pg.TaskLLMState(ctx, t.ID)
	if err != nil {
		return sel, err
	}
	if st.HasChain {
		// 显式链 active 已耗尽时不静默回退全局配置。
		if st.ActiveProfileID == nil {
			return sel, &taskLLMError{taskID: t.ID, chainExhausted: true, cause: errors.New("no active profile")}
		}
		sel.profileID, sel.revision = *st.ActiveProfileID, st.Revision
		sel.provider, err = s.profileProvider(ctx, sel.profileID)
		return sel, err
	}
	pool, err := s.globalPool(ctx)
	if err != nil {
		return sel, err
	}
	sel.provider = pool
	return sel, nil
}

func (r *taskLLMRuntime) attribute(ctx context.Context) context.Context {
	return llmrec.WithAttribution(ctx, llmrec.Attribution{TaskID: r.task.ID, ExplorationID: r.task.ExpID, Worker: r.agentKey})
}

func (r *taskLLMRuntime) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	ctx = r.attribute(ctx)
	return streamTaskLLM(ctx, r.task.ID, req, r.hooks(ctx))
}

func (r *taskLLMRuntime) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	ctx = r.attribute(ctx)
	return completeTaskLLM(ctx, r.task.ID, req, r.hooks(ctx))
}

// maxChainHops 防御性限制一次调用内的配置链跳转次数。
const maxChainHops = 32

// streamTaskLLM：外层跨 Profile 配额故障转移（仅 !committed 可切链重放），内层同 Provider 提交前重试。
func streamTaskLLM(ctx context.Context, taskID int64, req llm.CompletionRequest, hooks taskLLMStreamHooks) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for hop := 0; ; hop++ {
			if hop >= maxChainHops {
				yield(llm.StreamEvent{}, fmt.Errorf("task %d: too many llm profile switches", taskID))
				return
			}
			selection, err := hooks.current()
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}

			var (
				committed bool
				pending   []llm.StreamEvent
				streamErr error
			)
			retries, backoffOf := selection.retry.policy()
			for attempt := 0; ; attempt++ {
				committed, pending, streamErr = false, nil, nil
				for event, err := range selection.provider.Stream(ctx, req) {
					if err != nil {
						streamErr = err
						break
					}
					// 未触及 commit 边界：缓冲元事件，避免失败重放时向下游发送重复开头。
					if !committed && !llm.CommitsOutput(event) {
						pending = append(pending, event)
						continue
					}
					if !committed {
						for _, buffered := range pending {
							if !yield(buffered, nil) {
								return
							}
						}
						pending, committed = nil, true
					}
					if !yield(event, nil) {
						return
					}
				}
				// 仅在零输出暴露、Context 存活且属瞬态错误时丢弃 pending 原地退避重试。
				if streamErr != nil && !committed && ctx.Err() == nil && attempt < retries && llm.IsRetryable(streamErr) {
					if sleepCtx(ctx, backoffOf(attempt)) {
						break
					}
					continue
				}
				break
			}

			if streamErr == nil {
				for _, buffered := range pending {
					if !yield(buffered, nil) {
						return
					}
				}
				return
			}
			// 非任务链路径或非配额耗尽：不触碰链状态，原样透传。
			if selection.profileID == 0 || !llm.IsQuotaExhausted(streamErr) {
				for _, buffered := range pending {
					if !yield(buffered, nil) {
						return
					}
				}
				yield(llm.StreamEvent{}, streamErr)
				return
			}

			transition, markErr := hooks.exhaust(selection, streamErr)
			if markErr != nil {
				cause := fmt.Errorf("mark profile quota exhausted after %v: %w", streamErr, markErr)
				if committed {
					yield(llm.StreamEvent{}, &taskLLMError{taskID: taskID, cause: cause})
				} else {
					yield(llm.StreamEvent{}, cause)
				}
				return
			}
			if transition.Advanced && !transition.Stale && hooks.transition != nil {
				hooks.transition(selection, transition, streamErr)
			}
			// 已提交：游标已为下一请求推进，当前半截流不拼接、不重放；链耗尽同样上抛。
			if committed || (!transition.Stale && transition.NextProfileID == nil) {
				yield(llm.StreamEvent{}, &taskLLMError{taskID: taskID, chainExhausted: transition.ChainExhausted, cause: streamErr})
				return
			}
			// 未提交且已切到下一 Profile（或因 Stale 需重读）：进入下一轮外层循环重放。
		}
	}
}

// completeTaskLLM 是非流式版本：成功前没有局部输出，配额耗尽总可切链重放。
func completeTaskLLM(ctx context.Context, taskID int64, req llm.CompletionRequest, hooks taskLLMStreamHooks) (llm.Message, string, llm.Usage, error) {
	for hop := 0; hop < maxChainHops; hop++ {
		selection, err := hooks.current()
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		retries, backoffOf := selection.retry.policy()
		var lastErr error
		for attempt := 0; ; attempt++ {
			msg, stop, usage, err := selection.provider.Complete(ctx, req)
			if llm.SingleAttempt(ctx) {
				return msg, stop, usage, err
			}
			if err == nil {
				return msg, stop, usage, nil
			}
			lastErr = err
			if ctx.Err() == nil && attempt < retries && llm.IsRetryable(err) {
				if sleepCtx(ctx, backoffOf(attempt)) {
					break
				}
				continue
			}
			break
		}
		if selection.profileID == 0 || !llm.IsQuotaExhausted(lastErr) {
			return llm.Message{}, "", llm.Usage{}, lastErr
		}
		transition, markErr := hooks.exhaust(selection, lastErr)
		if markErr != nil {
			return llm.Message{}, "", llm.Usage{}, fmt.Errorf("mark profile quota exhausted after %v: %w", lastErr, markErr)
		}
		if transition.Advanced && !transition.Stale && hooks.transition != nil {
			hooks.transition(selection, transition, lastErr)
		}
		if !transition.Stale && transition.NextProfileID == nil {
			return llm.Message{}, "", llm.Usage{}, &taskLLMError{taskID: taskID, chainExhausted: transition.ChainExhausted, cause: lastErr}
		}
	}
	return llm.Message{}, "", llm.Usage{}, fmt.Errorf("task %d: too many llm profile switches", taskID)
}
