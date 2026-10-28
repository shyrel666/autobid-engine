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
	err = d.InTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO tender_documents(tenant_id, task_id, tender_no, lot_no, title, raw_text, content_hash,
			                             file_name, mime_type, page_count, parse_meta, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, 0), $11::jsonb, NULLIF($12, ''))
			ON CONFLICT (tenant_id, task_id, tender_no, lot_no, content_hash) DO NOTHING
			RETURNING id`, tenant, doc.TaskID, tenderNo, lotNo, doc.Title, doc.RawText, hash,
			doc.FileName, doc.MimeType, doc.PageCount, nullableJSON(doc.ParseMeta), doc.CreatedBy).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return tx.QueryRowContext(ctx, `SELECT id FROM tender_documents
				WHERE tenant_id = $1 AND task_id = $2 AND tender_no = $3 AND lot_no = $4 AND content_hash = $5`,
				tenant, doc.TaskID, tenderNo, lotNo, hash).Scan(&id)
		}
		if err != nil {
			return err
		}
		created = true
		for _, c := range chunks {
			start := c.PageStart
			if start <= 0 {
				start = max(c.PageNo, 1)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO document_chunks(doc_id, task_id, seq, level, section_code, section_title, page_no,
				                            page_start, page_end, content, token_estimate, is_mandatory, tables_json, bbox_meta)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8, $9, $10, $11, $12::jsonb, $13::jsonb)`,
				id, doc.TaskID, c.Seq, c.Level, c.SectionCode, c.SectionTitle, start, max(c.PageEnd, start),
				c.Content, c.TokenEstimate, c.IsMandatory, jsonOr(c.Tables, "[]"), jsonOr(c.BBoxMeta, "{}")); err != nil {
				return fmt.Errorf("insert chunk %d: %w", c.Seq, err)
			}
		}
		return nil
	})
	return id, created, err
}

const docCols = `d.id, d.tenant_id, d.task_id, d.tender_no, d.lot_no, d.title, d.content_hash,
	(SELECT COUNT(*) FROM document_chunks c WHERE c.doc_id = d.id), d.created_at,
	COALESCE(d.file_name, ''), COALESCE(d.mime_type, ''), COALESCE(d.page_count, 0), d.parse_meta, COALESCE(d.created_by, '')`

