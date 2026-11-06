package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
)

type s6Embedder struct {
	id    string
	texts atomic.Int64
	fn    func(context.Context, []string) ([][]float64, error)
}

func (e *s6Embedder) Model() string {
	if e.id != "" {
		return e.id
	}
	return "s6@1"
}
func (e *s6Embedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	e.texts.Add(int64(len(texts)))
	if e.fn != nil {
		return e.fn(ctx, texts)
	}
	v := make([][]float64, len(texts))
	for i := range v {
		v[i] = []float64{1, 1}
	}
	return v, nil
}

type s6Ranker struct {
	fn func(context.Context, string, []string) ([]float64, error)
}

func (*s6Ranker) Model() string { return "s6-rank@1" }
func (r *s6Ranker) Rank(ctx context.Context, q string, texts []string) ([]float64, error) {
	if r.fn != nil {
		return r.fn(ctx, q, texts)
	}
	out := make([]float64, len(texts))
	for i := range out {
		out[i] = float64(len(out) - i)
	}
	return out, nil
}

func s6Fixture(t *testing.T) (*Service, string, int64) {
	t.Helper()
	if os.Getenv("AUTOBID_TEST_DATABASE_URL") == "" {
		t.Fatal("S6 integration tests require a real PostgreSQL database")
	}
	s, tenant, id := fixture(t)
	t.Cleanup(func() {
		_, _ = s.DB.ExecContext(context.Background(), `DELETE FROM knowledge_index_events WHERE tenant_id=$1`, tenant)
	})
	return s, tenant, id
}

