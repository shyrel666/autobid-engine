package db

// D2 企业证据域：业绩、原件与页码、要求绑定。企业证据是租户级全局对象，不随任务删除。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrEvidenceInadmissible = errors.New("d2: evidence inadmissible")
	ErrAdmissibilityManual  = errors.New("d2: admissibility requires manual review")
	ErrAdmissibilityMissing = errors.New("d2: admissibility run missing")
	ErrAdmissibilityStale   = errors.New("d2: admissibility run is not current")
	ErrBidDeadlineMissing   = errors.New("d2: task bid deadline is not set")
)

// 证据绑定域。
const (
	EvidenceDomainCredential  = "credential"
	EvidenceDomainPerformance = "performance"
)

// 原件文件类型与页角色。
const (
	DocKindCertificate      = "certificate"
	DocKindContract         = "contract"
	DocKindAcceptanceReport = "acceptance_report"
	DocKindSocialSecurity   = "social_security"
	DocKindProductCatalog   = "product_catalog"
	DocKindOther            = "other"
)

// CompanyPerformance 是一条业绩证据（租户级）。
type CompanyPerformance struct {
	ID             int64           `json:"id"`
	TenantID       string          `json:"tenant_id"`
	Code           string          `json:"code"`
	ProjectName    string          `json:"project_name"`
	Buyer          string          `json:"buyer,omitempty"`
	ContractNo     string          `json:"contract_no,omitempty"`
	ContractDate   *string         `json:"contract_date,omitempty"`
	AcceptanceDate *string         `json:"acceptance_date,omitempty"`
	Amount         string          `json:"amount,omitempty"`
	Currency       string          `json:"currency,omitempty"`
	SubjectName    string          `json:"subject_name,omitempty"`
	SubjectType    string          `json:"subject_type,omitempty"`
	IsSubsidiary   bool            `json:"is_subsidiary"`
	Scope          string          `json:"scope,omitempty"`
	Attributes     json.RawMessage `json:"attributes,omitempty"`
	DeletedAt      *time.Time      `json:"deleted_at,omitempty"`
	CreatedBy      string          `json:"created_by,omitempty"`
	UpdatedBy      string          `json:"updated_by,omitempty"`
	ApprovedBy     string          `json:"approved_by,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

const performanceCols = `id, tenant_id, code, project_name, buyer, contract_no, to_char(contract_date,'YYYY-MM-DD'),
	to_char(acceptance_date,'YYYY-MM-DD'), COALESCE(amount::text,''), currency, subject_name, subject_type, is_subsidiary, scope,
	attributes, deleted_at, created_by, updated_by, approved_by, created_at, updated_at`

func scanPerformance(r rowScanner) (*CompanyPerformance, error) {
	var (
		p                CompanyPerformance
		contract, accept sql.NullString
		attrs            []byte
		del              sql.NullTime
	)
	if err := r.Scan(&p.ID, &p.TenantID, &p.Code, &p.ProjectName, &p.Buyer, &p.ContractNo, &contract, &accept, &p.Amount, &p.Currency,
		&p.SubjectName, &p.SubjectType, &p.IsSubsidiary, &p.Scope, &attrs, &del, &p.CreatedBy, &p.UpdatedBy, &p.ApprovedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Attributes = attrs
	if contract.Valid {
		p.ContractDate = &contract.String
	}
	if accept.Valid {
		p.AcceptanceDate = &accept.String
	}
	p.DeletedAt = nullTimePtr(del)
	return &p, nil
}

func perfDates(contract, accept *string) (any, any, error) {
	parse := func(v *string, field string) (any, error) {
		if v == nil || strings.TrimSpace(*v) == "" {
			return nil, nil
		}
		s := strings.TrimSpace(*v)
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			return nil, fmt.Errorf("db: %s must be YYYY-MM-DD: %q", field, s)
		}
		return s, nil
	}
	c, err := parse(contract, "contract_date")
	if err != nil {
		return nil, nil, err
	}
	a, err := parse(accept, "acceptance_date")
	if err != nil {
		return nil, nil, err
	}
	return c, a, nil
}

func validatePerfSubject(t string) error {
	switch t {
	case "", "unknown", "bidder", "parent", "subsidiary", "other":
		return nil
	}
	return fmt.Errorf("db: invalid performance subject_type %q", t)
}

// UpsertCompanyPerformance 按 (tenant_id, code) 幂等写入业绩证据。
func (d *DB) UpsertCompanyPerformance(ctx context.Context, p *CompanyPerformance) (*CompanyPerformance, bool, error) {
	tenant := tenantOr(p.TenantID)
	code, name := strings.TrimSpace(p.Code), strings.TrimSpace(p.ProjectName)
	if code == "" || name == "" {
		return nil, false, errors.New("db: performance code and project_name are required")
	}
	subjectType := strings.TrimSpace(p.SubjectType)
	if subjectType == "" {
		subjectType = "unknown"
	}
	if err := validatePerfSubject(subjectType); err != nil {
		return nil, false, err
	}
	attrs, err := marshalPayload(p.Attributes)
	if err != nil {
		return nil, false, err
	}
	contract, accept, err := perfDates(p.ContractDate, p.AcceptanceDate)
	if err != nil {
		return nil, false, err
	}
	var amount any
	if strings.TrimSpace(p.Amount) != "" {
		amount = strings.TrimSpace(p.Amount)
	}
	currency := strings.TrimSpace(p.Currency)
	if currency == "" {
		currency = "CNY"
	}
	row := d.QueryRowContext(ctx, `INSERT INTO company_performance(tenant_id, code, project_name, buyer, contract_no, contract_date,
			acceptance_date, amount, currency, subject_name, subject_type, is_subsidiary, scope, attributes, created_by, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6::date,$7::date,$8::numeric,$9,$10,$11,$12,$13,$14::jsonb,COALESCE(NULLIF($15,''),''),COALESCE(NULLIF($15,''),''))
		ON CONFLICT (tenant_id, code) DO UPDATE SET project_name=EXCLUDED.project_name, buyer=EXCLUDED.buyer,
			contract_no=EXCLUDED.contract_no, contract_date=EXCLUDED.contract_date, acceptance_date=EXCLUDED.acceptance_date,
			amount=EXCLUDED.amount, currency=EXCLUDED.currency, subject_name=EXCLUDED.subject_name, subject_type=EXCLUDED.subject_type,
			is_subsidiary=EXCLUDED.is_subsidiary, scope=EXCLUDED.scope, attributes=company_performance.attributes || EXCLUDED.attributes,
			deleted_at=NULL, updated_by=EXCLUDED.updated_by
		RETURNING `+performanceCols+`, (xmax = 0)`, tenant, code, name, strings.TrimSpace(p.Buyer), strings.TrimSpace(p.ContractNo),
		contract, accept, amount, currency, strings.TrimSpace(p.SubjectName), subjectType, p.IsSubsidiary, strings.TrimSpace(p.Scope),
		string(attrs), p.CreatedBy)
	var (
		out     CompanyPerformance
		c, a    sql.NullString
		raw     []byte
		del     sql.NullTime
		created bool
	)
	if err := row.Scan(&out.ID, &out.TenantID, &out.Code, &out.ProjectName, &out.Buyer, &out.ContractNo, &c, &a, &out.Amount, &out.Currency,
		&out.SubjectName, &out.SubjectType, &out.IsSubsidiary, &out.Scope, &raw, &del, &out.CreatedBy, &out.UpdatedBy, &out.ApprovedBy,
		&out.CreatedAt, &out.UpdatedAt, &created); err != nil {
		return nil, false, err
	}
	out.Attributes = raw
	if c.Valid {
		out.ContractDate = &c.String
	}
	if a.Valid {
		out.AcceptanceDate = &a.String
	}
	out.DeletedAt = nullTimePtr(del)
	return &out, created, nil
}

// EvidenceOwnerExists 判断证明原件要挂靠的资信或业绩记录是否存在且未删除。
func (d *DB) EvidenceOwnerExists(ctx context.Context, tenant, domain string, id int64) (bool, error) {
	table := "company_credentials"
	if domain == EvidenceDomainPerformance {
		table = "company_performance"
	}
	var exists bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL)`,
		tenantOr(tenant), id).Scan(&exists)
	return exists, err
}

