package server

// S2 端到端：Worker 经测试工具提交作业并订阅 -> 原子收尾进入 blocked+waiting_job（释放槽位、不通知 Planner 已结束）
// -> 作业终态事件 -> 唤醒协调器重开 -> 新回合送达事件 -> 校验真实结果后 done。测试作业直接写固定解析队列表，
// 只用于验证框架，不代表真实解析工具交付。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

var testJobSeq atomic.Int64

// insertTestFileJob 在任务内登记原件并入队一个 queued 固定解析作业。
func insertTestFileJob(t *testing.T, pg *db.DB, taskID int64) int64 {
	t.Helper()
	ctx := context.Background()
	sha := fmt.Sprintf("%064x", time.Now().UnixNano()+testJobSeq.Add(1))
	var fileID, jobID int64
	if err := pg.QueryRowContext(ctx, `INSERT INTO task_file_objects(task_id, kind, sha256, byte_size, relative_path, original_name)
		VALUES ($1, 'original', $2, 10, $3, 'tender.pdf') RETURNING id`, taskID, sha, "objects/"+sha).Scan(&fileID); err != nil {
		t.Error(err)
		return 0
	}
	if err := pg.QueryRowContext(ctx, `INSERT INTO fixed_file_jobs(task_id, file_id, source_hash, request_key, fingerprint, input)
		VALUES ($1, $2, $3, $4, 'fp', '{}') RETURNING id`, taskID, fileID, sha, "rk-"+sha[len(sha)-20:]).Scan(&jobID); err != nil {
		t.Error(err)
	}
	return jobID
}

func succeedTestFileJob(t *testing.T, pg *db.DB, jobID int64) {
	t.Helper()
	ctx := context.Background()
	var rec int64
	if err := pg.QueryRowContext(ctx, `INSERT INTO file_processing_records(task_id, file_id, operation, request_key, fingerprint, source_hash, status)
		SELECT task_id, file_id, 'import', 'rec-' || id, 'fp', source_hash, 'adopted' FROM fixed_file_jobs WHERE id = $1 RETURNING id`, jobID).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ExecContext(ctx, `UPDATE fixed_file_jobs SET status = 'succeeded', record_id = $2 WHERE id = $1`, jobID, rec); err != nil {
		t.Fatal(err)
	}
}

