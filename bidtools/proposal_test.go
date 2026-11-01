package bidtools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

func TestParseSectionDraftRecognizesInlineAndSpilledEvidence(t *testing.T) {
	sc := &SectionContext{
		SectionID:      1,
		Code:           "2.1",
		Title:          "总体架构设计",
		UsableEvidence: []ContextEvidence{{ID: 10, Domain: "knowledge_chunk", Usable: true, Excerpt: "内联证据摘录"}},
		SpilledEvidence: []SpilledEvidenceBrief{
			{ID: 20, Domain: "knowledge_chunk", Snippet: "溢出证据微摘要"},
		},
	}

	raw := `{
		"content_md": "根据我方既有高并发云平台架构经验，支持百万级并发接入。",
		"summary_note": "总体架构采用云原生微服务底座。",
		"diff_summary": "初稿",
		"evidence_ids": [10, 20, 999]
	}`
	draft, err := ParseSectionDraft(raw, sc)
	if err != nil {
		t.Fatalf("ParseSectionDraft failed: %v", err)
	}
	if len(draft.EvidenceIDs) != 2 || draft.EvidenceIDs[0] != 10 || draft.EvidenceIDs[1] != 20 {
		t.Fatalf("expected inline (10) and spilled (20) evidence IDs to be kept, got %v", draft.EvidenceIDs)
	}
	if len(draft.Gaps) != 1 || draft.Gaps[0].Kind != "unknown_evidence" {
		t.Fatalf("expected unknown evidence 999 to be recorded as gap, got %+v", draft.Gaps)
	}
}

func TestReadChunkToolPaginationBudget(t *testing.T) {
	pg := testDB(t)
	ctx := context.Background()
	task, err := pg.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("read-chunk-page-%d", time.Now().UnixNano()), Goal: "分页测试"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.DeleteTask(ctx, task.ID) }()

	longContent := strings.Repeat("第三章 技术参数要求与系统性能指标说明。\n", 120)
	doc := &db.TenderDocument{
		TenantID:    db.DefaultTenant,
		TaskID:      task.ID,
		TenderNo:    fmt.Sprintf("ZB-PAGE-%d", time.Now().UnixNano()),
		Title:       "超长切片招标文件",
		RawText:     longContent,
		ContentHash: db.ContentHash(longContent),
	}
	_, _, err = pg.InsertTenderDocument(ctx, doc, []db.DocumentChunk{
		{
			Seq: 0, Level: 2, SectionCode: "3.1", SectionTitle: "技术参数", PageNo: 1, PageStart: 1, PageEnd: 2,
			Content: longContent, Tables: json.RawMessage(`[]`), BBoxMeta: json.RawMessage(`{}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := pg.TaskChunks(ctx, task.ID, 0, 10)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("TaskChunks: %v len=%d", err, len(chunks))
	}

	kit := New(pg, task.ID, db.NewAssetStore(pg, task.ID), db.NewExplorationStore(pg, task.ExplorationID))
	out, res := callKBTool(t, kit.ResidentTools(), ToolReadChunk, fmt.Sprintf(`{"chunk_id":%d,"context_budget":2048}`, chunks[0].ID))
	if res.IsError {
		t.Fatalf("read_chunk page 1 error: %s", res.Content)
	}
	if out["text_complete"] != false {
		t.Fatalf("expected text_complete=false under tight budget, got %+v", out)
	}
	nextOffset := int(out["next_text_offset"].(float64))
	if nextOffset <= 0 {
		t.Fatalf("expected positive next_text_offset, got %d", nextOffset)
	}

	// Read page 2 using next_text_offset
	out2, res2 := callKBTool(t, kit.ResidentTools(), ToolReadChunk, fmt.Sprintf(`{"chunk_id":%d,"text_offset":%d,"context_budget":16000}`, chunks[0].ID, nextOffset))
	if res2.IsError || out2["text_complete"] != true {
		t.Fatalf("expected page 2 to complete text, got err=%v out=%+v", res2.IsError, out2)
	}
}

