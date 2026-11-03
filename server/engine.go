package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/intercept"
	"github.com/autobid/autobid-engine/internal/harness"
)

type Config struct {
	Workers             int            // 每任务 Worker 槽数，默认 3
	Debounce            time.Duration  // Planner 固定合并窗口，默认 800ms
	WorkerIdlePoll      time.Duration  // 无可领工作时的轮询间隔，默认 800ms
	FrontierBatch       int            // 每次领取尝试的候选数，默认 20
	LeaseTTL            time.Duration  // Intent 租约，默认 db.DefaultIntentLeaseTTL
	ModelErrorRetries   int            // 会话级 model_error 续跑次数，默认 2；负数关闭
	ModelErrorBackoff   time.Duration  // 默认 3s
	EmptyTurnNudgeLimit int            // 空转轻推上限，0 取默认 2，负数关闭
	SettleDrainGrace    time.Duration  // 任务超时后等待在途工作自行收尾的上限，默认 90s
	SettleCancelWait    time.Duration  // drain 超时强制取消后的落库缓冲，默认 3s
	DeadlinePoll        time.Duration  // 截止协调器轮询，默认 2s
	DeadlineMaxSleep    time.Duration  // 截止协调器单次最长睡眠，默认 30s
	WorkControlWait     time.Duration  // ControlWork 等待终态落库的上限，默认 30s
	MinPlanHeartbeat    time.Duration  // Planner 心跳下限/默认，默认 600s
	Watchdog            WatchdogConfig // 独立 Watchdog 跑偏检测与自动纠偏/熔断配置
	OnActivity          func(db.Activity)
}

func (c *Config) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if c.Workers <= 0 {
		c.Workers = 3
	}
	if c.FrontierBatch <= 0 {
		c.FrontierBatch = 20
	}
	if c.ModelErrorRetries == 0 {
		c.ModelErrorRetries = 2
	}
	set(&c.Debounce, 800*time.Millisecond)
	set(&c.WorkerIdlePoll, 800*time.Millisecond)
	set(&c.LeaseTTL, db.DefaultIntentLeaseTTL)
	set(&c.ModelErrorBackoff, 3*time.Second)
	set(&c.SettleDrainGrace, 90*time.Second)
	set(&c.SettleCancelWait, 3*time.Second)
	set(&c.DeadlinePoll, 2*time.Second)
	set(&c.DeadlineMaxSleep, 30*time.Second)
	set(&c.WorkControlWait, 30*time.Second)
	set(&c.MinPlanHeartbeat, db.MinPlanHeartbeatSeconds*time.Second)
	c.Watchdog.defaults()
}

// Resolver 返回任务当前可用的 Planner / Worker；LLM 未就绪时返回 nil，循环会等待。
type Resolver func(t *Task) (*agent.Planner, *agent.Worker)

type Engine struct {
	m       *Manager
	cfg     Config
	resolve Resolver

	started  sync.Map // taskID -> struct{}
	paused   sync.Map // taskID -> struct{}
	deleting sync.Map // taskID -> struct{}
	deleteMu sync.RWMutex

	runtimeMu sync.Mutex
	runtimes  map[int64]*taskRuntime

	execMu     sync.Mutex
	execCancel map[int64]context.CancelCauseFunc
	execCtx    map[int64]context.Context

	workMu sync.Mutex
	work   map[int64]*workExecution

	steerMu  sync.Mutex
	steerBox map[int64][]string

	plannerRound sync.Map // taskID -> *atomic.Int64
	settling     sync.Map // taskID -> struct{}
	inflight     sync.Map // taskID -> *atomic.Int64
}

type taskRuntime struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type workExecution struct {
	cancel  context.CancelCauseFunc
	done    chan error // 容量 1；完成方发送一次结果，不 close
	action  string
	taskID  int64
	anchors []int64
}

var errWorkControlConflict = errors.New("server: work is not running or already under control")

func NewEngine(m *Manager, resolve Resolver, cfg Config) *Engine {
	cfg.defaults()
	e := &Engine{
		m:          m,
		cfg:        cfg,
		resolve:    resolve,
		runtimes:   map[int64]*taskRuntime{},
		execCancel: map[int64]context.CancelCauseFunc{},
		execCtx:    map[int64]context.Context{},
		work:       map[int64]*workExecution{},
		steerBox:   map[int64][]string{},
	}
	return e
}

func (e *Engine) emit(t *Task, a db.Activity) {
	if e.cfg.OnActivity == nil {
		return
	}
	a.TaskID = t.ID
	if a.At.IsZero() {
		a.At = time.Now()
	}
	e.cfg.OnActivity(a)
}

