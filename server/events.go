package server

import (
	"sync"
	"sync/atomic"

	"github.com/autobid/autobid-engine/db"
)

// Hub 把 db.Activity 按任务扇出给 SSE 订阅者。发布永不阻塞调度循环：订阅者缓冲区满时丢弃并计数。
type Hub struct {
	mu   sync.RWMutex
	subs map[int64]map[*Subscription]struct{}
	seq  atomic.Int64
}

type Subscription struct {
	C       chan HubEvent
	dropped atomic.Int64
}

// HubEvent 是带全局递增序号的活动事件（序号用作 SSE id）。
type HubEvent struct {
	Seq      int64
	Activity db.Activity
}

// Dropped 返回因订阅者消费过慢而丢弃的事件数。
func (s *Subscription) Dropped() int64 { return s.dropped.Load() }

func NewHub() *Hub { return &Hub{subs: map[int64]map[*Subscription]struct{}{}} }

// Publish 投递一条活动；没有订阅者时直接丢弃（活动流不持久化）。
func (h *Hub) Publish(a db.Activity) {
	ev := HubEvent{Seq: h.seq.Add(1), Activity: a}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs[a.TaskID] {
		select {
		case s.C <- ev:
		default:
			s.dropped.Add(1)
		}
	}
}

// Subscribe 订阅任务活动；返回的取消函数必须调用。
func (h *Hub) Subscribe(taskID int64, buffer int) (*Subscription, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	s := &Subscription{C: make(chan HubEvent, buffer)}
	h.mu.Lock()
	if h.subs[taskID] == nil {
		h.subs[taskID] = map[*Subscription]struct{}{}
	}
	h.subs[taskID][s] = struct{}{}
	h.mu.Unlock()
	return s, func() {
		h.mu.Lock()
		delete(h.subs[taskID], s)
		if len(h.subs[taskID]) == 0 {
			delete(h.subs, taskID)
		}
		h.mu.Unlock()
	}
}
