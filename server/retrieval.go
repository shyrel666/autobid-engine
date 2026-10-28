package server

import (
	"encoding/json"
	"errors"
	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/retrieval"
	"net/http"
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
		fail(w, err)
		return
	}
	writeJSON(w, 200, x)
}
func (a *API) buildKnowledgeIndex(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Retrieval.Embedder == nil {
		writeError(w, 503, errors.New("semantic embedding service is not configured"))
		return
	}
	var req struct {
		Dimension int `json:"dimension"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || req.Dimension < 1 || req.Dimension > 8192 {
		fail(w, badRequest("dimension must be 1..8192"))
		return
	}
	x, err := a.cfg.Retrieval.BuildIndex(r.Context(), db.DefaultTenant, operator(r), req.Dimension)
	if err != nil {
		retrievalError(w, err)
		return
	}
	writeJSON(w, 201, x)
}
func (a *API) activateKnowledgeIndex(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "indexID")
	if err == nil {
		err = a.pg.ActivateKnowledgeIndex(r.Context(), db.DefaultTenant, id, operator(r))
	}
	if err != nil {
		retrievalError(w, err)
		return
	}
	w.WriteHeader(204)
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
