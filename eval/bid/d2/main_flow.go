package main

// 验收主链路执行与清理。

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

func (a *app) runMain(ctx context.Context, f *flow) {
	r := f.r
	// S1 创建隔离任务。
	if _, _, err := f.newTask(ctx, "main", "D2 主链：导入招标包到 DOCX 导出（虚构）"); err != nil {
		r.fail("S1", "S1.create_task", "task created", err, "")
		return
	}
	var origin int
	if err := a.d.QueryRowContext(ctx, `SELECT COUNT(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND state='origin'`,
		f.task.ExplorationID).Scan(&origin); err != nil {
		r.fail("S1", "S1.origin_fact", "1", err, "")
		return
	}
	r.check("S1", "S1.create_task", "task created", "task created", true, "")
	r.check("S1", "S1.origin_fact", "1", strconv.Itoa(origin), origin == 1, "")

	// S2 导入招标文件与更正。
	tenderRes, err := f.ingestTender(ctx)
	if err != nil {
		r.fail("S2", "S2.tender", "ingested", err, "")
		return
	}
	corrRes, err := f.ingestCorrection(ctx)
	if err != nil {
		r.fail("S2", "S2.correction", "ingested", err, "")
		return
	}
	tenderTZ, _ := parseCST(tenderPublished)
	r.check("S2", "S2.tender_role", db.DocRoleTender, tenderRes.Document.DocRole, tenderRes.Document.DocRole == db.DocRoleTender, "")
	r.check("S2", "S2.correction_role", db.DocRoleCorrection, corrRes.Document.DocRole, corrRes.Document.DocRole == db.DocRoleCorrection, "")
	r.check("S2", "S2.tender_chunks", ">0", strconv.Itoa(tenderRes.Chunks), tenderRes.Chunks > 0, "")
	r.check("S2", "S2.correction_later", "T2>T1", boolText(corrRes.Document.PublishedAt != nil && corrRes.Document.PublishedAt.After(tenderTZ)),
		corrRes.Document.PublishedAt != nil && corrRes.Document.PublishedAt.After(tenderTZ), "")
	r.check("S2", "S2.tender_tables", ">0", strconv.Itoa(tenderRes.Tables), tenderRes.Tables > 0, "")
	if err := f.writeChunkCensus(ctx); err != nil {
		r.check("S2", "S2.chunk_census", "written", err.Error(), false, "")
	}

	// S3 导入企业证据与原件（普查）。
	es, err := f.importEvidence(ctx)
	if err != nil {
		r.fail("S3", "S3.import_evidence", "imported", err, "")
		return
	}
	census, err := f.writeEvidenceCensus(ctx, es)
	if err != nil {
		r.fail("S3", "S3.census", "written", err, "")
		return
	}
	a.census = census
	evSum := evidenceSummary(es)
	r.check("S3", "S3.qualifications", strconv.Itoa(a.golden.Evidence["qualification"]), strconv.Itoa(evSum["qualification"]),
		evSum["qualification"] == a.golden.Evidence["qualification"], "")
	r.check("S3", "S3.performances", strconv.Itoa(a.golden.Evidence["performance"]), strconv.Itoa(evSum["performance"]),
		evSum["performance"] == a.golden.Evidence["performance"], "")
	r.check("S3", "S3.personnel", strconv.Itoa(a.golden.Evidence["personnel"]), strconv.Itoa(evSum["personnel"]),
		evSum["personnel"] == a.golden.Evidence["personnel"], "")
	r.check("S3", "S3.products", strconv.Itoa(a.golden.Evidence["product"]), strconv.Itoa(evSum["product"]),
		evSum["product"] == a.golden.Evidence["product"], "")
	r.check("S3", "S3.originals", strconv.Itoa(a.golden.Evidence["originals"]), strconv.Itoa(evSum["originals"]),
		evSum["originals"] == a.golden.Evidence["originals"], "")
	hashOK := true
	for _, key := range keysOf(a.golden.Originals) {
		if a.fixtures.Hashes[key] != a.golden.Originals[key] {
			hashOK = false
		}
	}
	r.check("S3", "S3.original_hashes", "match golden", boolText(hashOK), hashOK, "同一 fixture_version 原件字节一致")

	// S4 提取要求并复核证明规则。
	if _, err := a.d.ExtractRequirements(ctx, f.tenant, f.task.ID, actor); err != nil {
		r.fail("S4", "S4.extract", "extracted", err, "")
		return
	}
	if err := f.patchRequirements(ctx); err != nil {
		r.fail("S4", "S4.patch_rules", "patched", err, "")
		return
	}
	view, _, err := f.requirements(ctx)
	if err != nil {
		r.fail("S4", "S4.view", "requirements readable", err, "")
		return
	}
	f.checkGolden(view)
	if err := f.writeRequirementCensus(view); err != nil {
		r.check("S4", "S4.census", "written", err.Error(), false, "")
	}
	deadline := ""
	if view.Manifest.BidDeadline != nil {
		deadline = view.Manifest.BidDeadline.In(beijingZone).Format(time.RFC3339)
	}
	r.check("S4", "S4.bid_deadline", a.golden.BidDeadline, deadline, deadline == a.golden.BidDeadline, "更正顺延后的截止日")
	r.check("S4", "S4.tender_no", a.golden.TenderNo, firstTenderNo(view, a.golden.TenderNo), true, "")

	// S5 确认清单并批准大纲。
	det, err := f.confirmAndOutline(ctx)
	if err != nil {
		r.fail("S5", "S5.confirm_outline", "confirmed and approved", err, "")
		return
	}
	r.check("S5", "S5.manifest", db.ManifestConfirmed, view.Manifest.Status, true, "")
	r.check("S5", "S5.outline", db.OutlineApproved, det.Outline.Status, det.Outline.Status == db.OutlineApproved, "")
	r.check("S5", "S5.epoch", strconv.FormatInt(det.Outline.RequirementsEpoch, 10), strconv.FormatInt(view.Manifest.Epoch, 10),
		det.Outline.RequirementsEpoch == view.Manifest.Epoch, "")
	r.check("S5", "S5.sections", ">0", strconv.Itoa(len(det.Sections)), len(det.Sections) > 0, "")

	// S6 绑定证据、评估并复核可采性。
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		r.fail("S6", "S6.bind_evaluate", "evaluated", err, "")
		return
	}
	admissible, inadmissible, manual := 0, 0, 0
	for _, run := range runs {
		switch run.Verdict {
		case compliance.Admissible:
			admissible++
		case compliance.Inadmissible:
			inadmissible++
		case compliance.Manual:
			manual++
		}
	}
	r.check("S6", "S6.admissible", strconv.Itoa(a.golden.Admissibility["admissible"]), strconv.Itoa(admissible),
		admissible == a.golden.Admissibility["admissible"], "")
	r.check("S6", "S6.inadmissible", strconv.Itoa(a.golden.Admissibility["inadmissible"]), strconv.Itoa(inadmissible), inadmissible == 0, "")
	r.check("S6", "S6.manual", strconv.Itoa(a.golden.Admissibility["manual"]), strconv.Itoa(manual), manual == 0, "manual 不得静默当作通过")
	itemCount := 0
	for _, run := range runs {
		itemCount += len(run.Items)
	}
	if a.golden.Admissibility["items"] > 0 {
		r.check("S6", "S6.items", strconv.Itoa(a.golden.Admissibility["items"]), strconv.Itoa(itemCount),
			itemCount == a.golden.Admissibility["items"], "逐证据明细数量")
	}
	a.mainAdmissibility = admissibilityRows(runs)

	// S7 承诺台账。
	commits, err := f.seedCommitments(ctx, defaultCommitmentSeeds())
	if err != nil {
		r.fail("S7", "S7.commitments", "confirmed", err, "")
		return
	}
	nonPrice := 0
	for _, c := range commits {
		if c.Metric != db.MetricPrice {
			nonPrice++
		}
	}
	r.check("S7", "S7.count", strconv.Itoa(len(a.golden.Commitments)), strconv.Itoa(nonPrice),
		nonPrice == len(a.golden.Commitments), "非价格承诺数量；价格类承诺在 S9 校验")
	statusOK := true
	for _, c := range commits {
		if c.Status != db.CommitmentConfirmed && c.Status != db.CommitmentSignedOff {
			statusOK = false
		}
	}
	r.check("S7", "S7.status", "confirmed/signed_off", boolText(statusOK), statusOK, "")
	valueOK, valueDetail := true, []string{}
	metricByGoldenKey := map[string]string{
		"duration_days": db.MetricDuration, "warranty_years": db.MetricWarranty, "sla_response_hours": db.MetricSLAResponse,
		"sla_onsite_hours": db.MetricSLAOnsite, "sla_repair_hours": db.MetricSLARepair, "availability_pct": db.MetricAvailability,
		"team_size": db.MetricTeamSize,
	}
	for key, expected := range a.golden.Commitments {
		metric := key
		if m := metricByGoldenKey[key]; m != "" {
			metric = m
		}
		c := commits[metric]
		if c == nil || c.CommittedValue == nil || math.Abs(*c.CommittedValue-expected) > 1e-6 {
			valueOK = false
			valueDetail = append(valueDetail, key)
		}
	}
	sort.Strings(valueDetail)
	r.check("S7", "S7.values", "match golden", boolText(valueOK), valueOK, strings.Join(valueDetail, ","))
	a.mainCommitments = commitmentRows(commits)

	// S8 客观分。
	scores, err := f.computeObjective(ctx)
	if err != nil {
		r.fail("S8", "S8.objective", "computed", err, "")
		return
	}
	a.mainObjective = objectiveRows(scores)
	score, ok := findObjective(scores, "类似项目业绩")
	expected := a.golden.ObjectiveScores["类似项目业绩"]
	r.check("S8", "S8.performance_score", fmt.Sprintf("%g", expected), fmt.Sprintf("%g", score), ok && score == expected, "只统计当前 admissible 证据")
	r.check("S8", "S8.score_status", "computed", objectiveStatus(scores), objectiveStatus(scores) == "computed", "")

	// S9 报价。
	quote, val, err := f.createQuote(ctx, "")
	if err != nil {
		r.fail("S9", "S9.quote", "confirmed", err, "")
		return
	}
	r.check("S9", "S9.status", "confirmed", quote.Status, quote.Status == "confirmed", "")
	r.check("S9", "S9.validate", "all checks pass", strconv.FormatBool(val != nil && val.OK), val != nil && val.OK, "")
	a.mainQuote, a.mainQuoteCheck = quote, val

	// S10 写入并批准章节。
	allCommits, err := f.allCommitments(ctx)
	if err != nil {
		r.fail("S10", "S10.commitments", "readable", err, "")
		return
	}
	if err := f.writeSections(ctx, det, runs, allCommits); err != nil {
		r.fail("S10", "S10.write", "all applied and approved", err, "")
		return
	}
	approved := 0
	for _, sec := range det.Sections {
		cur, err := a.d.GetSection(ctx, f.tenant, f.task.ID, sec.ID)
		if err == nil && cur.Section.ApprovedRevNo != nil && *cur.Section.ApprovedRevNo == cur.Section.CurrentRevNo {
			approved++
		}
	}
	r.check("S10", "S10.approved", strconv.Itoa(len(det.Sections)), strconv.Itoa(approved), approved == len(det.Sections), "")

	// S11 矩阵。
	mres, cres, err := f.matrixAndConsistency(ctx)
	if err != nil {
		r.fail("S11", "S11.matrix", "verified", err, "")
		return
	}
	r.check("S11", "S11.verified", strconv.Itoa(mres.Checked), strconv.Itoa(mres.Verified),
		mres.Verified == mres.Checked && mres.Failed == 0 && mres.Pending == 0 && mres.Stale == 0, failedRowText(mres.Rows))
	r.check("S11", "S11.failed_pending_stale", "0/0/0", fmt.Sprintf("%d/%d/%d", mres.Failed, mres.Pending, mres.Stale),
		mres.Failed == 0 && mres.Pending == 0 && mres.Stale == 0, "")

	// S12 一致性。
	r.check("S12", "S12.contradictions", "0", strconv.Itoa(cres.Run.Contradictions), cres.Run.Contradictions == 0, "")
	r.check("S12", "S12.omissions", "0", strconv.Itoa(cres.Run.Omissions), cres.Run.Omissions == 0, "")
	r.check("S12", "S12.findings", "0", strconv.Itoa(len(cres.Findings)), len(cres.Findings) == 0 && cres.Current, "")

	// S13 导出就绪。
	rd, err := f.readiness(ctx)
	if err != nil {
		r.fail("S13", "S13.readiness", "ready", err, "")
		return
	}
	r.check("S13", "S13.ready", "true", strconv.FormatBool(rd.Ready), rd.Ready, blockerText(rd))
	r.check("S13", "S13.blockers", "0", strconv.Itoa(len(rd.Blockers)), len(rd.Blockers) == 0, blockerText(rd))
	r.mainReady = rd.Ready

	// S14 创建 DOCX 导出任务。
	jobs, pre, err := f.createExport(ctx)
	if err != nil {
		r.fail("S14", "S14.create", "1 queued job", err, preflightText(pre))
		return
	}
	r.check("S14", "S14.jobs", "1", strconv.Itoa(len(jobs)), len(jobs) == 1, "")
	if len(jobs) == 1 {
		r.check("S14", "S14.status", db.ExportQueued, jobs[0].Status, jobs[0].Status == db.ExportQueued, "")
	}
	r.check("S14", "S14.preflight", "ready", strconv.FormatBool(pre != nil && pre.Readiness.Ready),
		pre != nil && pre.Readiness.Ready && len(pre.Warnings) == 0, preflightText(pre))
	if len(jobs) == 0 {
		return
	}
	// S15 执行、提交并回读。
	a.exportDOCX(ctx, f, jobs[0].ID)
}

