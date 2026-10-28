package server

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
)

// fakeStream 依次回放每次调用的脚本：events 先产出，然后返回 err（可为 nil）。
type fakeStream struct {
	calls   atomic.Int32
	scripts []streamScript
}

type streamScript struct {
	events []llm.StreamEvent
	err    error
}

func (f *fakeStream) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	i := int(f.calls.Add(1)) - 1
	s := f.scripts[min(i, len(f.scripts)-1)]
	if s.err != nil {
		return llm.Message{}, "", llm.Usage{}, s.err
	}
	return llm.UserText("ok"), llm.StopEndTurn, llm.Usage{}, nil
}

func (f *fakeStream) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		i := int(f.calls.Add(1)) - 1
		s := f.scripts[min(i, len(f.scripts)-1)]
		for _, ev := range s.events {
			if !yield(ev, nil) {
				return
			}
		}
		if s.err != nil {
			yield(llm.StreamEvent{}, s.err)
		}
	}
}

var (
	evStart = llm.StreamEvent{Type: llm.SEMessageStart}
	evText  = llm.StreamEvent{Type: llm.SETextDelta, Text: "hi"}
	evStop  = llm.StreamEvent{Type: llm.SEMessageStop}
	err500  = &llm.APIError{Provider: "fake", StatusCode: 500, Type: "api_error"}
	err400  = &llm.APIError{Provider: "fake", StatusCode: 400, Type: "invalid_request_error"}
	errQuot = &llm.APIError{Provider: "fake", StatusCode: 400, Type: "invalid_request_error", Message: "Your credit balance is too low"}
)

