package ingest

// 集成测试：需要 AUTOBID_TEST_DATABASE_URL，未设置时跳过。

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

var (
	pgOnce sync.Once
	pg     *db.DB
	pgErr  error
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTOBID_TEST_DATABASE_URL not set")
	}
	pgOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if pg, pgErr = db.Open(ctx, dsn); pgErr == nil {
			pgErr = pg.Migrate(ctx)
		}
	})
	if pgErr != nil {
		t.Fatalf("open db: %v", pgErr)
	}
	return pg
}

func TestIngestCreatesRootAssetsAndIsIdempotent(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("ingest-%d", time.Now().UnixNano()), Goal: "解析招标文件"})
	if err != nil {
		t.Fatal(err)
	}
	as := db.NewAssetStore(d, task.ID)
	tenderNo := fmt.Sprintf("ZB-%d", time.Now().UnixNano())
	in := Input{TenderNo: tenderNo, LotNo: "A", Text: sampleTender}

	res, err := Ingest(ctx, d, as, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Chunks != len(Split(sampleTender)) || res.Mandatory < 3 || res.TenderAssetID == 0 || res.LotAssetID == 0 {
		t.Fatalf("first ingest: %+v", res)
	}
	if res.Document.Title != "XX市政务云平台采购项目招标文件" {
		t.Fatalf("default title: %q", res.Document.Title)
	}
	again, err := Ingest(ctx, d, as, in)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Document.ID != res.Document.ID || again.Chunks != res.Chunks || again.Mandatory != res.Mandatory ||
		again.TenderAssetID != res.TenderAssetID || again.LotAssetID != res.LotAssetID {
		t.Fatalf("re-ingest must be idempotent: %+v vs %+v", again, res)
	}

	// 根实体进入任务范围，tender -contains-> lot 边已建立。
	assets, err := as.ListScopedAssets(ctx, []string{db.AssetTender, db.AssetLot}, 10)
	if err != nil || len(assets) != 2 {
		t.Fatalf("scoped root assets: %d err=%v", len(assets), err)
	}
	edges, err := as.EntityEdgesOf(ctx, res.TenderAssetID, db.EntityRelContains, true, 10)
	if err != nil || len(edges) != 1 || edges[0].DstAssetID != res.LotAssetID {
		t.Fatalf("contains edge: %+v err=%v", edges, err)
	}

	// 同一招标项目的另一份文档复用同一 tender 实体。
	other, err := Ingest(ctx, d, as, Input{TenderNo: tenderNo, Title: "澄清公告", Text: "第一章 澄清\n1.1 投标截止时间顺延。"})
	if err != nil || !other.Created || other.TenderAssetID != res.TenderAssetID || other.LotAssetID != 0 {
		t.Fatalf("second document: %+v err=%v", other, err)
	}

	if _, err := Ingest(ctx, d, as, Input{TenderNo: tenderNo, Text: "  "}); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, err := Ingest(ctx, d, db.NewAssetStore(d, 0), in); err == nil {
		t.Fatal("unbound asset store accepted")
	}
}
