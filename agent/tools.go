package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
)

const (
	AgentPlanner = "planner"
	AgentWorker  = "worker"
)

// 图工具名。
const (
	ToolGraphOverview = "graph_overview"
	ToolNodeDetail    = "node_detail"
	ToolListAssets    = "list_assets"
	ToolAddIntent     = "add_intent"
	ToolProveGoal     = "prove_goal"
	ToolKillWork      = "kill_work"
	ToolSteerWork     = "steer_work"
	ToolInsertAssets  = "insert_assets"
	ToolLinkAssets    = "link_assets"
	ToolRecordFact    = "record_fact"
	ToolReportFinding = "report_finding"
	ToolCompactNodes  = "compact_nodes"
	ToolConsumeHint   = "consume_hint"
	ToolAddGoal       = "add_goal"
)

// WorkerWritebackTools 是 Worker 收尾阶段的写回白名单（Patch ②）；list_assets 用于查实体 ID 以便锚定。
var WorkerWritebackTools = []string{ToolInsertAssets, ToolLinkAssets, ToolRecordFact, ToolReportFinding, ToolListAssets}

type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// ToolSet 是一次 Plan / Execute 的图工具集合与本轮写入计数。计数不是互斥锁、去重账本或写回验证器。
type ToolSet struct {
	as        *db.AssetStore
	ts        *db.ExplorationStore
	worker    string
	taskID    int64
	ownerNode int64 // Worker 当前 Intent；Planner 为 0
	round     int64 // 当前规划轮次（compact_nodes 写入 Digest）

	killWork      func(intentID int64) error
	steerWork     func(intentID int64, msg string) error
	notifyFinding func(intentID int64, summary string)

	mu      sync.Mutex
	goalMet bool
	writes  WriteCounts
}

func newToolSet(as *db.AssetStore, ts *db.ExplorationStore, worker string, taskID, owner int64) *ToolSet {
	return &ToolSet{as: as, ts: ts, worker: worker, taskID: taskID, ownerNode: owner}
}

func (s *ToolSet) GoalMet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goalMet
}

func (s *ToolSet) Writes() WriteCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func (s *ToolSet) count(fn func(*WriteCounts)) {
	s.mu.Lock()
	fn(&s.writes)
	s.mu.Unlock()
}

// plannerTools 返回 Planner 工具；final 为超时终局轮，硬移除投放与干预工具，只允许核验与总结。
func (s *ToolSet) plannerTools(todos *tool.TodoStore, final bool) []tool.CoreTool {
	tools := []tool.CoreTool{s.graphOverviewTool(true), s.nodeDetailTool(), s.listAssetsTool(), s.proveGoalTool()}
	if final {
		return tools
	}
	tools = append(tools, s.addIntentTool(), s.addGoalTool(), s.consumeHintTool(), s.compactNodesTool(), tool.NewTodoWriteTool(todos))
	if s.killWork != nil {
		tools = append(tools, s.killWorkTool())
	}
	if s.steerWork != nil {
		tools = append(tools, s.steerWorkTool())
	}
	return tools
}

func (s *ToolSet) workerTools() []tool.CoreTool {
	return []tool.CoreTool{
		s.graphOverviewTool(false), s.nodeDetailTool(), s.listAssetsTool(),
		s.insertAssetsTool(), s.linkAssetsTool(), s.recordFactTool(), s.reportFindingTool(),
		s.consumeHintTool(), s.addGoalTool(),
	}
}

// ---------------------------------------------------------------------------
// schema helpers
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
func ids(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": desc}
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

func writeTool(spec tool.Spec, fn func(ctx context.Context, in json.RawMessage) (tool.Result, error)) tool.CoreTool {
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		return fn(ctx, in)
	})
}

// summaryOf 从 payload 取 summary（回落 title），截断到 max 个字符。
func summaryOf(n *db.Node, max int) string { return db.NodeSummary(n, max) }

