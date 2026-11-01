package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 标准 ARTEX RFP 目标分解器根节点编码。
const (
	GoalCodeVeto      = "G-VETO"
	GoalCodeDeviation = "G-DEVIATION"
	GoalCodeScoring   = "G-SCORING"
	GoalCodeOutline   = "G-OUTLINE"
)

// RFPGoalNodeSummary 描述由目标分解器幂等创建或同步的一个图谱 Goal 根节点。
type RFPGoalNodeSummary struct {
	GoalCode string  `json:"goal_code"`
	NodeID   int64   `json:"node_id"`
	Summary  string  `json:"summary"`
	Anchors  []int64 `json:"anchors"`
	Created  bool    `json:"created"`
}

// RFPGoalDecomposition 汇总一次 RFP 目标分解器（BootstrapRFPGoals）的跨四域执行产出。
type RFPGoalDecomposition struct {
	ExtractResult  *ExtractResult       `json:"extract_result"`
	SyncedAssets   int                  `json:"synced_assets"`
	Goals          []RFPGoalNodeSummary `json:"goals"`
	OutlineID      *int64               `json:"outline_id,omitempty"`
	OutlineVersion int                  `json:"outline_version,omitempty"`
	OutlineCreated bool                 `json:"outline_created"`
}

// BootstrapRFPGoals 实现 ARTEX 架构中的 Goals（目标分解器）：
//  1. 域 C：调用 ExtractRequirements 从招标文件中结构化拆解出否决项（资格/符合性/法定）、评分标准（含分档规则）、
//     ★实质性条款与技术偏离要求、投标文件格式要求；
//  2. 域 A：将尚未关联静态实体的要求条目幂等同步至 assets 表（scoring_item / qualification / clause），
//     自动纳入任务范围并回填 requirement_items.source_asset_id；
//  3. 域 B：在任务 exploration_nodes 中按 goal_code 幂等初始化 4 类标准图谱 Goal 根节点（G-VETO / G-DEVIATION /
//     G-SCORING / G-OUTLINE），从 origin Fact 连出 spawns 边并挂载对应 asset_id 锚点；
//  4. 域 C（当 initOutline=true 且尚无大纲时）：自动生成 v1 draft 标书整体目录大纲并分配章节要求映射（保留人工确认门禁）。
func (d *DB) BootstrapRFPGoals(ctx context.Context, tenant string, taskID int64, actor string, initOutline bool) (*RFPGoalDecomposition, error) {
	tenant = tenantOr(tenant)
	if strings.TrimSpace(actor) == "" {
		actor = "goals-decomposer"
	}
	ext, err := d.ExtractRequirements(ctx, tenant, taskID, actor)
	if err != nil {
		return nil, err
	}
	out := &RFPGoalDecomposition{
		ExtractResult: ext,
		Goals:         []RFPGoalNodeSummary{},
	}

	var expID int64
	if err := d.QueryRowContext(ctx, `SELECT exploration_id FROM tasks WHERE id = $1`, taskID).Scan(&expID); err != nil {
		return nil, fmt.Errorf("lookup task %d exploration: %w", taskID, err)
	}
	tenderNo, defaultLot, rootAssets, err := d.taskPrimaryTenderMeta(ctx, tenant, taskID)
	if err != nil {
		return nil, err
	}
	if tenderNo == "" {
		tenderNo = fmt.Sprintf("TASK-%d", taskID)
	}

	reqs, err := d.listActiveRequirementsForGoals(ctx, tenant, taskID)
	if err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return out, nil
	}

	as := NewAssetStore(d, taskID)
	var (
		vetoAnchors      []int64
		deviationAnchors []int64
		scoringAnchors   []int64
		outlineAnchors   = append([]int64{}, rootAssets...)
		vetoCount        int
		mandatoryCount   int
		obligationCount  int
		scoringCount     int
		totalScore       float64
	)

	for _, r := range reqs {
		assetID := int64(0)
		if r.SourceAssetID != nil && *r.SourceAssetID > 0 {
			assetID = *r.SourceAssetID
		} else {
			aType := requirementToAssetType(r)
			lot := r.Lot
			if lot == "" {
				lot = defaultLot
			}
			var scorePtr *float64
			if r.ScoreWeight > 0 {
				v := r.ScoreWeight
				scorePtr = &v
			}
			id, created, uErr := as.UpsertAsset(ctx, &Asset{
				Type:        aType,
				TenderNo:    tenderNo,
				LotNo:       lot,
				Code:        r.Code,
				Title:       r.Title,
				Content:     r.Description,
				Category:    r.Category,
				IsMandatory: r.IsMandatory || r.Category == ReqVetoItem,
				ScoreMax:    scorePtr,
				SourceRef:   r.SourceLoc,
			}, 0)
			if uErr != nil {
				return nil, fmt.Errorf("sync asset for requirement %s: %w", r.Code, uErr)
			}
			assetID = id
			if created {
				out.SyncedAssets++
			}
			if _, err := d.ExecContext(ctx, `UPDATE requirement_items SET source_asset_id = $4 WHERE tenant_id = $1 AND task_id = $2 AND id = $3`,
				tenant, taskID, r.ID, assetID); err != nil {
				return nil, err
			}
		}

		switch r.Category {
		case ReqVetoItem:
			vetoCount++
			if assetID > 0 {
				vetoAnchors = append(vetoAnchors, assetID)
			}
		case ReqMandatoryClause:
			mandatoryCount++
			if assetID > 0 {
				deviationAnchors = append(deviationAnchors, assetID)
			}
		case ReqNonScoringRequired:
			obligationCount++
			if assetID > 0 {
				deviationAnchors = append(deviationAnchors, assetID)
			}
		case ReqScoringItem:
			scoringCount++
			totalScore += r.ScoreWeight
			if assetID > 0 {
				scoringAnchors = append(scoringAnchors, assetID)
			}
		case ReqFormatAttachment:
			if assetID > 0 {
				outlineAnchors = append(outlineAnchors, assetID)
			}
		}
	}

	type goalSpec struct {
		code      string
		summary   string
		rationale string
		anchors   []int64
	}
	specs := []goalSpec{
		{
			code:      GoalCodeVeto,
			summary:   fmt.Sprintf("【否决项红线核验】完成全部资格性、符合性及法定否决项（共 %d 项）的 100%% 逐条合规核验与证明材料对齐，杜绝废标风险", vetoCount),
			rationale: "由 Goals 目标分解器从招标文件审查表及废标条款自动拆解生成",
			anchors:   uniqueIDs(vetoAnchors),
		},
		{
			code:      GoalCodeDeviation,
			summary:   fmt.Sprintf("【★实质性条款与技术偏离表响应】完成 %d 项 ★ 强制性条款与 %d 项采购技术要求的正/零偏离逐条响应及证据核实", mandatoryCount, obligationCount),
			rationale: "由 Goals 目标分解器从招标文件实质性条款与采购需求章节自动拆解生成",
			anchors:   uniqueIDs(deviationAnchors),
		},
		{
			code:      GoalCodeScoring,
			summary:   fmt.Sprintf("【评分标准拆解与高分策略】针对 %d 项评分因素（累计 %.1f 分）完成分档夺分策略推演、企业知识库证据检索核验与绑定", scoringCount, totalScore),
			rationale: "由 Goals 目标分解器从评标办法及评分标准表自动拆解生成",
			anchors:   uniqueIDs(scoringAnchors),
		},
		{
			code:      GoalCodeOutline,
			summary:   "【标书整体目录大纲与章节协同】对齐 S1~S6 技术标整体目录大纲、商务承诺台账与投标文件格式要求，按章节 Mandate 推进编制与全文一致性审查",
			rationale: "由 Goals 目标分解器初始化标书全局结构根节点",
			anchors:   uniqueIDs(outlineAnchors),
		},
	}

	es := NewExplorationStore(d, expID)
	for _, sp := range specs {
		gs, err := es.upsertStructuredGoalTx(ctx, sp.code, sp.summary, sp.rationale, sp.anchors)
		if err != nil {
			return nil, err
		}
		out.Goals = append(out.Goals, gs)
	}

	if initOutline {
		ol, err := d.ListOutlines(ctx, tenant, taskID)
		if err == nil && len(ol.Outlines) == 0 {
			det, cErr := d.CreateOutline(ctx, tenant, taskID, actor, OutlineInput{
				Note: "Goals 目标分解器自动生成 v1 技术标目录大纲草案",
			})
			if cErr == nil && det != nil && det.Outline != nil {
				out.OutlineID = &det.Outline.ID
				out.OutlineVersion = det.Outline.Version
				out.OutlineCreated = true
			}
		}
	}
	return out, nil
}

