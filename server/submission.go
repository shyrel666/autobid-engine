package server

// D4：提交检查清单（白手套清单）API，按导出请求组。
//   GET    /api/tasks/{id}/export-requests/{requestID}/submission            清单（系统可查项每次重算 + 人工确认记录）与确认审计
//   PUT    /api/tasks/{id}/export-requests/{requestID}/submission/{key}      {status: confirmed|failed|not_applicable, note?, reference?, expected_version?}
//   DELETE /api/tasks/{id}/export-requests/{requestID}/submission/{key}      撤回一项确认（?expected_version=N 可选）
// 人工确认只接受当前最终稿：预览稿、未全部成功或导出后正文又变了的请求组返回 409 not_final。系统判定为通过 / 不通过的条目不能
// 手工改写（400）。确认人是当前登录账号（R2，服务端会话核验）；只有投标经理能确认（见 permissions.go）。
// 清单只判定与留痕：ready 表示该成稿的系统项与人工项都已处理，不代表已在交易平台提交。

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
)

func (a *API) registerSubmissionRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/export-requests/{requestID}/submission", a.withTask(a.getSubmission))
	mux.HandleFunc("PUT /api/tasks/{id}/export-requests/{requestID}/submission/{key}", a.withTask(a.setSubmissionCheck))
	mux.HandleFunc("DELETE /api/tasks/{id}/export-requests/{requestID}/submission/{key}", a.withTask(a.withdrawSubmissionCheck))
}

func submissionFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrSubmissionNotFinal):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "not_final"})
	case errors.Is(err, db.ErrSubmissionCheckInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_check"})
	case errors.Is(err, db.ErrRevisionConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "stale_revision"})
	default:
		exportFail(w, err)
	}
}

func (a *API) getSubmission(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "requestID")
	if err != nil {
		submissionFail(w, err)
		return
	}
	cl, err := a.pg.SubmissionChecklist(r.Context(), db.DefaultTenant, t.ID, id)
	if err != nil {
		submissionFail(w, err)
		return
	}
	events, err := a.pg.ListSubmissionCheckEvents(r.Context(), db.DefaultTenant, t.ID, id, 100)
	if err != nil {
		submissionFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checklist": cl, "events": events})
}

func (a *API) setSubmissionCheck(w http.ResponseWriter, r *http.Request, t *Task) {
	var in db.SubmissionCheckInput
	if err := decodeBody(r, &in); err != nil {
		submissionFail(w, err)
		return
	}
	if strings.TrimSpace(in.Status) == "" {
		submissionFail(w, badRequest("status is required (confirmed, failed or not_applicable); use DELETE to withdraw a confirmation"))
		return
	}
	a.writeSubmissionCheck(w, r, t, in)
}

func (a *API) withdrawSubmissionCheck(w http.ResponseWriter, r *http.Request, t *Task) {
	var in db.SubmissionCheckInput
	if v := r.URL.Query().Get("expected_version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			submissionFail(w, badRequest("expected_version must be a positive integer"))
			return
		}
		in.ExpectedVersion = n
	}
	a.writeSubmissionCheck(w, r, t, in)
}

func (a *API) writeSubmissionCheck(w http.ResponseWriter, r *http.Request, t *Task, in db.SubmissionCheckInput) {
	id, err := pathID(r, "requestID")
	if err != nil {
		submissionFail(w, err)
		return
	}
	key := r.PathValue("key")
	cl, err := a.pg.SetSubmissionCheck(r.Context(), db.DefaultTenant, t.ID, id, key, in, operator(r))
	if err != nil {
		submissionFail(w, err)
		return
	}
	label, action := key, "撤回确认"
	for _, it := range cl.Items {
		if it.Key == key {
			label = it.Label
		}
	}
	switch in.Status {
	case db.CheckConfirmed:
		action = "已确认"
	case db.CheckFailed:
		action = "检查未通过"
	case db.CheckNotApplicable:
		action = "标为不适用"
	}
	state := fmt.Sprintf("还有 %d 项待处理", cl.Open)
	if cl.Ready {
		state = "清单已全部处理"
	}
	a.hub.Publish(db.Activity{TaskID: t.ID, Worker: "export", Kind: "state", At: time.Now(),
		Summary: fmt.Sprintf("提交检查清单（导出请求组 #%d）：%s %s，%s", id, label, action, state)})
	events, err := a.pg.ListSubmissionCheckEvents(r.Context(), db.DefaultTenant, t.ID, id, 100)
	if err != nil {
		submissionFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checklist": cl, "events": events})
}
