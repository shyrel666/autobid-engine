package server

// 端到端调度测试：真实 PostgreSQL + 脚本化假模型。需要 AUTOBID_TEST_DATABASE_URL，未设置时跳过。

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

var (
	pgOnce sync.Once
	pg     *db.DB
	pgErr  error
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTOBID_TEST_DATABASE_URL not set")
	}
	pgOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if pg, pgErr = db.Open(ctx, dsn); pgErr == nil {
			pgErr = pg.Migrate(ctx)
		}
	})
	if pgErr != nil {
		t.Fatalf("open db: %v", pgErr)
	}
	return pg
}

// brain 是按请求决定回复的假模型。
type brain func(req llm.CompletionRequest) llm.Message

func (b brain) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return b(req), "", llm.Usage{}, nil
}

func (b brain) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		m := b(req)
		for _, c := range m.Content {
			var evs []llm.StreamEvent
			switch c.Type {
			case llm.BlockText:
				evs = []llm.StreamEvent{{Type: llm.SETextDelta, Text: c.Text}}
			case llm.BlockToolUse:
				evs = []llm.StreamEvent{
					{Type: llm.SEToolUseStart, ToolUse: &llm.ToolUse{ID: c.ID, Name: c.Name}},
					{Type: llm.SEToolUseDelta, Text: string(c.Input)},
					{Type: llm.SEToolUseEnd},
				}
			}
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

var callSeq sync.Map

func use(name string, input any) llm.Message {
	raw, _ := json.Marshal(input)
	n, _ := callSeq.LoadOrStore(name, new(int))
	*n.(*int)++
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockToolUse, ID: fmt.Sprintf("%s-%d", name, *n.(*int)), Name: name, Input: raw},
	}}
}

func say(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: text}}}
}

// lastToolResult 返回末条 user 消息中的工具结果所属工具名（由 tool_use id 前缀编码）。
func lastToolResult(req llm.CompletionRequest) (string, bool) {
	last := req.Messages[len(req.Messages)-1]
	for _, b := range last.Content {
		if b.Type == llm.BlockToolResult {
			return b.ToolUseID[:strings.LastIndex(b.ToolUseID, "-")], true
		}
	}
	return "", false
}

func lastUserText(req llm.CompletionRequest) string {
	return req.Messages[len(req.Messages)-1].Text()
}

func toolNames(req llm.CompletionRequest) []string {
	var out []string
	for _, t := range req.Tools {
		out = append(out, t.Name)
	}
	return out
}

// plannerBrain：首轮投放一个意图；有事实且 prove=true 时证明全部目标；工具结果之后结束本轮。
func plannerBrain(t *testing.T, task **Task, prove bool) brain {
	return func(req llm.CompletionRequest) llm.Message {
		if _, ok := lastToolResult(req); ok {
			return say("本轮规划完成")
		}
		ctx := context.Background()
		s := (*task).Store
		facts, _ := s.RecentNodes(ctx, db.KindFact, []string{db.StateConfirmed}, 1)
		goals, _ := s.NodesByKind(ctx, db.KindGoal, []string{db.StateGoalOpen}, 10)
		final := strings.Contains(req.System, "终局轮")
		if prove && len(facts) > 0 && len(goals) > 0 {
			return use(agent.ToolProveGoal, map[string]any{"goal_id": goals[0].ID, "evidence_id": facts[0].ID})
		}
		intents, _ := s.NodesByKind(ctx, db.KindIntent, nil, 1)
		if len(intents) == 0 && !final {
			return use(agent.ToolAddIntent, map[string]any{"summary": "核对 A 标段资质要求 4.2 与我方证书"})
		}
		return say("暂无新方向")
	}
}

type workerProbe struct {
	mu              sync.Mutex
	settlementTools []string
}

