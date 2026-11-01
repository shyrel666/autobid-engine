package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type FixedFileJob struct {
	ID          int64           `json:"id"`
	TaskID      int64           `json:"task_id"`
	TenantID    string          `json:"-"`
	FileID      int64           `json:"file_id"`
	SourceHash  string          `json:"source_hash"`
	RequestKey  string          `json:"request_key"`
	Fingerprint string          `json:"-"`
	Input       json.RawMessage `json:"input"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	DeadlineAt  time.Time       `json:"deadline_at"`
	LeaseToken  int64           `json:"-"`
	RecordID    *int64          `json:"record_id"`
	Error       string          `json:"error,omitempty"`
	CreatedBy   string          `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
}

const fixedJobCols = `id,task_id,tenant_id,file_id,source_hash,request_key,fingerprint,input,status,attempts,max_attempts,deadline_at,lease_token,record_id,error,created_by,created_at`

func scanFixedJob(row rowScanner) (*FixedFileJob, error) {
	j := new(FixedFileJob)
	err := row.Scan(&j.ID, &j.TaskID, &j.TenantID, &j.FileID, &j.SourceHash, &j.RequestKey, &j.Fingerprint, &j.Input, &j.Status, &j.Attempts, &j.MaxAttempts, &j.DeadlineAt, &j.LeaseToken, &j.RecordID, &j.Error, &j.CreatedBy, &j.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}
func (d *DB) GetFixedFileJob(ctx context.Context, tenant string, taskID, id int64) (*FixedFileJob, error) {
	return scanFixedJob(d.QueryRowContext(ctx, `SELECT `+fixedJobCols+` FROM fixed_file_jobs WHERE tenant_id=$1 AND task_id=$2 AND id=$3`, tenantOr(tenant), taskID, id))
}
func (d *DB) FindFixedFileJob(ctx context.Context, tenant string, taskID int64, key string) (*FixedFileJob, error) {
	return scanFixedJob(d.QueryRowContext(ctx, `SELECT `+fixedJobCols+` FROM fixed_file_jobs WHERE tenant_id=$1 AND task_id=$2 AND request_key=$3`, tenantOr(tenant), taskID, key))
}
func (d *DB) ListFixedFileJobs(ctx context.Context, tenant string, taskID, before int64) ([]*FixedFileJob, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+fixedJobCols+` FROM fixed_file_jobs WHERE tenant_id=$1 AND task_id=$2 AND ($3::bigint=0 OR id<$3) ORDER BY id DESC LIMIT 50`, tenantOr(tenant), taskID, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*FixedFileJob{}
	for rows.Next() {
		j, err := scanFixedJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (d *DB) EnqueueFixedFileJob(ctx context.Context, j FixedFileJob) (*FixedFileJob, error) {
	var out *FixedFileJob
	err := d.Transaction(ctx, func(tx *DB) error {
		if err := tx.LockFileProcessingTask(ctx, j.TenantID, j.TaskID); err != nil {
			return err
		}
		existing, err := tx.FindFixedFileJob(ctx, j.TenantID, j.TaskID, j.RequestKey)
		if err == nil {
			if existing.Fingerprint != j.Fingerprint {
				return ErrFileProcessingConflict
			}
			out = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT paused FROM tasks WHERE id=$1`, j.TaskID).Scan(&paused); err != nil {
			return err
		}
		if paused {
			return fmt.Errorf("%w: task is paused", ErrFileProcessingConflict)
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM fixed_file_jobs WHERE tenant_id=$1 AND task_id=$2 AND status IN ('queued','running')`, tenantOr(j.TenantID), j.TaskID).Scan(&active); err != nil {
			return err
		}
		if active >= 100 {
			return fmt.Errorf("%w: at most 100 pending file jobs per task", ErrFileProcessingConflict)
		}
		f, _, err := tx.GetTaskFileObject(ctx, j.TenantID, j.TaskID, j.FileID)
		if err != nil {
			return err
		}
		if f.Availability != "available" || f.SHA256 != j.SourceHash {
			return ErrInvalidParseRevision
		}
		out, err = scanFixedJob(tx.QueryRowContext(ctx, `INSERT INTO fixed_file_jobs(tenant_id,task_id,file_id,source_hash,request_key,fingerprint,input,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$8) RETURNING `+fixedJobCols, tenantOr(j.TenantID), j.TaskID, j.FileID, j.SourceHash, j.RequestKey, j.Fingerprint, string(j.Input), j.CreatedBy))
		return err
	})
	return out, err
}

// Claim also reclaims expired leases. Attempts and the original deadline never reset.
func (d *DB) ClaimFixedFileJob(ctx context.Context, owner string, lease time.Duration, taskID int64) (*FixedFileJob, error) {
	var id, tid int64
	var tenant string
	err := d.QueryRowContext(ctx, `SELECT j.id,j.task_id,j.tenant_id FROM fixed_file_jobs j JOIN tasks t ON t.id=j.task_id WHERE NOT t.paused AND ($1::bigint=0 OR j.task_id=$1) AND (j.status='queued' OR (j.status='running' AND j.lease_until<now())) ORDER BY j.id LIMIT 1`, taskID).Scan(&id, &tid, &tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out *FixedFileJob
	err = d.Transaction(ctx, func(tx *DB) error {
		if err := tx.LockFileProcessingTask(ctx, tenant, tid); err != nil {
			return err
		}
		// Cancel/pause and completion use task -> job order, as does export.
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT paused FROM tasks WHERE id=$1`, tid).Scan(&paused); err != nil {
			return err
		}
		if paused {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE fixed_file_jobs SET status='failed',error='执行次数或截止时间已耗尽',lease_token=lease_token+1,lease_until=NULL,updated_at=now(),updated_by=$2 WHERE id=$1 AND (status='queued' OR (status='running' AND lease_until<now())) AND (attempts>=max_attempts OR deadline_at<=now())`, id, owner)
		if err != nil {
			return err
		}
		out, err = scanFixedJob(tx.QueryRowContext(ctx, `UPDATE fixed_file_jobs SET status='running',attempts=attempts+1,lease_token=lease_token+1,lease_until=LEAST(deadline_at,now()+$2*interval '1 second'),lease_owner=$3,updated_by=$3,updated_at=now() WHERE id=$1 AND (status='queued' OR (status='running' AND lease_until<now())) AND attempts<max_attempts AND deadline_at>now() RETURNING `+fixedJobCols, id, lease.Seconds(), owner))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	return out, err
}
func (d *DB) HeartbeatFixedFileJob(ctx context.Context, j *FixedFileJob, lease time.Duration) error {
	res, err := d.ExecContext(ctx, `UPDATE fixed_file_jobs SET lease_until=LEAST(deadline_at,now()+$3*interval '1 second') WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>now() AND deadline_at>now()`, j.ID, j.LeaseToken, lease.Seconds())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Guard must run inside the same transaction as all output writes.
func (d *DB) GuardFixedFileJob(ctx context.Context, j *FixedFileJob) error {
	var id int64
	err := d.QueryRowContext(ctx, `SELECT id FROM fixed_file_jobs WHERE id=$1 AND tenant_id=$2 AND task_id=$3 AND lease_token=$4 AND status='running' AND lease_until>now() AND deadline_at>now() FOR UPDATE`, j.ID, j.TenantID, j.TaskID, j.LeaseToken).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}
func (d *DB) FinishFixedFileJob(ctx context.Context, j *FixedFileJob, record *FileProcessingRecord, detail string) error {
	if err := d.GuardFixedFileJob(ctx, j); err != nil {
		return err
	}
	state := "succeeded"
	var recordID any
	if record != nil {
		recordID = record.ID
		if record.Status == "failed" {
			state = "failed"
			detail = record.Error
		}
	} else if detail != "" {
		state = "failed"
	}
	_, err := d.ExecContext(ctx, `UPDATE fixed_file_jobs SET status=$3,record_id=$4,error=$5,lease_until=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2`, j.ID, j.LeaseToken, state, recordID, detail)
	return err
}
func (d *DB) CancelFixedFileJob(ctx context.Context, tenant string, taskID, id int64, actor string) error {
	return d.Transaction(ctx, func(tx *DB) error {
		if err := tx.LockFileProcessingTask(ctx, tenant, taskID); err != nil {
			return err
		}
		if _, err := tx.GetFixedFileJob(ctx, tenant, taskID, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE fixed_file_jobs SET status='cancelled',error='用户取消',lease_token=lease_token+1,lease_until=NULL,updated_by=$4,updated_at=now() WHERE tenant_id=$1 AND task_id=$2 AND id=$3 AND status IN ('queued','running')`, tenantOr(tenant), taskID, id, actor)
		return err
	})
}
