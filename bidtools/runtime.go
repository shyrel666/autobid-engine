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
	return []tool.CoreTool{runtimeStatusTool(k.runtime, k.retrieval, k.tenant, multi)}
}

// RuntimeStatusTool 构造 tool_runtime_status。
func RuntimeStatusTool(reg *toolhealth.Registry, services ...*retrieval.Service) tool.CoreTool {
	var svc *retrieval.Service
	if len(services) > 0 {
		svc = services[0]
	}
	return runtimeStatusTool(reg, svc, db.DefaultTenant, false)
}

func runtimeStatusTool(reg *toolhealth.Registry, svc *retrieval.Service, tenant string, multi bool) tool.CoreTool {
	caps := []any{"all"}
	for _, c := range toolhealth.All {
		caps = append(caps, string(c))
	}
	return readTool(tool.Spec{
		Name: ToolRuntimeStatus,
		Description: "查询本机运行能力的功能自检状态：office（WPS / Word 文档引擎，决定 PDF 与最终稿导出）、mermaid（图表渲染）、ocr（扫描件识别）、" +
			"enhanced_pdf（增强 PDF 解析）、embedding / reranker（语义检索模型）。state 为 unconfigured / starting / healthy / degraded / self_test_failed；" +
			"available=false 时依赖该能力的工作会失败或降级，应如实记录缺口，不要反复重试。查询 all / embedding / reranker 时同时返回语义检索与活动索引状态。",
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
