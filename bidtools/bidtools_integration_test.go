package bidtools

// 工具对真实 PostgreSQL 的集成测试：需要 AUTOBID_TEST_DATABASE_URL，未设置时跳过。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/internal/tool"
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

const tenderText = `政务云平台采购项目招标文件
第一章 投标人须知
1.1 投标人资格要求
1.1.1 投标人须具有独立法人资格。
★1.1.2 投标人须具备信息系统集成一级资质，否则投标无效。
1.1.3 报价超过最高限价的，按废标处理。
第二章 评标办法
2.1 技术方案（30 分）
2.2 类似业绩（10 分）`

type kitEnv struct {
	kit      *Kit
	task     *db.Task
	es       *db.ExplorationStore
	as       *db.AssetStore
	tenderNo string
	ingested *ingest.Result
}

func newKitEnv(t *testing.T) *kitEnv {
	t.Helper()
	d := testDB(t)
	ctx := context.Background()
	task, err := d.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("bidtools-%d", time.Now().UnixNano()), Goal: "资格审查"})
	if err != nil {
		t.Fatal(err)
	}
	env := &kitEnv{task: task, es: db.NewExplorationStore(d, task.ExplorationID), as: db.NewAssetStore(d, task.ID),
		tenderNo: fmt.Sprintf("ZB-%d", time.Now().UnixNano())}
	env.kit = New(d, task.ID, env.as, env.es)
	if env.ingested, err = ingest.Ingest(ctx, d, env.as, ingest.Input{TenderNo: env.tenderNo, LotNo: "A", Text: tenderText}); err != nil {
		t.Fatal(err)
	}
	return env
}

