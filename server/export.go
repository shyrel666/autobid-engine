package server

// Phase 6B：锁定版本导出 API（导出附件域；章节图表路由见 server/charts.go）。
//   GET    /api/tasks/{id}/exports/preflight[?mode=final] 导出门禁明细：6A 阻断项 + 图表 / 外部依赖提示 + 可用模板；
//                                                         mode=final 按严格最终稿策略另行阻断未渲染图表与文档引擎不可用
//   POST   /api/tasks/{id}/exports                        {formats, template_id?, bidder?, request_key?, mode?, expected_outline_version?}
//                                                         S4：带 request_key（或 Idempotency-Key 头）时登记请求组 202，同键同参重放原请求组；
//                                                         mode=final 强制严格最终稿（文档引擎刷新目录、图表已渲染、产物回读），mode 缺省为预览稿；
//                                                         不带请求键的旧调用按预览稿兼容返回 201；门禁未通过 422 + 全部明细
//   GET    /api/tasks/{id}/exports                        导出任务列表（新到旧）
//   GET    /api/tasks/{id}/exports/{jobID}
//   GET    /api/tasks/{id}/exports/{jobID}/download       下载产物（校验任务归属、状态与文件大小；X-Export-Business-State 标明最终稿 / 仅预览）
//   POST   /api/tasks/{id}/exports/{jobID}/cancel         取消排队 / 执行中的作业（已终态原样返回）
//   GET    /api/tasks/{id}/export-requests[/{requestID}]  请求组列表 / 详情（含组内作业与汇总状态；刷新与重连以此恢复）
//   POST   /api/tasks/{id}/export-requests/{requestID}/cancel  逐项取消组内未终态作业
//   GET    /api/export/templates | POST（multipart file + name）| DELETE /api/export/templates/{templateID}
//   GET    /api/export/templates/default/file             下载内置默认模板（在 Word 中修改样式 / 封面后再上传）
// 状态码：参数错误 400；不存在 404；模板被引用 / 产物未就绪 / 同键异参 / 大纲版本已变化 409；门禁未通过或任务已暂停 422；
// 活跃作业过多 429；最终稿所需的文档引擎不可用 503。

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/toolhealth"
)

func (a *API) registerExportRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/exports/preflight", a.withTask(a.exportPreflight))
	mux.HandleFunc("POST /api/tasks/{id}/exports", a.withTask(a.createExport))
	mux.HandleFunc("GET /api/tasks/{id}/exports", a.withTask(a.listExports))
	mux.HandleFunc("GET /api/tasks/{id}/exports/{jobID}", a.withTask(a.getExport))
	mux.HandleFunc("GET /api/tasks/{id}/exports/{jobID}/download", a.withTask(a.downloadExport))
	mux.HandleFunc("POST /api/tasks/{id}/exports/{jobID}/cancel", a.withTask(a.cancelExport))
	mux.HandleFunc("GET /api/tasks/{id}/export-requests", a.withTask(a.listExportRequests))
	mux.HandleFunc("GET /api/tasks/{id}/export-requests/{requestID}", a.withTask(a.getExportRequest))
	mux.HandleFunc("POST /api/tasks/{id}/export-requests/{requestID}/cancel", a.withTask(a.cancelExportRequest))
	mux.HandleFunc("GET /api/export/templates", a.listTemplates)
	mux.HandleFunc("GET /api/export/templates/default/file", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
		w.Header().Set("Content-Disposition", `attachment; filename="autobid-default-template.docx"`)
		_, _ = w.Write(export.DefaultTemplateBytes())
	})
	mux.HandleFunc("POST /api/export/templates", a.uploadTemplate)
	mux.HandleFunc("DELETE /api/export/templates/{templateID}", a.deleteTemplate)
}

