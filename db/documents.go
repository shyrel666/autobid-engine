package db

// 招标原文（tender_documents / document_chunks）与企业资料库（company_credentials）存储。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultTenant 是 Phase 7 多租户落地前所有业务数据归属的租户。
const DefaultTenant = "default"

// TenderDocument 是任务上传的一份招标文件；列表接口不回传 RawText。
// FileName / MimeType / PageCount / ParseMeta 记录原始文件来源与解析过程（纯文本上传时为空）。
type TenderDocument struct {
	ID          int64           `json:"id"`
	TenantID    string          `json:"tenant_id"`
	TaskID      int64           `json:"task_id"`
	TenderNo    string          `json:"tender_no"`
	LotNo       string          `json:"lot_no,omitempty"`
	Title       string          `json:"title"`
	FileName    string          `json:"file_name,omitempty"`
	MimeType    string          `json:"mime_type,omitempty"`
	PageCount   int             `json:"page_count,omitempty"`
	ParseMeta   json.RawMessage `json:"parse_meta,omitempty"`
	RawText     string          `json:"raw_text,omitempty"`
	ContentHash string          `json:"content_hash"`
	ChunkCount  int             `json:"chunk_count"`
	CreatedBy   string          `json:"created_by,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	// DocRole 是文件在招标包中的角色（tender / correction / clarification / qa / attachment），PublishedAt 为公告发布时间。
	// 二者决定效力先后：后发布的更正 / 澄清优先于原招标文件。
	DocRole     string     `json:"doc_role"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	// F1：原始上传文件登记（task_file_objects）；旧数据为 NULL。ParseRevision 采用重新解析修订时递增（乐观锁）。
	OriginalFileID *int64 `json:"original_file_id,omitempty"`
	ParseRevision  int    `json:"parse_revision"`
}

// 招标包文件角色（tender_documents.doc_role）。
const (
	DocRoleTender        = "tender"
	DocRoleCorrection    = "correction"
	DocRoleClarification = "clarification"
	DocRoleQA            = "qa"
	DocRoleAttachment    = "attachment"
)

// ValidDocRole 报告文件角色是否合法。
func ValidDocRole(r string) bool {
	switch r {
	case DocRoleTender, DocRoleCorrection, DocRoleClarification, DocRoleQA, DocRoleAttachment:
		return true
	}
	return false
}

// GuessDocRole 按标题 / 文件名推断未指定的文件角色：更正（变更）、澄清、答疑，其余视为招标文件。
func GuessDocRole(names ...string) string {
	s := strings.Join(names, " ")
	switch {
	case strings.Contains(s, "更正") || strings.Contains(s, "变更公告") || strings.Contains(s, "补遗"):
		return DocRoleCorrection
	case strings.Contains(s, "澄清"):
		return DocRoleClarification
	case strings.Contains(s, "答疑"):
		return DocRoleQA
	}
	return DocRoleTender
}

// DocumentChunk 是按章节/条款切分的段落；大纲查询不回传 Content / Tables / BBoxMeta。
// PageNo 为起始页（兼容旧接口），PageStart~PageEnd 为段落跨越的页码范围。
type DocumentChunk struct {
	ID            int64           `json:"id"`
	DocID         int64           `json:"doc_id"`
	TaskID        int64           `json:"task_id"`
	Seq           int             `json:"seq"`
	Level         int             `json:"level"`
	SectionCode   string          `json:"section_code"`
	SectionTitle  string          `json:"section_title"`
	PageNo        int             `json:"page_no"`
	PageStart     int             `json:"page_start"`
	PageEnd       int             `json:"page_end"`
	Content       string          `json:"content,omitempty"`
	TokenEstimate int             `json:"token_estimate"`
	IsMandatory   bool            `json:"is_mandatory"`
	TableCount    int             `json:"table_count"`
	Tables        json.RawMessage `json:"tables,omitempty"`
	BBoxMeta      json.RawMessage `json:"bbox_meta,omitempty"`
	// PageIsPhysical 为 false 表示表格类文档的行窗口，page_no 只是兼容占位值，不得当页码展示或核验。
	PageIsPhysical bool `json:"page_is_physical"`
}

