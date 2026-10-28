// Package bidtools 是招投标领域的 Worker 工具集：
// 常驻文档精读工具（document_outline / search_document / read_chunk，挂 WorkerConfig.ExtraTools），
// 以及经 Skill 渐进式披露的专业延迟工具（§4.3，挂 WorkerConfig.Skills + WorkerConfig.Deferred）。
package bidtools

import (
	"context"
	"encoding/json"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/retrieval"
)

// 常驻工具名。
const (
	ToolDocumentOutline = "document_outline"
	ToolSearchDocument  = "search_document"
	ToolReadChunk       = "read_chunk"
)

// 延迟工具名（须先调用所属 Skill 解锁，再经 SearchExtraTools 取 Schema）。
const (
	ToolQueryCredentials = "query_company_credentials"
	ToolSimulateScoring  = "simulate_scoring_table"
	ToolVerifyCommercial = "verify_commercial_bid"
	// 企业方案知识库（knowledge-reuse 技能）
	ToolKBSearchProposals = "kb_search_proposals"
	ToolKBSearchProducts  = "kb_search_products"
	ToolKBReadChunk       = "kb_read_chunk"
)

// Kit 绑定一个任务：文档检索只读该任务的 document_chunks，要素与事实读取走任务的 AssetStore / ExplorationStore；
// 知识库检索按 tenant 隔离（Phase 7 前为默认租户），向量召回使用 emb。
type Kit struct {
	pg        *db.DB
	taskID    int64
	as        *db.AssetStore
	es        *db.ExplorationStore
	tenant    string
	emb       db.Embedder
	retrieval *retrieval.Service
	research  *retrieval.Coordinator
	charts    *export.Service
}

func (k *Kit) WithRetrieval(s *retrieval.Service) *Kit { k.retrieval = s; return k }

func New(pg *db.DB, taskID int64, as *db.AssetStore, es *db.ExplorationStore) *Kit {
	return &Kit{pg: pg, taskID: taskID, as: as, es: es, tenant: db.DefaultTenant, emb: db.DefaultEmbedder()}
}

// WithKnowledge 设置知识库租户与向量器（emb 为 nil 时只做全文 + 关键词检索）。
func (k *Kit) WithKnowledge(tenant string, emb db.Embedder) *Kit {
	if tenant != "" {
		k.tenant = tenant
	}
	k.emb = emb
	return k
}

// ResidentTools 返回常驻文档精读工具。
func (k *Kit) ResidentTools() []tool.CoreTool {
	return []tool.CoreTool{k.documentOutlineTool(), k.searchDocumentTool(), k.readChunkTool()}
}

// DeferredTools 返回专业延迟工具；它们只注册、不下发 Schema。
func (k *Kit) DeferredTools() []tool.CoreTool {
	return append([]tool.CoreTool{k.queryCredentialsTool(), k.simulateScoringTool(), k.verifyCommercialTool(),
		k.kbSearchProposalsTool(), k.kbSearchProductsTool(), k.kbReadChunkTool(), k.chartGenerateTool()},
		append(append(k.proposalTools(), k.reviewTools()...), k.retrievalTools()...)...)
}

// ---------------------------------------------------------------------------
// schema helpers（与 agent 包同构）
// ---------------------------------------------------------------------------

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func number(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func decode[T any](input json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(input, &v)
	return v, err
}

func readTool(spec tool.Spec, fn func(ctx context.Context, in json.RawMessage) (tool.Result, error)) tool.CoreTool {
	spec.ReadOnly, spec.Concurrent = true, true
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		return fn(ctx, in)
	})
}

func truncate(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
