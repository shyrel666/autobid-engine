package db

// Phase 6A：导出就绪度判定（只读）。6B 的导出门禁在锁定版本快照的同一事务内调用 exportReadinessTx，
// 判定逻辑只在这里维护，HTTP 层与导出模块都不得另行实现。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
)

// 阻断原因类型。
const (
	BlockNoApprovedOutline     = "no_approved_outline"
	BlockManifest              = "requirements_not_confirmed"
	BlockUnmapped              = "requirement_unmapped"
	BlockSectionUnapproved     = "section_not_approved"
	BlockMatrixStale           = "matrix_stale"
	BlockMatrixFailed          = "matrix_failed"
	BlockMatrixUnverified      = "matrix_unverified"
	BlockConsistencyMissing    = "consistency_not_run"
	BlockConsistencyStale      = "consistency_stale"
	BlockFindingOpen           = "finding_open"
	BlockEvidenceInvalid       = "evidence_invalid"
	BlockVetoPending           = "veto_pending"                // 否决项尚未核对
	BlockVetoFailed            = "veto_failed"                 // 否决项核对不通过：投标将被判无效
	BlockNoBid                 = "no_bid_decision"             // 投/不投决策为放弃投标
	BlockRequirementSuperseded = "requirement_superseded"      // 章节仍映射已被更正/澄清覆盖的旧要求
	BlockSourcePending         = "requirements_source_changed" // 招标原文已变更，要求尚未按当前来源版本重新提取

	// D2 持久化阻断：只在任务已启用 D2（存在绑定 / 可采性 / 台账 / 报价）时生效，不改变 M1 基线行为。
	BlockAdmissibilityMissing      = "admissibility_missing"
	BlockAdmissibilityStale        = "admissibility_stale"
	BlockAdmissibilityManual       = "admissibility_manual"
	BlockAdmissibilityInadmissible = "admissibility_inadmissible"
	BlockObjectiveEvidenceMissing  = "objective_evidence_missing"
	BlockObjectiveManual           = "objective_manual"
	BlockCommitmentStale           = "commitment_stale"
	BlockCommitmentUnconfirmed     = "commitment_unconfirmed"
	BlockQuoteUnconfirmed          = "quote_unconfirmed"
	BlockQuoteInconsistent         = "quote_inconsistent"
	BlockQuoteOverCeiling          = "quote_over_ceiling"

	// D3 持久化阻断：只在任务已启用 D3（存在故事板或写作核验记录）时生效，不改变 M1/D2 基线行为。
	BlockStoryboardMissing         = "storyboard_missing"
	BlockStoryboardUnapproved      = "storyboard_unapproved"
	BlockStoryboardStale           = "storyboard_stale"
	BlockUngroundedClaim           = "ungrounded_claim"
	BlockWritingVerificationStale  = "writing_verification_stale"
	BlockWritingVerificationFailed = "writing_verification_failed"
	BlockSectionSelfApproved       = "section_self_approved"
)

// ExportBlocker 是一条阻止导出的原因。
type ExportBlocker struct {
	Kind            string `json:"kind"`
	Message         string `json:"message"`
	SectionID       int64  `json:"section_id,omitempty"`
	SectionCode     string `json:"section_code,omitempty"`
	RequirementID   int64  `json:"requirement_item_id,omitempty"`
	RequirementCode string `json:"requirement_code,omitempty"`
	FindingID       int64  `json:"finding_id,omitempty"`
	EvidenceID      int64  `json:"evidence_id,omitempty"`
}

type ExportReadiness struct {
	Materials      []*TaskMaterial `json:"materials,omitempty"`
	Ready          bool            `json:"ready"`
	OutlineVersion int             `json:"outline_version"`
	ConsistencyRun int64           `json:"consistency_run_id,omitempty"`
	CheckedAt      time.Time       `json:"checked_at"`
	Counts         map[string]int  `json:"counts"`
	Blockers       []ExportBlocker `json:"blockers"`
}

func (r *ExportReadiness) block(b ExportBlocker) {
	r.Blockers = append(r.Blockers, b)
	r.Counts[b.Kind]++
}

