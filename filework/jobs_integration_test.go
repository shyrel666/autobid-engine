package filework

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/ingest"
)

type delayedOCR struct {
	entered chan struct{}
	release chan struct{}
}

func (o *delayedOCR) Name() string { return "test-delayed" }
func (o *delayedOCR) Recognize(context.Context, []byte, string) (*ingest.OCRPage, error) {
	close(o.entered)
	<-o.release // deliberately ignore cancellation to test the write fence
	return &ingest.OCRPage{Width: 100, Height: 100, Lines: []ingest.OCRLine{{Text: "第一章 技术要求", Confidence: 1}, {Text: "服务器数量不少于12台", Confidence: 1}}}, nil
}
func TestFixedJobLateWorkerCannotCommit(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq("late-fixed"), Goal: "file"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	obj, err := store.PutBytes(ctx, PutOptions{TenantID: db.DefaultTenant, TaskID: task.ID, Kind: db.FileKindOriginal, OriginalName: "scan.png", Format: "image"}, []byte("test OCR input"))
	if err != nil {
		t.Fatal(err)
	}
	delayed := &delayedOCR{entered: make(chan struct{}), release: make(chan struct{})}
	p := &Processor{DB: d, Store: store, OCR: delayed}
	j, err := p.Enqueue(ctx, task.ID, ProcessInput{FileID: obj.ID, TenderNo: dbtest.Uniq("scan"), RequestKey: "once"}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Processor: p, Owner: "old", TaskID: task.ID, Lease: time.Minute}
	done := make(chan error, 1)
	go func() { _, err := runner.RunOnce(ctx); done <- err }()
	select {
	case <-delayed.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("OCR never started")
	}
	if _, err := d.Exec(`UPDATE fixed_file_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	next, err := d.ClaimFixedFileJob(ctx, "new", time.Minute, task.ID)
	if err != nil || next == nil {
		t.Fatalf("takeover=%+v %v", next, err)
	}
	close(delayed.release)
	select {
	case err := <-done:
		if !errors.Is(err, db.ErrLeaseLost) {
			t.Fatalf("old writer=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late worker stuck")
	}
	var docs, records int
	d.QueryRow(`SELECT count(*) FROM tender_documents WHERE task_id=$1`, task.ID).Scan(&docs)
	d.QueryRow(`SELECT count(*) FROM file_processing_records WHERE task_id=$1`, task.ID).Scan(&records)
	if docs != 0 || records != 0 {
		t.Fatalf("late worker committed docs=%d records=%d", docs, records)
	}
	// The current attempt can commit its candidate/document and receipt atomically.
	if err := d.CancelFixedFileJob(ctx, db.DefaultTenant, task.ID, j.ID, "test cleanup"); err != nil {
		t.Fatal(err)
	}
}

func TestFixedJobOutputRollbackOnCompletionFailure(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq("fixed-rollback"), Goal: "file"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	obj, err := store.PutBytes(ctx, PutOptions{TenantID: db.DefaultTenant, TaskID: task.ID, Kind: db.FileKindOriginal, OriginalName: "list.csv", Format: "csv"}, []byte("名称,数量\n服务器,12\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Processor{DB: d, Store: store}
	j, err := p.Enqueue(ctx, task.ID, ProcessInput{FileID: obj.ID, TenderNo: dbtest.Uniq("rollback"), RequestKey: "once"}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	// A task-specific constraint fails only the final success update, after PersistTender.
	// Use a generated numeric task ID for SQL identifiers/values, never user input.
	name := "fixed_completion_guard_" + fmt.Sprint(task.ID)
	if _, err := d.Exec(`ALTER TABLE fixed_file_jobs ADD CONSTRAINT ` + name + ` CHECK (id <> ` + fmt.Sprint(j.ID) + ` OR status <> 'succeeded')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`ALTER TABLE fixed_file_jobs DROP CONSTRAINT IF EXISTS ` + name) })
	r := &Runner{Processor: p, Owner: "rollback", TaskID: task.ID}
	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("completion failure was hidden")
	}
	var count int
	d.QueryRow(`SELECT count(*) FROM tender_documents WHERE task_id=$1`, task.ID).Scan(&count)
	if count != 0 {
		t.Fatal("document escaped rollback")
	}
	d.QueryRow(`SELECT count(*) FROM file_processing_records WHERE task_id=$1`, task.ID).Scan(&count)
	if count != 0 {
		t.Fatal("record escaped rollback")
	}
	d.QueryRow(`SELECT count(*) FROM assets WHERE task_ids @> ARRAY[$1::bigint]`, task.ID).Scan(&count)
	if count != 0 {
		t.Fatal("asset escaped rollback")
	}
}

func TestFixedJobPauseWaitsForInFlightWorker(t *testing.T) {
	d := dbtest.Open(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: dbtest.Uniq("fixed-pause"), Goal: "file"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	obj, err := store.PutBytes(ctx, PutOptions{TenantID: db.DefaultTenant, TaskID: task.ID, Kind: db.FileKindOriginal, OriginalName: "scan.png", Format: "image"}, []byte("pause-test"))
	if err != nil {
		t.Fatal(err)
	}
	ocr := &delayedOCR{entered: make(chan struct{}), release: make(chan struct{})}
	p := &Processor{DB: d, Store: store, OCR: ocr}
	job, err := p.Enqueue(ctx, task.ID, ProcessInput{FileID: obj.ID, TenderNo: dbtest.Uniq("pause"), RequestKey: "pause"}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Processor: p, Owner: "pause-test", TaskID: task.ID}
	done := make(chan struct{})
	go func() { defer close(done); runner.RunOnce(ctx) }()
	select {
	case <-ocr.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("OCR not started")
	}
	if err := d.SetTaskPaused(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := runner.StopTask(stopCtx, task.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop barrier: %v", err)
	}
	close(ocr.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	latest, err := d.GetFixedFileJob(ctx, db.DefaultTenant, task.ID, job.ID)
	if err != nil || latest.Status != "cancelled" {
		t.Fatalf("cancel state: %+v %v", latest, err)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM tender_documents WHERE task_id=$1`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cancelled output: %d %v", count, err)
	}
}
