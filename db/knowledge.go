package db

// 企业方案与产品知识库（域 D：knowledge_documents / knowledge_chunks）。
// 所有读写都按 tenant_id 隔离；混合检索 = tsvector GIN 全文召回 + 可选向量召回 + 关键词命中，加权融合排序，
// 每条命中都带来源引用（文档标题、类别、章节号/路径、页码范围）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// 知识库文档类别。
const (
	KnowledgeHistoricalBid    = "historical_bid"    // 历史标书
	KnowledgeWhitepaper       = "whitepaper"        // 技术白皮书
	KnowledgeProductSpec      = "product_spec"      // 产品规格书
	KnowledgeStandardSolution = "standard_solution" // 标准技术方案
)

// KnowledgeCategories 是全部合法类别。
var KnowledgeCategories = []string{KnowledgeHistoricalBid, KnowledgeWhitepaper, KnowledgeProductSpec, KnowledgeStandardSolution}

func ValidKnowledgeCategory(c string) bool {
	for _, k := range KnowledgeCategories {
		if c == k {
			return true
		}
	}
	return false
}

// ErrInvalidKnowledge 表示知识库请求参数非法（映射为 HTTP 400）。
var ErrInvalidKnowledge = errors.New("db: invalid knowledge request")

type KnowledgeDocument struct {
	ID          int64           `json:"id"`
	TenantID    string          `json:"tenant_id"`
	Category    string          `json:"category"`
	Title       string          `json:"title"`
	Tags        []string        `json:"tags"`
	FileName    string          `json:"file_name,omitempty"`
	MimeType    string          `json:"mime_type,omitempty"`
	PageCount   int             `json:"page_count"`
	ContentHash string          `json:"content_hash"`
	RawText     string          `json:"raw_text,omitempty"`
	ParseMeta   json.RawMessage `json:"parse_meta,omitempty"`
	Extra       json.RawMessage `json:"extra,omitempty"`
	ChunkCount  int             `json:"chunk_count"`
	TableCount  int             `json:"table_count"`
	CreatedBy   string          `json:"created_by,omitempty"`
	UpdatedBy   string          `json:"updated_by,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type KnowledgeChunk struct {
	ID            int64           `json:"id"`
	DocID         int64           `json:"doc_id"`
	Seq           int             `json:"seq"`
	Level         int             `json:"level"`
	SectionCode   string          `json:"section_code"`
	SectionTitle  string          `json:"section_title"`
	SectionPath   string          `json:"section_path"`
	PageStart     int             `json:"page_start"`
	PageEnd       int             `json:"page_end"`
	Content       string          `json:"content,omitempty"`
	TokenEstimate int             `json:"token_estimate"`
	IsMandatory   bool            `json:"is_mandatory"`
	TableCount    int             `json:"table_count"`
	Tables        json.RawMessage `json:"tables,omitempty"`
	BBoxMeta      json.RawMessage `json:"bbox_meta,omitempty"`
}

// KnowledgeCitation 是检索命中与分块读取强制附带的来源引用。
type KnowledgeCitation struct {
	DocID        int64  `json:"doc_id"`
	DocTitle     string `json:"doc_title"`
	Category     string `json:"category"`
	FileName     string `json:"file_name,omitempty"`
	ChunkID      int64  `json:"chunk_id"`
	Seq          int    `json:"seq"`
	SectionCode  string `json:"section_code"`
	SectionTitle string `json:"section_title"`
	SectionPath  string `json:"section_path"`
	PageStart    int    `json:"page_start"`
	PageEnd      int    `json:"page_end"`
}

// Cite 生成分块的来源引用。
func (c *KnowledgeChunk) Cite(doc *KnowledgeDocument) KnowledgeCitation {
	return KnowledgeCitation{DocID: doc.ID, DocTitle: doc.Title, Category: doc.Category, FileName: doc.FileName, ChunkID: c.ID, Seq: c.Seq,
		SectionCode: c.SectionCode, SectionTitle: c.SectionTitle, SectionPath: c.SectionPath, PageStart: c.PageStart, PageEnd: c.PageEnd}
}

func tenantOr(t string) string {
	if t = strings.TrimSpace(t); t == "" {
		return DefaultTenant
	}
	return t
}

// NormalizeTags 去空白、去重、保持顺序。
func NormalizeTags(tags []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range tags {
		for _, p := range strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == '，' || r == '、' || r == ';' || r == '；' }) {
			p = strings.TrimSpace(p)
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// InsertKnowledgeDocument 单事务写入知识库文档与分块；同租户 content_hash 相同的重复上传不再写入，
// 返回已有文档 id 与 created=false。emb 非空时为每个分块计算向量。
func (d *DB) InsertKnowledgeDocument(ctx context.Context, doc *KnowledgeDocument, chunks []KnowledgeChunk, emb Embedder) (int64, bool, error) {
	tenant := tenantOr(doc.TenantID)
	doc.Title = strings.TrimSpace(doc.Title)
	if !ValidKnowledgeCategory(doc.Category) {
		return 0, false, fmt.Errorf("%w: category must be one of %s", ErrInvalidKnowledge, strings.Join(KnowledgeCategories, ", "))
	}
	if doc.Title == "" {
		return 0, false, fmt.Errorf("%w: title is required", ErrInvalidKnowledge)
	}
	hash := doc.ContentHash
	if hash == "" {
		hash = ContentHash(doc.RawText)
	}
	tags := NormalizeTags(doc.Tags)
	var vectors [][]float64
	model := ""
	if emb != nil && len(chunks) > 0 {
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = doc.Title + " " + c.SectionPath + " " + c.Content
		}
		var err error
		if vectors, err = emb.Embed(ctx, texts); err != nil {
			return 0, false, fmt.Errorf("db: embed knowledge chunks: %w", err)
		}
		model = emb.Model()
	}
	var (
		id      int64
		created bool
	)
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO knowledge_documents(tenant_id, category, title, tags, file_name, mime_type, page_count, content_hash,
			                                raw_text, parse_meta, extra, created_by, updated_by)
			VALUES ($1, $2, $3, $4::text[], $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12, $12)
			ON CONFLICT (tenant_id, content_hash) DO NOTHING
			RETURNING id`, tenant, doc.Category, doc.Title, tags, doc.FileName, doc.MimeType, max(doc.PageCount, 0), hash,
			doc.RawText, jsonOr(doc.ParseMeta, "{}"), jsonOr(doc.Extra, "{}"), doc.CreatedBy).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return tx.QueryRowContext(ctx, `SELECT id FROM knowledge_documents WHERE tenant_id = $1 AND content_hash = $2`, tenant, hash).Scan(&id)
		}
		if err != nil {
			return err
		}
		created = true
		searchTags := strings.Join(tags, " ")
		for i, c := range chunks {
			var vec any
			if vectors != nil && i < len(vectors) {
				vec = vectors[i]
			}
			start := max(c.PageStart, 1)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO knowledge_chunks(tenant_id, doc_id, seq, level, section_code, section_title, section_path, page_start, page_end,
				                             content, tables_json, bbox_meta, token_estimate, is_mandatory, search_title, search_body,
				                             embedding, embedding_model, created_by, updated_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12::jsonb, $13, $14, $15, $16, $17::float8[], $18, $19, $19)`,
				tenant, id, c.Seq, c.Level, c.SectionCode, c.SectionTitle, c.SectionPath, start, max(c.PageEnd, start),
				c.Content, jsonOr(c.Tables, "[]"), jsonOr(c.BBoxMeta, "{}"), c.TokenEstimate, c.IsMandatory,
				KnowledgeSearchText(doc.Title, c.SectionPath, c.SectionTitle, searchTags), KnowledgeSearchText(c.Content, string(doc.Extra)),
				vec, modelIf(vec, model), doc.CreatedBy); err != nil {
				return fmt.Errorf("insert knowledge chunk %d: %w", c.Seq, err)
			}
		}
		return nil
	})
	return id, created, err
}

func modelIf(vec any, model string) string {
	if vec == nil {
		return ""
	}
	return model
}

const kdocCols = `d.id, d.tenant_id, d.category, d.title, to_jsonb(d.tags), d.file_name, d.mime_type, d.page_count, d.content_hash,
	d.parse_meta, d.extra, d.created_by, d.updated_by, d.created_at, d.updated_at,
	(SELECT COUNT(*) FROM knowledge_chunks c WHERE c.doc_id = d.id),
	(SELECT COALESCE(SUM(jsonb_array_length(c.tables_json)), 0) FROM knowledge_chunks c WHERE c.doc_id = d.id)`

func scanKnowledgeDocument(r rowScanner) (*KnowledgeDocument, error) {
	var (
		doc               KnowledgeDocument
		tags, meta, extra []byte
	)
	if err := r.Scan(&doc.ID, &doc.TenantID, &doc.Category, &doc.Title, &tags, &doc.FileName, &doc.MimeType, &doc.PageCount, &doc.ContentHash,
		&meta, &extra, &doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt, &doc.ChunkCount, &doc.TableCount); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(tags, &doc.Tags)
	if doc.Tags == nil {
		doc.Tags = []string{}
	}
	doc.ParseMeta, doc.Extra = meta, extra
	return &doc, nil
}

// KnowledgeDocFilter 过滤知识库文档：Tags 任一命中即入选，Query 匹配标题/文件名（不区分大小写）。
type KnowledgeDocFilter struct {
	TenantID string
	Category string
	Tags     []string
	Query    string
	Limit    int
	Offset   int
}

func (d *DB) ListKnowledgeDocuments(ctx context.Context, f KnowledgeDocFilter) ([]*KnowledgeDocument, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	pattern := ""
	if q := strings.TrimSpace(f.Query); q != "" {
		pattern = likePattern(q)
	}
	rows, err := d.QueryContext(ctx, `SELECT `+kdocCols+` FROM knowledge_documents d
		WHERE d.tenant_id = $1 AND ($2 = '' OR d.category = $2)
		  AND (COALESCE(cardinality($3::text[]), 0) = 0 OR d.tags && $3::text[])
		  AND ($4 = '' OR d.title ILIKE $4 OR d.file_name ILIKE $4)
		ORDER BY d.id DESC LIMIT $5 OFFSET $6`, tenantOr(f.TenantID), f.Category, NormalizeTags(f.Tags), pattern, f.Limit, max(f.Offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*KnowledgeDocument{}
	for rows.Next() {
		doc, err := scanKnowledgeDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// GetKnowledgeDocument 读取租户内的一份文档；withText 为 true 时附带原文。
func (d *DB) GetKnowledgeDocument(ctx context.Context, tenant string, id int64, withText bool) (*KnowledgeDocument, error) {
	doc, err := scanKnowledgeDocument(d.QueryRowContext(ctx, `SELECT `+kdocCols+` FROM knowledge_documents d
		WHERE d.id = $1 AND d.tenant_id = $2`, id, tenantOr(tenant)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: knowledge document %d", ErrNotFound, id)
	}
	if err != nil || !withText {
		return doc, err
	}
	err = d.QueryRowContext(ctx, `SELECT raw_text FROM knowledge_documents WHERE id = $1`, id).Scan(&doc.RawText)
	return doc, err
}

// DeleteKnowledgeDocument 删除租户内的一份文档（分块级联删除）；不存在返回 ErrNotFound。
func (d *DB) DeleteKnowledgeDocument(ctx context.Context, tenant string, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1 AND tenant_id = $2`, id, tenantOr(tenant))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: knowledge document %d", ErrNotFound, id)
	}
	return nil
}

