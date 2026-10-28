package server

// 控制面端到端测试：Worker 的工具调用经 guard -> intercept 挂起等待人工审批（§6.3）。
// 需要 AUTOBID_TEST_DATABASE_URL，未设置时跳过。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/guard"
	"github.com/autobid/autobid-engine/intercept"
	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

const guardedTool = "submit_bid"

// guardedBrain：先调用受控工具（入参携带唯一 tag 供规则匹配）；拿到结果后把结果写成事实并结束。
func guardedBrain(tag string) brain {
	return func(req llm.CompletionRequest) llm.Message {
		prev, isResult := lastToolResult(req)
		switch {
		case isResult && prev == guardedTool:
			var res string
			for _, b := range req.Messages[len(req.Messages)-1].Content {
				if b.Type == llm.BlockToolResult {
					res = fmt.Sprintf("is_error=%v %s", b.IsError, b.Text)
				}
			}
			return use(agent.ToolRecordFact, map[string]any{"summary": "提交结果：" + res})
		case isResult:
			return say("核验完成")
		default:
			return use(guardedTool, map[string]any{"bid": tag})
		}
	}
}

type guardedEnv struct {
	*harnessEnv
	ic       *intercept.Interceptor
	executed *atomic.Int64
}

func newGuardedEnv(t *testing.T, rule db.InterceptRule, mods ...func(*envOpts)) *guardedEnv {
	t.Helper()
	pg := testDB(t)
	ctx := context.Background()
	tag := fmt.Sprintf("bid-%d", time.Now().UnixNano())
	rule.Name, rule.ToolFilter, rule.MatchTarget, rule.MatchType, rule.Pattern, rule.Enabled =
		tag, guardedTool, "tool_input", "string", tag, true
	ruleID, err := pg.CreateInterceptRule(ctx, rule)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteInterceptRule(context.Background(), ruleID) })

	ic := intercept.New(pg, intercept.Options{EnabledTools: []string{guardedTool}, WaiterTTL: 2 * time.Second, Waiter: "test"})
	g := guard.New(ic)
	executed := &atomic.Int64{}
	submit := tool.New(tool.Spec{Name: guardedTool, Description: "submits the bid (external side effect)"},
		func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			executed.Add(1)
			return tool.Result{Content: "bid submitted"}, nil
		})
	opts := envOpts{
		hooks:      func(*Task) harness.HookRunner { return g },
		extraTools: []tool.CoreTool{submit},
	}
	for _, mod := range mods {
		mod(&opts)
	}
	env := newEnvWith(t, 0, true, guardedBrain(tag), Config{}, opts)
	return &guardedEnv{harnessEnv: env, ic: ic, executed: executed}
}

func (e *guardedEnv) awaitPending(t *testing.T) int64 {
	t.Helper()
	var id int64
	waitFor(t, 15*time.Second, "pending approval", func() bool {
		ids, _ := testDB(t).ListOpenInterceptPending(context.Background(), e.task.ID)
		if len(ids) == 1 && e.ic.Waiting(ids[0]) {
			id = ids[0]
		}
		return id != 0
	})
	return id
}

func (e *guardedEnv) factSummary(t *testing.T) string {
	t.Helper()
	in := onlyIntent(t, e.harnessEnv)
	ys, _ := e.task.Store.YieldedNodes(context.Background(), in.ID, 10)
	if len(ys) != 1 {
		t.Fatalf("yields = %+v", ys)
	}
	return summaryText(ys[0])
}

