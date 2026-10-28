package llmpool

import (
	"sync"
	"time"

	"github.com/autobid/autobid-engine/internal/llm"
)

// State 是单个成员的熔断状态（附录 A.17）。
type State struct {
	Fails     int
	Trips     int
	OpenUntil time.Time
	LastError string
	LastAt    time.Time
}

// DefaultCooldowns 是熔断冷却梯度：第 1/2/3+ 次熔断分别冷却 1、5、30 分钟。
var DefaultCooldowns = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// Registry 记录“暂时熔断”（与任务链的“配额耗尽”是不同状态）。持久化回调在解锁后调用。
type Registry struct {
	mu        sync.Mutex
	m         map[int64]*State
	persist   func(id int64, st State)
	forget    func(id int64)
	softTrip  int
	cooldowns []time.Duration
	now       func() time.Time
}

func NewRegistry(persist func(id int64, st State), forget func(id int64)) *Registry {
	return &Registry{
		m:         map[int64]*State{},
		persist:   persist,
		forget:    forget,
		softTrip:  3,
		cooldowns: DefaultCooldowns,
		now:       time.Now,
	}
}

// Restore 在启动时恢复仍有效的熔断。
func (r *Registry) Restore(id int64, st State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := st
	r.m[id] = &s
}

// Available 报告成员当前是否未处于熔断冷却期。
func (r *Registry) Available(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.m[id]
	return st == nil || !r.now().Before(st.OpenUntil)
}

func (r *Registry) Snapshot(id int64) (State, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.m[id]
	if !ok {
		return State{}, false
	}
	return *st, true
}

// Pass 在成功调用后清理故障状态。
func (r *Registry) Pass(id int64) {
	r.mu.Lock()
	_, had := r.m[id]
	delete(r.m, id)
	r.mu.Unlock()
	if had && r.forget != nil {
		r.forget(id)
	}
}

// Fail 记录一次可换池故障：hard failure（401/402/403/404）首次即熔断，其余连续 softTrip 次熔断。
func (r *Registry) Fail(id int64, err error) {
	r.mu.Lock()
	st := r.m[id]
	if st == nil {
		st = &State{}
		r.m[id] = st
	}
	now := r.now()
	st.Fails++
	st.LastAt = now
	if err != nil {
		st.LastError = err.Error()
	}
	if llm.IsHardFailure(err) || st.Fails >= r.softTrip {
		st.Trips++
		idx := min(st.Trips-1, len(r.cooldowns)-1)
		st.OpenUntil = now.Add(r.cooldowns[idx])
		st.Fails = 0
	}
	snap := *st
	r.mu.Unlock()
	if r.persist != nil {
		r.persist(id, snap)
	}
}
