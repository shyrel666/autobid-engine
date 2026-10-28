package intercept

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/intercept/intercepttest"
)

var tools = []string{"Bash", "fetch"}

func TestRuleMatchingAndPriority(t *testing.T) {
	st := intercepttest.New(
		db.InterceptRule{ID: 1, Name: "deny-rm", MatchTarget: "tool_input", MatchType: "regex", Pattern: `rm\s+-rf`, Action: ActionDeny, Priority: 10},
		db.InterceptRule{ID: 2, Name: "allow-safe", MatchTarget: "tool_input", MatchType: "string", Pattern: "SAFE", Action: ActionAllow, Priority: 20},
		db.InterceptRule{ID: 3, Name: "broken", MatchTarget: "tool_name", MatchType: "regex", Pattern: `(`, Action: ActionDeny, Priority: 99},
		db.InterceptRule{ID: 4, Name: "ask-fetch", ToolFilter: "fetch", MatchType: "string", Pattern: "", Action: ActionAsk, Priority: 0},
		db.InterceptRule{ID: 5, Name: "deny-bash-name", MatchTarget: "tool_name", MatchType: "string", Pattern: "Bas", Action: ActionDeny, Priority: 0},
		db.InterceptRule{ID: 6, Name: "same-prio-later", MatchTarget: "tool_name", MatchType: "string", Pattern: "Bash", Action: ActionAllow, Priority: 0},
	)
	// 模拟 DB 排序：priority DESC, id ASC。
	st.Rules = []db.InterceptRule{st.Rules[2], st.Rules[1], st.Rules[0], st.Rules[3], st.Rules[4], st.Rules[5]}
	ic := New(st, Options{EnabledTools: tools})
	ctx := context.Background()

	cases := []struct {
		tool, input, action string
		rule                int64
		enabled             bool
	}{
		{"Bash", `{"cmd":"rm -rf /"}`, ActionDeny, 1, true},
		{"Bash", `{"cmd":"rm -rf /tmp SAFE"}`, ActionAllow, 2, true}, // 高优先级 allow 截断 deny
		{"Bash", `{"cmd":"ls"}`, ActionDeny, 5, true},                // string 为包含匹配；同优先级 id 小者先
		{"fetch", `{"url":"x"}`, ActionAsk, 4, true},                 // tool_filter 作用域
		{"other", `{"cmd":"rm -rf /"}`, "", 0, false},                // 未启用拦截的工具不进入规则链
	}
	for _, c := range cases {
		dec, enabled := ic.Evaluate(ctx, c.tool, []byte(c.input))
		if enabled != c.enabled || dec.Action != c.action || dec.RuleID != c.rule {
			t.Errorf("%s %s: got enabled=%v action=%q rule=%d, want %v %q %d", c.tool, c.input, enabled, dec.Action, dec.RuleID, c.enabled, c.action, c.rule)
		}
	}

	// 无规则命中、无 Judge：默认放行。
	st2 := intercepttest.New()
	dec, enabled := New(st2, Options{EnabledTools: tools}).Evaluate(ctx, "Bash", []byte(`{}`))
	if !enabled || dec.Action != ActionAllow || dec.Source != SourceDefault {
		t.Fatalf("default = %+v enabled=%v", dec, enabled)
	}

	// Invalidate 后重新加载规则。
	st.Mu.Lock()
	st.Rules = nil
	st.Mu.Unlock()
	if dec, _ := ic.Evaluate(ctx, "Bash", []byte(`{"cmd":"ls"}`)); dec.Action != ActionDeny {
		t.Fatal("cache should still hold old rules before Invalidate")
	}
	ic.Invalidate()
	if dec, _ := ic.Evaluate(ctx, "Bash", []byte(`{"cmd":"ls"}`)); dec.Action != ActionAllow {
		t.Fatalf("after Invalidate: %+v", dec)
	}
}

func TestDefaultEnabledToolsAndConfigOverride(t *testing.T) {
	st := intercepttest.New(db.InterceptRule{Pattern: "", Action: ActionDeny})
	ctx := context.Background()
	if _, enabled := New(st, Options{}).Evaluate(ctx, "Bash", nil); !enabled {
		t.Fatal("Bash should be enabled by default")
	}
	if _, enabled := New(st, Options{}).Evaluate(ctx, "record_fact", nil); enabled {
		t.Fatal("graph tools are not enabled by default")
	}
	st.Cfg.EnabledTools = []string{"record_fact"}
	ic := New(st, Options{})
	if _, enabled := ic.Evaluate(ctx, "record_fact", nil); !enabled {
		t.Fatal("config override not applied")
	}
	if _, enabled := ic.Evaluate(ctx, "Bash", nil); enabled {
		t.Fatal("config override should replace the default set")
	}
}

