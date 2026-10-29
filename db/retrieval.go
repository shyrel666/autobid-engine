package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

var ErrRetrievalConflict = errors.New("retrieval conflict")
var ErrRetrievalBudget = errors.New("retrieval budget exhausted")

type KnowledgeIndex struct {
	ID            int64  `json:"id"`
	TenantID      string `json:"tenant_id"`
	Model         string `json:"model"`
	Dimension     int    `json:"dimension"`
	Preprocessing string `json:"preprocessing"`
	Epoch         int64  `json:"epoch"`
	Status        string `json:"status"`
	Active        bool   `json:"active"`
	Error         string `json:"error"`
}

func ValidateVectors(v [][]float64, n, dim int) error {
	if len(v) != n {
		return fmt.Errorf("embedding count: got %d want %d", len(v), n)
	}
	for _, a := range v {
		if len(a) != dim || dim <= 0 {
			return fmt.Errorf("invalid embedding dimension")
		}
		norm := 0.0
		for _, x := range a {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return fmt.Errorf("non-finite embedding")
			}
			norm += x * x
		}
		if norm == 0 || math.IsInf(norm, 0) {
			return fmt.Errorf("invalid embedding norm")
		}
	}
	return nil
}
func (d *DB) KnowledgeEpoch(ctx context.Context, tenant string) (int64, error) {
	var n int64
	err := d.QueryRowContext(ctx, `SELECT COALESCE((SELECT epoch FROM knowledge_catalogs WHERE tenant_id=$1),0)`, tenantOr(tenant)).Scan(&n)
	return n, err
}
func (d *DB) KnowledgeIndexes(ctx context.Context, tenant string) ([]KnowledgeIndex, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,tenant_id,model,dimension,preprocessing,epoch,status,active,error FROM knowledge_indexes WHERE tenant_id=$1 ORDER BY id DESC`, tenantOr(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KnowledgeIndex{}
	for rows.Next() {
		var x KnowledgeIndex
		if err := rows.Scan(&x.ID, &x.TenantID, &x.Model, &x.Dimension, &x.Preprocessing, &x.Epoch, &x.Status, &x.Active, &x.Error); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (d *DB) StartKnowledgeIndex(ctx context.Context, tenant, model, prep, actor string, dim int) (KnowledgeIndex, error) {
	x := KnowledgeIndex{TenantID: tenantOr(tenant), Model: model, Dimension: dim, Preprocessing: prep, Status: "building"}
	var err error
	x.Epoch, err = d.KnowledgeEpoch(ctx, tenant)
	if err != nil {
		return x, err
	}
	err = d.QueryRowContext(ctx, `INSERT INTO knowledge_indexes(tenant_id,model,dimension,preprocessing,epoch,status,created_by,updated_by) VALUES($1,$2,$3,$4,$5,'building',$6,$6) RETURNING id`, x.TenantID, model, dim, prep, x.Epoch, actor).Scan(&x.ID)
	return x, err
}
func ChunkFingerprint(doc *KnowledgeDocument, c *KnowledgeChunk) string {
	b, _ := json.Marshal([]any{doc.ContentHash, doc.Title, doc.Extra, doc.ParseMeta, doc.Category, c.SectionPath, c.Content, c.Tables, c.BBoxMeta, c.PageStart, c.PageEnd})
	return ContentHash(string(b))
}

type IndexChunk struct {
	Chunk    *KnowledgeChunk
	Document *KnowledgeDocument
}

func (d *DB) IndexChunks(ctx context.Context, tenant string, after int64, limit int) ([]IndexChunk, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+kchunkCols+`, `+kchunkBodyCols+`,d.title,d.content_hash,d.extra,d.parse_meta,d.category FROM knowledge_chunks c JOIN knowledge_documents d ON d.id=c.doc_id AND d.tenant_id=c.tenant_id WHERE c.tenant_id=$1 AND c.id>$2 ORDER BY c.id LIMIT $3`, tenantOr(tenant), after, min(max(limit, 1), 128))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IndexChunk{}
	for rows.Next() {
		doc := &KnowledgeDocument{}
		var extra, meta []byte
		c, err := scanKnowledgeChunk(rows, true, &doc.Title, &doc.ContentHash, &extra, &meta, &doc.Category)
		if err != nil {
			return nil, err
		}
		doc.ID = c.DocID
		doc.Extra = extra
		doc.ParseMeta = meta
		out = append(out, IndexChunk{c, doc})
	}
	return out, rows.Err()
}
func (d *DB) PutKnowledgeEmbeddings(ctx context.Context, x KnowledgeIndex, chunks []IndexChunk, v [][]float64) error {
	if err := ValidateVectors(v, len(chunks), x.Dimension); err != nil {
		return err
	}
	return d.InTx(ctx, func(tx *sql.Tx) error {
		for i, c := range chunks {
			res, err := tx.ExecContext(ctx, `INSERT INTO knowledge_embeddings(tenant_id,index_id,chunk_id,content_hash,embedding,created_by,updated_by)
 SELECT $1,$2,c.id,$4,$5::float8[],i.created_by,i.created_by FROM knowledge_chunks c JOIN knowledge_indexes i ON i.id=$2 AND i.tenant_id=$1
 WHERE c.id=$3 AND c.tenant_id=$1 AND i.status='building' ON CONFLICT(tenant_id,index_id,chunk_id) DO UPDATE SET content_hash=EXCLUDED.content_hash,embedding=EXCLUDED.embedding`, x.TenantID, x.ID, c.Chunk.ID, ChunkFingerprint(c.Document, c.Chunk), v[i])
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return ErrRetrievalConflict
			}
		}
		return nil
	})
}
func (d *DB) FinishKnowledgeIndex(ctx context.Context, x KnowledgeIndex, buildErr error) error {
	status, msg := "ready", ""
	if buildErr != nil {
		status, msg = "failed", buildErr.Error()
	}
	_, err := d.ExecContext(ctx, `UPDATE knowledge_indexes SET status=$3,error=$4 WHERE tenant_id=$1 AND id=$2 AND status='building'`, x.TenantID, x.ID, status, msg)
	return err
}

// Activation and rollback both require an intact snapshot. A partial or stale build cannot become active.
func (d *DB) ActivateKnowledgeIndex(ctx context.Context, tenant string, id int64, actor string) error {
	tenant = tenantOr(tenant)
	return d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO knowledge_catalogs(tenant_id) VALUES($1) ON CONFLICT DO NOTHING`, tenant); err != nil {
			return err
		}
		var epoch int64
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM knowledge_catalogs WHERE tenant_id=$1 FOR UPDATE`, tenant).Scan(&epoch); err != nil {
			return err
		}
		var ok bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_indexes i WHERE i.id=$2 AND i.tenant_id=$1 AND i.status='ready' AND i.epoch=$3
    AND NOT EXISTS(SELECT 1 FROM knowledge_chunks c WHERE c.tenant_id=$1 AND NOT EXISTS(SELECT 1 FROM knowledge_embeddings e WHERE e.tenant_id=$1 AND e.index_id=i.id AND e.chunk_id=c.id AND cardinality(e.embedding)=i.dimension)))`, tenant, id, epoch).Scan(&ok)
		if err != nil {
			return err
		}
		if !ok {
			return ErrRetrievalConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE knowledge_indexes SET active=false,updated_by=$2 WHERE tenant_id=$1 AND active`, tenant, actor); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE knowledge_indexes SET active=true,updated_by=$3 WHERE tenant_id=$1 AND id=$2`, tenant, id, actor)
		return err
	})
}

