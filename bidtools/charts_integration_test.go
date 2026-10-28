package bidtools

// chart_generate 工具对真实 PostgreSQL 的集成测试（需要 AUTOBID_TEST_DATABASE_URL）：结构化规格生成 Mermaid、绑定章节当前修订、
// mmdc 不可用时为 unrendered；规格错误与章节无修订返回工具错误；未配置图表服务时明确报错。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/export"
)

func TestChartGenerateTool(t *testing.T) {
	d := dbtest.Open(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	ctx := context.Background()
	task, err := d.GetTask(ctx, p.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	kit := New(d, p.TaskID, db.NewAssetStore(d, p.TaskID), db.NewExplorationStore(d, task.ExplorationID))
	call := func(in string) (string, bool) {
		t.Helper()
		for _, tl := range kit.DeferredTools() {
			if tl.Name() == ToolChartGenerate {
				res, err := tl.Call(ctx, json.RawMessage(in), nil)
				if err != nil {
					t.Fatal(err)
				}
				return res.Content, res.IsError
			}
		}
		t.Fatal("chart_generate not registered")
		return "", false
	}
	if out, isErr := call(`{"section_code":"5","chart_type":"gantt","title":"进度","spec":{}}`); !isErr || !strings.Contains(out, "not configured") {
		t.Fatalf("without chart service: %s", out)
	}
	kit.WithCharts(&export.Service{DB: d, Store: &export.Store{Root: t.TempDir()}, RendererReason: "测试：mmdc 未安装"})
	out, isErr := call(`{"section_code":"5","chart_type":"gantt","title":"实施进度","spec":{"start":"2026-11-01","tasks":[{"id":"a","name":"调研","days":5},{"name":"实施","after":"a","days":20}]}}`)
	if isErr {
		t.Fatalf("chart_generate: %s", out)
	}
	var res map[string]any
	_ = json.Unmarshal([]byte(out), &res)
	if res["render_status"] != db.ChartUnrendered || res["rev_no"] != float64(1) || !strings.Contains(fmt.Sprint(res["source"]), "after a") {
		t.Fatalf("result = %+v", res)
	}
	if out, isErr := call(`{"section_code":"5","chart_type":"org","title":"组织","spec":{"nodes":[{"id":"a","name":"A","parent":"x"}]}}`); !isErr || !strings.Contains(out, "unknown parent") {
		t.Fatalf("invalid org spec: %s", out)
	}
	charts, err := d.ListCharts(ctx, db.DefaultTenant, p.TaskID, p.Sections["5"])
	if err != nil || len(charts) != 1 || charts[0].CreatedBy != "worker" {
		t.Fatalf("charts = %+v %v", charts, err)
	}
	found := false
	for _, s := range Skills() {
		if s.Name == SkillProposalWriting {
			for _, n := range s.Tools {
				found = found || n == ToolChartGenerate
			}
		}
	}
	if !found {
		t.Fatal("proposal-writing skill does not unlock chart_generate")
	}
}
