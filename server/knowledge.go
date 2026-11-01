package server

// Phase 4：多格式招标文档上传（multipart）与企业方案知识库 API。
//   POST   /api/tasks/{id}/documents         JSON {tender_no, lot_no, title, text, wake_planner} 或 multipart（file + 同名字段）
//   GET    /api/knowledge/documents          ?category=&tags=a,b&q=&limit=&offset=
//   POST   /api/knowledge/documents          JSON {category, title, tags, extra, text, file_name} 或 multipart（file + category/title/tags/extra）
//   GET    /api/knowledge/documents/{kid}    ?content=1 附带分块正文与表格
//   DELETE /api/knowledge/documents/{kid}
//   GET    /api/knowledge/search             ?q=&product=&category=a,b&tags=a,b&limit=&prefer_tables=1
// 知识库在 Phase 7 前固定使用默认租户；X-Autobid-User 请求头（可选）记录为 created_by。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
)

// maxUploadBytes 是 multipart 文件上传的请求体上限（64 MiB）。
const maxUploadBytes = 64 << 20

// operator 返回请求声明的操作人（Phase 7 前仅作审计记录，不做鉴权）。
func operator(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Autobid-User"))
}

func isMultipart(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "multipart/form-data"
}

// readMultipartFile 解析 multipart 表单并读取 file 字段；没有文件时返回 nil。
func readMultipartFile(w http.ResponseWriter, r *http.Request) (*ingest.File, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &httpError{http.StatusRequestEntityTooLarge, fmt.Errorf("upload exceeds %d MiB", maxUploadBytes>>20)}
		}
		return nil, badRequest("invalid multipart form: %v", err)
	}
	f, hdr, err := r.FormFile("file")
	if errors.Is(err, http.ErrMissingFile) {
		return nil, nil
	}
	if err != nil {
		return nil, badRequest("read file: %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, badRequest("read file: %v", err)
	}
	return &ingest.File{Name: filepath.Base(hdr.Filename), MIME: hdr.Header.Get("Content-Type"), Data: data}, nil
}

func formBool(v string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && b || strings.EqualFold(strings.TrimSpace(v), "on")
}

func (a *API) ocr() ingest.OCRProvider {
	if a.cfg.OCR != nil {
		return a.cfg.OCR
	}
	return ingest.UnavailableOCR{}
}

func (a *API) embedder() db.Embedder {
	if a.cfg.Embedder != nil {
		return a.cfg.Embedder
	}
	return db.DefaultEmbedder()
}

// retainOriginal 在解析前保存上传原件或文本快照；FileStore 未启用时返回 nil（保持旧行为）。
func (a *API) retainOriginal(r *http.Request, t *Task, file *ingest.File, text string) (*db.TaskFileObject, error) {
	if a.cfg.FileStore == nil {
		return nil, nil
	}
	opts := filework.PutOptions{TenantID: db.DefaultTenant, TaskID: t.ID, CreatedBy: operator(r)}
	if file != nil {
		opts.Kind, opts.OriginalName, opts.MediaType = db.FileKindOriginal, file.Name, file.MIME
		opts.Format = ingest.DetectFormat(file.Name, file.MIME, file.Data)
		if opts.Format == "" {
			opts.Format = "unsupported"
		}
		obj, err := a.cfg.FileStore.PutBytes(r.Context(), opts, file.Data)
		if err == nil {
			file.ObjectID, file.SHA256 = obj.ID, obj.SHA256
		}
		return obj, err
	}
	opts.Kind, opts.OriginalName, opts.MediaType = db.FileKindTextSnapshot, "text.txt", "text/plain"
	opts.Format, opts.ProcessStatus = ingest.FormatText, "parsed"
	return a.cfg.FileStore.PutBytes(r.Context(), opts, []byte(text))
}

// uploadDocument 解析、切分并入库招标文档；wake_planner 为 true 且产生了新文档时向 Planner 投递 document_added 触发。
func (a *API) uploadDocument(w http.ResponseWriter, r *http.Request, t *Task) {
	var req uploadDocumentReq
	var file *ingest.File
	if isMultipart(r) {
		var err error
		if file, err = readMultipartFile(w, r); err != nil {
			fail(w, err)
			return
		}
		req = uploadDocumentReq{TenderNo: r.FormValue("tender_no"), LotNo: r.FormValue("lot_no"), Title: r.FormValue("title"),
			Text: r.FormValue("text"), WakePlanner: formBool(r.FormValue("wake_planner")),
			DocRole: r.FormValue("doc_role"), PublishedAt: r.FormValue("published_at")}
		if file == nil && strings.TrimSpace(req.Text) == "" {
			fail(w, badRequest("multipart upload requires a file field (or a text field)"))
			return
		}
	} else if err := decodeBodyLimit(r, &req, maxDocumentBody); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(req.TenderNo) == "" || (file == nil && strings.TrimSpace(req.Text) == "") {
		fail(w, badRequest("tender_no and text (or file) are required"))
		return
	}
	published, err := req.publishedAt()
	if err != nil {
		fail(w, err)
		return
	}
	retained, err := a.retainOriginal(r, t, file, req.Text)
	if err != nil {
		fail(w, err)
		return
	}
	var originalID *int64
	if retained != nil {
		originalID = &retained.ID
	}
	if file != nil && ingest.DetectFormat(file.Name, file.MIME, file.Data) == ingest.FormatZIP {
		a.uploadArchive(w, r, t, file, retained)
		return
	}
	res, err := ingest.Ingest(r.Context(), a.pg, t.Assets, ingest.Input{TenderNo: req.TenderNo, LotNo: req.LotNo, Title: req.Title,
		Text: req.Text, File: file, OCR: a.ocr(), CreatedBy: operator(r), DocRole: strings.TrimSpace(req.DocRole),
		PublishedAt: published, TenantID: db.DefaultTenant, OriginalFileID: originalID})
	if err != nil {
		// 解析失败也保留原件：响应带上 file ID，用户可修正后重试，而不是重新上传。
		if retained != nil {
			writeJSON(w, errorStatus(err), map[string]any{"error": err.Error(), "original_file_id": retained.ID,
				"file_sha256": retained.SHA256, "retryable": true})
			return
		}
		fail(w, err)
		return
	}
	woke := false
	if req.WakePlanner && res.Created {
		detail := fmt.Sprintf("%d 个章节/条款，其中 %d 个强制性条款", res.Chunks, res.Mandatory)
		if res.Tables > 0 {
			detail += fmt.Sprintf("，%d 张结构化表格", res.Tables)
		}
		t.NotifyDocument(fmt.Sprintf("#%d %s（招标编号 %s）", res.Document.ID, res.Document.Title, res.Document.TenderNo), detail)
		woke = true
	}
	resp := map[string]any{"result": res, "planner_notified": woke}
	if formBool(r.URL.Query().Get("bootstrap_goals")) {
		if decomp, bErr := a.pg.BootstrapRFPGoals(r.Context(), db.DefaultTenant, t.ID, operator(r), true); bErr == nil {
			resp["goal_decomposition"] = decomp
		}
	}
	status := http.StatusCreated
	if !res.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}

