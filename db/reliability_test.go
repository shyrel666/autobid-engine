package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSectionWriteJobRecoveryAndFencing(t *testing.T) {
	p := newProposalFixture(t)
	_, ids := p.approvedOutline(t)
	ctx := context.Background()
	sid := ids["1"]
	j, err := p.d.EnqueueSectionWrite(ctx, DefaultTenant, p.task.ID, sid, "same", "keep this instruction", "writer", nil)
	if err != nil {
		t.Fatal(err)
	}
	dup, err := p.d.EnqueueSectionWrite(ctx, DefaultTenant, p.task.ID, sid, "same", "keep this instruction", "writer", nil)
	if err != nil || dup.ID != j.ID {
		t.Fatalf("idempotency: %v %+v", err, dup)
	}
	if _, err = p.d.EnqueueSectionWrite(ctx, DefaultTenant, p.task.ID, sid, "same", "changed", "writer", nil); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("different input accepted: %v", err)
	}
	if _, err = p.d.EnqueueSectionWrite(ctx, DefaultTenant, p.task.ID, sid, "different", "", "writer", nil); !errors.Is(err, ErrSectionBusy) {
		t.Fatalf("duplicate active request: %v", err)
	}
	var claims sync.WaitGroup
	claimed := make(chan *SectionWriteJob, 8)
	for range 8 {
		claims.Add(1)
		go func() {
			defer claims.Done()
			c, e := p.d.ClaimSectionWriteJob(ctx, p.task.ID, 1)
			if e != nil {
				t.Error(e)
			}
			if c != nil {
				claimed <- c
			}
		}()
	}
	claims.Wait()
	close(claimed)
	if len(claimed) != 1 {
		t.Fatalf("concurrent executors claimed %d times", len(claimed))
	}
	first := <-claimed
	if _, err = p.d.ExecContext(ctx, `UPDATE section_write_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	// A fresh DB wrapper simulates a new executor with no local state.
	restarted := &DB{DB: p.d.DB}
	second, err := restarted.ClaimSectionWriteJob(ctx, p.task.ID, 1)
	if err != nil || second == nil {
		t.Fatalf("reclaim: %v", err)
	}
	if second.Attempts != 2 || second.LeaseToken == first.LeaseToken || !second.DeadlineAt.Equal(j.DeadlineAt) || second.Instruction != j.Instruction {
		t.Fatalf("lost immutable input or budget: %+v", second)
	}
	in := SaveRevisionInput{Source: RevAIInitial, ContentMD: "恢复后的章节正文", SummaryNote: "摘要"}
	if _, err = p.d.FinishSectionWriteJob(ctx, first, in); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale executor wrote: %v", err)
	}
	res, err := restarted.FinishSectionWriteJob(ctx, second, in)
	if err != nil || res.Outcome != SaveApplied {
		t.Fatalf("finish: %v %+v", err, res)
	}
	if _, err = p.d.FinishSectionWriteJob(ctx, second, in); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("duplicate finish: %v", err)
	}
	det, err := p.d.GetSection(ctx, DefaultTenant, p.task.ID, sid)
	if err != nil || len(det.Revisions) != 1 {
		t.Fatalf("duplicate revisions: %v", err)
	}
	j, err = p.d.GetSectionWriteJob(ctx, DefaultTenant, p.task.ID, j.ID)
	if err != nil || j.Status != "awaiting_review" {
		t.Fatalf("finish status: %+v %v", j, err)
	}
}

func TestSectionWriteJobCancelPauseAndBudget(t *testing.T) {
	p := newProposalFixture(t)
	_, ids := p.approvedOutline(t)
	ctx := context.Background()
	sid := ids["1"]
	enqueue := func(key string) *SectionWriteJob {
		t.Helper()
		j, err := p.d.EnqueueSectionWrite(ctx, DefaultTenant, p.task.ID, sid, key, "", "writer", nil)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	j := enqueue("cancel")
	running, err := p.d.ClaimSectionWriteJob(ctx, p.task.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.d.CancelSectionWriteJob(ctx, DefaultTenant, p.task.ID, j.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	if err = p.d.HeartbeatSectionWriteJob(ctx, running); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("cancel heartbeat: %v", err)
	}
	if _, err = p.d.FinishSectionWriteJob(ctx, running, SaveRevisionInput{Source: RevAIInitial, ContentMD: "late"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("cancel write: %v", err)
	}
	j = enqueue("paused")
	running, err = p.d.ClaimSectionWriteJob(ctx, p.task.ID, 1)
	if err != nil || running == nil {
		t.Fatalf("running before pause: %v", err)
	}
	if err = p.d.SetTaskPaused(ctx, p.task.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, err := p.d.ClaimSectionWriteJob(ctx, p.task.ID, 1); err != nil || got != nil {
		t.Fatalf("paused claim: %v %+v", err, got)
	}
	if err = p.d.SetTaskPaused(ctx, p.task.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err = p.d.FinishSectionWriteJob(ctx, running, SaveRevisionInput{Source: RevAIInitial, ContentMD: "暂停前旧任务"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("pause/resume fence: %v", err)
	}
	if _, err = p.d.ExecContext(ctx, `UPDATE section_write_jobs SET deadline_at=now()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := p.d.ClaimSectionWriteJob(ctx, p.task.ID, 1); err != nil || got != nil {
		t.Fatalf("expired claim: %v %+v", err, got)
	}
	j, err = p.d.GetSectionWriteJob(ctx, DefaultTenant, p.task.ID, j.ID)
	if err != nil || j.Status != "blocked" {
		t.Fatalf("expired status: %v %+v", err, j)
	}
	j = enqueue("exhausted")
	if _, err = p.d.ExecContext(ctx, `UPDATE section_write_jobs SET attempts=max_attempts WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := p.d.ClaimSectionWriteJob(ctx, p.task.ID, 1); err != nil || got != nil {
		t.Fatalf("exhausted budget claimed: %+v %v", got, err)
	}
	j, err = p.d.GetSectionWriteJob(ctx, DefaultTenant, p.task.ID, j.ID)
	if err != nil || j.Status != "blocked" {
		t.Fatalf("attempt budget status: %+v %v", j, err)
	}
}

func TestConcurrentHintAcknowledgementsAndTerminalReturn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	aid, _, err := f.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: uniq("hint-broadcast"), Code: "1", Title: "共同指令"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 2)
	epochs := make([]int64, 2)
	for k := range ids {
		ids[k], err = f.es.AddNode(ctx, KindIntent, StateIntentOpen, map[string]any{"summary": "work"}, 1, "", []int64{aid})
		if err != nil {
			t.Fatal(err)
		}
		var ok bool
		epochs[k], ok, err = f.es.ClaimIntentWithLease(ctx, ids[k], "worker", time.Minute)
		if err != nil || !ok {
			t.Fatal(err)
		}
	}
	hid, recipients, err := f.es.AddRoutedHint(ctx, "共享要求", []int64{aid})
	if err != nil || len(recipients) != 2 {
		t.Fatalf("recipients: %v %v", recipients, err)
	}
	for k, id := range ids {
		if _, ok, err := f.es.TakeIntentInput(ctx, id, epochs[k]); err != nil || !ok {
			t.Fatalf("delivery: %v", err)
		}
	}
	var wg sync.WaitGroup
	for k, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.es.AcknowledgeIntentInputs(ctx, id, epochs[k]); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	h, err := f.es.GetNode(ctx, hid)
	if err != nil || h.State != StateHintConsumed {
		t.Fatalf("concurrent ack lost: %+v %v", h, err)
	}
	hid, _, err = f.es.AddRoutedHint(ctx, "任务失败后交回主编审", []int64{aid})
	if err != nil {
		t.Fatal(err)
	}
	for k, id := range ids {
		if ok, err := f.es.CompareAndSetIntentStateFenced(ctx, id, StateIntentRunning, StateIntentBlocked, epochs[k]); err != nil || !ok {
			t.Fatalf("block: %v", err)
		}
	}
	if ok, err := f.es.ConsumeHint(ctx, hid); err != nil || !ok {
		t.Fatalf("terminal recipients stranded hint: %v", err)
	}
}

func TestDurableHintReplayAndAcknowledgement(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := NewExplorationStore(f.d, f.task.ExplorationID)
	aid, _, err := f.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: uniq("hint"), Code: "1", Title: "人工变更"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	iid, err := s.AddNode(ctx, KindIntent, StateIntentOpen, map[string]any{"summary": "work"}, 1, "", []int64{aid})
	if err != nil {
		t.Fatal(err)
	}
	epoch, ok, err := s.ClaimIntentWithLease(ctx, iid, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	hid, recipients, err := s.AddRoutedHint(ctx, "important instruction", []int64{aid})
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients) != 1 || recipients[0] != iid {
		t.Fatalf("routing lost recipient: %v", recipients)
	}
	if ok, err := s.ConsumeHint(ctx, hid); err != nil || ok {
		t.Fatalf("premature consumption: %v %v", ok, err)
	}
	msg, ok, err := s.TakeIntentInput(ctx, iid, epoch)
	if err != nil || !ok || msg == "" {
		t.Fatalf("delivery: %v %v", ok, err)
	}
	if _, ok, err := s.TakeIntentInput(ctx, iid, epoch); err != nil || ok {
		t.Fatalf("duplicate within attempt: %v", err)
	}
	if _, err = f.d.ExecContext(ctx, `UPDATE exploration_nodes SET lease_until=now()-interval '1 second' WHERE id=$1`, iid); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReapExpiredRunningIntents(ctx); err != nil {
		t.Fatal(err)
	}
	next, ok, err := s.ClaimIntentWithLease(ctx, iid, "replacement", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err = s.AcknowledgeIntentInputs(ctx, iid, epoch); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old owner acknowledged: %v", err)
	}
	if again, ok, err := s.TakeIntentInput(ctx, iid, next); err != nil || !ok || again != msg {
		t.Fatalf("lost input after restart: %v %v %q", err, ok, again)
	}
	if err = s.AcknowledgeIntentInputs(ctx, iid, next); err != nil {
		t.Fatal(err)
	}
	h, err := s.GetNode(ctx, hid)
	if err != nil || h.State != StateHintConsumed {
		t.Fatalf("ack state: %v %+v", err, h)
	}
}

func TestIntentRequiredSectionCompletion(t *testing.T) {
	p := newProposalFixture(t)
	o, ids := p.approvedOutline(t)
	ctx := context.Background()
	sid := ids["1"]
	iid, err := p.es.AddNode(ctx, KindIntent, StateIntentOpen, map[string]any{"summary": "write section", "mandate": IntentMandate{RequiredSectionIDs: []int64{sid}}}, 1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	epoch, ok, err := p.es.ClaimIntentWithLease(ctx, iid, "writer", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	n, err := p.es.GetNode(ctx, iid)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.d.CheckIntentDeliverables(ctx, p.task.ID, n); err == nil {
		t.Fatal("empty chapter accepted")
	}
	p.save(t, sid, 0, o.Outline.Version, RevAIInitial, "有效技术方案", 0)
	if err := p.d.CheckIntentDeliverables(ctx, p.task.ID, n); err == nil {
		t.Fatal("another writer's output accepted")
	}
	producerCtx := WithIntentExecution(ctx, iid, epoch)
	in := SaveRevisionInput{TaskID: p.task.ID, SectionID: sid, BaseRevNo: 1, BasedOnOutlineVersion: o.Outline.Version, Source: RevAIRewrite, ContentMD: "本任务实际产出的有效技术方案"}
	if _, err = p.d.SaveSectionRevision(producerCtx, DefaultTenant, "worker", in); err != nil {
		t.Fatal(err)
	}
	if err := p.d.CheckIntentDeliverables(ctx, p.task.ID, n); err != nil {
		t.Fatal(err)
	}
	if err = p.es.CompleteIntentOrContinue(ctx, iid, epoch); err != nil {
		t.Fatal(err)
	}
	in.BaseRevNo = 2
	if _, err = p.d.SaveSectionRevision(producerCtx, DefaultTenant, "worker", in); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("finished producer wrote: %v", err)
	}
	p.save(t, sid, 2, o.Outline.Version, RevHumanEdit, "其他人的替换稿", 0)
	if err := p.d.CheckIntentDeliverables(ctx, p.task.ID, n); err == nil {
		t.Fatal("superseded output accepted")
	}
}

func TestHintArrivingAtCompletionRemainsActionable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	aid, _, err := f.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: uniq("hint-race"), Code: "1", Title: "交付要求"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	iid, err := f.es.AddNode(ctx, KindIntent, StateIntentOpen, map[string]any{"summary": "work"}, 1, "", []int64{aid})
	if err != nil {
		t.Fatal(err)
	}
	epoch, ok, err := f.es.ClaimIntentWithLease(ctx, iid, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	hid, _, err := f.es.AddRoutedHint(ctx, "最后时刻的新要求", []int64{aid})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.es.CompleteIntentOrContinue(ctx, iid, epoch); err != nil {
		t.Fatal(err)
	}
	n, err := f.es.GetNode(ctx, iid)
	if err != nil || n.State != StateIntentOpen {
		t.Fatalf("pending input lost at completion: %+v %v", n, err)
	}
	next, ok, err := f.es.ClaimIntentWithLease(ctx, iid, "replacement", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := f.es.TakeIntentInput(ctx, iid, next); err != nil || !ok {
		t.Fatalf("no replay: %v", err)
	}
	if err = f.es.AcknowledgeIntentInputs(ctx, iid, next); err != nil {
		t.Fatal(err)
	}
	if err = f.es.CompleteIntentOrContinue(ctx, iid, next); err != nil {
		t.Fatal(err)
	}
	_, recipients, err := f.es.AddRoutedHint(ctx, "完成后新要求交回主编审", []int64{aid})
	if err != nil || len(recipients) != 0 {
		t.Fatalf("terminal recipient: %v %v", recipients, err)
	}
	h, err := f.es.GetNode(ctx, hid)
	if err != nil || h.State != StateHintConsumed {
		t.Fatalf("acknowledged hint: %+v %v", h, err)
	}
}
