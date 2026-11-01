package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
)

type fileAPI struct {
	*apiClient
	pg    *db.DB
	store *filework.Store
}

func newFileAPI(t *testing.T) *fileAPI {
	t.Helper()
	pg := testDB(t)
	return newFileAPIWithDB(t, pg)
}

func newFileAPIWithDB(t *testing.T, pg *db.DB) *fileAPI {
	t.Helper()
	store, err := filework.NewStore(pg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(pg)
	e := NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil },
		Config{Workers: 1, Debounce: 20 * time.Millisecond, WorkerIdlePoll: 20 * time.Millisecond})
	web, _ := filepath.Abs("../web/dist")
	api := NewAPI(ctx, m, e, NewLLMService(pg, LLMServiceConfig{}), nil, NewHub(), APIConfig{Token: testToken, FileStore: store, WebDir: web})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		e.StopAll()
		cancel()
	})
	return &fileAPI{apiClient: &apiClient{t: t, url: srv.URL}, pg: pg, store: store}
}

func (c *fileAPI) createTask(t *testing.T) int64 {
	t.Helper()
	var created struct {
		ID int64 `json:"id"`
	}
	if code := c.do("POST", "/api/tasks", map[string]any{"name": fmt.Sprintf("files-%d", time.Now().UnixNano()), "goal": "处理招标包"}, &created); code != http.StatusCreated {
		t.Fatalf("create task: %d", code)
	}
	return created.ID
}

func (c *fileAPI) upload(t *testing.T, taskID int64, field, name string, data []byte, form map[string]string) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range form {
		_ = mw.WriteField(k, v)
	}
	part, err := mw.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/tasks/%d/documents", c.url, taskID), &body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "eval", "file-processing", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestUploadOriginalRetentionXLSXAndText(t *testing.T) {
	c := newFileAPI(t)
	ctx := context.Background()
	taskID := c.createTask(t)

	xlsx := fixture(t, "clean.xlsx")
	status, raw := c.upload(t, taskID, "file", "clean.xlsx", xlsx, map[string]string{"tender_no": fmt.Sprintf("ZIP-XLSX-%d", time.Now().UnixNano())})
	if status != http.StatusCreated {
		t.Fatalf("xlsx upload: %d %s", status, raw)
	}
	var up struct {
		Result ingest.Result `json:"result"`
	}
	if err := json.Unmarshal(raw, &up); err != nil {
		t.Fatal(err)
	}
	if up.Result.Document == nil || up.Result.Document.OriginalFileID == nil || up.Result.PageCount != 0 {
		t.Fatalf("xlsx result = %+v", up.Result)
	}
	got, obj, err := c.store.Read(ctx, db.DefaultTenant, taskID, *up.Result.Document.OriginalFileID)
	if err != nil || !bytes.Equal(got, xlsx) || obj.Kind != db.FileKindOriginal || obj.SHA256 == "" {
		t.Fatalf("read original: %+v %v", obj, err)
	}
	var pageCount *int
	if err := c.pg.QueryRowContext(ctx, `SELECT page_count FROM tender_documents WHERE id=$1`, up.Result.Document.ID).Scan(&pageCount); err != nil {
		t.Fatal(err)
	}
	if pageCount != nil {
		t.Fatalf("page_count = %v, want NULL", *pageCount)
	}

	// 解析失败仍保留原件：伪 DOCX 返回 415 + original_file_id。
	status, raw = c.upload(t, taskID, "file", "not_docx.docx", fixture(t, "not_docx.docx"), map[string]string{"tender_no": fmt.Sprintf("BAD-%d", time.Now().UnixNano())})
	if status != http.StatusUnsupportedMediaType {
		t.Fatalf("bad docx upload: %d %s", status, raw)
	}
	var failure struct {
		Error          string `json:"error"`
		OriginalFileID int64  `json:"original_file_id"`
	}
	if err := json.Unmarshal(raw, &failure); err != nil || failure.OriginalFileID == 0 || !strings.Contains(failure.Error, "docx_missing_document_xml") {
		t.Fatalf("bad docx failure = %s (%v)", raw, err)
	}
	if _, obj, err := c.store.Read(ctx, db.DefaultTenant, taskID, failure.OriginalFileID); err != nil || obj.Kind != db.FileKindOriginal {
		t.Fatalf("failed parse original lost: %+v %v", obj, err)
	}

	// 文本上传登记 text_snapshot 并关联到文档。
	text := "第一章 投标人须知\n1.1 投标人须提供有效营业执照。"
	var textOut struct {
		Result ingest.Result `json:"result"`
	}
	if code := c.do("POST", fmt.Sprintf("/api/tasks/%d/documents", taskID), map[string]any{
		"tender_no": fmt.Sprintf("TXT-%d", time.Now().UnixNano()), "title": "文本招标文件", "text": text,
	}, &textOut); code != http.StatusCreated {
		t.Fatalf("text upload: %d", code)
	}
	if textOut.Result.Document.OriginalFileID == nil {
		t.Fatalf("text snapshot not linked: %+v", textOut.Result.Document)
	}
	got, obj, err = c.store.Read(ctx, db.DefaultTenant, taskID, *textOut.Result.Document.OriginalFileID)
	if err != nil || string(got) != text || obj.Kind != db.FileKindTextSnapshot {
		t.Fatalf("text snapshot = %+v %v", obj, err)
	}
}

