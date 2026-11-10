package server

// D2 证据可采性、承诺台账与最小报价门禁 API（Phase 7 前固定使用默认租户）。

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/autobid/autobid-engine/db"
)

func (a *API) registerD2Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/company/performance", a.listPerformance)
	mux.HandleFunc("POST /api/company/performance", a.upsertPerformance)
	mux.HandleFunc("DELETE /api/company/performance/{perfID}", a.deletePerformance)
	mux.HandleFunc("GET /api/company/evidence/{domain}/{ownerID}/originals", a.listEvidenceOriginals)
	mux.HandleFunc("POST /api/company/evidence/{domain}/{ownerID}/originals", a.addEvidenceOriginal)

	mux.HandleFunc("GET /api/tasks/{id}/requirements/{reqID}/evidence-bindings", a.withTask(a.listEvidenceBindings))
	mux.HandleFunc("POST /api/tasks/{id}/requirements/{reqID}/evidence-bindings", a.withTask(a.bindEvidence))
	mux.HandleFunc("DELETE /api/tasks/{id}/requirements/{reqID}/evidence-bindings/{domain}/{evidenceID}", a.withTask(a.unbindEvidence))

	mux.HandleFunc("GET /api/tasks/{id}/admissibility", a.withTask(a.listAdmissibility))
	mux.HandleFunc("GET /api/tasks/{id}/admissibility/{runID}", a.withTask(a.getAdmissibility))
	mux.HandleFunc("POST /api/tasks/{id}/admissibility/evaluate", a.withTask(a.evaluateAdmissibility))
	mux.HandleFunc("POST /api/tasks/{id}/admissibility/{runID}/resolve", a.withTask(a.resolveAdmissibility))

	mux.HandleFunc("GET /api/tasks/{id}/commitments", a.withTask(a.listCommitments))
	mux.HandleFunc("PUT /api/tasks/{id}/commitments/{code}", a.withTask(a.upsertCommitment))
	mux.HandleFunc("POST /api/tasks/{id}/commitments/{code}/confirm", a.withTask(a.confirmCommitment))
	mux.HandleFunc("POST /api/tasks/{id}/commitments/{code}/signoff", a.withTask(a.signOffCommitment))

	mux.HandleFunc("GET /api/tasks/{id}/quotes", a.withTask(a.listQuotes))
	mux.HandleFunc("POST /api/tasks/{id}/quotes", a.withTask(a.createQuote))
	mux.HandleFunc("POST /api/tasks/{id}/quotes/{quoteID}/validate", a.withTask(a.validateQuote))
	mux.HandleFunc("POST /api/tasks/{id}/quotes/{quoteID}/confirm", a.withTask(a.confirmQuote))

	mux.HandleFunc("GET /api/tasks/{id}/objective-scores", a.withTask(a.listObjectiveScores))
	mux.HandleFunc("POST /api/tasks/{id}/objective-scores/compute", a.withTask(a.computeObjectiveScores))
}

func d2Fail(w http.ResponseWriter, err error) {
	proposalFail(w, err)
}