// dbCtx 返回不随运行 Context 取消的短超时 Context，用于终态 CAS 等必须落库的写入。
func dbCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
}

// ---------------------------------------------------------------------------
// 运行时生命周期
// ---------------------------------------------------------------------------

// Start 启动全局后台循环（租约回收）；ctx 结束时退出。
func (e *Engine) Start(ctx context.Context) {
	go e.leaseReaper(ctx)
}

// Run 启动任务的 Planner、Worker 与截止协调器循环（同一任务只启动一次）。
func (e *Engine) Run(ctx context.Context, t *Task) error {
	if _, loaded := e.started.LoadOrStore(t.ID, struct{}{}); loaded {
		return nil
	}
	c, cancel := dbCtx(ctx)
	defer cancel()
	if t.lifecycleSnapshot().Status == db.TaskCreated {
		if ok, err := e.m.pg.ApplyTaskAdmission(c, t.ID, db.TaskCreated); err != nil {
			e.started.Delete(t.ID)
			return err
		} else if ok {
			t.setLifecycle(func(l *lifecycle) { l.Status = db.TaskRunning })
		}
	}

	// 重启恢复：DB 中的暂停标记要同步到内存，否则恢复后的循环会忽略暂停。
	if t.lifecycleSnapshot().Paused {
		e.paused.Store(t.ID, struct{}{})
	}

	rctx, rcancel := context.WithCancel(ctx)
	rt := &taskRuntime{ctx: rctx, cancel: rcancel}
	e.runtimeMu.Lock()
	e.runtimes[t.ID] = rt
	e.runtimeMu.Unlock()

	rt.wg.Add(2 + e.cfg.Workers)
	go func() { defer rt.wg.Done(); e.plannerLoop(rctx, t) }()
	go func() { defer rt.wg.Done(); e.deadlineCoordinator(rctx, t) }()
	for i := 1; i <= e.cfg.Workers; i++ {
		name := fmt.Sprintf("work#%d", i)
		go func() { defer rt.wg.Done(); e.workerLoop(rctx, t, name) }()
	}
	// 有待领工作时 Worker 先跑，Planner 等完成事件或心跳；否则立即触发首轮规划。
	if active, err := t.Store.HasActiveIntent(c); err == nil && !active {
		t.Notify()
	}
	return nil
}

// StopTask 建立删除屏障、取消执行并等待任务全部 goroutine 退出（不删除 DB 数据）。
func (e *Engine) StopTask(taskID int64) {
	e.deleteMu.Lock()
	e.deleting.Store(taskID, struct{}{})
	e.deleteMu.Unlock()
	e.cancelExec(taskID, agent.AbortTaskDeleted)

	e.runtimeMu.Lock()
	rt := e.runtimes[taskID]
	delete(e.runtimes, taskID)
	e.runtimeMu.Unlock()
	if rt != nil {
		rt.cancel()
		rt.wg.Wait()
	}
	e.started.Delete(taskID)
}

// IsRunning 报告任务运行时是否已启动。
func (e *Engine) IsRunning(taskID int64) bool {
	_, ok := e.started.Load(taskID)
	return ok
}

// StopAll 停止全部任务运行时（优雅退出）；在途 Intent 以 task_deleted 原因回到 open，供下次启动重领。
func (e *Engine) StopAll() {
	e.runtimeMu.Lock()
	ids := make([]int64, 0, len(e.runtimes))
	for id := range e.runtimes {
		ids = append(ids, id)
	}
	e.runtimeMu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); e.StopTask(id) }()
	}
	wg.Wait()
}

func (e *Engine) IsPaused(taskID int64) bool {
	_, ok := e.paused.Load(taskID)
	return ok
}

func (e *Engine) IsDeleting(taskID int64) bool {
	_, ok := e.deleting.Load(taskID)
	return ok
}

func (e *Engine) isSettling(taskID int64) bool {
	_, ok := e.settling.Load(taskID)
	return ok
}

// Pause 暂停任务：标记、落库并以 cause 取消执行 Context；在途 Intent 回到 open。
func (e *Engine) Pause(ctx context.Context, t *Task, cause *agent.AbortCause) error {
	if cause == nil {
		cause = agent.AbortPausedByUser
	}
	e.paused.Store(t.ID, struct{}{})
	if err := e.m.pg.SetTaskPaused(ctx, t.ID, true); err != nil {
		return err
	}
	t.setLifecycle(func(l *lifecycle) {
		l.Paused = true
		if l.Status == db.TaskRunning || l.Status == db.TaskCreated {
			l.Status = db.TaskPaused
		}
	})
	e.cancelExec(t.ID, cause)
	return nil
}

