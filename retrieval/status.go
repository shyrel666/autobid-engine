package retrieval

import (
	"context"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
)

const (
	IndexStateCurrent          = "current"
	IndexStateUnconfigured     = "unconfigured"
	IndexStateMissing          = "missing"
	IndexStateModelChanged     = "model_changed"
	IndexStateDimensionChanged = "dimension_changed"
	IndexStatePrepChanged      = "preprocessing_changed"
	IndexStateDocsChanged      = "documents_changed"
)

type Diagnostics struct {
	Mode               string            `json:"mode"`
	EmbeddingModel     string            `json:"embedding_model,omitempty"`
	RerankerModel      string            `json:"reranker_model,omitempty"`
	Preprocessing      string            `json:"preprocessing,omitempty"`
	IndexState         string            `json:"index_state"`
	FusedCandidates    int               `json:"fused_candidates"`
	RerankedCandidates int               `json:"reranked_candidates"`
	Failures           map[string]string `json:"failures,omitempty"`
}

func (s *Service) embeddingDimension() int {
	if m, ok := s.Embedder.(*HTTPModel); ok {
		return m.Dimension
	}
	return 0
}

// IndexState is the shared model/index compatibility gate for queries and status views.
func (s *Service) IndexState(idx *db.KnowledgeIndex, epoch int64) string {
	switch {
	case s.Embedder == nil:
		return IndexStateUnconfigured
	case idx == nil:
		return IndexStateMissing
	case idx.Model != s.Embedder.Model():
		return IndexStateModelChanged
	case s.embeddingDimension() > 0 && s.embeddingDimension() != idx.Dimension:
		return IndexStateDimensionChanged
	case idx.Preprocessing != Preprocessing:
		return IndexStatePrepChanged
	case idx.Epoch != epoch:
		return IndexStateDocsChanged
	default:
		return IndexStateCurrent
	}
}

type RetrievalStatus struct {
	State             string             `json:"state"`
	Reasons           []string           `json:"reasons"`
	IndexState        string             `json:"index_state"`
	Epoch             int64              `json:"epoch"`
	Chunks            int                `json:"chunks"`
	Strategy          string             `json:"strategy"`
	Preprocessing     string             `json:"preprocessing"`
	MultiAgentEnabled bool               `json:"multi_agent_enabled"`
	Embedding         ModelStatus        `json:"embedding"`
	Reranker          ModelStatus        `json:"reranker"`
	ActiveIndex       *db.KnowledgeIndex `json:"active_index,omitempty"`
	ActiveBuild       *db.KnowledgeIndex `json:"active_build,omitempty"`
	Indexes           []IndexView        `json:"indexes"`
	CheckedAt         time.Time          `json:"checked_at"`
}

type ModelStatus struct {
	Configured bool              `json:"configured"`
	Health     toolhealth.Status `json:"health"`
	Info       *ModelInfo        `json:"info,omitempty"`
}

type IndexView struct {
	db.KnowledgeIndex
	State       string `json:"state"`
	CanActivate bool   `json:"can_activate"`
	CanDelete   bool   `json:"can_delete"`
}

func (s *Service) modelStatus(model interface{ Model() string }, cap toolhealth.Capability) ModelStatus {
	m := ModelStatus{Configured: model != nil, Health: toolhealth.Status{Capability: cap, State: toolhealth.Unconfigured}}
	if s.Health != nil {
		m.Health = s.Health.Get(cap)
	} else if m.Configured {
		m.Health.State, m.Health.Available = toolhealth.Healthy, true
	}
	m.Health.Provider = toolhealth.SanitizeModel(m.Health.Provider)
	m.Health.Version = toolhealth.SanitizeModel(m.Health.Version)
	m.Health.Reason = toolhealth.SanitizeModel(m.Health.Reason)
	if model != nil {
		i := ModelInfo{ID: model.Model(), Name: model.Model()}
		if h, ok := model.(*HTTPModel); ok {
			i = h.Info()
		}
		i.ID, i.Name, i.Revision, i.ServedVersion = toolhealth.SanitizeModel(i.ID), toolhealth.SanitizeModel(i.Name), toolhealth.SanitizeModel(i.Revision), toolhealth.SanitizeModel(i.ServedVersion)
		m.Info = &i
	}
	return m
}