func exportFail(w http.ResponseWriter, err error) {
	var blocked *db.ExportBlockedError
	codes := []struct {
		err    error
		status int
		code   string
	}{
		{db.ErrIdempotencyKeyConflict, http.StatusConflict, "idempotency_conflict"},
		{db.ErrRevisionConflict, http.StatusConflict, "stale_revision"},
		{db.ErrJobCapacity, http.StatusTooManyRequests, "capacity_busy"},
		{db.ErrJobPrecondition, http.StatusUnprocessableEntity, "precondition_failed"},
		{export.ErrOfficeUnavailable, http.StatusServiceUnavailable, "runtime_unavailable"},
	}
	switch {
	case errors.As(err, &blocked):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "导出门禁未通过：" + fmt.Sprintf("%d 项阻断", len(blocked.Preflight.Readiness.Blockers)),
			"code": "export_blocked", "preflight": blocked.Preflight})
		return
	case errors.Is(err, db.ErrInvalidExport), errors.Is(err, export.ErrTemplate):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_export"})
		return
	case errors.Is(err, db.ErrTemplateInUse):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "template_in_use"})
		return
	case errors.Is(err, db.ErrExportNotReady):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "not_ready"})
		return
	}
	for _, c := range codes {
		if errors.Is(err, c.err) {
			writeJSON(w, c.status, map[string]string{"error": toolhealth.Sanitize(err.Error()), "code": c.code})
			return
		}
	}
	chartFail(w, err)
}

// ---------------------------------------------------------------------------
// 导出
// ---------------------------------------------------------------------------

// dependencyWarnings 把外部依赖不可用转为门禁提示（预览稿不阻断：DOCX 仍可导出，PDF 任务会明确失败；最终稿见 exportPreflight）。
func (a *API) dependencyWarnings() []db.ExportWarning {
	st := a.exportSvc().Status()
	var out []db.ExportWarning
	if !st.Office {
		out = append(out, db.ExportWarning{Kind: "office_unavailable",
			Message: "文档引擎（WPS / Word）不可用：最终稿与 PDF 导出将被拒绝或失败；预览稿 DOCX 可以导出，但目录页码未刷新（" + st.OfficeReason + "）"})
	}
	if !st.Renderer {
		out = append(out, db.ExportWarning{Kind: "renderer_unavailable", Message: "mermaid-cli（mmdc）不可用：Mermaid 图表无法渲染，导出时插入源码占位框"})
	}
	return out
}

// exportMode 解析 ?mode=final|preview（缺省 preview）。
func exportMode(r *http.Request) (bool, error) {
	switch m := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode"))); m {
	case "", export.ExportModePreview:
		return false, nil
	case export.ExportModeFinal:
		return true, nil
	default:
		return false, badRequest("mode must be final or preview")
	}
}

func (a *API) exportPreflight(w http.ResponseWriter, r *http.Request, t *Task) {
	strict, err := exportMode(r)
	if err != nil {
		exportFail(w, err)
		return
	}
	pre, err := a.pg.ExportPreflightPolicy(r.Context(), db.DefaultTenant, t.ID, strict)
	if err != nil {
		exportFail(w, err)
		return
	}
	pre.Warnings = append(pre.Warnings, a.dependencyWarnings()...)
	if st := a.exportSvc().Status(); strict && !st.Office {
		// 最终稿要求文档引擎真实刷新目录：引擎不可用时提交会被拒绝（503），预检同样显示为阻断。
		pre.Readiness.Blockers = append(pre.Readiness.Blockers, db.ExportBlocker{Kind: "office_unavailable",
			Message: "文档引擎（WPS / Word）不可用，不能生成最终稿：" + toolhealth.Sanitize(st.OfficeReason)})
		pre.Readiness.Ready = false
	}
	tpls, err := a.pg.ListExportTemplates(r.Context(), db.DefaultTenant)
	if err != nil {
		exportFail(w, err)
		return
	}
	policy := db.ExportPolicyPreviewV1
	if strict {
		policy = db.ExportPolicyFinal
	}
	writeJSON(w, http.StatusOK, map[string]any{"readiness": pre.Readiness, "warnings": pre.Warnings, "charts": pre.Charts,
		"strict_runtime": strict, "policy_version": policy, "status": a.exportSvc().Status(), "templates": tpls,
		"default_template_hash": export.DefaultTemplateHash()})
}

