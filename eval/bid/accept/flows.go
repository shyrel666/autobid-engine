package main

// 主链路与门禁场景。执行器直接调用 db / ingest / export / bidtools，不经过 HTTP 路由或浏览器。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/bidtools"
	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/ingest"
)

var cst = time.FixedZone("CST", 8*3600)

const (
	tenderPublished     = "2026-10-01T09:00:00+08:00"
	correctionPublished = "2026-10-10T09:00:00+08:00"
)

type appConfig struct {
	ArtifactsDir string
	GitCommit    string
	GoVersion    string
	Keep         bool
}

type app struct {
	cfg       appConfig
	d         *db.DB
	golden    *goldenSpec
	fixtures  *fixtureSet
	exportSvc *export.Service
	baseRunID string
	runID     string
}

// flow 是一轮执行的上下文。
type flow struct {
	app   *app
	r     *round
	runID string
}

func parseCST(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.In(cst), nil
}

func (f *flow) newTask(ctx context.Context, scenario, goal string) (*db.Task, error) {
	name := fmt.Sprintf("E2E-%s-%s", f.runID, scenario)
	// M1 基线用确定性替身正文、不建故事板，显式沿用旧写作门禁；D3 强制门禁由 eval/bid/d3 验收。
	t, err := f.app.d.CreateTask(ctx, db.CreateTaskInput{Name: name, Description: "E 级端到端验收（虚构演示数据，默认自动清理）",
		Goal: goal, LegacyWriting: true})
	if err == nil {
		f.r.addTask(t.ID)
	}
	return t, err
}

func (f *flow) ingestDoc(ctx context.Context, taskID int64, data []byte, name, title, role, published string) (*ingest.Result, error) {
	at, err := parseCST(published)
	if err != nil {
		return nil, err
	}
	as := db.NewAssetStore(f.app.d, taskID)
	return ingest.Ingest(ctx, f.app.d, as, ingest.Input{TenderNo: f.app.golden.TenderNo, Title: title,
		File: &ingest.File{Name: name, MIME: demoDOCXMIME, Data: data}, TenantID: demoTenant, CreatedBy: actor,
		DocRole: role, PublishedAt: &at})
}

func (f *flow) ingestTender(ctx context.Context, taskID int64) (*ingest.Result, error) {
	return f.ingestDoc(ctx, taskID, f.app.fixtures.Tender, "tender.docx", demoTenderName, db.DocRoleTender, tenderPublished)
}

func (f *flow) requirements(ctx context.Context, taskID int64) (*db.RequirementsView, map[int64]*db.RequirementItem, error) {
	view, err := f.app.d.ListRequirements(ctx, demoTenant, taskID)
	if err != nil {
		return nil, nil, err
	}
	byID := map[int64]*db.RequirementItem{}
	for _, it := range view.Items {
		byID[it.ID] = it
	}
	return view, byID, nil
}

func (f *flow) passVetoes(ctx context.Context, taskID int64, view *db.RequirementsView, note string) (int, error) {
	n := 0
	for _, it := range view.Items {
		if it.Category != db.ReqVetoItem || it.OutOfBid || it.IsSuperseded() {
			continue
		}
		if _, err := f.app.d.SetVetoStatus(ctx, demoTenant, taskID, it.ID, actor, db.VetoPassed, note); err != nil {
			return n, fmt.Errorf("veto %s: %w", it.Code, err)
		}
		n++
	}
	return n, nil
}

func (f *flow) createDefaultOutline(ctx context.Context, taskID int64) (*db.OutlineDetail, error) {
	return f.app.d.CreateOutline(ctx, demoTenant, taskID, actor, db.OutlineInput{Note: "E2E 默认目录（由确认后的清单生成）"})
}

func (f *flow) submitApprove(ctx context.Context, taskID int64, version int) (*db.OutlineDetail, error) {
	if _, err := f.app.d.SubmitOutline(ctx, demoTenant, taskID, version, actor); err != nil {
		return nil, err
	}
	return f.app.d.ApproveOutline(ctx, demoTenant, taskID, version, actor)
}

func (f *flow) writeAll(ctx context.Context, taskID int64, det *db.OutlineDetail) (map[string]int, error) {
	_, byID, err := f.requirements(ctx, taskID)
	if err != nil {
		return nil, err
	}
	written := map[string]int{}
	for _, sec := range det.Sections {
		reqs := make([]*db.RequirementItem, 0, len(sec.RequirementIDs))
		for _, rid := range sec.RequirementIDs {
			if it := byID[rid]; it != nil {
				reqs = append(reqs, it)
			}
		}
		sort.Slice(reqs, func(i, j int) bool { return reqs[i].Code < reqs[j].Code })
		res, err := f.app.d.SaveSectionRevision(ctx, demoTenant, actor, db.SaveRevisionInput{TaskID: taskID, SectionID: sec.ID,
			BaseRevNo: 0, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevAIInitial,
			ContentMD: stubSectionContent(sec, reqs), SummaryNote: sectionSummary(sec), DiffSummary: "E2E 确定性替身写作初稿"})
		if err != nil {
			return written, fmt.Errorf("section %s: %w", sec.Code, err)
		}
		if res.Outcome != db.SaveApplied {
			return written, fmt.Errorf("section %s save outcome is %s: %s", sec.Code, res.Outcome, res.Reason)
		}
		written[sec.Code] = res.Revision.RevNo
	}
	return written, nil
}

func (f *flow) approveAll(ctx context.Context, taskID int64, det *db.OutlineDetail) (int, error) {
	n := 0
	for _, sec := range det.Sections {
		cur, err := f.app.d.GetSection(ctx, demoTenant, taskID, sec.ID)
		if err != nil {
			return n, err
		}
		if cur.Section.CurrentRevNo == 0 {
			return n, fmt.Errorf("section %s has no current revision", sec.Code)
		}
		if _, err := f.app.d.ApproveSectionRevision(ctx, demoTenant, taskID, sec.ID, cur.Section.CurrentRevNo, actor); err != nil {
			return n, fmt.Errorf("approve %s: %w", sec.Code, err)
		}
		n++
	}
	return n, nil
}

func (f *flow) matrix(ctx context.Context, taskID, sectionID int64) (*db.VerifyMatrixResult, error) {
	return f.app.d.VerifyComplianceMatrix(ctx, demoTenant, taskID, sectionID, compliance.RuleVerifier{}, actor)
}

func (f *flow) consistency(ctx context.Context, taskID int64) (*db.ConsistencyRunDetail, error) {
	return f.app.d.RunConsistencyCheck(ctx, demoTenant, taskID, actor)
}

func (f *flow) readiness(ctx context.Context, taskID int64) (*db.ExportReadiness, error) {
	return f.app.d.ExportReadiness(ctx, demoTenant, taskID)
}

