package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/autobid/autobid-engine/db"
	"strings"
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
type sizedRanker struct{ sizes []int }

func (r *sizedRanker) Model() string { return "test-ranker-sized" }
func (r *sizedRanker) Rank(ctx context.Context, q string, texts []string) ([]float64, error) {
	r.sizes = append(r.sizes, len(texts))
	out := make([]float64, len(texts))
	for i := range out {
		out[i] = float64(i) // reverse the fused order so the reranked head is visibly re-sorted
	}
	return out, nil
}
func TestRerankHeadFitsRunBudget(t *testing.T) {
	pg := database(t)
	ctx := context.Background()
	tenant := fmt.Sprintf("retrieval-head-%d", time.Now().UnixNano())
	body := strings.Repeat("双活容灾切换与数据同步。", 100) // ~3.6 KB per chunk
	var chunks []db.KnowledgeChunk
	for i, tag := range []string{"甲", "乙", "丙"} {
		chunks = append(chunks, db.KnowledgeChunk{Seq: i, Content: "恢复时间：30秒（" + tag + "）" + body, SectionTitle: "恢复", Tables: json.RawMessage(`[]`)})
	}
	id, _, err := pg.InsertKnowledgeDocument(ctx, &db.KnowledgeDocument{TenantID: tenant, Title: "容灾方案", Category: db.KnowledgeStandardSolution, RawText: "synthetic"}, chunks, db.DefaultEmbedder())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM retrieval_runs WHERE tenant_id=$1`, tenant)
		_ = pg.DeleteKnowledgeDocument(context.Background(), tenant, id)
		_, _ = pg.Exec(`DELETE FROM knowledge_catalogs WHERE tenant_id=$1`, tenant)
	})
	rank := &sizedRanker{}
	s := New(pg, nil, rank)
	q := db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间", Limit: 3}
	full, err := s.candidates(ctx, q)
	if err != nil || len(full.Hits) != 3 || rank.sizes[0] != 3 || strings.Contains(strings.Join(full.Degraded, ","), "rerank") {
		t.Fatalf("unbudgeted rerank must cover all candidates: %+v sizes=%v %v", full.Degraded, rank.sizes, err)
	}
	// The budget covers one ~3.7 KB candidate but not two: rerank the head instead of skipping rerank entirely.
	run, _, err := pg.CreateRetrievalRun(ctx, db.RetrievalRun{TenantID: tenant, RequestKey: "head", Request: json.RawMessage(`{}`), Kind: "search", MaxCalls: 4, MaxTokens: 256 + 5500, Deadline: time.Now().Add(time.Minute)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	part, err := s.candidates(withBudget(ctx, run), q)
	if err != nil || len(part.Hits) != 3 || rank.sizes[1] != 1 || !strings.Contains(strings.Join(part.Degraded, ","), "rerank_truncated") {
		t.Fatalf("budget-bounded head: %+v sizes=%v %v", part.Degraded, rank.sizes, err)
	}
	// The stub reverses whatever it reranks: all three come back reversed, a one-item head leaves fused order intact.
	for i := range part.Hits {
		if part.Hits[i].Chunk.ID != full.Hits[2-i].Chunk.ID {
			t.Fatalf("tail must keep fused order: head-only %v vs full %v", part.Hits[i].Chunk.ID, full.Hits[2-i].Chunk.ID)
		}
	}
	if left, err := pg.RetrievalTokensLeft(ctx, tenant, run.ID); err != nil || left >= 5500 || left < 0 {
		t.Fatalf("rerank reservation not recorded: left=%d %v", left, err)
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