func s6BuildActive(t *testing.T, s *Service, tenant string) db.KnowledgeIndex {
	t.Helper()
	idx, err := s.BuildIndex(context.Background(), tenant, "s6-test", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateIndex(context.Background(), tenant, idx.ID, "s6-test"); err != nil {
		t.Fatal(err)
	}
	return idx
}

func s6Healthy() *toolhealth.Registry {
	h := toolhealth.New()
	for _, cap := range []toolhealth.Capability{toolhealth.Embedding, toolhealth.Reranker} {
		h.Record(cap, toolhealth.Result{}, 0, nil)
	}
	return h
}

func s6Get(t *testing.T, s *Service, tenant string, id int64) db.KnowledgeIndex {
	t.Helper()
	x, err := s.DB.GetKnowledgeIndex(context.Background(), tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func s6Claim(t *testing.T, s *Service, x db.KnowledgeIndex) db.KnowledgeIndex {
	t.Helper()
	claimed, err := s.DB.ClaimKnowledgeIndexBuildByID(context.Background(), x.TenantID, x.ID, "s6-test", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	return *claimed
}

func TestHungModelsDegradeWithinDeadline(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	s.Embedder = &s6Embedder{}
	s6BuildActive(t, s, tenant)
	s.Embedder = &s6Embedder{fn: func(ctx context.Context, _ []string) ([][]float64, error) { <-ctx.Done(); return nil, ctx.Err() }}
	s.Reranker = &s6Ranker{fn: func(ctx context.Context, _ string, _ []string) ([]float64, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s.ChannelTimeout = 35 * time.Millisecond
	start := time.Now()
	r, err := s.Search(context.Background(), db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间"}, 0, "s6-test")
	if err != nil || len(r.Hits) == 0 || !slices.Contains(r.Degraded, "semantic_channel_failed") || !slices.Contains(r.Degraded, "reranker_failed") {
		t.Fatalf("lexical fallback: %+v %v", r, err)
	}
	if time.Since(start) > 2*time.Second || r.Diagnostics.Mode != "lexical_only" || r.Diagnostics.Failures["embedding"] == "" || r.Diagnostics.Failures["reranker"] == "" {
		t.Fatalf("deadline/diagnostics: %+v", r.Diagnostics)
	}
}

func TestRuntimeHealthDegradeAndRecover(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	s.Embedder = &s6Embedder{}
	s6BuildActive(t, s, tenant)
	s.Health = s6Healthy()
	var failed atomic.Bool
	failed.Store(true)
	e := &s6Embedder{fn: func(_ context.Context, texts []string) ([][]float64, error) {
		if failed.Load() {
			return nil, errors.New(`POST https://private-model.example/secret?api_key=secret failed C:\private\model.bin`)
		}
		out := make([][]float64, len(texts))
		for i := range out {
			out[i] = []float64{1, 1}
		}
		return out, nil
	}}
	rank := &s6Ranker{fn: func(_ context.Context, _ string, texts []string) ([]float64, error) {
		if failed.Load() {
			return nil, errors.New("model unavailable")
		}
		return make([]float64, len(texts)), nil
	}}
	s.Embedder, s.Reranker = e, rank
	q := db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间"}
	r, err := s.Search(context.Background(), q, 0, "test")
	if err != nil || len(r.Hits) == 0 {
		t.Fatal(err)
	}
	for _, cap := range []toolhealth.Capability{toolhealth.Embedding, toolhealth.Reranker} {
		if s.Health.Get(cap).State != toolhealth.Degraded {
			t.Fatal(s.Health.Get(cap))
		}
	}
	if strings.Contains(r.Diagnostics.Failures["embedding"], "private") || strings.Contains(r.Diagnostics.Failures["embedding"], "secret") {
		t.Fatal("private model details leaked")
	}
	failed.Store(false)
	if _, err = s.Search(context.Background(), q, 0, "test"); err != nil {
		t.Fatal(err)
	}
	for _, cap := range []toolhealth.Capability{toolhealth.Embedding, toolhealth.Reranker} {
		if s.Health.Get(cap).State != toolhealth.Healthy {
			t.Fatal(s.Health.Get(cap))
		}
	}
	// A malformed vector is a provider failure; caller cancellation is not.
	s.mu.Lock()
	s.cache = map[string]cacheEntry{}
	s.mu.Unlock()
	e.fn = func(context.Context, []string) ([][]float64, error) { return [][]float64{{0, 0}}, nil }
	if _, err = s.Search(context.Background(), q, 0, "test"); err != nil || s.Health.Get(toolhealth.Embedding).State != toolhealth.Degraded {
		t.Fatalf("vector validation did not degrade: %v", err)
	}
	s.Health.Recover(toolhealth.Embedding)
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	s.modelHealth(cancelCtx, toolhealth.Embedding, context.Canceled)
	if s.Health.Get(toolhealth.Embedding).State != toolhealth.Healthy {
		t.Fatal("caller cancellation degraded health")
	}
}

func TestBudgetExhaustionDoesNotDegradeHealth(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	e := &s6Embedder{}
	s.Embedder = e
	s6BuildActive(t, s, tenant)
	s.Health, s.Reranker = s6Healthy(), &s6Ranker{}
	before := e.texts.Load()
	run, _, err := s.DB.CreateRetrievalRun(context.Background(), db.RetrievalRun{TenantID: tenant, RequestKey: "s6-budget", Request: json.RawMessage(`{}`), Kind: "search", MaxCalls: 1, MaxTokens: 1, Deadline: time.Now().Add(time.Minute)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.candidates(withBudget(context.Background(), run), db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间", Limit: 5})
	if err != nil || len(r.Hits) == 0 || e.texts.Load() != before || r.Diagnostics.Failures["embedding"] != "budget" || r.Diagnostics.Failures["reranker"] != "budget" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, cap := range []toolhealth.Capability{toolhealth.Embedding, toolhealth.Reranker} {
		if s.Health.Get(cap).State != toolhealth.Healthy {
			t.Fatal("budget degraded health")
		}
	}
}

func TestFilteredSemanticSearchRemainsHybrid(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	e := &s6Embedder{}
	s.Embedder, s.Reranker = e, &s6Ranker{}
	idx := s6BuildActive(t, s, tenant)
	before := e.texts.Load()
	r, err := s.Search(context.Background(), db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间", MustTerms: []string{"absent-filter-term"}}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if e.texts.Load() != before+1 || r.IndexID != idx.ID || r.Diagnostics.Mode != "hybrid" || r.Diagnostics.IndexState != IndexStateCurrent || r.Total != 0 || len(r.Hits) != 0 || len(r.Degraded) != 0 || len(r.Diagnostics.Failures) != 0 {
		t.Fatalf("successful empty semantic channel: %+v, embeds=%d", r, e.texts.Load()-before)
	}
}

func TestDimensionChangeSkipsEmbedding(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Inputs []string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		v := make([][]float64, len(req.Inputs))
		for i := range v {
			v[i] = []float64{1, 1}
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	defer srv.Close()
	m := &HTTPModel{URL: srv.URL, Name: "s6", Revision: "1", Dimension: 2, Format: "tei"}
	s.Embedder = m
	s6BuildActive(t, s, tenant)
	before := calls.Load()
	m.Dimension = 3
	r, err := s.Search(context.Background(), db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间"}, 0, "test")
	if err != nil || calls.Load() != before || r.Diagnostics.IndexState != IndexStateDimensionChanged || r.IndexID != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestModelSwitchNeverQueriesForeignIndex(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	s.Embedder = &s6Embedder{}
	s6BuildActive(t, s, tenant)
	foreign := &s6Embedder{id: "foreign@2"}
	s.Embedder = foreign
	r, err := s.Search(context.Background(), db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间"}, 0, "test")
	if err != nil || r.IndexID != 0 || r.Diagnostics.Mode != "lexical_only" || r.Diagnostics.IndexState != IndexStateModelChanged || foreign.texts.Load() != 0 || len(r.Hits) != 1 {
		t.Fatalf("foreign index queried: %+v %v", r, err)
	}
}

func s6AddChunks(t *testing.T, s *Service, tenant string, n int) int64 {
	t.Helper()
	chunks := make([]db.KnowledgeChunk, n)
	for i := range chunks {
		chunks[i] = db.KnowledgeChunk{Seq: i, Content: fmt.Sprintf("恢复时间：30秒 新增分块%d", i), Tables: json.RawMessage(`[]`)}
	}
	id, _, err := s.DB.InsertKnowledgeDocument(context.Background(), &db.KnowledgeDocument{TenantID: tenant, Title: "新增文档", Category: db.KnowledgeProductSpec, RawText: fmt.Sprintf("s6-%d", time.Now().UnixNano())}, chunks, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DB.DeleteKnowledgeDocument(context.Background(), tenant, id) })
	return id
}

func TestStaleDocumentsDegradeAndRebuildReuses(t *testing.T) {
	s, tenant, id := s6Fixture(t)
	e := &s6Embedder{}
	s.Embedder = e
	a := s6BuildActive(t, s, tenant)
	if e.texts.Load() != 2 {
		t.Fatal("initial chunks missing")
	}
	s6AddChunks(t, s, tenant, 1)
	status, err := s.Status(context.Background(), tenant)
	if err != nil || status.IndexState != IndexStateDocsChanged || status.State != "lexical_only" {
		t.Fatalf("%+v %v", status, err)
	}
	if r, err := s.Search(context.Background(), db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间"}, 0, "test"); err != nil || r.IndexID != 0 || e.texts.Load() != 2 {
		t.Fatalf("stale query: %+v %v", r, err)
	}
	b, err := s.BuildIndex(context.Background(), tenant, "test", 2)
	if err != nil || b.EmbeddedChunks != 1 || b.ReusedChunks != 2 || b.ReusedFrom == nil || *b.ReusedFrom != a.ID || e.texts.Load() != 3 {
		t.Fatalf("reuse: %+v %v", b, err)
	}
	if _, err = s.DB.ExecContext(context.Background(), `UPDATE knowledge_chunks SET content='恢复时间：10秒' WHERE doc_id=$1 AND seq=0`, id); err != nil {
		t.Fatal(err)
	}
	c, err := s.BuildIndex(context.Background(), tenant, "test", 2)
	if err != nil || c.EmbeddedChunks != 1 || c.ReusedChunks != 2 || e.texts.Load() != 4 {
		t.Fatalf("changed chunk: %+v %v", c, err)
	}
	s.Embedder = &s6Embedder{id: "new-model@2"}
	d, err := s.BuildIndex(context.Background(), tenant, "test", 2)
	if err != nil || d.EmbeddedChunks != 3 || d.ReusedChunks != 0 || d.ReusedFrom != nil {
		t.Fatalf("model reuse: %+v %v", d, err)
	}
}

func TestIndexRebuildReusesFailedBuildEmbeddings(t *testing.T) {
	for _, differentModel := range []bool{false, true} {
		t.Run(fmt.Sprint("different-model-", differentModel), func(t *testing.T) {
			s, tenant, _ := s6Fixture(t)
			s6AddChunks(t, s, tenant, 63)
			failure := errors.New("second embedding batch failed")
			calls := 0
			e := &s6Embedder{fn: func(_ context.Context, texts []string) ([][]float64, error) {
				calls++
				if calls == 2 {
					return nil, failure
				}
				vectors := make([][]float64, len(texts))
				for i := range vectors {
					vectors[i] = []float64{1, 1}
				}
				return vectors, nil
			}}
			s.Embedder = e
			failed, err := s.BuildIndex(context.Background(), tenant, "test", 2)
			if !errors.Is(err, failure) || failed.Status != "failed" || failed.ErrorCode != "MODEL_ERROR" || failed.EmbeddedChunks != 32 || failed.ReusedFrom != nil {
				t.Fatalf("partial build: %+v %v", failed, err)
			}
			before := e.texts.Load()
			e.fn = nil
			wantEmbedded, wantReused := 33, 32
			if differentModel {
				e = &s6Embedder{id: "different-model@2"}
				s.Embedder = e
				before, wantEmbedded, wantReused = 0, 65, 0
			}
			rebuilt, err := s.BuildIndex(context.Background(), tenant, "test", 2)
			if err != nil || rebuilt.Status != "ready" || rebuilt.EmbeddedChunks != wantEmbedded || rebuilt.ReusedChunks != wantReused || rebuilt.ReusedFrom != nil || e.texts.Load()-before != int64(wantEmbedded) {
				t.Fatalf("rebuild: %+v %v, embeds=%d", rebuilt, err, e.texts.Load()-before)
			}
			if err = s.ActivateIndex(context.Background(), tenant, rebuilt.ID, "test"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIndexRunnerDoesNotClaimOtherTenants(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	other, otherTenant, _ := s6Fixture(t)
	s.Embedder, other.Embedder = &s6Embedder{}, &s6Embedder{}
	// Queue the foreign job first so an unscoped runner would claim it first.
	foreign, _, err := other.SubmitIndexBuild(context.Background(), IndexBuildRequest{TenantID: otherTenant, Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	own, _, err := s.SubmitIndexBuild(context.Background(), IndexBuildRequest{TenantID: tenant, Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := &IndexRunner{Service: s, TenantID: tenant, Poll: time.Hour, OnChange: func(x *db.KnowledgeIndex) {
		if x.ID == own.ID && x.Status == "ready" {
			cancel()
		}
	}}
	runner.init()
	runner.run(ctx)
	if got := s6Get(t, s, tenant, own.ID); got.Status != "ready" {
		t.Fatalf("own job not completed: %+v", got)
	}
	if got := s6Get(t, s, otherTenant, foreign.ID); got.Status != "queued" || got.Attempts != 0 || got.LeaseToken != 0 || got.LeaseOwner != "" {
		t.Fatalf("runner claimed a foreign job: %+v", got)
	}
}

func TestIndexRunnerResumesAfterCrash(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	e := &s6Embedder{}
	s.Embedder = e
	s6AddChunks(t, s, tenant, 63)
	x, _, err := s.SubmitIndexBuild(context.Background(), IndexBuildRequest{TenantID: tenant, Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	x = s6Claim(t, s, x)
	ctx, cancel := context.WithCancel(context.Background())
	runner := &IndexRunner{Service: s, TenantID: tenant, OnChange: func(progress *db.KnowledgeIndex) {
		if progress.EmbeddedChunks == 32 {
			cancel()
		}
	}}
	if err = runner.execute(ctx, x, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("simulated shutdown: %v", err)
	}
	interrupted := s6Get(t, s, tenant, x.ID)
	if interrupted.Status != "building" || interrupted.EmbeddedChunks != 32 || interrupted.CursorChunkID == 0 || interrupted.FinishedAt != nil {
		t.Fatalf("%+v", interrupted)
	}
	if _, err = s.DB.ExecContext(context.Background(), `UPDATE knowledge_indexes SET lease_until=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2`, tenant, x.ID); err != nil {
		t.Fatal(err)
	}
	resumed := s6Claim(t, s, x)
	if resumed.LeaseToken <= x.LeaseToken || resumed.Attempts != 2 || resumed.CursorChunkID != interrupted.CursorChunkID {
		t.Fatalf("%+v", resumed)
	}
	if err = (&IndexRunner{Service: s, TenantID: tenant}).execute(context.Background(), resumed, false); err != nil {
		t.Fatal(err)
	}
	final := s6Get(t, s, tenant, x.ID)
	if final.Status != "ready" || final.EmbeddedChunks != 65 || e.texts.Load() != 65 {
		t.Fatalf("repeated paid work: %+v embeds=%d", final, e.texts.Load())
	}
	if err = s.DB.FinishKnowledgeIndexBuild(context.Background(), x, nil, ""); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatal("crashed writer was not fenced", err)
	}
}

func TestIndexRunnerTransientRetryWithinBudget(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	s.Embedder = &s6Embedder{fn: func(context.Context, []string) ([][]float64, error) {
		return nil, fmt.Errorf("%w: loading", ErrModelTransient)
	}}
	s.Health = s6Healthy()
	x, _, err := s.SubmitIndexBuild(context.Background(), IndexBuildRequest{TenantID: tenant, Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	deadline := *x.DeadlineAt
	for attempt := 1; attempt <= 3; attempt++ {
		claimed := s6Claim(t, s, x)
		if err = (&IndexRunner{Service: s, TenantID: tenant, RetryDelay: time.Millisecond}).execute(context.Background(), claimed, false); !errors.Is(err, ErrModelTransient) {
			t.Fatal(err)
		}
		stored := s6Get(t, s, tenant, x.ID)
		if stored.Status != "queued" || stored.Attempts != attempt || !stored.DeadlineAt.Equal(deadline) || stored.RetryAfter == nil {
			t.Fatalf("retry reset budget: %+v", stored)
		}
		if _, err = s.DB.ExecContext(context.Background(), `UPDATE knowledge_indexes SET retry_after=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2`, tenant, x.ID); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := s.DB.ClaimKnowledgeIndexBuildByID(context.Background(), tenant, x.ID, "next", time.Minute)
	stored := s6Get(t, s, tenant, x.ID)
	if err != nil || claimed != nil || stored.Status != "failed" || stored.ErrorCode != "BUDGET_EXHAUSTED" || stored.Error == "" || !stored.DeadlineAt.Equal(deadline) || s.Health.Get(toolhealth.Embedding).State != toolhealth.Degraded {
		t.Fatalf("%+v %v", stored, err)
	}
	// A running request also keeps the original deadline and terminalizes at expiry.
	s.IndexBuildDeadline = 40 * time.Millisecond
	s.Embedder = &s6Embedder{fn: func(ctx context.Context, _ []string) ([][]float64, error) { <-ctx.Done(); return nil, ctx.Err() }}
	y, _, err := s.SubmitIndexBuild(context.Background(), IndexBuildRequest{TenantID: tenant, Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	y = s6Claim(t, s, y)
	_ = (&IndexRunner{Service: s, TenantID: tenant}).execute(context.Background(), y, false)
	if final := s6Get(t, s, tenant, y.ID); final.ErrorCode != "BUDGET_EXHAUSTED" || final.Status != "failed" {
		t.Fatalf("deadline: %+v", final)
	}
}

func TestIndexBuildHealthOnEmbeddingInterruption(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantErr    error
		wantStatus string
		wantCode   string
		wantHealth toolhealth.State
	}{
		{"cancelled", db.ErrLeaseLost, "cancelled", "CANCELLED", toolhealth.Healthy},
		{"lease_lost", db.ErrLeaseLost, "building", "", toolhealth.Healthy},
		{"build_deadline", context.DeadlineExceeded, "failed", "BUDGET_EXHAUSTED", toolhealth.Healthy},
		{"model_timeout", ErrModelTransient, "queued", "", toolhealth.Degraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, tenant, _ := s6Fixture(t)
			s.Health = s6Healthy()
			if tc.name == "build_deadline" {
				s.IndexBuildDeadline = 500 * time.Millisecond
			}
			entered := make(chan struct{})
			s.Embedder = &s6Embedder{fn: func(ctx context.Context, _ []string) ([][]float64, error) {
				close(entered)
				if tc.name == "model_timeout" {
					request, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
					defer cancel()
					<-request.Done()
					return nil, fmt.Errorf("%w: %v", ErrModelTransient, request.Err())
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			x, _, err := s.SubmitIndexBuild(context.Background(), IndexBuildRequest{TenantID: tenant, Dimension: 2})
			if err != nil {
				t.Fatal(err)
			}
			x = s6Claim(t, s, x)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				done <- (&IndexRunner{Service: s, TenantID: tenant, Lease: time.Second}).execute(ctx, x, false)
			}()
			defer func() {
				cancel()
				<-finished
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("embed never started")
			}
			switch tc.name {
			case "cancelled":
				err = s.DB.CancelKnowledgeIndexBuild(ctx, tenant, x.ID, "test")
			case "lease_lost":
				_, err = s.DB.ExecContext(ctx, `UPDATE knowledge_indexes SET lease_token=lease_token+1 WHERE tenant_id=$1 AND id=$2`, tenant, x.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("build error = %v, want %v", err, tc.wantErr)
				}
			case <-ctx.Done():
				t.Fatal("interrupted embed did not finish")
			}
			if got := s6Get(t, s, tenant, x.ID); got.Status != tc.wantStatus || got.ErrorCode != tc.wantCode || got.EmbeddedChunks != 0 {
				t.Fatalf("build outcome: %+v", got)
			}
			if got := s.Health.Get(toolhealth.Embedding); got.State != tc.wantHealth {
				t.Fatalf("health = %+v, want %s", got, tc.wantHealth)
			}
		})
	}
}

func TestIndexBuildCancelFencesLateWrites(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	s.Embedder = &s6Embedder{fn: func(_ context.Context, texts []string) ([][]float64, error) {
		close(entered)
		<-release
		v := make([][]float64, len(texts))
		for i := range v {
			v[i] = []float64{1, 1}
		}
		return v, nil
	}}
	x, _, err := s.SubmitIndexBuild(context.Background(), IndexBuildRequest{TenantID: tenant, Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	x = s6Claim(t, s, x)
	done := make(chan error, 1)
	go func() { done <- (&IndexRunner{Service: s, TenantID: tenant}).execute(context.Background(), x, false) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("embed never started")
	}
	if err = s.DB.CancelKnowledgeIndexBuild(context.Background(), tenant, x.ID, "cancel-actor"); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err = <-done:
		if !errors.Is(err, db.ErrLeaseLost) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not finish")
	}
	if err = s.DB.HeartbeatKnowledgeIndexBuild(context.Background(), x, time.Minute); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatal(err)
	}
	if err = s.DB.FinishKnowledgeIndexBuild(context.Background(), x, nil, ""); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM knowledge_embeddings WHERE index_id=$1`, x.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("late vectors persisted: %d %v", count, err)
	}
	if final := s6Get(t, s, tenant, x.ID); final.Status != "cancelled" || final.EmbeddedChunks != 0 {
		t.Fatalf("%+v", final)
	}
}

func TestIndexBuildIdempotency(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	s.Embedder = &s6Embedder{}
	req := IndexBuildRequest{TenantID: tenant, RequestKey: "same", Dimension: 2}
	x, replay, err := s.SubmitIndexBuild(context.Background(), req)
	if err != nil || replay {
		t.Fatal(err)
	}
	for _, terminal := range []bool{false, true} {
		if terminal {
			claimed := s6Claim(t, s, x)
			if err = (&IndexRunner{Service: s, TenantID: tenant}).execute(context.Background(), claimed, false); err != nil {
				t.Fatal(err)
			}
		}
		y, replayed, err := s.SubmitIndexBuild(context.Background(), req)
		if err != nil || !replayed || y.ID != x.ID || !y.DeadlineAt.Equal(*x.DeadlineAt) {
			t.Fatalf("replay: %+v %v", y, err)
		}
		bad := req
		bad.Dimension = 3
		if _, _, err = s.SubmitIndexBuild(context.Background(), bad); !errors.Is(err, db.ErrIdempotencyKeyConflict) {
			t.Fatal("same key changed input accepted", err)
		}
		if !terminal {
			other := req
			other.RequestKey = "other"
			if _, _, err = s.SubmitIndexBuild(context.Background(), other); !errors.Is(err, db.ErrIndexBuildActive) {
				t.Fatal("second build accepted", err)
			}
		}
	}
	// Cancellation does not release the idempotency key.
	req.RequestKey = "cancelled"
	y, _, err := s.SubmitIndexBuild(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.CancelKnowledgeIndexBuild(context.Background(), tenant, y.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if got, replayed, err := s.SubmitIndexBuild(context.Background(), req); err != nil || !replayed || got.Status != "cancelled" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRunnerRefusesChangedModel(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	s.Embedder = &s6Embedder{}
	x, _, err := s.SubmitIndexBuild(context.Background(), IndexBuildRequest{TenantID: tenant, Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	newModel := &s6Embedder{id: "changed@2"}
	s.Embedder = newModel
	claimed := s6Claim(t, s, x)
	if err = (&IndexRunner{Service: s, TenantID: tenant}).execute(context.Background(), claimed, false); err == nil {
		t.Fatal("changed model executed")
	}
	if got := s6Get(t, s, tenant, x.ID); got.Status != "failed" || got.ErrorCode != "RUNTIME_UNAVAILABLE" || newModel.texts.Load() != 0 {
		t.Fatalf("%+v", got)
	}
}

func activationCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *db.IndexActivationError
	if !errors.As(err, &e) || e.Code != want || !errors.Is(err, db.ErrRetrievalConflict) {
		t.Fatalf("activation = %v, want %s", err, want)
	}
}

func TestActivationGuards(t *testing.T) {
	s, tenant, id := s6Fixture(t)
	e := &s6Embedder{}
	s.Embedder = e
	a, err := s.BuildIndex(context.Background(), tenant, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	s.Embedder = nil
	activationCode(t, s.ActivateIndex(context.Background(), tenant, a.ID, "test"), "model_unconfigured")
	s.Embedder = &s6Embedder{id: "other@2"}
	activationCode(t, s.ActivateIndex(context.Background(), tenant, a.ID, "test"), "model_mismatch")
	s.Embedder = &HTTPModel{Name: "s6", Revision: "1", Dimension: 3}
	activationCode(t, s.ActivateIndex(context.Background(), tenant, a.ID, "test"), "dimension_mismatch")
	s.Embedder = e
	baseline, err := s.BuildBaselineIndex(context.Background(), tenant, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	activationCode(t, s.ActivateIndex(context.Background(), tenant, baseline.ID, "test"), "preprocessing_mismatch")
	b, err := s.BuildIndex(context.Background(), tenant, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int64{a.ID, b.ID, a.ID} {
		if err = s.ActivateIndex(context.Background(), tenant, idx, "operator"); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.DB.KnowledgeIndexEvents(context.Background(), tenant, 200)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Event != "rolled_back" || events[0].FromIndexID == nil || *events[0].FromIndexID != b.ID {
		t.Fatalf("%+v", events)
	}
	if err = s.ActivateIndex(context.Background(), tenant, a.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	again, _ := s.DB.KnowledgeIndexEvents(context.Background(), tenant, 200)
	if len(again) != len(events) {
		t.Fatal("repeat activation created an event")
	}
	if _, err = s.DB.ExecContext(context.Background(), `DELETE FROM knowledge_embeddings WHERE tenant_id=$1 AND index_id=$2`, tenant, b.ID); err != nil {
		t.Fatal(err)
	}
	activationCode(t, s.ActivateIndex(context.Background(), tenant, b.ID, "test"), "coverage_incomplete")
	if _, err = s.DB.ExecContext(context.Background(), `UPDATE knowledge_documents SET title='changed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	activationCode(t, s.ActivateIndex(context.Background(), tenant, b.ID, "test"), "documents_changed")
	queued, _, err := s.SubmitIndexBuild(context.Background(), IndexBuildRequest{TenantID: tenant, Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	activationCode(t, s.ActivateIndex(context.Background(), tenant, queued.ID, "test"), "not_ready")
}

func TestRetrievalStatusStates(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	status, err := s.Status(context.Background(), tenant+"-empty")
	if err != nil || status.State != "empty" || len(status.Reasons) != 0 {
		t.Fatalf("%+v %v", status, err)
	}
	status, err = s.Status(context.Background(), tenant)
	if err != nil || status.State != "lexical_only" || status.IndexState != "unconfigured" || len(status.Reasons) < 2 || status.Reasons[0] != "semantic_model_unconfigured" {
		t.Fatalf("%+v %v", status, err)
	}
	s.Embedder = &s6Embedder{}
	status, _ = s.Status(context.Background(), tenant)
	if status.IndexState != "missing" {
		t.Fatal(status)
	}
	a := s6BuildActive(t, s, tenant)
	s.Reranker = &s6Ranker{}
	status, _ = s.Status(context.Background(), tenant)
	if status.State != "semantic" || status.Embedding.Info.ID != "s6@1" {
		t.Fatal(status)
	}
	b, err := s.BuildIndex(context.Background(), tenant, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	status, _ = s.Status(context.Background(), tenant)
	if status.Indexes[0].ID != b.ID || status.Indexes[0].State != "rollback_ready" || !status.Indexes[0].CanActivate || !status.Indexes[0].CanDelete || status.ActiveIndex.ID != a.ID {
		t.Fatalf("%+v", status)
	}
	s.Health = s6Healthy()
	for _, state := range []toolhealth.State{toolhealth.Starting, toolhealth.SelfTestFailed, toolhealth.Degraded} {
		s.Health = s6Healthy()
		switch state {
		case toolhealth.Starting:
			s.Health = toolhealth.New()
		case toolhealth.SelfTestFailed:
			s.Health.Record(toolhealth.Embedding, toolhealth.Result{}, 0, errors.New("self-test failed"))
		case toolhealth.Degraded:
			s.Health.Degrade(toolhealth.Embedding, errors.New("transport failed"))
		}
		status, err = s.Status(context.Background(), tenant)
		if err != nil || status.State != "degraded" || status.Reasons[0] != "embedding_"+string(state) {
			t.Fatalf("%+v %v", status, err)
		}
	}
	s.Health = s6Healthy()
	s.Reranker = nil
	status, _ = s.Status(context.Background(), tenant)
	if status.State != "degraded" || !slices.Contains(status.Reasons, "reranker_unconfigured") {
		t.Fatal(status)
	}
	// Compatibility precedence is deterministic and never contacts a model.
	s.Embedder = &HTTPModel{Name: "s6", Revision: "1", Dimension: 2}
	idx := a
	idx.Epoch = 5
	for _, tc := range []struct {
		model string
		dim   int
		prep  string
		epoch int64
		want  string
	}{
		{"other", 3, "old", 4, IndexStateModelChanged}, {"s6@1", 3, "old", 4, IndexStateDimensionChanged}, {"s6@1", 2, "old", 4, IndexStatePrepChanged}, {"s6@1", 2, Preprocessing, 4, IndexStateDocsChanged}, {"s6@1", 2, Preprocessing, 5, IndexStateCurrent},
	} {
		idx.Model, idx.Dimension, idx.Preprocessing, idx.Epoch = tc.model, tc.dim, tc.prep, tc.epoch
		if got := s.IndexState(&idx, 5); got != tc.want {
			t.Fatalf("%s != %s", got, tc.want)
		}
	}
}

func TestUsageRecordedOnRun(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	stub := &dashScopeStub{}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	s.Embedder = &HTTPModel{URL: srv.URL, Name: "emb", Revision: "1", Dimension: 2, Format: "dashscope"}
	s.Reranker = &HTTPModel{URL: srv.URL, Name: "rank", Revision: "1", Format: "dashscope"}
	s6BuildActive(t, s, tenant)
	var wg sync.WaitGroup
	for _, q := range []string{"恢复时间", "质保期"} {
		wg.Add(1)
		go func(q string) {
			defer wg.Done()
			r, err := s.Search(context.Background(), db.KnowledgeQuery{TenantID: tenant, Text: q}, 0, "test")
			if err != nil {
				t.Error(err)
				return
			}
			run, err := s.DB.GetRetrievalRun(context.Background(), tenant, r.RunID)
			if err != nil || run.ActualTokens != 12 || r.Diagnostics.Mode != "hybrid" {
				t.Errorf("usage: %+v %v", run, err)
			}
		}(q)
	}
	wg.Wait()
}

func TestMaxRerankIsNotDegraded(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	s.Embedder = &s6Embedder{}
	s6BuildActive(t, s, tenant)
	rank := &sizedRanker{}
	s.Reranker = rank
	s.MaxRerank = 1
	q := db.KnowledgeQuery{TenantID: tenant, Text: "恢复时间"}
	r, err := s.Search(context.Background(), q, 0, "test")
	if err != nil || r.Diagnostics.FusedCandidates != 2 || r.Diagnostics.RerankedCandidates != 1 || slices.Contains(r.Degraded, "rerank_truncated") || len(r.Degraded) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	s.MaxRerank = 2
	r, err = s.Search(context.Background(), q, 0, "test")
	if err != nil || r.Cached || r.Diagnostics.RerankedCandidates != 2 || len(rank.sizes) != 2 {
		t.Fatalf("cache ignored cap: %+v %v", r, err)
	}
}

func TestResearchDiagnosticsAndDegradedDedup(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	c := &Coordinator{Service: s, MultiEnabled: true, Reasoner: &fakeReasoner{plan: `{"facets":[{"query":"恢复时间","domain":"knowledge","role":"first"},{"query":"质保期","domain":"knowledge","role":"second"}]}`, decision: `{"next_query":"","gap":"review"}`}}
	r, err := c.Run(context.Background(), tenant, 0, "test", ResearchRequest{Key: "s6-diagnostics", Question: "恢复和质保", Multi: true})
	if err != nil || r.Diagnostics == nil || r.Diagnostics.Mode != "lexical_only" || len(r.Degraded) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, j := range r.Jobs[1:] {
		var payload struct {
			Diagnostics *Diagnostics `json:"diagnostics"`
		}
		if json.Unmarshal(j.Result, &payload) != nil || payload.Diagnostics == nil {
			t.Fatal("job diagnostics missing")
		}
	}
}

func TestSynchronousIndexCancellationIsInterrupted(t *testing.T) {
	s, tenant, _ := s6Fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Embedder = &s6Embedder{fn: func(context.Context, []string) ([][]float64, error) { cancel(); return nil, context.Canceled }}
	x, err := s.BuildIndex(ctx, tenant, "test", 2)
	if !errors.Is(err, context.Canceled) || x.Status != "failed" || x.ErrorCode != "INTERRUPTED" || !strings.HasPrefix(x.RequestKey, "sys:index:") {
		t.Fatalf("%+v %v", x, err)
	}
}

func TestDocumentsChangingDuringBuildRemainReady(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint("deleted-", deleted), func(t *testing.T) {
			s, tenant, id := s6Fixture(t)
			s.Embedder = &s6Embedder{fn: func(ctx context.Context, texts []string) ([][]float64, error) {
				statement := `UPDATE knowledge_chunks SET content='changed during embedding' WHERE doc_id=$1 AND seq=0`
				if deleted {
					statement = `DELETE FROM knowledge_chunks WHERE doc_id=$1 AND seq=0`
				}
				if _, err := s.DB.ExecContext(ctx, statement, id); err != nil {
					return nil, err
				}
				vectors := make([][]float64, len(texts))
				for i := range vectors {
					vectors[i] = []float64{1, 1}
				}
				return vectors, nil
			}}
			x, err := s.BuildIndex(context.Background(), tenant, "test", 2)
			if err != nil || x.Status != "ready" {
				t.Fatalf("changing documents changed build outcome: %+v %v", x, err)
			}
			activationCode(t, s.ActivateIndex(context.Background(), tenant, x.ID, "test"), "documents_changed")
		})
	}
}

// Real tender tables contain "<", ">" and "&". The cached candidate payload goes through a JSON round trip that
// escapes them inside RawMessage, so evidence capture must compare JSON values rather than bytes.
func TestSearchCapturesTableWithEscapableCharacters(t *testing.T) {
	pg := database(t)
	ctx := context.Background()
	tenant := fmt.Sprintf("retrieval-escape-%d", time.Now().UnixNano())
	tables := json.RawMessage(`[{"rows":[["响应时间","<= 30 秒 & 7×24 值守"]],"headers":["指标<单位>","要求"]}]`)
	doc := &db.KnowledgeDocument{TenantID: tenant, Title: "运维方案", Category: db.KnowledgeStandardSolution, RawText: "synthetic", Extra: json.RawMessage(`{"product":"A&B<C>"}`)}
	id, _, err := pg.InsertKnowledgeDocument(ctx, doc, []db.KnowledgeChunk{{Seq: 0, Content: "响应时间要求", SectionTitle: "运维", Tables: tables}}, db.DefaultEmbedder())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM retrieval_runs WHERE tenant_id=$1`, tenant)
		_ = pg.DeleteKnowledgeDocument(context.Background(), tenant, id)
		_, _ = pg.Exec(`DELETE FROM knowledge_catalogs WHERE tenant_id=$1`, tenant)
	})
	r, err := New(pg, nil, nil).Search(ctx, db.KnowledgeQuery{TenantID: tenant, Text: "响应时间", Limit: 5}, 0, "test")
	if err != nil {
		t.Fatalf("search must capture evidence for a table with escapable characters: %v", err)
	}
	if r.Total != 1 || len(r.EvidenceIDs) != 1 {
		t.Fatalf("%+v", r)
	}
}

// Real chunks are large enough that one rerank head can fill a research reservation, which used to starve every
// follow-up call. A research run keeps at least half of its remaining budget; a plain search may use all of it.
func TestResearchRerankLeavesBudgetForFollowUp(t *testing.T) {
	pg := database(t)
	ctx := context.Background()
	tenant := fmt.Sprintf("retrieval-share-%d", time.Now().UnixNano())
	body := strings.Repeat("运维响应与故障处理要求。", 110) // ~4 KB per chunk
	var chunks []db.KnowledgeChunk
	for i := 0; i < 20; i++ {
		chunks = append(chunks, db.KnowledgeChunk{Seq: i, Content: fmt.Sprintf("响应时间：%d 分钟。", i) + body, SectionTitle: "运维", Tables: json.RawMessage(`[]`)})
	}
	id, _, err := pg.InsertKnowledgeDocument(ctx, &db.KnowledgeDocument{TenantID: tenant, Title: "运维方案", Category: db.KnowledgeStandardSolution, RawText: "synthetic"}, chunks, db.DefaultEmbedder())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM retrieval_runs WHERE tenant_id=$1`, tenant)
		_ = pg.DeleteKnowledgeDocument(context.Background(), tenant, id)
		_, _ = pg.Exec(`DELETE FROM knowledge_catalogs WHERE tenant_id=$1`, tenant)
	})
	s := New(pg, nil, &sizedRanker{})
	q := db.KnowledgeQuery{TenantID: tenant, Text: "响应时间", Limit: 5}
	for _, c := range []struct {
		kind     string
		wantHalf bool
	}{{"research", true}, {"search", false}} {
		run, _, err := pg.CreateRetrievalRun(ctx, db.RetrievalRun{TenantID: tenant, RequestKey: "share-" + c.kind, Request: json.RawMessage(`{}`), Kind: c.kind, MaxCalls: 24, MaxTokens: 48000, Deadline: time.Now().Add(time.Minute)}, "test")
		if err != nil {
			t.Fatal(err)
		}
		q.Limit++ // a distinct query per kind: the cache must not answer the second run
		if _, err = s.candidates(withBudget(ctx, run), q); err != nil {
			t.Fatal(err)
		}
		left, err := pg.RetrievalTokensLeft(ctx, tenant, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if c.wantHalf && left < 24000 {
			t.Fatalf("research rerank must leave half the budget for follow-up: left=%d", left)
		}
		if !c.wantHalf && left >= 24000 {
			t.Fatalf("a plain search may spend its whole budget on rerank: left=%d", left)
		}
	}
}