func truncate(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// ---------------------------------------------------------------------------
// 读工具
// ---------------------------------------------------------------------------

// overview 读取折叠后的有界态势（db.ExplorationStore.Overview）；Worker 视图不含覆盖率块（§1.6）。
func (s *ToolSet) overview(ctx context.Context, withCoverage bool) (*db.Overview, error) {
	o, err := s.ts.Overview(ctx)
	if err != nil {
		return nil, err
	}
	if withCoverage && s.as != nil {
		if c, err := s.as.Coverage(ctx); err == nil {
			o.Coverage = &c
		}
	}
	return o, nil
}

func (s *ToolSet) graphOverviewTool(withCoverage bool) tool.CoreTool {
	return readTool(tool.Spec{
		Name:        ToolGraphOverview,
		Description: "Bounded snapshot of the task's reasoning graph: goals, hints, open/running/recent intents, recent facts and findings.",
		Schema:      obj(map[string]any{}),
	}, func(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
		o, err := s.overview(ctx, withCoverage)
		if err != nil {
			return tool.Result{}, err
		}
		return tool.JSON(o), nil
	})
}

func (s *ToolSet) nodeDetailTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name:        ToolNodeDetail,
		Description: "Full payload, anchors and edges of one reasoning-graph node.",
		Schema:      obj(map[string]any{"id": integer("node id")}, "id"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			ID int64 `json:"id"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		n, err := s.ts.GetNode(ctx, req.ID)
		if errors.Is(err, db.ErrNotFound) {
			return tool.Errorf("node %d not found in this task", req.ID), nil
		}
		if err != nil {
			return tool.Result{}, err
		}
		edges, err := s.ts.NodeEdges(ctx, req.ID, 200)
		if err != nil {
			return tool.Result{}, err
		}
		return tool.JSON(map[string]any{"node": n, "edges": edges}), nil
	})
}

func (s *ToolSet) listAssetsTool() tool.CoreTool {
	return readTool(tool.Spec{
		Name: ToolListAssets,
		Description: "List tender-document elements (tender, lot, clause, qualification, scoring_item, commercial_item) " +
			"within this task's declared scope. Use the returned ids as anchors.",
		Schema: obj(map[string]any{
			"types": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"limit": integer("max rows, default 200"),
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Types []string `json:"types"`
			Limit int      `json:"limit"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		if req.Limit <= 0 || req.Limit > 1000 {
			req.Limit = 200
		}
		assets, err := s.as.ListScopedAssets(ctx, req.Types, req.Limit)
		if err != nil {
			return tool.Result{}, err
		}
		type row struct {
			ID          int64    `json:"id"`
			Type        string   `json:"type"`
			TenderNo    string   `json:"tender_no"`
			LotNo       string   `json:"lot_no,omitempty"`
			Code        string   `json:"code,omitempty"`
			Title       string   `json:"title,omitempty"`
			IsMandatory bool     `json:"is_mandatory,omitempty"`
			ScoreMax    *float64 `json:"score_max,omitempty"`
			Amount      *string  `json:"amount,omitempty"`
		}
		rows := make([]row, 0, len(assets))
		for _, a := range assets {
			rows = append(rows, row{a.ID, a.Type, a.TenderNo, a.LotNo, a.Code, truncate(a.Title, 120), a.IsMandatory, a.ScoreMax, a.Amount})
		}
		return tool.JSON(rows), nil
	})
}

// ---------------------------------------------------------------------------
// Planner 写工具
// ---------------------------------------------------------------------------

func (s *ToolSet) addIntentTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name: ToolAddIntent,
		Description: "Dispatch one bounded, executable intent to workers (e.g. verify one qualification requirement against our credentials). " +
			"parents are the existing fact/finding ids this intent builds on.",
		Schema: obj(map[string]any{
			"summary":   str("what the worker must verify or produce, concrete and bounded"),
			"rationale": str("why this is the next step"),
			"priority":  integer("1-10, default 5"),
			"parents":   ids("fact/finding ids this intent is derived from"),
			"anchors":   ids("asset ids this intent concerns"),
		}, "summary"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Summary   string  `json:"summary"`
			Rationale string  `json:"rationale"`
			Priority  int     `json:"priority"`
			Parents   []int64 `json:"parents"`
			Anchors   []int64 `json:"anchors"`
		}](in)
		if err != nil || strings.TrimSpace(req.Summary) == "" {
			return tool.Errorf("summary is required"), nil
		}
		if req.Priority < 0 || req.Priority > 10 {
			return tool.Errorf("priority must be within 1-10"), nil
		}
		id, err := s.ts.AddIntentTx(ctx, db.AddIntentInput{
			Payload:  map[string]any{"summary": req.Summary, "rationale": req.Rationale},
			Priority: req.Priority,
			Parents:  req.Parents,
			Anchors:  req.Anchors,
		})
		if err != nil {
			return tool.Errorf("add_intent rejected: %v", err), nil
		}
		return tool.JSON(map[string]any{"intent_id": id}), nil
	})
}

