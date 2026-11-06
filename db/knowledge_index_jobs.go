package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var ErrIndexBuildActive = fmt.Errorf("%w: knowledge index build already active", ErrRetrievalConflict)
var ErrIndexNothingToBuild = errors.New("knowledge base has no chunks to index")

type IndexActivationError struct{ Code string }

func (e *IndexActivationError) Error() string { return "knowledge index activation: " + e.Code }
func (e *IndexActivationError) Unwrap() error { return ErrRetrievalConflict }

type KnowledgeIndex struct {
	ID             int64      `json:"id"`
	TenantID       string     `json:"tenant_id"`
	Model          string     `json:"model"`
	Dimension      int        `json:"dimension"`
	Preprocessing  string     `json:"preprocessing"`
	Epoch          int64      `json:"epoch"`
	Status         string     `json:"status"`
	Active         bool       `json:"active"`
	Error          string     `json:"error"`
	ErrorCode      string     `json:"error_code"`
	RequestKey     string     `json:"request_key"`
	RequestHash    string     `json:"-"`
	TotalChunks    int        `json:"total_chunks"`
	EmbeddedChunks int        `json:"embedded_chunks"`
	ReusedChunks   int        `json:"reused_chunks"`
	ReusedFrom     *int64     `json:"reused_from,omitempty"`
	CursorChunkID  int64      `json:"-"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	DeadlineAt     *time.Time `json:"deadline_at,omitempty"`
	LeaseToken     int64      `json:"-"`
	LeaseOwner     string     `json:"-"`
	LeaseUntil     *time.Time `json:"-"`
	RetryAfter     *time.Time `json:"retry_after,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	ActivatedAt    *time.Time `json:"activated_at,omitempty"`
	ActivatedBy    string     `json:"activated_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CreatedBy      string     `json:"created_by"`
	UpdatedBy      string     `json:"updated_by"`
	ApprovedBy     string     `json:"approved_by"`
}

const knowledgeIndexCols = `id,tenant_id,model,dimension,preprocessing,epoch,status,active,error,error_code,
 request_key,request_hash,total_chunks,embedded_chunks,reused_chunks,reused_from,cursor_chunk_id,
 attempts,max_attempts,deadline_at,lease_token,lease_owner,lease_until,retry_after,started_at,finished_at,
 activated_at,activated_by,created_at,updated_at,created_by,updated_by,approved_by`

func scanKnowledgeIndex(r rowScanner) (KnowledgeIndex, error) {
	var x KnowledgeIndex
	err := r.Scan(&x.ID, &x.TenantID, &x.Model, &x.Dimension, &x.Preprocessing, &x.Epoch, &x.Status, &x.Active, &x.Error, &x.ErrorCode,
		&x.RequestKey, &x.RequestHash, &x.TotalChunks, &x.EmbeddedChunks, &x.ReusedChunks, &x.ReusedFrom, &x.CursorChunkID,
		&x.Attempts, &x.MaxAttempts, &x.DeadlineAt, &x.LeaseToken, &x.LeaseOwner, &x.LeaseUntil, &x.RetryAfter, &x.StartedAt, &x.FinishedAt,
		&x.ActivatedAt, &x.ActivatedBy, &x.CreatedAt, &x.UpdatedAt, &x.CreatedBy, &x.UpdatedBy, &x.ApprovedBy)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return x, err
}

func (d *DB) KnowledgeIndexes(ctx context.Context, tenant string) ([]KnowledgeIndex, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+knowledgeIndexCols+` FROM knowledge_indexes WHERE tenant_id=$1 ORDER BY id DESC`, tenantOr(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KnowledgeIndex{}
	for rows.Next() {
		x, e := scanKnowledgeIndex(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (d *DB) GetKnowledgeIndex(ctx context.Context, tenant string, id int64) (KnowledgeIndex, error) {
	return scanKnowledgeIndex(d.QueryRowContext(ctx, `SELECT `+knowledgeIndexCols+` FROM knowledge_indexes WHERE tenant_id=$1 AND id=$2`, tenantOr(tenant), id))
}

type KnowledgeIndexRequest struct {
	TenantID, Model, Preprocessing, Actor, RequestKey, RequestHash string
	Dimension                                                      int
	Deadline                                                       time.Time
	MaxAttempts                                                    int
}

func (d *DB) KnowledgeChunkCount(ctx context.Context, tenant string) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_chunks WHERE tenant_id=$1`, tenantOr(tenant)).Scan(&n)
	return n, err
}

