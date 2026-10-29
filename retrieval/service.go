package retrieval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
	"golang.org/x/sync/singleflight"
)

const Strategy = "hybrid-rrf-v2" // v2: query-side encoding for asymmetric models; budget-bounded rerank head
const Preprocessing = "title-path-metadata-body-v2"

type Service struct {
	DB       *db.DB
	Embedder db.Embedder
	Reranker Reranker
	flights  singleflight.Group
	mu       sync.Mutex
	cache    map[string]cacheEntry
}
type cacheEntry struct {
	Raw   []byte
	Until time.Time
}
type Result struct {
	EvidenceIDs []int64           `json:"evidence_ids"`
	RunID       int64             `json:"run_id"`
	Total       int               `json:"total"`
	Hits        []db.KnowledgeHit `json:"hits"`
	Degraded    []string          `json:"degraded"`
	IndexID     int64             `json:"index_id"`
	Epoch       int64             `json:"epoch"`
	Strategy    string            `json:"strategy"`
	Timings     map[string]int64  `json:"timings_ms"`
	Cached      bool              `json:"cached"`
}

func New(pg *db.DB, e db.Embedder, r Reranker) *Service {
	return &Service{DB: pg, Embedder: e, Reranker: r, cache: map[string]cacheEntry{}}
}
func key() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func tenant(t string) string {
	if t == "" {
		return db.DefaultTenant
	}
	return t
}
func validateQuery(q db.KnowledgeQuery) error {
	if q.DocID < 0 || len(q.Categories) > 4 || len(q.Tags) > 50 || len(q.MustTerms) > 20 || len(q.Text)+len(strings.Join(q.MustTerms, " "))+len(strings.Join(q.Tags, " ")) > 8192 {
		return fmt.Errorf("%w: query filters too large or invalid", db.ErrInvalidKnowledge)
	}
	for _, term := range q.MustTerms {
		if strings.TrimSpace(term) == "" {
			return fmt.Errorf("%w: empty exact filter", db.ErrInvalidKnowledge)
		}
	}
	for _, category := range q.Categories {
		if !db.ValidKnowledgeCategory(category) {
			return fmt.Errorf("%w: invalid category", db.ErrInvalidKnowledge)
		}
	}
	return nil
}
func (s *Service) BuildIndex(ctx context.Context, t, actor string, dimension int) (db.KnowledgeIndex, error) {
	return s.buildIndex(ctx, t, actor, dimension, Preprocessing)
}
func (s *Service) BuildBaselineIndex(ctx context.Context, t, actor string, dimension int) (db.KnowledgeIndex, error) {
	return s.buildIndex(ctx, t, actor, dimension, "title-path-body-v1")
}
func (s *Service) buildIndex(ctx context.Context, t, actor string, dimension int, prep string) (db.KnowledgeIndex, error) {
	if s.Embedder == nil {
		return db.KnowledgeIndex{}, fmt.Errorf("semantic embedding service is not configured")
	}
	x, err := s.DB.StartKnowledgeIndex(ctx, t, s.Embedder.Model(), prep, actor, dimension)
	if err != nil {
		return x, err
	}
	build := func() error {
		var after int64
		for {
			chunks, err := s.DB.IndexChunks(ctx, t, after, 32)
			if err != nil {
				return err
			}
			if len(chunks) == 0 {
				break
			}
			texts := make([]string, len(chunks))
			for i, c := range chunks {
				texts[i] = c.Document.Title + " " + c.Chunk.SectionPath + " " + c.Chunk.Content
				if prep == Preprocessing {
					texts[i] = c.Document.Title + " " + c.Chunk.SectionPath + " " + string(c.Document.Extra) + " " + c.Chunk.Content
				}
			}
			vectors, err := s.Embedder.Embed(ctx, texts)
			if err != nil {
				return err
			}
			if err = s.DB.PutKnowledgeEmbeddings(ctx, x, chunks, vectors); err != nil {
				return err
			}
			after = chunks[len(chunks)-1].Chunk.ID
		}
		return nil
	}
	err = build()
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if ferr := s.DB.FinishKnowledgeIndex(finishCtx, x, err); ferr != nil {
		return x, ferr
	}
	if err != nil {
		x.Status = "failed"
		x.Error = err.Error()
	} else {
		x.Status = "ready"
	}
	return x, err
}