func (s *ToolSet) proveGoalTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name:        ToolProveGoal,
		Description: "Mark a goal as met, citing one fact or finding as evidence. Only call when the evidence is sufficient.",
		Schema:      obj(map[string]any{"goal_id": integer("goal node id"), "evidence_id": integer("fact/finding node id")}, "goal_id", "evidence_id"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			GoalID     int64 `json:"goal_id"`
			EvidenceID int64 `json:"evidence_id"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		allMet, total, err := s.ts.ProveGoalTx(ctx, req.GoalID, req.EvidenceID)
		if err != nil {
			return tool.Errorf("prove_goal rejected: %v", err), nil
		}
		if allMet {
			s.mu.Lock()
			s.goalMet = true
			s.mu.Unlock()
		}
		return tool.JSON(map[string]any{"goal_id": req.GoalID, "all_goals_met": allMet, "total_goals": total}), nil
	})
}

func (s *ToolSet) killWorkTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name:        ToolKillWork,
		Description: "Stop a running intent that is no longer useful. The intent becomes stopped and can be reopened later.",
		Schema:      obj(map[string]any{"intent_id": integer("running intent id"), "reason": str("why")}, "intent_id"),
	}, func(_ context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			IntentID int64 `json:"intent_id"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		if err := s.killWork(req.IntentID); err != nil {
			return tool.Errorf("kill_work: %v", err), nil
		}
		return tool.Text(fmt.Sprintf("intent %d is being stopped", req.IntentID)), nil
	})
}

func (s *ToolSet) steerWorkTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name:        ToolSteerWork,
		Description: "Send a correction to a running worker; it is delivered before the worker's next tool call.",
		Schema:      obj(map[string]any{"intent_id": integer("running intent id"), "message": str("instruction")}, "intent_id", "message"),
	}, func(_ context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			IntentID int64  `json:"intent_id"`
			Message  string `json:"message"`
		}](in)
		if err != nil || strings.TrimSpace(req.Message) == "" {
			return tool.Errorf("intent_id and message are required"), nil
		}
		if err := s.steerWork(req.IntentID, req.Message); err != nil {
			return tool.Errorf("steer_work: %v", err), nil
		}
		return tool.Text("steering queued"), nil
	})
}

// ---------------------------------------------------------------------------
// Worker 写工具
// ---------------------------------------------------------------------------

type assetInput struct {
	Type        string          `json:"type"`
	TenderNo    string          `json:"tender_no"`
	LotNo       string          `json:"lot_no"`
	Code        string          `json:"code"`
	Title       string          `json:"title"`
	Content     string          `json:"content"`
	Category    string          `json:"category"`
	IsMandatory bool            `json:"is_mandatory"`
	ScoreMax    *float64        `json:"score_max"`
	Amount      json.Number     `json:"amount"`
	Currency    string          `json:"currency"`
	Quantity    json.Number     `json:"quantity"`
	Unit        string          `json:"unit"`
	SourceRef   json.RawMessage `json:"source_ref"`
}

func decimalPtr(n json.Number) *string {
	if n == "" {
		return nil
	}
	s := n.String()
	return &s
}