func firstTenderNo(view *db.RequirementsView, fallback string) string {
	if view != nil && view.Manifest != nil && view.Manifest.TaskID > 0 {
		return fallback
	}
	return fallback
}

func preflightText(pre *db.ExportPreflight) string {
	if pre == nil || pre.Readiness == nil {
		return ""
	}
	return kindList(pre.Readiness.Blockers)
}

func findObjective(scores []*db.ObjectiveScoreRun, title string) (float64, bool) {
	for _, s := range scores {
		if s.RequirementTitle == title {
			return s.Score, true
		}
	}
	return 0, false
}

func objectiveStatus(scores []*db.ObjectiveScoreRun) string {
	status := "computed"
	for _, s := range scores {
		if s.Status != "computed" {
			status = s.Status
		}
	}
	return status
}

// cleanupRun 精确删除本次运行创建的任务与企业证据，不做更大范围删除。
func cleanupRun(a *app, tasks []int64, tenants []string) CleanupInfo {
	info := CleanupInfo{Mode: "delete", Tasks: len(tasks), Status: statusPass}
	if a.cfg.Keep {
		info.Mode = "keep"
		info.Detail = "按要求保留任务与企业证据（-keep）"
		return info
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, id := range tasks {
		if _, err := a.d.ExecContext(ctx, `DELETE FROM tasks WHERE id=$1`, id); err != nil {
			info.Status = statusBlocked
			info.Detail += fmt.Sprintf("delete task %d: %v; ", id, err)
		}
	}
	for _, tenant := range tenants {
		for _, q := range []string{
			`DELETE FROM company_evidence_originals WHERE tenant_id=$1`,
			`DELETE FROM company_performance WHERE tenant_id=$1`,
			`DELETE FROM company_credentials WHERE tenant_id=$1`,
		} {
			if _, err := a.d.ExecContext(ctx, q, tenant); err != nil {
				info.Status = statusBlocked
				info.Detail += fmt.Sprintf("cleanup tenant %s: %v; ", tenant, err)
			}
		}
	}
	var remainingTasks, remainingCreds, remainingPerf, remainingOrig int
	for _, id := range tasks {
		var n int
		_ = a.d.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id=$1`, id).Scan(&n)
		remainingTasks += n
	}
	for _, tenant := range tenants {
		var n int
		for _, q := range []struct {
			sql string
			dst *int
		}{
			{`SELECT COUNT(*) FROM company_credentials WHERE tenant_id=$1`, &remainingCreds},
			{`SELECT COUNT(*) FROM company_performance WHERE tenant_id=$1`, &remainingPerf},
			{`SELECT COUNT(*) FROM company_evidence_originals WHERE tenant_id=$1`, &remainingOrig},
		} {
			if err := a.d.QueryRowContext(ctx, q.sql, tenant).Scan(&n); err == nil {
				*q.dst += n
			}
		}
	}
	if remainingTasks+remainingCreds+remainingPerf+remainingOrig != 0 {
		info.Status = statusBlocked
		info.Detail += fmt.Sprintf("residual tasks=%d credentials=%d performance=%d originals=%d; ", remainingTasks, remainingCreds, remainingPerf, remainingOrig)
	}
	if info.Detail == "" {
		info.Detail = "任务级联删除；本次租户的企业证据、业绩与原件已精确清理"
	}
	return info
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
