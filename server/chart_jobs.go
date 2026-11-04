package server

// S3：图表渲染作业 API（AGENT_TOOL_SKILLS_PLAN 8，域 C）。
//   POST /api/tasks/{id}/chart-jobs                 {request_key, section_id, expected_rev_no, chart_type, title, spec} 新图表；
//                                                   {request_key, chart_id[, expected_rev_no]} 重试已有图表；202
//   GET  /api/tasks/{id}/chart-jobs                 ?chart_id=&section_id=&before=&limit= 作业列表（新者在前）
//   GET  /api/tasks/{id}/chart-jobs/{jobID}         作业及其图表
//   POST /api/tasks/{id}/chart-jobs/{jobID}/cancel  取消排队 / 执行中的作业（已终态原样返回）
// 状态码：参数 / 规格错误 400；不存在 404；同键异参、修订已变化、图表已有活跃作业 409；前置条件未满足 422；
// 活跃作业过多 429；渲染器不可用 503。提交只登记作业，渲染由 export.ChartRunner 在事务外完成；SSE 只提示重读，
// 刷新与重连以本组列表 / 详情接口恢复。

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/toolhealth"
)

func (a *API) registerChartJobRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/tasks/{id}/chart-jobs", a.withTask(a.submitChartJob))
	mux.HandleFunc("GET /api/tasks/{id}/chart-jobs", a.withTask(a.listChartJobs))
	mux.HandleFunc("GET /api/tasks/{id}/chart-jobs/{jobID}", a.withTask(a.getChartJob))
	mux.HandleFunc("POST /api/tasks/{id}/chart-jobs/{jobID}/cancel", a.withTask(a.cancelChartJob))
}

// chartJobFail 在图表错误映射之外补充作业错误；错误原因脱敏后返回。
func chartJobFail(w http.ResponseWriter, err error) {
	codes := []struct {
		err    error
		status int
		code   string
	}{
		{db.ErrIdempotencyKeyConflict, http.StatusConflict, "idempotency_conflict"},
		{db.ErrChartJobActive, http.StatusConflict, "chart_job_active"},
		{db.ErrRevisionConflict, http.StatusConflict, "stale_revision"},
		{db.ErrJobCapacity, http.StatusTooManyRequests, "capacity_busy"},
		{db.ErrJobPrecondition, http.StatusUnprocessableEntity, "precondition_failed"},
		{export.ErrRendererMissing, http.StatusServiceUnavailable, "runtime_unavailable"},
	}
	for _, c := range codes {
		if errors.Is(err, c.err) {
			writeJSON(w, c.status, map[string]string{"error": toolhealth.Sanitize(err.Error()), "code": c.code})
			return
		}
	}
	chartFail(w, err)
}

func (a *API) submitChartJob(w http.ResponseWriter, r *http.Request, t *Task) {
	var req export.ChartRenderRequest
	if err := decodeBody(r, &req); err != nil {
		chartJobFail(w, err)
		return
	}
	if req.SectionID <= 0 && req.ChartID <= 0 {
		chartJobFail(w, badRequest("section_id or chart_id is required"))
		return
	}
	sub, err := a.exportSvc().SubmitChartRender(r.Context(), db.DefaultTenant, t.ID, operator(r), req)
	if err != nil {
		chartJobFail(w, err)
		return
	}
	if !sub.Replayed {
		a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "chart-render", Kind: "state", At: time.Now(),
			Summary: fmt.Sprintf("章节 %s 提交图表渲染作业 #%d《%s》（rev %d）", sub.Chart.SectionCode, sub.Job.ID, sub.Chart.Title, sub.Chart.RevNo)})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "replayed": sub.Replayed, "job": sanitizedChartJob(sub.Job), "chart": sub.Chart})
}

func sanitizedChartJob(j *db.ChartRenderJob) *db.ChartRenderJob {
	c := *j
	c.Error = toolhealth.Sanitize(c.Error)
	return &c
}

func (a *API) listChartJobs(w http.ResponseWriter, r *http.Request, t *Task) {
	var f db.ChartJobFilter
	var err error
	for name, dst := range map[string]*int64{"chart_id": &f.ChartID, "section_id": &f.SectionID, "before": &f.Before} {
		if *dst, err = queryID(r, name); err != nil {
			chartJobFail(w, err)
			return
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if f.Limit, err = strconv.Atoi(v); err != nil || f.Limit < 0 {
			chartJobFail(w, badRequest("limit must be a non-negative integer"))
			return
		}
	}
	jobs, err := a.pg.ListChartRenderJobs(r.Context(), db.DefaultTenant, t.ID, f)
	if err != nil {
		chartJobFail(w, err)
		return
	}
	for i, j := range jobs {
		jobs[i] = sanitizedChartJob(j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "renderer": a.exportSvc().Status()})
}

func (a *API) getChartJob(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "jobID")
	if err != nil {
		chartJobFail(w, err)
		return
	}
	j, c, err := a.pg.GetChartRenderJob(r.Context(), db.DefaultTenant, t.ID, id)
	if err != nil {
		chartJobFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": sanitizedChartJob(j), "chart": c})
}

func (a *API) cancelChartJob(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "jobID")
	if err != nil {
		chartJobFail(w, err)
		return
	}
	j, err := a.pg.CancelChartRenderJob(r.Context(), db.DefaultTenant, t.ID, id, operator(r))
	if err != nil {
		chartJobFail(w, err)
		return
	}
	c, err := a.pg.GetChart(r.Context(), db.DefaultTenant, t.ID, j.ChartID)
	if err != nil {
		chartJobFail(w, err)
		return
	}
	if j.Status == db.ChartJobCancelled {
		a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "chart-render", Kind: "state", At: time.Now(),
			Summary: fmt.Sprintf("图表渲染作业 #%d（图表 #%d）已取消", j.ID, j.ChartID)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": sanitizedChartJob(j), "chart": c})
}
