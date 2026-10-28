// Package server 实现任务运行时：Manager 管理任务句柄与生命周期，Engine 驱动 Planner–Worker 调度、
// 单意图终态分流、任务级超时结算与租约回收。
package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/harness"
)

// Task 是运行期任务句柄：DB 快照 + 唤醒通道 + 触发事件缓冲。
type Task struct {
	ID                   int64
	ExpID                int64
	Name                 string
	Goal                 string
	TimeoutSeconds       int
	PlanHeartbeatSeconds int
	Store                *db.ExplorationStore
	Assets               *db.AssetStore
	// Hooks 是拦截/审批控制面（guard/intercept），由 Manager 的 Hook 工厂在任务创建/加载时装配；可为 nil。
	Hooks harness.HookRunner

	lifecycleMu sync.RWMutex
	status      string
	paused      bool
	firstRunAt  *time.Time
	deadlineAt  *time.Time

	notify          chan struct{} // 容量 1 的边沿信号，不承载事件内容
	trigMu          sync.Mutex
	pendingTriggers []agent.TriggerEvent
}

type lifecycle struct {
	Status     string
	Paused     bool
	FirstRunAt *time.Time
	DeadlineAt *time.Time
}

func newTask(pg *db.DB, row *db.Task, goal string) *Task {
	return &Task{
		ID:                   row.ID,
		ExpID:                row.ExplorationID,
		Name:                 row.Name,
		Goal:                 goal,
		TimeoutSeconds:       row.TimeoutSeconds,
		PlanHeartbeatSeconds: row.PlanHeartbeatSeconds,
		Store:                db.NewExplorationStore(pg, row.ExplorationID),
		Assets:               db.NewAssetStore(pg, row.ID),
		status:               row.Status,
		paused:               row.Paused,
		firstRunAt:           row.FirstRunAt,
		deadlineAt:           row.DeadlineAt,
		notify:               make(chan struct{}, 1),
	}
}

func (t *Task) lifecycleSnapshot() lifecycle {
	t.lifecycleMu.RLock()
	defer t.lifecycleMu.RUnlock()
	return lifecycle{Status: t.status, Paused: t.paused, FirstRunAt: t.firstRunAt, DeadlineAt: t.deadlineAt}
}

func (t *Task) setLifecycle(fn func(l *lifecycle)) {
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	l := lifecycle{Status: t.status, Paused: t.paused, FirstRunAt: t.firstRunAt, DeadlineAt: t.deadlineAt}
	fn(&l)
	t.status, t.paused, t.firstRunAt, t.deadlineAt = l.Status, l.Paused, l.FirstRunAt, l.DeadlineAt
}

// Notify 非阻塞地发出边沿信号；重复信号合并。
func (t *Task) Notify() {
	select {
	case t.notify <- struct{}{}:
	default:
	}
}

func (t *Task) appendTrigger(ev agent.TriggerEvent) {
	t.trigMu.Lock()
	t.pendingTriggers = append(t.pendingTriggers, ev)
	t.trigMu.Unlock()
	t.Notify()
}

func (t *Task) drainTriggers() []agent.TriggerEvent {
	t.trigMu.Lock()
	defer t.trigMu.Unlock()
	out := t.pendingTriggers
	t.pendingTriggers = nil
	return out
}

func (t *Task) NotifyDone(intentID int64) {
	t.appendTrigger(agent.TriggerEvent{Kind: agent.TriggerIntentDone, IntentID: intentID})
}

func (t *Task) NotifyFinding(intentID int64, summary string) {
	t.appendTrigger(agent.TriggerEvent{Kind: agent.TriggerFinding, IntentID: intentID, Summary: summary})
}

// Manager 持有任务句柄集合，并把生命周期写入同时落到 DB 与内存快照。
type Manager struct {
	pg *db.DB

	mu    sync.RWMutex
	tasks map[int64]*Task

	hooksFor func(t *Task) harness.HookRunner
}

func NewManager(pg *db.DB) *Manager {
	return &Manager{pg: pg, tasks: map[int64]*Task{}}
}

func (m *Manager) DB() *db.DB { return m.pg }

// SetHookFactory 装配任务的控制面 Hook（通常为 guard.Guard）；须在创建/加载任务之前调用，
// Planner（LLMService.Resolve）与 Worker（Engine.runIntent 的 steerHooks）都经 Task.Hooks 取得它。
func (m *Manager) SetHookFactory(f func(t *Task) harness.HookRunner) {
	m.mu.Lock()
	m.hooksFor = f
	m.mu.Unlock()
}