func callTool(t *testing.T, tools []tool.CoreTool, name string, input any) map[string]any {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() != name {
			continue
		}
		raw, _ := json.Marshal(input)
		if err := tool.ValidateInput(tl.InputSchema(), raw); err != nil {
			t.Fatalf("%s input invalid: %v", name, err)
		}
		res, err := tl.Call(context.Background(), raw, &tool.ToolContext{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s error result: %s", name, res.Content)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
			t.Fatalf("%s: non-JSON result %q", name, res.Content)
		}
		return out
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

func TestDocumentToolsAgainstDB(t *testing.T) {
	env := newKitEnv(t)
	tools := env.kit.ResidentTools()

	outline := callTool(t, tools, ToolDocumentOutline, map[string]any{"max_level": 4})
	entries := outline["outline"].([]any)
	if len(entries) != 6 { // 文首、第一章、1.1、第二章、2.1、2.2（1.1.x 为 level 5 被过滤）
		t.Fatalf("outline entries = %d: %v", len(entries), entries)
	}

	found := callTool(t, tools, ToolSearchDocument, map[string]any{"query": "资质", "mandatory_only": true})
	matches := found["matches"].([]any)
	if found["total"].(float64) != 1 {
		t.Fatalf("search: %v", found)
	}
	m := matches[0].(map[string]any)
	if m["section_code"] != "1.1.2" || !strings.Contains(m["snippet"].(string), "资质") {
		t.Fatalf("match: %v", m)
	}
	all := callTool(t, tools, ToolSearchDocument, map[string]any{"mandatory_only": true})
	if all["total"].(float64) != 2 { // ★1.1.2 与 废标 1.1.3
		t.Fatalf("mandatory_only total: %v", all["total"])
	}

	read := callTool(t, tools, ToolReadChunk, map[string]any{"chunk_id": m["chunk_id"], "before": 1, "after": 9})
	ref := read["source_ref"].(map[string]any)
	if ref["section_code"] != "1.1.2" || ref["tender_no"] != env.tenderNo || ref["lot_no"] != "A" ||
		ref["doc_id"].(float64) != float64(env.ingested.Document.ID) || ref["chunk_id"] != m["chunk_id"] || ref["page_no"].(float64) != 1 {
		t.Fatalf("source_ref: %v", ref)
	}
	if len(read["before"].([]any)) != 1 || len(read["after"].([]any)) != 3 {
		t.Fatalf("window: before=%v after=%v", read["before"], read["after"])
	}
	if !strings.Contains(read["chunk"].(map[string]any)["content"].(string), "否则投标无效") {
		t.Fatalf("content: %v", read["chunk"])
	}

	// 其他任务读不到本任务的段落。
	other := newKitEnv(t)
	raw, _ := json.Marshal(map[string]any{"chunk_id": m["chunk_id"]})
	for _, tl := range other.kit.ResidentTools() {
		if tl.Name() == ToolReadChunk {
			if res, _ := tl.Call(context.Background(), raw, nil); !res.IsError {
				t.Fatalf("cross-task read allowed: %s", res.Content)
			}
		}
	}
}

func TestDeferredToolsAgainstDB(t *testing.T) {
	env := newKitEnv(t)
	ctx := context.Background()
	d := testDB(t)
	tag := fmt.Sprintf("cred-%d", time.Now().UnixNano())
	if _, _, err := d.UpsertCompanyCredential(ctx, &db.CompanyCredential{Category: db.CredentialQualification, Code: tag,
		Title: "信息系统集成及服务资质 " + tag, Attributes: json.RawMessage(`{"level":"一级"}`)}); err != nil {
		t.Fatal(err)
	}
	tools := env.kit.DeferredTools()
	cred := callTool(t, tools, ToolQueryCredentials, map[string]any{"category": db.CredentialQualification, "keywords": []string{tag},
		"attributes": map[string]any{"level": "一级"}, "requirement": "信息系统集成一级资质"})
	if cred["verdict"] != VerdictSatisfied {
		t.Fatalf("credentials: %v", cred)
	}
	cred = callTool(t, tools, ToolQueryCredentials, map[string]any{"category": db.CredentialQualification, "keywords": []string{tag},
		"attributes": map[string]any{"level": "特级"}})
	if cred["verdict"] != VerdictPartial {
		t.Fatalf("credentials gap: %v", cred)
	}

	tech, _, _ := env.as.UpsertAsset(ctx, &db.Asset{Type: db.AssetScoringItem, TenderNo: env.tenderNo, LotNo: "A", Code: "2.1",
		Title: "技术方案", Category: "技术", ScoreMax: ptr(30.0)}, 0)
	if _, _, err := env.as.UpsertAsset(ctx, &db.Asset{Type: db.AssetScoringItem, TenderNo: env.tenderNo, LotNo: "A", Code: "2.2",
		Title: "类似业绩", Category: "商务", ScoreMax: ptr(10.0)}, 0); err != nil {
		t.Fatal(err)
	}
	intent, _ := env.es.AddIntentTx(ctx, db.AddIntentInput{Payload: map[string]any{"summary": "评分模拟"}})
	if _, err := env.es.RecordFactWithYieldsTx(ctx, intent, map[string]any{"summary": "技术方案预计 24 分", "expected_score": 24}, 0, "w", []int64{tech}); err != nil {
		t.Fatal(err)
	}
	score := callTool(t, tools, ToolSimulateScoring, map[string]any{"lot_no": "A"})
	if score["max_total"].(float64) != 40 || score["expected_total"].(float64) != 24 || score["ratio"].(float64) != 0.6 {
		t.Fatalf("scoring: %v", score)
	}

	if _, _, err := env.as.UpsertAsset(ctx, &db.Asset{Type: db.AssetLot, TenderNo: env.tenderNo, LotNo: "A", Amount: ptr("100000")}, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.as.UpsertAsset(ctx, &db.Asset{Type: db.AssetCommercialItem, TenderNo: env.tenderNo, LotNo: "A", Code: "C-01",
		Title: "云主机", Quantity: ptr("10")}, 0); err != nil {
		t.Fatal(err)
	}
	price := callTool(t, tools, ToolVerifyCommercial, map[string]any{"lot_no": "A",
		"quotes": []map[string]any{{"code": "C-01", "unit_price": 12000}}})
	if price["verdict"] != "fail" || price["ceiling_source"] != "lot_asset" || price["computed_total"] != "120000.00" {
		t.Fatalf("pricing: %v", price)
	}
}
