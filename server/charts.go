package server

// Phase 6B：章节图表 API（标书正文域）。
//   GET    /api/tasks/{id}/charts                         ?section_id= 图表列表（stale = 绑定修订已不是章节当前主版本）
//   POST   /api/tasks/{id}/charts                         {section_id, chart_type, title, spec} 生成并绑定章节当前修订
//   GET    /api/tasks/{id}/charts/{chartID}
//   GET    /api/tasks/{id}/charts/{chartID}/image         已渲染图表的 PNG
//   POST   /api/tasks/{id}/charts/{chartID}/render        重新渲染（mmdc 可用后补渲染）
//   POST   /api/tasks/{id}/charts/{chartID}/rebind        重新绑定到章节当前修订（人工确认图表仍适用）
//   DELETE /api/tasks/{id}/charts/{chartID}
//   GET    /api/export/status                             LibreOffice / mermaid-cli 可用性
// 状态码：规格错误 400；不存在 404；渲染期间源码被改 409；章节尚无修订 422。

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
)

func (a *API) registerChartRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/charts", a.withTask(a.listCharts))
	mux.HandleFunc("POST /api/tasks/{id}/charts", a.withTask(a.generateChart))
	mux.HandleFunc("GET /api/tasks/{id}/charts/{chartID}", a.withTask(a.getChart))
	mux.HandleFunc("GET /api/tasks/{id}/charts/{chartID}/image", a.withTask(a.chartImage))
	mux.HandleFunc("POST /api/tasks/{id}/charts/{chartID}/render", a.withTask(a.renderChart))
	mux.HandleFunc("POST /api/tasks/{id}/charts/{chartID}/rebind", a.withTask(a.rebindChart))
	mux.HandleFunc("DELETE /api/tasks/{id}/charts/{chartID}", a.withTask(a.deleteChart))
	mux.HandleFunc("GET /api/export/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, a.exportSvc().Status()) })
}

func (a *API) exportSvc() *export.Service { return a.cfg.Export }

// chartFail 在域 C 错误映射之外补充图表错误。
func chartFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, export.ErrChartSpec), errors.Is(err, db.ErrInvalidChart):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_chart"})
	case errors.Is(err, db.ErrChartNoRevision):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "code": "no_revision"})
	case errors.Is(err, db.ErrChartSourceMoved):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "chart_changed"})
	default:
		proposalFail(w, err)
	}
}

// ---------------------------------------------------------------------------
// 图表
// ---------------------------------------------------------------------------

func (a *API) listCharts(w http.ResponseWriter, r *http.Request, t *Task) {
	sid, err := queryID(r, "section_id")
	if err != nil {
		chartFail(w, err)
		return
	}
	cs, err := a.pg.ListCharts(r.Context(), db.DefaultTenant, t.ID, sid)
	if err != nil {
		chartFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"charts": cs, "renderer": a.exportSvc().Status()})
}

func (a *API) generateChart(w http.ResponseWriter, r *http.Request, t *Task) {
	var req export.ChartRequest
	if err := decodeBody(r, &req); err != nil {
		chartFail(w, err)
		return
	}
	if req.SectionID <= 0 {
		chartFail(w, badRequest("section_id is required"))
		return
	}
	c, err := a.exportSvc().GenerateChart(r.Context(), db.DefaultTenant, t.ID, operator(r), req)
	if err != nil {
		chartFail(w, err)
		return
	}
	a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "proposal", Kind: "state", At: time.Now(),
		Summary: fmt.Sprintf("章节 %s 生成图表《%s》（%s，%s）", c.SectionCode, c.Title, c.ChartType, c.RenderStatus)})
	writeJSON(w, http.StatusCreated, c)
}

func (a *API) chartFromPath(w http.ResponseWriter, r *http.Request, t *Task) *db.ProposalChart {
	id, err := pathID(r, "chartID")
	if err != nil {
		chartFail(w, err)
		return nil
	}
	c, err := a.pg.GetChart(r.Context(), db.DefaultTenant, t.ID, id)
	if err != nil {
		chartFail(w, err)
		return nil
	}
	return c
}

func (a *API) getChart(w http.ResponseWriter, r *http.Request, t *Task) {
	if c := a.chartFromPath(w, r, t); c != nil {
		writeJSON(w, http.StatusOK, c)
	}
}

func (a *API) chartImage(w http.ResponseWriter, r *http.Request, t *Task) {
	c := a.chartFromPath(w, r, t)
	if c == nil {
		return
	}
	png, err := a.exportSvc().ChartImage(c)
	if err != nil {
		chartFail(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(png)
}

func (a *API) renderChart(w http.ResponseWriter, r *http.Request, t *Task) {
	c := a.chartFromPath(w, r, t)
	if c == nil {
		return
	}
	out, err := a.exportSvc().RenderChart(r.Context(), db.DefaultTenant, t.ID, c, operator(r))
	if err != nil {
		chartFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) rebindChart(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "chartID")
	if err != nil {
		chartFail(w, err)
		return
	}
	c, err := a.pg.RebindChart(r.Context(), db.DefaultTenant, t.ID, id, operator(r))
	if err != nil {
		chartFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *API) deleteChart(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "chartID")
	if err != nil {
		chartFail(w, err)
		return
	}
	if err := a.pg.DeleteChart(r.Context(), db.DefaultTenant, t.ID, id); err != nil {
		chartFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
