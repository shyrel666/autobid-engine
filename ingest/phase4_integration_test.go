package ingest

// Phase 4 集成测试：DOCX/PDF 样本经解析入库后的章节、页码范围、表格 JSON 溯源，以及知识库入库去重与混合检索引用。
// 需要 AUTOBID_TEST_DATABASE_URL，未设置时跳过。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

func TestIngestDOCXFileStoresPagesTablesAndProvenance(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("docx-%d", time.Now().UnixNano()), Goal: "解析 DOCX 招标文件"})
	if err != nil {
		t.Fatal(err)
	}
	as := db.NewAssetStore(d, task.ID)
	tenderNo := fmt.Sprintf("ZB-DOCX-%d", time.Now().UnixNano())
	file := &File{Name: "招标文件.docx", Data: sampleTenderDOCX()}
	res, err := Ingest(ctx, d, as, Input{TenderNo: tenderNo, File: file, CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Chunks != 10 || res.Mandatory != 2 || res.Tables != 1 || res.PageCount != 4 || res.Format != FormatDOCX {
		t.Fatalf("ingest: %+v", res)
	}
	doc := res.Document
	var meta ParseMeta
	_ = json.Unmarshal(doc.ParseMeta, &meta)
	if doc.Title != "智慧园区安防系统采购项目招标文件" || doc.FileName != "招标文件.docx" || doc.PageCount != 4 || doc.TenantID != db.DefaultTenant ||
		doc.CreatedBy != "tester" || !strings.Contains(doc.MimeType, "wordprocessingml") || meta.Tables != 1 || meta.Format != FormatDOCX {
		t.Fatalf("document: %+v meta=%s", doc, doc.ParseMeta)
	}
	again, err := Ingest(ctx, d, as, Input{TenderNo: tenderNo, File: file})
	if err != nil || again.Created || again.Document.ID != doc.ID || again.Tables != 1 || again.Mandatory != 2 {
		t.Fatalf("re-upload must be idempotent: %v %+v", err, again)
	}

	chunks, err := d.TaskChunks(ctx, task.ID, doc.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var spec *db.DocumentChunk
	for _, c := range chunks {
		if c.SectionCode == "2.1" {
			spec = c
		}
	}
	if spec == nil || spec.PageStart != 2 || spec.PageEnd != 3 || spec.PageNo != 2 || spec.TableCount != 1 || !spec.IsMandatory {
		t.Fatalf("2.1 chunk: %+v", spec)
	}
	var tables []Table
	if err := json.Unmarshal(spec.Tables, &tables); err != nil || len(tables) != 1 || tables[0].Rows != 5 || tables[0].Cols != 4 ||
		tables[0].PageStart != 2 || tables[0].PageEnd != 3 || tables[0].Grid[2][1] != "摄像机" {
		t.Fatalf("tables_json: %v %s", err, spec.Tables)
	}
	var bbox struct {
		Source     string `json:"source"`
		BlockStart int    `json:"block_start"`
		BlockEnd   int    `json:"block_end"`
	}
	if err := json.Unmarshal(spec.BBoxMeta, &bbox); err != nil || bbox.Source != FormatDOCX || bbox.BlockEnd <= bbox.BlockStart {
		t.Fatalf("bbox_meta: %v %s", err, spec.BBoxMeta)
	}
	outline, _ := d.DocumentOutline(ctx, task.ID, doc.ID, 100)
	if len(outline) != 10 || outline[6].TableCount != 1 || outline[6].PageEnd != 3 || outline[6].Tables != nil {
		t.Fatalf("outline must carry page range/table count but not bodies: %+v", outline[6])
	}
}

func TestIngestKnowledgeDedupAndHybridSearch(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	emb := db.DefaultEmbedder()
	tag := fmt.Sprintf("园区-%d", time.Now().UnixNano())

	// 同一份 PDF 作为历史标书入库两次：第二次幂等返回已有文档。
	pdf := &File{Name: "historical-bid.pdf", Data: sampleTenderPDF()}
	first, err := IngestKnowledge(ctx, d, KnowledgeInput{Category: db.KnowledgeHistoricalBid, Tags: []string{tag, "安防"}, File: pdf, CreatedBy: "kb-admin"}, emb)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Format != FormatPDF || first.Tables != 1 || first.PageCount != 3 || first.Document.Title != "智慧园区安防系统采购项目招标文件" {
		t.Fatalf("first: %+v doc=%+v", first, first.Document)
	}
	second, err := IngestKnowledge(ctx, d, KnowledgeInput{Category: db.KnowledgeHistoricalBid, Tags: []string{"其他"}, File: pdf}, emb)
	if err != nil || second.Created || second.Document.ID != first.Document.ID {
		t.Fatalf("dedupe: %v %+v", err, second)
	}
	t.Cleanup(func() { _ = d.DeleteKnowledgeDocument(ctx, "", first.Document.ID) })

	// DOCX 作为产品规格书，extra 记录产品名。
	spec, err := IngestKnowledge(ctx, d, KnowledgeInput{Category: db.KnowledgeProductSpec, Title: "安防设备选型规格书", Tags: []string{tag},
		Extra: map[string]any{"product": fmt.Sprintf("XG-%d", time.Now().UnixNano())}, File: &File{Name: "spec.docx", Data: sampleTenderDOCX()}}, emb)
	if err != nil || !spec.Created {
		t.Fatalf("spec: %v %+v", err, spec)
	}
	t.Cleanup(func() { _ = d.DeleteKnowledgeDocument(ctx, "", spec.Document.ID) })

	hits, err := d.SearchKnowledge(ctx, db.KnowledgeQuery{Text: "夜视距离 存储容量", Tags: []string{tag}, Categories: []string{db.KnowledgeHistoricalBid}}, emb)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search: %v %+v", err, hits)
	}
	top := hits[0]
	if top.Citation.DocID != first.Document.ID || top.Citation.SectionCode != "2.1" || top.Citation.PageStart != 1 || top.Citation.PageEnd != 2 ||
		top.Citation.SectionPath != "第二章 技术要求 / 2.1 设备技术参数" || top.Citation.FileName != "historical-bid.pdf" || top.Chunk.TableCount != 1 ||
		len(top.MatchedTerms) != 2 {
		t.Fatalf("top hit citation: %+v", top)
	}
	var tables []Table
	if err := json.Unmarshal(top.Chunk.Tables, &tables); err != nil || len(tables) != 1 || tables[0].PageEnd != 2 || tables[0].Rows != 5 {
		t.Fatalf("hit tables: %v %s", err, top.Chunk.Tables)
	}
	hits, err = d.SearchKnowledge(ctx, db.KnowledgeQuery{Text: "存储容量", Tags: []string{tag}, Categories: []string{db.KnowledgeProductSpec}}, emb)
	if err != nil || len(hits) != 1 || hits[0].Citation.DocID != spec.Document.ID || hits[0].Citation.PageStart != 2 || hits[0].Citation.PageEnd != 3 {
		t.Fatalf("docx spec hit: %v %+v", err, hits)
	}
}
