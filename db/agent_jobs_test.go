package db

// S2 集成测试：Agent 后台作业订阅、终态事件、原子回合收尾与唤醒。需要 AUTOBID_TEST_DATABASE_URL。
// 测试作业直接写入 fixed_file_jobs（固定解析队列的表）以驱动框架；不代表真实解析工具交付。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// testFileJob 在任务内登记一个原件并入队一个固定解析作业（queued），返回作业 ID 与原件 ID。
func (f *fixture) testFileJob(t *testing.T) (jobID, fileID int64) {
	t.Helper()
	ctx := context.Background()
	sha := fmt.Sprintf("%064x", time.Now().UnixNano()+seq.Add(1))
	if err := f.d.QueryRowContext(ctx, `INSERT INTO task_file_objects(task_id, kind, sha256, byte_size, relative_path, original_name)
		VALUES ($1, 'original', $2, 10, $3, 'tender.pdf') RETURNING id`, f.task.ID, sha, "objects/"+sha).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if err := f.d.QueryRowContext(ctx, `INSERT INTO fixed_file_jobs(task_id, file_id, source_hash, request_key, fingerprint, input)
		VALUES ($1, $2, $3, $4, 'fp', '{}') RETURNING id`, f.task.ID, fileID, sha, uniq("rk")).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	return jobID, fileID
}