// jsonOr 返回 raw，为空时返回 def（写入 JSONB 列）。
func jsonOr(raw json.RawMessage, def string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return def
	}
	return string(raw)
}

// ContentHash 是招标原文的幂等键。
func ContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// InsertTenderDocument 单事务写入文档与其全部段落。同一任务、同一项目/标段下原文相同的重复上传
// 不再写入，返回已有文档 id 与 created=false。chunks 的 Seq 由调用方按文档顺序给出。
func (d *DB) InsertTenderDocument(ctx context.Context, doc *TenderDocument, chunks []DocumentChunk) (id int64, created bool, err error) {
	tenderNo, lotNo := strings.TrimSpace(doc.TenderNo), strings.TrimSpace(doc.LotNo)
	if doc.TaskID <= 0 || tenderNo == "" {
		return 0, false, errors.New("db: tender document requires task_id and tender_no")
	}
	hash := doc.ContentHash
	if hash == "" {
		hash = ContentHash(doc.RawText)
	}
	tenant := strings.TrimSpace(doc.TenantID)
	if tenant == "" {
		tenant = DefaultTenant
	}
	role := strings.TrimSpace(doc.DocRole)
	if role == "" {
		role = GuessDocRole(doc.Title, doc.FileName)
	}
	if !ValidDocRole(role) {
		return 0, false, fmt.Errorf("db: invalid doc_role %q", role)
	}
	err = d.InTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO tender_documents(tenant_id, task_id, tender_no, lot_no, title, raw_text, content_hash,
			                             file_name, mime_type, page_count, parse_meta, created_by, doc_role, published_at, original_file_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, 0), $11::jsonb, NULLIF($12, ''), $13, $14, $15)
			ON CONFLICT (tenant_id, task_id, tender_no, lot_no, content_hash) DO NOTHING
			RETURNING id`, tenant, doc.TaskID, tenderNo, lotNo, doc.Title, doc.RawText, hash,
			doc.FileName, doc.MimeType, doc.PageCount, nullableJSON(doc.ParseMeta), doc.CreatedBy, role, doc.PublishedAt, doc.OriginalFileID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.QueryRowContext(ctx, `SELECT id FROM tender_documents
				WHERE tenant_id = $1 AND task_id = $2 AND tender_no = $3 AND lot_no = $4 AND content_hash = $5`,
				tenant, doc.TaskID, tenderNo, lotNo, hash).Scan(&id); err != nil {
				return err
			}
			// 同一原文重复上传：只补关联原件。original_file_id / parse_revision 不在检索失效触发列内。
			if doc.OriginalFileID != nil {
				if _, err := tx.ExecContext(ctx, `UPDATE tender_documents SET original_file_id = COALESCE(original_file_id, $4)
					WHERE id = $1 AND tenant_id = $2 AND task_id = $3 AND content_hash = $5`,
					id, tenant, doc.TaskID, doc.OriginalFileID, hash); err != nil {
					return fmt.Errorf("link original file: %w", err)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		created = true
		for _, c := range chunks {
			pageNo := max(c.PageNo, 1)
			var start, end any
			if c.PageStart > 0 || c.PageNo > 0 {
				s := max(c.PageStart, c.PageNo, 1)
				start, end = s, max(c.PageEnd, s)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO document_chunks(doc_id, task_id, seq, level, section_code, section_title, page_no,
				                            page_start, page_end, content, token_estimate, is_mandatory, tables_json, bbox_meta)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14::jsonb)`,
				id, doc.TaskID, c.Seq, c.Level, c.SectionCode, c.SectionTitle, pageNo, start, end,
				c.Content, c.TokenEstimate, c.IsMandatory, jsonOr(c.Tables, "[]"), jsonOr(c.BBoxMeta, "{}")); err != nil {
				return fmt.Errorf("insert chunk %d: %w", c.Seq, err)
			}
		}
		actor := strings.TrimSpace(doc.CreatedBy)
		if actor == "" {
			actor = "system"
		}
		// 新文档进入清单后，旧确认不再覆盖当前原文：推进 source_epoch 并复用清单失效。
		if _, err := tx.ExecContext(ctx, `SELECT requirement_source_bump($1, $2, $3)`, tenant, doc.TaskID, actor); err != nil {
			return fmt.Errorf("bump requirement source epoch: %w", err)
		}
		return nil
	})
	return id, created, err
}

