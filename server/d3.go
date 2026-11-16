package server

// D3 章节故事板（Pink Team 粉队评审）、受控引用渲染与句子级写作核验 API。

import (
	"errors"
	"io"
	"net/http"

	"github.com/autobid/autobid-engine/db"
)

func (a *API) registerD3Routes(mux *routeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/storyboards", a.withTask(a.listTaskStoryboards))
	mux.HandleFunc("GET /api/tasks/{id}/sections/{sectionID}/storyboard", a.withTask(a.getSectionStoryboard))
	mux.HandleFunc("PUT /api/tasks/{id}/sections/{sectionID}/storyboard", a.withTask(a.upsertSectionStoryboard))
	mux.HandleFunc("POST /api/tasks/{id}/sections/{sectionID}/storyboard/submit", a.withTask(a.submitSectionStoryboard))
	mux.HandleFunc("POST /api/tasks/{id}/sections/{sectionID}/storyboard/approve", a.withTask(a.approveSectionStoryboard))
	mux.HandleFunc("POST /api/tasks/{id}/sections/{sectionID}/storyboard/reject", a.withTask(a.rejectSectionStoryboard))
	mux.HandleFunc("POST /api/tasks/{id}/sections/{sectionID}/storyboard/render", a.withTask(a.renderSectionGroundedDraft))

	mux.HandleFunc("GET /api/tasks/{id}/writing-verifications", a.withTask(a.listTaskWritingVerifications))
	mux.HandleFunc("GET /api/tasks/{id}/sections/{sectionID}/writing-verification", a.withTask(a.getSectionWritingVerification))
	mux.HandleFunc("POST /api/tasks/{id}/sections/{sectionID}/writing-verification/evaluate", a.withTask(a.evaluateSectionWritingVerification))
}

func (a *API) listTaskStoryboards(w http.ResponseWriter, r *http.Request, t *Task) {
	out, err := a.pg.ListTaskStoryboards(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		proposalFail(w, err)
		return
	}
	if out == nil {
		out = []*db.SectionStoryboard{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getSectionStoryboard(w http.ResponseWriter, r *http.Request, t *Task) {
	secID, err := pathID(r, "sectionID")
	if err != nil {
		fail(w, err)
		return
	}
	sb, err := a.pg.GetSectionStoryboard(r.Context(), db.DefaultTenant, t.ID, secID)
	if err != nil {
		proposalFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (a *API) upsertSectionStoryboard(w http.ResponseWriter, r *http.Request, t *Task) {
	secID, err := pathID(r, "sectionID")
	if err != nil {
		fail(w, err)
		return
	}
	var in db.UpsertStoryboardInput
	if err := decodeBody(r, &in); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	in.TaskID, in.SectionID = t.ID, secID
	sb, err := a.pg.UpsertStoryboard(r.Context(), db.DefaultTenant, operator(r), in)
	if err != nil {
		proposalFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (a *API) submitSectionStoryboard(w http.ResponseWriter, r *http.Request, t *Task) {
	secID, err := pathID(r, "sectionID")
	if err != nil {
		fail(w, err)
		return
	}
	sb, err := a.pg.SubmitStoryboard(r.Context(), db.DefaultTenant, t.ID, secID, operator(r))
	if err != nil {
		proposalFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (a *API) approveSectionStoryboard(w http.ResponseWriter, r *http.Request, t *Task) {
	secID, err := pathID(r, "sectionID")
	if err != nil {
		fail(w, err)
		return
	}
	var body struct {
		ReviewNote string `json:"review_note"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeBody(r, &body); err != nil && !errors.Is(err, io.EOF) {
			fail(w, badRequest("invalid JSON body: %v", err))
			return
		}
	}
	sb, err := a.pg.ApproveStoryboard(r.Context(), db.DefaultTenant, t.ID, secID, body.ReviewNote, operator(r))
	if err != nil {
		proposalFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (a *API) rejectSectionStoryboard(w http.ResponseWriter, r *http.Request, t *Task) {
	secID, err := pathID(r, "sectionID")
	if err != nil {
		fail(w, err)
		return
	}
	var body struct {
		ReviewNote string `json:"review_note"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	sb, err := a.pg.RejectStoryboard(r.Context(), db.DefaultTenant, t.ID, secID, body.ReviewNote, operator(r))
	if err != nil {
		proposalFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (a *API) renderSectionGroundedDraft(w http.ResponseWriter, r *http.Request, t *Task) {
	secID, err := pathID(r, "sectionID")
	if err != nil {
		fail(w, err)
		return
	}
	var body struct {
		TemplateMD string `json:"template_md"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, badRequest("invalid JSON body: %v", err))
		return
	}
	res, err := a.pg.RenderSectionGroundedDraft(r.Context(), db.DefaultTenant, t.ID, secID, body.TemplateMD)
	if err != nil {
		proposalFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) listTaskWritingVerifications(w http.ResponseWriter, r *http.Request, t *Task) {
	out, err := a.pg.ListTaskWritingVerifications(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		proposalFail(w, err)
		return
	}
	if out == nil {
		out = []*db.SectionWritingVerification{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getSectionWritingVerification(w http.ResponseWriter, r *http.Request, t *Task) {
	secID, err := pathID(r, "sectionID")
	if err != nil {
		fail(w, err)
		return
	}
	wv, err := a.pg.GetSectionWritingVerification(r.Context(), db.DefaultTenant, t.ID, secID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		proposalFail(w, err)
		return
	}
	if errors.Is(err, db.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "writing verification not found", "code": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, wv)
}

func (a *API) evaluateSectionWritingVerification(w http.ResponseWriter, r *http.Request, t *Task) {
	secID, err := pathID(r, "sectionID")
	if err != nil {
		fail(w, err)
		return
	}
	wv, err := a.pg.EvaluateSectionWritingVerification(r.Context(), db.DefaultTenant, t.ID, secID, operator(r))
	if err != nil {
		proposalFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wv)
}