// Resume 解除暂停并唤醒；后续 execContextFor 会建立新的执行子树。
func (e *Engine) Resume(ctx context.Context, t *Task) error {
	if err := e.m.pg.SetTaskPaused(ctx, t.ID, false); err != nil {
		return err
	}
	t.setLifecycle(func(l *lifecycle) {
		l.Paused = false
		if l.Status == db.TaskPaused {
			l.Status = db.TaskRunning
		}
	})
	e.paused.Delete(t.ID)
	t.Notify()
	return nil
}

// Revive 让终态任务重新准入（如新增目标后）：清空首跑/截止时间与结算标记。
func (e *Engine) Revive(ctx context.Context, t *Task) (bool, error) {
	from := t.lifecycleSnapshot().Status
	if !db.IsTerminalTaskStatus(from) {
		return false, nil
	}
	ok, err := e.m.pg.ApplyTaskAdmission(ctx, t.ID, from)
	if err != nil || !ok {
		return ok, err
	}
	e.settling.Delete(t.ID)
	t.setLifecycle(func(l *lifecycle) {
		l.Status, l.Paused, l.FirstRunAt, l.DeadlineAt = db.TaskRunning, false, nil, nil
	})
	t.Notify()
	return true, nil
}

// ---------------------------------------------------------------------------
// 执行 Context 与在途计数
// ---------------------------------------------------------------------------

// execContextFor 返回任务的可取消执行 Context；已被取消（暂停/目标达成）时重建。
func (e *Engine) execContextFor(parent context.Context, taskID int64) context.Context {
	e.execMu.Lock()
	defer e.execMu.Unlock()
	if c, ok := e.execCtx[taskID]; ok && c.Err() == nil {
		return c
	}
	c, cancel := context.WithCancelCause(parent)
	e.execCtx[taskID], e.execCancel[taskID] = c, cancel
	return c
}

func (e *Engine) cancelExec(taskID int64, cause error) {
	e.execMu.Lock()
	cancel := e.execCancel[taskID]
	delete(e.execCancel, taskID)
	delete(e.execCtx, taskID)
	e.execMu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

// interceptCtx 注入拦截控制面所需的任务/意图/Agent 身份与审批活动投递（§6.3）；只携带值，不改变取消语义。
func (e *Engine) interceptCtx(ctx context.Context, t *Task, intentID int64, agentName string) context.Context {
	ctx = intercept.WithTask(ctx, t.ID, intentID)
	ctx = intercept.WithAgent(ctx, agentName)
	return intercept.WithEmit(ctx, func(a db.Activity) { e.emit(t, a) })
}

// clockCtx 注入 TaskClock；它只携带截止时间数值，不取消父 Context。
func (e *Engine) clockCtx(ctx context.Context, t *Task, final bool) context.Context {
	var dl int64
	if d := t.lifecycleSnapshot().DeadlineAt; d != nil {
		dl = d.Unix()
	}
	return agent.WithTaskClock(ctx, agent.TaskClock{DeadlineUnix: dl, Final: final})
}

func (e *Engine) inflightCounter(taskID int64) *atomic.Int64 {
	v, _ := e.inflight.LoadOrStore(taskID, new(atomic.Int64))
	return v.(*atomic.Int64)
}

// beginTaskOperation 在删除屏障读锁下登记在途操作，杜绝删除后的 check-then-start 竞态。
func (e *Engine) beginTaskOperation(taskID int64) bool {
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(taskID) {
		return false
	}
	e.inflightCounter(taskID).Add(1)
	return true
}

func (e *Engine) decInflight(taskID int64)         { e.inflightCounter(taskID).Add(-1) }
func (e *Engine) inflightCount(taskID int64) int64 { return e.inflightCounter(taskID).Load() }

func (e *Engine) nextPlannerRound(taskID int64) int64 {
	v, _ := e.plannerRound.LoadOrStore(taskID, new(atomic.Int64))
	return v.(*atomic.Int64).Add(1)
}

// stampFirstRun 首次真实执行时持久化 first_run_at / deadline_at（幂等）。
func (e *Engine) stampFirstRun(ctx context.Context, t *Task) {
	if t.lifecycleSnapshot().FirstRunAt != nil {
		return
	}
	c, cancel := dbCtx(ctx)
	defer cancel()
	fr, dl, err := e.m.pg.StampFirstRun(c, t.ID)
	if err != nil {
		e.emit(t, db.Activity{Worker: "system", Kind: "error", Summary: "记录首次执行时间失败", Detail: err.Error()})
		return
	}
	t.setLifecycle(func(l *lifecycle) { l.FirstRunAt, l.DeadlineAt = fr, dl })
}

func sleepCtx(ctx context.Context, d time.Duration) (cancelled bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-timer.C:
		return false
	}
}

