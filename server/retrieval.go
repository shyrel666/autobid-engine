package server

import (
	"encoding/json"
	"errors"
	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/retrieval"
	"github.com/autobid/autobid-engine/toolhealth"
	"net/http"
	"strconv"
	"strings"
)

func (a *API) researchTask(w http.ResponseWriter, r *http.Request, t *Task) {
	var req retrieval.ResearchRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req) != nil {
		fail(w, badRequest("invalid research request"))
		return
	}
	c := &retrieval.Coordinator{Service: a.cfg.Retrieval}
	if a.llm != nil {
		c.MultiEnabled = a.llm.cfg.MultiRetrieval
		c.Reasoner = &agent.RetrievalReasoner{Planner: &taskLLMRuntime{s: a.llm, task: t, agentKey: agent.AgentPlanner}, Worker: &taskLLMRuntime{s: a.llm, task: t, agentKey: agent.AgentWorker}}
	}
	result, err := c.Run(r.Context(), db.DefaultTenant, t.ID, operator(r), req)
	if err != nil {
		retrievalError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) taskRetrievalRuns(w http.ResponseWriter, r *http.Request, t *Task) {
	list, err := a.pg.TaskRetrievalRuns(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}
func (a *API) cancelRetrievalRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "runID")
	if err == nil {
		err = a.pg.CancelRetrievalRun(r.Context(), db.DefaultTenant, id, operator(r))
	}
	if err != nil {
		retrievalError(w, err)
		return
	}
	w.WriteHeader(204)
}

func retrievalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrRetrievalConflict):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, db.ErrRetrievalBudget):
		writeError(w, http.StatusTooManyRequests, err)
	default:
		fail(w, err)
	}
}
func (a *API) listKnowledgeIndexes(w http.ResponseWriter, r *http.Request) {
	x, err := a.pg.KnowledgeIndexes(r.Context(), db.DefaultTenant)
	if err != nil {
		indexBuildError(w, err)
		return
	}
	for i := range x {
		x[i] = retrieval.SanitizedIndex(x[i])
	}
	writeJSON(w, 200, x)
}
func (a *API) buildKnowledgeIndex(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Retrieval.Embedder == nil {
		indexBuildError(w, retrieval.ErrEmbeddingUnconfigured)
		return
	}
	var req struct {
		RequestKey string `json:"request_key"`
		Dimension  *int   `json:"dimension"`
	}
	if decodeBodyLimit(r, &req, 4096) != nil {
		indexBuildError(w, badRequest("invalid index build request"))
		return
	}
	req.RequestKey = strings.TrimSpace(req.RequestKey)
	if strings.HasPrefix(req.RequestKey, db.ReservedRequestKeyPrefix) || len(req.RequestKey) > 160 || req.Dimension != nil && (*req.Dimension < 1 || *req.Dimension > 8192) {
		indexBuildError(w, badRequest("request_key is reserved or too long, or dimension is outside 1..8192"))
		return
	}
	dimension := 0
	if req.Dimension != nil {
		dimension = *req.Dimension
	}
	x, replayed, err := a.cfg.Retrieval.SubmitIndexBuild(r.Context(), retrieval.IndexBuildRequest{TenantID: db.DefaultTenant, Actor: operator(r), RequestKey: req.RequestKey, Dimension: dimension})
	if err != nil {
		if errors.Is(err, db.ErrIndexBuildActive) {
			writeJSON(w, 409, map[string]any{"error": db.ErrIndexBuildActive.Error(), "code": "build_active", "index_id": x.ID})
		} else {
			indexBuildError(w, err)
		}
		return
	}
	a.cfg.IndexRunner.Wake()
	writeJSON(w, 202, map[string]any{"index": retrieval.SanitizedIndex(x), "replayed": replayed})
}
func (a *API) activateKnowledgeIndex(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "indexID")
	if err == nil {
		err = a.cfg.Retrieval.ActivateIndex(r.Context(), db.DefaultTenant, id, operator(r))
	}
	if err != nil {
		indexBuildError(w, err)
		return
	}
	w.WriteHeader(204)
}

