// Package intercepttest 提供 intercept.Store 的内存实现，语义对齐 db 层（status='pending' 条件 CAS、等待方心跳），供测试使用。
package intercepttest

import (
	"context"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
)

// Store 是 intercept.Store 的内存实现，语义对齐 db 层：ResolveIntercept 为 status='pending' 条件 CAS。
type Store struct {
	Mu          sync.Mutex
	Rules       []db.InterceptRule
	Cfg         db.InterceptConfig
	rows        map[int64]*db.InterceptPendingRow
	Audit       []db.InterceptAuditRow
	next        int64
	AfterCreate func(id int64) // 在 INSERT “提交”之后、HandleAsk 注册 Channel 之前回调
}

// New 创建内存 Store；规则统一置为启用，未设 ID 时按位置编号。
func New(rules ...db.InterceptRule) *Store {
	for k := range rules {
		rules[k].Enabled = true
		if rules[k].ID == 0 {
			rules[k].ID = int64(k + 1)
		}
	}
	return &Store{Rules: rules, rows: map[int64]*db.InterceptPendingRow{}}
}

func (m *Store) EnabledInterceptRules(context.Context) ([]db.InterceptRule, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return append([]db.InterceptRule(nil), m.Rules...), nil
}

func (m *Store) GetInterceptConfig(context.Context) (db.InterceptConfig, error) {
	return m.Cfg, nil
}

func (m *Store) CreateInterceptPending(_ context.Context, p db.NewInterceptPending) (int64, error) {
	m.Mu.Lock()
	m.next++
	id := m.next
	until := time.Now().Add(p.WaiterTTL)
	m.rows[id] = &db.InterceptPendingRow{ID: id, RuleID: p.RuleID, TaskID: p.TaskID, ToolName: p.ToolName,
		ToolInput: string(p.ToolInput), Status: db.InterceptPending, TimeoutAction: p.TimeoutAction, WaiterUntil: &until}
	cb := m.AfterCreate
	m.Mu.Unlock()
	if cb != nil {
		cb(id)
	}
	return id, nil
}

func (m *Store) GetInterceptPending(_ context.Context, id int64) (*db.InterceptPendingRow, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return nil, db.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (m *Store) ResolveIntercept(_ context.Context, id int64, status, action, reason string, live bool) (bool, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.Status != db.InterceptPending || (live && !r.WaiterUntil.After(time.Now())) {
		return false, nil
	}
	now := time.Now()
	r.Status, r.EffectiveAction, r.Reason, r.DecidedAt = status, action, reason, &now
	return true, nil
}

func (m *Store) TouchInterceptWaiter(_ context.Context, id int64, ttl time.Duration) (bool, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.Status != db.InterceptPending {
		return false, nil
	}
	until := time.Now().Add(ttl)
	r.WaiterUntil = &until
	return true, nil
}

func (m *Store) CompleteIntercept(_ context.Context, id int64, outcome, detail string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if r, ok := m.rows[id]; ok {
		r.ExecOutcome, r.ExecDetail = outcome, detail
	}
	return nil
}

func (m *Store) ReapOrphanedIntercepts(context.Context) ([]int64, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	var out []int64
	for id, r := range m.rows {
		if r.Status == db.InterceptPending && !r.WaiterUntil.After(time.Now()) {
			r.Status, r.EffectiveAction, r.ExecOutcome = db.InterceptDenied, "deny", "not_executed"
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *Store) InsertInterceptAudit(_ context.Context, a db.InterceptAuditRow) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.Audit = append(m.Audit, a)
	return nil
}

// Row 返回记录快照。
func (m *Store) Row(id int64) db.InterceptPendingRow {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return *m.rows[id]
}

// ExpireWaiter 让等待方心跳立即过期（模拟进程失联）。
func (m *Store) ExpireWaiter(id int64) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	past := time.Now().Add(-time.Second)
	m.rows[id].WaiterUntil = &past
}
