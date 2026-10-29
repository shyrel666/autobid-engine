package db

// Phase 5（域 C）：可复核的完整投标要求清单（requirement_items）与任务级清单确认状态（requirement_manifests）。
// 清单来自对任务招标原文分块与已入图招标要素的逐条遍历，再经人工增删改与确认；检索 Top-K 只用于为每条要求找证据，
// 查无证据记录在 evidence_gap，不从清单中删除。清单的实质变更由触发器递增 epoch 并使审批中 / 已批准大纲失效，
// Go 层随后在同一事务内派生新版本草案大纲（见 proposal.go）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 要求类别（requirement_items.category CHECK）。
const (
	ReqScoringItem        = "scoring_item"
	ReqMandatoryClause    = "mandatory_clause"
	ReqNonScoringRequired = "non_scoring_required"
	ReqFormatAttachment   = "format_attachment"
)

// 要求复核状态与清单状态。
const (
	ReviewDraft         = "draft"
	ReviewConfirmed     = "confirmed"
	ReviewNeedsRevision = "needs_revision"

	ManifestDraft     = "draft"
	ManifestConfirmed = "confirmed"
	ManifestStale     = "stale"
)

var (
	ErrInvalidRequirement       = errors.New("proposal: invalid requirement")
	ErrRequirementsNotConfirmed = errors.New("proposal: requirement manifest not confirmed for this epoch")
	ErrRequirementReview        = errors.New("proposal: requirements need revision before confirmation")
)

func validRequirementCategory(c string) bool {
	switch c {
	case ReqScoringItem, ReqMandatoryClause, ReqNonScoringRequired, ReqFormatAttachment:
		return true
	}
	return false
}

// RequirementItem 是一条可复核的投标要求。MappedSectionCodes 为最新大纲版本中映射到该要求的章节（只读计算字段）。
type RequirementItem struct {
	ID                 int64           `json:"id"`
	TenantID           string          `json:"tenant_id"`
	TaskID             int64           `json:"task_id"`
	Code               string          `json:"code"`
	Category           string          `json:"category"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	IsMandatory        bool            `json:"is_mandatory"`
	ScoreWeight        float64         `json:"score_weight"`
	ScoreGroup         string          `json:"score_group"` // 评分项分组：technical / commercial / price，空串为未分类
	Origin             string          `json:"origin"`
	SourceAssetID      *int64          `json:"source_asset_id,omitempty"`
	SourceDocID        *int64          `json:"source_doc_id,omitempty"`
	SourceChunkID      *int64          `json:"source_chunk_id,omitempty"`
	SourceHash         string          `json:"source_hash"`
	SourceLoc          json.RawMessage `json:"source_loc"`
	EvidenceRunID      *int64          `json:"evidence_run_id,omitempty"`
	EvidenceIDs        []int64         `json:"evidence_ids"`
	EvidenceGap        string          `json:"evidence_gap"`
	ReviewStatus       string          `json:"review_status"`
	CreatedBy          string          `json:"created_by"`
	UpdatedBy          string          `json:"updated_by"`
	ApprovedBy         string          `json:"approved_by"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	MappedSectionCodes []string        `json:"mapped_section_codes"`
}