func scanDocument(r rowScanner) (*TenderDocument, error) {
	var (
		doc  TenderDocument
		meta []byte
	)
	err := r.Scan(&doc.ID, &doc.TenantID, &doc.TaskID, &doc.TenderNo, &doc.LotNo, &doc.Title, &doc.ContentHash, &doc.ChunkCount, &doc.CreatedAt,
		&doc.FileName, &doc.MimeType, &doc.PageCount, &meta, &doc.CreatedBy)
	if len(meta) > 0 {
		doc.ParseMeta = meta
	}
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
	COALESCE(page_start, page_no), COALESCE(page_end, page_start, page_no), COALESCE(jsonb_array_length(tables_json), 0)`

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
			&c.PageStart, &c.PageEnd, &c.TableCount}
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
type CompanyCredential struct {
	ID         int64           `json:"id"`
	Category   string          `json:"category"`
	Code       string          `json:"code,omitempty"`
	Title      string          `json:"title"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
	ValidUntil *string         `json:"valid_until,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// 资料库推荐分类（category 只要求非空，以下为约定词表）。
const (
	CredentialQualification = "qualification" // 企业资质证书
	CredentialPerformance   = "performance"   // 类似项目业绩
	CredentialPersonnel     = "personnel"     // 项目经理/技术负责人等人员证书
	CredentialFinancial     = "financial"     // 财务/信誉
	CredentialCertificate   = "certificate"   // 体系认证、检测报告等
)

const credentialCols = `id, category, code, title, attributes, to_char(valid_until, 'YYYY-MM-DD'), created_at, updated_at`

func scanCredential(r rowScanner) (*CompanyCredential, error) {
	var (
		c     CompanyCredential
		attrs []byte
		valid sql.NullString
	)
	if err := r.Scan(&c.ID, &c.Category, &c.Code, &c.Title, &attrs, &valid, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Attributes = attrs
	if valid.Valid {
		c.ValidUntil = &valid.String
	}
	return &c, nil
}

// UpsertCompanyCredential 写入一条企业资料。code 非空时按 (category, code) 幂等更新：
// 非空标题覆盖、attributes 做 JSONB 合并、valid_until 以新值为准。
func (d *DB) UpsertCompanyCredential(ctx context.Context, c *CompanyCredential) (*CompanyCredential, bool, error) {
	category, code, title := strings.TrimSpace(c.Category), strings.TrimSpace(c.Code), strings.TrimSpace(c.Title)
	if category == "" || title == "" {
		return nil, false, errors.New("db: credential category and title are required")
	}
	attrs, err := marshalPayload(c.Attributes)
	if err != nil {
		return nil, false, err
	}
	var valid any
	if c.ValidUntil != nil && strings.TrimSpace(*c.ValidUntil) != "" {
		v := strings.TrimSpace(*c.ValidUntil)
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			return nil, false, fmt.Errorf("db: valid_until must be YYYY-MM-DD: %q", v)
		}
		valid = v
	}
	q := `INSERT INTO company_credentials(category, code, title, attributes, valid_until)
		VALUES ($1, $2, $3, $4::jsonb, $5::date)`
	if code != "" {
		q += ` ON CONFLICT (category, code) WHERE code <> '' DO UPDATE
		SET title = EXCLUDED.title,
		    attributes = company_credentials.attributes || EXCLUDED.attributes,
		    valid_until = EXCLUDED.valid_until`
	}
	row := d.QueryRowContext(ctx, q+` RETURNING `+credentialCols+`, (xmax = 0)`, category, code, title, string(attrs), valid)
	var (
		out     CompanyCredential
		raw     []byte
		vu      sql.NullString
		created bool
	)
	if err := row.Scan(&out.ID, &out.Category, &out.Code, &out.Title, &raw, &vu, &out.CreatedAt, &out.UpdatedAt, &created); err != nil {
		return nil, false, err
	}
	out.Attributes = raw
	if vu.Valid {
		out.ValidUntil = &vu.String
	}
	return &out, created, nil
}

// ErrCredentialConflict 表示修改后的 (category, code) 与另一条记录冲突。
var ErrCredentialConflict = errors.New("db: credential (category, code) already exists")

// UpdateCompanyCredential 按主键全量替换一条企业资料：category/code/title/valid_until 以新值为准，
// attributes 整体替换（不做 JSONB 合并），因此可删除旧属性键、修改分类与编号、编辑 code 为空的记录。
// 记录不存在返回 ErrNotFound；新 (category, code) 与其他记录冲突返回 ErrCredentialConflict。
func (d *DB) UpdateCompanyCredential(ctx context.Context, c *CompanyCredential) (*CompanyCredential, error) {
	category, code, title := strings.TrimSpace(c.Category), strings.TrimSpace(c.Code), strings.TrimSpace(c.Title)
	if c.ID <= 0 || category == "" || title == "" {
		return nil, errors.New("db: credential id, category and title are required")
	}
	if string(c.Attributes) == "null" {
		c.Attributes = nil
	}
	attrs, err := marshalPayload(c.Attributes)
	if err != nil {
		return nil, err
	}
	var valid any
	if c.ValidUntil != nil && strings.TrimSpace(*c.ValidUntil) != "" {
		v := strings.TrimSpace(*c.ValidUntil)
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			return nil, fmt.Errorf("db: valid_until must be YYYY-MM-DD: %q", v)
		}
		valid = v
	}
	out, err := scanCredential(d.QueryRowContext(ctx, `UPDATE company_credentials
		SET category = $2, code = $3, title = $4, attributes = $5::jsonb, valid_until = $6::date
		WHERE id = $1 RETURNING `+credentialCols, c.ID, category, code, title, string(attrs), valid))
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: credential %d", ErrNotFound, c.ID)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return nil, fmt.Errorf("%w: (%s, %s)", ErrCredentialConflict, category, code)
	}
	return out, err
}

// DeleteCompanyCredential 按主键删除一条企业资料；不存在返回 ErrNotFound。
func (d *DB) DeleteCompanyCredential(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM company_credentials WHERE id = $1`, id)
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
	// ValidOn 非空（YYYY-MM-DD）时只返回当日仍有效的记录。
	ValidOn string
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
		WHERE ($1 = '' OR category = $1)
		  AND (COALESCE(cardinality($2::text[]), 0) = 0
		       OR (code || ' ' || title || ' ' || attributes::text) ILIKE ANY($2::text[]))
		  AND ($3::date IS NULL OR valid_until IS NULL OR valid_until >= $3::date)
		ORDER BY category, id LIMIT $4`, f.Category, patterns, validOn, f.Limit)
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
