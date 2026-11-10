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
	// Executor 是首次提交时固定的解析路由（S5a）：固定解析器版本，路由含 OCR 时另加锁定的 OCR 服务版本；旧作业为空。
	Executor string `json:"executor,omitempty"`
	// ErrorCode 是结构化失败原因（RUNTIME_UNAVAILABLE / BUDGET_EXHAUSTED / PARSE_FAILED / CANCELLED）。
	ErrorCode string `json:"error_code,omitempty"`
	// RecordStatus 是处理记录的当前状态（ready / adopted / failed / rejected），读取时关联，不在作业中冗余保存。
	RecordStatus string `json:"record_status,omitempty"`
}

// JobErrParseFailed 是固定解析的确定性失败（与 chart / export 作业共用 RUNTIME_UNAVAILABLE / BUDGET_EXHAUSTED / CANCELLED）。
const JobErrParseFailed = "PARSE_FAILED"

const fixedJobCols = `id,task_id,tenant_id,file_id,source_hash,request_key,fingerprint,input,status,attempts,max_attempts,deadline_at,lease_token,record_id,error,created_by,created_at,
	executor,error_code,COALESCE((SELECT r.status FROM file_processing_records r WHERE r.id = record_id),'')`

func scanFixedJob(row rowScanner) (*FixedFileJob, error) {
	j := new(FixedFileJob)
	err := row.Scan(&j.ID, &j.TaskID, &j.TenantID, &j.FileID, &j.SourceHash, &j.RequestKey, &j.Fingerprint, &j.Input, &j.Status, &j.Attempts, &j.MaxAttempts, &j.DeadlineAt, &j.LeaseToken, &j.RecordID, &j.Error, &j.CreatedBy, &j.CreatedAt,
		&j.Executor, &j.ErrorCode, &j.RecordStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// Active reports whether the job is queued or running.
func (j *FixedFileJob) Active() bool { return j.Status == "queued" || j.Status == "running" }

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
		var err error
		out, _, err = tx.enqueueFixedFileJobTx(ctx, j, false)
		return err
	})
	return out, err
}