func TestEngineWaitsForAgentJobThenFinishes(t *testing.T) {
	pg := testDB(t)
	var (
		envRef      atomic.Pointer[harnessEnv]
		jobID       atomic.Int64
		plannerMu   sync.Mutex
		plannerSeen []string
	)
	submit := tool.New(tool.Spec{Name: "submit_test_job", Description: "测试作业：入队一个固定解析作业并订阅"},
		func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			task := envRef.Load().task
			id := insertTestFileJob(t, pg, task.ID)
			sub, err := pg.SubscribeAgentJob(ctx, db.DefaultTenant, task.ID, db.SubscribeAgentJobInput{
				Job: db.JobRef{Kind: db.JobKindTenderParse, ID: id}, TargetRef: map[string]any{"operation": "import"}, Actor: "worker"})
			if err != nil {
				return tool.Errorf("submit rejected: %v", err), nil
			}
			jobID.Store(id)
			return tool.JSON(map[string]any{"accepted": true, "job_ref": sub.Job, "status": sub.JobStatus, "next_action": "wait_for_job_event"}), nil
		})
	planner := func(env *harnessEnv) brain {
		envRef.Store(env)
		return func(req llm.CompletionRequest) llm.Message {
			plannerMu.Lock()
			plannerSeen = append(plannerSeen, lastUserText(req))
			plannerMu.Unlock()
			if _, ok := lastToolResult(req); ok {
				return say("本轮规划完成")
			}
			if ns, _ := env.task.Store.NodesByKind(context.Background(), db.KindIntent, nil, 1); len(ns) == 0 {
				return use(agent.ToolAddIntent, map[string]any{"summary": "解析新上传的招标原件",
					"mandate": map[string]any{"require_writeback": true, "required_job_kinds": []string{db.JobKindTenderParse}}})
			}
			return say("暂无新方向")
		}
	}
	worker := brain(func(req llm.CompletionRequest) llm.Message {
		prev, isResult := lastToolResult(req)
		switch {
		case strings.Contains(lastUserText(req), "后台作业事件 #"):
			return say("后台解析已成功并采用，结果已核对")
		case isResult && prev == "submit_test_job":
			return say("已提交解析作业，等待后台处理")
		case isResult:
			return say("工具已返回")
		default:
			return use("submit_test_job", map[string]any{})
		}
	})
	env := newEnvWith(t, 0, false, worker, Config{JobWakePoll: 100 * time.Millisecond}, envOpts{extraTools: []tool.CoreTool{submit}, planner: planner})
	ctx := context.Background()

	var in *db.Node
	waitFor(t, 15*time.Second, "intent waiting for its job", func() bool {
		ns, _ := env.task.Store.NodesByKind(ctx, db.KindIntent, nil, 1)
		if len(ns) == 1 && db.IsWaitingIntent(ns[0]) {
			in = ns[0]
		}
		return in != nil
	})
	if !env.acts.has("等待后台作业") {
		t.Fatal("waiting activity was not published")
	}
	if active, err := env.task.Store.HasActiveIntent(ctx); err != nil || !active {
		t.Fatalf("waiting intent not active: %v %v", active, err)
	}
	time.Sleep(300 * time.Millisecond) // 给 Planner 一次机会：等待态不得被渲染为“已结束”
	plannerMu.Lock()
	for _, s := range plannerSeen {
		if strings.Contains(s, fmt.Sprintf("意图 #%d 已结束", in.ID)) {
			t.Fatalf("planner told a waiting intent had ended:\n%s", s)
		}
	}
	plannerMu.Unlock()
	if st := taskStatus(t, env); st != db.TaskRunning {
		t.Fatalf("task status while waiting: %s", st)
	}

	succeedTestFileJob(t, pg, jobID.Load())
	env.e.WakeAgentJobs()
	waitFor(t, 15*time.Second, "intent done after job event", func() bool {
		n, _ := env.task.Store.GetNode(ctx, in.ID)
		return n != nil && n.State == db.StateIntentDone
	})
	subs, err := pg.ListAgentJobs(ctx, db.DefaultTenant, env.task.ID, db.AgentJobFilter{IntentID: in.ID})
	if err != nil || len(subs) != 1 || subs[0].State != "acknowledged" || subs[0].Event == nil || subs[0].Event.State != "acknowledged" {
		t.Fatalf("subscription after completion: %+v %v", subs, err)
	}
	msgs, _ := db.NewTranscriptStore(pg).Load(ctx, agent.WorkerSessionID(env.task.ExpID, in.ID))
	raw, _ := json.Marshal(msgs)
	if ids := db.AgentJobEventIDsIn(string(raw)); len(ids) != 1 || ids[0] != subs[0].Event.ID {
		t.Fatalf("saved transcript markers %v, event %d", ids, subs[0].Event.ID)
	}
	if !env.acts.has("重新排队") {
		t.Fatal("wake activity was not published")
	}
}

func TestWatchdogStopReleasesSlotForDurableWork(t *testing.T) {
	w := newWatchdogHooks(nil, 1, "w", &db.IntentMandate{RequireWriteback: true}, WatchdogConfig{}, nil, nil, nil)
	w.durableWork = func() bool { return true }
	if _, blocking, _ := w.Stop(context.Background(), nil); len(blocking) != 0 {
		t.Fatalf("durable subscription still nudged: %v", blocking)
	}
	w.durableWork = func() bool { return false }
	if _, blocking, _ := w.Stop(context.Background(), nil); len(blocking) == 0 {
		t.Fatal("missing writeback was not nudged")
	}
}

