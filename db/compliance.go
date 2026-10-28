package db

// Phase 6A（标书正文域）：双层响应核验矩阵。
//   第一层（大纲映射验收）：直接基于当前批准大纲的 proposal_section_requirements 计算覆盖率，不另建映射。
//   第二层（正文实质响应验收）：对每条「要求 × 映射章节」，用可替换的 compliance.Verifier 核验章节当前有效修订的正文，
//   结果写入 compliance_matrix 并绑定 section_rev_id + rev_no + 段落锚点。
//
// 并发不变量：核验在一个事务内完成，锁顺序与 SaveSectionRevision 一致——批准大纲（FOR SHARE）-> 章节（FOR SHARE）；
// 持锁期间人工 / AI 写回会等待本事务提交，提交后由触发器把本次结论置为 stale。即使锁被绕过，写入守卫触发器
// （compliance_matrix_guard）也会把修订号已变、大纲非 approved 或证据已失效的写入强制落为 stale，旧正文的结论不会挂到新修订上。
// Verifier 在持锁事务内调用，实现必须有界（RuleVerifier 为纯计算；LLM 实现须自带超时）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrFindingStale    = errors.New("compliance: finding is stale; rerun the consistency check")
	ErrInvalidFinding  = errors.New("compliance: invalid finding resolution")
	ErrFindingConflict = errors.New("compliance: finding version conflict")
)

