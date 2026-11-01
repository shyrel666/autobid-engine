package db

// F1：正式招标文档的重新解析修订。采用“原地按 seq 差量更新 + 不可变快照”模型：
// 文档 ID 与 (doc_id, seq) 不变；旧正文/分块全集写入 tender_parse_revisions；
// 只有实质内容变化才推进 requirement_manifests.source_epoch 并使要求待复核。
// content_hash 保持原始上传字节哈希语义，不在修订时覆盖。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

var (
	ErrParseRevisionConflict = errors.New("db: tender parse revision conflict")
	ErrInvalidParseRevision  = errors.New("db: invalid tender parse revision")
)

// ParseRevisionInput 是一次采用请求；Chunks 的 Seq 必须唯一且 >= 0。
type ParseRevisionInput struct {
	TenantID         string
	TaskID           int64
	DocID            int64
	ExpectedRevision int
	OriginalFileID   *int64
	ParserVersion    string
	RawText          string
	ParseMeta        json.RawMessage
	PageCount        int
	Chunks           []DocumentChunk
	Actor            string
}

type ParseRevisionResult struct {
	DocumentID       int64 `json:"document_id"`
	RevisionNo       int   `json:"revision_no"`       // 采用后的 parse_revision
	SnapshotRevision int   `json:"snapshot_revision"` // 快照对应的旧 parse_revision
	SnapshotID       int64 `json:"snapshot_id"`
	Inserted         int   `json:"inserted"`
	Updated          int   `json:"updated"`
	Deleted          int   `json:"deleted"`
	Unchanged        int   `json:"unchanged"`
	SourceBumped     bool  `json:"source_bumped"` // 是否发生实质来源变化
}

type revisionChunk struct {
	ID            int64           `json:"id"`
	Seq           int             `json:"seq"`
	Level         int             `json:"level"`
	SectionCode   string          `json:"section_code"`
	SectionTitle  string          `json:"section_title"`
	PageNo        int             `json:"page_no"`
	PageStart     *int            `json:"page_start"`
	PageEnd       *int            `json:"page_end"`
	Content       string          `json:"content"`
	TokenEstimate int             `json:"token_estimate"`
	IsMandatory   bool            `json:"is_mandatory"`
	Tables        json.RawMessage `json:"tables"`
	BBoxMeta      json.RawMessage `json:"bbox_meta"`
}

type revisionDocument struct {
	RawText        string
	ContentHash    string
	PageCount      *int
	ParseMeta      json.RawMessage
	OriginalFileID *int64
	ParseRevision  int
}

