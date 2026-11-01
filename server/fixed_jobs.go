package server

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
)

// Raw body avoids multipart buffering. The original is durable before parsing starts.
func (a *API) uploadProcessingFile(w http.ResponseWriter, r *http.Request, t *Task) {
	if a.fixedProcessor(w) == nil {
		return
	}
	name := path.Base(strings.ReplaceAll(r.URL.Query().Get("filename"), `\`, "/"))
	if name == "" || name == "." || name == "/" || len(name) > 255 {
		fail(w, badRequest("filename is required (max 255 bytes)"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	input := bufio.NewReaderSize(r.Body, 4096)
	prefix, err := input.Peek(4096)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		fail(w, err)
		return
	}
	if len(prefix) == 0 {
		fail(w, badRequest("empty file"))
		return
	}
	format := ingest.DetectFormat(name, r.Header.Get("Content-Type"), prefix)
	if format == "" {
		format = "unsupported"
	}
	obj, err := a.cfg.FileStore.Put(r.Context(), filework.PutOptions{TenantID: db.DefaultTenant, TaskID: t.ID, Kind: db.FileKindOriginal, OriginalName: name, MediaType: r.Header.Get("Content-Type"), Format: format, CreatedBy: operator(r)}, input)
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			fail(w, &httpError{status: 413, err: err})
		} else {
			fail(w, err)
		}
		return
	}
	a.e.emit(t, db.Activity{Worker: "file-processing", Kind: "state", Summary: "原件已留存，待处理"})
	writeJSON(w, http.StatusCreated, obj)
}
func (a *API) listTaskMaterials(w http.ResponseWriter, r *http.Request, t *Task) {
	rows, err := a.pg.ListTaskMaterials(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
func (a *API) saveTaskMaterial(w http.ResponseWriter, r *http.Request, t *Task) {
	var in db.TaskMaterial
	if err := decodeBody(r, &in); err != nil {
		fail(w, err)
		return
	}
	saved, err := a.pg.SaveTaskMaterial(r.Context(), db.DefaultTenant, t.ID, in, operator(r))
	if err != nil {
		fail(w, err)
		return
	}
	a.e.emit(t, db.Activity{Worker: "file-processing", Kind: "state", Summary: "必需材料清单已更新"})
	writeJSON(w, 200, saved)
}
func (a *API) enqueueFixedJob(w http.ResponseWriter, r *http.Request, t *Task) {
	p := a.fixedProcessor(w)
	if p == nil {
		return
	}
	var in filework.ProcessInput
	if err := decodeBody(r, &in); err != nil {
		fail(w, err)
		return
	}
	j, err := p.Enqueue(r.Context(), t.ID, in, operator(r))
	if err != nil {
		fail(w, err)
		return
	}
	if a.cfg.FileRunner != nil {
		a.cfg.FileRunner.Wake()
	}
	a.e.emit(t, db.Activity{Worker: "file-processing", Kind: "state", Summary: "材料处理作业已提交"})
	writeJSON(w, 202, j)
}
func (a *API) listFixedJobs(w http.ResponseWriter, r *http.Request, t *Task) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	rows, err := a.pg.ListFixedFileJobs(r.Context(), db.DefaultTenant, t.ID, max(0, before))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
func (a *API) getFixedJob(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "jobID")
	if err != nil {
		fail(w, err)
		return
	}
	j, err := a.pg.GetFixedFileJob(r.Context(), db.DefaultTenant, t.ID, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, j)
}
func (a *API) cancelFixedJob(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "jobID")
	if err != nil {
		fail(w, err)
		return
	}
	if err = a.pg.CancelFixedFileJob(r.Context(), db.DefaultTenant, t.ID, id, operator(r)); err != nil {
		fail(w, err)
		return
	}
	if a.cfg.FileRunner != nil {
		a.cfg.FileRunner.Cancel(id)
	}
	a.e.emit(t, db.Activity{Worker: "file-processing", Kind: "state", Summary: "材料处理作业已取消"})
	a.getFixedJob(w, r, t)
}