func (d *DB) lockKnowledgeCatalog(ctx context.Context, tenant string) (int64, error) {
	if _, err := d.ExecContext(ctx, `INSERT INTO knowledge_catalogs(tenant_id) VALUES($1) ON CONFLICT DO NOTHING`, tenant); err != nil {
		return 0, err
	}
	var epoch int64
	err := d.QueryRowContext(ctx, `SELECT epoch FROM knowledge_catalogs WHERE tenant_id=$1 FOR UPDATE`, tenant).Scan(&epoch)
	return epoch, err
}

func (d *DB) indexEvent(ctx context.Context, x KnowledgeIndex, event, actor string, from *int64, detail any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, `INSERT INTO knowledge_index_events(tenant_id,index_id,event,from_index_id,model,dimension,detail,created_by,updated_by)
 VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$8)`, x.TenantID, x.ID, event, from, x.Model, x.Dimension, raw, actor)
	return err
}

func (d *DB) SubmitKnowledgeIndexBuild(ctx context.Context, in KnowledgeIndexRequest) (KnowledgeIndex, bool, error) {
	in.TenantID = tenantOr(in.TenantID)
	if in.Model == "" || in.Preprocessing == "" || in.Dimension < 1 || in.Dimension > 8192 || in.MaxAttempts < 0 {
		return KnowledgeIndex{}, false, ErrInvalidKnowledge
	}
	if in.MaxAttempts == 0 {
		in.MaxAttempts = 3
	}
	if in.Deadline.IsZero() {
		in.Deadline = time.Now().Add(2 * time.Hour)
	}
	var x KnowledgeIndex
	replayed := false
	err := d.Transaction(ctx, func(tx *DB) error {
		epoch, err := tx.lockKnowledgeCatalog(ctx, in.TenantID)
		if err != nil {
			return err
		}
		if in.RequestKey != "" {
			x, err = scanKnowledgeIndex(tx.QueryRowContext(ctx, `SELECT `+knowledgeIndexCols+` FROM knowledge_indexes WHERE tenant_id=$1 AND request_key=$2`, in.TenantID, in.RequestKey))
			if err == nil {
				if x.RequestHash != in.RequestHash {
					return ErrIdempotencyKeyConflict
				}
				replayed = true
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		x, err = scanKnowledgeIndex(tx.QueryRowContext(ctx, `SELECT `+knowledgeIndexCols+` FROM knowledge_indexes WHERE tenant_id=$1 AND status IN ('queued','building')`, in.TenantID))
		if err == nil {
			return ErrIndexBuildActive
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		n, err := tx.KnowledgeChunkCount(ctx, in.TenantID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrIndexNothingToBuild
		}
		x, err = scanKnowledgeIndex(tx.QueryRowContext(ctx, `INSERT INTO knowledge_indexes
 (tenant_id,model,dimension,preprocessing,epoch,status,request_key,request_hash,total_chunks,reused_from,deadline_at,max_attempts,created_by,updated_by)
 VALUES($1,$2,$3,$4,$5,'queued',$6,$7,$8,
 (SELECT id FROM knowledge_indexes WHERE tenant_id=$1 AND model=$2 AND dimension=$3 AND preprocessing=$4 AND status='ready' ORDER BY id DESC LIMIT 1),$9,$10,$11,$11)
 RETURNING `+knowledgeIndexCols, in.TenantID, in.Model, in.Dimension, in.Preprocessing, epoch, in.RequestKey, in.RequestHash, n, in.Deadline, in.MaxAttempts, in.Actor))
		if err != nil {
			return err
		}
		return tx.indexEvent(ctx, x, "build_requested", in.Actor, x.ReusedFrom, map[string]any{})
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		// Also handle writers that did not acquire the catalog lock. Never expose SQLSTATE to the API.
		if in.RequestKey != "" {
			existing, e := scanKnowledgeIndex(d.QueryRowContext(ctx, `SELECT `+knowledgeIndexCols+` FROM knowledge_indexes WHERE tenant_id=$1 AND request_key=$2`, in.TenantID, in.RequestKey))
			if e == nil {
				if existing.RequestHash != in.RequestHash {
					return existing, false, ErrIdempotencyKeyConflict
				}
				return existing, true, nil
			}
		}
		return x, false, ErrIndexBuildActive
	}
	return x, replayed, err
}

func (d *DB) ClaimKnowledgeIndexBuild(ctx context.Context, tenant, owner string, lease time.Duration) (*KnowledgeIndex, error) {
	return d.claimKnowledgeIndex(ctx, tenant, 0, owner, lease)
}

func (d *DB) ClaimKnowledgeIndexBuildByID(ctx context.Context, tenant string, id int64, owner string, lease time.Duration) (*KnowledgeIndex, error) {
	return d.claimKnowledgeIndex(ctx, tenantOr(tenant), id, owner, lease)
}

func (d *DB) claimKnowledgeIndex(ctx context.Context, tenant string, id int64, owner string, lease time.Duration) (*KnowledgeIndex, error) {
	if lease <= 0 {
		lease = time.Minute
	}
	var claimed *KnowledgeIndex
	err := d.Transaction(ctx, func(tx *DB) error {
		for {
			x, err := scanKnowledgeIndex(tx.QueryRowContext(ctx, `SELECT `+knowledgeIndexCols+` FROM knowledge_indexes
 WHERE ($1::text='' OR tenant_id=$1) AND ($2::bigint=0 OR id=$2) AND
 ((status='queued' AND (retry_after IS NULL OR retry_after<=now())) OR (status='building' AND lease_until<now()))
 ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1`, tenant, id))
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if x.DeadlineAt != nil && !x.DeadlineAt.After(time.Now()) || x.Attempts >= x.MaxAttempts {
				if _, err = tx.ExecContext(ctx, `UPDATE knowledge_indexes SET status='failed',error_code='BUDGET_EXHAUSTED',
 finished_at=now(),lease_token=lease_token+1,lease_until=NULL,lease_owner='' WHERE tenant_id=$1 AND id=$2`, x.TenantID, x.ID); err != nil {
					return err
				}
				if err = tx.indexEvent(ctx, x, "build_failed", owner, nil, map[string]any{"error_code": "BUDGET_EXHAUSTED"}); err != nil {
					return err
				}
				continue
			}
			epoch, err := tx.KnowledgeEpoch(ctx, x.TenantID)
			if err != nil {
				return err
			}
			if epoch != x.Epoch {
				x.Epoch = epoch
				x.TotalChunks, err = tx.KnowledgeChunkCount(ctx, x.TenantID)
				if err != nil {
					return err
				}
				x.CursorChunkID, x.EmbeddedChunks, x.ReusedChunks = 0, 0, 0
				if _, err = tx.ExecContext(ctx, `DELETE FROM knowledge_embeddings WHERE tenant_id=$1 AND index_id=$2`, x.TenantID, x.ID); err != nil {
					return err
				}
			}
			x, err = scanKnowledgeIndex(tx.QueryRowContext(ctx, `UPDATE knowledge_indexes SET status='building',attempts=attempts+1,
 lease_token=lease_token+1,lease_owner=$3,lease_until=now()+$4::double precision*interval '1 second',retry_after=NULL,
 started_at=COALESCE(started_at,now()),epoch=$5,total_chunks=$6,cursor_chunk_id=$7,embedded_chunks=$8,reused_chunks=$9
 WHERE tenant_id=$1 AND id=$2 RETURNING `+knowledgeIndexCols, x.TenantID, x.ID, owner, lease.Seconds(), x.Epoch, x.TotalChunks, x.CursorChunkID, x.EmbeddedChunks, x.ReusedChunks))
			if err != nil {
				return err
			}
			claimed = &x
			return nil
		}
	})
	return claimed, err
}

func indexLeaseResult(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrLeaseLost
	}
	return err
}

func (d *DB) HeartbeatKnowledgeIndexBuild(ctx context.Context, x KnowledgeIndex, lease time.Duration) error {
	return indexLeaseResult(d.ExecContext(ctx, `UPDATE knowledge_indexes SET lease_until=now()+$4::double precision*interval '1 second'
 WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND status='building' AND lease_until>now()`, x.TenantID, x.ID, x.LeaseToken, lease.Seconds()))
}

type ReusableEmbedding struct {
	ContentHash string
	Vector      []float64
}

func (d *DB) ReusableEmbeddings(ctx context.Context, x KnowledgeIndex, fingerprints map[int64]string) (map[int64]ReusableEmbedding, error) {
	out := map[int64]ReusableEmbedding{}
	if len(fingerprints) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(fingerprints))
	hashes := make([]string, 0, len(fingerprints))
	for id, hash := range fingerprints {
		ids = append(ids, id)
		hashes = append(hashes, hash)
	}
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT ON (e.chunk_id) e.chunk_id,e.content_hash,to_json(e.embedding) FROM knowledge_embeddings e
 JOIN knowledge_indexes i ON i.tenant_id=e.tenant_id AND i.id=e.index_id
	JOIN unnest($3::bigint[],$4::text[]) AS wanted(chunk_id,content_hash) ON wanted.chunk_id=e.chunk_id AND wanted.content_hash=e.content_hash
	WHERE e.tenant_id=$1 AND e.index_id<>$2 AND i.status IN ('ready','failed','cancelled')
	AND i.model=$5 AND i.dimension=$6 AND i.preprocessing=$7
	ORDER BY e.chunk_id,i.id DESC`, x.TenantID, x.ID, ids, hashes, x.Model, x.Dimension, x.Preprocessing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var v ReusableEmbedding
		var raw []byte
		if err = rows.Scan(&id, &v.ContentHash, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &v.Vector); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

func (d *DB) PutIndexBatch(ctx context.Context, x KnowledgeIndex, chunks []IndexChunk, vectors [][]float64, embedded, reused int) error {
	return d.Transaction(ctx, func(tx *DB) error {
		var cursor int64
		err := tx.QueryRowContext(ctx, `SELECT cursor_chunk_id FROM knowledge_indexes WHERE tenant_id=$1 AND id=$2
 AND lease_token=$3 AND status='building' AND lease_until>now() FOR UPDATE`, x.TenantID, x.ID, x.LeaseToken).Scan(&cursor)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if err = ValidateVectors(vectors, len(chunks), x.Dimension); err != nil {
			return err
		}
		if embedded < 0 || reused < 0 || embedded+reused != len(chunks) {
			return ErrInvalidKnowledge
		}
		for i, c := range chunks {
			if c.Chunk == nil || c.Document == nil || c.Chunk.ID <= cursor {
				return ErrRetrievalConflict
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO knowledge_embeddings(tenant_id,index_id,chunk_id,content_hash,embedding,created_by,updated_by)
 SELECT $1,$2,c.id,$4,$5::float8[],$6,$6 FROM knowledge_chunks c WHERE c.tenant_id=$1 AND c.id=$3 FOR KEY SHARE OF c
 ON CONFLICT(tenant_id,index_id,chunk_id) DO UPDATE SET content_hash=EXCLUDED.content_hash,embedding=EXCLUDED.embedding`, x.TenantID, x.ID, c.Chunk.ID, ChunkFingerprint(c.Document, c.Chunk), vectors[i], x.CreatedBy)
			if err != nil {
				return err
			}
			written, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if written != 1 {
				// A source chunk can be deleted while the model works outside the
				// transaction. That changes the catalog, not ownership of this job.
				// Finish the stale snapshot; activation will reject its old epoch.
				epoch, err := tx.KnowledgeEpoch(ctx, x.TenantID)
				if err != nil {
					return err
				}
				if epoch == x.Epoch {
					return ErrRetrievalConflict
				}
			}
			cursor = c.Chunk.ID
		}
		_, err = tx.ExecContext(ctx, `UPDATE knowledge_indexes SET embedded_chunks=embedded_chunks+$3,reused_chunks=reused_chunks+$4,cursor_chunk_id=$5
 WHERE tenant_id=$1 AND id=$2`, x.TenantID, x.ID, embedded, reused, cursor)
		return err
	})
}

