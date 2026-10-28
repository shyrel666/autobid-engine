package db

// 任务推演报告：覆盖率、招标要素清单（含核验状态与原文出处）、全部产出物及其事实证据链。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type ReportAnchor struct {
	AssetID     int64           `json:"asset_id"`
	Type        string          `json:"type"`
	LotNo       string          `json:"lot_no,omitempty"`
	Code        string          `json:"code,omitempty"`
	Title       string          `json:"title,omitempty"`
	IsMandatory bool            `json:"is_mandatory,omitempty"`
	SourceRef   json.RawMessage `json:"source_ref,omitempty"`
}

type ReportEvidence struct {
	NodeID    int64           `json:"node_id"`
	Kind      string          `json:"kind"`
	State     string          `json:"state"`
	Summary   string          `json:"summary"`
	Evidence  string          `json:"evidence,omitempty"`
	SourceRef json.RawMessage `json:"source_ref,omitempty"`
	Owner     string          `json:"owner,omitempty"`
	// Inherited 为 true 表示证据来自直接来源任务的推演空间。
	Inherited bool           `json:"inherited,omitempty"`
	Anchors   []ReportAnchor `json:"anchors,omitempty"`
}

type ReportFinding struct {
	*Finding
	SourceRefs []json.RawMessage `json:"source_refs,omitempty"`
	Anchors    []ReportAnchor    `json:"anchors,omitempty"`
	Evidence   []ReportEvidence  `json:"evidence"`
}

type ReportElement struct {
	ReportAnchor
	Category   string   `json:"category,omitempty"`
	ScoreMax   *float64 `json:"score_max,omitempty"`
	Amount     *string  `json:"amount,omitempty"`
	Quantity   *string  `json:"quantity,omitempty"`
	Unit       string   `json:"unit,omitempty"`
	Verified   bool     `json:"verified"`
	FactIDs    []int64  `json:"fact_ids,omitempty"`
	FindingIDs []int64  `json:"finding_ids,omitempty"`
}

type ReportSummary struct {
	Documents         int            `json:"documents"`
	Elements          int            `json:"elements"`
	ElementsByType    map[string]int `json:"elements_by_type"`
	MandatoryElements int            `json:"mandatory_elements"`
	MandatoryVerified int            `json:"mandatory_verified"`
	Facts             int            `json:"confirmed_facts"`
	Findings          int            `json:"findings"`
	BySeverity        map[string]int `json:"findings_by_severity"`
	ByCategory        map[string]int `json:"findings_by_category"`
	GoalsTotal        int            `json:"goals_total"`
	GoalsMet          int            `json:"goals_met"`
}

type TaskReport struct {
	Task        *Task             `json:"task"`
	Goal        string            `json:"goal"`
	GeneratedAt time.Time         `json:"generated_at"`
	Coverage    Coverage          `json:"coverage"`
	Summary     ReportSummary     `json:"summary"`
	Documents   []*TenderDocument `json:"documents"`
	Elements    []ReportElement   `json:"elements"`
	Findings    []ReportFinding   `json:"findings"`
}

