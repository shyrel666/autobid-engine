package db

// Phase 6A：导出就绪度判定（只读）。6B 的导出门禁在锁定版本快照的同一事务内调用 exportReadinessTx，
// 判定逻辑只在这里维护，HTTP 层与导出模块都不得另行实现。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/autobid/autobid-engine/compliance"
)

// 阻断原因类型。
const (
	BlockNoApprovedOutline  = "no_approved_outline"
	BlockManifest           = "requirements_not_confirmed"
	BlockUnmapped           = "requirement_unmapped"
	BlockSectionUnapproved  = "section_not_approved"
	BlockMatrixStale        = "matrix_stale"
	BlockMatrixFailed       = "matrix_failed"
	BlockMatrixUnverified   = "matrix_unverified"
	BlockConsistencyMissing = "consistency_not_run"
	BlockConsistencyStale   = "consistency_stale"
	BlockFindingOpen        = "finding_open"
	BlockEvidenceInvalid    = "evidence_invalid"
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
	var outlineID int64
	var epoch int64
	err := q.QueryRowContext(ctx, `SELECT id, version, requirements_epoch FROM proposal_outlines
		WHERE tenant_id = $1 AND task_id = $2 AND status = 'approved'`, tenant, taskID).Scan(&outlineID, &r.OutlineVersion, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		r.block(ExportBlocker{Kind: BlockNoApprovedOutline, Message: "任务没有已批准的技术标大纲"})
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	m, err := ensureManifestTx(ctx, q, tenant, taskID, false)
	if err != nil {
		return nil, err
	}
	if m.Status != ManifestConfirmed || m.Epoch != epoch {
		r.block(ExportBlocker{Kind: BlockManifest, Message: fmt.Sprintf("要求清单状态 %s（epoch %d），批准大纲依据 epoch %d", m.Status, m.Epoch, epoch)})
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
	r.Ready = len(r.Blockers) == 0
	return r, nil
}