func (f *flow) assertReady(ctx context.Context, taskID int64) (*db.ExportReadiness, error) {
	rd, err := f.readiness(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !rd.Ready {
		return rd, fmt.Errorf("export readiness blocked by %s", kindList(rd.Blockers))
	}
	return rd, nil
}

// buildReady 构造一个完整可导出的任务；失败时返回带步骤的错误。
func (f *flow) buildReady(ctx context.Context, scenario string, withCorrection bool) (*db.Task, *db.OutlineDetail, error) {
	t, err := f.newTask(ctx, scenario, "E2E 门禁场景：构造可导出标书")
	if err != nil {
		return nil, nil, fmt.Errorf("create task: %w", err)
	}
	if _, err := f.ingestTender(ctx, t.ID); err != nil {
		return t, nil, fmt.Errorf("ingest tender: %w", err)
	}
	if withCorrection {
		if _, err := f.ingestCorrection(ctx, t.ID); err != nil {
			return t, nil, fmt.Errorf("ingest correction: %w", err)
		}
	}
	if _, err := f.app.d.ExtractRequirements(ctx, demoTenant, t.ID, actor); err != nil {
		return t, nil, fmt.Errorf("extract requirements: %w", err)
	}
	if _, err := f.app.d.ConfirmRequirements(ctx, demoTenant, t.ID, actor, 0); err != nil {
		return t, nil, fmt.Errorf("confirm requirements: %w", err)
	}
	view, _, err := f.requirements(ctx, t.ID)
	if err != nil {
		return t, nil, err
	}
	if _, err := f.passVetoes(ctx, t.ID, view, "E2E 虚构演示：已核对，证明材料索引见投标文件相应章节"); err != nil {
		return t, nil, err
	}
	det, err := f.createDefaultOutline(ctx, t.ID)
	if err != nil {
		return t, nil, fmt.Errorf("create outline: %w", err)
	}
	if det, err = f.submitApprove(ctx, t.ID, det.Outline.Version); err != nil {
		return t, nil, fmt.Errorf("approve outline: %w", err)
	}
	if _, err := f.writeAll(ctx, t.ID, det); err != nil {
		return t, nil, fmt.Errorf("write sections: %w", err)
	}
	if _, err := f.approveAll(ctx, t.ID, det); err != nil {
		return t, nil, fmt.Errorf("approve sections: %w", err)
	}
	mres, err := f.matrix(ctx, t.ID, 0)
	if err != nil {
		return t, nil, fmt.Errorf("matrix: %w", err)
	}
	if mres.Verified != mres.Checked || mres.Failed > 0 || mres.Pending > 0 || mres.Stale > 0 {
		return t, nil, fmt.Errorf("matrix is not fully verified: checked=%d verified=%d failed=%d pending=%d stale=%d",
			mres.Checked, mres.Verified, mres.Failed, mres.Pending, mres.Stale)
	}
	cres, err := f.consistency(ctx, t.ID)
	if err != nil {
		return t, nil, fmt.Errorf("consistency: %w", err)
	}
	if len(cres.Findings) != 0 || !cres.Current {
		return t, nil, fmt.Errorf("consistency has %d finding(s), current=%v", len(cres.Findings), cres.Current)
	}
	if _, err := f.assertReady(ctx, t.ID); err != nil {
		return t, nil, fmt.Errorf("ready: %w", err)
	}
	return t, det, nil
}

func (f *flow) ingestCorrection(ctx context.Context, taskID int64) (*ingest.Result, error) {
	return f.ingestDoc(ctx, taskID, f.app.fixtures.Correction, "correction.docx", demoCorrection, db.DocRoleCorrection, correctionPublished)
}

// ---------------------------------------------------------------------------
// 主链路
// ---------------------------------------------------------------------------

func (a *app) runMain(ctx context.Context, f *flow) {
	r := f.r
	// S1 创建隔离任务。
	task, err := f.newTask(ctx, "main", "E2E 主链路：导入招标文件到 DOCX 导出（虚构）")
	if err != nil {
		r.fail("main.setup", "main.setup.create_task", "task created", err, "")
		return
	}
	var origin int
	err = a.d.QueryRowContext(ctx, `SELECT COUNT(*) FROM exploration_nodes WHERE exploration_id = $1 AND kind = 'fact' AND state = 'origin'`,
		task.ExplorationID).Scan(&origin)
	if err != nil {
		r.fail("main.setup", "main.setup.origin_fact", "1", err, "检查推演空间 origin Fact")
		return
	}
	r.check("main.setup", "main.setup.create_task", "task created", "task created", true, "")
	r.check("main.setup", "main.setup.origin_fact", "1", strconv.Itoa(origin), origin == 1, "")

	// S2/S3 导入招标文件与更正公告。
	tenderRes, err := f.ingestTender(ctx, task.ID)
	if err != nil {
		r.fail("main.ingest", "main.ingest.tender", "tender ingested", err, "")
		return
	}
	corrRes, err := f.ingestCorrection(ctx, task.ID)
	if err != nil {
		r.fail("main.ingest", "main.ingest.correction", "correction ingested", err, "")
		return
	}
	tenderTZ, _ := parseCST(tenderPublished)
	r.check("main.ingest", "main.ingest.tender_role", "tender", tenderRes.Document.DocRole, tenderRes.Document.DocRole == db.DocRoleTender, "")
	r.check("main.ingest", "main.ingest.tender_chunks", ">0", strconv.Itoa(tenderRes.Chunks), tenderRes.Chunks > 0, "")
	r.check("main.ingest", "main.ingest.tender_tables", ">0", strconv.Itoa(tenderRes.Tables), tenderRes.Tables > 0, "")
	r.check("main.ingest", "main.ingest.tender_published", tenderPublished, tenderRes.Document.PublishedAt.In(cst).Format(time.RFC3339),
		tenderRes.Document.PublishedAt != nil && tenderRes.Document.PublishedAt.Equal(tenderTZ), "")
	r.check("main.ingest", "main.ingest.correction_role", "correction", corrRes.Document.DocRole, corrRes.Document.DocRole == db.DocRoleCorrection, "")
	r.check("main.ingest", "main.ingest.correction_chunks", ">0", strconv.Itoa(corrRes.Chunks), corrRes.Chunks > 0, "")
	r.check("main.ingest", "main.ingest.correction_later", "T2>T1", corrRes.Document.PublishedAt.In(cst).Format(time.RFC3339),
		corrRes.Document.PublishedAt != nil && corrRes.Document.PublishedAt.After(tenderTZ) &&
			corrRes.Document.PublishedAt.In(cst).Format(time.RFC3339) == correctionPublished, "")

	chunks, err := a.d.TaskChunks(ctx, task.ID, 0, 1000)
	if err != nil {
		r.fail("main.census", "main.census.chunks", "chunks readable", err, "")
		return
	}
	f.checkCensus(chunks, tenderRes, corrRes)

	// S4 提取要求。
	exres, err := a.d.ExtractRequirements(ctx, demoTenant, task.ID, actor)
	if err != nil {
		r.fail("main.extract", "main.extract.run", "extracted", err, "")
		return
	}
	view, _, err := f.requirements(ctx, task.ID)
	if err != nil {
		r.fail("main.extract", "main.extract.view", "requirements readable", err, "")
		return
	}
	f.checkGolden(exres, view)
	if err := f.writeCensus(chunks, view, map[int64]string{tenderRes.Document.ID: db.DocRoleTender, corrRes.Document.ID: db.DocRoleCorrection}); err != nil {
		r.check("main.census", "main.census.artifact", "written", err.Error(), false, "")
	}

	// S5 确认清单。
	manifest, err := a.d.ConfirmRequirements(ctx, demoTenant, task.ID, actor, 0)
	if err != nil {
		r.fail("main.confirm", "main.confirm.run", "confirmed", err, "")
		return
	}
	r.check("main.confirm", "main.confirm.status", "confirmed", manifest.Status, manifest.Status == db.ManifestConfirmed, "")
	r.check("main.confirm", "main.confirm.epoch", ">0", fmt.Sprint(manifest.Epoch), manifest.Epoch > 0, "")
	confirmedEpoch := manifest.Epoch

	// S6 核对否决项。
	if _, err := f.passVetoes(ctx, task.ID, view, "E2E 虚构演示：已核对，证明材料索引见投标文件相应章节"); err != nil {
		r.fail("main.veto", "main.veto.run", "all passed", err, "")
		return
	}
	view, _, err = f.requirements(ctx, task.ID)
	if err != nil {
		r.fail("main.veto", "main.veto.reload", "requirements readable", err, "")
		return
	}
	v := view.Stats.Veto
	r.check("main.veto", "main.veto.all_passed", fmt.Sprintf("%d/%d", v.Total, v.Total),
		fmt.Sprintf("%d/%d failed=%d pending=%d", v.Passed, v.Total, v.Failed, v.Pending), v.Total > 0 && v.Passed == v.Total && v.Failed == 0 && v.Pending == 0, "")

	// S7 创建默认大纲。
	det, err := f.createDefaultOutline(ctx, task.ID)
	if err != nil {
		r.fail("main.outline", "main.outline.create", "draft created", err, "")
		return
	}
	r.check("main.outline", "main.outline.version", "1", strconv.Itoa(det.Outline.Version), det.Outline.Version == 1, "")
	r.check("main.outline", "main.outline.epoch", "manifest epoch", strconv.FormatInt(det.Outline.RequirementsEpoch, 10),
		det.Outline.RequirementsEpoch == confirmedEpoch, "大纲 epoch 必须等于 S5 记录的清单 epoch")
	r.check("main.outline", "main.outline.sections", strconv.Itoa(a.golden.OutlineSections), strconv.Itoa(len(det.Sections)),
		len(det.Sections) == a.golden.OutlineSections, "")
	r.check("main.outline", "main.outline.mapped", strconv.Itoa(a.golden.MappedRequirements),
		fmt.Sprintf("%d/%d", det.Coverage.Mapped, det.Coverage.Total),
		det.Coverage.Total == a.golden.MappedRequirements && det.Coverage.Mapped == a.golden.MappedRequirements && len(det.Coverage.Unmapped) == 0,
		unmappedText(det.Coverage.Unmapped))

	// S8 提交并批准大纲。
	det, err = f.submitApprove(ctx, task.ID, det.Outline.Version)
	if err != nil {
		r.fail("main.outline", "main.outline.approve", "approved", err, "")
		return
	}
	r.check("main.outline", "main.outline.approved", "approved", det.Outline.Status, det.Outline.Status == db.OutlineApproved, "")
	r.check("main.outline", "main.outline.version_stable", "1", strconv.Itoa(det.Outline.Version), det.Outline.Version == 1, "")
	r.check("main.outline", "main.outline.epoch_stable", strconv.FormatInt(confirmedEpoch, 10), strconv.FormatInt(det.Outline.RequirementsEpoch, 10),
		det.Outline.RequirementsEpoch == confirmedEpoch, "")

	// S9 逐章节替身写作。
	written, err := f.writeAll(ctx, task.ID, det)
	if err != nil {
		r.fail("main.write", "main.write.run", "all applied", err, "")
		return
	}
	for _, sec := range det.Sections {
		rev, ok := written[sec.Code]
		r.check("main.write", "main.write."+sec.Code, "applied", fmt.Sprintf("rev=%d", rev), ok && rev > 0, "")
	}

	// S10 逐章节批准。
	approved, err := f.approveAll(ctx, task.ID, det)
	if err != nil {
		r.fail("main.approve", "main.approve.run", "all approved", err, "")
		return
	}
	r.check("main.approve", "main.approve.all", fmt.Sprintf("%d/%d", len(det.Sections), len(det.Sections)),
		fmt.Sprintf("%d/%d", approved, len(det.Sections)), approved == len(det.Sections), "")
	bound := 0
	for _, sec := range det.Sections {
		cur, err := a.d.GetSection(ctx, demoTenant, task.ID, sec.ID)
		if err != nil {
			r.fail("main.approve", "main.approve.bound", "approved_rev_no == current_rev_no", err, "")
			return
		}
		if cur.Section.ApprovedRevNo != nil && *cur.Section.ApprovedRevNo == cur.Section.CurrentRevNo {
			bound++
		}
	}
	r.check("main.approve", "main.approve.bound", fmt.Sprintf("%d/%d", len(det.Sections), len(det.Sections)),
		fmt.Sprintf("%d/%d", bound, len(det.Sections)), bound == len(det.Sections), "每章 approved_rev_no == current_rev_no")

	// S11 矩阵核验。
	mres, err := f.matrix(ctx, task.ID, 0)
	if err != nil {
		r.fail("main.matrix", "main.matrix.run", "verified", err, "")
		return
	}
	r.check("main.matrix", "main.matrix.checked", strconv.Itoa(a.golden.MappedRequirements), strconv.Itoa(mres.Checked),
		mres.Checked == a.golden.MappedRequirements, "")
	r.check("main.matrix", "main.matrix.verified", strconv.Itoa(mres.Checked), strconv.Itoa(mres.Verified),
		mres.Verified == mres.Checked && mres.Failed == 0 && mres.Pending == 0 && mres.Stale == 0,
		failedRowText(mres.Rows))
	r.check("main.matrix", "main.matrix.failed", "0", strconv.Itoa(mres.Failed), mres.Failed == 0, "")
	r.check("main.matrix", "main.matrix.pending", "0", strconv.Itoa(mres.Pending), mres.Pending == 0, "")
	r.check("main.matrix", "main.matrix.stale", "0", strconv.Itoa(mres.Stale), mres.Stale == 0, "")

	// S12 一致性核验。
	cres, err := f.consistency(ctx, task.ID)
	if err != nil {
		r.fail("main.consistency", "main.consistency.run", "no findings", err, "")
		return
	}
	r.check("main.consistency", "main.consistency.contradictions", "0", strconv.Itoa(cres.Run.Contradictions), cres.Run.Contradictions == 0, "")
	r.check("main.consistency", "main.consistency.omissions", "0", strconv.Itoa(cres.Run.Omissions), cres.Run.Omissions == 0, "")
	r.check("main.consistency", "main.consistency.findings", "0", strconv.Itoa(len(cres.Findings)), len(cres.Findings) == 0 && cres.Current, "")

	// S13 导出就绪。
	rd, err := f.readiness(ctx, task.ID)
	if err != nil {
		r.fail("main.readiness", "main.readiness.run", "ready", err, "")
		return
	}
	r.check("main.readiness", "main.readiness.ready", "true", strconv.FormatBool(rd.Ready), rd.Ready, kindList(rd.Blockers))
	r.check("main.readiness", "main.readiness.blockers", "0", strconv.Itoa(len(rd.Blockers)), len(rd.Blockers) == 0, kindList(rd.Blockers))
	r.mainReady = rd.Ready

	// S14 创建 DOCX 导出任务。
	jobs, pre, err := a.d.CreateExportJobs(ctx, demoTenant, task.ID, actor, db.CreateExportInput{Formats: []string{db.ExportDOCX},
		DefaultTemplateHash: export.DefaultTemplateHash(), Bidder: demoBidder})
	if err != nil {
		r.fail("main.export", "main.export.create", "1 queued job", err, preflightText(pre))
		return
	}
	r.check("main.export", "main.export.jobs", "1", strconv.Itoa(len(jobs)), len(jobs) == 1, "")
	if len(jobs) == 1 {
		r.check("main.export", "main.export.status", db.ExportQueued, jobs[0].Status, jobs[0].Status == db.ExportQueued, "")
	}
	r.check("main.export", "main.export.preflight", "ready", strconv.FormatBool(pre != nil && pre.Readiness.Ready),
		pre != nil && pre.Readiness.Ready && len(pre.Warnings) == 0, preflightText(pre))
	if len(jobs) == 0 {
		return
	}

	// S15 执行、提交并回读导出。
	a.exportDOCX(ctx, f, task.ID, jobs[0].ID)
}

func (f *flow) checkCensus(chunks []*db.DocumentChunk, tenderRes, corrRes *ingest.Result) {
	tenderCodes := map[string]bool{}
	corrCodes := map[string]bool{}
	for _, c := range chunks {
		switch c.DocID {
		case tenderRes.Document.ID:
			tenderCodes[c.SectionCode] = true
		case corrRes.Document.ID:
			corrCodes[c.SectionCode] = true
		}
	}
	var missingTender, missingCorr []string
	for _, code := range []string{"第一章", "1.1", "1.1.1", "1.1.2", "1.2", "1.3", "1.4", "第二章", "2.1",
		"2.1.1", "2.1.2", "2.1.3", "2.2", "2.2.1", "2.2.2", "2.3", "2.3.1", "2.3.2", "第三章", "3.1"} {
		if !tenderCodes[code] {
			missingTender = append(missingTender, code)
		}
	}
	if !corrCodes["2.3.2"] {
		missingCorr = append(missingCorr, "2.3.2")
	}
	f.r.check("main.census", "main.census.tender_sections", "20 required codes", fmt.Sprintf("missing=%d", len(missingTender)),
		len(missingTender) == 0, strings.Join(missingTender, ","))
	f.r.check("main.census", "main.census.correction_section", "2.3.2", fmt.Sprintf("missing=%d", len(missingCorr)),
		len(missingCorr) == 0, strings.Join(missingCorr, ","))

	// 表格分块单独核对：审查表与评分表都必须落在各自段落内。
	byCode := map[string]*db.DocumentChunk{}
	for _, c := range chunks {
		if c.DocID == tenderRes.Document.ID {
			byCode[c.SectionCode] = c
		}
	}
	for _, code := range []string{"1.2", "1.3", "3.1"} {
		c := byCode[code]
		f.r.check("main.census", "main.census.table."+code, "1 table", fmt.Sprintf("table_count=%d", chunkTableCount(c)),
			c != nil && chunkTableCount(c) >= 1, "")
	}
}

func chunkTableCount(c *db.DocumentChunk) int {
	if c == nil || len(c.Tables) <= 2 {
		return 0
	}
	var tables []json.RawMessage
	_ = json.Unmarshal(c.Tables, &tables)
	return len(tables)
}

func (f *flow) checkGolden(exres *db.ExtractResult, view *db.RequirementsView) {
	g := f.app.golden
	vetoByKind := map[string]int{}
	scoring := map[string]float64{}
	scoringTotal, priceOut := 0, 0
	mandatoryClauses, nonScoring := 0, 0
	for _, it := range view.Items {
		if it.IsSuperseded() {
			continue
		}
		switch it.Category {
		case db.ReqVetoItem:
			vetoByKind[it.VetoKind]++
		case db.ReqMandatoryClause:
			mandatoryClauses++
		case db.ReqNonScoringRequired:
			nonScoring++
		}
		if it.Category == db.ReqScoringItem && it.ParentID == nil {
			scoringTotal++
			scoring[it.Title] = it.ScoreWeight
			if !it.InTechnicalScope() {
				priceOut++
			}
		}
	}
	vetoTotal := 0
	for _, n := range vetoByKind {
		vetoTotal += n
	}
	f.r.check("main.extract", "golden.veto_total", strconv.Itoa(g.VetoTotal), strconv.Itoa(vetoTotal), vetoTotal == g.VetoTotal,
		fmt.Sprintf("by_kind=%s", mapTextInt(vetoByKind)))
	for _, kind := range []string{db.VetoQualification, db.VetoConformity, db.VetoStatutory} {
		f.r.check("main.extract", "golden.veto."+kind, strconv.Itoa(g.VetoByKind[kind]), strconv.Itoa(vetoByKind[kind]),
			vetoByKind[kind] == g.VetoByKind[kind], "")
	}
	f.r.check("main.extract", "golden.scoring_total", strconv.Itoa(g.ScoringTotal), strconv.Itoa(scoringTotal), scoringTotal == g.ScoringTotal, "")
	f.r.check("main.extract", "golden.scoring_weights", mapTextFloat(g.ScoringWeights), mapTextFloat(scoring),
		mapTextFloat(g.ScoringWeights) == mapTextFloat(scoring), "")
	f.r.check("main.extract", "golden.mandatory_clause_total", strconv.Itoa(g.MandatoryClauseTotal), strconv.Itoa(mandatoryClauses),
		mandatoryClauses == g.MandatoryClauseTotal, "")
	f.r.check("main.extract", "golden.non_scoring_required_total", strconv.Itoa(g.NonScoringTotal), strconv.Itoa(nonScoring),
		nonScoring == g.NonScoringTotal, "")
	f.r.check("main.extract", "golden.price_out_of_scope", strconv.Itoa(g.PriceItemsOutOfScope), strconv.Itoa(priceOut),
		priceOut == g.PriceItemsOutOfScope, "")
	f.r.check("main.extract", "golden.superseded", strconv.Itoa(g.Superseded), strconv.Itoa(exres.Superseded),
		exres.Superseded == g.Superseded && view.Stats.Superseded == g.Superseded, "")
	deadline := ""
	if view.Manifest.BidDeadline != nil {
		deadline = view.Manifest.BidDeadline.In(cst).Format(time.RFC3339)
	}
	f.r.check("main.extract", "golden.bid_deadline", g.BidDeadline, deadline, deadline == g.BidDeadline, "")
}

func mapTextInt(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ",")
}

