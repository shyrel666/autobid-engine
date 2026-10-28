package llmpool

import (
	"context"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/internal/llm"
)

type fake struct {
	name   string
	calls  atomic.Int32
	events []llm.StreamEvent
	err    error
}

func (f *fake) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	f.calls.Add(1)
	if f.err != nil {
		return llm.Message{}, "", llm.Usage{}, f.err
	}
	return llm.UserText(f.name), llm.StopEndTurn, llm.Usage{}, nil
}

func (f *fake) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		f.calls.Add(1)
		for _, ev := range f.events {
			if !yield(ev, nil) {
				return
			}
		}
		if f.err != nil {
			yield(llm.StreamEvent{}, f.err)
		}
	}
}

func member(id int64, rank int, p llm.Provider) *Member {
	return &Member{ID: id, Name: "m", Rank: rank, Active: true, Prov: p}
}

var (
	e503 = &llm.APIError{StatusCode: 503}
	e401 = &llm.APIError{StatusCode: 401}
	e400 = &llm.APIError{StatusCode: 400}
	meta = llm.StreamEvent{Type: llm.SEMessageStart}
)

func drain(seq iter.Seq2[llm.StreamEvent, error]) (n int, err error) {
	for _, e := range seq {
		if e != nil {
			return n, e
		}
		n++
	}
	return n, nil
}

func TestStreamFailsOverOnlyBeforeAnyEvent(t *testing.T) {
	bad, good := &fake{err: e503}, &fake{events: []llm.StreamEvent{meta}}
	pool := New([]*Member{member(1, 0, bad), member(2, 1, good)}, nil)
	if n, err := drain(pool.Stream(context.Background(), llm.CompletionRequest{})); err != nil || n != 1 || good.calls.Load() != 1 {
		t.Fatalf("failover before emit: n=%d err=%v", n, err)
	}

	emitted := &fake{events: []llm.StreamEvent{meta}, err: e503}
	other := &fake{events: []llm.StreamEvent{meta}}
	pool = New([]*Member{member(3, 0, emitted), member(4, 1, other)}, nil)
	if _, err := drain(pool.Stream(context.Background(), llm.CompletionRequest{})); err == nil || other.calls.Load() != 0 {
		t.Fatalf("must not fail over after an event was emitted: err=%v other=%d", err, other.calls.Load())
	}
}

func TestNoFailoverOn400OrCancel(t *testing.T) {
	bad, good := &fake{err: e400}, &fake{}
	pool := New([]*Member{member(1, 0, bad), member(2, 1, good)}, nil)
	if _, _, _, err := pool.Complete(context.Background(), llm.CompletionRequest{}); err == nil || good.calls.Load() != 0 {
		t.Fatalf("400 must not fail over: err=%v", err)
	}
	cancelled := &fake{err: context.Canceled}
	pool = New([]*Member{member(3, 0, cancelled), member(4, 1, good)}, nil)
	if _, _, _, err := pool.Complete(context.Background(), llm.CompletionRequest{}); err == nil || good.calls.Load() != 0 {
		t.Fatalf("cancel must not fail over: err=%v", err)
	}
}

func TestCircuitBreakerTrips(t *testing.T) {
	var persisted atomic.Int32
	reg := NewRegistry(func(int64, State) { persisted.Add(1) }, nil)
	now := time.Now()
	reg.now = func() time.Time { return now }

	reg.Fail(1, e401)
	if reg.Available(1) {
		t.Fatal("hard failure must trip immediately")
	}
	for range 2 {
		reg.Fail(2, e503)
	}
	if !reg.Available(2) {
		t.Fatal("two soft failures must not trip")
	}
	reg.Fail(2, e503)
	st, _ := reg.Snapshot(2)
	if reg.Available(2) || st.Trips != 1 || st.OpenUntil.Sub(now) != time.Minute {
		t.Fatalf("third soft failure must trip for 1m: %+v", st)
	}
	now = now.Add(2 * time.Minute)
	if !reg.Available(2) {
		t.Fatal("cooldown elapsed, member should be probed again")
	}
	reg.Pass(2)
	if _, ok := reg.Snapshot(2); ok || persisted.Load() != 4 {
		t.Fatalf("pass must clear state; persisted=%d", persisted.Load())
	}
}

func TestSameRankRoundRobinAndTrippedSkip(t *testing.T) {
	a, b := &fake{name: "a"}, &fake{name: "b"}
	reg := NewRegistry(nil, nil)
	pool := New([]*Member{member(1, 0, a), member(2, 0, b)}, reg)
	for range 4 {
		if _, _, _, err := pool.Complete(context.Background(), llm.CompletionRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if a.calls.Load() != 2 || b.calls.Load() != 2 {
		t.Fatalf("round robin: a=%d b=%d", a.calls.Load(), b.calls.Load())
	}
	reg.Fail(1, e401)
	for range 3 {
		_, _, _, _ = pool.Complete(context.Background(), llm.CompletionRequest{})
	}
	if a.calls.Load() != 2 {
		t.Fatalf("tripped member must be skipped: a=%d", a.calls.Load())
	}
	reg.Fail(2, e401)
	_, _, _, _ = pool.Complete(context.Background(), llm.CompletionRequest{})
	if a.calls.Load()+b.calls.Load() != 8 {
		t.Fatal("when every member is tripped the head member is still probed")
	}
}
