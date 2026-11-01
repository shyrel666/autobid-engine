package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/filework"
)

func streamFile(t *testing.T, c *fileAPI, task int64, name string, data []byte) *db.TaskFileObject {
	t.Helper()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/tasks/%d/files?filename=%s", c.url, task, name), bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 201 {
		t.Fatalf("stream %d: %s", res.StatusCode, b)
	}
	var obj db.TaskFileObject
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	return &obj
}
func fixedRunner(c *fileAPI, task int64) *filework.Runner {
	return &filework.Runner{Processor: &filework.Processor{DB: c.pg, Store: c.store}, Owner: "test-worker", TaskID: task}
}
func checkMaterialBlock(t *testing.T, c *fileAPI, task int64, want int) {
	t.Helper()
	r, err := c.pg.ExportReadiness(context.Background(), db.DefaultTenant, task)
	if err != nil {
		t.Fatal(err)
	}
	if r.Counts["required_material_incomplete"] != want {
		t.Fatalf("material blockers: %+v", r)
	}
}
func TestFixedAsyncUploadMaterialsAndRecovery(t *testing.T) {
	c := newFileAPI(t)
	ctx := context.Background()
	task := c.createTask(t)
	base := fmt.Sprintf("/api/tasks/%d", task)
	var material db.TaskMaterial
	input := map[string]any{"name": "设备清单", "required": true}
	if code := c.do("POST", base+"/materials", input, &material); code != 200 {
		t.Fatal(code)
	}
	checkMaterialBlock(t, c, task, 1)
	content := []byte("名称,数量\n服务器,12\n")
	obj := streamFile(t, c, task, "list.csv", content)
	read, _, err := c.store.Read(ctx, db.DefaultTenant, task, obj.ID)
	if err != nil || !bytes.Equal(read, content) {
		t.Fatalf("original: %v", err)
	}
	material.FileID = &obj.ID
	if code := c.do("POST", base+"/materials", material, &material); code != 200 {
		t.Fatal(code)
	}
	checkMaterialBlock(t, c, task, 1)
	var job, replayed db.FixedFileJob
	req := map[string]any{"file_id": obj.ID, "tender_no": fmt.Sprintf("ASYNC-%d", task), "request_key": "first"}
	if code := c.do("POST", base+"/file-jobs", req, &job); code != 202 || job.Status != "queued" {
		t.Fatalf("queue=%d %+v", code, job)
	}
	if code := c.do("POST", base+"/file-jobs", req, &replayed); code != 202 || replayed.ID != job.ID {
		t.Fatalf("replay=%d %+v", code, replayed)
	}
	req["tender_no"] = "changed"
	if code := c.do("POST", base+"/file-jobs", req, nil); code != 409 {
		t.Fatalf("conflict=%d", code)
	}
	other := c.createTask(t)
	if code := c.do("GET", fmt.Sprintf("/api/tasks/%d/file-jobs/%d", other, job.ID), nil, nil); code != 404 {
		t.Fatalf("scope=%d", code)
	}
	runner := fixedRunner(c, task)
	// Simulate restart after a worker claimed the job but before any output committed.
	old, err := c.pg.ClaimFixedFileJob(ctx, "dead-worker", time.Minute, task)
	if err != nil || old == nil {
		t.Fatal(err)
	}
	if _, err := c.pg.Exec(`UPDATE fixed_file_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := runner.RunOnce(ctx); !ok || err != nil {
		t.Fatalf("recover=%v %v", ok, err)
	}
	current, err := c.pg.GetFixedFileJob(ctx, db.DefaultTenant, task, job.ID)
	if err != nil || current.Status != "succeeded" || current.Attempts != 2 || current.RecordID == nil {
		t.Fatalf("result=%+v %v", current, err)
	}
	err = c.pg.Transaction(ctx, func(tx *db.DB) error { return tx.FinishFixedFileJob(ctx, old, nil, "late writer") })
	if !errors.Is(err, db.ErrLeaseLost) {
		t.Fatalf("stale writer=%v", err)
	}
	checkMaterialBlock(t, c, task, 0)
	if ok, err := runner.RunOnce(ctx); ok || err != nil {
		t.Fatalf("duplicate execution=%v %v", ok, err)
	}
	var docs int
	if err := c.pg.QueryRow(`SELECT count(*) FROM tender_documents WHERE task_id=$1`, task).Scan(&docs); err != nil || docs != 1 {
		t.Fatalf("docs=%d %v", docs, err)
	}
	// Successful parsing is a candidate for existing documents, never implicit adoption.
	var docID int64
	c.pg.QueryRow(`SELECT id FROM tender_documents WHERE task_id=$1`, task).Scan(&docID)
	if code := c.do("POST", base+"/file-jobs", map[string]any{"document_id": docID, "expected_revision": 0, "request_key": "reparse"}, &job); code != 202 {
		t.Fatal(code)
	}
	checkMaterialBlock(t, c, task, 1)
	if _, err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	checkMaterialBlock(t, c, task, 1)
	current, _ = c.pg.GetFixedFileJob(ctx, db.DefaultTenant, task, job.ID)
	if code := c.do("POST", fmt.Sprintf("%s/file-processing-records/%d/adopt", base, *current.RecordID), map[string]any{"expected_revision": 0}, nil); code != 200 {
		t.Fatal(code)
	}
	checkMaterialBlock(t, c, task, 0)
	if err := c.pg.MarkTaskFileObjectMissing(ctx, db.DefaultTenant, task, obj.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	checkMaterialBlock(t, c, task, 1)
	// A non-required material does not veto export; reason and optimistic lock required.
	stale := material
	material.Required = false
	material.Note = ""
	if code := c.do("POST", base+"/materials", material, nil); code != 400 {
		t.Fatalf("missing reason=%d", code)
	}
	material.Note = "本次投标不适用"
	if code := c.do("POST", base+"/materials", material, &material); code != 200 {
		t.Fatal(code)
	}
	if code := c.do("POST", base+"/materials", stale, nil); code != 409 {
		t.Fatalf("stale update=%d", code)
	}
	checkMaterialBlock(t, c, task, 0)
	var events int
	c.pg.QueryRow(`SELECT count(*) FROM task_material_events WHERE material_id=$1`, material.ID).Scan(&events)
	if events != 3 {
		t.Fatalf("audit events=%d", events)
	}
}

func TestFixedAsyncFailureCancelAndArchive(t *testing.T) {
	c := newFileAPI(t)
	ctx := context.Background()
	task := c.createTask(t)
	base := fmt.Sprintf("/api/tasks/%d", task)
	r := fixedRunner(c, task)
	obj := streamFile(t, c, task, "broken.docx", []byte("invalid docx"))
	var j db.FixedFileJob
	req := map[string]any{"file_id": obj.ID, "tender_no": "BROKEN", "request_key": "bad"}
	if code := c.do("POST", base+"/file-jobs", req, &j); code != 202 {
		t.Fatal(code)
	}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := c.pg.GetFixedFileJob(ctx, db.DefaultTenant, task, j.ID)
	if got.Status != "failed" || got.RecordID == nil || got.Attempts != 1 {
		t.Fatalf("failed=%+v", got)
	}
	if ok, _ := r.RunOnce(ctx); ok {
		t.Fatal("deterministic error auto-retried")
	}
	if _, _, err := c.store.Read(ctx, db.DefaultTenant, task, obj.ID); err != nil {
		t.Fatal(err)
	}
	req["request_key"] = "explicit-retry"
	if code := c.do("POST", base+"/file-jobs", req, &j); code != 202 || j.ID == got.ID {
		t.Fatal(code)
	}
	if code := c.do("POST", fmt.Sprintf("%s/file-jobs/%d/cancel", base, j.ID), map[string]any{}, nil); code != 200 {
		t.Fatal(code)
	}
	if ok, _ := r.RunOnce(ctx); ok {
		t.Fatal("cancelled job ran")
	}
	req["request_key"] = "pause-me"
	if code := c.do("POST", base+"/file-jobs", req, &j); code != 202 {
		t.Fatal(code)
	}
	if err := c.pg.SetTaskPaused(ctx, task, true); err != nil {
		t.Fatal(err)
	}
	got, _ = c.pg.GetFixedFileJob(ctx, db.DefaultTenant, task, j.ID)
	if got.Status != "cancelled" {
		t.Fatal(got.Status)
	}
	if err := c.pg.SetTaskPaused(ctx, task, false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.RunOnce(ctx); ok {
		t.Fatal("resume resurrected cancellation")
	}
	zip := streamFile(t, c, task, "safe.zip", fixture(t, "safe.zip"))
	if code := c.do("POST", base+"/file-jobs", map[string]any{"file_id": zip.ID, "operation": "archive", "request_key": "archive"}, &j); code != 202 {
		t.Fatal(code)
	}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = c.pg.GetFixedFileJob(ctx, db.DefaultTenant, task, j.ID)
	if got.Status != "succeeded" {
		t.Fatal(got)
	}
	var children int
	c.pg.QueryRow(`SELECT count(*) FROM task_file_objects WHERE task_id=$1 AND parent_file_id=$2`, task, zip.ID).Scan(&children)
	if children == 0 {
		t.Fatal("archive children missing")
	}
	if _, err := c.pg.Exec(`UPDATE fixed_file_jobs SET input='{}' WHERE id=$1`, j.ID); err == nil {
		t.Fatal("immutable input changed")
	}
	// Crash recovery is bounded, and lease expiration never silently resets its budget.
	req["request_key"] = "exhaust"
	if code := c.do("POST", base+"/file-jobs", req, &j); code != 202 {
		t.Fatal(code)
	}
	for i := 0; i < 3; i++ {
		claimed, err := c.pg.ClaimFixedFileJob(ctx, "crashing", time.Minute, task)
		if err != nil || claimed == nil {
			t.Fatal(err)
		}
		c.pg.Exec(`UPDATE fixed_file_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, j.ID)
	}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = c.pg.GetFixedFileJob(ctx, db.DefaultTenant, task, j.ID)
	if got.Status != "failed" || got.Attempts != 3 {
		t.Fatalf("budget=%+v", got)
	}
}