// ---------------------------------------------------------------------------
// plannerLoop（骨架 1-A）
// ---------------------------------------------------------------------------

func resetPlannerTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (e *Engine) plannerHeartbeat(t *Task) time.Duration {
	d := time.Duration(t.PlanHeartbeatSeconds) * time.Second
	if d < e.cfg.MinPlanHeartbeat {
		d = e.cfg.MinPlanHeartbeat
	}
	return d
}

func (e *Engine) plannerLoop(ctx context.Context, t *Task) {
	interval := e.plannerHeartbeat(t)
	heartbeat := time.NewTimer(interval)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.notify:
			e.runPlannerRound(ctx, t)
		case <-heartbeat.C:
			e.runPlannerRound(ctx, t)
		}
		// 处理完毕后重新起算心跳，而非固定频率 ticker。
		resetPlannerTimer(heartbeat, interval)
	}
}

func (e *Engine) runPlannerRound(ctx context.Context, t *Task) {
	// 1. 固定长度合并窗口：吸收突发 notify，不因新事件重置。
	timer := time.NewTimer(e.cfg.Debounce)
drain:
	for {
		select {
		case <-t.notify:
		case <-timer.C:
			break drain
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}

	// 2. 前置守卫：LLM 就绪 -> 暂停 -> 删除 -> 终态 -> 结算中 -> 无开放目标。
	planner, _ := e.resolve(t)
	if planner == nil || e.IsPaused(t.ID) || e.IsDeleting(t.ID) {
		return
	}
	if db.IsTerminalTaskStatus(t.lifecycleSnapshot().Status) || e.isSettling(t.ID) {
		return
	}
	c, cancel := dbCtx(ctx)
	open, err := t.Store.HasOpenGoal(c)
	if err == nil && !open {
		t.drainTriggers()
		if active, err := t.Store.HasActiveIntent(c); err == nil && !active {
			if won, err := e.m.SetTaskStatusGuarded(c, t, db.TaskDone); err == nil && won {
				e.emit(t, db.Activity{Worker: "system", Kind: "state", Summary: "目标全部达成且无在途意图，任务完成"})
			}
		}
		cancel()
		return
	}
	cancel()

	// 3. 登记在途并派生带时钟的执行 Context。
	if !e.beginTaskOperation(t.ID) {
		return
	}
	defer e.decInflight(t.ID)
	e.stampFirstRun(ctx, t)
	ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false)
	if ectx.Err() != nil || e.IsDeleting(t.ID) {
		return
	}
	ectx = e.interceptCtx(ectx, t, 0, "planner")

	// 4. 摘取合并的触发事件，同步执行单轮规划。
	e.emit(t, db.Activity{Worker: "planner", Kind: "round", Summary: fmt.Sprintf("第 %d 轮规划", e.nextPlannerRound(t.ID))})
	triggers := t.drainTriggers()
	met, _, err := planner.Plan(ectx, t.ID, t.Assets, t.Store, t.Goal, triggers, func(a db.Activity) { e.emit(t, a) })

	// 5. 全部目标达成：落库 done 并以 AbortGoalMet 取消在途 Worker。
	switch {
	case err != nil && ectx.Err() == nil:
		e.emit(t, db.Activity{Worker: "planner", Kind: "error", Summary: "规划失败，等待下一次唤醒", Detail: err.Error()})
	case met:
		c, cancel := dbCtx(ctx)
		defer cancel()
		if won, _ := e.m.SetTaskStatusGuarded(c, t, db.TaskDone); won {
			e.emit(t, db.Activity{Worker: "system", Kind: "state", Summary: "全部目标已证明达成，任务完成"})
		}
		e.cancelExec(t.ID, agent.AbortGoalMet)
	}
}

// ---------------------------------------------------------------------------
// workerLoop / claimNext
// ---------------------------------------------------------------------------

func (e *Engine) workerLoop(ctx context.Context, t *Task, name string) {
	for ctx.Err() == nil {
		_, worker := e.resolve(t)
		if worker == nil || e.IsPaused(t.ID) || e.IsDeleting(t.ID) ||
			db.IsTerminalTaskStatus(t.lifecycleSnapshot().Status) || e.isSettling(t.ID) {
			sleepCtx(ctx, e.cfg.WorkerIdlePoll)
			continue
		}
		if !e.beginTaskOperation(t.ID) {
			sleepCtx(ctx, e.cfg.WorkerIdlePoll)
			continue
		}
		intent, epoch := e.claimNext(ctx, t, name)
		if intent == nil {
			e.decInflight(t.ID)
			sleepCtx(ctx, e.cfg.WorkerIdlePoll)
			continue
		}
		e.runIntent(ctx, t, name, worker, intent, epoch, "", "")
		e.decInflight(t.ID)
	}
}

