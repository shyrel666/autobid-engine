package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

func TestIngestXLSXChunkingProvenanceAndRequirements(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("xlsx-%d", time.Now().UnixNano()), Goal: "表格招标文件"})
	if err != nil {
		t.Fatal(err)
	}
	as := db.NewAssetStore(d, task.ID)
	res, err := Ingest(ctx, d, as, Input{TenderNo: fmt.Sprintf("XLSX-%d", time.Now().UnixNano()),
		File: &File{Name: "clean.xlsx", Data: fixtureBytes(t, "clean.xlsx")}, TenantID: db.DefaultTenant})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.PageCount != 0 || res.Chunks == 0 || res.Tables != 4 {
		t.Fatalf("ingest = %+v", res)
	}
	var pageCount *int
	if err := d.QueryRowContext(ctx, `SELECT page_count FROM tender_documents WHERE id = $1`, res.Document.ID).Scan(&pageCount); err != nil {
		t.Fatal(err)
	}
	if pageCount != nil {
		t.Fatalf("spreadsheet page_count = %v, want NULL", *pageCount)
	}
	var physicalChunks int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_chunks WHERE doc_id = $1 AND (page_start IS NOT NULL OR page_end IS NOT NULL)`,
		res.Document.ID).Scan(&physicalChunks); err != nil {
		t.Fatal(err)
	}
	if physicalChunks != 0 {
		t.Fatalf("spreadsheet chunks with physical pages = %d", physicalChunks)
	}
	chunks, err := d.TaskChunks(ctx, task.ID, res.Document.ID, 1000)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("chunks: %d %v", len(chunks), err)
	}
	spreadsheetChunks := 0
	for _, c := range chunks {
		if c.PageIsPhysical {
			t.Fatalf("spreadsheet chunk marked physical page: %+v", c)
		}
		var meta map[string]any
		if err := json.Unmarshal(c.BBoxMeta, &meta); err != nil {
			t.Fatal(err)
		}
		if meta["page_is_physical"] != false {
			t.Fatalf("chunk %d bbox meta = %s", c.ID, c.BBoxMeta)
		}
		var tables []struct {
			SourceLocator *SourceLocator `json:"source_locator"`
			Range         string         `json:"range"`
			Sheet         string         `json:"sheet"`
		}
		if err := json.Unmarshal(c.Tables, &tables); err != nil || len(tables) == 0 {
			t.Fatalf("chunk %d tables = %s (%v)", c.ID, c.Tables, err)
		}
		if tables[0].SourceLocator == nil || tables[0].SourceLocator.Kind != "spreadsheet" || tables[0].SourceLocator.Range == "" {
			t.Fatalf("chunk %d locator = %+v", c.ID, tables[0].SourceLocator)
		}
		spreadsheetChunks++
	}
	if spreadsheetChunks == 0 {
		t.Fatal("no spreadsheet chunks")
	}
	extract, err := d.ExtractRequirements(ctx, db.DefaultTenant, task.ID, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if extract.Created == 0 {
		t.Fatalf("no requirements extracted: %+v", extract)
	}
	view, err := d.ListRequirements(ctx, db.DefaultTenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, it := range view.Items {
		var loc struct {
			Kind  string `json:"kind"`
			Sheet string `json:"sheet"`
			Range string `json:"range"`
		}
		_ = json.Unmarshal(it.SourceLoc, &loc)
		if loc.Kind != "spreadsheet" {
			continue
		}
		found++
		if loc.Sheet == "" || loc.Range == "" || it.SourceHash == "" || it.SourceChunkID == nil {
			t.Fatalf("requirement source not traceable: %+v loc=%+v", it, loc)
		}
	}
	if found == 0 {
		var codes []string
		for _, it := range view.Items {
			codes = append(codes, it.Code+" "+string(it.SourceLoc))
		}
		t.Fatalf("no spreadsheet-sourced requirement; items=%v", codes)
	}
	if view.Manifest.SourceEpoch == 0 || view.Manifest.ExtractedSourceEpoch != view.Manifest.SourceEpoch {
		t.Fatalf("source epoch = %+v", view.Manifest)
	}
	if _, err := d.ConfirmRequirements(ctx, db.DefaultTenant, task.ID, "tester", 0); err != nil {
		t.Fatalf("confirm after extraction: %v", err)
	}
	ready, err := d.ExportReadiness(ctx, db.DefaultTenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Counts[db.BlockSourcePending] != 0 {
		t.Fatalf("unexpected source pending blocker: %+v", ready.Blockers)
	}
	// 相同字节重复上传幂等，不重复切分。
	again, err := Ingest(ctx, d, as, Input{TenderNo: res.Document.TenderNo,
		File: &File{Name: "clean.xlsx", Data: fixtureBytes(t, "clean.xlsx")}, TenantID: db.DefaultTenant})
	if err != nil || again.Created || again.Document.ID != res.Document.ID {
		t.Fatalf("re-ingest = %+v %v", again, err)
	}
	if !strings.Contains(again.Document.Title, "报价清单") && again.Document.Title != res.Document.Title {
		t.Fatalf("title changed: %q vs %q", again.Document.Title, res.Document.Title)
	}
}

func TestXLSXRequirementSourceHashCoversCellValues(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("xlsx-hash-%d", time.Now().UnixNano()), Goal: "表格来源哈希"})
	if err != nil {
		t.Fatal(err)
	}
	as := db.NewAssetStore(d, task.ID)
	if _, err := Ingest(ctx, d, as, Input{TenderNo: fmt.Sprintf("XLSX-HASH-%d", time.Now().UnixNano()),
		File: &File{Name: "clean.xlsx", Data: fixtureBytes(t, "clean.xlsx")}, TenantID: db.DefaultTenant}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExtractRequirements(ctx, db.DefaultTenant, task.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	view, err := d.ListRequirements(ctx, db.DefaultTenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var target *db.RequirementItem
	for _, it := range view.Items {
		var loc struct {
			Kind  string `json:"kind"`
			Sheet string `json:"sheet"`
		}
		_ = json.Unmarshal(it.SourceLoc, &loc)
		if loc.Kind == "spreadsheet" && loc.Sheet == "评分表" && it.SourceChunkID != nil {
			target = it
			break
		}
	}
	if target == nil {
		t.Fatal("no scoring requirement from 评分表")
	}
	beforeHash := target.SourceHash
	var tables []byte
	chunkID := *target.SourceChunkID
	if err := d.QueryRowContext(ctx, `SELECT tables_json::text FROM document_chunks WHERE id=$1`, chunkID).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(tables), "\"50\"", "\"60\"", 1)
	if mutated == string(tables) {
		t.Fatalf("score cell not found in tables: %s", tables)
	}
	if _, err := d.ExecContext(ctx, `UPDATE document_chunks SET tables_json=$2::jsonb WHERE id=$1`, chunkID, mutated); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRequirement(ctx, db.DefaultTenant, task.ID, target.ID)
	if err != nil || got.ReviewStatus != db.ReviewNeedsRevision {
		t.Fatalf("requirement after raw cell change = %+v %v", got, err)
	}
	if _, err := d.ExtractRequirements(ctx, db.DefaultTenant, task.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	after, err := d.GetRequirement(ctx, db.DefaultTenant, task.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SourceHash == beforeHash {
		t.Fatal("source_hash did not change after cell raw value change")
	}
	if _, err := d.ExecContext(ctx, `UPDATE requirement_items SET review_status='confirmed'
		WHERE tenant_id=$1 AND task_id=$2 AND review_status='needs_revision'`, db.DefaultTenant, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConfirmRequirements(ctx, db.DefaultTenant, task.ID, "tester", 0); err != nil {
		t.Fatalf("confirm after raw change: %v", err)
	}
}
