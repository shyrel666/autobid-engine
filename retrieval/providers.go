// Package retrieval provides the shared search/evidence layer for HTTP and agents.
package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
)

type Reranker interface {
	Model() string
	Rank(context.Context, string, []string) ([]float64, error)
}
type HTTPModel struct {
	URL           string
	Name          string
	Revision      string
	APIKey        string
	Dimension     int
	Format        string // tei, compatible (embeddings: indexed data; rerank: results) or dashscope (Bailian native output envelope)
	MaxBatch      int    // texts per embedding request; 0 sends each batch whole
	Client        *http.Client
	Timeout       time.Duration
	Truncate      bool
	InfoURL       string
	QueryPrefix   string
	MaxCandidates int
	used          atomic.Int64
	servedVersion atomic.Value
}

var ErrModelTransient = errors.New("retrieval: model service temporarily unavailable")

type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Revision      string `json:"revision"`
	Format        string `json:"format"`
	Dimension     int    `json:"dimension,omitempty"`
	Hosting       string `json:"hosting"`
	TimeoutSec    int    `json:"timeout_seconds"`
	MaxCandidates int    `json:"max_candidates,omitempty"`
	TokensUsed    int64  `json:"tokens_used"`
	ServedVersion string `json:"served_version,omitempty"`
}

type ServedModel struct{ ModelID, ModelSHA, Version string }

func (m *HTTPModel) timeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return 20 * time.Second
}

func (m *HTTPModel) Info() ModelInfo {
	hosting := "remote"
	if u, err := url.Parse(m.URL); err == nil {
		switch strings.ToLower(u.Hostname()) {
		case "127.0.0.1", "::1", "localhost":
			hosting = "local"
		}
	}
	i := ModelInfo{ID: m.Model(), Name: m.Name, Revision: m.Revision, Format: m.Format, Dimension: m.Dimension,
		Hosting: hosting, TimeoutSec: int(m.timeout() / time.Second), MaxCandidates: m.MaxCandidates, TokensUsed: m.TokensUsed()}
	if v := m.servedVersion.Load(); v != nil {
		i.ServedVersion = v.(string)
	}
	i.ID, i.Name, i.Revision, i.ServedVersion = toolhealth.SanitizeModel(i.ID), toolhealth.SanitizeModel(i.Name), toolhealth.SanitizeModel(i.Revision), toolhealth.SanitizeModel(i.ServedVersion)
	return i
}

type usageSinkKey struct{}

func WithUsageSink(ctx context.Context, sink func(int64)) context.Context {
	return context.WithValue(ctx, usageSinkKey{}, sink)
}

func (m *HTTPModel) request(ctx context.Context, method, endpoint string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("invalid model endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if m.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.APIKey)
	}
	// Copy even an injected client so redirects cannot send credentials to a different endpoint.
	client := http.Client{}
	if m.Client != nil {
		client = *m.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: model transport failed", ErrModelTransient)
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		msg := fmt.Sprintf("model HTTP %d", res.StatusCode)
		var e struct {
			Code string `json:"code"`
		}
		if m.Format == "dashscope" && json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&e) == nil && errorCode.MatchString(e.Code) {
			msg += " (" + e.Code + ")"
		}
		switch res.StatusCode {
		case 429, 502, 503, 504:
			return nil, fmt.Errorf("%w: %s", ErrModelTransient, msg)
		}
		return nil, errors.New(msg)
	}
	return res, nil
}

