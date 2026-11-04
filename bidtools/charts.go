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

// ChartSpecGuide 说明四类图表的结构化规格（chart_generate、chart_render_submit 与章节撰写的图表计划共用）。
const ChartSpecGuide = "支持四类图表（chart_type）：" +
	`architecture（技术架构图，spec 示例：{"direction":"TB|LR","layers":[{"name":"感知层","nodes":["摄像机"]}],"edges":[{"from":"摄像机","to":"视频平台","label":"RTSP"}]}）；` +
	`gantt（实施甘特图，spec 示例：{"start":"2026-11-01","tasks":[{"id":"t1","name":"需求调研","phase":"准备阶段","days":10},{"name":"部署","after":"t1","days":20}]}）；` +
	`org（项目组织架构图，spec 示例：{"nodes":[{"id":"pm","name":"项目经理","person":"张三"},{"id":"tech","name":"技术组","parent":"pm"}]}）；` +
	`equipment_table（设备选型表，spec 示例：{"columns":["序号","设备名称","品牌型号","数量"],"rows":[["1","核心交换机","S7706","2"]]}）。`

// WithCharts 设置图表服务（为 nil 时 chart_generate 返回未配置错误）。
func (k *Kit) WithCharts(s *export.Service) *Kit { k.charts = s; return k }

func (k *Kit) chartGenerateTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolChartGenerate,
		Description: "按结构化规格生成技术标章节图表并绑定到该章节当前主版本（同步兼容工具；Agent 走 diagram-generation 技能的 chart_render_submit）。" +
			ChartSpecGuide +
			"服务端确定性生成并校验 Mermaid 源码；若未安装 mmdc 渲染器则保存为源码占位框并在导出门禁中提示。章节产生新修订版本后旧图表自动标记为过期。",
		Schema: obj(map[string]any{
			"section_id": integer("所属技术标章节 ID（与 section_code 二选一）"), "section_code": str("批准大纲中的章节编号（如 \"2.1\"，与 section_id 二选一）"),
			"chart_type": map[string]any{"type": "string", "enum": export.ChartTypes(), "description": "图表类型：architecture（技术架构图）| gantt（实施甘特图）| org（组织架构图）| equipment_table（设备选型表）"},
			"title":      str("图表标题/图注（如 \"系统总体技术架构图\"）"),
			"spec":       map[string]any{"type": "object", "description": "结构化图表规格对象（具体字段见工具说明）"},
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
