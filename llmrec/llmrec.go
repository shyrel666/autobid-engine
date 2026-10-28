// Package llmrec 在路由选择之后包装每个实际 Profile Provider，为每次真实调用写一行 token 账本（§5.7）。
// 账本独立于正文录制与任务活动；写账失败不阻塞主执行。
package llmrec

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
)

// Attribution 标注一次调用的归属；Worker 为调用通道（planner / worker / judge）。
type Attribution struct {
	TaskID        int64
	ExplorationID int64
	Worker        string
}

type attrKey struct{}

func WithAttribution(ctx context.Context, a Attribution) context.Context {
	return context.WithValue(ctx, attrKey{}, a)
}

func AttributionFrom(ctx context.Context) Attribution {
	a, _ := ctx.Value(attrKey{}).(Attribution)
	return a
}

// Profile 描述被包装 Provider 的实际配置。
type Profile struct {
	ID     int64
	Name   string
	Model  string
	Effort string
}

// Sink 写入一行账本；为 nil 时只透传。
type Sink func(ctx context.Context, u db.LLMUsage) error

type Recorder struct {
	inner   llm.Provider
	sink    Sink
	profile Profile
	onError func(error)
}

func New(inner llm.Provider, sink Sink, profile Profile, onError func(error)) *Recorder {
	return &Recorder{inner: inner, sink: sink, profile: profile, onError: onError}
}

func (r *Recorder) write(ctx context.Context, streaming bool, start time.Time, usage llm.Usage, err error, cancelled bool) {
	if r.sink == nil {
		return
	}
	a := AttributionFrom(ctx)
	u := db.LLMUsage{
		TaskID: a.TaskID, ExplorationID: a.ExplorationID, SessionID: llm.SessionIDFrom(ctx), Worker: a.Worker,
		Model: r.profile.Model, ProfileID: r.profile.ID, ProfileName: r.profile.Name, Effort: r.profile.Effort,
		Streaming: streaming, LatencyMs: int(time.Since(start).Milliseconds()),
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheRead: usage.CacheReadTokens, CacheWrite: usage.CacheWriteTokens,
		Status: "ok",
	}
	switch {
	case cancelled || errors.Is(err, context.Canceled):
		u.Status = "cancelled"
	case err != nil:
		u.Status = "error"
	}
	if err != nil {
		u.Error = err.Error()
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if werr := r.sink(wctx, u); werr != nil && r.onError != nil {
		r.onError(werr)
	}
}

func (r *Recorder) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	start := time.Now()
	msg, stop, usage, err := r.inner.Complete(ctx, req)
	r.write(ctx, false, start, usage, err, false)
	return msg, stop, usage, err
}

// Stream 在正常结束、错误、取消或消费者提前停止时统一记账一次。
func (r *Recorder) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		start := time.Now()
		var (
			usage     llm.Usage
			streamErr error
			stopped   bool
			once      sync.Once
		)
		finalize := func() {
			once.Do(func() { r.write(ctx, true, start, usage, streamErr, stopped && streamErr == nil) })
		}
		defer finalize()
		for ev, err := range r.inner.Stream(ctx, req) {
			if err != nil {
				streamErr = err
				yield(ev, err)
				return
			}
			if ev.Usage != nil {
				usage = *ev.Usage
			}
			if !yield(ev, nil) {
				stopped = true
				return
			}
		}
	}
}