func collect(seq iter.Seq2[llm.StreamEvent, error]) (evs []llm.StreamEvent, err error) {
	for ev, e := range seq {
		if e != nil {
			return evs, e
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

func fixedHooks(p llm.Provider, profileID int64) taskLLMStreamHooks {
	return taskLLMStreamHooks{
		current: func() (taskLLMSelection, error) {
			return taskLLMSelection{provider: p, profileID: profileID, retry: RetryConfig{Backoff: time.Millisecond}}, nil
		},
	}
}

func TestStreamRetriesBeforeCommitWithoutDuplicatingMeta(t *testing.T) {
	p := &fakeStream{scripts: []streamScript{
		{events: []llm.StreamEvent{evStart}, err: err500},
		{events: []llm.StreamEvent{evStart, evText, evStop}},
	}}
	evs, err := collect(streamTaskLLM(context.Background(), 1, llm.CompletionRequest{}, fixedHooks(p, 0)))
	if err != nil || len(evs) != 3 || p.calls.Load() != 2 {
		t.Fatalf("evs=%d err=%v calls=%d", len(evs), err, p.calls.Load())
	}
}

func TestStreamDoesNotRetryAfterCommit(t *testing.T) {
	p := &fakeStream{scripts: []streamScript{
		{events: []llm.StreamEvent{evStart, evText}, err: err500},
		{events: []llm.StreamEvent{evStart, evText, evStop}},
	}}
	evs, err := collect(streamTaskLLM(context.Background(), 1, llm.CompletionRequest{}, fixedHooks(p, 0)))
	if !errors.Is(err, err500) || len(evs) != 2 || p.calls.Load() != 1 {
		t.Fatalf("evs=%d err=%v calls=%d", len(evs), err, p.calls.Load())
	}
}

func TestStreamDoesNotRetryNonTransient(t *testing.T) {
	p := &fakeStream{scripts: []streamScript{{err: err400}, {events: []llm.StreamEvent{evText}}}}
	_, err := collect(streamTaskLLM(context.Background(), 1, llm.CompletionRequest{}, fixedHooks(p, 0)))
	if !errors.Is(err, err400) || p.calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, p.calls.Load())
	}
}

func TestStreamQuotaOutsideChainPassesThrough(t *testing.T) {
	p := &fakeStream{scripts: []streamScript{{err: errQuot}}}
	_, err := collect(streamTaskLLM(context.Background(), 1, llm.CompletionRequest{}, fixedHooks(p, 0)))
	var tle *taskLLMError
	if errors.As(err, &tle) || !errors.Is(err, errQuot) {
		t.Fatalf("non-chain quota error must pass through: %v", err)
	}
}

// chainHooks 模拟两段配置链：A 配额耗尽后游标推进到 B。
func chainHooks(a, b llm.Provider, nextAvailable bool) (taskLLMStreamHooks, *int) {
	active := int64(1)
	transitions := 0
	return taskLLMStreamHooks{
		current: func() (taskLLMSelection, error) {
			if active == 1 {
				return taskLLMSelection{provider: a, profileID: 1, revision: 1, retry: RetryConfig{Attempts: -1}}, nil
			}
			return taskLLMSelection{provider: b, profileID: 2, revision: 2, retry: RetryConfig{Attempts: -1}}, nil
		},
		exhaust: func(sel taskLLMSelection, _ error) (db.TaskLLMTransition, error) {
			if !nextAvailable {
				return db.TaskLLMTransition{PreviousProfileID: sel.profileID, Advanced: true, ChainExhausted: true}, nil
			}
			next := int64(2)
			active = 2
			return db.TaskLLMTransition{PreviousProfileID: sel.profileID, NextProfileID: &next, Advanced: true}, nil
		},
		transition: func(taskLLMSelection, db.TaskLLMTransition, error) { transitions++ },
	}, &transitions
}

func TestStreamQuotaBeforeCommitSwitchesProfileAndReplays(t *testing.T) {
	a := &fakeStream{scripts: []streamScript{{events: []llm.StreamEvent{evStart}, err: errQuot}}}
	b := &fakeStream{scripts: []streamScript{{events: []llm.StreamEvent{evStart, evText, evStop}}}}
	hooks, transitions := chainHooks(a, b, true)
	evs, err := collect(streamTaskLLM(context.Background(), 7, llm.CompletionRequest{}, hooks))
	if err != nil || len(evs) != 3 || *transitions != 1 || b.calls.Load() != 1 {
		t.Fatalf("evs=%d err=%v transitions=%d bcalls=%d", len(evs), err, *transitions, b.calls.Load())
	}
}

func TestStreamQuotaAfterCommitAdvancesCursorButFailsCurrentCall(t *testing.T) {
	a := &fakeStream{scripts: []streamScript{{events: []llm.StreamEvent{evStart, evText}, err: errQuot}}}
	b := &fakeStream{scripts: []streamScript{{events: []llm.StreamEvent{evText}}}}
	hooks, transitions := chainHooks(a, b, true)
	evs, err := collect(streamTaskLLM(context.Background(), 7, llm.CompletionRequest{}, hooks))
	var tle *taskLLMError
	if !errors.As(err, &tle) || tle.chainExhausted || !isTaskLLMRuntimeError(err) || isTaskLLMChainExhausted(err) {
		t.Fatalf("want non-exhausted taskLLMError, got %v", err)
	}
	if len(evs) != 2 || b.calls.Load() != 0 || *transitions != 1 {
		t.Fatalf("committed stream must not be replayed: evs=%d bcalls=%d transitions=%d", len(evs), b.calls.Load(), *transitions)
	}
}

func TestStreamChainExhausted(t *testing.T) {
	a := &fakeStream{scripts: []streamScript{{err: errQuot}}}
	hooks, _ := chainHooks(a, nil, false)
	_, err := collect(streamTaskLLM(context.Background(), 7, llm.CompletionRequest{}, hooks))
	if !isTaskLLMChainExhausted(err) || retryableWorkerModelError("model_error", err) {
		t.Fatalf("want chain exhausted taskLLMError that the engine does not retry, got %v", err)
	}
}

func TestCompleteQuotaSwitchesProfile(t *testing.T) {
	a := &fakeStream{scripts: []streamScript{{err: errQuot}}}
	b := &fakeStream{scripts: []streamScript{{}}}
	hooks, transitions := chainHooks(a, b, true)
	_, _, _, err := completeTaskLLM(context.Background(), 7, llm.CompletionRequest{}, hooks)
	if err != nil || *transitions != 1 || b.calls.Load() != 1 {
		t.Fatalf("err=%v transitions=%d bcalls=%d", err, *transitions, b.calls.Load())
	}
}
