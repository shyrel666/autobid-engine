package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
)

// quotaDead 永远返回配额耗尽。
type quotaDead struct{ fakeStream }

// 端到端：任务显式链 [A(配额耗尽), B(可用)]，LLMService 作为 Engine 的 Resolver。
// 期望首个调用在未提交输出前从 A 切到 B，任务正常完成，账本按实际 Profile 与调用通道入账。
func TestLLMServiceChainFailoverEndToEnd(t *testing.T) {
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	suffix := fmt.Sprint(time.Now().UnixNano())
	a, err := pg.CreateLLMProfile(ctx, db.LLMProfile{Name: "dead-" + suffix, Model: "claude-opus-5-5", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pg.CreateLLMProfile(ctx, db.LLMProfile{Name: "live-" + suffix, Model: "claude-sonnet-5-5", Enabled: true})

	m := NewManager(pg)
	acts := &activityLog{}
	var task *Task
	live := brain(func(req llm.CompletionRequest) llm.Message {
		if strings.Contains(req.System, "Planner") {
			return plannerBrain(t, &task, true)(req)
		}
		return workerBrain("fact", &workerProbe{})(req)
	})
	svc := NewLLMService(pg, LLMServiceConfig{
		Factory: func(p *db.LLMProfile, _ *http.Client) (llm.Provider, error) {
			if p.ID == a {
				return &quotaDead{fakeStream{scripts: []streamScript{{err: errQuot}}}}, nil
			}
			return live, nil
		},
		OnActivity: func(_ int64, a db.Activity) { acts.add(a) },
	})
	e := NewEngine(m, svc.Resolve, Config{
		Workers: 1, Debounce: 30 * time.Millisecond, WorkerIdlePoll: 30 * time.Millisecond, ModelErrorRetries: -1,
		OnActivity: acts.add,
	})
	svc.Bind(e)

	task, err = m.CreateTask(ctx, db.CreateTaskInput{Name: "llm-" + suffix, Goal: "完成资格审查推演"}, "资格审查无废标风险")
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetTaskLLMChain(ctx, task.ID, []int64{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx, task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.StopTask(task.ID) })

	waitFor(t, 15*time.Second, "task done", func() bool {
		r, _ := pg.GetTask(context.Background(), task.ID)
		return r.Status == db.TaskDone
	})
	st, _ := pg.TaskLLMState(ctx, task.ID)
	if st.ActiveProfileID == nil || *st.ActiveProfileID != b {
		t.Fatalf("active profile = %v, want %d", st.ActiveProfileID, b)
	}
	if !acts.has("切换到下一个配置") {
		t.Fatal("chain transition activity missing")
	}

	rows, err := pg.QueryContext(ctx, `SELECT profile_id, worker, status FROM llm_usage WHERE task_id = $1 ORDER BY id`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var deadErr, livePlanner, liveWorker int
	for rows.Next() {
		var pid int64
		var worker, status string
		if err := rows.Scan(&pid, &worker, &status); err != nil {
			t.Fatal(err)
		}
		switch {
		case pid == a && status == "error":
			deadErr++
		case pid == b && worker == agent.AgentPlanner && status == "ok":
			livePlanner++
		case pid == b && worker == agent.AgentWorker && status == "ok":
			liveWorker++
		}
	}
	if deadErr != 1 || livePlanner == 0 || liveWorker == 0 {
		t.Fatalf("ledger: dead errors=%d planner ok=%d worker ok=%d", deadErr, livePlanner, liveWorker)
	}
}
