package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
)

func TestFixedFileImportAndRevisionWorkflow(t *testing.T) {
	c := newFileAPI(t)
	ctx := context.Background()
	task := c.createTask(t)
	status, raw := c.upload(t, task, "file", "safe.zip", fixture(t, "safe.zip"), map[string]string{"tender_no": "F3-ZIP"})
	if status != 202 {
		t.Fatalf("upload: %d %s", status, raw)
	}
	var files struct {
		Enabled bool                 `json:"enabled"`
		Files   []*db.TaskFileObject `json:"files"`
	}
	if code := c.do("GET", fmt.Sprintf("/api/tasks/%d/files", task), nil, &files); code != 200 || !files.Enabled {
		t.Fatal(code)
	}
	var child *db.TaskFileObject
	for _, f := range files.Files {
		if f.Format == ingest.FormatCSV {
			child = f
			break
		}
	}
	if child == nil {
		t.Fatal("missing CSV child")
	}
	path := fmt.Sprintf("/api/tasks/%d/files/%d/import", task, child.ID)
	req := map[string]any{"request_key": "import-one", "tender_no": fmt.Sprintf("F3-%d", task), "lot_no": "L1", "doc_role": "clarification", "published_at": "2026-10-31"}
	var record db.FileProcessingRecord
	if code := c.do("POST", path, req, &record); code != 200 {
		t.Fatalf("import=%d", code)
	}
	var result ingest.Result
	if err := json.Unmarshal(record.Result, &result); err != nil {
		t.Fatal(err)
	}
	doc := result.Document
	if doc == nil || doc.DocRole != "clarification" || doc.LotNo != "L1" || doc.PublishedAt == nil || doc.OriginalFileID == nil || *doc.OriginalFileID != child.ID {
		t.Fatalf("provenance/role: %+v", doc)
	}
	var retry db.FileProcessingRecord
	if code := c.do("POST", path, req, &retry); code != 200 || retry.ID != record.ID {
		t.Fatalf("retry=%d %+v", code, retry)
	}
	req["title"] = "different input"
	if code := c.do("POST", path, req, nil); code != 409 {
		t.Fatalf("key conflict=%d", code)
	}
	delete(req, "title")
	req["request_key"] = "import-again"
	if code := c.do("POST", path, req, &retry); code != 200 {
		t.Fatal(code)
	}
	var count int
	if err := c.pg.QueryRowContext(ctx, `SELECT count(*) FROM tender_documents WHERE task_id=$1`, task).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicates=%d %v", count, err)
	}
	other := c.createTask(t)
	if code := c.do("POST", fmt.Sprintf("/api/tasks/%d/files/%d/import", other, child.ID), req, nil); code != 404 {
		t.Fatalf("cross task import=%d", code)
	}
	// Simulate an older parser with different extracted text. Retained bytes stay intact.
	if _, err := c.pg.ExecContext(ctx, `UPDATE document_chunks SET content='旧解析遗漏金额' WHERE doc_id=$1`, doc.ID); err != nil {
		t.Fatal(err)
	}
	reparse := fmt.Sprintf("/api/tasks/%d/documents/%d/reparse", task, doc.ID)
	reqParse := map[string]any{"request_key": "parse-a", "expected_revision": 0}
	var candidate, competing db.FileProcessingRecord
	if code := c.do("POST", reparse, reqParse, &candidate); code != 200 {
		t.Fatalf("reparse=%d", code)
	}
	reqParse["request_key"] = "parse-b"
	if code := c.do("POST", reparse, reqParse, &competing); code != 200 {
		t.Fatalf("second candidate=%d", code)
	}
	var preview struct {
		Preview db.ParsePreview `json:"preview"`
		Stale   bool            `json:"stale"`
	}
	detail := fmt.Sprintf("/api/tasks/%d/file-processing-records/%d", task, candidate.ID)
	if code := c.do("GET", detail, nil, &preview); code != 200 || preview.Preview.Updated == 0 {
		t.Fatalf("preview=%d %+v", code, preview)
	}
	if code := c.do("GET", fmt.Sprintf("/api/tasks/%d/file-processing-records/%d", other, candidate.ID), nil, nil); code != 404 {
		t.Fatalf("cross task detail=%d", code)
	}
	var rev int
	c.pg.QueryRowContext(ctx, `SELECT parse_revision FROM tender_documents WHERE id=$1`, doc.ID).Scan(&rev)
	if rev != 0 {
		t.Fatal("preview changed formal document")
	}
	adopted := map[string]any{"expected_revision": 0, "review_note": "核对 CSV 原件"}
	if code := c.do("POST", detail+"/adopt", adopted, &record); code != 200 || record.Status != "adopted" {
		t.Fatalf("adopt=%d %+v", code, record)
	}
	if code := c.do("POST", detail+"/adopt", adopted, &retry); code != 200 || retry.ID != record.ID {
		t.Fatalf("repeat adopt=%d", code)
	}
	stale := fmt.Sprintf("/api/tasks/%d/file-processing-records/%d", task, competing.ID)
	if code := c.do("GET", stale, nil, &preview); code != 200 || !preview.Stale {
		t.Fatalf("stale=%d %+v", code, preview)
	}
	if code := c.do("POST", stale+"/adopt", adopted, nil); code != 409 {
		t.Fatalf("stale adopt=%d", code)
	}
	c.pg.QueryRowContext(ctx, `SELECT count(*) FROM tender_parse_revisions WHERE document_id=$1`, doc.ID).Scan(&count)
	if count != 1 {
		t.Fatalf("snapshot count=%d", count)
	}
	ready, err := c.pg.ExportReadiness(ctx, db.DefaultTenant, task)
	if err != nil || ready.Counts[db.BlockSourcePending] == 0 {
		t.Fatalf("source gate=%+v %v", ready, err)
	}
	if _, err := c.pg.ConfirmRequirements(ctx, db.DefaultTenant, task, "test", 0); err == nil {
		t.Fatal("confirmed unextracted source")
	}
}

