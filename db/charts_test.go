package db

// Phase 6B 图表集成测试（真实 PostgreSQL）：图表绑定章节当前修订、章节新修订后查询时判定过期（不进入导出快照）、
// 重新绑定、渲染写回的源码哈希校验、章节尚无修订时拒绝生成。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// readyComplianceFixture 让 6A 样本达到可导出状态（与 TestNewRevisionStalesMatrixAndBlocksExport 相同的修正路径）。
func readyComplianceFixture(t *testing.T) *complianceFixture {
	t.Helper()
	f := newComplianceFixture(t)
	ctx := context.Background()
	f.writeAll(t, nil)
	f.save(t, f.secs["3"], 1, f.ver, RevHumanEdit, "我方承诺免费质保期 5 年，优于招标要求。", 0)
	f.save(t, f.secs["5"], 1, f.ver, RevHumanEdit, "实施进度安排：工期 90 天，分三个阶段完成。", 0)
	for _, id := range f.secs {
		det := f.section(t, id)
		if _, err := f.d.ApproveSectionRevision(ctx, DefaultTenant, f.task.ID, id, det.Section.CurrentRevNo, "approver"); err != nil {
			t.Fatal(err)
		}
	}
	f.verify(t, 0)
	f.consistency(t)
	if r, err := f.d.ExportReadiness(ctx, DefaultTenant, f.task.ID); err != nil || !r.Ready {
		t.Fatalf("fixture not ready: %+v %v", r, err)
	}
	return f
}

func TestChartsBindRevisionAndGoStale(t *testing.T) {
	f := readyComplianceFixture(t)
	ctx := context.Background()
	sid := f.secs["1"]
	in := ChartInput{SectionID: sid, ChartType: ChartArchitecture, Title: "总体架构图", SourceFormat: ChartSourceMermaid, Source: "flowchart TB\n  A-->B", SourceHash: "h1"}
	if _, err := f.d.CreateChart(ctx, DefaultTenant, f.task.ID, "w", ChartInput{SectionID: f.secs["2"], ChartType: "pie", Title: "x", Source: "x", SourceHash: "x"}); !errors.Is(err, ErrInvalidChart) {
		t.Fatalf("invalid type: %v", err)
	}
	c, err := f.d.CreateChart(ctx, DefaultTenant, f.task.ID, "writer", in)
	if err != nil || c.RevNo != 1 || c.Stale || c.RenderStatus != ChartPending {
		t.Fatalf("CreateChart = %+v %v", c, err)
	}
	if _, err := f.d.SetChartRender(ctx, DefaultTenant, f.task.ID, c.ID, "other-hash", ChartRender{Status: ChartUnrendered}, "w"); !errors.Is(err, ErrChartSourceMoved) {
		t.Fatalf("render with stale source hash: %v", err)
	}
	if c, err = f.d.SetChartRender(ctx, DefaultTenant, f.task.ID, c.ID, "h1", ChartRender{Status: ChartUnrendered, Error: "mmdc 不可用"}, "w"); err != nil || c.RenderStatus != ChartUnrendered {
		t.Fatalf("SetChartRender = %+v %v", c, err)
	}
	// 章节新修订：图表过期（查询时判定）；重新绑定后恢复。
	f.save(t, sid, 1, f.ver, RevHumanEdit, "本项目采用云边协同总体架构，总体架构合理、技术路线先进，我方承诺总工期 90 天，按计划交付。", 0)
	c, _ = f.d.GetChart(ctx, DefaultTenant, f.task.ID, c.ID)
	if !c.Stale || c.CurrentRevNo != 2 || !strings.Contains(c.StaleReason, "rev 2") {
		t.Fatalf("chart after revision = %+v", c)
	}
	if c, err = f.d.RebindChart(ctx, DefaultTenant, f.task.ID, c.ID, "w"); err != nil || c.Stale || c.RevNo != 2 {
		t.Fatalf("rebind = %+v %v", c, err)
	}
	list, err := f.d.ListCharts(ctx, DefaultTenant, f.task.ID, sid)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListCharts = %v %v", list, err)
	}
	if err := f.d.DeleteChart(ctx, DefaultTenant, f.task.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.GetChart(ctx, DefaultTenant, f.task.ID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted chart: %v", err)
	}
	// 章节尚无修订时不能生成图表。
	p := newProposalFixture(t)
	_, ids := p.approvedOutline(t)
	for _, id := range ids {
		if _, err := p.d.CreateChart(ctx, DefaultTenant, p.task.ID, "w", ChartInput{SectionID: id, ChartType: ChartGantt, Title: "x", Source: "gantt", SourceHash: "x"}); !errors.Is(err, ErrChartNoRevision) {
			t.Fatalf("chart without revision: %v", err)
		}
		break
	}
}