type RetrievalRun struct {
	ID             int64           `json:"id"`
	TenantID       string          `json:"tenant_id"`
	TaskID         *int64          `json:"task_id,omitempty"`
	RequestKey     string          `json:"request_key"`
	RequestHash    string          `json:"request_hash"`
	Request        json.RawMessage `json:"request"`
	Kind           string          `json:"kind"`
	Status         string          `json:"status"`
	Result         json.RawMessage `json:"result"`
	MaxCalls       int             `json:"max_calls"`
	MaxTokens      int             `json:"max_tokens"`
	UsedCalls      int             `json:"used_calls"`
	ReservedTokens int             `json:"reserved_tokens"`
	ActualTokens   int             `json:"actual_tokens"`
	Deadline       time.Time       `json:"deadline"`
}

const runCols = `id,tenant_id,task_id,request_key,request_hash,request,kind,status,result,max_calls,max_tokens,used_calls,reserved_tokens,actual_tokens,deadline`

func scanRun(r rowScanner) (*RetrievalRun, error) {
	var x RetrievalRun
	err := r.Scan(&x.ID, &x.TenantID, &x.TaskID, &x.RequestKey, &x.RequestHash, &x.Request, &x.Kind, &x.Status, &x.Result, &x.MaxCalls, &x.MaxTokens, &x.UsedCalls, &x.ReservedTokens, &x.ActualTokens, &x.Deadline)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return &x, err
}
func (d *DB) CreateRetrievalRun(ctx context.Context, x RetrievalRun, actor string) (*RetrievalRun, bool, error) {
	raw := jsonOr(x.Request, "{}")
	hash := ContentHash(raw)
	r, err := scanRun(d.QueryRowContext(ctx, `INSERT INTO retrieval_runs(tenant_id,task_id,request_key,request_hash,request,kind,max_calls,max_tokens,deadline,created_by,updated_by) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,$10,$10) ON CONFLICT(tenant_id,request_key) DO NOTHING RETURNING `+runCols, tenantOr(x.TenantID), x.TaskID, x.RequestKey, hash, raw, x.Kind, x.MaxCalls, x.MaxTokens, x.Deadline, actor))
	if !errors.Is(err, ErrNotFound) {
		return r, err == nil, err
	}
	r, err = scanRun(d.QueryRowContext(ctx, `SELECT `+runCols+` FROM retrieval_runs WHERE tenant_id=$1 AND request_key=$2`, tenantOr(x.TenantID), x.RequestKey))
	if err == nil && (r.RequestHash != hash || r.Kind != x.Kind || (r.TaskID == nil) != (x.TaskID == nil) || r.TaskID != nil && *r.TaskID != *x.TaskID) {
		err = ErrRetrievalConflict
	}
	return r, false, err
}
func (d *DB) GetRetrievalRun(ctx context.Context, tenant string, id int64) (*RetrievalRun, error) {
	return scanRun(d.QueryRowContext(ctx, `SELECT `+runCols+` FROM retrieval_runs WHERE tenant_id=$1 AND id=$2`, tenantOr(tenant), id))
}
func (d *DB) TaskRetrievalRuns(ctx context.Context, tenant string, taskID int64) ([]*RetrievalRun, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+runCols+` FROM retrieval_runs WHERE tenant_id=$1 AND task_id=$2 ORDER BY id DESC LIMIT 100`, tenantOr(tenant), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RetrievalRun{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}
func (d *DB) ReserveRetrieval(ctx context.Context, tenant string, id int64, calls, tokens int) error {
	if calls < 0 || tokens < 0 {
		return ErrRetrievalBudget
	}
	res, err := d.ExecContext(ctx, `UPDATE retrieval_runs SET used_calls=used_calls+$3,reserved_tokens=reserved_tokens+$4 WHERE tenant_id=$1 AND id=$2 AND status='running' AND deadline>now() AND used_calls+$3<=max_calls AND reserved_tokens+$4<=max_tokens`, tenantOr(tenant), id, calls, tokens)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrRetrievalBudget
	}
	return nil
}

