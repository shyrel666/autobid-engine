package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// doneIntentWithFact 造一个已完成意图及其事实，返回 (intent, fact)。
func (f *fixture) doneIntentWithFact(t *testing.T, summary string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	in := f.intent(t)
	epoch, ok, err := f.es.ClaimIntentWithLease(ctx, in, "w", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	fact, err := f.es.RecordFactWithYieldsTx(ctx, in, map[string]any{"summary": summary}, 0, "w", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.es.CompareAndSetIntentStateFenced(ctx, in, StateIntentRunning, StateIntentDone, epoch); !ok {
		t.Fatal("finish intent")
	}
	return in, fact
}

func (f *fixture) advance(t *testing.T, rounds int) int64 {
	t.Helper()
	var r int64
	for range rounds {
		var err error
		if r, err = f.es.AdvanceRound(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func overviewIDs(bs []NodeBrief) map[int64]bool {
	m := map[int64]bool{}
	for _, b := range bs {
		m[b.ID] = true
	}
	return m
}

func TestColdStampingAndReheat(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.advance(t, 1)
	in, fact := f.doneIntentWithFact(t, "资质 4.2 满足")
	goal, _ := f.es.AddGoalTx(ctx, map[string]any{"summary": "g"}, nil)

	round := f.advance(t, 2)
	if n, _ := f.es.StampColdNodes(ctx, round, 3); n != 0 {
		t.Fatalf("too early to cool: %d", n)
	}
	round = f.advance(t, 2)
	if n, err := f.es.StampColdNodes(ctx, round, 3); err != nil || n != 2 {
		t.Fatalf("stamp: n=%d err=%v", n, err)
	}
	if g, _ := f.es.GetNode(ctx, goal); g.ColdSinceRound != nil {
		t.Fatal("goals must never cool")
	}
	o, err := f.es.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.ColdIndex) != 2 || overviewIDs(o.RecentFacts)[fact] || overviewIDs(o.RecentTerminal)[in] {
		t.Fatalf("cold nodes must move to cold_index: %+v", o)
	}

	// 任何状态写入都会回温。
	if err := f.es.SetNodeState(ctx, fact, StateDismissed); err != nil {
		t.Fatal(err)
	}
	n, _ := f.es.GetNode(ctx, fact)
	if n.ColdSinceRound != nil || n.ActiveRound != round {
		t.Fatalf("reheat: cold=%v active_round=%d want %d", n.ColdSinceRound, n.ActiveRound, round)
	}
}

func TestDigestFoldSupersedeAndStale(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	round := f.advance(t, 1)
	in1, fact1 := f.doneIntentWithFact(t, "评分项 PF-1 满分")
	_, fact2 := f.doneIntentWithFact(t, "评分项 PF-2 得分 8/10")

	a, sup, err := f.es.CreateDigest(ctx, "PF-1", "PF-1 已满分", []int64{in1, fact1}, round)
	if err != nil || len(sup) != 0 {
		t.Fatalf("digest a: %v %v", err, sup)
	}
	o, _ := f.es.Overview(ctx)
	if overviewIDs(o.RecentFacts)[fact1] || !overviewIDs(o.RecentFacts)[fact2] || len(o.Digests) != 1 || o.FoldedNodes != 2 {
		t.Fatalf("fold: facts=%+v digests=%+v folded=%d", o.RecentFacts, o.Digests, o.FoldedNodes)
	}

	// 新摘要完全覆盖旧摘要成员 -> 旧摘要 superseded 且其 covers 边删除。
	b, sup, err := f.es.CreateDigest(ctx, "评分项", "PF-1 满分，PF-2 8 分", []int64{in1, fact1, fact2}, round)
	if err != nil || len(sup) != 1 || sup[0] != a {
		t.Fatalf("digest b: %v superseded=%v", err, sup)
	}
	if n, _ := f.es.GetNode(ctx, a); n.State != StateDigestSuperseded {
		t.Fatalf("old digest state %s", n.State)
	}
	if edges, _ := f.es.NodeEdges(ctx, a, 10); len(edges) != 0 {
		t.Fatalf("superseded digest still has covers edges: %+v", edges)
	}
	// 只覆盖部分成员的新摘要不取代 b。
	if _, sup, err := f.es.CreateDigest(ctx, "PF-2", "8 分", []int64{fact2}, round); err != nil || len(sup) != 0 {
		t.Fatalf("partial digest: %v %v", err, sup)
	}

	// 成员变化 -> 摘要过期，成员重新展开。
	if err := f.es.SetNodeState(ctx, fact1, StateDismissed); err != nil {
		t.Fatal(err)
	}
	views, err := f.es.ActiveDigestsWithMembers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var bv *DigestView
	for _, v := range views {
		if v.Node.ID == b {
			bv = v
		}
	}
	if bv == nil || !bv.Stale || len(bv.ChangedMembers) != 1 || bv.ChangedMembers[0] != fact1 {
		t.Fatalf("stale detection: %+v", bv)
	}
	o, _ = f.es.Overview(ctx)
	if !overviewIDs(o.RecentTerminal)[in1] {
		t.Fatal("members of a stale digest must be expanded again")
	}
}

func TestDigestRejectsLiveMembersAndConsumeHint(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	goal, _ := f.es.AddGoalTx(ctx, nil, nil)
	open := f.intent(t)
	for _, members := range [][]int64{{goal}, {open}, {f.origin}, {1 << 60}, {}} {
		if _, _, err := f.es.CreateDigest(ctx, "t", "b", members, 1); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("members %v: err=%v", members, err)
		}
	}
	hint, _ := f.es.AddNode(ctx, KindHint, StateHintActive, map[string]any{"summary": "h"}, 0, "", nil)
	if ok, err := f.es.ConsumeHint(ctx, hint); !ok || err != nil {
		t.Fatalf("consume: %v %v", ok, err)
	}
	if ok, _ := f.es.ConsumeHint(ctx, hint); ok {
		t.Fatal("double consume must be a no-op")
	}
	if ok, _ := f.es.ConsumeHint(ctx, goal); ok {
		t.Fatal("only hints can be consumed")
	}
}