func (m *Manager) attachHooks(t *Task) {
	m.mu.RLock()
	f := m.hooksFor
	m.mu.RUnlock()
	if f != nil {
		t.Hooks = f(t)
	}
}

// CreateTask 创建任务并写入初始目标；任务以 created 状态返回，由 Engine.Run 准入。
func (m *Manager) CreateTask(ctx context.Context, in db.CreateTaskInput, goals ...string) (*Task, error) {
	row, err := m.pg.CreateTask(ctx, in)
	if err != nil {
		return nil, err
	}
	t := newTask(m.pg, row, in.Goal)
	m.attachHooks(t)
	for _, g := range goals {
		if _, err := t.Store.AddGoalTx(ctx, map[string]any{"summary": g}, nil); err != nil {
			return nil, fmt.Errorf("add goal: %w", err)
		}
	}
	m.mu.Lock()
	m.tasks[t.ID] = t
	m.mu.Unlock()
	return t, nil
}

// LoadTask 从 DB 加载（或返回已缓存的）任务句柄。
func (m *Manager) LoadTask(ctx context.Context, id int64) (*Task, error) {
	if t := m.Task(id); t != nil {
		return t, nil
	}
	row, err := m.pg.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	var goal string
	if err := m.pg.QueryRowContext(ctx, `SELECT goal FROM explorations WHERE id = $1`, row.ExplorationID).Scan(&goal); err != nil {
		return nil, err
	}
	t := newTask(m.pg, row, goal)
	m.attachHooks(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.tasks[id]; ok {
		return existing, nil
	}
	m.tasks[id] = t
	return t, nil
}

// Forget 移除任务句柄（任务删除后调用）。
func (m *Manager) Forget(id int64) {
	m.mu.Lock()
	delete(m.tasks, id)
	m.mu.Unlock()
}

func (m *Manager) Task(id int64) *Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tasks[id]
}

func (m *Manager) taskByExploration(expID int64) *Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.ExpID == expID {
			return t
		}
	}
	return nil
}

func (m *Manager) TaskStatus(id int64) string {
	if t := m.Task(id); t != nil {
		return t.lifecycleSnapshot().Status
	}
	return ""
}

func (m *Manager) SetTaskStatus(ctx context.Context, t *Task, status string) error {
	if err := m.pg.SetTaskStatus(ctx, t.ID, status); err != nil {
		return err
	}
	t.setLifecycle(func(l *lifecycle) { l.Status = status })
	return nil
}

// SetTaskStatusGuarded 仅在任务未终态时写入；返回是否由本次调用写入终态。
func (m *Manager) SetTaskStatusGuarded(ctx context.Context, t *Task, status string) (bool, error) {
	won, err := m.pg.SetTaskStatusGuarded(ctx, t.ID, status)
	if err != nil || !won {
		return won, err
	}
	t.setLifecycle(func(l *lifecycle) { l.Status = status })
	return true, nil
}

// AddGoal 追加目标并唤醒 Planner；已终态任务由调用方经 Engine.Revive 重新准入。
func (m *Manager) AddGoal(ctx context.Context, t *Task, summary string) (int64, error) {
	id, err := t.Store.AddGoalTx(ctx, map[string]any{"summary": summary}, nil)
	if err != nil {
		return 0, err
	}
	t.appendTrigger(agent.TriggerEvent{Kind: agent.TriggerGoalAdded, Goals: []string{summary}})
	return id, nil
}

// NotifyDocument 告知 Planner 有新招标文档上传，并唤醒它。
func (t *Task) NotifyDocument(summary, detail string) {
	t.appendTrigger(agent.TriggerEvent{Kind: agent.TriggerDocumentAdded, Summary: summary, Detail: detail})
}

// AddHint 追加人工补充上下文并唤醒 Planner。
func (m *Manager) AddHint(ctx context.Context, t *Task, text string, anchors []int64) (int64, error) {
	id, err := t.Store.AddNode(ctx, db.KindHint, db.StateHintActive, map[string]any{"summary": text}, 0, "", anchors)
	if err != nil {
		return 0, err
	}
	t.appendTrigger(agent.TriggerEvent{Kind: agent.TriggerHintAdded, Hints: []string{text}})
	return id, nil
}