const kchunkCols = `c.id, c.doc_id, c.seq, c.level, c.section_code, c.section_title, c.section_path, c.page_start, c.page_end,
	c.token_estimate, c.is_mandatory, jsonb_array_length(c.tables_json)`

const kchunkBodyCols = `c.content, c.tables_json, c.bbox_meta`

func scanKnowledgeChunk(r rowScanner, withBody bool, extra ...any) (*KnowledgeChunk, error) {
	var (
		c             KnowledgeChunk
		tables, boxes []byte
	)
	dest := []any{&c.ID, &c.DocID, &c.Seq, &c.Level, &c.SectionCode, &c.SectionTitle, &c.SectionPath, &c.PageStart, &c.PageEnd,
		&c.TokenEstimate, &c.IsMandatory, &c.TableCount}
	if withBody {
		dest = append(dest, &c.Content, &tables, &boxes)
	}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	if withBody {
		c.Tables, c.BBoxMeta = tables, boxes
	}
	return &c, nil
}

// KnowledgeChunks 列出文档分块（按 seq）；withBody 为 false 时只返回目录。
func (d *DB) KnowledgeChunks(ctx context.Context, tenant string, docID int64, withBody bool) ([]*KnowledgeChunk, error) {
	cols := kchunkCols
	if withBody {
		cols += ", " + kchunkBodyCols
	}
	rows, err := d.QueryContext(ctx, `SELECT `+cols+` FROM knowledge_chunks c
		WHERE c.tenant_id = $1 AND c.doc_id = $2 ORDER BY c.seq`, tenantOr(tenant), docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*KnowledgeChunk{}
	for rows.Next() {
		c, err := scanKnowledgeChunk(rows, withBody)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// KnowledgeChunkWindow 读取文档第 seq 个分块及其前 before、后 after 个分块（含正文与表格 JSON）。
func (d *DB) KnowledgeChunkWindow(ctx context.Context, tenant string, docID int64, seq, before, after int) (*KnowledgeDocument, *KnowledgeChunk, []*KnowledgeChunk, error) {
	doc, err := d.GetKnowledgeDocument(ctx, tenant, docID, false)
	if err != nil {
		return nil, nil, nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT `+kchunkCols+`, `+kchunkBodyCols+` FROM knowledge_chunks c
		WHERE c.tenant_id = $1 AND c.doc_id = $2 AND c.seq BETWEEN $3::int - $4::int AND $3::int + $5::int ORDER BY c.seq`,
		tenantOr(tenant), docID, seq, max(before, 0), max(after, 0))
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	var (
		target *KnowledgeChunk
		window []*KnowledgeChunk
	)
	for rows.Next() {
		c, err := scanKnowledgeChunk(rows, true)
		if err != nil {
			return nil, nil, nil, err
		}
		window = append(window, c)
		if c.Seq == seq {
			target = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	if target == nil {
		return nil, nil, nil, fmt.Errorf("%w: knowledge document %d chunk seq %d", ErrNotFound, docID, seq)
	}
	return doc, target, window, nil
}

// ---------------------------------------------------------------------------
// 混合检索
// ---------------------------------------------------------------------------

// KnowledgeQuery 是一次混合检索。
//   - Text：检索词（空白/中文标点分隔，词间 OR，由排序体现命中越多越靠前）；
//   - MustTerms：每个词都必须出现在文档标题、extra、章节标题/路径或正文中（如产品名），用于精确限定；
//   - Categories / Tags：类别与行业标签过滤（标签任一命中）；
//   - PreferTables：含结构化表格的分块加分（产品技术指标多在参数表中）。
type KnowledgeQuery struct {
	TenantID     string   `json:"tenant_id,omitempty"`
	Text         string   `json:"text"`
	MustTerms    []string `json:"must_terms,omitempty"`
	Categories   []string `json:"categories,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	DocID        int64    `json:"doc_id,omitempty"`
	Limit        int      `json:"limit,omitempty"`
	PreferTables bool     `json:"prefer_tables,omitempty"`
}

// KnowledgeHit 是一条检索命中；Score 为融合分，FTSRank / VectorScore / KeywordScore 为各路原始分。
type KnowledgeHit struct {
	Metadata     json.RawMessage   `json:"source_metadata,omitempty"`
	Chunk        *KnowledgeChunk   `json:"chunk"`
	Citation     KnowledgeCitation `json:"citation"`
	Tags         []string          `json:"tags"`
	Score        float64           `json:"score"`
	FTSRank      float64           `json:"fts_rank"`
	VectorScore  float64           `json:"vector_score"`
	KeywordScore float64           `json:"keyword_score"`
	MatchedTerms []string          `json:"matched_terms"`
}

// 融合权重与向量独立召回阈值。
const (
	weightFTS       = 0.5
	weightVector    = 0.3
	weightKeyword   = 0.2
	tableBonus      = 0.05
	minVectorRecall = 0.35
)

type kbCandidate struct {
	chunk *KnowledgeChunk
	doc   KnowledgeDocument
	fts   float64
	vec   float64
}

// SearchKnowledge 执行混合检索：全文召回（search_tsv @@ tsquery，ts_rank_cd 排序）与向量召回（kb_cosine，同 embedding_model）
// 各取候选池，合并后计算关键词命中率并加权融合：score = 0.5·fts/max(fts) + 0.3·max(vec,0) + 0.2·keyword (+0.05 表格加分)。
// 只有全文命中、关键词命中或向量相似度 ≥ 0.35 的候选进入结果。
func (d *DB) SearchKnowledge(ctx context.Context, q KnowledgeQuery, emb Embedder) ([]KnowledgeHit, error) {
	return d.searchKnowledge(ctx, q, emb, 0, 0)
}

// SearchKnowledgeWithIndex preserves baseline weighting and query preprocessing for ablation B.
// The explicitly selected ready index must belong to the current tenant snapshot and model.
func (d *DB) SearchKnowledgeWithIndex(ctx context.Context, q KnowledgeQuery, emb Embedder, indexID int64) ([]KnowledgeHit, error) {
	if emb == nil {
		return nil, ErrInvalidKnowledge
	}
	indexes, err := d.KnowledgeIndexes(ctx, q.TenantID)
	if err != nil {
		return nil, err
	}
	epoch, err := d.KnowledgeEpoch(ctx, q.TenantID)
	if err != nil {
		return nil, err
	}
	for _, idx := range indexes {
		if idx.ID == indexID && idx.Model == emb.Model() && idx.Epoch == epoch && idx.Status == "ready" && idx.Preprocessing == "title-path-body-v1" {
			return d.searchKnowledge(ctx, q, emb, indexID, idx.Dimension)
		}
	}
	return nil, ErrRetrievalConflict
}

func (d *DB) searchKnowledge(ctx context.Context, q KnowledgeQuery, emb Embedder, indexID int64, dimension int) ([]KnowledgeHit, error) {
	tenant := tenantOr(q.TenantID)
	if q.Limit <= 0 {
		q.Limit = 10
	}
	q.Limit = min(q.Limit, 50)
	for _, c := range q.Categories {
		if !ValidKnowledgeCategory(c) {
			return nil, fmt.Errorf("%w: unknown category %q", ErrInvalidKnowledge, c)
		}
	}
	terms := QueryTerms(q.Text)
	must := QueryTerms(strings.Join(q.MustTerms, " "))
	allTerms := append(append([]string(nil), terms...), must...)
	if len(allTerms) == 0 {
		return nil, fmt.Errorf("%w: query is required", ErrInvalidKnowledge)
	}
	mustPatterns := make([]string, 0, len(must))
	for _, m := range must {
		mustPatterns = append(mustPatterns, likePattern(m))
	}
	pool := max(q.Limit*5, 50)
	cats := q.Categories
	if cats == nil {
		cats = []string{}
	}
	filter := `c.tenant_id = $1 AND d.tenant_id = $1
		  AND (COALESCE(cardinality($2::text[]), 0) = 0 OR d.category = ANY($2::text[]))
		  AND (COALESCE(cardinality($3::text[]), 0) = 0 OR d.tags && $3::text[])
		  AND ($4::bigint = 0 OR c.doc_id = $4::bigint)
		  AND NOT EXISTS (SELECT 1 FROM unnest($5::text[]) AS m(p)
		                  WHERE NOT (d.title ILIKE m.p OR d.extra::text ILIKE m.p OR c.section_title ILIKE m.p
		                             OR c.section_path ILIKE m.p OR c.content ILIKE m.p))`
	args := []any{tenant, cats, NormalizeTags(q.Tags), q.DocID, mustPatterns}
	selectCols := kchunkCols + `, ` + kchunkBodyCols + `, d.id, d.title, d.category, d.file_name, to_jsonb(d.tags)`

	cands := map[int64]*kbCandidate{}
	var order []int64
	collect := func(rows *sql.Rows, withScore func(c *kbCandidate, score float64)) error {
		defer rows.Close()
		for rows.Next() {
			var (
				doc   KnowledgeDocument
				tags  []byte
				score float64
			)
			ch, err := scanKnowledgeChunk(rows, true, &doc.ID, &doc.Title, &doc.Category, &doc.FileName, &tags, &score)
			if err != nil {
				return err
			}
			_ = json.Unmarshal(tags, &doc.Tags)
			c, ok := cands[ch.ID]
			if !ok {
				c = &kbCandidate{chunk: ch, doc: doc}
				cands[ch.ID] = c
				order = append(order, ch.ID)
			}
			withScore(c, score)
		}
		return rows.Err()
	}

	if tsq := KnowledgeTSQuery(allTerms); tsq != "" {
		rows, err := d.QueryContext(ctx, `SELECT `+selectCols+`, ts_rank_cd(c.search_tsv, to_tsquery('simple', $6), 1)::float8 AS rank
			FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.doc_id
			WHERE `+filter+` AND c.search_tsv @@ to_tsquery('simple', $6)
			ORDER BY rank DESC, c.id LIMIT $7`, append(args, tsq, pool)...)
		if err != nil {
			return nil, err
		}
		if err := collect(rows, func(c *kbCandidate, s float64) { c.fts = s }); err != nil {
			return nil, err
		}
	}
	if emb != nil {
		vecs, err := EmbedQuery(ctx, emb, []string{strings.Join(allTerms, " ")})
		if err != nil {
			return nil, fmt.Errorf("db: embed query: %w", err)
		}
		if len(vecs) != 1 || len(vecs[0]) == 0 {
			return nil, fmt.Errorf("invalid query embedding")
		}
		if dimension > 0 {
			if err = ValidateVectors(vecs, 1, dimension); err != nil {
				return nil, err
			}
		}
		vectorExpr, vectorJoin, vectorFilter := "c.embedding", "", `c.embedding IS NOT NULL AND c.embedding_model = $7`
		var modelOrIndex any = emb.Model()
		if indexID > 0 {
			vectorExpr = "ve.embedding"
			vectorJoin = ` JOIN knowledge_embeddings ve ON ve.chunk_id=c.id AND ve.tenant_id=c.tenant_id`
			vectorFilter = `ve.index_id=$7`
			modelOrIndex = indexID
		}
		rows, err := d.QueryContext(ctx, `SELECT `+selectCols+`, kb_cosine(`+vectorExpr+`, $6::float8[]) AS sim
			FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.doc_id`+vectorJoin+`
			WHERE `+filter+` AND `+vectorFilter+`
			ORDER BY sim DESC, c.id LIMIT $8`, append(args, vecs[0], modelOrIndex, pool)...)
		if err != nil {
			return nil, err
		}
		if err := collect(rows, func(c *kbCandidate, s float64) { c.vec = s }); err != nil {
			return nil, err
		}
	}

	maxFTS := 0.0
	for _, c := range cands {
		maxFTS = math.Max(maxFTS, c.fts)
	}
	hits := make([]KnowledgeHit, 0, len(order))
	for _, id := range order {
		c := cands[id]
		hay := strings.ToLower(c.doc.Title + "\n" + c.chunk.SectionPath + "\n" + c.chunk.SectionTitle + "\n" + c.chunk.Content)
		matched := []string{}
		for _, t := range allTerms {
			if strings.Contains(hay, strings.ToLower(t)) {
				matched = append(matched, t)
			}
		}
		kw := float64(len(matched)) / float64(len(allTerms))
		if c.fts == 0 && kw == 0 && c.vec < minVectorRecall {
			continue
		}
		score := weightKeyword * kw
		if maxFTS > 0 {
			score += weightFTS * c.fts / maxFTS
		}
		score += weightVector * math.Max(c.vec, 0)
		if q.PreferTables && c.chunk.TableCount > 0 {
			score += tableBonus
		}
		tags := c.doc.Tags
		if tags == nil {
			tags = []string{}
		}
		hits = append(hits, KnowledgeHit{Chunk: c.chunk, Citation: c.chunk.Cite(&c.doc), Tags: tags,
			Score: round4(score), FTSRank: round4(c.fts), VectorScore: round4(c.vec), KeywordScore: round4(kw), MatchedTerms: matched})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Chunk.ID < hits[j].Chunk.ID
	})
	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return hits, nil
}

func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }
