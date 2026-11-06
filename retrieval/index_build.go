package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
)

var ErrEmbeddingUnconfigured = errors.New("semantic embedding service is not configured")

type IndexBuildRequest struct {
	TenantID, Actor, RequestKey string
	Dimension                   int
	Preprocessing               string
}

func (s *Service) SubmitIndexBuild(ctx context.Context, req IndexBuildRequest) (db.KnowledgeIndex, bool, error) {
	if s.Embedder == nil {
		return db.KnowledgeIndex{}, false, ErrEmbeddingUnconfigured
	}
	if req.Dimension == 0 {
		req.Dimension = s.embeddingDimension()
	}
	if req.Dimension < 1 || req.Dimension > 8192 {
		return db.KnowledgeIndex{}, false, fmt.Errorf("%w: dimension must be 1..8192", db.ErrInvalidKnowledge)
	}
	if req.Preprocessing == "" {
		req.Preprocessing = Preprocessing
	}
	if req.RequestKey == "" {
		req.RequestKey = "sys:index:" + key()
	}
	raw, _ := json.Marshal([]any{s.Embedder.Model(), req.Dimension, req.Preprocessing})
	duration := s.IndexBuildDeadline
	if duration <= 0 {
		duration = 2 * time.Hour
	}
	return s.DB.SubmitKnowledgeIndexBuild(ctx, db.KnowledgeIndexRequest{TenantID: tenant(req.TenantID), Model: s.Embedder.Model(),
		Dimension: req.Dimension, Preprocessing: req.Preprocessing, Actor: req.Actor, RequestKey: req.RequestKey,
		RequestHash: db.ContentHash(string(raw)), Deadline: time.Now().Add(duration)})
}

func (s *Service) ActivateIndex(ctx context.Context, tenant string, id int64, actor string) error {
	if s.Embedder == nil {
		return &db.IndexActivationError{Code: "model_unconfigured"}
	}
	x, err := s.DB.GetKnowledgeIndex(ctx, tenant, id)
	if errors.Is(err, db.ErrNotFound) {
		return &db.IndexActivationError{Code: "not_ready"}
	}
	if err != nil {
		return err
	}
	code := ""
	switch {
	case x.Model != s.Embedder.Model():
		code = "model_mismatch"
	case s.embeddingDimension() > 0 && x.Dimension != s.embeddingDimension():
		code = "dimension_mismatch"
	case x.Preprocessing != Preprocessing:
		code = "preprocessing_mismatch"
	}
	if code != "" {
		return &db.IndexActivationError{Code: code}
	}
	return s.DB.ActivateKnowledgeIndex(ctx, tenant, id, actor)
}

func (s *Service) BuildIndex(ctx context.Context, tenant, actor string, dimension int) (db.KnowledgeIndex, error) {
	return s.buildIndex(ctx, tenant, actor, dimension, Preprocessing)
}

func (s *Service) BuildBaselineIndex(ctx context.Context, tenant, actor string, dimension int) (db.KnowledgeIndex, error) {
	return s.buildIndex(ctx, tenant, actor, dimension, "title-path-body-v1")
}

func (s *Service) buildIndex(ctx context.Context, tenant, actor string, dimension int, prep string) (db.KnowledgeIndex, error) {
	x, _, err := s.SubmitIndexBuild(ctx, IndexBuildRequest{TenantID: tenant, Actor: actor, Dimension: dimension, Preprocessing: prep})
	if err != nil {
		return x, err
	}
	r := &IndexRunner{Service: s, TenantID: x.TenantID, Owner: "sys:index:" + key()}
	r.init()
	claimCtx, claimDone := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	claimed, err := s.DB.ClaimKnowledgeIndexBuildByID(claimCtx, x.TenantID, x.ID, r.Owner, r.Lease)
	claimDone()
	if err != nil {
		return x, err
	}
	if claimed == nil {
		return x, db.ErrLeaseLost
	}
	err = r.execute(ctx, *claimed, true)
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	latest, readErr := s.DB.GetKnowledgeIndex(readCtx, x.TenantID, x.ID)
	if readErr == nil {
		x = latest
	}
	if err == nil {
		err = readErr
	}
	return x, err
}