// enqueueFixedFileJobTx runs in the caller's transaction: task row and file-processing lock first, then (for an
// intent-bound submission) the submitting Intent's live lease, then the job row. Same key and fingerprint replays
// the original job in any state; a different fingerprint is an idempotency conflict.
func (d *DB) enqueueFixedFileJobTx(ctx context.Context, j FixedFileJob, bound bool) (*FixedFileJob, bool, error) {
	if err := d.LockFileProcessingTask(ctx, j.TenantID, j.TaskID); err != nil {
		return nil, false, err
	}
	existing, err := d.FindFixedFileJob(ctx, j.TenantID, j.TaskID, j.RequestKey)
	if err == nil {
		if existing.Fingerprint != j.Fingerprint {
			return nil, false, fmt.Errorf("%w: %w: %s", ErrFileProcessingConflict, ErrIdempotencyKeyConflict, j.RequestKey)
		}
		return existing, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	var paused bool
	if err := d.QueryRowContext(ctx, `SELECT paused FROM tasks WHERE id=$1`, j.TaskID).Scan(&paused); err != nil {
		return nil, false, err
	}
	if paused {
		return nil, false, fmt.Errorf("%w: %w: task is paused", ErrFileProcessingConflict, ErrJobPrecondition)
	}
	if bound {
		if _, _, _, err := d.LockSubmittingIntent(ctx, j.TaskID); err != nil {
			return nil, false, err
		}
	}
	var active int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM fixed_file_jobs WHERE tenant_id=$1 AND task_id=$2 AND status IN ('queued','running')`, tenantOr(j.TenantID), j.TaskID).Scan(&active); err != nil {
		return nil, false, err
	}
	if active >= 100 {
		return nil, false, fmt.Errorf("%w: %w: at most 100 pending file jobs per task", ErrFileProcessingConflict, ErrJobCapacity)
	}
	f, _, err := d.GetTaskFileObject(ctx, j.TenantID, j.TaskID, j.FileID)
	if err != nil {
		return nil, false, err
	}
	if f.Availability != "available" || f.SHA256 != j.SourceHash {
		return nil, false, ErrInvalidParseRevision
	}
	out, err := scanFixedJob(d.QueryRowContext(ctx, `INSERT INTO fixed_file_jobs(tenant_id,task_id,file_id,source_hash,request_key,fingerprint,input,executor,created_by,updated_by)
		VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$9) RETURNING `+fixedJobCols,
		tenantOr(j.TenantID), j.TaskID, j.FileID, j.SourceHash, j.RequestKey, j.Fingerprint, string(j.Input), j.Executor, j.CreatedBy))
	return out, false, err
}

// TenderParseSubmission 是 Agent 提交解析作业的结果；Replayed 表示同键同参重放（可能已终态）。
type TenderParseSubmission struct {
	Job          *FixedFileJob         `json:"job"`
	Subscription *AgentJobSubscription `json:"subscription,omitempty"`
	Replayed     bool                  `json:"replayed"`
}

// SubmitTenderParse 在一个事务内登记固定解析作业并订阅宿主绑定的当前 Intent（S5a，Tool 提交强制订阅）。
// 锁序：任务行 / 文件处理锁 → Intent 活租约 → 作业 → 订阅。target 与 expected（目标 parse_revision）写入订阅供查询。
func (d *DB) SubmitTenderParse(ctx context.Context, j FixedFileJob, target any, expected *int64) (*TenderParseSubmission, error) {
	if !IntentExecutionBound(ctx) {
		return nil, ErrNoIntentExecution
	}
	if err := CheckJobKindScope(ctx, JobKindTenderParse); err != nil {
		return nil, err
	}
	if err := CheckFileScope(ctx, j.FileID); err != nil {
		return nil, err
	}
	out := &TenderParseSubmission{}
	err := d.Transaction(ctx, func(tx *DB) error {
		var err error
		if out.Job, out.Replayed, err = tx.enqueueFixedFileJobTx(ctx, j, true); err != nil {
			return err
		}
		if err := CheckFileScope(ctx, out.Job.FileID); err != nil {
			return err
		}
		out.Subscription, err = tx.SubscribeAgentJob(ctx, j.TenantID, j.TaskID, SubscribeAgentJobInput{Job: JobRef{Kind: JobKindTenderParse, ID: out.Job.ID},
			TargetRef: target, ExpectedVersion: expected, Actor: j.CreatedBy})
		return err
	})
	if err != nil {
		return nil, err
	}
	if out.Job, err = d.GetFixedFileJob(context.WithoutCancel(ctx), j.TenantID, j.TaskID, out.Job.ID); err != nil {
		return nil, err
	}
	return out, nil
}

// GetTenderParseJob 读取一个解析作业并复验宿主范围：作业类型、原件范围，以及限定对象时只能读本 Intent 订阅的作业。
func (d *DB) GetTenderParseJob(ctx context.Context, tenant string, taskID, id int64) (*FixedFileJob, error) {
	if err := CheckJobKindScope(ctx, JobKindTenderParse); err != nil {
		return nil, err
	}
	if err := d.checkJobOwnedByIntent(ctx, tenant, taskID, JobRef{Kind: JobKindTenderParse, ID: id}); err != nil {
		return nil, err
	}
	j, err := d.GetFixedFileJob(ctx, tenant, taskID, id)
	if err != nil {
		return nil, err
	}
	if err := CheckFileScope(ctx, j.FileID); err != nil {
		return nil, err
	}
	return j, nil
}

// exhaustFixedJobSQL ends a job whose original deadline or attempt budget is used up ($1 job, $2 actor).
// Claim and SweepFixedFileJobs share it, so a job ends the same way whether or not a runner exists.
const exhaustFixedJobSQL = `UPDATE fixed_file_jobs SET status='failed',error_code='BUDGET_EXHAUSTED',
	error='执行次数或截止时间已耗尽'||CASE WHEN error<>'' THEN '；最近一次：'||left(error,300) ELSE '' END,
	lease_token=lease_token+1,lease_until=NULL,retry_after=NULL,updated_at=now(),updated_by=$2
	WHERE id=$1 AND (status='queued' OR (status='running' AND lease_until<now())) AND (attempts>=max_attempts OR deadline_at<=now())`

// Claim also reclaims expired leases. Attempts and the original deadline never reset.
func (d *DB) ClaimFixedFileJob(ctx context.Context, owner string, lease time.Duration, taskID int64) (*FixedFileJob, error) {
	var id, tid int64
	var tenant string
	err := d.QueryRowContext(ctx, `SELECT j.id,j.task_id,j.tenant_id FROM fixed_file_jobs j JOIN tasks t ON t.id=j.task_id WHERE NOT t.paused AND ($1::bigint=0 OR j.task_id=$1) AND ((j.status='queued' AND (j.retry_after IS NULL OR j.retry_after<=now())) OR (j.status='running' AND j.lease_until<now())) ORDER BY j.id LIMIT 1`, taskID).Scan(&id, &tid, &tenant)
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
		_, err := tx.ExecContext(ctx, exhaustFixedJobSQL, id, owner)
		if err != nil {
			return err
		}
		out, err = scanFixedJob(tx.QueryRowContext(ctx, `UPDATE fixed_file_jobs SET status='running',attempts=attempts+1,lease_token=lease_token+1,lease_until=LEAST(deadline_at,now()+$2*interval '1 second'),lease_owner=$3,retry_after=NULL,updated_by=$3,updated_at=now() WHERE id=$1 AND ((status='queued' AND (retry_after IS NULL OR retry_after<=now())) OR (status='running' AND lease_until<now())) AND attempts<max_attempts AND deadline_at>now() RETURNING `+fixedJobCols, id, lease.Seconds(), owner))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	return out, err
}

// SweepFixedFileJobs keeps the budget fixed at submission meaningful while file processing is
// switched off and no runner claims jobs. It applies only the claim-time rules: a job past its
// original deadline or attempt budget ends as BUDGET_EXHAUSTED (the terminal update publishes the
// job event, which wakes a waiting Agent intent), and a running job whose lease expired returns to
// the queue. Paused tasks are skipped, as at claim time. Nothing is started or extended, and only
// jobs that change are locked, so the export snapshot fence is not bumped needlessly. taskID 0 sweeps
// every task (the server); tests pass their own task. Returns the jobs it changed.
func (d *DB) SweepFixedFileJobs(ctx context.Context, owner string, taskID int64) ([]*FixedFileJob, error) {
	rows, err := d.QueryContext(ctx, `SELECT j.id,j.task_id,j.tenant_id FROM fixed_file_jobs j JOIN tasks t ON t.id=j.task_id
		WHERE NOT t.paused AND ($1::bigint=0 OR j.task_id=$1)
		  AND ((j.status='queued' AND (j.attempts>=j.max_attempts OR j.deadline_at<=now())) OR (j.status='running' AND j.lease_until<now()))
		ORDER BY j.id LIMIT 100`, taskID)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id, task int64
		tenant   string
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.task, &c.tenant); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var changed []*FixedFileJob
	for _, c := range cands {
		var out *FixedFileJob
		err := d.Transaction(ctx, func(tx *DB) error {
			if err := tx.LockFileProcessingTask(ctx, c.tenant, c.task); err != nil {
				return err
			}
			var paused bool
			if err := tx.QueryRowContext(ctx, `SELECT paused FROM tasks WHERE id=$1`, c.task).Scan(&paused); err != nil {
				return err
			}
			if paused {
				return nil
			}
			j, err := scanFixedJob(tx.QueryRowContext(ctx, exhaustFixedJobSQL+` RETURNING `+fixedJobCols, c.id, owner))
			if !errors.Is(err, ErrNotFound) {
				out = j
				return err
			}
			j, err = scanFixedJob(tx.QueryRowContext(ctx, `UPDATE fixed_file_jobs SET status='queued',lease_token=lease_token+1,lease_until=NULL,lease_owner='',updated_at=now(),updated_by=$2
				WHERE id=$1 AND status='running' AND lease_until<now() RETURNING `+fixedJobCols, c.id, owner))
			if errors.Is(err, ErrNotFound) {
				return nil // changed by someone else since the scan
			}
			out = j
			return err
		})
		if errors.Is(err, ErrNotFound) {
			continue // task deleted since the scan
		}
		if err != nil {
			return changed, err
		}
		if out != nil {
			changed = append(changed, out)
		}
	}
	return changed, nil
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
	return d.FinishFixedFileJobCode(ctx, j, record, "", detail)
}

// FinishFixedFileJobCode writes the terminal status under the job's lease token inside the output transaction.
// A failed record ends the job as failed with PARSE_FAILED; a bare detail ends it as failed with code.
func (d *DB) FinishFixedFileJobCode(ctx context.Context, j *FixedFileJob, record *FileProcessingRecord, code, detail string) error {
	if err := d.GuardFixedFileJob(ctx, j); err != nil {
		return err
	}
	state := "succeeded"
	var recordID any
	switch {
	case record != nil && record.Status == "failed":
		recordID, state, code, detail = record.ID, "failed", JobErrParseFailed, record.Error
	case record != nil:
		recordID, code = record.ID, ""
	case detail != "":
		state = "failed"
	default:
		code = ""
	}
	_, err := d.ExecContext(ctx, `UPDATE fixed_file_jobs SET status=$3,record_id=$4,error=$5,error_code=$6,lease_until=NULL,retry_after=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2`,
		j.ID, j.LeaseToken, state, recordID, detail, code)
	return err
}

// ReleaseFixedFileJob returns a running job to the queue after a transient runtime failure (for example the local
// OCR service restarting). The attempt stays counted, the original deadline and attempt budget are kept, the token
// bump fences the old attempt and the next claim waits until retry_after. Exhaustion is settled by ClaimFixedFileJob.
func (d *DB) ReleaseFixedFileJob(ctx context.Context, j *FixedFileJob, code, detail string, after time.Duration) error {
	return d.Transaction(ctx, func(tx *DB) error {
		if err := tx.LockFileProcessingTask(ctx, j.TenantID, j.TaskID); err != nil {
			return err
		}
		if err := tx.GuardFixedFileJob(ctx, j); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE fixed_file_jobs SET status='queued',lease_token=lease_token+1,lease_until=NULL,lease_owner='',error=$3,error_code=$4,
			retry_after=now()+$5*interval '1 second',updated_at=now() WHERE id=$1 AND lease_token=$2`, j.ID, j.LeaseToken, detail, code, after.Seconds())
		return err
	})
}

func (d *DB) CancelFixedFileJob(ctx context.Context, tenant string, taskID, id int64, actor string) error {
	return d.Transaction(ctx, func(tx *DB) error {
		if err := tx.LockFileProcessingTask(ctx, tenant, taskID); err != nil {
			return err
		}
		if _, err := tx.GetFixedFileJob(ctx, tenant, taskID, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE fixed_file_jobs SET status='cancelled',error='用户取消',error_code='CANCELLED',lease_token=lease_token+1,lease_until=NULL,retry_after=NULL,updated_by=$4,updated_at=now() WHERE tenant_id=$1 AND task_id=$2 AND id=$3 AND status IN ('queued','running')`, tenantOr(tenant), taskID, id, actor)
		return err
	})
}