// ---------------------------------------------------------------------------
// 知识库
// ---------------------------------------------------------------------------

type knowledgeReq struct {
	Category string          `json:"category"`
	Title    string          `json:"title"`
	Tags     []string        `json:"tags"`
	Extra    json.RawMessage `json:"extra"`
	Text     string          `json:"text"`
	FileName string          `json:"file_name"`
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '，' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (a *API) uploadKnowledge(w http.ResponseWriter, r *http.Request) {
	var (
		req  knowledgeReq
		file *ingest.File
	)
	if isMultipart(r) {
		var err error
		if file, err = readMultipartFile(w, r); err != nil {
			fail(w, err)
			return
		}
		req = knowledgeReq{Category: r.FormValue("category"), Title: r.FormValue("title"), Text: r.FormValue("text"),
			Extra: json.RawMessage(strings.TrimSpace(r.FormValue("extra")))}
		for _, t := range r.MultipartForm.Value["tags"] {
			req.Tags = append(req.Tags, splitList(t)...)
		}
	} else if err := decodeBodyLimit(r, &req, maxDocumentBody); err != nil {
		fail(w, err)
		return
	}
	if !db.ValidKnowledgeCategory(strings.TrimSpace(req.Category)) {
		fail(w, badRequest("category must be one of %s", strings.Join(db.KnowledgeCategories, ", ")))
		return
	}
	if file == nil && strings.TrimSpace(req.Text) == "" {
		fail(w, badRequest("text or file is required"))
		return
	}
	var extra map[string]any
	if s := strings.TrimSpace(string(req.Extra)); s != "" && s != "null" {
		if err := json.Unmarshal(req.Extra, &extra); err != nil {
			fail(w, badRequest("extra must be a JSON object"))
			return
		}
	}
	res, err := ingest.IngestKnowledge(r.Context(), a.pg, ingest.KnowledgeInput{Category: req.Category, Title: req.Title, Tags: req.Tags,
		Extra: extra, Text: req.Text, FileName: req.FileName, File: file, OCR: a.ocr(), CreatedBy: operator(r)}, a.embedder())
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusCreated
	if !res.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

func (a *API) listKnowledge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	category := q.Get("category")
	if category != "" && !db.ValidKnowledgeCategory(category) {
		fail(w, badRequest("unknown category %q", category))
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	docs, err := a.pg.ListKnowledgeDocuments(r.Context(), db.KnowledgeDocFilter{Category: category, Tags: splitList(q.Get("tags")),
		Query: q.Get("q"), Limit: limit, Offset: offset})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

func (a *API) getKnowledge(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "kid")
	if err != nil {
		fail(w, err)
		return
	}
	doc, err := a.pg.GetKnowledgeDocument(r.Context(), db.DefaultTenant, id, false)
	if err != nil {
		fail(w, err)
		return
	}
	chunks, err := a.pg.KnowledgeChunks(r.Context(), db.DefaultTenant, id, r.URL.Query().Get("content") == "1")
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": doc, "chunks": chunks})
}

func (a *API) deleteKnowledge(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "kid")
	if err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.DeleteKnowledgeDocument(r.Context(), db.DefaultTenant, id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// searchKnowledge 是知识库检索测试接口：q 为检索词，product 为必须出现的产品名（可选）。
func (a *API) searchKnowledge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	kq := db.KnowledgeQuery{Text: q.Get("q"), Categories: splitList(q.Get("category")), Tags: splitList(q.Get("tags")),
		Limit: limit, PreferTables: formBool(q.Get("prefer_tables"))}
	if p := strings.TrimSpace(q.Get("product")); p != "" {
		kq.MustTerms = []string{p}
	}
	if v := q.Get("doc_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			fail(w, badRequest("invalid doc_id %q", v))
			return
		}
		kq.DocID = id
	}
	result, err := a.cfg.Retrieval.Search(r.Context(), kq, 0, operator(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
