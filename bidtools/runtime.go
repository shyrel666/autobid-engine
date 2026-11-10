package bidtools

// 公共只读工具 tool_runtime_status：查询本机运行能力（文档引擎、Mermaid-CLI、OCR、增强 PDF、embedding、reranker）
// 的自检状态。与 GET /api/runtime/tools 读取同一个 toolhealth.Registry；原因文本已脱敏。

import (
	"context"
	"encoding/json"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/retrieval"
	"github.com/autobid/autobid-engine/toolhealth"
)

// ToolRuntimeStatus 是公共只读运行能力查询工具（Planner 与 Worker 常驻，不经 Skill 解锁）。
const ToolRuntimeStatus = "tool_runtime_status"

// WithRuntime 设置运行能力状态来源（为 nil 时不提供 tool_runtime_status）。
func (k *Kit) WithRuntime(r *toolhealth.Registry) *Kit { k.runtime = r; return k }

// RuntimeTools 返回公共只读运行能力工具；未设置状态来源时为空。
func (k *Kit) RuntimeTools() []tool.CoreTool {
	if k.runtime == nil {
		return nil
	}
	multi := k.research != nil && k.research.MultiEnabled
	return []tool.CoreTool{runtimeStatusTool(k.runtime, k.retrieval, k.tenant, multi, k.hostFeatures)}
}

// hostFeatures 报告服务器配置授予的能力开关。它与运行组件健康无关：开关关闭时对应提交一律被拒绝，
// 重试或换参数都没有用，所以要让 Planner / Worker 在派发和调用前就看到（计划第 11 节“缺失运行服务时不向 Agent 宣称可用”）。
func (k *Kit) hostFeatures() map[string]any {
	feature := func(enabled bool, off string) map[string]any {
		f := map[string]any{"enabled": enabled}
		if !enabled {
			f["note"] = off
		}
		return f
	}
	return map[string]any{
		"file_processing": feature(k.files != nil, "服务器未启用材料处理：document_parse_* 提交返回 FEATURE_DISABLED，招标原件由人工在页面导入，不要派发解析方向"),
		"agent_export":    feature(k.exportSubmit, "服务器未开放 Agent 提交导出：proposal_export_submit 返回 FEATURE_DISABLED，可以预检和查询，最终稿由人工在导出页提交"),
	}
}

// RuntimeStatusTool 构造 tool_runtime_status。
func RuntimeStatusTool(reg *toolhealth.Registry, services ...*retrieval.Service) tool.CoreTool {
	var svc *retrieval.Service
	if len(services) > 0 {
		svc = services[0]
	}
	return runtimeStatusTool(reg, svc, db.DefaultTenant, false, nil)
}

func runtimeStatusTool(reg *toolhealth.Registry, svc *retrieval.Service, tenant string, multi bool, features func() map[string]any) tool.CoreTool {
	caps := []any{"all"}
	for _, c := range toolhealth.All {
		caps = append(caps, string(c))
	}
	return readTool(tool.Spec{
		Name: ToolRuntimeStatus,
		Description: "查询本机运行能力的功能自检状态：office（WPS / Word 文档引擎，决定 PDF 与最终稿导出）、mermaid（图表渲染）、ocr（扫描件识别）、" +
			"enhanced_pdf（增强 PDF 解析）、embedding / reranker（语义检索模型）。state 为 unconfigured / starting / healthy / degraded / self_test_failed；" +
			"available=false 时依赖该能力的工作会失败或降级，应如实记录缺口，不要反复重试。查询 all / embedding / reranker 时同时返回语义检索与活动索引状态。" +
			"查询 all 时另返回 features：服务器开关 file_processing（招标文件解析）与 agent_export（Agent 提交导出），enabled=false 时对应提交一律被拒绝。",
		Schema: obj(map[string]any{
			"capability": map[string]any{"type": "string", "enum": caps, "description": "要查询的能力，默认 all"},
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		args, err := decode[struct {
			Capability string `json:"capability"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		var list []toolhealth.Status
		switch c := toolhealth.Capability(args.Capability); {
		case c == "" || c == "all":
			list = reg.List()
		case toolhealth.Valid(c):
			list = reg.List(c)
		default:
			return tool.Errorf("INVALID_ARGUMENT: unknown capability %q", args.Capability), nil
		}
		out := map[string]any{"capabilities": list}
		if features != nil && (args.Capability == "" || args.Capability == "all") {
			out["features"] = features()
		}
		if svc != nil && (args.Capability == "" || args.Capability == "all" || args.Capability == "embedding" || args.Capability == "reranker") {
			status, err := svc.Status(ctx, tenant)
			if err != nil {
				out["semantic_retrieval_error"] = "semantic retrieval status unavailable"
			} else {
				semantic := map[string]any{"state": status.State, "reasons": status.Reasons, "index_state": status.IndexState, "strategy": status.Strategy, "multi_agent_enabled": multi}
				if x := status.ActiveIndex; x != nil {
					semantic["active_index"] = map[string]any{"id": x.ID, "model": x.Model, "dimension": x.Dimension, "epoch": x.Epoch}
				}
				out["semantic_retrieval"] = semantic
			}
		}
		return tool.JSON(out), nil
	})
}