// claimNext 读取 frontier 候选并逐个以租约争抢；返回领到的 Intent 及其 Fencing Token。
func (e *Engine) claimNext(ctx context.Context, t *Task, name string) (*db.Node, int64) {
	cands, err := t.Store.Frontier(ctx, e.cfg.FrontierBatch)
	if err != nil {
		return nil, 0
	}
	for _, n := range cands {
		epoch, ok, err := t.Store.ClaimIntentWithLease(ctx, n.ID, name, e.cfg.LeaseTTL)
		if err != nil || !ok {
			continue
		}
		full, err := t.Store.GetNode(ctx, n.ID)
		if err != nil {
			full = n
		}
		return full, epoch
	}
	return nil, 0
}

// ---------------------------------------------------------------------------
// runIntent（骨架 1-B + Patch ④ fencing）
// ---------------------------------------------------------------------------

func (e *Engine) registerWork(intentID int64, cancel context.CancelCauseFunc, taskID int64, anchors []int64) {
	e.workMu.Lock()
	e.work[intentID] = &workExecution{
		cancel:  cancel,
		done:    make(chan error, 1),
		taskID:  taskID,
		anchors: slices.Clone(anchors),
	}
	e.workMu.Unlock()
}

// detachWork 注销执行句柄并取消 workCtx；必须在读取 context.Cause(workCtx) 之后调用。
func (e *Engine) detachWork(intentID int64) (action string, complete func(error)) {
	e.workMu.Lock()
	run := e.work[intentID]
	if run != nil {
		delete(e.work, intentID)
		action = run.action
		run.cancel(agent.AbortWorkFinished)
	}
	e.workMu.Unlock()

	e.steerMu.Lock()
	delete(e.steerBox, intentID)
	e.steerMu.Unlock()

	if run == nil {
		return action, func(error) {}
	}
	return action, func(err error) { run.done <- err }
}

func (e *Engine) releaseWorkControl(intentID int64, run *workExecution, action string) {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.work[intentID] == run && run.action == action {
		run.action = ""
	}
}

// ControlWork 暂停或取消单个在途 Intent，并等待其终态落库（最长 WorkControlWait）。
func (e *Engine) ControlWork(ctx context.Context, intentID int64, action string) error {
	if action != "pause" && action != "cancel" {
		return fmt.Errorf("unsupported work action %q", action)
	}
	e.workMu.Lock()
	run := e.work[intentID]
	if run == nil || run.action != "" {
		e.workMu.Unlock()
		return errWorkControlConflict
	}
	run.action = action
	done := run.done
	cause := error(agent.AbortWorkPausedByUser)
	if action == "cancel" {
		cause = agent.AbortWorkCancelledByUser
	}
	run.cancel(cause)
	e.workMu.Unlock()

	timer := time.NewTimer(e.cfg.WorkControlWait)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		e.releaseWorkControl(intentID, run, action)
		return ctx.Err()
	case <-timer.C:
		e.releaseWorkControl(intentID, run, action)
		return context.DeadlineExceeded
	}
}

// KillWork 由 Planner 单点终止在途 Intent（任务仍存活）；Intent 将转 stopped 并唤醒重规划。
func (e *Engine) KillWork(intentID int64) error {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	run := e.work[intentID]
	if run == nil {
		return fmt.Errorf("intent %d is not running", intentID)
	}
	run.cancel(agent.AbortKilledByPlanner)
	return nil
}

// SteerWork 排队一条纠偏指令，在该 Worker 下一次工具调用前送达。
func (e *Engine) SteerWork(intentID int64, msg string) error {
	e.workMu.Lock()
	_, running := e.work[intentID]
	e.workMu.Unlock()
	if !running {
		return fmt.Errorf("intent %d is not running", intentID)
	}
	e.steerMu.Lock()
	e.steerBox[intentID] = append(e.steerBox[intentID], msg)
	e.steerMu.Unlock()
	return nil
}

func (e *Engine) drainSteer(intentID int64) (string, bool) {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	q := e.steerBox[intentID]
	if len(q) == 0 {
		return "", false
	}
	e.steerBox[intentID] = q[1:]
	return q[0], true
}