func Fuse(channels ...[]db.KnowledgeHit) []db.KnowledgeHit {
	byID := map[int64]*db.KnowledgeHit{}
	for _, ch := range channels {
		seen := map[int64]bool{}
		for i, h := range ch {
			if seen[h.Chunk.ID] {
				continue
			}
			seen[h.Chunk.ID] = true
			v := byID[h.Chunk.ID]
			if v == nil {
				v = new(db.KnowledgeHit)
				*v = h
				v.Score = 0
				byID[h.Chunk.ID] = v
			}
			v.Score += 1 / float64(60+i+1)
			if h.FTSRank > 0 {
				v.FTSRank = h.FTSRank
			}
			if h.VectorScore != 0 {
				v.VectorScore = h.VectorScore
			}
		}
	}
	out := make([]db.KnowledgeHit, 0, len(byID))
	for _, h := range byID {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].Chunk.ID < out[j].Chunk.ID
		}
		return out[i].Score > out[j].Score
	})
	return out
}

// Search records a distinct audit run even when the immutable candidate payload is cached.
func (s *Service) Search(ctx context.Context, q db.KnowledgeQuery, taskID int64, actor string) (Result, error) {
	q.TenantID = tenant(q.TenantID)
	q.Text = strings.TrimSpace(q.Text)
	if err := validateQuery(q); err != nil {
		return Result{}, err
	}
	if len(q.Text) > 4096 || len(q.MustTerms) > 20 || len(q.Tags) > 50 || len(db.QueryTerms(q.Text))+len(q.MustTerms) == 0 {
		return Result{}, fmt.Errorf("%w: invalid query", db.ErrInvalidKnowledge)
	}
	if q.Limit <= 0 {
		q.Limit = 10
	}
	q.Limit = min(q.Limit, 50)
	raw, _ := json.Marshal(q)
	var task *int64
	if taskID > 0 {
		task = &taskID
	}
	run, _, err := s.DB.CreateRetrievalRun(ctx, db.RetrievalRun{TenantID: q.TenantID, TaskID: task, RequestKey: key(), Request: raw, Kind: "search", MaxCalls: 4, MaxTokens: 64000, Deadline: time.Now().Add(45 * time.Second)}, actor)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, run.Deadline)
	defer cancel()
	ctx = withBudget(ctx, run)
	r, err := s.candidates(ctx, q)
	r.RunID = run.ID
	if err == nil {
		for _, h := range r.Hits[:min(len(r.Hits), 12)] {
			e, captureErr := s.capture(ctx, q.TenantID, actor, run.ID, h)
			if captureErr != nil {
				err = captureErr
				break
			}
			r.EvidenceIDs = append(r.EvidenceIDs, e.ID)
		}
	}
	finishCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	status := "completed"
	if err != nil {
		status = "failed"
	}
	if ctx.Err() != nil {
		status = "cancelled"
	}
	payload := any(r)
	if err != nil {
		payload = map[string]any{"error": err.Error()}
	}
	if e := s.DB.FinishRetrievalRun(finishCtx, q.TenantID, run.ID, status, payload); e != nil && err == nil {
		err = e
	}
	return r, err
}
func (s *Service) candidates(ctx context.Context, q db.KnowledgeQuery) (Result, error) {
	epoch, err := s.DB.KnowledgeEpoch(ctx, q.TenantID)
	if err != nil {
		return Result{}, err
	}
	indexes, err := s.DB.KnowledgeIndexes(ctx, q.TenantID)
	if err != nil {
		return Result{}, err
	}
	var active *db.KnowledgeIndex
	for i := range indexes {
		if indexes[i].Active {
			active = &indexes[i]
			break
		}
	}
	model, rank := "", ""
	if s.Embedder != nil {
		model = s.Embedder.Model()
	}
	if s.Reranker != nil {
		rank = s.Reranker.Model()
	}
	b, _ := json.Marshal([]any{q, epoch, active, model, rank, Strategy, "server-tenant-scope-v1"})
	cacheKey := db.ContentHash(string(b))
	s.mu.Lock()
	entry, ok := s.cache[cacheKey]
	s.mu.Unlock()
	if ok && entry.Until.After(time.Now()) {
		var r Result
		_ = json.Unmarshal(entry.Raw, &r)
		r.Cached = true
		return r, nil
	}
	// Deduplicate within one budget owner. Cancellation must stop paid work.
	flightKey := cacheKey
	if run, ok := ctx.Value(budgetKey{}).(*db.RetrievalRun); ok {
		flightKey += fmt.Sprint(":", run.ID)
	}
	ch := s.flights.DoChan(flightKey, func() (any, error) {
		work, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		r, err := s.retrieve(work, q, epoch, active)
		if err != nil {
			return r, err
		}
		latest, e := s.DB.KnowledgeEpoch(work, q.TenantID)
		if e != nil {
			return r, e
		}
		if latest != epoch {
			return r, db.ErrRetrievalConflict
		}
		if len(r.Degraded) == 0 {
			raw, _ := json.Marshal(r)
			s.mu.Lock()
			if len(s.cache) >= 128 {
				s.cache = map[string]cacheEntry{}
			}
			s.cache[cacheKey] = cacheEntry{raw, time.Now().Add(time.Minute)}
			s.mu.Unlock()
		}
		raw, _ := json.Marshal(r)
		return raw, nil
	})
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case v := <-ch:
		if v.Err != nil {
			return Result{}, v.Err
		}
		var r Result
		err = json.Unmarshal(v.Val.([]byte), &r)
		return r, err
	}
}
func (s *Service) retrieve(ctx context.Context, q db.KnowledgeQuery, epoch int64, idx *db.KnowledgeIndex) (Result, error) {
	r := Result{Hits: []db.KnowledgeHit{}, Degraded: []string{}, Epoch: epoch, Strategy: Strategy, Timings: map[string]int64{}}
	start := time.Now()
	cq := q
	cq.Limit = 50
	if err := s.reserve(ctx, 1, 0); err != nil {
		return r, err
	}
	lex, err := s.DB.RetrievalCandidates(ctx, cq, 0, nil)
	if err != nil {
		return r, err
	}
	r.Timings["lexical"] = time.Since(start).Milliseconds()
	var dense []db.KnowledgeHit
	if s.Embedder == nil {
		r.Degraded = append(r.Degraded, "semantic_model_unconfigured")
	} else if idx == nil || idx.Model != s.Embedder.Model() || idx.Epoch != epoch || idx.Preprocessing != Preprocessing {
		r.Degraded = append(r.Degraded, "semantic_index_missing_or_stale")
	} else {
		r.IndexID = idx.ID
		start = time.Now()
		text := strings.TrimSpace(q.Text + " " + strings.Join(q.MustTerms, " "))
		var v [][]float64
		e := s.reserve(ctx, 1, len(text)+256)
		if e == nil {
			v, e = db.EmbedQuery(ctx, s.Embedder, []string{text})
		}
		if e == nil {
			e = db.ValidateVectors(v, 1, idx.Dimension)
		}
		if e == nil {
			dense, e = s.DB.RetrievalCandidates(ctx, cq, idx.ID, v[0])
		}
		if e != nil {
			r.Degraded = append(r.Degraded, "semantic_channel_failed")
		}
		r.Timings["semantic"] = time.Since(start).Milliseconds()
	}
	hits := Fuse(lex, dense)
	if len(hits) > 100 {
		hits = hits[:100]
	}
	if s.Reranker == nil {
		r.Degraded = append(r.Degraded, "reranker_unconfigured")
	} else if len(hits) > 0 {
		start = time.Now()
		texts := make([]string, len(hits))
		query := q.Text + " " + strings.Join(q.MustTerms, " ")
		bound, n := 256, len(hits)
		cost := make([]int, len(hits))
		for i, h := range hits {
			texts[i] = h.Citation.DocTitle + " " + h.Chunk.SectionPath + " " + string(h.Metadata) + " " + h.Chunk.Content
			cost[i] = len(texts[i]) + len(query) + 32
		}
		// Within a budgeted run rerank the fused head that fits the remaining reservation; the tail keeps RRF order.
		if left, ok := s.tokensLeft(ctx); ok {
			n = 0
			for n < len(hits) && bound+cost[n] <= left {
				bound += cost[n]
				n++
			}
		} else {
			for _, c := range cost {
				bound += c
			}
		}
		var scores []float64
		var e error = db.ErrRetrievalBudget
		if n > 0 {
			e = s.reserve(ctx, 1, bound)
		}
		if e == nil {
			scores, e = s.Reranker.Rank(ctx, query, texts[:n])
		}
		for _, score := range scores {
			if math.IsNaN(score) || math.IsInf(score, 0) {
				e = fmt.Errorf("invalid score")
			}
		}
		if e != nil || len(scores) != n {
			r.Degraded = append(r.Degraded, "reranker_failed")
		} else {
			head := hits[:n]
			for i := range head {
				head[i].Score = scores[i]
			}
			sort.SliceStable(head, func(i, j int) bool { return head[i].Score > head[j].Score })
			if n < len(hits) {
				r.Degraded = append(r.Degraded, "rerank_truncated")
			}
		}
		r.Timings["rerank"] = time.Since(start).Milliseconds()
	}
	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	r.Hits = hits
	r.Total = len(hits)
	return r, nil
}
