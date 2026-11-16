package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/retrieval"
	"github.com/autobid/autobid-engine/toolhealth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// The public routes fix the tenant to default. A fresh schema keeps empty-KB and
// active-index assertions independent of unrelated HTTP fixtures in this test database.
func s6HTTPFixture(t *testing.T) (*apiClient, *API, *db.DB, *retrieval.HTTPModel) {
	t.Helper()
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("S6 HTTP tests require a real PostgreSQL database")
	}
	base := testDB(t)
	schema := fmt.Sprintf("s6_http_%d", time.Now().UnixNano())
	if _, err := base.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal("create isolated HTTP test schema failed")
	}
	t.Cleanup(func() {
		if _, err := base.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Error("drop isolated HTTP test schema failed")
		}
	})
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse test database configuration failed")
	}
	config.RuntimeParams["search_path"] = schema
	pool := sql.OpenDB(stdlib.GetConnector(*config))
	pg := &db.DB{DB: pool}
	t.Cleanup(func() { _ = pg.Close() })
	if err = pg.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Inputs []string `json:"inputs"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		vectors := make([][]float64, len(req.Inputs))
		for i := range vectors {
			vectors[i] = []float64{1, 1}
		}
		_ = json.NewEncoder(w).Encode(vectors)
	}))
	t.Cleanup(models.Close)
	m := &retrieval.HTTPModel{URL: models.URL + "/private-endpoint-marker", InfoURL: models.URL + "/private-info-marker", APIKey: "private-api-key-marker", Name: "http-embedding", Revision: "r1", Dimension: 2, Format: "tei"}
	svc := retrieval.New(pg, m, nil)
	reg := toolhealth.New()
	reg.Record(toolhealth.Embedding, toolhealth.Result{Version: m.Model()}, 0, nil)
	reg.SetUnconfigured(toolhealth.Reranker, "", "unconfigured")
	svc.Health = reg
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	api := testAPI(ctx, NewManager(pg), nil, NewLLMService(pg, LLMServiceConfig{MultiRetrieval: true}), nil, NewHub(), APIConfig{Retrieval: svc, Runtime: reg})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &apiClient{t: t, url: srv.URL}, api, pg, m
}

func s6HTTPCode(t *testing.T, c *apiClient, method, path string, body any, status int, code string) map[string]any {
	t.Helper()
	var got map[string]any
	actual := c.raw(method, path, body, &got)
	if actual != status || code != "" && got["code"] != code {
		t.Fatalf("%s %s: status=%d body=%v; want %d %s", method, path, actual, got, status, code)
	}
	return got
}

func TestHTTPKnowledgeIndexJobContractsAndEvents(t *testing.T) {
	c, api, pg, model := s6HTTPFixture(t)
	svc := api.cfg.Retrieval
	svc.Embedder = nil
	s6HTTPCode(t, c, "POST", "/api/knowledge/indexes", map[string]any{}, 503, "runtime_unavailable")
	svc.Embedder = model
	s6HTTPCode(t, c, "POST", "/api/knowledge/indexes", map[string]any{}, 422, "nothing_to_index")
	id, _, err := pg.InsertKnowledgeDocument(context.Background(), &db.KnowledgeDocument{Title: "HTTP恢复服务", Category: db.KnowledgeProductSpec, RawText: "HTTP独立知识库"}, []db.KnowledgeChunk{{Seq: 0, Content: "恢复时间：30秒"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []any{map[string]any{"request_key": "sys:index:client"}, map[string]any{"request_key": strings.Repeat("k", 161)}, map[string]any{"dimension": 0}, map[string]any{"dimension": -1}, map[string]any{"dimension": 8193}, map[string]any{"dimension": "two"}} {
		s6HTTPCode(t, c, "POST", "/api/knowledge/indexes", body, 400, "invalid_argument")
	}
	var sub struct {
		Index    db.KnowledgeIndex `json:"index"`
		Replayed bool              `json:"replayed"`
	}
	if status := c.do("POST", "/api/knowledge/indexes", map[string]any{"request_key": "first"}, &sub); status != 202 || sub.Replayed || sub.Index.Status != "queued" || sub.Index.Dimension != 2 {
		t.Fatalf("%d %+v", status, sub)
	}
	first := sub.Index
	if status := c.do("POST", "/api/knowledge/indexes", map[string]any{"request_key": "first"}, &sub); status != 202 || !sub.Replayed || sub.Index.ID != first.ID {
		t.Fatalf("replay %d %+v", status, sub)
	}
	s6HTTPCode(t, c, "POST", "/api/knowledge/indexes", map[string]any{"request_key": "first", "dimension": 3}, 409, "idempotency_conflict")
	conflict := s6HTTPCode(t, c, "POST", "/api/knowledge/indexes", map[string]any{"request_key": "second"}, 409, "build_active")
	if conflict["index_id"] != float64(first.ID) {
		t.Fatal("active build ID missing")
	}
	path := fmt.Sprintf("/api/knowledge/indexes/%d", first.ID)
	s6HTTPCode(t, c, "POST", path+"/activate", nil, 409, "not_ready")
	s6HTTPCode(t, c, "DELETE", path, nil, 409, "index_in_use")
	if status := c.do("POST", path+"/cancel", nil, nil); status != 204 {
		t.Fatal(status)
	}
	s6HTTPCode(t, c, "POST", path+"/cancel", nil, 409, "already_terminal")
	if status := c.do("POST", "/api/knowledge/indexes", map[string]any{"request_key": "first"}, &sub); status != 202 || !sub.Replayed || sub.Index.Status != "cancelled" {
		t.Fatalf("terminal replay %d %+v", status, sub)
	}
	if status := c.do("DELETE", path, nil, nil); status != 204 {
		t.Fatal(status)
	}
	s6HTTPCode(t, c, "DELETE", path, nil, 404, "not_found")
	for _, bad := range []string{"bad", "0", "-1"} {
		s6HTTPCode(t, c, "POST", "/api/knowledge/indexes/"+bad+"/activate", nil, 400, "invalid_argument")
	}
	s6HTTPCode(t, c, "GET", "/api/knowledge/index-events?limit=bad", nil, 400, "invalid_argument")
	s6HTTPCode(t, c, "GET", "/api/knowledge/index-events?limit=0", nil, 400, "invalid_argument")
	a, err := svc.BuildIndex(context.Background(), db.DefaultTenant, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.BuildIndex(context.Background(), db.DefaultTenant, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []db.KnowledgeIndex{a, b, a} {
		if status := c.do("POST", fmt.Sprintf("/api/knowledge/indexes/%d/activate", x.ID), nil, nil); status != 204 {
			t.Fatal(status)
		}
	}
	s6HTTPCode(t, c, "DELETE", fmt.Sprintf("/api/knowledge/indexes/%d", a.ID), nil, 409, "index_in_use")
	var events []db.KnowledgeIndexEvent
	if status := c.do("GET", "/api/knowledge/index-events?limit=999", nil, &events); status != 200 {
		t.Fatal(status)
	}
	names := []string{}
	for _, e := range events {
		names = append(names, e.Event)
	}
	for _, want := range []string{"build_requested", "build_cancelled", "deleted", "build_ready", "activated", "rolled_back"} {
		if !slices.Contains(names, want) {
			t.Fatalf("missing %s: %v", want, names)
		}
	}
	if events[0].Event != "rolled_back" || events[0].FromIndexID == nil || *events[0].FromIndexID != b.ID {
		t.Fatalf("rollback event %+v", events[0])
	}
	if status := c.do("GET", "/api/knowledge/index-events?limit=1", nil, &events); status != 200 || len(events) != 1 {
		t.Fatalf("event pagination %d %+v", status, events)
	}
	var indexes []db.KnowledgeIndex
	if status := c.do("GET", "/api/knowledge/indexes", nil, &indexes); status != 200 || len(indexes) != 2 {
		t.Fatalf("list %d %+v", status, indexes)
	}
	svc.Embedder = nil
	s6HTTPCode(t, c, "POST", fmt.Sprintf("/api/knowledge/indexes/%d/activate", b.ID), nil, 409, "model_unconfigured")
	svc.Embedder = &retrieval.HTTPModel{Name: "other", Revision: "r2", Dimension: 2}
	s6HTTPCode(t, c, "POST", fmt.Sprintf("/api/knowledge/indexes/%d/activate", b.ID), nil, 409, "model_mismatch")
	svc.Embedder = &retrieval.HTTPModel{Name: model.Name, Revision: model.Revision, Dimension: 3}
	s6HTTPCode(t, c, "POST", fmt.Sprintf("/api/knowledge/indexes/%d/activate", b.ID), nil, 409, "dimension_mismatch")
	svc.Embedder = model
	baseline, err := svc.BuildBaselineIndex(context.Background(), db.DefaultTenant, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	s6HTTPCode(t, c, "POST", fmt.Sprintf("/api/knowledge/indexes/%d/activate", baseline.ID), nil, 409, "preprocessing_mismatch")
	if _, err = pg.ExecContext(context.Background(), `DELETE FROM knowledge_embeddings WHERE index_id=$1`, b.ID); err != nil {
		t.Fatal(err)
	}
	s6HTTPCode(t, c, "POST", fmt.Sprintf("/api/knowledge/indexes/%d/activate", b.ID), nil, 409, "coverage_incomplete")
	if _, err = pg.ExecContext(context.Background(), `UPDATE knowledge_documents SET title='changed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	s6HTTPCode(t, c, "POST", fmt.Sprintf("/api/knowledge/indexes/%d/activate", b.ID), nil, 409, "documents_changed")
	if status := c.do("DELETE", fmt.Sprintf("/api/knowledge/indexes/%d", b.ID), nil, nil); status != 204 {
		t.Fatal(status)
	}
}

