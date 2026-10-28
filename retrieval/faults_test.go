package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/autobid/autobid-engine/db"
	"sync/atomic"
	"testing"
	"time"
)

type countedRanker struct {
	calls atomic.Int32
	fail  bool
}

func (r *countedRanker) Model() string { return "test-ranker-v1" }
func (r *countedRanker) Rank(ctx context.Context, q string, texts []string) ([]float64, error) {
	r.calls.Add(1)
	if r.fail {
		return nil, fmt.Errorf("injected failure")
	}
	out := make([]float64, len(texts))
	for i := range out {
		out[i] = float64(len(out) - i)
	}
	return out, nil
}
func TestCacheIsolationAndModelFailure(t *testing.T) {
	s, tenant, _ := fixture(t)
	ctx := context.Background()
	rank := &countedRanker{}
	s.Reranker = rank
	s.Embedder = db.HashEmbedder{Dim: 16}
	idx, err := s.BuildIndex(ctx, tenant, "test", 16)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.ActivateKnowledgeIndex(ctx, tenant, idx.ID, "test"); err != nil {
		t.Fatal(err)
	}
	q := db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间", Limit: 1}
	first, err := s.Search(ctx, q, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	first.Hits[0].Chunk.Content = "corrupted caller data"
	second, err := s.Search(ctx, q, 0, "test")
	if err != nil || !second.Cached || second.Hits[0].Chunk.Content == "corrupted caller data" || rank.calls.Load() != 1 {
		t.Fatalf("%+v %v", second, err)
	}
	q.TenantID = "other-tenant"
	other, err := s.Search(ctx, q, 0, "test")
	if err != nil || len(other.Hits) != 0 {
		t.Fatalf("tenant leaked %+v %v", other, err)
	}
	t.Cleanup(func() { _, _ = s.DB.Exec(`DELETE FROM retrieval_runs WHERE id=$1`, other.RunID) })
	broken := New(s.DB, s.Embedder, &countedRanker{fail: true})
	q.TenantID = tenant
	degraded, err := broken.Search(ctx, q, 0, "test")
	if err != nil || len(degraded.Degraded) != 1 || degraded.Degraded[0] != "reranker_failed" || len(degraded.Hits) == 0 {
		t.Fatalf("%+v %v", degraded, err)
	}
}
func TestRerankerBudgetCheckedBeforeDispatch(t *testing.T) {
	s, tenant, _ := fixture(t)
	ctx := context.Background()
	rank := &countedRanker{}
	s.Reranker = rank
	run, _, err := s.DB.CreateRetrievalRun(ctx, db.RetrievalRun{TenantID: tenant, RequestKey: "no-tokens", Request: json.RawMessage(`{}`), Kind: "research", MaxCalls: 10, MaxTokens: 1, Deadline: time.Now().Add(time.Minute)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.candidates(withBudget(ctx, run), db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间", Limit: 1})
	if err != nil || rank.calls.Load() != 0 || len(result.Hits) != 1 {
		t.Fatalf("dispatch beyond budget %+v %v", result, err)
	}
}