// SanitizedIndex removes private runtime error details without mutating the stored audit record.
func SanitizedIndex(x db.KnowledgeIndex) db.KnowledgeIndex {
	x.Error = toolhealth.SanitizeModel(x.Error)
	x.Model = toolhealth.SanitizeModel(x.Model)
	return x
}

func (s *Service) Status(ctx context.Context, t string) (RetrievalStatus, error) {
	r := RetrievalStatus{Reasons: []string{}, Indexes: []IndexView{}, Strategy: Strategy, Preprocessing: Preprocessing, CheckedAt: time.Now().UTC()}
	var err error
	if r.Epoch, err = s.DB.KnowledgeEpoch(ctx, t); err != nil {
		return r, err
	}
	if r.Chunks, err = s.DB.KnowledgeChunkCount(ctx, t); err != nil {
		return r, err
	}
	indexes, err := s.DB.KnowledgeIndexes(ctx, t)
	if err != nil {
		return r, err
	}
	r.Embedding, r.Reranker = s.modelStatus(s.Embedder, toolhealth.Embedding), s.modelStatus(s.Reranker, toolhealth.Reranker)
	var active *db.KnowledgeIndex
	for _, x := range indexes {
		if x.Active {
			copy := x
			active = &copy
			break
		}
	}
	r.IndexState = s.IndexState(active, r.Epoch)
	for _, x := range indexes {
		state := s.IndexState(&x, r.Epoch)
		v := IndexView{KnowledgeIndex: SanitizedIndex(x), CanDelete: !x.Active && x.Status != "queued" && x.Status != "building"}
		v.CanActivate = x.Status == "ready" && !x.Active && state == IndexStateCurrent
		switch {
		case x.Status != "ready":
			v.State = x.Status
		case state == IndexStateUnconfigured || state == IndexStateModelChanged:
			v.State = "model_mismatch"
		case state == IndexStateDimensionChanged:
			v.State = "dimension_mismatch"
		case state == IndexStatePrepChanged:
			v.State = "preprocessing_mismatch"
		case state == IndexStateDocsChanged:
			v.State = "documents_changed"
		case v.CanActivate:
			v.State = "rollback_ready"
		default:
			v.State = "current"
		}
		r.Indexes = append(r.Indexes, v)
		copy := v.KnowledgeIndex
		if x.Active {
			r.ActiveIndex = &copy
		}
		if x.Status == "queued" || x.Status == "building" {
			r.ActiveBuild = &copy
		}
	}
	if r.Chunks == 0 {
		r.State = "empty"
		return r, nil
	}
	r.State = "semantic"
	if r.IndexState != IndexStateCurrent {
		r.State = "lexical_only"
		code := "semantic_index_missing_or_stale"
		if r.IndexState == IndexStateUnconfigured {
			code = "semantic_model_unconfigured"
		}
		r.Reasons = append(r.Reasons, code, "index_"+r.IndexState)
	} else if r.Embedding.Health.State != toolhealth.Healthy {
		r.State = "degraded"
		r.Reasons = append(r.Reasons, "embedding_"+string(r.Embedding.Health.State))
	}
	if !r.Reranker.Configured {
		r.Reasons = append(r.Reasons, "reranker_unconfigured")
		if r.State == "semantic" {
			r.State = "degraded"
		}
	} else if r.Reranker.Health.State != toolhealth.Healthy {
		r.Reasons = append(r.Reasons, "reranker_"+string(r.Reranker.Health.State))
		if r.State == "semantic" {
			r.State = "degraded"
		}
	}
	return r, nil
}