func indexBuildError(w http.ResponseWriter, err error) {
	var activation *db.IndexActivationError
	if errors.As(err, &activation) {
		writeJSON(w, 409, map[string]string{"error": activation.Error(), "code": activation.Code})
		return
	}
	codes := []struct {
		err    error
		status int
		code   string
	}{
		{retrieval.ErrEmbeddingUnconfigured, 503, "runtime_unavailable"},
		{db.ErrIndexBuildActive, 409, "build_active"},
		{db.ErrIdempotencyKeyConflict, 409, "idempotency_conflict"},
		{db.ErrIndexNothingToBuild, 422, "nothing_to_index"},
		{db.ErrInvalidKnowledge, 400, "invalid_argument"},
		{db.ErrNotFound, 404, "not_found"},
	}
	for _, c := range codes {
		if errors.Is(err, c.err) {
			writeJSON(w, c.status, map[string]string{"error": toolhealth.SanitizeModel(err.Error()), "code": c.code})
			return
		}
	}
	var he *httpError
	if errors.As(err, &he) {
		writeJSON(w, he.status, map[string]string{"error": toolhealth.SanitizeModel(he.Error()), "code": "invalid_argument"})
		return
	}
	writeJSON(w, 500, map[string]string{"error": "knowledge index operation failed", "code": "internal_error"})
}

func (a *API) retrievalStatus(w http.ResponseWriter, r *http.Request) {
	status, err := a.cfg.Retrieval.Status(r.Context(), db.DefaultTenant)
	if err != nil {
		indexBuildError(w, err)
		return
	}
	if a.llm != nil {
		status.MultiAgentEnabled = a.llm.cfg.MultiRetrieval
	}
	writeJSON(w, 200, status)
}

func (a *API) cancelKnowledgeIndex(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "indexID")
	if err == nil {
		err = a.pg.CancelKnowledgeIndexBuild(r.Context(), db.DefaultTenant, id, operator(r))
	}
	if errors.Is(err, db.ErrRetrievalConflict) {
		writeJSON(w, 409, map[string]string{"error": "index build is already terminal", "code": "already_terminal"})
		return
	}
	if err != nil {
		indexBuildError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) deleteKnowledgeIndex(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "indexID")
	if err == nil {
		err = a.pg.DeleteKnowledgeIndex(r.Context(), db.DefaultTenant, id, operator(r))
	}
	if errors.Is(err, db.ErrRetrievalConflict) {
		writeJSON(w, 409, map[string]string{"error": "index is active or building", "code": "index_in_use"})
		return
	}
	if err != nil {
		indexBuildError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) knowledgeIndexEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			indexBuildError(w, badRequest("limit must be positive"))
			return
		}
		limit = min(n, 200)
	}
	list, err := a.pg.KnowledgeIndexEvents(r.Context(), db.DefaultTenant, limit)
	if err != nil {
		indexBuildError(w, err)
		return
	}
	for i := range list {
		list[i].Model = toolhealth.SanitizeModel(list[i].Model)
		list[i].Detail = sanitizeIndexEventDetail(list[i].Detail)
	}
	writeJSON(w, 200, list)
}

func sanitizeIndexEventDetail(raw json.RawMessage) json.RawMessage {
	// Sanitize string values before re-encoding so redaction cannot corrupt JSON.
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return json.RawMessage(`{}`)
	}
	var clean func(any) any
	clean = func(v any) any {
		switch x := v.(type) {
		case string:
			return toolhealth.SanitizeModel(x)
		case map[string]any:
			for k, v := range x {
				x[k] = clean(v)
			}
			return x
		case []any:
			for i, v := range x {
				x[i] = clean(v)
			}
			return x
		default:
			return v
		}
	}
	out, _ := json.Marshal(clean(value))
	return out
}
func (a *API) getRetrievalRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "runID")
	if err != nil {
		fail(w, err)
		return
	}
	run, err := a.pg.GetRetrievalRun(r.Context(), db.DefaultTenant, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, run)
}
func (a *API) getRetrievalEvidence(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "runID")
	if err != nil {
		fail(w, err)
		return
	}
	if _, err = a.pg.GetRetrievalRun(r.Context(), db.DefaultTenant, id); err != nil {
		fail(w, err)
		return
	}
	list, err := a.cfg.Retrieval.Evidence(r.Context(), db.DefaultTenant, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}
func (a *API) verifyRetrievalEvidence(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "runID")
	if err != nil {
		fail(w, err)
		return
	}
	eid, err := pathID(r, "evidenceID")
	if err != nil {
		fail(w, err)
		return
	}
	var claim retrieval.Claim
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&claim); err != nil {
		fail(w, badRequest("invalid claim"))
		return
	}
	e, err := a.cfg.Retrieval.Verify(r.Context(), db.DefaultTenant, operator(r), id, eid, claim)
	if err != nil {
		retrievalError(w, err)
		return
	}
	writeJSON(w, 200, e)
}