func (a *API) listPerformance(w http.ResponseWriter, r *http.Request) {
	include := r.URL.Query().Get("include_deleted") == "1"
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := a.pg.ListCompanyPerformance(r.Context(), db.DefaultTenant, include, limit)
	if err != nil {
		fail(w, err)
		return
	}
	if out == nil {
		out = []*db.CompanyPerformance{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) upsertPerformance(w http.ResponseWriter, r *http.Request) {
	var p db.CompanyPerformance
	if err := decodeBody(r, &p); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	p.TenantID, p.CreatedBy = db.DefaultTenant, operator(r)
	saved, created, err := a.pg.UpsertCompanyPerformance(r.Context(), &p)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, saved)
}

func (a *API) deletePerformance(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "perfID")
	if err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.DeleteCompanyPerformanceScoped(r.Context(), db.DefaultTenant, id, operator(r)); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listEvidenceOriginals(w http.ResponseWriter, r *http.Request) {
	ownerID, err := pathID(r, "ownerID")
	if err != nil {
		fail(w, err)
		return
	}
	domain := r.PathValue("domain")
	out, err := a.pg.ListCompanyEvidenceOriginals(r.Context(), db.DefaultTenant, domain, ownerID, r.URL.Query().Get("current") != "0")
	if err != nil {
		fail(w, err)
		return
	}
	if out == nil {
		out = []*db.CompanyEvidenceOriginal{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) addEvidenceOriginal(w http.ResponseWriter, r *http.Request) {
	ownerID, err := pathID(r, "ownerID")
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		DocumentKind   string `json:"document_kind"`
		PageLabel      string `json:"page_label"`
		OriginalPath   string `json:"original_path"`
		OriginalSHA256 string `json:"original_sha256"`
		PageStart      int    `json:"page_start"`
		PageEnd        int    `json:"page_end"`
		Supersede      bool   `json:"supersede"`
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			fail(w, badRequest("证明原件上传失败，请检查文件大小（上限 32 MiB）"))
			return
		}
		defer r.MultipartForm.RemoveAll()
		in.DocumentKind, in.PageLabel = r.FormValue("document_kind"), r.FormValue("page_label")
		in.PageStart, _ = strconv.Atoi(r.FormValue("page_start"))
		in.PageEnd, _ = strconv.Atoi(r.FormValue("page_end"))
		domain := r.PathValue("domain")
		if domain != db.EvidenceDomainCredential && domain != db.EvidenceDomainPerformance {
			fail(w, badRequest("请选择资信或业绩记录"))
			return
		}
		validKind := map[string]bool{"certificate": true, "contract": true, "acceptance_report": true, "social_security": true, "product_catalog": true, "other": true}
		if !validKind[in.DocumentKind] || in.PageStart < 1 || in.PageEnd < in.PageStart {
			fail(w, badRequest("请选择证明材料类型并填写有效页码范围"))
			return
		}
		exists, err := a.pg.EvidenceOwnerExists(r.Context(), db.DefaultTenant, domain, ownerID)
		if err != nil {
			fail(w, err)
			return
		}
		if !exists {
			fail(w, db.ErrNotFound)
			return
		}
		if a.cfg.Evidence == nil {
			fail(w, errors.New("证明材料存储未配置，请联系管理员"))
			return
		}
		f, header, err := r.FormFile("file")
		if err != nil {
			fail(w, badRequest("请选择证明材料原件"))
			return
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil || len(data) == 0 {
			fail(w, badRequest("证明材料原件为空或读取失败"))
			return
		}
		in.OriginalPath, in.OriginalSHA256, err = a.cfg.Evidence.Put(db.DefaultTenant, filepath.Ext(header.Filename), data)
		if err != nil {
			fail(w, errors.New("证明材料原件未能保存，请检查存储空间后重试"))
			return
		}
		in.Supersede = r.FormValue("supersede") == "true"
	} else if err := decodeBody(r, &in); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	saved, err := a.pg.AddCompanyEvidenceOriginal(r.Context(), &db.CompanyEvidenceOriginal{TenantID: db.DefaultTenant,
		OwnerDomain: r.PathValue("domain"), OwnerID: ownerID, DocumentKind: in.DocumentKind, PageLabel: in.PageLabel,
		OriginalPath: in.OriginalPath, OriginalSHA256: in.OriginalSHA256, PageStart: in.PageStart, PageEnd: in.PageEnd,
		CreatedBy: operator(r)}, in.Supersede)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (a *API) listEvidenceBindings(w http.ResponseWriter, r *http.Request, t *Task) {
	reqID, err := pathID(r, "reqID")
	if err != nil {
		fail(w, err)
		return
	}
	out, err := a.pg.ListRequirementEvidenceBindings(r.Context(), db.DefaultTenant, t.ID, reqID)
	if err != nil {
		d2Fail(w, err)
		return
	}
	if out == nil {
		out = []*db.RequirementEvidenceBinding{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) bindEvidence(w http.ResponseWriter, r *http.Request, t *Task) {
	reqID, err := pathID(r, "reqID")
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		EvidenceDomain string `json:"evidence_domain"`
		EvidenceID     int64  `json:"evidence_id"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	out, err := a.pg.BindRequirementEvidence(r.Context(), db.DefaultTenant, t.ID, reqID, in.EvidenceDomain, in.EvidenceID, operator(r))
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) unbindEvidence(w http.ResponseWriter, r *http.Request, t *Task) {
	reqID, err := pathID(r, "reqID")
	if err != nil {
		fail(w, err)
		return
	}
	evidenceID, err := pathID(r, "evidenceID")
	if err != nil {
		fail(w, err)
		return
	}
	if err := a.pg.UnbindRequirementEvidence(r.Context(), db.DefaultTenant, t.ID, reqID, r.PathValue("domain"), evidenceID); err != nil {
		d2Fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listAdmissibility(w http.ResponseWriter, r *http.Request, t *Task) {
	reqID, _ := strconv.ParseInt(r.URL.Query().Get("requirement_id"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := a.pg.ListAdmissibilityRuns(r.Context(), db.DefaultTenant, t.ID, reqID, limit)
	if err != nil {
		d2Fail(w, err)
		return
	}
	if out == nil {
		out = []*db.AdmissibilityRun{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getAdmissibility(w http.ResponseWriter, r *http.Request, t *Task) {
	runID, err := pathID(r, "runID")
	if err != nil {
		fail(w, err)
		return
	}
	out, err := a.pg.GetAdmissibilityRun(r.Context(), db.DefaultTenant, t.ID, runID)
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) evaluateAdmissibility(w http.ResponseWriter, r *http.Request, t *Task) {
	var in struct {
		RequirementID int64 `json:"requirement_id"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	if in.RequirementID <= 0 {
		fail(w, badRequest("requirement_id is required"))
		return
	}
	out, err := a.pg.EvaluateAdmissibility(r.Context(), db.DefaultTenant, t.ID, in.RequirementID, a.cfg.Evidence, operator(r))
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) resolveAdmissibility(w http.ResponseWriter, r *http.Request, t *Task) {
	runID, err := pathID(r, "runID")
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Decisions []db.AdmissibilityDecision `json:"decisions"`
		Reason    string                     `json:"reason"`
	}
	if err := decodeBody(r, &in); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	if len(in.Decisions) == 0 {
		fail(w, badRequest("decisions are required"))
		return
	}
	out, err := a.pg.ResolveAdmissibility(r.Context(), db.DefaultTenant, t.ID, runID, in.Decisions, operator(r))
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listCommitments(w http.ResponseWriter, r *http.Request, t *Task) {
	out, err := a.pg.ListCommitments(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		d2Fail(w, err)
		return
	}
	if out == nil {
		out = []*db.Commitment{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) upsertCommitment(w http.ResponseWriter, r *http.Request, t *Task) {
	var in db.CommitmentInput
	if err := decodeBody(r, &in); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	in.Code = r.PathValue("code")
	out, err := a.pg.UpsertCommitment(r.Context(), db.DefaultTenant, t.ID, in, operator(r))
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) confirmCommitment(w http.ResponseWriter, r *http.Request, t *Task) {
	out, err := a.pg.ConfirmCommitment(r.Context(), db.DefaultTenant, t.ID, r.PathValue("code"), operator(r))
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) signOffCommitment(w http.ResponseWriter, r *http.Request, t *Task) {
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeBody(r, &in); err != nil && !errors.Is(err, io.EOF) {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	out, err := a.pg.SignOffCommitment(r.Context(), db.DefaultTenant, t.ID, r.PathValue("code"), operator(r), in.Note)
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listQuotes(w http.ResponseWriter, r *http.Request, t *Task) {
	out, err := a.pg.ListBidQuotes(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		d2Fail(w, err)
		return
	}
	if out == nil {
		out = []*db.BidQuote{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) createQuote(w http.ResponseWriter, r *http.Request, t *Task) {
	var in db.BidQuoteInput
	if err := decodeBody(r, &in); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	out, err := a.pg.CreateBidQuote(r.Context(), db.DefaultTenant, t.ID, in, operator(r))
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) validateQuote(w http.ResponseWriter, r *http.Request, t *Task) {
	quoteID, err := pathID(r, "quoteID")
	if err != nil {
		fail(w, err)
		return
	}
	out, err := a.pg.ValidateBidQuote(r.Context(), db.DefaultTenant, t.ID, quoteID)
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) confirmQuote(w http.ResponseWriter, r *http.Request, t *Task) {
	quoteID, err := pathID(r, "quoteID")
	if err != nil {
		fail(w, err)
		return
	}
	out, val, err := a.pg.ConfirmBidQuote(r.Context(), db.DefaultTenant, t.ID, quoteID, operator(r))
	if err != nil {
		if val != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "code": "quote_inconsistent", "validation": val})
			return
		}
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quote": out, "validation": val})
}

func (a *API) listObjectiveScores(w http.ResponseWriter, r *http.Request, t *Task) {
	out, err := a.pg.LatestObjectiveScores(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		d2Fail(w, err)
		return
	}
	if out == nil {
		out = []*db.ObjectiveScoreRun{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) computeObjectiveScores(w http.ResponseWriter, r *http.Request, t *Task) {
	var in struct {
		RequirementID int64 `json:"requirement_id"`
	}
	if err := decodeBody(r, &in); err != nil && !errors.Is(err, io.EOF) {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	out, err := a.pg.ComputeObjectiveScores(r.Context(), db.DefaultTenant, t.ID, in.RequirementID, operator(r))
	if err != nil {
		d2Fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
