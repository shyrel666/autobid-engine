package server

// F1：ZIP 招标包清单与受限展开。旧上传入口在解析前识别 .zip，先登记原件，再逐条登记子文件；
// 子文件不自动变成正式招标文档；用户在材料处理清单选择导入。

import (
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
)

var errFileProcessingDisabled = errors.New("招标包处理未启用：缺少原件存储")

// uploadArchive 处理 .zip 上传：保存原件与每个子文件，返回清单、哈希、格式与 file ID。
func (a *API) uploadArchive(w http.ResponseWriter, r *http.Request, t *Task, file *ingest.File, parent *db.TaskFileObject) {
	if a.cfg.FileStore == nil || parent == nil {
		fail(w, &httpError{status: http.StatusUnsupportedMediaType, err: errFileProcessingDisabled})
		return
	}
	childIDs := []int64{}
	arch, err := ingest.WalkArchive(file.Data, ingest.DefaultArchiveLimits(), func(e *ingest.ArchiveEntry, data []byte) error {
		status := "listed"
		if e.Format == "unsupported" {
			status = "unsupported"
		}
		obj, err := a.cfg.FileStore.PutBytes(r.Context(), filework.PutOptions{
			TenantID: db.DefaultTenant, TaskID: t.ID, Kind: db.FileKindDerived,
			OriginalName: path.Base(e.Path), MediaType: e.MediaType, ArchivePath: e.Path,
			Format: e.Format, ProcessStatus: status,
			ParentFileID: &parent.ID, CreatedBy: operator(r),
		}, data)
		if err != nil {
			return err
		}
		e.FileID = obj.ID
		childIDs = append(childIDs, obj.ID)
		return nil
	})
	if err != nil {
		writeJSON(w, errorStatus(err), map[string]any{"error": err.Error(), "original_file_id": parent.ID,
			"file_sha256": parent.SHA256, "retryable": true})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"result": map[string]any{
			"original_file_id": parent.ID, "archive": arch, "child_file_ids": childIDs,
			"note": "ZIP 已列清单并登记子文件；请在页面的“材料处理”中选择子文件导入。",
		},
		"planner_notified": false,
	})
}

func (a *API) fixedProcessor(w http.ResponseWriter) *filework.Processor {
	if a.cfg.FileStore == nil {
		fail(w, &httpError{status: http.StatusServiceUnavailable, err: errFileProcessingDisabled})
		return nil
	}
	return &filework.Processor{DB: a.pg, Store: a.cfg.FileStore, OCR: a.ocr()}
}

func (a *API) listProcessingFiles(w http.ResponseWriter, r *http.Request, t *Task) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	files, err := a.pg.ListTaskFileObjectsAfter(r.Context(), db.DefaultTenant, t.ID, "", max(0, after), 201)
	if err != nil {
		fail(w, err)
		return
	}
	if files == nil {
		files = []*db.TaskFileObject{}
	}
	hasMore := len(files) > 200
	if hasMore {
		files = files[:200]
	}
	writeJSON(w, 200, map[string]any{"enabled": a.cfg.FileStore != nil, "files": files, "has_more": hasMore})
}

