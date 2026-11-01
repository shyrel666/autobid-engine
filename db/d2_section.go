package db

// D2 章节级引用门禁：只允许引用当前可采性运行中 verdict=admissible 的明细与已确认 / 已会签台账。
// 台账正文值必须与台账一致；不可采 / manual / 未确认一律拒绝成为主版本。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func insertSectionRevisionEvidenceTx(ctx context.Context, q queryer, tenant string, taskID, revID int64, itemIDs []int64, actor string) error {
	for _, id := range uniqueIDs(itemIDs) {
		if _, err := q.ExecContext(ctx, `INSERT INTO section_revision_evidence(tenant_id, task_id, section_rev_id, admissibility_item_id, created_by)
			VALUES ($1,$2,$3,$4,COALESCE(NULLIF($5,''),'')) ON CONFLICT DO NOTHING`, tenant, taskID, revID, id, actor); err != nil {
			return fmt.Errorf("bind admissibility item %d: %w", id, err)
		}
	}
	return nil
}

func validateCommitmentRefsTx(ctx context.Context, q queryer, tenant string, taskID int64, ids []int64, content string) ([]*Commitment, error) {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT `+commitmentCols+commitmentJoin+
		`WHERE c.tenant_id=$1 AND c.task_id=$2 AND c.id = ANY($3::bigint[]) ORDER BY c.code`, tenant, taskID, pgInt64Array(ids))
	if err != nil {
		return nil, err
	}
	var out []*Commitment
	for rows.Next() {
		c, err := scanCommitment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(ids) {
		return nil, fmt.Errorf("%w: unknown commitment id", ErrCommitmentUnconfirmed)
	}
	for _, c := range out {
		if c.Status != CommitmentConfirmed && c.Status != CommitmentSignedOff {
			return nil, fmt.Errorf("%w: %s status=%s", ErrCommitmentUnconfirmed, c.Code, c.Status)
		}
	}
	if err := CheckCommitmentText(content, out); err != nil {
		return nil, err
	}
	return out, nil
}

// admissibilityRefState 是章节引用校验所需的运行状态。
type admissibilityRefState struct {
	ItemID  int64
	Verdict string
	RunID   int64
	Status  string
	TaskID  int64
}

func validateAdmissibilityRefsTx(ctx context.Context, q queryer, tenant string, taskID int64, ids []int64) ([]int64, error) {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT i.id, i.verdict, r.id, r.status, r.task_id
		FROM admissibility_items i JOIN admissibility_runs r ON r.tenant_id=i.tenant_id AND r.id=i.run_id
		WHERE i.tenant_id=$1 AND i.id = ANY($2::bigint[]) ORDER BY i.id`, tenant, pgInt64Array(ids))
	if err != nil {
		return nil, err
	}
	found := map[int64]admissibilityRefState{}
	for rows.Next() {
		var s admissibilityRefState
		if err := rows.Scan(&s.ItemID, &s.Verdict, &s.RunID, &s.Status, &s.TaskID); err != nil {
			rows.Close()
			return nil, err
		}
		found[s.ItemID] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		s, ok := found[id]
		if !ok || s.TaskID != taskID {
			return nil, fmt.Errorf("%w: admissibility item %d does not belong to task", ErrEvidenceInadmissible, id)
		}
		switch {
		case s.Status != "current":
			return nil, fmt.Errorf("%w: item %d run %d is %s", ErrAdmissibilityStale, id, s.RunID, s.Status)
		case s.Verdict == "manual":
			return nil, fmt.Errorf("%w: item %d requires manual review", ErrAdmissibilityManual, id)
		case s.Verdict != "admissible":
			return nil, fmt.Errorf("%w: item %d verdict=%s", ErrEvidenceInadmissible, id, s.Verdict)
		}
	}
	return ids, nil
}

