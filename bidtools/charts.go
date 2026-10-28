package bidtools

// Phase 6B 标书图表工具（经 proposal-writing 技能解锁）：
//   chart_generate  按结构化规格生成章节图表并绑定到该章节当前主版本：架构图 / 实施甘特图 / 组织架构图生成 Mermaid 源码
//                   （由 export 包确定性生成并校验，不接受模型直接给出的 Mermaid），设备选型表保存为结构化 JSON；
//                   mmdc 可用时渲染为 PNG，不可用时为 unrendered（导出时插入源码占位框）。
// 图表属于标书正文域（proposal_charts），不写 assets 或 explorations/findings。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/internal/tool"
)

const ToolChartGenerate = "chart_generate"

// WithCharts 设置图表服务（为 nil 时 chart_generate 返回未配置错误）。
func (k *Kit) WithCharts(s *export.Service) *Kit { k.charts = s; return k }

func (k *Kit) chartGenerateTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolChartGenerate,
		Description: "Generate a proposal chart for a section and bind it to the section's current revision. chart_type and spec: " +
			`architecture {"direction":"TB|LR","layers":[{"name":"感知层","nodes":["摄像机"]}],"edges":[{"from":"摄像机","to":"视频平台","label":"RTSP"}]}; ` +
			`gantt {"start":"2026-11-01","tasks":[{"id":"t1","name":"需求调研","phase":"准备阶段","days":10},{"name":"部署","after":"t1","days":20}]}; ` +
			`org {"nodes":[{"id":"pm","name":"项目经理","person":"张三"},{"id":"tech","name":"技术组","parent":"pm"}]}; ` +
			`equipment_table {"columns":["序号","设备名称","品牌型号","数量"],"rows":[["1","核心交换机","S7706","2"]]}. ` +
			"Mermaid source is generated and validated by the server. When mermaid-cli is unavailable the chart stays unrendered and is exported as a source placeholder. " +
			"A new revision of the section makes older charts stale (they are left out of exports until regenerated).",
		Schema: obj(map[string]any{
			"section_id": integer("proposal section id"), "section_code": str("section code in the approved outline (alternative to section_id)"),
			"chart_type": map[string]any{"type": "string", "enum": export.ChartTypes(), "description": "chart type"},
			"title":      str("chart caption, e.g. 系统总体架构图"),
			"spec":       map[string]any{"type": "object", "description": "structured chart spec (see description)"},
		}, "chart_type", "title", "spec")}
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		if k.charts == nil {
			return tool.Errorf("chart service is not configured on this server"), nil
		}
		req, err := decode[struct {
			SectionID int64           `json:"section_id"`
			Code      string          `json:"section_code"`
			ChartType string          `json:"chart_type"`
			Title     string          `json:"title"`
			Spec      json.RawMessage `json:"spec"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		id, err := k.resolveSection(ctx, req.SectionID, req.Code)
		if err != nil {
			return proposalErr(err)
		}
		c, err := k.charts.GenerateChart(ctx, k.tenant, k.taskID, "worker", export.ChartRequest{SectionID: id, ChartType: strings.TrimSpace(req.ChartType),
			Title: req.Title, Spec: req.Spec})
		switch {
		case errors.Is(err, export.ErrChartSpec), errors.Is(err, db.ErrInvalidChart):
			return tool.Errorf("%v; fix the spec and call again", err), nil
		case errors.Is(err, db.ErrChartNoRevision):
			return tool.Errorf("the section has no revision yet: write the section draft first, then generate its charts"), nil
		case err != nil:
			return proposalErr(err)
		}
		out := map[string]any{"chart_id": c.ID, "section": c.SectionCode, "rev_no": c.RevNo, "chart_type": c.ChartType,
			"render_status": c.RenderStatus, "source_format": c.SourceFormat, "source": c.Source}
		if c.RenderError != "" {
			out["render_error"] = c.RenderError
		}
		if c.RenderStatus != db.ChartRendered {
			out["note"] = "chart saved but not rendered; it will be exported as a source placeholder and listed in the export gate details"
		}
		return tool.JSON(out), nil
	})
}
