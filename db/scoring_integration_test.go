package db

// 评分表结构化提取集成测试（真实 PostgreSQL，需要 AUTOBID_TEST_DATABASE_URL）：分包评分表入库为带分组的评分项、
// 合计校验、价格评分项不进入技术标大纲与响应覆盖、分组修订使清单失效、旧提取项标记待复核。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scoringChunk 构造一个含评分表的原文分块：分块正文包含表格 Markdown，前缀文字携带分包标识。
func scoringChunk(t *testing.T, seq int, title, before string, tb *scoreTable) DocumentChunk {
	t.Helper()
	tb.Markdown = "| " + strings.Join(tb.Grid[0], " | ") + " |\n| 表格 " + title + " |"
	raw, err := json.Marshal([]*scoreTable{tb})
	if err != nil {
		t.Fatal(err)
	}
	return DocumentChunk{Seq: seq, Level: 2, SectionTitle: title, Content: title + "\n" + before + "\n" + tb.Markdown, Tables: raw}
}

func TestScoringTableExtractionScopesAndOutline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	lot := func(price, tech1, tech2, biz float64) *scoreTable {
		num := func(v float64) string { b, _ := json.Marshal(v); return string(b) }
		return buildScoreTable([][]string{
			{"序号", "评分因素及权重", "分值", "评分标准"},
			{"1", "投标报价（" + num(price) + "%）", num(price), "最低价为评标基准价"},
			{"2", "技术部分（" + num(tech1+tech2) + "%）", "总体方案" + num(tech1), "方案完整、架构合理"},
			{"2", "技术部分（" + num(tech1+tech2) + "%）", "运维保障" + num(tech2), "保障措施完善"},
			{"3", "商务部分（" + num(biz) + "%）", "类似业绩" + num(biz), "每个业绩得2分"},
		})
	}
	assess := buildScoreTable([][]string{{"序号", "考核项目", "考核内容", "扣分标准"}, {"1", "系统稳定性", "运行稳定", "宕机扣2分"}})
	chunks := []DocumentChunk{
		{Seq: 0, Level: 1, SectionCode: "第一章", SectionTitle: "投标人须知", Content: "★1.1 投标人须具备一级资质，否则投标无效。", IsMandatory: true},
		{Seq: 1, Level: 1, SectionCode: "第三章", SectionTitle: "评分办法", Content: "评分办法：采用综合评分法，满分100分。"},
		scoringChunk(t, 2, "包1评审因素", "（一）包1评审因素", lot(30, 30, 10, 30)),
		scoringChunk(t, 3, "包2评审因素", "（二）包2评审因素", lot(20, 40, 20, 20)),
		scoringChunk(t, 4, "考核办法", "履约考核评分表", assess),
	}
	if _, _, err := f.d.InsertTenderDocument(ctx, &TenderDocument{TaskID: f.task.ID, TenderNo: uniq("ZB-SC"), Title: "两包评分表", RawText: uniq("raw")}, chunks); err != nil {
		t.Fatal(err)
	}
	res, err := f.d.ExtractRequirements(ctx, DefaultTenant, f.task.ID, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if res.ScoringItems != 8 || res.Orphaned != 0 {
		t.Fatalf("extract = %+v", res)
	}
	v, err := f.d.ListRequirements(ctx, DefaultTenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]int{}
	var techID int64
	for _, it := range v.Items {
		if it.Category != ReqScoringItem {
			continue
		}
		groups[it.ScoreGroup]++
		if !strings.HasPrefix(it.Title, "【包1】") && !strings.HasPrefix(it.Title, "【包2】") {
			t.Fatalf("lot label missing: %q", it.Title)
		}
		if it.Title == "【包1】总体方案" {
			techID = it.ID
			if it.ScoreWeight != 30 || !strings.Contains(it.Description, "方案完整") || !strings.Contains(it.Description, "技术部分") {
				t.Fatalf("scoring item detail: %+v", it)
			}
		}
		if strings.Contains(it.Title, "评分办法") || strings.Contains(it.Title, "系统稳定性") {
			t.Fatalf("scoring prose / assessment table extracted as scoring item: %q", it.Title)
		}
	}
	if groups[ScoreGroupPrice] != 2 || groups[ScoreGroupTechnical] != 4 || groups[ScoreGroupCommercial] != 2 || techID == 0 {
		t.Fatalf("score groups = %v", groups)
	}
	if len(v.Scoring) != 2 || v.Scoring[0].Lot != "1" || v.Scoring[0].Total != 100 || v.Scoring[1].Price != 20 || len(v.Scoring[0].Warnings) != 0 {
		t.Fatalf("scoring summary = %+v", v.Scoring)
	}
	if v.Stats.OutOfScope != 2 {
		t.Fatalf("price items must be out of technical scope: %+v", v.Stats)
	}

	// 分组属于清单实质字段：修订使清单 epoch 递增；非法分组被拒绝。
	epoch := v.Manifest.Epoch
	commercial := ScoreGroupCommercial
	if _, err := f.d.UpdateRequirement(ctx, DefaultTenant, f.task.ID, techID, "reviewer", RequirementInput{ScoreGroup: &commercial}); err != nil {
		t.Fatal(err)
	}
	technical := ScoreGroupTechnical
	if _, err := f.d.UpdateRequirement(ctx, DefaultTenant, f.task.ID, techID, "reviewer", RequirementInput{ScoreGroup: &technical}); err != nil {
		t.Fatal(err)
	}
	bad := "bonus"
	if _, err := f.d.UpdateRequirement(ctx, DefaultTenant, f.task.ID, techID, "reviewer", RequirementInput{ScoreGroup: &bad}); !errors.Is(err, ErrInvalidRequirement) {
		t.Fatalf("invalid score_group accepted: %v", err)
	}
	if m, _ := f.d.RequirementManifest(ctx, DefaultTenant, f.task.ID); m.Epoch <= epoch {
		t.Fatalf("score_group change did not bump the manifest epoch: %d -> %d", epoch, m.Epoch)
	}

	// 旧规则产出、本次不再产出的提取项：标记待复核，确认清单前须人工处理。
	if _, err := f.d.ExecContext(ctx, `INSERT INTO requirement_items(tenant_id, task_id, code, category, title, score_weight, origin, review_status)
		VALUES ($1, $2, 'SC-OLD-T1R1', 'scoring_item', '务材料', 20, 'extracted', 'confirmed')`, DefaultTenant, f.task.ID); err != nil {
		t.Fatal(err)
	}
	again, err := f.d.ExtractRequirements(ctx, DefaultTenant, f.task.ID, "reviewer")
	if err != nil || again.Orphaned != 1 || len(again.OrphanCodes) != 1 || again.OrphanCodes[0] != "SC-OLD-T1R1" || again.Created != 0 {
		t.Fatalf("re-extract orphans = %+v %v", again, err)
	}
	current, err := f.d.ListRequirements(ctx, DefaultTenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Scoring) != 2 || current.Scoring[0].Total != 100 || current.Scoring[1].Total != 100 {
		t.Fatalf("orphan score counted in current results: %+v", current.Scoring)
	}
	m, _ := f.d.RequirementManifest(ctx, DefaultTenant, f.task.ID)
	if _, err := f.d.ConfirmRequirements(ctx, DefaultTenant, f.task.ID, "reviewer", m.Epoch); !errors.Is(err, ErrRequirementReview) {
		t.Fatalf("manifest confirmed with an orphan awaiting review: %v", err)
	}
	if _, err := f.d.ExecContext(ctx, `DELETE FROM requirement_items WHERE task_id = $1 AND code = 'SC-OLD-T1R1'`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	m, _ = f.d.RequirementManifest(ctx, DefaultTenant, f.task.ID)
	if _, err := f.d.ConfirmRequirements(ctx, DefaultTenant, f.task.ID, "reviewer", m.Epoch); err != nil {
		t.Fatal(err)
	}

	// 默认大纲：价格评分项不映射也不阻断批准；技术评分项逐项成节，商务评分项归入一章。
	det, err := f.d.CreateOutline(ctx, DefaultTenant, f.task.ID, "editor", OutlineInput{})
	if err != nil {
		t.Fatal(err)
	}
	if det.Coverage.OutOfScope != 2 || len(det.Coverage.Unmapped) != 0 || det.Coverage.Total != det.Coverage.Mapped {
		t.Fatalf("outline coverage = %+v", det.Coverage)
	}
	var titles []string
	for _, s := range det.Sections {
		titles = append(titles, s.Title)
	}
	joined := strings.Join(titles, "|")
	if strings.Contains(joined, "投标报价") || !strings.Contains(joined, "【包1】总体方案") || !strings.Contains(joined, "商务评分项响应") {
		t.Fatalf("outline sections = %s", joined)
	}
	if _, err := f.d.SubmitOutline(ctx, DefaultTenant, f.task.ID, det.Outline.Version, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ApproveOutline(ctx, DefaultTenant, f.task.ID, det.Outline.Version, "approver"); err != nil {
		t.Fatalf("approve with price items unmapped: %v", err)
	}
	cov, err := f.d.ComplianceCoverage(ctx, DefaultTenant, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Total != det.Coverage.Total || cov.Mapped != cov.Total {
		t.Fatalf("compliance coverage must exclude price items: %+v vs outline %+v", cov, det.Coverage)
	}
}
