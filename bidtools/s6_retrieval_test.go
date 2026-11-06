package bidtools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/retrieval"
	"github.com/autobid/autobid-engine/toolhealth"
)

func s6Kit(t *testing.T) (*Kit, *retrieval.Service, string) {
	t.Helper()
	if os.Getenv("AUTOBID_TEST_DATABASE_URL") == "" {
		t.Fatal("S6 tool tests require a real PostgreSQL database")
	}
	pg := testDB(t)
	tenant := fmt.Sprintf("s6-tools-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, table := range []string{"retrieval_runs", "knowledge_index_events", "knowledge_indexes", "knowledge_catalogs"} {
			_, _ = pg.ExecContext(context.Background(), "DELETE FROM "+table+" WHERE tenant_id=$1", tenant)
		}
	})
	for _, category := range []string{db.KnowledgeStandardSolution, db.KnowledgeProductSpec} {
		id, _, err := pg.InsertKnowledgeDocument(context.Background(), &db.KnowledgeDocument{TenantID: tenant, Category: category, Title: "恢复服务", RawText: tenant + category}, []db.KnowledgeChunk{{Seq: 0, Content: "恢复时间：30秒", Tables: json.RawMessage(`[]`)}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pg.DeleteKnowledgeDocument(context.Background(), tenant, id) })
	}

	s := retrieval.New(pg, nil, nil)
	reg := toolhealth.New()
	for _, cap := range toolhealth.All {
		reg.SetUnconfigured(cap, "", "test unconfigured")
	}
	k := New(pg, 0, nil, nil).WithKnowledge(tenant, nil).WithRetrieval(s).WithRuntime(reg)
	return k, s, tenant
}

func TestKnowledgeSearchExplainsLexicalOnly(t *testing.T) {
	k, _, _ := s6Kit(t)
	for _, tc := range []struct{ name, input string }{{ToolKBSearchProposals, `{"query":"恢复时间"}`}, {ToolKBSearchProducts, `{"specs":["恢复时间"]}`}, {ToolKBSearchProposals, `{"query":"不存在的知识内容"}`}} {
		out, res := callKBTool(t, k.DeferredTools(), tc.name, tc.input)
		if res.IsError || out["retrieval_mode"] != "lexical_only" || out["index_state"] != "unconfigured" || out["degraded_note"] != "semantic channel unavailable; only full-text and keyword recall ran. No hits does not prove the knowledge base lacks the content." {
			t.Fatalf("%s: %+v", tc.name, out)
		}
		if out["run_id"] == nil || out["degraded"] == nil || out["citation_rule"] == nil {
			t.Fatal("existing result fields lost")
		}
	}
}

func TestToolRuntimeStatusSemanticRetrieval(t *testing.T) {
	k, s, tenant := s6Kit(t)
	k.WithResearch(&retrieval.Coordinator{Service: s, MultiEnabled: true})
	for _, cap := range []string{"all", "embedding", "reranker"} {
		out, res := callKBTool(t, k.RuntimeTools(), ToolRuntimeStatus, `{"capability":"`+cap+`"}`)
		if res.IsError {
			t.Fatal(res.Content)
		}
		semantic, ok := out["semantic_retrieval"].(map[string]any)
		if !ok || semantic["state"] != "lexical_only" || semantic["index_state"] != "unconfigured" || semantic["multi_agent_enabled"] != true || semantic["strategy"] != retrieval.Strategy {
			t.Fatalf("%+v", out)
		}
	}
	out, _ := callKBTool(t, k.RuntimeTools(), ToolRuntimeStatus, `{"capability":"office"}`)
	if out["semantic_retrieval"] != nil {
		t.Fatal("unrelated capability included retrieval status")
	}
	s.Embedder = db.HashEmbedder{Dim: 2}
	x, err := s.BuildIndex(context.Background(), tenant, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateIndex(context.Background(), tenant, x.ID, "test"); err != nil {
		t.Fatal(err)
	}
	out, _ = callKBTool(t, k.RuntimeTools(), ToolRuntimeStatus, `{}`)
	active := out["semantic_retrieval"].(map[string]any)["active_index"].(map[string]any)
	if len(active) != 4 || active["id"] != float64(x.ID) || active["dimension"] != float64(2) {
		t.Fatalf("active index summary: %+v", active)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := k.RuntimeTools()[0].Call(ctx, json.RawMessage(`{"capability":"embedding"}`), nil)
	if err != nil || res.IsError || !strings.Contains(res.Content, "semantic_retrieval_error") {
		t.Fatalf("status error failed tool: %+v %v", res, err)
	}
	k.WithRetrieval(nil)
	out, _ = callKBTool(t, k.RuntimeTools(), ToolRuntimeStatus, `{}`)
	if out["semantic_retrieval"] != nil || out["semantic_retrieval_error"] != nil {
		t.Fatal("nil retrieval service must omit semantic fields")
	}
}

func TestKBResearchReturnsDiagnostics(t *testing.T) {
	k, s, _ := s6Kit(t)
	k.WithResearch(&retrieval.Coordinator{Service: s})
	out, res := callKBTool(t, k.DeferredTools(), ToolKBResearch, `{"request_key":"s6-research","question":"恢复时间"}`)
	if res.IsError || out["degraded"] == nil || out["diagnostics"] == nil || out["diagnostics"].(map[string]any)["mode"] != "lexical_only" {
		t.Fatalf("%+v", out)
	}
}