// finishFileJob 把作业置为终态；succeeded 时附已采用的导入记录（模拟固定解析 Runner 的成功终态事务）。
func (f *fixture) finishFileJob(t *testing.T, jobID, fileID int64, status string) {
	t.Helper()
	ctx := context.Background()
	if status != "succeeded" {
		if _, err := f.d.ExecContext(ctx, `UPDATE fixed_file_jobs SET status = $2, error = 'test failure' WHERE id = $1`, jobID, status); err != nil {
			t.Fatal(err)
		}
		return
	}
	var rec int64
	if err := f.d.QueryRowContext(ctx, `INSERT INTO file_processing_records(task_id, file_id, operation, request_key, fingerprint, source_hash, status)
		SELECT task_id, file_id, 'import', $2, 'fp', source_hash, 'adopted' FROM fixed_file_jobs WHERE id = $1 RETURNING id`, jobID, uniq("rec")).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE fixed_file_jobs SET status = 'succeeded', record_id = $2 WHERE id = $1`, jobID, rec); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) runningIntent(t *testing.T, m *IntentMandate) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	payload := map[string]any{"summary": "提交后台作业并处理结果"}
	if m != nil {
		payload["mandate"] = m
	}
	id, err := f.es.AddNode(ctx, KindIntent, StateIntentOpen, payload, 1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	epoch, ok, err := f.es.ClaimIntentWithLease(ctx, id, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	return id, epoch
}

func (f *fixture) subscribe(t *testing.T, iid, epoch, jobID int64) *AgentJobSubscription {
	t.Helper()
	ctx := WithIntentExecution(context.Background(), iid, epoch)
	s, err := f.d.SubscribeAgentJob(ctx, DefaultTenant, f.task.ID, SubscribeAgentJobInput{
		Job: JobRef{Kind: JobKindTenderParse, ID: jobID}, TargetRef: map[string]any{"file_id": 1}, Actor: "worker"})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return s
}

func (f *fixture) jobSubs(t *testing.T, iid int64) []*AgentJobSubscription {
	t.Helper()
	subs, err := f.d.ListAgentJobs(context.Background(), DefaultTenant, f.task.ID, AgentJobFilter{IntentID: iid})
	if err != nil {
		t.Fatal(err)
	}
	return subs
}

func (f *fixture) eventCount(t *testing.T, subID int64) int {
	t.Helper()
	var n int
	if err := f.d.QueryRowContext(context.Background(), `SELECT count(*) FROM agent_job_events WHERE subscription_id = $1`, subID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAgentJobSubscribeBeforeAndAfterTerminal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	iid, epoch := f.runningIntent(t, nil)
	job1, file1 := f.testFileJob(t)

	s := f.subscribe(t, iid, epoch, job1)
	if s.State != "waiting" || s.Event != nil || s.JobStatus != "queued" || s.CreatedIntentEpoch != epoch {
		t.Fatalf("waiting subscription: %+v", s)
	}
	if again := f.subscribe(t, iid, epoch, job1); again.ID != s.ID {
		t.Fatalf("duplicate subscription: %d vs %d", again.ID, s.ID)
	}
	// 回到 queued 不是终态。
	if _, err := f.d.ExecContext(ctx, `UPDATE fixed_file_jobs SET status = 'running', lease_until = now() + interval '1 minute' WHERE id = $1`, job1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE fixed_file_jobs SET status = 'queued', lease_until = NULL WHERE id = $1`, job1); err != nil {
		t.Fatal(err)
	}
	if n := f.eventCount(t, s.ID); n != 0 {
		t.Fatalf("non-terminal transition published %d events", n)
	}
	f.finishFileJob(t, job1, file1, "failed")
	subs := f.jobSubs(t, iid)
	if len(subs) != 1 || subs[0].State != "notified" || subs[0].Event == nil || subs[0].Event.TerminalStatus != "failed" ||
		subs[0].Event.BusinessState != "failed" || subs[0].Event.State != "pending" {
		t.Fatalf("terminal publication: %+v", subs[0])
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE agent_job_events SET payload = '{}' WHERE id = $1`, subs[0].Event.ID); err == nil {
		t.Fatal("terminal payload was rewritten")
	}

	// 先完成后订阅：直接得到 notified + 事件，不丢通知。
	job2, file2 := f.testFileJob(t)
	f.finishFileJob(t, job2, file2, "succeeded")
	late := f.subscribe(t, iid, epoch, job2)
	if late.State != "notified" || late.Event == nil || late.Event.TerminalStatus != "succeeded" || late.Event.BusinessState != "adopted" {
		t.Fatalf("subscribe after terminal: %+v %+v", late, late.Event)
	}

	// 宿主身份、任务归属、租约与范围都在事务内校验。
	job3, _ := f.testFileJob(t)
	in := SubscribeAgentJobInput{Job: JobRef{Kind: JobKindTenderParse, ID: job3}, Actor: "worker"}
	if _, err := f.d.SubscribeAgentJob(ctx, DefaultTenant, f.task.ID, in); !errors.Is(err, ErrNoIntentExecution) {
		t.Fatalf("subscription without host intent: %v", err)
	}
	if _, err := f.d.SubscribeAgentJob(WithIntentExecution(ctx, iid, epoch+1), DefaultTenant, f.task.ID, in); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale epoch subscribed: %v", err)
	}
	other := newFixture(t)
	foreign, _ := other.testFileJob(t)
	if _, err := f.d.SubscribeAgentJob(WithIntentExecution(ctx, iid, epoch), DefaultTenant, f.task.ID,
		SubscribeAgentJobInput{Job: JobRef{Kind: JobKindTenderParse, ID: foreign}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign task job subscribed: %v", err)
	}
	if _, err := f.d.SubscribeAgentJob(WithIntentExecution(ctx, iid, epoch), DefaultTenant, f.task.ID,
		SubscribeAgentJobInput{Job: JobRef{Kind: JobKindProposalExport, ID: job3}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kind confusion accepted: %v", err)
	}
	if _, err := f.d.SubscribeAgentJob(WithIntentExecution(ctx, iid, epoch), DefaultTenant, f.task.ID,
		SubscribeAgentJobInput{Job: JobRef{Kind: "chart_render", ID: job3}}); !errors.Is(err, ErrJobKindUnsupported) {
		t.Fatalf("unsupported kind accepted: %v", err)
	}
	scoped := WithExecutionScope(WithIntentExecution(ctx, iid, epoch), &IntentMandate{AllowedJobKinds: []string{JobKindProposalExport}})
	if _, err := f.d.SubscribeAgentJob(scoped, DefaultTenant, f.task.ID, in); err == nil || !strings.Contains(err.Error(), "execution scope") {
		t.Fatalf("job kind outside host scope accepted: %v", err)
	}
	if got, err := f.d.ListAgentJobs(scoped, DefaultTenant, f.task.ID, AgentJobFilter{}); err != nil || len(got) != 0 {
		t.Fatalf("scoped read returned kinds outside scope: %d %v", len(got), err)
	}
	if _, err := f.d.GetAgentJobSnapshot(WithExecutionScope(WithIntentExecution(ctx, iid, epoch), &IntentMandate{RestrictObjects: true, AllowedJobKinds: []string{JobKindTenderParse}}),
		DefaultTenant, f.task.ID, JobRef{Kind: JobKindTenderParse, ID: job3}); err == nil {
		t.Fatal("restricted scope read an unsubscribed job")
	}
}

func TestAgentJobListIsBoundToHostIntent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, ea := f.runningIntent(t, nil)
	b, eb := f.runningIntent(t, nil)
	ja, _ := f.testFileJob(t)
	jb, _ := f.testFileJob(t)
	f.subscribe(t, a, ea, ja)
	f.subscribe(t, b, eb, jb)
	own, err := f.d.ListAgentJobs(WithIntentExecution(ctx, a, ea), DefaultTenant, f.task.ID, AgentJobFilter{})
	if err != nil || len(own) != 1 || own[0].IntentID != a {
		t.Fatalf("host-bound list: %+v %v", own, err)
	}
	if _, err := f.d.ListAgentJobs(WithIntentExecution(ctx, a, ea), DefaultTenant, f.task.ID, AgentJobFilter{IntentID: b}); err == nil {
		t.Fatal("worker read another intent's subscriptions")
	}
	if all, err := f.d.ListAgentJobs(ctx, DefaultTenant, f.task.ID, AgentJobFilter{}); err != nil || len(all) != 2 {
		t.Fatalf("planner list: %d %v", len(all), err)
	}
}

// 作业行锁串行化订阅与终态发布：两种先后顺序都恰好产生一个事件。
func TestAgentJobTerminalRacesSubscription(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	iid, epoch := f.runningIntent(t, nil)

	// 终态事务先持锁：订阅等待，提交后看到终态并自建事件。
	job1, _ := f.testFileJob(t)
	tx, err := f.d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fixed_file_jobs SET status = 'failed', error = 'boom' WHERE id = $1`, job1); err != nil {
		t.Fatal(err)
	}
	done := make(chan *AgentJobSubscription, 1)
	go func() { done <- f.subscribe(t, iid, epoch, job1) }()
	select {
	case <-done:
		t.Fatal("subscription did not wait for the terminal transaction")
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := <-done
	if s.State != "notified" || s.Event == nil || f.eventCount(t, s.ID) != 1 {
		t.Fatalf("terminal-first: %+v", s)
	}

	// 订阅事务先持锁：终态更新等待，提交后触发器看到 waiting 订阅并发布。
	job2, _ := f.testFileJob(t)
	release, subscribed := make(chan struct{}), make(chan int64, 1)
	subErr := make(chan error, 1)
	go func() {
		subErr <- f.d.Transaction(WithIntentExecution(ctx, iid, epoch), func(tx *DB) error {
			s, err := tx.SubscribeAgentJob(WithIntentExecution(ctx, iid, epoch), DefaultTenant, f.task.ID, SubscribeAgentJobInput{Job: JobRef{Kind: JobKindTenderParse, ID: job2}})
			if err != nil {
				return err
			}
			subscribed <- s.ID
			<-release
			return nil
		})
	}()
	subID := <-subscribed
	finished := make(chan error, 1)
	go func() {
		_, err := f.d.ExecContext(ctx, `UPDATE fixed_file_jobs SET status = 'cancelled' WHERE id = $1`, job2)
		finished <- err
	}()
	select {
	case <-finished:
		t.Fatal("terminal update did not wait for the subscription transaction")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-subErr; err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if n := f.eventCount(t, subID); n != 1 {
		t.Fatalf("subscribe-first published %d events", n)
	}
}

func TestFinalizeIntentTurnWaitWakeAndAcknowledge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	iid, epoch := f.runningIntent(t, &IntentMandate{RequiredJobKinds: []string{JobKindTenderParse}})
	job, file := f.testFileJob(t)
	f.subscribe(t, iid, epoch, job)

	outcome, _, err := f.es.FinalizeIntentTurn(ctx, iid, epoch, nil)
	if err != nil || outcome != TurnWaiting {
		t.Fatalf("finalize with waiting job: %s %v", outcome, err)
	}
	n, _ := f.es.GetNode(ctx, iid)
	if !IsWaitingIntent(n) || n.CompletedAt != nil || n.LeaseUntil != nil {
		t.Fatalf("waiting state: %+v", n)
	}
	if active, err := f.es.HasActiveIntent(ctx); err != nil || !active {
		t.Fatalf("waiting intent not active: %v %v", active, err)
	}
	if ok, err := f.es.ReopenIntent(ctx, iid); err != nil || ok {
		t.Fatalf("reopen bypassed the wait gate: %v %v", ok, err)
	}
	if _, err := f.es.ReopenIntentsByBlockedReason(ctx, BlockedReasonWaitingJob); err == nil {
		t.Fatal("bulk reopen accepted waiting_job")
	}
	o, err := f.es.Overview(ctx)
	if err != nil || !slices.ContainsFunc(o.Waiting, func(b NodeBrief) bool { return b.ID == iid }) ||
		slices.ContainsFunc(o.RecentTerminal, func(b NodeBrief) bool { return b.ID == iid }) {
		t.Fatalf("overview waiting=%v terminal=%v err=%v", o.Waiting, o.RecentTerminal, err)
	}
	if woken, err := f.d.WakeWaitingIntents(ctx, 100); err != nil || slices.ContainsFunc(woken, func(w WokenIntent) bool { return w.IntentID == iid }) {
		t.Fatalf("woken without event: %v %v", woken, err)
	}

	f.finishFileJob(t, job, file, "succeeded")
	if err := f.d.SetTaskPaused(ctx, f.task.ID, true); err != nil {
		t.Fatal(err)
	}
	if woken, _ := f.d.WakeWaitingIntents(ctx, 100); slices.ContainsFunc(woken, func(w WokenIntent) bool { return w.IntentID == iid }) {
		t.Fatal("paused task dispatched a woken intent")
	}
	if err := f.d.SetTaskPaused(ctx, f.task.ID, false); err != nil {
		t.Fatal(err)
	}
	woken, err := f.d.WakeWaitingIntents(ctx, 100)
	if err != nil || !slices.ContainsFunc(woken, func(w WokenIntent) bool { return w.IntentID == iid && w.ExplorationID == f.task.ExplorationID }) {
		t.Fatalf("resume did not rescan: %v %v", woken, err)
	}

	e2, ok, err := f.es.ClaimIntentWithLease(ctx, iid, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	ev, ok, err := f.es.TakeAgentJobEvent(ctx, iid, e2)
	if err != nil || !ok || ev.Job.ID != job || ev.TerminalStatus != "succeeded" {
		t.Fatalf("delivery: %+v %v %v", ev, ok, err)
	}
	if _, ok, _ := f.es.TakeAgentJobEvent(ctx, iid, e2); ok {
		t.Fatal("same epoch injected twice")
	}
	// transcript 未保存该投递：不确认，退回 pending 并继续。
	outcome, _, err = f.es.FinalizeIntentTurn(ctx, iid, e2, nil)
	if err != nil || outcome != TurnContinue {
		t.Fatalf("unsaved delivery: %s %v", outcome, err)
	}
	e3, ok, err := f.es.ClaimIntentWithLease(ctx, iid, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	again, ok, err := f.es.TakeAgentJobEvent(ctx, iid, e3)
	if err != nil || !ok || again.EventID != ev.EventID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, _, err := f.es.FinalizeIntentTurn(ctx, iid, e2, []int64{ev.EventID}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old epoch finalized: %v", err)
	}
	outcome, detail, err := f.es.FinalizeIntentTurn(ctx, iid, e3, AgentJobEventIDsIn("x [后台作业事件 #"+fmt.Sprint(ev.EventID)+"] y"))
	if err != nil || outcome != TurnDone {
		t.Fatalf("done: %s %s %v", outcome, detail, err)
	}
	subs := f.jobSubs(t, iid)
	if subs[0].State != "acknowledged" || subs[0].Event.State != "acknowledged" || subs[0].Event.AcknowledgedAt == nil {
		t.Fatalf("ack state: %+v %+v", subs[0], subs[0].Event)
	}
	n, _ = f.es.GetNode(ctx, iid)
	if n.State != StateIntentDone || n.CompletedAt == nil {
		t.Fatalf("final state: %+v", n)
	}
}

// 收尾与终态发布在订阅行上串行化：终态事务未提交时收尾等待，随后看到事件而继续，不会误入等待态丢唤醒。
func TestFinalizeSerializesWithTerminalPublication(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	iid, epoch := f.runningIntent(t, nil)
	job, _ := f.testFileJob(t)
	f.subscribe(t, iid, epoch, job)
	tx, err := f.d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fixed_file_jobs SET status = 'failed', error = 'late' WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}
	type res struct {
		o   TurnOutcome
		err error
	}
	done := make(chan res, 1)
	go func() {
		o, _, err := f.es.FinalizeIntentTurn(ctx, iid, epoch, nil)
		done <- res{o, err}
	}()
	select {
	case <-done:
		t.Fatal("finalize did not serialize with the terminal transaction")
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.o != TurnContinue {
		t.Fatalf("event committed during finalize was lost: %s %v", r.o, r.err)
	}
}

func TestRequiredJobFailureBlocksIntent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	aid, _, err := f.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: uniq("job-fail"), Code: "1", Title: "解析"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	iid, err := f.es.AddNode(ctx, KindIntent, StateIntentOpen, map[string]any{"summary": "解析"}, 1, "", []int64{aid})
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := f.es.ClaimIntentWithLease(ctx, iid, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	job, file := f.testFileJob(t)
	f.subscribe(t, iid, epoch, job)
	f.finishFileJob(t, job, file, "failed")
	if _, _, err := f.es.AddRoutedHint(ctx, "失败时交回主编审", []int64{aid}); err != nil {
		t.Fatal(err)
	}
	ev, ok, err := f.es.TakeAgentJobEvent(ctx, iid, epoch)
	if err != nil || !ok {
		t.Fatal(err)
	}
	outcome, detail, err := f.es.FinalizeIntentTurn(ctx, iid, epoch, []int64{ev.EventID})
	if err != nil || outcome != TurnBlocked || !strings.Contains(detail, "failed") {
		t.Fatalf("failed job completed: %s %q %v", outcome, detail, err)
	}
	n, _ := f.es.GetNode(ctx, iid)
	if n.State != StateIntentBlocked || n.BlockedReason != BlockedReasonRequiredJobFailed {
		t.Fatalf("blocked state: %+v", n)
	}
	var pending int
	if err := f.d.QueryRowContext(ctx, `SELECT count(*) FROM intent_inputs WHERE intent_id = $1 AND state IN ('pending','delivered')`, iid).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("hint stranded on failed intent: %d %v", pending, err)
	}
	if subs := f.jobSubs(t, iid); subs[0].Event.State != "acknowledged" {
		t.Fatalf("failure event not acknowledged: %+v", subs[0].Event)
	}
}

// waiting_job 保留未确认输入；只改 blocked_reason 离开等待也要交回 Hint。
func TestWaitingIntentKeepsInputsUntilLeavingWait(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	aid, _, err := f.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: uniq("wait-hint"), Code: "1", Title: "等待"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	iid, err := f.es.AddNode(ctx, KindIntent, StateIntentOpen, map[string]any{"summary": "等待"}, 1, "", []int64{aid})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.es.ClaimIntentWithLease(ctx, iid, "worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.es.AddRoutedHint(ctx, "等待期间保留", []int64{aid}); err != nil {
		t.Fatal(err)
	}
	state := func() string {
		var s string
		if err := f.d.QueryRowContext(ctx, `SELECT state FROM intent_inputs WHERE intent_id = $1`, iid).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE exploration_nodes SET state = 'blocked', blocked_reason = 'waiting_job' WHERE id = $1`, iid); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != "pending" {
		t.Fatalf("waiting returned input: %s", s)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE exploration_nodes SET blocked_reason = NULL WHERE id = $1`, iid); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != "returned" {
		t.Fatalf("leaving the wait kept input: %s", s)
	}
}

func TestSettleWaitingIntentsOnTaskEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	iid, epoch := f.runningIntent(t, nil)
	job, file := f.testFileJob(t)
	s := f.subscribe(t, iid, epoch, job)
	if o, _, err := f.es.FinalizeIntentTurn(ctx, iid, epoch, nil); err != nil || o != TurnWaiting {
		t.Fatalf("wait: %s %v", o, err)
	}
	if _, err := f.es.SettleWaitingIntents(ctx, StateIntentDone); err == nil {
		t.Fatal("settle accepted done")
	}
	if n, err := f.es.SettleWaitingIntents(ctx, StateIntentExhausted); err != nil || n != 1 {
		t.Fatalf("settle: %d %v", n, err)
	}
	f.finishFileJob(t, job, file, "succeeded") // 迟到的终态不再唤醒已结束的 Intent
	n, _ := f.es.GetNode(ctx, iid)
	if n.State != StateIntentExhausted {
		t.Fatalf("settled state: %+v", n)
	}
	subs := f.jobSubs(t, iid)
	if subs[0].ID != s.ID || subs[0].State != "cancelled" || subs[0].Event != nil {
		t.Fatalf("settled subscription: %+v %+v", subs[0], subs[0].Event)
	}
	if woken, _ := f.d.WakeWaitingIntents(ctx, 100); slices.ContainsFunc(woken, func(w WokenIntent) bool { return w.IntentID == iid }) {
		t.Fatal("settled intent revived")
	}
}

