package server

// Phase 6A HTTP 集成测试（需要 AUTOBID_TEST_DATABASE_URL）：响应矩阵核验 / 查询 / 覆盖率、全文一致性核验与结论处理（乐观锁）、
// 导出就绪度阻断原因，以及参数错误 400、不存在 404、冲突 / 失效 409、无批准大纲 422。

import (
	"fmt"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

func TestHTTPComplianceAndConsistency(t *testing.T) {
	c := newProposalAPI(t, nil)
	var task struct {
		ID int64 `json:"id"`
	}
	if code := c.raw("POST", "/api/tasks", map[string]any{"name": "phase6a", "goal": "技术标审查"}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	base := fmt.Sprintf("/api/tasks/%d", task.ID)
	var e apiErr

	// 无批准大纲：核验与一致性核验 422；就绪度列出原因；结论查询返回空。
	if code := c.raw("POST", base+"/compliance/verify", nil, &e); code != 422 || e.Code != "outline_not_approved" {
		t.Fatalf("verify without outline: %d %+v", code, e)
	}
	if code := c.raw("POST", base+"/consistency/runs", nil, &e); code != 422 {
		t.Fatalf("consistency without outline: %d", code)
	}
	var ready db.ExportReadiness
	if code := c.raw("GET", base+"/export-readiness", nil, &ready); code != 200 || ready.Ready || ready.Counts[db.BlockNoApprovedOutline] != 1 {
		t.Fatalf("readiness without outline: %d %+v", code, ready)
	}
	var empty db.ConsistencyRunDetail
	if code := c.raw("GET", base+"/consistency/findings", nil, &empty); code != 200 || empty.Run != nil {
		t.Fatalf("findings without runs: %d %+v", code, empty)
	}

	reqs := map[string]int64{}
	for _, r := range []map[string]any{
		{"code": "★1", "category": "mandatory_clause", "title": "★ 总工期", "description": "★ 投标人须承诺总工期不超过120日历天。", "is_mandatory": true},
		{"code": "★2", "category": "mandatory_clause", "title": "★ 质保期", "description": "★ 投标人须承诺免费质保期不少于3年。", "is_mandatory": true},
	} {
		var it db.RequirementItem
		if code := c.raw("POST", base+"/requirements", r, &it); code != 201 {
			t.Fatalf("create requirement: %d", code)
		}
		reqs[it.Code] = it.ID
	}
	if code := c.raw("POST", base+"/requirements/confirm", map[string]any{}, nil); code != 200 {
		t.Fatalf("confirm: %d", code)
	}
	var outline db.OutlineDetail
	if code := c.raw("POST", base+"/outlines", map[string]any{"sections": []map[string]any{
		{"section_code": "1", "title": "总体方案", "level": 1, "requirement_item_ids": []int64{reqs["★1"]}},
		{"section_code": "2", "title": "质量保证", "level": 1, "requirement_item_ids": []int64{reqs["★2"]}},
		{"section_code": "3", "title": "进度计划", "level": 1},
	}}, &outline); code != 201 {
		t.Fatalf("create outline: %d", code)
	}
	c.raw("POST", base+"/outlines/1/submit", nil, nil)
	if code := c.raw("POST", base+"/outlines/1/approve", nil, &outline); code != 200 {
		t.Fatalf("approve outline: %d", code)
	}
	ids := codesOf(&outline)
	save := func(code string, baseRev int, body string) {
		t.Helper()
		if st := c.raw("POST", fmt.Sprintf("%s/sections/%d/revisions", base, ids[code]),
			map[string]any{"base_rev_no": baseRev, "based_on_outline_version": 1, "content_md": body}, nil); st != 201 {
			t.Fatalf("save %s: %d", code, st)
		}
	}
	save("1", 0, "我方承诺总工期 90 天，严格按计划交付。")
	save("2", 0, "## 质量保证\n\n★ 质保期")
	save("3", 0, "实施进度：工期 180 日，分三阶段。")

	// 参数校验。
	if code := c.raw("GET", base+"/compliance/matrix?status=bogus", nil, &e); code != 400 {
		t.Fatalf("bad status: %d", code)
	}
	if code := c.raw("GET", base+"/compliance/matrix?kind=bogus", nil, &e); code != 400 {
		t.Fatalf("bad kind: %d", code)
	}
	if code := c.raw("POST", base+"/compliance/verify", map[string]any{"section_id": 1 << 50}, &e); code != 404 {
		t.Fatalf("verify unknown section: %d", code)
	}
	if code := c.raw("POST", base+"/compliance/verify", map[string]any{"bogus": 1}, &e); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}

	// 第二层核验与查询。
	var vr db.VerifyMatrixResult
	if code := c.raw("POST", base+"/compliance/verify", map[string]any{}, &vr); code != 200 || vr.Checked != 2 || vr.Verified != 1 || vr.Failed != 1 {
		t.Fatalf("verify: %d %+v", code, vr)
	}
	var mv db.MatrixView
	if code := c.raw("GET", base+"/compliance/matrix?status=failed&kind=star", nil, &mv); code != 200 || len(mv.Rows) != 1 ||
		mv.Rows[0].RequirementID != reqs["★2"] || !mv.Approved || mv.Rows[0].SectionRevID == nil {
		t.Fatalf("matrix filter: %d %+v", code, mv)
	}
	if code := c.raw("GET", fmt.Sprintf("%s/compliance/matrix?section_id=%d", base, ids["1"]), nil, &mv); code != 200 || len(mv.Rows) != 1 ||
		mv.Rows[0].VerifyStatus != compliance.StatusVerified {
		t.Fatalf("matrix by section: %d %+v", code, mv)
	}
	var cov db.ComplianceCoverage
	if code := c.raw("GET", base+"/compliance/coverage", nil, &cov); code != 200 || cov.Total != 2 || cov.Mapped != 2 || cov.Verified != 1 || cov.MappingRate != 1 {
		t.Fatalf("coverage: %d %+v", code, cov)
	}

	// 一致性核验：90 天 vs 180 日矛盾 + ★2 漏项。
	var run db.ConsistencyRunDetail
	if code := c.raw("POST", base+"/consistency/runs", nil, &run); code != 201 || run.Run.Contradictions != 1 || run.Run.Omissions != 1 || !run.Current {
		t.Fatalf("consistency: %d %+v", code, run.Run)
	}
	var open db.ConsistencyRunDetail
	if code := c.raw("GET", base+"/consistency/findings?status=open", nil, &open); code != 200 || len(open.Findings) != 2 {
		t.Fatalf("open findings: %d %+v", code, open)
	}
	var runs struct {
		Runs []db.ConsistencyRun `json:"runs"`
	}
	if code := c.raw("GET", base+"/consistency/runs", nil, &runs); code != 200 || len(runs.Runs) != 1 {
		t.Fatalf("runs: %d %+v", code, runs)
	}
	if code := c.raw("GET", base+"/consistency/findings?run_id=999999999", nil, &e); code != 404 {
		t.Fatalf("unknown run: %d", code)
	}
	var contra *db.ConsistencyFinding
	for _, f := range open.Findings {
		if f.Kind == db.FindingContradiction {
			contra = f
		}
	}
	if contra == nil || contra.ASectionCode != "1" || contra.BSectionCode != "3" {
		t.Fatalf("contradiction sides: %+v", open.Findings)
	}
	resolve := fmt.Sprintf("%s/consistency/findings/%d/resolve", base, contra.ID)
	if code := c.raw("POST", resolve, map[string]any{"version": contra.Version, "resolution_kind": "bogus"}, &e); code != 400 || e.Code != "invalid_finding" {
		t.Fatalf("bad resolution: %d %+v", code, e)
	}
	var resolved db.ConsistencyFinding
	if code := c.raw("POST", resolve, map[string]any{"version": contra.Version, "resolution_kind": "accepted", "note": "分期工期"}, &resolved); code != 200 ||
		resolved.Status != db.FindingResolved || resolved.ResolvedBy != "http-reviewer" {
		t.Fatalf("resolve: %d %+v", code, resolved)
	}
	var conflict struct {
		Code    string                 `json:"code"`
		Finding *db.ConsistencyFinding `json:"finding"`
	}
	if code := c.raw("POST", resolve, map[string]any{"version": contra.Version, "resolution_kind": "fixed"}, &conflict); code != 409 ||
		conflict.Code != "finding_conflict" || conflict.Finding == nil || conflict.Finding.Version != resolved.Version {
		t.Fatalf("stale version: %d %+v", code, conflict)
	}
	if code := c.raw("POST", fmt.Sprintf("%s/consistency/findings/%d/resolve", base, 1<<50), map[string]any{"version": 1, "resolution_kind": "fixed"}, &e); code != 404 {
		t.Fatalf("unknown finding: %d", code)
	}

	// 章节新修订：矩阵行 stale、包含该章节的结论 stale（处理返回 409 finding_stale）、导出就绪度给出阻断原因。
	var omission *db.ConsistencyFinding
	for _, f := range open.Findings {
		if f.Kind == db.FindingOmission {
			omission = f
		}
	}
	save("2", 1, "我方承诺免费质保期 5 年。")
	if code := c.raw("GET", base+"/compliance/matrix?status=stale", nil, &mv); code != 200 || len(mv.Rows) != 1 || mv.Rows[0].RequirementID != reqs["★2"] {
		t.Fatalf("stale rows: %d %+v", code, mv)
	}
	if code := c.raw("POST", fmt.Sprintf("%s/consistency/findings/%d/resolve", base, omission.ID),
		map[string]any{"version": omission.Version, "resolution_kind": "fixed"}, &e); code != 409 || e.Code != "finding_stale" {
		t.Fatalf("resolve stale finding: %d %+v", code, e)
	}
	if code := c.raw("GET", base+"/export-readiness", nil, &ready); code != 200 || ready.Ready || ready.Counts[db.BlockMatrixStale] != 1 ||
		ready.Counts[db.BlockConsistencyStale] != 1 || ready.Counts[db.BlockSectionUnapproved] != 3 {
		t.Fatalf("readiness: %d %+v", code, ready)
	}
	found := false
	for _, b := range ready.Blockers {
		found = found || (b.Kind == db.BlockMatrixStale && b.RequirementCode == "★2" && strings.Contains(b.Message, "revised"))
	}
	if !found {
		t.Fatalf("matrix stale blocker missing: %+v", ready.Blockers)
	}
}