const docCols = `d.id, d.tenant_id, d.task_id, d.tender_no, d.lot_no, d.title, d.content_hash,
	(SELECT COUNT(*) FROM document_chunks c WHERE c.doc_id = d.id), d.created_at,
	COALESCE(d.file_name, ''), COALESCE(d.mime_type, ''), COALESCE(d.page_count, 0), d.parse_meta, COALESCE(d.created_by, ''),
	d.doc_role, d.published_at, d.original_file_id, d.parse_revision`

func scanDocument(r rowScanner) (*TenderDocument, error) {
	var (
		doc       TenderDocument
		meta      []byte
		published sql.NullTime
	)
	err := r.Scan(&doc.ID, &doc.TenantID, &doc.TaskID, &doc.TenderNo, &doc.LotNo, &doc.Title, &doc.ContentHash, &doc.ChunkCount, &doc.CreatedAt,
		&doc.FileName, &doc.MimeType, &doc.PageCount, &meta, &doc.CreatedBy, &doc.DocRole, &published,
		&doc.OriginalFileID, &doc.ParseRevision)
	if len(meta) > 0 {
		doc.ParseMeta = meta
	}
	doc.PublishedAt = nullTimePtr(published)
	return &doc, err
}

// nullableJSON 把空 JSON 转为 SQL NULL。
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}

