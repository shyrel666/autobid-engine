package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/db"
)

// RunSectionJobs recovers durable requests on startup and dispatches bounded work.
// Hosts must run it with the service lifecycle, rather than a request context.
func (a *API) RunSectionJobs(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		ids, err := a.pg.PendingSectionWriteTasks(ctx)
		if err == nil {
			for _, id := range ids {
				a.wakeSectionJobs(id)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *API) wakeSectionJobs(taskID int64) {
	if _, loaded := a.sectionJobPumps.LoadOrStore(taskID, true); loaded {
		return
	}
	go func() {
		defer a.sectionJobPumps.Delete(taskID)
		for a.runCtx.Err() == nil {
			// Reserve process capacity before claiming a database lease. Queued
			// work must not lose its lease while waiting for another model call.
			select {
			case a.sectionJobSlots <- struct{}{}:
			default:
				return
			}
			j, err := a.pg.ClaimSectionWriteJob(a.runCtx, taskID, 2)
			if err != nil || j == nil {
				<-a.sectionJobSlots
				return
			}
			go func() { defer func() { <-a.sectionJobSlots; a.wakeSectionJobs(taskID) }(); a.executeSectionJob(j) }()
		}
	}()
}

func (a *API) executeSectionJob(j *db.SectionWriteJob) {
	ctx, cancel := context.WithTimeout(a.runCtx, sectionWriteTimeout)
	defer cancel()
	ctx, deadlineCancel := context.WithDeadline(ctx, j.DeadlineAt)
	defer deadlineCancel()
	emit := func(kind, summary, detail string) {
		a.hub.Publish(db.Activity{TaskID: j.TaskID, Worker: "section-writer", Kind: kind, Summary: summary, Detail: detail, At: time.Now()})
	}
	fail := func(err error) {
		// Service shutdown leaves the lease to expire for another executor.
		if a.runCtx.Err() != nil {
			return
		}
		c, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		if e := a.pg.FailSectionWriteJob(c, j, err.Error()); e != nil && !errors.Is(e, db.ErrLeaseLost) {
			emit("error", "无法保存写作失败状态", e.Error())
		}
		emit("error", fmt.Sprintf("章节 %s 写作已阻塞", j.SectionCode), err.Error())
	}
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := a.pg.HeartbeatSectionWriteJob(ctx, j); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	t, err := a.m.LoadTask(ctx, j.TaskID)
	if err != nil {
		fail(err)
		return
	}
	writer := a.sectionWriter()
	if writer == nil {
		fail(errors.New("no LLM profile configured"))
		return
	}
	emit("state", fmt.Sprintf("章节 %s 开始第 %d 次写作", j.SectionCode, j.Attempts), fmt.Sprintf("job_id=%d", j.ID))
	sc, err := bidtools.BuildSectionContext(ctx, a.pg, j.TenantID, j.TaskID, j.SectionID)
	if err != nil {
		fail(err)
		return
	}
	draft, err := writer.WriteSection(ctx, t, sc, j.Instruction)
	if err != nil {
		fail(err)
		return
	}
	if err = ctx.Err(); err != nil {
		fail(err)
		return
	}
	// S3：图表计划在保存修订前全部校验并生成确定性源码；修订与图表作业随后在同一事务登记。
	charts, err := a.exportSvc().BuildChartPlans(draft.Charts)
	if err != nil {
		fail(fmt.Errorf("图表计划无效，修订未保存：%w", err))
		return
	}
	source := db.RevAIInitial
	if j.BaseRevNo > 0 {
		source = db.RevAIRewrite
	}
	res, err := a.pg.FinishSectionWriteJob(ctx, j, db.SaveRevisionInput{Source: source, ContentMD: draft.ContentMD, SummaryNote: draft.SummaryNote, DiffSummary: draft.DiffSummary, EvidenceIDs: draft.EvidenceIDs, AdmissibilityItemIDs: draft.AdmissibilityItemIDs, CommitmentIDs: draft.CommitmentIDs, Gaps: draft.Gaps}, charts)
	if err != nil {
		fail(err)
		return
	}
	emit("state", fmt.Sprintf("章节 %s 写回 rev %d（%s），待人工评审", j.SectionCode, res.Revision.RevNo, res.Outcome), fmt.Sprintf("job_id=%d", j.ID))
	if charts != nil && res.Outcome == db.SaveApplied {
		// 作业已随修订提交，这里只加速执行者领取。
		a.exportSvc().WakeChartRunner()
		a.hub.Publish(db.Activity{TaskID: j.TaskID, Worker: "chart-render", Kind: "state", At: time.Now(),
			Summary: fmt.Sprintf("章节 %s rev %d 登记 %d 张图表的渲染作业", j.SectionCode, res.Revision.RevNo, len(charts.Plans))})
	}
}

func (a *API) listSectionJobs(w http.ResponseWriter, r *http.Request, t *Task) {
	jobs, err := a.pg.ListSectionWriteJobs(r.Context(), db.DefaultTenant, t.ID)
	if err != nil {
		proposalFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}
func (a *API) cancelSectionJob(w http.ResponseWriter, r *http.Request, t *Task) {
	id, err := pathID(r, "jobID")
	if err == nil {
		err = a.pg.CancelSectionWriteJob(r.Context(), db.DefaultTenant, t.ID, id, operator(r))
	}
	if err != nil {
		proposalFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