func TestAgentJobEventReplayAfterLeaseTakeover(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	iid, epoch := f.runningIntent(t, nil)
	job, file := f.testFileJob(t)
	f.subscribe(t, iid, epoch, job)
	f.finishFileJob(t, job, file, "succeeded")
	ev, ok, err := f.es.TakeAgentJobEvent(ctx, iid, epoch)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := f.d.ExecContext(ctx, `UPDATE exploration_nodes SET lease_until = now() - interval '1 second' WHERE id = $1`, iid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.es.ReapExpiredRunningIntents(ctx); err != nil {
		t.Fatal(err)
	}
	next, ok, err := f.es.ClaimIntentWithLease(ctx, iid, "replacement", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	again, ok, err := f.es.TakeAgentJobEvent(ctx, iid, next)
	if err != nil || !ok || again.EventID != ev.EventID {
		t.Fatalf("delivered event lost after takeover: %+v %v", again, err)
	}
	if o, _, err := f.es.FinalizeIntentTurn(ctx, iid, next, []int64{again.EventID}); err != nil || o != TurnDone {
		t.Fatalf("takeover finalize: %s %v", o, err)
	}
}

// 终态发布覆盖真实入口：截止/尝试耗尽回收、任务暂停取消、导出成功（仅预览）；回到 queued 不发布。
func TestAgentJobTerminalPathsPublish(t *testing.T) {
	p := newProposalFixture(t)
	o, _ := p.approvedOutline(t)
	ctx := context.Background()
	iid, epoch := p.runningIntent(t, nil)

	// 截止时间已过的作业在下一次领取时被判失败。
	seedJob, fileID := p.testFileJob(t)
	if _, err := p.d.ExecContext(ctx, `UPDATE fixed_file_jobs SET status = 'cancelled' WHERE id = $1`, seedJob); err != nil {
		t.Fatal(err) // 只借用它的原件；不让它排在过期作业之前被领取
	}
	var expired int64
	if err := p.d.QueryRowContext(ctx, `INSERT INTO fixed_file_jobs(task_id, file_id, source_hash, request_key, fingerprint, input, deadline_at)
		SELECT task_id, id, sha256, $2, 'fp', '{}', now() - interval '1 second' FROM task_file_objects WHERE id = $1 RETURNING id`, fileID, uniq("rk")).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	subExpired := p.subscribe(t, iid, epoch, expired)
	if _, err := p.d.ClaimFixedFileJob(ctx, "runner", time.Minute, p.task.ID); err != nil {
		t.Fatal(err)
	}

	// 导出作业：租约过期回到 queued 不是终态；成功后业务状态为 preview_only。
	var exportID int64
	if err := p.d.QueryRowContext(ctx, `INSERT INTO export_jobs(task_id, outline_id, outline_version, format, rev_snapshot)
		VALUES ($1, $2, $3, 'docx', '{}') RETURNING id`, p.task.ID, o.Outline.ID, o.Outline.Version).Scan(&exportID); err != nil {
		t.Fatal(err)
	}
	version := int64(o.Outline.Version)
	subExport, err := p.d.SubscribeAgentJob(WithIntentExecution(ctx, iid, epoch), DefaultTenant, p.task.ID,
		SubscribeAgentJobInput{Job: JobRef{Kind: JobKindProposalExport, ID: exportID}, ExpectedVersion: &version})
	if err != nil {
		t.Fatal(err)
	}
	job, err := p.d.ClaimExportJob(ctx, "runner", time.Minute, p.task.ID)
	if err != nil || job == nil || job.ID != exportID {
		t.Fatalf("claim export: %+v %v", job, err)
	}
	if err := p.d.FailExportJob(ctx, exportID, job.LeaseToken, "transient", true); err != nil {
		t.Fatal(err)
	}
	if n := p.eventCount(t, subExport.ID); n != 0 {
		t.Fatalf("retry back to queued published %d events", n)
	}
	job, err = p.d.ClaimExportJob(ctx, "runner", time.Minute, p.task.ID)
	if err != nil || job == nil {
		t.Fatalf("reclaim export: %v", err)
	}
	if err := p.d.CompleteExportJob(ctx, exportID, job.LeaseToken, ExportResult{ArtifactPath: "a.docx", ArtifactName: "a.docx", ArtifactHash: "h", ArtifactSize: 1}); err != nil {
		t.Fatal(err)
	}

	// 任务暂停取消排队中的解析作业。
	queued, _ := p.testFileJob(t)
	subQueued := p.subscribe(t, iid, epoch, queued)
	if err := p.d.SetTaskPaused(ctx, p.task.ID, true); err != nil {
		t.Fatal(err)
	}

	want := map[int64][2]string{subExpired.ID: {"failed", "failed"}, subExport.ID: {"succeeded", "preview_only"}, subQueued.ID: {"cancelled", "cancelled"}}
	for _, s := range p.jobSubs(t, iid) {
		w, ok := want[s.ID]
		if !ok {
			continue
		}
		if s.State != "notified" || s.Event == nil || s.Event.TerminalStatus != w[0] || s.Event.BusinessState != w[1] {
			t.Fatalf("subscription %d (%s #%d): %+v %+v, want %v", s.ID, s.Job.Kind, s.Job.ID, s, s.Event, w)
		}
		delete(want, s.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing subscriptions: %v", want)
	}
}