func mapTextFloat(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%g", k, m[k]))
	}
	return strings.Join(parts, ",")
}

func unmappedText(refs []db.RequirementRef) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, len(refs))
	for i, ref := range refs {
		parts[i] = ref.Code
	}
	return strings.Join(parts, ",")
}

func failedRowText(rows []*db.ComplianceRow) string {
	var out []string
	for _, row := range rows {
		if row.VerifyStatus != compliance.StatusVerified {
			out = append(out, fmt.Sprintf("%s@%s:%s %s", row.RequirementTitle, row.SectionCode, row.VerifyStatus, row.FailureReason))
		}
	}
	return strings.Join(out, "; ")
}

func preflightText(pre *db.ExportPreflight) string {
	if pre == nil || pre.Readiness == nil {
		return ""
	}
	return kindList(pre.Readiness.Blockers)
}

// writeCensus 写出“提取普查表”产物：分块编号、标题、内容摘要与提取出的要求编码。
func (f *flow) writeCensus(chunks []*db.DocumentChunk, view *db.RequirementsView, roles map[int64]string) error {
	reqByChunk := map[int64][]string{}
	for _, it := range view.Items {
		if it.SourceChunkID != nil {
			reqByChunk[*it.SourceChunkID] = append(reqByChunk[*it.SourceChunkID], it.Code)
		}
	}
	var b strings.Builder
	b.WriteString("# 提取普查表（E 级，虚构数据）\n\n")
	b.WriteString("> 本表由 `eval/bid/accept` 自动生成，用于核对每个条款是否被切成独立分块，以及提取器实际产出的要求编码。\n")
	b.WriteString("> 只证明工程流程，不代表真实招标文件的召回率或准确率。\n\n")
	b.WriteString("| 文档 | 序号 | 层级 | 编号 | 标题 | 强制 | 表格 | 内容摘要 | 提取要求 |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, c := range chunks {
		role := roles[c.DocID]
		if role == "" {
			role = fmt.Sprintf("doc-%d", c.DocID)
		}
		codes := reqByChunk[c.ID]
		sort.Strings(codes)
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %s | %v | %d | %s | %s |\n", role, c.Seq, c.Level, mdInline(c.SectionCode),
			mdInline(c.SectionTitle), c.IsMandatory, chunkTableCount(c), mdInline(excerpt(c.Content, 80)), mdInline(strings.Join(codes, ", ")))
	}
	path := filepath.Join(f.app.cfg.ArtifactsDir, fmt.Sprintf("extraction-census-r%d.md", f.r.index))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	art, err := artifactOf("extraction_census", path, "分块普查表（编号/标题/内容摘要/提取要求）")
	if err != nil {
		return err
	}
	f.r.artifacts = append(f.r.artifacts, art)
	return nil
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func mdInline(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// DOCX 导出执行与回读
// ---------------------------------------------------------------------------

func (a *app) exportDOCX(ctx context.Context, f *flow, taskID, jobID int64) {
	r := f.r
	ref, err := a.d.ClaimExportJob(ctx, "e2e-acceptance", 2*time.Minute, taskID)
	if err != nil {
		r.fail("main.export_readback", "export.claim", "claimed", err, "")
		return
	}
	if ref == nil || ref.ID != jobID {
		r.check("main.export_readback", "export.claim", "claimed", "no claimable job", false, "")
		return
	}
	r.check("main.export_readback", "export.claim", "running", ref.Status, ref.Status == db.ExportRunning, "")
	res, err := a.exportSvc.Execute(ctx, ref.Tenant, ref.ExportJob)
	if err != nil {
		_ = a.d.FailExportJob(ctx, ref.ID, ref.LeaseToken, err.Error(), false)
		r.fail("main.export_readback", "export.execute", "succeeded", err, "")
		return
	}
	if err := a.d.CompleteExportJob(ctx, ref.ID, ref.LeaseToken, *res); err != nil {
		r.fail("main.export_readback", "export.complete", "succeeded", err, "")
		return
	}
	job, err := a.d.GetExportJob(ctx, demoTenant, taskID, ref.ID)
	if err != nil {
		r.fail("main.export_readback", "export.reload", "succeeded", err, "")
		return
	}
	r.check("main.export_readback", "export.status", db.ExportSucceeded, job.Status, job.Status == db.ExportSucceeded, job.Error)
	abs, err := a.exportSvc.ArtifactPath(job)
	if err != nil {
		r.fail("main.export_readback", "export.artifact", "exists", err, "")
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		r.fail("main.export_readback", "export.artifact", "exists", err, "")
		return
	}
	r.check("main.export_readback", "export.artifact_size", "matches task record", boolText(job.ArtifactSize == int64(len(data)) && len(data) > 0),
		job.ArtifactSize == int64(len(data)) && len(data) > 0, "")
	r.check("main.export_readback", "export.artifact_hash", "matches task record", boolText(job.ArtifactHash == sha256Hex(data)),
		job.ArtifactHash == sha256Hex(data), "")

	dst := filepath.Join(a.cfg.ArtifactsDir, "exports", fmt.Sprintf("main-r%d.docx", r.index))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		r.fail("main.export_readback", "export.copy", "copied", err, "")
		return
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		r.fail("main.export_readback", "export.copy", "copied", err, "")
		return
	}
	if art, err := artifactOf("export_docx", dst, "主链路导出的技术标 DOCX（M1 未启用 文档引擎）"); err == nil {
		r.artifacts = append(r.artifacts, art)
	}

	parsed, perr := ingest.Parse(ctx, ingest.File{Name: "main-export.docx", MIME: demoDOCXMIME, Data: data}, ingest.ParseOptions{})
	if perr != nil {
		r.fail("main.export_readback", "export.parse", "parsed", perr, "")
		return
	}
	r.check("main.export_readback", "export.parse", "no error", "ok", len(parsed.Text) > 0, "")
	var missing []string
	det, err := a.d.GetOutline(ctx, demoTenant, taskID, job.OutlineVersion)
	if err != nil {
		r.fail("main.export_readback", "export.headings", "readable", err, "")
		return
	}
	r.headings = r.headings[:0]
	for _, sec := range det.Sections {
		heading := strings.TrimSpace(sec.Code + " " + sec.Title)
		r.headings = append(r.headings, heading)
		if !strings.Contains(parsed.Text, heading) {
			missing = append(missing, heading)
		}
	}
	r.check("main.export_readback", "export.headings", strconv.Itoa(len(det.Sections)), strconv.Itoa(len(det.Sections)-len(missing)),
		len(missing) == 0, strings.Join(missing, "; "))
	needle := "我方承诺完全满足上述要求"
	r.check("main.export_readback", "export.body", "present", strconv.FormatBool(strings.Contains(parsed.Text, needle)),
		strings.Contains(parsed.Text, needle), "")
	kinds := warningKinds(job.Warnings)
	sort.Strings(kinds)
	r.warningKinds = kinds
	tocOK := job.TOCRefreshed || containsString(kinds, db.WarnTOCNotRefreshed)
	r.check("main.export_readback", "export.toc", "refreshed or warned", fmt.Sprintf("refreshed=%v warnings=%s", job.TOCRefreshed, strings.Join(kinds, ",")),
		tocOK, "M1 只导出 DOCX；目录未刷新允许为 false，但必须记录 toc_not_refreshed 警告")
	r.exportReadback = "ok"
	r.mainExported = job.Status == db.ExportSucceeded && len(missing) == 0
}

