package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServedRevisionMismatch(t *testing.T) {
	var served atomic.Value
	served.Store("wrong")
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(map[string]any{"model_id": "served-model", "model_sha": served.Load().(string), "version": "local-service-1", "max_input_length": 512})
			return
		}
		posts.Add(1)
		if strings.HasSuffix(r.URL.Path, "rank") {
			_, _ = w.Write([]byte(`[{"index":0,"score":0.9},{"index":1,"score":0.1}]`))
		} else {
			_, _ = w.Write([]byte(`[[1,1]]`))
		}
	}))
	defer srv.Close()
	emb := &HTTPModel{URL: srv.URL + "/embed", InfoURL: srv.URL + "/info", Name: "model", Revision: "expected", Format: "tei", Dimension: 2}
	rank := &HTTPModel{URL: srv.URL + "/rank", InfoURL: srv.URL + "/info", Name: "rank", Revision: "expected", Format: "tei"}
	for _, test := range []func(context.Context) error{func(ctx context.Context) error { return SelfTestEmbedder(ctx, emb) }, func(ctx context.Context) error { return SelfTestReranker(ctx, rank) }} {
		err := test(context.Background())
		if err == nil || err.Error() != "model revision mismatch: configured expected, served wrong" || errors.Is(err, ErrModelTransient) {
			t.Fatalf("%v", err)
		}
	}
	if posts.Load() != 0 {
		t.Fatal("revision mismatch still invoked models")
	}
	served.Store("expected")
	if err := SelfTestEmbedder(context.Background(), emb); err != nil {
		t.Fatal(err)
	}
	if err := SelfTestReranker(context.Background(), rank); err != nil {
		t.Fatal(err)
	}
	if emb.Info().ServedVersion != "local-service-1" || rank.Info().ServedVersion != "local-service-1" {
		t.Fatal("served version missing")
	}
	info, err := emb.Served(context.Background())
	if err != nil || info.ModelSHA != "expected" || info.ModelID != "served-model" {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestEnvOptions(t *testing.T) {
	for _, prefix := range []string{"AUTOBID_EMBEDDING", "AUTOBID_RERANKER"} {
		for suffix, value := range map[string]string{"_URL": "http://127.0.0.1:18766/model", "_MODEL": "s6", "_REVISION": "r1", "_DIMENSION": "2", "_FORMAT": "tei", "_MAX_BATCH": "", "_TIMEOUT_SECONDS": "", "_TRUNCATE": "", "_INFO_URL": ""} {
			t.Setenv(prefix+suffix, value)
		}
	}
	for _, name := range []string{"AUTOBID_RERANKER_MAX_CANDIDATES", "AUTOBID_EMBEDDING_QUERY_PREFIX", "AUTOBID_INDEX_BUILD_DEADLINE_SECONDS", "AUTOBID_RETRIEVAL_CHANNEL_TIMEOUT_SECONDS"} {
		t.Setenv(name, "")
	}
	emb, rank, err := ModelsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if emb.(*HTTPModel).Timeout != 20*time.Second || rank.(*HTTPModel).MaxCandidates != 100 || emb.(*HTTPModel).Truncate {
		t.Fatal("defaults changed")
	}
	s := New(nil, nil, nil)
	if err = s.ConfigureFromEnv(); err != nil || s.MaxRerank != 100 || s.ChannelTimeout != 18*time.Second || s.IndexBuildDeadline != 2*time.Hour {
		t.Fatal("service defaults changed", err)
	}
	for _, tc := range []struct {
		name string
		good []string
		bad  []string
	}{
		{"AUTOBID_EMBEDDING_TIMEOUT_SECONDS", []string{"1", "600"}, []string{"0", "601", "x", "1.5", "-1"}},
		{"AUTOBID_RERANKER_TIMEOUT_SECONDS", []string{"1", "600"}, []string{"0", "601", "bad"}},
		{"AUTOBID_RERANKER_MAX_CANDIDATES", []string{"1", "100"}, []string{"0", "101", "bad"}},
		{"AUTOBID_RETRIEVAL_CHANNEL_TIMEOUT_SECONDS", []string{"1", "19"}, []string{"0", "20", "bad"}},
		{"AUTOBID_INDEX_BUILD_DEADLINE_SECONDS", []string{"1", "86400"}, []string{"0", "86401", "bad"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func() error {
				if strings.HasPrefix(tc.name, "AUTOBID_RETRIEVAL_") || strings.Contains(tc.name, "INDEX_BUILD") {
					return s.ConfigureFromEnv()
				}
				_, _, err := ModelsFromEnv()
				return err
			}
			for _, v := range tc.good {
				t.Setenv(tc.name, v)
				if err := check(); err != nil {
					t.Fatalf("%s: %v", v, err)
				}
			}
			for _, v := range tc.bad {
				t.Setenv(tc.name, v)
				if err := check(); err == nil {
					t.Fatalf("accepted %s", v)
				}
			}
		})
	}
	for _, prefix := range []string{"AUTOBID_EMBEDDING", "AUTOBID_RERANKER"} {
		t.Run(prefix+"-metadata", func(t *testing.T) {
			for _, v := range []string{"https://models.example/info", "http://[::1]:18766/info"} {
				t.Setenv(prefix+"_INFO_URL", v)
				if _, _, err := ModelsFromEnv(); err != nil {
					t.Fatal(err)
				}
			}
			for _, v := range []string{"file:///tmp/model", "ftp://models/info", "https://user:secret@models/info", "/info"} {
				t.Setenv(prefix+"_INFO_URL", v)
				if _, _, err := ModelsFromEnv(); err == nil {
					t.Fatalf("accepted info endpoint %s", v)
				}
			}
			t.Setenv(prefix+"_INFO_URL", "")
			for _, v := range []string{"1", "true"} {
				t.Setenv(prefix+"_TRUNCATE", v)
				e, r, err := ModelsFromEnv()
				if err != nil || !e.(*HTTPModel).Truncate && !r.(*HTTPModel).Truncate {
					t.Fatalf("truncate: %v", err)
				}
			}
			t.Setenv(prefix+"_TRUNCATE", "garbage")
			if _, _, err := ModelsFromEnv(); err == nil {
				t.Fatal("invalid truncate accepted")
			}
			t.Setenv(prefix+"_TRUNCATE", "true")
			for _, format := range []string{"compatible", "dashscope"} {
				t.Setenv(prefix+"_FORMAT", format)
				if _, _, err := ModelsFromEnv(); err == nil {
					t.Fatal("truncate accepted outside TEI")
				}
			}
		})
	}
	t.Setenv("AUTOBID_EMBEDDING_QUERY_PREFIX", "为检索编码：")
	t.Setenv("AUTOBID_RERANKER_MAX_CANDIDATES", "7")
	emb, rank, err = ModelsFromEnv()
	if err != nil || emb.(*HTTPModel).QueryPrefix != "为检索编码：" || rank.(*HTTPModel).MaxCandidates != 7 {
		t.Fatal(err)
	}
}

func TestQueryPrefixTruncateAndPrivateInfo(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests = append(requests, body)
		if body["query"] != nil {
			_, _ = w.Write([]byte(`[{"index":0,"score":0.9}]`))
		} else {
			_, _ = w.Write([]byte(`[[1,1]]`))
		}
	}))
	defer srv.Close()
	m := &HTTPModel{URL: srv.URL, Name: "emb", Revision: "r1", Dimension: 2, Format: "tei", APIKey: "private-key-marker", QueryPrefix: "查询前缀：", Truncate: true}
	input := []string{"正文"}
	if _, err := m.Embed(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedQuery(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rank(context.Background(), "query", input); err != nil {
		t.Fatal(err)
	}
	if input[0] != "正文" || requests[0]["inputs"].([]any)[0] != "正文" || requests[1]["inputs"].([]any)[0] != "查询前缀：正文" {
		t.Fatal("query prefix altered document input")
	}
	for _, req := range requests {
		if req["truncate"] != true {
			t.Fatal("truncate not forwarded")
		}
	}
	raw, _ := json.Marshal(m.Info())
	if strings.Contains(string(raw), srv.URL) || strings.Contains(string(raw), m.APIKey) || m.Info().Hosting != "local" || m.Info().TimeoutSec != 20 {
		t.Fatalf("unsafe info %s", raw)
	}
	for _, host := range []string{"http://localhost:80", "http://127.0.0.1:80", "http://[::1]:80"} {
		m.URL = host
		if m.Info().Hosting != "local" {
			t.Fatal(host)
		}
	}
	m.URL = "https://models.example"
	if m.Info().Hosting != "remote" {
		t.Fatal("remote hosting")
	}
}

func TestModelTransientClassification(t *testing.T) {
	for _, status := range []int{400, 401, 413, 429, 500, 502, 503, 504} {
		t.Run(fmtStatus(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"private response sk-secret-marker"}`))
			}))
			defer srv.Close()
			m := &HTTPModel{URL: srv.URL, APIKey: "sk-secret-marker", Dimension: 2}
			_, err := m.Embed(context.Background(), []string{"x"})
			want := status == 429 || status == 502 || status == 503 || status == 504
			if errors.Is(err, ErrModelTransient) != want || strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("%v", err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := srv.URL
	srv.Close()
	m := &HTTPModel{URL: endpoint, Dimension: 2}
	if _, err := m.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrModelTransient) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Embed(ctx, []string{"x"}); !errors.Is(err, context.Canceled) || errors.Is(err, ErrModelTransient) {
		t.Fatal(err)
	}
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hung.Close()
	m.URL, m.Timeout = hung.URL, 25*time.Millisecond
	if _, err := m.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrModelTransient) {
		close(release)
		t.Fatal(err)
	}
	close(release)
}

func fmtStatus(status int) string { return http.StatusText(status) }
