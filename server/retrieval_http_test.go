package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/retrieval"
)

func TestHTTPRetrievalEvidenceAndDefaultMultiGate(t *testing.T) {
	c, _ := newIdleAPI(t)
	product := fmt.Sprintf("HTTP-%d", time.Now().UnixNano())
	var up struct {
		Document db.KnowledgeDocument `json:"document"`
	}
	if code := c.do("POST", "/api/knowledge/documents", map[string]any{"category": "product_spec", "title": product, "text": "恢复时间：30秒", "extra": map[string]string{"product": product}}, &up); code != 201 {
		t.Fatal(code)
	}
	defer c.do("DELETE", fmt.Sprintf("/api/knowledge/documents/%d", up.Document.ID), nil, nil)
	var result retrieval.Result
	if code := c.do("GET", "/api/knowledge/search?q="+product, nil, &result); code != 200 || result.RunID == 0 || len(result.Degraded) == 0 || len(result.EvidenceIDs) == 0 {
		t.Fatalf("%d %+v", code, result)
	}
	var evidence []db.RetrievalEvidence
	path := fmt.Sprintf("/api/retrieval/runs/%d/evidence", result.RunID)
	if code := c.do("GET", path, nil, &evidence); code != 200 || len(evidence) == 0 || evidence[0].Status != "unverified" {
		t.Fatalf("%d %+v", code, evidence)
	}
	var verified db.RetrievalEvidence
	if code := c.do("POST", fmt.Sprintf("%s/%d/verify", path, evidence[0].ID), retrieval.Claim{Text: "恢复时间≤30秒", Parameter: "恢复时间", Operator: "<=", Value: 30, Unit: "秒", Product: product}, &verified); code != 200 || verified.Relation != "supports" {
		t.Fatalf("%d %+v", code, verified)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if code := c.do("POST", "/api/tasks", map[string]any{"name": "research", "goal": "bounded research"}, &created); code != 201 {
		t.Fatal(code)
	}
	if code := c.do("POST", fmt.Sprintf("/api/tasks/%d/research", created.ID), retrieval.ResearchRequest{Key: product, Question: product, Multi: true}, nil); code != 409 {
		t.Fatalf("multi default gate %d", code)
	}
	var research retrieval.ResearchResult
	if code := c.do("POST", fmt.Sprintf("/api/tasks/%d/research", created.ID), retrieval.ResearchRequest{Key: product, Question: product}, &research); code != 200 || research.Status != "partial" {
		t.Fatalf("%d %+v", code, research)
	}
	if code := c.do("POST", "/api/knowledge/indexes", map[string]int{"dimension": 16}, nil); code != 503 {
		t.Fatalf("missing semantic service %d", code)
	}
	if code := c.do("DELETE", fmt.Sprintf("/api/knowledge/documents/%d", up.Document.ID), nil, nil); code != 204 {
		t.Fatal(code)
	}
	if code := c.do("GET", path, nil, &evidence); code != 200 || evidence[0].Status != "stale" {
		t.Fatalf("%d %+v", code, evidence)
	}
}
