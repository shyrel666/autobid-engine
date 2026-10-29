package retrieval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// dashScopeStub mimics the DashScope native envelopes: input/parameters in, output/usage out.
type dashScopeStub struct {
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
}

func (s *dashScopeStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		input := body["input"].(map[string]any)
		if texts, ok := input["texts"].([]any); ok {
			var out []map[string]any
			for i := len(texts) - 1; i >= 0; i-- { // reversed order: the client must place vectors by text_index
				out = append(out, map[string]any{"text_index": i, "embedding": []float64{float64(len([]rune(texts[i].(string)))), 1}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"output": map[string]any{"embeddings": out}, "usage": map[string]any{"total_tokens": 7}})
			return
		}
		docs := input["documents"].([]any)
		var results []map[string]any
		for i := len(docs) - 1; i >= 0; i-- {
			results = append(results, map[string]any{"index": i, "relevance_score": float64(i) / 10})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"output": map[string]any{"results": results}, "usage": map[string]any{"total_tokens": 5}})
	}
}

func TestDashScopeEmbeddingContract(t *testing.T) {
	stub := &dashScopeStub{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	m := &HTTPModel{URL: server.URL, Name: "emb", Revision: "r1", APIKey: "k", Dimension: 2, Format: "dashscope", MaxBatch: 2}
	v, err := m.Embed(context.Background(), []string{"a", "bb", "ccc", "dddd", "eeeee"})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{1, 2, 3, 4, 5} {
		if v[i][0] != want {
			t.Fatalf("vector %d placed out of order: %v", i, v)
		}
	}
	if len(stub.bodies) != 3 {
		t.Fatalf("MaxBatch=2 over 5 texts must send 3 requests, sent %d", len(stub.bodies))
	}
	for i, n := range []int{2, 2, 1} {
		b := stub.bodies[i]
		params := b["parameters"].(map[string]any)
		if b["model"] != "emb" || len(b["input"].(map[string]any)["texts"].([]any)) != n || params["text_type"] != "document" || params["dimension"] != float64(2) || stub.auth[i] != "Bearer k" {
			t.Fatalf("request %d = %v auth=%q", i, b, stub.auth[i])
		}
	}
	if _, err = m.EmbedQuery(context.Background(), []string{"q"}); err != nil {
		t.Fatal(err)
	}
	if tt := stub.bodies[3]["parameters"].(map[string]any)["text_type"]; tt != "query" {
		t.Fatalf("query encoded as %v", tt)
	}
	if m.TokensUsed() != 4*7 {
		t.Fatalf("provider usage = %d", m.TokensUsed())
	}
}

func TestDashScopeRerankContract(t *testing.T) {
	stub := &dashScopeStub{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	m := &HTTPModel{URL: server.URL, Name: "rank", Revision: "r1", Format: "dashscope"}
	scores, err := m.Rank(context.Background(), "q", []string{"x", "y", "z"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 3 || scores[0] != 0 || scores[2] != 0.2 {
		t.Fatalf("scores not mapped by index: %v", scores)
	}
	b := stub.bodies[0]
	in, params := b["input"].(map[string]any), b["parameters"].(map[string]any)
	if in["query"] != "q" || len(in["documents"].([]any)) != 3 || params["top_n"] != float64(3) || params["return_documents"] != false {
		t.Fatalf("rerank request = %v", b)
	}
	if scores, err = m.Rank(context.Background(), "q", nil); err != nil || len(scores) != 0 || len(stub.bodies) != 1 {
		t.Fatalf("empty rerank must not call the provider: %v %v", scores, err)
	}
}

func TestDashScopeErrorsExposeCodeOnly(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"code":"InvalidParameter","message":"batch size is invalid; key sk-secret"}`, "model HTTP 400 (InvalidParameter)"},
		{`{"code":"not a code!","message":"x"}`, "model HTTP 400"},
		{`not json`, "model HTTP 400"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(tc.body))
		}))
		m := &HTTPModel{URL: server.URL, Name: "emb", Revision: "r1", APIKey: "sk-secret", Dimension: 2, Format: "dashscope"}
		_, err := m.Embed(context.Background(), []string{"a"})
		server.Close()
		if err == nil || err.Error() != tc.want || strings.Contains(err.Error(), "secret") {
			t.Fatalf("%s: %v", tc.body, err)
		}
	}
}

func TestModelsFromEnvDashScope(t *testing.T) {
	t.Setenv("AUTOBID_EMBEDDING_URL", "https://dashscope.example/api/v1/services/embeddings/text-embedding/text-embedding")
	t.Setenv("AUTOBID_EMBEDDING_MODEL", "emb")
	t.Setenv("AUTOBID_EMBEDDING_REVISION", "r1")
	t.Setenv("AUTOBID_EMBEDDING_DIMENSION", "1024")
	t.Setenv("AUTOBID_EMBEDDING_FORMAT", "dashscope")
	t.Setenv("AUTOBID_EMBEDDING_MAX_BATCH", "")
	t.Setenv("AUTOBID_RERANKER_URL", "")
	emb, _, err := ModelsFromEnv()
	if err != nil || emb.(*HTTPModel).MaxBatch != 10 {
		t.Fatalf("dashscope default batch: %+v %v", emb, err)
	}
	t.Setenv("AUTOBID_EMBEDDING_MAX_BATCH", "20")
	if emb, _, err = ModelsFromEnv(); err != nil || emb.(*HTTPModel).MaxBatch != 20 {
		t.Fatalf("configured batch: %+v %v", emb, err)
	}
	for _, bad := range []string{"0", "x", "4096"} {
		t.Setenv("AUTOBID_EMBEDDING_MAX_BATCH", bad)
		if _, _, err = ModelsFromEnv(); err == nil {
			t.Fatalf("MAX_BATCH=%s accepted", bad)
		}
	}
	t.Setenv("AUTOBID_EMBEDDING_MAX_BATCH", "")
	t.Setenv("AUTOBID_EMBEDDING_FORMAT", "openai")
	if _, _, err = ModelsFromEnv(); err == nil {
		t.Fatal("unsupported format accepted")
	}
}