// RetrievalTokensLeft 返回运行尚未预留的 token 额度；运行已结束或超时时为 0。
func (d *DB) RetrievalTokensLeft(ctx context.Context, tenant string, id int64) (int, error) {
	var left int
	err := d.QueryRowContext(ctx, `SELECT GREATEST(max_tokens-reserved_tokens,0) FROM retrieval_runs WHERE tenant_id=$1 AND id=$2 AND status='running' AND deadline>now()`, tenantOr(tenant), id).Scan(&left)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return left, err
}
func (d *DB) RecordRetrievalUsage(ctx context.Context, tenant string, id int64, tokens int) error {
	_, err := d.ExecContext(ctx, `UPDATE retrieval_runs SET actual_tokens=actual_tokens+$3 WHERE tenant_id=$1 AND id=$2`, tenantOr(tenant), id, max(tokens, 0))
	return err
}
func (d *DB) FinishRetrievalRun(ctx context.Context, tenant string, id int64, status string, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	res, err := d.ExecContext(ctx, `UPDATE retrieval_runs SET status=$3,result=$4::jsonb,finished_at=now() WHERE tenant_id=$1 AND id=$2 AND status='running'
 AND NOT EXISTS(SELECT 1 FROM retrieval_jobs j WHERE j.tenant_id=$1 AND j.run_id=$2 AND j.status IN ('pending','running'))`, tenantOr(tenant), id, status, raw)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrRetrievalConflict
	}
	return nil
}

