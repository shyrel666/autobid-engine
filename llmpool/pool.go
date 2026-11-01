// Package llmpool 把一组 Provider 包装为一个按 rank、健康状态与上下文窗口选择成员的可用性池（§5.6）。
// 它不是工作队列，也不限制 Worker 数。
package llmpool

import (
	"context"
	"errors"
	"iter"
	"sort"
	"sync/atomic"

	"github.com/autobid/autobid-engine/internal/llm"
)

// Member 是池成员（附录 A.16）。
type Member struct {
	ID           int64
	Name         string
	Model        string
	Format       string
	Priority     int
	Active       bool
	Rank         int
	WindowTokens int // 0 表示不按窗口过滤
	Prov         llm.Provider
}

type Pool struct {
	members []*Member
	health  *Registry
	rr      atomic.Uint64
}

var ErrEmptyPool = errors.New("llmpool: no members")

// New 按 rank 升序、priority 降序、ID 升序固定成员顺序。
func New(members []*Member, health *Registry) *Pool {
	ms := append([]*Member(nil), members...)
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Rank != ms[j].Rank {
			return ms[i].Rank < ms[j].Rank
		}
		if ms[i].Priority != ms[j].Priority {
			return ms[i].Priority > ms[j].Priority
		}
		return ms[i].ID < ms[j].ID
	})
	if health == nil {
		health = NewRegistry(nil, nil)
	}
	return &Pool{members: ms, health: health}
}

func (p *Pool) Members() []*Member { return p.members }

// estimateTokens 粗估请求 token 数（约 3 字节/token），只用于窗口过滤。
func estimateTokens(req llm.CompletionRequest) int {
	n := len(req.System)
	for _, m := range req.Messages {
		for _, b := range m.Content {
			n += len(b.Text) + len(b.Input)
		}
	}
	return n / 3
}

// candidates 返回本次请求的尝试顺序：同 rank 内轮转；按启用、健康与窗口过滤。
// 全部被过滤时仍试探头部成员（不是硬容量拒绝）。
func (p *Pool) candidates(req llm.CompletionRequest) []*Member {
	if len(p.members) == 0 {
		return nil
	}
	turn := int(p.rr.Add(1) - 1)
	need := estimateTokens(req)
	var ordered []*Member
	for i := 0; i < len(p.members); {
		j := i
		for j < len(p.members) && p.members[j].Rank == p.members[i].Rank && p.members[j].Priority == p.members[i].Priority {
			j++
		}
		group := p.members[i:j]
		for k := range group {
			ordered = append(ordered, group[(k+turn)%len(group)])
		}
		i = j
	}
	var out []*Member
	for _, m := range ordered {
		if !m.Active || !p.health.Available(m.ID) {
			continue
		}
		if m.WindowTokens > 0 && need > m.WindowTokens {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		for _, m := range ordered {
			if m.Active {
				return []*Member{m}
			}
		}
		return ordered[:1]
	}
	return out
}

func (p *Pool) record(m *Member, err error) {
	switch {
	case err == nil:
		p.health.Pass(m.ID)
	case llm.ShouldFailover(err):
		p.health.Fail(m.ID, err)
	}
}

func (p *Pool) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	lastErr := ErrEmptyPool
	for _, m := range p.candidates(req) {
		msg, stop, usage, err := m.Prov.Complete(ctx, req)
		p.record(m, err)
		if llm.SingleAttempt(ctx) {
			return msg, stop, usage, err
		}
		if err == nil {
			return msg, stop, usage, nil
		}
		lastErr = err
		if !llm.ShouldFailover(err) || ctx.Err() != nil {
			break
		}
	}
	return llm.Message{}, "", llm.Usage{}, lastErr
}

// Stream buffers metadata until observable output, then never replays that output.
func (p *Pool) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		lastErr := ErrEmptyPool
		for _, m := range p.candidates(req) {
			emitted := false
			var pending []llm.StreamEvent
			var streamErr error
			for ev, err := range m.Prov.Stream(ctx, req) {
				if err != nil {
					streamErr = err
					break
				}
				if !emitted && !llm.CommitsOutput(ev) {
					pending = append(pending, ev)
					continue
				}
				if !emitted {
					for _, meta := range pending {
						if !yield(meta, nil) {
							return
						}
					}
					pending = nil
					emitted = true
				}
				if !yield(ev, nil) {
					return
				}
			}
			p.record(m, streamErr)
			if streamErr == nil {
				for _, meta := range pending {
					if !yield(meta, nil) {
						return
					}
				}
				return
			}
			lastErr = streamErr
			if emitted || llm.SingleAttempt(ctx) || !llm.ShouldFailover(streamErr) || ctx.Err() != nil {
				break
			}
		}
		yield(llm.StreamEvent{}, lastErr)
	}
}
