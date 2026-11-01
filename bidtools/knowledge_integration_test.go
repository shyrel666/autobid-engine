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
	"github.com/autobid/autobid-engine/retrieval"
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

func TestEvidenceVerifyAndBindToSectionContext(t *testing.T) {
	pg := testDB(t)
	ctx := context.Background()
	tenant := db.DefaultTenant
	emb := db.DefaultEmbedder()
	retSvc := retrieval.New(pg, emb, nil)

	task, err := pg.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("kb-bind-%d", time.Now().UnixNano()), Goal: "证据核验与绑定"})
	if err != nil {
		t.Fatal(err)
	}
	product := fmt.Sprintf("CAM%d", time.Now().UnixNano()%1_000_000)
	if _, _, err := pg.InsertKnowledgeDocument(ctx, &db.KnowledgeDocument{
		TenantID: tenant, Category: db.KnowledgeProductSpec, Title: product + " 高清摄像机规格书", Tags: []string{"安防"},
		RawText: product + " 400万像素", Extra: json.RawMessage(fmt.Sprintf(`{"product":%q}`, product)),
	}, []db.KnowledgeChunk{
		{
			Seq: 0, Level: 2, SectionCode: "1.1", SectionTitle: "传感器参数", SectionPath: "1.1 传感器参数", PageStart: 1, PageEnd: 1,
			Content: "分辨率：400万像素\n| 参数 | 指标值 |\n| --- | --- |\n| 分辨率 | 400万像素 |",
			Tables:  json.RawMessage(`[{"rows":2,"cols":2,"grid":[["参数","指标值"],["分辨率","400万像素"]],"markdown":"| 参数 | 指标值 |"}]`),
		},
	}, emb); err != nil {
		t.Fatal(err)
	}

	s := func(v string) *string { return &v }
	yes := true
	reqItem, err := pg.CreateRequirement(ctx, tenant, task.ID, "reviewer", db.RequirementInput{
		Code: s("REQ-CAM-1"), Category: s(db.ReqMandatoryClause), Title: s("摄像机分辨率要求"),
		Description: s("网络摄像机分辨率须不低于400万像素。"), IsMandatory: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ConfirmRequirements(ctx, tenant, task.ID, "reviewer", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CreateOutline(ctx, tenant, task.ID, "editor", db.OutlineInput{
		Sections: []db.SectionInput{{Code: "2.1", Title: "前端感知设备", Level: 1, RequirementIDs: []int64{reqItem.ID}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.SubmitOutline(ctx, tenant, task.ID, 1, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ApproveOutline(ctx, tenant, task.ID, 1, "approver"); err != nil {
		t.Fatal(err)
	}

	kit := New(pg, task.ID, db.NewAssetStore(pg, task.ID), db.NewExplorationStore(pg, task.ExplorationID)).WithRetrieval(retSvc)
	tools := kit.DeferredTools()

	// 1. kb_search_products 在每个 hit 上直接透出 evidence_id
	searchOut, _ := callKBTool(t, tools, ToolKBSearchProducts, fmt.Sprintf(`{"product":%q,"specs":["分辨率","400万像素"]}`, product))
	hits := searchOut["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("expected 1 product hit, got %+v", searchOut)
	}
	evID := int64(hits[0].(map[string]any)["evidence_id"].(float64))
	if evID <= 0 {
		t.Fatalf("expected positive evidence_id on hit: %+v", hits[0])
	}

	// 2. 未经核验（unverified）的证据不能通过 requirement_bind_evidence 绑定为支撑证据
	if _, res := callKBTool(t, tools, ToolRequirementBindEvidence, fmt.Sprintf(`{"requirement_code":"REQ-CAM-1","evidence_ids":[%d]}`, evID)); !res.IsError {
		t.Fatalf("unverified evidence must be rejected by requirement_bind_evidence")
	}

	// 3. 调用 kb_verify_evidence 完成确定性核验（verified + supports）
	verOut, res := callKBTool(t, tools, ToolKBVerifyEvidence, fmt.Sprintf(`{"evidence_id":%d,"claim":{"text":"分辨率>=400万像素","parameter":"分辨率","operator":">=","value":400,"unit":"万像素","product":%q}}`, evID, product))
	if res.IsError || verOut["status"] != "verified" || verOut["relation"] != "supports" {
		t.Fatalf("expected verified+supports, got err=%v content=%s out=%+v", res.IsError, res.Content, verOut)
	}

	// 4. 调用 requirement_bind_evidence 绑定已核验证据，且不推翻已批准大纲
	bindOut, _ := callKBTool(t, tools, ToolRequirementBindEvidence, fmt.Sprintf(`{"requirement_code":"REQ-CAM-1","evidence_ids":[%d]}`, evID))
	boundIDs := bindOut["evidence_ids"].([]any)
	if len(boundIDs) != 1 || int64(boundIDs[0].(float64)) != evID {
		t.Fatalf("unexpected bind output: %+v", bindOut)
	}

	// 5. section_read_context 返回 writable=true 且 usable_evidence 包含该证据
	ctxOut, _ := callKBTool(t, tools, ToolSectionReadContext, `{"section_code":"2.1"}`)
	if ctxOut["writable"] != true {
		t.Fatalf("outline must remain approved and writable after evidence binding: %+v", ctxOut)
	}
	usable := ctxOut["usable_evidence"].([]any)
	if len(usable) != 1 || int64(usable[0].(map[string]any)["id"].(float64)) != evID {
		t.Fatalf("expected usable_evidence to contain %d, got %+v", evID, usable)
	}
}