func askRule(timeoutSecs int, timeoutAction string) *intercepttest.Store {
	return intercepttest.New(db.InterceptRule{Name: "ask", Pattern: "Bash", Action: ActionAsk, Message: "需要审批",
		TimeoutEnabled: timeoutSecs > 0, TimeoutSeconds: timeoutSecs, TimeoutAction: timeoutAction})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func checkAsync(ic *Interceptor, ctx context.Context) <-chan Verdict {
	out := make(chan Verdict, 1)
	go func() { out <- ic.Check(ctx, "Bash", []byte(`{"cmd":"deploy"}`)) }()
	return out
}

func recv(t *testing.T, ch <-chan Verdict) Verdict {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("HandleAsk did not return")
		return Verdict{}
	}
}

// Decide 在 INSERT 之后、Channel 注册之前到达：二次读库必须闭合该窗口。
func TestDecideBeforeWait(t *testing.T) {
	for _, allow := range []bool{true, false} {
		st := askRule(0, "")
		ic := New(st, Options{EnabledTools: tools})
		st.AfterCreate = func(id int64) {
			if err := ic.Decide(context.Background(), id, allow, ""); err != nil {
				t.Errorf("Decide: %v", err)
			}
		}
		v := recv(t, checkAsync(ic, context.Background()))
		if v.Allowed != allow || v.PendingID == 0 {
			t.Fatalf("allow=%v: verdict %+v", allow, v)
		}
		if ic.Waiting(v.PendingID) {
			t.Fatal("waiter not removed")
		}
	}
}

func TestWaitBeforeDecideAndDoubleDecide(t *testing.T) {
	st := askRule(0, "")
	ic := New(st, Options{EnabledTools: tools})
	ch := checkAsync(ic, context.Background())
	waitUntil(t, "waiter registered", func() bool { return ic.Waiting(1) })

	if err := ic.Decide(context.Background(), 1, true, "同意部署"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	v := recv(t, ch)
	if !v.Allowed || v.PendingID != 1 {
		t.Fatalf("verdict %+v", v)
	}
	if err := ic.Decide(context.Background(), 1, false, ""); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second Decide = %v, want ErrAlreadyDecided", err)
	}
	if r := st.Row(1); r.Status != db.InterceptAllowed || r.Reason != "同意部署" {
		t.Fatalf("row = %+v", r)
	}
	if err := ic.Decide(context.Background(), 99, true, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v", err)
	}
}

// 并发 Decide：恰好一个胜出。
func TestConcurrentDecideSingleWinner(t *testing.T) {
	st := askRule(0, "")
	ic := New(st, Options{EnabledTools: tools})
	ch := checkAsync(ic, context.Background())
	waitUntil(t, "waiter registered", func() bool { return ic.Waiting(1) })
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins []bool
	)
	for k := 0; k < 8; k++ {
		wg.Add(1)
		go func(allow bool) {
			defer wg.Done()
			if err := ic.Decide(context.Background(), 1, allow, ""); err == nil {
				mu.Lock()
				wins = append(wins, allow)
				mu.Unlock()
			} else if !errors.Is(err, ErrAlreadyDecided) {
				t.Errorf("Decide: %v", err)
			}
		}(k%2 == 0)
	}
	wg.Wait()
	if len(wins) != 1 {
		t.Fatalf("winners = %v", wins)
	}
	if v := recv(t, ch); v.Allowed != wins[0] {
		t.Fatalf("waiter got %v, winner decided %v", v.Allowed, wins[0])
	}
}

func TestCtxCancelRecordsDenial(t *testing.T) {
	st := askRule(0, "")
	ic := New(st, Options{EnabledTools: tools})
	ctx, cancel := context.WithCancel(context.Background())
	ch := checkAsync(ic, ctx)
	waitUntil(t, "waiter registered", func() bool { return ic.Waiting(1) })
	cancel()
	v := recv(t, ch)
	if v.Allowed {
		t.Fatal("cancelled wait must deny")
	}
	r := st.Row(1)
	if r.Status != db.InterceptDenied || r.EffectiveAction != ActionDeny || r.ExecOutcome != "not_executed" {
		t.Fatalf("row = %+v", r)
	}
	if err := ic.Decide(context.Background(), 1, true, ""); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("Decide after cancel = %v", err)
	}
}

func TestTimeoutAction(t *testing.T) {
	for _, action := range []string{ActionAllow, ActionDeny, "bogus"} {
		st := askRule(1, action)
		ic := New(st, Options{EnabledTools: tools})
		v := recv(t, checkAsync(ic, context.Background()))
		want := action == ActionAllow
		if v.Allowed != want {
			t.Fatalf("timeout_action=%q: allowed=%v", action, v.Allowed)
		}
		r := st.Row(v.PendingID)
		if r.Status != db.InterceptTimeout || r.Allowed() != want {
			t.Fatalf("timeout_action=%q: row %+v", action, r)
		}
		if err := ic.Decide(context.Background(), v.PendingID, true, ""); !errors.Is(err, ErrAlreadyDecided) {
			t.Fatalf("Decide after timeout = %v", err)
		}
	}
}