func (s *ToolSet) insertAssetsTool() tool.CoreTool {
	item := obj(map[string]any{
		"type":         map[string]any{"type": "string", "enum": []string{db.AssetTender, db.AssetLot, db.AssetClause, db.AssetQualification, db.AssetScoringItem, db.AssetCommercialItem}},
		"tender_no":    str("tender number (招标编号)"),
		"lot_no":       str("lot number; empty when the element applies to all lots"),
		"code":         str("element code inside the document, required except for tender/lot"),
		"title":        str("short title"),
		"content":      str("verbatim or condensed text"),
		"category":     str("free-form category, e.g. 技术/商务/资格"),
		"is_mandatory": map[string]any{"type": "boolean", "description": "★ substantive clause / disqualification item"},
		"score_max":    number("max score of a scoring item"),
		"amount":       number("amount for commercial items"),
		"currency":     str("currency code"),
		"quantity":     number("quantity"),
		"unit":         str("unit"),
		"source_ref":   map[string]any{"type": "object", "description": "where it appears, e.g. {\"file\":\"...\",\"page\":12}"},
	}, "type", "tender_no")
	return writeTool(tool.Spec{
		Name: ToolInsertAssets,
		Description: "Upsert tender-document elements into the shared static graph. Elements are deduplicated by natural key; " +
			"they are anchored to your current intent. Returns one result per item.",
		Schema: obj(map[string]any{"assets": map[string]any{"type": "array", "items": item}}, "assets"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		dec := json.NewDecoder(strings.NewReader(string(in)))
		dec.UseNumber()
		var req struct {
			Assets []assetInput `json:"assets"`
		}
		if err := dec.Decode(&req); err != nil || len(req.Assets) == 0 {
			return tool.Errorf("assets must be a non-empty array: %v", err), nil
		}
		type result struct {
			Index   int    `json:"index"`
			ID      int64  `json:"id,omitempty"`
			Created bool   `json:"created,omitempty"`
			Error   string `json:"error,omitempty"`
		}
		out := make([]result, 0, len(req.Assets))
		failed := 0
		for i, a := range req.Assets {
			id, created, err := s.as.UpsertAsset(ctx, &db.Asset{
				Type: a.Type, TenderNo: a.TenderNo, LotNo: a.LotNo, Code: a.Code, Title: a.Title, Content: a.Content,
				Category: a.Category, IsMandatory: a.IsMandatory, ScoreMax: a.ScoreMax, Amount: decimalPtr(a.Amount),
				Currency: a.Currency, Quantity: decimalPtr(a.Quantity), Unit: a.Unit, SourceRef: a.SourceRef,
			}, s.ownerNode)
			if err != nil {
				failed++
				out = append(out, result{Index: i, Error: err.Error()})
				continue
			}
			s.count(func(w *WriteCounts) { w.Assets++ })
			out = append(out, result{Index: i, ID: id, Created: created})
		}
		res := tool.JSON(out)
		res.IsError = failed == len(req.Assets)
		return res, nil
	})
}

func (s *ToolSet) linkAssetsTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name: ToolLinkAssets,
		Description: "Record a directed relation between two tender elements, e.g. lot -requires-> qualification, " +
			"lot -scored_by-> scoring_item, clause -conflicts_with-> clause.",
		Schema: obj(map[string]any{
			"src_asset_id": integer("source asset id"),
			"rel":          str("contains | requires | scored_by | priced_by | references | conflicts_with | ..."),
			"dst_asset_id": integer("target asset id"),
			"weight":       number("default 1.0"),
			"props":        map[string]any{"type": "object"},
		}, "src_asset_id", "rel", "dst_asset_id"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Src    int64          `json:"src_asset_id"`
			Rel    string         `json:"rel"`
			Dst    int64          `json:"dst_asset_id"`
			Weight *float64       `json:"weight"`
			Props  map[string]any `json:"props"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		weight := 1.0
		if req.Weight != nil {
			weight = *req.Weight
		}
		var src *int64
		if s.ownerNode > 0 {
			owner := s.ownerNode
			src = &owner
		}
		id, err := s.as.UpsertEntityEdge(ctx, req.Src, req.Rel, req.Dst, s.taskID, src, weight, req.Props)
		if err != nil {
			return tool.Errorf("link_assets rejected: %v", err), nil
		}
		return tool.JSON(map[string]any{"edge_id": id}), nil
	})
}

func (s *ToolSet) recordFactTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name: ToolRecordFact,
		Description: "Record one verified fact from your work (positive or negative), anchored to the tender elements it concerns. " +
			"It is linked to your current intent.",
		Schema: obj(map[string]any{
			"summary":    str("the fact, one or two sentences"),
			"evidence":   str("quote or location supporting the fact"),
			"confidence": number("0-1"),
			"anchors":    ids("asset ids"),
		}, "summary"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Summary    string   `json:"summary"`
			Evidence   string   `json:"evidence"`
			Confidence *float64 `json:"confidence"`
			Anchors    []int64  `json:"anchors"`
		}](in)
		if err != nil || strings.TrimSpace(req.Summary) == "" {
			return tool.Errorf("summary is required"), nil
		}
		if req.Confidence != nil && (*req.Confidence < 0 || *req.Confidence > 1) {
			return tool.Errorf("confidence must be within 0-1"), nil
		}
		payload := map[string]any{"summary": req.Summary, "evidence": req.Evidence}
		if req.Confidence != nil {
			payload["confidence"] = *req.Confidence
		}
		// v2：默认归属当前 Intent，保证 yields 血缘完整（原仓不回落 ownerNode）。
		id, err := s.ts.RecordFactWithYieldsTx(ctx, s.ownerNode, payload, 0, s.worker, req.Anchors)
		if err != nil {
			return tool.Errorf("record_fact rejected: %v", err), nil
		}
		s.count(func(w *WriteCounts) { w.Facts++ })
		return tool.JSON(map[string]any{"fact_id": id}), nil
	})
}

func (s *ToolSet) reportFindingTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name: ToolReportFinding,
		Description: "Report a confirmed deliverable conclusion: compliance risk, disqualification risk, scoring strategy, pricing or bid strategy. " +
			"Cite the facts it rests on as evidence.",
		Schema: obj(map[string]any{
			"category": map[string]any{"type": "string", "enum": []string{db.FindingCompliance, db.FindingRisk, db.FindingScoring, db.FindingPricing, db.FindingStrategy}},
			"severity": map[string]any{"type": "string", "enum": []string{db.SeverityInfo, db.SeverityLow, db.SeverityMedium, db.SeverityHigh, db.SeverityCritical}},
			"title":    str("one-line conclusion"),
			"summary":  str("explanation and recommended action"),
			"detail":   map[string]any{"type": "object"},
			"evidence": ids("fact/finding ids supporting it"),
			"anchors":  ids("asset ids"),
		}, "category", "title"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Category string          `json:"category"`
			Severity string          `json:"severity"`
			Title    string          `json:"title"`
			Summary  string          `json:"summary"`
			Detail   json.RawMessage `json:"detail"`
			Evidence []int64         `json:"evidence"`
			Anchors  []int64         `json:"anchors"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		rec, err := s.ts.RecordFinding(ctx, db.RecordFindingInput{
			TaskID: s.taskID, IntentID: s.ownerNode, Category: req.Category, Severity: req.Severity,
			Title: req.Title, Summary: req.Summary, Detail: req.Detail, Evidence: req.Evidence,
			Anchors: req.Anchors, Worker: s.worker,
		})
		if err != nil {
			return tool.Errorf("report_finding rejected: %v", err), nil
		}
		s.count(func(w *WriteCounts) { w.Findings++ })
		if s.notifyFinding != nil {
			s.notifyFinding(s.ownerNode, req.Title)
		}
		return tool.JSON(map[string]any{"finding_id": rec.FindingID, "node_id": rec.NodeID}), nil
	})
}