// ExportReadiness 返回任务当前是否可导出以及全部阻断原因（单一快照读取）。
func (d *DB) ExportReadiness(ctx context.Context, tenant string, taskID int64) (*ExportReadiness, error) {
	tenant = tenantOr(tenant)
	var out *ExportReadiness
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = exportReadinessTx(ctx, tx, tenant, taskID)
		return err
	})
	return out, err
}

// exportReadinessTx 汇总阻断原因：无批准大纲 / 清单未确认、要求未映射、章节未批准当前修订、矩阵行 stale / failed / 未核验、
// 一致性核验未运行 / 已过期 / 有 open 结论、当前修订或要求引用的证据失效（stale / 缺失 / 矛盾）。
func exportReadinessTx(ctx context.Context, q queryer, tenant string, taskID int64) (*ExportReadiness, error) {
	r := &ExportReadiness{CheckedAt: time.Now().UTC(), Counts: map[string]int{}, Blockers: []ExportBlocker{}}
	materials, err := listTaskMaterials(ctx, q, tenant, taskID)
	if err != nil {
		return nil, err
	}
	r.Materials = materials
	for _, material := range materials {
		if material.Required && material.State != "complete" {
			r.block(ExportBlocker{Kind: "required_material_incomplete", Message: fmt.Sprintf("必需材料 #%d %s：%s", material.ID, material.Name, material.StateLabel())})
		}
	}
	m, err := ensureManifestTx(ctx, q, tenant, taskID, false)
	if err != nil {
		return nil, err
	}
	if m.BidDecision == BidDecisionNoBid {
		r.block(ExportBlocker{Kind: BlockNoBid, Message: fmt.Sprintf("任务已决策不投标（no_bid）：%s", m.BidDecisionNote)})
	}
	// 独立于清单状态：即使清单被其他路径改回 confirmed，只要当前来源版本尚未重新提取，就阻断导出。
	if m.SourceEpoch > m.ExtractedSourceEpoch {
		r.block(ExportBlocker{Kind: BlockSourcePending,
			Message: fmt.Sprintf("招标原文已变更（source epoch %d > extracted %d），需重新提取并复核要求", m.SourceEpoch, m.ExtractedSourceEpoch)})
	}
	var outlineID int64
	var epoch int64
	err = q.QueryRowContext(ctx, `SELECT id, version, requirements_epoch FROM proposal_outlines
		WHERE tenant_id = $1 AND task_id = $2 AND status = 'approved'`, tenant, taskID).Scan(&outlineID, &r.OutlineVersion, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		r.block(ExportBlocker{Kind: BlockNoApprovedOutline, Message: "任务没有已批准的技术标大纲"})
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if m.Status != ManifestConfirmed || m.Epoch != epoch {
		r.block(ExportBlocker{Kind: BlockManifest, Message: fmt.Sprintf("要求清单状态 %s（epoch %d），批准大纲依据 epoch %d", m.Status, m.Epoch, epoch)})
	}

	// 否决项：本次投标分包内的每一项都须核对通过，待核对与不通过都阻断导出（已被后发文件覆盖的旧否决项除外）。
	vrows, err := q.QueryContext(ctx, `SELECT id, code, title, veto_status FROM requirement_items
		WHERE tenant_id = $1 AND task_id = $2 AND category = 'veto_item' AND NOT out_of_bid
		  AND superseded_by_id IS NULL AND review_status <> 'stale' AND veto_status <> 'passed' ORDER BY code`, tenant, taskID)
	if err != nil {
		return nil, err
	}
	for vrows.Next() {
		var id int64
		var code, title, status string
		if err := vrows.Scan(&id, &code, &title, &status); err != nil {
			vrows.Close()
			return nil, err
		}
		if status == VetoFailed {
			r.block(ExportBlocker{Kind: BlockVetoFailed, RequirementID: id, RequirementCode: code, Message: fmt.Sprintf("否决项 %s %s 核对不通过，投标将被判无效", code, title)})
		} else {
			r.block(ExportBlocker{Kind: BlockVetoPending, RequirementID: id, RequirementCode: code, Message: fmt.Sprintf("否决项 %s %s 尚未核对", code, title)})
		}
	}
	vrows.Close()
	if err := vrows.Err(); err != nil {
		return nil, err
	}

	// 已被更正/澄清覆盖的旧要求不得仍挂在批准大纲的章节下。
	srows, err := q.QueryContext(ctx, `SELECT s.id, s.section_code, r.id, r.code, r.title
		FROM proposal_section_requirements m
		JOIN proposal_sections s ON s.tenant_id = m.tenant_id AND s.id = m.section_id
		JOIN requirement_items r ON r.tenant_id = m.tenant_id AND r.id = m.requirement_id
		WHERE m.tenant_id = $1 AND s.outline_id = $2 AND (r.superseded_by_id IS NOT NULL OR r.review_status = 'stale')
		ORDER BY s.seq, r.code`, tenant, outlineID)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var sid, rid int64
		var scode, rcode, rtitle string
		if err := srows.Scan(&sid, &scode, &rid, &rcode, &rtitle); err != nil {
			srows.Close()
			return nil, err
		}
		r.block(ExportBlocker{Kind: BlockRequirementSuperseded, SectionID: sid, SectionCode: scode, RequirementID: rid, RequirementCode: rcode,
			Message: fmt.Sprintf("章节 %s 仍映射已被更正/澄清覆盖的旧要求 %s %s", scode, rcode, rtitle)})
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}

	// 章节：须批准当前主版本。
	rows, err := q.QueryContext(ctx, `SELECT id, section_code, title, status, current_rev_no, approved_rev_no FROM proposal_sections
		WHERE tenant_id = $1 AND outline_id = $2 ORDER BY seq`, tenant, outlineID)
	if err != nil {
		return nil, err
	}
	var secIDs []int64
	for rows.Next() {
		var id int64
		var code, title, status string
		var cur int
		var appr sql.NullInt32
		if err := rows.Scan(&id, &code, &title, &status, &cur, &appr); err != nil {
			rows.Close()
			return nil, err
		}
		secIDs = append(secIDs, id)
		switch {
		case cur == 0:
			r.block(ExportBlocker{Kind: BlockSectionUnapproved, SectionID: id, SectionCode: code, Message: fmt.Sprintf("章节 %s %s 尚无正文", code, title)})
		case !appr.Valid || int(appr.Int32) != cur || status != SectionApproved:
			r.block(ExportBlocker{Kind: BlockSectionUnapproved, SectionID: id, SectionCode: code,
				Message: fmt.Sprintf("章节 %s %s 的当前修订 rev %d 未批准（状态 %s）", code, title, cur, status)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 两层覆盖。
	cov, err := complianceCoverageTx(ctx, q, tenant, taskID)
	if err != nil {
		return nil, err
	}
	for _, rc := range cov.Requirements {
		b := ExportBlocker{RequirementID: rc.ID, RequirementCode: rc.Code}
		switch rc.Status {
		case compliance.StatusVerified:
			continue
		case "unmapped":
			b.Kind, b.Message = BlockUnmapped, fmt.Sprintf("要求 %s %s 未映射到批准大纲的章节", rc.Code, rc.Title)
		case compliance.StatusStale:
			b.Kind, b.Message = BlockMatrixStale, fmt.Sprintf("要求 %s 的响应核验已失效：%s", rc.Code, rc.Reason)
		case compliance.StatusFailed:
			b.Kind, b.Message = BlockMatrixFailed, fmt.Sprintf("要求 %s 的正文响应核验未通过：%s", rc.Code, rc.Reason)
		default:
			b.Kind, b.Message = BlockMatrixUnverified, fmt.Sprintf("要求 %s 尚未完成正文响应核验", rc.Code)
		}
		r.block(b)
	}

	// 一致性核验：最新运行须为当前快照且无 open 结论。
	det, err := consistencyRunTx(ctx, q, tenant, taskID, 0, "")
	switch {
	case errors.Is(err, ErrNotFound):
		r.block(ExportBlocker{Kind: BlockConsistencyMissing, Message: "尚未执行全文一致性核验"})
	case err != nil:
		return nil, err
	case !det.Current:
		r.ConsistencyRun = det.Run.ID
		reason := det.Run.StaleReason
		if reason == "" {
			reason = "快照与当前章节修订不一致"
		}
		r.block(ExportBlocker{Kind: BlockConsistencyStale, Message: "最近一次全文一致性核验已过期：" + reason})
	default:
		r.ConsistencyRun = det.Run.ID
		for _, f := range det.Findings {
			if f.Status != FindingOpen {
				continue
			}
			b := ExportBlocker{Kind: BlockFindingOpen, FindingID: f.ID, Message: f.Detail}
			if f.RequirementID != nil {
				b.RequirementID, b.RequirementCode = *f.RequirementID, f.RequirementCode
			}
			if f.ASectionID != nil {
				b.SectionID, b.SectionCode = *f.ASectionID, f.ASectionCode
			}
			r.block(b)
		}
	}

	// 证据：当前修订与要求引用的证据须仍有效（stale / 缺失 / 矛盾均阻断；未核验证据已作为修订缺口记录，不单独阻断）。
	evOwner := map[int64]ExportBlocker{}
	var evIDs []int64
	revs, err := currentRevisionsTx(ctx, q, tenant, secIDs)
	if err != nil {
		return nil, err
	}
	codes := map[int64]string{}
	crow, err := q.QueryContext(ctx, `SELECT id, section_code FROM proposal_sections WHERE tenant_id = $1 AND outline_id = $2`, tenant, outlineID)
	if err != nil {
		return nil, err
	}
	for crow.Next() {
		var id int64
		var c string
		if err := crow.Scan(&id, &c); err != nil {
			crow.Close()
			return nil, err
		}
		codes[id] = c
	}
	crow.Close()
	for sid, rev := range revs {
		for _, id := range rev.evidence {
			evIDs = append(evIDs, id)
			if _, ok := evOwner[id]; !ok {
				evOwner[id] = ExportBlocker{SectionID: sid, SectionCode: codes[sid]}
			}
		}
	}
	reqs, err := listRequirementsTx(ctx, q, tenant, taskID)
	if err != nil {
		return nil, err
	}
	for _, it := range reqs {
		for _, id := range it.EvidenceIDs {
			evIDs = append(evIDs, id)
			if _, ok := evOwner[id]; !ok {
				evOwner[id] = ExportBlocker{RequirementID: it.ID, RequirementCode: it.Code}
			}
		}
	}
	states, err := evidenceStateListTx(ctx, q, tenant, taskID, evIDs)
	if err != nil {
		return nil, err
	}
	for _, e := range states {
		if e.Status != "stale" && !e.Missing && e.Relation != "contradicts" {
			continue
		}
		b := evOwner[e.ID]
		b.Kind, b.EvidenceID = BlockEvidenceInvalid, e.ID
		what := "已失效"
		if e.Relation == "contradicts" && e.Status != "stale" {
			what = "与承诺矛盾"
		}
		b.Message = fmt.Sprintf("证据 %d %s：%s", e.ID, what, e.Reason)
		r.block(b)
	}
	if err := d2ReadinessTx(ctx, q, tenant, taskID, outlineID, r); err != nil {
		return nil, err
	}
	if err := d3ReadinessTx(ctx, q, tenant, taskID, outlineID, r); err != nil {
		return nil, err
	}
	r.Ready = len(r.Blockers) == 0
	return r, nil
}

// d2ReadinessTx 汇总 D2 持久化阻断。只有任务已启用 D2 时才检查，M1 基线任务保持原行为。
func d2ReadinessTx(ctx context.Context, q queryer, tenant string, taskID, outlineID int64, r *ExportReadiness) error {
	active, err := taskHasD2ActivityQ(ctx, q, tenant, taskID)
	if err != nil {
		return err
	}
	if !active {
		return nil
	}
	// 1) 有证明规则或客观评分的当前要求，必须有当前可采性结论；manual / inadmissible 不得当作通过。
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.code, r.title, r.eval_kind,
			(SELECT run.verdict FROM admissibility_runs run
			  WHERE run.tenant_id=r.tenant_id AND run.task_id=r.task_id AND run.requirement_id=r.id AND run.status='current' LIMIT 1),
			(SELECT run.status FROM admissibility_runs run
			  WHERE run.tenant_id=r.tenant_id AND run.task_id=r.task_id AND run.requirement_id=r.id
			  ORDER BY run.version DESC LIMIT 1),
			(SELECT run.reasons::text FROM admissibility_runs run
			  WHERE run.tenant_id=r.tenant_id AND run.task_id=r.task_id AND run.requirement_id=r.id AND run.status='current' LIMIT 1)
		FROM requirement_items r
		WHERE r.tenant_id=$1 AND r.task_id=$2 AND NOT r.out_of_bid AND r.superseded_by_id IS NULL AND r.review_status <> 'stale'
		  AND r.category IN ('scoring_item','mandatory_clause','non_scoring_required')
		  AND ((r.eval_kind='objective' AND r.score_group <> 'price') OR r.proof_rules <> '{}'::jsonb)
		ORDER BY r.code`, tenant, taskID)
	if err != nil {
		return err
	}
	blockedReq := map[int64]bool{}
	for rows.Next() {
		var id int64
		var code, title, evalKind string
		var verdict, latest, reasons sql.NullString
		if err := rows.Scan(&id, &code, &title, &evalKind, &verdict, &latest, &reasons); err != nil {
			rows.Close()
			return err
		}
		b := ExportBlocker{RequirementID: id, RequirementCode: code}
		switch {
		case !verdict.Valid && !latest.Valid:
			b.Kind = BlockAdmissibilityMissing
			b.Message = fmt.Sprintf("要求 %s %s 尚未执行证据可采性评估", code, title)
		case !verdict.Valid:
			b.Kind = BlockAdmissibilityStale
			b.Message = fmt.Sprintf("要求 %s %s 的可采性结论已失效（%s），需重评", code, title, latest.String)
		case verdict.String == compliance.Manual:
			b.Kind = BlockAdmissibilityManual
			b.Message = fmt.Sprintf("要求 %s %s 的可采性结论为待人工复核（manual），不能当作通过", code, title)
		case verdict.String == compliance.Inadmissible:
			if evalKind == EvalObjective && reasons.Valid &&
				(strings.Contains(reasons.String, "\"min_count\"") || strings.Contains(reasons.String, "min_count")) {
				b.Kind = BlockObjectiveEvidenceMissing
				b.Message = fmt.Sprintf("客观评分项 %s %s 的可采证据数量不足", code, title)
			} else {
				b.Kind = BlockAdmissibilityInadmissible
				b.Message = fmt.Sprintf("要求 %s %s 的证据不可采", code, title)
			}
		default:
			continue
		}
		blockedReq[id] = true
		r.block(b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// 2) 章节引用的当前明细与台账：运行失效、台账未确认或台账变更导致章节降级均阻断。
	secRows, err := q.QueryContext(ctx, `SELECT s.id, s.section_code, s.status, rv.status, rv.id,
			EXISTS(SELECT 1 FROM section_revision_evidence sre
			       JOIN admissibility_items i ON i.tenant_id=sre.tenant_id AND i.id=sre.admissibility_item_id
			       JOIN admissibility_runs run ON run.tenant_id=i.tenant_id AND run.id=i.run_id
			       WHERE sre.tenant_id=s.tenant_id AND sre.section_rev_id=rv.id
			         AND (run.status <> 'current' OR i.verdict <> 'admissible'))
		FROM proposal_sections s
		JOIN section_revisions rv ON rv.tenant_id=s.tenant_id AND rv.section_id=s.id AND rv.rev_no=s.current_rev_no
		WHERE s.tenant_id=$1 AND s.outline_id=$2 ORDER BY s.seq`, tenant, outlineID)
	if err != nil {
		return err
	}
	type secRef struct {
		id, revID int64
		code      string
	}
	var secs []secRef
	for secRows.Next() {
		var s ExportBlocker
		var secStatus, revStatus string
		var revID int64
		var badRefs bool
		if err := secRows.Scan(&s.SectionID, &s.SectionCode, &secStatus, &revStatus, &revID, &badRefs); err != nil {
			secRows.Close()
			return err
		}
		secs = append(secs, secRef{id: s.SectionID, revID: revID, code: s.SectionCode})
		if secStatus == SectionStaleCommitment || revStatus == RevStaleCommitment {
			b := s
			b.Kind = BlockCommitmentStale
			b.Message = fmt.Sprintf("章节 %s 引用的承诺台账已变更，需重新渲染并批准", s.SectionCode)
			r.block(b)
		}
		if badRefs {
			b := s
			b.Kind = BlockAdmissibilityStale
			b.Message = fmt.Sprintf("章节 %s 引用的可采性明细已失效或不属于当前运行", s.SectionCode)
			r.block(b)
		}
	}
	secRows.Close()
	if err := secRows.Err(); err != nil {
		return err
	}

	for _, s := range secs {
		var raw []byte
		if err := q.QueryRowContext(ctx, `SELECT to_jsonb(commitment_ids) FROM section_revisions WHERE tenant_id=$1 AND id=$2`,
			tenant, s.revID).Scan(&raw); err != nil {
			return err
		}
		ids := int64sJSON(raw)
		for _, id := range ids {
			var code, status string
			err := q.QueryRowContext(ctx, `SELECT code, status FROM commitment_ledger WHERE tenant_id=$1 AND id=$2`, tenant, id).Scan(&code, &status)
			if errors.Is(err, sql.ErrNoRows) {
				r.block(ExportBlocker{Kind: BlockCommitmentUnconfirmed, SectionID: s.id, SectionCode: s.code,
					Message: fmt.Sprintf("章节 %s 引用的承诺台账 %d 不存在", s.code, id)})
				continue
			}
			if err != nil {
				return err
			}
			if status != CommitmentConfirmed && status != CommitmentSignedOff {
				r.block(ExportBlocker{Kind: BlockCommitmentUnconfirmed, SectionID: s.id, SectionCode: s.code,
					Message: fmt.Sprintf("章节 %s 引用的承诺 %s 状态为 %s", s.code, code, status)})
			}
		}
	}

	// 3) 客观分：最新计算仍为 manual（规则无法解析）阻断；证据不足已由要求级运行给出。
	objRows, err := q.QueryContext(ctx, `SELECT o.requirement_id, req.code, o.status, o.gaps::text
		FROM objective_score_runs o JOIN requirement_items req ON req.tenant_id=o.tenant_id AND req.task_id=o.task_id AND req.id=o.requirement_id
		WHERE o.tenant_id=$1 AND o.task_id=$2
		  AND o.id=(SELECT MAX(x.id) FROM objective_score_runs x WHERE x.tenant_id=o.tenant_id AND x.task_id=o.task_id AND x.requirement_id=o.requirement_id)
		ORDER BY req.code`, tenant, taskID)
	if err != nil {
		return err
	}
	for objRows.Next() {
		var reqID int64
		var code, status, gaps string
		if err := objRows.Scan(&reqID, &code, &status, &gaps); err != nil {
			objRows.Close()
			return err
		}
		if status == "manual" && strings.Contains(gaps, "score_rule_unparsed") {
			r.block(ExportBlocker{Kind: BlockObjectiveManual, RequirementID: reqID, RequirementCode: code,
				Message: fmt.Sprintf("客观评分项 %s 的评分规则无法解析，需人工复核", code)})
		}
		if status == "manual" && strings.Contains(gaps, "admissibility_run_missing") && !blockedReq[reqID] {
			r.block(ExportBlocker{Kind: BlockAdmissibilityMissing, RequirementID: reqID, RequirementCode: code,
				Message: fmt.Sprintf("客观评分项 %s 尚未执行可采性评估", code)})
		}
	}
	objRows.Close()
	if err := objRows.Err(); err != nil {
		return err
	}

	// 4) 报价：每个分包必须有已确认版本，且内部勾稽、限价与价格类承诺一致。
	lotRows, err := q.QueryContext(ctx, `SELECT lot FROM bid_quotes WHERE tenant_id=$1 AND task_id=$2 GROUP BY lot ORDER BY lot`, tenant, taskID)
	if err != nil {
		return err
	}
	var lots []string
	for lotRows.Next() {
		var lot string
		if err := lotRows.Scan(&lot); err != nil {
			lotRows.Close()
			return err
		}
		lots = append(lots, lot)
	}
	lotRows.Close()
	if err := lotRows.Err(); err != nil {
		return err
	}
	for _, lot := range lots {
		var quoteID int64
		err := q.QueryRowContext(ctx, `SELECT id FROM bid_quotes WHERE tenant_id=$1 AND task_id=$2 AND lot=$3 AND status='confirmed'
			ORDER BY version DESC LIMIT 1`, tenant, taskID, lot).Scan(&quoteID)
		if errors.Is(err, sql.ErrNoRows) {
			r.block(ExportBlocker{Kind: BlockQuoteUnconfirmed, Message: fmt.Sprintf("分包 %q 尚无已确认报价", lot)})
			continue
		}
		if err != nil {
			return err
		}
		val, err := validateBidQuoteQ(ctx, q, tenant, taskID, quoteID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				r.block(ExportBlocker{Kind: BlockQuoteUnconfirmed, Message: fmt.Sprintf("分包 %q 的已确认报价不可读", lot)})
				continue
			}
			return err
		}
		if !val.OK {
			over := false
			for _, c := range val.Checks {
				if c.Status == "fail" && c.Rule == "ceiling" {
					over = true
				}
			}
			kind := BlockQuoteInconsistent
			if over {
				kind = BlockQuoteOverCeiling
			}
			r.block(ExportBlocker{Kind: kind, Message: fmt.Sprintf("分包 %q 的报价 v%d 未通过内部勾稽 / 限价校验", lot, val.Version)})
		}
	}
	return nil
}

func taskHasD2ActivityQ(ctx context.Context, q queryer, tenant string, taskID int64) (bool, error) {
	var active bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requirement_evidence_bindings WHERE tenant_id=$1 AND task_id=$2)
		OR EXISTS(SELECT 1 FROM admissibility_runs WHERE tenant_id=$1 AND task_id=$2)
		OR EXISTS(SELECT 1 FROM commitment_ledger WHERE tenant_id=$1 AND task_id=$2)
		OR EXISTS(SELECT 1 FROM bid_quotes WHERE tenant_id=$1 AND task_id=$2)`, tenant, taskID).Scan(&active)
	return active, err
}

// d3ReadinessTx 汇总 D3 故事板粉队评审与句子级写作核验持久化阻断（仅在任务已启用 D3 时生效）。
func d3ReadinessTx(ctx context.Context, q queryer, tenant string, taskID, outlineID int64, r *ExportReadiness) error {
	active, err := taskHasD3ActivityQ(ctx, q, tenant, taskID)
	if err != nil || !active {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT s.id, s.section_code, s.title, s.current_rev_no,
			sb.status, COALESCE(sb.stale_reason,''),
			rv.id, COALESCE(rv.created_by,''), COALESCE(rv.approved_by,''),
			wv.verdict, COALESCE(wv.stale_reason,''), COALESCE(wv.ungrounded_claim_count, 0), COALESCE(wv.rule_version,'')
		FROM proposal_sections s
		LEFT JOIN section_storyboards sb
		  ON sb.tenant_id = s.tenant_id AND sb.section_id = s.id AND sb.status <> 'superseded'
		LEFT JOIN section_revisions rv
		  ON rv.tenant_id = s.tenant_id AND rv.section_id = s.id AND rv.rev_no = s.current_rev_no
		LEFT JOIN section_writing_verifications wv
		  ON wv.tenant_id = s.tenant_id AND wv.section_rev_id = rv.id
		WHERE s.tenant_id = $1 AND s.outline_id = $2
		ORDER BY s.seq`, tenant, outlineID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			secID          int64
			secCode, title string
			curRevNo       int
			sbStatus       sql.NullString
			sbStale        string
			revID          sql.NullInt64
			revCreatedBy   string
			revApprovedBy  string
			wvVerdict      sql.NullString
			wvStale        string
			ungrounded     int
			wvRule         string
		)
		if err := rows.Scan(&secID, &secCode, &title, &curRevNo, &sbStatus, &sbStale, &revID, &revCreatedBy, &revApprovedBy, &wvVerdict, &wvStale, &ungrounded, &wvRule); err != nil {
			return err
		}
		switch {
		case !sbStatus.Valid:
			r.block(ExportBlocker{Kind: BlockStoryboardMissing, SectionID: secID, SectionCode: secCode,
				Message: fmt.Sprintf("章节 %s %s 尚未创建 D3 故事板", secCode, title)})
		case sbStatus.String == StoryboardStale:
			r.block(ExportBlocker{Kind: BlockStoryboardStale, SectionID: secID, SectionCode: secCode,
				Message: fmt.Sprintf("章节 %s %s 的故事板已失效（%s），需更新并重新通过粉队评审", secCode, title, sbStale)})
		case sbStatus.String != StoryboardPinkApproved:
			r.block(ExportBlocker{Kind: BlockStoryboardUnapproved, SectionID: secID, SectionCode: secCode,
				Message: fmt.Sprintf("章节 %s %s 的故事板状态为 %s，尚未通过粉队评审", secCode, title, sbStatus.String)})
		}
		if curRevNo > 0 && revID.Valid {
			if revApprovedBy != "" && revApprovedBy == revCreatedBy {
				r.block(ExportBlocker{Kind: BlockSectionSelfApproved, SectionID: secID, SectionCode: secCode,
					Message: fmt.Sprintf("章节 %s %s 的红队审批人与作者相同（%s），违反职责分离", secCode, title, revApprovedBy)})
			}
			switch {
			case !wvVerdict.Valid:
				r.block(ExportBlocker{Kind: BlockWritingVerificationFailed, SectionID: secID, SectionCode: secCode,
					Message: fmt.Sprintf("章节 %s %s 当前修订 rev %d 尚未完成 D3 写作核验", secCode, title, curRevNo)})
			case wvVerdict.String == compliance.WritingVerdictStale:
				r.block(ExportBlocker{Kind: BlockWritingVerificationStale, SectionID: secID, SectionCode: secCode,
					Message: fmt.Sprintf("章节 %s %s 的写作核验已失效（%s），需重新核验", secCode, title, wvStale)})
			case wvRule != compliance.WritingVerificationRuleVersion:
				// 旧规则的"通过"不能沿用：规则升级可能是为了堵住旧规则的漏检。
				r.block(ExportBlocker{Kind: BlockWritingVerificationStale, SectionID: secID, SectionCode: secCode,
					Message: fmt.Sprintf("章节 %s %s 的写作核验按旧规则 %s 完成，当前规则为 %s，需重新核验", secCode, title, wvRule, compliance.WritingVerificationRuleVersion)})
			case ungrounded > 0:
				r.block(ExportBlocker{Kind: BlockUngroundedClaim, SectionID: secID, SectionCode: secCode,
					Message: fmt.Sprintf("章节 %s %s 存在 %d 条未绑定有效证据或承诺台账的断言", secCode, title, ungrounded)})
			case wvVerdict.String != compliance.WritingVerdictPassed:
				r.block(ExportBlocker{Kind: BlockWritingVerificationFailed, SectionID: secID, SectionCode: secCode,
					Message: fmt.Sprintf("章节 %s %s 的 D3 写作核验未通过", secCode, title)})
			}
		}
	}
	return rows.Err()
}