// 审批通过：工具调用挂起 -> Decide(allow) -> 工具执行 -> 事实写回 -> Intent done -> 任务 done。
func TestEngineToolApprovalAllowed(t *testing.T) {
	env := newGuardedEnv(t, db.InterceptRule{Action: intercept.ActionAsk, Message: "提交投标需人工审批"})
	id := env.awaitPending(t)
	if env.executed.Load() != 0 {
		t.Fatal("tool executed before approval")
	}
	in := onlyIntent(t, env.harnessEnv)
	if in.State != db.StateIntentRunning {
		t.Fatalf("intent should stay running while suspended: %s", in.State)
	}
	if !env.acts.has("请求审批") {
		t.Fatal("intercept_request activity not emitted")
	}
	ctx := context.Background()
	if err := env.ic.Decide(ctx, id, true, "同意提交"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := env.ic.Decide(ctx, id, false, ""); !errors.Is(err, intercept.ErrAlreadyDecided) {
		t.Fatalf("second Decide = %v", err)
	}
	waitFor(t, 15*time.Second, "task done", func() bool { return taskStatus(t, env.harnessEnv) == db.TaskDone })

	if n := env.executed.Load(); n != 1 {
		t.Fatalf("tool executed %d times", n)
	}
	if in := onlyIntent(t, env.harnessEnv); in.State != db.StateIntentDone {
		t.Fatalf("intent = %s", in.State)
	}
	if s := env.factSummary(t); !strings.Contains(s, "bid submitted") {
		t.Fatalf("fact = %q", s)
	}
	waitFor(t, 5*time.Second, "execution backfill", func() bool {
		r, _ := testDB(t).GetInterceptPending(ctx, id)
		return r.ExecOutcome == "executed"
	})
	r, _ := testDB(t).GetInterceptPending(ctx, id)
	if r.Status != db.InterceptAllowed || r.Reason != "同意提交" || r.TaskID != env.task.ID || r.IntentID != in.ID || r.AgentName != "work#1" {
		t.Fatalf("pending row = %+v", r)
	}
	audit, _ := testDB(t).InterceptAuditForTask(ctx, env.task.ID)
	if len(audit) != 1 || audit[0].Source != intercept.SourceAsk || audit[0].EffectiveAction != intercept.ActionAllow {
		t.Fatalf("audit = %+v", audit)
	}
}

// 人工拒绝：拒绝变成当前工具的错误结果，模型据此调整；工具不执行，Intent 仍正常完成。
func TestEngineToolApprovalDenied(t *testing.T) {
	env := newGuardedEnv(t, db.InterceptRule{Action: intercept.ActionAsk})
	id := env.awaitPending(t)
	if err := env.ic.Decide(context.Background(), id, false, "报价未复核"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	waitFor(t, 15*time.Second, "task done", func() bool { return taskStatus(t, env.harnessEnv) == db.TaskDone })
	if n := env.executed.Load(); n != 0 {
		t.Fatalf("denied tool executed %d times", n)
	}
	if s := env.factSummary(t); !strings.Contains(s, "is_error=true") || !strings.Contains(s, "报价未复核") {
		t.Fatalf("fact = %q", s)
	}
	r, _ := testDB(t).GetInterceptPending(context.Background(), id)
	if r.Status != db.InterceptDenied || r.ExecOutcome != "not_executed" {
		t.Fatalf("pending row = %+v", r)
	}
}

// 审批超时：按 timeout_action=deny 落 timeout，工具不执行。
func TestEngineToolApprovalTimeout(t *testing.T) {
	env := newGuardedEnv(t, db.InterceptRule{Action: intercept.ActionAsk, TimeoutEnabled: true, TimeoutSeconds: 1, TimeoutAction: "deny"})
	waitFor(t, 20*time.Second, "task done", func() bool { return taskStatus(t, env.harnessEnv) == db.TaskDone })
	if n := env.executed.Load(); n != 0 {
		t.Fatalf("timed-out tool executed %d times", n)
	}
	ids, _ := testDB(t).ListOpenInterceptPending(context.Background(), env.task.ID)
	if len(ids) != 0 {
		t.Fatalf("still pending: %v", ids)
	}
	var status, action string
	if err := testDB(t).QueryRow(`SELECT status, effective_action FROM intercept_pending WHERE task_id = $1`, env.task.ID).Scan(&status, &action); err != nil {
		t.Fatal(err)
	}
	if status != db.InterceptTimeout || action != intercept.ActionDeny {
		t.Fatalf("status=%s action=%s", status, action)
	}
}

// 规则 deny：不挂起，直接以错误结果反馈模型。
func TestEngineToolRuleDeny(t *testing.T) {
	env := newGuardedEnv(t, db.InterceptRule{Action: intercept.ActionDeny, Message: "禁止直接提交"})
	waitFor(t, 15*time.Second, "task done", func() bool { return taskStatus(t, env.harnessEnv) == db.TaskDone })
	if env.executed.Load() != 0 {
		t.Fatal("denied tool executed")
	}
	if s := env.factSummary(t); !strings.Contains(s, "禁止直接提交") {
		t.Fatalf("fact = %q", s)
	}
}

// 等待中工作被取消（单 work cancel）：审批等待终止，DB 记录拒绝，工具不执行。
func TestEngineToolApprovalCancelledWork(t *testing.T) {
	env := newGuardedEnv(t, db.InterceptRule{Action: intercept.ActionAsk})
	id := env.awaitPending(t)
	in := onlyIntent(t, env.harnessEnv)
	if err := env.e.ControlWork(context.Background(), in.ID, "cancel"); err != nil {
		t.Fatalf("ControlWork: %v", err)
	}
	r, _ := testDB(t).GetInterceptPending(context.Background(), id)
	if r.Status != db.InterceptDenied || r.ExecOutcome != "not_executed" {
		t.Fatalf("pending row = %+v", r)
	}
	if err := env.ic.Decide(context.Background(), id, true, ""); !errors.Is(err, intercept.ErrAlreadyDecided) {
		t.Fatalf("Decide after cancel = %v", err)
	}
	if env.executed.Load() != 0 {
		t.Fatal("tool executed after cancellation")
	}
}

func TestParseJudgeVerdict(t *testing.T) {
	d, err := parseJudgeVerdict("判断如下：\n```json\n{\"action\":\"ASK\",\"reason\":\"外部提交\"}\n```")
	if err != nil || d.Action != intercept.ActionAsk || d.Message != "外部提交" {
		t.Fatalf("verdict = %+v err=%v", d, err)
	}
	if _, err := parseJudgeVerdict("allow"); err == nil {
		t.Fatal("non-JSON output must error (FailAction applies)")
	}
}