func warningKinds(warns []db.ExportWarning) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range warns {
		if !seen[w.Kind] {
			seen[w.Kind] = true
			out = append(out, w.Kind)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 门禁与解析校验 G01 ~ G13
// ---------------------------------------------------------------------------

func (a *app) runGates(ctx context.Context, f *flow) {
	a.gateG01(ctx, f)
	a.gateG02(ctx, f)
	a.gateG03(ctx, f)
	a.gateG04(ctx, f)
	a.gateG05(ctx, f)
	a.gateG06(ctx, f)
	a.gateG07(ctx, f)
	a.gateG08(ctx, f)
	a.gateG09(ctx, f)
	a.gateG10(ctx, f)
	a.gateG11(ctx, f)
	a.gateG12(ctx, f)
	a.gateG13(ctx, f)
}

func (a *app) gateTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 60*time.Second)
}

// prepareToOutline 导入、提取、确认并（可选）核对否决项，返回任务与清单视图；不创建大纲。
func (f *flow) prepareToOutline(ctx context.Context, scenario string, passVetoes bool) (*db.Task, *db.RequirementsView, error) {
	t, err := f.newTask(ctx, scenario, "E2E 门禁场景：准备已确认清单")
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.ingestTender(ctx, t.ID); err != nil {
		return t, nil, err
	}
	if _, err := f.ingestCorrection(ctx, t.ID); err != nil {
		return t, nil, err
	}
	if _, err := f.app.d.ExtractRequirements(ctx, demoTenant, t.ID, actor); err != nil {
		return t, nil, err
	}
	if _, err := f.app.d.ConfirmRequirements(ctx, demoTenant, t.ID, actor, 0); err != nil {
		return t, nil, err
	}
	view, _, err := f.requirements(ctx, t.ID)
	if err != nil {
		return t, nil, err
	}
	if passVetoes {
		if _, err := f.passVetoes(ctx, t.ID, view, "E2E 虚构演示：已核对通过"); err != nil {
			return t, nil, err
		}
	}
	return t, view, nil
}

