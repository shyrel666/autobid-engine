package server

// Phase 6B：锁定版本导出 API（导出附件域；章节图表路由见 server/charts.go）。
//   GET    /api/tasks/{id}/exports/preflight              导出门禁明细：6A 阻断项 + 图表 / 外部依赖提示 + 可用模板
//   POST   /api/tasks/{id}/exports                        {formats: ["docx","pdf"], template_id?, bidder?}：门禁通过 201，否则 422 + 全部明细
//   GET    /api/tasks/{id}/exports                        导出任务列表（新到旧）
//   GET    /api/tasks/{id}/exports/{jobID}
//   GET    /api/tasks/{id}/exports/{jobID}/download       下载产物（校验任务归属、状态与文件大小）
//   GET    /api/export/templates | POST（multipart file + name）| DELETE /api/export/templates/{templateID}
//   GET    /api/export/templates/default/file             下载内置默认模板（在 Word 中修改样式 / 封面后再上传）
// 状态码：参数错误 400；不存在 404；模板被引用 / 产物未就绪 409；门禁未通过 422。

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
)

func (a *API) registerExportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/exports/preflight", a.withTask(a.exportPreflight))
	mux.HandleFunc("POST /api/tasks/{id}/exports", a.withTask(a.createExport))
	mux.HandleFunc("GET /api/tasks/{id}/exports", a.withTask(a.listExports))
	mux.HandleFunc("GET /api/tasks/{id}/exports/{jobID}", a.withTask(a.getExport))
	mux.HandleFunc("GET /api/tasks/{id}/exports/{jobID}/download", a.withTask(a.downloadExport))
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
	switch {
	case errors.As(err, &blocked):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "导出门禁未通过：" + fmt.Sprintf("%d 项阻断", len(blocked.Preflight.Readiness.Blockers)),
			"code": "export_blocked", "preflight": blocked.Preflight})
	case errors.Is(err, db.ErrInvalidExport), errors.Is(err, export.ErrTemplate):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_export"})
	case errors.Is(err, db.ErrTemplateInUse):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "template_in_use"})
	case errors.Is(err, db.ErrExportNotReady):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "not_ready"})
	default:
		chartFail(w, err)
	}
}

// ---------------------------------------------------------------------------
// 导出
// ---------------------------------------------------------------------------

// dependencyWarnings 把外部依赖不可用转为门禁提示（不阻断：DOCX 仍可导出，PDF 任务会明确失败）。
func (a *API) dependencyWarnings() []db.ExportWarning {
	st := a.exportSvc().Status()
	var out []db.ExportWarning
	if !st.Office {
		out = append(out, db.ExportWarning{Kind: "office_unavailable",
			Message: "LibreOffice 不可用：PDF 导出任务将失败；DOCX 可以导出，但目录页码未刷新（" + st.OfficeReason + "）"})
	}
	if !st.Renderer {
		out = append(out, db.ExportWarning{Kind: "renderer_unavailable", Message: "mermaid-cli（mmdc）不可用：Mermaid 图表无法渲染，导出时插入源码占位框"})
	}
	return out
}

func (a *API) exportPreflight(w http.ResponseWriter, r *http.Request, t *Task) {
	pre, err := a.pg.ExportPreflight(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		exportFail(w, err)
		return
	}
	pre.Warnings = append(pre.Warnings, a.dependencyWarnings()...)
	tpls, err := a.pg.ListExportTemplates(r.Context(), db.DefaultTenant)
	if err != nil {
		exportFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"readiness": pre.Readiness, "warnings": pre.Warnings, "charts": pre.Charts,
		"status": a.exportSvc().Status(), "templates": tpls, "default_template_hash": export.DefaultTemplateHash()})
}

func (a *API) createExport(w http.ResponseWriter, r *http.Request, t *Task) {
	var req struct {
		Formats    []string `json:"formats"`
		TemplateID int64    `json:"template_id"`
		Bidder     string   `json:"bidder"`
	}
	if err := decodeBody(r, &req); err != nil {
		exportFail(w, err)
		return
	}
	if req.TemplateID < 0 || len([]rune(req.Bidder)) > 200 {
		exportFail(w, badRequest("invalid template_id or bidder"))
		return
	}
	jobs, pre, err := a.pg.CreateExportJobs(r.Context(), db.DefaultTenant, t.ID, operator(r), db.CreateExportInput{Formats: req.Formats,
		TemplateID: req.TemplateID, DefaultTemplateHash: export.DefaultTemplateHash(), Bidder: strings.TrimSpace(req.Bidder)})
	if err != nil {
		var blocked *db.ExportBlockedError
		if errors.As(err, &blocked) {
			blocked.Preflight.Warnings = append(blocked.Preflight.Warnings, a.dependencyWarnings()...)
		}
		exportFail(w, err)
		return
	}
	if a.cfg.ExportRunner != nil {
		a.cfg.ExportRunner.Wake()
	}
	formats := make([]string, 0, len(jobs))
	for _, j := range jobs {
		formats = append(formats, fmt.Sprintf("#%d %s", j.ID, j.Format))
	}
	a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "export", Kind: "state", At: time.Now(),
		Summary: fmt.Sprintf("创建导出任务 %s（大纲 v%d，锁定 %d 个章节修订）", strings.Join(formats, "、"), jobs[0].OutlineVersion, len(jobs[0].RevSnapshot))})
	writeJSON(w, http.StatusCreated, map[string]any{"jobs": jobs, "warnings": append(pre.Warnings, a.dependencyWarnings()...)})
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