// workerBrain：mode=fact 直接记录事实；mode=block 调用阻塞工具直至被取消。
// 收尾提示出现时写回一条事实；人工指令出现时直接结束。
func workerBrain(mode string, probe *workerProbe) brain {
	return func(req llm.CompletionRequest) llm.Message {
		text := lastUserText(req)
		prev, isResult := lastToolResult(req)
		switch {
		case strings.Contains(text, "任务即将超时") || strings.Contains(text, "【收尾】"):
			probe.mu.Lock()
			probe.settlementTools = toolNames(req)
			probe.mu.Unlock()
			return use(agent.ToolRecordFact, map[string]any{"summary": "收尾写回：资质 4.2 已部分核对"})
		case strings.Contains(text, "人工指令"):
			return say("已按人工指令结束")
		case isResult && prev == agent.ToolRecordFact:
			return say("核验完成")
		case isResult:
			return say("工具已返回")
		case mode == "fact":
			return use(agent.ToolRecordFact, map[string]any{"summary": "我方 ISO27001 证书在有效期内，满足 4.2"})
		default:
			return use("wait_for_cancel", map[string]any{})
		}
	}
}

var waitTool = tool.New(tool.Spec{Name: "wait_for_cancel", Description: "blocks until cancelled"},
	func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		<-ctx.Done()
		return tool.Result{}, context.Cause(ctx)
	})

type harnessEnv struct {
	e        *Engine
	task     *Task
	acts     *activityLog
	cancel   context.CancelFunc
	pl       *agent.Planner
	wk       *agent.Worker
	workerFn brain
}

type activityLog struct {
	mu   sync.Mutex
	list []db.Activity
}

func (l *activityLog) add(a db.Activity) {
	l.mu.Lock()
	l.list = append(l.list, a)
	l.mu.Unlock()
}

func (l *activityLog) has(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, a := range l.list {
		if strings.Contains(a.Summary, substr) {
			return true
		}
	}
	return false
}

func newEnv(t *testing.T, timeoutSec int, prove bool, workerMode string, probe *workerProbe, cfg Config) *harnessEnv {
	t.Helper()
	return newEnvWith(t, timeoutSec, prove, workerBrain(workerMode, probe), cfg, envOpts{})
}

// envOpts 为控制面测试装配 Hook 工厂与额外领域工具。
type envOpts struct {
	hooks      func(*Task) harness.HookRunner
	extraTools []tool.CoreTool
	onActivity func(db.Activity) // 额外的活动投递（如 SSE Hub）
}