// AdoptTenderParseRevision 在一个事务内采用重新解析结果。
func (d *DB) AdoptTenderParseRevision(ctx context.Context, in ParseRevisionInput) (*ParseRevisionResult, error) {
	tenant := tenantOr(in.TenantID)
	if in.TaskID <= 0 || in.DocID <= 0 || in.ExpectedRevision < 0 {
		return nil, fmt.Errorf("%w: task_id/doc_id/expected_revision are required", ErrInvalidParseRevision)
	}
	if strings.TrimSpace(in.RawText) == "" || len(in.Chunks) == 0 {
		return nil, fmt.Errorf("%w: raw_text and chunks are required", ErrInvalidParseRevision)
	}
	seen := map[int]bool{}
	for i := range in.Chunks {
		c := &in.Chunks[i]
		if c.Seq < 0 || seen[c.Seq] {
			return nil, fmt.Errorf("%w: duplicate or negative seq %d", ErrInvalidParseRevision, c.Seq)
		}
		seen[c.Seq] = true
		if c.ID != 0 {
			return nil, fmt.Errorf("%w: new chunk %d must not carry an existing id", ErrInvalidParseRevision, c.Seq)
		}
	}
	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		actor = "system"
	}
	var out *ParseRevisionResult
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		doc, err := lockDocumentForRevision(ctx, tx, tenant, in.TaskID, in.DocID)
		if err != nil {
			return err
		}
		if doc.ParseRevision != in.ExpectedRevision {
			return fmt.Errorf("%w: current parse_revision %d, expected %d", ErrParseRevisionConflict, doc.ParseRevision, in.ExpectedRevision)
		}
		if in.OriginalFileID != nil {
			var hash, availability string
			err := tx.QueryRowContext(ctx, `SELECT sha256, availability FROM task_file_objects
				WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR SHARE`, tenant, in.TaskID, *in.OriginalFileID).Scan(&hash, &availability)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: original file %d not found for this task", ErrInvalidParseRevision, *in.OriginalFileID)
			}
			if err != nil {
				return err
			}
			if availability != "available" {
				return fmt.Errorf("%w: original file %d is %s", ErrInvalidParseRevision, *in.OriginalFileID, availability)
			}
			// 修订只能重新解析同一原件，不能更换原始字节却继续沿用旧 content_hash。
			if hash != doc.ContentHash {
				return fmt.Errorf("%w: original file %d hash does not match document %d", ErrInvalidParseRevision, *in.OriginalFileID, in.DocID)
			}
		}
		// 与要求提取串行化：提取锁清单行，这里也锁；快照/差量/失效在同一提交边界。
		if _, err := ensureManifestTx(ctx, tx, tenant, in.TaskID, true); err != nil {
			return err
		}
		old, err := loadRevisionChunks(ctx, tx, tenant, in.TaskID, in.DocID)
		if err != nil {
			return err
		}
		snapshot, err := json.Marshal(old)
		if err != nil {
			return err
		}
		result := &ParseRevisionResult{DocumentID: in.DocID, SnapshotRevision: doc.ParseRevision,
			RevisionNo: doc.ParseRevision + 1, Inserted: 0, Updated: 0, Deleted: 0, Unchanged: 0}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO tender_parse_revisions(tenant_id, task_id, document_id, revision_no, original_file_id,
				parser_version, raw_text, chunks_snapshot, parse_meta, adopted_by, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10,$10)
			RETURNING id`, tenant, in.TaskID, in.DocID, doc.ParseRevision, doc.OriginalFileID,
			parseMetaVersion(doc.ParseMeta), doc.RawText, string(snapshot), jsonOr(doc.ParseMeta, "{}"), actor).Scan(&result.SnapshotID); err != nil {
			return fmt.Errorf("snapshot tender parse revision: %w", err)
		}
		oldBySeq := make(map[int]*revisionChunk, len(old))
		for i := range old {
			oldBySeq[old[i].Seq] = &old[i]
		}
		newBySeq := make(map[int]*DocumentChunk, len(in.Chunks))
		for i := range in.Chunks {
			newBySeq[in.Chunks[i].Seq] = &in.Chunks[i]
		}
		contentChanged := false
		for seq, nc := range newBySeq {
			prev, ok := oldBySeq[seq]
			if !ok {
				if err := insertRevisionChunk(ctx, tx, in.DocID, in.TaskID, nc); err != nil {
					return err
				}
				result.Inserted++
				continue
			}
			changed, content := revisionChunkChanged(prev, nc)
			if !changed {
				result.Unchanged++
				delete(oldBySeq, seq)
				continue
			}
			if err := updateRevisionChunk(ctx, tx, prev.ID, in.DocID, in.TaskID, nc); err != nil {
				return err
			}
			result.Updated++
			contentChanged = contentChanged || content
			delete(oldBySeq, seq)
		}
		for _, prev := range oldBySeq {
			if _, err := tx.ExecContext(ctx, `DELETE FROM document_chunks WHERE id = $1 AND doc_id = $2 AND task_id = $3`,
				prev.ID, in.DocID, in.TaskID); err != nil {
				return fmt.Errorf("delete chunk seq %d: %w", prev.Seq, err)
			}
			result.Deleted++
		}
		result.SourceBumped = result.Inserted > 0 || result.Deleted > 0 || contentChanged
		// 更新（内容列）与删除已由 requirement_source_changed 触发器推进 source_epoch；插入没有触发器，这里补齐一次。
		if result.Inserted > 0 {
			if _, err := tx.ExecContext(ctx, `SELECT requirement_source_bump($1,$2,$3)`, tenant, in.TaskID, actor); err != nil {
				return err
			}
		}
		meta, err := revisionParseMeta(in)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `UPDATE tender_documents
			SET raw_text = $4, parse_meta = $5::jsonb, page_count = NULLIF($6, 0),
			    original_file_id = COALESCE($7, original_file_id), parse_revision = parse_revision + 1
			WHERE tenant_id = $1 AND task_id = $2 AND id = $3
			RETURNING parse_revision`, tenant, in.TaskID, in.DocID, in.RawText, string(meta), in.PageCount,
			in.OriginalFileID).Scan(&result.RevisionNo); err != nil {
			return fmt.Errorf("update tender document revision: %w", err)
		}
		out = result
		return nil
	})
	return out, err
}

func lockDocumentForRevision(ctx context.Context, tx *sql.Tx, tenant string, taskID, docID int64) (*revisionDocument, error) {
	var doc revisionDocument
	var pageCount sql.NullInt32
	var meta []byte
	err := tx.QueryRowContext(ctx, `SELECT raw_text, content_hash, page_count, COALESCE(parse_meta, '{}'::jsonb),
		original_file_id, parse_revision FROM tender_documents
		WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR UPDATE`, tenant, taskID, docID).Scan(
		&doc.RawText, &doc.ContentHash, &pageCount, &meta, &doc.OriginalFileID, &doc.ParseRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: document %d", ErrNotFound, docID)
	}
	if err != nil {
		return nil, err
	}
	if pageCount.Valid {
		n := int(pageCount.Int32)
		doc.PageCount = &n
	}
	doc.ParseMeta = meta
	return &doc, nil
}

func loadRevisionChunks(ctx context.Context, tx *sql.Tx, tenant string, taskID, docID int64) ([]revisionChunk, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id, c.seq, c.level, c.section_code, c.section_title, c.page_no,
		c.page_start, c.page_end, c.content, c.token_estimate, c.is_mandatory,
		COALESCE(c.tables_json, '[]'::jsonb), COALESCE(c.bbox_meta, '{}'::jsonb)
		FROM document_chunks c JOIN tender_documents d ON d.id = c.doc_id
		WHERE c.doc_id = $1 AND c.task_id = $2 AND d.tenant_id = $3 ORDER BY c.seq FOR UPDATE`, docID, taskID, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []revisionChunk
	for rows.Next() {
		var c revisionChunk
		var start, end sql.NullInt32
		var tables, boxes []byte
		if err := rows.Scan(&c.ID, &c.Seq, &c.Level, &c.SectionCode, &c.SectionTitle, &c.PageNo, &start, &end,
			&c.Content, &c.TokenEstimate, &c.IsMandatory, &tables, &boxes); err != nil {
			return nil, err
		}
		if start.Valid {
			v := int(start.Int32)
			c.PageStart = &v
		}
		if end.Valid {
			v := int(end.Int32)
			c.PageEnd = &v
		}
		c.Tables, c.BBoxMeta = tables, boxes
		out = append(out, c)
	}
	return out, rows.Err()
}