func requirementToAssetType(r *RequirementItem) string {
	switch r.Category {
	case ReqScoringItem:
		return AssetScoringItem
	case ReqVetoItem:
		if r.VetoKind == VetoQualification {
			return AssetQualification
		}
		return AssetClause
	default:
		return AssetClause
	}
}

func (d *DB) taskPrimaryTenderMeta(ctx context.Context, tenant string, taskID int64) (tenderNo, lotNo string, rootAssets []int64, err error) {
	_ = d.QueryRowContext(ctx, `SELECT tender_no, lot_no FROM tender_documents
		WHERE tenant_id = $1 AND task_id = $2 ORDER BY id LIMIT 1`, tenant, taskID).Scan(&tenderNo, &lotNo)
	rows, qErr := d.QueryContext(ctx, `SELECT a.id, a.tender_no, a.lot_no FROM assets a
		WHERE a.type IN ('tender', 'lot')
		  AND (a.task_ids @> ARRAY[$1::bigint] OR EXISTS (SELECT 1 FROM task_asset_links l WHERE l.task_id = $1 AND l.asset_id = a.id))
		ORDER BY a.id`, taskID)
	if qErr != nil {
		return tenderNo, lotNo, nil, qErr
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var tNo, lNo string
		if err := rows.Scan(&id, &tNo, &lNo); err != nil {
			return tenderNo, lotNo, nil, err
		}
		rootAssets = append(rootAssets, id)
		if tenderNo == "" && tNo != "" {
			tenderNo = tNo
		}
		if lotNo == "" && lNo != "" {
			lotNo = lNo
		}
	}
	return tenderNo, lotNo, rootAssets, rows.Err()
}

