package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

var testOnce sync.Once
var testDB *db.DB
var testErr error

func database(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTOBID_TEST_DATABASE_URL not set")
	}
	testOnce.Do(func() {
		testDB, testErr = db.Open(context.Background(), dsn)
		if testErr == nil {
			testErr = testDB.Migrate(context.Background())
		}
	})
	if testErr != nil {
		t.Fatal(testErr)
	}
	return testDB
}
func fixture(t *testing.T) (*Service, string, int64) {
	t.Helper()
	pg := database(t)
	tenant := fmt.Sprintf("retrieval-%d", time.Now().UnixNano())
	doc := &db.KnowledgeDocument{TenantID: tenant, Title: "恢复服务", Category: db.KnowledgeProductSpec, RawText: "synthetic recovery spec", Extra: json.RawMessage(`{"product":"X-1","version":"v1","valid_from":"2026-01-01","valid_until":"2027-01-01"}`)}
	id, _, err := pg.InsertKnowledgeDocument(context.Background(), doc, []db.KnowledgeChunk{{Seq: 0, Content: "恢复时间：0.5分钟", SectionTitle: "恢复时间", SectionPath: "技术参数", Tables: json.RawMessage(`[]`)}, {Seq: 1, Content: "质保期：2年", SectionTitle: "质保期", Tables: json.RawMessage(`[]`)}}, db.DefaultEmbedder())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM retrieval_runs WHERE tenant_id=$1`, tenant)
		_ = pg.DeleteKnowledgeDocument(context.Background(), tenant, id)
		_, _ = pg.Exec(`DELETE FROM knowledge_indexes WHERE tenant_id=$1`, tenant)
		_, _ = pg.Exec(`DELETE FROM knowledge_catalogs WHERE tenant_id=$1`, tenant)
	})
	return New(pg, nil, nil), tenant, id
}
func TestSearchEvidenceAndSourceInvalidation(t *testing.T) {
	s, tenant, id := fixture(t)
	ctx := context.Background()
	r, err := s.Search(ctx, db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间", Limit: 1}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 || len(r.Degraded) != 2 || len(r.EvidenceIDs) != 1 {
		t.Fatalf("%+v", r)
	}
	claim := Claim{Text: "恢复时间不超过30秒", Parameter: "恢复时间", Operator: "<=", Value: 30, Unit: "秒", Product: "X-1", Version: "v1", AsOf: "2026-10-28"}
	e, err := s.Verify(ctx, tenant, "test", r.RunID, r.EvidenceIDs[0], claim)
	if err != nil || e.Relation != "supports" || e.Status != "verified" {
		t.Fatalf("%+v %v", e, err)
	}
	claim.Product = "X-2"
	e, err = s.Verify(ctx, tenant, "test", r.RunID, e.ID, claim)
	if err != nil || e.Relation != "insufficient" {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err = s.DB.GetRetrievalRun(ctx, "other", r.RunID); err == nil {
		t.Fatal("cross tenant run")
	}
	if err = s.DB.DeleteKnowledgeDocument(ctx, tenant, id); err != nil {
		t.Fatal(err)
	}
	evidence, err := s.Evidence(ctx, tenant, r.RunID)
	if err != nil || evidence[0].Status != "stale" {
		t.Fatalf("%+v %v", evidence, err)
	}
	if _, err = s.Verify(ctx, tenant, "test", r.RunID, e.ID, claim); err == nil {
		t.Fatal("stale source verified")
	}
}
func TestIndexSwitchRollbackAndStaleRejection(t *testing.T) {
	s, tenant, id := fixture(t)
	ctx := context.Background()
	s.Embedder = db.HashEmbedder{Dim: 16}
	a, err := s.BuildIndex(ctx, tenant, "test", 16)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.ActivateKnowledgeIndex(ctx, tenant, a.ID, "test"); err != nil {
		t.Fatal(err)
	}
	b, err := s.BuildIndex(ctx, tenant, "test", 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int64{b.ID, a.ID} {
		if err = s.DB.ActivateKnowledgeIndex(ctx, tenant, i, "test"); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Search(ctx, db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间"}, 0, "test")
	if err != nil || r.IndexID != a.ID {
		t.Fatalf("%+v %v", r, err)
	}
	s.Embedder = db.HashEmbedder{Dim: 32}
	r, err = s.Search(ctx, db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间"}, 0, "test")
	if err != nil || r.IndexID != 0 {
		t.Fatalf("mismatched model space used: %+v %v", r, err)
	}
	s.Embedder = db.HashEmbedder{Dim: 16}
	if _, err = s.DB.ExecContext(ctx, `UPDATE knowledge_documents SET title='changed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.ActivateKnowledgeIndex(ctx, tenant, b.ID, "test"); err == nil {
		t.Fatal("stale index activated")
	}
	r, err = s.Search(ctx, db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间"}, 0, "test")
	if err != nil || r.IndexID != 0 {
		t.Fatalf("stale index used: %+v %v", r, err)
	}
}
func TestHTTPModelRejectsMalformedAndHandlesOrder(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`, true},
		{`{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`, false},
		{`{"data":[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[0,1]}]}`, false},
		{`{"data":[]}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			m := &HTTPModel{URL: server.URL, Dimension: 2}
			v, err := m.Embed(context.Background(), []string{"a", "b"})
			if (err == nil) != tc.ok {
				t.Fatalf("%v", err)
			}
			if err == nil && v[0][0] != 1 {
				t.Fatal("index order ignored")
			}
		})
	}
}
func TestConservativeNumericVerification(t *testing.T) {
	c := Claim{Text: "恢复时间≤30秒", Parameter: "恢复时间", Operator: "<=", Value: 30, Unit: "秒", Product: "X"}
	snap := Snapshot{Document: &db.KnowledgeDocument{Category: db.KnowledgeProductSpec, Extra: json.RawMessage(`{"product":"X"}`)}, Chunk: &db.KnowledgeChunk{}}
	for _, tc := range []struct{ body, relation string }{
		{"恢复时间：0.5分钟", "supports"}, {"恢复时间：31秒", "contradicts"}, {"恢复时间：≤60秒", "insufficient"}, {"恢复时间：30秒（仅适用于实验环境）", "insufficient"}, {"不支持恢复时间：30秒", "insufficient"}, {"恢复时间：30秒\n恢复时间：60秒", "insufficient"},
	} {
		snap.Chunk.Content = tc.body
		v := VerifyNumeric(snap, c, time.Now())
		if v.Relation != tc.relation {
			t.Fatalf("%s: %+v", tc.body, v)
		}
	}
	snap.Chunk.Content = ""
	snap.Chunk.Tables = json.RawMessage(`[{"grid":[["参数","值"],["恢复时间","30秒"]]}]`)
	v := VerifyNumeric(snap, c, time.Now())
	if v.Relation != "supports" || v.Row == nil || *v.Row != 1 {
		t.Fatalf("%+v", v)
	}
	snap.Document.Category = db.KnowledgeHistoricalBid
	if VerifyNumeric(snap, c, time.Now()).Relation != "insufficient" {
		t.Fatal("historical promise verified")
	}
}
func TestGlobalBudgetAndFencedJobs(t *testing.T) {
	s, tenant, _ := fixture(t)
	ctx := context.Background()
	r, _, err := s.DB.CreateRetrievalRun(ctx, db.RetrievalRun{TenantID: tenant, RequestKey: "budget", Request: json.RawMessage(`{}`), Kind: "research", MaxCalls: 2, MaxTokens: 100, Deadline: time.Now().Add(time.Minute)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.DB.ReserveRetrieval(ctx, tenant, r.ID, 1, 50) == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 2 {
		t.Fatalf("reservations %d", success)
	}
	if err = s.DB.EnsureRetrievalJobs(ctx, tenant, r.ID, []json.RawMessage{json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.DB.RetrievalJobs(ctx, tenant, r.ID)
	j := jobs[0]
	old, err := s.DB.ClaimRetrievalJob(ctx, tenant, j.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if s.DB.FinishRetrievalRun(ctx, tenant, r.ID, "completed", nil) == nil {
		t.Fatal("aggregated running child")
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE retrieval_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.DB.ClaimRetrievalJob(ctx, tenant, j.ID, time.Minute)
	if err != nil || newer <= old {
		t.Fatal("lease not reclaimed", err)
	}
	if s.DB.FinishRetrievalJob(ctx, tenant, j.ID, old, "completed", nil) == nil {
		t.Fatal("stale lease wrote result")
	}
	if err = s.DB.FinishRetrievalJob(ctx, tenant, j.ID, newer, "completed", nil); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.FinishRetrievalRun(ctx, tenant, r.ID, "partial", nil); err != nil {
		t.Fatal(err)
	}
}

func TestBaselineAblationKeepsOriginalFusion(t *testing.T) {
	s, tenant, _ := fixture(t)
	ctx := context.Background()
	s.Embedder = db.DefaultEmbedder()
	idx, err := s.BuildBaselineIndex(ctx, tenant, "test", 256)
	if err != nil {
		t.Fatal(err)
	}
	q := db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间", Limit: 20}
	a, err := s.DB.SearchKnowledge(ctx, q, s.Embedder)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.DB.SearchKnowledgeWithIndex(ctx, q, s.Embedder, idx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatal("baseline candidate count changed")
	}
	for i := range a {
		if a[i].Chunk.ID != b[i].Chunk.ID || a[i].Score != b[i].Score {
			t.Fatal("baseline ranking changed")
		}
	}
	enhanced, err := s.BuildIndex(ctx, tenant, "test", 256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.SearchKnowledgeWithIndex(ctx, q, s.Embedder, enhanced.ID); err == nil {
		t.Fatal("baseline accepted changed preprocessing")
	}
}