func insertRevisionChunk(ctx context.Context, tx *sql.Tx, docID, taskID int64, c *DocumentChunk) error {
	pageNo, start, end := chunkPageValues(c)
	if _, err := tx.ExecContext(ctx, `INSERT INTO document_chunks(doc_id, task_id, seq, level, section_code, section_title,
		page_no, page_start, page_end, content, token_estimate, is_mandatory, tables_json, bbox_meta)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb,$14::jsonb)`,
		docID, taskID, c.Seq, c.Level, c.SectionCode, c.SectionTitle, pageNo, start, end, c.Content, c.TokenEstimate,
		c.IsMandatory, jsonOr(c.Tables, "[]"), jsonOr(c.BBoxMeta, "{}")); err != nil {
		return fmt.Errorf("insert chunk seq %d: %w", c.Seq, err)
	}
	return nil
}

func updateRevisionChunk(ctx context.Context, tx *sql.Tx, id, docID, taskID int64, c *DocumentChunk) error {
	pageNo, start, end := chunkPageValues(c)
	if _, err := tx.ExecContext(ctx, `UPDATE document_chunks SET seq=$4, level=$5, section_code=$6, section_title=$7,
		page_no=$8, page_start=$9, page_end=$10, content=$11, token_estimate=$12, is_mandatory=$13,
		tables_json=$14::jsonb, bbox_meta=$15::jsonb
		WHERE id=$1 AND doc_id=$2 AND task_id=$3`,
		id, docID, taskID, c.Seq, c.Level, c.SectionCode, c.SectionTitle, pageNo, start, end, c.Content, c.TokenEstimate,
		c.IsMandatory, jsonOr(c.Tables, "[]"), jsonOr(c.BBoxMeta, "{}")); err != nil {
		return fmt.Errorf("update chunk seq %d: %w", c.Seq, err)
	}
	return nil
}

