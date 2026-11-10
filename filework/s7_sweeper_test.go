package filework

import (
	"context"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/ingest"
)

// TestSweeperWhileFileProcessingIsOff: with file processing switched off there is no Runner, so
// nothing claims jobs. A job queued before the switch-off must still end when its budget is used
// up, exactly as a claim would end it, and the intent waiting on it must get the job event instead
// of waiting until the task deadline. A run that crashed goes back to the queue. Jobs with budget
// left are not touched, and the export snapshot fence only moves for jobs that change.
func TestSweeperWhileFileProcessingIsOff(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	newTask := func(name string) *db.Task {
		task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq(name), Goal: "file processing off"})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	store, err := NewStore(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &Processor{DB: d, Store: store}
	enqueue := func(task *db.Task, key string) *db.FixedFileJob {
		t.Helper()
		obj := putFile(t, store, task.ID, key+".txt", ingest.FormatText, []byte("第一章 投标须知\n1.1 "+key+"\n"))
		j, err := p.Enqueue(ctx, task.ID, ProcessInput{FileID: obj.ID, TenderNo: "SWEEP", RequestKey: key}, "tester")
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	gate := func(task *db.Task) (v int64) {
		t.Helper()
		if err := d.QueryRow(`SELECT file_gate_version FROM tasks WHERE id=$1`, task.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	task := newTask("sweep")
	exhausted, fresh, crashed := enqueue(task, "exhausted"), enqueue(task, "fresh"), enqueue(task, "crashed")
	ictx, es, iid, epoch := intentCtx(t, d, task, nil)
	if _, err := d.SubscribeAgentJob(ictx, db.DefaultTenant, task.ID, db.SubscribeAgentJobInput{Job: db.JobRef{Kind: db.JobKindTenderParse, ID: exhausted.ID}, Actor: "worker"}); err != nil {
		t.Fatal(err)
	}
	// attempts is the mutable half of the budget; the deadline half goes through the same SQL.
	if _, err := d.Exec(`UPDATE fixed_file_jobs SET attempts=max_attempts WHERE id=$1`, exhausted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE fixed_file_jobs SET status='running',attempts=1,lease_until=now()-interval '1 second',lease_owner='dead-process' WHERE id=$1`, crashed.ID); err != nil {
		t.Fatal(err)
	}
	crashedToken := jobByID(t, d, task.ID, crashed.ID).LeaseToken
	before := gate(task)

	changed, err := d.SweepFixedFileJobs(ctx, "sweeper", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Fatalf("changed %d jobs, want the exhausted and the crashed one: %+v", len(changed), changed)
	}
	if j := jobByID(t, d, task.ID, exhausted.ID); j.Status != "failed" || j.ErrorCode != db.JobErrBudgetExhausted {
		t.Fatalf("exhausted job: %s %s", j.Status, j.ErrorCode)
	}
	if j := jobByID(t, d, task.ID, crashed.ID); j.Status != "queued" || j.LeaseToken != crashedToken+1 || j.Attempts != 1 {
		t.Fatalf("crashed job must return to the queue with a new fence and its attempt counted: %+v", j)
	}
	if j := jobByID(t, d, task.ID, fresh.ID); j.Status != "queued" || j.LeaseToken != fresh.LeaseToken {
		t.Fatalf("job with budget left must not change: %+v", j)
	}
	if got := gate(task); got != before+2 {
		t.Fatalf("file gate moved %d times, want once per changed job (2)", got-before)
	}

	// The waiting intent receives the terminal event, as if a runner had ended the job.
	ev, ok, err := es.TakeAgentJobEvent(ctx, iid, epoch)
	if err != nil || !ok || ev.TerminalStatus != "failed" {
		t.Fatalf("waiting intent event: %+v ok=%v err=%v", ev, ok, err)
	}

	// A second sweep changes nothing.
	if again, err := d.SweepFixedFileJobs(ctx, "sweeper", task.ID); err != nil || len(again) != 0 {
		t.Fatalf("second sweep: %v %+v", err, again)
	}

	// The Sweeper loop is what the server runs in place of the Runner. (Pausing a task cancels its
	// fixed jobs, so no queued job ever waits inside a paused task.)
	other := newTask("sweep-loop")
	held := enqueue(other, "held")
	if _, err := d.Exec(`UPDATE fixed_file_jobs SET attempts=max_attempts WHERE id=$1`, held.ID); err != nil {
		t.Fatal(err)
	}
	notified := make(chan *db.FixedFileJob, 1)
	loop, stop := context.WithCancel(ctx)
	defer stop()
	go (&Sweeper{DB: d, Owner: "sweeper", Every: 20 * time.Millisecond, TaskID: other.ID, OnChange: func(j *db.FixedFileJob) { notified <- j }}).Run(loop)
	select {
	case j := <-notified:
		if j.ID != held.ID || j.Status != "failed" || j.ErrorCode != db.JobErrBudgetExhausted {
			t.Fatalf("sweeper loop: %+v", j)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sweeper loop did not end the exhausted job")
	}
}
