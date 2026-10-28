package db

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestInterceptRulesOrderingAndChecks(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tag := uniq("rule")
	low, err := d.CreateInterceptRule(ctx, InterceptRule{Name: tag, ToolFilter: tag, Pattern: "a", Action: "deny", Priority: -1000, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	high, _ := d.CreateInterceptRule(ctx, InterceptRule{Name: tag, ToolFilter: tag, Pattern: "b", Action: "allow", Priority: 1000, Enabled: true})
	tie, _ := d.CreateInterceptRule(ctx, InterceptRule{Name: tag, ToolFilter: tag, Pattern: "c", Action: "ask", Priority: 1000, Enabled: true,
		TimeoutEnabled: true, TimeoutSeconds: 5, TimeoutAction: "allow-ish"}) // timeout_action 无 CHECK
	off, _ := d.CreateInterceptRule(ctx, InterceptRule{Name: tag, ToolFilter: tag, Pattern: "d", Action: "deny", Priority: 2000, Enabled: false})
	t.Cleanup(func() {
		for _, id := range []int64{low, high, tie, off} {
			_ = d.DeleteInterceptRule(ctx, id)
		}
	})

	rules, err := d.EnabledInterceptRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine []int64
	for _, r := range rules {
		if r.Name == tag {
			mine = append(mine, r.ID)
		}
	}
	if want := []int64{high, tie, low}; !slices.Equal(mine, want) {
		t.Fatalf("order = %v, want %v (priority DESC, id ASC, disabled filtered)", mine, want)
	}

	for _, bad := range []InterceptRule{
		{Pattern: "x", Action: "maybe"},
		{Pattern: "x", Action: "deny", MatchTarget: "tool_output"},
		{Pattern: "x", Action: "deny", MatchType: "glob"},
	} {
		if id, err := d.CreateInterceptRule(ctx, bad); err == nil {
			_ = d.DeleteInterceptRule(ctx, id)
			t.Fatalf("CHECK should reject %+v", bad)
		}
	}
}

func TestInterceptConfigRoundTrip(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	orig, err := d.GetInterceptConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.SaveInterceptConfig(ctx, orig) })

	if err := d.SaveInterceptConfig(ctx, InterceptConfig{EnabledTools: []string{"Bash", "record_fact"}, Judge: []byte(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetInterceptConfig(ctx)
	if !slices.Equal(got.EnabledTools, []string{"Bash", "record_fact"}) || string(got.Judge) != `{"enabled": true}` {
		t.Fatalf("config = %+v %s", got, got.Judge)
	}
	_ = d.SaveInterceptConfig(ctx, InterceptConfig{EnabledTools: []string{}})
	if got, _ := d.GetInterceptConfig(ctx); got.EnabledTools == nil || len(got.EnabledTools) != 0 {
		t.Fatalf("empty list must stay empty (not default): %#v", got.EnabledTools)
	}
	_ = d.SaveInterceptConfig(ctx, InterceptConfig{})
	if got, _ := d.GetInterceptConfig(ctx); got.EnabledTools != nil {
		t.Fatalf("nil must mean default set: %#v", got.EnabledTools)
	}
}

func TestInterceptPendingSingleDecision(t *testing.T) {
	f := newFixture(t)
	d, ctx := f.d, context.Background()
	id, err := d.CreateInterceptPending(ctx, NewInterceptPending{TaskID: f.task.ID, IntentID: 42, AgentName: "work#1",
		ToolName: "Bash", ToolInput: []byte(`{"cmd":"deploy"}`), Message: "需要审批", Waiter: "test", WaiterTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.GetInterceptPending(ctx, id)
	if err != nil || r.Status != InterceptPending || r.TaskID != f.task.ID || r.TimeoutAction != "deny" || r.WaiterUntil == nil {
		t.Fatalf("row = %+v err=%v", r, err)
	}
	if open, _ := d.ListOpenInterceptPending(ctx, f.task.ID); !slices.Equal(open, []int64{id}) {
		t.Fatalf("open = %v", open)
	}

	// 人工审批与超时并发争抢同一 CAS：恰好一个胜出。
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins []string
	)
	for k := 0; k < 10; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			status, action := InterceptAllowed, "allow"
			if k%2 == 1 {
				status, action = InterceptTimeout, "deny"
			}
			ok, err := d.ResolveIntercept(ctx, id, status, action, "race", k%2 == 0)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				wins = append(wins, status)
				mu.Unlock()
			}
		}(k)
	}
	wg.Wait()
	if len(wins) != 1 {
		t.Fatalf("winners = %v", wins)
	}
	r, _ = d.GetInterceptPending(ctx, id)
	if r.Status != wins[0] || r.DecidedAt == nil {
		t.Fatalf("row = %+v", r)
	}
	if r.Allowed() != (wins[0] == InterceptAllowed) {
		t.Fatalf("Allowed() = %v for %s/%s", r.Allowed(), r.Status, r.EffectiveAction)
	}
	if still, _ := d.TouchInterceptWaiter(ctx, id, time.Minute); still {
		t.Fatal("touch must report decided rows")
	}
	if err := d.CompleteIntercept(ctx, id, "executed", "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetInterceptPending(ctx, -1); err != ErrNotFound {
		t.Fatalf("missing row err = %v", err)
	}
}

func TestInterceptOrphanReapAndLiveWaiter(t *testing.T) {
	f := newFixture(t)
	d, ctx := f.d, context.Background()
	orphan, _ := d.CreateInterceptPending(ctx, NewInterceptPending{TaskID: f.task.ID, ToolName: "Bash", WaiterTTL: time.Minute})
	live, _ := d.CreateInterceptPending(ctx, NewInterceptPending{TaskID: f.task.ID, ToolName: "Bash", WaiterTTL: time.Minute})
	if _, err := d.Exec(`UPDATE intercept_pending SET waiter_until = now() - interval '1 second' WHERE id = $1`, orphan); err != nil {
		t.Fatal(err)
	}
	// 人工审批要求等待方心跳存活。
	if ok, _ := d.ResolveIntercept(ctx, orphan, InterceptAllowed, "allow", "", true); ok {
		t.Fatal("must not approve a request whose waiter is gone")
	}
	reaped, err := d.ReapOrphanedIntercepts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(reaped, orphan) || slices.Contains(reaped, live) {
		t.Fatalf("reaped = %v (orphan=%d live=%d)", reaped, orphan, live)
	}
	r, _ := d.GetInterceptPending(ctx, orphan)
	if r.Status != InterceptDenied || r.ExecOutcome != "not_executed" || r.Allowed() {
		t.Fatalf("orphan = %+v", r)
	}
	if ok, _ := d.ResolveIntercept(ctx, live, InterceptDenied, "deny", "人工拒绝", true); !ok {
		t.Fatal("live waiter should be decidable")
	}

	if err := d.InsertInterceptAudit(ctx, InterceptAuditRow{TaskID: f.task.ID, ToolName: "Bash", Source: "ask",
		Action: "ask", EffectiveAction: "deny", PendingID: live, Reason: "人工拒绝"}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertInterceptAudit(ctx, InterceptAuditRow{TaskID: f.task.ID, ToolName: "Bash", Source: "bogus",
		Action: "x", EffectiveAction: "x"}); err == nil {
		t.Fatal("audit source CHECK should reject")
	}
	as, _ := d.InterceptAuditForTask(ctx, f.task.ID)
	if len(as) != 1 || as[0].PendingID != live {
		t.Fatalf("audit = %+v", as)
	}
}