// GetCompanyPerformance 读取租户内一条业绩（含已软删除记录，供可采性判定为不可采）。
func (d *DB) GetCompanyPerformance(ctx context.Context, tenant string, id int64) (*CompanyPerformance, error) {
	p, err := scanPerformance(d.QueryRowContext(ctx, `SELECT `+performanceCols+` FROM company_performance WHERE tenant_id=$1 AND id=$2`, tenantOr(tenant), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: performance %d", ErrNotFound, id)
	}
	return p, err
}

// ListCompanyPerformance 列出租户内业绩；默认排除软删除。
func (d *DB) ListCompanyPerformance(ctx context.Context, tenant string, includeDeleted bool, limit int) ([]*CompanyPerformance, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := d.QueryContext(ctx, `SELECT `+performanceCols+` FROM company_performance
		WHERE tenant_id=$1 AND ($2 OR deleted_at IS NULL) ORDER BY id LIMIT $3`, tenantOr(tenant), includeDeleted, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CompanyPerformance
	for rows.Next() {
		p, err := scanPerformance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteCompanyPerformanceScoped 软删除一条业绩；已绑定记录仍保留供历史运行引用。
func (d *DB) DeleteCompanyPerformanceScoped(ctx context.Context, tenant string, id int64, actor string) error {
	res, err := d.ExecContext(ctx, `UPDATE company_performance SET deleted_at=now(), updated_by=COALESCE(NULLIF($3,''),updated_by)
		WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL`, tenantOr(tenant), id, actor)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: performance %d", ErrNotFound, id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 原件与页码
// ---------------------------------------------------------------------------

// CompanyEvidenceOriginal 是一份原件的登记：页码角色、内容哈希与替换状态。
type CompanyEvidenceOriginal struct {
	ID             int64      `json:"id"`
	TenantID       string     `json:"tenant_id"`
	OwnerDomain    string     `json:"owner_domain"`
	OwnerID        int64      `json:"owner_id"`
	DocumentKind   string     `json:"document_kind"`
	PageLabel      string     `json:"page_label"`
	OriginalPath   string     `json:"original_path"`
	OriginalSHA256 string     `json:"original_sha256"`
	PageStart      int        `json:"page_start"`
	PageEnd        int        `json:"page_end"`
	SupersededAt   *time.Time `json:"superseded_at,omitempty"`
	CreatedBy      string     `json:"created_by,omitempty"`
	UpdatedBy      string     `json:"updated_by,omitempty"`
	ApprovedBy     string     `json:"approved_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const originalCols = `id, tenant_id, owner_domain, owner_id, document_kind, page_label, original_path, original_sha256,
	page_start, page_end, superseded_at, created_by, updated_by, approved_by, created_at, updated_at`

func scanOriginal(r rowScanner) (*CompanyEvidenceOriginal, error) {
	var (
		o          CompanyEvidenceOriginal
		superseded sql.NullTime
	)
	if err := r.Scan(&o.ID, &o.TenantID, &o.OwnerDomain, &o.OwnerID, &o.DocumentKind, &o.PageLabel, &o.OriginalPath,
		&o.OriginalSHA256, &o.PageStart, &o.PageEnd, &superseded, &o.CreatedBy, &o.UpdatedBy, &o.ApprovedBy, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, err
	}
	o.SupersededAt = nullTimePtr(superseded)
	return &o, nil
}

func validOriginalKind(k string) bool {
	switch k {
	case DocKindCertificate, DocKindContract, DocKindAcceptanceReport, DocKindSocialSecurity, DocKindProductCatalog, DocKindOther:
		return true
	}
	return false
}

// AddCompanyEvidenceOriginal 登记一份原件。supersede=true 时先把同一证据、同一类型与页角色的当前原件置为已替换，
// 旧记录保留审计；可采性只使用 superseded_at IS NULL 的原件。
func (d *DB) AddCompanyEvidenceOriginal(ctx context.Context, o *CompanyEvidenceOriginal, supersede bool) (*CompanyEvidenceOriginal, error) {
	tenant := tenantOr(o.TenantID)
	if o.OwnerDomain != EvidenceDomainCredential && o.OwnerDomain != EvidenceDomainPerformance {
		return nil, fmt.Errorf("db: invalid owner_domain %q", o.OwnerDomain)
	}
	if o.OwnerID <= 0 || !validOriginalKind(o.DocumentKind) || strings.TrimSpace(o.OriginalPath) == "" ||
		strings.TrimSpace(o.OriginalSHA256) == "" || o.PageStart <= 0 || o.PageEnd < o.PageStart {
		return nil, errors.New("db: original requires owner, document_kind, path, sha256 and page range")
	}
	var out *CompanyEvidenceOriginal
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		switch o.OwnerDomain {
		case EvidenceDomainCredential:
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM company_credentials WHERE tenant_id=$1 AND id=$2)`, tenant, o.OwnerID).Scan(&exists); err != nil {
				return err
			}
		default:
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM company_performance WHERE tenant_id=$1 AND id=$2)`, tenant, o.OwnerID).Scan(&exists); err != nil {
				return err
			}
		}
		if !exists {
			return fmt.Errorf("%w: %s %d", ErrNotFound, o.OwnerDomain, o.OwnerID)
		}
		if supersede {
			if _, err := tx.ExecContext(ctx, `UPDATE company_evidence_originals SET superseded_at=now(), updated_by=$5
				WHERE tenant_id=$1 AND owner_domain=$2 AND owner_id=$3 AND document_kind=$4
				  AND page_label=$6 AND superseded_at IS NULL`, tenant, o.OwnerDomain, o.OwnerID, o.DocumentKind, o.CreatedBy, o.PageLabel); err != nil {
				return err
			}
		}
		var err error
		out, err = scanOriginal(tx.QueryRowContext(ctx, `INSERT INTO company_evidence_originals(tenant_id, owner_domain, owner_id,
			document_kind, page_label, original_path, original_sha256, page_start, page_end, created_by, updated_by, approved_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,COALESCE(NULLIF($10,''),''),COALESCE(NULLIF($10,''),''),COALESCE(NULLIF($11,''),'')) RETURNING `+originalCols,
			tenant, o.OwnerDomain, o.OwnerID, o.DocumentKind, o.PageLabel, o.OriginalPath, o.OriginalSHA256, o.PageStart, o.PageEnd,
			o.CreatedBy, o.ApprovedBy))
		return err
	})
	return out, err
}

// ListCompanyEvidenceOriginals 返回一条证据的全部原件（默认包含已替换记录，供审计）。
func (d *DB) ListCompanyEvidenceOriginals(ctx context.Context, tenant, domain string, ownerID int64, currentOnly bool) ([]*CompanyEvidenceOriginal, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+originalCols+` FROM company_evidence_originals
		WHERE tenant_id=$1 AND owner_domain=$2 AND owner_id=$3 AND (NOT $4 OR superseded_at IS NULL)
		ORDER BY document_kind, page_start, id`, tenantOr(tenant), domain, ownerID, currentOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CompanyEvidenceOriginal
	for rows.Next() {
		o, err := scanOriginal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SupersedeCompanyEvidenceOriginals 把一条证据指定类型的当前原件整体置为已替换（用于 G11 原件变更）。
func (d *DB) SupersedeCompanyEvidenceOriginals(ctx context.Context, tenant, domain string, ownerID int64, documentKind, actor string) (int, error) {
	res, err := d.ExecContext(ctx, `UPDATE company_evidence_originals SET superseded_at=now(), updated_by=COALESCE(NULLIF($5,''),updated_by)
		WHERE tenant_id=$1 AND owner_domain=$2 AND owner_id=$3 AND document_kind=$4 AND superseded_at IS NULL`,
		tenantOr(tenant), domain, ownerID, documentKind, actor)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// 要求与企业证据绑定
// ---------------------------------------------------------------------------

// RequirementEvidenceBinding 是要求与租户级企业证据的绑定；不混入 retrieval_evidence。
type RequirementEvidenceBinding struct {
	ID            int64     `json:"id"`
	TenantID      string    `json:"tenant_id"`
	TaskID        int64     `json:"task_id"`
	RequirementID int64     `json:"requirement_id"`
	Domain        string    `json:"evidence_domain"`
	EvidenceID    int64     `json:"evidence_id"`
	SourceHash    string    `json:"source_hash"`
	CreatedBy     string    `json:"created_by,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// BindRequirementEvidence 幂等绑定一条企业证据；证据必须属于同一租户且未软删除。
func (d *DB) BindRequirementEvidence(ctx context.Context, tenant string, taskID, requirementID int64, domain string, evidenceID int64, actor string) (*RequirementEvidenceBinding, error) {
	if err := CheckRequirementScope(ctx, requirementID); err != nil {
		return nil, err
	}
	tenant = tenantOr(tenant)
	if domain != EvidenceDomainCredential && domain != EvidenceDomainPerformance {
		return nil, fmt.Errorf("db: invalid evidence_domain %q", domain)
	}
	var hash string
	var deleted bool
	var err error
	switch domain {
	case EvidenceDomainCredential:
		err = d.QueryRowContext(ctx, `SELECT ''::text, deleted_at IS NOT NULL FROM company_credentials WHERE tenant_id=$1 AND id=$2`, tenant, evidenceID).Scan(&hash, &deleted)
	case EvidenceDomainPerformance:
		err = d.QueryRowContext(ctx, `SELECT ''::text, deleted_at IS NOT NULL FROM company_performance WHERE tenant_id=$1 AND id=$2`, tenant, evidenceID).Scan(&hash, &deleted)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s %d", ErrNotFound, domain, evidenceID)
	}
	if err != nil {
		return nil, err
	}
	if deleted {
		return nil, fmt.Errorf("%w: %s %d is deleted", ErrEvidenceInadmissible, domain, evidenceID)
	}
	var out *RequirementEvidenceBinding
	err = d.InTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `INSERT INTO requirement_evidence_bindings(tenant_id, task_id, requirement_id, evidence_domain,
			evidence_id, source_hash, created_by)
			VALUES ($1,$2,$3,$4,$5,'',$6) ON CONFLICT (tenant_id, task_id, requirement_id, evidence_domain, evidence_id) DO NOTHING
			RETURNING id, tenant_id, task_id, requirement_id, evidence_domain, evidence_id, source_hash, created_by, created_at`,
			tenant, taskID, requirementID, domain, evidenceID, actor)
		var b RequirementEvidenceBinding
		err := row.Scan(&b.ID, &b.TenantID, &b.TaskID, &b.RequirementID, &b.Domain, &b.EvidenceID, &b.SourceHash, &b.CreatedBy, &b.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT id, tenant_id, task_id, requirement_id, evidence_domain, evidence_id, source_hash, created_by, created_at
				FROM requirement_evidence_bindings WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3 AND evidence_domain=$4 AND evidence_id=$5`,
				tenant, taskID, requirementID, domain, evidenceID).Scan(&b.ID, &b.TenantID, &b.TaskID, &b.RequirementID, &b.Domain, &b.EvidenceID, &b.SourceHash, &b.CreatedBy, &b.CreatedAt)
			out = &b
			return err
		}
		out = &b
		return err
	})
	return out, err
}

// UnbindRequirementEvidence 删除一条绑定；找不到返回 ErrNotFound。
func (d *DB) UnbindRequirementEvidence(ctx context.Context, tenant string, taskID, requirementID int64, domain string, evidenceID int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM requirement_evidence_bindings
		WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3 AND evidence_domain=$4 AND evidence_id=$5`,
		tenantOr(tenant), taskID, requirementID, domain, evidenceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: binding %s %d", ErrNotFound, domain, evidenceID)
	}
	return nil
}

// ListRequirementEvidenceBindings 列出一个要求的全部绑定。
func (d *DB) ListRequirementEvidenceBindings(ctx context.Context, tenant string, taskID, requirementID int64) ([]*RequirementEvidenceBinding, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, tenant_id, task_id, requirement_id, evidence_domain, evidence_id, source_hash, created_by, created_at
		FROM requirement_evidence_bindings WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3 ORDER BY evidence_domain, evidence_id`,
		tenantOr(tenant), taskID, requirementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RequirementEvidenceBinding
	for rows.Next() {
		var b RequirementEvidenceBinding
		if err := rows.Scan(&b.ID, &b.TenantID, &b.TaskID, &b.RequirementID, &b.Domain, &b.EvidenceID, &b.SourceHash, &b.CreatedBy, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// TaskHasD2Activity 报告任务是否已使用 D2 能力（绑定 / 可采性 / 台账 / 报价）。
// 只有启用 D2 的任务才接受新增导出阻断，M1 基线任务保持原行为。
func (d *DB) TaskHasD2Activity(ctx context.Context, tenant string, taskID int64) (bool, error) {
	var active bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requirement_evidence_bindings WHERE tenant_id=$1 AND task_id=$2)
		OR EXISTS(SELECT 1 FROM admissibility_runs WHERE tenant_id=$1 AND task_id=$2)
		OR EXISTS(SELECT 1 FROM commitment_ledger WHERE tenant_id=$1 AND task_id=$2)
		OR EXISTS(SELECT 1 FROM bid_quotes WHERE tenant_id=$1 AND task_id=$2)`, tenantOr(tenant), taskID).Scan(&active)
	return active, err
}

// EvidenceSourceHash 计算证据源指纹（证据字段 + 全部原件版本），用于绑定与运行指纹。
func EvidenceSourceHash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CompanyLegalName 从 company_profile 证据或投标人主体推断投标人法定名称。
func (d *DB) CompanyLegalName(ctx context.Context, tenant string) (string, error) {
	tenant = tenantOr(tenant)
	rows, err := d.QueryContext(ctx, `SELECT attributes FROM company_credentials
		WHERE tenant_id=$1 AND category='company_profile' AND deleted_at IS NULL ORDER BY id`, tenant)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return "", err
		}
		var attrs map[string]any
		if json.Unmarshal(raw, &attrs) == nil {
			if s, ok := attrs["legal_name"].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	var name string
	err = d.QueryRowContext(ctx, `SELECT subject_name FROM company_credentials WHERE tenant_id=$1 AND subject_type='bidder'
		AND subject_name <> '' AND deleted_at IS NULL ORDER BY id LIMIT 1`, tenant).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return strings.TrimSpace(name), err
}

// SortedBindings 按 domain/id 排序，保证指纹与两轮比较稳定。
func SortedBindings(in []*RequirementEvidenceBinding) []*RequirementEvidenceBinding {
	out := append([]*RequirementEvidenceBinding{}, in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].EvidenceID < out[j].EvidenceID
	})
	return out
}