// ComplianceRow 是矩阵中的一行（附带要求与章节的展示字段）。
type ComplianceRow struct {
	ID               int64           `json:"id"`
	TaskID           int64           `json:"task_id"`
	OutlineID        int64           `json:"outline_id"`
	OutlineVersion   int             `json:"outline_version"`
	RequirementID    int64           `json:"requirement_item_id"`
	RequirementCode  string          `json:"requirement_code"`
	RequirementTitle string          `json:"requirement_title"`
	Category         string          `json:"category"`
	Kind             string          `json:"kind"`
	IsMandatory      bool            `json:"is_mandatory"`
	SectionID        int64           `json:"section_id"`
	SectionCode      string          `json:"section_code"`
	SectionTitle     string          `json:"section_title"`
	SectionRevID     *int64          `json:"section_rev_id,omitempty"`
	RevNo            *int            `json:"rev_no,omitempty"`
	CurrentRevNo     int             `json:"current_rev_no"`
	Anchor           string          `json:"anchor"`
	AnchorExcerpt    string          `json:"anchor_excerpt"`
	ResponseSummary  string          `json:"response_summary"`
	Deviation        string          `json:"deviation"`
	VerifyStatus     string          `json:"verify_status"`
	FailureReason    string          `json:"failure_reason"`
	StaleReason      string          `json:"stale_reason"`
	EvidenceIDs      []int64         `json:"evidence_ids"`
	Checks           json.RawMessage `json:"checks"`
	Verifier         string          `json:"verifier"`
	VerifiedAt       *time.Time      `json:"verified_at,omitempty"`
	CreatedBy        string          `json:"created_by"`
	UpdatedBy        string          `json:"updated_by"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

const matrixCols = `m.id, m.task_id, m.outline_id, m.outline_version, m.requirement_item_id, r.code, r.title, r.category, r.description, r.is_mandatory,
	m.section_id, s.section_code, s.title, m.section_rev_id, m.rev_no, s.current_rev_no, m.anchor, m.anchor_excerpt, m.response_summary,
	m.deviation, m.verify_status, m.failure_reason, m.stale_reason, to_jsonb(m.evidence_ids), m.checks, m.verifier, m.verified_at,
	m.created_by, m.updated_by, m.updated_at`

const matrixFrom = ` FROM compliance_matrix m
	JOIN requirement_items r ON r.tenant_id = m.tenant_id AND r.id = m.requirement_item_id
	JOIN proposal_sections s ON s.tenant_id = m.tenant_id AND s.id = m.section_id`

func scanMatrixRow(rs rowScanner) (*ComplianceRow, error) {
	var x ComplianceRow
	var desc string
	var rev sql.NullInt64
	var revNo sql.NullInt32
	var ev, checks []byte
	var at sql.NullTime
	err := rs.Scan(&x.ID, &x.TaskID, &x.OutlineID, &x.OutlineVersion, &x.RequirementID, &x.RequirementCode, &x.RequirementTitle, &x.Category, &desc,
		&x.IsMandatory, &x.SectionID, &x.SectionCode, &x.SectionTitle, &rev, &revNo, &x.CurrentRevNo, &x.Anchor, &x.AnchorExcerpt, &x.ResponseSummary,
		&x.Deviation, &x.VerifyStatus, &x.FailureReason, &x.StaleReason, &ev, &checks, &x.Verifier, &at, &x.CreatedBy, &x.UpdatedBy, &x.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if rev.Valid {
		x.SectionRevID = &rev.Int64
	}
	if revNo.Valid {
		v := int(revNo.Int32)
		x.RevNo = &v
	}
	x.Kind = compliance.RequirementKind(x.Category, x.RequirementTitle, desc, x.IsMandatory)
	x.EvidenceIDs = int64sJSON(ev)
	x.Checks = checks
	x.VerifiedAt = nullTimePtr(at)
	return &x, nil
}

// ---------------------------------------------------------------------------
// 第二层：正文实质响应核验
// ---------------------------------------------------------------------------

// VerifyMatrixResult 汇总一次矩阵核验；Stale 为写入时被守卫判定已失效的行数（并发修订 / 证据失效）。
type VerifyMatrixResult struct {
	OutlineVersion int              `json:"outline_version"`
	SectionID      int64            `json:"section_id,omitempty"`
	Checked        int              `json:"checked"`
	Verified       int              `json:"verified"`
	Failed         int              `json:"failed"`
	Pending        int              `json:"pending"`
	Stale          int              `json:"stale"`
	Rows           []*ComplianceRow `json:"rows"`
}

type lockedSection struct {
	id         int64
	code       string
	title      string
	currentRev int
}

type sectionRevText struct {
	id       int64
	revNo    int
	content  string
	evidence []int64
}

// lockApprovedSectionsTx 按「批准大纲 FOR SHARE -> 章节 FOR SHARE（按 id）」加锁并返回章节；sectionID > 0 时只锁该章节。
func lockApprovedSectionsTx(ctx context.Context, tx *sql.Tx, tenant string, taskID, sectionID int64) (*ProposalOutline, []lockedSection, error) {
	approved, err := approvedOutlineTx(ctx, tx, tenant, taskID)
	if err != nil {
		return nil, nil, err
	}
	if approved == nil {
		return nil, nil, fmt.Errorf("%w: task has no approved outline", ErrOutlineNotApproved)
	}
	q := `SELECT id, section_code, title, current_rev_no FROM proposal_sections WHERE tenant_id = $1 AND outline_id = $2`
	args := []any{tenant, approved.ID}
	if sectionID > 0 {
		q += ` AND id = $3`
		args = append(args, sectionID)
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY id FOR SHARE`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []lockedSection
	for rows.Next() {
		var s lockedSection
		if err := rows.Scan(&s.id, &s.code, &s.title, &s.currentRev); err != nil {
			return nil, nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if sectionID > 0 && len(out) == 0 {
		var v int
		err := tx.QueryRowContext(ctx, `SELECT outline_version FROM proposal_sections WHERE tenant_id = $1 AND task_id = $2 AND id = $3`,
			tenant, taskID, sectionID).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: section is in v%d, approved outline is v%d", ErrOutlineStale, v, approved.Version)
	}
	return approved, out, nil
}

// currentRevisionsTx 读取章节当前主版本正文（调用方已持有章节锁）。
func currentRevisionsTx(ctx context.Context, q queryer, tenant string, ids []int64) (map[int64]sectionRevText, error) {
	out := map[int64]sectionRevText{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT r.section_id, r.id, r.rev_no, r.content_md, to_jsonb(r.evidence_ids)
		FROM section_revisions r JOIN proposal_sections s ON s.tenant_id = r.tenant_id AND s.id = r.section_id AND s.current_rev_no = r.rev_no
		WHERE r.tenant_id = $1 AND r.section_id = ANY($2::bigint[])`, tenant, pgInt64Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sid int64
		var x sectionRevText
		var ev []byte
		if err := rows.Scan(&sid, &x.id, &x.revNo, &x.content, &ev); err != nil {
			return nil, err
		}
		x.evidence = int64sJSON(ev)
		out[sid] = x
	}
	return out, rows.Err()
}

type mappedRequirement struct {
	sectionID int64
	req       *RequirementItem
}

func mappedRequirementsTx(ctx context.Context, q queryer, tenant string, taskID int64, sectionIDs []int64) ([]mappedRequirement, error) {
	if len(sectionIDs) == 0 {
		return nil, nil
	}
	reqs, err := listRequirementsTx(ctx, q, tenant, taskID)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*RequirementItem{}
	for _, r := range reqs {
		byID[r.ID] = r
	}
	rows, err := q.QueryContext(ctx, `SELECT m.section_id, m.requirement_id FROM proposal_section_requirements m
		WHERE m.tenant_id = $1 AND m.section_id = ANY($2::bigint[]) ORDER BY m.section_id, m.requirement_id`, tenant, pgInt64Array(sectionIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mappedRequirement
	for rows.Next() {
		var sid, rid int64
		if err := rows.Scan(&sid, &rid); err != nil {
			return nil, err
		}
		if r := byID[rid]; r != nil {
			out = append(out, mappedRequirement{sectionID: sid, req: r})
		}
	}
	return out, rows.Err()
}

func complianceRequirement(r *RequirementItem) compliance.Requirement {
	return compliance.Requirement{ID: r.ID, Code: r.Code, Category: r.Category, Title: r.Title, Description: r.Description,
		IsMandatory: r.IsMandatory, ScoreWeight: r.ScoreWeight, Kind: compliance.RequirementKind(r.Category, r.Title, r.Description, r.IsMandatory)}
}

// VerifyComplianceMatrix 核验当前批准大纲下（sectionID > 0 时仅该章节）全部「要求 × 章节」映射的正文实质响应。
func (d *DB) VerifyComplianceMatrix(ctx context.Context, tenant string, taskID, sectionID int64, v compliance.Verifier, actor string) (*VerifyMatrixResult, error) {
	tenant = tenantOr(tenant)
	if v == nil {
		v = compliance.RuleVerifier{}
	}
	res := &VerifyMatrixResult{SectionID: sectionID, Rows: []*ComplianceRow{}}
	var outlineID int64
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		approved, secs, err := lockApprovedSectionsTx(ctx, tx, tenant, taskID, sectionID)
		if err != nil {
			return err
		}
		res.OutlineVersion, outlineID = approved.Version, approved.ID
		ids := make([]int64, 0, len(secs))
		byID := map[int64]lockedSection{}
		for _, s := range secs {
			ids = append(ids, s.id)
			byID[s.id] = s
		}
		revs, err := currentRevisionsTx(ctx, tx, tenant, ids)
		if err != nil {
			return err
		}
		maps, err := mappedRequirementsTx(ctx, tx, tenant, taskID, ids)
		if err != nil {
			return err
		}
		var allEv []int64
		for _, m := range maps {
			allEv = append(allEv, m.req.EvidenceIDs...)
			allEv = append(allEv, revs[m.sectionID].evidence...)
		}
		states, err := evidenceStatesTx(ctx, tx, tenant, taskID, allEv)
		if err != nil {
			return err
		}
		for _, m := range maps {
			sec, rev := byID[m.sectionID], revs[m.sectionID]
			evIDs := uniqueIDs(append(append([]int64{}, m.req.EvidenceIDs...), rev.evidence...))
			ev := make([]compliance.Evidence, 0, len(evIDs))
			for _, id := range evIDs {
				e, ok := states[id]
				if !ok {
					ev = append(ev, compliance.Evidence{ID: id, Status: "stale", Reason: "evidence missing or not visible to this task", Missing: true})
					continue
				}
				ev = append(ev, compliance.Evidence{ID: id, Status: e.Status, Relation: e.Relation, Reason: e.Reason})
			}
			var verdict compliance.Verdict
			if sec.currentRev == 0 || rev.revNo == 0 {
				verdict = compliance.Verdict{Status: compliance.StatusPending, Deviation: compliance.DeviationNone, Verifier: v.Name(),
					Reason: "章节尚无有效修订", Checks: []compliance.Check{}}
			} else {
				verdict, err = v.Verify(ctx, compliance.Input{Requirement: complianceRequirement(m.req),
					Section:  compliance.Section{ID: sec.id, Code: sec.code, Title: sec.title},
					Revision: compliance.Revision{ID: rev.id, RevNo: rev.revNo, ContentMD: rev.content}, Evidence: ev})
				if err != nil {
					return fmt.Errorf("verify requirement %s in section %s: %w", m.req.Code, sec.code, err)
				}
			}
			status, err := upsertMatrixRowTx(ctx, tx, tenant, taskID, approved, m.req.ID, sec.id, rev, evIDs, verdict, actor)
			if err != nil {
				return err
			}
			res.Checked++
			switch status {
			case compliance.StatusVerified:
				res.Verified++
			case compliance.StatusFailed:
				res.Failed++
			case compliance.StatusPending:
				res.Pending++
			default:
				res.Stale++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows, err := d.listMatrix(ctx, d, tenant, taskID, outlineID, MatrixFilter{SectionID: sectionID})
	res.Rows = rows
	return res, err
}

func upsertMatrixRowTx(ctx context.Context, tx *sql.Tx, tenant string, taskID int64, o *ProposalOutline, reqID, sectionID int64, rev sectionRevText,
	evIDs []int64, v compliance.Verdict, actor string) (string, error) {
	var revID, revNo any
	var verifiedAt any
	if v.Status != compliance.StatusPending {
		revID, revNo, verifiedAt = rev.id, rev.revNo, time.Now()
	}
	verifier := v.Verifier
	if verifier != compliance.VerifierLLM {
		verifier = compliance.VerifierRule
	}
	checks, _ := json.Marshal(v.Checks)
	if v.Checks == nil {
		checks = []byte("[]")
	}
	var status string
	err := tx.QueryRowContext(ctx, `INSERT INTO compliance_matrix(tenant_id, task_id, outline_id, outline_version, requirement_item_id, section_id,
			section_rev_id, rev_no, anchor, anchor_excerpt, response_summary, deviation, verify_status, failure_reason, stale_reason, evidence_ids, checks,
			verifier, verified_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, '', $15::bigint[], $16::jsonb, $17, $18, $19, $19)
		ON CONFLICT (tenant_id, section_id, requirement_item_id) DO UPDATE SET
			outline_id = EXCLUDED.outline_id, outline_version = EXCLUDED.outline_version, section_rev_id = EXCLUDED.section_rev_id, rev_no = EXCLUDED.rev_no,
			anchor = EXCLUDED.anchor, anchor_excerpt = EXCLUDED.anchor_excerpt, response_summary = EXCLUDED.response_summary,
			deviation = EXCLUDED.deviation, verify_status = EXCLUDED.verify_status, failure_reason = EXCLUDED.failure_reason, stale_reason = '',
			evidence_ids = EXCLUDED.evidence_ids, checks = EXCLUDED.checks, verifier = EXCLUDED.verifier, verified_at = EXCLUDED.verified_at,
			updated_by = EXCLUDED.updated_by
		RETURNING verify_status`,
		tenant, taskID, o.ID, o.Version, reqID, sectionID, revID, revNo, v.Anchor, v.AnchorExcerpt, v.ResponseSummary, v.Deviation, v.Status,
		v.Reason, pgInt64Array(evIDs), string(checks), verifier, verifiedAt, actor).Scan(&status)
	return status, err
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// MatrixFilter 过滤矩阵行；Kind 取 compliance.Kind*（star / triangle / scoring / format / required）。
type MatrixFilter struct {
	Status    string
	Kind      string
	SectionID int64
}

// matrixOutlineTx 返回矩阵展示所依据的大纲：当前批准大纲，否则为最近一次有矩阵记录的大纲（其行均已 stale）。
func matrixOutlineTx(ctx context.Context, q queryer, tenant string, taskID int64) (int64, int, error) {
	var id int64
	var v int
	err := q.QueryRowContext(ctx, `SELECT id, version FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2 AND status = 'approved'`,
		tenant, taskID).Scan(&id, &v)
	if errors.Is(err, sql.ErrNoRows) {
		err = q.QueryRowContext(ctx, `SELECT outline_id, outline_version FROM compliance_matrix WHERE tenant_id = $1 AND task_id = $2
			ORDER BY outline_version DESC LIMIT 1`, tenant, taskID).Scan(&id, &v)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
	}
	return id, v, err
}

func (d *DB) listMatrix(ctx context.Context, q queryer, tenant string, taskID, outlineID int64, f MatrixFilter) ([]*ComplianceRow, error) {
	out := []*ComplianceRow{}
	if outlineID == 0 {
		return out, nil
	}
	query := `SELECT ` + matrixCols + matrixFrom + ` WHERE m.tenant_id = $1 AND m.task_id = $2 AND m.outline_id = $3`
	args := []any{tenant, taskID, outlineID}
	if f.Status != "" {
		args = append(args, f.Status)
		query += fmt.Sprintf(` AND m.verify_status = $%d`, len(args))
	}
	if f.SectionID > 0 {
		args = append(args, f.SectionID)
		query += fmt.Sprintf(` AND m.section_id = $%d`, len(args))
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY r.code, s.seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		x, err := scanMatrixRow(rows)
		if err != nil {
			return nil, err
		}
		if f.Kind == "" || x.Kind == f.Kind {
			out = append(out, x)
		}
	}
	return out, rows.Err()
}

// MatrixView 是矩阵查询结果。
type MatrixView struct {
	OutlineVersion int              `json:"outline_version"`
	Approved       bool             `json:"approved"`
	Rows           []*ComplianceRow `json:"rows"`
}

// ListComplianceMatrix 返回矩阵行（单一快照读取）。
func (d *DB) ListComplianceMatrix(ctx context.Context, tenant string, taskID int64, f MatrixFilter) (*MatrixView, error) {
	tenant = tenantOr(tenant)
	out := &MatrixView{}
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		id, v, err := matrixOutlineTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		out.OutlineVersion = v
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM proposal_outlines WHERE id = $1 AND status = 'approved')`, id).Scan(&out.Approved); err != nil {
			return err
		}
		out.Rows, err = d.listMatrix(ctx, tx, tenant, taskID, id, f)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 两层覆盖率
// ---------------------------------------------------------------------------

// RequirementCompliance 是一条要求的两层状态。Status：unmapped（第一层缺口）/ unchecked（已映射未核验）/
// pending（章节尚无正文）/ failed / stale / verified（任一映射章节的当前修订实质响应通过）。
type RequirementCompliance struct {
	ID           int64    `json:"id"`
	Code         string   `json:"code"`
	Title        string   `json:"title"`
	Category     string   `json:"category"`
	Kind         string   `json:"kind"`
	IsMandatory  bool     `json:"is_mandatory"`
	Mapped       bool     `json:"mapped"`
	SectionCodes []string `json:"section_codes"`
	Status       string   `json:"status"`
	Deviation    string   `json:"deviation"`
	Reason       string   `json:"reason,omitempty"`
}

// KindCoverage 按要求类型汇总两层覆盖。
type KindCoverage struct {
	Kind      string `json:"kind"`
	Total     int    `json:"total"`
	Mapped    int    `json:"mapped"`
	Verified  int    `json:"verified"`
	Failed    int    `json:"failed"`
	Stale     int    `json:"stale"`
	Pending   int    `json:"pending"`
	Unchecked int    `json:"unchecked"`
}

type ComplianceCoverage struct {
	OutlineVersion int                      `json:"outline_version"`
	Approved       bool                     `json:"approved"`
	Total          int                      `json:"total"`
	Mapped         int                      `json:"mapped"`
	MappingRate    float64                  `json:"mapping_rate"`
	Verified       int                      `json:"verified"`
	ResponseRate   float64                  `json:"response_rate"`
	NegativeCount  int                      `json:"negative_deviations"`
	ByKind         []KindCoverage           `json:"by_kind"`
	Requirements   []*RequirementCompliance `json:"requirements"`
}

var kindOrder = []string{compliance.KindStar, compliance.KindTriangle, compliance.KindScoring, compliance.KindRequired, compliance.KindFormat}

func statusRank(s string) int {
	switch s {
	case compliance.StatusVerified:
		return 5
	case compliance.StatusStale:
		return 4
	case compliance.StatusFailed:
		return 3
	case compliance.StatusPending:
		return 2
	}
	return 1
}

// complianceCoverageTx 依据当前批准大纲计算两层覆盖率；尚无批准大纲时全部要求计为 unmapped。
func complianceCoverageTx(ctx context.Context, q queryer, tenant string, taskID int64) (*ComplianceCoverage, error) {
	out := &ComplianceCoverage{ByKind: []KindCoverage{}, Requirements: []*RequirementCompliance{}}
	var outlineID int64
	err := q.QueryRowContext(ctx, `SELECT id, version FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2 AND status = 'approved'`,
		tenant, taskID).Scan(&outlineID, &out.OutlineVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	out.Approved = outlineID != 0
	reqs, err := listRequirementsTx(ctx, q, tenant, taskID)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*RequirementCompliance{}
	for _, r := range reqs {
		rc := &RequirementCompliance{ID: r.ID, Code: r.Code, Title: r.Title, Category: r.Category, IsMandatory: r.IsMandatory,
			Kind: compliance.RequirementKind(r.Category, r.Title, r.Description, r.IsMandatory), SectionCodes: []string{}, Status: "unmapped",
			Deviation: compliance.DeviationNone}
		byID[r.ID] = rc
		out.Requirements = append(out.Requirements, rc)
	}
	if outlineID != 0 {
		rows, err := q.QueryContext(ctx, `SELECT m.requirement_id, s.section_code, COALESCE(c.verify_status, ''), COALESCE(c.deviation, 'none'),
				COALESCE(NULLIF(c.failure_reason, ''), c.stale_reason, '')
			FROM proposal_section_requirements m
			JOIN proposal_sections s ON s.tenant_id = m.tenant_id AND s.id = m.section_id
			LEFT JOIN compliance_matrix c ON c.tenant_id = m.tenant_id AND c.section_id = m.section_id AND c.requirement_item_id = m.requirement_id
			WHERE m.tenant_id = $1 AND s.outline_id = $2 ORDER BY s.seq`, tenant, outlineID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var rid int64
			var code, status, dev, reason string
			if err := rows.Scan(&rid, &code, &status, &dev, &reason); err != nil {
				rows.Close()
				return nil, err
			}
			rc := byID[rid]
			if rc == nil {
				continue
			}
			if !rc.Mapped {
				rc.Mapped, rc.Status = true, "unchecked"
			}
			rc.SectionCodes = append(rc.SectionCodes, code)
			if status == "" {
				continue
			}
			if dev == compliance.DeviationNegative {
				rc.Deviation = dev
			} else if dev == compliance.DeviationPositive && rc.Deviation == compliance.DeviationNone {
				rc.Deviation = dev
			}
			if statusRank(status) > statusRank(rc.Status) {
				rc.Status, rc.Reason = status, reason
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	kinds := map[string]*KindCoverage{}
	for _, k := range kindOrder {
		kinds[k] = &KindCoverage{Kind: k}
	}
	for _, rc := range out.Requirements {
		k := kinds[rc.Kind]
		k.Total++
		out.Total++
		if rc.Mapped {
			k.Mapped++
			out.Mapped++
		}
		if rc.Deviation == compliance.DeviationNegative {
			out.NegativeCount++
		}
		switch rc.Status {
		case compliance.StatusVerified:
			k.Verified++
			out.Verified++
		case compliance.StatusFailed:
			k.Failed++
		case compliance.StatusStale:
			k.Stale++
		case compliance.StatusPending:
			k.Pending++
		case "unchecked":
			k.Unchecked++
		}
	}
	for _, k := range kindOrder {
		if kinds[k].Total > 0 {
			out.ByKind = append(out.ByKind, *kinds[k])
		}
	}
	if out.Total > 0 {
		out.MappingRate = float64(out.Mapped) / float64(out.Total)
		out.ResponseRate = float64(out.Verified) / float64(out.Total)
	}
	sort.SliceStable(out.Requirements, func(i, j int) bool {
		return statusRank(out.Requirements[i].Status) < statusRank(out.Requirements[j].Status)
	})
	return out, nil
}

// ComplianceCoverage 返回两层覆盖率汇总（单一快照读取）。
func (d *DB) ComplianceCoverage(ctx context.Context, tenant string, taskID int64) (*ComplianceCoverage, error) {
	tenant = tenantOr(tenant)
	var out *ComplianceCoverage
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = complianceCoverageTx(ctx, tx, tenant, taskID)
		return err
	})
	return out, err
}

// isRetryableTx 判断是否为可重试的序列化失败 / 死锁。
func isRetryableTx(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}