type RetrievalEvidence struct {
	ID       int64           `json:"id"`
	RunID    int64           `json:"run_id"`
	Domain   string          `json:"source_domain"`
	SourceID int64           `json:"source_id"`
	DocID    int64           `json:"doc_id"`
	Hash     string          `json:"content_hash"`
	Snapshot json.RawMessage `json:"snapshot"`
	Claim    json.RawMessage `json:"claim"`
	Relation string          `json:"relation"`
	Status   string          `json:"status"`
	Reason   string          `json:"reason"`
}

func (d *DB) SaveRetrievalEvidence(ctx context.Context, tenant, actor string, e *RetrievalEvidence) error {
	return d.QueryRowContext(ctx, `INSERT INTO retrieval_evidence(tenant_id,run_id,source_domain,source_id,doc_id,content_hash,snapshot,claim,relation,status,reason,created_by,updated_by)
 SELECT $1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9,$10,$11,$12,$12 WHERE EXISTS(SELECT 1 FROM retrieval_runs WHERE tenant_id=$1 AND id=$2)
 ON CONFLICT(tenant_id,run_id,source_domain,source_id) DO UPDATE SET
 claim=CASE WHEN EXCLUDED.claim='{}'::jsonb THEN retrieval_evidence.claim ELSE EXCLUDED.claim END,
 relation=CASE WHEN EXCLUDED.claim='{}'::jsonb THEN retrieval_evidence.relation ELSE EXCLUDED.relation END,
 status=CASE WHEN retrieval_evidence.status='stale' OR retrieval_evidence.content_hash<>EXCLUDED.content_hash THEN 'stale' WHEN EXCLUDED.claim='{}'::jsonb THEN retrieval_evidence.status ELSE EXCLUDED.status END,
 reason=CASE WHEN retrieval_evidence.status='stale' OR retrieval_evidence.content_hash<>EXCLUDED.content_hash THEN 'source changed or deleted' WHEN EXCLUDED.claim='{}'::jsonb THEN retrieval_evidence.reason ELSE EXCLUDED.reason END,updated_by=EXCLUDED.updated_by
 RETURNING id,status,relation,reason,claim,snapshot,content_hash`, tenantOr(tenant), e.RunID, e.Domain, e.SourceID, e.DocID, e.Hash, jsonOr(e.Snapshot, "{}"), jsonOr(e.Claim, "{}"), e.Relation, e.Status, e.Reason, actor).Scan(&e.ID, &e.Status, &e.Relation, &e.Reason, &e.Claim, &e.Snapshot, &e.Hash)
}
func (d *DB) RetrievalEvidence(ctx context.Context, tenant string, runID int64) ([]RetrievalEvidence, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,run_id,source_domain,source_id,doc_id,content_hash,snapshot,claim,relation,status,reason FROM retrieval_evidence WHERE tenant_id=$1 AND run_id=$2 ORDER BY id`, tenantOr(tenant), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RetrievalEvidence{}
	for rows.Next() {
		var e RetrievalEvidence
		if err := rows.Scan(&e.ID, &e.RunID, &e.Domain, &e.SourceID, &e.DocID, &e.Hash, &e.Snapshot, &e.Claim, &e.Relation, &e.Status, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
