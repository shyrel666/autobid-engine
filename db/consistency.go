package db

// Phase 6A（标书正文域）：全文交叉一致性核验 fulltext_consistency_check。
// 输入为当前批准大纲下每个章节的有效修订，在 REPEATABLE READ 事务中先按「批准大纲 FOR SHARE -> 章节 FOR SHARE」加锁
// 再读取，保证快照（section_id -> rev_no）与被比对的正文一致；规则抽取关键承诺（compliance.Extract）并比对，
// ★/▲ 要求在第二层矩阵中没有当前修订上的 verified 结论的全部记为漏项。结论与 explorations/findings 推演域隔离。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
)

// 结论类型与状态。
const (
	FindingContradiction = "contradiction"
	FindingOmission      = "omission"
	FindingOpen          = "open"
	FindingResolved      = "resolved"
	FindingStale         = "stale"
	RunCompleted         = "completed"
	RunStale             = "stale"
)

type ConsistencyRun struct {
	ID             int64           `json:"id"`
	TaskID         int64           `json:"task_id"`
	OutlineID      int64           `json:"outline_id"`
	OutlineVersion int             `json:"outline_version"`
	Snapshot       map[string]int  `json:"snapshot"`
	Status         string          `json:"status"`
	StaleReason    string          `json:"stale_reason"`
	Contradictions int             `json:"contradictions"`
	Omissions      int             `json:"omissions"`
	Summary        json.RawMessage `json:"summary"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type ConsistencyFinding struct {
	ID               int64      `json:"id"`
	RunID            int64      `json:"run_id"`
	Kind             string     `json:"kind"`
	Metric           string     `json:"metric"`
	MetricLabel      string     `json:"metric_label"`
	RequirementID    *int64     `json:"requirement_item_id,omitempty"`
	RequirementCode  string     `json:"requirement_code,omitempty"`
	RequirementTitle string     `json:"requirement_title,omitempty"`
	RequirementKind  string     `json:"requirement_kind,omitempty"`
	SectionIDs       []int64    `json:"section_ids"`
	ASectionID       *int64     `json:"a_section_id,omitempty"`
	ASectionCode     string     `json:"a_section_code,omitempty"`
	ASectionTitle    string     `json:"a_section_title,omitempty"`
	ARevNo           *int       `json:"a_rev_no,omitempty"`
	AValue           string     `json:"a_value"`
	AExcerpt         string     `json:"a_excerpt"`
	BSectionID       *int64     `json:"b_section_id,omitempty"`
	BSectionCode     string     `json:"b_section_code,omitempty"`
	BSectionTitle    string     `json:"b_section_title,omitempty"`
	BRevNo           *int       `json:"b_rev_no,omitempty"`
	BValue           string     `json:"b_value"`
	BExcerpt         string     `json:"b_excerpt"`
	Detail           string     `json:"detail"`
	Status           string     `json:"status"`
	StaleReason      string     `json:"stale_reason"`
	ResolutionKind   string     `json:"resolution_kind"`
	ResolutionNote   string     `json:"resolution_note"`
	ResolvedBy       string     `json:"resolved_by"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
	Version          int        `json:"version"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type ConsistencyRunDetail struct {
	Run      *ConsistencyRun       `json:"run"`
	Findings []*ConsistencyFinding `json:"findings"`
	// Current 为 false 表示该运行的快照已不是当前批准大纲的有效修订集合（须重新核验）。
	Current bool `json:"current"`
}

const consistencyRunCols = `id, task_id, outline_id, outline_version, snapshot, status, stale_reason, contradictions, omissions, summary, created_by, created_at, updated_at`

func scanConsistencyRun(r rowScanner) (*ConsistencyRun, error) {
	var x ConsistencyRun
	var snap []byte
	err := r.Scan(&x.ID, &x.TaskID, &x.OutlineID, &x.OutlineVersion, &snap, &x.Status, &x.StaleReason, &x.Contradictions, &x.Omissions, &x.Summary,
		&x.CreatedBy, &x.CreatedAt, &x.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	x.Snapshot = map[string]int{}
	_ = json.Unmarshal(snap, &x.Snapshot)
	return &x, nil
}

const findingCols = `f.id, f.run_id, f.kind, f.metric, f.metric_label, f.requirement_item_id, COALESCE(r.code, ''), COALESCE(r.title, ''),
	COALESCE(r.category, ''), COALESCE(r.description, ''), COALESCE(r.is_mandatory, false), to_jsonb(f.section_ids),
	f.a_section_id, COALESCE(sa.section_code, ''), COALESCE(sa.title, ''), f.a_rev_no, f.a_value, f.a_excerpt,
	f.b_section_id, COALESCE(sb.section_code, ''), COALESCE(sb.title, ''), f.b_rev_no, f.b_value, f.b_excerpt,
	f.detail, f.status, f.stale_reason, f.resolution_kind, f.resolution_note, f.resolved_by, f.resolved_at, f.version, f.created_at, f.updated_at`

const findingFrom = ` FROM consistency_findings f
	LEFT JOIN requirement_items r ON r.tenant_id = f.tenant_id AND r.id = f.requirement_item_id
	LEFT JOIN proposal_sections sa ON sa.tenant_id = f.tenant_id AND sa.id = f.a_section_id
	LEFT JOIN proposal_sections sb ON sb.tenant_id = f.tenant_id AND sb.id = f.b_section_id`

func scanFinding(rs rowScanner) (*ConsistencyFinding, error) {
	var x ConsistencyFinding
	var rid, aid, bid sql.NullInt64
	var arev, brev sql.NullInt32
	var cat, desc string
	var mand bool
	var secs []byte
	var at sql.NullTime
	err := rs.Scan(&x.ID, &x.RunID, &x.Kind, &x.Metric, &x.MetricLabel, &rid, &x.RequirementCode, &x.RequirementTitle, &cat, &desc, &mand, &secs,
		&aid, &x.ASectionCode, &x.ASectionTitle, &arev, &x.AValue, &x.AExcerpt, &bid, &x.BSectionCode, &x.BSectionTitle, &brev, &x.BValue, &x.BExcerpt,
		&x.Detail, &x.Status, &x.StaleReason, &x.ResolutionKind, &x.ResolutionNote, &x.ResolvedBy, &at, &x.Version, &x.CreatedAt, &x.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if rid.Valid {
		x.RequirementID = &rid.Int64
		x.RequirementKind = compliance.RequirementKind(cat, x.RequirementTitle, desc, mand)
	}
	if aid.Valid {
		x.ASectionID = &aid.Int64
	}
	if bid.Valid {
		x.BSectionID = &bid.Int64
	}
	if arev.Valid {
		v := int(arev.Int32)
		x.ARevNo = &v
	}
	if brev.Valid {
		v := int(brev.Int32)
		x.BRevNo = &v
	}
	x.SectionIDs = int64sJSON(secs)
	x.ResolvedAt = nullTimePtr(at)
	return &x, nil
}

// inRepeatableRead 在 REPEATABLE READ 事务中执行 fn，遇序列化失败 / 死锁时重试（最多 3 次）。
func (d *DB) inRepeatableRead(ctx context.Context, fn func(tx *sql.Tx) error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = func() error {
			tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := fn(tx); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err == nil || !isRetryableTx(err) {
			return err
		}
	}
	return err
}

type sectionTextRow struct {
	id      int64
	code    string
	title   string
	seq     int
	revNo   int
	content string
}

// RunConsistencyCheck 执行一次全文一致性核验并落库（运行 + 结论）。
func (d *DB) RunConsistencyCheck(ctx context.Context, tenant string, taskID int64, actor string) (*ConsistencyRunDetail, error) {
	tenant = tenantOr(tenant)
	var runID int64
	err := d.inRepeatableRead(ctx, func(tx *sql.Tx) error {
		approved, locked, err := lockApprovedSectionsTx(ctx, tx, tenant, taskID, 0)
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(locked))
		snapshot := map[string]int{}
		for _, s := range locked {
			ids = append(ids, s.id)
			snapshot[strconv.FormatInt(s.id, 10)] = s.currentRev
		}
		secs := []sectionTextRow{}
		rows, err := tx.QueryContext(ctx, `SELECT s.id, s.section_code, s.title, s.seq, s.current_rev_no, COALESCE(r.content_md, '')
			FROM proposal_sections s LEFT JOIN section_revisions r ON r.tenant_id = s.tenant_id AND r.section_id = s.id AND r.rev_no = s.current_rev_no
			WHERE s.tenant_id = $1 AND s.outline_id = $2 ORDER BY s.seq`, tenant, approved.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var x sectionTextRow
			if err := rows.Scan(&x.id, &x.code, &x.title, &x.seq, &x.revNo, &x.content); err != nil {
				rows.Close()
				return err
			}
			secs = append(secs, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// 关键承诺抽取与比对。
		var mentions []compliance.Mention
		written := 0
		for _, s := range secs {
			if s.revNo == 0 {
				continue
			}
			written++
			mentions = append(mentions, compliance.Extract(compliance.SectionText{SectionID: s.id, Code: s.code, Title: s.title, Seq: s.seq,
				RevNo: s.revNo, ContentMD: s.content})...)
		}
		contradictions := compliance.Compare(mentions)

		// 漏项：★/▲ 要求在当前修订上没有 verified 的矩阵结论（含未映射、未核验、核验失败、已失效）。
		omissions, err := omissionsTx(ctx, tx, tenant, taskID, approved.ID)
		if err != nil {
			return err
		}
		metrics := map[string]int{}
		for _, m := range mentions {
			metrics[m.Metric]++
		}
		summary, _ := json.Marshal(map[string]any{"sections": len(secs), "sections_with_text": written, "mentions": len(mentions), "by_metric": metrics})
		snap, _ := json.Marshal(snapshot)
		if err := tx.QueryRowContext(ctx, `INSERT INTO consistency_runs(tenant_id, task_id, outline_id, outline_version, snapshot, status,
				contradictions, omissions, summary, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5::jsonb, 'completed', $6, $7, $8::jsonb, $9, $9) RETURNING id`,
			tenant, taskID, approved.ID, approved.Version, string(snap), len(contradictions), len(omissions), string(summary), actor).Scan(&runID); err != nil {
			return err
		}
		for _, c := range contradictions {
			secIDs := uniqueIDs([]int64{c.A.SectionID, c.B.SectionID})
			if _, err := tx.ExecContext(ctx, `INSERT INTO consistency_findings(tenant_id, task_id, run_id, kind, metric, metric_label, section_ids,
					a_section_id, a_rev_no, a_value, a_excerpt, b_section_id, b_rev_no, b_value, b_excerpt, detail, created_by, updated_by)
				VALUES ($1, $2, $3, 'contradiction', $4, $5, $6::bigint[], $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16)`,
				tenant, taskID, runID, c.Metric, c.Label, pgInt64Array(secIDs), c.A.SectionID, c.A.RevNo, compliance.Describe(c.A), c.A.Excerpt,
				c.B.SectionID, c.B.RevNo, compliance.Describe(c.B), c.B.Excerpt, c.Detail, actor); err != nil {
				return err
			}
		}
		for _, o := range omissions {
			if _, err := tx.ExecContext(ctx, `INSERT INTO consistency_findings(tenant_id, task_id, run_id, kind, metric, metric_label,
					requirement_item_id, section_ids, detail, created_by, updated_by)
				VALUES ($1, $2, $3, 'omission', $4, $5, $6, $7::bigint[], $8, $9, $9)`,
				tenant, taskID, runID, o.kind, o.label, o.reqID, pgInt64Array(o.sectionIDs), o.detail, actor); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d.GetConsistencyRun(ctx, tenant, taskID, runID, "")
}

type omission struct {
	reqID      int64
	kind       string
	label      string
	sectionIDs []int64
	detail     string
}

func omissionsTx(ctx context.Context, q queryer, tenant string, taskID, outlineID int64) ([]omission, error) {
	reqs, err := listRequirementsTx(ctx, q, tenant, taskID)
	if err != nil {
		return nil, err
	}
	type cell struct {
		sectionID int64
		code      string
		status    string
		reason    string
		current   bool
	}
	cells := map[int64][]cell{}
	rows, err := q.QueryContext(ctx, `SELECT m.requirement_id, s.id, s.section_code, COALESCE(c.verify_status, ''),
			COALESCE(CASE WHEN c.verify_status = 'stale' THEN c.stale_reason ELSE c.failure_reason END, ''), COALESCE(c.rev_no = s.current_rev_no, false)
		FROM proposal_section_requirements m
		JOIN proposal_sections s ON s.tenant_id = m.tenant_id AND s.id = m.section_id
		LEFT JOIN compliance_matrix c ON c.tenant_id = m.tenant_id AND c.section_id = m.section_id AND c.requirement_item_id = m.requirement_id
		WHERE m.tenant_id = $1 AND s.outline_id = $2 ORDER BY s.seq`, tenant, outlineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rid int64
		var c cell
		if err := rows.Scan(&rid, &c.sectionID, &c.code, &c.status, &c.reason, &c.current); err != nil {
			return nil, err
		}
		cells[rid] = append(cells[rid], c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []omission
	for _, r := range reqs {
		kind := compliance.RequirementKind(r.Category, r.Title, r.Description, r.IsMandatory)
		if !r.InTechnicalScope() || kind != compliance.KindStar && kind != compliance.KindTriangle {
			continue
		}
		label := map[string]string{compliance.KindStar: "★ 条款漏项", compliance.KindTriangle: "▲ 条款漏项"}[kind]
		cs := cells[r.ID]
		ok := false
		var ids []int64
		var notes []string
		for _, c := range cs {
			ids = append(ids, c.sectionID)
			switch {
			case c.status == compliance.StatusVerified && c.current:
				ok = true
			case c.status == "":
				notes = append(notes, c.code+"：未核验")
			default:
				notes = append(notes, fmt.Sprintf("%s：%s %s", c.code, c.status, c.reason))
			}
		}
		if ok {
			continue
		}
		detail := fmt.Sprintf("%s %s 在正文中没有通过核验的实质应答", r.Code, r.Title)
		if len(cs) == 0 {
			detail += "（未映射到批准大纲的任何章节）"
		} else {
			detail += "（" + strings.Join(notes, "；") + "）"
		}
		out = append(out, omission{reqID: r.ID, kind: "omission:" + kind, label: label, sectionIDs: uniqueIDs(ids), detail: detail})
	}
	return out, nil
}

// GetConsistencyRun 返回一次运行及其结论；runID = 0 时返回最新运行；status 非空时过滤结论状态。
func (d *DB) GetConsistencyRun(ctx context.Context, tenant string, taskID, runID int64, status string) (*ConsistencyRunDetail, error) {
	tenant = tenantOr(tenant)
	var out *ConsistencyRunDetail
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = consistencyRunTx(ctx, tx, tenant, taskID, runID, status)
		return err
	})
	return out, err
}

func consistencyRunTx(ctx context.Context, q queryer, tenant string, taskID, runID int64, status string) (*ConsistencyRunDetail, error) {
	query := `SELECT ` + consistencyRunCols + ` FROM consistency_runs WHERE tenant_id = $1 AND task_id = $2`
	args := []any{tenant, taskID}
	if runID > 0 {
		query += ` AND id = $3`
		args = append(args, runID)
	} else {
		query += ` ORDER BY id DESC LIMIT 1`
	}
	run, err := scanConsistencyRun(q.QueryRowContext(ctx, query, args...))
	if err != nil {
		return nil, err
	}
	det := &ConsistencyRunDetail{Run: run, Findings: []*ConsistencyFinding{}}
	fq := `SELECT ` + findingCols + findingFrom + ` WHERE f.tenant_id = $1 AND f.run_id = $2`
	fargs := []any{tenant, run.ID}
	if status != "" {
		fq += ` AND f.status = $3`
		fargs = append(fargs, status)
	}
	rows, err := q.QueryContext(ctx, fq+` ORDER BY f.kind, f.id`, fargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		det.Findings = append(det.Findings, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	det.Current, err = runIsCurrentTx(ctx, q, tenant, taskID, run)
	return det, err
}

// runIsCurrentTx：运行未 stale、属于当前批准大纲，且快照与各章节 current_rev_no 完全一致。
func runIsCurrentTx(ctx context.Context, q queryer, tenant string, taskID int64, run *ConsistencyRun) (bool, error) {
	if run.Status != RunCompleted {
		return false, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT s.id, s.current_rev_no FROM proposal_sections s JOIN proposal_outlines o ON o.id = s.outline_id
		WHERE s.tenant_id = $1 AND s.task_id = $2 AND o.status = 'approved'`, tenant, taskID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var rev int
		if err := rows.Scan(&id, &rev); err != nil {
			return false, err
		}
		n++
		if v, ok := run.Snapshot[strconv.FormatInt(id, 10)]; !ok || v != rev {
			return false, rows.Err()
		}
	}
	return n > 0 && n == len(run.Snapshot), rows.Err()
}