// RouteHintToRunningWork is a transient notification helper retained for callers
// that explicitly want best-effort steering. Manager.AddHint uses the durable
// database inbox instead; this helper must never acknowledge a Hint.
func (e *Engine) RouteHintToRunningWork(t *Task, hintID int64, text string, anchors []int64) []int64 {
	if t == nil || len(anchors) == 0 {
		return nil
	}
	anchorSet := make(map[int64]bool, len(anchors))
	for _, a := range anchors {
		anchorSet[a] = true
	}
	var matched []int64
	e.workMu.Lock()
	for iid, run := range e.work {
		if run == nil || run.taskID != t.ID || run.action != "" {
			continue
		}
		for _, a := range run.anchors {
			if anchorSet[a] {
				matched = append(matched, iid)
				break
			}
		}
	}
	e.workMu.Unlock()
	if len(matched) == 0 {
		return nil
	}
	slices.Sort(matched)
	msg := fmt.Sprintf("【人工实时补充 Hint #%d（事件直通）】%s", hintID, text)
	var routed []int64
	for _, iid := range matched {
		if err := e.SteerWork(iid, msg); err == nil {
			routed = append(routed, iid)
			idCopy := iid
			e.emit(t, db.Activity{
				Worker:  "system",
				Kind:    "passthrough",
				NodeID:  &idCopy,
				Summary: fmt.Sprintf("Hint #%d 已直通投递至在途意图 #%d", hintID, iid),
				Detail:  text,
			})
		}
	}
	return routed
}

// 第 4 步 LLM 运行时的错误通过以下可选接口声明语义，引擎不依赖其具体类型。
type taskLLMRuntimeError interface{ TaskLLMRuntimeError() bool }
type chainExhaustedError interface{ ChainExhausted() bool }

func isTaskLLMRuntimeError(err error) bool {
	var e taskLLMRuntimeError
	return errors.As(err, &e) && e.TaskLLMRuntimeError()
}

func isTaskLLMChainExhausted(err error) bool {
	var e chainExhaustedError
	return errors.As(err, &e) && e.ChainExhausted()
}

func retryableWorkerModelError(reason harness.TerminalReason, err error) bool {
	return reason == harness.ReasonModelError && err != nil && !isTaskLLMRuntimeError(err)
}

func (e *Engine) modelErrorRetries() int {
	if e.cfg.ModelErrorRetries < 0 {
		return 0
	}
	return e.cfg.ModelErrorRetries
}

func (e *Engine) emptyTurnNudgeLimit() int {
	switch {
	case e.cfg.EmptyTurnNudgeLimit < 0:
		return -1
	case e.cfg.EmptyTurnNudgeLimit == 0:
		return 2
	}
	return e.cfg.EmptyTurnNudgeLimit
}

// transitionIntent 以 Fencing Token 做受保护 CAS；失败（被回收/人工改写）时不覆盖。
func (e *Engine) transitionIntent(ctx context.Context, t *Task, intentID, epoch int64, from, to string) error {
	c, cancel := dbCtx(ctx)
	defer cancel()
	ok, err := t.Store.CompareAndSetIntentStateFenced(c, intentID, from, to, epoch)
	if err == nil && !ok {
		err = fmt.Errorf("intent %d: %s->%s rejected (state changed or lease lost)", intentID, from, to)
	}
	if err != nil {
		e.emit(t, db.Activity{Worker: "system", Kind: "error", NodeID: &intentID, Summary: "意图状态写回被拒绝", Detail: err.Error()})
	} else {
		e.emit(t, db.Activity{Worker: "system", Kind: "state", NodeID: &intentID, Summary: from + " -> " + to})
	}
	return err
}

