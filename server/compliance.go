package server

// Phase 6A：双层响应核验矩阵、全文一致性核验与导出就绪度 API（标书正文域）。
//   POST /api/tasks/{id}/compliance/verify                         {section_id?} 核验当前批准大纲（或单章）的正文实质响应
//   GET  /api/tasks/{id}/compliance/matrix                         ?status=pending|verified|stale|failed&kind=star|triangle|scoring|required|format&section_id=
//   GET  /api/tasks/{id}/compliance/coverage                       两层覆盖率（大纲映射 / 正文实质响应，按要求类型拆分）
//   POST /api/tasks/{id}/consistency/runs                          执行全文一致性核验（REPEATABLE READ 快照）
//   GET  /api/tasks/{id}/consistency/runs                          运行列表（新到旧）
//   GET  /api/tasks/{id}/consistency/findings                      ?run_id=&status=open|resolved|stale；缺省为最新运行
//   POST /api/tasks/{id}/consistency/findings/{findingID}/resolve  {version, resolution_kind, note} 乐观锁处理
//   GET  /api/tasks/{id}/export-readiness                          只读：列出全部阻止导出的原因（判定在 db.ExportReadiness）
// 状态码：参数错误 400；不存在 404；结论版本冲突 / 已失效 409（冲突时响应体携带当前结论）；无批准大纲等前置条件 422。

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

func (a *API) registerComplianceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/tasks/{id}/compliance/verify", a.withTask(a.verifyCompliance))
	mux.HandleFunc("GET /api/tasks/{id}/compliance/matrix", a.withTask(a.complianceMatrix))
	mux.HandleFunc("GET /api/tasks/{id}/compliance/coverage", a.withTask(a.complianceCoverage))
	mux.HandleFunc("POST /api/tasks/{id}/consistency/runs", a.withTask(a.runConsistency))
	mux.HandleFunc("GET /api/tasks/{id}/consistency/runs", a.withTask(a.listConsistencyRuns))
	mux.HandleFunc("GET /api/tasks/{id}/consistency/findings", a.withTask(a.consistencyFindings))
	mux.HandleFunc("POST /api/tasks/{id}/consistency/findings/{findingID}/resolve", a.withTask(a.resolveFinding))
	mux.HandleFunc("GET /api/tasks/{id}/export-readiness", a.withTask(a.exportReadiness))
}

func (a *API) complianceVerifier() compliance.Verifier {
	if a.cfg.ComplianceVerifier != nil {
		return a.cfg.ComplianceVerifier
	}
	return compliance.RuleVerifier{}
}

// complianceFail 在域 C 错误映射之外补充 6A 结论错误。
func complianceFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrInvalidFinding):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_finding"})
	case errors.Is(err, db.ErrFindingStale):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "finding_stale"})
	default:
		proposalFail(w, err)
	}
}

func queryID(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest("invalid %s %q", name, v)
	}
	return id, nil
}

func oneOf(v string, allowed ...string) bool {
	if v == "" {
		return true
	}
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func (a *API) verifyCompliance(w http.ResponseWriter, r *http.Request, t *Task) {
	var req struct {
		SectionID int64 `json:"section_id"`
	}
	if r.ContentLength != 0 {
		if err := decodeBody(r, &req); err != nil {
			complianceFail(w, err)
			return
		}
	}
	if req.SectionID < 0 {
		complianceFail(w, badRequest("invalid section_id %d", req.SectionID))
		return
	}
	res, err := a.pg.VerifyComplianceMatrix(r.Context(), db.DefaultTenant, t.ID, req.SectionID, a.complianceVerifier(), operator(r))
	if err != nil {
		complianceFail(w, err)
		return
	}
	a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "proposal", Kind: "state", At: time.Now(),
		Summary: fmt.Sprintf("响应矩阵核验 v%d：%d 行，通过 %d、未通过 %d、待写 %d、失效 %d", res.OutlineVersion, res.Checked, res.Verified, res.Failed, res.Pending, res.Stale)})
	writeJSON(w, http.StatusOK, res)
}

func (a *API) complianceMatrix(w http.ResponseWriter, r *http.Request, t *Task) {
	q := r.URL.Query()
	f := db.MatrixFilter{Status: q.Get("status"), Kind: q.Get("kind")}
	if !oneOf(f.Status, compliance.StatusPending, compliance.StatusVerified, compliance.StatusStale, compliance.StatusFailed) {
		complianceFail(w, badRequest("invalid status %q", f.Status))
		return
	}
	if !oneOf(f.Kind, compliance.KindStar, compliance.KindTriangle, compliance.KindScoring, compliance.KindRequired, compliance.KindFormat) {
		complianceFail(w, badRequest("invalid kind %q", f.Kind))
		return
	}
	sid, err := queryID(r, "section_id")
	if err != nil {
		complianceFail(w, err)
		return
	}
	f.SectionID = sid
	v, err := a.pg.ListComplianceMatrix(r.Context(), db.DefaultTenant, t.ID, f)
	if err != nil {
		complianceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) complianceCoverage(w http.ResponseWriter, r *http.Request, t *Task) {
	v, err := a.pg.ComplianceCoverage(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		complianceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) runConsistency(w http.ResponseWriter, r *http.Request, t *Task) {
	det, err := a.pg.RunConsistencyCheck(r.Context(), db.DefaultTenant, t.ID, operator(r))
	if err != nil {
		complianceFail(w, err)
		return
	}
	a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "proposal", Kind: "state", At: time.Now(),
		Summary: fmt.Sprintf("全文一致性核验 #%d：矛盾 %d、漏项 %d", det.Run.ID, det.Run.Contradictions, det.Run.Omissions)})
	writeJSON(w, http.StatusCreated, det)
}

func (a *API) listConsistencyRuns(w http.ResponseWriter, r *http.Request, t *Task) {
	runs, err := a.pg.ListConsistencyRuns(r.Context(), db.DefaultTenant, t.ID, 20)
	if err != nil {
		complianceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (a *API) consistencyFindings(w http.ResponseWriter, r *http.Request, t *Task) {
	status := r.URL.Query().Get("status")
	if !oneOf(status, db.FindingOpen, db.FindingResolved, db.FindingStale) {
		complianceFail(w, badRequest("invalid status %q", status))
		return
	}
	runID, err := queryID(r, "run_id")
	if err != nil {
		complianceFail(w, err)
		return
	}
	det, err := a.pg.GetConsistencyRun(r.Context(), db.DefaultTenant, t.ID, runID, status)
	if errors.Is(err, db.ErrNotFound) && runID == 0 {
		writeJSON(w, http.StatusOK, db.ConsistencyRunDetail{Findings: []*db.ConsistencyFinding{}})
		return
	}
	if err != nil {
		complianceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, det)
}

func (a *API) resolveFinding(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "findingID")
	if err != nil {
		complianceFail(w, err)
		return
	}
	var in db.ResolveFindingInput
	if err := decodeBody(r, &in); err != nil {
		complianceFail(w, err)
		return
	}
	f, err := a.pg.ResolveConsistencyFinding(r.Context(), db.DefaultTenant, t.ID, id, in, operator(r))
	if errors.Is(err, db.ErrFindingConflict) {
		cur, _ := a.pg.GetConsistencyFinding(r.Context(), db.DefaultTenant, t.ID, id)
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "finding_conflict", "finding": cur})
		return
	}
	if err != nil {
		complianceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (a *API) exportReadiness(w http.ResponseWriter, r *http.Request, t *Task) {
	v, err := a.pg.ExportReadiness(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		complianceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
