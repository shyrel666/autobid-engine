package bidtools

import (
	"context"
	"encoding/json"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/retrieval"
)

func (k *Kit) WithResearch(c *retrieval.Coordinator) *Kit { k.research = c; return k }
func (k *Kit) retrievalTools() []tool.CoreTool {
	if k.retrieval == nil {
		return nil
	}
	list := []tool.CoreTool{readTool(tool.Spec{Name: "kb_read_evidence", Description: "Read evidence snapshots and current validity for a retrieval run belonging to this task. Relevant hits are unverified until explicitly checked.", Schema: obj(map[string]any{"run_id": integer("run id")}, "run_id")}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
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
	}), readTool(tool.Spec{Name: "kb_verify_evidence", Description: "Verify a structured numeric claim against original evidence, exact product metadata, units and validity. Conditional/ambiguous claims remain insufficient.", Schema: obj(map[string]any{
		"run_id": integer("run id"), "evidence_id": integer("evidence id"), "claim": obj(map[string]any{"text": str("full claim"), "parameter": str("exact property name"), "operator": str("<=, >= or ="), "value": number("threshold"), "unit": str("unit"), "product": str("exact model"), "version": str("exact version"), "as_of": str("YYYY-MM-DD"), "conditions": strs("additional conditions; requires review")}, "text", "parameter", "operator", "value", "unit")}, "run_id", "evidence_id", "claim")}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		var req struct {
			RunID      int64           `json:"run_id"`
			EvidenceID int64           `json:"evidence_id"`
			Claim      retrieval.Claim `json:"claim"`
		}
		if json.Unmarshal(in, &req) != nil {
			return tool.Errorf("invalid input"), nil
		}
		if err := k.ownRun(ctx, req.RunID); err != nil {
			return kbErr(err)
		}
		v, err := k.retrieval.Verify(ctx, k.tenant, "worker", req.RunID, req.EvidenceID, req.Claim)
		if err != nil {
			return kbErr(err)
		}
		return tool.JSON(v), nil
	})}
	if k.research != nil {
		list = append(list, readTool(tool.Spec{Name: "kb_research", Description: "Bounded evidence research. Idempotent request_key, shared budget and up to two follow-ups. Multi-agent mode is off unless enabled after evaluation. Returns durable run/evidence IDs and explicit gaps.", Schema: obj(map[string]any{"request_key": str("unique key for this task/question; reuse to resume"), "question": str("question preserving all constraints"), "product": str("exact model filter"), "multi_agent": boolean("request independent evidence workers; default false")}, "request_key", "question")}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
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
		}))
	}
	return list
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
