package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrFileProcessingConflict = errors.New("file processing request/version conflict")
	// ErrInvalidReview 表示人工复核输入不合法（如拒绝候选未写原因）。
	ErrInvalidReview = errors.New("file processing review input is invalid")
)

// 候选解析配置（S5a，AGENT_TOOL_SKILLS_PLAN 4.1）：auto 原生文本层 + 无文本层页面 OCR；native 不调用 OCR；
// ocr_only 每页都由页面图像 OCR；enhanced_pdf 增强后端尚未交付。
const (
	ParserProfileAuto        = "auto"
	ParserProfileNative      = "native"
	ParserProfileOCROnly     = "ocr_only"
	ParserProfileEnhancedPDF = "enhanced_pdf"
)

// 处理记录状态。
const (
	RecordReady    = "ready"
	RecordAdopted  = "adopted"
	RecordFailed   = "failed"
	RecordRejected = "rejected"
)

// Fixed imports and adoptions in one task serialize their short commit phases.
// Parsing runs outside this lock. This also prevents file/document lock inversion
// when importing a previously linked original concurrently with adopting it.
func (d *DB) LockFileProcessingTask(ctx context.Context, tenant string, taskID int64) error {
	// Updating the row also invalidates an older REPEATABLE READ export snapshot.
	res, err := d.ExecContext(ctx, `UPDATE tasks SET file_gate_version=file_gate_version+1 WHERE id=$1`, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	_, err = d.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 7319))`, fmt.Sprintf("file-processing:%s:%d", tenantOr(tenant), taskID))
	return err
}

type FileProcessingRecord struct {
	ID               int64           `json:"id"`
	FileID           int64           `json:"file_id"`
	Operation        string          `json:"operation"`
	RequestKey       string          `json:"request_key"`
	Fingerprint      string          `json:"-"`
	SourceHash       string          `json:"source_hash"`
	TargetDocumentID *int64          `json:"target_document_id,omitempty"`
	ExpectedRevision int             `json:"expected_revision"`
	Status           string          `json:"status"`
	Payload          json.RawMessage `json:"-"`
	Preview          json.RawMessage `json:"preview"`
	Result           json.RawMessage `json:"result"`
	Error            string          `json:"error,omitempty"`
	CreatedBy        string          `json:"created_by"`
	ApprovedBy       string          `json:"approved_by"`
	CreatedAt        time.Time       `json:"created_at"`
	AdoptedAt        *time.Time      `json:"adopted_at,omitempty"`
	ReviewNote       string          `json:"review_note,omitempty"`
	// CandidateOnly 表示导入只生成候选，人工采用后才写入正式招标文档（Agent 提交恒为 true）。
	CandidateOnly bool `json:"candidate_only"`
	// ParserProfile / ParserVersion 是首次提交时固定的解析配置与实际执行版本（含锁定的 OCR 服务版本）。
	ParserProfile string     `json:"parser_profile,omitempty"`
	ParserVersion string     `json:"parser_version,omitempty"`
	RejectedBy    string     `json:"rejected_by,omitempty"`
	RejectReason  string     `json:"reject_reason,omitempty"`
	RejectedAt    *time.Time `json:"rejected_at,omitempty"`
}

const processingCols = `id,file_id,operation,request_key,fingerprint,source_hash,target_document_id,expected_revision,status,payload,preview,result,error,created_by,approved_by,created_at,adopted_at,
	review_note,candidate_only,parser_profile,parser_version,rejected_by,reject_reason,rejected_at`

func scanProcessing(r rowScanner) (*FileProcessingRecord, error) {
	var v FileProcessingRecord
	err := r.Scan(&v.ID, &v.FileID, &v.Operation, &v.RequestKey, &v.Fingerprint, &v.SourceHash, &v.TargetDocumentID, &v.ExpectedRevision, &v.Status, &v.Payload, &v.Preview, &v.Result, &v.Error, &v.CreatedBy, &v.ApprovedBy, &v.CreatedAt, &v.AdoptedAt,
		&v.ReviewNote, &v.CandidateOnly, &v.ParserProfile, &v.ParserVersion, &v.RejectedBy, &v.RejectReason, &v.RejectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &v, err
}
func (d *DB) GetFileProcessingRecord(ctx context.Context, tenant string, taskID, id int64, lock bool) (*FileProcessingRecord, error) {
	q := `SELECT ` + processingCols + ` FROM file_processing_records WHERE tenant_id=$1 AND task_id=$2 AND id=$3`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanProcessing(d.QueryRowContext(ctx, q, tenantOr(tenant), taskID, id))
}
func (d *DB) FindFileProcessingRequest(ctx context.Context, tenant string, taskID int64, key string) (*FileProcessingRecord, error) {
	return scanProcessing(d.QueryRowContext(ctx, `SELECT `+processingCols+` FROM file_processing_records WHERE tenant_id=$1 AND task_id=$2 AND request_key=$3`, tenantOr(tenant), taskID, key))
}
func (d *DB) ListFileProcessingRecords(ctx context.Context, tenant string, taskID, before int64) ([]*FileProcessingRecord, error) {
	// Payload and detailed diffs are fetched through the bounded detail endpoint.
	listCols := strings.Replace(processingCols, "payload,preview", "'{}'::jsonb,'{}'::jsonb", 1)
	rows, err := d.QueryContext(ctx, `SELECT `+listCols+` FROM file_processing_records WHERE tenant_id=$1 AND task_id=$2 AND ($3::bigint=0 OR id<$3) ORDER BY id DESC LIMIT 50`, tenantOr(tenant), taskID, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*FileProcessingRecord{}
	for rows.Next() {
		v, err := scanProcessing(rows)
		if err != nil {
			return nil, err
		}
		v.Payload = nil
		v.Preview = json.RawMessage(`{}`)
		out = append(out, v)
	}
	return out, rows.Err()
}

// InsertFileProcessingRecord must join the business transaction for successful imports.
func (d *DB) InsertFileProcessingRecord(ctx context.Context, tenant string, taskID int64, v *FileProcessingRecord) (*FileProcessingRecord, error) {
	saved, err := scanProcessing(d.QueryRowContext(ctx, `INSERT INTO file_processing_records
 (tenant_id,task_id,file_id,operation,request_key,fingerprint,source_hash,target_document_id,expected_revision,status,payload,preview,result,error,created_by,updated_by,
  candidate_only,parser_profile,parser_version)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13::jsonb,$14,$15,$15,$16,$17,$18)
 ON CONFLICT(tenant_id,task_id,request_key) DO UPDATE SET request_key=EXCLUDED.request_key RETURNING `+processingCols,
		tenantOr(tenant), taskID, v.FileID, v.Operation, v.RequestKey, v.Fingerprint, v.SourceHash, v.TargetDocumentID, v.ExpectedRevision, v.Status, jsonOr(v.Payload, "{}"), jsonOr(v.Preview, "{}"), jsonOr(v.Result, "{}"), v.Error, v.CreatedBy,
		v.CandidateOnly, v.ParserProfile, v.ParserVersion))
	if err == nil && saved.Fingerprint != v.Fingerprint {
		return nil, ErrFileProcessingConflict
	}
	return saved, err
}
func (d *DB) CompleteFileProcessing(ctx context.Context, tenant string, taskID, id int64, result json.RawMessage, actor, note string) error {
	_, err := d.ExecContext(ctx, `UPDATE file_processing_records SET status='adopted',result=$4::jsonb,approved_by=$5,updated_by=$5,review_note=$6,adopted_at=now() WHERE tenant_id=$1 AND task_id=$2 AND id=$3`, tenantOr(tenant), taskID, id, string(result), actor, note)
	return err
}

// RejectFileProcessingRecord 由人工拒绝一份待采用候选（S5a）：持久化拒绝人、原因与时间。重复拒绝原样返回；
// 已采用或失败的记录不能拒绝。候选正文保留供审计；拒绝不会修改正式招标文档，也不推进来源 epoch。
func (d *DB) RejectFileProcessingRecord(ctx context.Context, tenant string, taskID, id int64, actor, reason string) (*FileProcessingRecord, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len([]rune(reason)) > 500 {
		return nil, fmt.Errorf("%w: reason is required (max 500 characters)", ErrInvalidReview)
	}
	if strings.TrimSpace(actor) == "" {
		actor = "operator"
	}
	var out *FileProcessingRecord
	err := d.Transaction(ctx, func(tx *DB) error {
		if err := tx.LockFileProcessingTask(ctx, tenant, taskID); err != nil {
			return err
		}
		cur, err := tx.GetFileProcessingRecord(ctx, tenant, taskID, id, true)
		if err != nil {
			return err
		}
		switch cur.Status {
		case RecordRejected:
			out = cur
			return nil
		case RecordAdopted:
			return fmt.Errorf("%w: 候选已采用，不能再拒绝", ErrFileProcessingConflict)
		case RecordFailed:
			return fmt.Errorf("%w: 解析失败的记录没有可拒绝的候选", ErrFileProcessingConflict)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE file_processing_records SET status='rejected',rejected_by=$4,reject_reason=$5,rejected_at=now(),updated_by=$4
			WHERE tenant_id=$1 AND task_id=$2 AND id=$3 AND status='ready'`, tenantOr(tenant), taskID, id, actor, reason); err != nil {
			return err
		}
		out, err = tx.GetFileProcessingRecord(ctx, tenant, taskID, id, false)
		return err
	})
	return out, err
}