func (d *DB) FinishKnowledgeIndexBuild(ctx context.Context, x KnowledgeIndex, buildErr error, code string) error {
	status, event, msg := "ready", "build_ready", ""
	if buildErr != nil {
		status, event, msg = "failed", "build_failed", buildErr.Error()
	}
	return d.Transaction(ctx, func(tx *DB) error {
		err := indexLeaseResult(tx.ExecContext(ctx, `UPDATE knowledge_indexes SET status=$4,error=$5,error_code=$6,finished_at=now(),
 lease_token=lease_token+1,lease_owner='',lease_until=NULL WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND status='building' AND lease_until>now()`, x.TenantID, x.ID, x.LeaseToken, status, msg, code))
		if err != nil {
			return err
		}
		return tx.indexEvent(ctx, x, event, x.CreatedBy, nil, map[string]any{"error_code": code})
	})
}

func (d *DB) ReleaseKnowledgeIndexBuild(ctx context.Context, x KnowledgeIndex, retry time.Time, reason string) error {
	return indexLeaseResult(d.ExecContext(ctx, `UPDATE knowledge_indexes SET status='queued',retry_after=$4,error=$5,
 lease_token=lease_token+1,lease_owner='',lease_until=NULL WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND status='building' AND lease_until>now()`, x.TenantID, x.ID, x.LeaseToken, retry, reason))
}

