package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestNodeMandateParsing(t *testing.T) {
	if NodeMandate(nil) != nil {
		t.Fatal("expected nil mandate for nil node")
	}
	emptyNode := &Node{Payload: json.RawMessage(`{"summary":"only summary"}`)}
	if NodeMandate(emptyNode) != nil {
		t.Fatal("expected nil mandate when mandate field is absent")
	}

	withMandate := &Node{Payload: json.RawMessage(`{
		"summary": "撰写第 2.1 节总体架构",
		"mandate": {
			"allowed_section_ids": [11, 12],
			"allowed_requirement_ids": [101],
			"target_words_min": 800,
			"target_words_max": 2500,
			"max_tool_calls": 10,
			"forbidden_tools": ["insert_assets"],
			"require_writeback": true
		}
	}`)}
	m := NodeMandate(withMandate)
	if m == nil || m.IsEmpty() {
		t.Fatal("expected non-empty mandate")
	}
	if len(m.AllowedSectionIDs) != 2 || m.AllowedSectionIDs[0] != 11 || m.TargetWordsMin != 800 || m.MaxToolCalls != 10 || !m.RequireWriteback {
		t.Fatalf("unexpected mandate fields: %+v", m)
	}
}

func TestBootstrapRFPGoalsIntegration(t *testing.T) {
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTOBID_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pg.Close()
	if err := pg.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	taskRow, err := pg.CreateTask(ctx, CreateTaskInput{
		Name: "rfp-goals-test",
		Goal: "完成技术标推演与编制",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	defer func() { _ = pg.DeleteTask(ctx, taskRow.ID) }()

	tenderNo := fmt.Sprintf("ZB-GOALS-%d", time.Now().UnixNano())
	as := NewAssetStore(pg, taskRow.ID)
	if _, _, err := as.UpsertAsset(ctx, &Asset{Type: AssetTender, TenderNo: tenderNo, Title: "智慧园区招标文件"}, 0); err != nil {
		t.Fatalf("UpsertAsset tender: %v", err)
	}
	doc := &TenderDocument{
		TenantID:    DefaultTenant,
		TaskID:      taskRow.ID,
		TenderNo:    tenderNo,
		Title:       "智慧园区招标文件",
		RawText:     "★投标人须具有电子与智能化工程专业承包一级资质，否则投标无效。\n技术方案评分：总体架构设计合理得15分。",
		ContentHash: ContentHash(tenderNo),
	}
	chunks := []DocumentChunk{
		{Seq: 0, Level: 2, SectionCode: "1.1", SectionTitle: "资格要求", PageNo: 1, PageStart: 1, PageEnd: 1,
			Content: "★投标人须具有电子与智能化工程专业承包一级资质，否则投标无效。", IsMandatory: true, Tables: json.RawMessage(`[]`), BBoxMeta: json.RawMessage(`{}`)},
		{Seq: 1, Level: 2, SectionCode: "3.1", SectionTitle: "技术评分标准", PageNo: 5, PageStart: 5, PageEnd: 5,
			Content: "技术方案评分：总体架构设计合理、可扩展性强得15分。", IsMandatory: false, Tables: json.RawMessage(`[]`), BBoxMeta: json.RawMessage(`{}`)},
	}
	if _, _, err := pg.InsertTenderDocument(ctx, doc, chunks); err != nil {
		t.Fatalf("InsertTenderDocument: %v", err)
	}

	// First call: extracts requirements, syncs assets, initializes 4 root Goals, and creates v1 draft outline.
	res1, err := pg.BootstrapRFPGoals(ctx, DefaultTenant, taskRow.ID, "tester", true)
	if err != nil {
		t.Fatalf("BootstrapRFPGoals(1): %v", err)
	}
	if res1.ExtractResult.Created < 2 {
		t.Fatalf("expected >= 2 extracted requirements, got %+v", res1.ExtractResult)
	}
	if res1.SyncedAssets < 2 {
		t.Fatalf("expected >= 2 synced assets, got %d", res1.SyncedAssets)
	}
	if len(res1.Goals) != 4 {
		t.Fatalf("expected 4 canonical root goals, got %d: %+v", len(res1.Goals), res1.Goals)
	}
	for _, g := range res1.Goals {
		if !g.Created || g.NodeID <= 0 {
			t.Fatalf("expected goal %s to be created on first run, got %+v", g.GoalCode, g)
		}
	}
	if !res1.OutlineCreated || res1.OutlineVersion != 1 {
		t.Fatalf("expected v1 draft outline to be created, got created=%v ver=%d", res1.OutlineCreated, res1.OutlineVersion)
	}

	// Second call: idempotent! Must not duplicate Goal nodes or create a second outline.
	res2, err := pg.BootstrapRFPGoals(ctx, DefaultTenant, taskRow.ID, "tester", true)
	if err != nil {
		t.Fatalf("BootstrapRFPGoals(2): %v", err)
	}
	for i, g := range res2.Goals {
		if g.Created || g.NodeID != res1.Goals[i].NodeID {
			t.Fatalf("expected idempotent goal reuse on second run, got %+v vs %+v", g, res1.Goals[i])
		}
	}
	if res2.OutlineCreated {
		t.Fatalf("expected second run not to create another outline")
	}
}