// linkedAdmissibilityItemIDsTx 返回修订登记的全部可采性明细 ID（含已失效，供审批时严格复核）。
func linkedAdmissibilityItemIDsTx(ctx context.Context, q queryer, tenant string, revID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT admissibility_item_id FROM section_revision_evidence
		WHERE tenant_id=$1 AND section_rev_id=$2 ORDER BY admissibility_item_id`, tenant, revID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// currentAdmissibilityItemIDsTx 返回某修订绑定的、仍属于当前 admissible 运行的明细 ID。
func currentAdmissibilityItemIDsTx(ctx context.Context, q queryer, tenant string, taskID, revID int64) ([]int64, []int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT i.id, i.verdict, r.status, r.task_id
		FROM section_revision_evidence sre
		JOIN admissibility_items i ON i.tenant_id=sre.tenant_id AND i.id=sre.admissibility_item_id
		JOIN admissibility_runs r ON r.tenant_id=i.tenant_id AND r.id=i.run_id
		WHERE sre.tenant_id=$1 AND sre.section_rev_id=$2 ORDER BY i.id`, tenant, revID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var keep, dropped []int64
	for rows.Next() {
		var id, taskID2 int64
		var verdict, status string
		if err := rows.Scan(&id, &verdict, &status, &taskID2); err != nil {
			return nil, nil, err
		}
		if taskID2 == taskID && status == "current" && verdict == "admissible" {
			keep = append(keep, id)
		} else {
			dropped = append(dropped, id)
		}
	}
	return keep, dropped, rows.Err()
}

func currentCommitmentIDsTx(ctx context.Context, q queryer, tenant string, taskID int64, ids []int64) ([]int64, []int64, error) {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return nil, nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT id, status FROM commitment_ledger
		WHERE tenant_id=$1 AND task_id=$2 AND id = ANY($3::bigint[]) ORDER BY id`, tenant, taskID, pgInt64Array(ids))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	found := map[int64]string{}
	for rows.Next() {
		var id int64
		var status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, nil, err
		}
		found[id] = status
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var keep, dropped []int64
	for _, id := range ids {
		if s, ok := found[id]; ok && (s == CommitmentConfirmed || s == CommitmentSignedOff) {
			keep = append(keep, id)
		} else {
			dropped = append(dropped, id)
		}
	}
	return keep, dropped, nil
}

// CurrentRevisionAdmissibilityIDs 返回章节当前修订引用的可采性明细（读取 API）。
func (d *DB) CurrentRevisionAdmissibilityIDs(ctx context.Context, tenant string, taskID, sectionID int64) ([]int64, error) {
	var revID int64
	err := d.QueryRowContext(ctx, `SELECT r.id FROM proposal_sections s JOIN section_revisions r
		ON r.tenant_id=s.tenant_id AND r.section_id=s.id AND r.rev_no=s.current_rev_no
		WHERE s.tenant_id=$1 AND s.task_id=$2 AND s.id=$3`, tenantOr(tenant), taskID, sectionID).Scan(&revID)
	if err == sql.ErrNoRows {
		return []int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	keep, _, err := currentAdmissibilityItemIDsTx(ctx, d, tenantOr(tenant), taskID, revID)
	return keep, err
}

// CommitmentsForRevision 读取修订引用的台账（供导出 / 详情展示）。
func (d *DB) CommitmentsForRevision(ctx context.Context, tenant string, taskID, revID int64) ([]*Commitment, error) {
	var raw []byte
	err := d.QueryRowContext(ctx, `SELECT to_jsonb(commitment_ids) FROM section_revisions WHERE tenant_id=$1 AND task_id=$2 AND id=$3`,
		tenantOr(tenant), taskID, revID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	ids := int64sJSON(raw)
	if len(ids) == 0 {
		return []*Commitment{}, nil
	}
	rows, err := d.QueryContext(ctx, `SELECT `+commitmentCols+commitmentJoin+
		`WHERE c.tenant_id=$1 AND c.task_id=$2 AND c.id = ANY($3::bigint[]) ORDER BY c.code`, tenantOr(tenant), taskID, pgInt64Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Commitment
	for rows.Next() {
		c, err := scanCommitment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func marshalIDs(ids []int64) string {
	raw, _ := json.Marshal(uniqueIDs(ids))
	return string(raw)
}