func TestUploadZIPArchiveListingAndOriginal(t *testing.T) {
	c := newFileAPI(t)
	ctx := context.Background()
	taskID := c.createTask(t)
	zipData := fixture(t, "safe.zip")
	status, raw := c.upload(t, taskID, "file", "safe.zip", zipData, map[string]string{
		"tender_no": fmt.Sprintf("ZIP-%d", time.Now().UnixNano()), "title": "招标包",
	})
	if status != http.StatusAccepted {
		t.Fatalf("zip upload: %d %s", status, raw)
	}
	var out struct {
		Result struct {
			OriginalFileID int64          `json:"original_file_id"`
			Archive        ingest.Archive `json:"archive"`
			ChildFileIDs   []int64        `json:"child_file_ids"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Result.OriginalFileID == 0 || len(out.Result.Archive.Entries) != 4 || len(out.Result.ChildFileIDs) != 4 {
		t.Fatalf("zip result = %+v", out.Result)
	}
	for _, e := range out.Result.Archive.Entries {
		if e.FileID == 0 || e.Path == "" || e.SHA256 == "" || e.Format == "" {
			t.Fatalf("entry not registered: %+v", e)
		}
	}
	objs, err := c.pg.ListTaskFileObjects(ctx, db.DefaultTenant, taskID, "", 100)
	if err != nil || len(objs) != 5 {
		t.Fatalf("file objects = %d %v", len(objs), err)
	}
	original, children := 0, 0
	for _, o := range objs {
		switch o.Kind {
		case db.FileKindOriginal:
			original++
		case db.FileKindDerived:
			children++
			if o.ParentFileID == nil || *o.ParentFileID != out.Result.OriginalFileID || o.ArchivePath == "" ||
				o.Format == "" || o.ProcessStatus == "" {
				t.Fatalf("child provenance = %+v", o)
			}
		}
	}
	if original != 1 || children != 4 {
		t.Fatalf("kinds original=%d children=%d", original, children)
	}
	got, _, err := c.store.Read(ctx, db.DefaultTenant, taskID, out.Result.OriginalFileID)
	if err != nil || !bytes.Equal(got, zipData) {
		t.Fatalf("zip original read: %v", err)
	}

	// 恶意 ZIP：明确 422 + archive 错误码，原件仍保留。
	status, raw = c.upload(t, taskID, "file", "zip-traversal.zip", fixture(t, "zip-traversal.zip"), map[string]string{
		"tender_no": fmt.Sprintf("ZIPBAD-%d", time.Now().UnixNano()),
	})
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), ingest.ArchivePathTraversal) {
		t.Fatalf("bad zip = %d %s", status, raw)
	}
	var failure struct {
		OriginalFileID int64 `json:"original_file_id"`
	}
	if err := json.Unmarshal(raw, &failure); err != nil || failure.OriginalFileID == 0 {
		t.Fatalf("bad zip failure = %s (%v)", raw, err)
	}
	if _, _, err := c.store.Read(ctx, db.DefaultTenant, taskID, failure.OriginalFileID); err != nil {
		t.Fatalf("bad zip original lost: %v", err)
	}
}