func (a *API) importProcessingFile(w http.ResponseWriter, r *http.Request, t *Task) {
	p := a.fixedProcessor(w)
	if p == nil {
		return
	}
	id, err := pathID(r, "fileID")
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		RequestKey  string `json:"request_key"`
		TenderNo    string `json:"tender_no"`
		LotNo       string `json:"lot_no"`
		Title       string `json:"title"`
		DocRole     string `json:"doc_role"`
		PublishedAt string `json:"published_at"`
	}
	if err = decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(req.TenderNo) == "" {
		fail(w, badRequest("tender_no is required"))
		return
	}
	published, err := (uploadDocumentReq{PublishedAt: req.PublishedAt}).publishedAt()
	if err != nil {
		fail(w, err)
		return
	}
	record, err := p.Process(r.Context(), t.ID, filework.ProcessInput{FileID: id, RequestKey: req.RequestKey, TenderNo: strings.TrimSpace(req.TenderNo), LotNo: strings.TrimSpace(req.LotNo), Title: req.Title, DocRole: req.DocRole, PublishedAt: published}, operator(r))
	a.processingResponse(w, r, t, record, err)
}
func (a *API) reparseDocument(w http.ResponseWriter, r *http.Request, t *Task) {
	p := a.fixedProcessor(w)
	if p == nil {
		return
	}
	id, err := pathID(r, "docID")
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		RequestKey       string `json:"request_key"`
		ExpectedRevision *int   `json:"expected_revision"`
	}
	if err = decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.ExpectedRevision == nil || *req.ExpectedRevision < 0 {
		fail(w, badRequest("expected_revision is required"))
		return
	}
	record, err := p.Process(r.Context(), t.ID, filework.ProcessInput{DocumentID: id, RequestKey: req.RequestKey, ExpectedRevision: *req.ExpectedRevision}, operator(r))
	a.processingResponse(w, r, t, record, err)
}
func (a *API) processingResponse(w http.ResponseWriter, r *http.Request, t *Task, record *db.FileProcessingRecord, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	// The list/detail endpoint is the source of truth after a refresh or lost response.
	record.Preview = json.RawMessage(`{}`)
	a.e.emit(t, db.Activity{Worker: "file-processing", Kind: "state", Summary: "材料处理记录已更新"})
	if record.Status == "failed" {
		writeJSON(w, 422, map[string]any{"error": record.Error, "record": record})
		return
	}
	writeJSON(w, 200, record)
}
func (a *API) listProcessingRecords(w http.ResponseWriter, r *http.Request, t *Task) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	rows, err := a.pg.ListFileProcessingRecords(r.Context(), db.DefaultTenant, t.ID, before)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
func (a *API) getProcessingRecord(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "recordID")
	if err != nil {
		fail(w, err)
		return
	}
	record, err := a.pg.GetFileProcessingRecord(r.Context(), db.DefaultTenant, t.ID, id, false)
	if err != nil {
		fail(w, err)
		return
	}
	var preview db.ParsePreview
	if err = json.Unmarshal(record.Preview, &preview); err != nil {
		fail(w, err)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(0, min(offset, len(preview.Changes)))
	total := len(preview.Changes)
	end := min(offset+20, total)
	preview.Changes = preview.Changes[offset:end]
	for _, change := range preview.Changes {
		for _, c := range []*db.DocumentChunk{change.Before, change.After} {
			if c == nil {
				continue
			}
			c.Tables = nil
			text := []rune(c.Content)
			if len(text) > 12000 {
				c.Content = string(text[:12000]) + "\n（本段预览已截断）"
			}
		}
	}
	var payload ingest.PreparedTender
	_ = json.Unmarshal(record.Payload, &payload)
	record.Preview = json.RawMessage(`{}`)
	stale := false
	if record.TargetDocumentID != nil && record.Status == "ready" {
		doc, err := a.pg.GetTenderDocument(r.Context(), t.ID, *record.TargetDocumentID)
		stale = err != nil || doc.ParseRevision != record.ExpectedRevision
		if err == nil {
			source := payload.Document
			sameDate := doc.PublishedAt == nil && source.PublishedAt == nil || doc.PublishedAt != nil && source.PublishedAt != nil && doc.PublishedAt.Equal(*source.PublishedAt)
			stale = stale || doc.TenderNo != source.TenderNo || doc.LotNo != source.LotNo || doc.Title != source.Title || doc.DocRole != source.DocRole || !sameDate
		}
		obj, _, err := a.pg.GetTaskFileObject(r.Context(), db.DefaultTenant, t.ID, record.FileID)
		stale = stale || err != nil || obj.Availability != "available" || obj.SHA256 != record.SourceHash
	}
	writeJSON(w, 200, map[string]any{"record": record, "preview": preview, "total_changes": total, "offset": offset, "has_more": end < total, "stale": stale, "warnings": payload.Summary.Warnings})
}
func (a *API) adoptProcessingRecord(w http.ResponseWriter, r *http.Request, t *Task) {
	p := a.fixedProcessor(w)
	if p == nil {
		return
	}
	id, err := pathID(r, "recordID")
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		ExpectedRevision *int   `json:"expected_revision"`
		ReviewNote       string `json:"review_note"`
	}
	if err = decodeBody(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.ExpectedRevision == nil || *req.ExpectedRevision < 0 {
		fail(w, badRequest("expected_revision is required"))
		return
	}
	record, err := p.Adopt(r.Context(), t.ID, id, *req.ExpectedRevision, operator(r), req.ReviewNote)
	a.processingResponse(w, r, t, record, err)
}