// runIntent 执行一个已领取的 Intent，并严格按优先级落库终态（骨架 1-B）。
func (e *Engine) runIntent(ctx context.Context, t *Task, name string, worker *agent.Worker, intent *db.Node, epoch int64, requestID, message string) {
	e.stampFirstRun(ctx, t)
	ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false)

	// [1] 领取后、执行前任务已取消或正在删除 -> running 回退 open。
	if ectx.Err() != nil || e.IsDeleting(t.ID) {
		_ = e.transitionIntent(ctx, t, intent.ID, epoch, db.StateIntentRunning, db.StateIntentOpen)
		return
	}

	workCtx, workCancel := context.WithCancelCause(e.interceptCtx(ectx, t, intent.ID, name))
	workCtx = db.WithExecutionScope(workCtx, db.NodeMandate(intent))
	workCtx = db.WithIntentExecution(workCtx, intent.ID, epoch)
	e.registerWork(intent.ID, workCancel, t.ID, intent.Anchors)
	// Patch ④：执行期间周期续租；租约丢失立即以 AbortLeaseLost 取消本次执行。
	go t.Store.KeepIntentLease(workCtx, intent.ID, name, epoch, e.cfg.LeaseTTL, func(error) {
		workCancel(agent.AbortLeaseLost)
	})

	iid := intent.ID
	label := fmt.Sprintf("%s · #%d", name, iid)
	emit := func(a db.Activity) { e.emit(t, a) }
	watchdog := newWatchdogHooks(
		t.Hooks,
		iid,
		name,
		db.NodeMandate(intent),
		e.cfg.Watchdog,
		func(steerMsg string) { _ = e.SteerWork(iid, steerMsg) },
		func(cause error) { workCancel(cause) },
		emit,
	)
	hooks := &steerHooks{
		inner: watchdog,
		drain: func() (string, bool) {
			msg, ok, deliveryErr := t.Store.TakeIntentInput(workCtx, iid, epoch)
			if deliveryErr != nil {
				workCancel(fmt.Errorf("read durable input: %w", deliveryErr))
				return "", false
			}
			if ok {
				return msg, true
			}
			return e.drainSteer(iid)
		},
		nudges: &atomic.Int64{}, // 建在重试循环外，跨 model_error 重试共享
		limit:  e.emptyTurnNudgeLimit(),
		label:  label,
	}
	runOnce := func() (harness.TerminalReason, agent.WriteCounts, error) {
		if message != "" {
			return worker.ExecuteWithMessage(workCtx, name, t.ID, t.Assets, t.Store, intent, hooks, emit, t.NotifyFinding, requestID, message)
		}
		return worker.Execute(workCtx, name, t.ID, t.Assets, t.Store, intent, hooks, emit, t.NotifyFinding)
	}

	reason, _, err := runOnce()
	retries, backoff := e.modelErrorRetries(), e.cfg.ModelErrorBackoff
	if policy, policyErr := e.m.DB().LLMSettings(workCtx); policyErr == nil {
		retries = retryCount(policy.Retry.Intent, retries)
		if policy.Retry.Intent.IntervalMS > 0 {
			backoff = time.Duration(policy.Retry.Intent.IntervalMS) * time.Millisecond
		}
	}
	for attempt := 1; attempt <= retries &&
		retryableWorkerModelError(reason, err) &&
		workCtx.Err() == nil && ectx.Err() == nil && !e.IsPaused(t.ID) && !e.isSettling(t.ID); attempt++ {
		if sleepCtx(workCtx, backoff) {
			break
		}
		message = "" // 续跑沿用同一 transcript
		reason, _, err = runOnce()
	}

	// Only a persisted, successful turn may acknowledge operator input.
	if err == nil && reason == harness.ReasonCompleted && workCtx.Err() == nil {
		if mandate := db.NodeMandate(intent); mandate != nil && mandate.RequireWriteback && !watchdog.HasWriteback() {
			err = errors.New("intent completion rejected: required writeback missing")
		}
		if err == nil {
			err = e.m.pg.CheckIntentDeliverables(workCtx, t.ID, intent)
		}
		if err == nil {
			err = t.Store.AcknowledgeIntentInputs(workCtx, iid, epoch)
		}
	}
	// 关键时序：先读取取消原因与 killed，再 detachWork（它会以 AbortWorkFinished 取消 workCtx）。
	workCause := context.Cause(workCtx)
	killed := workCtx.Err() != nil && ectx.Err() == nil
	action, completeWork := e.detachWork(intent.ID)
	if action == "" {
		switch {
		case errors.Is(workCause, agent.AbortWorkPausedByUser):
			action = "pause"
		case errors.Is(workCause, agent.AbortWorkCancelledByUser):
			action = "cancel"
		}
	}
	var controlErr error
	defer func() { completeWork(controlErr) }()

	from := db.StateIntentRunning
	switch {
	// [0] Patch ④：租约已丢失，Intent 已被回收或由他人持有，不写任何状态。
	case errors.Is(workCause, agent.AbortLeaseLost):
		e.emit(t, db.Activity{Worker: "system", Kind: "error", NodeID: &iid, Summary: "租约丢失，放弃本次写回"})
		t.Notify()
	// [2] 单 work 暂停；[3] 单 work 取消先落 paused 栅栏，交由上层软删除或清理。
	case action == "pause", action == "cancel":
		controlErr = e.transitionIntent(ctx, t, iid, epoch, from, db.StateIntentPaused)
	// [4] 任务级暂停 -> open，恢复后按稳定 Session ID 续跑。
	case ectx.Err() != nil && agent.IsTaskExecutionPaused(context.Cause(ectx)):
		_ = e.transitionIntent(ctx, t, iid, epoch, from, db.StateIntentOpen)
	// [5] 任务超时 drain 被强制打断 -> exhausted。
	case ectx.Err() != nil && e.isSettling(t.ID):
		_ = e.transitionIntent(ctx, t, iid, epoch, from, db.StateIntentExhausted)
	// [6] 任务已终态（如全部目标达成）-> stopped。
	case ectx.Err() != nil && db.IsTerminalTaskStatus(t.lifecycleSnapshot().Status):
		_ = e.transitionIntent(ctx, t, iid, epoch, from, db.StateIntentStopped)
	// [7] 被 Planner kill 或被 Watchdog 熔断（任务仍存活）-> stopped，并唤醒重规划。
	case killed:
		_ = e.transitionIntent(ctx, t, iid, epoch, from, db.StateIntentStopped)
		if errors.Is(workCause, agent.AbortKilledByWatchdog) {
			t.appendTrigger(agent.TriggerEvent{Kind: agent.TriggerWatchdogAlert, IntentID: iid, Detail: workCause.Error()})
		} else {
			t.appendTrigger(agent.TriggerEvent{Kind: agent.TriggerWorkStopped, IntentID: iid, Detail: "killed"})
		}
	// [8] 执行失败 -> blocked；配额链耗尽另记原因，供配额恢复后批量重开。
	case err != nil:
		if isTaskLLMChainExhausted(err) {
			c, cancel := dbCtx(ctx)
			_, _ = t.Store.BlockIntentFenced(c, iid, epoch, db.BlockedReasonQuotaExhausted)
			cancel()
		} else {
			_ = e.transitionIntent(ctx, t, iid, epoch, from, db.StateIntentBlocked)
		}
		e.emit(t, db.Activity{Worker: name, Kind: "error", NodeID: &iid, Summary: "意图执行失败", Detail: err.Error()})
		t.NotifyDone(iid)
	// [9] 预算耗尽（已经过收尾写回）-> exhausted。
	case reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout:
		_ = e.transitionIntent(ctx, t, iid, epoch, from, db.StateIntentExhausted)
		t.NotifyDone(iid)
	// [10] A message arriving after the last hook needs a new turn.
	default:
		c, cancel := dbCtx(ctx)
		completionErr := t.Store.CompleteIntentOrContinue(c, iid, epoch)
		cancel()
		if completionErr != nil {
			e.emit(t, db.Activity{Worker: name, Kind: "error", NodeID: &iid, Summary: "工作完成状态未提交", Detail: completionErr.Error()})
		}
		t.NotifyDone(iid)
	}
	if node, readErr := t.Store.GetNode(ctx, iid); readErr == nil && node != nil && node.State != db.StateIntentRunning && node.State != db.StateIntentOpen && node.State != db.StateIntentPaused {
		_ = t.Store.ReturnIntentInputs(ctx, iid)
	}
}

