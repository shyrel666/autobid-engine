package bidtools

// 公共只读工具 tool_runtime_status：查询本机运行能力（文档引擎、Mermaid-CLI、OCR、增强 PDF、embedding、reranker）
// 的自检状态。与 GET /api/runtime/tools 读取同一个 toolhealth.Registry；原因文本已脱敏。

import (
	"context"
	"encoding/json"

	"github.com/autobid/autobid-engine/internal/tool"
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
	return []tool.CoreTool{RuntimeStatusTool(k.runtime)}
}

// RuntimeStatusTool 构造 tool_runtime_status。
func RuntimeStatusTool(reg *toolhealth.Registry) tool.CoreTool {
	caps := []any{"all"}
	for _, c := range toolhealth.All {
		caps = append(caps, string(c))
	}
	return readTool(tool.Spec{
		Name: ToolRuntimeStatus,
		Description: "查询本机运行能力的功能自检状态：office（WPS / Word 文档引擎，决定 PDF 与最终稿导出）、mermaid（图表渲染）、ocr（扫描件识别）、" +
			"enhanced_pdf（增强 PDF 解析）、embedding / reranker（语义检索模型）。state 为 unconfigured / starting / healthy / degraded / self_test_failed；" +
			"available=false 时依赖该能力的工作会失败或降级，应如实记录缺口，不要反复重试。",
		Schema: obj(map[string]any{
			"capability": map[string]any{"type": "string", "enum": caps, "description": "要查询的能力，默认 all"},
		}),
	}, func(_ context.Context, in json.RawMessage) (tool.Result, error) {
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
		return tool.JSON(map[string]any{"capabilities": list}), nil
	})
}