func newEnvWith(t *testing.T, timeoutSec int, prove bool, workerFn brain, cfg Config, o envOpts) *harnessEnv {
	t.Helper()
	pg := testDB(t)
	m := NewManager(pg)
	if o.hooks != nil {
		m.SetHookFactory(o.hooks)
	}
	acts := &activityLog{}
	cfg.OnActivity = func(a db.Activity) {
		acts.add(a)
		if o.onActivity != nil {
			o.onActivity(a)
		}
	}
	if cfg.Workers == 0 {
		cfg.Workers = 1
	}
	set := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	set(&cfg.Debounce, 30*time.Millisecond)
	set(&cfg.WorkerIdlePoll, 30*time.Millisecond)
	set(&cfg.LeaseTTL, 3*time.Second)
	set(&cfg.SettleDrainGrace, 5*time.Second)
	set(&cfg.SettleCancelWait, 200*time.Millisecond)
	set(&cfg.DeadlinePoll, 50*time.Millisecond)
	set(&cfg.DeadlineMaxSleep, 100*time.Millisecond)
	set(&cfg.WorkControlWait, 5*time.Second)
	cfg.ModelErrorRetries = -1

	env := &harnessEnv{acts: acts}
	var e *Engine
	env.pl = agent.NewPlanner(agent.PlannerConfig{
		Provider:  plannerBrain(t, &env.task, prove),
		KillWork:  func(id int64) error { return e.KillWork(id) },
		SteerWork: func(id int64, msg string) error { return e.SteerWork(id, msg) },
	})
	env.wk = agent.NewWorker(agent.WorkerConfig{
		Provider:    workerFn,
		Transcripts: db.NewTranscriptStore(pg),
		ExtraTools:  append([]tool.CoreTool{waitTool}, o.extraTools...),
	})
	e = NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return env.pl, env.wk }, cfg)
	env.e = e

	ctx, cancel := context.WithCancel(context.Background())
	env.cancel = cancel
	task, err := m.CreateTask(ctx, db.CreateTaskInput{
		Name: fmt.Sprintf("engine-%d", time.Now().UnixNano()), Goal: "完成 A 标段资格审查推演", TimeoutSeconds: timeoutSec,
	}, "A 标段资格审查无废标风险")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	env.task = task
	e.Start(ctx)
	if err := e.Run(ctx, task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		e.StopTask(task.ID)
		cancel()
	})
	return env
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func taskStatus(t *testing.T, env *harnessEnv) string {
	r, err := testDB(t).GetTask(context.Background(), env.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
}

func onlyIntent(t *testing.T, env *harnessEnv) *db.Node {
	t.Helper()
	ns, err := env.task.Store.NodesByKind(context.Background(), db.KindIntent, nil, 10)
	if err != nil || len(ns) != 1 {
		t.Fatalf("intents = %d err=%v", len(ns), err)
	}
	return ns[0]
}

// 正常路径：Planner 投放 -> Worker 领取并写事实 -> NotifyDone 唤醒 Planner -> prove_goal -> 任务 done。
func TestEngineHappyPath(t *testing.T) {
	env := newEnv(t, 0, true, "fact", &workerProbe{}, Config{})
	waitFor(t, 15*time.Second, "task done", func() bool { return taskStatus(t, env) == db.TaskDone })

	in := onlyIntent(t, env)
	if in.State != db.StateIntentDone || in.LeaseUntil != nil {
		t.Fatalf("intent = %+v", in)
	}
	ctx := context.Background()
	ys, _ := env.task.Store.YieldedNodes(ctx, in.ID, 10)
	if len(ys) != 1 || ys[0].Kind != db.KindFact {
		t.Fatalf("yields = %+v", ys)
	}
	if total, met, _ := env.task.Store.GoalStats(ctx); total != 1 || met != 1 {
		t.Fatalf("goals total=%d met=%d", total, met)
	}
	msgs, _ := db.NewTranscriptStore(testDB(t)).Load(ctx, agent.WorkerSessionID(env.task.ExpID, in.ID))
	if len(msgs) < 3 {
		t.Fatalf("worker transcript not persisted: %d messages", len(msgs))
	}
}

// 任务级超时：Worker 被夹逼预算打断 -> 收尾白名单内写回 -> Intent exhausted -> 终局 Planner -> 任务 timeout。
func TestEngineTaskTimeoutSettlement(t *testing.T) {
	probe := &workerProbe{}
	env := newEnv(t, 2, false, "block", probe, Config{})
	waitFor(t, 20*time.Second, "task timeout", func() bool { return taskStatus(t, env) == db.TaskTimeout })

	in := onlyIntent(t, env)
	if in.State != db.StateIntentExhausted {
		t.Fatalf("intent state = %s, want exhausted", in.State)
	}
	ys, _ := env.task.Store.YieldedNodes(context.Background(), in.ID, 10)
	if len(ys) != 1 || !strings.Contains(summaryText(ys[0]), "收尾写回") {
		t.Fatalf("settlement write-back missing: %+v", ys)
	}
	probe.mu.Lock()
	got := probe.settlementTools
	probe.mu.Unlock()
	if slices.Contains(got, "wait_for_cancel") || !slices.Contains(got, agent.ToolRecordFact) {
		t.Fatalf("settlement-visible tools = %v", got)
	}
	if !env.acts.has("终局判定") {
		t.Fatal("final planner round did not run")
	}
}

func summaryText(n *db.Node) string {
	var p struct {
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	return p.Summary
}

// 单 work 暂停 -> paused 栅栏；人工消息直投 -> 以新租约续跑同一 transcript -> done。
func TestEngineMessageWithoutSavedHistory(t *testing.T) {
	env := newEnv(t, 0, false, "block", &workerProbe{}, Config{})
	var in *db.Node
	waitFor(t, 5*time.Second, "registered work", func() bool {
		ns, _ := env.task.Store.NodesByKind(context.Background(), db.KindIntent, []string{db.StateIntentRunning}, 1)
		if len(ns) != 1 {
			return false
		}
		in = ns[0]
		env.e.workMu.Lock()
		defer env.e.workMu.Unlock()
		return env.e.work[in.ID] != nil
	})
	ctx := context.Background()
	if err := env.e.ControlWork(ctx, in.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	// Force the exact early-pause boundary independently of scheduling speed.
	if _, err := testDB(t).ExecContext(ctx, `DELETE FROM transcripts WHERE session_id=$1`, agent.WorkerSessionID(env.task.ExpID, in.ID)); err != nil {
		t.Fatal(err)
	}
	if err := env.e.SendMessage(ctx, env.task, in.ID, "fresh-message", "人工指令：直接结束本意图"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "message respected with no history", func() bool { n, _ := env.task.Store.GetNode(ctx, in.ID); return n.State == db.StateIntentDone })
}

func TestEngineControlWorkPauseAndMessage(t *testing.T) {
	env := newEnv(t, 0, false, "block", &workerProbe{}, Config{})
	var in *db.Node
	waitFor(t, 10*time.Second, "intent running", func() bool {
		ns, _ := env.task.Store.NodesByKind(context.Background(), db.KindIntent, []string{db.StateIntentRunning}, 1)
		if len(ns) == 1 {
			in = ns[0]
		}
		return in != nil
	})
	waitFor(t, 5*time.Second, "work registered", func() bool {
		env.e.workMu.Lock()
		defer env.e.workMu.Unlock()
		return env.e.work[in.ID] != nil
	})
	ctx := context.Background()
	if err := env.e.ControlWork(ctx, in.ID, "pause"); err != nil {
		t.Fatalf("ControlWork: %v", err)
	}
	n, _ := env.task.Store.GetNode(ctx, in.ID)
	if n.State != db.StateIntentPaused {
		t.Fatalf("after pause: %s", n.State)
	}
	if err := env.e.SendMessage(ctx, env.task, in.ID, "req-1", "人工指令：直接结束本意图"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitFor(t, 10*time.Second, "intent done", func() bool {
		n, _ := env.task.Store.GetNode(ctx, in.ID)
		return n.State == db.StateIntentDone
	})
	n, _ = env.task.Store.GetNode(ctx, in.ID)
	if n.LeaseEpoch != 2 || n.Owner != "chat" {
		t.Fatalf("resumed run should hold a new lease: epoch=%d owner=%s", n.LeaseEpoch, n.Owner)
	}
}

// Patch ④：租约被他人抢占后，原 Worker 被取消且不写回任何状态。
func TestEngineLeaseLostStopsWithoutWriteback(t *testing.T) {
	env := newEnv(t, 0, false, "block", &workerProbe{}, Config{LeaseTTL: 600 * time.Millisecond})
	var in *db.Node
	waitFor(t, 10*time.Second, "intent running", func() bool {
		ns, _ := env.task.Store.NodesByKind(context.Background(), db.KindIntent, []string{db.StateIntentRunning}, 1)
		if len(ns) == 1 {
			in = ns[0]
		}
		return in != nil
	})
	if _, err := testDB(t).Exec(`UPDATE exploration_nodes
		SET owner = 'intruder', lease_epoch = lease_epoch + 1, lease_until = now() + interval '1 hour'
		WHERE id = $1`, in.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "lease lost activity", func() bool { return env.acts.has("租约丢失") })
	n, _ := env.task.Store.GetNode(context.Background(), in.ID)
	if n.State != db.StateIntentRunning || n.Owner != "intruder" {
		t.Fatalf("stale worker overwrote state: %+v", n)
	}
}
