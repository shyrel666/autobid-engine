package server

// Phase 4 HTTP 测试：multipart 上传 .docx 招标文件、不支持格式 415、扫描件无 OCR 422，
// 以及企业方案知识库的上传（multipart / JSON）、去重、列表筛选、检索与删除。需要 AUTOBID_TEST_DATABASE_URL。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest/ingesttest"
)

func (c *apiClient) multipart(path string, fields map[string]string, fileName string, data []byte, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if fileName != "" {
		fw, _ := mw.CreateFormFile("file", fileName)
		_, _ = fw.Write(data)
	}
	_ = mw.Close()
	req, _ := http.NewRequest("POST", c.url+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Test-Actor", "http-tester")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return resp.StatusCode
}

func sampleDOCX() []byte {
	return ingesttest.BuildDOCX("园区安防招标文件",
		ingesttest.Para{Text: "第一章 投标人须知", Heading: 1},
		ingesttest.Para{Text: "★1.1 投标人须具备安防工程一级资质，否则投标无效。"},
		ingesttest.Para{Text: "第二章 技术要求", Heading: 1, RenderedBreak: true},
		ingesttest.Para{Text: "2.1 设备参数", Heading: 2},
		ingesttest.Table{Cols: 2, Rows: []ingesttest.Row{
			{Header: true, Cells: []ingesttest.Cell{{Text: "参数"}, {Text: "要求"}}},
			{Cells: []ingesttest.Cell{{Text: "★分辨率"}, {Text: "≥400万像素"}}},
			{RenderedBreak: true, Cells: []ingesttest.Cell{{Text: "存储"}, {Text: "≥30天"}}},
		}},
	)
}

func TestHTTPMultipartDocumentUpload(t *testing.T) {
	c, e := newIdleAPI(t)
	tender := fmt.Sprintf("ZB-MP-%d", time.Now().UnixNano())
	var created struct {
		ID int64 `json:"id"`
	}
	if code := c.do("POST", "/api/tasks", map[string]any{"name": "multipart", "goal": "解析 DOCX", "scopes": []map[string]string{{"tender_no": tender}}}, &created); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	base := fmt.Sprintf("/api/tasks/%d/documents", created.ID)
	var up struct {
		Result struct {
			Document  db.TenderDocument `json:"document"`
			Created   bool              `json:"created"`
			Chunks    int               `json:"chunks"`
			Mandatory int               `json:"mandatory_chunks"`
			Tables    int               `json:"tables"`
			PageCount int               `json:"page_count"`
			Format    string            `json:"format"`
		} `json:"result"`
		PlannerNotified bool `json:"planner_notified"`
	}
	fields := map[string]string{"tender_no": tender, "lot_no": "A", "wake_planner": "true"}
	if code := c.multipart(base, fields, "招标文件.docx", sampleDOCX(), &up); code != 201 || !up.Result.Created || up.Result.Format != "docx" ||
		up.Result.Tables != 1 || up.Result.PageCount != 3 || up.Result.Mandatory != 2 || !up.PlannerNotified ||
		up.Result.Document.FileName != "招标文件.docx" || up.Result.Document.Title != "园区安防招标文件" || up.Result.Document.CreatedBy != "http-tester" {
		t.Fatalf("multipart upload: %d %+v", code, up)
	}
	if tr := e.m.Task(created.ID).drainTriggers(); len(tr) != 1 || !strings.Contains(tr[0].Detail, "1 张结构化表格") {
		t.Fatalf("planner trigger: %+v", tr)
	}
	if code := c.multipart(base, fields, "招标文件.docx", sampleDOCX(), &up); code != 200 || up.Result.Created {
		t.Fatalf("re-upload must be idempotent: %d", code)
	}
	if code := c.multipart(base, map[string]string{"tender_no": tender}, "old.doc", []byte{0xD0, 0xCF, 0x11, 0xE0}, nil); code != http.StatusUnsupportedMediaType {
		t.Fatalf(".doc: %d", code)
	}
	if code := c.multipart(base, map[string]string{"tender_no": tender}, "scan.png", []byte("\x89PNG\r\n\x1a\nxxxx"), nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("scan without OCR: %d", code)
	}
	if code := c.multipart(base, map[string]string{"lot_no": "A"}, "a.docx", sampleDOCX(), nil); code != 400 {
		t.Fatalf("missing tender_no: %d", code)
	}
	if code := c.multipart(base, map[string]string{"tender_no": tender}, "", nil, nil); code != 400 {
		t.Fatalf("missing file: %d", code)
	}

	var docs []struct {
		db.TenderDocument
		Chunks []db.DocumentChunk `json:"chunks"`
	}
	if code := c.do("GET", base+"?content=1", nil, &docs); code != 200 || len(docs) != 1 {
		t.Fatalf("list: %d %+v", code, docs)
	}
	var spec *db.DocumentChunk
	for i := range docs[0].Chunks {
		if docs[0].Chunks[i].SectionCode == "2.1" {
			spec = &docs[0].Chunks[i]
		}
	}
	if spec == nil || spec.PageStart != 2 || spec.PageEnd != 3 || spec.TableCount != 1 || !strings.Contains(string(spec.Tables), `"page_end":3`) {
		t.Fatalf("chunk with table: %+v", spec)
	}
	if docs[0].PageCount != 3 || len(docs[0].ParseMeta) == 0 {
		t.Fatalf("document parse metadata: %+v", docs[0].TenderDocument)
	}
}

func TestHTTPKnowledgeBase(t *testing.T) {
	c, _ := newIdleAPI(t)
	tag := fmt.Sprintf("kbtag%d", time.Now().UnixNano())
	var up struct {
		Document db.KnowledgeDocument `json:"document"`
		Created  bool                 `json:"created"`
		Chunks   int                  `json:"chunks"`
		Tables   int                  `json:"tables"`
	}
	docx := sampleDOCX()
	if code := c.multipart("/api/knowledge/documents", map[string]string{"category": "historical_bid", "tags": tag + ",安防", "extra": `{"year":2025}`},
		"history.docx", docx, &up); code != 201 || !up.Created || up.Tables != 1 || up.Document.Category != "historical_bid" ||
		len(up.Document.Tags) != 2 || up.Document.CreatedBy != "http-tester" || up.Document.FileName != "history.docx" {
		t.Fatalf("multipart knowledge upload: %d %+v", code, up)
	}
	docID := up.Document.ID
	t.Cleanup(func() { c.do("DELETE", fmt.Sprintf("/api/knowledge/documents/%d", docID), nil, nil) })
	if code := c.multipart("/api/knowledge/documents", map[string]string{"category": "historical_bid"}, "copy.docx", docx, &up); code != 200 || up.Created || up.Document.ID != docID {
		t.Fatalf("dedupe: %d %+v", code, up)
	}
	text := fmt.Sprintf("# 统一视频平台白皮书\n## 1 平台能力\n支持%s级联与GB28181接入。", tag)
	if code := c.do("POST", "/api/knowledge/documents", map[string]any{"category": "whitepaper", "title": "统一视频平台白皮书", "tags": []string{tag},
		"text": text, "file_name": "wp.md"}, &up); code != 201 || up.Document.Title != "统一视频平台白皮书" {
		t.Fatalf("json knowledge upload: %d %+v", code, up)
	}
	wpID := up.Document.ID
	t.Cleanup(func() { c.do("DELETE", fmt.Sprintf("/api/knowledge/documents/%d", wpID), nil, nil) })
	if code := c.do("POST", "/api/knowledge/documents", map[string]any{"category": "bogus", "text": "x"}, nil); code != 400 {
		t.Fatalf("invalid category: %d", code)
	}
	if code := c.do("POST", "/api/knowledge/documents", map[string]any{"category": "whitepaper"}, nil); code != 400 {
		t.Fatalf("missing text: %d", code)
	}

	var list []db.KnowledgeDocument
	if code := c.do("GET", "/api/knowledge/documents?tags="+url.QueryEscape(tag), nil, &list); code != 200 || len(list) != 2 {
		t.Fatalf("list by tag: %d %+v", code, list)
	}
	if code := c.do("GET", "/api/knowledge/documents?category=whitepaper&tags="+url.QueryEscape(tag), nil, &list); code != 200 || len(list) != 1 || list[0].ID != wpID {
		t.Fatalf("list by category: %d %+v", code, list)
	}
	var detail struct {
		Document db.KnowledgeDocument `json:"document"`
		Chunks   []db.KnowledgeChunk  `json:"chunks"`
	}
	if code := c.do("GET", fmt.Sprintf("/api/knowledge/documents/%d?content=1", docID), nil, &detail); code != 200 || len(detail.Chunks) == 0 || detail.Document.TableCount != 1 ||
		detail.Chunks[len(detail.Chunks)-1].SectionPath != "第二章 技术要求 / 2.1 设备参数" {
		t.Fatalf("detail: %d %+v", code, detail)
	}

	var res struct {
		Total int               `json:"total"`
		Hits  []db.KnowledgeHit `json:"hits"`
	}
	if code := c.do("GET", "/api/knowledge/search?q="+url.QueryEscape("分辨率 存储")+"&tags="+url.QueryEscape(tag), nil, &res); code != 200 || res.Total == 0 {
		t.Fatalf("search: %d %+v", code, res)
	}
	top := res.Hits[0].Citation
	if top.DocID != docID || top.SectionCode != "2.1" || top.PageStart != 2 || top.PageEnd != 3 || top.DocTitle != "园区安防招标文件" || top.Category != "historical_bid" {
		t.Fatalf("search citation: %+v", top)
	}
	if code := c.do("GET", "/api/knowledge/search?q=GB28181&category=whitepaper&tags="+url.QueryEscape(tag), nil, &res); code != 200 || res.Total != 1 || res.Hits[0].Citation.DocID != wpID {
		t.Fatalf("category search: %d %+v", code, res)
	}
	if code := c.do("GET", "/api/knowledge/search?q=", nil, nil); code != 400 {
		t.Fatalf("empty query: %d", code)
	}
	if code := c.do("GET", "/api/knowledge/search?q=x&category=bogus", nil, nil); code != 400 {
		t.Fatalf("bad category: %d", code)
	}
	if code := c.do("DELETE", fmt.Sprintf("/api/knowledge/documents/%d", wpID), nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code := c.do("DELETE", fmt.Sprintf("/api/knowledge/documents/%d", wpID), nil, nil); code != 404 {
		t.Fatalf("delete again: %d", code)
	}
	if code := c.do("GET", fmt.Sprintf("/api/knowledge/documents/%d", wpID), nil, nil); code != 404 {
		t.Fatalf("get deleted: %d", code)
	}
}
