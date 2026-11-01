package bidtools

import (
	"context"
	"encoding/json"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/retrieval"
)

func (k *Kit) WithResearch(c *retrieval.Coordinator) *Kit { k.research = c; return k }

func (k *Kit) kbReadEvidenceTool() tool.CoreTool {
	return readTool(tool.Spec{Name: ToolKBReadEvidence, Description: "读取当前任务下某次检索运行（run_id）保存的证据快照及其实时有效状态。注意：检索命中仅代表相关，未经显式核验前状态为 unverified（未核验）。", Schema: obj(map[string]any{"run_id": integer("检索运行记录 ID（run_id）")}, "run_id")}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		if k.retrieval == nil {
			return tool.Errorf("retrieval service is not configured on this server"), nil
		}
		var req struct {
			RunID int64 `json:"run_id"`
		}
		if json.Unmarshal(in, &req) != nil {
			return tool.Errorf("invalid input"), nil
		}
		if err := k.ownRun(ctx, req.RunID); err != nil {
			return kbErr(err)
		}
		v, err := k.retrieval.Evidence(ctx, k.tenant, req.RunID)
		if err != nil {
			return kbErr(err)
		}
		return tool.JSON(v), nil
	})
}

func (k *Kit) kbVerifyEvidenceTool() tool.CoreTool {
	return readTool(tool.Spec{Name: ToolKBVerifyEvidence, Description: "对照原始证据快照、产品元数据、计量单位与时效性，对结构化数值或技术指标声明（claim）执行确定性核验。带附加条件或存在歧义的指标将保持 insufficient（证据不足）状态。", Schema: obj(map[string]any{
		"run_id": integer("检索运行记录 ID（可选；若省略将按 evidence_id 自动定位）"), "evidence_id": integer("待核验的证据快照 ID"), "claim": obj(map[string]any{"text": str("完整指标响应声明文本"), "parameter": str("精确技术参数名称"), "operator": str("比较运算符：<=、>= 或 ="), "value": number("指标目标阈值数值"), "unit": str("计量单位"), "product": str("精确产品或设备型号"), "version": str("精确硬件/软件版本号"), "as_of": str("有效性基准日期 YYYY-MM-DD"), "conditions": strs("附加测试或运行条件列表（存在时需人工复核）")}, "text", "parameter", "operator", "value", "unit")}, "evidence_id", "claim")}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		if k.retrieval == nil {
			return tool.Errorf("retrieval service is not configured on this server"), nil
		}
		var req struct {
			RunID      int64           `json:"run_id"`
			EvidenceID int64           `json:"evidence_id"`
			Claim      retrieval.Claim `json:"claim"`
		}
		if json.Unmarshal(in, &req) != nil {
			return tool.Errorf("invalid input"), nil
		}
		if req.RunID <= 0 && k.pg != nil {
			if states, err := k.pg.EvidenceStates(ctx, k.tenant, k.taskID, []int64{req.EvidenceID}); err == nil && len(states) == 1 && !states[0].Missing {
				req.RunID = states[0].RunID
			}
		}
		if err := k.ownRun(ctx, req.RunID); err != nil {
			return kbErr(err)
		}
		v, err := k.retrieval.Verify(ctx, k.tenant, "worker", req.RunID, req.EvidenceID, req.Claim)
		if err != nil {
			return kbErr(err)
		}
		return tool.JSON(v), nil
	})
}

func (k *Kit) kbResearchTool() tool.CoreTool {
	return readTool(tool.Spec{Name: ToolKBResearch, Description: "受控多步证据研究工具：基于幂等 request_key、共享检索预算与最多两轮追问补查知识库证据（多智能体并行模式仅在评测开启后生效），返回持久化 run_id、evidence_ids 与明确证据缺口。", Schema: obj(map[string]any{"request_key": str("当前任务与研究问题的唯一幂等键（传入相同键可恢复已有研究进度）"), "question": str("完整保留所有技术约束条件的研究问题"), "product": str("精确限定检索的产品型号名称"), "multi_agent": boolean("是否请求独立证据子智能体协同检索（默认 false）")}, "request_key", "question")}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		if k.research == nil {
			return tool.Errorf("research coordinator is not configured on this server"), nil
		}
		var req struct {
			Key      string `json:"request_key"`
			Question string `json:"question"`
			Product  string `json:"product"`
			Multi    bool   `json:"multi_agent"`
		}
		if json.Unmarshal(in, &req) != nil {
			return tool.Errorf("invalid input"), nil
		}
		q := db.KnowledgeQuery{}
		if req.Product != "" {
			q.MustTerms = []string{req.Product}
		}
		v, err := k.research.Run(ctx, k.tenant, k.taskID, "worker", retrieval.ResearchRequest{Key: req.Key, Question: req.Question, Query: q, Multi: req.Multi})
		if err != nil {
			return tool.Errorf("%v", err), nil
		}
		return tool.JSON(v), nil
	})
}

func (k *Kit) retrievalTools() []tool.CoreTool {
	return []tool.CoreTool{k.kbReadEvidenceTool(), k.kbVerifyEvidenceTool(), k.kbResearchTool()}
}
func (k *Kit) ownRun(ctx context.Context, id int64) error {
	run, err := k.pg.GetRetrievalRun(ctx, k.tenant, id)
	if err != nil {
		return err
	}
	if run.TaskID == nil || *run.TaskID != k.taskID {
		return db.ErrNotFound
	}
	return nil
}