func TestFixedMaterialsSerializeWithExport(t *testing.T) {
	c := newFileAPI(t)
	ctx := context.Background()
	p := dbtest.Ready(t, c.pg, dbtest.DefaultSpec(0))
	material, err := c.pg.SaveTaskMaterial(ctx, db.DefaultTenant, p.TaskID, db.TaskMaterial{Name: "尚未收到的附件", Required: false, Note: "尚未纳入"}, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.pg.CreateExportJobs(ctx, db.DefaultTenant, p.TaskID, "reviewer", db.CreateExportInput{Formats: []string{"docx"}, DefaultTemplateHash: "test"}); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	err = c.pg.Transaction(ctx, func(tx *db.DB) error {
		material.Required = true
		if _, err := tx.SaveTaskMaterial(ctx, db.DefaultTenant, p.TaskID, *material, "reviewer"); err != nil {
			return err
		}
		go func() {
			_, _, err := c.pg.CreateExportJobs(ctx, db.DefaultTenant, p.TaskID, "exporter", db.CreateExportInput{Formats: []string{"docx"}, DefaultTemplateHash: "test"})
			result <- err
		}()
		// The export cannot commit while the required-material mutation is uncommitted.
		select {
		case err := <-result:
			return fmt.Errorf("export escaped task lock: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		var blocked *db.ExportBlockedError
		if !errors.As(err, &blocked) || blocked.Preflight.Readiness.Counts["required_material_incomplete"] != 1 {
			t.Fatalf("concurrent export=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("export deadlocked")
	}
}

func TestFixedStreamingOversizeUpload(t *testing.T) {
	c := newFileAPI(t)
	task := c.createTask(t)
	// Exercise the actual streaming store limit without allocating a 64 MiB request.
	c.store.SetMaxBytes(16)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/tasks/%d/files?filename=large.txt", c.url, task), strings.NewReader(strings.Repeat("x", 32)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 413 {
		t.Fatal(res.StatusCode)
	}
	var count int
	c.pg.QueryRow(`SELECT count(*) FROM task_file_objects WHERE task_id=$1`, task).Scan(&count)
	if count != 0 {
		t.Fatal("oversize registered original")
	}
}