func (m *HTTPModel) Served(ctx context.Context) (ServedModel, error) {
	if m.InfoURL == "" {
		return ServedModel{}, fmt.Errorf("model info endpoint is not configured")
	}
	work, cancel := context.WithTimeout(ctx, m.timeout())
	defer cancel()
	res, err := m.request(work, http.MethodGet, m.InfoURL, nil)
	if err != nil {
		return ServedModel{}, modelRequestError(ctx, err)
	}
	defer res.Body.Close()
	var v struct {
		ID      string `json:"model_id"`
		SHA     string `json:"model_sha"`
		Version string `json:"version"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&v) != nil || v.ID == "" || v.SHA == "" {
		return ServedModel{}, fmt.Errorf("invalid model info JSON")
	}
	return ServedModel{v.ID, v.SHA, v.Version}, nil
}

func modelRequestError(caller context.Context, err error) error {
	if caller.Err() != nil {
		return caller.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: model request timed out", ErrModelTransient)
	}
	return err
}

func (m *HTTPModel) selfTestServed(ctx context.Context) error {
	if m.InfoURL == "" {
		return nil
	}
	v, err := m.Served(ctx)
	if err != nil {
		return err
	}
	if v.ModelSHA != m.Revision {
		return fmt.Errorf("model revision mismatch: configured %s, served %s", m.Revision, v.ModelSHA)
	}
	m.servedVersion.Store(v.Version)
	return nil
}

// errorCode accepts only machine-readable provider codes such as DashScope's "InvalidParameter"; messages are never surfaced.
var errorCode = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func (m *HTTPModel) Model() string { return m.Name + "@" + m.Revision }

// TokensUsed is the provider-reported token total (response usage.total_tokens) since start; TEI reports none.
func (m *HTTPModel) TokensUsed() int64 { return m.used.Load() }
func (m *HTTPModel) post(ctx context.Context, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	work, cancel := context.WithTimeout(ctx, m.timeout())
	defer cancel()
	res, err := m.request(work, http.MethodPost, m.URL, bytes.NewReader(raw))
	if err != nil {
		return modelRequestError(ctx, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: model response read failed", ErrModelTransient)
	}
	if len(data) > 16<<20 {
		return fmt.Errorf("model response too large")
	}
	var usage struct {
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &usage) == nil && usage.Usage.TotalTokens > 0 {
		m.used.Add(usage.Usage.TotalTokens)
		if sink, ok := ctx.Value(usageSinkKey{}).(func(int64)); ok && sink != nil {
			sink(usage.Usage.TotalTokens)
		}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid model JSON")
	}
	return nil
}

// Embed encodes documents. Batches larger than MaxBatch are split into consecutive requests.
func (m *HTTPModel) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	return m.embed(ctx, texts, "document")
}

// EmbedQuery encodes search queries; dashscope models encode them with text_type=query, other formats as documents.
func (m *HTTPModel) EmbedQuery(ctx context.Context, texts []string) ([][]float64, error) {
	if m.QueryPrefix != "" {
		prefixed := make([]string, len(texts))
		for i, t := range texts {
			prefixed[i] = m.QueryPrefix + t
		}
		texts = prefixed
	}
	return m.embed(ctx, texts, "query")
}
func (m *HTTPModel) embed(ctx context.Context, texts []string, textType string) ([][]float64, error) {
	step := len(texts)
	if m.MaxBatch > 0 && m.MaxBatch < step {
		step = m.MaxBatch
	}
	v := make([][]float64, 0, len(texts))
	for start := 0; start < len(texts); start += step {
		part, err := m.embedBatch(ctx, texts[start:min(start+step, len(texts))], textType)
		if err != nil {
			return nil, err
		}
		v = append(v, part...)
	}
	if err := db.ValidateVectors(v, len(texts), m.Dimension); err != nil {
		return nil, err
	}
	return v, nil
}

type indexedVector struct {
	Index     int
	Embedding []float64
}

func (m *HTTPModel) embedBatch(ctx context.Context, texts []string, textType string) ([][]float64, error) {
	var items []indexedVector
	switch m.Format {
	case "tei":
		var v [][]float64
		if err := m.post(ctx, map[string]any{"inputs": texts, "truncate": m.Truncate}, &v); err != nil {
			return nil, err
		}
		if len(v) != len(texts) {
			return nil, fmt.Errorf("embedding count: got %d want %d", len(v), len(texts))
		}
		return v, nil
	case "dashscope":
		params := map[string]any{"text_type": textType}
		if m.Dimension > 0 {
			params["dimension"] = m.Dimension
		}
		var out struct {
			Output struct {
				Embeddings []struct {
					TextIndex int       `json:"text_index"`
					Embedding []float64 `json:"embedding"`
				} `json:"embeddings"`
			} `json:"output"`
		}
		if err := m.post(ctx, map[string]any{"model": m.Name, "input": map[string]any{"texts": texts}, "parameters": params}, &out); err != nil {
			return nil, err
		}
		for _, e := range out.Output.Embeddings {
			items = append(items, indexedVector{e.TextIndex, e.Embedding})
		}
	default:
		var out struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if err := m.post(ctx, map[string]any{"model": m.Name, "input": texts, "encoding_format": "float"}, &out); err != nil {
			return nil, err
		}
		for _, d := range out.Data {
			items = append(items, indexedVector{d.Index, d.Embedding})
		}
	}
	v := make([][]float64, len(texts))
	for _, d := range items {
		if d.Index < 0 || d.Index >= len(v) || v[d.Index] != nil {
			return nil, fmt.Errorf("invalid embedding indices")
		}
		v[d.Index] = d.Embedding
	}
	return v, nil
}
func (m *HTTPModel) Rank(ctx context.Context, q string, texts []string) ([]float64, error) {
	type item struct {
		Index     int      `json:"index"`
		Score     *float64 `json:"score"`
		Relevance *float64 `json:"relevance_score"`
	}
	if len(texts) == 0 {
		return []float64{}, nil
	}
	var items []item
	switch m.Format {
	case "tei":
		if err := m.post(ctx, map[string]any{"query": q, "texts": texts, "raw_scores": true, "truncate": m.Truncate}, &items); err != nil {
			return nil, err
		}
	case "dashscope":
		var out struct {
			Output struct {
				Results []item `json:"results"`
			} `json:"output"`
		}
		if err := m.post(ctx, map[string]any{"model": m.Name, "input": map[string]any{"query": q, "documents": texts},
			"parameters": map[string]any{"top_n": len(texts), "return_documents": false}}, &out); err != nil {
			return nil, err
		}
		items = out.Output.Results
	default:
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
	maxCandidates, err := envRange("AUTOBID_RERANKER_MAX_CANDIDATES", 100, 1, 100)
	if err != nil {
		return nil, nil, err
	}
	read := func(prefix string) (*HTTPModel, error) {
		seconds, err := envRange(prefix+"_TIMEOUT_SECONDS", 20, 1, 600)
		if err != nil {
			return nil, err
		}
		endpoint := strings.TrimSpace(os.Getenv(prefix + "_URL"))
		if endpoint == "" {
			return nil, nil
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return nil, fmt.Errorf("invalid %s_URL", prefix)
		}
		m := &HTTPModel{URL: endpoint, Name: os.Getenv(prefix + "_MODEL"), Revision: os.Getenv(prefix + "_REVISION"), Format: os.Getenv(prefix + "_FORMAT"), APIKey: os.Getenv(prefix + "_API_KEY")}
		m.Timeout = time.Duration(seconds) * time.Second
		m.InfoURL = strings.TrimSpace(os.Getenv(prefix + "_INFO_URL"))
		if m.InfoURL != "" && !validModelURL(m.InfoURL) {
			return nil, fmt.Errorf("invalid %s_INFO_URL", prefix)
		}
		if m.Format == "" {
			m.Format = "compatible"
		}
		if m.Name == "" || m.Revision == "" || (m.Format != "compatible" && m.Format != "tei" && m.Format != "dashscope") {
			return nil, fmt.Errorf("%s requires MODEL, REVISION and supported FORMAT", prefix)
		}
		if raw, set := os.LookupEnv(prefix + "_TRUNCATE"); set && raw != "" {
			if m.Format != "tei" {
				return nil, fmt.Errorf("%s_TRUNCATE requires tei format", prefix)
			}
			switch strings.ToLower(strings.TrimSpace(raw)) {
			case "1", "true":
				m.Truncate = true
			case "0", "false":
			default:
				return nil, fmt.Errorf("invalid %s_TRUNCATE", prefix)
			}
		}
		if prefix == "AUTOBID_EMBEDDING" {
			m.QueryPrefix = os.Getenv(prefix + "_QUERY_PREFIX")
		} else {
			m.MaxCandidates = maxCandidates
		}
		m.Dimension, _ = strconv.Atoi(os.Getenv(prefix + "_DIMENSION"))
		if raw := strings.TrimSpace(os.Getenv(prefix + "_MAX_BATCH")); raw != "" {
			if m.MaxBatch, err = strconv.Atoi(raw); err != nil || m.MaxBatch < 1 || m.MaxBatch > 2048 {
				return nil, fmt.Errorf("%s_MAX_BATCH must be 1..2048", prefix)
			}
		} else if m.Format == "dashscope" {
			m.MaxBatch = 10 // strictest DashScope text-embedding batch limit; raise per model, e.g. 20 for qwen3.7-text-embedding-flash
		}
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

func validModelURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil
}

func envRange(name string, def, low, high int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < low || n > high {
		return 0, fmt.Errorf("%s must be %d..%d", name, low, high)
	}
	return n, nil
}

// SelfTestEmbedder embeds one fixed short text and checks the vector size against the configured dimension.
func SelfTestEmbedder(ctx context.Context, e db.Embedder) error {
	if e == nil {
		return fmt.Errorf("embedding model is not configured")
	}
	if m, ok := e.(*HTTPModel); ok {
		if err := m.selfTestServed(ctx); err != nil {
			return err
		}
	}
	vs, err := e.Embed(ctx, []string{"招标文件技术要求自检"})
	if err != nil {
		return fmt.Errorf("self-test embed: %w", err)
	}
	if len(vs) != 1 || len(vs[0]) == 0 {
		return fmt.Errorf("self-test embed: got %d vectors", len(vs))
	}
	if m, ok := e.(*HTTPModel); ok && m.Dimension > 0 && len(vs[0]) != m.Dimension {
		return fmt.Errorf("self-test embed: vector has %d dimensions, configured %d", len(vs[0]), m.Dimension)
	}
	for _, x := range vs[0] {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Errorf("self-test embed: non-finite vector value")
		}
	}
	return nil
}

// SelfTestReranker ranks two fixed passages and checks that the relevant one scores higher.
func SelfTestReranker(ctx context.Context, r Reranker) error {
	if r == nil {
		return fmt.Errorf("reranker is not configured")
	}
	if m, ok := r.(*HTTPModel); ok {
		if err := m.selfTestServed(ctx); err != nil {
			return err
		}
	}
	scores, err := r.Rank(ctx, "视频监控摄像机分辨率", []string{"本项目采购的视频监控摄像机分辨率不低于400万像素。", "投标保证金应在截止时间前缴纳。"})
	if err != nil {
		return fmt.Errorf("self-test rerank: %w", err)
	}
	if len(scores) != 2 {
		return fmt.Errorf("self-test rerank: got %d scores for 2 passages", len(scores))
	}
	if scores[0] <= scores[1] {
		return fmt.Errorf("self-test rerank: relevant passage scored %.4f, unrelated %.4f", scores[0], scores[1])
	}
	return nil
}