func TestAgentJobStatusToolAndRoute(t *testing.T) {
	c, _ := newIdleAPI(t)
	pg := testDB(t)
	ctx := context.Background()
	newTask := func() (*db.Task, *db.ExplorationStore) {
		task, err := pg.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("jobs-%d", time.Now().UnixNano()), Goal: "作业查询"})
		if err != nil {
			t.Fatal(err)
		}
		return task, db.NewExplorationStore(pg, task.ExplorationID)
	}
	task, es := newTask()
	claim := func() (int64, int64) {
		id, err := es.AddNode(ctx, db.KindIntent, db.StateIntentOpen, map[string]any{"summary": "work"}, 1, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		epoch, _, err := es.ClaimIntentWithLease(ctx, id, "worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return id, epoch
	}
	a, ea := claim()
	b, eb := claim()
	ja, jb := insertTestFileJob(t, pg, task.ID), insertTestFileJob(t, pg, task.ID)
	for _, x := range []struct{ intent, epoch, job int64 }{{a, ea, ja}, {b, eb, jb}} {
		if _, err := pg.SubscribeAgentJob(db.WithIntentExecution(ctx, x.intent, x.epoch), db.DefaultTenant, task.ID,
			db.SubscribeAgentJobInput{Job: db.JobRef{Kind: db.JobKindTenderParse, ID: x.job}}); err != nil {
			t.Fatal(err)
		}
	}
	tl := bidtools.AgentJobStatusTool(pg, task.ID)
	call := func(ctx context.Context, in any) map[string]any {
		raw, _ := json.Marshal(in)
		res, err := tl.Call(ctx, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			return map[string]any{"error": res.Content}
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
			t.Fatalf("decode %s: %v", res.Content, err)
		}
		return out
	}
	host := db.WithIntentExecution(ctx, a, ea)
	if got := call(host, map[string]any{}); len(got["jobs"].([]any)) != 1 {
		t.Fatalf("worker list not bound to its intent: %v", got)
	}
	if got := call(host, map[string]any{"intent_id": b}); got["error"] == nil {
		t.Fatalf("worker read another intent: %v", got)
	}
	job := call(host, map[string]any{"job_ref": map[string]any{"kind": db.JobKindTenderParse, "id": ja}})
	if v := job["job"].(map[string]any); v["status"] != "queued" || v["next_action"] != "wait_for_job_event" {
		t.Fatalf("job status: %v", job)
	}
	other, _ := newTask()
	foreign := insertTestFileJob(t, pg, other.ID)
	if got := call(ctx, map[string]any{"job_ref": map[string]any{"kind": db.JobKindTenderParse, "id": foreign}}); !strings.Contains(fmt.Sprint(got["error"]), "SCOPE_VIOLATION") {
		t.Fatalf("foreign job readable: %v", got)
	}
	if got := call(ctx, map[string]any{"job_ref": map[string]any{"kind": "chart_render", "id": ja}}); !strings.Contains(fmt.Sprint(got["error"]), "INVALID_ARGUMENT") {
		t.Fatalf("unsupported kind: %v", got)
	}
	if got := call(ctx, map[string]any{"intent_id": b}); len(got["jobs"].([]any)) != 1 {
		t.Fatalf("planner list by intent: %v", got)
	}

	var list []db.AgentJobSubscription
	if st := c.do("GET", fmt.Sprintf("/api/tasks/%d/agent-jobs", task.ID), nil, &list); st != 200 || len(list) != 2 {
		t.Fatalf("route: %d %+v", st, list)
	}
	if st := c.do("GET", fmt.Sprintf("/api/tasks/%d/agent-jobs?intent_id=%d", task.ID, a), nil, &list); st != 200 || len(list) != 1 || list[0].Job.ID != ja {
		t.Fatalf("route filter: %d %+v", st, list)
	}
	if st := c.do("GET", fmt.Sprintf("/api/tasks/%d/agent-jobs?kind=chart_render", task.ID), nil, nil); st != 400 {
		t.Fatalf("route kind validation: %d", st)
	}
}
