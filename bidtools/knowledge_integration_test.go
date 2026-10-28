package bidtools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
)

func callKBTool(t *testing.T, tools []tool.CoreTool, name string, input string) (map[string]any, tool.Result) {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() != name {
			continue
		}
		res, err := tl.Call(context.Background(), json.RawMessage(input), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var out map[string]any
		if !res.IsError {
			if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
				t.Fatalf("%s: decode %s: %v", name, res.Content, err)
			}
		}
		return out, res
	}
	t.Fatalf("tool %s not registered", name)
	return nil, tool.Result{}
}

func TestKnowledgeToolsReturnCitations(t *testing.T) {
	pg := testDB(t)
	ctx := context.Background()
	tenant := fmt.Sprintf("kb-tools-%d", time.Now().UnixNano())
	emb := db.DefaultEmbedder()
	product := fmt.Sprintf("XG%d", time.Now().UnixNano()%1_000_000)

	bidID, _, err := pg.InsertKnowledgeDocument(ctx, &db.KnowledgeDocument{TenantID: tenant, Category: db.KnowledgeHistoricalBid,
		Title: "某区智慧园区安防项目技术标", Tags: []string{"智慧园区"}, RawText: tenant + "bid", FileName: "bid.docx"}, []db.KnowledgeChunk{
		{Seq: 0, Level: 1, SectionCode: "第一章", SectionTitle: "总体方案", SectionPath: "第一章 总体方案", PageStart: 1, PageEnd: 1,
			Content: "采用云边协同架构建设视频监控平台。", Tables: json.RawMessage(`[]`)},
		{Seq: 1, Level: 4, SectionCode: "1.1", SectionTitle: "运维保障", SectionPath: "第一章 总体方案 / 1.1 运维保障", PageStart: 2, PageEnd: 3,
			Content: "提供7×24小时运维服务，故障2小时内到场。", Tables: json.RawMessage(`[]`)},
	}, emb)
	if err != nil {
		t.Fatal(err)
	}
	specID, _, err := pg.InsertKnowledgeDocument(ctx, &db.KnowledgeDocument{TenantID: tenant, Category: db.KnowledgeProductSpec,
		Title: product + " 网络摄像机规格书", Tags: []string{"安防"}, RawText: tenant + "spec"}, []db.KnowledgeChunk{
		{Seq: 0, Level: 3, SectionCode: "1", SectionTitle: "技术参数", SectionPath: "1 技术参数", PageStart: 4, PageEnd: 5,
			Content: "| 参数 | 指标 |\n| --- | --- |\n| 分辨率 | 400万像素 |",
			Tables:  json.RawMessage(`[{"rows":2,"cols":2,"grid":[["参数","指标"],["分辨率","400万像素"]],"markdown":"| 参数 | 指标 |"}]`)},
	}, emb)
	if err != nil {
		t.Fatal(err)
	}

	kit := New(pg, 1, nil, nil).WithKnowledge(tenant, emb)
	tools := kit.DeferredTools()

	out, _ := callKBTool(t, tools, ToolKBSearchProposals, `{"query":"运维 到场","tags":["智慧园区"]}`)
	hits := out["hits"].([]any)
	if len(hits) == 0 {
		t.Fatalf("proposals: %v", out)
	}
	top := hits[0].(map[string]any)
	cite := top["citation"].(map[string]any)
	if cite["doc_id"].(float64) != float64(bidID) || cite["seq"].(float64) != 1 || cite["doc_title"] != "某区智慧园区安防项目技术标" ||
		cite["category"] != db.KnowledgeHistoricalBid || cite["section_code"] != "1.1" || cite["page_start"].(float64) != 2 || cite["page_end"].(float64) != 3 ||
		!strings.Contains(top["cite"].(string), "第2-3页") || !strings.Contains(top["snippet"].(string), "到场") {
		t.Fatalf("proposal hit: %v", top)
	}
	if out["citation_rule"] == nil {
		t.Fatal("search result must state the citation rule")
	}
	// 方案检索不得返回产品规格书。
	out, _ = callKBTool(t, tools, ToolKBSearchProposals, `{"query":"分辨率"}`)
	if len(out["hits"].([]any)) != 0 {
		t.Fatalf("proposal search must exclude product specs: %v", out)
	}
	if _, res := callKBTool(t, tools, ToolKBSearchProposals, `{"query":"x","categories":["product_spec"]}`); !res.IsError {
		t.Fatal("product category must be rejected by kb_search_proposals")
	}

	out, _ = callKBTool(t, tools, ToolKBSearchProducts, `{"product":"`+product+`","specs":["分辨率","400万像素"]}`)
	hits = out["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("products: %v", out)
	}
	top = hits[0].(map[string]any)
	if cite := top["citation"].(map[string]any); cite["doc_id"].(float64) != float64(specID) || cite["page_start"].(float64) != 4 || top["table_count"].(float64) != 1 {
		t.Fatalf("product hit: %v", top)
	}
	if out, _ := callKBTool(t, tools, ToolKBSearchProducts, `{"product":"不存在的型号ZZZ","specs":["分辨率"]}`); len(out["hits"].([]any)) != 0 {
		t.Fatalf("product must be a hard filter: %v", out)
	}
	if _, res := callKBTool(t, tools, ToolKBSearchProducts, `{}`); !res.IsError {
		t.Fatal("empty product search must be rejected")
	}

	out, _ = callKBTool(t, tools, ToolKBReadChunk, fmt.Sprintf(`{"doc_id":%d,"seq":0,"after":1}`, bidID))
	chunk := out["chunk"].(map[string]any)
	if chunk["section_path"] != "第一章 总体方案" || len(out["after"].([]any)) != 1 || out["citation"].(map[string]any)["page_end"].(float64) != 1 {
		t.Fatalf("read chunk: %v", out)
	}
	out, _ = callKBTool(t, tools, ToolKBReadChunk, fmt.Sprintf(`{"doc_id":%d,"seq":0}`, specID))
	tables := out["chunk"].(map[string]any)["tables"].([]any)
	if len(tables) != 1 || tables[0].(map[string]any)["grid"].([]any)[1].([]any)[1] != "400万像素" {
		t.Fatalf("read chunk tables: %v", out)
	}
	if _, res := callKBTool(t, tools, ToolKBReadChunk, fmt.Sprintf(`{"doc_id":%d,"seq":99}`, specID)); !res.IsError {
		t.Fatal("missing chunk must be a tool error")
	}
	// 其他租户读取不到。
	other := New(pg, 1, nil, nil).WithKnowledge(tenant+"-other", emb).DeferredTools()
	if _, res := callKBTool(t, other, ToolKBReadChunk, fmt.Sprintf(`{"doc_id":%d,"seq":0}`, specID)); !res.IsError {
		t.Fatal("cross-tenant read must fail")
	}
}