func chunkPageValues(c *DocumentChunk) (pageNo int, start, end any) {
	pageNo = max(c.PageNo, 1)
	if c.PageStart > 0 || c.PageNo > 0 {
		s := max(c.PageStart, c.PageNo, 1)
		return pageNo, s, max(c.PageEnd, s)
	}
	return pageNo, nil, nil
}

// revisionChunkChanged 比较全部字段；content 表示是否命中检索/要求失效的内容列。
func revisionChunkChanged(prev *revisionChunk, next *DocumentChunk) (changed, content bool) {
	pageNo, start, end := chunkPageValues(next)
	changed = prev.Level != next.Level || prev.SectionCode != next.SectionCode || prev.SectionTitle != next.SectionTitle ||
		prev.PageNo != pageNo || !sameNullableInt(prev.PageStart, start) || !sameNullableInt(prev.PageEnd, end) ||
		prev.Content != next.Content || prev.TokenEstimate != next.TokenEstimate || prev.IsMandatory != next.IsMandatory ||
		!sameJSONValue(prev.Tables, jsonOr(next.Tables, "[]")) || !sameJSONValue(prev.BBoxMeta, jsonOr(next.BBoxMeta, "{}"))
	content = prev.SectionCode != next.SectionCode || prev.SectionTitle != next.SectionTitle ||
		prev.Content != next.Content || !sameNullableInt(prev.PageStart, start) || !sameNullableInt(prev.PageEnd, end) ||
		!sameJSONValue(prev.Tables, jsonOr(next.Tables, "[]"))
	return changed, content
}

func sameNullableInt(prev *int, next any) bool {
	if next == nil {
		return prev == nil
	}
	n, ok := next.(int)
	return ok && prev != nil && *prev == n
}

func sameJSONValue(a []byte, b string) bool {
	var av, bv any
	if len(a) == 0 {
		a = []byte("null")
	}
	if json.Unmarshal(a, &av) != nil || json.Unmarshal([]byte(b), &bv) != nil {
		return string(a) == b
	}
	return reflect.DeepEqual(av, bv)
}

func parseMetaVersion(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if v, ok := m["parser_version"].(string); ok && v != "" {
		return v
	}
	if v, ok := m["parser"].(string); ok {
		return v
	}
	return ""
}

// revisionParseMeta 合并新解析元数据与派生哈希；content_hash 仍保存原始上传字节哈希。
func revisionParseMeta(in ParseRevisionInput) ([]byte, error) {
	m := map[string]any{}
	if len(in.ParseMeta) > 0 && string(in.ParseMeta) != "null" {
		if err := json.Unmarshal(in.ParseMeta, &m); err != nil {
			return nil, fmt.Errorf("%w: parse_meta must be a JSON object", ErrInvalidParseRevision)
		}
	}
	if in.ParserVersion != "" {
		m["parser_version"] = in.ParserVersion
	}
	sum := sha256.Sum256([]byte(in.RawText))
	m["raw_text_sha256"] = hex.EncodeToString(sum[:])
	chunks, err := json.Marshal(in.Chunks)
	if err != nil {
		return nil, err
	}
	sum = sha256.Sum256(chunks)
	m["chunks_sha256"] = hex.EncodeToString(sum[:])
	return json.Marshal(m)
}
