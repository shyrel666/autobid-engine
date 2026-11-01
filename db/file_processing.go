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

var ErrFileProcessingConflict = errors.New("file processing request/version conflict")

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
}

const processingCols = `id,file_id,operation,request_key,fingerprint,source_hash,target_document_id,expected_revision,status,payload,preview,result,error,created_by,approved_by,created_at,adopted_at`

func scanProcessing(r rowScanner) (*FileProcessingRecord, error) {
	var v FileProcessingRecord
	err := r.Scan(&v.ID, &v.FileID, &v.Operation, &v.RequestKey, &v.Fingerprint, &v.SourceHash, &v.TargetDocumentID, &v.ExpectedRevision, &v.Status, &v.Payload, &v.Preview, &v.Result, &v.Error, &v.CreatedBy, &v.ApprovedBy, &v.CreatedAt, &v.AdoptedAt)
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
 (tenant_id,task_id,file_id,operation,request_key,fingerprint,source_hash,target_document_id,expected_revision,status,payload,preview,result,error,created_by,updated_by)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13::jsonb,$14,$15,$15)
 ON CONFLICT(tenant_id,task_id,request_key) DO UPDATE SET request_key=EXCLUDED.request_key RETURNING `+processingCols,
		tenantOr(tenant), taskID, v.FileID, v.Operation, v.RequestKey, v.Fingerprint, v.SourceHash, v.TargetDocumentID, v.ExpectedRevision, v.Status, jsonOr(v.Payload, "{}"), jsonOr(v.Preview, "{}"), jsonOr(v.Result, "{}"), v.Error, v.CreatedBy))
	if err == nil && saved.Fingerprint != v.Fingerprint {
		return nil, ErrFileProcessingConflict
	}
	return saved, err
}
func (d *DB) CompleteFileProcessing(ctx context.Context, tenant string, taskID, id int64, result json.RawMessage, actor, note string) error {
	_, err := d.ExecContext(ctx, `UPDATE file_processing_records SET status='adopted',result=$4::jsonb,approved_by=$5,updated_by=$5,review_note=$6,adopted_at=now() WHERE tenant_id=$1 AND task_id=$2 AND id=$3`, tenantOr(tenant), taskID, id, string(result), actor, note)
	return err
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
