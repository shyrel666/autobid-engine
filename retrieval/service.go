package retrieval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
	"golang.org/x/sync/singleflight"
)

const Strategy = "hybrid-rrf-v2" // v2: query-side encoding for asymmetric models; budget-bounded rerank head
const Preprocessing = "title-path-metadata-body-v2"

type Service struct {
	DB                 *db.DB
	Embedder           db.Embedder
	Reranker           Reranker
	Health             *toolhealth.Registry
	ChannelTimeout     time.Duration
	MaxRerank          int
	IndexBuildDeadline time.Duration
	flights            singleflight.Group
	mu                 sync.Mutex
	cache              map[string]cacheEntry
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
	Diagnostics Diagnostics       `json:"diagnostics"`
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
	b, _ := json.Marshal([]any{q, epoch, active, model, rank, s.embeddingDimension(), s.maxRerank(), Strategy, "server-tenant-scope-v1"})
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
		latest, e := s.DB.KnowledgeEpoch(ctx, q.TenantID)
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
	r.Diagnostics = Diagnostics{Mode: "lexical_only", IndexState: s.IndexState(idx, epoch), Preprocessing: Preprocessing, Failures: map[string]string{}}
	if s.Embedder != nil {
		r.Diagnostics.EmbeddingModel = toolhealth.SanitizeModel(s.Embedder.Model())
	}
	if s.Reranker != nil {
		r.Diagnostics.RerankerModel = toolhealth.SanitizeModel(s.Reranker.Model())
	}
	if run, ok := ctx.Value(budgetKey{}).(*db.RetrievalRun); ok {
		usageCtx := ctx
		ctx = WithUsageSink(ctx, func(tokens int64) {
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(usageCtx), 3*time.Second)
			defer cancel()
			_ = s.DB.RecordRetrievalUsage(writeCtx, run.TenantID, run.ID, int(min(tokens, int64(math.MaxInt32))))
		})
	}
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
	} else if r.Diagnostics.IndexState != IndexStateCurrent {
		r.Degraded = append(r.Degraded, "semantic_index_missing_or_stale")
	} else {
		r.IndexID = idx.ID
		start = time.Now()
		text := strings.TrimSpace(q.Text + " " + strings.Join(q.MustTerms, " "))
		var v [][]float64
		e := s.reserve(ctx, 1, len(text)+256)
		if e == nil {
			channel, cancel := s.channelContext(ctx)
			v, e = db.EmbedQuery(channel, s.Embedder, []string{text})
			cancel()
			if e == nil {
				e = db.ValidateVectors(v, 1, idx.Dimension)
			}
			s.modelHealth(ctx, toolhealth.Embedding, e)
		}
		if e != nil {
			r.Diagnostics.Failures["embedding"] = modelFailure(e)
		}
		if e == nil {
			dense, e = s.DB.RetrievalCandidates(ctx, cq, idx.ID, v[0])
			if e != nil {
				r.Diagnostics.Failures["embedding"] = "vector search failed"
			}
		}
		if e != nil {
			r.Degraded = append(r.Degraded, "semantic_channel_failed")
		} else {
			r.Diagnostics.Mode = "hybrid"
		}
		r.Timings["semantic"] = time.Since(start).Milliseconds()
	}
	hits := Fuse(lex, dense)
	if len(hits) > 100 {
		hits = hits[:100]
	}
	r.Diagnostics.FusedCandidates = len(hits)
	if s.Reranker == nil {
		r.Degraded = append(r.Degraded, "reranker_unconfigured")
	} else if len(hits) > 0 {
		start = time.Now()
		maxN := min(len(hits), s.maxRerank())
		texts := make([]string, maxN)
		query := q.Text + " " + strings.Join(q.MustTerms, " ")
		bound, n := 256, maxN
		cost := make([]int, maxN)
		for i, h := range hits[:maxN] {
			texts[i] = h.Citation.DocTitle + " " + h.Chunk.SectionPath + " " + string(h.Metadata) + " " + h.Chunk.Content
			cost[i] = len(texts[i]) + len(query) + 32
		}
		// Within a budgeted run rerank the fused head that fits the remaining reservation; the tail keeps RRF order.
		if left, ok := s.tokensLeft(ctx); ok {
			// A research run still has to pay for reasoning and follow-up searches: reranking a full head of real
			// tender chunks would otherwise reserve everything and leave bounded follow-up unreachable.
			if run, isRun := ctx.Value(budgetKey{}).(*db.RetrievalRun); isRun && run.Kind == "research" {
				left /= 2
			}
			n = 0
			for n < maxN && bound+cost[n] <= left {
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
			channel, cancel := s.channelContext(ctx)
			scores, e = s.Reranker.Rank(channel, query, texts[:n])
			cancel()
			if e == nil && len(scores) != n {
				e = fmt.Errorf("incomplete reranker results")
			}
			for _, score := range scores {
				if math.IsNaN(score) || math.IsInf(score, 0) {
					e = fmt.Errorf("invalid score")
				}
			}
			s.modelHealth(ctx, toolhealth.Reranker, e)
		}
		if e != nil || len(scores) != n {
			r.Degraded = append(r.Degraded, "reranker_failed")
			r.Diagnostics.Failures["reranker"] = modelFailure(e)
		} else {
			r.Diagnostics.RerankedCandidates = n
			head := hits[:n]
			for i := range head {
				head[i].Score = scores[i]
			}
			sort.SliceStable(head, func(i, j int) bool { return head[i].Score > head[j].Score })
			if n < maxN {
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

func (s *Service) maxRerank() int {
	if s.MaxRerank <= 0 {
		return 100
	}
	return min(s.MaxRerank, 100)
}

func (s *Service) channelContext(ctx context.Context) (context.Context, context.CancelFunc) {
	duration := s.ChannelTimeout
	if duration <= 0 {
		duration = 18 * time.Second
	}
	if deadline, ok := ctx.Deadline(); ok {
		duration = min(duration, time.Until(deadline)-2*time.Second)
	}
	return context.WithTimeout(ctx, duration)
}

func (s *Service) modelHealth(caller context.Context, cap toolhealth.Capability, err error) {
	if s.Health == nil || caller.Err() != nil || errors.Is(err, db.ErrRetrievalBudget) {
		return
	}
	if err == nil {
		s.Health.Recover(cap)
	} else {
		s.Health.Degrade(cap, errors.New(toolhealth.SanitizeModel(err.Error())))
	}
}

func modelFailure(err error) string {
	if errors.Is(err, db.ErrRetrievalBudget) {
		return "budget"
	}
	if err == nil {
		return "invalid model result"
	}
	return toolhealth.SanitizeModel(err.Error())
}