func TestHTTPRetrievalStatusPrivateAndNoModelCalls(t *testing.T) {
	c, api, pg, model := s6HTTPFixture(t)
	_, _, err := pg.InsertKnowledgeDocument(context.Background(), &db.KnowledgeDocument{Title: "私有状态测试", Category: db.KnowledgeProductSpec, RawText: "状态正文"}, []db.KnowledgeChunk{{Seq: 0, Content: "状态正文"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	x, err := api.cfg.Retrieval.BuildIndex(context.Background(), db.DefaultTenant, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	private := `POST ` + model.URL + `?api_key=` + model.APIKey + ` C:\private-host-path\model.bin`
	if _, err = pg.ExecContext(context.Background(), `UPDATE knowledge_indexes SET error=$2 WHERE id=$1`, x.ID, private); err != nil {
		t.Fatal(err)
	}
	api.cfg.Runtime.Degrade(toolhealth.Embedding, errors.New(private))
	if err = pg.NoteConfiguredModels(context.Background(), db.DefaultTenant, map[string]any{"embedding": model.Model(), "nested": map[string]any{"reason": private}}, "test"); err != nil {
		t.Fatal(err)
	}
	// Status only reads configuration and persistent state; an unreachable URL is safe to inspect.
	model.URL = "http://127.0.0.1:1/private-endpoint-marker"
	for _, path := range []string{"/api/knowledge/retrieval-status", "/api/knowledge/indexes", "/api/knowledge/index-events"} {
		var raw json.RawMessage
		if status := c.do("GET", path, nil, &raw); status != 200 {
			t.Fatal(path, status)
		}
		for _, secret := range []string{"http://", "https://", "private-endpoint-marker", "private-info-marker", "private-api-key-marker", "private-host-path", "lease_token", "request_hash", "cursor_chunk_id"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("%s leaked %s: %s", path, secret, raw)
			}
		}
	}
	var status retrieval.RetrievalStatus
	if code := c.do("GET", "/api/knowledge/retrieval-status", nil, &status); code != 200 || !status.MultiAgentEnabled || status.State != "lexical_only" || status.Indexes[0].State != "rollback_ready" {
		t.Fatalf("%d %+v", code, status)
	}
}

func TestHTTPIndexSubmissionWakesRunner(t *testing.T) {
	c, api, _, _ := s6HTTPFixture(t)
	_, _, err := api.pg.InsertKnowledgeDocument(context.Background(), &db.KnowledgeDocument{Title: "异步构建", Category: db.KnowledgeProductSpec, RawText: "异步构建正文"}, []db.KnowledgeChunk{{Seq: 0, Content: "恢复时间：30秒"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan int64, 1)
	runner := &retrieval.IndexRunner{Service: api.cfg.Retrieval, TenantID: db.DefaultTenant, Poll: time.Hour, OnChange: func(x *db.KnowledgeIndex) {
		if x.Status == "ready" {
			select {
			case ready <- x.ID:
			default:
			}
		}
	}}
	api.cfg.IndexRunner = runner
	runner.Start(ctx)
	var sub struct {
		Index db.KnowledgeIndex `json:"index"`
	}
	if status := c.do("POST", "/api/knowledge/indexes", map[string]any{"request_key": "async"}, &sub); status != 202 {
		t.Fatal(status)
	}
	select {
	case id := <-ready:
		if id != sub.Index.ID {
			t.Fatal("wrong index completed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP did not wake runner")
	}
}

type retryProbeEmbedder struct{ calls atomic.Int64 }

func (*retryProbeEmbedder) Model() string { return "loading@1" }
func (e *retryProbeEmbedder) Embed(context.Context, []string) ([][]float64, error) {
	if e.calls.Add(1) == 1 {
		return nil, retrieval.ErrModelTransient
	}
	return [][]float64{{1, 1}}, nil
}

func TestProbeRuntimeRetriesTransientAndShowsServiceVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	reg := toolhealth.New()
	emb := &retryProbeEmbedder{}
	ProbeRuntime(ctx, reg, nil, emb, nil)
	deadline := time.Now().Add(5 * time.Second)
	for reg.Get(toolhealth.Embedding).State == toolhealth.Starting && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := reg.Get(toolhealth.Embedding); st.State != toolhealth.Healthy || emb.calls.Load() != 2 || st.Version != "loading@1" {
		t.Fatalf("%+v calls=%d", st, emb.calls.Load())
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"model_id":"rank","model_sha":"r1","version":"local-1"}`))
		} else {
			_, _ = w.Write([]byte(`[{"index":0,"score":0.9},{"index":1,"score":0.1}]`))
		}
	}))
	defer srv.Close()
	rank := &retrieval.HTTPModel{URL: srv.URL, InfoURL: srv.URL, Name: "rank", Revision: "r1", Format: "tei"}
	reg = toolhealth.New()
	ProbeRuntime(ctx, reg, nil, nil, rank)
	for reg.Get(toolhealth.Reranker).State == toolhealth.Starting && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := reg.Get(toolhealth.Reranker); st.State != toolhealth.Healthy || st.Version != "rank@r1 (service local-1)" {
		t.Fatalf("%+v", st)
	}
}