type IndexRunner struct {
	Service    *Service
	TenantID   string
	Owner      string
	Logger     *slog.Logger
	Poll       time.Duration
	Lease      time.Duration
	RetryDelay time.Duration
	OnChange   func(*db.KnowledgeIndex)
	once       sync.Once
	start      sync.Once
	wake       chan struct{}
}

func (r *IndexRunner) init() {
	r.once.Do(func() {
		if r.Poll <= 0 {
			r.Poll = 2 * time.Second
		}
		if r.Lease <= 0 {
			r.Lease = time.Minute
		}
		if r.RetryDelay <= 0 {
			r.RetryDelay = 30 * time.Second
		}
		if r.Owner == "" {
			r.Owner = "index:" + key()
		}
		r.wake = make(chan struct{}, 1)
	})
}

func (r *IndexRunner) Wake() {
	if r == nil {
		return
	}
	r.init()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *IndexRunner) Start(ctx context.Context) {
	r.init()
	r.start.Do(func() { go r.run(ctx) })
}

func (r *IndexRunner) run(ctx context.Context) {
	ticker := time.NewTicker(r.Poll)
	defer ticker.Stop()
	waiting := map[int64]db.KnowledgeIndex{}
	for ctx.Err() == nil {
		x, err := r.Service.DB.ClaimKnowledgeIndexBuild(ctx, r.TenantID, r.Owner, r.Lease)
		// Claim may terminalize an exhausted retry before returning the next job.
		// Keep observing released requests so that transition also reaches OnChange.
		for id, previous := range waiting {
			if current, e := r.Service.DB.GetKnowledgeIndex(ctx, previous.TenantID, id); e == nil && current.Status != "queued" {
				r.notify(ctx, current)
				delete(waiting, id)
			}
		}
		if err != nil && ctx.Err() == nil && r.Logger != nil {
			r.Logger.Warn("claim knowledge index failed", "err", toolhealth.SanitizeModel(err.Error()))
		}
		if x != nil && err == nil {
			r.notify(ctx, *x)
			_ = r.execute(ctx, *x, false)
			if r.OnChange != nil && ctx.Err() == nil {
				if current, e := r.Service.DB.GetKnowledgeIndex(ctx, x.TenantID, x.ID); e == nil && current.Status == "queued" {
					waiting[x.ID] = current
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-ticker.C:
		}
	}
}

func (r *IndexRunner) notify(ctx context.Context, x db.KnowledgeIndex) {
	if r.OnChange == nil {
		return
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if latest, err := r.Service.DB.GetKnowledgeIndex(readCtx, x.TenantID, x.ID); err == nil {
		x = latest
	}
	// UI/logging hooks must not break a durable build or its lease keeper.
	defer func() { _ = recover() }()
	r.OnChange(&x)
}

func (r *IndexRunner) execute(ctx context.Context, x db.KnowledgeIndex, synchronous bool) error {
	r.init()
	s := r.Service
	work := ctx
	deadlineCancel := func() {}
	if x.DeadlineAt != nil {
		work, deadlineCancel = context.WithDeadline(work, *x.DeadlineAt)
	}
	defer deadlineCancel()
	work, cancel := context.WithCancel(work)
	var lost atomic.Bool
	leased := x // heartbeat only reads the immutable lease snapshot, not the execution cursor
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(max(r.Lease/3, time.Millisecond))
		defer tick.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-tick.C:
				err := s.DB.HeartbeatKnowledgeIndexBuild(work, leased, r.Lease)
				if err != nil {
					if work.Err() == nil {
						lost.Store(true)
						cancel()
					}
					return
				}
			}
		}
	}()
	build := func() error {
		if s.Embedder == nil || x.Model != s.Embedder.Model() {
			return fmt.Errorf("model changed since index submission")
		}
		for {
			if err := work.Err(); err != nil {
				return err
			}
			chunks, err := s.DB.IndexChunks(work, x.TenantID, x.CursorChunkID, 32)
			if err != nil {
				return err
			}
			if len(chunks) == 0 {
				return nil
			}
			fingerprints := make(map[int64]string, len(chunks))
			for _, c := range chunks {
				fingerprints[c.Chunk.ID] = db.ChunkFingerprint(c.Document, c.Chunk)
			}
			reusable, err := s.DB.ReusableEmbeddings(work, x, fingerprints)
			if err != nil {
				return err
			}
			vectors := make([][]float64, len(chunks))
			texts := []string{}
			positions := []int{}
			reused := 0
			for i, c := range chunks {
				if v, ok := reusable[c.Chunk.ID]; ok && v.ContentHash == fingerprints[c.Chunk.ID] && db.ValidateVectors([][]float64{v.Vector}, 1, x.Dimension) == nil {
					vectors[i] = v.Vector
					reused++
					continue
				}
				text := c.Document.Title + " " + c.Chunk.SectionPath + " " + c.Chunk.Content
				if x.Preprocessing == Preprocessing {
					text = c.Document.Title + " " + c.Chunk.SectionPath + " " + string(c.Document.Extra) + " " + c.Chunk.Content
				}
				texts = append(texts, text)
				positions = append(positions, i)
			}
			if len(texts) > 0 {
				generated, e := s.Embedder.Embed(work, texts)
				if e == nil {
					e = db.ValidateVectors(generated, len(texts), x.Dimension)
				}
				s.modelHealth(work, toolhealth.Embedding, e)
				if e != nil {
					return e
				}
				for i, pos := range positions {
					vectors[pos] = generated[i]
				}
			}
			if err = s.DB.PutIndexBatch(work, x, chunks, vectors, len(texts), reused); err != nil {
				return err
			}
			s.modelHealth(ctx, toolhealth.Embedding, nil)
			x.CursorChunkID = chunks[len(chunks)-1].Chunk.ID
			x.EmbeddedChunks += len(texts)
			x.ReusedChunks += reused
			r.notify(ctx, x)
		}
	}
	err := build()
	// Stop and join the heartbeat before finishing; cancellation/lease loss cannot race a terminal write.
	cancel()
	<-heartbeatDone
	if lost.Load() || errors.Is(err, db.ErrLeaseLost) {
		r.notify(ctx, x)
		return db.ErrLeaseLost
	}
	if ctx.Err() != nil && !synchronous {
		return ctx.Err()
	}
	finishCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	code := ""
	switch {
	case ctx.Err() != nil:
		err, code = ctx.Err(), "INTERRUPTED"
	case x.DeadlineAt != nil && !x.DeadlineAt.After(time.Now()):
		err, code = context.DeadlineExceeded, "BUDGET_EXHAUSTED"
	case s.Embedder == nil || s.Embedder.Model() != x.Model:
		code = "RUNTIME_UNAVAILABLE"
	case errors.Is(err, ErrModelTransient):
		s.modelHealth(ctx, toolhealth.Embedding, err)
		if e := s.DB.ReleaseKnowledgeIndexBuild(finishCtx, x, time.Now().Add(r.RetryDelay), toolhealth.SanitizeModel(err.Error())); e != nil {
			return e
		}
		r.notify(ctx, x)
		return err
	case err != nil:
		code = "MODEL_ERROR"
	}
	var storedErr error
	if err != nil {
		storedErr = errors.New(toolhealth.SanitizeModel(err.Error()))
	}
	if e := s.DB.FinishKnowledgeIndexBuild(finishCtx, x, storedErr, code); e != nil {
		return e
	}
	r.notify(ctx, x)
	return err
}