// ListTenderDocuments 按上传顺序列出任务的招标文档（不含原文）。
func (d *DB) ListTenderDocuments(ctx context.Context, taskID int64) ([]*TenderDocument, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+docCols+` FROM tender_documents d WHERE d.task_id = $1 ORDER BY d.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TenderDocument
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// GetTenderDocument 读取任务下的一份文档（不含原文）。
func (d *DB) GetTenderDocument(ctx context.Context, taskID, docID int64) (*TenderDocument, error) {
	doc, err := scanDocument(d.QueryRowContext(ctx, `SELECT `+docCols+` FROM tender_documents d
		WHERE d.id = $1 AND d.task_id = $2`, docID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: document %d", ErrNotFound, docID)
	}
	return doc, err
}

const chunkHeadCols = `id, doc_id, task_id, seq, level, section_code, section_title, page_no, token_estimate, is_mandatory,
	COALESCE(page_start, page_no), COALESCE(page_end, page_start, page_no), COALESCE(jsonb_array_length(tables_json), 0),
	COALESCE(bbox_meta->>'page_is_physical', 'true') <> 'false'`

// chunkBodyCols 跟在 chunkHeadCols 之后，含正文、表格与版面溯源。
const chunkBodyCols = `content, COALESCE(tables_json, '[]'::jsonb), COALESCE(bbox_meta, '{}'::jsonb)`

func scanChunks(rows *sql.Rows, withContent bool) ([]*DocumentChunk, error) {
	defer rows.Close()
	var out []*DocumentChunk
	for rows.Next() {
		var (
			c             DocumentChunk
			tables, boxes []byte
		)
		dest := []any{&c.ID, &c.DocID, &c.TaskID, &c.Seq, &c.Level, &c.SectionCode, &c.SectionTitle, &c.PageNo, &c.TokenEstimate, &c.IsMandatory,
			&c.PageStart, &c.PageEnd, &c.TableCount, &c.PageIsPhysical}
		if withContent {
			dest = append(dest, &c.Content, &tables, &boxes)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if withContent {
			c.Tables, c.BBoxMeta = tables, boxes
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// DocumentOutline 返回任务下段落的目录（不含正文），按文档与顺序排列；docID=0 表示全部文档。
func (d *DB) DocumentOutline(ctx context.Context, taskID, docID int64, limit int) ([]*DocumentChunk, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+chunkHeadCols+` FROM document_chunks
		WHERE task_id = $1 AND ($2::bigint = 0 OR doc_id = $2::bigint)
		ORDER BY doc_id, seq LIMIT $3`, taskID, docID, limit)
	if err != nil {
		return nil, err
	}
	return scanChunks(rows, false)
}

// TaskChunks 返回任务下段落（含正文），供检索；docID=0 表示全部文档。
func (d *DB) TaskChunks(ctx context.Context, taskID, docID int64, limit int) ([]*DocumentChunk, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+chunkHeadCols+`, `+chunkBodyCols+` FROM document_chunks
		WHERE task_id = $1 AND ($2::bigint = 0 OR doc_id = $2::bigint)
		ORDER BY doc_id, seq LIMIT $3`, taskID, docID, limit)
	if err != nil {
		return nil, err
	}
	return scanChunks(rows, true)
}

// ChunkWindow 读取任务下的一个段落及同一文档内前 before、后 after 个相邻段落（按 seq 升序，含目标本身）。
func (d *DB) ChunkWindow(ctx context.Context, taskID, chunkID int64, before, after int) (target *DocumentChunk, window []*DocumentChunk, err error) {
	rows, err := d.QueryContext(ctx, `
		WITH t AS (SELECT doc_id, seq FROM document_chunks WHERE id = $1 AND task_id = $2)
		SELECT `+chunkHeadCols+`, `+chunkBodyCols+` FROM document_chunks c
		WHERE c.doc_id = (SELECT doc_id FROM t)
		  AND c.seq BETWEEN (SELECT seq FROM t) - $3 AND (SELECT seq FROM t) + $4
		ORDER BY c.seq`, chunkID, taskID, max(before, 0), max(after, 0))
	if err != nil {
		return nil, nil, err
	}
	window, err = scanChunks(rows, true)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range window {
		if c.ID == chunkID {
			target = c
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("%w: chunk %d", ErrNotFound, chunkID)
	}
	return target, window, nil
}

// ---------------------------------------------------------------------------
// 企业资料库
// ---------------------------------------------------------------------------

// CompanyCredential 是本企业的一条资质/业绩/人员记录；ValidUntil 为 YYYY-MM-DD，nil 表示长期有效。
// D2 增加租户、主体、证书编号 / 签发机构 / 签发日 / 认证范围与软删除字段；旧数据 subject_type=unknown。
type CompanyCredential struct {
	ID            int64           `json:"id"`
	TenantID      string          `json:"tenant_id"`
	Category      string          `json:"category"`
	Code          string          `json:"code,omitempty"`
	Title         string          `json:"title"`
	SubjectName   string          `json:"subject_name,omitempty"`
	SubjectType   string          `json:"subject_type,omitempty"`
	CertificateNo string          `json:"certificate_no,omitempty"`
	Issuer        string          `json:"issuer,omitempty"`
	IssuedOn      *string         `json:"issued_on,omitempty"`
	Scope         string          `json:"scope,omitempty"`
	Attributes    json.RawMessage `json:"attributes,omitempty"`
	ValidUntil    *string         `json:"valid_until,omitempty"`
	DeletedAt     *time.Time      `json:"deleted_at,omitempty"`
	CreatedBy     string          `json:"created_by,omitempty"`
	UpdatedBy     string          `json:"updated_by,omitempty"`
	ApprovedBy    string          `json:"approved_by,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// 资料库推荐分类（category 只要求非空，以下为约定词表）。
const (
	CredentialQualification = "qualification" // 企业资质证书
	CredentialPerformance   = "performance"   // 类似项目业绩
	CredentialPersonnel     = "personnel"     // 项目经理/技术负责人等人员证书
	CredentialFinancial     = "financial"     // 财务/信誉
	CredentialCertificate   = "certificate"   // 体系认证、检测报告等
)

const credentialCols = `id, tenant_id, category, code, title, subject_name, subject_type, certificate_no, issuer,
	to_char(issued_on, 'YYYY-MM-DD'), scope, attributes, to_char(valid_until, 'YYYY-MM-DD'), deleted_at,
	created_by, updated_by, approved_by, created_at, updated_at`

func scanCredential(r rowScanner) (*CompanyCredential, error) {
	var (
		c      CompanyCredential
		attrs  []byte
		valid  sql.NullString
		issued sql.NullString
		del    sql.NullTime
	)
	if err := r.Scan(&c.ID, &c.TenantID, &c.Category, &c.Code, &c.Title, &c.SubjectName, &c.SubjectType, &c.CertificateNo, &c.Issuer,
		&issued, &c.Scope, &attrs, &valid, &del, &c.CreatedBy, &c.UpdatedBy, &c.ApprovedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Attributes = attrs
	if valid.Valid {
		c.ValidUntil = &valid.String
	}
	if issued.Valid {
		c.IssuedOn = &issued.String
	}
	c.DeletedAt = nullTimePtr(del)
	return &c, nil
}

func validSubjectType(t string) bool {
	switch t {
	case "", "unknown", "bidder", "parent", "subsidiary", "person", "manufacturer", "other":
		return true
	}
	return false
}

// UpsertCompanyCredential 写入一条企业资料。code 非空时按 (tenant_id, category, code) 幂等更新：
// 非空标题覆盖、attributes 做 JSONB 合并、valid_until 以新值为准；主体 / 证书字段为空时保留旧值。
func (d *DB) UpsertCompanyCredential(ctx context.Context, c *CompanyCredential) (*CompanyCredential, bool, error) {
	category, code, title := strings.TrimSpace(c.Category), strings.TrimSpace(c.Code), strings.TrimSpace(c.Title)
	if category == "" || title == "" {
		return nil, false, errors.New("db: credential category and title are required")
	}
	tenant := strings.TrimSpace(c.TenantID)
	if tenant == "" {
		tenant = DefaultTenant
	}
	subjectType := strings.TrimSpace(c.SubjectType)
	if subjectType == "" {
		subjectType = "unknown"
	}
	if !validSubjectType(subjectType) {
		return nil, false, fmt.Errorf("db: invalid subject_type %q", subjectType)
	}
	attrs, err := marshalPayload(c.Attributes)
	if err != nil {
		return nil, false, err
	}
	valid, issued, err := credentialDates(c.ValidUntil, c.IssuedOn)
	if err != nil {
		return nil, false, err
	}
	q := `INSERT INTO company_credentials(tenant_id, category, code, title, subject_name, subject_type, certificate_no, issuer,
			issued_on, scope, attributes, valid_until, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::date, $10, $11::jsonb, $12::date, $13, $13)`
	args := []any{tenant, category, code, title, strings.TrimSpace(c.SubjectName), subjectType, strings.TrimSpace(c.CertificateNo),
		strings.TrimSpace(c.Issuer), issued, strings.TrimSpace(c.Scope), string(attrs), valid, c.CreatedBy}
	if code != "" {
		q += ` ON CONFLICT (tenant_id, category, code) WHERE code <> '' DO UPDATE
		SET title = EXCLUDED.title,
		    attributes = company_credentials.attributes || EXCLUDED.attributes,
		    valid_until = EXCLUDED.valid_until,
		    subject_name = COALESCE(NULLIF(EXCLUDED.subject_name, ''), company_credentials.subject_name),
		    subject_type = CASE WHEN EXCLUDED.subject_type <> 'unknown' THEN EXCLUDED.subject_type ELSE company_credentials.subject_type END,
		    certificate_no = COALESCE(NULLIF(EXCLUDED.certificate_no, ''), company_credentials.certificate_no),
		    issuer = COALESCE(NULLIF(EXCLUDED.issuer, ''), company_credentials.issuer),
		    issued_on = COALESCE(EXCLUDED.issued_on, company_credentials.issued_on),
		    scope = COALESCE(NULLIF(EXCLUDED.scope, ''), company_credentials.scope),
		    deleted_at = NULL,
		    updated_by = EXCLUDED.updated_by`
	}
	q += ` RETURNING ` + credentialCols + `, (xmax = 0)`
	row := d.QueryRowContext(ctx, q, args...)
	var (
		out     CompanyCredential
		raw     []byte
		vu      sql.NullString
		is      sql.NullString
		del     sql.NullTime
		created bool
	)
	if err := row.Scan(&out.ID, &out.TenantID, &out.Category, &out.Code, &out.Title, &out.SubjectName, &out.SubjectType, &out.CertificateNo,
		&out.Issuer, &is, &out.Scope, &raw, &vu, &del, &out.CreatedBy, &out.UpdatedBy, &out.ApprovedBy, &out.CreatedAt, &out.UpdatedAt, &created); err != nil {
		return nil, false, err
	}
	out.Attributes = raw
	if vu.Valid {
		out.ValidUntil = &vu.String
	}
	if is.Valid {
		out.IssuedOn = &is.String
	}
	out.DeletedAt = nullTimePtr(del)
	return &out, created, nil
}

func credentialDates(valid, issued *string) (validOut, issuedOut any, err error) {
	if valid != nil && strings.TrimSpace(*valid) != "" {
		v := strings.TrimSpace(*valid)
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			return nil, nil, fmt.Errorf("db: valid_until must be YYYY-MM-DD: %q", v)
		}
		validOut = v
	}
	if issued != nil && strings.TrimSpace(*issued) != "" {
		v := strings.TrimSpace(*issued)
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			return nil, nil, fmt.Errorf("db: issued_on must be YYYY-MM-DD: %q", v)
		}
		issuedOut = v
	}
	return validOut, issuedOut, nil
}