// ---------------------------------------------------------------------------
// 上下文压缩、Hint 与目标
// ---------------------------------------------------------------------------

func (s *ToolSet) compactNodesTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name: ToolCompactNodes,
		Description: "Fold settled nodes (finished intents, facts, findings, consumed hints — typically from cold_index) into one digest " +
			"to keep the overview small. Lossless: members stay in the graph and remain reachable via node_detail. " +
			"A digest whose members later change is shown as stale and expanded again.",
		Schema: obj(map[string]any{
			"title":      str("short title of what the folded nodes established"),
			"body":       str("the conclusions you need to remember from these nodes, with key ids"),
			"member_ids": ids("node ids to fold"),
		}, "title", "body", "member_ids"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Title     string  `json:"title"`
			Body      string  `json:"body"`
			MemberIDs []int64 `json:"member_ids"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		id, superseded, err := s.ts.CreateDigest(ctx, strings.TrimSpace(req.Title), req.Body, req.MemberIDs, s.round)
		if err != nil {
			return tool.Errorf("compact_nodes rejected: %v", err), nil
		}
		return tool.JSON(map[string]any{"digest_id": id, "superseded_digests": superseded, "folded": len(req.MemberIDs)}), nil
	})
}

func (s *ToolSet) consumeHintTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name:        ToolConsumeHint,
		Description: "Mark an operator hint as consumed once you have acted on it, so it stops appearing as active.",
		Schema:      obj(map[string]any{"hint_id": integer("hint node id")}, "hint_id"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			HintID int64 `json:"hint_id"`
		}](in)
		if err != nil {
			return tool.Errorf("invalid input: %v", err), nil
		}
		ok, err := s.ts.ConsumeHint(ctx, req.HintID)
		if err != nil {
			return tool.Result{}, err
		}
		if !ok {
			return tool.Errorf("hint %d is not an active hint of this task", req.HintID), nil
		}
		return tool.Text(fmt.Sprintf("hint %d consumed", req.HintID)), nil
	})
}

func (s *ToolSet) addGoalTool() tool.CoreTool {
	return writeTool(tool.Spec{
		Name: ToolAddGoal,
		Description: "Add a new task goal when the tender documents reveal an objective that must be verified separately " +
			"(e.g. a newly found disqualification clause). Do not restate existing goals.",
		Schema: obj(map[string]any{"summary": str("the goal, verifiable and specific")}, "summary"),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		req, err := decode[struct {
			Summary string `json:"summary"`
		}](in)
		if err != nil || strings.TrimSpace(req.Summary) == "" {
			return tool.Errorf("summary is required"), nil
		}
		id, err := s.ts.AddGoalTx(ctx, map[string]any{"summary": req.Summary, "added_by": s.worker}, nil)
		if err != nil {
			return tool.Errorf("add_goal rejected: %v", err), nil
		}
		return tool.JSON(map[string]any{"goal_id": id}), nil
	})
}