// TaskReport 汇总任务的结构化推演报告。读操作不在同一快照内执行，报告反映生成时刻的近似一致状态。
func (d *DB) TaskReport(ctx context.Context, taskID int64) (*TaskReport, error) {
	task, err := d.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	rep := &TaskReport{Task: task, GeneratedAt: time.Now().UTC(), Documents: []*TenderDocument{}, Elements: []ReportElement{}, Findings: []ReportFinding{}}
	rep.Summary.ElementsByType, rep.Summary.BySeverity, rep.Summary.ByCategory = map[string]int{}, map[string]int{}, map[string]int{}
	if err := d.QueryRowContext(ctx, `SELECT goal FROM explorations WHERE id = $1`, task.ExplorationID).Scan(&rep.Goal); err != nil {
		return nil, err
	}
	es := NewExplorationStore(d, task.ExplorationID)
	if rep.Summary.GoalsTotal, rep.Summary.GoalsMet, err = es.GoalStats(ctx); err != nil {
		return nil, err
	}
	if rep.Coverage, err = d.TaskCoverageWithSources(ctx, taskID); err != nil {
		return nil, err
	}
	docs, err := d.ListTenderDocuments(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if docs != nil {
		rep.Documents = docs
	}
	rep.Summary.Documents = len(docs)

	// 要素清单：声明范围内的全部要素 + 本推演空间 confirmed Fact / Finding 对它们的锚定。
	assets, err := NewAssetStore(d, taskID).ListScopedAssets(ctx, nil, 5000)
	if err != nil {
		return nil, err
	}
	factsOf, findingsOf, err := d.anchoredNodeIDs(ctx, task.ExplorationID)
	if err != nil {
		return nil, err
	}
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM exploration_nodes
		WHERE exploration_id = $1 AND kind = 'fact' AND state = 'confirmed'`, task.ExplorationID).Scan(&rep.Summary.Facts); err != nil {
		return nil, err
	}
	for _, a := range assets {
		el := ReportElement{ReportAnchor: anchorOf(a), Category: a.Category, ScoreMax: a.ScoreMax, Amount: a.Amount,
			Quantity: a.Quantity, Unit: a.Unit, FactIDs: factsOf[a.ID], FindingIDs: findingsOf[a.ID]}
		el.Verified = len(el.FactIDs) > 0
		rep.Summary.Elements++
		rep.Summary.ElementsByType[a.Type]++
		if a.IsMandatory {
			rep.Summary.MandatoryElements++
			if el.Verified {
				rep.Summary.MandatoryVerified++
			}
		}
		rep.Elements = append(rep.Elements, el)
	}

	// 产出物与证据链：证据节点可来自直接来源任务，按 id 直接读取。
	findings, err := d.ListFindings(ctx, taskID, "")
	if err != nil {
		return nil, err
	}
	var nodeIDs []int64
	for _, f := range findings {
		nodeIDs = append(nodeIDs, f.NodeID)
		nodeIDs = append(nodeIDs, f.EvidenceID...)
	}
	nodes, err := d.nodesByID(ctx, nodeIDs)
	if err != nil {
		return nil, err
	}
	var assetIDs []int64
	for _, n := range nodes {
		assetIDs = append(assetIDs, n.Anchors...)
	}
	anchorAssets, err := d.assetsByID(ctx, assetIDs)
	if err != nil {
		return nil, err
	}
	anchors := func(ids []int64) []ReportAnchor {
		var out []ReportAnchor
		for _, id := range ids {
			if a, ok := anchorAssets[id]; ok {
				out = append(out, anchorOf(a))
			}
		}
		return out
	}
	for _, f := range findings {
		rf := ReportFinding{Finding: f, Evidence: []ReportEvidence{}}
		var detail struct {
			SourceRefs []json.RawMessage `json:"source_refs"`
		}
		_ = json.Unmarshal(f.Payload, &detail)
		rf.SourceRefs = detail.SourceRefs
		if n, ok := nodes[f.NodeID]; ok {
			rf.Anchors = anchors(n.Anchors)
		}
		for _, ev := range f.EvidenceID {
			n, ok := nodes[ev]
			if !ok {
				continue
			}
			var p struct {
				Evidence  string          `json:"evidence"`
				SourceRef json.RawMessage `json:"source_ref"`
			}
			_ = json.Unmarshal(n.Payload, &p)
			rf.Evidence = append(rf.Evidence, ReportEvidence{NodeID: n.ID, Kind: n.Kind, State: n.State, Summary: NodeSummary(n, 0),
				Evidence: p.Evidence, SourceRef: p.SourceRef, Owner: n.Owner, Inherited: n.ExplorationID != task.ExplorationID,
				Anchors: anchors(n.Anchors)})
		}
		rep.Summary.Findings++
		rep.Summary.BySeverity[f.Severity]++
		rep.Summary.ByCategory[f.Category]++
		rep.Findings = append(rep.Findings, rf)
	}
	return rep, nil
}

func anchorOf(a *Asset) ReportAnchor {
	ref := a.SourceRef
	if string(ref) == "{}" {
		ref = nil
	}
	return ReportAnchor{AssetID: a.ID, Type: a.Type, LotNo: a.LotNo, Code: a.Code, Title: a.Title, IsMandatory: a.IsMandatory, SourceRef: ref}
}

// anchoredNodeIDs 返回本推演空间中 confirmed Fact / Finding 节点按锚定实体分组的 id。
func (d *DB) anchoredNodeIDs(ctx context.Context, expID int64) (facts, findings map[int64][]int64, err error) {
	facts, findings = map[int64][]int64{}, map[int64][]int64{}
	rows, err := d.QueryContext(ctx, `SELECT an.asset_id, n.id, n.kind
		FROM exploration_anchors an JOIN exploration_nodes n ON n.id = an.node_id
		WHERE n.exploration_id = $1 AND n.kind IN ('fact','finding') AND n.state = 'confirmed'
		ORDER BY an.asset_id, n.id`, expID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			assetID, nodeID int64
			kind            string
		)
		if err := rows.Scan(&assetID, &nodeID, &kind); err != nil {
			return nil, nil, err
		}
		if kind == KindFact {
			facts[assetID] = append(facts[assetID], nodeID)
		} else {
			findings[assetID] = append(findings[assetID], nodeID)
		}
	}
	return facts, findings, rows.Err()
}

// nodesByID 按 id 读取节点（不限推演空间）及其锚点。
func (d *DB) nodesByID(ctx context.Context, ids []int64) (map[int64]*Node, error) {
	out := map[int64]*Node{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.QueryContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		out[n.ID] = n
	}
	arows, err := d.QueryContext(ctx, `SELECT node_id, asset_id FROM exploration_anchors
		WHERE node_id = ANY($1::bigint[]) ORDER BY node_id, asset_id`, ids)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var nodeID, assetID int64
		if err := arows.Scan(&nodeID, &assetID); err != nil {
			return nil, err
		}
		if n, ok := out[nodeID]; ok {
			n.Anchors = append(n.Anchors, assetID)
		}
	}
	return out, arows.Err()
}

func (d *DB) assetsByID(ctx context.Context, ids []int64) (map[int64]*Asset, error) {
	out := map[int64]*Asset{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.QueryContext(ctx, `SELECT `+assetCols+` FROM assets WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, fmt.Errorf("scan asset: %w", err)
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}