func TestFixedProcessingFailuresAndAtomicity(t *testing.T) {
	c := newFileAPIWithDB(t, isolatedModelDB(t))
	ctx := context.Background()
	task := c.createTask(t)
	put := func(name string, data []byte) *db.TaskFileObject {
		o, err := c.store.PutBytes(ctx, filework.PutOptions{TaskID: task, Kind: db.FileKindOriginal, OriginalName: name}, data)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	bad := put("bad.docx", []byte("invalid"))
	path := fmt.Sprintf("/api/tasks/%d/files/%d/import", task, bad.ID)
	req := map[string]any{"request_key": "bad", "tender_no": "F3-BAD"}
	if code := c.do("POST", path, req, nil); code != 422 {
		t.Fatalf("bad parse=%d", code)
	}
	if code := c.do("POST", path, req, nil); code != 422 {
		t.Fatal(code)
	}
	var records []*db.FileProcessingRecord
	c.do("GET", fmt.Sprintf("/api/tasks/%d/file-processing-records", task), nil, &records)
	if len(records) != 1 || records[0].Status != "failed" {
		t.Fatalf("failure persistence: %+v", records)
	}
	// Failure after assets/chunks were written must roll back all business rows.
	good := put("valid.csv", []byte("名称,金额\n设备,123.45\n"))
	derived, err := c.store.PutBytes(ctx, filework.PutOptions{TaskID: task, Kind: db.FileKindDerived, OriginalName: "output.csv"}, []byte("名称,金额\n派生结果,1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if code := c.do("POST", fmt.Sprintf("/api/tasks/%d/files/%d/import", task, derived.ID), map[string]any{"request_key": "unreviewed-output", "tender_no": "F3-OUTPUT"}, nil); code != 422 {
		t.Fatalf("unreviewed output import=%d", code)
	}
	if _, err := c.pg.ExecContext(ctx, `CREATE FUNCTION fail_processing_commit() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected final status failure'; END; $$ LANGUAGE plpgsql;
 CREATE TRIGGER fail_processing_commit BEFORE UPDATE OF status ON file_processing_records FOR EACH ROW EXECUTE FUNCTION fail_processing_commit()`); err != nil {
		t.Fatal(err)
	}
	req = map[string]any{"request_key": "atomic", "tender_no": "F3-ATOMIC", "lot_no": "L1"}
	path = fmt.Sprintf("/api/tasks/%d/files/%d/import", task, good.ID)
	if code := c.do("POST", path, req, nil); code != 500 {
		t.Fatalf("injection=%d", code)
	}
	for _, query := range []string{`SELECT count(*) FROM tender_documents`, `SELECT count(*) FROM document_chunks`, `SELECT count(*) FROM assets`, `SELECT count(*) FROM task_asset_links`, `SELECT count(*) FROM task_scopes`, `SELECT count(*) FROM requirement_manifests`, `SELECT count(*) FROM file_processing_records WHERE request_key='atomic'`} {
		var n int
		if err := c.pg.QueryRowContext(ctx, query).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial commit %s = %d %v", query, n, err)
		}
	}
	if _, err := c.pg.ExecContext(ctx, `DROP TRIGGER fail_processing_commit ON file_processing_records`); err != nil {
		t.Fatal(err)
	}
	var r db.FileProcessingRecord
	if code := c.do("POST", path, req, &r); code != 200 {
		t.Fatalf("retry=%d", code)
	}
	var result ingest.Result
	json.Unmarshal(r.Result, &result)
	p := &filework.Processor{DB: c.pg, Store: c.store}
	// Concurrent same-key calls share one record and one document.
	in := filework.ProcessInput{FileID: good.ID, TenderNo: "F3-ATOMIC", LotNo: "L1", RequestKey: "concurrent"}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	ids := make(chan int64, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := p.Process(ctx, task, in, "test")
			if err != nil {
				errs <- err
			} else {
				ids <- r.ID
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatal(err)
	}
	var first int64
	for id := range ids {
		if first == 0 {
			first = id
		}
		if id != first {
			t.Fatal("duplicate request records")
		}
	}
	// Reparse then inject failure after revision writes; snapshot and revision roll back.
	candidate, err := p.Process(ctx, task, filework.ProcessInput{DocumentID: result.Document.ID, RequestKey: "reparse-atomic"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	c.pg.ExecContext(ctx, `CREATE TRIGGER fail_processing_commit BEFORE UPDATE OF status ON file_processing_records FOR EACH ROW EXECUTE FUNCTION fail_processing_commit()`)
	if _, err = p.Adopt(ctx, task, candidate.ID, 0, "test", ""); err == nil {
		t.Fatal("expected adoption failure")
	}
	var count, rev int
	c.pg.QueryRowContext(ctx, `SELECT count(*) FROM tender_parse_revisions`).Scan(&count)
	c.pg.QueryRowContext(ctx, `SELECT parse_revision FROM tender_documents WHERE id=$1`, result.Document.ID).Scan(&rev)
	if count != 0 || rev != 0 {
		t.Fatalf("partial adoption snapshots=%d rev=%d", count, rev)
	}
	c.pg.ExecContext(ctx, `DROP TRIGGER fail_processing_commit ON file_processing_records`)
	// Changing retained bytes invalidates a saved candidate at adoption.
	if err = os.WriteFile(filepath.Join(c.store.Root(), good.RelativePath), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Adopt(ctx, task, candidate.ID, 0, "test", ""); err == nil {
		t.Fatal("adopted tampered original")
	}
}