func (d *DB) CancelKnowledgeIndexBuild(ctx context.Context, tenant string, id int64, actor string) error {
	return d.Transaction(ctx, func(tx *DB) error {
		x, err := scanKnowledgeIndex(tx.QueryRowContext(ctx, `SELECT `+knowledgeIndexCols+` FROM knowledge_indexes WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantOr(tenant), id))
		if err != nil {
			return err
		}
		if x.Status != "queued" && x.Status != "building" {
			return ErrRetrievalConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE knowledge_indexes SET status='cancelled',error_code='CANCELLED',finished_at=now(),
 lease_token=lease_token+1,lease_until=NULL,lease_owner='',updated_by=$3 WHERE tenant_id=$1 AND id=$2`, x.TenantID, id, actor); err != nil {
			return err
		}
		return tx.indexEvent(ctx, x, "build_cancelled", actor, nil, map[string]any{})
	})
}

func (d *DB) DeleteKnowledgeIndex(ctx context.Context, tenant string, id int64, actor string) error {
	return d.Transaction(ctx, func(tx *DB) error {
		x, err := scanKnowledgeIndex(tx.QueryRowContext(ctx, `SELECT `+knowledgeIndexCols+` FROM knowledge_indexes WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantOr(tenant), id))
		if err != nil {
			return err
		}
		if x.Active || x.Status == "queued" || x.Status == "building" {
			return ErrRetrievalConflict
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM knowledge_indexes WHERE tenant_id=$1 AND id=$2`, x.TenantID, id); err != nil {
			return err
		}
		return tx.indexEvent(ctx, x, "deleted", actor, nil, map[string]any{})
	})
}

// Catalog locking serializes activation, rollback, and new submissions.
func (d *DB) ActivateKnowledgeIndex(ctx context.Context, tenant string, id int64, actor string) error {
	tenant = tenantOr(tenant)
	return d.Transaction(ctx, func(tx *DB) error {
		epoch, err := tx.lockKnowledgeCatalog(ctx, tenant)
		if err != nil {
			return err
		}
		x, err := scanKnowledgeIndex(tx.QueryRowContext(ctx, `SELECT `+knowledgeIndexCols+` FROM knowledge_indexes WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, id))
		if errors.Is(err, ErrNotFound) {
			return &IndexActivationError{Code: "not_ready"}
		}
		if err != nil {
			return err
		}
		if x.Active {
			return nil
		}
		if x.Status != "ready" {
			return &IndexActivationError{Code: "not_ready"}
		}
		if x.Epoch != epoch {
			return &IndexActivationError{Code: "documents_changed"}
		}
		var covered bool
		if err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM knowledge_chunks c WHERE c.tenant_id=$1
 AND NOT EXISTS(SELECT 1 FROM knowledge_embeddings e WHERE e.tenant_id=$1 AND e.index_id=$2 AND e.chunk_id=c.id AND cardinality(e.embedding)=$3))`, tenant, id, x.Dimension).Scan(&covered); err != nil {
			return err
		}
		if !covered {
			return &IndexActivationError{Code: "coverage_incomplete"}
		}
		var old *int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM knowledge_indexes WHERE tenant_id=$1 AND active`, tenant).Scan(&old); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE knowledge_indexes SET active=false,updated_by=$2 WHERE tenant_id=$1 AND active`, tenant, actor); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE knowledge_indexes SET active=true,activated_at=now(),activated_by=$3,updated_by=$3 WHERE tenant_id=$1 AND id=$2`, tenant, id, actor); err != nil {
			return err
		}
		event := "activated"
		if old != nil && id < *old {
			event = "rolled_back"
		}
		return tx.indexEvent(ctx, x, event, actor, old, map[string]any{})
	})
}

type KnowledgeIndexEvent struct {
	ID          int64           `json:"id"`
	TenantID    string          `json:"tenant_id"`
	IndexID     int64           `json:"index_id"`
	Event       string          `json:"event"`
	FromIndexID *int64          `json:"from_index_id,omitempty"`
	Model       string          `json:"model"`
	Dimension   int             `json:"dimension"`
	Detail      json.RawMessage `json:"detail"`
	CreatedAt   time.Time       `json:"created_at"`
	CreatedBy   string          `json:"created_by"`
	UpdatedBy   string          `json:"updated_by"`
	ApprovedBy  string          `json:"approved_by"`
}

func (d *DB) KnowledgeIndexEvents(ctx context.Context, tenant string, limit int) ([]KnowledgeIndexEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.QueryContext(ctx, `SELECT id,tenant_id,index_id,event,from_index_id,model,dimension,detail,created_at,created_by,updated_by,approved_by
 FROM knowledge_index_events WHERE tenant_id=$1 ORDER BY id DESC LIMIT $2`, tenantOr(tenant), min(limit, 200))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KnowledgeIndexEvent{}
	for rows.Next() {
		var e KnowledgeIndexEvent
		if err = rows.Scan(&e.ID, &e.TenantID, &e.IndexID, &e.Event, &e.FromIndexID, &e.Model, &e.Dimension, &e.Detail, &e.CreatedAt, &e.CreatedBy, &e.UpdatedBy, &e.ApprovedBy); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) NoteConfiguredModels(ctx context.Context, tenant string, detail map[string]any, actor string) error {
	return d.Transaction(ctx, func(tx *DB) error {
		tenant = tenantOr(tenant)
		if _, err := tx.lockKnowledgeCatalog(ctx, tenant); err != nil {
			return err
		}
		raw, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO knowledge_index_events(tenant_id,event,detail,created_by,updated_by)
 SELECT $1,'model_configured',$2::jsonb,$3,$3 WHERE NOT EXISTS(SELECT 1 FROM
 (SELECT detail FROM knowledge_index_events WHERE tenant_id=$1 AND event='model_configured' ORDER BY id DESC LIMIT 1) latest WHERE detail=$2::jsonb)`, tenant, raw, actor)
		return err
	})
}

// Compatibility entry points use the same durable queue and fencing as the runner.
func (d *DB) StartKnowledgeIndex(ctx context.Context, tenant, model, prep, actor string, dim int) (KnowledgeIndex, error) {
	x, _, err := d.SubmitKnowledgeIndexBuild(ctx, KnowledgeIndexRequest{TenantID: tenant, Model: model, Preprocessing: prep, Actor: actor, Dimension: dim})
	if err != nil {
		return x, err
	}
	claimed, err := d.ClaimKnowledgeIndexBuildByID(ctx, x.TenantID, x.ID, actor, time.Minute)
	if err == nil && claimed == nil {
		err = ErrLeaseLost
	}
	if claimed != nil {
		x = *claimed
	}
	return x, err
}

func (d *DB) PutKnowledgeEmbeddings(ctx context.Context, x KnowledgeIndex, chunks []IndexChunk, vectors [][]float64) error {
	return d.PutIndexBatch(ctx, x, chunks, vectors, len(chunks), 0)
}

func (d *DB) FinishKnowledgeIndex(ctx context.Context, x KnowledgeIndex, err error) error {
	return d.FinishKnowledgeIndexBuild(ctx, x, err, "")
}