// ErrCredentialConflict 表示修改后的 (category, code) 与另一条记录冲突。
var ErrCredentialConflict = errors.New("db: credential (category, code) already exists")

// UpdateCompanyCredential 按主键全量替换一条企业资料：category/code/title/valid_until 以新值为准，
// attributes 整体替换（不做 JSONB 合并），因此可删除旧属性键、修改分类与编号、编辑 code 为空的记录。
// 记录不存在返回 ErrNotFound；新 (tenant_id, category, code) 与其他记录冲突返回 ErrCredentialConflict。
func (d *DB) UpdateCompanyCredential(ctx context.Context, c *CompanyCredential) (*CompanyCredential, error) {
	tenant := strings.TrimSpace(c.TenantID)
	if tenant == "" {
		tenant = DefaultTenant
	}
	category, code, title := strings.TrimSpace(c.Category), strings.TrimSpace(c.Code), strings.TrimSpace(c.Title)
	if c.ID <= 0 || category == "" || title == "" {
		return nil, errors.New("db: credential id, category and title are required")
	}
	subjectType := strings.TrimSpace(c.SubjectType)
	if subjectType == "" {
		subjectType = "unknown"
	}
	if !validSubjectType(subjectType) {
		return nil, fmt.Errorf("db: invalid subject_type %q", subjectType)
	}
	if string(c.Attributes) == "null" {
		c.Attributes = nil
	}
	attrs, err := marshalPayload(c.Attributes)
	if err != nil {
		return nil, err
	}
	valid, issued, err := credentialDates(c.ValidUntil, c.IssuedOn)
	if err != nil {
		return nil, err
	}
	out, err := scanCredential(d.QueryRowContext(ctx, `UPDATE company_credentials
		SET category = $3, code = $4, title = $5, subject_name = $6, subject_type = $7, certificate_no = $8, issuer = $9,
		    issued_on = $10::date, scope = $11, attributes = $12::jsonb, valid_until = $13::date, updated_by = COALESCE(NULLIF($14, ''), updated_by)
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL RETURNING `+credentialCols,
		c.ID, tenant, category, code, title, strings.TrimSpace(c.SubjectName), subjectType, strings.TrimSpace(c.CertificateNo),
		strings.TrimSpace(c.Issuer), issued, strings.TrimSpace(c.Scope), string(attrs), valid, c.UpdatedBy))
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: credential %d", ErrNotFound, c.ID)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return nil, fmt.Errorf("%w: (%s, %s)", ErrCredentialConflict, category, code)
	}
	return out, err
}