// createExport：带 request_key（请求体或 Idempotency-Key 头）的请求走 S4 请求组（202，同键同参重放原请求组）；不带请求键的旧调用
// 保持 Phase 6B 兼容语义（201，预览稿）。最终稿策略由服务器按 mode 决定并强制，客户端不能关闭其中任何一项检查。
func (a *API) createExport(w http.ResponseWriter, r *http.Request, t *Task) {
	var req export.ExportRequest
	if err := decodeBody(r, &req); err != nil {
		exportFail(w, err)
		return
	}
	if req.RequestKey == "" {
		req.RequestKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if req.TemplateID < 0 || len([]rune(req.Bidder)) > 200 {
		exportFail(w, badRequest("invalid template_id or bidder"))
		return
	}
	if strings.EqualFold(strings.TrimSpace(req.Mode), export.ExportModeFinal) && !requireProjectRole(w, r, db.RoleBidManager) {
		return // 最终稿只能由投标经理提交；预览稿技术撰写人也可以生成
	}
	if req.RequestKey == "" {
		if strings.EqualFold(strings.TrimSpace(req.Mode), export.ExportModeFinal) {
			exportFail(w, badRequest("a final draft export needs request_key (or an Idempotency-Key header) so that a resend does not create a second export"))
			return
		}
		a.createLegacyExport(w, r, t, req)
		return
	}
	sub, err := a.exportSvc().SubmitExport(r.Context(), db.DefaultTenant, t.ID, operator(r), req)
	if err != nil {
		a.exportSubmitFail(w, err)
		return
	}
	if !sub.Replayed {
		a.wakeExports()
		a.publishExportCreated(t.ID, sub.Request)
	}
	var warns []db.ExportWarning
	if sub.Preflight != nil {
		warns = sub.Preflight.Warnings
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "replayed": sub.Replayed, "request": sub.Request, "jobs": sub.Jobs,
		"warnings": append(warns, a.dependencyWarnings()...)})
}

func (a *API) createLegacyExport(w http.ResponseWriter, r *http.Request, t *Task, req export.ExportRequest) {
	jobs, pre, err := a.pg.CreateExportJobs(r.Context(), db.DefaultTenant, t.ID, operator(r), db.CreateExportInput{Formats: req.Formats,
		TemplateID: req.TemplateID, DefaultTemplateHash: export.DefaultTemplateHash(), Bidder: strings.TrimSpace(req.Bidder)})
	if err != nil {
		a.exportSubmitFail(w, err)
		return
	}
	a.wakeExports()
	formats := make([]string, 0, len(jobs))
	for _, j := range jobs {
		formats = append(formats, fmt.Sprintf("#%d %s", j.ID, j.Format))
	}
	a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "export", Kind: "state", At: time.Now(),
		Summary: fmt.Sprintf("创建导出任务 %s（预览稿，大纲 v%d，锁定 %d 个章节修订）", strings.Join(formats, "、"), jobs[0].OutlineVersion, len(jobs[0].RevSnapshot))})
	writeJSON(w, http.StatusCreated, map[string]any{"jobs": jobs, "warnings": append(pre.Warnings, a.dependencyWarnings()...)})
}

func (a *API) exportSubmitFail(w http.ResponseWriter, err error) {
	var blocked *db.ExportBlockedError
	if errors.As(err, &blocked) {
		blocked.Preflight.Warnings = append(blocked.Preflight.Warnings, a.dependencyWarnings()...)
	}
	exportFail(w, err)
}

func (a *API) wakeExports() {
	if a.cfg.ExportRunner != nil {
		a.cfg.ExportRunner.Wake()
	}
}

func (a *API) publishExportCreated(taskID int64, q *db.ExportRequest) {
	parts := make([]string, 0, len(q.Jobs))
	for _, j := range q.Jobs {
		parts = append(parts, fmt.Sprintf("#%d %s", j.ID, j.Format))
	}
	kind := "预览稿"
	if q.StrictRuntime {
		kind = "最终稿（" + q.Executor + "）"
	}
	a.hub.Publish(db.Activity{TaskID: taskID, Worker: "export", Kind: "state", At: time.Now(),
		Summary: fmt.Sprintf("导出请求组 #%d 创建作业 %s，%s，大纲 v%d", q.ID, strings.Join(parts, "、"), kind, q.OutlineVersion)})
}

func (a *API) listExports(w http.ResponseWriter, r *http.Request, t *Task) {
	jobs, err := a.pg.ListExportJobs(r.Context(), db.DefaultTenant, t.ID, 50)
	if err != nil {
		exportFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (a *API) jobFromPath(w http.ResponseWriter, r *http.Request, t *Task) *db.ExportJob {
	id, err := pathID(r, "jobID")
	if err != nil {
		exportFail(w, err)
		return nil
	}
	j, err := a.pg.GetExportJob(r.Context(), db.DefaultTenant, t.ID, id)
	if err != nil {
		exportFail(w, err)
		return nil
	}
	return j
}

func (a *API) getExport(w http.ResponseWriter, r *http.Request, t *Task) {
	if j := a.jobFromPath(w, r, t); j != nil {
		writeJSON(w, http.StatusOK, j)
	}
}

func (a *API) cancelExport(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "jobID")
	if err != nil {
		exportFail(w, err)
		return
	}
	j, cancelled, err := a.pg.CancelExportJob(r.Context(), db.DefaultTenant, t.ID, id, operator(r))
	if err != nil {
		exportFail(w, err)
		return
	}
	if cancelled {
		a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "export", Kind: "state", At: time.Now(),
			Summary: fmt.Sprintf("导出任务 #%d（%s）已取消", j.ID, j.Format)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j, "cancelled": cancelled})
}