// RequirementManifest 是任务要求清单的整体确认状态；epoch 随清单实质变更递增。
type RequirementManifest struct {
	TenantID    string     `json:"tenant_id"`
	TaskID      int64      `json:"task_id"`
	Epoch       int64      `json:"epoch"`
	Status      string     `json:"status"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	ApprovedBy  string     `json:"approved_by"`
	UpdatedBy   string     `json:"updated_by"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// RequirementStats 汇总清单规模、复核、证据与大纲映射覆盖情况。
type RequirementStats struct {
	Total          int            `json:"total"`
	ByCategory     map[string]int `json:"by_category"`
	Mandatory      int            `json:"mandatory"`
	Confirmed      int            `json:"confirmed"`
	NeedsRevision  int            `json:"needs_revision"`
	WithEvidence   int            `json:"with_evidence"`
	EvidenceGaps   int            `json:"evidence_gaps"`   // 已记录缺口说明
	NoEvidence     int            `json:"no_evidence"`     // 既无证据也无缺口说明（尚未检索 / 复核）
	OutlineVersion int            `json:"outline_version"` // 映射统计所依据的最新大纲版本，0 表示尚无大纲
	Mapped         int            `json:"mapped"`
	Unmapped       []string       `json:"unmapped"`
	OutOfScope     int            `json:"out_of_scope"` // 价格评分项：由报价文件响应，不进入技术标大纲
}

type RequirementsView struct {
	Manifest *RequirementManifest `json:"manifest"`
	Items    []*RequirementItem   `json:"items"`
	Stats    RequirementStats     `json:"stats"`
	Scoring  []ScoringSummary     `json:"scoring"` // 按分包汇总的评分分值与合计校验提示
}

const reqCols = `id, tenant_id, task_id, code, category, title, description, is_mandatory, score_weight, score_group, origin,
	source_asset_id, source_doc_id, source_chunk_id, source_hash, source_loc, evidence_run_id, to_jsonb(evidence_ids),
	evidence_gap, review_status, created_by, updated_by, approved_by, created_at, updated_at`

func scanRequirement(r rowScanner) (*RequirementItem, error) {
	var x RequirementItem
	var loc, ev []byte
	err := r.Scan(&x.ID, &x.TenantID, &x.TaskID, &x.Code, &x.Category, &x.Title, &x.Description, &x.IsMandatory, &x.ScoreWeight, &x.ScoreGroup, &x.Origin,
		&x.SourceAssetID, &x.SourceDocID, &x.SourceChunkID, &x.SourceHash, &loc, &x.EvidenceRunID, &ev,
		&x.EvidenceGap, &x.ReviewStatus, &x.CreatedBy, &x.UpdatedBy, &x.ApprovedBy, &x.CreatedAt, &x.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	x.SourceLoc = loc
	x.EvidenceIDs = int64sJSON(ev)
	x.MappedSectionCodes = []string{}
	return &x, nil
}

// pgTextArray 把字符串列表编码为 PostgreSQL 数组字面量（配合 $n::text[] 使用），元素一律加引号转义。
func pgTextArray(vals []string) string {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = `"` + esc.Replace(v) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// pgInt64Array 把 ID 列表编码为 PostgreSQL 数组字面量（配合 $n::bigint[] 使用）。
func pgInt64Array(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func int64sJSON(raw []byte) []int64 {
	out := []int64{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func uniqueIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ensureManifestTx 保证任务存在清单行，并返回当前状态（FOR UPDATE 时加行锁）。
func ensureManifestTx(ctx context.Context, q queryer, tenant string, taskID int64, lock bool) (*RequirementManifest, error) {
	if _, err := q.ExecContext(ctx, `INSERT INTO requirement_manifests(tenant_id, task_id) SELECT $1, $2
		WHERE EXISTS (SELECT 1 FROM tasks WHERE id = $2) ON CONFLICT DO NOTHING`, tenant, taskID); err != nil {
		return nil, err
	}
	query := `SELECT tenant_id, task_id, epoch, status, confirmed_at, approved_by, updated_by, updated_at
		FROM requirement_manifests WHERE tenant_id = $1 AND task_id = $2`
	if lock {
		query += ` FOR UPDATE`
	}
	var m RequirementManifest
	var at sql.NullTime
	err := q.QueryRowContext(ctx, query, tenant, taskID).Scan(&m.TenantID, &m.TaskID, &m.Epoch, &m.Status, &at, &m.ApprovedBy, &m.UpdatedBy, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	m.ConfirmedAt = nullTimePtr(at)
	return &m, err
}

// RequirementManifest 返回任务清单确认状态（不存在时初始化为 epoch 1 草稿）。
func (d *DB) RequirementManifest(ctx context.Context, tenant string, taskID int64) (*RequirementManifest, error) {
	return ensureManifestTx(ctx, d, tenantOr(tenant), taskID, false)
}

// ListRequirements 返回清单、确认状态与覆盖统计；映射统计依据任务最新版本的大纲。
func (d *DB) ListRequirements(ctx context.Context, tenant string, taskID int64) (*RequirementsView, error) {
	tenant = tenantOr(tenant)
	m, err := d.RequirementManifest(ctx, tenant, taskID)
	if err != nil {
		return nil, err
	}
	items, err := listRequirementsTx(ctx, d, tenant, taskID)
	if err != nil {
		return nil, err
	}
	view := &RequirementsView{Manifest: m, Items: items, Stats: RequirementStats{ByCategory: map[string]int{}, Unmapped: []string{}}}
	var outlineID int64
	err = d.QueryRowContext(ctx, `SELECT id, version FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2
		ORDER BY version DESC LIMIT 1`, tenant, taskID).Scan(&outlineID, &view.Stats.OutlineVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	mapped := map[int64][]string{}
	if outlineID > 0 {
		rows, err := d.QueryContext(ctx, `SELECT m.requirement_id, s.section_code FROM proposal_section_requirements m
			JOIN proposal_sections s ON s.tenant_id = m.tenant_id AND s.id = m.section_id
			WHERE m.tenant_id = $1 AND s.outline_id = $2 ORDER BY s.seq`, tenant, outlineID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var code string
			if err := rows.Scan(&id, &code); err != nil {
				rows.Close()
				return nil, err
			}
			mapped[id] = append(mapped[id], code)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	view.Scoring = summarizeScoring(items)
	s := &view.Stats
	for _, it := range items {
		s.Total++
		s.ByCategory[it.Category]++
		if it.IsMandatory {
			s.Mandatory++
		}
		switch it.ReviewStatus {
		case ReviewConfirmed:
			s.Confirmed++
		case ReviewNeedsRevision:
			s.NeedsRevision++
		}
		if codes := mapped[it.ID]; !it.InTechnicalScope() && len(codes) == 0 {
			s.OutOfScope++ // 价格评分项：不需要证据与大纲映射
			continue
		}
		switch {
		case len(it.EvidenceIDs) > 0:
			s.WithEvidence++
		case strings.TrimSpace(it.EvidenceGap) != "":
			s.EvidenceGaps++
		default:
			s.NoEvidence++
		}
		if codes := mapped[it.ID]; len(codes) > 0 {
			it.MappedSectionCodes = codes
			s.Mapped++
		} else {
			s.Unmapped = append(s.Unmapped, it.Code)
		}
	}
	return view, nil
}

func listRequirementsTx(ctx context.Context, q queryer, tenant string, taskID int64) ([]*RequirementItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+reqCols+` FROM requirement_items WHERE tenant_id = $1 AND task_id = $2
		ORDER BY CASE category WHEN 'mandatory_clause' THEN 0 WHEN 'scoring_item' THEN 1 WHEN 'non_scoring_required' THEN 2 ELSE 3 END, code`, tenant, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RequirementItem{}
	for rows.Next() {
		it, err := scanRequirement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetRequirement 读取单条要求（限定租户与任务）。
func (d *DB) GetRequirement(ctx context.Context, tenant string, taskID, id int64) (*RequirementItem, error) {
	return scanRequirement(d.QueryRowContext(ctx, `SELECT `+reqCols+` FROM requirement_items WHERE tenant_id = $1 AND task_id = $2 AND id = $3`,
		tenantOr(tenant), taskID, id))
}

// RequirementInput 是人工新增 / 修订要求的字段；修订时 nil 字段保持不变。
type RequirementInput struct {
	Code          *string          `json:"code"`
	Category      *string          `json:"category"`
	Title         *string          `json:"title"`
	Description   *string          `json:"description"`
	IsMandatory   *bool            `json:"is_mandatory"`
	ScoreWeight   *float64         `json:"score_weight"`
	ScoreGroup    *string          `json:"score_group"`
	SourceChunkID *int64           `json:"source_chunk_id"`
	SourceLoc     *json.RawMessage `json:"source_loc"`
	EvidenceRunID *int64           `json:"evidence_run_id"`
	EvidenceIDs   *[]int64         `json:"evidence_ids"`
	EvidenceGap   *string          `json:"evidence_gap"`
	ReviewStatus  *string          `json:"review_status"`
}

type chunkSource struct {
	DocID, ChunkID int64
	Hash           string
	Loc            map[string]any
	Content        string
	Tables         []byte
	Mandatory      bool
	SectionCode    string
	SectionTitle   string
	Level          int
}

const chunkSourceSQL = `SELECT c.id, c.doc_id, c.seq, c.level, c.section_code, c.section_title, COALESCE(c.page_start, c.page_no),
	COALESCE(c.page_end, c.page_no), c.content, COALESCE(c.tables_json, '[]'::jsonb), c.is_mandatory, d.tender_no, d.lot_no,
	COALESCE(d.file_name, ''), d.title
	FROM document_chunks c JOIN tender_documents d ON d.id = c.doc_id`

func scanChunkSource(r rowScanner) (*chunkSource, error) {
	var (
		cs                               chunkSource
		seq, ps, pe                      int
		tenderNo, lotNo, fileName, title string
	)
	err := r.Scan(&cs.ChunkID, &cs.DocID, &seq, &cs.Level, &cs.SectionCode, &cs.SectionTitle, &ps, &pe, &cs.Content, &cs.Tables, &cs.Mandatory,
		&tenderNo, &lotNo, &fileName, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	cs.Hash = ContentHash(cs.Content + "\x00" + string(cs.Tables))
	cs.Loc = map[string]any{"doc_id": cs.DocID, "chunk_id": cs.ChunkID, "seq": seq, "section_code": cs.SectionCode,
		"section_title": cs.SectionTitle, "page_start": ps, "page_end": pe, "tender_no": tenderNo, "lot_no": lotNo,
		"file_name": fileName, "doc_title": title}
	return &cs, nil
}

func chunkSourceTx(ctx context.Context, q queryer, tenant string, taskID, chunkID int64) (*chunkSource, error) {
	cs, err := scanChunkSource(q.QueryRowContext(ctx, chunkSourceSQL+` WHERE c.id = $1 AND c.task_id = $2 AND d.tenant_id = $3`, chunkID, taskID, tenant))
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: source chunk %d does not belong to this task", ErrInvalidRequirement, chunkID)
	}
	return cs, err
}

// validateEvidenceRefsTx 校验证据属于同租户且来自本任务（或未绑定任务的租户级检索）的运行。
func validateEvidenceRefsTx(ctx context.Context, q queryer, tenant string, taskID int64, runID *int64, ids []int64) error {
	if runID != nil {
		var ok bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM retrieval_runs WHERE tenant_id = $1 AND id = $2 AND (task_id IS NULL OR task_id = $3))`,
			tenant, *runID, taskID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: retrieval run %d not found for this task", ErrEvidenceInvalid, *runID)
		}
	}
	states, err := evidenceStatesTx(ctx, q, tenant, taskID, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := states[id]; !ok {
			return fmt.Errorf("%w: evidence %d not found for this task", ErrEvidenceInvalid, id)
		}
	}
	return nil
}

// CreateRequirement 人工新增一条要求（origin=manual）。
func (d *DB) CreateRequirement(ctx context.Context, tenant string, taskID int64, actor string, in RequirementInput) (*RequirementItem, error) {
	tenant = tenantOr(tenant)
	if in.Code == nil || strings.TrimSpace(*in.Code) == "" || in.Title == nil || strings.TrimSpace(*in.Title) == "" ||
		in.Category == nil || !validRequirementCategory(*in.Category) {
		return nil, fmt.Errorf("%w: code, title and a valid category are required", ErrInvalidRequirement)
	}
	var out *RequirementItem
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		before, err := lockActiveOutlineTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		x := RequirementItem{Code: strings.TrimSpace(*in.Code), Category: *in.Category, Title: strings.TrimSpace(*in.Title),
			SourceLoc: json.RawMessage(`{}`), ReviewStatus: ReviewDraft}
		if err := applyRequirementInput(ctx, tx, tenant, taskID, &x, in); err != nil {
			return err
		}
		out, err = scanRequirement(tx.QueryRowContext(ctx, `INSERT INTO requirement_items(tenant_id, task_id, code, category, title, description,
			is_mandatory, score_weight, origin, source_doc_id, source_chunk_id, source_hash, source_loc, evidence_run_id, evidence_ids,
			evidence_gap, review_status, created_by, updated_by, score_group)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'manual',$9,$10,$11,$12::jsonb,$13,$14::bigint[],$15,$16,$17,$17,$18)
			ON CONFLICT (tenant_id, task_id, code) DO NOTHING RETURNING `+reqCols,
			tenant, taskID, x.Code, x.Category, x.Title, x.Description, x.IsMandatory, x.ScoreWeight, x.SourceDocID, x.SourceChunkID,
			x.SourceHash, string(x.SourceLoc), x.EvidenceRunID, pgInt64Array(x.EvidenceIDs), x.EvidenceGap, x.ReviewStatus, actor, x.ScoreGroup))
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: requirement code %q already exists", ErrProposalConflict, x.Code)
		}
		if err != nil {
			return err
		}
		return afterRequirementChangeTx(ctx, tx, tenant, taskID, actor, before)
	})
	return out, err
}

// applyRequirementInput 把输入合并进 x 并做归属校验（原文分块、证据须属于本任务）。
func applyRequirementInput(ctx context.Context, q queryer, tenant string, taskID int64, x *RequirementItem, in RequirementInput) error {
	if in.Code != nil {
		x.Code = strings.TrimSpace(*in.Code)
	}
	if in.Category != nil {
		x.Category = *in.Category
	}
	if in.Title != nil {
		x.Title = strings.TrimSpace(*in.Title)
	}
	if x.Code == "" || x.Title == "" || !validRequirementCategory(x.Category) {
		return fmt.Errorf("%w: code, title and a valid category are required", ErrInvalidRequirement)
	}
	if in.Description != nil {
		x.Description = *in.Description
	}
	if in.IsMandatory != nil {
		x.IsMandatory = *in.IsMandatory
	}
	if in.ScoreWeight != nil {
		if *in.ScoreWeight < 0 {
			return fmt.Errorf("%w: score_weight must be >= 0", ErrInvalidRequirement)
		}
		x.ScoreWeight = *in.ScoreWeight
	}
	if in.ScoreGroup != nil {
		if !validScoreGroup(*in.ScoreGroup) {
			return fmt.Errorf("%w: invalid score_group %q", ErrInvalidRequirement, *in.ScoreGroup)
		}
		x.ScoreGroup = *in.ScoreGroup
	}
	if x.Category != ReqScoringItem {
		x.ScoreGroup = ""
	}
	if in.SourceChunkID != nil {
		if *in.SourceChunkID <= 0 {
			x.SourceChunkID, x.SourceDocID, x.SourceHash, x.SourceLoc = nil, nil, "", json.RawMessage(`{}`)
		} else {
			cs, err := chunkSourceTx(ctx, q, tenant, taskID, *in.SourceChunkID)
			if err != nil {
				return err
			}
			loc, _ := json.Marshal(cs.Loc)
			x.SourceChunkID, x.SourceDocID, x.SourceHash, x.SourceLoc = &cs.ChunkID, &cs.DocID, cs.Hash, loc
		}
	}
	if in.SourceLoc != nil {
		if !json.Valid(*in.SourceLoc) {
			return fmt.Errorf("%w: source_loc must be JSON", ErrInvalidRequirement)
		}
		var merged map[string]any
		_ = json.Unmarshal(x.SourceLoc, &merged)
		var extra map[string]any
		if err := json.Unmarshal(*in.SourceLoc, &extra); err != nil {
			return fmt.Errorf("%w: source_loc must be a JSON object", ErrInvalidRequirement)
		}
		if merged == nil {
			merged = map[string]any{}
		}
		for k, v := range extra {
			merged[k] = v
		}
		x.SourceLoc, _ = json.Marshal(merged)
	}
	if in.EvidenceRunID != nil {
		if *in.EvidenceRunID <= 0 {
			x.EvidenceRunID = nil
		} else {
			x.EvidenceRunID = in.EvidenceRunID
		}
	}
	if in.EvidenceIDs != nil {
		x.EvidenceIDs = uniqueIDs(*in.EvidenceIDs)
	}
	if x.EvidenceIDs == nil {
		x.EvidenceIDs = []int64{}
	}
	if in.EvidenceGap != nil {
		x.EvidenceGap = strings.TrimSpace(*in.EvidenceGap)
	}
	if in.ReviewStatus != nil {
		switch *in.ReviewStatus {
		case ReviewDraft, ReviewConfirmed, ReviewNeedsRevision:
			x.ReviewStatus = *in.ReviewStatus
		default:
			return fmt.Errorf("%w: invalid review_status %q", ErrInvalidRequirement, *in.ReviewStatus)
		}
	}
	if in.EvidenceRunID != nil || in.EvidenceIDs != nil {
		return validateEvidenceRefsTx(ctx, q, tenant, taskID, x.EvidenceRunID, x.EvidenceIDs)
	}
	return nil
}

// UpdateRequirement 人工修订要求或绑定证据。实质字段变化（编码、类别、标题、描述、强制性、分值、原文来源）
// 使清单 epoch 递增并使审批中 / 已批准大纲失效；仅证据绑定或复核状态变化不影响大纲。
func (d *DB) UpdateRequirement(ctx context.Context, tenant string, taskID, id int64, actor string, in RequirementInput) (*RequirementItem, error) {
	tenant = tenantOr(tenant)
	var out *RequirementItem
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		before, err := lockActiveOutlineTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		x, err := scanRequirement(tx.QueryRowContext(ctx, `SELECT `+reqCols+` FROM requirement_items
			WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR UPDATE`, tenant, taskID, id))
		if err != nil {
			return err
		}
		old := *x
		if err := applyRequirementInput(ctx, tx, tenant, taskID, x, in); err != nil {
			return err
		}
		substantive := old.Code != x.Code || old.Category != x.Category || old.Title != x.Title || old.Description != x.Description ||
			old.IsMandatory != x.IsMandatory || old.ScoreWeight != x.ScoreWeight || old.ScoreGroup != x.ScoreGroup ||
			!sameInt64Ptr(old.SourceChunkID, x.SourceChunkID) || string(old.SourceLoc) != string(x.SourceLoc)
		if substantive && in.ReviewStatus == nil {
			x.ReviewStatus = ReviewDraft
		}
		approvedBy := x.ApprovedBy
		if x.ReviewStatus == ReviewConfirmed && old.ReviewStatus != ReviewConfirmed {
			approvedBy = actor
		}
		out, err = scanRequirement(tx.QueryRowContext(ctx, `UPDATE requirement_items SET code = $4, category = $5, title = $6, description = $7,
			is_mandatory = $8, score_weight = $9, source_doc_id = $10, source_chunk_id = $11, source_hash = $12, source_loc = $13::jsonb,
			evidence_run_id = $14, evidence_ids = $15::bigint[], evidence_gap = $16, review_status = $17, approved_by = $18, updated_by = $19,
			score_group = $20
			WHERE tenant_id = $1 AND task_id = $2 AND id = $3 RETURNING `+reqCols,
			tenant, taskID, id, x.Code, x.Category, x.Title, x.Description, x.IsMandatory, x.ScoreWeight, x.SourceDocID, x.SourceChunkID,
			x.SourceHash, string(x.SourceLoc), x.EvidenceRunID, pgInt64Array(x.EvidenceIDs), x.EvidenceGap, x.ReviewStatus, approvedBy, actor,
			x.ScoreGroup))
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: requirement code %q already exists", ErrProposalConflict, x.Code)
			}
			return err
		}
		return afterRequirementChangeTx(ctx, tx, tenant, taskID, actor, before)
	})
	return out, err
}

func sameInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// DeleteRequirement 显式删除一条要求（清单 epoch 递增，大纲失效）。
func (d *DB) DeleteRequirement(ctx context.Context, tenant string, taskID, id int64, actor string) error {
	tenant = tenantOr(tenant)
	return d.InTx(ctx, func(tx *sql.Tx) error {
		before, err := lockActiveOutlineTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		// 先记录操作人（非实质字段，不触发失效），删除触发器以 OLD.updated_by 作为失效操作人。
		res, err := tx.ExecContext(ctx, `UPDATE requirement_items SET updated_by = $4 WHERE tenant_id = $1 AND task_id = $2 AND id = $3`,
			tenant, taskID, id, actor)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM requirement_items WHERE tenant_id = $1 AND task_id = $2 AND id = $3`, tenant, taskID, id); err != nil {
			return err
		}
		return afterRequirementChangeTx(ctx, tx, tenant, taskID, actor, before)
	})
}

// ConfirmRequirements 人工确认当前 epoch 的要求清单已完整。expectedEpoch > 0 时须与当前 epoch 一致（防止确认过期视图）。
func (d *DB) ConfirmRequirements(ctx context.Context, tenant string, taskID int64, actor string, expectedEpoch int64) (*RequirementManifest, error) {
	tenant = tenantOr(tenant)
	var out *RequirementManifest
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		m, err := ensureManifestTx(ctx, tx, tenant, taskID, true)
		if err != nil {
			return err
		}
		if expectedEpoch > 0 && expectedEpoch != m.Epoch {
			return fmt.Errorf("%w: manifest epoch is %d, not %d", ErrProposalConflict, m.Epoch, expectedEpoch)
		}
		var total, needs int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE review_status = 'needs_revision')
			FROM requirement_items WHERE tenant_id = $1 AND task_id = $2`, tenant, taskID).Scan(&total, &needs); err != nil {
			return err
		}
		if total == 0 {
			return fmt.Errorf("%w: requirement list is empty", ErrRequirementReview)
		}
		if needs > 0 {
			return fmt.Errorf("%w: %d item(s) marked needs_revision", ErrRequirementReview, needs)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE requirement_items SET review_status = 'confirmed', approved_by = $3, updated_by = $3
			WHERE tenant_id = $1 AND task_id = $2 AND review_status <> 'confirmed'`, tenant, taskID, actor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE requirement_manifests SET status = 'confirmed', confirmed_at = now(), approved_by = $3, updated_by = $3
			WHERE tenant_id = $1 AND task_id = $2`, tenant, taskID, actor); err != nil {
			return err
		}
		out, err = ensureManifestTx(ctx, tx, tenant, taskID, false)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 要求清单提取：遍历任务已入图的招标要素（scoring_item / qualification / clause / commercial_item）与全部原文分块。
// ---------------------------------------------------------------------------

// ExtractResult 汇总一次幂等提取：同编码且原文未变的要求保持不变；原文已变的提取项更新并标记 needs_revision；
// 人工维护（origin=manual）的同编码要求不被覆盖。
// ScoringItems 为本次从评分表结构化解析出的评分项数；Orphaned 为本次未再产出的旧提取项（规则升级或原文变化），
// 它们被标记为 needs_revision，须人工确认后删除或保留。
type ExtractResult struct {
	Created      int      `json:"created"`
	Updated      int      `json:"updated"`
	Unchanged    int      `json:"unchanged"`
	Skipped      int      `json:"skipped_manual"`
	Scanned      int      `json:"scanned_chunks"`
	ScoringItems int      `json:"scoring_items"`
	Orphaned     int      `json:"orphaned"`
	Codes        []string `json:"codes"`
	OrphanCodes  []string `json:"orphan_codes"`
}

type reqCandidate struct {
	code, category, title, description string
	mandatory                          bool
	score                              float64
	scoreGroup                         string
	tableItem                          bool // 来自结构化评分表
	assetID                            *int64
	src                                *chunkSource
	loc                                map[string]any
}

var (
	reFormat     = regexp.MustCompile(`格式|附件|装订|签字|签章|盖章|密封|封面|份数|正本|副本|电子版|投标文件组成`)
	reScoring    = regexp.MustCompile(`评分|评审标准|评审因素|评标标准|评标办法|得分|分值`)
	reObligation = regexp.MustCompile(`必须|须提供|应提供|应满足|须满足|不得|应具备|须具备|须承诺|应承诺|要求提供`)
	reScore      = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*分`)
)

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n。；;"); i > 0 {
		s = s[:i]
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

func chunkTitle(cs *chunkSource) string {
	t := strings.TrimSpace(strings.TrimSpace(cs.SectionCode + " " + cs.SectionTitle))
	if line := firstLine(cs.Content, 60); line != "" && (t == "" || cs.Level == 0) {
		return line
	}
	if t == "" {
		return fmt.Sprintf("原文段落 %d", cs.ChunkID)
	}
	return t
}

func cloneLoc(loc map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(loc)+len(kv)/2)
	for k, v := range loc {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func extractCandidatesTx(ctx context.Context, q queryer, tenant string, taskID int64) ([]reqCandidate, int, error) {
	chunks := map[int64]*chunkSource{}
	var order []*chunkSource
	rows, err := q.QueryContext(ctx, chunkSourceSQL+` WHERE c.task_id = $1 AND d.tenant_id = $2 ORDER BY c.doc_id, c.seq`, taskID, tenant)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		cs, err := scanChunkSource(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		chunks[cs.ChunkID] = cs
		order = append(order, cs)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// 评分表优先结构化解析；有结构化评分项时，招标要素中的评分项与按段落猜测的评分项都不再产出（避免重复计分）。
	out, scoringChunks, scoringDocs := scoringCandidates(order)
	covered := map[int64]bool{} // 已由招标要素覆盖的原文分块
	arows, err := q.QueryContext(ctx, `SELECT a.id, a.type, a.code, a.title, a.content, a.is_mandatory, COALESCE(a.score_max, 0)::float8, a.source_ref
		FROM assets a WHERE a.type IN ('scoring_item','qualification','clause','commercial_item')
		  AND (a.task_ids @> ARRAY[$1::bigint] OR EXISTS (SELECT 1 FROM task_asset_links l WHERE l.task_id = $1 AND l.asset_id = a.id))
		ORDER BY a.type, a.code`, taskID)
	if err != nil {
		return nil, 0, err
	}
	for arows.Next() {
		var (
			id                        int64
			typ, code, title, content string
			mandatory                 bool
			score                     float64
			ref                       []byte
		)
		if err := arows.Scan(&id, &typ, &code, &title, &content, &mandatory, &score, &ref); err != nil {
			arows.Close()
			return nil, 0, err
		}
		var sr struct {
			ChunkID int64 `json:"chunk_id"`
		}
		_ = json.Unmarshal(ref, &sr)
		if typ == AssetScoringItem && len(scoringDocs) > 0 {
			continue
		}
		c := reqCandidate{title: title, description: content, mandatory: mandatory, loc: map[string]any{"asset_id": id, "asset_code": code}}
		if c.title == "" {
			c.title = firstLine(content, 60)
		}
		if c.title == "" {
			c.title = code
		}
		assetID := id
		c.assetID = &assetID
		switch typ {
		case AssetScoringItem:
			c.code, c.category, c.score, c.scoreGroup = "SC-"+code, ReqScoringItem, score, classifyScoreGroup("", c.title)
		case AssetQualification:
			c.code, c.category = "QA-"+code, ReqNonScoringRequired
		case AssetClause:
			c.code, c.category = "CL-"+code, ReqNonScoringRequired
		default:
			c.code, c.category = "CM-"+code, ReqNonScoringRequired
		}
		if mandatory && typ != AssetScoringItem {
			c.category = ReqMandatoryClause
		}
		if cs := chunks[sr.ChunkID]; cs != nil {
			c.src = cs
			c.loc = cloneLoc(cs.Loc, "asset_id", id, "asset_code", code)
			covered[cs.ChunkID] = true
		}
		out = append(out, c)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, 0, err
	}

	for _, cs := range order {
		if covered[cs.ChunkID] || strings.TrimSpace(cs.Content) == "" {
			continue
		}
		docKey := chunkKey(cs)
		head := cs.SectionTitle + " " + firstLine(cs.Content, 80)
		scoringText := reScoring.MatchString(head) && reScore.MatchString(cs.Content)
		if scoringChunks[cs.ChunkID] || (scoringText && scoringDocs[cs.DocID]) {
			// 评分表所在段落与评分办法说明已由结构化评分项覆盖；段落本身含 ★/废标条款时仍单列强制项
			if cs.Mandatory {
				out = append(out, reqCandidate{code: "MC-" + docKey, category: ReqMandatoryClause, title: chunkTitle(cs), description: cs.Content,
					mandatory: true, src: cs, loc: cs.Loc})
			}
			continue
		}
		switch {
		case scoringText:
			score := 0.0
			if m := reScore.FindStringSubmatch(cs.Content); m != nil {
				score, _ = strconv.ParseFloat(m[1], 64)
			}
			title := chunkTitle(cs)
			out = append(out, reqCandidate{code: "SC-" + docKey, category: ReqScoringItem, title: title, description: cs.Content,
				mandatory: cs.Mandatory, score: score, scoreGroup: classifyScoreGroup(cs.SectionTitle, title), src: cs, loc: cs.Loc})
		case cs.Mandatory:
			out = append(out, reqCandidate{code: "MC-" + docKey, category: ReqMandatoryClause, title: chunkTitle(cs), description: cs.Content,
				mandatory: true, src: cs, loc: cs.Loc})
		case reFormat.MatchString(head):
			out = append(out, reqCandidate{code: "FA-" + docKey, category: ReqFormatAttachment, title: chunkTitle(cs), description: cs.Content,
				src: cs, loc: cs.Loc})
		case reObligation.MatchString(cs.Content):
			out = append(out, reqCandidate{code: "RQ-" + docKey, category: ReqNonScoringRequired, title: chunkTitle(cs), description: cs.Content,
				src: cs, loc: cs.Loc})
		}
	}
	return out, len(order), nil
}

// ExtractRequirements 幂等地从任务招标原文与招标要素同步要求清单草案。
func (d *DB) ExtractRequirements(ctx context.Context, tenant string, taskID int64, actor string) (*ExtractResult, error) {
	tenant = tenantOr(tenant)
	res := &ExtractResult{Codes: []string{}}
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		before, err := lockActiveOutlineTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		if _, err := ensureManifestTx(ctx, tx, tenant, taskID, true); err != nil {
			return err
		}
		cands, scanned, err := extractCandidatesTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		res.Scanned = scanned
		seen := map[string]bool{}
		for _, c := range cands {
			if seen[c.code] {
				continue
			}
			seen[c.code] = true
			if c.tableItem {
				res.ScoringItems++
			}
			var docID, chunkID *int64
			hash := ContentHash(c.title + "\x00" + c.description)
			if c.src != nil {
				docID, chunkID, hash = &c.src.DocID, &c.src.ChunkID, c.src.Hash
			}
			loc, _ := json.Marshal(c.loc)
			var origin, oldHash string
			err := tx.QueryRowContext(ctx, `SELECT origin, source_hash FROM requirement_items WHERE tenant_id = $1 AND task_id = $2 AND code = $3 FOR UPDATE`,
				tenant, taskID, c.code).Scan(&origin, &oldHash)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				if _, err := tx.ExecContext(ctx, `INSERT INTO requirement_items(tenant_id, task_id, code, category, title, description, is_mandatory,
					score_weight, origin, source_asset_id, source_doc_id, source_chunk_id, source_hash, source_loc, created_by, updated_by, score_group)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'extracted',$9,$10,$11,$12,$13::jsonb,$14,$14,$15)`,
					tenant, taskID, c.code, c.category, c.title, c.description, c.mandatory, c.score, c.assetID, docID, chunkID, hash, string(loc), actor,
					c.scoreGroup); err != nil {
					return fmt.Errorf("insert requirement %s: %w", c.code, err)
				}
				res.Created++
				res.Codes = append(res.Codes, c.code)
			case err != nil:
				return err
			case origin == "manual":
				res.Skipped++
			case oldHash == hash:
				res.Unchanged++
			default:
				if _, err := tx.ExecContext(ctx, `UPDATE requirement_items SET category = $4, title = $5, description = $6, is_mandatory = $7,
					score_weight = $8, source_asset_id = $9, source_doc_id = $10, source_chunk_id = $11, source_hash = $12, source_loc = $13::jsonb,
					review_status = 'needs_revision', updated_by = $14, score_group = $15 WHERE tenant_id = $1 AND task_id = $2 AND code = $3`,
					tenant, taskID, c.code, c.category, c.title, c.description, c.mandatory, c.score, c.assetID, docID, chunkID, hash, string(loc), actor,
					c.scoreGroup); err != nil {
					return err
				}
				res.Updated++
				res.Codes = append(res.Codes, c.code)
			}
		}
		// 本次未再产出的旧提取项（提取规则升级、原文已删改）不自动删除：标记待复核，由人工确认删除或保留。
		rows, err := tx.QueryContext(ctx, `SELECT code FROM requirement_items WHERE tenant_id = $1 AND task_id = $2 AND origin = 'extracted'
			AND review_status <> 'needs_revision' ORDER BY code`, tenant, taskID)
		if err != nil {
			return err
		}
		var orphans []string
		for rows.Next() {
			var code string
			if err := rows.Scan(&code); err != nil {
				rows.Close()
				return err
			}
			if !seen[code] {
				orphans = append(orphans, code)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		res.OrphanCodes = []string{}
		if len(orphans) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE requirement_items SET review_status = 'needs_revision', updated_by = $4
				WHERE tenant_id = $1 AND task_id = $2 AND code = ANY($3::text[])`, tenant, taskID, pgTextArray(orphans), actor); err != nil {
				return err
			}
			res.Orphaned, res.OrphanCodes = len(orphans), orphans
		}
		return afterRequirementChangeTx(ctx, tx, tenant, taskID, actor, before)
	})
	return res, err
}
