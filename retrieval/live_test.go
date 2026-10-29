package retrieval

import (
	"context"
	"math"
	"os"
	"testing"
)

// TestLiveModelContract calls the configured embedding and rerank services. It spends provider quota,
// so it runs only with AUTOBID_LIVE_MODEL_TEST=1 and the AUTOBID_EMBEDDING_* / AUTOBID_RERANKER_* variables set.
func TestLiveModelContract(t *testing.T) {
	if os.Getenv("AUTOBID_LIVE_MODEL_TEST") != "1" {
		t.Skip("AUTOBID_LIVE_MODEL_TEST=1 not set (calls the configured model services and spends quota)")
	}
	emb, rank, err := ModelsFromEnv()
	if err != nil || emb == nil || rank == nil {
		t.Fatalf("AUTOBID_EMBEDDING_* and AUTOBID_RERANKER_* must both be configured: %v", err)
	}
	ctx := context.Background()
	docs := []string{"系统采用同城双活架构，灾难发生时业务恢复时间不超过 30 秒。", "财务报表审计流程与凭证抽样方法。"}
	m := emb.(*HTTPModel)
	dv, err := m.Embed(ctx, docs)
	if err != nil {
		t.Fatal(err)
	}
	qv, err := m.EmbedQuery(ctx, []string{"容灾能力"})
	if err != nil {
		t.Fatal(err)
	}
	cos := func(a, b []float64) float64 {
		var dot, na, nb float64
		for i := range a {
			dot, na, nb = dot+a[i]*b[i], na+a[i]*a[i], nb+b[i]*b[i]
		}
		return dot / math.Sqrt(na*nb)
	}
	related, unrelated := cos(qv[0], dv[0]), cos(qv[0], dv[1])
	t.Logf("dimension %d; cos(容灾能力, 双活容灾)=%.3f cos(容灾能力, 审计)=%.3f", len(dv[0]), related, unrelated)
	if related <= unrelated {
		t.Fatalf("query embedding prefers the unrelated document")
	}
	scores, err := rank.Rank(ctx, "容灾能力", docs)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rerank scores %.3f / %.3f", scores[0], scores[1])
	if scores[0] <= scores[1] {
		t.Fatalf("reranker prefers the unrelated document: %v", scores)
	}
	if r, ok := rank.(*HTTPModel); ok {
		t.Logf("provider-reported tokens: embedding %d, rerank %d", m.TokensUsed(), r.TokensUsed())
	}
}