// ListConsistencyRuns 返回任务的一致性核验运行（新到旧）。
func (d *DB) ListConsistencyRuns(ctx context.Context, tenant string, taskID int64, limit int) ([]*ConsistencyRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := d.QueryContext(ctx, `SELECT `+consistencyRunCols+` FROM consistency_runs WHERE tenant_id = $1 AND task_id = $2 ORDER BY id DESC LIMIT $3`,
		tenantOr(tenant), taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ConsistencyRun{}
	for rows.Next() {
		r, err := scanConsistencyRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveFindingInput：Kind 为 fixed / accepted / false_positive；accepted 与 false_positive 必须填写说明。
type ResolveFindingInput struct {
	Version int    `json:"version"`
	Kind    string `json:"resolution_kind"`
	Note    string `json:"note"`
}

// ResolveConsistencyFinding 以 version 乐观锁处理一条 open 结论；stale 结论须重新核验，不能处理。
func (d *DB) ResolveConsistencyFinding(ctx context.Context, tenant string, taskID, findingID int64, in ResolveFindingInput, actor string) (*ConsistencyFinding, error) {
	tenant = tenantOr(tenant)
	in.Note = strings.TrimSpace(in.Note)
	switch in.Kind {
	case "fixed":
	case "accepted", "false_positive":
		if in.Note == "" {
			return nil, fmt.Errorf("%w: note is required for %s", ErrInvalidFinding, in.Kind)
		}
	default:
		return nil, fmt.Errorf("%w: resolution_kind must be fixed, accepted or false_positive", ErrInvalidFinding)
	}
	if in.Version <= 0 {
		return nil, fmt.Errorf("%w: version is required", ErrInvalidFinding)
	}
	var out *ConsistencyFinding
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		var status string
		var version int
		err := tx.QueryRowContext(ctx, `SELECT status, version FROM consistency_findings WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR UPDATE`,
			tenant, taskID, findingID).Scan(&status, &version)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case status == FindingStale:
			return ErrFindingStale
		case version != in.Version:
			return fmt.Errorf("%w: expected version %d, current %d", ErrFindingConflict, in.Version, version)
		case status != FindingOpen:
			return fmt.Errorf("%w: finding is already %s", ErrFindingConflict, status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE consistency_findings SET status = 'resolved', resolution_kind = $2, resolution_note = $3,
			resolved_by = $4, resolved_at = now(), approved_by = $4, updated_by = $4, version = version + 1 WHERE id = $1`,
			findingID, in.Kind, in.Note, actor); err != nil {
			return err
		}
		out, err = scanFinding(tx.QueryRowContext(ctx, `SELECT `+findingCols+findingFrom+` WHERE f.id = $1`, findingID))
		return err
	})
	return out, err
}

// GetConsistencyFinding 读取单条结论。
func (d *DB) GetConsistencyFinding(ctx context.Context, tenant string, taskID, findingID int64) (*ConsistencyFinding, error) {
	return scanFinding(d.QueryRowContext(ctx, `SELECT `+findingCols+findingFrom+` WHERE f.tenant_id = $1 AND f.task_id = $2 AND f.id = $3`,
		tenantOr(tenant), taskID, findingID))
}
