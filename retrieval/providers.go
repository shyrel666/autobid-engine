// Package retrieval provides the shared search/evidence layer for HTTP and agents.
package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
)

type Reranker interface {
	Model() string
	Rank(context.Context, string, []string) ([]float64, error)
}
type HTTPModel struct {
	URL       string
	Name      string
	Revision  string
	APIKey    string
	Dimension int
	Format    string // tei or compatible (embeddings: indexed data; rerank: results)
	Client    *http.Client
}

func (m *HTTPModel) Model() string { return m.Name + "@" + m.Revision }
func (m *HTTPModel) post(ctx context.Context, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("invalid model endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if m.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.APIKey)
	}
	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("model transport failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("model HTTP %d", res.StatusCode)
	} // never expose remote bodies or credentials
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20+1))
	if err != nil {
		return fmt.Errorf("model response read failed")
	}
	if len(data) > 16<<20 {
		return fmt.Errorf("model response too large")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid model JSON")
	}
	return nil
}
func (m *HTTPModel) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	var v [][]float64
	if m.Format == "tei" {
		if err := m.post(ctx, map[string]any{"inputs": texts, "truncate": false}, &v); err != nil {
			return nil, err
		}
	} else {
		var out struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if err := m.post(ctx, map[string]any{"model": m.Name, "input": texts, "encoding_format": "float"}, &out); err != nil {
			return nil, err
		}
		v = make([][]float64, len(texts))
		for _, d := range out.Data {
			if d.Index < 0 || d.Index >= len(v) || v[d.Index] != nil {
				return nil, fmt.Errorf("invalid embedding indices")
			}
			v[d.Index] = d.Embedding
		}
	}
	if err := db.ValidateVectors(v, len(texts), m.Dimension); err != nil {
		return nil, err
	}
	return v, nil
}
func (m *HTTPModel) Rank(ctx context.Context, q string, texts []string) ([]float64, error) {
	type item struct {
		Index     int      `json:"index"`
		Score     *float64 `json:"score"`
		Relevance *float64 `json:"relevance_score"`
	}
	var items []item
	if m.Format == "tei" {
		if err := m.post(ctx, map[string]any{"query": q, "texts": texts, "raw_scores": true, "truncate": false}, &items); err != nil {
			return nil, err
		}
	} else {
		var out struct {
			Results []item `json:"results"`
		}
		if err := m.post(ctx, map[string]any{"model": m.Name, "query": q, "documents": texts, "top_n": len(texts)}, &out); err != nil {
			return nil, err
		}
		items = out.Results
	}
	if len(items) != len(texts) {
		return nil, fmt.Errorf("incomplete reranker results")
	}
	v := make([]float64, len(texts))
	seen := map[int]bool{}
	for _, x := range items {
		if x.Index < 0 || x.Index >= len(v) || seen[x.Index] {
			return nil, fmt.Errorf("invalid rerank indices")
		}
		seen[x.Index] = true
		s := x.Score
		if m.Format != "tei" {
			s = x.Relevance
		}
		if s == nil || math.IsNaN(*s) || math.IsInf(*s, 0) {
			return nil, fmt.Errorf("non-finite rerank score")
		}
		v[x.Index] = *s
	}
	return v, nil
}

// ModelsFromEnv never contacts the network. Endpoint paths are configured explicitly.
func ModelsFromEnv() (db.Embedder, Reranker, error) {
	read := func(prefix string) (*HTTPModel, error) {
		endpoint := strings.TrimSpace(os.Getenv(prefix + "_URL"))
		if endpoint == "" {
			return nil, nil
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return nil, fmt.Errorf("invalid %s_URL", prefix)
		}
		m := &HTTPModel{URL: endpoint, Name: os.Getenv(prefix + "_MODEL"), Revision: os.Getenv(prefix + "_REVISION"), Format: os.Getenv(prefix + "_FORMAT"), APIKey: os.Getenv(prefix + "_API_KEY")}
		if m.Format == "" {
			m.Format = "compatible"
		}
		if m.Name == "" || m.Revision == "" || (m.Format != "compatible" && m.Format != "tei") {
			return nil, fmt.Errorf("%s requires MODEL, REVISION and supported FORMAT", prefix)
		}
		m.Dimension, _ = strconv.Atoi(os.Getenv(prefix + "_DIMENSION"))
		return m, nil
	}
	e, err := read("AUTOBID_EMBEDDING")
	if err != nil {
		return nil, nil, err
	}
	r, err := read("AUTOBID_RERANKER")
	if err != nil {
		return nil, nil, err
	}
	if e != nil && (e.Dimension < 1 || e.Dimension > 8192) {
		return nil, nil, fmt.Errorf("AUTOBID_EMBEDDING_DIMENSION must be 1..8192")
	}
	var emb db.Embedder
	var rank Reranker
	if e != nil {
		emb = e
	}
	if r != nil {
		rank = r
	}
	return emb, rank, nil
}