func (f *flow) approveDefaultOutline(ctx context.Context, taskID int64) (*db.OutlineDetail, error) {
	det, err := f.createDefaultOutline(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return f.submitApprove(ctx, taskID, det.Outline.Version)
}

func (a *app) gateG01(ctx context.Context, f *flow) {
	a.runGate(f, "G01", "no_approved_outline", func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, _, err := f.prepareToOutline(ctx, "G01", false)
		if err != nil {
			return "", false, false, err
		}
		rd, err := f.readiness(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		observed := kindList(rd.Blockers)
		g := f.r.gates[len(f.r.gates)-1]
		g.check("G01.trigger", db.BlockNoApprovedOutline, observed, kindsContain(rd.Blockers, db.BlockNoApprovedOutline), "")
		if _, err := f.approveDefaultOutline(ctx, t.ID); err != nil {
			return observed, true, false, err
		}
		rd, err = f.readiness(ctx, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G01.remediated", "no "+db.BlockNoApprovedOutline, kindList(rd.Blockers), !kindsContain(rd.Blockers, db.BlockNoApprovedOutline), "")
		return observed, true, true, nil
	})
}

func (a *app) gateG02(ctx context.Context, f *flow) {
	a.runGate(f, "G02", "ErrRequirementsNotConfirmed", func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, err := f.newTask(ctx, "G02", "E2E 门禁场景：清单未确认")
		if err != nil {
			return "", false, false, err
		}
		if _, err := f.ingestTender(ctx, t.ID); err != nil {
			return "", false, false, err
		}
		if _, err := f.ingestCorrection(ctx, t.ID); err != nil {
			return "", false, false, err
		}
		if _, err := a.d.ExtractRequirements(ctx, demoTenant, t.ID, actor); err != nil {
			return "", false, false, err
		}
		det, err := f.createDefaultOutline(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		g := f.r.gates[len(f.r.gates)-1]
		_, subErr := a.d.SubmitOutline(ctx, demoTenant, t.ID, det.Outline.Version, actor)
		blocked := errors.Is(subErr, db.ErrRequirementsNotConfirmed)
		observed := "nil"
		switch {
		case blocked:
			observed = "ErrRequirementsNotConfirmed"
		case subErr != nil:
			observed = subErr.Error()
		}
		g.check("G02.trigger", "ErrRequirementsNotConfirmed", observed, blocked, "")
		if _, err := a.d.ConfirmRequirements(ctx, demoTenant, t.ID, actor, 0); err != nil {
			return observed, blocked, false, err
		}
		if _, err := a.d.SubmitOutline(ctx, demoTenant, t.ID, det.Outline.Version, actor); err != nil {
			return observed, blocked, false, err
		}
		g.check("G02.remediated", "submitted", "submitted", true, "")
		return observed, blocked, true, nil
	})
}

func (a *app) gateG03(ctx context.Context, f *flow) {
	a.runGate(f, "G03", db.BlockVetoPending, func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, view, err := f.prepareToOutline(ctx, "G03", false)
		if err != nil {
			return "", false, false, err
		}
		if _, err := f.approveDefaultOutline(ctx, t.ID); err != nil {
			return "", false, false, err
		}
		rd, err := f.readiness(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		observed := kindList(rd.Blockers)
		g := f.r.gates[len(f.r.gates)-1]
		g.check("G03.trigger", db.BlockVetoPending, observed, kindsContain(rd.Blockers, db.BlockVetoPending), "全部否决项保持 pending")
		if _, err := f.passVetoes(ctx, t.ID, view, "E2E 虚构演示：已核对通过"); err != nil {
			return observed, true, false, err
		}
		rd, err = f.readiness(ctx, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G03.remediated", "no "+db.BlockVetoPending, kindList(rd.Blockers), !kindsContain(rd.Blockers, db.BlockVetoPending), "")
		return observed, true, true, nil
	})
}

func (a *app) gateG04(ctx context.Context, f *flow) {
	a.runGate(f, "G04", db.BlockVetoFailed, func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, view, err := f.prepareToOutline(ctx, "G04", true)
		if err != nil {
			return "", false, false, err
		}
		var failed *db.RequirementItem
		for _, it := range view.Items {
			if it.Category == db.ReqVetoItem && !it.OutOfBid && !it.IsSuperseded() {
				failed = it
				break
			}
		}
		if failed == nil {
			return "", false, false, errors.New("no veto item available")
		}
		if _, err := a.d.SetVetoStatus(ctx, demoTenant, t.ID, failed.ID, actor, db.VetoFailed, "E2E 虚构演示：模拟不满足条件"); err != nil {
			return "", false, false, err
		}
		if _, err := f.approveDefaultOutline(ctx, t.ID); err != nil {
			return "", false, false, err
		}
		rd, err := f.readiness(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		observed := kindList(rd.Blockers)
		g := f.r.gates[len(f.r.gates)-1]
		g.check("G04.trigger", db.BlockVetoFailed, observed, kindsContain(rd.Blockers, db.BlockVetoFailed), "")
		if _, err := a.d.SetVetoStatus(ctx, demoTenant, t.ID, failed.ID, actor, db.VetoPassed, "E2E 虚构演示：修正后已核对通过"); err != nil {
			return observed, true, false, err
		}
		rd, err = f.readiness(ctx, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G04.remediated", "no "+db.BlockVetoFailed, kindList(rd.Blockers), !kindsContain(rd.Blockers, db.BlockVetoFailed), "")
		return observed, true, true, nil
	})
}

func (a *app) gateG05(ctx context.Context, f *flow) {
	a.runGate(f, "G05", db.BlockNoBid, func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, _, err := f.prepareToOutline(ctx, "G05", true)
		if err != nil {
			return "", false, false, err
		}
		if _, err := f.approveDefaultOutline(ctx, t.ID); err != nil {
			return "", false, false, err
		}
		noBid, note := db.BidDecisionNoBid, "虚构场景：主动放弃"
		if _, err := a.d.SetBidScope(ctx, demoTenant, t.ID, actor, db.BidScopeInput{Decision: &noBid, DecisionNote: &note}); err != nil {
			return "", false, false, err
		}
		rd, err := f.readiness(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		observed := kindList(rd.Blockers)
		g := f.r.gates[len(f.r.gates)-1]
		g.check("G05.trigger", db.BlockNoBid, observed, kindsContain(rd.Blockers, db.BlockNoBid), "")
		bid, bidNote := db.BidDecisionBid, "虚构场景：复核后决定投标"
		if _, err := a.d.SetBidScope(ctx, demoTenant, t.ID, actor, db.BidScopeInput{Decision: &bid, DecisionNote: &bidNote}); err != nil {
			return observed, true, false, err
		}
		rd, err = f.readiness(ctx, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G05.remediated", "no "+db.BlockNoBid, kindList(rd.Blockers), !kindsContain(rd.Blockers, db.BlockNoBid), "已确认无 failed 否决项")
		return observed, true, true, nil
	})
}

func (a *app) gateG06(ctx context.Context, f *flow) {
	a.runGate(f, "G06", db.BlockNoApprovedOutline+" (requirement change)", func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, det, err := f.buildReady(ctx, "G06", true)
		if err != nil {
			return "", false, false, err
		}
		g := f.r.gates[len(f.r.gates)-1]
		code, category, title, desc := "NEW-1", db.ReqNonScoringRequired, "新增运维要求",
			"投标人须承诺提供每季度一次的现场巡检服务，并提交巡检记录。"
		created, err := a.d.CreateRequirement(ctx, demoTenant, t.ID, actor, db.RequirementInput{Code: &code, Category: &category,
			Title: &title, Description: &desc})
		if err != nil {
			return "", false, false, err
		}
		rd, err := f.readiness(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		observed := kindList(rd.Blockers)
		g.check("G06.trigger", db.BlockNoApprovedOutline, observed, kindsContain(rd.Blockers, db.BlockNoApprovedOutline), "")
		list, err := a.d.ListOutlines(ctx, demoTenant, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		oldStatus, newStatus, newVersion := "", "", 0
		for _, o := range list.Outlines {
			switch o.Version {
			case det.Outline.Version:
				oldStatus = o.Status
			default:
				if o.Version > newVersion {
					newVersion, newStatus = o.Version, o.Status
				}
			}
		}
		g.check("G06.old_outline", "superseded", oldStatus, oldStatus == db.OutlineSuperseded, "")
		g.check("G06.new_draft", "draft", newStatus, newStatus == db.OutlineDraft, "")
		view, _, err := f.requirements(ctx, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G06.epoch_grew", ">", fmt.Sprintf("%d>%d", view.Manifest.Epoch, det.Outline.RequirementsEpoch),
			view.Manifest.Epoch > det.Outline.RequirementsEpoch, "")
		unmapped := false
		for _, u := range view.Stats.Unmapped {
			if u == code {
				unmapped = true
			}
		}
		g.check("G06.unmapped", code, fmt.Sprint(unmapped), unmapped, "")
		if _, err := a.d.ConfirmRequirements(ctx, demoTenant, t.ID, actor, 0); err != nil {
			return observed, true, false, err
		}
		m, err := a.d.RequirementManifest(ctx, demoTenant, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G06.confirmed", db.ManifestConfirmed, m.Status, m.Status == db.ManifestConfirmed, "")
		draft, err := a.d.GetOutline(ctx, demoTenant, t.ID, newVersion)
		if err != nil {
			return observed, true, false, err
		}
		secs := make([]db.SectionInput, 0, len(draft.Sections))
		mapped := false
		for _, s := range draft.Sections {
			in := db.SectionInput{Code: s.Code, Title: s.Title, Level: s.Level, WritingGuide: s.WritingGuide, TargetWords: s.TargetWords,
				RequirementIDs: append([]int64{}, s.RequirementIDs...), DependsOn: append([]string{}, s.DependsOnCodes...)}
			if strings.Contains(s.Title, "其他必答项") {
				in.RequirementIDs = append(in.RequirementIDs, created.ID)
				mapped = true
			}
			secs = append(secs, in)
		}
		if !mapped {
			return observed, true, false, errors.New("no section available to map the new requirement")
		}
		g.check("G06.mapped_new", "true", "true", mapped, "编辑自动派生草案，把新要求映射到已有章节")
		if _, err := a.d.UpdateOutline(ctx, demoTenant, t.ID, newVersion, actor, db.OutlineInput{Sections: secs, Note: "E2E G06：映射新增要求"}); err != nil {
			return observed, true, false, err
		}
		approved, err := f.submitApprove(ctx, t.ID, newVersion)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G06.approved", "approved", approved.Outline.Status, approved.Outline.Status == db.OutlineApproved, "")
		g.check("G06.coverage", "12/12", fmt.Sprintf("%d/%d", approved.Coverage.Mapped, approved.Coverage.Total),
			approved.Coverage.Total == 12 && approved.Coverage.Mapped == 12 && len(approved.Coverage.Unmapped) == 0, unmappedText(approved.Coverage.Unmapped))
		written, err := f.writeAll(ctx, t.ID, approved)
		if err != nil {
			return observed, true, false, err
		}
		approvedCount, err := f.approveAll(ctx, t.ID, approved)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G06.sections", fmt.Sprintf("%d/%d", len(approved.Sections), len(approved.Sections)),
			fmt.Sprintf("written=%d approved=%d", len(written), approvedCount),
			len(written) == len(approved.Sections) && approvedCount == len(approved.Sections), "新大纲全部章节重新写回并批准")
		mres, err := f.matrix(ctx, t.ID, 0)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G06.matrix", fmt.Sprintf("%d/%d", mres.Checked, mres.Checked), fmt.Sprintf("verified=%d failed=%d pending=%d", mres.Verified, mres.Failed, mres.Pending),
			mres.Verified == mres.Checked && mres.Failed == 0 && mres.Pending == 0, failedRowText(mres.Rows))
		cres, err := f.consistency(ctx, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G06.consistency", "0 findings", fmt.Sprintf("findings=%d current=%v", len(cres.Findings), cres.Current),
			len(cres.Findings) == 0 && cres.Current, "")
		if _, err := f.assertReady(ctx, t.ID); err != nil {
			return observed, true, false, err
		}
		g.check("G06.full_ready", "true", "true", true, "新增要求后完整恢复")
		return observed, true, true, nil
	})
}

func (a *app) gateG07(ctx context.Context, f *flow) {
	a.runGate(f, "G07", db.BlockSectionUnapproved, func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, det, err := f.buildReady(ctx, "G07", true)
		if err != nil {
			return "", false, false, err
		}
		sec := findSection(det, "2.1")
		if sec == nil {
			return "", false, false, errors.New("section 2.1 not found")
		}
		cur, err := a.d.GetSection(ctx, demoTenant, t.ID, sec.ID)
		if err != nil {
			return "", false, false, err
		}
		res, err := a.d.SaveSectionRevision(ctx, demoTenant, actor, db.SaveRevisionInput{TaskID: t.ID, SectionID: sec.ID,
			BaseRevNo: cur.Section.CurrentRevNo, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit,
			ContentMD: stubSectionContent(sec, sectionReqs(cur)) + "\n\n补充说明：我方将结合现场条件细化实施安排。", SummaryNote: "G07 未批准草稿"})
		if err != nil || res.Outcome != db.SaveApplied {
			return "", false, false, fmt.Errorf("save draft: %v %+v", err, res)
		}
		rd, err := f.readiness(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		observed := kindList(rd.Blockers)
		g := f.r.gates[len(f.r.gates)-1]
		g.check("G07.trigger", db.BlockSectionUnapproved, observed, kindsContain(rd.Blockers, db.BlockSectionUnapproved), "")
		if _, err := a.d.ApproveSectionRevision(ctx, demoTenant, t.ID, sec.ID, res.Section.CurrentRevNo, actor); err != nil {
			return observed, true, false, err
		}
		rd, err = f.readiness(ctx, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G07.remediated", "no "+db.BlockSectionUnapproved, kindList(rd.Blockers), !kindsContain(rd.Blockers, db.BlockSectionUnapproved), "")
		return observed, true, true, nil
	})
}

func sectionReqs(det *db.SectionDetail) []*db.RequirementItem { return det.Requirements }

func (a *app) gateG08(ctx context.Context, f *flow) {
	a.runGate(f, "G08", db.BlockMatrixFailed, func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, det, err := f.buildReady(ctx, "G08", true)
		if err != nil {
			return "", false, false, err
		}
		sec := findSection(det, "2.1")
		if sec == nil {
			return "", false, false, errors.New("section 2.1 not found")
		}
		cur, err := a.d.GetSection(ctx, demoTenant, t.ID, sec.ID)
		if err != nil {
			return "", false, false, err
		}
		bad, err := a.d.SaveSectionRevision(ctx, demoTenant, actor, db.SaveRevisionInput{TaskID: t.ID, SectionID: sec.ID,
			BaseRevNo: cur.Section.CurrentRevNo, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit,
			ContentMD: "详见招标文件。", SummaryNote: "G08 只挂引用"})
		if err != nil || bad.Outcome != db.SaveApplied {
			return "", false, false, fmt.Errorf("save thin draft: %v %+v", err, bad)
		}
		if _, err := a.d.ApproveSectionRevision(ctx, demoTenant, t.ID, sec.ID, bad.Section.CurrentRevNo, actor); err != nil {
			return "", false, false, err
		}
		mres, err := f.matrix(ctx, t.ID, sec.ID)
		if err != nil {
			return "", false, false, err
		}
		rd, err := f.readiness(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		observed := kindList(rd.Blockers)
		g := f.r.gates[len(f.r.gates)-1]
		g.check("G08.matrix_failed", "1", strconv.Itoa(mres.Failed), mres.Failed == 1, failedRowText(mres.Rows))
		g.check("G08.trigger", db.BlockMatrixFailed, observed, kindsContain(rd.Blockers, db.BlockMatrixFailed), "")
		cur, err = a.d.GetSection(ctx, demoTenant, t.ID, sec.ID)
		if err != nil {
			return observed, true, false, err
		}
		reqs := cur.Requirements
		good, err := a.d.SaveSectionRevision(ctx, demoTenant, actor, db.SaveRevisionInput{TaskID: t.ID, SectionID: sec.ID,
			BaseRevNo: cur.Section.CurrentRevNo, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit,
			ContentMD: stubSectionContent(sec, reqs), SummaryNote: "G08 修正为实质响应"})
		if err != nil || good.Outcome != db.SaveApplied {
			return observed, true, false, fmt.Errorf("save substantive draft: %v %+v", err, good)
		}
		if _, err := a.d.ApproveSectionRevision(ctx, demoTenant, t.ID, sec.ID, good.Section.CurrentRevNo, actor); err != nil {
			return observed, true, false, err
		}
		mres, err = f.matrix(ctx, t.ID, sec.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G08.remediated", "verified", fmt.Sprintf("verified=%d failed=%d", mres.Verified, mres.Failed),
			mres.Verified == mres.Checked && mres.Failed == 0, failedRowText(mres.Rows))
		return observed, true, true, nil
	})
}

func (a *app) gateG09(ctx context.Context, f *flow) {
	a.runGate(f, "G09", db.BlockFindingOpen, func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, err := f.newTask(ctx, "G09", "E2E 门禁场景：一致性冲突夹具")
		if err != nil {
			return "", false, false, err
		}
		if _, err := f.ingestDoc(ctx, t.ID, f.app.fixtures.G09, "g09-consistency.docx", "G09 一致性冲突夹具（虚构）", db.DocRoleTender, tenderPublished); err != nil {
			return "", false, false, err
		}
		if _, err := a.d.ExtractRequirements(ctx, demoTenant, t.ID, actor); err != nil {
			return "", false, false, err
		}
		if _, err := a.d.ConfirmRequirements(ctx, demoTenant, t.ID, actor, 0); err != nil {
			return "", false, false, err
		}
		view, _, err := f.requirements(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		if _, err := f.passVetoes(ctx, t.ID, view, "E2E G09 虚构演示：否决项核对通过"); err != nil {
			return "", false, false, err
		}
		var req1, req2 *db.RequirementItem
		for _, it := range view.Items {
			if it.IsSuperseded() {
				continue
			}
			switch {
			case strings.Contains(it.Title, "建设工期"):
				req1 = it
			case strings.Contains(it.Title, "交付周期"):
				req2 = it
			}
		}
		if req1 == nil || req2 == nil {
			return "", false, false, fmt.Errorf("g09 fixture must extract exactly two requirements (got %d items)", len(view.Items))
		}
		det, err := a.d.CreateOutline(ctx, demoTenant, t.ID, actor, db.OutlineInput{Sections: []db.SectionInput{
			{Code: "1", Title: "建设工期响应章节", Level: 1, RequirementIDs: []int64{req1.ID}},
			{Code: "2", Title: "交付周期响应章节", Level: 1, RequirementIDs: []int64{req2.ID}},
		}})
		if err != nil {
			return "", false, false, err
		}
		det, err = f.submitApprove(ctx, t.ID, det.Outline.Version)
		if err != nil {
			return "", false, false, err
		}
		secA, secB := findSection(det, "1"), findSection(det, "2")
		if secA == nil || secB == nil {
			return "", false, false, errors.New("g09 sections missing")
		}
		save := func(sec *db.ProposalSection, content string) error {
			cur, err := a.d.GetSection(ctx, demoTenant, t.ID, sec.ID)
			if err != nil {
				return err
			}
			res, err := a.d.SaveSectionRevision(ctx, demoTenant, actor, db.SaveRevisionInput{TaskID: t.ID, SectionID: sec.ID,
				BaseRevNo: cur.Section.CurrentRevNo, BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit,
				ContentMD: content, SummaryNote: "G09 冲突夹具"})
			if err != nil || res.Outcome != db.SaveApplied {
				return fmt.Errorf("save %s: %v %+v", sec.Code, err, res)
			}
			_, err = a.d.ApproveSectionRevision(ctx, demoTenant, t.ID, sec.ID, res.Section.CurrentRevNo, actor)
			return err
		}
		if err := save(secA, stubDurationSection(req1, 90)); err != nil {
			return "", false, false, err
		}
		if err := save(secB, stubDurationSection(req2, 120)); err != nil {
			return "", false, false, err
		}
		mres, err := f.matrix(ctx, t.ID, 0)
		if err != nil {
			return "", false, false, err
		}
		g := f.r.gates[len(f.r.gates)-1]
		g.check("G09.matrix", "2 verified", fmt.Sprintf("verified=%d failed=%d", mres.Verified, mres.Failed),
			mres.Verified == 2 && mres.Checked == 2 && mres.Failed == 0, failedRowText(mres.Rows))
		cres, err := f.consistency(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		rd, err := f.readiness(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		observed := kindList(rd.Blockers)
		g.check("G09.trigger", "contradiction>0", fmt.Sprintf("contradictions=%d", cres.Run.Contradictions),
			cres.Run.Contradictions > 0, "")
		g.check("G09.finding_open", db.BlockFindingOpen, observed, kindsContain(rd.Blockers, db.BlockFindingOpen), "")
		// 统一为 120 天，重新保存并批准受影响章节。
		if err := save(secA, stubDurationSection(req1, 120)); err != nil {
			return observed, true, false, err
		}
		if err := save(secB, stubDurationSection(req2, 120)); err != nil {
			return observed, true, false, err
		}
		approvedSecs := 0
		for _, sec := range []*db.ProposalSection{secA, secB} {
			cur, err := a.d.GetSection(ctx, demoTenant, t.ID, sec.ID)
			if err != nil {
				return observed, true, false, err
			}
			if cur.Section.ApprovedRevNo != nil && *cur.Section.ApprovedRevNo == cur.Section.CurrentRevNo {
				approvedSecs++
			}
		}
		g.check("G09.sections", "2/2", fmt.Sprintf("%d/2", approvedSecs), approvedSecs == 2, "重新保存并批准受影响章节")
		if _, err := f.matrix(ctx, t.ID, 0); err != nil {
			return observed, true, false, err
		}
		cres, err = f.consistency(ctx, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G09.remediated", "no open finding", fmt.Sprintf("findings=%d", len(cres.Findings)),
			len(cres.Findings) == 0 && cres.Current, "")
		if _, err := f.assertReady(ctx, t.ID); err != nil {
			return observed, true, false, err
		}
		g.check("G09.full_ready", "true", "true", true, "统一工期承诺后完整恢复")
		return observed, true, true, nil
	})
}

func strPtr(s string) *string { return &s }

func (a *app) gateG10(ctx context.Context, f *flow) {
	a.runGate(f, "G10", "conflict_candidate", func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, err := f.newTask(ctx, "G10", "E2E 门禁场景：乐观锁冲突")
		if err != nil {
			return "", false, false, err
		}
		code, title, desc := "R1", "章节基线要求", "投标人须承诺按要求提交实施方案。"
		req, err := a.d.CreateRequirement(ctx, demoTenant, t.ID, actor, db.RequirementInput{Code: &code,
			Category: strPtr(db.ReqNonScoringRequired), Title: &title, Description: &desc})
		if err != nil {
			return "", false, false, err
		}
		if _, err := a.d.ConfirmRequirements(ctx, demoTenant, t.ID, actor, 0); err != nil {
			return "", false, false, err
		}
		det, err := a.d.CreateOutline(ctx, demoTenant, t.ID, actor, db.OutlineInput{Sections: []db.SectionInput{
			{Code: "1", Title: "实施方案", Level: 1, RequirementIDs: []int64{req.ID}},
		}})
		if err != nil {
			return "", false, false, err
		}
		det, err = f.submitApprove(ctx, t.ID, det.Outline.Version)
		if err != nil {
			return "", false, false, err
		}
		sec := det.Sections[0]
		save := func(base int, body string) (*db.SaveRevisionResult, error) {
			return a.d.SaveSectionRevision(ctx, demoTenant, actor, db.SaveRevisionInput{TaskID: t.ID, SectionID: sec.ID, BaseRevNo: base,
				BasedOnOutlineVersion: det.Outline.Version, Source: db.RevHumanEdit, ContentMD: body, SummaryNote: "G10"})
		}
		first, err := save(0, "第一版正文，我方承诺按要求提交实施方案并明确责任人。")
		if err != nil || first.Outcome != db.SaveApplied {
			return "", false, false, fmt.Errorf("first save: %v %+v", err, first)
		}
		g := f.r.gates[len(f.r.gates)-1]
		second, err := save(0, "第二版正文，基于过期基准提交。")
		if err != nil {
			return "", false, false, err
		}
		blocked := second.Outcome == db.SaveConflict && second.Section.CurrentRevNo == first.Section.CurrentRevNo
		g.check("G10.trigger", db.SaveConflict, second.Outcome, blocked,
			fmt.Sprintf("current_rev_no=%d", second.Section.CurrentRevNo))
		third, err := save(first.Section.CurrentRevNo, "第三版正文，我方承诺按要求提交实施方案并明确责任人与交付安排。")
		if err != nil {
			return "", false, blocked, err
		}
		g.check("G10.remediated", db.SaveApplied, third.Outcome, third.Outcome == db.SaveApplied && third.Section.CurrentRevNo > first.Section.CurrentRevNo, "")
		return second.Outcome, blocked, third.Outcome == db.SaveApplied, nil
	})
}

func (a *app) gateG11(ctx context.Context, f *flow) {
	a.runGate(f, "G11", db.BlockNoApprovedOutline+" (correction supersedes)", func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, det, err := f.buildReady(ctx, "G11", false)
		if err != nil {
			return "", false, false, err
		}
		if _, err := f.ingestCorrection(ctx, t.ID); err != nil {
			return "", false, false, err
		}
		exres, err := a.d.ExtractRequirements(ctx, demoTenant, t.ID, actor)
		if err != nil {
			return "", false, false, err
		}
		view, _, err := f.requirements(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		g := f.r.gates[len(f.r.gates)-1]
		g.check("G11.superseded", strconv.Itoa(a.golden.Superseded), strconv.Itoa(exres.Superseded),
			exres.Superseded == a.golden.Superseded, "")
		var oldReq, newReq *db.RequirementItem
		for _, it := range view.Items {
			if !strings.Contains(it.Title, "2.3.2") && !strings.Contains(it.Description, "驻场服务人员须持有") {
				continue
			}
			if it.ReviewStatus == db.ReviewStale && it.SupersededByID != nil {
				oldReq = it
			} else if !it.IsSuperseded() {
				newReq = it
			}
		}
		if oldReq == nil || newReq == nil {
			return "", false, false, fmt.Errorf("old/new 2.3.2 requirement not identified (old=%v new=%v)", oldReq != nil, newReq != nil)
		}
		g.check("G11.stale_link", "old->new link", fmt.Sprintf("stale=%v link=%v", oldReq.ReviewStatus == db.ReviewStale,
			oldReq.SupersededByID != nil && *oldReq.SupersededByID == newReq.ID),
			oldReq.ReviewStatus == db.ReviewStale && oldReq.SupersededByID != nil && *oldReq.SupersededByID == newReq.ID, "")
		rd, err := f.readiness(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		observed := kindList(rd.Blockers)
		g.check("G11.trigger", db.BlockNoApprovedOutline, observed, kindsContain(rd.Blockers, db.BlockNoApprovedOutline), "")
		list, err := a.d.ListOutlines(ctx, demoTenant, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		var draft *db.ProposalOutline
		oldSuperseded := false
		for _, o := range list.Outlines {
			if o.Version == det.Outline.Version && o.Status == db.OutlineSuperseded {
				oldSuperseded = true
			}
			if o.Status == db.OutlineDraft && (draft == nil || o.Version > draft.Version) {
				draft = o
			}
		}
		g.check("G11.old_outline", "superseded", strconv.FormatBool(oldSuperseded), oldSuperseded, "")
		if draft == nil {
			return observed, true, false, errors.New("no auto-derived draft outline")
		}
		detail, err := a.d.GetOutline(ctx, demoTenant, t.ID, draft.Version)
		if err != nil {
			return observed, true, false, err
		}
		mapsOld, mapsNew := false, false
		for _, s := range detail.Sections {
			for _, rid := range s.RequirementIDs {
				if rid == oldReq.ID {
					mapsOld = true
				}
				if rid == newReq.ID {
					mapsNew = true
				}
			}
		}
		g.check("G11.auto_v2", "maps new, not old", fmt.Sprintf("new=%v old=%v", mapsNew, mapsOld), mapsNew && !mapsOld, "")
		if _, err := a.d.ConfirmRequirements(ctx, demoTenant, t.ID, actor, 0); err != nil {
			return observed, true, false, err
		}
		m, err := a.d.RequirementManifest(ctx, demoTenant, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G11.confirmed", db.ManifestConfirmed, m.Status, m.Status == db.ManifestConfirmed, "")
		approved, err := f.submitApprove(ctx, t.ID, draft.Version)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G11.approved", "approved", approved.Outline.Status, approved.Outline.Status == db.OutlineApproved,
			"直接使用自动派生 v2，未再次调用 CreateOutline 或 UpdateOutline")
		written, err := f.writeAll(ctx, t.ID, approved)
		if err != nil {
			return observed, true, false, err
		}
		approvedCount, err := f.approveAll(ctx, t.ID, approved)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G11.sections", fmt.Sprintf("%d/%d", len(approved.Sections), len(approved.Sections)),
			fmt.Sprintf("written=%d approved=%d", len(written), approvedCount),
			len(written) == len(approved.Sections) && approvedCount == len(approved.Sections), "新大纲全部章节重新写回并批准")
		mres, err := f.matrix(ctx, t.ID, 0)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G11.matrix", fmt.Sprintf("%d/%d", mres.Checked, mres.Checked), fmt.Sprintf("verified=%d failed=%d pending=%d", mres.Verified, mres.Failed, mres.Pending),
			mres.Verified == mres.Checked && mres.Failed == 0 && mres.Pending == 0, failedRowText(mres.Rows))
		cres, err := f.consistency(ctx, t.ID)
		if err != nil {
			return observed, true, false, err
		}
		g.check("G11.consistency", "0 findings", fmt.Sprintf("findings=%d current=%v", len(cres.Findings), cres.Current),
			len(cres.Findings) == 0 && cres.Current, "")
		if _, err := f.assertReady(ctx, t.ID); err != nil {
			return observed, true, false, err
		}
		g.check("G11.full_ready", "true", "true", true, "更正覆盖后完整恢复")
		return observed, true, true, nil
	})
}

func (a *app) gateG12(ctx context.Context, f *flow) {
	a.runGate(f, "G12", "unknown_evidence", func(f *flow) (string, bool, bool, error) {
		_ = ctx
		sc := &bidtools.SectionContext{TaskID: 1, SectionID: 1, Code: "1", Title: "示例章节",
			UsableEvidence: []bidtools.ContextEvidence{{ID: 7, Status: "verified", Relation: "supports", Usable: true}}}
		draft, err := bidtools.ParseSectionDraft(`{"content_md":"示例正文","summary_note":"摘要","evidence_ids":[7,999999],"gaps":[]}`, sc)
		if err != nil {
			return "", false, false, err
		}
		g := f.r.gates[len(f.r.gates)-1]
		g.check("G12.known_kept", "7", intsText(draft.EvidenceIDs), len(draft.EvidenceIDs) == 1 && draft.EvidenceIDs[0] == 7, "")
		gaps := 0
		for _, gap := range draft.Gaps {
			if gap.Kind == "unknown_evidence" {
				gaps++
			}
		}
		g.check("G12.trigger", "1 unknown_evidence gap", strconv.Itoa(gaps), gaps == 1, "")
		clean, err := bidtools.ParseSectionDraft(`{"content_md":"示例正文","summary_note":"摘要","evidence_ids":[7],"gaps":[]}`, sc)
		if err != nil {
			return "unknown_evidence", gaps > 0, false, err
		}
		unknown := 0
		for _, gap := range clean.Gaps {
			if gap.Kind == "unknown_evidence" {
				unknown++
			}
		}
		g.check("G12.remediated", "0 unknown_evidence", strconv.Itoa(unknown), unknown == 0, "")
		return "unknown_evidence", gaps > 0, unknown == 0, nil
	})
}

func (a *app) gateG13(ctx context.Context, f *flow) {
	a.runGate(f, "G13", "ExportBlockedError", func(f *flow) (string, bool, bool, error) {
		ctx, cancel := a.gateTimeout(ctx)
		defer cancel()
		t, _, err := f.buildReady(ctx, "G13", true)
		if err != nil {
			return "", false, false, err
		}
		view, _, err := f.requirements(ctx, t.ID)
		if err != nil {
			return "", false, false, err
		}
		var veto *db.RequirementItem
		for _, it := range view.Items {
			if it.Category == db.ReqVetoItem && !it.OutOfBid && !it.IsSuperseded() {
				veto = it
				break
			}
		}
		if veto == nil {
			return "", false, false, errors.New("no veto item available")
		}
		if _, err := a.d.SetVetoStatus(ctx, demoTenant, t.ID, veto.ID, actor, db.VetoPending, ""); err != nil {
			return "", false, false, err
		}
		jobs, pre, err := a.d.CreateExportJobs(ctx, demoTenant, t.ID, actor, db.CreateExportInput{Formats: []string{db.ExportDOCX},
			DefaultTemplateHash: export.DefaultTemplateHash(), Bidder: demoBidder})
		var blockedErr *db.ExportBlockedError
		isBlocked := errors.As(err, &blockedErr)
		existing, _ := a.d.ListExportJobs(ctx, demoTenant, t.ID, 10)
		g := f.r.gates[len(f.r.gates)-1]
		observed := "nil"
		if err != nil {
			observed = err.Error()
		}
		if isBlocked && blockedErr.Preflight != nil {
			observed = "ExportBlockedError: " + kindList(blockedErr.Preflight.Readiness.Blockers)
			g.check("G13.veto_pending", db.BlockVetoPending, kindList(blockedErr.Preflight.Readiness.Blockers),
				kindsContain(blockedErr.Preflight.Readiness.Blockers, db.BlockVetoPending), "")
		}
		g.check("G13.trigger", "ExportBlockedError", observed, isBlocked, preflightText(pre))
		g.check("G13.no_job", "0", strconv.Itoa(len(jobs)+len(existing)), len(jobs) == 0 && len(existing) == 0, "")
		if _, err := a.d.SetVetoStatus(ctx, demoTenant, t.ID, veto.ID, actor, db.VetoPassed, "E2E 虚构演示：修正后已核对通过"); err != nil {
			return observed, isBlocked, false, err
		}
		jobs, pre, err = a.d.CreateExportJobs(ctx, demoTenant, t.ID, actor, db.CreateExportInput{Formats: []string{db.ExportDOCX},
			DefaultTemplateHash: export.DefaultTemplateHash(), Bidder: demoBidder})
		if err != nil {
			return observed, isBlocked, false, err
		}
		g.check("G13.remediated", "1 queued", fmt.Sprintf("jobs=%d status=%s", len(jobs), jobStatus(jobs)),
			len(jobs) == 1 && jobs[0].Status == db.ExportQueued && pre != nil && pre.Readiness.Ready, preflightText(pre))
		return observed, isBlocked, len(jobs) == 1, nil
	})
}

func jobStatus(jobs []*db.ExportJob) string {
	if len(jobs) == 0 {
		return ""
	}
	return jobs[0].Status
}

func boolText(v bool) string {
	if v {
		return "match"
	}
	return "mismatch"
}

func findSection(det *db.OutlineDetail, code string) *db.ProposalSection {
	for _, s := range det.Sections {
		if s.Code == code {
			return s
		}
	}
	return nil
}

// runGate 执行一个门禁场景并记录结果；意外错误只影响该门禁。
func (a *app) runGate(f *flow, id, name string, fn func(f *flow) (observed string, blocked, remediated bool, err error)) *Gate {
	g := f.r.gate(id, name)
	observed, blocked, remediated, err := fn(f)
	g.Observed, g.Blocked, g.Remediated = observed, blocked, remediated
	for _, c := range g.Checks {
		if strings.HasSuffix(c.ID, ".full_ready") && c.Status == statusPass {
			g.FullReady = true
		}
	}
	if err != nil {
		g.fail(id+".exec", "gate scenario completes", err)
	}
	return g
}
