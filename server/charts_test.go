package server

// Phase 6B 章节图表 HTTP 集成测试（需要 AUTOBID_TEST_DATABASE_URL）：规格错误 400、生成 201（绑定当前修订）、列表、PNG、
// 原生表无图片 404、重新渲染、重新绑定、跨任务 404、章节无修订 422、删除 204。

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/export"
)

type pngRenderer struct{}

func (pngRenderer) Name() string { return "test-png" }
func (pngRenderer) RenderPNG(context.Context, string) ([]byte, error) {
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewGray(image.Rect(0, 0, 800, 500)))
	return buf.Bytes(), nil
}

func newChartAPI(t *testing.T) (*apiClient, *export.Service) {
	t.Helper()
	pg := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(pg)
	e := NewEngine(m, func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil }, Config{Workers: 1})
	svc := &export.Service{DB: pg, Store: &export.Store{Root: t.TempDir()}, Renderer: pngRenderer{}}
	api := testAPI(ctx, m, e, NewLLMService(pg, LLMServiceConfig{}), nil, NewHub(), APIConfig{Export: svc})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		e.StopAll()
		cancel()
	})
	return &apiClient{t: t, url: srv.URL}, svc
}

func (c *apiClient) download(path string) (int, http.Header, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest("GET", c.url+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

func TestHTTPCharts(t *testing.T) {
	d := dbtest.Open(t)
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(0))
	c, _ := newChartAPI(t)
	base := fmt.Sprintf("/api/tasks/%d", p.TaskID)
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}

	// 图表：规格错误 400；生成 201（绑定当前修订）；列表；图片；重新渲染；重新绑定；删除。
	if code := c.raw("POST", base+"/charts", map[string]any{"section_id": p.Sections["5"], "chart_type": "gantt", "title": "进度", "spec": map[string]any{"tasks": []any{}}}, &e); code != 400 || e.Code != "invalid_chart" {
		t.Fatalf("invalid spec: %d %+v", code, e)
	}
	if code := c.raw("POST", base+"/charts", map[string]any{"chart_type": "gantt", "title": "进度", "spec": map[string]any{}}, &e); code != 400 {
		t.Fatalf("missing section_id: %d", code)
	}
	var chart db.ProposalChart
	if code := c.raw("POST", base+"/charts", map[string]any{"section_id": p.Sections["5"], "chart_type": "gantt", "title": "实施进度甘特图",
		"spec": map[string]any{"start": "2026-11-01", "tasks": []map[string]any{{"id": "a", "name": "调研", "days": 5}, {"name": "实施", "after": "a", "days": 20}}}}, &chart); code != 201 {
		t.Fatalf("generate chart: %d", code)
	}
	if chart.RenderStatus != db.ChartRendered || chart.RevNo != 1 || chart.SectionCode != "5" || !strings.HasPrefix(chart.Source, "gantt") {
		t.Fatalf("chart = %+v", chart)
	}
	var table db.ProposalChart
	if code := c.raw("POST", base+"/charts", map[string]any{"section_id": p.Sections["6"], "chart_type": "equipment_table", "title": "设备选型",
		"spec": map[string]any{"columns": []string{"设备", "数量"}, "rows": [][]string{{"交换机", "2"}}}}, &table); code != 201 || table.RenderFormat != db.ChartFormatNative {
		t.Fatalf("equipment table: %d %+v", code, table)
	}
	var list struct {
		Charts   []db.ProposalChart `json:"charts"`
		Renderer export.Status      `json:"renderer"`
	}
	if code := c.raw("GET", fmt.Sprintf("%s/charts?section_id=%d", base, p.Sections["5"]), nil, &list); code != 200 || len(list.Charts) != 1 || !list.Renderer.Renderer {
		t.Fatalf("list charts: %d %+v", code, list)
	}
	if code, h, img := c.download(fmt.Sprintf("%s/charts/%d/image", base, chart.ID)); code != 200 || h.Get("Content-Type") != "image/png" || len(img) < 50 {
		t.Fatalf("chart image: %d %s", code, h.Get("Content-Type"))
	}
	if code := c.raw("GET", fmt.Sprintf("%s/charts/%d/image", base, table.ID), nil, &e); code != 404 {
		t.Fatalf("native table image: %d", code)
	}
	if code := c.raw("POST", fmt.Sprintf("%s/charts/%d/render", base, chart.ID), nil, &chart); code != 200 || chart.RenderStatus != db.ChartRendered {
		t.Fatalf("rerender: %d", code)
	}
	if code := c.raw("POST", fmt.Sprintf("%s/charts/%d/rebind", base, chart.ID), nil, &chart); code != 200 || chart.Stale {
		t.Fatalf("rebind: %d %+v", code, chart)
	}
	if code := c.raw("GET", fmt.Sprintf("/api/tasks/%d/charts/%d", p.TaskID+100000, chart.ID), nil, &e); code != 404 {
		t.Fatalf("chart of other task: %d", code)
	}

	var status export.Status
	if code := c.raw("GET", "/api/export/status", nil, &status); code != 200 || !status.Renderer {
		t.Fatalf("export status: %d %+v", code, status)
	}
	if code := c.raw("DELETE", fmt.Sprintf("%s/charts/%d", base, chart.ID), nil, nil); code != 204 {
		t.Fatalf("delete chart: %d", code)
	}
	if code := c.raw("GET", fmt.Sprintf("%s/charts/%d", base, chart.ID), nil, &e); code != 404 {
		t.Fatalf("deleted chart: %d", code)
	}

	// 章节尚无修订：422。
	var task struct {
		ID int64 `json:"id"`
	}
	if code := c.raw("POST", "/api/tasks", map[string]any{"name": dbtest.Uniq("charts"), "goal": "图表"}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	tb := fmt.Sprintf("/api/tasks/%d", task.ID)
	var it db.RequirementItem
	c.raw("POST", tb+"/requirements", map[string]any{"code": "R1", "category": "scoring_item", "title": "技术方案", "description": "方案合理得分。"}, &it)
	c.raw("POST", tb+"/requirements/confirm", map[string]any{}, nil)
	var outline db.OutlineDetail
	if code := c.raw("POST", tb+"/outlines", map[string]any{"sections": []map[string]any{{"section_code": "1", "title": "方案", "level": 1, "requirement_item_ids": []int64{it.ID}}}}, &outline); code != 201 {
		t.Fatalf("outline: %d", code)
	}
	if code := c.raw("POST", tb+"/charts", map[string]any{"section_id": outline.Sections[0].ID, "chart_type": "org", "title": "组织",
		"spec": map[string]any{"nodes": []map[string]any{{"id": "pm", "name": "项目经理"}}}}, &e); code != 422 || e.Code != "no_revision" {
		t.Fatalf("chart without revision: %d %+v", code, e)
	}
	_ = strings.HasPrefix
}