// 另一实例上的 Decide 无法唤醒本进程 Channel：等待方心跳发现记录已决后重新读库。
func TestCrossInstanceDecideWakesViaHeartbeat(t *testing.T) {
	st := askRule(0, "")
	waiter := New(st, Options{EnabledTools: tools, WaiterTTL: 60 * time.Millisecond})
	other := New(st, Options{EnabledTools: tools})
	ch := checkAsync(waiter, context.Background())
	waitUntil(t, "waiter registered", func() bool { return waiter.Waiting(1) })
	if err := other.Decide(context.Background(), 1, true, ""); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if v := recv(t, ch); !v.Allowed {
		t.Fatalf("verdict %+v", v)
	}
}

// 等待方已失联（心跳过期）：批准被拒绝并落为 denied；回收器同样落 denied。
func TestDecideWaiterGoneAndReap(t *testing.T) {
	st := askRule(0, "")
	ic := New(st, Options{EnabledTools: tools})
	ctx := context.Background()
	id, _ := st.CreateInterceptPending(ctx, db.NewInterceptPending{ToolName: "Bash", WaiterTTL: time.Minute})
	st.ExpireWaiter(id)
	if err := ic.Decide(ctx, id, true, ""); !errors.Is(err, ErrWaiterGone) {
		t.Fatalf("Decide = %v, want ErrWaiterGone", err)
	}
	if r := st.Row(id); r.Status != db.InterceptDenied {
		t.Fatalf("row = %+v", r)
	}
	id2, _ := st.CreateInterceptPending(ctx, db.NewInterceptPending{ToolName: "Bash", WaiterTTL: time.Minute})
	st.ExpireWaiter(id2)
	if got, _ := ic.ReapOrphans(ctx); len(got) != 1 || got[0] != id2 {
		t.Fatalf("reaped = %v", got)
	}
}

func TestJudge(t *testing.T) {
	ctx := WithAgent(WithTask(context.Background(), 7, 9), "work#1")
	st := intercepttest.New()
	st.Cfg.Judge = json.RawMessage(`{"enabled":true,"ask_timeout_seconds":1}`)
	var got ReviewInput
	reviewer := func(_ context.Context, _ int64, _ string, in ReviewInput) (Decision, error) {
		got = in
		return Decision{Action: "DENY", Message: "外部副作用"}, nil
	}
	dec, _ := New(st, Options{EnabledTools: tools, Reviewer: reviewer}).Evaluate(ctx, "Bash", []byte(`{"cmd":"x"}`))
	if dec.Action != ActionDeny || dec.Source != SourceJudge || dec.RuleID != 0 || dec.Message != "外部副作用" {
		t.Fatalf("judge deny = %+v", dec)
	}
	if got.TaskID != 7 || got.IntentID != 9 || got.Agent != "work#1" || got.ToolName != "Bash" {
		t.Fatalf("review input = %+v", got)
	}

	failing := func(context.Context, int64, string, ReviewInput) (Decision, error) {
		return Decision{}, errors.New("boom")
	}
	dec, _ = New(st, Options{EnabledTools: tools, Reviewer: failing}).Evaluate(ctx, "Bash", []byte(`{}`))
	if dec.Action != ActionAllow || !dec.ModelFallback {
		t.Fatalf("judge failure should fall back to allow: %+v", dec)
	}

	dec, _ = New(st, Options{EnabledTools: tools, Reviewer: reviewer}).Evaluate(ctx, "Bash", []byte(`{"cmd":`))
	if dec.Action != ActionAsk || !dec.TimeoutEnabled || dec.TimeoutSeconds != 1 || dec.TimeoutAction != ActionDeny {
		t.Fatalf("incomplete input should ask with judge ask-timeout: %+v", dec)
	}
}

func TestAuditAndActivity(t *testing.T) {
	st := askRule(0, "")
	var (
		mu   sync.Mutex
		acts []db.Activity
	)
	ic := New(st, Options{EnabledTools: tools})
	ctx := WithEmit(WithAgent(WithTask(context.Background(), 3, 4), "work#2"), func(a db.Activity) {
		mu.Lock()
		acts = append(acts, a)
		mu.Unlock()
	})
	ch := checkAsync(ic, ctx)
	waitUntil(t, "waiter registered", func() bool { return ic.Waiting(1) })
	_ = ic.Decide(context.Background(), 1, false, "不允许")
	if v := recv(t, ch); v.Allowed || v.Message == "" {
		t.Fatalf("verdict %+v", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(acts) != 2 || acts[0].Kind != "intercept_request" || acts[0].TaskID != 3 || acts[0].Worker != "work#2" || *acts[0].NodeID != 4 {
		t.Fatalf("activities = %+v", acts)
	}
	if len(st.Audit) != 1 || st.Audit[0].Source != SourceAsk || st.Audit[0].EffectiveAction != ActionDeny || st.Audit[0].PendingID != 1 {
		t.Fatalf("audit = %+v", st.Audit)
	}
}