// DeleteCompanyCredential 软删除一条企业资料（deleted_at），保留原件与审计；已绑定证据不得物理删除。
func (d *DB) DeleteCompanyCredential(ctx context.Context, id int64) error {
	return d.DeleteCompanyCredentialScoped(ctx, DefaultTenant, id, "")
}

// DeleteCompanyCredentialScoped 在指定租户内软删除企业资料。
func (d *DB) DeleteCompanyCredentialScoped(ctx context.Context, tenant string, id int64, actor string) error {
	res, err := d.ExecContext(ctx, `UPDATE company_credentials SET deleted_at = now(), updated_by = COALESCE(NULLIF($3, ''), updated_by)
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, id, tenantOr(tenant), actor)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: credential %d", ErrNotFound, id)
	}
	return nil
}

// CredentialFilter 过滤企业资料：Terms 任一命中 code/title/attributes 文本（不区分大小写）即入选。
type CredentialFilter struct {
	Category string
	Terms    []string
	// ValidOn 非空（YYYY-MM-DD）时只返回当日仍有效的记录；为空但指定了 TaskID 时自动以任务投标截止日为基准。
	ValidOn string
	TaskID  int64
	Tenant  string
	Limit   int
}

func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

func (d *DB) ListCompanyCredentials(ctx context.Context, f CredentialFilter) ([]*CompanyCredential, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	if f.ValidOn == "" && f.TaskID > 0 {
		var dl sql.NullTime
		if err := d.QueryRowContext(ctx, `SELECT bid_deadline FROM requirement_manifests WHERE tenant_id = $1 AND task_id = $2`,
			tenantOr(f.Tenant), f.TaskID).Scan(&dl); err == nil && dl.Valid {
			f.ValidOn = dl.Time.In(beijing).Format(time.DateOnly)
		}
	}
	var patterns []string
	for _, t := range f.Terms {
		if t = strings.TrimSpace(t); t != "" {
			patterns = append(patterns, likePattern(t))
		}
	}
	var validOn any
	if f.ValidOn != "" {
		if _, err := time.Parse(time.DateOnly, f.ValidOn); err != nil {
			return nil, fmt.Errorf("db: valid_on must be YYYY-MM-DD: %q", f.ValidOn)
		}
		validOn = f.ValidOn
	}
	rows, err := d.QueryContext(ctx, `SELECT `+credentialCols+` FROM company_credentials
		WHERE tenant_id = $1 AND deleted_at IS NULL
		  AND ($2 = '' OR category = $2)
		  AND (COALESCE(cardinality($3::text[]), 0) = 0
		       OR (code || ' ' || title || ' ' || attributes::text) ILIKE ANY($3::text[]))
		  AND ($4::date IS NULL OR valid_until IS NULL OR valid_until >= $4::date)
		ORDER BY category, id LIMIT $5`, tenantOr(f.Tenant), f.Category, patterns, validOn, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CompanyCredential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 推演证据查询（评分模拟与报告）
// ---------------------------------------------------------------------------

// ConfirmedFactsByAsset 返回本推演空间内锚定到给定实体的 confirmed Fact，按实体分组、id 升序。
func (s *ExplorationStore) ConfirmedFactsByAsset(ctx context.Context, assetIDs []int64) (map[int64][]*Node, error) {
	out := map[int64][]*Node{}
	if len(assetIDs) == 0 {
		return out, nil
	}
	// exploration_anchors 只有 node_id / asset_id 两列，与 nodeCols 不重名，可直接连接。
	rows, err := s.db.QueryContext(ctx, `SELECT asset_id, `+nodeCols+`
		FROM exploration_anchors JOIN exploration_nodes ON id = node_id
		WHERE exploration_id = $1 AND kind = 'fact' AND state = 'confirmed' AND asset_id = ANY($2::bigint[])
		ORDER BY asset_id, id`, s.expID, assetIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var assetID int64
		n, err := scanNode(prefixScanner{rows, &assetID})
		if err != nil {
			return nil, err
		}
		out[assetID] = append(out[assetID], n)
	}
	return out, rows.Err()
}

// prefixScanner 在行首多扫一个前置列，再交给 scanNode。
type prefixScanner struct {
	rows  *sql.Rows
	first any
}

func (p prefixScanner) Scan(dest ...any) error {
	return p.rows.Scan(append([]any{p.first}, dest...)...)
}
