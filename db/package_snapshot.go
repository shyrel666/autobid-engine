package db

// D4（成稿与提交）：导出快照中的附表数据 PackageSnapshot。
//
// 与 doc / chart / matrix 快照在同一个 REPEATABLE READ 事务、同一批锁（批准大纲 → 章节）下固化：
//   - 评分索引：本次投标范围内的技术 / 商务评分项（价格分由报价文件响应，不进技术标）及批准大纲中映射它的章节，
//     按评分项编号排序，评委顺着评分表逐项查找；
//   - 承诺事项：已确认 / 已会签的承诺台账条目（价格类不写进技术标）及锁定修订中引用它的章节；
//   - 证明材料：锁定修订引用的可采性明细对应的企业证据、当前原件的页码范围、所支撑的要求与引用它的章节。
// 成稿里的页码不在快照里：页码由文档引擎排版后才确定，成稿用指向章节标题书签的 PAGEREF 域，回读时核对。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// buildPackageSnapshotTx 读取附表数据；sections 是文档快照中的章节（含锁定修订 ID）。
func buildPackageSnapshotTx(ctx context.Context, q queryer, tenant string, taskID, outlineID int64, sections []SnapshotSection) (PackageSnapshot, error) {
	var p PackageSnapshot
	refByID := map[int64]SnapshotSectionRef{}
	secOfRev := map[int64]int64{}
	revIDs := make([]int64, 0, len(sections))
	seqOf := map[int64]int{}
	for i, s := range sections {
		refByID[s.SectionID] = SnapshotSectionRef{SectionID: s.SectionID, Code: s.Code, Title: s.Title}
		secOfRev[s.RevID] = s.SectionID
		revIDs = append(revIDs, s.RevID)
		seqOf[s.SectionID] = i
	}
	refs := func(ids map[int64]bool) []SnapshotSectionRef {
		out := []SnapshotSectionRef{}
		for id := range ids {
			if r, ok := refByID[id]; ok {
				out = append(out, r)
			}
		}
		slices.SortFunc(out, func(a, b SnapshotSectionRef) int { return seqOf[a.SectionID] - seqOf[b.SectionID] })
		return out
	}

	// 评分索引。
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.code, r.title, r.score_group, r.eval_kind, r.score_weight::float8, s.id
		FROM requirement_items r
		JOIN proposal_section_requirements m ON m.tenant_id = r.tenant_id AND m.requirement_id = r.id
		JOIN proposal_sections s ON s.tenant_id = m.tenant_id AND s.id = m.section_id
		WHERE r.tenant_id = $1 AND r.task_id = $2 AND s.outline_id = $3 AND r.category = 'scoring_item' AND r.score_group <> 'price'
		  AND NOT r.out_of_bid AND r.superseded_by_id IS NULL AND r.review_status <> 'stale' AND r.parent_id IS NULL
		ORDER BY r.code, r.id, s.seq`, tenant, taskID, outlineID)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var row SnapshotScoreRow
		var secID int64
		if err := rows.Scan(&row.RequirementID, &row.Code, &row.Title, &row.ScoreGroup, &row.EvalKind, &row.Score, &secID); err != nil {
			rows.Close()
			return p, err
		}
		ref, ok := refByID[secID]
		if !ok {
			continue
		}
		if n := len(p.ScoreIndex); n > 0 && p.ScoreIndex[n-1].RequirementID == row.RequirementID {
			p.ScoreIndex[n-1].Sections = append(p.ScoreIndex[n-1].Sections, ref)
			continue
		}
		row.Sections = []SnapshotSectionRef{ref}
		p.ScoreIndex = append(p.ScoreIndex, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}

	// 承诺事项：锁定修订引用关系。
	citing := map[int64]map[int64]bool{} // commitment id -> section ids
	crows, err := q.QueryContext(ctx, `SELECT id, to_jsonb(commitment_ids) FROM section_revisions
		WHERE tenant_id = $1 AND task_id = $2 AND id = ANY($3::bigint[])`, tenant, taskID, pgInt64Array(revIDs))
	if err != nil {
		return p, err
	}
	for crows.Next() {
		var revID int64
		var raw []byte
		if err := crows.Scan(&revID, &raw); err != nil {
			crows.Close()
			return p, err
		}
		for _, cid := range int64sJSON(raw) {
			if citing[cid] == nil {
				citing[cid] = map[int64]bool{}
			}
			citing[cid][secOfRev[revID]] = true
		}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return p, err
	}
	lrows, err := q.QueryContext(ctx, `SELECT c.id, c.code, c.version, c.metric, c.metric_label, c.unit, c.tender_threshold_raw, c.committed_raw,
			c.committed_value::float8, c.status, COALESCE(r.code, '')
		FROM commitment_ledger c
		LEFT JOIN requirement_items r ON r.tenant_id = c.tenant_id AND r.task_id = c.task_id AND r.id = c.requirement_id
		WHERE c.tenant_id = $1 AND c.task_id = $2 AND c.status IN ('confirmed','signed_off') AND c.metric <> 'price'
		ORDER BY c.code, c.id`, tenant, taskID)
	if err != nil {
		return p, err
	}
	for lrows.Next() {
		var c SnapshotCommitment
		var value sql.NullFloat64
		if err := lrows.Scan(&c.ID, &c.Code, &c.Version, &c.Metric, &c.MetricLabel, &c.Unit, &c.TenderThreshold, &c.Committed, &value, &c.Status,
			&c.RequirementCode); err != nil {
			lrows.Close()
			return p, err
		}
		if c.Committed == "" && value.Valid {
			c.Committed = strconv.FormatFloat(value.Float64, 'f', -1, 64)
		}
		c.Sections = refs(citing[c.ID])
		p.Commitments = append(p.Commitments, c)
	}
	lrows.Close()
	if err := lrows.Err(); err != nil {
		return p, err
	}

	// 证明材料：锁定修订引用的可采性明细 -> 绑定 -> 企业证据。
	type evKey struct {
		domain string
		id     int64
	}
	type evAgg struct {
		sections map[int64]bool
		reqs     map[string]bool
	}
	agg := map[evKey]*evAgg{}
	var order []evKey
	erows, err := q.QueryContext(ctx, `SELECT sre.section_rev_id, b.evidence_domain, b.evidence_id, COALESCE(rq.code, '')
		FROM section_revision_evidence sre
		JOIN admissibility_items i ON i.tenant_id = sre.tenant_id AND i.id = sre.admissibility_item_id
		JOIN requirement_evidence_bindings b ON b.tenant_id = i.tenant_id AND b.id = i.binding_id
		LEFT JOIN requirement_items rq ON rq.tenant_id = b.tenant_id AND rq.task_id = b.task_id AND rq.id = b.requirement_id
		WHERE sre.tenant_id = $1 AND sre.task_id = $2 AND sre.section_rev_id = ANY($3::bigint[])
		ORDER BY b.evidence_domain, b.evidence_id, rq.code`, tenant, taskID, pgInt64Array(revIDs))
	if err != nil {
		return p, err
	}
	for erows.Next() {
		var revID int64
		var k evKey
		var reqCode string
		if err := erows.Scan(&revID, &k.domain, &k.id, &reqCode); err != nil {
			erows.Close()
			return p, err
		}
		a := agg[k]
		if a == nil {
			a = &evAgg{sections: map[int64]bool{}, reqs: map[string]bool{}}
			agg[k] = a
			order = append(order, k)
		}
		a.sections[secOfRev[revID]] = true
		if reqCode != "" {
			a.reqs[reqCode] = true
		}
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return p, err
	}
	for _, k := range order {
		e := SnapshotEvidence{Domain: k.domain, EvidenceID: k.id, Originals: []SnapshotOriginal{}}
		var err error
		switch k.domain {
		case "credential":
			err = q.QueryRowContext(ctx, `SELECT code, title, certificate_no, subject_name FROM company_credentials WHERE tenant_id = $1 AND id = $2`,
				tenant, k.id).Scan(&e.Code, &e.Name, &e.Number, &e.Subject)
		case "performance":
			err = q.QueryRowContext(ctx, `SELECT code, project_name, contract_no, subject_name FROM company_performance WHERE tenant_id = $1 AND id = $2`,
				tenant, k.id).Scan(&e.Code, &e.Name, &e.Number, &e.Subject)
		default:
			err = fmt.Errorf("db: unknown evidence domain %q", k.domain)
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue // 就绪度已保证引用的明细可采；证据行被物理清理时不进索引
		}
		if err != nil {
			return p, err
		}
		orows, err := q.QueryContext(ctx, `SELECT document_kind, page_label, original_sha256, page_start, page_end FROM company_evidence_originals
			WHERE tenant_id = $1 AND owner_domain = $2 AND owner_id = $3 AND superseded_at IS NULL ORDER BY id`, tenant, k.domain, k.id)
		if err != nil {
			return p, err
		}
		for orows.Next() {
			var o SnapshotOriginal
			if err := orows.Scan(&o.DocumentKind, &o.PageLabel, &o.SHA256, &o.PageStart, &o.PageEnd); err != nil {
				orows.Close()
				return p, err
			}
			e.Originals = append(e.Originals, o)
		}
		orows.Close()
		if err := orows.Err(); err != nil {
			return p, err
		}
		a := agg[k]
		e.Sections = refs(a.sections)
		e.RequirementCodes = []string{}
		for c := range a.reqs {
			e.RequirementCodes = append(e.RequirementCodes, c)
		}
		slices.Sort(e.RequirementCodes)
		p.Evidence = append(p.Evidence, e)
	}
	return p, nil
}