// SendMessage 向 paused Intent 直投人工消息：以新租约 paused->running 后异步续跑。
func (e *Engine) SendMessage(ctx context.Context, t *Task, intentID int64, requestID, message string) error {
	_, worker := e.resolve(t)
	if worker == nil {
		return errors.New("server: worker not available")
	}
	if !e.beginTaskOperation(t.ID) {
		return errors.New("server: task is being deleted")
	}
	epoch, ok, err := t.Store.ResumePausedIntentWithLease(ctx, intentID, "chat", e.cfg.LeaseTTL)
	if err != nil || !ok {
		e.decInflight(t.ID)
		if err == nil {
			err = fmt.Errorf("intent %d is not paused", intentID)
		}
		return err
	}
	intent, err := t.Store.GetNode(ctx, intentID)
	if err != nil {
		e.decInflight(t.ID)
		return err
	}
	e.runtimeMu.Lock()
	rt := e.runtimes[t.ID]
	e.runtimeMu.Unlock()
	if rt == nil {
		e.decInflight(t.ID)
		_ = e.transitionIntent(ctx, t, intentID, epoch, db.StateIntentRunning, db.StateIntentPaused)
		return errors.New("server: task is not running")
	}
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer e.decInflight(t.ID)
		e.runIntent(rt.ctx, t, "chat", worker, intent, epoch, requestID, message)
	}()
	return nil
}

// ---------------------------------------------------------------------------
// 租约回收
// ---------------------------------------------------------------------------

func (e *Engine) leaseReaper(ctx context.Context) {
	interval := e.cfg.LeaseTTL / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		e.reapOnce(ctx)
	}
}

func (e *Engine) reapOnce(ctx context.Context) {
	expIDs, err := e.m.pg.ReapExpiredIntents(ctx)
	if err != nil {
		return
	}
	for _, id := range expIDs {
		if t := e.m.taskByExploration(id); t != nil {
			e.emit(t, db.Activity{Worker: "system", Kind: "state", Summary: "回收过期租约的意图"})
			t.appendTrigger(agent.TriggerEvent{Kind: agent.TriggerLeaseReaped})
		}
	}
}