// GetTenderParseRecord 读取一个解析记录并复验宿主范围（S5a document_parse_result）：作业类型、原件范围；
// 宿主限定对象时，只能读本 Intent 订阅的解析作业产生的记录。
func (d *DB) GetTenderParseRecord(ctx context.Context, tenant string, taskID, id int64) (*FileProcessingRecord, error) {
	if err := CheckJobKindScope(ctx, JobKindTenderParse); err != nil {
		return nil, err
	}
	r, err := d.GetFileProcessingRecord(ctx, tenant, taskID, id, false)
	if err != nil {
		return nil, err
	}
	if err := CheckFileScope(ctx, r.FileID); err != nil {
		return nil, err
	}
	if p, ok := intentExecutionFrom(ctx); ok && restrictsObjects(ctx) {
		var owned bool
		if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_job_subscriptions s JOIN fixed_file_jobs j
				ON j.tenant_id = s.tenant_id AND j.task_id = s.task_id AND j.id = s.job_id
			WHERE s.tenant_id = $1 AND s.task_id = $2 AND s.intent_id = $3 AND s.job_kind = 'tender_parse' AND j.record_id = $4)`,
			tenantOr(tenant), taskID, p.ID, id).Scan(&owned); err != nil {
			return nil, err
		}
		if !owned {
			return nil, fmt.Errorf("execution scope: parse record #%d is not produced by a job of the current intent", id)
		}
	}
	return r, nil
}
func (d *DB) LockTaskFile(ctx context.Context, tenant string, taskID, id int64, hash string) error {
	var actual, state string
	err := d.QueryRowContext(ctx, `SELECT sha256,availability FROM task_file_objects WHERE tenant_id=$1 AND task_id=$2 AND id=$3 FOR UPDATE`, tenantOr(tenant), taskID, id).Scan(&actual, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if actual != hash || state != "available" {
		return fmt.Errorf("%w: 原件已变化或不可用", ErrInvalidParseRevision)
	}
	return nil
}

type ParseChange struct {
	Seq    int            `json:"seq"`
	Kind   string         `json:"kind"`
	Before *DocumentChunk `json:"before,omitempty"`
	After  *DocumentChunk `json:"after,omitempty"`
}
type ParsePreview struct {
	Inserted             int           `json:"inserted"`
	Updated              int           `json:"updated"`
	Deleted              int           `json:"deleted"`
	Unchanged            int           `json:"unchanged"`
	AffectedRequirements int           `json:"affected_requirements"`
	Changes              []ParseChange `json:"changes"`
}

// PreviewParseRevision uses the same comparison as adoption and locks the base
// revision while constructing the saved preview. No formal data is changed.
func (d *DB) PreviewParseRevision(ctx context.Context, in ParseRevisionInput) (*ParsePreview, error) {
	var out *ParsePreview
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		doc, err := lockDocumentForRevision(ctx, tx, tenantOr(in.TenantID), in.TaskID, in.DocID)
		if err != nil {
			return err
		}
		if doc.ParseRevision != in.ExpectedRevision {
			return ErrParseRevisionConflict
		}
		old, err := loadRevisionChunks(ctx, tx, tenantOr(in.TenantID), in.TaskID, in.DocID)
		if err != nil {
			return err
		}
		bySeq := map[int]*revisionChunk{}
		for i := range old {
			bySeq[old[i].Seq] = &old[i]
		}
		out = &ParsePreview{Changes: []ParseChange{}}
		ids := []int64{}
		convert := func(c *revisionChunk) *DocumentChunk {
			v := &DocumentChunk{Seq: c.Seq, Content: c.Content, SectionCode: c.SectionCode, SectionTitle: c.SectionTitle, Tables: c.Tables, BBoxMeta: c.BBoxMeta}
			if c.PageStart != nil {
				v.PageStart = *c.PageStart
			}
			if c.PageEnd != nil {
				v.PageEnd = *c.PageEnd
			}
			return v
		}
		for i := range in.Chunks {
			n := &in.Chunks[i]
			o, ok := bySeq[n.Seq]
			if !ok {
				out.Inserted++
				out.Changes = append(out.Changes, ParseChange{Seq: n.Seq, Kind: "insert", After: n})
				continue
			}
			changed, content := revisionChunkChanged(o, n)
			delete(bySeq, n.Seq)
			if !changed {
				out.Unchanged++
				continue
			}
			out.Updated++
			out.Changes = append(out.Changes, ParseChange{Seq: n.Seq, Kind: "update", Before: convert(o), After: n})
			if content {
				ids = append(ids, o.ID)
			}
		}
		for _, o := range old {
			if _, ok := bySeq[o.Seq]; ok {
				out.Deleted++
				ids = append(ids, o.ID)
				out.Changes = append(out.Changes, ParseChange{Seq: o.Seq, Kind: "delete", Before: convert(&o)})
			}
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM requirement_items WHERE tenant_id=$1 AND task_id=$2 AND source_chunk_id=ANY($3::bigint[])`, tenantOr(in.TenantID), in.TaskID, ids).Scan(&out.AffectedRequirements)
	})
	return out, err
}