func (d *DB) listActiveRequirementsForGoals(ctx context.Context, tenant string, taskID int64) ([]*RequirementItem, error) {
	var all []*RequirementItem
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		var err error
		all, err = listRequirementsTx(ctx, tx, tenant, taskID)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]*RequirementItem, 0, len(all))
	for _, r := range all {
		if r.IsSuperseded() || r.OutOfBid {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// upsertStructuredGoalTx 在 exploration_nodes 中按 payload->>'goal_code' 幂等创建或更新结构化 Goal 根节点。
func (s *ExplorationStore) upsertStructuredGoalTx(ctx context.Context, code, summary, rationale string, anchors []int64) (RFPGoalNodeSummary, error) {
	res := RFPGoalNodeSummary{
		GoalCode: code,
		Summary:  summary,
		Anchors:  anchors,
	}
	payload := map[string]any{
		"goal_code": code,
		"summary":   summary,
		"rationale": rationale,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return res, err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var nodeID int64
		qErr := tx.QueryRowContext(ctx, `
			SELECT id FROM exploration_nodes
			WHERE exploration_id = $1 AND kind = 'goal' AND payload->>'goal_code' = $2
			ORDER BY id LIMIT 1 FOR UPDATE`, s.expID, code).Scan(&nodeID)
		switch {
		case errors.Is(qErr, sql.ErrNoRows):
			id, err := s.insertNodeTx(ctx, tx, KindGoal, StateGoalOpen, json.RawMessage(raw), 0, "goals-decomposer", anchors)
			if err != nil {
				return err
			}
			origin, err := originFactID(ctx, tx, s.expID)
			if err != nil {
				return err
			}
			if origin > 0 {
				if err := s.linkTx(ctx, tx, origin, RelSpawns, id); err != nil {
					return err
				}
			}
			res.NodeID = id
			res.Created = true
			return nil
		case qErr != nil:
			return qErr
		default:
			if _, err := tx.ExecContext(ctx, `
				UPDATE exploration_nodes
				SET payload = exploration_nodes.payload || $3::jsonb,
				    updated_at = now()
				WHERE id = $1 AND exploration_id = $2`, nodeID, s.expID, string(raw)); err != nil {
				return err
			}
			if err := anchorTx(ctx, tx, nodeID, anchors); err != nil {
				return err
			}
			res.NodeID = nodeID
			res.Created = false
			return nil
		}
	})
	return res, err
}