func (a *API) listExportRequests(w http.ResponseWriter, r *http.Request, t *Task) {
	before, err := queryID(r, "before")
	if err != nil {
		exportFail(w, err)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 0 {
			exportFail(w, badRequest("limit must be a non-negative integer"))
			return
		}
	}
	reqs, err := a.pg.ListExportRequests(r.Context(), db.DefaultTenant, t.ID, before, limit)
	if err != nil {
		exportFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": reqs, "status": a.exportSvc().Status()})
}

func (a *API) getExportRequest(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "requestID")
	if err != nil {
		exportFail(w, err)
		return
	}
	q, err := a.pg.GetExportRequest(r.Context(), db.DefaultTenant, t.ID, id)
	if err != nil {
		exportFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (a *API) cancelExportRequest(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "requestID")
	if err != nil {
		exportFail(w, err)
		return
	}
	q, res, err := a.pg.CancelExportRequest(r.Context(), db.DefaultTenant, t.ID, id, operator(r))
	if err != nil {
		exportFail(w, err)
		return
	}
	var cancelled []string
	for _, x := range res {
		if x.Cancelled {
			cancelled = append(cancelled, fmt.Sprintf("#%d %s", x.JobID, x.Format))
		}
	}
	if len(cancelled) > 0 {
		a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "export", Kind: "state", At: time.Now(),
			Summary: fmt.Sprintf("导出请求组 #%d 已取消 %s", q.ID, strings.Join(cancelled, "、"))})
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": q, "results": res})
}

func (a *API) downloadExport(w http.ResponseWriter, r *http.Request, t *Task) {
	j := a.jobFromPath(w, r, t)
	if j == nil {
		return
	}
	p, err := a.exportSvc().ArtifactPath(j)
	if err != nil {
		exportFail(w, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeJSON(w, http.StatusGone, map[string]string{"error": "export artifact is missing on disk", "code": "artifact_missing"})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() != j.ArtifactSize {
		writeJSON(w, http.StatusGone, map[string]string{"error": "export artifact on disk does not match the job record", "code": "artifact_mismatch"})
		return
	}
	ct := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	if j.Format == db.ExportPDF {
		ct = "application/pdf"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": j.ArtifactName}))
	if w.Header().Get("Content-Disposition") == "" { // 非 ASCII 文件名回退为 RFC 5987
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="export.%s"; filename*=UTF-8''%s`, j.Format, url.PathEscape(j.ArtifactName)))
	}
	w.Header().Set("X-Artifact-SHA256", j.ArtifactHash)
	w.Header().Set("X-Export-Business-State", j.BusinessState)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// ---------------------------------------------------------------------------
// 模板
// ---------------------------------------------------------------------------

func (a *API) listTemplates(w http.ResponseWriter, r *http.Request) {
	tpls, err := a.pg.ListExportTemplates(r.Context(), db.DefaultTenant)
	if err != nil {
		exportFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": tpls, "default_template_hash": export.DefaultTemplateHash()})
}

func (a *API) uploadTemplate(w http.ResponseWriter, r *http.Request) {
	if !isMultipart(r) {
		exportFail(w, badRequest("upload the .docx template as multipart/form-data (field file)"))
		return
	}
	f, err := readMultipartFile(w, r)
	if err != nil {
		exportFail(w, err)
		return
	}
	if f == nil {
		exportFail(w, badRequest("multipart field file is required"))
		return
	}
	if !strings.HasSuffix(strings.ToLower(f.Name), ".docx") {
		exportFail(w, badRequest("template must be a .docx file"))
		return
	}
	tpl, err := a.exportSvc().SaveTemplate(r.Context(), db.DefaultTenant, operator(r), strings.TrimSpace(r.FormValue("name")), f.Name, f.Data)
	if err != nil {
		exportFail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tpl)
}

func (a *API) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "templateID")
	if err != nil {
		exportFail(w, err)
		return
	}
	if _, err := a.pg.DeleteExportTemplate(r.Context(), db.DefaultTenant, id); err != nil {
		exportFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
